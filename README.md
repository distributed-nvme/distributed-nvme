# distributed-nvme (dnv)

dnv is a distributed NVMe-oF block storage system. It aggregates the raw disks
of disk nodes into storage pools, runs the volume logic (thin provisioning,
striping, redundancy, snapshots, cloning, live migration) on controller nodes,
and exports virtual volumes to hosts over NVMe-oF with native NVMe multipath
and ANA. etcd holds all desired state; the control-plane processes and the
per-node agents converge the data plane to it. The binaries:

- `dnv-gateway` serves the `Gateway` gRPC API, stateless and active-active.
- `dnv-worker` turns the desired state into agent calls, runs the automatic
  reactions and drains deleted pools and clones.
- `dnv-agent` runs as `dn` on a disk node or `cn` on a controller node; it
  converges local state to the desired state it is sent, and never talks to etcd.
- `dnv-cdc` is the central discovery controller: it serves NVMe-oF discovery to hosts.
- `dnvctl` is the operator CLI, one command per `Gateway` RPC.

Module: `github.com/distributed-nvme/distributed-nvme`.

## Start here

Read [`doc/glossary.md`](doc/glossary.md) first: it defines the project
vocabulary the documents use. Then read
[`doc/architecture.md`](doc/architecture.md), the design as a whole: the object
model, the device stacks, naming, the etcd data model, allocation, common
validation, the `Gateway` API contract, the agent and worker contracts, the
procedures that span components and the design decisions. The other documents
each own a part:

- [`doc/gateway.md`](doc/gateway.md) — `dnv-gateway`: how it carries out each RPC.
- [`doc/dnv-worker.md`](doc/dnv-worker.md) — `dnv-worker` and the `etcdutil` and `model` packages.
- [`doc/dnagent.md`](doc/dnagent.md) — `dnv-agent`: what both roles share, the command, the dn role.
- [`doc/cnagent.md`](doc/cnagent.md) — the cn role of `dnv-agent`.
- [`doc/cdc.md`](doc/cdc.md) — `dnv-cdc`: the discovery service and its etcd watcher.
- [`doc/dnvctl.md`](doc/dnvctl.md) — `dnvctl`: invocation, the command tree, output and errors.
- [`doc/log.md`](doc/log.md) — the JSON logging and the trace id every binary shares.
- [`doc/grpc.md`](doc/grpc.md) — the gRPC interceptors every dnv connection and server chains.
- [`doc/osclient.md`](doc/osclient.md) — the `OsClient`, the one path to the operating system.
- [`doc/layout.md`](doc/layout.md) — the directory tree, the dependency rules, protobuf generation.
- [`doc/dependencies.md`](doc/dependencies.md) — the direct module dependencies and why each is allowed.

The integration suites drive real binaries on lab VMs over ssh, one script
under `integtest/` each; what a suite proves, its topology and its cleanup are
its document's:

- `integtest/dnagent_test.sh` — [`doc/dnagent_integtest.md`](doc/dnagent_integtest.md)
- `integtest/cnagent_test.sh` — [`doc/cnagent_integtest.md`](doc/cnagent_integtest.md)
- `integtest/e2e_test.sh` — [`doc/e2e_integtest.md`](doc/e2e_integtest.md)
- `integtest/worker_test.sh` — [`doc/dnv-worker.md`, Integration test plan](doc/dnv-worker.md#integration-test-plan)
- `integtest/gateway_test.sh` — [`doc/gateway.md`, Integration test plan](doc/gateway.md#integration-test-plan)
- `integtest/cdc_test.sh` — [`doc/cdc.md`, Integration test plan](doc/cdc.md#integration-test-plan)
- `integtest/dnvctl_test.sh` — [`doc/dnvctl.md`, Integration test plan](doc/dnvctl.md#integration-test-plan)

## Build

The build entry points are `Makefile` targets, run as "make build" and so on:

- `build` compiles every binary under `cmd/` into `bin/` (`layout.md`, `cmd/` wiring).
- `vet` runs go vet over the whole module.
- `test` runs go test over the whole module. The etcd-backed tests skip unless an
  etcd binary is on the PATH or named by `ETCD_BIN` (`dnv-worker.md` EU7).
- `fmt` formats the Go sources with gofmt and `pb/schema.proto` with
  clang-format, which must be installed.
- `gen` regenerates `pb/schema.pb.go` and `pb/schema_grpc.pb.go`; it needs
  `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc` on the PATH. The generated
  files are committed, so build and test never need protoc (`layout.md`,
  Protobuf generation).

## Docs and code

The documents under `doc/` are the high-level guide the code must follow: they
state the current design, its roles, contracts, invariants, orderings and gates,
and each decision with its reason. The code is the source of truth for every
detail, so a document never restates a constant's value, a signature or a flag
table. Each rule has exactly one owner document; anywhere else a rule is cited
by its id, as in "`gateway.md` GW6", or by its owner's heading text, which the
bold lead-in of a paragraph under that heading may follow, never by a section
number, and never restated. A comment or a message names code by identifier,
as in "ProbeSubsystem (agent/nvmet.go)", never by a line number, and carries
no date and no citation of a note kept outside the repository.
`go test ./doclint/`, which `test` also runs, checks the documents, this
README, and the citations, pointers and dates in the code.
