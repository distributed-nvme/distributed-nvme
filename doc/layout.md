# layout.md — Repository Layout (dnv)

Status: **normative**. Companion to `architecture.md` (design), `log.md`,
`osclient.md`, `grpc.md`, `dnagent.md` (component specs). This document fixes where every
package and file lives, so that all specs and implementers agree on paths and
import strings. The project is built from scratch against this layout; the
design inputs are `architecture.md`, `schema.proto`, `constants.go` and
`name_fmt.go`.

## 1. Module identity

* Repository: `https://github.com/distributed-nvme/distributed-nvme`
* Repo root folder: `distributed-nvme/` = Go module root.
* `go.mod`: `module github.com/distributed-nvme/distributed-nvme`, `go 1.26.5`
  (feature floors: `log/slog` 1.21, `exec.Cmd.Cancel`/`WaitDelay` 1.20, the
  `crypto/rand.Read` never-fails guarantee 1.24 — `go.mod` is authoritative).
* Direct dependencies today (must match `go.mod`): `google.golang.org/grpc`,
  `google.golang.org/protobuf`, `golang.org/x/sync`,
  `github.com/spf13/viper` (flags/config/env per `architecture.md` §13) and
  `github.com/spf13/cobra` (the `dnv-agent` and `dnvctl` subcommand trees,
  `dnagent.md` §3) — the last two entered `go.mod` with `cmd/dnv-agent`.
  `go.etcd.io/etcd/client/v3` (the v3.6 line, incl. `concurrency`) entered
  `go.mod` with `etcdutil/` (§4, `dnv-worker.md` §3), and brought
  `go.etcd.io/etcd/api/v3` (`rpctypes`, for EU3's `ErrCompacted` report) and
  `go.uber.org/zap` (only `zap.NewNop()`, EU1's silenced client logger) with
  it as unavoidable direct imports.

Library packages sit directly under the module root (no `pkg/` or
`internal/` prefix), so the file paths used by the component specs —
`common/log.go`, `common/osclient.go`, `common/interceptor.go` — are literal
repository paths. Binaries live one-per-directory under `cmd/`.

## 2. Directory tree

```
distributed-nvme/                      # repo root = module root
├── go.mod                             # module github.com/distributed-nvme/distributed-nvme
├── go.sum
├── .clang-format                      # proto style for `make fmt` (4-space indent, no column limit)
├── Makefile                           # targets: gen, fmt, build, vet, test
├── README.md
├── LICENSE
├── .gitignore                         # ignores bin/
├── bin/                               # build outputs (never committed):
│                                      # dnv-gateway, dnv-worker, dnv-agent, dnv-cdc, dnvctl
├── doc/                               # the documents driving the implementation
│   ├── architecture.md
│   ├── layout.md                      # this document
│   ├── log.md
│   ├── osclient.md
│   ├── grpc.md
│   ├── dnagent.md                     # dnv-agent: agent/ (shared), agent/dnagent/, cmd/dnv-agent
│   ├── cnagent.md                     # dnv-agent cn: agent/cnagent/ (builds on dnagent.md §2/§3)
│   ├── dnv-worker.md                  # dnv-worker: worker/, model/, etcdutil/, cmd/dnv-worker + its integration suite
│   ├── cdc.md                         # dnv-cdc: cdc/, cmd/dnv-cdc + its integration suite
│   ├── gateway.md                     # Gateway: gateway/, cmd/dnv-gateway + its integration suite
│   ├── dnvctl.md                      # dnvctl: ctl/, cmd/dnvctl + its integration suite
│   ├── dnagent_integtest.md           # the on-hardware dn agent suite
│   ├── cnagent_integtest.md           # the on-hardware cn agent suite
│   ├── ThinDeviceCreated.md           # the ThinDevice.created change record (normative)
│   ├── dependencies.md                # direct-dependency ledger (must match go.mod)
│   └── risks_and_gaps.md              # ranked v1 risks / operational gaps (informational)
├── pb/                                # protobuf: source + generated code
│   ├── schema.proto                   # from the design inputs + go_package (§4); proto package stays unset
│   ├── schema.pb.go                   # generated, committed
│   └── schema_grpc.pb.go              # generated, committed
├── common/                            # the shared leaf package (package common)
│   ├── constants.go                   # from the design inputs + LogStrDataLimit, DefaultOsClientLimit
│   ├── name_fmt.go                    # NameFmt helpers; architecture.md §4 is normative where the input file differs
│   ├── log.go                         # per log.md
│   ├── osclient.go                    # per osclient.md (+ the exported raw block helpers of §4.5.1)
│   ├── osclient_fake.go               # per osclient.md §6
│   └── interceptor.go                 # per grpc.md
├── etcdutil/                          # central etcd helpers of log.md §5.3 (dnv-worker.md §3)
│   └── etcdutil.go                    # typed Get/Put/Delete/Range/RangeKeys/WatchTyped + STM runners, logging inside
├── model/                             # the §5 etcd data model as Go (dnv-worker.md §4)
│   ├── keys.go                        # §5.3 key formats, prefixes, parsers; §5.2 cluster_id
│   ├── stm.go                         # the typed SP snapshot loader
│   ├── capacity.go                    # §5.6 capacity-key maintenance
│   ├── alloc.go                       # §6.3/§6.4 candidate scans
│   └── ops.go                         # the internal §8/§10.4 mutations shared by gateway and worker
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
├── worker/                            # dnv-worker.md §6-§11
│   ├── worker.go                      # Run/Config, deps, seed mint, the worker-lifecycle §12 msg constants
│   ├── vote.go                        # §6 vote worker: registry, heartbeat, grace timers, tickets, shard-worker lifecycle
│   ├── shard.go                       # §7 shard worker: scan+watch of one rev prefix, revision-worker lifecycle
│   ├── revision.go                    # §8.1 per-object loop: Check stream, rounds, syncup trigger
│   ├── conn.go                        # the RW7 refcounted agent-connection cache
│   ├── dnrole.go                      # §8.2 SyncupDn builder + node health
│   ├── cnrole.go                      # §8.3 SyncupCn builder + node health
│   ├── sprole.go                      # §8.4 SP snapshot, fan-out, provisioned/created flips
│   ├── clusterconf.go                 # §8.5 ClusterConf cache
│   ├── health.go                      # §9 err_epoch bookkeeping (through model)
│   ├── bmpush.go                      # §10 Push*Bitmap calls, bm_idx bookkeeping
│   └── reaction.go                    # §11 automatic reactions
├── agent/                             # shared dn/cn mechanism (dnagent.md §2)
│   ├── agent.go                       # bootstrap: reconcile-then-serve, grpc server wiring
│   ├── store.go                       # local store helper (Local*Path files, load-on-start)
│   ├── revision.go                    # §9.1 revision gate + reply codes
│   ├── locks.go                       # node/object lock hierarchy (dnagent.md §2.6)
│   ├── resinfo.go                     # ResInfo/status-epoch tracker (§9.5)
│   ├── bitmap.go                      # §9.6 chunk store + §11.4 math skeleton
│   ├── dm.go                          # dmsetup wrapper + table builders, blkdiscard, lsblk
│   ├── nvmet.go                       # nvmet configfs wrapper, fixed ANA groups [D4]
│   ├── nvmehost.go                    # nvme connect/disconnect/list-subsys wrapper
│   ├── dnagent/                       # DiskNodeAgent policy (§9.2, dnagent.md §4): server.go,
│   │                                  # diskmeta.go ([D13] on-disk format + allocators),
│   │                                  # syncup_dn.go, syncup_side.go, push_migr_bm.go,
│   │                                  # check.go (§9.7), migr.go, probe.go,
│   │                                  # zeroing.go (§9.4 background side zeroing)
│   └── cnagent/                       # ControllerNodeAgent policy (§9.3, cnagent.md §4): server.go,
│                                      # plan.go, syncup_cn.go, syncup_cntlr.go, push_clone_bm.go,
│                                      # check.go (§9.7), leg.go, healthcheck.go (the CN11 probers
│                                      # and their direct-syscall IO, osclient.md §4.5.1), md.go,
│                                      # clonemeta.go (the CN base state plus the clone-metadata
│                                      # slot allocator over the one loop device — no LVM anywhere),
│                                      # pool.go, td.go, clone.go, xfer.go,
│                                      # dmutil.go (the shared dm ensure/probe helpers),
│                                      # thinbm.go (the thin-metadata reader and the §11.4
│                                      # bitmap math), bitmapread.go (the GetThinDeviceBm /
│                                      # GetLegBm RPCs), probe.go
├── cdc/                               # §12 discovery controller (cdc.md §1)
│   ├── cdc.go                         # Run, the dependencies, the process wiring
│   ├── watch.go                       # the WV etcd watcher: scan + watch of {p} cdc
│   ├── view.go                        # the DS view registry: per-host records, GENCTR
│   ├── logpage.go                     # DS3/DS9 record rendering + log-page snapshots
│   ├── server.go                      # NP1 listener, one goroutine per connection
│   ├── conn.go                        # NP4-NP12 admin-queue state machine
│   └── pdu.go                         # NP2/NP3 NVMe/TCP PDU codec
├── ctl/
│   ├── root.go                        # dnvctl root: globals, dial, emit (dnvctl.md §2-§3)
│   └── cluster.go, dn.go, cn.go, sp.go, cntlr.go, td.go, ss.go, ns.go, clone.go, xfer.go, migr.go, spare.go
│                                      # one noun group each (dnvctl.md §5); the §11.4 copier is future work outside dnvctl
├── integtest/                         # on-hardware suites: dnagent_integtest.md, cnagent_integtest.md, dnv-worker.md §14, cdc.md §9, gateway.md §10, dnvctl.md §7
│   ├── dnagent_test.sh, dnagentctl/   # dn agent suite + its gRPC driver
│   ├── cnagent_test.sh, cnagentctl/   # cn agent suite + its gRPC driver
│   ├── worker_test.sh                 # worker suite (one server, real etcd, fake agents)
│   ├── workerctl/main.go              # the etcd driver that plays the gateway (+ the two gateway.md §2.4 worker flips)
│   ├── fakeagent/main.go              # fake dn/cn agents driven by a behavior file
│   ├── cdc_test.sh                    # cdc suite (four servers, real etcd, real nvmet, real hosts)
│   ├── cdcctl/main.go                 # the etcd driver that plays gateway + worker for CdcEntry keys
│   ├── gateway_test.sh                # gateway suite (one server, real etcd, 3 gateways, fake agents)
│   ├── gatewayctl/main.go             # the gRPC driver of the gateway suite (one subcommand per RPC + `race`)
│   ├── dnvctl_test.sh                 # dnvctl suite (one VM, no sudo: the real CLI against a fake gateway)
│   ├── fakegateway/main.go            # all 59 Gateway methods behind a behavior file (dnvctl.md §7.5)
│   └── bin/                           # built drivers + the etcd download cache (gitignored via bin/)
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
| `etcdutil` | `common` | `go.etcd.io/etcd/client/v3` (incl. `concurrency`). Takes `proto.Message` parameters; MUST NOT import `pb` (`_test.go` files in `etcdutil` MAY import `pb` for fixtures, as in `common`: EU7's tests need some concrete `proto.Message`). |
| `model` | `common`, `pb`, `etcdutil` | the §5 data model as Go (`dnv-worker.md` §4): key formats, `cluster_id`, capacity keys, the §6 allocator, the internal §8/§10.4 mutations. No gRPC; never dials an agent; MUST NOT import `gateway`, `worker`, `agent`, `cdc`, `ctl`. |
| `gateway` | `common`, `pb`, `etcdutil`, `model` | grpc server + client (dials agents) |
| `worker` | `common`, `pb`, `etcdutil`, `model` | grpc client only (dials agents); `dnv-worker.md` |
| `agent`, `agent/dnagent`, `agent/cnagent` | `common`, `pb` | grpc server only; **no etcd** — agents never talk to etcd (`architecture.md` §1) |
| `cdc` | `common`, `pb`, `etcdutil`, `model` | serves NVMe-oF discovery, not gRPC |
| `ctl` | `common`, `pb` | grpc client to the Gateway; **no etcd** |
| `cmd/*` | the matching top-level package + `common` (`cmd/dnv-worker` also `etcdutil`: main builds the client `worker.Run` takes) | viper + cobra live here (flag/config/env parsing and subcommand trees per §13, `dnagent.md` §3) |
| `integtest/*` | `common`, `pb`, `agent` (the agent drivers, for `ParseCloneStatus`), `model` + `etcdutil` (`workerctl`, which plays the gateway, and `cdcctl`, which plays gateway + worker for the `CdcEntry` keys) | test drivers only, never linked into a `cmd/` binary |

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
* Makefile `fmt` target: `gofmt -w .` plus `clang-format -i
  pb/schema.proto` (style pinned in `.clang-format`; the binary comes from
  `pip install clang-format`). Run it before committing proto changes.
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
* `cmd/dnv-agent/main.go` — a cobra root command with `dn`/`cn` subcommands
  (`dnagent.md` §3) dispatching to `agent/dnagent` or `agent/cnagent`; both
  share `agent` for the server bootstrap, `--local-store` handling and the
  single `common.NewLimitedOsClient(...)` instance (the CN11 leg probers
  deliberately bypass that instance — `osclient.md` §4.5.1).
* `cmd/dnv-worker/main.go` — a cobra root command without subcommands
  (`dnv-worker.md` §5): `--etcd-endpoints`, `--roles`, `--vote-interval`,
  `--vote-grace-time`, `--etcd-dial-timeout`, `--config`; env prefix
  `DNV_WORKER_`; builds the `etcdutil` client and hands off to `worker.Run`,
  which owns the vote worker and the `ClusterConf` cache.
* `cmd/dnv-cdc/main.go` — a cobra root command without subcommands
  (`cdc.md` §6): `--etcd-endpoints`, `--etcd-dial-timeout`, `--range`,
  `--tr-type`, `--adr-fam`, `--tr-addr`, `--tr-svc-id`, `--config`; env
  prefix `DNV_CDC_`; builds the `etcdutil` client and hands off to
  `cdc.Run`, which owns the watcher, the per-host view registry and the
  NVMe/TCP listener.
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
3. `etcdutil/` and `model/` (`dnv-worker.md` §3-§4; their tests run against a real
   `etcd` binary when one is on `PATH` or named by `ETCD_BIN`, and skip otherwise).
4. `gateway/` (§§6–8) + `cmd/dnv-gateway`.
5. `agent/` + `dnagent/` + `cnagent/` (§9, §11) + `cmd/dnv-agent`.
6. `worker/` (`dnv-worker.md` §6-§11) + `cmd/dnv-worker`, then the `dnv-worker.md`
   §14 suite against the lab server.
7. `cdc/` (§12, `cdc.md`) + `cmd/dnv-cdc`, then the `cdc.md` §9 suite against the
   four lab servers.
8. `ctl/` + `cmd/dnvctl` (`dnvctl.md`), then the `dnvctl.md` §7 suite against
   one lab VM (the §11.4 copier is future work outside dnvctl).

## 7. Acceptance checklist

1. `go build ./...` succeeds from the repo root with the module path above.
2. `go vet ./...` and `go test ./...` pass.
3. `grep go_package pb/schema.proto` finds the §4 option; the generated files
   exist and are committed; the proto has no `package` statement.
4. Imports obey §3 — in particular
   `go list -deps ./cmd/dnv-agent ./cmd/dnvctl | grep etcd` finds nothing, and
   `model` imports none of `gateway`, `worker`, `agent`, `cdc`, `ctl`.
5. `common/` contains exactly the six files of §2 (plus tests), package name
   `common`.
6. All five binaries build into `bin/` via `make build`, and `bin/` is listed
   in `.gitignore`.

## 8. Amendments applied to this document

Recorded for traceability; the edits are already applied. Unlike the
"amendments applied to companion documents" sections of `dnagent.md` §5 and
`cnagent.md` §5, this one records edits made **to this document**. It is
appended rather than inserted because §2, §3, §4 and §7 are cited by number
from `cnagent.md`, `dnagent.md` and this file itself.

* [D14] LVM removal (suite amendment U3-T4) — LVM is gone from the CN as well as the DN, so the
  §2 `agent/cnagent/` list loses `lvm.go` ("the clone VG — the one LVM user
  left") and gains `clonemeta.go`: the CN base-state wrappers plus the
  clone-metadata slot allocator over the single loop device, whose kind-`b`
  wrapper dm-linears are their own allocation registry (`cnagent.md` §4.1,
  `architecture.md` [D14]).
* [D15] side provisioning (suite amendment U4-T2) — the §2 `agent/dnagent/` list gains `zeroing.go`,
  the background side-provisioning goroutine of `architecture.md` §9.4
  ([D15]). No package boundary changed: it is a new file in an existing
  package.
* The probe-IO carve-out (suite amendments U2-T2/U2-T4) — the cn probers'
  block IO left the
  `OsClient`. The probe-IO dependency lives in the already-listed
  `healthcheck.go` (no new file), so the §2 note only names it; `common/`
  gained the exported raw helpers `WriteBlockAt`/`ReadBlockDirectAt` inside the
  existing `osclient.go` (`osclient.md` §4.5.1), and §5's `cmd/` wiring records
  that the probers bypass the process's single `LimitedOsClient`. The `common/`
  file count of §7 item 5 is therefore unchanged.
* `cdc.md` §10 — `dnv-cdc` arrived: the §2 `cdc/` entry became that document's
  §1 file split (`cdc.go`, `watch.go`, `view.go`, `logpage.go`, `server.go`,
  `conn.go`, `pdu.go`), the §2 `doc/` tree gained `cdc.md`, the §2 `integtest/`
  tree gained `cdc_test.sh` and `cdcctl/`, the §3 `integtest/*` row lists
  `cdcctl` beside `workerctl` as a `model` + `etcdutil` importer, §5 gained the
  `cmd/dnv-cdc` bullet (viper + cobra root, no subcommands, env prefix
  `DNV_CDC_`) and §6 step 7 cites `cdc.md`. No package boundary changed: `cdc`
  already imported `common`, `pb`, `etcdutil` and `model` per §3.
* Earlier amendments, recorded in their originating documents: `dnagent.md` §5
  (cobra/viper in §1, the `agent/` split, this document listed in the `doc/`
  tree, `agent/lvm.go` deleted with the dn LVM removal) and `cnagent.md` §5
  (the `doc/` tree entry, the `agent/cnagent/` split).
* `dnv-worker.md` — the §1 dependency note names the v3.6 etcd client line; §2 gains
  `doc/dnv-worker.md`, the new `model/` package, the `worker/` file split of
  `dnv-worker.md` §1 (`vote.go`, `shard.go`, `revision.go`, `dnrole.go`, `cnrole.go`,
  `sprole.go`, `clusterconf.go`, `health.go`, `bmpush.go`, `reaction.go` — replacing
  `membership.go`/`check.go`) and the `integtest/` tree; §3 gains the `model` row (and
  lets `gateway`, `worker`, `cdc` import it) plus the `integtest/*` row; §5 gains the
  `cmd/dnv-worker` bullet; §6 steps 3 and 6 and §7 item 4 follow. Package boundaries
  are otherwise unchanged: `common` and `pb` stay leaves, agents and `dnvctl` still
  never link the etcd client.
* Housekeeping (2026-09-09): the §2 `doc/` tree caught up with the files
  that arrived after the amendments above — `gateway.md` (whose suite the §2
  `integtest/` tree and the §3 `integtest/*` row already reflected),
  the two on-hardware suite specs `dnagent_integtest.md` /
  `cnagent_integtest.md`, `ThinDeviceCreated.md`, `dependencies.md`, and the
  then-current resolved-issue and applied-amendment records; the minor-debt
  ledger the same verification produced entered the tree the
  same day (records and ledger all removed
  2026-09-10, below). No package boundary or path changed.
* Second doc-amendment pass (2026-09-10, after the second full doc-vs-code
  verification): an amendment record deciding five code fixes (removed
  2026-09-10, below) and
  `risks_and_gaps.md` (ranked v1 risks, informational) entered the §2 tree;
  the §2 `worker/`
  tree gained `worker.go` and `conn.go` (matching dnv-worker.md §1's
  amended table — the file list above predates them). The pass also fixed
  stale or imprecise passages across `architecture.md`, `gateway.md`,
  `dnv-worker.md`, `cnagent.md`, `dnagent.md`, `ThinDeviceCreated.md`,
  `cdc.md`, `grpc.md`, `log.md`, `osclient.md`, `dependencies.md` and both
  integtest specs, plus eight stale code comments; each document's own
  amendments section is not extended for these (they correct drift, not
  decisions — the amendment and verification records were the
  provenance). No package boundary or path changed.
* Amendment implementation pass (2026-09-10, the same day the five fixes
  were decided): all five landed with their tests — the disabled-primary
  failover trigger (`worker/reaction.go`, `model/ops.go`), the
  adopted-fence gate backstop (`agent/dnagent/fence.go`),
  failure-domain-aware repair placement (`model/alloc.go`,
  `gateway/alloc.go` +
  `migration.go`/`spareleg.go`/`storagepool.go`, `worker/reaction.go`),
  park-before-remove for removed namespaces
  (`agent/cnagent/syncup_cntlr.go`) and the allocator-ledger invariant
  errors (`gateway/alloc.go`). Implementation forced corrections to two
  companion edits that had landed carrying a superseded tier-2 trigger for
  the placement rule — `architecture.md` §6.5 and `dnv-worker.md` §4 MD5,
  both re-amended the
  same day. No file entered or left the
  tree, and no package boundary or path changed.
* Housekeeping (2026-09-10): the four resolved-issue / applied-amendment
  history records and the fully struck minor-debt ledger left the §2 `doc/`
  tree — every decision and finding they carried is
  stated by the normative documents and pinned by the tests those documents
  name, and the remaining doc and code-comment citations of the records
  were retargeted to those documents. No package boundary or path changed.
