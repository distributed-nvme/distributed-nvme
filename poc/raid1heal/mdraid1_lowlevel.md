# Driving a RAID1 without mdadm: metadata by hand, everything else via sysfs

Lab notes, 2026-08-22. Host: `192.168.122.125` (`ubuntu2604b`), Linux 7.0.0-29-generic.
**`mdadm` is not installed on that host**, so nothing here can silently fall back to it.

**Question.** Can we create, monitor, fail and repair a RAID1 purely through the kernel
interfaces (`/sys/module/md_mod/parameters/new_array`, `/sys/block/md0/md/*`,
`/proc/mdstat`), with an internal write-intent bitmap?

**Answer.** Yes for everything except *creating the metadata* and *one repair path*.
md never invents a superblock: `new_dev` runs `md_import_device()` → `super_1_load()`,
which rejects any device without a valid v1.x superblock — at create time *and* on every
hot-add. So a zeroed disk can never be handed to the kernel directly. Writing the ~4 KiB
of metadata is userspace's job (it is essentially the only thing `mdadm --create` does
that sysfs cannot). Assembly, status, failure handling and teardown are all plain sysfs
writes.

The exception is putting a leg *back*. There are three ways, and **only two of them use the
write-intent bitmap** — the third re-copies the whole device. One of those two is a sysfs
write; the other is the `ADD_NEW_DISK` ioctl, and it has no sysfs equivalent that is safe to
use. Part 5 measures all three.

The helpers used below now live in the POC rather than being pasted here, so they cannot
drift: [`poc/raid1heal/mdsb.py`](poc/raid1heal/mdsb.py) writes and decodes v1.2 metadata,
[`poc/raid1heal/mdctl.py`](poc/raid1heal/mdctl.py) holds the three ways back in.
[Appendix A](#appendix-a--the-add_new_disk-call) quotes the ioctl in full, because it is the
one piece with no sysfs equivalent.

---

## Part 0 — Test rig

file → loop → dm-linear. The dm layer is the failure injector: swapping the
`linear` target for `error` makes the leg return `-EIO` for every request.

```bash
sudo modprobe raid1

mkdir -p /var/tmp/raid1demo
sudo truncate -s 1G /var/tmp/raid1demo/disk0.img      # sparse => reads as all zeros
sudo truncate -s 1G /var/tmp/raid1demo/disk1.img
sudo losetup -f --show /var/tmp/raid1demo/disk0.img   # -> /dev/loop0
sudo losetup -f --show /var/tmp/raid1demo/disk1.img   # -> /dev/loop1
sudo blockdev --getsz /dev/loop0                      # 2097152 sectors

echo '0 2097152 linear /dev/loop0 0' | sudo dmsetup create leg0    # 252:0
echo '0 2097152 linear /dev/loop1 0' | sudo dmsetup create leg1    # 252:1
sudo dmsetup ls
#   leg0	(252:0)
#   leg1	(252:1)
```

## Part 1 — Lay down v1.2 metadata + internal bitmap (userspace)

```bash
sudo python3 mdsb.py --create /dev/mapper/leg0 /dev/mapper/leg1
#   {'uuid': 'd6d366f8-1c4d-47ff-a9b5-f16a96905562', 'data_size': 2095104}
sudo python3 mdsb.py /dev/mapper/leg0 /dev/mapper/leg1
#   /dev/mapper/leg0: events=0 utime=... role=0 roles=[0:0 1:1]
#       bitmap: events=0 events_cleared=0
```

On-disk layout written per leg (1 GiB device):

| sectors | contents |
|---|---|
| 0–7 | untouched |
| 8–15 | `mdp_superblock_1` (4 KiB), `feature_map=0x1` (`MD_FEATURE_BITMAP_OFFSET`), `bitmap_offset=+8` |
| 16–2047 | internal bitmap: 256-byte `bitmap_super_t` (magic `bitm`, version 4, 64 KiB chunk) + bits, all zero |
| 2048– | data (`data_offset=2048`, `data_size=2095104`) |

Key field choices:

- `events = 0`, identical `set_uuid` on both legs, `dev_roles[] = {0, 1}`.
- `resync_offset = MaxSector` — "assume clean", so the array starts with **no** initial
  resync. Legal only because both disks are zero-filled and therefore already identical.
- `sb_csum` = fold-to-32-bit sum of the first `256 + max_dev*2` bytes with the csum
  field zeroed (`calc_sb_1_csum()`).
- `bblog_offset = 0` → per-device bad-block log disabled, so any media error kicks the
  whole leg instead of being recorded. Simpler, and what we want for failure testing.

## Part 2 — Assemble through sysfs

```bash
echo md0    | sudo tee /sys/module/md_mod/parameters/new_array   # creates /dev/md0 (9:0)
echo 1.2    | sudo tee /sys/block/md0/md/metadata_version        # MUST precede new_dev
echo 252:0  | sudo tee /sys/block/md0/md/new_dev
echo 252:1  | sudo tee /sys/block/md0/md/new_dev
echo active | sudo tee /sys/block/md0/md/array_state             # runs the array
```

Level, `raid_disks`, chunk and bitmap location are **not** set by hand — `analyze_sbs()`
→ `super_1_validate()` fills them in from the superblocks:

```
$ cat /proc/mdstat
Personalities : [raid1]
md0 : active raid1 dm-1[1] dm-0[0]
      1047552 blocks super 1.2 [2/2] [UU]
      bitmap: 0/8 pages [0KB], 64KB chunk

$ grep . /sys/block/md0/md/{array_state,degraded,raid_disks,level,resync_start} \
         /sys/block/md0/md/bitmap/{location,chunksize,metadata,space}
/sys/block/md0/md/array_state:clean
/sys/block/md0/md/degraded:0
/sys/block/md0/md/raid_disks:2
/sys/block/md0/md/level:raid1
/sys/block/md0/md/resync_start:none
/sys/block/md0/md/bitmap/location:+8
/sys/block/md0/md/bitmap/chunksize:65536
/sys/block/md0/md/bitmap/metadata:internal
/sys/block/md0/md/bitmap/space:2032
```

`array_state` accepts `clear|inactive|readonly|read-auto|clean|active`; `active` on a
not-yet-running array is what performs `do_md_run()`.

Then some data:

```bash
sudo dd if=/dev/urandom of=/dev/md0 bs=1M count=64 conv=fsync
sudo blockdev --flushbufs /dev/md0; sudo md5sum /dev/md0
```

## Part 3 — One disk fails

```bash
sudo dmsetup suspend --nolockfs --noflush leg1        # requeue in-flight IO, don't flush
sudo dmsetup load    leg1 --table '0 2097152 error'
sudo dmsetup resume  leg1
sudo dd if=/dev/urandom of=/dev/md0 bs=1M count=4 conv=fsync   # IO is what makes md notice
```

`--nolockfs --noflush` matters: a plain suspend tries to flush pending IO through a leg
that is about to start erroring.

### Status: how many failed, and which one

```bash
$ cat /proc/mdstat
md0 : active raid1 dm-1[1](F) dm-0[0]
      1047552 blocks super 1.2 [2/1] [U_]       # 2 slots, 1 working
      bitmap: 1/8 pages [4KB], 64KB chunk       # dirty regions accumulating

$ cat /sys/block/md0/md/degraded                # 1  <- how many are gone
$ cat /sys/block/md0/md/array_state             # clean (array still writable)

$ for d in /sys/block/md0/md/dev-*; do \
    echo "$d slot=$(cat $d/slot) state=$(cat $d/state) errors=$(cat $d/errors)"; done
/sys/block/md0/md/dev-dm-0 slot=0    state=in_sync errors=0
/sys/block/md0/md/dev-dm-1 slot=none state=faulty  errors=0     # <- which one

$ ls -l /sys/block/md0/md/rd?                   # rdN symlink per *occupied* slot
lrwxrwxrwx ... /sys/block/md0/md/rd0 -> dev-dm-0                # rd1 is gone

$ cat /sys/block/md0/md/dev-dm-1/block/dev      # 252:1 -> back to the real device
$ dmesg | tail -3
md: super_written gets error=-5
md/raid1:md0: Disk failure on dm-1, disabling device.
md/raid1:md0: Operation continuing on 1 devices.
```

Other status files worth knowing: `dev-*/recovery_start`, `dev-*/bad_blocks`,
`md/sync_action`, `md/sync_completed`, `md/sync_speed`, `md/mismatch_cnt`,
`md/last_sync_action`. Note `dev-*/errors` counts *corrected read* errors, not IO
failures — it stays 0 through all of this.

## Part 4 — Both disks fail

Default `fail_last_dev=0`: md refuses to kick the last member and marks the array
broken instead.

```bash
sudo dmsetup suspend --nolockfs --noflush leg0
sudo dmsetup load    leg0 --table '0 2097152 error'
sudo dmsetup resume  leg0
sudo dd if=/dev/urandom of=/dev/md0 bs=1M count=4 conv=fsync    # dd: Input/output error
```

```
md0 : broken raid1 dm-1[1](F) dm-0[0]
      1047552 blocks super 1.2 [2/1] [U_]

array_state = broken       degraded = 1
dev-dm-0 slot=0    state=in_sync,write_error,want_replacement   <- tell for the survivor
dev-dm-1 slot=none state=faulty
```

Reads and writes now fail (`dd: Input/output error`, `Buffer I/O error on dev md0`).
With `echo 1 > /sys/block/md0/md/fail_last_dev` set beforehand you get the symmetric
picture instead:

```
md0 : broken raid1 dm-1[1](F) dm-0[0](F)
      1047552 blocks super 1.2 [2/0] [__]
array_state=broken   degraded=2   both state=faulty   no rd0/rd1 links
md/raid1:md0: Disk failure on dm-0, disabling device.
md/raid1:md0: Operation continuing on 0 devices.
```

### Which one died first, when the array is down

Read the metadata off the raw disks, bypassing the erroring dm layer:

```bash
$ sudo blockdev --flushbufs /dev/loop0 /dev/loop1
$ sudo python3 mdsb.py /dev/loop0 /dev/loop1
/dev/loop0:
  name=demo:0 uuid=d6d366f8-... level=1 raid_disks=2
  events=14 utime=1787447859 resync_offset=clean
  this dev: number=0 role=0
  array roles: 0:0 1:faulty            # <- the survivor records who died
/dev/loop1:
  events=10 utime=1787447793 resync_offset=clean
  this dev: number=1 role=1
  array roles: 0:0 1:1                 # <- stale, still thinks both are healthy
```

Highest `events` = freshest = authoritative. This is the same comparison mdadm makes;
see `mdadm_test.md` for the degraded-assembly policy built on it.

## Part 5 — Putting a healed disk back

Parts 0–4 above are the original 2026-08-22 session. Part 5 was re-run 2026-08-26 on the
same host and kernel by [`poc/raid1heal/lowlevel_demo.py`](poc/raid1heal/lowlevel_demo.py),
on a second pair of 1 GiB legs — `ll0`/`ll1` on `md127`, so it could coexist with the live
array from `raid1_selfheal_test.md` — with `data_offset=32768` (`data_size = 2064384`
sectors). The transcripts below use those names.

Each round: swap `ll1` to `dm error`, let md kick it, write 1024 scattered 4 KiB blocks
through the array (which dirties ~40 MiB worth of 64 KiB bitmap chunks), swap the leg back
to `linear`, then put it back one of three ways. **Copied** is the delta of field 7 of
`/sys/block/dm-1/stat` — the sectors md actually wrote onto the returning leg, which is the
only unambiguous way to tell a bitmap-scoped recovery from a full rebuild.

| way back in | copied | | wall | uses the bitmap? |
|---|---|---|---|---|
| `echo re-add > dev-dm-1/state` | 82,713 | 40.4 MiB | 0.113 s | yes |
| `ioctl(md_fd, ADD_NEW_DISK, …)` | 82,713 | 40.4 MiB | 0.109 s | yes |
| `echo 252:1 > md/new_dev` | **2,064,509** | **1008 MiB** | **5.392 s** | no |

The two bitmap paths copy *identically* — 82,713 sectors both times — which is the point:
they converge on the same `saved_raid_disk >= 0` state and the same dirty-chunk list.

The deciding line is in `raid1_add_disk()`:

```c
/* As all devices are equivalent, we don't need a full recovery
 * if this was recently any drive of the array */
if (rdev->saved_raid_disk < 0)
        conf->fullsync = 1;
```

`conf->fullsync` is what stops `raid1_sync_request()` skipping clean bitmap chunks. So the
only question is which path sets `saved_raid_disk`.

### 5a. `re-add` — rdev still attached (bitmap-scoped)

md kicked the leg but the rdev is still bound: `remove_spares()` did
`rdev->saved_raid_disk = rdev->raid_disk` on the way out, and that lives in kernel memory.

```bash
sudo dmsetup suspend --nolockfs --noflush ll1
sudo dmsetup load    ll1 --table '0 2097152 linear /dev/loop3 0'
sudo dmsetup resume  ll1
cat /sys/block/md127/md/dev-dm-1/state                 # faulty
echo re-add | sudo tee /sys/block/md127/md/dev-dm-1/state
```

```
    ll1 events=0, ll0 bitmap events_cleared=0
    re-add -> (True, 're-add')
    copied 82713 sectors in 0.113s
md127 : active raid1 dm-1[1] dm-0[0]
```

`state_store()` requires `Faulty && raid_disk == -1 && saved_raid_disk >= 0`, so this works
only while the rdev never detached. It needs no event-count check and mdadm makes none:
while the rdev has been faulty the array has been degraded, and `bitmap_endwrite()` only
advances `events_cleared` when `!mddev->degraded`, so the dirty-chunk list still covers the
whole gap by construction.

### 5b. `ADD_NEW_DISK` — rdev detached, or a freshly re-assembled array (bitmap-scoped)

This is the one thing here that is not a sysfs write, and there is no safe sysfs
substitute. It is what mdadm falls back to when `re-add` is refused
(`Manage.c:1608` → `attempt_re_add()`), and it is the only way to get a bitmap-scoped
recovery after a stop and re-assembly.

```python
ADD_NEW_DISK = 0x40140921          # _IOW(MD_MAJOR, 0x21, mdu_disk_info_t), 5 ints
disc = struct.pack("<5i", sb["dev_number"],       # from the leg's OWN superblock
                          os.major(rdev_t), os.minor(rdev_t),
                          sb["role"],             # the slot it already claims
                          (1 << 1) | (1 << 2) | (1 << 10))   # ACTIVE | SYNC | FAILFAST
fcntl.ioctl(os.open("/dev/md127", os.O_RDONLY), ADD_NEW_DISK, disc)
```

The kernel does the safety check itself, in `super_1_validate()`:

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

and `md_add_new_disk()` turns "too old" into a hard failure rather than a silent full
rebuild, because we asserted `MD_DISK_SYNC` together with a slot number:

```c
if ((info->state & (1<<MD_DISK_SYNC)) && rdev->raid_disk != info->raid_disk) {
        export_rdev(rdev, mddev);
        return -EINVAL;
}
```

So the ioctl either succeeds bitmap-scoped or returns `EINVAL`, and `EINVAL` is the caller's
signal to fall back to `new_dev` and pay for the full copy. Verified by rewriting the
returning leg's `events` to 1 while its peer's bitmap held `events_cleared=674`: the ioctl
refused, and only then did the whole device get copied. The end-to-end version of that
experiment is `raid1_selfheal_test.md` §5.4.

It also carries `MD_DISK_FAILFAST` in `disc.state`, so the flag survives — see the findings.

### 5c. Stop, re-assemble, and put the stale leg back

The interesting case, because assembly and repair pull in opposite directions.

```bash
# ll1 has been failed and healed; its superblock is behind
echo inactive | sudo tee /sys/block/md127/md/array_state  # stop personality, keep rdevs
echo clear    | sudo tee /sys/block/md127/md/array_state  # release rdevs, the gendisk goes

echo md127  | sudo tee /sys/module/md_mod/parameters/new_array
echo 1.2    | sudo tee /sys/block/md127/md/metadata_version
echo 252:0  | sudo tee /sys/block/md127/md/new_dev
echo 252:1  | sudo tee /sys/block/md127/md/new_dev
echo active | sudo tee /sys/block/md127/md/array_state
```

```
/dev/mapper/ll0: events=42 utime=1787731111 role=0 roles=[0:0 1:faulty] dirty
/dev/mapper/ll1: events=39 utime=1787731111 role=1 roles=[0:0 1:1]

md: kicking non-fresh dm-1 from array!
md127 : active raid1 dm-0[0]        <- [2/1] [U_], started from the fresh leg alone
```

`analyze_sbs()` kicks it, unchanged, and it will always kick it: that path runs
`super_1_validate()` with `mddev->pers == NULL`, where the check is pure event arithmetic
(`if (ev1 + 1 < mddev->events) return -EINVAL`) and the bitmap is never consulted. Note
also that `analyze_sbs()` only runs at all when `mddev->raid_disks == 0` — assembling by
writing `level`/`raid_disks` by hand would skip it, which is what mdadm does, but then
every freshness decision becomes userspace's problem.

The fix is not to argue with assembly. Let it start degraded from the fresh leg, then use
5b:

```
    ADD_NEW_DISK -> (True, 'ADD_NEW_DISK slot 1')
    copied 82713 sectors in 0.086s
md127 : active raid1 dm-1[1] dm-0[0]
```

The array is back to `[2/2]` having copied 40 MiB — the same amount as the never-detached
`re-add` in 5a — where the `new_dev` hot-add in this exact position copies 1008 MiB.

### 5d. Administrative fail / remove / add

```bash
echo faulty | sudo tee /sys/block/md127/md/dev-dm-1/state   # force-fail
echo remove | sudo tee /sys/block/md127/md/dev-dm-1/state   # detach the rdev
```

After `remove` the rdev is gone, so `re-add` is no longer available — but `ADD_NEW_DISK`
still is, and still uses the bitmap. `new_dev` here is a full rebuild (5183 ms measured).

## Part 6 — Teardown

```bash
echo inactive | sudo tee /sys/block/md0/md/array_state
echo clear    | sudo tee /sys/block/md0/md/array_state
sudo dmsetup remove leg0 leg1
sudo losetup -d /dev/loop0 /dev/loop1
sudo rm -rf /var/tmp/raid1demo
sudo modprobe -r raid1
```

## Findings / gotchas

- **`new_dev` is the wrong hot-add, and it is the obvious one.** `new_dev_store()`
  binds the device and nothing else — it never calls `validate_super()` — so
  `saved_raid_disk` stays −1 and `raid1_add_disk()` sets `conf->fullsync = 1`. It is the
  only one of the three paths that copies the whole device (2,064,509 sectors / 5.4 s
  against 82,713 sectors / 0.11 s for the same fault). Treat it as the fallback, never the default.
- **`ADD_NEW_DISK` is the missing sysfs verb.** There is no sysfs attribute that safely
  sets `saved_raid_disk` for a detached rdev, so a stop-and-re-assemble does *not* have to
  mean a full rebuild — it means you have to use the ioctl, exactly as mdadm does.
- **Do not fake it with `insync` + `slot`.** Writing `insync` then the slot number
  (what `sysfs_add_disk()` does inside mdadm, where the event counts have already been
  validated) raced with md's own spare activation here and left `degraded` out of sync
  with reality (`[2/1] [UU]`); the next single-disk failure then took the array straight
  to `broken`. Worse, `slot_store()` does `clear_bit(Bitmap_sync, &rdev->flags)`
  unconditionally, so this route asserts "in sync with the bitmap" without any check at
  all. Use `re-add` or `ADD_NEW_DISK`, both of which are gated.
- **Who checks that the bitmap still covers the gap.** `re-add` needs no check (the array
  has been degraded throughout, so `bitmap_endwrite()` has not advanced `events_cleared`).
  `ADD_NEW_DISK` is checked **by the kernel** — `super_1_validate()` compares
  `ev1 < md_bitmap_events_cleared(mddev)` and `md_add_new_disk()` returns `-EINVAL` if the
  device is too old. A userspace `events` comparison is still worth logging, but it is not
  what makes the operation safe.
- **`ADD_NEW_DISK` carries `FailFast`; `new_dev` does not.** `md_add_new_disk()` reads
  `MD_DISK_FAILFAST` out of `disc.state`, while a leg hot-added through `new_dev` silently
  loses the flag `super_1_validate()` would have read from `devflags`. That matters: see `raid1_selfheal_test.md` §5.3 — without failfast a wedged-but-connected
  backend produces no IO error at all, so a leg that lost the flag on repair silently regresses
  to hanging instead of erroring.
- `metadata_version` must be written **before** the first `new_dev` (`-EBUSY` once any
  disk is bound).
- Internal bitmap requires real metadata. With `metadata_version=none`,
  `location_store()` rejects every offset (`major_version == 0 && offset !=
  default_offset`), so the "no-superblock array" route cannot have one.
- Rig artifact: `dd if=/dev/loopN` can return **stale page-cache** data, because md
  writes reach the loop device as bios through dm and do not invalidate the loop bdev's
  own cache. Always `blockdev --flushbufs /dev/loopN` before comparing legs — this
  produced one false "legs differ" during the session.
- The test VM ships uutils `dd` 0.8.0, where `iflag/oflag=direct` misbehave; use
  `conv=fsync` plus `blockdev --flushbufs` instead.
- Kernel 7.0 logs two harmless notes: `md: async del_gendisk mode will be removed in
  future, please upgrade to mdadm-4.5+` and `md0: array will not be assembled in old
  kernels that lack configurable LBS support (<= 6.18)`. `feature_map` stays `0x1`.

## Appendix A — the `ADD_NEW_DISK` call

`poc/raid1heal/mdctl.py`, in full. This is mdadm's `attempt_re_add()` with the parts we do
not need removed. Everything it passes comes from the returning leg's **own** superblock, so
a leg can only ever be offered back into the slot it already claims.

```python
ADD_NEW_DISK = 0x40140921        # _IOW(MD_MAJOR, 0x21, mdu_disk_info_t), 5 ints
MD_DISK_ACTIVE = 1 << 1
MD_DISK_SYNC = 1 << 2
MD_DISK_FAILFAST = 1 << 10


def add_new_disk(md, path):
    sb = mdsb.examine(path)
    if sb is None:
        return False, "no superblock on %s" % path
    role = sb["role"]
    if role >= sb["raid_disks"]:
        return False, "own superblock says role=%s, not a member slot" % mdsb.role_str(role)
    dn = devname(path)
    if dn is None:
        return False, "no block device"
    rdev_t = os.stat("/dev/" + dn).st_rdev
    state = MD_DISK_ACTIVE | MD_DISK_SYNC | (MD_DISK_FAILFAST if sb["failfast"] else 0)
    disc = ctypes.create_string_buffer(
        struct.pack("<5i", sb["dev_number"], os.major(rdev_t), os.minor(rdev_t),
                    role, state), 20)
    fd = os.open("/dev/" + md, os.O_RDONLY)
    try:
        fcntl.ioctl(fd, ADD_NEW_DISK, disc)
    except OSError as e:
        name = errno.errorcode.get(e.errno, str(e.errno))
        why = " (bitmap no longer covers the gap)" if e.errno == errno.EINVAL else ""
        return False, "ADD_NEW_DISK slot %d: %s%s" % (role, name, why)
    finally:
        os.close(fd)
    return True, "ADD_NEW_DISK slot %d" % role
```

`mdu_disk_info_t` is five `int`s — `number`, `major`, `minor`, `raid_disk`, `state` — so the
buffer is 20 bytes and the ioctl number is `_IOW(9, 0x21, 20) = 0x40140921`. `md_ioctl_valid()`
requires only `CAP_SYS_ADMIN`, so an `O_RDONLY` fd on `/dev/md0` is enough; opening the array
while an application has it open is fine.

Failure modes worth handling separately:

| errno | meaning |
|---|---|
| `EINVAL` | too old — `ev1 < events_cleared`, so `raid_disk` stayed −1 and the `MD_DISK_SYNC` assertion failed. Fall back to `new_dev`. |
| `EEXIST` / `EBUSY` | the rdev is still bound (use `re-add`) or the slot is occupied |
| `ENODEV` | the array is not running — assemble it first |
