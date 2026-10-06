# dnv-worker.md — `dnv-worker`: vote, shard and revision workers

This document owns the `dnv-worker` binary and the two libraries it is the
first consumer of: package `etcdutil`, the one door to etcd (EU1 to EU7);
package `model`, the etcd data model as Go together with the internal
mutations the gateway's handlers share (MD1 to MD9); the `cmd/dnv-worker`
command (CM1 to CM7); the vote worker (VW1 to VW11); the shard worker (SW1 to
SW6); the revision worker with its dn, cn and sp role files (RW1 to RW21);
health bookkeeping (HL1 to HL6); bitmap pushes (BM1 to BM6); the automatic
reactions (AR1 to AR9) with the sp drain (SPD1 to SPD14) and the clone drain
(CLD1 to CLD12); the worker's log records; and the intent of the worker
integration suite. It leans on `architecture.md` for the object model, the
etcd data model, allocation, common validation, the agent contracts, the
cross-component procedures (failover, migration, teardown by sweep, the
levels) and the decisions [D8], [D10], [D15], [D16] and [D17]; on `log.md`
and `grpc.md` for the logging, trace-id and interceptor rules; on
`dnagent.md` and `cnagent.md` for the agent side of every RPC the worker
issues; on `gateway.md` for the public RPCs whose transaction bodies `model`
carries; and on `layout.md` for package placement and the dependency rules.

## Scope and placement

`dnv-worker` is the control-plane process that converges the data plane to
the desired state in etcd (`architecture.md`, System overview): it watches
the revision keys, shards the work by shard code, drives agents through the
unary `SyncupDn`/`SyncupSide`/`SyncupCn`/`SyncupCntlr` and `Push*Bitmap`
calls, watches them through the `Check*` streams, maintains the health fields
and capacity keys, flips `Side.provisioned` and `ThinDevice.created`, and
performs the automatic reactions. It never serves gRPC, never calls
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

Packages and files (`layout.md`, Directory tree, and `layout.md`, Dependency
rules):

| package | files | may import (internal) |
|---|---|---|
| `etcdutil` | `etcdutil.go` | `common` — takes `proto.Message`, never imports `pb` |
| `model` | `keys.go`, `stm.go`, `capacity.go`, `alloc.go`, `ops.go`, `drain.go` (the sp drain's ops of MD6), `clonedrain.go` (the clone drain's ops) | `common`, `pb`, `etcdutil` |
| `worker` | `worker.go` (`Run`, `Config` and the worker-lifecycle record names; the flip, bitmap and reaction records live beside their emitters in `sprole.go`, `bmpush.go` and `reaction.go`), `vote.go`, `shard.go`, `revision.go`, `conn.go` (the RW7 connection cache), `dnrole.go`, `cnrole.go`, `sprole.go`, `clusterconf.go`, `health.go`, `bmpush.go`, `reaction.go`, `drain.go` (the sp drain's worker half), `clonedrain.go` (the clone drain's) | `common`, `pb`, `etcdutil`, `model` |
| `cmd/dnv-worker` | `main.go` | `worker`, `common`, `etcdutil` (CM4: main builds the client `worker.Run` takes), plus cobra and viper |

Out of scope here: the gateway (request validation, the public RPCs, the
`Inspect*`/`Get*Size`/`Get*Bm` agent calls), `dnv-cdc`, `dnvctl`. Where the
worker performs an "internal" variant of a gateway mutation (Automatic
reactions), the STM body lives in `model` and the gateway adds only
validation and reply mapping on top (MD8).

## Constants and schema

### Additions to `common/constants.go`

The worker's constants are one region of the constant block in
`common/constants.go`, which is authoritative for their comments: `DefaultVoteWorkerInterval`, the seconds between two refreshes of a
worker's registry key (VW2), a registration not refreshed for twice that long
being dead (VW3); `DefaultVoteWorkerGraceTime`, the seconds an observed
membership transition must hold before it is committed into the effective
membership (VW5); `DefaultWorkerSyncupTimeout` and `DefaultWorkerPushTimeout`,
the per-call deadlines of the worker's agent RPCs (RW5, BM3);
`DefaultEtcdDialTimeout` and `DefaultEtcdOpTimeout`, the etcd client's dial
deadline and its budget per plain operation and per whole transaction, every
retry included (EU1, EU5); `EtcdMaxTxnOps`, the `--max-txn-ops` requirement on
every etcd serving dnv, a constant the block holds for the gateway
(`gateway.md`, Additions to `common/constants.go`) because the transaction it
is sized by is `CreateStoragePool` at its widest shape, while the sp drain's
slice batch (SPD13, SPD14) and the created flip's transaction (RW19) are the
other bounded transactions it has to cover, and the clone drain's batch
(CLD11) is checked against it but fits etcd's default cap;
`MaxFlipCreatedPerTxn`, the most candidates one created-flip transaction
carries (RW19); and the three role names `WorkerRoleDn`, `WorkerRoleCn` and
`WorkerRoleSp`.

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

### `pb/schema.proto` — one added message, one added field

Two members of the schema are this document's. `WorkerReg`, the value of a
registration key (`architecture.md`, Key table), holds the writer's unix
seconds at the put in `epoch`; the field is informational, because liveness
is judged by the observer's own clock since the put it last saw (VW3, VW4),
never by comparing the epoch with local time. `Cntlr.settling` is the flag
HL2 clears and AR5 reads: true from the moment a cntlr becomes primary (or,
still primary, is re-enabled) until the worker first observes it clean in
that role with its stack built — no row outside `leg_id_to_leg` and
`grp_id_to_md_raid` provisioning, or missing but for the level's — or a
failover demotes it; AR5 holds a settling primary to `cntlr_unhealthy`
instead of `primary_unhealthy` when that is the longer. A record that lacks
the field decodes false, settled, and is judged by `primary_unhealthy` as
any settled primary is. Nothing else in the schema is this document's; in
particular `Migration` carries no field of this document's, because there is
no migration reaction (Automatic reactions). The generated code is
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
message interceptors). `Close` releases it. A failure to reach any endpoint
at construction is **not** fatal to the caller by itself: the client retries
in the background; the worker's vote layer stays fenced until its first
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
compacted away is an error, never silently served from a newer one. `Decode`
unmarshals one scanned value and logs it as an `etcd get` with `found` true,
so every value a caller actually reads is logged individually, as
`log.md`, etcd, requires.

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

EU4. **STM.** Two runners over the etcd client's STM with
serializable-snapshot isolation: `RunSTM`, read/write, which the etcd client
retries on conflict until the ctx ends or the transaction's EU5 budget is
exhausted — in the ordinary case the earlier of the two, a worker's ctx
living as long as the worker does; and `Snapshot`, read-only, whose reads
are all served at one store revision and whose commit is a no-op. It serves
a multi-key load that has no use for the revision itself; a load that does
need it — every one in the worker (MD3, AR1) — goes through `SnapshotRev`.

A third runner, `SnapshotRev`, is `Snapshot` that also **reports** the store
revision its reads were served at: the client's STM pins that revision at
its first read but keeps it private, and MD3 needs exactly that number to
run its bitmap scans (`RangeKeysAtRev`) at the snapshot's own revision, so
`SnapshotRev` pins it itself — first read linearizable, every later read at
that revision — instead of going through the client's STM constructor. Its
callback runs exactly once, a read-only load having no write set to conflict
on, and a put or delete inside it is a programming error that fails the
load.

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

EU7. **Tests.** The package's tests run against a real etcd binary found on
the PATH or named by `ETCD_BIN`, started on a free localhost port with a
temporary data directory, and are skipped otherwise — there is no dependency
on the etcd *server* module. The same contract serves `model`'s and the
worker's etcd-backed tests (MD9).

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
Finding CN candidates), `ops.go` (the internal mutations of MD6), `drain.go`
(MD6's sp-drain ops, The sp drain), `clonedrain.go` (MD6's clone-drain ops,
The clone drain). Unit tests are colocated.

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
`WorkerRegKey`, `WorkerRegPrefix` and `ParseWorkerRegKey`.

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
— and a `clone_bitmap` key in the six-field format parses as not ok and is
skipped by MD3's loader, which is the whole of dnv's handling of that format:
no tolerating or converting code exists anywhere.

MD3. **SP snapshot.** `LoadSp` reads, in **one** `SnapshotRev` (EU4),
everything the sp role fans out or reacts on, into an `SpState`: `Rev`, the
store revision of the snapshot; `SpRevision`, the SP's `SpRev` revision in
that snapshot, zero when the key is absent; `Conf`; `Cntlrs` by cntlr id from
`cntlr_id_list`; `Slices` by slice id from `slice_id_list`; `Tds` in
`td_name_list` order, with `TdNames` naming each entry; `Subsystems` by nqn;
`Clones`, `Xfers` and `Migrs` by name; `CloneBmIdx` and `MigrBmIdx`, the chunk
addresses present (keys only) with their mod revisions, one `BmChunk` per
chunk carrying its slice index, its index and its `ModRev`, the slice index
always zero for a migration chunk; `DnByAddr`, the `DnConf` of every side's
`addr_port` of the SP; and `CnByAddr`, the `CnConf` of every cntlr's. A
missing `SpConf` returns `ErrNotFound` (the SP is being deleted; the rev key's
delete follows). A listed sub-object whose key is missing is reported in
`Missing` and logged by the caller; the load still succeeds. `SpRevision` is
the revision the loaded state belongs to, for RW14's revision guard; the rev
key is not a listed sub-object, so an absent one is no `Missing` entry. Bitmap
**values** are never loaded here — pushes read one chunk at a time (BM3).
Bitmap indexes come from a keys-only scan (`RangeKeysAtRev`, EU2) performed
**outside** the STM at the same store revision: the STM cannot range, and the
pin is what keeps the two reads consistent — the set of indexes is not
append-only, since the clone drain deletes chunk keys, so the scan is served
at the snapshot's own revision and never at a newer one. One loader serves
both prefixes and forks on the key parser (MD2): `ParseCloneBmKey` under a
`CloneBitmapPrefix`, `ParseBmIdx` — reported at slice index zero — under a
`MigrBitmapPrefix`. A key the prefix's own parser rejects is skipped silently,
so one stray key of another shape never fails a whole SP load.

MD4. **Capacity keys** (`architecture.md`, Capacity index keys, and
`architecture.md`, DN bins). `DnBinIdx` maps a free extent count to its bin
under the cluster's `DnBinConf`, reporting not ok below the smallest bin;
`DnAllocatable` and `CnAllocatable` implement the presence rule verbatim
(pointer-list cap, `err_epoch` set, `disabled`, free floor).
`MaintainDnCapacity` takes the `addr_port` as a parameter — a capacity key
embeds it and `DnConf`, which is keyed by it, does not carry it — and deletes
the key implied by the old record (if the old record was allocatable) and puts
the key implied by the new one (if allocatable; the value holds the location);
`MaintainCnCapacity` likewise. Both are idempotent and are called by every op
that changes an input of the rule, inside that op's STM. The old record is the
record as read in the same STM, which is what makes the delete target exact.

MD5. **Allocator** (`architecture.md`, Finding DN candidates, and
`architecture.md`, Finding CN candidates, verbatim). `FindDnCandidates` scans
for a DN able to hold the requested extents, honouring the black and white
lists; its `excludeLocs` seeds the taken locations, so the caller's own
failure domains are excluded before the walk starts. `FindDnCandidatesAntiAffine` is
the two-tier rule of `architecture.md`, Per-operation allocation: tier 1 with
the exclusion; tier 2 rescans without it when tier 1 yields fewer than the
required count — the DNs the caller must actually place, never the oversampled
candidate count — and its candidates are merged **behind** tier 1's, so a
distinct-domain DN tier 1 found is never dropped; it reports whether tier 2
ran. `FindCnCandidates` scans for a CN outside the SP's own CN endpoints, its
`excludeLocs` seeding the taken locations as for DNs, and its two-tier twin
`FindCnCandidatesAntiAffine` runs tier 2 when tier 1 finds no CN at all (every
cntlr pick places one) and relaxes the location exclusion only, never the SP's
endpoints, the black list or the white list. A `Cand` carries the endpoint,
the location, the free extents and the bin index; `PickRandom` picks from a
candidate list. The scans are plain descending `Range`s **outside** any STM.
Because the picked node's free count is part of its capacity key, every op of
MD6 re-validates a pick by getting the exact capacity key the scan saw: gone
means an `ErrPrecondition` "candidate changed", on which the caller rescans
and retries the scan and the STM as one unit (the candidate unit of
`architecture.md`, Storage pools). `ReplaceCntlr` fails with the same reason
when the plan the scan ran against has moved: a surviving cntlr of the SP sits
on a CN outside the SP endpoints it is handed (MD6).

MD6. **Internal mutations.** Each is **one** `RunSTM` (`FlipCreated`: one per
`MaxFlipCreatedPerTxn` candidates, RW19), re-validates every precondition it
lists (MD7), and bumps exactly the revisions the equivalent RPC bumps
(`architecture.md`, `service Gateway` — RPC specifications, and
`architecture.md`, Workers). `now` is the caller's unix seconds; thresholds
are resolved inside from `SpConf.event_threshold` with the defaults of
`architecture.md`, Common validation. The shard and the sp id address the
`SpRev` key; the sp name the `SpConf`. Three ops — `GrowSlice`,
`CreateSpareLeg`, `SwitchSpareLeg` — also take an expected revision: when
non-zero the STM re-checks `SpRev.revision` against it first
(`ErrPrecondition` "stale revision" on mismatch); zero skips the check. The
gateway passes its request token when the request carried one and zero when it
did not — `gateway.md` GW6 is presence-based, so the skip at this layer is the
same opt-out as the skip at that one; this worker's reaction path passes zero.

* `SetDnErrEpoch` and `SetCnErrEpoch` require the record to exist. With a
  non-zero epoch they set it only when the stored value is zero (the threshold
  clock never restarts); with a zero epoch they clear it. They run
  `MaintainDnCapacity` or `MaintainCnCapacity` over the old and new records.
  **No rev bump** (`architecture.md`, Revision keys and the sync fan-out). No
  write when unchanged.
* `SetCntlrErrEpoch` requires the record to exist and applies the same
  set-or-clear rule on the `Cntlr`; when its settle argument is set and the
  epoch is zero, it also clears `settling` in the same STM (HL2) — a settle
  with a non-zero epoch clears nothing; no bump; no write when unchanged.
* `SetLegErrEpoch` and `SetSideErrEpoch` require the slice to exist and the
  leg or side to be found in any group's `leg_list` or `spare_leg_list`; the
  same rule on the embedded record; they rewrite the `Slice`; no bump.
* `FlipProvisioned` takes a list of `SideRef`s and requires the slice to
  exist: every listed side still `provisioned` false is set true; `SpRev` is
  bumped once iff any was written. It returns the sides actually flipped, so
  the `flip applied` record can name each (`architecture.md`, sp role).
* `FlipCreated` takes a list of `TdRef`s (name and td id) and commits them
  `MaxFlipCreatedPerTxn` at a time in list order, one STM each (RW19): it
  skips a candidate whose key is absent, whose `td_id` differs, or that is
  already `created`; sets the rest; each STM bumps once iff it wrote; it
  returns the tds actually flipped, as above — on an error, those of the STMs
  that committed before the failing one, which the sp worker does not log
  (RW19).
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
  cntlr's `settling` and clears the old one's (HL2), and bumps `SpRev`
  (`architecture.md`, Automatic reactions).
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
  in (the worker passes the primary's reported total; the gateway passes the
  largest possible value, so a user-driven grow is never "pending" —
  `architecture.md`, GrowSlice; `gateway.md`, Storage pools and GrowSlice);
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
  the plan the pick was scanned against (else "candidate changed", MD5: a
  cntlr committed after the caller's snapshot, whose CN and domain the scan
  could not exclude; only a gain is checked, and ahead of "not hosting a cntlr
  of this SP"); the picked CN allocatable with free extents at least the SP
  footprint (the sum of `ext_cnt` over all groups), not hosting a cntlr of
  this SP, its capacity key unchanged. It deletes the old `Cntlr` (its CN, if
  the record still exists: pointer out, footprint back, capacity, `CnRev`);
  creates the new `Cntlr` with the old one's `cntlid_slot`, `primary` as
  requested, `disabled` false and `settling` equal to `primary` (HL2), its
  `cntlr_id` the next id (new CN: pointer in, footprint out, capacity,
  `CnRev`); rewrites every existing `CdcEntry` of the SP (`ss_id` via each
  `Subsystem` in `nqn_list`), old `nvme_tr_conf` out, new in — a missing one
  is skipped, not rebuilt as the gateway's `CreateCntlr` and `DeleteCntlr`
  rebuild it (`architecture.md`, Cntlrs); the `SpConf`; one `SpRev` bump for
  both halves.
* `CreateSpareLeg` takes the slice, the group, the picked DN and the cluster
  conf, and requires the SP checks; the expected revision as in the preamble;
  the group to exist and be `RedundMdRaid1`; `spare_leg_list` below
  `MaxSpareLegPerGrp`; no spare of the group with a side still `provisioned`
  false — AR8 step 3's hold re-applied in-STM, which fails a second owner's
  create for one repair while the first owner's spare is still unprovisioned
  (AR2; `ErrPrecondition` "spare_unprovisioned"); the DN hosting no leg or
  spare of the group, allocatable, with free extents at least the group's
  `ext_cnt`, its capacity key unchanged. It appends a `Leg` with a fresh id,
  `leg_idx` one past the largest over both lists and a `Side` (`provisioned`
  false, `cntlid_slot` the first of `cntlid_slot_list`) to `spare_leg_list`;
  DN bookkeeping plus `DnRev`; the `Slice` and the `SpConf`; a `SpRev` bump
  (`architecture.md`, Spare legs).
* `SwitchSpareLeg` takes the slice, the group, the spare and the target, and
  requires the SP checks; the expected revision as in the preamble; the spare
  in `spare_leg_list`, the target in `leg_list`; each with exactly one side
  ("spare leg has no single side" / "target leg has no single side": a second
  side is a migration's destination, and a migrating leg is neither promoted
  nor parked, `architecture.md`, Spare legs); the spare's side `provisioned`.
  The spare takes the target's position in `leg_list`; the target is appended
  to `spare_leg_list`; a `SpRev` bump.
* `DrainSpCntlrs` requires the drain checks of SPD2 (`SpConf` exists, `sp_id`
  unchanged, `deleting` true — `sp_level` is deliberately not consulted) and
  every listed `Cntlr` key to exist. It deletes every `Cntlr`; per DISTINCT CN
  the SP footprint back, pointer out, capacity, one `CnRev` bump (a CN whose
  record is gone is skipped, as in `ReplaceCntlr`); the `SpConf` with an empty
  `cntlr_id_list`; a `SpRev` bump; it returns the count removed. An
  already-empty list is a no-op that writes and bumps nothing.
* `DrainSpSlice` takes the slice and the cluster conf and requires the drain
  checks; the cluster conf valid (`architecture.md`, Common validation, for
  `MaintainDnCapacity`'s ladder); `cntlr_id_list` empty; the slice key to
  exist. It pops up to `MaxDelGrpPerTxn` groups from the TAIL of
  `data_grp_list`, then of `meta_grp_list`; per DISTINCT DN every popped
  side's group `ext_cnt` back, pointer out, capacity, one `DnRev` bump (a DN
  whose record is gone is skipped); if both lists are now empty it deletes the
  `Slice` key AND removes the id from `slice_id_list` in the same STM, else
  puts the shrunken `Slice`; a `SpRev` bump; it returns the count removed and
  whether the slice is done. A slice id no longer listed is a no-op.
* `FinishSpDelete` requires the drain checks; `cntlr_id_list` and
  `slice_id_list` both empty; `SpRev` and `SpGlobal` to exist. It deletes
  `SpConf`, `SpName` and `SpRev` and decrements the shard's entry of
  `SpGlobal.shard_bucket`. It is the ONE op that does not bump `SpRev` — it
  deletes the key, which is the shard worker's stop signal (RW14).
* `DrainCloneBm` takes the clone and a list of chunks and requires the clone
  drain checks of CLD2 (`SpConf` exists and is not itself deleting, `sp_id`
  unchanged, `Clone` exists, `clone_id` unchanged, `deleting` true) and at
  most `MaxDelBmPerTxn` chunks. It deletes each named `CloneBitmap` key —
  nothing else, and nothing the caller did not name; the `Clone` record is NOT
  rewritten; a `SpRev` bump. Its return is the SIZE of the batch it was
  handed, never a count of keys that were still there — a delete is an
  idempotent pop, so the loser of an accepted two-owner overlap reports a full
  batch and removes nothing. An empty batch is a no-op that writes and bumps
  nothing.
* `FinishCloneDelete` requires the clone drain checks. It deletes the `Clone`
  key AND removes the name from `clone_name_list` in the same STM; a `SpRev`
  bump (the SP outlives the clone, so this one bumps).

MD7. **`ErrPrecondition`.** A struct carrying the op and the reason;
returned from inside the STM callback, it aborts without commit (EU4). An
op never writes partially (`FlipCreated` is the exception: its STMs, RW19,
each commit whole, and an error in one leaves those before it committed),
never sleeps, and never retries a precondition failure — the caller logs
`reaction skipped` (Log records) and re-evaluates on its next pass.

MD8. **Gateway reuse.** This rule describes the gateway and names no decision
of this document: the public RPCs of `architecture.md`, `service Gateway` —
RPC specifications, are the same STM bodies plus request validation
(`architecture.md`, Common validation), the public preconditions
(`DeleteCntlr`'s `disabled` true and `primary` false) and reply mapping; the
gateway adds those on top of `model` rather than duplicating the bodies.

MD9. **Tests.** `model`'s unit tests are colocated; the etcd-backed ones —
the allocator's walks and every MD6 op's transaction — run against the real
etcd binary of EU7 and are skipped without one.

## `cmd/dnv-worker`

CM1. **Flags.** One cobra root command, no subcommands; every flag is also a
config key and an environment variable under the prefix `DNV_WORKER_`:
`--etcd-endpoints`, the required comma-separated endpoint list; `--roles`, a
subset of `dn`, `cn` and `sp`, every role by default, each registered and
driven independently (VW10); `--vote-interval`, the seconds between registry
heartbeats (VW2), defaulting to `DefaultVoteWorkerInterval`;
`--vote-grace-time`, the seconds a membership change must hold (VW5),
defaulting to `DefaultVoteWorkerGraceTime`; `--etcd-dial-timeout`, in seconds,
defaulting to `DefaultEtcdDialTimeout` (EU1); and `--config`, an optional
viper config file. The two vote timers exist as flags so the worker suite can
run membership cases in seconds; production runs the defaults.

CM2. **Binding.** As `cmd/dnv-agent` binds (`dnagent.md`, `cmd/dnv-agent` —
cobra + viper): the flags bound through viper, the environment prefix, a
dash-to-underscore key replacer, automatic environment lookup, `--config`
read when set; required values are checked after binding so a file or the
environment satisfies them.

CM3. **Validation.** `--etcd-endpoints` non-empty; `--roles` a non-empty,
duplicate-free subset of the three roles; both timers positive;
`--vote-grace-time` SHOULD exceed twice `--vote-interval` (a warning is
logged otherwise — a grace window not longer than the dead threshold is
legal but pointless).

CM4. **Startup.** Install the default JSON logger (`common`'s `init`), mint
a startup trace id, build the `etcdutil` client (EU1), then call
`worker.Run` with the client and a `Config` carrying the endpoints, the
roles and the two timers (the endpoints feed the `worker starting` record's
`endpoints` attribute). `Run` starts the `ClusterConf` cache (RW21) and the
vote worker and blocks until the ctx ends. Startup never fails because etcd
is unreachable: the vote layer stays fenced (VW8) and retries every
interval, logging each failure. Only configuration errors make the process
exit non-zero at start.

CM5. **Shutdown.** SIGINT and SIGTERM cancel the ctx; `Run` then, in order:
stops the heartbeat loop; **deletes its own registrations** (best effort,
one `Delete` per role under `DefaultEtcdOpTimeout`, so peers start their
grace windows now rather than after the dead threshold); stops every shard
worker gracefully and in parallel (SW5 then RW11 — an in-flight `Syncup*`
or `Push*` is allowed to finish, so this can take up to
`DefaultWorkerSyncupTimeout`); closes the cache watch and the client;
returns nil. A second signal during that window exits immediately with
status 1. Deleting before draining means the successor can start while a
last syncup finishes — the accepted overlap of an ownership change (VW7).

CM6. **Logging.** JSON on stderr, Info level by default (`log.md`, R2 and R6).
The `worker starting` record carries `roles`, `seed`, `endpoints`,
`vote_interval` and `grace_time`; `worker stopping` carries `seed`. Every
round, syncup, push, flip and reaction runs under a trace id minted per RW10,
so the seed prefix in an agent's `trace_id` attributes the request to this
process.

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
rescan's revision plus one.

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
the observer's **own** still-heartbeating key takes VW8's fence exit instead
of deleting a live worker's registration.

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
the agents' revision gate, every etcd reaction is STM-guarded (save a second
owner's spare create that lands after RW18 has flipped the first owner's
spare, which can leave its group a spare no failure asked for: AR2, Known
limits), and an `err_epoch` the overlap leaves stale is corrected by the
owner's first verdict after it next loads the record — within a pass and a
round for a side, a leg or a cntlr, within a `nodeRecordMaxAge` and a round
for a DN or a CN — though at the default thresholds a pass can fail a
primary over on it first (HL3, Known limits).

### Self-fence and rejoin

VW8. A worker MUST **fence** itself when any of these holds, checked on
every heartbeat tick and on every own-key watch event:

* (a) now minus `lastOkPut` has reached the dead threshold — its heartbeat
  has not reached etcd for that long; peers are about to (or already do)
  consider it dead. This also covers a process that was stopped (SIGSTOP, a
  VM pause): the monotonic clock advances meanwhile.
* (b) its own put is not echoed by its own watch: the latest observed put
  event for its own key (any role) is older than the dead threshold while
  puts report success — the watch is broken and its view of the peers is
  stale.
* (c) a delete event for its own key that this process did not issue — a
  peer committed it dead (VW6).

Fencing means: mint a new seed (VW1) — first, because the `worker fenced`
record names it and a fence that cannot mint one tears nothing down (below);
log `worker fenced` (`reason`, `old_seed`, `new_seed`); stop
the heartbeat loop and join it, so no put of the old seed lands after its
delete; start the graceful stop of every shard worker of every role, in
parallel (SW5), and go on without waiting for it — every shard worker is
told at once, also in a role that is still waiting for the stop of a shard
it no longer owns (VW9); best-effort `Delete` the old registrations (they
may already be gone). Like CM5's delete ahead of its drain, and with CM5's
accepted overlap, this lets the peers observe a delete that lands as a
disappear and start their grace windows at once, rather than when the drain
ends — which can take `DefaultWorkerSyncupTimeout` (RW11) — or the dead
threshold passes, whichever comes first. The drain is started before the
deletes because a delete that cannot land — a VW8 (a) fence is usually cut
off from etcd — takes up to `DefaultEtcdOpTimeout` per role, and shard
workers not yet told to stop would go on driving rounds and `Syncup*` calls
behind it. Then join the drain; discard every tracking entry, timer and
effective set; and restart VW2 to VW7 from scratch under the new seed;
VW7 applies to the new incarnation: nothing is driven until one full grace
window after the new seed's first successful put. There is no "resume with
the old seed" path — a worker that lost etcd for the dead threshold is a
new worker, exactly like a restart; one code path serves every "the fleet
gave up on me" case. A fence whose new seed cannot be minted (VW1) tears
nothing down and is instead **remembered** and retried on every following
heartbeat tick, ahead of the checks above: (a) and (b) are conditions that
would fire again by themselves, but (c) is an event whose delete has
already been consumed, so a fence dropped there would be lost for good.

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
watch, tracking entries, timers, effective set, shard workers and log
records, and ownership of a role considers only the registrations under
that role's prefix. Fencing (VW8) is process-wide because the seed is.

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
is a coordinator with one child goroutine per side and per cntlr. An
**object** is a DN, a CN, a side or a cntlr — the unit of one `Check*`
stream and one goroutine; a **round** is one `Check*` request and reply
exchange on an object's stream; the **handle** is the mutable part of a rev
value, `addr_port` for a node and `sp_name` for an SP (`architecture.md`,
Revision keys and the sync fan-out).

### The per-object loop

RW1. **One goroutine per object** — one per DN (dn role), per CN (cn role),
per side and per cntlr (sp role). The goroutine alone owns the object's
`Check*` stream, its `Syncup*` calls and the loop's own state, which is what
makes the "one `Syncup*` at a time per object" of `architecture.md`, Common
agent rules, and the "one stream per object" of `architecture.md`, Check
streams, hold by construction. A parent does not wait for the stop of a
child it replaces (RW11), so a worker started for an object whose previous
worker is still stopping drives nothing until that stop has returned: a
restart never puts two loops on one object. Three deliberate carve-outs
share the object: the `Push*Bitmap` calls run on the pusher's own
goroutines (BM3 — one in flight per migration or clone, concurrently with
this loop); `lastInfo` is published by the stream's pump goroutine under a
mutex so the sp coordinator can snapshot it (AR1); and the sp coordinator's
pass, under a mutex of each side and cntlr child's health monitor, reads
the count of the monitor's own writes before its load and offers the child
its record's `err_epoch` after it, which only the child's own goroutine
folds into the memo, before its next verdict (HL3).

RW2. **State**: the desired revision plus the inputs the request is built
from, the stream, `lastInfo` (the last `*Info` received), the health state
(Health bookkeeping), and the round timer. No round's outcome is kept as
WORK STILL OWED: no last-acknowledged revision decides a re-sync, and no
"re-sync wanted" flag exists anywhere in the loop. RW4 step 5 recomputes
the whole re-sync condition from the reply in hand — its own `revision`
against the desired revision, its own `agent_reply.code` — so a worker just
handed the shard decides exactly as one that has driven the object for an
hour, and no failure can be forgotten by a flag something cleared. Two
things a round produces do outlive it, and neither is such a record:
`lastInfo`, because a `show_info` false reply omits an unchanged `*Info`
and health is evaluated on the latest known one (HL5); and the health
state, a cache of the record: the last verdict WRITTEN, re-seeded from the
record by the loads HL3 names — which is exactly what HL3's
transitions-only rule has to compare the next observation against — the
time a DN's or a CN's monitor last read or wrote the record, which paces
HL3's re-read, and a cntlr's settle memo included (HL2: the record's
`settling` as the last plan loaded it, cleared by the write that clears it
in etcd). BM5's mod-revision memo survives rounds too, but it belongs to
the pusher; and the memos of what was LOGGED — RW9's two, plus, on a cntlr
child, the last standby leg row logged per leg, so HL2's standing row does
not repeat every round — never record what is owed, and neither does a
side child's memo of what it REPORTED: the last revision it handed RW14's
sides-first barrier as applied, so a standing reply does not report it
every round.

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
4. no reply, or a stream error: close the stream; health "unreachable"
   (HL1, HL2) — except for a round abandoned under RW6, and for a round the
   graceful stop of RW11 cut short, each of which drops its stream the same
   way but carries no health verdict: a cancelled stop ctx is not a sick
   agent, and the verdict would start the threshold clock of Automatic
   reactions on a healthy object;
5. reply: process its `*Info` if present (Health bookkeeping; sp: RW18,
   RW19); then if `agent_reply.code` is non-zero **or** the reply's
   `revision` differs from the desired revision, issue the `Syncup*`
   (RW5) — this is also how the first sync after a shard handoff, a worker
   restart or an agent restart happens, with no recovery step of its own,
   and how a leftover is re-driven: the agent recomputes
   `ReplyCodeLeftover` (RW5) from a fresh enumeration of its node on every
   `Check*`, so for as long as it still holds something the desired state
   does not want, every round issues the `Syncup*` that sweeps again, and
   the round it comes clean is the round the re-sync stops — no backoff
   (RW12), no flag, the code is the state;
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
and is logged at Error. Invalid conf means the agent found a proto3 zero
where `architecture.md`, Common validation, requires a concrete value and
converged nothing (`dnagent.md`, Additions to `common`). Only `SyncupDn` and
`SyncupCntlr` can return it, and the same members are checked on this side
first — `extent_size` by RW9's gate, the SP's `bdev_conf` by RW14's — so a
request this worker sends should never provoke it; a `ReplyCodeInvalidConf`
from a live agent means the agent's copy of those rules and `model`'s have
drifted apart (`dnagent.md`, Shared mechanism — package `agent`).

`ReplyCodeLeftover` is the one non-zero code that is **not** a rejection:
the request was applied — the desired state is stored and every wanted
object converged — and the node still holds objects the desired state does
not want, or an enumeration of what it holds did not answer, which proves
nothing either way, or — on a dn — a disk whose identity it has not
confirmed, which it reports the same way so that its `SyncupDn` is re-sent
(`architecture.md`, Teardown by sweep); the agents' other such conditions
ride only on `Check*` and `Get*Info` replies. It is logged as
`syncup leftover` (`log.md`, Leftovers) beside the ordinary
`syncup result`: nothing agent-side re-drives the sweep for a leftover (the
agents' own background converges, `cnagent.md` CN10's connect retry and
`dnagent.md` DN13's connect retry and DN12's fence timer, sweep only while
one is registered or armed for a reason of its own); it runs again on the
re-sync RW4 step 5 issues, so a leftover is a normal state for as long as a
dead remote's failfast window lasts, and the record is what makes one the
re-sync keeps finding visible in the log. A cn teardown's reply carries the
code routinely: the cn agent's sweep sets every `nvme disconnect` it issues
— of a leg or a clone source — going off its pass (`cnagent.md` CN10) and
names the connection as a leftover (the nvme kind plus the NQN) until a
later pass finds it gone (a removing pass once it has no controller; a
Check round once its sysfs subsystem directory, which can outlive the last
controller, is gone), so a `SyncupCntlr` or `SyncupCn` whose sweep
disconnects anything is answered `ReplyCodeLeftover` however fast the
disconnect turns out to be. The disconnect needs no re-sync to finish, and
the first Check round that no longer finds the connection, with nothing
else left, answers code zero, after which RW4 step 5 issues nothing more; a
round before it re-issues the `Syncup*`, whose pass sets no second
disconnect of that subsystem going while the first still runs. While all
that is left is a subsystem directory whose last controller is gone, the
re-issued `Syncup*`'s removing pass counts the connection gone and answers
code zero, so the worker re-syncs every round and logs a `syncup result`
with no `syncup leftover`; what names the connection then is the agent's
own `sweep leftover` record of each Check verdict (`log.md`, Leftovers).

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
round is such a round. The next round opens a fresh stream, which the
agent answers with the complete `*Info` again (`architecture.md`, Check
streams). An abandoned round carries **no** health verdict: nothing was
observed about the agent, the round was only overtaken. The sp coordinator
is the one parent that hands a change on late, and on purpose: its cntlr
children receive a fan-out once every running side child has reported it
applied, and at the latest one `cntlr_interval` after it (RW14's sides
first); from that hand-over on, this rule applies unchanged.

RW7. **Connections.** A process-wide cache from `addr_port` to one client
connection (`grpc.NewClient` with insecure transport credentials and both
client interceptors — `grpc.md`, Wiring), reference-counted by the objects
using an endpoint and closed when the last one stops. Every stream and
unary call to one agent multiplexes over that connection. A stream is
opened lazily on the first round and after every failure, and closed with
`CloseSend` plus ctx cancellation on stop (RW11).

RW8. **Round timeout** is the object kind's interval: a reply arriving
later is a missed reply (`architecture.md`, Check streams). The timer is
re-armed *after* each round, so a slow round never queues a burst of
catch-up rounds.

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
under which the agent runs the round (`grpc.md` T3).

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
half of an sp fan-out that restarted a side: RW14's sides-first hold waits
for that side's report, which its new child can send only once the old
child's stop has returned (RW1), so those cntlrs can wait out the call up
to the hold's one-`cntlr_interval` bound. A parent that stops joins its
children, those stops included, with a wait group (SW5).

RW12. **No backoff anywhere.** The round is the retry cadence for
everything the loop could not finish — an unreachable agent, a rejected
syncup, a leftover reply, a health write that did not commit, a missing
cluster conf. A failed PUSH is the one thing NOT on that list, and not
because it waits longer: it arms nothing at all (BM6), so no round retries
it. It is re-planned only when the object's next `Syncup*` is issued for
some other reason, from the agent's own acknowledged set (BM2).

### dn role — `worker/dnrole.go`

RW13. Inputs: `cluster_id` and `dn_id` from the key, `addr_port` and
`revision` from the value. Per syncup the loop reads the `DnConf` at
`DnConfKey` of the cluster and endpoint with a plain `Get` (missing: log,
skip, retry next round) and the cluster conf from the cache, and sends a
`SyncupDnRequest` carrying `cluster_id`, `dn_id`, the revision,
`side_pointer_list` from the record's `side_ptr_list` and `extent_size`
from the cluster's `dn_bin_conf` to the endpoint. Rounds send a
`CheckDnRequest` carrying `cluster_id`, `dn_id`, the revision, `show_info`
and `trace_id`. Health per HL1, judged against a cache of the record's
`err_epoch` that the syncup's read re-seeds and that the health monitor
re-reads itself, with the same `Get`, before a verdict once
`nodeRecordMaxAge` has passed without its reading or writing the record
(HL3). A put whose only change is `addr_port` re-syncs the node at its new
endpoint: the loop drops its stream and connection reference and continues
at the new address; no delete ever reaches the agent (`architecture.md`,
dn / cn roles, [D10]).

### cn role — `worker/cnrole.go`

The mirror image: the `CnConf` at `CnConfKey`, a `SyncupCnRequest` carrying
`cluster_id`, `cn_id`, the revision, `cntlr_pointer_list` from the record's
`cntlr_ptr_list` and `qos_ratio` from the `ClusterConf`, a `CheckCnRequest`,
and health per HL1.

### sp role — `worker/sprole.go`

RW14. The SP revision worker is a **coordinator**. On every desired change
(an `SpRev` put: `revision` plus `sp_name`) it loads the SP with
`model.LoadSp` (MD3), resolves every side's `dn_id` (through `DnByAddr` by
the side's `addr_port`) and every cntlr's `cn_id`, builds every
`SyncupSideRequest` and `SyncupCntlrRequest` once (RW15, RW16), and diffs
its **children**: one per side — the sp id, leg id and side id, spare legs'
sides included — and one per cntlr. New: start; gone: stop (RW11), without
waiting for the stop; a child whose endpoint changed is restarted at the
new one, the new child driving nothing until the old one has stopped (RW1);
every remaining child receives its new request as a desired change (RW6). A
child whose **request** changed while the `SpRev` revision did not — a
re-resolution tick that altered a standby list — is restarted too, because
RW3's coalescing sees the unchanged revision and would otherwise swallow
the change and leave the child driving a stale request until the next
bump. That is the fan-out: unordered within each kind, sides before cntlrs
(below). `ErrNotFound` from `LoadSp` means the SP is being deleted: log,
keep the children until the `SpRev` delete arrives (the agents tear down
through the pointer lists). An endpoint without a `DnConf` or `CnConf`
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
their slice (HL2).

**Sides first.** The side half of the diff is applied at once. The cntlr
half — every running cntlr child's new request, and every cntlr child to
start — is HELD until every running side child has reported the fan-out's
revision (or a newer one) applied, or until one `cntlr_interval` has passed
since the hold began, whichever comes first. A side left idle (above) is
not waited for, and with every side idle the cntlrs go at once. A side
child the diff restarts — at a new endpoint, or on a changed request — is
waited for like any other, though its new child reports nothing until the
old child's stop has returned (RW1; the cost is below). A cntlr child the
diff stops is stopped at once, beside the sides', and one it restarts is
started with the release. A side child reports each accepted reply (RW5)
whose revision is newer than the last one it reported, and a `CheckSide`
reply counts as much as a `SyncupSide` one, because a side whose agent
already holds the revision — after a handoff, say — is sent no `Syncup*` at
all (RW4 step 5). The hold lives on the coordinator's loop, released by
those reports or by a timer on the same loop, and never blocks it: reports,
ticks and the next desired change are served while it waits, and every
child keeps its rounds. A fan-out that finds a hold pending replaces what
is held and keeps the deadline, so a stream of bumps cannot hold the cntlrs
past one interval, and the superseded requests are never sent (RW3). A
release by the timer logs `sp sides unsynced` (Log records). Nothing of the
hold is persisted: a new owner, or a restarted worker, holds its own first
fan-out the same way. While a hold is pending, the leg rows of a cntlr the
held plan no longer makes primary are dropped (HL2): its agent still runs
the primary shape over legs the sides have already reloaded onto dm-error.
The coordinator judges every leg row by the plan it last decided for that
cntlr, so rows the old primary built before the release and that are read
after it are dropped too. The order mitigates three races and is no
correctness dependency (`architecture.md`, [D16]): a promotion's
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

**What the hold costs.** It holds every cntlr request, a failover's demotion
as much as its promotion: the sides' flips fence the old primary before it is
told it is no longer primary, so an old primary still serving host IO answers
that IO with target-internal errors on its still-optimized path from the time
the flips have fenced its legs until its agent has applied the demotion the
release hands its child: the hold, then that `SyncupCntlr`'s delivery, its
wait for the cntlr's object lock (`cnagent.md` CN1) and its converge up to the
first **old_primary** step (`architecture.md`, Failover, and
`architecture.md`, [D16]). Only an accepted reply reports, so a side whose
agent does not accept the fan-out's revision costs that fan-out up to one
`cntlr_interval` of cntlr latency and an `sp sides unsynced` record, a
failover's promotion and demotion included, and RW9 lets that interval be as
long as `MaxHealthCheckInterval` allows. That is paid on every fan-out of a
pool with a side on a dead disk node, and AR8's repair does not end it: the
leg AR8 replaces is parked in `spare_leg_list` with its side (AR8 step 1), and
a parked leg's side is a side child like any other. Until that disk node
returns or an operator's `DeleteSpareLeg` removes the parked leg, every hold
of that SP runs the whole interval and logs `sp sides unsynced`, a disable of
its primary is failed over inside its own hold whenever AR5 has a candidate,
and after a restart or a handoff its cntlr children start — and begin their
`CheckCntlr` rounds — one whole interval late. It is also paid in normal
operation: a new side — of a new SP, a grow, a spare, a migration destination
— whose first `CheckSide` and `SyncupSide` reach its disk node before the dn
role's `SyncupDn` has introduced its pointer is refused
(`ReplyCodeUnknownObject`, `dnagent.md` DN8; normal per RW5) and tries again
at its next round, one `side_interval` after its first round ended, which
falls after the hold's deadline when the two intervals are equal, as they are
by default. It is paid as well, in part or in whole, when the diff restarts a
side whose old child is still inside a `Syncup*`: the new child sends and
reports nothing until that call has ended and the old child has stopped (RW1,
RW11), which can be up to `DefaultWorkerSyncupTimeout` later — past the hold's
deadline at the default `cntlr_interval`.

Between the load and the plan the fan-out validates the SP's stored
`bdev_conf` with `model.ValidateBdevConf` (`architecture.md`, Common
validation). That geometry is what RW16's request carries verbatim and what
RW15's migration destination takes its `block_size` from — a plain side
request carries none of it. `dm_raid0_conf.stripe_size` is read on no
worker path at all, and `redund_md_raid1.bitmap_chunk_block_cnt` only
inside `model.GrowSlice`'s geometry (MD6), which AR6 drives; otherwise both
travel verbatim inside `bdev_conf` to the cn agent, so this is the one
place the worker can refuse to hand that agent a geometry nobody chose. A
refusal emits one `invalid stored conf` record (Log records, once per
distinct error) and builds no request, starts no child and updates no
running one — `buildPlan` and the child diff are simply not reached. It
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

RW15. **Side request.** A `SyncupSideRequest` carrying `cluster_id`,
`dn_id`, the `side_pointer` (sp id, leg id, side id), `revision` (the
`SpRev` revision) and a `side_conf` with `ext_cnt` from the group,
`cntlid_slot` from the side, `primary_cn_id` the `cn_id` of the cntlr with
`primary` set (zero when cntlrs exist but none is primary; an SP with NO
cntlr gets no side request at all, its side children staying idle — SPD7),
`standby_id_list` the `cn_id`s of every other cntlr — disabled ones
included, a disabled cntlr keeps its standby shape (`cnagent.md` CN9) —,
`sp_level` from the `SpConf` and `provisioned` from the side. If the side's
leg has two sides and a `Migration` of the SP names this side as
`src_side_id`: a `migr_src_conf` with `migr_id`, `dst_side_id`, `dst_dn_id`
and `dst_provisioned` (the destination side's `provisioned`); as
`dst_side_id`: a `migr_dst_conf` with `migr_id`, `src_side_id`, `src_dn_id`,
`src_nvme_tr_conf` (the source side's `nvme_tr_conf`), `block_size` from
`bdev_conf.dm_pool_conf.data_block_size`, `meta_blocks` from the group, the
migration's `dm_clone_conf` and `bm_cnt` from the `Migration`. The
migration's `dm_clone_conf` is the one conf still filled in here (a zero
`hydration_threshold` or `hydration_batch_size` takes its default of
`architecture.md`, Common validation, on a copy — the loaded state is
shared with every child). `block_size` and everything else is sent as
stored; a zero in the SP's geometry was refused by RW14's gate instead.

RW16. **Cntlr request.** A `SyncupCntlrRequest` carrying `cluster_id`,
`cn_id`, the `cntlr_pointer` (sp id, cntlr id), the revision, `bdev_conf`
from the `SpConf`, `sp_level`, `cntlr` (the `Cntlr` record), `id_to_slice`
keyed by the slice id rendered with `IdKeyFmt` — the key the cn agent reads
—, `td_list` in `td_name_list` order, `nqn_to_subsystem`, `clone_list`,
`xfer_list` and `migr_list`. Every cntlr of the SP receives the full state
(`architecture.md`, sp role) — less any clone whose `deleting` is set, which
is absent from every cntlr's `clone_list` (CLD5).

RW17. **Rounds** send a `CheckSideRequest` carrying `cluster_id`, `dn_id`,
the `side_pointer`, the revision, `show_info` and `trace_id`, and a
`CheckCntlrRequest` carrying `cluster_id`, `cn_id`, the `cntlr_pointer`,
the revision, `show_info` and `trace_id`.

RW18. **Provisioned flip** (`architecture.md`, sp role). On an ACCEPTED
`SyncupSide` or `CheckSide` reply (code zero or `ReplyCodeLeftover` — the
gate RW19 defers to, HL1 the reason) for a side whose driven request
(`provisioned` moves only from false to true, so the request being driven
and the last one the agent acknowledged cannot disagree here) carried
`provisioned` false and whose `side_info` reports `zeroed_ext_cnt` equal
to a non-zero `total_ext_cnt`, the child reports the side to the
coordinator, which runs `model.FlipProvisioned` (several sides reported
within one round MAY share one STM). The bump re-fans the SP (RW14) and the
re-synced sides carry `provisioned` true. Nothing is remembered across a
handoff: the new owner's first round carries the full `*Info` and flips
whatever is still false.

RW19. **Created flip** (`architecture.md`, sp role). Every `SyncupCntlrReply`
and `CheckCntlrReply` with an accepted code (RW18: code zero or
`ReplyCodeLeftover`) is scanned: a td of the loaded state with `created` false
is a candidate when `cntlr_info.td_id_to_thin_info` holds its `td_id`, that
entry's `slice_id_to_dm_thin` key set equals the SP's slice ids exactly, and
every row is `RES_STATUS_OK`. Candidates go to `model.FlipCreated`; a reply
that completes none causes no etcd traffic. The call commits them
`MaxFlipCreatedPerTxn` at a time, in list order, one STM each, and each STM
that wrote bumps `SpRev` once: the coordinator folds every td one drain of its
reports completed into one call, a set only `MaxTdCntPerSp` bounds, and one
STM over all of it would exceed `EtcdMaxTxnOps` long before that bound and be
refused again on every round. One such STM compares every key it read and
every key it wrote — a get of each candidate's td key, a put of each one it
flips, and `SpRev`'s read and put — so it costs at most two compares per
candidate plus two, a count a test pins from the named constants against
`EtcdMaxTxnOps`, while another commits such STMs against a real etcd at the td
ceiling and pins which candidates each one carried. A pending provisioned flip
of the same SP MAY share one of those STMs; that STM then also reads the
`Slice` of each side the provisioned flip lists and writes each `Slice` it
changes, up to two compares per slice over the count above, which an
implementation taking the MAY must add to the budget test. This one does not
take it: RW18's flip runs as an STM of its own, before the created flip's.
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
`model.ValidateClusterConf` and **refuses** rather than guessing a geometry
the rest of the cluster may not agree with; the cache validates nothing
itself, because its four readers refuse differently — idle the loop (RW9),
keep the tick cadence and skip the pass (AR1), fail the DN health write
(HL1) — and a validating get would either hide that or log it four times.
An invalid conf is deliberately still **cached**: dropping it would make a
cluster whose conf went bad indistinguishable from a deleted one and send
the operator chasing a phantom deletion that never happened. A cluster
**absent** from the cache is the separate RW9 and SW6 idle path, with a
record of its own. Watch errors and compaction are handled like SW4.

## Health bookkeeping — `worker/health.go`

HL1. **Nodes (dn/cn roles).** Evaluated per round and per syncup reply on
the node's own stream; written through `model.SetDnErrEpoch` and
`SetCnErrEpoch`, which maintain the capacity key in the same STM (MD4;
`architecture.md`, Capacity index keys). The effect of each observation on
`DnConf.err_epoch` or `CnConf.err_epoch`:

* the stream cannot be opened, breaks, or no reply arrives within the round
  timeout: set to now if zero; the in-memory info is marked
  `RES_STATUS_UNKNOWN` (what the worker records itself while the stream is
  dead, `architecture.md`, Live-state reporting — never written to etcd);
* any `RES_STATUS_ERROR` row in `DnInfo` (`disk_info`, `meta_info`,
  `port_info`) or `CnInfo` (`port_info`, `tmpfs_info`, `tmp_file_info`,
  `loop_dev_info`): set to now if zero — including a `meta_info` that reads
  "disk lacks Write Zeroes" (`architecture.md`, Side provisioning protocol),
  which is a plain `ERROR`;
* a clean round — a reply in time, an accepted code (zero or
  `ReplyCodeLeftover`), no `ERROR` row in the latest known info: cleared to
  zero;
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

The **DN** write needs a usable `ClusterConf` and re-reads it from the cache
per write rather than capturing it: MD4 derives the capacity key's bin
index from `dn_bin_conf`, so a conf that is missing from the cache, or that
fails `model.ValidateClusterConf`, fails the write and leaves it to the
next round (RW12). It re-validates rather than trusting RW9's gate because
it is not COVERED by one: that gate checked the conf the round was built
from, while this write is the health monitor's, issued against a conf
re-read from the cache after the gate ran — one the watch goroutine may
have replaced in between. It is not the only worker STM write that takes a
`ClusterConf` — the sp role's `GrowSlice`, `CreateSpareLeg` and
`DrainSpSlice` take one too (MD6, HL6) — but those three sit behind AR1's
cluster-conf gate, and `GrowSlice` re-checks both confs, `DrainSpSlice` the
cluster's, at the top of its own STM besides. Guessing a ladder here would
leave the real capacity key undeleted, and the bin scan of
`architecture.md`, Finding DN candidates, would go on offering it as an
allocation candidate for a DN HL1 has just flagged unhealthy — a DN whose
`err_epoch` is set implies no key at all (MD4), so nothing is written in
its place. The **CN** write needs no conf at all: CN capacity keys carry no
bin index (`architecture.md`, Finding CN candidates).

HL2. **SP objects (sp role).** Written through `SetCntlrErrEpoch`,
`SetLegErrEpoch` and `SetSideErrEpoch`:

* `Cntlr.err_epoch` is set to now (if zero) when its `CheckCntlr` stream
  cannot be opened, breaks or misses a round, or any `RES_STATUS_ERROR` row of
  its `CntlrInfo` **other than** `leg_id_to_leg` reads so; it is cleared when
  its next round is clean. **Settle**: `Cntlr.settling` is cleared when the
  record is settling and a clean round is a reply of the cntlr as an enabled
  **primary** at the revision its child drives — the child's plan says
  `primary` and the record it carries is not `disabled` (the agent's own
  effective role, `cnagent.md` CN9), and the reply's `revision` equals the
  plan's, and the reply shows the stack built: no row of the maps this row is
  judged by (every map but `leg_id_to_leg`), `grp_id_to_md_raid` aside, reads
  `PROVISIONING`, or `MISSING` with details other than `cnagent.md` CN19's
  "sp_level"; with the `err_epoch` clear in one write, or in a write of its
  own when `err_epoch` is already zero.
* `Leg.err_epoch` is set to now (if zero) when the **primary** cntlr's
  `leg_id_to_leg` row of the leg reads `RES_STATUS_ERROR` (the health-block
  probe of `architecture.md`, Group on-leg layout: meta region, data region,
  health block; spares included) — a standby's leg row is logged, never
  recorded — and cleared when the primary reports the row `RES_STATUS_OK`.
* `Side.err_epoch` is set to now (if zero) when its `CheckSide` stream cannot
  be opened, breaks or misses a round, or `side_dev_info` or any
  `cn_id_to_dm_error`, `cn_id_to_dm_linear` or `cn_id_to_nvmeof` row is
  `RES_STATUS_ERROR`, or a `migr_src_info` or `migr_dst_info` row is `ERROR`;
  it is cleared when its next round is clean.

`PROVISIONING`, `PENDING`, `MISSING` and a rejection code never set any of
the three, and `Leg.err_epoch` clears on the primary's `RES_STATUS_OK`
alone. `PENDING` is the primary's leg row while its prober for the leg has
not completed a round (or none is registered yet) — a first round stalled
past `CnLegProbeStallSeconds` reads `ERROR` — and a fresh prober starts at
every promotion and agent restart (`cnagent.md` CN11); were that row to
read `OK`, every promotion's fresh probers would clear a dead leg's
`err_epoch` and restart AR8 case 1's `leg_unhealthy` clock.
`ReplyCodeLeftover` is not a rejection: its rows are evaluated exactly as a
code-zero reply's — here, and in the RW18 and RW19 reports the same replies
carry — for HL1's reason. The **primary** of the `Leg.err_epoch` row is the
cntlr that the plan the coordinator last decided makes primary — the held
plan while an RW14 sides-first hold is pending, else the one its child was
handed — so the leg rows of a primary that a held failover demotes, and
the rows it built before the release that are read after it, are dropped:
they neither set nor clear a `Leg.err_epoch`, and, unlike a standby's,
they are not logged (RW14).

**Row classes.** The `ERROR` rows a cntlr is judged by — every map but
`leg_id_to_leg` — are of two classes. A **shared-state** row belongs to the
stack of a td whose `created` is set and whose thin row, in some slice,
reads `ERROR` with "No data available" in its details: the ENODATA with
which dm-thin refuses the bare "dmsetup create" of a created td's thin
table once the slice's pool no longer holds its id, as dmsetup prints it
(`cnagent.md` CN14). The stack is the td's own rows — its thin rows, its
raid0 and its dm-error — and every row of an object that exists for it
alone: the ns-dev and the nvmet namespace of each of its namespaces, the
three rows of a transfer out of one of them, the three of a clone onto it.
The pool lives on the SP's legs, so whichever cntlr holds the primary role
reads the same rows, and no failover brings the td back: an operator does
(`architecture.md`, v1 assumptions and known limits). Every other `ERROR`
row is the cntlr's **own**. Both classes set `Cntlr.err_epoch` as the
rules above say — the row stays visible and the epoch set — and the class
steers AR5 and AR7 alone: a report whose `ERROR` rows are all shared-state
is no failover trigger, nor a replacement one for a primary with no
failover candidate. Only a converge's report names the id. A Check round's
probe finds the volume absent and reads it `MISSING` with empty details,
while the raid0 over it still reads `ERROR`, so its report of the same loss
holds own rows only. It is the second refusals that bound what those rows
cost: AR5's keeps the role from moving back and forth over them, and AR7's,
once a primary with no failover candidate has been replaced over them,
keeps its replacement, which reads the same rows, from being replaced in
turn (Known limits).

**The settle.** `Cntlr.settling` is set by the op that makes the cntlr primary
(`Failover`, a primary `ReplaceCntlr`, MD6; the gateway's `CreateStoragePool`
for the SP's first primary, and its `UpdateCntlrEnabled` when it re-enables a
cntlr that is still primary — its agent then builds the primary stack from the
standby shape it held while disabled) and cleared by `SetCntlrErrEpoch` with a
zero epoch and the settle argument on the settle condition above, or by a
`Failover` that demotes the cntlr before it settles (MD6) — the flag describes
a primary, and a standby's would steer nothing. The revision
gate is load-bearing: when the promotion's `SyncupCntlr` never reached the
agent (a transport failure before the agent stored the request), the next
Check round's reply carries the previous revision and describes the
**standby** shape — clean, and meaningless for the promotion (RW4 step 5
re-syncs it). One the agent applied but whose reply was lost leaves the agent
at the driven revision, and its Check reply then reports the primary shape.
Rounds and syncups run on the child's one goroutine (RW1), and a revision
carries one set of roles: a role change is agent-visible desired state, which
bumps `SpRev` (`architecture.md`, Revision keys and the sync fan-out), and
RW14 builds no plan from a state newer than the revision it labels the plan
with (an etcd restore, which can hand a revision number out twice, aside). So
an accepted reply at the driven revision from a cntlr that is primary and not
disabled is a report of the primary shape; a disabled primary converges the
standby shape (`cnagent.md` CN9), so its clean reply settles nothing. Nor does
a reply whose stack is not built yet (`primaryShapeBuilt`): a new SP's primary
reports a slice's pool rows, and the thin volumes in that pool, `PROVISIONING`
until every leg of the groups under it has a provisioned side, and its raid0s
— with the ns-devs, namespaces, clones and transfers over them — until no
slice is deferred (`cnagent.md` CN9; the ns-devs, on the td's dm-error, their
namespaces and a transfer's device, subsystem and namespace are built
meanwhile but read `PROVISIONING` all the same, CN16 rule 0, CN17): clean, and
no proof of the build that follows, and settling on it would leave that build
to `primary_unhealthy`. A `MISSING` row of the maps the settle reads holds it
for the same reason, unless its details are "sp_level" (below). A converge
that finds a member not available — a promotion ahead of the sides' ANA flips,
a provisioned flip ahead of a side's export (`architecture.md`, [D16]) that
the connect step's pass budget does not cover (`cnagent.md` CN10) — reports
the groups and pools it could not build `ERROR` and leaves them to the CN10
retry, whose first attempt comes `CnConnectRetryInterval` later; a Check round
in between probes those devices absent and reports them `MISSING` with empty
details, not `ERROR`, and in an SP with no td, as a new SP is until one is
created, that reply has no `ERROR` row outside `leg_id_to_leg` (a td's raid0
row reads `ERROR` while its thins are absent; a leg row reads the CN11 prober,
which fails on a path still non-optimized). Every other `MISSING` the cn agent
reports is likewise a device not built, or a clone whose source is not
connected or could not be read this pass (CN18), so a primary showing one
stays settling, held to the longer threshold, until it clears — for as long as
a clone's source stays unconnected or unread. Leg rows do not count: a spare
still zeroing reads `PROVISIONING` there for as long as it zeroes. Nor do
group rows, for a grow's sake: a grow appends its new groups to their lists,
and while their sides zero they stay out of the live concat (CN9's prefix
cut), so their group rows alone read `PROVISIONING`, beside a serving pool,
for minutes. Nothing else needs them: an SP is created with one group per list
(`architecture.md`, Storage pools), so a group of a new SP that is still
provisioning defers its whole slice, whose pool rows say so, and a group a
converge could not assemble leaves the pool rows over it `ERROR` or `MISSING`.
A primary of an SP still in that first zeroing therefore stays settling,
judged by the longer threshold, until the build that follows it reports its
stack built and clean — indefinitely if a side never finishes, since a leg
still provisioning never sets an `err_epoch` for AR8 to repair it by. So does
a primary that takes the role (promoted, re-enabled, or created as primary by
AR7) while a leg of the first group of its list has as its only side a
migration destination that `FinishMigration` forced before that side's RW18
flip: the cn agent cannot tell that leg from one still in its first zeroing,
so it defers the whole slice and every td with it (CN9) — until that flip, and
without end if the side never finishes. In a later group, one the pool's
concat already spans, the same leg reads as a fault instead: the concat under
the thin-pool cannot shrink, so a pool row reads `ERROR`, which holds the
settle as any fault does. A row the `sp_level` suppresses reads `MISSING`
"sp_level", not `PROVISIONING` (`cnagent.md` CN19), and holds nothing, so a
primary does not wait for a layer its level suppresses. CN9's deferral does
not depend on the level, though: from `SP_LEVEL_NO_THINPOOL` through `NO_SIDE`
the pool, thin, raid0 and clone rows read "sp_level" (the group rows too from
`NO_REDUND`, the leg rows at `NO_SIDE`), but while a slice is deferred the
ns-dev, namespace and transfer rows still read `PROVISIONING` (CN16 rule 0,
CN17). So at those levels a primary with a namespace or a transfer settles
only once the zeroing ends, and one with neither — or any primary at
`SP_LEVEL_DISABLE`, where every row reads "sp_level" — settles on a reply that
shows no pool built, possibly before its sides are even zeroed; AR3 suppresses
AR5 at all of those levels, so the longer hold changes no reaction there. That
settle is final — `UpdateStoragePoolLevel` re-arms nothing — so the build that
follows a lowered level is judged by `primary_unhealthy`, as any settled
primary's is. The child keeps a memo of the flag, seeded from the record in
every plan it takes (RW14's plan copies it), so the settle is normally written
once and logged as `cntlr settled` (Log records): a plan loaded after the
settle write clears the memo; one loaded before it costs one redundant write
that changes nothing and one extra `cntlr settled` record; a failed write
keeps the memo and is retried on the next reply (RW12). The flag steers AR5
alone — and `Failover`, which re-validates AR5 in its STM — and is re-read
from etcd on every pass: health bookkeeping of the same kind as `err_epoch`,
not a memo of a failed step.

HL3. **Transitions only.** A record is written when the observed health
changes (healthy to unhealthy sets the epoch once — while the record stays
set, the threshold clock of Automatic reactions never restarts; unhealthy
to healthy clears it). The op re-reads the record inside its STM, so two
owners observing the same transition write once. Health never bumps a
revision (`architecture.md`, Revision keys and the sync fan-out). What an
observation is compared with is a cache of the record, not only a memory
of the monitor's own writes, and these loads re-seed it: the sp
coordinator's reaction pass, every `cntlr_interval` (AR1), hands each side
and cntlr child its record's `err_epoch`, which the child folds in before
it judges its next reply or missed round, and re-seeds the leg monitors it
keeps itself; the dn and cn roles re-seed from the `DnConf` or `CnConf`
every syncup reads (RW13), and since a node whose revision stays put and
whose agent answers code zero never syncs, a dn or cn monitor also
re-reads the record itself before a verdict once `nodeRecordMaxAge` has
passed without its reading or writing it — one `Get` per `nodeRecordMaxAge`
for a node that neither syncs nor changes health, and one at each verdict
while that `Get` fails. A monitor that has written nothing yet — a child
just started, a new owner after a handoff — starts empty and writes its
first verdict whatever the record holds. So a record that says unhealthy
while the observation is clean is a transition and clears, and one that
says healthy while the observation is not sets a new epoch: an `err_epoch`
another observer wrote or cleared — the other owner of an overlap (VW7), a
partitioned observer (Known limits) — is corrected at the owner's first
verdict after its next load, a reply that neither sets nor clears being no
verdict (HL1, HL2): within a pass and a round for a side, a leg or a cntlr
— though at the default thresholds a pass can fail a primary over on it
first, as it can after one bad round (Known limits) — and within a
`nodeRecordMaxAge` and a round for a DN or a CN. While two owners disagree,
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
reply's (the secondary signal, `architecture.md`, Check streams). A
`Syncup*` gRPC failure is not by itself a health failure — an unreachable
agent breaks the stream too.

HL5. `show_info` false streams carry an `*Info` only when something
changed; the worker evaluates health on the latest known info, and a reply
with neither info nor error is a clean round.

HL6. **No cross-role health writes.** The sp role's health bookkeeping —
this section's `err_epoch` writers — never touches `DnConf` or `CnConf`
(it writes `Cntlr`, `Leg` and `Side` only), and the dn/cn roles never touch
SP records. (The sp role's *reactions* do rewrite `DnConf` and `CnConf`
through the model ops — `GrowSlice` charges DNs and every cntlr CN,
`CreateSpareLeg` charges its one DN, `ReplaceCntlr` rewrites two `CnConf`s
— and so does the sp drain, whose D1 credits every cntlr CN and whose D2
batches credit every DN they touch (`model/drain.go`) — but that is
allocation bookkeeping, not health.) An unreachable DN is therefore marked
on `DnConf` by its dn-role owner and on the `Side` records of its sides by
each sp-role owner, independently.

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
`PushMigrBitmapRequest` carrying `cluster_id`, `dn_id`, the
`side_pointer`, `migr_id`, `bm_idx` and the `bitmap`, or a
`PushCloneBitmapRequest` carrying `cluster_id`, `cn_id`, the
`cntlr_pointer`, `clone_id`, `src_slice_idx`, `bm_idx` and the `bitmap`,
under `DefaultWorkerPushTimeout`; the next part is sent only after a
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
dead, the longer one when only the cntlr's path is bad.

### Pass, priority, suppression

AR1. **Cadence and inputs.** The coordinator runs one pass per SP every
`cntlr_interval` (its own ticker). A pass starts with a fresh `SnapshotRev`
(EU4) of `SpConf`, every `Cntlr` and every `Slice` of the SP — the records
the reactions read, and the ones this worker itself writes the
`err_epoch`s into — and the sub-objects `SpConf` lists (AR5's and AR7's
row classes attribute rows through its tds, subsystems, transfers and
clones), plus the in-memory latest `CntlrInfo` of the **primary** cntlr
(pool usage, spare readiness, AR5's and AR7's rows), `now` (unix seconds)
and the two records kept from an earlier pass that a pass decides by: the
coordinator's records of the last failover it applied (AR5's second
refusal) and of the last replacement of a primary it applied (AR7's second
refusal). The snapshot re-seeds the SP's health memos first, ahead of every
gate below (HL3). The primary is the one the snapshot names, and its info
is taken only once its child has been handed the primary plan. While
RW14's sides-first hold keeps a promotion from the child, the child's info
is still a standby's report — a leg row there is transport liveness and
ANA state, not the health-block probe AR8 step 1 reads as a spare's
readiness — so the pass has none: AR6 grows nothing and AR8 switches no
spare in, as when the primary has not reported yet. From the hand-over
until the reply to the promotion, the info is still that standby report
(Known limits, one-pass-old inputs).

**Both** stored confs are validated before the reactions are evaluated —
the cluster's with `model.ValidateClusterConf`, the SP's `bdev_conf` with
`model.ValidateBdevConf` (`architecture.md`, Common validation). Both,
because this pass takes its own fresh snapshot of the SP rather than
reusing the fan-out's: AR6 reads `low_water_mark_pct` and `data_block_size`
straight off it and sizes a meta grow with the cluster's `extent_size`,
while the DN scans of AR6 and AR8 walk the bin ladder and every allocating
reaction takes its oversampling batch from `alloc_conf`. A failure refuses
the pass with one `invalid stored conf` record (Log records, once per
distinct error) and no reaction runs: no candidate scan, no reaction
`model` op, no `reaction applied` and no `reaction skipped`. Neither drain
sits behind both gates: the clone drain's steps (CLD7) run first of all,
ahead of the cluster-conf lookup and of both validations, and a latched
SP's drain step (SPD6) runs behind the cluster-conf gate — its D2 batch
maintains DN capacity keys off the ladder — but ahead of the `bdev_conf`
one, so a refusal of the SP's own geometry still lets that SP drain. The
ticker keeps running — the pass cadence falls back to
`DefaultHealthCheckInterval` on an unusable conf and logs nothing of its
own, because a coordinator that stopped ticking would stop retrying the
fan-out and the pass forever, and those two are where the refusal is
already recorded. A cluster missing from the cache stops the pass the same
way, silently — after the clone drain's steps, which read no cluster conf,
and before an sp drain step, which does: its children log
`cluster conf missing` for the idle period (RW9).

AR2. **One action per SP per pass** — "action" meaning a *reaction*
throughout this rule — evaluated in this priority; the first applicable one
runs and the pass ends:
1. primary failover (AR5)
2. thin-pool auto-grow (AR6)
3. cntlr replacement (AR7)
4. leg repair (AR8)

Every action is a `model` op that re-validates its preconditions inside its
STM (MD6, MD7); success logs `reaction applied` and the resulting `SpRev`
bump re-fans the SP (RW14), so the next pass sees the new state. The
invariant is **at most one applied action per SP per pass**; the clone
drain's steps are not actions in this sense and commit alongside one — a
pass over a live SP can bump `SpRev` once per deleting clone (CLD7, CLD12)
and once more for its reaction. A `reaction skipped` that means "not
applicable here, keep looking" does not end the pass. The pass continues
past: AR5's no-failover-candidate (AR7's sole-primary variant is defined as
"AR5 found none" and would otherwise be unreachable); AR5's `shared_state`
and `same_error`, which can hold for as long as the error does — an
operator's intervention, for a lost thin id; AR6's `grow_pending`,
`grp_list_full`, `meta_ladder_cap` and `no_data_group` (each can hold
indefinitely — a grow deferred on the CN, a ceiling of `architecture.md`,
GrowSlice — and must not disable AR7 and AR8 for the duration); AR7's
`shared_state` and `same_error`, which can hold as long as AR5's and move
the scan to the next cntlr; and AR8's `leg_has_two_sides`,
`spare_list_full`, step 3's `spare_unprovisioned` and step 2's wait for a
pending spare (`spare_pending`), which move the scan to the next candidate
leg — a spare that cannot be connected stays pending until its leg has
been unhealthy for `leg_unhealthy`, one the primary never reports has no
bound at all, and step 3's hold on a spare whose DN failed while it zeroed
has none either. Everything else ends the pass: every `ErrPrecondition`,
every empty allocator scan and every transient op failure. Two owners
overlapping on one SP (VW7) cannot apply an action twice — the second STM
fails its precondition — with one exception: a spare create.
`CreateSpareLeg` refuses one while a spare of the group still has an
unprovisioned side (MD6), but nothing records which repair a spare was
made for, so a second owner's create that lands only after RW18 has
flipped the side of the first owner's spare can commit — when the group
held no spare before the first create (otherwise that create filled the
list) and the two owners picked different DNs (Known limits).

AR3. **Suppression.** No reaction runs for an SP with `sp_level` at or
above `SP_LEVEL_NO_THINPOOL` (the disaster-recovery levels of
`architecture.md`, SpLevel, where an operator is in charge). An SP with
`deleting` true is not suppressed here: it runs at most one DRAIN step per
pass and no reaction at all, at ANY `sp_level`, because a doomed SP must
drain whatever level an operator left it at — "at most", because the AR1
gates that sit ahead of the branch still apply: a pass whose `LoadSp`
failed, or whose cluster conf is missing or unusable, runs no step at all
(the SP's own `bdev_conf` gate sits *behind* it, deliberately — SPD6). A
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
validation). That `leg_unhealthy` exceeds `side_unhealthy` is a gateway
validation rule (`architecture.md`, Common validation); the worker is
correct either way.

### Failover

AR5. When the primary cntlr is `disabled`, or has `err_epoch` set and `now`
minus it at least `primary_unhealthy` — `cntlr_unhealthy`, when that is the
longer, while it is **settling** (HL2): the candidate is the cntlr with the
smallest `cntlr_id` among those with `primary` false, `disabled` false and
`err_epoch` zero; none means `reaction skipped` (`no candidate`) and the
pass continues (AR2); else `model.Failover` of the old primary and the
candidate — for an enabled primary, unless one of the two refusals below
holds it (the first is judged ahead of the candidate, and where there is
none it logs in place of `no candidate`). This is the failover trigger of
`architecture.md`, Failover; the data-plane choreography is the agents'.
The `disabled` trigger waits out no threshold: disabling is explicit
operator intent, and the disabled primary has normally stopped serving by
then (`architecture.md`, Cntlrs) — its disable request reaches it one
sides-first hold after the bump (RW14). A pass runs inside that hold
whenever a side of the SP does not report and the hold lasts a whole
`cntlr_interval` — at every disable, while the SP keeps a leg AR8 parked
on a dead disk node (RW14) — and fails the primary over first: the
failover's fan-out replaces the held request, which is never sent, and the
sides fence that primary before its demotion arrives (`architecture.md`,
Failover). The candidate-skip bookkeeping is unchanged by that trigger, so
it is deliberately **not** edge-triggered: a `disabled` primary with no
eligible candidate re-emits `reaction skipped` / `no candidate` once per
pass, for as long as it takes an operator to enable a standby or add a
cntlr.

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
(MD6, MD7, reason "cntlr_unhealthy not reached for a settling primary"). The
settling hold leaves the `disabled` trigger and the candidate rule as they are
— `failoverEligible` never reads the flag. A settling primary with no
candidate reaches the no-candidate skip only at the longer of the two
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
  it by (every map but `leg_id_to_leg`) is of HL2's shared-state class:
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
  do not name — a lost thin id a Check round reports, whose probe names no
  id, among them (HL2) — does not send the role back to the cntlr it last
  left while the new primary fails no row the old one did not: in an SP of
  two cntlrs it moves the role once, and in a larger one it can first move
  it on to a cntlr that has not held it. An `err_epoch` set later is a new
  error, judged as any, and a report with no `ERROR` row — a primary read
  unreachable, whose rows the worker marks `UNKNOWN` — is refused nothing.

Neither refusal holds a `disabled` primary: that trigger is the operator's.
Both read a report, which no STM sees, so `model.Failover` does not
re-validate them. The second rests on the coordinator's record of the last
failover it applied, one of the two records of its own a pass decides by
(AR7's second refusal rests on the other): why the role moved is kept in no
etcd record. A coordinator without it — restarted, handed the shard, or the
other owner of an overlap (VW7) — can fail over once more, which records it
again.

### Thin-pool auto-grow

AR6. Per slice, from the primary's `slice_id_to_dm_pool` row of the slice,
which MUST be `RES_STATUS_OK` (an `ERROR`, `PROVISIONING` or absent pool is
never grown). Its `details` is the raw "dmsetup status" line of the thin
pool; after the "thin-pool" token come the transaction id, the used and
total metadata blocks and the used and total data blocks — metadata counts
in dm-thin's fixed metadata block size (`thinMetaBlockSize`), data counts
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
times the block size divided by `thinMetaBlockSize`. While pending, no grow of
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
`model.GrpListFull`.

### Cntlr replacement

AR7. When a cntlr has `err_epoch` set, `now` minus it at least
`cntlr_unhealthy`, `disabled` false, and either `primary` false or it is the
primary and **no failover candidate exists** (AR5 found none — the sole-cntlr
SP, or every other cntlr unhealthy or disabled): the candidates are
`FindCnCandidatesAntiAffine` with the SP footprint (the sum of `ext_cnt` over
all groups), a candidate count of `cn_batch_size`, a black list of the old
cntlr's `addr_port`, the SP's other cntlrs' endpoints as the SP's CN endpoints
and those CNs' locations excluded — the locations out of the pass's own
snapshot (MD3; a CN missing from it contributes none), the old cntlr adding
none of its own because it is the one leaving, so its domain is excluded only
when a survivor shares it (unlike AR8 step 3, whose spare also avoids the
failing leg's domain: excluding the old cntlr's domain here would empty tier 1
in a two-domain cluster the SP's cntlrs already span, and the tier-2 rescan
could then put the replacement in a survivor's domain), and the tier 2 of
`architecture.md`, Per-operation allocation, rescanning without them when tier
1 finds no CN; pick one; an internal `ReplaceCntlr` of the old cntlr, the
pick, those endpoints and the old cntlr's `primary` as the new one's — the
same `cntlid_slot`, `primary` true and `settling` true only in the
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

The sole-primary variant takes two refusals, the twins of AR5's. Both are
judged once AR7's threshold has run out, whether or not AR5's has, on the
report AR5 judges, the info the pass holds for the primary (AR1, HL5), and
each skip moves the scan to the next cntlr, so a standby due for
replacement is still replaced, and with none the pass goes on (AR2). A
standby's report is not read here, and carries no thin row (`cnagent.md`
CN14), so its `ERROR` rows are its own.

* **Shared state.** A primary whose latest report has `ERROR` rows of HL2's
  shared-state class alone is not replaced — `reaction skipped`
  (`shared_state`, with its `old_cntlr_id` and the smallest such `td_id`).
  The replacement would read the same rows from the same pool. The report
  names the lost id only while it is a converge's (HL2). A Check reply
  carries an info when the probe's view changed since its stream last sent
  one, and always on a fresh stream's first probe (HL5; `architecture.md`,
  Check streams), so a converge's report stays the one held only until that
  view changes or the stream is replaced, as by a round timeout, a broken
  stream, a desired change that overtakes a round (RW6), or a worker
  restart or shard handoff. A primary whose held report is a probe's,
  whose rows a dead stream has marked `UNKNOWN` (HL1), or whose child holds
  no report yet is not held by this refusal (Known limits).
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
  Check round's report of a lost thin id names no id, so the first refusal
  lets the primary be replaced on it, and the replacement, which reads the
  same rows from the same pool, would otherwise be replaced in turn once
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
  overlap (VW7) has none, and can replace the replacement once more, which
  records it again — with no row when the pass holds no report of it yet,
  as that coordinator's first pass can find it, so that the one after it is
  not held either.

Like AR5's refusals these read a report, which no STM sees, so
`model.ReplaceCntlr` does not re-validate them.

### Leg repair

AR8. **Triggers.** A leg in a group's `leg_list` needs repair when either

* **Case 1** — `Leg.err_epoch` is set and `now` minus it is at least
  `leg_unhealthy`: unhealthy from the cntlr's perspective for the long
  threshold (the primary reports it has no healthy path — possibly a CN to
  DN connectivity problem, hence the longer wait); or
* **Case 2** — `Leg.err_epoch` is set (any duration) **and** its side has
  `Side.err_epoch` set with `now` minus it at least `side_unhealthy`: the
  side reports an error or the worker cannot talk to its DN, so the DN
  itself is probably dead, hence the shorter wait. A side the worker cannot
  reach while the primary still sees the leg healthy triggers nothing.

**Preconditions**: the group is `RedundMdRaid1` (`RedundNone` groups have
no spare — both cases only log); the leg has exactly one side (a leg with
two sides has a user migration in flight and is left alone;
`SwitchSpareLeg` re-checks this inside its STM, MD6, so step 1's switch is
refused too when the migration starts after the pass read the leg); the SP
is not suppressed (AR3). Several unhealthy legs: the smallest `leg_id`
first.

**Procedure**, one step per pass, on the leg's group:
1. a **ready** spare exists — it has exactly one side (a spare with two
   sides has a user migration in flight and is neither ready nor pending;
   MD6's switch would refuse it), that side has `Side.provisioned` true,
   and the primary's latest `leg_id_to_leg` row of the spare reads
   `RES_STATUS_OK` (spares are connected and probed, `architecture.md`,
   Spare legs) — then an internal `SwitchSpareLeg` of the spare and the
   leg: the spare takes the leg's place, the old leg is **parked** in
   `spare_leg_list` — still connected and probed, its `err_epoch` still
   set, never repaired again (only `leg_list` legs are); a user-created
   ready spare is used the same way — that is what spares are for.
   "Probed" holds literally — the primary reports a leg whose prober has
   not completed a round as `RES_STATUS_PENDING` (a first round stalled
   past `CnLegProbeStallSeconds` reads `ERROR`, `cnagent.md` CN11), so a
   fresh spare is not switched in before any probe has run; such a spare is
   not ready, and step 2 waits for it unless it is dead;
2. else a **pending** spare exists — a spare that is not ready yet
   (provisioning, or not yet reported `OK` — `PENDING` included) and not
   dead: its side has `err_epoch` zero, and its leg has `err_epoch` zero
   **or has had it for less than `leg_unhealthy`** — then wait for it — on
   this group only; the scan goes on to the next candidate leg (AR2). The
   leg's `err_epoch` does not disqualify a spare outright: HL2 probes spares
   too, so a fresh spare can read `ERROR` from the moment its side is
   provisioned until the primary's connect to it completes, and its leg
   then carries an `err_epoch` a few seconds old; read bare, that would
   make the spare this step waits for a dead one, and a pass landing inside
   the transient would run step 3 and create a second spare for one repair
   — usable, since the group's next repair switches it in, but asked for
   by no failure, holding an extent and a connection per cntlr.
   `leg_unhealthy` is AR8 case 1's own threshold: a spare whose leg keeps
   reading `ERROR` that long is dead, and step 3 replaces it while the
   group has a slot. The side test stays bare — a side with an `err_epoch`
   is one whose DN the worker cannot reach or that reports an `ERROR` row
   (case 2's condition), which is how a leg parked by case 2 was retired;
   a leg parked by case 1 was retired after `leg_unhealthy`, so it starts
   out dead by the leg test. Neither stays dead by identity: a parked leg
   is still probed, so one whose DN comes back (side `err_epoch` cleared)
   while its leg still reads `ERROR`, or one that recovers and later fails
   again (a fresh leg `err_epoch`), is a pending spare again by this same
   rule, and holds this group's next repair until it reads `OK` (step 1
   then switches it in) or its leg has been unhealthy for `leg_unhealthy`;
3. else `spare_leg_list` is below `MaxSpareLegPerGrp` — then an internal
   `CreateSpareLeg` on a fresh DN: `FindDnCandidatesAntiAffine` with the
   group's `ext_cnt`, a candidate count of `dn_batch_size`, a required
   count of one and a black list of the DNs of every leg and spare of the
   group; tier 1 also excludes their locations — taken from the pass's own
   `DnByAddr`, a DN missing from it contributing none — and a tier-2 rescan
   without the location exclusion runs when tier 1 offers none of the
   **one** DN this step places, never when it merely falls short of the
   candidate count (`architecture.md`, Per-operation allocation), so a
   cluster with one failure domain still repairs; pick one; the new side
   provisions (`architecture.md`, Side provisioning protocol), RW18 flips
   it, the cntlrs connect to it, and a later pass finds it ready. The step
   is held — no scan, no op — while a spare of the group still has an
   unprovisioned side, which `CreateSpareLeg` refuses (MD6): the pass logs
   `reaction skipped` (`spare_unprovisioned`) and goes on to the next
   candidate leg (AR2). Step 2 waits for such a spare while it provisions;
   the ones this hold catches are those step 2 does not wait for, such as
   one whose DN failed while it zeroed (its side's `err_epoch`), which
   holds the step until that DN finishes zeroing it or an operator deletes
   it (Known limits), or one a migration gave a second side that is not
   provisioned yet;
4. else `reaction skipped` (`spare_list_full`): once repairs of one group
   have parked `MaxSpareLegPerGrp` legs the list is full, and only
   `DeleteSpareLeg` by an operator frees a slot — a spare list that is full
   is an operator event, the worker never deletes a parked leg (Known
   limits) — and it refuses a parked leg with a migration running on it
   (`architecture.md`, Spare legs).

AR9. **The worker never**: deletes a td, subsystem, transfer, migration or
spare, or a clone that is not `deleting`; touches a `RedundNone` leg; acts
on a suppressed SP; starts a migration; or runs two reactions in one pass.
Every automatic action re-homes redundancy or roles (`architecture.md`,
Automatic reactions). It does delete the implicit children — cntlrs,
slices, groups, legs, sides — of an SP a `DeleteStoragePool` has LATCHED,
which is the sp drain and not a reaction (the five-empty-lists
precondition that let it be latched means it holds no td, subsystem, clone,
transfer or migration; a spare leg it may still hold is a leg and goes with
its group), and it does delete a `Clone` a `DeleteClone` has LATCHED — its
chunk keys in batches, then the record together with its
`clone_name_list` entry — which is the clone drain and not a reaction
(CLD8, CLD9). Both drains finish a deletion a user began with the latch and
neither ever starts one; the list above is what no reaction touches.

### The sp drain

`DeleteStoragePool` does not tear an SP down. It LATCHES it — `deleting`
true plus one `BumpSpRev`, nothing else (`architecture.md`, Storage pools;
`gateway.md`, Storage pools and GrowSlice) — and the sp coordinator takes
it apart in steps whose size is a constant. A one-shot teardown is
unbounded in the DN dimension, and `GrowSlice` lets a slice's group count
grow to `MaxGrpCntPerSlice` per group list (`architecture.md`, GrowSlice),
so no single transaction could ever be proven legal.

SPD1. **The allocator's real group shape is a named constant.**
`MaxAllocLegPerGrp` is the widest group the allocator builds
(`MaxLegPerGrp` is declared and unenforced). It is CITED from all three
places that choose a leg count — `legCntOf` in `gateway/alloc.go` and in
`model/ops.go`, `legCnt` in `worker/reaction.go` — and from the SPD14
tripwire, so widening the shape fails a test instead of a deployment.

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

SPD3. **The repeat delete is a no-op.** `DeleteStoragePool` on an SP whose
`deleting` is already true returns OK with no writes and NO second `SpRev`
bump — the drain is running, and a bump would only invalidate every
client's token to force a pointless re-resolve. A stale token still ABORTs
first.

SPD4. **The check and the latch are atomic.** The five-empty-lists check
and the `deleting` put share one STM with the reads, and once latched
`resolveSp`'s `rejectDeleting` gate refuses every other mutator — so no new
child can appear after the check, ever.

SPD5. **The latch is one-way.** No code path in any component writes
`deleting` false on an existing SP. There is no undelete, and the
one-way-ness is what makes SPD8's derivation total: a latched SP has
exactly one future.

SPD6. **Entry and cadence.** AR3 splits: a pass over an SP with `deleting`
true runs at most ONE drain step and no reaction, regardless of `sp_level`
suppression — "at most", because the gates ahead of the branch still
apply: a pass whose `LoadSp` failed, or whose cluster conf is missing or
unusable, runs nothing at all and retries on the next tick. (The SP's OWN
`bdev_conf` gate is deliberately NOT one of them: the drain reads no
geometry, and gating it there would make an SP whose stored `bdev_conf`
cannot be read permanently undeletable, the latch being one-way.) The
FIRST step comes from the ordinary `cntlr_interval` pass — the latch's
`SpRev` bump reaches the coordinator as a desired change, which drives the
fan-out, not the pass. After that a committed step ends in `BumpSpRev`
(except the last) and schedules the next pass from its own commit, so the
drain is its own tick; a step that removed nothing does not schedule one,
and is picked up by the tick instead. A FAILED step commits nothing, bumps
nothing and is retried on the next tick, forever (RW12, no backoff): there
is no terminal-failure state, because a delete that gave up would only
strand garbage. Progress is already visible through `GetStoragePool` —
`deleting` true and a shrinking inventory — so no status field and no
progress key is added.

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

SPD8. **Step selection.** From the freshly loaded `SpConf` ALONE, first
match wins: `cntlr_id_list` non-empty means **D1** `DrainSpCntlrs`; else
`slice_id_list` non-empty means **D2** `DrainSpSlice` on the LOWEST listed
slice id; else **D3** `FinishSpDelete`. Nothing is remembered between steps
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
GW12's deletion half on `SpGlobal`, guarded on both id lists being empty so an
owner one step behind cannot skip to the end. It is the one drain STM that
does NOT bump `SpRev`: it deletes the key, which is already the shard worker's
stop signal, so the drain terminates itself in the transaction that finishes
the job. After the commit the name is reusable.

**A lost sub-object key wedges the drain, deliberately.** D1 refuses when a
listed `Cntlr` key is absent and D2 when a listed `Slice` key is; both are the
record that says which node reserved what, so without them the release cannot
be made exact, and a drain that carried on would under-credit a node silently
and for ever. The opposite call is made for a missing `DnConf` or `CnConf`:
those are the RECEIVING ledger, there is nothing left to credit, and skipping
keeps the SP deletable (`releaseCn`'s stance). The cost of the refusal is that
such an SP stays latched — the latch is one-way (SPD5) and nothing else
removes the keys — so the drain names the repair on every tick in
`sp drain failed` (reason "cntlr not found" or "slice not found"), and an
operator restores the missing key by hand — a raw etcd put, with etcdctl, of
the `Cntlr` or `Slice` record: the worker suite's driver has no subcommand
that CREATES such a key for an SP that already exists (its `set-cntlr`
rewrites only a cntlr that still exists, and its `put-sp` refuses an SP whose
`SpConf` exists) — to let it finish. That is a louder and more recoverable
state than a silent ledger drift, which is why it is the direction chosen.

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
key, so at every commit boundary the keys and the budgets agree exactly.

**Transaction budget.** etcd caps a transaction at the largest of its
compare, success and failure counts, and `etcdutil`'s serializable-snapshot
STM compares every key it READ *and* every key it WROTE, so the compare
count is what binds. Per D2 batch the compares grow with the distinct DNs
the batch touches — the `SpConf`, the `Slice` and the `SpRev` plus, per DN,
its `DnConf` and `DnRev` on the read side, and the `Slice`, the `SpConf`
and the `SpRev` plus, per DN, its `DnConf`, its capacity key's delete and
put and its `DnRev` on the write side — and that DN count is bounded by
`MaxDelGrpPerTxn` groups of at most `MaxAllocLegPerGrp` legs plus
`MaxSpareLegPerGrp` spares each. Sides contribute one DN apiece because a
latched SP has no migrations and therefore no two-side legs. D1, D3 and the
latch are trivially legal. SPD14 is the tripwire pair that keeps it so.
This batch is one of the bounded transactions above etcd's default cap —
the created flip's transaction (RW19) is another — and not the one
`EtcdMaxTxnOps` is sized by: that is `CreateStoragePool`'s widest shape
(`gateway.md`, Additions to `common/constants.go`).

SPD14. **Tripwires.** The budget of SPD13 is kept legal by a pair of tests:
an arithmetic assertion over the NAMED constants, and a maximum-shape D2
batch committed against a real etcd started with `--max-txn-ops` at
`EtcdMaxTxnOps`, so a widening of `MaxDelGrpPerTxn`, `MaxAllocLegPerGrp` or
`MaxSpareLegPerGrp` that would overrun the budget fails a test instead of a
deployment (SPD1).

### The clone drain

The clone drain is the sp drain's sibling. `DeleteClone` does not sweep a
clone's bitmap chunks. It LATCHES the clone — `deleting` true, the destination
namespaces resumed, one `BumpSpRev` (`architecture.md`, Clones; `gateway.md`,
Clones) — and the sp coordinator removes the chunk keys in batches of a
constant size and then the clone itself. A sweep of the whole rectangle of
chunk keys in one transaction would grow with the slice and chunk ceilings,
which can still grow; a batch is a constant number of ops whatever they
become.

CLD1. **Live-clone gate.** `AppendCloneBitmap` and `UpdateCloneTrConf`
refuse `FAILED_PRECONDITION` when `deleting` is true. `DeleteClone` is the
only RPC allowed to act on a deleting clone, and it acts as a no-op (CLD3).
The append half is load-bearing: a racing append could otherwise write a
chunk key behind the drain, and CLD9's emptiness guard rests on "after the
latch, no chunk key can ever appear again". `CreateClone` needs no check —
the surviving `Clone` key keeps same-name creation at `ALREADY_EXISTS`
until the final STM.

CLD2. **Load and refuse, never skip.** Each drain op loads the `SpConf` and
the `Clone` inside its OWN STM and returns an `ErrPrecondition` when the SP
is missing, its `sp_id` changed, the SP is itself `deleting`, the `Clone`
is missing, its `clone_id` changed, or the `Clone`'s `deleting` is false.
It is SPD2's shape verbatim and inverted the same way: normal clone RPCs
(CLD1) require the flag CLEAR, drain ops require it SET. The SP-deleting
arm cannot fire — `DeleteStoragePool` needs an empty `clone_name_list` and
a draining clone keeps its name there — and is guarded anyway, so the two
drains can never run on one SP at once.

CLD3. **The repeat delete is a no-op, checked in PHASE 1.** OK, no writes,
no bump, and no agent call, with or without `force`. The placement is the
rule: after the latch the clone has left every cntlr's `clone_list` and the
CN's next sweep has taken the stack with it (CLD5), so `GetCntlrInfo`
reports no dm-clone and a hydration check would wedge every repeat delete
in `FAILED_PRECONDITION` for ever. Phase 1 therefore runs `gateway.md`
GW6's token check itself, since it is the decision there and `openSpRead`
skips it.

CLD4. **The latch, and what it does not write.** The write set is the
destination-namespace resume — one `Subsystem` put per subsystem of the SP
that held a suspended namespace of the destination td, at most
`MaxSsCntPerSp` of them (`resumeCloneDstNs` in `gateway/clone.go`) — plus
exactly two more: the `Clone` put with `deleting` true and one `BumpSpRev`.
It does not delete the `Clone` key, does not touch a chunk key and does not
shrink `clone_name_list` — `model.LoadSp` fetches clones by iterating that
list, so a dangling name would break every later load (SPD11, the sp
drain's slice-final rule, applying verbatim). The RESUME rides the latch so
that it and CLD5's exclusion arrive in one `SpRev` bump, hence one syncup:
deferred to the end of the drain, CN16's `auto_resume` override would
vanish the moment the clone left the plan while etcd still said suspended,
and the destination namespace would go dark for the whole teardown — a
host-visible outage. (Force-deleting an unhydrated clone exposes unhydrated
data on the resumed namespace.)

CLD6. **The latch is one-way** — CLD4's latch, and SPD5 scoped to a clone:
no code path in any component writes `deleting` false on an existing
`Clone`, so the flag is a point of no return across restarts of every
component. CLD9's emptiness argument rests on it — with an un-latch, chunk
keys could appear again after the scan that found none — and so do CLD1
and CLD3.

CLD5. **Exclusion is the teardown.** From the first post-latch fan-out the
deleting clone is absent from every cntlr's `clone_list` and from the
primary's chunk-push plans (BM4 targets), at every `sp_level`. Full absence
is deliberately distinct from level suppression: a level-suppressed clone
(`SP_LEVEL_NO_CLONE`) keeps its local chunk files for a later rebuild,
while a deleting one must lose them, and the cn agent's cntlr-level sweep
(`cnagent.md` CN21) drops them precisely when the clone id is absent from
the request — the test is membership of `clone_list`, not the presence of
a wrapper, since a standby builds no wrapper to key it off. That sweep, and
the build phase that follows it in the same converge, are the whole
physical teardown — the ns-dev parked and then put back on its ordinary
backing, dm-clone and metadata wrapper removed, arena units freed, the
source's disconnect set going unless another clone on the node may still
use that source, local chunk files dropped — so there are ZERO agent
changes: to an agent this is indistinguishable from a post-delete syncup.
The disconnect runs off the pass (`cnagent.md` CN10), so the pass that sets
it going answers `ReplyCodeLeftover` with the source among its leftovers
(RW5), the sweep's layers under the source wait for a pass that finds it
gone (`cnagent.md` CN21), and the first Check round that no longer finds it
(its sysfs subsystem directory gone, RW5), with nothing else left, answers
code zero. No push of the clone is submitted once the exclusion has reached
the primary's child, which is when the RW14 sides-first hold the latch's
fan-out lands in releases the cntlrs. Until then that child drives the
pre-latch plan, whose clones still carry the clone's chunks, and a
`SyncupCntlr` reply can submit a push from it (a leftover reply re-syncs
every round, RW4 step 5), while a pass inside the hold may already run CLD8
batches; a push already submitted also runs on after the hand-over. Such a
push can find a chunk key a batch deleted and end with `bitmap push failed`
with the error "chunk not found" (BM6), which is harmless: the clone is
going away.

CLD7. **Cadence and step selection.** The drain runs ALONGSIDE the normal
reaction pass, not instead of it — this is the deliberate deviation from
SPD6, whose latched SP runs only drain steps: here the SP is healthy and
its other children must keep converging. Each pass runs at most one step
per deleting clone, bounded by `MaxCloneCntPerSp`, in `clone_name_list`
order; the steps run ahead of EVERY gate of the pass — the cluster-conf
cache lookup, `ValidateClusterConf`, the SP's own `bdev_conf` gate and
AR3's suppression — because none of them applies. A doomed clone drains at
any `sp_level`; the drain reads no geometry and no cluster conf (unlike the
sp drain's D2, which maintains DN capacity keys and therefore stays behind
the ladder's gate); and a gate that could stop it would strand a latched
clone permanently, the latch being one-way. The derivation, from the pass's
snapshot alone: surviving chunk keys in the state's `CloneBmIdx` mean one
batch on the LOWEST `MaxDelBmPerTxn` of them in `(src_slice_idx, bm_idx)`
order; none means the final STM. No other state is consulted — not the
`Clone` record, not the rectangle, not a progress key — so crash, restart
and handoff all resume through it. Ascending order is free to choose (chunk
keys are independent and self-positioning, unlike the sp drain's tail-pop)
and is picked for determinism.

CLD8. **The batch.** Deletes ONLY keys named by the caller's keys-only
snapshot scan, at most `MaxDelBmPerTxn` of them, each delete an idempotent
pop; refuses a larger batch, so a caller that handed over its whole scan
could not silently rebuild the unbounded sweep. It MUST NOT rewrite the
`Clone` record and ends in `BumpSpRev`. Physical effect: none. The CN drops
its local chunk files in the sweep that follows the exclusion (CLD5),
agents never read etcd, and a push of the clone that reads a chunk key a
batch deleted ends there, harmlessly (CLD5).

CLD9. **The final STM.** Delete the `Clone` key, put the `SpConf` with the
name removed from `clone_name_list`, `BumpSpRev` — the two removals
together and never separately. It is invoked only when the derivation's
scan found zero surviving chunk keys, and that emptiness is stable even
though an STM cannot range: after the latch commits no chunk key can ever
appear again (CLD1 refuses an append inside an STM that reads the `Clone`,
and an append that read the `Clone` BEFORE the latch fails its compare
because the latch rewrote that key), and batches only delete. Unlike the sp
drain's D3 this BUMPS `SpRev`: the SP outlives the clone, so the bump is
every mutator's normal epilogue rather than a stop-signal deletion.

CLD10. **Asynchrony and retry.** SPD13 verbatim: no drain STM waits on,
calls or verifies any agent; etcd emptiness MAY outrun physical teardown,
and a CN that is down keeps its stale stack until its next syncup, which
removes it because the clone is no longer in the request and not because
anything recorded that it should be. A failed step commits nothing, bumps
nothing and retries on the next RW12 tick, forever; progress has exactly
two user-visible states — `deleting` true, then `NOT_FOUND` — accepted
deliberately, since even a maximum-shape drain is a handful of sub-second
transactions after the latch: its batches and the final STM (CLD12).

CLD12. **Termination and the revision contract.** The latch, every batch
and the final STM each end in `BumpSpRev` with their own op tag; a repeat
delete and a failed step bump nothing. Termination is structural: no
committed batch can ever ADD a chunk key (CLD1 closed the only writer), the
set is finite, the final STM removes the clone, and a pass that finds no
deleting clone runs no drain step, so the drain commits and bumps nothing
(that pass's own reactions are unaffected, CLD7) — under one owner, one
bump for the latch, one per batch and one for the final STM per deleted
clone. Note the shrink is a property of the SET, not of each batch: the
loser of an accepted two-owner overlap commits a batch of keys the winner
already popped, so ITS batch shrinks nothing — and still terminates,
because its next pass re-scans, sees the winner's deletes and moves on.
That is also why the coordinator arms after every committed batch rather
than on progress, which is the sp drain's rule (SPD6: "a step that removed
nothing does not schedule one") and would be dead code here: a batch
reports the size it was handed, never a count of keys it found. A bump
invalidates a client's stale `gateway.md` GW6 token exactly as
`ReplaceCntlr` does: correct, and not new.

CLD11. **Transaction budget, and its tripwire pair.** Ledger-free, so one line
per STM. etcd caps a transaction at the largest of its compare, success and
failure counts, and `etcdutil`'s serializable-snapshot STM compares every key
it READ *and* every key it WROTE, so a batch costs `MaxDelBmPerTxn` compares
plus a constant few for the `SpConf`, the `Clone` and the `SpRev` — the bound,
and independent of every ceiling constant. The final STM is a handful; the
latch is strictly smaller than a sweep of the whole rectangle in one
transaction. A batch also fits etcd's DEFAULT cap, which asks no deployment
change: the requirement is `EtcdMaxTxnOps` for the transactions that do NOT
fit it, such as `CreateStoragePool`'s maximum shape, which is what that number
is sized by (`gateway.md`, Additions to `common/constants.go`), the sp drain's
D2 batch (SPD13) and the created flip's transaction (RW19). Tests pin, from
the named constants, the batch's compare count and those of the transactions
just named, and a drain of a whole maximum-shape rectangle against a real etcd
pins the batch COUNT.

## Log records

The worker's records are JSON records under the rules of `log.md`, R1 to R12:
all at Info unless stated, all carrying the trace id of RW10 where one exists.
Record names (the `msg` strings) are normative — the worker suite parses them.
An attribute written "error?" is present only when the operation failed. Two
families are `log.md`'s and are not restated here: the records `etcdutil`
emits for every etcd read, write, range and watch event (`log.md`, etcd), and
the `syncup leftover` record RW5 emits (`log.md`, Leftovers). The records of
the client interceptors are `grpc.md`'s (L1 to L6); an agent's log mirrors
each as its server-side record under the same `trace_id`, which on a `Check*`
stream is the id the stream was opened under, for every round, while a round's
own id is the `trace_id` inside its request (`grpc.md` T3).

Lifecycle and membership:

* `worker starting` with `roles`, `seed`, `endpoints`, `vote_interval` and
  `grace_time` (CM4, CM6), and `worker stopping` with `seed` (CM5);
* `worker registered` with `role` and `seed`, at the first successful put of
  a role (VW2);
* `membership observed` with `role`, `seed`, `state` (live or dead) and
  `own` (a bool), at every observed transition (VW3);
* `membership committed` with `role`, `seed`, `state` (member or nonmember)
  and `member_cnt` (VW6);
* `shard owned` and `shard released` with `role`, `shard` and `seed`, the
  owner being the logging worker itself (VW9); a release is logged after
  the join (SW5);
* `worker fenced` with `reason` (`heartbeat_stalled`, `watch_stalled` or
  `key_deleted`), `old_seed` and `new_seed` (VW8).

Revision workers:

* `revision worker started` and `revision worker stopped` with `role`,
  `shard`, `cluster_id` and `id`, plus `side_pointer` or `cntlr_pointer` for
  an sp child (SW3, RW11);
* `cluster conf missing` with `cluster_id`, once per idle period (RW9);
* `invalid stored conf`, at Error, once per distinct error and never once
  per round: from a revision worker's conf gate (RW9) with `role`, `shard`,
  `cluster_id`, `id`, an sp child's pointer and `error`; from the sp
  coordinator's two gates, the fan-out's `bdev_conf` gate (RW14) and the
  pass gate (AR1), with `cluster_id`, `sp_id`, `sp_name` and `error`;
* `syncup result` with the object's ids, `revision`, `code` and "error?",
  on every `Syncup*` reply or failure (RW5);
* `syncup rejected` with the ids, `revision`, `code` and `details`, at Error
  for a stale revision (RW5).

Health and the sp role:

* `health changed` with `role`, `cluster_id`, the ids, `record` (dn, cn,
  cntlr, leg or side), `err_epoch` (zero or now), `reason` (`unreachable`,
  `error_row` or `recovered`) and "res_name?", on every HL1 or HL2
  transition, judged against the memo HL3 re-seeds from the record — the
  correction of an epoch another observer wrote or cleared included;
* `cntlr settled` with `role` (sp), `cluster_id`, `sp_id`, `cn_id`,
  `cntlr_pointer` and `revision` (the reply's, which the settle requires to
  be the one the child drives), when HL2's settle is written: at most once
  per acquisition of the primary role, the re-enable of a primary counting
  as one and none when the cntlr is demoted before it settles, plus a repeat
  for a plan loaded before the write landed (HL2) or for a second owner in
  an overlap (VW7);
* `flip applied` with `kind` (provisioned or created), `cluster_id`,
  `sp_id`, the ids and `revision`, the new `SpRev` (RW18, RW19);
* `sp sides unsynced` with `cluster_id`, `sp_id`, `revision` (the held
  fan-out's) and `side_cnt` (the side children that had not reported it
  applied when the timer fired), when RW14's sides-first hold is released
  by its timer: one record per hold, never per round. It is expected in
  normal operation too: for a new side whose first `SyncupSide` its disk
  node refused (`dnagent.md` DN8), which usually reports only after the
  deadline; at every hold of an SP with a side on a dead disk node, that of
  a leg AR8 parked there included, until the leg is deleted (RW14); and at a
  hold whose fan-out restarted a side while its old child was inside a
  `Syncup*` that outlasts the hold (RW1).

Bitmap pushes:

* `bitmap pushed` with, in this order, `kind` (migr or clone), the object's
  ids, the `migr_id` or `clone_id`, `src_slice_idx` (always zero for a
  migration), `bm_idx` and `code` (BM3);
* `bitmap push failed` with the `bitmap pushed` attributes up to `bm_idx`
  (both indexes zero for a push that never reached a chunk), then either
  `error` (transport, fetch, "chunk not found") or `code` and `details`, the
  agent's own explanation, which the `bitmap pushed` record cannot carry
  (BM6). The record is non-normative — it names no decision — and exists
  because a push that produced no `AgentReply` has no code to report and must
  not be logged as a `bitmap pushed` with an invented zero.

Reactions and drains:

* `reaction applied` with `cluster_id`, `sp_id`, `kind` (`failover`,
  `grow_data`, `grow_meta`, `replace_cntlr`, `spare_create` or
  `spare_switch`), the ids and `revision` (AR2);
* `reaction skipped` with `cluster_id`, `sp_id`, `kind`, `reason`, the ids and
  "error?", the error accompanying the reason `op_failed` (AR2);
* `sp drain step` with `cluster_id`, `sp_id`, `sp_name` and `phase` — `cntlrs`
  with `cntlr_cnt`, or `slice` with `slice_id`, `grp_cnt` and `slice_done` —
  on every committed D1 and D2 step; D3 logs `sp drained` with `cluster_id`,
  `sp_id` and `sp_name` instead (SPD12), so no step record carries a final
  phase. The step record is non-normative — it names no decision — but the
  worker suite counts it: it is the only record that shows a multi-batch drain
  advancing;
* `sp drain failed` with `cluster_id`, `sp_id`, `sp_name`, `phase` (`cntlrs`,
  `slice` or `final`), `slice_id` for a slice step, "reason?" (an
  `ErrPrecondition`'s) and `error`, on a drain step that did not commit; it is
  retried on the next tick, and there is no terminal-failure state (SPD6);
* `clone drain step` with `cluster_id`, `sp_id`, `clone_name`, `clone_id`,
  `step` (`bitmap`) and `chunk_cnt`, the size of the batch (CLD8; the `Clone`
  record carries no chunk count), on every committed batch; the final STM logs
  `clone drained` with `cluster_id`, `sp_id`, `clone_name` and `clone_id`
  instead (CLD9), so no step record carries a final step. Like the sp drain's,
  the step record is non-normative, and the worker suite counts it;
* `clone drain failed` with `cluster_id`, `sp_id`, `sp_name`, `step` (`bitmap`
  or `final`), `clone_name`, `clone_id`, "reason?" and `error`, on a
  clone-drain step that did not commit (CLD10).

## Integration test plan

**What the suite proves.** `integtest/worker_test.sh` proves a real
`dnv-worker` fleet — the real `worker`, `model` and `etcdutil` packages over a
real single-node etcd — against fake dn and cn agents. The agents are fake
because the data plane is proven by the agent suites (`dnagent_integtest.md`,
Goal and scope; `cnagent_integtest.md`, Goal and scope); what this suite
proves is everything between etcd and the agent's gRPC surface: membership and
ownership, revision propagation, health bookkeeping, the flips, the pushes,
the reactions, the two drains and handoff — the happy paths plus every failure
the worker is specified to handle; error paths of etcd itself (quorum loss,
compaction races) are out of scope. It runs on one server as a plain user,
because nothing it exercises needs root: real etcd, three real workers, fake
agents driven by a behavior file, etcd driven by a fake-gateway CLI, and
assertions over the JSON logs.

**Topology.** The developer machine drives one Linux server over ssh, as a
plain user with no sudo. The server runs one etcd on localhost, configured the
way every etcd serving dnv must be (its transaction-op cap at `EtcdMaxTxnOps`,
which the suite reads from the driver it has just built instead of copying the
number), three workers carrying all three roles (the vote case starts and
stops a fourth), four fake DNs and three fake CNs listening on the server's
own address, so that every stored `addr_port` looks like a production one and
the worker dials a real interface, and `workerctl`, run on the server against
etcd. The vote timers are shortened through the CM1 flags and the intervals
and thresholds through each cluster's and pool's stored conf, so every
membership change and every reaction completes in seconds. The driver reads
every worker and fake log over ssh and asserts on it; a fake's gRPC records
carry each request's trace id, whose seed prefix names the worker that sent it
(RW10).

**Cases.** Each case runs in its own cluster after a restart of the worker
fleet that also deletes every dnv key from etcd, resets every fake and
truncates every log, so that neither membership state nor an earlier case's
objects leak into it: an earlier case's cluster left in etcd would stay
driven and write into this case's logs, and a fake still holding an earlier
case's revision would refuse this case's first syncup as stale, the
revision gate being per object and not per cluster.

* smoke — one worker alone: the full-state fan-out, the Check rounds, the
  provisioned flip and the re-fan its bump causes, the created flip, clean
  health;
* revision — bumps, an endpoint moved without a delete, stale and
  unknown-object replies, a deleted rev key;
* health — `err_epoch` set and cleared together with the capacity keys; a hung
  stream, a killed and restarted agent, provisioning rows, and the rules of
  the three sp records;
* bitmap — ordered one-in-flight pushes and their targets, an append, a grown
  clone chunk, a rejected syncup that blocks the diff, arms nothing and is
  re-driven only by the `Check*` reply's own code, and a primary change;
* reaction — failover of an unhealthy and of a disabled primary, cntlr
  replacement including the sole primary, the `CdcEntry` a replacement
  rewrites (read back as exactly the transports of the SP's cntlrs after it:
  the replaced cntlr's gone, its successor's in), data and meta auto-grow
  with the pending rule, leg repair in both cases, a full spare list,
  suppression, and the settle with a settling primary held to
  `cntlr_unhealthy`;
* drain — the sp drain by the real coordinator (one D1, one D2 batch per
  slice, D3, every ledger restored), its resume from the `SpConf` alone after
  an in-case stop and restart of the workers that keeps etcd's keys, and its
  `MaxDelGrpPerTxn` batch bound; and a multi-batch clone drain, its exclusion
  seen from the CN, and its resume from the surviving chunk keys after the
  same kind of restart;
* vote — exact single ownership, a join that moves about a quarter of the
  shards and leaves the rest where they were, a crash, a graceful stop, a stop
  and continue that ends in a self-fence, and attribution by trace id;
* handoff — a killed owner's shards re-driven at the same revision with no
  handover state, and a flip in the middle of a handoff applied exactly once.

**Rules exercised.** The vote and handoff cases exercise VW1 to VW7 and VW9 to
VW11, the vote case VW8 (a), and every case's fleet restart VW7, RW11 and CM5;
SW1 to SW3, SW5 and SW6 are exercised by the smoke, revision and handoff
cases; RW1 to RW12 by the smoke, revision and health cases, RW10 by the
attribution of the vote and handoff cases; RW13 to RW21 by the smoke case's
request builders, the revision case's moved endpoint, the bitmap case's
migration confs and the reaction case's grow and spare confs; HL1 to HL6 by
the health case and HL2's settle by the reaction case; BM1 to BM5 by the
bitmap case; AR1 to AR9 by the reaction case; SPD6 to SPD13, CLD5 and CLD7 to
CLD12 by the drain case, whose `workerctl` latches only stand in for the
gateway's SPD3, SPD4, CLD1, CLD3 and CLD4; MD2 to MD6, EU1 to EU6 and CM1 to
CM6 by every case, every launch and the driver; and the log records by every
assertion. Left to the unit tests: VW8 (b) and (c), because a broken watch
with live puts cannot be induced from outside the process; SW4, compaction;
BM6, because the one lever the suite has over a reply code, a forced code,
rejects the object's `Syncup*` itself, so no push is attempted, let alone
failed; AR5's and AR7's refusals (HL2's row classes, and the rows of the last
failover or primary replacement), since the reaction case only keeps clear of
AR5's second refusal and fails its sole primary on a row of its own; the
tripwires of SPD1 and SPD14 and the guards of SPD2; and the guards of CLD2 and
the gateway halves of CLD1 and CLD3, the gateway suite owning the clone latch.
The suite never produces `invalid stored conf`, because its driver resolves
every conf it writes, and the three conf gates are unit-tested instead. Five
records no case stages: `sp drain failed` and `clone drain failed`, which the
drain case asserts at zero after every drain; `cluster conf missing`, which
every case avoids by writing its cluster conf before any rev key and no case
asserts either way; `syncup leftover`, which no fake produces on its own,
since no fake computes a sweep verdict and no case forces that code; and
`bitmap push failed`, which the forced-code lever cannot stage either.

Out of scope: real agents; more than one server; a three-member etcd; etcd
quorum loss, restore or compaction races; clock-skew injection, clock
agreement across workers being no correctness dependency (VW4); TLS and
authentication, the fabric being trusted
(`architecture.md`, v1 assumptions and known limits); failures of `RedundNone`
legs, for which nothing automatic is specified; and a clone push to a primary
whose build is still deferred. Two things the clone drain's design asks for
are out of reach of every suite in the tree: a host IO probe of a clone's
destination namespace right after the latch, which would need a real host and
a real CN, so CLD4's resume is asserted at the etcd level only, and only by
the gateway suite; and an observation that the coordinator makes no agent call
during a drain (CLD10).

**What a pass means.** Exit status zero means every case's assertions held,
each against the state of its own cluster, in a run that stops at the first
failure with a non-zero status. Every wait is a bounded poll on a condition,
never a bare sleep, except the deliberate "nothing must happen for this long"
negatives; every counting helper, and every read a negative rests on, fails on
a failed read instead of answering zero, because a failed read taken for zero
would pass every negative built on it.

**Cleanup.** Cleanup runs at the start of every run and at its end only on
success: a failing run leaves etcd's data, every log and every behavior file
in place and prints its diagnostics — the failing stage and its trace id,
the registrations, the ownership table, the tail of every log and the dnv
keys — so the debris is what the developer reads. Cleanup signals every
process the suite started, continuing a stopped worker first so that it
acts on its termination, and removes the suite's work directory; nothing
outside that directory is touched, and the suite leaves no kernel state, no
packages and no users behind.

**The driver, `workerctl`.** `workerctl` is the etcd driver that plays the
gateway: it formats keys through `model`, marshals the protos, bumps revisions
and reads keys back as protojson; it never dials an agent and never sleeps. It
is the gateway's write path with explicit placement, its resolve-at-write
included, because the worker refuses a conf nobody made concrete (RW9, RW14,
AR1): the cluster conf and the pool's `bdev_conf` it writes go through
`model.ResolveClusterConf` and `model.ResolveBdevConf`, and its rewrite of a
pool's low water mark maps a zero to `DefaultPoolLowWatermarkPct` itself.
Where the gateway's writes carry more than the record, its own are raw — its
cntlr rewrite sets no settle (HL2), though its pool create, which builds its
own `Cntlr` records, writes the primary settling itself, as
`CreateStoragePool` does; and its stand-ins for the two latches apply neither
the public preconditions nor the clone latch's destination resume, because
those gates are the gateway's and re-implementing a public precondition in a
driver is how the two drift apart — so a case plants exactly the state it
means to. Its drain loops are the gateway suite's stand-ins for the worker;
this suite uses them only to build a partly drained SP or clone for the resume
steps. Its `constants` and `geometry` subcommands open no etcd and run on the
driver, so that a shell suite reads the Go constants it sizes itself by,
`EtcdMaxTxnOps` among them, and the group geometry `GrowSlice` computes,
instead of copying either.

**The fake agent, `fakeagent`.** `fakeagent` serves the generated
`DiskNodeAgent` and `ControllerNodeAgent` services with the real server
interceptors, so its log records every message the worker sent under the
caller's trace id — the suite's evidence of what was sent and by which
worker. It applies the agents' revision gate per object and their ordering
rule — a side or cntlr its node's last syncup does not list is an unknown
object, which the worker's independent roles must survive (RW5) — refuses a
push for a migration or clone its object's last request does not name, and
derives every `*Info` row from the last accepted request. It persists its
last accepted requests and the chunks it received, so a killed and restarted
fake replies like a restarted agent. A behavior file per fake, re-read
whenever it changes, sets per object the row statuses — a cntlr's row
optionally only while that cntlr is primary, because the fake reports a
standby's pool and md rows, which the real cn agent reports for a primary
only — the zeroing progress, the thin rows, the applied chunk sets, a hung
or dropped stream and a forced reply code. It computes no sweep verdict, so
it never answers `ReplyCodeLeftover` on its own.

## Known limits

* **A partitioned observer is tolerated, not repaired.** An observer whose
  watch is broken while its puts succeed is caught by VW8 (b) only if its own
  echoes stop too; a watch that delivers its own echoes but drops a peer's is
  not distinguishable from that peer dying. The wrong commit deletes the
  peer's key; the peer sees it (VW8 (c)), fences and rejoins as a fresh
  identity; in between, both drive the same shards — bounded by the grace
  window plus one round, harmless to agents (idempotent syncups), but not
  prevented. In etcd every reaction is STM-guarded — save a second owner's
  spare create that lands after RW18 has flipped the first owner's spare,
  which can leave the group a spare no failure asked for (AR2, and the limit
  below on a spare whose DN fails while it zeroes) — but a health epoch is a
  reaction input, and one the other driver left stale is corrected by the
  owner's first verdict after it next loads the record (HL3): within a pass
  and a round for a side, a leg or a cntlr — though at the default thresholds
  a pass can fail a primary over on it before that verdict lands, as it can
  after one bad round (below) — and within a `nodeRecordMaxAge` and a round
  for a DN or a CN, a stamped node being out of allocation meanwhile (MD4).
* **Overlap and gap at every ownership change** (VW7): a few seconds of double
  driving or of no driving per shard per change. Accepted.
* **Undriven windows**: a fresh worker's first grace window; twice the vote
  interval plus the grace time after a crash; the grace time after a graceful
  stop or after a fence — both of which delete their registrations without
  waiting for their drain (CM5, VW8), so the window does not wait out the
  drain's in-flight calls. Revision keys are durable, so nothing is lost:
  convergence is delayed, not skipped.
* **The grown-chunk memo of BM5** (`architecture.md`, [D8]) is per worker; a
  handoff can leave a shorter chunk at the agent (cost: extra copying, never
  correctness).
* **A group's spare list can fill with parked legs** (AR8 step 4); the worker
  never frees a slot itself.
* **A slice whose data list holds `MaxGrpCntPerSlice` groups never grows its
  data again** (AR6): every pass that finds the pool's data usage above
  `low_water_mark_pct`, with no data grow pending, logs `reaction skipped`
  (`grp_list_full`) and goes on (AR2). Nothing frees the list, and the pool
  can run out of data space (`architecture.md`, Automatic reactions;
  `architecture.md`, v1 assumptions and known limits).
* **A spare that never connects holds its own group's repair for
  `leg_unhealthy`.** AR8 step 2 waits for a pending spare, and a spare whose
  leg keeps reading `ERROR` stays pending until it has been unhealthy that
  long — each replacement spare the same again, until `spare_list_full`. A
  parked leg is such a spare again once its DN comes back or once it recovers
  and fails again (step 2). The wait holds that group only; the scan repairs
  the others (AR2). A spare the primary never reports at all stays pending
  with no bound.
* **A spare whose DN fails while it zeroes blocks its group's next spare until
  that DN finishes zeroing it or an operator deletes it.** `CreateSpareLeg`
  refuses while any spare of the group has an unprovisioned side (MD6), so AR8
  step 3 cannot replace it; the pass holds that group only
  (`spare_unprovisioned`, AR2). The same refusal fails a second owner's create
  for one repair only until RW18 flips the first owner's spare: nothing
  records which repair a spare was made for, so a second owner whose create
  lands after that flip — on a group that held no spare before the first
  create, and on a DN other than the first owner's — leaves the group a spare
  no failure asked for.
* **A primary that never settles is failed over or replaced no earlier than
  `cntlr_unhealthy`**, unless it is disabled (AR5, HL2) — nor failed over
  before `primary_unhealthy`, AR5 taking the longer of the two. A primary that
  never reports its stack built and clean as primary — a new primary that is
  itself dead, a build that keeps it unhealthy longer than that, a new SP
  whose first zeroing never finishes, which keeps its pools `PROVISIONING`, a
  migration destination that `FinishMigration` forced before its RW18 flip and
  that never finishes zeroing while it is the only side of a leg in the first
  group of its list, which keeps that slice's pools and every td
  `PROVISIONING` the same way, or a clone whose source stays unconnected,
  which keeps its target row `MISSING` (HL2) — keeps the SP on it that long
  once it is unhealthy, where a settled primary is judged by
  `primary_unhealthy` alone; an old primary that recovered meanwhile gets the
  role back only then, and not while the new primary fails only on rows it
  failed on (AR5's same error). A *settled* primary can still be failed over
  on one bad round at the defaults, the default `primary_unhealthy` being no
  longer than the default `cntlr_interval`; such a failover costs one settle
  rather than a loop.
* **Sides first holds every cntlr request** (RW14): a side that does not
  report — a dead disk node, a new side its disk node refuses until the dn
  role's `SyncupDn` has introduced it (`dnagent.md` DN8), or a side the
  fan-out restarted whose old child is still inside a `Syncup*` (RW1) — delays
  every cntlr request of its SP's fan-out by up to one `cntlr_interval`, which
  RW9 lets be as long as `MaxHealthCheckInterval`, a failover's promotion and
  demotion included. A dead disk node's share of it outlives AR8's repair,
  because the leg AR8 parks there keeps its side in `spare_leg_list` and that
  side is waited for like any other, until the disk node returns or
  `DeleteSpareLeg` removes the leg. A failed-over primary that is still
  serving is fenced before its demotion is sent (`architecture.md`, Failover,
  and `architecture.md`, [D16]). The leg-removal race between a CN's
  disconnect and the disk node's unlink of the side's export is narrowed, not
  closed, because the unlink rides the dn role's `SyncupDn`, which the hold
  does not wait for (`cnagent.md`, Known limits); the sp drain's twin of that
  race, which no hold orders because the drain waits on no agent (SPD13), is
  kept by decision; and a migration destination's `SyncupSide` at
  `FinishMigration`, whose disconnect of the source connection the dn sweep
  runs under the side's lock, races the source disk node's `SyncupDn`, which
  no hold orders either and which takes that export away (`dnagent.md`, Known
  limits).
* **A lost thin id costs a failover or a replacement, and the second refusals
  that bound them are a memory.** A report names a created td's lost thin id
  only when it is a converge's; a Check round's probe reads the absent volume
  `MISSING` and names no id (HL2), so a primary whose latest report is a
  probe's is failed over, and the second refusal then keeps the role from
  going back to it for as long as the new primary fails only on rows it failed
  on (AR5) — in an SP of two cntlrs, one failover in all, and one more each
  time a new primary also fails a row of its own; in a larger one the role can
  first move on to a cntlr that has not held it. That refusal rests on the
  coordinator's record of the last failover it applied: a coordinator after a
  restart or a shard handoff, or the other owner of an overlap (VW7), has
  none, and can fail over once more before it has. It compares rows, not
  causes: a new primary whose own fault fails only rows the old primary failed
  on is held as well, for as long as that fault lasts or until an operator
  moves the role, every pass logging why. AR7's sole-primary variant takes the
  twins of both refusals, judged on the same report (AR7). The first holds the
  primary of an SP with no failover candidate only while the report the
  coordinator holds for it is a converge's that names the lost id, which lasts
  until a Check reply carries an info — as one does as soon as the probe's
  view differs from the last one its stream sent, and on a fresh stream's
  first probe (HL5; `architecture.md`, Check streams) — or a dead stream marks
  its rows `UNKNOWN` (HL1): after a re-sync over a view the stream had already
  sent — as when an agent restart left it a revision behind — it lasts until
  that view changes or the stream is replaced, as by a round timeout, a broken
  stream, a desired change that overtakes a round (RW6), or a worker restart
  or shard handoff, whose new child holds no report until a reply carries one.
  It does not break the replacement loop on its own: a replacement's first
  probe after its converge goes out on a stream that has sent no info yet, so
  the report held for it is a probe's, which names no id. The second does:
  once a primary whose held report is a probe's has been replaced at
  `cntlr_unhealthy`, by a cntlr that reads the same rows, that replacement is
  not replaced while it fails only on rows the primary it replaced failed on,
  from an error set within `cntlr_unhealthy` of that replacement — one
  replacement, rather than one per `cntlr_unhealthy` for as long as the td
  stays lost, each moving the whole stack of an SP with one cntlr to another
  CN; one more each time a replacement also fails a row of its own. That
  refusal rests on the coordinator's record of the last replacement of a
  primary it applied, as AR5's second rests on its record of the last
  failover: a coordinator after a restart or a shard handoff, or the other
  owner of an overlap, has none, and can replace the replacement once more
  before it has. Neither record holds anything when the reaction it records
  was taken over a report with no `ERROR` row — a primary read unreachable,
  whose rows a dead stream has marked `UNKNOWN` (HL1), or one whose child has
  not reported yet, as a coordinator's first pass after a restart or a handoff
  can find it — so the reaction after such a one is not held either: a
  replacement read unreachable costs two replacements more, a restart or a
  handoff can cost two where it costs one, and after a failover over such a
  report the role can be handed back once more. So the replacements a lost
  thin id costs are bounded to one per coordinator that drives the SP, those
  exceptions aside. It compares rows, not causes: a replacement whose own
  fault fails only rows the old primary failed on is held as well, for as long
  as that fault lasts, every pass logging why.
* **`RedundNone` legs have no automatic repair**: no spare can exist, and the
  migration that could have moved a readable-but-sick side is an operator's
  tool (`CreateMigration`), not a reaction (Automatic reactions).
* **Reaction inputs are one pass old**: `err_epoch`s are read fresh each pass,
  but pool usage and spare readiness come from the primary's last Check reply,
  at most one interval old. A reaction is at most one interval late; never
  wrong, because the op re-validates. That does not hold across a promotion,
  since no op can re-validate a probe: from the moment RW14's release hands a
  failover's promotion to the new primary's child until that primary's reply
  to it, its last reply is still its standby's report, whose leg row for a
  spare is transport liveness and ANA state, not the health-block probe, and a
  pass in that window can switch the spare in on it (AR8 step 1). The pass
  reads no info while the hold keeps the promotion from the child (AR1), so
  the window is the promotion's own round trip. Nor does it hold for an
  `err_epoch` another driver left stale, since the op re-validates against
  that very record (the partitioned-observer limit above, HL3).
* **Log volume**: every round logs a client send and receive record pair per
  object (`grpc.md`, L6) and every heartbeat an `etcd put`; with thousands of
  objects this is the dominant log stream of the control plane, as it is for
  the agents. A DN or CN that neither syncs nor changes health adds an
  `etcd get` per `nodeRecordMaxAge`, HL3's re-read of its record, and one at
  each verdict while that read fails. The worker's own per-object records are
  mostly event-driven: health transitions (`health changed`, the `unreachable`
  reason included — the `RES_STATUS_UNKNOWN` marking itself is in-memory and
  logs nothing, HL1), a `syncup result` per `Syncup*`, and a cntlr's
  `cntlr settled`, about once per acquisition of the primary role. Among those
  that recur every round for as long as their cause lasts: an unreachable
  object's `check stream open failed`, `check stream send failed` or
  `check round failed` (the revision loop's own records, which the Log records
  above do not list, and whose names are not normative); the `syncup result`
  of each `Syncup*` that RW4 step 5 re-issues, paired with a `syncup leftover`
  or a `syncup rejected` while its reply still carries the leftover or the
  rejection (RW5; a leftover only the `Check*` verdict still names is paired
  with neither) — a stale revision after an etcd restore repeats its
  `syncup rejected` at Error every round until the revision in etcd reaches
  the agent's; a `dn conf missing` or `cn conf missing` while the node's
  `DnConf` or `CnConf` is absent (RW13); and a `health write failed` while a
  health write keeps failing (RW12). The sp coordinator's pass adds its own,
  once per pass rather than per object — AR5's `reaction skipped` with
  `no candidate`, say, for as long as a primary due for failover has no
  eligible candidate, or `shared_state` or `same_error` for as long as the
  error that holds one lasts, and AR7's `shared_state` or `same_error` once
  the primary it holds is due for replacement — beside AR5's, or alone while a
  `primary_unhealthy` longer than `cntlr_unhealthy` has not yet run out.
