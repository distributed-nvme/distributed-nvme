# When does an NVMe-oF target fault become a host I/O error?

Eight target-side fault scenarios (a..h) measured against a single-path NVMe-oF
device (test A) and against a two-path ANA multipath device (test B, all 8x8
fault combinations under three ANA matrices = 192 cases).

Everything below is measured, not derived from reading source. Harness lives in
`poc/ioerr/` (`cnctl.sh`, `hostctl.sh`, `io_probe.py`, `run_test.py`,
`report.py`, `run_e2.sh`); raw per-case JSON in `poc/ioerr/results/`.

## 1. Setup

| node | address | role |
|---|---|---|
| host0 | 192.168.122.193 | initiator, `nvme_core.multipath=Y`, `iopolicy=numa` |
| cn0 | 192.168.122.125 | nvmet target |
| cn1 | 192.168.122.229 | nvmet target (test B only) |

Kernel 7.0.0-29-generic, nvme-cli 2.16 / libnvme 1.16.1 on all three.

Each test case gets its own independent **lane** on the target so many cases can
run concurrently:

```
8G sparse file -> loop -> dm-linear 64MiB slice   (per-lane, so dm-error and
                             |                     dmsetup suspend are per-lane)
                             v
              nvmet subsystem (unique NQN, ns 1, fixed uuid/nguid)
                             |
                             v
              nvmet port with its own TCP port    (per-lane, so iptables rules
              + anchor subsystem, unlinked         and "port stops listening"
                                                   are per-lane)
```

For test B the same NQN and the same ns uuid/nguid are exported from both cn0
and cn1 with **disjoint `attr_cntlid_min/max` ranges** (cn0 1-100, cn1 101-200)
and identical `attr_serial`/`attr_model`; without that the host rejects the
second controller instead of aggregating it. Both targets carry ANA group 1 and
the matrix is applied by writing `ports/<p>/ana_groups/1/ana_state`.

**I/O load.** `io_probe.py` issues a 4 KiB `O_DIRECT` write + read-back every
100 ms in a worker thread, while the main thread samples the worker so a request
that never returns is recorded as a stall rather than freezing the measurement.
Every state transition carries a wall-clock epoch, and the target prints the
epoch at which it applied each fault, so *time to first I/O error* is a
subtraction of two timestamps on two hosts (NTP-synced VMs, sub-second skew).
`dd` is deliberately not used anywhere: the uutils `dd` on these images
mishandles `iflag/oflag=direct`.

### The eight scenarios

| | fault on the target |
|---|---|
| a | `iptables -I INPUT -p tcp --dport <port> -j DROP` (black hole) |
| b | `iptables ... -j REJECT --reject-with tcp-reset` |
| c | subsystem unlinked from the port, nothing else linked -> nvmet tears the TCP listener down |
| d | subsystem unlinked, an anchor subsystem keeps the listener up -> live target, unknown NQN |
| e | host symlink removed from `subsystems/<nqn>/allowed_hosts/` |
| f | `echo 0 > subsystems/<nqn>/namespaces/1/enable` |
| g | backend returns EIO (`dmsetup load` an `error` target) |
| h | backend swallows I/O (`dmsetup suspend --nolockfs --noflush`) |

Rules a/b match on `--dport` only, so ssh to the target stays up while every
packet the host sends to the NVMe port is dropped or reset.

## 2. The three mechanisms that decide everything

Every one of the 200 results falls out of three independent layers. Worth
internalising before reading the tables.

### 2.1 A completed command with a non-path error goes straight to the application

`dm-error` makes the backend return `-EIO`; nvmet maps that to **Internal Error
(sct 0x0 / sc 0x6) with the DNR bit set**:

```
nvme2c2n1: I/O Cmd(0x1) @ LBA 21040, 8 blocks, I/O Error (sct 0x0 / sc 0x6) MORE DNR
```

`nvme_failover_req` only reroutes *path*-related statuses. An Internal Error is
a completion, not a path failure, so multipath declines to retry it and the
block layer gets `BLK_STS_IOERR`. This is why scenario **g** produces `EIO`
within **80 ms** in test A, and why it still produces `EIO` in test B even when
the other path is perfectly healthy. A target that answers "I am broken" is
obeyed; only a target that stops answering triggers failover.

### 2.2 A broken transport hangs I/O; only losing the *last* path errors it

Any transport breakage moves the controller to `connecting`. While at least one
controller in the subsystem is `live` or `connecting`,
`nvme_ns_head_submit_bio` **requeues** the bio. The two kernel messages are the
whole story:

```
block nvme0n1: no usable path - requeuing I/O      <- some ctrl still live/connecting: hang, no error
block nvme7n1: no available path - failing I/O     <- last path gone: EIO
```

So an I/O error requires the controller to be *deleted*, and that is governed by
`ctrl_loss_tmo` — which is **not a wall-clock deadline**. The driver converts it
to a retry budget:

```
max_reconnects = ctrl_loss_tmo / reconnect_delay          (600/10 = 60, 10/2 = 5)
```

and each failed retry costs `reconnect_delay` *plus* however long the failure
itself takes. Measured on this fabric: nvme-tcp's own socket connect timeout is
**~3.4 s** when the SYN is black-holed (a bare TCP connect to the same dropped
port takes 136 s, so nvme-tcp is clearly imposing its own, much shorter, limit),
and ~0 s when the target sends RST or ICMP port-unreachable. Keep-alive
(`kato=5`) notices a black hole in ~9 s. That predicts test A exactly:

| | model | measured |
|---|---|---|
| b, c (instant refusal) | 9 + 60x10 + ~6 = 615 s | **615.7 s** |
| a (black hole) | 9 + 60x(10+3.4) = ~813 s | **808.8 s** |

**Consequence: `ctrl_loss_tmo=600` does not mean "errors after 600 s".** Against
a black hole it means ~810 s, and the multiplier grows with
`reconnect_delay`-to-failure-cost ratio. If you want a bounded failure time, set
`ctrl_loss_tmo` from the retry count you actually want, not from the seconds you
want.

### 2.3 A DNR-tagged Connect rejection skips the retry budget entirely

Scenarios d and e make the target *refuse* rather than *fail*. The reject
carries DNR, so the driver deletes the controller on the first attempt instead
of burning its budget:

```
d: nvme0: Connect Invalid Data Parameter, subsysnqn "nqn.2026-08.dnvio:d2"
   nvme0: failed to connect queue: 0 ret=16770        # 0x4182 = INVALID_PARAM | DNR
   nvme0: Failed reconnect attempt 1/5
   nvme0: Removing controller (16770)...
   (target) nvmet: connect request for invalid subsystem nqn.2026-08.dnvio:d2!

e: nvme0: Connect for subsystem nqn.2026-08.dnvio:e2 is not allowed, hostnqn: ...
   nvme0: failed to connect queue: 0 ret=16772        # 0x4184 = INVALID_HOST | DNR
   nvme0: Removing controller (16772)...
   (target) nvmet: connect by host ... for subsystem ... not allowed
```

Hence scenario **d** costs only `reconnect_delay + ~1.3 s` — 11.5 s with
`reconnect_delay=10`, 3.3 s with `reconnect_delay=2` — regardless of
`ctrl_loss_tmo`.

This is also the difference between c and d, which look almost identical from
the target's point of view. **c** (no listener) yields ECONNREFUSED, a transport
error, retried for the full budget: 615.7 s. **d** (listener up, NQN unknown) is
an administrative refusal: 11.5 s. Removing a subsystem from a port is 50x more
disruptive when the port still has another subsystem on it.

## 3. Test A -- single path

`nvme connect` with stock timeouts, `ctrl_loss_tmo=600 reconnect_delay=10
kato=5`, `io_timeout=30 s`; 8 lanes on cn0 faulted simultaneously and observed
for 900 s.

| case | fault | 1st I/O error | errno | I/O stalled from | ok / failed I/Os |
|---|---|---|---|---|---|
| a | DROP | **808.8 s** | EIO | 0.07 s | 104 / 335 |
| b | RST | **615.7 s** | EIO | 0.05 s | 104 / 977 |
| c | port not listening | **615.7 s** | EIO | 0.07 s | 104 / 977 |
| d | listening, NQN unknown | **11.5 s** | EIO | 0.05 s | 104 / 2984 |
| e | not in `allowed_hosts` | **never** | -- | never | 8882 / 0 |
| f | namespace disabled | **~0 s** | EIO | -- | 103 / 3022 |
| g | backend EIO | **0.08 s** | EIO | -- | 105 / 2997 |
| h | backend suspended | **never** | -- | 0.05 s | 105 / **0** |

The first `EIO` is followed by `ENOENT` in a/b/c/d/f: once the last controller
goes, the subsystem and its `/dev/nvmeXnY` are removed, so the probe's reopen
fails. In f the errno is sometimes `ENOSPC` instead — the namespace is gone but
the head disk still exists at zero capacity, so an `O_DIRECT` write lands past
EOF.

### Per-case notes

**a, b, c — transport loss, error only when the retry budget runs out.** All
three behave identically in kind: I/O stalls within ~50 ms (in-flight requests
never complete), the controller cycles `connecting` for the whole budget, then is
deleted and the queued bios fail. The only difference is how fast each retry
fails (§2.2). Mid-run state for all three: `nvme6=connecting/optimized`.

**d — administrative refusal, error in 11.5 s.** Unlinking the subsystem tears
the live controller down immediately (`starting error recovery in 1 seconds`),
and the very first reconnect is refused with DNR. Note the asymmetry with c
described in §2.3.

**e — no error, ever. This is the one true "no" in the table.** 8882 I/Os
completed over 900 s with zero errors and no stall; the controller stayed
`live/optimized` throughout. nvmet consults `allowed_hosts` **only in the
Connect handler** — there is no revocation path for an established controller.
The fault is latent, not absent: forcing a single reconnect exposes it instantly
(`run_e2.sh`):

```
15 s after removing the host from allowed_hosts:
    nqn=...:e2 dev=/dev/nvme0n1 paths: nvme0=live/optimized@...   # still serving I/O
# nvme reset /dev/nvme0
    Reset: Network dropped connection on reset
    nqn=...:e2 subsys=absent                                       # gone, one attempt, DNR
```

So `allowed_hosts` is an admission-control knob, not a fencing knob. If you need
to actually cut a host off, you must also delete its controller (unlink the
subsystem from the port, scenario c/d) — otherwise the host keeps reading and
writing until something unrelated happens to reset the controller, at which
point it dies with no warning and no retry.

**f — namespace disabled, error immediately.** Disabling the ns is not blocked
by the live controller and did not hang (113 ms to complete, even with I/O in
flight — the percpu ref drains as soon as the outstanding bios finish on a
healthy backend). The host gets an AEN, rescans, drops the namespace, and the
head disk disappears. Mid-run state: `nvme3=live/-` with `dev=-` — controller
healthy, namespace gone.

**g — backend EIO, error immediately.** 80 ms. See §2.1.

**h — no error, but the device is dead: zero I/Os completed in 900 s.** This is
the most dangerous row in test A, and the one place where "no I/O error" is much
worse news than an I/O error. The cycle, from dmesg:

```
nvme4: I/O tag 36 type 4 opcode 0x2 (I/O Cmd) QID 1 timeout   # io_timeout, 30 s
nvme4: starting error recovery in 1 seconds
nvme4: Reconnecting in 10 seconds...
nvme4: Successfully reconnected (attempt 1/60)                # target is fine!
... 30 s later, timeout again, attempt 1/60 again, forever
```

The transport is healthy, so every reconnect **succeeds**, which resets the
retry counter to `1/60`. `ctrl_loss_tmo` is therefore never reached and the
controller is never deleted, so the bios are requeued rather than failed. In-
flight requests cancelled during each reset are tagged
`NVME_SC_HOST_ABORTED_CMD`, which multipath treats as a path error and retries.
A target that accepts connections but never completes I/O produces an
**unbounded hang with no error and no timeout you can configure away** — no
`ctrl_loss_tmo` value changes this, because the counter keeps resetting.

## 4. Test B -- two paths

Same subsystem exported from cn0 and cn1, aggregated by the host into one
multipath device:

```
nqn=...:b-0 dev=/dev/nvme2n1 paths: nvme2=live/optimized@traddr=192.168.122.125
                                    nvme6=live/optimized@traddr=192.168.122.229
```

Both faults are then applied, one per target, within ~0.5 s of each other. Each
case is a fresh round: faults cleared, ANA reset to optimized, both controllers
reconnected, health verified, probe started, ANA matrix applied at +5 s, faults
at +15 s, observed 60 s.

**Compressed timeouts.** Test B uses `ctrl_loss_tmo=10 reconnect_delay=2
kato=5` (5 retries instead of 60) so 192 cases fit in ~20 minutes;
`io_timeout` stays at the 30 s default. To read a test B number as a
stock-timeout number, substitute the §2.2 model: the ~11.5 s cells become
~615 s and the ~32-35 s cells become ~810 s. The *shape* of every table is
timeout-independent; only the numeric cells scale.

**Reading the cells.** A number is seconds from fault injection to the first
`EIO`. Otherwise:

- `ok` — no error, no stall over 3 s. Failover was seamless.
- `stall/ok` — I/O paused, then resumed for good. Successful failover.
- `cycle` — no error, but I/O alternates long stall / brief service.
- `hang` — no error and I/O never resumed inside the window.

Negative values within a few hundred ms (e.g. `-0.41`) are batch skew: faults are
applied per-lane sequentially on each node and the baseline is the later of the
two epochs.

### 4.1 Matrix X — cn0 optimized, cn1 optimized

| cn0 \ cn1 | a | b | c | d | e | f | g | h |
|---|---|---|---|---|---|---|---|---|
| **a** | 34.41 | 34.4 | 34.38 | 34.37 | stall/ok | 34.85 | 9.15 | hang |
| **b** | 32.14 | 11.65 | 11.51 | 11.5 | ok | 11.47 | 0.06 | hang |
| **c** | 32.24 | 11.55 | 11.54 | 11.53 | ok | 11.51 | 0.02 | hang |
| **d** | 34.64 | 11.53 | 11.51 | 3.31 | ok | 3.28 | 0 | hang |
| **e** | ok | ok | ok | ok | ok | ok | ok | ok |
| **f** | 32.32 | 11.54 | 11.3 | 3.05 | ok | 0.03 | 0.01 | hang |
| **g** | 0.02 | 0.03 | 0.01 | 0.09 | 0.03 | 0.01 | 0.09 | -0 |
| **h** | hang | hang | hang | hang | cycle | hang | 31.59 | hang |

Note the matrix is **not symmetric**, even though both targets are configured
identically and both are `optimized`. With `iopolicy=numa` the head picks one
path and keeps using it, so the cn0 fault (the row) is usually the one that
decides the outcome, and the cn1 fault (the column) only matters once failover
happens. Compare `(g,h) = 0 s EIO` with `(h,g) = 31.6 s EIO`: the same pair of
faults, opposite assignment, two orders of magnitude apart in detection time.

**Row/column e — the fault with no effect.** Row e is `ok` in all eight cells:
cn0 keeps serving every I/O no matter what happens to cn1, because scenario e
does not disturb the established controller at all (§3, case e). Column e is
`ok` for cn0 in {b, c, d, f} — those faults tear the cn0 connection down
actively, the host notices in under 3 s and fails over to cn1 without the probe
ever seeing a 3 s stall.

**Errors when both paths are genuinely lost.** The b/c/d/f x b/c/d/f block is
~11.5 s (or 3.3 s where d/f is involved on cn1) and the a row/column is
~32-35 s. In every case the number is the time for the **slower** of the two
paths to be deleted, which is what you would expect: the last path to go
decides. Scale by §2.2 for stock timeouts.

**`(a,e) = stall/ok` vs `(b,e) = ok`.** Both end healthy on cn1; the difference
is 9 s of stalled I/O. A black hole must wait for keep-alive to expire; an RST
is instant. **If you care about failover latency, make failures loud.** A target
that is being taken down deliberately should reset its connections, not go
silent.

**Row h — a suspended backend wedges the multipath device even though a
healthy path exists.** `(h,e)`, the one case with a perfectly good alternate
path, is `cycle` — a long stall broken by a brief burst of service:

```
+0.0 s   stall             (cn0 selected, backend suspended)
+31.6 s  20 I/Os complete  (io_timeout -> reset -> failover to cn1)
+33.7 s  stalled again     (cn0 reconnected, numa policy sends I/O back to it)
```

The 60 s window caught one such cycle, so its period is not established; what is
established is that 177 I/Os completed in 80 s where a healthy device did 774.

This is §3 case h plus path reselection: the controller never dies, so it never
stops being a candidate, and the policy keeps preferring it. Every other cell in
row h is a flat `hang`. A target whose backend has stopped completing I/O is
worse than a target that is down, and adding a second path does not fix it.

### 4.2 Matrix Y — cn0 inaccessible, cn1 optimized

| cn0 \ cn1 | a | b | c | d | e | f | g | h |
|---|---|---|---|---|---|---|---|---|
| **a** | 34.68 | 32.05 | 32.16 | 32.02 | ok | 31.99 | 0 | hang |
| **b** | 34.54 | 11.55 | 11.66 | 11.71 | ok | 11.5 | 0.07 | hang |
| **c** | 35.01 | 11.51 | 11.5 | 11.48 | ok | 11.53 | 0.05 | hang |
| **d** | 34.88 | 11.57 | 11.55 | 3.34 | ok | 3.25 | 0.05 | hang |
| **e** | hang | hang | hang | hang | ok | hang | 0.02 | hang |
| **f** | 34.79 | 11.53 | 11.35 | 3.05 | ok | 0 | -0.41 | hang |
| **g** | hang | hang | hang | hang | ok | hang | 0.02 | hang |
| **h** | hang | hang | hang | hang | ok | hang | 0.02 | hang |

All I/O runs on cn1, so the matrix now cleanly separates by **whether the cn0
fault kills the cn0 controller or leaves it alive**:

- Rows **b, c, d, f** (and **a**, after ~35 s): the cn0 controller is deleted.
  When cn1 also loses its path, the subsystem has no paths at all -> `EIO`.
- Rows **e, g, h**: the cn0 controller stays **`live`, merely ANA-inaccessible**.
  `nvme_available_path()` counts it, so the head requeues forever -> **`hang`,
  no error, indefinitely.**

**This is the most important result in test B.** A standby path parked in
`inaccessible` is not free: it converts "the array returns errors so the
application fails over / aborts" into "the application hangs forever with no
diagnostic". `(g,a)` is the sharp case — cn0's backend is returning EIO and
cn1's network is black-holed, i.e. *nothing works anywhere*, and the host still
reports no error, because an inaccessible-but-live controller looks like a
usable future path. Column g is the only escape (`0.02 s`): a DNR Internal Error
from the accessible path is delivered regardless (§2.1).

Column e is `ok` everywhere for the same reason as matrix X: the cn1 controller
is untouched, and it is the only path carrying traffic.

### 4.3 Matrix Z — cn0 inaccessible, cn1 inaccessible

| cn0 \ cn1 | a | b | c | d | e | f | g | h |
|---|---|---|---|---|---|---|---|---|
| **a** | 33.83 | 32.09 | 32.08 | 32.06 | hang | 32.1 | hang | hang |
| **b** | 32.16 | 11.86 | 11.53 | 11.71 | hang | 11.55 | hang | hang |
| **c** | 32.39 | 11.77 | 11.5 | 11.49 | hang | 11.53 | hang | hang |
| **d** | 32.26 | 11.89 | 11.5 | 3.36 | hang | 3.27 | hang | hang |
| **e** | hang | hang | hang | hang | hang | hang | hang | hang |
| **f** | 32.28 | 11.76 | 11.33 | 3.02 | hang | 0.01 | hang | hang |
| **g** | hang | hang | hang | hang | hang | hang | hang | hang |
| **h** | hang | hang | hang | hang | hang | hang | hang | hang |

With every group inaccessible, I/O stops the moment the ANA matrix is applied —
10 s **before** any a..h fault is injected — and no error is produced. `EIO`
appears only where **both** faults delete **both** controllers (the
a/b/c/d/f x a/b/c/d/f block); the timings match matrix Y because the mechanism
is the same. Everywhere at least one controller survives as live-inaccessible
(row/column e, g, h) the device hangs indefinitely.

Column g is `hang` here, unlike in matrices X and Y, and that is the cleanest
demonstration of §2.1's precondition: an inaccessible path is never *selected*,
so its backend's `EIO` is never even solicited. The error in X/Y column g was
only visible because some path was actually issuing commands.

**Setup caveat.** Test B connects with both groups `optimized` and flips ANA
afterwards, because a namespace whose every path is inaccessible **has no head
block device at all**:

```
# connect with ana_state=inaccessible:
nqn=...:d2 dev=-  paths: nvme0=live/inaccessible@...
/sys/block/nvme0c0n1 ana=inaccessible          # per-path device exists
                                               # /dev/nvme0n1 does not
# echo optimized > .../ana_groups/1/ana_state:
nqn=...:d2 dev=/dev/nvme0n1 paths: nvme0=live/optimized@...
```

So "both sides inaccessible" is only reachable as a transition, never as an
initial state — worth knowing for any failover design that parks a namespace in
`inaccessible` before the host has ever seen it accessible.

### 4.4 Why `(d,d)` is ~3.3 s in all three matrices

`(d,d)` is 3.31 s in X, 3.34 s in Y and 3.36 s in Z -- the same number three
times, and far faster than any other cell in which both paths are destroyed
(11.5 s for b/c, 32-35 s for a). Both halves of that are worth spelling out.

**Why it is fast.** Scenario d is the only fault that combines an *immediate*
teardown with a *DNR* refusal, so neither the detection phase nor the retry
budget contributes anything. Traced with sub-second timestamps at
`reconnect_delay=5`:

```
T+0.00  (fault applied on the target)
T+0.00  nvme0: starting error recovery in 1 seconds      <- unlinking the subsystem
T+0.03  block nvme0n1: no usable path - requeuing I/O       drops the controller at
                                                            once; no keep-alive wait
T+1.27  nvme0: Reconnecting in 5 seconds...              <- 1 s error-recovery delay
T+6.65  nvme0: Connect Invalid Data Parameter, subsysnqn "..."
        nvme0: failed to connect queue: 0 ret=16770      <- 0x4182 = INVALID_PARAM | DNR
        nvme0: Failed reconnect attempt 1/5              <- DNR: no attempt 2
        nvme0: Removing controller (16770)...
        block nvme0n1: no available path - failing I/O
T+6.64  probe sees EIO
```

So the whole cost is `1 s + reconnect_delay + ~0.4 s`, where the 1 s is the
fixed error-recovery delay and the ~0.4 s is the TCP connect, the Connect
command round trip and the controller teardown. The anchor subsystem keeps the
listener up, so the TCP connect costs nothing. Measured across three values:

| `reconnect_delay` | measured | minus `reconnect_delay` |
|---|---|---|
| 2 (test B) | 3.31 / 3.34 / 3.36 s | 1.31 / 1.34 / 1.36 s |
| 5 (check run) | 6.64 s | 1.64 s |
| 10 (test A) | 11.52 s | 1.52 s |

The slope is exactly 1.0, confirming `reconnect_delay` is the only tunable in
play. `ctrl_loss_tmo` does not appear at all -- test A used 600 and test B used
10, and both land on the same line.

**Why the ANA matrix does not change it.** ANA state decides which controller
I/O is *issued to*; it has no influence on how fast a controller *dies*.
Scenario d kills a controller from the target side whether or not that path was
carrying any traffic, so in all three matrices both controllers are torn down at
the same instant (the two targets are faulted within 30-70 ms of each other) and
the head loses its last path at the same instant. What differs between the
matrices is only what the I/O was doing beforehand:

- **X**: I/O was on cn0, and would have failed over to cn1 -- but cn1 died too.
- **Y**: I/O was on cn1; cn0 was live-but-inaccessible. In rows e/g/h that
  survivor is what suppresses the error indefinitely, but scenario d deletes it
  as well, so nothing is left to requeue onto.
- **Z**: I/O had already been stalled for 10.5 s, since the ANA flip. The raw
  record shows this -- `t_first_stuck = -10.47` (the stall predates the fault)
  while `t_first_err = 3.36` is unchanged.

`(d,d)` is therefore one of the few cells where the ANA matrix is irrelevant,
and that is precisely because scenario d removes the controller. Every cell
where the ANA state *does* change the answer (rows e/g/h in Y and Z) is one
where some controller stays `live`.

## 5. Summary

| target fault | host outcome | why |
|---|---|---|
| g backend EIO | **error, immediately** | DNR Internal Error is a completion, not a path failure; never retried, never failed over |
| d subsystem off a still-listening port | **error in `reconnect_delay` + ~1.3 s** | Connect refused with DNR -> controller deleted on attempt 1, retry budget bypassed |
| f namespace disabled | **error, immediately** | AEN -> rescan -> namespace and head disk removed |
| b RST, c port not listening | **error after the full retry budget** (615 s stock) | transport error, retried `ctrl_loss_tmo/reconnect_delay` times, each retry ~`reconnect_delay` |
| a DROP | **error after the full retry budget + SYN timeouts** (809 s stock) | same, but each retry also burns nvme-tcp's ~3.4 s connect timeout |
| e host removed from `allowed_hosts` | **no error, ever** | nvmet checks `allowed_hosts` only at Connect; latent until the next reconnect, then instant DNR death |
| h backend suspended | **no error, ever — and no I/O either** | reconnects keep succeeding, so the retry counter keeps resetting and the controller is never deleted |

Four things worth carrying into a design:

1. **`ctrl_loss_tmo` is a retry count, not a deadline.** Stock 600/10 gives 615 s
   against an RST and 809 s against a black hole. Pick it from the retry count
   you want.
2. **Two classes of target fault produce opposite host behaviour.** A target
   that *answers with an error* (g) fails the application in milliseconds and
   defeats multipath. A target that *stops answering* (a/b/c) hangs the
   application for the whole retry budget. Neither is failover; only an actively
   torn-down connection (b/c/d on the unused path) gives clean, sub-3 s failover.
3. **A live-but-inaccessible ANA path suppresses I/O errors indefinitely.** It
   keeps `nvme_available_path()` true, so a subsystem with one inaccessible
   standby and one broken active path hangs forever instead of erroring. Any
   design that parks a standby in `inaccessible` must have its own liveness
   check; the host will not report a problem.
4. **A hung backend is the worst failure mode and no host timeout catches it.**
   Successful reconnects reset the retry counter, so scenario h hangs
   unboundedly with a healthy transport, and even a second healthy path only
   yields brief bursts of service because path selection keeps returning to the
   wedged controller. Detect and fence hung backends on the target side; the
   initiator cannot.

## 6. Reproducing

```sh
cd poc/ioerr
python3 run_test.py A                     # 8 lanes, stock timeouts, ~16 min
python3 run_test.py B --obs 60 --lanes 16 # 192 cases, 12 rounds, ~20 min
python3 report.py                         # re-render the tables above
bash run_e2.sh                            # the allowed_hosts latent-fault demo
python3 run_test.py cleanup
```

`cnctl.sh init` installs `/etc/udev/rules.d/58-dnv-ioerr.rules`, which sets the
`DM_UDEV_DISABLE_*` flags for `dm-*` devices. Without it, udev blkid-probes the
suspended dm device from scenario h and the udev worker wedges for the rest of
the run. (`OPTIONS+="ignore_device"` is obsolete and does not work.)
