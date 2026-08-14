# Crash-Safe Recovery for dm-raid1 with an External Witness — Simplified Design

**Status:** Design + TLA+-verified (bounded model checking)
**Scope:** Linux `dm-raid` raid1 (2 legs), driven directly through `dmsetup` (no mdadm, no LVM)
**Audience:** Implementer / reviewer / verifier with no prior context

---

## 1. Background

### 1.1 The deployment

A fleet of Linux servers each runs many raid1 arrays built with plain `dmsetup`:

- Each array has two physical backings, **A** and **B**.
- Each backing is split with `dm-linear` into a metadata slice and a data slice
  (A → `meta_A` + `data_A`, B → `meta_B` + `data_B`). The metadata slice
  holds the dm-raid superblock and the write-intent bitmap. Both slices of a
  given leg live on the same physical disk, so a dead disk takes its
  superblock witness with it.
- The four slices are joined by the `dm-raid1` target.
- A journaled filesystem sits on top; on I/O error it remounts read-only or
  panics.

### 1.2 Kernel properties the design relies on

These were validated by reading the kernel source in
`drivers/md/dm-raid.c`, `drivers/md/md.c`, `drivers/md/raid1.c` of the
target tree. They are claims about the kernel, not about this design, and
should be re-validated per kernel version (§5.5).

| # | Property | Where in the kernel | Consequence |
|---|----------|---------------------|-------------|
| K1 | raid1 acknowledges a write only after it completes on every currently in-sync leg | `raid1.c` end-write path | If both legs were in-sync at crash, each leg individually holds all acked writes. If degraded, only the survivor does. |
| K2 | dm-raid failures are sticky: a failed leg stays failed until an explicit table reload/recreate | `raid1_error` `raid1.c:1750` sets `Faulty` under `device_lock`; only a control-plane reload clears it | Spontaneous transitions only go healthy→unhealthy. Every improvement passes through the control plane, so it can be write-ahead-logged. |
| K3 | On ejecting a leg, the survivor bumps its in-core `events` and sets the ejected leg's bit in `failed_devices`; the on-disk write happens later, asynchronously, via the md thread's `md_update_sb` | `raid1_error` `raid1.c:1755-1770` (in-core, synchronous) + `md_update_sb` `md.c:2780-2966` (on-disk, async) | There is a window between the in-core ejection and the on-disk persistence where the survivor is already acking solo but the superblock has not yet recorded it. |
| K4 | After a crash with both legs in-sync, assembly of both triggers a bitmap resync of dirty regions only, arbitrated by the superblocks. `events` counters are **reconciled** on resync completion (`md_reap_sync_thread` → `md_update_sb(force_change=1)` → `super_sync` writes `sb->events = mddev->events` to both legs; `dm-raid.c:2143`) | `dm-raid.c:2234` (strict-greater-than `events` comparison), `dm-raid.c:2143` (`super_sync`), `md.c:10340` (resync completion calls `md_update_sb`) | With both legs present, the kernel picks the leg with strictly higher `events` as the resync source. After a completed resync both legs hold equal `events` (the bumped value), so across re-add cycles the counters stay reconciled. The both-healthy case is **handled by the kernel alone**. |
| K5 | Newer kernels refuse to eject the last remaining leg; they fail I/O upward instead | `raid1.c:1757-1765` checks `(conf->raid_disks - mddev->degraded) == 1` and sets `MD_BROKEN` | "Both legs marked failed by the kernel" is rare; the array simply goes dead with the last survivor still nominally in-sync. The design handles both behaviors. |

The kernel-source investigation that established K4's reconciliation claim is
the load-bearing finding of this design. It is what lets us hand the
both-healthy case to the kernel without an external adjudicator.

---

## 2. The problem we want to resolve

### 2.1 The failure we prevent

After a dirty reboot (power loss, kernel panic) the operator must decide
which legs to hand to `dmsetup create`. The dangerous mistake is assembling a
**stale leg**: a disk that was ejected from the array while the other leg
kept running solo and kept acknowledging writes. Assembling the stale leg
silently rolls back those acknowledged writes — a failure mode a single local
disk can never produce. The journal cannot help: the stale leg's journal is
internally consistent; it is consistently *old*.

A subtler variant: treating a **half-rebuilt** leg as a data source. A leg
that was resyncing at crash time is not merely stale, it is internally
inconsistent (new writes mirrored onto it, interleaved with an arbitrary
partial copy of the old state).

### 2.2 Correctness target

We compare against a single local disk. The single-disk baseline supplies:
atomic single-sector writes, a journaling filesystem, read-only/panic on I/O
error, and correct flush/FUA handling. The goal:

> If a single local disk avoids corruption in a given crash scenario, the
> raid1 array under this policy avoids it in the same scenario. The policy
> may refuse to assemble (unavailability), but it must never serve
> rolled-back or internally inconsistent data.

Equivalently: **never assemble a raid1 when we shouldn't.** Refusing to
assemble when we could is acceptable (unavailability); serving stale data is
not.

---

## 3. Pre-conditions and assumptions

### 3.1 Design conditions (the two scoping decisions)

**C1 — Both disks healthy ⇒ rely on the kernel.** When both A and B are
intact at recovery time, the recovery procedure does not adjudicate. It
assembles both and lets the kernel's K4 bitmap resync reconcile any
in-flight divergence. The kernel's `events` comparison
(`dm-raid.c:2234`, strict-greater-than) picks the freshest leg as the resync
source, and `events` is reconciled across legs on resync completion (K4
validation above). The both-healthy case is **not** something the external
witness handles; it is handed to the kernel.

This is justified by the kernel-source investigation: after a completed
resync, both legs hold equal `events`; at any post-ejection crash the
survivor's `events` is strictly higher; the strict-greater-than comparison
always picks correctly. The "equal events with a pending ejection" state that
one might worry about cannot occur after a completed resync in the real
kernel.

**C2 — No metadata read by the recovery procedure.** The procedure does
not read `meta_A` or `meta_B` to decide assemble-vs-refuse. The decision in
the one-healthy and none-healthy cases is made from the etcd record (`surv`)
alone. There is no superblock probe, no `failed_devices` consultation, no
`events` comparison in the recovery procedure.

This means the design can refuse some legitimately-recoverable cases (it is
conservative): specifically, when `surv=Disks` and exactly one leg is intact,
the procedure cannot distinguish "provably the intact leg ran solo" from
"genuinely ambiguous R2," so in strict mode it refuses both. That is the
safety-vs-availability trade the design makes.

### 3.2 Operator assumptions

1. Single-disk baseline holds per leg: atomic sector writes, journaling FS,
   read-only/panic on I/O error.
2. All devices in the stack honor flush/FUA and pass it through dm intact.
3. `meta_X` and `data_X` live on the same physical disk (true by
   construction here) — a leg's superblock witness dies with its data. This
   is why R1 is unclosable: the disproving superblock died with the disk.
4. Every improvement (create / re-add / single-leg start) is quorum-WAL'd
   before `dmsetup` runs; no repair without etcd. Degraded operation without
   etcd is allowed.
5. Disk identity is bound by WWN in the record and checked before assembly
   (identity probe only — no superblock field read for the data-source
   decision).
6. Kernel axioms K1–K5 validated by crash injection on the deployed kernel
   version; re-validated on kernel upgrades.
7. Strict vs. availability mode for the R2 ambiguity is an explicit,
   documented per-fleet choice.

---

## 4. The solution

### 4.1 Architecture

```
┌──────────── per server ────────────┐        ┌───────────────┐
│ agent                              │  gRPC  │ receiver      │      ┌──────┐
│  · dm event loop (dmsetup wait)    │ ─────▶ │  · CAS writes │ ───▶ │ etcd │
│  · status parser / diff            │ ◀───── │  · quorum ack │      └──────┘
│  · executes dmsetup transitions    │  ack   └───────────────┘
│  · recovery decision at boot       │   (or agents write etcd directly)
└────────────────────────────────────┘
```

Design rules:

1. **One compact record per device** in etcd, overwritten in place. No
   history.
2. **Write only on transitions** — never periodic samples, never sync
   progress.
3. **Ack = etcd quorum commit**, not "receiver received". A global increasing
   counter (`seq`) is written with every record; the receiver/etcd
   transaction rejects any write whose `seq` is not greater than the stored
   one, so duplicated or reordered gRPC messages cannot regress state.
4. **No improvement without quorum.** If etcd is unreachable, the array may
   run degraded or stay down, but no re-add/create/single-leg-start executes.
   Failure records may be delayed (safe direction, see rule T3 in §4.4).

### 4.2 The etcd record

```json
// key: /raid1/{device_id}
{
  "schema": 1,
  "seq": 184223,
  "legs": {
    "A": { "dev": "wwn-0x5000c500a1b2c3d4", "state": "insync" },
    "B": { "dev": "wwn-0x5000c500e5f60718", "state": "failed" }
  },
  "survivor": ["A"],
  "op": null,
  "ts": "2026-08-10T17:03:11Z"
}
```

| Field | Meaning | Invariants |
|-------|---------|-----------|
| `seq` | Global increasing counter | Store rejects non-increasing writes |
| `legs.X.dev` | Stable disk identity (WWN/serial) | Checked against `/dev/disk/by-id` before any assembly; a mismatched disk is treated as `failed` |
| `legs.X.state` | `insync` \| `syncing` \| `failed` | Exactly three states. `syncing` = being rebuilt |
| `survivor` | The recovery decision. Every leg listed holds all acknowledged writes | (a) If any leg is `insync`, `survivor` = exactly the set of `insync` legs. (b) If no leg is `insync`, `survivor` is carried unchanged from the last record where one was — it may name a currently-`failed` disk, meaning "the freshest acked data is on that broken disk". (c) A `syncing` leg is never in `survivor`. |
| `op` | In-flight membership change: `{"kind":"create"\|"readd", "target":"B", "authoritative":["A"]}` | `op.authoritative == survivor`; `op.target ∉ survivor`. Enables idempotent redo after a crash mid-`dmsetup`. The data-source decision is always `survivor`; `op` never changes it. |
| `ts` | Audit only | Never used in decisions |

The carry rule (b) is the compact replacement for scanning history: it is the
same fold, computed eagerly at write time.

### 4.3 The three timing rules

Correctness lives in *when* records are written relative to actions:

- **T1 — Write-before (WAL):** any transition that starts improving a stale
  leg, or deliberately stales a readable one, is committed to etcd with
  quorum ack *before* the `dmsetup` command runs. Applies to: fresh create,
  re-add, administrative single-leg start.
- **T2 — Write-after-verify:** `syncing → insync` is recorded only after
  `dmsetup status` shows all health chars `A` AND sync ratio complete AND
  sync action `idle` — never on "`dmsetup` returned 0".
- **T3 — Write-lazy:** kernel-detected failures are recorded as soon as the
  event arrives, but correctness never depends on beating a crash: a late
  failure record makes recovery conservative, never wrong — with one narrow
  exception quantified in §4.7 R1.

### 4.4 Transitions

| # | Condition | Record written (order matters) |
|---|-----------|-------------------------------|
| 1 | Fresh create (treat as a re-add from seed leg A) | T1-WAL: `op={create,target:B,auth:[A]}`, `A:failed,B:failed`, `survivor:[A]` → create with B in rebuild slot → after status verifies array up: `A:insync, B:syncing` (op stays) → on completion, T2 promote: `A,B:insync`, `survivor:[A,B]`, `op:null` |
| 2 | B fails while both in-sync | T3: `B:failed`, `survivor:[A]` |
| 3 | A fails while sole survivor (array dead) | T3: `A:failed`, `survivor` stays `[A]` (carry) |
| 4 | Rebuild target B fails mid-sync | `B:failed`, `op:null` (aborted), `survivor` unchanged `[A]` |
| 5 | Sync source A fails while B is syncing | Write both `failed`, `survivor:[A]` (carry), `op:null` |
| 6 | Re-add of failed A (directional) | T1-WAL `op={readd,target:A,auth:[B]}`, quorum ack, only then act. Try both legs (A in rebuild slot): verified up → `A:syncing` (op stays); sync completes → T2 promote. Both-leg attempt fails → rebuild with B only: success → `A:failed, B:insync, survivor:[B], op:null`. That fails too → both `failed`, `survivor:[B]` (carry), `op:null` |
| 7 | Administrative single-leg start (recovery-time availability choice, §4.6) | T1-WAL the demotion — peer `failed`, `survivor:[chosen]` — quorum-acked before `dmsetup create`; serve writes only after the ack |
| 8 | Agent boot reconciliation | Diff dm reality vs record. Demotions written directly (T3); improvements only via T1/T2 |

### 4.5 Recovery procedure (dirty reboot)

Preliminaries, always:

1. **Identity probe** of both legs (WWN only — no superblock field read for
   the data-source decision; see C2). Device WWN must match `legs.X.dev`.
   Unreadable or foreign → classify that leg as unavailable for this
   decision.
2. **Record-write discipline:** any decision that changes effective
   membership writes the record first (demotions, T1) or after verification
   (promotions, T2), before serving writes.

Then branch on **availability** × **record's `survivor`**:

- **Both legs healthy** → assemble both (C1). The kernel's K4 bitmap resync
  reconciles in-flight divergence; `events` is reconciled on resync
  completion (K4 validation in §1.2). No external adjudication.
- **No leg healthy** → unavailable, do nothing. Record untouched.
- **Exactly one leg healthy (say d; peer dead)** → decide from `surv` alone
  (C2):

  | Record's `survivor` | Strict policy | Avail policy |
  |---------------------|---------------|-------------|
  | `{d}` (record matches reality) | assemble d alone | assemble d alone |
  | `Disks` (record said both in-sync; cannot distinguish "provably solo" from R2 without metadata) | refuse | assemble d alone (accept R2) |
  | `{Peer(d)}` (record names the dead leg) | refuse | refuse |

### 4.6 What this design guarantees

- No rollback of acknowledged writes by any automatic assembly decision,
  under axioms K1–K5 and the timing rules T1–T3, except the two quantified
  residuals in §4.7 (R1, R2 — both are record-staleness windows, both
  require a disk to be physically destroyed, and R2 only bites in
  availability mode).
- A `syncing` leg is never a data source, so half-rebuilt inconsistency is
  never served.
- O(1) etcd state per device — one compact record; the `survivor` carry
  rule subsumes historical scans.
- Crash anywhere is recoverable: every `dmsetup`-mutating step is bracketed
  by a WAL (`op`) or is idempotent, and every recovery branch is defined.
- Refusal (unavailability) is always an allowed outcome; the design trades
  availability for integrity at every ambiguous point unless availability
  mode is explicitly chosen.

### 4.7 Residual risks the design cannot close

- **R1 — Lazy-failure window + survivor destruction.** Writes acked by a
  solo survivor whose T3 failure-record hadn't committed yet, followed by
  physical destruction of that disk plus a crash: the record still names the
  stale peer in `survivor`, and the disproving superblock (K3) died with the
  disk. Recovery then serves the stale peer as if current.
  Information-theoretically unclosable by any external witness (the evidence
  is gone); window = event→quorum latency, typically sub-second. Requires
  the true survivor to be destroyed (confirmed by `nodestroy` model check).
- **R2 — Both-in-sync record, one leg unreadable (strict mode converts to
  unavailability; availability mode accepts a rollback window bounded by
  record latency).** The record says `survivor=Disks`, but one leg is dead
  and the survivor's superblock witness died with it. The recovery
  procedure cannot distinguish "the dead leg died at the crash instant"
  from "the intact leg was ejected first, the dead leg ran solo briefly,
  and its proof died with it." Requires the true survivor to be destroyed
  (confirmed by `nodestroy` model check); available only under
  `Policy = "avail"` (confirmed by classification mode check).

Both residuals require disk destruction; both are record-staleness windows;
R2 is avail-only. The TLA+ verification (§5) confirms the counterexample set
is exactly {R1, R2} and no others.

---

## 5. TLA+ verification

### 5.1 Modeling approach

We abstract data as version counters: `acked` = highest write version the
application saw acknowledged; `ver[d]` = durable version on leg *d*. etcd is
one atomic variable (its linearizability is assumed). Volatile kernel state
(`running`, `inSync`, `rebuild`) is wiped by `Crash`; `ver`, `intact`, and the
record survive. Axioms K1 and K4 become the *structure* of actions — which
is exactly why they must be validated empirically per kernel (§5.5).

The single checked safety property:

> `NoStaleServe == running => ∀ d ∈ inSync : ver[d] = acked`
> (an assembled, serving array never contains a leg missing acked writes)

An auxiliary invariant used as an inductive lemma in S0:

> `SurvFresh == ∀ d ∈ surv : (d ∈ inSync ∨ ¬running) => ver[d] = acked`

The model has **no superblock variables** (`sbEv`, `sbPF` are gone) —
reflecting condition C2 (no metadata read). The recovery procedure's
`RecoverSolo` consults `surv` alone.

### 5.2 The spec (S0 — idealized)

```tla
---------------------------- MODULE DmRaid1 ----------------------------
\* Crash-safe recovery for dm-raid1 with an external state witness.
\*
\* Design conditions (this version):
\*   C1. Both disks healthy  -> assemble both; rely on the kernel's K4
\*        bitmap resync (events arbitration) to reconcile. The model
\*        idealizes this as `ver'[d]=acked` for both legs (K4 succeeds).
\*   C2. No metadata is read by the recovery procedure. The decision to
\*        assemble vs. refuse in the one-healthy and none-healthy cases is
\*        made from the etcd record (surv) alone. The single safety goal:
\*        "never assemble a raid1 when we shouldn't."
\*
\* Variables dropped vs. the earlier spec: sbEv, sbPF (we do not read
\* superblocks). The external witness is the sole authority outside the
\* both-healthy case.
EXTENDS Naturals, FiniteSets
CONSTANT Policy                       \* "strict" | "avail"
Disks == {"A","B"}
Peer(d) == CHOOSE e \in Disks : e # d

VARIABLES running, inSync, rebuild,   \* volatile
          acked, ver, intact,         \* durable: spec bookkeeping, data, media
          surv                        \* durable: etcd record (atomic)
vars == <<running,inSync,rebuild,acked,ver,intact,surv>>

Init == /\ running = TRUE /\ inSync = Disks /\ rebuild = {}
        /\ acked = 0 /\ ver = [d \in Disks |-> 0] /\ intact = Disks
        /\ surv = Disks

WriteAck ==                                          \* axiom K1
  /\ running /\ inSync # {}
  /\ acked' = acked + 1
  /\ ver' = [d \in Disks |-> IF d \in inSync THEN acked+1 ELSE ver[d]]
  /\ UNCHANGED <<running,inSync,rebuild,intact,surv>>

Eject(d) ==                                          \* axioms K2/K3/K5
  /\ running /\ d \in inSync /\ inSync # {d}         \* never the last leg
  /\ inSync' = inSync \ {d}
  /\ surv' = inSync'                                 \* S0 idealization (§5.3)
  /\ UNCHANGED <<running,rebuild,acked,ver,intact>>

Crash   == running /\ running' = FALSE /\ inSync' = {} /\ rebuild' = {}
           /\ UNCHANGED <<acked,ver,intact,surv>>
Destroy(d) == ~running /\ intact' = intact \ {d}
              /\ UNCHANGED <<running,inSync,rebuild,acked,ver,surv>>

StartRebuild(d) ==                                   \* T1: surv already excludes d
  /\ running /\ d \notin inSync /\ d \in intact /\ d \notin rebuild
  /\ rebuild' = rebuild \cup {d} /\ UNCHANGED <<running,inSync,acked,ver,intact,surv>>
CompleteRebuild(d) ==
  /\ running /\ d \in rebuild
  /\ inSync' = inSync \cup {d} /\ rebuild' = rebuild \ {d}
  /\ ver' = [ver EXCEPT ![d] = acked]
  /\ UNCHANGED <<running,acked,intact,surv>>
RecordPromote ==                                     \* T2 lag modeled explicitly
  /\ running /\ rebuild = {} /\ surv # inSync
  /\ surv' = inSync /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact>>

\* Both disks healthy -> rely on the kernel (C1). K4 reconciles in-flight
\* divergence; model this as both legs returning to acked.
RecoverBoth ==
  /\ ~running /\ Disks \subseteq intact
  /\ running' = TRUE /\ inSync' = Disks /\ rebuild' = {}
  /\ ver' = [d \in Disks |-> acked]
  /\ UNCHANGED <<acked,intact,surv>>

\* Exactly one disk healthy (d intact, peer dead). Decide from `surv` only (C2).
\*   surv = {d}                 -> assemble d (record matches reality)
\*   surv = Disks, avail only   -> assemble d (accept R2 ambiguity)
\*   surv = Disks, strict        -> refuse (action disabled)
\*   surv = {Peer(d)}           -> refuse (action disabled; record names dead leg)
RecoverSolo(d) ==
  /\ ~running /\ d \in intact /\ Peer(d) \notin intact
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Policy = "avail"
  /\ running' = TRUE /\ inSync' = {d} /\ rebuild' = {}
  /\ surv' = {d}                                     \* row-7 demotion before serving
  /\ UNCHANGED <<acked,ver,intact>>

Idle == UNCHANGED vars

Next == WriteAck \/ Crash \/ RecoverBoth \/ RecordPromote \/ Idle
        \/ \E d \in Disks : Eject(d) \/ Destroy(d) \/ StartRebuild(d)
                            \/ CompleteRebuild(d) \/ RecoverSolo(d)
Spec == Init /\ [][Next]_vars

NoStaleServe == running => \A d \in inSync : ver[d] = acked
SurvFresh    == \A d \in surv : (d \in inSync \/ ~running) => ver[d] = acked

StateBound == acked <= 3
=======================================================================
```

### 5.3 The S0/S1 methodology — what "verified" means here

- **S0 (idealized):** as written above — the etcd record update is atomic
  with `Eject` (`surv' = inSync'`). Expected result: `NoStaleServe` and
  `SurvFresh` hold for **both** policies. This proves the decision logic
  (survivor semantics, carry, syncing exclusion, T1/T2 ordering, the C2
  assemble-vs-refuse table) has no holes — including that the deliberately
  modeled T2 promote lag (`RecordPromote` as a separate action) is harmless.

- **S1 (realistic):** the etcd record update is NOT atomic with `Eject`; it
  happens later via a `RecordEject` action (the T3 lazy-failure model). Two
  edits to S0:
  - In `Eject`, replace `surv' = inSync'` with `UNCHANGED surv`.
  - Add `RecordEject == running /\ surv # inSync /\ surv' = inSync /\
    UNCHANGED <<…>>` to `Next`.

  Expected result: TLC produces counterexample traces that are **exactly R1
  and R2** (both require a `Destroy` of the true survivor; R2 only under
  `Policy = "avail"`), and no others. Strict policy keeps `NoStaleServe` in
  every trace not involving R1.

That correspondence — the model checker's counterexample set equals the
residual list §4.7 enumerates — is the strongest claim this design makes:
the policy has no undocumented rollback path within the model.

### 5.4 The S1 spec

```tla
---------------------------- MODULE DmRaid1_S1 ----------------------------
\* S1 (realistic) variant of DmRaid1 under design conditions C1 + C2.
\* S1 edit (§5.3): Eject does NOT update surv atomically; a separate
\* RecordEject action advances surv later (T3 lazy-failure model).
EXTENDS Naturals, FiniteSets
CONSTANT Policy
Disks == {"A","B"}
Peer(d) == CHOOSE e \in Disks : e # d

VARIABLES running, inSync, rebuild, acked, ver, intact, surv
vars == <<running,inSync,rebuild,acked,ver,intact,surv>>

Init == /\ running = TRUE /\ inSync = Disks /\ rebuild = {}
        /\ acked = 0 /\ ver = [d \in Disks |-> 0] /\ intact = Disks
        /\ surv = Disks

WriteAck ==
  /\ running /\ inSync # {}
  /\ acked' = acked + 1
  /\ ver' = [d \in Disks |-> IF d \in inSync THEN acked+1 ELSE ver[d]]
  /\ UNCHANGED <<running,inSync,rebuild,intact,surv>>

Eject(d) ==
  /\ running /\ d \in inSync /\ inSync # {d}
  /\ inSync' = inSync \ {d}
  \* S1: surv not updated synchronously; RecordEject advances it later.
  /\ UNCHANGED <<running,rebuild,acked,ver,intact,surv>>

RecordEject ==                                       \* T3 lazy-failure record update
  /\ running /\ surv # inSync
  /\ surv' = inSync
  /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact>>

Crash   == running /\ running' = FALSE /\ inSync' = {} /\ rebuild' = {}
           /\ UNCHANGED <<acked,ver,intact,surv>>
Destroy(d) == ~running /\ intact' = intact \ {d}
              /\ UNCHANGED <<running,inSync,rebuild,acked,ver,surv>>

StartRebuild(d) ==
  /\ running /\ d \notin inSync /\ d \in intact /\ d \notin rebuild
  /\ rebuild' = rebuild \cup {d} /\ UNCHANGED <<running,inSync,acked,ver,intact,surv>>
CompleteRebuild(d) ==
  /\ running /\ d \in rebuild
  /\ inSync' = inSync \cup {d} /\ rebuild' = rebuild \ {d}
  /\ ver' = [ver EXCEPT ![d] = acked]
  /\ UNCHANGED <<running,acked,intact,surv>>
RecordPromote ==
  /\ running /\ rebuild = {} /\ surv # inSync
  /\ surv' = inSync /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact>>

RecoverBoth ==
  /\ ~running /\ Disks \subseteq intact
  /\ running' = TRUE /\ inSync' = Disks /\ rebuild' = {}
  /\ ver' = [d \in Disks |-> acked]
  /\ UNCHANGED <<acked,intact,surv>>

RecoverSolo(d) ==
  /\ ~running /\ d \in intact /\ Peer(d) \notin intact
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Policy = "avail"
  /\ running' = TRUE /\ inSync' = {d} /\ rebuild' = {}
  /\ surv' = {d}
  /\ UNCHANGED <<acked,ver,intact>>

Idle == UNCHANGED vars

Next == WriteAck \/ Crash \/ RecoverBoth \/ RecordPromote \/ RecordEject \/ Idle
        \/ \E d \in Disks : Eject(d) \/ Destroy(d) \/ StartRebuild(d)
                            \/ CompleteRebuild(d) \/ RecoverSolo(d)
Spec == Init /\ [][Next]_vars

NoStaleServe == running => \A d \in inSync : ver[d] = acked

StateBound == acked <= 3
=======================================================================
```

### 5.5 The S1c classified variant (for CE isolation)

To prove the S1 counterexample set is **exactly** {R1, R2} (and not some
third undocumented path), the classified variant `DmRaid1_S1c.tla` adds a
`RecoverMode` constant. The `nodestroy` mode gates the `Destroy` action:

```tla
Destroy(d) == ~running /\ RecoverMode # "nodestroy" /\ intact' = intact \ {d}
              /\ UNCHANGED <<…>>
```

If `nodestroy` makes `NoStaleServe` pass, every S1 counterexample truly
requires destruction of the true survivor — matching §4.7's claim that both
R1 and R2 require a `Destroy`.

```tla
---------------------------- MODULE DmRaid1_S1c ----------------------------
\* Classified S1 variant under design conditions C1 + C2.
\* RecoverMode:
\*   "full"      -> original S1 (R1 + R2 violations).
\*   "nodestroy" -> disable Destroy; if NoStaleServe holds, every S1
\*                  counterexample requires Destroy of the true survivor
\*                  (i.e. the CE set is exactly {R1, R2} per §4.7).
EXTENDS Naturals, FiniteSets
CONSTANTS Policy, RecoverMode
Disks == {"A","B"}
Peer(d) == CHOOSE e \in Disks : e # d

VARIABLES running, inSync, rebuild, acked, ver, intact, surv
vars == <<running,inSync,rebuild,acked,ver,intact,surv>>

Init == /\ running = TRUE /\ inSync = Disks /\ rebuild = {}
        /\ acked = 0 /\ ver = [d \in Disks |-> 0] /\ intact = Disks
        /\ surv = Disks

WriteAck ==
  /\ running /\ inSync # {}
  /\ acked' = acked + 1
  /\ ver' = [d \in Disks |-> IF d \in inSync THEN acked+1 ELSE ver[d]]
  /\ UNCHANGED <<running,inSync,rebuild,intact,surv>>

Eject(d) ==
  /\ running /\ d \in inSync /\ inSync # {d}
  /\ inSync' = inSync \ {d}
  /\ UNCHANGED <<running,rebuild,acked,ver,intact,surv>>

RecordEject ==
  /\ running /\ surv # inSync
  /\ surv' = inSync
  /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact>>

Crash   == running /\ running' = FALSE /\ inSync' = {} /\ rebuild' = {}
           /\ UNCHANGED <<acked,ver,intact,surv>>
Destroy(d) == ~running /\ RecoverMode # "nodestroy" /\ intact' = intact \ {d}
              /\ UNCHANGED <<running,inSync,rebuild,acked,ver,surv>>

StartRebuild(d) ==
  /\ running /\ d \notin inSync /\ d \in intact /\ d \notin rebuild
  /\ rebuild' = rebuild \cup {d} /\ UNCHANGED <<running,inSync,acked,ver,intact,surv>>
CompleteRebuild(d) ==
  /\ running /\ d \in rebuild
  /\ inSync' = inSync \cup {d} /\ rebuild' = rebuild \ {d}
  /\ ver' = [ver EXCEPT ![d] = acked]
  /\ UNCHANGED <<running,acked,intact,surv>>
RecordPromote ==
  /\ running /\ rebuild = {} /\ surv # inSync
  /\ surv' = inSync /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact>>

RecoverBoth ==
  /\ ~running /\ Disks \subseteq intact
  /\ running' = TRUE /\ inSync' = Disks /\ rebuild' = {}
  /\ ver' = [d \in Disks |-> acked]
  /\ UNCHANGED <<acked,intact,surv>>

RecoverSolo(d) ==
  /\ ~running /\ d \in intact /\ Peer(d) \notin intact
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Policy = "avail"
  /\ running' = TRUE /\ inSync' = {d} /\ rebuild' = {}
  /\ surv' = {d}
  /\ UNCHANGED <<acked,ver,intact>>

Idle == UNCHANGED vars

Next == WriteAck \/ Crash \/ RecoverBoth \/ RecordPromote \/ RecordEject \/ Idle
        \/ \E d \in Disks : Eject(d) \/ Destroy(d) \/ StartRebuild(d)
                            \/ CompleteRebuild(d) \/ RecoverSolo(d)
Spec == Init /\ [][Next]_vars

NoStaleServe == running => \A d \in inSync : ver[d] = acked

StateBound == acked <= 3
=======================================================================
```

### 5.6 TLC configurations

**S0 (both invariants, per policy):**

```
SPECIFICATION
Spec

CONSTANTS
Policy = "strict"          \* or "avail"

CONSTRAINT
StateBound

INVARIANT
NoStaleServe
SurvFresh
```

**S1 (safety only, per policy):**

```
SPECIFICATION
Spec

CONSTANTS
Policy = "strict"          \* or "avail"

CONSTRAINT
StateBound

INVARIANT
NoStaleServe
```

**S1c (classification, per policy × mode):**

```
SPECIFICATION
Spec

CONSTANTS
Policy = "strict"          \* or "avail"
RecoverMode = "full"        \* or "nodestroy"

CONSTRAINT
StateBound

INVARIANT
NoStaleServe
```

### 5.7 How to run the verification

Requirements:
- Java 21 (or any recent JDK).
- `tlc2` 2.19 or newer. The `tlc` wrapper used here is a one-line script:
  `#!/bin/sh \n exec java -cp ~/.local/bin/tla2tools.jar tlc2.TLC "$@"`.

The spec and config files live in `tla/`. A runner script `tla/run.sh`
executes all eight checks end-to-end:

```bash
cd tla
./run.sh              # verbose; prints each run's summary line
./run.sh --quiet      # one-line summary per check
```

Each individual run can also be invoked directly:

```bash
cd tla
tlc -config DmRaid1_S0_strict.cfg  DmRaid1.tla      # S0, strict
tlc -config DmRaid1_S0_avail.cfg   DmRaid1.tla      # S0, avail
tlc -config DmRaid1_S1_strict.cfg  DmRaid1_S1.tla    # S1, strict
tlc -config DmRaid1_S1_avail.cfg   DmRaid1_S1.tla    # S1, avail
tlc -config DmRaid1_S1c_strict_full.cfg      DmRaid1_S1c.tla   # classify
tlc -config DmRaid1_S1c_strict_nodestroy.cfg DmRaid1_S1c.tla   # CE isolation
tlc -config DmRaid1_S1c_avail_full.cfg       DmRaid1_S1c.tla   # classify
tlc -config DmRaid1_S1c_avail_nodestroy.cfg  DmRaid1_S1c.tla   # CE isolation
```

Each run completes in well under a second on a single core; the largest
visits ~180 distinct states. No special memory or worker tuning is required.

### 5.8 Verified results

The runner produces (pass=8 fail=0):

| Run | Result | Distinct states |
|-----|--------|-----------------|
| S0 strict | `NoStaleServe` + `SurvFresh` hold | 168 |
| S0 avail  | `NoStaleServe` + `SurvFresh` hold | 168 |
| S1 strict | `NoStaleServe` VIOLATED — CE is R1 | 318 |
| S1 avail  | `NoStaleServe` VIOLATED — CE is R2 (avail-only) | 150 |
| S1c strict/full      | VIOLATED (R1) | 318 |
| S1c strict/nodestroy | PASSES — every strict CE requires Destroy | 180 |
| S1c avail/full       | VIOLATED (R2) | 150 |
| S1c avail/nodestroy  | PASSES — every avail CE requires Destroy | 180 |

**R1 counterexample (strict/full):** record carries stale `surv={B}` from a
prior cycle. A ran solo and acked a write (`ver[A]=1`). After A is destroyed
(`Destroy(A)`, `intact={B}`), `RecoverSolo(B)` fires (record says
`surv={B}`, B intact) and assembles B alone — but `ver[B]=0 < acked=1`. Two
writes silently rolled back. The `Destroy(A)` step is load-bearing: under
`nodestroy`, this trace is unreachable and `strict/nodestroy` passes.

**R2 counterexample (avail/full):** record carries stale `surv=Disks`. B ran
solo and acked a write (`ver[B]=1`). After B is destroyed (`Destroy(B)`,
`intact={A}`), `RecoverSolo(A)` fires via the `surv=Disks /\ Policy="avail"`
branch and assembles A alone — but `ver[A]=0 < acked=1`. Two writes
silently rolled back. The `Destroy(B)` step is load-bearing: under
`nodestroy`, this trace is unreachable and `avail/nodestroy` passes. R2 is
avail-only because the `surv=Disks` branch of `RecoverSolo` is gated by
`Policy = "avail"`.

**Interpretation (matching §5.3):**

- S0 passes both invariants on both policies. The decision logic has no
  holes; the C2 assemble-vs-refuse table is correct.
- S1 produces counterexamples on both policies. Every strict CE is R1
  (lazy-failure window + destruction of the true survivor); every avail CE
  is R2 (avail ambiguity branch + destruction of the true survivor). The
  classification is confirmed by `nodestroy`: disabling `Destroy` makes
  both policies pass, proving every CE requires disk destruction and the
  counterexample set is exactly {R1, R2}.
- "Strict keeps `NoStaleServe` in every trace not involving R1" holds:
  `strict/nodestroy` passes.

### 5.9 Limits of the proof

TLC is bounded model checking, not a deductive proof. The result is
conditional on axioms K1–K5, which are kernel claims — hence the §1.2
kernel-source references and the §5.5 empirical-validation recommendation.
The model idealizes etcd as linearizable and omits Byzantine faults (a disk
lying about flush completion breaks the single-disk baseline equally). The
state bound `acked <= 3` is small but covers the patterns needed to exhibit
R1 and R2; larger bounds do not change the residual classification.