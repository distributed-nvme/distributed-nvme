# distributed-nvme (dnv)

A distributed NVMe-oF block storage system. `doc/architecture.md` is the
design; `doc/layout.md` fixes the repository layout; `doc/log.md`,
`doc/osclient.md` and `doc/grpc.md` are the normative specs of the shared
components; `doc/dnv-worker.md` is the normative spec of `dnv-worker`, `model/`
and `etcdutil/`, `doc/cdc.md` of `dnv-cdc` and `doc/gateway.md` of
`dnv-gateway`; each carries its own integration-test plan.

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
make fmt     # gofmt + clang-format on pb/schema.proto (pip install clang-format)
```

`make gen` regenerates `pb/schema.pb.go` and `pb/schema_grpc.pb.go`; it needs
`protoc`, `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`. The generated
files are committed, so an ordinary build or test never requires protoc.

## Implemented so far

* `pb/` — generated from `pb/schema.proto` (protoc v7.36.0 / libprotoc 36.0,
  protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2). The proto deliberately has no
  `package` statement, so method names stay `/Gateway/…`,
  `/DiskNodeAgent/…`, `/ControllerNodeAgent/…`.
* `common/` — complete per its specs: `constants.go` and `name_fmt.go` (the
  architecture §4/§7 constants and the deterministic dm/md/NQN/local-store
  name formats, `DnNsIdentity`, `NvmeHostId`), `log.go` (`log/slog` JSON
  logging on stdout, trace ids on the context, `PbToLogValue`,
  `TruncForLog`), `osclient.go`/`osclient_fake.go` (the single path for OS
  commands and file/proto/block I/O plus the §4.5.1 raw probe helpers), and
  `interceptor.go` (the four gRPC interceptors).
* `etcdutil/` — the one and only door to etcd (`dnv-worker.md` §3, `log.md`
  §5.3): typed `Get`/`Put`/`Delete`/`Range`/`RangeKeys`/`WatchTyped` and the
  two STM runners, with the protobuf (un)marshaling and the `log.md` records
  inside.
* `model/` — the architecture §5 etcd data model as Go (`dnv-worker.md` §4):
  key formats and parsers, `cluster_id`, the §5.6 capacity keys, the §6
  candidate scans and the internal §8/§10.4 mutations that the worker uses now
  and the gateway will reuse.
* `worker/` — `dnv-worker.md` §6-§11: the heartbeat/grace/ticket vote layer and
  its shard ownership, the per-shard revision watchers, the per-object
  `Check*` loops with their `Syncup*` and `Push*Bitmap` calls, the `err_epoch`
  health bookkeeping, the `provisioned`/`created` flips and the four automatic
  reactions.
* `agent/` — the shared dn/cn agent mechanism of `dnagent.md` §2:
  reconcile-then-serve bootstrap, local store, revision gate, lock
  hierarchy, `ResInfo` tracking, dm/nvmet/nvme-host wrappers, bitmap-chunk
  store.
* `agent/dnagent/` — the dn role (`dnagent.md` §4): the [D13] on-disk
  format, [D15] side provisioning (background zeroing), per-CN exports,
  migration source/destination with the [D12] bounded fence, bitmap pushes,
  check streams.
* `agent/cnagent/` — the cn role (`cnagent.md` §4): leg connections and
  md-raid1 groups, thin pools and volumes, raid0/ns-dev/nvmet stacks,
  clones and transfers, the CN11 leg health probers, thin-metadata bitmap
  reads.
* `cmd/dnv-agent` — the cobra/viper `dn`/`cn` binary (`dnagent.md` §3).
* `cmd/dnv-worker` — the cobra/viper root command of `dnv-worker.md` §5:
  `--etcd-endpoints`, `--roles`, the two vote timers and `--etcd-dial-timeout`,
  env prefix `DNV_WORKER_`; it builds the `etcdutil` client and hands off to
  `worker.Run`.
* `cdc/` and `cmd/dnv-cdc` — the discovery controller of `doc/cdc.md`: the
  etcd watch that holds the `CdcEntry` view, the NVMe/TCP PDU codec and the
  discovery-log server that answers hosts and fans out AENs.
* `gateway/` and `cmd/dnv-gateway` — the control-plane API server of
  `doc/gateway.md`: all 59 RPCs of `service Gateway` over `etcdutil`'s STM
  machinery, the §6.5 allocation, the §5.5 revision tokens and the ten agent
  calls behind `Get*Size` / `Inspect*` / `Get*Bitmap`. Stateless and
  active-active: any instance serves any request.
* `integtest/` — the on-hardware suites of `dnagent_integtest.md`,
  `cnagent_integtest.md`, `dnv-worker.md` §14, `cdc.md` §9 and `gateway.md`
  §10 (`dnagent_test.sh`, `cnagent_test.sh`, `worker_test.sh`, `cdc_test.sh`,
  `gateway_test.sh` and their drivers `dnagentctl`, `cnagentctl`, `workerctl`,
  `cdcctl`, `gatewayctl`, plus the `fakeagent` the worker and gateway suites
  drive).

Not yet implemented: `ctl/` and `cmd/dnvctl` — `doc/architecture.md` §13 is
their spec.
