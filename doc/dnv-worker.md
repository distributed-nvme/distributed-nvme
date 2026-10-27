# dnv-worker.md — `dnv-worker`: vote, shard and revision workers

This document owns the `dnv-worker` binary and the two libraries it is the
first consumer of: package `etcdutil`, the one door to etcd (EU1 to EU7);
package `model`, the etcd data model as Go together with the internal
mutations the gateway's handlers share (MD1 to MD9); the `cmd/dnv-worker`
command (CM1 to CM7); the vote worker (VW1 to VW11); the shard worker (SW1 to
SW6); the revision worker with its dn, cn and sp role files (RW1 to RW23);
health bookkeeping (HL1 to HL7); bitmap pushes (BM1 to BM6); the automatic
reactions (AR1 to AR10) with the two pass steps of a repair (AR11, AR12) and
the forced-start clear (AR13), the sp drain (SPD1 to SPD15) and the clone
drain (CLD1 to CLD12); the worker's log records; and the intent of the worker
integration suite. It leans on `architecture.md` for the object model, the
etcd data model, allocation, common validation, the agent contracts, the
cross-component procedures (failover, migration, teardown by sweep, the
levels) and the decisions [D8], [D10], [D15], [D16], [D17] and [D18]; on
`log.md` and `grpc.md` for the logging, trace-id and interceptor rules; on
`dnagent.md` and `cnagent.md` for the agent side of every RPC the worker
issues; on `gateway.md` for the public RPCs whose transaction bodies `model`
carries; and on `layout.md` for package placement and the dependency rules.

## Scope and placement

`dnv-worker` is the control-plane process that converges the data plane to
the desired state in etcd (`architecture.md`, System overview): it watches
the revision keys, shards the work by shard code, drives agents through the
unary `SyncupDn`/`SyncupSide`/`SyncupCn`/`SyncupCntlr` and `Push*Bitmap`
calls, watches them through the `Check*` streams, maintains the health fields
and capacity keys, flips `Side.provisioned` and `ThinDevice.created`, moves
the stages of repairs and runs the retire timer, and performs the automatic
reactions. It never serves gRPC, never calls
`Get*Info`, and never talks to hosts.

A process carries one to three **roles**, `dn`, `cn` and `sp`
(`WorkerRoleDn`, `WorkerRoleCn`, `WorkerRoleSp`); each role is voted, sharded
and driven independently. Three layers per role, one process:

```
dnv-worker process  (every role by default; one seed per incarnation)
└── vote worker (VW) — registers the seed under every role prefix, observes the
    registries, commits membership after the grace window, computes ownership
    ├── role dn: owned shards ──▶ shard worker per shard (SW): watches the shard's dn rev prefix
    │       └── revision worker per DN (RW13): one goroutine, one CheckDn stream,
    │           SyncupDn on every revision, DnConf.err_epoch + DnCapacity (HL1)
    ├── role cn: … the shard's cn rev prefix ──▶ per CN: CheckCn / SyncupCn (the cn role)
    └── role sp: … the shard's sp rev prefix ──▶ per SP (RW14): snapshot + fan-out coordinator
            ├── per side:  goroutine, CheckSide stream, SyncupSide, PushMigrBitmap (BM)
            ├── per cntlr: goroutine, CheckCntlr stream, SyncupCntlr, PushCloneBitmap
            └── flips (provisioned, created), sp health (HL2), reactions (AR)
```

The packages this document owns — `etcdutil`, `model`, `worker` and
`cmd/dnv-worker` — are placed by `layout.md`, Directory tree, and what each
may import is `layout.md`, Dependency rules.

Out of scope here: the gateway (request validation, the public RPCs, and
the agent calls it makes: the `Get*Info` info reads, which serve its
inspects, its hydration checks and its forced start's check of a group's
array, and the `Get*Size` and `Get*Bm` calls),
`dnv-cdc`, `dnvctl`. Where the
worker performs an "internal" variant of a gateway mutation (Automatic
reactions), the STM body lives in `model` and the gateway adds only
validation and reply mapping on top (MD8).

## Constants and schema

### Constants this document owns

The worker's constants are one region of the constant block in
`common/constants.go`, which is authoritative for their comments: `DefaultVoteWorkerInterval`, the seconds between two refreshes of a
worker's registry key (VW2), a registration not refreshed for twice that long
being dead (VW3); `DefaultVoteWorkerGraceTime`, the seconds an observed
membership transition must hold before it is committed into the effective
membership (VW5); `RetiredLegMaxAge`, the age from its `retired_epoch` at
which the retire timer may delete a retired leg (AR12);
`DefaultWorkerSyncupTimeout` and `DefaultWorkerPushTimeout`,
the per-call deadlines of the worker's agent RPCs (RW5, BM3);
`DemotionHoldTimeout`, the bound of the demotion hold, in which a fan-out
that demotes a primary holds its sides and its other cntlrs for the demoted
cntlr to report its demotion applied (RW22);
`DefaultEtcdDialTimeout` and `DefaultEtcdOpTimeout`, the etcd client's dial
deadline and its budget per plain operation and per whole transaction, every
retry included (EU1, EU5); `DefaultEtcdKeepaliveTime` and
`DefaultEtcdKeepaliveTimeout`, how long a connection of the etcd client
carries no read before the client pings it and how long the client waits for
the answer before it closes the connection (EU1); `DefaultEtcdWatchSilence`,
how long a watch may hear nothing from etcd, neither an event nor a progress
notification, before the etcd client ends it with an error (EU3);
`EtcdMaxTxnOps`, the `--max-txn-ops` requirement on every etcd serving dnv,
a constant the block holds for the gateway
(`gateway.md`, Constants this document owns) because the transaction it
is sized by is `CreateStoragePool` at its widest shape, while the sp drain's
slice batch (SPD13, SPD14), its repair and retired-leg batches (SPD15), the
created flip's transaction (RW19) and the clone flip's (MD6) are the other
bounded transactions it has to cover, and
the cntlr replacement's (MD6, AR7) and the clone drain's batch and final
transaction (CLD11) are checked against it but fit etcd's default cap;
`EtcdMaxRequestBytes`, the `--max-request-bytes` requirement on every etcd
serving dnv and the etcd client's send limit (EU1), sized by the provisioned
flip (RW18) whose sides span every slice of a pool at the widest shape —
`MaxGrpCntPerSlice` groups in each list, `MaxAllocLegPerGrp` legs, a
repair's name and `MaxRetiredLegPerGrp` retired-leg ids per group,
`MaxMigrCntPerSp` legs with a migration's second side, every string at its
bound of `architecture.md`, Common validation, every id at its widest —
since the flip writes each slice
it touches once, in one transaction; it is above etcd's default and above
the request size etcd recommends, which etcd notes when it starts, and a
unit test commits that flip against a real etcd started with it;
`MaxFlipCreatedPerTxn`, the most candidates one created-flip transaction
carries (RW19); and the three role names `WorkerRoleDn`, `WorkerRoleCn` and
`WorkerRoleSp`. One constant of this document's sits outside that region,
beside the health-check intervals it counts in: `ObsMaxAgeIntervals`, how
many of an object's intervals the observation a round's reply answers from
may be old before the round is a missed one (HL7).

Derived, never a constant: the **dead threshold** is twice the configured vote
interval; the **round timeout** is the object kind's health-check interval
(RW8). `DefaultHealthCheckInterval` is the worker's own fallback cadence — the
retry of a missing or unusable cluster conf (RW9) and the sp coordinator's
tick when one is unusable (AR1). `roundPeriod` keeps a zero guard on the same
constant, but only as a backstop against a hot loop: RW9's gate refuses a zero
interval before any round can read one, so no round period is ever computed
from a substituted value. The four reaction thresholds, whose defaults are
`DefaultPrimaryUnhealthy`, `DefaultCntlrUnhealthy`, `DefaultSideUnhealthy` and
`DefaultLegUnhealthy`, are resolved from `SpConf.event_threshold` when a pass
reads them (AR4). `DefaultDnExtSize` and `DefaultPoolLowWatermarkPct` are read
on no worker code path at all: the gateway resolves those two at write time
(`architecture.md`, Common validation), and the worker uses what is stored or
refuses it (RW9, RW14, AR1).

### Schema this document owns

Three members of the schema are this document's. `WorkerReg`, the value of a
registration key (`architecture.md`, Key table), holds the writer's unix
seconds at the put in `epoch`; the field is informational, because liveness
is judged by the observer's own clock since the put it last saw (VW3, VW4),
never by comparing the epoch with local time. `Cntlr.settling` is the flag
HL2 clears and AR5 reads: true from the moment a cntlr becomes primary (or,
still primary, is re-enabled) until the worker first observes it clean in
that role with its stack built — no row outside `leg_id_to_leg`,
`grp_id_to_md_raid` and `side_id_to_path` provisioning, or missing but for
the level's — or a
failover demotes it; AR5 holds a settling primary to `cntlr_unhealthy`
instead of `primary_unhealthy` when that is the longer. A record that lacks
the field decodes false, settled, and is judged by `primary_unhealthy` as
any settled primary is. `Migration.cutover_pending` is the flag RW18's
cutover clears: `CreateMigration` writes it set (`architecture.md`,
Migrations), and nothing but the cutover changes it after that; a record
that lacks the field decodes false, a cutover that has started
(`architecture.md`, Upgrade in lockstep). Nothing else in the schema is
this document's; in particular `Migration` carries no other field of this
document's, because there is no migration reaction (Automatic reactions).
The generated code is
regenerated and committed with the schema (`layout.md`, Protobuf
generation).

## Package `etcdutil`

`log.md`, Record obligations by subsystem, fixes the logging obligations of
every etcd access and forbids direct `clientv3` calls elsewhere;
`layout.md`, Directory tree, places the package. The worker is its first
consumer; the gateway (`gateway.md` GW2) and `dnv-cdc` (`cdc.md`, The etcd
watcher) use it too, within what is specified here.

EU1. **Client.** `New` wraps one etcd v3 client built from the endpoints and
the dial timeout it is given. No dnv gRPC interceptor is attached to it
(`grpc.md`, Placement — etcd logging is done by these helpers, not by
message interceptors). Its send limit is `EtcdMaxRequestBytes` (Constants
this document owns): the client's own default is below the largest
transactions dnv commits, and the client refuses a larger request before
etcd sees it. Its receive limit stays the client's own default, since a set
one would cap every range reply too, which no dnv constant sizes. It pings
a connection that has carried no read for `DefaultEtcdKeepaliveTime` while a
request or a watch is open on it, and closes the connection when the ping
goes unanswered for `DefaultEtcdKeepaliveTimeout`, so that an etcd member
that died without a TCP reset drops out of the connections the client
spreads its requests over within those two times once a request or a watch
is open on its connection, instead of when the kernel gives up
retransmitting; the keepalive time is no
shorter than the etcd server's default minimum ping interval, so etcd needs
no flag for it. `Close` releases it. A failure to reach any endpoint at
construction is **not** fatal to the caller by itself: the client retries in
the background; the worker's vote layer stays fenced until its first
successful put (VW8).

EU2. **Typed plain operations.** All take a ctx, log the record of their kind
with `slog.InfoContext` as `log.md`, Record obligations by subsystem,
specifies for etcd, and (un)marshal binary protobuf. `Get` is the point
read, leaving its message untouched when the key is not found, and logs
`etcd get`. `Put` writes and logs `etcd put`. `Delete` deletes, an absent
key being no error, and logs `etcd delete`. `Range` scans a prefix in key
order and returns one `KV` per key — the key, its value and `ModRev`, the
key's mod revision, which VW3's rescan compares — together with the store
revision the scan was served at, logging `etcd range` with the prefix and
the count and never dumping the values. `RangeDesc` scans in **descending**
key order and stops at a limit (a limit of zero or less meaning none): the
capacity keys embed the free extent count in `FreeSpaceFmt`, so descending
key order is largest-free-first, which is the MD5 allocator's walk.
`RangeKeys` is the keys-only scan, returning one `KeyRev` per key — the key
and its mod revision, which BM5 memoizes. `RangeKeysAtRev` is `RangeKeys`
pinned to one store revision, for MD3's bitmap-index scans, which must be
served at the revision the SP snapshot was read at; a revision the store has
compacted away is an error, never silently served from a newer one.
`RangeAtRev` is `Range` pinned to one store revision the same way, a
compacted revision being an error as well, for MD3's ranges of a pool's
repairs and retired legs: it returns each key with its value and logs the
one `etcd range` record, each value being logged as its caller decodes it.
`Decode` unmarshals one scanned value and logs it as an `etcd get` with
`found` true, so every value a caller actually reads is logged
individually, as `log.md`, etcd, requires.

EU3. **Typed watch.** `WatchTyped` opens one prefix watch starting at the
revision it is given, with a constructor for the prefix's message type.
Every event is delivered as an `Event` carrying its type (put or delete),
the key, the decoded message for a put, and the event's revision — for a
put the key's mod revision, which VW3 compares with a scan's `ModRev`; for a
delete the revision of the deletion — in order, and logged as
`etcd watch event` with the key, the type and, for puts, the decoded value.
The etcd client reconnects transparently; a watch cancelled by the server as
compacted is reported once on the error channel and both channels close —
the caller rescans (SW4, VW3), which `IsCompacted` lets it recognize without
importing the etcd rpc types itself. When the ctx ends both channels close
without an error. There is no untyped watch: every dnv prefix holds one
message type.

The watch asks etcd for progress notifications, and a watch that hears
nothing from etcd, neither an event nor a notification, for
`DefaultEtcdWatchSilence` ends the same way, with an error that names the
silence: etcd can stop serving one watch while its connection lives, and
nothing else would notice. The open counts toward the silence and never
blocks the caller, because an open hangs on a dead connection. The
threshold must exceed twice etcd's notification interval with that
interval's jitter, since etcd skips the first notification after an event,
and the deployment bounds that interval to fit (`architecture.md`,
Components: invocation reference). Every caller rescans on any watch error
(SW4, VW3, RW21; `cdc.md` WV4), and the record of its restart carries the
error. etcd sends no notification to a watch whose
start revision lies past the store's revision until the store reaches it,
and every dnv watch starts at its scan's revision plus one: a worker's
registration puts move the store, but a cdc that runs alone on an etcd
nothing writes to restarts its watch once per threshold, each time with a
rescan, which is accepted because a rescan that finds nothing changed
impacts no host (`cdc.md` WV4).

Every watch of a process rides one gRPC stream of the etcd client, because
no caller puts outgoing gRPC metadata on the contexts it watches with, and
the client keeps one stream per distinct set of that metadata. A dead
connection, a member cut off from the leader or a stalled stream therefore
stops the events of a worker's registry watches and of the revision watches
of its shards together, and the registry echo check (VW8 (b)) stands for
the latter: when it fences the worker, every shard worker stops, and a
shard is driven again only from a fresh scan (SW2), by the worker's next
incarnation or by a peer. The `ClusterConf` watch (RW21) loses its events
with them too, but it belongs to the process rather than to an
incarnation, so no fence restarts it (Known limits). A context with
outgoing metadata would get a stream of its own, which the echo check
would no longer cover.

EU4. **STM.** Two runners over the etcd client's STM with
serializable-snapshot isolation: `RunSTM`, read/write, which the etcd client
retries on conflict until the ctx ends or the transaction's EU5 budget is
exhausted — in the ordinary case the earlier of the two, a worker's ctx
living as long as the worker does; and `Snapshot`, read-only, whose reads
are all served at one store revision and whose commit is a no-op. It serves
a multi-key load that has no use for the revision itself, such as the
gateway's. Every multi-key load in the worker goes through `SnapshotRev`
instead, or, for the part of MD3's load that follows its ranges,
`SnapshotAtRev`: MD3's and AR1's because they need the revision, and the
dn role's read of a `DnConf` with its `DnRev` (RW13) because `SnapshotRev`
serves a later read linearizable at the pinned revision, while `Snapshot`
serves it from whichever member the client picks and fails on one that has
not applied that revision yet.

A third runner, `SnapshotRev`, is `Snapshot` that also **reports** the store
revision its reads were served at: the client's STM pins that revision at
its first read but keeps it private, and MD3 needs exactly that number to
run its scans (`RangeAtRev`, `RangeKeysAtRev`) and its node reads at the
snapshot's own revision, so `SnapshotRev` pins it itself — first read
linearizable, every later read at that revision — instead of going through
the client's STM constructor. Its callback runs exactly once, a read-only
load having no write set to conflict on, and a put or delete inside it is
a programming error that fails the load.

A fourth runner, `SnapshotAtRev`, is `SnapshotRev` served at a revision the
caller already holds: every read, the first one included, is served at that
revision, a revision below one is refused before the callback runs, a
compacted one is an error, and the callback runs once. MD3 reads its node
records with it after its ranges, so a load split around a scan the STM
cannot perform reads one store state in all its parts.

`STM` is the typed view: `Get` (reporting whether the key was found), `Put`,
`Del` and `Rev`. Each call logs the record of its kind (`etcd get`,
`etcd put`, `etcd delete`); a retried transaction logs twice, which `log.md`,
etcd, accepts. A callback that returns an `ErrPrecondition` (MD7) aborts the
transaction **without** commit and without retry, and `RunSTM` returns that
error unchanged. Any callback error ends the attempt before the commit txn is
issued; what marks one a deliberate, non-fatal abort that must reach the
caller as it is, is that it wraps the sentinel `ErrNoCommit` — which is what
`ErrPrecondition`'s `Unwrap` returns, so `etcdutil` states the contract
without importing `model` (`layout.md`, Dependency rules).

EU5. **Timeouts.** Every plain operation runs under `DefaultEtcdOpTimeout`
(`--etcd-op-timeout` is deliberately not a flag); a shorter caller ctx wins.
A transaction is budgeted as a **whole**, every EU4 conflict retry included,
and not per attempt: the client's STM owns its retry loop and fixes its
abort ctx at construction, so `etcdutil` has no way to express a per-attempt
deadline, and re-running the transaction under a fresh budget would not
terminate while etcd is unreachable — a budget expiry caused by contention
is indistinguishable from one caused by a dead etcd. Under sustained
contention on one key a transaction can therefore exhaust the budget across
its attempts and fail while the caller's ctx is still alive; for the worker
that is benign, its retry cadence being its own round (RW12) and every
mutation re-validating its preconditions on the next pass (MD7, AR2), and a
caller with no round of its own is left to decide what to do with the
failure (EU6). A read-only load is budgeted the same way, as one operation
rather than one per key read. `New` uses `DefaultEtcdDialTimeout` unless the
caller passes another value (`--etcd-dial-timeout`, CM1).

EU6. **Errors.** Connection, timeout and (de)serialization errors are
returned wrapped, never retried inside the helpers (the STM conflict retry
of EU4 is the client's, not ours). The caller decides — the worker always
retries on its own cadence (the next heartbeat, the next round, the next
pass) and never crashes on an etcd error.

EU7. **Tests.** The package's tests run against a real etcd binary, the one
`ETCD_BIN` names or else the one on the PATH, started on a free localhost
port with a temporary data directory; there is no dependency on the etcd
*server* module. With no binary, or an `ETCD_BIN` that names no file, the
package's test binary exits with failure before any test runs — naming the
`ETCD_BIN` it cannot use, or, with no binary, saying where one comes from —
because a run that skipped the etcd-backed tests would report success while
testing nothing. The `test` target of the `Makefile` passes an `ETCD_BIN`
that is not empty on to the tests unchecked, so a test binary refuses one
that names no file as above; with `ETCD_BIN` empty or unset it hands the
tests the etcd on the PATH, else the one in `integtest/bin/cache`, where
the `fetch-etcd` target fetches the etcd release the integration suites
pin, and with neither it fails before any test runs and names the
`fetch-etcd` target. The same contract serves the etcd-backed tests of
`model` (MD9), the worker, the gateway, the cdc (`cdc.md`, Integration
test plan) and the planting library.

## Package `model`

The etcd data model of `architecture.md`, etcd data model, as Go, shared by
the worker, the gateway and `dnv-cdc`. `model` imports `common`, `pb` and
`etcdutil`; it never dials an agent, never sleeps, and logs nothing of its
own beyond the `etcdutil` records its reads and writes produce. The package
exists so that the worker's internal mutations and the gateway's public RPCs
run the same transaction bodies (MD8).

MD1. **Files.** `keys.go` (key formats, parsers, `ClusterId`), `stm.go` (the
SP snapshot loader), `capacity.go` (the capacity keys), `alloc.go` (the
allocator of `architecture.md`, Finding DN candidates, and `architecture.md`,
Finding CN candidates), `ops.go` (the internal mutations of MD6),
`repair.go` (MD6's repair and retired-leg ops, `architecture.md`, Repair),
`forcedstart.go` (MD6's forced-start ops, `architecture.md`, Repair),
`drain.go` (MD6's sp-drain ops, The sp drain), `clonedrain.go` (MD6's
clone-drain ops, The clone drain). Unit tests are colocated.

MD2. **Keys.** One function per key of `architecture.md`, Key table, one per
prefix a component scans or watches, and a parser for every key whose fields a
component reads back out of it (the rev, registry, capacity, bitmap-chunk and
cdc keys); the list-range and `ClusterConf` keys carry one name after their
prefix and are decoded by a prefix trim at the caller (`pageNames` in
`gateway/common.go`, `clusterConfName` in `worker/clusterconf.go`), not by a
`model` parser. The families: `ClusterConfKey` and `ClusterConfPrefix`; the
three globals (`DnGlobalKey` and its siblings); the rev keys per shard,
`DnRevKey`, `DnRevPrefix` and `ParseDnRevKey` — likewise for `Cn*` and `Sp*`;
`DnConfKey` and `CnConfKey` with `DnConfPrefix` and `CnConfPrefix`, the
gateway's `ListDiskNodes` and `ListControllerNodes` page ranges (`gateway.md`
GW10), scanned by no worker; `DnCapacityKey`, `DnCapacityPrefix` and
`ParseDnCapacityKey` — MD5's scan reads the candidate out of the key itself —
and `CnCapacityKey`, `CnCapacityPrefix` and `ParseCnCapacityKey`;
`CdcEntryKey`, `CdcEntryPrefix` and `ParseCdcEntryKey`, a prefix and parser
pair no worker uses (like the three list-range prefixes): `dnv-cdc` scans and
watches the prefix and decodes the keys off it (`cdc.md`, The etcd watcher);
`SpConfKey`, `SpNameKey` and `SpConfPrefix`, the `ListStoragePools` page range
(`gateway.md` GW10), scanned by no worker; `CntlrKey` and `SliceKey`;
`ThinDeviceKey` and `SubsystemKey`; `CloneKey` and `CloneBitmapKey` — the
seven-field key, BOTH indexes rendered with `BmIdxFmt` —, `CloneBitmapPrefix`
and `ParseCloneBmKey`; `TransferKey`; `MigrationKey`, `MigrBitmapKey`,
`MigrBitmapPrefix` and `ParseBmIdx`, the migration-only six-field key;
`RepairKey` and `RetiredLegKey`, with `RepairPrefix` and
`RetiredLegPrefix`, the prefixes MD3's load ranges; `WorkerRegKey`,
`WorkerRegPrefix` and `ParseWorkerRegKey`.

`ClusterId` is the derivation of `architecture.md`, cluster_id derivation,
verbatim. Formats are exactly those of `architecture.md`, Key grammar:
`IdKeyFmt` for every id, `ShardCodeFmt`, `FreeSpaceFmt`, `BinIdxFmt`,
`BmIdxFmt`; a key is the fields joined by one space, and a prefix always ends
in one space.

A clone's skip bitmap is NOT one value per source slice: it is a set of chunks
addressed by the PAIR `(src_slice_idx, bm_idx)`. Chunk (*s*, *b*) holds the
bytes of source slice *s*'s bitmap from *b* times `CloneBmChunkBytes` on, at
most `CloneBmChunkBytes` of them, with *s* below the clone's `src_slice_cnt`
(itself at most `MaxSliceCntPerSp`) and *b* below `MaxCloneBmCnt` — the chunks
**per source slice** that constant bounds. A chunk's position is fixed by *b*
alone, so chunks are order-independent and gap-tolerant: an absent chunk, and
a short chunk's missing tail, read as *written* — the safe direction, the
region being copied rather than skipped. A migration's chunks keep their
six-field key and their flat append sequence. The two parsers are not
interchangeable — `ParseCloneBmKey` takes the seven-field `clone_bitmap` key,
`ParseBmIdx` the six-field `migr_bitmap` one, each rejecting the other's shape
— and a six-field key under the clone prefix is malformed: it parses as not
ok and is skipped by MD3's loader.

MD3. **SP snapshot.** `LoadSp` reads everything the sp role fans out or
reacts on, at **one** store revision, into an `SpState`. One `SnapshotRev`
(EU4) reads the `SpConf` and every sub-object it lists under a key of its
own, and fixes that revision; one range of each of the two prefixes
`RepairPrefix` and `RetiredLegPrefix` (`RangeAtRev`, EU2) then reads the
repairs and the retired legs at it, since a pool holds up to one repair and
`MaxRetiredLegPerGrp` retired legs per group, too many for a point read each
within the load's one budget (EU5); and a snapshot pinned to the same
revision (`SnapshotAtRev`, EU4) reads the node records last, because the
repairs' new legs and the retired legs name nodes too. The fields: `Rev`,
the store revision of the snapshot; `SpRevision`, the SP's `SpRev` revision
in that snapshot, zero when the key is absent; `Conf`; `Cntlrs` by cntlr id
from `cntlr_id_list`; `Slices` by slice id from `slice_id_list`; `Tds` in
`td_name_list` order, with `TdNames` naming each entry; `Subsystems` by nqn;
`Clones`, `Xfers` and `Migrs` by name; `Repairs` by name from
`repair_name_list` and `RetiredLegs` by leg id from `retired_leg_id_list`,
keeping only the keys a list names, since no writer leaves another under
either prefix and nothing could reach one; `CloneBmIdx` and `MigrBmIdx`, the
chunk addresses present (keys only) with their mod revisions, one `BmChunk`
per chunk carrying its slice index, its index and its `ModRev`, the slice
index always zero for a migration chunk; `DnByAddr`, the `DnConf` of every
side's `addr_port` of the SP, read in this order: the slices' sides, the
sides of every clone's clone meta group in `clone_name_list` order, a
latched clone's too, the sides of the repairs' new legs in
`repair_name_list` order and the retired sides in `retired_leg_id_list`
order; and `CnByAddr`, the `CnConf` of every cntlr's. A missing `SpConf`
returns `ErrNotFound` (the SP is being deleted; the rev key's delete
follows). A listed sub-object whose key is missing is reported in `Missing`,
in the order the load reads it — the `SpConf`'s sub-objects, then the
repairs and the retired legs, then the node records — and logged by the
caller; the load still succeeds. `SpRevision` is the revision the loaded
state belongs to, for RW14's revision guard; the rev key is not a listed
sub-object, so an absent one is no `Missing` entry. Bitmap **values** are
never loaded here — pushes read one chunk at a time (BM3). Bitmap indexes
come from a keys-only scan (`RangeKeysAtRev`, EU2) performed **outside** the
STM at the same store revision: the STM cannot range, and the pin is what
keeps the reads consistent — the set of indexes is not append-only, since
the clone drain deletes chunk keys, nor are the repairs and the retired
legs, so every scan is served at the snapshot's own revision and never at a
newer one. One loader serves both prefixes and forks on the key parser
(MD2): `ParseCloneBmKey` under a `CloneBitmapPrefix`, `ParseBmIdx` —
reported at slice index zero — under a `MigrBitmapPrefix`. A key the
prefix's own parser rejects is skipped silently, so one stray key of another
shape never fails a whole SP load.

MD4. **Capacity keys** (`architecture.md`, Capacity index keys, and
`architecture.md`, DN bins). `DnBinIdx` maps a free extent count to its bin
under the cluster's `DnBinConf`, reporting not ok below the smallest bin;
`DnAllocatable` and `CnAllocatable` implement the presence rule verbatim
(pointer-list cap, `err_epoch` set, `disabled`, free floor).
`MaintainDnCapacity` takes the `addr_port` as a parameter — a capacity key
embeds it and `DnConf`, which is keyed by it, does not carry it — and deletes
the key implied by the old record (if the old record was allocatable) and puts
the key implied by the new one (if allocatable; the value, `DnCapacityValue`,
holds the location and a copy of the record's `used_meta_units`);
`MaintainCnCapacity` likewise, with `CnCapacityValue`, which holds the
location alone. Both are idempotent and are called by every op that changes
an input of the rule, and `MaintainDnCapacity` also by every op that changes
a DN's used metadata units — a change of the units alone keeps the key where
it is and rewrites its value, units being no input of the rule
(`architecture.md`, Metadata units) — inside that op's STM. The old record
is the record as read in the same STM, which is what makes the delete
target exact. `DnFreeMetaUnits` gives a DN record's free units,
`DnCapacityFreeMetaUnits` a DN capacity value's — the ceiling less the used
units, none once they reach it — and `ReleaseMetaUnits` a count after a
release, which stops at zero.

MD5. **Allocator** (`architecture.md`, Finding DN candidates, and
`architecture.md`, Finding CN candidates, verbatim). `FindDnCandidates` scans
for a DN able to hold the requested extents, honouring the black and white
lists; its `candUnits`, zero for none, is the floor of free metadata units a
candidate's capacity value must still show (`architecture.md`, Metadata
units); its `excludeLocs` seeds the taken locations, so the caller's own
failure domains are excluded before the walk starts. `FindDnCandidatesAntiAffine` is
the two-tier rule of `architecture.md`, Per-operation allocation: tier 1 with
the exclusion; tier 2 rescans without it when tier 1 yields fewer than the
required count — the DNs the caller must actually place, never the oversampled
candidate count — and its candidates are merged **behind** tier 1's, so a
distinct-domain DN tier 1 found is never dropped; it reports whether tier 2
ran. `FindCnCandidates` scans for a CN outside the SP's own CN endpoints, its
`excludeLocs` working as for DNs and no unit floor, a CN counting no metadata
units, and its two-tier twin `FindCnCandidatesAntiAffine` runs tier 2 when
tier 1 finds no CN at all (every cntlr pick places one) and relaxes the
location exclusion only, never the SP's endpoints, the black list or the
white list. The worker's own DN scans, the grow's and the repair's for its
fresh new leg, ask for no units (AR6, AR8). A `Cand` carries the endpoint, the location, the free
extents and the bin index, and no units; `PickRandom` picks from a
candidate list. The scans are plain descending `Range`s **outside** any STM.
Because the picked node's free count is part of its capacity key, every op of
MD6 re-validates a pick by getting the exact capacity key the scan saw: gone
means an `ErrPrecondition` with `ReasonCandidateChanged`, on which the caller rescans
and retries the scan and the STM as one unit (the candidate unit of
`architecture.md`, Storage pools). `ReplaceCntlr` fails with the same reason
when the plan the scan ran against has moved: a surviving cntlr of the SP sits
on a CN outside the SP endpoints it is handed (MD6). `CreateRepair` with a
fresh new leg fails with it when the group holds a DN outside the group DNs
it is handed, and when its pick has gone stale in any other way since the
scan — its record deleted, the DN no longer allocatable, or short of
extents (MD6).

MD6. **Internal mutations.** Each is **one** `RunSTM` (`FlipCreated`: one per
`MaxFlipCreatedPerTxn` candidates, RW19), re-validates every precondition it
lists (MD7), and bumps exactly the revisions the equivalent RPC bumps
(`architecture.md`, `service Gateway` — RPC specifications, and
`architecture.md`, Workers). `now` is the caller's unix seconds; thresholds
are resolved inside from `SpConf.event_threshold` with the defaults of
`architecture.md`, Common validation. The shard and the sp id address the
`SpRev` key; the sp name the `SpConf`. The ops a gateway call runs are
shared mutations this worker runs too: `GrowSlice` (AR6); the repair ops
`CreateRepair`, `FinishRepair`, `CancelRepair` and `SetRepairAbort`, which
it runs on its own repairs (AR8); and `DeleteRetiredLeg`, which its retire
timer runs (AR12); save `ForceStartGroup`, the body of the gateway's call
of that name, which this worker never runs: its clear of the mark that op
writes is `ClearForcedStart` (AR13); and save `SetGrpOperatorInCharge`, the
body of `UpdateGroupOperatorInCharge`, which it never runs either: its pass
reads the flag that op writes (AR8). The ops a gateway call runs take an
expected revision: when non-zero the STM re-checks `SpRev.revision`
against it first (`ErrPrecondition` with
`ReasonStaleRevision` on mismatch); zero skips the check — the caller's
opt-out, which `gateway.md` GW6 defines for the public RPCs; this worker's
own calls pass zero, save where the worker's rule names
the revision it passes, as the retire timer's `DeleteRetiredLeg` passes
that of the snapshot it judged the leg on (AR12). The three ops that mint
ids — `GrowSlice`, `ReplaceCntlr` and `CreateRepair` — take them through
`TakeSpIds` and require the pool's `next_id` to have every id they take
left (`ReasonNoIdLeft` otherwise;
`architecture.md`, Globals: id allocation + shard buckets).

The clone-keyed twins — `SetCloneLegErrEpoch`, `SetCloneSideErrEpoch` and
`FlipCloneProvisioned` — apply the rules of their slice-keyed siblings to a
clone's clone meta group (`architecture.md`, Terminology and object
model), through functions of their own, so no slice-keyed op ever walks a clone and no clone-keyed op a
slice. A clone's record is keyed by its name, so each takes the clone's
name and its `clone_id`, and requires, as its clone checks, the `Clone` to
exist ("clone not found"), to carry that id ("clone id changed": a clone
deleted and re-created under the name is another clone) and to have a
group ("clone has no group"); group, leg and side ids are unique in the SP
and address the rest. No clone-keyed op takes a clone with no group; the
clone drain's batches and its final transaction take one all the same, so
a drain is never stranded on one. The sp worker writes the health epochs
of a clone meta group's legs and sides through the two epoch twins (HL2),
runs RW18's clone flip through `FlipCloneProvisioned`, and runs leg repair
on a clone meta group through the repair ops, which take a group of either
kind (AR8).

The repair and retired-leg ops are neither slice-keyed nor clone-keyed:
each takes a group of either kind (`architecture.md`, Repairs). A `Repair`
and a `RetiredLeg` record the parent of their group, the slice by its id
or the clone by its name, exactly one of the two (`GrpParent`).
`CreateRepair` takes the parent from its caller's look-up, slices first and
then clones; every later op that writes the group reads that one record,
checks that it holds the group, and puts it back where it found it;
`SetRepairAbort` reads the parent of either kind, since it refuses while
the group's forced-start mark names the repair's new leg (`architecture.md`,
Repairs); `SetRepairStage`
and `FlipRepairProvisioned`, which write only the `Repair`, read a clone
parent alone, for the refusal below, and the epoch writes read no parent.
The forced start's two ops take the group the same way: `ForceStartGroup`
the parent from its caller's look-up, as `CreateRepair` does, and
`ClearForcedStart` the parent the worker's pass found the group in
(`ForcedStartRef`); so does `SetGrpOperatorInCharge`, from its caller's
look-up. A group never moves to another parent, so the record
stays true for the group's life, and no repair op walks the slices or the
clones. Each names the check of the pool it makes.
Every op but the epoch writes makes the any-level SP checks: the SP exists
with its id and is not `deleting`, at any `sp_level`, so that an operator
can finish, abort, cancel and delete at the levels where reactions stop;
the sp drain deletes a deleting pool's repairs and retired legs
itself (SPD15). `CreateRepair` then applies its level gate, which depends
on the name (`CreateRepairLevelGate`; `architecture.md`, Repairs), and
`ForceStartGroup`'s write its own, the step that stops group arrays. The
three epoch writes take an SP that exists with its id, at any level and
while it is deleting, as the other epoch writes read their record whatever
its pool's state. A clone parent being deleted is refused by every repair
op, save the epoch writes, which accept it as their clone twins do, and
the flip and `ClearForcedStart`, which skip it: its group goes back with
its repair and retired legs in CLD9's transaction. A repair or a retired
leg the pool lists whose key is missing is refused ("repair record missing", "retired leg record
missing") by every op that reaches it through the pool's list, a lost
invariant key (`architecture.md`, Key table), never `ReasonRepairNotFound`
or `ReasonRetiredLegNotFound`, which name an object the pool does not
list; the flip, which reads the repair's key alone, skips it as gone.

* `SetDnErrEpoch` and `SetCnErrEpoch` require the record to exist. With a
  non-zero epoch they set it only when the stored value is zero (the threshold
  clock never restarts); with a zero epoch they clear it. They run
  `MaintainDnCapacity` or `MaintainCnCapacity` over the old and new records.
  **No rev bump** (`architecture.md`, Revision keys and the sync fan-out). No
  write when unchanged.
* `SetCntlrErrEpoch` requires the record to exist and applies the same
  set-or-clear rule on the `Cntlr`; when its settle argument is set and the
  epoch is zero, it also clears `settling` in the same STM (HL2) — a settle
  with a non-zero epoch clears nothing; no bump; no write when unchanged. A
  write that moves the cntlr into or out of the discovery listing
  (`architecture.md`, [D18]) — the `err_epoch` of an enabled standby set or
  cleared — also rewrites the SP's `CdcEntry` records in the same STM. The op
  finds the SP by its id, through its `SpName` record to a `SpConf` that must
  still carry that id; an SP whose records are gone, or whose name another SP
  holds, has none to rewrite, and the cntlr is written alone. This rewrite,
  and those of `Failover` and `ReplaceCntlr`, sets every entry's
  `nvme_tr_conf_list` by the listing rule from the cntlrs the STM reads, its
  own writes included, and puts only an entry whose list changes. A
  `Subsystem` of `nqn_list` that is missing fails the op, because the
  entry's key needs its `ss_id` and an op never writes half of what it owes;
  a missing `CdcEntry` is skipped, where the gateway's writers rebuild it
  (`architecture.md`, [D18]).
* `SetLegErrEpoch` and `SetSideErrEpoch` require the slice to exist and the
  leg or side to be found in any group's `leg_list`; the same rule on the
  embedded record; they rewrite the `Slice`; no bump.
* `SetCloneLegErrEpoch` and `SetCloneSideErrEpoch` require the clone checks
  and the leg or side to be found in the clone's group, in its `leg_list`;
  the same rule. They accept a `deleting` clone too: its
  legs and sides stay in its record, with their health epochs, until CLD9's
  transaction removes them. They rewrite the `Clone`; no bump.
* `FlipProvisioned` takes a list of `SideRef`s and requires the slice to
  exist: every listed side still `provisioned` false is set true; `SpRev` is
  bumped once iff any was written. It returns the sides actually flipped and
  the revision its bump wrote, so the `flip applied` record can name each,
  at that revision (RW18; Log records).
* `FlipCloneProvisioned` takes a list of `CloneSideRef`s (the clone's name
  and `clone_id`, the leg id and the side id) and flips as
  `FlipProvisioned` does: each `Clone` read once and written at most once,
  every listed side still `provisioned` false set true, `SpRev` bumped once
  iff any was written, and the sides flipped returned with that revision.
  Unlike a missing slice, a clone that is gone, carries another `clone_id`,
  has no group or is `deleting` is skipped with its sides and fails nothing:
  a clone leaves its pool in normal operation, through the clone drain, and
  failing the batch would drop every other clone's flip, while a `deleting`
  clone's sides go back with its record (CLD9). Its one STM, when it flips
  a side of each of `MaxCloneCntPerSp` clones, is past etcd's default
  transaction cap (Constants this document owns).
* `FlipCreated` takes a list of `TdRef`s (name, td id and the torn mark the
  reports carried) and commits them
  `MaxFlipCreatedPerTxn` at a time in list order, one STM each (RW19): it
  skips a candidate whose key is absent, whose `td_id` differs, or that is
  already `created`; sets the rest — `created`, and `torn` as the ref
  carries it, in the same put, so the mark is written once and a mark that
  comes after the flip changes nothing; each STM bumps once iff it wrote; it
  returns the tds actually flipped, as above — on an error, those of the STMs
  that committed before the failing one, which the sp worker does not log
  (RW19).
* `StartCutover` takes one migration — its name and `migr_id`, the slice
  and the leg that hold its two sides, and the two side ids — and the cntlr
  whose report it acts on. It requires the SP's `SpConf` to hold the SP's
  id and an `sp_level` below `SP_LEVEL_NO_MIGRATION`, the `Migration` to
  exist with that `migr_id` and destination side and with
  `cutover_pending` set, the destination side in that leg of that slice
  and `provisioned`, and the cntlr to exist, be `primary` and not be
  `disabled`; it clears `cutover_pending`, bumps `SpRev` once and returns
  the revision its bump wrote (RW18; `architecture.md`, Migration). The
  reports it acts on are no part of the store, so the caller judges them.
  The level is the store's, so the transaction reads it: the destination's
  report that it waits for the cutover can come from before a raise of the
  level, and at `SP_LEVEL_NO_MIGRATION` and above the destination builds
  no dm-clone (`dnagent.md` DN11), so a cutover committed there would fence
  the source with no side left to serve the leg. A disabled primary's path
  counts for nothing (RW18), so the transaction reads `disabled` too, for a
  disable that commits after the report.
* `Failover` requires the SP not `deleting` and its `sp_level` below
  `SP_LEVEL_NO_THINPOOL`; the old cntlr `primary`; and, unless the old cntlr
  is `disabled` (a disabled primary is the AR5 trigger on its own,
  `architecture.md`, Cntlrs, and waits out no threshold), its `err_epoch` set
  (`ErrPrecondition` "old cntlr is healthy and enabled") and `now` minus that
  epoch at least the threshold — `cntlr_unhealthy` when the old cntlr is
  `settling` and `cntlr_unhealthy` exceeds `primary_unhealthy`, else
  `primary_unhealthy` — "primary_unhealthy not reached" resp. "cntlr_unhealthy
  not reached for a settling primary" (AR5); the new cntlr is not `primary`,
  not `disabled`, has `err_epoch` zero **and** has the smallest `cntlr_id`
  among all such cntlrs. It flips both `primary` booleans, sets the new
  cntlr's `settling` and clears the old one's (HL2), rewrites the SP's
  `CdcEntry` records as `SetCntlrErrEpoch` does — which takes an enabled old
  primary with an `err_epoch` out of them, until its next clean round lists
  it again as a standby (HL2), while a disabled one left them with its
  disable — and bumps `SpRev` (`architecture.md`, Automatic reactions).
  The own verdict AR10 asks for is not re-checked: no STM sees it (AR5).
* `GrowSlice` takes the slice, the kind (meta or data), a pool total, the
  cluster conf and the picked legs, and requires the SP checks above; the
  expected revision as in the preamble; the SP's `bdev_conf` and the cluster
  conf both valid (`architecture.md`, Common validation — the two checks sit
  at the top of the STM, ahead of its first put, so a refusal aborts with
  `ErrPrecondition` and commits nothing); the slice to exist; the slice's list
  of that kind below `MaxGrpCntPerSlice` groups (`GrpListFull`, reason
  `grp_list_full` — `architecture.md`, md names, and `architecture.md`,
  GrowSlice; checked ahead of the sizing, so it holds for a meta grow whatever
  the ladder says); the meta ladder not at its cap; no grow of that kind
  pending — AR6's rule re-applied in-STM, judged against the pool total handed
  in (the worker passes the primary's reported total; a user-driven grow is
  not so gated, `architecture.md`, GrowSlice, and what the gateway passes is
  `gateway.md`, Storage pools and GrowSlice);
  every picked DN allocatable, with at least the new group's `ext_cnt` free
  and its capacity key unchanged; every cntlr's CN with at least that
  `ext_cnt` free. Its `ext_cnt` is the first data group's (for a data grow) or
  the ladder value (`architecture.md`, GrowSlice); `meta_blocks` and
  `data_blocks` follow `architecture.md`, Group on-leg layout: meta region,
  data region, health block, with the SP's `block_size` and
  `bitmap_chunk_block_cnt` and the cluster's `extent_size`, each used as
  stored; ids come from `SpConf.next_id`; the new `Group` carries one `Leg`
  plus `Side` per pick (`leg_idx` from zero, `cntlid_slot` the first of
  `cntlid_slot_list`, `provisioned` false, `addr_port` and `nvme_tr_conf` from
  the DN); DN bookkeeping (`side_ptr_list`, `free_ext_cnt`, capacity, a
  `DnRev` bump each); CN budgets (`free_ext_cnt`, capacity, a `CnRev` bump
  each); the `Slice` and the `SpConf`; a `SpRev` bump.
* `ReplaceCntlr` takes the old cntlr, the picked CN, the SP's CN endpoints the
  scan ran against, whether the new cntlr is primary and `now`, and requires
  the SP checks; the old cntlr's `err_epoch` set and `now` minus it at least
  `cntlr_unhealthy`, and the old cntlr not `disabled`; if the old cntlr is
  `primary`: the new one requested primary and no failover candidate existing;
  every cntlr of the SP but the old one on a CN among the handed endpoints,
  the plan the pick was scanned against (else `ReasonCandidateChanged`, MD5: a
  cntlr committed after the caller's snapshot, whose CN and domain the scan
  could not exclude; only a gain is checked, and ahead of "not hosting a cntlr
  of this SP"); the picked CN allocatable with free extents at least the SP
  footprint (the sum of `ext_cnt` over all groups of its slices), not hosting
  a cntlr of this SP, its capacity key unchanged. It deletes the old `Cntlr`
  (its CN, if the record still exists: pointer out, footprint back,
  capacity, `CnRev`); creates the new `Cntlr` with the old one's
  `cntlid_slot`, `primary` as requested, `disabled` false and `settling` equal
  to `primary` (HL2), its `cntlr_id` the next id (new CN: pointer in,
  footprint out, capacity, `CnRev`); rewrites the SP's `CdcEntry` records as
  `SetCntlrErrEpoch` does, which drops the old cntlr and lists the new one;
  the `SpConf`; one `SpRev` bump for both halves. It reads no clone: a clone
  charges no CN (`architecture.md`, Metadata units), so its STM stays under
  etcd's default transaction cap at the ceilings (`gateway.md`, Constants
  this document owns). Nor is AR10's own verdict re-checked here (AR7).
* `CreateRepair` takes the repair's name, the group's parent and id, the
  bad leg, the new leg's source (`RepairNewLeg`: a retired leg of the
  group, or a picked DN with the group's DNs the pick was scanned against)
  and the cluster conf, and requires the expected revision as in the
  preamble; the any-level SP checks and the name's level gate; the pool
  `RedundMdRaid1`; the name in neither `repair_name_list` nor a key
  (`ReasonRepairExists`); the parent to hold the group, a clone parent not
  `deleting`; for a name with the reserved prefix, the group's
  `operator_in_charge` flag clear (`ReasonGrpOperatorInCharge`), an
  operator's name never refused for it; the group with no repair
  (`ReasonGrpHasRepair`); the bad leg in `leg_list` with
  exactly one side ("bad leg has no single side": a second side is a
  migration's destination, and a migration holds its leg) and its
  `err_epoch` set ("bad leg is healthy: use a migration"). From a retired
  leg it requires a name without the reserved prefix, since the worker's
  repair always takes a fresh leg; the leg listed in the pool's and the
  group's lists and of this group, with a side whose `err_epoch` is clear,
  on a DN that holds no leg of the group but the bad one; it deletes the
  `RetiredLeg` key and takes it out of both lists, the leg keeping its leg
  id, its `leg_idx` and its side, the repair keeping its `retired_epoch`,
  and takes one id, the `rep_id` — at `MaxRetiredLegPerGrp` too, since the
  finish then retires one leg as this took one. A fresh leg under an
  operator's name requires the group below `MaxRetiredLegPerGrp` retired
  legs (`ReasonRetiredListFull`); under the worker's name it is allowed at
  that bound, since its finish gives the oldest back. A fresh leg then
  checks the plan before the pick: a DN of the group's DN set
  (`architecture.md`, Per-operation allocation) outside the handed DNs is
  `ReasonCandidateChanged` (MD5), a gain since the caller's snapshot that
  its scan could not exclude, checked ahead of the address-based "dn
  already in the group", which then fires only for a pick inside the
  caller's own plan; and the pick is re-validated with `checkDnPick`'s
  `vanishedIsChanged`, a DN deleted, no longer allocatable or short of
  extents since the scan being `ReasonCandidateChanged` too. It takes three
  ids through `TakeSpIds` — the `rep_id`, the leg id and the side id, in
  that order —, gives the `Leg` the next unused `leg_idx` of the group and
  a `Side` (`provisioned` false, `cntlid_slot` the first of
  `cntlid_slot_list`), and does the DN bookkeeping plus `DnRev`. Either way
  it writes the `Repair`, `REP_STAGE_CREATED` with its flag clear and its
  parent's field set; the parent with the group's `rep_name`; the `SpConf`
  with the name listed; a `SpRev` bump; and returns the `rep_id` and the
  new leg's id. No metadata units, no CN.
* `FinishRepair` takes the name, `now` and the cluster conf, and requires
  the cluster conf valid (`architecture.md`, Common validation, checked
  first, so a refusal writes nothing); the expected revision; the
  any-level SP checks; the repair listed and its key present;
  `REP_STAGE_STARTED`, any other stage refused with its name; its parent
  holding its group, a clone parent not `deleting`; the group naming the
  repair; one bad leg, in `leg_list` with one side, and one new leg. The new
  leg takes the bad leg's position in `leg_list` and its `leg_idx`, and
  keeps its own leg id, side and `err_epoch`. The bad leg becomes a
  `RetiredLeg` under its own leg id — its side as it is, which keeps its
  pointer and its extents on its DN, the group's id, its `leg_idx`, the
  repair's parent and `now` as its `retired_epoch` —, listed in the group's
  and the pool's `retired_leg_id_list`; the bad leg's own `err_epoch` goes
  with it. The `Repair` key and its list entry go, the group's `rep_name`
  is cleared, and so is the group's forced-start mark when it names the
  bad leg, which the finish retires, one that names the new leg staying;
  and `SpRev` is bumped. For a repair whose name starts with
  the reserved prefix, on a group that holds `MaxRetiredLegPerGrp` retired
  legs, the same STM first gives back the group's oldest retired leg: the
  one with the smallest `retired_epoch` and then the smallest leg id among
  the records the group lists, each read in the STM, listed in the pool's
  list and naming the group; its side goes back to its DN as
  `CancelRepair` gives a fresh side back, and its `RetiredLeg` key and its
  two list entries go. It returns the `rep_id`, the retired leg's id and
  the given-back leg's id, zero when it gave none back
  (`FinishRepairResult`). The group's retired legs never pass
  `MaxRetiredLegPerGrp` here: an operator's fresh new leg was refused at
  the bound, a new leg taken from a retired leg left the group one retired
  leg fewer, which the finish's retired leg makes up, and the worker's
  finish gives one back first.
* `CancelRepair` takes the name and the cluster conf, and requires the
  cluster conf valid (`architecture.md`, Common validation, checked first,
  so a refusal writes nothing); the expected revision; the any-level SP
  checks; the repair listed and its key present; `REP_STAGE_ABORTED`, any
  other stage refused with its name; its parent holding its group, a clone
  parent not `deleting`; the group naming the repair. A new leg taken from
  a retired leg goes back as that `RetiredLeg`, with the `retired_epoch`
  the repair kept, its own `leg_idx`, its side as it is now and the
  repair's parent, listed again in both lists; a fresh new leg's side goes
  back to its DN as `DrainSpSlice` gives a popped group's back: per
  DISTINCT DN the group's `ext_cnt` back, pointer out, capacity, one
  `DnRev` bump, a DN whose record is gone skipped. The `Repair` key and its
  list entry go, the group's `rep_name` is cleared, and `SpRev` is bumped;
  it returns the `rep_id`.
* `SetRepairAbort`, the body of `TryToAbortRepair`, takes the name and
  requires the expected revision; the any-level SP checks; the repair
  listed and its key present; a clone parent existing and not `deleting`;
  `REP_STAGE_CREATED`, any other stage refused with its name; and, for a
  flag not yet set, its parent holding its group and the group's
  forced-start mark naming no new leg of the repair, which the flag would
  take out of the members the forced start keeps (`architecture.md`,
  Repairs). It sets `try_to_abort` and bumps `SpRev`, and returns the
  `rep_id`; a flag already set is answered with no write and no bump.
* `SetRepairStage` takes the name, the `rep_id` the primary's verdict named
  and the stage to move to, and no expected revision: the worker converges
  on what the store holds. It requires the any-level SP checks; the repair
  listed and its key present; a clone parent existing and not `deleting`;
  that `rep_id` ("repair id changed": a repair finished or cancelled and
  created again under the name is another repair); `REP_STAGE_CREATED`; a
  move to `REP_STAGE_STARTED`, or to `REP_STAGE_ABORTED` with the stored
  flag set ("abort flag not set"), any other stage being "not a stage
  move". It writes the `Repair` and bumps `SpRev`, since the cn agent acts
  on the stage (`architecture.md`, Repair).
* `DeleteRetiredLeg` takes the leg id and the cluster conf, and requires
  the cluster conf valid, checked first; the expected revision; the
  any-level SP checks; the retired leg listed and its key present; its
  parent holding its group and the group listing it, a clone parent not
  `deleting`. Its side goes back to its DN as `CancelRepair` gives a fresh
  side back; the `RetiredLeg` key and its two list entries go; a `SpRev`
  bump. The operator's delete and the retire timer run it alike, and
  `FinishRepair` gives a retired leg back the same way for the worker's
  repair at the bound.
* `SetRepairLegErrEpoch` and `SetRepairSideErrEpoch` take the repair's name
  and its `rep_id`, which proves the record is the one whose leg the
  caller observed, and a new leg's or its side's id; `SetRetiredSideErrEpoch`
  takes a retired leg's id and its side's id. Each requires the SP to exist
  with its id, at any level and while it is deleting; the record listed,
  its key present and, for the first two, carrying that `rep_id` ("repair
  id changed"); and the leg or side in it ("leg not found", "side not
  found"). The same set-or-clear rule; they rewrite the `Repair` or the
  `RetiredLeg`; no bump; no write when unchanged.
* `FlipRepairProvisioned` takes one `RepairSideRef` — the repair's name and
  `rep_id`, the new leg's id and the side's id — and requires the any-level
  SP checks. It skips, and fails nothing, a repair that is gone or carries
  another `rep_id`, a clone parent that is gone or `deleting`, a leg or a
  side the repair does not hold, and a side already `provisioned`, as
  `FlipCloneProvisioned` skips a clone that left; otherwise it sets the
  side `provisioned`, bumps `SpRev` once and returns that it flipped and
  the revision its bump wrote. One side per call: a repair is a record of
  its own, so a flip over every repair of a pool would be as wide as the
  pool's groups.
* `ForceStartGroup`, the body of the gateway's call of that name, takes the
  group's parent and id, the leg and the cancel flag, and requires the
  expected revision; the any-level SP checks; for a write, an `sp_level`
  below `SP_LEVEL_NO_REDUND` ("sp level builds no group array"); the pool
  `RedundMdRaid1`; the parent holding the group, a clone parent not
  `deleting`. A write requires the leg to be one the mark may name
  (`architecture.md`, Repair, Forced start), a leg of the group's
  `leg_list` other than the bad leg of a STARTED repair, or a new leg of
  the repair the group names while it is STARTED or is CREATED with its
  abort flag clear (`ReasonLegNotNamable`), the repair read through the
  pool's list as every op reads one whenever the group names one; it sets the
  group's `forced_start` to the leg and the revision its own `SpRev` bump
  writes,
  replacing a mark that stands, and returns that revision. A cancel clears
  the mark and bumps `SpRev` and returns zero, and a group with no mark is
  answered with no write and no bump.
* `ClearForcedStart` takes a `ForcedStartRef` — the group's parent and id,
  and the leg and the revision of the mark the caller judged — and no
  expected revision: the worker converges on what the store holds. It
  requires the any-level SP checks. It skips, and fails nothing, a parent
  or a group that is gone, a clone parent `deleting`, and a mark that is
  gone or names another leg or revision, replaced or withdrawn since;
  otherwise it clears the mark, bumps `SpRev` once, so the fan-out hands
  every cntlr the group without it, and returns that it cleared and the
  revision its bump wrote (AR13).
* `SetGrpOperatorInCharge`, the body of `UpdateGroupOperatorInCharge`, takes
  the group's parent and id and the flag's value, and requires the expected
  revision; the any-level SP checks; the pool `RedundMdRaid1`; the parent
  holding the group, a clone parent not `deleting`. It sets the group's
  `operator_in_charge` to the value and writes the parent with no `SpRev`
  bump, as the call's spec says (`architecture.md`, Repairs); a group whose
  flag already holds the value is answered with no write.
* `DrainSpCntlrs` requires the drain checks of SPD2 (`SpConf` exists, `sp_id`
  unchanged, `deleting` true — `sp_level` is deliberately not consulted) and
  every listed `Cntlr` key to exist. It deletes every `Cntlr`; per DISTINCT CN
  the SP footprint back, pointer out, capacity, one `CnRev` bump (a CN whose
  record is gone is skipped, as in `ReplaceCntlr`); the `SpConf` with an empty
  `cntlr_id_list`; a `SpRev` bump; it returns the count removed. An
  already-empty list is a no-op that writes and bumps nothing.
* `DrainSpRepairs` takes a batch of repair names and the cluster conf, and
  requires the cluster conf valid, checked first; the drain checks;
  `cntlr_id_list` empty; at most `MaxDelRepPerTxn` names ("batch too
  large"); every listed name's key present ("repair record missing"); all
  of one parent ("batch spans two parents"), which must hold each group. A
  name the pool no longer lists, or one named twice, is skipped. It reads
  the parent once, gives every new leg's side back as `DrainSpSlice` gives
  a popped group's, by its group's `ext_cnt`, clears each group's
  `rep_name`, deletes each `Repair` key with its `repair_name_list` entry,
  bumps `SpRev`, and returns the count removed; a batch left empty writes
  and bumps nothing. `SpDrainRepairBatch` derives the batch from a
  snapshot: the repairs, in `repair_name_list` order, that share the parent
  of the first listed one, at most `MaxDelRepPerTxn`; a first listed one
  the snapshot does not hold is a batch of its own, which the op refuses
  (SPD15).
* `DrainSpRetiredLegs` and `SpDrainRetiredLegBatch` are their twins for
  retired legs: at most `MaxDelRetiredLegPerTxn` leg ids of one parent, the
  op also requiring `repair_name_list` empty, each side given back by its
  group's `ext_cnt`, and each `RetiredLeg` key deleted with its entries in
  the group's and the pool's `retired_leg_id_list`.
* `DrainSpSlice` takes the slice and the cluster conf and requires the drain
  checks; the cluster conf valid (`architecture.md`, Common validation, for
  `MaintainDnCapacity`'s ladder); `cntlr_id_list`, `repair_name_list` and
  `retired_leg_id_list` empty — a repair's or a retired leg's side goes back
  by its group's `ext_cnt`, which the pop takes away (SPD15); the slice key
  to exist. It pops up to `MaxDelGrpPerTxn` groups from the TAIL of
  `data_grp_list`, then of `meta_grp_list`; per DISTINCT DN every popped
  side's group `ext_cnt` back, pointer out, capacity, one `DnRev` bump (a DN
  whose record is gone is skipped); if both lists are now empty it deletes the
  `Slice` key AND removes the id from `slice_id_list` in the same STM, else
  puts the shrunken `Slice`; a `SpRev` bump; it returns the count removed and
  whether the slice is done. A slice id no longer listed is a no-op.
* `FinishSpDelete` requires the drain checks; `cntlr_id_list`,
  `repair_name_list`, `retired_leg_id_list` and `slice_id_list` all empty;
  `SpRev` and `SpGlobal` to exist. It deletes
  `SpConf`, `SpName` and `SpRev` and decrements the shard's entry of
  `SpGlobal.shard_bucket`. It is the ONE op that does not bump `SpRev` — it
  deletes the key, which is the shard worker's stop signal (RW14).
* `DrainCloneBm` takes the clone and a list of chunks and requires the clone
  drain checks of CLD2 (`SpConf` exists and is not itself deleting, `sp_id`
  unchanged, `Clone` exists, `clone_id` unchanged, `deleting` true) and at
  most `MaxDelBmPerTxn` chunks. It deletes each named `CloneBitmap` key —
  nothing else, and nothing the caller did not name; the `Clone` record is NOT
  rewritten, and `SpRev` is neither read nor bumped (CLD8). Its return is the
  SIZE of the batch it was handed, never a count of keys that were still
  there — a delete is an idempotent pop, so the loser of an accepted
  two-owner overlap reports a full batch and removes nothing. An empty batch
  is a no-op that writes nothing.
* `FinishCloneDelete` takes the clone and the cluster conf, and requires
  the cluster conf valid (`architecture.md`, Common validation, for
  `MaintainDnCapacity`'s ladder, checked first, so a refusal writes
  nothing) and the clone drain checks. It deletes the `Clone` key AND
  removes the name from `clone_name_list` in the same STM; gives every side
  of the clone's group's legs back as `DrainSpSlice` gives a
  popped group's: per DISTINCT DN the group's `ext_cnt` back, pointer out,
  capacity, one `DnRev` bump, a DN whose record is gone skipped, and a
  clone with no group giving nothing back; deletes the `Repair` the group's
  `rep_name` names and every `RetiredLeg` its `retired_leg_id_list` names,
  with their entries in the pool's lists, giving their sides back the same
  way while the group's `ext_cnt` is at hand, a named record whose key is
  missing refused; a `SpRev` bump (the SP outlives the clone, so this one
  bumps). It reads no `Cntlr` and no `CnConf`: a
  clone charges no CN.

MD7. **`ErrPrecondition`.** A struct carrying the op and the reason;
returned from inside the STM callback, it aborts without commit (EU4). An
op never writes partially (`FlipCreated` is the exception: its STMs, RW19,
each commit whole, and an error in one leaves those before it committed),
never sleeps, and never retries a precondition failure — the caller logs
the refusal with its reason (Log records) and re-evaluates on its next
pass.

MD8. **Gateway reuse.** This rule describes the gateway and names no decision
of this document: the public RPCs of `architecture.md`, `service Gateway` —
RPC specifications, are the same STM bodies plus request validation
(`architecture.md`, Common validation), the public preconditions
(`gateway.md`, Handlers by resource group) and reply mapping; the gateway
adds those on top of `model` rather than duplicating the bodies.

MD9. **Tests.** `model`'s unit tests are colocated; the etcd-backed ones —
the allocator's walks and every MD6 op's transaction — run against the real
etcd binary of EU7, and without one the package's tests fail as EU7 says.

## `cmd/dnv-worker`

CM1. **Flags.** One cobra root command, no subcommands; every flag is also a
config key and an environment variable under the prefix `DNV_WORKER_`. The
flags name the etcd endpoints (required), the roles this process carries
(every role by default, each registered and driven independently, VW10), the
two vote timers (VW2, VW5), the etcd dial timeout (EU1) and an optional viper
config file; `cmd/dnv-worker` holds their names and defaults. The two vote
timers exist as flags so the worker suite can run membership cases in
seconds; production runs the defaults.

CM2. **Binding.** As `cmd/dnv-agent` binds (`dnagent.md`, `cmd/dnv-agent` —
cobra + viper): the flags bound through viper, the environment prefix, a
dash-to-underscore key replacer, automatic environment lookup, `--config`
read when set; required values are checked after binding so a file or the
environment satisfies them.

CM3. **Validation.** `--etcd-endpoints` non-empty; `--roles` a non-empty,
duplicate-free subset of the three roles; both timers positive;
`--vote-grace-time` SHOULD exceed twice `--vote-interval` (a warning is
logged otherwise — a grace window not longer than the dead threshold is
legal but pointless, and with one not longer than one vote interval the
peers of a worker cut off from etcd can commit it dead about when it fences
itself, or before, since VW8 (a) waits one interval past the dead
threshold).

CM4. **Startup.** Install the default JSON logger (`common`'s `init`), mint
a startup trace id, build the `etcdutil` client (EU1), then call
`worker.Run` with the client and a `Config` carrying the endpoints, the
roles and the two timers (the endpoints feed the `worker starting` record's
`endpoints` attribute). `Run` starts the `ClusterConf` cache (RW21) and the
vote worker and blocks until the ctx ends. Startup never fails because etcd
is unreachable: the vote layer stays fenced (VW8) and retries every
interval, logging each failure. Only configuration errors make the process
exit non-zero at start.

CM5. **Shutdown.** SIGINT and SIGTERM cancel the ctx; `Run` then leaves the
fleet in the order a fence leaves it, and for the same reasons (VW8), but
mints no new seed and does not rejoin: it stops the heartbeat loop and
joins it; tells every shard worker to stop, gracefully and in parallel (SW5
then RW11 — an in-flight `Syncup*` or `Push*` is allowed to finish),
without waiting for them; **deletes its own registrations** (best effort,
one `Delete` per role under `DefaultEtcdOpTimeout`, so peers start their
grace windows now rather than after the dead threshold); joins the shard
workers, which can take up to `DefaultWorkerSyncupTimeout`; closes the cache
watch and the client; returns nil. A second signal during that window exits
immediately with status 1. Both ways out share this one order. A last
`Syncup*` or `Push*` can still finish after the deletes, possibly after a
peer has taken its shard: the overlap an ownership change accepts (VW7).

CM6. **Logging.** JSON on stderr, Info level by default (`log.md`, R2 and R6).
The `worker starting` record carries `roles`, `seed`, `endpoints`,
`vote_interval` and `grace_time`; `worker stopping` carries `seed`. Every
round, syncup, push, flip and reaction runs under a trace id minted per RW10,
so the seed prefix in an agent's `trace_id` attributes the request to this
process. The parts of an incarnation that outlive one unit of work hold an
id of that shape for their life (RW10), so the worker's own records carry
the seed prefix too, all but those written under CM4's startup trace id,
such as the process's start and stop records, the `ClusterConf` cache's
(RW21) and those of CM5's deletes.

CM7. **Wiring.** The worker binary links the etcd client; `dnv-agent` and
`dnvctl` link none (`layout.md`, Dependency rules).

## The vote worker — `worker/vote.go`

One vote worker per process. It owns the seed, the heartbeat loop, one
registry watch per configured role (VW3, VW10), the per-registration state
machines, the effective membership of every role, the ownership
computation, and the lifecycle of the shard workers. Its terms: a
**registration** is the key `WorkerRegKey` names for one role and one seed,
refreshed every vote interval (VW2); the **observed state** of a
registration is *live* or *dead*, as judged by one observer (VW3); the
**effective membership** of a role is the set of registrations an observer
has *committed* after a full grace window (VW5), and ownership is computed
from it, never from the raw observed set (VW11). Membership rests on
heartbeat registrations and no etcd lease (`architecture.md`, [D17]): a
worker re-puts its registration every vote interval, and a registration not
refreshed for the dead threshold is dead.

### Identity

VW1. The **seed** is a v4 uuid: random bytes from `crypto/rand` with the
version and variant bits set, rendered canonically in lower-case hex — no new
dependency (the repo already shapes uuids by hand in `common.NvmeHostId`). A
seed identifies one **incarnation** of the vote layer: a process start, and
every rejoin after a fence (VW8), mints a new one; nothing is persisted. All
roles of one process share the incarnation's seed.

### Registration and heartbeat

VW2. For every configured role the worker keeps `WorkerRegKey` of the role
and its seed refreshed: the value is a `WorkerReg` carrying the current
unix seconds as `epoch`, re-put every `--vote-interval` from a ticker, each
put under `DefaultEtcdOpTimeout`. The **first** put of every role happens
before the worker observes anything (VW3), so its own registration is part
of its first scan or its first watch events. A failed put is logged by the
`etcd put` record and retried at the next tick. The loop records
`lastOkPut`, the monotonic time of the last tick at which **every** role's
put succeeded (VW8 (a)).

### Observation

VW3. Per role: one `Range` of `WorkerRegPrefix`, then a `WatchTyped` from
the scan's revision plus one, decoding `WorkerReg`. For every registration
key the observer keeps `lastSeen` — the monotonic time of the last put it
observed (the scan time for a put it learned of from a scan) — the key's
mod revision as last observed (a put event's revision, a scan's `ModRev`,
EU2 and EU3), and the observed state, live or dead:

* a key found by a scan: `lastSeen` becomes now and the key is observed
  live — an **appear** transition if it was not already observed live —
  unless the observer already saw the key at the mod revision the scan
  reports: then nothing was put since, and the scan changes nothing
  (below);
* a put event: `lastSeen` becomes now; if the key was observed dead it is
  now live (appear or reappear);
* a delete event: observed dead (disappear) at once;
* a deadline at `lastSeen` plus the dead threshold, re-armed by every put:
  when it fires with no put in between, observed dead (disappear).

The worker's own keys are tracked exactly like everyone else's. After a
compaction or any watch error the role rescans: keys present that the
observer does not track, or whose mod revision moved since it last saw
them, get `lastSeen` now (a key already observed live has no transition);
keys present at the mod revision already seen are left as they are — live
with their deadline still running, or dead — because nothing expires a
registration (VW6): a scan finds a dead worker's key as surely as a live
one's, and a rescan that counted it as a put would re-arm the deadline of
every dead peer, so a watch that keeps failing faster than the dead
threshold would keep them alive for good; keys that were live but are
absent from the rescan transition to dead; the watch restarts from the
rescan's revision plus one. A scan that fails, the first or a rescan, is
tried again one `--vote-interval` later — never a hot loop — and the role's
watch restarts only from a scan that succeeded, since its revision is where
the watch resumes.

VW4. The stored `epoch` is **never** compared with local time. Liveness
depends only on the observer's own monotonic clock and the events it saw; the
epoch is informational, what `workerctl list-workers` and operators read
(`architecture.md`, [D17]). Clock agreement across workers (NTP) is an
operational assumption, not a correctness dependency: the vote worker reads
only its own clock, and a skew between workers shifts, by at most its size,
the moment a reaction measured from an `err_epoch` another worker stamped
fires.

### Grace and effective membership

VW5. Per registration the observer also keeps the effective state, member or
nonmember (initially nonmember), and at most one **pending** grace timer with
a target state. On every observed transition of a key: cancel the key's
pending timer; let the target be member if the key is now observed live, else
nonmember; start a new timer of `--vote-grace-time` with that target — on
**every** transition, even when the target already equals the key's effective
state. Every observed transition of one registration — appear, disappear,
reappear — therefore starts that registration's own timer, and the change is
committed only if it still holds when the timer fires: a flapping worker never
becomes effective and never blocks others. The always-arm rule is what VW7's
never-became-effective disappear case relies on: a worker that appears and
dies inside its own grace window must still get a commit, because the commit's
VW6 garbage collection is the only thing that ever removes a dead key
(`architecture.md`, [D17]: no lease) — a guard that skipped a timer whose
target already matched would leak that key and its tracking entry forever.
When a timer fires, its target is **committed** (VW6); a commit whose target
already equals the committed state changes no membership, logs nothing and
recomputes no ownership — it is a no-op apart from the VW6 collection. A
registration that flaps faster than the grace time therefore still never
changes anybody's effective membership, and never delays the commit of any
other registration. One commit is never a collection: a nonmember target for
the observer's **own** still-heartbeating key takes VW8 (d)'s fence exit
instead of deleting a live worker's registration.

VW6. **Commit** of a key and a target: set the key's effective state to the
target; log `membership committed`; if the target is nonmember: issue a
best-effort `Delete` of the key — the garbage collection of a dead
registration, performed by every observer that commits it (idempotent),
and the only thing that ever removes a key whose owner died, since without
a lease nothing else would expire it — and drop the key's tracking entry (a
later put creates a fresh entry with an appear transition). Then recompute
ownership for the role (VW9).

VW7. **Startup.** The effective membership of every role starts **empty**,
and every key found by the first scan — the worker's own included — enters
through an appear transition at scan time. The rule is symmetric: a worker
applies the same grace to its own registration. Consequently a fresh worker
drives nothing for its first grace window, even when it is the only worker;
the fleet's old and new owners of a shard switch within seconds of each
other (they all started the same timer for the same key at about the same
time); and a key found by the scan whose owner is already dead never
becomes effective — its disappear at the scan time plus the dead threshold
cancels the pending appear and starts a disappear timer whose commit is a
no-op except for the VW6 garbage collection. The residual overlap and gap
of an ownership change are accepted: every `Syncup*` is idempotent under
the agents' revision gate, every etcd reaction is STM-guarded (AR2), and
an `err_epoch` the
overlap leaves stale is corrected by the owner's first verdict after it
next loads the record — within a pass and a round for a side, a leg or a
cntlr, within a `nodeRecordMaxAge` and a round for a DN or a CN (HL3) —
while no threshold reaction fires on it before the owner has judged the
object unhealthy itself (AR10).

### Self-fence and rejoin

VW8. A worker MUST **fence** itself when any of these holds, checked on
every heartbeat tick and on every own-key watch event:

* (a) now minus `lastOkPut` has reached the dead threshold plus one vote
  interval — its heartbeat has not fully reached etcd for that long, as
  when the puts of two ticks in a row failed, and its peers observe it
  dead, or are about to, in each role whose puts did not land. This also
  covers a process that was stopped (SIGSTOP, a VM pause): the monotonic
  clock advances meanwhile, and a tick checks (a) before it records its
  own put, so the tick the process resumes with fences although its put
  lands. A single tick whose puts fail does not fence a worker whose puts
  return, and are echoed, within the vote interval, and whose observers'
  grace windows, its own included, outlast the dead and live flap they then
  see (VW3, VW5): the next tick finds `lastOkPut` the dead threshold old.
  Fencing on one failed tick would leave the worker's shards undriven for
  about a grace window over a short etcd outage, such as a leader election,
  and an outage that fails one tick on every worker would fence them all at
  once. The cost: a worker cut off from etcd fences up to one interval later
  while its puts fail promptly, and possibly later still while they hang,
  and the time between its self-fence and its peers' commit of it shrinks
  by as much (CM3). Its `reason` is `heartbeat_stalled`.
* (b) in some role, its own put is not echoed by that role's watch: the
  dead threshold plus one vote interval has passed since the latest put
  event for its own key of that role that the role's watch delivered, or
  since the incarnation started while it has delivered none, while puts
  report success — that watch is broken and its view of the role's peers is
  stale. The time of the last echo is kept per role, because each role has
  its own watch (VW10) and an echo on one says nothing of another's. It
  waits as long as (a) because a missed put is also a missed echo: on the
  very tick (a) rides out, checks that run before that tick's own echo
  arrives find the last echo about the dead threshold old, so a wait of only
  the dead threshold could fence there. Its `reason` is `watch_stalled`.
* (c) a delete event for its own key that this process did not issue — a
  peer committed it dead (VW6). Its `reason` is `key_deleted`.
* (d) its own observer commits its own registration nonmember (VW5, VW6):
  in some role this observer has observed its own key dead (VW3) — no put
  of it seen for the dead threshold, or the key absent from a rescan — and
  the disappear has then held for the grace time; by the first path that
  is one dead threshold plus one grace time after the last own put this
  observer saw. The commit takes the fence exit instead of the collection,
  because the key's owner is alive and still heartbeating (VW5). Its
  `reason` names the cause where the signals of (a) and (b) can tell it,
  judged against the dead threshold — by which this observer found its own
  key dead — rather than against the threshold of (a) and (b), which
  neither may have reached yet: a fence a failed seed mint deferred keeps
  its own reason; else `heartbeat_stalled` when `lastOkPut` is the dead
  threshold old; else `watch_stalled` when the last own echo of that role
  is; else `key_deleted`, the fallback when neither signal is stale — the
  key gone from the registry without this process deleting it, (c)'s fact
  learned from a rescan rather than from a delete event. After a stop
  longer than the threshold of (a), the fence is logged `heartbeat_stalled`
  whichever check the loop reaches first — (a) on the tick, (a) ahead of (c)
  on the own-key delete a peer may have issued meanwhile, or (d) through a
  grace timer armed before the stop — because (d) too judges the stalled
  heartbeat first.

Fencing means: mint a new seed (VW1) — first, because the `worker fenced`
record names it and a fence that cannot mint one tears nothing down (below);
log `worker fenced` (`reason`, `old_seed`, `new_seed`); stop
the heartbeat loop and join it, so no put of the old seed lands after its
delete; start the graceful stop of every shard worker of every role, in
parallel (SW5), and go on without waiting for it — every shard worker is
told at once, also in a role that is still waiting for the stop of a shard
it no longer owns (VW9); best-effort `Delete` the old registrations (they
may already be gone). Deleting before the drain is joined lets the peers
observe a delete that lands as a disappear and start their grace windows at
once, rather than when the drain ends — which can take
`DefaultWorkerSyncupTimeout` (RW11) — or the dead threshold passes,
whichever comes first; a last syncup can then finish after a peer took its
shard, the accepted overlap of an ownership change (VW7). The drain is
started before the deletes because a delete that cannot land — a VW8 (a)
fence is usually cut off from etcd — takes up to `DefaultEtcdOpTimeout` per
role, and shard workers not yet told to stop would go on driving rounds and
`Syncup*` calls behind it. Then join the drain; discard every tracking
entry, timer and effective set; and restart VW2 to VW7 from scratch under
the new seed; VW7 applies to the new incarnation: nothing is driven until
one full grace window after the new seed's first successful put. There is
no "resume with the old seed" path — a worker that lost etcd for the dead
threshold plus one vote interval is a new worker, exactly like a restart;
one code path serves every "the fleet gave up on me" case. A fence whose
new seed cannot be minted (VW1) tears nothing down and is instead
**remembered** and retried on every following heartbeat tick, ahead of the
checks above: (a) and (b) are conditions that would fire again by
themselves, but (c) and (d) are events — a delete, a commit — already
consumed, so a fence dropped there would be lost for good.

### Tickets and ownership

VW9. For a role, a shard of the bucket (`ShardBucketSize` shards) and an
effective member, the **ticket** is the sha256 of the member's seed, the
role and the shard code (rendered with `ShardCodeFmt`), joined by dashes;
the effective member whose ticket is largest under a bytewise comparison
owns the shard (a tie is a sha256 collision; it is broken by the larger
seed string and is never expected). Ownership is recomputed on every commit
that changed the effective set; the worker owns the shards whose owner is
itself, and diffs that set against its running shard workers: newly owned,
start one (SW1) and log `shard owned`; no longer owned, stop it gracefully
(SW5) and log `shard released`. A shard's owner changes only when the owner
leaves or a new member outranks it, so a membership change among n members
moves about one n-th of the shards and never reshuffles the rest.

VW10. **Roles are independent**: each role has its own registry prefix,
watch, last own echo (VW8 (b)), tracking entries, timers, effective set,
shard workers and log records, and ownership of a role considers only the
registrations under that role's prefix. Fencing (VW8) is process-wide
because the seed is.

VW11. An empty effective membership owns nothing. The worker never drives a
shard from the raw observed set.

## The shard worker — `worker/shard.go`

SW1. One shard worker per owned role and shard, started and stopped only by
the vote worker (VW9). Its prefix is `DnRevPrefix`, `CnRevPrefix` or
`SpRevPrefix` of the shard; its message type `DnRev`, `CnRev` or `SpRev`.

SW2. **Start.** A `Range` of the prefix yields the revision; every key is
decoded (`Decode`) and parsed (`ParseDnRevKey` and its siblings; a
malformed key or value is logged and skipped); one revision worker per
cluster id and object id is started with the initial desired state — the
value's revision and handle (RW3); then a `WatchTyped` from that revision
plus one.

SW3. **Events.** A put for an existing key updates its revision worker's
desired state (RW3 coalescing; a put that changes neither `revision` nor
the handle is a no-op); a put for a new key starts a revision worker; a
delete forgets it and stops it gracefully (RW11) on a goroutine of its own,
without waiting: the stop waits out the worker's in-flight unary call,
which can take `DefaultWorkerSyncupTimeout`, and the shard's other keys are
not held behind it. A worker started for the same key before that stop has
returned drives nothing until it has (RW1). Each start and stop logs
`revision worker started` / `revision worker stopped`; a worker waiting
for its predecessor logs its start when the wait ends, and one stopped
during the wait logs neither.

SW4. **Compaction and watch errors.** Rescan the prefix: start workers for
keys not known; update known ones whose revision or handle differ; stop
workers whose key is absent from the scan, without waiting, as a delete
does (SW3); restart the watch from the new revision plus one. A failing
rescan is retried every `--vote-interval` — never a hot loop.

SW5. **Graceful stop.** Cancel the watch, stop every revision worker in
parallel (RW11), join them and every stop SW3 or SW4 left running. The vote
worker logs `shard released` after the join, so a shard shows as released
only when nothing is driving it any more.

SW6. **Multi-cluster.** Keys of every cluster share the shard prefix; the
`cluster_id` comes from the key, and the `ClusterConf` cache (RW21)
supplies that cluster's configuration. A revision worker whose
`cluster_id` is not in the cache idles (RW9); it never guesses defaults for
an unknown cluster.

## The revision worker — `worker/revision.go` and the role files

A revision worker runs per rev key (a cluster id and an object id): it
pushes revisions to the agent(s) and health-checks them; for the sp role it
is a coordinator with one child goroutine per side and per cntlr of the SP
(RW14). An **object** is a DN, a CN, a side or a cntlr — the unit
of one `Check*` stream and one goroutine; a **round** is one `Check*`
request and reply exchange on an object's stream; the **handle** is the
mutable part of a rev value, `addr_port` for a node and `sp_name` for an SP
(`architecture.md`, Revision keys and the sync fan-out).

### The per-object loop

RW1. **One goroutine per object** — one per DN (dn role), per CN (cn role),
per side and per cntlr of an SP (sp role). The goroutine alone
owns the object's `Check*` stream, its `Syncup*` calls and the loop's own
state, which is what makes the "one `Syncup*` at a time per object" of
`architecture.md`, Common agent rules, and the "one stream per object" of
`architecture.md`, Check streams, hold by construction. A parent does not
wait for the stop of a child it replaces (RW11), so a worker started for an
object whose previous worker is still stopping drives nothing until that
stop has returned: a restart never puts two loops on one object. Three
deliberate carve-outs share the object: the `Push*Bitmap` calls run on the
pusher's own goroutines (BM3 — one in flight per migration or clone,
concurrently with this loop); `lastInfo` is published by the stream's pump
goroutine under a mutex so the sp coordinator can snapshot it (AR1); and
the sp coordinator's pass, under a mutex of each side and cntlr child's
health monitor, reads the count of the monitor's own writes before its load
and offers the child its record's `err_epoch` after it, which only the
child's own goroutine folds into the memo, before its next verdict (HL3).

RW2. **State**: the desired revision plus the inputs the request is built
from, the stream, `lastInfo` (the last `*Info` received, on a dn loop with
the revision of the reply that carried it, RW13), the health state (Health
bookkeeping), the latest observation time seen for the object with the
endpoint it was seen at (RW23), the observation time of the latest
`Syncup*` reply, kept for the late record's leave until the next round is
judged or a round fails (HL7), the agent's answer time on the first round
after a pause in the loop's rounds, kept until the next pause (HL7), and
the round timer. No round's outcome is
kept as WORK STILL OWED: no last-acknowledged revision decides a re-sync,
and no "re-sync wanted" flag exists anywhere in the loop. RW4 step 5
recomputes the whole re-sync condition from the reply in hand — its own
`revision` against the desired revision, its own `agent_reply.code` — so a
worker just handed the shard decides its re-syncs exactly as one that has
driven the object for an hour, and no failure can be forgotten by a flag
something cleared. Five things the loop's replies leave do outlive them, and
none is such a record: `lastInfo`, because a `show_info` false reply omits
an unchanged `*Info` and health is evaluated on the latest known one (HL5);
the health state, a cache of the record: the last health verdict WRITTEN,
re-seeded from the record by the loads HL3 names — which is exactly what
HL3's transitions-only rule has to compare the next health verdict
against — the time a DN's or a CN's monitor last read or wrote the record, which paces
HL3's re-read, and a cntlr's settle memo included (HL2: the record's
`settling` as the last plan loaded it, cleared by the write that clears it
in etcd); the latest observation time seen, because a run of rounds in a
row counts a round only on an observation later than every one seen
(RW23); the answer time of the first round after a pause, because an
observation's age counts from no earlier than that round and one that
ended before it gives no verdict, no round having asked the agent to read
during the pause (HL7); and the latest `Syncup*` reply's observation time,
because the round right after that reply, when it answers from that observation, is
late by the loop's own wait when that `Syncup*` ended a round (RW4 step 5,
RW6), which the late record leaves out (HL7).
BM5's mod-revision memo survives rounds too, but it belongs to the pusher,
and AR10's memo of the health verdicts the rounds reach belongs to the sp
coordinator: it holds health verdicts — the objects the coordinator last judged
unhealthy and each cntlr's run of unhealthy verdicts from rounds — and no
work owed. A dn loop's count compare (RW13) keeps a cache of the record as
well — its free and total extents with the `DnRev` revision read with them,
replaced by every read of the record that answers — and two memos of its
compares that owe nothing: its run of disagreeing compares, and whether
that run was logged. The memos of what was LOGGED — RW9's two, plus, on a
cntlr child, the last standby leg row logged per leg, so HL2's standing row
does not repeat every round — never record what is owed, and neither does
a side or cntlr child's memo of what it REPORTED:
the last revision it handed RW14's sides-first hold or RW22's demotion
hold as applied, so a standing reply does not report it every round.

RW3. **Coalescing.** Desired changes arrive from the parent (shard worker
or SP coordinator) over a channel of capacity one that is overwritten, so
the loop only ever sees the latest revision — every request carries the
complete state, intermediate revisions need not be sent (`architecture.md`,
Common agent rules).

RW4. **Round**, every interval (RW9):
1. no stream: open one over the cached connection (RW7); a failure to open
   counts as a broken stream;
2. send the `Check*` request carrying the ids, `revision` (the desired
   revision), `show_info` false and the round's `trace_id` (RW10);
3. wait for the reply at most one interval (RW8);
4. no reply, a stream error, or a reply whose observation is too old, which
   is a missed round in every way and issues no `Syncup*` whatever its code
   and revision (HL7): close the stream; health "unreachable" (HL1, HL2) —
   except for a round abandoned under RW6, and for a round the graceful
   stop of RW11 cut short, each of which drops its stream the same way but
   carries no health verdict: a cancelled stop ctx is not a sick agent, and
   the verdict would start the threshold clock of Automatic reactions on a
   healthy object;
5. any other reply: process its `*Info` if present (Health bookkeeping; sp:
   RW18, RW19), which for a round that answers from an observation no
   round asked for is only to keep it as the latest known info (HL7);
   then if `agent_reply.code` is non-zero **or** the reply's
   `revision` differs from the desired revision, issue the `Syncup*`
   (RW5) — this is also how the first sync after a shard handoff, a worker
   restart or an agent restart happens, with no recovery step of its own,
   and how a leftover is re-driven: a round's code and revision are those
   of the observation it answers from (`dnagent.md` SH28), which the
   converges and the background reads of the object store, each with its
   own verdict on the node (`dnagent.md` SH29), so for as long as the node
   still holds something the desired state does not want, a round whose
   observation names it issues the `Syncup*` that sweeps again, and a round
   answering from a clean observation at the desired revision issues none —
   no backoff (RW12), no flag, the code is the state. The re-sync stops for
   good there only when no later read names the condition again: a
   converge's observation carries the code of the converge's own reply,
   which leaves out what only a read's verdict names (`dnagent.md` SH28,
   SH29), so for such a condition a converge's clean observation only
   pauses the re-sync, and while the condition lasts the re-sends fall to
   every other round at most instead of stopping (the subsystem directory
   a cn teardown leaves is such a condition, RW5);
6. re-arm the timer.

RW5. **Syncup.** Build the request from the current inputs (RW13 to RW16),
send under `DefaultWorkerSyncupTimeout` with a trace id (RW10). An ACCEPTED
reply — code zero, or `ReplyCodeLeftover` below — has its `*Info` processed
(Health bookkeeping), its `bm_info` or `bm_info_list` diffed (Bitmap
pushes) and its sp flips run (RW18, RW19). A REJECTED one
(`ReplyCodeStaleRevision`, `ReplyCodeUnknownObject`, `ReplyCodeInvalidConf`,
and any other non-zero code — below) is logged as `syncup rejected`
(`code`, `details`) and left to the next round: unknown object means the
parent syncup has not landed yet — an sp worker's `SyncupSide` reaching the
DN before the dn worker's `SyncupDn` listed the side, say, which is normal
since the roles are independent (VW10) — and stale revision means the agent
holds a revision newer than etcd's, which only an etcd restore can cause
(`architecture.md`, etcd backup and restore) and is logged at Error. Invalid
conf means the agent found a value the control plane cannot have written —
a proto3 zero where `architecture.md`, Common validation, requires a
concrete value, or a side's length to zero
outside the bounds of the dn agent's side conf gate (`dnagent.md` DN8) —
and converged nothing (`dnagent.md`, Names and constants in `common`).
`SyncupDn`, `SyncupSide` and `SyncupCntlr` can return it, and the same
values are checked on this side first — `extent_size` by RW9's gate, the
SP's `bdev_conf` and every group's length to zero by RW14's — so a
request this worker sends should never provoke it. From a live agent, a
`ReplyCodeInvalidConf` to `SyncupDn` or `SyncupCntlr` means the agent's
copy of those rules and `model`'s have drifted apart (`dnagent.md`, Shared
mechanism — package `agent`); one to `SyncupSide` means a stored group
whose block counts claim more than its extents hold, the one bound the dn
agent checks and RW14 does not, since checking it needs the extent size.

`ReplyCodeLeftover` is the one non-zero code that is **not** a rejection:
the request was applied — the desired state is stored and every wanted
object converged — and the node still holds objects the desired state does
not want, or an enumeration of what it holds did not answer, which proves
nothing either way, or — on a dn — a disk whose identity it has not
confirmed, which it reports the same way so that its `SyncupDn` is re-sent
(`architecture.md`, Teardown by sweep); the agents' other such conditions
come only from the verdict a background read gives (`dnagent.md` SH29), so
they ride only on the `Check*` and `Get*Info` replies that answer from a
read's observation, never on a converge's, a `Syncup*` reply's included.
It is logged as
`syncup leftover` (`log.md`, Leftovers) beside the ordinary
`syncup result`: nothing agent-side re-drives the sweep for a leftover (the
agents' own background converges, `cnagent.md` CN10's connect retry and
`dnagent.md` DN13's connect retry and DN12's fence timer, sweep only
while one is registered or armed for a
reason of its own); it runs again on the
re-sync RW4 step 5 issues, so a leftover is a normal state for as long as a
dead remote's failfast window lasts, and the record is what makes one the
re-sync keeps finding visible in the log. A cn teardown's reply carries the
code routinely: the cn agent's sweep sets every `nvme disconnect` it issues
— of a leg or a clone source — going off its pass (`cnagent.md` CN10) and
names the connection as a leftover (the nvme kind plus the NQN) until a
later pass finds it gone (a removing pass once it has no controller; a
background read once its sysfs subsystem directory, which can outlive the
last controller, is gone), so a `SyncupCntlr` or `SyncupCn` whose sweep
disconnects anything is answered `ReplyCodeLeftover` however fast the
disconnect turns out to be. The disconnect needs no re-sync to finish: a
round answering from an observation that no longer finds the connection,
with nothing else left, answers code zero and issues nothing, and once no
later read names the connection again RW4 step 5 issues nothing more for
it; a round whose observation still names it re-issues the `Syncup*`,
whose pass sets no second disconnect of that subsystem going while the
first still runs. While all that is left is a subsystem directory whose
last controller is gone, the re-issued `Syncup*`'s removing pass counts
the connection gone and answers code zero, and the observation its
converge stores carries that code (`dnagent.md` SH28): the round after it
issues nothing and starts a read (`dnagent.md` SH29), whose verdict names
the directory again, so the worker re-syncs on every other round at most
and logs a `syncup result` with no `syncup leftover`; what names the
connection then is the agent's own `sweep leftover` record of each read's
verdict (`log.md`, Leftovers).

The rejection handling does not enumerate the codes: only
`ReplyCodeStaleRevision` raises the record to Error, every other value —
including one this worker does not know — is Info, and an unknown code
counts as a rejection, because a reply this worker cannot interpret carries
no verdict it may act on. Nothing is remembered either way (RW2): the
re-send is not armed here, it comes from the NEXT round's own reply, which
repeats the same code or the same revision mismatch until the agent's
answer changes. An agent that stores a non-zero outcome must therefore
report it on `Check*` too, or the `Syncup*` that would clear it is never
issued. A gRPC error is logged and left to the next round. A desired change
that arrives while a syncup is in flight is applied when it returns.

RW6. **Immediate syncup.** A desired change from the parent triggers a
`Syncup*` at once, without waiting for the round. A change that arrives
while a round waits for its reply (RW4 step 3) is applied there and then,
because that reply may be a whole interval away (RW9): the round is
**abandoned** — its reply would answer the request built from the
superseded revision — and its stream is dropped with it: RW4 step 4 never
reuses a stream across a round that ended without a reply, and an abandoned
round is such a round. An abandoned round carries **no** health verdict:
nothing was observed about the agent, the round was only overtaken. Its
stream is dropped **before** the change's `Syncup*` is sent. Reason: the
drop is for the sake of the round's reply, which would answer the request
built from the superseded revision and must never be read as a later
round's answer, and no lock rides on it: the round holds none the
`Syncup*` needs, because the
agent answers it from its latest observation and takes no lock for it
(`dnagent.md` SH25), and the `Syncup*` cancels the object's background read
itself before it waits for its locks (`dnagent.md` SH30). The next round
opens a fresh stream, which the agent answers with the complete `*Info`
again (`architecture.md`, Check streams). The sp coordinator is the one
parent that hands a change on late, and on purpose: its cntlr children receive a
fan-out once every side child RW14's sides-first hold waits for has
reported it applied, and at the latest one `cntlr_interval` after the sides
were handed it; a fan-out that demotes a primary reaches the demoted cntlr
at once and the sides only when RW22's demotion hold ends; from each
hand-over on, this rule applies unchanged.

RW7. **Connections.** A process-wide cache from `addr_port` to one client
connection (`grpc.NewClient` with insecure transport credentials, both
client interceptors and the message limit — `grpc.md`, Wiring),
reference-counted by the objects using an endpoint and closed when the last
one stops. Every stream and unary call to one agent multiplexes over that
connection. A stream is opened lazily on the first round and after every
failure, and closed with `CloseSend` plus ctx cancellation on stop (RW11).

RW8. **Round timeout** is the object kind's interval: a reply arriving
later is a missed reply (`architecture.md`, Check streams). A reply in time
is a missed round too when HL7 finds the observation it answers from too
old. The timer is re-armed *after* each round, so a slow round never
queues a burst of catch-up rounds.

RW9. **Inputs from the cache** (RW21), every one used **as stored**:
`dn_interval`, `cn_interval`, `side_interval` and `cntlr_interval` (already
inside the bounds `MinHealthCheckInterval` and `MaxHealthCheckInterval` set —
`CreateCluster` resolved and clamped them at write time, `architecture.md`,
Common validation), `extent_size`, `qos_ratio`, `dn_bin_conf`. The intervals,
`extent_size` and `qos_ratio` are read from the process-wide cache every
round, never captured once. A cluster absent from the cache makes the loop
**idle**: no stream, no syncup, one `cluster conf missing` record, a retry
every `DefaultHealthCheckInterval`.

A cluster whose entry fails `model.ValidateClusterConf` makes the loop
**refuse** it: the same quiesced state (stream dropped, RW7 connection
reference released) on the same retry cadence, but with its own
`invalid stored conf` record at Error (Log records), so that the
`cluster conf missing` grep keeps meaning "not in the cache" and nothing
else. The gate sits in `revWorker.run` ahead of the round, so one rule
covers all four object kinds — dn, cn, sp side and sp cntlr — and the
refusal reaches nothing: no stream is opened, no `Syncup*` is sent, no
`err_epoch` is written, no STM runs. A desired change that arrives
meanwhile is still recorded (RW3) but not sent; the first round after the
conf is repaired syncs it. The record is memoized on the error string, so a
steady bad conf costs one record and a conf that changes from one invalid
value to another still reports.

RW10. **Trace ids.** Every round, syncup, fan-out, push, flip and reaction
runs under `common.WithTraceId` with an id made of a prefix of the seed, a
dash and `common.NewTraceId`'s output. The interceptors carry it to the
agent (`grpc.md` T1), whose log then names the worker that sent each
request — the worker suite's "one owner per shard" evidence. A `Check*`
stream's metadata is sent once, at open, under the round that opened it, so
every round also puts its own id in the request's `trace_id` (RW4 step 2),
under which the agent runs the round (`grpc.md` T3), and the background
read the round starts (`dnagent.md` SH29). The `Syncup*` a round issues
(RW4 step 5) runs under that round's id, while an immediate syncup (RW6)
gets an id of its own; the `syncup result` record of a round's re-send also
names the trace id of the observation the round answered from
(`dnagent.md` SH28). Reason: the agent logs a verdict that was not clean
under the id of the read that reached it, so that record joins the
verdict to the re-send it brought, and the re-send's id joins the worker's
record to the converge, which is how a leftover that only a verdict names
is followed to its re-send (`log.md`, Leftovers); the call itself keeps
the worker's own id, the owner's mark in the agent's log.

The parts of an incarnation that outlive one unit of work hold an id of the
same shape for their whole life, minted when each starts: the vote worker's
incarnation (VW1), each role's shard workers, which share one, each
per-object loop, each sp coordinator and each pusher (BM3). Every unit of
work under them still mints its own. A record such a part writes outside a
unit of work carries its id: one id groups every heartbeat, registry scan
and watch, membership and fence record of an incarnation; one per role the
`shard owned` and `shard released` records and the shard workers' scans,
watches and malformed keys; one per loop or coordinator its start and stop
records, and a loop's idle records and its refusals of a cluster's stored
conf as well. Only CM4's startup trace id goes without the seed: the
`ClusterConf` cache (RW21) runs under it across every rejoin after a
self-fence (VW8) and would carry a stale prefix. Reason: a Go case with
several workers tells each worker's records apart by these ids, and an
operator pulls one incarnation's records by its prefix.

RW11. **Graceful stop.** On ctx cancellation the loop finishes an in-flight
unary call (its own deadline bounds the wait; the stop ctx is not the RPC
ctx), then `CloseSend`s and cancels the stream, releases the connection and
logs `revision worker stopped`. A loop cancelled while it still waits for
its predecessor (RW1) has done nothing yet: it returns once its predecessor
has stopped, and logs neither `revision worker started` nor
`revision worker stopped` (SW3). A parent that goes on running — the shard
worker on a delete or a rescan (SW3, SW4), the sp coordinator on its child
diff (RW14) — runs such a stop on a goroutine of its own and does not wait
for it, so no other object it drives waits out that call, save the cntlr
half of an sp fan-out that restarted a side the sides-first hold waits for
(RW14): the hold waits for that side's report, which its new child can send
only once the old child's stop has returned (RW1), so those cntlrs can wait
out the call up to the hold's one-`cntlr_interval` bound. A parent that
stops joins its children, those stops included, with a wait group (SW5).

RW12. **No backoff anywhere.** The round is the retry cadence for
everything the loop could not finish — an unreachable agent, a rejected
syncup, a leftover reply, a health write that did not commit, a missing
cluster conf. A failed PUSH is the one thing NOT on that list, and not
because it waits longer: it arms nothing at all (BM6), so no round retries
it. It is re-planned only when the object's next `Syncup*` is issued for
some other reason, from the agent's own acknowledged set (BM2).

RW23. **New observations.** Each loop keeps the latest observation time it
has seen for its object in a round's reply or a `Syncup*` reply
(`dnagent.md` SH28), and a round's reply whose observation time is later
than that answers from a new observation. Of the rounds whose reply comes
in time from an observation that is not too old, only one on a new
observation that a round asked for (HL7) adds to a run of rounds in a row:
a cntlr's run of unhealthy verdicts (AR10) and a DN's run of disagreeing
count compares (RW13). A round whose observation is too old counts in each
run as a missed round does (HL7): it adds to a cntlr's run every time and
does nothing to a DN's. A `Syncup*` reply's observation is seen but never
adds to a run. A round that answers from an observation no round asked for
is seen too, and neither adds to a run nor ends one, since it gives no
verdict and makes no compare (HL7). What ends
a run is that run's own rule: only an agreeing compare ends a DN's run, and
only a round on a new observation that a round asked for makes a compare
(RW13), while any clean
health verdict ends a cntlr's run, that of a round on an observation already seen
or of a `Syncup*` reply included (AR10). The time seen is forgotten when
the object moves to another endpoint (RW13), and a loop starts with none.
A child restarted at the same endpoint never counts in its cntlr's run
(AR10) what a look its predecessor saw reported: its loop's start is a
pause (HL7), and the new loop starts only once the old one has stopped,
its call in flight finished (RW1, RW11), so every observation the old
loop saw ended before the new loop's first round and gives that loop no
verdict until it is too old, when a round on it counts as a missed round
does, by its age and not by its rows (HL7; Known limits). Reason: while a
converge holds an object's reads off, several rounds carry one
observation, and counting each of them would act on one look.

### dn role — `worker/dnrole.go`

RW13. Inputs: `cluster_id` and `dn_id` from the key, `addr_port` and
`revision` from the value. Per syncup the loop reads the cluster conf from
the cache and, at one store revision (EU4), the `DnConf` at `DnConfKey` of
the cluster and endpoint together with the `DnRev` of the DN that record
names, so that the record comes with the revision it belongs to (a missing
`DnConf`: log, skip, retry next round; a read that fails fails the syncup,
whose `syncup result` names the read's error, and the next round retries);
it sends a `SyncupDnRequest` carrying `cluster_id`, `dn_id`, the revision,
`side_pointer_list` from the record's `side_ptr_list` and `extent_size` from
the cluster's `dn_bin_conf` to the endpoint. Rounds send a
`CheckDnRequest` carrying `cluster_id`, `dn_id`, the revision, `show_info`
and `trace_id`. Health per HL1, judged against a cache of the record's
`err_epoch` that the syncup's read re-seeds and that the health monitor
re-reads itself, with the same read, before a verdict once
`nodeRecordMaxAge` has passed without its reading or writing the record
(HL3). A put whose only change is `addr_port` re-syncs the node at its new
endpoint: the loop drops its stream and connection reference and continues
at the new address; no delete ever reaches the agent (`architecture.md`,
dn / cn roles, [D10]).

A round's accepted reply (code zero or `ReplyCodeLeftover`, RW5) that
answers from a new observation (RW23) that a round asked for (HL7) is
also a **count compare** when
three things hold: the last read of the record — a syncup's, or HL3's
re-read — found this DN's record with its `DnRev`; the disk counts in the
latest known `DnInfo` (HL5; `dnagent.md` DN18) are present and say that the
disk's side records name exactly the side pointers of the request the
agent holds; and the revision of the reply that carried those counts —
that of the observation they belong to (`dnagent.md` SH28) —, the round
reply's own and the `DnRev` revision read with the record are one. A
compare sets the counts' free extents beside the record's `free_ext_cnt`.
At one revision a DN has one side pointer list and
one free count: every change of its free count also changes that list and
bumps its `DnRev` (`architecture.md`, Per-operation allocation). An
agreeing compare ends the run of disagreeing ones, and the second
disagreeing compare in a row logs `dn free count disagrees` (Log records),
once until a compare agrees. Nothing else extends or ends the run: not a
`SyncupDn`'s reply, a rejected round, a missed one, a round that answers
from an observation already seen or from one no round asked for (HL7),
nor a round whose counts are absent or
do not match or whose revisions differ. Two in a row, because one compare
can disagree with no fault: while two owners overlap (VW7), the other
owner's `SyncupDn` can hand the agent the revision of this loop's record
with a side pointer list read after it, until either owner's watch
delivers the newer revision; and only rounds on new observations count, so
that the two compares rest on two different looks at the node and lie at
least a round apart. Free metadata units are not compared: the dn
agent takes a migration destination's run of units in that side's
converge, after the create charged the record, and holds none while the
side waits for room (`dnagent.md` DN13), so the two counts can differ on a
healthy disk node. Nothing is written for a disagreement, neither the
agent's count nor an `err_epoch` (HL1). Reason: the record's count and the
disk's volume table are kept by different programs, and nothing else
compares them; the dn role writes the record only on a health change (HL3),
so storing the agent's count would need a new kind of write.

### cn role — `worker/cnrole.go`

The mirror image, without RW13's count compare: the `CnConf` at
`CnConfKey`, read alone with a plain `Get`, a `SyncupCnRequest` carrying
`cluster_id`, `cn_id`, the revision, `cntlr_pointer_list` from the record's
`cntlr_ptr_list` and `qos_ratio` from the `ClusterConf`, a `CheckCnRequest`,
and health per HL1.

### sp role — `worker/sprole.go`

RW14. The SP revision worker is a **coordinator**. On every desired change
(an `SpRev` put: `revision` plus `sp_name`) it loads the SP with
`model.LoadSp` (MD3), resolves the `dn_id` of every side of the SP
(through `DnByAddr` by the side's `addr_port`) and every cntlr's
`cn_id`, builds every `SyncupSideRequest` and every
`SyncupCntlrRequest` once (RW15, RW16), keeping
with each side's plan the side's `err_epoch` as that same load read it, for
the sides-first hold (below), and diffs
its **children**: one per side — the sp id, leg id and
side id — and one per cntlr. The sides of the
SP are those of its slices' groups and, after them in `clone_name_list`
order, those of every clone's clone meta group (`architecture.md`,
Terminology and object model), then the side of every repair's new leg, in
`repair_name_list` order, and every retired side, in `retired_leg_id_list`
order (`architecture.md`, Repair): a clone being deleted keeps its sides —
those of its group's repair and retired legs included — driven until the
clone drain's final transaction takes them from their DNs (CLD5, CLD9), a
clone with no group has none, and the side of a repair or a retired leg
whose group the load does not find in the parent the record names gets no
request, since its request takes the group's `ext_cnt` and length to zero;
the coordinator logs the record it skips, `sp group missing`. Each side's
plan names the record that owns the side — its slice, its clone, its repair
by name and `rep_id`, or its retired leg — which its health writes take
(HL2). New:
start; gone: stop (RW11), without waiting for the stop; a child whose
endpoint changed is restarted at the new one, the new child driving nothing
until the old one has stopped (RW1); so is a side child whose side moved to
another record: a finish, a cancel and the reuse of a retired leg keep the
side's leg id and side id (`architecture.md`, Repair), so the diff compares
the record that owns each side, and the restarted child's fresh monitor
writes to the record the new plan names;
every remaining child receives its new request as a desired change (RW6). A
child whose **request** changed while the `SpRev` revision did not — a
re-resolution tick that altered a standby list — is restarted too, because
RW3's coalescing sees the unchanged revision and would otherwise swallow
the change and leave the child driving a stale request until the next
bump. That is the fan-out: unordered within each kind, sides before cntlrs
(below), and a demoted cntlr before both (RW22). `ErrNotFound` from
`LoadSp` means the SP is being deleted: log, keep the children until the
`SpRev` delete arrives (the agents tear down through the pointer lists).
An endpoint without a `DnConf` or `CnConf`
leaves that child **idle**: the fan-out builds no request for it, so the
diff treats it as gone — a running child is stopped (RW11) and none is
started. While the last fan-out left anything unresolved — such an
endpoint, or a cntlr's missing record (below) — the coordinator re-resolves
every `cntlr_interval`, and the fan-out that resolves one starts a new child
for it — a cntlr's with the sides-first release below — which drives
nothing until an old one's stop, if still running, has returned (RW1). When
the object that cannot be resolved is a **cntlr** — its `Cntlr` record is
missing (MD3) or its `CnConf` is absent — **every side child** of the SP is
left idle, not just that cntlr's own: RW15's `primary_cn_id` and
`standby_id_list` must name every cntlr of the SP, and a `side_conf` built
from a shrunken set makes every DN of the SP tear the missing CN's
dm-error, dm-linear, subsystem and namespace down. The **other** cntlr
children are unaffected — RW16's request carries no peer's `cn_id`, so on
the cntlr side the effect stays confined to that cntlr's own child, left
idle by the rule above — and the primary's leg rows are still placed on
the record that holds each leg, its slice, its clone or its repair (HL2).

**Sides first.** The side half of the diff is applied at once, unless a
demotion hold keeps it back (RW22). The cntlr half — every running cntlr
child's new request, and every cntlr child to start — is HELD until every
side the hold waits for has reported the fan-out's revision (or a newer
one) applied, or until one `cntlr_interval` has passed since the hold
began — when the sides were handed the fan-out — whichever comes first.
The hold waits for a set of sides: the running side children whose `Side`
record carried no `err_epoch` in the load the set is taken from, retired
sides left out, since no cntlr connects one (`architecture.md`, Repair); a
repair's new leg's side is waited for, since every cntlr connects it. A
hold
takes the set from its fan-out's load when it begins, and from a bump's
load when the bump — a fan-out of a newer revision — replaces the held
plans (below); nothing else changes it. A re-fan at the held revision,
such as a re-resolution tick's, keeps the set and adds none of the side
children it starts; a side child of the set that the diff stops is not
waited for, and one the diff restarts is. Until a bump takes the set
again, a side that turns unhealthy during the wait stays in it, so one
whose agent stops answering can hold the cntlrs for the whole interval,
and one whose epoch clears meanwhile is not added. A side the load found
unhealthy is left out because the hold only narrows races (below):
waiting for a side that does not answer — on a dead disk node, such as the
side of a repair's bad leg there (AR8) — would make every change of the
pool wait the whole interval and let a failover fire inside the wait. A
side left idle (above) is not waited for either, and a hold that waits for
no side lets the cntlrs go at once. A side child the diff restarts — at a new
endpoint, on a changed request, or when its side moved to another record —
reports nothing until the old child's stop has returned (RW1; the cost is
below). A cntlr child the diff stops
is stopped at once, beside the sides', and one it restarts is started with
the release. A side child reports each accepted reply (RW5)
whose revision is newer than the last one it reported, and a `CheckSide`
reply counts as much as a `SyncupSide` one, because a side whose agent
already holds the revision is sent no `Syncup*` at all (RW4 step 5). A
round that answers from an observation no round asked for reports nothing
(HL7), so a new side child whose first round brings no `Syncup*` — its
agent already holding the revision, after a handoff or a worker restart,
or when the diff restarts the child — reports from its second round at
the earliest (the cost is below). The hold lives on the coordinator's loop, released by
those reports or by a timer on the same loop, and never blocks it: reports,
ticks and the next desired change are served while it waits, and every
child keeps its rounds. A fan-out that finds a hold pending replaces what
is held — a bump takes the sides to wait for afresh, from its own load —
and keeps the deadline, so a stream of bumps cannot hold the cntlrs past
one interval, and the superseded requests are never sent (RW3); one
that demotes a primary ends the hold instead, and its demotion hold keeps
the newest plans (RW22). A release by the timer logs `sp sides unsynced`
(Log records). Nothing of the hold is persisted: a new owner, or a
restarted worker, holds its own first fan-out the same way. While a hold is
pending, the coordinator judges every leg row by the held plan (HL2). The
order mitigates three races and is no correctness dependency
(`architecture.md`, [D16]): a promotion's
`SyncupCntlr` outrunning the sides' ANA flips, which leaves the new
primary's groups to the agent's own retry, and a cntlr's connect to a new
side outrunning that side's export (both `cnagent.md` CN10); and a leg
removal's `nvme disconnect` on a CN overlapping the disk node's unlink of
that side's export, which can leave the disconnect waiting out the kernel's
admin timeout. That unlink comes from the dn role's `SyncupDn`, which the
barrier does not wait for: the cntlrs gain only the sides' round trip on
it, and more when that disk node also holds a side of the SP whose
`SyncupSide` finds the `SyncupDn` ahead of it on the node lock
(`dnagent.md` DN1).

**What the hold costs.** No demotion RW22 finds is held: the demotion hold
hands it to the old primary before the sides fence its legs. The fence-first window
— an old primary still serving host IO answering it with target-internal
errors on its still-optimized path from the time the sides' flips fence its
legs until its agent has applied its demotion (`architecture.md`, Failover, and
`architecture.md`, [D16]) — therefore remains only for an old primary that
does not apply its demotion within RW22's wait, or whose failover a
coordinator fans out with no running child of it last handed a primary
plan, as after a shard loss before the demotion hold ends, or before that
coordinator's first hold has started its cntlr children (Known limits).
Hosts that follow the discovery log have dropped the path of one that
cannot be reached by the end of the whole wait (RW22; `architecture.md`,
Known limits). The wait is the demotion hold's own cost: a failover whose
old primary does not answer — every failover of one that cannot be
reached, since the worker judges no reachability — holds its sides and its
promotion for the whole demotion hold, and the sides-first hold begins only
after it. Only an accepted reply reports,
so a side the hold waits for whose agent does not accept the fan-out's
revision costs that fan-out up to one `cntlr_interval` of cntlr latency and
an `sp sides unsynced` record, a failover's promotion included, and RW9
lets that interval be as long as `MaxHealthCheckInterval` allows. A side on
a dead disk node costs that only in a hold that waits for it: one whose
fan-out loaded the pool before the side's child had stamped its
`err_epoch` (HL2), or one during which the disk node dies. A disable of the
primary is failed over inside such a hold whenever AR5 has a candidate, the
failover's demotion hold ending that sides-first hold (RW22). Every hold
that takes its set from a load made once the epoch is stamped — its
fan-out's when it begins, a bump's that replaces its plans, or, when it
begins as a demotion hold ends, that of the newest plans the demotion hold
kept (RW22) — leaves the side out, and the bad leg of a repair keeps its
side's epoch until its finish makes it a retired leg, whose side no hold
waits for, so a bad leg costs no hold. The
price of leaving a side out is the narrowing itself: a side the load found
unhealthy that answers after all is not waited for, and the three races
above stay open for it. The cost is also paid in normal
operation: a new side — of a new SP, a grow, a repair's fresh new
leg, a migration destination, a clone's clone meta group — whose first
`CheckSide` and `SyncupSide` reach
its disk node before the dn role's `SyncupDn` has introduced its pointer is
refused
(`ReplyCodeUnknownObject`, `dnagent.md` DN8; normal per RW5) and tries again
at its next round, one `side_interval` after its first round ended, which
falls after the hold's deadline when the two intervals are equal, as they are
by default. It is paid as well, in part or in whole, when the diff restarts a
side whose old child is still inside a `Syncup*`: the new child sends and
reports nothing until that call has ended and the old child has stopped (RW1,
RW11), which can be up to `DefaultWorkerSyncupTimeout` later — past the hold's
deadline at the default `cntlr_interval`. And it is paid at the first
fan-out of a new owner or a restarted worker, and at a fan-out that
restarts a side, for each side the hold waits for whose new child's first
round brings no `Syncup*`, its agent already holding the fan-out's
revision: that round answers from an observation no round asked for and
reports nothing (HL7), and the child's next round comes one
`side_interval` after the first ended, after the hold's deadline when the two intervals are
equal. Such a hold runs to its timer and logs `sp sides unsynced`, and
the cntlr requests it holds, and the cntlr children it starts, go about
one `cntlr_interval` after the sides.

Between the load and the plan the fan-out validates the SP's stored
`bdev_conf` with `model.ValidateBdevConf` (`architecture.md`, Common
validation), and then the length to zero of every group of the SP, a
clone meta group's included, a deleting clone's too (RW15): a group from
whose stored block counts `model.SideZeroBytes`, or for a clone meta group
`model.CloneMetaSideZeroBytes`, derives no length is refused the same way,
because its sides would otherwise be sent a length the dn agent refuses
(`dnagent.md` DN8). That geometry is what RW16's
request carries verbatim and what RW15's side request derives values from
— every side's `zero_bytes` and the migration destination's `block_size`.
`dm_raid0_conf.stripe_size` is used as a value by no worker
computation, and `redund_md_raid1.bitmap_chunk_block_cnt` only inside
`model.GrowSlice`'s geometry (MD6), which AR6 drives; this gate only checks
both for zero, and otherwise both travel verbatim inside `bdev_conf` to
the cn agent, so this is the one place the worker can refuse to hand that
agent a geometry nobody chose. A refusal of either emits one
`invalid stored conf` record (Log records, once per distinct error) and
builds no request, starts no child and updates no running one —
`buildPlan` and the child diff are simply not reached. It
arms the same fan-out retry a failed `LoadSp` arms, because the tick
re-enters the fan-out only when a retry is armed and the child diff — the
other thing that arms one, through the idle count — was not reached. What
it deliberately does **not** do is stop the children already running: they
keep driving the plan built from the last good conf, because a conf that
cannot be read is not a reason to stop serving IO.

A load whose `SpRevision` (MD3) is ahead of the revision the coordinator
was delivered builds nothing either — no request, and no child started,
stopped or updated — and arms no retry, because the newer revision's own
delivery is a desired change, which re-enters the fan-out. A load runs
ahead whenever it follows a bump that has not been delivered yet: a tick's
fan-out (the idle re-resolution, or a retry the fan-out armed) between a
bump's commit and its delivery, say, or a new owner's first fan-out from
its parent's older value. Every request is labelled with the delivered
revision (RW15, RW16), so the plan built then would pair that label with a
newer role: a promotion labelled with the revision its agent applied as a
standby restarts the child (its request changed, its revision did not), the
agent answers the first Check at that revision with a clean standby shape,
RW4 step 5 finds nothing to re-sync, and HL2 would settle the promoted
cntlr on that reply.

RW22. **Demotion hold.** A fan-out **demotes** a cntlr when the cntlr's
running child was last handed a primary plan and the fan-out's plan makes it
a standby; only a failover does so in production, on either of AR5's
triggers. Such a fan-out runs a demotion hold ahead of RW14's sides-first
hold: the demoted cntlr is handed its plan at once (RW6), and the side half
and every other cntlr's request are held until each demoted cntlr's child
has reported its demotion's revision, or a newer one, applied, or until
`DemotionHoldTimeout` has passed, whichever comes first. A cntlr child
reports as a side child does (RW14): each accepted reply (RW5), a
`CheckCntlr` one as much as a `SyncupCntlr` one, whose revision is newer
than the last one it reported. Then the sides are handed the fan-out, and
the other cntlrs follow under the sides-first hold, which takes the sides
it waits for (RW14) from the side plans the demotion hold kept, the
newest, and whose bound of one `cntlr_interval` runs from that moment.
The demotion hold waits for the demoted cntlrs alone, so it holds the
other cntlrs even when every side is idle.
Like the sides-first hold, it lives on the coordinator's loop and never
blocks it, and a demotion hold its timer ends logs `sp demotion unsynced`
(Log records).

The worker judges no reachability; one wait serves both cases. The
demotion hold lets a reachable old primary apply its demotion, whose first step moves its
namespaces to ANA inaccessible (`architecture.md`, Failover), before any
side fences its legs, so hosts queue their IO instead of failing it, and
its report ends the demotion hold early. One that cannot be reached never
reports,
and the wait is sized so that a host that follows the discovery log drops
its path, withdrawn from the records, before the fence
(`architecture.md`, [D18], and `architecture.md`, Host view): nvme-stas
collects discovery log changes for 1.5 s before it disconnects a path.

A fan-out during the demotion hold replaces what is held and keeps the
deadline, and a demoted cntlr is handed its newest plan at once. A further
demotion joins the demotion hold and re-arms its timer, so that every
demoted agent has the whole wait. A demoted cntlr that a newer plan makes
the primary again leaves the demotion hold and is held like any other
cntlr, and a demotion hold left with no cntlr to wait for ends. One the
newer plan does not name — a cntlr replacement deleted it, or the fan-out
left it idle (RW14) — stays in the demotion hold, because its agent may
still export its namespaces optimized: the diff has stopped its child, so
nothing reports for it, and the demotion hold waits out its timer unless a
later plan names that cntlr again. A demotion found while the sides-first
hold is pending ends that hold: its timer stops, the demotion hold keeps
the newest plans, and the sides-first hold begins again when the demotion
hold ends.

Two cases are no demotion. A cntlr replacement deletes the old cntlr, whose
child the diff stops (RW14), so its fan-out keeps RW14's order, unless it
comes during a demotion hold, which holds it like any other fan-out
(above). And a
cntlr with no running child — every cntlr of a coordinator that has just
started, after a restart or a handoff, until its first hold starts the
cntlr children, and one left idle (RW14) — has no agent this coordinator
told it was the primary, so a failover that a coordinator fans out before
its first hold has started the cntlr children runs in RW14's order. Nothing
of the demotion hold is persisted either: a new owner or a restarted worker
cannot know an earlier owner's demotion, so a failover whose owner loses
the shard before its demotion hold ends is fenced in RW14's order as well
(Known limits). The demotion hold is no correctness dependency (`architecture.md`, [D16]): at its end
the sides go whether the old primary reported or not.

RW15. **Side request.** A `SyncupSideRequest` carrying `cluster_id`,
`dn_id`, the `side_pointer` (sp id, leg id, side id), `revision` (the
`SpRev` revision) and a `side_conf` with `ext_cnt` from the group,
`zero_bytes` the group's length to zero (below), `cntlid_slot` from the
side, `primary_cn_id` the `cn_id` of the cntlr with `primary` set (zero
when cntlrs exist but none is primary; an SP with NO cntlr gets no side
request at all, its side children staying idle — SPD7), `standby_id_list`
the `cn_id`s of every other cntlr — disabled ones included, a disabled
cntlr keeps its standby shape (`cnagent.md` CN9) —, `sp_level` from the
`SpConf` and `provisioned` from the side. The length to zero is the part
of each side of the group that `architecture.md`, Side provisioning
protocol, says to zero: for a group of the meta list its leg span, the
group's `meta_blocks` plus its `data_blocks`, and for a group of the data
list its meta region and first data block, its `meta_blocks` plus one,
each block of the pool's `bdev_conf.dm_pool_conf.data_block_size`
(`model.SideZeroBytes`), and for a clone meta group the length of a group
of the data list, not its leg span (`model.CloneMetaSideZeroBytes`;
`architecture.md`, Side provisioning protocol, says why). It is computed
once per group from stored values alone — no extent size and no cluster
conf — and every side of the group
carries it: a leg's, a migration's source and destination, the side of the
group's repair's new leg and a retired side of the group, provisioned or
not. A side of a clone meta group carries no migration conf: no migration
moves its leg (`architecture.md`, Clones). The side of a repair's new leg
is sent `provisioned` from the side, as a leg's side is, and a retired
side's request says `provisioned` true whatever its
record holds (`architecture.md`, Side provisioning protocol, says why);
neither carries a migration conf, since neither is a leg a migration moves
(`architecture.md`, Repair).
If the side's leg has two sides and a
`Migration` of the SP names this side as
`src_side_id`: a `migr_src_conf` with `migr_id`, `dst_side_id`,
`dst_dn_id`, `dst_provisioned` — the destination side's `provisioned` while
the `Migration`'s `cutover_pending` is clear, and false while it is set, so
the source serves on until the cutover (RW18; `architecture.md`,
Migration) — and `cutover_pending` from the `Migration`; as
`dst_side_id`: a `migr_dst_conf` with `migr_id`, `src_side_id`, `src_dn_id`,
`src_nvme_tr_conf` (the source side's `nvme_tr_conf`), `block_size` from
`bdev_conf.dm_pool_conf.data_block_size`, `meta_blocks` from the group, the
migration's `dm_clone_conf`, and `bm_cnt` and `cutover_pending` from the
`Migration`. The
migration's `dm_clone_conf` is the one conf still filled in here (a zero
`hydration_threshold` or `hydration_batch_size` takes its default of
`architecture.md`, Common validation, on a copy — the loaded state is
shared with every child). `block_size` and everything else is sent as
stored; a zero in the SP's geometry, or a group whose block counts give no
length to zero, was refused by RW14's gate instead.

RW16. **Cntlr request.** A `SyncupCntlrRequest` carrying `cluster_id`,
`cn_id`, the `cntlr_pointer` (sp id, cntlr id), the revision, `bdev_conf`
from the `SpConf`, `sp_level`, `cntlr` (the `Cntlr` record), `id_to_slice`
keyed by the slice id rendered with `IdKeyFmt` — the key the cn agent reads
—, `td_list` in `td_name_list` order, `nqn_to_subsystem`, `clone_list`,
`xfer_list` and `name_to_repair`, every repair of the SP by its name, at
every stage (`architecture.md`, Repair). Every cntlr of the SP receives the
full state (`architecture.md`, sp role) — less any clone whose `deleting` is
set, which is absent from every cntlr's `clone_list` (CLD5), and less the
repair of such a clone's group, which the clone drain's final transaction
gives back with the group, and a repair whose group the load does not find,
which no group of the request names.

RW17. **Rounds** send a `CheckSideRequest` or a `CheckCntlrRequest` that
names the object by its pointer and node, carries the driven revision,
`show_info` false (RW4) and the round's own `trace_id`.

RW18. **Provisioned flip.** On an ACCEPTED
`SyncupSide` or `CheckSide` reply (code zero or `ReplyCodeLeftover` — the
gate RW19 defers to, HL1 the reason) for a side whose driven request
(`provisioned` moves only from false to true, so the request being driven
and the last one the agent acknowledged cannot disagree here) carried
`provisioned` false and whose `side_info` reports `zeroed_bytes` equal
to a non-zero `zero_bytes`, the child reports the side to the
coordinator, which runs `model.FlipProvisioned`, or for a side of a clone
meta group the clone flip, `model.FlipCloneProvisioned`, in an STM of its
own after the slice flip's (several sides reported within one round MAY
share one STM of each), or for the side of a repair's new leg the repair
flip, `model.FlipRepairProvisioned`, one STM per side after the clone
flip's. A side of a clone being deleted is not reported: the clone flip and
the repair flip would skip it (MD6). A retired side's request says
`provisioned`, so it is never reported. The bump re-fans the SP (RW14) and the
re-synced sides carry `provisioned` true. Nothing is remembered across a
handoff: the new owner's first round that gets a reply carries the full
`*Info`, as the first reply on every stream does (a missed round drops its
stream), and that info becomes the latest known one. That round answers
from an observation no round asked for and flips nothing (HL7); the first
later reply that HL7 lets give a verdict — a `Syncup*` reply the round
brings, or the first round that answers from an observation made after
it — flips whatever is still false.

**The cutover** of `architecture.md`, Migration, runs beside the flip. A
migration of the loaded state is a candidate when its `cutover_pending` is
set, a leg holds exactly its two sides and its destination side is
`provisioned`. The coordinator starts it when two reports hold: the
destination side's latest `SideInfo` reads `migr_dst_info.target_info`
`RES_STATUS_PROVISIONING` with the reason `RES_REASON_AWAITING_CUTOVER`
(`dnagent.md` DN13, DN18), and a cntlr that the plan the coordinator last
decided makes primary — the primary HL2 records leg rows from — and not
`disabled` reported the
destination side's `side_id_to_path` row `RES_STATUS_OK` (`cnagent.md` CN28)
in the drain of reports the coordinator is handling, read from a reply's own
info: for a round, what its stream last carried; for a `SyncupCntlr` reply,
the info that reply carried; and of one cntlr's replies that the drain
folds, the last counts, live or not. It never reads the latest info a child
holds, which can be another reply's, one from before a failover among them.
A round of the destination side whose stream breaks or that misses, a
round whose observation is too old included (HL7), marks the side's rows
`RES_STATUS_UNKNOWN` (`architecture.md`, Live-state reporting), so the
side reads no wait until a later reply reports it again.
A standby's path counts for nothing, so after a failover the cutover waits
for the new primary's own report. A disabled primary's path counts for
nothing either: its agent runs the standby shape (`cnagent.md` CN9), with
no arrays and no thin pools, and its disable is AR5's trigger on its own,
so the cutover waits for the report of the primary that takes over, or of
the same cntlr once it is enabled again.
The coordinator runs `model.StartCutover`
(MD6) for it, after the provisioned flips and before the created flip,
one STM per migration. One that commits leaves the candidates, and its bump
re-fans the SP, whose side requests then carry the cleared flag (RW15); one
that fails or is refused is logged and tried again on a later report. The
coordinator does not read the pool's level; `model.StartCutover` refuses at
`SP_LEVEL_NO_MIGRATION` and above (MD6), so the cutover waits there, even
on a destination report of the wait from before the level rose. Its
reasons are those of `architecture.md`, Migration. Nothing of it is
remembered across a handoff either: the new owner's first rounds carry
the full `*Info`, and the cutover runs off the first later replies that
HL7 lets give a verdict, as the provisioned flip does (above).

RW19. **Created flip.** Every `SyncupCntlrReply`
and `CheckCntlrReply` with an accepted code (RW18: code zero or
`ReplyCodeLeftover`) is scanned: a td of the loaded state with `created` false
is a candidate when `cntlr_info.td_id_to_thin_info` holds its `td_id`, that
entry's `slice_id_to_dm_thin` key set equals the SP's slice ids exactly, and
every row is `RES_STATUS_OK`. The entry's `torn` mark, the cn agent's report
that the snapshot is torn (`cnagent.md` CN14), travels with the candidate,
and `model.FlipCreated` writes it into the record's `torn` in the put that
sets `created` (MD6; `architecture.md`, Thin devices). A reply's mark counts
only for a td that reply completes; when the replies one drain folds into
the call complete a td more than once, the td is flipped torn if any of
them marks it. A `SyncupCntlrReply` and a `CheckCntlrReply` carry the mark
alike, so the flip records it whichever reply completes the td. Reason: the
mark lives only in the agent's memory until the flip, and the thin device
record is the one place an operator reads it from (`ListThinDevices`).
Candidates go to `model.FlipCreated`; a reply
that completes none causes no etcd traffic. The call commits them
`MaxFlipCreatedPerTxn` at a time, in list order, one STM each, and each STM
that wrote bumps `SpRev` once: the coordinator folds every td one drain of its
reports completed into one call, a set only `MaxTdCntPerSp` bounds, and one
STM over all of it would exceed `EtcdMaxTxnOps` long before that bound and be
refused on every round. One such STM compares every key it read and every key
it wrote — a get of each candidate's td key, a put of each one it flips, and
`SpRev`'s read and put — so it costs at most two compares per candidate plus
two, which `MaxFlipCreatedPerTxn` keeps within `EtcdMaxTxnOps`. A pending
provisioned flip of the same SP MAY share one of those STMs; that STM then
also reads the record that holds each side the provisioned flip lists, a
`Slice`, a `Clone` or a `Repair`, and writes each record it changes, up to
two compares per record over the count above. This one does not take it: RW18's
provisioned flips each run in STMs of their own, before the created flip's.
When a call fails at its second STM or a later one, the coordinator logs that
failure and no `flip applied` record (Log records), not even for the tds the
earlier STMs committed: those stay `created` in etcd, and the refs
`model.FlipCreated` returns with the error go unlogged.

RW20. Health of sides and cntlrs per HL2; pushes per Bitmap pushes; the
reaction pass runs on the coordinator's own ticker (AR1).

### `ClusterConf` cache — `worker/clusterconf.go`

RW21. One per process: a `Range` of `ClusterConfPrefix`, then a `WatchTyped`
from that revision plus one, decoding `ClusterConf`. Entries are keyed by
`ClusterId` of the name and the creation epoch — the name is the key
suffix, the epoch is in the value — and a delete removes the entry. Readers
receive an immutable snapshot of the entry **exactly as etcd holds it**:
the cache resolves nothing, because `CreateCluster` made every defaultable
member concrete when it wrote the key (`architecture.md`, Common
validation), so a zero read back here is corruption or foreign data, not an
omission. Each reader validates the snapshot with
`model.ValidateClusterConf`, the clone drain's final step through the model
op it hands the snapshot to (CLD7), and **refuses** rather than guessing a
geometry the rest of the cluster may not agree with; the cache validates
nothing itself, because its five readers refuse differently — idle the loop
(RW9), keep the tick cadence and skip the pass (AR1), fail the DN health
write (HL1), fail the clone drain's final step, which the next pass runs
again (CLD7) — and a validating get would either hide that or log it five
times.
An invalid conf is deliberately still **cached**: dropping it would make a
cluster whose conf went bad indistinguishable from a deleted one and send
the operator chasing a phantom deletion that never happened. A cluster
**absent** from the cache is the separate RW9 and SW6 idle path, with a
record of its own. Watch errors and compaction are handled like SW4.

## Health bookkeeping — `worker/health.go`

HL1. **Nodes (dn/cn roles).** Evaluated per round and per syncup reply on
the node's own stream; written through `model.SetDnErrEpoch` and
`SetCnErrEpoch`, which maintain the capacity key in the same STM (MD4;
`architecture.md`, Capacity index keys). The effect of each reply or
missed round on `DnConf.err_epoch` or `CnConf.err_epoch`:

* the stream cannot be opened, breaks, or no reply arrives within the round
  timeout, or a reply answers from an observation HL7 finds too old: set to
  now if zero; the in-memory info is marked
  `RES_STATUS_UNKNOWN` (what the worker records itself while the stream is
  dead, `architecture.md`, Live-state reporting — never written to etcd);
* any `RES_STATUS_ERROR` row in `DnInfo` (`disk_info`, `meta_info`,
  `port_info`) or `CnInfo` (`port_info`), save on a round that answers from
  an observation no round asked for (below): set to now if zero;
* a clean round — a reply in time from an observation that is not too old
  and that a round asked for (HL7), an accepted code (zero or
  `ReplyCodeLeftover`), no `ERROR` row in the latest known info: cleared to
  zero;
* a round that answers from an observation no round asked for (HL7):
  neither set nor clear, whatever its code and rows; it still triggers the
  re-sync of RW4 step 5 when its code or revision calls for one;
* `RES_STATUS_PROVISIONING` and `MISSING` never set it
  (`architecture.md`, [D15]); neither is an `ERROR` row, so a reply that
  carries them and no `ERROR` row is the clean round above and clears it.
  Legs are the exception: HL2's `Leg.err_epoch` clears on the primary's
  `RES_STATUS_OK` alone, so a `PROVISIONING` or `MISSING` leg row neither
  sets nor clears it;
* a rejection code (stale revision, unknown object, invalid conf, or one this
  worker does not know): neither set nor clear; it triggers a re-sync (RW4
  step 5);
* `agent_reply.code` equal to `ReplyCodeLeftover`: evaluated exactly as code
  zero — the rows above set it, clear it or do neither — and the re-sync of
  RW4 step 5 still runs. The request WAS applied, so the `*Info` is a full
  probe of every WANTED object; a leftover is by definition an object nothing
  wants, so it has no row of its own to be judged by. Reading the code as a
  rejection would freeze health — and the pushes of Bitmap pushes, and the
  RW18 and RW19 flips — for as long as one leftover survived.

The disk counts a `DnInfo` carries beside its rows (`dnagent.md` DN18) are
no row: whatever they say, they neither set nor clear `DnConf.err_epoch`,
and the marking of a dead stream's info drops them rather than marking
them, so RW13's count compare waits for counts a live stream reports.

The **DN** write needs a usable `ClusterConf` and re-reads it from the cache
per write rather than capturing it: MD4 derives the capacity key's bin
index from `dn_bin_conf`, so a conf that is missing from the cache, or that
fails `model.ValidateClusterConf`, fails the write and leaves it to the
next round (RW12). It re-validates rather than trusting RW9's gate because
it is not COVERED by one: that gate checked the conf the round was built
from, while this write is the health monitor's, issued against a conf
re-read from the cache after the gate ran — one the watch goroutine may
have replaced in between. It is not the only worker STM write that takes a
`ClusterConf` — the sp role's `GrowSlice`, leg repair's `CreateRepair`,
`FinishRepair` and `CancelRepair` (AR8), the retire timer's
`DeleteRetiredLeg` (AR12), and the sp drain's `DrainSpRepairs`,
`DrainSpRetiredLegs` and `DrainSpSlice` take one too (MD6, HL6) — but those
sit behind AR1's cluster-conf gate, and `GrowSlice` re-checks both confs,
`FinishRepair`, `CancelRepair`, `DeleteRetiredLeg` and the three drain
batches the cluster's, at the top of their own STMs besides; the clone drain's `FinishCloneDelete` takes one as
well, ahead of every gate of the pass, and refuses an unusable one at the
top of its own STM (CLD7). Guessing a ladder here would
leave the real capacity key undeleted, and the bin scan of
`architecture.md`, Finding DN candidates, would go on offering it as an
allocation candidate for a DN HL1 has just flagged unhealthy — a DN whose
`err_epoch` is set implies no key at all (MD4), so nothing is written in
its place. The **CN** write needs no conf at all: CN capacity keys carry no
bin index (`architecture.md`, Finding CN candidates).

HL2. **SP objects (sp role).** Written through `SetCntlrErrEpoch`,
`SetLegErrEpoch` and `SetSideErrEpoch` and, for a leg or a side of a clone
meta group, whose record is the clone's, through their clone-keyed twins
`SetCloneLegErrEpoch` and `SetCloneSideErrEpoch`; for a repair's new leg and
its side, whose record is the repair's until the finish, through
`SetRepairLegErrEpoch` and `SetRepairSideErrEpoch`, the repair named by its
name and proved by its `rep_id`; and for a retired side through
`SetRetiredSideErrEpoch` (MD6; `architecture.md`, Repair). Each monitor
writes to the record that owns its object as the plan that started it
named it (RW14): a side child whose side moved to another record is
restarted with a fresh monitor, and the coordinator drops a leg monitor
whose leg's record changed, so the leg's next row starts a fresh one.
The rules are the same for every kind of record:

* `Cntlr.err_epoch` is set to now (if zero) when its `CheckCntlr` stream
  cannot be opened, breaks or misses a round — a round whose observation
  HL7 finds too old included — or any `RES_STATUS_ERROR` row of its
  `CntlrInfo` **other than** `leg_id_to_leg` and `side_id_to_path` reads
  so; it is cleared when its next round is clean (HL1). The write keeps the
  SP's discovery records to the
  listing rule of `architecture.md`, [D18], in the same transaction (MD6):
  an enabled standby leaves them when its epoch is set and returns when it
  is cleared, while an enabled primary stays. A clean reply of a cntlr its
  child drives as a standby, at a revision below the one the child drives,
  neither sets nor clears: the agent of an old primary that resumes still
  runs the primary shape at the old revision, possibly over legs its sides
  have fenced, and clearing its epoch would list it again while its
  namespaces are optimized. The `Syncup*` of the driven revision, which RW4
  step 5 or RW6 sends next, brings the verdict. **Settle**:
  `Cntlr.settling` is cleared when the record is settling and a clean round
  is a reply of the cntlr as an enabled **primary** at the revision its
  child drives — the child's plan says `primary` and the record it carries
  is not `disabled` (the agent's own effective role, `cnagent.md` CN9), and
  the reply's `revision` equals the plan's, and the reply shows the stack
  built: no row of the maps this row is judged by (every map of rows but
  `leg_id_to_leg` and `side_id_to_path`), `grp_id_to_md_raid` aside, reads
  `PROVISIONING`, or
  `MISSING` with details other than `cnagent.md` CN19's "sp_level"; with the
  `err_epoch` clear in one write, or in a write of its own when `err_epoch`
  is already zero.
* `Leg.err_epoch` is set to now (if zero) when the **primary** cntlr's
  `leg_id_to_leg` row of the leg reads `RES_STATUS_ERROR` (the health-block
  probe of `architecture.md`, Group on-leg layout: meta region, data region,
  health block; repairs' new legs included) — a standby's leg row
  is logged, never recorded — and cleared when the primary reports the row
  `RES_STATUS_OK`. A retired leg has no leg row: no cntlr connects it.
* `Side.err_epoch` is set to now (if zero) when its `CheckSide` stream cannot
  be opened, breaks or misses a round — a round whose observation HL7 finds
  too old included — or `side_dev_info` or any
  `cn_id_to_dm_error`, `cn_id_to_dm_linear` or `cn_id_to_nvmeof` row is
  `RES_STATUS_ERROR`, or a `migr_src_info` or `migr_dst_info` row is `ERROR`;
  it is cleared when its next round is clean. A sides-first hold whose
  fan-out loads the epoch set does not wait for the side (RW14).

The path rows of `side_id_to_path` (`cnagent.md` CN28), a primary's and a
standby's alike, set and clear none of the three: they exist for RW18's
cutover alone, and a leg's health is its leg row's. The primary's verdicts
on repairs and the groups' whole flags and in-sync lists, which a
`CntlrInfo` carries beside its rows (`rep_id_to_repair_info`,
`grp_id_to_grp_info`), are no rows:
they set and clear none of the three, the marking of a dead stream's info
leaves them as they are, and the pass reads them as fields (AR8, AR11,
AR12, AR13).

`PROVISIONING`, `PENDING`, `MISSING` and a rejection code never set any of
the three, and `Leg.err_epoch` clears on the primary's `RES_STATUS_OK`
alone. `PENDING` is the primary's leg row while its prober for the leg has
not completed a round (or none is registered yet) — a first round stalled
past `CnLegProbeStallSeconds` reads `ERROR` — and a fresh prober starts at
every promotion and agent restart (`cnagent.md` CN11); were that row to
read `OK`, every promotion's fresh probers would clear a dead leg's
`err_epoch` and restart AR8 case 1's `leg_unhealthy` clock. A round that
answers from an observation no round asked for sets and clears none of the
three, whatever its rows (HL7).
`ReplyCodeLeftover` is not a rejection: its rows are evaluated exactly as a
code-zero reply's — here, and in the RW18 and RW19 reports the same replies
carry — for HL1's reason. The **primary** of the `Leg.err_epoch` row is the
cntlr that the plan the coordinator last decided makes primary: the held
plan while a hold keeps one for the cntlr (RW14, RW22), else the one its
child was handed — for a demoted cntlr the standby plan, handed at once
(RW22). A demoted primary's agent runs the primary shape until it applies
its demotion, possibly over legs the sides have already reloaded onto
dm-error, so its rows describe that fence, not the legs: the rows it built
as the primary and that are read after the hand-over are dropped — they
neither set nor clear a `Leg.err_epoch`, and, unlike a standby's, they are
not logged — and its child logs the rows of the later replies as a
standby's.

**Row classes.** The `ERROR` rows a cntlr is judged by — every map of rows
but `leg_id_to_leg` and `side_id_to_path` — are of two classes. A **shared-state**
row belongs to the stack of a td whose `created` is set and whose thin row, in
some slice, reads `ERROR` with the reason `RES_REASON_THIN_ID_LOST`
(`architecture.md`, Live-state reporting): the cn agent sets it when dm-thin
refuses a created td's thin table with ENODATA because the slice's pool has
lost its id (`cnagent.md` CN14). The class reads that field, never a row's
details:
a control-plane decision does not parse a tool's English. The stack is the
td's own rows — its thin rows, its raid0 and its dm-error — and every row
of an object that exists for it alone: the ns-dev and the nvmet namespace
of each of its namespaces, the three rows of a transfer out of one of
them, the two of a clone onto it. The pool lives on the SP's legs, so
whichever cntlr holds the primary role reads the same rows, and no
failover brings the td back: an operator does (`architecture.md`, Known
limits). Every other `ERROR` row is the cntlr's **own**. Both classes set
`Cntlr.err_epoch` as the rules above say — the row stays visible and the
epoch set — and the class steers AR5 and AR7 alone: a report whose
`ERROR` rows are all shared-state is no failover trigger, nor a
replacement one for a primary with no failover candidate. Only a
converge's observation carries the reason (`dnagent.md` SH28). A
background read's probe finds the volume absent and reads it `MISSING`
with empty details and no reason, while the raid0 over it still reads
`ERROR`, so a report from a read's observation of the same loss holds own
rows only. It is the second refusals that bound what those
rows cost: AR5's keeps the role from moving back and forth over them, and
AR7's, once a primary with no failover candidate has been replaced over
them, keeps its replacement, which reads the same rows, from being
replaced in turn (Known limits).

**The settle.** `Cntlr.settling` is set by the op that makes the cntlr primary
(`Failover`, a primary `ReplaceCntlr`, MD6; the gateway's `CreateStoragePool`
for the SP's first primary, and its `UpdateCntlrEnabled` when it re-enables a
cntlr that is still primary — its agent then builds the primary stack from the
standby shape it held while disabled) and cleared by `SetCntlrErrEpoch` with a
zero epoch and the settle argument on the settle condition above, or by a
`Failover` that demotes the cntlr before it settles (MD6) — the flag describes
a primary, and a standby's would steer nothing. The revision gate is
load-bearing: when the promotion's `SyncupCntlr` never reached the agent (a
transport failure before the agent stored the request), the next Check round's
reply carries the previous revision and describes the **standby** shape —
clean, and meaningless for the promotion (RW4 step 5 re-syncs it). One the
agent applied but whose reply was lost leaves the agent at the driven
revision, and a Check reply that answers from an observation made at that
revision (`dnagent.md` SH28) reports the primary shape. Rounds and
syncups run on the child's one goroutine (RW1), and a revision carries one set
of roles: a role change is agent-visible desired state, which bumps `SpRev`
(`architecture.md`, Revision keys and the sync fan-out), and RW14 builds no
plan from a state newer than the revision it labels the plan with (an etcd
restore, which can hand a revision number out twice, aside). So an accepted
reply at the driven revision from a cntlr that is primary and not disabled is
a report of the primary shape; a disabled primary converges the standby shape
(`cnagent.md` CN9), so its clean reply settles nothing. Nor does a reply whose
stack is not built yet (`primaryShapeBuilt`): a `PROVISIONING` row, or a
`MISSING` one whose details are not `ResDetailsSpLevel`, in a map the settle
reads is clean and no proof of the build that follows, and settling on it
would leave that build to `primary_unhealthy`. Which rows read `PROVISIONING`,
`MISSING` or `ERROR` in which state — a slice deferred by its first zeroing, a
member a converge found unavailable and left to the connect retry, a device a
background read probes absent, a clone whose source is not connected, has an
endpoint with no live controller or no namespace, or could not be read, a
clone whose destination td or clone meta group still zeroes, whose two
rows read `PROVISIONING` below `SP_LEVEL_NO_CLONE`, a clone with no
dm-clone while its clone meta group is not ready, whose dm-clone row
reads `MISSING` — is the cn
agent's (`cnagent.md` CN9, CN10, CN16, CN17, CN18); a
primary showing such a row stays settling, held to the longer threshold, until
it clears. Four kinds of row are left out of that test, each for one reason.
Leg rows: a repair's new leg still zeroing reads `PROVISIONING` there for
as long as it zeroes. Path rows: a migration's destination side reads `PROVISIONING` there
for as long as it zeroes. Group rows: a grow appends its new groups to their
lists, and while their sides zero they stay out of the live concat, so their
group rows alone read `PROVISIONING` beside a serving pool while those sides
zero — and nothing else needs them, since a group of a new SP that is still
provisioning defers its whole slice, whose pool rows say so, and a group a
converge could not assemble leaves the pool rows over it `ERROR` or `MISSING`;
a clone meta group has no pool over it, and below `SP_LEVEL_NO_CLONE` its
clone's rows show it, `PROVISIONING` while its sides zero and the dm-clone
row `MISSING` while it is not ready and no dm-clone is there (`cnagent.md`
CN18), while at that level and above no layer over it is wanted.
Rows the
`sp_level` suppresses: they read `MISSING` with `ResDetailsSpLevel`
(`cnagent.md` CN19) and hold nothing, so a primary does not wait for a layer
its level suppresses; the deferral of a slice does not depend on the level,
though — its ns-dev, namespace and transfer rows read `PROVISIONING` while it
is deferred (`cnagent.md` CN16, CN17) — so at a level that suppresses the
pools a primary with a namespace or a transfer still settles only once the
zeroing ends, and one with neither, or any primary at `SP_LEVEL_DISABLE`,
settles on a reply that shows no pool built, possibly before its sides are
even zeroed; AR3 suppresses AR5 at all of those levels, so the longer hold
changes no reaction there. A primary of an SP still in its first zeroing
therefore stays settling, judged by the longer threshold, until the build
that follows reports its stack built and clean — indefinitely if a side never
finishes, since a leg still provisioning never sets an `err_epoch` for AR8 to
repair it by (Known limits). That settle is final — `UpdateStoragePoolLevel`
re-arms nothing — so the build that follows a lowered level is judged by
`primary_unhealthy`, as any settled primary's is. The child keeps a memo of
the flag, seeded from the record in every plan it takes (RW14's plan copies
it), so the settle is normally written once and logged as `cntlr settled`
(Log records): a plan loaded after the settle write clears the memo; one
loaded before it costs one redundant write that changes nothing and one extra
`cntlr settled` record; a failed write keeps the memo and is retried on the
next reply (RW12). The flag steers AR5 alone — and `Failover`, which
re-validates AR5 in its STM — and is re-read from etcd on every pass: health
bookkeeping of the same kind as `err_epoch`, not a memo of a failed step.

HL3. **Transitions only.** A record is written when the observed health
changes (healthy to unhealthy sets the epoch once — while the record stays
set, the threshold clock of Automatic reactions never restarts; unhealthy
to healthy clears it). The op re-reads the record inside its STM, so two
owners observing the same transition write once. Health never bumps a
revision (`architecture.md`, Revision keys and the sync fan-out). What a
health verdict is compared with is a cache of the record, not only a memory
of the monitor's own writes, and these loads re-seed it: the sp
coordinator's reaction pass, every `cntlr_interval` (AR1), hands each side
and cntlr child its record's `err_epoch`, which the child folds in before
it judges its next reply or missed round, and re-seeds the leg monitors it
keeps itself — the legs and sides of the SP's slices and of its clones'
clone meta groups alike, the new legs of its repairs with their sides, and
its retired sides; the dn and cn roles re-seed from the `DnConf` or
`CnConf` every syncup reads (RW13), and since a node whose revision stays put and
whose agent answers code zero never syncs, a dn or cn monitor also
re-reads the record itself before a verdict once `nodeRecordMaxAge` has
passed without its reading or writing it — one read per `nodeRecordMaxAge`
for a node that neither syncs nor changes health, a DN's taking its `DnRev`
with it (RW13), and one at each verdict while that read fails. A monitor
that has written nothing yet — a child just started, a new owner after a
handoff — starts empty and writes its first verdict whatever the record
holds. So a record that says unhealthy
while the health verdict is clean is a transition and clears, and one that
says healthy while the health verdict is not sets a new epoch: an `err_epoch`
another observer wrote or cleared — the other owner of an overlap (VW7), a
partitioned observer (Known limits) — is corrected at the owner's first
verdict after its next load, a reply that neither sets nor clears being no
verdict (HL1, HL2): within a pass and a round for a side, a leg or a cntlr,
and within a `nodeRecordMaxAge` and a round for a DN or a CN. No threshold
reaction fires on such an epoch before the owner's own verdict reads the
object unhealthy (AR10), and its clock then runs from the stamp the record
already holds (AR4). While two owners disagree,
each puts the record back to its own view after each of its loads, so the
threshold clock of an object one of them sees unhealthy restarts after each
of the other's clears, and a threshold longer than a pass and a round —
every default but `primary_unhealthy` (AR4) — is not reached meanwhile. A
load that may predate a side's or cntlr's own latest write is not folded
in: the pass reads the child's count of its own writes before it loads,
and the child drops an offer whose count has moved since. Folded in, such
an offer would hand back the state the write replaced, so a next verdict
reversing the write would write nothing and one repeating it would write
again. The next pass offers again. HL2's settle is the one write that can
be issued with no health transition behind it — a standby that was clean,
is promoted and reports clean at once has no edge to write on; when a
transition is due, the settle rides in that same write (HL2) — and it too
is written once per memo, the op re-reading the record so that a second
owner's settle writes nothing.

HL4. A `Syncup*` reply's `*Info` is processed exactly like a `Check*`
reply's (the secondary signal, `architecture.md`, Check streams), but for
a cntlr's run of unhealthy health verdicts, to which only rounds add
(AR10), and for its observation, which the loop sees but which never adds
to a run (RW23) and is never judged by its age (HL7). A `Syncup*`
gRPC failure is not by itself a health failure — an unreachable agent
breaks the stream too.

HL5. `show_info` false streams carry an `*Info` only when something
changed; the worker evaluates health on the latest known info, and a reply
with neither info nor error is a clean round when the observation it
answers from is not too old and is one a round asked for (HL7).

HL6. **No cross-role health writes.** The sp role's health bookkeeping —
this section's `err_epoch` writers — never touches `DnConf` or `CnConf`
(it writes `Cntlr`, `Leg` and `Side` — a `Leg` inside its `Slice`, `Clone`
or `Repair`, a `Side` inside those or inside its `RetiredLeg` — and the
`CdcEntry` keys a cntlr write rewrites (HL2), nothing else), and the dn/cn
roles never touch SP records. (The sp role's *reactions* do rewrite `DnConf`
and `CnConf`
through the model ops — `GrowSlice` charges DNs and every cntlr CN,
leg repair's `CreateRepair` charges the DN of its fresh new leg, its
`CancelRepair` gives that side back and its `FinishRepair`, at the
retired-leg bound, the oldest retired side (AR8),
`ReplaceCntlr` rewrites two `CnConf`s
— and so do the sp drain, whose D1 credits every cntlr CN and whose
repair, retired-leg and D2 batches credit every DN they touch
(`model/drain.go`), the clone
drain's final STM, which gives a clone meta group's extents back to their
DNs (CLD9), and the retire timer's `DeleteRetiredLeg`, which gives a retired
side's extents back to its DN (AR12) — but
that is allocation bookkeeping, not health.) An unreachable DN is therefore
marked
on `DnConf` by its dn-role owner and on the `Side` records of its sides by
each sp-role owner, independently.

HL7. **The observation's age.** An agent answers a round from its latest
observation of the object (`dnagent.md` SH28). The worker takes its age as
the agent's answer time less the observation's time, both from the reply's
stamp and both by the node's clock, so no clock difference between the
worker's host and the node enters a verdict. It counts the age from no
earlier than the first round after a pause in its rounds of the object,
and from that round alone for a reply that carries no observation time,
and keeps the agent's answer time on that round until the next pause
(RW2). A pause is the loop's start, an idle or a refused conf (RW9), or a
move to another endpoint (RW13); its first round is the first that the
agent answers in time without refusing the object as unknown. A gap
between two rounds of a running loop is no pause: a round that waited out
a long `Syncup*` behind a converge that holds the object's reads off finds
the observation as old as it is. A round whose observation ended before
the agent answered that first round, or that has none, answers from an
observation no round asked for. Until the observation is too old, such
a round gives no health verdict, settles nothing (HL2), makes no count compare (RW13),
counts in no run and reports nothing to the sp coordinator: no flip, no
cutover report, no leg row (RW18, RW19, HL2) and no revision for RW14's or
RW22's hold. Its `*Info`, when it carries one, still becomes the latest
known info (HL5), and it still brings the re-sync of RW4 step 5, whose
`Syncup*` reply is processed as any other (HL4). The first round that
answers from an observation that ended after that answer — normally the
next round, which answers from the read the first round started — is
judged as any other (Known limits). A round answered with
`ReplyCodeUnknownObject` is not judged by its age: the agent holds no
state of the object and so no observation of it, and the re-sync of RW4
step 5 that this reply brings is what makes the object known. A round
whose observation's age, so counted, exceeds `ObsMaxAgeIntervals` of the
object's intervals, or whose reply carries no answer time to count it by —
an observation too old — is a missed round in every way (RW4 step 4): the
worker logs `check round failed` with the cause, drops the stream, and
gives the "unreachable" verdict of HL1 and HL2 — the rows read
`RES_STATUS_UNKNOWN`, a side's wait for the cutover is off (RW18), the
health epoch is set until the next clean round — which adds to a cntlr's
run on every such round (AR10); it issues no `Syncup*` on that reply. A
round whose observation's age, so counted, exceeds one interval, or whose
reply carries no answer time, is logged as `late observation` (Log
records), too old or not, save the first round after a `Syncup*` reply
when it answers from that reply's observation and that observation is not
too old. After
the re-send a round issues (RW4 step 5), and after an immediate syncup
that abandons a round (RW6), the loop waits a whole interval from the
reply (RW4 step 6), and no read of the object ends meanwhile: only a round
starts one (`dnagent.md` SH29), and the syncup cancelled the read the
round before it had started, if one still ran (`dnagent.md` SH30). So
that round is late by the loop's own wait. After an immediate syncup sent
between rounds, the round timer, armed after the round before (RW8),
keeps running, so the next round comes less than an interval after the
reply and is seldom late at all. A converge in flight that the
stamp reports is not unhealthy by itself, and a `Syncup*` reply's
observation is never judged by its age. Reason: a retry attempt or a
migration's converge can hold an object's reads off for seconds at the
largest pools, which a bound of several intervals lets pass where a round's
one-interval wait would not; a node whose reads hang is found later than
that wait alone would find it, by about the bound less one of the object's
intervals; and a hang can sit in a read as well as in a converge. The age
counts from the first round after a pause because only a round starts a
read (`dnagent.md` SH29): while no round asks — the worker restarts, fences
itself (VW8), takes over the shard or idles — every observation ages with
no fault of the agent's, and judged from its own time the first round
after the pause would set the health epoch of every object. An
observation that ended before that first round tells of the object as it
was while no round asked, so no verdict rests on what it reports.
The stream drops because the agent leaves an unchanged info out on a kept
stream, which the unknown marking would then read as clean.

## Bitmap pushes — `worker/bmpush.go`

The worker side of `architecture.md`, Bitmap push protocol, restated as
rules of the sp children.

BM1. **Sources.** For a side that is a migration's destination: the
`MigrBitmap` chunks of that migration, indexes from zero below `bm_cnt`.
For the cntlr that is **primary**: the `CloneBitmap` chunks of every clone
of the SP that is not deleting (CLD5), each addressed by the pair
`(src_slice_idx, bm_idx)` (MD2).
Chunk addresses come from the snapshot's keys-only scans (`MigrBmIdx` and
`CloneBmIdx`, MD3) — a migration chunk's address is its `bm_idx` alone,
carried at slice index zero; chunk **values** are read one at a time
(`Get`) when pushed, at the key the address names.

BM2. **Diff.** After every `SyncupSide` reply: the missing set is
`MigrBmIdx` of the migration minus `bm_info.bm_idx_list` (only when
`bm_info.res_id` is the `migr_id`). After every `SyncupCntlr` reply on the
primary: per `bm_info_list` entry (its `res_id` the `clone_id`), the
missing set is `CloneBmIdx` of the clone minus `bm_info.chunk_id_list`. A
clone reports `chunk_id_list` and leaves `bm_idx_list` unset — that field
is migration-only — and both sides are compared by the WHOLE pair, so one
`bm_idx` acknowledged on one source slice says nothing about the same
`bm_idx` on another. A clone absent from the list, and every pair a listed
`chunk_id_list` omits, has everything missing.

BM3. **One in flight per migration/clone, ascending address,** each a
`PushMigrBitmapRequest` or a `PushCloneBitmapRequest` that addresses one
chunk of the object's bitmap and carries its bytes, under
`DefaultWorkerPushTimeout`; the next part is sent only after a
code-zero reply. The order is ascending `bm_idx` for a migration and
ascending `(src_slice_idx, bm_idx)` **lexicographic** for a clone, so that
two source slices sharing a `bm_idx` are two ordered chunks and not one.
The ordering is LOAD-BEARING FOR MIGRATIONS ONLY, whose chunks concatenate
in `bm_idx` order; a clone's chunks are self-positioned pairs, so there the
order is deterministic and nothing more. Different migrations and clones
push independently and MAY run concurrently toward one agent
(`architecture.md`, Bitmap push protocol); within one object the child
sequences them.

**Neither request carries a revision**, and no agent gates one on a
revision. A chunk is position-addressed data keyed by a `migr_id` or
`clone_id` that is never reused (`architecture.md`, Globals: id allocation
+ shard buckets), applying it advances no stored revision, and an agent
that does not hold the object refuses the push BY NAME
(`ReplyCodeUnknownObject`, BM6) rather than by comparing revisions. A push
planned from a report the desired state has since superseded is therefore
either still correct or refused by name; gating it on a revision would only
ever discard work that was about to be redone anyway.

BM4. **Targets.** Migration chunks go only to the destination side's DN;
clone chunks only to the CN hosting the **primary** cntlr. A standby's
`bm_info_list` is ignored. After a failover the new primary's syncup reply
reports an empty or partial set and the pushes follow it there.

BM5. **Grown clone chunks (`architecture.md`, [D8]).** The child memoizes, per
`res_id`, `src_slice_idx` and `bm_idx` — the `res_id` being the `clone_id`
here, the `migr_id` for a migration, whose `src_slice_idx` is always zero —
the etcd mod revision of the chunk it last pushed (`CloneBmIdx` and
`MigrBmIdx` carry it, MD3); a chunk whose mod revision advanced is re-pushed
even though the agent acknowledges its address. `AppendCloneBitmap` grows a
chunk in place, at the pair that already addresses it, so the memo must key on
the whole pair: judged by `bm_idx` alone it would compare one source slice's
growth against another slice's revision and drop a chunk the agent never
received. The memo is in-memory and lost on a handoff — accepted by
`architecture.md`, [D8]. Migration chunks are immutable, so nothing can make
the rule re-push one; `worker/bmpush.go` implements BM1 to BM6 once for both
kinds and therefore memoizes migration chunks too, where the mod-revision
comparison is inert.

BM6. **Failure.** A push that fails — a gRPC error or timeout, a chunk
whose value can no longer be read from etcd, or a non-zero
`agent_reply.code` (`ReplyCodeUnknownObject`: the introducing `Syncup*` has
not been applied yet, or the address is out of range for the object) — is
logged as `bitmap push failed` (Log records) and **ends that plan**. The
remaining parts are not sent, nothing is re-armed and no flag is raised
anywhere. The next `Syncup*` reply for the object re-plans the diff (BM2)
from the agent's OWN acknowledged set, which is the only place a diff has
ever come from; a plan rebuilt from a remembered failure would be a plan
built from the worker's memory instead of from the agent's state. The rest
of the plan goes with the failed part because the parts are ordered (BM3)
and a migration's chunks are interpretable only in sequence.

There is no push-specific timer and no push-driven re-sync. When the
refusal was the agent's own, its `Check*` reply says so too — the same
rejection code, or a stored revision behind the desired one — and RW4 step
5 issues the `Syncup*` that re-plans within the round. When the push failed
on the wire while the agent is healthy and at the desired revision, no
re-sync is due and the chunk waits for the next `Syncup*` from any cause (a
revision bump, a leftover, a handoff). That wait is accepted because the
skip bitmap is an optimization: a chunk that has not arrived leaves its
regions hydrated instead of discarded (`architecture.md`, Bitmap push
protocol), which costs copying, never correctness.

## Automatic reactions — `worker/reaction.go`

The automation of `architecture.md`, Automatic reactions, as the sp
coordinator's **pass**: one evaluation of the automatic reactions for one
SP. Everything here is the worker's job; agents only report. There are
four reactions — failover, thin-pool auto-grow, cntlr replacement, leg
repair — and no migration reaction: every condition that sets
`Side.err_epoch` also prevents the source DN from serving a migration, so
`side_unhealthy` and `leg_unhealthy` are instead the two triggers of one
leg-repair procedure (AR8) — the shorter wait when the DN itself looks
dead, the longer one when only the cntlr's path is bad. Beside the
reactions the pass runs the three steps of the procedure Repair of
`architecture.md` that are the worker's, a repair's stage moves (AR11), the
retire timer (AR12) and the clear of a forced-start mark (AR13), which are
no reactions.

### Pass, priority, suppression

AR1. **Cadence and inputs.** The coordinator runs one pass per SP every
`cntlr_interval` (its own ticker). A pass starts with a fresh `SnapshotRev`
(EU4) of `SpConf`, every `Cntlr` and every `Slice` of the SP — the records
the reactions read, and with the `Clone`s the ones this worker itself
writes the `err_epoch`s into — and the sub-objects `SpConf` lists (AR5's
and AR7's row classes attribute rows through its tds, subsystems,
transfers and clones; AR11 and AR12 read its repairs and retired legs),
plus the in-memory latest `CntlrInfo` of the **primary** cntlr
(pool usage, group rows, AR5's and AR7's rows, and the verdicts on repairs
and the groups' whole flags and in-sync lists (`GrpInfo`) AR8, AR11, AR12
and AR13 read)
with the revision of the observation it reports (`architecture.md`,
Live-state reporting), `now` (unix seconds),
the two records kept from an earlier pass that a pass decides by — the
coordinator's records of the last failover it applied (AR5's second
refusal) and of the last replacement of a primary it applied (AR7's second
refusal) — and a copy of AR10's memo of the coordinator's own verdicts. The
snapshot re-seeds the SP's health memos first, ahead of every gate below
(HL3); it seeds nothing into AR10's memo, because a stored epoch is no
verdict of this coordinator's. The primary is the one the snapshot names,
and its info is taken only once its child has been handed the primary plan.
While a hold keeps a promotion from the child (RW22's demotion hold, then
RW14's sides-first hold), the child's info is still a standby's report — a
leg row there is transport liveness and ANA state, not the health-block
probe — so the pass has none: AR6 grows nothing, AR8 creates no repair,
AR11 moves no stage, AR12 deletes no retired leg and AR13 clears no
forced-start mark, as when the primary has not reported yet. From the
hand-over until the reply to the promotion, the info is still that standby
report (Known limits, one-pass-old inputs), which holds no group row, a
standby having no group (`cnagent.md` CN12), and no group's whole flag or
in-sync list, so AR8's gate holds the create (`grp_row_absent`) until the
reply reports the group's array running; and once the failover has
committed, the old primary's last report, whose group rows may still read
`OK`, is never the one read.

**Both** stored confs are validated before the reactions are evaluated —
the cluster's with `model.ValidateClusterConf`, the SP's `bdev_conf` with
`model.ValidateBdevConf` (`architecture.md`, Common validation). Both,
because this pass takes its own fresh snapshot of the SP rather than
reusing the fan-out's: AR6 reads `low_water_mark_pct` and `data_block_size`
straight off it and sizes a meta grow with the cluster's `extent_size`,
while the DN scans of AR6 and AR8 walk the bin ladder and every allocating
reaction takes its oversampling batch from `alloc_conf`. A third gate
follows the `bdev_conf` one: every group of the SP, a clone meta group
included, must give a length to zero (`model.SideZeroBytes`,
`model.CloneMetaSideZeroBytes`), the check RW14's fan-out makes. No
reaction reads a length to zero, but the pass must not commit a reaction
the fan-out cannot deliver, and the fan-out builds no request for an SP
with a group that gives none: a failover, say, would take the old
primary out of the discovery records at its commit, while the new one's
promotion waits on the refused fan-out. A failure of any gate refuses the
pass with one `invalid stored conf` record (Log records, once per distinct
error) and no reaction runs: no candidate scan, no reaction `model` op, no
`reaction applied` and no `reaction skipped`. Neither drain sits behind
every gate: the clone drain's steps (CLD7) run first of all, ahead of the
cluster-conf lookup and of every validation, and a latched SP's drain
step (SPD6) runs behind the cluster-conf gate — its repair, retired-leg
and D2 batches maintain DN capacity keys off the ladder — but ahead of the
SP's own two, so a
refusal of the SP's geometry or of a group's length still lets that SP
drain. The ticker keeps running — the pass cadence falls back to
`DefaultHealthCheckInterval` on an unusable conf and logs nothing of its
own, because a coordinator that stopped ticking would stop retrying the
fan-out and the pass forever, and those two are where the refusal is
already recorded. A cluster missing from the cache stops the pass the same
way, silently — after the clone drain's steps, whose batches read no
cluster conf and whose final step reads it from the cache itself (CLD7),
and before an sp drain step, which reads it behind the pass's gate: its
children log
`cluster conf missing` for the idle period (RW9).

The steps of a live SP's pass run in this order: the re-seed of the health
memos (HL3), the clone drain's steps (CLD7), the three gates, AR11's stage
moves and AR13's forced-start clear at every `sp_level`, AR3's
suppression, AR12's retire timer, and the reactions in AR2's priority. A
latched SP runs its drain step behind the cluster-conf gate and neither the
two steps of a repair nor the forced-start clear (SPD6).

AR2. **One action per SP per pass** — "action" meaning a *reaction*
throughout this rule — evaluated in this priority; the first applicable one
runs and the pass ends:
1. primary failover (AR5)
2. thin-pool auto-grow (AR6)
3. cntlr replacement (AR7)
4. leg repair (AR8)

Every action is a `model` op that re-validates its preconditions inside its
STM (MD6, MD7); success logs `reaction applied` and the resulting `SpRev`
bump re-fans the SP (RW14), so the next pass sees the new state. Leg
repair's actions are the finish, the cancel and the try-to-abort of a
worker's repair and the create of one (AR8). The
invariant is **at most one applied action per SP per pass**; the clone
drain's steps, AR11's stage moves, AR12's retire timer and AR13's
forced-start clear are not actions in this sense and commit alongside one —
a pass over a live SP can bump `SpRev` once per deleting clone whose final
STM it commits (CLD9, CLD12), a batch bumping nothing (CLD8), once per stage
move, once per clear, once for the timer's delete — which an earlier bump
of the same pass, a stage move's, a clear's or a clone's final STM's, makes
refuse as stale (AR12) — and once more for its reaction. A
`reaction skipped` that means "not applicable here, keep looking" does not
end the pass. The pass continues
past: AR5's no-failover-candidate (AR7's sole-primary variant is defined as
"AR5 found none" and would otherwise be unreachable); AR5's `shared_state`
and `same_error`, which can hold for as long as the error does — an
operator's intervention, for a lost thin id; AR6's `grow_pending`,
`grp_list_full`, `meta_ladder_cap` and `no_data_group` when the pass's own
check finds them, ahead of any scan and any transaction (each can hold
indefinitely — a grow deferred on the CN, a ceiling of `architecture.md`,
GrowSlice — and must not disable AR7 and AR8 for the duration); AR7's
`shared_state` and `same_error`, which can hold as long as AR5's and move
the scan to the next cntlr; AR8's refusal of a drive step by its op, which
holds that repair's group and moves the walk to the next repair; and
AR8's `leg_has_two_sides`, `grp_operator_in_charge`, whether the pass's
own check or the create's op finds it, `grp_forced_start`, its gate on the
group's row (`grp_row_error`, `grp_row_missing`, `grp_row_unknown`,
`grp_row_provisioning`, `grp_row_pending`, `grp_row_absent` and
`grp_row_stale`), `no_healthy_leg` and `grp_has_repair`, which move the
scan to the next candidate leg — a group whose `operator_in_charge` flag is
set waits until the operator clears it; a group with a forced-start mark
waits until the mark goes in one of the ways AR8 names, a try-to-abort of
the group's repair not among them (MD6); a group
whose row does not read `OK` waits until it does (`cnagent.md` CN28),
for example while its array is stopped or broken, or while the in-sync
guard holds over it (`cnagent.md` CN12), and one whose row reads `OK` in a
report older than the snapshot waits for the primary's report of the
snapshot's revision; a group whose other legs of its `leg_list` are
unhealthy or not held in sync waits until one of them reads healthy and in
sync again; and a group with a repair waits until its finish or its cancel,
an operator's repair until the operator acts.
Everything else ends the pass: every `ErrPrecondition` but a drive
step's and the create's `grp_operator_in_charge`, every empty
allocator scan and every transient op failure — `GrowSlice`'s
refusal from inside its transaction among them, though it carries the
`grow_pending` or `grp_list_full` of the pass's own check (its record names
the op, Log records): that check found the grow applicable on the pass's
snapshot, so the slice gained a group after the snapshot was read — another
owner's grow (VW7) or an operator's `GrowSlice` — and the pass applies
nothing more from a snapshot the store has overtaken; the next pass reads
the slice afresh, and its own check then finds the grow pending or the list
full and goes on. Two owners overlapping on one SP (VW7) cannot apply an
action twice — the second STM fails its precondition, except a
try-to-abort's: a second create of a worker's repair finds the group's
repair and is refused with `grp_has_repair`, or with `repair exists`,
which the op checks first, when both owners built the same name in the
same second (AR8); a second finish or cancel finds the repair gone or past
its stage; and a second try-to-abort finds the flag set, which
`SetRepairAbort` answers as done with no write, so it changes nothing
(MD6).

AR3. **Suppression.** No reaction runs for an SP with `sp_level` at or
above `SP_LEVEL_NO_THINPOOL` (the disaster-recovery levels of
`architecture.md`, SpLevel, the levels where reactions stop), and neither
does AR12's retire timer; AR11's stage moves run at every level, since a
move only follows the primary's report and an operator may need to finish
a repair there, and so does AR13's forced-start clear, since a
clear only follows the primary's report too and the operator force-starts
a group at the levels where reactions stop. Leg repair's drive step is a
reaction, so the
operator drives the worker's repairs there with the gateway's calls (AR8).
An SP with `deleting` true is not suppressed
here: it runs at most one DRAIN step per
pass and no reaction at all, at ANY `sp_level`, because a doomed SP must
drain whatever level an operator left it at — "at most", because the AR1
gates that sit ahead of the branch still apply: a pass whose `LoadSp`
failed, or whose cluster conf is missing or unusable, runs no step at all
(the SP's own `bdev_conf` and group-length gates sit *behind* it,
deliberately — SPD6). A
**disabled** cntlr is never a candidate, replaced or repaired — but a
disabled *primary* is itself the AR5 failover trigger (`architecture.md`,
Cntlrs). Disabling is the operator's hands-off signal, and the "skipping
the enabled check" of `architecture.md`, Automatic reactions, is read as
skipping the public `disabled` precondition of `DeleteCntlr`, not as
replacing disabled cntlrs.

AR4. **Thresholds.** `now` minus `err_epoch` at least the threshold, which
is the SP's `event_threshold` field of that kind, a zero meaning its
default (`DefaultPrimaryUnhealthy`, `DefaultCntlrUnhealthy`,
`DefaultSideUnhealthy`, `DefaultLegUnhealthy`; `architecture.md`, Common
validation). That `leg_unhealthy` exceeds `side_unhealthy`, and that
`primary_unhealthy` covers at least two of its cluster's `cntlr_interval`,
are gateway validation rules (`architecture.md`, Common validation); the
worker is correct either way. The second alone does not keep one missed
round from failing a primary over, so AR5's threshold trigger also needs
AR10's run; what that run does not cover is in Known limits.

AR10. **Own verdict.** A threshold reaction — AR5's failover on its
threshold, AR7's replacement, AR8's create on either of its clocks and
AR8's try-to-abort on a clock —
fires only for an object whose latest health verdict by this coordinator is
unhealthy; the clock still runs from the stored `err_epoch` (AR4), and
AR5's `disabled` trigger needs no verdict. AR8's case 1 needs the leg's
verdict, and its case 2 the leg's and that of the side whose clock fires;
AR8's try-to-abort on a new leg's clock needs the verdict on that leg or on
the side whose clock fires, and on another leg its triggers' verdicts.
AR5's threshold trigger needs more than the latest verdict: the primary's
last two verdicts from check rounds — a round with no reply in time or
whose observation is too old (HL7), which counts on every such round, or a
reply with an `ERROR` row that answers from an observation new to the
loop of the cntlr's child (RW23) and that a round asked for (HL7) — must
both be unhealthy,
with no clean answer, of a round or of a `Syncup*`, since the earlier of
them; a `Syncup*` reply with an `ERROR` row neither starts that run nor
adds to it, and neither does a round that answers from an observation
already seen and not too old (RW23), nor one that answers from an
observation no round asked for and not too old, which gives no verdict at
all (HL7). A
round counts by its
own report: the info its reply carried or, when the reply leaves it out as
unchanged, the info its stream last carried, never a `Syncup*`'s info that
the latest known state holds since (HL5). The coordinator keeps a memo of
the verdicts its own monitors reach on its cntlrs, legs and sides (HL2), the
new legs of its repairs, their sides and its retired sides included, which
the memo keeps while the pass's snapshot lists them (`architecture.md`,
Repair): an
unhealthy verdict puts the object in, a clean one takes it out, and a reply
that neither sets nor clears changes nothing; for a cntlr it also counts
the run of unhealthy verdicts from rounds, which a clean answer ends and
which outlives the cntlr's child (RW23 says what a restarted child adds
to it). A
verdict counts whether or not its monitor writes, so one that
finds the record set already — re-seeded from another observer's epoch,
say (HL3) — counts though it is no transition. The memo lives with the
coordinator from its start to its stop, which is the coordinator's tenure
of the SP: a restart or a handoff begins with an empty one, and the
snapshot seeds nothing into it (AR1). AR8's try-to-abort on a bad leg that
reads healthy again reads its epoch as stored, needing none, and so do
AR8's healthy-leg test and AR12's test of a group's legs, none of which is
a threshold. A
reaction this rule holds back logs nothing, as one whose threshold has not run
out logs nothing, and no
`model` op can re-check the rule, since no STM sees the memo. The reasons:
an epoch an earlier owner left, or another observer wrote (VW7, HL3), fires
nothing before this coordinator has judged the object itself, and one rule
for every threshold reaction is simpler than an exception for one. The run
is the failover's alone because its threshold is the one counted in check
rounds: epochs are whole seconds and the next verdict can come late, so one
missed round could otherwise reach it. A `Syncup*` reply is left out of
the run because the converge of a grow or of a promotion that waits for a
member reports its groups `ERROR` though the next primary would wait the
same. A round that answers from a retry attempt of such a converge is not
left out: the cn agent's connect retry re-runs the converge while the
member stays late (`cnagent.md` CN10, CN12), each attempt's observation
carries the same rows, and a round that answers from one new to the
child's loop counts them (RW23), so two such rounds in a row fill the run with no round
missed (Known limits). The cost: after a restart or a handoff a reaction
waits for this coordinator's first verdict on its object. A missed round
gives one at once; otherwise it comes from a `Syncup*` reply that the
first answered round brings, or from the first round that answers from an
observation made after that round, normally the second (HL7). A failover
on its threshold waits for the second counted round (RW23), and a repair
of a leg whose primary's rounds give it no verdict waits for that
primary's next converge (Known limits).

### Failover

AR5. When the primary cntlr is `disabled`, or has `err_epoch` set, `now`
minus it at least `primary_unhealthy` — `cntlr_unhealthy`, when that is the
longer, while it is **settling** (HL2) — and this coordinator's own
verdicts on it unhealthy for two of its counted rounds in a row (AR10): the
candidate is the cntlr with the
smallest `cntlr_id` among those with `primary` false, `disabled` false and
`err_epoch` zero; none means `reaction skipped` (`no candidate`) and the
pass continues (AR2); else `model.Failover` of the old primary and the
candidate — for an enabled primary, unless one of the two refusals below
holds it (the first is judged ahead of the candidate, and where there is
none it logs in place of `no candidate`). This is the failover trigger of
`architecture.md`, Failover; the data-plane choreography is the agents'.
The `disabled` trigger waits out no threshold and needs no verdict:
disabling is explicit operator intent, and the disabled primary has
normally stopped serving by then (`architecture.md`, Cntlrs) — its disable
request reaches it one sides-first hold after the bump (RW14). A pass runs
inside that hold whenever a side the hold waits for does not report and
the hold lasts a whole `cntlr_interval` — a side on a disk node that died
too recently for its epoch to be in the fan-out's load, or during the hold
(RW14) — and fails the primary over first: the failover's demotion hold
ends that sides-first hold, so the held disable request is never sent, and
the primary is handed its demotion ahead of the sides, which wait for its
report or for `DemotionHoldTimeout` (RW22). The
candidate-skip bookkeeping is unchanged by that trigger, so it is
deliberately **not** edge-triggered: a `disabled` primary with no eligible
candidate re-emits `reaction skipped` / `no candidate` once per pass, for
as long as it takes an operator to enable a standby or add a cntlr.

A settling primary — one that has not yet reported its stack built and clean
as primary since it acquired the role (HL2) — is held to `cntlr_unhealthy`,
the threshold the worker already gives a cntlr before giving up on it. The
hold lasts until that report: the promotion's, or, while a new SP's sides are
still being zeroed, the end of its first build (HL2's settle paragraph); a
primary that never makes it is failed over at the same threshold AR7 would
replace the cntlr at, unless one of the two refusals below holds it. The hold
applies only where `cntlr_unhealthy` is the longer, as it is at the defaults:
nothing orders the two thresholds (AR4), and an SP stored with a shorter
`cntlr_unhealthy` would otherwise have its settling primaries failed over
sooner than its settled ones. Without the hold, a promotion that raced the
sides' ANA flips would read `ERROR` at its first reply and be failed back
`primary_unhealthy` later, to the peer its own promotion had just made clean,
on every pass. `model.Failover` re-validates the same selection inside its STM
(MD6, MD7). The settling hold leaves the `disabled` trigger and the candidate
rule as they are — `failoverEligible` never reads the flag. A settling primary
with no candidate reaches the no-candidate skip only at the longer of the two
thresholds, and AR7's sole-primary variant, which waits `cntlr_unhealthy`
either way, replaces it unless the first refusal below holds it or AR7's twin
of the second does (AR7); the replacement is created settling in turn.

A primary unhealthy past its threshold is not failed over where a failover
cannot help, or is presumed not to (the second refusal compares rows, not
causes) — its `err_epoch` stays set, and every pass logs why and goes on
(AR2), as AR8 does for what only an operator can repair; a primary with no
candidate is replaced by AR7's sole-primary variant at `cntlr_unhealthy`,
except over a report the first refusal holds or, for the replacement AR7
last made, over only the rows of the primary it replaced (AR7, Known
limits):

* **Shared state.** Every `ERROR` row of its latest report that HL2 judges
  it by (every map of rows but `leg_id_to_leg` and `side_id_to_path`) is of
  HL2's shared-state class:
  `reaction skipped` (`shared_state`, with the primary's `cntlr_id` and the
  smallest such `td_id`), judged ahead of the candidate, so a primary with
  none logs this rather than `no candidate`; nor does AR7 replace such a
  primary once it is due for replacement (AR7). The next primary would read
  the same rows from the same pool: failing over would move the host paths
  and nothing else, and the promoted cntlr, never settling, would lose the
  role again at `cntlr_unhealthy` — to the peer whose standby report had
  cleared its `err_epoch` — once per window, for as long as the td stayed
  lost.
* **The same error.** The failover this coordinator last applied took the
  role from the candidate, the primary's `err_epoch` was set less than
  `cntlr_unhealthy` after that failover, and every `ERROR` row of the
  primary's latest report that HL2 judges it by — the same map and key, and
  the same td for a thin row — is one the candidate's report read `ERROR`
  on when it lost the role: `reaction skipped` (`same_error`, with
  `old_cntlr_id` and `new_cntlr_id`). The rule presumes the error followed
  the role rather than being the primary's own — it compares rows, not
  causes (Known limits) — and handing the role back would move it again
  over the same error, once per `cntlr_unhealthy` while the primary never
  settles and sooner once it has. A report with any other `ERROR` row is
  taken for a fault of the primary's own, and the role moves as for any;
  that failover records the report's rows in turn. So an error the classes
  do not name — a lost thin id a background read reports, whose probe
  carries no reason, among them (HL2) — does not send the role back to the
  cntlr it last left while the new primary fails no row the old one did
  not: in an SP of two cntlrs it moves the role once, and in a larger one
  it can first move it on to a cntlr that has not held it. An `err_epoch` set later is a new
  error, judged as any, and a report with no `ERROR` row — a primary read
  unreachable, whose rows the worker marks `UNKNOWN` — is refused nothing.

Neither refusal holds a `disabled` primary: that trigger is the operator's.
Both read a report, which no STM sees, so `model.Failover` does not
re-validate them, nor AR10's verdict, which no STM sees either. The second
rests on the coordinator's record of the last failover it applied, one of
the two records of its own a pass decides by (AR7's second refusal rests on
the other): why the role moved is kept in no etcd record. A coordinator
without it — restarted, handed the shard, or the other owner of an overlap
(VW7) — can fail over once more once its own verdicts read the primary
unhealthy for two of its counted rounds in a row (AR10), which records it
again.

### Thin-pool auto-grow

AR6. Per slice, from the primary's `slice_id_to_dm_pool` row of the slice,
which MUST be `RES_STATUS_OK` (an `ERROR`, `PROVISIONING` or absent pool is
never grown). Its `details` is the raw "dmsetup status" line of the thin
pool; after the "thin-pool" token come the transaction id, the used and
total metadata blocks and the used and total data blocks — metadata counts
in dm-thin's fixed metadata block size (`ThinMetaBlockSize`), data counts
in the pool's `data_block_size`. An unparsable line is skipped and logged
once per change. With the mark `dm_pool_conf.low_water_mark_pct` **as
stored** (a mark above a hundred percent switches auto-grow off, the one
value that is a meaning rather than a default; a zero never reaches here —
`CreateStoragePool` resolved it at write time and AR1's gate refuses one
that did, `architecture.md`, Common validation):

* a **data grow** when the used data blocks exceed the mark's percentage of
  the total data blocks: an internal `GrowSlice` of the data kind with
  `ext_cnt` the slice's first data group's (grow by the original allocation
  unit);
* a **meta grow** when the used metadata blocks exceed the mark's percentage
  of the total metadata blocks: an internal `GrowSlice` of the meta kind,
  ladder-sized (`architecture.md`, GrowSlice); data is checked first when both
  breach.

**Pending rule (stateless).** A grow of a kind is *pending* while the reported
total is no larger than the total implied by the slice's groups of that kind
**excluding the newest one**: for data, the total data is at most the sum of
`data_blocks` over all data groups but the last; for metadata, the total
metadata is at most the sum over all meta groups but the last of `data_blocks`
times the block size divided by `ThinMetaBlockSize`. While pending, no grow of
that kind starts — this is the "one grow per pool at a time" of
`architecture.md`, Automatic reactions, reconstructed from etcd and the status
line on every pass as a memo built from facts, so a worker restart or a
handoff cannot issue a second grow; a grow deferred on the CN
(`architecture.md`, [D15]) stays pending the same way, because its totals have
not moved. Candidates: `FindDnCandidatesAntiAffine` with the group's
`ext_cnt`, a candidate count of the leg count times `dn_batch_size`, the leg
count as the required count, an empty black list and no excluded locations
(`architecture.md`, Per-operation allocation, leaves the grow on the plain
scan, so tier 2 never runs), where the leg count is the SP's group shape by
its redundancy kind (`legCnt`: one for `RedundNone`, `MaxAllocLegPerGrp` for
`RedundMdRaid1`), picked randomly with the growing black list so the legs land
on distinct DNs; fewer candidates than legs, or a cntlr's CN below the budget
(checked in the op), means `reaction skipped`. A slice whose list of that kind
already holds `MaxGrpCntPerSlice` groups (`architecture.md`, GrowSlice) can
never grow that kind again: a breach of that kind that is not pending logs
`reaction skipped` (`grp_list_full`) without a scan and the pass goes on
(AR2); `GrowSlice`'s in-STM refusal carries the same reason, both through
`model.GrpListFull`, and ends the pass, as its in-STM `grow_pending` does
(AR2).

### Cntlr replacement

AR7. When a cntlr has `err_epoch` set, `now` minus it at least
`cntlr_unhealthy`, this coordinator's own latest verdict on it unhealthy
(AR10), `disabled` false, and either `primary` false or it is the primary and
**no failover candidate exists** (AR5 found none — the sole-cntlr SP, or every
other cntlr unhealthy or disabled): the candidates are
`FindCnCandidatesAntiAffine` with the SP footprint (the sum of `ext_cnt` over
all groups of its slices), a candidate count of `cn_batch_size`, a black list
of the old cntlr's `addr_port`, the SP's other cntlrs' endpoints as the SP's
CN endpoints and those CNs' locations excluded — the locations out of the
pass's own snapshot (MD3; a CN missing from it contributes none), the old
cntlr adding none of its own because it is the one leaving, so its domain is
excluded only when a survivor shares it (unlike AR8's create, whose new leg
also avoids the failing leg's domain: excluding the old cntlr's domain here would
empty tier 1 in a two-domain cluster the SP's cntlrs already span, and the
tier-2 rescan could then put the replacement in a survivor's domain), and the
tier 2 of `architecture.md`, Per-operation allocation, rescanning without them
when tier 1 finds no CN; pick one; an internal `ReplaceCntlr` of the old
cntlr, the pick, those endpoints and the old cntlr's `primary` as the new
one's — the same `cntlid_slot`, `primary` true and `settling` true only in the
sole-primary variant, `disabled` false, `err_epoch` zero (HL2: a primary
replacement is created settling, as a promoted primary is, and AR5 judges it
by the settling threshold until it reports its stack built and clean as
primary). The old CN is black-listed even when the node itself is healthy: its
cntlr is what failed. None means `reaction skipped`. The op is handed the
endpoints the scan was, and its STM refuses the pick as `candidate changed`
(MD5, MD6) when the SP holds a cntlr on a CN outside it — one the gateway's
`CreateCntlr` committed after the pass's snapshot, whose CN and domain the
scan could not exclude: `reaction skipped` (`candidate changed`), and the next
pass plans from a snapshot that holds it.

The new CN takes no clone's metadata: a primary on it builds every clone
of the SP over the clone's clone meta group (`cnagent.md` CN18), so the
scan and the op count the footprint alone (MD6).

The sole-primary variant takes two refusals, the twins of AR5's. Both are
judged once AR7's trigger holds, whether or not AR5's threshold has run
out, on the report AR5 judges, the info the pass holds for the primary
(AR1, HL5), and each skip moves the scan to the next cntlr, so a standby
due for replacement is still replaced, and with none the pass goes on
(AR2). A standby's report is not read here, and carries no thin row
(`cnagent.md` CN14), so its `ERROR` rows are its own.

* **Shared state.** A primary whose latest report has `ERROR` rows of HL2's
  shared-state class alone is not replaced — `reaction skipped`
  (`shared_state`, with its `old_cntlr_id` and the smallest such `td_id`).
  The replacement would read the same rows from the same pool. The report
  carries the lost-id reason only while it comes from a converge's
  observation (HL2). A `Check*` reply carries an info when the info of the
  observation it answers from differs from the last one its stream sent,
  and always on a fresh stream's first reply (HL5; `dnagent.md` SH26), and
  a read that a later round starts replaces the converge's observation with
  one whose rows carry no reason (`dnagent.md` SH29), so a converge's
  report stays the one held only until a round answers from a read's
  observation, or a dead stream marks its rows `UNKNOWN`. A primary whose
  held report is a read's, whose rows a dead stream has marked `UNKNOWN`
  (HL1), or whose child holds no report yet is not held by this refusal
  (Known limits).
* **The same error.** When the pass replaces a primary, the coordinator
  records the replacement the op created, the pass's `now` and the `ERROR`
  rows of the replaced primary's latest report, each named and compared as
  AR5's second refusal names and compares a row (map, key, and td for a
  thin row). The primary is not replaced while it is that replacement, its
  `err_epoch` was set less than `cntlr_unhealthy` after that replacement,
  and every `ERROR` row of its latest report is one of those rows:
  `reaction skipped` (`same_error`, with its `old_cntlr_id`; the kind,
  `replace_cntlr`, tells the record from AR5's). The rule presumes the
  error followed the replacement, as AR5's presumes it followed the role: a
  read's report of a lost thin id carries no reason, so the first
  refusal lets the primary be replaced on it, and the replacement, which
  reads the same rows from the same pool, would otherwise be replaced in
  turn once
  per `cntlr_unhealthy` for as long as the td stayed lost — in an SP of one
  cntlr, each time moving its whole stack to another CN. It compares rows,
  not causes: a replacement whose own fault fails only rows the old primary
  failed on is held as well. A replacement whose report has any other
  `ERROR` row, or none — one read unreachable has its rows `UNKNOWN` — or
  whose `err_epoch` was set later, is judged as any primary, and replacing
  it records its rows in turn. Replacing a primary over a report with no
  `ERROR` row records none, a record that holds nothing, so the cntlr it
  puts in is judged as any primary as well. Only a primary's replacement is
  recorded, and only the primary is judged: a standby's replacement leaves
  the record as it is, and a replacement that has lost the role since is
  judged as any standby. The record is the coordinator's memory, as AR5's
  is: a coordinator restarted, handed the shard, or the other owner of an
  overlap (VW7) has none, and can replace the replacement once more once its
  own verdict reads it unhealthy (AR10), which records it again — with no
  row when the pass holds no report of it yet, as when that verdict is a
  missed round of a child that has never had a reply, so that the one after
  it is not held either.

Like AR5's refusals these read a report, which no STM sees, so
`model.ReplaceCntlr` does not re-validate them, nor AR10's verdict.

### Leg repair

AR8. **Triggers.** A leg in the `leg_list` of a slice's group, or of the
clone meta group of a clone that is not `deleting` (`architecture.md`,
Terminology and object model), needs repair when either

* **Case 1** — `Leg.err_epoch` is set, `now` minus it is at least
  `leg_unhealthy`, and this coordinator's own latest verdict on the leg is
  unhealthy (AR10): unhealthy from the cntlr's perspective for the long
  threshold (the primary reports it has no healthy path — possibly a CN to
  DN connectivity problem, hence the longer wait); or
* **Case 2** — `Leg.err_epoch` is set (any duration) **and** its side has
  `Side.err_epoch` set with `now` minus it at least `side_unhealthy`, this
  coordinator's own latest verdicts on the leg and on that side both
  unhealthy (AR10): the side reports an error or the worker cannot talk to
  its DN, so the DN itself is probably dead, hence the shorter wait. A side
  the worker cannot reach while the primary still sees the leg healthy
  triggers nothing.

A repair's new leg is no leg of its group before its finish, and a retired
leg is no leg at all, so neither is a candidate. Several legs that need
repair: the smallest `leg_id` first, over the slices' groups and the clone
meta groups of the SP together.

**The worker's repair.** Leg repair replaces such a leg by a worker's
repair (`architecture.md`, Repair): a repair whose name carries the reserved
prefix, built from the pool id, the group id and the pass's `now`
(`common.WorkerRepName`). The worker creates it with a fresh new leg
(`CreateRepair`), and its drive step finishes it, cancels it or tries to
abort it (`FinishRepair`, `CancelRepair`, `SetRepairAbort`), each op with
no revision token (MD6); the stage moves between are AR11's, as for every
repair, and the worker drives no operator's repair (AR9). Each
pass's leg repair runs the drive step first and the create after it. A
finish, a cancel, a try-to-abort and a create are each the pass's one
action (AR2), and none runs at a level that suppresses reactions (AR3);
there the operator drives the worker's repairs with the gateway's calls,
which take a name with the reserved prefix (`architecture.md`, Repair). The
group's `operator_in_charge` flag holds the create alone (The create,
below): the drive step reads no `operator_in_charge` flag, so while an
operator takes the group over the worker still finishes, cancels and tries
to abort its own repair of it (`architecture.md`, Repairs).

**The drive step.** The pass walks the repairs whose name carries the
reserved prefix, in `repair_name_list` order. It passes over the repair of
a deleting clone's group, or of a clone the snapshot does not hold, which
the clone drain's final transaction gives back (CLD9), and a repair whose
stage this pass moved (AR11), whose record in the snapshot still holds the
stage it left. For each other repair:

* **STARTED**: the pass finishes it (`FinishRepair`, at `now`, with the
  cluster's stored conf), whatever its abort flag says. The finish needs no
  gate on the group's row, since the primary reports *started* only from a
  running array (`cnagent.md` CN28), and it loses nothing, since the bad
  leg's data is kept as a retired leg. When the group holds
  `MaxRetiredLegPerGrp` retired legs, the finish gives back the oldest
  (`architecture.md`, Repairs), and its record names the leg it gave back.
* **ABORTED**: the pass cancels it (`CancelRepair`, with the cluster's
  stored conf), which gives its fresh new leg's side back to its DN.
* **CREATED, its abort flag clear**: the pass tries to abort it
  (`SetRepairAbort`) on the first of three triggers, in this order: its bad
  leg reads healthy again, its `err_epoch` clear, with no clock and no
  verdict; its new leg's `err_epoch` is at least `leg_unhealthy` old, or the
  `err_epoch` of the new leg's side is at least `side_unhealthy` old, each
  with this coordinator's own unhealthy verdict on that leg or that side
  (AR10); or another leg of the group's `leg_list` needs repair by the
  triggers above. The record names the trigger and its leg. A CREATED
  repair whose flag is set waits for the primary's verdict and AR11's stage
  move.

The first step that commits ends the pass (AR2). A refusal of the op
(`ErrPrecondition`, MD7) holds that repair's group only: it is logged and
the walk goes on to the next repair, and then to the create; any other
failure of the op ends the pass, since its transaction may have committed.
A repair that reaches STARTED with its flag set is finished like any other
(`architecture.md`, Repair), and a new repair may follow, through the
create, for a leg that goes bad later.

Reasons: a bad leg that reads healthy again needs no repair, and while md
holds it in its place, serving with an `OK` leg row, the primary never
fails it, so the repair could only wait (`architecture.md`, Known limits).
The clocks keep
the abort off a fresh new leg: it reads `ERROR` from its provisioned flip
until the primary's connect to it completes, its `err_epoch` is set at
once, and the abort flag is never cleared, so an abort read off that window
would turn every worker's repair into a loop of create, abort and cancel.
These are the clocks the triggers use. The side clock needs no `err_epoch`
on the new leg, unlike case 2, because an unprovisioned new leg is neither
connected nor probed, so only its side's health shows a DN that dies while
the side zeroes; the primary confirms the abort once the array runs, since
md holds nothing of that leg, and the cancel gives the side back. Another leg
that needs repair aborts the repair because md copies onto the new leg
from the group's other legs; that leg gets a repair of its own only once
another leg is healthy and in sync again (A healthy leg, below), so each
repair names one bad leg and a second bad leg waits for a repair of its
own.

**The create.** For each leg that needs repair, the smallest `leg_id`
first, the conditions run in this order, a failing one logged as `reaction
skipped` with its reason:

1. the pool is `RedundMdRaid1`: `redund_none` otherwise, which ends the
   pass, since redundancy is pool-wide and no later candidate differs; a
   `RedundNone` group has no redundancy to re-home, and an operator moves
   its data;
2. the leg has exactly one side: `leg_has_two_sides` otherwise, since a leg
   with two sides has a user migration in flight and `CreateRepair` refuses
   such a bad leg;
3. for a clone meta group, the clone is not being deleted: such a clone's
   legs are no candidates at all, so nothing is logged, since its group
   goes back to its DNs in CLD9's transaction and `CreateRepair` refuses
   such a clone;
4. the group's `operator_in_charge` flag is clear (`architecture.md`,
   Repairs): `grp_operator_in_charge` otherwise, since an operator has
   taken the group's repair over from the worker. The hold lasts until the
   operator clears the flag, and comes before the forced-start hold, so
   that the record names the flag when both stand: the flag is the
   operator's own and lasts longer than a mark. `CreateRepair` checks the
   flag again (MD6), so a create from a snapshot older than the flag is
   refused with the same reason, logged with the op's name, and that
   refusal holds the group only, as the hold does, the one refusal of the
   create that does not end the pass;
5. the group carries no forced-start mark (`architecture.md`, Repair):
   `grp_forced_start` otherwise, naming the leg the mark names, since the
   operator is acting on the group. The hold lasts until the mark goes:
   the worker clears it from the primary's report (AR13), the operator
   withdraws it, or a finish of the group's repair withdraws a mark that
   names the bad leg it retires (MD6), the worker's own drive step's among
   them, since the drive step reads no mark. The hold comes before the gate,
   which already holds the group while its array does not run, so that the
   record names the operator's hold whatever the row reads. The mark of
   the pass's snapshot holds the create in the pass that clears it, and
   the next pass reads the group without it. The hold is the
   pass's alone, as the gate is, since no STM of the create reads the
   mark: a create from a snapshot older than the mark, or an operator's
   `CreateRepair`, is not held, and the primary then counts that repair's
   new leg among the members its forced start keeps (`cnagent.md` CN12);
6. the gate on the group's row (below), the `grp_row_` reasons otherwise;
7. the healthy-leg test (below), `no_healthy_leg` otherwise;
8. the group has no repair, an operator's or the worker's own:
   `grp_has_repair` otherwise, naming the repair, since a group has one
   repair at a time and its repair keeps the worker from creating another
   for the group until its finish or its cancel; the drive step moves the
   worker's own on, while an operator's waits for the operator (Known
   limits);
9. the pool is not being deleted: a latched pool runs its drain and no
   reaction (SPD6), so this one needs no test of the pass's own.

Conditions 2 to 8 are each one leg's or one group's, read in the pass's
snapshot before any scan, and each holds that group only: the scan goes
on to the next candidate leg (AR2). Each can hold for a long time — a user
migration for hours, the `operator_in_charge` flag until the operator
clears it, a forced-start mark until the mark goes in one of
the three ways condition 5 names, an array that does not run until
its legs come back or an operator acts, a group with no healthy leg until
one answers again, an operator's repair until the operator acts — and
ending the pass on one would leave every other group of the SP degraded on
one md member as long, where a further failure loses data. The drive step
reads no mark: the worker's own repair of a group with a mark moves on as
any other does, save that `SetRepairAbort` refuses its try-to-abort while
the mark names its new leg (MD6), a refusal that holds that group only, the
next pass trying again while the trigger holds.

The create then runs `FindDnCandidatesAntiAffine` with the group's
`ext_cnt`, a candidate count of `dn_batch_size`, a required count of one
and a black list of the DNs of the group's legs; tier 1 also excludes their
locations, taken from the pass's own `DnByAddr`, a DN missing from it
contributing none, and a tier-2 rescan without the location exclusion runs
when tier 1 offers none of the **one** DN this step places, never when it
merely falls short of the candidate count (`architecture.md`, Per-operation
allocation), so a cluster with one failure domain still repairs. It picks
one and hands `CreateRepair` the name, the group's parent, the bad leg, the
pick and the black list as the plan the pick was scanned against; the op
refuses the pick as `candidate changed` when the group holds a DN outside
the plan — a migration destination that joined after the pass's snapshot —
or when the pick has gone stale since the scan (MD6), and the next pass
plans from a snapshot that holds what changed, as AR7's does. An empty scan
logs `no candidate`. Every refusal of the op but `grp_operator_in_charge`
(condition 4), and every empty scan or failed one, ends the pass (AR2).
The new leg is always fresh, and the
create proceeds while the group holds `MaxRetiredLegPerGrp` retired legs,
since the worker cannot wait for an operator: the finish then gives back
the oldest. The new leg's side provisions (`architecture.md`, Side
provisioning protocol), RW18 flips it in the repair, the cntlrs connect to
it, and the primary adds it to the array under the conditions of
`cnagent.md` CN12.

**The gate on the group's row.** The create runs only while the group's row
(`grp_id_to_md_raid`) in the current primary's latest report reads
`RES_STATUS_OK`, in a report of an observation made at the snapshot's
`SpRev` revision or later (`architecture.md`, Live-state reporting). An
`OK` row means that the group's array runs, whole or degraded (`cnagent.md`
CN28), and a running array can read `ERROR` too (`cnagent.md` CN12, CN28).
On any status but `OK` the reason names the row's status — `grp_row_error`,
`grp_row_missing`, `grp_row_unknown`, `grp_row_provisioning` or
`grp_row_pending`, and `grp_row_absent` for a report without the row — and
an `OK` row in an older report holds with `grp_row_stale`. Reasons: a repair
made over an array that does not run would sit CREATED, since the primary
adds a new leg only to a running array, and would race its own try-to-abort
when the bad leg returns; an `ERROR` row does not say whether the array
runs, and no other status says that it does, so every status but `OK`
holds. The report is the current primary's: the pass reads a cntlr's report
only while that cntlr's child drives the primary plan, and from a
failover's hand-over until the reply to the promotion that report is the
standby's last, which holds no group row (AR1), so the old primary's last
`OK` row never opens the gate. The revision test: every create, finish and
cancel bumps `SpRev` (AR2), and a report of an older observation describes
the array from before that change, so its row and its in-sync list may
place legs where the group has changed since — a finish puts the new leg in
the bad leg's place. The agent can make such a report after the bump: while
the bump's syncup waits for the cntlr's lock, a converge of the old plan
can hold it, as the late-member retry does (`cnagent.md` CN10, CN12). No
record keeps the revision of a group's own last change, so after any bump
every group waits for the primary's report of an observation made at that
revision, which the syncup of the bump's fan-out brings (RW14). The gate is
the pass's alone, since no STM sees a report: `CreateRepair` does not
re-check it, and an operator's `CreateRepair` is not held by it.

**A healthy leg.** Leg repair creates a repair for a group, a slice's or a
clone meta group, only while another leg of its `leg_list` has no
`err_epoch` and the current primary's latest report holds it in sync
(`grp_id_to_grp_info`'s `in_sync_leg_id_list`; `architecture.md`, Repair) —
the healthy-leg test. A report that says nothing of the group's array —
none at all, a standby's last report, read from a promotion's hand-over
until its reply (AR1), or one of a group whose array does not run — holds
no leg in sync, so the group waits for one that does. Reason: a leg with an
`err_epoch` may hold the only copy of what was written on the group, and
once every leg of its `leg_list` carries one, a repair would start
replacing the last legs that may still hold it with a fresh leg holding
none of it, and its copy would read a dead member, which breaks the array.
The gate does not cover the case: md learns that a member is dead only from
IO to it, so an idle array whose legs have all died still runs and its row
reads `OK`. The legs' `err_epoch`s come from the primary's probe of each
leg, which runs whether or not the array has IO (`cnagent.md` CN11; HL2),
so this test sees what the row does not; and a primary whose own network
fails, which reads every leg unhealthy at once, leaves its groups as they
are. The in-sync half is there because a leg with no `err_epoch` can still
be half copied — a repair's new leg that a finish put in place while md
copies onto it (`architecture.md`, Repair), or a leg md has failed out of
the array whose probe passes again — and repairing the only leg md holds in
sync loses the group at the next failover. The `err_epoch` half stays
because md still holds a dead member in sync on an idle array, where only
the leg's probe sees the death. The rule is the pass's alone: `CreateRepair`
does not re-check it, and an operator's create is not held by it.

**A clone meta group.** Leg repair runs on it as on a slice's group, through
the same ops, which find the group through its parent (MD6). There the
healthy-leg test also keeps the clone's progress: on the zeroed first data
block of fresh new legs for every leg, the clone's dm-clone would format
new metadata and copy the source again over what the hosts wrote
(`cnagent.md` CN18). The cntlrs connect and probe a clone meta group's legs
and its repair's new leg, and the primary reports their rows, as for a
slice's group (`cnagent.md` CN10, CN11), so the triggers and the drive step
read them as they read a slice's.

AR9. **The worker never**: deletes a td, subsystem, transfer or migration,
or a clone that is not `deleting`; touches a `RedundNone` leg; acts on a
suppressed SP; starts a migration; creates, finishes, cancels or tries to
abort an operator's repair (`architecture.md`, Repair); or runs two
reactions in one pass. The repairs it does create, finish, cancel and try
to abort are its own (AR8).
Every automatic action re-homes redundancy or roles (`architecture.md`,
Automatic reactions). It does delete the implicit children — cntlrs,
repairs, retired legs, slices, groups, legs, sides — of an SP a
`DeleteStoragePool` has LATCHED, which is the sp drain and not a reaction
(the five-empty-lists precondition that let it be latched means it holds
no td, subsystem, clone, transfer or migration), and it does delete a
`Clone` a
`DeleteClone` has LATCHED — its chunk keys in batches, then the record
together with its `clone_name_list` entry, its clone meta group and that
group's repair and retired legs, all their sides going back to their DNs —
which is the clone drain and not a reaction (CLD8, CLD9). Both drains
finish a deletion a user began with the latch and neither ever starts one;
the list above is what no reaction touches. Beside the drains, the retire
timer deletes a retired leg that has aged on a whole group: a retired leg
is no leg, the timer is no reaction, and the data of the leg it deletes is
held twice elsewhere (AR12). The finish of the worker's own repair at the
retired-leg bound gives back the group's oldest retired leg in its own
transaction, a copy older than the newest one the group keeps beside the
leg md holds in sync (AR8; `architecture.md`, Repairs), and its cancel gives
back only its fresh new leg's side, which the array does not use, since the
primary confirms an abort only once md holds nothing on that leg
(`architecture.md`, Repair).

### Repairs

The worker's part of `architecture.md`, Repair, beside the fan-out's (RW14
to RW18), the health bookkeeping's (HL2, HL3) and leg repair's, which
creates, finishes, cancels and tries to abort the worker's own repairs
(AR8): two steps of the pass that follow every repair, an operator's and
the worker's own, and a third that ends an operator's forced start of a
group, the clear of its mark (AR13). None is a reaction, so none is the
pass's one action (AR2); the worker creates, finishes, cancels and tries to
abort no operator's repair (AR9), and writes no forced-start mark.

AR11. **Stage moves.** For a live SP, after the pass's gates and before
AR3's suppression — so at every `sp_level` — the pass walks the SP's
repairs in `repair_name_list` order and moves each CREATED one whose
`rep_id` keys a verdict in the current primary's latest report
(`rep_id_to_repair_info`, the report AR1 takes): to STARTED through
`model.SetRepairStage` when the verdict says started, whatever the
repair's abort flag says, and otherwise to ABORTED when it says abort
confirmed and the snapshot's record carries the flag. The op is handed the
`rep_id` the verdict was keyed by and checks the stage, that `rep_id` and
the flag again in its STM (MD6). A verdict under another `rep_id` — one of
a repair finished or cancelled and created again under its name — matches
no repair and moves nothing, and neither does a verdict that says neither,
a repair past CREATED, or a primary that has reported none. A verdict on
a CREATED repair that says neither and carries the primary's refusal of a
take-back (`architecture.md`, Repair; `cnagent.md` CN12) moves nothing
too, and logs `repair stage skipped` with the reason `take_back_refused`
and the refusal's text, on every pass while it stands; every other verdict
that says neither logs nothing, and so does a verdict on a repair past
CREATED, even one that carries a refusal. Reason: the primary refuses to
add a new leg taken from a retired leg whose md events count is above that
of the member that keeps the data (what that count proves is
`architecture.md`, Known limits), the repair then waits in CREATED with no
row in error, and the record tells the operator why, so that they abort and
cancel it. The repair of
a deleting clone's group, or of a clone the snapshot does not hold, is
skipped (`clone_deleting`): the clone drain's final transaction gives it
back (CLD9), and the fan-out leaves it out of every cntlr's request
(RW16). A verdict of abort confirmed on a repair whose stored flag is
clear is skipped (`abort_flag_clear`), as the op would refuse it. Each
move logs `repair stage moved`, naming the repair, the stage, the primary
and the revision of its report; each skip, and each op
that refuses or fails, logs `repair stage skipped` with its reason, the
op's own for a refusal (MD7), and the next pass judges again. A move's
`SpRev` bump re-fans the SP, so every cntlr is handed the new stage (RW14,
RW16), and AR8's drive step leaves a repair this pass moved to the next
pass, which reads its new stage. The reasons are `architecture.md`, Repair.
A latched SP moves no stage: its drain deletes its repairs (SPD6, SPD15).

AR12. **Retire timer.** For a live SP where reactions run — after AR3's
suppression — and before the reactions, the pass deletes at most one
retired leg, whoever's repair retired it, through `model.DeleteRetiredLeg`
with the cluster's stored conf. It walks the SP's retired legs oldest first
by `retired_epoch`, then by leg id. A leg younger than `RetiredLegMaxAge`
by its `retired_epoch` is passed over with no record; by that age the
copy is stale. An old one is kept, and `retired leg kept` logs the reason,
while its group is not found in the parent the record names
(`grp_not_found`) or is a deleting clone's (`clone_deleting`), whose
drain gives the leg back (CLD9); while the group has a repair
(`grp_has_repair`); while the group's row in the current primary's latest
report reads anything but `OK`, or while that report is of an observation
older than the snapshot's `SpRev` revision (the `grp_row_` reasons of AR8's
gate on the group's row, `grp_row_stale` included); while that report does
not set
the group's whole flag (`grp_not_whole`); and while a leg of the group's
`leg_list`, or that leg's side, has an `err_epoch` (`grp_leg_unhealthy`).
Reasons: the retired leg's data exists twice elsewhere only while the group
is whole, so a group waiting for its legs, through a repair or until md
makes its array whole, keeps its retired copies; a row turns `UNKNOWN` when
the primary's stream dies, while a plain field such as the whole flag does
not, so the row must read `OK`; the timer reads the row and the whole flag
only from an observation at the snapshot's revision or later, since an
older report's whole flag can be from before the group's last repair began,
read while the bad leg was still in sync, while a finish has since put in
its place a new leg md may still copy onto; and md learns
that a member is dead only from IO, so an idle array over a dead leg still
reads whole, while the leg epochs come from probes that run without IO.
The first old leg nothing keeps is deleted, and the step ends. The delete
carries the snapshot's `SpRev` revision as its expected revision (MD6), so
a pool that changed since the snapshot the conditions were judged on
refuses it, and the next pass judges again; an earlier bump of the same
pass, a stage move's (AR11), a forced-start clear's (AR13) or a clone's
final STM's (CLD9), changes it too, so after one the delete is refused as
stale and waits a pass. A refusal or a
failure logs `retired leg kept` with its reason, the op's own for a
refusal (MD7), and ends the step as well. A delete logs `retired leg
deleted`.

AR13. **Forced-start clear.** For a live SP, right after AR11's stage
moves and before AR3's suppression — so at every `sp_level` — the pass
walks the SP's groups, the slices' in `slice_id_list` order, meta groups
before data groups, then the clone meta groups in `clone_name_list` order,
and clears each forced-start mark (`architecture.md`, Repair) that the
current primary's latest report (the report AR1 takes) shows done: the
group's row (`grp_id_to_md_raid`) reads `RES_STATUS_OK`, the group's
in-sync list (`grp_id_to_grp_info`), which names a repair's new leg as it
names a leg of the group's `leg_list` (`cnagent.md` CN28), holds a leg
whose own row (`leg_id_to_leg`) in that report reads `RES_STATUS_OK`, the
leg the mark names or another, and the report is of an observation made
at the mark's revision or later. The revision is the mark's, not the
snapshot's that AR8's gate reads: the mark carries the revision its own
write committed,
so a report of the array as it ran before the operator's call never clears
it. The op, `model.ClearForcedStart`, is handed the group's parent and id
and the mark's leg and revision, and clears only the mark they name,
skipping a group that is gone, a deleting clone's, and a mark replaced or
withdrawn since the snapshot (MD6); it takes no revision token and raises
`SpRev`, so the fan-out hands every cntlr the group without its mark. A
mark the report does not show done logs nothing, and neither does a
primary that has reported none, nor a mark the op skips, which the next
pass reads as it then stands. A due mark of a deleting clone's group is
handed to no op and logs `forced start kept` with the reason
`clone_deleting`; an op that refuses or fails logs `forced start kept`
with its reason, the op's own for a refusal (MD7), and the next pass
judges again. A clear logs `forced start cleared`, naming the group, the
leg, the mark's revision, the primary, the revision of its report and the
revision the op's own STM committed. Reasons: a mark that stayed would
start the group's array again from the named leg at its next fault, from a
leg that may be stale by then, and the operator uses the forced start at
the levels where reactions stop. Reason for any leg: an array that runs
on a member md holds in sync on a leg that serves needs no forced start,
and the primary never stops a running array for one (`cnagent.md` CN12),
while a mark left standing over it — its named leg dead after the start,
or failed by md before a mark written over a row that did not read `OK` —
would hold leg repair off the group (AR8) and start the array from that
leg, stale by then, at its next stop. Reason for the leg's own row, the
named leg's included: md keeps a dead member in sync while it retries a
copy whose only source died, the array's state swinging back to running,
and the named leg is that dead member when the mark is written while it is
dead, as `architecture.md`, Known limits, advises; after a start the named
leg serves, and its row reads `OK` once the primary's probe has passed on
it (`cnagent.md` CN11). The clear is
no reaction and not the pass's one action (AR2); its `SpRev` bump makes AR12's delete of the same
pass wait a pass, as a stage move's does, and leg repair's create, which
reads the mark in the pass's snapshot, still holds the group in that pass
(AR8). A latched SP clears no mark: its drain deletes its groups.

### The sp drain

`DeleteStoragePool` does not tear an SP down. It LATCHES it — `deleting`
true plus one `BumpSpRev`, nothing else (`architecture.md`, Storage pools;
`gateway.md`, Storage pools and GrowSlice) — and the sp coordinator takes
it apart in steps whose size is a constant. A one-shot teardown is
unbounded in the DN dimension, and `GrowSlice` lets a slice's group count
grow to `MaxGrpCntPerSlice` per group list (`architecture.md`, GrowSlice),
so no single transaction could ever be proven legal.

SPD1. **The allocator's real group shape is a named constant.**
`MaxAllocLegPerGrp`, the allocator's per-group leg count (`architecture.md`,
Cardinality limits), is CITED from all three places that choose a leg count
— `legCntOf` in `gateway/alloc.go` and in `model/ops.go`, `legCnt` in
`worker/reaction.go` — and from the SPD14 tripwire, the tripwire of the
clone drain's final transaction (CLD11) and `CreateClone`'s (`gateway.md`,
Constants this document owns), so widening the shape fails a test instead
of a deployment.

SPD2. **Load and refuse, never skip.** Each drain op loads the `SpConf`
inside its OWN STM and returns an `ErrPrecondition` — never a silent skip —
when the `SpConf` is missing, the `sp_id` changed, or `deleting` is false.
It is the inverse of `loadSpConfForOp`'s "sp deleting" refusal: normal ops
require the flag CLEAR, drain ops require it SET, and both share the
load-and-refuse structure. A reconcile loop that reads absence as
permission is how the wrong thing gets destroyed: an `SpConf` that is gone,
or whose `sp_id` moved because the name was deleted and re-created,
describes a DIFFERENT storage pool. (`sp_level` is deliberately not among
the three: SPD6.)

SPD3. **The repeat delete is a no-op.** `DeleteStoragePool` latches the pool
as `gateway.md`, Storage pools and GrowSlice, describes, a repeat delete
writing and bumping nothing; the drain starts from that latch.

SPD4. **The check and the latch are atomic.** The emptiness check and the
`deleting` put share one STM, and once latched the SP refuses every other
mutator, as `gateway.md`, Storage pools and GrowSlice, describes — so no new
child can appear after the check, ever.

SPD5. **The latch is one-way.** No code path in any component writes
`deleting` false on an existing SP — there is no undelete (`gateway.md`,
Storage pools and GrowSlice) — which is what makes SPD8's derivation total:
a latched SP has exactly one future.

SPD6. **Entry and cadence.** AR3 splits: a pass over an SP with `deleting`
true runs at most ONE drain step, no reaction, neither of the two pass steps
of a repair (AR11, AR12) and no forced-start clear (AR13), regardless of
`sp_level` suppression — "at most", because the gates ahead of the branch
still apply: a pass whose `LoadSp` failed, or whose cluster conf is missing
or unusable, runs nothing at all and retries on the next tick. (The SP's OWN
gates, its `bdev_conf` and its groups' lengths (AR1), are deliberately NOT
among them: the drain reads no geometry, and gating it there would make an
SP whose stored geometry cannot be read permanently undeletable, the latch
being one-way.) The FIRST step comes from the ordinary `cntlr_interval`
pass — the latch's `SpRev` bump reaches the coordinator as a desired
change, which drives the fan-out, not the pass. After that a committed step
ends in `BumpSpRev` (except the last), and once its call succeeds the step
itself, not that bump, wakes the coordinator for the next step at once, so
the drain is its own tick. That wake-up loads the SP afresh and runs the
drain step alone, behind the same gates as the pass's drain branch: a
failed load, or a cluster conf missing or unusable, runs nothing and leaves
the drain to the next pass. It evaluates no reaction and re-seeds no health
memo, because a drain step calls no agent (SPD13) and both stay the
`cntlr_interval` pass's (AR1, HL3). A step that removed nothing does not
wake it, and is picked up by the next pass instead. A FAILED step whose
transaction did not commit changes nothing, bumps nothing and is retried on
the next tick, forever (RW12, no backoff): there is no terminal-failure
state, because a delete that gave up would only strand garbage. A step
whose call fails after etcd applied its transaction logs `sp drain failed`
all the same, posts no wake-up and is not retried as such: the next tick
derives its step from the freshly loaded `SpConf` (SPD8), which already
holds the commit, and after the last step there is no next tick (SPD12).
Progress is already visible through `GetStoragePool` — `deleting` true and
a shrinking inventory — so no status field and no progress key is added.

SPD7. **Fan-out tolerance.** The drain's first step leaves an SP with NO
cntlr, a shape `CreateStoragePool` can never produce. `buildCntlrPlans`
yields an empty plan set for it, and `buildSidePlans` leaves every side
child IDLE (RW14: stopped, none started, and no re-resolution armed, since
nothing is unresolved) with an `sp sides idle` record whose reason is "no
cntlr" — not RW15's `primary_cn_id` zero, which the dn agent's export list
would read as a real CN id and build a dm-error, a dm-linear, a subsystem
and a namespace for the CN numbered zero. (An SP that merely has no
PRIMARY among cntlrs that do exist keeps RW15's documented behaviour.) The
sides are removed by each DN agent's own sweep as the batches empty the DN
pointer lists, not through these children.

SPD8. **Step selection.** The step's phase comes from the freshly loaded
`SpConf` ALONE, and the first match wins:

1. `cntlr_id_list` non-empty: **D1** `DrainSpCntlrs`;
2. else `repair_name_list` non-empty: one repair batch, `DrainSpRepairs`
   on `SpDrainRepairBatch` of the same load (SPD15);
3. else `retired_leg_id_list` non-empty: one retired-leg batch,
   `DrainSpRetiredLegs` on `SpDrainRetiredLegBatch` of the same load
   (SPD15);
4. else `slice_id_list` non-empty: **D2** `DrainSpSlice` on the LOWEST
   listed slice id;
5. else **D3** `FinishSpDelete`.

Nothing is remembered between steps
and there is no progress key — it would be a second copy of the truth that
can disagree with the first — so crash, restart and shard handoff all
resume through this same derivation. The accepted transient two-owner
overlap (VW7) is safe for the usual reason: both owners run the same
guarded op against whatever remains, the loser's STM fails its compares,
and a step is an idempotent "pop what is still there".

SPD9. **D1 — cntlrs first.** Every cntlr in one transaction, BEFORE any
slice work, deliberately inverting the naive slices-first order: every
cntlr stacks the WHOLE SP on its CN and the coordinator keeps syncing
during the drain, so slices-first would make every CN reload pool concats
and disband md arrays on every batch, racing the DN export teardown each
time, for stacks nothing will ever use. Cntlrs-first tears each CN stack
down exactly once through the pointer diff and frees CN capacity at once;
the remainder is pure DN accounting. It skips `DeleteCntlr`'s
disabled-first and non-primary preconditions, which exist to protect host
IO and discovery: a latched SP has an empty `nqn_list`, hence no
subsystems, no `CdcEntry` keys and no host paths.

SPD15. **The repair and retired-leg batches.** A pool's repairs and retired
legs go before its slices, the repairs first: a side is given back by its
group's `ext_cnt`, which D2's pop takes away. Each kind goes in batches,
`DrainSpRepairs` and `DrainSpRetiredLegs` (MD6), of at most
`MaxDelRepPerTxn` repairs or `MaxDelRetiredLegPerTxn` retired legs of ONE
parent, the slice or the clone that holds their groups, taken in
`repair_name_list` or `retired_leg_id_list` order from the parent of the
first listed one (`SpDrainRepairBatch`, `SpDrainRetiredLegBatch`, exported
so that every caller derives one batch the same way). One parent per
batch, so that a batch reads and writes one parent record, as a D2 batch
touches one slice (SPD10). A repair batch gives back the side of every new
leg, a fresh one and one taken from a retired leg alike, since the pool is
going away; a retired-leg batch gives back every retired side; each clears
its group's link to the record, deletes the key with its entry in the
pool's list in the same transaction, and bumps `SpRev`. The order is a
precondition, as D2 makes D1's one: a repair batch refuses while cntlrs
remain, a retired-leg batch while cntlrs or repairs remain, D2 while
repairs or retired legs remain, and D3 while any of the four lists holds
an entry (SPD12). A name or id the pool no longer lists is skipped, as for
D2's slice, since another owner's batch removed it.

SPD10. **D2 — one slice batch.** Up to `MaxDelGrpPerTxn` groups of ONE
slice, popped from the TAIL of `data_grp_list` first and then, if budget
remains, from the tail of `meta_grp_list`. A batch MUST NOT touch a second
slice even when the first has fewer groups left than the budget.
Tail-popping is not a detail: a group's position in its list and a leg's
`leg_idx` inside it are the md member order, so no surviving group or leg
is ever renumbered.

SPD11. **D2's slice-final half**, the same batch as SPD10's and never a
step of its own. The batch that empties a slice deletes the `Slice` key AND
its `slice_id_list` entry in the same transaction — there is no "empty
slice" intermediate state, and `model.LoadSp` iterates the id lists, so a
dangling id would break every later load.

SPD12. **D3 — the final keys.** `SpConf`, `SpName`, `SpRev` and `gateway.md`
GW12's deletion half on `SpGlobal`, guarded on the cntlr, repair, retired-leg
and slice lists all being empty so an owner one step behind cannot skip to
the end. It is the one drain STM that
does NOT bump `SpRev`: it deletes the key, which is already the shard worker's
stop signal, so the drain terminates itself in the transaction that finishes
the job. The step runs under a context that the coordinator's stop does not
cancel, bounded by `etcdutil`'s own budget (EU5): the stop its own delete
brings (SW3) can come before the commit's reply has reached the step, and a
stop lets a call in flight finish (RW11), so that stop never turns a
committed step into an `sp drain failed`. The step's time limit or a lost
connection still can end the call after etcd applied the commit; the record
then says `sp drain failed` for a pool whose keys are gone, and the step is
not run again, since its own delete has stopped the pool's coordinator
(SW3), so no `sp drained` follows (SPD6). After the commit the name is
reusable.

**A lost sub-object key wedges the drain, deliberately.** D1 refuses when a
listed `Cntlr` key is absent, D2 when a listed `Slice` key is, and the SPD15
batches when a listed `Repair` or `RetiredLeg` key is, or the parent it
names; each is the record that says which node reserved what, so without it
the release cannot be made exact, and a drain that carried on would
under-credit a node silently and for ever. The opposite call is made for a
missing `DnConf` or `CnConf`: those are the RECEIVING ledger, there is
nothing left to credit, and skipping keeps the SP deletable (`releaseCn`'s
stance). The cost of the refusal is that such an SP stays latched — the
latch is one-way (SPD5) and nothing else removes the keys — so the drain
names the missing key on every tick in `sp drain failed`, and an operator
restores it by hand — a raw etcd put of the `Cntlr`, `Slice`, `Repair` or
`RetiredLeg` record — to let it finish. That is a louder and more
recoverable state than a silent ledger drift, which is why it is the
direction chosen.

SPD13. **Asynchrony.** No drain STM waits on, calls or verifies any agent.
Etcd emptiness MAY outrun physical teardown — an agent that is down keeps
its stale stacks until its next syncup. What makes that safe is that each
agent derives what to REMOVE by enumerating what its node actually holds
and subtracting what the desired state wants (`architecture.md`, Teardown
by sweep): an SP whose keys this drain deleted leaves the pointer lists,
and the next `SyncupCn` or `SyncupDn` sweeps its devices away by name, with
no plan, no list and no memory of the drain needed at either end — a
removal that does not succeed is reported as `ReplyCodeLeftover` and
retried on every round (RW4 step 5) instead of being forgotten. This is
the system's existing convergence contract, not new risk.

**Budget consistency.** Partial teardown is a real, observable state, and
what a single transaction would protect is not atomicity but AGREEMENT: DN
and CN budgets must never disagree with the keys that describe them. Every
batch releases budget in the SAME transaction that shrinks the describing
key, so at every commit boundary the keys and the budgets agree exactly. No
metadata unit is released: a latched SP holds no migration (SPD4), so no
DN holds any of its units.

**Transaction budget.** etcd caps a transaction at the largest of its
compare, success and failure counts, and `etcdutil`'s serializable-snapshot
STM compares every key it READ *and* every key it WROTE, so the compare
count is what binds. Per D2 batch the compares are a constant few for the
SP's own keys plus a constant few per distinct DN the batch touches, and
that DN count is bounded by `MaxDelGrpPerTxn` groups of at most
`MaxAllocLegPerGrp` legs each: D2 runs only once the pool's repairs and
retired legs are gone (SPD15), so a popped group gives back the sides of
its legs alone. Sides contribute one DN apiece because a latched SP has no
migrations and therefore no two-side legs. An SPD15 batch costs a constant
few for the SP's own keys and its parent, plus, per repair or retired leg, its key
read and deleted and its one side's DN, at most `MaxDelRepPerTxn` or
`MaxDelRetiredLegPerTxn` of them: a repair has one new leg of one side.
D1, D3 and the latch are trivially legal. SPD14 is the tripwire pair that
keeps it so. The D2 batch is one of the bounded transactions above etcd's
default cap — the SPD15 batches, the created flip's transaction (RW19) and
the clone flip's (MD6) are the others — and not the one `EtcdMaxTxnOps` is
sized by: that is `CreateStoragePool`'s widest shape (`gateway.md`,
Constants this document owns).

SPD14. **Tripwires.** The budget of SPD13 is kept legal by a pair of tests:
an arithmetic assertion over the NAMED constants, and a maximum-shape
batch committed against a real etcd started with `--max-txn-ops` at
`EtcdMaxTxnOps`, for D2 and for each SPD15 batch, so a widening of
`MaxDelGrpPerTxn`, `MaxAllocLegPerGrp`, `MaxDelRepPerTxn` or
`MaxDelRetiredLegPerTxn` that would overrun the
budget fails a test instead of a deployment (SPD1).

### The clone drain

The clone drain is the sp drain's sibling. `DeleteClone` does not sweep a
clone's bitmap chunks. It LATCHES the clone — `deleting` true, the destination
namespaces resumed, one `BumpSpRev` (`architecture.md`, Clones; `gateway.md`,
Clones) — and the sp coordinator removes the chunk keys in batches of a
constant size and then the clone itself. A sweep of the whole rectangle of
chunk keys in one transaction would grow with the slice and chunk ceilings,
which can still grow; a batch is a constant number of ops whatever they
become.

CLD1. **Live-clone gate.** The gateway refuses every mutation of a deleting
clone but `DeleteClone` itself, as `gateway.md`, Clones, describes; CLD9's
emptiness guard rests on the append half of that gate, since after the latch
no chunk key can ever appear again.

CLD2. **Load and refuse, never skip.** Each drain op loads the `SpConf` and
the `Clone` inside its OWN STM and returns an `ErrPrecondition` when the SP
is missing, its `sp_id` changed, the SP is itself `deleting`, the `Clone`
is missing, its `clone_id` changed, or the `Clone`'s `deleting` is false.
It is SPD2's shape verbatim and inverted the same way: normal clone RPCs
(CLD1) require the flag CLEAR, drain ops require it SET. The SP-deleting
arm cannot fire — `DeleteStoragePool` needs an empty `clone_name_list` and
a draining clone keeps its name there — and is guarded anyway, so the two
drains can never run on one SP at once.

CLD3. **The repeat delete is a no-op, checked in PHASE 1.** `DeleteClone` on
a clone whose `deleting` is already true writes nothing, bumps nothing and
calls no agent, the check sitting ahead of the hydration check because the
stack is gone by then (CLD5), as `gateway.md`, Clones, describes.

CLD4. **The latch, and what it does not write.** `DeleteClone` latches the
clone — the destination namespaces resumed, `deleting` true, one `SpRev`
bump, and neither the `Clone` key, a chunk key nor `clone_name_list`
touched — as `gateway.md`, Clones, describes; the drain starts from that
latch, and the surviving name is what CLD9 removes last (SPD11, the sp
drain's slice-final rule, applying verbatim). Force-deleting an unhydrated
clone exposes unhydrated data on the resumed namespace.

CLD6. **The latch is one-way** — SPD5 scoped to a clone: no code path in any
component writes `deleting` false on an existing `Clone`, so the latch of
`gateway.md`, Clones, is a point of no return across restarts of every
component, and CLD9's emptiness argument, CLD1 and CLD3 all rest on a flag
that never clears.

CLD5. **Exclusion is the teardown.** From the first post-latch fan-out the
deleting clone is absent from every cntlr's `clone_list` and from the
primary's chunk-push plans (BM4 targets), and the repair of its clone meta
group from every cntlr's `name_to_repair` (RW16), at every `sp_level`; the
cn agent's sweep (`cnagent.md` CN21) tears the clone's stack down, the
array or group device and the legs of its clone meta group included, and
CLD9's transaction takes the pointers of the clone meta group's sides —
those of its repair's new leg and of its retired legs included — out of
their DNs' records, the sp worker driving those sides until then (RW14),
so each DN's agent tears its side down as it does any side whose pointer
left; there are ZERO agent changes: to an agent either is
indistinguishable from a post-delete syncup. Until that transaction the
repair flip skips the sides of that repair (RW18), the stage moves skip
the repair (AR11) and the retire timer keeps the group's retired legs
(AR12), since `SetRepairStage` and `DeleteRetiredLeg` refuse a clone being
deleted and `FlipRepairProvisioned` skips one (MD6), and CLD9 gives the
records back with the group (`architecture.md`, Repair). Full absence is
deliberately distinct from level suppression, under which the cn agent keeps a clone's local chunk files for
a later rebuild (`cnagent.md` CN21). The teardown's disconnect of a source
that is a transfer of the cluster (`cnagent.md` CN21) runs off the pass
(`cnagent.md` CN10), so until a later pass finds the
source gone the converge answers `ReplyCodeLeftover` (RW5). No push of the
clone is submitted once the exclusion has reached the primary's child, which
is when the RW14 sides-first hold the latch's fan-out lands in releases the
cntlrs. Until then that child drives the pre-latch plan, whose clones still
carry the clone's chunks, and a `SyncupCntlr` reply can submit a push from
it (a leftover reply re-syncs every round, RW4 step 5), while a pass, or
the drain's wake-up (CLD7), inside the hold may already run CLD8 batches; a
push already submitted also runs on after the hand-over. Such a push can
find a chunk key a batch deleted and end with `bitmap push failed` (BM6),
which is harmless: the clone is going away.

CLD7. **Cadence and step selection.** The drain runs ALONGSIDE the normal
reaction pass, not instead of it — this is the deliberate deviation from
SPD6, whose latched SP runs only drain steps: here the SP is healthy and
its other children must keep converging. Each pass runs at most one step
per deleting clone, bounded by `MaxCloneCntPerSp`, in `clone_name_list`
order; the steps run ahead of EVERY gate of the pass — the cluster-conf
cache lookup, `ValidateClusterConf`, the SP's own `bdev_conf` and
group-length gates (AR1) and AR3's suppression — because none of them
applies. A batch whose call succeeds also wakes the coordinator at once
(CLD12), and that wake-up runs these steps alone: it loads the SP afresh
and runs at most one step per deleting clone, in the same order and with no
gate in front of them, and it evaluates no reaction and re-seeds no health
memo, because a batch calls no agent (CLD10) and a pass would evaluate
every reaction again for each batch; the reactions and the health memos
keep their `cntlr_interval` pass (AR1, HL3). A doomed clone drains at any
`sp_level`; the drain reads no geometry; its batches read no cluster conf;
and a gate that could stop it would strand a latched clone permanently, the
latch being one-way. Its final STM maintains DN capacity keys, as the sp
drain's D2 does, so it needs the ladder: the step takes the cluster conf
from the worker's cache itself, and the STM refuses an unusable one at its
top (MD6), so a cluster missing from the cache, or a conf that does not
validate, fails that one step — logged as `clone drain failed` and run
again by the next pass or wake-up (CLD10) — and stops no batch and no
other clone's step. The derivation, from the snapshot of
the pass or of the wake-up's load alone: surviving chunk keys in the
state's `CloneBmIdx` mean one batch on the LOWEST `MaxDelBmPerTxn` of them
in `(src_slice_idx, bm_idx)` order; none means the final STM. No other
state is consulted — not the `Clone` record, not the rectangle, not a
progress key — so crash, restart and handoff all resume through it.
Ascending order is free to choose (chunk keys are independent and
self-positioning, unlike the sp drain's tail-pop) and is picked for
determinism.

CLD8. **The batch.** Deletes ONLY keys named by the caller's keys-only
snapshot scan, at most `MaxDelBmPerTxn` of them, each delete an idempotent
pop; refuses a larger batch, so a caller that handed over its whole scan
could not silently rebuild the unbounded sweep. It MUST NOT rewrite the
`Clone` record, and it neither reads nor bumps `SpRev`. No bump, because
the keys belong to a clone the latch already took out of every cntlr's
request (CLD5): a bump would re-send the SP's whole desired state to every
side and cntlr for deletes no agent reads. No read, because CLD2's load of
the `SpConf` already refuses an SP that is gone, the sp drain's final step
deleting the `SpConf` and the `SpRev` in one transaction (SPD12). A batch
whose call succeeds wakes the drain for its next step itself, not through a
bump (CLD7, CLD12). Physical effect: none. The CN drops its local chunk
files in the sweep that follows the exclusion (CLD5), agents never read
etcd, and a push of the clone that reads a chunk key a batch deleted ends
there, harmlessly (CLD5).

CLD9. **The final STM.** Delete the `Clone` key, put the `SpConf` with the
name removed from `clone_name_list`, give every side of its clone meta group
back to its DN, delete the group's `Repair` and its `RetiredLeg`s, with
their entries in the pool's lists and their sides given back too,
`BumpSpRev` — the removals together and never separately, and the release
with them, in the transaction that removes the record the group is read
from, whose `ext_cnt` the sides go back by, so each is given back exactly
once (MD6's `FinishCloneDelete` says how: the sides as an sp drain batch
gives a popped group's back, with one `DnRev` bump per DN, which takes them
out of their dn agents' syncups, CLD5). No CN is touched: a clone charges
none (`architecture.md`, Metadata units). The STM is handed the cluster conf
and refuses an unusable one first (CLD7). It is invoked only when the
derivation's scan found zero surviving chunk keys, and that emptiness is
stable even though an STM cannot range: after the latch commits no chunk key
can ever appear again (CLD1 refuses an append inside an STM that reads the
`Clone`, and an append that read the `Clone` BEFORE the latch fails its
compare because the latch rewrote that key), and batches only delete. Unlike
the sp drain's D3 this BUMPS `SpRev`: the SP outlives the clone, so the bump
is every mutator's normal epilogue rather than a stop-signal deletion.

CLD10. **Asynchrony and retry.** SPD13 verbatim: no drain STM waits on,
calls or verifies any agent; etcd emptiness MAY outrun physical teardown,
and a CN that is down keeps its stale stack until its next syncup, which
removes it because the clone is no longer in the request and not because
anything recorded that it should be. A failed step whose transaction did
not commit changes nothing, bumps nothing and is run again by the next pass
or wake-up that runs the drain (CLD7), forever and with no backoff (RW12),
as for the sp drain (SPD6). A step whose call fails after etcd applied its
transaction logs `clone drain failed` all the same, posts no wake-up and is
not retried as such: the next step is derived afresh (CLD7) from chunk keys
that no longer hold the batch's, and a final STM that committed took the
clone's name out of `clone_name_list`, so no step runs for that clone again
and no `clone drained` follows. Progress has exactly two user-visible
states — `deleting` true, then `NOT_FOUND` — accepted deliberately, since
even a maximum-shape drain is a handful of sub-second transactions after
the latch: its batches and the final STM (CLD12).

CLD12. **Termination and the revision contract.** The latch and the final
STM each end in `BumpSpRev` with their own op tag; a batch bumps nothing
(CLD8), and neither does a repeat delete or a step whose transaction did
not commit. Termination is structural: no committed batch can ever ADD a
chunk key (CLD1 closed the only writer), the set is finite, the final STM
removes the clone, and a pass or a wake-up that finds no deleting clone
runs no drain step, so the
drain commits and bumps nothing (that pass's own reactions are unaffected,
CLD7) — under one owner, one bump for the latch and one for the final STM
per deleted clone, whatever its batch count. Note the shrink is a property
of the SET, not of each batch: the loser of an accepted two-owner overlap
commits a batch of keys the winner already popped, so ITS batch shrinks
nothing — and still terminates, because its next step re-scans, sees the
winner's deletes and moves on. That is also why every batch whose call
succeeds wakes the drain (CLD7) whatever it removed, where the sp drain
wakes only on progress (SPD6): that guard would be dead code here, a batch
reporting the size it was handed, never a count of keys it found. The two
bumps invalidate a client's stale `gateway.md` GW6 token exactly as
`ReplaceCntlr` does: correct, and not new; a batch leaves a token as valid
as it found it.

CLD11. **Transaction budget, and its tripwire pair.** The batches are
ledger-free, so one line per batch. etcd caps a transaction at the largest
of its compare, success and failure counts, and `etcdutil`'s
serializable-snapshot STM compares every key it READ *and* every key it
WROTE, so a batch costs `MaxDelBmPerTxn` compares plus a constant few for
the `SpConf` and the `Clone` — the bound, and independent of every ceiling
constant. The final STM is a constant few plus a few per distinct DN of its
clone meta group — the DN's record and revision read, and the record, its
capacity key's delete and put and the revision written — plus a read and a
delete per `Repair` or `RetiredLeg` key, and a clone meta group has at most
`MaxAllocLegPerGrp` legs, one repair's new leg and `MaxRetiredLegPerGrp`
retired legs, of one side each, its legs never being migrated, so those
bound the rest (SPD1); a tripwire asserts that arithmetic from the named
constants and a maximum-shape final STM is committed against a real etcd, as SPD14 does
for the sp drain. The latch is strictly smaller than a sweep of the whole
rectangle in one transaction. A batch and the final STM also fit etcd's
DEFAULT cap, which asks no deployment change: the requirement is
`EtcdMaxTxnOps` for the transactions that do NOT fit it, such as
`CreateStoragePool`'s maximum shape, which is what that number is sized by
(`gateway.md`, Constants this document owns), the sp drain's D2 batch
(SPD13) and its repair and retired-leg batches (SPD15), the created flip's
transaction (RW19) and the clone flip's (MD6).

## Log records

The worker's records are JSON records under the rules of `log.md`, R1 to R12,
carrying a trace id: the one RW10 gives the unit of work that wrote the
record; or else the one RW10 gives the part of the incarnation that wrote
it for that part's life, so that one id groups, for example, every
heartbeat and registry scan record of an incarnation; or else, on a record
written outside every unit of work and every such part, CM4's startup trace
id, the one id with no seed prefix (CM6). Record names (the `msg` strings) are
normative — the worker suite parses them; what each record carries is the
code's. Two families are `log.md`'s and are not restated here:
the records `etcdutil` emits for every etcd read, write, range and watch event
(`log.md`, etcd), and the `syncup leftover` record RW5 emits (`log.md`,
Leftovers). The records of the client interceptors are `grpc.md`'s (L1 to
L6); an agent's log mirrors each as its server-side record under the same
`trace_id`, which on a `Check*` stream is the id the stream was opened under,
for every round, while a round's own id is the `trace_id` inside its request
(`grpc.md` T3).

**Lifecycle and membership.** `worker starting` and `worker stopping` mark
the process's start and its graceful stop (CM4, CM5, CM6);
`worker registered` the first successful put of a role (VW2);
`membership observed` every observed transition and `membership committed`
every commit that changes the effective membership (VW3, VW6); `shard owned`
and `shard released` the ownership diff, the owner being the logging worker
itself and a release logged after the join (VW9, SW5); `worker fenced` the
fence, its `reason` naming the cause as VW8 lists them and the record naming
the old and the new seed (VW8); `worker reg watch restarting` a registry
watch that ended on an error or a compaction and is reopened with a rescan
(VW3, EU3).

**Revision workers.** `revision worker started` and
`revision worker stopped` mark the life of a per-object loop (SW3, RW11);
`rev watch restarting` a shard's revision watch that ended on an error or a
compaction and is reopened with a rescan (SW4);
`cluster conf watch restarting` the same for the process's one `ClusterConf`
watch (RW21); `cluster conf missing` the idle of a loop whose cluster is
absent from the cache, once per idle period (RW9); `invalid stored conf`, at
Error, a stored conf one of the three conf gates refused — a revision
worker's (RW9), the sp fan-out's (RW14) or the pass's (AR1), whose
cluster-conf gate a latched SP's drain wake-up applies too (SPD6) — once
per distinct error and never once per round; `syncup result` every
`Syncup*` reply or failure (RW5, RW10), and `syncup rejected` a rejecting
code, at Error for a stale revision (RW5); `late observation` a round HL7
judges whose observation's age, counted from no earlier than the first
round after a pause, exceeds one interval, or whose reply carries no
answer time, too old or not, but for the leave HL7 gives the round right
after a `Syncup*` reply (HL7), the record telling a round whose reply
carries no observation from one that answers from an observation;
`dn free count disagrees`, at Error, the second compare in a row, each on
a round's new observation, that found a disk node's own free count other
than its record's, once until a compare agrees (RW13, RW23).

**Health and the sp role.** `health changed` every HL1 or HL2 transition,
judged against the memo HL3 re-seeds from the record — the correction of an
epoch another observer wrote or cleared included —, a clean one with the
reason `initial` while its monitor has written nothing yet and `recovered`
otherwise: every worker start, every rejoin after a self-fence (VW8) and
every shard handoff gives the objects concerned new monitors, whose first
clean verdict on a healthy object is no recovery, and HL3 writes it
whatever the record holds, so an `initial` can also clear an epoch another
observer or an earlier owner left;
`cntlr settled` the write of HL2's settle: at most once per acquisition of
the primary role, the re-enable of a primary counting as one and none when
the cntlr is demoted before it settles, plus a repeat for a plan loaded
before the write landed (HL2) or for a second owner in an overlap (VW7);
`flip applied` a committed provisioned flip, the clone flip's and the
repair flip's included, a
committed created flip or a committed cutover, naming the new `SpRev`
— for a provisioned flip and a cutover the revision their own transaction
wrote, never a later one, so a reply to the first request that carries the
change is never older than the record — and a cutover's naming the cntlr
whose report it acted on and that report's revision (RW18, RW19);
`sp flip failed` a flip or a cutover whose STM did not commit, which a
later report tries again (RW18, RW19);
`sp sides unsynced` the release of RW14's sides-first hold by its timer, one
record per hold and never per round, and expected in normal operation too:
for a new side whose first `SyncupSide` its disk node refused
(`dnagent.md` DN8), at a hold that waits for a side on a disk node that died
too recently for its epoch to be in the hold's load, or during the hold,
at a hold whose fan-out restarted a side while its old child was inside
a `Syncup*` that outlasts the hold (RW1, RW14), and at a hold that waits for
a side whose child a new owner's or a restarted worker's first fan-out
starts, or a fan-out restarts, and whose first round brings no `Syncup*`,
its agent already holding the revision, and so reports nothing (RW14,
HL7); `sp demotion unsynced` the
end of RW22's demotion
hold by its timer, one record per demotion hold and never per round, and expected
at every failover whose old primary does not report its demotion applied
within the wait — always for an old primary on a dead CN, or one cut off
from the worker (RW22).

**Bitmap pushes.** `bitmap pushed` every push that got a reply, with the
agent's code (BM3); `bitmap push failed` a push that got none — the chunk
unreadable or gone, or the call failed — or, beside the `bitmap pushed` of a
non-zero code, the agent's own explanation of it (BM6). The failure record is
non-normative — it names no decision — and exists because a push with no
reply has no code to report and must not be logged as a `bitmap pushed` with
an invented zero.

**Reactions and drains.** `reaction applied` a reaction whose op call
succeeded and `reaction skipped` a reaction the pass does not take, with its
reason (AR2; for AR8's gate, the status of the group's row it read, or
that its report is older than the snapshot),
and with the op's name as `op` when a `model` op refused it
(`ErrPrecondition`, MD7): one reason can come from the pass's own check or
from the op, and only the op's refusal always ends the pass, save a refusal
of AR8's drive step, which holds that repair's group alone, and the
create's refusal under the group's `operator_in_charge` flag, which holds
that group alone (AR2, AR8); each
record names its reaction's kind, leg repair's being four, its create and
the three steps of its drive step (AR8), and the record of a finish also
names the retired leg it gave back, if any, and that of a try-to-abort its
trigger; an op
call that failed otherwise is also logged `reaction skipped`, with its
error, and its transaction may still have committed, which the next pass
reads from the stored state;
`repair stage moved` a stage move whose call succeeded and `repair stage
skipped` a move AR11 finds due and does not make, with its reason, the
op's name as `op` when the op refused it, or its error, and a CREATED
repair's verdict that carries the primary's refusal of a take-back, with
`take_back_refused` and the refusal's text as `refusal` (AR11);
`retired leg deleted` a retire timer's delete whose call succeeded and
`retired leg kept` an old retired leg the timer keeps, with its reason, the
op's name as `op` when the op refused the delete, or its error (AR12);
`forced start cleared` a clear of a forced-start mark whose call wrote,
with the revision its own STM committed, and `forced start kept` a clear
AR13 finds due and does not make, with its reason, the op's name as `op`
when the op refused it, or its error (AR13) — none of these steps is a
reaction, so none logs a reaction record;
`sp drain step` every D1, repair, retired-leg and D2 step whose call
succeeded (SPD8), each with its own phase, and `sp drained` a D3 whose call
succeeded, the step that ends the drain (SPD12), so no step record carries
a final phase;
`sp drain failed` a drain step of any phase whose call failed, with its
phase, retried on the next tick with no terminal-failure state when its
transaction did not commit (SPD6);
`clone drain step` every batch whose call succeeded and `clone drained` a
final STM whose call succeeded (CLD8, CLD9, CLD10), so no step record
carries a final step; `clone drain failed` a clone-drain step whose call
failed, run again with no terminal-failure state when its transaction did
not commit (CLD10), and a final step not called because the cache holds no
conf of the cluster (CLD7). The two step records are non-normative — they
name no decision — but the worker suite counts them: they are the only records that
show a multi-batch drain advancing.

## Integration test plan

**What the suite proves.** `integtest/worker_test.sh` proves a real
`dnv-worker` fleet — the real `worker`, `model` and `etcdutil` packages over a
real single-node etcd — against fake dn and cn agents. The agents are fake
because the data plane is proven by the agent suites (`dnagent_integtest.md`,
Goal and scope; `cnagent_integtest.md`, Goal and scope); what this suite
proves is everything between etcd and the agent's gRPC surface: membership and
ownership, revision propagation, health bookkeeping, the flips, the pushes,
the reactions, the two pass steps of a repair, the forced-start clear, the
two drains and handoff —
the happy paths plus every failure
the worker is specified to handle; error paths of etcd itself (quorum loss,
compaction races) are out of scope. It runs on one server as a plain user,
because nothing it exercises needs root: real etcd, several real workers, fake
agents driven by a behavior file, etcd driven by a fake-gateway CLI, and
assertions over the JSON logs.

**What the Go cases prove.** Most of the worker package's Go tests drive its
layers directly, over an in-memory store on a fake clock or over the package's
shared etcd of EU7. The whole-worker cases run whole workers — the real
`worker`, `model` and `etcdutil` packages, from the vote layer down to the
syncups and the reactions — inside the test process, on the real clock, since
simulated time cannot run beside a real etcd. Each whole-worker case starts an
etcd of its own from the binary EU7 finds: the registrations, the revision
prefixes and the cluster confs a worker watches are keys of no one cluster, so
whole workers on a shared etcd would drive one another's objects, and a
private etcd lets the cases run in parallel. The agents are fake agents of
`integtest/agentfake`, served in the process and reached through the dial the
connection cache takes (RW7), and the cluster is planted through
`integtest/plant` with the placement the case chooses; the paragraphs on the
fake agent and on the driver below say what each does. A worker starts through
an unexported entry beside `Run` that takes the clock, the dial and the four
fixed timeouts: the per-call deadlines of a `Syncup*` and of a push (RW5,
BM3), the bound of the demotion hold (RW22) and the bound of each CM5 delete.
`Run` hands it the production ones — `DefaultWorkerSyncupTimeout`,
`DefaultWorkerPushTimeout`, `DemotionHoldTimeout` and `DefaultEtcdOpTimeout` —
and a case short ones, with the health-check intervals, the event thresholds
and the vote timers shortened too, so that a call no fake answers costs a case
seconds, not a minute; the `etcdutil` client bounds each of its own operations
as EU5 says whatever the entry is handed. A whole-worker case stops a worker
in CM5's order, or crashes it: the worker's agent connections and etcd client
are cut before its context ends, so a call it has in flight ends at once, its
registrations stay, and its peers find it dead by the dead threshold (VW3), as
they find a killed process. A restart is a new incarnation under a new seed
(VW1). A worker's records reach its case by the seed prefix of their trace id
(RW10) or by the seed they name as their own. Each whole-worker case asserts
on what etcd holds, on what each fake received and on those records, so a
case proves a whole path, from a planted desired state to the agents'
syncups and the worker's own writes, at the moments the fakes' levers
choose. The fake records each round's
own trace id (RW4 step 2), under which the round's syncup and verdict are
logged (RW10), so a case tells what a round caused from what a syncup did. A
handoff case crashes the worker that owns a pool while a lever holds a step of
a failover, a cutover or a leg repair open, so that the peer that takes the
pool's shard, handed nothing, must finish that step from etcd and its own
rounds alone. A case can also act from inside one of a worker's records, in
the call that logs it, so that a change lands between two steps of that call,
such as a scan and its commit. A whole-worker case that holds one dimension at
its maximum of `architecture.md`, Cardinality limits, asserts the state the
worker leaves in etcd and in the fakes, never how long it takes.

**Topology.** The developer machine drives one Linux server over ssh, as a
plain user with no sudo. The server runs one etcd on localhost, configured
with the request caps every etcd serving dnv must have (its transaction-op cap
at `EtcdMaxTxnOps` and its request-size cap at `EtcdMaxRequestBytes`, which
the suite reads from the driver it has just built instead of copying the
numbers), several workers carrying every role, fake disk and controller
nodes listening on the server's
own address, so that every stored `addr_port` looks like a production one and
the worker dials a real interface, and `workerctl`,
run on the server against etcd. The vote timers are shortened through the CM1
flags, so every membership change completes in seconds. Every case but the
failover and hold cases shortens its intervals, and the thresholds it means
to reach, through its cluster's and pool's stored conf, so every reaction
completes in seconds. The failover case keeps the shipped default intervals
and thresholds instead, since it proves the timing those defaults give; its
cntlr, side and leg thresholds are long enough that only failovers move a
role in it. The hold case shortens every interval but `cntlr_interval`, the
bound of the sides-first hold (RW14), which it lengthens so that a hold that
runs to its bound and one its sides end lie seconds apart in the fakes'
records, which carry the server's clock; its thresholds are long enough
that nothing reacts on health. The
driver reads every worker and fake log over ssh and asserts on it; a fake's
gRPC records carry each request's trace id, whose seed prefix names the
worker that sent it (RW10).

**Cases.** Each case runs in its own cluster after a restart of the worker
fleet that also deletes every dnv key from etcd, resets every fake and
truncates every log, so that neither membership state nor an earlier case's
objects leak into it: an earlier case's cluster left in etcd would stay
driven and write into this case's logs, and a fake still holding an earlier
case's revision would refuse this case's first syncup as stale, the
revision gate being per object and not per cluster.

* smoke — one worker alone converges a new SP: the fan-out, the rounds, both
  flips and clean health;
* revision — a bump and a moved endpoint reach the agent, a deleted
  revision key stops its worker, and a rejected reply is left to the next
  round;
* health — the health fields and the capacity keys follow what the agents
  report and whether they answer, for the nodes and for the three sp
  records;
* bitmap — chunks reach their targets one at a time, in order, an empty
  diff pushes nothing, and a rejected syncup is re-driven by the round's
  reply; its migration's cutover waits for the primary's live path to the
  destination, a standby's live path counting for nothing and the
  primary's path row in `ERROR` setting no health epoch, is logged once,
  naming the primary and a report at or after the destination's
  provisioned flip, and tells the source that its destination is
  provisioned only from the cutover's own revision on;
* reaction — every automatic reaction fires on its trigger and holds on its
  guard, and a settling primary is held to `cntlr_unhealthy`; leg repair's
  repair of a leg due on case 1 is created with a fresh new leg, moved to
  STARTED by the primary's verdict and finished, retiring the leg, and one
  due on case 2 is tried to abort once its bad leg reads healthy again
  before the copy starts, confirmed, moved to ABORTED and cancelled;
* repair — an operator's repair, driven through the model operations the
  gateway runs: its new leg's side is driven, zeroed and flipped
  provisioned in the repair and the repair carried to every cntlr; leg
  repair skips its group on every pass; the primary's verdict started moves
  it to STARTED and the operator's finish retires its bad leg, whose side
  keeps a provisioned request on its disk node; a second repair is aborted,
  its abort confirmed, moved to ABORTED and cancelled, giving its new side
  back; the retired leg is listed and deleted; the retire timer keeps an
  old retired leg while the primary reads its group not whole and deletes it
  once the group reads whole; a repair that takes a retired leg back, whose
  add the primary's verdict refuses, stays CREATED while every pass logs
  the refusal's text, and is aborted and cancelled, giving the leg back as
  a retired leg; the mark of an operator's forced start is cleared
  once a report of an observation at or after its revision holds the named
  leg in sync on a leg row that reads `OK`, the primary having been carried
  the mark first; while a mark stands, it rides to every cntlr and holds
  leg repair's create of its
  leg, ahead of the healthy-leg test, as long as the primary holds no leg
  in sync; once it holds the group's other leg in sync on a leg that
  serves, the named leg still out of sync, the mark is cleared, the clear
  hands every cntlr the group without the mark, and leg repair's create
  runs in a later pass; at a level where reactions stop a mark is
  cleared too, though not while the primary's reports are of an observation older than
  the mark, nor while its report of the mark's revision reads the group's
  row other than `OK`; and a group's `operator_in_charge` flag is set while
  reactions stop and cleared later, each write leaving the pool's revision
  as it was: while it stands leg repair holds the group's due leg on every
  pass, ahead of the group's repair, and creates no repair, while the drive
  step still tries to abort the worker's own repair of the group once its
  bad leg reads healthy again and cancels it once the primary confirms; once
  the flag is cleared the worker's repair of the leg is created;
* failover — failovers at the shipped default timers: a standby that stops
  answering leaves the discovery records and returns once it answers again,
  each time in its health write's own transaction, with no revision bump and
  no reaction; an unhealthy primary with no failover candidate stays in the
  records; a primary that answers neither its rounds nor its syncups is
  handed its demotion first, the sides follow one `DemotionHoldTimeout`
  later and the other cntlrs only once the sides have answered, the
  failover's own transaction takes the old primary out of the records, and
  its first clean round as a standby puts it back; a disabled primary that
  answers its demotion releases the sides at once; one missed round followed
  by an answer, prompt or late, or by refused replies, fails no primary
  over, while a primary that misses every round fails over once its
  threshold has run out; and a
  failover on its threshold waits for the coordinator's own verdicts, two of
  its rounds in a row, so after a handoff a stale epoch fires nothing before
  the new owner's own, and a verdict counts whether or not it writes;
* hold — the sides the sides-first hold waits for: a side with no health
  epoch that never answers its syncup holds the cntlrs for the whole
  interval, while one whose epoch the fan-out's load holds is not waited
  for and the cntlrs follow the healthy side's answer; a bump inside a hold
  takes the sides again from its own load; and after a failover's demotion
  hold the wait leaves out a side that the load of the plans the demotion
  hold kept found unhealthy;
* drain — the sp drain takes an SP apart in bounded steps and restores every
  ledger, the clone drain removes a clone's chunks in batches that bump no
  `SpRev`, so the pool's revision moves once for the latch and once for the
  last step (CLD12), reaches the CN as an exclusion and in its last step
  gives the sides of its clone meta group, which the pool's coordinator has
  driven to provisioned and flipped (RW14, RW18), back to their DNs,
  extents, pointers and capacity keys, with one `DnRev` bump each, after
  which each DN's agent is sent a side list without its side (CLD9), and
  both resume from etcd alone after a restart of the fleet;
* vote — every shard has exactly one owner through a join, a crash, a
  graceful stop and a self-fence, and every request names its sender;
* handoff — a killed owner's shards are re-driven at the same revision with
  no handover state, and a flip in the middle of a handoff applies exactly
  once.

**Rules exercised.** The vote and handoff cases exercise VW1 to VW7 and VW9
to VW11, the vote case VW8 (a), and every case's fleet restart VW7, RW11
and CM5; SW1 to SW3, SW5 and SW6 are exercised by the smoke, revision and
handoff cases; RW1 to RW12 by the smoke, revision and health cases, RW10 by
the attribution of the vote and handoff cases; RW13 to RW21 (save RW13's
count compare, below) by the smoke case's request builders, the revision
case's moved endpoint, the bitmap case's migration confs and cutover and
the reaction case's grow confs, and RW14 to RW16's sides and requests of a
repair and RW18's repair flip by the repair and reaction cases; RW22, its
demotion hold ended once
by the demoted cntlr's report and once by its timer, with the sides-first hold
of RW14 behind it, by the failover case; RW14's choice of the sides its hold
waits for, at a hold's start, at a bump and after a demotion hold, by the hold
case; HL1 to HL6 (save HL1's rule for the disk counts, below) by the
health case and HL2's settle by the reaction case; the rewrite of the
discovery records by the listing rule (`architecture.md`, [D18]) in a cntlr's
health write (HL2) and in a failover on either trigger (MD6) by the failover
case, which finds each in its writer's own transaction, and in a cntlr
replacement (MD6) by the reaction case; BM1 to BM5 by the bitmap case; AR1
to AR9, and AR11 on the worker's repairs, by the reaction case, AR8's skip
of a group with an operator's repair, its hold on a group with a
forced-start mark and its hold on a group whose `operator_in_charge` flag
is set, the drive step's try-to-abort and cancel running under the
flag, AR11, its record of a
refused take-back included, AR12
and AR13, its clear at a level where reactions stop included, by the
repair case, and
AR10 on the failover's threshold trigger,
with AR5 at the default primary threshold (`architecture.md`, Common
validation), by the failover case; SPD6 to SPD13, CLD5 and CLD7 to CLD12
(save what the drain's wake-up leaves out, SPD8's repair and
retired-leg phases and the repair half of CLD5 and CLD9, below) by the
drain case, whose
`workerctl` latches only stand in for the gateway's SPD3, SPD4, CLD1, CLD3
and CLD4; MD2 to MD6, EU1 to EU6 and CM1 to CM6 by every case, every launch
and the driver; and the log records by every assertion. Left to the unit
tests: VW8 (b), (c) and (d), because a broken watch with live puts cannot
be induced from outside the process; that the drain's wake-up runs no
reaction and re-seeds no health memo (SPD6, CLD7), because a pass with no
reaction due and no epoch to correct leaves no record, so no case can tell
the wake-up from a pass; RW13's count compare and HL1's rule for the disk
counts, since no suite case sets `disk_counts`, the one lever by which a
fake reports a disk node's counts, the end-to-end suite holding real disk nodes
to the compare (`e2e_integtest.md`, The cases); RW23's count of new
observations only and HL7's late rounds and rounds whose observation is too
old, since no suite case sets `obs_age_ms` or `obs_none`, the levers by
which a fake answers from an old observation or from none;
SW4, compaction; BM6, because every code a suite case forces, a stale
revision or an unknown object, rejects the object's `Syncup*` itself, so no
push is attempted, let alone failed, while no suite case forces
`ReplyCodeLeftover`, the one forced code under which the worker can still
push and the fake refuses the push with that code, and `leftover_rounds`
leaves a push's code alone; AR5's and AR7's refusals (HL2's row classes,
and the rows of the last failover or primary replacement), since the
reaction case only keeps
clear of AR5's second refusal and fails its sole primary on a row of its own;
the cutover's wait for the destination's reason, its reading of a reply's
own info rather than the latest a child holds, its wait for a new
primary's own report after a failover, its refusal of a disabled primary's
path (RW18), and `StartCutover`'s refusal at `SP_LEVEL_NO_MIGRATION` and
above (MD6), which the bitmap case does not stage;
a bump or a cntlr replacement during RW22's demotion hold, HL2's rule that a standby's
clean reply below the revision its child drives is no verdict, AR10
holding back a cntlr replacement or a leg repair, and RW14's hold still
waiting, with no bump, for a side that turns unhealthy during the wait and
keeping its sides at a re-fan of the held revision, all of which no suite
case stages;
the `candidate changed` refusal of AR8's
create — a group that gained a DN after the pass's snapshot, or a stale
pick — with the next pass's retry, and its `grp_operator_in_charge`
refusal of a create from a snapshot older than the flag, with the scan
going on, since no suite case writes the flag between a pass's snapshot
and its create; AR8 on a clone meta group, its order
across the pool and its skip of a clone being deleted, since no suite case
fails a leg of one; AR8's healthy-leg test and its gate on the group's
row, since no suite case leaves a group with no healthy leg, holds a leg
out of a group's in-sync list or has a group's row read anything but `OK`
while a leg of the group is due for repair, but behind a forced-start
mark, whose hold AR8 checks first; AR8's try-to-abort on the
clocks of a new leg or of another leg of the group, the drive step's
refusals and the finish's give-back at the retired-leg bound, since no
suite case fails a new leg or a second leg of a group under repair or
fills a group's retired legs; the drive step's finish of a worker's repair
under the `operator_in_charge` flag, since no suite case starts a worker's
repair while the flag stands; of AR11, a verdict that says started
and abort confirmed at once, a stale verdict under another `rep_id`, and a
deleting clone's repair; AR12's holds other than the whole flag's, its
order and its one delete a pass; of AR13, its walk order, a mark on a clone
meta group or on a repair's new leg, a deleting clone's mark, a mark the op
skips, refuses or fails to clear, a primary that has reported none, and
the wait a clear's bump gives the retire timer's delete of the same pass,
since the repair case clears one slice's group's marks, each on a leg of
its `leg_list`, from a primary that reports throughout, and holds no old
retired leg then; HL2's epoch writes into a `Repair` or a
`RetiredLeg`, since no suite case fails a repair's new leg, its side or a
retired side; the restart of a side child whose side moves to another
record (RW14, HL2), which the repair case's finish causes and none of its
assertions reads; the tripwires of SPD1 and
SPD14 and the guards of SPD2; SPD8's repair and retired-leg phases and
SPD15's batches, since no suite case drains a pool that holds a repair or
a retired leg; the repair half of CLD5 and CLD9 — a deleting clone's
repair left out of every cntlr's request, the sides of its new leg and
retired legs driven until the final transaction, and that transaction
giving them back — since no suite case deletes a clone whose group holds a
repair or a retired leg; and the guards of CLD2 and the gateway halves
of CLD1 and CLD3, the gateway suite owning the clone latch.
The suite never produces `invalid stored conf`, because its driver resolves
every conf it writes, and the three conf gates are unit-tested instead.
Ten records no suite case stages: `sp drain failed` and `clone drain failed`,
which the drain case asserts at zero after every drain; `sp flip failed`,
which the bitmap case asserts at zero for its cutover; `cluster conf
missing`, which every suite case avoids by writing its cluster conf before
any rev key and no suite case asserts either way; `syncup leftover`, which
no fake produces on its own, since no fake computes a sweep verdict, and no
suite case forces that code or sets `leftover_rounds`;
`bitmap push failed`, for the reason given for BM6 above;
`late observation`, since no suite case sets `obs_age_ms` or `obs_none`;
`dn free count disagrees`, since no suite case sets `disk_counts`;
`forced start kept`, since every clear the repair case stages commits; and
`sp group missing`, since no suite case plants a repair or a retired leg
whose group is missing. `repair stage skipped` is staged only as the
record of a refused take-back, every stage move the repair and reaction
cases stage committing.

Out of scope: real agents, and real hosts, whose drop of a path the discovery
records withdraw is the e2e suite's to show (`e2e_integtest.md`, The cases);
more than one server; a three-member etcd; etcd quorum loss, restore or
compaction races; clock-skew injection, clock agreement across workers being
no correctness dependency (VW4); TLS and authentication, the fabric being
trusted (`architecture.md`, Known limits); failures of `RedundNone` legs, for
which nothing automatic is specified; and a clone push to a primary whose
build is still deferred. Two things the clone drain's design asks for are out
of reach of every suite in the tree: a host IO probe of a clone's destination
namespace right after the latch, which would need a real host and a real CN,
so CLD4's resume is asserted at the etcd level only, and only by the gateway
suite; and a proof that the coordinator makes no agent call during a
drain (CLD10).

**What a pass means.** Exit status zero means every case's assertions held,
each against the state of its own cluster, in a run that stops at the first
failure with a non-zero status. A wait on an event is a bounded poll on a
condition; a fixed sleep is used only where a window must elapse — a grace
window, a rate measured over a window, a "nothing must happen for this long"
negative; every counting helper, and every read a negative rests on, fails on
a failed read instead of answering zero, because a failed read taken for zero
would pass every negative built on it.

**Cleanup.** Cleanup runs at the start of every run and at its end only on
success: a failing run leaves etcd's data, every log and every behavior file
in place and prints its diagnostics — the failing stage and its trace id,
the registrations, the ownership table, the tail of every log and the dnv
keys — so the debris is what the developer reads; a cleanup-only run does
the cleanup alone. Cleanup signals every process the suite started,
continuing a stopped worker first so that it acts on its termination, and
removes the suite's work directory; nothing outside that directory is
touched, and the suite leaves no kernel state, no packages and no users
behind.

**The driver, `workerctl`.** `workerctl` is the etcd driver that plays the
gateway: it writes through `model` as a gateway's STM would, resolves the
cluster conf and the pool `bdev_conf` it writes — because the worker refuses
a conf nobody made concrete (RW9, RW14, AR1) — and never dials an agent.
It is a front over the planting library `integtest/plant` (`layout.md`,
Directory tree), whose calls the Go cases make directly, so what this
paragraph says of a `workerctl` write holds of the library call behind it.
Where the gateway's writes carry more than the record, its own are raw, so a
case plants exactly the state it means to — save the node bookkeeping of the
copies it plants: `put-clone` charges the extents of its clone meta group,
which it places where the case says, to the DNs of the group's sides, and
`put-migr` charges its destination DN the side's extents and the
migration's units, as the gateway's creates do but never refusing a node
past its metadata area (`architecture.md`, Metadata units), so that a
later release, the clone drain's among them, finds what it gives back.
`put-migr` sets the migration's `cutover_pending` only when a case asks
for it, which a case that means to stage what `CreateMigration` writes
does. Its
`set-epoch` is raw in the
same way: it plants one `err_epoch`, on a cntlr, or on a leg or a side of a
slice or of a clone's clone meta group, and touches
neither a revision nor a `CdcEntry` nor a `settling` flag, so a case can
stage the epoch an earlier owner or another observer would have left (AR10).
The operator's calls are not raw, and all but `abort-repair`,
`force-start` and `set-operator-in-charge` bypass the library:
`create-repair`, `finish-repair`,
`cancel-repair`, `abort-repair` and `delete-retired-leg` run the model
operations the gateway's repair handlers run, `force-start` the one
the gateway's `ForceStartGroup` runs, which writes a group's forced-start
mark, or withdraws it with its cancel flag, and prints the revision the
mark carries, and `set-operator-in-charge` the one the gateway's
`UpdateGroupOperatorInCharge` runs, which sets or clears a group's
`operator_in_charge` flag and prints the pool's revision after it; all of
them run with no revision token,
`list-repairs` reads the pool's repairs and retired legs as one load does
(MD3), and `put-retired-leg` plants a retired leg as a finish leaves one,
at an age the case chooses, so that a case can stage a leg older than the
retire timer's age (AR12). `set-repair-stage` is the stage move AR11
makes, which the gateway suite plays with no worker.

**The fake agent, `fakeagent`.** The fake agent is one library,
`integtest/agentfake`, whose levers are set in two ways, so that it cannot
drift from itself: through the program `fakeagent`, which the worker and
gateway suites run and which takes its levers from a behavior file per fake,
re-read whenever it changes; and through Go calls, by which a Go test serves
a fleet of fake agents inside its own process over in-memory connections,
one listener per address and port, reached through a dial that takes the
place of the worker's own (RW7). It serves the generated `DiskNodeAgent` and
`ControllerNodeAgent` services. In `fakeagent` it serves them with the real
server interceptors, so its log records every message the worker sent under
the caller's trace id — the suite's evidence of what was sent, when, and by
which worker. The whole-worker cases serve their fleets without those
interceptors, and so without that log, each fake still reading a unary
call's trace id from the call's metadata; every fake keeps, per object, the
requests it received and the streams it served, the evidence those cases
read instead. Their wait for convergence compares etcd with the request a
fake keeps for each object, the last one it saved, and not with the last
one it received, which it records as it arrives, before it accepts, holds
or refuses it. It applies the agents' revision gate per object and
their ordering rule — a side or cntlr its node's last syncup does not list
is an unknown object, which the worker's independent roles must survive
(RW5) — refuses a bitmap push for a migration or a clone its object's last
request does not name, and derives every `*Info` row from the last accepted
request. It keeps what it accepted, in memory or in a state file, so that a
killed and restarted fake replies like a restarted agent; a Go test stops a
fake, which ends every call and stream it serves as a killed agent's end,
so that no call in flight at the stop applies anything after it, and starts
it again with what it kept or with nothing, as an agent restarts on its
local files or on empty ones. Of its levers, `hang` holds an
object's `Check*` rounds while it is set, `hang_rounds` holds only as many
of the next rounds as it counts, so that a case misses exactly the rounds
it means to whatever its own polls cost, `delay_rounds_ms` answers every
round late, and `hang_syncup` leaves the object's `Syncup*` unanswered
while it is set, as a stopped cn agent leaves its demotion (RW22), or as a
side whose agent answers its rounds but never its syncup holds a
sides-first hold to its bound (RW14), or as a primary whose rounds still
answer from before a write it has not applied keeps a forced-start mark
from its clear (AR13); a call `hang` or `hang_syncup` holds
ends as soon as its lever is cleared or its caller gives up, and a round
`hang_rounds` holds only when its caller gives up. Among the rows it
derives, a migration destination whose request carries the side
provisioned and its cutover pending reports both migration rows waiting
for the cutover, with the reason the cutover reads, and each cntlr reports
a path row per side of a slice's leg that has two sides, live once that
side is provisioned (RW18); a lever's row overrides them, its reason
included. A cntlr also reports, for each clone its request carries, a
group row and a leg row per leg of the clone's clone meta
group, as for a slice's group, the kinds of row the cn agent reports for a
group (`cnagent.md` CN28), so that a Go case can stage HL2's leg rows and
AR8 on such a group; no leg of it has a second side, so none has a path
row. A cntlr reports a leg row per new leg of every repair its request
carries, as the cn agent connects and probes a new leg as it does a leg of
the group (`architecture.md`, Repair), and, while its request makes it the primary,
every group of the request as whole, with every leg of its `leg_list`
held in sync, and no verdict on a repair; the levers `repairs`,
a verdict by a repair's name, the text of a refused take-back included,
that the fake reports under the repair's
`rep_id`, and `grps`, a group's whole flag or in-sync list by `grp_id`,
change what a primary reports, and a standby reports neither. It stamps
every reply as an agent does
(`dnagent.md` SH28): a round or an info read about an object it knows, and
a `Syncup*` it does not refuse, answers
from a new observation, made as it answers and under the call's trace id —
the round's own for a round — so that, unless a lever says otherwise, no
round answers from an observation that is late, too old or already seen; a
round or an info read that refuses its object as unknown carries no
observation; a `Syncup*` it refuses — a stale revision, an object its
node's last syncup does not list, or a refusing code a lever forces —
carries no stamp, as an agent's refusal carries none (`architecture.md`,
Live-state reporting); and a `Syncup*` that saved its request carries the
save time, with its observation after it, while one whose code a lever
forces saves nothing, so a forced `ReplyCodeLeftover` carries an
observation and no save time.

Further levers, each set on one object, shape what a round, an info read
or a `Syncup*` answers. Two of them shape the observation a round or an
info read answers from, the one HL7 judges by its age:
`obs_age_ms` answers from an observation that ended a given time before the
answer, or, while the last observation the fake gave is newer, repeats that
one whole, its rows, code and revision with its time, as an agent whose
reads stall answers; and `obs_none` answers as an agent that holds the
object and no observation of it yet, with code zero, revision zero, no
`*Info` and no observation time (`dnagent.md` SH28). A `Syncup*` still
answers from its converge's new observation under either.
`syncup_revision` makes an accepted `Syncup*` reply carry another revision
than the request's, and the fake stores the request as it would without
the lever. An agent answers an accepted request with that request's
revision (`architecture.md`, Live-state reporting). What the lever plays is
a reply read against a plan newer than the request, one the pool
coordinator handed the child while the call was in flight (RW14): a
cntlr's loop reads the reply against that plan itself (HL2), and the
sides-first hold and the demotion hold read the revision a side's or a
demoted cntlr's loop reports applied (RW14, RW22). A disk node's or a
controller node's loop compares only a round's revision with the desired
one (RW4 step 5), so there the lever changes
nothing but the revision a dn loop keeps with the `DnInfo` a `SyncupDn`
reply carried (RW2), which holds the count compare off (RW13) until a
round's reply carries a `DnInfo` again. A value above the request's
revision plays nothing an agent produces: on a side or a cntlr it can
release the sides-first hold (RW14) or the demotion hold (RW22) early.
`leftover_rounds` is a leftover that clears: the fake
applies and saves the request, unlike under a forced code, answers
`ReplyCodeLeftover` (RW5) on the syncups and info reads meanwhile and on as
many rounds as it counts, and then answers code zero. `disk_counts` gives
the dn disk counts, sent only beside an OK meta row, as the dn agent sends
them (`dnagent.md` DN18). The fake computes no sweep verdict and no disk
counts of its own: a leftover and the counts come only from those levers,
which no suite case sets, so no suite case runs RW13's count compare.

## Known limits

* **A partitioned observer is tolerated, not repaired.** An observer whose
  watch of a role is broken while its puts succeed is caught by VW8 (b) only
  if that watch stops echoing its own puts too; a watch that delivers its
  own echoes but drops a peer's is not distinguishable from that peer
  dying. The wrong commit deletes the peer's key; the peer sees it
  (VW8 (c)), fences and rejoins as a fresh
  identity; in between, both drive the same shards — bounded by the grace
  window plus one round, harmless to agents (idempotent syncups), but not
  prevented. In etcd every reaction is STM-guarded, but a health
  epoch is a reaction input, and one the other driver left stale is
  corrected by the owner's first verdict after it next loads the record
  (HL3): within a pass and a round for a side, a leg or a cntlr, no
  threshold reaction firing on it before the owner's own verdict reads the
  object unhealthy (AR10), and within a `nodeRecordMaxAge` and a round for a
  DN or a CN, a stamped node being out of allocation meanwhile (MD4).
* **A watch that etcd stops serving is found late.** A shard's revision
  watch that etcd stops serving while the registry watch of its role goes
  on is not caught by VW8 (b). No fence restarts the `ClusterConf` watch
  (RW21), not even one VW8 (b) made for the fault that stopped its events
  (EU3). Such a watch finds the fault itself only after
  `DefaultEtcdWatchSilence` (EU3), unless a restart, or for a revision
  watch a fence or a shard release, ends it first, and until then what it
  follows stays frozen on that worker, with no record. A watch that goes on
  hearing progress notifications while it loses events is not found at
  all.
* **Overlap and gap at every ownership change** (VW7): a few seconds of double
  driving or of no driving per shard per change. Accepted.
* **Undriven windows**: a fresh worker's first grace window; twice the vote
  interval plus the grace time after a crash; the grace time after a graceful
  stop or after a fence — both of which delete their registrations without
  waiting for their drain (CM5, VW8), so the window does not wait out the
  drain's in-flight calls. Revision keys are durable, so nothing is lost:
  convergence is delayed, not skipped.
* **After a pause, the first verdict waits a round.** Only rounds start
  background reads (`dnagent.md` SH29), so while no round drives an
  object — an undriven window, the gap of an ownership change (VW7), an
  idle (RW9) — its observation ages, unless a converge of the agent's own
  stores one: a background retry attempt, the dn's fence timer or a
  startup reconcile (`dnagent.md` SH28). The first round after the pause
  answers from that observation and gives no verdict on it (HL7): it
  re-syncs when its reply says so, but sets and clears no health epoch,
  counts in no run and reports nothing to the sp coordinator. Unless a
  round misses, the object's first verdict, flip and report to a hold come
  from a `Syncup*` reply that round brings or from the first round that
  answers from an observation made after it, normally the next, which is
  also the first round to count in a run. So a stored epoch that the
  object has outgrown stays until that verdict, and at the default
  intervals a new owner's first sides-first hold runs to its timer when
  the sides' agents already hold its revision (RW14). The pause alone turns
  rounds into missed ones only when no observation of the object ends
  within HL7's bound after that first round; a gap between two rounds of a
  running loop stays judged by the observation's own age (HL7).
* **A node clock that steps back hides an observation's age for up to the
  step.** The agent keeps each object's observation times strictly
  increasing (`dnagent.md` SH28), and the worker reads an observation's
  age, and the first round after a pause that it counts that age from, by
  the node's clock (HL7). After the node's clock steps back, an
  observation stored before the step reads younger by up to the step, and
  every one the agent stores after it carries a time ahead of that clock
  until the clock catches up, so its age reads zero meanwhile: a node
  whose reads hang in that time is taken for unreachable up to the step
  later. A cntlr child restarted at the same endpoint just after such a
  step can also count again a look its predecessor counted, since an
  observation made before the step can read as ending after the new
  loop's first round (RW23).
* **The grown-chunk memo of BM5** (`architecture.md`, [D8]) is per worker; a
  handoff can leave a shorter chunk at the agent (cost: extra copying, never
  correctness).
* **A slice whose data list holds `MaxGrpCntPerSlice` groups never grows its
  data again** (AR6): every pass that finds the pool's data usage above
  `low_water_mark_pct`, with no data grow pending, logs `reaction skipped`
  (`grp_list_full`) and goes on (AR2). Nothing frees the list, and the pool
  can run out of data space (`architecture.md`, Automatic reactions;
  `architecture.md`, Known limits).
* **An operator's open repair keeps the worker from repairing its group.**
  The worker drives no
  operator's repair (AR9), and a group has one repair at a time, so a group
  with an operator's repair gets no repair of the worker's until the
  operator finishes or cancels it, whatever its legs do; a pass that finds
  a leg of the group due for repair, past the gate on the group's row and
  the healthy-leg test, logs the wait (`grp_has_repair`, AR8). An
  operator's repair whose bad leg serves with an `OK` leg row waits in
  CREATED until the operator aborts it (`architecture.md`, Known limits).
  The wait holds that group only; the scan repairs the others (AR2). The
  group's `operator_in_charge` flag holds the worker's create the same
  way, without a repair of the operator's and without a level raise: while
  it stands the worker creates no repair of the group, whatever its legs do,
  and a pass that finds a leg of the group due for repair, past the
  conditions AR8 checks before the flag, logs the hold
  (`grp_operator_in_charge`, AR8); the worker never writes the flag (MD6;
  `architecture.md`, Repairs).
* **A new leg the primary never reports keeps its repair CREATED.** AR8's
  try-to-abort reads the new leg's health epoch and its side's, and the
  worker stamps the leg's only from the primary's leg row of it (HL2), so a
  new leg whose side is provisioned and healthy while the primary reports
  nothing of it, as when the primary never applies the syncup that carries
  the repair, never asks for an abort. The repair stays CREATED, and keeps
  the worker from creating another repair for its group, until the primary
  reports the new leg holding a place in the array or confirms an abort. An abort, whether the operator asks for
  it or one of AR8's other triggers does, ends the wait only once the
  primary applies a syncup that carries the abort flag and confirms the
  abort, since only the primary's verdict moves the stage
  (`architecture.md`, Repair). The wait holds that group only (AR2).
* **A DN that stays healthy but cannot serve a new leg gets a new
  worker's repair after each cancel.** The create's black list holds the
  DNs of the group's own legs alone (AR8), and a side's health epoch takes
  no capacity key from its DN (MD4), so after the try-to-abort, the
  primary's confirmation and the cancel, the next create's scan can offer
  the same DN again, the only offer when it is the only candidate. A DN
  that cannot zero a new leg's side, which then reads `ERROR`, or that the
  primary cannot connect to, thus gets a new worker's repair about once
  per `side_unhealthy` or `leg_unhealthy`, each costing a zeroing and the
  `SpRev` bumps of its steps, until an operator disables the DN, which
  takes it out of allocation, or the DN recovers. The worker remembers no
  DN a repair failed on: a pass carries from earlier passes only what AR1
  names, and a remembered failure would outlive the fault it records. The
  cycle holds that group only (AR2).
* **A group whose array does not run, or that has no healthy leg, is never
  repaired on its own.** AR8's gate creates no repair for a group whose
  row does not read `OK`, as a stopped or broken array's does not
  (`cnagent.md` CN28), and leg repair leaves alone a group none of whose
  other `leg_list` legs is free of an `err_epoch` and held in sync by the
  primary's latest report (AR8), which a group whose array does not run
  never has, since the primary reports nothing of such a group's array.
  Such a group keeps its legs until enough of them come back, for the
  primary to start its array or for a leg to read healthy, or until an
  operator acts. A running array whose row reads `ERROR`, as while the
  in-sync guard holds over it (`cnagent.md` CN12), gets no repair either
  until its row reads `OK` again.
* **A group whose only in-sync leg dies while md copies onto a finished
  repair's new leg waits for an operator's forced start.** A finish runs as
  soon as the primary reports the new leg holding a place (AR8), and md may
  still be copying onto it; the group's latest data is then on the leg that
  died, the new leg holds part of it, and the bad leg the finish retired
  holds an older copy. md will not start the array from the half-copied new
  leg, and the group has no healthy leg, so no repair follows (AR8): the
  group stays down until its legs return in a way md accepts (`cnagent.md`
  CN12 and its Known limits) or an operator force-starts it
  (`architecture.md`, Repair) — from the leg that died, which holds the
  latest data, the mark written while that leg is still dead so that the
  start runs once it is back (`architecture.md`, Known limits), or, while
  it stays dead, from the retired leg taken back by a repair, which brings
  back the older copy. The worker's part is the clear of the mark once the
  primary reports the group's row `OK` with a member in sync on a leg whose
  own row reads `OK` (AR13), which a report from md's retries of the copy
  can show with no start only while the dead member md holds in sync still
  has a leg row that reads `OK`, before the primary's probe finds it dead
  (`cnagent.md`, Known limits), and leg repair's hold on the group while
  the mark stands (AR8); it never writes a mark of its own.
* **The gate can read a row that is about to turn.** A leg that dies
  under a running array while it is the array's only in-sync member on a
  live leg, as when the group's other leg is the one due for repair,
  leaves the array running, and its row `OK`, until md sees the death,
  which breaks the array, and nothing in the row says that it will. A
  pass in that window can create a repair for another leg of that
  group, due for repair: the gate reads the row `OK`, and the
  healthy-leg test passes while the dying leg's `err_epoch` is not yet
  set (AR8), so the array then has no member to copy the new leg from. The
  primary adds the new leg while the dying leg still reads `OK`
  (`cnagent.md` CN12), the copy reads the dying leg, and md breaks the
  array when it sees the death. The group then stays down until its legs
  return in a way md accepts, or an operator force-starts it
  (`architecture.md`, Repair), whatever stage the repair has reached: a
  repair still CREATED stays so while the array does not run, since the
  primary gives no verdict on such an array (`cnagent.md` CN28), and one
  that reached STARTED is finished where reactions run (AR3, AR8), which
  leaves the group as the item above says. What the cn agent does meanwhile is `cnagent.md` CN12 and
  its Known limits. The window ends at whichever comes first: a report
  whose group row reads the array broken (`cnagent.md` CN28), or the dying
  leg's `err_epoch`, which is set once a report of the primary, from a round or from a
  syncup reply, reads that leg's row `ERROR` (HL1, HL2), normally after
  the leg's next health probe fails or stalls (`cnagent.md` CN11). On an
  array with IO, md breaks the array at the end of the path's failfast
  window (`dnagent.md` SH20, `DefaultNvmeFastIoFailTmo`). On an idle
  array md does not see the death and the row stays `OK`, so the window
  lasts until the `err_epoch` is set, which is longer than the failfast
  window. Legs that die together reach a repair threshold only long
  after the window, since the shortest, `side_unhealthy`
  (`DefaultSideUnhealthy` when the SP sets none), is far longer than it
  at the defaults, but no validation rule keeps a threshold above it,
  and a leg that dies within that window before a pass that finds
  another leg of its group due meets it at any threshold.
* **The in-sync half of the healthy-leg test reads md, which learns of a
  death only from IO.** A leg counts as its group's healthy leg only while
  the current primary's latest report holds it in sync and it has no
  `err_epoch` (AR8). md holds a member in sync once its copy has ended and
  until md sees it die, which on an idle array it does not, so a leg whose
  member dies with no IO goes on counting by the report until its probe
  fails and HL2 stamps its `err_epoch`, the window that "The gate can read
  a row that is about to turn", above, describes. A half-copied leg —
  a member md has failed out and put back, or a repair's new leg a finish
  put in place while md copies — counts only from the first report that
  holds it in sync, so leg repair does not repair the only leg md holds in
  sync while such a copy runs, save on a report that predates the copy
  (Reaction inputs are one pass old, below).
* **The retire timer, or a give-back at the retired-leg bound, deletes a
  retired leg that holds newer data than a restored copy.** After a forced
  start from an older copy, a retired leg taken back by a repair
  (`architecture.md`, Repair), the leg that held the newer data is a retired
  leg once that repair is finished, and the timer deletes it as it deletes
  any other, once it is `RetiredLegMaxAge` old on a whole group whose legs
  are healthy (AR12); the finish of a worker's repair of the group gives it
  back sooner, at any age, once it is the group's oldest retired leg and the
  group is at the bound (AR8; `architecture.md`, Repairs, FinishRepair). No
  setting keeps it, so its data is lost then unless an operator has brought
  it back before.
* **A primary that never settles is failed over or replaced no earlier than
  `cntlr_unhealthy`**, unless it is disabled (AR5, HL2) — nor failed over
  before `primary_unhealthy`, AR5 taking the longer of the two. A primary that
  never reports its stack built and clean as primary — a new primary that is
  itself dead, a build that keeps it unhealthy longer than that, a new SP
  whose first zeroing never finishes, which keeps its pools `PROVISIONING`, a
  migration destination whose record carries no `cutover_pending`
  (`architecture.md`, Upgrade in lockstep), which `FinishMigration` forced
  before its RW18 flip and
  that never finishes zeroing while it is the only side of a leg in the first
  group of its list, which keeps that slice's pools and every td
  `PROVISIONING` the same way, a clone whose source stays unconnected, or
  keeps an endpoint with no live controller or no namespace, which keeps
  its target row `MISSING` (HL2), or a clone whose clone meta group no
  cntlr can assemble, which keeps its dm-clone row `MISSING`
  (`cnagent.md` CN18; `architecture.md`, Known limits) — keeps the SP on
  it that long once it is unhealthy, where a settled primary is judged by
  `primary_unhealthy` alone; an old primary that recovered meanwhile gets the
  role back only then, and not while the new primary fails only on rows it
  failed on (AR5's same error).
* **Two missed rounds in a row fail over a primary that is only busy.** A
  failover on its threshold needs two unhealthy verdicts in a row from the
  primary's rounds (AR10), so one missed or slow round, or one whose
  observation is too old, followed by a clean answer fails no primary
  over, however late that answer comes, and neither does a `Syncup*` reply
  that lists a converge still waiting for a member, though a round that
  answers from a retry attempt of that converge can add to the run (next
  item). A round waits for no lock of the cntlr, since the agent answers it
  from its latest observation (`dnagent.md` SH25), but no read of the cntlr
  starts while a converge of it, or a syncup of its node that marks the
  node, is in flight (`dnagent.md` SH29, SH30). A converge that holds the cntlr's reads off
  long enough for the cntlr's latest observation to grow older than HL7's
  bound, or a read that takes that long on a loaded node, makes its
  rounds answer from an observation too old, and the worker judges no
  reachability: two such rounds in a row are two missed rounds all the
  same, and fail over a primary that still serves its hosts once its
  threshold has run out.
* **A round that answers from a retry attempt counts the attempt's rows.**
  The cn agent's connect retry re-runs the cntlr's whole converge while a
  member stays late (`cnagent.md` CN10, CN12), and each attempt stores its
  observation with that converge's rows, in which the groups that wait
  read `ERROR`, as they do in the `Syncup*` reply AR10 leaves out of the
  run. No reply has carried that observation to a child of the cntlr
  before, so the first round that answers from it answers from a new one,
  and its `ERROR` rows add to the primary's failover run (AR10, RW23). Two
  counted rounds in a row that each answer from an attempt fill the run
  with no round missed, as when an attempt outlasts
  `CnConnectRetryInterval`, the next follows at once and no read fits
  between them (`cnagent.md`, Known limits). Once the primary's threshold
  has run out, that fails over a primary that only waits for a member,
  though the next primary would wait the same.
* **While a leftover keeps every round re-sending, a round's own report
  counts in a run only when a background converge made it.** Each such
  round answers from the object's latest observation: the one the
  `Syncup*` before it stored, unless a background converge of the object
  stored a later one between that reply and the round. The loop saw the
  `Syncup*`'s observation on that reply, so a round that answers from it
  answers from no new one (RW23), and the `Syncup*` the round issues
  cancels the read the round starts (`dnagent.md` SH30), so no round
  answers from a read. A cntlr's background converge is an attempt of the
  cn agent's connect retry (`cnagent.md` CN10, CN18), which runs for a
  cause of its own beside the re-sends. An
  attempt's observation reaches the loop first in a round's reply, so the
  first round that answers from it answers from a new one, and its report
  counts in the cntlr's run (AR10; previous item). A primary's failover
  on its threshold therefore waits until the leftover clears unless its
  rounds miss, answer from an observation too old or answer from such
  attempts (AR10). The dn agent's background converges, its fence timer
  and its migration retry, each converge a side (`dnagent.md` DN12,
  DN13), so a DN's count compare (RW13) waits until the leftover clears. A `Syncup*` reply's observation
  counts for nothing because a converge's rows can read unhealthy while it
  waits for a member (AR10); the case needs a leftover that lasts and an
  unhealthy primary at once. A leftover of a disk node or a controller
  node keeps no read of its sides or cntlrs from ending (`dnagent.md`
  SH30).
* **After a restart or a handoff, a leg its primary reads missing waits
  for a converge.** A primary's background read reads a leg whose
  device-mapper wrapper is absent — one it had to build while the leg's
  disk node was down, or after its CN rebooted — `MISSING` with no details,
  which neither sets nor clears (HL2); only its converge's observation
  reports such a leg `ERROR`. A coordinator that starts after the one that
  stamped the leg's `err_epoch` therefore has no verdict on the leg, and AR8
  waits (AR10) until something makes the primary converge, a revision bump
  of the SP say, and a reply carries that converge's report.
* **The hold delays the cntlr requests, and at a failover the sides too**
  (RW14, RW22): a side the hold waits for that does not report — one on a
  disk node that died too recently for its epoch to be in the fan-out's
  load, or during the hold, a new side its disk node refuses until the dn
  role's `SyncupDn` has introduced it (`dnagent.md` DN8), a side the
  fan-out restarted whose old child is still inside a `Syncup*` (RW1), or
  a side whose child a new owner's or a restarted worker's first fan-out
  starts, or a fan-out restarts, and whose first round brings no
  `Syncup*`, its agent already holding the revision, so that it reports
  from its second round at the earliest (RW14, HL7) —
  delays every cntlr request of its SP's fan-out but a demotion by up to
  one `cntlr_interval`, which RW9 lets be as long as
  `MaxHealthCheckInterval`, a failover's promotion included; and a failover
  whose old primary does not report its demotion applied holds its sides
  and its promotion for the whole `DemotionHoldTimeout` first. A side whose
  `err_epoch` the fan-out's load holds is not waited for, so a dead disk
  node delays its pool's fan-outs only until its sides' epochs are stamped
  and loaded, and the side of a repair's bad leg there delays none (AR8);
  a side so found that
  answers after all is not waited for either, and the races the hold
  narrows stay open for it (RW14). Nor does `sp sides unsynced` count such
  a side, so an alert on that record does not see a dead disk node's sides
  once their epochs are stamped and loaded: their own `err_epoch` reports
  them (HL2).
  A failed-over primary is fenced before it has applied its demotion
  only when it does not apply it within that wait, or when a coordinator
  fans its failover out with no running child of it last handed a primary
  plan (below). Hosts that follow the discovery log have dropped the path
  of one that cannot be reached by the end of the whole wait (RW22), a
  stale discovery view aside (`architecture.md`, Known limits); one whose
  agent applies its demotion late — a converge that waits for the cntlr's
  locks behind a slow earlier one (`cnagent.md` CN1) — answers host IO on
  its still-optimized path with errors from the fence until it has applied
  it (`architecture.md`, Known limits). The leg-removal
  race between a CN's disconnect and the disk node's unlink of the side's
  export is narrowed, not closed, because the unlink rides the dn role's
  `SyncupDn`, which the hold does not wait for (`cnagent.md`, Known limits);
  the sp drain's twin of that race, which no hold orders because the drain
  waits on no agent (SPD13), is kept by decision; and a migration
  destination's `SyncupSide` at `FinishMigration`, whose disconnect of the
  source connection the dn sweep runs under the side's lock, races the
  source disk node's `SyncupDn`, which no hold orders either and which takes
  that export away (`dnagent.md`, Known limits).
* **A failover fanned out with no child that was handed the primary plan
  can be fenced first.** RW22 finds a demotion only through the old
  primary's running child last handed a primary plan, and nothing of the
  demotion hold is persisted. A coordinator that has just started, after a restart
  or a handoff, has no cntlr child until its first hold starts them, so a
  failover that reaches it before then runs in RW14's order: one the other
  owner of an overlap decided (VW7), or one its own pass decided on a
  disabled primary (AR5). And a coordinator whose first fan-out already
  carries the failover, as when the failover's owner lost the shard before
  its demotion hold ended, never hands the old primary the primary plan at all, so
  that fan-out runs in RW14's order too. Its sides can then fence the old
  primary before that primary has applied its demotion: RW14's fence-first
  window, for any host still on the old primary's path, which a host that
  follows the discovery log may drop only after the fence
  (`architecture.md`, Known limits).
* **A lost thin id costs a failover or a replacement, and the second refusals
  that bound them are a memory.** A report marks a created td's lost thin
  id, with its thin row's reason, only when it comes from a converge's
  observation; a background read's probe reads the absent volume `MISSING`
  with no reason (HL2), so a primary whose latest report is a read's is
  failed over, and the second refusal then keeps the role from
  going back to it for as long as the new primary fails only on rows it failed
  on (AR5) — in an SP of two cntlrs, one failover in all, and one more each
  time a new primary also fails a row of its own; in a larger one the role can
  first move on to a cntlr that has not held it. That refusal rests on the
  coordinator's record of the last failover it applied: a coordinator after a
  restart or a shard handoff, or the other owner of an overlap (VW7), has
  none, and once its own verdicts read the primary unhealthy for two of its
  counted rounds in a row (AR10) it can fail over once more before it has.
  It compares rows, not causes: a new primary whose own fault fails only
  rows the old primary failed on is held as well, for as long as that fault
  lasts or until an operator
  moves the role, every pass logging why. AR7's sole-primary variant takes the
  twins of both refusals, judged on the same report (AR7). The first holds the
  primary of an SP with no failover candidate only while the report the
  coordinator holds for it is a converge's that carries the reason, which
  lasts until a round answers from a read's observation, whose info differs
  from the converge's and so travels in the reply (HL5; `dnagent.md` SH26),
  or a dead stream marks its rows `UNKNOWN` (HL1): a round starts a read
  when no read or converge of the cntlr and no syncup of its node that
  marks the node is in flight (`dnagent.md` SH29), and a later round
  answers from it once it has
  ended; a new child after a worker restart or a shard handoff holds no
  report until a reply carries one. It does not break the replacement loop
  on its own: the report held for a replacement is a read's, which carries
  no reason, from the first round that answers from a read after its
  converge. The second does: once a primary whose held report is a read's
  has been replaced at `cntlr_unhealthy`, by a cntlr that reads the same
  rows, that replacement is
  not replaced while it fails only on rows the primary it replaced failed on,
  from an error set within `cntlr_unhealthy` of that replacement — one
  replacement, rather than one per `cntlr_unhealthy` for as long as the td
  stays lost, each moving the whole stack of an SP with one cntlr to another
  CN; one more each time a replacement also fails a row of its own. That
  refusal rests on the coordinator's record of the last replacement of a
  primary it applied, as AR5's second rests on its record of the last
  failover: a coordinator after a restart or a shard handoff, or the other
  owner of an overlap, has none, and once its own verdict reads the
  replacement unhealthy it can replace it once more before it has. Neither
  record holds anything when the reaction it records was taken over a
  report with no `ERROR` row — a primary read unreachable, whose rows a dead
  stream has marked `UNKNOWN` (HL1), or whose child, new after a restart or
  a handoff, has had no reply at all, its verdict being a missed round
  (AR10) — so the reaction after such a one is not held either: a
  replacement read unreachable costs two replacements more, a restart or a
  handoff can cost two where it costs one, and after a failover over such a
  report the role can be handed back once more. So the replacements a lost
  thin id costs are bounded to one per coordinator that drives the SP, those
  exceptions aside. It compares rows, not causes: a replacement whose own
  fault fails only rows the old primary failed on is held as well, for as long
  as that fault lasts, every pass logging why.
* **A dm-clone left with hydration off moves the role of the clone's
  pool.** Its row reads `ERROR` in every observation, a background read's
  as much as a converge's (`cnagent.md` CN18, CN28). That is a row of the
  primary's own (HL2), so its `err_epoch` stays set, and AR5 fails it over at its
  threshold, or AR7 replaces it where there is no failover candidate; the
  new primary creates the dm-clone over the metadata its clone meta group
  holds and finishes the build (`cnagent.md` CN18). A dm-clone whose
  metadata is read-only or failed reads `ERROR` and moves the role the
  same way. A fault of the clone's source does not itself move the role
  (`cnagent.md` CN18): a source with no live controller at an endpoint, or
  with no namespace, reads `MISSING`, which sets no `err_epoch` (HL2), so
  neither AR5 nor AR7 acts on that row, and a fault in the primary's own
  path to the source reads the same; the operator can move the role by
  disabling the primary cntlr (AR5). The cn agent's retry of a source it
  cannot connect, or that has no namespace, holds the cntlr's reads off
  while each attempt is in flight (`dnagent.md` SH30; `cnagent.md`, Known
  limits), so an attempt that outlasts HL7's bound makes the primary's
  rounds answer from an observation too old, and such rounds can fail it
  over, as they can a primary that is only busy (above). A role that moves while a fault at the source
  lasts can take even a clone that has hydrated in full out of service: a
  new primary that cannot connect the
  source, or finds no source namespace, cannot build the clone's dm-clone
  while the fault lasts, so the clone's destination namespace waits
  inaccessible, its hosts' IO queued, until the source comes back or the
  clone is deleted with `force` (`cnagent.md` CN16).
  What else those rows cost on the cn agent is `cnagent.md`, Known limits.
* **A clone's drain can give its clone meta group back before the primary
  removes the dm-clone.** CLD9's transaction waits for no agent (CLD10),
  so a DN can tear a side of the group down while the primary's dm-clone
  still maps the group, until the primary's next converge removes the
  dm-clone (`cnagent.md` CN18, CN21). A clone whose copy is whole writes
  no metadata, so it loses nothing then, and a delete without `force` is
  refused until the copy is whole (`architecture.md`, Clones). A forced
  delete of a clone still copying can see the dm-clone's metadata writes
  fail, and a host write to a region not copied yet fail with them, until
  that converge; a forced delete already leaves that destination's data
  incomplete.
* **`RedundNone` legs have no automatic repair**: no repair can exist, since
  `CreateRepair` refuses a group that is not md-raid1, and the
  migration that could have moved a readable-but-sick side is an operator's
  tool (`CreateMigration`), not a reaction (Automatic reactions). A leg of
  a clone meta group in such a pool has neither, since `CreateMigration`
  refuses its side (`architecture.md`, Clones).
* **Reaction inputs are one pass old**: `err_epoch`s are read fresh each pass,
  but pool usage, group rows, the groups' whole flags and
  in-sync lists and the verdicts on repairs come from the primary's
  last Check reply, at most one interval old, which answers from an
  observation HL7 lets be up to its bound older. A reaction is late by at
  most that much; never wrong, because the op re-validates, save AR8's gate
  on the create, which no op can re-validate, since no STM sees a report
  (AR8): a row that turned since that observation is read as it was,
  though the gate never reads an observation older than the snapshot's
  revision (AR8). AR8's healthy-leg test and AR12's
  retire timer read the groups' whole flags and in-sync lists, and the
  timer the group rows, the same way, though the timer reads the row and
  the whole flag only from an observation at the snapshot's revision or
  later, as AR8's gate does (AR12), so a leg that left md's in-sync
  set, or a group that turned degraded, since that observation is read as
  it was. AR11's stage moves read the verdicts the same way,
  and `SetRepairStage` re-checks the stage, the `rep_id` and the flag a
  verdict answers (MD6). A promotion adds no further case: from the moment RW14's release
  hands a failover's promotion to the new primary's child until that
  primary's reply to it, its last reply is still its standby's report,
  whose leg rows are transport liveness and ANA state, not the
  health-block probe, but that report holds no group row and no leg in
  sync, so AR8's gate holds the create for that window, the
  promotion's own round trip (AR1); and
  the pass reads no info at all while the hold keeps the promotion from
  the child (AR1). Nor does it hold for an
  `err_epoch` another driver stamped: AR10 holds a reaction until this
  coordinator's own verdict reads the object unhealthy, and a failover on
  its threshold until its verdicts on two of the primary's counted rounds
  in a row do, but the clock then runs from that stamp, which is the very record the op re-validates against
  (the partitioned-observer limit above, HL3). Nor is HL7's bound kept just
  after a pause in the primary's rounds: the first round after it answers
  from an observation of any age, to which HL7 gives no verdict but which
  becomes the latest known info (HL5), so until a later round answers from
  an observation made after that round, normally one interval later, the
  pass reads pool usage and group rows from that older
  observation, and AR6, which waits for no verdict, can act on that usage.
* **A disk node's own free count is compared, never reconciled** (RW13): a
  disagreement that two compares in a row see is logged, once until a
  compare agrees, and nothing corrects the record or the disk's volume
  table; an operator sets `dnvctl dn inspect`, the agent's counts, beside
  `dnvctl dn get`, the record's. A disk node whose side records never come
  to name exactly its side pointer list — a side its sweep cannot remove,
  or a listed side the disk never allocates — is never compared, and no
  record says that the compare is not running.
* **Log volume**: every round logs a client send and receive record pair per
  object (`grpc.md`, L6) and every heartbeat an `etcd put`; with thousands of
  objects this is the dominant log stream of the control plane, as it is for
  the agents. A DN or CN that neither syncs nor changes health adds HL3's
  re-read of its record per `nodeRecordMaxAge` — an `etcd get`, and for a DN
  a second one of its `DnRev` (RW13) — and that re-read again at each
  verdict while it fails. The worker's own per-object records are
  mostly event-driven: health transitions (`health changed`, the `unreachable`
  reason included — the `RES_STATUS_UNKNOWN` marking itself is in-memory and
  logs nothing, HL1), a `syncup result` per `Syncup*`, and a cntlr's
  `cntlr settled`, about once per acquisition of the primary role. Among those
  that recur every round for as long as their cause lasts: an unreachable
  object's `check stream open failed`, `check stream send failed` or
  `check round failed` (the revision loop's own records, which the Log records
  above do not list and no suite parses, though `integtest/round_counts.sh`,
  which no suite calls, counts them from a run's copied logs); the
  `syncup result`
  of each `Syncup*` that RW4 step 5 re-issues, paired with a `syncup leftover`
  or a `syncup rejected` while its reply still carries the leftover or the
  rejection (RW5; a leftover only a background read's verdict still names
  is paired with neither) — a stale revision after an etcd restore repeats its
  `syncup rejected` at Error every round until the revision in etcd reaches
  the agent's; a `dn conf missing` or `cn conf missing` while the node's
  `DnConf` or `CnConf` is absent (RW13); a `late observation` while an
  object's observations stay older than an interval (HL7); and a
  `health write failed` while a health write keeps failing (RW12). The sp
  coordinator's pass adds its own,
  once per pass rather than per object — AR5's `reaction skipped` with
  `no candidate`, say, for as long as a primary due for failover has no
  eligible candidate, or `shared_state` or `same_error` for as long as the
  error that holds one lasts, and AR7's `shared_state` or `same_error` once
  the primary it holds is due for replacement — beside AR5's, or alone while a
  `primary_unhealthy` longer than `cntlr_unhealthy` has not yet run out.
