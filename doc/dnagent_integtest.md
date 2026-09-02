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
  plain `nvme connect --hostnqn <CnHostNqn>` from the VMs themselves,
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
- Agents run as root (LVM/dm/configfs/nvme all need it):
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

Agent launch line (VM1 shown; VM2 identical with its own IP):

```
nohup $WORK/dnv-agent dn \
  --grpc-network tcp --grpc-address <ip1>:29528 \
  --tr-type tcp --adr-fam ipv4 --tr-addr <ip1> --tr-svc-id 4200 \
  --local-store $WORK/store \
  --disk <loopdev> > $WORK/agent.log 2>&1 &
```

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

Preflight (fail fast with a `missing: <what> on <vm>` message; installs
nothing). The driver half runs first; the per-VM half runs **after** the
start-of-run cleanup, since the port check is only meaningful once a crashed
prior run's agents are gone:

- driver: `go`, `ssh`, `scp`, `sha256sum`, and a jq (a system `jq`, else the
  drop-in `gojq` built into the gitignored `integtest/bin/` with the Go
  toolchain — no package install); repo builds (`make build`).
- per VM, via `ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new`:
  - `sudo -n true` succeeds.
  - binaries: `dmsetup`, `nvme`, `losetup`, `blkdiscard`, `lsblk`, `dd`,
    `fallocate`, `sha256sum`, `cmp`, `pkill`, `jq`, `timeout`. No `lvm`: the
    dn agent runs no LVM command ([D13]).
  - `modprobe` (ignore errors, then verify): `nvmet`, `nvmet_tcp`,
    `nvme_tcp`, `nvme_fabrics`, `dm_clone`, `loop`; verify
    `/sys/kernel/config/nvmet` exists (mount configfs if absent) — the agent
    hardcodes that path and neither mounts nor modprobes.
  - `/sys/module/nvme_core/parameters/multipath` == `Y` (case A's standby
    assertions and the migration multipath merge depend on it).
  - `df /var/tmp` ≥ 3 GiB free; `/var/tmp` filesystem supports punch-hole
    (probe: `fallocate -p` on a scratch file) — the side trim (`blkdiscard`)
    and the case C zeros-verify rely on discard reaching the backing file.
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
3 s soft / 5 s kill timeout, and the trim protocol `blkdiscard`s the whole
side device — small sides keep that safely inside the budget.

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
3. `LOOP=$(losetup --find --show $WORK/backing.img)` (script records it)
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
   before any case runs.

The driver binary `integtest/bin/dnagentctl` is built natively on the driver
(`go build ./integtest/dnagentctl`).

## 8. The driver: `dnagentctl`

Plaintext gRPC (`insecure.NewCredentials()`), **no server reflection exists**
— that is why this exists instead of `grpcurl` (which would need
`-proto pb/schema.proto` plus bash-assembled JSON and base64 bitmaps).

Global flags: `--addr <ip:29528>` (required), `--cluster`, `--dn`,
`--trace-id <s>` (sent as gRPC metadata key `trace_id`; the script passes
`it-<case>-<step>` so records correlate across driver and both agent logs),
`--timeout <sec>` (default 10). All id flags accept `0x` hex. Every reply is
printed as protojson on stdout (the script parses with `jq`). Exit non-zero
on gRPC error **or** `agent_reply.code != 0` (except where the script
explicitly expects a code, e.g. the case D stale probe: `--expect-code 1`).

Subcommands:

| cmd | flags beyond globals | notes |
|---|---|---|
| `get-dn-size` | `--wait <sec>` | retry until success within wait |
| `syncup-dn` | `--revision`, `--extent-size`, repeated `--side sp:leg:side` | full desired side list every call (declarative) |
| `syncup-side` | `--revision`, `--sp --leg --side`, `--ext-cnt`, `--cntlid-slot`, `--primary-cn`, repeated `--standby-cn`, `--sp-level readwrite\|no_migration`, optional `--migr-src migr:dstSide:dstDn`, optional `--migr-dst migr:srcSide:srcDn --src-traddr <ip> --src-trsvcid 4200 --block-size --meta-blocks --hydr-threshold --hydr-batch --bm-cnt` | src_nvme_tr_conf is always `tcp/ipv4` |
| `push-migr-bm` | `--revision`, `--sp --leg --side`, `--migr`, `--bm-idx`, `--bitmap-hex` | revision = side's current revision (gates, never advances) |
| `get-dn-info` / `get-side-info` | (`--sp --leg --side`) | |
| `check-dn` / `check-side` | `--revision`, `--show-info` (+ side ptr) | opens the bidi stream, one request/reply round, closes |
| `wait-hydrated` | side ptr, `--interval 0.5`, `--timeout 120`, `--min-first <n>` | polls `GetSideInfo`, parses `migr_dst_info.dm_clone_info.details` with `agent.ParseCloneStatus`; `--min-first` asserts the first sample's hydrated ≥ n; exits when hydrated == total |
| `ns-id` | `--sp --leg` | prints uuid/nguid/serial via `common.DnNsIdentity` for CN device lookup |

## 9. Conventions

- **Revisions**: the script keeps one monotonic counter per DN (`REV1`,
  `REV2`), incremented before every state-changing `syncup-*`. Equal-revision
  re-sends are legal full re-applies and are used deliberately (fetching
  `bm_info`, case D idempotency). Never send a lower value except the case D
  stale probe.
- **Ordering**: a side pointer must appear in `SyncupDn.side_pointer_list`
  before its first `SyncupSide` (else `code 2`).
- **Converge check**: after each case reaches steady state, one `check-dn`
  and one `check-side` round with `--show-info` asserts reply code 0,
  matching revision, and all expected `ResInfo.status == RES_STATUS_OK`.
- **Data IO on CNs**: writes `dd conv=fsync`, reads after
  `sync; echo 3 > drop_caches`; never `iflag=/oflag=` (§4).
- Every ssh command the script runs is echoed with a `[vm1]`/`[vm2]` prefix;
  stages log `=== <case>: <stage>` lines.

## 10. Case S — `smoke`

1. `syncup-dn` DN1 (REV1++) with side `(0xa1, 0x1, 0x11)`; assert code 0.
2. `syncup-side` DN1 side 0x11: `ext_cnt 1`, slot 0, primary CN 0x21, no
   standbys, `sp_level readwrite`. Assert code 0, `side_dev_info` OK,
   `cn_id_to_dm_error/linear/nvmeof[0x21]` all OK.
3. VM2 (as CN 0x21): `nvme connect -t tcp -a <ip1> -s 4200 -n <SideToCnNqn>
   --hostnqn <CnHostNqn(0x21)>`; wait for
   `/dev/disk/by-id/nvme-uuid.<uuid>` (uuid from `dnagentctl ns-id`);
   `nvme list-subsys -o json` shows the path `live optimized`.
4. IO: write 4 MiB from a local urandom file (`dd bs=1M count=4 conv=fsync`),
   `sync`, drop caches, read back, sha256 equal.
5. `check-dn` + `check-side` rounds (§9).
6. Teardown: VM2 `nvme disconnect -n <nqn>`; `syncup-dn` DN1 (REV1++) with
   an **empty side list** → side torn down declaratively. Assert via
   `get-dn-info` (all base infos still OK) and on VM1: no `dnv-*-0xa1-*` dm
   devices at all (the side device is dm kind 4, so the one pattern covers
   it) and no `:2:` subsystem in configfs.

Success: all assertions pass; proves build/ship/launch, gRPC, LVM/dm/nvmet
plumbing, CN connect and the IO path — before the bigger cases run.

## 11. Case A — `sides` (multiple sides → multiple CNs)

Layout (§5): 4 legs; sides 0x11,0x12 on DN1 (primary CN 0x21, standby CN
0x22), sides 0x13,0x14 on DN2 (primary CN 0x22, standby CN 0x21). Each side:
`ext_cnt 1`, slot 0, `sp_level readwrite`. That yields 8 nvmet subsystems
(one per side per CN with `allowed_hosts` = exactly that CN's hostnqn).

1. `syncup-dn` per DN (its two side pointers), then 4 × `syncup-side`.
   Assert every `cn_id_to_*` map entry OK for both CN ids.
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
     that wiring, not a bug.
   - isolation: on each DN, each subsystem's `allowed_hosts/` contains
     exactly one hostnqn (configfs `ls`).
4. `check-dn`/`check-side` rounds on both DNs.
5. Teardown: disconnect all 8; empty `syncup-dn` side lists both DNs; assert
   no `0xb1` dm devices or subsystems remain.

## 12. Cases B and C — migration choreography (shared)

Both cases run **two concurrent migrations in opposite directions**
(DN1→DN2 and DN2→DN1, §5), advanced stage-by-stage in lockstep; within each
stage the two per-migration `dnagentctl` calls run as background jobs joined
with `wait`, so the same agent handles overlapping side converges (locks
exercised) while each DN simultaneously plays src for one leg and dst for
the other.

Per migration (leg L: src side S1 slot 0 on DNsrc, dst side S2 slot 1 on
DNdst, migr M, primary CN C hosted on the *other* VM from DNsrc):

**Stage 0 — src side up, data prep**
1. `syncup-dn` both DNs: DNsrc list += (sp,L,S1), DNdst list += (sp,L,S2).
2. `syncup-side` DNsrc S1: `ext_cnt 2` (128 MiB), slot 0, primary C,
   `sp_level readwrite`, no migr confs. Assert OK.
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
6. `syncup-side` DNdst S2 (REVdst++): `ext_cnt 2`, slot 1, primary C,
   `sp_level no_migration`, `migr_dst_conf{migr_id M, src_side_id S1,
   src_dn_id DNsrc, src_nvme_tr_conf{tcp,ipv4,<ip_src>,"4200"},
   block_size 1048576, meta_blocks 3, dm_clone_conf{1,1}, bm_cnt (C: 2, B: 0)}`.
   Assert: `side_dev_info` OK, per-CN stack OK,
   `migr_dst_info.dm_clone_info` **not** OK (missing — gated), no
   `nvme connect` issued yet.
7. CN VM: connect the dst path (same NQN, `<ip_dst>`). Path appears with ns
   `inaccessible` (no by-id node for it yet — expected); the multipath
   device still serves via src.
8. **Case C only**: push bitmap chunks (§14), then re-send step 6 verbatim
   (equal revision — idempotent) purely to read the reply's
   `bm_info`: assert `res_id == M`, `bm_idx_list == [0,1]`.

**Stage 2 — src cutover** *(CN IO quiesced from here until stage 3 asserts
`optimized`: with src inaccessible and dst not yet live, the namespace
briefly has no serving path and IO would hang; the ANA-inaccessible path
also has no /dev node)*
9. `syncup-side` DNsrc S1 (REVsrc++): same side_conf plus
   `migr_src_conf{migr_id M, dst_side_id S2, dst_dn_id DNdst}`. Assert
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
10. CN VM: poll `nvme list-subsys` — src path ana_state → `inaccessible`.

**Stage 3 — enable dst (the concurrency point: both migrations' enables run
in parallel)**
11. `syncup-side` DNdst S2 (REVdst++): identical to step 6 but
    `sp_level readwrite`. In this one converge the agent: allocates the
    clone-metadata slot (first 8 KiB zeroed before the record is persisted)
    and builds its wrapper device `dnv-*-5-*`, `nvme connect`s to
    `nqn...:3:{cluster}:{DNsrc}:{sp}:{M}` as
    `DnHostNqn(cluster, DNdst)`, creates the dm-clone with hydration off,
    (C:) applies persisted chunks via `blkdiscard`, enables hydration,
    reloads the primary linear onto the clone, moves the ns to `optimized`.
    Assert `migr_dst_info.{target_info,dm_clone_info}` both OK.
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
    (missing/empty) and per-CN infos OK.
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
    assert no case LVs/dm/subsystems remain; `check-dn` rounds.

## 13. Case B — `migr_full` specifics

- Step 6/11 `bm_cnt 0`; **no** `PushMigrBitmap` calls.
- Step 8 replaced by: equal-revision re-send asserting `bm_info.bm_idx_list`
  is **empty**.
- Step 18 data check: sha256 of the whole 128 MiB device ==
  sha256(`pattern-M.bin`) — full copy, including the regions case C will
  skip, including the first 3 MiB meta region.
- Log check on DNdst: **zero** `blkdiscard` records mentioning the dm-clone
  device `dnv-*-3-*` (discard-based skipping must not happen without
  bitmaps; the side trim `blkdiscard` from side creation targets the side
  device (`dnv-*-4-*`) and does not match).

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
   agent skipped, not copied. (Freshly created dst sides read zero: the
   trim protocol discards the side device and loop devices punch holes —
   preflight verified punch-hole support.)
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
   adjust the grep, not the assertion.)

## 15. Case D — `restart` (persistence and idempotent reconcile)

Setup: on DN1, side (0xe1, 0x1, 0x11) exported to CN 0x21 (as in smoke,
with the CN connected and 4 MiB of data written); on DN2, the matching
**gated** migration dst side 0x12 (`sp_level no_migration`,
`migr_dst_conf{migr 0x51, ...}`) with **one** bitmap chunk pushed
(`bm_idx 0`, `--bitmap-hex 00000000000000e0`).

1. Snapshot: `get-dn-info` + `get-side-info` on both DNs; `bm_info` via
   equal-revision `syncup-side` re-send on DN2 (`bm_idx_list == [0]`).
2. Restart both agents: `pkill -x dnv-agent`; wait for exit;
   `mv agent.log agent.pre-restart.log`; relaunch (§3); `get-dn-size --wait`.
   The kernel-side state (nvmet, dm, LVM, the CN's connection) is
   untouched — only the agent process dies.
3. **Data-plane continuity**: while the agents are down and again after
   restart, the CN's read of its 4 MiB test data still succeeds (the data
   path does not depend on the agent process).
4. Reconcile assertions (state reloaded from `--local-store` protobuf files
   `dn-*`, `side-*`, `migr-bm-*` — plus, for extent placement, the on-disk
   volume table, which is the authority [D13]):
   - `get-dn-info`/`get-side-info` on both DNs deep-equal the step 1
     snapshots (jq-normalized; `ResInfo.epoch` fields excluded if they
     differ).
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
   disk.
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

1. `pkill -x dnv-agent` (also kills the 5 s migration-connect retry loops).
2. Host-side `nvme disconnect` of every `nqn.2024-01.io.dnv:2:*` connection
   (CN roles) — nvmet controllers must die before nvmet teardown.
3. nvmet configfs teardown, inside-out: unlink
   `ports/1/subsystems/*`; per subsystem: `echo 0 > namespaces/1/enable`,
   `rmdir namespaces/1`, unlink `allowed_hosts/*`, `rmdir` the subsystem;
   `rmdir hosts/*`; `rmdir ports/1/ana_groups/{2,3}`; `rmdir ports/1`.
   (Namespaces must be disabled before their backing dm devices can go;
   a port with live host connections wedges — hence step 2 first.)
4. dm removal, top-down with a retry sweep: pass 1 removes `dnv-*-1-*`
   (per-CN linears) then `dnv-*-3-*` (clones — **while the migration nvme
   connection is still up**: a clone flushes to its source on remove and
   blocks otherwise); then `nvme disconnect` every `nqn...:3:*`
   (migration connections); pass 2 removes `dnv-*-5-*` (the clone-metadata
   wrappers the clones sat on), `dnv-*-2-*`, `dnv-*-0-*` and finally
   `dnv-*-4-*` (the side devices everything above was stacked on), then
   retries anything still busy.
5. Unformat each loop device: `dd if=/dev/zero of=<loop> bs=4096 count=1
   conv=fsync` (no `oflag=`, per the §4 dd rule) plus `wipefs -a <loop>`.
   Zeroing the 4 KiB header block is enough: a volume-table slot only counts
   when its `format_uuid` matches the header's, so the slots are inert
   without it ([D13]). There is no LVM step — the DN has no VGs.
6. `losetup -j $WORK/backing.img` → `losetup -d` each; `rm -rf $WORK`
   (removes store dir, logs, binary, backing file).

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
| `SyncupSide` | S, A, B, C, D | side_info statuses, per-CN maps, migr confs, gated→enabled transition, equal-rev idempotency |
| `PushMigrBitmap` | C, D | reply code 0; effects via §14 layers |
| `GetDnInfo` | teardown checks, D | statuses, snapshot equality |
| `GetSideInfo` | B/C polling, D | dm_clone status parsing, snapshot equality |
| `CheckDn` | S, A, B/C teardown, D | stream round: code 0, revision echo, show_info statuses |
| `CheckSide` | S, A, D | same |

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
step 19 residue checks.
Listed here so their later addition extends this file rather than reshaping
it.

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
