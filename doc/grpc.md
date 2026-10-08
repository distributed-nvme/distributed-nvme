# grpc.md — gRPC interceptors

This document owns the four gRPC interceptors every dnv connection and server
chains: the trace-id propagation between context and gRPC metadata, the
logging of every request and reply message with its record names and
ordering, the wiring rule that puts both chains on every dnv client
connection and server, and the conventions the integration-test drivers and
fakes follow instead. It leans on `log.md` for the helpers it reuses
(`WithTraceId`, `TraceIdFromCtx`, `TraceIdMetadataKey`, `PbToLogValue`) and
for its rules on Info records and typed attributes, on `schema.proto` for the
three services (`Gateway`, `DiskNodeAgent`, `ControllerNodeAgent`), and on
`architecture.md`, System overview, for who calls whom.

## Placement

The interceptors live in package `common`, in `common/interceptor.go`. They
apply to dnv-internal gRPC only: the `Gateway` service (dnvctl and users to
dnv-gateway) and the agent services (dnv-gateway and dnv-worker to
dnv-agent). They are not attached to the etcd client, because a message
logger on etcd's own RPCs would log raw key and value bytes and violate the
human-readable-data rule: etcd logging is implemented once, inside the
central helpers of `etcdutil` (`log.md`, etcd), not at the call sites.

Of the RPCs `schema.proto` declares, the four `Check*` RPCs — `CheckDn`,
`CheckSide`, `CheckCn`, `CheckCntlr` — are bidirectional streams and every
other RPC is unary, so four interceptors are required: unary client
(`GrpcUnaryClientInterceptor`), stream client
(`GrpcStreamClientInterceptor`), unary server
(`GrpcUnaryServerInterceptor`) and stream server
(`GrpcStreamServerInterceptor`). They depend on the gRPC and protobuf
modules (`dependencies.md`, Direct dependencies).

## Trace-id propagation

T1. **Client side (unary and stream)**: if the outgoing ctx carries a trace id
    (`TraceIdFromCtx`), append it to the outgoing gRPC metadata under
    `TraceIdMetadataKey` with `metadata.AppendToOutgoingContext`. If the ctx
    carries none, do nothing — the interceptor never invents a trace id.

T2. **Server side (unary and stream)**: if the incoming metadata carries a
    non-empty value under `TraceIdMetadataKey`, take the first value and put
    it into the request ctx with `WithTraceId` **before** any logging and
    before invoking the handler, so every downstream record (handler logic,
    `OsClient` calls, etcd calls, further outbound RPCs) carries it. For
    streams this means wrapping the `grpc.ServerStream` so its `Context`
    method returns the enriched ctx.

T3. Propagation is transitive end to end by construction: dnvctl sets an id
    (T4), the client interceptor puts it in metadata, the gateway's server
    interceptor puts it in ctx, the gateway's outbound agent calls go through
    the client interceptor with that ctx, the agent's server interceptor
    restores it, and the agent's `OsClient` and state-file logs carry the
    same `trace_id`. A stream's metadata travels once, when the stream
    opens, so a long-lived `Check*` stream (`architecture.md`, Check
    streams) carries in its metadata only the id of the worker round that
    opened it, while every round runs under an id of its own
    (`dnv-worker.md` RW10). On these streams the id therefore also travels
    in the request: every `Check*Request` the worker sends carries its
    round's id in `trace_id`, and the agent runs the round under it
    (`agent.CheckRoundCtx`, `dnagent.md` SH24), so that round's
    `os command` and file records carry the round's id; an empty `trace_id`
    keeps the stream's. The stream's own per-message records (L4) are logged
    under the stream ctx and keep the id its metadata brought at open; a
    round's id shows in the `data` of its request's `grpc client send` and
    `grpc server recv` records.

T4. Minting trace ids is the entry points' job, never the interceptors':
    the shared interceptors only move an id that is already there between
    ctx and metadata (T1, T2). `dnvctl` mints one per CLI invocation unless
    `--trace-id` supplies one (`dnvctl.md` CT2); `dnv-worker` one per unit
    of work, an in-round syncup sharing the round's id (`dnv-worker.md`
    RW10); `dnv-gateway` one for a request that arrived without one
    (`gateway.md` GW2), not in its handlers but in its entry-point mint,
    `ensureTraceIdUnary` and `ensureTraceIdStream` (`gateway/traceid.go`),
    which `serverOptions` chains first, upstream of the shared pair, so the
    shared chain adopts the id as it adopts a client's and logs the request
    under it. Each daemon also mints one at startup, for its startup and
    other process-lifetime records; `dnv-agent` mints one per attempt of a
    background task, all but one: the cn sweep's background
    "nvme disconnect" runs under the id of the pass that set it going and
    mints one only when that pass had none (`dnagent.md` SH27, `cnagent.md`
    CN10); and `dnv-cdc` mints one per accepted host connection, one per
    scan attempt (the watch it opens included) and one per applied watch
    event. The generator is `NewTraceId` (`log.md`, Placement); the ids the
    worker mints per unit of work put a prefix of its seed in front of the
    generator's output (`dnv-worker.md` RW10).

## Message logging

L1. Every request and reply **message** is logged at Info with
    `slog.InfoContext` using the trace-enriched ctx. Attributes: `method`
    (the full method string), `data` (`slog.Any` of the message as
    `PbToLogValue` renders it), and "error?" (only when the call or step
    failed). `PbToLogValue` guarantees that `bytes` fields — the `bitmap`
    payloads of `Push*Bitmap`, `Append*Bitmap` and `Get*Bitmap`/`Get*Bm` —
    are logged as a byte count only. Only populated fields appear
    (`log.md` R10), so a reply whose fields all hold their zero values
    renders as an empty object; that is acceptable.

L2. The record names are normative:

    * client side: `grpc client request` (unary request sent),
      `grpc client reply` (unary reply received), `grpc client stream open`
      (stream opened), `grpc client send` (stream message sent) and
      `grpc client recv` (stream message received);
    * server side: `grpc server request` (unary request received),
      `grpc server reply` (unary reply sent), `grpc server stream open`
      (stream opened), `grpc server recv` (stream message received),
      `grpc server send` (stream message sent) and
      `grpc server stream close` (stream handler returned).

L3. Unary ordering: the client logs the request before invoking and the
    reply or error after; the server logs the request after trace extraction
    (T2) and the reply or error after the handler returns. On error, the
    reply record carries `method` and `error` and omits `data` (the reply
    message is not meaningful).

L4. Stream rules: `stream open` records carry `method` (plus "error?" if
    opening failed). Each `SendMsg` and `RecvMsg` logs one record per
    message. A client `RecvMsg` returning `io.EOF` is the normal end of
    stream and is **not** logged; a server `RecvMsg` returning `io.EOF`
    likewise. Any other send or receive error logs `method` and `error`
    without `data`. There is no client-side "stream close" record (streams
    end via EOF or ctx); the server logs `grpc server stream close` when the
    handler returns.

L5. If a message is not a `proto.Message` (defensive; it does not happen
    with generated code), fall back to `slog.Any` of the raw message.

L6. The long-lived `Check*` streams (`architecture.md`, Check streams) carry
    one request/reply pair per health round; with per-message logging each
    round produces its own records, just as every unary `Syncup*` and
    `Push*Bitmap` call produces one request and one reply record — which is
    exactly the intent of the "all grpc client/server request/reply" rule
    (`log.md` R8).

## Wiring

Every dnv binary uses both chain options on every dnv connection and server.
A client connection (dnvctl to the gateway; the gateway and the worker to the
agents) is dialed with `grpc.NewClient` under plaintext transport credentials
(`insecure.NewCredentials`) and both client chain options,
`grpc.WithChainUnaryInterceptor` with `GrpcUnaryClientInterceptor` and
`grpc.WithChainStreamInterceptor` with `GrpcStreamClientInterceptor`. A server
(the gateway's `Gateway` service; the agents' `DiskNodeAgent` and
`ControllerNodeAgent`) is built with `grpc.NewServer` under both server chain
options, `grpc.ChainUnaryInterceptor` with `GrpcUnaryServerInterceptor` and
`grpc.ChainStreamInterceptor` with `GrpcStreamServerInterceptor`; the gateway
chains its trace-id mint ahead of them (T4).

Who is a server or a client of whom:

| binary | server interceptors on | client interceptors on |
|---|---|---|
| dnv-gateway | its `Gateway` gRPC server | its connections to dn and cn agents (`gateway.md`, Agent calls) |
| dnv-worker | — | its connections to dn and cn agents: `Syncup*`, `Push*Bitmap` and the `Check*` streams; the worker never calls `Get*Info` |
| dnv-agent, dn and cn roles | its `DiskNodeAgent` or `ControllerNodeAgent` server | — |
| dnvctl | — | its connection to the gateway (it mints a trace id per invocation unless `--trace-id` supplies one, T4) |
| dnv-cdc | — | — (no dnv-internal gRPC: it talks to etcd and serves hosts over NVMe/TCP, `cdc.md`, Scope and placement) |

Those dnv binaries (`log.md`, Placement) are the whole of the rule. The
`integtest/` drivers are not dnv components; the conventions they follow
instead are the next section's.

## Drivers and fakes

The `integtest/` drivers are scoped out of the wiring rule on purpose.

* `gatewayctl`, `cnagentctl` and `dnagentctl` dial **without** the client
  interceptors. A driver is not a dnv component, and its own request and
  reply records would only duplicate what the gateway or agent it calls
  already logs — the suites read the daemon's log, not the driver's. Each
  instead passes its `--trace-id` as plain outgoing metadata
  (`metadata.AppendToOutgoingContext` with `TraceIdMetadataKey`), which is
  all T2 needs on the far side, so the T3 chain the suites assert holds
  unchanged; `dnagentctl` and `cnagentctl` also put it in the `trace_id` of
  their `Check*` requests, as the worker does (T3).
* `integtest/fakeagent` installs both server interceptors of the wiring
  rule: it stands in for an agent, and its `agent.log` is the record the
  suites read for what the worker sent — `worker_test.sh` matches
  `grpc server *` records only, never the worker's own client-side ones,
  which carry the same payloads. `integtest/fakegateway` does the same for
  the dnvctl suite: it serves every `Gateway` method behind both server
  interceptors, and its `fakegateway.log` is the record `dnvctl_test.sh`
  reads for which RPC dnvctl put on the wire and under which trace id — it
  selects `grpc server request` records by `trace_id` and `method`; the
  request payloads it asserts come from the fake's `state.json`
  (`dnvctl.md`, Integration test plan).
* No driver installs a logger of its own: each inherits `common`'s
  default chain, which is what keeps its stdout the result channel
  the suites parse (`log.md` R3). The only log records reaching a driver's
  stderr are the `etcd *` ones `etcdutil` emits under `workerctl` and
  `cdcctl` — `gatewayctl` calls nothing that logs, and `dnagentctl` and
  `cnagentctl` never call `slog` — while each driver's die and usage
  diagnostics share that stream as plain text. `fakeagent` and `fakegateway`
  inherit the same default and, as daemons emitting an interceptor record per
  gRPC message, log far more than a one-shot driver; the suite that launches
  each captures its stderr into the log file it reads.
