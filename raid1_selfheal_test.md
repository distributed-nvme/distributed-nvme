# Self-healing a RAID1 over two NVMe-oF legs, without mdadm

Lab notes, 2026-08-26. `cn0` 192.168.122.125, `dn0` 192.168.122.48, `dn1` 192.168.122.70,
all Ubuntu 26.04 / Linux 7.0.0-29-generic. **`mdadm` is not installed on cn0**, so nothing
here can silently fall back to it — metadata is written by hand and everything else is sysfs
plus the one ioctl mdadm uses, as established in [`mdraid1_lowlevel.md`](mdraid1_lowlevel.md).

**Question.** When md kicks a leg, was the leg actually broken? When it comes back, is it
*safe* to put it back? And when we do put it back, can we avoid re-copying a whole device?

**Answer.** md's verdict is a *trigger*, never a diagnosis: a leg is re-admitted only after it
passes an independent read-after-write probe three times, and — when both legs were kicked —
only after its own superblock proves it was the last one standing. That much was already true.
What changed in this run is the repair itself. Every path back now aims at
`rdev->saved_raid_disk >= 0`, so **the write-intent bitmap is used even after a stop and
re-assembly**: the both-legs-down case that used to copy 1 GiB now copies 36 MiB in 0.21 s.
All of it is in [`poc/raid1heal/`](poc/raid1heal/).

---

## 1. The rig

```
        cn0  (initiator)                    dn0 / dn1  (targets)
  ┌───────────────────────────┐        ┌───────────────────────────────┐
  │ io_gen.py  (verified IO)  │        │  nvmet subsystem (tcp:4420)   │
  │        │                  │        │        nqn...dnv:dn0 / :dn1   │
  │  /dev/md0  raid1          │        │        │                      │
  │   ├─ nvme0n1 ── tcp ──────┼────────┼──► ns1 │                      │
  │   └─ nvme1n1 ── tcp ──────┼────────┼──► /dev/mapper/dnv-leg        │
  │                           │        │        │  (dm-linear)         │
  │ raid1_healer.py (60 s)    │        │        └─► /dev/loop0 ─► 1G file
  └───────────────────────────┘        └───────────────────────────────┘
                                          dm-linear is the fault injector:
                                          reload it as error / delay / flakey
```

Build it with [`poc/raid1heal/dn_setup.sh`](poc/raid1heal/dn_setup.sh) (on each target) and
[`poc/raid1heal/cn_setup.sh`](poc/raid1heal/cn_setup.sh) (on cn0). The rig is hard-coded in
[`rig.py`](poc/raid1heal/rig.py) — there is no config file and nothing is tunable at run time.

### 1.1 Targets

Each of dn0/dn1 gets a 1 GiB sparse file → loop → `dm-linear` named `dnv-leg` → an nvmet
subsystem over TCP with cn0 in `allowed_hosts` and a fixed namespace uuid, so cn0 always sees
the same `/dev/disk/by-id/nvme-uuid.…` path:

```bash
sudo truncate -s 1024M /var/tmp/dnv/dn0.img
LOOP=$(sudo losetup -f --show /var/tmp/dnv/dn0.img)
echo "0 2097152 linear $LOOP 0" | sudo dmsetup create dnv-leg

S=/sys/kernel/config/nvmet/subsystems/nqn.2026-08.org.dnv:dn0
sudo mkdir -p $S/namespaces/1
echo /dev/mapper/dnv-leg                        | sudo tee $S/namespaces/1/device_path
echo 11111111-1111-1111-1111-000000000000       | sudo tee $S/namespaces/1/device_uuid
echo 1                                          | sudo tee $S/namespaces/1/enable
sudo ln -s /sys/kernel/config/nvmet/hosts/nqn.2026-08.org.dnv:host:cn0 $S/allowed_hosts/
sudo ln -s $S /sys/kernel/config/nvmet/ports/1/subsystems/
```

The `dm-linear` layer exists only so the fault injector has something to reload. A udev rule
(`58-dnv-test.rules`, [`dn_setup.sh`](poc/raid1heal/dn_setup.sh)) suppresses blkid/bcache probes
on `dnv-*` devices — without it a `dm-delay` table parks udev workers in D state and they hold
the device open forever.

### 1.2 Initiator — and why the transport settings are not options

```bash
echo 10 | sudo tee /sys/module/nvme_core/parameters/io_timeout
sudo nvme connect -t tcp -a 192.168.122.48 -s 4420 -n nqn.2026-08.org.dnv:dn0 \
     --reconnect-delay=5 --ctrl-loss-tmo=-1 --fast_io_fail_tmo=5
```

| knob | value | why |
|---|---|---|
| `io_timeout` | 10 s | how long one command may hang before nvme resets the controller |
| `reconnect-delay` | 5 s | retry cadence once the transport is down |
| `ctrl-loss-tmo` | **−1 (off)** | never give up: the leg must be able to return on its own |
| `fast_io_fail_tmo` | **5 s** | transport down this long ⇒ commands error instead of queueing |
| md `failfast` | **always on** | a wedged-but-connected backend becomes an IO error, not a hang |

These three are a set, and §5.3 is the measurement behind them: with `ctrl_loss_tmo` off and
`fast_io_fail_tmo` off and md failfast off, a black-holed leg produces **no IO error at all**,
ever. `ctrl_loss_tmo=-1` is not negotiable — we need a leg to be able to come back by itself,
and exhausting the reconnect budget *deletes the controller*. So the other two carry the load.

### 1.3 The array

`mdsb.py` writes the v1.2 superblock and the internal-bitmap superblock; the kernel does the
rest. `data_offset` is deliberately huge:

```
sector      0–7      untouched
sector      8–15     mdp_superblock_1, feature_map=0x1, bitmap_offset=+8, devflags=FailFast1
sector     16–4111   internal bitmap (sectors_reserved=4096; ~4 KiB actually used)
sector  32760–32767  ── 4 KiB health-probe scratch slot ────────────────────
sector  32768–       array data (data_offset=32768 = 16 MiB, data_size=2064384s)
```

16 MiB of head room means the probe slot at `data_offset − 4096` can never collide with the
bitmap however the bitmap is resized.

Assembly is four sysfs writes; level, `raid_disks`, chunk and bitmap location are never set by
hand — `analyze_sbs()` → `super_1_validate()` fills them in from the metadata:

```bash
echo md0    | sudo tee /sys/module/md_mod/parameters/new_array
echo 1.2    | sudo tee /sys/block/md0/md/metadata_version   # MUST precede new_dev
echo 259:1  | sudo tee /sys/block/md0/md/new_dev
echo 259:3  | sudo tee /sys/block/md0/md/new_dev
echo active | sudo tee /sys/block/md0/md/array_state
```

```
md0 : active raid1 nvme1n1[1] nvme0n1[0]
      1032192 blocks super 1.2 [2/2] [UU]
      bitmap: 1/8 pages [4KB], 64KB chunk
array_state=clean degraded=0 fail_last_dev=1 bitmap/space=4096
dev-nvme0n1: slot=0 state=in_sync,failfast
dev-nvme1n1: slot=1 state=in_sync,failfast
```

`failfast` arrives on its own, from `devflags=FailFast1` in the metadata — `super_1_validate()`
reads it. `fail_last_dev=1` is written by `mdctl.tunables()`; it lets md kick the *second* leg
too rather than keeping a half-alive array, which is what makes the both-legs-down cases in
§5.2 reachable at all.

---

## 2. The healer

[`poc/raid1heal/raid1_healer.py`](poc/raid1heal/raid1_healer.py), run every 60 s on cn0.
One pass is: *ask md → distrust md → put the leg back the way mdadm would*.

### 2.1 Ask md

A leg is "bad" if its rdev is `faulty`, if it has no rdev at all, or if the array is not
assembled. Note that `array_state` does **not** reliably read `broken` when both legs are gone
— across the two runs in §5.2 it read `broken` once and plain `active` with `degraded=2` the
other time — so the healer keys off "how many rdevs are `in_sync`", which is zero in both.

### 2.2 Distrust md — health-check the leg

Three checks, in cost order ([`probe.py`](poc/raid1heal/probe.py)):

1. **NVMe-oF path.** `/sys/class/nvme/nvmeN/state` must be `live`, the namespace must have a
   block device, and `ana_state` must not be `inaccessible` — an ANA-inaccessible path keeps
   `nvme_available_path()` true and swallows IO forever without ever erroring.
2. **Write probe.** 4 KiB of a fresh random token, `O_DIRECT`, into the scratch slot at
   `data_offset − 4096` **on the leg itself**, not through the array. Must finish in 1 s.
3. **Read probe.** Read it straight back and compare byte-for-byte. Must finish in 1 s.

Check 3 is not redundant. A leg whose writes are silently discarded returns success to every
IO — md is structurally blind to it (§6.1) and so is any check that only asks "did the IO
complete?". Only the read-back comparison sees it.

The 1-second deadline is enforced by running each probe in a forked child and **never
waiting on it**. A child blocked on a wedged nvme queue is in uninterruptible D state; `SIGKILL`
will not land until the transport gives up, so `subprocess.run(timeout=…)` would block the
healer for the full `io_timeout`. The parent polls, kills the process group, and abandons the
child, reaping it non-blockingly on later passes.

Three rounds, 10 s apart. Any single failure condemns the leg — a leg that is flapping is not
a leg you want back.

### 2.3 Repair: three ways back in, and only one of them is a full copy

This is the part that changed, and it is the point of the whole run.

`raid1_add_disk()` decides between a bitmap-scoped recovery and copying the entire device on
exactly one field:

```c
/* As all devices are equivalent, we don't need a full recovery
 * if this was recently any drive of the array */
if (rdev->saved_raid_disk < 0)
        conf->fullsync = 1;
```

and `conf->fullsync` is what stops `raid1_sync_request()` skipping clean bitmap chunks. So the
whole question is which sysfs/ioctl path sets `saved_raid_disk`. mdadm tries two, in this order
(`Manage.c:1608`, then `attempt_re_add()`), and the healer now does the same:

| # | verb | when it works | sets `saved_raid_disk`? |
|---|---|---|---|
| 1 | `echo re-add > dev-*/state` | rdev still bound and `faulty` | yes — `remove_spares()` saved it when md kicked the leg |
| 2 | `ioctl(md_fd, ADD_NEW_DISK, …)` | rdev detached, or a fresh array after a re-assembly | yes — `super_1_validate()` sets it from the device's own role map |
| 3 | `echo <maj:min> > md/new_dev` | always | **no** — `new_dev_store()` never calls `validate_super()` at all |

Path 3 was the only hot-add the previous version of this POC had, and that single omission is
why it paid a full rebuild for every leg that did not come back through path 1. Measured
directly, same fault, same 12 s outage, on the 1 GiB array:

| verb | sectors copied | | wall time |
|---|---|---|---|
| `re-add` | 15,897 | 7.8 MiB | 0.110 s |
| `ADD_NEW_DISK` | 15,654 | 7.6 MiB | 0.122 s |
| `new_dev` | **2,065,020** | **1008 MiB** | **5.372 s** |

`data_size` is 2,064,384 sectors, so `new_dev` copies the array and then some (the extra is
metadata and concurrent application writes). **132× the data, 44× the time.**

#### Why `ADD_NEW_DISK` is safe as well as fast

`re-add` needs no safety check and mdadm makes none: while that rdev has been faulty the array
has been degraded, and `bitmap_endwrite()` only advances `events_cleared` when
`!mddev->degraded` — so the dirty-chunk list still covers the whole gap by construction.

`ADD_NEW_DISK` does need a check, because the device has been away from md's bookkeeping. The
kernel makes it itself, in `super_1_validate()`:

```c
} else if (mddev->bitmap) {
        /* If adding to array with a bitmap, then we can accept an
         * older device, but not too old. */
        if (ev1 < md_bitmap_events_cleared(mddev))
                return 0;                              /* raid_disk stays -1 */
        if (ev1 < mddev->events)
                set_bit(Bitmap_sync, &rdev->flags);
}
```

and `md_add_new_disk()` turns the refusal into a hard error rather than a silent full rebuild,
because we asserted `MD_DISK_SYNC` and a slot number:

```c
if ((info->state & (1<<MD_DISK_SYNC)) && rdev->raid_disk != info->raid_disk) {
        export_rdev(rdev, mddev);
        return -EINVAL;
}
```

So the ioctl either succeeds bitmap-scoped, or returns `EINVAL` and the healer falls back to
`new_dev`. It cannot quietly do the wrong thing. Proved in §5.4.

Two more things the ioctl gets right for free: it takes `MD_DISK_FAILFAST` in `disc.state`, so
a leg that comes back this way keeps its failfast flag (the old `new_dev` path silently lost
it — see §6.2), and it takes the slot number from the returning leg's **own** superblock, so a
leg can only ever go back into the slot it already claimed.

### 2.4 Both legs kicked — the ordering rule

This is the part that cannot be delegated to the kernel. With both legs down the healer stops
the array and re-assembles from metadata, and the decision is:

> Take the available leg with the highest `events`. If any member is **missing**, that leg's own
> `dev_roles[]` must already record the missing member as `faulty`. Otherwise refuse.

The reasoning: when md kicks leg A it writes the updated role map to the *survivors*. So B's
superblock records `0:faulty` and A's does not record anything about B. A superblock that still
believes its absent partner was `in_sync` was therefore written *before* that partner's last
write — it is stale by construction, and starting the array from it would silently roll the
data back.

Only the fresh legs are handed to `new_dev` at assembly; every other leg then goes through
§2.3, which now means it is re-admitted bitmap-scoped instead of rebuilt.

Stopping the array needs it closed, so the healer touches `/run/dnv-io-pause`; `io_gen.py`
drops its fd while that file exists.

---

## 3. Running it

Two systemd units on cn0 ([`cn_services.sh`](poc/raid1heal/cn_services.sh)):

```ini
ExecStart=/usr/bin/python3 /opt/dnv/raid1_healer.py     # dnv-healer, 60 s loop
ExecStart=/usr/bin/python3 /opt/dnv/io_gen.py           # dnv-iogen, 20 IOPS
```

`io_gen.py` is not just load. Every write records the token it wrote at that offset and every
read of a previously-written offset is compared against it, so the load generator doubles as the
correctness oracle: a wrong re-assembly shows up as `STALE DATA`, not as an IO error. That is
what caught §6.1.

---

## 4. Fault injection

[`poc/raid1heal/fault.sh`](poc/raid1heal/fault.sh) on the target. All of the dm cases reload the
same `dnv-leg` device, always with `suspend --nolockfs --noflush` first — a plain suspend tries
to flush pending IO through a leg that is about to start erroring.

```bash
fault.sh net-drop            # iptables -j DROP           from cn0
fault.sh net-reset           # iptables -j REJECT --reject-with tcp-reset
fault.sh suspend             # dmsetup suspend the leg
fault.sh delay 20000         # 0 N delay <dev> 0 20000
fault.sh flakey error_writes # 0 N flakey <dev> 0 0 1 1 error_writes
fault.sh flakey drop_writes  #                     1 drop_writes
fault.sh flakey error_reads  #                     1 error_reads
fault.sh flakey corrupt_read #                     5 corrupt_bio_byte 32 r 1 0
fault.sh error               # 0 N error
fault.sh heal / net-clear / resume
```

---

## 5. Results

`t_detect` = fault injected → md marks the leg faulty, sampled at 50 Hz **on cn0**
([`watch.py`](poc/raid1heal/watch.py)); an ssh round-trip is ~0.5 s, the same order as the
fastest cases, so it cannot be measured from the orchestrator. `t_recover` = fault cleared →
array back to `[2/2] [UU]`. `copied` = delta of field 7 of `/sys/block/<leg>/stat`, i.e. the
sectors md actually wrote onto the returning leg — the only unambiguous way to tell a
bitmap-scoped recovery from a full rebuild. Full data in
[`poc/raid1heal/results_*.json`](poc/raid1heal/).

### 5.1 One leg, one fault

| § | fault | md notices | t_detect | verb used | copied | t_recover |
|---|---|---|---|---|---|---|
| 4.1 | iptables `DROP` from cn0 | yes | 7.88 s | `re-add` | 40,534 | 34.0 s |
| 4.2 | iptables `tcp-reset` | yes | 2.43 s | `re-add` | 60,788 | 60.8 s |
| 4.3 | dm-linear suspended | yes | 12.74 s | `re-add` | 54,703 | 49.7 s |
| 4.4 | `dm-delay` 20 s (> `io_timeout` 10 s) | yes | 12.76 s | `re-add` | 56,534 | 52.0 s |
| 4.5a | `dm-flakey error_writes` | yes | 1.32 s | `re-add` | 61,285 | 63.2 s |
| 4.5b | `dm-flakey error_reads` | yes | 38.57 s | `re-add` | 36,761 | 25.1 s |
| 4.5c | `dm-flakey drop_writes` | **no** | — | — | — | — |
| 4.5d | `dm-flakey corrupt_bio_byte 32 r` | **no** | — | — | — | — |
| 4.5e | `dm-flakey` error both directions | yes | 1.11 s | `re-add` | 50,389 | 49.7 s |
| 4.x | `dm error` table (on dn0) | yes | 1.29 s | `re-add` | 60,380 | 60.7 s |

**Every fault md could see was healed automatically, with no manual intervention and no data
error reaching the application** — the surviving mirror absorbed all of it. Every repair copied
between 36 k and 61 k sectors (18–30 MiB) against a `data_size` of 2,064,384: the bitmap is
doing its job on every single-leg case, as it did before, because these all come back through
`re-add`.

Detection times are the interesting column. They are **2–3× faster than the 2026-08-24 run**,
which recorded 14.4 s / 9.0 s / 36.0 s / 35.8 s for rows 4.1–4.4. I have not isolated why, and
the two are not cleanly comparable: that run sampled md's state over ssh from the orchestrator
(~0.5–2 s granularity, and the harness has since been deleted), whereas these are sampled at
50 Hz on cn0. That accounts for a second or two, not twenty. The new numbers are at least
stable — the suspend case re-run three times gave 12.74 / 12.98 / 12.75 s, with
`dev-nvme1n1/state` reading `faulty,write_error,failfast` at the moment of the kick, so
failfast was definitely on. Treat the old column as superseded rather than explained.

What the numbers themselves say:

- **~1.3 s** whenever the target returns an error: the very next md write fails.
- **2.4 s** for `tcp-reset` — the RST kills the socket immediately.
- **7.9 s** for `DROP` — a keep-alive has to time out first.
- **12.7 s** for suspend and dm-delay: one `io_timeout` (10 s) plus error recovery, and with
  failfast on the cancelled command is *completed with an error* (`blk_noretry_request()` is
  true in `nvme_decide_disposition()`) instead of being handed to `nvme_failover_req()`. One
  timeout cycle, not three.
- **38.6 s** for `error_reads`, because RAID1 only reads from one leg at a time and md had to
  happen to send a read to dn1; writes were still succeeding.

### 5.2 Both legs fail — the case the bitmap used to be thrown away on

Fail dn0, wait 15 s so writes accumulate on dn1 alone, fail dn1, then heal them in one order or
the other. This is the scenario from the earlier run where the healer stopped the array,
re-assembled, and hot-added the stale leg with `new_dev` — a full 1 GiB copy every time.

**Reverse order** (A, B fail → B, A return). B was the last leg standing, so it may start the
array alone:

```
07:45:01   dn1 sb: events=596 utime=1787730253 role=1 roles=[0:faulty 1:1] dirty
07:45:01   assembly: GO -- freshest slot 1 events=596 roles=[0:faulty 1:1]; start from [1]
07:45:01   stop md0: stopped
07:45:01   assembled from slots [1] -> clean degraded=1
07:46:21   dn0: events=588 vs peer events_cleared=588 -> bitmap does cover the gap
07:46:21   dn0: ADD_NEW_DISK slot 0
07:46:21   dn0: back in 0.21s, md copied 74931 sectors onto it
```

**74,931 sectors — 36.6 MiB in 0.21 s.** The previous version of this POC hot-added the stale
leg here with `new_dev`, which on this rig copies the whole 2,064,384-sector device in ~5.4 s
(measured as the control in §2.3, and again in §5.4 when the gate forces that fallback).
Array back to `[2/2]` 80.9 s after the second leg healed, all of it healer-loop latency.

**Same order** (A, B fail → A, B return). A dropped out first, so its role map still says
`1:1` and it must be refused:

```
07:47:42   dn0 sb: events=654 utime=1787730388 role=0 roles=[0:0 1:1] dirty
07:47:42   assembly: WAIT -- slot 1 is absent, but the freshest leg we have (slot 0,
           events=654) still records it as role 1 -- that leg dropped out first and its
           data is stale; waiting
07:49:02   ... WAIT (same)
07:50:42   dn1 sb: events=668 utime=1787730404 role=1 roles=[0:faulty 1:1] dirty
07:50:42   assembly: GO -- freshest slot 1 events=668 roles=[0:faulty 1:1]; start from [1]
07:50:42   assembled from slots [1] -> clean degraded=1
07:50:42   dn0: events=654 vs peer events_cleared=654 -> bitmap does cover the gap
07:50:42   dn0: ADD_NEW_DISK slot 0
07:50:42   dn0: back in 0.11s, md copied 15641 sectors onto it
```

`refused_correctly=True`: it waited the full 200 s the test watched, across three passes, then
started from dn1 — the correct leg — and rebuilt dn0 *from dn1*, in 15,641 sectors. No
`STALE DATA`. Note `events=654` vs `events_cleared=654`: dn0's superblock is exactly as old as
the moment the array stopped being non-degraded, which is the boundary case the gate is
written for, and it passes.

### 5.3 What `failfast` is worth: an all-packets-dropped leg

Measured previously on dn1 with `iptables -j DROP`, varying md failfast and
`fast_io_fail_tmo` with `ctrl_loss_tmo` off. Retained here because it is the reason §1.2 is
fixed rather than configurable:

| md `failfast` | nvme `fast_io_fail_tmo` | md gets an IO error? |
|---|---|---|
| on  | 5 s | yes, **14.4 s** |
| on  | off | yes, **10.6 s** |
| off | 5 s | yes, **15.7 s** |
| off | off | **no — watched 300 s, still nothing** |

The last row: nothing is degraded, nothing is faulty, nothing errors — the array is simply
frozen in `md_write_start`, while `nvme nvme1: Failed reconnect attempt 35/-1` counts up.
Without failfast the cancelled command is a *path* error, so `nvme_decide_disposition()`
returns `FAILOVER`, `nvme_failover_req()` steals the bios onto the multipath head's requeue
list, and `nvme_ns_head_submit_bio()` only calls `bio_io_error()` when `nvme_available_path()`
is false — which it never is, because a controller reconnecting forever is `CONNECTING`
forever. Exactly two things break the loop: `fast_io_fail_tmo` expiring, or `ctrl_loss_tmo`
exhausting the reconnect budget and *deleting the controller*. We need the second not to
happen, so the first must be on.

`nvme_core.io_timeout` does not rescue it. `nvme_tcp_timeout()` on a `LIVE` controller returns
`BLK_EH_RESET_TIMER` — its job is "reset the controller", not "fail this IO" — and once
`nvme_failover_req()` has parked the bio on the multipath head's requeue list the bio is no
longer a request at all. The head is a bio-based gendisk with no `blk_mq` timeout. Counted over
100 s of black-holed traffic: exactly 1 timeout, 1 error recovery, 1 `no usable path -
requeuing I/O`, and **0** of the `no available path - failing I/O` that would call
`bio_io_error()`. `io_timeout` bounds how long a *command* may be outstanding; it does not
bound how long a *bio* may take.

Two things that make failfast safe to leave on permanently:

- raid1 only sets `MD_FAILFAST` on a bio when the target rdev is **not** the last working
  device (`!test_bit(LastDev, &rdev->flags)`), so it can never kick the final leg prematurely.
- Relying on `ctrl_loss_tmo` instead is worse than slow: at the stock 600/10 the budget runs
  ~790–810 s for an `iptables DROP`, and when it fires the controller is deleted — the
  namespace disappears and the leg cannot come back without a fresh `nvme connect`.

### 5.4 The `events_cleared` gate really fires

To prove the kernel refuses rather than silently mis-repairing, dn1 was failed, detached, and
its superblock's `events` rewritten to 1 — far below any `events_cleared` the peer can hold
([`mdsb.set_events()`](poc/raid1heal/mdsb.py)). The healer then ran normally:

```
07:52:03   dn1: healthy across all 3 rounds
07:52:03   dn1: events=1 vs peer events_cleared=674 -> bitmap does NOT cover the gap
07:52:03   dn1: ADD_NEW_DISK slot 1: EINVAL (bitmap no longer covers the gap)
07:52:03   dn1: new_dev (FULL rebuild)
07:52:08   dn1: back in 5.58s, md copied 2065009 sectors onto it
```

`EINVAL`, then the full rebuild — 2,065,009 sectors in 5.58 s, correct and slow, which is
exactly what should happen when the bitmap cannot be trusted. The healer's own userspace
comparison (the `does NOT cover the gap` line) is logged for visibility only; the decision is
the kernel's.

---

## 6. Findings

### 6.1 A silently-dropped write is unrecoverable, and RAID1 will spread it

Still the most important result here, and this run measured it properly.

Scenario 4.5c parks `drop_writes` on dn1 for 30 s. dm-flakey returns **success** for every
write and discards it. md sees no error, so both legs stay `in_sync`, the bitmap stays clean,
and no resync is ever scheduled. The legs are now silently divergent, and the damage stays
invisible while both legs are up because `read_balance()` may still serve those blocks from
dn0 — until it doesn't:

```
07:32:06 STALE DATA off=20398080: expected b'w00016870-off020398080-' got b'w00011137-off020398080-'
07:32:08 STALE DATA off=57700352: expected b'w00015921-off057700352-' got b'w00255705-blk00014087-w'
```

120 stale reads by the end of the matrix. The second line is worth a look: the token it got
back is in the *previous* run's format, i.e. that block had not been overwritten since the last
time this rig was used, and dn1 had quietly ignored every write to it since.

An explicit scrub is the only thing in this stack that can see it:

```
$ echo check  > /sys/block/md0/md/sync_action     # then wait for idle
mismatch_cnt=29184                                 # 14.25 MiB silently divergent
$ echo repair > /sys/block/md0/md/sync_action
$ echo check  > /sys/block/md0/md/sync_action
mismatch_cnt=0
```

The `repair` above restored correctness only by luck of ordering: raid1's repair copies the
**first** in-sync leg over the others, and here the good leg happened to be slot 0. md has no
way to know which side was lying. Had the dropping leg been slot 0, `repair` would have
propagated the damage to the good leg with equal confidence.

Nothing in this stack can prevent that. md v1.2 has no data checksums, the write-intent bitmap
records *intent* and not *content*, and `check` reports a count without an opinion. The only
defences are (a) the read-after-write probe in §2.2, which does catch `drop_writes` directly,
and (b) integrity metadata below md (`dm-integrity`, T10 DIF/DIX) so a lying device cannot stay
silent. Worth deciding deliberately for the product, not inheriting by default.

### 6.2 `new_dev` drops `failfast`; `ADD_NEW_DISK` does not

`super_1_validate()` sets `FailFast` from the superblock's `devflags`, but `new_dev_store()`
never calls it, so a leg that came back through the old hot-add path ended up `in_sync` with no
`failfast`. That is not cosmetic — §5.3 shows failfast is what makes a wedged-but-connected backend
produce an IO error at all rather than hanging, so a leg that lost the flag would silently
regress to "hangs instead of errors" after its first repair.

`md_add_new_disk()` reads the flag straight out of `disc.state`:

```c
if (info->state & (1<<MD_DISK_FAILFAST))
        set_bit(FailFast, &rdev->flags);
```

so [`mdctl.add_new_disk()`](poc/raid1heal/mdctl.py) passes
`MD_DISK_ACTIVE|MD_DISK_SYNC|MD_DISK_FAILFAST` and the flag survives. `mdctl.tunables()` still
re-applies `fail_last_dev` and per-rdev `failfast` at the top of every pass, because the
`new_dev` fallback in §5.4 does still lose it.

### 6.3 dm-flakey gotchas

- The `<num_features>` count includes the **feature name as well as its arguments**.
  `corrupt_bio_byte 32 r 1 0` is *five* tokens, so the table must say `5 corrupt_bio_byte 32 r 1 0`;
  writing `4` gets the whole table rejected with a bare `Invalid argument`.
- `random_read_corrupt` / `random_write_corrupt` are **no-ops on this kernel** at every
  probability tried (1, 2, 100), verified on an isolated scratch device: 0/20 reads altered.
  `corrupt_bio_byte 32 r 1 0` corrupts 20/20 under the identical table, so use that when you
  want deterministic silent corruption.
- `up_interval=0 down_interval=1` keeps the device permanently in the "down" interval, which is
  what you want for a steady fault rather than a flapping one. With no features that is
  normalised by the kernel to `error_reads error_writes`.

### 6.4 Smaller ones

- **`array_state` is not a reliable "both legs are gone" signal.** With `fail_last_dev=1` it
  read `broken` in §5.2's first run and plain `active` (with `degraded=2`) in the second, in the
  same session. Count `in_sync` rdevs instead.
- **Never `wait()` on a probe child.** It can be in D state on a wedged nvme queue where
  `SIGKILL` does not land until the transport gives up, so `subprocess.run(timeout=…)` blocks
  the caller for the full `io_timeout` — exactly when the caller most needs to stay responsive.
- **`calc_sb_1_csum()` zeroes `sb_csum` before summing.** Verifying a superblock without zeroing
  that field first rejects every valid superblock.
- **Read the superblock with `O_DIRECT`.** md writes reach the leg as bios; the block device's
  own page cache will happily hand back a pre-failure copy.
- **Both test VMs answer `hostname -s` with `ubuntu2604b`**, so anything that derives a per-node
  path from the hostname silently collides. `fault.sh` takes the backing device out of the saved
  dm table instead.
- `ctrl-loss-tmo=-1` is spelled `--ctrl-loss-tmo` but fast-io-fail is `--fast_io_fail_tmo`
  (underscores) in nvme-cli 2.16.
- **Only the orchestrator has ssh keys to all three VMs.** cn0 cannot ssh to dn0/dn1, so any
  test harness that injects a fault from cn0 fails silently and looks like "md never noticed".

---

## 7. Reproducing

```bash
# targets
scp poc/raid1heal/{dn_setup.sh,fault.sh} dn0:/tmp/ && ssh dn0 'bash /tmp/dn_setup.sh dn0'
scp poc/raid1heal/{dn_setup.sh,fault.sh} dn1:/tmp/ && ssh dn1 'bash /tmp/dn_setup.sh dn1'
# initiator
ssh cn0 'sudo mkdir -p /opt/dnv'
scp poc/raid1heal/{rig,mdsb,mdctl,probe,raid1_healer,io_gen,snapshot,watch,check_stale}.py cn0:/opt/dnv/
scp poc/raid1heal/lowlevel_demo.py cn0:/opt/dnv/     # optional, for mdraid1_lowlevel.md Part 5
scp poc/raid1heal/{cn_setup.sh,cn_services.sh} cn0:/tmp/
ssh cn0 'bash /tmp/cn_setup.sh && bash /tmp/cn_services.sh'
# scenarios (from the orchestrator, which is the only host with keys to all three)
python3 poc/raid1heal/scenarios.py single    # 5.1  -> results_single.json
python3 poc/raid1heal/scenarios.py double    # 5.2  -> results_double.json
python3 poc/raid1heal/scenarios.py gate      # 5.4  -> results_gate.json
# and afterwards
bash poc/raid1heal/teardown.sh
```

Teardown order matters: defuse every dm fault **on the targets first**. A stalled backend leaves
an nvmet request outstanding, and then namespace disable, port unlink and controller teardown all
block forever.

If you run `single` and then `double` back to back, scrub between them — scenario 4.5c leaves
the legs genuinely divergent (§6.1), and every later scenario inherits it.
