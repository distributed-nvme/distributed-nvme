# gateway.md — the dnv-gateway control-plane API server

This document owns the `dnv-gateway` binary: package `gateway`, the
stateless server of the `Gateway` gRPC service, with its serving and
lifecycle (GW1 to GW3), the handler pattern every RPC follows (GW4 to GW12
and GW14), the mechanism of each handler by resource group (GW15 to GW20
among them), the agent calls
(AG1 to AG4), the `cmd/dnv-gateway` command (CM1 to CM3), its log records
(LG1 to LG3) and the intent of the gateway integration suite. It owns how the
gateway carries out an RPC — files, helpers, STM shapes, error mapping and
agent-call mechanics; what each RPC does — its errors, defaults and action —
is `architecture.md`, `service Gateway` — RPC specifications, which the
handlers below cite and do not duplicate. It leans on `architecture.md` for
the etcd data model, allocation and common validation; on `dnv-worker.md`
for `etcdutil` (EU1 to EU7) and `model` (MD1 to MD9), whose shared mutations
it reuses (MD8), and for the sp and clone drains that finish its two
latches; on `grpc.md` and `log.md` for the interceptors, the trace id and
logging; and on `layout.md` for package placement and the dependency rules.

## Scope and placement

`dnv-gateway` serves the `Gateway` gRPC service to users and CLIs
(`architecture.md`, System overview). It reads and writes etcd, and calls
agents only for `GetDnSize` and `GetCnSize`, the `Get*Info` behind its
`Inspect*` RPCs, behind the `force` false proof of `DeleteDiskNode` and
`DeleteControllerNode` that the node is clean, behind the `force` false
hydration checks of `DeleteClone` and `FinishMigration`, behind
`ForceStartGroup`'s check of the group's array, behind `CreateRepair`'s
read of the bad leg's row and behind `GetThinDeviceBitmap`'s check of the
parks, the `GetThinDeviceBm` bitmap read, the `InspectGrp` read behind
`InspectGroup`, the primaries' `GetCntlrInfo` and the migration
destinations' `GetSideInfo` behind the status read, `GetClusterStatus`,
and the `GetCnStore` read behind the restore check,
`CheckControllerNodeRestore` (Agent calls). It is one of the three
co-located control-plane processes — gateway, worker, cdc — that any number
of control-plane servers may run.
This document covers `gateway/` and `cmd/dnv-gateway` only: `dnvctl` is a
separate component with its own document (`dnvctl.md`), and the integration
suite drives the gateway with a dedicated test client, `gatewayctl`, not
with `dnvctl`.

The STM bodies of the public RPCs live in the per-resource files of
`gateway/`. The `model` operations a handler runs as its deciding STM —
`GrowSlice`, `WithdrawGrowAsked`, the repair operations `FinishRepair`, `CancelRepair` and
`SetRepairAbort` and the retired-leg operations `DeleteRetiredLeg` and
`SetRetiredLegKeep`, which take the group of either kind through the parent
their record holds, and
`CreateRepair`, which
writes the `Repair` record, with `ForceStartGroup` and `SetGrpOperatorInCharge`,
which take it through the parent their caller found (`dnv-worker.md` MD6)
— and `model.EndMigrationIn`, the write that ends a migration, which
`FinishMigration` and `CancelMigration` run inside their deciding STM, are
reused, never duplicated (`dnv-worker.md` MD8).

The package's files follow `layout.md`, Directory tree: serving and the
trace-id mint (GW1 to GW3); the shared handler helpers — resolution, the
token check, error mapping, the candidate unit, id minting, paging and the
agent connection (GW5 to GW7, GW9, GW10, GW12, AG2), with the `CdcEntry`
maintenance the cntlr mutators and `UpdateSubsystemHosts` share, which sets
an entry's whole transport list by the listing rule of `architecture.md` [D18]
(`syncCdcEntries`, `eachCdcEntry`, `rebuildCdcEntry`); the handlers, one file
per resource group of Handlers by resource group; the per-operation
candidate compositions of `architecture.md`, Per-operation allocation, with
the DN and CN ledgers; and the request validation (GW4). `cmd/dnv-gateway`
is the command (CM1 to CM3); `integtest/gatewayctl/` and
`integtest/gateway_test.sh` are the suite's driver and script (Integration
test plan).

Dependency rule (`layout.md`, Dependency rules): `gateway` imports only
`common`, `pb`, `etcdutil` and `model` plus the gRPC runtime — it is a gRPC
server and a client, since it dials agents. `cmd/dnv-gateway` imports
`gateway`, `common` and `etcdutil` (it builds the client GW2 takes, CM3);
cobra and viper live in `cmd/` and `ctl/`, never in `gateway/`. `make build`
picks the binary up from `cmd/dnv-gateway/main.go` with no Makefile edit.

Out of scope: `dnvctl`; TLS and authentication (the whole of dnv is
plaintext gRPC); an agent connection cache (AG2); and any gateway-side
watch or convergence logic, which is the worker's job.

## Constants, schema and model

The gateway's surface is `service Gateway` in `pb/schema.proto`. It adds no
etcd key kind: it writes only kinds of `architecture.md`, Key table, and
nothing that describes itself (GW1). The `model` exports it rests on are
stated beside the rules that call them — `SpNextId` (GW12, GW19) and
`TakeSpIds` (GW12), the revision bump helpers and the expected revision of
the shared mutations (GW6), the list-range prefixes (GW10), the
conf resolvers and stored-conf validators (GW11), and, for the status
read, `LoadSp`, the grow and repair-wait rules it shares with the sp
worker and the revision keys' kind prefixes (GW20); `model` itself is
`dnv-worker.md`'s (MD1 to MD9).

### Constants this document owns

Six constants are this document's, and `common/constants.go` is
authoritative for their comments.

`DefaultGatewayAgentTimeout` is the per-call budget, in seconds, of the
gateway's agent RPCs — `GetDnSize` and `GetCnSize`, the `Get*Info` behind
`Inspect*`, behind the `force` false proof of `DeleteDiskNode` and
`DeleteControllerNode` that the node is clean, behind the `force` false
checks of `DeleteClone` and `FinishMigration`, behind `ForceStartGroup`'s
check of the group's array, behind `CreateRepair`'s read of the bad leg's
row and behind `GetThinDeviceBitmap`'s check of the parks, the
`GetThinDeviceBm` bitmap read, the `GetCntlrInfo` and `InspectGrp` behind
`InspectGroup`, and the `GetCnStore` behind `CheckControllerNodeRestore` —
applied as a timeout around each dial and its calls (AG2), so
`InspectGroup`'s two calls share one budget. It equals
`DefaultEtcdOpTimeout`, so a hung agent and a hung etcd bound an RPC alike.
The status read's calls take `StatusAgentTimeout` instead.

`StatusAgentTimeout` is the per-call budget, in seconds, of the status
read's agent calls (GW20). It is shorter than `DefaultGatewayAgentTimeout`
because one read calls many nodes: with a node that does not answer, a
page still answers inside dnvctl's default deadline, since the read calls
an address that failed once in a read no more.

`StatusFanOut` is how many pools one status read loads and calls at once
(GW20): a bound of one request, which keeps the primary reports a read
holds at once to a few, each as wide as a cntlr report at the limits.

`CloneBmChunkBytes` is the fixed capacity of ONE clone bitmap chunk and the
quantum that positions it; the chunk rule it sizes is `architecture.md`,
Bitmap push protocol. Its size keeps a grown chunk value plus the rev-bump
put of one `AppendCloneBitmap` inside etcd's default request cap, so a clone
append asks nothing of `EtcdMaxRequestBytes`, and every `PushCloneBitmap`
message far inside `GrpcMaxMsgSize`. It is a clone positioning quantum
only; a migration's bitmap never crosses the gateway (`dnv-worker.md` BM7).

`EtcdMaxTxnOps` is a deployment requirement, not a client setting: every
etcd serving dnv runs with `--max-txn-ops` at this value or higher, well
above etcd's own default. etcd caps a transaction on the longest of its
compare and operation lists, and the serializable-snapshot STM of `etcdutil`
compares every key it read AND every key it wrote, so that sum is what has
to fit. The transaction this number is SIZED by is `CreateStoragePool` at
its widest shape: `MaxSliceCntPerSp` slices of two md-raid1 groups of
`MaxAllocLegPerGrp` legs, each leg on a distinct DN, and `MaxCntlrCntPerSp`
cntlrs. `init_ext_cnt` moves extent counts, not key counts, so every factor
of that shape is a ceiling constant and the bound is tripwirable: a unit
test asserts the arithmetic from the named constants, and another commits
one widest-shape create against a real etcd started with `--max-txn-ops` at
`EtcdMaxTxnOps`. The headroom is thin in the DN dimension: one more key read
or written per DN costs one compare per DN of the widest shape. Other
bounded transactions exceed etcd's default too without sizing this number:
the sp drain's D2 batch (`dnv-worker.md` SPD13), its repair and retired-leg
batches (`dnv-worker.md` SPD15), the created flip's transaction
(`dnv-worker.md` RW19), the clone flip's, whose one STM reads and writes
every clone its sides belong to (`dnv-worker.md` MD6), and the worker's
write of the flag that a group's array has run, whose one STM reads and
writes every slice and clone its groups belong to (`dnv-worker.md` AR15);
each of the six is held to its count by a test pair like the create's.
`CreateClone`, which also charges its clone meta group's DNs, and
`UpdateStoragePoolCntlidSlotList` read every clone of the SP and stay under
etcd's default; so do
`CreateCntlr`, `DeleteCntlr` and the worker's cntlr replacement
(`dnv-worker.md` MD6, AR7), which read no clone, since a clone charges no
CN. So do the repair operations and the forced start's, since each reads
at most the one parent record of its group, never a walk of the slices or
the clones, at most one repair, at most the group's retired legs, which
`MaxRetiredLegPerGrp` bounds, and at most one DN.
Each of these is held to its count by a tripwire. `DeleteThinDevice` stays below
all seven — the create, the D2 batch, the two SPD15 batches, the two flips
and the flag's write
— whatever the td count: its deciding STM commits having read no td but its
target, because the walk for uncreated snapshots is a read-only plan outside
it, verified by the pool's identity and revision (Thin devices). The clone
drain's batches and its final transaction fit etcd's default and size
nothing here (`dnv-worker.md` CLD11).

`MaxCandidateRoundCnt` bounds the rounds of one candidate unit (GW9): a
unit whose every round so far ended in candidate changed answers `ABORTED`
once it has run that many. A client that set no deadline must not hold a
handler for ever under steady churn, and the bound sits well above the
rounds the integration suite's parallel creates need, where a create loses
a round only to another create's commit.

No other constant is this document's. `MaxAllocLegPerGrp` and
`MaxDelGrpPerTxn` are the sp drain's (`dnv-worker.md`, The sp drain) and
appear here through the transaction budget above, and `MaxAllocLegPerGrp`
also as the md-raid1 leg count of `legCntOf`; `DefaultSliceCntPerSp`, the
count substituted for a zero `slice_cnt`, is carried by `architecture.md`,
Storage pools (Storage pools and GrowSlice only names where the handler
substitutes it); `DefaultClusterName`, `ShardBucketSize`, the
`Max*CntPerCluster` ceilings, the id step's `IdStepStride`,
`ThinIdStepStride` and `MaxThinDevId`, `MaxCloneBmCnt`,
`MaxRetiredLegPerGrp` and the bounds of `architecture.md`, Common validation,
are `architecture.md`'s,
and so are the metadata ceiling `DnCloneMetaUnitCnt` and the unit formula
`CloneMetaUnits` the gateway charges a migration by (`architecture.md`,
Metadata units), and `CloneMetaGrpExtCnt`,
the extents of a clone meta group's side, with the byte budget
`CloneMetaBytes` its bound compares (`architecture.md`, Clones).
`EtcdMaxRequestBytes` is `dnv-worker.md`'s and `GrpcMaxMsgSize` is
`grpc.md`'s.

## Serving and lifecycle

GW1. **Stateless and active-active.** `dnv-gateway` has no leader election,
no registration key, no shard split and no instance-count limit. Any
instance serves any request; multi-instance correctness rests entirely on
`etcdutil.RunSTM`'s serializable-snapshot isolation plus the revision tokens
of `architecture.md`, Revision keys and the sync fan-out. A gateway writes
nothing to etcd that describes itself. `gateway.Server` holds exactly one
piece of state — the `*etcdutil.Client` — beside the mandatory
`pb.UnimplementedGatewayServer` embed. Handlers keep no in-process state,
take no locks and impose no concurrency limit — all coordination is
etcd's. The status read's bounds on its own agent calls (GW20) are the
request's own, held for its length and shared with no other request. Two requests racing inside one instance and across two instances are
the same case by construction.

GW2. **Serving.** `gateway.Run` takes the etcd client and a `gateway.Config`
holding the gRPC network, the gRPC address and the etcd endpoints. `Run`
emits `gateway starting`, with the build version, as its first record and
`gateway stopping` on the
way out, closes the client as the last step of its drain (so `main` does
not, CM3), and serves: it listens on the configured network and address;
builds the gRPC server from `serverOptions`, which carries the message limit
of `grpc.md`, Wiring, and chains the gateway's entry-point trace-id mint,
`ensureTraceIdUnary` and `ensureTraceIdStream`, FIRST and the shared pair,
`common.GrpcUnaryServerInterceptor` and
`common.GrpcStreamServerInterceptor`, behind it; registers the `Gateway`
service; starts a goroutine that turns the end of the ctx into a plain
`GracefulStop`, with no time bound of its own (GW3); emits
`gateway serving`; then serves. The gateway mints a trace id for a request
that arrived without one (it is one of the entry points of `grpc.md` T4):
when the incoming metadata carries no `trace_id`, the mint injects a fresh
`common.NewTraceId` into the incoming metadata, upstream of the shared
interceptors and outside `common/interceptor.go`, and they adopt it as they
adopt a client's and never mint themselves.

GW3. **Shutdown** is `GracefulStop`: in-flight handlers finish (each bounded
by its client deadline and the per-STM budget of `dnv-worker.md` EU5, and a
candidate unit also by GW9's round cap), new requests are refused. There is
nothing else to drain — no registry key to delete, no background loop to
stop.

Startup never fails because etcd is unreachable (`etcdutil.New` dials
lazily); only a configuration error fails `Run` before serving.

## The handler pattern

A handler is one gRPC method of `service Gateway`, implemented on
`gateway.Server`. Every handler has the same shape; the deviations of each
RPC are under Handlers by resource group.

GW4. **Request validation first.** `validate.go` implements the table of
`architecture.md`, Common validation, as pure functions with no I/O: string
sizes and patterns, the NQN rules and bounded numerics. A zero passes as
"give me the default", and for every member that ends up in a stored conf
substituting that default is GW11's job on the write path, never this
one's. `md_check_conf.interval_days` and `start_hold_conf.seconds` are
the two exceptions: the zero of each in a present message is kept, as no
md check and as no start hold (GW11). `validate.go` also refuses a
`bdev_feature_list` that is not empty, a `qos_ratio` that is not all
zero, an extent size that is not a whole number of `DnExtSizeUnit` and a
transport conf whose `tr_type` or `adr_fam` is not one dnv takes, and it
checks the `bdev_conf` geometry rules and the block's bound, which
`CreateCluster` judges once more on its resolved conf, still before any
read (Clusters).
The list `count` is the one bounded
numeric GW11 does not cover, because it reaches no stored message at all:
`pageLimit` refuses a value above `MaxListCnt` (`INVALID_ARGUMENT`, never a
silent cap) and turns a zero into `DefaultListCnt` per request, and
`validatePageArgs` runs it — with the token decode — before the three
cluster-scoped List handlers' `ClusterConf` read; `ListClusters` reaches the
same two checks through `pageNames` before its range (GW10). Violations are
`INVALID_ARGUMENT` before any etcd read.
State-dependent validation (a slot in use, an `ns_idx` taken, the geometry
rules on the `bdev_conf` `CreateStoragePool` merges over the cluster's, the
primary threshold judged against the cluster's `cntlr_interval`
(`architecture.md`, Common validation), …) happens inside the STM.
`CreateStoragePool` also judges the merged `bdev_conf` and the primary
threshold before its STM and before its candidate scans, at the place and
for the reason `architecture.md`, Storage pools, gives.

GW5. **Resolution in-STM.** Resolution is the in-STM reads that turn
`cluster_name`, and `sp_name`, into the cluster id and the `SpConf`; the
contract — the `ClusterConf` read first, the `SpConf` read, the `deleting`
gate every mutating SP RPC but `DeleteStoragePool` applies, and the RPCs
that make the `ClusterConf` read in a planning snapshot — is
`architecture.md`, `service Gateway` — RPC specifications, and STM
discipline. GW11's resolution of conf defaults is a different operation, and
the text says which it means. `resolveCluster` makes the first read of every
handler's STM and planning snapshot, except `CreateCluster`'s,
`ListClusters`' and `ListWorkers`' — the three RPCs that resolve no cluster
— the pool transactions of `AdvanceClusterIds` (GW19), the per-pool
snapshots of `GetClusterStatus` (`model.LoadSp`, GW20) and of
`CheckControllerNodeRestore` (`readRestoredPool`, Controller nodes), which
take the cluster id from the request's first read, and the later
transactions of the RPCs named below: `model.ClusterConfKey` of the
`cluster_name`, defaulted to `common.DefaultClusterName`, yielding the
`ClusterConf` and the cluster id, `model.ClusterId` of the name and the
stored creation epoch, which prefixes every further key. `resolveSp` then
reads `model.SpConfKey` of the cluster id and the `sp_name` and applies the
`deleting` gate when its caller asks for it. The SP-scoped handlers open
with one of three wrappers over that pair: `openSp` — resolution with the
gate, then the `SpRev` read and GW6's token check — opens every planning
snapshot and the deciding STM of every SP-scoped mutator but
`DeleteStoragePool` and those whose deciding STM is a `model` op;
`openSpRead` — resolution without the gate and without the token check —
opens every SP-scoped read-only RPC and `DeleteClone`'s phase 1 (Clones);
`openSpFlags` is `openSp` with the gate selectable, and `DeleteStoragePool`
is its one caller with the gate off (Storage pools and GrowSlice).
`GrowSlice`, `WithdrawGrow` and the eight repair calls that run a `model` op —
`CreateRepair`, `FinishRepair`, `CancelRepair`, `TryToAbortRepair`,
`DeleteRetiredLeg`, `ForceStartGroup`, `UpdateGroupOperatorInCharge` and
`UpdateRetiredLegKeep` — resolve in their planning `Snapshot` only (Storage
pools and GrowSlice, Repairs and retired legs) — `CreateRepair` in a fresh
one each round of its candidate unit (GW9): their later transactions — the
deciding STM, which is their `model` op (Scope and placement), and
`GrowSlice`'s CN-budget pre-check — take the cluster id, and the
`ClusterConf` where they use it, from that snapshot and never read
`ClusterConf` themselves. The **paged** `List*` RPCs use plain reads, not an
STM (`architecture.md`, page_token; GW10). So does `GetClusterCapacity`,
whose plain `SpGlobal` read and two ranges follow the same plain
`ClusterConf` read the cluster-scoped lists make (Clusters), so does
`GetClusterStatus` for that read, its page of pools and its cluster rows
(GW20), and so does `ListWorkers`, whose one range needs no `ClusterConf`
read (Worker registry). `ListThinDevices`, `ListSubsystems`, `ListRepairs`,
`ListRetiredLegs` and the single-object `Get*` RPCs are one-STM consistency
reads (Thin devices, Subsystems and namespaces, Clones, Repairs and retired
legs).

GW6. **Token check, presence-based.** The token is the request's revision
token: its `DnRev`, `CnRev` or `SpRev` message. The contract — presence,
not value, selects the mode; a present zero is a real token that never
matches; the echoed handle inside the message is ignored; a token-less
mutator accepts the lost update — is `architecture.md`, Revision keys and
the sync fan-out; `checkSpToken`, `checkDnToken` and `checkCnToken` carry it
out. Immediately after resolution and before any other state check, a
mutator reads its rev key (`SpRevKey` of the SP's shard, the cluster id and
the sp id, and likewise `DnRevKey` and `CnRevKey`). It then asserts that
the stored revision equals the token's **only when the request carries the
token message**; a mismatch is `ABORTED` with the message "stale revision".
A request that carries no token message skips the comparison and proceeds
with no optimistic-concurrency gate, everything else unchanged: the rev key
is still read, the other preconditions still apply, and a successful
mutation bumps as it does with a token. AG4 says what the skipped check
costs a two-phase RPC.

The rev key is read either way: it is an invariant key of
`architecture.md`, Key table, whose absence is `ABORTED`
(`architecture.md`, UNEXPECTED_ERROR → `ABORTED`), the bump helpers rely on
it having been read, and keeping it in the read set leaves a skipped check
no weaker than a checked one against a concurrent delete. Checking the token
first means a client that sent a stale one always sees `ABORTED`, never a
misleading precondition error computed against state it has not read; a
client that sent none has waived that ordering and meets its other
preconditions directly. A token-carrying mutator that reads before its
deciding STM makes the check in the first of those reads — its planning
snapshot, or phase 1 of a two-phase RPC — right after resolution, so a token
already stale there is `ABORTED` before any later check, a candidate scan or
an agent call included, can answer; the deciding STM checks it again, which
catches a token that goes stale after that read unless a check in between
has answered first. That first read resolves through `openSp`, GW5's
`deleting` gate included, so a deleting SP is `FAILED_PRECONDITION` there
with any token or none — except in `DeleteClone`, whose phase 1 resolves
through `openSpRead`, which has no `deleting` gate, before its
`checkSpToken`: only its deciding STM applies the gate, so a stale token's
`ABORTED`, phase 1's other answers — `NOT_FOUND` for an unknown clone among
them — and the hydration check's agent call all come ahead of it (Clones).

The `model` mutations the gateway runs (Scope and placement) — `GrowSlice`,
`WithdrawGrowAsked`, `CreateRepair`, `FinishRepair`, `CancelRepair`, `SetRepairAbort`,
`DeleteRetiredLeg`, `ForceStartGroup`, `SetGrpOperatorInCharge` and
`SetRetiredLegKeep` — take an expected revision and re-check it first inside their own STM, zero
skipping the check (`dnv-worker.md` MD6); a mismatch is an
`ErrPrecondition` with `ReasonStaleRevision`. The gateway passes the
token's revision, which is zero exactly when the request carried no token
message — and zero means "skip" there just as absence does here, so the two
layers agree by construction; what the worker's own calls pass is
`dnv-worker.md` MD6's. A token that is merely *present* with a zero
revision never reaches `model`: this check refuses it first.

Every mutation that changes agent-visible desired state bumps the matching
revision exactly once in the same STM, through `model.BumpSpRev`,
`model.BumpDnRev` or `model.BumpCnRev`: each reads the rev key — a missing
one is an error — and puts it back one revision higher with its handle,
`addr_port` or `sp_name`, preserved, a rewrite and never a delete and
recreate. The `Update*Disabled` RPCs and the capacity and `err_epoch`
maintenance never bump (`architecture.md`, Revision keys and the sync
fan-out), and neither does a write that changes only a DN's
`used_meta_units`: the DN ledger writes such a node and rewrites its
capacity value, and bumps no revision for it (`architecture.md`, Metadata
units). Nor does `UpdateRetiredLegKeep`, whose flag no agent reads
(`architecture.md`, Revision keys and the sync fan-out, Retired legs), so a
token survives its writes; `UpdateGroupOperatorInCharge` bumps, since the
primary reads its flag (`architecture.md`, Repairs).
`AdvanceClusterIds`, the id step, bumps nothing either: no agent reads a
counter (`architecture.md`, Clusters). A flag update that asks for the
state the record already holds —
`UpdateDiskNodeDisabled`, `UpdateControllerNodeDisabled`,
`UpdateCntlrEnabled`, `TryToAbortRepair` on a CREATED repair whose flag
is already set, `ForceStartGroup`'s cancel of a group that carries no
mark, `UpdateGroupOperatorInCharge` with the value the group's flag holds
and `UpdateRetiredLegKeep` with the value the leg's flag holds — writes
nothing, bumps nothing and replies OK, a token the request
carried still being checked first: a no-op that bumped would invalidate
every client's token for nothing. An `AppendCloneBitmap` that names the
position where the same bytes already stand writes nothing, bumps nothing
and replies OK in the same way (`architecture.md`, Clones).

GW7. **Error mapping.** This is the gateway's single error table. Handlers
return a gRPC status error straight from inside their STM closure:
`etcdutil.RunSTM` hands an error the closure returns back unchanged and
uncommitted (`dnv-worker.md` EU4).

* `INVALID_ARGUMENT`: a violation of `architecture.md`, Common validation, a
  malformed `page_token`, a bad enum or oneof.
* `NOT_FOUND`: the cluster, the SP, or a named or id-addressed object is
  absent — a repair name or a retired leg id the pool does not list among
  them, whether the gateway's own read sees it or a repair op's
  `ErrPrecondition` with `model.ReasonRepairNotFound` or
  `model.ReasonRetiredLegNotFound` reaches the handler (Repairs and retired
  legs).
* `ALREADY_EXISTS`: a create finds the name key (or, for `CreateCluster`, a
  global) present — for `CreateRepair`, the name in `repair_name_list`,
  whether its planning read sees it or `model.CreateRepair`'s
  `ErrPrecondition` with `model.ReasonRepairExists` reaches the handler.
* `FAILED_PRECONDITION`: a documented public precondition fails, a
  `model.ErrPrecondition` with any reason but those this table answers with
  another code included — the
  meta ladder cap among them, `GrowSlice`'s `MaxGrpCntPerSlice` group
  ceiling (`architecture.md`, GrowSlice), a counter the id step cannot move
  without wrapping (GW19), a `next_id` with fewer ids left than the call
  takes (`architecture.md`, Globals: id allocation + shard buckets; GW12),
  a pool that has spent every thin id (`architecture.md`, Thin
  devices), a clone destination whose dm-clone metadata one side of a clone
  meta group cannot hold (`CreateClone`, the message naming the largest
  destination one side holds), a clone whose dm-clone would report an
  optimal IO size over `MaxNvmeTcpCmdBytes` (`CreateClone`,
  `cloneIoSizeCheck`), a side of a clone meta group named to
  `CreateMigration`, a side of a repair's bad leg named to
  `CreateMigration`, a repair call on the group of a clone being deleted,
  a repair call the repair's stage does not allow (`architecture.md`,
  Clones, Migrations, Repairs), and a node create with no `location` whose
  agent reports a host name that is no valid `location` (`architecture.md`,
  Disk nodes).
* `RESOURCE_EXHAUSTED`: a global's `shard_bucket` sum at `Max*CntPerCluster`;
  the per-SP and per-group count ceilings — `cntlr_id_list` at
  `MaxCntlrCntPerSp`, `td_name_list` at `MaxTdCntPerSp`, `nqn_list` at
  `MaxSsCntPerSp`, a subsystem's `ns_list` at `MaxNsCntPerSs`,
  `clone_name_list` at `MaxCloneCntPerSp`, `xfer_name_list` at
  `MaxXferCntPerSp`, `migr_name_list` at `MaxMigrCntPerSp`
  (`architecture.md`, `service Gateway` — RPC specifications); a group's
  retired legs at `MaxRetiredLegPerGrp` for a fresh new leg of an operator's
  `CreateRepair`, whether its
  planning read sees them or `model.ReasonRetiredListFull`, which a finish
  committed after that read reaches (Repairs and retired legs); too few
  candidates
  (`architecture.md`, Per-operation allocation); `AppendCloneBitmap`'s
  stored chunk plus the new page above `CloneBmChunkBytes` — one chunk's
  ceiling reached by previous appends (its `INVALID_ARGUMENT` refusals
  are an invalid request: an empty `bitmap`, both
  index bounds — `src_slice_idx` at or above `src_slice_cnt` and `bm_idx` at
  or above `MaxCloneBmCnt`, checked in-STM — the stateless page cap, a
  page longer than `CloneBmChunkBytes` judged on the request alone because a
  page longer than a whole chunk fits nowhere whatever is stored, and the
  four of a `src_start_block`: sent beside a `bm_idx` or an `offset`, not
  a multiple of 8, past the last chunk, or with a page that crosses the
  end of its chunk; all under Clones); a cntlr's CN below a grow's extent count (`GrowSlice`'s
  pre-check); metadata room, which is capacity like extents
  (`architecture.md`, Metadata units): a scan that found no DN with room
  (`CreateMigration`), the message saying "metadata room"; the ledgers' own
  `charge` shortfalls and the DN ledger's `chargeUnits` shortfall
  (`dnLedger.charge`, `cnLedger.charge` and `dnLedger.chargeUnits`,
  `gateway/alloc.go`) — defensive where a pick precedes them, since
  `verifyPick` answers the same shortfall as candidate-changed first.
* `ABORTED` with "stale revision": a token mismatch, and a
  `model.ErrPrecondition` with `ReasonStaleRevision`.
* `ABORTED`: everything of `architecture.md`, UNEXPECTED_ERROR → `ABORTED` —
  STM-client, conflict-budget, etcd and proto errors; a stored conf that is
  not concrete (GW11; the message is `model`'s, beginning
  "invalid stored conf: "); an agent gRPC failure where the RPC says so; a
  candidate unit that did not settle — its ctx ended, or its rounds reached
  GW9's cap.

`model.ErrNotFound` maps to `NOT_FOUND`, though none of the shared
mutations (GW6) returns it: when the SP its handler resolved is gone by
the time the mutation's own STM runs, `checkSpRev` or the op's own read of
the `SpConf` refuses with an `ErrPrecondition`, so the client sees
`FAILED_PRECONDITION`. An `ErrPrecondition` with reason "candidate changed"
maps to nothing (GW9).

The dividing line: `RESOURCE_EXHAUSTED` is capacity or quota that could be
freed or extended (extents, candidates, count ceilings — though not
`GrowSlice`'s `MaxGrpCntPerSlice` group ceiling, `architecture.md`,
GrowSlice); `FAILED_PRECONDITION` is the object's own state forbidding the
operation.
A `GrowSlice` capacity shortfall sits on both sides, on either half of the
allocation. A CN-budget shortfall is `RESOURCE_EXHAUSTED` when `GrowSlice`'s
pre-check sees it, but `FAILED_PRECONDITION` when it only appears in the
deciding STM, where `model.chargeSpCns` can refuse solely as an
`ErrPrecondition` (Storage pools and GrowSlice). A leg DN short of free
extents is likewise `RESOURCE_EXHAUSTED` when the scan of
`architecture.md`, Per-operation allocation, skips it and the pick comes up
short (the "too few candidates" case above), but on `GrowSlice`
`FAILED_PRECONDITION` (`model.checkDnPick`, reason "dn free_ext_cnt too
low") when only the STM sees it — that check precedes the capacity-key
test, so it is not swallowed by `ReasonCandidateChanged`. The same ordering
means a pick whose DN was **deleted or disabled** between scan and STM
surfaces on `GrowSlice` as `FAILED_PRECONDITION` ("dn not found" or "dn not
allocatable") rather than a GW9 re-scan. `model.CreateRepair` sets
`checkDnPick`'s `vanishedIsChanged` for a fresh new leg, which turns those
three refusals into `ReasonCandidateChanged`, so a stale pick there
re-scans as it does on the gateway-ledger paths, which treat every vanished
pick, and one that has lost the metadata room the round needs, as
candidate-changed.

GW8. **One STM per RPC** (`architecture.md`, STM discipline). Everything
computable beforehand (name formatting, group plans, candidate lists, the
stamped `creation_epoch`) is prepared outside; all reads and writes commit
in one `RunSTM`. The closure MUST be a pure function of what it reads
through the STM (it is re-run on conflict); minted values that escape (ids,
generated uuids) are written to closure-captured variables that the closure
itself (re)assigns. The deciding STM is the one that commits the mutation;
for a two-phase RPC (AG4) it is the second one. `AdvanceClusterIds` is the
one RPC that commits in several: the globals' STM, then one per pool (GW19).
`CheckControllerNodeRestore` commits nothing and reads in several: one
snapshot for the node, one per pool and a plain range of a pool's thin
devices (Controller nodes).

GW9. **Candidate unit retry.** Allocating RPCs (`CreateStoragePool`,
`GrowSlice`, `CreateCntlr`, `CreateMigration`, `CreateRepair`,
`CreateClone`) loop over a candidate unit: scan candidates outside
(`model.FindDnCandidatesAntiAffine` or `model.FindCnCandidatesAntiAffine`,
then `model.PickRandom`), then run the STM, which re-reads each pick's exact
capacity key (the bin index, free extents and endpoint its `Cand` carries)
and fails `ErrPrecondition` "candidate changed" when one is gone; when the
pick's DN no longer has the free metadata units the round needs, which
`verifyPick` reads from the DN's record because a write that changes only
units leaves the key where it is (`architecture.md`, Metadata units); or
when the RPC's own plan re-check fails — `CreateStoragePool`'s cluster id
and leg count and `CreateClone`'s cluster id, `sp_id`, leg count and pool revision
(`architecture.md`, Storage pools; `architecture.md`, Per-operation
allocation; `architecture.md`, Clones), and the gain checks of `CreateCntlr`,
`CreateMigration` and `CreateRepair` (`architecture.md`, Per-operation
allocation). `CreateRepair`'s op fails it too for a pick deleted, disabled
or otherwise taken out of allocation, or short of extents, since the scan
(GW7's dividing line). On that error — and only that error — the
handler plans, scans and commits again, for at most `MaxCandidateRoundCnt`
rounds in all: a unit whose ctx has ended, which is checked first, or whose
rounds have reached the cap answers `ABORTED` "candidate allocation did not
settle", naming the ctx's error or the cap. The cap is what bounds a
request whose client set no deadline, which steady churn could otherwise
hold in its handler for ever (Constants this document owns). The reason is
never surfaced to a client. Each allocating RPC answers its count ceiling in
every round ahead of its scan, from a read made before the scan:
`MaxSpCntPerCluster`
(`CreateStoragePool`, at the place `architecture.md`, Storage pools, gives
it), `MaxCntlrCntPerSp` (`CreateCntlr`), `MaxMigrCntPerSp`
(`CreateMigration`) and `MaxCloneCntPerSp` (`CreateClone`) from the planning
read, right behind GW6's token check, and a group's retired legs at
`MaxRetiredLegPerGrp` for an operator's `CreateRepair` with a fresh new leg
and `GrowSlice`'s group ceiling from the planning snapshot. A caller at a
ceiling then hears the ceiling rather than a scan's shortfall, and no scan
is run for a create that cannot commit. `CreateMigration` answers its name,
its pool's block, its source side and its slot from the same planning read,
in that order behind the ceiling, so a re-send of a created migration hears
`ALREADY_EXISTS` rather than a scan's shortfall, unless that migration
filled the pool to `MaxMigrCntPerSp`, when the ceiling answers first
(Migrations). `GrowSlice` is the one allocating RPC whose op keeps a stale
pick's own refusal: a record that is gone, a DN
no longer allocatable or one short of extents is `FAILED_PRECONDITION`
(GW7) with no further round (`dnv-worker.md` MD5), where every other
allocating RPC answers "candidate changed", and only a key gone under a
record that still fits is "candidate changed" there.
The deciding STM checks the ceiling again, because only it sees a write
that commits between the two reads. `DeleteThinDevice` runs the same loop,
cap included, without allocating: its scan is the plan's walk for uncreated
snapshots, and its STM fails candidate-changed when the SP it resolves is
not the one the plan walked or its `SpRev.revision` is no longer the one the
plan read — though a request carrying the token that finds the revision
moved fails GW6 first, `ABORTED` (Thin devices). `UpdateNamespaceDev` runs
it too: its scan is the plan's walk for the td its namespace points at,
and its STM fails candidate-changed when the SP it resolves is not the one
the plan walked or the namespace no longer points at that td (Subsystems
and namespaces).

GW10. **Pagination.** The token, the page and the plain reads of the paged
lists are `architecture.md`, page_token; `pageNames` carries them out. It
resolves `count` through `pageLimit` (GW4), decodes the token, ranges the
prefix outside any STM, cuts the page after the decoded key and returns the
next token, empty when the page is not full. `validatePageArgs` runs the
same two checks ahead of the three cluster-scoped lists' plain
`ClusterConf` read, so a bad `count` or token is answered before a missing
cluster is. The prefixes are `model`'s `ClusterConfPrefix`, `DnConfPrefix`,
`CnConfPrefix` and `SpConfPrefix`, the last three of the cluster id; the
returned names are the key suffixes after the prefix. Paging is implemented
in `gateway/`: `model` carries no helper for it beyond those prefix
builders.

GW11. **Defaults resolved at WRITE time.** The rule, its two reasons, its
order after the request validation and the two stored messages it exempts
are `architecture.md`, Common validation, and its order after
`CreateStoragePool`'s merge is `architecture.md`, Storage pools; the rungs
are a member from the request, else from the owning `ClusterConf`, else from
a constant of `common/constants.go` or, for the stripe, from the block, all
applied by the RPC that writes the conf; a pool's stripe from its cluster is
then narrowed where its product with the slice count is over the bound
(`architecture.md`, Common validation, the stripe's product). This rule is
where each handler carries that out. `CreateCluster` stores
`model.ResolveClusterConf` of its request, which settles `bdev_conf`,
`dn_bin_conf`, `alloc_conf`, `health_check_conf`, `md_check_conf` and
`start_hold_conf` (Clusters); `CreateStoragePool` stores
`resolveSpBdevConf`'s conf, `model.ResolveSpBdevConf` of the merge
`mergeSpBdevConf` makes over the cluster's conf (`architecture.md`, Storage
pools; Storage pools and GrowSlice). A handler that then COMPUTES with a
stored member validates it first and REFUSES rather than guessing around a
zero: `model.ValidateClusterConf` in `CreateDiskNode` and
`CreateControllerNode` (before dividing a reported size by `extent_size`),
in `DeleteDiskNode` and `UpdateDiskNodeDisabled` (before their first write,
the last of their checks of the request and the store, because the one
capacity key each moves is named by shifting the STORED ladder), in
`CreateStoragePool` (on its plain pre-read, before the merge that gives its
scans their leg count and that the geometry rules of `architecture.md`,
Common validation, judge, and again in its STM before any group geometry)
and the `GrowSlice` handler (before any group geometry), in the
`WithdrawGrow` handler (before its op, which gives extents back by the
stored ladder), in `newDnLedger`
(the DN ledger `CreateStoragePool`, `CreateClone` and `CreateMigration`
build before staging their first write, for the same capacity-key reason),
at the top of `model.EndMigrationIn` for `FinishMigration` and
`CancelMigration`, in the planning read of `FinishRepair`, `CancelRepair`
and `DeleteRetiredLeg` (`releaseConfGate`), whose `model` ops give a side
back through the drains' ledger by the same capacity keys (`FinishRepair`'s
op gives back the side of the oldest retired leg that is not kept when a
worker's repair finishes at the retired-leg bound, and the gate runs whether
or not a side goes back), and in the scans `pickDns` and `pickCn` (where a
zero batch size would silently make the scan width zero and turn every
allocation into `RESOURCE_EXHAUSTED`), in `GetClusterCapacity` right after
its `ClusterConf` read (its space answer counts disk nodes by the stored
ladder and turns extents into bytes by the stored extent size), in
`GetClusterStatus` right after its `ClusterConf` read (GW20), in
`CreateMigration`'s plan and STM before the destination's metadata units are
sized by the cluster's `extent_size` (`migrMetaUnitsOf`), and in
`CreateClone`'s plan and STM before its clone meta group is laid out from
that `extent_size` (`planCloneGrp`); `model.ValidateBdevConf` in
`GrowSlice`, in `WithdrawGrow`, in `CreateThinDevice` (before sizing against the stripe), in
`InspectStoragePool` (before it sizes the data space by the block size), in
`CreateClone`'s plan and STM before its clone meta group is laid out and its
metadata bounded by the SP's block size (`planCloneGrp`), and in
`CreateMigration` before a migration's metadata units are sized by that
block size (`migrMetaUnitsOf`: a zero there would size every migration at
one unit, `architecture.md`, Metadata units). Such a zero is a lost
invariant, not a bad request, so every one of those gates is GW7's `ABORTED`
and never `INVALID_ARGUMENT`. `model.GrowSlice` re-runs both validators
inside its own STM and can report that refusal only as an `ErrPrecondition`,
which `mapModelErr` renders `FAILED_PRECONDITION`; the handler's pre-check
above is what makes that a race rather than the ordinary path, the same
asymmetry GW7 records for `chargeSpCns`. `model.FinishRepair`,
`model.CancelRepair` and `model.DeleteRetiredLeg` re-run the `ClusterConf`
validator on the conf they are handed, the planning read's copy that
`releaseConfGate` has already passed, so through the gateway their refusal
never fires; each op gates the conf it is handed as the drains do, whoever
calls it.

The resolvers and validators are `model`'s. `model.ResolveBdevConf` is the
write-time resolver of a `bdev_conf`: `model.ResolveClusterConf` applies it
to a `ClusterConf`'s own, and `model.ResolveSpBdevConf`, which
`CreateStoragePool` stores through, applies it to the merge and then
narrows an inherited stripe with `model.FitStripe`. The stored-conf validators `model.ValidateBdevConf` and
`model.ValidateClusterConf` begin every message with "invalid stored conf: "
and name the proto field. `ValidateBdevConf` checks the defaultable members
for PRESENCE only — the bitmap chunk count solely when the oneof did choose
md-raid1, a `redund_none` pool having no bitmap to size; the ranges of
`architecture.md`, Common validation, belong to the request, and a
`low_water_mark_pct` above 100 is the legal "auto-grow off" setting, never an
error. `ValidateClusterConf` adds `dn_bin_conf.extent_size` non-zero —
non-zero only, because a DN header may legitimately be formatted at an
unusual size — the shift ladder of `architecture.md`, DN bins, the two batch
sizes, the four intervals, `md_check_conf.interval_days` no greater than
`MaxMdCheckDays` (a zero or an absent message passing, as no check),
`start_hold_conf.seconds` no greater than `MaxStartHoldSeconds` (a zero
passing as no hold, and an absent message as the default hold,
`model.ResolveStartHoldSeconds`), and
`ValidateBdevConf` of `bdev_conf` when the cluster carries one. The ladder predicate is one unexported function shared
with `model.ResolveDnBinConf`, so resolver and validator cannot drift; the
all-zero shift set a `ClusterConf` written without a `dn_bin_conf` would
carry is therefore NOT valid stored state.

The two stored messages `architecture.md`, Common validation, exempts are
stored exactly as sent: `CreateStoragePool` stores `event_threshold` as the
request carried it, and `model.ResolveEventThreshold` resolves it
member-wise where it is read; `CreateClone` and `CreateMigration` store the
`dm_clone_conf` hydration pair as it arrived (`architecture.md`, Clones and
Migrations), the sp worker resolving a migration's (`dnv-worker.md` RW15)
and the cn agent messaging only a clone's non-zero members (`cnagent.md`
CN18).

GW12. **Id minting.** Cluster-scoped ids follow `architecture.md`, Globals:
id allocation + shard buckets, inside the STM: `mintClusterId`, which
`CreateDiskNode`, `CreateControllerNode` and `CreateStoragePool` run on the
global they read, draws the id and the shard code and applies the
`Max*CntPerCluster` gate, then the refusal of a `next_id` with no id left
(`architecture.md`, Globals: id allocation + shard buckets);
`releaseShard`, which `DeleteDiskNode` and `DeleteControllerNode` run,
gives the bucket entry back (the sp drain gives an SP's back from the
worker, `dnv-worker.md` SPD12). Per-SP ids come from `SpConf.next_id`:
`model.SpNextId` reads it clamped to `SpFirstId` and mutates nothing, and
`model.TakeSpIds` takes a run of ids from it, advancing it past the run,
and refuses a run the counter has too few ids left for — the gateway
through a `spIdMinter`, created inside the STM closure on the `SpConf`
being written, whose commit takes the minted run once, so a retried
attempt mints the same ids; the `model` ops take theirs directly. A thin
device's `dev_id` comes from `SpConf.next_dev_id`
through `nextDevId`, advanced once per device. The id step moves the
counters this rule draws from (GW19).

GW14. **Bitmaps are opaque.** Bitmap bytes cross the gateway VERBATIM
in both directions: `AppendCloneBitmap` stores what the request carries
(Clones — the push of those stored chunks to the agents is the worker's,
`dnv-worker.md` BM1 to BM6, not a gateway RPC), and the bitmap read
replies the agent's bytes (Bitmap reads). A migration's bitmap never
crosses the gateway: the worker reads it from the agent itself
(`dnv-worker.md` BM7).
The wire convention — 1 means unmapped or never written, least significant
bit first — is produced and consumed by the agents (`architecture.md`, raid0
bitmap math: the single inversion at the agent boundary); the gateway never
converts or trims a bit and reads none for its meaning, so it can never
disagree with the agents about the convention. The one comparison it makes
is of stored bytes with a request's, byte for byte, to recognise a page sent
again at the position it names: a clone page with the stored bytes at the
offset it names (`architecture.md`, Clones).

## Handlers by resource group

Each RPC gets its mechanism: the work before the STM, the STM outline in key
and helper terms, the bump and the reply. Semantics, exact error sentences
and field-level rules stay in the cited section of `architecture.md`,
`service Gateway` — RPC specifications; nothing below overrides them.

### Clusters

* **CreateCluster** — validate the confs (`architecture.md`, Common
  validation) on the RAW request, where a zero still asks for the default,
  and its `bdev_conf` once more resolved, where an omitted member meets the
  geometry rules as its default, its constant or, for the stripe, the
  stripe its block gives (a refusal is `INVALID_ARGUMENT`, and the
  message says the defaults were filled in); the `dn_bin_conf` shifts are
  judged as a set, all-or-nothing (`architecture.md`, DN bins). Stamp the
  creation epoch **once, outside** the STM, so that the STM's retries of
  this attempt reuse it (`architecture.md`, Clusters), and build the message
  to store outside it too, for the same reason: it is
  `model.ResolveClusterConf` of the request's conf members plus that epoch,
  so `bdev_conf`, `dn_bin_conf`, `alloc_conf`, `health_check_conf`,
  `md_check_conf` and `start_hold_conf` land concrete — an absent
  `md_check_conf` as the default interval and an absent `start_hold_conf`
  as the default hold, a present one as given, its zero included — while `qos_ratio`
  and the epoch, which have no default, always pass through as given (GW11;
  validation first, resolution second — never the other way round). STM:
  `ClusterConfKey` of the name present is `ALREADY_EXISTS`; compute the
  cluster id from the name and the epoch; any of `DnGlobalKey`, `CnGlobalKey`
  and `SpGlobalKey` of that id present is `ALREADY_EXISTS` (the
  hash-collision guard, re-evaluated inside every attempt); put that
  `ClusterConf` and the three globals, each at its first id with
  `ShardBucketSize` zero buckets. Reply the cluster id.
* **DeleteCluster** — STM: resolve; the emptiness check is a zero
  `shard_bucket` sum on **all three** globals (`architecture.md`, Globals:
  id allocation + shard buckets, makes the sum the live object count, so no
  range read is needed), else `FAILED_PRECONDITION`; delete the
  `ClusterConf` and the three globals. Reply the cluster id.
* **GetCluster** — one STM: the `ClusterConf`, the cluster id, the three
  globals (a missing global is `ABORTED`). Reply the name, the cluster id,
  the conf and the globals.
* **ListClusters** — no cluster resolution (with `CreateCluster` and
  `ListWorkers`, one of the three RPCs that resolve no cluster, GW5); a paged
  plain range over `ClusterConfPrefix` (GW10); the names are the key
  suffixes.
* **AdvanceClusterIds** — validate the name (GW4); GW19's transactions.
  Reply the cluster id and the count of pools whose `SpConf` it moved.
* **GetClusterCapacity** — validate the name (GW4) and the pool shape
  (`validateCapacityShape`: the three counts against the bounds
  `CreateStoragePool` holds, before any read); the plain `ClusterConf` read
  of the cluster-scoped lists for the cluster id (GW5) and the stored-conf
  gate (GW11); a plain `SpGlobal` read for the pool count; then two plain
  ranges, `DnConfPrefix` and `CnConfPrefix` of that id, outside any STM,
  each record decoded and copied into its entry: the counts, the flags and
  the pointer count, never the pointer list. The zero counts are resolved
  as `CreateStoragePool` resolves them, the legs from the stored
  `bdev_conf` (`legCntOf`), and `model.ClusterSpaceOf` computes the space
  answer from the decoded records. Reply the defaulted name, the extent
  size, the two entry maps and the space (`architecture.md`, Clusters).
* **GetClusterStatus** — validate the name and the page arguments (GW4,
  GW10); GW20's reads and calls, with no STM. Reply the defaulted name, the
  rows of the cluster on the first page and of each pool read, the token,
  the count of pools read and the read's time (`architecture.md`,
  Clusters). A pool's row of a grow that waits (`waitingGrowRows`) reads
  the loaded records and the read's time alone: the newest grown group of
  a kind not ready (`newestGrownNotReady`) whose `grow_epoch` plus the
  pool's resolved `side_unhealthy` lies before the read, in a pool below
  `SP_LEVEL_NO_THINPOOL`.

GW19. **The id step.** `AdvanceClusterIds` runs the transactions of
`architecture.md`, Clusters, which owns their order and their reasons. One
STM resolves the cluster and moves the three globals' `next_id` with
`advanceNextId`. A keys-only range of `SpConfPrefix` of the cluster id,
outside any STM, then lists the pools, and one STM per listed pool reads its
`SpConf`, moves its `next_id` with `advanceNextId` over `model.SpNextId` and
its `next_dev_id` with `advanceDevId`, and puts it back. A pool whose
`SpConf` is gone is skipped and nothing is put for it; a pool latched for
deletion moves like any other. `advanceNextId` refuses a counter it cannot
move by its stride without wrapping (`architecture.md`, Clusters, has the
error), and that transaction puts nothing. The pool transactions read no
`ClusterConf`: the first transaction's cluster id names their keys, and a
pool of a recreated cluster lives under another id. No transaction reads or
bumps a rev key. An error in any transaction ends the RPC there.

GW20. **The status read.** `GetClusterStatus` (`gateway/status.go`) is the
one handler that calls agents for many objects in one request, so it bounds
those calls itself, for its own request alone. It validates the name and the
page arguments (GW4, GW10), reads the `ClusterConf` plainly and judges it
with `model.ValidateClusterConf`, as every reader that computes from it
does, since the read takes the cntlr interval, the extent size and the batch
from it, and pages the pools with `pageNames`. The first page's cluster rows
come from plain ranges outside any STM: the worker registry, the cluster's
`dn_conf` and `cn_conf` prefixes, and, for the revision keys, one keys-only
range of each node kind's whole prefix (`model.DnRevKindPrefix`,
`model.CnRevKindPrefix`), since those keys are sharded and a cluster has no
prefix of its own there; a key of another cluster is passed over. Each pool
is loaded with `model.LoadSp`, its own snapshot, as the sp worker loads it,
so its lost records are the ones the worker meets (`dnv-worker.md` MD3); a
pool whose `SpConf` went since the page's range is passed over, and one
whose load fails gives one row of status UNKNOWN. A deleting pool gives one
row and is called for nothing. Then the pool's calls run at once: the
primary's `GetCntlrInfo`, without the block counters, and each migration
destination's `GetSideInfo` past its cutover. After them, one point read of
the pool's id-to-name record (`model.SpNameKey`), which no list names and so
`LoadSp` does not report missing, gives its lost-record row; it is not made
when the calls have used up the pool's time. The rows are computed from the
state and the answers (`statusPoolRows`): the sp worker's gate on its
groups' lengths to zero (`model.SideZeroBytes`,
`model.CloneMetaSideZeroBytes`; `dnv-worker.md` AR1) gives the pool's row of
a conf the worker refuses, and then no grow is judged; the grow's checks
come from `model`'s own rules — `GrowPending`, `DataCovered`,
`GrowExtCnt`, `SpCnCharges` and `CnBudgetShort` — in the order the sp
worker's grow takes them (`dnv-worker.md` AR6), the disk node scan last, its
answer kept for the request by the extent and leg counts; a repair's wait
comes from `model.RepairWaitReason`, the worker's own rule (`dnv-worker.md`
AR17). Every text that carries a record's, an error's or an agent's words
goes through `common.CutRowText`, the agents' cut of the row text cap. The
bounds, each the request's own: `StatusFanOut` pools are read at once; each
agent call takes `StatusAgentTimeout`; and the first call of the request to
an address runs alone (`statusGate`): the pools that need the same address
wait for it and skip the address with an UNKNOWN row when it failed, the
deadline included, or call themselves once it answered, so a node that hangs
costs one budget per read whatever it holds. An agent's code is an answer,
never a failure of the address. Under a request deadline the read starts no
pool once less than one agent budget and `statusReplyMargin` are left, the
page's first pool aside so that a page always moves on, and ends each pool's
work the margin before the deadline. A pool whose work that deadline ended,
its own calls or its wait at the gate cut by the read and not by its agent,
is left off the page with every pool after it, the page's first aside. Each
call records whether the read's deadline ended it, and the first pool's rows
say the read's time ran out for such a call, the name read and the grow's
disk node scan, and blame its agent for any other failure; the token is the
key of the last pool the page keeps, a key of the pool prefix as GW10's
tokens are, so the next page reads the pool the deadline cut. An agent that
fails is a row, never the call's error (AG3); only the cluster's own reads
answer `ABORTED`. Nothing is written.

### Worker registry

`gateway/worker.go` holds the one RPC of the worker registry, which belongs
to no cluster (`architecture.md`, Worker registry).

* **ListWorkers** — no validation, since the request has no field, and no
  cluster resolution (GW5); one plain range of
  `model.WorkerRegistryPrefix`, every role at once, outside any STM, as the
  paged lists range their prefixes (GW10), and a failed range is `ABORTED`
  as `pageNames` answers one (GW7). Not paged. Each key goes through
  `model.ParseWorkerRegKey`, and one it refuses is skipped; each value is
  decoded through `etcdutil`, and one that does not decode is listed as a
  fresh empty `WorkerReg`, never as the message the failed decode half
  filled — the reasons are `architecture.md`, Worker registry. Reply every
  role of `common.WorkerRoleDn`, `common.WorkerRoleCn` and
  `common.WorkerRoleSp`, each with its registrations by seed, and empty
  when it has none. Nothing is written and no revision moves.

### Disk nodes

* **CreateDiskNode** — validate; **pre-STM agent call** (AG1):
  `DiskNodeAgent.GetDnSize` at `addr_port` (with a zero `dn_id`, for logging
  only) — a gRPC failure is `ABORTED`. With no `location` in the request,
  the reply's host name is the node's `location` (`hostNameLocation`): one
  that `validateOptionalName` refuses, or an empty one, is
  `FAILED_PRECONDITION` with the advice to pass a `location`, before the
  STM, so nothing is written (`architecture.md`, Disk nodes). STM:
  resolve; `DnConfKey` of the
  cluster id and `addr_port` present is `ALREADY_EXISTS`;
  `model.ValidateClusterConf` on the `ClusterConf` this STM resolved, then
  `total_ext_cnt` is the reported bytes divided by `extent_size`
  (`architecture.md`, Size → extents; the stored size used as stored — a
  zero is refused `ABORTED`, never divided by, GW11); gate and mint from
  `DnGlobal` (GW12, `RESOURCE_EXHAUSTED` at `MaxDnCntPerCluster`); put the
  `DnConf` with `dn_id`, `shard_code`, `disabled`, `nvme_tr_conf`,
  `location`, `total_ext_cnt` and a `free_ext_cnt` equal to the total;
  `model.MaintainDnCapacity` from no record to the new one; put the `DnRev`
  holding the `addr_port` and the first revision at `DnRevKey`; put the
  global. Reply `dn_id`.
* **DeleteDiskNode** — the checks (`dnDeleteChecks`): resolve; the
  `DnConf` by address (`NOT_FOUND`); the token against the `DnRev` (GW6),
  whose read gives the node's revision; a non-empty `side_ptr_list` is
  `FAILED_PRECONDITION`, with `force` too, and its message does not offer
  `force`; the `DnGlobal` (missing is `ABORTED`);
  `model.ValidateClusterConf` on the resolved `ClusterConf` (a zero is
  `ABORTED`, GW11 — the last check of the request and the store, since
  the capacity key to delete is named by the stored ladder). Without `force`,
  two-phase (AG4): a planning `Snapshot` makes the checks and keeps the
  cluster id, the `dn_id` and the revision; then `checkDnClean` calls
  `GetDnInfo` with the cluster id and the `dn_id` and no `read_io_stats`,
  and refuses `FAILED_PRECONDITION`, naming what failed and `force`, an
  agent that does not answer, a non-zero code (its details passed on, cut
  to the row text cap by `common.CutRowText`), no observation, a revision
  other than the node's, and disk counts that are
  absent or show a side record, a held extent or a held clone-metadata
  unit (`architecture.md`, Disk nodes); the deciding STM makes the checks
  again and answers `ABORTED`, with "run the delete again", when the
  cluster id, the `dn_id` or the revision differs from the planning one
  (`nodePlan.sameNode`), whatever the token. With `force`, the one STM
  with the checks and no agent call. The STM deletes the `DnRev`, the
  `DnConf` and the capacity key (`MaintainDnCapacity` from the old record
  to none) and decrements the node's entry of `DnGlobal`'s `shard_bucket`.
  Reply `dn_id`.
* **GetDiskNode** — one STM: the `DnConf`, then the `DnRev` by the id and
  shard it read (missing is `ABORTED`). Reply the address, the conf and the
  rev (the token source).
* **ListDiskNodes** — a paged plain range over `DnConfPrefix` of the cluster
  id (GW10).
* **UpdateDiskNodeDisabled** — STM: resolve; the `DnConf`; the token; if
  `disabled` already equals the request: OK, nothing written (GW6); else
  `model.ValidateClusterConf` (a zero is `ABORTED`, GW11 — below the no-op,
  above the put, because the flip moves a capacity key the stored ladder
  names), set it and `MaintainDnCapacity` from the old record to the new.
  **No revision bump, no agent call** (`architecture.md`, Disk nodes). Reply
  `dn_id`.
* **InspectDiskNode** — STM: read the `DnConf` (the ids that address the
  agent request and the log line; the resolving `Snapshot` supplies the
  cluster id, so there is no plain pre-read — and no `DnRev` read: a client
  that wants the desired-state token calls `GetDiskNode`); **after** the STM
  call `GetDnInfo` with the cluster id, `dn_id` and `read_io_stats`, the one
  caller that sets it (`dnagent.md` DN16); an agent failure is `ABORTED`.
  Reply `applied_revision`, `dn_info` and `obs_stamp` from the agent's
  reply, which answers from its latest observation of the node, or with none
  while the agent starts (`dnagent.md` SH33), its code not passed on,
  `disk_io_stats`, which the agent reads at the call (`dnagent.md` DN16),
  and `obs_age`, which `obsAge` derives from that stamp (`architecture.md`,
  Disk nodes — deliberately the observation's revision, never the stored rev
  key).

### Controller nodes

The exact mirror of Disk nodes over `CnConf`, the CN capacity key, `CnRev`
and `CnGlobal`, with these differences: `CreateControllerNode` calls
`GetCnSize` and maps the reply through `cnCapBudget`, the budget rule of
`architecture.md`, Size → extents, and takes the reply's host name as the
default `location` as `CreateDiskNode` does; the per-cluster ceiling is
`MaxCnCntPerCluster`; the delete's occupancy precondition is
`cntlr_ptr_list`, and its proof without `force` (`checkCnClean`) calls
`GetCnInfo` and reads no counts, since a controller node holds no disk;
the capacity maintenance is `model.MaintainCnCapacity`,
which needs no `ClusterConf` because a CN capacity key carries no bin index
(`architecture.md`, Capacity index keys), so `DeleteControllerNode` and
`UpdateControllerNodeDisabled` run no `model.ValidateClusterConf` (GW11);
`InspectControllerNode` calls `GetCnInfo`, which takes no `read_io_stats`,
and its reply carries no block counters: a controller node's counters are
per cntlr, in `InspectCntlr`. The same six RPCs: create, delete, get, list,
update-disabled and inspect; and a seventh with no disk node twin:

* **CheckControllerNodeRestore** — the restore check (`architecture.md`,
  Controller nodes). One `Snapshot`: resolve; the `CnConf` at `addr_port`,
  absent being no error, and, when present, its `CnRev` through the token
  check with no token (missing is `ABORTED`). Then `heldCnStore` calls
  `GetCnStore` with the cluster id and a zero `cn_id`, and answers
  `FAILED_PRECONDITION` for an agent that does not answer, that starts or
  that answers any code but zero and `ReplyCodeUnknownObject`, its details
  cut to the row text cap by `common.CutRowText`; the unknown object reads
  as no node held. Then `judgeHeldCntlr` makes the row of each pointer of
  each held node and the handler one per pointer of the restored node that
  no held node of its id names: `readRestoredPool` reads the pool in a
  `Snapshot` of its own, its Slice, Clone and Repair records, and the
  RetiredLeg record of each retired leg its counted groups list, only when
  the restored revision is below the held one, a repair counted only when
  its parent group is a slice's or the meta group of a clone that is not
  latched, as the worker's `cntlrRepairs` sends it, and `layoutSides` counts
  the restored layout's sides as the agent counts the held one, a retired
  leg by the side id of its record; past the backup it also reads the
  `Cntlr` record of the row's cntlr when `cntlr_id_list` names it, every
  `Migration`, and every `Subsystem` and `Transfer`, parking each
  namespace by `nsParked`, the rule of `cnagent.md` CN16 over the clones
  that are not latched. `breakReasons` gives one text per cause, in
  order: a held layout with a higher largest side id or another count; a
  summary that says a retired leg's side id is missing, which also sets
  the row's `held_side_ids_incomplete`; each migration whose cutover is
  pending in the restored state, in one text; a held role other than the
  restored one; and the namespaces the restored state holds and does not
  park that the summary's `parked_ns_id_list` names, in one text, and that
  its `ns_id_list` lacks, deleted since the backup, in another, each text
  cut by `common.CutRowText`. A row with any text reads
  `RESTORE_VERDICT_MAY_BREAK` and carries them in `break_reason_list`;
  the texts are made only once the thin devices' range found no lost
  record, so a row read RECORD_LOST carries none. `restoredTds` ranges `ThinDevicePrefix` with
  no STM for a pool held above the restored revision. A record the row reads
  that does not decode, or that is missing past the pool's id-to-name
  record, whose absence alone reads as a pool made after the backup, a name
  of `td_name_list` with no record, and a cntlr of the restored node, held
  or not, whose pool the restored state names nowhere make the row
  `RESTORE_VERDICT_RECORD_LOST`, never the answer's error. Rows are ordered
  by `sp_id`, `cntlr_id` and node, thin devices by `td_id`. Nothing is
  written.

### Storage pools and GrowSlice

* **CreateStoragePool** — validate the request against the bounds and
  defaults of `architecture.md`, Storage pools, and the confs against
  `architecture.md`, Common validation: a zero `cntlr_cnt` and a zero
  `slice_cnt` are replaced — before the bound above each is judged — by
  `DefaultCntlrCntPerSp` and `DefaultSliceCntPerSp`, and it is the
  substituted count the plan and the STM's slice loop build the SP from.
  A stripe the request names is then judged against that count by
  `validateStripeProduct` (`architecture.md`, Common validation, the
  stripe's product) before any read, and so are the threshold rules that
  read the request alone (`validateEventThreshold`): the leg threshold
  over the side one, and the legs' fast IO fail timeout in its range
  whatever the cluster's intervals, which no rule therefore reads
  `ClusterConf` for.

  Pre-STM plan (`planSpGroups`), in GW15's order: per slice the meta group
  (one extent) first, then the data group (`init_ext_cnt` extents) — extent
  counts only; `model.GroupBlocks` turns each into `meta_blocks` and
  `data_blocks` in the STM, where the conf it needs has been read and
  checked. Candidate unit (GW9): first the checks `architecture.md`,
  Storage pools, makes before the scans, in its order and for its reasons.
  Over a plain pre-read of `ClusterConf` (`spPreReadCluster`) they are
  `model.ValidateClusterConf` (a stored conf that fails it is `ABORTED`,
  GW11), the overflow of `init_ext_cnt` times that conf's `extent_size`,
  the same merge and resolution the STM makes below (`resolveSpBdevConf`),
  which gives the scans their leg count (`legCntOf`) and which
  `validateMergedBdevConf` judges, the stripe's product included, and
  `validateMdBitmapRoom` at that conf's `extent_size` (`architecture.md`,
  Common validation, the md bitmap's room),
  and `validatePrimaryThreshold`; then come the name (`spNameUnused`) on a
  plain read of its own, and on another of `SpGlobal` the pool count
  ceiling and then whether its id counter has an id left
  (`spCntBelowCeiling`). Then the CN scan per cntlr (`pickCn`) and the DN
  scan per group (`pickSpDns`, one `pickDns` each) of `architecture.md`,
  Per-operation allocation, which owns the growing black list, the random
  pick, the two CN tiers and the DN scans' first preference: the DN scans
  run with the first CN pick's `location` as tier 1's exclusion, and once
  more with none when those refuse `RESOURCE_EXHAUSTED`. The DN scan asks
  for no metadata units, since a new SP has no migration, and a CN counts
  none; too few at any point is `RESOURCE_EXHAUSTED`, and the CN scan,
  which runs first, answers when both would refuse.

  STM, in this order: resolve; build the `bdev_conf` to store with
  `resolveSpBdevConf`, `model.ResolveSpBdevConf` of `mergeSpBdevConf` of the
  request over the cluster's at the substituted slice count — merge first
  so an omitted member still inherits from the cluster, resolve second so
  what is stored is concrete, and narrow an inherited stripe last
  (`architecture.md`, Storage pools; `architecture.md`, Common validation;
  GW11) — and fail the unit right there, before a single other key is
  read, when this transaction's cluster id or that conf's leg count no
  longer matches the one the scan drew its picks for;
  `validateMergedBdevConf` on that conf, `model.ValidateClusterConf` of
  the `ClusterConf` this STM read (a failure is `ABORTED`, GW11) and
  `validateMdBitmapRoom` on the conf at that `ClusterConf`'s `extent_size`,
  and `validatePrimaryThreshold` on that `ClusterConf`, once more each (`ClusterConf` is
  write-once, so for an unchanged cluster id these repeat the verdicts of
  the pre-read, but the in-STM read is the authoritative one);
  `SpConfKey` present is `ALREADY_EXISTS`; mint `sp_id`
  and the shard from `SpGlobal` (GW12), then every `cntlr_id` in pick order
  (GW15); `model.ValidateClusterConf` before the cluster's `extent_size` is
  used (a zero is `ABORTED`, GW11); then per slice its `slice_id` and, in
  plan order, its meta group before its data group — for each,
  `model.GroupBlocks` against that `extent_size` and the resolved
  `bdev_conf`, then the `grp_id` and per leg a `leg_id` and its `side_id`,
  re-reading that leg's pick capacity key as its DN is charged and failing
  the unit if it moved; then every CN pick likewise. Everything above is
  staged in memory: a refusal — a moved capacity key above all — returns
  before the first `Put`, not merely before the commit. The write set is
  then one `Slice` per slice (every `Side` with `provisioned` false), one
  `Cntlr` per pick (the first `primary` and `settling`, `dnv-worker.md` HL2;
  `cntlid_slot` the next unused slot in list order), the `SpConf` (that
  `bdev_conf`; `event_threshold` exactly as the request sent it — one of
  GW11's two read-time exceptions; the lists, `next_id`, `next_dev_id` at
  its first value, `deleting` false), the `SpName` and the `SpRev` holding
  the `sp_name` and the first revision; then per DN: `free_ext_cnt` lowered
  by the group's extents, `side_ptr_list` appended, `MaintainDnCapacity`,
  and `BumpDnRev` once; per CN likewise with `cntlr_ptr_list` and
  `BumpCnRev`; last the updated `SpGlobal`. Reply `sp_id`.

GW15. **One fixed create order.** `CreateStoragePool` plans, scans and
mints in one fixed order — per slice the one-extent meta group, then the
data group; the cntlr scan before the group scans, and the cntlr ids first,
in pick order, then per slice its `slice_id` and, in plan order, each
group's `grp_id` and per leg a `leg_id` and its `side_id` — so the scan's
picks and the STM's ids line up by plan position, and a re-run of the STM
closure on the same picks and reads (GW8) reproduces exactly the same write
set.

* **DeleteStoragePool** — it LATCHES, and the sp worker drains
  (`dnv-worker.md` SPD9 to SPD12, SPD15). STM: `openSpFlags` with the `deleting`
  gate off — resolve, read the `SpRev`, run GW6's token check; if `deleting`
  is ALREADY true return OK here, with **no writes and no bump** (a repeat
  delete must not invalidate every client's token to force a pointless
  re-resolve; the token check has already run, so a stale token still
  ABORTs first); else all five name lists (`td_name_list`, `nqn_list`,
  `clone_name_list`, `xfer_name_list`, `migr_name_list`) empty, else
  `FAILED_PRECONDITION`; put the `SpConf` with `deleting` true; `BumpSpRev`.
  Reply `sp_id` — the repeat-delete no-op replies with it too, since the RPC
  resolved the SP before short-circuiting and `DeleteStoragePoolReply`
  carries nothing else. The emptiness check and the latch share the STM
  with the reads, so they are atomic, and once latched `resolveSp`'s
  `rejectDeleting` gate refuses every other mutator — no new child can
  appear after the check, ever; no path writes `deleting` false back on an
  existing SP, so the latch is one-way across restarts of every component.
  Nothing else is written: the SP, its cntlrs, its slices and every extent
  they charge survive the reply. Why the teardown is a drain and not one
  transaction, and what a caller sees while it runs, is `architecture.md`,
  Storage pools.
* **GetStoragePool** — one STM: the `SpConf`, the `SpRev`, every listed
  `Cntlr`, every listed `Slice`; a missing listed key is `ABORTED`. Reply all
  of it (the `SpRev` is the token source).
* **ListStoragePools** — a paged plain range over `SpConfPrefix` of the
  cluster id (GW10).
* **UpdateStoragePoolCntlidSlotList** — the list's own validation of
  `architecture.md`, Storage pools, first, before any read (GW4): an empty
  list, a duplicate or a value at or above `CnCntlidSlotCnt` is
  `INVALID_ARGUMENT`. Then STM: resolve; token; removing a slot in use is
  `INVALID_ARGUMENT`, the
  slots in use read from every cntlr, every slice's sides and the sides of
  every clone's clone meta group, the clones read for that alone, and a
  refusal for a clone's side naming the clone; no repair and no retired leg
  is read, for the reason `architecture.md`, Storage pools, gives; write;
  `BumpSpRev`. Reply `sp_id`.
* **UpdateStoragePoolLevel** — STM: resolve; token; set `sp_level`;
  `BumpSpRev`. Reply `sp_id`.
* **FindStoragePoolNames** — an `etcdutil` snapshot (one store revision, no
  STM): the `ClusterConf` to the cluster id, then per requested `sp_id` a
  read of `SpNameKey`; unknown ids are omitted, never an error. Reply the
  map.
* **GrowSlice** — validate the exclusivity of `architecture.md`, GrowSlice
  (a data grow states a positive `ext_cnt`, a meta grow none, else
  `INVALID_ARGUMENT`). Everything after that runs inside one candidate unit
  (GW9), so every round re-plans from a fresh read instead of asking for the
  same stale size again: a `Snapshot` pre-read for planning, opened with
  `openSp` (resolution, the `deleting` gate and the token check first,
  GW6), whose `ClusterConf` and SP `bdev_conf` are both validated before
  anything is sized from them (`model.ValidateClusterConf`,
  `model.ValidateBdevConf`; a failure is `ABORTED`, GW11) — which is also
  what keeps a false from `model.MetaLadderExtCnt` meaning the meta cap and
  nothing else, since an unvalidated zero `extent_size` would report the
  same false and reach the operator as a metadata ceiling; the group ceiling
  of `architecture.md`, GrowSlice, answered from the same snapshot
  (`model.GrpListFull`, `FAILED_PRECONDITION`, GW7's object-state class)
  ahead of the scan, so a cluster short of DNs cannot report it as
  `RESOURCE_EXHAUSTED` (`model.GrowSlice` re-checks it in-STM); a newest
  grown group of the kind that is not ready (`newestGrownNotReady`, the
  slice holding two or more of the kind and `model.GroupNotReady` true),
  `FAILED_PRECONDITION` naming it and WithdrawGrow, skipped at a level
  where the model's level gate answers, and decided again by
  `model.GrowSlice` in its STM (`ReasonGrowNotReady`); the new
  group's size, computed as `architecture.md`, GrowSlice, says and never
  taken from the request's `ext_cnt` — the slice's first data group's
  `ext_cnt` for a data grow, `model.MetaLadderExtCnt` of the slice's meta
  total for a meta grow, the cap reached being `FAILED_PRECONDITION`; the
  CN-budget pre-check (`growSliceCnBudget`), a second `Snapshot` over the
  SP's cntlrs and their `CnConf`s at one store revision — every cntlr stacks
  the new group, so every one of their CNs needs that size free, and a
  shortfall is the `RESOURCE_EXHAUSTED` of `architecture.md`, GrowSlice,
  named with the CN that is short; a cntlr or `CnConf` key gone under the
  snapshot is skipped, not raised, because this only gives the common case
  its documented code and the STM re-reads all of it; the DN scan
  (`pickDns`) with the request's own black list and no location exclusion,
  as `architecture.md`, Per-operation allocation, has it; then the call of
  `model.GrowSlice` with the token's revision as its expected revision
  (GW6), the largest pool total (`math.MaxUint64`), which leaves the
  pending rule of `dnv-worker.md` AR6 to the worker's auto-grow
  (`architecture.md`, GrowSlice), and the gateway's clock as the group's
  `grow_epoch`. The op re-validates in-STM and bumps the
  `SpRev`, the leg DNs' revs and — through `chargeSpCns`, which charges the
  CN of every cntlr — those CNs' `CnRev`s itself. A CN that loses its budget
  in the window after the pre-check is therefore still refused, but by
  `chargeSpCns` as an `ErrPrecondition`, so the caller sees
  `FAILED_PRECONDITION` instead: the pre-check is deliberately the generous
  one, since only the STM decides. `ReasonStaleRevision` maps to `ABORTED`,
  any other `ErrPrecondition` to `FAILED_PRECONDITION`. Reply `slice_id` and
  `grp_id`.
* **WithdrawGrow** — validate the names and a non-zero `grp_id`; one
  `Snapshot` opened with `openSp` (GW6) names the refusals of
  `architecture.md`, WithdrawGrow, the operator acts on: the slice not in
  the pool `NOT_FOUND`, and a pool at `SP_LEVEL_NO_THINPOOL` or above, a
  first group, a group that is not the newest of its kind and a ready one
  `FAILED_PRECONDITION`; then `model.ValidateClusterConf` and
  `model.ValidateBdevConf` (`ABORTED`, GW11) and
  `model.WithdrawGrowAsked` with the token's revision, which decides in
  its own STM and re-checks them all with the model's other guards,
  mapped as `GrowSlice`'s errors are. Reply `slice_id` and `grp_id`.

### Cntlrs and inspects

* **CreateCntlr** — validate the slot; a candidate unit for one CN
  (`pickCn`, with the CN exclusion and the two tiers of `architecture.md`,
  Per-operation allocation). Each round plans from one read-only `Snapshot` opened with
  `openSp`, as `CreateMigration`'s planning read is: resolve, the `deleting`
  gate included, then the token (GW6), so a deleting SP is
  `FAILED_PRECONDITION` and a stale token `ABORTED` before the scan can
  answer `RESOURCE_EXHAUSTED` for want of an eligible CN — to a stale
  client, an answer computed against cntlrs it has not read; then the
  `MaxCntlrCntPerSp` ceiling (`cntlrCntBelowCeiling`, GW9); then the SP's
  slices, which size the scan, and its cntlrs, whose CNs the scan excludes;
  no clone is read, since a clone charges no CN (`architecture.md`,
  Metadata units). The locations are read after the snapshot,
  from those CNs' `CnConf`s (`cnLocations`), outside every transaction.
  STM: resolve; token; the `MaxCntlrCntPerSp` ceiling again; the slot in
  `cntlid_slot_list` and unused, else
  `INVALID_ARGUMENT`; the gain check of `architecture.md`, Per-operation
  allocation — the SP as this STM reads it having a cntlr on a CN the
  round's plan did not hold is candidate changed (GW9), which only a
  token-less request meets, since the gain's `SpRev` bump fails a sent token
  first; the footprint from the slices this STM reads; re-verify the pick's
  capacity key and, on the CN's record, its room for the footprint
  (`verifyPick`; a CN that filled up since the scan is candidate changed);
  mint `cntlr_id`; put the `Cntlr` (`addr_port`, the CN's `nvme_tr_conf`,
  `cntlid_slot`, `primary` false, `disabled` false); append to
  `cntlr_id_list`; the CN's bookkeeping — the footprint — and one
  `BumpCnRev`; set the
  transport list of **every** `CdcEntry` of the SP by the listing rule
  (`architecture.md` [D18]) from the cntlrs this STM reads, the new one
  included (`syncCdcEntries`, which walks `nqn_list` to each `Subsystem`
  and its `CdcEntryKey` through `eachCdcEntry` and rebuilds a missing entry
  with `rebuildCdcEntry` before setting it, as `architecture.md`, Cntlrs,
  requires; `DeleteCntlr` and a flag-changing `UpdateCntlrEnabled` set the
  entries the same way, each after its own `Cntlr` write); `BumpSpRev`.
  Reply `cntlr_id`.
* **DeleteCntlr** — STM: resolve; token; `primary` false and `disabled`
  true, else `FAILED_PRECONDITION`; reverse everything `CreateCntlr` did
  (the id list, the key, the CN's bookkeeping with `BumpCnRev` — the
  footprint back — and every `CdcEntry` set again by the listing rule,
  without the deleted cntlr); `BumpSpRev`. Reply `cntlr_id`.
* **UpdateCntlrEnabled** — STM: resolve; token; a no-op when already at the
  requested state (GW6); else set `disabled` to the negation of `enabled` —
  an enable of a cntlr that is still `primary` also sets `settling`
  (`dnv-worker.md` HL2: the re-enabled primary builds its stack from the
  standby shape, as a promoted one does) — put the `Cntlr`, and then set
  every `CdcEntry` by the listing rule (`syncCdcEntries`;
  `architecture.md` [D18]); `BumpSpRev`. Reply `cntlr_id` and `enabled`.
* **InspectCntlr** — STM: resolve; the `Cntlr` by id (`NOT_FOUND`), keeping
  its `addr_port` and the `CnConf`'s `cn_id` (read `CnConfKey` of that
  address in the same STM; deliberately no `SpRev` read); after the STM,
  `GetCntlrInfo` with the cluster id, `cn_id`, `sp_id`, `cntlr_id` and
  `read_io_stats` at that address; a failure is `ABORTED`. Reply
  `applied_revision`, `cntlr_info`, `obs_stamp` and `obs_age` as
  `InspectDiskNode` replies its own, and the agent's `io_stats`
  (`architecture.md`, Cntlrs). It is the one `GetCntlrInfo` caller that
  sets `read_io_stats`: the others read rows alone, and cost the agent no
  read of the node (`cnagent.md` CN23). Beside a person, through `dnvctl`,
  the CSI driver's controller plugin polls it during a detach's and an
  attach's wait for the primary's report of the volume's subsystem
  (`csi.md` CSI10), so each inspect costs that agent one read of its
  counters and the gateway's log the whole reply.
* **InspectStoragePool** — validate the names (GW4); one `Snapshot`:
  resolve (`openSpRead`, no token, a deleting pool read as any other); the
  stored-conf gate on the pool's `bdev_conf` (GW11); every slice
  (`loadSlices`) and every thin device of `td_name_list`, a missing key
  `ABORTED`; the primary and its controller node's `cn_id`
  (`openBitmapTarget`, as the bitmap read resolves them: no primary is
  `FAILED_PRECONDITION`). After the snapshot, `GetCntlrInfo` about the
  primary at its address, with no `read_io_stats`; a failure is `ABORTED`.
  Reply the primary's `cntlr_id`, `applied_revision`, `obs_stamp` and
  `obs_age` as `InspectCntlr` replies them, and the space
  `model.PoolSpaceOf` computes from the records and the agent's rows
  (`architecture.md`, Storage pools).
* **InspectSide** — STM: resolve; find the side by scanning the SP's slices
  (bounded, `architecture.md`, Cntlrs) for `side_id`, only when no slice
  holds it the clones' clone meta groups (`findCloneSide`), and only when no
  clone holds it the new legs of the pool's repairs and then its retired
  legs (`findRepairSide`), whose sides the dn agents host as a leg's, each
  record read through its list, so the inspect of a slice's side reads no
  clone and no repair (`NOT_FOUND` when none holds it), keeping its DN's
  `addr_port`, its `dn_id` (through `DnConfKey`) and the side pointer, whose
  leg is the slice's, the clone's, the new leg or the retired leg
  (deliberately no `SpRev` read); after the STM, `GetSideInfo` with
  the cluster id, `dn_id` and the side pointer; a failure is `ABORTED`.
  Reply `applied_revision`, `side_info`, `obs_stamp` and `obs_age` as
  `InspectDiskNode` replies its own.

### Thin devices

* **CreateThinDevice** — validate `size` against the stripe multiple that
  `architecture.md`, Thin devices, requires, with the stripe as the SP
  stored it (`bdev_conf.dm_raid0_conf.stripe_size`; state-dependent, so
  checked in-STM, behind a `model.ValidateBdevConf` of that conf — GW11 — so
  a zero stripe is `ABORTED` instead of reaching the "has no slice" refusal,
  which is about `slice_id_list`). STM: resolve; token; `td_name` in
  `td_name_list` or its key present is `ALREADY_EXISTS`; if `ori_name` is
  set: read the origin (`NOT_FOUND` if absent), and its `created` false is
  `FAILED_PRECONDITION` **writing nothing** (no id consumed, no bump —
  `architecture.md`, Thin devices), then an origin named by any
  `Clone.dst_td_id` (walk `clone_name_list` as `DeleteThinDevice` does, a
  draining clone included) is `FAILED_PRECONDITION` naming the clone, also
  writing nothing: while a clone hydrates, a snapshot of its destination
  would capture only the regions hydrated so far; a `next_dev_id` above
  `MaxThinDevId` is `FAILED_PRECONDITION`, writing nothing
  (`architecture.md`, Thin devices); mint `td_id` (GW12), the
  `dev_id` from `next_dev_id`, and `ori_id` as the origin's `dev_id` or
  zero; put the `ThinDevice` with `created` false; append to
  `td_name_list`; `BumpSpRev`. Reply `td_id` and `dev_id`.

GW17. **A snapshot inherits its size.** `CreateThinDevice` accepts a `size`
of zero exactly when `ori_name` is set, and the snapshot then takes its
origin's size; a fresh device asking for a zero `size` is
`INVALID_ARGUMENT` before any read.

* **DeleteThinDevice** — a plan, then a deciding STM, looped as a GW9 unit;
  why the snapshot walk stays out of the deciding transaction and how the
  pool's identity and revision close the window between the two is
  `architecture.md`, Thin devices. Plan, one read-only `Snapshot`: resolve;
  token; the td (`NOT_FOUND`); walk `td_name_list` for the tds whose
  `ori_id` is this td's `dev_id` and whose `created` is false (a listed key
  missing is `ABORTED`) and keep their names, the resolved cluster id and
  `sp_id`, and the `SpRev.revision` it read. STM: resolve; token; a cluster
  id, `sp_id` or `SpRev.revision` other than the plan's is candidate
  changed, re-plan — a token-carrying request fails the token check ahead
  of it, `ABORTED`, no re-plan, unless the name resolves to a pool created
  again at the plan's revision, which passes the token check and re-plans.
  Then the td (`NOT_FOUND`); `FAILED_PRECONDITION` when it is
  referenced by any `Namespace.td_id` (walk `nqn_list` to each `Subsystem`'s
  `ns_list`), by any `Clone.dst_td_id` (walk `clone_name_list`), or when a
  td the plan named still has this td's `dev_id` as its `ori_id` and
  `created` false (re-read those keys only); delete the key, remove the name
  from the list, `BumpSpRev`. Reply `td_id`.
* **ListThinDevices** — one STM: the `SpConf` and every listed td (missing
  is `ABORTED`) into `name_to_td`. This is the documented client wait
  primitive for `created`, and the record's `torn` marks a snapshot to
  delete and re-create (`architecture.md`, Thin devices); a client
  snapshotting a clone's destination also
  waits until the clone is deleted and drained, `GetClone` answering
  `NOT_FOUND`. Neither RPC returns a revision, while the flip bumps `SpRev`
  and so do a clone's latch and the last step of its drain, so a client
  whose snapshot request carries a token re-reads it from
  `GetStoragePool` after its last poll (`architecture.md`, Thin devices).

### Subsystems and namespaces

All pure etcd; every mutator: resolve, token, mutate, `BumpSpRev`. The codes
are those of `architecture.md`, Subsystems, namespaces.

* **CreateSubsystem** — the NQN valid (`architecture.md`, Common
  validation, the dnv-namespace rule included; the discovery NQN can never
  validate) and absent; mint `ss_id`; the serial is
  `ss_id` rendered with `IdKeyFmt` and the model is `NvmeModel`
  (`architecture.md`, [D2]); put the `Subsystem` with an empty `ns_list`
  and the `allowed_hosts`, append to `nqn_list`, and put the `CdcEntry` —
  the NQN, the transport confs of the cntlrs the listing rule lists
  (`model.CdcTrConfList`; `architecture.md` [D18]), the `allowed_hosts` —
  at `CdcEntryKey` of the cluster id, the shard, `sp_id` and `ss_id`. Reply
  `ss_id`.
* **DeleteSubsystem** — the NQN checked for length alone (`architecture.md`,
  Common validation, so a subsystem stored under an NQN the rules refuse can
  still be deleted; the same section says what a CN keeps of one in the dnv
  namespace); the subsystem by NQN; `ns_list` empty; delete the
  `Subsystem`, the `CdcEntry` and the list entry. Reply `ss_id`.
* **ListSubsystems** — one STM: `nqn_list` to each `Subsystem` into
  `nqn_to_subsystem`; a listed key that is missing is `ABORTED`.
* **UpdateSubsystemHosts** — rewrite `allowed_hosts` in **both** the
  `Subsystem` and its `CdcEntry`, and set that entry's transport list by
  the listing rule as every writer does (`architecture.md` [D18]); a
  `CdcEntry` whose key is missing is rebuilt as `CreateSubsystem` writes it
  (`rebuildCdcEntry`) and written with the new hosts. Reply `ss_id`, at once:
  the cut of a host the call takes off the list, where a controller node's
  agent runs in cut mode, follows at the agents' next converge
  (`cnagent.md` CN33).
* **CreateNamespace** — `ns_idx` neither zero nor `NvmeNsidAll`, judged
  with the rest of the validation before any read (GW4), so that a full
  subsystem never answers it with its ceiling; STM: the subsystem by NQN;
  the `MaxNsCntPerSs`
  ceiling; `ns_idx` unused in this subsystem; no transfer naming the place
  as its origin (`nsTransferRef`); the td by `td_name`; no transfer whose
  origin namespace lies on that td (`tdTransferRef`); mint
  `ns_id`; defaults: an empty
  `dev_uuid` is a random RFC 4122 version 4 UUID in the canonical dashed
  form, an empty `dev_nguid` a random NGUID in hex; append to `ns_list`.
  Reply `ns_id`.
* **DeleteNamespace** — the NQN checked for length alone, as for
  `DeleteSubsystem`; locate by NQN and `ns_idx`; no transfer naming it as
  its origin (`nsTransferRef`); remove from `ns_list`. Reply `ns_id`.
* **UpdateNamespaceDev** — a plan, then a deciding STM, looped as a GW9
  unit; why the walk stays out of the deciding transaction is
  `architecture.md`, Subsystems, namespaces. Plan, one read-only
  `Snapshot`: resolve; token; locate the namespace (`NOT_FOUND`); walk
  `td_name_list` for the td whose `td_id` the namespace holds
  (`tdNameById`; a listed key missing, or no td found, is `ABORTED`) and
  keep its name, its id, the resolved cluster id and `sp_id`. STM:
  resolve; token; a cluster id or `sp_id` other than the plan's is
  candidate changed; locate the namespace; no transfer naming it as its
  origin (`nsTransferRef`); resolve the new `td_name`; no transfer whose
  origin namespace lies on that td (`tdTransferRef`); a namespace that no
  longer points at the plan's td is candidate changed, re-plan; that td by
  the plan's name, holding the plan's `td_id` (else `ABORTED`); a new td of
  another `size` is `FAILED_PRECONDITION` naming both sizes; repoint the
  namespace's td reference. Reply `ns_id`.
* **UpdateNamespaceSuspended** — locate the namespace; set `suspended`.
  Reply `ns_id`.

### Clones

The codes are those of `architecture.md`, Clones, and the ones the bullets
below name.

* **CreateClone** — validate the bounds of `architecture.md`, Clones
  (`validateCloneGeometry`); then a candidate unit (GW9), because the
  clone's clone meta group takes extents on DNs. Each round plans from one
  read-only `Snapshot` opened with `openSp`, so a stale token is `ABORTED`
  first; then `cloneSrcLayoutCheck`, in the planning read alone, since a
  pool's layout never changes: a source that is a transfer of this
  cluster names its pool by id, and the request's three layout numbers
  must be that pool's (`architecture.md`, Clones), judged before the
  checks below that take them as typed, `cloneIoSizeCheck` above all, so
  that a mistyped number hears its own refusal. Then the round runs every
  refusal of `cloneCreateChecks`, which the deciding STM runs again, in
  its order: the `MaxCloneCntPerSp` ceiling, checked
  ahead of the name; the name free; the destination td by name; a
  destination td that any `Clone.dst_td_id` names (walk `clone_name_list`, a
  draining clone included) refused; a destination td a transfer's origin
  namespace lies on (`tdTransferRef`, walking `xfer_name_list`) refused
  `FAILED_PRECONDITION` naming the transfer; then the group's plan
  (`planCloneGrp`) —
  `model.ValidateClusterConf` and `model.ValidateBdevConf` of the stored
  confs (GW11), the legs `legCntOf` gives the SP's stored conf, the layout
  `model.GroupBlocks` gives a group of `CloneMetaGrpExtCnt` extents (a
  failure there is `ABORTED`: every slice of the pool already has a meta
  group of the meta ladder's first rung laid out from the same write-once
  confs), and the bound of `architecture.md`, Clones, the destination's
  `common.CloneMetaBytes` at the SP's block size against the data blocks of
  one side, `FAILED_PRECONDITION` naming the largest destination one side
  holds; then `cloneIoSizeCheck`, `model.CloneOptimalIo` of the request's
  `src_stripe_size` and `src_slice_cnt` and the SP's stored stripe, slice
  count and block against `MaxNvmeTcpCmdBytes`, `FAILED_PRECONDITION`
  naming both products, the size, the pool's block and the bound
  (`architecture.md`, Clones). The planning read also walks
  `td_name_list` for a snapshot of
  the destination not yet `created` (`tdUncreatedSnapshots`), refused
  `FAILED_PRECONDITION` naming it, and records the pool's `sp_id` and
  revision; the deciding STM does not repeat that walk. Then the
  DN scan (`pickDns`) for that many legs of
  `CloneMetaGrpExtCnt` extents each, with no black list and no location
  excluded, as `architecture.md`, Per-operation allocation, places a clone
  meta group; too few is `RESOURCE_EXHAUSTED`. STM: resolve; token; the same
  checks again; the cluster id, the `sp_id`, the leg count or the pool's
  revision other than the plan's is candidate changed (`architecture.md`,
  Clones, says why the plan is held to the `sp_id` and the revision); a
  token-carrying request whose name resolves to a pool created again at
  the plan's revision passes the token check and re-plans like a
  token-less one. Then the DN ledger
  (`newDnLedger`) and every pick
  re-verified (`verifyPick`; a moved one is candidate changed);
  mint `clone_id`, then the group's `grp_id`, then per pick, in pick order,
  a `leg_id` and its `side_id` (GW15's order); per pick a `Leg` of one
  `Side` (`addr_port` and `nvme_tr_conf` from the DN, `cntlid_slot` the
  first of `cntlid_slot_list`, `provisioned` false) and the DN charged the
  group's extents and the side's pointer; put the `Clone` with its
  `dst_td_id` and its group (`meta_grp`); flush the DN ledger,
  which writes each charged DN once, moves its capacity key and bumps its
  `DnRev` once; append to `clone_name_list`; `BumpSpRev`. No cntlr and no
  CN is read or charged: the clone meta group is in no cntlr's footprint
  (`architecture.md`, Terminology and object model), and the clone's
  dm-clone keeps its metadata on it. Reply `clone_id`.
* **DeleteClone** — it LATCHES, and the sp worker drains (`dnv-worker.md`,
  The clone drain). Two-phase (AG4). Phase 1 STM (read-only): resolve,
  through `openSpRead` and so without the SP's `deleting` gate, which only
  phase 2 applies (GW6); the token, checked explicitly because phase 1 is
  the decision on the path below and `openSpRead` skips the check (a stale
  token must still ABORT ahead of the clone's `deleting` answer); the clone
  (`loadClone`); **if the clone's `deleting` is already true, return OK
  here** — with no writes, no bump and NO agent call. The short-circuit
  sits before the agent call and not in phase 2 for a reason that is not an
  optimization: after the latch the CN has torn the stack down, so
  `GetCntlrInfo` no longer reports the dm-clone and a hydration check would
  wedge every repeat delete in `FAILED_PRECONDITION` for ever. Otherwise,
  when `force` is false, also resolve the **primary** cntlr's `addr_port`
  and `cn_id` — an SP with no primary cntlr is `FAILED_PRECONDITION`: there
  is nobody to prove hydration with, and a promotion makes the retry succeed
  (`force` true skips the lookup
  entirely: no agent call follows, which is what lets force delete a clone
  whose cntlr is unreachable). Between the phases, `force` false calls
  `GetCntlrInfo` and judges hydration with `common.HydrationComplete` on the
  agent's latest observation of the primary cntlr, which that call answers
  with (`architecture.md`, Clones); incomplete hydration **or an
  unreachable agent** is `FAILED_PRECONDITION`. Phase 2 STM (deciding): full re-resolution and the token check (GW6 — any interleaved
  mutation bumped `SpRev`, so a token the request carried subsumes the
  staleness of phase 1; a token-less request gets the re-resolution only,
  AG4); `loadClone` again; a clone whose `clone_id` is not the one phase 1
  read — drained and created again under the name meanwhile, so the proof
  was of another — is `ABORTED`; if `deleting` became true since phase 1,
  return OK with no writes (the same rule, raced variant); else write
  nothing but the resume, the latch and the bump — `suspended` false on
  every namespace whose `td_id` is the clone's `dst_td_id`, as one
  `Subsystem` put per subsystem that actually changed and none for the rest
  (the destination namespaces resume, their data being local, the twin of
  `DeleteTransfer`'s finalize under Transfers), the `Clone` put with
  `deleting` true and every other field unchanged, and `BumpSpRev` — through
  `model.LatchClone`, the body the worker's latch of a whole copy stages
  too (`dnv-worker.md` CLD13), whose every refusal, a listed subsystem's key
  or the revision key gone, is `ABORTED`. Reply `clone_id`. A delete after
  the worker's latch is the no-op above, and after its drain `NOT_FOUND`.

  The latch does NOT delete the `Clone` key, does not touch a chunk key and
  does not shrink `clone_name_list`: the name must survive until the
  drain's last transaction, because `model.LoadSp` fetches clones by
  iterating it. The latch is one-way: no path writes `deleting` false back
  on a `Clone`. The resume rides the LATCH so that it and the fan-out
  exclusion arrive in one `SpRev` bump; deferred, the destination namespace
  would go dark for the whole drain. What a caller meets while the clone
  drains is `architecture.md`, Clones.
* **loadLiveClone** — the gate the two clone mutators that ADDRESS an
  existing clone open with: `UpdateCloneTrConf` and `AppendCloneBitmap`
  answer `FAILED_PRECONDITION` when `deleting` is true. (`CreateClone`
  addresses none and needs no gate: the surviving key
  and list entry keep it refused.) The append half is load-bearing, not
  cosmetic: a racing append could otherwise write a chunk key behind the
  drain, and the drain's final emptiness guard rests on "after the latch,
  no chunk key can ever appear again". `GetClone` and `DeleteClone`'s own
  phase 1 keep using plain `loadClone`. The eight repair calls that write,
  which change a clone meta group, its repair or one of its retired legs,
  refuse a deleting clone
  the same way, in their `model` op, and `CreateRepair`, `ForceStartGroup`
  and `UpdateGroupOperatorInCharge` first in their planning read
  (`refuseDeletingClone`; Repairs and retired legs).
* **GetClone** — one STM read. Reply the `Clone`.
* **UpdateCloneTrConf** — STM: resolve; token; the clone
  (`loadLiveClone`); replace `src_tr_conf_list`; `BumpSpRev`. Reply
  `clone_id`.
* **AppendCloneBitmap** — one chunk of the SOURCE bitmap, addressed by the
  PAIR (`src_slice_idx`, `bm_idx`); how a chunk is positioned and what an
  absent chunk or a short tail means is `architecture.md`, Bitmap push
  protocol. Validation: `bitmap` non-empty (`validateBitmap`), then a page
  of at most `CloneBmChunkBytes`, judged on this request alone (a page
  longer than a whole chunk fits nowhere, whatever is already stored), then
  its place (`clonePagePlace`): the request's `bm_idx` and `offset`, or the
  chunk and the offset its `src_start_block` falls on, refused beside
  either.
  STM:
  resolve; token; the clone (`loadLiveClone`, so an append to a latched
  clone is refused and the clone key is in the STM's read set either way);
  `src_slice_idx` below `src_slice_cnt` (the WHOLE slice bound —
  `CreateClone` already holds it to `MaxSliceCntPerSp`, so a second constant
  check here would judge nothing the geometry has not judged already);
  `bm_idx` below `MaxCloneBmCnt`; read the chunk at `CloneBitmapKey` of the
  pair — an absent key decodes to an empty bitmap, which is what makes
  "create if absent" fall out and is the same read the ceiling check needs;
  with `offset` present, an offset other than the stored length answers OK
  writing nothing when the same bytes stand at it and is
  `FAILED_PRECONDITION` otherwise (`architecture.md`, Clones), judged before
  the ceiling; the stored length plus the page's at most
  `CloneBmChunkBytes`, else
  `RESOURCE_EXHAUSTED` (GW7's ceiling-reached case: the constant bounds the
  STORED chunk, not one page); **append** the bytes to that chunk — the
  action of `architecture.md`, Clones, because the pages of ONE chunk
  concatenate into exactly the bytes that chunk holds (`architecture.md`,
  Bitmap push protocol), so a replace would keep only the last page and
  place its bits at the chunk's own offset, which `PushCloneBitmap` would
  then hand the primary as "never written" and the agent would discard
  regions the source really wrote. The `Clone` record is **not rewritten**:
  it carries no chunk count, and how many chunks a clone holds is how many
  `CloneBitmap` keys it has — which is what the drain and `PushCloneBitmap`
  read; `BumpSpRev`. Reply `clone_id`.

### Transfers

All pure etcd, the standard mutator shape; the codes are those of
`architecture.md`, Transfers. **CreateTransfer** refuses an `ori_nqn` that
breaks the NQN rules of `architecture.md`, Common validation, the
dnv-namespace one included, resolves the origin namespace per
`architecture.md`, Transfers, refuses an origin whose td another namespace
of the SP lies on, walking `nqn_list` (`tdOtherNs`), and one whose td a
clone's `dst_td_id` names, walking `clone_name_list` (`tdCloneRef`), both
`FAILED_PRECONDITION`, mints `xfer_id` and appends to `xfer_name_list`. **DeleteTransfer** with `force` false *finalizes*: it
additionally sets `suspended` true on the origin namespace in the same STM;
with `force` true it *aborts* (the origin untouched); both delete the
`Transfer` and its list entry and `BumpSpRev`. **GetTransfer** is one STM
read. **UpdateTransferHosts** rewrites `allowed_hosts` (the transfer's own —
no `CdcEntry` involvement). The replies carry `xfer_id`.

GW16. **Finalize skips a vanished origin.** `DeleteTransfer`'s finalize
skips an origin subsystem or `ns_idx` that no longer exists instead of
failing: the transfer is being deleted either way, and the RPC must stay
able to complete. The namespace RPCs refuse to remove a live transfer's
origin (`architecture.md`, Transfers), so no record this gateway writes
meets this; one changed outside the gateway, or left by a gateway without
that refusal, may.

### Migrations

The codes are those of `architecture.md`, Migrations.

* **CreateMigration** — the `MaxMigrCntPerSp` ceiling, the name free and
  the pool's block within the block's bound, right behind the token in each
  round's planning read (`migrPoolChecks`, GW9); locate
  `src_side_id` by a slice scan, and, only when no slice holds it, among the
  sides of the clones' clone meta groups, where a find is
  `FAILED_PRECONDITION` naming the clone (`migrSrcLocation`, in the plan and
  in the STM alike); a side of the bad leg of the group's repair, read through
  the group's `rep_name` (`grpRepair`), is refused `FAILED_PRECONDITION`, and
  its leg already holding two sides is refused; the destination's slot
  (`migrDstCntlidSlot`, GW18), in the planning read too; a candidate unit for one DN
  (`pickDns`, with the black list and the two tiers of `architecture.md`,
  Per-operation allocation, both over the group's DN set, its repair's new
  legs included (`grpDnAddrs`); the group's DN locations are read outside
  every transaction through `grpDnLocations`; each round reads the group
  afresh —
  hence the candidate extent count, the destination's metadata units
  (`migrMetaUnitsOf`, behind GW11's two gates), the black list and the
  locations — before it scans). The scan passes over a DN without room for
  those units (`architecture.md`, Metadata units), and a round left with no
  DN for that reason alone answers `RESOURCE_EXHAUSTED` saying no DN has
  metadata room. STM: resolve; token; the ceiling, the name and the block
  again (`migrPoolChecks`); re-verify the topology and the slot; the units
  computed again from what this STM reads; the gain check of
  `architecture.md`, Per-operation allocation — the group as this STM reads
  it, with its repair's new legs, holding a DN the round's read of it did
  not is candidate changed
  (GW9), which only a token-less request meets, since the new side's
  `SpRev` bump fails a sent token first; re-verify the capacity key and, on
  the DN's record, its room for the units (`verifyPick`; a DN that filled up
  since the scan is candidate changed); mint `migr_id` and `dst_side_id`;
  append the new `Side` to the leg per `architecture.md`, Migrations
  (`provisioned` false, the `cntlid_slot` of GW18); the destination DN's
  bookkeeping — the extents and the units (`chargeUnits`) — and one
  `BumpDnRev`; put the `Migration` with its `meta_units` and
  `cutover_pending` set, which only the worker's cutover clears
  (`architecture.md`, Migration); `BumpSpRev`. Reply `migr_id`.

GW18. **The destination side's slot.** A migration destination side takes
the first `cntlid_slot_list` entry that differs from the source side's
(`migrDstCntlidSlot`), the one slot constraint a side has
(`architecture.md`, cntlid slots); a list offering no second value is
`FAILED_PRECONDITION`, a standing property of the SP's own configuration
rather than a candidate problem.

* **FinishMigration** — two-phase like `DeleteClone`. Phase 1 (read-only)
  opens with `openSp`, as `CreateMigration`'s planning read does: resolve,
  the `deleting` gate included, then the token, so a deleting SP is
  `FAILED_PRECONDITION` and a stale token `ABORTED` ahead of the migration
  lookup and of any agent call; then the migration, refused
  `FAILED_PRECONDITION` while its `cutover_pending` is set, with `force` or
  without, before any agent call (`migrCutoverStarted`; `architecture.md`,
  Migrations); with `force` false, the **destination** side's DN. Between
  the phases, `force` false calls `GetSideInfo` and judges hydration with
  `common.HydrationComplete` from `migr_dst_info.dm_clone_info` of the agent's
  latest observation of the destination side, which that call answers with
  (`architecture.md`, Migrations); incomplete or unreachable is
  `FAILED_PRECONDITION`. Deciding STM: re-resolve and the
  token; the migration's `cutover_pending` again, since a cancel and a
  create under the same name between the phases leave a pending record
  there; the pool's and the migration's ids against phase 1's, `ABORTED`
  when either differs, whatever the token and `force`, since another
  migration can take the name between the phases, after a forced cancel,
  another caller's finish or the worker's own, and the agent call proved
  the copy of the first; apply the finish of `architecture.md`, Migrations, through
  `model.EndMigrationIn`, the write the sp worker's finish runs too
  (`dnv-worker.md` MD6): the destination side becomes the leg's side, the
  source side is removed, the source DN's bookkeeping is released with
  `BumpDnRev`, and the destination DN gets back the `meta_units` the
  `Migration` records — a units-only change, so that DN is written and
  re-indexed with no `BumpDnRev`; a record with no units leaves it unread;
  the `Migration` and its list entry are deleted, with the keys an older
  build kept the migration's bitmap under (`architecture.md`, Upgrade in
  lockstep); `BumpSpRev`. The write's errors are lost invariants,
  `ABORTED`. Reply `migr_id`.
* **CancelMigration** — STM: resolve; token; the migration; its
  `cutover_pending` clear and `force` false is `FAILED_PRECONDITION`
  (`migrCancelAllowed`; `architecture.md`, Migrations); the reverse of the
  create through `model.EndMigrationIn`, the write the sp worker's cancel
  runs too (`dnv-worker.md` MD6): drop the destination side, release the
  destination DN's extents and the `meta_units` the `Migration` records
  with one `BumpDnRev`, delete the `Migration` and its list entry, with
  the keys an older build kept the migration's bitmap under
  (`architecture.md`, Upgrade in lockstep); `BumpSpRev`. Reply `migr_id`.
* **GetMigration** — one STM read; the record as stored, `cutover_pending`
  included.

### Repairs and retired legs

The codes are those of `architecture.md`, Repairs and Retired legs. The eight
calls that write run the `model` repair operations as their deciding STMs
(Scope and placement), each behind a planning read in a read-only snapshot
opened with `openSp` — resolution, the `deleting` gate, then the token
(GW5, GW6) — whose scope hands the op the cluster id, the shard, the sp id
and, where the op takes one, the stored `ClusterConf` (`repairOpenSp`,
`repairCreateChecks` for the create, `forceStartChecks` for the forced
start and `operatorInChargeChecks` for the operator-in-charge flag); the op
makes every check again inside its own STM, the token's revision as its
expected revision ahead of every other read (GW6), save the checks of the
primary that the planning reads of `ForceStartGroup` and `CreateRepair`
alone make, and the checks of the primary's report, which no transaction
sees: `ForceStartGroup`'s of the group's row, and `CreateRepair`'s of the
bad leg's row, whose verdict the op takes as
`model.RepairNewLeg.IdentityRefused`. `model` raises every refusal as an
`ErrPrecondition`, which `mapModelErr` answers `FAILED_PRECONDITION` save
a stale token and candidate changed (GW7), so the other codes come from
the gateway's own checks — the pure checks (GW4), the planning read's
resolution and token, `CreateRepair`'s own planning checks and its scan,
and the own planning checks of `ForceStartGroup` and
`UpdateGroupOperatorInCharge` — and from the four reasons `model` exports
for them, `model.ReasonRepairExists`, `model.ReasonRepairNotFound`,
`model.ReasonRetiredLegNotFound` and `model.ReasonRetiredListFull`, which
a handler answers before `mapModelErr` (`modelReason`) with the code and
the words the gateway's own check of the same refusal gives.
`ForceStartGroup` and `CreateRepair` alone read an agent between their
planning read and their op (Agent calls).
The three reads and `InspectGroup` open with `openSpRead`, so a deleting
pool's repairs, retired legs and groups still answer.

* **CreateRepair** — the pure checks first (GW4): `rep_name` by the common
  name rule and without the reserved prefix, `common.RepNameReservedPrefix`,
  whose rule `architecture.md`, Common validation, owns (`validateRepName`),
  exactly one `bad_leg_id_list` entry, the selector. Then a candidate unit
  (GW9), each round of which makes the planning checks of
  `architecture.md`, Repairs, in its order, from its own snapshot
  (`repairCreateChecks`) behind `openSp`: the level gate of the request's
  name through `model.CreateRepairLevelGate` (`architecture.md`, Repairs);
  the group through `locateGrp`, a deleting clone through
  `refuseDeletingClone`, a bad leg with two sides in `errMigrRunning`'s
  words (`repairBadLegChecks`); the pool's primary (`primaryTarget`, none
  for a pool with none, a primary whose CN is missing `ABORTED`); and the
  new leg's checks (`repairNewLegChecks`), a retired leg through
  `retired_leg_id_list` (a listed key missing is `ABORTED`) and the
  retired-leg bound in `errRetiredListFull`'s words, whose refusal the
  round holds back. The round then reads the primary's latest report for
  the bad leg's row (`repairBadLegReason`, an agent call between the
  snapshot and the op, AG1) and judges the bad leg's health
  (`repairBadLegVerdict`, `architecture.md`, Repairs, A leg of another
  identity), and only then answers the new leg's refusal, so the order is
  the one `architecture.md`, Repairs, gives.
  A fresh new leg is then placed by `architecture.md`, Per-operation
  allocation, CreateRepair: the group's DN set (`grpDnAddrs`, the group
  having no repair) as the black list and the plan, their locations as
  tier 1 (`grpDnLocations`), the request's `dn_selector` filtering
  `pickDns` for one DN of the group's `ext_cnt`, and no metadata units.
  Last the call of `model.CreateRepair` with the group's
  parent, the bad leg and the new leg — the retired leg's id, or the pick
  and the plan — with the request's `force`, which the record keeps
  (`architecture.md`, Repair), and the verdict's identity reason
  (`model.RepairNewLeg.IdentityRefused`); the request's `force_undecided`
  is read by the verdict alone. Its candidate changed sends the round
  back (GW9) and
  whose `model.ReasonRepairExists`, `model.ReasonRetiredLegNotFound` and
  `model.ReasonRetiredListFull`, met by a write committed after the
  snapshot, are answered as the planning read answers them. Reply `rep_id`
  and `new_leg_id`.
* **FinishRepair** — the planning read, the stored `ClusterConf` gated there
  (`releaseConfGate`, GW11); then `model.FinishRepair` with the gateway's
  clock in unix seconds as the retired leg's `retired_epoch` and that conf.
  The planning read does not look the name up, so a name the pool does not list
  is the op's `model.ReasonRepairNotFound`, `NOT_FOUND`; a stage other than
  STARTED, which the message names, a deleting clone, and a worker's repair
  at the retired-leg bound whose group's retired legs all carry `keep`
  (`model.ReasonRetiredLegsKept`) are the op's `FAILED_PRECONDITION`. Reply `rep_id`, `retired_leg_id` and
  `given_back_leg_id`, as the op returns them (`architecture.md`, Repairs).
* **CancelRepair** — the planning read, the stored `ClusterConf` gated there
  (`releaseConfGate`, GW11); then `model.CancelRepair`, which gives a fresh
  new leg's side back through the drains' DN ledger, one `BumpDnRev` per DN,
  and a new leg taken from a retired leg back as that retired leg; the
  op's reasons answer as FinishRepair's do. Reply `rep_id`.
* **TryToAbortRepair** — the planning read; then `model.SetRepairAbort`,
  which answers a CREATED repair whose flag is already set with no write
  and no bump, and refuses a flag not yet set while the group's
  forced-start mark names the repair's new leg (`architecture.md`,
  Repairs); the op's reasons answer as FinishRepair's do. Reply `rep_id`.
* **ForceStartGroup** — two-phase (AG4). The pure checks first (GW4): the
  names, a `leg_id` that is set with `cancel` or zero without it, an
  `override_list` entry that names no refusal or a kind named twice
  (`validateOverrideList`), and an `override_list` set with `cancel`, each
  `INVALID_ARGUMENT`.
  Then the planning read (`forceStartChecks`) behind `openSp` makes the
  refusals of `architecture.md`, Repairs, in its order: the level gate at
  `SP_LEVEL_NO_REDUND`, a `RedundNone` pool, the group through
  `locateGrp`, a deleting clone through `refuseDeletingClone`, the leg
  through the group's `leg_list` and the repair it names (`grpRepair`;
  `forceStartLegChecks`), a new leg the repair took back that carries the
  partial mark refused unless `override_list` names `PARTIAL`
  (`errPartialLeg`), and the primary, whose `CnConf` it reads for the
  agent call, a primary's CN that is missing being `ABORTED` as in
  `DeleteClone`. Outside any STM, `checkGrpArrayDown` reads the
  primary's report (Agent calls). Last `model.ForceStartGroup` with the
  group's parent, the leg, the cancel flag and `override_list`, which
  writes the mark, the list in it as named and no force, with the revision
  its own bump commits, and
  re-checks the token, the pool, the
  group and the leg in its STM. The gateway makes no refusal of a named
  leg's bad-block list: the primary reads the leg's superblock at the start
  and refuses a list unless the mark passes `BAD_BLOCKS` (`cnagent.md`
  CN12), as it refuses an older named leg unless the mark passes
  `OLDER_LEG`. Its `model.ReasonLegNotNamable`, a leg a
  write took away after the planning read, and its
  `model.ReasonLegPartial`, a leg a write marked partial since, are
  answered `FAILED_PRECONDITION` in the gateway's words (`modelReason`). A cancel
  stops the planning read after the clone, reads no agent, and the op
  answers a group with no mark with no write and no bump. Reply `grp_id`
  and `revision` as the op returns it.
* **UpdateGroupOperatorInCharge** — the pure checks first (GW4): the names.
  Then the planning read (`operatorInChargeChecks`) behind `openSp` makes
  the refusals of `architecture.md`, Repairs, in its order, with no level
  gate: a `RedundNone` pool, the group through `locateGrp`, a deleting
  clone through `refuseDeletingClone`. Last `model.SetGrpOperatorInCharge`
  with the group's parent and the request's value, which re-checks the
  token, the pool, the group and its clone in its STM, answers a group whose
  flag already holds the value with no write, and bumps `SpRev` for a
  write (GW6). Reply `grp_id`.
* **GetRepair** — one snapshot: resolve, then the `Repair` through
  `repair_name_list` (`loadListedRepair`: a name not listed is `NOT_FOUND`, a
  listed key missing `ABORTED`). Reply `rep_name` and the record as stored.
* **ListRepairs** — one snapshot: resolve, then every `Repair` the list
  names, each as GetRepair reads it. Reply `name_to_repair`.
* **ListRetiredLegs** — ListRepairs over `retired_leg_id_list`
  (`loadListedRetiredLeg`). Reply `leg_id_to_retired_leg`.
* **DeleteRetiredLeg** — CancelRepair's shape: the planning read with the
  conf gate, then `model.DeleteRetiredLeg`, which gives the side back through
  the drains' DN ledger with one `BumpDnRev`;
  `model.ReasonRetiredLegNotFound` is `NOT_FOUND`. Reply `leg_id`.
* **InspectGroup** — read-only. The pure checks (GW4): the names,
  `override_list` as ForceStartGroup checks it, and an `override_list` set
  with no leg, which only a dry run reads, each `INVALID_ARGUMENT`. One
  snapshot behind `openSpRead`, with no token and no deleting gate, makes
  the refusals of `architecture.md`, Repairs, in its order: a `RedundNone`
  pool, the group through `locateGrp`; then reads the group's legs, its
  open repair's new legs (`grpRepair`), its retired legs and the pool's
  `SpRev`; with a leg in the request, it makes `ForceStartGroup`'s
  planning refusals of a write from that leg — the pool being deleted,
  the level, a clone being deleted, `forceStartLegChecks` — keeping the
  first for the reply instead of failing the call, and its refusal of a
  partial leg, which the override `PARTIAL` passes, as a listed refusal
  (`forceStartRefusal`); and it finds the primary and its CN (`grpPrimaryTarget`), a pool with
  none being `FAILED_PRECONDITION`. Then one connection to the primary's
  agent: `GetCntlrInfo`, for each leg's row, the report's stamp and the
  revision of the request the agent holds, and `InspectGrp` (`cnagent.md`
  CN31), with the request's leg and `override_list`; a transport failure or an
  agent's error is `ABORTED`. Reply each leg of the records with the
  agent's read of it, or with no read when the agent's held request does
  not name it yet, its disk node and health epochs and its row; the
  array's state; the retired legs' ids; the stamp; both revisions; and
  the dry run (`inspectDryRun`): the first refusal `ForceStartGroup` would
  answer — the first planning refusal, the partial leg when the request's
  `override_list` does not name `PARTIAL`, then its judgement of the group's row
  (`grpRowRefusal`) — in the gateway's own words with no zeroes, update or
  stop, else the agent's dry run as the agent wrote it; and either way the
  list of refusals an override passes, the partial leg's first, its text
  cut to the row text cap as the agent cuts its own (`common.CutRowText`;
  `architecture.md`, Live-state reporting), then the agent's. Nothing is
  written.
* **UpdateRetiredLegKeep** — DeleteRetiredLeg's planning read with no conf
  gate, then `model.SetRetiredLegKeep` with the request's value, which
  re-checks the token, the pool, the leg and its group and clone in its
  STM, answers a leg whose flag already holds the value with no write, and
  bumps no `SpRev` for a write (GW6); `model.ReasonRetiredLegNotFound` is
  `NOT_FOUND`. Reply `leg_id`.

### Bitmap reads

`GetThinDeviceBitmap` is read-only and two-phase: one STM resolves, then
the agent calls, the report of a park when a namespace exports the td and
the read. It addresses the SP through its **primary** cntlr
(`openBitmapTarget`), so an SP with none is `FAILED_PRECONDITION`: there is
no controller to ask, and a promotion makes the retry succeed, which is
what a precondition means. The codes are those of `architecture.md`,
Bitmap reads, and the ones the bullet below names. A leg's bitmap has no
gateway call: the worker reads it from the agent (`dnv-worker.md` BM7).

* **GetThinDeviceBitmap** — the reply echoes the request's `slice_idx` and
  `start_block`. STM: resolve; the td by name to its `td_id`;
  the **primary** cntlr to its `addr_port` and, through `CnConfKey`, its
  `cn_id`; `slice_idx` within the SP's slice count, else
  `INVALID_ARGUMENT`; the window (`checkBitmapWindow`): a `start_block`
  that is not a multiple of 8 or lies past the slice's part of the td
  (`size` over the slice count and the pool's `data_block_size`), a
  `block_cnt` past what is left of it, and a `block_cnt` that is not a
  multiple of 8, save zero and a window ending at that part's end, are
  `INVALID_ARGUMENT`; a clone whose `dst_td_id` is the td's
  (`tdCloneRef`) is `FAILED_PRECONDITION` naming it; the namespaces of the
  SP whose `td_id` is the td's (`volumeNamespaces`), one the desired state
  does not keep effectively suspended `FAILED_PRECONDITION`, a listed
  subsystem or transfer whose key is gone `ABORTED`. Then, when there is
  any, `GetCntlrInfo` of the primary (`checkVolumeParked`): each one's
  `ns_id_to_dm_linear` row must read `RES_STATUS_OK` with
  `RES_REASON_PARKED`, else `FAILED_PRECONDITION` naming the namespace, as
  is a reply with a non-zero code or a call that fails, as for
  `DeleteClone`'s proof: the read is not proven safe yet
  (`architecture.md`, Bitmap reads). Then
  `GetThinDeviceBm` with the cluster id, `cn_id`,
  `sp_id`, `cntlr_id`, `td_id`, `slice_idx`, `start_block` and `block_cnt`
  (a zero `block_cnt` reads to the end); a failure is `ABORTED`. Reply the
  bitmap **verbatim** — the wire convention is produced by the agent, and
  the gateway never touches bits (GW14).

## Agent calls

AG1. **Placement.** Agent calls happen outside STMs (`architecture.md`, STM
discipline); the matrix below places each one. A `DeleteClone` whose clone
is ALREADY `deleting` answers in phase 1 and makes no agent call at all
(Clones), and neither does a `ForceStartGroup` with `cancel` (Repairs and
retired legs), a `CreateRepair` in a pool with no primary, which judges the
bad leg on its record alone (`architecture.md`, Repairs, A leg of another
identity), nor `GetClusterStatus` for a pool being deleted, which for a pool
with no primary calls only the destinations of its migrations past their
cutover (GW20), nor a node delete with `force` (Disk nodes). The two pre-STM
size calls take the cluster id from a plain pre-read of `ClusterConf`, the
in-STM read staying authoritative; the status read's calls take it from its
own plain `ClusterConf` read, the read's first (GW20); the other calls
after resolution take it from the resolving transaction.

AG2. **Connection.** Per call (`withAgentConn`): a ctx derived from the
request's with `DefaultGatewayAgentTimeout` as its timeout, one plaintext
gRPC client connection to the agent's `addr_port` with
`common.GrpcUnaryClientInterceptor` and `common.GrpcStreamClientInterceptor`
chained and the message limit set, closed when the call returns. The
interceptors and the limit are mandatory on every dnv connection
(`grpc.md`, Wiring), and the interceptors forward the request's trace id
(`grpc.md` T3). The gateway dials per call and closes after the call: it
keeps no connection cache like the worker's (`dnv-worker.md` RW7). One
handler, `InspectGroup`, makes two calls on one dial, under the one
timeout of that dial (InspectGroup, below). `GetClusterStatus` bounds each
of its calls by `StatusAgentTimeout` instead, inside the same per-call
dial (GW20).

AG3. **Failure mapping.** A transport or non-OK status maps to the code the
RPC's specification names: `ABORTED` everywhere except the calls whose
answer a caller needs as proof, where an unreachable agent is
`FAILED_PRECONDITION`, since silence proves nothing: the `force` false
checks of `DeleteClone` and `FinishMigration` (hydration is not proven
done); the `force` false proof of the node deletes (the node is not proven
clean), which also refuses a reply with a non-zero code; the restore
check's `GetCnStore` (what the restore loses is not known), which also
refuses a reply with any code but zero and the unknown object of an agent
that holds no node of the cluster; `ForceStartGroup`'s check of the
group's array (the array is not proven down); and `GetThinDeviceBitmap`'s
check of the parks (the volume is not proven to take no writes), which
also refuses a reply with a non-zero code.
`CreateRepair`'s read of the bad leg's row maps no failure at all: an agent
that does not answer leaves the answer the leg's record gives, so a dead
leg's repair never waits on a silent primary (`architecture.md`, Repairs).
The agent-side `AgentReply.code` convention does not apply to `Get*Size`,
`Get*Info` or `Get*Bm` beyond what their replies define: `ForceStartGroup`'s
check reads the group's row whatever the reply's code, as `CreateRepair`
reads the bad leg's row, a reply with no info giving it no reason, and a `GetCntlrInfo`
reply with no info, as the `UnknownObject` refusal of an agent that does not
hold the cntlr carries (`cnagent.md` CN23), reads to it as a report with no
row for the group, which it refuses as it refuses an agent that does not
answer (`architecture.md`, Repairs). `GetClusterStatus` maps no failure
to a code either: an agent that fails or answers a code is a row of its
reply (GW20).

AG4. **Two-phase rule.** Any handler with a mid-flight agent call re-runs
**full resolution and the token check** in its deciding STM, save that the
ones whose deciding STM is a `model` op — `ForceStartGroup` and
`CreateRepair` — re-read the pool under the cluster id their planning
snapshot resolved and never read `ClusterConf` again (GW5). No facts from phase 1 are trusted in phase 2
except as hints; for a request that carries a token, the token check makes
any interleaved mutation visible as `ABORTED` ("stale revision"), which is
what keeps such a two-phase RPC exactly as safe as a one-STM RPC. A
**token-less** request keeps the re-resolution but not that visibility —
GW6 is presence-based, so an interleaved mutation it did not observe stays
invisible to it. That is the
AG4 half of the token-less cost `architecture.md`, Revision keys and the
sync fan-out, accepts. The node deletes, `DeleteClone` and
`FinishMigration` are the exception: their agent call proves a fact of one
object, so their deciding STM compares the ids it read, a node's cluster
id, node id and revision, a clone's id, a pool's and a migration's ids,
with the planning snapshot's whatever the token, and answers `ABORTED`
when one moved (Disk nodes; the call matrix below).

The complete call matrix:

| Gateway RPC | agent RPC | when |
|---|---|---|
| `CreateDiskNode` | `DiskNodeAgent.GetDnSize` | before the STM |
| `CreateControllerNode` | `ControllerNodeAgent.GetCnSize` | before the STM |
| `InspectDiskNode` | `GetDnInfo` | after the STM |
| `InspectControllerNode` | `GetCnInfo` | after the STM |
| `InspectCntlr` | `GetCntlrInfo` | after the STM |
| `InspectSide` | `GetSideInfo` | after the STM |
| `InspectStoragePool` | `GetCntlrInfo` | after the snapshot |
| `DeleteDiskNode` with `force` false | `GetDnInfo` | between the snapshot and the STM |
| `DeleteControllerNode` with `force` false | `GetCnInfo` | between the snapshot and the STM |
| `DeleteClone` with `force` false | `GetCntlrInfo` | between the STMs |
| `FinishMigration` with `force` false | `GetSideInfo` | between the STMs |
| `ForceStartGroup` without `cancel` | `GetCntlrInfo` | between the STMs |
| `CreateRepair`, when the pool has a primary | `GetCntlrInfo` | between the snapshot and the op, every round |
| `InspectGroup` | `GetCntlrInfo`, then `InspectGrp` | after the snapshot |
| `GetThinDeviceBitmap` | `GetCntlrInfo`, when a namespace exports the td, then `GetThinDeviceBm` | after the STM |
| `GetClusterStatus` | `GetCntlrInfo` of each pool's primary, and `GetSideInfo` of each migration's destination past its cutover | after each pool's snapshot |
| `CheckControllerNodeRestore` | `GetCnStore` | after the first snapshot, before the per-pool reads |

No other `service Gateway` RPC leaves etcd — the matrix is complete.

## cmd/dnv-gateway

CM1. **Shape.** `cmd/dnv-gateway/main.go` mirrors `cmd/dnv-worker/main.go`
and `cmd/dnv-cdc` structurally — cobra and viper, an environment prefix,
two-signal handling, a thin `main` — and it is the only binary needing both
the gRPC-server flags (from `cmd/dnv-agent`) and the etcd flag (from
`cmd/dnv-worker`). It has a package doc comment naming this spec; every
flag but `--version` is also a config key and an environment variable under
the prefix `DNV_GATEWAY_`; `main` executes `newRootCmd`; one root command, no
subcommands, cobra's own usage and error printing silenced, no positional
arguments; `addFlags` and `bindViper` (the flags bound through viper, the
environment prefix, a dash-to-underscore key replacer, automatic
environment lookup, an optional `--config` file); an I/O-free
`optionsFromViper`; and comma-splitting of list flags through the local
`splitList` idiom.

CM2. **Flags.** The gRPC flags are the agent command's network and address
pair, the etcd flag the worker command's endpoints, and
the optional config file is the one both carry; the gateway adds no flag of
its own beyond the `--version` and the `--log-level` every daemon answers
(`architecture.md`, Components: invocation reference; `log.md` R6), and
each flag keeps its sibling's default and required status.
Deliberately no etcd operation-timeout flag (`dnv-worker.md` EU5) and no
default gRPC port: the address is required exactly as the agent's is, and
no default-port constant exists.

CM3. **Startup.** `run` binds viper, reads the options, sets the
`--log-level`, mints a startup trace id (`common.WithTraceId` of a fresh
`common.NewTraceId`), watches SIGUSR1 and SIGUSR2 under it until the process
ends (`log.md` R13), derives a cancelable ctx, arms the two-signal
`watchSignals` pattern — a signal
channel that holds both signals; the first SIGINT or SIGTERM cancels the
ctx, and a second one exits with status 1 without a clean drain — builds
the client with `etcdutil.New` (a lazy dial: startup never fails on an
unreachable etcd), and returns `gateway.Run` of the ctx, the client and the
`gateway.Config`. `Run` closes the client (GW2), so `main` does not.
Logging setup: `common`'s `init` installs the JSON logger, and `run` sets
its log level (`log.md` R6).

## Log records

LG1. The gateway adds no bespoke logging for gRPC or etcd traffic: the gRPC
and etcd items of `log.md` R8 are fully covered by the interceptor chains
(every server and client request and reply, the trace id included;
`grpc.md`, L1 to L6) and by `etcdutil` (every get, put, delete and range,
once per STM attempt — duplicates on retry are expected and acceptable;
`log.md`, etcd). The OS-command, file, directory-listing,
modification-time-read, device-number-read, discard, detached-child and
TCP-dial items of `log.md` R8 are implemented inside `LimitedOsClient`,
which the gateway never uses.

LG2. The record names this component owns, all emitted through the ctx
forms, are normative: `gateway starting` and `gateway stopping` from
`gateway.Run`, with `etcd client close failed` between them when GW2's
deferred close of the client fails; `gateway serving` just before serving;
`signal received` and `second signal, exiting without a clean drain` from
`main`'s `watchSignals`; and `log level changed` of `log.md` R13.

LG3. Protobuf in any gateway-authored record goes through
`common.PbToLogValue` (`log.md` R10); `bytes` fields, the bitmaps, therefore
log as "<N bytes>" only.

## Integration test plan

**What the suite proves.** `integtest/gateway_test.sh` proves the real
`dnv-gateway` binary — three instances of it — against a real single-node
etcd and fake agents: (a) every RPC of `service Gateway` maintains exactly
the etcd state the handlers above specify, verified against ground truth
that never goes through the code under test; (b) parallel requests across
instances are correct — disjoint work all succeeds, contended work has
exactly one winner and no partial writes; (c) refusals of every class write
nothing, provably; (d) instances are stateless — one can be killed mid-load
and restarted with no cleanup and no corruption. Correctness only: the
suite asserts nothing about latency or throughput. No `dnv-worker` runs; the
suite plays the worker through `workerctl` wherever a gateway precondition
waits on a worker flip, a migration's cutover, a leg's health epoch or a
repair's stage move, or a latch needs the worker's drain. It runs on one
server as a plain user, because nothing it exercises needs root.

**What the Go cases prove.** The package's Go tests call the handlers as plain
methods against one real etcd per test binary (`dnv-worker.md` EU7), with the
package's own fake agent (`startFakeAgent`) behind every node a handler calls,
and with `model` calls or planted writes in the worker's place where a
precondition waits on the worker. Beyond each handler's own rules, they hold,
through the real handlers, the count ceilings of `architecture.md`,
Cardinality limits, on a cluster's nodes and pools, a pool's cntlrs and named
objects, a subsystem's namespaces, a node's sides or cntlrs and a slice's
groups, and the size edges of
`architecture.md`, Size → extents and Group on-leg layout, and of a transport
service id: a ceiling is filled by real creates, the create that takes its
last place is accepted and the next is refused with nothing written, and a
size is accepted at its edge and, one past it, refused or, for a controller
node's budget, cut to it. The allowed-host list is held at its length through
every handler that takes one, and the rest of `architecture.md`, Common
validation, through the validator's own functions (GW4). The pool count is the
one exception to the real creates: the case that runs in every mode plants the
pool count for its race and its refusals, which keeps it fast enough for every
run; only outside `go test -short` do real creates fill the count in full, and
the next create is then refused. The id counters at the id step's last values,
and one below their top, are planted too, since no run of calls reaches
them in a test. Each of these
count ceilings is also raced at its last place by two token-less creates, the
second run to its end inside the first at the moment the first has read the
counted record — a log handler on the etcd read record (`volReadHook`) is the
seam — so the order that only the deciding STM's check can refuse is played on
every run (GW9). For a node's ceiling that check is the STM's re-check of its
pick: a node at its ceiling leaves the scan by the presence rule of
`architecture.md`, Capacity index keys, and a create that picked it before is
sent back to its scan.

**Topology.** The developer machine builds the binaries and drives one Linux
server over ssh, with no sudo anywhere. Everything runs on that server and
listens on its loopback, on a port block disjoint from every other suite:
one etcd, configured with the request caps every etcd serving dnv must have
(its transaction-op cap at `EtcdMaxTxnOps` and its request-size cap at
`EtcdMaxRequestBytes`, which the suite reads from the `workerctl` it has
just built instead of copying the numbers, as it reads `MaxSliceCntPerSp`
for the clone it creates at that ceiling and `MaxNvmeTcpCmdBytes` for that
clone's source stripe, the widest that keeps the clone's optimal IO size
within the bound, `DefaultHealthCheckInterval` for
the health intervals the smoke case expects in the stored `ClusterConf`,
`DefaultDmPoolDataBlockSize` and the `DefaultStripeSize` that block gives
for the block and stripe the smoke case expects in the stored cluster and
pool confs (its pools have one slice, so a pool's stripe is the
cluster's), a DN's metadata ceiling `DnCloneMetaUnitCnt` and its unit size
`DnCloneMetaUnit`, for the area the ceiling case fills, and the id step's
strides `IdStepStride` and `ThinIdStepStride`, which its smoke case checks
the moved counters against); three gateway instances on one launch line that
differs only in the gRPC address, each at `--log-level debug`, the log
level the suites run their dnv daemons at, save one dn agent in one case of
the dn suite (`dnagent_integtest.md`, Conventions, Log level), the tutorial
suite's, which runs the tutorial's start lines as written, and the e2e suite's
v0.1.0 programs (`e2e_integtest.md`, Topology and parameters); four fake DNs
and three fake CNs, the DNs in four distinct locations and the CNs in three of
them, each CN in its DN twin's, since the CN scan dedupes locations too and a
CN that shares a location with a DN shows the first preference of the pool
create's DN scans (`architecture.md`, Per-operation allocation), served by
`fakeagent`, the worker suite's fake (`dnv-worker.md`, Integration test plan);
and the two drivers, `gatewayctl` for the RPCs and `workerctl` for the etcd
ground truth, both run on the server. Every stage runs under a trace id of its
own, the thread that ties a script stage to the gateway, agent and etcd
records it caused; the suite asserts once that the id a stage sent reaches the
fake agent the gateway called.

**Cases.** The cases run in a fixed order, fail-fast, each against a store
wiped of every dnv key, with every gateway restarted and every fake reset,
so no state leaks between them; each creates its own cluster through the
gateway, never through `workerctl`.

* smoke (one gateway) — the full lifecycle: every RPC's happy path but
  `ListStoragePools`, which the parallel and restart cases run, and
  `WithdrawGrow`, which the gateway's Go tests and the dnvctl suite's sweep
  drive, each
  mutation's exact write set read back, the first pool's one leg in its
  first primary's location a leg of its data group, never of its meta
  group, the `CdcEntry` the cdc serves
  included — its transport list read back against the listing rule of
  `architecture.md` [D18] as `CreateSubsystem` writes it and as
  `UpdateSubsystemHosts` sets it, once more while a cntlr is disabled,
  which a list of every cntlr would fail, but only the list's length as
  the cntlr mutators change it, and the entry gone with its subsystem;
  every defaultable member of the stored confs resolved on the write path
  but `md_check_conf` and `start_hold_conf`, which the Go cases pin;
  paging with its empty last page and a malformed token; the inspects passing
  the agent's report and revision through verbatim, a revision the pool's rev
  key has passed, the pool's space read turning a thin pool row the fake
  reports into the pool's sizes and use beside the records' data space,
  thin devices and footprint, and the bitmap read passing its bitmap
  through, which the fake replies empty, having no setting for its bytes, so the case proves the
  reply's shape and not that the bytes cross unchanged (GW14), refused with
  nothing written for a td whose namespace the desired state does not keep
  suspended, for one whose suspended namespace the primary reports serving,
  not parked, which a row lever then reports parked for the read to pass,
  and for a window off a multiple of 8 blocks, and no suite
  compares the bytes with the agent's: the e2e suite reads a non-empty thin
  bitmap from the real agents and its bit for the block its setup wrote
  (`e2e_integtest.md`, The cases); the worker flip the snapshot
  precondition waits for; a migration's `cutover_pending` set
  at its create, a cancel while it is pending, and a finish refused while
  it is pending, with `force` and without, before the suite plays the
  destination's provisioned flip and the cutover; a clone of
  `MaxSliceCntPerSp` source slices of the largest stripe a source may
  have, whose dm-clone would report an optimal IO size over
  `MaxNvmeTcpCmdBytes`, refused with nothing written; the clone's
  pair-addressed chunks, a page placed at the position it names, sent again
  with no write and refused at another position, past the end and inside
  the stored bytes, its latch with the destination
  namespace resumed,
  its repeat delete and its refusals while latched, then its drain; the
  clone's clone meta group, as many legs of `CloneMetaGrpExtCnt` extents
  as the pool's groups have, each on a DN of its own, charged to those DNs
  at the create and given back by the drain, extents, pointers and capacity
  keys alike, with one `DnRev` bump each time, and no CN's free extents,
  cntlr pointers, capacity key or revision moved by the clone; the
  metadata units of `architecture.md`, Metadata units — a new DN's none,
  each migration's on its destination DN, given back by the cancel and the
  finish, and none for a repair's new leg or a clone — each read
  on the DN's record and its capacity value alike, a units-only write
  moving no revision; a
  repair's whole life (`architecture.md`, Repairs, Retired legs, Repair) —
  its create refused while the bad leg is healthy, then created with a
  fresh new leg off the group's DNs, read back by the get and the list,
  aborted, an abort repeated with no write, moved to ABORTED, its finish
  refused for the stage, and cancelled, its new side given back; a second
  one moved to STARTED, its abort refused for the stage, and finished, the
  new leg in the bad leg's place and the bad leg retired with its side kept
  on its DN, the reply naming no retired leg given back; the retired leg
  listed, kept with no revision moved, taken back as a third repair's new
  leg with nothing allocated and its keep remembered, and given back as it
  was, kept, by that repair's cancel, its keep then cleared, then deleted,
  its side given back; a group's forced start (`architecture.md`, Repairs, Repair) —
  refused for a leg sent with the cancel flag, an unknown group, a leg of
  another group, the level that stops group arrays and an array the
  primary's report, seeded in the fake, shows running, then, with the
  report showing the array broken, written with the revision its own bump
  committed, replaced, replaced again passing two kinds of refusal, which
  the mark stores as named and with no force, refused with nothing written
  for an override that names no refusal, a kind named twice and an override
  beside a cancel, and cancelled, a cancel of no mark writing nothing;
  a repair of a leg with no health epoch (`architecture.md`, Repairs, A
  leg of another identity) — refused while the primary's report names no
  identity on its row, created with no flag once the report marks it of
  another identity, and, marked of an undecided identity, refused without
  `force_undecided` and created with it, neither keeping a force and
  each marked to retire its bad leg with keep, each
  aborted and cancelled with every disk node given back what it took;
  a group's operator-in-charge flag (`architecture.md`, Repairs) — set,
  read back on the group's record and through the gateway's own read of
  the pool, and cleared, at the level that stops group arrays too, each
  write bumping `SpRev`, a repeated set and a repeated clear writing nothing, and
  refused for an unknown group, an unknown pool and a stale token;
  and the sp latch with the refusals it brings, then its drain, after
  which every extent, metadata unit and capacity key is back; then the
  node deletes without `force`, each fake first given the answer of an
  agent that has applied and swept its node's emptied state, since no
  worker runs to send a fake a syncup; and no dnv key is left. Before the
  case tears its
  pool down, the id step runs on the live pool: every counter moves by its
  stride, in one store revision for the globals and one for the pool, no rev
  key moves, and a thin device created after it draws its ids from the moved
  counters, above every old one. Then the restore check runs on the store
  the id step left, over the primary's controller node, whose fake is
  given what an agent would hold: the pool at its stored revision reads
  the same; one revision ahead over the stored layout, the pool holding
  no open repair and no retired leg, which the planted held layout leaves
  out, holding a thin
  device the store lacks at the pool's next thin id and not one the store
  has, the pool reads older, the two layouts counting the same sides with
  no retired side id missing, the
  lost thin device listed with its thin id marked as one that may return
  and the other listed as coming back, beside a pool the store lacks, read
  as new with its NQN; nothing is written.
* parallel (three gateways, disjoint resources) — waves of concurrent
  creates and deletes across all three instances all succeed, the minted
  ids forming the expected sets and the accounting exact: every free count
  matching its capacity key and every pool's legs on distinct DNs; the
  concurrent sp latches are then drained one by one.
* contention (three gateways, barrier races) — same-name creates and
  same-token mutations race: exactly one winner, and the losers burn no id
  and write nothing; the token works as a fence — a stale token and a
  present zero token are refused — and, through the documented client retry
  on "stale revision", as a liveness loop that converges.
* faults (one gateway) — first, the capacity read's space answer for the
  shape of the case's pool, before the pool is made and after it
  (`architecture.md`, Clusters): the disk side's share as the limit, the
  largest first data group, and after the create the pool's legs charged to
  the disk nodes and its footprint to each cntlr's controller node; then the
  refusal batteries on a live pool: validation, not-found and precondition
  classes — among them an extent size off its unit and a data block over the
  block's bound at a cluster's and a pool's create, the NSID that names
  every namespace, a clone source's namespace id of zero, a node's `tr_type`
  of "TCP" and `adr_fam` of "IPv4", each refused before any read or agent
  call, a namespace moved to a thin device of another size, a migration
  re-sent under a taken name, which hears `ALREADY_EXISTS` ahead of its
  leg's two sides, and a finish of a migration whose cutover is pending,
  refused with `force` and without —, each refusal provably writing nothing;
  the primary threshold's floor of `architecture.md`, Common validation,
  from both sides — the validation battery refuses a pool whose primary
  threshold is one second under twice the cluster's stored `cntlr_interval`,
  and the same request at exactly twice it is accepted with its threshold
  stored as sent, the pool then deleted and drained so that every node gets
  back what its create took; agent faults — a hung agent bounded by the
  agent-call budget, a closed port refused fast, inspects of hung and
  stopped fakes; the hydration checks of `DeleteClone` and `FinishMigration`
  refusing on incomplete hydration and on an unreachable agent, `force`
  skipping them; the node deletes' proof, on a disk node and a controller
  node made for it late in the case, refusing an agent that holds nothing of
  the node, which answers it as unknown, an agent behind the node, a
  leftover the agent names, a side record the disk's table still holds and a
  stopped agent, and `force` deleting the node, while the precondition
  battery refuses with `force` too a disk node that still hosts a side; and
  the status read over planted faults (GW20) — one row per worker role,
  since no worker runs, and none of the clean pool; a leg whose health epoch
  is set and whose row the primary reads ERROR, listed as one row; a thin
  pool that needs a check; a thin pool out of data space; a primary that
  hangs, listed as one UNKNOWN row within the status read's budget; pages of
  one pool, the cluster rows on the first alone; and, last, the pool's
  revision key deleted, listed as a lost record.
* restart (kill under load) — one gateway is killed with SIGKILL while it
  provably holds a create in its handler; every pool of the wave is then
  either complete or absent, never partial, and the accounting matches the
  survivors; the absent ones are re-driven through a survivor, since a
  failed reply may still be a committed write; the killed instance restarts
  on the same command line with no cleanup, and the store holds only the
  data's keys, nothing that describes a gateway.
* ceiling (one gateway) — a DN's metadata area of `architecture.md`,
  Metadata units, filled to its ceiling with real calls, and a clone at the
  bound of its clone meta group, on fake nodes that report sizes large
  enough for copies of many units and on pools at the smallest block size,
  where a copy costs the most units and a clone's metadata grows the most:
  a clone whose metadata one side of its clone meta group cannot hold is
  refused, the refusal naming the largest destination one side holds; a
  destination one region past that size is refused the same way, and the
  largest the pool's stripe allows below it is accepted, its clone meta
  group charged to its DNs as the smoke case's is and no CN charged; a
  cntlr created beside that clone charges its CN the pool's footprint
  alone; the clone's drain gives its DNs their extents back; a migration
  create whose one candidate DN is full is refused, and placed on the DN
  with room once a second candidate is allowed; then the cancel and the
  finish give every unit back. The refusal
  that only the scan's skip may answer has one candidate on purpose: with
  two, the random pick would land on the node with room either way. Every
  refusal writes nothing, every refusal of room says "metadata room", and
  every charge and release shows on the DN's record and its capacity value
  alike, a units-only one with no revision moved.

**Rules exercised.** The smoke case exercises GW4 to GW12, GW14 and GW19 across
every resource group, GW14's bitmap reads only on the empty bitmap the fake
replies (above), GW5's `deleting` gate on a latched pool, GW10's
paging, GW11's write-time resolution, AG1 and AG3 on the inspects and the
bitmap reads, AG1 on the restore check, the trace-id chain of GW2 and AG2,
and the listing rule of
`architecture.md` [D18] on the `CdcEntry` transport lists it reads back;
the parallel case GW9 — candidate units that lose a pick and run again,
invisibly — and GW12 under concurrency; the contention case GW6 as fence
and as liveness loop, GW8 and GW12 (a loser burns no id); the faults
case GW4, GW7 throughout, AG2's budget, AG3, of AG4 only that
`DeleteClone`, `FinishMigration` and the node deletes run their agent call
between the phases, GW20's rows, budget and pages, and the primary
threshold's floor of `architecture.md`, Common validation, from both
sides, with GW11's `event_threshold` stored as sent; the restart
case GW1, with the atomicity of one STM per RPC (GW8); and the ceiling
case GW7's metadata-room class and the scan skip of `architecture.md`,
Metadata units, whose charges and releases it shares with the smoke case,
each units-only write bumping no revision (GW6). The smoke case
sees only part of the listing rule. No worker runs, and the suite's rule
on `workerctl` writes (below) allows a leg's `err_epoch` alone, so every
cntlr's health epoch stays zero and the rule lists exactly the enabled
cntlrs: the case shows a disabled cntlr left out, but it cannot tell the
rule apart from a list of every enabled cntlr, since it never sees how
the rule treats a cntlr with a health epoch, a standby left out and a
primary kept. Left to the unit tests: `CreateCluster`'s hash-collision
guard, the token-decode internals of GW10, `GracefulStop` (GW3) and the
token-less path of GW6, which the driver never sends (below); the listing
rule for a cntlr with a health epoch, the transports the cntlr mutators
write, which the suite only counts, and the order of the list, which it
never checks; the primary threshold's floor for a threshold left to
its default, and where `CreateStoragePool` judges it, before its
candidate scans and again in its STM; GW9's round cap and the in-STM
re-check of a pick that filled up after its scan, which no stage can time
between a scan and its commit; AG4's re-check of a write that lands
between a two-phase RPC's agent call and its deciding STM, which no stage
times either; `CreateRepair`'s candidate-changed refusals
of a group that gained a DN or a pick that went stale; `CreateRepair`'s
answers to
the reasons `model` exports, which only a write between its planning read
and its op meets; a release that stops at zero; and GW19's refusal of a
counter that would wrap, its skip of a pool deleted under its range and its
transaction per pool at more pools than one transaction holds.

Out of scope: performance, latency and soak; real agents, `dnv-worker`,
`dnv-cdc` and `dnvctl`; a multi-member etcd; etcd outages (unit level, and
the one the end-to-end suite's cutoff case stages: `e2e_integtest.md`, The
cases); TLS and authentication, which dnv does not have. The real drains that
consume the two latches belong to the worker suite (`dnv-worker.md`,
Integration test plan).

**What a pass means.** Exit status zero and `PASS` mean every case's
assertions held, each against its own wiped store, in a run that stops at
the first failure. Every mutating stage asserts the call's expected code,
then the raw etcd state through `workerctl` — exact fields, never the
gateway's own reading — and then, where a read RPC covers the same state,
the gateway's reply as a secondary check. Every refusal stage brackets the
call with the etcd store revision, which etcd advances only for a
transaction that wrote, and asserts that it did not move; where a readable
statement of the same fact exists it is asserted too, and the faults case
also compares a whole pool snapshot before and after each refusal battery.
A bracket read that fails stops the run, because two failed reads would
compare equal. Waits are bounded polls for process readiness only: the
gateway is synchronous, so no stage ever sleeps waiting for etcd content.
After every successful pool mutation outside the race waves that moves the
pool's revision, a worker flip included, the script refreshes its token from
`GetStoragePool` and checks it against the pool's revision key in etcd, so
that every revision count made against the token rests on etcd, with the
gateway's reply as the second check, except where no checked token is sent
next: after a `DeleteStoragePool` latch it drains at once, after the sp
drain, which removes the pool, after the faults case's create of the second
pool its status read pages over, whose token no call sends, and after its
create of the threshold pool, whose delete sends the gateway's token alone;
the restart case refreshes the pools its race wave created only before a call
sends their token. Every pool token a call sends comes from the gateway's
reply; the parallel case's wave tokens, the tokens `race` reads again for a
retry and the threshold pool's delete token are not checked against etcd,
since a wrong one fails the next call. A disk node's or a controller node's
token is read from etcd; where a stage counts such a revision after a bump, it
also reads the gateway's `GetDiskNode` or `GetControllerNode` reply of it as
the second check. A crash fails the run: as each case ends, and in the reset
before each case, right before it truncates the logs, the suite reads every
log under its work directory, and a line that starts as Go's report of a crash
starts, `panic: ` or `fatal error: `, fails the run with the log and the line,
while a record that holds those words inside a field never matches; the
preflight proves the read on a sample log it plants on the server. A crash
while the cleanup at the end of a passing run stops a program is not read, as
that cleanup removes the logs with the programs.

**Cleanup.** Cleanup runs unconditionally at the start of every run —
before the server preflight, so a crashed earlier run cannot fail it — and
at the end only on success: a failing run leaves etcd's data, every log and
every behavior file in place and prints its diagnostics — the crash lines
of every log, the failing stage and its trace id, the tail of every process
log, a full dump of the dnv
keys and the listening ports — so the debris is what the developer reads;
a cleanup-only run does the cleanup alone. A run that SIGTERM, SIGINT or
SIGHUP stops before its exit handler exits non-zero and logs no PASS; the
handler's removals are best effort, as a signal while they run, a dead
terminal or tee or a full disk can end them early; after a stopped or failed
run, run the suite's `--cleanup-only`, as every lab chain does
(`e2e_integtest.md` E2E6). Cleanup signals every daemon the suite started —
etcd, the gateways and the fake agents — by its recorded pid with a pattern
kill of the programs under the work directory as a safety net, and removes the
suite's work directory and the crash scan's sample folder in `/var/tmp` by its
name; nothing else outside that directory is touched but for a process a stale
pid file names, as a pid is signalled with no check that it still runs the
suite's program, and the suite leaves no kernel state, no packages and no
users behind.

**The driver, `gatewayctl`.** `gatewayctl` is the gRPC driver of this
suite, in the style of the agent suites' drivers: the gateway serves
plaintext gRPC without reflection, so it speaks the generated
`pb.GatewayClient`, with one subcommand per RPC and the request fields as
flags. It runs on the server, prints exactly one JSON document per
invocation on stdout, and turns the expected outcome into its exit status,
so a wrong success and a wrong failure fail the stage alike. It always
sends a token message: an omitted revision is a present zero token, GW6's
always-stale probe. Its `race` subcommand is the barrier runner of the
parallel and contention cases: it prepares every job, releases them all at
once across the instances, can retry a job that fails "stale revision" by
re-reading its pool's token — the documented client protocol, exercised end
to end — and reports each job's code and attempt count, so a retry loop
that does not converge shows at its budget instead of as a timeout. `race`
measures; the script asserts. It dials without the client interceptors and
passes its trace id as plain metadata (`grpc.md`, Drivers and fakes). It
refuses a word left after a subcommand's flags, as dnvctl's leaves refuse a
positional argument (`dnvctl.md`, Conventions), since Go's flag package
would read `--force false` as `--force` and leave the flags after the word
unread: a boolean flag is written `--flag` or `--flag=false`.

**The etcd verification, `workerctl`.** Ground truth never goes through the
code under test: after every mutating stage the script reads raw decoded
etcd state through `workerctl`'s read-only subcommands, and the gateway's
own get, list and inspect replies must agree with it. `workerctl` also
answers the readiness probe of etcd and, on the developer machine before
anything is shipped, prints the Go constants the suite reads instead of
copying them (above), and, through its `meta-units` subcommand, the units
the formula of `architecture.md`, Metadata units, gives a copy, by which
the suite sizes its copies and checks the units the gateway stored. The
only writes it performs here are the worker-role ones, the suite playing
the worker: the flips `set-created` and `set-provisioned`, the cutover
`start-cutover`, a leg's health epoch `set-epoch` and the repair stage
moves `set-repair-stage`, at exactly the stages where a gateway
precondition or the status read waits on the worker — before every migration finish the
destination's provisioned flip and then the cutover, which runs the
worker's own operation (`dnv-worker.md` MD6), before a repair's create the
bad leg's health epoch, cleared again once no repair needs it, and before
its finish or its cancel the move to STARTED or to ABORTED, which runs the
worker's own stage move, and for the status read a leg's health epoch,
cleared again after it —, the
drains `drain-sp` and `drain-clone`, standing in for the sp coordinator
after a latch the gateway committed (`dnv-worker.md`, The sp drain and The
clone drain). A health epoch moves no revision, as the worker's own does
not (`architecture.md`, Revision keys and the sync fan-out). Every flip,
the cutover, every stage move and every drain step move `SpRev` as the
worker's own would: each bumps it, except
the sp drain's last step, which deletes the key, and a clone drain's
batch, which leaves it alone (`dnv-worker.md` CLD8); the script's token
bookkeeping counts them. One more write plants a fault no worker
makes: the faults case's status read deletes a pool's revision key with
`del-rev`, the lost record it must list, last in its case. Any other
`workerctl` write would test `workerctl` rather than the gateway and is
forbidden here; the store wipe
between cases and the store-revision probe of the refusal brackets go
through `etcdctl`. The drain stand-ins run `model`'s own drain ops in a
loop with no pass and no wake-up, and a step bound lets the worker suite
build a partly drained pool instead of racing one. The latch stand-ins,
`set-deleting` and `set-clone-deleting`, are the worker suite's, and
`set-clone-deleting` deliberately does not resume the destination
namespaces the way `DeleteClone` does: that write is the gateway's, and a
driver that re-implemented it would drift from it.
