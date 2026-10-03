# gateway.md — the dnv-gateway control-plane API server

This document owns the `dnv-gateway` binary: package `gateway`, the
stateless server of the `Gateway` gRPC service, with its serving and
lifecycle (GW1 to GW3), the handler pattern every RPC follows (GW4 to GW12
and GW14), the mechanism of each handler by resource group, the agent calls
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
`Inspect*` RPCs and behind the `force` false hydration checks of
`DeleteClone` and `FinishMigration`, and the `Get*Bm` bitmap reads (Agent
calls). It is one of the three co-located control-plane processes —
gateway, worker, cdc — that any number of control-plane servers may run.
This document covers `gateway/` and `cmd/dnv-gateway` only: `dnvctl` is a
separate component with its own document (`dnvctl.md`), and the integration
suite drives the gateway with a dedicated test client, `gatewayctl`, not
with `dnvctl`.

The STM bodies of the public RPCs live in the per-resource files of
`gateway/`. The three mutations `model` shares with the worker —
`GrowSlice`, `CreateSpareLeg` and `SwitchSpareLeg` — are reused, never
duplicated (`dnv-worker.md` MD8).

Files, as a guide to the package; an implementation may split differently
but keeps the package boundaries (`layout.md`, Directory tree) and the
dependency rule:

| file | holds |
|---|---|
| `gateway/server.go` | `Server`, `Run`, the gRPC server and its interceptor chains (GW1 to GW3) |
| `gateway/traceid.go` | the gateway's entry-point trace-id mint, `ensureTraceIdUnary` and `ensureTraceIdStream`, chained ahead of the shared pair of server interceptors (GW2) |
| `gateway/common.go` | resolution, the token check, error mapping, the candidate unit, id minting, paging and the agent connection (GW5 to GW7, GW9, GW10, GW12, AG2); and the `CdcEntry` maintenance the cntlr mutators and `UpdateSubsystemHosts` share (`eachCdcEntry`, `rebuildCdcEntry`) |
| `cluster.go`, `disknode.go`, `controllernode.go`, `storagepool.go`, `cntlr.go`, `thindevice.go`, `subsystem.go`, `clone.go`, `transfer.go`, `migration.go`, `spareleg.go`, `bitmap.go` | the handlers, one file per resource group of Handlers by resource group: `storagepool.go` holds `GrowSlice` too, `cntlr.go` holds `InspectCntlr` and `InspectSide`, and `subsystem.go` holds the subsystems, the namespaces and the subsystem RPCs' own `CdcEntry` writes, while the cntlr mutators rewrite the entries through `gateway/common.go` |
| `gateway/alloc.go` | the per-operation candidate compositions of `architecture.md`, Per-operation allocation, and the DN and CN ledgers |
| `gateway/validate.go` | the request validation of `architecture.md`, Common validation (GW4) |
| `cmd/dnv-gateway/main.go` | the command (CM1 to CM3) |
| `integtest/gatewayctl/` and `integtest/gateway_test.sh` | the integration suite's driver and script (Integration test plan) |

Dependency rule (`layout.md`, Dependency rules): `gateway` imports only
`common`, `pb`, `etcdutil` and `model` plus the gRPC runtime — it is a gRPC
server and a client, since it dials agents. `cmd/dnv-gateway` imports
`gateway`, `common` and `etcdutil` (it builds the client GW2 takes, CM3);
cobra and viper live in `cmd/` and `ctl/`, never in `gateway/`. `make build`
picks the binary up from `cmd/dnv-gateway/main.go` with no Makefile edit.

Out of scope: `dnvctl`; TLS and authentication (the whole of dnv is
plaintext gRPC); tracking the per-CN clone budget, which stays untracked
(Clones); an agent connection cache (AG2); and any gateway-side watch or
convergence logic, which is the worker's job.

## Constants, schema and model

The gateway's surface is `service Gateway` in `pb/schema.proto`. It adds no
etcd key kind: it writes only kinds of `architecture.md`, Key table, and
nothing that describes itself (GW1). The `model` exports it rests on are
stated beside the rules that call them — `SpNextId` (GW12), the revision
bump helpers and the expected revision of the three shared mutations (GW6),
the list-range prefixes (GW10), and the conf resolvers and stored-conf
validators (GW11); `model` itself is `dnv-worker.md`'s (MD1 to MD9).

### Additions to `common/constants.go`

Three constants are this document's, and `common/constants.go` is
authoritative for their comments.

`DefaultGatewayAgentTimeout` is the per-call budget, in seconds, of the
gateway's agent RPCs — `GetDnSize` and `GetCnSize`, the `Get*Info` behind
`Inspect*` and behind the `force` false checks of `DeleteClone` and
`FinishMigration`, and the `Get*Bm` bitmap reads — applied as a timeout
around each dial and call (AG2). It equals `DefaultEtcdOpTimeout`, so a hung
agent and a hung etcd bound an RPC alike.

`CloneBmChunkBytes` is the fixed capacity of ONE clone bitmap chunk, and the
quantum that positions it: chunk (s, b) holds the bytes of source slice s's
bitmap from b times `CloneBmChunkBytes` on, at most `CloneBmChunkBytes` of
them (Clones). Its size keeps a grown chunk value plus the rev-bump put of
one `AppendCloneBitmap` inside etcd's default request cap,
and every `PushCloneBitmap` message inside gRPC's default message cap. It is
a clone positioning quantum only — migration appends carry no byte cap.
`MaxCloneBmCnt` counts the chunks ONE source slice's bitmap may be split
into (a `bm_idx` is below it, Clones) and is NOT a bound on the source slice
count — that is `MaxSliceCntPerSp`, which `CreateClone`'s geometry check
already enforces.

`EtcdMaxTxnOps` is a deployment requirement, not a client setting: every
etcd serving dnv runs with `--max-txn-ops` at this value or higher, well
above etcd's own default. etcd caps a transaction on the longest of its
compare and operation lists, and the serializable-snapshot STM of
`etcdutil` compares every key it read AND every key it wrote, so that sum is
what has to fit. The transaction this number is SIZED by is
`CreateStoragePool` at its widest shape: `MaxSliceCntPerSp` slices of two
md-raid1 groups of `MaxAllocLegPerGrp` legs, each leg on a distinct DN, and
`MaxCntlrCntPerSp` cntlrs. `init_ext_cnt` moves extent counts, not key
counts, so every factor of that shape is a ceiling constant and the bound is
tripwirable: a unit test asserts the arithmetic from the named constants, and
another commits one widest-shape create against a real etcd started with
`--max-txn-ops` at `EtcdMaxTxnOps`. The headroom is thin in the DN dimension:
one more key read or written per DN costs one compare per DN of the widest
shape. Other bounded transactions exceed etcd's default too without sizing
this number: the sp drain's D2 batch (`dnv-worker.md` SPD13) and the
created flip's transaction (`dnv-worker.md` RW19). `DeleteThinDevice` stays
below all three — the create, the D2 batch and the flip — whatever the td
count: its deciding STM commits having read no td but its target, because
the walk for uncreated snapshots is a read-only plan outside it, verified by
the pool's identity and revision (Thin devices). The clone drain's batches
fit etcd's default and size nothing here (`dnv-worker.md` CLD11).

No other constant is this document's. `MaxAllocLegPerGrp` and
`MaxDelGrpPerTxn` are the sp drain's (`dnv-worker.md`, The sp drain) and
appear here through the transaction budget above, and `MaxAllocLegPerGrp`
also as the md-raid1 leg count of `legCntOf`; `DefaultSliceCntPerSp`,
the count substituted for a zero `slice_cnt`, is carried by
`architecture.md`, Storage pools, and only restated under Storage pools and
GrowSlice; `DefaultClusterName`, `ShardBucketSize`, the `Max*CntPerCluster`
ceilings, `MaxCloneBmCnt`, `MaxMigrBmCnt` and the bounds of
`architecture.md`, Common validation, are `architecture.md`'s.

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
etcd's. Two requests racing inside one instance and across two instances are
the same case by construction.

GW2. **Serving.** `gateway.Run` takes the etcd client and a `gateway.Config`
holding the gRPC network, the gRPC address and the etcd endpoints. `Run`
emits `gateway starting` (with the config) as its first record and
`gateway stopping` on the way out, closes the client as the last step of its
drain (so `main` does not, CM3), and serves exactly as `Serve` in
`agent/agent.go` does: it listens on the configured network and address;
builds the gRPC server from `serverOptions`, which chains the gateway's
entry-point trace-id mint, `ensureTraceIdUnary` and `ensureTraceIdStream`,
FIRST and the shared pair, `common.GrpcUnaryServerInterceptor` and
`common.GrpcStreamServerInterceptor`, behind it; registers the `Gateway`
service; starts a goroutine that turns the end of the ctx into
`GracefulStop`; emits an Info `gateway serving` record with `network` and
`address`; then serves. The gateway mints a trace id for a request that
arrived without one (it is one of the entry points of `grpc.md` T4): when
the incoming metadata carries no `trace_id`, the mint injects a fresh
`common.NewTraceId` into the incoming metadata, upstream of the shared
interceptors and outside `common/interceptor.go`, and they adopt it as they
adopt a client's and never mint themselves.

GW3. **Shutdown** is `GracefulStop`: in-flight handlers finish (each bounded
by its client deadline and the per-STM budget of `dnv-worker.md` EU5), new
requests are refused. There is nothing else to drain — no registry key to
delete, no background loop to stop.

Startup never fails because etcd is unreachable (`etcdutil.New` dials
lazily); only a configuration error fails `Run` before serving.

## The handler pattern

A handler is one gRPC method of `service Gateway`, implemented on
`gateway.Server`. Every handler has the same shape; the deviations of each
RPC are under Handlers by resource group.

GW4. **Request validation first.** `validate.go` implements the table of
`architecture.md`, Common validation, as pure functions with no I/O: string
sizes and patterns, the NQN rules, bounded numerics — a zero passes as
"give me the default", and for every member that ends up in a stored conf
substituting that default is GW11's job on the write path, never this one's
— an empty `bdev_feature_list`, and the `bdev_conf` geometry rules, which
`CreateCluster` judges once more on its resolved conf, still before any read
(Clusters). The list `count` is the one bounded numeric GW11 does not cover,
because it reaches no stored message at all: `pageLimit` refuses a value
above `MaxListCnt` (`INVALID_ARGUMENT`, never a silent cap) and turns a zero
into `DefaultListCnt` per request, and `validatePageArgs` runs it — with the
token decode — before the three cluster-scoped List handlers' `ClusterConf`
read; `ListClusters` reaches the same two checks through `pageNames` before
its range (GW10). Violations are `INVALID_ARGUMENT` before any etcd read.
State-dependent validation (a slot in use, an `ns_idx` taken, the geometry
rules on the `bdev_conf` `CreateStoragePool` merges over the cluster's, …)
happens inside the STM. `CreateStoragePool` judges that merged `bdev_conf`
before its STM as well, right after its plain pre-read of `ClusterConf` and
before its candidate scans (Storage pools and GrowSlice), so a request whose
merge breaks a rule is `INVALID_ARGUMENT` even on a cluster whose scans would
come up short (`RESOURCE_EXHAUSTED`).

GW5. **Resolution in-STM.** Resolution is the in-STM reads that turn
`cluster_name`, and `sp_name`, into the cluster id and the `SpConf`
(`architecture.md`, STM discipline); GW11's resolution of conf defaults is a
different operation, and the text says which it means. Except
`CreateCluster` and `ListClusters`, the STM's first read is
`model.ClusterConfKey` of the `cluster_name` (defaulted to
`common.DefaultClusterName`) — absent is `NOT_FOUND` — and the cluster id,
`model.ClusterId` of the name and the stored creation epoch, prefixes every
further key. `GrowSlice`, `CreateSpareLeg` and `SwitchSpareLeg` make that
read in their planning `Snapshot` only (Storage pools and GrowSlice, Spare
legs): their later transactions — the deciding STM, which is the `model` op
the sp worker shares, and `GrowSlice`'s CN-budget pre-check — take the
cluster id, and the `ClusterConf` where they use it, from that snapshot and
never read `ClusterConf` themselves. SP-scoped RPCs then read
`model.SpConfKey` of the cluster id and the `sp_name` — absent is
`NOT_FOUND`; mutators (except `DeleteStoragePool`) fail
`FAILED_PRECONDITION` when `SpConf.deleting` is set. `DeleteStoragePool` is
what SETS that flag (Storage pools and GrowSlice), so the gate is live from
the moment it commits until the drain removes the key. The **paged**
`List*` RPCs (clusters, disk nodes, controller nodes, storage pools) use
plain reads, not an STM (`architecture.md`, page_token): the three
cluster-scoped ones do one `Get` of `ClusterConf` for the cluster id and
then range; `ListClusters` ranges the `cluster_conf` prefix directly, there
being no cluster id to derive. `ListThinDevices`, `ListSubsystems` and the
single-object `Get*` RPCs are one-STM consistency reads (Thin devices,
Subsystems and namespaces, Clones).

GW6. **Token check, presence-based.** The token is the request's revision
token: its `DnRev`, `CnRev` or `SpRev` message (`architecture.md`, Revision
keys and the sync fan-out). Immediately after resolution and before any
other state check, a mutator reads its rev key (`SpRevKey` of the SP's
shard, the cluster id and the sp id, and likewise `DnRevKey` and
`CnRevKey`). It then asserts that the stored revision equals the token's
**only when the request carries the token message**; a mismatch is
`ABORTED` with the message "stale revision". A request that carries no token
message skips the comparison and proceeds.

Presence, not value, selects the mode. With the message **absent** the
mutator runs with no optimistic-concurrency gate, which is what omitting the
token asks for; everything else is unchanged: the rev key is still read (a
missing one is still `ABORTED`), the other preconditions still apply, and a
successful mutation still bumps. With the message **present** the comparison
is strict equality. Because a stored revision starts at one and only grows, a
message carrying a zero revision — or carrying only the echoed `addr_port`
or `sp_name` — can never match and is always `ABORTED`: that keeps the
deliberate always-stale probe available, and it is why presence and not the
zero value is the discriminator. Clients obtain tokens from the `Get*` RPCs,
and an operator who wants the gate sends one; omitting it is opting out, per
request. The cost — a token-less mutator can lose an update, and AG4's
two-phase safety argument does not cover it — is accepted: omitting the
token is opting out of the gate.

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
The echoed `addr_port` or `sp_name` inside the token message is ignored
(`architecture.md`, Revision keys and the sync fan-out).

The three `model` mutations the gateway shares with the worker —
`GrowSlice`, `CreateSpareLeg` and `SwitchSpareLeg` — take an expected
revision and re-check it first inside their own STM, zero skipping the
check (`dnv-worker.md` MD6); a mismatch is an `ErrPrecondition` with
`ReasonStaleRevision`. The gateway passes the token's revision, which is
zero exactly when the request carried no token message — and zero means
"skip" there just as absence does here, so the two layers agree by
construction; the worker's own calls pass zero. A token that is merely
*present* with a zero revision never reaches `model`: this check refuses it
first.

Every mutation that changes agent-visible desired state bumps the matching
revision exactly once in the same STM, through `model.BumpSpRev`,
`model.BumpDnRev` or `model.BumpCnRev`: each reads the rev key — a missing
one is an error — and puts it back one revision higher with its handle,
`addr_port` or `sp_name`, preserved, a rewrite and never a delete and
recreate. The `Update*Disabled` RPCs and the capacity and `err_epoch`
maintenance never bump (`architecture.md`, Revision keys and the sync
fan-out). A flag update that asks for the state the record already holds —
`UpdateDiskNodeDisabled`, `UpdateControllerNodeDisabled`,
`UpdateCntlrEnabled` — writes nothing, bumps nothing and replies OK, a token
the request carried still being checked first: a no-op that bumped would
invalidate every client's token for nothing.

GW7. **Error mapping.** This is the gateway's single error table. Handlers
return a gRPC status error straight from inside their STM closure:
`etcdutil.RunSTM` hands an error the closure returns back unchanged and
uncommitted (`dnv-worker.md` EU4).

* `INVALID_ARGUMENT`: a violation of `architecture.md`, Common validation, a
  malformed `page_token`, a bad enum or oneof.
* `NOT_FOUND`: the cluster, the SP, or a named or id-addressed object is
  absent.
* `ALREADY_EXISTS`: a create finds the name key (or, for `CreateCluster`, a
  global) present.
* `FAILED_PRECONDITION`: a documented public precondition fails, a
  `model.ErrPrecondition` with any reason but the two below included — the
  meta ladder cap among them, and `GrowSlice`'s `MaxGrpCntPerSlice` group
  ceiling: a count, but one nothing frees, since a live slice's groups are
  only appended.
* `RESOURCE_EXHAUSTED`: a global's `shard_bucket` sum at `Max*CntPerCluster`;
  the per-SP and per-group count ceilings — `cntlr_id_list` at
  `MaxCntlrCntPerSp`, `td_name_list` at `MaxTdCntPerSp`, `nqn_list` at
  `MaxSsCntPerSp`, a subsystem's `ns_list` at `MaxNsCntPerSs`,
  `clone_name_list` at `MaxCloneCntPerSp`, `xfer_name_list` at
  `MaxXferCntPerSp`, `migr_name_list` at `MaxMigrCntPerSp`, a group's
  `spare_leg_list` at `MaxSpareLegPerGrp` (`architecture.md`,
  `service Gateway` — RPC specifications); too few candidates
  (`architecture.md`, Per-operation allocation); `AppendMigrationBitmap`'s
  `bm_cnt` at `MaxMigrBmCnt`; `AppendCloneBitmap`'s stored chunk plus the
  new page above `CloneBmChunkBytes` — one chunk's ceiling reached by
  previous appends, the same shape (its four other refusals are an invalid
  request, `INVALID_ARGUMENT`: the empty `bitmap` of the first case, both
  index bounds — `src_slice_idx` at or above `src_slice_cnt` and `bm_idx` at
  or above `MaxCloneBmCnt`, checked in-STM — and the stateless page cap, a
  page longer than `CloneBmChunkBytes` judged on the request alone because a
  page longer than a whole chunk fits nowhere whatever is stored; all four
  under Clones); a cntlr's CN below a grow's extent count (`GrowSlice`'s
  pre-check); the ledgers' own `charge` shortfall (`dnLedger.charge` and
  `cnLedger.charge`, `gateway/alloc.go`) — defensive, since `verifyPick`
  answers the same shortfall as candidate-changed first.
* `ABORTED` with "stale revision": a token mismatch, and a
  `model.ErrPrecondition` with `ReasonStaleRevision`.
* `ABORTED`: everything of `architecture.md`, UNEXPECTED_ERROR → `ABORTED` —
  STM-client, conflict-budget, etcd and proto errors; a stored conf that is
  not concrete (GW11; the message is `model`'s, beginning
  "invalid stored conf: "); an agent gRPC failure where the RPC says so.

`model.ErrNotFound` maps to `NOT_FOUND`, though none of the three shared
mutations (GW6) returns it: when the SP its handler resolved is gone by the time
the mutation's own STM runs, `checkSpRev` or `loadSpConfForOp` refuses with
an `ErrPrecondition`, so the client sees `FAILED_PRECONDITION`. An
`ErrPrecondition` with reason "candidate changed" maps to nothing (GW9).

The dividing line: `RESOURCE_EXHAUSTED` is capacity or quota that could be
freed or extended (extents, candidates, count ceilings — though not
`GrowSlice`'s `MaxGrpCntPerSlice`, which nothing frees);
`FAILED_PRECONDITION` is the object's own state forbidding the operation,
that group ceiling included, since a live slice's groups are only appended.
A `GrowSlice` capacity shortfall sits on both sides, on either half of the
allocation. A CN-budget shortfall is `RESOURCE_EXHAUSTED` when `GrowSlice`'s
pre-check sees it, but `FAILED_PRECONDITION` when it only appears in the
deciding STM, where `model.chargeSpCns` can refuse solely as an
`ErrPrecondition` (Storage pools and GrowSlice). A leg DN short of free
extents is likewise `RESOURCE_EXHAUSTED` when the scan of
`architecture.md`, Per-operation allocation, skips it and the pick comes up
short (the "too few candidates" case above), but `FAILED_PRECONDITION`
(`model.checkDnPick`, reason "dn free_ext_cnt too low") when only the STM
sees it — that check precedes the capacity-key test, so it is not swallowed
by `ReasonCandidateChanged`. The same ordering means a pick whose DN was
**deleted or disabled** between scan and STM surfaces on the model paths
(`GrowSlice`, `CreateSpareLeg`) as `FAILED_PRECONDITION` ("dn not found" or
"dn not allocatable") rather than a GW9 re-scan; only the gateway-ledger
paths treat every vanished pick as candidate-changed and re-scan.

GW8. **One STM per RPC** (`architecture.md`, STM discipline). Everything
computable beforehand (name formatting, group plans, candidate lists, the
stamped `creation_epoch`) is prepared outside; all reads and writes commit
in one `RunSTM`. The closure MUST be a pure function of what it reads
through the STM (it is re-run on conflict); minted values that escape (ids,
generated uuids) are written to closure-captured variables that the closure
itself (re)assigns. The deciding STM is the one that commits the mutation;
for a two-phase RPC (AG4) it is the second one.

GW9. **Candidate unit retry.** Allocating RPCs (`CreateStoragePool`,
`GrowSlice`, `CreateCntlr`, `CreateMigration`, `CreateSpareLeg`) loop over a
candidate unit: scan candidates outside (`model.FindDnCandidatesAntiAffine`
or `model.FindCnCandidatesAntiAffine`, then `model.PickRandom`), then run the
STM, which re-reads each pick's exact capacity key (the bin index, free
extents and endpoint its `Cand` carries) and fails `ErrPrecondition`
"candidate changed" when one is gone — `CreateStoragePool`'s STM also when
its cluster id or leg count no longer matches the scan's (Storage pools and
GrowSlice), `CreateCntlr`'s when the SP, as it reads it, has a cntlr on a CN
the read its scan was planned from did not hold (Cntlrs and inspects),
`CreateMigration`'s when the group, as it reads it, has gained a DN since the
read its scan was planned from (Migrations). On that error — and only that
error — the handler re-scans and retries until the ctx ends (then
`ABORTED`); the reason is never surfaced to a client. `DeleteThinDevice` runs
the same loop without allocating: its scan is the plan's walk for uncreated
snapshots, and its STM fails candidate-changed when the SP it resolves is
not the one the plan walked or its `SpRev.revision` is no longer the one the
plan read — though a request carrying the token that finds the revision
moved fails GW6 first, `ABORTED` (Thin devices).

GW10. **Pagination** (`architecture.md`, page_token). The `page_token` is
the last returned key in base64 standard encoding; a decode failure is
`INVALID_ARGUMENT`; an empty token is the start of the prefix; a page
holds, in key order, the names whose keys sort **after** the decoded one, at
most `count` of them as GW4 resolves it; a reply whose page is not full
returns an empty token, which ends the listing. The prefixes are `model`'s
`ClusterConfPrefix`,
`DnConfPrefix`, `CnConfPrefix` and `SpConfPrefix`, the last three of the
cluster id; the returned names are the key suffixes after the prefix. Paging
is implemented in `gateway/` (`pageNames`): `model` carries no helper for it
beyond those prefix builders.

GW11. **Defaults resolved at WRITE time** (`architecture.md`, Common
validation). The rungs are a member from the request, else from the owning
`ClusterConf`, else from a constant of `common/constants.go` — and ALL of
them are applied by the RPC that writes the conf, so every stored conf
anything is formatted or addressed with is concrete in every DEFAULTABLE
member and nothing downstream substitutes (the `redund_conf` oneof is a
choice and not a default — unset still means `redund_none`
(`architecture.md`, Storage pools) — and the two exceptions to the rule,
`event_threshold` and the `dm_clone_conf` hydration pair, close it below).
`CreateCluster` stores `model.ResolveClusterConf` of its request, which
settles `bdev_conf`, `dn_bin_conf`, `alloc_conf` and `health_check_conf`
(Clusters); `CreateStoragePool` stores `model.ResolveBdevConf` of D-C's
merge (Storage pools and GrowSlice). A handler that then COMPUTES with a
stored member validates it first and REFUSES rather than guessing around a
zero: `model.ValidateClusterConf` in `CreateDiskNode` and
`CreateControllerNode` (before dividing a reported size by `extent_size`),
in `DeleteDiskNode` and `UpdateDiskNodeDisabled` (the last check before
their first write, because the one capacity key each moves is named by
shifting the STORED ladder), in `CreateStoragePool` (on its plain pre-read,
before the merge that gives its scans their leg count and that the geometry
rules of `architecture.md`, Common validation, judge, and again in its STM
before any group geometry) and the `GrowSlice` handler (before any group
geometry), in `newDnLedger` — the DN ledger `CreateStoragePool`,
`DeleteSpareLeg`, `CreateMigration`, `FinishMigration` and `CancelMigration`
build before staging their first write, for the same capacity-key reason —
and in the scans `pickDns` and `pickCn` (where a zero batch size would
silently make the scan width zero and turn every allocation into
`RESOURCE_EXHAUSTED`); `model.ValidateBdevConf` in `GrowSlice` and in
`CreateThinDevice` (before sizing against the stripe). Such a zero is a lost
invariant, not a bad request, so every one of those gates is GW7's `ABORTED`
and never `INVALID_ARGUMENT`. `model.GrowSlice` re-runs both validators
inside its own STM and can report that refusal only as an `ErrPrecondition`,
which `mapModelErr` renders `FAILED_PRECONDITION`; the handler's pre-check
above is what makes that a race rather than the ordinary path, the same
asymmetry GW7 records for `chargeSpCns`.

Resolving at use time has two costs, and they are why resolution happens on
the write path: a consumer that forgets to resolve computes with zeros in
silence — no reader can tell a member the user omitted from one the user
chose, and an SP's `bdev_conf` reaches the cn agent as stored — and a stored
zero pins geometry to whatever `common.Default*` the RUNNING binary carries,
so editing a constant would re-geometry live storage pools and strand every
capacity key already written under the old bin ladder. Two consequences an
implementer must keep: resolution runs AFTER the GW4 validation of the
request — on the raw request a zero still means "give me the default", so
resolving first would make every bound check a tautology — and AFTER the
member-wise merge in `CreateStoragePool`, never before it, because a
resolved request has no member left at the zero that inherits from the
cluster.

The resolvers and validators are `model`'s. `model.ResolveBdevConf` is the
write-time resolver `CreateStoragePool` stores through, and
`model.ResolveClusterConf` applies it to a `ClusterConf`'s own `bdev_conf`
too. The stored-conf validators `model.ValidateBdevConf` and
`model.ValidateClusterConf` begin every message with "invalid stored conf: "
and name the proto field. `ValidateBdevConf` checks the defaultable members
for PRESENCE only — the bitmap chunk count solely when the oneof did choose
md-raid1, a `redund_none` pool having no bitmap to size; the ranges of
`architecture.md`, Common validation, belong to the request, and a
`low_water_mark_pct` above 100 is the legal "auto-grow off" setting, never an
error. `ValidateClusterConf` adds `dn_bin_conf.extent_size` non-zero —
non-zero only, because a DN header may legitimately be formatted at an
unusual size — the shift ladder of `architecture.md`, DN bins, the two batch
sizes, the four intervals, and `ValidateBdevConf` of `bdev_conf` when the
cluster carries one. The ladder predicate is one unexported function shared
with `model.ResolveDnBinConf`, so resolver and validator cannot drift; the
all-zero shift set a `ClusterConf` written without a `dn_bin_conf` would
carry is therefore NOT valid stored state.

Two conf messages are resolved when they are READ instead, and both are
stored exactly as sent, because both are policy knobs rather than geometry —
nothing is formatted or addressed with either, and an operator reads back
what they asked for (`architecture.md`, Common validation, names the same
two). `event_threshold` is resolved member-wise by
`model.ResolveEventThreshold`. The `dm_clone_conf` hydration pair is the
other: `CreateClone` and `CreateMigration` store the request's message as it
arrived (`architecture.md`, Clones and Migrations), the sp worker fills a
migration's zeros in with their constants as it builds the side request
(`dnv-worker.md` RW15), and a zero in a clone's is simply never messaged to
the dm-clone target by the cn agent (`ensureHydrationKnobs`, which sends the
`hydration_threshold` and `hydration_batch_size` messages of `cnagent.md`
CN18 step 3, sends each only for a non-zero member), leaving the target's
own default in place (`architecture.md`, Common validation).

GW12. **Id minting.** Cluster-scoped ids follow `architecture.md`, Globals:
id allocation + shard buckets, inside the STM: read the global, take its
`next_id` as the id and advance it; the shard code is the index of the
smallest `shard_bucket` entry (the first on ties), and that entry is
incremented; the `Max*CntPerCluster` gate is the bucket sum before the
increment. Deletion decrements the bucket and never reuses an id. Per-SP ids
come from `model.SpNextId`, the per-SP id read: it returns
`SpConf.next_id` clamped to `SpFirstId` and mutates nothing, so every caller
advances and persists `next_id` itself, as the `model` ops and the gateway's
`spIdMinter` do. A thin device's `dev_id` comes from `SpConf.next_dev_id`,
advanced once per device (an `ori_id` of zero means no origin).

GW14. **D-J. Bitmaps are opaque.** Bitmap bytes cross the gateway VERBATIM
in both directions: `AppendCloneBitmap` and `AppendMigrationBitmap` store
what the request carries (Clones, Migrations — the push of those stored
chunks to the agents is the worker's, `dnv-worker.md` BM1 to BM6, not a
gateway RPC), and the bitmap reads reply the agent's bytes (Bitmap reads).
The wire convention — 1 means unmapped or never written, least significant
bit first — is produced and consumed by the agents (`architecture.md`, raid0
bitmap math: the single inversion at the agent boundary); the gateway never
inspects, converts or trims a bit, so it can never disagree with the agents
about the convention.

## Handlers by resource group

Each RPC gets its mechanism: the work before the STM, the STM outline in key
and helper terms, the bump and the reply. Semantics, exact error sentences
and field-level rules stay in the cited section of `architecture.md`,
`service Gateway` — RPC specifications; nothing below overrides them.

### Clusters

* **CreateCluster** — validate the confs (`architecture.md`, Common
  validation) on the RAW request, where a zero still asks for the default,
  and its `bdev_conf` once more resolved, where an omitted member meets the
  geometry rules as its constant (a refusal is `INVALID_ARGUMENT`, prefixed
  "bdev_conf with its defaults filled in:"). The `dn_bin_conf` shifts are
  not bounded one by one but judged as a set (`architecture.md`, DN bins),
  all-or-nothing: all four zero asks for the default ladder and is accepted;
  any other set that is not a strictly increasing ladder within the shift
  bounds is `INVALID_ARGUMENT`, because the stored ladder is what every
  capacity key of this cluster is written under for the cluster's whole life
  and an operator handed a silently different ladder has no RPC to correct
  it (`ClusterConf` is write-once). Stamp the creation epoch — the wall clock
  in nanoseconds — **once, outside** the STM (retries of this attempt reuse
  it; a client retry stamps anew, `architecture.md`, Clusters), and build the
  message to store outside it too, for the same reason: it is
  `model.ResolveClusterConf` of the request's conf members plus that epoch,
  so `bdev_conf`, `dn_bin_conf`, `alloc_conf` and `health_check_conf` land
  concrete and only `qos_ratio` and the epoch pass through as given (GW11;
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
* **ListClusters** — no cluster resolution (the one exception); a paged
  plain range over `ClusterConfPrefix` (GW10); the names are the key
  suffixes.

### Disk nodes

* **CreateDiskNode** — validate; **pre-STM agent call** (AG1):
  `DiskNodeAgent.GetDnSize` at `addr_port` (with a zero `dn_id`, for logging
  only) — a gRPC failure is `ABORTED`. STM: resolve; `DnConfKey` of the
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
* **DeleteDiskNode** — STM: resolve; the `DnConf` by address (`NOT_FOUND`);
  the token against the `DnRev` (GW6); a non-empty `side_ptr_list` is
  `FAILED_PRECONDITION`; `model.ValidateClusterConf` on the resolved
  `ClusterConf` (a zero is `ABORTED`, GW11 — the last check before the first
  write, since the capacity key to delete is named by the stored ladder);
  delete the `DnRev`, the `DnConf` and the capacity key (`MaintainDnCapacity`
  from the old record to none); decrement the node's entry of `DnGlobal`'s
  `shard_bucket`. Reply `dn_id`.
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
  call `GetDnInfo` with the cluster id and `dn_id`; an agent failure is
  `ABORTED`. Reply `applied_revision` and `dn_info`, both from the agent's
  reply (`architecture.md`, Disk nodes — deliberately the agent's applied
  revision, never the stored rev key).

### Controller nodes

The exact mirror of Disk nodes over `CnConf`, the CN capacity key, `CnRev`
and `CnGlobal`, with three differences: `CreateControllerNode` calls
`GetCnSize` and maps the reply per `architecture.md`, Size → extents (a zero
takes `DefaultCnCap`, a size above `MaxCnCap` is clamped to it, and a
non-zero one below `MinCnCap` is treated as zero); the delete's occupancy
precondition is `cntlr_ptr_list`; `InspectControllerNode` calls `GetCnInfo`.
The same six RPCs: create, delete, get, list, update-disabled and inspect.

### Storage pools and GrowSlice

* **CreateStoragePool** — validate: the `cntlid_slot_list` entries below
  `CnCntlidSlotCnt`, with no duplicates; `cntlr_cnt` between
  `MinCntlrCntPerSp` and `MaxCntlrCntPerSp` and at most the length of the
  slot list; `slice_cnt` at most `MaxSliceCntPerSp`; `init_ext_cnt` at
  least one; the confs per `architecture.md`, Common validation. A zero
  `cntlr_cnt` and a zero `slice_cnt` are requests for a default, not
  refusals: each is replaced — before the bound above it is judged — by
  `DefaultCntlrCntPerSp` and `DefaultSliceCntPerSp`, and it is the
  substituted count the plan and the STM's slice loop build the SP from
  (`architecture.md`, Storage pools, its defaults). A zero `init_ext_cnt` is
  still `INVALID_ARGUMENT`, and so is `CreateClone`'s zero `src_slice_cnt`
  (Clones): that one describes a source which already exists, so no default
  can stand in for it.

  Pre-STM plan (`planSpGroups`), in D-D's order: per slice the meta group
  (one extent) first, then the data group (`init_ext_cnt` extents) — extent
  counts only; `model.GroupBlocks` turns each into `meta_blocks` and
  `data_blocks` in the STM, where the conf it needs has been read and
  checked. Candidate unit (GW9): a plain pre-read of `ClusterConf`, gated by
  `model.ValidateClusterConf` as the scans gate it (a stored conf that fails
  it is `ABORTED`, GW11, before anything is computed from it), and over it
  the same merge and resolution the STM makes below, which gives the scans
  their leg count; `validateBdevConf` on that conf
  (`validateMergedBdevConf`), because the raw request was judged by the
  geometry rules only between the members it set (a refusal is
  `INVALID_ARGUMENT`, prefixed "bdev_conf merged over the cluster's:"), and
  before the scans, so that a cluster short of nodes does not answer a
  request that breaks a rule `RESOURCE_EXHAUSTED` first; then scan DNs per
  group with the growing black list of `architecture.md`, Per-operation
  allocation (the group's leg count — one for `RedundNone`,
  `MaxAllocLegPerGrp` for `RedundMdRaid1` (`legCntOf`) — drawn from
  `dn_batch_size` times that many candidates, a random pick, the picked DNs
  black-listed so every leg of the SP lands on a distinct DN) and CNs (the
  candidate extent count is the sum of `ext_cnt` over all groups;
  `cntlr_cnt` rounds, a random pick, black-listed, and the two tiers applied
  — tier 1 of every later round excludes the `location`s of the CNs already
  picked, tier 2 drops that exclusion when tier 1 finds no CN); too few at
  any point is `RESOURCE_EXHAUSTED`.

  STM, in this order: resolve; build the `bdev_conf` to store as
  `model.ResolveBdevConf` of `mergeSpBdevConf` of the request over the
  cluster's — merge first so an omitted member still inherits from the
  cluster, resolve second so what is stored is concrete (D-C, GW11) — and
  fail the unit right there, before a single other key is read, when this
  transaction's cluster id or that conf's leg count no longer matches the
  one the scan drew its picks for; `validateMergedBdevConf` on that conf
  once more (`ClusterConf` is write-once, so for an unchanged cluster id
  this repeats the verdict of the pre-read, but the in-STM read is the
  authoritative one); `SpConfKey` present is `ALREADY_EXISTS`; mint `sp_id`
  and the shard from `SpGlobal` (GW12), then every `cntlr_id` in pick order
  (D-D); `model.ValidateClusterConf` before the cluster's `extent_size` is
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

  **D-C.** The `bdev_conf` `CreateStoragePool` STORES is the member-wise
  merge of the request over `ClusterConf.bdev_conf`: a member wins unless
  left at the proto3 zero that means "unset"; the redundancy KIND is a
  oneof choice (the request's, else the cluster's, else `redund_none`), and
  a kind chosen by both merges member-wise inside. The merge then passes
  through `model.ResolveBdevConf`, which settles any member still zero on
  both sides against its constant, so all three rungs land in the stored
  message (GW11). That stored `bdev_conf` is never re-resolved at read time,
  and an SP's geometry is therefore immutable under later cluster-default
  edits — immutable only because those stored members are CONCRETE, since a
  stored zero would float with whatever constant the reading binary carries.

  **D-D.** `CreateStoragePool` plans, scans and mints in one fixed order —
  per slice the one-extent meta group, then the data group; the cntlr ids
  first, in pick order — so a retried candidate unit reproduces exactly the
  same write set.
* **DeleteStoragePool** — it LATCHES, and the sp worker drains
  (`dnv-worker.md`, The sp drain). STM: `openSpFlags` with the `deleting`
  gate off — resolve, read the `SpRev`, run GW6's token check; if `deleting`
  is ALREADY true return OK here, with **no writes and no bump** (a repeat
  delete must not invalidate every client's token to force a pointless
  re-resolve; the token check has already run, so a stale token still
  ABORTs first; `dnv-worker.md` SPD3); else all five name lists
  (`td_name_list`, `nqn_list`, `clone_name_list`, `xfer_name_list`,
  `migr_name_list`) empty, else `FAILED_PRECONDITION`; put the `SpConf` with
  `deleting` true; `BumpSpRev`. Reply `sp_id` — the repeat-delete no-op
  replies with it too, since the RPC resolved the SP before short-circuiting
  and `DeleteStoragePoolReply` carries nothing else. The emptiness check and
  the latch share the STM with the reads, so they are atomic, and once
  latched `resolveSp`'s `rejectDeleting` gate refuses every other mutator —
  no new child can appear after the check, ever (`dnv-worker.md` SPD4).
  Nothing else is written: the SP, its cntlrs, its slices and every extent
  they charge survive the reply, and partial teardown is a real, visible
  state (`architecture.md`, Storage pools). What a one-shot teardown would
  guarantee is not atomicity but AGREEMENT — DN and CN budgets never
  disagreeing with the keys that describe them — and every drain batch keeps
  it by releasing budget in the same transaction that shrinks the describing
  key. Consequences: `CreateStoragePool` keeps failing `ALREADY_EXISTS` on
  the surviving `sp_conf` key until the drain's last transaction, so name
  reuse resumes only then, and an observer polls `GetStoragePool` until
  `NOT_FOUND`.
* **GetStoragePool** — one STM: the `SpConf`, the `SpRev`, every listed
  `Cntlr`, every listed `Slice`; a missing listed key is `ABORTED`. Reply all
  of it (the `SpRev` is the token source).
* **ListStoragePools** — a paged plain range over `SpConfPrefix` of the
  cluster id (GW10).
* **UpdateStoragePoolCntlidSlotList** — STM: resolve; token; the validation
  of `architecture.md`, Storage pools (a duplicate, a value at or above
  `CnCntlidSlotCnt`, or removing a slot in use is `INVALID_ARGUMENT`);
  write; `BumpSpRev`. Reply `sp_id`.
* **UpdateStoragePoolLevel** — STM: resolve; token; set `sp_level`;
  `BumpSpRev`. Reply `sp_id`.
* **FindStoragePoolNames** — an `etcdutil` snapshot (one store revision, no
  STM): the `ClusterConf` to the cluster id, then per requested `sp_id` a
  read of `SpNameKey`; unknown ids are omitted, never an error. Reply the
  map.
* **GrowSlice** — validate the exclusivity of `architecture.md`, GrowSlice:
  a data grow (`is_meta` false) states a positive `ext_cnt`, a meta grow
  (`is_meta` true) states none, else `INVALID_ARGUMENT`. A `Snapshot`
  pre-read for planning, whose `ClusterConf` and SP `bdev_conf` are both
  validated before anything is sized from them (`model.ValidateClusterConf`,
  `model.ValidateBdevConf`; a failure is `ABORTED`, GW11) — which is also
  what keeps a false from `model.MetaLadderExtCnt` meaning the meta cap and
  nothing else, since an unvalidated zero `extent_size` would report the same
  false and reach the operator as a metadata ceiling. Before it plans, the
  handler answers the group ceiling of `architecture.md`, GrowSlice, from the
  same snapshot: a slice whose list of the requested kind already holds
  `MaxGrpCntPerSlice` groups (`model.GrpListFull`) is `FAILED_PRECONDITION`,
  GW7's object-state class, ahead of the scan, so a cluster short of DNs
  cannot report it as `RESOURCE_EXHAUSTED`; `model.GrowSlice` re-checks it
  in-STM. The plan itself is the slice's current meta total for
  `model.MetaLadderExtCnt` — the cap reached is `FAILED_PRECONDITION`, GW7's
  object-state class — and the request's own black list, which is the entire
  seed of the scan (D-F); then a second `Snapshot`, the CN-budget pre-check
  (`growSliceCnBudget`), over the SP's cntlrs and their `CnConf`s at one
  store revision — every cntlr stacks the new group, so every one of their
  CNs needs the new group's extent count free (the size D-E recomputes,
  never the request's `ext_cnt`), and a shortfall is the
  `RESOURCE_EXHAUSTED` of `architecture.md`, GrowSlice, named with the CN
  that is short; a cntlr or `CnConf` key gone under the snapshot is skipped,
  not raised, because this only gives the common case its documented code
  and the STM re-reads all of it; the candidate unit; then the call of
  `model.GrowSlice` with the token's revision as its expected revision
  (GW6), which re-validates in-STM and bumps the `SpRev`, the leg DNs' revs
  and — through `chargeSpCns`, which charges the CN of every cntlr — those
  CNs' `CnRev`s itself (`architecture.md`, GrowSlice). A CN that loses its
  budget in the window after the pre-check is therefore still refused, but
  by `chargeSpCns` as an `ErrPrecondition`, so the caller sees
  `FAILED_PRECONDITION` instead: the pre-check is deliberately the generous
  one, since only the STM decides. `ReasonStaleRevision` maps to `ABORTED`,
  any other `ErrPrecondition` to `FAILED_PRECONDITION`. Reply `slice_id` and
  `grp_id`.

  **D-B.** The gateway passes `model.GrowSlice` the largest possible pool
  total, `math.MaxUint64`: the pending rule of `dnv-worker.md` AR6 gates
  only the worker's auto-grow, never a user-driven grow (`architecture.md`,
  GrowSlice).

  **D-E.** `GrowSlice`'s request `ext_cnt` never reaches the model: it is
  only the exclusivity signal (a data grow states one, a meta grow must
  not); sizes are recomputed from the stored first data group or the meta
  ladder.

  **D-F.** `GrowSlice`'s DN black list is the request's own and nothing
  else; the gateway never appends to it, because one scan-and-pick round
  serves the whole new group and the location rule of `architecture.md`,
  Finding DN candidates, already leaves the candidate list with one entry
  per DN and per location, so the group's legs land on distinct DNs anyway.
  The worker's auto-grow (`dnv-worker.md` AR6) draws from the same kind of
  list and reaches the same distinctness by black-listing each pick as it
  goes.

### Cntlrs and inspects

* **CreateCntlr** — validate the slot; a candidate unit for one CN (the
  candidate extent count is the sum of every group's `ext_cnt`; the scan
  excludes the CNs of the SP's cntlrs (`architecture.md`, Finding CN
  candidates) and, at tier 1 of `architecture.md`, Per-operation allocation,
  their `location`s; tier 2 drops the location exclusion when tier 1 finds
  no CN). Each round plans from one read-only `Snapshot` opened with
  `openSp`, as `CreateMigration`'s planning read is: resolve, the `deleting`
  gate included, then the token (GW6), so a deleting SP is
  `FAILED_PRECONDITION` and a stale token `ABORTED` before the scan can
  answer `RESOURCE_EXHAUSTED` for want of an eligible CN — to a stale
  client, an answer computed against cntlrs it has not read; then the SP's
  slices, which size the scan, and its cntlrs, whose CNs the scan excludes.
  The locations are read after the snapshot, from those CNs' `CnConf`s —
  `cnLocations`, sound outside every transaction because a location never
  changes (`architecture.md`, Controller nodes) and because the STM
  re-checks the plan's cntlrs, below. STM: resolve; token; the slot in
  `cntlid_slot_list` and unused, else `INVALID_ARGUMENT`; the SP as this STM
  reads it having a cntlr on a CN the round's plan did not hold is candidate
  changed (GW9: the round planned its CN exclusion and tier-1 locations from
  the cntlrs as it read them, and a cntlr committed since — by another
  `CreateCntlr`, or the worker's cntlr replacement, `dnv-worker.md` AR7 —
  was not among them, so the pick may sit in its failure domain behind a
  capacity key that still verifies, or on its very CN when the scan ran
  after its charge; the next round plans from the SP with that cntlr in it;
  only a gain is checked, and only a token-less request meets one here,
  since the gain's `SpRev` bump fails a sent token first); re-verify the
  pick's capacity key; mint `cntlr_id`; put the `Cntlr` (`addr_port`, the
  CN's `nvme_tr_conf`, `cntlid_slot`, `primary` false, `disabled` false);
  append to `cntlr_id_list`; the CN's bookkeeping and `BumpCnRev`; append
  the CN's `nvme_tr_conf` to **every** `CdcEntry` of the SP (walk `nqn_list`
  to each `Subsystem` and its `CdcEntryKey`; an entry whose key is missing
  is first rebuilt as `CreateSubsystem` writes it, from the `Subsystem` and
  the cntlrs as this STM reads them, then changed like the rest —
  `DeleteCntlr` and a flag-changing `UpdateCntlrEnabled` walk the entries
  the same way); `BumpSpRev`. Reply `cntlr_id`.
* **DeleteCntlr** — STM: resolve; token; `primary` false and `disabled`
  true, else `FAILED_PRECONDITION`; reverse everything `CreateCntlr` did
  (the id list, the key, the CN's bookkeeping with `BumpCnRev`, the
  transport conf out of every `CdcEntry`); `BumpSpRev`. Reply `cntlr_id`.
* **UpdateCntlrEnabled** — STM: resolve; token; a no-op when already at the
  requested state (GW6); else set `disabled` to the negation of `enabled`
  and add (enable) or remove (disable) the transport conf in every
  `CdcEntry` — an enable of a cntlr that is still `primary` also sets
  `settling` (`dnv-worker.md` HL2: the re-enabled primary builds its stack
  from the standby shape, as a promoted one does) — `BumpSpRev`. Reply
  `cntlr_id` and `enabled`.
* **InspectCntlr** — STM: resolve; the `Cntlr` by id (`NOT_FOUND`), keeping
  its `addr_port` and the `CnConf`'s `cn_id` (read `CnConfKey` of that
  address in the same STM; deliberately no `SpRev` read); after the STM,
  `GetCntlrInfo` with the cluster id, `cn_id`, `sp_id` and `cntlr_id` at
  that address; a failure is `ABORTED`. Reply `applied_revision` and
  `cntlr_info`, both from the agent's reply (`architecture.md`, Cntlrs).
* **InspectSide** — STM: resolve; find the side by scanning the SP's slices
  (bounded, `architecture.md`, Cntlrs) for `side_id` (`NOT_FOUND`), keeping
  its DN's `addr_port`, its `dn_id` (through `DnConfKey`) and the side
  pointer (deliberately no `SpRev` read); after the STM, `GetSideInfo` with
  the cluster id, `dn_id` and the side pointer; a failure is `ABORTED`.
  Reply `applied_revision` and `side_info`, both from the agent's reply.

### Thin devices

* **CreateThinDevice** — validate that `size` is a positive multiple of
  `slice_cnt` times the stripe size (the stripe as the SP stored it,
  `bdev_conf.dm_raid0_conf.stripe_size`; state-dependent, so checked
  in-STM, behind a `model.ValidateBdevConf` of that conf — GW11 — so a zero
  stripe is `ABORTED` instead of reaching the "has no slice" refusal, which
  is about `slice_id_list`). STM: resolve; token; `td_name` in
  `td_name_list` or its key present is `ALREADY_EXISTS`; if `ori_name` is
  set: read the origin (`NOT_FOUND` if absent), and its `created` false is
  `FAILED_PRECONDITION` **writing nothing** (no id consumed, no bump —
  `architecture.md`, Thin devices), then an origin named by any
  `Clone.dst_td_id` (walk `clone_name_list` as `DeleteThinDevice` does, a
  draining clone included) is `FAILED_PRECONDITION` naming the clone, also
  writing nothing: while a clone hydrates, a snapshot of its destination
  would capture only the regions hydrated so far; mint `td_id`
  (`SpNextId`), the `dev_id` from `next_dev_id`, and `ori_id` as the
  origin's `dev_id` or zero; put the `ThinDevice` with `created` false;
  append to `td_name_list`; `BumpSpRev`. Reply `td_id` and `dev_id`.

  **D-H.** A `size` of zero is legal exactly when `ori_name` is set: a
  snapshot inherits its origin's size.
* **DeleteThinDevice** — a plan, then a deciding STM, looped as a GW9 unit.
  Plan, one read-only `Snapshot`: resolve; token; the td (`NOT_FOUND`); walk
  `td_name_list` for the tds whose `ori_id` is this td's `dev_id` and whose
  `created` is false (a listed key missing is `ABORTED`) and keep their
  names, the resolved cluster id and `sp_id`, and the `SpRev.revision` it
  read. The walk is the one read that grows with `MaxTdCntPerSp`, and it
  stays out of the STM because the STM compares every key it read: inside,
  a full pool's delete would exceed `EtcdMaxTxnOps` (Additions to
  `common/constants.go`). STM: resolve; token; a cluster id, `sp_id` or
  `SpRev.revision` other than the plan's is candidate changed, re-plan. The
  ids are there because a recreated SP's `SpRev` starts again at one, so
  the revision alone cannot tell two SPs of one name apart; the revision
  because every write to a td bumps `SpRev`: a snapshot created between the
  plan and this STM's commit moves the revision this STM reads, or,
  committing after that read, conflicts the STM on the `SpRev` key and
  moves it for the re-run. A token-less request then trips this check and
  re-plans; a token-carrying one fails the token check before it — its
  token is the revision the plan read — `ABORTED` ("stale revision"), no
  re-plan. Then the td (`NOT_FOUND`); `FAILED_PRECONDITION` when it is
  referenced by any `Namespace.td_id` (walk `nqn_list` to each `Subsystem`'s
  `ns_list`), by any `Clone.dst_td_id` (walk `clone_name_list`), or when a
  td the plan named still has this td's `dev_id` as its `ori_id` and
  `created` false (re-read those keys only); delete the key, remove the name
  from the list, `BumpSpRev`. Reply `td_id`.
* **ListThinDevices** — one STM: the `SpConf` and every listed td (missing
  is `ABORTED`) into `name_to_td`. This is the documented client wait
  primitive for `created`; a client snapshotting a clone's destination also
  waits until the clone is deleted and drained, `GetClone` answering
  `NOT_FOUND`. Neither RPC returns a revision, while the flip bumps `SpRev`
  and so do a clone's latch and drain, so a client whose snapshot request
  carries a token re-reads it from `GetStoragePool` after its last poll
  (`architecture.md`, Thin devices).

### Subsystems and namespaces

All pure etcd; every mutator: resolve, token, mutate, `BumpSpRev`.

* **CreateSubsystem** — the NQN valid (`architecture.md`, Common
  validation, the dnv-namespace rule included; the discovery NQN can never
  validate) and absent, else `ALREADY_EXISTS`; mint `ss_id`; the serial is
  `ss_id` rendered with `IdKeyFmt` and the model is `subsystemModel`
  (`architecture.md`, [D2]); put the `Subsystem` with an empty `ns_list`
  and the `allowed_hosts`, append to `nqn_list`, and put the `CdcEntry` —
  the NQN, the transport confs of every **enabled** cntlr's CN, the
  `allowed_hosts` — at `CdcEntryKey` of the cluster id, the shard, `sp_id`
  and `ss_id`. Reply `ss_id`.
* **DeleteSubsystem** — the NQN checked for length alone (`architecture.md`,
  Common validation, so a subsystem stored under an NQN the rules refuse can
  still be deleted; the same section says what a CN keeps of one in the dnv
  namespace); the subsystem by NQN (`NOT_FOUND`); `ns_list` empty, else
  `FAILED_PRECONDITION`; delete the `Subsystem`, the `CdcEntry` and the list
  entry. Reply `ss_id`.
* **ListSubsystems** — one STM: `nqn_list` to each `Subsystem` into
  `nqn_to_subsystem` (missing is `ABORTED`).
* **UpdateSubsystemHosts** — rewrite `allowed_hosts` in **both** the
  `Subsystem` and its `CdcEntry`; a `CdcEntry` whose key is missing is
  rebuilt as `CreateSubsystem` writes it and written with the new hosts.
  Reply `ss_id`.
* **CreateNamespace** — the subsystem by NQN (`NOT_FOUND`); `ns_idx`
  non-zero and unused in this subsystem, else `INVALID_ARGUMENT`; the td by
  `td_name` (`NOT_FOUND`); mint `ns_id`; defaults: an empty `dev_uuid` is a
  random RFC 4122 version 4 UUID in the canonical dashed form, an empty
  `dev_nguid` a random NGUID in hex; append to `ns_list`. Reply `ns_id`.
* **DeleteNamespace** — the NQN checked for length alone, as for
  `DeleteSubsystem`; locate by NQN and `ns_idx` (`NOT_FOUND`); remove from
  `ns_list`. Reply `ns_id`.
* **UpdateNamespaceDev** — locate the namespace; resolve the new `td_name`
  (`NOT_FOUND`); repoint the namespace's td reference. Reply `ns_id`.
* **UpdateNamespaceSuspended** — locate the namespace; set `suspended`.
  Reply `ns_id`.

### Clones

* **CreateClone** — validate the bounds of `architecture.md`, Clones
  (`validateCloneGeometry`: `src_slice_cnt` from one to
  `MaxSliceCntPerSp`, the source stripe and block sizes within their bounds
  and the block a multiple of the stripe); STM: resolve; token;
  `clone_name_list` at `MaxCloneCntPerSp` is `RESOURCE_EXHAUSTED`; the name
  free, else `ALREADY_EXISTS`; the destination td by name (`NOT_FOUND`); a
  destination td that any `Clone.dst_td_id` names (walk `clone_name_list`,
  a draining clone included) is `FAILED_PRECONDITION`;
  mint `clone_id`; put the `Clone` with its `dst_td_id`; append to
  `clone_name_list`; `BumpSpRev`. Reply `clone_id`. The "destination td
  must be empty" precondition is documented-unverifiable
  (`architecture.md`, [D3]). The per-CN clone budget is not tracked:
  `architecture.md`, Clones, lets a gateway track it in etcd, and this one
  does not.
* **DeleteClone** — it LATCHES, and the sp worker drains (`dnv-worker.md`,
  The clone drain). Two-phase (AG4). Phase 1 STM (read-only): resolve,
  through `openSpRead` and so without the SP's `deleting` gate, which only
  phase 2 applies (GW6); the token, checked explicitly because phase 1 is
  the decision on the path below and `openSpRead` skips the check (a stale
  token must still ABORT ahead of the clone's `deleting` answer); the clone
  (`NOT_FOUND`); **if the clone's `deleting` is already true, return OK
  here** — with no writes, no bump and NO agent call (`dnv-worker.md`
  CLD3). The short-circuit sits before the agent call and not in phase 2
  for a reason that is not an optimization: after the latch the CN has
  torn the stack down, so `GetCntlrInfo` no longer reports the dm-clone and a
  hydration check would wedge every repeat delete in `FAILED_PRECONDITION`
  for ever. Otherwise, when `force` is false, also resolve the **primary**
  cntlr's `addr_port` and `cn_id` — an SP with no primary cntlr is
  `FAILED_PRECONDITION`, since there is nobody to prove hydration with and a
  promotion makes the retry succeed (`force` true skips the lookup
  entirely: no agent call follows, which is what lets force delete a clone
  whose cntlr is unreachable). Between the phases, `force` false calls
  `GetCntlrInfo`; incomplete hydration **or an unreachable agent** is
  `FAILED_PRECONDITION` (`architecture.md`, Clones). Phase 2 STM
  (deciding): full re-resolution and the token check (GW6 — any interleaved
  mutation bumped `SpRev`, so a token the request carried subsumes the
  staleness of phase 1; a token-less request gets the re-resolution only,
  AG4); `loadClone` again, and if `deleting` became true since phase 1,
  return OK with no writes (the same rule, raced variant); else write
  nothing but the resume, the latch and the bump — `suspended` false on
  every namespace whose `td_id` is the clone's `dst_td_id`, as one
  `Subsystem` put per subsystem that actually changed and none for the rest
  (`resumeCloneDstNs`; `architecture.md`, Clones — the destination
  namespaces resume, their data being local, the twin of `DeleteTransfer`'s
  finalize under Transfers), the `Clone` put with `deleting` true and every
  other field unchanged, and `BumpSpRev` (`dnv-worker.md` CLD4). Reply
  `clone_id`.

  It does NOT delete the `Clone` key, does not touch a chunk key and does
  not shrink `clone_name_list`: the name must survive until the drain's
  last transaction, because `model.LoadSp` fetches clones by iterating it.
  The resume rides the LATCH so that it and the fan-out exclusion arrive in
  one `SpRev` bump; deferred, the destination namespace would go dark for
  the whole drain. Consequences for callers, all following from the `Clone`
  key and its list entry surviving: a `DeleteClone` returns while the clone
  still exists, so an observer polls `GetClone` until `NOT_FOUND`; a
  same-name `CreateClone` keeps failing until then — `ALREADY_EXISTS`, or
  `RESOURCE_EXHAUSTED` on an SP whose `clone_name_list` the surviving entry
  holds at `MaxCloneCntPerSp`, that ceiling being checked ahead of the name,
  which also fails an UNRELATED `CreateClone` for the whole drain; a
  `CreateClone` onto the same destination td, a `DeleteThinDevice` of that
  td and a `CreateThinDevice` snapshotting it all keep failing
  `FAILED_PRECONDITION` because their scans walk `clone_name_list`; and
  `DeleteStoragePool` keeps refusing while any clone drains.
* **loadLiveClone** — the gate of `dnv-worker.md` CLD1 that the two clone
  mutators that ADDRESS an existing clone open with: `UpdateCloneTrConf`
  and `AppendCloneBitmap` answer `FAILED_PRECONDITION` when `deleting` is
  true. (`CreateClone` addresses none and needs no gate: the surviving key
  and list entry keep it refused.) The append half is load-bearing, not
  cosmetic: a racing append could otherwise write a chunk key behind the
  drain, and the drain's final emptiness guard rests on "after the latch,
  no chunk key can ever appear again". `GetClone` and `DeleteClone`'s own
  phase 1 keep using plain `loadClone`.
* **GetClone** — one STM read. Reply the `Clone`.
* **UpdateCloneTrConf** — STM: resolve; token; the clone; replace
  `src_tr_conf_list`; `BumpSpRev`. Reply `clone_id`.
* **AppendCloneBitmap** — one chunk of the SOURCE bitmap, addressed by the
  PAIR (`src_slice_idx`, `bm_idx`). Chunk (s, b) holds the bytes of source
  slice s's bitmap from b times C on, at most C of them, C being
  `CloneBmChunkBytes` (Additions to `common/constants.go`): `src_slice_idx`
  picks the source slice, `bm_idx` fixes the chunk's byte offset WITHIN
  that one slice at the fixed quantum C and says nothing about any other
  slice. No chunk's meaning depends on any other chunk's existence or
  length, so a caller may append to any (s, b) at any time, in any order,
  and may leave chunks unsent entirely; an absent chunk reads as all-zero
  (all written) and a short chunk's missing tail reads as written, both the
  safe direction (`architecture.md`, Bitmap push protocol). Validation:
  `bitmap` non-empty (`INVALID_ARGUMENT`, `validateBitmap`, shared with
  `AppendMigrationBitmap`), then a page of at most C bytes, else
  `INVALID_ARGUMENT` — handler-local and NOT in `validateBitmap`, because it
  is judged on this request alone (a page longer than a whole chunk fits
  nowhere, whatever is already stored) and migration chunks carry no byte
  cap of their own. STM: resolve; token; the clone; `src_slice_idx` below
  `src_slice_cnt`, else `INVALID_ARGUMENT` (`src_slice_cnt` is the WHOLE
  slice bound — `CreateClone` already holds it to `MaxSliceCntPerSp`, so a
  second constant check here would judge nothing the geometry has not
  judged already); `bm_idx` below `MaxCloneBmCnt`, else `INVALID_ARGUMENT`;
  read the chunk at `CloneBitmapKey` of the pair — an absent key decodes to
  an empty bitmap, which is what makes "create if absent" fall out and is
  the same read the ceiling check needs; the stored length plus the page's
  at most C, else `RESOURCE_EXHAUSTED` (GW7's ceiling-reached case: C bounds
  the STORED chunk, not one page); **append** the bytes to that chunk — the
  action of `architecture.md`, Clones, because the caller pages one source
  slice's bitmap through `GetThinDeviceBitmap` and the pages of ONE chunk
  concatenate into exactly the bytes that chunk holds (`architecture.md`,
  Bitmap push protocol), so a replace would keep only the last page and
  place its bits at the chunk's own offset, which `PushCloneBitmap` would
  then hand the primary as "never written" and the agent would `blkdiscard`
  regions the source really wrote. The `Clone` record is **not rewritten**:
  it carries no chunk count, and how many chunks a clone holds is how many
  `CloneBitmap` keys it has — which is what the drain and `PushCloneBitmap`
  read. It opens with `loadLiveClone`, so an append to a latched clone is
  `FAILED_PRECONDITION` (`dnv-worker.md` CLD1) and the clone key is in the
  STM's read set either way; `BumpSpRev`. Reply `clone_id`.

### Transfers

All pure etcd, the standard mutator shape. **CreateTransfer** refuses an
`ori_nqn` that breaks the NQN rules of `architecture.md`, Common
validation, the dnv-namespace one included (`INVALID_ARGUMENT`), resolves
the origin namespace per `architecture.md`, Transfers, mints `xfer_id` and
appends to `xfer_name_list`. **DeleteTransfer** with `force` false
*finalizes*: it additionally sets `suspended` true on the origin namespace
in the same STM; with `force` true it *aborts* (the origin untouched); both
delete the `Transfer` and its list entry and `BumpSpRev`. **GetTransfer** is
one STM read. **UpdateTransferHosts** rewrites `allowed_hosts` (the
transfer's own — no `CdcEntry` involvement). The replies carry `xfer_id`.

**D-G.** `DeleteTransfer`'s finalize skips a vanished origin subsystem or
`ns_idx` instead of failing: the transfer is being deleted either way, and
the RPC must stay able to complete.

### Migrations

* **CreateMigration** — locate `src_side_id` by a slice scan (`NOT_FOUND`);
  its leg already holding two sides is `FAILED_PRECONDITION`; a candidate
  unit for one DN (the candidate extent count is the group's `ext_cnt`; the
  black list is seeded with the DNs of every leg and side of the group, and
  the two tiers of `architecture.md`, Per-operation allocation, apply —
  tier 1 also excludes those DNs' `location`s, read outside every
  transaction through `grpDnLocations` because a location never changes
  (`architecture.md`, Disk nodes); tier 2 rescans without the location
  exclusion when tier 1 yields fewer than the **one DN** this RPC places —
  never when it merely falls short of the oversampled candidate count — and
  its candidates are merged behind tier 1's; each round reads the group
  afresh — hence the candidate extent count, the black list and the
  locations — before it scans). STM: resolve; token; re-verify the
  topology; the group as this STM reads it holding a DN the round's read of
  it did not is candidate changed (GW9: the round planned its black list
  and tier-1 locations from the group as it read it, and a side hung off
  the group since — a migration of its other leg, a spare — was not in that
  group, so the pick may sit on that side's DN or in its failure domain,
  and the next round plans from the group with the side in it; a pick on a
  DN of the group always trips this, the scan having black-listed every DN
  the round read; only a token-less request meets a gained DN here, since
  the new side's `SpRev` bump fails a sent token first); re-verify the
  capacity key; mint `migr_id` and `dst_side_id`; append the new `Side` to
  the leg per `architecture.md`, Migrations (`provisioned` false, a
  `cntlid_slot` other than the source side's, D-I); the destination DN's
  bookkeeping and `BumpDnRev`; put the `Migration`; `BumpSpRev`. Reply
  `migr_id`.

  **D-I.** A migration destination side's cntlid slot is the first
  `cntlid_slot_list` entry that differs from the source side's; a list
  offering no second value is `FAILED_PRECONDITION` (`architecture.md`,
  cntlid slots).
* **FinishMigration** — two-phase like `DeleteClone`. Phase 1 (read-only)
  opens with `openSp`, as `CreateMigration`'s planning read does: resolve,
  the `deleting` gate included, then the token, so a deleting SP is
  `FAILED_PRECONDITION` and a stale token `ABORTED` ahead of the migration
  lookup and of any agent call; then the migration (`NOT_FOUND`); with
  `force` false, the **destination** side's DN. Between the phases, `force`
  false calls `GetSideInfo` and judges hydration from
  `migr_dst_info.dm_clone_info`; incomplete or unreachable is
  `FAILED_PRECONDITION`. Deciding STM: re-resolve and the token; apply the
  finish of `architecture.md`, Migrations (the destination side becomes the
  leg's side, the source side is removed, the source DN's bookkeeping is
  released with `BumpDnRev`); delete the `Migration`, its `MigrBitmap`
  chunks (every index below `bm_cnt`) and its list entry; `BumpSpRev`.
  Reply `migr_id`.
* **CancelMigration** — STM: resolve; token; the reverse of the create
  (drop the destination side, release the destination DN with `BumpDnRev`,
  delete the `Migration`, its chunks and its list entry); `BumpSpRev`. Reply
  `migr_id`.
* **GetMigration** — one STM read.
* **AppendMigrationBitmap** — validate non-empty; STM: resolve; token; the
  migration; `bm_cnt` at `MaxMigrBmCnt` is `RESOURCE_EXHAUSTED`; put a
  **new** chunk at the index `bm_cnt` (chunks are immutable, append-only);
  advance `bm_cnt`; `BumpSpRev`. Reply `migr_id`.

### Spare legs

The requests carry `grp_id` but no slice id, so each handler must locate the
slice containing the group. `CreateSpareLeg` and `SwitchSpareLeg` do it by
scanning the SP's slices in a plain snapshot (the model op re-verifies in
its own STM); `DeleteSpareLeg` has no snapshot — its locate runs directly
inside its one deciding STM below.

* **CreateSpareLeg** — a `RedundNone` group is `INVALID_ARGUMENT`; a
  candidate unit for one DN (the black list is the group's leg and side
  DNs, under the same two-tier rule of `architecture.md`, Per-operation
  allocation, as `CreateMigration`: tier 1 excludes their `location`s too,
  and tier 2 drops that exclusion — when tier 1 offers nothing for the one
  DN this RPC places — rather than leave the group unrepaired); then the
  call of `model.CreateSpareLeg` with the token's revision as its expected
  revision (GW6), whose STM also refuses while a spare of the group still
  has an unprovisioned side (`FAILED_PRECONDITION` through
  `ErrPrecondition`, reason "spare_unprovisioned" — `dnv-worker.md` AR8
  step 3). Reply `leg_id`. (The spare's side is written `provisioned`
  false.)
* **DeleteSpareLeg** — STM: resolve; token; the group and the spare leg by
  id (`NOT_FOUND`); a spare with two sides is `FAILED_PRECONDITION`, with
  `CreateMigration`'s own two-sides message (a migration is running on it,
  and releasing both sides would strand the `Migration`, `architecture.md`,
  Spare legs); remove it from `spare_leg_list`, release its DN (with
  `BumpDnRev`); `BumpSpRev`. Reply `leg_id`.
* **SwitchSpareLeg** — the pre-read refuses either leg with two sides,
  `FAILED_PRECONDITION`, with the same message; then the call of
  `model.SwitchSpareLeg` with the token's revision as its expected revision
  (GW6); its own preconditions apply (the spare's side must be
  `provisioned` — `architecture.md`, Side provisioning protocol — and each
  leg must have exactly one side, the pre-read's check again inside the
  STM; either failing is `FAILED_PRECONDITION` through `ErrPrecondition`).
  Reply `curr_active_leg_id` and `curr_spare_leg_id`.

### Bitmap reads

Both are read-only and two-phase: one STM resolves, then one agent call.
Both address the SP through its **primary** cntlr, so an SP with none is
`FAILED_PRECONDITION`: there is no controller to ask, and a promotion makes
the retry succeed, which is what a precondition means.

* **GetThinDeviceBitmap** — STM: resolve; the td by name to its `td_id`;
  the **primary** cntlr to its `addr_port` and, through `CnConfKey`, its
  `cn_id`; `slice_idx` within the SP's slice count, else
  `INVALID_ARGUMENT`. Then `GetThinDeviceBm` with the cluster id, `cn_id`,
  `sp_id`, `cntlr_id`, `td_id`, `slice_idx`, `start_block` and `block_cnt`
  (a zero `block_cnt` reads to the end); a failure is `ABORTED`. Reply the
  bitmap **verbatim** — the wire convention is produced by the agent, and
  the gateway never touches bits (GW14).
* **GetLegBitmap** — the same shape; the leg is located by a slice scan
  (`NOT_FOUND`), and the call is `GetLegBm` with the `leg_id`,
  `start_block` and `block_cnt`.

## Agent calls

AG1. **Placement.** Agent calls happen strictly outside STMs
(`architecture.md`, STM discipline): *before* the STM for `CreateDiskNode`
and `CreateControllerNode` (`Get*Size`), *after* the resolving STM for
`Inspect*` and the bitmap reads, *between* the two STMs for `DeleteClone`
and `FinishMigration` with `force` false — except that a `DeleteClone`
whose clone is ALREADY `deleting` answers in phase 1 and makes no agent call
at all (Clones, `dnv-worker.md` CLD3). When a call needs the cluster id, a
plain pre-read of `ClusterConf` supplies it; the in-STM read stays
authoritative.

AG2. **Connection.** Per call (`withAgentConn`): a ctx derived from the
request's with `DefaultGatewayAgentTimeout` as its timeout, one plaintext
gRPC client connection to the agent's `addr_port` with
`common.GrpcUnaryClientInterceptor` and `common.GrpcStreamClientInterceptor`
chained, closed when the call returns. The interceptors are mandatory on
every dnv connection (`grpc.md`, Wiring) and forward the request's trace id
(`grpc.md` T3). The gateway dials per call and closes after the call: it
keeps no connection cache like the worker's (`dnv-worker.md` RW7).

AG3. **Failure mapping.** A transport or non-OK status maps to the code the
RPC's specification names: `ABORTED` everywhere except the `force` false
checks of `DeleteClone` and `FinishMigration`, where an unreachable agent is
`FAILED_PRECONDITION` (the caller cannot prove hydration is done). The
agent-side `AgentReply.code` convention does not apply to `Get*Size`,
`Get*Info` or `Get*Bm` beyond what their replies define.

AG4. **Two-phase rule.** Any handler with a mid-flight agent call re-runs
**full resolution and the token check** in its deciding STM. No facts from
phase 1 are trusted in phase 2 except as hints; for a request that carries
a token, the token check makes any interleaved mutation visible as
`ABORTED` ("stale revision"), which is what keeps such a two-phase RPC
exactly as safe as a one-STM RPC. A **token-less** request keeps the full
re-resolution but not that visibility — GW6 is presence-based, so an
interleaved mutation it did not observe stays invisible to it. That is the
AG4 half of the token-less cost GW6 accepts.

The complete call matrix:

| Gateway RPC | agent RPC | when |
|---|---|---|
| `CreateDiskNode` | `DiskNodeAgent.GetDnSize` | before the STM |
| `CreateControllerNode` | `ControllerNodeAgent.GetCnSize` | before the STM |
| `InspectDiskNode` | `GetDnInfo` | after the STM |
| `InspectControllerNode` | `GetCnInfo` | after the STM |
| `InspectCntlr` | `GetCntlrInfo` | after the STM |
| `InspectSide` | `GetSideInfo` | after the STM |
| `DeleteClone` with `force` false | `GetCntlrInfo` | between the STMs |
| `FinishMigration` with `force` false | `GetSideInfo` | between the STMs |
| `GetThinDeviceBitmap` | `GetThinDeviceBm` | after the STM |
| `GetLegBitmap` | `GetLegBm` | after the STM |

No other `service Gateway` RPC leaves etcd — the matrix is complete.

## cmd/dnv-gateway

CM1. **Shape.** `cmd/dnv-gateway/main.go` mirrors `cmd/dnv-worker/main.go`
and `cmd/dnv-cdc` structurally — cobra and viper, an environment prefix,
two-signal handling, a thin `main` — and it is the only binary needing both
the gRPC-server flags (from `cmd/dnv-agent`) and the etcd flags (from
`cmd/dnv-worker`). It has a package doc comment naming this spec; every
flag is also a config key and an environment variable under the prefix
`DNV_GATEWAY_`; `main` executes `newRootCmd`; one root command, no
subcommands, cobra's own usage and error printing silenced, no positional
arguments; `addFlags` and `bindViper` (the flags bound through viper, the
environment prefix, a dash-to-underscore key replacer, automatic
environment lookup, an optional `--config` file); an I/O-free
`optionsFromViper`; and comma-splitting of list flags through the local
`splitList` idiom.

CM2. **Flags.** `--grpc-network` (tcp by default) and `--grpc-address`
(required), as in `cmd/dnv-agent`; `--etcd-endpoints` (required),
`--etcd-dial-timeout` (defaulting to `common.DefaultEtcdDialTimeout`) and
`--config`, as in `cmd/dnv-worker`. Deliberately no `--etcd-op-timeout`
(`dnv-worker.md` EU5) and no default gRPC port: `--grpc-address` is
required exactly as the agent's is, and no default-port constant exists.

CM3. **Startup.** `run` binds viper, reads the options, mints a startup
trace id (`common.WithTraceId` of a fresh `common.NewTraceId`), derives a
cancelable ctx, arms the two-signal `watchSignals` pattern — a signal
channel that holds both signals; the first SIGINT or SIGTERM cancels the
ctx, and a second one exits with status 1 without a clean drain — builds
the client with `etcdutil.New` (a lazy dial: startup never fails on an
unreachable etcd), and returns `gateway.Run` of the ctx, the client and the
`gateway.Config`. `Run` closes the client (GW2), so `main` does not.
Logging setup: none — `common`'s `init` installs the JSON logger, and the
gateway keeps level Info by doing nothing (`log.md` R6).

## Log records

LG1. The gateway adds no bespoke logging for gRPC or etcd traffic: the gRPC
and etcd items of `log.md` R8 are fully covered by the interceptor chains (every
server and client request and reply, the trace id included; `grpc.md`, L1
to L6) and by `etcdutil` (every get, put, delete and range, once per STM
attempt — duplicates on retry are expected and acceptable; `log.md`, etcd).
The OS-command and file items of `log.md` R8 are implemented inside
`LimitedOsClient`, which the gateway never uses.

LG2. The records this component owns, all through the ctx forms:
`gateway starting` (`grpc_network`, `grpc_address`, `etcd_endpoints`) and
`gateway stopping` from `gateway.Run`, with `etcd client close failed`
(`error`) between them when GW2's deferred close of the client fails;
`gateway serving` (`network`, `address`) just before serving;
`signal received` and `second signal, exiting without a clean drain` from
`main`'s `watchSignals`. All are Info except two Warns: the failed close,
and the second signal — giving up the drain abandons in-flight RPCs, so it
is not a routine event.

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
suite asserts nothing about latency or throughput. No `dnv-worker` runs;
the suite plays the worker through `workerctl` wherever a gateway
precondition waits on a worker flip or a latch needs the worker's drain. It
runs on one server as a plain user, because nothing it exercises needs
root.

**Topology.** The developer machine builds the binaries and drives one
Linux server over ssh, with no sudo anywhere. Everything runs on that
server and listens on its loopback, on a port block disjoint from every
other suite: one etcd, configured the way every etcd serving dnv must be
(its transaction-op cap at `EtcdMaxTxnOps`, which the suite reads from the
`workerctl` it has just built instead of copying the number, as it reads
`MaxSliceCntPerSp` for the clone it creates at that ceiling); three gateway
instances on one launch line that differs only in the gRPC address; four fake DNs and three fake
CNs, in distinct locations because the CN scan dedupes locations too, served
by `fakeagent`, the worker suite's fake (`dnv-worker.md`, Integration test
plan); and the two drivers, `gatewayctl` for the RPCs and `workerctl` for
the etcd ground truth, both run on the server. Every stage runs under a
trace id of its own, the thread that ties a script stage to the gateway,
agent and etcd records it caused; the suite asserts once that the id a
stage sent reaches the fake agent the gateway called.

**Cases.** The cases run in a fixed order, fail-fast, each against a store
wiped of every dnv key, with every gateway restarted and every fake reset,
so no state leaks between them; each creates its own cluster through the
gateway, never through `workerctl`.

* smoke (one gateway) — the full lifecycle: every RPC's happy path but
  `ListStoragePools`, which the parallel and restart cases run, each
  mutation's exact write set read back, the `CdcEntry` the cdc serves
  included — as `CreateSubsystem` writes it, as `UpdateSubsystemHosts` and
  the cntlr mutators rewrite it, and gone with its subsystem; every
  defaultable member of the
  stored confs resolved on the write path; paging with its empty last page
  and a malformed token; the inspects and bitmap reads passing the agent's
  bytes and applied revision through verbatim, a revision no stored rev key
  holds; the worker flips the snapshot and spare-switch preconditions wait
  for; the clone's pair-addressed chunks, its latch with the destination
  namespace resumed, its repeat delete and its refusals while latched, then
  its drain; and the sp latch with the refusals it brings, then its drain,
  after which every extent and capacity key is back and no dnv key is left.
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
* faults (one gateway) — the refusal batteries on a live pool: validation,
  not-found and precondition classes, each refusal provably writing
  nothing; agent faults — a hung agent bounded by the agent-call budget, a
  closed port refused fast, inspects of hung and stopped fakes; and the
  hydration checks of `DeleteClone` and `FinishMigration` refusing on
  incomplete hydration and on an unreachable agent, `force` skipping them.
* restart (kill under load) — one gateway is killed with SIGKILL while it
  provably holds a create in its handler; every pool of the wave is then
  either complete or absent, never partial, and the accounting matches the
  survivors; the absent ones are re-driven through a survivor, since a
  failed reply may still be a committed write; the killed instance restarts
  on the same command line with no cleanup, and the store holds only the
  data's keys, nothing that describes a gateway.

**Rules exercised.** The smoke case exercises GW4 to GW12 and GW14 across
every resource group, GW5's `deleting` gate on a latched pool, GW10's
paging, GW11's write-time resolution, AG1 and AG3 on the inspects and the
bitmap reads, and the trace-id chain of GW2 and AG2; the parallel case GW9
— candidate units that lose a pick and run again, invisibly — and GW12
under concurrency; the contention case GW6 as fence and as liveness loop,
GW8 and GW12 (a loser burns no id); the faults case GW4, GW7 throughout,
AG2's budget, AG3 and AG4; and the restart case GW1, with the atomicity of
one STM per RPC (GW8). Left to the unit tests: `CreateCluster`'s
hash-collision guard, GW10's token-decode internals, `GracefulStop` (GW3),
and the token-less path of GW6, which the driver never sends (below).

Out of scope: performance, latency and soak; real agents, `dnv-worker`,
`dnv-cdc` and `dnvctl`; a multi-member etcd; etcd outages (unit level only);
TLS and authentication, which dnv does not have; clone-budget enforcement,
which the gateway does not do. The real drains that consume the two latches
belong to the worker suite (`dnv-worker.md`, Integration test plan).

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
After every successful pool mutation, a worker flip included, the script
refreshes its token from `GetStoragePool`.

**Cleanup.** Cleanup runs unconditionally at the start of every run —
before the server preflight, so a crashed earlier run cannot fail it — and
at the end only on success: a failing run leaves etcd's data, every log and
every behavior file in place and prints its diagnostics — the failing stage
and its trace id, the tail of every process log, a full dump of the dnv
keys and the listening ports — so the debris is what the developer reads.
Cleanup signals every daemon the suite started — etcd, the gateways and the
fake agents — by its recorded pid with a pattern kill as a safety net, and
removes the suite's work directory; nothing outside that directory is
touched, and the suite leaves no kernel state, no packages and no users
behind.

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
passes its trace id as plain metadata (`grpc.md`, Drivers and fakes).

**The etcd verification, `workerctl`.** Ground truth never goes through the
code under test: after every mutating stage the script reads raw decoded
etcd state through `workerctl`'s read-only subcommands, and the gateway's
own get, list and inspect replies must agree with it. `workerctl` also
answers the readiness probe of etcd and, on the developer machine before
anything is shipped, prints the Go constants the suite sizes itself by. The only writes
it performs here are the worker-role ones, the suite playing the worker:
the flips `set-created` and `set-provisioned`, at exactly the stages where
a gateway precondition waits on the worker, and the drains `drain-sp` and
`drain-clone`, standing in for the sp coordinator after a latch the gateway
committed (`dnv-worker.md`, The sp drain and The clone drain). Every flip
and every drain step bumps `SpRev` as the worker's own would, except the sp
drain's last step, which deletes the key, so the script's token bookkeeping
counts them. Any other `workerctl` write would test `workerctl` rather than
the gateway and is forbidden here; the store wipe between cases and the
store-revision probe of the refusal brackets go through `etcdctl`. The
drain stand-ins run `model`'s own drain ops in a loop with no pass and no
timer, and a step bound lets the worker suite build a partly drained pool
instead of racing one. The latch stand-ins, `set-deleting` and
`set-clone-deleting`, are the worker suite's, and `set-clone-deleting`
deliberately does not resume the destination namespaces the way
`DeleteClone` does: that write is the gateway's, and a driver that
re-implemented it would drift from it.
