# update_04.md — `Inspect*` replies the agent's applied revision, and the gateway's `GrowSlice` `poolTotal` is settled

Status: **decided; companion documents amended; the code changes below are
APPLIED.** This file resolves both items of `issue_03.md`. Item ids are
`U1`–`U7` in the `update_0N.md` style, append-only once cited from a spec or
from code. The companion specs already state the end-state and cite this
file (§1), so until U1–U7 are applied the code — which still implements
issue_03.md's interim decisions — deliberately trails the documents; that
window is this file's reason to exist. Section references are `gateway.md`
unless prefixed.

---

## 0. The decisions

### I1 — resolved for architecture.md §8.2: the reply carries the agent's last APPLIED revision, and the field is renamed `applied_revision`

Of issue_03.md I1's two readings, §8.2's wins, for three reasons:

1. **It is the only reading where the field carries information.**
   `GetDiskNode` / `GetControllerNode` / `GetStoragePool` already return the
   stored rev as the client's token; an `Inspect*` that repeats it adds
   nothing, while the applied revision is the one number no other RPC
   exposes — today it arrives in every agent reply and is discarded.
2. **It makes the reply one coherent snapshot.** Under the interim decision
   the reply paired a revision from etcd (desired state) with info from the
   agent (actual state), separated by the whole worker-convergence lag and
   inviting the misreading "this info is the state at that revision". Both
   fields from one agent reply is the only pairing whose juxtaposition means
   something: what the agent reports, and the revision at which it reports
   it. The operator workflow follows directly: mutate, then watch `Inspect*`
   until `applied_revision` catches up with the token.
3. **The plumbing exists end to end and no client depends on the old
   meaning.** All four agent `Get*InfoReply` messages already carry
   `revision` (from the last `Syncup*` the agent applied), and `dnvctl` does
   not exist yet (§0 #1) — the only consumers are the tests. The semantics
   flip is at its cheapest now.

The rename (`revision` → `applied_revision`, same field number, U1) removes
the ambiguity that produced I1 in the first place: a reply field called
`revision` sitting in an API whose request tokens are also revisions.

### I2 — resolved as implemented: AR6's pending rule is OFF on the gateway path; the defect was the silence, and the silence is fixed

The "one grow per pool at a time" rule is defined as the **sp-worker's**
auto-grow rule (architecture.md §10.4, dnv-worker.md §0 #14 / §11.3 AR6),
judged by the primary's *reported* usage. The user-facing GrowSlice spec
(architecture.md §8.5) lists no pending precondition, and a user-driven grow
is explicit operator intent with no report to judge "pending" by — so the
gateway passing `poolTotal = math.MaxUint64` (rule never fires) is the
spec-consistent reading, not a coin flip. The alternative issue_03.md
describes (a `GetCntlrInfo` pre-read) is worse than it looks: besides the
unauthorised AG4 two-phase shape and an unreachable primary blocking grows,
a one-shot report is a point-in-time value the worker's converging loop
tolerates being stale but a one-shot user refusal does not.

The exposure of stacked grows is bounded: `model.GrowSlice` recomputes the
meta ladder in-STM from the *stored* groups (stacked ones included), so
stacking can never pass the 16 GiB meta cap; §8.5's deferred growth absorbs
stacked data groups. The only cost is DN extents charged for groups the CN
has not yet materialised — visible in `GetStoragePool` and reversible.

What issue_03.md correctly called the actual defect is now fixed:
architecture.md §8.5 and gateway.md §5.4 say it (§1), and U7 pins it with
the test that was missing.

---

## 1. Companion documents (amended together with this file)

**Applied** — the `update_02.md` pattern: recorded here, edited into the
documents in the same change. The code (§2–§8) is what remains.

* `architecture.md` §8.2 **InspectDiskNode** — the parenthetical now names
  `InspectDiskNodeReply.applied_revision` (renamed by U1) and the Action
  reads "STM-read `DnConf` for the ids (they address the agent request and
  the log line)".
* `architecture.md` §8.3 — the InspectControllerNode bullet replies
  `applied_revision` + `cn_info`.
* `architecture.md` §8.5 **GrowSlice** — a closing sentence: the pending
  rule binds only the worker's internal auto-grow; a user-driven GrowSlice
  is not so gated (U7).
* `architecture.md` §8.6 **InspectCntlr** / **InspectSide** — replies are
  `applied_revision` (+ what it means and what to diff it against) +
  `CntlrInfo` / `SideInfo`.
* `gateway.md` §2.3 — notes U1's field rename (still no new message, RPC or
  key kind).
* `gateway.md` §5.2 **InspectDiskNode** — the STM reads `DnConf` only (no
  `DnRev` read), the reply is `{applied_revision, dn_info}` from the agent's
  reply; the stale "plain pre-read of ClusterConf" parenthetical is replaced
  by what the code actually does (the resolving Snapshot supplies the cid).
* `gateway.md` §5.4 **GrowSlice** — names the argument:
  `poolTotal = math.MaxUint64`, with the AR6 rationale and U7 as the pin.
* `gateway.md` §5.5 **InspectCntlr** / **InspectSide** — the `SpRev.revision`
  in-STM reads are gone; replies are `{applied_revision, *_info}` from the
  agent's reply.
* `gateway.md` §9 #4 — the agent-path suite line reads "`Inspect*`
  `applied_revision` + info pass-through (both from the agent's reply)".
* `gateway.md` §10.11 steps 4 and 16 — rewritten for the flipped assertions
  (U6): unsynced fakes reply `applied_revision 0`; seeded state carries a
  distinctive applied revision that must pass through.
* `issue_03.md` — Status flipped to resolved, one Resolution block per item;
  the body is kept verbatim as the record of the pre-update_04 state.
* **No change** to `doc/dnv-worker.md` (AR6 stays a worker rule, exactly as
  written), `doc/grpc.md`, `doc/log.md`, `doc/layout.md`, `README.md`,
  `doc/cdc.md`.

---

## 2. U1 — `pb/schema.proto`: rename the four `Inspect*Reply` fields

In `message InspectDiskNodeReply`, `InspectControllerNodeReply`,
`InspectCntlrReply` and `InspectSideReply` (pb/schema.proto:578, :643, :787,
:798) rename the field, keeping its number:

```proto
    uint64 revision = 1;          // before, all four messages
    uint64 applied_revision = 1;  // after,  all four messages
```

Then `make gen` (rewrites `pb/schema.pb.go`; `pb/schema_grpc.pb.go` is
unaffected). Notes:

* Same field number ⇒ wire-compatible; only generated identifiers change
  (`GetRevision()` → `GetAppliedRevision()`, `Revision:` →
  `AppliedRevision:`).
* The protojson key changes with the name. `gatewayctl` marshals every reply
  with `UseProtoNames: true` (integtest/gatewayctl/main.go:88), so its
  output key becomes `applied_revision` with **no gatewayctl change**; the
  suite's `jq` paths move in U6.
* The agent-side `GetDnInfoReply` / `GetCnInfoReply` / `GetCntlrInfoReply` /
  `GetSideInfoReply` `revision` fields are deliberately **not** renamed:
  they sit in the agent protocol next to no token, their meaning (the
  revision of the last applied `Syncup*`) was never ambiguous, and renaming
  them would churn the agents, the worker and both fakes for nothing.

---

## 3. U2 — `InspectDiskNode` takes the agent's revision (`gateway/disknode.go`)

Four hunks (current line numbers from the pre-U2 tree):

1. Doc comment (:409–:415) — replace the middle paragraph (the [D-A]
   disagreement note) with:

   ```go
   // The reply is the agent's, whole: `applied_revision` and `dn_info` both
   // come from the GetDnInfo reply (architecture.md §8.2; update_04.md U2
   // reversed issue_03.md I1's interim stored-revision reading), so the pair
   // is one coherent agent snapshot and a caller can diff `applied_revision`
   // against the desired-state token GetDiskNode hands out.
   ```

   The surrounding paragraphs (purpose; Snapshot-resolves-the-cluster) stay.
2. The Snapshot (:439–:452) drops the rev key entirely:

   ```go
   var cid uint64                      // `var revision uint64` deleted
   var dnId uint64
   err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
       cid, dnId = 0, 0
       ...                             // resolveCluster + DnConf read stay
       cid, dnId = clusterId, dn.GetDnId()
       return nil
   })
   ```

   Deleted with it: the `model.DnRevKey(...)` read and its
   `errAborted("dn_rev key %q is missing", ...)` — an Inspect reads no rev
   key any more, so its only `ABORTED` is agent failure, exactly
   architecture.md §8.2's error list.
3. The agent call (:458–:468) captures the revision it used to discard:

   ```go
   var appliedRevision uint64
   var info *pb.DnInfo
   agentErr := withDnAgent(ctx, req.GetAddrPort(),
       func(ctx context.Context, client pb.DiskNodeAgentClient) error {
           reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
               ClusterId: cid,
               DnId:      dnId,
           })
           if err != nil {
               return err
           }
           appliedRevision = reply.GetRevision()
           info = reply.GetDnInfo()
           return nil
       })
   ```
4. The reply (:474):

   ```go
   return &pb.InspectDiskNodeReply{
       AppliedRevision: appliedRevision,
       DnInfo:          info,
   }, nil
   ```

---

## 4. U3 — `InspectControllerNode` (`gateway/controllernode.go`)

The exact U2 mirror:

* Doc comment (:455–:458): same replacement paragraph with CnRev /
  `GetControllerNode` names. The closing "Nothing is written, so no token is
  taken — a client that wants the desired-state token calls
  GetControllerNode" sentence stays: it is now the whole story.
* Snapshot (:493–:499): delete the `model.CnRevKey(...)` read and its
  `errAborted("cn_rev key %q is missing", ...)`; `cid, cnId = clusterId,
  cn.GetCnId()`; drop the `revision` variable.
* Agent call (:505–:516): `appliedRevision = reply.GetRevision()` next to
  `info = reply.GetCnInfo()`. The AG3 comment on the error path (:517–:519)
  stays.
* Reply (:524–:527): `AppliedRevision: appliedRevision`.

---

## 5. U4 — `InspectCntlr` / `InspectSide`; delete `inspectSpRevision` (`gateway/cntlr.go`)

* **Delete** `inspectSpRevision` (:63–:75) — its only two callers go with
  it. `SpRev` absence as `ABORTED` remains checkSpToken's business on the
  mutating RPCs; the Inspects no longer touch the key.
* **InspectCntlr** — in the doc comment (:463–:481) replace the "reply's
  `revision`" paragraph with:

  ```go
  // The reply is the agent's, whole: `applied_revision` (the revision of
  // the last SyncupCntlr the agent applied for this cntlr) and `cntlr_info`
  // both come from the GetCntlrInfo reply (architecture.md §8.6;
  // update_04.md U4) — diff `applied_revision` against the SpRev token
  // GetStoragePool hands out to see how far the agent lags desired state.
  ```

  The opening sentence about the one-store-revision Snapshot shrinks to the
  Cntlr + CnConf pair (SpRev is out of the read set); the CnConf-absence-is-
  ABORTED and AG3 pass-through paragraphs stay. In the body: drop `revision`
  from the var block (:494–:500) and its zeroing (:503), delete the
  `inspectSpRevision` call block (:518–:523), capture
  `appliedRevision = reply.GetRevision()` in the agent closure (:548
  region), reply `AppliedRevision: appliedRevision` (:556).
* **InspectSide** — same shape: doc comment (:575–:577) replaced by the U4
  sentence (SyncupSide / `side_info` names); drop `revision` from the var
  block and its zeroing, delete the `inspectSpRevision` block (:616–:620),
  capture the agent reply's revision, reply `AppliedRevision:` (:655).

---

## 6. U5 — unit tests: invert the three pins, close the cntlr/side gap

The three tests that pinned the interim decision now pin this one; the fake
constants keep their values and flip their meaning.

* `gateway/etcdenv_test.go` — `fakeAgentRevision` (:566, value `0xfa5e`)
  keeps name and value; rewrite its comment and the one in `GetDnInfo`
  (:469–:471): the constant is now the number every `Inspect*` reply MUST
  carry, and it stays a value no rev-key bump sequence reaches so a handler
  that regressed to the stored revision (1, 2, …) is unmistakable.
* `gateway/handler_node_test.go` —
  `TestInspectDiskNodeRepliesTheStoredRevision` (:1153) becomes
  `TestInspectDiskNodeRepliesTheAppliedRevision`: assert
  `reply.GetAppliedRevision() == fakeAgentRevision`; **invert the bump
  half** (:1197–:1210) — write `DnRev.revision = 5` as today, re-inspect,
  and assert the reply is *still* `fakeAgentRevision` ("a bumped rev key
  must not move the reply: the store is not the source, U2"). Mirror for
  `TestInspectControllerNodeRepliesTheStoredRevision` (:1600) →
  `TestInspectControllerNodeRepliesTheAppliedRevision`, same two assertions
  against `CnRev`. Comment headers cite U2/U3 instead of [D-A].
* `gateway/agentpath_test.go` —
  `TestAgentPathInspectRepliesTheStoredRevision` (:618) becomes
  `TestAgentPathInspectRepliesTheAppliedRevision`. Keep `storedRev = 7`
  and the hand-written rev keys — they are now the value that must NOT
  surface — and flip both subtests' assertions to `agentRev` (99). Rewrite
  the comments at :93–:96 (`agentRev` field: "the number every Inspect
  reply must carry back"), :168–:170 (`setInfos`) and the test header
  (:609–:616).
* **Close the gap**: no unit test exercises `InspectCntlr` /
  `InspectSide` (`handler_sp_test.go`'s header defers them to the §9.4
  agent-path suite, where they never arrived). Extend `agentpathFake` with
  `GetCntlrInfo` / `GetSideInfo` (reply `agentRev` + a fixed
  `CntlrInfo`/`SideInfo`, capture the requests) and add `cntlr` / `side`
  subtests: seed one DN + one CN at the fake's addr_port (the existing
  subtests' create-RPC route, or direct puts), `CreateStoragePool` with the
  minimal shape (1 slice, 1 cntlr, RedundNone ⇒ one leg per group), then
  assert both Inspects reply `applied_revision == agentRev` while `SpRev` is
  a different number, that the infos pass through, and that the requests
  carried `(cluster_id, cn_id, {sp_id, cntlr_id})` / the full `SidePointer`.

---

## 7. U6 — `integtest/gateway_test.sh`: stages 4 and 16, case C

`gatewayctl` and `workerctl` need **no change** (U1's protojson note). All
line numbers pre-U6.

* **Stage 4** (:1389–:1427) — title becomes "inspect-dn / inspect-cn: the
  agent's applied revision and live info"; rewrite the [D-A] comment block
  (:1391–:1394). Unseeded half: `.applied_revision` = `"0"` ("an unsynced
  fake has applied nothing"), `dn_info`/`cn_info` null asserts unchanged.
  Seeded half: seed `state.json` with a **distinctive** applied revision —
  `"revision":7` in the object wrapper and `"revision":"7"` in the embedded
  request, both dn and cn seeds — and assert `.applied_revision` = `"7"`
  ("the seeded applied revision passes through; the stored rev key is still
  1, so the two cannot be confused"). Info asserts unchanged.
* **Stage 16** (:2259–:2308) — in both hand-written `set_state` documents
  change the seeded revisions from 1 to 7 (wrapper + embedded request, cn
  and dn files); rewrite the [D-A] comment (:2295–:2298); the two asserts
  become `.applied_revision` = `"7"` — distinctive against the stored
  `sp_rev`, which is far past 7 by stage 16, so a regressed handler cannot
  pass. `$SP_REV` drops out of the two lines.
* **Case C** (:3732–:3739) — the recovery comment loses its "revision is
  the DnRev" clause; the assert becomes `.applied_revision` = `"0"` (the
  restored fake has still applied nothing). If that was `dn_rev_of`'s last
  use, drop the helper.

---

## 8. U7 — pin the `GrowSlice` `poolTotal` decision

* **New test** in `gateway/handler_sp_test.go` (after
  `TestGrowSliceData`):

  ```go
  // TestGrowSliceConsecutiveDataGrows pins gateway.md §5.4's poolTotal
  // argument (update_04.md U7): the gateway passes math.MaxUint64, so AR6's
  // pending rule — a WORKER convergence guard judged by the primary's
  // reported usage — never refuses a user-driven grow. The discriminator is
  // the SECOND grow of a kind: per kind the first is never pending
  // (len(grps) < 2), so a gateway that passed a real total (0 being what
  // "no report" naively becomes) would refuse it FAILED_PRECONDITION
  // "grow_pending" — issue_03.md I2's measured table.
  func TestGrowSliceConsecutiveDataGrows(t *testing.T) {
      env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
      env.createSp(sptDefaultSpec(sptSpName))
      sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
      for _, tok := range []uint64{1, 2} { // GrowSlice bumps SpRev itself
          reply, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
              ClusterName: env.name,
              SpName:      sptSpName,
              SpRev:       &pb.SpRev{Revision: tok},
              SliceId:     sliceId,
              ExtCnt:      1, // the exclusivity signal, not the size (D-E)
          })
          // want: OK both times; then assert the slice holds THREE data
          // groups (initial + 2), sizes per TestGrowSliceData's helpers.
      }
  }
  ```

  Capacity fits the standard fixture: two data grows charge
  `2 × first-group ext_cnt (4)` per leg across distinct DNs (64 free each)
  and `+4` twice on each cntlr CN (64 free, footprint 10 after create).
  Two meta grows are deliberately not added: `GrowPending` is one
  implementation for both kinds (model/ops.go), and the per-kind unit
  conversion is model's `TestGrowPending` business.
* **Comment retargets**, no behavior change:
  `gateway/storagepool.go:981` — the paragraph opens "poolTotal is
  math.MaxUint64 (decision D-B)"; retarget to "…(gateway.md §5.4,
  architecture.md §8.5; update_04.md U7 pins it)" and keep the rationale
  sentences. `model/ops.go` `GrowSlice` doc comment (the `poolTotal`
  paragraph, :1035) — append one sentence: "The gateway passes
  `math.MaxUint64` — a user-driven grow is not gated on the reported usage
  (architecture.md §8.5, gateway.md §5.4)."

---

## 9. Acceptance

1. `make gen && make build`; `go vet ./...` and `gofmt -l .` clean; the
   worker and model suites still compile and pass untouched (U1 renames no
   symbol they use).
2. `ETCD_BIN=… go test ./gateway/ ./model/` green; the two renamed node
   tests, the renamed+extended agent-path test and
   `TestGrowSliceConsecutiveDataGrows` all run (not skip) and pass.
3. Grep audits: `grep -n inspectSpRevision gateway/*.go` → empty;
   `grep -n "\[D-A\]" gateway/*.go` → empty (retargeted to U-items);
   `grep -rn "GetRevision()" gateway/*.go` (non-test) → request tokens,
   common.go token checks and the four `reply.GetRevision()` agent-reply
   reads only — no rev-key read inside an Inspect handler;
   `grep -n "'.revision'" integtest/gateway_test.sh` → no inspect hits.
4. `bash integtest/gateway_test.sh user@<lab vm>` prints `PASS`; case S
   stages 4/16 and case C assert the U6 values.
5. The `update_02.md` pattern closes out: §1's document amendments were
   committed with this file; the code lands citing U-items, and issue_03.md
   needs no further edit.
