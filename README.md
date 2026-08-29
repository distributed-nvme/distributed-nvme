# distributed-nvme (dnv)

A distributed NVMe-oF block storage system. `doc/architecture.md` is the
design; `doc/layout.md` fixes the repository layout; `doc/log.md`,
`doc/osclient.md` and `doc/grpc.md` are the normative specs of the shared
components.

Module: `github.com/distributed-nvme/distributed-nvme`.

## Layout

| path | contents |
|---|---|
| `pb/` | `schema.proto` plus the committed generated code |
| `common/` | leaf package: constants, name formats, logging, `OsClient`, gRPC interceptors |
| `etcdutil/` | central etcd helpers (proto (un)marshal + logging) |
| `gateway/`, `worker/`, `agent/`, `cdc/`, `ctl/` | the service implementations |
| `cmd/` | one directory per binary: `dnv-gateway`, `dnv-worker`, `dnv-agent`, `dnv-cdc`, `dnvctl` |

Import rules are in `doc/layout.md` §3. In short: `common` and `pb` import
nothing internal, agents and `dnvctl` never link the etcd client.

## Build

```shell
make build   # compiles every cmd/* that has sources into bin/
make vet
make test
```

`make gen` regenerates `pb/schema.pb.go` and `pb/schema_grpc.pb.go`; it needs
`protoc`, `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`. The generated
files are committed, so an ordinary build or test never requires protoc.

## Implemented so far

* `pb/` — generated from `pb/schema.proto` (protoc 29.3, protoc-gen-go
  v1.36.12, protoc-gen-go-grpc v1.6.2). The proto deliberately has no
  `package` statement, so method names stay `/Gateway/…`,
  `/DiskNodeAgent/…`, `/ControllerNodeAgent/…`.
* `common/log.go` — `log/slog` JSON logging on stdout, trace ids on the
  context, `PbToLogValue` (bytes fields → `"<N bytes>"`), `TruncForLog`.
* `common/osclient.go`, `common/osclient_fake.go` — the single path for OS
  commands and file/proto I/O: concurrency-limited, atomic file replaces, one
  Info record per operation.
* `common/interceptor.go` — the four gRPC interceptors that move the trace id
  between context and metadata and log every request/reply message.
