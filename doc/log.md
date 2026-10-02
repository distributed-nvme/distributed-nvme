# log.md — Logging

This document owns the JSON logging every dnv binary shares: the handler
chain and the stream it writes to, the trace id that joins the records of one
operation across processes, the per-binary log levels, the attribute
discipline, the shared rendering helpers, and the records each subsystem must
emit. `osclient.md` and `grpc.md` build on the helpers defined here;
`architecture.md` gives the system context and `dnvctl.md` the CLI's output
contract that the choice of stream serves.

## Placement

All logging code is shared and lives in package `common`, in `common/log.go`.
Every dnv binary (`dnv-gateway`, `dnv-worker`, `dnv-agent`, `dnv-cdc`,
`dnvctl`) imports `common`, so its `init` installs the default logger before
any `main` runs. `NewTraceId` relies on `rand.Read` of `crypto/rand` never
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
    CT7), and nothing from a running daemon (cobra's own `--help` text is
    all four ever put there). A consumer that reads stdout alone therefore
    gets the result and never a log record. The etcd client's own logger is
    silenced (`zap.NewNop`, in `etcdutil`) so that stderr carries one JSON
    stream and it is dnv's; no dnv record comes from anything but
    `log/slog`. All handler options stay at their defaults except `Level`
    (see R6), the one deliberate deviation from nil options, required by the
    per-binary level rule.

R3. `common`'s `init` calls `slog.SetDefault` with that chain, and that call
    is the only `slog.SetDefault` in the repository outside test files. The
    dnv binaries do not build their own loggers: the chain's handler holds
    `logLevel`, a `slog.LevelVar`, so `SetLogLevel` (R6) still bites after
    `init` has run, which is precisely what a second handler built around a
    static level would throw away. The `integtest/` drivers are not dnv
    binaries, and they build no logger either: they inherit this stderr
    default, which is exactly what keeps their stdout the result channel the
    suites parse.

R4. Every log call uses the context-aware form — `slog.InfoContext`,
    `slog.WarnContext`, `slog.ErrorContext` — and passes the request-scoped
    ctx. Use `context.Background` explicitly only when no operation context
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
    without the client interceptors (`grpc.md`, Drivers and fakes) — and two places that move an id they
    were handed: the `Check*` streams, whose requests carry each round's id
    in a `trace_id` field that the worker fills and the agent adopts
    (`grpc.md` T3), and the cn sweep's background "nvme disconnect", which
    puts the id of the pass that set it going onto the agent's lifetime ctx
    (`rootCtx`) and mints one only if that pass had none (`dnagent.md` SH27,
    `cnagent.md` CN10). On a logger derived with `WithGroup` the attribute
    is nested inside that group rather than at the top level — see R12 — so
    a consumer that greps `trace_id` positionally must account for it.

R6. Log levels per binary: the four daemons — `dnv-gateway`, `dnv-worker`,
    `dnv-agent` in both roles and `dnv-cdc` — log at Info; `dnvctl` logs at
    Warn. Implemented with a package-level `slog.LevelVar`, whose zero value
    is Info, plus `SetLogLevel`. The four daemons do nothing; `dnvctl` calls
    `SetLogLevel` with `slog.LevelWarn` as the first statement of its `main`.
    Because all the R8 logging points are Info, they are automatically
    silenced in dnvctl — this is intended.

R7. Attributes are always built with the strongly-typed constructors of
    `log/slog` — `slog.String`, `slog.Int`, `slog.Int64`, `slog.Uint64`,
    `slog.Bool`, `slog.Any` and their typed siblings. Never pass loose
    alternating key, value arguments. Use `slog.Any` only for slices, maps
    and the `PbToLogValue` output.

R8. The following events MUST be logged at **Info** level, with the record
    names and attributes of "Record obligations by subsystem" below:
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
       implemented in the central etcd helpers of `etcdutil`.

R9. Each logging point emits exactly **one** record per operation, on
    completion, so the record can carry the outcome (exit code, error). When
    the operation failed, the same Info record additionally carries an
    `error` attribute holding the error's text. These logging points do not
    use other levels; failures still reach the caller through the returned
    error.

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
     replaced by "<binary N bytes>" (this is the "the code should decide"
     resolution for bytes data). Protobuf files are the exception:
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

## Record obligations by subsystem

Record names and attribute names are normative: the integration suites grep
for them. An attribute written "error?" is present only when the operation
returned a non-nil error (R9).

### OS commands and file IO

`LimitedOsClient` emits one record per operation; `osclient.md`, Logging,
implements them:

* a command run logs `os command` with `cmd`, `args`, `stdin`, `stdout`,
  `stderr`, `exit_code` and "error?";
* a file read logs `os read file`, a file write `os write file` and a direct
  write `os write file direct`, each with `path`, `size` (the bytes read or
  written), `data` (as `TruncForLog` renders it) and "error?";
* a raw block read logs `os read block` and a raw block write
  `os write block`, each with `path`, `offset`, `length` and "error?" —
  **never** `data`;
* a proto read logs `os read proto` and a proto write `os write proto`, each
  with `path`, `size` (the serialized bytes), `data` (the message as
  `PbToLogValue` renders it) and "error?".

Command stdin, stdout and stderr are logged in full; the `TruncForLog`
truncation applies to file data only. The two raw-block records — the DN's
on-disk metadata path (`architecture.md`, [D13]) — deliberately carry no
`data` attribute at all: the blocks are large, opaque, and may hold
arbitrary tenant bytes.

Two block-IO records are emitted **outside** `LimitedOsClient`, by the cn
agent's `directLegProbeIO`, because the CN11 leg health probers deliberately
bypass the OsClient semaphore (see `osclient.md`, Exported raw helpers and
the probe-IO carve-out): the probe's write half logs `probe write block` and
its direct read half `probe read block direct`, each with `path`, `offset`,
`length` and "error?" — **never** `data`. They are "probe …" and not "os …"
on purpose: the cn suite's mutation grep names `os write block` explicitly
(`cnagent_integtest.md`, Conventions), and the continuous health probes must
fall out of that list by construction rather than by a path-based exemption.
Everything else about them — one Info record per operation, emitted on
completion, under the caller's trace id, with "error?" only on failure —
follows the same rules as the records above.

### gRPC messages

Every request and reply message of the dnv-internal services (`Gateway`,
`DiskNodeAgent`, `ControllerNodeAgent`) is logged by the shared interceptors
with `method` (the full method string) and `data` (the message as
`PbToLogValue` renders it), so a bitmap push logs its `bitmap` as a byte
count instead of the payload. `grpc.md` owns the record names, the ordering
and the stream rules (L1 to L6).

### etcd

etcd keys in dnv are human-readable space-joined strings and values are
binary-serialized protobuf messages (`architecture.md`, etcd data model).
All etcd access, plain client and STM alike, goes through the central
helpers of the `etcdutil` package (`dnv-worker.md`, EU1 to EU4), which do
the proto (un)marshal **and** the logging. Direct `clientv3` or STM
`Get`/`Put` calls with ad-hoc marshaling elsewhere are forbidden — otherwise
the rule cannot be enforced. The records, all Info, via `slog.InfoContext`:

* a point read logs `etcd get` with `key`, `found`, `value` (the decoded
  message as `PbToLogValue` renders it; omitted when not found) and
  "error?";
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
logs it on the way out — each of the four `WatchTyped` consumers with its
own record, so grep for all of `cluster conf watch restarting`,
`rev watch restarting` and `worker reg watch restarting` on the worker and
`cdc watch restarting` on the cdc (`cdc.md`, WV4) — and its outer loop
rescans and re-opens the watch, so this record is not the only trace of
which key broke it. Inside an STM, reads and writes may be re-executed on
transaction retry; each attempt logs, so duplicate records for retried
transactions are expected and acceptable.

### Leftovers

An agent decides what to REMOVE by comparing what its node actually holds
with what the desired state wants (`architecture.md`, Teardown by sweep),
so "something is left over" is the verdict of one comparison, recomputed
every pass and stored nowhere. Two records carry it, one on each side of
the RPC; they are specified here rather than in one subsystem's document
because an operator chasing a leaked device greps both logs for the same
thing.

* The agent logs `sweep leftover` (`agent/sweep.go`) on a sweep or
  read-only verdict that was not clean. It carries the object's ids —
  `cluster_id` plus `cn_id` or `dn_id` for a node-level pass, plus `sp_id`
  and `cntlr_id` or `side_id` for an object-level one — then `leftover_cnt`,
  `leftovers` (the sorted "kind:name" entries joined by a space; `kind` is
  one of `dm`, `md`, `nvmet`, `nvmet_ns`, `nvme` and `record`) and
  `failures`: the steps that could not prove the scope clean — an
  enumeration that did not answer, a record the pass could not free, or, on
  the dn, a disk whose identity is not confirmed or a side with extents
  still to zero and no zeroing goroutine, or, on the cn, a piece of the
  node's base state a `CheckCn` round's or `GetCnInfo`'s probe read absent,
  or an ANA group it read in a state other than its fixed one on a port
  whose transport attributes match — each rendered "what: error" and joined
  by "; ".
* The worker logs `syncup leftover` (`worker/revision.go`) on a `Syncup*`
  reply carrying `ReplyCodeLeftover`, with the object's ids, `revision`
  (the one the request carried) and `details` (the agent's own leftover
  text).

Both are Info: a leftover is a normal state for as long as a dead remote's
failfast window lasts, on the pass of a cn teardown that sets its
"nvme disconnect" commands going (they run off the pass, `cnagent.md` CN10),
and for as long as such a delete waits out the kernel's admin timeout. A
disconnect still running goes when it completes, and the next `Check*`
verdict or `Syncup*` probe finds it gone; a later `Syncup*` sweeps away any
other leftover and issues again a disconnect that failed. Nothing on the
agent re-drives that sweep because of a leftover — a `Check*` round
recomputes the verdict and removes nothing, and the agents' own background
converges (`cnagent.md` CN10's connect retry, `dnagent.md` DN13's connect
retry and DN12's fence timer) sweep only while one is registered or armed
for a reason of its own — so what brings the removal back is the worker
seeing the leftover code again and re-issuing the syncup (`dnv-worker.md`,
RW4). Both are emitted once per pass — the agent's on every sweep and every
verdict that was not clean, the worker's on every such reply — so a
leftover that does NOT go away is in both logs every round, which is
exactly what distinguishes it from one the next pass removed. A clean pass
emits nothing; `sweep leftover` is never a "nothing to report" record. The
agent's record carries the FULL list, while `details` — in the reply and
therefore in `syncup leftover` — shows at most `maxLeftoverNames` leftover
names before a "[+k more]" tail, so that one stuck node cannot fill the
reply with names; the failure lines that follow them are not capped.
