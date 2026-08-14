# Crash-Safe Recovery for dm-raid1 with an External State Witness

**Status:** Design + verification plan (self-contained)
**Scope:** Linux `dm-raid` raid1 only (2 legs), managed directly with `dmsetup` (no mdadm, no LVM)
**Audience:** An engineer or LLM implementing, reviewing, or formally verifying this system with no prior context

---

## 1. Problem statement

### 1.1 The setup

A fleet of Linux servers each hosts many raid1 devices built with plain `dmsetup`:

- Each raid1 device uses two physical disks, **A** and **B**.
- Each disk is split with `dm-linear` into a metadata part and a data part:
  disk A → `meta_A` + `data_A`, disk B → `meta_B` + `data_B`.
- The four pieces are aggregated with the `dm-raid` target (`raid1` type). The
  metadata areas hold the dm-raid **superblock** and the **write-intent bitmap**.
- A filesystem with a journal sits on top; the FS remounts read-only or panics
  on I/O error.

### 1.2 The failure mode

After a **dirty reboot** (power loss, kernel panic), the operator must decide
which legs to pass to `dmsetup create`. The dangerous mistake is assembling a
**stale leg**: a disk that was ejected from the array while the other leg kept
running solo and kept acknowledging writes. Assembling the stale leg silently
**rolls back acknowledged writes** — a failure a single local disk can never
exhibit. The journal cannot help, because the stale leg's journal is
internally consistent; it is consistently *old*.

A second, subtler mistake: treating a **half-rebuilt** leg as a data source. A
leg that was resyncing at crash time is not merely stale, it is *internally
inconsistent* (new writes mirrored + an arbitrary partially-copied prefix).

### 1.3 Correctness criterion

**Relative to a single local disk.** The baseline single disk is assumed to
provide: atomic single-sector writes (512 B/4 KiB), a journaling filesystem,
read-only/panic on I/O error, and correct flush/FUA handling. The goal:

> If a single local disk avoids corruption/inconsistency in a given crash
> scenario, the raid1 device under this policy avoids it in the same scenario.
> The policy may *refuse to assemble* (unavailability), but it must never
> serve rolled-back or inconsistent data.

### 1.4 Kernel facts the design relies on (axioms)

These are claims about the kernel, not about this design. §9.8 explains how to
validate them empirically on your exact kernel version.

| # | Axiom | Consequence |
|---|-------|-------------|
| K1 | raid1 acknowledges a write only after it completes on **every currently in-sync leg** | If both legs were in-sync at crash, *each leg individually* holds all acked writes. If degraded, only the survivor does. |
| K2 | dm-raid failures are **sticky**: a failed leg stays failed (`D` in status) until an explicit administrative table reload / recreate | Spontaneous transitions only go healthy→unhealthy. Every *improvement* passes through the control plane, so it can be write-ahead-logged. |
| K3 | On ejecting a leg, the kernel bumps the surviving leg's superblock `events` counter and sets the ejected leg's bit in `failed_devices` **before acknowledging further writes** | The surviving superblock is a crash-consistent, kernel-maintained witness of "I ran solo". |
| K4 | After a crash with both legs in-sync, assembly of both triggers a **bitmap resync** of dirty regions only, arbitrated by the superblocks | In-flight divergence between legs is reconciled; the higher-`events` leg is the copy source. |
| K5 | Newer kernels refuse to eject the **last** remaining leg; they fail the I/O upward instead | "Both legs marked failed by the kernel" is rare; the array simply goes dead with the last survivor still nominally in-sync. The design handles both behaviors. |

---

## 2. Architecture overview

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
3. **Ack = etcd quorum commit**, not "receiver received". The user's existing
   global increasing counter (`seq`) is written with every record; the
   receiver/etcd transaction rejects any write whose `seq` is not greater than
   the stored one, so duplicated/reordered gRPC messages cannot regress state.
4. **No improvement without quorum.** If etcd is unreachable, the array may
   run degraded or stay down, but no re-add/create/single-leg-start executes.
   Failure records may be delayed (safe direction, see rule T3 in §4).

Why one record suffices (no history): the entire recovery decision reduces to
a single derived fact — **the last writable leg set** ("who was in-sync the
last time writes could be acknowledged"). While both legs are down no writes
are acked, so both-down periods contribute nothing; and by K2 any improvement
passes through the control plane, which updates the record. Maintaining this
fact transitionally (the `survivor` field below, with a *carry* rule) replaces
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
| `survivor` | **The recovery decision.** Every leg listed holds *all* acknowledged writes | (a) If any leg is `insync`, `survivor` = exactly the set of `insync` legs. (b) If no leg is `insync`, `survivor` is **carried unchanged** from the last record where one was — it may name a currently-`failed` disk, meaning "the freshest acked data is on that broken disk". (c) A `syncing` leg is **never** in `survivor`. |
| `op` | In-flight membership change: `{"kind":"create"\|"readd", "target":"B", "authoritative":["A"]}` | `op.authoritative == survivor`; `op.target ∉ survivor`. Enables idempotent redo after a crash mid-`dmsetup`. It never changes the data-source decision — that is always `survivor`. |
| `ts` | Audit only | Never used in decisions |

The *carry* rule (b) is the compact replacement for scanning history: it is
the same fold, computed eagerly at write time.

---

## 4. The three timing rules

Correctness lives in *when* records are written relative to actions:

- **T1 — Write-before (WAL):** any transition that starts improving a stale
  leg, or deliberately stales a readable one, is committed to etcd with quorum
  ack **before** the `dmsetup` command runs. Applies to: fresh create, re-add,
  administrative single-leg start.
- **T2 — Write-after-verify:** `syncing → insync` is recorded only after
  `dmsetup status` shows **all health chars `A` AND sync ratio complete AND
  sync action `idle`** — never on "`dmsetup` returned 0". Recording it on
  command success is a real corruption bug: if the sync source then dies and
  the machine crashes, the record would name a half-rebuilt leg as survivor.
- **T3 — Write-lazy:** kernel-detected failures are recorded as soon as the
  event arrives, but correctness never depends on beating a crash: a late
  failure record makes recovery *conservative* (or is overridden by superblock
  evidence, §6), never wrong — with one narrow exception quantified in §8-R1.

---

## 5. Transitions: what to write, when

| # | Condition | Record written (order matters) |
|---|-----------|-------------------------------|
| 1 | **Fresh create** (treat as a re-add from seed leg A) | T1-WAL: `op={create,target:B,auth:[A]}`, `A:failed,B:failed`, `survivor:[A]` → run create with B in the rebuild slot → after status verifies array up: `A:insync, B:syncing` (op stays) → on completion, T2 promote: `A,B:insync`, `survivor:[A,B]`, `op:null` |
| 2 | **B fails while both in-sync** | T3: `B:failed`, `survivor:[A]` |
| 3 | **A fails while sole survivor** (array dead) | T3: `A:failed`, `survivor` stays `[A]` (**carry**) |
| 4 | **Rebuild target B fails mid-sync** | `B:failed`, `op:null` (aborted), `survivor` unchanged `[A]` |
| 5 | **Sync source A fails while B is `syncing`** | Write **both** `failed`, `survivor:[A]` (carry), `op:null`. B is recorded `failed` regardless of what the kernel briefly reports — half-synced is not a member |
| 6 | **Re-add of failed A** (directional, replaces symmetric "unknown/unknown") | T1-WAL `op={readd,target:A,auth:[B]}`, quorum ack, only then act. Try both legs (A in rebuild slot): verified up → `A:syncing` (op stays); sync completes → T2 promote. Both-leg attempt fails → rebuild with B only: success → `A:failed, B:insync, survivor:[B], op:null`. That fails too → both `failed`, `survivor:[B]` (carry), `op:null` |
| 7 | **Administrative single-leg start** (recovery-time availability choice, §6) | T1-WAL the demotion — peer `failed`, `survivor:[chosen]` — quorum-acked **before** `dmsetup create`; serve writes only after the ack |
| 8 | **Agent boot reconciliation** | Diff dm reality vs record. Demotions written directly (T3); improvements only via T1/T2 |

Lifecycle example:

```
t0 create WAL      op{create,B,auth:[A]}  A:failed  B:failed   surv:[A]
t1 create verified op{create,B,auth:[A]}  A:insync  B:syncing  surv:[A]
t2 sync complete   op:null                A:insync  B:insync   surv:[A,B]
t3 B fails         op:null                A:insync  B:failed   surv:[A]
t4 A fails too     op:null                A:failed  B:failed   surv:[A]   <- carry
t5 reboot, A ok    op:null                A:insync  B:failed   surv:[A]   (write, then assemble)
t6 readd B WAL     op{readd,B,auth:[A]}   A:insync  B:failed   surv:[A]
t7 dm verified     op{readd,B,auth:[A]}   A:insync  B:syncing  surv:[A]
t8 promote         op:null                A:insync  B:insync   surv:[A,B]
```

Why the directional `op` matters: with symmetric "both unknown", a crash during
a re-add forces a refusal. With `auth:[B]` recorded, a crash at *any* point in
row 6 resolves to "assemble B": during the both-leg attempt B is still the ack
path (K1); after a failed attempt B is unchanged; even if the final success
notification was lost while the array ran fully synced, B individually holds
every acked write (K1 again).

---

## 6. Recovery procedure (dirty reboot)

Preliminaries, always:

1. **Identity + superblock probe** of both meta areas (§9.4): device WWN must
   match `legs.X.dev`; superblock magic/`num_devices`/`array_position` must be
   sane. Unreadable, blank, or foreign → classify that leg `failed` for this
   decision. This guards against swapped disks winning arbitration.
2. **Record-write discipline:** any decision that changes effective
   membership writes the record first (demotions, per T1) or after
   verification (promotions, per T2), before serving writes.

Then branch on the record. Note that the *data-source* choice is a function of
`survivor` + superblocks + availability only; `legs` states and `op` affect
only the redo/cleanup path.

**Case `survivor:[A,B]`, `op:null`** — both legs were in-sync at last record.

- Both readable → assemble both. K4's bitmap resync reconciles in-flight
  divergence.
- Exactly one readable (say A) → read A's superblock:
  - `failed_devices` marks B → A provably ran solo after the last record
    (K3). Write the row-7 demotion, assemble A alone.
  - A's superblock is clean and B is unreadable → **the irreducible
    ambiguity**: "B died at the crash instant" vs. "A was ejected first, B ran
    solo, and the proof died with B". *Strict mode:* refuse. *Availability
    mode:* execute row 7 with eyes open; the rollback exposure is bounded by
    the event-propagation latency since the last record (§8-R2).
- Neither readable → unavailable; record untouched.

**Case `survivor:[X]`** (covers `peer:failed`, `peer:syncing`, and
both-`failed` with carried survivor — i.e., the entire old "walk back through
history" logic):

- X readable → assemble X alone (write the confirmation record if states
  changed, e.g. after a both-failed carry where X now answers). If `op` is
  present, idempotent redo is licensed: assemble X with the target back in the
  rebuild slot, or assemble X alone and re-add later.
- X unreadable → **unavailable**, no matter how healthy the peer looks. A
  `failed` peer is stale by construction (serving it = silent rollback); a
  `syncing` peer is internally inconsistent — strictly worse. Leave the peer
  untouched as material for *manual* disaster recovery.
- Sanity tripwire: if the peer's superblock shows strictly higher `events`
  with X marked failed, an improvement happened without a WAL. Unreachable
  under this protocol → alert, go manual; never auto-pick.

**Case record missing/corrupt** — superblock-only arbitration: equal `events`
on both → assemble both (or either alone, per K1); strictly higher `events` +
peer marked in `failed_devices` → that leg alone; anything else → refuse.

---

## 7. What this plan guarantees

- **No rollback of acknowledged writes** by any automatic assembly decision,
  under axioms K1–K4 and the timing rules T1–T3, except the two quantified
  residuals in §8 (R1, R2 — both are record-staleness windows, both require a
  disk to be *destroyed*, and R2 only bites in availability mode).
- **A `syncing` leg is never a data source**, so half-rebuilt inconsistency is
  never served.
- **O(1) etcd state per device** — one compact record; the historical-scan
  policy ("find the last record that isn't both-unhealthy") is subsumed by the
  `survivor` carry rule. Deleting all history is safe by construction.
- **Crash anywhere is recoverable**: every `dmsetup`-mutating step is bracketed
  by a WAL (`op`) or is idempotent, and every recovery branch is defined.
- Refusal (unavailability) is always an allowed outcome; the design trades
  availability for integrity at every ambiguous point unless availability
  mode is explicitly chosen.

## 8. What this plan cannot resolve (residual risks)

- **R1 — Lazy-failure window + survivor destruction.** Writes acked by a solo
  survivor whose T3 failure-record hadn't committed yet, followed by *physical
  destruction* of that disk plus a crash: the record still names the stale
  peer in `survivor`, and the disproving superblock (K3) died with the disk.
  Recovery then serves the stale peer as if current. Information-theoretically
  unclosable by any external witness (the evidence is gone); window =
  event→quorum latency, typically sub-second. Equivalent in *data loss* to
  losing your only current disk, but manifests as silent staleness rather
  than an error.
- **R2 — Both-in-sync record, one leg unreadable, clean surviving
  superblock** (§6 case 1). Strict mode converts this to unavailability;
  availability mode accepts a rollback window bounded by record latency.
- **R3 — In-flight divergence semantics.** Until the post-crash bitmap resync
  covers a region, the two legs may hold different versions of an in-flight
  sector and reads may return either copy — successive reads can disagree,
  which a single atomic-sector disk cannot do. Journal replay tolerates this
  in practice (replay decides once, then rewrites), but it is technically
  weaker than the single-disk baseline. Unavoidable in raid1 without a
  write-journal target.
- **R4 — Silent bit rot.** raid1 cannot arbitrate a mismatch found by scrub;
  "repair" copies one leg over the other arbitrarily. Mitigation: stack
  `dm-integrity` under each leg, converting rot into read errors that raid1
  *can* repair from the peer.
- **R5 — Witness loss.** If etcd data is lost, recovery degrades to
  superblock-only arbitration (§6 last case): correct where superblocks are
  decisive, refusals where they aren't.
- **R6 — Axiom risk.** K1–K5 are kernel claims that have varied in detail
  across versions (e.g. rebuild-direction handling for a previously-failed
  leg). They must be validated per kernel (§9.8); the formal proof in §10 is
  conditional on them.
- Out of scope: raid5/6 (adds the write-hole; requires dm-raid
  `journal_dev`/`journal_mode` and a "no dirty+degraded start" rule —
  different document), multi-writer, performance tuning.

---

## 9. Implementation guide

### 9.1 Agent event loop

```
last = read event counter:  dmsetup info -c --noheadings -o events <dev>
loop:
  dmsetup wait <dev> <last>          # blocks until event_nr > last
  last = new event counter
  st = parse(dmsetup status <dev>)   # events coalesce and carry no payload:
  diff st against cached state       # ALWAYS re-read and diff, never assume
  emit transition records per §5 (respecting T1/T2/T3)
```

Per-device serialization: one goroutine/actor per device; all etcd writes for
a device go through it in order, stamped with the next `seq`.

### 9.2 dmsetup command patterns

Table format: `start len raid raid1 <#params> <params...> <#devs> <meta data>...`
where the first raid param is chunk size (unused by raid1 → `0`).

```bash
# Fresh create / re-add direction forced: B (index 1) is the rebuild target
dmsetup create r0 --table "0 209715200 raid raid1 5 0 region_size 8192 rebuild 1 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"

# Assemble both after a crash with survivor:[A,B] (kernel arbitrates via superblocks + bitmap)
dmsetup create r0 --table "0 209715200 raid raid1 3 0 region_size 8192 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"

# Degraded, A only ('-' = absent device)
dmsetup create r0 --table "0 209715200 raid raid1 3 0 region_size 8192 2 \
  /dev/mapper/meta_A /dev/mapper/data_A - -"

# Re-add B into a RUNNING degraded array (preferred: FS stays mounted)
dmsetup suspend r0
dmsetup reload  r0 --table "0 209715200 raid raid1 5 0 region_size 8192 rebuild 1 2 \
  /dev/mapper/meta_A /dev/mapper/data_A /dev/mapper/meta_B /dev/mapper/data_B"
dmsetup resume  r0
```

Rules: always force rebuild direction explicitly (`rebuild <idx>`, and/or wipe
the target's meta area) — never rely on "create returned 0" implying the
kernel picked your intended source; verify from status that the intended leg
is the one rebuilding. Prefer suspend/reload/resume over teardown+create for
re-adds.

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

The dm-raid superblock is at offset 0 of each meta device, little-endian
(`struct dm_raid_superblock`, kernel `drivers/md/dm-raid.c` — pin the layout
against your kernel source and cover it in tests):

| Offset | Field | Use here |
|--------|-------|----------|
| 0 | `magic` (le32) = `0x64526D44` | sanity |
| 8 | `num_devices` (le32) | sanity (== 2) |
| 12 | `array_position` (le32) | sanity (matches slot) |
| 16 | `events` (le64) | arbitration: higher = fresher |
| 24 | `failed_devices` (le64 bitmask) | K3 witness: "I saw the peer fail" |

Identity (which physical disk is which) is **not** in the superblock — bind it
via the WWNs stored in the etcd record and `/dev/disk/by-id`. Probe outcome ∈
{sane, blank, foreign/garbage, unreadable}; anything but *sane* → `failed` for
the decision in §6.

### 9.5 etcd access rules

Single-key-per-device; every write is a transaction:
`if stored.seq < new.seq then put(new)` (the user's global counter provides
`seq`). Improvement paths (T1) block on quorum ack before any `dmsetup`
executes; T3 failure paths fire-and-retry in the background. Sync progress is
never written.

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
        if sb(peer(x)).events > sb(x).events and sb(peer(x)).marks(x):
            return MANUAL_ALERT               # protocol-violation tripwire
        write_confirmation_if_changed(x)      # e.g. after both-failed carry
        return assemble([x])                  # op ⇒ may redo rebuild of peer
    return UNAVAILABLE                        # never auto-serve the peer
```

### 9.8 Testing (validating the axioms)

- **Crash injection:** `dm-log-writes` under each leg (or `dm-flakey` +
  replay) to cut power at arbitrary write boundaries; after each cut, run
  recovery and `fsck`/checksum-verify against the acked-write log. This is
  what validates K1/K3/K4 on *your* kernel.
- **Targeted scenarios:** crash while degraded; crash during resync (both
  directions: target death, source death); crash between `dmsetup` success and
  the T2 promote; re-add of a previously-failed leg (K2 stickiness, and
  whether your kernel requires `rebuild` explicitly); disk-swap (foreign
  superblock must lose); K5 behavior of last-leg failure.
- **Fault drills:** etcd unreachable during each transition class; gRPC
  duplication/reorder (must be absorbed by `seq` CAS); agent kill between WAL
  and `dmsetup` (op redo path).

---

## 10. Formal verification with TLA+

### 10.1 Modeling approach

Abstract data as **version counters**: `acked` = highest write version the
application saw acknowledged; `ver[d]` = durable version on leg *d*. etcd is
one atomic variable (its linearizability is assumed, not re-proven). Volatile
kernel state (`running`, `inSync`, `rebuild`) is wiped by `Crash`; `ver`,
superblocks, `intact`, and the record survive. Axioms K1/K3 become the
*structure* of actions — which is exactly why they must be validated
empirically (§9.8): the proof is conditional on them.

The single checked safety property:

> `NoStaleServe == running => ∀ d ∈ inSync : ver[d] = acked`
> (an assembled, serving array never contains a leg missing acked writes)

### 10.2 Spec skeleton

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

Init == /\ running /\ inSync = Disks /\ rebuild = {}
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
  /\ UNCHANGED <<acked,intact,sbEv,sbPF,surv>>
RecoverSolo(d) ==
  /\ ~running /\ d \in intact
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Peer(d) \notin intact
        /\ (sbPF[d] \/ Policy = "avail")             \* §6 ambiguity branch
  /\ running' = TRUE /\ inSync' = {d} /\ rebuild' = {}
  /\ surv' = {d}                                     \* row-7 demotion before serving
  /\ sbEv' = [sbEv EXCEPT ![d] = @ + 1] /\ sbPF' = [sbPF EXCEPT ![d] = TRUE]
  /\ UNCHANGED <<acked,ver,intact>>

Next == WriteAck \/ Crash \/ RecoverBoth \/ RecordPromote
        \/ \E d \in Disks : Eject(d) \/ Destroy(d) \/ StartRebuild(d)
                            \/ CompleteRebuild(d) \/ RecoverSolo(d)
Spec == Init /\ [][Next]_vars

NoStaleServe == running => \A d \in inSync : ver[d] = acked
SurvFresh    == \A d \in surv : d \in inSync \/ ~running => ver[d] = acked \* aux/inductive
========================================================================
```

TLC config: check `NoStaleServe` (and `SurvFresh` as an auxiliary invariant);
state constraint `acked <= 3 /\ sbEv["A"] <= 4 /\ sbEv["B"] <= 4`; run once
per `Policy` value. 2 disks, ≤3 writes, ≤2 crash/recover cycles is exhaustive
in seconds.

> **Verified.** The extracted spec, configs, and TLC runs live in `tla/` (see
> `tla/README.md` for the full report). The §10.2 skeleton requires four
> small fixes to reproduce the §10.3 expected results exactly:
>
> 1. `Init` must write `running = TRUE` (the doc's bare `/\ running` is not a
>    boolean expression TLC accepts).
> 2. The `CONSTRAINT`/`SPECIFICATION` clauses must name operators defined in
>    the module (introduced `StateBound`); the doc's inline prose form is
>    rejected by TLC.
> 3. `RecoverBoth` must clear `sbPF` (`sbPF' = [e \in Disks |-> FALSE]`) on
>    K4 bitmap resync. The doc's `UNCHANGED sbPF` leaves stale `failed_devices`
>    bits across re-add cycles, which then masquerade as fresh K3 witnesses and
>    produce counterexamples that are **not** R1/R2 (they reproduce with
>    `Destroy` disabled, contradicting §8).
> 4. The `RecoverSolo` §6 tripwire must be `~(Peer(d) \in intact /\
>    sbPF[Peer(d)])`, not the doc's literal "strictly-higher peer `events`."
>    In a re-add cycle both legs bump `events` symmetrically, so the literal
>    form never fires; the tripwire then fails to refuse a stale `surv={d}`
>    record even when the intact peer's K3 witness freshly contradicts it,
>    producing a strict-mode counterexample with no `Destroy` (not R1).
>    `CompleteRebuild` clears `sbPF`, so a `TRUE` bit is always
>    post-last-resync evidence that `d` was ejected and the peer ran solo.
>
> An `Idle == UNCHANGED vars` action is added to `Next` in all three modules
> so the `UNAVAILABLE` terminal states (where the tripwire correctly refuses
> to serve either leg) are not reported as deadlocks.

### 10.3 Verified results (see `tla/README.md` for the full report)

- **S0 (idealized):** `NoStaleServe` and `SurvFresh` hold for both policies
  (4000/4100 distinct states, <1s each).
- **S1 (realistic):** `NoStaleServe` is violated.
  - Strict: the counterexamples are exactly R1. Disabling `Destroy` (TLC
    `RecoverMode = "nodestroy"`) makes strict pass, confirming every strict
    violation requires destruction of the true survivor — i.e. all are R1.
  - Avail: the counterexamples are R1 and R2. Disabling `Destroy` makes avail
    pass too, confirming no third residual. R2 is reachable only under
    `Policy = "avail"` (it requires the §6 ambiguity branch's `Policy="
    avail"` sub-branch); R1 is reachable in avail exactly as in strict.
- The "counterexample set equals the documented residual list" correspondence
  §10.3 makes — the model checker's CE set equals {R1, R2}, no others —
  holds after the four fixes above.

### 10.3 The S0/S1 methodology — what "verified" means here

- **S0 (idealized):** as written above — the etcd record update is atomic with
  `Eject`. Expected result: `NoStaleServe` holds for **both** policies. This
  proves the *decision logic* (survivor semantics, carry, syncing exclusion,
  T1/T2 ordering, superblock arbitration) has no holes — including that the
  deliberately-modeled T2 promote lag (`RecordPromote` as a separate action)
  is harmless.
- **S1 (realistic):** delete the `surv' = inSync'` conjunct from `Eject`
  (replace with `UNCHANGED surv`) and add
  `RecordEject == running /\ surv # inSync /\ surv' = inSync /\ UNCHANGED ...`.
  Expected result: TLC produces counterexample traces that are **exactly R1
  and R2 from §8** (both require a `Destroy` of the true survivor; R2 only
  under `Policy = "avail"`), and *no others*. Strict policy keeps
  `NoStaleServe` in every trace not involving R1.

That correspondence — the model checker's counterexample set equals the
documented residual list — is the strongest claim this document makes: the
policy has no *undocumented* rollback path within the model.

### 10.4 Limits of the proof

TLC is bounded model checking, not a deductive proof (TLAPS is possible but
heavy; at this state-space size TLC is the pragmatic choice). The result is
conditional on axioms K1–K5, which are kernel claims — hence §9.8. The model
also idealizes etcd as linearizable and omits Byzantine faults (a disk lying
about flush completion breaks the single-disk baseline equally).

---

## 11. Operator assumptions checklist

1. Single-disk baseline holds per leg: atomic sector writes, journaling FS,
   read-only/panic on I/O error.
2. All devices in the stack honor flush/FUA and pass it through dm intact
   (journal ordering depends on it).
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