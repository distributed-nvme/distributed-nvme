# Crash-Safe Recovery for dm-raid1 with an External Witness

**Status:** Design + TLA+-verified
**Scope:** Linux `dm-raid` raid1 (2 legs), driven directly through `dmsetup` (no mdadm, no LVM)
**Audience:** Implementer / reviewer / verifier with no prior context

---

## 1. The problem we are solving

### 1.1 The deployment

A fleet of Linux servers each runs many raid1 arrays built with plain `dmsetup`:

- Each array has two physical backings, **A** and **B**.
- Each backing is split with `dm-linear` into a metadata slice and a data slice
  (A → `meta_A` + `data_A`, B → `meta_B` + `data_B`).
- The four slices are joined by the `dm-raid1` target. The metadata slices
  carry the dm-raid superblock and the write-intent bitmap.
- A journaled filesystem sits on top; on I/O error it remounts read-only or
  panics.

### 1.2 The failure we prevent

After a dirty reboot (power loss, kernel panic) the operator must decide which
legs to hand to `dmsetup create`. The dangerous mistake is assembling a
**stale leg**: a disk that was ejected from the array while the other leg kept
running solo and kept acknowledging writes. Assembling the stale leg silently
rolls back those acknowledged writes — a failure mode a single local disk can
never produce. The journal cannot help: the stale leg's journal is internally
consistent; it is consistently *old*.

A subtler variant: treating a **half-rebuilt** leg as a data source. A leg that
was resyncing at crash time is not merely stale, it is internally inconsistent
(new writes mirrored onto it, interleaved with an arbitrary partial copy of the
old state).

### 1.3 Correctness target

We compare against a single local disk. The single-disk baseline supplies:
atomic single-sector writes, a journaling filesystem, read-only/panic on I/O
error, and correct flush/FUA handling. The goal:

> If a single local disk avoids corruption in a given crash scenario, the
> raid1 array under this policy avoids it in the same scenario. The policy may
> refuse to assemble (unavailability), but it must never serve rolled-back or
> internally inconsistent data.

### 1.4 Kernel properties the design depends on

These are claims about the dm-raid kernel code, not about this design. They
must be validated empirically per kernel version (§9).

| # | Property | Consequence |
|---|----------|-------------|
| K1 | raid1 acknowledges a write only after it completes on every currently in-sync leg | If both legs were in-sync at crash, each leg individually holds all acked writes. If degraded, only the survivor does. |
| K2 | dm-raid failures are sticky: a failed leg stays failed until an explicit table reload/recreate | Spontaneous transitions only go healthy→unhealthy. Every improvement passes through the control plane, so it can be write-ahead-logged. |
| K3 | On ejecting a leg, the kernel bumps the surviving leg's superblock `events` counter and sets the ejected leg's bit in `failed_devices` before acknowledging further writes | The surviving superblock is a crash-consistent, kernel-maintained witness of "I ran solo." |
| K4 | After a crash with both legs in-sync, assembly of both triggers a bitmap resync of dirty regions only, arbitrated by the superblocks | In-flight divergence between legs is reconciled; the higher-`events` leg is the copy source. |
| K5 | Newer kernels refuse to eject the last remaining leg; they fail I/O upward instead | "Both legs marked failed by the kernel" is rare; the array simply goes dead with the last survivor still nominally in-sync. The design handles both behaviors. |

---

## 2. Architecture

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

1. **One compact record per device** in etcd, overwritten in place. No history.
2. **Write only on transitions** — never periodic samples, never sync progress.
3. **Ack = etcd quorum commit**, not "receiver received". A global increasing
   counter (`seq`) is written with every record; the receiver/etcd
   transaction rejects any write whose `seq` is not greater than the stored
   one, so duplicated or reordered gRPC messages cannot regress state.
4. **No improvement without quorum.** If etcd is unreachable, the array may
   run degraded or stay down, but no re-add/create/single-leg-start executes.
   Failure records may be delayed (safe direction, see rule T3 in §4).

Why one record suffices: the entire recovery decision reduces to a single
derived fact — *the last writable leg set* ("who was in-sync the last time
writes could be acknowledged"). While both legs are down no writes are acked,
so both-down periods contribute nothing; and by K2 any improvement passes
through the control plane, which updates the record. Maintaining this fact
transitionally (the `survivor` field below, with a *carry* rule) replaces
walking back through historical records.

---

## 3. The etcd record

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

---

## 4. The three timing rules

Correctness lives in *when* records are written relative to actions:

- **T1 — Write-before (WAL):** any transition that starts improving a stale
  leg, or deliberately stales a readable one, is committed to etcd with
  quorum ack *before* the `dmsetup` command runs. Applies to: fresh create,
  re-add, administrative single-leg start.
- **T2 — Write-after-verify:** `syncing → insync` is recorded only after
  `dmsetup status` shows all health chars `A` AND sync ratio complete AND
  sync action `idle` — never on "`dmsetup` returned 0". Recording it on
  command success is a real corruption bug: if the sync source then dies and
  the machine crashes, the record would name a half-rebuilt leg as survivor.
- **T3 — Write-lazy:** kernel-detected failures are recorded as soon as the
  event arrives, but correctness never depends on beating a crash: a late
  failure record makes recovery conservative (or is overridden by superblock
  evidence, §6), never wrong — with one narrow exception quantified in §8-R1.

---

## 5. Transitions

| # | Condition | Record written (order matters) |
|---|-----------|-------------------------------|
| 1 | Fresh create (treat as a re-add from seed leg A) | T1-WAL: `op={create,target:B,auth:[A]}`, `A:failed,B:failed`, `survivor:[A]` → create with B in rebuild slot → after status verifies array up: `A:insync, B:syncing` (op stays) → on completion, T2 promote: `A,B:insync`, `survivor:[A,B]`, `op:null` |
| 2 | B fails while both in-sync | T3: `B:failed`, `survivor:[A]` |
| 3 | A fails while sole survivor (array dead) | T3: `A:failed`, `survivor` stays `[A]` (carry) |
| 4 | Rebuild target B fails mid-sync | `B:failed`, `op:null` (aborted), `survivor` unchanged `[A]` |
| 5 | Sync source A fails while B is syncing | Write both `failed`, `survivor:[A]` (carry), `op:null`. B is recorded `failed` regardless of what the kernel briefly reports — half-synced is not a member |
| 6 | Re-add of failed A (directional) | T1-WAL `op={readd,target:A,auth:[B]}`, quorum ack, only then act. Try both legs (A in rebuild slot): verified up → `A:syncing` (op stays); sync completes → T2 promote. Both-leg attempt fails → rebuild with B only: success → `A:failed, B:insync, survivor:[B], op:null`. That fails too → both `failed`, `survivor:[B]` (carry), `op:null` |
| 7 | Administrative single-leg start (recovery-time availability choice, §6) | T1-WAL the demotion — peer `failed`, `survivor:[chosen]` — quorum-acked before `dmsetup create`; serve writes only after the ack |
| 8 | Agent boot reconciliation | Diff dm reality vs record. Demotions written directly (T3); improvements only via T1/T2 |

---

## 6. Recovery procedure (dirty reboot)

Preliminaries, always:

1. Identity + superblock probe of both meta areas: device WWN must match
   `legs.X.dev`; superblock magic/`num_devices`/`array_position` must be sane.
   Unreadable, blank, or foreign → classify that leg `failed` for this
   decision. This guards against swapped disks winning arbitration.
2. Record-write discipline: any decision that changes effective membership
   writes the record first (demotions, T1) or after verification (promotions,
   T2), before serving writes.

Then branch on the record. The data-source choice is a function of `survivor`
+ superblocks + availability only; `legs` states and `op` affect only the
redo/cleanup path.

**Case `survivor:[A,B]`, `op:null`** — both legs were in-sync at last record.

- Both readable → assemble both. K4's bitmap resync reconciles in-flight
  divergence; both superblocks' `failed_devices` bits are cleared by the
  completed resync.
- Exactly one readable (say A) → read A's superblock:
  - `failed_devices` marks B → A provably ran solo after the last record
    (K3). Write the row-7 demotion, assemble A alone.
  - A's superblock is clean and B is unreadable → the irreducible ambiguity:
    "B died at the crash instant" vs. "A was ejected first, B ran solo, and
    the proof died with B". *Strict mode:* refuse. *Availability mode:*
    execute row 7 with eyes open; the rollback exposure is bounded by the
    event-propagation latency since the last record (§8-R2).
- Neither readable → unavailable; record untouched.

**Case `survivor:[X]`** (covers `peer:failed`, `peer:syncing`, and
both-`failed` with carried survivor):

- X readable, and X's intact peer (if any) does not carry a `failed_devices`
  bit marking X → assemble X alone. If `op` is present, idempotent redo is
  licensed: assemble X with the target back in the rebuild slot, or assemble
  X alone and re-add later.
- X readable, but X's intact peer carries a `failed_devices` bit marking X
  → refuse. An intact peer's K3 witness is fresh evidence that X was ejected
  and the peer ran solo; the record's `survivor:[X]` is stale. Demote X;
  let the peer assemble alone (or go unavailable).
- X unreadable → unavailable, no matter how healthy the peer looks. A
  `failed` peer is stale by construction; a `syncing` peer is internally
  inconsistent — strictly worse. Leave the peer untouched as material for
  manual disaster recovery.

**Case record missing/corrupt** — superblock-only arbitration: equal `events`
on both → assemble both (or either alone, per K1); strictly higher `events` +
peer marked in `failed_devices` → that leg alone; anything else → refuse.

---

## 7. What this design guarantees

- No rollback of acknowledged writes by any automatic assembly decision, under
  axioms K1–K4 and the timing rules T1–T3, except the two quantified residuals
  in §8 (R1, R2 — both are record-staleness windows, both require a disk to be
  physically destroyed, and R2 only bites in availability mode).
- A `syncing` leg is never a data source, so half-rebuilt inconsistency is
  never served.
- O(1) etcd state per device — one compact record; the historical-scan policy
  is subsumed by the `survivor` carry rule.
- Crash anywhere is recoverable: every `dmsetup`-mutating step is bracketed by
  a WAL (`op`) or is idempotent, and every recovery branch is defined.
- Refusal (unavailability) is always an allowed outcome; the design trades
  availability for integrity at every ambiguous point unless availability mode
  is explicitly chosen.

## 8. Residual risks the design cannot close

- **R1 — Lazy-failure window + survivor destruction.** Writes acked by a solo
  survivor whose T3 failure-record hadn't committed yet, followed by physical
  destruction of that disk plus a crash: the record still names the stale peer
  in `survivor`, and the disproving superblock (K3) died with the disk.
  Recovery then serves the stale peer as if current. Information-theoretically
  unclosable by any external witness (the evidence is gone); window =
  event→quorum latency, typically sub-second.
- **R2 — Both-in-sync record, one leg unreadable, clean surviving
  superblock.** Strict mode converts this to unavailability; availability mode
  accepts a rollback window bounded by record latency.
- **R3 — In-flight divergence semantics.** Until the post-crash bitmap resync
  covers a region, the two legs may hold different versions of an in-flight
  sector and reads may return either copy. Journal replay tolerates this in
  practice (replay decides once, then rewrites), but it is technically weaker
  than the single-disk baseline. Unavoidable in raid1 without a write-journal
  target.
- **R4 — Silent bit rot.** raid1 cannot arbitrate a mismatch found by scrub;
  "repair" copies one leg over the other arbitrarily. Mitigation: stack
  `dm-integrity` under each leg.
- **R5 — Witness loss.** If etcd data is lost, recovery degrades to
  superblock-only arbitration: correct where superblocks are decisive,
  refusals where they aren't.
- **R6 — Axiom risk.** K1–K5 are kernel claims; they must be validated per
  kernel version (§9); the formal proof is conditional on them.

---

## 9. Implementation notes

### 9.1 Agent event loop

The agent reads `dmsetup info` for the event counter, then `dmsetup wait`
until the counter advances. On wake it re-reads `dmsetup status`, diffs
against the cached state (always re-read, never assume), and emits transition
records per §5 respecting T1/T2/T3. Per-device serialization: one goroutine
per device; all etcd writes for a device go through it in order, stamped with
the next `seq`.

### 9.2 dmsetup command patterns

Table format: `start len raid raid1 <#params> <params...> <#devs> <meta data>...`
where the first raid param is chunk size (unused by raid1 → `0`).

Fresh create / re-add with rebuild direction forced (B in rebuild slot):

```bash
dmsetup create r0 --table "0 209715200 raid raid1 5 0 region_size 8192 rebuild 1 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"
```

Assemble both after a crash with `survivor:[A,B]` (kernel arbitrates via
superblocks + bitmap):

```bash
dmsetup create r0 --table "0 209715200 raid raid1 3 0 region_size 8192 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"
```

Degraded, A only (`-` = absent device):

```bash
dmsetup create r0 --table "0 209715200 raid raid1 3 0 region_size 8192 2 \
  /dev/mapper/meta_A /dev/mapper/data_A - -"
```

Re-add B into a RUNNING degraded array (preferred: FS stays mounted):

```bash
dmsetup suspend r0
dmsetup reload  r0 --table "0 209715200 raid raid1 5 0 region_size 8192 rebuild 1 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"
dmsetup resume  r0
```

Rules: always force rebuild direction explicitly (`rebuild <idx>`, and/or wipe
the target's meta area) — never rely on "create returned 0" implying the kernel
picked your intended source; verify from status that the intended leg is the
one rebuilding. Prefer suspend/reload/resume over teardown+create for re-adds.

### 9.3 Status parsing and the promotion predicate

```
# dmsetup status r0
0 209715200 raid raid1 2 AA 209715200/209715200 idle 0 0 -
                       │  │  │                   └ sync_action
                       │  │  └ sync ratio cur/total
                       │  └ health chars: A=in-sync, a=alive+rebuilding, D=dead
                       └ #devices
```

```python
def insync_verified(st):                       # T2 gate — all three required
    return (all(c == 'A' for c in st.health)
            and st.cur == st.total
            and st.action == "idle")
```

### 9.4 Superblock probe

The dm-raid superblock sits at offset 0 of each meta device, little-endian
(`struct dm_raid_superblock`, kernel `drivers/md/dm-raid.c` — pin the layout
against your kernel source and cover it in tests):

| Offset | Field | Use here |
|--------|-------|----------|
| 0 | `magic` (le32) = `0x64526D44` | sanity |
| 8 | `num_devices` (le32) | sanity (== 2) |
| 12 | `array_position` (le32) | sanity (matches slot) |
| 16 | `events` (le64) | arbitration: higher = fresher |
| 24 | `failed_devices` (le64 bitmask) | K3 witness: "I saw the peer fail" |

Identity (which physical disk is which) is not in the superblock — bind it via
the WWNs stored in the etcd record and `/dev/disk/by-id`. Probe outcome ∈
{sane, blank, foreign/garbage, unreadable}; anything but *sane* → `failed` for
the §6 decision. A completed bitmap resync (K4) clears `failed_devices` on
both superblocks.

### 9.5 etcd access rules

Single-key-per-device; every write is a transaction:
`if stored.seq < new.seq then put(new)` (the global counter provides `seq`).
Improvement paths (T1) block on quorum ack before any `dmsetup` executes; T3
failure paths fire-and-retry in the background. Sync progress is never written.

### 9.6 Boot reconciliation

On agent start: read record, probe dm + disks, diff. Reality worse than record
→ write demotions (T3). Reality "better" → never trust it blindly; route
through §6 (superblock evidence) and T1/T2. Then enter the event loop.

### 9.7 Recovery decision (pseudocode)

```python
def recover(rec, policy):                     # policy: STRICT | AVAIL
    if rec is None or not valid(rec):
        return superblock_only_arbitration()  # §6 last case
    avail = {L for L in ("A", "B")
             if wwn_matches(L, rec) and sb_probe(L) == SANE}
    S = set(rec.survivor)

    if S == {"A", "B"}:
        if S <= avail:
            return assemble(["A", "B"])                       # K4 resync
        if len(S & avail) == 1:
            x = (S & avail).pop()
            if sb(x).failed_devices_marks(peer(x)):
                wal_demote(peer(x), survivor=[x])             # row 7, T1
                return assemble([x])
            if policy == AVAIL:
                wal_demote(peer(x), survivor=[x])             # accepts R2
                return assemble([x])
        return UNAVAILABLE

    # |S| == 1: sole survivor (incl. carried survivor, incl. op present)
    x = S.pop()
    if x in avail:
        if peer(x) in avail and sb(peer(x)).failed_devices_marks(x):
            return UNAVAILABLE                # stale-record tripwire
        write_confirmation_if_changed(x)      # e.g. after both-failed carry
        return assemble([x])                  # op ⇒ may redo rebuild of peer
    return UNAVAILABLE                        # never auto-serve the peer
```

### 9.8 Testing (validating the axioms)

- Crash injection: `dm-log-writes` under each leg (or `dm-flakey` + replay) to
  cut power at arbitrary write boundaries; after each cut, run recovery and
  `fsck`/checksum-verify against the acked-write log. This validates K1/K3/K4
  on your kernel.
- Targeted scenarios: crash while degraded; crash during resync (both
  directions: target death, source death); crash between `dmsetup` success and
  the T2 promote; re-add of a previously-failed leg (K2 stickiness, and whether
  your kernel requires `rebuild` explicitly); disk-swap (foreign superblock
  must lose); K5 behavior of last-leg failure.
- Fault drills: etcd unreachable during each transition class; gRPC
  duplication/reorder (must be absorbed by `seq` CAS); agent kill between WAL
  and `dmsetup` (op redo path).

---

## 10. Formal verification with TLA+

### 10.1 Modeling approach

We abstract data as version counters: `acked` = highest write version the
application saw acknowledged; `ver[d]` = durable version on leg *d*. etcd is
one atomic variable (its linearizability is assumed, not re-proven). Volatile
kernel state (`running`, `inSync`, `rebuild`) is wiped by `Crash`; `ver`,
superblocks, `intact`, and the record survive. Axioms K1/K3 become the
*structure* of actions — which is exactly why they must be validated
empirically: the proof is conditional on them.

The single checked safety property:

> `NoStaleServe == running => ∀ d ∈ inSync : ver[d] = acked`
> (an assembled, serving array never contains a leg missing acked writes)

### 10.2 The spec

```tla
---------------------------- MODULE DmRaid1 ----------------------------
EXTENDS Naturals, FiniteSets
CONSTANT Policy                       \* "strict" | "avail"
Disks == {"A","B"}
Peer(d) == CHOOSE e \in Disks : e # d

VARIABLES running, inSync, rebuild,   \* volatile
          acked, ver, intact,         \* durable: spec bookkeeping, data, media
          sbEv, sbPF,                 \* durable: superblock events / peer-failed
          surv                        \* durable: etcd record (atomic)
vars == <<running,inSync,rebuild,acked,ver,intact,sbEv,sbPF,surv>>

Init == /\ running = TRUE /\ inSync = Disks /\ rebuild = {}
        /\ acked = 0 /\ ver = [d \in Disks |-> 0] /\ intact = Disks
        /\ sbEv = [d \in Disks |-> 0] /\ sbPF = [d \in Disks |-> FALSE]
        /\ surv = Disks

WriteAck ==                                          \* axiom K1
  /\ running /\ inSync # {}
  /\ acked' = acked + 1
  /\ ver' = [d \in Disks |-> IF d \in inSync THEN acked+1 ELSE ver[d]]
  /\ UNCHANGED <<running,inSync,rebuild,intact,sbEv,sbPF,surv>>

Eject(d) ==                                          \* axioms K2/K3/K5
  /\ running /\ d \in inSync /\ inSync # {d}         \* never the last leg
  /\ inSync' = inSync \ {d}
  /\ sbEv' = [sbEv EXCEPT ![Peer(d)] = @ + 1]        \* K3: before next ack
  /\ sbPF' = [sbPF EXCEPT ![Peer(d)] = TRUE]
  /\ surv' = inSync'                                 \* S0 idealization (§10.3)
  /\ UNCHANGED <<running,rebuild,acked,ver,intact>>

Crash   == running /\ running' = FALSE /\ inSync' = {} /\ rebuild' = {}
           /\ UNCHANGED <<acked,ver,intact,sbEv,sbPF,surv>>
Destroy(d) == ~running /\ intact' = intact \ {d}
              /\ UNCHANGED <<running,inSync,rebuild,acked,ver,sbEv,sbPF,surv>>

StartRebuild(d) ==                                   \* T1: surv already excludes d
  /\ running /\ d \notin inSync /\ d \in intact /\ d \notin rebuild
  /\ rebuild' = rebuild \cup {d} /\ UNCHANGED <<running,inSync,acked,ver,intact,sbEv,sbPF,surv>>
CompleteRebuild(d) ==
  /\ running /\ d \in rebuild
  /\ inSync' = inSync \cup {d} /\ rebuild' = rebuild \ {d}
  /\ ver' = [ver EXCEPT ![d] = acked]
  /\ sbPF' = [e \in Disks |-> FALSE] /\ UNCHANGED <<running,acked,intact,sbEv,surv>>
RecordPromote ==                                     \* T2 lag modeled explicitly
  /\ running /\ rebuild = {} /\ surv # inSync
  /\ surv' = inSync /\ UNCHANGED <<running,inSync,rebuild,acked,ver,intact,sbEv,sbPF>>

RecoverBoth ==
  /\ ~running /\ surv = Disks /\ Disks \subseteq intact
  /\ running' = TRUE /\ inSync' = Disks /\ rebuild' = {}
  /\ ver' = [d \in Disks |-> acked]                  \* K4 bitmap resync
  /\ sbPF' = [e \in Disks |-> FALSE]                \* resync clears failed_devices
  /\ UNCHANGED <<acked,intact,sbEv,surv>>
RecoverSolo(d) ==
  /\ ~running /\ d \in intact
  \* §6 tripwire: refuse when an intact peer's failed_devices marks d.
  \*   The asymmetric witness is sbPF[Peer(d)] (CompleteRebuild clears it,
  \*   so a TRUE bit is fresh post-last-resync evidence that d was ejected
  \*   and the peer ran solo). sbEv counters bump symmetrically across a
  \*   re-add cycle and cannot carry the asymmetry.
  /\ ~(Peer(d) \in intact /\ sbPF[Peer(d)])
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Peer(d) \notin intact
        /\ (sbPF[d] \/ Policy = "avail")             \* §6 ambiguity branch
  /\ running' = TRUE /\ inSync' = {d} /\ rebuild' = {}
  /\ surv' = {d}                                     \* row-7 demotion before serving
  /\ sbEv' = [sbEv EXCEPT ![d] = @ + 1] /\ sbPF' = [sbPF EXCEPT ![d] = TRUE]
  /\ UNCHANGED <<acked,ver,intact>>

Idle == UNCHANGED vars

Next == WriteAck \/ Crash \/ RecoverBoth \/ RecordPromote \/ Idle
        \/ \E d \in Disks : Eject(d) \/ Destroy(d) \/ StartRebuild(d)
                            \/ CompleteRebuild(d) \/ RecoverSolo(d)
Spec == Init /\ [][Next]_vars

NoStaleServe == running => \A d \in inSync : ver[d] = acked
SurvFresh    == \A d \in surv : (d \in inSync \/ ~running) => ver[d] = acked

StateBound == acked <= 3 /\ sbEv["A"] <= 4 /\ sbEv["B"] <= 4
=======================================================================
```

TLC configuration (one per `Policy` value):

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

Bounds: 2 disks, ≤3 writes, ≤2 crash/recover cycles (the `StateBound`
constraint). Exhaustive in well under a second per run.

### 10.3 The S0/S1 methodology — what "verified" means here

- **S0 (idealized):** as written above — the etcd record update is atomic
  with `Eject`. Expected result: `NoStaleServe` holds for **both** policies.
  This proves the decision logic (survivor semantics, carry, syncing
  exclusion, T1/T2 ordering, superblock arbitration, the §6 tripwire) has no
  holes — including that the deliberately-modeled T2 promote lag
  (`RecordPromote` as a separate action) is harmless.

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
documented residual list §8 enumerates — is the strongest claim this design
makes: the policy has no undocumented rollback path within the model.

### 10.4 Verified results

The spec and configs live in `tla/`; `tla/run.sh` reproduces all 14 checks
(pass=14 fail=0) end-to-end in a few seconds on a single core.

**S0 — both invariants hold for both policies:**

| Config | Result | Distinct states |
|--------|--------|-----------------|
| `DmRaid1_S0_strict.cfg` | No error | 4000 |
| `DmRaid1_S0_avail.cfg`  | No error | 4100 |

**S1 — `NoStaleServe` is violated on both policies; the counterexample set
is exactly {R1, R2}:**

Counterexample isolation uses a classified variant (`DmRaid1_S1c.tla`) with a
`RecoverMode` constant selecting which sub-branch of `RecoverSolo`'s ambiguity
case is enabled, plus a `nodestroy` mode disabling `Destroy` to prove every
counterexample truly requires destruction of the true survivor (matching
§8-R1/R2).

| Policy / Mode | Violated? | Residual |
|---------------|-----------|----------|
| strict / full     | yes | R1 |
| strict / noavail  | yes | R1 (R1 doesn't use the `Policy="avail"` sub-branch) |
| strict / nosbpf   | yes | R1 |
| strict / noambig   | yes | R1 (reaches `RecoverSolo` via `surv={d}`, not the ambiguity branch) |
| strict / nodestroy | **no** | — proves every strict CE requires `Destroy` (i.e. is R1) |
| avail / full     | yes | R2 (shortest) + R1 |
| avail / noavail  | yes | R1 |
| avail / nosbpf   | yes | R2 (only the `Policy="avail"` sub-branch) |
| avail / noambig   | yes | R1 |
| avail / nodestroy | **no** | — proves every avail CE requires `Destroy` (i.e. is R1 or R2) |

Interpretation (matching §10.3):

- Every strict-mode counterexample is R1 (lazy-failure window + destruction of
  the true survivor). `strict/nodestroy` passes, so no third residual exists.
- Every avail-mode counterexample is R1 or R2. `avail/nodestroy` passes, so
  no third residual exists. R2 is reachable only under `Policy = "avail"`;
  R1 is reachable in avail exactly as in strict.
- "Strict keeps `NoStaleServe` in every trace not involving R1" holds:
  `strict/nodestroy` (which closes R1 by removing `Destroy`) passes.

### 10.5 Limits of the proof

TLC is bounded model checking, not a deductive proof (TLAPS is possible but
heavy; at this state-space size TLC is the pragmatic choice). The result is
conditional on axioms K1–K5, which are kernel claims — hence §9.8. The model
idealizes etcd as linearizable and omits Byzantine faults (a disk lying about
flush completion breaks the single-disk baseline equally). The state bound is
small but covers the patterns needed to exhibit R1 and R2; larger bounds do
not change the residual classification.

---

## 11. Operator assumptions

1. Single-disk baseline holds per leg: atomic sector writes, journaling FS,
   read-only/panic on I/O error.
2. All devices in the stack honor flush/FUA and pass it through dm intact.
3. `meta_X` and `data_X` live on the same physical disk (true by
   construction here) — a leg's superblock witness dies with its data.
4. Every improvement (create / re-add / single-leg start) is quorum-WAL'd
   before `dmsetup` runs; no repair without etcd. Degraded operation without
   etcd is allowed.
5. `syncing → insync` is promoted only via the §9.3 predicate.
6. Disk identity is bound by WWN in the record and checked before assembly.
7. Kernel axioms K1–K5 validated by crash injection on the deployed kernel
   version; re-validated on kernel upgrades.
8. Strict vs. availability mode for the §6/R2 ambiguity is an explicit,
   documented per-fleet choice.
9. Optional but recommended: `dm-integrity` under each leg (closes R4);
   bounded audit log of records with timestamps, kept outside the decision
   path.