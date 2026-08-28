# mdadm RAID1 single-leg assembly: stale-mirror rejection

Lab notes, 2026-08-14. Host: `192.168.122.125` (`ubuntu2604b`), Linux 7.0.0-29-generic,
`mdadm - v4.5 - 2025-12-16 - Ubuntu 4.5-5ubuntu1`.

**Question.** For a 2-leg RAID1 where only one leg is available, we want to *approve*
degraded assembly if that leg's superblock records the peer as already failed, and
*reject* it if the superblock still records both legs as healthy (meaning this leg may
be a stale mirror half). Does mdadm enforce this on its own, and if so how do we make
sure it actually does?

**Answer.** mdadm implements exactly this rule, and it is the default — but only on the
explicit-device-list `--assemble` path. `--run`, `--force`, `--scan`, and the systemd
last-resort timer all bypass it. See
[How to guarantee the rejection](#how-to-guarantee-the-rejection).

---

## Part 1 — Reproducing a stale mirror half

### 1. Two 1 GiB zero-filled backing files

```bash
mkdir -p ~/rtest && cd ~/rtest
truncate -s 1G disk0.img          # sparse => reads as all zeros
truncate -s 1G disk1.img
```

### 2. Loop devices

```bash
sudo losetup -f --show disk0.img  # -> /dev/loop0
sudo losetup -f --show disk1.img  # -> /dev/loop1
sudo blockdev --getsz /dev/loop0  # 2097152 sectors
```

### 3. dm-linear on top of each loop device

```bash
echo "0 2097152 linear /dev/loop0 0" | sudo dmsetup create rt_l0   # 252:0
echo "0 2097152 linear /dev/loop1 0" | sudo dmsetup create rt_l1   # 252:1
```

```
$ sudo dmsetup table rt_l0
0 2097152 linear 7:0 0
$ sudo dmsetup table rt_l1
0 2097152 linear 7:1 0
```

### 4. RAID1 across the two dm-linear devices

```bash
sudo mdadm --create /dev/md/rtest --level=1 --raid-devices=2 \
    --assume-clean --bitmap=internal --bitmap-chunk=4M \
    --data-offset=12M --metadata=1.2 --run \
    /dev/mapper/rt_l0 /dev/mapper/rt_l1
```

```
mdadm: array /dev/md/rtest started.

md127 : active raid1 dm-1[1] dm-0[0]
      1036288 blocks super 1.2 [2/2] [UU]
      bitmap: 1/1 pages [4KB], 4096KB chunk
```

`mdadm --detail` confirms `State : clean`, `Active Devices : 2`, both legs
`active sync`.

### 5. Write block 0, verify it lands at 12 MiB on both loop devices

`--data-offset=12M` means RAID data LBA 0 maps to byte 12 MiB of each member, so
member offset `12M` = `4096 * 3072`.

```bash
printf "HELLO-RAID1-TEST-PATTERN-0001" > /tmp/pat.bin
truncate -s 4096 /tmp/pat.bin
sudo dd if=/tmp/pat.bin of=/dev/md/rtest bs=4096 count=1 conv=fsync
sync
sudo blockdev --flushbufs /dev/md/rtest /dev/mapper/rt_l0 /dev/mapper/rt_l1 \
                          /dev/loop0 /dev/loop1

sudo dd if=/dev/loop0 bs=4096 skip=3072 count=1 | md5sum
sudo dd if=/dev/loop1 bs=4096 skip=3072 count=1 | md5sum
md5sum /tmp/pat.bin
```

All three identical:

```
c3764002905f93fcd6f1943eba6eb112  -                 # loop0 @ 12M
c3764002905f93fcd6f1943eba6eb112  -                 # loop1 @ 12M
c3764002905f93fcd6f1943eba6eb112  /tmp/pat.bin
```

Plain-text confirmation of the same bytes:

```
--- read back from md ---   HELLO-RAID1-TEST-PATTERN-000
--- loop0 @12M ---          HELLO-RAID1-TEST-PATTERN-000
--- loop1 @12M ---          HELLO-RAID1-TEST-PATTERN-000
```

> Note: do **not** use `dd iflag=direct` / `oflag=direct` on these VMs — the shipped
> uutils `dd` mishandles them (false failures and silent zero-writes). `conv=fsync` +
> `blockdev --flushbufs` is the reliable pattern.

### 6. Swap `rt_l1` to a dm-error table

```bash
sudo dmsetup suspend --noflush rt_l1
echo "0 2097152 error" | sudo dmsetup load rt_l1
sudo dmsetup resume rt_l1
sudo dmsetup table rt_l1     # 0 2097152 error
```

### 7. Write again so md notices the failure

```bash
printf "SECOND-WRITE-AFTER-ERROR-XXXX" > /tmp/pat2.bin
truncate -s 4096 /tmp/pat2.bin
sudo dd if=/tmp/pat2.bin of=/dev/md/rtest bs=4096 count=1 seek=100 conv=fsync
sync
```

```
md127 : active raid1 dm-1[1](F) dm-0[0]
      1036288 blocks super 1.2 [2/1] [U_]

State : clean, degraded
Failed Devices : 1
   1     252        1        -      faulty   /dev/dm-1
```

### 8. Stop the array

```bash
sudo mdadm --stop /dev/md/rtest
```

### 9. Restore `rt_l1` to dm-linear

```bash
sudo dmsetup suspend --noflush rt_l1
echo "0 2097152 linear /dev/loop1 0" | sudo dmsetup load rt_l1
sudo dmsetup resume rt_l1
```

### 10. The resulting superblock divergence

```bash
sudo mdadm --examine /dev/mapper/rt_l0
sudo mdadm --examine /dev/mapper/rt_l1
```

| device | Events | Update Time | Array State |
|---|---|---|---|
| `rt_l0` (up to date) | **6** | 06:47:13 | `A.` |
| `rt_l1` (stale)      | **2** | 06:46:59 | `AA` |

`rt_l1` was already erroring when md tried to record the failure, so it never received
the update. **It still believes both legs are alive.** That is the exact condition we
want to reject.

---

## Part 2 — Does mdadm reject the stale leg?

### First result: yes, by default — but only on one path

```
$ sudo mdadm --assemble /dev/md127 /dev/mapper/rt_l0     # Array State "A."
mdadm: /dev/md127 has been started with 1 drive (out of 2).
exit=0

$ sudo mdadm --assemble /dev/md127 /dev/mapper/rt_l1     # Array State "AA"
mdadm: /dev/md127 assembled from 1 drive - need all 2 to start it (use --run to insist).
exit=1
```

Data proves the stakes. Assembled from `rt_l0`, both writes are present. Forced up from
`rt_l1`, the array silently rolls back:

```
from rt_l0:  offset 0 = HELLO-RAID1-TEST-PATTERN-0001
             offset 400K = SECOND-WRITE-AFTER-ERROR-XXXX     # correct

from rt_l1:  offset 0 = HELLO-RAID1-TEST-PATTERN-0001
             offset 400K = <all zeros>                       # post-failure write LOST
             Events : 2
```

### Testing methodology note

The first run of this matrix produced garbage (`is busy - skipping`,
`cannot re-read metadata`). Cause: `/usr/lib/udev/rules.d/64-md-raid-assembly.rules`
runs `mdadm --incremental` on every device that appears, so udev was auto-assembling an
inactive `md127` holding the leg before the test command ran. All results below were
taken with auto-assembly suppressed:

```bash
sudo cp /etc/mdadm/mdadm.conf /etc/mdadm/mdadm.conf.bak
echo "AUTO -all" | sudo tee -a /etc/mdadm/mdadm.conf
# ... and between every test:
sudo udevadm settle; sudo mdadm --stop --scan
```

### The mechanism

From mdadm's `Assemble.c`, `start_array()`:

```c
req_cnt = content->array.working_disks;

if (c->runstop == 1 ||
    (c->runstop <= 0 &&
     (enough(content->array.level, content->array.raid_disks,
             content->array.layout, clean, avail) &&
      (okcnt + rebuilding_cnt >= req_cnt || start_partial_ok))))
```

`working_disks` is derived from the 1.x superblock's `dev_roles` table — it is exactly
the number of `A`/`R` characters printed on the `Array State` line. So `req_cnt` means
*"how many legs did this superblock last believe were healthy"*, and the gate
`okcnt >= req_cnt` says *"refuse unless I have at least that many"*.

That **is** the rule we wanted, already implemented.

### Proof that `req_cnt` comes from the superblock, not from `raid_disks`

Built a 3-leg RAID1 (same parameters) and failed a different number of peers, always
assembling from the single survivor `rt_m0`:

```
survivor rt_m0: Events=4  ArrayState=AA.        # one peer failed
$ mdadm --assemble /dev/md200 /dev/mapper/rt_m0
    mdadm: /dev/md200 assembled from 1 drive - need 2 to start (use --run to insist).

survivor rt_m0: Events=8  ArrayState=A..        # two peers failed
$ mdadm --assemble /dev/md200 /dev/mapper/rt_m0
    mdadm: /dev/md200 has been started with 1 drive (out of 3).
md200 : active raid1 dm-2[0]
      1036288 blocks super 1.2 [3/1] [U__]
```

"need **2** to start", not "need all 3". The threshold tracks the `A` count in the
survivor's own superblock, one for one.

### Full 2-leg result matrix

| scenario | survivor's Array State | command | result |
|---|---|---|---|
| peer failed | `A.` | `--assemble /dev/md127 /dev/mapper/rt_l0` | started, exit 0 ✅ |
| stale leg | `AA` | `--assemble /dev/md127 /dev/mapper/rt_l1` | `need all 2 to start it`, exit 1 ✅ |
| clean stop, leg 0 | `AA` | `--assemble /dev/md127 /dev/mapper/rt_l0` | `need all 2 to start it`, exit 1 ✅ |
| clean stop, leg 1 | `AA` | `--assemble /dev/md127 /dev/mapper/rt_l1` | `need all 2 to start it`, exit 1 ✅ |

The "clean stop" rows matter: after a clean shutdown with both legs healthy, *neither*
leg can be assembled alone. Correct — with both legs believing the peer is fine, mdadm
has no basis to pick a winner.

---

## How to guarantee the rejection

> **This is the operationally important part.** The rule holds on exactly one code path.
> Four common invocations disable it, and one of them runs automatically at boot.

The gate is skipped whenever `start_partial_ok` is set:

```c
start_partial_ok = (c->runstop >= 0) && (c->force || devlist == NULL || auto_assem);
```

Measured against the stale `AA` leg (`rt_l1`), which must never come up alone:

| invocation | outcome | why |
|---|---|---|
| `mdadm --assemble /dev/md127 /dev/mapper/rt_l1` | **rejected** ✅ | the only safe form |
| `mdadm --assemble --run …` | started from stale leg ❌ | `runstop == 1` short-circuits everything |
| `mdadm --assemble --force …` | started from stale leg ❌ | `force` sets `start_partial_ok` |
| `mdadm --assemble --scan` | started from stale leg ❌ | `devlist == NULL` sets `start_partial_ok` |
| `mdadm -I /dev/mapper/rt_l1` | `not enough to start safely` ✅ | incremental path is safe on its own |
| `mdadm -I …` then `mdadm -IRs` | `started array /dev/md/rtest` ❌ | `-R` forces the run |

Verbatim, the two that surprise people most:

```
$ sudo mdadm --assemble --scan --config=<conf exposing only rt_l1>
mdadm: /dev/md/rtest has been started with 1 drive (out of 2).
md127 : active raid1 dm-1[1]  1036288 blocks super 1.2 [2/1] [_U]

$ sudo mdadm --incremental /dev/mapper/rt_l1
mdadm: /dev/mapper/rt_l1 attached to /dev/md/rtest, not enough to start safely.
$ sudo mdadm -IRs
mdadm: started array /dev/md/rtest
md127 : active (auto-read-only) raid1 dm-1[1]  1036288 blocks super 1.2 [2/1] [_U]
```

### Rules to follow

1. **Always pass an explicit device list.** `mdadm --assemble /dev/mdX /dev/leg` — never
   `--assemble --scan`. `--scan` leaves `devlist == NULL`, which sets `start_partial_ok`
   and disables the check entirely. This is the form most distro boot scripts use.
2. **Never pass `--run` / `-R`.** It short-circuits the whole condition
   (`c->runstop == 1`) before `req_cnt` is even consulted.
3. **Never pass `--force` / `-f`.** It sets `start_partial_ok`, and it additionally
   rewrites superblocks, destroying the very evidence the rule depends on.
4. **Mask the systemd last-resort units.** `mdadm -I` behaves correctly, but
   `mdadm-last-resort@.timer` fires ~30 s after a member appears and runs `mdadm -IRs`,
   force-starting the degraded array from whatever it has — including the stale leg:

   ```bash
   sudo systemctl mask mdadm-last-resort@.timer mdadm-last-resort@.service
   ```

5. **Constrain auto-assembly.** `auto_assem` also sets `start_partial_ok`. Keep
   `AUTO -all` in `/etc/mdadm/mdadm.conf` and assemble deliberately from your own
   control plane, rather than letting the udev rule
   (`64-md-raid-assembly.rules`) drive it.
6. **Check `mdadm`'s exit code, not just its output.** Rejection is exit 1; a successful
   degraded start is exit 0.

Follow all six and mdadm enforces the policy for you, with no extra tooling. Miss any
one and a stale mirror half can silently come online and roll the volume back.

---

## Reading the health status out of the superblock

If you want to make the decision yourself before ever invoking `--assemble`:

**`--examine --export` does not expose it.** Complete output on our survivor:

```
MD_LEVEL=raid1
MD_DEVICES=2
MD_NAME=ubuntu2604b:rtest
MD_ARRAY_SIZE=1061.16MB
MD_UUID=97f20e66:8f8cd1ad:db4717e0:fdf39585
MD_UPDATE_TIME=1786691025
MD_DEV_UUID=5bd09f90:1a25d5cc:2ad907db:4e0e3be1
MD_EVENTS=5
```

No per-leg state. The only mdadm-provided source is the human-readable line:

```
$ sudo mdadm --examine /dev/mapper/rt_l0
   Raid Devices : 2
          State : clean
         Events : 5
   Device Role : Active device 0
   Array State : A. ('A' == active, '.' == missing, 'R' == replacing)
```

One character per raid slot, in slot order. `mdadm --examine` exits 1 when the device
carries no md superblock.

### `raid1-gate.sh`

Exit 0 = approve, 1 = reject, 2 = error.

```bash
#!/bin/bash
set -u
DEV="${1:?usage: raid1-gate.sh <device>}"

EX=$(mdadm --examine "$DEV" 2>&1) || { echo "ERROR: no md superblock on $DEV"; exit 2; }

LEVEL=$(printf '%s\n' "$EX" | awk -F': *' '/^ *Raid Level/{print $2}')
[ "$LEVEL" = "raid1" ] || { echo "ERROR: $DEV is $LEVEL, not raid1"; exit 2; }

STATE=$(printf '%s\n'  "$EX" | awk -F': *' '/^ *Array State/{print $2}' | awk '{print $1}')
ROLE=$(printf '%s\n'   "$EX" | awk -F': *' '/^ *Device Role/{print $2}')
EVENTS=$(printf '%s\n' "$EX" | awk -F': *' '/^ *Events/{print $2}')
[ -n "$STATE" ] || { echo "ERROR: could not read Array State from $DEV"; exit 2; }

ACTIVE=$(printf '%s' "$STATE" | tr -cd 'AR' | wc -c)   # 'R' == replacing, counts as present

echo "device      : $DEV"
echo "role        : $ROLE"
echo "events      : $EVENTS"
echo "array state : $STATE   (active legs recorded: $ACTIVE)"

if [ "$ACTIVE" -le 1 ]; then
    echo "VERDICT     : APPROVE - superblock records the peer as failed/missing;"
    echo "              this leg is the authoritative survivor."
    exit 0
else
    echo "VERDICT     : REJECT - superblock still records $ACTIVE healthy legs;"
    echo "              this leg may be stale. Do not assemble degraded."
    exit 1
fi
```

Tested:

```
=== gate on rt_l0 (survivor, A.) ===
array state : A.   (active legs recorded: 1)
VERDICT     : APPROVE ...                                     gate exit=0

=== gate on rt_l1 (stale, AA) ===
array state : AA   (active legs recorded: 2)
VERDICT     : REJECT - superblock still records 2 healthy legs ...   gate exit=1

=== gate on blank device ===
ERROR: no md superblock on /dev/loop5                          gate exit=2
```

---

## Caveats on the rule itself

**It cannot detect split-brain.** If leg A and leg B were each run degraded separately
at different times, A records `A.` and B records `.A`, and *both* individually pass the
gate while holding divergent data. A single superblock carries no information that can
catch this. Detecting it requires comparing `MD_EVENTS` (plus `MD_UUID`) across both
legs whenever both are reachable — which is what mdadm does natively when you hand it
both devices.

**The gate is not a freshness test.** Once you approve and assemble from an `A.` leg,
that leg keeps recording `A.` forever, so every later check approves it too. The rule
answers "is this leg a legitimate survivor?", not "is this leg currently up to date?".

**`--force` destroys the evidence.** Beyond bypassing the check, `--force` rewrites the
superblocks it examines. After one `--force` run there is no longer a reliable `AA`
marker to reject on.

---

## Artifacts

Scripts left on `192.168.122.125` under `/home/yupeng/rtest/`:

| file | purpose |
|---|---|
| `setup.sh [failed\|clean]` | rebuild the 2-leg scenario; `failed` → `rt_l0=A.`, `rt_l1=AA`; `clean` → both `AA` |
| `setup3.sh <1\|2>` | rebuild the 3-leg scenario with N failed peers |
| `test2.sh` | full result matrix (scenarios A/B/C above) |
| `raid1-gate.sh <dev>` | the approve/reject gate |

`/etc/mdadm/mdadm.conf` was restored from `mdadm.conf.bak` after testing; the temporary
`AUTO -all` line is gone and all test arrays are stopped.

### Teardown

```bash
sudo mdadm --stop --scan
sudo dmsetup remove rt_l0 rt_l1 rt_m0 rt_m1 rt_m2
sudo losetup -D
rm -rf ~/rtest
```
