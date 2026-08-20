# failover1 — two-level ANA failover, dn0 → cn0/cn1 → host0

Everything needed to understand, re-run and interpret this test. Written after a
successful run on 2026-08-18, kernel `7.0.0-29-generic` (Ubuntu 26.04).

---

## 1. What this proves

A three-tier NVMe-oF stack can move the active data path from one connection
node to another **without the host losing a single I/O**. The host only ever
sees a short latency spike, because every step of the cutover is signalled with
ANA state changes rather than by breaking connections.

Measured: **6726 I/Os, 0 failures, one 200 ms blip.**

---

## 2. Fleet

| role  | address                | function |
|-------|------------------------|----------|
| host0 | yupeng@192.168.122.193 | consumer, runs the parallel O_DIRECT load |
| dn0   | yupeng@192.168.122.48  | data node, owns the only real storage |
| cn0   | yupeng@192.168.122.125 | connection node, initially the **active** path |
| cn1   | yupeng@192.168.122.229 | connection node, initially the **standby** path |

All four are NTP-synced but drift ~15 ms relative to each other; `run.sh`
measures each node's offset and normalises every event stamp into host0's clock
before reporting.

---

## 3. Topology

```
  dn0                                   cn0                          host0
  ---                                   ---                          -----
  dnv-zero (dm-zero, 1 GiB)
    └─ dnv-delay (dm-delay 50 ms)
         └─ dnv-lin-cn0 ──► ss-to-cn0 ──► nvme0n1 ─► dnv-lin-on-cn0 ─► ss-path ─┐
              (ANA grp1 optimized)                        dnv-err-on-cn0        │
                                                                                ├─► /dev/nvme0n1
  dnv-err-cn0 (dm-error)                cn1                                     │   (2 ANA paths,
  dnv-err-cn1 (dm-error)                ---                                     │    one NQN)
    └─ dnv-lin-cn1 ──────► ss-to-cn1 ──► nvme0n1    dnv-err-on-cn1              │
              (ANA grp2 non-optimized)               └─ dnv-lin-on-cn1 ─► ss-path ┘
                                                                (ANA grp2 inaccessible)
```

Initial state: the real storage (zero + 50 ms delay) is wired to the **cn0**
lane; the cn1 lane is parked on a `dm-error` device. host0 therefore drives all
I/O through cn0.

### Name mapping (spec term → actual name)

| spec | dn0 | cn0 | cn1 |
|---|---|---|---|
| zero-dev | `dnv-zero` | | |
| delay-dev | `dnv-delay` | | |
| error-dev-cn0 / cn1 | `dnv-err-cn0`, `dnv-err-cn1` | | |
| linear-dev-cn0 / cn1 | `dnv-lin-cn0`, `dnv-lin-cn1` | | |
| error-dev-on-cnX | | `dnv-err-on-cn0` | `dnv-err-on-cn1` |
| linear-dev-on-cnX | | `dnv-lin-on-cn0` | `dnv-lin-on-cn1` |
| port-dn0 / port-cn0 / port-cn1 | nvmet port `1` | nvmet port `1` | nvmet port `1` |
| optimized-dn0 / non-optimized-dn0 | ana group `1` / `2` | | |
| optimized-cnX / inaccessible-cnX | | ana group `1` / `2` | ana group `1` / `2` |
| ss-to-cn0 | `nqn.2026-08.org.dnv:dn0.cn0` | | |
| ss-to-cn1 | `nqn.2026-08.org.dnv:dn0.cn1` | | |
| ss-path-cn0 / ss-path-cn1 | | `nqn.2026-08.org.dnv:vol0` | same NQN |
| nvme-mpath-dev | | | host0 `/dev/nvme0n1` |

The `dnv-` device-name prefix is not cosmetic: the udev rule installed by
`install_udev_rule()` keys off it.

---

## 4. Files

Workstation-side drivers (run from this directory):

| file | purpose |
|---|---|
| `nodes.sh` | node addresses + remote dir (`~/failover1`) |
| `deploy.sh` | push the node-side scripts to all four nodes |
| `teardown_all.sh` | unwind the fleet top-down, then print a residue check |
| `setup_all.sh` | build the topology bottom-up |
| `run.sh` | the experiment: start load → baseline → fire all three failover scripts on a shared barrier → collect → report |
| `report.py` | turn `io.json` + `markers.json` into the phase table and timeline |

Node-side scripts (deployed to `~/failover1`):

| file | runs on | purpose |
|---|---|---|
| `common.sh` | all | constants, dm/nvmet/nvme helpers |
| `cleanup.sh` | all | one teardown phase: `disconnect` \| `nvmet` \| `dm` \| `all` |
| `dn0_setup.sh` | dn0 | dm stack + two subsystems in two ANA groups |
| `cn_setup.sh cn0\|cn1` | cn0, cn1 | connect upstream, local dm, re-export shared NQN |
| `host0_setup.sh` | host0 | connect both paths, verify aggregation |
| `dn0_failover.sh` | dn0 | the 6 steps below |
| `cn0_failover.sh` | cn0 | retire cn0 |
| `cn1_failover.sh` | cn1 | promote cn1 |
| `barrier.sh` | dn0/cn0/cn1 | spin until a given epoch, then exec — used only to start the three failover scripts together, it does not alter their contents |
| `io_load.py` | host0 | parallel O_DIRECT load generator |

---

## 5. Running it

```bash
./deploy.sh
./teardown_all.sh          # safe on a dirty fleet; verify the residue check is empty
./setup_all.sh
DUR=45 BASELINE=12 THREADS=8 OUT=results ./run.sh
./teardown_all.sh
```

`run.sh` prints the report and leaves `io.json`, `markers.json`, the three
`*.stamps` files and `report.txt` in `$OUT`.

**Order is load-bearing in both directions.** Setup is bottom-up (dn0 → cn0 →
cn1 → host0) because each layer exports a device that the layer above imports.
Teardown is top-down (host0 disconnect → cn nvmet → cn dm → cn disconnect →
dn0 nvmet → dn0 dm) because each layer holds the one below it open; getting this
wrong yields `EBUSY` or, worse, a wedged `nvmet-wq` worker.

---

## 6. The failover choreography

Three scripts run **in parallel**, coordinating only through ANA state — no node
ever queries another node.

**dn0_failover.sh** — move the storage from the cn0 lane to the cn1 lane:
1. suspend `dnv-lin-cn0` (quiesce the cn0 lane)
2. suspend `dnv-lin-cn1`, reload it onto `dnv-delay` (give cn1 the real storage)
3. `ns-to-cn1` ana_grpid → 1 (optimized) — this is the signal cn1 waits for
4. sleep 5
5. reload `dnv-lin-cn0` onto `dnv-err-cn0`, resume
6. `ns-to-cn0` ana_grpid → 2 (non-optimized) — this is the signal cn0 waits for

**cn0_failover.sh** — retire cn0:
1. `ns-path-cn0` ana_grpid → 2 (inaccessible) — host0 stops using this path
2. wait until the local upstream ANA state reads `non-optimized`, read purely
   from `/sys/class/nvme/nvme0/nvme0c0n1/ana_state`
3. reload `dnv-lin-on-cn0` onto `dnv-err-on-cn0`

**cn1_failover.sh** — promote cn1:
1. wait until the local upstream ANA state reads `optimized` (same sysfs file)
2. reload `dnv-lin-on-cn1` onto the upstream nvme namespace
3. `ns-path-cn1` ana_grpid → 1 (optimized) — host0 starts using this path

---

## 7. Results (2026-08-18)

8 threads, 4 KiB O_DIRECT write+read on host0's `/dev/nvme0n1`, 45 s run,
12 s baseline before the trigger.

| phase | IOs | ok | **failed** | min | p50 | avg | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|---|
| before failover | 2016 | 2016 | **0** | 51.20 ms | 53.58 ms | 53.61 ms | 55.09 ms | 55.77 ms | 57.14 ms |
| **during** failover (5.195 s) | 776 | 776 | **0** | 50.52 ms | 52.00 ms | 53.54 ms | 52.69 ms | 53.27 ms | **199.91 ms** |
| after failover | 3934 | 3934 | **0** | 51.29 ms | 53.58 ms | 53.63 ms | 55.00 ms | 55.60 ms | 72.39 ms |

* **6726 I/Os total, 0 failed, 0 threads stuck at exit.**
* Longest gap with no successful completion: **0.200 s**, from +0.011 s to
  +0.211 s relative to the trigger.
* Exactly **8 I/Os** (one per thread — the ones in flight when cn0 went
  inaccessible) exceeded 100 ms; all 8 took ~199.9 ms and all 8 succeeded. This
  matches exactly 8 `block nvme0n1: no usable path - requeuing I/O` lines in
  host0's dmesg: requeued and replayed on the cn1 path, never failed.
* Steady-state ~53.6 ms = the 50 ms `dm-delay` plus ~3.6 ms of TCP + nvmet
  overhead across both hops. Throughput ~144 IOPS (delay-bound, not a
  performance measurement).
* Interesting: p50 *drops* to 52.0 ms during/just after the cutover. The cn1
  lane is a fresh dm-linear over the same delay device with no accumulated
  queueing, so it is marginally faster.

### Event log (normalised to host0's clock)

```
+0.000  dn0 step1 suspend dnv-lin-cn0
+0.001  cn0 step1 ns-path-cn0 ana_grpid -> 2 (inaccessible)
+0.013  cn1 step1 waiting for ana_state=optimized  (now: non-optimized)
+0.023  cn0 step2 waiting for ana_state=non-optimized (now: optimized)
+0.040  dn0 step2 done   (dnv-lin-cn1 -> dnv-delay)
+0.043  dn0 step3 ns-to-cn1 ana_grpid -> 1 (optimized)
+0.072  cn1 step1 done, ana_state=optimized              <-- 29 ms ANA propagation
+0.151  cn1 step2 done   (dnv-lin-on-cn1 -> /dev/nvme0n1)
+0.153  cn1 step3 ns-path-cn1 ana_grpid -> 1 (optimized)
+0.167  cn1 finished                                      <-- 167 ms to promote
+5.059  dn0 step5 reload dnv-lin-cn0 -> dnv-err-cn0
+5.075  dn0 step6 ns-to-cn0 ana_grpid -> 2 (non-optimized)
+5.097  cn0 step2 done, ana_state=non-optimized           <-- 22 ms ANA propagation
+5.189  cn0 step3 done   (dnv-lin-on-cn0 -> dnv-err-on-cn0)
+5.195  cn0 finished
```

The host-visible outage is bounded by cn1's 167 ms promotion, not by dn0's 5 s
sleep. Everything after +0.167 s is cleanup of the abandoned cn0 lane.

Final path states on host0: `nvme0c0n1: inaccessible`, `nvme0c1n1: optimized`.

---

## 8. Design decisions and deviations from the original spec

**cn1 got a dm layer the spec omitted.** The spec says `ns-path-cn1` is backed
by `nvme-dev-cn1` directly, but `cn1_failover.sh` step 2 reloads a
`linear-dev-on-cn1` that would then not exist — the cn1 setup section is missing
the `dmsetup` block that cn0 has. Resolved symmetrically with cn0: cn1 gets
`dnv-err-on-cn1` + `dnv-lin-on-cn1` parked on the error device, the namespace is
backed by the linear, and failover swings it onto the nvme namespace. This is
what makes step 2 meaningful.

**dn0 step 1 uses a flushing suspend** (`--nolockfs`, deliberately *not*
`--noflush`), so in-flight I/O completes against the good table and only new
I/O is held. With `--noflush` those bios would be requeued, replayed against the
error table at +5 s, and returned as hard EIO with DNR set — which nvme-multipath
does **not** retry on another path, so they would surface as real host failures.

**cn1's upstream is non-optimized, not inaccessible.** A namespace whose only
path is ANA-inaccessible gets no `/dev` node at all (the head disk is added only
by `nvme_mpath_set_live`), so cn1 would have nothing to build its dm stack on at
setup time. `non-optimized` is usable, so the device appears while still telling
cn1 it is the standby.

**ANA group moves are done live.** Writing `ana_grpid` on an *enabled* namespace
with a connected controller works and is what the failover scripts do — no
disable/enable, no port unlink/relink. Unlinking a subsystem from a port kills
the host controller with a DNR refusal and forces a reconnect; disabling a
namespace is unnecessary for a group move.

**Disjoint cntlid ranges.** cn0 uses `attr_cntlid_min/max` 1–255, cn1 256–511.
Both controllers live in one subsystem on host0, so overlapping controller IDs
would collide. Confirmed in dmesg: cn0 issued controller `1`, cn1 issued `256`.

**Identical namespace identity.** `nqn`, `nsid`, `device_uuid`, `device_nguid`,
size, `attr_model` and `attr_serial` are byte-identical on cn0 and cn1, or the
host refuses to aggregate the two controllers into one namespace head.

---

## 9. Environment traps hit along the way

* **`dd` is unusable here.** These VMs ship `dd (uutils coreutils) 0.8.0`, whose
  `iflag/oflag=direct` produce false EINVAL failures and, worse, silent
  zero-writes. Every I/O in this test goes through `os.O_DIRECT` with an
  `mmap.mmap(-1, n)` page-aligned buffer in `io_load.py`.

* **udev probes wedge on dm devices.** `install_udev_rule()` drops a
  `/etc/udev/rules.d/58-dnv-test.rules` that sets `DM_UDEV_DISABLE_DISK_RULES_FLAG`
  and friends for `dnv-*` devices. It must sort after `55-dm.rules` (which sets
  `DM_NAME`) and before `60-persistent-storage-dm.rules`.
  `OPTIONS+="ignore_device"` is obsolete and does nothing. All `dmsetup` calls
  pass `--noudevsync`.

* **`set -e` + `a && b` aborts the script** when `a` fails, because the AND-list
  itself returns non-zero. This silently killed `host0_setup.sh`'s
  wait-for-device loop on its first iteration. All such loops now use explicit
  `if`.

* **`nvme list-subsys <nqn>` returns nothing** — the argument is expected to be a
  device, not an NQN. Path counting reads sysfs instead.

* **A backgrounded remote command still holds the ssh channel open.** The first
  run started the load with `nohup ... &` over ssh; the ssh call blocked for the
  full 45 s, so the load finished 13 s *before* the failover and the report
  showed an empty "during" phase. `run.sh` now runs the ssh itself in the
  background and `wait`s on it.

* **A dm suspend over a possibly-inaccessible nvme namespace can block forever**,
  because DM waits for bios already handed to the underlying device even with
  `--nolockfs --noflush`, and requeued nvme bios are only freed by deleting the
  controller. `dm_reload()` in `common.sh` runs the suspend in the background
  with a deadline and warns rather than hanging. In practice the deadline was
  never hit, because the choreography always resumes dn0's lane onto an error
  table before cn0 tries to suspend on top of it.

* **Attributes read back space-padded.** `attr_serial` reads back padded to 20
  chars, so a naive "write only if different" guard never matches and retries the
  write, which fails with `EBUSY` once controllers are connected. `nvmet_write()`
  trims trailing blanks on both sides. Likewise `ln -sfn` on an existing
  port→subsystem link is *not* a no-op — it tears down attached controllers — so
  `nvmet_link_subsys()` guards on the link already existing.

---

## 10. Current state

The fleet was torn down after the run. `teardown_all.sh`'s residue check reads
empty on all four nodes:

```
192.168.122.193  ports=[] subsys=[] dm=[] ctrls=[]
192.168.122.48   ports=[] subsys=[] dm=[] ctrls=[]
192.168.122.125  ports=[] subsys=[] dm=[] ctrls=[]
192.168.122.229  ports=[] subsys=[] dm=[] ctrls=[]
```

No nvmet ports/subsystems/hosts, no `dnv-*` dm devices, no nvme controllers, no
D-state tasks anywhere, and the test udev rule removed.

Two things are deliberately left behind, both inert and both rewritten
idempotently by setup:

* the node-side scripts, deployed at `~/failover1` on all four nodes, so a
  re-run starts straight at `./setup_all.sh`;
* `/etc/nvme/hostnqn` on host0/cn0/cn1 — a normal system file. It is written
  explicitly because these VMs are clones and would otherwise share a
  DMI-derived default hostnqn, which collides on the target.
