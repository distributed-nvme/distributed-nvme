# A Logic Gap in the dm-raid1 Recovery Tripwire, Found via TLA+

> **Caveat — read this first.** The gap described below is real *in the
> model*, but whether it manifests in a real dm-raid kernel depends on a
> kernel-property assumption that is **not** in axioms K1–K5 and was not
> empirically validated for this document: *does the kernel reconcile
> `events` counters across legs when a bitmap resync completes?*
>
> - If the kernel **does** reconcile (brings the re-added leg's `events` up to
>   match the survivor's before declaring both in-sync), then at any
>   post-ejection crash the survivor's `events` is strictly higher than the
>   ejected leg's, the original "strictly higher peer `events`" tripwire fires
>   correctly, and the gap in §2.5 does **not** occur. The intuition "if both
>   disks are healthy, dm-raid1's own metadata can re-assemble correctly" is
>   essentially right in that case.
> - If the kernel **does not** reconcile (leaves the re-added leg's `events`
>   at its pre-resync value, as the TLA+ model's `CompleteRebuild` does), then
>   across a re-add cycle the two counters become equal and the original
>   tripwire fails as described below.
>
> The TLA+ spec modeled `CompleteRebuild` as not touching `sbEv` (only
> clearing `sbPF`), which is how TLC found the trace in §2.5. That modeling
> choice should be validated against real kernel behavior (§9.8 of the
> external doc) before concluding the gap is more than a model artifact. The
> corrected `failed_devices`-based tripwire is defense-in-depth regardless:
> it is correct whether or not the kernel reconciles `events`, because
> `failed_devices` is asymmetric by construction (cleared on resync, set on
> ejection, never copied). But if the kernel does reconcile `events`, the
> original tripwire also works and §2.5's rollback path does not manifest.
>
> The external witness (etcd record) remains valuable in either case for the
> **disk-destroyed** residuals R1/R2 (§8 of the external doc), where the
> kernel has no peer superblock to compare and K4's tiebreaker is helpless —
> that value is independent of the `events`-reconciliation question.

This note describes a logic gap that the §10 TLA+ model checking exposed
in the recovery procedure of `dm_raid1_crash_recovery_1.md` §6. The
external-facing document states the corrected procedure; this note is the
record of what was wrong, how the model found it, and why the fix is correct.

---

## 1. The recovery procedure's tripwire

The recovery procedure (§6 of the external doc) has a branch named `Case
survivor:[X]`: the etcd record names exactly one leg as holding all
acknowledged writes. The procedure must decide whether to assemble that leg
alone, refuse, or do something else.

The decision needs a *tripwire* — a check that refuses to trust the record
when on-disk evidence contradicts it. Without a tripwire, the procedure will
happily assemble a leg that the record names as the survivor even when the
other (intact) leg's superblock proves the survivor was actually ejected and
the other leg ran solo. That is the silent-rollback failure the whole design
exists to prevent.

The original design stated the tripwire as:

> Sanity tripwire: if the peer's superblock shows strictly higher `events`
> with X marked failed, an improvement happened without a WAL. Unreachable
> under this protocol → alert, go manual; never auto-pick.

In words: compare the `events` counter on each leg's superblock. The leg with
the strictly-higher counter is fresher; if the fresher leg also marks the
recorded survivor as `failed` in its `failed_devices` bitmask, the record is
stale and the leg the record names should not be assembled.

That reads plausibly. The TLA+ model says it is wrong.

---

## 2. Why the tripwire is wrong: `events` bumps symmetrically across a re-add cycle

The kernel-claim K3 (§1.4 of the external doc) is: when a leg is ejected, the
*surviving* leg's superblock `events` counter is bumped and the ejected leg's
bit is set in the survivor's `failed_devices`. The bump and the bit-set are
recorded on the peer, not on the ejectee.

Now walk a re-add cycle, starting from both legs in-sync (`events` both 0,
`failed_devices` both empty):

1. `Eject(A)`: B's `events` bumps to 1; B's `failed_devices` marks A.
2. `RecordEject` (T3) commits: record says `survivor:[B]`.
3. `StartRebuild(A)` then `CompleteRebuild(A)`: A rejoins, both in-sync, and
   — crucially — `CompleteRebuild` clears `failed_devices` on both legs (K4
   completed bitmap resync writes fresh superblocks). `events` is *not*
   decremented; both legs still hold `events == 1`.
4. `Eject(B)`: A's `events` bumps to 2; A's `failed_devices` marks B.
5. `RecordEject` commits: record says `survivor:[A]`.

At this point, A's `events` (2) is strictly higher than B's (1). So far so
good — the tripwire would fire on a stale `survivor:[B]` here, which is
exactly what we want.

But continue the cycle:

6. `StartRebuild(B)` then `CompleteRebuild(B)`: B rejoins, both in-sync,
   `failed_devices` cleared on both. A's `events` still 2, B's still 1.
7. `Eject(A)` again: B's `events` bumps to 2; B's `failed_devices` marks A.
8. `RecordEject` commits: record says `survivor:[B]`.

After step 7, A's `events == 2` and B's `events == 2`. The counters are
**equal**. The tripwire's "strictly higher" condition is *not satisfied on
either side*. Now crash at this moment with a T3 record lag — the record
still names `survivor:[A]` from an earlier (stale) state, but the just-acknowledged
solo-running leg is B. Both legs are intact, both have equal `events`, and
both have a `failed_devices` bit marking the *other* leg (B's marks A from
step 7; A's marks B from step 4 — wait, step 6 cleared A's `failed_devices`,
so only B's bit is set).

The tripwire looks at the peer of the recorded survivor (A → peer B):
- peer B's `events` is not strictly higher than A's (both 2), so the
  "strictly higher" condition fails.
- the tripwire does not fire. The procedure assembles A alone.
- But the freshest-acked data is on B (B just ran solo after step 7, before
  the record caught up). Assembling A alone serves stale data.

The rollback the design promised to prevent has happened, *without* any disk
being destroyed, *without* entering availability mode, and *not* one of the
documented residuals in §8.

The root cause is that `events` bumps are symmetric: every `Eject` bumps the
peer by one, so over a long re-add/eject history the two counters stay within
one of each other. The "strictly higher" relation is only transiently true
right after a single ejection and before the next one; it is not a reliable
asymmetry witness across cycles.

---

## 2.5 A concrete end-to-end example

To make the gap concrete, here is a full timeline with actual superblock
values. The columns are what the two legs' superblocks hold (read from
`meta_A` and `meta_B`), what the etcd record holds, and how many writes the
application has seen acknowledged. `fd(X)` is the `failed_devices` bitmask on
X's superblock; `ev(X)` is X's `events` counter.

Start: both legs in-sync, no writes yet.

| # | Action | ev(A) | fd(A) | ev(B) | fd(B) | record surv | inSync | acked | ver[A] | ver[B] |
|---|--------|-------|-------|-------|-------|-------------|--------|-------|--------|--------|
| 0 | (initial)                          | 0 | {}  | 0 | {}  | [A,B] | {A,B} | 0 | 0 | 0 |

B fails (kernel-detected). Per K3, the survivor A bumps `events` and marks B
in `failed_devices` *before* more writes are acked. The T3 record update is
lazy: it fires next, not simultaneously.

| 1 | Eject(B)            | 1 | {B} | 0 | {}  | [A,B] (T3 lag) | {A} | 0 | 0 | 0 |
| 2 | RecordEject (T3)    | 1 | {B} | 0 | {}  | [A]            | {A} | 0 | 0 | 0 |
| 3 | WriteAck            | 1 | {B} | 0 | {}  | [A]            | {A} | 1 | 1 | 0 |

At step 3, A is sole survivor holding the acked write (`ver[A]=1=acked`),
B has stale data (`ver[B]=0`).

Operator re-adds B. The resync completes (K4 bitmap resync). The model
represents the completed-resync state by clearing `failed_devices` on
both legs — a leg that just resynced is back in-sync and has no surviving
witness of any ejection. `events` is *not* decremented.

| 4 | StartRebuild(B)     | 1 | {B} | 0 | {}  | [A]            | {A}, rebuild={B} | 1 | 1 | 0 |
| 5 | CompleteRebuild(B)  | 1 | {}  | 0 | {}  | [A]            | {A,B} | 1 | 1 | 1 |
| 6 | RecordPromote (T2)  | 1 | {}  | 0 | {}  | [A,B]          | {A,B} | 1 | 1 | 1 |

Now A fails (kernel-detected again). Survivor B bumps `events` and marks A:

| 7 | Eject(A)            | 1 | {}  | 1 | {A} | [A,B] (T3 lag)  | {B}   | 1 | 1 | 1 |
| 8 | RecordEject (T3)    | 1 | {}  | 1 | {A} | [B]            | {B}   | 1 | 1 | 1 |
| 9 | WriteAck            | 1 | {}  | 1 | {A} | [B]            | {B}   | 2 | 1 | 2 |

At step 9, B is sole survivor holding the acked write (`ver[B]=2=acked`),
A has stale data (`ver[A]=1`).

Operator re-adds A:

| 10 | StartRebuild(A)    | 1 | {}  | 1 | {A} | [B]            | {B}, rebuild={A} | 2 | 1 | 2 |
| 11 | CompleteRebuild(A) | 1 | {}  | 1 | {}  | [B]            | {A,B} | 2 | 1 | 2 |
| 12 | RecordPromote (T2) | 1 | {}  | 1 | {}  | [A,B]          | {A,B} | 2 | 1 | 2 |

Now B fails:

| 13 | Eject(B)            | 2 | {B} | 1 | {}  | [A,B] (T3 lag)  | {A}   | 2 | 1 | 2 |
| 14 | WriteAck            | 2 | {B} | 1 | {}  | [A,B] (T3 lag)  | {A}   | 3 | 3 | 2 |

Now we crash *before* the step-13 `RecordEject` commits — the lazy-failure
window. `running` becomes false; volatile state is wiped; everything durable
survives. Both legs are intact (no `Destroy`).

| 15 | Crash               | 2 | {B} | 1 | {}  | [A,B]          | {}     | 3 | 3 | 2 |

Note carefully the superblock state at the crash:

- `ev(A) = 2`, `ev(B) = 1`. A's `events` is strictly *higher* than B's. The
  survivor-of-the-last-ejection is A (correct: A ran solo after step 13).
- The *current record*, however, says `survivor=[A,B]` — the T3 record update
  for step 13 had not committed yet. Per the §6 case "`survivor:[A,B]`,
  `op:null`", both legs intact ⇒ assemble both and let K4's bitmap resync
  reconcile. On the model, `RecoverBoth` sets `ver[A]=ver[B]=acked=3` and
  clears `fd`. The assembled array serves fresh data — no rollback.

So far, no gap. The tripwire's job is the `survivor:[X]` branch. Let me replay
the **exact same** superblock history but crash at a different instant — the
one the TLA+ model found.

### The failing replay

We replay the same sequence but crash *between step 13 and the next
RecordEject*, at a moment where the record still says `survivor=[B]` from
step 8 — the lazy-T3 window of step 13 has not yet replaced it. To get there,
stay in step 7–9 longer and crash before step 10's `RecordPromote` can catch
up to the new reality. Concretely, change the order: crash at the moment when
the record is *still carrying an older `survivor:[B]` from a prior cycle*, and
A has since cycled through rebuild and back to solo-acking.

| step | superblock state | record | inSync | acked |
|------|------------------|--------|--------|-------|
| (after step 9)         | ev(A)=1, fd(A)={},   ev(B)=2, fd(B)={A} | surv=[B] | {B}   | 2 |
| StartRebuild(A)         | unchanged                                   | surv=[B] | {B}+rebuild={A} | 2 |
| CompleteRebuild(A)      | ev(A)=1, fd(A)={},   ev(B)=2, fd(B)={}     | surv=[B] | {A,B} | 2 |
| Eject(B)                | ev(A)=2, fd(A)={B},   ev(B)=2, fd(B)={}    | surv=[B] (T3 lag) | {A} | 2 |
| WriteAck                | unchanged                                  | surv=[B] (T3 lag) | {A} | 3, ver[A]=3 |
| **Crash** (no Destroy)  | ev(A)=2, fd(A)={B},   ev(B)=2, fd(B)={}    | surv=[B] | {}    | 3, ver[A]=3, ver[B]=2 |

At the crash:

- Both A and B are intact (no `Destroy`), so the superblock probe (§9.4 of
  the external doc) reads both `meta_A` and `meta_B`. From `meta_A` it reads
  `ev(A) = 2` and `fd(A) = {B}`; from `meta_B` it reads `ev(B) = 2` and
  `fd(B) = {}`. Both fields are available from both legs.
- Recorded survivor is `[B]`. The recovery procedure enters the
  `Case survivor:[X]` branch with `X = B`.
- B is intact. B's peer A is also intact.
- `ev(A) = 2`, `ev(B) = 2`. **Equal.** The original "strictly higher peer
  `events`" tripwire looks at peer A's `events` (2) vs B's `events` (2),
  finds them equal, and **does not fire** — even though `fd(A) = {B}` was
  read in the same probe and is asymmetric evidence pointing the other way.
- The procedure assembles B alone (no demotion, no alert). B goes `inSync`,
  the array serves reads from B at `ver[B] = 2`. The application has seen
  `acked = 3`. Two acknowledged writes — written to A in the last solo
  window — are silently rolled back. **No disk was destroyed; this is not
  R1. Strict mode was used; this is not R2.**

### The same crash, with the corrected tripwire

At the crash, the procedure instead consults peer A's `failed_devices`:

- `fd(A) = {B}` — A's superblock marks B as failed. Per K3 this is fresh
  evidence (set at the most recent `Eject(B)`, in step 13, and not cleared
  since — no `CompleteRebuild` has run since).
- The corrected tripwire `~(Peer(d) \in intact /\ sbPF[Peer(d)])` evaluates
  to `~(A \in intact /\ fd(A)={B})` = `~(TRUE /\ TRUE)` = **FALSE**. The
  `RecoverSolo(B)` action is disabled.
- The procedure refuses to assemble B. It neither serves B (stale) nor
  serves A (A's record is the survivor it would pick under a fallback).
  Per the design, refusing an ambiguous recovery is the safe outcome —
  the operator is alerted, and the leg is left as material for manual
  disaster recovery.

The corrected tripwire prevents the rollback *without* depending on the T3
record being current and *without* needing any disk to be destroyed. The
asymmetry comes from `fd(A)`, which the kernel only ever writes on the
surviving leg and which `CompleteRebuild` clears on both — a stable,
post-last-resync witness, exactly the property `events` lacks across a
re-add cycle.

---

## 3. Why the correct tripwire is `failed_devices` (the `sbPF` bit)

The `failed_devices` bitmask is the real asymmetric witness. The reason is
the way it is cleared:

- `Eject(d)` *sets* `failed_devices[Peer(d)] = TRUE` on the surviving peer.
  It is never set on the ejectee.
- `CompleteRebuild` (the T2 promotion, which models K4's bitmap-resync
  completion) *clears* `failed_devices` on **both** legs. A leg that has just
  rejoined and resynced has no surviving witness of any ejection.

So a `TRUE` bit on leg X's superblock is *post-last-resync* evidence that leg
X saw its peer get ejected after the most recent completed resync. It is
exactly K3's "I ran solo" witness, and it is asymmetric in the way `events`
is not: the only way for X's `failed_devices` bit for Y to be `TRUE` right
now is for Y to have been ejected and for no resync to have completed since.

With that, the tripwire becomes:

> Refuse to assemble X when an intact peer Y's superblock has
> `failed_devices` marking X as failed. The witness is fresh (post-last-resync)
> and contradicts the record's `survivor:[X]`; go manual or unavailable.

In the TLA+ spec, this is:

```tla
RecoverSolo(d) ==
  /\ ~running /\ d \in intact
  /\ ~(Peer(d) \in intact /\ sbPF[Peer(d)])   \* the tripwire
  /\ \/ surv = {d}
     \/ /\ surv = Disks /\ Peer(d) \notin intact
        /\ (sbPF[d] \/ Policy = "avail")
  /\ ...
```

`sbPF[X]` in the model is leg X's `failed_devices[Y]` bit (where Y is X's
peer). The tripwire fires exactly when an intact peer's bit marks the
candidate.

### Why `events` does not go away

To be explicit: the superblock probe (§9.4 of the external doc) still reads
*both* the `events` field (offset 16) and the `failed_devices` bitmask (offset
24) from each leg's meta device. Nothing about reading `events` changes. What
changes is *which field is used for which decision*.

`events` is still the right signal for the **record-missing/corrupt** case
(§6 last branch of the external doc), where there is no `survivor` to
disprove and the design must fall back to superblock-only arbitration. Equal
`events` on both → assemble both; strictly higher `events` + peer marked →
that leg alone. In that fallback `events` is the best available signal, and
"strictly higher" is a perfectly fine predicate for *breaking ties between
two legs neither of which we trust*.

What `events` cannot do is serve as a *contradiction witness* against a known
stored `survivor`. That role needs an asymmetric, post-last-resync signal —
which is exactly what `failed_devices` provides, because `CompleteRebuild`
clears it. So:

| Field | Read from meta? | Used as | Reason |
|-------|-----------------|---------|--------|
| `events` | yes, both legs | tiebreaker in the record-missing/corrupt fallback | symmetric across cycles, but ties are fine to break with a symmetric signal |
| `failed_devices` | yes, both legs | tripwire against a stored `survivor` | asymmetric and cleared by resync, so a `TRUE` bit is fresh evidence of a post-last-resync ejection |

---

## 4. How the model exposed this

S1 of the spec is the realistic variant where the etcd record update is not
atomic with `Eject` — it happens later via a `RecordEject` action (T3
lazy-failure model). Run that variant and `NoStaleServe` is violated. That is
expected: §8 documents two residuals R1 and R2, both requiring physical
destruction of the true survivor.

To prove the counterexample set is **exactly** {R1, R2} (and not some third
undocumented path), the model introduces a `Destroy` action and runs with a
`nodestroy` mode that disables it. The expectation:

- Every strict-mode counterexample involves a `Destroy` (R1 requires it). So
  `strict / nodestroy` should pass.
- Every avail-mode counterexample involves a `Destroy` (R1 and R2 both
  require it). So `avail / nodestroy` should pass.

With the original "strictly higher `events`" tripwire, **strict / nodestroy
failed** — TLC produced a counterexample with no `Destroy` at all. The trace:

```
Init           inSync={A,B}  surv={A,B}   acked=0  sbPF=[F,F]
Eject(A)       inSync={B}    surv={A,B}   acked=0  sbPF[B]=T   (lazy T3: surv unchanged)
RecordPromote  inSync={B}    surv={B}     acked=0              (surv catches up to {B})
StartRebuild(A) inSync={B}   surv={B}
CompleteRebuild inSync={A,B} surv={B}     sbPF=[F,F]           (resync clears sbPF; ver[B] stays old)
Eject(B)       inSync={A}    surv={B}     sbPF[A]=T            (A's events bumps to 2; B's stays 1)
WriteAck       inSync={A}    ver[A]=1     acked=1              (A acks solo)
Crash          running=F     surv={B}                          (no Destroy — both intact)
RecoverSolo(B) inSync={B}    ver[B]=0     acked=1              (B served stale; ver[B]=0 < acked=1)
```

Notes on this trace:

- No `Destroy` is ever taken. Both disks are intact at recovery time.
- The crash happens after A cycled through a rebuild and back to solo-acking.
  A's `events` (2) is strictly higher than B's (1) at the crash, so if the
  tripwire looked at peer-A's `events` vs B's it would correctly fire — but
  the *recorded survivor* is B, and B's peer is A, and A's `events` is
  strictly *lower* than B's in the lazy window before `RecordPromote`
  catches up in the next cycle. The model finds the exact instant where the
  counters' asymmetry that the tripwire relied on is pointing at the wrong
  leg.
- At the final `RecoverSolo(B)`, `sbPF[A] = TRUE` (A marks B failed from the
  last `Eject(B)`). The corrected tripwire `~(Peer(B) \in intact /\
  sbPF[Peer(B)])` = `~(A \in intact /\ TRUE)` = `FALSE` — the action is
  disabled, B is not served, and the trace is not reachable.

After replacing the tripwire with the `sbPF` form, `strict / nodestroy`
passes; the only remaining S1 counterexamples all involve a `Destroy` and
match the §8 R1/R2 description exactly.

---

## 5. Why a human review missed this

The prose of the original §6 read naturally: "peer shows strictly higher
events with X marked failed → alert." A reviewer reading it forward, in a
single-eject scenario, sees it fire correctly. The bug only surfaces across
a re-add cycle, where:

- the `events` relation becomes symmetric,
- `failed_devices` carries the asymmetry (because of the clear-on-resync
  semantics), and
- the asymmetric witness and the "strictly higher" witness disagree about
  which leg to trust.

This is a closed-loop interaction between two kernel-managed superblock
fields across multiple ejection/resync cycles. It is exactly the kind of
 reasoned-about-correctly-in-the-small-but-wrong-in-the-large thing that
bounded model checking is good at finding: the state space is small (2
disks, ≤3 writes, ≤2 crash/recover cycles), but the *interesting* traces
are deep enough that a forward-reading human doesn't enumerate them.

---

## 6. Summary

- The original tripwire, "peer's superblock shows strictly higher `events`
  with X marked failed," is too weak. `events` counters bump symmetrically
  across re-add cycles, so the "strictly higher" condition is only
  transiently present and cannot serve as a reliable contradiction witness
  against a stored `survivor`.
- The correct tripwire is "an intact peer's `failed_devices` bitmask marks
  the candidate leg as failed." `failed_devices` is cleared by each
  completed resync, so a `TRUE` bit is always post-last-resync evidence
  that the candidate was ejected and the peer ran solo — the asymmetric
  witness the tripwire needs.
- The TLA+ model exposed the gap by producing a strict-mode counterexample
  with no `Destroy` (i.e., one of the documented residuals R1/R2 is not
  involved) — a rollback path §8 does not enumerate. After the fix, the S1
  counterexample set is exactly {R1, R2} as §8 documents; the design has
  no undocumented rollback path within the model.
- `events` retains its role in the record-missing/corrupt fallback case, as
  a tiebreaker between two records neither of which the design trusts. It
  is just not a contradiction witness against a known stored `survivor`.