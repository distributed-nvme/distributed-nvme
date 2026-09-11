# ThinDeviceCreated.md — `ThinDevice.created` (dnv)

Status: **normative** — applied, in full. U1, U4 and U5 were implemented
first; U2 and U3 have since landed with `gateway/` and `worker/`
(`gateway/thindevice.go`, `worker/sprole.go` + `model.FlipCreated`).
`gateway.md` §5.6/§9 and `dnv-worker.md` RW19/§13 are now the normative
gateway/worker spec and test inventories of record for U2/U3, re-scoping the
U2-T/U3-T lists below (§9 item 5). §10
records where the implementation had to depart from this document. This
document is the complete spec of
one change: a `created` flag on `ThinDevice` that records when every slice
pool of an SP holds the td's thin volume, the gateway rules that gate on it,
the sp-worker rule that sets it, and the `dnv-agent cn` simplification it
funds. It is written in the shape of `update_01.md`/`update_02.md`: numbered
items U1-U5, each with its own test list (U*n*-T*m*), followed by the exact
amendments the companion documents receive when the change is implemented.
`schema.proto`, `architecture.md`, `cnagent.md`, `cnagent_integtest.md` and
the code were edited when this landed, and each companion document records
its own edits in its amendments section, citing `ThinDeviceCreated.md
U*n*`. §8 below is the list those edits were made from, kept as written.

Required background: `schema.proto` (`ThinDevice`, `CntlrInfo.ThinInfo`,
`ResInfo`/`ResStatus`, `SyncupCntlrRequest.td_list`, `SyncupCntlrReply`,
`CheckCntlrReply`, `ListThinDevicesReply`); `architecture.md` §3.3 (the
per-td × slice thin volumes), §5.5 (revision keys and the sync fan-out),
§5.8 (STM discipline), §8.7 (thin devices), §9.1 (agent rules), §9.5
(live-state reporting), §9.7 (Check streams), §10.3 (sp role, the
`provisioned` flip), [D12], [D15], Appendix D; `cnagent.md` CN9 (pass
structure), CN14 (thin volumes, the `update_02.md` U1 snapshot pre-pass),
CN21 (teardown deactivates, never deletes), §6 test 19; `layout.md` §2
(`gateway/thindevice.go`, `worker/sprole.go`, `worker/revision.go` — the
Check-stream consumer; the worker landed without a `check.go`,
`layout.md` §8 — `agent/cnagent/pool.go`, `agent/cnagent/syncup_cntlr.go`).

Conventions: "the origin" is the td whose `dev_id` a snapshot's `ori_id`
names; "materialized" means the td's thin volume id exists in every slice
pool's dm-thin metadata; "row" is one `ResInfo` of `CntlrInfo`. Citation
form from other documents: `ThinDeviceCreated.md U2`, `U4-T3`.

---

## 0. Decision record

The design was settled in an interview; every branch below is a recorded
decision, not an assumption.

| # | decision | why |
|---|---|---|
| R1 | Field name `created`, `bool created = 5` on `ThinDevice`. | The next free field number; `ThinDevice` already rides in `td_list` and `name_to_td`, so no other message changes. |
| R2 | `created` is **monotonic**: set once by the sp-worker, never cleared. | A pool holds a thin id until a `delete {dev_id}` reaches it — sent by the td's own deletion or, when CN14's pool-presence gate skipped that fan-out's message (a demotion or pool suppression coalesced with the delete), by the CN14 activation sweep at the pool device's next re-creation (update_05.md U3); a later bad row is a health event (`err_epoch`), not evidence the id is gone. |
| R3 | A td deleted and recreated under the same name is a **different td**: new `td_id` (from `next_id`), new `dev_id` (from `next_dev_id`), `created = false`. | ids are never reused (§8.7); the key is rewritten, not updated. |
| R4 | Flip predicate: `agent_reply.code == 0`, and the td's `slice_id_to_dm_thin` holds **exactly** the SP's slice ids, all `RES_STATUS_OK`. No revision match. | Standbys report no thin rows at all, so "every row OK" over an empty map is vacuously true; the coverage clause closes that. Thin ids are monotonic facts, so a reply against an older revision that shows every slice OK is still true. |
| R5 | The flip STM **bumps `SpRev`** once. | §5.5: any STM that changes agent-visible desired state bumps the revision once; `created` rides in `td_list` and the agent consumes it (R9). |
| R6 | One STM and one bump **per reply**, covering every td that reply completes (batching allowed). | One `CheckCntlr` reply carries every td of the primary; per-td bumps would fan the identical state out once per td. Same allowance §10.3 gives the `provisioned` flips. |
| R7 | Gating scope: **snapshot creation only**. `CreateNamespace`, `UpdateNamespaceDev`, `CreateClone`, `GetThinDeviceBitmap` are not gated. | `create_snap` is the one operation with a kernel-level dependency on the origin id being in the pool; ns-devs park on `CnErrorName` and clone destinations are empty tds ([D3]). |
| R8 | `DeleteThinDevice` of an origin is refused while any snapshot of it has `created == false`, found by reading the SP's tds inside the STM (no reverse index). | Retire runs before build (CN9): an origin leaving `td_list` in the converge that first materializes its snapshot sends `delete {ori dev_id}` before `create_snap` and loses the snapshot for good. `ListThinDevices` already reads the same set in one STM. |
| R9 | The cn agent **uses** the flag: `created == true` means "never send a pool message for this td again". | A failover sends zero pool messages instead of one failing `create_thin`/`create_snap` per td × slice, and a pool that lost an id surfaces as `RES_STATUS_ERROR` instead of being silently recreated as an empty volume — which is what today's unconditional `create_thin` would do. |
| R10 | The snapshot pre-pass owns every message of an uncreated snapshot; the lazy `createSnapId` fallback and the `snapDone` handoff are removed; `td_list` order carries no meaning. | With the origin guaranteed materialized (R7/R8), same-pass origin-then-snapshot ordering can no longer occur, which was the only reason for both. |
| R11 | A violated precondition at the agent (a `create_snap` whose origin id the pool lacks) is left to dm-thin: the row reports `RES_STATUS_ERROR` with the dmsetup output and is retried on every converge. | The origin guarantee is the gateway's contract to keep, not the agent's to re-check. |
| R12 | Any cntlr's reply may flip. | Thin rows are only ever filled by a cntlr acting as primary at the revision it applied; the ids live in the shared pool metadata on the DN legs. Identity is guarded by the STM's `td_id` re-read. |
| R13 | Clients learn that a td can be snapshotted by polling `ListThinDevices` for `created == true`. No new RPC; `CreateThinDevice` never blocks. | §5.8 keeps every RPC short; `dnvctl`'s `vol` subcommands show the field once `ctl/` lands. |
| R14 | On-hardware coverage: `integtest/cnagent_test.sh` case B gains a teardown-and-rebuild stage asserting zero *device-set-mutating* pool messages with `created = true` — no `create_thin`, no `create_snap`, no `delete`; the rebuild's pool re-creation does run the update_05.md U3 activation sweep, whose `reserve_metadata_snap`/`release_metadata_snap` pair is the stage's only `dmsetup message` traffic. | It is the only place a real dm-thin pool proves that a bare `dmsetup create` on an existing id works without the message. |

---

## 1. Problem

Snapshot creation (`create_snap {dev_id} {ori_id}`) needs the origin's id
in each slice pool's metadata. Today nothing in the control plane knows
whether that is so: `CreateThinDevice` with `ori_name` succeeds the moment
the origin *record* exists, and the cn agent copes by ordering — the
`update_02.md` U1 pre-pass declines any slice whose origin thin device is
absent on this CN and hands it to the lazy per-td path, which relies on the
origin preceding the snapshot in `td_list` (cnagent.md CN14, §6 test 19).
Three consequences:

* Ordering logic on the agent (`snapshotPrePass`'s origin-device filter, the
  `snapDone` handoff between the pre-pass and `ensureThin`, the snapshot
  branch of `createThinId`) exists only to survive a control-plane state
  that a materialization flag would make impossible.
* `DeleteThinDevice` of an origin is unconditional. If the primary is down
  when a snapshot is created and the origin is deleted before it comes
  back, the next converge sends `delete {ori dev_id}` (retire phase) before
  `create_snap` (build phase) and the snapshot is permanently empty.
* Every converge on a CN whose devices are absent — a failover, a restart
  reconcile — sends one `create_thin`/`create_snap` per td × slice that the
  pool already holds, tolerating `EEXIST` (cnagent.md CN14: "a message for
  an id the pool already holds fails harmlessly"). That tolerance is also a
  hazard: a pool that somehow *lost* an id is silently given a fresh, empty
  volume under the same `dev_id`.

`ThinDevice.created` fixes all three: it is set exactly once, when the
primary has reported the td's thin volume `RES_STATUS_OK` in every slice.

---

## 2. U1 — Schema: `ThinDevice.created`

**U1-S1 (`pb/schema.proto`).** `ThinDevice` gains one field:

```proto
// {dnv_prefix} thin_device {cluster_id} {sp_id} {td_name}
message ThinDevice {
    uint64 td_id = 1;
    uint32 dev_id = 2;
    uint32 ori_id = 3;
    uint64 size = 4;
    // created is set exactly once by the sp-worker, when a cntlr has
    // reported this td's thin volume RES_STATUS_OK in every slice of the SP
    // (§10.3); it is never cleared. It gates snapshot creation and origin
    // deletion (§8.7) and tells the cn agent that the ids exist in every
    // slice pool, so no pool message is ever sent for this td again (CN14).
    bool created = 5;
}
```

`make gen` regenerates `pb/schema.pb.go` (`GetCreated()`); no service, RPC
or other message changes. The field rides where `ThinDevice` already rides:
`SyncupCntlrRequest.td_list` (agent-visible, §9.1 full sync) and
`ListThinDevicesReply.name_to_td` (client-visible, R13). The agent's
persisted `SyncupCntlrRequest` (§9.1 local store) therefore carries it too.

**U1-S2 (semantics).**

* Written `false` by `CreateThinDevice` (U2). Set `true` by the sp-worker
  flip (U3). Never written by anything else in a dnv binary; never cleared
  (R2). (The gateway integration suite's `workerctl set-created` plays the
  sp-worker on a lab where no dnv-worker runs — gateway.md §2.4/§10.9 —
  through the same `model.FlipCreated` STM, identity guard included, so R2's
  monotonicity holds there too.)
* proto3 default: a record without the field reads `false`, which is the
  correct meaning for any td whose materialization has not been observed.
  There are no deployments to migrate, so no migration step exists; a
  future reader that meets an
  old record simply waits for the next Check round to flip it.
* Same-name recreate (R3): `DeleteThinDevice` deletes the key, a later
  `CreateThinDevice` with the same `td_name` writes a new record with new
  ids and `created = false`. A snapshot record's `ori_id` names a `dev_id`,
  which is never reused, so an old snapshot can never resolve to the new
  td.

**U1-T1.** `go build ./...` after `make gen`; `grep -n "bool created = 5"
pb/schema.proto` hits inside `message ThinDevice`; `clang-format` (the
`make fmt` proto style) leaves the file unchanged.

---

## 3. U2 — Gateway: the snapshot precondition and the origin-deletion guard (§8.7)

All three RPCs stay one STM each (§5.8). `cluster_id` is derived in-STM as
today; every read below is inside the same transaction.

**U2-S1 `CreateThinDevice`.**

* Existing validation and errors are unchanged (`ALREADY_EXISTS`,
  `RESOURCE_EXHAUSTED`, `NOT_FOUND` `ori_name` set but absent,
  `INVALID_ARGUMENT` size rules).
* New error, evaluated after the `NOT_FOUND` check on the origin:
  `FAILED_PRECONDITION` when `ori_name` is set and the origin's
  `created == false`. Details: `origin {ori_name} is not created yet; wait
  for ListThinDevices to report created = true`. Nothing is written, no
  `SpRev` bump.
* Action: unchanged, plus the new record is written with `created = false`.
  A fresh td (`ori_name = ""`) needs no check.
* A snapshot of a snapshot follows the same rule: the *immediate* origin
  must be created. Its own materialization is what makes it eligible as an
  origin later.

**U2-S2 `DeleteThinDevice`.**

* New error: `FAILED_PRECONDITION` when any td of the SP has
  `ori_id == target.dev_id && created == false`. Details name the blocking
  snapshot(s): `snapshot {td_name} of {target} is not created yet`.
* Read set: `SpConf.td_name_list`, then every `ThinDevice` it names — the
  `ListThinDevices` read set, bounded by `MaxTdCntPerSp` = 1024 — in the
  same STM as the existing `Namespace`/`Clone` reference checks, so a
  snapshot created concurrently conflicts the transaction (§5.8/§5.9
  `ABORTED`, client retries). `etcdutil` MAY serve the td reads as one
  range under the `{p} thin_device {cluster_id} {sp_id} ` prefix when its
  STM wrapper supports prefix reads (logged as one `etcd range`, log.md
  §5.3), and per key otherwise; either is inside the transaction.
* A snapshot with `created == true` does **not** block: dm-thin snapshots
  stay valid after their origin is deleted (§8.7, unchanged). The match is
  on `dev_id`, which is never reused, so no stale snapshot can block a
  same-name recreate.
* The existing `Namespace.td_id` / `Clone.dst_td_id` checks and the action
  (remove from `td_name_list`, delete the key, bump `SpRev`) are unchanged.

**U2-S3 `ListThinDevices`.** Unchanged; `name_to_td` now carries `created`.
This is the client's wait primitive (R13): after `CreateThinDevice`, poll
until `created == true` before creating a snapshot of the td. Typical
latency is one fan-out: the `SpRev` bump of `CreateThinDevice` sends
`SyncupCntlr` to the primary, whose reply already reports every slice `OK`
in the common case, so the flip lands in that same round (U3); worst case is
one `health_check_conf.cntlr_interval` later through `CheckCntlr` (§9.7).

**U2-S4 Not gated (R7).** `CreateNamespace`/`UpdateNamespaceDev` on an
uncreated td: allowed; the ns-dev parks on the td's `CnErrorName` and the
namespace stays `inaccessible` until the backing chain exists (cnagent.md
CN16). `CreateClone` with an uncreated `dst_td_name`: allowed ([D3], the
destination td is empty by construction). `GetThinDeviceBitmap` on an
uncreated td: not gated at the gateway; when the slice's pool does not hold
the id yet, `GetThinDeviceBm` fails with the existing gRPC error (`thin
device {dev_id} not in the metadata snapshot`, cnagent.md CN26), which the
gateway surfaces as `ABORTED` like any other agent RPC failure (§5.9).

**U2-S5 Interaction with `SpLevel` and provisioning.** At an `sp_level`
that suppresses pools, thin rows read `RES_STATUS_MISSING` /
`"sp_level"` (cnagent.md CN19), so no td of that SP ever flips and every
snapshot request is refused with `FAILED_PRECONDITION` until the level is
restored and a converge reports the rows `OK`. A td created while a slice is
still provisioning-deferred ([D15]) reads `RES_STATUS_PROVISIONING` in that
slice and flips when the last slice clears. Both are the intended meaning of
"not created yet".

**Tests (gateway; landed in `gateway/handler_vol_test.go` — gateway.md §9
is the inventory of record and re-scoped this list, §9 item 5).**

* **U2-T1** `CreateThinDevice` without `ori_name` writes `created = false`
  and bumps `SpRev` once; `ListThinDevices` reads it back `false`.
* **U2-T2** `CreateThinDevice` with `ori_name` naming an origin whose
  `created == false` ⇒ `FAILED_PRECONDITION`, no key written,
  `td_name_list` and `SpRev.revision` unchanged, `next_id`/`next_dev_id`
  unchanged. With the origin flipped `true` (write the record directly in
  the test) ⇒ `OK`, `ori_id` = the origin's `dev_id`, the new record
  `created = false`.
* **U2-T3** `NOT_FOUND` still wins over `FAILED_PRECONDITION` when
  `ori_name` is absent.
* **U2-T4** `DeleteThinDevice` of an origin with one uncreated snapshot ⇒
  `FAILED_PRECONDITION` whose details name the snapshot; after the
  snapshot's record is flipped `true` ⇒ `OK`, the origin's key gone, the
  snapshot record untouched. With two snapshots, one created and one not ⇒
  refused, and the details name only the uncreated one.
* **U2-T5** Same-name recreate: create td `a` (`dev_id` 1), delete it,
  create `a` again ⇒ new `td_id`, `dev_id` 2, `created = false`; a snapshot
  record left over with `ori_id = 1` does not block deleting the new `a`.
* **U2-T6** The scan is transactional: a test that injects a concurrent
  `CreateThinDevice` (snapshot of the target) between the STM's read and
  its commit sees the delete retried and then refused, never a deleted
  origin with a live uncreated snapshot.

---

## 4. U3 — sp-worker: the materialization flip (§10.3)

The sp role already consumes every `SyncupCntlrReply` (fan-out) and every
`CheckCntlrReply` (the streams it keeps open to every cntlr of its SPs,
§9.7) for `err_epoch` maintenance. The flip is one more consumer of the same
replies; no new RPC, no polling, no timer. `GetCntlrInfo` replies (the
gateway's `Inspect*` path) are not a source.

**U3-S1 Complete predicate.** For a reply `R` from any cntlr of SP `S`
(R12) and a td `X` of `S`:

1. `R.agent_reply.code == 0` (a rejected request carries no trustworthy
   info);
2. `R.cntlr_info.td_id_to_thin_info[X.td_id]` exists;
3. the key set of its `slice_id_to_dm_thin` equals the set of `S`'s slice
   ids (the worker's loaded `SpConf.slice_id_list` — every slice, no extra,
   no missing);
4. every row's `status == RES_STATUS_OK`.

Anything else — no entry (a standby, cnagent.md CN14 "primary only"), a
partial map, any `MISSING`/`ERROR`/`PROVISIONING`/`UNKNOWN` row — is "not
yet". The reply's `revision` is **not** compared with anything (R4): the
thin ids are monotonic facts about the shared pool metadata, and identity is
guarded in the STM below. `show_info = false` Check replies are sufficient:
§9.7 delivers the `*Info` whenever any resource changed status, explicitly
including `PROVISIONING → OK`, and `MISSING → OK` is a status change like
any other.

**U3-S2 Candidates.** The tds of `R` that are complete **and** that the
worker's loaded state of `S` shows `created == false`. A reply that
completes no candidate causes no etcd traffic at all — steady state costs
nothing, and the re-sync that follows a bump (below) finds its own tds
already `true`.

**U3-S3 One STM per reply (R5, R6).** If the candidate set is non-empty:

1. For each candidate, re-read `{p} thin_device {cluster_id} {sp_id}
   {td_name}`. Skip it when the key is absent (deleted meanwhile), when its
   `td_id` differs (deleted and recreated under the same name, R3), or when
   `created` is already `true` (another worker, or a concurrent reply of
   the same SP, got there first). Otherwise set `created = true` and write
   the record.
2. If at least one record was written, bump `SpRev` **once** (`revision +
   1`, `sp_name` unchanged — the key is id-based and is updated, never
   deleted and recreated, §5.5). If nothing was written, the STM commits no
   change and there is no bump.
3. Log per log.md §5.3 (`etcd get`/`etcd put` records inside the STM; a
   retried transaction logs twice, which is expected).

The bump re-fans the SP: `SyncupSide` to every side and `SyncupCntlr` to
every cntlr with the new `td_list` (§10.3). That is the delivery path of
`created` to the cn agent (U4). It cannot loop: the worker reloads the SP on
its own bump, so the re-sync's reply — which reports the same rows `OK` —
finds no candidate. Two workers or two replies racing on one td meet at step
1 and only one writes.

Several tds completed by one reply share the one STM and the one bump (R6);
the worker MAY also fold a pending `provisioned` flip of the same SP into
the same transaction — the two rules are independent and both bump once.
(The implementation does not take the MAY: the two flips run as two
consecutive STMs.) Replies do not get one STM each either: the sp worker
drains every report pending on its channel at that moment — across replies
and across cntlrs — and folds all their candidates into the one
`FlipCreated` call. Only the primary fills thin rows, so one reply per
round per SP remains the common case; the fold is at most one STM and one
bump regardless, and the flag is monotonic, so the batching is
observationally equivalent.

**U3-S4 Ownership and restarts.** The flip is idempotent and driven by
observation, so a worker restart or a shard-ownership change (§10.1) needs
no recovery step: the new owner's first reply on a fresh Check stream
carries the complete `*Info` (§9.7) and flips whatever is complete and still
`false`.

**U3-S5 What never flips.** Thin rows of a cntlr at a pool-suppressing
`sp_level` (`MISSING`/`"sp_level"`), of a provisioning-deferred slice
(`PROVISIONING`), of a standby (no entry), or of a td whose message or
create failed (`ERROR`). `RES_STATUS_PROVISIONING` keeps its [D15] meaning:
healthy, not ready, no `err_epoch`, and — here — not created.

**Tests (worker; landed in `worker/sprole_test.go` (`TestSpCompletedTds`,
`TestSpFlipBatchesOneStm`, …) and `model/ops_test.go` — dnv-worker.md §13
is the inventory of record and re-scoped this list, §9 item 5. The Check
stream is consumed in `worker/revision.go`; no `worker/check.go` exists.)**

* **U3-T1** A reply with `agent_reply.code == 0` whose `ThinInfo` for td
  `X` holds every slice id `OK` ⇒ one STM: `X.created = true`, `SpRev`
  bumped exactly once; the fan-out that follows carries `created = true` in
  `td_list`.
* **U3-T2** Two tds complete in one reply ⇒ both flipped in one STM, one
  bump. A second identical reply ⇒ no write, no bump.
* **U3-T3** No flip, no write, no bump for each of: a standby's reply (no
  `td_id_to_thin_info` entry); a map missing one slice; a map with an extra
  slice id; one row `PROVISIONING`; one row `MISSING` with details
  `"sp_level"`; one row `ERROR`; `agent_reply.code != 0` with otherwise
  perfect rows.
* **U3-T4** Identity guard: the td is deleted between the reply and the
  STM ⇒ skipped, no bump; the td is deleted and recreated under the same
  name (new `td_id`) ⇒ skipped, the new record stays `false`; the record is
  already `true` ⇒ skipped, no bump.
* **U3-T5** A `show_info = false` Check reply carrying the `*Info` because
  a thin row moved `PROVISIONING → OK` (the last deferred slice cleared) ⇒
  flips.
* **U3-T6** An older-revision reply (the worker has since synced a higher
  revision) with complete rows ⇒ flips; no revision comparison is made.
* **U3-T7** The re-sync triggered by a flip's own bump does not bump again
  (steady-state cost is zero).

---

## 5. U4 — cn agent: created tds are never messaged; the pre-pass owns every snapshot message (CN14)

`agent/cnagent/pool.go` (`ensureThin`, `createThinId`, `createSnapId`) and
`agent/cnagent/syncup_cntlr.go` (the build-phase thin loop and
`snapshotPrePass`). The plan (`plan.go`), the probes (`probe.go`), the
retire phase, CN15-CN21 and the standby shape are untouched.

**U4-S1 Three cases, decided per td by two request fields.**

| `td.created` | `td.ori_id` | who messages | what |
|---|---|---|---|
| `true` | any | nobody | `dmsetup create` of the `thin` table when the device is absent; the id is known to exist in every slice pool (U3). |
| `false` | `0` | `ensureThin` | `create_thin {dev_id}` when the device is absent, then `dmsetup create` — as today (`EEXIST` tolerated: a crash between the message and the create, or a message a previous primary already sent). |
| `false` | `≠ 0` | the pre-pass, only | `create_snap {dev_id} {ori_id}` per pool-ready slice whose snapshot device is absent, inside the `update_02.md` U1 quiesce when the origin's raid0 is live; then the thin loop's `dmsetup create`. |

`ensureThin` therefore never messages a td with `ori_id != 0`, whatever the
pre-pass did: the predicate is re-derivable from the request alone, which is
what lets the `snapDone` handoff go (U4-S3).

**U4-S2 Created tds (R9).** A td with `created == true` gets no pool message
on any pass — fresh primary after a failover, startup reconcile from the
local store (SH1-SH3), a device removed by hand, ever. `ensureThin` reads
`dm.Info`; when the device is absent it runs `dmsetup create` directly. If
that fails because the pool does not hold the id, the row reads
`RES_STATUS_ERROR` with the dmsetup output in `details`, the worker records
`err_epoch`, and **no later converge sends a message either**: a created td
is never re-created by message, so pool-metadata loss surfaces as an
intervention event (Appendix D) instead of an empty volume under a live
`dev_id`. Standby cntlrs build no thin volumes and are unaffected.

**U4-S3 Uncreated snapshots: the pre-pass, simplified (R10).** For every td
with `ori_id != 0 && !created`, after the pool loop has filled `poolReady`
and before the thin loop (its position is unchanged):

1. *Claim set*: every slice with `poolReady[slice]` whose snapshot thin
   device is absent (`dm.Info == nil`; an `Info` error skips the slice, as
   `ensureThin` will fail it with the same error). The gate is pool
   readiness, never the plan-global `deferred` flag (unchanged from U1).
   **Removed:** the check that the origin's own thin device exists in the
   slice. The gateway guarantees the origin is materialized in every slice
   pool (U2), whether or not this CN has built the origin's device yet.
2. *Quiesce*: `suspendSnapOrigin` when the plan holds the origin
   (`plan.tdByDevId[ori_id]`) — it suspends the raid0 iff it is live and
   returns `""` otherwise, which already covers a fresh primary (no raid0
   built yet at pre-pass time), a deferred plan and a raid0 someone else
   holds suspended. **Removed:** the `origin == nil || origin.deferred`
   early return; an origin absent from the plan simply means nothing to
   quiesce. (Under U2 an origin can only leave `td_list` after every
   snapshot of it is created, so an uncreated snapshot without an origin in
   the plan is a control-plane bug; the agent still sends its messages,
   R11.)
3. *Messages*: `createSnapId` per claimed slice, unchanged — the nested
   per-slice origin-thin suspend is applied iff that device is live, and a
   failed message is logged and tolerated because the `dmsetup create` that
   follows decides the outcome (the pool may already hold the id after a
   crashed earlier pass).
4. *Resume* on every path out ([D12], unchanged).

**Removed code:** the `snapDone` map and its parameter on `ensureThin`; the
snapshot branch of `createThinId` (it sends only `create_thin`, and is
called only for `created == false && ori_id == 0`); `createSnapId` keeps
one caller, the pre-pass.

**U4-S4 Order independence.** Neither the thin loop nor the pre-pass depends
on the relative position of an origin and its snapshot in `td_list`, and
`buildTds` keeps request order with no sort. Same-pass creation of an origin
and its snapshot — the case cnagent.md CN14 currently orders around — is
not a state the gateway can produce.

**U4-S5 Violated precondition (R11).** A `create_snap` whose `ori_id` the
pool does not hold fails at the message; the `dmsetup create` fails; the
row reads `RES_STATUS_ERROR` with the dmsetup output; the td stays
`created == false`, so every converge retries. No detection, no distinct
status.

**U4-S6 Local store and the flip's arrival.** The persisted
`SyncupCntlrRequest` carries `created`. Between a td's materialization and
the flip's re-sync the stored copy says `false`; that is harmless (the
devices exist, so nothing messages) and is corrected by the bump's
higher-revision request (SH8: apply, then persist). An equal-revision
re-apply of a fully built cntlr still issues no mutating command (SH16).

**Tests (`agent/cnagent/snapshot_test.go`, `cnagent_test.go`; the
recorded-call fake).** The fake's thin pool already models ids
(`thinIds`, `create_thin`/`create_snap` fail on a held id, `delete`
removes one) and, as `TestDeclarativeCntlrTeardown` pins, a CN21 teardown
keeps them. Two fixture additions: `reqOpts.tds` entries carry `Created`,
and the fake's `dmsetup create` of a `thin` table rejects a `dev_id` the
pool does not hold (the real kernel does) — U4-T4 needs it.

Kept unchanged: `TestSnapshotQuiescesOriginRaid0`,
`TestSnapshotResumesRaid0AfterAFailedMessage`,
`TestSnapshotReapplySuspendsNothing`, `TestPlainThinNeverQuiescesARaid0`,
`TestSnapshotWithADeferredSliceStillMessagesTheReadyOne` (the scenario is no
longer reachable through the gateway, but the pool-readiness gate is still
the rule and the test pins it), `TestSnapshotClaimsASliceEvenWhenItsMessageFailed`
(reworded: the "exactly one `create_snap` per slice, none after the raid0
resume" property now holds because `ensureThin` never messages a snapshot
td; the count assertion stays, since every ordering assertion stops at its
first match).

Replaced: `TestFreshSnapshotMessagesAfterTheOriginsCreateThin` (it pins the
same-pass ordering that no longer exists) and
`TestSnapshotWithoutAnOriginStillMessages` (its orphan is now a created
snapshot, U4-T3).

* **U4-T1** `TestSnapshotMessagesWithoutTheOriginDevice` — the fresh-primary
  shape. Converge the origin alone at revision 2 (`create_thin 1`, devices
  live); tear the cntlr down through `SyncupCn` without the pointer (CN21:
  every device gone, `thinIds` keeps id 1); re-introduce the pointer and
  converge at revision 4 with `td_list = [origin{created: true},
  snapshot{ori_id: 1}]`. Assert: `create_snap 2 1` recorded exactly once
  per slice; **no** `dmsetup suspend` at all (neither raid0 nor origin thin
  is live); **no** `create_thin 1`; `dmsetup create` of every origin and
  snapshot thin device; every thin row `OK` for both tds. Run the same with
  `td_list` reversed (snapshot first) and assert the identical call
  multiset — `td_list` order is meaningless (U4-S4).
* **U4-T2** `TestCreatedTdIsNeverMessaged` — a plain td with `created:
  true` on a fresh cntlr whose fake pool holds its id: `dmsetup create` of
  the thin device, no `create_thin`, row `OK`; an equal-revision re-apply
  records no mutation. The CN9 order test (§6 test 5) is unchanged because
  its tds are `created = false`.
* **U4-T3** `TestCreatedSnapshotIsNeverMessaged` — origin and snapshot both
  `created: true` on a fresh cntlr (ids held): no `create_snap`, no
  `dmsetup suspend`, both tds' devices created, rows `OK`. Repeat with the
  origin absent from `td_list` (deleted after the snapshot's flip, U2):
  identical — nothing to message, nothing to quiesce.
* **U4-T4** `TestCreatedTdWithAMissingIdIsAnError` — a td with `created:
  true` whose id the fake pool does **not** hold: no message, the `dmsetup
  create` fails, the row reads `RES_STATUS_ERROR` carrying the fake's
  stderr; a second converge at the same revision sends no message either
  and the row stays `ERROR` (no self-heal, U4-S2).
* **U4-T5** `TestUncreatedSnapshotRetriesWhenTheOriginIdIsMissing` — the
  R11 case: `td_list = [snapshot{ori_id: 7}]`, no td with `dev_id` 7 in the
  pool: `create_snap 2 7` is sent (unquiesced), the fake rejects the create,
  the row reads `ERROR`; the next converge sends the message again.
* **U4-T6** `grep -rn "snapDone" agent/cnagent/` finds nothing; `grep -rn
  "createSnapId(" agent/cnagent/*.go` (non-test) hits its definition and
  the pre-pass call only.

---

## 6. U5 — Integration: case B rebuild stage (R14)

`integtest/cnagent_test.sh`, case B (`thinbm`), plan `cnagent_integtest.md`
§12. The driver takes the request as JSON, so `created` is one more
`req_td` field.

**U5-S1 Harness.** `req_td` gains an optional fourth argument `created`
(default `false`): `req_td tdid dev_id ori_id [created]`. A new helper
`assert_absent stream regex label` fails when `event_line` finds a match
(the negative twin of `assert_before`).

**U5-S2 Step 6 (snapshot) models the gateway.** The origin td `0x9` is sent
with `created = true` when the snapshot is added (the gateway would never
let the snapshot exist otherwise, U2). Every existing assertion of the stage
still holds: the origin's device is live, the snapshot is `created = false`,
so the pre-pass quiesces the raid0, suspends the origin thin, sends
`create_snap 2 1`, resumes, and creates the snapshot device after the resume.

**U5-S3 New step 9 — `rebuild`, after the host disconnects and before the
existing check/teardown.** Sequence:

1. `host_disconnect` both NQNs (moved up from the teardown stage; the
   teardown keeps them as harmless repeats or drops them).
2. `cn_drop "$cn"` — `SyncupCn` with an empty cntlr list: CN21 tears the
   cntlr down, sends **no** `delete` message, the ids stay in the pool
   metadata on DN1's legs.
3. `bump_cn_sync "$cn"`; `syncup-cn --cntlr "$sp:$cntlr"` re-introduces the
   pointer (as the case's "CN1 builds the primary stack" stage does).
4. `req_set "$req" '.td_list |= map(.created = true)'`; `bump_cn_rev`;
   `cn_syncup_cntlr` — the desired state the sp-worker would publish after
   both flips.
5. Assert on the stage's event stream: `assert_absent … "dmsetup message
   .* create_thin"`, `assert_absent … "dmsetup message .* create_snap"`,
   `assert_absent … "^dmsetup suspend"`; `assert_thin_ok` for `0x9` and
   `0xc`; `assert_map_ok` for the pool and the namespaces; and the two
   bitmaps re-read through `get-td-bm`: `0xc` ⇒ `1effffffffffffff`, `0x9`
   ⇒ `1efdffffffffffff` (the mappings of both tds survived the rebuild
   intact, so the bare `dmsetup create` attached the *existing* ids — no
   empty volume, no data loss).
6. `converge_check`, then the usual teardown and `assert_no_residue`.

The stage's revision bookkeeping follows §9 (`CNREV`/`CNSYNC`); the cntlr's
local file was deleted by step 2 (SH7), so step 4 is a fresh cntlr at a
higher revision and the CN8 gate is satisfied by step 3.

**U5-T1** Case B passes end to end on the two-VM lab, including the new
stage; `mutations()` over the rebuild syncup's trace contains `dmsetup
create` lines and, of `dmsetup message`, exactly the activation sweep's
`reserve_metadata_snap`/`release_metadata_snap` pair — no `create_thin`, no
`create_snap`, no `delete` (update_05.md U3: the rebuild re-creates the pool
device, a designed sweep trigger — the drop's teardown sent no deletes, so a
td removed while the cntlr was down is healed exactly here; the bitmap proof
below is what shows the bare creates attached the existing ids).

---

## 7. Residuals and known limits

* **A torn snapshot is still possible across a primary crash** (Appendix
  D, unchanged): a crash between two slices' `create_snap` messages leaves
  per-slice snapshots dated from different instants; the next primary sends
  the remaining messages (the td is still `created == false`), every row
  goes `OK`, and the flag flips. `created` certifies **materialization,
  not point-in-time consistency**; delete and re-create a snapshot whose
  creation raced a primary crash, as before.
* **Pool-metadata loss is an intervention event, not a self-healing one.**
  A created td whose id a pool no longer holds reads `RES_STATUS_ERROR` on
  every converge and is never re-created by message (U4-S2). This
  supersedes today's behaviour, which would silently hand the `dev_id` a
  fresh empty volume; it belongs next to the "no thin-metadata repair path"
  limit of Appendix D.
* **One more fan-out per td creation.** Every flip bumps `SpRev` (R5), so
  creating a td costs two full fan-outs of the SP instead of one; per-reply
  batching (R6) keeps bulk creation at one extra fan-out per reply. Appendix
  D's "revision granularity is the SP" entry covers the cost model.
* **A snapshot request between creation and flip is refused, not queued.**
  Clients retry after `ListThinDevices` shows `created == true` (R13). At a
  pool-suppressing `sp_level` the refusal lasts until the level is restored
  (U2-S5).

---

## 8. Amendments to companion documents (apply at implementation time)

Each bullet is the exact edit; the companion document records it in its own
amendments section, citing `ThinDeviceCreated.md U*n*`.

### `pb/schema.proto`

* U1 — `message ThinDevice` gains `bool created = 5;` with the U1-S1
  comment; `make gen`; commit the regenerated `pb/schema.pb.go`.

### `architecture.md`

* §2 terminology, the **thin device (td)** row — append: "`created` marks
  a td whose thin volume the primary has reported `OK` in every slice
  (§10.3); only a created td can be snapshotted, and an origin cannot be
  deleted while a snapshot of it is uncreated (§8.7)."
* §3.3 step 4 — after "created with pool message `create_thin {dev_id}` or
  `create_snap {dev_id} {ori_id}`", add: "— sent only while
  `ThinDevice.created == false`; a created td's volume is attached with a
  bare `dmsetup create` (`cnagent.md` CN14, `ThinDeviceCreated.md` U4)".
* §8.7 **CreateThinDevice** — Errors: append "`FAILED_PRECONDITION`
  `ori_name` set and the origin's `created == false` (checked after
  `NOT_FOUND`)". Action: "write `ThinDevice` (`created = false`)". New
  paragraph after the point-in-time paragraph, titled **Materialization
  (`ThinDeviceCreated.md` U2/U3)**: the U2-S1/U2-S3/U2-S4/U2-S5 text — the
  flag's meaning, the poll-`ListThinDevices` contract, what is and is not
  gated, and the `sp_level`/provisioning interaction.
* §8.7 **DeleteThinDevice** — Errors: append "`FAILED_PRECONDITION` if any
  td of the SP has `ori_id == this td's dev_id` and `created == false`
  (read `td_name_list` + every `ThinDevice` in the same STM — the
  `ListThinDevices` read set, bounded by `MaxTdCntPerSp`)". Keep "Deleting
  the origin of snapshots is allowed — dm-thin snapshots stay valid" and
  qualify it with "once every snapshot of it is created".
* §8.7 **ListThinDevices** — append "`name_to_td` carries `created`; it is
  the client's wait primitive before snapshotting".
* §9.5 — after the `td_id_to_thin_info[td].slice_id_to_dm_thin` sentence,
  add "(the sp-worker's `created` flip reads exactly these rows, §10.3)".
* §10.3 — new paragraph after the **Provisioning gate** paragraph, titled
  **Materialization flip (`ThinDeviceCreated.md` U3)**: the U3-S1 predicate
  (four clauses, no revision comparison), U3-S2 candidates, U3-S3 one STM
  per reply with the `td_id` re-read guard and the single `SpRev` bump,
  the no-loop argument, the allowance to fold several tds and a pending
  `provisioned` flip into one STM, and U3-S5.
* Appendix C — new bullet: "`ThinDeviceCreated.md` U1-U5 — `ThinDevice`
  gains `created`; §8.7 gates snapshot creation and origin deletion on it;
  §10.3 gains the materialization flip; §3.3 and `cnagent.md` CN14 record
  that a created td is never messaged and that the snapshot pre-pass owns
  every snapshot message; Appendix D updated. The §8/§10 items specify
  gateway and worker behaviour that is not implemented yet — this document
  is their spec."
* Appendix D — the **Snapshot creation is not atomic across a primary
  crash** bullet gains: "`ThinDevice.created` certifies materialization,
  not point-in-time consistency: a torn snapshot still flips to `created`
  once every slice's volume exists." New bullet **A created td is never
  re-created by message** with the §7 second-bullet text, cross-referencing
  the "no thin-metadata repair path" bullet.

### `cnagent.md`

* CN14 — rewrite the three paragraphs that follow the first one (the
  cross-slice point-in-time rule, the "claims a slice only when the
  origin's own thin volume exists" paragraph, and the residual) with U4-S1
  through U4-S5: the three-case table, "a created td is never messaged",
  the simplified pre-pass (claim set = pool-ready slices with an absent
  snapshot device; quiesce iff the origin raid0 is live; per-slice
  origin-thin suspend iff live; no origin-device filter; no `snapDone`; no
  lazy snapshot path), order independence, and the R11 failure mode. Keep
  the U1 quiesce mechanics and the [D12] resume-on-every-path sentence
  verbatim.
* CN21 — after "(no `delete` messages — CN14: this is deactivation, the
  metadata on the legs is the next CN's to find)", add "; a created td's
  volumes are re-attached there without any message
  (`ThinDeviceCreated.md` U4)".
* §5 — new bullet: "`architecture.md` §3.3/§8.7/§10.3
  (`ThinDeviceCreated.md` U1-U4) — the `created` flag and its gateway and
  worker rules; this document's CN14 is the agent-side spec."
* §6 test 5 — "`create_thin` messages" becomes "`create_thin` messages (for
  `created = false` tds only)".
* §6 test 19 — rewrite: keep the raid0-bracket, failed-message resume,
  equal-revision re-apply, plain-td and deferred-slice sentences; replace
  the "creates the origin **and** its snapshot in one pass" sentence with
  the U4-T1 fresh-primary shape (no suspend, `create_snap` per slice, no
  `create_thin` for the created origin, identical results with `td_list`
  reversed); replace "A snapshot whose origin td is absent from the plan
  … still sends the messages" with U4-T3; keep the "exactly one
  `create_snap` per slice, none after the resume" sentence with its new
  reason (`ensureThin` never messages a snapshot td).
* §6 new tests 20-23 — U4-T2, U4-T3, U4-T4, U4-T5 as written above.
* §7 — new item: "`grep -rn "snapDone" agent/cnagent/` finds nothing, and
  `grep -rn "dmsetup message" agent/cnagent/pool.go` hits only
  `create_thin`, `create_snap` and `delete` sites that are gated on
  `created == false` (U4-T6)."

### `cnagent_integtest.md`

* §12 — step 6 states that `0x9` is sent with `created = true`; new step 9
  (U5-S3), the former step 9 becomes 10.
* §18 — the `SyncupCntlr` row gains "created-td rebuild with zero pool
  messages (B)"; the `GetThinDeviceBm` row gains "the B rebuild mapping
  proof".
* §20 — new bullet: "**U5 (`ThinDeviceCreated.md`)** — `req_td` gained the
  optional `created` argument, `assert_absent` was added, case B models the
  gateway's `created` on the origin at the snapshot step and gained the
  rebuild step 9."

### `layout.md`, `dnagent.md`, `log.md`, `osclient.md`, `grpc.md`

* No change: no new file, package, dependency, log record or interceptor.

---

## 9. Acceptance checklist

1. `make gen` is clean and `go build ./...`, `go vet ./...`, `go test
   ./...` pass; `pb/schema.proto` carries `bool created = 5` inside
   `message ThinDevice` (U1-T1).
2. `agent/cnagent` has no `snapDone`; `createSnapId` has exactly one
   caller; `ensureThin` sends a message only when `!created && ori_id ==
   0` (U4-T6).
3. The six retained snapshot tests still pass unmodified in their
   assertions; U4-T1 through U4-T5 exist and pass.
4. `integtest/cnagent_test.sh` case B passes with the rebuild step, and the
   rebuild syncup's `mutations()` output contains, of `dmsetup message`,
   exactly the activation sweep's `reserve_metadata_snap`/
   `release_metadata_snap` pair — no `create_thin`, no `create_snap`, no
   `delete` (U5-T1 as amended by update_05.md U3).
5. The companion documents carry the §8 amendments, each citing
   `ThinDeviceCreated.md U*n*`. `gateway/` and `worker/` have since landed
   with U2/U3 implemented, and gateway.md §9 / dnv-worker.md §13 — their
   unit-test inventories of record — re-scoped the U2-T/U3-T lists: most
   scenarios exist under different names, and the 2026-09-10 minor_issues
   MT11 sweep added dedicated tests for three that had been structural-only
   — U2-T5's same-name-recreate sequence
   (`gateway/handler_vol_test.go` `TestThinDeviceSameNameRecreateTakesFreshIds`)
   and U3-T5/T6's check-round-to-flip paths (`worker/sprole_test.go`
   `TestSpCreatedFlipFromACheckRound` /
   `TestSpCreatedFlipIgnoresTheReplyRevision`). Only U2-T6's injected
   mid-STM race remains covered structurally (GW8/EU4's serializable STM):
   `etcdutil.Client.run` has no seam to inject it through, and the outcome
   is unreachable anyway — a real concurrent create bumps `SpRev`, so GW6's
   token check refuses the retry first (minor_issues.md MT11).

---

## 10. Implementation notes — where this document was wrong

Recorded at implementation time. Each item is a place where following §2-§9
literally would have produced a broken or self-contradictory tree; the
companion documents carry the corrected text.

**N1 — the fake needed three fixture changes, not two (§5).** §5 says the
recorded-call fake "already models ids … and, as `TestDeclarativeCntlrTeardown`
pins, a CN21 teardown keeps them". It does not: `dmsetup remove` deleted the
pool's `fakeDm` and with it its `thinIds`, so U4-T1's rebuild had nothing to
re-attach. Thin ids are now node-level state keyed by the pool's dm name
(`fakeNode.thinPools`), which a `fakeDm` aliases — the model the real system
has, where the metadata lives on the DN legs and outlives the dm device. A
`holdThinIds` seeder was added with it, because U4-T2/U4-T4 need "a fresh
cntlr whose fake pool holds (or does not hold) the id" and no other path
reaches that state.

**N2 — `create_snap` must reject an origin the pool does not hold.** With only
the two fixture changes §5 lists, U4-T5 cannot reach its `RES_STATUS_ERROR`
row: the fake would accept `create_snap 2 7`, register id 2, and let the
`dmsetup create` succeed. The fake now models the kernel's own rule, which is
also what makes R11/U4-S5 observable at all.

**N3 — one retained test's assertion had to be inverted, and §9 item 3 reads
five, not six.** `TestSnapshotWithADeferredSliceStillMessagesTheReadyOne`
asserted that no origin raid0 is suspended. U4-S3 removes the
`origin == nil || origin.deferred` early return and claims
`suspendSnapOrigin` "already covers … a deferred plan" — but in that fixture
the origin's raid0 was built by the preceding revision and is still live, so
the pre-pass now quiesces it, which is the U1 rule applied honestly. The test
asserts the suspend/resume bracket instead. `TestSnapshotReapplySuspendsNothing`
kept its assertions verbatim but moved to the two-pass build shape, because its
one-pass fixture (an uncreated origin and an uncreated snapshot together) is
exactly the state U4-S4 says the gateway cannot produce. `snapTds()` now marks
the origin `created`, and `TestThinDeviceBitmapSharedSubtree` — not in either
§5 list — was corrected the same way.

**N4 — the `cnagent.md` §7 grep in §8 matches nothing and is wrong on
substance.** `grep -rn "dmsetup message" agent/cnagent/pool.go` hits one
comment; the call sites are `s.dm.Message(`. And `deleteThinId` is
deliberately *not* gated on `created` (R2: `delete {dev_id}` comes from the
td's own deletion — or from the U3 activation sweep when that fan-out's
message was gate-skipped — never from the flag). The acceptance item as
written into `cnagent.md` §7 uses the working grep and says so.

**N5 — §8 missed two sentences that stated the old unconditional rule.**
`architecture.md` §8.7 ("The primary creates one thin volume per slice
(`create_thin` / `create_snap` pool message)") and `cnagent.md` CN14's *first*
paragraph both had to be rewritten; §8 lists neither, and CN14 would have
contradicted its own new table.

**N7 — "identical call multiset" (U4-T1) is not literally checkable.** The
recorded-call fake hands out dm minor numbers in creation order, so running the
same `td_list` in both orders yields raid0 tables citing different ones. The
test and `cnagent.md` §6 test 19 assert what U4-S4 actually means: the same pool
messages, the same absence of quiesces, the same device creations and the same
rows.

**N8 — the "a deferred plan needs no quiesce" claim is wrong wherever it
appears.** U4-S3 step 2 says `suspendSnapOrigin` returning `""` "already covers
… a deferred plan". It does not: `deferred` suppresses the raid0's *converge*,
not the device, so a raid0 an earlier revision built stays live and is quiesced.
That is the behaviour N3's test change pins. `cnagent.md` CN14 carried the same
claim in a paragraph §8 said to keep verbatim; it and the two code comments that
repeated it now state the liveness rule instead.

**N6 — U5-T1 was unsatisfiable and U5-S3 needs two stages.** `mutations()`
took only a log path with no trace filter, so over case B's whole log it
necessarily contains step 6's `create_snap`; it gained an optional leading
`trace_id` argument (§9 of `cnagent_integtest.md` updated with it). And the
rebuild cannot be one stage: `cn_drop` parks the ns-devs onto dm-error with a
`dmsetup reload` — a suspend — which would defeat the stage's own
`assert_absent … "^dmsetup suspend"`. The teardown and the re-introduction of
the pointer are a `drop` stage; the converge and its assertions are the
`rebuild` stage, with its own trace id.
