# t04 — Distributed Block Storage POC, Design Notes

## 1. What this is

A six-VM testbed that builds a **disk array** (`da`) out of commodity parts —
loop files, dm-raid1, dm-thin-pool, dm-raid0, NVMe-oF/TCP — and then moves it
from one controller node to another without the host losing a single I/O.

The scripts in this directory (`common.sh`, `setup.sh`, `teardown.sh`,
`failover.sh`, `grow.sh`, `host0_io.sh`) are meant to be read alongside this
note: they are the source of truth, this document is the map.

---

## 2. Glossary

Every term below has exactly one meaning in this codebase.  When a script,
comment, or log line uses one of these words, it refers to the definition here.

### 2.1 Node roles

| term | meaning |
| --- | --- |
| **dn** | **Disk node.** A Linux server that holds physical disks (loop files) and exports virtual-disk slices over NVMe-oF/TCP to the controller nodes. There are two: `dn0`, `dn1`. |
| **cn** | **Controller node.** A Linux server that imports virtual disks from the DNs, assembles them into a raid1 + thin-pool + raid0 stack, and re-exports the result to the host. There are two: `cn0` (active), `cn1` (standby). A `cn` hosts one **cntlr** (the virtual controller of a `da`) — see §2.3. |
| **ref0** | **Referral server.** A bare NVMe-oF discovery service (no storage). Its port carries one *referral* per CN so the host learns the CN addresses from ref0 alone. |
| **host0** | **The host.** The NVMe initiator. Runs `nvme-stas` (stafd + stacd), never runs `nvme connect`. Discovers CNs via ref0's referrals and aggregates both paths into a single multipath namespace. |

### 2.2 Storage objects (bottom-up)

The stack is built in layers.  Each layer is a device-mapper target or an
nvmet export.  The terms below name the layers, from raw disk to exported
volume.

| term | full name | meaning | t03 equivalent |
| --- | --- | --- | --- |
| **pd** | physical disk | A 4 GB loop file on a DN. Backing store for everything. One per DN. | loop file |
| **vd** | virtual disk | A 500 MB slice carved from the pd, plus a symmetric fault-injection pair (`-real` / `-err` / `-delay`) per CN, exported to one CN via NVMe-oF. The `-real` slice is an LVM LV in `dnv-<dn>-da0-vg` (the loop is the PV); `-err`/`-delay`/`-cn` are plain dmsetup devices stacked on it. Identified by `vd_id` (0 = from dn0, 1 = from dn1). | `ld0` / `ld1` |
| **grp** | group | One raid1 mirror (two VDs, one from each DN) plus the thin-pool metadata/data slices carved from it. A group is the unit of mirroring; there are two per leg (`grp0`, `grp1`). | `grp0` / `grp1` |
| **leg** | leg | The raid0 underlying disk. One leg = the concatenation of both groups' thin-pool slices into one thin-pool, plus the default thin device (snap 0). There are two legs per da (`leg0`, `leg1`). | `stripe0` / `stripe1` |
| **side** | side | One half of a raid1 mirror. A group's raid1 has two sides: `side0` (VD from dn0) and `side1` (VD from dn1). "Side" replaces the ambiguous "leg" used in t03 raid1 comments. | `ld0` / `ld1` (raid1 "leg") |
| **da** | disk array | The container: N legs × M groups × thin-pool + snaps + one raid0 export. `da0` is the only da in this POC. A da is what a host sees as a single NVMe namespace. | `vol0` |
| **snap** | snapshot | A thin-device id inside a da, shared across all legs. `snap 0` is the live writable origin (created by `create_thin 0`). Higher ids are derived snapshots (created by `create_snap <new> <src>`). | `thin 23` |
| **exp** | exporter | The top-level device exported to the host: a raid0 of the per-leg snap thin-devs, plus fault-injection (`-real` / `-error` / `-delay`) and an nvmet subsystem. `exp0` is the only exp. | `${vp}` exported volume |

### 2.3 Other recurring terms

| term | meaning |
| --- | --- |
| **cntlr** | **Controller** (virtual). The controller of a `da` — the abstraction that owns the raid1 + thin-pool + raid0 stack and the nvmet export. A cntlr runs on a `cn`: a `da` has exactly one **active cntlr** (the one serving host I/O, ANA `optimized`, exp table → `-real`) and may have one or more **standby cntlrs** (ANA `inaccessible`, exp table → `-delay`), each on its own `cn`. `create_cntlr_active`/`create_cntlr_standby` build the active/standby side of a cntlr on a `cn`. |
| **active** | The cntlr (and the `cn` it runs on) currently serving host I/O (ANA `optimized`, exp table → `-real`). |
| **standby** | A cntlr ready to take over but not yet serving (ANA `inaccessible`, exp table → `-delay`). Lives on its own `cn`. |
| **disarm** | Temporarily swap a DN's `-delay-cn` device to an error table so the kernel's partition scan fails fast instead of hanging on the 3600 s delay. |
| **arm** | Put the real delay table back after the scan is done. |
| **defuse** | Suspend a dm-delay device and load an error table so parked bios fail fast. Used in teardown phase 0. |
| **referral** | A discovery-log entry of subtype *discovery subsystem referral* — "go ask that other discovery service". |

---

## 3. Architecture

```
                    host0 (initiator)
                     |
                     | NVMe-oF/TCP (multipath, ANA)
                     |
              +------+------+
              |             |
            cn0 (active)  cn1 (standby)
              |             |
              | NVMe-oF/TCP (per-VD NQN, hostnqn-scoped)
              |             |
         +----+----+   +----+----+
         |         |   |         |
        dn0       dn1  dn0      dn1
         |         |   |         |
       [pd0]    [pd1] [pd0]   [pd1]
      (loop)   (loop) (loop)  (loop)
```

ref0 sits to the side and hands out the CN addresses:

```
  host0 ---discover---> ref0
                         |
          referral: "cn0 is at 192.168.122.125:4420"
          referral: "cn1 is at 192.168.122.229:4420"
```

host0 talks to ref0 only; ref0 tells it where the CNs are; the CNs serve
storage.

---

## 4. The storage stack (one CN, active)

This is what `create_cntlr_active` + `create_exp_active` build on cn0.  Every
line is a device-mapper target or an nvmet export.

```
                                   host0
                                     |
                          exp NQN:  nqn...:da:da0:snap0:exp0
                                     |
                            +-- exp0 (dm-linear, 2 GB) --+        ← exported to host
                            |                             |
                         -real (dm-raid0, 2 GB)      -delay / -error
                            |                        (fault injection,
                            |                         standby path)
                   +--------+--------+
                   |                 |
             leg0-snap0          leg1-snap0          (dm-thin, 1 GB each)
                   |                 |
             leg0-thinpool      leg1-thinpool        (dm-thin-pool, 976 MB)
                   |                 |
          +--------+--------+  +-----+-----+
          |                 |  |           |
     grp0-thinmeta    grp0-thindata  grp1-...  (dm-linear slices)
          |                 |
     grp0-raid1       grp0-raid1  (dm-raid1, 496 MB)
          |                 |
     raid1-meta-side0  raid1-data-side0  (dm-linear on vd0)
     raid1-meta-side1  raid1-data-side1  (dm-linear on vd1)
          |                 |
         vd0               vd1   (NVMe namespaces imported from dn0, dn1)
```

**Reading the diagram bottom-up:**

1. **pd** (loop file on each DN, 4 GB) → carved into four 500 MB slices
2. **vd** (500 MB slice + fault-injection pair) → exported via NVMe-oF to a CN
3. **grp** (two VDs → raid1 → thinmeta/thindata slices) → the mirror unit
4. **leg** (two grps → concat → thin-pool → snap0 thin device) → one stripe of the raid0
5. **exp** (two legs → raid0 → dm-linear → nvmet export) → what the host sees

**Key sizes** (512 B sectors):

| object | size | note |
| --- | --- | --- |
| pd | 4 GB | loop file |
| vd | 500 MB = 1 024 000 | one slice per (leg, grp) |
| raid1 metadata | 4 MB = 8 192 | per side |
| raid1 data | 496 MB = 1 015 808 | per side |
| thin-pool metadata | 8 MB = 16 384 | per grp |
| thin-pool data | 488 MB = 999 424 | per grp |
| thin-pool (concat) | 976 MB = 1 998 848 | per leg (2 grps) |
| snap0 (thin device) | 1 GB = 2 097 152 | per leg |
| exp (raid0) | 2 GB = 4 194 304 | 2 legs × 1 GB |

The 2 GB raid0 is thin-provisioned over 2 × 976 MB ≈ 1.9 GB, so writes must
stay under ~1.9 GB. `host0_io.sh` uses the first 64 MB (rotating slots) plus
a 1 MB anchor at 512 MB.

---

## 5. Fault injection — why the standby path is invisible

Each VD on a DN has a symmetric fault-injection pair for both CNs:

```
  -real  → LVM LV in dnv-<dn>-da0-vg (loop is the PV; the actual data)
  -err-cn0 / -err-cn1  → dm-error (instant EIO)
  -delay-cn0 / -delay-cn1  → dm-delay(3600 s) on -err (read/write hangs)
  -cn0  → dm-linear on -real (cn0 sees real data)
  -cn1  → dm-linear on -delay-cn1 (cn1 sees the 3600 s delay)
```

Only one CN at a time can reach the real data. The other gets the 3600 s delay,
which means any read hangs. This is deliberate: the standby CN must not touch
the data until failover swaps the `-cn0` / `-cn1` tables on the DN.

---

## 6. Resource naming

All dm devices and NQNs follow a strict convention so that `common.sh`'s
`create_*` functions and `failover.sh`'s inline dmsetup commands produce
identical names.

### 6.1 DN-side dm devices

```
dnv-<dn>-da0-leg<leg>-grp<grp>-vd<vd>          (base, no -cn suffix)
    + -real                                     LVM LV in dnv-<dn>-da0-vg (PV = loop)
    + -err-cn0, -err-cn1                        dm-error
    + -delay-cn0, -delay-cn1                    dm-delay on -err
    + -cn0, -cn1                                dm-linear (exported to each CN)
```

Example: `dnv-dn0-da0-leg0-grp0-vd0-cn0` is the device dn0 exports to cn0,
for leg 0, grp 0, vd 0 (from dn0).

### 6.2 CN-side dm devices

```
dnv-<cn>-da0-leg<leg>-grp<grp>-raid1-side<side>
    + -meta-side<side>, -data-side<side>        dm-linear slices of the VD

dnv-<cn>-da0-leg<leg>-grp<grp>-thinmeta, -thindata   dm-linear on raid1

dnv-<cn>-da0-leg<leg>-thinmeta, -thindata      concat of grp0 + grp1
dnv-<cn>-da0-leg<leg>-thinpool                  dm-thin-pool
dnv-<cn>-da0-leg<leg>-snap0                     dm-thin (thin device 0)

dnv-<cn>-da0-snap0-exp0                         dm-linear (the export)
    + -real                                     dm-raid0 (2 legs)
    + -error, -delay                            fault injection
```

### 6.3 NQNs

| NQN | used by |
| --- | --- |
| `nqn.2026-07.org.dnv:dn:<dn>:da0-leg<leg>-grp<grp>-vd<vd>-cn<cn>` | DN → CN VD export |
| `nqn.2026-07.org.dnv:da:da0:snap0:exp0` | CN → host exp export |
| `nqn.2026-07.org.dnv:ref:ref0-anchor` | ref0 anchor subsystem |
| `nqn.2026-07.org.dnv:host:cn0` / `:cn1` | CN host identity (for VD connect) |
| `nqn.2026-07.org.dnv:host:host0` | host0 identity (for exp connect) |

---

## 7. The `common.sh` API

`common.sh` is sourced once by `setup.sh`, `teardown.sh`, and `failover.sh`.
It has two parts:

### Part A — Resource API (high-level, SSH internally)

Each function takes a node name and IP as its first arguments, SSHes to that
node, and runs the Part B helpers there. The orchestrator reads like a recipe:

```bash
source ./common.sh

create_pd    dn0 192.168.122.48          # loop file on dn0
create_pd    dn1 192.168.122.70          # loop file on dn1
create_vd    dn0 ... cn0 ... 0 0 0       # dn0's vd0 exported to cn0, leg0 grp0
create_vd    dn0 ... cn1 ... 0 0 0       # dn0's vd0 exported to cn1, leg0 grp0
create_vd    dn1 ... cn0 ... 1 0 0       # dn1's vd1 exported to cn0, leg0 grp0
create_vd    dn1 ... cn1 ... 1 0 0       # dn1's vd1 exported to cn1, leg0 grp0
create_cntlr_active  cn0 ... da0 2 <hnqn> <hid>   # build the stack on cn0
create_exp_active    cn0 ... da0 0 <hnqn> <hid> 1 255   # export to host
# ... disarm, connect standby, arm, export standby ...
```

`create_vd`/`delete_vd`/`connect_vd`/`disconnect_vd`/`disarm_vd_delay`/
`arm_vd_delay` take `leg` and `grp` as explicit trailing params (the base
`setup.sh` loops the 2×2 grid; `grow.sh` passes them directly to add `grp2`
to a running pool without touching the existing slices).

Full function list:

| function | what it creates / removes |
| --- | --- |
| `create_pd` / `delete_pd` | loop file + nvmet port on a DN |
| `create_vd` / `delete_vd` | VD stack (-real [LVM LV], -err, -delay, -cn [dmsetup]) + nvmet subsys for both CNs, per (leg,grp) |
| `connect_vd` / `disconnect_vd` | nvme connect/disconnect from a CN to a DN's VD, per (leg,grp) |
| `create_grp` / `delete_grp` | raid1 + thinmeta/thindata slices (connects VDs internally) |
| `create_leg` / `delete_leg` | concat → thin-pool → snap0 thin device |
| `create_cntlr_active` / `delete_cntlr_active` | orchestrates create_grp + create_leg for N legs |
| `create_cntlr_standby` / `delete_cntlr_standby` | connects VDs only (no stack) |
| `create_snap` / `delete_snap` | create_thin (id 0) or create_snap (derived) across all legs |
| `create_exp_active` / `delete_exp_active` | raid0 + real + error + delay + exp + nvmet export (ANA optimized) |
| `create_exp_standby` / `delete_exp_standby` | error + delay + exp stub + nvmet export (ANA inaccessible) |
| `disarm_vd_delay` / `arm_vd_delay` | DN-side delay disarm/arm for CN namespace scan, per (leg,grp) |

### Part B — Infrastructure helpers (low-level, run ON the remote node)

These are uploaded verbatim to each node via a quoted heredoc (`_emit_common`)
and called by the Part A functions. They include: `prep_node`, `set_host_identity`,
`dm_create`, `dm_reload`, `dm_remove`, `dm_defuse_delay`, `dnv_remove_all_dm`,
`cfg_set`, `nvmet_add_subsys`, `nvmet_port`, `nvmet_link`, `nvmet_referral`,
`nvmet_remove_*`, `nvmet_remove_all_dnv_subsys`, `nvme_dev_by_nqn`, `nvme_wait_dev`,
`nvme_conn`, `nvme_disc`, `nvme_disconnect_all_dnv_vd`, `ana_set`, `stas_install`,
`stas_write_config`, `stas_start`, `stas_stop_restore`, `rd_ok`, `rd_eio`, `rd_hang`,
`verify_summary`, `_t`, `slow_summary`.

`nvmet_remove_all_dnv_subsys` and `nvme_disconnect_all_dnv_vd` are
topology-agnostic teardown safety nets: they scan `nvmet/subsystems/` and
`/sys/class/nvme/*/subsysnqn` for `:dn:*` NQNs (the VD subsystem/connections)
and remove/disconnect each, so `teardown.sh` cleans up groups beyond the
hardcoded grp0/grp1 base (e.g. grp2 added by `grow.sh`) without the loops
needing to know the topology.

---

## 8. The failover (cn0 → cn1)

The failover moves the entire stack from cn0 to cn1 without losing in-flight
I/O. Seven steps:

| step | node | what happens |
| --- | --- | --- |
| 1 | cn1 | Suspend exp (noflush — it sits on a 3600 s delay). ANA → `optimized`. Host I/O now queues inside the suspended dm device. |
| 2 | ref0 | Remove the cn0 referral. host0's stafd sees the discovery-log change, drops cn0's discovery controller, stacd disconnects host0 from cn0. **Runs in `force` mode too** — it touches ref0, not cn0. |
| 3 | cn0 | ANA → `inaccessible`. Flushing suspend (in-flight I/O drains to the thin pool). Repoint exp → delay early (frees the raid0). Dismantle the entire stack in reverse order. Removing the thin-pool **commits its metadata** — this is what lets cn1 inherit it. |
| 4 | dn0 | Swap `-cn0` → delay, `-cn1` → real. cn0 loses data access; cn1 gains it. |
| 5 | dn1 | Same swap. |
| 6 | cn1 | `create_cntlr_active` rebuilds the stack (raid1 + thin-pool + snap0). The thin-pool metadata is cn0's committed copy — `create_thin 0` fails (inherited), which is the **snap-0 inheritance invariant**. Build raid0, repoint exp → real, resume. Queued host I/O drains. |
| 7 | cn0 | Repoint exp → delay (park it). |

**`force` mode** skips steps 3 and 7 — models cn0 dying outright. The
metadata is whatever cn0 last committed to disk; cn1 comes up on that.
Everything else (1, 2, 4, 5, 6) runs unchanged.

### Why the snap-0 inheritance matters

The thin-pool metadata lives on the raid1's thinmeta slice, which is on the
VD, which is on the DN's `-real` device (an LVM LV in the loop-backed VG
`dnv-<dn>-da0-vg`). When cn0 removes its thin-pool (step 3), the pool's
destructor flushes the metadata to that slice. When cn1 creates its
thin-pool (step 6) on the *same* slice (now pointing at `-real` after the DN
swap), it reads cn0's committed metadata. The mapping for thin device 0 —
and therefore all the data the host wrote — is preserved.

If `create_thin 0` *succeeds* on cn1, it means the metadata was NOT inherited
(empty pool) and all pre-existing data is lost. The scripts log this as a
warning. In testing, `create_thin 0` always fails (inherited), confirming
data continuity.

---

## 9. The referral chain (how host0 connects without `nvme connect`)

host0 is told one address: ref0's. Everything else it learns.

1. **ref0** runs nvmet with one port. The port carries two *referrals* (one
   per CN) and one *anchor* subsystem (no namespace, no allowed hosts — it
   exists only to make the port's TCP listener come up).
2. **stafd** on host0 is configured with exactly one discovery controller
   (ref0) and `zeroconf=disabled`. It connects to ref0, reads the discovery
   log, follows both referrals, and ends up with discovery connections to
   cn0 and cn1.
3. **stacd** sees the CN discovery services advertising `nqn...:da:da0:snap0:exp0`
   and connects both. Same NQN + same namespace identity → the kernel folds
   them into one multipath namespace.
4. **ANA**: cn0 advertises `optimized`, cn1 advertises `inaccessible`. The
   multipath layer steers all I/O to cn0.
5. **Teardown** reverses the chain: remove ref0's referrals → stafd drops
   the CN discovery controllers → stacd disconnects the I/O controllers.
   host0 lets go on its own; no `nvme disconnect` needed.

---

## 10. The disarm/arm dance

The kernel runs `nvme_partition_scan_work` on every new namespace, which
reads sector 0 outside udev's control. If cn1's namespaces are backed by a
3600 s dm-delay at connect time, that read never completes: it wedges an
nvme-wq worker, a udev worker, and — worst of all — leaves an nvmet request
outstanding on the DN, making controller teardown hang.

**Fix**: `setup.sh` disarms the DN delay devices (swap to error table) before
cn1 connects, then re-arms them afterwards. The partition scan fails instantly
(EIO) instead of hanging. The final layout is exactly as specified.

---

## 11. Scripts

| script | usage | what it does |
| --- | --- | --- |
| `setup.sh` | `./setup.sh [all\|dn0\|dn1\|cn0\|cn1\|ref0\|host0\|verify]` | Builds the whole environment. `verify` reads every dm device and checks behavior (linear/raid/thin read OK, error gives EIO, delay blocks). |
| `teardown.sh` | `./teardown.sh [all\|defuse\|unexport\|<node>]` | Removes everything. Phase 0 defuses all delays first. Safe on partial/clean systems. Uses topology-agnostic safety nets (`dnv_remove_all_dm`, `nvmet_remove_all_dnv_subsys`, `nvme_disconnect_all_dnv_vd`) so it cleans up groups beyond the base 2×2 grid (e.g. grp2 added by `grow.sh`). |
| `failover.sh` | `./failover.sh [force]` | Moves the da from cn0 to cn1. `force` skips steps 3 and 7. Mutually exclusive with `grow.sh`. |
| `grow.sh` | `./grow.sh` | Extends leg0's thin pool with a third group (`grp2`) while host I/O runs: creates the grp2 VDs on the DNs, disarms/connects/re-arms so cn1 imports them, `create_grp` builds the raid1+slice pair on cn0, then suspends leg0-thinpool and reloads the thinmeta/thindata concats and the pool table from 2-way to 3-way. Grow and failover are never combined. |
| `host0_io.sh` | `./host0_io.sh start\|stop\|status\|mark <label>\|report` | Continuous verified O_DIRECT I/O on host0. One JSON record per I/O. `mark` drops a timestamped marker. `report` prints a phase-by-phase summary. |
| `check.sh` | `./check.sh` | Thin-pool block-allocation audit. Dumps each leg's thin-pool metadata with `thin_dump`, derives 8 VD bitmaps (one bit per 4 MiB VD block), reads each marked block on cn0 and confirms it is non-zero. Run after `setup.sh all` + a short `host0_io.sh` run. Exit 0 = all allocated blocks verified written. |
| `common.sh` | sourced by the above | Part A resource API + Part B infra helpers. |

---

## 12. Test results

```
setup.sh all        exit 0   all device checks passed, 0 warnings
setup.sh all        exit 0   idempotent re-run, live paths kept
setup.sh verify     exit 0   5/5 node checks passed
failover            exit 0   8.8 s wall, snap-0 inherited from cn0
host0 I/O           83594 I/Os, 83594 ok, 0 failed, 0 mismatches
teardown.sh all     exit 0   all 6 nodes: dm=0 nvmet=0 loop=0
```

| phase | I/Os | ok | fail | succ% | avg | p50 | p95 | max |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| before (11.3 s) | 3385 | 3385 | 0 | 100% | 3.18 ms | 1.54 ms | 4.46 ms | 94.6 ms |
| during (158.7 s) | 77565 | 77565 | 0 | 100% | 1.90 ms | 1.58 ms | 3.03 ms | 7951 ms |
| after (5.3 s) | 2644 | 2644 | 0 | 100% | 1.85 ms | 1.61 ms | 3.22 ms | 9.7 ms |
| **total** | **83594** | **83594** | **0** | **100%** | 1.91 ms | 1.58 ms | 3.08 ms | 7951 ms |

**0 I/O failures.** The anchor at 512 MB (written once pre-failover, never
rewritten) was byte-compared on all 27 864 anchor-reads and always matched.
The 7.95 s max latency is one I/O that absorbed the entire handover window
inside cn1's suspended exp device — well under the 30 s NVMe I/O timeout.

---

## 13. Things worth knowing

* **Idempotency**: every `create_*` / `delete_*` checks existence and skips
  if already correct. `cfg_set` and `nvmet_link` are no-op-on-match.
  Re-running `setup.sh all` is safe and keeps live connections.

* **Thin-pool hand-off**: cn0 removing its thin-pool (step 3) commits the
  metadata. cn1 creates its pool with `skip_block_zeroing` and must **not**
  zero the thinmeta — it holds the inherited mapping. `create_thin 0` is
  expected to fail on cn1.

* **raid1 hand-off**: cn1 *does* zero the 4 MB raid1 metadata areas and
  re-assembles with `nosync`. Both sides were mirrored by cn0 up to the
  handover, so declaring the array in-sync is correct. Only metadata is
  touched; the data area is untouched.

* **udev rule** (`58-dnv-test.rules`): suppresses probing of dm-delay and
  nvme devices backed by delays. Without it, systemd-udevd workers wedge in
  D-state. The rule matches on `DM_NAME=dnv-*` (dm) and `subsysnqn`/`model`
  (nvme).

* **host0 aggregation** needs both CN subsystems to agree on NQN, namespace
  UUID/NGUID, serial, model *and size*, with **disjoint `cntlid` ranges**
  (cn0: 1–255, cn1: 256–511) because both controllers live in one subsystem
  on the host.

* **O_DIRECT**: `host0_io.sh` uses Python (`os.O_DIRECT` + `preadv`/`pwritev`
  on page-aligned `mmap` buffers) rather than `dd`, because uutils coreutils
  0.8.0's `iflag=direct` is broken on these devices.

* **LVM for `-real`**: the DN-side `-real` device (the one backed directly by
  the loop) is the *only* LVM-managed object in the whole stack. One VG per DN
  (`dnv-<dn>-da0-vg`, loop = PV, created in `create_pd`), one LV per
  `(leg,grp,vd)` slice (`leg<leg>-grp<grp>-vd<vd>-real`, created in `create_vd`
  via `lvcreate -L ${SEC_PD}s`). Downstream consumers reference it as
  `/dev/<vg>/<lv>`, never `/dev/mapper/...-real`. Every other dm device in the
  DN stack (`-err-*`, `-delay-*`, `-cn0`, `-cn1`) and all CN-side dm devices
  remain plain `dmsetup`. LV allocation offsets are *not* pinned — correctness
  depends on the LV's name, not its byte offset on the loop, so all invariants
  (data integrity, snap-0 inheritance, failover) hold regardless of where LVM
  places the extents. `dnv_remove_all_dm` skips LVM LVs in its dm passes and
  tears them down in a dedicated `lvremove`/`vgremove`/`pvremove` sweep.
