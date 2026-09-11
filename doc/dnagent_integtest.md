# dnagent_integtest.md — integration test plan for `dnv-agent dn`

Status: **normative** for the integration test harness under `integtest/`.
Required background: `dnagent.md` (the contract under test), `schema.proto`
(service `DiskNodeAgent`), `architecture.md` §3.1/§4/§11.2/§11.8/§13. This
document is deliberately implementation-ready: a later session (or another
engineer) implements `integtest/dnagent_test.sh` and `integtest/dnagentctl/`
from it without re-deriving any decision.

---

## 1. Goal and scope

Prove that a real `dnv-agent dn` — real device-mapper, the [D13] on-disk
metadata, nvmet configfs, nvme-tcp — behaves per `dnagent.md` on two machines, driven over gRPC from the developer
machine. All 8 `DiskNodeAgent` RPCs are exercised (§18). Happy-path
correctness only; error-path testing is out of scope (§19) except one
stale-revision probe that is structurally required by case D.

Test cases:

| case | name | what it proves |
|---|---|---|
| S | `smoke` | end-to-end plumbing: one side, one CN, IO round-trip |
| A | `sides` | multiple sides exported to multiple CNs; ANA + allowed-hosts |
| B | `migr_full` | 2 concurrent opposite-direction migrations, full copy |
| C | `migr_bitmap` | same, with `PushMigrBitmap`; bitmap + meta_blocks honored |
| D | `restart` | state persistence, reconcile, idempotent re-apply |

Cases are independent: each creates its own objects (own `sp_id`) and tears
them down. They run in the order above, fail-fast.

## 2. Deliverables and usage contract

Two artifacts, both under `integtest/`:

- `integtest/dnagent_test.sh` — bash, `set -euo pipefail`, the orchestrator.
- `integtest/dnagentctl/main.go` — a small Go gRPC driver (§8), built by the
  script. It imports the repo's `pb`, `agent` (for `ParseCloneStatus`) and
  `common` (for name/identity helpers) packages.

Usage:

```
bash integtest/dnagent_test.sh [--only <case>] [--cleanup-only] \
    user1@192.168.10.12 user2@192.168.10.13
```

- Two positional args: ssh targets for VM1 and VM2. Passwordless ssh and
  passwordless sudo are assumed (checked in preflight, §4).
- `--only <case>`: run a single case (`smoke|sides|migr_full|migr_bitmap|restart`).
  Setup and start-cleanup still run.
- `--cleanup-only`: scrub both VMs and exit.
- Fully automated: no prompts, exit 0 on success, non-zero on first failure.
- Cleanup policy: best-effort cleanup at **start, always** (a crashed prior
  run must not break this one); cleanup at **end only on success** — on
  failure all debris (agent state, dm/nvmet objects, logs) is left for
  inspection and diagnostics are dumped (§17).

## 3. Topology

```
 driver (dev box, amd64 linux)
   | ssh + scp (orchestration)          gRPC :29528 (plaintext, no reflection)
   v                                    v
 VM1 = DN1 (dn_id 0x1) ---------- VM2 = DN2 (dn_id 0x2)
   nvmet tcp :4200                  nvmet tcp :4200
   loop dev on 2G file = --disk     loop dev on 2G file = --disk
```

- One IP per VM — the one in the ssh arg — is used for everything: ssh, the
  gRPC target, and nvmet `--tr-addr`. gRPC listens on `<ip>:29528`
  (`architecture.md` §13 convention); nvmet `--tr-svc-id 4200`.
- The `cn` role is not implemented, so **CN identities are emulated** with
  plain `nvme connect --hostnqn <CnHostNqn> --hostid <NvmeHostId>` from the
  VMs themselves,
  cross-connected: CN identities hosted on VM2 connect to DN1's exports and
  vice versa. The driver machine never loads nvme modules and never touches
  the data path.
- For migration cases, the leg's primary CN connects to **both** DNs. The
  side→CN subsystem NQN is keyed by leg, not DN
  (`nqn.2024-01.io.dnv:2:{cluster}:{sp}:{leg}:{cn}`), and the namespace
  identity (uuid/nguid/serial) is deterministic per leg, so the CN kernel
  merges the two connections into one multipath namespace with two paths.
  This requires the two sides of a migrating leg to use **different
  `cntlid_slot`s** (`architecture.md` §11.8) — the plan uses slot 0 for src,
  slot 1 for dst.
- **Every `nvme connect` passes `--hostid`**, from `dnagentctl host-id`
  (i.e. `common.NvmeHostId(hostnqn)`, what the agents themselves use). The
  kernel keeps one hostnqn per hostid, and each VM here plays two CN
  identities on top of its own dn agent's `DnHostNqn`; leaving the node-wide
  `/etc/nvme/hostid` implicit makes the second identity's connect fail
  `EINVAL` ("found same hostid … but different hostnqn").
- Agents run as root (dm/configfs/nvme all need it):
  `ssh <vm> sudo ...` for every remote command.

Per-VM layout (all under `WORK=/var/tmp/dnv-integtest`):

```
/var/tmp/dnv-integtest/
  backing.img     # 2 GiB, fallocate'd
  store/          # --local-store (must exist BEFORE agent start; agent
                  # will not create it and fails startup reconcile without it)
  dnv-agent       # scp'd binary
  agent.log       # nohup'd stdout (JSON log records)
  pattern-*.bin   # test data files (migration cases, on the CN-role VM)
```

The VM-side helper the script runs for the per-case dm/nvmet/ANA state
queries and for the cleanup itself lives **outside** `$WORK`, at
`/var/tmp/dnv-integtest-helper.sh`. Not every VM-side command goes through
it: §4's preflight probes, `nvme connect` and the data IO are plain `ssh`
one-liners. It is re-shipped by `scp` at the start of every run
(`--cleanup-only` included), so a stale copy never matters, and §16's
`rm -rf $WORK` deliberately leaves it on the VMs — it is the file that runs
that cleanup.

Agent launch line (VM1 shown; VM2 identical with its own IP):

```
setsid nohup $WORK/dnv-agent dn \
  --grpc-network tcp --grpc-address <ip1>:29528 \
  --tr-type tcp --adr-fam ipv4 --tr-addr <ip1> --tr-svc-id 4200 \
  --local-store $WORK/store \
  --disk <loopdev> >> $WORK/agent.log 2>&1 < /dev/null &
```

`setsid` puts the agent in its own session so the exiting ssh command cannot
signal it with the channel's process group, and `< /dev/null` keeps it off
the ssh stdin, which the command would otherwise stay open on. The redirect
**appends**, which is what makes §15 step 2's "move the previous log aside
first" load-bearing: case D is the one case that relaunches an agent, and
its step 5 asserts the *post*-restart log holds no mutating operation, so
without the `mv` the re-opened file would still carry every pre-restart
`dmsetup create` and `nvme connect` and fail that assertion.

Notes: there is no extent-size flag (extent size arrives in
`SyncupDnRequest.extent_size`); logging is JSON on stdout only; shutdown is
SIGTERM (graceful). Kill in cleanup with `pkill -x dnv-agent`.

## 4. Assumptions and preflight checks

Hard assumptions (documented, not checked): both VMs are linux/amd64, same
lab image as prior nvmet/RAID1 experiments; kernel has nvme native multipath
and dm-clone available.

**uutils dd rule (lab landmine):** the VMs ship uutils dd 0.8.0, where
`iflag=`/`oflag=direct` silently misbehave (false failures / dropped writes).
The whole harness therefore **never uses `iflag=`/`oflag=` at all**. Writes
use `conv=fsync`; reads that must hit the media are preceded by
`sync; echo 3 > /proc/sys/vm/drop_caches`. Offsets use block-unit
`bs=1M skip=N count=N` only. Do not "fix" this back to direct IO.

Preflight (fail fast; installs nothing). A missing tool or missing
passwordless sudo dies with `missing: <what> on <vm>`, the form that names
what to install; the capability checks — free space, punch-hole, the free
ports and §7 step 3's deferred Write-Zeroes probe — instead name the VM and
the value measured (`vm1: /dev/loop0 reports write_zeroes_max_bytes=0`),
because there is nothing to install and the number is what the operator has
to act on. Neither form is universal: the driver-side build steps die with
their own text (`make build failed`), and the nvmet-configfs and
`nvme_core.multipath` equality checks below print the generic form
`<what> on vm<n>: got '<x>', want '<y>'`. The driver half runs first; the
per-VM half runs **after** the start-of-run cleanup, since the port check
is only meaningful once a crashed prior run's agents are gone:

- driver: `go`, `ssh`, `scp`, `sha256sum`, and a jq (a system `jq`, else the
  drop-in `gojq` built into the gitignored `integtest/bin/` with the Go
  toolchain — no package install); repo builds (`make build`).
- per VM, via `ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new`:
  - `sudo -n true` succeeds.
  - binaries: `dmsetup`, `nvme`, `losetup`, `blkdiscard`, `lsblk`, `dd`,
    `fallocate`, `sha256sum`, `cmp`, `pkill`, `jq`, `timeout`. No `lvm`: the
    dn agent runs no LVM command ([D13]).
  - `modprobe` (errors ignored, and **not** verified per module): `nvmet`,
    `nvmet_tcp`, `nvme_tcp`, `nvme_fabrics`, `dm_clone`, `loop`; then verify
    `/sys/kernel/config/nvmet` exists (mount configfs if absent) — the agent
    hardcodes that path and neither mounts nor modprobes. That check is not
    a per-module check either: it says only that `nvmet` is live, however it
    got there (module, built in, or already loaded). The other five are
    never verified, so one that failed to load surfaces later, at the first
    command that needs it — an `nvme connect`, a dm-clone create — with that
    command's own message. The next bullet's `/sys/module/nvme_core` read is
    the one exception, and it covers only `nvme_core`: if that is neither
    loaded nor built in, the `cat` fails and the preflight dies there.
  - `/sys/module/nvme_core/parameters/multipath` == `Y` (case A's standby
    assertions and the migration multipath merge depend on it).
  - `df /var/tmp` ≥ 3 GiB free; `/var/tmp` filesystem supports punch-hole
    (probe: `fallocate -p` on a scratch file) — side provisioning
    (`blkdiscard --zeroout`, `update_01.md` U4) and the case C zeros-verify
    both need the loop device's Write Zeroes to reach the backing file, and
    loop implements it with `fallocate`. The 3 GiB floor is unchanged and
    still ample: `--zeroout` on a loop materializes at most ~1 GiB of backing
    pages per DN, and here not even that, since `backing.img` is
    `fallocate -l 2G`-preallocated and the worst per-case draw is 4 extents =
    256 MiB per VM.
  - Write Zeroes on the loop device: once §7 step 3 has created it, assert
    `/sys/class/block/<loop>/queue/write_zeroes_max_bytes` is non-zero. `0`
    means the kernel would fall back to writing zero pages at bulk speed,
    breaking `update_01.md` U4's fast-Write-Zeroes assumption; DN5 then
    reports `meta_info = RES_STATUS_ERROR "disk lacks Write Zeroes"` and §7
    step 7's assertion fails with a much less obvious message. This one
    per-VM check necessarily runs inside §7 rather than in the pre-setup
    preflight block — the device does not exist yet — but it fails the run
    the same way, with the capability form of the message above.
  - ports 29528 and 4200 not listening (`ss -ltn`).

## 5. Identity plan and naming

`cluster_id = 0x1` on both DNs (shared cluster; identity is
`(cluster_id, dn_id)` — hostnames are irrelevant). `dn_id`: VM1 = `0x1`,
VM2 = `0x2`. All ids below are chosen so every case has a distinct `sp_id`
(cases never collide even if teardown is skipped with `--only`).

| case | sp_id | legs (leg: src side → dst side) | migr ids | CN ids (hosted on) |
|---|---|---|---|---|
| S | 0xa1 | leg 0x1: side 0x11 on DN1 | — | 0x21 (VM2) |
| A | 0xb1 | legs 0x1,0x2: sides 0x11,0x12 on DN1; legs 0x3,0x4: sides 0x13,0x14 on DN2 | — | 0x21 (VM2), 0x22 (VM1) |
| B | 0xc1 | leg 0x1: 0x11@DN1 → 0x12@DN2; leg 0x2: 0x21@DN2 → 0x22@DN1 | 0x31, 0x32 | 0x23 (VM2), 0x24 (VM1) |
| C | 0xd1 | leg 0x1: 0x11@DN1 → 0x12@DN2; leg 0x2: 0x21@DN2 → 0x22@DN1 | 0x41, 0x42 | 0x23 (VM2), 0x24 (VM1) |
| D | 0xe1 | leg 0x1: side 0x11 on DN1; gated dst side 0x12 on DN2 | 0x51 | 0x21 (VM2) |

Derived names the script greps for (all ids rendered `%016x`), e.g. for
cluster 0x1 / DN1 / case B leg 0x1 / CN 0x23:

- side device: `dnv-0000000000000001-0000000000000001-4-00000000000000c1-0000000000000011`
  (dm kind `4`: a dm-linear concatenating the side's extent runs, [D13])
- clone-metadata wrapper (dst DN 0x2, migr 0x31):
  `dnv-0000000000000001-0000000000000002-5-00000000000000c1-0000000000000031`
- side→CN subsystem NQN:
  `nqn.2024-01.io.dnv:2:0000000000000001:00000000000000c1:0000000000000001:0000000000000023`
- CN hostnqn: `nqn.2024-01.io.dnv:1:0000000000000001:0000000000000023`
- migration-src subsystem NQN (src DN 0x1, migr 0x31):
  `nqn.2024-01.io.dnv:3:0000000000000001:0000000000000001:00000000000000c1:0000000000000031`
- dst DN hostnqn (used by the agent itself for the migration connect):
  `nqn.2024-01.io.dnv:0:0000000000000001:0000000000000002`
- dm devices: `dnv-{cluster}-{dn}-0-{sp}-{side}-{cn}` (error),
  `...-1-...` (linear, the exported dev), `...-2-{sp}-{migr}` (migr src),
  `...-3-{sp}-{migr}` (dm-clone), `...-4-{sp}-{side}` (side device),
  `...-5-{sp}-{migr}` (clone-metadata wrapper).
- namespace identity per leg: uuid/nguid = `common.DnNsIdentity(cluster, sp, leg)`,
  serial = `%016x` of leg_id, model `dnv`. The CN-side device node is found
  via `/dev/disk/by-id/nvme-uuid.<uuid>` (uuid printed by `dnagentctl ns-id`).

Cleanup can therefore target everything by prefix: dm `dnv-*` and nvmet
objects `nqn.2024-01.io.dnv:*` — there are no VG prefixes left, since the DN
carries the [D13] disk format instead of LVM.

## 6. Sizing and the extent budget

**Why 2 GiB:** headroom. The [D13] format's fixed prefix costs 256 MiB
(`DnDataOffset`) and there is no per-node LVM tax on top, so a smaller file
would fit the cases — but 2 GiB keeps the per-case draws comfortable and lets
the sizing assertion below stay a single exact number.

Budget per VM, `extent_size = 67108864` (64 MiB — the proto field is raw
bytes; also exactly `MinDnExtSize`):

```
backing.img              2048 MiB (fallocate -l 2G) = 2147483648 B
[D13] fixed prefix       -256 MiB (DnDataOffset: header + A/B table slots
                                   + the 192 MiB clone-metadata area)
data area                1792 MiB = 1879048192 B  ← the exact GetDnSize reply
usable extents             28       (floor(1792/64))
```

Worst per-case draw: case B/C = 4 extents per VM (one 2-extent src side +
one 2-extent dst side). Cases fit with >20 extents headroom. Sides are kept
at 1–2 extents deliberately: every agent OS command runs under a hard-coded
3 s soft / 5 s kill timeout, and side provisioning (`update_01.md` U4)
`blkdiscard --zeroout`s the side device in batches of
`DnZeroBatchExtCnt = 10` extents — 640 MiB per command at this extent size,
so a 1–2 extent side is always a single batch, and on a loop device that
batch is `fallocate` on the backing file, i.e. microseconds. A failed or
timed-out batch leaves its bits unset and is retried no sooner than
`DnZeroRetryInterval = 5` seconds later, so the harness never has to reason
about partial zeros.

Migration parameters (mirroring CP defaults and `migr_test.go`):
`block_size = 1048576` (1 MiB regions), `meta_blocks = 3`,
`dm_clone_conf = {hydration_threshold: 1, hydration_batch_size: 1}`
(`DefaultMigrThreshold`/`DefaultMigrBatchSize`; batch 1 also slows hydration
slightly, widening the mid-hydration observation window). A 128 MiB side ⇒
128 regions; the dm-clone metadata slot rounds ~4 MiB up to whole
`DnCloneMetaUnit` (4 MiB) units inside the 192 MiB clone-metadata area (48
units) — no pressure. Bitmap chunks ≤ `MaxMigrBmCnt` = 4; the plan uses 2.

## 7. Setup phase (after start-cleanup)

Per VM, in order:

1. `mkdir -p $WORK $WORK/store`
2. `fallocate -l 2G $WORK/backing.img`
3. `LOOP=$(losetup --find --show $WORK/backing.img)` (script records it);
   immediately after it, the deferred §4 preflight item — assert
   `/sys/class/block/$(basename $LOOP)/queue/write_zeroes_max_bytes != 0`,
   failing the run with `vm<n>: <loop> reports write_zeroes_max_bytes=0`
   (§4: the `missing:` form would misname a lab capability as an
   uninstalled binary)
4. `scp bin/dnv-agent` (built once on the driver:
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build` — pure-Go tree, static)
5. launch the agent (§3 command line), as root, via nohup
6. wait-up: `dnagentctl get-dn-size --wait 15` and **assert
   `size == 1879048192` exactly** — the [D13] *data area* (raw size minus
   `DnDataOffset`), not the raw device. First real assertion, and `GetDnSize`
   is lock-free so it doubles as the liveness probe
7. `dnagentctl syncup-dn` with the case-independent baseline: revision 1,
   `extent_size 67108864`, empty side list; assert reply code 0 and
   `dn_info.{disk,meta,port}_info.status == RES_STATUS_OK` — this proves the
   disk format (header + volume table) and the nvmet port setup on both VMs
   before any case runs. Since `update_01.md` U4 that `meta_info` OK
   additionally proves the DN5 Write-Zeroes fail-fast check passed
   (`write_zeroes_max_bytes != 0` on `--disk`), which is why step 3 asserts
   the same attribute directly: a `0` there is a lab problem, not an agent
   bug, and deserves the clearer message.

The driver binary `integtest/bin/dnagentctl` is built natively on the driver
(`go build ./integtest/dnagentctl`).

## 8. The driver: `dnagentctl`

Plaintext gRPC (`insecure.NewCredentials()`), **no server reflection exists**
— that is why this exists instead of `grpcurl` (which would need
`-proto pb/schema.proto` plus bash-assembled JSON and base64 bitmaps).

Global flags: `--addr <ip:29528>` (required), `--cluster`, `--dn`,
`--trace-id <s>` (sent as gRPC metadata key `trace_id`; the script passes
`it-<case>-<step>` so records correlate across driver and both agent logs),
`--timeout <sec>` (default 10). The script's `ctl` wrapper raises it to 60 s
for `syncup-dn` and `syncup-side`: one converge pass runs dozens of OS
commands under their own 3 s soft / 5 s kill budgets (§6), and in §12 each
DN plays src for one leg and dst for the other, so a converge pass runs
while that same node is serving the other migration's hydration and
NVMe-oF traffic. The 10 s default is sized for the read-only RPCs; the
syncups get the much larger budget. The wrapper places the flag *before* the
caller's arguments, so an explicit per-call `--timeout` still wins. All id
flags accept `0x` hex. Every reply is printed as protojson on stdout
(the script parses with `jq`). Exit non-zero on gRPC error **or**
`agent_reply.code != 0` (except where the script explicitly expects a
code, e.g. the case D stale probe: `--expect-code 1`).

Subcommands:

| cmd | flags beyond globals | notes |
|---|---|---|
| `get-dn-size` | `--wait <sec>` | retry until success within wait |
| `syncup-dn` | `--revision`, `--extent-size`, repeated `--side sp:leg:side` | full desired side list every call (declarative) |
| `syncup-side` | `--revision`, `--sp --leg --side`, `--ext-cnt`, `--cntlid-slot`, `--primary-cn`, repeated `--standby-cn`, `--sp-level readwrite\|no_migration` (the parser takes all eight `SpLevel` names, so the flag stays usable if a later case needs another level; this suite sends only these two, §19), `--provisioned` (bool, **default false** — the proto zero value and the gateway's default for a new `Side`; every steady-state call must pass it, §9), optional `--migr-src migr:dstSide:dstDn` (+ `--dst-provisioned`, bool, default false), optional `--migr-dst migr:srcSide:srcDn --src-traddr <ip> --src-trsvcid 4200 --block-size --meta-blocks --hydr-threshold --hydr-batch --bm-cnt` | src_nvme_tr_conf is always `tcp/ipv4`; `side_conf.provisioned` and, with `--migr-src`, `migr_src_conf.dst_provisioned` are sent verbatim — the script plays the worker's flip (§9). Both bools MUST be written `--flag=true` / `--flag=false`: Go's `flag` package never consumes the next token for a bool, so `--provisioned true` parses as `--provisioned=true` plus a positional and silently drops every later flag |
| `push-migr-bm` | `--revision`, `--sp --leg --side`, `--migr`, `--bm-idx`, `--bitmap-hex` | revision = side's current revision (gates, never advances) |
| `get-dn-info` / `get-side-info` | (`--sp --leg --side`) | `get-side-info` prints the reply as protojson on **stdout** (unchanged — case D deep-equals it) plus a `dnagentctl: zeroed <k>/<n>` progress line on **stderr**, from `side_info.{zeroed_ext_cnt,total_ext_cnt}` |
| `check-dn` / `check-side` | `--revision`, `--show-info` (+ side ptr) | opens the bidi stream, one request/reply round, closes |
| `wait-hydrated` | side ptr, `--interval 0.5`, `--timeout 120`, `--min-first <n>` | polls `GetSideInfo`, parses `migr_dst_info.dm_clone_info.details` with `agent.ParseCloneStatus`; `--min-first` asserts the first sample's hydrated ≥ n; logs each sample to stderr and exits when hydrated == total, printing `{"first_hydrated":k,"first_total":n,"hydrated":n,"samples":s,"total":n}` on stdout — the `first_*` pair is the §14 layer-3 skip jump, kept in the exit line so a run log carries it even when nobody watched stderr |
| `wait-zeroed` | side ptr, `--interval 0.5`, `--timeout 120` | polls `GetSideInfo`, reads `side_info.zeroed_ext_cnt`/`total_ext_cnt`, logs each sample to stderr; exits when `zeroed == total > 0` (the `> 0` guard matters: before the allocation record exists `total_ext_cnt` is 0 and `zeroed >= total` would trivially succeed), printing `{"zeroed":n,"total":n,"samples":k}` on stdout; fails fast on `side_dev_info.status == RES_STATUS_ERROR`. Binds its globals with `withTimeout = false` and spends `--timeout` as its own polling budget, exactly like `wait-hydrated` |
| `ns-id` | `--sp --leg` | prints uuid/nguid/serial via `common.DnNsIdentity` for CN device lookup, plus `by_id` = `/dev/disk/by-id/nvme-uuid.<uuid>` ready to use — the script reads that field and never assembles the path itself, so the by-id convention lives in one place |
| `host-id` | `--hostnqn <nqn>` | prints `common.NvmeHostId(nqn)`; local, no gRPC. Every emulated `nvme connect` passes it as `--hostid` (§3) |

## 9. Conventions

- **Revisions**: the script keeps one monotonic counter per DN (`REV1`,
  `REV2`), incremented before every state-changing `syncup-*`. Equal-revision
  re-sends are legal full re-applies and are used deliberately (fetching
  `bm_info`, case D idempotency). Never send a lower value except the case D
  stale probe.
- **Ordering**: a side pointer must appear in `SyncupDn.side_pointer_list`
  before its first `SyncupSide` (else `code 2`).
- **Two-phase side setup (the worker's `provisioned` flip, played by the
  script).** Since `update_01.md` U4 a side is exported only when the
  request carries `provisioned = true` **and** every extent's `zeroed_bits`
  bit is set. There is no worker in this suite, so every `syncup-side` that
  *creates* a side is issued twice: through the `sync_side_2phase()` helper,
  or — for §12's two migration destinations — through the equivalent pair
  `migr_provision_dst()` (phases a and b) and `migr_declare_dst()` (c):
  (a) `--provisioned=false` — the agent allocates the extent runs, builds
      `DnSideName` and starts the background zeroing goroutine. Assert
      `side_dev_info.status` is `RES_STATUS_PROVISIONING` **or already
      `RES_STATUS_OK`** (`assert_provisioning_or_ok` — which of the two a
      phase-(a) reply carries is a genuine race against the zeroing
      goroutine, the two-valued case the Status-assertions bullet below
      blesses; the `zeroing k/n` details string is not asserted); every
      `cn_id_to_dm_error/linear/nvmeof[<cn>]` entry
      present and `RES_STATUS_PROVISIONING`; **no** `:2:` subsystem in
      configfs yet; and, kernel-side, **no dm device of kind 0 or 1 for that
      side** (helper `export_dms`, the per-CN dm-error/dm-linear pair) — the
      reply's statuses say the agent believes it exported nothing, this says
      the node agrees. Kind 4, the side device, is deliberately not matched:
      building it is exactly what phase (a) does. The pattern is scoped to
      the one side rather than the whole `sp`, because cases A and B/C
      provision the sides of a pool one after another and a pool-wide
      pattern would match a sibling side's already-live export and fail a
      correct run. `zeroed_ext_cnt < total_ext_cnt` logged tolerantly (see
      the window note below).
  (b) `wait-zeroed` until `zeroed_ext_cnt == total_ext_cnt`.
  (c) `REV<dn>++` (the per-DN `REV[dn]` counter of the Revisions bullet)
      and re-send the identical request with
      `--provisioned=true` (the flip). Assert the case's listed rows
      `RES_STATUS_OK` — cases S and A check every per-CN map entry; later
      stages check the subset their own step names (the §12 sources:
      `side_dev_info` + `cn_id_to_nvmeof`; case D likewise) — and
      `zeroed_ext_cnt == total_ext_cnt == ext_cnt`.
  Every *later* `syncup-side` for that side — re-sends, migration stages,
  the finish step — **must keep `--provisioned=true`**: a request that drops
  it is a legal instruction to retire the side's exports (row 3 of the
  `update_01.md` §9.4 converge matrix), not a no-op. `--provisioned=true`
  with incomplete bits is an `ERROR` with no export, and on a side whose
  allocation record is missing it is a hard `ERROR` that never re-allocates
  — the agent always trusts its own bits over the flag; neither negative row
  is exercised here (§19). `RES_STATUS_PROVISIONING` never sets `err_epoch`:
  it means *healthy, not ready, no action*. Timing: with 64 MiB extents a
  `DnZeroBatchExtCnt = 10` batch is 640 MiB and loop maps Write Zeroes onto
  `fallocate`, so phase (b) is normally instant — sample once before it and
  log `provisioning window HIT` when `zeroed < total`, else a tolerated
  `window missed`, in the same tolerant style as the §12 grace-window and
  read-through probes.
- **Status assertions are exact.** `RES_STATUS_PROVISIONING` is a fourth
  status, so a bare "not OK" assertion silently accepts it. Any assertion
  that means "must be ERROR" compares against `RES_STATUS_ERROR` explicitly,
  and the genuinely two-valued phase-(a) checks use
  `assert_provisioning_or_ok` — never `assert_not_ok`.
- **Converge check**: after a case reaches steady state — cases S and A run
  both rounds, the B/C teardown runs `check-dn` only, and case D proves its
  steady state by the §15 snapshot diff instead of check rounds (§18) — one
  `check-dn` and/or one `check-side` round with `--show-info` asserts reply
  code 0, matching revision, and all expected `ResInfo.status ==
  RES_STATUS_OK`.
  Check rounds only ever run at steady state, i.e. after the phase-(c) flip,
  so `RES_STATUS_PROVISIONING` must never appear in one.
- **Data IO on CNs**: writes `dd conv=fsync`, reads after
  `sync; echo 3 > drop_caches`; never `iflag=/oflag=` (§4).
- Every ssh command the script runs is echoed with a `[vm1]`/`[vm2]` prefix;
  stages log `=== <case>: <stage>` lines.

## 10. Case S — `smoke`

1. `syncup-dn` DN1 (REV1++) with side `(0xa1, 0x1, 0x11)`; assert code 0.
2. `syncup-side` DN1 side 0x11, §9 two-phase: (a) `ext_cnt 1`, slot 0,
   primary CN 0x21, no standbys, `sp_level readwrite`,
   `--provisioned=false` — assert code 0, `side_dev_info` PROVISIONING-or-OK
   (§9(a)), `cn_id_to_dm_error/linear/nvmeof[0x21]` all PROVISIONING, and no `:2:`
   subsystem and no kind-0/1 dm device for the side yet (the §9 phase-(a)
   gate proof); (b) `wait-zeroed` ⇒ 1/1; (c) REV1++ and re-send with
   `--provisioned=true` — assert code 0, `side_dev_info` OK,
   `cn_id_to_dm_error/linear/nvmeof[0x21]` all OK.
3. VM2 (as CN 0x21): `nvme connect -t tcp -a <ip1> -s 4200 -n <SideToCnNqn>
   --hostnqn <CnHostNqn(0x21)> --hostid <host-id of that hostnqn>`; wait for
   `/dev/disk/by-id/nvme-uuid.<uuid>` (uuid from `dnagentctl ns-id`);
   the path shows `live` in `nvme list-subsys -o json` and `optimized` in
   the path's sysfs `ana_state` (list-subsys carries no ANA state on
   nvme-cli 2.16, so ANA is always read from sysfs).
4. IO: write 4 MiB from a local urandom file (`dd bs=1M count=4 conv=fsync`),
   `sync`, drop caches, read back, sha256 equal.
5. `check-dn` + `check-side` rounds (§9).
6. Teardown: VM2 `nvme disconnect -n <nqn>`; `syncup-dn` DN1 (REV1++) with
   an **empty side list** → side torn down declaratively. Assert via
   `get-dn-info` (all base infos still OK) and on VM1: no `dnv-*-0xa1-*` dm
   devices at all (the side device is dm kind 4, so the one pattern covers
   it) and no `:2:` subsystem in configfs.

Success: all assertions pass; proves build/ship/launch, gRPC, dm/nvmet
plumbing, CN connect and the IO path — before the bigger cases run.

## 11. Case A — `sides` (multiple sides → multiple CNs)

Layout (§5): 4 legs; sides 0x11,0x12 on DN1 (primary CN 0x21, standby CN
0x22), sides 0x13,0x14 on DN2 (primary CN 0x22, standby CN 0x21). Each side:
`ext_cnt 1`, slot 0, `sp_level readwrite`. That yields 8 nvmet subsystems
(one per side per CN with `allowed_hosts` = exactly that CN's hostnqn).

1. `syncup-dn` per DN (its two side pointers), then 4 × `syncup-side` in
   the §9 two-phase form, one side after another — the §9 helper's
   per-side bookkeeping relies on sequential provisioning; only the §12
   lockstep pairs run concurrently. Assert every
   `cn_id_to_*` map entry PROVISIONING after phase (a) and OK after phase
   (c), for both CN ids.
2. Connects — the multi-CN matrix, 8 `nvme connect`s total:
   - VM2 as CN 0x21 → DN1's two subsystems for 0x21 (**primary** paths) and
     DN2's two subsystems for 0x21 (**standby** paths).
   - VM1 as CN 0x22 → DN1's two for 0x22 (**standby**) and DN2's two for
     0x22 (**primary**).
3. Assertions per VM:
   - primary namespaces: ana_state `optimized`; 4 MiB write/readback sha256
     per §9 (per leg, distinct pattern files).
   - standby namespaces: ana_state `non-optimized`; the device node exists;
     **reads fail with EIO** (`dd ... count=1` must fail) — the standby
     export is deliberately backed by dm-error until promotion; this asserts
     that wiring, not a bug. Note this is a *provisioned* side: an
     unprovisioned one has no export at all, which is a different thing
     (§9).
   - isolation: on each DN, each subsystem's `allowed_hosts/` contains
     exactly one hostnqn (configfs `ls`).
4. `check-dn`/`check-side` rounds on both DNs.
5. Teardown: disconnect all 8; empty `syncup-dn` side lists both DNs; assert
   no `0xb1` dm devices or subsystems remain.

## 12. Cases B and C — migration choreography (shared)

Both cases run **two concurrent migrations in opposite directions**
(DN1→DN2 and DN2→DN1, §5), advanced stage-by-stage in lockstep; within each
converge/poll stage the two per-migration `dnagentctl` calls run as
background jobs joined with `wait`, so the same agent handles overlapping
side converges (locks exercised) while each DN simultaneously plays src for
one leg and dst for the other. (The bitmap-push stage is the exception:
its per-chunk `push-migr-bm` calls run sequentially.)

Per migration (leg L: src side S1 slot 0 on DNsrc, dst side S2 slot 1 on
DNdst, migr M, primary CN C hosted on the *other* VM from DNsrc):

**Stage 0 — src side up, data prep**
1. `syncup-dn` both DNs: DNsrc list += (sp,L,S1), DNdst list += (sp,L,S2).
2. `syncup-side` DNsrc S1, §9 two-phase: `ext_cnt 2` (128 MiB), slot 0,
   primary C, `sp_level readwrite`, no migr confs — phase (a)
   `--provisioned=false`, phase (b) `wait-zeroed` ⇒ 2/2, phase (c) REVsrc++
   with `--provisioned=true`. Assert OK only after (c); the step 4 pattern
   write must not start before it, since an unprovisioned side has no export
   to write to.
3. CN VM: connect the src path; wait `optimized`.
4. Data prep **through the CN device** (the production path): generate
   `pattern-M.bin` = 128 MiB from `/dev/urandom` on the CN VM, record
   `sha256(pattern)` and `sha256(first 64 MiB)`; `dd if=pattern-M.bin
   of=<dev> bs=1M conv=fsync`, `sync`. The pattern file is the verification
   oracle; nothing re-reads src later.
5. Baseline `get-side-info` DNsrc S1: `migr_src_info`/`migr_dst_info` empty.

**Stage 1 — declare dst, gated** *(the staged flow that makes bitmaps
race-free: `sp_level no_migration` declares the migration without creating
the clone or connecting, so chunks can be pushed first)*
6. `syncup-side` DNdst S2, §9 two-phase, at `sp_level no_migration`
   throughout:
   - 6a. (REVdst++) `--provisioned=false`, `ext_cnt 2`, slot 1, primary C,
     `migr_dst_conf{migr_id M, src_side_id S1, src_dn_id DNsrc,
     src_nvme_tr_conf{tcp,ipv4,<ip_src>,"4200"}, block_size 1048576,
     meta_blocks 3, dm_clone_conf{1,1}, bm_cnt (C: 2, B: 0)}`. Assert:
     `side_dev_info` PROVISIONING-or-OK (§9(a)), the §9 phase-(a) gate proof for the one
     CN this request names (`cn_id_to_dm_error`, `cn_id_to_dm_linear` and
     `cn_id_to_nvmeof` all PROVISIONING, no `:2:` subsystem in configfs on
     DNdst, and no kind-0/1 dm device for S2),
     `migr_dst_info.dm_clone_info` **absent** — `assert_gated`, i.e. the
     field is omitted or `MISSING`, never `PROVISIONING` and never `OK`.
     Two gates coincide on this request and the **level wins**:
     `sp_level no_migration` makes `wantMigr` false, so no `migr_dst_info`
     is emitted at all, and step 6c below asserts exactly the same absence
     once the side is provisioned. (`update_01.md` U4's "`migr_dst_info.*`
     report `PROVISIONING`" is the *provisioning* gate's row and applies only
     at a level **below** `SP_LEVEL_NO_MIGRATION` with
     `provisioned = false` — a shape this suite never sends, because it
     declares the dst gated at `no_migration` and only lowers the level in
     step 11, after the flip.) The dst still provisions first, linear +
     zeroing only: no per-CN stacks, no metadata slot, no `nvme connect`, no
     dm-clone.
   - 6b. `wait-zeroed` DNdst S2 ⇒ 2/2.
   - 6c. (REVdst++) the identical request plus `--provisioned=true`.
     Assert: `side_dev_info` OK, per-CN stack OK,
     `migr_dst_info.dm_clone_info` **missing** (still gated by
     `sp_level no_migration` — assert absence, not merely "not OK", since
     PROVISIONING would also pass a bare not-OK check, §9), no
     `nvme connect` issued yet.
7. CN VM: connect the dst path (same NQN, `<ip_dst>`). Path appears with ns
   `inaccessible` (no by-id node for it yet — expected); the multipath
   device still serves via src.
8. **Case C only**: push bitmap chunks (§14), then re-send step 6c verbatim
   (equal revision — idempotent, `--provisioned=true` included per §9)
   purely to read the reply's `bm_info`: assert `res_id == M`,
   `bm_idx_list == [0,1]`.
8b. **`dst_provisioned = false` equivalence probe** (both cases; CN IO still
   running, so it must land *before* stage 2 opens the quiesce window —
   its whole point is that IO keeps flowing). `syncup-side` DNsrc S1
   (REVsrc++): the stage-0 side_conf (still `--provisioned=true`) **plus**
   `migr_src_conf{migr_id M, dst_side_id S2, dst_dn_id DNdst,
   --dst-provisioned=false}`. `update_01.md` U4 makes that normatively
   equivalent to *no* `migr_src_conf` at all, so assert code 0 and that the
   src is untouched:
   - `migr_src_info.{dm_linear_info,nvmeof_info}` report
     `RES_STATUS_PROVISIONING` — the only observable difference;
   - `fenced_linears` finds **no** suspended `dnv-*-1-{sp}-*` (no grace
     window opened);
   - no `nqn...:3:{cluster}:{DNsrc}:{sp}:{M}` subsystem exists in configfs
     (no migr-src export);
   - the CN's src path is still `optimized` and a 1 MiB read through the CN
     device still succeeds.
   Without this gate the src would fence the primary's path the moment the
   migration is declared, and the leg would have no serving path for the
   whole dst zeroing window — which is exactly why the gate exists. In
   production the worker fills `dst_provisioned` from the dst `Side`'s flag;
   here the script sets it by hand, so the probe is meaningful even though
   step 6c already flipped the dst.

**Stage 2 — src cutover** *(CN IO quiesced from here until stage 3 asserts
`optimized`: with src inaccessible and dst not yet live, the namespace
briefly has no serving path and IO would hang; the ANA-inaccessible path
also has no /dev node)*
9. `syncup-side` DNsrc S1 (REVsrc++): same side_conf plus
   `migr_src_conf{migr_id M, dst_side_id S2, dst_dn_id DNdst,
   --dst-provisioned=true}` — the same call as step 8b with the gate open,
   which is what actually starts the §11.2 cutover. Assert
   `migr_src_info.{dm_linear_info,nvmeof_info}` OK. The per-CN linears now
   enter the §11.2 grace window — suspended in place for `SuspendSeconds` =
   60 s, then reloaded onto their dm-errors ([D12]) — and the RPC returns
   without waiting for it. The script **observes** the window
   (`fenced_linears`, which greps `dmsetup info -o name,attr` for a
   suspended `dnv-*-1-{sp}-*`) and logs `grace window HIT` or a
   `window missed` warning, in the same tolerant style as the step 13
   read-through probe: a slow preceding step could push the sample past the
   window, and the end state is what the step 19 residue checks prove. Note
   that from here until the window closes, an *external* block-device scan on
   DNsrc (udev, `blkid`, an operator's `lsblk`) would block in D state — the
   bounded window is the whole mitigation.
10. CN VM: poll the src path's sysfs `ana_state` → `inaccessible`
    (liveness via `nvme list-subsys`; ANA from sysfs, as in §10 step 3).

**Stage 3 — enable dst (the concurrency point: both migrations' enables run
in parallel)**
11. `syncup-side` DNdst S2 (REVdst++): identical to step 6c but
    `sp_level readwrite`. In this one converge the agent: allocates the
    clone-metadata slot (first 8 KiB zeroed before the record is persisted)
    and builds its wrapper device `dnv-*-5-*`, `nvme connect`s to
    `nqn...:3:{cluster}:{DNsrc}:{sp}:{M}` as
    `DnHostNqn(cluster, DNdst)`, creates the dm-clone with hydration off,
    (C:) applies persisted chunks via `blkdiscard`, enables hydration,
    reloads the primary linear onto the clone, moves the ns to `optimized`.
    Assert `migr_dst_info.{target_info,dm_clone_info}` both OK, **and** the
    dm-clone's live table: one `ssh … dmsetup table dnv-*-3-{sp}-{M}` on
    DNdst (helper `clone_table`) matching
    `' 2 no_hydration no_discard_passdown( |$)'`. That check is
    **mandatory**, not optional hardening, and the regex is exact on both
    features. `dmsetup table` (`STATUSTYPE_TABLE`) reprints the constructor
    args dm-clone saved at create time, so `no_hydration` never leaves it —
    only `dmsetup status` (`STATUSTYPE_INFO`) recomputes the live flags that
    `dmsetup message … enable_hydration` changes — and the agent never
    reloads the clone on feature drift, so the pair the create used is the
    pair the table shows for the life of the device. It is the one
    `update_01.md` U1 assertion this suite can make against a real kernel:
    hydration *progress* is observed only through
    `migr_dst_info.dm_clone_info.details`, the raw `dmsetup status` line the
    agent produced, parsed with `agent.ParseCloneStatus` (§8
    `wait-hydrated`, steps 13-14, §14 layer 3), and a missing
    `no_discard_passdown` is invisible there — after U1 the dn migration
    dm-clone carries both features, exactly like the cn clone dm-clone, so
    that a §11.4 skip `blkdiscard` stays metadata-only instead of reaching
    the destination disk.
12. CN VM: poll until dst path `optimized` (≤30 s; typically immediate).
13. **Mid-hydration read-through probe**: read the *last* must-copy MiB via
    the CN device (`dd bs=1M skip=<K> count=1`; B: K=127, C: K=63) and
    sha256-compare against the same window of `pattern-M.bin`
    (`dd if=pattern-M.bin bs=1M skip=<K> count=1`). The data equality is a
    **hard** assert (correct whether or not hydration reached it — served by
    read-through from src if not). Immediately before the read, sample
    `get-side-info` DNdst: if `hydrated < total`, log `read-through window
    HIT` (the strong version of the proof); if hydration already finished,
    log a warning `window missed` but do not fail — loop-device hydration of
    ≤128 MiB can outrun the script.
14. `wait-hydrated` DNdst S2 until `hydrated == total` (=128), timeout 120 s.
    Case C passes `--min-first 64` (§14).

**Stage 4 — finish (production order: dst first, then src)**
15. `syncup-side` DNdst S2 (REVdst++): side_conf only (still slot 1),
    **no `migr_dst_conf`** → clone flushed and removed, agent-side
    `nvme disconnect` from src, the metadata wrapper removed and its slot
    freed, primary linear back onto the plain side device, ns stays
    `optimized`. Assert `migr_dst_info` no longer OK
    (missing/empty) and per-CN infos OK. The request still carries
    `--provisioned=true`: dropping it here would read as "retire this side's
    exports", not as "finish the migration" (§9).
16. `syncup-dn` DNsrc (REVsrc++): side list **minus** (sp,L,S1) → full src
    side teardown (export, dm stack, side device, extent record). If this
    lands while the §11.2 grace window is still open, the agent resumes the
    fenced linears before its first teardown step — disabling an nvmet
    namespace closes its backing device, and `dmsetup remove` does not
    succeed on a suspended one. The CN's src controller dies with
    DNR and will not reconnect.
17. CN VM: explicitly `nvme disconnect` the dead src controller — identify
    it from `nvme list-subsys -o json` by `traddr == <ip_src>` and
    disconnect by device (`nvme disconnect -d /dev/<ctrl>`), **not** by NQN
    (`-n` would kill both paths of the shared NQN).
18. Final verification via the CN multipath device (only the dst path
    remains): drop caches, then per-case checks (§13/§14), then a
    post-migration **write** probe: write 1 MiB at `bs=1M seek=5`,
    readback equal — the migrated side is live and writable.
19. Case teardown: CN disconnects; `syncup-dn` both DNs back to empty lists;
    assert no case dm devices or subsystems remain; `check-dn` rounds.

## 13. Case B — `migr_full` specifics

- Step 6/11 `bm_cnt 0`; **no** `PushMigrBitmap` calls.
- Step 8 replaced by: equal-revision re-send asserting `bm_info.bm_idx_list`
  is **empty**.
- Step 18 data check: sha256 of the whole 128 MiB device ==
  sha256(`pattern-M.bin`) — full copy, including the regions case C will
  skip, including the first 3 MiB meta region.
- Log check on DNdst: **zero** `blkdiscard` records mentioning the dm-clone
  device `dnv-*-3-*` (discard-based skipping must not happen without
  bitmaps). The device scoping is load-bearing, and doubly so since
  `update_01.md` U4: side provisioning issues `blkdiscard --zeroout` against
  the **side** device (`dnv-*-4-*`), one record per `DnZeroBatchExtCnt`
  batch at phase (a) of the §9 two-phase setup, and those are expected.

## 14. Case C — `migr_bitmap` specifics (worked example)

Bitmap semantics under test: wire bit `1` = unwritten/skippable, LSB-first
within a byte; chunks concatenate in `bm_idx` order and only the contiguous
prefix from index 0 applies; bit `i` maps to dm-clone region
`meta_blocks + i`; the agent marks skipped regions hydrated via
`blkdiscard --offset ((meta_blocks+i) * block_size) --length ...` on the
dm-clone device, coalescing runs. `meta_blocks` regions 0..2 are never
skippable — the meta region always copies.

Chosen layout (128 regions of 1 MiB; 125 data bits, 3 pad bits):

```
bits 0..60   = 0 (must copy)   → regions  3..63   (+ meta regions 0..2)
bits 61..124 = 1 (skip)        → regions 64..127
bits 125..127 = 0 (pad past region count; agent clips)
→ 16 bitmap bytes, pushed as 2 chunks of 8:
   bm_idx 0: 00 00 00 00 00 00 00 e0      --bitmap-hex 00000000000000e0
   bm_idx 1: ff ff ff ff ff ff ff 1f      --bitmap-hex ffffffffffffff1f
→ 64 regions copied (regions 0..63 = first 64 MiB), 64 regions skipped
   (regions 64..127 = second 64 MiB): one clean 64 MiB boundary.
```

(Byte 7 = bits 56..63 with 61,62,63 set = 0x20|0x40|0x80 = 0xe0; byte 15 =
bits 120..127 with 120..124 set = 0x1f.)

Src data prep is identical to case B — **random data across all 128 MiB,
including the skip range**. That makes the proof differential against case
B: same input, different outcome, so the delta is attributable only to the
bitmap.

The four assertion layers:

1. **Behavioral (data)**, step 18: first 64 MiB == `sha256(head -c 67108864
   pattern-M.bin)` (meta region + must-copy regions copied); second 64 MiB
   is **all zeros** (`dd bs=1M skip=64 count=64` into a file, `cmp` against
   64 MiB of `/dev/zero`) even though src holds random data there — the
   agent skipped, not copied. (Freshly provisioned dst sides read zero
   because `update_01.md` U4 writes zeros over the whole side device with
   `blkdiscard --zeroout` before the side is ever exported and records it
   per extent in the volume table's `zeroed_bits` — a guarantee, not a
   discard side effect. The old wording here leaned on "the trim protocol
   discards the side device and loop devices punch holes", which was never a
   hardware guarantee; that is exactly the multi-tenancy hole U4 closes.
   Preflight still verifies punch-hole/Write-Zeroes support, since loop
   implements both through `fallocate`.)
2. **API**: step 8 `bm_info.res_id == M`, `bm_idx_list == [0,1]`; both
   `push-migr-bm` replies code 0.
3. **Hydration counter**: `wait-hydrated --min-first 64` — the first status
   sample after enable must already show ≥ 64/128 hydrated (the skip jump;
   background copying has barely started at batch size 1). If this proves
   racy in practice, keep ≥ 64 but drop the implicit < 128 expectation —
   the hard floor is what matters.
4. **Log arithmetic (meta_blocks proof)**: on DNdst,
   `grep '"blkdiscard"' agent.log` must contain exactly one record for the
   dm-clone device `dnv-*-3-{sp}-{migr}` with `--offset 67108864
   --length 67108864`: first skip bit 61 → region 3+61 = 64 → offset
   64 × 1 MiB; run of 64 bits → length 64 MiB. Wrong meta_blocks handling
   shifts the offset by ±N MiB and this assertion catches it exactly. (This
   is a log-format-coupled check by design; if the JSON layout drifts,
   adjust the grep, not the assertion.) Scope the grep by device: since
   `update_01.md` U4 the same log also holds side-provisioning
   `blkdiscard --zeroout` records against `dnv-*-4-{sp}-{side}`; and since
   U1 the dm-clone's table reads `2 no_hydration no_discard_passdown`, so
   these skip discards are metadata-only by construction and never reach the
   destination disk.

## 15. Case D — `restart` (persistence and idempotent reconcile)

Setup: on DN1, side (0xe1, 0x1, 0x11) exported to CN 0x21 (as in smoke,
with the CN connected and 4 MiB of data written); on DN2, the matching
**gated** migration dst side 0x12 (`sp_level no_migration`,
`migr_dst_conf{migr 0x51, ...}`) with **one** bitmap chunk pushed
(`bm_idx 0`, `--bitmap-hex 00000000000000e0`). Both sides come up through
the §9 two-phase sequence and are fully zeroed
(`zeroed_ext_cnt == total_ext_cnt`) before the step 1 snapshot; the dst
side carries `--provisioned=true` even though its migration is still gated
by `sp_level no_migration`.

1. Snapshot: `get-dn-info` + `get-side-info` on both DNs; `bm_info` via
   equal-revision `syncup-side` re-send on DN2 (`bm_idx_list == [0]`).
2. Restart both agents: `pkill -x dnv-agent`; wait for exit;
   `mv agent.log agent.pre-restart.log`; relaunch (§3); `get-dn-size --wait`.
   The kernel-side state (nvmet, dm, the CN's connection) is
   untouched — only the agent process dies. The wait for exit is bounded
   (`pgrep -x dnv-agent`, up to 5 s) and its outcome is **printed, not
   asserted** (`stopped` / `STILL_RUNNING`): it is a diagnostic line for the
   run log, and one worth reading, since an agent that outlived the SIGTERM
   keeps the gRPC port, the relaunched process then exits on the bind, and
   every later step of this case is answered by the old, never-restarted
   agent — which would pass the reconcile assertions while proving nothing.
3. **Data-plane continuity**: while the agents are down and again after
   restart, the CN's read of its 4 MiB test data still succeeds (the data
   path does not depend on the agent process).
4. Reconcile assertions (state reloaded from `--local-store` protobuf files
   `dn-*`, `side-*`, `migr-bm-*` — plus, for extent placement, the on-disk
   volume table, which is the authority [D13]):
   - `get-dn-info`/`get-side-info` on both DNs deep-equal the step 1
     snapshots (jq-normalized; `ResInfo.epoch` fields excluded if they
     differ). `zeroed_ext_cnt`/`total_ext_cnt` are part of the compared
     snapshot and must be equal to each other and unchanged across the
     restart — the cheapest proof that `zeroed_bits` survived in the on-disk
     volume table.
   - `bm_info` re-fetch still `[0]` — chunk files survived.
5. **Idempotency (mutation-free re-apply)**: re-send the *same-revision*
   `syncup-dn` and `syncup-side` to both DNs; assert code 0. Then assert the
   post-restart log (`agent.log`, which covers reconcile + these re-applies)
   contains **no mutating operations**: the `mutations()` helper lists
   `dmsetup create|reload|remove|suspend|resume|message`,
   `nvme connect|disconnect`, `blkdiscard`, the configfs object commands
   `mkdir`/`rmdir`/`ln`/`rm`, every `os write file direct` record and every
   **`os write block`** record, and the assertion requires zero of them.
   Probe operations (`lsblk`, `dmsetup info/table/status`, `ls`, and
   `os read block`) are expected and deliberately not in the list.
   The `os write block` clause is what additionally proves the [D13]
   metadata re-load allocates nothing and rewrites no slot on a converged
   disk. Both sides are fully zeroed before the restart, so the
   `update_01.md` U4 zeroing registry finds nothing to do on reconcile and
   starts no goroutine: zero `blkdiscard` records is therefore the
   *expected* outcome, and the `os write block` clause now additionally
   proves that no spurious `zeroed_bits` slot rewrite happens either.
6. **Revision persistence probe**: `syncup-dn` DN1 with `revision REV1-1`
   and `--expect-code 1` (`ReplyCodeStaleRevision`, a normal reply, not a
   gRPC error). This is the one intentional "negative" call in the suite:
   equal-revision acceptance alone cannot distinguish a persisted revision
   from a wiped store, so only the stale *rejection* proves the revision
   survived the restart.
7. Teardown as usual (disconnect, empty side lists, residue checks).

## 16. Teardown and cleanup (`cleanup()`, also `--cleanup-only`)

Best-effort (`|| true` throughout), per VM, in this order — the order is
load-bearing:

1. `pkill -x dnv-agent` (also kills the 5 s migration-connect retry loops
   and any `update_01.md` U4 side-zeroing goroutine; the agent's `Serve`
   waits for those after `GracefulStop`, so no orphan `blkdiscard` child
   outlives it — but a run killed mid-batch can still leave the side device
   briefly busy, which the step 4 retry sweep already absorbs).
2. Host-side `nvme disconnect` of every `nqn.2024-01.io.dnv:2:*` connection
   (CN roles) — nvmet controllers must die before nvmet teardown.
3. nvmet configfs teardown of the side exports, inside-out and scoped to the
   `:2:` subsystems: unlink them from `ports/1/subsystems/`; per subsystem
   `echo 0 > namespaces/1/enable`, `rmdir namespaces/1`, unlink
   `allowed_hosts/*`, `rmdir` the subsystem. (Namespaces must be disabled
   before their backing dm devices can go; a port with live host connections
   wedges — hence step 2 first.) The migration-source (`:3:`) subsystems are
   deliberately left up here and come down in step 4, after the dm-clones.
   Note that the `:3:` export a node owns is *not* the source of that node's
   own clone: the `:3:` NQN carries the *source* DN's id (§5), so a
   destination side connects to the peer's export, not its own
   (dnagent.md §4.6). In cases B/C each DN is source for one leg and
   destination for the other, so the clone a node removes in its own step 4
   flushes over the connection to the *other* node's `:3:` export. Deferring
   this node's `:3:` teardown past its own clones therefore protects the
   peer's clone, and it does so only because `cleanup_all` runs the two VMs
   concurrently — which is also why that function keeps the window in which
   one node's nvmet teardown can kill the other's migration-source
   connection as short as it can. The port-level objects follow:
   `rmdir hosts/nqn.2024-01.io.dnv:*` (prefix-scoped like the subsystem
   loops above, so a foreign host directory is left alone),
   `rmdir ports/1/ana_groups/{2,3}` and `rmdir ports/1` run at the end of
   step 4.
4. dm removal, top-down with a retry sweep: pass 1 removes `dnv-*-1-*`
   (per-CN linears) then `dnv-*-3-*` (clones — **while the migration nvme
   connection is still up**: a clone flushes to its source on remove and
   blocks otherwise); then `nvme disconnect` every `nqn...:3:*` this node is
   a *host* of (its controllers to the peer's migration sources) and drop
   this node's *own* `nqn...:3:*` subsystems the same inside-out way as
   step 3 — two disjoint sets on any one VM, since the NQN carries the dn id
   of the node exporting it; pass 2 removes `dnv-*-5-*` (the clone-metadata
   wrappers the clones sat on), `dnv-*-2-*`, `dnv-*-0-*` and finally
   `dnv-*-4-*` (the side devices everything above was stacked on), then
   retries anything still busy.
5. Unformat each loop device: `dd if=/dev/zero of=<loop> bs=4096 count=1
   conv=fsync` (no `oflag=`, per the §4 dd rule) plus `wipefs -a <loop>`.
   Zeroing the 4 KiB header block is enough: a volume-table slot only counts
   when its `format_uuid` matches the header's, so the slots are inert
   without it ([D13]). There is no LVM step — the DN has no VGs.
6. `losetup -j $WORK/backing.img` → `losetup -d` each; `rm -rf $WORK`
   (removes store dir, logs, binary, backing file). The §3 helper is outside
   `$WORK` and stays on the VMs — it is the script running this cleanup, and
   every run re-ships it anyway.

The script additionally resumes every suspended `dnv-*` dm device before
step 2 and again before the loop devices are unformatted (`resume_suspended`).
This is load-bearing, not defensive: a migration source holds its per-CN
dm-linears suspended for the `SuspendSeconds` = 60 s grace window of §11.2
([D12]), so a run killed mid-cutover leaves them that way with no agent left
to retire them. A suspended device wedges `dmsetup remove`, the nvmet
namespace disable above it, and above all any block-device scan, in
uninterruptible D state.

End-of-run cleanup (success only) is the same function; start-of-run cleanup
runs it unconditionally before setup.

## 17. Failure diagnostics

On any assertion/command failure, before exiting: dump to the driver console
— last 120 lines of both `agent.log`s, `dmsetup ls`, `dmsetup table`,
`ls -R /sys/kernel/config/nvmet/{ports,subsystems}`,
`nvme list-subsys -o json`, and (migration cases) the last `get-side-info`
of every involved side. Debris is left in place (§2). The failing stage name
and the `trace_id`s it used are printed so records can be pulled from the
JSON logs on either VM.

## 18. RPC coverage matrix

| RPC | exercised by | asserted |
|---|---|---|
| `GetDnSize` | setup wait-up | exact data-area size 1879048192; liveness |
| `SyncupDn` | every case + setup | reply code, dn_info statuses, declarative side add/remove, stale probe (D) |
| `SyncupSide` | S, A, B, C, D | side_info statuses, per-CN maps, migr confs, gated→enabled transition, equal-rev idempotency; the two-phase `provisioned` gate (§9) and the `dst_provisioned = false` equivalence probe (§12 step 8b) |
| `PushMigrBitmap` | C, D | reply code 0; effects via §14 layers |
| `GetDnInfo` | teardown checks, D | statuses, snapshot equality |
| `GetSideInfo` | every case (the §9 two-phase helper samples it and `wait-zeroed` polls it on each first side converge), B/C hydration polling, D | dm_clone status parsing, snapshot equality; `zeroed_ext_cnt`/`total_ext_cnt` (polled by `wait-zeroed`) |
| `CheckDn` | S, A, B/C teardown | stream round: code 0, revision echo, show_info statuses |
| `CheckSide` | S, A | same (case D proves steady state by the §15 snapshot diff instead of check rounds) |

## 19. Out of scope (v1)

Negative/error-path testing (bad configs, unknown objects, `code 2` paths)
beyond the case D stale probe; CN-role integration (`dnv-agent cn` is
unimplemented); fault injection (dm-flakey/dm-delay, target crashes mid
hydration); performance/soak; TLS/auth (none exists in the agent);
`SP_LEVEL` values other than `READWRITE`/`NO_MIGRATION` (still true — and
since [D11] the levels between them have no DN-side behavior at all, so there
would be nothing to observe);
`CancelMigration`-style flows (dropping a migration before completion). The
§11.2 grace window is observed but not asserted (§12 step 9): pinning it would
make the suite timing-dependent, and its end state is already proved by the
step 19 residue checks. Also out of scope: the `update_01.md` U4
side-provisioning error rows (`provisioned = true` with incomplete bits ⇒
`ERROR` and no export; a missing allocation record at `provisioned = true` ⇒
`ERROR` and **no** allocation write) and the DN5 Write-Zeroes fail-fast
(sysfs `write_zeroes_max_bytes == 0`) — all negative paths, unit-tested per
`dnagent.md` §6; likewise cancel-and-wait of a zeroing goroutine mid-batch,
which needs the `CancelMigration`-style flows this suite does not run.
Listed here so their later addition extends this file rather than reshaping
it.

## 20. Amendments

Recorded for traceability (`update_01.md` "Conventions"); the edits are
already applied above. Appended, never inserted, so no section number
another document or the harness cites can shift.

- **U1-T4 (`update_01.md` U1)** — every dnv dm-clone table is now
  `… 2 no_hydration no_discard_passdown …`, the dn migration clone
  (`DnMigrFinalName`) included, so a §11.4 skip `blkdiscard` stays
  metadata-only and can never destroy an acknowledged post-cutover write on
  the destination device. This suite had no dm-clone **table** assertion at
  all, so §12 step 11 gained a **mandatory** one — helper `clone_table`
  (`dmsetup table` of `dnv-*-3-{sp}-{M}` on DNdst) matched against
  `' 2 no_hydration no_discard_passdown( |$)'`, the exact pair, not a
  substring. The pinned form is safe because `dmsetup table`
  (`STATUSTYPE_TABLE`) reprints dm-clone's saved constructor args:
  `dmsetup message … enable_hydration` only flips a live flag that
  `dmsetup status` reports, and the agent never reloads the clone on feature
  drift. It is also the only on-hardware coverage U1 can get here —
  `SideInfo.migr_dst_info.dm_clone_info.details` (the raw `dmsetup status`
  line, parsed with `agent.ParseCloneStatus`; §8 `wait-hydrated`, §12
  steps 13-14, §14 layer 3) shows hydration progress and would not move at
  all if `no_discard_passdown` were dropped. The cn suite carries the twin
  assertion (`cnagent_integtest.md` §13 stage 4, §20 U1-T4).
- **U4-T6 (`update_01.md` U4, new decision [D15])** — the §9.4 trim protocol
  is replaced by whole-side zeroing behind a `provisioned` gate. §4's
  punch-hole rationale and §6's sizing rationale now describe
  `blkdiscard --zeroout` in `DnZeroBatchExtCnt = 10` extent batches paced by
  `DnZeroRetryInterval = 5` seconds; §4 and §7 step 3 add the DN5
  Write-Zeroes fail-fast check
  (`/sys/class/block/<loop>/queue/write_zeroes_max_bytes != 0`, run inside setup
  because the per-VM preflight block precedes the `losetup`); §8 gives
  `dnagentctl` a `--provisioned` flag on `syncup-side`, a
  `--dst-provisioned` flag alongside `--migr-src`, a `zeroed/total` **stderr**
  line on `get-side-info` (stdout stays one protojson line, which case D
  deep-equals) and a new `wait-zeroed` subcommand; §9 gains the two-phase
  side-setup convention that §10/§11/§12/§15 now follow, plus the rule that
  a "must be ERROR" assertion compares against `RES_STATUS_ERROR` explicitly
  and phase-(a) checks use `assert_provisioning_or_ok`, because
  `RES_STATUS_PROVISIONING` would otherwise slip through a bare "not OK";
  §12 gains step 8b, the normative `dst_provisioned = false` ⇒ "behave
  exactly as if `migr_src_conf` were absent" equivalence probe, without
  which the src would fence the primary's path for the entire dst zeroing
  window, and its step 6a states the dst clone row as `assert_gated`
  (ABSENT/`MISSING`) rather than `PROVISIONING`: that step's request is at
  `sp_level no_migration`, which makes `wantMigr` false so `migr_dst_info`
  is not emitted at all — U4's `migr_dst_info.*` `PROVISIONING` row belongs
  to levels *below* `SP_LEVEL_NO_MIGRATION` with `provisioned = false`,
  which this suite never sends; §13's case-B `blkdiscard` check and §14's
  layer-1 and layer-4 notes are re-scoped by device because provisioning now
  emits its own `blkdiscard` records against `dnv-*-4-*`; §15's mutation-free re-apply
  gains its precondition (all sides zeroed before the restart, so the
  zeroing registry starts nothing) and its snapshot now includes
  `zeroed_ext_cnt`/`total_ext_cnt`; §16 step 1 notes the goroutine's
  cancel-and-wait; §19 records the negative rows as unit-test-only.
  Rationale: multi-tenancy — discard-reads-zeros is not a hardware guarantee
  (the kernel dropped `discard_zeroes_data` in 4.12; NVMe DLFEAT
  read-zeroes is optional) — plus the correctness sites that already
  *assumed* zeros (fresh thin-pool metadata, `--assume-clean`, stale md
  superblocks).

## Appendix A — lab gotchas baked into this plan

- **uutils dd 0.8.0**: `iflag=/oflag=direct` silently broken → the §4 rule.
- **nvmet teardown ordering**: disconnect hosts before dismantling
  ports/subsystems; disable namespaces before removing backing dm devices;
  remove dm-clones while their source connection is alive (§16).
- **ANA `inaccessible` namespaces have no `/dev` node** and IO against a
  namespace with no serving path hangs (queued) → the stage 2→3 CN IO
  quiesce window in §12.
- **Port/subsystem unlink kills the host controller with DNR** — the host
  will not reconnect on its own → explicit CN disconnect of the dead src
  path in §12 step 17.
- **nvmet cannot export a read-only block device**: it opens a namespace's
  backing device `BLK_OPEN_READ | BLK_OPEN_WRITE`, so `echo 1 > enable`
  returns `EACCES` on a dm device created `--readonly`. Together with the
  measured fact that a read-only flag below the top of a stack does not stop
  dm-remapped writes, dnv therefore has no bdev-level read-only anywhere;
  `SP_LEVEL_READONLY` is enforced only at the CN's user-facing namespaces
  ([D11]), and the DN agent never flips any device or LV read-only.
- **`--local-store` must pre-exist** (agent startup reconcile fails
  otherwise) and defaults to shared `/var/tmp` — always pass the dedicated
  `$WORK/store`.
- **`attr_serial` reads back space-padded** from configfs — irrelevant to
  current assertions; relevant if serial-based checks are added.
- **udev + dm**: this plan uses only error/linear/clone targets against live
  backends, so the historical dm-delay udev-worker hang does not apply; if
  udev stalls appear anyway, revisit the 58-* udev rule from the earlier
  experiments before blaming the agent.
- **`blkdiscard --zeroout` on a loop device is `fallocate`, not IO**: the
  `update_01.md` U4 provisioning of a 128 MiB side finishes in microseconds
  here, so the `PROVISIONING` window is easy to miss — sample it tolerantly
  (`provisioning window HIT` / `window missed`) rather than asserting it, in
  the same style as the §12 grace-window and read-through probes. It also
  means this suite exercises U4's *protocol* but not its *timing*; the
  `write_zeroes_max_bytes` preflight is what stands in for real hardware.

### Integration-run fixes (first on-hardware run)

- **IR-T1 (`--hostid`)** — §3 gains the rule that every emulated `nvme connect`
  passes `--hostid`, §8 gains the local `dnagentctl host-id --hostnqn` that
  prints it, and §10/§11/§12's connects use it. Each VM plays two CN identities
  on top of its own dn agent's `DnHostNqn`, and the kernel allows exactly one
  hostnqn per hostid, so leaving the node-wide `/etc/nvme/hostid` implicit made
  the second identity's connect fail `EINVAL`.
- No other change was needed here: the four remaining findings of this run
  (`dnagent.md` IR1-IR5) were agent defects that this suite detected exactly as
  designed — the stage-3 target assertion caught the connect flag and the sysfs
  gap, the case-B teardown residue check caught the dm-clone ordering leak, and
  the case-D `mutations` assertion caught the namespace-identity rewrite.
