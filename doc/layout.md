# layout.md — Repository Layout

This document owns the layout of the repository: the module identity, the
directory tree with the role of every Go package directory, how the
integration suites are run, the dependency rules between the packages,
protobuf generation, and the rules every binary's `main` follows, so that
the documents and the imports agree on every package path. It leans on
`dependencies.md` for the module's direct dependencies, on `architecture.md`
for the system the packages implement, and on the component documents for
what each package holds and which of its files holds what.

## Module identity

The repository, https://github.com/distributed-nvme/distributed-nvme, is one
Go module, and the repository root is the module root. `go.mod` declares the
module path `github.com/distributed-nvme/distributed-nvme`, the prefix of
every internal import string. `go.mod` also pins the Go version and is
authoritative for it: the code relies on standard-library features that
version provides (`log.md`, Placement; `osclient.md`, Scope and placement).
The direct dependencies, and why each is allowed, are `dependencies.md`,
Direct dependencies.

Library packages sit directly under the module root, with no "pkg/" or
"internal/" prefix, so the file paths the component documents use —
`common/log.go`, `common/osclient.go`, `common/interceptor.go` — are literal
repository paths. Binaries live one per directory under `cmd/`.

## Directory tree

The table names every Go package directory of the repository by its path,
with its role, and the other entries that matter. A package directory that
arrives, leaves or moves changes the table in the same commit.

Package boundaries are binding; the split of a package into files is not:
implementers MAY split a package's files differently but MUST keep the
package boundaries. Which file holds what is the package's own concern, and a
component document names its package's files where its reader needs them.
Unit tests are colocated `_test.go` files inside each package.

| path | role |
|---|---|
| `go.mod`, `go.sum` | the module (Module identity) and its requirements (`dependencies.md`, Direct dependencies) |
| `Makefile` | the build entry points: protobuf generation and formatting (Protobuf generation), the binaries (`cmd/` wiring), vet and test over the whole module, and the fetch of the etcd release the integration suites pin (`dnv-worker.md` EU7) |
| `doc/` | the design documents the code follows: `architecture.md` for the system as a whole, one document per component and per shared piece, the repository-level documents (this one and `dependencies.md`), the documents of the on-hardware suites, `glossary.md`, the project vocabulary, and `core_glossary.md`, its core words in plain language |
| `bin/`, `integtest/bin/` | build outputs, never committed: the binaries of `cmd/` in `bin/`, and in `integtest/bin/` the built drivers, the etcd download cache the suites and the unit tests share (`dnv-worker.md` EU7), the gojq fallback a suite installs when the machine running it has no jq, and the runner's output, one directory for each time it runs |
| `pb/` | package `pb`: the protobuf schema `pb/schema.proto` and the Go code generated from it, committed (Protobuf generation), beside tests that read the schema's descriptors |
| `common/` | the shared leaf package: the constants, the dm, md, NQN and local-store name formats, the name format of a worker's repair with its reserved prefix (`architecture.md`, Common validation), the strict parsers of the dm-name and NQN formats, the metadata-unit formula the gateway and the dn agent share (`architecture.md`, Metadata units), the dm line parser and the dm-clone status parser the gateway, both agents and the two agent drivers share (`architecture.md`, Live-state reporting), logging (`log.md`), the OS client and its fake (`osclient.md`), and the gRPC interceptors and the message-limit options (`grpc.md`) |
| `etcdutil/` | the one door to etcd: typed reads, writes, scans and watches and the STM runners over one client, with the protobuf (un)marshaling and the etcd log records inside (`dnv-worker.md`, EU1 to EU7) |
| `model/` | the etcd data model as Go, shared by the gateway, the worker and dnv-cdc: key formats and their parsers, the `cluster_id` derivation, the typed SP snapshot loader, capacity-key maintenance, the allocator's candidate scans, the mutations the gateway and the worker share, and the worker's sp-drain and clone-drain steps (`dnv-worker.md`, MD1 to MD9) |
| `gateway/` | dnv-gateway, the stateless server of the `Gateway` gRPC service: serving and the trace-id mint, the shared handler helpers, the handlers in one file per resource group, allocation and request validation (`gateway.md`) |
| `worker/` | dnv-worker: the vote worker, the shard workers, the per-object revision workers with their dn, cn and sp roles, the agent-connection cache, the `ClusterConf` cache, health bookkeeping, the bitmap pushes, the automatic reactions and the sp and clone drains (`dnv-worker.md`) |
| `agent/` | the mechanism both agent roles share: bootstrap, the local store, the revision gate, the stored-conf validators, the lock hierarchy, `ResInfo` tracking, the bitmap-chunk store, the part of teardown by sweep both roles share, the per-pass wait budget, the store of each object's latest observation with the bookkeeping of its background read, the converge marks, and the dm, nvmet and nvme-host wrappers (`dnagent.md`, SH1 to SH30) |
| `agent/dnagent/` | the dn role's policy, the `DiskNodeAgent` service: which disk-metadata records, dm tables and nvmet objects a disk node builds, and when (`dnagent.md`) |
| `agent/cnagent/` | the cn role's policy, the `ControllerNodeAgent` service: which leg connections, md arrays, thin pools, dm and nvmet objects a controller node builds, and when, with the wrappers of the tools only the cn runs (`cnagent.md`) |
| `cdc/` | dnv-cdc: the etcd watcher of the `CdcEntry` keys, the per-host view registry, the discovery-log rendering and the NVMe/TCP discovery controller (`cdc.md`) |
| `ctl/` | dnvctl: the cobra command tree, one file per noun group, and in `ctl/root.go` what the groups share — the viper binding, the dial, the result rendering and the exit codes (`dnvctl.md`) |
| `csi/` | dnv-csi: the Container Storage Interface driver, served over a unix socket to Kubernetes: the identity service, the controller plugin, which is a gRPC client of the gateway, the volume naming and the capacity arithmetic (`csi.md`) |
| `integtest/` | the integration suites, one shell script per suite, the runner `integtest/runner.sh` with its configuration file `integtest/runner.conf`, the script that fetches the etcd release the suites pin, `integtest/fetch_etcd.sh`, and the script that counts, in worker logs copied out of a run, the worker's failed check rounds by cause, the check streams it failed to open or to send a round on, its health changes to unreachable and its late observations, `integtest/round_counts.sh`, which no suite calls, beside the suites' drivers and fakes (below) |
| `integtest/dnagentctl/` | the gRPC driver of a real dn agent, which both agent suites use |
| `integtest/cnagentctl/` | the gRPC driver of the cn agent suite; the end-to-end suite runs it only as a host-id printer |
| `integtest/agentfake/` | the fake dn and cn agent as a library: the agents' gates, the `*Info` derived from each object's last accepted request, the levers and the format of the behavior file that sets them, the stored state in memory or in a state file, and the fleet that serves fake agents inside a Go test's process over in-memory connections, one per address and port (`dnv-worker.md`, Integration test plan) |
| `integtest/fakeagent/` | the program over `integtest/agentfake` that serves one fake dn or cn agent over TCP, its levers read from a behavior file, which the worker and gateway suites run in place of real agents |
| `integtest/workerctl/` | the etcd driver that plays the gateway in the worker suite, a front over the planting library that adds the reads a shell suite needs and runs the `model` operations of an operator's repair as the gateway does; the gateway suite reads etcd back through it and plays the worker through it; the cdc and end-to-end suites run it only as a constants printer |
| `integtest/plant/` | the planting library: the writes that plant a desired state into etcd as Go calls, through `model` and `etcdutil` as a gateway's STM would and with the placement the caller chooses; `workerctl` is a front over it, and the worker's Go tests may call it |
| `integtest/cdcctl/` | the etcd driver that plays the gateway and the worker for the `CdcEntry` keys in the cdc suite |
| `integtest/gatewayctl/` | the gRPC driver of the gateway suite, one subcommand per `Gateway` RPC |
| `integtest/fakegateway/` | every `Gateway` method behind a behavior file, which the dnvctl suite runs in place of a gateway |
| `cmd/dnv-gateway/` | the `main` of `dnv-gateway`, the control-plane API server |
| `cmd/dnv-worker/` | the `main` of `dnv-worker`, the control-plane worker |
| `cmd/dnv-agent/` | the `main` of `dnv-agent`, the node agent in its dn and cn roles |
| `cmd/dnv-cdc/` | the `main` of `dnv-cdc`, the central discovery controller |
| `cmd/dnvctl/` | the `main` of `dnvctl`, the operator CLI |
| `cmd/dnv-csi/` | the `main` of `dnv-csi`, the CSI driver |
| `doclint/` | a test-only package: the lint that holds the documents under `doc/` and `README.md` to the documentation rules |

The integration suites are driven over ssh against remote hosts. The two
agent suites need passwordless sudo, for real dm, md, nvmet and NVMe/TCP over
loop devices; so does the cdc suite, for real nvmet and NVMe/TCP over
dm-zero, the end-to-end suite on every guest but its control-plane one, and
the Kubernetes suite on all three of its guests.
The worker, gateway and dnvctl suites need no root. The drivers and fakes are
test drivers only, never linked into a `cmd/` binary. The end-to-end suite
adds no driver of its own and drives the shipped `dnvctl` for every
control-plane call (`e2e_integtest.md` E2E2). One dnv suite runs at a time
in the lab: the suites share guests, and a run's cleanup cannot tell its
own dnv processes and devices from another run's. What each suite
proves is its document's: `dnagent_integtest.md`, `cnagent_integtest.md`,
`e2e_integtest.md`, and the Integration test plan of `dnv-worker.md`,
`cdc.md`, `gateway.md`, `dnvctl.md` and `csi.md`.

**The runner.** `integtest/runner.sh` runs the suites one after another,
never two at once, in the order its configuration file lists the runs, each
run a suite and its arguments; the end-to-end suite has one run for each
slice count it is run at. Each suite still stops at its first failure. The
runner judges a run by the suite's exit status alone and takes the failing
stage and its trace id from the failure report every suite prints. After a
failed run, a usage error included, it keeps the run's output and runs the
same suite with the same arguments as a cleanup-only run, so that the next
run finds the guests clean; then it goes on with the next run. Before that
cleanup it copies the logs the suite left in its work directory on each of
the run's guests next to the kept output. The runner ends with a summary of
every run, naming the failing stage of each failed run and whether its
cleanup finished, and exits non-zero when any run failed. It names the
commit the tree is on and whether the tree holds changes when it starts,
and its summary names both as read at the start and at the end, since every
suite builds the programs from the working tree; a git call that fails
reads as unknown, never as clean. An interrupted runner starts no further
copy, no cleanup-only run and no later run, and stops once the running suite
or copy has ended.

## Dependency rules

"May import" lists internal packages; anything not listed is forbidden.

| package | may import (internal) | external notes |
|---|---|---|
| `pb` | — | generated code only, its tests aside; imports the gRPC and protobuf runtimes |
| `common` | — | the standard library, `golang.org/x/sync`, protobuf and gRPC. MUST NOT import `pb`, the etcd client or viper: it stays the leaf. `PbToLogValue`, the `OsClient` and the interceptors all operate on the generic `proto.Message`; `_test.go` files in `common` MAY import `pb` for fixtures. |
| `etcdutil` | `common` | the etcd client, its `concurrency` package included, and the two modules that arrive with it (`dependencies.md`, Direct dependencies). Takes `proto.Message` parameters and MUST NOT import `pb`; its `_test.go` files MAY import `pb` for fixtures, as in `common`, because the tests of `dnv-worker.md` EU7 need a concrete `proto.Message`. |
| `model` | `common`, `pb`, `etcdutil` | the etcd data model as Go (`dnv-worker.md`, MD1 to MD9): key formats, `cluster_id`, capacity keys, the allocator, the shared mutations. No gRPC; never dials an agent; MUST NOT import `gateway`, `worker`, `agent`, `cdc` or `ctl`. |
| `gateway` | `common`, `pb`, `etcdutil`, `model` | gRPC server and client: it dials agents |
| `worker` | `common`, `pb`, `etcdutil`, `model` | gRPC client only: it dials agents (`dnv-worker.md`) |
| `agent`, `agent/dnagent`, `agent/cnagent` | `common`, `pb`; the two role packages also `agent`, the shared mechanism they build on | gRPC server only; no etcd: agents never talk to etcd (`architecture.md`, System overview) |
| `cdc` | `common`, `pb`, `etcdutil`, `model` | serves NVMe-oF discovery, not gRPC |
| `ctl` | `common`, `pb` | gRPC client of the gateway; cobra, pflag and viper, because the dnvctl command tree and its viper binding (`dnvctl.md` CT9) live here, not in `cmd/dnvctl` (`dnvctl.md`, Files); no etcd |
| `csi` | `common`, `pb` | gRPC client of the gateway, under the shared client chain, and gRPC server of the CSI services, under an interceptor of its own (`csi.md` CSI8); the CSI spec module (`dependencies.md`, Direct dependencies); no etcd |
| each `cmd/` package | the matching top-level package and `common`. `cmd/dnv-worker`, `cmd/dnv-gateway` and `cmd/dnv-cdc` also `etcdutil`: each main builds the client its `Run` takes. `cmd/dnv-agent` also `agent/dnagent`, `agent/cnagent` and `pb`: its main builds the role servers and their `NvmeTrConf` and registers each through `agent.Serve`. `cmd/dnvctl` imports only `common` and `ctl`, and `cmd/dnv-csi` only `common` and `csi`. | cobra and viper live here, for the flag, config and environment parsing and the subcommand trees (`architecture.md`, Components: invocation reference; `dnagent.md` CM1), except for dnvctl, whose tree is `ctl`'s and whose main imports neither |
| each `integtest/` driver | `common`, `pb`; `integtest/agentfake` for `fakeagent`; `model` and `etcdutil` for `workerctl`, which plays the gateway, and `cdcctl`, which plays the gateway and the worker for the `CdcEntry` keys; `integtest/plant` for `workerctl`, which writes through it | test drivers only, never linked into a `cmd/` binary |
| `integtest/agentfake` | `common`, `pb`, `agent` (`CheckRoundCtx` and `Timestamp`, `dnagent.md` SH24, SH28) | a test library: imported by `fakeagent` and by the `_test.go` files of `worker`; the `_test.go` files of `gateway` and `cdc` MAY import it; no other package does, so it is never linked into a `cmd/` binary; gRPC server and client, over TCP or over in-memory connections |
| `integtest/plant` | `common`, `pb`, `etcdutil`, `model` | the planting library, test code only: `workerctl` and the `_test.go` files of `worker` may import it, no other package does, and it is never linked into a `cmd/` binary |
| `doclint` | — | test-only; reads the documents and the sources as files |

Consequences worth stating: the agent, dnvctl and dnv-csi binaries do not
link the etcd client, and there are no import cycles because `common` and `pb` import
nothing internal. The central etcd helpers are a package of their own,
`etcdutil`, rather than part of `common`, so that the etcd client is linked
only into the binaries that use it. The dm line parser and the dm-clone
status parser live in `common`, not `agent`, because the gateway, which
may not import `agent`, proves a copy finished from the same status line
the agents read, and two readers of one kernel format drift.

## Protobuf generation

`pb/schema.proto` is dnv's one protobuf schema; `architecture.md` and the
component documents specify its messages and its three services, `Gateway`,
`DiskNodeAgent` and `ControllerNodeAgent`. It sets the `go_package` option to
the import path of `pb`, `github.com/distributed-nvme/distributed-nvme/pb`.

Do not add a proto `package` statement: the fully-qualified gRPC method names
would change from "/Gateway/…", "/DiskNodeAgent/…" and
"/ControllerNodeAgent/…" to "/<pkg>.Gateway/…", and the `method` attribute of
the interceptor records (`grpc.md`, L1), the suites that match it and any
recorded logs rely on the unqualified names.

The `gen` target of the `Makefile` regenerates `pb/schema.pb.go` and
`pb/schema_grpc.pb.go` with `protoc` and its two Go plugins, `protoc-gen-go`
and `protoc-gen-go-grpc`, which must be on the PATH. The generated files are
committed, so go build, go test and CI never require protoc; `gen` is rerun
only when `schema.proto` changes, and its output is committed with that
change. The `fmt` target formats the Go sources with gofmt and the schema
with clang-format, in the style `.clang-format` pins; run it before committing
a proto change.

## `cmd/` wiring

Each `main.go` is thin: it parses flags, config file and environment with
viper (`architecture.md`, Components: invocation reference), constructs the
dependencies and hands off to the matching library package. Because every
main imports `common`, at least transitively, the `init` of `common/log.go`
installs the default JSON logger on stderr before `main` runs (`log.md` R3).

Each daemon binds every flag to an environment prefix of its own, so a flag
is also a config-file key and an environment variable; dnvctl binds only its
env-backed global flags that way, in `ctl/` (`dnvctl.md` CT9). Specifics:

* `cmd/dnvctl/main.go` sets the log level to Warn with `SetLogLevel` as its
  first statement (`log.md` R6), then hands off to `ctl.Execute`.
* `cmd/dnv-agent/main.go` is a cobra root command with the `dn` and `cn`
  subcommands (`dnagent.md` CM1), dispatching to `agent/dnagent` or
  `agent/cnagent`. Both roles share `agent` for the server bootstrap and the
  `--local-store` handling, and the process builds a single
  `LimitedOsClient` (`dnagent.md` CM4), which the CN11 leg probers
  deliberately bypass (`osclient.md`, Exported raw helpers and the probe-IO
  carve-out).
* `cmd/dnv-worker/main.go`, `cmd/dnv-gateway/main.go` and
  `cmd/dnv-cdc/main.go` each build a cobra root command without subcommands
  (`dnv-worker.md` CM1, `gateway.md` CM1, `cdc.md` CM1), build the
  `etcdutil` client and hand off to their package's `Run`: `worker.Run` owns
  the vote worker and the `ClusterConf` cache, `gateway.Run` serves the
  `Gateway` service, and `cdc.Run` owns the watcher, the per-host view
  registry and the NVMe/TCP listener.
* Interceptor wiring and the message limit per binary follow `grpc.md`,
  Wiring.
* The `build` target of the `Makefile` compiles every `cmd/` directory that
  holds Go sources into a binary of the same name under `bin/`, which
  `.gitignore` keeps out of the repository.
