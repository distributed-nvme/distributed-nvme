# ANA failover by group-membership move: timeline and timing budget

Lab notes, 2026-08-17. Nodes: `host0` 192.168.122.193, `dn0` 192.168.122.48,
`cn0` 192.168.122.125, `cn1` 192.168.122.229 — all `ubuntu2604b`,
Linux 7.0.0-29-generic. Driver script: [`poc/ana5.sh`](poc/ana5.sh); workload:
[`poc/host0_io.sh`](poc/host0_io.sh) (unmodified).

**Question.** A 4-node ANA failover where every port carries a permanently
`optimized` group (A) and a permanently `inaccessible` group (B), and a failover
*moves the namespace between groups* rather than flipping a group's state. Each
node runs its own step sequence with no cross-node orchestration. Which steps are
timing-critical, and how fast must they be before the host sees I/O errors?

**Answer.** Exactly one window is correctness-critical: **the interval on dn0
during which the subsystems are off its nvmet port**. It must close before the
CNs' first reconnect attempt (~2.0–2.6 s here). Measured: a 1.3 s window is
clean, a 2.3 s window loses 100% of I/O. Every other step is a latency dial with
no correctness cliff — stalling any of them by 2 s costs ~2 s of extra stall and
zero errors. The failure is not a stall: it is real `EIO`, because the CN's
backing namespace is *deleted* while the CN is still advertising its export as
Optimized, and the resulting error is a **generic** NVMe status with DNR, which
native multipath completes instead of failing over.

---

## Part 1 — The stack

### 1.1 Design

Every node has **one** nvmet port carrying **two** ANA groups whose states never
change:

| node | port | group A (id 1) | group B (id 2) |
|---|---|---|---|
| dn0 | dn0:4420 | `dn0A` optimized | `dn0B` inaccessible |
| cn0 | cn0:4420 | `cn0A` optimized | `cn0B` inaccessible |
| cn1 | cn1:4420 | `cn1A` optimized | `cn1B` inaccessible |

A failover is "put the namespace in the other group", i.e.
`echo 0 > enable` → `echo <gid> > ana_grpid` → `echo 1 > enable`. Since a
namespace cannot be disabled while a live controller is using it, each move is
wrapped in an unlink/relink of the subsystem from the port. This is the whole
source of the timing problem in Part 4.

```
dn0   loop(1G) --+-- dm-linear-cn0 -> loop           real data,  subsys-cn0, dn0A
                 +-- dm-error-cn0                    parking lot for lin-cn0
                 +-- dm-error-cn1 <- dm-linear-cn1   EIO backing, subsys-cn1, dn0B
      port 1: subsys-fake-dn0 (anchor) + subsys-cn0 + subsys-cn1

cn0   imports subsys-cn0 -> /dev/nvme0n1
      dm-linear-cn0 -> that ns ; dm-error-cn0 standby
      port 1: subsys-fake-cn0 (anchor) + subsys-host0-cn0, ns in cn0A (optimized)

cn1   imports subsys-cn1 (inaccessible, so no /dev node yet)
      dm-linear-cn1 -> LOCAL dm-error
      port 1: subsys-fake-cn1 (anchor) + subsys-host0-cn1, ns in cn1B (inaccessible)

host0 connects to cn0 and cn1 -> ONE native-multipath namespace /dev/nvme0n1
```

### 1.2 Two things the design forces

**Anchor subsystems are mandatory.** nvmet only creates a port's TCP listener
when the port has at least one subsystem. During a failover the real subsystems
are unlinked, so without `subsys-fake-*` the port would disappear and the whole
node would drop off the network. With the anchor, the port keeps listening — and
that turns out to be the mechanism that *causes* the failure mode in Part 4,
because a listening port answers a reconnect with a refusal rather than letting
it time out.

**The two host0 exports must share one NQN.** NVMe native multipath aggregates
*controllers of the same subsystem*, and a subsystem is identified by its NQN.
`subsys-host0-cn0` and `subsys-host0-cn1` are therefore the cn0-side and cn1-side
instance of one subsystem — `nqn.2026-07.org.dnv:sp:sp0:td0:exp0`, same ns
uuid/nguid, serial and model, cntlid ranges 1–255 and 256–511. Two distinct NQNs
cannot be one multipath device.

### 1.3 Host tuning

Both CNs and host0 connect with `--reconnect-delay 1 --ctrl-loss-tmo 600`.
The default `reconnect_delay` is 10 s, which would make every window in Part 4
roughly 10× more forgiving — but that parameter belongs to the *host*, not the
target, so a target-side design cannot rely on it.

```
host0 /sys/module/nvme_core/parameters:
  io_timeout=30   max_retries=5   iopolicy=numa   multipath=Y
```

---

## Part 2 — Baseline: orchestrated (sequential) failover

Run of 2026-08-17 06:57, stages driven one after another from the workstation.
Sequence: dn0 swaps the groups → cn1 waits Optimized, takes the data path,
promotes itself → cn0 waits Inaccessible, steps down → dn0 releases the parked
`lin-cn0` → cn0 comes back as an inaccessible standby.

**28,774 O_DIRECT I/Os, 0 failures.**

| phase | IOs | ok | fail | avg | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|
| before | 5,593 | 5,593 | 0 | 1.272 | 1.219 | 1.914 | 2.692 | 5.454 |
| during | 13,841 | 13,841 | 0 | 1.619 | 1.215 | 1.904 | 2.701 | **4909.8** |
| after | 9,340 | 9,340 | 0 | 1.197 | 1.153 | 1.727 | 2.485 | 4.185 |
| **total** | **28,774** | **28,774** | **0** | 1.415 | 1.195 | 1.856 | 2.635 | 4909.8 |

(ms; 718 IO/s before, 764 IO/s after.) The entire failover cost **one 4.91 s
read**. The 1 MiB anchor block at 512 MiB verified byte-for-byte on all 9,591
iterations, so nothing was lost or rolled back.

### 2.1 Finding: a path taken off a port never comes back by itself

While `subsys-host0-cn0` was unlinked, host0's reconnect was answered:

```
nvme nvme0: Connect Invalid Data Parameter, subsysnqn "nqn.2026-07.org.dnv:sp:sp0:td0:exp0"
nvme nvme0: failed to connect queue: 0 ret=16770
nvme nvme0: Failed reconnect attempt 1/600
nvme nvme0: Removing controller (16770)...
```

The status carries DNR, so nvme-tcp gives up after **one** attempt regardless of
`--ctrl-loss-tmo 600`, and the controller is removed permanently. Relinking does
not bring it back; the host must re-issue `nvme connect`. In production a
discovery controller + nvme-stas would do that on the "discovery log changed"
AEN. `do_verify_host` does it explicitly here.

This did **not** cause I/O errors, because the DNR was on the *Connect* command,
not on any I/O: it kills a controller, and the multipath layer requeues the bios
onto the surviving cn1 path (`no usable path - requeuing I/O`). Contrast with
Part 5, where a DNR lands on an I/O command and the outcome is completely
different.

### 2.2 Finding: cn0's `dmsetup` reload deadlocks without an escape hatch

The moment `subsys-cn0` went inaccessible, cn0 logged
`block nvme0n1: no usable path - requeuing I/O`. That bio was never sent to dn0,
so dn0 resuming `lin-cn0` onto its dm-error does **not** free it — it would sit
there until a path went optimized again, which never happens. A
`dmsetup suspend` of anything stacked on that namespace then blocks forever, even
with `--nolockfs --noflush`, because DM waits for bios already handed to the
underlying device.

`dm_suspend_deadline` runs the suspend in the background and, if it has not taken
within N seconds, runs an escape hatch — `nvme disconnect -n <subsys-cn0>` — which
marks the namespace dead and fails every requeued bio. This fires on every run;
it is deterministic, not a fluke.

---

## Part 3 — The timing experiment

### 3.1 Method

`do_failover` now launches the three node sequences **concurrently** and
interleaves their timestamped logs. Nothing coordinates them except what each
node can observe locally: cn0 and cn1 poll their own sysfs `ana_state` (refreshed
from the ANA log on the AEN — no query to dn0), and dn0 has only its own
`sleep 3`. Within a node the order is exactly as specified.

A `_pause <step-id>` after each key step sleeps `FO_DELAY` seconds; `FO_ONLY`
restricts the stall to a single step. Each id names the **window that step
opens**:

| node | ordered steps | window |
|---|---|---|
| dn0 | 1 unlink both → 2 ns-cn0→dn0B → 3 ns-cn1→dn0A → 4 suspend lin-cn0 → 5 lin-cn1→loop → 6 relink both → 7 sleep 3 + lin-cn0→err | **1–5 are inside the port-off window** |
| cn1 | 1 wait Optimized → 2 dm→subsys-cn1 → 3 unlink export → 4 ns→cn1A → 5 relink | 3–4 inside cn1's export-off window |
| cn0 | 1 wait Inaccessible → 2 unlink export → 3 dm→dm-error → 4 ns→cn0B → 5 relink | 2–4 inside cn0's export-off window |

One run = fresh stack, steady-state I/O, one parallel failover, 6 s soak, one
verdict line. Nothing is repaired afterwards — the experiment is about what
survives on its own.

```bash
./ana5.sh sweep 0 0.25 0.5 1 2 4                    # same stall after every step
./ana5.sh attrib 2 dn0-unlink dn0-relink cn1-wait \
                   cn1-unlink cn0-wait cn0-unlink   # stall one window only
FO_DELAY=1 FO_ONLY=dn0-unlink ./ana5.sh run-once    # bracket a threshold
```

### 3.2 Uniform sweep — stall after *every* step

| D | IOs | failed | first fail | max stall | end state |
|---|---|---|---|---|---|
| 0 s | 7,578 | **0** | – | 4.70 s | cn1 optimized, serving |
| 0.25 s | 7,630 | **0** | – | 5.30 s | cn1 optimized, serving |
| 0.5 s | 215,053 | 214,815 | +2.7 s | – | both paths inaccessible |
| 1 s | 219,652 | 219,453 | +2.7 s | – | broken |
| 2 s | 231,769 | 231,591 | +2.7 s | – | broken |
| 4 s | 240,398 | 240,212 | +2.6 s | – | broken |

The cliff is between 0.25 s and 0.5 s, and every failing run breaks at the **same
absolute time**, +2.6–2.7 s, however much delay is injected. That is a fixed
timer, not a proportional effect. (The six-figure I/O counts are the workload
spinning on instant `EIO`s.)

### 3.3 Attribution — 2 s applied to exactly one window

| window stalled | failed | max stall | verdict |
|---|---|---|---|
| **dn0-unlink** | **224,802** | – | **fatal on its own** |
| dn0-relink | 0 | 4.72 s | harmless (outside the window) |
| cn1-wait | 0 | 6.75 s | +2 s latency, 1:1 |
| cn1-unlink | 0 | 5.00 s | harmless |
| cn0-wait | 0 | 5.75 s | +1 s latency |
| cn0-unlink | 0 | 4.95 s | harmless |
| dn0-unlink @ **1 s** | 0 | 4.69 s | still clean |

A 2 s stall on `dn0-unlink` alone reproduces the full failure, identical to the
uniform D=4 run. A 2 s stall on `dn0-relink` — the same node, two steps later,
but *after* the subsystems are back on the port — is indistinguishable from
baseline. The budget is bracketed between **1.3 s (clean)** and **2.3 s (total
loss)**.

---

## Part 4 — Two timelines

### 4.1 Clean run (parallel, D=0). t0 = dn0's unlink, 07:43:47.384

t0 is back-calculated from dn0's step-2 stamp minus the measured step durations
(32 ms unlink + 77 ms ns-move, both taken from the instrumented run in 4.2);
everything else is a logged stamp.

```
+0.00  dn0  unlink subsys-cn0 + subsys-cn1  (32 ms)   <-- window opens
+0.11  dn0  ns subsys-cn0 -> dn0B           (77 ms)
+0.15  dn0  ns subsys-cn1 -> dn0A           (54 ms)
+0.28  dn0  suspend lin-cn0                 (134 ms)  <-- dm work, inside window
+0.32  dn0  lin-cn1 -> loop                 (54 ms)   <-- dm work, inside window
+0.34  dn0  relink both                     (18 ms)   <-- window closes, 336 ms
+2.39  cn1  observes Optimized locally  (its ctrl to dn0 reconnected)
+2.44  cn1  dm-linear -> subsys-cn1 disk
+2.45  cn1  unlink export
+2.48  cn0  observes Inaccessible locally
+2.49  cn0  unlink export
+2.50  cn1  ns -> cn1A
+2.51  cn1  relink export                             <-- cn1 serving, real data
+3.38  dn0  parked lin-cn0 -> dm-error-cn0, resume
+4.70       host0's longest I/O completes (on cn1)
+7.79  cn0  dm-linear -> local dm-error   (after the 5 s escape hatch)
+7.85  cn0  ns -> cn0B
+7.86  cn0  relink export                             <-- cn0 back as standby
```

The 336 ms window is 188 ms (56%) dm work — steps 4 and 5 — that has no reason to
be inside it.

### 4.2 Fatal run (2 s on `dn0-unlink`). t0 = 08:38:05.18

```
+0.03  dn0  unlink both done                          <-- window opens
       ...  stalled 2 s ...
~+2.0  cn0  first reconnect attempt to dn0 lands INSIDE the window:
              nvme nvme0: Failed reconnect attempt 1/600
              nvme nvme0: Removing ctrl: NQN "...:subsys-cn0"
              block nvme0n1: no available path - failing I/O
+2.11  dn0  ns subsys-cn0 -> dn0B
+2.16  dn0  ns subsys-cn1 -> dn0A
+2.30  dn0  suspend lin-cn0
+2.35  dn0  lin-cn1 -> loop
+2.37  dn0  relink both                               <-- window closes, 2.37 s
                                                          ~0.35 s too late
~+2.5       host0's first EIO  (+2.7 s after the failover_start mark, which
                                precedes dn0's t0 by the ssh round trip)
```

The window closed roughly a third of a second after the deadline, and that was
enough to lose everything.

---

## Part 5 — Why it is `EIO` and not a stall

The chain, each link confirmed in `dmesg`:

1. dn0 unlinks `subsys-cn0`; cn0's controller drops and retries after 1 s.
2. dn0's port is **still listening** (the anchor subsystem), so the retry is
   *answered* — with `Connect Invalid Data Parameter` + DNR. cn0's controller is
   removed permanently after one attempt.
3. cn0's imported namespace disk is deleted:
   `block nvme0n1: no available path - failing I/O`. Its `dm-linear` now sits on
   a dead device and every bio into it fails.
4. cn0 is **still advertising its export as Optimized** — and can never learn
   otherwise, because the sysfs it polls belongs to the controller that just
   disappeared. Its `wait_ana` runs to full timeout; it never steps down.
5. host0 keeps routing there and receives:

```
nvme0c0n1: I/O Cmd(0x2) @ LBA 1048576, 2048 blocks, I/O Error (sct 0x0 / sc 0x6) MORE DNR
I/O error, dev nvme0c0n1, sector 1048576 op 0x0:(READ)
```

`sct 0x0 / sc 0x6` is **Generic Command Status / Internal Error** — a *generic*
status, not a path status (`sct 0x3`). `nvme_is_path_error()` is false and DNR is
set, so `nvme_decide_disposition()` returns COMPLETE: the multipath layer hands
the error straight to the application instead of failing over to cn1. Immediate,
continuous `EIO`.

The failover also deadlocks in the same stroke: cn1's controller to dn0 is
removed for the same reason, so cn1 never observes Optimized, never takes the
data path and never promotes itself. Both exports end `inaccessible`.

**Contrast with Part 2.1.** Same DNR bit, opposite outcome. On a *Connect*
command it removes a controller, and multipath requeues the bios onto a sibling
path — zero errors. On an *I/O* command it suppresses failover entirely. What
makes the difference is not the flag, it is which command carries it.

---

## Part 6 — Conclusions

### 6.1 The single deadline

**dn0's unlink → relink window must close before the CNs' first reconnect
attempt.** Here that lands ~2.0–2.6 s after the drop (`reconnect_delay` 1 s plus
~1 s of error recovery). Measured: 1.3 s clean, 2.3 s = 100% loss.

Do not budget against 2 s. That number *is* `reconnect_delay`, which the **host**
chooses, not the target — at the kernel default it would be ~11 s, and a host may
set it lower. Treat the window as needing to be in the **hundreds of
milliseconds**, and design so that nothing blocking can enter it.

### 6.2 Everything else is a latency dial

`cn1-wait`, `cn1-unlink`, `cn0-wait`, `cn0-unlink`, `dn0-relink`: 2 s of stall on
any of them produced **zero** errors and roughly a second-for-second increase in
the worst-case I/O. A CN that is slow to take over, or slow to step down, costs
latency and nothing else. The floor of ~4.7 s at D=0 is host0's own error
recovery on the cn0 path plus cn1's ~2 s wait for the ANA state to propagate — it
is not the scripts being slow.

### 6.3 Recommendations

1. **Move dm work out of the port-off window.** dn0 spends 188 ms of its 336 ms
   window on `suspend lin-cn0` + `reload lin-cn1`. Doing them before the unlink
   or after the relink leaves only unlink + two `ana_grpid` moves + relink
   (~181 ms), taking the safety margin against a 2 s deadline from ~6× to ~11×.
2. **Prefer flipping `ana_state` over moving `ana_grpid`.** A state flip is a live
   attribute write: no unlink, no ns disable, no window at all, and the ANA-change
   AEN reaches the hosts without any controller being torn down. This is what
   [`poc/ana4.sh`](poc/ana4.sh) does. Only reach for a membership move when the
   namespace genuinely has to change groups.
3. **If a membership move is required, unlink and relink back-to-back** with
   nothing but the `ana_grpid` writes in between — no logging round-trips, no
   dm operations, no RPCs, no lock acquisition.
4. **A node that loses its upstream must fail closed.** cn0's failure was not
   being slow; it was continuing to advertise Optimized after its backing
   namespace vanished. A watchdog on the backing device (or on the controller's
   `state`) that demotes the export to the B group on loss would turn this from
   data-plane failure into an ordinary path switch.
5. **Re-establish paths explicitly after any unlink-based step.** A DNR-refused
   reconnect is permanent; relinking does not undo it.

---

## Artifacts

| file | contents |
|---|---|
| `poc/ana5.sh` | the whole demo: `prep setup host failover verify showreport diagnose status teardown`, plus `run-once` / `sweep` / `attrib` |
| `poc/host0_io.sh` | O_DIRECT workload + JSON-per-I/O log + phase report (unmodified) |
| `$RESULTS` (default `/tmp/ana5-results.tsv`) | one verdict line per run — the 13 lines of Part 3 are reproduced there |
| `/tmp/dnv-io.jsonl` on host0 | every individual I/O of the last run, one JSON object each |

The raw per-run logs of this session lived in a session-local scratchpad and are
not preserved; everything load-bearing from them is quoted above. Re-running
`./ana5.sh sweep 0 0.25 0.5 1 2 4` regenerates the table in Part 3.2.

All resources on all four nodes were torn down after the runs: no dm devices, no
nvmet subsystems or ports, no nvme controllers, no loop devices, backing file and
udev rule removed.
