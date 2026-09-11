# risks_and_gaps.md — ranked v1 risks and operational gaps

Status: **informational, non-normative.** This file ranks the residual
risks of the v1 design for operators and for v2 planning. Every mechanism
named here behaves as its owning spec says — nothing below is a defect in
the sense of the update_0N ledgers; each is a boundary the design accepts,
with the accepting decision cited. Companions: architecture.md Appendix D
(v1 assumptions and known limits), decisions [D12]/[D15]/[D16]/[D17].
Item ids `RK1`–`RK8`, append-only once cited — a closed item keeps its id and
is marked CLOSED in place, never deleted or renumbered. RK1–RK6 written
2026-09-10, from the second full doc-vs-code verification and its design
review; RK7 added 2026-09-11 with `dnvctl.md` and closed the same day by the
presence-based GW6 change, which opened RK8 in its place.

---

## RK1 — the alive-but-CP-partitioned old primary ("zombie") serves errors hosts cannot fail away from

**What.** The common failover — a dead CN — is clean: the host's paths to
it drop, IO queues, and the new primary's ANA-`optimized` flip releases it.
The bad case is an old primary that keeps **running** while only its
control-plane connectivity is lost ([D16], §11.1): it cannot apply the
syncup that demotes it, so its host-facing namespaces stay in the
`optimized` ANA group while the DNs fence its data paths underneath. Its md
arrays then fail, and the errors nvmet returns on that still-`optimized`
path are target-internal (DNR) — host multipath does **not** retry DNR
errors on the new primary's path, and `ctrl_loss_tmo` never fires because
the controller is alive.

**Blast radius.** Applications on affected hosts can see EIO for as long as
the zombie keeps running — unbounded until the node is stopped, restarted,
or its CP connectivity returns and it applies its demotion. Data stays
consistent throughout (the [D16] safety argument: per-side atomic flips
mean one leg never has two writers, and md event-count arbitration
preserves every acked write), so this is an **availability** hole, not a
correctness one.

**Operator guidance.** Treat "primary failed over but the old CN is still
up" as an incident step: stop the old `dnv-agent` (or the node) promptly;
until then expect application EIO on hosts that keep using the stale path.
Monitor for the signature: a cntlr that etcd says is no longer primary
whose CN still answers on the data network. `UpdateCntlrEnabled(false)` on a
reachable-but-suspect primary is the planned-failover tool
(`architecture.md` §8.6: a `disabled` primary is itself a failover trigger, with
no threshold wait); it does not help against a genuinely CP-partitioned node,
which cannot converge the disable either.

**Direction.** [D16] records the v2 shape: a fencing epoch checked on the
data path — NVMe reservations, or a per-revision gate at the side exports —
not more ordering in the worker, which cannot reach a partitioned node
anyway.

---

## RK2 — a transfer origin's ns-dev stays suspended for the whole transfer; block-device scanners wedge on it

**What.** `CreateTransfer(auto_suspend = true)` suspends the origin
namespace's `CnNsDevName` and leaves it suspended while the destination
clone hydrates (§8.10, §11.3). Unlike the §11.2 migration fence — bounded
at `SuspendSeconds` = 60 s with a reload-onto-error at the deadline — this
suspension is **unbounded** ([D12]'s recorded residual). A suspended dm
target queues bios forever with no timeout: any process that opens the
device blocks in uninterruptible D state (`exit_aio` then makes it
unkillable; the node needs a reboot to clear it).

**Blast radius.** dnv's own agents stopped scanning block devices when
[D13]/[D14] removed LVM, so the exposure is **external tooling on the CN**:
a udev worker, `blkid`, `lsblk`, monitoring or backup agents. A transfer
hydrating a large td runs for hours, and for that whole window one touch of
the suspended ns-dev wedges the toucher unkillably.

**Operator guidance.** Keep block-device walkers away from dnv devices on
CNs while transfers run (the spirit of the Appendix A udev guard, which
covers md assembly but not generic scanners). Schedule transfers away from
inventory/backup scans, or filter dnv's `dnv-*` dm names out of those
tools.

**Direction.** [D12] records a considered-but-undecided bounded
alternative: after a grace window, reload the origin ns-dev onto its
dm-error the way the migration fence does — hosts already queue against the
`inaccessible` ANA state, and the transfer's own data path
(`CnXferFinalName` on the raid0) never maps the ns-dev. The design review's
recommendation is to decide that alternative for v2; until then this stays
an operational rule.

---

## RK3 — aborting a transfer+clone after cutover discards writes served by the destination

**What.** In the §11.3 cross-SP move, `CreateClone(auto_resume = true)`
makes the destination SP's namespace the serving path while hydration runs.
On abort, the source resumes from its retained copy, which stopped
receiving writes at `CreateTransfer(auto_suspend = true)` — so every write
a host made through the destination since the cutover is abandoned on the
destination td. And the abort has a sharp edge of its own: `DeleteClone`
(force included) unconditionally sets `suspended = false` on the dst td's
namespaces (§8.9 — the resume that the finalize path needs), which would
bring the destination back `optimized` over its *partial* copy as a second,
divergent host path. §11.3 therefore now specifies the abort order: retire
the destination namespace first (`DeleteNamespace` — a mere suspend is
undone by `DeleteClone`'s resume), then `DeleteClone(force = true)`, then
`DeleteTransfer(force = true)`; the sp2 td and its partial bytes remain
until deleted.

**Blast radius.** Data loss proportional to the time between cutover and
abort, on exactly the volume being moved. This is inherent to aborting a
live migration whose destination has served writes — no mechanism bug —
but it is easy to trip: "the copy is slow, let me abort and retry" is the
natural operator move and is only lossless while nothing has written via
the destination.

**Operator guidance.** Treat abort as safe only in the setup window
(before hosts write through the destination). After that, prefer letting
hydration finish and finalizing; if an abort is unavoidable, quiesce the
host workload first, or accept the loss window. There is no automated
guard: the CP does not track "first write via destination".

**Direction.** A v2 refinement could refuse `force = true` on a clone whose
destination namespace has served writes (needs a cheap dirty signal — e.g.
the destination td's thin-pool mapped count exceeding what hydration alone
explains), or require an explicit second flag. Not designed; recorded here
so the trade is deliberate.

---

## RK4 — a full thin pool queues IO for ~60 s and then errors; auto-grow is best-effort

**What.** The §10.4 auto-grow reacts to the pool's low-water-mark report,
but nothing reserves space ahead of writes and a grow can fail (no DN
candidates — `RESOURCE_EXHAUSTED`; the meta ladder at its 16 GiB cap —
`FAILED_PRECONDITION`; §8.5).
The agent writes no feature arguments to the thin-pool table (`cnagent.md`
CN13), so an exhausted pool behaves as dm-thin's default
`queue_if_no_space`: IO that needs a new block queues for the kernel's
`no_space_timeout` (a dm-thin module parameter, 60 s by default) and then
fails with EIO; reads and overwrites of already-provisioned blocks keep
serving. §10.4 now states this next to the auto-grow rule.

**Blast radius.** Hosts see a hard ~60 s write stall followed by EIO on
new-allocation writes, per affected slice pool, until a grow lands or space
is freed. Filesystems on the volume may remount read-only on the EIO.

**Operator guidance.** Alert on pool data/metadata usage well before 100 %
(the `dmsetup status` ratios ride in every `CheckCntlr` reply and are
visible via `InspectCntlr`); keep enough free DN extents for the auto-grow
to draw on; treat `low_water_mark_pct > 100` (auto-grow off) as "manual
`GrowSlice` is now on the pager path". The `queue_if_no_space` default
means brief exhaustion is absorbed if a grow lands within the timeout.

**Direction.** none needed beyond monitoring; if a future deployment wants
fail-fast semantics instead, that is a deliberate `error_if_no_space` table
change with its own doc update, not a tuning knob.

---

## RK5 — scale ceilings: one primary per SP, SP-granular fan-out, per-node head-of-line blocking

**What** (all recorded in architecture.md Appendix D; gathered here because
they compose): a volume's whole throughput is its SP's single primary CN —
slices shard within that CN, never across CNs; any `SpRev` bump re-fans the
complete desired state to every side and cntlr of the SP, so bring-up of a
large SP costs O(sides × (sides + cntlrs)) syncups and every mutation costs
one full fan-out; and on an agent node, one pending node-level write
(`SyncupDn`/`SyncupCn`) queues behind the slowest in-flight object converge
(SH10-SH13), so one sick object can delay every other object's converge and
Check round on that node by up to a converge pass.

**Blast radius.** Performance and convergence latency, not correctness:
revision keys are durable, syncs are idempotent, convergence is delayed,
never skipped.

**Operator guidance.** Scale out by storage pool — spread SPs (hence
primaries) across CNs; size slices so one CN's bandwidth is acceptable per
volume; expect provisioning of very large SPs to take a fan-out burst (the
§10.3 batched `provisioned` flips are the mitigation); on nodes with many
objects, watch for one object pinning its converge at the command timeouts
(§7) and dragging the node's rounds.

**Direction.** Appendix D marks these as accepted v1 boundaries. The
natural v2 levers — per-object revisions, cross-CN slice placement — are
deliberate non-goals until the single-primary model itself is revisited.

---

## RK6 — minor hardening notes

* **cntlid slot ranges touch at their boundaries.** §11.8 assigns slot *s*
  the inclusive nvmet range `[10000 + s×5000, 10000 + (s+1)×5000]`, so
  adjacent slots share one boundary CNTLID (15000, 20000, …). Unreachable
  in practice — nvmet's ida allocates lowest-first and a dnv subsystem
  hosts at most `MaxHostCntPerSs` = 8 controllers — but the "partitions the
  cntlid space" claim has a one-value hole per boundary. Hardening, when
  convenient: `attr_cntlid_max = min + step − 1` in both agents'
  `cntlidRange` (`agent/cnagent/plan.go:894-898`,
  `agent/dnagent/plan.go:223-227`) plus the §11.8 sentence; it changes no
  observable behavior at today's scales.
* **No metrics endpoint.** Observability is JSON logs (log.md), the
  `ResInfo` trees behind `Inspect*`, and the etcd state itself; there is no
  scrape endpoint, counter set, or health URL on any daemon. Fine for the
  integration lab; production operation will want an exporter (pool usage,
  err_epoch ages, reaction counts, zeroing/hydration progress are all
  already computed and logged — the gap is exposition, not measurement).
  Recorded as a deliberate v1 omission alongside the Appendix D security
  posture (trusted fabric, plaintext gRPC, deployment-configured etcd
  access, spoofable hostnqn allow-lists — deploy on an isolated fabric).

---

## RK7 — a dnvctl mutator without `--rev` fails until the gateway's token check becomes presence-based — **CLOSED 2026-09-11**

**CLOSED the same day it was opened**, by the gateway change it prescribed.
GW6 is now presence-based: an absent token message skips the revision check,
a present one is compared strictly as before (`gateway/common.go`
`checkSpToken`/`checkDnToken`/`checkCnToken` now take the token *message* and
compare only `if tok != nil`). A dnvctl mutator without `--rev` therefore
succeeds. gateway.md §0 #7 and GW6, and architecture.md §5.5, were amended
with the change; the unit tests that pinned the old rule were reworked to pin
the new one. The residual exposure the closure creates is **RK8** below —
this entry is kept, per the append-only rule, for the record of what the
window was.

**What it was.** `dnvctl.md` §0 #9 made `--rev` an optional pass-through: an
omitted flag sends no token message, on the assumption that the gateway would
skip the GW6 revision check when the token is absent. The gateway then did
the opposite by design — an absent token read as revision 0, which never
matched a stored revision (they seed at 1 and only grow), so every token-less
mutator failed `ABORTED "stale revision"`.

**Blast radius while open.** Operator friction only: the 34 token-carrying
mutators required the `dn|cn|sp get` → `--rev` two-step; no state was at risk
(the check failed closed), and scripts written against the assumed semantics
failed loudly with ABORTED, never silently. Added 2026-09-11 with
`dnvctl.md` §0 #9; closed the same day.

---

## RK8 — a token-less mutator has no optimistic-concurrency gate

**What.** GW6 is presence-based (gateway.md §0 #7, architecture.md §5.5): a
mutator whose request omits the `DnRev`/`CnRev`/`SpRev` message runs with no
revision check at all. Two such requests racing on one object are serialized
only by etcd's STM and by whatever in-STM preconditions the RPC itself
carries — the last writer wins, and neither client is told its view was
stale. This is the deliberate cost of closing RK7, not a defect: omitting the
token *is* the opt-out, chosen per request.

**Blast radius.** Lost updates on concurrently mutated objects, confined to
callers that chose not to send a token. Two consequences are worth naming
individually:

* **AG4's two-phase safety argument weakens for these callers**
  (gateway.md AG4, §5.8 DeleteClone, §5.11 FinishMigration). A two-phase RPC
  still re-resolves everything in its deciding STM, but without a token an
  interleaved mutation it never observed stays invisible; the "exactly as
  safe as a one-STM RPC" claim holds only for token-carrying requests.
* **`CreateCntlr`'s anti-affinity premise weakens** (`gateway/cntlr.go`): two
  token-carrying `CreateCntlr`s on one SP are serialized by the token, two
  token-less ones are not, and the exclusion list they scanned can go stale
  between the scan and the commit. What still protects them is the in-STM
  slot/placement check, not the token.

Nothing here can corrupt an invariant key or produce a partial write: the STM
is still all-or-nothing, resolution still runs, and every other precondition
still applies. The exposure is exactly "a mutation computed against a view
that moved".

**Operator guidance.** For anything concurrent, anything scripted against a
shared SP, and every destructive mutator, do the two-step: `dnvctl sp get`
(or `dn get` / `cn get`) and pass the returned `--rev`. Reserve the
token-less form for interactive, single-operator work. `--rev 0` remains the
deliberate always-stale probe — a *present* zero token — and is never a way
to skip the check.

**Direction.** If the ungated path proves too easy to reach by accident, the
cheap next step is a gateway-side policy flag making the token mandatory for
some or all mutators (refusing a token-less mutator with
`INVALID_ARGUMENT`), rather than reverting to the RK7 semantics — the two are
distinguishable precisely because presence, not value, is the discriminator.
Added 2026-09-11 with the RK7 closure.
