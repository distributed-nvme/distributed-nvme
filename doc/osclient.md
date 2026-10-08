# osclient.md — OS client

This document owns the one path every dnv binary takes to the operating
system: the `OsClient` interface for OS commands and for file, protobuf and
raw block IO, its production implementation `LimitedOsClient` with its
concurrency limit, its command timeouts and kill rule, the guarantees of each
operation (the atomic replace and fsync of a file write, the exact-length
read and fsynced write of a block), the exported raw helpers and the probe-IO
carve-out that is the one sanctioned bypass, and what emitting the record of
each operation requires. It leans on `log.md` for the helpers it reuses
(`PbToLogValue`, `TruncForLog`, `LogStrDataLimit`), for its rules on Info
records, typed attributes and ctx propagation, and for the names and
attributes of the records (`log.md`, OS commands and file IO); on
`architecture.md`, Common validation, for the command timeouts, and Common
agent rules for the local-store write protocol this client makes automatic;
and on `dnagent.md` and `cnagent.md` for the wrappers and probers that call
it.

## Scope and placement

* Files: `common/osclient.go` (the interface, `LimitedOsClient` and the
  exported raw block helpers `WriteBlockAt` and `ReadBlockDirectAt` of
  "Exported raw helpers and the probe-IO carve-out") and
  `common/osclient_fake.go` (the test double). Package `common`, alongside
  `constants.go` and `name_fmt.go` (the `Local*Path` files that go through
  `ReadProto` and `WriteProto`).
* Consumers: `dnv-agent` in both roles, for its dmsetup, mdadm, nvme and
  nvmet-configfs work and for persisting the `Local*Path` protobuf state
  files. **All** OS command execution and disk file IO in production code
  goes through an `OsClient`
  — never call `os/exec` or `os.ReadFile`/`os.WriteFile` directly outside
  this file. This is what makes the logging rules of `log.md` R8 (its first
  two items) enforceable and makes every consumer unit-testable via
  `FakeOsClient`. There is exactly **one** sanctioned exception in
  production code, recorded under "Exported raw helpers and the probe-IO
  carve-out": the CN11 leg-health probers call the package-level helpers
  `WriteBlockAt` / `ReadBlockDirectAt` directly — outside the semaphore,
  never under a lock — and log their own records. Test fixtures that spawn a
  helper process for their own package (the etcd fixtures that start a real
  etcd binary, and the tests that re-run the test binary itself) are outside
  the rule, not exceptions to it: no production code path spawns them, so
  routing them through an `OsClient` would buy neither the logging nor the
  `FakeOsClient` testability the rule exists for.
* Dependencies: the semaphore and protobuf modules of `dependencies.md`,
  Direct dependencies; the command's `Cancel` and `WaitDelay` fields come
  with the Go version `go.mod` pins.

## Interface

`OsClient` is the contract every consumer compiles against and every
implementation — `LimitedOsClient` in production, `FakeOsClient` in tests —
satisfies; `common/osclient.go` holds the signatures, which are fixed, and
the method set is exactly the following, each taking the caller's ctx:

* `RunCommand` executes an OS binary with its command-line arguments and an
  optional stdin payload (empty when none) and returns the captured stdout
  and stderr separately, the exit code (zero on success, non-zero for an
  error exit, -1 when the process never reported) and an error that is
  non-nil for a non-zero exit or an execution failure.
* `ReadFile` loads the whole file at a path into memory as a string; the
  error is non-nil when the ctx is cancelled, the file is missing or the IO
  fails.
* `WriteFile` creates or overwrites a file with string data under the
  default file permissions, by atomic replace.
* `WriteFileDirect` writes string data straight into the file at a path —
  no temp file, no rename — and exists only for kernel virtual filesystems.
* `ReadBlock` reads exactly the requested length at a byte offset of a block
  device (or regular file); a short read is an error; buffered IO, which
  callers use only for regions no dm table references.
* `WriteBlock` writes data at a byte offset and fsyncs the file descriptor
  before returning.
* `ReadProto` reads a protobuf binary file and deserializes it into a
  non-nil target message.
* `WriteProto` serializes a message to wire binary and writes it with the
  same atomic replace as `WriteFile`.

## `LimitedOsClient` — normative behavior

### Construction and concurrency limit

* `NewLimitedOsClient` takes the limit; a limit of zero or less means "use
  `DefaultOsClientLimit`", the default cap on in-flight operations
  (commands, file IO and proto IO combined) that `constants.go` holds. Its
  reason: during convergence an agent fans out many dmsetup, mdadm, nvme and
  blkdiscard invocations plus state-file writes, and the default bounds fork
  and disk pressure while leaving ample parallelism.
* The limit caps the **sum of in-flight calls across all interface
  methods**. It deliberately does **not** cover the package-level helpers of
  the carve-out: probe IO must never consume a slot. One weighted semaphore
  (`semaphore.NewWeighted`) carries the limit. Every public method first
  acquires one unit (blocking, ctx-aware: a cancelled or expired ctx returns
  the ctx error without performing the operation and without logging an
  operation record) and releases it when it returns.
* `LimitedOsClient` is safe for concurrent use and holds no other mutable
  state. One instance per process is the expected usage.

### RunCommand

* The command runs through `exec.CommandContext`; the stdin payload is
  attached only when it is non-empty; stdout and stderr are captured into
  separate buffers.
* Timeout and kill semantics (implements the command timeouts of
  `architecture.md`, Common validation): the **caller** owns the deadline —
  agents wrap the ctx with the `CmdSoftTimeout` deadline (`agent.CmdCtx`;
  `dnagent.md` SH15) before calling. `LimitedOsClient` adds no deadline of
  its own, but configures:
  * SIGTERM when the ctx fires (the soft timeout), through the command's
    `Cancel`;
  * a `WaitDelay` of the difference between `CmdHardTimeout` and
    `CmdSoftTimeout` — if the process ignores SIGTERM, it is SIGKILLed after
    this grace, i.e. at the hard timeout relative to the soft one.
    `WaitDelay` also prevents the wait from hanging on inherited pipes.
  * Neither signal bounds a child blocked in an uninterruptible kernel wait.
    Both are delivered, but the child exits only when the kernel returns,
    and the wait reaps it before anything else: the call returns, and gives
    back the semaphore slot it holds, only then. An `nvme disconnect` whose
    target vanishes mid-delete waits out the kernel's admin timeout this
    way, which is why the cn sweep issues it off its locks (`cnagent.md`
    CN10).
* Exit-code mapping: a nil error → 0; an `exec.ExitError` → its exit code
  (a signal-killed process reports -1 here, with a non-nil error —
  acceptable); any other error (start failure, ctx cancelled before start)
  → -1. The semaphore refusal of "Construction and concurrency limit"
  returns -1 with the ctx error as well, and logs no record, because no
  command ran.
* The error is returned exactly as produced (non-nil for a non-zero exit,
  signal death, start failure, or ctx cancellation). stderr is not wrapped
  into the error; the caller already receives stderr separately.
* **"Did not answer" is not "absent".** The mapping above yields exactly two
  classes of outcome, and every caller keeps them apart:
  * an exit code above zero, always with a non-nil error — the tool RAN and
    answered. For a probe that answer is "no": the array is not running,
    the dm device does not exist, the directory is not there.
  * an exit code of -1 with a non-nil error — the process never reported:
    SIGTERMed at the caller's soft timeout, SIGKILLed at the hard one,
    failed to start, ctx cancelled, or refused a semaphore slot. The caller
    learned NOTHING about the object. (A child in an uninterruptible kernel
    wait comes back this way only when the kernel lets it go, however long
    after the hard timeout that is.)

  A killed command may still have completed in the kernel — the ioctl or
  the configfs write runs to the end regardless of the signal that hit the
  process waiting on it — so neither "it happened" nor "it did not" follows
  from the kill, and only a fresh probe afterwards can say which it was.
  Reading the second class as the first lets an agent teardown skip
  `mdadm --stop` for an array whose `mdadm --detail` was killed at the soft
  timeout (a `--detail` opens the members, so it blocks until the leg's
  fast_io_fail_tmo expires, which is longer than `CmdSoftTimeout`), leave
  the array pinning its two leg wrappers, and forget it for ever; on the DN
  the same conflation frees an extent record while the device still maps
  those extents, and the next allocation hands them out twice.

  `agent.Reported` (`agent/oswrap.go`) is the one predicate — a nil error,
  or an exit code above zero — and `osBase.runProbe` is the only probe
  wrapper that applies it, so it is the one place a probe's exit code is
  classified. Every probe whose non-zero exit means "absent" runs through
  it, a role package's own through `Cmd.RunProbe` or `Cmd.ListDir`, and
  answers "absent" only for a reported non-zero exit and an error for a run
  that did not answer; `dnagent.md` SH15 names the primitives whose callers
  act on that "absent".
  The predicate's one other caller, `Dm.BlkZeroout`, is not a probe: it returns
  the verdict as answered, so the `dnagent.md` DN9 zeroing loop can tell a
  batch the soft timeout killed from one the tool refused. The enumerators
  a removal decision is taken from — `Dm.List`, `Md.ListArrays`,
  `NvmeHost.ListAllSubsys`, `Nvmet.ListSubsystems` — and `Md.Gone`, the
  probe that judges a stop, propagate that error to their caller instead of
  answering "nothing there"; the md walk applies the same rule
  (`cnagent.md` CN12). The agents' sweeps record an enumeration that did
  not answer as a failure of the pass (`SweepResult.Fail`, which keeps it
  from being clean exactly as a leftover object does), because a listing
  that failed cannot prove a node holds nothing. `Dm.List` goes one step
  further and treats every non-zero exit as an error: `dmsetup ls` exits
  zero and prints "No devices found" on an empty node, so a failure there
  is never an empty listing. `CloneMeta.LoopDevices` does the same for
  `losetup --associated`, which exits zero with no output when nothing is
  attached. The file-read half of the same rule is `osBase.readAttrStrict`
  over `ReadFile`: only `fs.ErrNotExist` is "absent", every other error
  propagates, which is what keeps a stalled sysfs or configfs read
  (`NvmeHost.readTrimmed`, `Nvmet.NsDevicePath`, `Nvmet.NsAnaGrpId`, the
  "enable" reads of `Nvmet.RemoveNamespace` / `RemoveSubsystem`, the
  attribute reads of `Nvmet.ProbePortState`, the port read of `cnagent.md`
  CN30's verdict, and the cn leg walk's "subsysnqn" read, through
  `Cmd.ReadAttr`) from making a live object read as an absent one.

### ReadFile / WriteFile / WriteFileDirect

* Each checks the ctx error after acquiring the semaphore and returns it if
  non-nil. That is the call's last cancellation point: the IO itself is not
  cancelable. For plain file IO on local disks this best-effort cancellation
  is sufficient; a sysfs or configfs access the kernel holds returns only
  when the kernel does, however far past the caller's deadline
  (`dnagent.md` SH13, SH15).
* `ReadFile` reads the whole file (`os.ReadFile`) and returns it as a
  string.
* `WriteFile` is an **atomic replace**: write to a temp file in the same
  directory, sync it, close it, set the default file permissions, then
  rename it onto the path, then open the directory and sync it. The temp
  file is named after the target with `AtomicWriteTmpInfix` and a random
  suffix, and every failure path removes it, the removal's own error aside,
  so what leaves one behind is a removal that fails, a process that dies
  between creating it and the rename, or a machine crash that undoes a
  rename (below) or a removal not yet durable; the agents' local store
  never reads such a leftover and deletes it at startup (`dnagent.md` SH6).
  The rename is a change to the directory, so until the directory is
  fsynced a crash can undo it — bring the old file back or, for a first
  write, leave none — however durable the temp file's own sync made the
  bytes; a failed directory sync therefore fails the write, although the
  rename has already happened: readers see the new file at the path, and
  only whether it survives a crash is unknown. This makes the agents'
  local-store requirement ("temp file in the same dir, fsync, rename, fsync
  the dir"; `architecture.md`, Common agent rules) automatic for every
  state file, and is the right default for every regular-file write.
* `WriteFileDirect` is a plain in-place write (`os.WriteFile` under the
  default permissions): it creates a missing file and overwrites an existing
  one in place, with no temp file, no fsync and no rename. It exists for
  kernel virtual filesystems (nvmet configfs, sysfs), where the atomic
  replace is impossible: configfs forbids creating arbitrary files, so
  `WriteFile` can never succeed there. Use it **only** for such attribute
  writes; the agents' local-store state files keep using
  `WriteFile`/`WriteProto` (`dnagent.md` SH18).

### ReadProto / WriteProto

* `ReadProto` reads the file bytes, then unmarshals them (`proto.Unmarshal`)
  into the target.
* `WriteProto` marshals the message (`proto.Marshal`), then does the same
  atomic replace as `WriteFile`.
* These are the methods the agents use for the `Local*Path` files
  (`architecture.md`, Agent local-store paths and Common agent rules): per
  object, the last `Syncup*Request` saved for it — `dnagent.md` SH5 says
  when — and the received `Push*BitmapRequest` chunks.

### ReadBlock / WriteBlock

The raw-device metadata path of `architecture.md`, [D13]: the dn agent reads
and writes its own on-disk format (header block, A/B volume-table slots,
dm-clone metadata slots) directly on the `--disk` device through these two
methods.

* Each checks the ctx error after acquiring the semaphore, exactly like the
  file methods.
* `ReadBlock` opens the path read-only, reads exactly the requested length
  at the offset (`io.ReadFull` over a section reader), and closes. **A short
  read is an error** — the method never returns a partially filled buffer,
  so a caller can trust the returned length whenever the error is nil. The
  bound is checked before the buffer is allocated, against a seek to the end
  (a stat reports no size for a block device), so a nonsense length — a
  corrupt on-disk length field, say — fails before it can become a huge
  allocation. A region inside the device that was never written reads as
  zeros; that is not an error.
* `WriteBlock` opens the path write-only, writes at the offset, syncs (the
  fsync the caller's crash protocol depends on), and closes. It never
  *creates* a file (no O_CREATE); the intended target is a block device,
  which always exists at full size. Against a regular file — tests — a write
  past the end extends it, as pwrite does. The body is the package-level
  helper `WriteBlockAt`; `WriteBlock` adds only the semaphore slot and the
  `os write block` record.
* **Buffered IO, never O_DIRECT.** Durability comes from the sync. The
  regions the DN agent reads back this way — the header block and the two
  volume-table slots — are never part of any dm table, so no dm path can
  write bytes underneath a cached read. The clone-metadata area *is* mapped,
  by each migration's wrapper dm-linear, but the agent only ever **writes**
  there (the zeroing of a freshly allocated slot), only before that slot's
  wrapper exists, and always followed by the sync — so the bytes reach the
  device before anything can map them, and no read of that area is ever
  served from the page cache.
* **Never shell out to dd for this.** The lab VMs ship a uutils dd whose
  direct-IO flags silently misbehave — false failures and dropped writes
  (`dnagent_integtest.md`, Assumptions and preflight checks).
* The payload is **never** logged: the records carry `path`, `offset` and
  `length` only (Logging). Metadata blocks are large and uninteresting in a
  log, and a device region may hold arbitrary user data.

### Exported raw helpers and the probe-IO carve-out

The `OsClient` interface carries no direct read: the CN11 leg-health prober
is the only caller of one, and a prober must never hold a semaphore slot
(below). The two raw bodies are exported from `common/osclient.go` as plain
package-level functions instead. They take no ctx, acquire **no** semaphore
slot, and log **nothing**:

* `WriteBlockAt` pwrites data at a byte offset and fsyncs the file
  descriptor before returning. Buffered IO; a fresh fd per call, never a
  cached one; never O_CREATE (the target is a block device that already
  exists at full size).
* `ReadBlockDirectAt` preads exactly the requested length at a byte offset
  with O_RDONLY, O_DIRECT and O_CLOEXEC into an aligned bounce buffer,
  copies the bytes out, then closes. Offset and length MUST be multiples of
  the alignment and the length must not be zero — checked **before** the
  open, so a misaligned caller never touches the device. A short read is an
  error, exactly like `ReadBlock`. A fresh close-on-exec fd per call: no
  command started while the read blocks inherits it.

The alignment is the probe's natural unit, the size `LegHealthBlockSize`
names: O_DIRECT alignment is per-device, and that unit satisfies every device
dnv touches. `LimitedOsClient.WriteBlock` is a thin wrapper over
`WriteBlockAt` (the semaphore plus the `os write block` record);
`ReadBlockDirectAt` has no `OsClient` wrapper at all. The buffered
`readBlockAt` behind `ReadBlock` stays **unexported** — nothing outside the
`OsClient` may bypass it for metadata IO (`architecture.md`, [D13]).

**The recorded carve-out.** Probe IO — and only probe IO — is the one
sanctioned direct-syscall path in dnv:

* *What it is.* The leg health probe (`cnagent.md` CN11 and Leg-probe IO
  leaves the `OsClient`; `architecture.md`, [D6] and Group on-leg layout:
  meta region, data region, health block): write a block through the leg
  wrapper with `WriteBlockAt`, read it back with `ReadBlockDirectAt`. The
  read must bypass the page cache — a buffered read of a just-written block
  would be answered from cache and observe no device IO at all, making the
  read-back vacuous. The write half needs no direct twin: its fsync already
  forces the data to the device and surfaces the IO error.
* *It may block indefinitely, by design.* A leg with no serving path (a
  ctrl_loss_tmo of -1) queues IO forever, so the calling goroutine sits in
  uninterruptible D state until that IO is errored — which happens only when
  the agent disconnects the path (`cnagent.md` CN10). This is the feature
  working: a hung probe is how a silently stalled target is detected. Two
  consequences the implementer must not design around: **cancelling the
  prober's context does not unblock a syscall already in flight** (the
  helpers take no ctx; the prober's ctx is checked once before the call and
  otherwise only carries the CN2 per-attempt trace id into the records
  below), and **nothing ever waits for a prober to finish** — cancel and
  move on, never cancel-and-wait. (Contrast the dn zeroing goroutines of
  `dnagent.md` DN9, which *are* waited for: their blkdiscard is a child
  process SIGKILL ends, not a blocked syscall of the agent's own — though
  one in an uninterruptible kernel wait dies only when the kernel returns,
  RunCommand above.)
* *Its descriptor is close-on-exec.* `ReadBlockDirectAt` opens with
  O_CLOEXEC, as `os.OpenFile` opens every descriptor (`WriteBlockAt`'s
  included); a bare `syscall.Open` does not add it. Every command the agent
  forks while a probe is blocked would otherwise inherit the leg wrapper's
  descriptor, and one that outlives the probe — an `nvme disconnect` stalled
  on its target — keeps the wrapper open, so the teardown's `dmsetup remove`
  fails with EBUSY until that command exits.
* *It must never run under a lock.* `cnagent.md` CN1 already keeps the
  probers out of the lock hierarchy; a wedged probe under the node or object
  lock would freeze every converge and Check round on the node.
* *It must never run through the semaphore.* `LimitedOsClient` is a
  `DefaultOsClientLimit`-slot semaphore held across the blocking syscall.
  One dead or partitioned DN can back more legs on a CN than the client has
  slots (`MaxSideCntPerDn` is far above `DefaultOsClientLimit`); that many
  wedged probes then starve **every** OS operation on the node — including
  the `nvme disconnect` that is the only documented release mechanism for
  those very probes, and the mdadm and dm commands the self-healing of
  `architecture.md`, Automatic reactions, needs. Deadlock by construction;
  hence the helpers, not a second `OsClient` and not a probe budget.
* *Who may call them.* Only the cn agent's lock-free prober goroutines
  (`cnagent.md` CN1 and CN11), through the small fakeable probe-IO
  dependency defined in `healthcheck.go`. These two helpers are the
  only block-IO syscalls any package outside `common` may call; every other
  caller — the dn `diskmeta.go` header/volume-table path above all — keeps
  using the `OsClient` methods of "ReadBlock / WriteBlock".
* *Records.* Because the helpers log nothing, the prober emits its own two
  records, `probe write block` and `probe read block direct` (Logging). They
  are deliberately **not** "os …" messages.

Everything "ReadBlock / WriteBlock" says about payload logging (`path`,
`offset` and `length` only, never `data` — a device region may hold arbitrary
user data) and about never shelling out to dd (the lab's uutils dd and its
broken direct-IO flags, `dnagent_integtest.md`, Assumptions and preflight
checks) applies to the helpers and to the prober's records unchanged.

### Logging

One `slog.InfoContext` record per call, emitted on completion, under the
`msg` strings and attributes `log.md` owns (R8, and OS commands and file IO:
each `OsClient` method emits the "os …" record assigned to it there, and
`error` is appended only when the error is non-nil). `ReadProto` and
`WriteProto` log the decoded message through `PbToLogValue` rather than the
file bytes (`log.md` R11).

The CN11 leg probers do **not** call an `OsClient`. They call the
package-level helpers and emit the two "probe …" records of `log.md`,
OS commands and file IO, themselves, one per probe half, under the attempt's fresh trace id — same
attributes as the block records, same "never `data`" rule. The "probe …"
prefix is load-bearing: the cn suite's mutation grep (`mutations` in
`integtest/cnagent_test.sh`; `cnagent_integtest.md`, Conventions) names
`os write block` explicitly, and the continuous health probes must fall out
of that grep **by construction** rather than by a path-based exemption; no
agent emits an "os read block direct" record. `os write block` is the dn
agent's `diskmeta.go` record, so a "probe …" record and an "os …" record can
never be confused for one another.

Notes:

* Command stdin, stdout and stderr are logged in full (no truncation) — the
  `TruncForLog` rule (`LogStrDataLimit` characters) applies to file string
  data only.
* `PbToLogValue` already reduces embedded `bytes` fields (the `bitmap` of a
  persisted `PushMigrBitmapRequest`, for one) to a byte count, so large
  payloads never reach the log.
* A semaphore-acquire failure (ctx cancelled while waiting) logs nothing —
  the operation never happened.
* The "probe …" records carry no semaphore semantics: a prober never waits
  for a slot. The same "never happened, never logged" rule still applies to
  it, through the one ctx check the prober makes *before* calling a helper —
  an attempt cancelled there emits no record at all.

## Test double — `common/osclient_fake.go`

`FakeOsClient` is the configurable `OsClient` test double: a function field
per interface method, of which a test sets only the ones it needs; an unset
field succeeds with zero values. It is exported — not a `_test.go` file — so
tests in other packages can reuse it. It has no
field for the direct read: the probe read is not on the interface, and probe
IO is faked through the cn agent's own probe-IO dependency (`cnagent.md`,
Leg-probe IO leaves the `OsClient`, and Server type and lock mapping), not
through this double.
