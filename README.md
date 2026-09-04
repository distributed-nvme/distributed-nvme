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
* `integtest/` — the on-hardware suites of `dnagent_integtest.md` and
  `cnagent_integtest.md` (`dnagent_test.sh`, `cnagent_test.sh` and their
  gRPC drivers); both have passed against the two lab VMs.

Not yet implemented: `etcdutil/`, `gateway/`, `worker/`, `cdc/`, `ctl/` and
their binaries — `doc/architecture.md` §§5-8, §10, §12 and §13 are their
spec. `doc/update_02.md` records the post-review fixes, all applied;
`doc/update_03.md` records two findings from implementing them — one applied,
one open.
