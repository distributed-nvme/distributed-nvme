# update_05.md — request trace-id minting, standby ANA verdicts, the thin-id activation sweep, and the meta-cap error class

Status: **decided; companion documents amended; the code changes below are
APPLIED (2026-09-09).** This file resolves the four open
findings of the 2026-09-09 doc-vs-code verification (`minor_issues.md`
header): U1 = trace-id minting (gateway.md §0 #6), U2 = the standby
ana_state probe (cnagent.md CN11/CN28), U3 = the CN14 thin-id delete gate
(the confirmed leak), U4 = the meta-ladder-cap error class (architecture.md
§8.5 vs gateway.md GW7). All four were decided **fix the code** (U2 and U3
with the scoping refinements below); where a doc must also move, §1 gives
the exact edit. Apply each U-item's §1 amendments in the same change as its
code. Item ids `U1`–`U4`, append-only once cited from a spec, a commit or
code. Section references are the named document's own sections; file:line
references are against the 2026-09-09 tree (post that day's amendment
pass) — re-locate by the quoted code if they have drifted.

Amended 2026-09-09 by the pre-implementation review, before any U-item
landed: **(a)** U2's expected state was corrected from `== optimized` to
"`optimized` or `non-optimized`" — the DN exports the non-optimized group
to every non-primary CN by design (`agent/dnagent/plan.go:279-282`), so
the draft predicate would have failed every healthy standby leg, which the
cn suite pins OK (`integtest/cnagent_test.sh:1742-1743`, ana
`non-optimized` at :1774-1779). **(b)** U3 gained the rule that the
startup reconcile arms but never *runs* the sweep: it converges from the
persisted request, which converge-then-persist
(`agent/cnagent/syncup_cntlr.go:42-51`, a failed Save only logged) lets
lag the pool's true contents, and sweeping a lagging `td_list` destroys a
live td. **(c)** §1's former "no change to `ThinDeviceCreated.md` or any
integration suite" claim was wrong: the thinbm drop/rebuild stages
re-create the pool device, so U5-T1 and that stage's blanket no-message
assert narrow to device-set-mutating messages. The U-item texts below are
the amended, authoritative versions.

Shared background an implementer needs: `common/log.go` exports
`NewTraceId()` (8 random bytes, hex), `WithTraceId`, `TraceIdFromCtx`, and
the metadata key `TraceIdMetadataKey = "trace_id"`; `common/interceptor.go`
is a **byte-for-byte copy of grpc.md §3's reference listing, pinned by
tests — do not edit it** (U1 works around it deliberately).
`agent/nvmet.go:18-20` exports
`AnaStateOptimized`/`AnaStateNonOptimized`/`AnaStateInaccessible` — the
three states the DN ever sets.
`agent/cnagent` fakes: `fakenode_test.go` models dm-thin ids
(`thinPools`/`holdThinIds`; `create_snap` rejects an unheld origin, `delete`
removes, a thin-table `dmsetup create` checks the pool) and the bitmap
tests script `thin_dump` output through `common.FakeOsClient.RunCommandFn`
— reuse both patterns. Prefer `node.callsMatching(...)` **counts** over
`indexOfCall`/`assertOrder` when asserting pool messages (both stop at the
first match and cannot see duplicates).

---

## 0. The decisions

### U1 — the gateway mints a trace id for a request that arrives without one

gateway.md §0 #6 already states this ("grpc.md T4's MAY is exercised") and
two code comments assert it as fact (`cmd/dnv-gateway/main.go:179-183`,
`common/log.go:49-51`), but nothing implements it: the shared server
interceptor only *adopts* an incoming id, so an id-less client produces
gateway/etcd/agent log chains with no `trace_id` and forwards none to
agents. Fix the code; the docs become true. Mechanism constraint: the mint
must happen **upstream of the shared chain** (so the shared interceptor's
own request/reply records carry the id) and **without touching
`common/interceptor.go`** — a small gateway-local interceptor injects the
id into the *incoming metadata*, and the shared chain then adopts it as "a
request that arrived with one".

### U2 — an unpromotable `ana_state` fails the standby leg row, on single-sided legs only

cnagent.md CN11 ("a live controller per desired side with the expected
ana_state"), the CN28 `leg_id_to_leg` row, cnagent.md §5's amendment bullet
and architecture.md §9.5 all claim an ana_state comparison;
`transportHealth` (`agent/cnagent/leg.go:404-441`) checks only
`ctrl.state != "live"` and renders the observed ana_state into details
without judging it — a live-but-inaccessible standby path reports
`RES_STATUS_OK` although it cannot serve a promote. Fix the code, with two
scoping rules the docs must gain:

1. The judgment applies **only to a leg with exactly one desired side**. A
   leg whose `side_list` holds two sides is mid-migration (CN10) and its
   ANA is wrong-by-design for hours (dst inaccessible until cutover, src
   after), with the phase known only to the DN — such legs keep the
   live-only check.
2. The healthy set is **`optimized` or `non-optimized`** — not `optimized`
   alone (the pre-review draft required `optimized`, which would have
   failed every healthy standby leg). `transportHealth` is reached only
   when `!plan.primary` (leg.go:389-399), and the DN grants the optimized
   group to the primary CN alone: every other CN's path sits in the
   non-optimized group over a dm-error backing
   (`agent/dnagent/plan.go:279-282`, `linearBacking` ~:243-244), so a
   standby leg's healthy steady state *is* `non-optimized` — the redund
   lab case pins exactly that (leg rows OK,
   `integtest/cnagent_test.sh:1742-1743`; ana `non-optimized`,
   :1774-1779). `optimized` appears on a standby only in the post-flip
   pre-promote window (the `leg_wait_ana … optimized` barrier, :1831-1832,
   waits for it), and `transportHealth` cannot know the promote phase, so
   both states pass. What fails the row is an *unpromotable* path:
   `inaccessible` (the DN parked or handed the side over) or any state the
   DN never sets (`change`, `persistent-loss`, garbage).

With that scoping, `RES_STATUS_ERROR` is safe end-to-end: `err_epoch`
semantics absorb a transient `inaccessible` read during a cutover's ~15 ms
`ana_grpid` rewrites (the threshold clock only starts, reactions fire only
past `leg_unhealthy`, default 1200 s), and a *persistent* unpromotable
path correctly ages toward AR8 leg repair.

### U3 — thin-id delete: keep the `wantPool` gate, add a sweep armed by pool creation

The confirmed leak: a td leaving `td_list` gets its per-slice
`delete {dev_id}` pool message only under `plan.wantPool`
(`agent/cnagent/syncup_cntlr.go:213-225`), `deleteThinId` silently returns
when the local pool device is absent (`agent/cnagent/pool.go:261-276`), and
`convergeCntlr` runs retire **before** build (`syncup_cntlr.go:74-75`) — so
in the one fan-out that both removes the td and moves/suppresses the pool
(demote+delete coalesced by RW3, delete at `sp_level ≥ NO_THINPOOL`, a
failover+delete race with the old primary dead), *nobody* sends the delete:
the old primary is gated, the new primary's pool does not exist yet at its
retire step, and afterwards every cntlr's `st.applied` has forgotten the td
forever. The dev_id and its mapped blocks then sit in the on-leg pool
metadata with no owner — invisible to `CntlrInfo`, charged against the
pool, skewing AR6's usage view.

The gate itself is correct (a standby has no pool device; only the primary
may write pool metadata) — do **not** remove it. The fix is a **sweep
armed by pool creation, run under an RPC-delivered request**: the converge
whose `ensurePool` *creates* the pool device marks the slice
sweep-pending, and the sweep itself — enumerate the pool's actual device
ids (the CN25 machinery: `reserve_metadata_snap` → `thin_dump` → parse →
`release_metadata_snap`; `parseThinDump` already yields `Devices[].DevId`)
and send `delete` for every id not in the request's `td_list` dev_id set —
runs in the first converge whose request arrived through the
revision-gated RPC: usually the very same converge, after a reboot the
first post-boot `SyncupCntlr`. Why "on creation" and not "at startup" or
"every converge", and why the startup reconcile only arms:

1. At process start the agent may be a standby — there is no pool device to
   enumerate; the metadata lives on the DN legs and activates only under
   the primary's pool target.
2. Every leak path above **ends the pool device's life** on the cntlr that
   skipped the delete, so the stray is caught exactly at the next converge
   that creates the device, wherever that happens (promotion, level
   restore, a reboot — swept at the first post-boot sync, point 4). Within
   one device lifetime the agent is the only writer, so no repeated
   checking is needed — the existing `removedTds` event diff stays the
   steady-state mechanism.
3. An agent **restart under a surviving pool device must not sweep**: no
   strays can have appeared while the only writer was down, and the CN2/SH16
   invariant — a re-converge on a converged node issues zero mutating calls
   — is pinned by the cn suite's case D (`mutations()` counts
   `dmsetup message`). Arming on the Create branch only preserves it. The
   one lab stage that *does* re-take the Create branch is the thinbm
   drop/rebuild (`integtest/cnagent_test.sh:2043`, :2058) — and there the
   sweep is supposed to fire: the CN21 teardown sends no `delete`s, so a
   td dropped while the cntlr was down is healed exactly at the rebuild.
   That stage's blanket `^dmsetup message` absence assert, and the
   ThinDeviceCreated.md U5-T1 clause it enforces, narrow to
   device-set-mutating messages (§1, §4's lab-suite block; the pre-review
   draft wrongly claimed no suite change).
4. A **startup reconcile never runs the sweep**, even when it re-creates
   the pool device (after a node reboot the dm state is empty and SH1
   rebuilds everything, Create branch included). Its request is the
   persisted copy, and `syncupCntlr` converges *first*, persists *after*,
   and only logs a failed `Save` (`agent/cnagent/syncup_cntlr.go:42-51`) —
   a crash between converge and Save has the same effect — so that copy
   can lag the pool's true contents. Sweeping a lagging `td_list` deletes
   a live created td's thin id, which U4-S2 then surfaces as a permanent,
   never-re-messaged `RES_STATUS_ERROR`: data loss, where the same
   staleness without the sweep is benign (the td is merely unbuilt until
   the sp-worker's next sync rebuilds it). The startup Create therefore
   only **arms** `pendingSweep`; the first revision-gated `SyncupCntlr`
   runs the sweep with the fresh `td_list`, keeping point 2's reboot
   heal-point.

Sweeping against an RPC-delivered request is always correct:
`GateRevision` makes it the newest desired state this cntlr has ever
accepted, `td_list` is authoritative, dev_ids are never reused
(`SpConf.next_dev_id`), thin ids belong exclusively to tds (clones and
transfers own none), and DN-side fencing leaves no other writer — so a
pool id absent from that `td_list` can only belong to a td deleted at or
before that revision. The persisted copy carries no such guarantee (point
4). Bounded residual, accepted for v1 and stated in CN14: a stray created
by a `delete`-message failure (already logged Error) or by a crash between
pool creation and sweep completion survives until that pool's next
rebuild.

### U4 — the meta ladder cap is `FAILED_PRECONDITION`

architecture.md §8.5 says `FAILED_PRECONDITION`; gateway.md GW7/§5.4 and
the code say `RESOURCE_EXHAUSTED` — and gateway.md §0 #2's own precedence
rule says §8.5 wins. §8.5 is also right on the merits: the 16 GiB cap is a
permanent per-SP structural ceiling that no added hardware or retry fixes —
per-object state, not exhaustible capacity. The principle to record in GW7:
`RESOURCE_EXHAUSTED` = capacity or quota that could be freed or extended
(DN/CN extents, candidates, count ceilings); `FAILED_PRECONDITION` = the
object's own state forbids the operation, the meta ladder cap included.
Bonus: the change **removes** a special case — the cap is an explicit
`errExhausted` carve-out in the gateway pre-check
(`gateway/storagepool.go:1064-1073`, comment acknowledging the conflict),
while `model.GrowSlice`'s in-STM re-check of the same cap
(`model/ops.go:1203-1206`, `fail(opGrowSlice, "meta ladder at the 16 GiB
cap")` = `ErrPrecondition`) already maps to `FAILED_PRECONDITION` through
GW7's generic row — today the same condition returns two different codes
depending on a race window. No model change; no suite change (the
integration suite never asserts the cap's code — case S stage 7 grows
within the ladder).

---

## 1. Companion documents (amend together with the matching U-item's code)

* **gateway.md §3** (U1) — append to the GW2 bullet: "The server chain is
  the gateway-local `ensureTraceId` interceptors followed by the §4 chains:
  when the incoming metadata carries no `trace_id`, the first interceptor
  injects `common.NewTraceId()` into the incoming metadata, so §0 #6's mint
  happens upstream of the shared interceptors and
  `common/interceptor.go` stays the grpc.md §3 reference verbatim
  (update_05.md U1)."
* **gateway.md §4 GW7** (U4) — in the `RESOURCE_EXHAUSTED` row (line ~320)
  delete "; meta ladder cap"; in the `FAILED_PRECONDITION` row append
  "(the meta ladder cap included — update_05.md U4)"; after the table add:
  "The dividing line: `RESOURCE_EXHAUSTED` is capacity or quota that could
  be freed or extended (extents, candidates, count ceilings);
  `FAILED_PRECONDITION` is the object's own state forbidding the
  operation."
* **gateway.md §5.4 GrowSlice** (U4, line ~481) — replace "Plain pre-reads
  for planning (slice's current meta total for `model.MetaLadderExtCnt` —
  cap reached ⇒ `RESOURCE_EXHAUSTED` — and the §6.5 black-list seed);"
  with "Plain pre-reads for planning (the slice's current meta total for
  `model.MetaLadderExtCnt` — cap reached ⇒ `FAILED_PRECONDITION`,
  update_05.md U4 — and the request's own black list, which seeds the §6.5
  scan empty-plus-request-entries, growing only with this group's picks);"
  — the second half also retires `minor_issues.md` DR14's gateway.md half
  (strike it there; DR14's architecture.md §6.5 half stays open).
* **cnagent.md CN11** (U2, the sentence at ~:562-566) — replace "the sysfs
  walk of §5 shows a live controller per desired side with the expected
  ana_state." with "the sysfs walk of §5 shows a live controller per
  desired (provisioned) side and, **for a leg with exactly one desired
  side**, an `ana_state` of `optimized` or `non-optimized` —
  `non-optimized` is a standby path's designed steady state (the DN grants
  the optimized group to the primary CN alone) and `optimized` the
  pre-promote window after the DN's flip, while `inaccessible`, or any
  state the DN never sets, cannot serve a promote and reports
  `RES_STATUS_ERROR`. A leg whose `side_list` holds two sides (a migration,
  CN10) is checked for liveness only: its ANA is wrong-by-design for the
  hydration and the phase is the DN's knowledge, not this CN's
  (update_05.md U2)."
* **cnagent.md CN28** (U2, the `leg_id_to_leg` row) — change "transport +
  ana_state per desired side, from sysfs (standby; §5)" to "transport per
  desired side, from sysfs, plus `ana_state` in {`optimized`,
  `non-optimized`} on single-sided legs — two-sided legs liveness only
  (CN11, update_05.md U2) (standby; §5)".
* **cnagent.md CN14** (U3) — after "then `delete {dev_id}` message per
  slice pool." insert: "The message is sent only while this cntlr holds the
  pool (`plan.wantPool`, and the device present): a standby has no pool
  device and only the primary may write pool metadata. The fan-outs where
  that skips every cntlr (demote+delete coalesced into one revision, a
  delete at a pool-suppressing `sp_level`, a failover+delete race) are
  healed by the **activation sweep**: creating a slice's pool device arms
  the sweep, and the first converge whose request arrived by the
  revision-gated RPC then enumerates the pool's device ids through the
  CN25 reserve → `thin_dump` → release machinery and deletes every id not
  in `td_list`'s dev_ids — correct because the RPC request is the newest
  accepted desired state, dev_ids are never reused and thin ids belong to
  tds alone. An agent restart under a surviving pool device does not sweep
  (nothing else writes the pool, and the CN2 zero-mutation reconcile stays
  intact), and a startup reconcile that *re-creates* the device (a node
  reboot) arms but does not run it: it converges from the persisted
  request, which may lag the pool's true contents (the converge runs
  before the persist and a failed persist is only logged), and deleting
  against a lagging `td_list` would destroy a live td — the first
  revision-gated `SyncupCntlr` after boot runs the sweep instead. A sweep
  failure is logged and retried on later converges until it succeeds
  once; a stray surviving a crash between creation and sweep, or a failed
  `delete` message, is collected at the pool's next rebuild (update_05.md
  U3)."
* **cnagent.md CN25** (U3) — add: "The activation sweep of CN14 shares this
  reservation machinery and its `CmdSoftTimeout` bound — metadata too large
  to dump inside the bound fails the sweep the same way it fails a bitmap
  read, and the sweep retries on later converges."
* **cnagent.md §6** (U2/U3) — the test list gains: the `transportHealth`
  ana table (single-sided unpromotable-ANA ⇒ ERROR, `non-optimized` ⇒ OK;
  two-sided ignores ANA) and the six sweep tests of §4 below.
* **ThinDeviceCreated.md U5-T1** (U3, the clause at :541-543) — replace
  "`mutations()` over the rebuild syncup's trace contains `dmsetup
  create` lines and no `dmsetup message` line." with "`mutations()` over
  the rebuild syncup's trace contains `dmsetup create` lines and, of
  `dmsetup message`, exactly the activation sweep's
  `reserve_metadata_snap`/`release_metadata_snap` pair — no `create_thin`,
  no `create_snap`, no `delete` (update_05.md U3: the rebuild re-creates
  the pool device, a designed sweep trigger — the drop's teardown sent no
  deletes, so a td removed while the cntlr was down is healed exactly
  here; the bitmap proof below is what shows the bare creates attached
  the existing ids)." U4-S2 is untouched: a created td *in `td_list`* is
  never messaged; the sweep deletes only ids **not** in `td_list`.
* **integtest/cnagent_test.sh** (U3, the thinbm rebuild stage,
  ~:2081-2089) — the blanket `assert_absent "$muts" "^dmsetup message"`
  narrows to device-set-mutating messages and pins the sweep's
  reserve/release pair; §4's "Lab suite" block is the exact replacement,
  applied in the same change as the sweep.
* **minor_issues.md** — header: the four A-items are now "specified in
  update_05.md"; strike DR14's gateway.md half when U4 lands.
* **No change** to `architecture.md` (§8.5 and §9.5 already state the
  U4/U2 end-states; §9.5's "expected ana_state" is defined by CN11),
  `dnagent.md`, `dnv-worker.md`, `cdc.md`, `log.md`, `osclient.md`,
  `grpc.md` (U1 leaves `common/interceptor.go` byte-identical to §3), or
  `layout.md` (this file is already in the §2 tree). The integration
  suite changes in exactly one place (the thinbm rebuild assert above —
  case D's zero-mutation restart assert stays true by U3 point 3), and
  `ThinDeviceCreated.md` in exactly one clause (U5-T1 above).

---

## 2. U1 — gateway trace-id minting

**New file `gateway/traceid.go`** (package gateway):

```go
// ensureTraceIdCtx returns ctx whose INCOMING metadata carries a trace_id,
// minting one when absent (gateway.md §0 #6, grpc.md T4's MAY). Injecting
// into the metadata — not the ctx value — upstream of the shared chain is
// deliberate: common's interceptor then adopts it exactly as "a request
// that arrived with one", its own request/reply records carry the id, and
// common/interceptor.go stays the grpc.md §3 reference verbatim
// (update_05.md U1).
func ensureTraceIdCtx(ctx context.Context) context.Context {
    md, ok := metadata.FromIncomingContext(ctx)
    if ok {
        if vals := md.Get(common.TraceIdMetadataKey); len(vals) > 0 &&
            vals[0] != "" {
            return ctx
        }
        md = md.Copy()
    } else {
        md = metadata.MD{}
    }
    md.Set(common.TraceIdMetadataKey, common.NewTraceId())
    return metadata.NewIncomingContext(ctx, md)
}
```

plus `ensureTraceIdUnary() grpc.UnaryServerInterceptor` (calls the handler
with `ensureTraceIdCtx(ctx)`) and `ensureTraceIdStream()
grpc.StreamServerInterceptor` with a `struct{ grpc.ServerStream; ctx
context.Context }` wrapper overriding `Context()` (the same shape common's
client stream wrapper uses at `common/interceptor.go:226-228`). The Gateway
service has no streaming RPCs today; the stream half is wired for symmetry
with the §4 table.

**`gateway/server.go`** (~:106-109) — the chains gain the new interceptors
FIRST:

```go
grpcServer := grpc.NewServer(
    grpc.ChainUnaryInterceptor(
        ensureTraceIdUnary(), common.GrpcUnaryServerInterceptor()),
    grpc.ChainStreamInterceptor(
        ensureTraceIdStream(), common.GrpcStreamServerInterceptor()),
)
```

Factor the `[]grpc.ServerOption` into an unexported helper
(`serverOptions()`) so the U1 test builds a server with exactly the
production chain.

**Comment retarget** — `cmd/dnv-gateway/main.go:179-183`: "gateway.Run's
handlers mint one otherwise" → "gateway.Run's server chain mints one
otherwise (gateway/traceid.go)". `common/log.go:49-51` ("dnv-gateway for a
request that arrived without one") becomes true as written — no edit.

**Tests** (`gateway/traceid_test.go` or extend `agentpath_test.go`; the
agent-path env already runs `agentpathFake` on a real listener whose
addr_port is seeded into etcd):

1. Extend `agentpathFake` to record, per call, the incoming-metadata
   `trace_id` (a `traceIds []string` field appended in `GetDnInfo`).
2. `TestServerMintsTraceIdWhenAbsent` — serve the gateway on `bufconn`
   with `serverOptions()`; dial with a **raw** client (no client
   interceptors, no metadata); call `InspectDiskNode` for a DN seeded at
   the fake's addr_port. Assert the fake recorded exactly one trace id, 16
   lowercase hex chars.
3. Subtest `provided`: same call with
   `metadata.AppendToOutgoingContext(ctx, common.TraceIdMetadataKey,
   "cafe0123deadbeef")` — assert the fake saw exactly that id (never
   overridden).

---

## 3. U2 — `transportHealth` judges `ana_state` on single-sided legs

**`agent/cnagent/leg.go`** — in `transportHealth` (~:404-441), the
per-side loop currently ends with:

```go
reports = append(reports, fmt.Sprintf("%s:%s %s/%s",
    tr.GetTrAddr(), tr.GetTrSvcId(), ctrl.state, ctrl.anaState))
if ctrl.state != "live" {
    ok = false
}
```

becomes:

```go
report := fmt.Sprintf("%s:%s %s/%s",
    tr.GetTrAddr(), tr.GetTrSvcId(), ctrl.state, ctrl.anaState)
switch {
case ctrl.state != "live":
    ok = false
case len(lp.sides) == 1 &&
    ctrl.anaState != agent.AnaStateOptimized &&
    ctrl.anaState != agent.AnaStateNonOptimized:
    // U2: a single-sided leg must hold a promotable path. non-optimized
    // is the standby's designed steady state (the DN grants optimized to
    // the primary CN alone) and optimized the pre-promote window, so
    // both pass; inaccessible — or a state the DN never sets — cannot
    // serve a promote and fails the row. A two-sided leg (a migration,
    // CN10) is exempt: its ANA is wrong-by-design for the hydration and
    // the phase is the DN's knowledge, not this CN's.
    report += " unpromotable"
    ok = false
}
reports = append(reports, report)
```

The `!side.GetProvisioned()` continue above it (the U4 rule) is untouched
— a single non-provisioned side never reaches the check. Rewrite the
function's doc comment to state the rule + exemption, citing U2.
`agent/cnagent/leg.go:83-93` (`available()`, `live && optimized`) is
deliberately untouched and is **not** this rule: it is the primary's
§11.1.1 assembly-time gate answering "is this path usable now" — which
`non-optimized` rightly fails — while the standby report answers "could
this path serve a promote" — which `non-optimized` rightly passes.

**Tests** (`agent/cnagent`, next to the existing `transportHealth`
coverage; it is a pure function — table-driven):
`TestTransportHealthAnaState` with cases: single-sided live+optimized ⇒
OK; single-sided live+non-optimized ⇒ OK (the standby steady state — the
redund lab shape, cnagent_test.sh:1742/:1774-1779); single-sided
live+inaccessible ⇒ ERROR, details contain "live/inaccessible
unpromotable"; single-sided live+"change" ⇒ ERROR (a state the DN never
sets is unpromotable); two sides both live, one optimized one
inaccessible ⇒ OK; existing missing-controller and not-live cases
unchanged. Grep the existing tests for any fixture that pins a
single-sided `inaccessible` path OK and flip it citing U2 (a
`non-optimized` ⇒ OK fixture is correct and stays).

---

## 4. U3 — the thin-id activation sweep

**State** — `cntlrState` (`agent/cnagent/server.go:84-100`) gains:

```go
// pendingSweep marks slices whose pool device THIS incarnation created
// but has not yet successfully swept for orphan thin ids (CN14's
// activation sweep, update_05.md U3). Keyed by slice_id; in-memory only —
// a crash in the window leaves a stray for the pool's next rebuild. The
// startup Reconcile's Create branch arms it too; running additionally
// requires reqFromRpc.
pendingSweep map[uint64]bool
// reqFromRpc is true once st.req was delivered by a revision-gated
// SyncupCntlr in THIS incarnation. The startup Reconcile converges from
// the persisted copy, which converge-then-persist (syncupCntlr saves
// after convergeCntlr and only logs a failed Save) lets lag the pool's
// true contents — the sweep deletes ids absent from td_list, so it only
// ever trusts an RPC-delivered one (update_05.md U3).
reqFromRpc bool
```

both initialized in `newCntlrState` (`reqFromRpc` false); `syncupCntlr`
sets `reqFromRpc = true` beside `st.req = req`
(`agent/cnagent/syncup_cntlr.go:39`) — the startup `Reconcile`, which
builds states through `newCntlrState` alone, never sets it.

**Arming** — `ensurePool` (`agent/cnagent/pool.go:124-148`): the branch
that **creates** the device (`dev == nil` → `s.dm.Create(...)`) sets
`st.pendingSweep[sp.sliceId] = true` on success (thread the `st` or return
a `created bool` to the caller, whichever fits the current signature with
less churn). The Reload and probe-matched branches arm nothing — that is
the whole restart-safety argument (§0 U3 point 3). The startup
reconcile's Create arms exactly like any other — the reboot deferral
lives at the run site, not here (§0 U3 point 4).

**The sweep** — new func in `agent/cnagent/pool.go`:

```go
// sweepThinIds is CN14's activation sweep (update_05.md U3): enumerate the
// armed pool's device ids and delete every id no td of td_list owns.
// Runs at most once per pool-device creation, only under an
// RPC-delivered request (the call site's reqFromRpc gate); a failure
// leaves pendingSweep set so a later converge retries.
func (s *CnAgentServer) sweepThinIds(
    ctx context.Context,
    st *cntlrState,
    plan *cntlrPlan,
    sp *slicePlan,
) {
    sb, err := s.dumpThinMetadata(ctx, sp) // CN25: reserve→dump→release
    if err != nil { log "thin id sweep failed" (pool, error); return }
    var strays []uint32
    for _, dev := range sb.Devices {
        if plan.tdByDevId[dev.DevId] == nil {
            strays = append(strays, dev.DevId)
        }
    }
    for each stray: s.dm.Message(ctx, sp.poolFinalName, 0,
        fmt.Sprintf("delete %d", id))
        — any failure: log "thin id sweep failed" and return (flag stays)
    delete(st.pendingSweep, sp.sliceId)
    log "thin id sweep" (pool, deleted list — may be empty)
}
```

Do **not** reuse `deleteThinId` here: it swallows errors by design (CN14's
fire-and-forget retire), while the sweep must know a delete failed so the
flag survives for a retry. Add the two msg strings as constants beside the
existing ones.

**Call site** — in `build()` (`agent/cnagent/syncup_cntlr.go:353…`),
immediately after the CN13 per-slice pool ensure succeeds and **before**
the U4 snapshot pre-pass (the only `createSnapId` caller): for each
`slicePlan` with `plan.wantPool`, `if st.reqFromRpc &&
st.pendingSweep[sp.sliceId] { s.sweepThinIds(ctx, st, plan, sp) }`. The
`reqFromRpc` half is §0 U3 point 4: a startup reconcile arms and skips;
the first post-boot `SyncupCntlr` finds the flag still set (its own
`ensurePool` probe-matches the surviving device and arms nothing) and
sweeps with the fresh `td_list`. Deleting before creating also frees
space; a stray can never collide with a new id (never reused).

**Disarming on teardown** — delete the slice's `pendingSweep` entry
wherever the pool device is removed: the retire step (7) `retiredSlices`
loop (`syncup_cntlr.go:235-239`) and `teardownCntlrResources` (clear the
map). (Re-creation re-arms anyway; clearing just keeps the map from
carrying dead slices.)

**Lab suite** — `integtest/cnagent_test.sh`, thinbm rebuild stage
(~:2081-2089; §1's suite bullet): the rebuild re-creates the pool device,
so the sweep fires there by design (§0 U3 point 3). Replace

```sh
	# U5-T1, from the other side: scoped to this stage's trace, the converge
	# activates dm devices and issues no `dmsetup message` at all. The
	# event stream above already says so; this says it through the §9 helper
	# every other "what did that converge actually change" assertion uses.
	muts=$(helper "$cn" "mutations $TRACE")
	[ -n "$(event_line "$muts" '^dmsetup create ')" ] ||
		die "thinbm rebuild: the converge activated no dm device"
	assert_absent "$muts" "^dmsetup message" \
		"thinbm rebuild: the converge mutated pool metadata"
```

with

```sh
	# U5-T1 as amended by update_05.md U3: the rebuild re-creates the pool
	# device, so the activation sweep runs — its reserve/release pair is
	# the only `dmsetup message` traffic, and nothing changes the device
	# set (no create_thin, no create_snap, no delete; the bitmap proof
	# below is what shows the bare creates attached the existing ids).
	muts=$(helper "$cn" "mutations $TRACE")
	[ -n "$(event_line "$muts" '^dmsetup create ')" ] ||
		die "thinbm rebuild: the converge activated no dm device"
	assert_absent "$muts" "^dmsetup message .* delete" \
		"thinbm rebuild: the sweep deleted a live id"
	assert_eq "$(event_cnt "$muts" '^dmsetup message .* reserve_metadata_snap$')" 1 \
		"thinbm rebuild: one sweep reservation"
	assert_eq "$(event_cnt "$muts" '^dmsetup message .* release_metadata_snap$')" 1 \
		"thinbm rebuild: one sweep release"
```

(the stage's earlier `create_thin`/`create_snap` absence asserts over
`$seq` already cover the other mutating messages, and the `^dmsetup
suspend` absence still holds — the sweep suspends nothing).

**Tests** (`agent/cnagent`; script `thin_dump` via `RunCommandFn` exactly
as the bitmap tests do; assert pool messages by `callsMatching` counts):

1. `TestRetireSkipsThinDeleteWithoutPool` — the long-missing CN14 pin: a
   primary converge holding td X, then one converge that both demotes to
   standby and drops X ⇒ **zero** `delete` messages, pool devices removed.
2. `TestActivationSweepDeletesStrays` — a standby→primary converge (pool
   Create branch); scripted dump lists devices {stray S, live td dev_id Y}
   ⇒ exactly one `delete S`, zero `delete Y`; `reserve_metadata_snap`
   before `thin_dump --metadata-snap` before `release_metadata_snap`; a
   second identical converge runs **no** `thin_dump` (count stays 1).
3. `TestRestartDoesNotSweep` — reconcile/converge over an already-existing
   pool device (probe-matched, no Create) ⇒ zero `thin_dump`, zero
   `reserve_metadata_snap` — the case-D zero-mutation invariant.
4. `TestSweepFailureRetries` — first converge scripts a `thin_dump` error
   ⇒ no deletes, converge rows unaffected (pool row still OK); next
   converge scripts success ⇒ sweep completes.
5. `TestSweepSurvivesDeleteFailure` — scripted dump with stray, `delete`
   message scripted to fail ⇒ flag stays set; next converge retries and
   succeeds.
6. `TestStartupReconcileDefersSweep` — the §0 point 4 data-loss pin: seed
   the store with a persisted primary request whose `td_list` holds only
   td A (`created` true); no pool device exists (the reboot shape); the
   scripted dump lists {A's dev_id, B's dev_id, stray S} and the fake
   pool's held ids include A and B so the bare creates attach.
   `Reconcile` ⇒ the pool device is created, **zero**
   `reserve_metadata_snap`/`thin_dump` calls, zero `delete` messages
   (armed, not run). Then a `SyncupCntlr` at a higher revision whose
   `td_list` holds A and B (both `created`) ⇒ exactly one `thin_dump`,
   exactly one `delete S`, and **no** `delete` for B's dev_id — the id
   the stale persisted copy had forgotten. (A sweep wrongly run at
   startup deletes B; a flag wrongly dropped never deletes S.)

---

## 5. U4 — meta ladder cap → `FAILED_PRECONDITION`

**`gateway/storagepool.go:1064-1073`** — replace the carve-out:

```go
ladder, ok := model.MetaLadderExtCnt(total, extentSize)
if !ok {
    // The 16 GiB dm-thin metadata cap is the SP's own permanent ceiling —
    // object state, not exhaustible capacity — so it is
    // FAILED_PRECONDITION (architecture.md §8.5, gateway.md GW7;
    // update_05.md U4 resolved the old GW7-vs-§8.5 conflict this way, and
    // model.GrowSlice's in-STM re-check already maps there).
    return errPrecondition(
        "slice %d has %d meta extents and cannot grow past the "+
            "16 GiB dm-thin metadata cap",
        req.GetSliceId(), total)
}
```

(`errExhausted` → `errPrecondition`, comment replaced; the message text is
unchanged.) No model change: `model/ops.go:1203-1206` already returns
`ErrPrecondition` for the same condition.

**Test** — `gateway/handler_sp_test.go` `TestGrowSliceMetaLadderCap`
(:2023-2054): flip `sptWantCode(t, err, codes.ResourceExhausted)` →
`codes.FailedPrecondition`; add a header sentence citing U4. Everything
else in the test (no bump, no key change) stands.

---

## 6. Acceptance

1. `make build`; `go vet ./...` and `gofmt -l .` clean;
   `common/interceptor.go` is byte-identical to before U1
   (`git diff common/interceptor.go` empty).
2. `ETCD_BIN=integtest/bin/cache/etcd-v3.6.14-linux-amd64/etcd go test
   -count=1 ./gateway/` green, including `TestServerMintsTraceIdWhenAbsent`
   (runs, not skips) and the flipped `TestGrowSliceMetaLadderCap`.
3. `go test -count=1 ./agent/cnagent/` green, including
   `TestTransportHealthAnaState` and the six U3 sweep tests.
4. Grep audits: `grep -n errExhausted gateway/storagepool.go` shows no
   meta-cap hit; `grep -n "meta ladder cap" doc/gateway.md` shows only
   `FAILED_PRECONDITION` contexts; `grep -rn NewTraceId gateway/` hits
   `traceid.go` (and tests) only; `grep -n pendingSweep
   agent/cnagent/*.go` hits server.go, pool.go, syncup_cntlr.go and tests;
   `grep -n reqFromRpc agent/cnagent/*.go` hits server.go,
   syncup_cntlr.go and tests; `grep -n unpromotable agent/cnagent/leg.go`
   hits the U2 branch.
5. §1's companion amendments are in the same change as their U-item;
   `minor_issues.md`'s header points here and DR14's gateway.md half is
   struck.
6. Lab (when next run): `bash integtest/cnagent_test.sh …` prints `PASS`
   with §4's thinbm amendment applied — case D's restart stage is the
   live proof a surviving pool device never sweeps, the thinbm rebuild
   stage the live proof the sweep fires (and deletes nothing) on a true
   pool re-creation, and the redund standby assertions (:1742-1743,
   :1774-1779 — leg rows OK with ana `non-optimized`) the live proof of
   U2's predicate; `bash integtest/gateway_test.sh …` still passes
   unchanged (nothing asserts the meta-cap code or trace-id absence).
