# e2e_integtest.md — the end-to-end integration suite (`integtest/e2e_test.sh`)

Status: **normative** for `integtest/e2e_test.sh`. Required background:
`architecture.md` (§3, §6.5, §8, §9, §11), `gateway.md` (the RPCs this suite
drives), `dnvctl.md` (the CLI it drives them with), `cnagent.md` and
`dnagent.md` (what the agents converge and what `Inspect*` reports),
`dnv-worker.md` §12 (the automatic reactions AR5, AR6, AR7 and AR8), `cdc.md`
§3 (the discovery log the hosts read).

Unlike `dnagent_integtest.md` and `cnagent_integtest.md`, which were written
before their suites, **this document was written after `integtest/e2e_test.sh`
and describes what that file actually does.** The design it came from is
`tmp_doc/use_32_slices.md` §7; where the suite and that plan differ, the suite
is the truth and §3 and §8 say so, item by item. Nothing here is a proposal.

---

## 1. Purpose — what the suite proves

One storage pool at the widest shape this tree can build, on ten real machines,
driven end to end through the shipped operator CLI, with two real kernel NVMe
hosts reading and writing its namespaces.

Concretely, a green run is the following five statements taken together.

1. **The ceiling is real.** `MaxSliceCntPerSp` slices — 32 — with md-raid1, so
   `2 × 32` groups, `2 × 32 × 2 = 128` sides on 128 *distinct* disk nodes, 64
   md arrays and 32 dm-thin pools on one controller node. The create commits a
   951-compare etcd transaction (967 at `MaxCntlrCntPerSp`), which is what
   `EtcdMaxTxnOps = 1024` was raised for; an etcd below that fails the create
   with *too many operations in txn request*. No other suite in this repo
   builds a storage pool at that ceiling — with real agents or fake ones.
2. **The operator surface works against a real control plane.** 53 of the 59
   RPCs of `service Gateway` are issued by the shipped `bin/dnvctl`, over ssh,
   against a real `dnv-gateway` on a real etcd, with real `dnv-agent dn` and
   `dnv-agent cn` processes converging real dm, md, nvmet and nvme-tcp state.
   Every reply the success wrapper accepts must parse as one JSON document
   before anything reads a field out of it, and an echoed id is never the whole
   of an assertion. Most mutators are read back from the record they changed —
   and wherever the device is the point of the mutation, from the agent that
   owns it as well. Four are proved somewhere else instead, because that is
   where their effect is observable: `ns set-suspended` on host0's ANA state
   and `ns set-dev` on the digest host0 reads through the same namespace (§4.3
   stage 07 — `ss list`'s `suspended` and `td_id` are never re-read), and
   `clone append-bm` and `clone set-tr` on the hydration that follows and the
   destination digest that ends it (§4.4 stage 02), since a bitmap chunk and a
   re-sent transport change nothing a reader could see. The teardown's three
   deletions are a third shape again: each asserts the id its reply echoes and
   is then proved by the whole sp's disappearance (§4.6).
3. **Data survives every operation.** A 4 MiB `/dev/urandom` pattern is written
   through host0 at setup and its digest (`SHA0`) is re-read after every step
   the case marks: two slice grows, the third controller's whole life, the
   `sp_level` ladder down to `SP_LEVEL_DISABLE` and back, a namespace park and
   repoint, the transfer-and-clone pass, a migration commit and a migration
   cancel, a spare-leg switch, a thin-pool auto-grow, a primary failover, a
   controller replacement and a leg repair.
4. **The four automatic reactions fire on real faults.** A killed cn agent
   moves the primary role (AR5) and then loses its controller record (AR7); a
   killed dn agent *plus* the removal of its nvmet port takes a leg out and the
   worker spares it back in (AR8); strided host writes push one slice's thin
   pool past its low-water mark and the worker appends a data group (AR6).
5. **The lab is left as it was found.** After every case: every disk node's
   `free_ext_cnt` is back to `total_ext_cnt` with an empty `side_ptr_list`, no
   `dnv-*` dm device, no dnv md array and no tree-minted nvmet subsystem
   survives on any of its dn or cn guests, no backing file has materialised
   beyond 256 MiB, and the whole run's allocation on all ten guests is under
   8 GiB.

### 1.1 What it does not prove

* **The six RPCs it never issues**, all of them deletions or listings of nodes
  and clusters: `cluster delete`, `cluster list`, `dn delete`, `dn list`,
  `cn delete`, `cn list` (`DeleteCluster`, `ListClusters`, `DeleteDiskNode`,
  `ListDiskNodes`, `DeleteControllerNode`, `ListControllerNodes`). The node
  deletions are covered by `gateway_test.sh`; nothing here removes a node
  record, because the run throws the whole cluster away between cases instead —
  and a listing proves nothing this suite does not already prove by reading
  each node it registered.
* **Error paths in general.** Five refusals are asserted on purpose, all in
  `ops`: three in §4.3 stage 3 and two in stage 8. Each has its own reason —
  the two cntlid-slot refusals *are* the point of that stage, the
  `DeleteCntlr` one pins a documented precondition, and the two
  `RESOURCE_EXHAUSTED`s are the only observable proof that `set-disabled` took
  effect. `copy` additionally reads `NOT_FOUND` back after each of its three
  object deletions. Everything else is a happy path; validation semantics stay
  `gateway_test.sh`'s job and CLI semantics stay `dnvctl_test.sh`'s.
* **Concurrency.** One dnvctl call at a time, one case at a time. Revision
  tokens are never sent: `--rev` is deliberately absent from the global argv,
  because the token is presence-based (an omitted `--rev` sends no token
  message at all) and passing one would change what the gateway checks on
  every mutator.

---

## 2. Topology and parameters

### 2.1 Roles

Every server comes from argv. Nothing is hardcoded, and the suite refuses to
start when two roles name the same target.

```
bash integtest/e2e_test.sh [--only <case>] [--cleanup-only] [--slice-cnt N]
     [--redund raid1|none] [--dns-per-vm N]
     --cp user@ip --cn user@ip … --dn user@ip … --host user@ip …
```

| flag | count | what runs there | root |
|---|---|---|---|
| `--cp` | exactly 1 | etcd, `dnv-gateway`, `dnv-worker`, `dnv-cdc`, and `dnvctl` itself | no (plain login user) |
| `--cn` | at least 3 | one `dnv-agent cn`; two carry the sp's cntlrs and the rest is the spare AR7 lands on | passwordless sudo |
| `--dn` | at least `LEGS` (in practice 4) | `DNS_PER_VM` × `dnv-agent dn` over loop devices | passwordless sudo |
| `--host` | exactly 2 | nothing of dnv: the kernel's nvme-tcp stack and nvme-cli | passwordless sudo |

cp is the one guest driven as a plain user, so there is no sudo check for it in
preflight; the single root thing it is ever asked for is `fstrim -a` at end
cleanup, and that caller tolerates a refusal.

The default lab shape is ten guests: 1 cp, 3 cn, 4 dn, 2 host.

### 2.2 Parameters and the numbers derived from them

| parameter | default | meaning |
|---|---|---|
| `--slice-cnt N` | 32 (`SLICE_CNT_DEFAULT`, mirroring `common.MaxSliceCntPerSp`) | slices per sp; accepted range 1..32, checked in `parse_args` rather than 200 lines into setup |
| `--redund raid1\|none` | `raid1` | `LEGS` = 2 for raid1 (`common.MaxAllocLegPerGrp`), 1 for none |
| `--dns-per-vm N` | the placement bound of §2.4 (43 for the default lab shape) | dn agents per DN VM; below the bound warns, never errors |
| `--only <case>` | all four | `smoke`, `ops`, `copy`, `react` |
| `--cleanup-only` | — | ship the helpers, run the start cleanup on every guest, stop |

Derived, all in `derive_params`:

```
GRP_CNT   = 2 × SLICE_CNT                 one meta group and one data group per slice
DNS_PER_VM_BOUND = ceil(LEGS × GRP_CNT / (DN_VM_CNT − LEGS + 1))
DN_TOTAL  = DN_VM_CNT × DNS_PER_VM
TD_UNIT   = SLICE_CNT × STRIPE_SIZE       every `td create --size` against sp0 is a
                                          positive multiple of it
```

`GRP_CNT` is the plan's `GROUPS` under another name, and the rename is
load-bearing: bash's own `GROUPS` is a special array of the user's gids and an
assignment to it is *silently ignored*, so `GROUPS=$((2 * SLICE_CNT))` would
leave `$GROUPS` at the primary gid and every number derived from it wrong
without a word of complaint.

Two `td create`s stand outside `TD_UNIT`, and both are deliberate. The snapshot
of §4.3 stage 06 sends `--size 0`: a plain `td create` wants a *positive*
multiple, and a snapshot is the one form in which 0 is legal, because it
inherits its origin's size instead. And the fallback source pool of §4.4 is a
second storage pool with its own geometry — one slice, the same stripe — so the
thin device made there is checked against **that** pool's `slice_cnt ×
stripe_size`, read back from its own `sp create` reply rather than assumed to
match `sp0`'s.

### 2.3 Fixed constants

The shell cannot import `common`, so every number a Go constant owns is
repeated with the constant it mirrors — except two, which are *read* rather
than copied (§3, rule E2E12).

| shell name | value | mirrors |
|---|---|---|
| `SLICE_CNT_DEFAULT` | 32 | `common.MaxSliceCntPerSp` |
| `EXTENT_SIZE` | 67108864 | `common.MinDnExtSize`; sent once, as `cluster create --extent-size` |
| `STRIPE_SIZE` | 1048576 | a suite choice, bounded above by `validateCloneGeometry`'s `256 × 4 KiB` ceiling on `src_stripe_size` — not by the sp's own 64 MiB limit |
| `INIT_EXT_CNT` | 1 | extents per data group; also fixes AR6's grow size |
| `CNTLR_CNT` | 2 | one primary and one standby, on two distinct CN VMs; the remaining `--cn` guests carry no cntlr, which is where AR7's replacement lands |
| `SLOTS` | `0,1` | the cntlid slot list §4.3 step 3 grows to `0,1,2` |
| `THR_QUIET_PRIMARY / _CNTLR / _SIDE / _LEG` | 1800 / 1800 / 1800 / 3600 s | the `sp create --thr-*` set of `smoke`, `ops` and `copy`; every one of them is longer than the longest wait in the suite, for the reason below the table |
| `THR_REACT_PRIMARY / _CNTLR / _SIDE / _LEG` | 5 / 20 / 20 / 30 s | the same four flags for `react`, the one case that has to watch a reaction land |
| `THR_PRIMARY / THR_CNTLR / THR_SIDE / THR_LEG`, `THR`, `THR_SET` | whichever set the sp being built carries | the *active* set, copied from one of the two by `sp_thresholds`. They are four variables and not one string because `setup_create_sp` asserts them back a **field at a time** out of `sp get` — which is also the proof that the argv string split into eight words — and because `react`'s messages name the individual threshold each of its waits is watching. **No wait bound is computed from any of them**: `WAIT_REACT` is a flat number, sized by hand |
| `VOTE_INTERVAL / VOTE_GRACE` | 2 / 6 s | `dnv-worker --vote-interval/--vote-grace-time` |
| `BACKING_SIZE` | `2G` | `truncate -s`, never `fallocate -l` |
| `DN_CAP_BYTES / RUN_CAP_BYTES` | 256 MiB / 8 GiB | the two allocation caps of §4.6 |
| `NQN_PREFIX` | `nqn.2024-01.io.dnv` | `common.NqnPrefix`; the suite computes tree-minted NQNs from it and sweeps them, and never mints one |
| `NQN_IT` | `nqn.2024-01.io.dnv-it:e2e` | the prefix of the host-facing subsystems this suite creates; a suite choice, since a host-facing NQN is literally the `ss create --nqn` string |
| `CLUSTER` / `SP` | `e2e` / `sp0` | the globals every dnvctl call carries |
| `ETCD_MAX_TXN_OPS`, `MAX_ALLOC_LEG_PER_GRP` | *read at preflight* | `workerctl constants` |

**Why the thresholds are per case, and why the choice is made at `sp create`.**
Nothing changes an `event_threshold` after the sp exists. `EventThreshold`
appears in exactly two messages — `SpConf` and `CreateStoragePoolRequest` — the
four `--thr-*` flags exist on `sp create` alone, the handler stores the message
verbatim, and no other one of the 59 RPCs of `service Gateway` touches it. So
the set a case needs is a parameter of the *build*, not of the case:
`sp_thresholds <case>` is called before setup and again before each rebuild
(§3, rule E2E11), and an unknown case name dies rather than defaulting to
either set — adding a fifth case is a decision about whether it may tolerate a
reaction mid-run, and a default would make that decision invisibly. `main`
therefore fills `RUN_CASES` — the cases this run will actually execute, in
`CASES` order with `--only` applied — **before** it builds anything, because
setup builds the first of them and has to be told whose sp it is building. Both
sets are printed by `log_topology` before the first ssh.

`smoke`, `ops` and `copy` test operations, and a failover or a spare leg
arriving in the middle of one is not a finding but noise that invalidates the
assertion it lands in: an absolute side count, a group that has no spare yet, a
digest read through a controller that has just stopped being the primary. They
therefore build under thresholds a case has no ordinary way to reach. 1800 s is
3.4 × the 525 s build **window** the first run measured (§8 item 12 — a window
with two failovers inside it, not one uninterrupted build) on the widest shape
this tree builds, and it is longer than any single wait in the suite — the largest,
`WAIT_BUILD`, is 1200 s — so an object would have to stay unhealthy across
several waits that all *succeeded* before a threshold was met. That is not a
proof of impossibility, and this document does not offer one; it is what lets
those three cases keep absolute counts and read a spare leg as a finding
(§4.2). `react` keeps the short set, because AR7 waits `cntlr_unhealthy` and AR8 waits
`side_unhealthy`/`leg_unhealthy`, and at the gateway defaults (600, 600, 1200)
neither is observable inside a bound this suite could wait out. What that costs
`react` is §4.5's snapshot.

Two traps the quiet set has to clear, both of them in the tree rather than in
the suite. `leg_unhealthy` must exceed `side_unhealthy` **after** the defaults
are resolved, which is why the leg number is doubled rather than equal. And a
`--thr-*` that is omitted or zero is not "no threshold": dnvctl sends no
`EventThreshold` message at all unless at least one of the four is non-zero,
and `model.ResolveEventThreshold` turns every zero field into
`common.Default*Unhealthy` — 5, 600, 600, 1200 — at every worker pass. A quiet
set that named three flags and left `--thr-primary` out would run on a
5-second primary threshold, which is the one that failed the first run
(§8 item 13).

Polling budgets, every one of them bounding a `wait_until` and none of them a
sleep: `WAIT_SHORT` 15 s, `WAIT_CP_READY` 30 s, `WAIT_AGENT` 30 s, `WAIT_BUILD`
1200 s, `WAIT_PROVISION` 600 s, `WAIT_DELETE` 900 s, `WAIT_REACT` 120 s,
`WAIT_HOST` 60 s, and the copy case's own `WAIT_HYDRATE` 600 s,
`WAIT_SRC_CONNECT` 60 s, `WAIT_HYDRATE_STALL` 90 s. Seven are unchanged:
`WAIT_SHORT`, `WAIT_CP_READY`, `WAIT_AGENT`, `WAIT_REACT`, `WAIT_HOST`,
`WAIT_SRC_CONNECT` and `WAIT_HYDRATE_STALL`. The other four are the first run's
doing — `WAIT_BUILD` is new, and what used to be one number repeated (300 s for
provisioning, for the drain and for a hydration) is now three different ones:

* `WAIT_BUILD` bounds a **whole cntlr stack built from nothing** — 32 pools, 64
  arrays and 128 legs on one CN — **and the other from-nothing convergence in
  the suite**: setup's first wait, all 128 sides `blkdiscard`-zeroed across the
  172 DN agents before any of them can be exported. The line between the two big
  budgets is *from nothing* against *an increment*, not *a cntlr stack* against
  everything else; that wait has never been timed on its own (the first run
  passed it and then died in the stack wait), so it carries the generous budget
  rather than a number nobody has. The sides waits of a *grow* keep
  `WAIT_PROVISION`: they are the `LEGS` sides one new group adds. It is 2.3 ×
  the 525 s window of §8 item 12, which leaves margin for a busier lab and for a
  `react` build that loses a failover's worth of work and starts again. Seven
  waits carry it: setup's three — the sides, the primary's stack and the
  standby's shape (§4.1 stage 07); the new primary of §4.5 stage 03, which is
  that same build on a node that had only legs; the third controller of §4.3
  stage 03 and the AR7 replacement of §4.5 stage 04, because a controller born
  now holds nothing and every leg is a fresh connection — the standby half of a
  build; and each rung of the `set-level` ladder of §4.3 stage 05, whose
  `DISABLE` rung tears the whole stack down and whose climb back builds it
  again.
* `WAIT_PROVISION` bounds an **incremental** convergence on something that
  already exists: the handful of sides a grow adds, one grown group's md array
  and pool reload, a thin device's volumes and raid0, one leg reconnecting, and
  — since the third run — a cntlr's host-facing export, its nvmet subsystem,
  the link that makes its port listen and its namespace (§4.1 stage 10).
  Twice the old 300 s. Separating the two is what keeps a stuck `td create` from
  costing twenty minutes.
* `WAIT_DELETE` bounds the drain, which is the build run backwards on the same
  2-vCPU CN, so it is sized against that 525 s window rather than against the
  old 300 s.
* `WAIT_HYDRATE` follows `WAIT_PROVISION`'s **number** and not its name, and
  the suite says why at the constant: a hydration is bounded by the nvme-tcp
  path and dm-clone's copy threads rather than by the spawn rate that decides
  a build, but it runs on the same 2-vCPU CN and competes with the same work.
  `WAIT_HYDRATE_STALL` stays 90 s and deliberately below it, since the stall
  is what routes to the fallback (§4.4).

`WAIT_REACT` stayed at 120 s on purpose. It bounds a reaction landing after its
threshold — threshold plus a few 5 s worker passes — and every threshold it
bounds belongs to `react`'s own short set, so nothing the build measurement
says bears on it.

dnvctl's own `--timeout` is 30 s by default here (its built-in 10 s is not
enough), raised to 180 s for the `sp create` of setup and 120 s for the
fallback pool's.

### 2.4 The placement bound, and why it is 43

Every side of the sp lands on a **distinct** disk node: `CreateStoragePool`
starts its DN black list as the request's and grows it with every pick, so all
`LEGS × GRP_CNT` sides — 128 at the default shape — need 128 disk nodes that
have never been picked. That is why a DN VM runs dozens of agents: four guests
cannot otherwise supply 128 nodes.

Placement is per **location**, not per node. Every dn agent of VM *v*
registers `--location dn<v>`, and a candidate scan keeps at most one candidate
per location, so a group draws its `LEGS` sides from `LEGS` *different VMs*,
and a pick fails `RESOURCE_EXHAUSTED` the moment fewer than `LEGS` VMs still
hold an unpicked DN.

The bound is a counting argument and nothing else:

* emptying `(V − LEGS + 1)` VMs costs `(V − LEGS + 1) × N` picks, where `V` is
  the number of DN VMs and `N` the agents on each;
* only `LEGS × GRP_CNT − LEGS` picks happen before the **last** group's scan;
* so if `(V − LEGS + 1) × N > LEGS × GRP_CNT − LEGS`, no scan can ever see
  fewer than `LEGS` VMs with a DN left, and the create cannot be starved.

At `V = 4`, `LEGS = 2`, `GRP_CNT = 64`: `3N > 126`, i.e. `N ≥ 43`. The suite
computes `N = ceil(LEGS × GRP_CNT / (V − LEGS + 1)) = ceil(128/3) = 43`, which
satisfies the inequality with at most one DN per VM to spare; the exact
minimum, `ceil((LEGS × GRP_CNT − LEGS + 1) / (V − LEGS + 1)) = ceil(127/3)`, is
the same 43 here. At `--redund none` on the same four VMs the bound is
`ceil(64/4) = 16`.

**Do not justify this with a "two-VM tail" argument.** When exactly two VMs
still hold DNs, both are returned by every scan and both are picked, so they
drain in lockstep — and lockstep *preserves* the difference between their
counts rather than closing it. The VM that entered the tail behind stays
behind and empties first. The counting argument above is the whole proof, and
the suite's comment says so where the code computes it.

Two consequences the suite acts on:

* `--dns-per-vm` below the bound is a **warning**, not an error, and the
  warning names what it risks: at exactly `LEGS × GRP_CNT / V` per VM (32 here)
  the create fails about four runs in five, and a run that *does* succeed
  leaves every VM at zero free DNs — which is where the anti-affinity relaxes
  and two sides of one leg can land on one kernel (§8, item 5).
* `DNS_PER_VM` above `MAX_DNS_PER_VM = 50` is a **die**, with the hint "add
  `--dn` guests": more VMs raises the divisor and lowers the bound. The same
  cap keeps the DN gRPC block below the CN port (`29900 + 49 = 29949 < 29950`).

### 2.5 Ports, paths and processes

| where | what | ports and paths |
|---|---|---|
| cp | etcd | client 16379, peer 16380, `--name dnv-e2e-it`, `--data-dir $WORK/etcd`, `--max-txn-ops=$ETCD_MAX_TXN_OPS` |
| cp | `dnv-gateway` | `--grpc-address $CP_IP:29850 --etcd-endpoints 127.0.0.1:16379` |
| cp | `dnv-worker` | `--roles dn,cn,sp --vote-interval 2 --vote-grace-time 6`; binds no port |
| cp | `dnv-cdc` | `--tr-addr $CP_IP --tr-svc-id 18020`; `--range` left at its default, so one instance serves every shard |
| cp | `dnvctl` | `$WORK/bin/dnvctl --gateway-address $CP_IP:29850 --cluster e2e --sp sp0 --trace-id <per stage> --timeout <secs>` |
| dn VM *v*, instance *k* | `dnv-agent dn` | gRPC `29900+k`, trsvcid `4300+k`, `--nvmet-port-id $((k+1))`, `--disk /dev/loopN` over `$WORK/dn<k>/backing.img`, `--local-store $WORK/dn<k>/store`, log `$WORK/dn<k>/agent.log`, registered `--location dn<v>` |
| cn VM *v* | `dnv-agent cn` | gRPC 29950, trsvcid 4300, `--capacity 0`, `$WORK/cn/store`, log `$WORK/cn/agent.log`; **no** `--nvmet-port-id`, so it converges `ports/1` |
| host *h* | nothing of dnv | its own `/etc/nvme/hostnqn` + `hostid`; `nvmf-connect@.service` and `nvmf-connect.target` masked, and the mask read back rather than announced |

`WORK=/var/tmp/dnv-e2e` on every guest. The generated guest helper lives at
`/var/tmp/dnv-e2e-helper.sh`, a **sibling** of `$WORK` and never a child,
because cleanup runs `rm -rf $WORK` through that very script and a script may
not delete itself while bash is reading it. The cn agents' clone-metadata
arena is at `/tmp/dnv-tmpfs` (`common.DefaultTmpfsPrefix`; no flag moves it),
which is outside `$WORK` — so both the space guard and cleanup look there
explicitly.

Each dn agent needs its own `addr_trsvcid` (two nvmet ports cannot share one
ip:port) and therefore its own configfs port, which is what
`dnv-agent --nvmet-port-id` buys; ANA groups nest under the port, so distinct
port ids also give each agent its own groups 1/2/3. Instance 0 keeps the
historical port 1.

**The md assembly mask, on both node roles.** Every dn *and* cn guest carries
`/etc/udev/rules.d/63-dnv-md.rules` for as long as its agents are up: `dn_up`
and `cn_up` each install it **before** they launch their agent, and
`dn_cleanup` and `cn_cleanup_phase2` each remove it again, because these are
shared lab machines. (So it goes on and off once per case, with the rest of the
data plane, at every between-cases rebuild.) The rule is `cnagent_test.sh`'s,
scoped by `MD_NAME` to this tree's `dnv-*` array names — a guest's own arrays,
a root filesystem raid above all, assemble as they always did — and it works by
setting `SYSTEMD_READY=0`, which is what makes the stock
`64-md-raid-assembly.rules` stand down for that device.
A CN needs it because that is where md **runs** and the stock rule would race
the agent for an array it is in the middle of creating. A DN needs it because
that is where md's metadata **lands**: the leg superblocks the CN writes travel
the side export onto the DN's own storage, and an unmasked DN assembles them
there (§8 item 15). Installing is idempotent — the helper writes only when the
content differs — which is what keeps `dn_up`, which runs once per instance and
so 43 times on one DN VM, from truncating and rewriting a file udev is watching
at every one of them.

**Port ownership.** Nothing above is *bound* by any other suite in this repo:
worker 12379/12380 + 29600-29603 + 29700-29702; gateway 15379/15380 +
29810-29812 + 29820-29823 + 29830-29832; cdc 13379/13380 + 18009-18012 +
14420-14423; the two agent suites 29528/29529 + trsvcid 4200; dnvctl
29840/29841. Two numbers of this suite's block do *appear* elsewhere —
`dnvctl_test.sh` carries `127.0.0.1:29901` and `:29902` as payload for its fake
to record — but that suite never dials them and its own port list is
`(29840 29841)`. "No port here appears in another suite" is false as written;
"no port here is bound by another suite" is what holds.

### 2.6 Identity and the names the suite computes

The suite computes NQNs the agents mint, so it can discover, connect, grep and
sweep them; it never mints one. Each formatter mirrors `common/name_fmt.go`
argument for argument, and the argument order is **not** uniform — `DnHostNqn`
and `CnHostNqn` take `(cluster, node)`, `SideToCnNqn` takes
`(cluster, sp, leg, cn)` and carries no dn id, `MigrSrcNqn` takes
`(cluster, dn, sp, migr)` with the dn id *before* the sp id, and `XferNqn`
takes `(cluster, sp, xfer)`.

Ids are rendered with `printf '%016x'` directly from the decimal string dnvctl
prints, never through `$(( ))`: a cluster id is a 64-bit fnv1a and has an even
chance of landing above 2^63, where bash's signed arithmetic wraps. A
non-decimal argument makes the helper log a bug line and return a poison string
(`notanid-…`) rather than the plausible `0000000000000000` that would name
cluster 0, and all three callers that build a name guard on it. Only two of
those guards can fire, though: every formatter prints `$NQN_PREFIX` and its kind
digit first, so the poison lands in the *third* or a later field of an otherwise
well-formed NQN, and a guard must match it anywhere in the string. The transfer
case's `*notanid-*)` on `XferNqn` and the migration's on `SideToCnNqn` do;
`copy_cn_host_nqns`'s `notanid-*)` on `CnHostNqn` is anchored at the start of
the NQN and is dead code. Nothing goes undetected today, because every id these
three hand to a formatter — the cn id, the transfer id, the leg id, the sp id
and the cluster id — is rejected for a non-decimal digit where it is read.

Two of the five formatters, `dn_host_nqn` and `migr_src_nqn`, have no caller
today: the suite never needs a DN's host nqn, and it observes a migration
through the record and
the destination side's own `InspectSide` rather than through the source
subsystem's name.

Namespaces always carry an explicit `ns create --uuid`, because an empty
`dev_uuid` makes the gateway mint a random v4 one; the host then resolves the
device as `/dev/disk/by-id/nvme-uuid.<uuid>`, which is the only stable name
(the multipath head's own `/dev/nvmeXnY` number moves between reconnects). The
three fixed uuids are `…8c01` for the sp's namespace, `…8c02` for the clone and
AR6 targets, and `…8c03` for the fallback source pool's.

Both hosts reach their namespaces as **themselves**: the nqn in
`/etc/nvme/hostnqn`, generated if absent and never overwritten, and the id in
`/etc/nvme/hostid`, passed explicitly on every connect. The suite asserts that
the two hosts' nqns differ.

---

## 3. The rules

The design (`tmp_doc/use_32_slices.md` §7.10) proposed twelve rules `E2E1` to
`E2E12`. Each is restated below as the design worded it, then checked against
`integtest/e2e_test.sh` and corrected where the suite does something else.

Two facts about the ids themselves, both verified rather than assumed:

* `ctl/doclint_test.go` used to extract a rule id as `[A-Z]{2,4}` followed by
  digits, which `E2E1` does not match — the family name contains a digit — so
  this family would have been **outside** the lint's namespace: no definition
  registered, no citation checked, here or anywhere else. That is the same
  silent gap the lint exists to close, so the family pattern was widened to
  `[A-Z]{2,4}|[A-Z][A-Z0-9]{0,2}[A-Z]` when this suite landed. The second
  alternative requires the family to END in a letter, which is what keeps
  `SPD14` splitting as `SPD` + `14` rather than `SPD1` + `4`; without that
  requirement every range in the tree parses as crossing families. `E2E` is
  now a registered family: `TestDocRuleCitationsAreDefined` and
  `TestDocRuleRangesResolve` both cover this file, verified by mutation —
  citing an id past the last one defined here, and widening the range below to
  reach it, each fail and name this file and the offending line. (Neither
  mutation can be quoted here: the lint reads this paragraph too, which is
  itself the demonstration.)
* The suite cites these ids in its own comments (`E2E2` in the header,
  `E2E11` in `main`, and so on), which is a one-way link: the shell cannot
  check them either.

* **E2E1 — servers come only from argv.** *As designed:* `--cp/--cn/--dn/--host`;
  nothing is hardcoded. **HOLDS.** `parse_args` accepts both `--flag value` and
  `--flag=value` for every flag and treats any positional word as a usage
  error; the arity rules of §2.1 are enforced; a target used for two roles is a
  die naming the duplicate. The only addresses the run ever uses are the ones
  argv gave it, plus `127.0.0.1` for cp's own loopback to etcd; the example
  invocation in the header comment is a comment.

* **E2E2 — the control path is `bin/dnvctl` only.** *As designed:* no
  `workerctl` or `gatewayctl` writes. **HOLDS, with one named exception that
  writes nothing — and one tool that is built and never run.** `workerctl` is
  built on the *driver* and run there for its `constants` subcommand alone,
  which opens no etcd client and reads no key. `cnagentctl` is built on the
  driver too, for `host-id --hostnqn` — a pure function of its argument — but
  **no step of this suite calls it today**, and the file says so where the
  wrapper is defined: every `nvme connect`/`connect-all` here presents the
  host's own `/etc/nvme/hostnqn` and `/etc/nvme/hostid`, so a derived host id
  is never needed. The wrapper stays because a step that connected a *host*
  under a tree-minted nqn would need it, and the build costs a couple of
  seconds of a package already in this repo. `etcdctl` is shipped to cp from
  the pinned tarball for read-only diagnostics and is not invoked anywhere in
  the file. Every control-plane read and every mutation is the shipped
  `$WORK/bin/dnvctl`, run on cp over ssh — one ssh per invocation, and many
  hundreds of them over a run, which is what the wall clock is mostly made of.
  At the default shape setup registers each of the 172 disk nodes with its own
  `dn create`, polls each one to readiness with its own `dn inspect`, and the
  shared ending reads each one back with its own `dn get`: more than 500 round
  trips before any case-specific invocation, and before a single poll has had
  to repeat. dnvctl's three streams are framed apart through files on cp so
  that "stdout is empty" and "stderr is exactly one line" can both be
  asserted.

* **E2E3 — `DNS_PER_VM` defaults to the placement bound.**
  `ceil(legs × 2 × slice_cnt / (V − legs + 1))`; an override below it is a
  warning, not an error. **HOLDS, and the suite adds a hard ceiling.**
  `DNS_PER_VM > MAX_DNS_PER_VM` (50) is a die with the hint "add `--dn`
  guests". The warning text names the failure it risks, not merely
  "the create may starve" (§2.4).

* **E2E4 — every dnagent of a VM registers `--location <vm role>`.** *As
  designed:* every group therefore straddles VMs and no two sides of one leg
  share a kernel. **CORRECTED — the second half is a headroom guarantee, not a
  structural one.** The registration holds, and the suite asserts after
  `sp create`, after each grow and after each automatic placement that the
  `LEGS` legs of every group sit on `LEGS` different DN VMs. But a migration
  destination and a spare leg are placed with the group's *locations* as a
  tier-1 exclusion, and tier 2 rescans without that exclusion when tier 1
  yields too few candidates — so with no free DN outside the group's VMs the
  destination can legally land on the source's VM, where `SideToCnNqn`
  (which carries no dn id) would collide between two agents of one kernel. The
  suite therefore makes its VM-distinctness assertions for migration, spare
  creation and AR8 **only when `DN_VM_CNT > LEGS`**, and logs a skip line
  otherwise; the bound of §2.4 is what keeps the hazard unreachable in the lab.

* **E2E5 — sparse backing files, the write-zeroes gate, and the allocation
  caps.** **HOLDS, with three gates rather than one.** Backing files are
  `truncate -s 2G` (never `fallocate -l`). `write_zeroes_max_bytes > 0` is
  checked by `dn_up` *before* it launches the agent (the only place that can
  refuse, since the agent must not exist yet), re-asserted on the driver from
  what `dn_up` reported, and re-read once more for every recorded loop device
  by `preflight_loop_devices` — which also asserts the record is **complete**,
  `DN_TOTAL` devices, so a setup that silently started fewer agents than
  `DNS_PER_VM` cannot reach `sp create` and fail there as
  `RESOURCE_EXHAUSTED`. The triple gate exists because the dn agent only
  *tags* a disk whose Write Zeroes is 0 and never refuses it, so side zeroing
  would fall back to writing real zero pages and materialise every sparse
  backing file on that guest. The per-file and whole-run caps are asserted
  after every case (§4.6).

* **E2E6 — cleanup runs unconditionally at start, and only on success at end.**
  **HOLDS, and since the second run the start sweep has a verdict of its own.**
  One function, `cleanup_all`, is called from `main` before anything is built,
  from `setup_between_cases`, from `on_exit` when the run's status is 0, and
  alone under `--cleanup-only`. It never dies — at the start a die would be
  wrong (leftovers are what it is for) and at the end it would turn a run whose
  every assertion passed into a failure and skip the other nine guests. It
  *reports* instead, and `SETUP_DONE` suppresses the end cleanup for a run that
  died before it built anything. What the second run added is
  `cleanup_start_gate`, which runs after the start sweep and after the
  between-cases sweep and nowhere else, and which does die: tolerance of
  **absence** is the whole of E2E6 at the start — a guest with nothing on it
  runs every verb to the end and says so — while a verb that never printed its
  sentinel is the opposite of absence, debris that outlived the sweep meant to
  remove it. The gate names each guest, verb and exit status and stops there,
  rather than letting the run walk into a failure about whatever the debris
  collides with first (§6, §8 item 15). The end cleanup and `--cleanup-only`
  are deliberately outside it: they *are* the end, and `CLEANUP_DIRTY` already
  carries their verdict into the exit code and the banner.

* **E2E7 — no `iflag=` / `oflag=`.** **HOLDS, verified by inspection of the
  whole file including the generated helper.** Every write is buffered plus
  `conv=fsync`; every read that must reach the media is preceded by
  `host_drop_caches` (a `sync` first, because `drop_caches` never discards a
  dirty page). The one read that may legitimately block —
  `SP_LEVEL_READONLY`'s — goes through `host_sha_probe`, which runs the `dd`
  detached on the guest with the *group's* stdout redirected and answers
  `blocked` inside its own budget, because `timeout` cannot bound a task in D
  state and command substitution hangs on a background child that still holds
  the ssh pipe.

* **E2E8 — pids in files, signals by pid, `pkill` only from helper files with
  bracketed patterns.** **HOLDS.** The four cp daemons record `$!` in
  `$WORK/<dir>/pid` and are signalled CONT → TERM → KILL by that pid; each
  agent records its pid in its own directory and the react case stops exactly
  one of them by that file. Every `pkill` in the file sits inside a
  single-quoted helper heredoc, is bracketed (`[d]nv-agent`, `[d]nv-gateway`,
  `[e]tcd --name dnv-e2e-it`), and is reached through a helper **verb**
  (`kill_agents dn`), so the pattern never appears in an ssh command string
  where `pkill -f` would match the wrapping `bash -lc` argv and kill its own
  shell.

* **E2E9 — never run while any other dnv suite runs anywhere in the lab.**
  **HOLDS as an operator rule; it is not enforceable from inside.** The suite
  states it in its header, prints it in `log_topology` before the first ssh
  with the guest count, and names the concrete collision: its cn agents mount
  their tmpfs at `/tmp/dnv-tmpfs`, the path `cnagent_test.sh` owns. What the
  suite *can* check it does check — that no port of its block is already
  listening and that no nvmet port it needs already exists (§5).

* **E2E10 — hosts reach namespaces only through the cdc.** *As designed:*
  `nvmf-connect@.service` is masked and `connect-all` is the suite's own act.
  **CORRECTED — the mask holds; "only through the cdc" has three exceptions,
  and each is forced.** The invariant the suite really keeps is that **every
  path a host holds was made by this suite**: the autoconnector is masked at
  preflight and again in `setup_infra` (because `host_cleanup` unmasks, and
  `setup_infra` is also the rebuild half of the between-cases step) — and since
  the third run the helper *reads both units back* with `systemctl is-enabled`
  instead of printing `masked` whatever happened, so the two `assert_eq`s that
  carry this rule are evidence rather than an echo — and `stafd`/`stacd` must
  be inactive. Discovery through the cdc is used wherever
  the subsystem is in a `CdcEntry`, and the expectation is derived from the
  cntlrs themselves — one record per non-disabled cntlr — never from a
  hand-written pair of addresses. The three direct `nvme connect -n <nqn>`
  calls are:
  1. host1 to the transfer's subsystem: a `Transfer` has no `ss_id` and is in
     no `nqn_list`, so it is in no `CdcEntry` at all and cannot be discovered;
  2. host1 to the fallback source pool's subsystem, where a discovery-driven
     `connect-all` would also bring in `sp0`'s namespace — which carries the
     same uuid as the transfer namespace host1 already holds;
  3. host0 to the new primary after AR5, because the cdc still advertises the
     dead CN's transport until AR7 rewrites the entries.

* **E2E11 — each case starts from an empty etcd and a fresh sp.** **The intent
  holds and the mechanism is bigger than the design's.** An etcd reset alone is
  *not* enough and the suite argues it at length: a second `cluster create`
  stamps a new `creation_epoch` and therefore mints a different `cluster_id`
  and new `dn_id`s, while every DN's 4 KiB disk header still names the old
  ones — and `EnsureFormatted` refuses such a disk as *foreign* and never
  re-formats. Only a real `dn_cleanup` (zero the loop's first 4 KiB, `wipefs`,
  `losetup -d`, `rm -rf $WORK`) removes that header, and only the two cn phases
  remove a CN's store and its tmpfs arena. So the between-cases step is
  `cleanup_all`, then `cleanup_start_gate` — that sweep is the *start* cleanup
  of the case that follows, and a case cannot begin from a guest that was not
  swept (§6) — then a fresh `setup_infra`, then `setup_case`: the whole of
  `setup` with a teardown in front, so the run pays for a full rebuild per
  case. Both halves are needed and the second is the one that is easy to
  forget: `cleanup_all` has just removed the cluster along with everything
  else, so a between-cases step that stopped after `setup_infra` would leave
  the next case reading `sp get` against an empty etcd. `setup_case` is
  re-entrant by construction — its first act is to clear the ids, the cntlr
  arrays and `SHA0`, so nothing of the previous case can be read by mistake.
  The rebuild is also where the next case's thresholds are chosen:
  `sp_thresholds` runs immediately before `setup_between_cases`, because the
  `sp create` inside it is the one moment an `event_threshold` can be set
  (§2.3). `reset_control_plane` — stop the daemons, wipe `$WORK/etcd`, restart — stays
  defined for a case that wants a fresh etcd *without* rebuilding the data
  plane, and nothing calls it.

* **E2E12 — the shell literals mirror named constants and say so.** *As
  designed:* `SLICE_CNT_DEFAULT`, `ETCD_MAX_TXN_OPS`, `EXTENT_SIZE`.
  **CORRECTED for the middle one.** `ETCD_MAX_TXN_OPS` is **not** a literal in
  this suite: `read_constants` fills it at driver preflight from
  `workerctl constants`, along with `MaxAllocLegPerGrp`, which it cross-checks
  against `LEGS`. `start_etcd` dies rather than pass an empty `--max-txn-ops`.
  That is what the other suites do since the ceiling was raised, and it is the
  one constant this suite must not hand-copy, because this is the suite that
  actually issues that transaction — 951 compares at its two cntlrs, against
  the 967 the constant is sized by. The literals that do
  exist name their constants in a comment: `SLICE_CNT_DEFAULT`,
  `EXTENT_SIZE`, `NQN_PREFIX`, `CN_PORT_ID`, `TMPFS_DIR`, and the arithmetic
  mirrors (`STRIPE_SIZE`, `INIT_EXT_CNT`, `CNTLR_CNT`) with the rule they
  encode.

---

## 4. The cases

Four cases, run in this order, each from a freshly built storage pool:
`smoke`, `ops`, `copy`, `react`. `--only` picks one; a run that names none
gets all four, with a full teardown and rebuild between them — and each build
carries its own case's `event_threshold` set (§2.3), which is why the sp
`setup` builds is the *first* case's and not a neutral one. Every stage sets a stage
name and a trace id `it-<case>-<nn>`, which the gateway, the worker and every
agent OS command carry, so one `jq 'select(.trace_id=="…")'` over any log pulls
the whole stage.

Reading the tables: the left column is the dnvctl invocation (or the act, where
it is not one), the right column what is asserted *after* it. Every wait on a
condition is a bounded `wait_until`, and no step waits out a fixed interval for
something to become true. The five bare `sleep`s in the file are all in the
process-control helpers: a 0.5 s settle before the `kill -0` liveness probe on
a just-launched agent (`dn_up` and `cn_up` — a doomed agent needs a moment to
exit before "still running" means anything), and three post-signal settles,
after a `KILL` or between a TERM sweep and a KILL sweep. The react case reaches
one of those three when it kills an agent by its pid file; the rest belong to
setup and cleanup. Every other `sleep` in the file is the pause inside a poll
loop — `wait_until`'s own, the host-side blocking-safe probe's, and the
`kill -0`/`pgrep` loops those same helpers spin.

### 4.1 Setup — the sp every case works on

| stage | command / act | assertion |
|---|---|---|
| 01 | `mkwork` on all ten guests; scp the binaries; re-mask `nvmf-connect@.service` and `nvmf-connect.target` on both hosts | each of the four steps dies on failure, and the mask is **asserted** here as it is in preflight: the helper's two `systemctl mask` calls carry `|| true`, so since the third run the verb re-reads both units with `systemctl is-enabled` and prints the word `masked` only when both say it — anything else comes back as `service=… target=…` and fails the `assert_eq` with the states it actually found. An unconditional `echo masked` made that assertion prove nothing, and rule E2E10 rests on it. This re-mask is here because `host_cleanup` unmasks and this function is also the rebuild half of the between-cases step |
| 02 | start etcd, gateway, worker, cdc on cp | each listener is up before the next process needs it; then `cluster get` answers — and **`NOT_FOUND` is the healthy answer** against an empty etcd, so the readiness predicate treats it as success and anything else (`UNAVAILABLE` from a gateway that is not listening, `ABORTED` from an etcd that has not elected itself) as not-yet |
| 03 | `dn_up` × `DNS_PER_VM` per DN VM (sequentially — `losetup --find` races with itself), `cn_up` per CN VM | each reports a loop device, a non-zero `write_zeroes_max_bytes` and a live pid; then `preflight_loop_devices` re-reads all `DN_TOTAL` devices |
| 04 | `cluster create --name e2e --extent-size 67108864` | `cluster get`'s `cluster_id` equals the create reply's; `cluster_conf.dn_bin_conf.extent_size` is the value sent, and `bin0..bin3_shift` are 0/4/8/12 — the whole default ladder survives an extent-size-only request |
| 05 | `dn create` per instance with `--location dn<v>`; `cn create` per CN | each retried until accepted: the gateway calls the agent's `GetDnSize`/`GetCnSize` inline and a transport failure is **`ABORTED`**, not `UNAVAILABLE`; `ALREADY_EXISTS` counts as success, since that is what a retry sees when dnvctl's own timeout fired on a call the gateway had committed |
| 05 | `dn inspect` / `cn inspect` per node | all three `DnInfo` rows and all four `CnInfo` rows `RES_STATUS_OK`; **`dn_info.port_info.res_name` equals `k+1`** — the end-to-end proof that `--nvmet-port-id` reached the agent and that the agents of one kernel are not all converging `ports/1`; `cn_info.port_info.res_name` is `1` |
| 06 | `sp create --cntlr-cnt 2 --slice-cnt 32 --init-ext-cnt 1 --slots 0,1 --redund raid1 --stripe-size 1048576 --thr-*` — the four `--thr-*` words are **this case's set**, which `sp_thresholds` chose before setup was entered (§2.3) | `slice_list` is 32 slices with `slice_idx` 0..31 and no gap; every slice has exactly one meta group (1 extent) and one data group (`INIT_EXT_CNT` extents); every group has `LEGS` legs, every leg exactly one side; 128 sides on 128 **distinct** `addr_port`s; the `LEGS` legs of every group on `LEGS` different DN VMs; two cntlrs on distinct nodes, exactly one primary, none disabled; `cntlid_slot_list == [0,1]` and cntlr *i* carries `cntlid_slot_list[i]`; every **side** carries `cntlid_slot_list[0]`; the stored `stripe_size`, redundancy arm, `sp_level == SP_LEVEL_READWRITE`, `deleting == false`; and the four thresholds read back one by one against the **active** variables — nothing resolves an `event_threshold` on the way in, so that is both the proof that the case got the set it asked for and the proof that the argv string split into eight words instead of arriving as one |
| 07 | wait (three waits, all `WAIT_BUILD`; the last two repeat together if the role moves under them) | no side has `provisioned == false` (progress is logged whenever the count moves, with a denominator read out of the same reply); then the **primary** reports one `RES_STATUS_OK` pool per slice, one OK group per group and one OK leg per leg **of the shape the poll just read**, and `applied_revision ≥ 1`; the **standby** reports the same leg total and *empty* `grp_id_to_md_raid`, `slice_id_to_dm_pool`, `slice_id_to_meta`, `slice_id_to_data`, `td_id_to_raid0` and `td_id_to_thin_info` — empty maps, not `MISSING` rows, and a non-null `cntlr_info` is asserted first so that `null \| length == 0` cannot pass for "the standby builds nothing". Two of the six are **vacuous where setup makes them**: this stage runs before `td create`, so `td_id_to_raid0` and `td_id_to_thin_info` are empty on the *primary* too, and the shell says so at the function. They would start carrying weight in a step that re-checked a standby once a thin device existed, and no step does: the two later standby checks — the third cntlr of §4.3 stage 03 and the AR7 replacement of §4.5 stage 04 — assert only `grp_id_to_md_raid` and `slice_id_to_dm_pool`. This stage is where the first run died, and three things about it are new (see below the table) |
| 08 | `td create --name t0 --size 134217728` (4 × `TD_UNIT`) | `created` flips (only the sp-worker writes it); stored size and `ori_id == 0`; then the primary carries `t0`'s raid0 — a *different* row from `created`, and the one a namespace's dm-linear points at |
| 09 | `ss create`, `ss set-hosts`, `ns create --idx 1 --td t0 --uuid …8c01` | `ss list` shows one subsystem whose key is the `--nqn` string unmunged, `allowed_hosts` exactly the two hosts' own nqns, one namespace at nsid 1 with the chosen uuid, the right `td_id`, `suspended == false` |
| 10 | host0 discovers through the cdc on `cp:18020`; **the export gate**; `connect-all`; **the verdict** | the discovery log equals one record per non-disabled cntlr, derived from the cntlr list — a statement about what the *gateway wrote*, which is why it is not enough on its own (below the table). Then `wait_ns_exported_all` holds out, for **every non-disabled cntlr** and not only the primary, until that cntlr's own agent reports `ss_id_to_subsystem[<ss0's id>]` and `ns_id_to_namespace[<ns 1's id>]` `RES_STATUS_OK`, on `WAIT_PROVISION`. Then the connect, and then `connect_verdict`, which **dies when host0 holds no controller for `ss0`** — nvme-cli's exit status is reported and never believed. **Then the ANA wait, then the device** — a namespace whose only path has never been usable gets no head disk at all, so `wait_dev` before the ANA wait would burn its whole budget; host0's **ANA state** for ns 1 is `optimized` through the primary's controller and `inaccessible` through the standby's, while **both controllers' path state** is `live`. The two are different readings from different places — an ANA state is per namespace, read out of `/sys/class/nvme/<ctrl>/nvme*n*/ana_state`, and a path state is per controller, read out of `nvme list-subsys -o json` — and the suite says at that line that confusing them is how a failover test ends up asserting nothing |
| 11 | write 4 MiB of `/dev/urandom` at offset 0, drop caches, read back | the digest equals the pattern file's; that digest is `SHA0` |

**Stage 07 is where the first run died, and it is three different things now.**

1. **The target is the live shape, re-read on every poll.** The two stack waits
   go through `sp_totals_poll`, which takes its own `sp get` first — one extra
   round trip per poll — and refreshes the four `SP_*_TOTAL` globals before the
   `cntlr inspect` they compare against. (The sides wait needs no such thing:
   it has always read its count and its denominator out of the same reply.) The old form read the shape once (or,
   in setup's case, took it from the constant `GRP_CNT × LEGS`) and then waited
   for a number that had already stopped being true: when a spare leg appeared
   the agent correctly reported 129 leg rows, because CN10 walks
   `spare_leg_list` as well as `leg_list`, against a target frozen at 128. A
   wait whose target cannot be reached is not a slow wait; it is a hang with a
   stopwatch on it. The two fresh-sp predicates that had that defect are
   deleted, and the comment where they stood says so and keeps the three facts
   they carried that are still true.
2. **A shape that moves under a wait is shouted about.** While one of these
   waits runs the suite itself is blocked, so nothing it did can have moved the
   shape — an automatic reaction did, and the line names the case's threshold
   set. Under the quiet set that line is a finding about the run rather than a
   hiccup to absorb; under `react`'s set it is expected and the wait survives
   it.
3. **Neither wait is pinned to a controller id, and the stage exits only when
   the two agree.** `primary_stack_ready` takes no id: it reads whichever cntlr
   is primary in the reply it just fetched and compares the stack against that
   one, because a wait pinned to the controller that was primary when it
   started would, after a failover, be watching a node that by CN12 and CN13
   builds neither groups nor pools — an unreachable target again, and a full
   `WAIT_BUILD` spent before a message about a node doing exactly what a
   standby should. `standby_shape_ready` takes no id either, for the mirror
   image of the same reason: it holds out for the legs **and** for
   `grp_id_to_md_raid` and `slice_id_to_dm_pool` being empty, so pinned to the
   id that was the standby when it began it would, after a failover, be
   insisting that the new *primary* hold no groups and no pools — a target that
   node spends the whole build making less reachable. (The emptiness is why it
   waits at all: a standby is not always a controller that was born one, a
   demoted old primary tears its groups and pools down on its next syncup, and
   a wait on the leg count alone would reach `setup_assert_standby` while those
   rows were still there and fail a node that was a second too slow.) Both
   predicates resolve their role out of the `sp get` their own poll made; a
   reply that does not show exactly one primary — or, for the standby, exactly
   one non-primary, or whose `cntlr_list` and `cntlr_id_list` lengths disagree —
   sends the poll round again. That guard is **insurance, not a transient the
   election passes through**: `GetStoragePool` answers out of one `Snapshot`, so
   the whole reply is a single store revision; every writer of the `primary`
   flag leaves exactly one primary in that revision (`idx == 0` at create,
   `false` at `CreateCntlr`, the old cntlr's own flag at `ReplaceCntlr`, which
   deletes the old key in the same STM, and `model.Failover`, which flips both
   booleans in one STM); and `loadCntlrs` walks `cntlr_id_list` and returns
   `ABORTED` on a missing key, so the two lists cannot come back different
   lengths. The guard is what keeps a future non-atomic writer from being read
   as a stack that is merely unfinished. Because the two waits are
   **consecutive**, following the role inside each one is not enough: the role
   can move in the second, after the first has already asserted a complete
   stack, and the standby wait is exactly where that is likeliest — the
   teardown it waits for is what clears the demoted node's `err_epoch` and
   makes it a failover candidate again. So the pair runs in a loop, and its exit
   condition is **"the primary holds a complete stack now"** rather than "the
   same controller is still primary": a demotion is a teardown, so a node
   demoted and re-promoted inside the standby wait is primary under its old id
   with its pools and groups gone, and one extra `cntlr inspect` is what tells
   the two apart. Either failure sends the stage round again with a `!!!` line,
   up to `SETUP_STACK_ROUNDS` (3) times before it dies as non-convergent. That is
   what lets everything after this stage — `td create`'s raid0 wait, the
   namespace, host0's transport, all of which read `PRIMARY_*` — name a
   controller that holds a complete stack *now*. The standby wait is also why
   the stage asserts `CNTLR_CNT == 2` first: "the cntlr that is not the
   primary" names one controller only at §7.1's count.

Finally the stage says out loud what the build actually produced: a leg total
that is not `GRP_CNT × LEGS`, or any spare leg at all, gets a `!!!` line naming
the threshold set the sp carries. Under the quiet set that cannot happen
without a finding behind it; under `react`'s it is the case's own starting
point (§4.5).

**Stage 10 is where the third run died, and it is three things now.** The run
before it reached this stage with a clean build and then connected host0 to
nothing: `connect-all` exited 0 having made no controller at all, and the
stage spent its budget waiting for an ANA state on a path that did not exist.
§8 item 16 is the evidence; what the stage does about it is here.

1. **The export gate, ahead of the connect.** A discovery log proves that the
   *gateway* wrote a `CdcEntry` — it is built from the cntlr records and
   served out of etcd, so it advertises a subsystem and every transport of it
   before any agent has built anything. `wait_ns_exported_all` therefore waits
   for the other half, out of `cntlr inspect`: for **every non-disabled cntlr**
   — `connect-all` connects every record the log offers, and the third run lost
   both of them — the agent's own `ss_id_to_subsystem[<ss0's id>]` and
   `ns_id_to_namespace[<ns 1's id>]` rows must read `RES_STATUS_OK`, on
   `WAIT_PROVISION`. The subsystem row is the one that matters most, because
   `RES_STATUS_OK` on it includes the **link into the nvmet port**, and that
   link is what makes the port listen at all; the namespace row is the
   freshness proof, since its key did not exist before `ns create` and a row
   under it cannot have come from an older syncup. `= RES_STATUS_OK` and never
   "not `MISSING`": a provisioning-deferred chain reports
   `RES_STATUS_PROVISIONING`, which is not ready. One predicate serves both
   roles because a standby exports the subsystem too, its namespace sitting in
   the inaccessible ANA group (§8 item 3) — the third run's own dump has
   `ss_id_to_subsystem` `RES_STATUS_OK` on both cntlrs, with `epoch`s three
   seconds apart — the two moments this gate is waiting for, and both of them
   after the connect that run made.
2. **The verdict, after it.** The helper's `connect_all` now prints every
   controller the host holds for that subsystem and a machine-readable
   `ctrl_cnt=`, and `connect_verdict` dies on zero, naming what it means: that
   there is no ANA state without a controller, that the transports used were
   the ones the gateway stored, and that the place to look is the CN agent's
   log for when it created the nvmet subsystem and linked it to its port. The
   sentence about the exit status is branched on the status itself — over an
   `rc` of 0 it says that a zero is not evidence, and over a non-zero one it
   points at nvme-cli's own output instead, because there the tool did say so.
   It is branched on **which verb ran**, too: the silent zero was measured for
   `connect-all` and never for the plain `nvme connect` behind `host_connect`,
   so only the `connect-all` branch cites it as a measurement, and the sentence
   about what the failure looked like on the wire differs with it — an nvmet
   port with no subsystem linked to it does not listen at all, which is
   `ECONNREFUSED`, but an agent has **one** nvmet port shared by every
   subsystem *it* exports, and a CN guest runs exactly one cn agent ([D12]), so
   at a `host_connect` site the port can already be listening for a different
   subsystem and the reader must not be sent looking for `-111`. An unreadable `ctrl_cnt` is its own death, naming the
   causes that are reachable: a truncated ssh reply, or a helper that died
   before its verification tail. It does **not** name a stale helper —
   `ship_helpers` regenerates and re-ships all four helpers to every guest on
   every run, dying on a failed `scp`, before anything is built or connected,
   so re-shipping is a last resort there rather than the first move.
   **The count is per subsystem, which is not the question at two of the
   sites.** §4.3 stage 03 and §4.5 stage 04 connect-all to pick up one *new*
   transport while host0 still holds a live path to the primary, so `ctrl_cnt`
   is ≥ 1 there whatever the new transport did and the verdict cannot fire.
   Those two sites follow it with `connect_added_ctrl`, the per-address half:
   an instantaneous read of the controller for **that** traddr, so a
   connect-all that silently added nothing dies at the connect instead of
   `WAIT_HOST` later in the `host_path_live` wait. The check is
   deliberately **instantaneous** rather than a poll: the file's reason is that
   both nvme verbs are synchronous and nvme-cli does not retry a connect it
   failed, so a poll here would paper over exactly the race the gate in front
   of it removes — and the third run is the evidence, since both CNs were
   listening within nine seconds of its connect and host0 still held nothing
   when the stage gave up a minute later.
3. **The ANA wait is two waits on one budget.** `ana_of` answers `none` both
   for "this host holds no controller on that transport" and for "the
   controller is there but carries no namespace with that uuid", and the third
   run died of the first while the message described the second.
   `host_wait_ana` now waits first for *any* controller for that subsystem on
   that transport, then for the state, and the second wait gets the remainder
   of the caller's budget rather than a second copy of it — floored at 1 s,
   since `wait_until` tests its predicate before it tests the clock, so a
   1-second budget is still one honest attempt. The controller it resolved is
   kept in `ANA_CTRL` beside `ANA_LAST`: the ANA wait's message names it, and
   the failure context prints the pair as `last ANA:` and `last ctrl:`, which
   is what tells "no path" from "the path is up and the namespace is not on it"
   (§7). `cn_wait_ana` carries the same split one hop down, for the same
   reason.

The gate and the verdict are not local to this stage. `host_connect_all` and
`host_connect` are now the only two places the driver reaches nvme-cli's
connect verbs at all, and every one of the suite's seven connect sites has an
export gate in front of it: setup's here, the third cntlr of §4.3 stage 03 and
the post-ladder reconnect of §4.3 stage 05 (`wait_ns_exported_all`, because
those are `connect-all`s), the transfer of §4.4 stage 01
(`wait_xfer_exported` — a transfer's rows are keyed by its xfer id) and the
fallback source pool of §4.4 (`wait_ns_exported` through `--sp`), host0's
return to the new primary in §4.5 stage 03 (where the `cntlr_level_ready` wait
already in front of it asks the same question over every row, so no second poll
was added) and the AR7 replacement of §4.5 stage 04.

**And the gate is not only for connects.** Every `ns create` whose namespace a
host then reads gets one, whether or not anything connects. There are four of
them: setup's ns 1 and §4.4's fallback source pool, both listed above because a
connect follows them, plus §4.4 stage 02's second namespace and §4.5 stage
01's, where nothing connects at all — host0 has held the controller since setup
and the kernel picks the new namespace up on the AEN. `ns create` returning is
still only a control-plane fact, and the gate is what makes a slow converge
fail naming the missing row instead of spending `WAIT_HOST` on the ANA state of
a namespace that does not exist yet. §4.4 stage 02's is the one with the most
behind it — CN16 rule 5 backs that namespace with the **live dm-clone**, so the
CN has strictly more to build there than at §4.5 stage 01 — and it was the site
the first sweep missed, because the sweep was made by grepping `connect`, and a
site that connects nothing cannot be found that way.

### 4.2 Case `smoke`

Setup is the subject; the case adds no operation of its own. It exists for the
pair of statements the other three assume and none of them proves: that the
widest sp this tree can build comes up whole, and that deleting it gives every
extent back and leaves nothing behind. It runs first, so a lab that cannot
build the shape fails after one build rather than four.

| stage | act | assertion |
|---|---|---|
| 01 | `sp get` | still the sp setup created; 32 slices, 128 sides, **no spare leg at all**, `SP_LEVEL_READWRITE`; `SHA0` re-read |
| 90-92 | the shared ending | §4.6 |

The two counts in that row are deliberately **absolute**, and since §2.3's
per-case thresholds they carry a second statement as well as the first:
`smoke`'s sp is built under the quiet set, so a side count that is not
`GRP_CNT × LEGS`, or a spare leg at all, means a reaction fired when none
could — a finding about the lab, not a shape to accommodate. It is the
opposite choice from `react`'s (§4.5), made for the opposite reason.

### 4.3 Case `ops`

Every sp-scoped mutator and reader, in nine stages. `SHA0` is re-read after
each stage the design marks. Its sp carries the quiet threshold set (§2.3), so
every count below is an absolute: while the case runs, the only thing that can
change the sp's shape is the case.

| stage | command | assertion |
|---|---|---|
| 01 | `sp get`, `sp list`, `sp find-names --ids <sp_id>` | `sp list` names `sp0` exactly once; `find-names` maps the id back to the name, keyed by the **quoted decimal string** protojson renders a uint64 map key as, and answers exactly the one id asked about |
| 02 | `sp grow-slice --slice <s0> --meta`; `sp grow-slice --slice <s1> --ext 1` | slice *s0* has two meta groups and the appended one is 1 extent (the ladder appends the slice's current meta total); slice *s1* has two data groups and the appended one carries **the slice's first data group's `ext_cnt`, not `--ext`** — `--ext` is only the exclusivity signal; every group still has `LEGS` legs on `LEGS` different VMs; the new sides provision and the primary's pools stay OK. It deliberately does **not** re-assert that every side of the sp is on a distinct DN: that is a create property, and `GrowSlice` passes a nil black list |
| 03 | `sp set-cntlid-slots --slots 0,1,2` | `cntlid_slot_list == [0,1,2]` |
| 03 | `sp set-cntlid-slots --slots 1,2` → `INVALID_ARGUMENT` | the message contains `cntlid_slot_list drops slot 0, which cntlr` — the handler checks every **cntlr** before it checks any side, so this is the cntlr loop's message even though it is also true that every side holds slot 0 |
| 03 | `sp set-cntlid-slots --slots 0,2` → `INVALID_ARGUMENT` | `… drops slot 1, which cntlr`; and a refused call changed nothing |
| 03 | `cntlr create --slot 2 --cn-white <spare cn>` | a third cntlr on that CN with `cntlid_slot 2`, **not** primary, **not** disabled; it connects every leg as a standby with no groups and no pools — on `WAIT_BUILD`, because a controller born now holds nothing and every leg is a fresh nvme-tcp connection, which is the standby half of a build rather than an incremental convergence; the cdc advertises the third transport — and being in the log is not the same as listening, so the export gate of §4.1 stage 10 runs over all three cntlrs before host0 reconnects, the two older ones answering on the first poll; host0's third path goes `live` and the namespace is `inaccessible` on it. **This half of the stage is skipped with a log line when no CN is free** — when every `--cn` guest already carries a cntlr of the sp — exactly as stage 08's CN half is; the refusals above still run and `SHA0` is re-read before the return. Neither skip can fire at an invocation the suite accepts (`--cn` is at least 3 and `CNTLR_CNT` is fixed at 2, so a spare CN always exists); both are guards against a shape a future flag could introduce, not branches the lab takes |
| 03 | `cntlr delete --id <c3>` while enabled → `FAILED_PRECONDITION` | `is enabled; disable it first` |
| 03 | `cntlr set-enabled --id <c3> --enabled=false`, then `cntlr delete` | the disabled cntlr's transport leaves the discovery log at once; after the delete the sp is back to two cntlrs and host0 loses that path by itself (the subsystem disappears under a live controller and the reconnect is refused with DNR) |
| 04 | `sp inspect-side --id <a side>`, `dn inspect`, `cn inspect`, `cntlr inspect` | the side's data device OK and `zeroed_ext_cnt == total_ext_cnt`, `applied_revision ≥ 1`; the DN's three rows OK with `port_info.res_name` still its own port id; the primary CN's four node rows OK; the primary cntlr's pools and groups OK |
| 05 | `sp set-level` down `READONLY → NO_CLONE → NO_THINPOOL → NO_REDUND → NO_MIGRATION → NO_SIDE → DISABLE` and back up to `READWRITE` | at each rung the stored `sp_level`, then **the documented shape**: every row of every map the level suppresses is present and `RES_STATUS_MISSING` with `details == "sp_level"`, and the rows it does not suppress are OK. A map with **no** rows is not accepted as "suppressed" — that is what a null `cntlr_info` looks like. `READONLY` has no `CntlrInfo` signature at all (the ns-dev is reloaded onto a dm-flakey `error_writes` table over its normal backing, which probes as the expected table), so that rung asserts the **host** instead: the data is still readable, through the blocking-safe probe. Every rung is bounded by `WAIT_BUILD` and not `WAIT_PROVISION`: `DISABLE` suppresses everything CN19 names, so the CN tears the whole stack down and the climb back builds all 32 pools and all 64 arrays again — the same work setup pays for. One budget for all of them, because the cheap rungs return on their first poll and cost nothing |
| 05 | after the ladder | host0 lost its controller when `DISABLE` removed the subsystem under it, so it discovers and connects again — behind the export gate over **every** cntlr, because the ladder is sp-scoped and the standby tore its own export down and rebuilt it too while `ops_set_level`'s wait watched only the primary. That gate is the **only** wait covering the standby here, and what it is waiting for there is a from-nothing rebuild, so it is given `WAIT_BUILD` rather than the default `WAIT_PROVISION`: `build()` is sequential with legs first and the subsystem among the last rows, so the standby's `ss_id_to_subsystem` cannot go `RES_STATUS_OK` until every leg has been reconnected — the same piece of work the rung above spends a `WAIT_BUILD` on for the primary. The primary returns on the first poll, so the wider budget costs nothing when nothing is wrong. Then the verdict, ANA first, device second; `SHA0` |
| 06 | `td create --name s0 --ori t0 --size 0` | a snapshot is the one case in which size 0 is legal; it inherits the origin's size and its `ori_id` is `t0`'s `dev_id` |
| 06 | `td get-bm --name t0 --slice-idx 0 --start 0 --cnt 0` | `--cnt 0` is the whole slice; the reply is the hex map, `byte_cnt` renders as a **bare number** (a Go `int`, unlike every uint64 here), `bitmap_hex` is exactly two hex digits per byte, and **bit 0 of byte 0 is clear**. Mind the polarity, because it inverts the obvious assertion: `1 = unmapped` is the wire convention of every bitmap RPC, produced by the cn agent — which starts from an all-ones map and *clears* the range of every mapped extent, inverting thin metadata's native "mapped = written" exactly once at that boundary — and passed through verbatim by the gateway. So an allocated block is a **clear** bit, and "some bit is set" would pass on a thin device nobody has ever written. What the check pins is block 0 of slice 0: dm-striped maps chunk *c* of a td to slice *c* mod `slice_cnt`, so setup's write at offset 0 is block 0 of slice 0's thin volume at every shape, and bits are LSB-first within a byte, which puts that block in the low bit of the first two hex digits |
| 06 | `td get-leg-bm --leg <slice 0 data leg>` | the same hex map. **Skipped with a log line under `--redund none`**, where a group has one leg and no md bitmap |
| 06 | `td create --name t1 --size <t0's size>` | three thin devices; the primary carries three raid0s |
| 07 | `ns set-suspended --idx 1 --suspended` | the path goes `inaccessible` **and the head disk stays** — a suspend is a park (the ns-dev is pointed at the td's dm-error), not a removal, so `wait_dev_gone` would time out here. No host IO at all between the suspend and the resume: a parked ns-dev requeues, and even the cache drop issues a `sync` |
| 07 | `ns set-suspended --suspended=false` | `optimized` again; `SHA0` |
| 07 | `ns set-dev --idx 1 --td t1` | host0 reads the digest of 4 MiB of zeros — computed on the host from `/dev/zero`, not merely asserted to differ |
| 07 | `ns set-dev --idx 1 --td t0` | `SHA0` |
| 08 | `dn set-disabled --addr <a DN with no side> --disabled`, then `sp grow-slice … --dn-white <LEGS free DNs on LEGS VMs, one of them the disabled one>` → `RESOURCE_EXHAUSTED` | `disabled` is a scheduling flag and invisible to the agent, so the only proof is a refused allocation. The white list is exactly `LEGS` DNs on `LEGS` different VMs: one fewer, or two on one VM, and the step would prove nothing |
| 08 | `cn set-disabled --addr <spare cn> --disabled`, then `cntlr create --slot 2 --cn-white <that cn>` → `RESOURCE_EXHAUSTED` | `no controller node`; **skipped with a log line when every CN already carries a cntlr** |
| 08 | re-enable both; `sp get` | neither refusal wrote anything: both happen before the transaction |
| 09 | `td delete s0`, `td delete t1` | only `t0` is left for the teardown; `SHA0` |

### 4.4 Case `copy`

The four RPC groups that move bytes: transfer, clone, migration, spare leg.
Its sp carries the quiet threshold set (§2.3), which matters twice here: the
case's counts are absolutes like `ops`'s, and its thresholds do not invite the
one event that would cost it the most — a failover in the middle of a
hydration, which would invalidate the digest comparison the whole fallback
exists to make. Three deviations from the design's literal wording, each
forced.

**(a) All host0 IO happens after the transfer is deleted.** The design says
"no host0 IO from here until step 4" and then asks for a host0 read while the
origin namespace is still parked; the two cannot both be obeyed. What is
gained is a *stronger* assertion: read while the clone still exists, the
destination is reached through `CnCloneFinalName`, which serves an unhydrated
region **from the source** — so a clone that copied nothing would still answer
correctly. Read after the clone is gone, it is reached through the
destination's own raid0, which holds only what hydration wrote.

**(b) host1 connects to the transfer directly.** A `Transfer` has no `ss_id`
and appears in no `CdcEntry`, so `nvme discover` cannot show it. It is also
why host1 and not host0 consumes it: the transfer's namespace carries the
**origin namespace's** uuid and nguid, so the two must never be on one kernel.

**(c) `spare switch` and `spare delete` take `--grp`.** All three spare leaves
declare it, because each request carries `grp_id` and the handler locates the
slice from it.

| stage | command | assertion |
|---|---|---|
| 01 | `xfer create --name x0 --ori-nqn <ss0> --ori-idx 1 --hosts <host1> --auto-suspend` | `xfer get` echoes the id, origin, `auto_suspend`, `allowed_hosts`; `xfer_name_list` names it; host0's ns 1 goes **`inaccessible` with its head disk intact** (an effective suspend is a park) |
| 01 | host1 `nvme connect -n <XferNqn>` to the primary's transport | first the export gate, `wait_xfer_exported` on the primary's `xfer_id_to_subsystem` and `xfer_id_to_namespace` rows — **and the park that precedes it is not that gate**: a cntlr converges in one retire phase top-down and then one build phase bottom-up, and the origin namespace's park belongs to the retire half while the transfer's subsystem belongs to the build half, so host0's ns 1 can already read `inaccessible` while `XferNqn` does not exist on that CN yet. Then the connect and its verdict; ANA `optimized`, then the device; the path is `live`; host1 reads `SHA0` through it — the transfer exports `t0`'s raid0 |
| 02 | `td create --name c0` (the destination), wait for its raid0 | the dm-clone's `dest` argument needs the raid0, which `created` does not cover |
| 02 | `xfer set-hosts --name x0 --hosts <host1>,<every CN's CnHostNqn>` | `allowed_hosts` is host1 plus all CN host nqns (the list *replaces*, so host1 is repeated or it loses its connection); **then the kernel's own answer is waited for** — the `allowed_hosts` symlink under the transfer's nvmet subsystem on the primary CN — so the clone's first connect is not refused for want of a link that is still only a record |
| 02 | `clone create --name k0 --dst-td c0 --src-nqn <XferNqn> --src-idx 1 --src-slices … --src-stripe … --src-block … --src-tr-* … --auto-resume` | `clone get` echoes every geometry field; all four `--src-tr-*` are passed, because those flags declare **defaults** (tcp/ipv4/127.0.0.1/4420) rather than empty strings and an omitted one would silently send the loopback |
| 02 | `clone append-bm --name k0 --src-slice-idx 0 --bm-idx 0 --bm-hex <from the source's `td get-bm`>` | the reply echoes the clone id. The bitmap is fed through **uninverted**: 1 = unwritten is the wire convention of every bitmap RPC and `GetThinDeviceBitmap` already answers in it. What the chunk *skips* is deliberately not asserted — the fold counts an absent or short chunk as written, the safe direction |
| 02 | `clone set-tr` with the same transport | exercises the RPC without moving anything; the agent skips an entry it is already connected to |
| 02 | wait for hydration | the primary's `clone_id_to_dm_clone` details are the raw `dmsetup status` line; field 7 is `<hydrated>/<total>`, the same field the gateway parses before it allows `clone delete`. The wait is also the fallback's trigger — see below |
| 02 | `ns create --idx 2 --td c0 --uuid …8c02` | first **the export gate** on the primary's `ns_id_to_namespace` row for ns 2, although nothing connects here — host0 has held the controller to `ss0` since setup and the kernel picks the namespace up on the AEN, but `ns create` returning is still only the gateway's answer, and this namespace is backed by the **live dm-clone** (CN16 rule 5), so the CN has strictly more to build than §4.5 stage 01's has; without the gate a slow converge spends the whole `WAIT_HOST` of the ANA wait and then blames ANA. Then `optimized` and a device for host0 (both reads are sysfs and `test -e`, so they are legal while ns 1 is parked); its digest is read in stage 03 |
| 02 | `clone delete --name k0` (no `--force`) | the gateway proves hydration from the primary's own row before it latches, so a successful call is a second, independent confirmation; `deleting == true` is read if the drain has not already finished, then `clone get` → `NOT_FOUND` and `clone_name_list` is empty |
| 03 | host1 disconnects, then `xfer delete --name x0 --force` | `--force` is the **abort** path: without it the same STM also writes `suspended = true` on the origin, finalising the hand-over. host1 lets go first, or the subsystem would be unlinked under a live controller and the kernel would delete it with DNR |
| 03 | — | `xfer get` → `NOT_FOUND`, `xfer_name_list` empty; host0's origin namespace is `optimized` with its device back; **the destination's digest now equals the source's**, read through `c0`'s own raid0 with no dm-clone above it; `SHA0` |
| 03 | `ns delete --idx 2`, `td delete c0` | the head disk goes (a real removal, the one direction `wait_dev_gone` means anything) |
| 04 | read the leg bitmap **before** `migr create` | a migration source goes ANA-inaccessible the moment the migration exists and the destination stays inaccessible until its dm-clone is built, so between the two the leg has no usable path on any CN; the read costs nothing earlier and removes the question |
| 04 | `migr create --name m0 --src-side <side of slice 0's data group, leg 0>` | the leg has two sides; the destination is **not** on a disk node the group already occupies (the black list is unconditional) and **not on a VM it occupies** when `DN_VM_CNT > LEGS`; the two sides hold different cntlid slots. The migration window can be long, and what keeps AR8 out of it is not a number: the worker skips a leg with two sides outright, so the window is invisible to leg repair however long it lasts. The quiet thresholds this case's sp carries are the belt, not the argument — the same step would be safe under `react`'s set |
| 04 | `migr append-bm --name m0 --bm-hex <the reading above>` | `bm_cnt` goes 0 → 1. Skipped with a log line when the leg bitmap is empty, since an empty `--bm-hex` is refused on purpose |
| 04 | wait, then `migr finish --name m0` | the destination side's own `migr_dst_info.dm_clone_info` is hydrated — the same measurement `FinishMigration` makes — so the RPC cannot be refused for lack of proof; afterwards the leg has one side, that side is the destination, the **leg id is unchanged** (a migration moves a side, not a leg), and `migr_name_list` is empty |
| 04 | wait on the primary CN's own path to the surviving side | the record is gone but the DN rewrites `ana_grpid` on its *next* syncup; an inaccessible namespace requeues rather than errors, and a requeued read is exactly what `timeout` cannot bound — so this wait is what makes the next `SHA0` a read and not a gamble |
| 04 | `migr create --name m1 --src-side <the previous destination>`, then `migr cancel --name m1` | a migrated-onto side is an ordinary side; the cancel leaves one side, the source, untouched; the same ANA wait, then `SHA0` |
| 05 | `spare create --grp <slice 0's data group>` | one spare leg with one side, the **active** leg list unchanged (a spare is not an md member); the same DN and VM exclusions as the migration destination; its side provisions, and both cntlrs connect it — the leg-row count now includes spare legs, which is why the general `sp_totals`-driven predicate exists |
| 05 | `spare switch --grp … --spare … --target …` | the reply's `curr_active_leg_id` / `curr_spare_leg_id`; the promoted spare is in the active list and the replaced leg is parked; the group still has `LEGS` active legs |
| 05 | wait for md | `RES_STATUS_OK` alone proves nothing — a rebuilding array is OK with a `State:` of "clean, degraded, recovering" — so the words are read, and the wait is additionally gated on the CN having applied the `SpRev` the switch bumped to, because a switch changes *which* legs the group has and not how many, and the pre-switch array also reads "clean" |
| 05 | `spare delete --grp … --leg <the parked one>` | no spare leg; the primary drops its row; `SHA0`. **The whole stage is skipped with a log line under `--redund none`**, where the gateway would refuse it anyway |

**The clone source, and its fallback.** The source is a transfer of the *same*
sp: the primary CN opens an nvme-tcp connection to its own nvmet port. Nothing
in the tree refuses that, so it is legal by construction — but whether one
kernel can be both initiator and target for the same bytes is a property of the
lab's kernel, not of this tree, so the suite does not assume it. Two
conditions route to the fallback, each read from the agent's own report:

* the primary's `clone_id_to_target` row is still not OK `WAIT_SRC_CONNECT`
  seconds after the clone was created — the connect never came up;
* the dm-clone is OK but its `<hydrated>/<total>` has not moved for
  `WAIT_HYDRATE_STALL` seconds — a same-kernel loopback that deadlocks under
  writeback pressure *hangs* rather than failing, and without a stall detector
  the case would burn its whole budget and never reach the fallback.

The fallback abandons the clone (`--force`, since hydration is unproven by
definition) and **rebuilds the destination thin device** rather than reusing
it: a partially hydrated destination violates both the "never written before
the clone" contract and the recovery rule that equates "mapped in the
destination pool" with "already copied". It then builds a second storage pool
`sp1` — one slice, `--redund none`, one cntlr pinned with `--cn-white` to a CN
that is *not* `sp0`'s primary, and the same `--thr-*` words `sp0` got, since
`sp_thresholds` chose them for the case this pool lives inside — gives it a
thin device, a subsystem and a namespace, has host1 write a pattern into it,
and clones from that over a real network hop. The destination is still proved
byte for byte; it is simply no longer proved against `SHA0`. A fallback that
also fails is a die naming both faults, never a third attempt. `sp1` is torn
down before the case's shared ending.

`sp1`'s readiness predicate is **the one wait target left in the suite that is
computed from constants instead of re-read**, and the file argues for it rather
than leaving it to be noticed: this pool is built once with one slice, one
cntlr and `--redund none`,
and nothing can move its shape — a spare needs raid1 and both `spare create`
and AR8 refuse a RedundNone group, AR5 needs a failover candidate and there is
exactly one controller, AR6 would need the pool over its low-water mark and it
holds one copy of `t0` with nothing else written into it, and it carries the
quiet set besides. A shape that cannot move may be compared against a constant;
`sp0`'s can, which is why it is not.

`sp1`'s subsystem and namespace get the same export gate as every other connect
in the suite (§4.1 stage 10): `ss create` and `ns create` returning are
control-plane facts, and this pool's own single cntlr still has to build the
nvmet objects behind them. The two ids the gate needs are read out of those two
replies and refused if they are empty, non-decimal or 0.

The design's "add +2 to `DNS_PER_VM` in that branch" is unnecessary and the
suite does not do it: a DN that already carries one side still reports
`free_ext_cnt > 0`, and a create's black list excludes the DNs *that create*
picked, not the DNs another sp uses.

### 4.5 Case `react`

The four automatic reactions, each triggered by a real fault and each asserted
from the record the reaction actually writes. The worker takes **at most one
action per pass** per sp, so every wait *on a reaction* is "threshold plus a
few 5 s passes" and never a sleep; the waits on what a reaction leaves behind
are the ordinary build and convergence budgets.

**This case's own build may have reacted before the case starts.** It is the
one case whose sp carries the reacting set — 5 / 20 / 20 / 30 s — and it has to
be: AR7 waits `cntlr_unhealthy` and AR8 waits `side_unhealthy` or
`leg_unhealthy`, and at the gateway defaults — 600, 600 and 1200 seconds —
neither reaction is observable inside a bound this suite could wait out. Since
the thresholds are fixed at `sp create` (§2.3), the values the case needs are
the values its *build* runs under — and the build is exactly the work that
trips them (§8 item 13). So when the case begins, the sp
may already hold a spare leg nobody asked for and the primary may be a
different controller than the create elected. That is expected, and every
assertion below is written against the shape the case actually starts from
rather than against a pristine count:

* **Stage 00, `react_snapshot`,** is one `sp get` taken before anything is done
  to the sp: the group total, the active-leg total, the spare total, slice 0's
  data-group count and the current primary, all logged, with a `!!!` line when
  that is not the fresh-sp shape. **One of the five is read by a later
  assertion** — slice 0's data-group count, by stage 01. The other four are
  recorded for that log line and for the shout, and nothing else reads them: the
  primary because AR5 is the thing under test and every step re-reads the roles
  for itself, and the three totals because every step judged on a delta takes
  its own `before` reading (below) instead.
* **AR6's proof is a delta of one**, taken from readings the AR6 step makes
  itself immediately before the write it is judged on.
* **AR8's proof is "the group gained one spare, and then the dead leg was
  parked in it"**, against the ids the group held before the kill.

Two of its waits carry `WAIT_BUILD` rather than `WAIT_PROVISION`: the new
primary of stage 03, which is the whole build of §8 item 12's window on a node
that had only legs — the very thing that makes the failover loop
self-defeating — and the AR7 replacement of stage 04, which is its standby half,
every leg connected from nothing.

**And stage 03's is not a longer budget on the old predicate.** Twenty minutes
spent watching one named controller is the setup hang moved, not fixed: for the
whole of that rebuild AR5 can fire *again*, because AR7 has by then minted the
replacement on an idle CN and a healthy non-primary cntlr is all
`failoverEligible` asks for — while the rebuild is exactly the work that makes
the node doing it miss a 5 s `primary_unhealthy`. Pinned, the wait would then be
comparing a standby against 32 pools and 64 groups, the `legs 129/128` shape of
§8 item 12 one role over. So stage 03 uses `react_new_primary_ready`: it follows
the role for its progress line, like setup's, but **dies the moment the role
leaves the controller AR5 elected**, and stage 04 repeats that check before its
own assertions. Setup can absorb a move because any cntlr may build its stack;
this case cannot, because stage 04 resolves the replacement *by elimination from
the primary* and asserts that the replacement is a standby — after a second AR5
neither sentence is true of a tree that behaved correctly. Failing in seconds
with that named is worth more than twenty minutes and then a misleading
assertion.

| stage | act | assertion |
|---|---|---|
| 00 | `sp get` | the snapshot above: the numbers are recorded, logged, and shouted about when they are not the fresh-sp shape. The one thing it asserts is that slice 0 reports at least one data group, which no `sp create` can violate — every slice is created with exactly one |
| 01 | `td create --name a0 --size 2 GiB`, `ns create --idx 2 --td a0 --uuid …8c02` | `slice_list[0].slice_idx == 0` (the stripe every strided write lands in); slice 0's data-group count is **still the one stage 00 recorded** — not the literal 1, so a build that grew slice 0 by itself does not fail the case here, and stage 00 has already shouted if it did; `stripe_size == data_block_size == 1 MiB`, so one strided 1 MiB write is exactly one new thin block; `low_water_mark_pct` in 1..100 — a 0 is refused by the pass gate and anything above 100 switches AR6 off; then the export gate on the primary's `ns_id_to_namespace` row for ns 2, although nothing connects here — host0 already holds the controller and the kernel picks the namespace up on the AEN, but `ns create` returning is still only the gateway's answer, and without the gate a slow converge would spend `WAIT_HOST` reporting an ANA state for a namespace the CN has not made; the device appears for host0 |
| 02 | strided 1 MiB writes at every `SLICE_CNT × 1 MiB` of the device | **the chunk count is computed, not the design's literal 40**: `floor(lwm × total / 100) + 1 − used + 4`, from the primary's own pool `used/total` pair, because that is the pair the worker compares. Three guards: the chunks must fit in `a0`'s per-slice thin volume, must stay *inside* the pool (filling it would put dm-thin into out-of-space mode instead of tripping AR6), and must stay under a sanity cap, since every chunk is 1 MiB on **every leg** of the group |
| 02 | wait for AR6 | slice 0 gained **exactly one** data group and the sp gained exactly one group, both against readings this step takes for itself just before the write — with `sp_read_roles` among them, because the role may have moved since stage 01 and inspecting a controller that is now a standby would find no pool row to read a `used/total` ratio out of. `data_grp_list[0]` is still the group setup created (a grow appends), so the appended group's index is the count *before* the grow rather than the literal `[1]`; the new group's `ext_cnt` is the first data group's; `LEGS` legs, one side each, on `LEGS` distinct DNs on `LEGS` different VMs. It is deliberately **not** asserted that the new group avoids the DNs the slice already occupies — the design says it does and the worker's own comment says the opposite: the grow passes a nil black list |
| 02 | wait for the device | the pool's data **total** grows: dm-thin reports it in its own status line, so a bigger total is the CN having reloaded the pool over the wider concat — proof the grow reached the device and not only etcd. And the grown pool is back under the mark, so slice 0 is not grown a second time |
| 02 | read back | every strided chunk after a cache drop; `SHA0` for ns 1 too |
| 03 | **host0 disconnects from `ss0` first**, then the primary's cn agent is killed by its pid file | this act is not in the design and is not optional. nvmet objects outlive the agent that made them, so the dead CN goes on advertising its namespaces as `optimized` with nothing left to rewrite `ana_grpid`; the instant AR5 promotes the standby, host0 would hold two optimized paths to one namespace and a write down the stale one would allocate blocks in a dm-thin metadata image the new primary also owns. A real node failure takes that path down; a killed process does not |
| 03 | wait for AR5 | exactly one cntlr is primary and it is not the killed one; it *is* the former standby (asserted only because `CNTLR_CNT == 2` makes the election predictable, and that assumption is itself asserted); the dead cntlr's **record survives**, listed as a non-primary — AR5 writes two `primary` flags and bumps `SpRev`, and a cntlr count can therefore never be this step's assertion |
| 03 | wait for the new primary (`WAIT_BUILD`, `react_new_primary_ready`) | it builds what a standby never had: one thin pool per slice and one device per group — counted from the current `sp get`, not from the shape setup created, since AR6 has already appended a group — plus every leg and both raid0s; and then the whole `READWRITE` shape including the subsystem, namespace and ns-dev rows, because the cntlr builds bottom-up and a connect issued on the strength of the raid0 alone can be refused by a target that has not created the subsystem yet. A second AR5 during that rebuild **stops the run there**, naming both controllers, for the reason above the table; the two smaller waits after it stay pinned to the same controller, since by then the spawn storm is over and a wrong target costs `WAIT_PROVISION` rather than `WAIT_BUILD` |
| 03 | host0 connects to the new primary **directly** | the cdc still advertises the dead CN until AR7; the export gate is the `cntlr_level_ready` wait in the row above, which asks the same question over every row of the cntlr rather than two of them, so no second poll was added — the connect is still followed by its verdict; `optimized` and a device for both namespaces; host0 holds **no** path to the dead CN; `SHA0`; then a fresh 4 MiB write at 1 MiB into `a0` (slices 1..4, so neither slice 0's accounting nor the strided chunks) and a read-back |
| 04 | leave the agent dead; wait for AR7 | the dead cntlr's id is gone from `cntlr_id_list` and the sp is back to `CNTLR_CNT` cntlrs. AR7 can only act on a cntlr AR5 has already demoted — it skips a primary while a failover candidate exists, and the model refuses it again inside its own STM — which is what makes the two reactions distinguishable at all. Then, before any assertion below: the primary is still the controller AR5 elected in stage 03, or the run stops. Every row below resolves the replacement by elimination from the primary, so a second AR5 in the window between the two steps would point them at the wrong controller |
| 04 | — | the replacement is on a CN that carried **no** cntlr when the kill happened (membership, not equality: with more than one such CN the pick is random); it has a **new** cntlr id, inherits the dead one's `cntlid_slot`, is a standby (a replacement carries the old cntlr's role, and AR5 had demoted it) and is enabled |
| 04 | — | the replacement connects every leg as a standby with no groups and no pools, on `WAIT_BUILD` for the same reason the third cntlr of §4.3 gets it; the discovery log has lost the dead CN's transport and gained the replacement's — which is what makes `connect-all` safe again, though being in the log is not being ready, so the export gate runs over both cntlrs first and the new primary answers on its first poll; host0's new path is `live` and the sp's namespace is `inaccessible` on it, and its path to the primary is still `live` |
| 04 | restart the killed cn agent | its node rows come back; its `cntlr_ptr_list` is **empty**; and it tears down the md arrays, dm devices and nvmet exports its dead predecessor left in that kernel — which the end-of-run cleanup would also do, but only at the end of the run, and the residue check runs before that |
| 05 | kill a dn agent **and drop its nvmet port** | killing the agent alone triggers nothing: its nvmet subsystem, port and dm-linear live in the kernel and outlive it, so the primary's probe IO still succeeds, the leg stays OK, `Leg.err_epoch` stays 0 and AR8 never fires. Both planes are needed — the gRPC rounds fail (side unhealthy) and the data path goes away (leg unhealthy). The leg is chosen so that its single side sits on a DN carrying exactly one side of the whole sp, because AR8 repairs the smallest unhealthy leg id and a DN with two sides would make two legs unhealthy |
| 05 | wait for AR8 | **one more spare than the group had**, read before the kill together with the ids themselves, so a group that already carried one cannot satisfy the wait on its first poll — which is what the old `== 1` form would have done, passing the step without AR8 having acted. Exactly one entry of `spare_leg_list` is new against that id set, and it has one side. Then the switch: the dead leg is out of `leg_list` **and** the group's parked set is exactly what it started with plus the dead leg, and the sp holds one more parked leg than when AR8 started — one more and not two, because `SwitchSpareLeg` takes the promoted spare out as it puts the dead leg in. Both halves are asserted, because either alone is also what a half-applied transaction looks like, and the promoted leg is read out of the dead leg's **position** in `leg_list` after the switch, not out of `spare_leg_list` before it. One honest limit, unchanged in kind from the old form: AR8 acts once per pass, so the create and the switch are different passes, but a poll that lands after both would find the *dead* leg as the new entry — the two assertions hold either way and only the log line would name the wrong leg |
| 05 | — | the spare is not on the dead node, not on a DN the group occupies, and not on a VM it occupies when `DN_VM_CNT > LEGS` — where "the group occupies" is read over its active legs **and its spare legs**, which is what the worker black-lists and therefore the statement AR8 actually makes; md finishes rebuilding onto it (a fresh spare has never been an md member, so this is a full recovery), gated on the applied revision as in the copy case; `SHA0` |
| 05 | restart the dn agent | it recreates **its own** nvmet port, not `ports/1`; the primary reports every leg again, the parked one included. It has to come back: the parked leg's side still occupies an extent, and only a live agent can retire it when the sp drains |
| 06 | `ns delete --idx 2`, `td delete a0` | only `t0` is left for the teardown; `SHA0` |

### 4.6 The ending every case shares

`smoke` *is* this ending; the other three put their stages in front of it.

**Stage 90 — teardown.** Before anything is deleted, the case must have cleaned
up after itself: exactly one subsystem (`ss0`), one namespace (idx 1) and one
thin device (`t0`) may reach the teardown, asserted first so that a case which
forgot its own objects fails with that sentence rather than with a bare
`FAILED_PRECONDITION` three calls later. Then, in this order and for these
reasons:

1. `ns delete --idx 1` **under the live controllers**, and `wait_dev_gone` —
   the one direction in which that wait means anything, because a suspend is a
   park and never removes the node;
2. both hosts `wipe` — every `$NQN_IT` and `$NQN_PREFIX` subsystem plus the
   discovery controller pointing at the cdc, and never `nvme disconnect-all`.
   Since the third run the verb **re-reads sysfs** and prints
   `wipe_left=<nqns>` when any of those connections survived, and this caller
   dies on that line: every `nvme disconnect` inside `wipe` is `|| true` with
   its output discarded, so the verb's own exit status says nothing, and step 3
   below is exactly where that matters;
3. `ss delete`, then `td delete` — a subsystem unlinked from its port under a
   live controller kills that controller with DNR and the host never reconnects
   by itself, which is why the hosts let go first;
4. `sp delete`, which **latches and returns**: the sp still exists when the
   reply arrives and the worker takes it apart in bounded steps, so
   `sp get` → `NOT_FOUND` is the only completion signal there is. This one call
   deliberately bypasses the success wrapper, so that a refusal reports *which*
   object is still there instead of "got '1', want '0'" — the pre-checks above
   cover subsystems and thin devices, while the precondition is five name lists
   (clones, transfers and migrations too);
5. `sp list` no longer names `sp0`.

**Stage 91 — residue.** Every disk node's `free_ext_cnt == total_ext_cnt`
*and* an empty `side_ptr_list` — checking both is what separates "the capacity
came back" from "the pointer list was cleared and the number was not", two
fields written by the same ledger flush. Then every DN guest holds no dm
device, no `$NQN_PREFIX:*` nvmet subsystem and — through the separate probe of
§8 item 8 — no dnv md array, which on a DN is the assertion that its md mask
held (§8 item 15); and every CN guest holds none of those three either.
Loop devices are deliberately in neither list: the
agents keep serving on them until cleanup, so they belong to the run and not to
the sp.

**Stage 92 — the space guard.** Two caps, measuring different things:

* `DN_CAP_BYTES` (256 MiB) — **allocated** bytes of one backing file, via
  `stat -c '%b %B'`. The whole space argument of this suite is that a sparse
  file stays sparse because side zeroing is `blkdiscard --zeroout` and the loop
  device turns WRITE ZEROES into a hole punch; a file that has materialised has
  exactly one cause, and this is the check that names it. A `stat` that could
  not be read is reported as `unknown` and fails, never as a silent 0.
* `RUN_CAP_BYTES` (8 GiB) — everything the run wrote on all ten guests,
  `$WORK` **plus** `/tmp/dnv-tmpfs`, counted separately because the tmpfs is not
  under `$WORK` and a guard that looked only there would miss a CN's whole
  clone-metadata arena.

Free space is re-asserted against **preflight's own floors** rather than a
separate number, so that the statement is "the run left the guest as usable as
preflight demanded it be". The hosts have no floor of their own (they run no
dnv binary and hold only a pattern file), so their numbers are reported and
counted but not asserted.

A separate, read-only pass (`space_note_case`) takes the same reading again
after each case for the run summary. It asserts nothing — a summary that could
fail a run would be a second, quieter copy of the same rule.

---

## 5. Preflight

Preflight runs **after** the unconditional start cleanup and **before** the
first setup write. The design said "before any cleanup or setup writes", and
that cannot be right for the checks that matter: a port check, a
`ports_busy` and an nvmet-port conflict check taken before the cleanup answer
about the *previous* run's corpses, not about whether this run can start.
`cnagent_test.sh` and `cdc_test.sh` put theirs in the same place for the same
reason, and nothing in the cleanup writes suite state — it only removes — so no
check is reading something this run made.

Since the second run one thing sits between them: `cleanup_start_gate` (§6).
The argument above holds only if the cleanup preflight follows actually *ran*.
A verb that never reached its sentinel turns every check below back into a
question about the previous run's corpses, and the answer arrives as a
preflight death that blames whatever the corpses collide with first — which is
exactly how the second run was read wrong.

It dies on the **first** failure, naming the guest and the fix. A preflight
that collected three problems and reported them together would still have to be
re-run after the first was fixed.

**Driver** (`preflight_driver`, before any guest is touched): `go`, `ssh`,
`scp`, `curl`, `tar`, `sha256sum`, `awk`, `sed`, `mktemp`; a JSON parser —
a system `jq`, else a `gojq` built into the gitignored `integtest/bin` with the
Go toolchain the driver already needs, and every filter in the file is written
to the intersection of the two; `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make
build` and the five binaries; `workerctl` and `cnagentctl`; then
`read_constants` (which must run before `start_etcd`, or `--max-txn-ops` would
arrive empty and etcd would refuse to start) and the pinned etcd tarball, which
is re-downloaded only when the cached one does not match the sha256 pin.

**Every guest:** passwordless ssh with `BatchMode`, checked for all ten before
any tool list is read — a guest that is simply down should say so first.

**DN and CN guests:** passwordless sudo; the tool list that role actually runs
(`dmsetup nvme losetup lsblk blkdiscard stat du df awk sed grep ss pgrep pkill
timeout fallocate tail`, plus `truncate wipefs dd` on a DN, `findmnt` on a CN,
and `mdadm` and `udevadm` on **both** — the md mask and the md stop run on both
node roles, so a DN without `mdadm` would make `dn_cleanup`'s `md_stop_all` a
silent no-op (and the mask's own `IMPORT{program}` names `/sbin/mdadm`) — which
is the second run's failure again and quieter (§8 item 15) — while a DN without
`udevadm` could not reload the rules `dn_up` has just written, so the mask may
still be inert as the agent starts, for as long as `systemd-udevd` takes to
notice the changed rules directory by itself; the list is
deliberately *not* `cnagent_test.sh`'s copied over: no guest here parses JSON,
reads thin metadata or runs `cmp`); `modprobe` of
`nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-clone dm-thin-pool raid1` and
`mount -t configfs` if it is not mounted (the agents hardcode the configfs path
and mount nothing themselves — these two writes are preconditions, not suite
state); the nvmet configfs tree exists; `nvme_core.multipath == Y`, which is
load-bearing on a DN too, because a migration destination is an nvme host and
reads its source's ANA state out of the hidden per-path device that only exists
under multipath; on **both** node roles, `/proc/mdstat` and that the stock
`64-md-raid-assembly.rules` honours `SYSTEMD_READY`, which is the single line
the mask `dn_up` and `cn_up` install works by setting — a stock rule that
ignored it would leave the mask inert on either role, and on a DN that means
the stray arrays straight back. The two readings are not the same statement.
On a CN `/proc/mdstat` is a capability the agent needs; on a DN it is the
hazard itself, the proof that this guest *can* assemble an array out of the
superblocks the CN writes through the side export (§8 item 15). Then a
`fallocate -p` punch-hole probe on `/var/tmp`, which is
what turns the agent's `blkdiscard --zeroout` into a hole punch and what the
whole space argument rests on; `MemAvailable ≥ 2 GiB`; free space under
`/var/tmp` ≥ 4 GiB on a CN and 4 GiB + `DNS_PER_VM × 64 MiB` on a DN; none of
this run's ports listening; and **no conflicting nvmet port**.

That last check is its own, because a configfs port is not a listening socket
until a subsystem is linked to it, so `ports_busy` cannot see it. Two distinct
collisions are refused: an **id** this run will use — `EnsurePort` is
probe-first but not read-only on a port that already exists, it reuses the
directory and rewrites the four `addr_*` attributes, so an agent would hijack a
stranger's port rather than fail — and a **service id** this run will bind,
since two nvmet ports cannot listen on one ip:port. The die names the suite the
port seems to belong to, derived from the other suites' own declarations
(4200 = the two agent suites, 14420-14423 = the cdc suite, 4420/4421 = the
nvme-tcp default and so a hand-made target).

**And then it says why the port is still standing, which is not one answer.**
This is the sentence the second run got wrong: it told the operator that the
start cleanup had refused a port on `addr_trsvcid` 4300 — a number inside this
suite's own band, which is the one case `port_drop` *removes* (§8 item 15).
Both dies now compute the cause with `port_cleanup_cause`, written against
`port_drop`'s own branches: an **empty** service id is debris `port_drop`
removes, an **in-band** one is this suite's own and `port_drop` removes it too,
so neither was left alone on purpose — while an **out-of-band** service id, or
one that is not a number at all, is the deliberate refusal the old sentence
claimed for everything. That refusal branch keeps the old wording, and it is
the only branch the wording was ever true for.

The two "`port_drop` would have removed this" branches share one remedy, and it
names the causes in the order the control flow leaves them. `port_drop` has a
**fourth** outcome the first three do not cover: it accepts the port, the
`rmdir` does not take, and it prints `port N STUCK …` and still returns 0 — so
the verb reaches its sentinel, `cleanup_report` raises `CLEANUP_DIRTY` and a
`!!!` line but not `CLEANUP_UNFINISHED`, and `cleanup_start_gate` (§6) lets the
run through to preflight. That makes `STUCK` the cause the remedy names first,
pointing at the `!!!` line from the sweep that has just run, which lists the
`ana_groups` and subsystems still in the directory.

What the remedy must *not* say is "look for a `WARNING:` line above": a missing
sentinel is exactly what the start gate dies on, so by the time preflight runs
there cannot be one. The message says so — a verb that stopped part way is
ruled out here — and names the one thing the gate cannot rule out, a stranger
that made the port *after* the sweep, which is rule E2E9 broken rather than a
cleanup that failed. Then `--cleanup-only`, and a hand `rmdir` with
`ana_groups/3` and `/2` first if it survives that.

**An id no sweep of this suite visits** is the other real cause, and it belongs
to one of the two dies only. The id-collision die fires for `1 ≤ id ≤ idmax`,
and `idmax` is `DNS_PER_VM` on a DN (which `parse_args` refuses above
`MAX_DNS_PER_VM`) or `CN_PORT_ID` on a CN — ids `dn_cleanup`'s
1..`MAX_DNS_PER_VM` sweep and `cn_cleanup_phase2`'s single `port_drop` both
walk, so there the alternative is impossible and printing it would repeat the
second run's mistake in a new place. The service-id die fires whatever the port
id is, so there a higher id on a CN VM really is swept by nothing here.
`port_cleanup_cause` therefore takes the caller's answer to "was this id swept?"
as an argument and prints the alternative only for the second die.

**Hosts:** passwordless sudo; `nvme uuidgen systemctl udevadm dd sha256sum awk
sed grep timeout du df tail` (`udevadm` because
`/dev/disk/by-id/nvme-uuid.<uuid>` is a udev symlink and there is no other
stable name; `du`/`df`/`tail` because a host runs the same space and log verbs
the nodes do); `modprobe nvme-tcp` and a runnable nvme-cli;
`nvme_core.multipath == Y`; the identity files, generated if absent and never
overwritten; `stafd`/`stacd` not active — `stacd` connects on its own, `stafd`
owns discovery controllers, and nvme-stas sends a DIM in-capsule to every
discovery controller it learns about; and the mask of `nvmf-connect@.service`
**and** `nvmf-connect.target`, asserted from the helper's own
`systemctl is-enabled` reading of both units rather than from the fact that it
ran two `systemctl mask` calls.
Finally, the two hosts' nqns must differ: two hosts that share one are one host
to the target, `ss set-hosts` would name the same entry twice, and the copy
case's "host1 sees the transfer, host0 does not" could not be told apart.

**cp:** ssh; `ss pgrep pkill awk sed du df tail nohup grep timeout`; the four
ports free; ≥ 2 GiB free under `/var/tmp`. No sudo check: cp is driven as a
plain user.

**Deferred:** `preflight_loop_devices` runs after the agents are started,
because the devices do not exist before that, and before the first
`sp create` (§3, rule E2E5).

---

## 6. Cleanup, and why the order is what it is

One function, `cleanup_all`, run at the start of every run, between cases, at
the end of a successful one, and alone under `--cleanup-only`. It never dies:
every guest call tolerates a non-zero status. They do not all tolerate it the
same way, and the difference is new since the second run — the closing
`fstrim -a` sweep is still `_ok` wrappers, while every cleanup **verb** now
goes through the *dying* wrapper with `cleanup_verb` catching the status itself
(`|| rc=$?`), because an `_ok` form throws that status away and the status is
the difference between "this guest was already clean" and "this guest still has
everything on it". What it does with it is **report**, through
`cleanup_report`: a missing sentinel line, with the reason read off the exit
status — 124 is `timeout`'s own and names the cause exactly, 255 is the ssh to
that guest or a verb that exited 255, a 0 without the line means the helper
there is stale, and anything else is printed as it stands — and any `REFUSED`
or `STUCK` nvmet port, with the owning-suite hint spelled out. Those two words
are the ones that matter to the next person, because **a leftover nvmet port
with live ana groups is what fails the next suite's setup.**

`CLEANUP_TIMEOUT` is **600 s**, and it is a wedge detector rather than a
budget: a per-verb ssh bound on a guest that is not converging anything. The
second run doubled it from 300 s, on the only measurement there is — with the
stray arrays stopped by hand, one `--cleanup-only` finished on all ten guests
inside the 300 s then in force, with no warning, leaving nothing behind. Read
that narrowly: it was the *second* sweep over that debris. The first ran to the
end on six of the ten guests and part way on the other four (`dn2`, `dn3`,
`cn0`, `cn2`), so the only guests still holding a DN's whole port-and-loop
debris — 43 and 43, which `ports_sweep` and `loop_teardown` sit too late in
`dn_cleanup` to have reached — were `dn2` and `dn3`. On the other eight the
measurement is of a sweep over what a previous sweep had already taken. The
per-verb times were not recorded either, so 300 s is an upper
bound on what was seen and not a reading of it, and 600 s is that bound
doubled: room for a *failed* run at 32 slices to leave more debris than a
successful one, and for the CN timeouts nobody has explained (§9). What the
start cleanup meets is usually either nothing or the remains of a run that
stopped part way — a successful run cleans up after itself, and tolerance of
absence is the whole of E2E6 at the start.

The bound sizes **two** heavy verbs, and the suite comment now says so rather
than only naming the DN. `dn_cleanup` on one DN VM is up to `DNS_PER_VM`
instances' worth — 43 nvmet ports with their ana_groups, 43 loop teardowns, the
dm devices of every kind and `md_stop_all`. `cn_cleanup_phase2` is at least as
heavy, and it is the verb that actually blew the old bound twice: on the CN
carrying the stack it is `disconnect_prefix` over up to 128 side connections at
`timeout 30` each, 64 `mdadm --stop`s at `timeout 15`, then nine dm kinds plus
`dm_remove_all`, where a device that will not go costs 10 + 10 + 15 s. Which CN
was carrying the stack in the run that timed out was not recorded, so not even
"it was the heavy one" is available as an explanation for `cn0` and `cn2` and
not `cn1` (§9).

**What the bound does not bound** is a task in uninterruptible D state. GNU
`timeout` signals and then waits for the child to be reaped, so an unkillable
one is never reaped: no 124, no `WARNING:` line, no `cleanup_start_gate`
verdict, just a run that stops. That class is reachable inside these verbs — a
`dmsetup remove` or a block-device scan against a suspended dm device (rule 5,
§8 item 9) — and `resume_suspended` running first is the only defence. What the
bound catches is the *killable* grind: `dm_force_remove` against a device
something else is holding open, which is the 2026-09-17 case.

The bound is also a wait. `cleanup_all` runs its verbs serially, 13 of them in
the ten-guest shape, so a sweep in which every one hits the bound is 130
minutes before the gate says anything — up from 65. That is not the operator's
first news, which is what makes it bearable: `cleanup_report` prints its
`WARNING:` for each verb as that verb returns, and the gate is the summary and
the verdict.

That `WARNING:` line is not a soft one, and the second run is what it costs to
walk past it. A verb that hits the bound stops **where it was**, not at a
convenient point, and `ports_sweep` sits near the *end* of `dn_cleanup`: on the
two DN guests where the verb ran past the 300 s then in force, this suite's own
43 nvmet ports were never swept, the run went on, and preflight died three
steps later on one of those ports with a message that blamed a refusal nothing
had made (§5, §8 item 15).

**So the start sweep now has a verdict: `cleanup_start_gate`.** It is the one
place in the file where a cleanup kills the run, and it is called after the
start sweep and after the between-cases sweep, nowhere else. The distinction it
rests on is E2E6's: the start cleanup is tolerant of **absence** — a guest with
nothing on it runs every verb to the end and prints its sentinel, and the
run goes on — while a verb that never printed its sentinel is debris that
outlived the sweep. The gate lists each guest, verb and status (a `timeout`
exit of 124 is reported as the timeout it is, everything else as the status it
was), then dies with `--cleanup-only` as the retry and a remedy **built from
the rows it just listed**, not from the one case that has been seen: the stray
md array and `cat /proc/mdstat` for a `dn*` timeout; for a `cn*` one, that
`cn_cleanup_phase2` has no recorded cause and is as heavy as anything here; for
`rc 255`, that the ssh or the passwordless sudo is what failed, since this gate
now runs *before* preflight's own check for exactly that; and for an exit of 0
without the sentinel, a stale helper. Handing an operator `cat /proc/mdstat` on
a guest they cannot reach is the shape of mistake this whole change exists to
remove. Nothing gates the **end** cleanup or `--cleanup-only` themselves: a die
there would skip the other nine guests, and `CLEANUP_DIRTY` already carries
that verdict into the exit code and into `cleanup_dirty_banner` — which now
ends with the same DN hint, plus the three readings of seeing a stray array
*there*, where `dn_cleanup` has already tried to stop one: `mdadm` is missing
on that guest, `mdadm --stop` would not take, or the array's name is not one
`md_stop_all` matches.

The order:

1. **Hosts first.** They hold the controllers over this suite's subsystems. A
   subsystem unlinked from its port under a live controller kills that
   controller with DNR and the host never reconnects by itself — acceptable
   during teardown, but only after the host has stopped issuing IO. Each host
   disconnects every `$NQN_IT` and `$NQN_PREFIX` subsystem and the discovery
   controller pointing at the cdc (never `disconnect-all`, which would take
   down subsystems no dnv suite has anything to do with), unmasks, and removes
   `$WORK`. The identity files are **not** removed: a hostnqn is node identity
   rather than run state, and the kernel keeps a strict 1:1 hostnqn↔hostid map
   that a replaced file under a live association violates.
   Two corrections the third run made here. The sweep is **not** "every
   connection this suite could have made and nothing else": `$NQN_PREFIX` is
   `nqn.2024-01.io.dnv` with no terminator, so the prefix test also matches
   `nqn.2024-01.io.dnv-it:cdc:*`, `cdc_test.sh`'s own host-facing subsystems on
   the two host guests the two suites share — at the start of a run that is
   debris worth sweeping, and at any other time it is one more reason for rule
   E2E9. (The discovery half is unaffected, being keyed by traddr.) And the
   disconnects are `|| true` with their output discarded, so the verb now
   re-reads sysfs and reports what survived as `wipe_left=`. That line is read
   by the teardown of §4.6, which dies on it; here in the cleanup `host_cleanup`
   sends `wipe`'s output to `/dev/null` and only its own `cleaned` sentinel
   reaches the driver, which is what the cleanup gate checks for.
2. **Every CN, phase 1.** Kill the agent, resume any suspended dm device, drop
   this suite's host-facing subsystems, then the ns-devs, the transfer finals
   and the **clone finals — while their transfer source connection is still
   up**, because a dm-clone flushes through its source on removal and that
   source is an nvmet export on one of these CN guests.
3. **Every CN, phase 2.** Only now the transfers themselves; then top-down
   through the cn stack, the md arrays, the leg and group wrappers, and last
   the clone-metadata wrappers (which hold the arena's loop device open and
   would wedge the tmpfs teardown with `EBUSY`); the side connections; the
   tmpfs; the nvmet hosts entries; `ports/1` — **its ana_groups 3 and 2 first**;
   the udev rule (these are shared lab machines); `$WORK`.

   Both CN phases must finish on **every** CN before the first DN VM is
   touched, which is why they are two loops and not one loop doing both.
4. **The DN VMs.** Kill every `[d]nv-agent` from the helper, resume anything
   suspended, **then stop every `dnv-` md array — before any dm device of this
   suite is removed**, and only then drop the `:2:` and `:3:` subsystems and
   the dm kinds in dependency order (migration sources after the devices that
   flush through them), sweep **every** nvmet port from 1 to `MAX_DNS_PER_VM` —
   not merely this run's `DNS_PER_VM`, since an aborted earlier run or one with
   a larger `--dns-per-vm` may have left a higher one — zero each loop device's
   first 4 KiB with `conv=fsync`, `wipefs`, `losetup -d`, remove the md mask,
   and remove `$WORK`. A CN's dm stack sits on nvme connections to these sides,
   which is why the CNs go first: dropping the sides first would leave the CN's
   md legs on dead paths.

   **Why an md stop on a disk node, where nothing this suite runs ever
   assembles an array.** Because something else can, and did: an unmasked DN
   guest assembles the leg superblocks the CN wrote through the side export
   (§8 item 15). Such an array sits on *top* of the DN's stack holding one of
   this suite's own dm devices open — the side (kind 4), or the per-CN linear
   (kind 1) that maps the side 1:1 and therefore carries the same superblock at
   the same offset; the lab reading named a dm *minor* and settles nothing
   between them, and a pinned kind-1 linear holds the kind-4 side open in its
   turn, so the side is unremovable either way. A held dm device does not come
   back: `dmsetup remove` fails, and `dm_force_remove`'s fallback — resume,
   then `remove --force --retry`, bounded at 10 + 10 + 15 s — only swaps an
   error table in and leaves the device where it was. So every pinned device
   costs the best part of half a minute and survives anyway — twice over where
   the array sits on the linear, once for the linear and once for the side it
   goes on holding — which is how a verb that removes dozens of them runs past
   its bound. That is what the second run's
   two DN timeouts were, and stopping the arrays by hand is what let the
   identical invocation finish. The stop stays in the order even now that
   `dn_up` installs the mask, because a guest that ran an older copy of this
   suite, or whose rule did not take, has to be cleanable **by the suite**; and
   `md_stop_all` matches only an `mdadm --detail --scan` name of `dnv-…` or
   `<homehost>:dnv-…` — the cn agent's own `--name` under `--homehost any` — so
   a lab guest's own array is never a candidate.

   The mask comes off at the *end*, in the same position `cn_cleanup_phase2`
   removes it: a udev event is raised by a block device, so the rule may only
   go once no block device on the guest still exposes a `dnv-` superblock. The
   dm devices are gone above it and `loop_teardown` has just detached every
   loop device; the superblocks themselves are at inner offsets of the backing
   **files**, which the `rm -rf $WORK` on the next line takes, and not at the
   4 KiB header `loop_teardown` zeroes.
5. **cp.** The four daemons by their recorded pids (CONT → TERM → KILL), then
   the helper's bracketed `pkill` sweep as the fallback for a pid file a crash
   lost — the etcd pattern carries this suite's own `--name`, so a stranger's
   etcd on that guest is never touched — and `rm -rf $WORK`.
6. **`fstrim -a`** on every guest, best-effort and bounded.

Dropping a port **refuses** to touch an nvmet port whose `addr_trsvcid` is
outside this suite's band, printing which suite it seems to belong to. The band
is one pair of numbers — `4300..4349`, `DN_TRSVCID_BASE` to
`DN_TRSVCID_BASE + MAX_DNS_PER_VM − 1` — and the helper preamble ships the same
pair to **both** roles; the check does not look at the role. What differs by
role is only *which* ports are offered to it: a DN sweeps `ports/1` through
`ports/50`, a CN drops `ports/1` and nothing else. So a stranger's `ports/1` on
a CN bound to, say, 4305 would be removed rather than refused. That costs
nothing in this lab — a CN agent binds 4300 — but the band is a shared pair of
variables, and narrowing it for CNs means giving the drop a `<lo> <hi>` of its
own, not rewording a comment. The one exception is a port whose `addr_trsvcid`
is *empty*: that is debris from an agent killed between `mkdir ports/<id>` and
its first attribute write, and it is removed rather than refused, because
refusing would leave it forever and a leftover port is exactly what fails the
next suite.

**On failure nothing is removed.** `on_exit` runs the diagnostics instead, and
every process, dm/md/nvmet object, loop device, host connection and log stays
where it is.

---

## 7. Diagnostics on failure

`on_exit` calls `diagnostics` **instead of** the cleanup, so everything the run
built is still there while it runs and afterwards. Three properties shape it:

* **Order.** cp's dump is first, because cp's `diag` prints
  `$WORK/last.{rc,err,out}` — the failing dnvctl call's three streams — and
  every dnvctl read below it would overwrite those three files.
* **It must not die.** `on_exit` runs it with `|| true`, which suspends `set -e`
  but would not survive a `die` (that calls `exit`), so every guest call is a
  tolerant form and every dnvctl read runs in a subshell.
* **It must be bounded.** Diagnostics runs when something is already wrong,
  which is when a guest command hangs; each dump is `timeout 60`.

What it prints, in order: the failure context (stage, trace id, case, whether
setup finished, **the last ANA state observed and the controller the last probe
resolved** — the state alone is the difference between "it never became
optimized" and "it became inaccessible", and since the third run the pair
carries a third answer the state could not: `none` is what the probe returns
both for a controller that is not there and for one whose namespaces do not
include that uuid. **Read the pair controller first.** `last ctrl: none` is the
no-path answer whatever the line above it says — that is the
`*_ctrl_is_present` half of the split wait, which takes no ANA reading at all
and therefore *clears* `ANA_LAST`, so what prints beside it is `(none read)`.
Leaving the previous reading standing was the first attempt at this and it was
backwards: the two globals are written in exactly two places and reset nowhere,
so a no-controller timeout at any stage after a successful ANA read would have
printed a fresh `last ctrl: none` beside a stale `last ANA: optimized` — the
dump claiming a reading that wait never took, which is the fault the split
exists to remove. `last ANA: none` beside a **controller name** is the other
answer: the path is up and the namespace is not on it. Both globals are shared
by the host and CN probes, so the pair is "the last reading either of them
took", and the stage on the failure line above says which. The third run had
only the state to print: it printed `none`, which was the first of those two,
while the failure line above it described the second. §4.1 stage 10 is the wait
that now
separates them before the timeout —
the shape, the cluster id, the cp daemons and their pids, every recorded loop
device, and the driver's copy of the last dnvctl call's three streams, which
survives a cp that has become unreachable); cp's own diag, 200 lines of each of
the four daemon logs, and the `"level":"ERROR"` records of the three dnv daemons
among them — etcd's log is tailed but not grepped, because the pattern is the
literal token `common/log.go`'s slog handler writes for an error record, quotes
included, and etcd is not an slog logger; the control-plane reads (`cluster get`, `sp get`, `cntlr inspect` of every cntlr —
by the ids in `sp_conf.cntlr_id_list`, since `cntlr_list` entries carry no id
at all — `cn inspect` of every CN and `dn inspect` of the disk nodes the case
registered as involved), attempted only when setup built something and the
gateway is still the process this suite started; each host's controllers, every
namespace's uuid and `ana_state`, the mask, stas, any task in D state — which
is what a wedged read looks like from outside — and 100 lines of its `dmesg`;
each CN's dm tree, dm status,
suspended devices, `/proc/mdstat`, `mdadm --detail --scan`, nvmet tree, loop
devices, tmpfs mounts, space, `dmesg`, `nvme list-subsys`, 200 lines of its
agent log and that log's `"level":"ERROR"` records; and each DN's same set —
including, since the second run, those same two md readings, each labelled
*a DN must show none*, and an `ls` of the md mask file, because the stray
arrays that wedged that run's cleanup had to be found by hand over ssh after
the dump had already said nothing about them — plus a **selected** log dump
— a DN VM holds up to 43 agent logs, so every
log carrying an ERROR record is *named* with its count and only the first few,
plus the instances the case registered, are tailed in full.

**When the failing stage is one that connected, read the host's `dmesg`
first.** The dump has always carried it; what the third run changed is how much
weight it has to bear. nvme-cli prints nothing and exits 0 whether it connected
every path or none (§8 item 16), so the kernel's lines are the only record that
a connect was attempted at all and the only place its fault is named: a
`new ctrl: NQN "<the subsystem>"` line for each path that came up, and for one
that did not, `failed to connect socket: -111` — `ECONNREFUSED`, one per
discovery record, which says the advertised address was not listening.
Neither appears anywhere else in the transcript. Since the third run the suite
gets there first: `connect_verdict` dies at the connect, naming the CN agent
log to read and the nvmet objects to look for, so the `dmesg` is the
confirmation rather than the discovery.

Stage names and trace ids are the index into all of it: the failure line ends
with the exact `jq 'select(.trace_id=="it-<case>-<nn>")'` to run.

---

## 8. Known limits

1. **A multi-case run pays for a full rebuild per case.** E2E11 is satisfied
   the expensive way: between cases the suite tears the whole data plane down
   (`cleanup_all`) and builds it again (`setup_infra` + `setup_case`), because
   an etcd reset alone would leave every DN disk header naming the previous
   cluster, which `EnsureFormatted` refuses as foreign for the rest of the run
   (§3, rule E2E11). A four-case run therefore costs four builds of a 32-slice
   sp on top of the cases themselves, and `--only` is how a single case is
   re-run without paying for the others. What a build costs is measured rather
   than guessed since the first run: the primary CN's agent log spans
   **8 m 45 s** of build work (item 12), against the design's "expect ~5 min
   per build". Read that number for what it is — the build as it actually ran,
   with two failovers restarting it from scratch inside those 8 m 45 s
   (item 13). An uninterrupted 32-slice build has not been timed yet, and this
   figure is the only one there is to size a wait from.
2. **The clone source may fall back.** The default source is a transfer of the
   same sp, which makes one kernel both initiator and target for the same
   bytes. That is legal by construction, and whether it *works* is a property
   of the lab's kernel. §4.4 describes the two triggers and the second pool the
   suite builds instead. A run that takes the fallback proves the same
   assertions against a different digest, and says so in its transcript; the
   branch that ran is logged.
3. **A standby's namespaces are `inaccessible`, never `non-optimized`.** The cn
   agent gives a host-facing namespace the optimized ANA group **iff** its
   cntlr is primary, not disabled, not effectively suspended and not
   provisioning-deferred; everything else goes to the **inaccessible** group.
   The port's group 2 is not unused in the tree — the dn agent puts a side's
   namespace there for every non-primary CN — but no namespace a *host* sees is
   ever in it. So every host-side ANA assertion in this suite is `optimized` or
   `inaccessible`, a host always has exactly one usable path per namespace, and
   ANA path selection across two *usable* controllers is not exercised here and
   cannot be while CN16 reads that way.
4. **The closing `fstrim -a` is a no-op in this lab.** As the suite records
   (checked 2026-09-17), none of the ten libvirt domains carries
   `discard='unmap'` on its vda `<driver>` line, so the guest filesystem has
   nothing to forward a discard to. The call is harmless and stays, because it
   costs nothing and becomes
   real the moment the operator adds that attribute. Until then the laptop's
   free space shrinks by the run's real writes — which §4.6 caps at 8 GiB.
5. **Anti-affinity is headroom, not structure.** See §3, rule E2E4: migration
   destinations and spare legs relax the location exclusion when tier 1 finds
   nobody, so the VM-distinctness assertions are made only when
   `DN_VM_CNT > LEGS`. Running with `--dns-per-vm` well below the §2.4 bound
   makes that relaxation reachable, and the warning says so.
6. **Six RPCs are not exercised** (§1.1), and nothing here removes a node or a
   cluster record.
7. **`--redund none` runs a strictly smaller suite.** The `td get-leg-bm` read
   of `ops` stage 06, the whole spare-leg stage of `copy` and the AR8 stage of
   `react` are skipped with a log line, because a RedundNone group has one leg,
   no md array and no redundancy to repair.
8. **The two md halves of the residue check ask different questions, and the
   DN half is no longer the weaker one.** Both grep `mdadm --detail --scan` for
   a `dnv-` array name — `cn_residue` inside the helper, `dn_md_residue`
   through a one-line read-only ssh. On a CN the question is whether the
   agent's own arrays went with the sp, which is teardown. On a DN the question
   is whether an array exists that *nothing in this suite assembles on purpose*
   — the stray assembly of item 15 — which makes it the assertion that the DN's
   md mask worked, and not a formality. It had two ways to be blind and both
   are now closed: `mdadm` is a required DN tool (§5), so preflight has already
   failed on a DN where the tool is missing, and the probe goes through the
   *dying* `ssh_dn` with the caller dying on a non-zero status, so an ssh or
   sudo that fails at that moment is a failure rather than an empty pass.
   It is still not a general "no md on this guest" check: a lab guest's own
   arrays are outside the name filter on both roles, deliberately. Neither half
   ran in either of the two lab runs so far, because both died before a case
   reached stage 91 — the first run's stray arrays were found by hand.
9. **A blocked probe leaves an unkillable `dd` behind.** When `host_sha_probe`
   answers `blocked` the reader is in D state, where `timeout` cannot reach it;
   the resume that unwedges the device reaps it. That is the price of learning
   the answer at all, and the alternative — a foreground read — hangs the run
   for ever.
10. **The suite occupies the whole lab.** Ten guests, and its cn agents mount
    their tmpfs at `/tmp/dnv-tmpfs`, the path `cnagent_test.sh` owns. Rule E2E9
    is an operator rule; nothing enforces it.
11. **The doc lint reaches the E2E ids in `doc/`, and nowhere else.** The
    family pattern was widened when this suite landed, so `ctl/doclint_test.go`
    now registers the twelve definitions of §3 and checks every citation of
    them in `doc/*.md` and `README.md` (§3 — and this paragraph is inside the
    text it reads). What it does not read is the *suite*: `docFiles` is
    `doc/*.md` plus `README.md`, so the ids `integtest/e2e_test.sh` cites in
    its own comments are checked in neither direction. Renaming a rule here
    leaves those comments pointing at nothing and no test fails.
12. **The build is bound by process-spawn rate, not by memory — and the
    design's first fallback names the wrong resource.** Measured on the first
    run (2026-09-17, commit `deca203`, the default shape on all ten lab
    guests): while the primary CN built the sp, its agent log recorded
    **126,657 process spawns in 8 m 45 s** — 82,099 `dmsetup`, 20,127 `mdadm`,
    16,795 `lsblk` — about 240 a second, sustained, on a 2-vCPU guest. Memory
    was never short. All three cn guests held about **2.8 GiB of 3.4 GiB
    available** during the run and after it, with load averages under 1. So
    `tmp_doc/use_32_slices.md` §9's risk table is wrong where it makes
    `virsh setmem`/`setmaxmem` to 8 GiB the *first* fallback for CN load:
    raising RAM addresses a resource that was not scarce. Its second fallback
    does bear on the measured one: at `--redund none` a group is one leg and
    its device is a dm-linear rather than an md array, so the sides halve and
    the `mdadm` work leaves the build with them. A smaller `--slice-cnt` cuts
    the same way — pools scale with it, groups and sides with twice it.
    Whether more vCPUs on the cn guests would fix it is **untested**: nothing
    in that run varied the cpu count, and this item records the measurement,
    not a cure.
13. **At this shape, on these guests, a 5-second `primary_unhealthy` made the
    build self-defeating — observed once.** On the same run the worker logged,
    all of it during setup and none of it provoked by a fault:
    `failover old_cntlr_id 1 new_cntlr_id 2`, then
    `spare_create slice_id 47 grp_id 53 leg_id 56 spare_leg_id 355`, then
    `failover old_cntlr_id 2 new_cntlr_id 1`. Two failovers and one spare leg.
    The loop is self-defeating in the literal sense: a controller that has
    just been promoted starts the same build from nothing, goes
    unresponsive in its turn, and hands the role back. Three things about it
    are worth keeping straight.
    * **The failovers were not caused by the suite's short value.**
      `common.DefaultPrimaryUnhealthy` is 5 as well, so an sp created with no
      `--thr-primary` at all — or with a zero, which `ResolveEventThreshold`
      turns into the same 5 at every worker pass — would have failed over the
      same way. The *spare* is the suite's own doing: `--thr-leg 30` against
      `common.DefaultLegUnhealthy` 1200, with `--thr-side 20` against
      `DefaultSideUnhealthy` 600 as AR8's other trigger.
    * **What the primary failed at is a health round, not the build.** Five
      seconds without a clean round is all AR5 needs once `Cntlr.err_epoch`
      is stamped. Which of HL2's two paths stamped it — a round that missed
      its timeout or an ERROR row in the reply — was not read out of the run,
      and it does not change what the suite does about it.
    * **This is one observation at one shape.** 32 slices, md-raid1, a 2-vCPU
      4 GiB cn guest sharing a laptop with nine other guests. Nothing here
      establishes where the shape stops being buildable under a 5-second
      threshold, nothing here says the tree's default is wrong, and the
      default is unchanged. What the *suite* does about it is §2.3: three of
      the four cases now build under thresholds a case has no ordinary way to
      reach, and `react`, which needs short ones, takes its assertions as
      deltas from a snapshot of the shape its own build left (§4.5).
14. **`react` cannot assert an absolute count of reactions.** Because its own
    build runs under the aggressive set, a failover or a spare leg that the
    case did not provoke can be there before its first stage. Every count it
    cares about is therefore read from a snapshot — stage 00's for the shape as
    a whole, each step's own `before` reading for what that step is judged on —
    and asserted as a difference (§4.5). That buys the case its own reaction
    and gives up the stronger statement that the sp reacted exactly once. The
    other three cases keep the absolute form, for the reason §2.3 gives, and
    `smoke` turns it into an assertion: a spare leg in *its* sp is a finding.
15. **A disk node holds the controller node's md superblocks, and an unmasked
    DN guest assembled them — observed on two guests, once.** md never *runs*
    on a disk node: CN12 is primary-only and the dn agent runs no `mdadm` at
    all. Its **metadata** gets there all the same. A leg is a whole-device
    dm-linear on the CN over the nvme namespace a DN exports, and
    `mdadm --create` writes a 1.2 superblock and an internal bitmap at the head
    of each member, so those bytes travel the nvme-tcp path to the DN. What
    they land on is not a copy: the per-CN linear a DN exports to the **primary**
    is a dm-linear whose table target is the side device itself
    (`linearBacking` in `agent/dnagent/plan.go` — a standby gets the dm-error
    instead, which is why only the primary's writes get through), so the
    superblock comes to rest on the side's own sectors, on that guest's own
    loop-backed extents. A DN guest carrying no md udev mask therefore has a
    `linux_raid_member` signature on a block device of its own, and the stock
    `64-md-raid-assembly.rules` skips a device whose `SYSTEMD_READY` is 0 and
    otherwise hands a `linux_raid_member` to `mdadm --incremental` — `dm-*`
    devices included, which that rule says in so many words. (That reading is
    of the rule as the distribution ships it; what preflight checks on each
    guest is narrower — that the `SYSTEMD_READY` line is in it, since that one
    line is the whole of what `63-dnv-md.rules` relies on.)
    What the incremental path then assembled was **degraded and read-only**,
    and it would be: a group's `LEGS` sides are placed on `LEGS` different DN
    VMs and the suite asserts it after every create and grow (E2E4; item 5 is
    the qualification), so one member of a two-member raid1 is all any one DN
    can see. The two assembly paths differ exactly here — the explicit-devlist
    form gates a degraded start on the survivor's own Array State and the
    incremental form does not. The array holds the DN's device open, which is
    what turns a lab annoyance into a stuck cleanup: the removal of that device
    cannot proceed.

    **Which device, exactly, is not established, and the suite no longer says
    it is.** The evidence below names a dm *minor* (`dm-76`) and not a dm kind.
    Two of this suite's devices carry that superblock at the same offset: the
    side (kind 4), and the per-CN dm-linear (kind 1) that the nvmet namespace
    actually exports (`agent/dnagent/syncup_side.go:597`), which `ensureDmLinear`
    builds over the side at offset 0 for its whole length (`:529-566`). udev
    probes both and `mdadm -I` takes whichever raised its event first.
    Operationally it does not change the remedy — `dn_cleanup` removes kind 1
    before kind 4, and a kind-1 linear that will not go holds the kind-4 side
    open in its turn, so the side survives either way — but an operator sent to
    look for "an array over a side device" would not recognise one over a
    `dnv-…-1-…` linear, which is why the messages name both.

    The evidence, from the lab on 2026-09-17 between the first run (`deca203`)
    and the second (`078de79`):

    * one of dn2's 35 arrays, in `/proc/mdstat`, created while the first run
      was building:

      ```
      md93 : active (auto-read-only) raid1 dm-76[1]
            62464 blocks super 1.2 [2/1] [_U]
            bitmap: 0/1 pages [0KB], 131072KB chunk
      ```

      The internal bitmap is the cn agent's own `--bitmap internal`
      (`agent/cnagent/md.go`), and `[2/1] [_U]` is that one visible member.
    * the four DN guests — named as that run's argv ordered them — separate
      exactly by whether they carried **any** md udev rule:

      | guest | udev rules | md arrays | its `dn_cleanup` |
      |---|---|---|---|
      | dn0 `192.168.122.48` | `58-dnv-test.rules` | 0 | finished |
      | dn1 `192.168.122.70` | `58-dnv-test.rules` | 0 | finished |
      | dn2 `192.168.122.49` | none | 35 | timed out at 300 s |
      | dn3 `192.168.122.122` | none | 28 | timed out at 300 s |

      `58-dnv-test.rules` is a different rule, not this mask: it was left
      behind by the dnagent lab work, nothing in this tree writes it, and no
      suite here maintains it. The two guests that came through clean are
      exactly the two that happened to be carrying it, so their clean cleanup
      says nothing about this suite, and that it kept the arrays away was
      incidental rather than something to rely on.
    * stopping every stray array by hand (`mdadm --stop` on each) and running
      the **identical** `--cleanup-only` invocation again finished on all ten
      guests with no warning at all, leaving no nvmet port, no dm device, no
      loop device and no md array on any DN. That is what separates "held
      open" from "slow".

    **`tmp_doc/use_32_slices.md` §7.4 step 3 is wrong.** It says: "Install the
    `63-dnv-md.rules` mask only on CN VMs (that is where md runs)". Its premise
    is true and its conclusion does not follow, because assembly is driven by
    what is *on* a device and not by which machine wrote it. The CN is where md
    runs; the DN is where md's metadata lands. The suite's own header rule 7
    restated the same instruction in its own words — "The `63-dnv-md.rules`
    mask goes on the CN VMs, which is where md runs." (`078de79`,
    `integtest/e2e_test.sh:63`) — and both now say the mask goes on both node
    roles (§5, §6).

    Read this as an observation and not a law. What it establishes is that
    these two unmasked guests assembled these arrays, at this shape, on this
    lab's kernel and udev. It does not establish that the mask is the only
    thing that can suppress the assembly — the two clean guests were carrying a
    different rule file, whose text nobody read — nor what a guest whose udev
    does not probe dm devices at all would have done.
16. **`nvme connect-all` exited 0 having connected nothing, and the cdc
    advertises a subsystem before any CN has built it — the third run's setup
    failure, observed once.** Two facts, both established on the guests at
    commit `7721516`. The second is why the first was reached at all, and
    neither is a slow step that a larger budget would have absorbed.

    **(a) An exit status is not evidence of a path.** Stage 10's helper printed
    `rc=0` and nothing else — `connect_all` folds nvme-cli's stderr into its
    stdout, so "nothing else" covers both streams — while host0 ended the stage
    holding no controller at all: the diagnostics' `nvme list-subsys`,
    "controllers" and "ana states" sections are empty, and its dmesg carries no
    `new ctrl: NQN "nqn.2024-01.io.dnv-it:e2e:ss0"` line anywhere. What that
    dmesg does carry, between the `new ctrl` and `Removing ctrl` pair of
    connect-all's own discovery connection, is
    `nvme nvme1: failed to connect socket: -111` **twice** — `ECONNREFUSED`,
    one per discovery record. So both connects were attempted, both failed in
    the kernel's TCP connect, and nvme-cli reported neither and returned 0.
    A second shape of the same zero was reproduced by hand against this same
    cluster: `connect-all` presenting a hostnqn that is not in the subsystem's
    `allowed_hosts` also exits 0 with nothing connected, and the cdc's own log
    says why — `nqn.2014-08.org.nvmexpress:uuid:00000000-0000-0000-0000-0000deadbeef`
    connected at 20:11:37.554 and disconnected 8 ms later — which is DS4 doing
    its job: a non-empty `allowed_hosts` hides the entry from every other host,
    the log page comes back empty, and there is nothing to connect. It is not a
    privilege story either: run as a non-root user the same command fails
    loudly with `Failed to open /dev/nvme-fabrics: Permission denied` and rc 1,
    and `ssh_host` prepends sudo. Read this as two observations rather than a
    law about nvme-cli: what is established is that on this lab's nvme-cli an
    empty discovery log and a socket that refuses both leave rc 0 — not that
    every failure does. The suite's older rule — report the status, never
    assert it — is unchanged, and the helper still only reports it. What the
    observation added is the other half of the verb: the helper now prints
    every controller the host holds for that subsystem and a machine-readable
    `ctrl_cnt=`, and `connect_verdict` dies on zero, so what a connect is
    judged by is the host's own state (§4.1 stage 10).

    **(b) The discovery log is a control-plane reading, so it runs ahead of the
    data plane.** `CreateSubsystem` writes the `CdcEntry` in the same STM as
    the Subsystem and the SpConf (`gateway/subsystem.go:189-195`), and the
    transports it advertises are `enabledCntlrTrConfs(cntlrs)` — every
    non-disabled cntlr's own `nvme_tr_conf`, taken from the cntlr **records**
    (`gateway/common.go:784-793`). Not one field of it is a report from an
    agent. dnv-cdc serves its log from an etcd watch (`cdc.md` §3), so the
    advertisement is live within milliseconds of the commit, while the CN
    learns of the subsystem on its next syncup and builds it bottom-up. The
    third run's own timestamps, from the cp daemons' logs and the primary CN's
    agent log:

    | when (UTC) | what |
    |---|---|
    | 20:09:11.3204 / .3259 | the gateway's `CreateSubsystem` request and reply |
    | 20:09:11.3265 / .3267 | the cdc's watch event for the new entry — both CN transports, no `allowed_hosts` yet — and the entry applied |
    | 20:09:11.3643 | the `SyncupCntlr` carrying that subsystem reaches the primary CN; its reply comes at 20:09:18.1439, 6.8 s later |
    | 20:09:11.6357 | `UpdateSubsystemHosts`'s rewrite of the entry reaches the cdc |
    | 20:09:12.0205 | `CreateNamespace` |
    | 20:09:13.5614 | host0's `nvme discover`, the suite's own, logged by the cdc as a host connect |
    | 20:09:13.9418 | `connect-all`'s own discovery connection — and 5 ms and 10 ms after it in host0's dmesg, the two `-111`s |
    | 20:09:15.6892 / .6977 | the **standby** cn0 creates the nvmet subsystem and links it into `ports/1/subsystems` |
    | 20:09:17.6375 | cn0 writes its namespace's `ana_grpid` 3 and enables it — the standby's inaccessible copy |
    | 20:09:18.1247 / .1313 | the **primary** cn2 creates the subsystem and links it |
    | 20:09:23.2982 / .3069 | cn2 links both hosts into the subsystem's `allowed_hosts` |
    | 20:09:23.3219 / .3223 | cn2 creates namespace 1 and writes its `enable` |

    The last five rows are the two agents' own `mkdir`, `ln` and configfs-write
    lines, read out of their logs. So the connects went out **1.75 s before
    either CN had a
    listener at all**, 4.2 s before the primary had one, and 9.4 s before the
    primary held the namespace whose ANA state stage 10 waits on. An nvmet port
    is not a listening socket until a subsystem is linked to it — the same fact
    preflight's nvmet-port check is built on (§5) — which is exactly what
    `-111` says. It was **not** a permissions race: in that first pass the
    desired host set was still empty, so the agent read `attr_allow_any_host`
    as `0` and wrote `1` (20:09:18.1265), and a subsystem that admits everybody
    would have admitted host0 too. Everything had caught up long before the
    stage's 60 s ANA wait expired: cn2's diagnostics dump shows `ss0` under
    `subsystems` **and** under `ports/1/subsystems`, both hosts' nqns in its
    `allowed_hosts` and namespace 1 present, and the guest still answers
    `LISTEN … 192.168.122.77:4300`. Nothing was wrong with the transports, the
    NQNs, the hostid or the cdc, and the end state is what says so: the address
    the log advertised is the one cn2 listens on, the NQN in the log is the one
    it created in configfs, and host0's own nqn is one of the two under that
    subsystem's `allowed_hosts`.

    **Connecting immediately after a control-plane write is a race the suite
    must wait out, and calling it a timing fluke would be wrong twice over.**
    It is not rare: there is no ordering in the tree that makes the etcd commit
    and the CN's convergence simultaneous, the gateway returns as soon as the
    STM commits, and a cntlr builds bottom-up with the subsystem, the
    namespaces and their ns-devs as the last rows to appear (§4.5) — so the
    window is about a whole syncup pass wide, 6.8 s here, on this sp's widest
    shape, on a 2-vCPU guest. And it is not a wait that was
    too short: the connect does not retry, so the step that timed out was not
    the connect but the ANA read behind it, watching for a path that had
    already been refused and would never be made again. Only a wait on the
    CN's own answer closes it, which is what §4.1 stage 10 now does. The
    pattern was already in this file at the other end of the run: `react`'s
    stage 03 waits for CN19's whole `READWRITE` shape — subsystem and namespace
    rows included — before host0 connects to the new primary, and says in so
    many words that a connect issued on the strength of the raid0 alone can be
    refused by a target that has not created the subsystem yet (§4.5). Setup,
    which connects first and which every case runs, did not.

---

## 9. Changelog

* **2026-09-17 — created.** `integtest/e2e_test.sh` and this document are
  commit 4 of the `use_32_slices` design (`tmp_doc/use_32_slices.md`), after
  commit 1 (`MaxSliceCntPerSp` 32, `EtcdMaxTxnOps` 1024,
  `DefaultSliceCntPerSp`, the `CreateStoragePool` tripwire), commit 2
  (`dnv-agent --nvmet-port-id` on both roles) and commit 3
  (`dnvctl cluster create --extent-size`). The suite is the first in this repo
  to build a storage pool at `MaxSliceCntPerSp`, and the only one that drives
  the shipped `dnvctl` against a real gateway, a real worker, a real cdc, real
  agents and real kernel NVMe hosts at the same time.

  This document was written from the file rather than from the plan. The
  places where the suite deliberately departs from `use_32_slices.md` §7 are
  recorded in §3 (rules E2E4, E2E10, E2E11, E2E12), §4.1 (the `NOT_FOUND`
  readiness answer, the `ABORTED` retry), §4.4 (the three deviations and the
  unnecessary `+2` to `DNS_PER_VM`), §4.5 (AR6's computed chunk count, AR6's
  unasserted placement claim, the host disconnect before the AR5 kill, AR8's
  second plane), §4.6 (preflight's floors in place of a flat 10 GiB), §5
  (preflight after the start cleanup) and §6.

  Companion edits, all of them enumerations that now have one more member:
  `doc/layout.md` §2 gained `doc/e2e_integtest.md` and `integtest/e2e_test.sh`,
  its `integtest/` note gained this suite's root requirement and this
  document's name, §6 gained the closing build-out step and §8 records the
  amendment; `dnv-worker.md` §14.4, `gateway.md` §10.4 and `cdc.md` §9.4 name
  `e2e_test.sh` beside the suites that already read `EtcdMaxTxnOps` from
  `workerctl constants`; `dnvctl.md` §7.3 and `gateway.md` §10.3 add this
  suite's block to the port inventories they check themselves against. No
  package boundary, path or dependency changed: the suite adds no Go file and
  no driver of its own — it builds `workerctl`, which it runs for `constants`,
  and `cnagentctl`, whose `host-id` wrapper no step calls today (every connect
  here presents the host's own `/etc/nvme/hostnqn` and `hostid`); both binaries
  already exist.

  Two sentences elsewhere were deliberately **not** touched, because they
  record what existed when their own suite was specified rather than a live
  count: `cdc.md` §9.2's "next to the five existing suites" and
  `dnv-worker.md` §14.2's "next to the two agent suites". `README.md`'s
  `integtest/` bullet still enumerates six suites and is outside this
  document's remit.

* **2026-09-17 — the first lab run, and the four changes it forced.** The suite
  ran for the first time on all ten guests, at commit `deca203`. It got a long
  way: preflight passed everywhere, 172 dn agents (43 per VM, each with its own
  `--nvmet-port-id`) and 3 cn agents came up, the cluster and all 175 node
  records were created with `--extent-size 67108864`, and
  `sp create --slice-cnt 32 --redund raid1` **committed** — the create's
  transaction against a real etcd at `--max-txn-ops 1024`, which is the ceiling
  `EtcdMaxTxnOps` was raised for. All 128 sides provisioned. It then failed in
  setup stage 07, waiting for the primary's stack, at the 300 s bound.

  Three causes, each read out of the run rather than inferred, and the four
  changes they forced:

  1. **The thresholds trip during the build, and the build is what trips
     them** (§8 items 12 and 13). Two changes come out of this one. Thresholds
     are now chosen per case at `sp create` (§2.3): `smoke`, `ops` and `copy`
     build under a set no case has an ordinary way to reach, and `react` keeps
     the short set, because its own reactions are the point of it. And `react`,
     which therefore builds under thresholds that can fire, asserts deltas
     against a snapshot of the shape its own build left rather than absolute
     counts (§4.5) — where `smoke` does the opposite and makes a spare leg in
     *its* sp an assertion failure (§4.2).
  2. **The shape target went stale, so the wait could never have passed.** It
     compared the agent's live row count against a `want` captured before the
     wait began; when AR8 created a spare leg the agent correctly reported 129
     leg rows — CN10 walks `spare_leg_list` as well as `leg_list` — against a
     `want` frozen at 128. The sp really did hold 128 legs and exactly one
     spare, and the agent held exactly those 129 ids, so no amount of time
     could have made that poll pass. The target is now re-read on every poll
     and a shape that moves under a wait is logged loudly (§4.1 stage 07).
  3. **The build waits were far under the measured time.** 300 s against a
     build window whose agent log spans 525 s; at the bound the primary was at
     21 of 32 pools and 49 of 64 groups and still climbing. `WAIT_BUILD` is new and
     `WAIT_PROVISION`, `WAIT_DELETE` and `WAIT_HYDRATE` are no longer one
     repeated 300 s; all four are sized from that measurement instead of from
     the plan's "~5 min per build" (§2.3).

  §8 also gained what the run says about the *lab* rather than about the suite:
  the constraint is process-spawn rate on a 2-vCPU guest and not memory, so
  `tmp_doc/use_32_slices.md` §9's risk table names the wrong first fallback for
  CN load (item 12); and, observed once at this shape, a 5-second
  `primary_unhealthy` — the tree's own default — makes the build self-defeating
  (item 13).

  **Then two adversarial re-reads of those four changes, and what they caught.**
  Three defects of substance, all of them the correction having been applied at
  one site and not at its twins:

  * **The unsatisfiable target survived in two more waits.** `primary_stack_ready`
    removed it from setup's primary wait and left it in setup's *standby* wait
    (pinned to an id that a failover makes the primary, which then builds the
    very groups and pools the predicate insists are absent) and in `react`
    stage 03's rebuild, where the change had only raised the budget from 300 s
    to 1200 s — quadrupling the cost of the same hang. Both are fixed, and
    differently on purpose: setup follows the role and now loops until the
    primary it waited on is still the primary (§4.1 stage 07), while `react`
    stops the run the moment the role leaves the controller AR5 elected, since
    its stage 04 is written about that controller (§4.5).
  * **525 s was being called the cost of one build.** The suite said "for one
    build of the default shape" while §8 item 1 of this document said the
    opposite three pages later — that the window contains two failovers and that
    no uninterrupted build has been timed. The document was right; every carrier
    of the one-build reading now says *window*, and the margins (3.4 ×, 2.3 ×)
    are stated against it (§2.3).
  * **Two comments described machinery that does not exist.** The four threshold
    variables were said to exist because `react` derives its wait bounds from
    them — nothing anywhere computes a bound from them (§2.3) — and
    `primary_stack_ready`'s three shape guards were said to cover a transient
    the election passes through, which one `Snapshot` and one STM make
    unreachable (§4.1 stage 07). Both now say what is true; the guards stay.

  Also corrected: `WAIT_BUILD`'s definition did not cover setup's 128-side wait
  while `WAIT_PROVISION`'s claimed it (§2.3), and `react_snapshot`'s header
  claimed every later assertion is written against its five globals when one of
  them is read by one assertion and the other four are logged (§4.5).

* **2026-09-17 — the case loop.** The first draft of this document recorded an
  open defect here: `setup_between_cases` stopped after `setup_infra`, so a
  multi-case run met the second case with an empty etcd and died in its first
  `sp get`. The suite now calls `setup_case` there as well; §3 (E2E11) and §8
  item 1 state the rebuild as it stands, and nothing in this document depends
  on the old behaviour.

* **2026-09-17 — the second lab run: it never reached setup, and the md mask
  now goes on both node roles.** The run, at commit `078de79`, stopped in
  preflight on dn2:

  ```
  dn2: /sys/kernel/config/nvmet/ports/1 already exists (addr_trsvcid=4300)
  and this run needs that id. … The start cleanup refused to remove it,
  which is deliberate
  ```

  The refusal to start was right. The explanation was wrong, and behind the
  wrong explanation was a lab trap nobody had written down.

  **The explanation could not have been true.** 4300 is inside this suite's own
  `TRSVCID_MIN..TRSVCID_MAX` band, which is exactly the case `port_drop`
  *removes*; a refusal is what it does to a port *outside* the band. So the
  message named the one cause the evidence ruled out.

  **What had happened instead**, established on the guests rather than
  inferred:

  1. the first run left 43 nvmet ports and 43 loop devices on each DN VM. The
     start cleanup cleared dn0 and dn1 and ran past the 300 s bound then in
     force on dn2 and dn3 — and `ports_sweep` sits near the *end* of
     `dn_cleanup`, so those two guests kept every port. The run went on, and
     preflight then died about one of them.
  2. the two guests whose cleanup ran past the bound are the two that were
     carrying stray md arrays over their own dm devices — 35 on dn2, 28 on dn3,
     degraded and auto-read-only, created during the first run — which held the
     side devices open against the dm removals.
  3. the arrays are there because a disk node's own storage holds the md
     superblocks the controller node wrote through the side export, and neither
     of those two guests carried an md udev mask. §8 item 15 is the mechanism,
     the four-guest correlation, and the check that separates "held open" from
     "slow".

  **What changed, in the suite:** the `63-dnv-md.rules` mask now goes on DN
  guests as well as CN guests, installed by `dn_up` exactly as `cn_up` installs
  it — the helper moved into the body both node roles share and writes only
  when the content differs, since `dn_up` runs 43 times per DN VM against a
  directory udev is watching (§2.5); `dn_cleanup` stops every `dnv-` array
  *before* it removes any dm device of this suite's, and drops the mask after
  the loop teardown (§6); preflight checks `/proc/mdstat` and the stock rule's
  `SYSTEMD_READY` handling on **both** node roles, and `mdadm` and `udevadm`
  are required DN tools (§5); the two nvmet-port dies compute their cause from
  the service id off `port_drop`'s own branches instead of asserting a refusal
  (§5); and the start sweep gained a verdict, `cleanup_start_gate`, so a verb
  that did not finish stops the run at the end of that sweep — the rest of the
  guests are swept first, deliberately — rather than three steps later in
  preflight (§3 rule E2E6, §6), with a remedy branched off the label and status
  of each row it lists; with `cleanup_report` now naming the exit status
  behind a missing sentinel, `CLEANUP_TIMEOUT` doubled to 600 s as a wedge
  detector (which also doubles the worst-case wait before that verdict, §6),
  and a DN diagnostic dump that prints `/proc/mdstat`,
  `mdadm --detail --scan` and the rule file, none of which it printed when
  those 63 arrays had to be found by hand.

  **`tmp_doc/use_32_slices.md` §7.4 step 3 is wrong**: "Install the
  `63-dnv-md.rules` mask only on CN VMs (that is where md runs)". So was the
  suite's header rule 7, which carried the same instruction in its own words —
  "The `63-dnv-md.rules` mask goes on the CN VMs, which is where md runs."
  (`078de79`, `integtest/e2e_test.sh:63`). The premise is true and the
  conclusion does not follow — md never runs on a DN, and its metadata lands
  there anyway. §8 item 15 states it as the observation it is. The design
  document keeps the wrong sentence, with a superseded marker pointing here,
  since it is the record of what was planned.

  In this document, §8 item 8 was the carrier of the same mistake: it read the
  DN-side md probe as the weak echo of the CN's, on the ground that "only a CN
  assembles arrays". That is the belief the run disproved, and the item now
  says what each half asks.

  **What the run did not establish.** `cn_cleanup_phase2` also ran past the
  bound on cn0 and cn2 in that same sweep, and nothing above explains it: the
  md chain is a DN story, and no mechanism was found by which a stray array on
  a DN could wedge a CN verb that runs before the DNs are touched. The repeat
  invocation finished on all ten guests, CNs included, but it does not settle
  the CN question either, because the first sweep had already done part of that
  work. For the DN guests the repeat *does* mean something — the intervention
  there was targeted (stop the arrays, change nothing else) and a held dm
  device does not come back with more time — which is why the arrays are
  recorded as the cause on the DNs and the CN timeouts are recorded as open.
  What the suite does about an open question is give it room and a verdict
  rather than a theory: 600 s is wide enough for a CN tearing down a whole
  32-slice stack, and any verb that still exceeds it now stops the run where it
  happened instead of surfacing later as something else (§6).

* **2026-09-17 — the third lab run: the build came up clean, and the host
  connected to nothing.** At commit `7721516` the two earlier rounds of fixes
  did what they were for. The md mask held on all four DN guests, preflight
  passed everywhere, and the build was clean in the strong sense — no failover,
  no spare leg:

  ```
  primary cntlr 1 on cn2 (192.168.122.77:29950), standby 2 on cn0 (192.168.122.125:29950)
  cntlr 1: pools 32/32, groups 64/64, legs 128/128
  cntlr 2: legs 128/128
  ```

  Setup steps 01 to 09 all passed: 172 dn agents, the cluster, 175 node
  records, the 32-slice raid1 sp, all 128 sides provisioned, the primary's
  whole stack, `t0`, `ss0`, `set-hosts` and ns 1. It then failed in setup
  step 10, 60 s after a `connect-all` that had reported `rc=0`:

  ```
  FAILED … timed out after 60s waiting for:
    host0 ANA 'optimized' for ns 2b6f0cc9-… via 192.168.122.77
  ```

  **The message pointed at the wrong part of the tree.** host0 held no
  controller at all — no ANA state was ever read, and the failure context's
  `last ANA` line said `none` — so the fault was not in ANA, and not in the
  cdc either: the discovery log printed just above the failure was correct and
  had been served correctly. §8 item 16 is the evidence and the timeline;
  in one sentence each, the two facts behind it are that **`nvme connect-all`
  exits 0 having connected nothing** — in this run both of its connects took
  `ECONNREFUSED` and it said nothing about either — and that **the cdc's
  discovery log is a control-plane reading**, written by the gateway from the
  cntlr records and served out of etcd, so it advertises a subsystem and both
  its transports before any CN agent has created the nvmet subsystem, linked it
  to its port or made the namespace. Step 10 discovered and connected 2.6 s
  after `ss create` committed, which was 1.75 s before the first of the two CNs
  had a listener at all. **That is a race to be waited out, not a timing
  fluke:** nothing in the tree makes the etcd commit and the CN's convergence
  simultaneous, and a longer wait would not have helped — both CNs were
  listening within nine seconds of that connect, and host0 still held nothing
  when the stage gave up a minute later, because nothing retried it.

  **What changed, in the suite:** every connect now has a gate in front of it
  and a verdict behind it, and the ANA wait no longer speaks for both.

  1. **The export gate.** `wait_ns_exported` / `wait_ns_exported_all` /
     `wait_xfer_exported` hold out for the *agent's* rows out of
     `cntlr inspect` — `ss_id_to_subsystem` plus `ns_id_to_namespace`, or a
     transfer's two rows keyed by its xfer id — `RES_STATUS_OK`, for every
     non-disabled cntlr where the connect is a `connect-all`. The budget is
     `WAIT_PROVISION` where the convergence really is incremental, which is
     everywhere a from-nothing wait has already run against the same cntlr or
     nothing was torn down; `ops` stage 05 passes `WAIT_BUILD` instead, because
     there the gate is the only wait covering the **standby** and the standby
     is rebuilding from nothing after `DISABLE`. `RES_STATUS_OK` on the
     subsystem row is what covers the link into the nvmet port, and that link
     is what makes the port listen (§4.1 stage 10). All seven connect sites
     have a gate in front of them — six through these helpers, and `react`
     stage 03 through the `cntlr_level_ready` wait that already asked the same
     question over every row of the cntlr — and so do the two namespaces that
     connect nothing but are read by a host: `react` stage 01's second
     namespace, and `copy` stage 02's, which is backed by the live dm-clone.
     That second one was missed by the first sweep, which was made by grepping
     `connect` — a method that structurally cannot find a site where nothing
     connects. The four `ns create` calls in the suite are the whole class and
     all four are now gated.
  2. **The verdict.** `connect_all` and `connect` now print the controllers the
     host holds for the subsystem and a `ctrl_cnt=`, and `connect_verdict` —
     reached through the driver's two new wrappers, `host_connect_all` and
     `host_connect`, the only places the driver touches nvme-cli's connect
     verbs — dies on zero with a message that says an exit status of 0 proves
     nothing, that this is neither an ANA nor a cdc fault, and where to look.
     The message is branched on **which verb ran**, because the silent zero was
     measured for `connect-all` and not for the plain `nvme connect`, and
     because the `ECONNREFUSED` shape belongs to a port with no subsystem
     linked to it — an agent has one nvmet port shared by every subsystem it
     exports, and a CN runs one cn agent, so at a `host_connect` site it may
     already be listening.
     Where the count cannot answer the question — the two sites that
     connect-all onto a subsystem the host already holds a path to —
     `connect_added_ctrl` reads the controller for the **new traddr**
     immediately afterwards. It does not poll: both verbs are synchronous, so a
     poll would hide the race the gate removes.
  3. **The ANA wait is two waits.** `ana_of`'s `none` means both "no
     controller" and "no such namespace on it", and the run died of the first
     while the message described the second. `host_wait_ana` and `cn_wait_ana`
     now wait for a controller first and the state second, sharing the caller's
     one budget, and keep the controller they resolved in `ANA_CTRL`, which the
     failure context prints as `last ctrl:` beside `last ANA:` (§4.1 stage 10,
     §7). The no-controller half **clears** `ANA_LAST` rather than leaving it:
     the two globals are reset nowhere else, so leaving the previous stage's
     reading standing would have made the dump print a fresh `last ctrl: none`
     beside a stale `last ANA: optimized` — a reading that wait never took,
     which is the exact fault the split was added to remove.
  4. **The mask is read back.** Unrelated to the failure, found beside it: the
     host helper's `mask` verb printed `masked` unconditionally while its two
     `systemctl mask` calls carried `|| true`, so preflight's `assert_eq`
     proved nothing about the state rule E2E10 rests on. It now re-reads both
     units with `systemctl is-enabled` and prints what it found, and the host
     `diag` dump prints both units too rather than only the service (§2.5, §3
     rule E2E10, §4.1 stage 01, §5, §7).
  5. **So is the host wipe.** The same shape once more: every `nvme disconnect`
     inside `wipe` is `|| true` with its output discarded, so the verb exited 0
     whether the host let go or not — and the teardown unlinks the subsystem
     immediately afterwards, which kills a surviving controller with DNR. The
     verb now re-reads sysfs and prints `wipe_left=`, and the teardown dies on
     it, **naming the NQNs that survived** rather than asserting they are
     `ss0`: for the reason in the same breath below, a `wipe_left=` line can
     carry a `cdc` NQN, and the DNR sentence about `ss delete` is said only
     when `ss0` is among the survivors (§4.6). Its header lost a false sentence
     at the same time: the sweep is not "every connection this suite could have
     made and nothing else", because the `$NQN_PREFIX` test has no terminator
     and reaches `cdc_test.sh`'s subsystems on the shared host guests (§6).
  6. **The three `disconnect_prefix` sites say what they can detect.** The same
     shape as the `wipe` one and left over from it: the verb discards every
     `nvme disconnect`'s status and ends `return 0`, so `|| die "disconnecting
     X failed"` could only ever fire on ssh or the dispatch. The messages now
     say that, and each site's real check remains the `host_path_gone` poll
     that already followed it (§4.4 stages 02-03, §4.5 stage 03).

  In this document, §4.1 stage 10 and the four case tables state the gate and
  the verdict where they run; §7 keeps the host `dmesg` dump, which the third
  run turned into the first thing an operator reads when a connecting stage
  fails, since nvme-cli's silence leaves the kernel's line as the only record;
  §8 item 16 holds the observation and its evidence.
