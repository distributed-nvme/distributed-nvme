# update_06.md — disabled-primary failover, fence adoption at the gate, failure-domain-aware repair placement, park-before-remove for namespaces, and ledger invariant errors

Status: **decided (2026-09-10); companion documents amended; the code
changes below are APPLIED (2026-09-10).** It resolves the five code-side
findings of the 2026-09-10 second full doc-vs-code verification. All five
were decided **fix the code**; the current normative text is already right
everywhere except the §6 companion edits listed per item, which MUST land in
the same change as their item's code. Item ids `U1`–`U5`, append-only once
cited from a spec, a commit or code. Section references are the named
document's own sections; `file:line` references are against the 2026-09-10
tree (commit `dd32bd0` plus that day's doc-amendment pass) — re-locate by
the quoted code if they have drifted. Implementation order is free except
that each U-item lands atomically with its §6 edits and its tests.

Amended 2026-09-10 by the implementation pass, as the U-items landed:
**(a)** U3's §3 Code 2 helper gained a `requiredCnt int` parameter and its
tier-2 trigger became `len(tier1) < requiredCnt`. As drafted it fired tier 2
"when it returns fewer than `candCnt`", but `candCnt` is the **oversampled
scan width** — `gateway/alloc.go` `pickDns` passes `plan.Legs ×
dn_batch_size`, the worker's AR8 scan passes `dn_batch_size`, and
`common/constants.go:29` sets `DefaultAllocDnBatchSize = 16` — while
`FindDnCandidates` returns at most one candidate per location. Under the
drafted reading tier 1's result is discarded wholesale in any cluster with
fewer than ~16 out-of-domain failure domains, U3's anti-affinity is inert in
every realistic deployment, and §3's OWN normative gateway test
("`CreateSpareLeg` picks the distinct-location DN") is unsatisfiable at the
default batch. §3's Decision prose already said "fewer candidates than
required" and architecture.md §6.5 already distinguishes `DnCandCnt =
RequiredCnt × dn_batch_size` from `RequiredCnt`, so this is the signature
catching up with the rule both documents had already stated. What landed:

    // FindDnCandidatesAntiAffine is FindDnCandidates with the two-tier
    // location rule of §6.5: tier 1 excludes excludeLocs; when it yields
    // fewer than requiredCnt — the DNs the caller must actually place, not
    // the oversampled scan width candCnt — tier 2 rescans without the
    // location exclusion and its candidates are merged behind tier 1's. The
    // bool reports whether tier 2 was used.
    func FindDnCandidatesAntiAffine(
        ctx context.Context, cli *etcdutil.Client, cid uint64,
        cc *pb.ClusterConf, candExt uint64, candCnt int, requiredCnt int,
        black, white, excludeLocs []string,
    ) ([]Cand, bool, error)

`requiredCnt` is `plan.Legs` at the gateway and `1` at the worker. Tier 2
**merges behind** tier 1 rather than replacing it: tier 2 walks
free-count-descending and stops at `candCnt`, and the group's own excluded
domains usually hold the fullest DNs, so a plain replace can drop a
distinct-domain candidate tier 1 had already found. §3's Code 2 block and
Tests bullets below are rewritten to this signature — a ledger whose body
still specified the superseded one is the half-applied correction this
project keeps hitting.
**(b)** §3 Code 3's closing sentence ("the worker's AR8 path reads the same
confs through its `etcdutil` client before the scan") was corrected to the
snapshot the code actually uses: a new `grpLocations(state, grp)` in
`worker/reaction.go` resolves every `grpAddrs` addr_port through
`SpState.DnByAddr`. It is the same data, read the same way, at no extra
cost: MD3 loads one `DnConf` per side `addr_port` of the SP into `DnByAddr`
in the STM that opens the pass (`model/stm.go:246-253`), and `stm.go`'s
`sideAddrs` walk (:113-144) covers `leg_list` **and** `spare_leg_list` of
every group — exactly the set `grpDnAddrs` names — so the locations still
come through the worker's own etcdutil client, still before the scan, with
no second round trip. dnv-worker.md §11.5 was already amended to the
snapshot wording (:1233-1235); §3 Code 3 now agrees with it.
**(c)** §7.2's U5 acceptance grep was over-broad and is rescoped to the
ledger file. It demanded `grep -rn "disk node %q not found" gateway/` hit
nothing, but that literal legitimately survives at four **request-named**
sites in `gateway/disknode.go` — `DeleteDiskNode` (:209), `GetDiskNode`
(:282), `UpdateDiskNodeDisabled` (:384) and `InspectDiskNode` (:444), each
on `req.GetAddrPort()` — which GW7 requires to stay `NOT_FOUND` and which
`integtest/gateway_test.sh:3686` pins (`gwx NOT_FOUND get-dn`).
`gateway/controllernode.go`'s four `"controller node %q not found"` sites
(:250, :324, :428, :491) are the CN twin, equally untouched and equally
correct. U5 only ever concerned the two allocator ledgers of
`gateway/alloc.go` (:195, :336), which now return `errAborted`; a verifier
running the drafted clause literally would have recorded U5 as unapplied, or
"fixed" `disknode.go` and broken the public API.
**(d)** §3's Tests bullets describe the U3 tests but, unlike U1/U2/U4/U5,
named none, so the seven names that landed are recorded here and in the
bullets themselves: `model/alloc_test.go`
`TestFindDnCandidatesExcludeLocs` (tier 1 inside the base scan, plus the
nil/empty-exclusion identity), `TestFindDnCandidatesAntiAffine` (both tiers
and the nil-exclusion no-second-scan case),
`TestFindDnCandidatesAntiAffineTriggerIsRequiredCnt` (the (a) trigger: tier
1 short of `candCnt` but holding `requiredCnt` does NOT fall through, and
`requiredCnt + 1` does) and `TestFindDnCandidatesAntiAffineMergesTier1`
(tier 1's find stays at the head of a `candCnt`-truncated tier-2 list);
`gateway/handler_vol_test.go`
`TestCreateSpareLegPrefersAnotherFailureDomain` and
`TestCreateMigrationPrefersAnotherFailureDomain`; `worker/reaction_test.go`
`TestReactionSpareCreateExcludesGroupLocations`. These seven are normative
from here on, on the same footing as the other items' names.
**(e)** Two of §3's own §6 companion edits had landed carrying the
**superseded** (a) trigger; both were re-amended to it the same day
(2026-09-10), in the fix wave that this note asked for, and this is the
record of it. architecture.md §6.5's CreateMigration bullet read "when tier
1 yields fewer candidates than `DnCandCnt`", where the rule is
`RequiredCnt` — the very distinction the same §6.5 list draws at :947-949
(`DnCandCnt = RequiredCnt × AllocConf.dn_batch_size`); it now reads "when
tier 1 yields fewer candidates than `RequiredCnt` — the DNs the operation
must actually place, never the oversampled `DnCandCnt`", with the
merged-behind clause (:964-977). dnv-worker.md §4 MD5's allocator row
quoted `FindDnCandidatesAntiAffine(…, black, white, excludeLocs []string)`
with "fewer than `candCnt` ⇒ tier 2", i.e. both the drafted signature and
the drafted trigger; it now quotes the landed
`FindDnCandidatesAntiAffine(…, candCnt, requiredCnt int, black, white,
excludeLocs []string) ([]Cand, bool, error)` and "fewer than `requiredCnt`
— the DNs the caller must actually place, never the oversampled `candCnt`",
merge included (:413-423). gateway.md §5.10/§5.11 and dnv-worker.md §11.5
had said only "when tier 1 comes up short", which was never wrong; the same
wave sharpened all three to name the one DN each path places and to deny
the `DnCandCnt`/`candCnt` reading outright (gateway.md :675-680, :710-714;
dnv-worker.md :1231-1238). No carrier of the superseded trigger survives
outside this file, which quotes it deliberately: both drafted phrases —
"fewer than `candCnt`" and "fewer candidates than `DnCandCnt`" — grep to
nothing under doc/ once this file is filtered out, per §7.2's convention.
U3 is applied whole, code and companions alike. The U-item texts below are
the amended, authoritative versions.
**(f)** Final read-through of this file against the finished tree
(2026-09-10, after both fix waves). Everything §1-§5 specifies is applied;
these are the facts the body did not yet carry, each verified in the tree
rather than inferred from the waves.
*§3's Code 1 and Code 4 landed at one granularity coarser than drafted,
with the same effect.* Code 1's "the only callers" is the pre-change
inventory: `FindDnCandidates` now has **no** non-test caller outside
`model/alloc.go` itself, because `FindDnCandidatesAntiAffine` wraps it for
both tiers, and Code 4's three call sites reach it through two switch
points — `gateway/alloc.go` `pickDns` and `worker/reaction.go`
`modelReactionOps.findDnCandidates`. The wrapper is therefore the single
entry point for all six DN allocation paths, the three §3 deliberately
leaves on the plain scan included: CreateStoragePool, GrowSlice and the AR6
grow pass an empty `ExcludeLocs`, and on an empty exclusion the helper
returns tier 1 unchanged and skips a tier 2 that would only repeat it — so
their behavior is identical to the direct call they had before.
*One wrong cross-reference, corrected in place.* §3 Code 3 sourced
`location` immutability to architecture.md `§5.5`, which is "Revision keys
and the sync fan-out" and says nothing about it. The rule lives in **§8.2**
(Disk nodes): `location = addr_port` as the CreateDiskNode default, with
`UpdateDiskNodeDisabled` the only later DN mutator — which is what
architecture.md §6.5, gateway.md §5.10 and `grpDnLocations`' own comment all
cite. The pointer is fixed below; the argument it supports is unchanged.
*Names.* (d)'s seven U3 names are all present exactly as written. One
landed test was still unnamed here, and is normative from now on like the
others: U1's model coverage is `model/ops_test.go`
`TestFailoverDisabledPrimary` — a **healthy** disabled old primary fails
over with `now` equal to the fixture epoch (no threshold waited), the old
cntlr keeps `err_epoch == 0` and keeps `disabled`, and `SpRev` bumps once —
with the two refusals as rows of `TestFailoverPreconditions` ("old cntlr is
healthy and enabled", "primary_unhealthy not reached"). §1's Tests bullet
names it below. The two tests this file sited with an "or" landed in the
first-named file for U2 (`agent/dnagent/migr_test.go`) and in the
alternative for U5: `gateway/handler_sp_test.go`, not
`handler_node_test.go`. U4's, sited only by package, landed in
`agent/cnagent/cnagent_test.go`.
*The §3 gateway pair is deterministic by fixture, not by luck.* Its
`volTwoDomainEnv` does two things and both are load-bearing: it relocates
dn-c into dn-a's failure domain **and** gives it `volDnCFree = volDnFree ×
2`, so the §6.3 descending capacity walk reaches dn-c *first* and a
placement on dn-d can only be the location rule's doing, never the index
order's. With dn-a and dn-b black-listed, tier 1 hands `PickRandom` a
one-entry list — which is what makes "picks the distinct-location DN" an
exact assertion rather than a probable one. §3's Tests bullet now says so.
*U1's integration step is no longer outstanding.* §1's SHOULD landed as
`integtest/worker_test.sh` stage 11 and dnv-worker.md §14 case D step 11,
so §7.3's `worker_test.sh` line now covers a new stage and not only a
regression re-run. One ordering fact is load-bearing and both carriers
state it: step 10's disabled standby is the SP's only other cntlr, so its
behavior row is cleared and its `err_epoch` waited back to 0 **while it is
still disabled** — enabled and still unhealthy, AR7 would replace it before
this step could elect it.
*`TestReactionDisabledCntlrIsHandsOff` needed no adjustment* — the
conditional edit §1's third Tests bullet allowed for. No fixture of it
disables the *primary*; it pins only the standby rule the new trigger
leaves alone, and it is unmodified in the diff.
*U4's suite verdict held.* `integtest/cnagent_test.sh` is untouched, as §4
predicted; what landed in cnagent_integtest.md is the record of the gap —
its §19 out-of-scope entry and its §20 amendment, both pointing at
cnagent.md §6 test 26 — and not the MAY stage.
*§7.2's greps all pass as written* against this tree, with the file-scope
fix (c) made: the `"old cntlr is healthy"` literal survives only under
`opReplaceCntlr` (`model/ops.go`) and its two `TestReplaceCntlrPreconditions`
pins.
*§4's "every removed namespace" is a universal its own Code paragraph
narrows*, and the companion edit inherited the unqualified form. The landed
loop skips `np.td == nil` — a namespace that never had a backing td has no
`CnErrorName` to park on and keeps `removeDm`'s resume as its backstop — so
cnagent.md CN9 now carries that carve-out in the sentence itself. Read §4's
Decision with its Code paragraph, which always stated the guard.

Conventions for the implementer: run `make build vet test` after each item
(`go test` needs a real etcd for the STM suites — set
`ETCD_BIN=$PWD/integtest/bin/cache/etcd-v3.6.14-linux-amd64/etcd`, otherwise
those tests SKIP silently); every new test name below is normative; where
this file quotes an error string, the string is normative too.

---

## 1. U1 — a disabled primary is a failover trigger

**Finding.** architecture.md §8.6 has always said "disabling the current
primary triggers the §10.4 primary re-election", and DeleteCntlr's
precondition rationale ("disable first so a failover has already happened")
depends on it — but nothing implements it. The only trigger is err_epoch:
`worker/reaction.go:515` (`tryFailover`) requires
`reached(now, p.primary.GetErrEpoch(), primary_unhealthy)`, and
`model/ops.go:984` refuses `fail(opFailover, "old cntlr is healthy")` when
`old.GetErrEpoch() == 0`. A disabled healthy primary converges to standby
shape (`agent/cnagent/plan.go:379` — `primary && !disabled`), so its health
stays clean, the etcd `primary` boolean never moves, no RPC can move it, and
`DeleteCntlr` refuses while `primary == true`: the SP is dark until the
operator re-enables the cntlr. dnv-worker.md AR5 sides with the code, so the
two normative documents contradict each other; this item decides for
architecture.md.

**Decision.** `Cntlr.disabled == true` on the primary is a failover trigger
in its own right, effective **immediately** (no threshold wait — disabling
is explicit operator intent, and the disabled primary has already stopped
serving). The candidate rule is unchanged: `failoverEligible` still requires
`!primary && !disabled && err_epoch == 0`, so a disabled cntlr is never
*elected*; AR3's hands-off meaning narrows to "a disabled cntlr is never a
candidate, never replaced (AR7) and never repaired — but a disabled primary
is itself the AR5 trigger".

**Code.**

1. `worker/reaction.go` `tryFailover` (:511-517): replace the single
   threshold gate with
   ```go
   if !p.primary.GetDisabled() &&
       !reached(p.now, p.primary.GetErrEpoch(), p.th.GetPrimaryUnhealthy()) {
       return false
   }
   ```
   Everything else (candidate skip, `ops.failover`, applied/failed
   bookkeeping) is unchanged.
2. `model/ops.go` `Failover` (:980-991): the in-STM re-validation mirrors
   the trigger. Keep the `"old cntlr is not primary"` check first; then:
   ```go
   if !old.GetDisabled() {
       if old.GetErrEpoch() == 0 {
           return fail(opFailover, "old cntlr is healthy and enabled")
       }
       if !thresholdReached(now, old.GetErrEpoch(),
           uint64(threshold.GetPrimaryUnhealthy())) {
           return fail(opFailover, "primary_unhealthy not reached")
       }
   }
   ```
   The reason string changes from `"old cntlr is healthy"` to
   `"old cntlr is healthy and enabled"`. In the Failover pinning case
   (`model/ops_test.go` ~:1141-1146 — its old primary is already enabled,
   the proto zero value) only the expected string changes; **do not touch**
   `ReplaceCntlr`, which uses the identical string for its own
   precondition (`model/ops.go:1382`, pinned at `ops_test.go:1868,1872`).
3. No gateway change: `UpdateCntlrEnabled` (`gateway/cntlr.go:398-443`)
   already writes only `disabled` + the CdcEntry lists and bumps `SpRev`,
   which is what wakes the sp-worker.

**Tests.**

* `worker/reaction_test.go` `TestReactionDisabledPrimaryFailsOver`: a
  **healthy** primary with `Disabled = true` plus one healthy enabled
  standby ⇒ the pass applies exactly one failover (old → the smallest
  eligible id) with no err_epoch set anywhere; a second sub-case with every
  other cntlr disabled or unhealthy ⇒ `reaction skipped` (`no candidate`)
  and the pass continues to the next reaction (AR2's continue list).
* `model/ops_test.go` `TestFailoverDisabledPrimary` (named by amendment
  (f)): `Failover` succeeds for a disabled healthy old primary (flips both
  `primary` booleans, bumps once); `TestFailoverPreconditions` still refuses
  an enabled healthy old (`"old cntlr is healthy and enabled"`) and still
  refuses an enabled unhealthy old below the threshold.
* Re-read `TestReactionDisabledCntlrIsHandsOff` (:753): it pins that a
  disabled **standby** is not a candidate (`reasonNoCandidate`) — that
  behavior is unchanged and the test must keep passing as written; adjust
  only if one of its fixtures disables the *primary* and asserts no
  failover, which the new rule inverts.

**Integration.** dnv-worker.md §14's reaction case SHOULD gain a
disabled-primary step (disable the fixture primary via a direct etcd write
through `workerctl`, assert the failover lands within one reaction pass);
locate the failover stage in `integtest/worker_test.sh` and mirror its
assertions. Unit coverage above is the MUST.

---

## 2. U2 — `settleFence` treats an adopted fence as started-and-elapsed

**Finding.** DN12 promises that a restart-adopted fence (per-CN linears
found suspended, window start unknown) is "treated as **elapsed**, and phase
2 runs on the first converge", and that a converge stopping at the DN9
side-device gate skips "never the fence". The gate backstop
`agent/dnagent/fence.go:136` (`settleFence`) guards on `fenceStarted(st)`,
which reads only `st.fenceAt` (:68-72) — zero for an adopted fence, because
`adoptFence` sets only `st.fenceRestarted` (:193-212). So: agent restart
mid-window + a first converge that fails the side-device probe
(`syncup_side.go:99-112`, `state != sideDevReady`) ⇒ `settleFence` returns
early, no phase 2, no timer, and the suspended dm-linears outlive §11.2's
"hard bound — a device is never left suspended beyond it, including across
an agent restart" (architecture.md:2515) until the next SpRev bump. The existing tests cover
restart+healthy (`TestFenceNotRestartedAcrossAnAgentRestart`) and
same-process+broken-gate (`TestFenceEndsEvenWhenTheSideDeviceIsBroken`), but
not the conjunction.

**Decision.** An adopted fence counts as "started" (and `inFence` already
answers false for it, i.e. elapsed), so the gate backstop finishes phase 2
for it exactly as it does for a same-process elapsed window.

**Code.** One guard: extend `fenceStarted` (fence.go:68-72) to
```go
return !st.fenceAt.IsZero() || st.fenceRestarted
```
and update its comment ("a window was ever started for this side — or
adopted, already elapsed, from a previous process"). Verify, and state in
the change description, the three call-path facts that make this the whole
fix: `inFence` stays false for an adopted fence (fenceAt zero, :77-84), so
`settleFence` falls through to its phase-2 loop and never calls
`armFenceTimer`; `beginFence` consults `fenceRestarted` itself first
(:54-57) and is unaffected; the normal (gate-open) adopted path through
`ensureCnDm` is unchanged. `settleFence`'s phase-2 loop is already
idempotent across passes, matching the elapsed-window behavior.

**Tests.** `agent/dnagent/migr_test.go` (or a new `fence_test.go`)
`TestFenceAdoptedSettlesAtTheGate`: seed the local store with a
migration-source request mid-fence (reuse
`TestFenceNotRestartedAcrossAnAgentRestart`'s adoption fixture: the fake dm
reports every per-CN linear `Suspended`), then make the reconcile's first
converge fail the DN9 gate (reuse
`TestFenceEndsEvenWhenTheSideDeviceIsBroken`'s broken-side fixture); assert
the pass still reloads every per-CN linear onto its dm-error and resumes it
(the same mutation set the elapsed-window test asserts), and that no fence
timer is armed.

**Integration.** None required — the trigger needs an injected probe
failure the hardware suites don't script; dnagent_integtest.md is untouched.

---

## 3. U3 — failure-domain-aware placement for migration destinations and spare legs

**Finding.** architecture.md §6.5 says CreateMigration's (and
CreateSpareLeg's) "black list starts with the DNs *(and thus locations)* of
every leg/side of the group", but the mechanism excludes only DNs:
`gateway/alloc.go:486` (`grpDnAddrs`) collects addr_ports, and in
`model/alloc.go:119-130` a black-listed DN is skipped **before** its
location enters `locSet` — so a spare leg or migration destination can land
on a different DN in the same failure domain as the surviving legs. The
worker's AR8 spare creation (`worker/reaction.go` `tryLegRepair` →
`findDnCandidates(black = grpAddrs)`) has the identical gap. (With the
default `location = addr_port` the exclusion degenerates to the DN
exclusion, so labs and default deployments see no behavior change.)

**Decision.** Two-tier placement, applied uniformly to gateway
CreateMigration, gateway CreateSpareLeg and the worker's AR8 spare
creation: **tier 1** scans with the group's existing failure domains
excluded (the locations of every leg side and spare side of the group);
when tier 1 yields fewer candidates than required, **tier 2** rescans
without the location exclusion (the DN black list always applies). Tier 2
is what keeps a two-rack cluster able to place a spare at all; the
resulting same-domain placement is visible in the stored topology, and no
new log record is added (the gateway.md §8 and dnv-worker.md §12 LG tables
stay untouched). CreateStoragePool, GrowSlice and CreateCntlr are
deliberately unchanged.

**Code.**

1. `model/alloc.go` `FindDnCandidates` gains one parameter,
   `excludeLocs []string`, seeded into `locSet` before the scan loop
   (insert after :100 `locSet := make(...)`). The only callers are
   `gateway/alloc.go` `pickDns` (:54, :65) and
   `worker/reaction.go` `modelReactionOps.findDnCandidates` (:212-223):
   give `pickDns` a new `ExcludeLocs` field on `dnPickPlan` and the worker
   op a matching parameter; the CreateStoragePool/GrowSlice `pickDns` call
   sites (`gateway/storagepool.go:363`, `:1111`) and the worker's AR6 grow
   scan leave it empty.
2. `model` gains the shared two-tier helper. The trigger is `requiredCnt` —
   the DNs the caller must place — and **not** `candCnt`, the oversampled
   scan width; see amendment (a) above for why the drafted `candCnt` trigger
   was wrong:
   ```go
   // FindDnCandidatesAntiAffine is FindDnCandidates with the two-tier
   // location rule of §6.5: tier 1 excludes excludeLocs; when it yields
   // fewer than requiredCnt — the DNs the caller must actually place, not
   // the oversampled scan width candCnt — tier 2 rescans without the
   // location exclusion and its candidates are merged behind tier 1's. The
   // bool reports whether tier 2 was used.
   func FindDnCandidatesAntiAffine(
       ctx context.Context, cli *etcdutil.Client, cid uint64,
       cc *pb.ClusterConf, candExt uint64, candCnt int, requiredCnt int,
       black, white, excludeLocs []string,
   ) ([]Cand, bool, error)
   ```
   `requiredCnt` is `plan.Legs` at the gateway's `pickDns` and `1` at the
   worker's AR8 scan. Tier 2 **merges behind** tier 1 rather than replacing
   it: tier 2 walks free-count-descending and stops at `candCnt`, and the
   group's own excluded domains usually hold the fullest DNs, so a plain
   replace can drop a distinct-domain candidate tier 1 had already found.
3. Locations of the group: a new `gateway/alloc.go` helper
   `grpDnLocations(ctx, cli, cid, grp) ([]string, error)` — one plain
   (non-STM) `DnConf` read per distinct addr_port from `grpDnAddrs`
   (which already covers active and spare legs alike, `alloc.go:464-494`).
   Plain pre-STM reads are sound because `location`
   is immutable in v1 — no RPC updates it: **§8.2** defaults it at
   CreateDiskNode and `UpdateDiskNodeDisabled` is the only later DN mutator
   (the drafted `§5.5` pointer was wrong, amendment (f)); a DN whose conf
   is gone contributes no location (it is black-listed by address anyway). The
   worker's AR8 path takes the same locations out of the pass's own
   snapshot — a `grpLocations(state, grp)` helper in `worker/reaction.go`
   resolving each `grpAddrs` addr_port through `SpState.DnByAddr` — which is
   the same data through the same client and still before the scan (see
   amendment (b) above).
4. Switch the three call sites to `FindDnCandidatesAntiAffine` with
   `excludeLocs = grpDnLocations(...)`: `gateway/migration.go:248`,
   `gateway/spareleg.go:210` (the `pickDns` calls), and the worker's
   spare-create scan in
   `worker/reaction.go` (`tryLegRepair`'s create branch). The in-STM
   re-checks stay address-based (`model/ops.go:1668` `grpHostsAddr`,
   `checkDnPick`) — location immutability makes a location re-check
   redundant.

**Tests.**

* `model/alloc_test.go` `TestFindDnCandidatesExcludeLocs`: `excludeLocs`
  drops a same-location candidate in tier 1, and `nil`/empty excludeLocs is
  byte-identical to today's behavior.
* `model/alloc_test.go` `TestFindDnCandidatesAntiAffine`: the fallback
  returning the same-location DN with `tier2 == true` when no
  other-location DN qualifies, and `nil` excludeLocs running the plain scan
  with no second scan.
* `model/alloc_test.go` `TestFindDnCandidatesAntiAffineTriggerIsRequiredCnt`:
  tier 1 short of `candCnt` but holding `requiredCnt` stays tier 1
  (`tier2 == false`) — an oversampled `candCnt` must never drag the scan
  into tier 2 — while `requiredCnt + 1` does fire it.
* `model/alloc_test.go` `TestFindDnCandidatesAntiAffineMergesTier1`: tier 1's
  distinct-domain find stays at the head of a `candCnt`-truncated tier-2
  list, which a replacing tier 2 would have dropped.
* `gateway/handler_vol_test.go`
  `TestCreateSpareLegPrefersAnotherFailureDomain` and
  `TestCreateMigrationPrefersAnotherFailureDomain`: with two spare-capable
  DNs — one sharing a leg's location, one not — `CreateSpareLeg` picks the
  distinct-location DN; delete that DN and the same request now places on
  the shared-location one (tier 2), never `RESOURCE_EXHAUSTED`. Both run at
  the default `dn_batch_size`, which is what the (a) trigger fix makes
  satisfiable. The shared-location DN also carries **twice** the free
  extents of any other, so the §6.3 descending walk reaches it first and the
  distinct-location pick cannot be an artifact of index order (amendment
  (f)).
* `worker/reaction_test.go` `TestReactionSpareCreateExcludesGroupLocations`:
  the AR8 spare-create scan passes the group's
  locations (fixture DNs in two named locations; assert the pick).

**§6 edits with this item** (the doc currently *overpromises* strict
exclusion): architecture.md §6.5's CreateMigration bullet — replace "black
list starts with the DNs (and thus locations) of every leg/side of the
group" with the two-tier rule stated above (and the CreateSpareLeg bullet's
"same black-list seeding" inherits it); dnv-worker.md §11.5's AR8 scan
sentence (`black = the DNs of every leg and spare of the group`, :1209)
gains "tier 1 also excludes their locations; a tier-2 rescan without the
location exclusion runs when tier 1 comes up short"; dnv-worker.md §4's
`FindDnCandidates` signature row (:413) gains the parameter and the new
helper; gateway.md §5.10's "black list seeded with the DNs of every
leg/side of the group" and §5.11's "black list = the group's leg/side DNs"
both cite the two-tier rule.

---

## 4. U4 — removed namespaces are parked before their nvmet objects go

**Finding.** cnagent.md CN9's retire order parks ns-devs (reload onto
`CnErrorName`) **before** nvmet removal, and CN21's rationale is that a
suspended device blocks the nvmet disable above it. The code parks only
*surviving* namespaces (`agent/cnagent/syncup_cntlr.go:164-169` iterates
`plan.namespaces`); a namespace **removed** from `ns_list` gets
`RemoveNamespace` (:178-188) while its ns-dev may still be dm-suspended
(§11.6 suspend, or a transfer origin — `DeleteNamespace` has no suspended
precondition), with the resume happening only later inside `removeDm`
(:189-193). The practical wedge is unlikely — the suspend path flushes
in-flight IO and nvmet completes IO to an ANA-inaccessible namespace
without touching the backing device, so a suspended ns-dev should hold no
queued bios — but the code diverges from the stated discipline and relies
on that subtle argument.

**Decision.** Align the code with CN9: every removed namespace (including
those of a removed subsystem) is parked — reloaded onto its td's
`CnErrorName` and resumed — before `removeExport`/`RemoveNamespace` run.

**Code.** In the retire phase of `agent/cnagent/syncup_cntlr.go`, extend
step (2): after the existing survivors loop (:164-169), iterate
`removedNamespaces(old, plan)` and call `s.parkNsDevLogged(ctx, np)` for
each entry whose `np.td != nil` (the old plan supplies `np`; its td's
`CnErrorName` still exists at this point because td teardown runs later in
the same phase — state that ordering fact in the comment). Skip-guard
`np.td == nil` keeps the `removeDm` resume as the backstop for a namespace
that never had a backing td. Update the step-(2) comment at :157-163 —
"Every ns-dev that must stop serving is reloaded onto its dm-error" becomes
literally true — and drop the now-redundant half of the step-(3) comment if
it claims the removed ones were not parked.

**Tests.** `agent/cnagent` unit test
`TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval`: converge A
builds a primary with one namespace whose `Namespace.suspended = true`
(assert the ns-dev ends suspended per CN16); converge B removes the
namespace from `ns_list`; assert the mutation order — the ns-dev's
reload-onto-`CnErrorName` and resume precede the nvmet
`enable=0`/rmdir for that nsid, which precede the ns-dev's `dmsetup
remove`. A second sub-case removes the whole subsystem and asserts the same
park-first order around `removeExport`.

**Integration.** No existing `integtest/cnagent_test.sh` stage removes a
host-facing namespace or subsystem short of full cntlr teardown (the
xfer/clone stages only flip `suspended` and *add* a snapshot ss), and the
suite's only `mutations()` verb-set assertion is Case B's rebuild stage,
which deletes nothing — so U4 changes no event any current stage asserts
and needs **no suite edit**; the unit test above is the MUST. A dedicated
stage (DeleteNamespace of an effectively-suspended namespace, asserting the
park-first order) MAY be added together with a matching
cnagent_integtest.md step; either way re-run the cn suite on the lab pair
(.125/.229) as regression.

---

## 5. U5 — ledger misses are lost invariants, not NOT_FOUND

**Finding.** gateway.md GW7 reserves `NOT_FOUND` for objects the *request*
named, and §5's outlines map a missing invariant key to `ABORTED`
(architecture.md §5.9) —
`gateway/clone.go:242-247` is the reference shape, comment and all. The two
allocator ledgers violate it: `dnLedger.get` (`gateway/alloc.go:181-187`)
and `cnLedger.get` (:319-325) return
`errNotFound("disk node %q not found", …)` for a DN/CN referenced only by a
stored `Side`/`Cntlr` — their own comment says "a missing one is a broken
invariant, not a race". Reachable only on a corrupted store, via the
release paths of DeleteStoragePool, DeleteCntlr, DeleteSpareLeg and
Finish/CancelMigration.

**Decision & code.** Both `get`s return
`errAborted("dn_conf for %q is missing", addrPort)` (resp. `cn_conf`),
each with a one-line comment citing GW7's rule the way clone.go:242-247
does. Grep `gateway/*_test.go` for assertions pinning `NotFound` on these
paths (none are expected) and add one test:
`gateway/handler_node_test.go` (or the sp handler suite)
`TestReleasePathsAbortOnALostConfKey` — build an SP via the normal
handlers, delete one member DN's `dn_conf` key directly through the test's
etcd client, call `DeleteStoragePool`, assert `codes.Aborted` (not
`NotFound`) and that nothing was partially deleted (the STM aborted whole).

---

## 6. Companion-document amendments (land with the named item)

* **U1** — dnv-worker.md AR5 (:1117): the trigger sentence becomes "When
  the primary cntlr is `disabled`, or has `err_epoch != 0` and `now −
  err_epoch ≥ primary_unhealthy`: …" (candidate rule unchanged);
  architecture.md §10.4's `primary_unhealthy` bullet ("the primary cntlr is
  unhealthy ⇒ …") gains "— or the primary is `disabled` (§8.6), with no
  threshold wait —"; dnv-worker.md's AR3/hands-off wording and
  architecture.md §10.4's
  "suppresses all of them … for disabled cntlrs" clause both gain the
  carve-out "a disabled cntlr is never a candidate, replaced or repaired —
  but a disabled *primary* is itself the AR5 failover trigger (§8.6)";
  dnv-worker.md §4 MD6's `Failover` row gains the disabled-old
  precondition and the new reason string. architecture.md §8.6 needs **no
  edit** — grep both §8.6 sentences quoted in §1 above to confirm they
  still read as written.
* **U2** — dnagent.md §6 is a numbered prose inventory, not a test-name
  table: extend its item 11 ("Cutover fence") with the
  adopted-fence-at-the-gate case; DN12 needs no edit (verify by re-reading
  its rules **1 and 4** — restart-adoption-as-elapsed and
  the-gate-never-skips-the-fence — against the new behavior).
* **U3** — listed inline in §3 (architecture.md §6.5, dnv-worker.md §11.5 +
  §4, gateway.md §5.10/§5.11).
* **U4** — cnagent.md CN9's park sentence gains "— the survivors that must
  stop serving *and every removed namespace's* —"; cnagent.md §6 gains the
  new test row; cnagent_integtest.md per §4 above.
* **U5** — gateway.md needs no edit (GW7 already states the rule); confirm
  with a grep that no doc outside this file cites the old
  `"disk node %q not found"` ledger message.

## 7. Acceptance

1. `make build vet` clean; `gofmt -l .` empty;
   `ETCD_BIN=… go test -count=1 ./...` green.
2. Greps (append `| grep -v doc/update_06.md` to each doc/ grep — this
   file deliberately quotes the superseded texts): the exact literal
   `old cntlr is healthy"` (with the closing quote) survives only as
   ReplaceCntlr's own precondition (`model/ops.go` under `opReplaceCntlr`,
   plus its `ops_test.go` pins) — the opFailover carrier and its test pin
   now read `healthy and enabled`;
   `grep -n "disk node %q not found" gateway/alloc.go` hits nothing (scope
   it to the ledger file — the same literal legitimately survives at the
   four request-named sites of `gateway/disknode.go`, and
   `"controller node %q not found"` at `controllernode.go`'s four, all of
   which GW7 requires to stay `NOT_FOUND`; see amendment (c) above);
   `grep -n "fenceRestarted" agent/dnagent/fence.go` shows the extended
   `fenceStarted`; `grep -rn "and thus locations" doc/` hits nothing.
3. Suites, on the lab of record: `cnagent_test.sh` (U4), `dnagent_test.sh`
   (unchanged, regression), `gateway_test.sh` (U3/U5), `worker_test.sh`
   (U1 — its stage 11 is new, amendment (f)). Each U-item's commit message
   cites its id.
