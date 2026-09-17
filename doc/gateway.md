# gateway.md — the dnv-gateway control-plane API server

Status: **normative**. Companion to `architecture.md` (whose §8 remains the
authority for every RPC's semantics — Errors / Defaults / Action), `grpc.md`
(interceptors and trace ids), `log.md` (logging), `layout.md` (package and file
placement) and `dnv-worker.md` (whose MD8 fixes the reuse relationship with
`model`). This document is deliberately implementation-ready: a later session
(or another engineer) implements `gateway/`, `cmd/dnv-gateway/`,
`integtest/gateway_test.sh` and `integtest/gatewayctl/` from it without
re-deriving any decision. Where this document restates a mechanism, it cites
the owning spec; where the two ever disagree, `architecture.md` §8 wins for
*what* an RPC does and this document wins for *how* the gateway does it.

Required background: `architecture.md` §5 (etcd data model), §6 (allocation),
§7 (validation), §8 (the RPC specs); `pb/schema.proto` (`service Gateway`, 59
RPCs, and the agent services); `doc/grpc.md`; `doc/log.md`.

Conventions: `{p}` is the etcd key prefix (`"dnv "`). Ids in keys are `%016x`
(`common.IdKeyFmt`). MUST/SHOULD/MAY are RFC 2119. Rule ids are append-only
once cited from code: `GW` (serving + handler pattern, §3–§4), `AG` (agent
calls, §6), `CM` (cmd, §7) and `LG` (log records, §8). §10's integration test
plan defines no rule ids of its own. "cid" abbreviates `cluster_id`; "token" always means the request-side
`DnRev`/`CnRev`/`SpRev` optimistic-concurrency value (`architecture.md` §5.5).

Terminology:

| term | meaning |
|---|---|
| handler | one gRPC method of `service Gateway`, implemented on the `gateway.Server` |
| resolution | the in-STM reads that turn `cluster_name` (and `sp_name`) into `cid` (and `SpConf`), per architecture.md §5.8 — GW11's resolution of conf DEFAULTS is a different operation, and the text says which it means |
| token check | asserting `stored.revision == request token revision`, when the request carries the token message at all (GW6) |
| deciding STM | the STM that commits a mutation; for two-phase RPCs (AG4) it is the second one |
| candidate unit | one "scan outside + STM commit" round of an allocating RPC (GW9) |
| plays the worker | the integration suite writing a worker-owned flip (`created`/`provisioned`) or running a worker-owned drain (sp, clone) through `workerctl`, so a gateway precondition can be exercised — or a latch finished — without running dnv-worker (§2.4) |

---

## 0. Decision record

Decisions fixed before writing this spec; the body cites them as "§0 #n".

1. **Scope**: this document covers `gateway/` and `cmd/dnv-gateway` only.
   `dnvctl` / `ctl/` is a separate component with its own document
   (`dnvctl.md`); the integration test drives the gateway with a dedicated
   test client, not dnvctl.
2. **Division of authority**: architecture.md §8 keeps RPC semantics; this
   document specifies the implementation mapping (files, helpers, STM shapes,
   error mapping, agent-call mechanics) and does not duplicate §8's
   Errors/Defaults/Action bodies.
3. **Statelessness**: dnv-gateway is stateless and active-active. There is no
   leader election, no registration key, no shard split and no instance-count
   limit. Any instance serves any request; multi-instance correctness rests
   entirely on `etcdutil.RunSTM` serializable-snapshot isolation plus the
   `architecture.md` §5.5 rev tokens. A gateway writes nothing to etcd that
   describes itself.
4. **Code placement**: new STM bodies live in `gateway/` per-resource files
   (layout.md's recommended split). `model` changes are limited to the exports
   and amendments of §2.2; the three mutations `model` already exports
   (`GrowSlice`, `CreateSpareLeg`, `SwitchSpareLeg`) are reused, not
   duplicated (dnv-worker.md MD8).
5. **Agent connections**: dial per call, close after the call, no connection
   cache in v1 (a cache like the worker's `worker/revision.go` one is future
   work). Every agent call is bounded by `common.DefaultGatewayAgentTimeout`
   (§2.1) and happens strictly outside any STM (architecture.md §5.8).
6. **Trace ids**: the gateway mints a trace id for a request that arrived
   without one (grpc.md T4's MAY is exercised).
7. **Token semantics — presence-based** (amended 2026-09-11; the original
   entry read the token as `req.GetSpRev().GetRevision()`, which folded an
   absent message into a `0` that could never match, so an omitted token
   always failed `ABORTED`). The handler now reads the token **message**,
   `req.GetSpRev()` (respectively `DnRev`/`CnRev`), and **presence**, not
   value, selects the mode:
   * message **absent** ⇒ the revision comparison is **skipped**. The mutator
     runs with no optimistic-concurrency gate, which is what omitting the
     token asks for. Everything else is unchanged: the rev key is still read
     (a missing one is still `ABORTED`), the other preconditions still apply,
     and a successful mutation still bumps.
   * message **present** ⇒ strict equality against the stored revision, as
     before. Because a stored revision starts at 1 and only grows, a message
     carrying `revision: 0` — or carrying only the echoed
     `addr_port`/`sp_name` — can never match and is always `ABORTED`
     ("stale revision"). That is what keeps the deliberate always-stale probe
     available, and it is why presence and not the value 0 is the
     discriminator.

   Clients still obtain tokens from the `Get*` RPCs, and an operator who
   wants the gate MUST send one; omitting it is opting out, per request. This
   **relaxes** architecture.md §5.5's MUST, which is amended to match: the
   assertion is required only when the request carries the message. The cost
   of the relaxation — a token-less mutator can lose an update, and the AG4
   two-phase safety argument no longer covers it — is accepted: omitting the
   token is opting out of the gate.
8. **Candidate retry**: `model.ErrPrecondition` with reason
   `"candidate changed"` is never surfaced; the handler re-runs the whole
   candidate unit (scan + STM) until the request context ends (GW9).
9. **Error mapping** is the single table GW7; handlers return
   `google.golang.org/grpc/status` errors directly from inside the STM closure
   (`RunSTM` returns f's error unchanged and uncommitted).
10. **No `DefaultGatewayPort` constant**: `--grpc-address` is a required flag,
    exactly like the agent's. 29527 stays the documented example
    (architecture.md §13).
11. **cmd skeleton**: `cmd/dnv-gateway/main.go` mirrors `cmd/dnv-worker` /
    `cmd/dnv-cdc` (cobra + viper, env prefix `DNV_GATEWAY`, two-signal
    handling, thin main); it is the only binary needing both the gRPC-server
    flags (from `cmd/dnv-agent`) and the etcd flags (from `cmd/dnv-worker`).
12. **Integration test**: one server (`user@ip`), everything loopback: etcd
    v3.6.14 on a fresh port block, **3** gateway instances, fakeagents 4 dn +
    3 cn. `integtest/gatewayctl` is a pure gRPC driver (dnagentctl style) plus
    a barrier-based `race` subcommand; etcd ground truth is verified with the
    existing `workerctl` read-only subcommands. No dnv-worker runs; the suite
    plays the worker with the `workerctl` worker-role subcommands of §2.4 —
    the `set-created`/`set-provisioned` flips and the `drain-sp`/`drain-clone`
    stand-ins (§10.9).
13. **Cases and coverage**: five cases S/A/B/C/D; all 59 RPCs appear in at
    least one case; §10.18 is the coverage matrix. The suite asserts
    correctness only — never latency or throughput.
14. **Unit tests**: the repo's real-etcd `TestMain` fixture pattern
    (`ETCD_BIN` env, else PATH, else skip — deliberately duplicated per
    package, model MD9 / etcdutil EU7), plus `bufconn` for gRPC-level tests.
15. **Pagination** (`architecture.md` §5.7) is implemented in `gateway/` — no model helper
    exists or is added beyond the three `*ConfPrefix` key builders of §2.2.
16. **Clone budget**: the per-CN clone budget stays untracked
    (architecture.md §8.9 "a future gateway MAY track the budget in etcd" —
    not in v1).
17. **UpdateCntlrEnabled / Update*Disabled idempotency**: when the stored flag
    already equals the requested one the handler performs no write and no
    revision bump, and replies OK (a token the request carried is still
    checked first, §0 #7).
18. **Brief decision ids.** Code comments cite decisions `D-A`…`D-J` from
    the implementation brief. `D-A` was later reversed (the `Inspect*`
    replies carry the agent's applied revision — §5.2/§5.5) and `D-B` stands
    as §5.4's `poolTotal = math.MaxUint64`, both pinned by unit tests; the
    rest are recorded here so the ids resolve from a committed document:
    * **D-C** — the `bdev_conf` CreateStoragePool STORES is the member-wise
      merge of the request over `ClusterConf.bdev_conf`: a member wins
      unless left at the proto3 zero that means "unset"; the redund KIND is
      a oneof choice (request's, else the cluster's, else `redund_none`),
      and a kind chosen by both merges member-wise inside. The merge then
      passes through `model.ResolveBdevConf`, which settles any member still
      zero on both sides against its §7 constant, so all three rungs land in
      the stored message (GW11). That stored `bdev_conf` is never
      re-resolved at read time, and an SP's geometry is therefore immutable
      under later cluster-default edits — immutable only because those stored
      members are CONCRETE, since a stored zero would still float with
      whatever constant the reading binary carried (§5.4).
    * **D-D** — CreateStoragePool plans, scans and mints in one fixed order
      (per slice the 1-extent META group then the DATA group; cntlr ids
      first, in pick order), so a retried candidate unit reproduces exactly
      the same write set (§5.4).
    * **D-E** — GrowSlice's `req.ext_cnt` never reaches the model: it is
      only §8.5's exclusivity signal (a data grow states one, a meta grow
      must not); sizes are recomputed from the stored first data group or
      the meta ladder (§5.4).
    * **D-F** — GrowSlice's DN black list is the request's own and nothing
      else; the gateway never appends to it, because one scan-and-pick round
      serves the whole new group and the §6.3 `LocList` rule already leaves
      the candidate list with one entry per DN and per location, so the
      group's legs land on distinct DNs anyway. The worker's AR6 auto-grow
      draws from the same kind of list and reaches the same distinctness by
      black-listing each pick as it goes (§5.4).
    * **D-G** — DeleteTransfer's finalize skips a vanished origin
      subsystem/`ns_idx` instead of failing: the transfer is being deleted
      either way, and the RPC must stay able to complete (§5.9).
    * **D-H** — CreateThinDevice `size == 0` is legal exactly when
      `ori_name` is set: a snapshot inherits its origin's size (§5.6).
    * **D-I** — a migration destination side's cntlid slot is the first
      `cntlid_slot_list` entry that differs from the source side's; a list
      offering no second value refuses `FAILED_PRECONDITION` (§5.10,
      architecture.md §11.8).
    * **D-J** — bitmaps are opaque to the gateway; = GW14.

---

## 1. Scope and placement

`dnv-gateway` "serves the `Gateway` gRPC service to users/CLI. Reads and
writes etcd. Calls agents only for `GetDnSize`/`GetCnSize`, the `Get*Info`
behind its `Inspect*` and behind the `force = false` hydration checks of
`DeleteClone`/`FinishMigration` (§8.9/§8.11), and the `Get*Bm` bitmap
reads" (architecture.md §1's binary table).
It is one of the three co-located control-plane processes (gateway, worker,
cdc) that any number of CP servers may run.

Files (layout.md's recommended split — implementers MAY split differently but
MUST keep the package boundaries and the dependency rule):

```
gateway/
  server.go           Server type, Run(), grpc.Server bootstrap + interceptors (§3)
  traceid.go          the §0 #6 trace-id mint: ensureTraceId* interceptors chained ahead of the grpc.md §4 pair (§3)
  common.go           resolution, token check, error mapping helpers (§4)
  cluster.go          §8.1 handlers
  disknode.go         §8.2 handlers
  controllernode.go   §8.3 handlers
  storagepool.go      §8.4 + §8.5 handlers
  cntlr.go            §8.6 handlers (incl. InspectCntlr / InspectSide)
  thindevice.go       §8.7 handlers
  subsystem.go        §8.8 handlers (subsystems + namespaces + CdcEntry writes)
  clone.go            §8.9 handlers
  transfer.go         §8.10 handlers
  migration.go        §8.11 handlers
  spareleg.go         §8.12 handlers
  bitmap.go           §8.13 handlers
  alloc.go            the §6.5 per-operation candidate compositions
  validate.go         the §7 request validation
cmd/dnv-gateway/main.go
integtest/gatewayctl/            (§10.8: main.go + cmd_node.go, cmd_sp.go, cmd_vol.go, cmd_copy.go)
integtest/gateway_test.sh        (§10)
```

Dependency rule (layout.md, normative): `gateway` imports only `common`,
`pb`, `etcdutil`, `model` plus the grpc runtime — it is a gRPC server *and*
client (it dials agents). `cmd/dnv-gateway` imports `gateway`, `common` and
`etcdutil` (it builds the client GW2 takes — CM3); cobra/viper live in
`cmd/` and `ctl/`, never in `gateway/`. `make build` picks the new binary up from
`cmd/dnv-gateway/main.go` with no Makefile edit.

Out of scope for v1: dnvctl, TLS/auth (the whole of dnv is plaintext gRPC),
clone-budget tracking (§0 #16), any agent conn cache, any gateway-side
watch/convergence logic (that is the worker's job).

---

## 2. Constants, schema and model amendments

### 2.1 Additions to `common/constants.go`

The listing is a condensed quote, not a byte-for-byte one: the three constants
sit in three different places of `common/constants.go`'s single `const` block,
in another order (only `DefaultGatewayAgentTimeout` is left under a
"dnv-gateway" heading there), and their comments there carry longer rationale
and a different wrapping. The names, the values and the rules the comments
state are the committed ones, and `common/constants.go` is authoritative for
comment text — as it is for `dnagent.md` §2.2's block.

```go
// Per-call budget for the gateway's agent RPCs (GetDnSize/GetCnSize, the
// Get*Info behind Inspect* and behind the force = false checks of
// DeleteClone/FinishMigration, the Get*Bm bitmap reads), in seconds. Applied
// with context.WithTimeout around each dial+call (AG2); chosen equal to
// DefaultEtcdOpTimeout so a hung agent and a hung etcd bound an RPC alike.
DefaultGatewayAgentTimeout = 10

// The fixed capacity of ONE clone bitmap chunk, and the quantum that
// positions it: chunk (s, b) holds the bytes [b*CloneBmChunkBytes, …+len)
// of source slice s's bitmap, len <= CloneBmChunkBytes (§5.8). 1 MiB keeps
// a grown chunk value plus the Clone and rev-bump puts of one
// AppendCloneBitmap inside etcd's default ~1.5 MiB request cap, and every
// PushCloneBitmap message inside gRPC's default 4 MiB. It is a clone
// positioning quantum only — migration appends carry no byte cap.
CloneBmChunkBytes = 1 << 20

// A DEPLOYMENT REQUIREMENT, not a client setting: every etcd serving dnv
// MUST run with --max-txn-ops=1024 or higher; etcd's default is 128. etcd
// caps on max(len(Compare), len(Success), …), and etcdutil's
// serializable-snapshot STM compares every key it read AND every key it
// wrote, so that sum is what has to fit. The transaction this number is
// SIZED by is CreateStoragePool at its widest shape, 967 compares: 7 fixed
// + one put per slice + 7 per distinct DN + 8 per CN, at MaxSliceCntPerSp x
// 2 groups per slice x MaxAllocLegPerGrp legs = 128 DNs and
// MaxCntlrCntPerSp cntlrs. init_ext_cnt moves ExtCnt VALUES, not key
// counts, so every factor of that shape is a ceiling constant and it IS
// tripwirable: gateway/txnbudget_test.go's TestCreateStoragePoolBudget
// asserts the arithmetic from them. The headroom is thin in the DN
// dimension — one more key read or written per DN costs 128 compares. The
// sp drain's D2 batch, 486 compares at the maximum shape (dnv-worker.md
// §11.6), is the SECOND bounded transaction this number has to cover and no
// longer the one that sizes it. DeleteClone's MaxSliceCntPerSp x
// MaxCloneBmCnt rectangle sweep — then 256 keys, at the 16-slice ceiling of
// the time — was this number's founding justification and is gone: the
// clone drain replaced it with 68-op batches that fit the default (§5.8,
// §10.4).
EtcdMaxTxnOps = 1024
```

No other constant is added by this document. (`MaxAllocLegPerGrp` and
`MaxDelGrpPerTxn` were added on 2026-09-15 by the sp drain, whose owner is
`dnv-worker.md` §11.6; they appear here only through the `EtcdMaxTxnOps`
arithmetic above. `DefaultSliceCntPerSp`(2) was added on 2026-09-17 with
`MaxSliceCntPerSp`'s move to 32; it is the value §5.4 substitutes for a zero
`slice_cnt`, and its normative carrier is architecture.md §8.4's `Defaults:`
line — §5.4 only restates it.) `DefaultClusterName`, `ShardBucketSize`,
`Max*CntPerCluster`, `MaxCloneBmCnt`, `MaxMigrBmCnt` and the §7 bounds all
exist already. `MaxCloneBmCnt` keeps its name and its value **16**, but it
counts the chunks ONE source slice's bitmap may be split into
(`bm_idx < MaxCloneBmCnt`, §5.8) and is NOT a bound on the source slice
count — that is `MaxSliceCntPerSp`, which `CreateClone`'s geometry check
already enforces.

### 2.2 Amendments to `model` (applied at implementation time)

Items 1–4 keep their bodies exactly as they are; only visibility and three
signatures change, each mechanically and with the worker still compiling.
Item 5 is new code: GW11's write-time resolver and the stored-conf
validators its read sites refuse with.

1. Export `spNextId` as `SpNextId(conf *pb.SpConf) uint64` — the per-SP id
   read (returns `conf.NextId` clamped to `SpFirstId`; it mutates nothing —
   every caller advances and persists `conf.NextId` itself, as the model ops
   and the gateway's `spIdMinter` do).
2. Export the SpRev bump as
   `BumpSpRev(s etcdutil.STM, op string, shard uint32, cid, spId uint64) error`
   (rev key missing ⇒ the existing precondition-style failure). Add
   `BumpDnRev(s etcdutil.STM, op string, cid uint64, dn *pb.DnConf)` /
   `BumpCnRev(…, cn *pb.CnConf)` — same behavior, taking the conf message the
   caller already holds: read the rev key — missing ⇒ error — `revision += 1`,
   put back **preserving** `addr_port`/`sp_name` (`architecture.md` §5.5:
   rewrite, never delete+recreate).
3. `GrowSlice`, `CreateSpareLeg` and `SwitchSpareLeg` gain one parameter
   `expectRev uint64` checked first inside their STM against the stored
   `SpRev.revision`: `0` skips the check (the worker's internal calls pass 0),
   any other value must match or the op fails
   `ErrPrecondition{Reason: ReasonStaleRevision}`. Export
   `const ReasonStaleRevision = "stale revision"`. The gateway passes the
   client token's revision, which is 0 exactly when the request carried no
   token message — and 0 means "skip" here just as it does at the handler,
   so the two layers agree by construction (amended 2026-09-11 with §0 #7;
   the entry previously said the gateway short-circuits nil to `ABORTED` so
   `model` never sees a gateway call with 0). A token that is merely
   *present* with revision 0 never reaches `model`: GW6 refuses it first.
4. Add to `model/keys.go`: `DnConfPrefix(cid uint64) string`,
   `CnConfPrefix(cid uint64) string`, `SpConfPrefix(cid uint64) string` — the
   `List*` range prefixes (the per-key builders exist; the prefixes do not).
5. Extend `model/capacity.go` with the two families GW11 rests on:
   `ResolveBdevConf(conf *pb.BdevConf) *pb.BdevConf`, the write-time resolver
   `CreateStoragePool` stores through — and which `ResolveClusterConf`, until
   now a helper that carried `bdev_conf` along as stored, applies to a
   ClusterConf's own `bdev_conf` too — plus the stored-conf validators
   `ValidateBdevConf(conf *pb.BdevConf) error` and
   `ValidateClusterConf(cc *pb.ClusterConf) error`, whose every message
   begins `invalid stored conf: ` and names the proto field.
   `ValidateBdevConf` checks the four defaultable members for PRESENCE only —
   the bitmap chunk count solely when the oneof did choose md-raid1, a
   `redund_none` pool having no bitmap to size; the §7 ranges belong to the
   request, and `low_water_mark_pct` above 100 is the legal "auto-grow off"
   setting, never an error. `ValidateClusterConf` adds
   `dn_bin_conf.extent_size` non-zero — non-zero only, because a DN header
   may legitimately be formatted at an unusual size — the §6.2 shift ladder,
   the two batch sizes, the four intervals and `ValidateBdevConf` of
   `bdev_conf` when the cluster carries one. The ladder predicate is one
   unexported function shared with `ResolveDnBinConf`, so resolver and
   validator cannot drift; the all-zero shift set a ClusterConf written
   without a `dn_bin_conf` would carry is therefore NOT valid stored state.

### 2.3 Schema

No `pb/schema.proto` change on the gateway's own surface beyond the four
`Inspect*Reply` fields' later rename from `revision` to `applied_revision`
(same field numbers) and, later still, `AppendCloneBitmapRequest`'s chunk
address: the single `slice_idx` became the pair `src_slice_idx` (5) +
`bm_idx` (6), pushing `bitmap` to 7 (§5.8) — deliberately wire-incompatible
with old peers, no compatibility path anywhere. (That same change's
`PushCloneBitmapRequest`, `BitmapInfo` and new `BmChunkId` edits are the
worker↔agent surface, not this one.) Still no new gateway RPC and **no new
etcd key kind** — `clone_bitmap`'s KEY gained a seventh field, but the kind
is the one that already existed: the gateway writes only the
`architecture.md` §5.3 kinds that already exist (`cluster_conf`, the three globals,
`dn_conf`/`cn_conf`, `dn_capacity`/`cn_capacity`, `dn_rev`/`cn_rev`/`sp_rev`,
`sp_conf`, `sp_id_to_name`, `cntlr`, `slice`, `thin_device`, `subsystem`,
`cdc`, `clone`, `clone_bitmap`, `transfer`, `migration`, `migration_bitmap`).

### 2.4 Amendment to `integtest/workerctl` (applied with §10)

Six subcommands so the integration suites can play the worker (§0 #12). This
suite runs four of them — the two flips and the two drains; `set-deleting` and
`set-clone-deleting`, the worker suite's stand-ins for the two gateway latches,
are listed with them because the six are one family:

* `set-created --sp <name|id> --name <td_name>` — resolves the td, calls
  `model.FlipCreated(ctx, cli, cid, shard, spId, []TdRef{{name, tdId}})`,
  emits `{"td_name":…, "td_id":…, "flipped":<bool>, "sp_rev":…}`. Note the flip bumps
  `SpRev` exactly once when it wrote (the suite's rev bookkeeping must count
  it).
* `set-provisioned --sp <name|id> --slice <id> --leg <id> --side <id>` — the
  same through `model.FlipProvisioned` with one `SideRef`; also bumps `SpRev`
  when it wrote.
* `set-clone-deleting --sp <name|id> --name <clone_name>` and
  `drain-clone --sp <name|id> --name <clone_name> [--max-steps N]` *(added
  2026-09-16 with the clone latch, §5.8)* — the clone twins of `set-deleting`
  and `drain-sp` below; this suite runs only `drain-clone` (§10.11 step 13,
  §10.14 step 5), the latch being the RPC's.
  `drain-clone` is CLD7's derivation in a loop over `model.DrainCloneBm` and
  `model.FinishCloneDelete`, and emits
  `{"sp_name":…, "clone_name":…, "steps":…, "chunk_cnt":…,
  "clone_deleted":<bool>}` — `chunk_cnt` counting the chunk KEYS the whole
  drain removed, summed over every batch (§10.11 step 13 asserts 3), not one
  batch's size.
  `set-clone-deleting` writes the flag and bumps, and deliberately does NOT
  resume the destination namespaces the way the RPC does: that write is the
  gateway's, and a driver that re-implemented it would drift from it.
* `set-deleting --sp <name|id>` *(added 2026-09-15 with the latch, §5.4)* —
  `SpConf.deleting = true` plus one `BumpSpRev`, `DeleteStoragePool`'s latch
  without its five-empty-lists precondition (that gate is the gateway's), and
  a no-op with no second bump when already latched (SPD3); emits
  `{"sp_name":…, "sp_id":…, "latched":<bool>, "sp_rev":…}`. The worker
  suite's, not this one's: it is how dnv-worker.md §14's case G puts an SP in
  front of the real coordinator without a gateway.
* `drain-sp --sp <name|id> [--max-steps N]` *(added 2026-09-15 with the
  latch, §5.4)* — runs the sp coordinator's drain to completion: SPD8's
  derivation in a loop over `model.DrainSpCntlrs` / `DrainSpSlice` /
  `FinishSpDelete`, with no pass and no timer. It emits
  `{"sp_name":…, "sp_id":…, "steps":…, "cntlr_cnt":…, "grp_cnt":…,
  "slice_cnt":…, "sp_deleted":<bool>}` (`sp_id` in the `%016x` key spelling,
  which is what §10.11 step 17 asserts it against).
  `--max-steps` is a real argument, not only a runaway guard: the worker
  suite asks for exactly one step to BUILD a partially drained SP rather than
  race one, so running out is reported in `sp_deleted` and is never fatal.
  Every step bumps `SpRev` except the last, which deletes the key.

---

## 3. Serving and lifecycle

* **GW1** `gateway.Server` holds exactly one piece of state — the
  `*etcdutil.Client` — beside the mandatory `pb.UnimplementedGatewayServer`
  embed.
  Handlers keep no in-process state, take no locks and impose no concurrency
  limit — all coordination is etcd's. Two requests racing inside one instance
  and across two instances are the same case by construction.
* **GW2** `gateway.Run(ctx context.Context, cli *etcdutil.Client, cfg
  gateway.Config) error` with `Config{GrpcNetwork, GrpcAddress string,
  Endpoints []string}`. `Run` emits `"gateway starting"` (with the config) as
  its first record and `"gateway stopping"` on the way out, closes `cli` as
  the last step of its drain (so `main` does not — CM3), and serves exactly
  like `agent/agent.go Serve`: `net.Listen(cfg.GrpcNetwork, cfg.GrpcAddress)`;
  `grpc.NewServer(serverOptions()...)` — where `serverOptions` chains the
  gateway-local `ensureTraceIdUnary()` / `ensureTraceIdStream()` FIRST and
  `common.GrpcUnaryServerInterceptor()` / `common.GrpcStreamServerInterceptor()`
  behind them;
  `pb.RegisterGatewayServer`; a goroutine doing `<-ctx.Done();
  grpcServer.GracefulStop()`; an Info `"gateway serving"` record with
  `network`/`address`; then `grpcServer.Serve(lis)`. The server chain is the
  gateway-local `ensureTraceId` interceptors followed by the §4 chains: when
  the incoming metadata carries no `trace_id`, the first interceptor injects
  `common.NewTraceId()` into the incoming metadata, so §0 #6's mint happens
  upstream of the shared interceptors and `common/interceptor.go` stays the
  grpc.md §3 reference verbatim.
* **GW3** Shutdown is `GracefulStop`: in-flight handlers finish (each bounded
  by its client deadline and the 10 s per-STM budget), new requests are
  refused. There is nothing else to drain — no registry key to delete, no
  background loop to stop.
* Startup never fails because etcd is unreachable (`etcdutil.New` dials
  lazily); only a configuration error fails `Run` before serving.

---

## 4. The handler pattern

Every handler is the same seven-step shape; per-RPC deviations are in §5.

* **GW4 — request validation first.** `validate.go` implements the §7 table
  as pure functions (no I/O): string sizes/patterns, NQN rules, bounded
  numerics — a zero passes as "give me the default", and for every member
  that ends up in a stored conf substituting that default is GW11's job on
  the write path, never this one's — and `bdev_feature_list` empty. The list
  `count` is the one bounded numeric GW11 does not cover, because it reaches
  no stored message at all: `pageLimit` refuses a value above `MaxListCnt`
  1024 (`INVALID_ARGUMENT`, never a silent cap) and turns a zero into
  `DefaultListCnt` 64 per request, and `validatePageArgs` runs it —
  with the token decode — before the three cluster-scoped List handlers'
  ClusterConf read; `ListClusters` reaches the same two checks through
  `pageNames` before its range (GW10).
  Violations ⇒ `INVALID_ARGUMENT` before any etcd read.
  State-dependent validation (slot in use, `ns_idx` taken, …) happens
  inside the STM.
* **GW5 — resolution in-STM.** Except `CreateCluster` and `ListClusters`,
  the STM's first read is `model.ClusterConfKey(cluster_name)`
  (`cluster_name` defaulted to `common.DefaultClusterName`) — absent ⇒
  `NOT_FOUND` — and `cid = model.ClusterId(name, conf.CreationEpoch)`
  prefixes every further key. SP-scoped RPCs then read
  `model.SpConfKey(cid, sp_name)` — absent ⇒ `NOT_FOUND`; mutators (except
  `DeleteStoragePool`) fail `FAILED_PRECONDITION` when `SpConf.deleting`.
  `DeleteStoragePool` is what SETS that flag (§5.4), so the gate is live from
  the moment it commits until the drain removes the key.
  The **paged** `List*` RPCs (clusters, disk nodes, controller nodes,
  storage pools) use plain reads, not an STM (`architecture.md` §5.7): the
  three cluster-scoped ones do one `Get` of ClusterConf for the cid and then
  `Range`; `ListClusters` ranges the `cluster_conf` prefix directly, there
  being no cid to derive. `ListThinDevices`, `ListSubsystems` and the
  single-object `Get*` RPCs are one-STM consistency reads (§5.6–§5.8).
* **GW6 — token check, presence-based.** Immediately after resolution and
  before any other state check, a mutator reads its rev key
  (`SpRevKey(shard, cid, spId)` etc.). It then asserts
  `stored.revision == req.Get<X>Rev().GetRevision()` **only when
  `req.Get<X>Rev() != nil`**; a mismatch ⇒ `ABORTED` with message
  `stale revision` (§0 #7). A request that carries no token message skips the
  comparison and proceeds. The rev key is read either way — it is an
  `architecture.md` §5.3 invariant key whose absence is `architecture.md`
  §5.9's `ABORTED`, the bump helpers rely on
  it having been read, and keeping it in the read set leaves a skipped check
  no weaker than a checked one against a concurrent delete. A present message
  carrying `revision: 0` is a real token, not an omission, and is always
  `ABORTED`. Checking the token first means a client that sent a stale one
  always sees `ABORTED`, never a misleading precondition error computed
  against state it has not read; a client that sent none has waived that
  ordering and meets its other preconditions directly. The echoed `addr_port`/`sp_name` inside the token message is
  ignored (`architecture.md` §5.5). Every mutation that changes agent-visible desired state
  bumps the matching revision exactly once in the same STM
  (`model.BumpSpRev`/`BumpDnRev`/`BumpCnRev`); `Update*Disabled` and
  capacity/err-epoch maintenance never bump (`architecture.md` §5.5).
* **GW7 — error mapping** (the only table; handlers return
  `status.Error(code, msg)` from inside the STM closure):

  | condition | code |
  |---|---|
  | §7 violation, malformed `page_token`, bad enum/oneof | `INVALID_ARGUMENT` |
  | cluster / SP / named or id-addressed object absent | `NOT_FOUND` |
  | create finds the name key (or, `CreateCluster`, a global) present | `ALREADY_EXISTS` |
  | a documented public precondition fails (incl. `model.ErrPrecondition` with any reason except the two below) (the meta ladder cap included) | `FAILED_PRECONDITION` |
  | `sum(shard_bucket) ≥ Max*CntPerCluster`; the per-SP and per-group count ceilings — `len(cntlr_id_list) ≥ MaxCntlrCntPerSp`, `td_name_list` at `MaxTdCntPerSp`, `nqn_list` at `MaxSsCntPerSp`, a subsystem's `ns_list` at `MaxNsCntPerSs`, `clone_name_list` at `MaxCloneCntPerSp`, `xfer_name_list` at `MaxXferCntPerSp`, `migr_name_list` at `MaxMigrCntPerSp`, a group's `spare_leg_list` at `MaxSpareLegPerGrp` (architecture.md §8.6–§8.12); too few candidates (§6.5); `AppendMigrationBitmap`'s `bm_cnt ≥ MaxMigrBmCnt` cap; `AppendCloneBitmap`'s `len(stored) + len(bitmap) > CloneBmChunkBytes` — one chunk's ceiling reached by previous appends, the same shape (AppendCloneBitmap's other four INVALID_ARGUMENT refusals are an invalid request ⇒ `INVALID_ARGUMENT`: the empty `bitmap` of the §7-violation row above, both index bounds — `src_slice_idx ≥ src_slice_cnt` and `bm_idx ≥ MaxCloneBmCnt`, checked in-STM — and the stateless `len(bitmap) > CloneBmChunkBytes` page cap, judged on the request alone because a page longer than a whole chunk fits nowhere whatever is stored; all four per §5.8); a cntlr's CN below a grow's ext count (§5.4's pre-check); the ledgers' own `charge` shortfall (`dnLedger.charge`/`cnLedger.charge`, `gateway/alloc.go`) — defensive, since `verifyPick` answers the same shortfall as candidate-changed first | `RESOURCE_EXHAUSTED` |
  | token mismatch; `model.ErrPrecondition{Reason: ReasonStaleRevision}` | `ABORTED` ("stale revision") |
  | everything `architecture.md` §5.9: STM-client/conflict-budget/etcd/proto errors; a stored conf that is not concrete (GW11; the message is `model`'s, beginning `invalid stored conf: `); agent gRPC failure where the RPC says so | `ABORTED` |

  `model.ErrNotFound` (from `LoadSp`) maps to `NOT_FOUND`;
  `ErrPrecondition{Reason: "candidate changed"}` maps to nothing — see GW9.

  The dividing line: `RESOURCE_EXHAUSTED` is capacity or quota that could be
  freed or extended (extents, candidates, count ceilings);
  `FAILED_PRECONDITION` is the object's own state forbidding the operation.
  A `GrowSlice` capacity shortfall sits on both sides, on either half of the
  allocation. A CN-budget shortfall is `RESOURCE_EXHAUSTED` when §5.4's
  pre-check sees it, but `FAILED_PRECONDITION` when it only appears in the
  deciding STM, where `model.chargeSpCns` can refuse solely as an
  `ErrPrecondition` (§5.4). A leg DN short of free extents is likewise
  `RESOURCE_EXHAUSTED` when the §6.5 scan skips it and the pick comes up
  short (the "too few candidates" row above), but `FAILED_PRECONDITION`
  (`model.checkDnPick`, reason `"dn free_ext_cnt too low"`) when only the
  STM sees it — that check precedes the capacity-key test, so it is not
  swallowed by `ReasonCandidateChanged`. The same ordering means a pick
  whose DN was **deleted or disabled** between scan and STM surfaces on the
  model paths (`GrowSlice`, `CreateSpareLeg`) as `FAILED_PRECONDITION`
  (`"dn not found"` / `"dn not allocatable"`) rather than a GW9 re-scan;
  only the gateway-ledger paths treat every vanished pick as
  candidate-changed and re-scan.
* **GW8 — one STM per RPC** (`architecture.md` §5.8). Everything computable beforehand (name
  formatting, group plans, candidate lists, the stamped `creation_epoch`) is
  prepared outside; all reads and writes commit in one `RunSTM`. The closure
  MUST be a pure function of what it reads through the STM (it is re-run on
  conflict); minted values that escape (ids, generated uuids) are written to
  closure-captured variables that the closure itself (re)assigns.
* **GW9 — candidate unit retry.** Allocating RPCs (`CreateStoragePool`,
  `GrowSlice`, `CreateCntlr`, `CreateMigration`, `CreateSpareLeg`) loop:
  scan candidates outside (`model.FindDnCandidatesAntiAffine` /
  `FindCnCandidates` + `PickRandom`), run the STM which re-reads each pick's
  exact capacity key (`Cand.BinIdx/FreeExt/AddrPort`) and fails
  `ErrPrecondition{"candidate changed"}` when one is gone; on that error —
  and only that error — re-scan and retry until `ctx` ends (then `ABORTED`).
* **GW10 — pagination** (`architecture.md` §5.7). `page_token` =
  `base64.StdEncoding.EncodeToString(lastReturnedKey)`; decode failure ⇒
  `INVALID_ARGUMENT`; empty ⇒ start of prefix; range starts at the key
  **after** the decoded one; a reply whose page is not full returns an empty
  token. Prefixes: `ClusterConfPrefix()`, `DnConfPrefix(cid)`,
  `CnConfPrefix(cid)`, `SpConfPrefix(cid)`; the returned names are the key
  suffixes after the prefix.
* **GW11 — defaults resolved at WRITE time** (§7). The rungs are unchanged —
  a member comes from the request, else from the owning `ClusterConf`, else
  from a `constants.go` constant — but ALL of them are applied by the RPC
  that writes the conf, so every stored conf anything is formatted or
  addressed with is concrete in every DEFAULTABLE member and nothing
  downstream substitutes (the `redund_conf` oneof is a choice and not a
  default — unset still means `redund_none`, §8.4 — and the two exceptions
  to the rule, `event_threshold` and the `dm_clone_conf` hydration pair,
  close it below).
  `CreateCluster` stores `model.ResolveClusterConf(…)`, which settles
  `bdev_conf`, `dn_bin_conf`, `alloc_conf` and `health_check_conf` (§5.1);
  `CreateStoragePool` stores `model.ResolveBdevConf(…)` of D-C's merge
  (§5.4). A handler that then COMPUTES with a stored member validates it
  first and REFUSES rather than guessing around a zero:
  `model.ValidateClusterConf` in `CreateDiskNode` and `CreateControllerNode`
  (before dividing a reported size by `extent_size`), in `DeleteDiskNode` and
  `UpdateDiskNodeDisabled` (the last check before their first write, because
  the one capacity key each moves is named by shifting the STORED ladder), in
  `CreateStoragePool` and the `GrowSlice` handler (before any group
  geometry), in `newDnLedger` — the DN ledger `CreateStoragePool`,
  `DeleteSpareLeg`, `CreateMigration`, `FinishMigration` and
  `CancelMigration` build before staging their first write, for the same
  capacity-key reason — and in the §6.5 scans `pickDns`/`pickCn` (where a zero batch
  size would silently make the scan width zero and turn every allocation
  into `RESOURCE_EXHAUSTED`); `model.ValidateBdevConf` in `GrowSlice` and in
  `CreateThinDevice` (before sizing against the stripe). Such a zero is a
  lost invariant, not a bad request, so every one of those gates is GW7's
  `architecture.md` §5.9 `ABORTED` and never `INVALID_ARGUMENT`. `model.GrowSlice` re-runs
  both validators inside its own STM and can report that refusal only as an
  `ErrPrecondition`, which `mapModelErr` renders `FAILED_PRECONDITION`; the
  handler's pre-check above is what makes that a race rather than the
  ordinary path, the same asymmetry GW7 records for `chargeSpCns`. Two costs
  of resolving at use time are what moved it: a consumer that forgets to
  resolve then computes with zeros in silence —
  no reader can tell a member the user omitted from one the user chose, and
  an SP's `bdev_conf` reaches the cn agent as stored — and a stored zero
  pins geometry to whatever `common.Default*` the RUNNING binary carries, so
  editing a constant would re-geometry live storage pools and strand every
  capacity key already written under the old bin ladder. Two consequences an
  implementer must keep: resolution runs AFTER the GW4 validation of the
  request — on the raw request a zero still means "give me the default", so
  resolving first would make every §7 bound check a tautology — and AFTER
  the member-wise merge in `CreateStoragePool`, never before it, because a
  resolved request has no member left at the zero that inherits from the
  cluster. Two conf messages are still resolved when they are READ, and both
  are stored exactly as sent, because both are policy knobs rather than
  geometry — nothing is formatted or addressed with either, and an operator
  reads back what they asked for (architecture.md §7 names the same two).
  `event_threshold` is resolved member-wise by
  `model.ResolveEventThreshold`. The `dm_clone_conf` hydration pair is the
  other: `CreateClone` and `CreateMigration` store the request's message as
  it arrived (§8.9, §8.11), the sp-worker fills a migration's zeros in with
  their §7 constants as it builds the side request (`dnv-worker.md` RW15),
  and a zero in a clone's is simply never messaged to the dm-clone target by
  the cn agent (`ensureHydrationKnobs`, which sends CN18 step 3's
  `hydration_threshold`/`hydration_batch_size` messages, sends each only for
  a non-zero member), leaving the target's own default in place
  (`architecture.md` §7).
* **GW12 — id minting.** Cluster-scoped ids per `architecture.md` §5.4 inside the STM: read the
  global, `id = next_id; next_id += 1`; `shard_code` = index of the smallest
  `shard_bucket` (first on ties), `bucket[shard_code] += 1`; the
  `Max*CntPerCluster` gate is `sum(bucket)` before increment. Deletion
  decrements the bucket, never reuses ids. Per-SP ids via `model.SpNextId`;
  thin-device `dev_id` from `SpConf.next_dev_id++` (`ori_id = 0` = none).
* **GW14 — bitmaps are opaque.** Bitmap bytes cross the gateway VERBATIM in
  both directions: `AppendCloneBitmap` and `AppendMigrationBitmap` store what
  the request carries (§5.8, §5.10 — the push of those stored chunks to the
  agents is the worker's, dnv-worker.md §10, not a gateway RPC), and the
  §5.12 reads reply the agent's bytes. The wire convention — 1 =
  unmapped/never-written, LSB-first — is produced and consumed by the
  agents (architecture.md §11.4's single inversion at the agent boundary);
  the gateway never inspects, converts or trims a bit, so it can never
  disagree with the agents about the convention. (There is no GW13: the
  implementation brief's numbering is kept because this id is already cited
  from code.)

---

## 5. Handlers by resource group

Format: each RPC gets its mechanism — pre-STM work, STM outline in key/helper
terms, bump, reply. Semantics, exact error sentences and field-level rules
stay in the cited architecture.md section; nothing below overrides them.

### 5.1 Clusters (§8.1)

* **CreateCluster** — validate confs (§7) on the RAW request, where a zero
  still asks for the default. One §7 row is a refusal rather than a bound:
  the `dn_bin_conf` shifts are all-or-nothing — all four zero asks for the
  0/4/8/12 default and is accepted, any other set that is not
  `0 ≤ bin0 < bin1 < bin2 < bin3 ≤ 63` is `INVALID_ARGUMENT`, because the
  stored ladder is what every capacity key of this cluster is written under
  for the cluster's whole life and an operator handed a silently different
  ladder has no RPC to correct it (ClusterConf is write-once). Stamp
  `creationEpoch = uint64(time.Now().UnixNano())` **once, outside** the STM
  (retries of this attempt reuse it; a client retry stamps anew — §8.1), and
  build the message to store outside it too, for the same reason: it is
  `model.ResolveClusterConf` of the request's conf members plus that epoch,
  so `bdev_conf`, `dn_bin_conf`, `alloc_conf` and `health_check_conf` land
  concrete and only `qos_ratio` and the epoch pass through as given (GW11;
  validation first, resolution second — never the other way round). STM:
  `ClusterConfKey(name)` present ⇒ `ALREADY_EXISTS`; compute
  `cid = ClusterId(name, epoch)`; any of `DnGlobalKey(cid)` /
  `CnGlobalKey(cid)` / `SpGlobalKey(cid)` present ⇒ `ALREADY_EXISTS`
  (hash-collision guard, re-evaluated inside every attempt); put that
  ClusterConf and the three globals (`next_id: 1`, `shard_bucket`:
  `ShardBucketSize` zeros). Reply `cluster_id`.
* **DeleteCluster** — STM: resolve; emptiness check is
  `sum(shard_bucket) == 0` on **all three** globals (`architecture.md` §5.4
  makes the sum the live object count, so no range read is needed) ⇒ else
  `FAILED_PRECONDITION`; delete ClusterConf + the three globals. Reply the
  cid.
* **GetCluster** — one STM: ClusterConf → cid → the three globals (a missing
  global ⇒ `ABORTED`). Reply name, cid, conf, globals.
* **ListClusters** — no cluster resolution (the one exception); paged plain
  range over `ClusterConfPrefix()` (GW10); names are the key suffixes.

### 5.2 Disk nodes (§8.2)

* **CreateDiskNode** — validate; **pre-STM agent call** (AG1):
  `DiskNodeAgent.GetDnSize` at `addr_port` (`dn_id` 0, logging only) — gRPC
  failure ⇒ `ABORTED`. STM: resolve; `DnConfKey(cid, addr_port)` present ⇒
  `ALREADY_EXISTS`; `model.ValidateClusterConf` on the ClusterConf this STM
  resolved, then `total_ext_cnt = bytes / extent_size` (§6.1, the stored size
  used as stored — a zero is refused `ABORTED`, never divided by, GW11);
  gate + mint from `DnGlobal` (GW12, `RESOURCE_EXHAUSTED` at
  `MaxDnCntPerCluster`); put `DnConf{dn_id, shard_code, disabled,
  nvme_tr_conf, location, total_ext_cnt, free_ext_cnt: total}`;
  `model.MaintainDnCapacity(s, cid, addr, cc, nil, newDn)`; put
  `DnRev{addr_port, revision: 1}` at `DnRevKey(shard, cid, dn_id)`; put the
  global. Reply `dn_id`.
* **DeleteDiskNode** — STM: resolve; `DnConf` by addr (`NOT_FOUND`); token vs
  `DnRev` (GW6); `side_ptr_list` non-empty ⇒ `FAILED_PRECONDITION`;
  `model.ValidateClusterConf` on the resolved ClusterConf (a zero ⇒ `ABORTED`,
  GW11 — the last check before the first write, since the capacity key to
  delete is named by the stored ladder); delete
  DnRev, DnConf, capacity key (`MaintainDnCapacity(…, old, nil)`);
  `DnGlobal.shard_bucket[shard] -= 1`. Reply `dn_id`.
* **GetDiskNode** — one STM: DnConf, then DnRev by the read id+shard
  (missing ⇒ `ABORTED`). Reply addr, conf, rev (the token source).
* **ListDiskNodes** — paged plain range over `DnConfPrefix(cid)` (GW10).
* **UpdateDiskNodeDisabled** — STM: resolve; DnConf; token; if
  `disabled` already equals the request: OK, nothing written (§0 #17); else
  `model.ValidateClusterConf` (a zero ⇒ `ABORTED`, GW11 — below the no-op,
  above the put, because the flip moves a capacity key the stored ladder
  names), set it and `MaintainDnCapacity(old, new)`. **No revision bump, no agent
  call** (§8.2). Reply `dn_id`.
* **InspectDiskNode** — STM: read DnConf (the ids that address the agent
  request and the log line; the resolving Snapshot supplies the cid, so no
  plain pre-read — and no DnRev read: a client that wants the desired-state
  token calls GetDiskNode); **after** the STM call
  `GetDnInfo(cluster_id, dn_id)`; agent failure ⇒ `ABORTED`. Reply
  `{applied_revision, dn_info}`, both from the agent's reply
  (architecture.md §8.2 — deliberately the agent's applied revision, never
  the stored rev key).

### 5.3 Controller nodes (§8.3)

The exact §5.2 mirror over `CnConf`/`CnCapacity`/`CnRev`/`CnGlobal` with:
`CreateControllerNode` calls `GetCnSize` and maps the reply per §6.1 (0 ⇒
`DefaultCnCap` 4 TiB, clamp at 64 TiB, below-min treated as 0); delete's
occupancy precondition is `cntlr_ptr_list`; `InspectControllerNode` calls
`GetCnInfo`. Six RPCs: Create/Delete/Get/List/UpdateDisabled/Inspect.

### 5.4 Storage pools (§8.4) and GrowSlice (§8.5)

* **CreateStoragePool** — validate (`cntlid_slot_list`: values < 8, no dupes;
  `cntlr_cnt` in `[MinCntlrCntPerSp, MaxCntlrCntPerSp]` and ≤ len(slot list);
  `slice_cnt ≤ MaxSliceCntPerSp`(32); `init_ext_cnt ≥ 1`; confs per §7). A
  zero `cntlr_cnt` and a zero `slice_cnt` are requests for a default, not
  refusals: each is replaced — before the bound above it is judged — by
  `DefaultCntlrCntPerSp`(2) and `DefaultSliceCntPerSp`(2), and it is the
  substituted count the plan and the STM's slice loop build the SP from
  (architecture.md §8.4's `Defaults:` line). A zero `init_ext_cnt` is still
  `INVALID_ARGUMENT`, and so is `CreateClone`'s zero `src_slice_cnt` (§5.8):
  that one describes a source which already exists, so no default can stand
  in for it. Pre-STM plan (`planSpGroups`), in D-D's order: per slice the
  meta group (`ext_cnt = 1`) first, then the data group
  (`ext_cnt = init_ext_cnt`) — ext counts only;
  `model.GroupBlocks` turns each into `meta_blocks`/`data_blocks` in
  the STM, where the conf it needs has been read and checked. Candidate unit
  (GW9): scan DNs per group with the §6.5 growing black list (`RequiredCnt`
  legs per group — RedundNone 1, RedundMdRaid1 2 — from
  `dn_batch_size × RequiredCnt` candidates, random pick, picked DNs
  black-listed so every leg of the SP lands on a distinct
  DN) and CNs (`CandExtCnt = Σ ext_cnt` over all groups, `cntlr_cnt` rounds,
  random pick, black-listed); too few at any point ⇒ `RESOURCE_EXHAUSTED`.
  STM, in this order: resolve; build the `bdev_conf` to store as
  `model.ResolveBdevConf(mergeSpBdevConf(request, cluster))` — merge first so
  an omitted member still inherits from the cluster, resolve second so what
  is stored is concrete (D-C, GW11) — and fail the unit right there, before
  a single other key is read, when this transaction's `cluster_id` or that
  conf's leg count no longer matches the one the scan drew its picks for;
  `SpConfKey` present ⇒ `ALREADY_EXISTS`; mint `sp_id`/shard from `SpGlobal`
  (GW12), then every `cntlr_id` in pick order (D-D);
  `model.ValidateClusterConf` before the cluster's `extent_size` is used (a
  zero ⇒ `ABORTED`, GW11); then per slice its `slice_id` and, in plan order,
  its meta group before its data group — for each, `model.GroupBlocks`
  against that `extent_size` and the resolved `bdev_conf`, then the `grp_id`
  and per leg a `leg_id` and its `side_id`, re-reading that leg's pick
  capacity key as its DN is charged and failing the unit if it moved; then
  every CN pick likewise. Everything above is staged in memory: a refusal —
  a moved capacity key above all — returns before the first `Put`, not
  merely before the commit. The write set is then one `Slice` per slice
  (every `Side` `provisioned: false`), one `Cntlr` per pick
  (first = `primary`, `cntlid_slot` = next unused slot in list order),
  `SpConf` (that `bdev_conf`; `event_threshold` exactly as the request sent
  it — one of GW11's two read-time exceptions; lists, `next_id`,
  `next_dev_id: 1`, `deleting: false`), `SpName` and
  `SpRev{sp_name, revision: 1}`; then per DN:
  `free_ext_cnt -= group ext`, append `side_ptr_list`,
  `MaintainDnCapacity`, `BumpDnRev` once; per CN likewise with
  `cntlr_ptr_list` and `BumpCnRev`; last the updated `SpGlobal`. Reply
  `sp_id`.
* **DeleteStoragePool** — *amended 2026-09-15: it LATCHES, and the sp-worker
  drains (dnv-worker.md §11.6).* STM: `openSpFlags(..., rejectDeleting =
  false)` — resolve, read `SpRev`, run GW6's token check; if `deleting` is
  ALREADY true return OK here, with **no writes and no bump** (a repeat delete
  must not invalidate every client's token to force a pointless re-resolve;
  the token check has already run, so a stale token still ABORTs first); else
  all five name lists (`td/nqn/clone/xfer/migr`) empty ⇒ else
  `FAILED_PRECONDITION`; put `SpConf` with `deleting = true`; `BumpSpRev`.
  Five ops. Reply `sp_id` — the repeat-delete no-op replies with it too, since
  the RPC resolved the SP before short-circuiting and `DeleteStoragePoolReply`
  carries nothing else.
  The emptiness check and the latch share the STM with the reads, so they are
  atomic, and once latched `resolveSp`'s existing `rejectDeleting` gate
  refuses every other mutator — no new child can appear after the check, ever.
  Nothing else is written: the SP, its cntlrs, its slices and every extent
  they charge survive the reply, and partial teardown is now a real, visible
  state (architecture.md §8.4). What the old one-shot really guaranteed was
  not atomicity but AGREEMENT — DN and CN budgets never disagreeing with the
  keys that describe them — and every drain batch keeps it by releasing budget
  in the same transaction that shrinks the describing key. Consequences:
  `CreateStoragePool` keeps failing `ALREADY_EXISTS` on the surviving
  `sp_conf` key until the drain's last transaction, so name reuse resumes only
  then, and an observer polls `GetStoragePool` until `NOT_FOUND`.
* **GetStoragePool** — one STM: SpConf, SpRev, every listed Cntlr, every
  listed Slice; a missing listed key ⇒ `ABORTED`. Reply all of it (SpRev is
  the token source).
* **ListStoragePools** — paged plain range over `SpConfPrefix(cid)`.
* **UpdateStoragePoolCntlidSlotList** — STM: resolve; token; §8.4 validation
  (dupes / ≥ 8 / removing an in-use slot ⇒ `INVALID_ARGUMENT`); write;
  `BumpSpRev`. Reply `sp_id`.
* **UpdateStoragePoolLevel** — STM: resolve; token; set `sp_level`;
  `BumpSpRev`. Reply `sp_id`.
* **FindStoragePoolNames** — `etcdutil` snapshot (one store revision, no
  STM): ClusterConf → cid, then per requested `sp_id` read
  `SpNameKey(cid, id)`; unknown ids are omitted, never an error. Reply the
  map.
* **GrowSlice** — validate the §8.5 exclusivity (`is_meta==false ⇒
  ext_cnt>0`, `is_meta==true ⇒ ext_cnt==0` ⇒ else `INVALID_ARGUMENT`).
  A `Snapshot` pre-read for planning, whose ClusterConf and SP `bdev_conf`
  are both validated before anything is sized from them
  (`model.ValidateClusterConf`, `model.ValidateBdevConf`; a failure ⇒
  `ABORTED`, GW11) — which is also what keeps `model.MetaLadderExtCnt`'s
  `false` meaning the 16 GiB cap and nothing else, since an unvalidated zero
  `extent_size` would report the same `false` and reach the operator as a
  metadata ceiling. The plan itself is the slice's current meta total for
  `model.MetaLadderExtCnt` — cap reached ⇒ `FAILED_PRECONDITION`, GW7's
  object-state class — and the request's own black list, which is the entire
  seed of the §6.5 scan (D-F): one scan-and-pick round serves the group and
  nothing is ever appended to it, the legs still landing on distinct DNs
  because the §6.3 `LocList` rule already left one candidate per DN and per
  location; then a second `Snapshot`, the CN-budget pre-check
  (`growSliceCnBudget`), over the SP's cntlrs and their CnConfs at one store
  revision — every cntlr stacks the new group, so every one of their CNs
  needs the new group's ext count free (the size D-E recomputes, never the
  request's `ext_cnt`), and a shortfall is §8.5's `RESOURCE_EXHAUSTED`,
  named with the CN that is short; a cntlr or CnConf key gone under the
  snapshot is skipped, not raised, because this only gives the common case
  its documented code and the STM re-reads all of it; candidate unit; then
  call the amended `model.GrowSlice(…, expectRev = token)` (§2.2 #3) which
  re-validates in-STM and bumps SpRev, the leg DNs' revs and — via
  `chargeSpCns`, which charges the CN of every cntlr — those CNs' `CnRev`s
  itself (architecture.md §8.5). A CN that loses its budget in the window
  after the pre-check is therefore still refused, but by `chargeSpCns` as an
  `ErrPrecondition`, so the caller sees `FAILED_PRECONDITION` instead: the
  pre-check is deliberately the generous one, since only the STM decides.
  The gateway passes `poolTotal = math.MaxUint64`: the AR6 pending rule
  gates only the worker's auto-grow, never a user-driven grow
  (architecture.md §8.5; `TestGrowSliceConsecutiveDataGrows` pins it). Map
  `ReasonStaleRevision` ⇒ `ABORTED`, other `ErrPrecondition` ⇒
  `FAILED_PRECONDITION`. Reply `slice_id, grp_id`.

### 5.5 Cntlrs and inspects (§8.6)

* **CreateCntlr** — validate slot; candidate unit for one CN
  (`CandExtCnt = Σ` all groups' ext). STM: resolve; token; slot in
  `cntlid_slot_list` and unused ⇒ else `INVALID_ARGUMENT`; mint `cntlr_id`;
  put `Cntlr{addr_port, nvme_tr_conf (the CN's), cntlid_slot,
  primary: false, disabled: false}`; append `cntlr_id_list`; CN bookkeeping +
  `BumpCnRev`; append the CN's `nvme_tr_conf` to **every** `CdcEntry` of the
  SP (iterate `nqn_list` → Subsystem → `CdcEntryKey(cid, shard, spId,
  ssId)`); `BumpSpRev`. Reply `cntlr_id`.
* **DeleteCntlr** — STM: resolve; token; `primary == false` and
  `disabled == true` ⇒ else `FAILED_PRECONDITION`; reverse everything
  CreateCntlr did (id list, key, CN bookkeeping + `BumpCnRev`, tr conf out of
  every CdcEntry); `BumpSpRev`. Reply `cntlr_id`.
* **UpdateCntlrEnabled** — STM: resolve; token; no-op when already at the
  requested state (§0 #17); else `disabled = !enabled`, add (enable) or
  remove (disable) the tr conf in every CdcEntry, `BumpSpRev`. Reply
  `cntlr_id, enabled`.
* **InspectCntlr** — STM: resolve; Cntlr by id (`NOT_FOUND`), keep its
  `addr_port` and the CnConf's `cn_id` (read `CnConfKey(cid, addr)` in the
  same STM; deliberately no SpRev read); after the STM
  `GetCntlrInfo(cluster_id, cn_id, sp_id, cntlr_id)` at that addr; failure ⇒
  `ABORTED`. Reply `{applied_revision, cntlr_info}`, both from the agent's
  reply (architecture.md §8.6).
* **InspectSide** — STM: resolve; find the side by scanning the SP's slices
  (bounded, §8.6) for `side_id` (`NOT_FOUND`), keep its DN `addr_port` +
  `dn_id` (via `DnConfKey`) + the side pointer (deliberately no SpRev
  read); after the STM `GetSideInfo(cluster_id, dn_id, side pointer)`; failure
  ⇒ `ABORTED`. Reply `{applied_revision, side_info}`, both from the agent's
  reply.

### 5.6 Thin devices (§8.7)

* **CreateThinDevice** — validate `size` is a positive multiple of
  `slice_cnt × stripe_size` (the stripe as the SP stored it,
  `bdev_conf.dm_raid0_conf.stripe_size`; state-dependent, so checked in-STM,
  behind a `model.ValidateBdevConf` of that conf — GW11 — so a zero stripe is
  `ABORTED` instead of reaching the "has no slice" refusal, which is about
  `slice_id_list`). STM: resolve; token; `td_name` in `td_name_list` or key
  present ⇒ `ALREADY_EXISTS`; if `ori_name` set: read origin (`NOT_FOUND` if
  absent), `created == false` ⇒ `FAILED_PRECONDITION` **writing nothing**
  (no id consumed, no bump — §8.7); mint `td_id = SpNextId`, `dev_id =
  next_dev_id++`, `ori_id` = origin's `dev_id` or 0; put
  `ThinDevice{…, created: false}`; append `td_name_list`; `BumpSpRev`. Reply
  `td_id, dev_id`.
* **DeleteThinDevice** — STM: resolve; token; td (`NOT_FOUND`);
  `FAILED_PRECONDITION` when referenced by any `Namespace.td_id` (walk
  `nqn_list` → each Subsystem's `ns_list`), by any `Clone.dst_td_id` (walk
  `clone_name_list`), or when some td has `ori_id == this.dev_id &&
  created == false` (walk `td_name_list`); delete key, remove from list,
  `BumpSpRev`. Reply `td_id`.
* **ListThinDevices** — one STM: SpConf + every listed td (missing ⇒
  `ABORTED`) into `name_to_td`. This is the documented client wait primitive
  for `created`.

### 5.7 Subsystems and namespaces (§8.8)

All pure etcd; every mutator: resolve, token, mutate, `BumpSpRev`.

* **CreateSubsystem** — nqn valid (§7; the discovery NQN can never validate)
  and absent ⇒ else `ALREADY_EXISTS`; mint `ss_id`;
  `serial = fmt.Sprintf("%016x", ss_id)`, `model = "dnv"`; put
  `Subsystem{empty ns_list, allowed_hosts}` + append `nqn_list` + put
  `CdcEntry{nqn, tr confs of every **enabled** cntlr's CN, allowed_hosts}`
  at `CdcEntryKey(cid, shard, spId, ssId)`. Reply `ss_id`.
* **DeleteSubsystem** — `ns_list` empty ⇒ else `FAILED_PRECONDITION`; delete
  Subsystem + CdcEntry + list entry. Reply `ss_id`.
* **ListSubsystems** — one STM: `nqn_list` → each Subsystem into
  `nqn_to_subsystem` (missing ⇒ `ABORTED`).
* **UpdateSubsystemHosts** — rewrite `allowed_hosts` in **both** the
  Subsystem and its CdcEntry. Reply `ss_id`.
* **CreateNamespace** — subsystem by nqn (`NOT_FOUND`); `ns_idx != 0` and
  unused in this subsystem ⇒ else `INVALID_ARGUMENT`; td by `td_name`
  (`NOT_FOUND`); mint `ns_id`; defaults: empty `dev_uuid` ⇒ RFC 4122 v4
  (canonical dashed string), empty `dev_nguid` ⇒ 16 random bytes as 32 hex
  chars; append to `ns_list`. Reply `ns_id`.
* **DeleteNamespace** — locate by nqn + `ns_idx` (`NOT_FOUND`); remove from
  `ns_list`. Reply `ns_id`.
* **UpdateNamespaceDev** — locate ns; resolve new `td_name` (`NOT_FOUND`);
  repoint the ns's td reference. Reply `ns_id`.
* **UpdateNamespaceSuspended** — locate ns; set `suspended`. Reply `ns_id`.

### 5.8 Clones (§8.9)

* **CreateClone** — validate §8.9 bounds (`src_slice_cnt` in
  1..`MaxSliceCntPerSp`(32); `src_stripe_size = i×4KiB, i ≤ 256`;
  `src_block_size = j×64KiB, j ≤ 16384` and a multiple of the stripe);
  STM: resolve; token; name free ⇒ else
  `ALREADY_EXISTS`; dst td by name (`NOT_FOUND`); mint `clone_id`; put
  `Clone{…, dst_td_id}`; append `clone_name_list`; `BumpSpRev`.
  Reply `clone_id`. (The "destination td must be empty" precondition is
  documented-unverifiable [D3]; the per-CN clone budget is untracked, §0
  #16.)
* **DeleteClone** — *amended 2026-09-16: it LATCHES, and the sp-worker drains
  (dnv-worker.md §11.7).* Two-phase (AG4). Phase 1 STM (read-only): resolve;
  clone (`NOT_FOUND`); **if `deleting` is already true, return OK here** — with
  no writes, no bump and NO agent call — after running GW6's token check
  explicitly, because phase 1 is the decision on that path and `openSpRead`
  skips the check (a stale token must still ABORT ahead of the `deleting`
  row). The short-circuit sits before the agent call and not in phase 2 for a
  reason that is not an optimization: after the latch the CN has retired the
  stack, so `GetCntlrInfo` no longer reports the dm-clone and a hydration check
  would wedge every repeat delete in `FAILED_PRECONDITION` for ever. Otherwise,
  when `force == false`, also resolve the **primary** cntlr's `addr_port` +
  `cn_id` — an SP with no primary cntlr ⇒ `FAILED_PRECONDITION`, since there is
  nobody to prove hydration with and a promotion makes the retry succeed
  (`force == true` skips the lookup entirely: no agent call follows, which is
  what lets force delete a clone whose cntlr is unreachable). Between phases,
  `force == false` calls `GetCntlrInfo`; incomplete hydration **or an
  unreachable agent** ⇒ `FAILED_PRECONDITION` (§8.9). Phase 2 STM (deciding):
  full re-resolution + token check (GW6 — any interleaved mutation bumped
  `SpRev`, so a token the request carried subsumes staleness of phase 1; a
  token-less request gets the re-resolution only, AG4); `loadClone` again,
  and if `deleting` became true since phase 1 return OK with no writes (the
  same rule, raced variant); else write nothing but the resume, the latch
  and the bump — `suspended = false` on every namespace whose
  `td_id == dst_td_id`, as one `Subsystem` put per subsystem that actually
  changed and none for the rest (`resumeCloneDstNs`; architecture.md §8.9 —
  the dst namespaces resume with the data now local,
  the §5.9 DeleteTransfer twin of this write), the `Clone` put with
  `deleting = true` and every other field unchanged, and `BumpSpRev`. Reply
  `clone_id`.
  It does NOT delete the Clone key, does not touch a chunk key and does not
  shrink `clone_name_list`: the name must survive until the drain's last
  transaction, because `model.LoadSp` fetches clones by iterating it. The
  resume rides the LATCH so that it and the fan-out exclusion arrive in one
  `SpRev` bump; deferred, the destination namespace would go dark for the whole
  drain. Consequences for callers, all following from the Clone key and its
  list entry surviving: `clone delete` returns while the clone still exists, so
  an observer polls `GetClone` until `NOT_FOUND`; same-name `CreateClone` keeps
  failing until then — `ALREADY_EXISTS`, or `RESOURCE_EXHAUSTED` on an SP whose
  `clone_name_list` the surviving entry holds at `MaxCloneCntPerSp`, that
  ceiling being checked ahead of the name, which also fails an UNRELATED
  `CreateClone` for the whole drain; a `CreateClone` onto the same destination td,
  and a `DeleteThinDevice` of that td, both keep failing
  `FAILED_PRECONDITION` because their scans walk `clone_name_list`; and
  `DeleteStoragePool` keeps refusing while any clone drains.
* **loadLiveClone** — the CLD1 gate the two clone mutators that ADDRESS an
  existing clone open with: `UpdateCloneTrConf` and `AppendCloneBitmap` answer
  `FAILED_PRECONDITION` when `deleting` is true. (`CreateClone` addresses none
  and needs no gate: the surviving key and list entry keep it refused.) The
  append half is load-bearing, not cosmetic: a racing append could otherwise write a chunk key behind the
  drain, and the drain's final emptiness guard rests on "after the latch, no
  chunk key can ever appear again". `GetClone` and DeleteClone's own phase 1
  keep using plain `loadClone`.
* **GetClone** — one STM read. Reply the Clone.
* **UpdateCloneTrConf** — STM: resolve; token; clone; replace
  `src_tr_conf_list`; `BumpSpRev`. Reply `clone_id`.
* **AppendCloneBitmap** — one chunk of the SOURCE bitmap, addressed by the
  PAIR (`src_slice_idx`, `bm_idx`). Chunk `(s, b)` holds the bytes
  `[b·C, b·C+len)` of source slice `s`'s bitmap, `len ≤ C =
  CloneBmChunkBytes` (§2.1): `src_slice_idx` picks the source slice, `bm_idx`
  fixes the chunk's byte offset WITHIN that one slice at the fixed quantum
  `C` and says nothing about any other slice. No chunk's meaning depends on
  any other chunk's existence or length, so a caller may append to any
  `(s, b)` at any time, in any order, and may leave chunks unsent entirely;
  an absent chunk reads as all-zero (= all-written) and a short chunk's
  missing tail reads as written, both the safe direction
  (architecture.md §9.6).
  Validation: `bitmap` non-empty (`INVALID_ARGUMENT`, `validateBitmap`,
  shared with `AppendMigrationBitmap`), then `len(bitmap) ≤ C` ⇒ else
  `INVALID_ARGUMENT` — handler-local and NOT in `validateBitmap`, because it
  is judged on this request alone (a page longer than a whole chunk fits
  nowhere, whatever is already stored) and migration chunks carry no byte cap
  of their own. STM: resolve; token; clone; `src_slice_idx < src_slice_cnt` ⇒
  else `INVALID_ARGUMENT` (`src_slice_cnt` is the WHOLE slice bound —
  `CreateClone` already holds it to `MaxSliceCntPerSp`, so a second constant
  check here would judge nothing the geometry has not judged already);
  `bm_idx < MaxCloneBmCnt` ⇒ else `INVALID_ARGUMENT`; read the chunk at
  `CloneBitmapKey(…, src_slice_idx, bm_idx)` — an absent key decodes to an
  empty bitmap, which is what makes "create if absent" fall out and is the
  same read the ceiling check needs; `len(stored) + len(bitmap) ≤ C` ⇒ else
  `RESOURCE_EXHAUSTED` (GW7's ceiling-reached row: `C` bounds the STORED
  chunk, not one page); **append** the bytes to that chunk — §8.9's action,
  because the caller pages one source slice's bitmap through
  `GetThinDeviceBitmap` and the pages of ONE chunk concatenate into exactly
  the bytes `[b·C, b·C+len)` that chunk holds (§9.6), so a replace would keep
  only the last page and place its bits at the chunk's own offset, which
  `PushCloneBitmap` would then hand the primary as "never written" and the
  agent would `blkdiscard` regions the source really wrote. The `Clone`
  record is **not rewritten**: it carries no chunk count, and how many chunks
  a clone holds is how many `CloneBitmap` keys it has — which is what the
  drain and `PushCloneBitmap` read. It opens with `loadLiveClone`, so an
  append to a latched clone is `FAILED_PRECONDITION` (CLD1) and the clone key
  is in the STM's read set either way; `BumpSpRev`. Reply `clone_id`.

### 5.9 Transfers (§8.10)

All pure etcd, standard mutator shape. **CreateTransfer** resolves the origin
ns per §8.10, mints `xfer_id`, appends `xfer_name_list`. **DeleteTransfer**
with `force == false` *finalizes*: additionally sets `suspended = true` on
the origin ns in the same STM; `force == true` *aborts* (origin untouched);
both delete the Transfer + list entry and `BumpSpRev`. **GetTransfer** one
STM read. **UpdateTransferHosts** rewrites `allowed_hosts` (the transfer's
own — no CdcEntry involvement). Replies `xfer_id`.

### 5.10 Migrations (§8.11)

* **CreateMigration** — locate `src_side_id` by slice scan (`NOT_FOUND`); its
  leg already has 2 sides ⇒ `FAILED_PRECONDITION`; candidate unit for one DN
  (`CandExtCnt` = the group's `ext_cnt`; black list seeded with the DNs of
  every leg/side of the group, and §6.5's two tiers applied — tier 1 also
  excludes those DNs' `location`s, read once before the unit through
  `grpDnLocations` because a location never changes (§8.2); tier 2 rescans
  without the location exclusion when tier 1 yields fewer than the **one DN**
  this RPC places — never when it merely falls short of the oversampled
  `DnCandCnt` — and its candidates are merged behind tier 1's). STM: resolve;
  token; re-verify topology + capacity key; mint `migr_id` and `dst_side_id`;
  append the new `Side` to the leg per §8.11 (`provisioned: false`,
  `cntlid_slot` ≠ the src side's); dst-DN bookkeeping + `BumpDnRev`; put
  `Migration`; `BumpSpRev`. Reply `migr_id`.
* **FinishMigration** — two-phase like DeleteClone: `force == false` resolves
  the **destination** side's DN in phase 1, calls `GetSideInfo` between
  phases, judges hydration from `migr_dst_info.dm_clone_info`; incomplete or
  unreachable ⇒ `FAILED_PRECONDITION`. Deciding STM: re-resolve + token;
  apply the §8.11 finish (dst side becomes the leg's side, src side removed,
  src-DN bookkeeping released + `BumpDnRev`); delete the Migration + its
  `MigrBitmap` chunks (`0..bm_cnt-1`) + list entry; `BumpSpRev`. Reply
  `migr_id`.
* **CancelMigration** — STM: resolve; token; reverse of Create (drop the dst
  side, release the dst DN + `BumpDnRev`, delete Migration + chunks + list
  entry); `BumpSpRev`. Reply `migr_id`.
* **GetMigration** — one STM read.
* **AppendMigrationBitmap** — validate non-empty; STM: resolve; token; migr;
  `bm_cnt ≥ MaxMigrBmCnt` ⇒ `RESOURCE_EXHAUSTED`; put a **new** chunk at
  `bm_idx = bm_cnt` (chunks are immutable, append-only); `bm_cnt += 1`;
  `BumpSpRev`. Reply `migr_id`.

### 5.11 Spare legs (§8.12)

The requests carry `grp_id` but no slice id, so each handler must locate the
slice containing the group. CreateSpareLeg and SwitchSpareLeg do it by
scanning the SP's slices in a plain snapshot (the model op re-verifies in its
own STM); DeleteSpareLeg has no snapshot — its locate runs directly inside
its one deciding STM below.

* **CreateSpareLeg** — group is RedundNone ⇒ `INVALID_ARGUMENT`; candidate
  unit for one DN (black list = the group's leg/side DNs, under the same
  §6.5 two-tier rule as CreateMigration: tier 1 excludes their `location`s
  too, and tier 2 drops that exclusion — when tier 1 offers nothing for the
  one DN this RPC places — rather than leave the group unrepaired);
  call the amended `model.CreateSpareLeg(…, expectRev = token)`. Reply
  `leg_id`. (The spare's side is written `provisioned: false`.)
* **DeleteSpareLeg** — STM: resolve; token; group + spare leg by id
  (`NOT_FOUND`); remove it from `spare_leg_list`, release its DN
  (+`BumpDnRev`); `BumpSpRev`. Reply `leg_id`.
* **SwitchSpareLeg** — call the amended
  `model.SwitchSpareLeg(…, expectRev = token)`; its own preconditions apply
  (spare's side must be `provisioned` — §9.4 — else `FAILED_PRECONDITION`
  via `ErrPrecondition`). Reply `curr_active_leg_id, curr_spare_leg_id`.

### 5.12 Bitmap reads (§8.13)

Both are read-only two-phase: one STM resolves, then one agent call. Both
address the SP through its **primary** cntlr, so an SP with none ⇒
`FAILED_PRECONDITION`: there is no controller to ask, and a promotion makes
the retry succeed, which is what a precondition means.

* **GetThinDeviceBitmap** — STM: resolve; td by name → `td_id`; the
  **primary** cntlr → its `addr_port` and (via `CnConfKey`) `cn_id`;
  `slice_idx` within the SP's `slice_cnt` ⇒ else `INVALID_ARGUMENT`. Then
  `GetThinDeviceBm(cluster_id, cn_id, sp_id, cntlr_id, td_id, slice_idx,
  start_block, block_cnt)` (`block_cnt = 0` ⇒ to the end); failure ⇒
  `ABORTED`. Reply the bitmap **verbatim** — wire convention 1 = unmapped,
  LSB-first, is produced by the agent; the gateway never touches bits (GW14).
* **GetLegBitmap** — same shape; the leg is located by slice scan
  (`NOT_FOUND`), the call is `GetLegBm(…, leg_id, start_block, block_cnt)`.

---

## 6. Agent calls

* **AG1 — placement.** Agent calls happen strictly outside STMs (`architecture.md` §5.8):
  *before* the STM for `CreateDiskNode`/`CreateControllerNode` (`Get*Size`),
  *after* the resolving STM for `Inspect*` and `Get*Bitmap`, *between* the
  two STMs for `DeleteClone`/`FinishMigration` with `force == false` — except
  that since 2026-09-16 a `DeleteClone` whose clone is ALREADY `deleting`
  answers in phase 1 and makes no agent call at all (§5.8, CLD3). When a
  call needs `cluster_id`, a plain pre-read of ClusterConf supplies it; the
  in-STM read stays authoritative.
* **AG2 — connection.** Per call:
  `ctx, cancel := context.WithTimeout(reqCtx, common.DefaultGatewayAgentTimeout * time.Second)`,
  `grpc.NewClient(addrPort, grpc.WithTransportCredentials(insecure.NewCredentials()),
  grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
  grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()))`,
  `defer conn.Close()`. The interceptors are mandatory on every dnv
  connection (grpc.md §4) and forward the request's trace id (T3). No cache
  in v1 (§0 #5).
* **AG3 — failure mapping.** A transport or non-OK status maps to the code
  the RPC's spec names: `ABORTED` everywhere except the `force == false`
  checks of `DeleteClone`/`FinishMigration`, where an unreachable agent is
  `FAILED_PRECONDITION` (the caller cannot prove hydration is done). The
  agent-side `AgentReply.code` convention does not apply to `Get*Size` /
  `Get*Info` / `Get*Bm` beyond what their replies define.
* **AG4 — two-phase rule.** Any handler with a mid-flight agent call re-runs
  **full resolution and the token check** in its deciding STM. No facts from
  phase 1 are trusted in phase 2 except as hints; for a request that carries
  a token, the token check makes any interleaved mutation visible as
  `ABORTED` ("stale revision"), which is what keeps such a two-phase RPC
  exactly as safe as a one-STM RPC. A **token-less** request keeps the full
  re-resolution but not that visibility — GW6 is presence-based (§0 #7), so
  an interleaved mutation it did not observe stays invisible to it. That is
  the AG4 half of the token-less cost (§0 #7).

The complete call matrix:

| Gateway RPC | agent RPC | when |
|---|---|---|
| CreateDiskNode | `DiskNodeAgent.GetDnSize` | pre-STM |
| CreateControllerNode | `ControllerNodeAgent.GetCnSize` | pre-STM |
| InspectDiskNode | `GetDnInfo` | post-STM |
| InspectControllerNode | `GetCnInfo` | post-STM |
| InspectCntlr | `GetCntlrInfo` | post-STM |
| InspectSide | `GetSideInfo` | post-STM |
| DeleteClone (force=false) | `GetCntlrInfo` | between STMs |
| FinishMigration (force=false) | `GetSideInfo` | between STMs |
| GetThinDeviceBitmap | `GetThinDeviceBm` | post-STM |
| GetLegBitmap | `GetLegBm` | post-STM |

No other `service Gateway` RPC leaves etcd — the matrix above is complete.

---

## 7. cmd/dnv-gateway

* **CM1** `cmd/dnv-gateway/main.go` mirrors `cmd/dnv-worker/main.go`
  structurally: package doc comment naming this spec; `const envPrefix =
  "DNV_GATEWAY"`; `main` → `newRootCmd().Execute()`; one root command, no
  subcommands, `SilenceUsage/SilenceErrors: true`, `Args: cobra.NoArgs`;
  `addFlags` / `bindViper` (`BindPFlags`, `SetEnvPrefix`, `-`→`_` replacer,
  `AutomaticEnv`, optional `--config` file); an I/O-free
  `optionsFromViper`; comma-splitting of list flags via the local
  `splitList` idiom.
* **CM2** Flags: `--grpc-network` (default `tcp`) and `--grpc-address`
  (required) as in `cmd/dnv-agent`; `--etcd-endpoints` (required),
  `--etcd-dial-timeout` (default `common.DefaultEtcdDialTimeout`) and
  `--config` as in `cmd/dnv-worker`. Deliberately no `--etcd-op-timeout`
  (EU5) and no default gRPC port (§0 #10).
* **CM3** `run`: bindViper → options → startup trace id
  (`common.WithTraceId(context.Background(), common.NewTraceId())`) →
  cancelable ctx → the two-signal `watchSignals` pattern (capacity-2 channel;
  second signal exits 1 without a clean drain) → `etcdutil.New` (lazy dial —
  startup never fails on unreachable etcd) →
  `return gateway.Run(ctx, cli, gateway.Config{…})`. `Run` closes the client
  (GW2), so `main` does not. Logging setup: none — `common`'s `init`
  installs the JSON logger; the gateway keeps level Info by doing nothing
  (log.md R6).

---

## 8. Log records

* **LG1** The gateway adds no bespoke logging for gRPC or etcd traffic:
  log.md R8's four Info classes are fully covered by the interceptor chains
  (every server/client request/reply, trace id included) and by `etcdutil`
  (every get/put/delete/range, once per STM attempt — duplicates on retry
  are expected and acceptable).
* **LG2** Records owned by this component, all via the ctx forms:
  `"gateway starting"` (`grpc_network`, `grpc_address`, `etcd_endpoints`) and
  `"gateway stopping"` from `gateway.Run`, with `"etcd client close failed"`
  (`error`) between them when GW2's deferred `cli.Close` fails; `"gateway
  serving"` (`network`, `address`) just before `Serve`; `"signal received"` /
  `"second signal, exiting without a clean drain"` from `main`'s
  `watchSignals`. Info except two **Warn**s: the failed close, and the second
  signal — giving up the drain abandons in-flight RPCs, so it is not a
  routine event.
* **LG3** Protobuf in any gateway-authored record goes through
  `common.PbToLogValue` (R10); `bytes` fields (the bitmaps) therefore log as
  `"<N bytes>"` only.

---

## 9. Unit tests

`gateway/*_test.go`, standard `go test`, no network beyond loopback.

1. **etcd fixture**: copy the `model/etcdenv_test.go` shape verbatim
   (deliberately duplicated per package — MD9/EU7): `TestMain` finds etcd via
   `ETCD_BIN` (misconfigured ⇒ exit 1) else PATH else the etcd-backed tests
   skip; one server for the whole package on an ephemeral port.
2. **validate.go**: table-driven, no I/O — every §7 row (sizes, patterns,
   NQN incl. the discovery-NQN impossibility, numeric bounds (a zero passes,
   asking for the default), the list `count` bound — zero accepted as the
   default, above `MaxListCnt` refused — `bdev_feature_list`, level enum),
   plus both arms of the `dn_bin_conf` shift rule: all four zero accepted (it
   asks for 0/4/8/12), any other non-ladder set `INVALID_ARGUMENT` (§5.1).
3. **Handler tests** against the real etcd through a `Server` constructed
   directly: per resource group, the happy path asserting **exact** etcd
   state via `etcdutil` reads (keys, ids, buckets, capacity keys, rev values)
   and the reply; plus, at minimum: token mismatch ⇒ `ABORTED`, a *present*
   zero token ⇒ `ABORTED` (the presence discriminator), an *absent* token ⇒
   the check is skipped and the mutator runs (§0 #7);
   name collision ⇒ `ALREADY_EXISTS` with nothing written; each
   `FAILED_PRECONDITION` of §5 with nothing written; `CreateCluster`
   collision-guard branch; `DeleteCluster` bucket-sum gate; pagination
   round-trip incl. bad token; `Update*` idempotent no-write (§0 #17);
   `FindStoragePoolNames` unknown-id omission; GW11 from both sides — the
   stored `ClusterConf`/`SpConf.bdev_conf` concrete in every defaultable
   member, with each rung (request, cluster, constant) a distinct value so a
   member taken from the wrong one reads differently, and a fixture that
   plants a sparse stored conf — a `ClusterConf` written back with
   `dn_bin_conf.extent_size` zeroed, an `SpConf` written back with
   `bdev_conf.dm_raid0_conf.stripe_size` zeroed — every RPC that computes
   from one then refusing `ABORTED` with `model`'s message verbatim
   (`invalid stored conf: ` + the field) and nothing written:
   `CreateDiskNode`, `CreateControllerNode`, `DeleteDiskNode` and
   `UpdateDiskNodeDisabled` on the cluster's, every GW9
   allocating RPC through the §6.5 scans on it, `DeleteSpareLeg`,
   `FinishMigration` and `CancelMigration` through `newDnLedger`'s gate on it
   — twelve cases for the cluster's conf, `GrowSlice` reaching it through a
   gate of its own (both confs, after its snapshot and before the meta
   ladder) as well as through the scan — and `GrowSlice` and
   `CreateThinDevice` on the SP's; per-SP id and `dev_id` sequences;
   `DeleteStoragePool`'s latch (SpConf + SpRev and nothing else, and a repeat
   delete pinned in BOTH directions: the first must bump, the second must
   not) plus the full-teardown accounting once the drain has run, driven here
   through `model`'s three drain ops, and the sp drain's budget tripwire over
   the named constants, and `CreateStoragePool`'s own beside it (§2.1: the
   create is the transaction `EtcdMaxTxnOps` is SIZED by, so its arithmetic
   is pinned twice — at the ceilings as they stand, and at the
   then-maximum 16-slice shape whose 503 compares the factors must still
   reproduce) plus, in a file of its own, the PROOF that tripwire can only
   approximate: one widest-shape create — `MaxSliceCntPerSp` slices,
   md-raid1, `MaxCntlrCntPerSp` cntlrs — committed against the real etcd this
   package starts with `--max-txn-ops=common.EtcdMaxTxnOps`, and the same
   test is where §6.5's distinct-NODE property — DNs and CNs alike — is
   asserted at the ceiling instead of at the two slices of the write-set
   case, since the two counts are what the create's compare total was computed
   from; §6.5's two-tier placement — a spare leg and a migration destination
   each land on the DN in the other failure domain, and still land (never
   `RESOURCE_EXHAUSTED`) once that DN is gone and the group's own domain is all
   that is left; a release path whose `dn_conf`/`cn_conf` invariant key is
   missing ⇒ `ABORTED`, not `NOT_FOUND` (GW7), with the whole message pinned and
   nothing torn down (`DeleteSpareLeg` for the DN half, `DeleteCntlr` for the
   CN half; `DeleteStoragePool` left that pair on 2026-09-15, releasing
   nothing itself any more, and the drain's deliberately OPPOSITE answer to
   the same lost key — skip, because a latched SP must still be deletable —
   is pinned in `model`). Clone bitmaps get three of their own, all on the
   PAIR addressing of §5.8: the appends `(slice 5, bm 0)`, `(slice 0, bm 3)`
   and `(slice 2, bm 1)` land in three chunk keys holding exactly the bytes
   each sent, **and the three pairs no append wrote — `(5, 1)`, `(0, 0)`,
   `(2, 0)` — are asserted ABSENT**, which is what kills a key formed from
   `bm_idx` alone (it makes `(0, 0)` the same key as `(5, 0)`) and one formed
   from `src_slice_idx` alone (it makes `(5, 1)` the same as `(5, 0)`). A byte
   compare discriminates neither, because the test reads back through the same
   key builder that wrote. A second append to `(slice 5, bm 0)` GROWS that
   chunk in place rather than replacing it; each of
   AppendCloneBitmap's five BOUNDS by code, including the boundary triple
   (an append landing exactly at `len == C` succeeds, one byte more is
   `RESOURCE_EXHAUSTED`, a single `C+1`-byte page is `INVALID_ARGUMENT` even
   on an empty chunk); and the LATCH and its drain removing the chunks of two
   different slices, with the resume asserted before a single batch has run.
   The latch's own no-op is pinned twice, because it is decided in two places:
   the repeat delete in phase 1 (no bump, the same `clone_id` back, and zero
   `GetCntlrInfo` calls), and the RACED variant in phase 2 — a second
   `DeleteClone` latching while the first is parked inside its agent call,
   which is the only way that branch is reachable and which sends no `sp_rev`,
   GW6 being presence-based. Its consequences are pinned together, since they
   are one mechanism: while a clone drains, the name is `ALREADY_EXISTS`, the
   destination thin device is `FAILED_PRECONDITION` naming that clone, and
   `DeleteStoragePool` refuses with `still holds 1 clones` — asserted with the
   td name lifted out of `td_name_list` for that one call, or the five-list
   check's first row would answer instead and the assertion would prove
   nothing. All three release at the final STM.
   The §5.8 budget is covered twice, and both halves moved with the sweep
   (2026-09-16): a pure-arithmetic tripwire — `MaxDelBmPerTxn + 4 ≤
   EtcdMaxTxnOps`, and the same expression against etcd's own default 128,
   which is the property that decoupled the deployment flag from the clone
   shape — and a PROOF against the real etcd: a clone whose whole
   `MaxSliceCntPerSp × MaxCloneBmCnt` rectangle of chunk keys exists is
   created, filled, latched and drained, and the 512 keys are asserted to
   leave in exactly `⌈512 / MaxDelBmPerTxn⌉` = 8 batches, none of which
   rewrote the record (the test computes the rectangle from the two
   constants; 512 and 8 are what they come to at the 2026-09-17 ceilings).
   Only the second one can catch a batch that grew a write.
4. **Agent-path tests**: an in-process fake implementing the generated
   `DiskNodeAgent`/`ControllerNodeAgent` servers on `127.0.0.1:0` — size
   consumed by CreateDiskNode/CreateControllerNode; `Inspect*`
   `applied_revision` + info pass-through, both from the agent's reply
   (never the stored rev keys); timeout path (a hanging fake ⇒ `ABORTED` within the
   budget); `DeleteClone`/`FinishMigration` force=false refusal on
   unreachable agent (`FAILED_PRECONDITION`); trace id visible at the fake
   (T3).
5. **Serving test** over `bufconn`: interceptors wired on the server, status
   codes cross the wire intact, `GracefulStop` on ctx cancel.
6. **Race test**: `-race`, two goroutines through one `Server` doing
   same-name creates against the fixture etcd — exactly one `OK` +
   one `ALREADY_EXISTS`/`ABORTED`, state exact.

---

## 10. Integration test plan

### 10.1 Goal and scope

Prove the real `dnv-gateway` binary — several instances of it — against a
real etcd and fake agents, on one lab server: (a) every one of the 59 RPCs
maintains **exactly** the §5 etcd schema (ground truth read back by
`workerctl`, independent of the code under test); (b) parallel gRPCs across
instances are correct — disjoint work all succeeds, contended work has
exactly one winner and no partial writes; (c) refusals of every class write
nothing; (d) instances are stateless — one can be killed mid-load and
restarted with no cleanup and no corruption. Correctness only: the suite
asserts nothing about latency or throughput (§0 #13). Out of scope: §10.19.

### 10.2 Deliverables and usage contract

`integtest/gateway_test.sh` (bash) + `integtest/gatewayctl/` (Go) + the four
`workerctl` worker-role subcommands of §2.4 this suite runs (`set-created`,
`set-provisioned`, `drain-sp`, `drain-clone`).

```
bash integtest/gateway_test.sh [--only <case>] [--cleanup-only] user@ip
```

Exactly one positional `user@ip` (passwordless ssh; `IP=${TARGET##*@}`); **no
sudo anywhere**. `--only` takes one of `smoke parallel contention faults
restart` (validated against the case list); `--cleanup-only` runs cleanup and
exits. Fail-fast: first failed assertion prints
`FAILED at stage '<case>: <desc>' (trace_id …)`, runs diagnostics (§10.17)
and leaves debris in place; success prints `PASS`. Cleanup runs ahead of the
server preflight (a crashed previous run must not fail it); only the driver's
own preflight — tools and builds — precedes it (§10.7). The flag parser, `stage`,
`die`, `assert_*`, `wait_until`, `sshw`, `remote_start`/pid files and
`SSH_OPTS` follow `worker_test.sh` verbatim.

### 10.3 Topology

Everything on the target server, everything bound to 127.0.0.1:

```
driver (this repo)                        server (user@ip)
  build + scp ─────────────────────────▶  /var/tmp/dnv-gateway-integtest ($WORK)
  ssh per stage                            ├── bin/  etcd etcdctl dnv-gateway
                                           │         gatewayctl workerctl fakeagent
   gatewayctl ──gRPC──▶ gw0 29810 ─┐       ├── etcd/     data + etcd.log   :15379/:15380
        (run on server) gw1 29811 ─┼─▶ etcd├── gw0..gw2/ gateway.log + pid
                        gw2 29812 ─┘       ├── dn0..dn3/ fakeagent dn  :29820-29823
   workerctl ──────────────▶ etcd          └── cn0..cn2/ fakeagent cn  :29830-29832
   gw* ──agent gRPC──▶ dn0..dn3, cn0..cn2   (each dir: behavior.json state.json agent.log)
```

Launch lines (via `remote_start <dir> <log> <cmd…>`, `nohup … >> log 2>&1`,
pid recorded; signals only ever by recorded pid):

```
$WORK/bin/etcd --name dnv-gw-it --data-dir $WORK/etcd \
  --listen-client-urls http://127.0.0.1:15379 --advertise-client-urls http://127.0.0.1:15379 \
  --listen-peer-urls http://127.0.0.1:15380 --initial-advertise-peer-urls http://127.0.0.1:15380 \
  --initial-cluster dnv-gw-it=http://127.0.0.1:15380 \
  --max-txn-ops=$ETCD_MAX_TXN_OPS         # common.EtcdMaxTxnOps, read at preflight (§10.4)
$WORK/bin/fakeagent dn --grpc-address 127.0.0.1:2982<i> --dir $WORK/dn<i> --size 68719476736
$WORK/bin/fakeagent cn --grpc-address 127.0.0.1:2983<j> --dir $WORK/cn<j> --size 0
$WORK/bin/dnv-gateway --grpc-network tcp --grpc-address 127.0.0.1:2981<k> \
  --etcd-endpoints 127.0.0.1:15379
```

Port block (fresh, disjoint from every other suite — worker 12379/29600s/
29700s, cdc 13379/18009-12/14420-23, agent suites 29528/29529, production
29527/2379):

| what | ports |
|---|---|
| etcd client / peer | 15379 / 15380 |
| gateways gw0 gw1 gw2 | 29810 29811 29812 |
| fakeagent dn0..dn3 | 29820 29821 29822 29823 |
| fakeagent cn0..cn2 | 29830 29831 29832 |

### 10.4 Assumptions and preflight checks

Driver: `go ssh scp curl tar sha256sum date awk sed` on PATH; `jq` or the
gojq fallback (`resolve_jq`); `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make
build` plus `go build` of `gatewayctl`, `workerctl`, `fakeagent` into
`integtest/bin`; etcd v3.6.14 fetched into `integtest/bin/cache` with the
sha256 pin (identical block to `worker_test.sh` — same cache). Server:
passwordless ssh (`sshw "true"`); `bash nohup pkill ss tar df sed awk`
present; ≥ 1 GiB free under `/var/tmp`; none of the §10.3 ports listening.
That etcd MUST be started with **`--max-txn-ops`** at `common.EtcdMaxTxnOps`
(§2.1, 1024 today): every etcd serving dnv must, for `CreateStoragePool`'s
967-compare maximum shape — the transaction the number is sized by — and for
the sp drain's 486-compare D2 batch (dnv-worker.md §11.6), the second bounded
transaction that constant has to cover. The suite is shell and cannot import
it, and it does not type the number either: `preflight_driver` runs the
`workerctl` it has just built — `constants` opens no etcd client and takes no
`--cluster`, so it runs on the DRIVER, before setup ships anything to the
server — and fills `ETCD_MAX_TXN_OPS` from the `EtcdMaxTxnOps` field of the
JSON it prints, well before setup starts etcd with it (§10.3). `worker_test.sh` and
`cdc_test.sh` take the same value from the same subcommand. NO case here
comes near the cap — step 13's `delete-clone` latches and its `wctl
drain-clone` removes three chunk keys, step 17's `wctl drain-sp` pops sp0's
four groups — so the flag is what makes the suite run against an etcd
configured the way production must be, not something a case needs.
etcd readiness is `wait_until WAIT_SHORT` on
`workerctl --endpoints 127.0.0.1:15379 ping`; each gateway's readiness on
`gatewayctl --gateway 127.0.0.1:2981<k> ping`.

### 10.5 Identity plan

Cluster name `itgw` (every case starts from a wiped store and creates it
through the gateway — never `workerctl put-cluster`). DNs are
`127.0.0.1:29820..23`, locations `rack0..rack3`; CNs `127.0.0.1:29830..32`,
locations `rack0..rack2` (distinct — the CN scan location-dedupes too). Fake
tr confs: `tcp/ipv4/127.0.0.1/44<port suffix>`. SPs: `sp0` (S, B, C
baseline), `spA0..spA9` (A), `spD0..spD9` (D). tds `t0 t1 …`, subsystems
`nqn.2025-01.io.dnv:itgw:<sp>:ss<i>`, namespaces by `ns_idx` 1.., clones
`cl0..`, transfers `x0..`, migrations `m0..`. Trace ids `it-<case>-<stage>`
via `stage` — the thread that ties script stages to gateway/agent/etcd log
records. Per-case reset: stop gw0..2 (recorded pids) → wipe
(`etcdctl del --prefix "dnv "`) → `reset_state`+`clear_behavior` on every
fake → truncate logs → start gw0..2 → ping all three.

### 10.6 Test-time constants

| constant | value | why |
|---|---|---|
| DN `--size` | 68719476736 (64 GiB) | 64 extents at the default 1 GiB `extent_size`; small exact numbers for free-count asserts |
| CN size | `--size 0`, passed explicitly | the fakeagent's own default is 1 TiB, so the suite asks for 0: `GetCnSize` 0 ⇒ `DefaultCnCap` 4 TiB ⇒ 4096 extents; never a constraint |
| SP shape | `cntlr_cnt=2 slice_cnt=1 init_ext_cnt=2`, raid1 (defaults otherwise) | 1 slice = meta grp (1 ext × 2 legs) + data grp (2 ext × 2 legs) = 6 extents on 4 distinct DNs — fits 4 fakes with room for migration/spare picks |
| per-SP extent cost | 6 | derived above; A's 10 SPs cost 60 of the 256 DN extents |
| `PAR_CLIENTS` | 10 | Q-settled parallel width (case A waves) |
| `RACE_N` | 8 | contention fan-in (case B) |
| `WAIT_SHORT` | 5 s | process/port readiness polls |
| `RETRY_BUDGET` | 20 | per-job stale-token retries, passed to `race` as `--retry-budget`, spent by `retry_stale` jobs |
| `td size` | 67108864 (64 MiB) | multiple of `slice_cnt × stripe_size` (1 × 64 KiB) |

### 10.7 Setup phase (after start-cleanup)

1. preflight driver (tools, `resolve_jq`, `fetch_etcd`, `make build`, the
three `go build`s and `read_constants` — §10.4's driver-side
`workerctl constants`, last because it runs the binary just built),
2. `cleanup` (unconditional), 3. preflight server (§10.4 —
after the cleanup, so a crashed previous run's ports cannot fail it),
4. `mkdir -p` the `$WORK` tree and `SETUP_DONE=1`, 5. one
`scp -q` of `etcd etcdctl dnv-gateway gatewayctl workerctl fakeagent` to
`$WORK/bin` + `chmod 0755`, 6. start etcd, `wait_until` workerctl ping,
7. start the 7 fakeagents, 8. start gw0..gw2, ping each via gatewayctl.

### 10.8 The driver: gatewayctl

`// Command gatewayctl is the gRPC driver of the dnv-gateway integration
test (gateway.md §10).` Same conventions as `dnagentctl`: speaks the
generated `pb.GatewayClient` (no server reflection exists); stdout is exactly
one JSON document per invocation (protojson with `UseProtoNames: true,
EmitUnpopulated: true`, re-encoded through `encoding/json`); all slog records
to stderr; exit 0 on the expected outcome, 1 on any other, 2 on usage errors;
runs **on the server** (`gw()` wraps it in `sshw` with `printf %q` per-arg
quoting, exactly like `worker_test.sh`'s `ctl()`).

Globals: `--gateway` (default `127.0.0.1:29810`), `--cluster` (default
`itgw`, becomes `cluster_name`), `--trace-id`, `--timeout` (seconds, default
10), `--expect <CODE>` (default `OK`; UPPER_SNAKE gRPC code names; when a
non-OK code is expected, a matching failure prints
`{"code":…,"message":…}` and exits 0 — the caller's `set -e` then catches
both wrong successes and wrong failures). Ids accept decimal or `0x` hex
(`hexUint`).

Subcommands — one per RPC, kebab-cased, flags = request fields (rev tokens
always an explicit `--rev <n>`; `--rev 0`/omitted deliberately sends a zero
token, which the B4 stage uses):

| group | subcommands |
|---|---|
| cluster | `create-cluster` `delete-cluster` `get-cluster` `list-clusters [--count --page-token]` |
| dn | `create-dn --addr --location [--tr-*] [--disabled]` · `delete-dn --addr --rev` · `get-dn --addr` · `list-dns` · `set-dn-disabled --addr --rev --disabled` · `inspect-dn --addr` |
| cn | the six mirrors (`create-cn` …) |
| sp | `create-sp --sp [--cntlr-cnt --slice-cnt --init-ext-cnt --slots --raid1]` · `delete-sp --sp --rev` · `get-sp --sp` · `list-sps` · `set-cntlid-slots --sp --rev --slots` · `set-sp-level --sp --rev --level` · `find-sp-names --ids` · `grow-slice --sp --rev --slice [--ext \| --meta] [--dn-black --dn-white]` |
| cntlr | `create-cntlr --sp --rev --slot` · `delete-cntlr --sp --rev --id` · `set-cntlr-enabled --sp --rev --id --enabled` · `inspect-cntlr --sp --id` · `inspect-side --sp --id` |
| td | `create-td --sp --rev --name --size [--ori]` · `delete-td --sp --rev --name` · `list-tds --sp` |
| ss/ns | `create-ss --sp --rev --nqn [--hosts]` · `delete-ss --sp --rev --nqn` · `list-sss --sp` · `set-ss-hosts --sp --rev --nqn --hosts` · `create-ns --sp --rev --nqn --idx --td [--uuid --nguid --suspended]` · `delete-ns --sp --rev --nqn --idx` · `set-ns-dev --sp --rev --nqn --idx --td` · `set-ns-suspended --sp --rev --nqn --idx --suspended` |
| clone | `create-clone --sp --rev --name --dst-td --src-nqn --src-idx --src-slices --src-stripe --src-block [--src-tr…]` · `delete-clone --sp --rev --name [--force]` · `get-clone --sp --name` · `set-clone-tr --sp --rev --name --src-tr…` · `append-clone-bm --sp --rev --name --src-slice-idx --bm-idx --bm-hex` |
| xfer | `create-xfer --sp --rev --name --ori-nqn --ori-idx [--hosts --auto-suspend]` · `delete-xfer --sp --rev --name [--force]` · `get-xfer --sp --name` · `set-xfer-hosts --sp --rev --name --hosts` |
| migr | `create-migr --sp --rev --name --src-side` · `finish-migr --sp --rev --name [--force]` · `cancel-migr --sp --rev --name` · `get-migr --sp --name` · `append-migr-bm --sp --rev --name --bm-hex` |
| spare | `create-spare --sp --rev --grp` · `delete-spare --sp --rev --grp --leg` · `switch-spare --sp --rev --grp --spare --target` |
| bitmap | `get-td-bm --sp --td --slice-idx [--start --cnt]` · `get-leg-bm --sp --leg [--start --cnt]` (bitmaps printed as hex) |
| harness | `ping` (a `ListClusters count=1`; proves the instance serves) · `race` (below) |

**`race`** — the barrier runner for cases A/B. Reads jobs as JSON lines on
stdin: `{"op": "<subcommand>", "params": {…flag names…}, "gateway":
"host:port"?, "retry_stale": bool?, "expect": "<CODE>"?}`; `--targets
a,b,c` round-robins jobs without an explicit gateway. All jobs are prepared,
then released simultaneously (one `sync.WaitGroup` barrier), each with the
global timeout. `retry_stale: true` makes a job that fails `ABORTED` with
"stale revision" re-fetch its SP token via `GetStoragePool` and retry, at
most `--retry-budget` (default 20) times — the documented client protocol,
exercised end-to-end. `expect` is accepted and overrides the global
`--expect` in that job's own copy of the globals — each job gets one, so a
per-job gateway or expectation never leaks into its neighbours — but on this
path nothing ever reads it back: only the one-RPC subcommands compare an
outcome against `--expect`, so a per-job `expect` has no observable effect at
all, on the reported code or the exit rule. `race` measures, the script
asserts. Output: one JSON array ordered by input index,
`{"idx":n,"op":…,"code":"UPPER_SNAKE","reply":{…}|"message":"…","tries":n}`,
where `tries` is how many attempts the job took — 1 unless `retry_stale`
made it re-fetch, so a convergence loop that is not converging shows up as a
max at the retry budget (case B step 6) instead of as a timeout. Exit 0
when every job executed (whatever its code); non-zero only on harness
failure. The script counts codes with jq and asserts the etcd outcome
separately.

### 10.9 Etcd verification: workerctl, read-only plus the worker-role writes

Ground truth never goes through the code under test: after every mutation
stage the script asserts raw decoded etcd state via the existing `workerctl`
subcommands `get --key · get-dn · get-cn · get-rev · get-sp ·
list-keys` (`wctl()` wrapper,
`--endpoints 127.0.0.1:15379 --cluster itgw --trace-id $TRACE`) — plus two
invocations that bypass that wrapper: `ping`, the §10.7 readiness probe, run
on the server with no `--cluster`, and `constants`, run on the driver's own
copy before the server has one (§10.4) — and the
gateway's own read RPCs are asserted as a **secondary** check wherever one
exists (get/list/inspect read-back must agree with ground truth). The only
`workerctl` writes this suite may perform are the four §2.4 worker-role
subcommands: the flips `set-created` and `set-provisioned` — playing the
worker at exactly the steps a precondition demands it (§10.11 steps 9 and
15, and the live `sp0` §10.14 stands up) — and the drains `drain-sp` and
`drain-clone` — standing in for the sp coordinator after a latch the gateway
committed (§10.11 steps 13 and 17, §10.12 step 6, §10.14 step 5, §10.15
step 6) — plus `etcdctl`, for `del --prefix "dnv "` in the per-case wipe and
for the `endpoint status` store-revision probe of the §10.10 refusal
brackets. Anything else writing through `workerctl` (`put-*`, `set-cntlr`,
`set-deleting`, `set-clone-deleting`, …) would test workerctl, not
the gateway, and is forbidden. Rev bookkeeping: `sp_rev`-consuming stages
run in the parent shell, never in subshells (the dnagent suite's documented
counter hazard), and both flips bump `SpRev`, as does every drain step — the
sp drain's last one deletes the key instead (§2.4).

### 10.10 Conventions

* Cases run in the order S A B C D, fail-fast, each against a wiped store —
  no state leaks between cases (`--only` runs one).
* Every mutating stage: ① gatewayctl call (expected code), ② `wctl`
  ground-truth asserts (exact fields through jq; protojson renders uint64 as
  strings — compare via `tostring`), ③ gateway read-back where a read RPC
  covers it. Every **refusal** stage brackets the call with the etcd store
  revision — `store_rev`, read through `etcdctl endpoint status`, which etcd
  advances only for a transaction that wrote — and asserts it did not move
  (`assert_no_write`); where a readable statement of the same fact exists
  (`next_dev_id`, a `wctl` field, a rev read back through the gateway) it is
  asserted too, and
  case C additionally compares a whole `wctl get-sp` snapshot before and
  after each of its steps 1-4 (step 5 mutates) — refusals write nothing,
  provably.
* `assert_field <json> <jq> <want> <label>`; `assert_eq/ne/ge`; `wait_until`
  only for process readiness — the gateway itself is synchronous, so **no
  stage ever sleeps** waiting for etcd content.
* Token discipline: after any successful SP mutation the script refreshes its
  cached `sp_rev` from the reply path (`get-sp`), including after flips.
* One trace id per stage (`it-<case>-<n>`), asserted once in S against the
  fakeagent's log (T3 chain proof).

### 10.11 Case S — `smoke` (sequential, gw0 only)

The full-lifecycle sweep; it gives every RPC but `ListStoragePools` its
happy path (that one runs in §10.12 steps 3 and 6 and §10.15 steps 3, 4 and
6), and §10.13/§10.14 add the refusal paths. Steps (each = one `stage`):

1. `create-cluster itgw` → reply cid; `wctl get --key "dnv cluster_conf
   itgw"`: `creation_epoch != 0` and, although the request named none of
   them, `bdev_conf` / `dn_bin_conf` / `alloc_conf` / `health_check_conf`
   all **present**, every defaultable member at its §7 constant (GW11
   resolves on the write path; the exception the probe must allow for is
   `dn_bin_conf.bin0_shift`, whose constant is itself 0 and which protojson
   therefore omits from the document — the script reads it as `// 0`, and
   bins 1/2/3 at 4/8/12 are what prove the ladder was written rather than
   left unset), while `qos_ratio` and `bdev_conf.redund_conf` — a value no
   rung defaults and a choice that means `redund_none` unset — stay absent;
   three globals `next_id 1`, 256-zero buckets; `get-cluster` read-back
   agrees;
   second `create-cluster itgw` → `ALREADY_EXISTS`.
2. Pagination: create clusters `pg0..pg4`; `list-clusters --count 2` walks
   the six names (incl. itgw) as 2+2+2 — three **full** pages, and per GW10
   a full page always returns a non-empty token, so the empty token costs a
   fourth, empty call, which is what the script asserts; bad
   `--page-token '!!'` → `INVALID_ARGUMENT`; delete `pg0..pg4` (reply cid
   echoes).
3. `create-dn` ×4 (rack0..3) → dn_ids 1..4; per DN: `DnConf`
   (`total_ext_cnt 64 free 64`, tr conf, location), `dn_rev` revision 1, one
   `dn_capacity` key (bin of 64 free), fakeagent `state.json` untouched but
   `agent.log` shows `GetDnSize` carrying this stage's trace id (the T3
   assert); `DnGlobal next_id 5, Σbucket 4`. `get-dn`/`list-dns` (paged)
   agree. `create-cn` ×3 likewise (cn_ids 1..3, 4096 extents).
4. `inspect-dn dn0` → `applied_revision 0` + null `dn_info` (no worker
   runs, so the fake has applied no Syncup — AG3's truthful empty); seed the
   fake's `state.json` (its §14.9 operator-edit path) with a distinctive
   applied revision (7) and re-inspect → `applied_revision 7` + the seeded
   DnInfo, both passed through verbatim while the stored `dn_rev` is still
   1; `inspect-cn` likewise.
5. `set-dn-disabled dn3 true` → capacity key **gone**, `dn_rev` **still 1**,
   `DnConf.disabled true`; repeat (idempotent, still rev 1); re-enable →
   capacity key back. Same once for a CN.
6. `create-sp sp0` (§10.6 shape) → sp_id 1; assert the whole §5.4 write set:
   `SpConf` (id lists, `next_dev_id 1`, and a
   `bdev_conf` concrete in every defaultable member: the three inherited
   from the cluster through D-C's merge (`data_block_size 1048576`,
   `low_water_mark_pct 50`, `stripe_size 65536`), plus
   `redund_md_raid1.bitmap_chunk_block_cnt 128` — the one member no rung
   above the constant supplied, since `--raid1` chooses the kind and names
   no count while the cluster carries no `redund_conf` at all (GW11 again)),
   `sp_id_to_name`, `SpRev{sp0, 1}`, 2 cntlrs (ids, slots 0/1, exactly one
   `primary`, addr = a CN), 1 slice (meta grp ext 1 + data grp ext 2, 2 legs
   each, every side `provisioned false`, distinct DN addrs across all 4
   legs), per-DN `free_ext_cnt` 64−ext, `side_ptr_list` len 1, capacity key
   rewritten, `dn_rev` 2; per-CN `free 4096−3`, `cntlr_ptr_list`, `cn_rev`
   2. `get-sp` read-back deep-equals; `find-sp-names --ids 1,99` → `{1:
   sp0}` only.
7. `grow-slice --meta` → grp per the ladder (+1 ext); `grow-slice --ext 2` →
   new data grp; DN accounting + `sp_rev` advances each time, and so does
   every cntlr's CN: the footprint each cntlr reserves goes 3 → 4 → 6, so
   per-CN `free 4096−footprint` with `cn_rev` 3 then 4, while a CN carrying
   no cntlr of this SP is untouched at rev 1.
8. `set-sp-level READONLY` then back; `set-cntlid-slots 0,1,2`; each bumps
   `sp_rev` by 1.
9. `create-td t0 size 64MiB` → `td_id`, `dev_id 1`, `created false`;
   `create-td t1 --ori t0` → `FAILED_PRECONDITION`; **bracketed no-write
   assert** (§10.10) — `next_id`/`next_dev_id`/`sp_rev` unchanged, no key;
   `wctl set-created --sp sp0 --name t0` (flip + rev note); retry snapshot →
   OK, `ori_id == t0.dev_id`, `dev_id 2`; `list-tds` map agrees.
10. `create-ss ss0` → `serial == %016x(ss_id)`, `model dnv`, `CdcEntry`
    holds **both enabled** cntlrs' tr confs + hosts; `set-ss-hosts` →
    Subsystem **and** CdcEntry; `create-cntlr` (slot 2, on whichever CN the
    random §6.5 pick left without a cntlr of sp0 — computed from the stored
    cntlrs, never hardcoded) → CdcEntry
    gains its tr conf; `set-cntlr-enabled false` → tr conf removed (and
    `true` back on); `delete-cntlr` of that disabled non-primary → clean
    reversal; `list-sss` agrees.
11. `create-ns idx 1 td t0` (empty uuid/nguid) → stored uuid matches
    RFC-4122 dashed form, nguid 32 hex; duplicate idx → `INVALID_ARGUMENT`;
    `set-ns-dev → t1`; `set-ns-suspended true`.
12. `create-xfer x0` (origin ss0/1) → Transfer stored; `set-xfer-hosts`;
    `delete-xfer --force` (abort: origin ns untouched); recreate;
    `delete-xfer` (finalize: origin ns `suspended true` — asserted).
13. `create-clone cl0` (dst t1, src bounds at their limits) → stored; then
    three `append-clone-bm` calls that DISCRIMINATE the §5.8 PAIR addressing
    rather than merely exercising it:
    `--src-slice-idx 0 --bm-idx 0` → one chunk key;
    `--src-slice-idx 0 --bm-idx 1` (a SECOND chunk of the SAME slice) → two
    chunk keys, so a key formed from `src_slice_idx` alone is dead;
    `--src-slice-idx 2 --bm-idx 0` (the FIRST chunk of a SECOND slice) → a
    third key, so a key formed from `bm_idx` alone is dead too. Three calls,
    three keys, none overwritten.
    `wctl get-sp`'s `clone_bm_idx.cl0` (workerctl's index of the chunk keys —
    the gateway's `GetStoragePoolReply` has no such field) is the pair list
    `["0:0","0:1","2:0"]`
    (decimal `src_slice_idx:bm_idx`, ascending);
    `get-clone`; `set-clone-tr`; `delete-clone --force` → the LATCH: `deleting
    true`, the dst namespace already `suspended false`, and the clone key, its
    three chunk keys and its `clone_name_list` entry ALL still there. Then the
    repeat delete (no second bump, forced and unforced), the CLD1 refusals
    (`append-clone-bm`, `set-clone-tr`) and same-name `create-clone` →
    `ALREADY_EXISTS`, each bracketed no-write. Finally `wctl drain-clone` →
    clone gone and the chunk keys of BOTH slices gone, which a drain that
    walked `bm_idx` alone would not manage.
14. Migration on the first data grp: pick a side id from `get-sp`;
    `create-migr m0` → leg has 2 sides (dst `provisioned false`, distinct
    DN, slot ≠ src), dst-DN accounting + `dn_rev` bump; `append-migr-bm` ×2
    → chunks 0,1, `bm_cnt 2`; `get-migr`; `cancel-migr` → fully reversed
    (sides, DN, chunks); recreate `m1`; `finish-migr --force` → dst side is
    the leg's side, src DN released, migration + chunks gone.
15. Spare legs on the data grp: `create-spare` → spare leg, side
    `provisioned false`, DN accounting; `switch-spare` →
    `FAILED_PRECONDITION` (unprovisioned — §9.4), bracketed no-write;
    `wctl set-provisioned` on the spare's side; `switch-spare` → OK, reply
    ids correct, positions swapped, replaced leg parked in
    `spare_leg_list`; `delete-spare` of the parked leg → DN released.
16. `get-td-bm t0 slice 0` and `get-leg-bm` → the fake's empty bitmap
    passes through verbatim (hex-printed); `inspect-cntlr`/`inspect-side` →
    `applied_revision` = the distinctive revision (7) seeded into the fake's
    hand-written object state — ≠ the current `sp_rev`, proving the store is
    not the source — + fake info.
17. Reverse teardown: delete ns → ss → tds (t1 then t0 — snapshot-child
    gate observed) → `delete-sp`, which LATCHES (§5.4): `deleting` true, the
    cntlr and slice keys still there, a repeat delete OK with no second bump,
    and — while latched — `create-sp` of the same name `ALREADY_EXISTS`,
    `set-sp-level` and `grow-slice` `FAILED_PRECONDITION`. Then `wctl
    drain-sp` (§2.4) finishes what the coordinator would: D1 + one batch per
    slice + D3, after which the full accounting is restored — every DN back
    to 64 free, CNs 4096, capacity keys back, `Σbucket` 0 for sp → `delete-dn`
    ×4 / `delete-cn` ×3 → `delete-cluster` → `wctl list-keys --prefix dnv`
    count 0.

Proves: every RPC's happy path writes/removes exactly the specified keys; the
worker-flip gates behave; trace ids flow end to end.

### 10.12 Case A — `parallel` (3 gateways, disjoint resources)

1. `create-cluster` (gw0).
2. `race` wave over gw0-2: 4 `create-dn` + 3 `create-cn` (7 jobs) → 7 × OK;
   dn_ids = {1..4} as a **set**, cn_ids {1..3}; globals `next_id` 5/4,
   `Σbucket` 4/3; every rev 1.
3. `race` wave: 10 `create-sp spA0..spA9` (§10.6 shape) → 10 × OK (internal
   candidate-unit retries invisible); sp_ids a 10-set; `SpGlobal` consistent;
   **aggregate accounting exact**: Σ DN free = 256 − 60, every capacity key
   matches its `DnConf`, every SP's 4 legs on 4 distinct DNs, every
   `sp_rev` 1.
4. `race` wave: 10 `create-td` (one per SP, per-job token fetched by the
   script from each `get-sp`) → 10 × OK.
5. `race` waves: 10 `create-ss`, then 10 `create-ns` (per-SP, fresh tokens
   between waves) → all OK; 10 CdcEntries.
6. Reverse `race` waves: 10 `delete-ns`, `delete-ss`, `delete-td`,
   `delete-sp` → all OK; the ten SPs are then all LATCHED and none removed,
   and `wctl drain-sp` finishes each in turn (sequentially: what the wave
   races is the ten concurrent latches, and concurrent drains are model's
   unit test). Final state exactly step-2's: frees 64/4096 restored, capacity
   keys back, only cluster+nodes remain.

Proves: N clients × M instances on disjoint resources all succeed with
exact global invariants — the settled definition of parallel correctness
(a).

### 10.13 Case B — `contention` (3 gateways, barrier races)

1. Wiped store; `race`: 8 × `create-cluster itgw` → exactly 1 OK, 7
   `ALREADY_EXISTS` (jq code count); exactly one `cluster_conf`; the three
   globals belong to the winner's cid (`get-cluster` cid == the OK reply's).
2. `race`: 8 × `create-dn` **same addr** → 1 OK, 7 `ALREADY_EXISTS`;
   `DnGlobal.next_id 2`, `Σbucket 1` — losers burned no id.
3. `create-sp sp0`; `race`: 8 × `create-td` distinct names, **same token**
   → exactly 1 OK, 7 `ABORTED`; exactly 1 td exists; `SpConf.next_id`
   advanced by 1; `sp_rev` +1. (The `architecture.md` §5.5 token is the serializer.)
4. Stale probes (sequential): `set-sp-level` with the pre-step-3 token →
   `ABORTED`, bracketed no-write; with the fresh token → OK. `create-td`
   duplicate name, fresh token → `ALREADY_EXISTS`, no-write
   (`next_dev_id` unchanged). `--rev 0` (a *present* zero token — gatewayctl
   never sends an absent message, §10.8) → `ABORTED`.
5. `create-ss` + `create-ns`; `race`: 2 jobs, same token —
   `set-ns-suspended` vs `delete-ns` → exactly one OK, one `ABORTED`; the
   script branches on which won and asserts etcd matches it exactly (ns gone
   xor ns suspended).
6. `race` with `retry_stale`: 8 × `create-td` distinct names, initial equal
   tokens → **all 8 eventually OK** within `RETRY_BUDGET`; 8 tds;
   `sp_rev` +8; `next_id` +8; `next_dev_id` +8. (The client retry protocol
   converges.)

Proves: exactly-one-winner semantics, no id/partial-write leakage by losers,
token protocol both as fence (3-5) and as liveness loop (6) — the settled
definition of (b).

### 10.14 Case C — `faults` (gw0; refusals and agent failures)

A fixture stage (the shell's stage 0, not one of the five batteries) first
creates the cluster, the nodes and a live `sp0` with four tds (one flipped
`created` by `wctl set-created`), a subsystem with a namespace, a clone, a
migration and a spare leg; the numbered steps are the batteries run against
it. The fixture's own RPCs are not counted in §10.18's matrix.

1. Validation battery (each → `INVALID_ARGUMENT`, one shared bracketed
   no-write around the whole batch): bad name pattern (`sp!0` — `/` and `.`
   are legal name characters, so `sp/../x` would prove nothing), 65-byte
   name, bad NQN (and the discovery NQN), td size not a multiple, grow-slice
   `--meta --ext 2`, slots with dupes / value 8, `count` at both ends of
   its bound (0→64 accepted, 2000 → `INVALID_ARGUMENT`, never capped), bad page token,
   nonempty `bdev_feature_list`.
2. `NOT_FOUND` battery: every group probed once against a missing cluster /
   sp / dn / td / nqn / ns_idx / clone / xfer / migr / side / spare ids.
3. Precondition battery on a live sp0 (each `FAILED_PRECONDITION`,
   bracketed): `delete-cluster` nonempty; `delete-dn` with sides;
   `delete-sp` with tds; `delete-ss` with ns; `delete-cntlr` primary, and
   enabled non-primary; snapshot of uncreated origin; `delete-td` referenced
   by ns / by clone dst / by uncreated snapshot child; `create-migr` on a
   2-side leg; `switch-spare` unprovisioned; and — since the 2026-09-15
   amendment made `delete-sp` a latch (§5.4) — an SP-level mutator against a
   really latched `deleting` SP, driven in case S's teardown stage rather than
   here, where the SP must stay live for the rest of the battery.
4. Agent faults via `behavior.json` (worker-suite knobs) and process
   control: dn0 `hang` → `create-dn` new addr on dn0's port… (a hung
   *listening* agent) → `ABORTED` and wall-clock between
   `DefaultGatewayAgentTimeout` and timeout+5 (the AG2 bound, generous upper
   fence); closed port → `ABORTED` fast; `inspect-dn`/`inspect-cn` against
   hung/stopped fakes → `ABORTED`; `clear_behavior` restores.
5. force=false checks: clone `cl0` with cn fake's behavior reporting
   incomplete hydration rows → `delete-clone` → `FAILED_PRECONDITION`;
   fake stopped → still `FAILED_PRECONDITION` (unreachable is not proof);
   rows complete → OK. Same for `finish-migr` via the dn fake's
   `GetSideInfo` (`migr_dst_info.dm_clone_info`), then `--force` path for
   the stopped-agent variant.

Proves: refusals are total (no partial state, provably via store_rev
brackets), codes match GW7, agent-call budget is bounded — the settled (c).

### 10.15 Case D — `restart` (kill -9 under load)

1. `create-cluster` + nodes (as A step 2, sequential is fine).
2. Launch in background: `race` 10 × `create-sp spD0..spD9` across gw0-2.
   While it runs, `kill -9` gw1 by recorded pid (no drain).
3. Join; partition results: jobs routed to gw1 ∈ {OK, UNAVAILABLE/ABORTED
   transport}; all others OK. For **every** spD*: either the complete §5.4
   write set exists (reuse S-step-6's `verify_sp` helper) or **no key of it
   at all** (`list-keys` audit per sp name + sp_id) — etcd transactionality
   means no third state. Recompute expected DN/CN frees from the survivor
   set and assert exactly.
4. Re-drive every failed job against gw0 → OK; now 10 complete SPs.
5. Restart gw1 with the identical command line → `ping` OK; one
   create/delete round-trip through gw1; assert `list-keys` full dump
   contains **only** `architecture.md` §5.3 kinds owned by the data — no gateway
   registration/lease/residue of any kind existed or exists.
6. `delete-sp` ×10 via round-robin, each latch immediately followed by its
   `wctl drain-sp`; accounting restored.

Proves: crash atomicity, statelessness (nothing to clean, instant rejoin),
survivors unaffected — the settled (d).

### 10.16 Teardown and cleanup (`cleanup()`, also `--cleanup-only`)

Heredoc piped to `ssh … bash -s`: gather `$WORK/*/pid`, `kill -CONT`,
`kill -TERM`, wait ≤ 5 s, `kill -KILL`; then the safety nets
`pkill -f 'bin/dnv-gateway'`, `pkill -f 'bin/fakeagent'`,
`pkill -f 'etcd --name dnv-gw-it'`; `rm -rf $WORK`. Run unconditionally at
start, on success at exit; skipped on failure (debris kept for inspection,
said so on stderr).

### 10.17 Failure diagnostics

On `die`: banner, then per process the last 50 log lines (`etcd`, `gw0..2`,
`dn0..3`, `cn0..2` — via `rlog`, tolerant of missing files), `wctl
list-keys --prefix dnv` full dump + `wctl get --key "dnv cluster_conf
itgw"`, `ss -ltn` filtered to the port block, `ps -ef | grep $WORK`. The
failing stage's trace id is in the banner — grep it across gateway/agent
logs to follow the request.

### 10.18 Coverage matrix

Every RPC appears in ≥ 1 case; S covers every happy path but
`ListStoragePools`, which runs only in A (steps 3, 6) and D (steps 3, 4, 6).

| RPCs | S | A | B | C | D |
|---|---|---|---|---|---|
| CreateCluster / DeleteCluster / GetCluster / ListClusters | 1,2,17 | 1 | 1,2 | 1,3 | 1 |
| CreateDiskNode / Delete / Get / List / UpdateDisabled / Inspect | 3,4,5,17 | 2,6 | 2,3 | 2,3,4 | 1 |
| the six ControllerNode mirrors | 3,4,5,17 | 2,6 | 3 | 2,4 | 1 |
| CreateStoragePool / Delete / Get / List / UpdateCntlidSlotList / UpdateLevel / FindNames | 6,8,17 (List: A, D only) | 3,6 | 3,4 | 1,2,3,5 | 2,3,4,6 |
| GrowSlice | 7,17 | — | — | 1 (validation) | — |
| CreateCntlr / DeleteCntlr / UpdateCntlrEnabled | 10 | — | — | 3 | — |
| InspectCntlr / InspectSide | 16 | — | — | 2 | — |
| CreateThinDevice / DeleteThinDevice / ListThinDevices | 9,17 | 4,6 | 3,4,6 | 1,2,3 | 5 |
| CreateSubsystem / Delete / List / UpdateHosts | 10,11,17 | 5,6 | 5 | 1,2,3 | — |
| CreateNamespace / Delete / UpdateDev / UpdateSuspended | 11,12,17 | 5,6 | 5 | 2 | — |
| CreateClone / Delete / Get / UpdateTrConf / AppendBitmap | 13 | — | — | 2,5 | — |
| CreateTransfer / Delete / Get / UpdateHosts | 12 | — | — | 2 | — |
| CreateMigration / Finish / Cancel / Get / AppendBitmap | 14 | — | — | 2,3,5 | — |
| CreateSpareLeg / Delete / Switch | 15 | — | — | 2,3 | — |
| GetThinDeviceBitmap / GetLegBitmap | 16 | — | — | — | — |

Rule coverage: GW6 → B3-6; GW7 → C throughout; GW9 → A3 (implicit) ;
GW12 → A2/B2; AG2/AG3 → C4-5; AG4 → C5; GW1/§0 #3 → D. Unit-only (§9):
`CreateCluster` hash-collision guard branch, GW10 token-decode internals,
GracefulStop. (The `SpConf.deleting` gate left this list on 2026-09-15:
`delete-sp` sets the flag now, so case S drives the gate for real.)

### 10.19 Out of scope (v1)

Performance/latency/soak; real agents, dnv-worker, dnv-cdc, dnvctl; multi-
node etcd; etcd outage behavior (unit-level only); TLS/auth (none exists in
dnv); clone-budget enforcement (§0 #16). The `deleting`-mediated async
teardowns left this list in HALF — `delete-sp` on 2026-09-15 and
`delete-clone` on 2026-09-16: each sets its flag and case S asserts the latch
(§10.11 steps 17 and 13), but the drains that consume them belong to the
sp-worker, which this suite does not run — `wctl drain-sp` and `wctl
drain-clone` stand in for them (§2.4), and dnv-worker.md §14's case G owns
the real coordinator for both.

---

## 11. Amendments to companion documents (applied with the implementation)

* `common/constants.go`: add `DefaultGatewayAgentTimeout` (§2.1).
* `model`: the five §2.2 items (exports; `expectRev` on the three shared
  ops + `ReasonStaleRevision`; the three `*ConfPrefix` builders; GW11's
  `ResolveBdevConf` plus the `Validate*Conf` pair). Worker call sites of the
  three ops pass `expectRev = 0`.
* `integtest/workerctl`: `set-created`, `set-provisioned`, and — with the
  2026-09-15 latch — `drain-sp`, the worker-role stand-in that finishes a
  teardown this suite's gateway only starts, plus `set-deleting`, the worker
  suite's own latch (§2.4, §5.4); with the 2026-09-16 clone latch, their
  clone twins `set-clone-deleting` and `drain-clone` (§2.4, §5.8).
* `doc/cdc.md` §"CdcEntry ownership": rename the RPC it calls
  `UpdateSubsystemAllowedHosts` to the real name **`UpdateSubsystemHosts`**
  (proto and architecture.md §8.8 agree; cdc.md is the outlier).
* `README.md`: remove `gateway/` from the not-yet-implemented list (and the
  already-stale `cdc/` mention while there); add the gateway suite to the
  integtest enumeration.
* `doc/layout.md`: add `integtest/gatewayctl/` and `gateway_test.sh` to the
  integtest tree listing.
* No change to `doc/grpc.md` (the agent-call timeout lives in this doc +
  the constant's comment) and none to `pb/schema.proto`.

---

## 12. Acceptance checklist

1. `make build` produces `bin/dnv-gateway`; `go vet ./...` clean;
   `go test ./...` passes with and without an etcd binary on PATH (skips
   are visible, not silent, when absent — EU7 spirit).
2. All 59 RPCs registered and implemented; no handler performs etcd I/O
   outside `etcdutil`; no agent call inside an STM (grep-auditable:
   `grpc.NewClient` appears in `gateway/` only in the AG2 helper).
3. Both interceptor chains on the server and on every agent connection
   (grpc.md acceptance applies to the new call sites).
4. Log records: `gateway starting/serving/stopping` present; no bespoke
   per-request logging in handlers; `PbToLogValue` for any pb payload.
5. §2.2/§2.4 amendments applied; worker unit+integration suites still pass
   (the `expectRev = 0` paths are behavior-preserving).
6. `bash integtest/gateway_test.sh user@<lab vm>` prints `PASS`; each
   `--only` case passes in isolation; `--cleanup-only` leaves the ports
   free and `$WORK` absent.
7. The §10.18 matrix is true of the implemented script (spot-audit: every
   RPC name greps to ≥ 1 stage).
8. Documentation amendments of §11 committed with the code.
