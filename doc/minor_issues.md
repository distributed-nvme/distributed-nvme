# minor_issues.md — minor doc/code debt from the 2026-09-09 verification

Status: **applied and struck in full, 2026-09-10** — kept as a tracking
ledger because its ids are append-only and cited elsewhere. None of these
items changed behavior or blocked work. This file holds the residue of the
2026-09-09 full doc-vs-code verification (all sixteen `doc/` files against
the tree, builds/vet/gofmt/
unit suites green, update_04.md U1–U7 confirmed applied) after the same-day
amendment pass fixed the substantive stale passages (recorded in
`layout.md` §8; the edits touched `architecture.md`, `dnagent.md`,
`dnv-worker.md`, `ThinDeviceCreated.md`, `gateway.md` §0 #18 + GW14,
`issue_03.md`, both integtest specs, `cdc.md`, and two stale code comments).

The four findings that needed a real doc-or-code **decision** are
deliberately NOT here — they are decided and specified in `update_05.md`
(U1–U4) and implemented: the code landed 2026-09-09, together with the
companion-document amendments that file's §1 lists. Each is described
below as it was found: gateway trace-id minting
(gateway.md §0 #6 claims it; nothing implements it, and
`cmd/dnv-gateway/main.go:179` / `common/log.go:49` assert it as fact), the
standby ana_state probe (cnagent.md CN11/CN28 and architecture.md §9.5 claim
a comparison `agent/cnagent/leg.go:405-441` does not make), the CN14 thin-id
delete gate (`plan.wantPool` — the confirmed leak), and the meta-ladder-cap
error class (architecture.md §8.5 `FAILED_PRECONDITION` vs gateway.md
GW7/§5.4 + code `RESOURCE_EXHAUSTED`).

Deliberate deferrals are also not tracked here (dnvctl/`ctl/` with its
pending log.md R6 and grpc.md client rows, QoS enforcement, the agent
connection cache, per-CN clone-budget tracking, TLS/auth) — the specs
already record them.

Item ids: `MT*` = missing tests/probes the docs promise; `DR*` = doc drift
where the code is right (usually test-pinned) and the doc should be amended;
`UB*` = undocumented behavior — sound code worth a doc sentence. Ids are
append-only once cited from a spec, a commit or code. Retire an item by
applying it and striking it here with a date, or by folding it into an
`update_0N.md`. Doc references use section/rule ids (stable); code
references are `file:line` as of 2026-09-09.

**The 2026-09-10 sweep.** Every item below is struck: the eleven `MT*` tests
and suite probes are written, and every `DR*` and `UB*` amendment is in its
document. The ids and their original text stay — they record the finding as
it was found, not the fix — and each section below records where its items
landed and the one sub-item that was not written. Verification:
`go build ./...`, `go vet ./...`, `gofmt -l .` and `go test ./...` clean,
the last with a real `etcd` on `ETCD_BIN` so the `etcdutil`, `model`,
`gateway` and `worker` suites ran rather than skipped; and the three
on-hardware suites the new probes touch — `dnagent_test.sh`,
`cnagent_test.sh`, `gateway_test.sh` — pass against the lab. All three were
run at the pre-sweep commit first and passed there, so the runs bracket the
change: a failure afterwards would have been attributable to it.

The amendments went in under four rounds of adversarial re-reading against
the code, which is worth recording because the failure those rounds kept
catching was not the missing edit but the *plausible* one — a new sentence
that reads correctly and misstates what the code does. Most of what they
killed were false universals ("the only other unfence"; "every caller is a
worker"; "step 4 runs only on a create") and half-applied corrections, where
the rule was fixed and the same false claim survived in an acceptance grep, a
quoted code block or an implementation-notes entry. Two survivors were in
code comments rather than in prose, and were corrected with the sweep:
`common/constants.go` still budgeted `DefaultEtcdOpTimeout` per STM
*attempt* (DR1), and `gateway/clone.go`'s `AppendCloneBitmap` header still
called a chunk re-writable where the STM below it appends (DR15).

**Left open on purpose.** The `ruling R3.6` cited by four `agent/cnagent/`
comments (`clone.go`, `clonemeta.go`, `syncup_cntlr.go` and one test) still
names no committed document. UB10's new `cnagent.md` sentence records the
behavior one of those comments guards, but honoring the id itself would mean
inventing a rule number, so the citations are left dangling rather than
retargeted at a guess.

---

## 1. MT — doc-promised tests and suite probes never written

**Struck (2026-09-10)**, MT11 in part. Where each landed: MT1 →
`TestDisableLevelKeepsZeroing` (`agent/dnagent/dnagent_test.go`, beside
`TestSpLevels`); MT2 → `gateway_test.sh` case C stage 5, which now spends a
second clone (`cl-force`) and a second migration (`m-force`) on the
stopped-agent `--force` path, each paired with the unforced refusal taken in
the *same* stopped state, and each asserted against etcd ground truth rather
than the reply code; MT3 → the tolerant window sample in `sync_side_2phase`;
MT4 → `sync_side_ext_cnt` feeding `wait_zeroed`'s optional expected count,
asserted hard on both the wait's last sample and the phase-2 reply; MT5 →
the `host_subsys_present` VM helper, asserted on DNdst inside
`migr_declare_dst`; MT6 → the 1 MiB read through the CN device in
`migr_gate_src`; MT7 → `DIAG_SIDES`/`diag_add_side`, dumped best-effort by
`diagnostics`; MT8 → the same tolerant sample in the cn suite's `dn_side`;
MT9 → the hydration sample logged immediately before case C's wipe; MT10 →
case C's post-teardown `cn_residue` / `clone_meta_wrappers` / `get-cn-info`
probe, run on both CNs the way case S runs it on one.

MT11 is struck for U2-T5 (`TestThinDeviceSameNameRecreateTakesFreshIds`,
`gateway/`), U3-T5 (`TestSpCreatedFlipFromACheckRound`) and U3-T6
(`TestSpCreatedFlipIgnoresTheReplyRevision`, both `worker/`). **U2-T6 stays
open**, and is the one item this sweep did not close: its literal "inject a
concurrent create between the STM's read and its commit" has nowhere to
inject — `etcdutil.Client.run` hands the callback straight to
`concurrency.NewSTM`, so no hook exists between the callback's last read and
the commit txn — and the outcome it describes is unreachable through a real
concurrent `CreateThinDevice` anyway, because a create bumps `SpRev` and the
retried delete is refused by GW6's token check before the snapshot guard is
ever consulted. Closing it needs either a test-only seam in `run` or a writer
that reaches the guard without bumping `SpRev`; neither is worth a production
seam for what `ThinDeviceCreated.md` §9 item 5 already calls optional
hardening over structural coverage.

* **MT1** — `dnagent.md` §6 test 21, second clause: a side re-synced at
  `SP_LEVEL_DISABLE` while `zeroed_bits` are still incomplete keeps issuing
  zeroing batches. The behavior exists (`agent/dnagent/syncup_side.go:89-99`
  runs `ensureSideDev` → `startZeroing` before the `!plan.wantDm` early
  return) but no test combines DISABLE with incomplete bits —
  `TestSpLevels` exercises DISABLE only on a fully-zeroed side.
* **MT2** — `gateway.md` §10.14 step 5: the `--force` finish/delete while
  the agent is **stopped** never made it into `gateway_test.sh` case C; the
  suite's only `--force` calls run with live agents, and the unit level
  covers only the refusal side
  (`TestAgentPathForceFalseRefusesUnreachableAgent`).
* **MT3** — `dnagent_integtest.md` §9: the tolerant "provisioning window
  HIT" / "window missed" sample before `wait-zeroed` is not in
  `sync_side_2phase`; the script's only HIT/missed probes are cutover and
  read-through.
* **MT4** — `dnagent_integtest.md` §9 phase (c): the exact
  `zeroed_ext_cnt == total_ext_cnt == ext_cnt` equality (and §10/§12's
  "⇒ 1/1" / "⇒ 2/2") is unasserted; the driver's `wait-zeroed` checks only
  `total != 0 && zeroed >= total`, which would accept a wrong-sized
  allocation.
* **MT5** — `dnagent_integtest.md` §12 step 6c: "no `nvme connect` issued
  yet" after declare-dst — `migr_declare_dst` asserts the gated clone but
  never verifies the absence of the `:3:` connection on DNdst.
* **MT6** — `dnagent_integtest.md` §12 step 8b: the 1 MiB read through the
  CN device during the src gate — `migr_gate_src` asserts states only,
  performs no read.
* **MT7** — `dnagent_integtest.md` §17: the failure dump omits "the last
  `get-side-info` of every involved side" for migration cases.
* **MT8** — `cnagent_integtest.md` §9: the same provisioning-window
  observation as MT3 is missing in `dn_side`.
* **MT9** — `cnagent_integtest.md` §13 stage 6: whether hydration had
  finished at wipe time is not logged ("log which").
* **MT10** — `cnagent_integtest.md` §13 stage 10: no post-teardown
  `check-cn`/`get-cn-info` base-state probe in case C (case S has the
  analogous probe).
* **MT11** — `ThinDeviceCreated.md` U2-T5/T6 and U3-T5/T6 are recorded as
  structurally covered (§9 item 5, amended 2026-09-09); writing them is
  optional hardening: the literal same-name recreate sequence, the injected
  mid-STM race for the delete guard, and the check-round-to-flip paths
  (`show_info = false` reply; an older-revision reply).

## 2. DR — minor doc drift (code is right; amend the doc)

**All struck (2026-09-10)**: DR1–DR37 are amended into their documents —
DR34–DR37 being four more of the same kind that the sweep itself turned up,
listed at the end of this section. DR14 went in as its remaining half only —
the `gateway.md` §5.4 half was
already struck 2026-09-09 by update_05.md U4, and this sweep took the
`architecture.md` §6.5 one. DR5 landed in all three of its carriers
(`architecture.md` §9.6, `dnagent.md` DN15, `cnagent.md` CN22), and DR26/UB20
in both of theirs (`grpc.md` §6 and `log.md` R5/R12).

**dnv-worker.md**

* **DR1** — EU5 says "every STM attempt runs under `DefaultEtcdOpTimeout`";
  the code budgets the **whole transaction, retries included**
  (`etcdutil/etcdutil.go:615-691`, marked DELIBERATE DEVIATION —
  `concurrency.NewSTM` fixes its abort ctx at construction, so a
  per-attempt deadline cannot be expressed).
* **DR2** — BM5 says "migration chunks are immutable and need no memo"; the
  shared pusher memoizes both kinds (`worker/bmpush.go:188-212, 390-394`) —
  inert for immutable chunks.

**dnagent.md / architecture.md §9.6**

* **DR3** — SH22 places the single §11.4 wire-convention inversion in
  `agent/bitmap.go`; it lives at `agent/cnagent/thinbm.go:277-279`
  (`bitmap.go`'s header states chunks arrive already wire-converted).
  Reassembly and the blkdiscard range computation are in `bitmap.go` as
  stated.
* **DR4** — DN6's teardown micro-order differs from `teardownSide`
  (`agent/dnagent/syncup_side.go:873-959`): `stopZeroing` runs after the
  nvmet removals (still before any `dmsetup remove`, the EBUSY reason), and
  the dm order is linears → clone → disconnect → migr-src → errors. Every
  load-bearing constraint the doc states as rationale is honored; the
  literal enumeration isn't what runs.
* **DR5** — a failed bitmap-chunk persist is still acked code 0 on **both**
  agents (`agent/dnagent/push_migr_bm.go:43-49`,
  `agent/cnagent/push_clone_bm.go:61-67`), relying on the chunk staying out
  of the applied set so the worker re-pushes. Self-healing, but
  architecture.md §9.6's natural "persist, then ack" reading (and
  DN15/CN22's silence) doesn't specify the failed-persist reply. State it.

**cnagent.md**

* **DR6** — CN2 lists clone-bm chunk reloading *after* the converges; the
  code (and CN2's own parenthetical) loads every chunk **before** the
  converges, then sweeps the arena (`agent/cnagent/syncup_cn.go:60-110`).
* **DR7** — CN5's pinned probe `findmnt --noheadings {path}`: the code runs
  `--output FSTYPE --target` plus a second `--output TARGET --mountpoint`
  call to reject a parent-mount false positive
  (`agent/cnagent/clonemeta.go:45-70`). Functionally stronger.
* **DR8** — the §6 preamble's "for RPC-level tests, bufconn with the
  generated `ControllerNodeAgent` client": no cn test uses bufconn; server
  methods are called directly and Check streams use a hand-rolled fake.
* **DR9** — the §5 layout bullet still naming `lvm.go` is superseded by the
  later U3 bullet ("`lvm.go` becomes `clonemeta.go`") in the same section.

**architecture.md**

* **DR10** — §9.3's SyncupCntlr reply row omits `revision`, contradicting
  §9.1 and cnagent.md CN20; the code returns all four fields
  (`agent/cnagent/syncup_cntlr.go:52-57`).
* **DR11** — §13's cn invocation example omits `--capacity`
  (`cmd/dnv-agent/main.go:73-76`), the only user-settable knob of the §6.1
  CN budget (0 = "no opinion" ⇒ `DefaultCnCap`).

**gateway.md**

* **DR12** — LG2 says the component-owned records are "all Info"; the
  second-signal record is Warn (`cmd/dnv-gateway/main.go:237`), matching
  the worker/cdc precedent CM1 says to mirror.
* **DR13** — §5.4's "bumps SpRev + the leg DNs' revs itself" omits the
  cntlr CNs' `CnRev` bumps (`model/ops.go` `chargeSpCns` → `BumpCnRev`;
  architecture.md §8.5 lists them).
* **DR14** — architecture.md §6.5's "black list starts with all DNs already
  hosting a leg of that group": the gateway passes an **empty** seed
  (`gateway/storagepool.go:1106-1112`, matching the worker's AR6 rule), and
  §6.5's sentence is vacuous for a brand-new group — the phrasing misleads.
  The gateway.md §5.4 half of this item — its "§6.5 black-list seed"
  pre-read — is **struck (2026-09-09)**: update_05.md U4 replaced that
  clause with the request's own black list seeding the §6.5 scan
  empty-plus-request-entries. The architecture.md §6.5 half is **struck
  (2026-09-10)**: the GrowSlice bullet now says its black list "is seeded
  with the request's own `NodeSelector.black_list` and nothing else, and the
  gateway never appends to it", and attributes the legs' distinct DNs to the
  §6.3 `LocList` rule rather than to a seed (`gateway/storagepool.go`'s
  GrowSlice carries the same D-F comment) — outside this ledger line the
  quoted sentence survives nowhere in `doc/`.
* **DR15** — §5.8's "put the chunk at `bm_idx = slice_idx`" reads as
  replace; §8.9 and the code **append** (`gateway/clone.go:526-545`).
* **DR16** — §10.6's row "CN size | fakeagent default (0)": the fakeagent
  default is 1 TiB (`integtest/fakeagent/main.go:72-75`); the suite passes
  `--size 0` explicitly to get the DefaultCnCap mapping.
* **DR17** — §10.11 step 2's "three pages (2+2+2 …, then empty token)": per
  GW10 a *full* third page returns a non-empty token, so the empty token
  costs a fourth, empty call — which is what the script asserts.

**cdc.md**

* **DR18** — §8's "the etcd-backed ones follow the EU7 rule" sentence is
  vacuous: no cdc unit test is etcd-backed (the watcher tests run against
  an in-memory fake; the §2.2 parser tests live in `model/keys_test.go`).
* **DR19** — WV4's "the rescan **diffs** … entry by entry": the code
  installs the new map wholesale and re-renders every active host,
  comparing rendered bytes (`cdc/view.go` `registry.replace`); pinned
  equivalent by `TestWatchRescanDiffsMissedChange` /
  `TestRegistryReplaceMatchesApply`.
* **DR20** — §9.9 names the helper `wait_for <desc> <secs> <cmd>` (script:
  `wait_until <secs> <label> <cmd>`) and a `jq -S` document comparison
  (script: a jq projection of the triples to sorted lines).
* **DR21** — "next to the three existing suites" (§7-era phrasing):
  `integtest/` now holds four others.
* **DR22** — NP14 nuance: the Connect data's CNTLID field is never
  validated (`connectDataCntlIdOff` defined, unused in `handleConnect`);
  only a non-conforming host could observe it. Worth a line under NP5/NP14.

**log.md / osclient.md / grpc.md**

* **DR23** — log.md §4's "complete file" listing omits `NewTraceId`
  (`common/log.go:52-58`), though log.md §1 and grpc.md T4 acknowledge it.
* **DR24** — log.md §7's acceptance grep for `zap` now hits etcdutil's
  deliberate `zap.NewNop()` silencer (`etcdutil/etcdutil.go:19,71`); R1's
  substance holds, the literal grep line fails.
* **DR25** — osclient.md §8's "no `os/exec` outside common/osclient.go"
  grep now hits the four etcd test fixtures
  (`gateway|worker|model|etcdutil/*_test.go`); production rule holds.
* **DR26** — grpc.md §6's "every dial uses the §4 chains" grep: the three
  integtest driver dials deliberately omit the client interceptors
  (`integtest/gatewayctl/main.go:331`, `cnagentctl/main.go:118`,
  `dnagentctl/main.go:173`; trace id travels as raw metadata). The
  carve-out exists only in code comments — scope the grep to the five
  binaries or record the driver convention (see also UB20).
* **DR27** — log.md §5.3's watch-event row: `decodeEvent` adds an `error`
  attr (and drops `value`) when a put's value fails to unmarshal
  (`etcdutil/etcdutil.go:459-461`); a superset of the table.

**cnagent_integtest.md**

* **DR28** — §16 step 5 puts kind-`b` removal in cn dm pass 1; the script —
  and the doc's own §13 stage-6 wipe order — removes it in phase 2, after
  kinds 5…0/md/a/9 and before the `:2:` disconnect + tmpfs teardown. Align
  §16 with §13.
* **DR29** — assertion substitutions weaker than (or equivalent to) the
  letter of the doc: §13 stage 4's `clone_id_to_meta[0xc].res_name` reply
  assert is covered via on-VM `dm_state`/`dm_table` checks instead; §12
  step 6's snapshot bracket anchors each suspend/resume only against
  `create_snap` (not the raid0↔thin pairwise order); §13 stage 6's recovery
  re-asserts a subset of the stage-4 orderings ("every bitmap lands before
  hydration" is only "first match before `enable_hydration`"); §11 step 2's
  "ns-dev table = error" is shorthand for "dm-linear backed by the td's
  `CnErrorName` devno" (what the script checks and the agent builds, CN16);
  Appendix A's "greps for exactly that shape" flakey assert is two
  substrings in the script (matching §11 step 6's looser main text); the
  per-case `residue()` matches nvmet entries by `:<sp16>:` and cannot catch
  host-facing `dnv-it:*` subsystems — only case S's extra `cn_residue` can,
  so §11.7/§12.10/§13.10/§14.7's "no residue … or subsystems" overstates.
* **DR30** — cosmetics: the deferred write-zeroes preflight failure doesn't
  use the `missing: <what> on <vm>` format (it dies with
  `vmN: <loop> reports write_zeroes_max_bytes=0`, reading
  `/sys/class/block/...`), and request files are per-CN
  `req-<case>-cn<n>.json` edited between steps rather than per-step files
  (§3's per-VM `req-*.json` listing also conflicts with §8's on-the-driver
  location; the script follows §8).

**dnagent_integtest.md**

* **DR31** — §16 step 3's unscoped "remove all subsystems" is kind-scoped
  in the script (`:2:` first, `:3:` deferred past the dm-clones) — the only
  order consistent with the doc's own step-4 note; flagged in-script as a
  deliberate refinement.
* **DR32** — §4/§7: the same preflight-message-format drift as DR30, and
  "modprobe (ignore errors, then verify)" verifies only the nvmet configfs
  mount and `nvme_core.multipath`, not each module.
* **DR33** — §3's launch line `> $WORK/agent.log 2>&1 &` vs the script's
  `setsid nohup … >> … < /dev/null &` (append matters for case D).

**Found by the 2026-09-10 sweep** (not by the 2026-09-09 verification), while
re-reading each amended passage against the code. All four are the same shape
as the items above — the code is right — and all four are **struck
(2026-09-10)** with the rest. Code references here are `file:line` as of
2026-09-10.

* **DR34** — `dnagent.md` CM2's `--disk` row called the device "the raw block
  device that becomes the DN VG", and the CM4 `main.go` sketch spelled its
  flag help "raw block device for the DN VG". Nothing creates a VG: the
  shipped help is "raw block device that carries the dnv disk format"
  (`cmd/dnv-agent/main.go:61-62`) and `agent/dnagent/diskmeta.go` writes the
  [D13] format onto it. The same document already said so in §2.1, in §5 and
  in its own §7 acceptance grep — the correction had reached the prose and
  missed the table and the code block. Two more carriers of the same removed
  LVM: DN11's "the side LV" and DN10's dm-error "sized like the LV", where
  the device is `DnSideName`, the multi-target dm-linear DN9 itself
  describes.
* **DR35** — `dnagent_integtest.md` carried the same surviving LVM claims
  after the same correction landed in its §4 tool list ("No `lvm`") and §5.
* **DR36** — `gateway.md` §10.6's `RETRY_BUDGET` row cited a
  `race --retry-stale` flag that exists nowhere: `race`'s globals are
  `--gateway --cluster --trace-id --timeout --expect --retry-budget`
  (`integtest/gatewayctl/main.go:307-320`) and `retry_stale` is a per-job
  JSON field. §10.8 spells both correctly — the surviving half of an
  otherwise-right pair.
* **DR37** — `cdc.md` §9.3's launch block redirects with `>` where the suite
  uses `>>` (`integtest/cdc_test.sh:402` for cdc, `:1659` for etcd). Not
  cosmetic: §9.9's per-case reset truncates the four logs and case H's
  restart assertions count *past* a pre-kill baseline, so a relaunch that
  truncated would drive those counters below their baselines and time out
  every mid-case restart assertion.

## 3. UB — sound but undocumented behavior (add a doc sentence)

**All struck (2026-09-10)**: UB1–UB20 are documented next to the rules that
should have carried them. UB10 is the only one that leaves anything behind —
the code comment it came from still cites the uncommitted `ruling R3.6`; see
the note at the top of this file.

**worker**

* **UB1** *(safety-relevant — RW14/RW15 should state it)* — when any cntlr
  of an SP cannot be resolved (missing `Cntlr` or `CnConf`), the
  coordinator leaves **every side child** idle, not just that child: a
  half-built `side_conf` would make every DN of the SP tear down that CN's
  stack (`worker/sprole.go:492-556`; pinned by
  `TestSpUnresolvedCntlrLeavesEverySideIdle`; leg-row placement still runs
  with `withRequests=false`).
* **UB2** — etcdutil surface beyond the EU tables: `RangeDesc`,
  `RangeKeysAtRev` + `SnapshotRev` (MD3's pinned-revision keys-only scans),
  `IsCompacted`, and `ErrNoCommit` (the abort sentinel
  `ErrPrecondition.Unwrap()` returns).
* **UB3** — a desired change arriving while a round waits for its reply
  aborts the round and drops the stream, never counting as a health failure
  (`errRoundAborted`, `worker/revision.go:490-524`); RW4/RW6 describe the
  immediate syncup but not the abandonment/never-reuse-the-stream rule.
* **UB4** — `keepChild` restarts a child whose request changed at an
  unchanged revision (a re-resolution tick altering standby lists), since
  RW3 coalescing would otherwise swallow it (`worker/sprole.go:849-854,
  990-1007`); RW14 names only endpoint changes.
* **UB5** — a VW8 fence whose replacement-seed mint fails is remembered and
  retried on every heartbeat tick (`pendingFence`, `worker/vote.go:284-314,
  1028-1074`); VW8(c) is an event, not a condition, so the retry is
  load-bearing.

**dnagent**

* **UB6** — `Reconcile` deletes orphan `migr-bm-*` chunk files at startup
  (side gone, or `migr_id` no longer matching the stored `migr_dst_conf`)
  (`agent/dnagent/syncup_dn.go:77-107`); DN2/SH7 cover chunk deletion only
  at side teardown.
* **UB7** — `sideState.chunkMigrId` plus `dropChunks` quarantine chunks
  from a previous migration on the same side
  (`agent/dnagent/server.go:84-87`, `migr.go:96-101`); implied by
  never-reused ids, stated nowhere.
* **UB8** — `settleFence` guarantees the §11.2 window still ends (phase 2
  run directly, or the timer re-armed) on a converge that never reaches
  `ensureCnDm` (`agent/dnagent/fence.go:113-161`; pinned by two tests).

**cnagent**

* **UB9** — `ensureDmMulti` refuses to reload a pool concat to fewer
  sectors, reporting ERROR instead of remapping live pool data
  (`agent/cnagent/dmutil.go:159-176`;
  `TestConcatNeverShrinksWhenALiveGroupDefers`); CN13/U4 explain why
  shrinking must not happen, not that the agent detects and refuses it.
* **UB10** — `Wrappers()` tolerates a concurrently vanished kind-`b`
  wrapper (absence confirmed via `dmsetup info`, name dropped; a `table`
  failure on a present wrapper stays fatal)
  (`agent/cnagent/clonemeta.go:266-292`); the "ruling R3.6" its comment
  cites exists in no committed doc.
* **UB11** — startup removal of orphan `clone-bm-*` files whose owning
  cntlr/clone is not in stored state (`agent/cnagent/syncup_cn.go:64-88`);
  CN2 documents only the kind-`b` wrapper sweep and chunk reloading.
* **UB12** — `demoteXfer` sizes the demotion error table from the
  *previous* plan when the origin td was deleted in the same request, so
  the departing raid0 is released (`agent/cnagent/syncup_cntlr.go:296-319`);
  CN17/CN21 don't mention it.

**cdc**

* **UB13** — server liveness bounds absent from NP1/NP13: the 100 ms
  accept-retry pause (`cdc/server.go:21`) and the 30 s per-PDU write
  deadline that terminates a non-reading host (`cdc/conn.go:79, 316-326`).
* **UB14** — harness supersets: `cdcctl` accepts `--endpoints` as an alias
  for `--etcd`; `cdc_host.sh` exposes `wipe`/`wipe_data`/`mask`/`unmask`/
  `uev_*` beyond §9.8's narrative; the per-case reset truncates the four
  cdc logs and, with nvme-stas running, deliberately leaves discovery
  controllers to stafd.

**gateway**

* **UB15** — `growSliceCnBudget` is a Snapshot pre-check producing §8.5's
  `RESOURCE_EXHAUSTED` for a CN shortfall; in the race window after it,
  model's in-STM `chargeSpCns` refusal surfaces as `FAILED_PRECONDITION`
  instead (`gateway/storagepool.go:193-228, 1086-1098`). §5.4 mentions
  neither the pre-check nor the dual code.
* **UB16** — "SP has no primary cntlr" ⇒ `FAILED_PRECONDITION` for
  `DeleteClone(force=false)` (`gateway/clone.go:234-238`) and both bitmap
  reads (`gateway/bitmap.go:72-75`); absent from §8.9/§8.13 and
  §5.8/§5.12's error lists.
* **UB17** — `gatewayctl race` accepts a per-job `"expect"` field and emits
  `"tries"` beyond §10.8's documented job/output schema
  (`integtest/gatewayctl/main.go:604-620`).

**integtest harnesses**

* **UB18** — dn suite beyond the doc: the kernel-side phase-(a) gate check
  (`export_dms` asserts no kind-0/1 dm devices pre-provisioning); the
  shipped `/var/tmp/dnv-integtest-helper.sh` (which §16 cleanup leaves on
  the VMs); `ctl` raising `--timeout` to 60 s for `syncup-dn`/`syncup-side`;
  driver supersets — `ns-id` also emits `by_id` (the script depends on it),
  `wait-hydrated`'s JSON stdout, `syncup-side --sp-level` accepting all
  eight level names; case D's agent-stop check prints without asserting.
* **UB19** — cn suite beyond the doc: syncup deadline overrides
  (`DN_SYNCUP_TIMEOUT=60`, `CN_SYNCUP_TIMEOUT=180`), wait budgets
  (`ZERO_TIMEOUT=120`, `get-cn-size --wait 120/60`), the pre-promote
  `leg_wait_ana … optimized` barrier on all four legs, case S extras
  (`allowed_hosts` empty, kind-`b` empty), `wipe_cn` also removing kind 8 +
  `md_stop_all`, `kill_role`'s SIGKILL escalation, the gojq v0.12.17 pin.

**common**

* **UB20** — `TraceIdHandler` injects via `r.AddAttrs`, so on a
  `WithGroup`-derived logger `trace_id` lands *inside* the open group
  (`common/log.go:70-75`; pinned by `TestDerivedLoggersKeepTraceId`);
  R5/R12 don't mention the nesting. The integtest-driver conventions
  (stderr default logger; interceptor-free dials with raw-metadata trace
  id) exist only as code comments — pair with DR26.
