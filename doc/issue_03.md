# issue_03.md — two open questions from implementing `gateway.md`

Status: **resolved** (2026-09-09) — decided in `update_04.md`: I1 for
architecture.md §8.2's reading, with the reply field renamed
`applied_revision` (U1–U6); I2 as implemented, now stated by both specs and
pinned (U7). The companion documents are amended; the code changes are
specified in `update_04.md` and pending, so the "What the code does today"
sections below stay accurate until U1–U7 land.
Both items were found while implementing `doc/gateway.md`
(the 59 RPCs of `gateway/`, `cmd/dnv-gateway/`, `integtest/gatewayctl/` and
`integtest/gateway_test.sh`). Neither blocked the implementation: each was
decided one way, the decision is documented at its call site, and the whole
suite is green with it. Both are recorded here because the decision is
**not** derivable from the specs alone — I1 is a genuine contradiction between
two normative documents, and I2 is a parameter no document mentions — so a
later session must not silently reverse either without reading this.

Item ids are `I1`, `I2`, in the `update_0N.md` `U1`-style so specs can cite
them. Section references are `architecture.md` unless prefixed.

---

## I1 — `Inspect*` replies the DESIRED revision, not the applied one

### The contradiction

`architecture.md` §8.2 (line 1121):

> **InspectDiskNode** — … Action: STM-read `DnConf` for logging ids only;
> then, outside the STM, call `DiskNodeAgent.GetDnInfo` and return its
> `revision` + `DnInfo` (`InspectDiskNodeReply.revision` = **the agent's last
> applied revision**, so callers can diff it against `GetDiskNode`'s desired
> `DnRev.revision`).

`gateway.md` §5.2 (line 375):

> **InspectDiskNode** — STM: read DnConf **+ DnRev (ids and the reply
> `revision`)**; **after** the STM call `GetDnInfo(cluster_id, dn_id)` …

These cannot both hold: one names the number in the agent's reply, the other
names the value of the `dn_rev` key. `gateway.md` says the same thing twice
more, in its integration-test plan:

* §10.11 step 4 — `inspect-dn dn0` → `revision 1` + the fake's DnInfo. The
  suite runs **no dnv-worker**, so no `SyncupDn` ever reaches the fake and its
  applied revision is 0; `1` can only be the freshly created `DnRev`.
* §10.11 step 16 — `inspect-cntlr`/`inspect-side` → `revision` = **current
  `sp_rev`**.

`gateway.md` §0 #2 says `architecture.md` §8 wins for *what* an RPC does, which
argues for the agent's revision; but §5.2's parenthetical, §10.11 step 4 and
§10.11 step 16 are three independent statements of the opposite, and under
§8.2's reading the in-STM `DnRev` read that §5.2 prescribes has no purpose at
all.

### What the code does today

The stored value. Every `Inspect*` fills its reply from the rev key read in
its own resolving snapshot, and the agent reply's `revision` field is received
and discarded — only `GetDnInfo()` / `GetCnInfo()` / `GetCntlrInfo()` /
`GetSideInfo()` is taken from it.

| RPC | reply `revision` | source |
|---|---|---|
| `InspectDiskNode` | `DnRev.revision` | `gateway/disknode.go:451`, replied at `:474` |
| `InspectControllerNode` | `CnRev.revision` | `gateway/controllernode.go:498`, replied at `:525` |
| `InspectCntlr` | `SpRev.revision` | `gateway/cntlr.go:73` (`inspectSpRevision`), replied at `:556` |
| `InspectSide` | `SpRev.revision` | same helper, replied at `:655` |

`grep -n 'GetRevision()' gateway/*.go` confirms it mechanically: every hit is
either a request token (`req.Get*Rev().GetRevision()`) or a rev key read
through the STM. No agent reply's revision is read anywhere in the package.

Pinned by `gateway/handler_node_test.go`
`TestInspectDiskNodeRepliesTheStoredRevision` /
`TestInspectControllerNodeRepliesTheStoredRevision` and by
`gateway/agentpath_test.go` `TestAgentPathInspectRepliesTheStoredRevision`.
The in-process fake answers every `*Info` with `revision = 0xfa5e`
(`gateway/etcdenv_test.go`, `fakeAgentRevision`) — a value no bump sequence
reaches — so a handler that passed the agent's number through would be
unmistakable. The tests assert the reply is the stored value and then bump the
rev key by hand and assert the reply follows it.

### What it costs

`GetDiskNode` already returns `DnRev` as the client's token, so
`InspectDiskNode` currently returns the same number and **a client cannot
diff desired against applied** — which is the one use §8.2 names for the
field. The applied revision is available at the agent and is simply dropped.

The other direction costs the `gateway.md` §10.11 expectations: with no worker
running, an agent that has applied nothing replies `revision 0`, so steps 4
and 16 would assert `0` rather than `1` / the current `sp_rev`. That is still
a meaningful assertion ("the agent has applied nothing yet"), just a different
one.

### Decision taken, and how to reverse it

Implemented per `gateway.md` (decision **D-A** in the implementation brief),
because it is the document this work was commissioned from, it states the rule
three times consistently, and it is the only reading under which §5.2's `DnRev`
read has a purpose. Every call site carries a comment naming the disagreement.

To adopt `architecture.md` §8.2 instead: take `reply.GetRevision()` in the
four handlers listed above (the agent reply is already in hand at each), drop
the now-unused rev-key read from `InspectDiskNode` / `InspectControllerNode`
and `inspectSpRevision` from `gateway/cntlr.go`, and update the three unit
tests plus `gateway_test.sh` case S steps 4 and 16. It is a contained change;
what it is **not** is a change either document currently authorises on its
own.

**Recommendation:** decide it in the documents first. §8.2's rationale
(diffing desired against applied) is the stronger engineering argument and is
the only one that makes the field carry information `GetDiskNode` does not
already give. If that is the intent, `gateway.md` §5.2, §5.3, §5.5 and §10.11
steps 4 and 16 need the correction; if `gateway.md` is the intent, §8.2's
parenthetical needs deleting, and the field should probably be renamed in a
future schema revision, because "revision" next to a `Get*` that returns the
same number invites exactly this confusion.

### Resolution (update_04.md)

Decided for §8.2's reading: the reply is the agent's applied revision, and
the field is renamed `applied_revision` (same field number). The documents
moved first — architecture.md §8.2/§8.3/§8.6 and gateway.md
§5.2/§5.5/§9/§10.11 now agree — and update_04.md U1–U6 specify the reversal
recipe above plus the rename, the inverted test pins and the suite's new
assertions.

---

## I2 — `GrowSlice` disables AR6's pending rule, and no spec says what it should pass

### The gap

`model.GrowSlice` takes a `poolTotal` parameter (`model/ops.go`, the
`GrowSlice` signature) and re-applies AR6's stateless pending rule inside its
STM:

```go
// model.GrowPending
if len(grps) < 2 { return false }
total := Σ data_blocks over all groups of that kind but the LAST
return reportedTotal <= total
```

The rule exists so that a **worker** cannot issue a second grow before the
primary has reported the first: `worker/reaction.go` passes the total the
primary actually reported (`poolTotal(usage, isMeta)`), and AR6 is the memo
reconstructed from that report.

The gateway has no such report. `architecture.md` §8.5 and `gateway.md` §5.4
both specify `GrowSlice` in full — §8.5 lists four error classes and the meta
ladder, §5.4 prescribes the pre-reads, the candidate unit and the amended
`model.GrowSlice(…, expectRev = token)` call — and **neither mentions
`poolTotal` at all**. `gateway.md` §2.2 #3 adds exactly one parameter to the
three shared ops (`expectRev`), leaving `poolTotal` in place with no guidance
for the gateway's call.

### What the code does today

`gateway/storagepool.go:1118` passes `math.MaxUint64`:

```go
newGrpId, err := model.GrowSlice(
    ctx, s.cli, cid, conf.GetShardCode(), conf.GetSpId(),
    req.GetSpName(), req.GetSpRev().GetRevision(),
    req.GetSliceId(), req.GetIsMeta(), math.MaxUint64, cc, picks)
```

`GrowPending` returns `reportedTotal <= total`, so `MaxUint64 <= total` is
false for any real group total: **the rule never fires, and consecutive grows
on one slice are always allowed.** Measured, two back-to-back data grows on
one slice:

| `poolTotal` | grow #1 | grow #2 | data groups |
|---|---|---|---|
| `math.MaxUint64` (today) | OK | OK | 3 |
| `0` | OK | `FAILED_PRECONDITION: GrowSlice: precondition failed: grow_pending` | 2 |

`0` is what a gateway that "has no pool report" would naively pass, and it is
wrong in a way that only shows up on the SECOND grow of a given kind: the rule
is per-kind and the first grow of a kind is never pending (`len(grps) < 2`).
`gateway_test.sh` case S step 7 grows once per kind — one `--meta`, then one
`--ext` — so it passes under either value and cannot tell them apart.

There is currently **no test pinning this either way**, in `gateway/` or in
the integration suite.

### The two readings

* **As implemented.** AR6 is a worker-convergence guard, not a user-facing
  precondition. A user-driven `GrowSlice` is explicit operator intent, the
  gateway holds no pool report to judge "pending" with, and §8.5's error list
  does not include a pending state — so the rule is disabled. Cost: a user
  can stack grows the CN has not yet materialised. §8.5 says the growth is
  "deferred on the CN while the new group still contains a provisioning leg"
  and completes by itself, so stacking is absorbed rather than lost; but the
  DN extents of every stacked group are charged immediately.
* **The alternative.** The gateway reads the primary's reported totals — via
  `GetCntlrInfo`, the same call `DeleteClone` already makes — and passes the
  real number, so a user's second grow is refused with the same `grow_pending`
  the worker sees. Cost: `GrowSlice` gains an agent call and the two-phase
  shape of AG4, which neither §8.5 nor §5.4 gives it, and an unreachable
  primary would then block a grow.

### Decision taken, and how to reverse it

`math.MaxUint64` — i.e. rule off — recorded as decision **D-B** in the
implementation brief and documented at `gateway/storagepool.go:981`. It is a
one-line change with nothing else to update, precisely because nothing pins
it.

**Recommendation:** whichever way it goes, say so in `architecture.md` §8.5
(a sentence on whether AR6 applies to the gateway path) and in `gateway.md`
§5.4 (the argument to pass), and add a unit test for it — the current silence
is the actual defect here, not the value.

### Resolution (update_04.md)

Kept as implemented, and the recommendation is carried out to the letter:
architecture.md §8.5 now says the pending rule binds only the worker's
auto-grow, gateway.md §5.4 names the `math.MaxUint64` argument, and
update_04.md U7 adds the missing pin (two consecutive data grows through the
handler — the second is the discriminator) plus the comment retargets in
`gateway/storagepool.go` and `model/ops.go`.
