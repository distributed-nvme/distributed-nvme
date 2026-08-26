#!/usr/bin/env python3
"""md v1.2 metadata: write it, read it back.

Writing the superblock is the one part of RAID1 life-cycle management the
kernel will not do -- md never invents metadata, and super_1_load() rejects any
device that does not already carry some, at create time and on every hot-add.
Everything after that is sysfs and one ioctl (see mdctl.py).
"""
import mmap
import os
import struct
import time
import uuid

MD_SB_MAGIC = 0xA92B4EFC
BITMAP_MAGIC = 0x6D746962
MAX_SECTOR = 0xFFFFFFFFFFFFFFFF
FEATURE_BITMAP_OFFSET = 1
FAILFAST1 = 2                    # sb->devflags: "avoid retries, just fail"

ROLE_SPARE, ROLE_FAULTY = 0xFFFF, 0xFFFE
SB_SECTOR = 8                    # v1.2: superblock 4 KiB into the device
BITMAP_SECTOR = 16               # SB_SECTOR + bitmap_offset(8)


def _csum(sb, max_dev):
    """calc_sb_1_csum(): fold-to-32 sum over 256 + max_dev*2 bytes."""
    tot = 0
    for i in range(0, 256 + max_dev * 2, 4):
        tot += struct.unpack_from("<I", sb, i)[0]
    return ((tot & 0xFFFFFFFF) + (tot >> 32)) & 0xFFFFFFFF


def _read(dev, sector, size):
    """O_DIRECT: md writes reach the leg as bios, so the block device's own page
    cache will happily hand back a pre-failure copy."""
    fd = os.open(dev, os.O_RDONLY | os.O_DIRECT)
    try:
        buf = mmap.mmap(-1, max(size, 4096))
        os.preadv(fd, [buf], sector * 512)
        return bytes(buf[:size])
    finally:
        os.close(fd)


def create(devices, name=None, data_offset=32768, chunk=65536, reserved=4096):
    """Lay a matching v1.2 superblock + internal bitmap on every device."""
    from rig import NAME, DATA_OFFSET
    name = name or NAME
    data_offset = data_offset or DATA_OFFSET
    set_uuid = uuid.uuid4().bytes
    sizes = []
    for d in devices:
        fd = os.open(d, os.O_RDONLY)
        sizes.append(os.lseek(fd, 0, os.SEEK_END) // 512 - data_offset)
        os.close(fd)
    data_size = min(sizes) & ~7
    n = len(devices)
    now = int(time.time()) & ((1 << 40) - 1)

    for slot, dev in enumerate(devices):
        sb = bytearray(256 + n * 2)
        struct.pack_into("<IIII", sb, 0, MD_SB_MAGIC, 1, FEATURE_BITMAP_OFFSET, 0)
        sb[16:32] = set_uuid
        sb[32:64] = name.encode()[:31].ljust(32, b"\0")
        struct.pack_into("<QiiQ", sb, 64, now, 1, 0, data_size)    # ctime level layout size
        struct.pack_into("<III", sb, 88, 0, n, 8)                  # chunk raid_disks bitmap_offset
        struct.pack_into("<iQiiii", sb, 100, -1, MAX_SECTOR, 0, 0, 0, 0)
        struct.pack_into("<QQQQ", sb, 128, data_offset, data_size, SB_SECTOR, 0)
        struct.pack_into("<II", sb, 160, slot, 0)                  # dev_number
        sb[168:184] = uuid.uuid4().bytes
        struct.pack_into("<BBHi", sb, 184, FAILFAST1, 0, 0, 0)     # devflags: always failfast
        # resync_offset = MaxSector: "assume clean". Legal only because both
        # legs are zero-filled and therefore already identical.
        struct.pack_into("<QQQ", sb, 192, now, 0, MAX_SECTOR)      # utime events resync
        struct.pack_into("<II", sb, 216, 0, n)                     # sb_csum max_dev
        for i in range(n):
            struct.pack_into("<H", sb, 256 + 2 * i, i)             # dev_roles[] = identity
        struct.pack_into("<I", sb, 216, _csum(sb, n))

        bm = bytearray(256)
        struct.pack_into("<II", bm, 0, BITMAP_MAGIC, 4)            # version 4 == for md 1.x
        bm[8:24] = set_uuid
        struct.pack_into("<QQQ", bm, 24, 0, 0, data_size)          # events events_cleared sync_size
        struct.pack_into("<IIIII", bm, 48, 0, chunk, 5, 0, reserved)

        fd = os.open(dev, os.O_RDWR)
        os.pwrite(fd, bytes(sb).ljust(4096, b"\0"), SB_SECTOR * 512)
        os.pwrite(fd, bytes(bm), BITMAP_SECTOR * 512)
        os.fsync(fd)
        os.close(fd)
    return dict(uuid=str(uuid.UUID(bytes=set_uuid)), data_size=data_size)


def examine(dev):
    """Decode one leg's superblock, or None."""
    try:
        sb = _read(dev, SB_SECTOR, 4096)
    except OSError:
        return None
    magic, ver, feat = struct.unpack_from("<III", sb, 0)
    if magic != MD_SB_MAGIC or ver != 1:
        return None
    max_dev = struct.unpack_from("<I", sb, 220)[0]
    if max_dev > 1024:
        return None
    stored = struct.unpack_from("<I", sb, 216)[0]
    tmp = bytearray(sb)
    struct.pack_into("<I", tmp, 216, 0)     # the kernel sums with sb_csum zeroed
    if _csum(tmp, max_dev) != stored:
        return None
    devnum = struct.unpack_from("<I", sb, 160)[0]
    roles = [struct.unpack_from("<H", sb, 256 + 2 * i)[0] for i in range(max_dev)]
    utime, events, resync = struct.unpack_from("<QQQ", sb, 192)
    return dict(dev=dev,
                name=sb[32:64].rstrip(b"\0").decode(errors="replace"),
                raid_disks=struct.unpack_from("<I", sb, 92)[0],
                dev_number=devnum,
                role=roles[devnum] if devnum < len(roles) else ROLE_SPARE,
                roles=roles,
                events=events,
                utime=utime & ((1 << 40) - 1),
                clean=(resync == MAX_SECTOR),
                failfast=bool(sb[184] & FAILFAST1))


def bitmap_sb(dev):
    """The internal bitmap's own superblock -- events_cleared is the number that
    decides whether a returning leg can be re-added bitmap-scoped."""
    try:
        b = _read(dev, BITMAP_SECTOR, 256)
    except OSError:
        return None
    if struct.unpack_from("<I", b, 0)[0] != BITMAP_MAGIC:
        return None
    events, events_cleared, sync_size = struct.unpack_from("<QQQ", b, 24)
    return dict(events=events, events_cleared=events_cleared, sync_size=sync_size)


def set_events(dev, value):
    """Rewrite events in the superblock (checksum fixed up). Only used to age a
    leg deliberately, to prove the kernel's events_cleared gate really fires."""
    sb = bytearray(_read(dev, SB_SECTOR, 4096))
    max_dev = struct.unpack_from("<I", sb, 220)[0]
    struct.pack_into("<Q", sb, 200, value)
    struct.pack_into("<I", sb, 216, 0)
    struct.pack_into("<I", sb, 216, _csum(sb, max_dev))
    fd = os.open(dev, os.O_RDWR)
    os.pwrite(fd, bytes(sb), SB_SECTOR * 512)
    os.fsync(fd)
    os.close(fd)


def role_str(r):
    return {ROLE_SPARE: "spare", ROLE_FAULTY: "faulty"}.get(r, str(r))


def summary(sb):
    return ("events=%d utime=%d role=%s roles=[%s]%s"
            % (sb["events"], sb["utime"], role_str(sb["role"]),
               " ".join("%d:%s" % (i, role_str(r)) for i, r in enumerate(sb["roles"])),
               "" if sb["clean"] else " dirty"))


if __name__ == "__main__":
    import sys
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    if sys.argv[1:2] == ["--create"]:
        print(create(sys.argv[2:]))
    else:
        for d in sys.argv[1:]:
            sb, bm = examine(d), bitmap_sb(d)
            print("%s: %s" % (d, summary(sb) if sb else "no v1.x superblock"))
            if bm:
                print("    bitmap: events=%d events_cleared=%d"
                      % (bm["events"], bm["events_cleared"]))
