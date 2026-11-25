# log.md — Logging

This document owns the JSON logging every dnv binary shares: the handler
chain and the stream it writes to, the trace id that joins the records of one
operation across processes, the log levels, the flag that sets one and the
signals that change it while a daemon runs, the attribute discipline, the
shared rendering helpers, and the records each subsystem must emit.
`osclient.md` and `grpc.md` build on the helpers defined here;
`architecture.md` gives the system context and `dnvctl.md` the CLI's output
contract that the choice of stream serves.

## Placement

All logging code is shared and lives in package `common`, in `common/log.go`:
the handler chain, the level, its parse and the watch of the signals that
change it. Every dnv binary (`dnv-gateway`, `dnv-worker`, `dnv-agent`,
`dnv-cdc`, `dnv-csi`, `dnvctl`) imports `common`, so its `init` installs the
default logger before any `main` runs. `NewTraceId` relies on `rand.Read` of `crypto/rand` never
failing, a guarantee of the Go version `go.mod` pins; the protobuf rendering
helper uses the protobuf module `go.mod` requires.

## Rules

R1. Use the standard library `log/slog` only. No third-party logging
    libraries. `fmt.Print*` is never used for operational logging (dnvctl
    printing command *results* to the user is CLI output, not logging, and is
    exempt).

R2. The handler chain is `TraceIdHandler` wrapping a `slog.NewJSONHandler` on
    `os.Stderr` whose options hold the process log level. All logs go to
    **stderr** as JSON, one record per line. The stream is the kubernetes
    convention — klog defaults to stderr, kubectl keeps stdout for the
    result a script pipes — and what it buys is that **stdout is the payload
    channel** of every dnv binary: dnvctl's one result document (`dnvctl.md`
    CT7), and nothing from a running daemon: the five daemons put there
    only cobra's own `--help` text, its `completion` command's script and
    their `--version` line, and `dnv-agent check` and `dnv-agent collect`,
    which run once and are no daemon, their reports. A consumer that reads stdout alone therefore
    gets the result and never a log record. The etcd client's own logger is
    silenced (`zap.NewNop`, in `etcdutil`) so that stderr carries one JSON
    stream and it is dnv's — a daemon that exits on an error prints that
    error there as plain text — and no dnv record comes from anything but
    `log/slog`. All handler options stay at their defaults except `Level`
    (see R6), the one deliberate deviation from nil options, required by the
    level rules.

R3. `common`'s `init` calls `slog.SetDefault` with that chain, and that call
    is the only `slog.SetDefault` in the repository outside test files. The
    dnv binaries do not build their own loggers: the chain's handler holds
    `logLevel`, a `slog.LevelVar`, so `SetLogLevel` (R6) and the signals of
    R13 still bite after `init` has run, which is precisely what a second
    handler built around a static level would throw away. The `integtest/` drivers are not dnv
    binaries, and they build no logger either: they inherit this stderr
    default, which is exactly what keeps their stdout the result channel the
    suites parse.

R4. Every log call uses the context-aware form — `slog.DebugContext`,
    `slog.InfoContext`, `slog.WarnContext`, `slog.ErrorContext`, or
    `slog.Log` and `slog.LogAttrs` with a ctx for a level the call works out
    at run time — and passes the request-scoped ctx. Use
    `context.Background` explicitly only when no operation context
    exists. **Always propagate the request ctx down** into `OsClient` calls,
    etcd calls and gRPC calls; the trace id rides on it (see R5 and
    `grpc.md`, T1 to T3).

R5. The trace id is carried in the context under an unexported key and
    injected into every record as the attribute `trace_id` by
    `TraceIdHandler`. Access it only through the exported helpers
    `WithTraceId` and `TraceIdFromCtx`. The gRPC interceptors (`grpc.md`, T1
    and T2) move the trace id between context and gRPC metadata. Nothing
    else touches it but the entry points that set one (`grpc.md` T4, and the
    `integtest/` drivers' `--trace-id`) — among them the gateway's mint,
    which writes it straight into the incoming metadata
    (`gateway/traceid.go`), and `gatewayctl`, `dnagentctl` and `cnagentctl`,
    which put it into their outgoing metadata themselves because they dial
    without the client interceptors (`grpc.md`, Drivers and fakes) — and four
    places that move an id they were handed: the `Check*` streams, whose
    requests carry each round's id in a `trace_id` field that the worker fills
    and the agent adopts (`grpc.md` T3); the background read a round starts,
    which puts that round's id onto the agent's lifetime ctx (`rootCtx`) and
    mints one only if the round had none (`dnagent.md` SH29); the cn sweep's
    background "nvme disconnect", which puts the id of the pass that set it
    going onto `rootCtx` and mints one only if that pass had none (`dnagent.md`
    SH27, `cnagent.md` CN10); and the end of a converge or a read, which stores
    its id in the observation it makes, for the reply's stamp to carry as
    `obs_trace_id` and the worker's `syncup result` record of a re-send to name
    (`grpc.md` T3). On a logger derived with `WithGroup` the attribute is
    nested inside that group rather than at the top level — see R12 — so a
    consumer that greps `trace_id` positionally must account for it.

R6. **What each level holds.** Debug holds what repeats every interval and
    changes nothing: a read that succeeded, or answered "absent" where R9
    says so, a check round's stream messages, the worker's record of a late
    round that passed (`dnv-worker.md`, Log records), the cn agent sweep's
    note of a subsystem in dnv's NQN space whose name it cannot decode and
    leaves alone (`cnagent.md` CN21), and the CSI driver's records of the
    calls Kubernetes repeats on a timer while they succeed (`csi.md`, Log
    records). Info holds a change, an event and a failure: every write,
    every request and reply of a dnv service's unary call (`grpc.md` L1),
    every other read that failed, and a failure the program clears alone, as
    its next pass or its retry does, so that a fault that heals sets off no
    alert. Warn holds a record an operator must look at: a fault, a refusal
    or a lost redundancy that holds until an operator acts or a lost program
    comes back (`dnv-worker.md`, Log records), a sign that a node's saved
    state was lost or changed behind the agent's back, such as a zeroed
    count a save rolled back (`dnagent.md` DN9), the outcome of a forced
    start, which chooses the copy a group keeps (`cnagent.md` CN12), or the
    control plane fencing itself, which the worker ends alone but an
    operator must know of. `log level changed` is the one Warn record that
    is no fault: it is at Warn only so that every `--log-level` shows it
    (R13), and an alert on Warn leaves it out. Error holds a fault the
    program could not carry out or must not pass over. Warn and Error are
    the records to alert on.

    **The flag.** The five daemons — `dnv-gateway`, `dnv-worker`,
    `dnv-agent` in both roles, `dnv-cdc` and `dnv-csi` — and `dnvctl` take
    `--log-level` with one of three words, `debug`, `info` or `warn`, in any
    case, through the same flag, environment and config binding as their
    other flags; anything else is refused before the program starts, naming
    the three words (`ParseLogLevel`). There is no `error`: a level that hid
    Warn would hide what an operator must look at, and the change records of
    R13. The daemons default to `info` and set the level before their first
    record. `dnvctl` defaults to `warn` (`dnvctl.md` CT7): its `main` sets
    Warn as its first statement and the command tree then sets its
    `--log-level`, so by default all the R8 logging points stay silent in
    dnvctl. `dnv-agent`'s node check takes the flag with its role's other
    flags, so an agent's command line serves its check, and reads no value
    of it; the gather takes none. Both log at Warn, set as the first
    statement of their `RunE` (`dnagent.md` CM5, CM6), since what they read
    is in their report and their archive. The level lives in a
    package-level `slog.LevelVar`, whose zero value is Info, set with
    `SetLogLevel` and read with `LogLevel`.

R7. Attributes are always built with the strongly-typed constructors of
    `log/slog` — `slog.String`, `slog.Int`, `slog.Int64`, `slog.Uint64`,
    `slog.Bool`, `slog.Any` and their typed siblings. Never pass loose
    alternating key, value arguments. Use `slog.Any` only for slices, maps
    and the `PbToLogValue` output.

R8. The following events MUST be logged, at the level R9 gives, with the
    record names and attributes of "Record obligations by subsystem" below:
    1. every OS command execution (stdin, stdout, stderr, exit code) —
       implemented once inside `LimitedOsClient` (`osclient.md`, Logging);
    2. every file read/write (full path; string data truncated to the first
       `LogStrDataLimit` characters; non-UTF-8 data logged as size only) —
       implemented once inside `LimitedOsClient`;
    3. every gRPC client/server request/reply message (protobuf rendered as
       JSON; `bytes` fields rendered as their size only) — implemented once
       in the interceptors (`grpc.md`, L1 to L6);
    4. every etcd read/write (human-readable key, value decoded from
       protobuf and rendered as JSON — never raw protobuf binary) —
       implemented in the central etcd helpers of `etcdutil`;
    5. every directory listing made in the caller's own process (full path;
       every name, never truncated) — implemented once inside
       `LimitedOsClient`;
    6. every modification-time read made in the caller's own process (full
       path; the time read) — implemented once inside `LimitedOsClient`;
    7. every device-number read made in the caller's own process (full path;
       the number read) — implemented once inside `LimitedOsClient`;
    8. every set of discards made in the caller's own process (full path;
       the ranges asked for, the ranges done) — implemented once inside
       `LimitedOsClient`;
    9. every detached child started (the command, its process id, its
       output file) — implemented once inside `LimitedOsClient`;
   10. every TCP dial made in the caller's own process (the address, the
       timeout, how long it took) — implemented once inside
       `LimitedOsClient`;
   11. every exclusive open of a block device made in the caller's own
       process (full path) — implemented once inside `LimitedOsClient`.

R9. Each logging point emits exactly **one** record per operation, on
    completion, so the record can carry the outcome (exit code, error); a
    unary call's request record, logged before the call (`grpc.md` L3), and
    the record of a put or delete that an STM holds back for its commit
    (etcd, below) carry no outcome. When the operation failed, the same
    record additionally carries an `error` attribute holding the error's
    text. The level follows R6. At Debug: a read that succeeded — a file,
    block, proto, modification-time or device-number read, a directory
    listing, a dial that connected, an exclusive open that was granted, an
    etcd get, range or watch event, a
    command of the read list, which is `commandReadsOnly`
    (`common/osclient.go`), a check round's stream message — and a leg
    probe's half that succeeded, the probe being a read of the leg's health
    though its first half writes (`ReadLevel`); and a file, proto,
    modification-time or device-number read, a directory listing or an
    exclusive open that found its path absent, and a device-number read
    that found no block
    device at its path, since its caller decides by that answer, the CSI
    node plugin's stats call by the second. At Info: a read that failed
    otherwise, every write, every command not on the read list, every
    request and reply of a dnv service's unary call, and a stream's open and
    close (`grpc.md` L1). These logging points use no other level; failures
    still reach the caller through the returned error. A point that can log
    at Debug works out its level first and builds its attributes only when
    the process logs that level (`LogEnabled`), so a dropped Debug record
    costs no rendering; a point that is always at Info builds its record as
    it logs it.

R10. Protobuf messages are rendered for logging exclusively through
     `PbToLogValue`, which replaces every `bytes` field, at any nesting depth
     (singular, repeated, map values), with the string "<N bytes>". Raw
     payload bytes never appear in logs. Enum values are rendered by name,
     repeated fields as arrays, map keys as strings, and only populated
     fields appear. This single helper satisfies both the gRPC bytes rule
     and the etcd human-readable rule.

R11. String data written to or read from files is truncated for logging with
     `TruncForLog`: the first `LogStrDataLimit` characters, with a
     "...(N chars total)" suffix when truncated; non-UTF-8 content is
     replaced by "<binary N bytes>". Protobuf files are the exception:
     `ReadProto` and `WriteProto` log the decoded message via `PbToLogValue`
     instead (see `osclient.md`, Logging).

R12. `TraceIdHandler` MUST override `WithAttrs` and `WithGroup` to re-wrap
     the returned inner handler: embedding alone forwards these calls to the
     inner handler and returns the *inner* type, so a derived logger
     (`With`, `WithGroup`) would silently drop trace-id injection. Injection
     happens in `Handle` via `r.AddAttrs`, i.e. as a record attribute rather
     than a handler attribute, so on a logger derived with `WithGroup` the
     `trace_id` lands **inside** that group exactly as any other record
     attribute does. That is slog's own grouping rule, not a defect. dnv's
     own logging points never open a group, so where a record carries
     `trace_id` it is at the top level.

R13. **The change while a daemon runs.** Each of the five daemons watches
     SIGUSR1 and SIGUSR2 from its start to its end (`WatchLogLevel`): SIGUSR1
     sets Debug, and SIGUSR2 sets the level the daemon started with. Both set
     a level outright, never one step up or down, so the result does not hang
     on a level the operator cannot see. Each signal logs `log level
     changed` at Warn, which every `--log-level` shows, with `log_level` (the
     new level's word), `from` (the old one's) and `signal`, a repeat of the
     same level too, so the operator sees that the signal arrived; the
     record carries the trace id the daemon's start mints for it. A change
     lasts until the next signal or the end of the process. `dnvctl`, the
     node check and the gather watch neither signal, and the Go runtime then
     takes no action on one. A child process a daemon started has the
     kernel's default action, which ends it, so an operator signals the main
     process only (`operations.md`, The logs). The two signals are the
     whole interface: no file is polled for a level, no call sets one, and
     SIGHUP keeps its default action.

## Record obligations by subsystem

Record names and attribute names are normative: the integration suites grep
for them. An attribute written "error?" is present only when the operation
returned a non-nil error (R9).

### OS commands and file IO

`LimitedOsClient` emits one record per operation, at the level R9 gives it;
`osclient.md`, Logging, implements them:

* a command run logs `os command` with `cmd`, `args`, `stdin`, `stdout`,
  `stderr`, `exit_code` and "error?";
* a file read logs `os read file`, a file write `os write file` and a direct
  write `os write file direct`, each with `path`, `size` (the bytes read or
  written), `data` (as `TruncForLog` renders it) and "error?";
* a directory listing made in the caller's own process (`ReadDir`) logs
  `os read dir` with `path`, `count` (the names returned), `names` (every
  name, as an array, empty when none) and "error?"; a listing by `ls` is a
  command run;
* a modification-time read made in the caller's own process (`ReadMtime`)
  logs `os read mtime` with `path`, `mtime` (the time read, the zero time
  when the read failed) and "error?";
* a device-number read made in the caller's own process (`ReadDevNo`) logs
  `os read devno` with `path`, `devno` (the number read as "major:minor",
  empty when the read failed) and "error?";
* a raw block read logs `os read block`, a raw block read with O_DIRECT
  (`ReadBlockDirect`) `os read block direct` and a raw block write
  `os write block`, each with `path`, `offset`, `length` and "error?" —
  **never** `data`;
* a set of discards made in the caller's own process (`DiscardRanges`)
  logs `os discard ranges` with `path`, `range_cnt` (the ranges asked
  for), `done` (the ranges discarded), `ranges` (at most the first
  `discardLogRanges`, as `[offset, length]` pairs) and "error?";
* an exclusive open made in the caller's own process (`OpenExclusive`)
  logs `os open exclusive` with `path` and "error?";
* a TCP dial made in the caller's own process (`DialTcp`) logs `os dial`
  with `address` (the "host:port" dialled), `timeout_ms` (the caller's
  timeout), `duration_ms` (how long the dial took) and "error?";
* a detached child's start (`StartDetached`) logs `os start detached` with
  `cmd`, `args`, `pid` (the child's, 0 when it did not start), `output` (the
  file its stdout and stderr go to, empty when it did not start) and
  "error?"; the child's end is not logged, since one stuck in the kernel
  never ends (`osclient.md`, StartDetached);
* a proto read logs `os read proto` and a proto write `os write proto`, each
  with `path`, `size` (the serialized bytes), `data` (the message as
  `PbToLogValue` renders it) and "error?".

Command stdin, stdout and stderr, and the names of a directory listing, are
logged in full; the `TruncForLog` truncation applies to file data only. The
three raw-block records — the DN's on-disk metadata path (`architecture.md`,
[D13]) and the CN's superblock read (`cnagent.md` CN12) — deliberately carry
no `data` attribute at all: the blocks are large, opaque, and may hold
arbitrary user data.

Two block-IO records are emitted **outside** `LimitedOsClient`, by the cn
agent's `directLegProbeIO`, because the CN11 leg health probers deliberately
bypass the OsClient semaphore (see `osclient.md`, Exported raw helpers and
the probe-IO carve-out): the probe's write half logs `probe write block` and
its direct read half `probe read block direct`, each with `path`, `offset`,
`length`, `duration_ms` (how long the half took, the write half a buffered
write and an fsync) and "error?" — **never** `data`. They are "probe …" and not "os …"
on purpose: the cn suite's mutation grep names `os write block` explicitly
(`cnagent_integtest.md`, Conventions), and the continuous health probes must
fall out of that list by construction rather than by a path-based exemption.
Everything else about them — one record per operation, emitted on
completion, under the caller's trace id, with "error?" only on failure, at
Debug when the half succeeded and at Info when it failed, as a read's (R9),
since the probe runs every probe interval on every leg — follows the same
rules as the records above.

### Agent start

An agent's start logs `agent node versions` (`agent.LogStartVersions`),
before it serves (`dnagent.md` CM4), with `version` (the build version of
the running binary, `common.BuildVersion`; `architecture.md`, Components:
invocation reference), `kernel_release` (the running kernel's release) and,
for the cn role, `mdadm_version` (the version line of the mdadm the agent
runs; `cnagent.md` CN-CM2). A read that failed
leaves its attribute empty and adds `error`, the failed reads' errors
joined, and the record is then at Warn; the start goes on either way.

A cn agent whose start check finds a cause (`agent.HostCutCauses`), and
which `--ignore-host-disconnection` lets start, logs, after that record and
before it serves, one Warn record `agent host cut disabled` with `causes`,
the start check's causes joined by "; " (`cnagent.md` CN-CM2); without the
flag the start ends with the error and no record of its own (`dnagent.md`
CM3). An agent whose check finds no cause logs neither.

### The cut of a removed host

A cn agent's cut of a stray connection (`cnagent.md` CN33) logs, under the
converge's trace id, with `nqn` (the subsystem), and per connection
`connection` (its directory name) and `host_nqn`: `stray connection ended`
at Info for each write of "fatal" the kernel took, which a connection
that has gone before the write, its file not there or answering EIO, does
not log; `ending a stray connection failed` at Error, with `error`, for a
write the kernel refused, which ends the cut not held, the only record of
the cut that names the refusal, which the row does not (it reads the
stray still connected, or under a lockdown the lockdown file shows "nvmet connections
not ended: lockdown <mode>"), while the record of the write itself (`os
write file direct`) carries the error as well; a read of the debug
directory that fails, a written connection's "state" or a host read just
before a write among them, has no record of the cut and is in the
`OsClient`'s record of that read, the row naming a failed listing or host
read ("nvmet connections not read"), and one refused an `OsClient` slot
has no record at all (`osclient.md`, Logging) and shows only in the row's
text, when that names it; and `stray connection wait spent` at Info, with
`host_nqn` of the first stray connection still listed and `strays`, their
count, when the pass's budget, or the end of the converge's own context
(its caller gone, or the agent shutting down), ended the wait before the
cut held, since the next converge cuts again (R6). The names are apart from the cut of a
migrating leg's path, `leg path cut` (`cnagent.md` CN10).

### gRPC messages

Every request and reply message of the dnv-internal services (`Gateway`,
`DiskNodeAgent`, `ControllerNodeAgent`) is logged by the shared interceptors
with `method` (the full method string) and `data` (the message as
`PbToLogValue` renders it), so a bitmap push logs its `bitmap` as a byte
count instead of the payload. `grpc.md` owns the record names, their levels,
the ordering and the stream rules (L1 to L6).

### etcd

etcd keys in dnv are human-readable space-joined strings and values are
binary-serialized protobuf messages (`architecture.md`, etcd data model).
All etcd access, plain client and STM alike, goes through the central
helpers of the `etcdutil` package (`dnv-worker.md`, EU1 to EU4), which do
the proto (un)marshal **and** the logging. Direct `clientv3` or STM
`Get`/`Put` calls with ad-hoc marshaling elsewhere are forbidden — otherwise
the rule cannot be enforced. The records, at the levels of R9 — a get and a
range at Debug when they answered and at Info when they failed, a watch
event at Debug unless its value did not decode, a put and a delete at Info:

* a point read logs `etcd get` with `key`, `found`, `value` (the decoded
  message as `PbToLogValue` renders it; present only when the key was found
  and its stored value decoded: a value that did not decode logs `found`
  true with `error` and no `value`) and "error?";
* a write logs `etcd put` with `key`, `value` (the message being stored) and
  "error?";
* a delete logs `etcd delete` with `key` and "error?";
* a range scan logs `etcd range` with `prefix`, `count` (the keys returned)
  and "error?" — it does not dump every value of a range; the individual
  keys of interest are then read and logged with `etcd get` semantics;
* a watch event logs `etcd watch event` with `key`, `type` (`put` or
  `delete`), `value` (puts only) and "error?".

Values are always logged decoded (`PbToLogValue`), never as raw or base64
protobuf bytes. A put whose value fails to unmarshal still emits its
`etcd watch event` record, carrying `error` and **no** `value` — there is no
decoded message to render. The unmarshal error then ends that watch
*generation*, not the watch: the error names the key itself, the consumer
logs it on the way out — each `WatchTyped` consumer with a record of its
own, the worker's named in `dnv-worker.md`, Log records, and the cdc's in
`cdc.md` WV4 — and its outer loop rescans and re-opens the watch, so this
record is not the only trace of which key broke it. Inside an STM a put or
a delete logs when it is held back for the commit, with no error, so its
`etcd put` or `etcd delete` record says only that the write was staged and
carries no commit outcome, which reaches the caller only through the returned
error. A transaction function that fails after the write was held back, or a
commit etcd refuses, leaves such a record for a write that never happened; a
commit that times out leaves one for a write that may or may not have happened.
Inside an STM, reads and writes may be re-executed on transaction retry; each
attempt logs, so duplicate records for retried transactions are expected and
acceptable.

### Leftovers

An agent decides what to REMOVE by comparing what its node actually holds
with what the desired state wants (`architecture.md`, Teardown by sweep),
so "something is left over" is the verdict of one comparison, recomputed by
every converge and every background read, and kept only as the code of the
observation each makes (`dnagent.md` SH28, SH29). Two records carry it, one
on each side of the RPC; they are specified here rather than in one
subsystem's document because an operator chasing a leaked device greps both
logs for the same thing.

* The agent logs `sweep leftover` (`agent/sweep.go`) on a sweep, or a
  background read's read-only verdict, that was not clean; a read logs it
  under the trace id of the round that started the read (`dnagent.md`
  SH29). It carries the object's ids — `cluster_id` plus `cn_id` or `dn_id`
  for a node-level pass, plus `sp_id` and `cntlr_id` or `side_id` for an
  object-level one — then `leftover_cnt`, `leftovers` (the sorted
  "kind:name" entries joined by a space; `kind` is one of the
  `LeftoverKind*` labels of `agent/sweep.go`, and a cntlr's read-only
  verdict lists there, each under a kind of its own, the md members, the
  broken arrays and the absent arrays `cnagent.md` CN30 names) and
  `failures`: the steps that could not prove the scope clean — an
  enumeration that did not answer, a record the pass could not free, or, on
  the dn, a disk whose identity is not confirmed or a side with bytes still
  to zero and no zeroing goroutine that is not a migration destination
  whose side device row reads the clone-metadata area's refusal of its run
  (`dnagent.md` DN13, DN16), or, on either role, each row that a converge
  or a background read found absent or different where a re-sent `Syncup*`
  would cure it, on a cntlr a leg wrapper, a `RedundNone` group's dm-linear
  and a dm-clone a converge would change among them (rendered "absent" or
  "differs" with the row's words), and on a cntlr a subsystem with a stray
  connection still there (rendered "pending" with the row's words,
  `cnagent.md` CN33) and, on a read alone, a subsystem whose nvmet
  connections could not be read or that a lockdown the lockdown file shows
  keeps from being ended (rendered "differs" with the row's words,
  `cnagent.md` CN30, CN33), which a re-send may not cure, or the node's
  nvmet port in a state one would rewrite (`dnagent.md` SH31, DN16), or, on
  a cntlr, a side of a wanted leg or an entry of a clone's source with no
  controller that the converge would connect, a migrating leg's path the
  converge would cut, a
  forced-start mark of the request the agent holds while that request does
  not count as delivered, a group's forced start its converge would carry
  through, a group's md check its converge would start, stop or adopt, or
  an uncreated snapshot whose create message its converge would send
  (`cnagent.md` CN8, CN10, CN12, CN14, CN30) — each rendered "what: error"
  and joined by "; ".
* The worker logs `syncup leftover` (`worker/revision.go`) on a `Syncup*`
  reply carrying `ReplyCodeLeftover`, with the object's ids, `revision`
  (the one the request carried) and `details` (the agent's own leftover
  text).

Both are Info: a leftover is a normal state for as long as a dead remote's
failfast window lasts, on the pass of a cn teardown that sets its
"nvme disconnect" commands going (they run off the pass, `cnagent.md` CN10),
and for as long as such a delete waits out the kernel's admin timeout. A
disconnect still running goes when it completes, and the next background
read or `Syncup*` finds it gone; a later `Syncup*` sweeps away any other
leftover and issues again a disconnect that failed. Nothing on the agent
re-drives that sweep because of a leftover: the worker re-issues the syncup
on the code (`dnv-worker.md` RW4). The agent's record is emitted once by
each sweep or read whose verdict was not clean, the worker's once per such
`Syncup*` reply, so a leftover that the converges of the re-sent `Syncup*`
calls do NOT remove is in both logs at every re-send, which is exactly
what distinguishes it from one the next pass removed. A leftover that only a
background read's verdict names, and that the `Syncup*` it brings does not
report — an md member that syncup's converge fails early, puts back, adds
or removes for a repair, a broken array it assembles again or an absent
array its assembly starts (`cnagent.md` CN12, CN30), a failure a cntlr's
verdict names only on a read, such as a forced start, an md check, an
uncreated snapshot or a source entry with no controller (`cnagent.md`
CN30), or a subsystem directory that outlives its last controller — is in
the agent's record alone, under the trace id of the round that started
the read. The worker re-sends the `Syncup*` on the
round that answers from that read's observation, under that round's own
trace id, and its `syncup result` record of the re-send also names the
observation's trace id (`dnv-worker.md` RW4, RW10): the read's id joins
the agent's record to that `syncup result` record, and the round's id
joins that record to the converge the re-send brings. That converge's observation carries its own
code, which does not name such a leftover, so only a read after it names
it again (`dnagent.md` SH28). A clean pass or read emits nothing;
`sweep leftover` is never a "nothing to report" record. The
agent's record carries the FULL list, while `details` — in the reply and
therefore in `syncup leftover` — shows at most `maxLeftoverNames` leftover
names before a "[+k more]" tail, so that one stuck node cannot fill the
reply with names, and at most as many failure lines, each with its error
cut to `AgentTextMax` (`architecture.md`, Live-state reporting), before a
"[+k more failures]" tail.
