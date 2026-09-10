# grpc.md — gRPC Interceptor Specification (dnv)

Status: **normative**. Read `log.md` first — this document reuses
`WithTraceId`, `TraceIdFromCtx`, `TraceIdMetadataKey` and `PbToLogValue`, and
follows its rules (Info records, typed attributes).

Required background: `schema.proto` (services `Gateway`, `DiskNodeAgent`,
`ControllerNodeAgent`; note that the four `Check*` RPCs — `CheckDn`, `CheckSide`,
`CheckCn`, `CheckCntlr` — are bidirectional streams and every other RPC is unary),
`architecture.md` §1/§9/§10 (who calls whom), `log.md`.

---

## 1. Scope and placement

* File: `common/interceptor.go` (+ `common/interceptor_test.go`), package
  `common`.
* The interceptors apply to **dnv-internal gRPC** only: the `Gateway` service
  (dnvctl/users → dnv-gateway) and the agent services (dnv-gateway/dnv-worker
  → dnv-agent). They are **not** attached to the etcd client — etcd logging is
  done at the call sites per `log.md` §5.3 (attaching a message logger to
  etcd's own RPCs would log raw key/value bytes, violating the
  "human readable data" rule).
* Because `schema.proto` uses both unary and bidirectional-streaming RPCs,
  four interceptors are required: unary client, stream client, unary server,
  stream server.
* Dependencies: `google.golang.org/grpc`, `google.golang.org/protobuf`.

## 2. Normative requirements

### 2.1 Trace-id propagation

T1. **Client side (unary and stream)**: if the outgoing ctx carries a trace id
    (`TraceIdFromCtx`), append it to the outgoing gRPC metadata under
    `TraceIdMetadataKey` (`"trace_id"`) with
    `metadata.AppendToOutgoingContext`. If the ctx carries none, do nothing —
    the interceptor never invents a trace id.

T2. **Server side (unary and stream)**: if the incoming metadata contains a
    non-empty `"trace_id"` value, take the first value and put it into the
    request ctx with `WithTraceId` **before** any logging and before invoking
    the handler, so every downstream record (handler logic, `OsClient` calls,
    etcd calls, further outbound RPCs) carries it. For streams this means
    wrapping the `grpc.ServerStream` so `Context()` returns the enriched ctx.

T3. Propagation is transitive end to end by construction: dnvctl mints an id →
    client interceptor puts it in metadata → gateway server interceptor puts
    it in ctx → gateway's outbound agent calls go through the client
    interceptor with that ctx → agent server interceptor restores it → the
    agent's `OsClient`/state-file logs carry the same `trace_id`.

T4. Minting trace ids is the entry points' job, not the interceptors'
    (non-normative recommendation): `dnvctl` creates one per CLI invocation,
    `dnv-worker` one per sync/health round, `dnv-gateway` handlers MAY create
    one when a request arrived without one. A sufficient generator:

    ```go
    func NewTraceId() string {
        var b [8]byte
        rand.Read(b[:]) // crypto/rand
        return hex.EncodeToString(b[:])
    }
    // ctx := common.WithTraceId(context.Background(), common.NewTraceId())
    ```

    (Put `NewTraceId` in `common/log.go` if implemented.)

### 2.2 Message logging

L1. Every request and reply **message** is logged at Info with
    `slog.InfoContext` using the (trace-enriched) ctx. Attributes:
    `method` (the full method string, e.g. `/DiskNodeAgent/SyncupSide`),
    `data` (`slog.Any` of `PbToLogValue(msg)`), and `error?` (only when the
    call/step failed). `PbToLogValue` guarantees that `bytes` fields — the
    `bitmap` payloads of `Push*Bitmap`, `Append*Bitmap`, `Get*Bitmap`/`Get*Bm`
    — are logged as `"<N bytes>"` only.

L2. Canonical `msg` strings (normative):

    | side | event | msg |
    |---|---|---|
    | client | unary request sent | `grpc client request` |
    | client | unary reply received | `grpc client reply` |
    | client | stream opened | `grpc client stream open` |
    | client | stream message sent | `grpc client send` |
    | client | stream message received | `grpc client recv` |
    | server | unary request received | `grpc server request` |
    | server | unary reply sent | `grpc server reply` |
    | server | stream opened | `grpc server stream open` |
    | server | stream message received | `grpc server recv` |
    | server | stream message sent | `grpc server send` |
    | server | stream handler returned | `grpc server stream close` |

L3. Unary ordering: client logs the request before invoking and the
    reply/error after; server logs the request after trace extraction (T2) and
    the reply/error after the handler returns. On error, the reply record
    carries `method` + `error` and omits `data` (the reply message is not
    meaningful).

L4. Stream rules: `stream open` records carry `method` (+ `error?` if opening
    failed). Each `SendMsg`/`RecvMsg` logs one record per message. A client
    `RecvMsg` returning `io.EOF` is the normal end of stream and is **not**
    logged; a server `RecvMsg` returning `io.EOF` likewise. Any other
    `Send/Recv` error logs `method` + `error` without `data`. There is no
    client-side "stream close" record (streams end via EOF/ctx); the server
    logs `grpc server stream close` when the handler returns.

L5. If a message is not a `proto.Message` (defensive; should not happen with
    generated code), fall back to `slog.Any("data", msg)`.

L6. The long-lived `Check*` streams of `architecture.md` §9.7 carry one
    request/reply pair per health round; with per-message logging each round
    produces its own records, just as every unary `Syncup*`/`Push*Bitmap` call
    produces one request and one reply record — which is exactly the intent of
    the "all grpc client/server request/reply" rule.

## 3. Reference implementation — `common/interceptor.go` (complete)

```go
package common

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// pbLogAttr renders a gRPC message for logging (log.md R10, grpc.md L1/L5).
func pbLogAttr(key string, msg any) slog.Attr {
	if pbMsg, ok := msg.(proto.Message); ok {
		return slog.Any(key, PbToLogValue(pbMsg))
	}
	return slog.Any(key, msg)
}

// attachTraceId copies the ctx trace id into the outgoing metadata (T1).
func attachTraceId(ctx context.Context) context.Context {
	if traceId, ok := TraceIdFromCtx(ctx); ok {
		return metadata.AppendToOutgoingContext(
			ctx, TraceIdMetadataKey, traceId,
		)
	}
	return ctx
}

// extractTraceId copies the incoming-metadata trace id into the ctx (T2).
func extractTraceId(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(TraceIdMetadataKey); len(vals) > 0 && vals[0] != "" {
			return WithTraceId(ctx, vals[0])
		}
	}
	return ctx
}

// ---------------------------------------------------------------------------
// Client side
// ---------------------------------------------------------------------------

// GrpcUnaryClientInterceptor propagates the trace id and logs the request and
// reply of every unary call.
func GrpcUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = attachTraceId(ctx)
		slog.InfoContext(ctx, "grpc client request",
			slog.String("method", method),
			pbLogAttr("data", req),
		)
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			slog.InfoContext(ctx, "grpc client reply",
				slog.String("method", method),
				slog.String("error", err.Error()),
			)
			return err
		}
		slog.InfoContext(ctx, "grpc client reply",
			slog.String("method", method),
			pbLogAttr("data", reply),
		)
		return nil
	}
}

// GrpcStreamClientInterceptor propagates the trace id at stream creation and
// logs every sent/received message.
func GrpcStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx = attachTraceId(ctx)
		clientStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			slog.InfoContext(ctx, "grpc client stream open",
				slog.String("method", method),
				slog.String("error", err.Error()),
			)
			return nil, err
		}
		slog.InfoContext(ctx, "grpc client stream open",
			slog.String("method", method),
		)
		return &loggingClientStream{
			ClientStream: clientStream,
			ctx:          ctx,
			method:       method,
		}, nil
	}
}

type loggingClientStream struct {
	grpc.ClientStream
	ctx    context.Context
	method string
}

func (s *loggingClientStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		slog.InfoContext(s.ctx, "grpc client send",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc client send",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

func (s *loggingClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if errors.Is(err, io.EOF) {
		return err // normal end of stream: not logged (L4)
	}
	if err != nil {
		slog.InfoContext(s.ctx, "grpc client recv",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc client recv",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

// ---------------------------------------------------------------------------
// Server side
// ---------------------------------------------------------------------------

// GrpcUnaryServerInterceptor extracts the trace id from metadata into the ctx
// and logs the request and reply of every unary call.
func GrpcUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = extractTraceId(ctx)
		slog.InfoContext(ctx, "grpc server request",
			slog.String("method", info.FullMethod),
			pbLogAttr("data", req),
		)
		reply, err := handler(ctx, req)
		if err != nil {
			slog.InfoContext(ctx, "grpc server reply",
				slog.String("method", info.FullMethod),
				slog.String("error", err.Error()),
			)
			return reply, err
		}
		slog.InfoContext(ctx, "grpc server reply",
			slog.String("method", info.FullMethod),
			pbLogAttr("data", reply),
		)
		return reply, nil
	}
}

// GrpcStreamServerInterceptor extracts the trace id into the stream context
// and logs every received/sent message.
func GrpcStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		serverStream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx := extractTraceId(serverStream.Context())
		slog.InfoContext(ctx, "grpc server stream open",
			slog.String("method", info.FullMethod),
		)
		wrapped := &loggingServerStream{
			ServerStream: serverStream,
			ctx:          ctx,
			method:       info.FullMethod,
		}
		err := handler(srv, wrapped)
		if err != nil {
			slog.InfoContext(ctx, "grpc server stream close",
				slog.String("method", info.FullMethod),
				slog.String("error", err.Error()),
			)
			return err
		}
		slog.InfoContext(ctx, "grpc server stream close",
			slog.String("method", info.FullMethod),
		)
		return nil
	}
}

type loggingServerStream struct {
	grpc.ServerStream
	ctx    context.Context
	method string
}

// Context returns the trace-enriched ctx so handler code (and everything it
// calls: OsClient, etcd helpers, outbound RPCs) logs with the trace id (T2).
func (s *loggingServerStream) Context() context.Context {
	return s.ctx
}

func (s *loggingServerStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if errors.Is(err, io.EOF) {
		return err // client finished sending: not logged (L4)
	}
	if err != nil {
		slog.InfoContext(s.ctx, "grpc server recv",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc server recv",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

func (s *loggingServerStream) SendMsg(m any) error {
	err := s.ServerStream.SendMsg(m)
	if err != nil {
		slog.InfoContext(s.ctx, "grpc server send",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc server send",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}
```

## 4. Wiring (every dnv binary MUST use both chain options on every dnv connection/server)

Client connections (dnvctl → gateway; gateway/worker → agents):

```go
conn, err := grpc.NewClient(
	target,
	grpc.WithTransportCredentials(insecure.NewCredentials()),
	grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
	grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
)
```

Servers (gateway's `Gateway` service; agents' `DiskNodeAgent` /
`ControllerNodeAgent`):

```go
grpcServer := grpc.NewServer(
	grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
	grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
)
```

| binary | server interceptors on | client interceptors on |
|---|---|---|
| dnv-gateway | its `Gateway` gRPC server | its connections to dn/cn agents (`GetDnSize`/`GetCnSize`, the `Get*Info` behind its `Inspect*`, the `Get*Bm` bitmap reads) |
| dnv-worker | — | its connections to dn/cn agents (`Syncup*`, `Push*Bitmap`, the `Check*` streams — the worker never calls `Get*Info`) |
| dnv-agent dn / cn | its `DiskNodeAgent` / `ControllerNodeAgent` server | — |
| dnvctl | — | its connection to the gateway (mints a trace id per invocation, T4) |
| dnv-cdc | — | — (talks only to etcd; excluded, see §1) |

Those five dnv binaries (`log.md` §1) are the whole of the rule. The
`integtest/` drivers are not dnv components; §6 records the conventions they
follow instead.

## 5. Example output

One (unary) `PushMigrBitmap` chunk, worker side then agent side:

```json
{"time":"...","level":"INFO","msg":"grpc client request","method":"/DiskNodeAgent/PushMigrBitmap","data":{"cluster_id":16981786240730056190,"dn_id":3,"side_pointer":{"sp_id":17,"leg_id":21,"side_id":22},"revision":9,"migr_id":30,"bm_idx":1,"bitmap":"<131072 bytes>"},"trace_id":"a1b2c3d4e5f60718"}
{"time":"...","level":"INFO","msg":"grpc server request","method":"/DiskNodeAgent/PushMigrBitmap","data":{"cluster_id":16981786240730056190,"dn_id":3,"side_pointer":{"sp_id":17,"leg_id":21,"side_id":22},"revision":9,"migr_id":30,"bm_idx":1,"bitmap":"<131072 bytes>"},"trace_id":"a1b2c3d4e5f60718"}
{"time":"...","level":"INFO","msg":"grpc server reply","method":"/DiskNodeAgent/PushMigrBitmap","data":{"agent_reply":{}},"trace_id":"a1b2c3d4e5f60718"}
{"time":"...","level":"INFO","msg":"grpc client reply","method":"/DiskNodeAgent/PushMigrBitmap","data":{"agent_reply":{}},"trace_id":"a1b2c3d4e5f60718"}
```

and one round on a long-lived `CheckDn` stream:

```json
{"time":"...","level":"INFO","msg":"grpc client send","method":"/DiskNodeAgent/CheckDn","data":{"cluster_id":16981786240730056190,"dn_id":3,"revision":9,"show_info":true},"trace_id":"0a1b2c3d4e5f6071"}
{"time":"...","level":"INFO","msg":"grpc server recv","method":"/DiskNodeAgent/CheckDn","data":{"cluster_id":16981786240730056190,"dn_id":3,"revision":9,"show_info":true},"trace_id":"0a1b2c3d4e5f6071"}
{"time":"...","level":"INFO","msg":"grpc server send","method":"/DiskNodeAgent/CheckDn","data":{"agent_reply":{},"revision":9,"dn_info":{"disk_info":{"res_name":"/dev/disk/by-uuid/4425c6a8-dc27-40a3-9fd5-0cc41f534360","status":"RES_STATUS_OK","epoch":1788051600},"meta_info":{"res_name":"/dev/nvme0n1","status":"RES_STATUS_OK","details":"seq=7 sides=2 clone_metas=0 free_ext=26 free_meta_units=48 provisioning=0","epoch":1788051600},"port_info":{"res_name":"1","status":"RES_STATUS_OK","epoch":1788051600}}},"trace_id":"0a1b2c3d4e5f6071"}
{"time":"...","level":"INFO","msg":"grpc client recv","method":"/DiskNodeAgent/CheckDn","data":{"agent_reply":{},"revision":9,"dn_info":{"disk_info":{"res_name":"/dev/disk/by-uuid/4425c6a8-dc27-40a3-9fd5-0cc41f534360","status":"RES_STATUS_OK","epoch":1788051600},"meta_info":{"res_name":"/dev/nvme0n1","status":"RES_STATUS_OK","details":"seq=7 sides=2 clone_metas=0 free_ext=26 free_meta_units=48 provisioning=0","epoch":1788051600},"port_info":{"res_name":"1","status":"RES_STATUS_OK","epoch":1788051600}}},"trace_id":"0a1b2c3d4e5f6071"}
```

(`agent_reply` renders as `{}` because `code = 0` and an empty `details` are proto3
zero values and therefore unpopulated; a zero-valued scalar such as `bm_idx = 0` is
omitted altogether. That is acceptable.)

## 6. Tests and acceptance checklist

Unit tests (`common/interceptor_test.go`) using
`google.golang.org/grpc/test/bufconn` and the generated stubs from
`schema.proto`:

1. **Unary trace propagation**: register a stub `DiskNodeAgent` whose
   `GetDnInfo` reads `metadata.FromIncomingContext` and also calls
   `TraceIdFromCtx(ctx)`; dial via bufconn with both client interceptors;
   call with `ctx = WithTraceId(context.Background(), "t-123")`; assert the
   server saw metadata `trace_id=["t-123"]` and `TraceIdFromCtx` returned
   `"t-123"` inside the handler.
2. **No minting**: the same call without a ctx trace id produces no
   `trace_id` metadata key.
3. **Stream propagation**: a stub `CheckDn` echo handler asserts
   `TraceIdFromCtx(stream.Context())` inside the handler (verifies the
   `Context()` override).
4. **Log records**: install a capturing handler (JSONHandler over a
   `bytes.Buffer` wrapped in `TraceIdHandler`, as in `log.md` §7) around a
   unary call and a two-round `CheckDn` stream exchange; assert the exact
   `msg` strings of L2 appear in order, each with `method`, `data` and
   `trace_id`; assert no record is emitted for the terminating `io.EOF`.
5. **Bytes redaction**: send a `PushMigrBitmapRequest` with a 4-byte bitmap
   through the (unary) `PushMigrBitmap`; assert the captured
   `grpc client request` / `grpc server request` records contain
   `"bitmap":"<4 bytes>"` and not the payload.
6. **Error path**: a handler returning `status.Error(codes.Aborted, "boom")`
   yields a `grpc server reply` / `grpc client reply` record with the `error`
   attribute and no `data` attribute.

Acceptance: `go test ./common/...` passes; every `grpc.NewClient` /
`grpc.NewServer` call site reachable from the five `cmd/` binaries (except the
etcd client) uses the chain options of §4 — today `gateway/server.go`,
`gateway/common.go`, `worker/conn.go` and `agent/agent.go`; a manual
end-to-end run shows one `trace_id` value flowing dnvctl → gateway → agent
across `grpc client request`, `grpc server request`, `os command` and
`etcd put` records.

The `integtest/` drivers are scoped out of that grep on purpose; the two
conventions they follow are recorded here so the carve-out does not live only
in their code comments:

* `gatewayctl`, `cnagentctl` and `dnagentctl` dial **without** the client
  interceptors. A driver is not a dnv component, and its own request/reply
  records would only duplicate what the gateway or agent it calls already
  logs — the suites read the daemon's log, not the driver's. Each instead
  passes its `--trace-id` as plain outgoing metadata
  (`metadata.AppendToOutgoingContext` with `common.TraceIdMetadataKey`), which
  is all T2 needs on the far side, so the T3 chain the suites assert holds
  unchanged. `integtest/fakeagent` does install both server interceptors of
  §4: it stands in for an agent, and its `agent.log` is the record the suites
  read for what the worker sent — `worker_test.sh` matches `grpc server *`
  records only, never the worker's own client-side ones, which carry the same
  payloads (§5 prints one such pair).
* `gatewayctl`, `workerctl` and `cdcctl` replace `common`'s default logger
  with the same `TraceIdHandler` over a `slog.NewJSONHandler(os.Stderr, nil)`,
  because their **stdout** is reserved for the command's own result — one JSON
  document for some subcommands, one line per key or record for others — which
  the suites parse with `jq` or line by line. None of the three logs on its
  own account: the redirect moves the `etcd *` records `etcdutil` emits under
  `workerctl` and `cdcctl`, and under `gatewayctl`, which calls nothing that
  logs, it is purely defensive. `log.md` R2's stdout rule and R3's
  default-logger rule bind the five dnv binaries its §1 lists, and a driver is
  not one of them. `integtest/fakeagent` logs far more than the three and
  keeps the stdout default instead: it has no result to reserve stdout for, so
  `common`'s `init()` chain stays in place and the suite captures its records
  as `agent.log`.

## 7. Amendments applied to this document

Recorded for traceability; the edits are already applied. Appended rather than
inserted because §2, §3 and §4 are cited by number from `log.md` and
`layout.md`.

* `update_01.md` U4 — the §5 `CheckDn` sample's `meta_info.details` gained the
  trailing ` provisioning=0` field. `DiskMeta.Describe()` now renders
  `seq=%d sides=%d clone_metas=%d free_ext=%d free_meta_units=%d provisioning=%d`,
  the appended count being the sides whose §9.4 zeroing has not finished
  (`dnagent.md` DN18). Sample values only; no interceptor behaviour changed.
