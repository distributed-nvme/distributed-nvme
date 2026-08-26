#!/usr/bin/env python3
"""Everything we do to md, in the order mdadm does it.

mdadm has no privileged channel into the kernel. Putting a returning leg back
is exactly two moves, and Manage.c does them in this order:

  1. `echo re-add > /sys/block/mdX/md/dev-YYY/state`          (Manage.c:1608)
  2. failing that, the ADD_NEW_DISK ioctl                     (attempt_re_add())

Both land on `rdev->saved_raid_disk >= 0`, and that single field is what stops
raid1_add_disk() doing

        if (rdev->saved_raid_disk < 0)
                conf->fullsync = 1;

i.e. it is the difference between copying the dirty bitmap chunks and copying
the whole device. `echo <maj:min> > md/new_dev` -- the obvious sysfs hot-add --
never sets it, because new_dev_store() does not call validate_super() at all.
That is the only reason the old version of this POC always paid a full rebuild.
"""
import ctypes
import errno
import fcntl
import os
import struct
import time

import mdsb

ADD_NEW_DISK = 0x40140921        # _IOW(MD_MAJOR, 0x21, mdu_disk_info_t), 5 ints
MD_DISK_ACTIVE = 1 << 1
MD_DISK_SYNC = 1 << 2
MD_DISK_FAILFAST = 1 << 10


# --------------------------------------------------------------------------
# sysfs
# --------------------------------------------------------------------------
def rd(path, default=None):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def wr(path, val):
    """Returns None on success, else the errno name."""
    try:
        with open(path, "w") as f:
            f.write(val)
        return None
    except OSError as e:
        return errno.errorcode.get(e.errno, str(e.errno))


def devname(path):
    """/dev/disk/by-id/... -> 'nvme0n1', or None if the node is gone."""
    try:
        return os.path.basename(os.path.realpath(path))
    except OSError:
        return None


def devt(path):
    dn = devname(path)
    return rd("/sys/block/%s/dev" % dn) if dn else None


def sectors_written(path):
    """Field 7 of /sys/block/<dev>/stat. The delta across a repair is exactly
    how much md copied onto the returning leg -- the only unambiguous way to
    tell a bitmap-scoped recovery from a full rebuild."""
    dn = devname(path)
    s = rd("/sys/block/%s/stat" % dn) if dn else None
    return int(s.split()[6]) if s else None


def read_md(md):
    """Snapshot of everything the kernel will tell us about the array."""
    base = "/sys/block/%s/md" % md
    if not os.path.isdir(base):
        return dict(present=False, base=base, rdevs={}, in_sync=0, degraded=0)
    st = dict(present=True, base=base,
              array_state=rd(base + "/array_state", "?"),
              degraded=int(rd(base + "/degraded", "0") or 0),
              sync_action=rd(base + "/sync_action"),
              sync_completed=rd(base + "/sync_completed"),
              rdevs={})
    for d in sorted(x for x in os.listdir(base) if x.startswith("dev-")):
        p = "%s/%s" % (base, d)
        state = set(x for x in (rd(p + "/state", "") or "").split(",") if x)
        st["rdevs"][d[4:]] = dict(name=d[4:], dir=p, slot=rd(p + "/slot", "none"),
                                  state=state, faulty="faulty" in state,
                                  in_sync="in_sync" in state)
    st["in_sync"] = sum(1 for r in st["rdevs"].values() if r["in_sync"])
    # array_state is not a reliable "everything is gone" signal: with
    # fail_last_dev=1 it has read plain `active` with degraded=2. Count rdevs.
    st["broken"] = st["in_sync"] == 0
    return st


def tunables(md):
    """fail_last_dev, and failfast on every rdev. Re-applied every pass because
    a leg that came back through new_dev does not inherit FailFast from the
    metadata (ADD_NEW_DISK does -- we pass the bit in disc.state)."""
    base = "/sys/block/%s/md" % md
    fixed = []
    if not os.path.isdir(base):
        return fixed
    if rd(base + "/fail_last_dev") != "1":
        wr(base + "/fail_last_dev", "1")
        fixed.append("fail_last_dev=1")
    for d in sorted(x for x in os.listdir(base) if x.startswith("dev-")):
        state = (rd("%s/%s/state" % (base, d), "") or "").split(",")
        if "failfast" not in state and "faulty" not in state:
            if wr("%s/%s/state" % (base, d), "failfast") is None:
                fixed.append(d[4:] + " failfast")
    return fixed


# --------------------------------------------------------------------------
# the three ways back in, best first
# --------------------------------------------------------------------------
def re_add(rdev):
    """state_store("re-add"): needs the rdev still bound, Faulty, raid_disk==-1
    and saved_raid_disk>=0. remove_spares() set saved_raid_disk when md kicked
    the leg, so this works only while the rdev never detached -- and only in
    kernel memory, which is why a stop/re-assemble loses it.

    No events_cleared check is needed here and mdadm does not make one: while
    this rdev has been faulty the array has been degraded, and
    bitmap_endwrite() only advances events_cleared when !mddev->degraded. The
    bitmap therefore still covers the whole gap by construction."""
    e = wr(rdev["dir"] + "/state", "re-add")
    return (False, "re-add refused: %s" % e) if e else (True, "re-add")


def add_new_disk(md, path):
    """mdadm's attempt_re_add(): tell the kernel this device belongs in the slot
    its own superblock claims, and assert it should come back in sync.

    super_1_validate() then applies the gate, in the `else if (mddev->bitmap)`
    branch:

        if (ev1 < md_bitmap_events_cleared(mddev))
                return 0;                       -> raid_disk stays -1
        if (ev1 < mddev->events)
                set_bit(Bitmap_sync, &rdev->flags);

    and md_add_new_disk() turns that first case into a hard failure:

        if ((info->state & (1<<MD_DISK_SYNC)) && rdev->raid_disk != info->raid_disk)
                return -EINVAL;

    So the ioctl either succeeds -- bitmap-scoped, saved_raid_disk set, no
    fullsync -- or fails with EINVAL because the dirty-chunk list no longer
    covers the gap. It cannot quietly do the wrong thing."""
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


def hot_add(md, path):
    """new_dev_store(): binds the device and nothing else -- no validate_super,
    so saved_raid_disk stays -1 and raid1_add_disk() sets conf->fullsync = 1.
    Last resort, and it is a full copy every time."""
    t = devt(path)
    if not t:
        return False, "no dev_t"
    e = wr("/sys/block/%s/md/new_dev" % md, t)
    return (False, "new_dev refused: %s" % e) if e else (True, "new_dev (FULL rebuild)")


def detach(rdev):
    wr(rdev["dir"] + "/state", "faulty")
    return wr(rdev["dir"] + "/state", "remove")


# --------------------------------------------------------------------------
# whole-array moves
# --------------------------------------------------------------------------
def stop(md):
    base = "/sys/block/%s/md" % md
    for _ in range(20):
        wr(base + "/array_state", "inactive")
        if wr(base + "/array_state", "clear") is None:
            break
        time.sleep(0.5)
    for _ in range(40):           # the gendisk goes away asynchronously
        if not os.path.exists("/sys/block/%s" % md):
            return True, "stopped"
        time.sleep(0.25)
    return False, "still present"


def assemble(md, paths):
    """Create the array from the given legs only and let super_1_validate()
    fill in level, raid_disks, chunk and bitmap location from the metadata."""
    wr("/sys/module/md_mod/parameters/new_array", md)
    base = "/sys/block/%s/md" % md
    if not os.path.isdir(base):
        return False, "new_array failed"
    e = wr(base + "/metadata_version", "1.2")      # must precede the first new_dev
    if e:
        return False, "metadata_version: %s" % e
    for p in paths:
        t = devt(p)
        if t:
            wr(base + "/new_dev", t)
    e = wr(base + "/array_state", "active")
    return (False, "array_state=active: %s" % e) if e else (True, "assembled")
