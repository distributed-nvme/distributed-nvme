# layout.md — Repository Layout (dnv)

Status: **normative**. Companion to `architecture.md` (design), `log.md`,
`osclient.md`, `grpc.md` (component specs). This document fixes where every
package and file lives, so that all specs and implementers agree on paths and
import strings. The project is built from scratch against this layout; the
design inputs are `architecture.md`, `schema.proto`, `constants.go` and
`name_fmt.go`.

## 1. Module identity

* Repository: `https://github.com/distributed-nvme/distributed-nvme`
* Repo root folder: `distributed-nvme/` = Go module root.
* `go.mod`: `module github.com/distributed-nvme/distributed-nvme`, `go 1.21`
  (or newer; `log/slog` and `exec.Cmd.Cancel`/`WaitDelay` are required).
* Direct dependencies: `google.golang.org/grpc`,
  `google.golang.org/protobuf`, `go.etcd.io/etcd/client/v3`,
  `golang.org/x/sync`, `github.com/spf13/viper` (flags/config/env per
  `architecture.md` §13; a CLI framework such as `github.com/spf13/cobra` MAY
  be added for the `dnvctl` subcommand tree).

Library packages sit directly under the module root (no `pkg/` or
`internal/` prefix), so the file paths used by the component specs —
`common/log.go`, `common/osclient.go`, `common/interceptor.go` — are literal
repository paths. Binaries live one-per-directory under `cmd/`.

## 2. Directory tree

```
distributed-nvme/                      # repo root = module root
├── go.mod                             # module github.com/distributed-nvme/distributed-nvme
├── go.sum
├── Makefile                           # targets: gen, build, vet, test
├── README.md
├── LICENSE
├── .gitignore                         # ignores bin/
├── bin/                               # build outputs (never committed):
│                                      # dnv-gateway, dnv-worker, dnv-agent, dnv-cdc, dnvctl
├── doc/                               # the five documents driving the implementation
│   ├── architecture.md
│   ├── layout.md                      # this document
│   ├── log.md
│   ├── osclient.md
│   └── grpc.md
├── pb/                                # protobuf: source + generated code
│   ├── schema.proto                   # from the design inputs + go_package (§4); proto package stays unset
│   ├── schema.pb.go                   # generated, committed
│   └── schema_grpc.pb.go              # generated, committed
├── common/                            # the shared leaf package (package common)
│   ├── constants.go                   # from the design inputs + LogStrDataLimit, DefaultOsClientLimit
│   ├── name_fmt.go                    # NameFmt helpers; architecture.md §4 is normative where the input file differs
│   ├── log.go                         # per log.md
│   ├── osclient.go                    # per osclient.md
│   ├── osclient_fake.go               # per osclient.md §6
│   └── interceptor.go                 # per grpc.md
├── etcdutil/                          # central etcd helpers of log.md §5.3
│   └── etcdutil.go                    # Get/Put/Delete/Range/Watch + STM wrappers, logging inside
├── gateway/                           # Gateway service implementation
│   ├── server.go                      # grpc.Server bootstrap + interceptor wiring
│   ├── cluster.go                     # §8.1   (one file per §8 RPC group:)
│   ├── disknode.go                    # §8.2
│   ├── controllernode.go              # §8.3
│   ├── storagepool.go                 # §8.4, §8.5
│   ├── cntlr.go                       # §8.6
│   ├── thindevice.go                  # §8.7
│   ├── subsystem.go                   # §8.8
│   ├── clone.go                       # §8.9
│   ├── transfer.go                    # §8.10
│   ├── migration.go                   # §8.11
│   ├── spareleg.go                    # §8.12
│   ├── bitmap.go                      # §8.13
│   ├── alloc.go                       # §6 bins / candidate scans
│   └── validate.go                    # §7 common validation
├── worker/
│   ├── membership.go                  # §10.1 registry + HRW shard ownership
│   ├── dnrole.go                      # §10.2 (dn)
│   ├── cnrole.go                      # §10.2 (cn)
│   ├── sprole.go                      # §10.3
│   ├── bmpush.go                      # §9.6 worker side (per-node Push* streams)
│   └── health.go                      # err_epoch / capacity-key maintenance, §10.4 reactions
├── agent/
│   ├── agent.go                       # shared bootstrap: grpc server, local store, OsClient wiring
│   ├── dnagent/                       # DiskNodeAgent (§9.2): syncup_dn.go, syncup_side.go,
│   │                                  # push_migr_bm.go, lvm.go, nvmet.go, migr.go
│   └── cnagent/                       # ControllerNodeAgent (§9.3): syncup_cn.go, syncup_cntlr.go,
│                                      # push_clone_bm.go, leg.go, md.go, pool.go, td.go,
│                                      # clone.go, xfer.go, healthcheck.go, bitmaps.go (§11.4 math)
├── cdc/
│   └── cdc.go                         # §12 discovery controller
├── ctl/
│   ├── root.go                        # dnvctl command tree (§13): dn.go, cn.go, sp.go, vol.go, …
│   └── copier.go                      # §11.4 userspace copier
└── cmd/
    ├── dnv-gateway/main.go
    ├── dnv-worker/main.go
    ├── dnv-agent/main.go              # dispatches the dn|cn subcommand
    ├── dnv-cdc/main.go
    └── dnvctl/main.go                 # first statement: common.SetLogLevel(slog.LevelWarn)
```

File lists inside `gateway/`, `worker/`, `agent/*`, `ctl/` are the
recommended split (they mirror the section structure of `architecture.md`);
implementers MAY split differently but MUST keep the package boundaries.
Unit tests are colocated `_test.go` files inside each package.

`log.md` §5.3 allows the central etcd helpers to live either in
`common/etcdutil.go` or in a dedicated kv layer; this layout picks the second
option as its own package `etcdutil` (import
`github.com/distributed-nvme/distributed-nvme/etcdutil`) so that the etcd
client is linked only into the binaries that use it (§3).

## 3. Dependency rules (normative)

"May import" lists internal packages; anything not listed is forbidden.

| package | may import (internal) | external notes |
|---|---|---|
| `pb` | — | generated code only; imports grpc/protobuf runtimes |
| `common` | — | stdlib, `x/sync`, `protobuf`, `grpc`. MUST NOT import `pb`, the etcd client, or viper — it stays the leaf. (`PbToLogValue`, `OsClient`, and the interceptors all operate on the generic `proto.Message`; `_test.go` files in `common` MAY import `pb` for fixtures.) |
| `etcdutil` | `common` | `go.etcd.io/etcd/client/v3` (incl. `concurrency`). Takes `proto.Message` parameters; MUST NOT import `pb`. |
| `gateway` | `common`, `pb`, `etcdutil` | grpc server + client (dials agents) |
| `worker` | `common`, `pb`, `etcdutil` | grpc client only |
| `agent`, `agent/dnagent`, `agent/cnagent` | `common`, `pb` | grpc server only; **no etcd** — agents never talk to etcd (`architecture.md` §1) |
| `cdc` | `common`, `pb`, `etcdutil` | serves NVMe-oF discovery, not gRPC |
| `ctl` | `common`, `pb` | grpc client to the Gateway; **no etcd** |
| `cmd/*` | the matching top-level package + `common` | viper lives here (flag/config/env parsing per §13) |

Consequences worth stating: the agent and `dnvctl` binaries do not link the
etcd client, and there are no import cycles because `common` and `pb` import
nothing internal.

## 4. Protobuf generation

* `pb/schema.proto` is the `schema.proto` from the design inputs plus exactly
  one added line (after `syntax`):

  ```proto
  option go_package = "github.com/distributed-nvme/distributed-nvme/pb";
  ```

* Do **not** add a proto `package` statement: the fully-qualified gRPC method
  names would change from `/Gateway/...`, `/DiskNodeAgent/...`,
  `/ControllerNodeAgent/...` to `/<pkg>.Gateway/...`, which the `grpc.md`
  examples (and any recorded logs) rely on.
* Makefile `gen` target:

  ```make
  gen:
  	protoc \
  		--go_out=. --go_opt=paths=source_relative \
  		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
  		pb/schema.proto
  ```

  (requires `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` on PATH).
* `schema.pb.go` and `schema_grpc.pb.go` are **committed**, so `go build` /
  `go test` / CI never require protoc; `make gen` is rerun only when
  `schema.proto` changes.

## 5. `cmd/` wiring

Each `main.go` is thin: parse flags/config/env with viper (`architecture.md`
§13), construct the dependencies, hand off to the matching library package.
Because every main imports `common` (at least transitively), the `init()` in
`common/log.go` installs the default JSON logger before `main` runs
(`log.md` R3). Specifics:

* `cmd/dnvctl/main.go` — first statement:
  `common.SetLogLevel(slog.LevelWarn)` (`log.md` R6).
* `cmd/dnv-agent/main.go` — reads the `dn`/`cn` subcommand and dispatches to
  `agent/dnagent` or `agent/cnagent`; both share `agent` for the server
  bootstrap, `--local-store` handling and the single
  `common.NewLimitedOsClient(...)` instance.
* Interceptor wiring per binary follows the table in `grpc.md` (gateway:
  server + client; worker: client; agents: server; dnvctl: client; cdc:
  none).
* Makefile `build` compiles all five `cmd/*` into `bin/`.

## 6. Suggested build-out order

Each step compiles and passes its tests before the next begins:

1. `go.mod`, `pb/schema.proto` (+ `go_package`), `make gen` → `pb/` builds.
2. `common/constants.go`, `common/name_fmt.go` (fix the input file's syntax
   errors; where it disagrees with `architecture.md` §4, §4 wins), then
   `common/log.go`, `common/osclient.go` + `osclient_fake.go`,
   `common/interceptor.go` per their specs, with the spec test lists.
3. `etcdutil/` (needs `common`; test against an embedded or dockerized etcd).
4. `gateway/` (§§6–8) + `cmd/dnv-gateway`.
5. `agent/` + `dnagent/` + `cnagent/` (§9, §11) + `cmd/dnv-agent`.
6. `worker/` (§10, §9.6) + `cmd/dnv-worker`.
7. `cdc/` (§12) + `cmd/dnv-cdc`.
8. `ctl/` (§13, §11.4) + `cmd/dnvctl`.

## 7. Acceptance checklist

1. `go build ./...` succeeds from the repo root with the module path above.
2. `go vet ./...` and `go test ./...` pass.
3. `grep go_package pb/schema.proto` finds the §4 option; the generated files
   exist and are committed; the proto has no `package` statement.
4. Imports obey §3 — in particular
   `go list -deps ./cmd/dnv-agent ./cmd/dnvctl | grep etcd` finds nothing.
5. `common/` contains exactly the six files of §2 (plus tests), package name
   `common`.
6. All five binaries build into `bin/` via `make build`, and `bin/` is listed
   in `.gitignore`.
