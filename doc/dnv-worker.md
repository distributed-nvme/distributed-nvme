# dnv-worker.md — `dnv-worker`: vote, shard and revision workers (dnv)

Status: **normative**. Companion to `architecture.md` (the design: §5 etcd data
model, §9 agent contracts, §10 workers), `layout.md`, `log.md`, `grpc.md`,
`dnagent.md` and `cnagent.md` (the agent side of every RPC the worker issues).
This document is the specification of the packages `worker/`, `model/` (new,
§4) and `etcdutil/` (§3), of the binary `cmd/dnv-worker` (§5), and of the
worker's integration-test suite (§14). Where it differs from the pre-existing
text of `architecture.md` §10.1 and §10.4, this document wins; §15 records
the amendments, all applied.

Conventions:

* `{p}` is the etcd key prefix `DnvPrefix` = `"dnv"`; ids render with
  `IdKeyFmt = "%016x"`, shard codes with `ShardCodeFmt = "%02x"`. Keys are
  space-joined (`architecture.md` §5.1); a *prefix* always ends in one space.
* "STM" is the `clientv3/concurrency` STM as in `architecture.md` §5.8.
* Rule ids are append-only once cited from code: `VW` vote worker (§6), `SW`
  shard worker (§7), `RW` revision worker (§8), `HL` health (§9), `BM` bitmap
  pushes (§10), `AR` automatic reactions (§11), `EU` etcdutil (§3), `MD`
  model (§4), `CM` cmd (§5), `LG` log records (§12).
* MUST / SHOULD in the RFC sense. Seconds unless stated.

Terminology used throughout:

| term | meaning |
|---|---|
| **role** | one of `dn`, `cn`, `sp` (`WorkerRoleDn`/`WorkerRoleCn`/`WorkerRoleSp`). A process carries one to three roles; each role is voted, sharded and driven independently. |
| **seed** | the v4 uuid that identifies one *incarnation* of a worker process's vote layer (§6.1). One seed per incarnation, shared by all its roles. |
| **registration** | the key `{p} worker {role} {seed}`, value `WorkerReg{epoch}`, refreshed every `DefaultVoteWorkerInterval` (§6.2). |
| **observed state** | *live* or *dead*, as judged by one observer for one registration (§6.3). |
| **effective membership** | per role, the set of registrations an observer has *committed* after a full grace window (§6.4). Ownership is computed from it, never from the raw observed set. |
| **ticket** | `sha256("{seed}-{role}-{shard}")`; the effective member with the largest ticket owns the shard (§6.6). |
| **vote worker** | the per-process layer that registers, observes, commits, computes ownership and starts/stops shard workers (§6). |
| **shard worker** | per owned `(role, shard)`: watches one rev prefix and keeps one revision worker per key under it (§7). |
| **revision worker** | per rev key `(cluster_id, id)`: pushes revisions to the agent(s) and health-checks them (§8). For `sp` it is a coordinator with one child goroutine per side and per cntlr. |
| **object** | a DN, CN, side or cntlr — the unit of one `Check*` stream and one goroutine (§8.1). |
| **handle** | the mutable part of a rev value: `addr_port` (`DnRev`/`CnRev`) or `sp_name` (`SpRev`) (`architecture.md` §5.5). |
| **round** | one `Check*` request/reply exchange on an object's stream (§8.1). |
| **pass** | one evaluation of the automatic reactions for one SP (§11.1). |

---

## 0. Decision record

The decisions behind this document, each with its rule. `architecture.md`
Appendix B carries the cross-component one as **[D17]**.

1. **Heartbeat registrations, no etcd lease** ([D17], §6.2). A worker re-puts
   `{p} worker {role} {seed}` every `DefaultVoteWorkerInterval` = 10 s; a
   registration not refreshed for 2 × interval is dead.
2. **Liveness is judged on the observer's own monotonic clock** (VW3, VW4),
   never by comparing the stored `epoch` with local time. The `epoch` is
   informational. NTP remains an operational assumption but not a
   correctness dependency.
3. **Per-registration grace timers** (VW5): every observed transition of one
   registration — appear, disappear, reappear — starts that registration's
   own `DefaultVoteWorkerGraceTime` = 60 s timer; the change is committed
   only if it still holds when the timer fires. A flapping worker never
   becomes effective and never blocks others.
4. **Symmetric** (VW7): a worker applies the same rule to its own
   registration, so it drives nothing during its first grace window, and
   old and new owners switch within seconds of each other. The residual
   overlap/gap is accepted: every `Syncup*` is idempotent under the agents'
   revision gate and every etcd reaction is STM-guarded.
5. **Self-fence** (VW8): a worker whose own heartbeat has not reached etcd
   for 2 × interval, or is not echoed back by its own watch, or whose key is
   deleted by a peer, stops driving everything and rejoins as a **fresh
   identity** (new seed) — one code path for every "the fleet gave up on me"
   case.
6. **Dead keys are garbage-collected by whoever commits them dead** (VW6);
   without a lease nothing else would expire them.
7. **Tickets**: `sha256(fmt.Sprintf("%s-%s-%02x", seed, role, shard))`,
   32 raw bytes compared with `bytes.Compare`, largest wins (VW9).
8. **One goroutine, one `Check*` stream, one round timer per object** (RW1);
   the round is the retry cadence — there is no separate backoff.
9. **Process-wide `ClusterConf` cache** fed by one watch (§8.5); intervals,
   `extent_size` and `qos_ratio` are read from it every round.
10. **A new shared package `model`** (§4) holds the §5 etcd data model as
    Go: key formats, `cluster_id`, capacity keys, the §6 allocator and the
    internal §8/§10.4 mutations, so the gateway later builds on the same
    code. `layout.md` §3 is amended accordingly.
11. **`etcdutil` is specified here** (§3) — the worker is its first consumer.
12. **Four automatic reactions**: failover, thin-pool auto-grow, cntlr
    replacement, leg repair (§11). The §10.4 `side_unhealthy` *migration* is
    withdrawn: every condition that sets `Side.err_epoch` also prevents the
    source DN from serving the migration. Instead `side_unhealthy` and
    `leg_unhealthy` are the two triggers of one **leg repair** procedure
    (create spare → switch; old leg parked in `spare_leg_list`) — the
    shorter wait when the DN itself looks dead, the longer one when only
    the cntlr's path is bad.
13. **One reaction per SP per pass**, in a fixed priority, every reaction
    re-validating its preconditions inside its STM (AR2).
14. **Stateless auto-grow pending rule** (AR6): a grow is pending while the
    pool's reported total is still no larger than the total implied by the
    slice's groups *before* the newest one — a memo reconstructed from facts.
15. **Disabled cntlrs are hands-off** (AR3): never failed over to, never
    replaced — but a disabled *primary* is itself the AR5 failover trigger
    (§8.6). Reactions are suppressed for `deleting` SPs and at
    `sp_level ≥ SP_LEVEL_NO_THINPOOL`.
16. **Sole-cntlr SPs are repaired** (AR7): a primary with no failover
    candidate is replaced by a new primary on a fresh CN with the same
    `cntlid_slot`.
17. **Spare-list-full is an operator event** (AR8): the worker never deletes
    a parked leg.
18. **Trace ids carry the worker's identity** (RW10): `"{seed[:8]}-{NewTraceId()}"`,
    so an agent's log attributes every request to a worker.
19. **The integration suite runs on one server as a plain user** (§14): real
    etcd, three real workers, fake agents driven by a behavior file, etcd
    driven by a fake-gateway CLI, assertions over the JSON logs.

---

## 1. Scope and placement

`dnv-worker` is the control-plane process that converges the data plane to
the desired state in etcd (`architecture.md` §1): it watches the revision
keys, shards the work by shard code, drives agents through the unary
`SyncupDn`/`SyncupSide`/`SyncupCn`/`SyncupCntlr` and `Push*Bitmap` calls,
watches them through the `Check*` streams, maintains the health fields and
capacity keys, flips `Side.provisioned` and `ThinDevice.created`, and
performs the automatic reactions. It never serves gRPC, never calls
`Get*Info`, and never talks to hosts.

Three layers per role, one process:

```
dnv-worker process  (--roles dn,cn,sp; one seed per incarnation)
└── vote worker (§6) — registers the seed under every role prefix, observes the
    registries, commits membership after the grace window, computes ownership
    ├── role dn: owned shards ──▶ shard worker per shard (§7): watches {p} dn_rev {s}␠
    │       └── revision worker per DN (§8.2): one goroutine, one CheckDn stream,
    │           SyncupDn on every revision, DnConf.err_epoch + DnCapacity (§9)
    ├── role cn: … {p} cn_rev {s}␠ ──▶ per CN: CheckCn / SyncupCn (§8.3)
    └── role sp: … {p} sp_rev {s}␠ ──▶ per SP (§8.4): snapshot + fan-out coordinator
            ├── per side:  goroutine, CheckSide stream, SyncupSide, PushMigrBitmap (§10)
            ├── per cntlr: goroutine, CheckCntlr stream, SyncupCntlr, PushCloneBitmap
            └── flips (provisioned, created), sp health (§9), reactions (§11)
```

Packages and files (`layout.md` §2/§3 as amended by §15):

| package | files | may import (internal) |
|---|---|---|
| `etcdutil` | `etcdutil.go` | `common` — takes `proto.Message`, never imports `pb` |
| `model` | `keys.go`, `stm.go`, `capacity.go`, `alloc.go`, `ops.go` | `common`, `pb`, `etcdutil` |
| `worker` | `worker.go` (`Run`/`Config` and the worker-lifecycle §12 msg constants; the flip/bitmap/reaction msgs live beside their emitters in `sprole.go`/`bmpush.go`/`reaction.go`), `vote.go`, `shard.go`, `revision.go`, `conn.go` (the RW7 connection cache), `dnrole.go`, `cnrole.go`, `sprole.go`, `clusterconf.go`, `health.go`, `bmpush.go`, `reaction.go` | `common`, `pb`, `etcdutil`, `model` |
| `cmd/dnv-worker` | `main.go` | `worker`, `common` (+ cobra, viper) |

Out of scope here: the gateway (request validation, the public RPCs, the
`Inspect*`/`Get*Size`/`Get*Bm` agent calls), `dnv-cdc`, `dnvctl`. Where the
worker performs an "internal" variant of a gateway mutation (§11), the STM
body lives in `model` so the gateway later adds only validation and reply
mapping on top (MD8).

Reading order for an implementer: §2 (constants) → §3/§4 (the two
libraries) → §6 → §7 → §8 → §9-§11 → §5 → §12 → §13/§14.

---

## 2. Constants and schema

### 2.1 Additions to `common/constants.go`

```go
// dnv-worker (dnv-worker.md §2.1).
const (
	// Seconds between two refreshes of a worker's registry key (VW2); a
	// registration not refreshed for 2 × this is dead (VW3).
	DefaultVoteWorkerInterval = 10
	// Seconds an observed membership transition must hold before it is
	// committed into the effective membership (VW5).
	DefaultVoteWorkerGraceTime = 60
	// Per-call deadlines of the worker's agent RPCs (RW5, BM3).
	DefaultWorkerSyncupTimeout = 60
	DefaultWorkerPushTimeout   = 60
	// etcd client: dial, and per plain operation / per whole
	// transaction, every retry included (EU1, EU5).
	DefaultEtcdDialTimeout = 5
	DefaultEtcdOpTimeout   = 10

	WorkerRoleDn = "dn"
	WorkerRoleCn = "cn"
	WorkerRoleSp = "sp"
)
```

Derived, never a constant: the **dead threshold** = 2 × the configured vote
interval; the **round timeout** = the object kind's health-check interval
(§8.1). The existing `DefaultHealthCheckInterval` = 5 (bounds 1..3600),
`DefaultDnExtSize`, `DefaultPoolLowWatermarkPct` and the four
`Default*Unhealthy` thresholds are read through the §8.5 cache and §11.4.

### 2.2 `pb/schema.proto` — one added message (applied at implementation time)

```proto
// {dnv_prefix} worker {role} {seed}
message WorkerReg {
    // The writer's unix seconds at the put. Informational: liveness is judged
    // by the observer's own clock since the put it last saw (dnv-worker.md
    // VW3/VW4), never by comparing this with local time.
    uint64 epoch = 1;
}
```

Placed after `SpName`, the last stored message. The edit is deliberately
**not** applied together with this document: a proto change without `make
gen` would leave the committed generated code out of sync, and the pinned
toolchain (README: protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2) is
what regenerates `schema.pb.go`/`schema_grpc.pb.go` in the same change.
Nothing else in the schema changes; in particular `Migration` gains no
field, because the §10.4 migration reaction is withdrawn (§0 item 12).

---

## 3. Package `etcdutil` [EU]

`log.md` §5.3 fixes the logging obligations and forbids direct `clientv3`
calls elsewhere; `layout.md` §2 places the package. The worker is its first
consumer; the gateway and `dnv-cdc` extend it later without changing what is
specified here.

EU1. **Client.** `New(ctx, endpoints []string, dialTimeout time.Duration)
     (*Client, error)` wraps one `clientv3.Client` (`go.etcd.io/etcd/client/v3`,
     v3.6 line). No dnv gRPC interceptor is attached to it (`grpc.md` §1 —
     etcd logging is done by these helpers, not by message interceptors).
     `Close()` releases it. A failure to reach any endpoint at construction is
     **not** fatal to the caller by itself: the client retries in the
     background; the worker's vote layer stays fenced until its first
     successful put (VW8).

EU2. **Typed plain operations.** All take a ctx, log per the `log.md` §5.3
     table with `slog.InfoContext`, and (un)marshal binary protobuf:

     | function | semantics | record |
     |---|---|---|
     | `Get(ctx, key, msg proto.Message) (found bool, err error)` | point read; `msg` untouched when not found | `etcd get` — `key`, `found`, `value` (decoded, only when found), `error?` |
     | `Put(ctx, key, msg)` | write | `etcd put` — `key`, `value`, `error?` |
     | `Delete(ctx, key)` | delete; deleting an absent key is not an error | `etcd delete` — `key`, `error?` |
     | `Range(ctx, prefix) ([]KV, rev int64, err)` | prefix scan, `KV{Key string; Value []byte}` in key order, `rev` = the store revision the scan was served at | `etcd range` — `prefix`, `count`, `error?` (values are **not** dumped) |
     | `RangeDesc(ctx, prefix, limit int64) ([]KV, rev, err)` | prefix scan in **descending** key order, at most `limit` keys (`≤ 0` ⇒ no limit): the capacity keys embed `free_ext_cnt` in `FreeSpaceFmt`, so descending key order is largest-free-first, which is the MD5 allocator's walk | `etcd range` — `prefix`, `count` |
     | `RangeKeys(ctx, prefix) ([]KeyRev, rev, err)` | keys-only prefix scan (`clientv3.WithKeysOnly`); `KeyRev{Key string; ModRev int64}` — the key and its `mod_revision`, which BM5 memoizes | `etcd range` — `prefix`, `count` |
     | `RangeKeysAtRev(ctx, prefix, rev) ([]KeyRev, err)` | `RangeKeys` pinned to one store revision (`clientv3.WithRev`), for MD3's bitmap-index scans, which must be served at the revision the SP snapshot was read at; a `rev` the store has compacted away is an error, never silently served from a newer one | `etcd range` — `prefix`, `count` |
     | `Decode(ctx, kv KV, msg) error` | unmarshal one scanned value | `etcd get` — `key`, `found = true`, `value` — so every value a caller actually reads is logged individually, as §5.3 requires |

EU3. **Typed watch.** `WatchTyped(ctx, prefix string, fromRev int64, newMsg
     func() proto.Message) (<-chan Event, <-chan error)` opens one prefix
     watch starting at `fromRev` (`clientv3.WithPrefix`, `WithRev`). Every
     event is delivered as `Event{Type Put|Delete, Key string, Msg
     proto.Message (puts only), Rev int64}` in order, and logged as `etcd
     watch event` with `key`, `type` and, for puts, the decoded `value`. The
     etcd client reconnects transparently; a watch cancelled by the server
     with `ErrCompacted` is reported once on the error channel and both
     channels close — the caller rescans (SW4, VW3), which `IsCompacted(err)
     bool` lets it recognize without importing `rpctypes` itself. When `ctx`
     ends both channels close without an error. There is no untyped watch:
     every dnv prefix holds one message type.

EU4. **STM.** Two runners over `concurrency.NewSTM` with
     `WithIsolation(concurrency.SerializableSnapshot)`:

     * `RunSTM(ctx, func(s STM) error) error` — read/write; the etcd client
       retries on conflict until `ctx` ends or the transaction's EU5 budget
       is exhausted, which in the ordinary case is the earlier of the two —
       a worker's `ctx` lives as long as the worker does.
     * `Snapshot(ctx, func(s STM) error) error` — read-only: all reads are
       served at one store revision; the commit is a no-op. It serves a
       multi-key load that has no use for the revision itself; a load that
       does need it — every one in the worker (MD3, AR1) — goes through
       `SnapshotRev` below.

     A third runner, `SnapshotRev(ctx, func(s STM) error) (rev int64, err
     error)`, is `Snapshot` that also **reports** the store revision its
     reads were served at: `concurrency.STM` pins that revision at its first
     read but keeps it private, and MD3 needs exactly that number to run its
     bitmap scans (`RangeKeysAtRev`) at the snapshot's own revision, so
     `SnapshotRev` pins it itself — first read linearizable, every later read
     `WithRev` — instead of going through `concurrency.NewSTM`. Its callback
     runs exactly once, a read-only load having no write set to conflict on,
     and a `Put`/`Del` inside it is a programming error that fails the load.

     `STM` is the typed view: `Get(key, msg proto.Message) (found bool)`,
     `Put(key, msg)`, `Del(key)`, `Rev(key) int64`. Each call logs the §5.3
     record of its kind (`etcd get` / `etcd put` / `etcd delete`); a retried
     transaction logs twice, which `log.md` accepts. A callback that returns
     `*ErrPrecondition` (MD7) aborts the transaction **without** commit and
     without retry, and `RunSTM` returns that error unchanged. Any callback
     error ends the attempt before the commit txn is issued; what marks one a
     deliberate, non-fatal abort that must reach the caller as it is, is that
     it wraps the sentinel `ErrNoCommit` — which is what
     `model.ErrPrecondition.Unwrap()` returns, so `etcdutil` states the
     contract without importing `model` (`layout.md` §3).

EU5. **Timeouts.** Every plain operation runs under `DefaultEtcdOpTimeout`
     seconds (`--etcd-op-timeout` is deliberately not a flag); a shorter
     caller ctx wins. A transaction is budgeted as a **whole**, every EU4
     conflict retry included, and not per attempt: `concurrency.NewSTM` owns
     its retry loop and fixes its abort ctx at construction, so
     `etcdutil/etcdutil.go` has no way to express a per-attempt deadline, and
     re-running the transaction under a fresh budget would not terminate
     while etcd is unreachable — a budget expiry caused by contention is
     indistinguishable from one caused by a dead etcd. Under sustained
     contention on one key a transaction can therefore exhaust the budget
     across its attempts and fail while the caller's ctx is still alive; for
     the worker that is benign, its retry cadence being its own round (RW12)
     and every mutation re-validating its preconditions on the next pass
     (MD7, AR2), and a caller with no round of its own is left to decide
     what to do with the failure (EU6). A read-only load is budgeted the
     same way, as one operation rather than one per key read. `New` uses
     `DefaultEtcdDialTimeout` unless the caller passes another value
     (`--etcd-dial-timeout`, CM1).

EU6. **Errors.** Connection, timeout and (de)serialization errors are
     returned wrapped, never retried inside the helpers (the STM conflict
     retry of EU4 is the client's, not ours). The caller decides — the worker
     always retries on its own cadence (the next heartbeat, the next round,
     the next pass) and never crashes on an etcd error.

EU7. **Tests.** `etcdutil_test.go` runs against a real `etcd` binary found on
     `PATH` or named by `ETCD_BIN`, started on a free localhost port with a
     temporary data dir, and is skipped (`t.Skip`) otherwise — no dependency
     on the etcd *server* module. Covered: each EU2 record's attributes
     (captured `slog` handler, `log.md` §7 style), `Range`/`RangeKeys` order
     and `rev`, `WatchTyped` ordering and its `ErrCompacted` report after an
     explicit compaction, `RunSTM` conflict retry, `Snapshot` reading one
     revision, `ErrPrecondition` aborting without commit.

---

## 4. Package `model` [MD]

The `architecture.md` §5 etcd data model as Go, shared by the worker now and
the gateway later. `model` imports `common`, `pb`, `etcdutil`; it never dials
an agent, never sleeps, and logs nothing of its own beyond the `etcdutil`
records its reads and writes produce.

MD1. **Files.** `keys.go` (key formats, parsers, `ClusterId`), `stm.go` (the
     SP snapshot loader), `capacity.go` (§5.6), `alloc.go` (§6.3/§6.4),
     `ops.go` (the internal mutations of MD6). Unit tests colocated.

MD2. **Keys.** One function per §5.3 key, one per prefix a component scans or
     watches, and a parser for every key a worker decodes from a watch:

     | message / purpose | function → key |
     |---|---|
     | `ClusterConf` | `ClusterConfKey(name)` → `{p} cluster_conf {name}`; `ClusterConfPrefix()` → `{p} cluster_conf ` |
     | `DnGlobal`/`CnGlobal`/`SpGlobal` | `DnGlobalKey(cid)` … → `{p} dn_global {cid}` … |
     | `DnRev`/`CnRev`/`SpRev` | `DnRevKey(shard, cid, dnId)` → `{p} dn_rev {shard} {cid} {dnId}`; `DnRevPrefix(shard)` → `{p} dn_rev {shard} `; `ParseDnRevKey(key) (shard uint32, cid, dnId uint64, ok bool)` — likewise `Cn*`, `Sp*` |
     | `DnConf`/`CnConf` | `DnConfKey(cid, addrPort)`, `CnConfKey(cid, addrPort)` |
     | `DnCapacity` | `DnCapacityKey(cid, binIdx, freeExt, addrPort)` → `{p} dn_capacity {cid} {bin:%01x} {free:%016x} {addr_port}`; `DnCapacityPrefix(cid, binIdx)` |
     | `CnCapacity` | `CnCapacityKey(cid, freeExt, addrPort)`; `CnCapacityPrefix(cid)` |
     | `CdcEntry` | `CdcEntryKey(cid, shard, spId, ssId)`; `CdcEntryPrefix()`; `ParseCdcEntryKey(key) (cid uint64, shard uint32, spId uint64, ssId uint64, ok bool)` — the one prefix and parser pair a worker never uses: dnv-cdc scans and watches the prefix and decodes the keys off it (`cdc.md` §2.2, §4) |
     | `SpConf`, `SpName` | `SpConfKey(cid, spName)`, `SpNameKey(cid, spId)` |
     | `Cntlr`, `Slice` | `CntlrKey(cid, spId, cntlrId)`, `SliceKey(cid, spId, sliceId)` |
     | `ThinDevice`, `Subsystem` | `ThinDeviceKey(cid, spId, tdName)`, `SubsystemKey(cid, spId, nqn)` |
     | `Clone`, `CloneBitmap` | `CloneKey(cid, spId, name)`, `CloneBitmapKey(cid, spId, name, bmIdx)` (`BmIdxFmt`), `CloneBitmapPrefix(cid, spId, name)`, `ParseBmIdx(key) (uint32, bool)` |
     | `Transfer` | `TransferKey(cid, spId, name)` |
     | `Migration`, `MigrBitmap` | `MigrationKey(cid, spId, name)`, `MigrBitmapKey(...)`, `MigrBitmapPrefix(...)` |
     | `WorkerReg` | `WorkerRegKey(role, seed)` → `{p} worker {role} {seed}`; `WorkerRegPrefix(role)` → `{p} worker {role} `; `ParseWorkerRegKey(key) (role, seed string, ok bool)` |

     `ClusterId(clusterName string, creationEpoch uint64) uint64` is the
     §5.2 function verbatim (fnv64a over the name bytes then the epoch as 8
     big-endian bytes). Formats are exactly §5.1's: `IdKeyFmt` for every id,
     `ShardCodeFmt`, `FreeSpaceFmt`, `BinIdxFmt`, `BmIdxFmt`; a key is the
     fields joined by one space. The unit test pins the §5.1 example
     (`dnv sp_id_to_name ebada5168620c5fe 0000000000000011`).

MD3. **SP snapshot.** `LoadSp(ctx, cli, cid uint64, spName string) (*SpState,
     error)` reads, in **one** `SnapshotRev` (EU4), everything the sp role
     fans out or reacts on:

     ```go
     type SpState struct {
     	Rev        int64                        // the store revision of the snapshot
     	Conf       *pb.SpConf
     	Cntlrs     map[uint64]*pb.Cntlr         // by cntlr_id, from Conf.cntlr_id_list
     	Slices     map[uint64]*pb.Slice         // by slice_id, from Conf.slice_id_list
     	Tds        []*pb.ThinDevice             // in Conf.td_name_list order (+ TdNames)
     	Subsystems map[string]*pb.Subsystem     // by nqn
     	Clones     map[string]*pb.Clone         // by clone_name
     	Xfers      map[string]*pb.Transfer
     	Migrs      map[string]*pb.Migration
     	CloneBmIdx map[string][]BmChunk         // chunk indexes present (keys only) + mod_revision
     	MigrBmIdx  map[string][]BmChunk         // BmChunk{Idx uint32; ModRev int64}
     	DnByAddr   map[string]*pb.DnConf        // every Side.addr_port of the SP
     	CnByAddr   map[string]*pb.CnConf        // every Cntlr.addr_port of the SP
     }
     ```

     A missing `SpConf` returns `ErrNotFound` (the SP is being deleted; the
     rev key's delete follows). A listed sub-object whose key is missing is
     reported in `SpState.Missing` and logged by the caller; the load still
     succeeds. Bitmap **values** are never loaded here — pushes read one chunk
     at a time (BM3). Bitmap indexes come from a keys-only scan
     (`RangeKeysAtRev`, EU2) performed **outside** the STM at the same store
     revision (`clientv3.WithRev(state.Rev)`): the STM cannot range, and the
     indexes only ever grow (§8.9/§8.11), so a slightly newer view is
     harmless.

MD4. **Capacity keys** (`architecture.md` §5.6, §6.2). `DnBinIdx(freeExt
     uint64, conf *pb.DnBinConf) (bin uint32, ok bool)` (`ok = false` below
     `1 << bin0_shift`); `DnAllocatable(dn *pb.DnConf, conf)` and
     `CnAllocatable(cn *pb.CnConf)` implement the presence rule verbatim
     (pointer-list cap, `err_epoch != 0`, `disabled`, free floor).
     `MaintainDnCapacity(s STM, cid, cc *pb.ClusterConf, old, new
     *pb.DnConf)` deletes the key implied by `old` (if `old` was allocatable)
     and puts the key implied by `new` (if allocatable; value =
     `DnCapacity{location}`); `MaintainCnCapacity` likewise. Both are
     idempotent and are called by every op that changes an input of the
     rule, inside that op's STM. `old` is the record as read in the same STM,
     which is what makes the delete target exact.

MD5. **Allocator** (§6.3/§6.4 verbatim). `FindDnCandidates(ctx, cli, cid,
     cc, candExt uint64, candCnt int, black, white, excludeLocs []string)
     ([]Cand, error)` — `excludeLocs` seeds the `LocList`, so the caller's
     own failure domains are excluded before the walk starts —
     `FindDnCandidatesAntiAffine(…, candCnt, requiredCnt int, black, white,
     excludeLocs []string) ([]Cand, bool, error)`, the §6.5 two-tier rule
     (tier 1 with the exclusion; tier 2 rescans without it when tier 1 yields
     fewer than `requiredCnt` — the DNs the caller must actually place, never
     the oversampled `candCnt` — and its candidates are merged **behind**
     tier 1's, so a distinct-domain DN tier 1 found is never dropped; the bool
     reports whether tier 2 ran), and `FindCnCandidates(ctx, cli, cid,
     candExt, candCnt, black, white, spCnAddrs []string)`;
     `Cand{AddrPort, Location string; FreeExt uint64;
     BinIdx uint32}`; `PickRandom(cands, n)`. The scans are plain descending
     `Range`s **outside** any STM. Because the picked node's free count is
     part of its capacity key, every op of MD6 re-validates a pick by
     `Get`-ing the exact capacity key the scan saw (`Cand.FreeExt`): gone ⇒
     `ErrPrecondition{"candidate changed"}` ⇒ the caller rescans and retries
     the scan + STM as one unit (`architecture.md` §8.4 step 2).

MD6. **Internal mutations.** Each is **one** `RunSTM`, re-validates every
     precondition it lists (MD7), and bumps exactly the revisions
     `architecture.md` §8/§10 assign to the equivalent RPC. `now` is the
     caller's unix seconds; thresholds are resolved inside from
     `SpConf.event_threshold` with the §7 defaults. `shard`/`spId` address
     the `SpRev` key; `spName` the `SpConf`. Three ops — `GrowSlice`,
     `CreateSpareLeg`, `SwitchSpareLeg` — also take `expectRev` (added by
     gateway.md §2.2 #3): when non-zero the STM re-checks `SpRev.revision ==
     expectRev` first (`ErrPrecondition` "stale revision" on mismatch); `0`
     skips the check. The gateway passes its request token when the request
     carried one and 0 when it did not — GW6 is presence-based, so the skip
     at this layer is the same opt-out as the skip at that one; this worker's
     reaction path passes 0.

     | op | preconditions (re-validated in the STM) | effects |
     |---|---|---|
     | `SetDnErrEpoch(cid, addrPort, epoch, cc)` / `SetCnErrEpoch` | record exists | if `epoch != 0`: set only when the stored value is 0 (the threshold clock never restarts); if `epoch == 0`: clear. `Maintain*Capacity(old, new)`. **No rev bump** (§5.5). No write when unchanged |
     | `SetCntlrErrEpoch(cid, spId, cntlrId, epoch)` | record exists | same set/clear rule on `Cntlr`; no bump |
     | `SetLegErrEpoch(cid, spId, sliceId, legId, epoch)` / `SetSideErrEpoch(..., sideId, epoch)` | slice exists, leg/side found in any group's `leg_list`/`spare_leg_list` | same rule on the embedded record; rewrite the `Slice`; no bump |
     | `FlipProvisioned(cid, shard, spId, sides []SideRef) (written []SideRef)` | slice exists | every listed side still `provisioned == false` is set `true`; bump `SpRev` once iff any was written. The return lists the sides actually flipped, so the §12 `flip applied` record can name each (count = `len(written)`) (§10.3) |
     | `FlipCreated(cid, shard, spId, cands []TdRef{Name, TdId}) (written []TdRef)` | — | per §10.3: skip a candidate whose key is absent, whose `td_id` differs, or already `created`; set the rest; bump once iff any was written; the return lists the tds actually flipped, as above |
     | `Failover(cid, shard, spId, spName, oldId, newId, now)` | SP not `deleting`, `sp_level < NO_THINPOOL`; `old.primary`; and, unless `old.disabled` (a disabled primary is the AR5 trigger on its own, §8.6, and waits out no threshold), `old.err_epoch != 0` (`ErrPrecondition` "old cntlr is healthy and enabled") and `now − old.err_epoch ≥ primary_unhealthy`; `new` is `!primary && !disabled && err_epoch == 0` **and** has the smallest `cntlr_id` among all such cntlrs | flip both `primary` booleans; bump `SpRev` (§10.4) |
     | `GrowSlice(cid, shard, spId, spName, expectRev, sliceId, isMeta, poolTotal, cc, legs []Cand) (grpId)` | SP checks as above; `expectRev` as in the preamble; slice exists; meta ladder not at the 16 GiB cap; no grow of that kind pending — AR6's rule re-applied in-STM, judged against `poolTotal` (the worker passes the primary's reported total; the gateway passes `math.MaxUint64`, so a user-driven grow is never "pending" — architecture.md §8.5, gateway.md §5.4); every picked DN allocatable, `free ≥ ext_cnt`, capacity key unchanged; every cntlr's CN `free ≥ ext_cnt` | `ext_cnt` = first data group's (`is_meta = false`) or the ladder value (§8.5); `meta_blocks`/`data_blocks` per §3.6 with the SP's `block_size`/`bitmap_chunk_block_cnt` and `cc.extent_size` (defaults resolved); ids from `SpConf.next_id`; new `Group` with one `Leg`+`Side` per pick (`leg_idx` 0…, `cntlid_slot = cntlid_slot_list[0]`, `provisioned = false`, `addr_port`/`nvme_tr_conf` from the DN); DN bookkeeping (`side_ptr_list`, `free_ext_cnt`, capacity, `DnRev` bump each); CN budgets (`free_ext_cnt`, capacity, `CnRev` bump each); `Slice`, `SpConf`; bump `SpRev` |
     | `ReplaceCntlr(cid, shard, spId, spName, oldId, newCn Cand, asPrimary, now) (newId)` | SP checks; `old.err_epoch != 0`, `now − old.err_epoch ≥ cntlr_unhealthy`, `!old.disabled`; if `old.primary`: `asPrimary` and no failover candidate exists; `newCn` allocatable, `free ≥` SP footprint (Σ `ext_cnt` over all groups), not hosting a cntlr of this SP, capacity key unchanged | delete old `Cntlr` (its CN, if the record still exists: pointer out, footprint back, capacity, `CnRev`); new `Cntlr{cntlid_slot = old's, primary = asPrimary, disabled = false}` with `cntlr_id = next_id++` (new CN: pointer in, footprint out, capacity, `CnRev`); every `CdcEntry` of the SP (`ss_id` via each `Subsystem` in `nqn_list`): old `nvme_tr_conf` out, new in; `SpConf`; bump `SpRev` (§8.6 ×2 in one STM) |
     | `CreateSpareLeg(cid, shard, spId, spName, expectRev, sliceId, grpId, dn Cand, cc) (legId)` | SP checks; `expectRev` as in the preamble; group exists and is `RedundMdRaid1`; `len(spare_leg_list) < MaxSpareLegPerGrp`; `dn` hosts no leg/spare of the group, allocatable, `free ≥ group.ext_cnt`, capacity key unchanged | `Leg{leg_id, leg_idx = 1 + max idx over both lists, Side{provisioned = false, cntlid_slot = cntlid_slot_list[0], …}}` appended to `spare_leg_list`; DN bookkeeping + `DnRev`; `Slice`, `SpConf`; bump `SpRev` (§8.12) |
     | `SwitchSpareLeg(cid, shard, spId, spName, expectRev, sliceId, grpId, spareLegId, targetLegId)` | SP checks; `expectRev` as in the preamble; spare in `spare_leg_list`, target in `leg_list`; the spare's side `provisioned == true` | the spare takes the target's position in `leg_list`; the target is appended to `spare_leg_list`; bump `SpRev` (§8.12) |

MD7. **`ErrPrecondition`.** `type ErrPrecondition struct{ Op, Reason string }`;
     returned from inside the STM callback, it aborts without commit (EU4).
     An op never writes partially, never sleeps, and never retries a
     precondition failure — the caller logs `reaction skipped` (LG) and
     re-evaluates on its next pass.

MD8. **Gateway reuse (non-normative).** The public RPCs of `architecture.md`
     §8 are the same STM bodies plus request validation (§7), the public
     preconditions (`DeleteCntlr`'s `disabled == true`, `primary == false`)
     and reply mapping; the gateway adds those on top of `model` rather than
     duplicating the bodies.

MD9. **Tests.** Key golden strings (the §5.1 example, every prefix ending in
     a space, round-trip of every parser); `ClusterId` against a fixed
     vector; `DnBinIdx`/allocatable tables; and, against the EU7 etcd binary
     (skipped without one): the allocator's bin walk, location dedupe,
     black/white lists, the `excludeLocs` seeding of §6.5 tier 1 and the
     two-tier helper (tier 1 alone; the tier-2 rescan merged **behind** it;
     and the trigger itself — a tier 1 short of `candCnt` but not of
     `requiredCnt` must NOT fall through); each MD6 op's happy path, every
     listed precondition as an `ErrPrecondition`, revision bumps counted
     exactly, and the candidate-changed retry.

---

## 5. `cmd/dnv-worker` [CM]

CM1. **Flags** (cobra root command, no subcommands; every flag also a config
     key and an environment variable, prefix `DNV_WORKER_`):

     | flag | default | meaning |
     |---|---|---|
     | `--etcd-endpoints` | (required) | comma-separated `host:port` list |
     | `--roles` | `dn,cn,sp` | subset of `dn`, `cn`, `sp`; each role is registered and driven independently (VW10) |
     | `--vote-interval` | `DefaultVoteWorkerInterval` (10) | seconds between registry heartbeats (VW2) |
     | `--vote-grace-time` | `DefaultVoteWorkerGraceTime` (60) | seconds a membership change must hold (VW5) |
     | `--etcd-dial-timeout` | `DefaultEtcdDialTimeout` (5) | seconds, EU1 |
     | `--config` | — | optional viper config file |

     The two vote timers exist as flags so the §14 suite can run membership
     cases in seconds; production runs the defaults.

CM2. **Binding.** As `cmd/dnv-agent` (`dnagent.md` §3): `viper.BindPFlags`,
     `SetEnvPrefix("DNV_WORKER")`, `-`→`_` key replacer, `AutomaticEnv`,
     `--config` read when set; required values are checked after binding so
     a file or environment satisfies them.

CM3. **Validation.** `--etcd-endpoints` non-empty; `--roles` a non-empty,
     duplicate-free subset of the three roles; both timers ≥ 1;
     `--vote-grace-time` SHOULD exceed 2 × `--vote-interval` (a warning is
     logged otherwise — a grace window shorter than the dead threshold is
     legal but pointless).

CM4. **Startup.** Install the default JSON logger (`common` `init`), mint a
     startup trace id, build the `etcdutil` client (EU1), then `worker.Run(ctx,
     client, Config{Endpoints, Roles, VoteInterval, GraceTime})` (`Endpoints`
     feeds the §12 `worker starting` record's `endpoints` attribute). `Run`
     starts the
     `ClusterConf` cache (§8.5) and the vote worker (§6) and blocks until
     `ctx` ends. Startup never fails because etcd is unreachable: the vote
     layer stays fenced (VW8) and retries every interval, logging each
     failure. Only configuration errors make the process exit non-zero at
     start.

CM5. **Shutdown.** `SIGINT`/`SIGTERM` cancel `ctx`; `Run` then, in order:
     stops the heartbeat loop; **deletes its own registrations** (best
     effort, one `Delete` per role under `DefaultEtcdOpTimeout`, so peers
     start their grace windows now rather than after the dead threshold);
     stops every shard worker gracefully and in parallel (SW5 → RW11 — an
     in-flight `Syncup*`/`Push*` is allowed to finish, so this can take up to
     `DefaultWorkerSyncupTimeout`); closes the cache watch and the client;
     returns `nil`. A second signal during that window exits immediately
     with status 1. Deleting before draining means the successor can start
     while a last syncup finishes — the accepted overlap of §0 item 4.

CM6. **Logging.** JSON on stdout, Info level by default (`log.md`). The
     `worker starting` record carries `roles`, `seed`, `endpoints`,
     `vote_interval`, `grace_time`; `worker stopping` carries `seed`. Every
     round, syncup, push, flip and reaction runs under a trace id minted per
     RW10, so the seed prefix in an agent's `trace_id` attributes the request
     to this process.

CM7. **Wiring check.** `go list -deps ./cmd/dnv-worker | grep etcd` finds the
     etcd client; `go list -deps ./cmd/dnv-agent ./cmd/dnvctl | grep etcd`
     still finds nothing (`layout.md` §7).

---

## 6. The vote worker — `worker/vote.go` [VW]

One vote worker per process. It owns the seed, the heartbeat loop, the three
registry watches, the per-registration state machines, the effective
membership of every role, the ownership computation, and the lifecycle of the
shard workers.

### 6.1 Identity

VW1. The **seed** is a v4 uuid: 16 bytes from `crypto/rand` with the version
     and variant bits set, rendered canonically (`8-4-4-4-12`, lower-case
     hex) — no new dependency (the repo already shapes uuids by hand in
     `common.NvmeHostId`). A seed identifies one **incarnation** of the vote
     layer: a process start, and every rejoin after a fence (VW8), mints a
     new one; nothing is persisted. All roles of one process share the
     incarnation's seed.

### 6.2 Registration and heartbeat

VW2. For every configured role `r` the worker keeps `WorkerRegKey(r, seed)`
     refreshed: value `WorkerReg{epoch = time.Now().Unix()}`, re-put every
     `--vote-interval` seconds from a ticker, each put under
     `DefaultEtcdOpTimeout`. The **first** put of every role happens before
     the worker observes anything (VW3), so its own registration is part of
     its first scan or its first watch events. A failed put is logged by the
     `etcd put` record and retried at the next tick. The loop records
     `lastOkPut`, the monotonic time of the last tick at which **every**
     role's put succeeded (VW8a).

### 6.3 Observation

VW3. Per role: one `Range(WorkerRegPrefix(r))` scan, then `WatchTyped(prefix,
     rev+1, WorkerReg)`. For every registration key the observer keeps
     `lastSeen` — the monotonic time of the last put it observed (the scan
     time for keys found by a scan) — and `observed ∈ {live, dead}`:

     * a key found by a scan: `lastSeen = now`; `observed = live` — an
       **appear** transition if it was not already observed live;
     * a put event: `lastSeen = now`; if `observed == dead` → `live`
       (appear / reappear);
     * a delete event: `observed = dead` (disappear) at once;
     * a deadline at `lastSeen + 2 × interval`, re-armed by every put: when
       it fires with no put in between → `observed = dead` (disappear).

     The worker's own keys are tracked exactly like everyone else's. After
     `ErrCompacted` or any watch error the role rescans: keys present get
     `lastSeen = now` (a key already observed live has no transition); keys
     that were live but are absent from the rescan transition to dead; the
     watch restarts from the rescan's `rev + 1`.

VW4. The stored `epoch` is **never** compared with local time. Liveness
     depends only on the observer's own monotonic clock and the events it
     saw; NTP is an operational nicety (the epoch is what `workerctl
     list-workers` and operators read), not a correctness requirement
     ([D17]).

### 6.4 Grace and effective membership

VW5. Per registration the observer also keeps `effective ∈ {member,
     nonmember}` (initially `nonmember`) and at most one **pending** grace
     timer with a target state. On every observed transition of key `k`:
     cancel `k`'s pending timer; let `target = member` if `k` is now observed
     live, else `nonmember`; start a new timer of `--vote-grace-time`
     seconds with that target — on **every** transition, even when `target`
     already equals `effective(k)`. The always-arm rule is what VW7's
     never-became-effective disappear case relies on: a worker that appears
     and dies inside its own grace window must still get a commit, because
     the commit's VW6 garbage collection is the only thing that ever removes
     a dead key ([D17], no lease) — a target-≠-effective guard here would
     leak that key and its tracking entry forever. When a timer fires, its
     target is **committed** (VW6); a commit whose target already equals the
     committed state changes no membership, logs nothing and recomputes no
     ownership — it is a no-op apart from the VW6 collection. A registration
     that flaps faster than the grace time therefore still never changes
     anybody's effective membership, and never delays the commit of any
     other registration. One commit is never a collection: a nonmember
     target for the observer's **own** still-heartbeating key takes VW8's
     fence exit instead of deleting a live worker's registration (reachable
     when the grace window closes before the next heartbeat tick's VW8
     check — a grace time below the dead threshold, legal per CM3).

VW6. **Commit** of `(k, target)`: set `effective(k) = target`; log
     `membership committed`; if `target == nonmember`: issue a best-effort
     `Delete(k)` — the garbage collection of a dead registration, performed
     by every observer that commits it (idempotent), and the only thing that
     ever removes a key whose owner died — and drop `k`'s tracking entry (a
     later put creates a fresh entry with an appear transition). Then
     recompute ownership for the role (VW9).

VW7. **Startup.** The effective membership of every role starts **empty**,
     and every key found by the first scan — the worker's own included —
     enters through an appear transition at scan time. Consequently a fresh
     worker drives nothing for its first grace window, even when it is the
     only worker; the fleet's old and new owners of a shard switch within
     seconds of each other (they all started the same timer for the same
     key at about the same time); and a key found by the scan whose owner is
     already dead never becomes effective — its disappear at `scan + 2 ×
     interval` cancels the pending appear and starts a disappear timer whose
     commit is a no-op except for the VW6 garbage collection.

### 6.5 Self-fence and rejoin

VW8. A worker MUST **fence** itself when any of these holds, checked on
     every heartbeat tick and on every own-key watch event:

     (a) `now − lastOkPut ≥ 2 × interval` — its heartbeat has not reached
         etcd for the dead threshold; peers are about to (or already do)
         consider it dead. This also covers a process that was stopped
         (`SIGSTOP`, VM pause): the monotonic clock advances meanwhile.
     (b) its own put is not echoed by its own watch: the latest observed put
         event for its own key (any role) is older than `2 × interval` while
         puts report success — the watch is broken and its view of the
         peers is stale.
     (c) a delete event for its own key that this process did not issue —
         a peer committed it dead (VW6).

     Fencing means: log `worker fenced` (`reason`, `old_seed`, `new_seed`);
     stop every shard worker of every role gracefully and in parallel (SW5);
     best-effort `Delete` the old registrations (they may already be gone);
     discard every tracking entry, timer and effective set; mint a new seed
     (VW1) and restart §6.2–§6.4 from scratch. VW7 applies to the new
     incarnation: nothing is driven until one full grace window after the
     new seed's first successful put. There is no "resume with the old
     seed" path — a worker that lost etcd for the dead threshold is a new
     worker, exactly like a restart. A fence whose new seed cannot be minted
     (VW1) tears nothing down and is instead **remembered** and retried on
     every following heartbeat tick, ahead of the checks above
     (`worker/vote.go`): (a) and (b) are conditions that would fire again by
     themselves, but (c) is an event whose delete has already been consumed,
     so a fence dropped there would be lost for good.

### 6.6 Tickets and ownership

VW9. For role `r`, shard `s ∈ [0, ShardBucketSize)` and effective member `m`:

     ```
     ticket(m, r, s) = sha256(fmt.Sprintf("%s-%s-%02x", m.seed, r, s))   // 32 bytes
     owner(r, s)     = the effective member whose ticket is largest under bytes.Compare
     ```

     (a tie is a sha256 collision; it is broken by the larger seed string and
     is never expected). Ownership is recomputed on every commit that changed
     the effective set; `owned(r) = { s : owner(r, s) == self }`. The worker
     diffs it against its running shard workers: newly owned → start one
     (SW1) and log `shard owned`; no longer owned → stop it gracefully (SW5)
     and log `shard released`. A shard's owner changes only when the owner
     leaves or a new member outranks it, so a membership change moves about
     1/n of the shards and never reshuffles the rest.

VW10. **Roles are independent**: each role has its own registry prefix,
      watch, tracking entries, timers, effective set, shard workers and log
      records, and ownership of role `r` considers only the registrations
      under `r`'s prefix. Fencing (VW8) is process-wide because the seed is.

VW11. An empty effective membership owns nothing. The worker never drives a
      shard from the raw observed set.

### 6.7 Worked timeline

Appendix A walks a join, a crash, a graceful stop and a pause with the
default timers.

---

## 7. The shard worker — `worker/shard.go` [SW]

SW1. One shard worker per owned `(role, shard)`, started and stopped only by
     the vote worker (VW9). Its prefix is `DnRevPrefix(s)`, `CnRevPrefix(s)`
     or `SpRevPrefix(s)`; its message type `DnRev`, `CnRev` or `SpRev`.

SW2. **Start.** `Range(prefix)` → `rev`; every key is decoded (`Decode`) and
     parsed (`ParseDnRevKey` …; a malformed key or value is logged and
     skipped); one revision worker per `(cluster_id, id)` is started with
     the initial desired state `(value.revision, handle)` (RW3); then
     `WatchTyped(prefix, rev + 1)`.

SW3. **Events.** A put for an existing key → `Update(desired)` on its
     revision worker (RW3 coalescing; a put that changes neither `revision`
     nor the handle is a no-op); a put for a new key → start a revision
     worker; a delete → stop it gracefully (RW11) and forget it. Each start
     and stop logs `revision worker started` / `revision worker stopped`.

SW4. **Compaction and watch errors.** Rescan the prefix: start workers for
     keys not known; update known ones whose `(revision, handle)` differ;
     stop workers whose key is absent from the scan; restart the watch from
     the new `rev + 1`. A failing rescan is retried every `--vote-interval`
     seconds — never a hot loop.

SW5. **Graceful stop.** Cancel the watch, stop every revision worker in
     parallel (RW11), join them. The vote worker logs `shard released` after
     the join, so a shard shows as released only when nothing is driving it
     any more.

SW6. **Multi-cluster.** Keys of every cluster share the shard prefix; the
     `cluster_id` comes from the key, and the §8.5 cache supplies that
     cluster's configuration. A revision worker whose `cluster_id` is not in
     the cache idles (RW9); it never guesses defaults for an unknown cluster.

---

## 8. The revision worker — `worker/revision.go` and the role files [RW]

### 8.1 The per-object loop

RW1. **One goroutine per object** — one per DN (dn role), per CN (cn role),
     per side and per cntlr (sp role). The goroutine alone owns the object's
     `Check*` stream, its `Syncup*` calls and the loop's own state, which is
     what makes `architecture.md` §9.1's "one `Syncup*` at a time per
     object" and §9.7's "one stream per object" hold by construction. Two
     deliberate carve-outs share the object: the `Push*Bitmap` calls run on
     the §10 pusher's own goroutines (BM3 — one in flight per
     migration/clone, concurrently with this loop), and `lastInfo` is
     published by the stream's pump goroutine under a mutex so the sp
     coordinator can snapshot it (AR1).

RW2. **State**: `desired` (the revision to reach plus the inputs the request
     is built from), `synced` (the last revision the agent acknowledged —
     the `revision` of a `code == 0` `Syncup*` reply or of a `Check*` reply),
     `stream`, `lastInfo` (the last `*Info` received), the health state of
     §9, `resyncWanted` (BM6), and the round timer.

RW3. **Coalescing.** Desired changes arrive from the parent (shard worker or
     SP coordinator) over a channel of capacity one that is overwritten, so
     the loop only ever sees the latest revision — every request carries the
     complete state, intermediate revisions need not be sent (§9.1).

RW4. **Round**, every `interval` seconds (RW9):
     1. no stream ⇒ open one over the cached connection (RW7); a failure to
        open counts as a broken stream;
     2. send `Check*Request{ids, revision = desired.revision, show_info =
        false}`;
     3. wait for the reply at most `interval` seconds (RW8);
     4. no reply, or a stream error ⇒ close the stream; health "unreachable"
        (§9) — except for a round abandoned under RW6, and for a round the
        graceful stop of RW11 cut short, each of which drops its stream the
        same way but carries no health verdict: a cancelled stop ctx is not
        a sick agent, and the verdict would start the §11 threshold clock on
        a healthy object;
     5. reply ⇒ process its `*Info` if present (§9; sp: RW18/RW19); then if
        `agent_reply.code != 0` **or** `reply.revision != desired.revision`
        ⇒ issue `Syncup*` (RW5) — this is also how the first sync after a
        shard handoff, a worker restart or an agent restart happens, with no
        recovery step of its own;
     6. `resyncWanted` ⇒ issue `Syncup*` (an equal-revision re-apply, §9.1);
     7. re-arm the timer.

RW5. **Syncup.** Build the request from the current inputs (RW13–RW16), send
     under `DefaultWorkerSyncupTimeout` with a trace id (RW10). `code == 0`
     ⇒ `synced = reply.revision`, process the `*Info` (§9), `bm_info` /
     `bm_info_list` (§10), and the sp flips (RW18/RW19). `code != 0` ⇒ log
     `syncup rejected` (`code`, `details`) and leave it to the next round:
     unknown object (`ReplyCodeUnknownObject`) means the parent syncup has
     not landed yet — e.g. an sp-worker's `SyncupSide` reaching the DN before
     the dn-worker's `SyncupDn` listed the side, which is normal since the
     roles are independent (VW10) — and stale revision
     (`ReplyCodeStaleRevision`) means the agent holds a revision newer than
     etcd's, which only an etcd restore can cause and is logged at `Error`.
     A gRPC error is logged and left to the next round. A desired change
     that arrives while a syncup is in flight is applied when it returns.

RW6. **Immediate syncup.** A desired change from the parent triggers a
     `Syncup*` at once, without waiting for the round. A change that arrives
     while a round waits for its reply (RW4 step 3) is applied there and
     then, because that reply may be a whole `interval` away (RW9): the round
     is **abandoned** — its reply would answer the request built from the
     superseded revision — and its stream is dropped with it: RW4 step 4
     never reuses a stream across a round that ended without a reply, and an
     abandoned round is such a round. The next round opens a fresh stream,
     which §9.7 answers with the complete `*Info` again. An abandoned round
     carries **no** health verdict (`worker/revision.go`): nothing was
     observed about the agent, the round was only overtaken.

RW7. **Connections.** A process-wide cache `addr_port → *grpc.ClientConn`
     (`grpc.NewClient`, `insecure.NewCredentials()`, both client
     interceptors — `grpc.md` §4), reference-counted by the objects using an
     endpoint and closed when the last one stops. Every stream and unary call
     to one agent multiplexes over that connection. A stream is opened lazily
     on the first round and after every failure, and closed with `CloseSend`
     plus ctx cancellation on stop (RW11).

RW8. **Round timeout** = the object kind's interval: a reply arriving later
     is a missed reply (§9.7). The timer is re-armed *after* each round, so a
     slow round never queues a burst of catch-up rounds.

RW9. **Inputs from the cache** (§8.5): `dn_interval` / `cn_interval` /
     `side_interval` / `cntlr_interval` (0 ⇒ `DefaultHealthCheckInterval`,
     clamped to `[MinHealthCheckInterval, MaxHealthCheckInterval]`),
     `extent_size`, `qos_ratio`, `dn_bin_conf`. A cluster absent from the
     cache ⇒ the loop **idles**: no stream, no syncup, one `cluster conf
     missing` record, a retry every `DefaultHealthCheckInterval` seconds.

RW10. **Trace ids.** Every round, syncup, push, flip and reaction runs under
      `common.WithTraceId(ctx, seed[:8] + "-" + common.NewTraceId())`. The
      interceptors carry it to the agent (`grpc.md` T1), whose log then names
      the worker that sent each request — the §14 suite's
      "one owner per shard" evidence.

RW11. **Graceful stop.** On ctx cancellation the loop finishes an in-flight
      unary call (its own deadline bounds the wait; the stop ctx is not the
      RPC ctx), then `CloseSend`s and cancels the stream, releases the
      connection and logs `revision worker stopped`. Parents join their
      children with a `sync.WaitGroup`.

RW12. **No backoff anywhere.** The round is the retry cadence for an
      unreachable agent, a rejected syncup, a failed push and a missing
      cluster conf alike.

### 8.2 dn role — `worker/dnrole.go`

RW13. Inputs: `cluster_id` and `dn_id` from the key, `addr_port` and
      `revision` from the value. Per syncup the loop reads `DnConf` at
      `DnConfKey(cluster_id, addr_port)` with a plain `Get` (missing ⇒ log,
      skip, retry next round) and the cluster conf from the cache, and sends
      `SyncupDnRequest{cluster_id, dn_id, revision, side_pointer_list =
      DnConf.side_ptr_list, extent_size = dn_bin_conf.extent_size}` to
      `addr_port`. Rounds send `CheckDnRequest{cluster_id, dn_id, revision,
      show_info}`. Health per HL1. A put whose only change is `addr_port`
      re-syncs the node at its new endpoint: the loop drops its stream and
      connection reference and continues at the new address; no delete ever
      reaches the agent (§10.2, [D10]).

### 8.3 cn role — `worker/cnrole.go`

The mirror image: `CnConf` at `CnConfKey`, `SyncupCnRequest{cluster_id,
cn_id, revision, cntlr_pointer_list = CnConf.cntlr_ptr_list, qos_ratio =
ClusterConf.qos_ratio}`, `CheckCnRequest`, HL1.

### 8.4 sp role — `worker/sprole.go`

RW14. The SP revision worker is a **coordinator**. On every desired change
      (an `SpRev` put: `revision` + `sp_name`) it loads the SP with
      `model.LoadSp` (MD3), resolves every side's `dn_id`
      (`DnByAddr[side.addr_port]`) and every cntlr's `cn_id`, builds every
      `SyncupSideRequest` and `SyncupCntlrRequest` once (RW15/RW16), and
      diffs its **children**: one per side `(sp_id, leg_id, side_id)` —
      spare legs' sides included — and one per cntlr `(sp_id, cntlr_id)`.
      New → start; gone → stop (RW11); a child whose endpoint changed is
      restarted at the new one; every remaining child receives its new
      request as a desired change (RW6). A child whose **request** changed
      while the `SpRev` revision did not — a re-resolution tick that altered
      a standby list — is restarted too, because RW3's coalescing sees the
      unchanged revision and would otherwise swallow the change and leave
      the child driving a stale request until the next bump
      (`worker/sprole.go`). That is the fan-out: unordered across sides and
      cntlrs by design ([D16]). `ErrNotFound` from `LoadSp` means the SP is
      being deleted: log, keep the children until the `SpRev` delete arrives
      (the agents tear down through the pointer lists). An endpoint without a
      `DnConf`/`CnConf` leaves that child idle; the coordinator re-resolves
      idle children every `cntlr_interval` seconds. When the object that
      cannot be resolved is a **cntlr** — its `Cntlr` record is missing (MD3)
      or its `CnConf` is absent — **every side child** of the SP is left
      idle, not just that cntlr's own: RW15's `primary_cn_id` /
      `standby_id_list` must name every cntlr of the SP, and a `side_conf`
      built from a shrunken set makes every DN of the SP tear the missing
      CN's dm-error, dm-linear, subsystem and namespace down
      (`worker/sprole.go`). The **other** cntlr children are unaffected —
      RW16's request carries no peer's `cn_id`, so on the cntlr side the
      effect stays confined to that cntlr's own child, left idle by the rule
      above — and the primary's leg rows are still placed on their slice
      (HL2).

RW15. **Side request.** `SyncupSideRequest{cluster_id, dn_id,
      side_pointer{sp_id, leg_id, side_id}, revision = SpRev.revision,
      side_conf{ext_cnt = group.ext_cnt, cntlid_slot = side.cntlid_slot,
      primary_cn_id = the cn_id of the cntlr with primary == true (0 if
      none), standby_id_list = the cn_ids of every other cntlr — disabled
      ones included, a disabled cntlr keeps its standby shape
      (`cnagent.md` §4) —, sp_level = SpConf.sp_level, provisioned =
      side.provisioned}}`. If the side's leg has two sides and a `Migration`
      of the SP names this side as `src_side_id`: `migr_src_conf{migr_id,
      dst_side_id, dst_dn_id, dst_provisioned = the dst side's provisioned}`;
      as `dst_side_id`: `migr_dst_conf{migr_id, src_side_id, src_dn_id,
      src_nvme_tr_conf = the src side's nvme_tr_conf, block_size =
      bdev_conf.dm_pool_conf.data_block_size, meta_blocks =
      group.meta_blocks, dm_clone_conf = the migration's, bm_cnt =
      Migration.bm_cnt}`. Zero-valued conf fields are replaced by their §7
      defaults before sending.

RW16. **Cntlr request.** `SyncupCntlrRequest{cluster_id, cn_id,
      cntlr_pointer{sp_id, cntlr_id}, revision, bdev_conf = SpConf.bdev_conf,
      sp_level, cntlr = the Cntlr record, id_to_slice keyed by
      fmt.Sprintf(IdKeyFmt, slice_id) — the key the cn agent reads —,
      td_list in td_name_list order, nqn_to_subsystem, clone_list,
      xfer_list, migr_list}`. Every cntlr of the SP receives the full state
      (§10.3).

RW17. **Rounds** send `CheckSideRequest{cluster_id, dn_id, side_pointer,
      revision, show_info}` / `CheckCntlrRequest{cluster_id, cn_id,
      cntlr_pointer, revision, show_info}`.

RW18. **Provisioned flip** (§10.3). On any `SyncupSide`/`CheckSide` reply
      for a side whose **synced** request carried `provisioned = false` and
      whose `side_info.zeroed_ext_cnt == total_ext_cnt > 0`, the child
      reports the side to the coordinator, which runs `model.FlipProvisioned`
      (several sides reported within one round MAY share one STM). The bump
      re-fans the SP (RW14) and the re-synced sides carry `provisioned =
      true`. Nothing is remembered across a handoff: the new owner's first
      round carries the full `*Info` and flips whatever is still `false`.

RW19. **Created flip** (§10.3, `ThinDeviceCreated.md` U3, verbatim). Every
      `SyncupCntlrReply` and `CheckCntlrReply` with `code == 0` is scanned:
      a td `X` of the loaded state with `created == false` is a candidate
      when `cntlr_info.td_id_to_thin_info[X.td_id]` exists, its
      `slice_id_to_dm_thin` key set equals the SP's slice ids exactly, and
      every row is `RES_STATUS_OK`. Candidates go to `model.FlipCreated`; a
      reply that completes none causes no etcd traffic. A pending
      provisioned flip of the same SP MAY share the STM.

RW20. Health of sides and cntlrs per HL2; pushes per §10; the reaction pass
      runs on the coordinator's own ticker (AR1).

### 8.5 `ClusterConf` cache — `worker/clusterconf.go`

RW21. One per process: `Range(ClusterConfPrefix())` then `WatchTyped(rev +
      1, ClusterConf)`. Entries are keyed by `ClusterId(name,
      creation_epoch)` — the name is the key suffix, the epoch is in the
      value — and a delete removes the entry. Readers receive an immutable
      snapshot of the entry, with defaults resolved at read time: the four
      intervals (0 ⇒ 5, clamped to `[1, 3600]`), `extent_size` (0 ⇒
      `DefaultDnExtSize`), `dn_bin_conf` shifts (a non-increasing set ⇒ the
      0/4/8/12 defaults), `qos_ratio` as stored. Watch errors and
      compaction are handled like SW4.

---

## 9. Health bookkeeping — `worker/health.go` [HL]

HL1. **Nodes (dn/cn roles).** Evaluated per round and per syncup reply on
     the node's own stream; written through `model.SetDnErrEpoch` /
     `SetCnErrEpoch`, which maintain the capacity key in the same STM (MD4,
     §5.6):

     | observation | effect on `DnConf.err_epoch` / `CnConf.err_epoch` |
     |---|---|
     | stream cannot be opened, breaks, or no reply within the round timeout | set to `now` if 0; the in-memory info is marked `RES_STATUS_UNKNOWN` (what the worker records itself while the stream is dead, §9.5 — never written to etcd) |
     | any `RES_STATUS_ERROR` row in `DnInfo` (`disk_info`, `meta_info`, `port_info`) or `CnInfo` (`port_info`, `tmpfs_info`, `tmp_file_info`, `loop_dev_info`) | set to `now` if 0 — including `meta_info` `"disk lacks Write Zeroes"` (§9.4), which is a plain `ERROR` |
     | a clean round: reply in time, `code == 0`, no `ERROR` row in the latest known info | cleared to 0 |
     | `RES_STATUS_PROVISIONING`, `MISSING` | neither set nor clear ([D15]) |
     | `agent_reply.code != 0` | neither set nor clear; triggers a re-sync (RW4) |

HL2. **SP objects (sp role).** Written through `SetCntlrErrEpoch` /
     `SetLegErrEpoch` / `SetSideErrEpoch`:

     | record | set to `now` (if 0) when | cleared when |
     |---|---|---|
     | `Cntlr.err_epoch` | its `CheckCntlr` stream cannot be opened / breaks / misses a round, or any `RES_STATUS_ERROR` row in its `CntlrInfo` **other than** `leg_id_to_leg` | its next round is clean |
     | `Leg.err_epoch` | the **primary** cntlr's `leg_id_to_leg[leg] == RES_STATUS_ERROR` (the §3.6 probe; spares included). A standby's leg row is logged, never recorded | the primary reports the row `RES_STATUS_OK` |
     | `Side.err_epoch` | its `CheckSide` stream cannot be opened / breaks / misses a round, or `side_dev_info` or any `cn_id_to_dm_error` / `cn_id_to_dm_linear` / `cn_id_to_nvmeof` row is `RES_STATUS_ERROR`, or a `migr_src_info` / `migr_dst_info` row is `ERROR` | its next round is clean |

     `PROVISIONING`, `MISSING` and `code != 0` never set any of the three.

HL3. **Transitions only.** A record is written when the observed health
     changes (healthy → unhealthy sets the epoch once — the threshold clock
     of §11 never restarts; unhealthy → healthy clears it). The op re-reads
     the record inside its STM, so two owners observing the same transition
     write once. Health never bumps a revision (§5.5).

HL4. A `Syncup*` reply's `*Info` is processed exactly like a `Check*`
     reply's (the secondary signal, §9.7). A `Syncup*` gRPC failure is not by
     itself a health failure — an unreachable agent breaks the stream too.

HL5. `show_info = false` streams carry an `*Info` only when something
     changed; the worker evaluates health on the latest known info, and a
     reply with neither info nor error is a clean round.

HL6. **No cross-role health writes.** The sp role's health bookkeeping — this
     section's err_epoch writers — never touches `DnConf`/`CnConf` (it writes
     `Cntlr`/`Leg`/`Side` only), and the dn/cn roles never touch SP records.
     (The sp role's §11 *reactions* do rewrite `DnConf`/`CnConf` through the
     model ops — GrowSlice charges DNs and every cntlr CN, CreateSpareLeg
     charges its one DN, ReplaceCntlr rewrites two `CnConf`s — but that is
     allocation bookkeeping, not health.) An unreachable DN is therefore
     marked on `DnConf` by its dn-role owner and on the `Side` records of its
     sides by each sp-role owner, independently.

---

## 10. Bitmap pushes — `worker/bmpush.go` [BM]

The worker side of `architecture.md` §9.6, restated as rules of the sp
children.

BM1. **Sources.** For a side that is a migration's destination: the
     `MigrBitmap` chunks of that migration, indexes `0 … bm_cnt − 1`. For the
     cntlr that is **primary**: the `CloneBitmap` chunks of every clone of
     the SP. Chunk indexes come from the snapshot's keys-only scans
     (`MigrBmIdx` / `CloneBmIdx`, MD3); chunk **values** are read one at a
     time (`Get`) when pushed.

BM2. **Diff.** After every `SyncupSide` reply: `missing = MigrBmIdx[migr] −
     bm_info.bm_idx_list` (only when `bm_info.res_id == migr_id`). After
     every `SyncupCntlr` reply on the primary: per `bm_info_list` entry
     (`res_id = clone_id`), `missing = CloneBmIdx[clone] − bm_idx_list`; a
     clone absent from the list has everything missing.

BM3. **One in flight per migration/clone, ascending `bm_idx`,** each
     `PushMigrBitmapRequest{cluster_id, dn_id, side_pointer, revision =
     synced, migr_id, bm_idx, bitmap}` / `PushCloneBitmapRequest{…,
     cntlr_pointer, clone_id, …}` under `DefaultWorkerPushTimeout`; the next
     part is sent only after a `code == 0` reply. Different
     migrations/clones push independently and MAY run concurrently toward
     one agent (§9.6 step 4); within one object the child sequences them.

BM4. **Targets.** Migration chunks go only to the destination side's DN;
     clone chunks only to the CN hosting the **primary** cntlr. A standby's
     `bm_info_list` is ignored. After a failover the new primary's syncup
     reply reports an empty/partial set and the pushes follow it there.

BM5. **Grown clone chunks ([D8]).** The child memoizes, per `(res_id,
     bm_idx)` — the `res_id` being the `clone_id` here, the `migr_id` for a
     migration — the etcd `mod_revision` of the chunk it last pushed
     (`CloneBmIdx` / `MigrBmIdx` carry it, MD3); a chunk whose `mod_revision`
     advanced is re-pushed even though the agent acknowledges its index. The
     memo is in-memory and lost on a handoff — accepted by [D8]. Migration
     chunks are immutable, so nothing can make the rule re-push one;
     `worker/bmpush.go` implements BM1–BM6 once for both kinds and therefore
     memoizes migration chunks too, where the `mod_revision` comparison is
     inert.

BM6. **Failure.** A push that fails (gRPC error, timeout) or is rejected
     (`code != 0`: stale revision, or the introducing syncup not applied yet)
     sets the object's `resyncWanted`; the next round issues an
     equal-revision `Syncup*` (RW4 step 6) whose reply restarts the diff.
     There is no push-specific timer.

---

## 11. Automatic reactions — `worker/reaction.go` [AR]

The `architecture.md` §10.4 automation as the sp coordinator's **pass**.
Everything here is the worker's job; agents only report.

### 11.1 Pass, priority, suppression

AR1. **Cadence and inputs.** The coordinator runs one pass per SP every
     `cntlr_interval` seconds (its own ticker). A pass starts with a fresh
     `SnapshotRev` (EU4) of `SpConf`, every `Cntlr` and every `Slice` of the
     SP — the records the reactions read, and the ones this worker itself
     writes the `err_epoch`s into — plus the in-memory latest `CntlrInfo` of
     the **primary** cntlr (pool usage, spare readiness) and `now` (unix
     seconds).

AR2. **One action per SP per pass**, evaluated in this priority; the first
     applicable one runs and the pass ends:
     1. primary failover (AR5)
     2. thin-pool auto-grow (AR6)
     3. cntlr replacement (AR7)
     4. leg repair (AR8)

     Every action is a `model` op that re-validates its preconditions inside
     its STM (MD6/MD7); success logs `reaction applied` and the resulting
     `SpRev` bump re-fans the SP (RW14), so the next pass sees the new state.
     The invariant is **at most one applied action per SP per pass**; a
     `reaction skipped` that means "not applicable here, keep looking" does
     not end the pass. The pass continues past: AR5's no-failover-candidate
     (AR7's sole-primary variant, §0 item 16, is defined as "AR5 found
     none" and would otherwise be unreachable); AR6's `grow_pending`,
     `meta_ladder_cap` and `no_data_group` (each can hold indefinitely — a
     grow deferred on the CN, the §8.5 ceiling — and must not disable
     AR7/AR8 for the duration); and AR8's `leg_has_two_sides` and
     `spare_list_full`, which move the scan to the next candidate leg.
     Everything else ends the pass as before: every `ErrPrecondition`, every
     empty allocator scan, every transient op failure, and AR8 step 2's wait
     for a pending spare (which clears itself within one provisioning). Two
     owners overlapping on one SP (§0 item 4) cannot apply an action twice:
     the second STM fails its precondition.

AR3. **Suppression.** No reaction runs for an SP with `deleting == true` or
     `sp_level ≥ SP_LEVEL_NO_THINPOOL` (the disaster-recovery levels of
     §11.7, where an operator is in charge). A **disabled** cntlr is never a
     candidate, replaced or repaired — but a disabled *primary* is itself the
     AR5 failover trigger (§8.6). Disabling is the operator's hands-off
     signal, and §10.4's "skipping the enabled check" is read as skipping the
     public `disabled == true` precondition of `DeleteCntlr`, not as replacing
     disabled cntlrs.

AR4. **Thresholds.** `now − err_epoch ≥ T` with `T` the SP's
     `event_threshold` field, `0` ⇒ the §7 default (`DefaultPrimaryUnhealthy`
     5, `DefaultCntlrUnhealthy` 600, `DefaultSideUnhealthy` 600,
     `DefaultLegUnhealthy` 1200). `leg_unhealthy > side_unhealthy` is a
     gateway validation rule (§7, amended by §15); the worker is correct
     either way.

### 11.2 Failover

AR5. When the primary cntlr is `disabled`, or has `err_epoch != 0` and `now −
     err_epoch ≥ primary_unhealthy`: candidate = the cntlr with the smallest
     `cntlr_id` among those with `primary == false`, `disabled == false`,
     `err_epoch == 0`; none ⇒ `reaction skipped` (`no candidate`) and the pass
     continues (AR2); else
     `model.Failover(old, candidate)`. This is the §11.1 failover trigger;
     the data-plane choreography is the agents'. The `disabled` trigger waits
     out no threshold: disabling is explicit operator intent and the disabled
     primary has already stopped serving (§8.6). The candidate-skip
     bookkeeping is unchanged by that trigger, so it is
     deliberately **not** edge-triggered: a `disabled` primary with no
     eligible candidate re-emits `reaction skipped` / `no candidate` once per
     pass, for as long as it takes an operator to enable a standby or add a
     cntlr.

### 11.3 Thin-pool auto-grow

AR6. Per slice, from the primary's `slice_id_to_dm_pool[slice_id]` row,
     which MUST be `RES_STATUS_OK` (an `ERROR`/`PROVISIONING`/absent pool is
     never grown). `details` is the raw `dmsetup status` line of the thin
     pool; after the `thin-pool` token: `<transaction_id>
     <used_meta>/<total_meta> <used_data>/<total_data> …` — metadata counts
     in dm-thin's fixed 4 KiB metadata blocks, data counts in the pool's
     `data_block_size`. An unparsable line is skipped and logged once per
     change. With `lwm = dm_pool_conf.low_water_mark_pct` (0 ⇒
     `DefaultPoolLowWatermarkPct`; `> 100` ⇒ auto-grow off):

     * **data grow** when `used_data × 100 > lwm × total_data`: internal
       `GrowSlice(is_meta = false)` with `ext_cnt` = the slice's first data
       group's `ext_cnt` (grow by the original allocation unit);
     * **meta grow** when `used_meta × 100 > lwm × total_meta`: internal
       `GrowSlice(is_meta = true)`, ladder-sized (§8.5); data is checked
       first when both breach.

     **Pending rule (stateless).** A grow of a kind is *pending* while the
     reported total is no larger than the total implied by the slice's
     groups of that kind **excluding the newest one**: data:
     `total_data ≤ Σ data_blocks` over all data groups but the last; meta:
     `total_meta ≤ Σ data_blocks × block_size / 4096` over all meta groups
     but the last. While pending, no grow of that kind starts — this is
     §10.4's "one grow per pool at a time", reconstructed from etcd and the
     status line on every pass, so a worker restart or a handoff cannot
     issue a second grow; a grow deferred on the CN ([D15]) stays pending the
     same way, because its totals have not moved. Candidates:
     `FindDnCandidatesAntiAffine(candExt = ext_cnt, candCnt = legs ×
     dn_batch_size, requiredCnt = legs, black = ∅, excludeLocs = ∅ — §6.5
     leaves the grow on the plain scan, so tier 2 never runs)` where
     `legs` = 1 (`RedundNone`) or 2 (`RedundMdRaid1`),
     picked randomly with the growing black list so the legs land on
     distinct DNs; fewer than `legs` candidates, or a cntlr's CN below the
     budget (checked in the op), ⇒ `reaction skipped`.

### 11.4 Cntlr replacement

AR7. When a cntlr has `err_epoch != 0`, `now − err_epoch ≥ cntlr_unhealthy`,
     `disabled == false`, and either `primary == false` or it is the primary
     and **no failover candidate exists** (AR5 found none — the sole-cntlr
     SP, or every other cntlr unhealthy/disabled): candidates =
     `FindCnCandidates(candExt = the SP footprint Σ ext_cnt over all groups,
     candCnt = cn_batch_size, black = {the old cntlr's addr_port}, spCnAddrs
     = the SP's other cntlrs' endpoints)`; pick one; internal
     `ReplaceCntlr(old, pick, asPrimary = old.primary)` — same `cntlid_slot`,
     `primary = true` only in the sole-primary variant, `disabled = false`,
     `err_epoch = 0`. The old CN is black-listed even when the node itself is
     healthy: its cntlr is what failed. None ⇒ `reaction skipped`.

### 11.5 Leg repair

AR8. **Triggers.** A leg in a group's `leg_list` needs repair when either

     * **Case 1** — `Leg.err_epoch != 0` and `now − Leg.err_epoch ≥
       leg_unhealthy`: unhealthy from the cntlr's perspective for the long
       threshold (the primary reports it has no healthy path — possibly a
       CN↔DN connectivity problem, hence the longer wait); or
     * **Case 2** — `Leg.err_epoch != 0` (any duration) **and** its side has
       `Side.err_epoch != 0` with `now − Side.err_epoch ≥ side_unhealthy`:
       the side reports an error or the worker cannot talk to its DN, so the
       DN itself is probably dead, hence the shorter wait. A side the worker
       cannot reach while the primary still sees the leg healthy triggers
       nothing.

     **Preconditions**: the group is `RedundMdRaid1` (`RedundNone` groups
     have no spare — both cases only log); the leg has exactly one side (a
     leg with two sides has a user migration in flight and is left alone);
     the SP is not suppressed (AR3). Several unhealthy legs ⇒ the smallest
     `leg_id` first.

     **Procedure**, one step per pass, on the leg's group:
     1. a **ready** spare exists — `Side.provisioned == true` and the
        primary's latest `leg_id_to_leg[spare_leg_id] == RES_STATUS_OK`
        (spares are connected and probed, §8.12) — ⇒ internal
        `SwitchSpareLeg(spare, leg)`: the spare takes the leg's place, the
        old leg is **parked** in `spare_leg_list` (§0 item 17) — still
        connected and probed, its `err_epoch` still set, never repaired again
        (only `leg_list` legs are); a user-created ready spare is used the
        same way — that is what spares are for;
     2. else a **pending** spare exists — a spare whose leg and side both
        have `err_epoch == 0` and that is not ready yet (provisioning, or
        not yet reported `OK`) ⇒ wait for it;
     3. else `len(spare_leg_list) < MaxSpareLegPerGrp` ⇒ internal
        `CreateSpareLeg` on a fresh DN: `FindDnCandidatesAntiAffine(candExt =
        group.ext_cnt, candCnt = dn_batch_size, requiredCnt = 1, black = the
        DNs of every leg and spare of the group)`; tier 1 also excludes their
        `location`s — taken from the pass's own `SpState.DnByAddr`, a DN
        missing from it contributing none — and a tier-2 rescan without the
        location exclusion runs when tier 1 offers none of the **one** DN
        this step places, never when it merely falls short of `candCnt`
        (`architecture.md` §6.5), so a cluster with one failure domain still
        repairs; pick one; the new side provisions (§9.4), RW18 flips it, the
        cntlrs connect to it, and a later pass finds it ready;
     4. else `reaction skipped` (`spare_list_full`): after two repairs of one
        group the list holds two parked legs, and only `DeleteSpareLeg` by an
        operator frees a slot (Appendix B).

AR9. **The worker never**: deletes a td, subsystem, clone, transfer,
     migration or spare; touches a `RedundNone` leg; acts on a suppressed SP;
     starts a migration; or runs two reactions in one pass. Every automatic
     action re-homes redundancy or roles (§10.4).

---

## 12. Log records [LG]

JSON records per `log.md`, all at `Info` unless stated, all with the trace
id of RW10 where one exists. The `msg` strings are normative — the §14 suite
parses them.

| msg | attributes | when |
|---|---|---|
| `worker starting` | `roles`, `seed`, `endpoints`, `vote_interval`, `grace_time` | CM4 |
| `worker registered` | `role`, `seed` | first successful put of a role (VW2) |
| `membership observed` | `role`, `seed`, `state` (`live`/`dead`), `own` (bool) | every observed transition (VW3) |
| `membership committed` | `role`, `seed`, `state` (`member`/`nonmember`), `member_cnt` | VW6 |
| `shard owned` / `shard released` | `role`, `shard` (`%02x`), `seed` (the owner = self) | VW9 (released after the join, SW5) |
| `worker fenced` | `reason` (`heartbeat_stalled`/`watch_stalled`/`key_deleted`), `old_seed`, `new_seed` | VW8 |
| `worker stopping` | `seed` | CM5 |
| `revision worker started` / `revision worker stopped` | `role`, `shard`, `cluster_id`, `id` (+ `side_pointer`/`cntlr_pointer` for sp children) | SW3, RW11 |
| `cluster conf missing` | `cluster_id` | RW9 (once per idle period) |
| `syncup result` | ids, `revision`, `code`, `error?` | every `Syncup*` reply or failure (RW5) |
| `syncup rejected` | ids, `revision`, `code`, `details` (`Error` for stale revision) | RW5 |
| `health changed` | `role`, `cluster_id`, ids, `record` (`dn`/`cn`/`cntlr`/`leg`/`side`), `err_epoch` (0 or now), `reason` (`unreachable`/`error_row`/`recovered`), `res_name?` | HL1/HL2 transitions |
| `flip applied` | `kind` (`provisioned`/`created`), `cluster_id`, `sp_id`, ids, `revision` (the new `SpRev`) | RW18/RW19 |
| `bitmap pushed` | `kind` (`migr`/`clone`), ids, `bm_idx`, `code` | BM3 |
| `reaction applied` | `cluster_id`, `sp_id`, `kind` (`failover`/`grow_data`/`grow_meta`/`replace_cntlr`/`spare_create`/`spare_switch`), ids, `revision` | AR2 |
| `reaction skipped` | `cluster_id`, `sp_id`, `kind`, `reason` | AR2 |

Plus the `etcd *` records of `etcdutil` (§3) and the `grpc client *`
records of the interceptors (`grpc.md`) — the latter are what an agent's
log mirrors as `grpc server *` with the same `trace_id`.

---

## 13. Unit tests

Colocated `_test.go` files; the etcd-backed ones follow EU7 (real `etcd`
binary or skip). Fakes: an in-memory registry/watch source for the vote
worker, a fake clock for every timer, `bufconn` agents built from the
generated servers for the revision loop (as `common/interceptor_test.go`
does).

* **vote.go** — appear/disappear/reappear with the grace commit and the
  cancel-on-transition rule; a flapping key never commits; symmetric
  startup (nothing owned for one grace window; a dead-at-scan key never
  becomes effective); VW8 (a)/(b)/(c) each fence and mint a new seed; GC
  delete issued exactly on `nonmember` commits; ticket determinism (a
  golden sha256), largest-wins, and the ~1/n movement property over 256
  shards with 3→4 members (only shards whose new max is the joiner move);
  role independence.
* **shard.go** — scan-then-watch, coalescing, delete stops, compaction
  rescan diff, parse rejects malformed keys.
* **revision.go** — round timeout ⇒ unreachable; revision mismatch and
  `code != 0` ⇒ syncup; desired change ⇒ immediate syncup and coalescing
  under an in-flight call; `resyncWanted` ⇒ equal-revision re-send; stop
  lets an in-flight unary finish before closing the stream; connection
  reference counting; idle without cluster conf.
* **dnrole/cnrole/sprole** — golden requests from a fixture `SpState`
  (side/cntlr requests incl. migration src/dst confs, `id_to_slice` keys,
  standby list with a disabled cntlr); child diff on a changed endpoint;
  RW18/RW19 candidate selection (all four `created` conditions, the
  partial-map and `PROVISIONING` negatives).
* **health.go** — the HL1/HL2 tables row by row; transitions-only writes;
  standby leg rows ignored.
* **bmpush.go** — ascending order, one in flight, target rules, the
  `mod_revision` memo, failure ⇒ `resyncWanted`.
* **reaction.go** — priority and one-per-pass; every suppression; AR5's two
  triggers (an unhealthy primary past `primary_unhealthy`, and a `disabled`
  primary with no threshold wait — `TestReactionDisabledPrimaryFailsOver`);
  the AR6 parser on a real status line and on garbage; the pending rule
  before and after a grow becomes visible (data and meta units); AR7's
  sole-primary variant and old-CN black list; AR8 cases 1 and 2, the
  readiness and pending-spare rules, two-sides skip, `RedundNone` skip,
  `spare_list_full`, and the spare-create scan's tier-1 exclusion of the
  group's `location`s at `requiredCnt = 1`
  (`TestReactionSpareCreateExcludesGroupLocations`).
* **clusterconf.go** — key→id derivation, defaults resolution, delete.

---

## 14. Integration test plan

### 14.1 Goal and scope

Prove a real `dnv-worker` fleet — real `worker/`, `model/`, `etcdutil/`, a
real single-node etcd — against **fake** dn/cn agents on one server, driven
from the developer machine over ssh. The agents are fake because the data
plane is already proven by `dnagent_integtest.md` and `cnagent_integtest.md`;
what this suite proves is everything between etcd and the agent's gRPC
surface: membership and ownership, revision propagation, health bookkeeping,
the flips, the pushes, the reactions, and handoff. Happy paths plus every
failure the worker is specified to handle; error paths of etcd itself
(quorum loss, compaction races) are out of scope (§14.15).

| case | name | proves |
|---|---|---|
| S | `smoke` | one worker: full-state fan-out, Check rounds, the provisioned flip → bump → re-fan, the created flip, clean health |
| A | `revision` | bumps, a moved endpoint without a delete, stale/unknown replies, a deleted rev key |
| B | `health` | `err_epoch` set/cleared with the capacity keys; hang, kill, `PROVISIONING`, the sp-object table |
| C | `bitmap` | ordered one-in-flight pushes, targets, append, grown clone chunk, push failure ⇒ resync, primary change |
| D | `reaction` | failover (unhealthy and disabled primary alike), cntlr replacement (incl. sole-primary), data and meta auto-grow with the pending rule, leg repair cases 1 and 2, `spare_list_full`, suppression |
| E | `vote` | exact single ownership, join (~¼ moves, the rest stable), `SIGKILL`, `SIGTERM`, `SIGSTOP`/`SIGCONT` with the self-fence, attribution by trace id |
| F | `handoff` | a killed owner's shards are re-driven at the same revision; a flip mid-handoff happens once |

Cases run in that order, fail-fast, each in its own cluster (`cluster_name =
it-<case>`) and each after a restart of the worker fleet (§14.10), so
membership state never leaks between cases.

### 14.2 Deliverables and usage contract

Three artifacts under `integtest/`, next to the two agent suites:

* `integtest/worker_test.sh` — bash, `set -euo pipefail`, the orchestrator.
* `integtest/workerctl/main.go` — the **etcd driver that plays the gateway**
  (§14.8): formats §5.3 keys through `model`, marshals protos, bumps
  revisions, reads keys back as protojson. Imports `pb`, `common`, `model`,
  `etcdutil`. Runs **on the server** (etcd listens on localhost) via ssh.
* `integtest/fakeagent/main.go` — the fake agents (§14.9): one binary,
  `dn`/`cn` subcommands, the generated `DiskNodeAgent` /
  `ControllerNodeAgent` servers with the real server interceptors, driven
  by a behavior file. Imports `pb`, `common`.

The script builds `bin/dnv-worker` (`make build`) and the two drivers into
`integtest/bin/` (gitignored: `bin/`), downloads the pinned etcd release
tarball once into `integtest/bin/cache/` (URL and sha256 pinned in the
script; the server needs no internet), and `scp`s all five binaries (`etcd`,
`etcdctl`, `dnv-worker`, `fakeagent`, `workerctl`) to the server.

```
bash integtest/worker_test.sh [--only <case>] [--cleanup-only] user@192.168.10.20
```

* One positional arg: the ssh target. Passwordless ssh is assumed (checked
  in preflight); **no sudo** — nothing in this suite needs root.
* `--only <case>`: `smoke|revision|health|bitmap|reaction|vote|handoff`;
  setup and start-cleanup still run.
* `--cleanup-only`: scrub the server and exit.
* No prompts; exit 0 on success, non-zero on the first failure. Cleanup at
  **start, always**; at **end only on success** — a failing run leaves
  etcd's data, every log and every behavior file in place and dumps
  diagnostics (§14.13).

### 14.3 Topology

```
 driver (dev box, linux/amd64)
   | ssh + scp (orchestration; plain user)         reads logs with `ssh cat … | jq` (§14.10)
   v
 server = user@192.168.10.20  (<ip> = the address in the ssh arg)
   etcd          --listen-client-urls http://127.0.0.1:12379  --listen-peer-urls http://127.0.0.1:12380
   dnv-worker ×3 w1 w2 w3, --roles dn,cn,sp  (+ w4 started/stopped by case E)
   fakeagent dn ×4  <ip>:29600 … <ip>:29603      fakeagent cn ×3  <ip>:29700 … <ip>:29702
   workerctl        invoked over ssh against 127.0.0.1:12379
```

* The fake agents bind the server's own `<ip>` so the `addr_port` values in
  etcd look like production ones and the worker dials a real interface.
* Ports: 12379/12380 (etcd), 29600-29603 (fake DNs), 29700-29702 (fake
  CNs); none of the agent suites' ports (29528/29529/4200) are used, so a
  lab VM can host both.
* Per-server layout, all under `WORK=/var/tmp/dnv-worker-integtest`:

```
/var/tmp/dnv-worker-integtest/
  bin/            etcd etcdctl dnv-worker fakeagent workerctl   # scp'd
  etcd/           data dir                                       etcd.log
  w1/ w2/ w3/ w4/ worker.log                                     # one dir per worker
  dn0/ … dn3/     agent.log  behavior.json  state.json           # one dir per fake DN
  cn0/ … cn2/     agent.log  behavior.json  state.json           # one dir per fake CN
```

Launch lines (all `nohup … &` over ssh; stdout is the JSON log):

```
$WORK/bin/etcd --name dnv-it --data-dir $WORK/etcd \
  --listen-client-urls http://127.0.0.1:12379 --advertise-client-urls http://127.0.0.1:12379 \
  --listen-peer-urls http://127.0.0.1:12380 --initial-advertise-peer-urls http://127.0.0.1:12380 \
  --initial-cluster dnv-it=http://127.0.0.1:12380 > $WORK/etcd/etcd.log 2>&1 &

$WORK/bin/dnv-worker --etcd-endpoints 127.0.0.1:12379 --roles dn,cn,sp \
  --vote-interval $VOTE_INTERVAL --vote-grace-time $VOTE_GRACE > $WORK/w1/worker.log 2>&1 &

$WORK/bin/fakeagent dn --grpc-address <ip>:29600 --dir $WORK/dn0 > $WORK/dn0/agent.log 2>&1 &
$WORK/bin/fakeagent cn --grpc-address <ip>:29700 --dir $WORK/cn0 > $WORK/cn0/agent.log 2>&1 &
```

Process control: the three workers share one command line, so the script
records every process's PID from `$!` in `$WORK/<dir>/pid` and signals by
PID; cleanup falls back to `pkill -f 'bin/dnv-worker'`, `pkill -f
'bin/fakeagent'`, `pkill -f 'etcd --name dnv-it'`.

### 14.4 Assumptions and preflight checks

Hard assumptions (not checked): the server is linux/amd64 like the driver
(binaries are cross-built with `GOOS=linux GOARCH=amd64`), and its `<ip>` is
bindable locally.

Preflight (fail fast, install nothing):

* driver: `go`, `ssh`, `scp`, `curl`, `tar`, `sha256sum`, and a `jq` (system
  `jq`, else the `gojq` drop-in built into `integtest/bin/` exactly as the
  dn suite does); `make build` succeeds; the etcd tarball's sha256 matches
  the pin after download.
* server, via `ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new`:
  `bash`, `nohup`, `pkill`, `ss`, `tar`, `df`; the nine ports of §14.3 not
  listening (`ss -ltn`, checked after the start-cleanup); `/var/tmp` with
  ≥ 1 GiB free.

### 14.5 Identity plan

* Cluster per case: `cluster_name = it-<case>`; `workerctl put-cluster`
  stamps `creation_epoch = now` (like the gateway) and prints `cluster_id`,
  which the script keeps in `CID`.
* DNs: `dn_id 1..4` ↔ `dn0..dn3`, `addr_port = <ip>:2960{0..3}`, `location
  = loc-dn{0..3}` (distinct, so §6.3's location dedupe never blocks a
  pick). CNs: `cn_id 1..3` ↔ `cn0..cn2`, `<ip>:2970{0..2}`, `location =
  loc-cn{0..2}`.
* Shard codes are chosen by the script (`--shard`), not by a bucket walk:
  `00` by default; case E spreads DNs over `00`, `55`, `aa`, `ff`.
* SPs: `sp_name sp0`/`sp1`, `sp_id 1`/`2`, shard `00`; sub-object ids are
  assigned explicitly by the script and echoed by `workerctl` (it also
  advances `SpConf.next_id` past them, so a worker reaction allocates fresh
  ids above them).
* Names: tds `td0…`, subsystems `nqn.2024-01.io.dnv-it:ss0…`, clones
  `c0…`, migrations `m0…`.
* Free extents are the script's lever for deterministic allocation: a
  reaction that must pick a node is always given **exactly** the eligible
  candidates it needs (`put-dn`/`put-cn --free-ext` before the trigger).

### 14.6 Test-time constants

| knob | value | why |
|---|---|---|
| `--vote-interval` / `--vote-grace-time` | 2 / 6 | dead detection after 4 s, commit 6 s later; a fresh worker drives after 6 s |
| `health_check_conf.*_interval` | 1 | one round per second on every stream (`MinHealthCheckInterval`) |
| `event_threshold` (`primary`, `cntlr`, `side`, `leg`) | 2, 4, 3, 6 | `leg > side` as §7 requires; every reaction fires within seconds |
| `low_water_mark_pct` | 50 (case D sets it per step) | |
| `extent_size` | 64 MiB (`MinDnExtSize`) | irrelevant to fakes; keeps `GrowSlice` math small |
| `WAIT_SHORT` / `WAIT_MEMBERSHIP` / `WAIT_SYNCUP` | 5 / 20 / 65 s | polling budgets: a round is 1 s; a membership change needs ≤ 10 s; a syncup deadline is 60 s |

Every wait is a poll (`wait_until`, §14.10) — never a bare `sleep` except
the deliberate "nothing must happen for N seconds" negative checks, which
sleep N then assert.

### 14.7 Setup phase (after start-cleanup)

1. Start-cleanup: signal every recorded PID, the `pkill -f` fallbacks,
   `rm -rf $WORK`.
2. `mkdir -p` the §14.3 layout; `scp` the five binaries into `$WORK/bin`.
3. Start etcd; wait for `workerctl ping` (an `etcd get` of a missing key
   succeeding) within `WAIT_SHORT`.
4. Start the four fake DNs and three fake CNs with an empty
   `behavior.json` (`{}`: every row `OK`, sides instantly zeroed); wait for
   the seven ports.
5. Start `w1`, `w2`, `w3`; wait for `worker registered` × 3 roles in each
   log; wait `VOTE_GRACE + 2` s; assert the ownership table (§14.10): every
   `(role, shard)` has exactly one owner, and each worker owns ≥ 50 of the
   256 shards per role (mean 85, σ ≈ 7.5 — a bound 4.7σ out).
6. `SETUP_DONE=1`.

### 14.8 The driver: `workerctl`

Global flags: `--endpoints 127.0.0.1:12379`, `--cluster <name>` (every
subcommand except `put-cluster`/`ping`/`list-*` reads `ClusterConf` first to
derive `cluster_id`, exactly as the gateway must, §5.2), `--trace-id`
(`it-<case>-<step>`, so the driver's `etcd put` records correlate with the
stage), `--timeout` (default 10 s). Ids accept `0x` hex. Every read prints
protojson on stdout; every mutation prints the ids it assigned. Exit non-zero
on any error.

| cmd | flags | writes |
|---|---|---|
| `ping` | | one `Get` of `dnv ping` |
| `put-cluster` | `--name`, `--dn-interval --cn-interval --side-interval --cntlr-interval`, `--extent-size`, `--lwm`, `--dn-batch --cn-batch` | `ClusterConf` (+ zeroed `DnGlobal`/`CnGlobal`/`SpGlobal`); prints `cluster_id` |
| `put-dn` | `--id --shard --addr --location --total-ext --free-ext [--disabled] [--side sp:leg:side]…` | `DnConf`, `DnRev` (`revision` = current + 1, or 1), `DnCapacity` per §5.6 through `model.MaintainDnCapacity` — one STM |
| `put-cn` | `--id --shard --addr --location --total-ext --free-ext [--disabled] [--cntlr sp:cntlr]…` | `CnConf`, `CnRev`, `CnCapacity` |
| `bump-rev` | `dn\|cn\|sp --id --shard` | `revision += 1` in place (never delete + put), prints the new value |
| `move-dn` | `--id --shard --addr <new>` | rewrites `DnRev.addr_port` and moves `DnConf`/`DnCapacity` to the new endpoint in one STM (the §5.5 moved-node case) |
| `del-rev` | `dn\|cn\|sp --id --shard` | deletes the rev key only |
| `put-sp` | `--name --id --shard --slots 0,1,2 --level N --thresholds p,c,s,l --lwm N --cntlr id:cn_id:slot:primary…  --slice id:idx…  --group slice:grp:meta\|data:ext_cnt:none\|raid1…  --leg grp:leg:idx…  --side leg:side:dn_id:slot…` | `SpConf` (+ `next_id` past every id), `SpName`, every `Cntlr`, every `Slice` (sides `provisioned = false`), `SpRev{revision = 1, sp_name}`; the DN/CN pointer lists, budgets, capacity keys and `DnRev`/`CnRev` bumps — the `CreateStoragePool` STM with explicit placement |
| `put-td`, `put-ss`, `put-clone`, `put-xfer`, `put-migr` | the message's fields (`--migr name:id:src_side:dst_side` also appends the dst `Side` to the leg) | the record + the `SpConf` list entry; bump `SpRev` |
| `put-bitmap` | `--kind clone\|migr --name --bm-idx --hex` | the chunk (+ `bm_cnt` on the parent); bump `SpRev` |
| `set-cntlr` | `--sp --id --primary=… --disabled=…` | rewrites the `Cntlr`; bump `SpRev` |
| `set-level` | `--sp --level N` | `SpConf.sp_level`; bump `SpRev` |
| `set-lwm` | `--sp --pct N` | `SpConf.bdev_conf.dm_pool_conf.low_water_mark_pct`; bump `SpRev` |
| `set-free` | `dn\|cn --id --free-ext N` | rewrites `free_ext_cnt` + capacity key; no rev bump |
| `get` | `--key "<full key>"` | prints the value as protojson, choosing the message type from the key's second field |
| `get-dn`/`get-cn`/`get-rev`/`get-sp`/`get-cntlr`/`get-slice`/`get-td` | ids | typed reads (`get-sp` = `SpConf` + every `Cntlr` + every `Slice` + every td, one snapshot) |
| `list-keys` | `--prefix` | keys only |
| `list-workers` | `--role` | seed + epoch per registration |

`workerctl` never dials an agent and never sleeps; it is the gateway's write
path with explicit placement (§0 item 19).

### 14.9 The fake agent: `fakeagent`

`fakeagent dn|cn --grpc-address <ip:port> --dir <dir>`: serves the generated
service on a plaintext listener with `common.GrpcUnaryServerInterceptor` /
`GrpcStreamServerInterceptor`, so `agent.log` carries one `grpc server
request`/`reply`/`recv`/`send` record per message with the caller's
`trace_id` — the suite's evidence of what the worker sent and which worker
sent it (RW10). Rules:

* **Revision gate**, per object (`dn`, `cn`, `side sp:leg:side`, `cntlr
  sp:cntlr`): a `Syncup*` with `revision <` the stored one ⇒ `code =
  ReplyCodeStaleRevision`; `≥` ⇒ apply (store the request and revision).
  `Push*` with a lower revision ⇒ code 1; a `migr_id`/`clone_id` not in the
  object's last request ⇒ `ReplyCodeUnknownObject`. `SyncupSide`/`CheckSide`
  for a side pointer absent from the DN's last `SyncupDn` ⇒ code 2, and
  likewise a cntlr absent from the CN's last `SyncupCn` — the real agents'
  ordering rule, which the worker's independent roles must survive (RW5).
* **`state.json`** in `--dir`: the last applied request and revision per
  object and the received chunks per migration/clone (index + byte length),
  written on every apply; loaded at start — a killed and restarted fake
  replies like a restarted real agent (last revision, `bm_idx_list` from
  the files), so case B's kill/restart is faithful.
* **`*Info` shape from the last request**: `SideInfo.cn_id_to_*` has one row
  per `primary_cn_id`/`standby_id_list` entry; `CntlrInfo` maps have one row
  per slice/group/leg/td/ss/ns/clone/xfer of the last `SyncupCntlr`;
  `td_id_to_thin_info` is filled only when the last request's
  `cntlr.primary` is true (CN14 "primary only") and `thin_ok` says so.
* **`behavior.json`**, re-read on every request when its mtime changed:

  ```json
  {
    "default": {"status": "OK"},
    "objects": {
      "dn": {"rows": {"meta_info": {"status": "PROVISIONING"}}},
      "side 1:3:5": {"zeroed_ext_cnt": 0, "total_ext_cnt": 2},
      "cntlr 1:1": {
        "thin_ok": true,
        "rows": {"slice_id_to_dm_pool.3": {"status": "OK",
                 "details": "0 8192 thin-pool 5 40/1024 600/1024 - rw discard_passdown queue_if_no_space - 512"},
                 "leg_id_to_leg.7": {"status": "ERROR", "details": "probe timeout"}},
        "hang": false, "drop_stream": false, "reply_code": 0
      }
    }
  }
  ```

  Per object: `status` (the default for every row), `details`, `rows`
  (per-row override, key = `<map>.<id>` or `<field>`), `zeroed_ext_cnt` /
  `total_ext_cnt` (sides; default `total = ext_cnt` of the request and
  `zeroed = total` — instant provisioning), `thin_ok` (cntlr) and
  `thin_missing_slices` (a partial map for the created-flip negative),
  `bm_idx_list` (override of the derived applied set), `hang` (accept a
  `Check*` request and never reply), `drop_stream` (close the `Check*`
  stream on the next request), `reply_code` (force `agent_reply.code` on
  every reply of the object). `Get*Size` replies a configured size,
  `Get*Info` the same info, `Get*Bm` an empty bitmap; the worker never calls
  them.

### 14.10 Conventions

* **Remote execution**: `sshw <cmd>` echoes `[server] <cmd>` and runs it as
  the plain user; `ctl <args>` = `sshw $WORK/bin/workerctl --endpoints
  127.0.0.1:12379 --cluster $CLUSTER --trace-id $TRACE <args>`.
* **Log reading on the driver**: `rlog <path>` = `ssh … cat <path>`; every
  assertion is `rlog … | jq …` on the driver — the server needs no `jq`.
* **`wait_until <secs> <label> <cmd…>`** polls twice a second until the
  command exits 0, else `die`. `assert_none_for <secs> <cmd…>` sleeps then
  asserts the command exits non-zero (the negative form).
* **Ownership table** (`owners <role>`): from every worker log, the shards
  with more `shard owned` than `shard released` records for that role; the
  union over workers must cover 256 shards exactly once.
* **Seeds**: `seed_of w1` = the `seed` of the latest `worker registered`
  record in `w1/worker.log`; `seed8` = its first 8 hex chars, matched
  against `trace_id | split("-")[0]` in the fakes' logs.
* **Request counters**: `reqs <agent> <method> [<jq filter>]` counts `grpc
  server request` records (unary) or `grpc server recv` (streams) for a
  method; `last_req` prints the newest one's `data`.
* **Revisions**: the script trusts `workerctl` (it prints every new
  revision) and keeps `DNREV[i]`, `CNREV[i]`, `SPREV[name]`.
* **Stages** mint `it-<case>-<step>` trace ids for `workerctl`; the worker
  mints its own (RW10), so worker-side correlation is by key and time.
* **Fleet restart before every case**: `SIGTERM` the workers, wait for
  `worker stopping` and exit, start them again, wait `VOTE_GRACE + 2` s,
  assert the ownership table. Each case then creates its own cluster.

### 14.11 Cases

Every step names the assertion; "within" means `wait_until` with the given
budget. Ids: `S<id>` side, `L<id>` leg, `G<id>` group, `C<id>` cntlr.

**S — `smoke`** (`--only smoke`; the fleet is reduced to `w1` alone for this
case: `w2`/`w3` are `SIGTERM`ed first and restarted after)

1. `put-cluster it-smoke` (intervals 1, extent 64 MiB). `put-dn 1 dn0`,
   `put-dn 2 dn1` (free 8), `put-cn 1 cn0`, `put-cn 2 cn1` (free 64).
2. Within `WAIT_SHORT`: `dn0` and `dn1` received `SyncupDn` with `revision
   1`, empty `side_pointer_list`, `extent_size 67108864`; `cn0`/`cn1`
   received `SyncupCn` `revision 1`; `CheckDn` `recv` records on `dn0`
   increase by ≥ 3 over 5 s with `show_info false` after the first.
3. `put-sp sp0`: cntlrs `C1` on cn 1 slot 0 primary, `C2` on cn 2 slot 1;
   slice `1`; meta group `G1` ext 1 raid1 legs `L1`(side `S1` on dn 1)
   `L2`(`S2` on dn 2); data group `G2` ext 2 raid1 legs `L3`(`S3` dn 1)
   `L4`(`S4` dn 2). `put-td td0`, `put-ss ss0` with one namespace on td0.
4. Within `WAIT_SYNCUP`: `SyncupDn revision 2` at both DNs listing their two
   side pointers; `SyncupSide` for `S1..S4` with `provisioned false`,
   `primary_cn_id 1`, `standby_id_list [2]`, `ext_cnt` 1/2, `sp_level 0`;
   `SyncupCntlr` at cn0 and cn1 with `cntlr.primary` true/false,
   `id_to_slice["0000000000000001"]` present, `td_list[0].td_id`, the
   subsystem under its nqn. Tolerated: earlier `SyncupSide` replies with
   `code 2` (a side before its `SyncupDn`) — the final state is asserted.
5. The fakes report `zeroed == total` by default ⇒ within `WAIT_SHORT`:
   `get-slice 1` shows every side `provisioned true`; `get-rev sp` revision
   ≥ 2; a `flip applied kind=provisioned` record in `w1`; `SyncupSide` with
   `provisioned true` at both DNs and `SyncupCntlr` with the new revision at
   both CNs.
6. `cn0` behavior `cntlr 1:1 {thin_ok: true}` ⇒ within `WAIT_SHORT`: `get-td
   td0` `created true`; `flip applied kind=created`; `SpRev` bumped once
   more; then `assert_none_for 5` that `SpRev` changes again (no flip loop).
   Negative first: with `thin_missing_slices: [1]` for 3 s nothing flips.
7. `CheckSide`/`CheckCntlr` `recv` counters increase on every fake.
8. `get-dn 1`/`get-cn 1`: `err_epoch 0`; `list-keys dn_capacity` and
   `cn_capacity` show all four nodes.

**A — `revision`**

1. `put-cluster it-revision`; `put-dn 1 dn0`; `bump-rev dn 1` ⇒ within
   `WAIT_SHORT` `SyncupDn revision 2` at dn0, and the next `CheckDn` `recv`
   carries `revision 2`.
2. `move-dn 1 --addr <ip>:29601` ⇒ within `WAIT_SHORT`: `SyncupDn revision
   3` at **dn1**; a `grpc server stream close` for `CheckDn` in dn0's log;
   no `SyncupDn` at dn0 after the move; no `revision worker stopped` in the
   owner's log (a put, not a delete).
3. Stale: edit dn1's `state.json` so the DN's stored revision is 9 ⇒ the
   next round's mismatch re-issues `SyncupDn 3`, rejected `code 1`; within
   5 s ≥ 2 `syncup rejected` records at `Error`, `err_epoch` stays 0;
   restore the file ⇒ a clean round, no further rejections.
4. Unknown: `put-cn 1 cn0`; behavior `cn {reply_code: 2}` ⇒ every round
   re-issues `SyncupCn` (≥ 3 in 5 s), `err_epoch` stays 0; clear ⇒ stops.
5. `del-rev dn 1` ⇒ within `WAIT_SHORT`: `revision worker stopped` in the
   owner's log, `stream close` in dn1's; `assert_none_for 3` any new
   `CheckDn` `recv` at dn1.
6. `put-sp sp0` (one cntlr on cn 1, one slice, one `RedundNone` data group
   with `S1` on dn… — dn 1's rev was deleted, so `put-dn 2 dn2` first);
   `bump-rev sp 1` ⇒ `SyncupSide` and `SyncupCntlr` with the new revision at
   every target within `WAIT_SHORT`, and the sp child's `CheckSide` request
   revision follows.

**B — `health`**

1. `put-cluster it-health`; `put-dn 1 dn0 --free-ext 8`, `put-cn 1 cn0`;
   both capacity keys present.
2. dn0 behavior `dn {rows: {disk_info: {status: ERROR, details: "io"}}}` ⇒
   within `WAIT_SHORT`: `get-dn 1` `err_epoch != 0`; `dn_capacity` key
   absent; `health changed record=dn reason=error_row`.
3. Clear ⇒ `err_epoch 0`, the capacity key back at the same bin/free.
4. `hang: true` ⇒ missed reply ⇒ `err_epoch` set, reason `unreachable`,
   `stream close` at dn0 each round; `hang: false` ⇒ cleared within
   `WAIT_SHORT`.
5. Kill dn0 (`SIGKILL`) ⇒ `err_epoch` set within `WAIT_SHORT`; restart it
   (state.json intact) ⇒ cleared; `assert_none_for 3` no new `SyncupDn`
   (the restarted fake reports the stored revision, so no re-sync).
6. `dn {rows: {meta_info: {status: PROVISIONING}}}` ⇒ `assert_none_for 3`
   `err_epoch` stays 0 and the capacity key stays.
7. `meta_info ERROR "disk lacks Write Zeroes"` ⇒ set (a plain `ERROR`).
8. `put-cn 2 cn1`; `put-sp sp0`: `C1` cn 1 primary, `C2` cn 2; one slice,
   raid1 data group `G1` legs `L1`(`S1` dn 1) `L2`(`S2` dn 2 — `put-dn 2
   dn1` first). Wait for provisioning and a clean state (`get-slice`
   `err_epoch` 0 everywhere; `SPREV` noted).
   * cn0 (primary) `rows leg_id_to_leg.<L1> ERROR` ⇒ `Leg.err_epoch` set on
     `L1` within `WAIT_SHORT`; clear ⇒ 0.
   * cn1 (standby) the same row ⇒ `assert_none_for 3` no `Leg.err_epoch`.
   * dn0 `side 1:<L1>:<S1> rows side_dev_info ERROR` ⇒ `Side.err_epoch`
     set; clear ⇒ 0. `hang` on the side object ⇒ set (`unreachable`).
   * cn0 `rows slice_id_to_dm_pool.1 ERROR` ⇒ `Cntlr.err_epoch` on `C1`;
     clear ⇒ 0.
   * Throughout: `get-rev sp` unchanged (health never bumps).

**C — `bitmap`**

1. `put-cluster it-bitmap`; `put-dn 1 dn0`, `put-dn 2 dn1`, `put-cn 1 cn0`,
   `put-cn 2 cn1`; `put-sp sp0` with `C1` primary on cn 1, `C2` on cn 2, one
   slice, raid1 data group `G1` legs `L1`(`S1` dn 1) `L2`(`S2` dn 2), td
   `td0`. Wait provisioned. `put-migr m0 id 3 src S1 dst S3 on dn 2 slot 1
   bm_cnt 0`; `put-bitmap migr m0 0 <hex>`, `put-bitmap migr m0 1 <hex>`
   (`bm_cnt 2`). `put-clone c0 dst td0 src_slice_cnt 2`; `put-bitmap clone
   c0 0 <hex>`, `1 <hex>`.
2. Within `WAIT_SHORT`: dn1 received `PushMigrBitmap bm_idx 0` then `1`,
   the second's `grpc server request` timestamp after the first's `grpc
   server reply` (one in flight, ascending), `revision` = the current
   `SpRev`; the next `SyncupSide` reply carries `bm_info.bm_idx_list [0,1]`
   and `assert_none_for 3` no further pushes. dn0 (the src) received none.
3. cn0 received `PushCloneBitmap` `0`, `1`; cn1 (standby) none.
4. `put-bitmap migr m0 2 <hex>` ⇒ exactly one new push at dn1, `bm_idx 2`.
5. `put-bitmap clone c0 0 <longer hex>` ⇒ a second `PushCloneBitmap bm_idx
   0` at cn0 with the new length (the `mod_revision` memo), no push of `1`.
6. dn1 behavior `side … {reply_code: 1}` and `put-bitmap migr m0 3` ⇒ the
   push is rejected; within 3 rounds an equal-revision `SyncupSide` is
   re-issued (`syncup result` with the same revision twice); clear ⇒ the
   push succeeds.
7. `set-cntlr --sp 1 --id 1 --primary=false`, `--id 2 --primary=true` ⇒
   within `WAIT_SHORT` cn1 receives `PushCloneBitmap 0` and `1`; cn0 none
   after the change.

**D — `reaction`** (thresholds 2/4/3/6; `lwm` set per step)

1. `put-cluster it-reaction --dn-batch 16 --cn-batch 16`; DNs 1..4 on
   dn0..dn3 with `--free-ext 8`, CNs 1..3 on cn0..cn2 with `--free-ext 64`.
   `put-sp sp0`: `C1` cn 1 slot 0 primary, `C2` cn 2 slot 1; slice `1`;
   meta `G1` ext 1 raid1 `L1`(`S1` dn 1) `L2`(`S2` dn 2); data `G2` ext 2
   raid1 `L3`(`S3` dn 1) `L4`(`S4` dn 2); td `td0`. Wait provisioned,
   created; note `SPREV`.
2. **Failover.** cn0 `cntlr 1:1 rows ss_id_to_subsystem.<ss> ERROR` ⇒
   `Cntlr.err_epoch` on `C1`; within `2 + WAIT_SHORT` s: `get-cntlr` `C2
   primary true`, `C1 primary false`; `reaction applied kind=failover`;
   `SyncupSide primary_cn_id 2` at both DNs; `SyncupCntlr cntlr.primary`
   true at cn1. Before the threshold (`assert_none_for 1`) nothing flips.
3. **Replacement.** Keep `C1` unhealthy; `set-free cn 1 64` (the CN node is
   healthy and allocatable — the black list is what must exclude it);
   within `4 + WAIT_SHORT` s: `C1` gone from `cntlr_id_list`; a new cntlr
   `C5`(the next id) on **cn 3** (the only other eligible: not hosting sp0)
   with `cntlid_slot 0`, `primary false`; `get-cn 1` `cntlr_ptr_list` empty
   and `free_ext_cnt` restored, `get-cn 3` pointer present and budget
   debited; `CdcEntry` of `ss0` lists cn 2 and cn 3, not cn 1; `SyncupCn`
   at cn0 (empty list) and cn2 (one pointer); `SyncupCntlr` at cn2;
   `reaction applied kind=replace_cntlr`.
4. **Sole primary.** `put-sp sp1` (id 2): one cntlr `C1'` on cn 2 slot 0
   primary; one slice, `RedundNone` data group ext 1 with a side on dn 3.
   cn1 `cntlr 2:1 rows ss_id_to_subsystem… ERROR` ⇒ after 4 s: replaced by a
   new **primary** cntlr on cn 3 (`set-free cn 1 0` beforehand so cn 3 is
   the only candidate) with slot 0; `reaction skipped kind=failover
   reason=no candidate` recorded first.
5. **Data grow.** `set-lwm sp0 50`; `set-free dn 3 8`, `set-free dn 4 8`,
   `set-free dn 1 0`, `set-free dn 2 0` (exactly two candidates for a 2-leg
   group). cn (sp0's current primary) `rows slice_id_to_dm_pool.1 OK
   details "0 16384 thin-pool 1 10/1024 600/1024 - rw …"` ⇒ within
   `WAIT_SHORT`: `get-slice 1` `data_grp_list` length 2, the new group
   `ext_cnt 2`, `meta_blocks`/`data_blocks` per §3.6 (64 MiB extents, 1 MiB
   blocks, 128-block chunks ⇒ `meta_blocks 3`, `data_blocks 125`), two
   sides on dn 3 and dn 4 `provisioned false`; `DnRev` of 3 and 4 bumped,
   `free_ext_cnt 6` each; `CnRev` of every cntlr CN bumped and budgets
   debited; `reaction applied kind=grow_data`. **Pending**: keep the same
   details for 5 s ⇒ `assert_none_for 5` a second grow (`reaction skipped
   reason=grow_pending` present). Report `600/2048` ⇒ still no grow (29 %).
   `set-free dn 1 8`, `dn 2 8`; report `1100/2048` ⇒ a third data group.
6. **Meta grow.** Report `900/1024` metadata (data below lwm) ⇒ one meta
   group with `ext_cnt 1` (the ladder: current meta total 1 ⇒ +1);
   `kind=grow_meta`; report metadata `900/2048` ⇒ pending until totals
   reflect the second group, then a group of `ext_cnt 2`. `set-lwm 101` ⇒
   `assert_none_for 3` no grow at any usage.
7. **Leg repair, case 1.** Reset free extents so exactly one DN not in `G2`
   is eligible. The primary reports `leg_id_to_leg.<L3> ERROR` (side rows
   stay OK) ⇒ `Leg.err_epoch` on `L3`; `assert_none_for 4` no spare (below
   6 s); within `6 + WAIT_SHORT`: `G2.spare_leg_list` has a new leg with
   one side on the eligible DN, `provisioned false`, `reaction applied
   kind=spare_create`; the fake zeroes instantly ⇒ RW18 flips it; the
   primary's default rows report the new spare `OK` ⇒ within `WAIT_SHORT`
   `kind=spare_switch`: the new leg in `leg_list` at `L3`'s position, `L3`
   in `spare_leg_list` with its `err_epoch` intact; `SyncupSide` for the
   spare's side carries `primary_cn_id`; `assert_none_for 4` no further
   repair of `L3`.
8. **Leg repair, case 2.** On `G1`: the primary reports `leg_id_to_leg.<L1>
   ERROR` **and** dn0 reports `side 1:<L1>:<S1> side_dev_info ERROR` ⇒
   both epochs set; within `3 + WAIT_SHORT` s (before the 6 s leg
   threshold) `kind=spare_create` on `G1` (one eligible DN prepared);
   then `spare_switch`. Negative first: side `ERROR` **without** the leg
   row ⇒ `assert_none_for 5` nothing.
9. **`spare_list_full`.** `G1` now holds one parked leg; the script repeats
   step 8 on `G1`'s surviving original leg to park a second one, then fails
   the new active leg ⇒ `reaction skipped reason=spare_list_full` within
   `6 + WAIT_SHORT`, no allocation.
10. **Suppression.** `set-level sp0 48` (`NO_THINPOOL`); fail the primary ⇒
    `assert_none_for 5` no failover; `set-level 0` ⇒ failover within
    `WAIT_SHORT`. `set-cntlr --disabled=true` on a standby, fail it ⇒
    `assert_none_for 6` no replacement.
11. **Disabled primary** (AR5's other trigger, §8.6). Step 10's disabled
    standby is the SP's only other cntlr, so it is made electable again
    first: clear its row and wait `err_epoch` back to 0 **while it is still
    disabled** (enabled and unhealthy, AR7 would replace it before this step
    could use it), then `set-cntlr --disabled=false`. Now `set-cntlr
    --disabled=true` on the **primary** ⇒ within `WAIT_SHORT` — one pass,
    with no threshold added to it: `reaction applied kind=failover`; the
    standby `primary true`, the disabled cntlr `primary false`; `err_epoch
    0` on both, so the flag is the only trigger the failover can have come
    from; a `SyncupCntlr` with `cntlr.primary true` at the new primary's CN.

**E — `vote`**

1. `put-cluster it-vote`; DNs 1..4 on shards `00`, `55`, `aa`, `ff` — so
   syncups happen on shards spread over the ownership table.
2. `owners dn/cn/sp` exact (§14.7 step 5 holds).
3. **Join**: start `w4`; within `WAIT_MEMBERSHIP`: `membership committed
   state=member member_cnt=4` for `w4`'s seed in `w1..w3` for every role;
   `w4` logs the same for the three others and itself; `owners` exact again;
   `w4` owns 40..90 shards per role; every shard **not** owned by `w4` has
   the same owner as before (stability); each moved shard shows `shard
   released` in exactly one old owner's log.
4. **Attribution**: for a DN whose shard moved, its fake log shows
   `SyncupDn` with the same revision from the old owner's `seed8` and then
   the new owner's; after settling, every `CheckDn recv` in the last 3 s
   carries the new owner's `seed8` only.
5. **`SIGKILL w2`** (no key delete): within 4 + `WAIT_SHORT` s `membership
   observed state=dead` for `w2`'s seed in the others; within
   `WAIT_MEMBERSHIP` `membership committed state=nonmember`; `list-workers`
   no longer lists `w2` (the VW6 delete); `owners` exact over `w1 w3 w4`.
6. **`SIGTERM w4`**: its log ends with `worker stopping`; `list-workers`
   drops it at once; the others commit `nonmember` within `VOTE_GRACE + 2`
   s — measurably sooner than step 5 (the script compares the two commit
   latencies against DERIVED bounds: `SIGTERM` < `VOTE_GRACE + 2` = 8 s;
   `SIGKILL`, measured from the signal, falls in `[VOTE_INTERVAL +
   VOTE_GRACE, 2·VOTE_INTERVAL + VOTE_GRACE]` = [8, 10] s, because the
   dead-detection window depends on where the kill lands in the victim's
   heartbeat cycle; and the ordering `SIGTERM < SIGKILL`, which is what the
   step actually proves); `owners` exact over
   `w1 w3`.
7. **`SIGSTOP w3`**: within 4 + `WAIT_SHORT` s `w1` observes it dead, within
   `WAIT_MEMBERSHIP` commits it and owns all 256 shards per role. **`SIGCONT
   w3`**: `w3` logs `worker fenced reason=heartbeat_stalled` with a new
   seed; `w1` commits the new seed within `WAIT_MEMBERSHIP`; `owners` exact
   over `w1 w3`, each ≥ 50; the old seed's key is absent from
   `list-workers`; `w3` logged `shard released` for every shard it held
   before driving anything under the new seed.
8. Start `w2` and `w4` again only through the §14.10 fleet restart of the
   next case.

**F — `handoff`**

1. `put-cluster it-handoff`; `put-dn 1 dn0`; find the owner of `(dn, 00)`
   from `owners`; `SIGKILL` it. Within `WAIT_MEMBERSHIP`: another worker
   owns `00`; dn0's log shows a `SyncupDn` with the **same** revision from
   the new `seed8`, then `CheckDn` rounds from it; `get-dn 1` `err_epoch 0`
   throughout.
2. `put-cn 1 cn0`, `put-sp sp0` (one cntlr on cn 1; one slice; raid1 group
   with sides on dn 1 and dn 2 — `put-dn 2 dn1`) with dn0/dn1 behavior
   `zeroed_ext_cnt 0` (sides never finish provisioning). Find the owner of
   `(sp, 00)`, `SIGKILL` it; wait for the new owner's `SyncupSide`
   (`provisioned false`) — the same revision again. Set `zeroed = total` on
   both fakes ⇒ within `WAIT_SHORT` `flip applied kind=provisioned`
   **exactly once** across all worker logs and `SpRev` advanced by exactly
   one (the flip is observation-driven and needs no recovery).
3. `owners` exact over the surviving workers.

### 14.12 Teardown and cleanup (`cleanup()`, also `--cleanup-only`)

Signal every recorded PID (`SIGTERM`, then `SIGKILL` after 5 s), the three
`pkill -f` fallbacks, `rm -rf $WORK`. On success it runs at the end; at the
start it always runs. Nothing outside `$WORK` is touched — the suite leaves
no kernel state, no packages, no users.

### 14.13 Failure diagnostics

On the first failure: the failing stage and trace id; `list-workers` for
every role; the ownership table; the last 40 records of every worker log
filtered to `msg != "etcd get"`; the last 20 `grpc server *` records of
every fake; `list-keys --prefix dnv`; `ss -ltn` for the nine ports; and
the pull hint `jq 'select(.trace_id=="…")'` per log. Debris stays.

### 14.14 Coverage matrix

| rule | cases |
|---|---|
| VW1-VW7, VW9-VW11 | E (all), F (handoff), every case's fleet restart (VW7) |
| VW8 (a) | E step 7; (b)/(c): unit tests only (§13) — a broken watch with live puts cannot be induced from outside |
| SW1-SW3, SW5, SW6 | S, A (delete), F |
| SW4 | unit tests only (compaction) |
| RW1-RW12 | S, A, B; RW10 by E/F attribution; RW11 by every fleet restart |
| RW13-RW21 | S (builders), A (moved endpoint), C (migration confs), D (grow, spare confs) |
| HL1-HL6 | B |
| BM1-BM6 | C |
| AR1-AR9 | D |
| MD2-MD6 (through the worker and `workerctl`) | every case; MD6 ops by D |
| EU1-EU6 | every case |
| CM1-CM6 | every launch, E (SIGTERM), fleet restarts |
| LG | every assertion |

### 14.15 Out of scope (v1)

Real agents (the agent suites); more than one server; a 3-member etcd;
etcd quorum loss, restore, or compaction races (SW4 is unit-tested only);
clock-skew injection (the design does not depend on clocks, VW4); TLS/auth
(Appendix D "the fabric is trusted"); a worker whose watch breaks while
its puts succeed (VW8 (b), unit-tested); `RedundNone` leg failures (nothing
automatic is specified); CN-side `PushCloneBitmap` for a clone whose
primary is `PROVISIONING`-deferred.

---

## 15. Amendments to companion documents (applied)

Recorded for traceability; every edit below is already applied to the named
file, in the style of `ThinDeviceCreated.md` §8 and the agents' §5 sections.

### `architecture.md`

* §1 process table — the `dnv-worker` row points at this document.
* §5.3 — the `(worker registry)` row becomes the `WorkerReg` message:
  `{p} worker {role} {seed}`, value = the heartbeat `epoch`, refreshed every
  `DefaultVoteWorkerInterval`, no lease, deleted by its owner on shutdown or
  by peers after it is committed dead.
* §7 — new validation rule: `EventThreshold.leg_unhealthy` MUST exceed
  `EventThreshold.side_unhealthy` after defaults are resolved
  (`INVALID_ARGUMENT` otherwise), because the leg repair fires on the side
  threshold when the DN looks dead and on the leg threshold otherwise.
* §8.12 — `SwitchSpareLeg` is "invoked by users or by the sp-worker's leg
  repair of §10.4" (was "the dn-worker reaction").
* §10.1 — replaced by a summary of §6 and a pointer here; the lease and HRW
  text is gone.
* §10.4 — the `side_unhealthy` migration bullet and the `leg_unhealthy`
  bullet are replaced by the one leg-repair rule (Case 1 / Case 2, ready
  spare → switch, else create, old leg parked, the migration withdrawn with
  its reason); the intro points at §11 for cadence, priority and
  suppression.
* §13 — the `dnv-worker` invocation shows the new flags; the flags sentence
  points at §5.
* Appendix B — new decision **[D17]** (heartbeat membership, observer-local
  liveness, grace windows, sha256 tickets; why not a lease; why not the
  stored epoch).
* Appendix C — the amendment entry for this document.
* Appendix D — three new limits: the partitioned observer is tolerated, not
  repaired; the undriven windows; a group's spare list can fill with parked
  legs.

### `layout.md`

* §1 — the etcd client dependency names this document and the v3.6 line.
* §2 — `doc/dnv-worker.md` in the tree; `etcdutil.go`'s comment names the
  §3 surface; the `worker/` file list is the §1 table of this document
  (`vote.go`, `shard.go`, `revision.go`, `dnrole.go`, `cnrole.go`,
  `sprole.go`, `clusterconf.go`, `health.go`, `bmpush.go`, `reaction.go`);
  new `model/` package (`keys.go`, `stm.go`, `capacity.go`, `alloc.go`,
  `ops.go`); the `integtest/` tree (both agent suites, `worker_test.sh`,
  `workerctl/`, `fakeagent/`, `bin/` with its `cache/`).
* §3 — new row `model` (may import `common`, `pb`, `etcdutil`); `gateway`,
  `worker` and `cdc` may import `model`; a row for the `integtest/*`
  drivers.
* §5 — the `cmd/dnv-worker/main.go` wiring bullet.
* §6 — step 3 becomes `etcdutil/` + `model/`; step 6 names §6-§11 and the
  §14 suite.
* §7 — item 4 also checks that `model` imports no service package.
* §8 — the amendment entry.

### `dependencies.md`

* The planned etcd client entry names this document and pins the v3.6 line.

### `README.md`

* The documents sentence and the "not yet implemented" paragraph point at
  this document for `etcdutil/`, `model/`, `worker/` and `cmd/dnv-worker`.

### Deferred to implementation time

* `pb/schema.proto`: the `WorkerReg` message of §2.2, regenerated with
  `make gen` (README toolchain) in the same change that adds `etcdutil/`.
* `common/constants.go`: the §2.1 block.
* `go.mod`: `go.etcd.io/etcd/client/v3` (and its transitive requirements)
  when `etcdutil/` lands; `dependencies.md` then moves it to "Current".

---

## 16. Acceptance checklist

1. `go build ./...`, `go vet ./...`, `go test ./...` pass; `go test
   ./etcdutil/ ./model/` also pass with `ETCD_BIN` set (EU7).
2. `go list -deps ./cmd/dnv-agent ./cmd/dnvctl | grep etcd` finds nothing;
   `go list -deps ./cmd/dnv-worker | grep etcd` finds the client; `model`
   imports none of `gateway`, `worker`, `agent`, `cdc`, `ctl`.
3. `common/constants.go` carries the §2.1 block; `pb/schema.proto` carries
   `WorkerReg` with the key comment of §2.2 and the generated files are
   committed from `make gen`.
4. `dnv-worker --help` lists exactly the CM1 flags; `DNV_WORKER_ROLES=dn`
   is honoured (CM2).
5. Two workers started with default timers against one etcd: after 60 s
   every `(role, shard)` has exactly one owner (LG `shard owned` records);
   `SIGTERM` of one deletes its keys and the other owns everything ~60 s
   later; `SIGKILL` of one does the same ~80 s later and its keys are gone
   (VW6).
6. Every `msg` string of §12 appears with the listed attributes in a run of
   the §14 suite; the suite passes end to end on the lab server
   (`bash integtest/worker_test.sh user@192.168.10.20`).
7. Every `grpc.NewClient` in `worker/` uses the `grpc.md` §4 chain options;
   every trace id in an agent log produced by the suite has the
   `{seed8}-{16 hex}` shape (RW10).
8. No `clientv3` import outside `etcdutil/` (`log.md` §5.3).
9. §15's companion edits are present (grep: `[D17]` in `architecture.md`
   Appendix B; `model/` in `layout.md` §2/§3; `dnv-worker.md` in
   `README.md`).

---

## Appendix A — Worked membership timeline (default timers)

Interval `I = 10 s`, grace `G = 60 s`, dead threshold `2I = 20 s`. Three
workers `A`, `B`, `C` are effective members of every role; shard `s` is
owned by `B`.

| t | event | what each observer does |
|---|---|---|
| 0 | `D` starts, puts its three keys | `A`, `B`, `C` observe an appear for `D` (VW3) and start `D`'s 60 s timers (VW5). `D` scans, observes `A`, `B`, `C` and itself, starts four timers; its effective set is empty (VW7): it drives nothing |
| 0–60 | `D` heartbeats every 10 s | puts refresh `lastSeen`; no transition, timers keep running |
| ≈60 | timers fire | `A`, `B`, `C` commit `D` as member (VW6) and recompute (VW9): the shards whose largest ticket is now `D`'s (~¼) move — `B` stops `s`'s shard worker if `D` outranks it, logging `shard released`; `D` commits all four and starts shard workers for the same shards, logging `shard owned`. The switch is simultaneous to within watch latency; agents see an equal-revision re-sync from `D` |
| 300 | `B` is `SIGKILL`ed | nothing yet: `B`'s keys stay, its last put was at ≈300 |
| ≈320 | `B`'s deadline (`lastSeen + 20`) fires everywhere | `A`, `C`, `D` observe `B` dead (disappear), start `B`'s 60 s timers |
| ≈380 | timers fire | each commits `B` as nonmember, issues `Delete` of `B`'s keys (VW6; the first wins, the rest are no-ops), recomputes: `B`'s shards go to whoever holds the next-largest ticket; `B`'s revision keys were never touched, so the new owners' first rounds re-sync at the current revisions (RW4 step 5) |
| 500 | `C` is `SIGTERM`ed | `C` deletes its keys at once (CM5) and drains; `A`, `D` observe a disappear immediately (a delete event) and start 60 s timers — no 20 s wait |
| ≈560 | timers fire | `A`, `D` commit, recompute; `C`'s in-flight syncups finished long before (≤ 60 s deadline) |
| 700 | `A` is `SIGSTOP`ped | its puts stop; at ≈720 `D` observes it dead and starts a timer; at ≈780 commits it nonmember, deletes its keys, owns everything |
| 800 | `A` is `SIGCONT`ed | `A`'s heartbeat tick sees `now − lastOkPut ≥ 20` (VW8a) and fences: stops its shard workers (already stale — the agents ignored nothing, every syncup was idempotent), mints seed `A'`, registers; `D` observes `A'` appear, commits it at ≈860; `A'` commits `D` and itself at ≈860 and takes ~½ of the shards |

The dead worker's shards were undriven from 300 to ≈380 (80 s); the
gracefully stopped worker's from 500 to ≈560 (60 s). Revision keys are
durable, so nothing is lost — convergence is delayed, not skipped
(Appendix D).

---

## Appendix B — Residuals and known limits

* **A partitioned observer is tolerated, not repaired.** An observer whose
  watch is broken while its puts succeed is caught by VW8 (b) only if its
  own echoes stop too; a watch that delivers its own echoes but drops a
  peer's is not distinguishable from that peer dying. The wrong commit
  deletes the peer's key; the peer sees it (VW8 c), fences and rejoins as a
  fresh identity; in between, both drive the same shards — bounded by the
  grace window plus one round, and harmless to agents (idempotent
  syncups) and to etcd (STM-guarded reactions), but not prevented.
* **Overlap and gap at every ownership change** (§0 item 4): a few seconds
  of double driving or of no driving per shard per change. Accepted.
* **Undriven windows**: a fresh worker's first grace window; 2I + G after a
  crash; G after a graceful stop; G after a fence (Appendix A).
* **The [D8] memo** of grown clone chunks is per worker; a handoff can leave
  a shorter chunk at the agent (cost: extra copying, never correctness).
* **A group's spare list can fill with parked legs** (AR8 step 4); the
  worker never frees a slot itself.
* **`RedundNone` legs have no automatic repair**: no spare can exist, and
  the migration that could have moved a readable-but-sick side is an
  operator's tool (`CreateMigration`), not a reaction (§0 item 12).
* **Reaction inputs are one pass old**: `err_epoch`s are read fresh each
  pass, but pool usage and spare readiness come from the primary's last
  Check reply (≤ one interval old). A reaction is at most one interval
  late; never wrong, because the op re-validates.
* **Log volume**: every round logs a `grpc client send`/`recv` pair per
  object (`grpc.md` L6) and every heartbeat an `etcd put`; with 5 s rounds
  and thousands of objects this is the dominant log stream of the control
  plane, as it is for the agents. `RES_STATUS_UNKNOWN` and health
  transitions are the only records the worker adds per object.
