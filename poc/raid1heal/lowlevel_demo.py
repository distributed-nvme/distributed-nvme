#!/usr/bin/env python3
"""The measurement behind mdraid1_lowlevel.md Part 5, self-contained.

Two loop-backed dm-linear legs -> md127, then the same leg failed and put back
three different ways, reporting the sectors md actually copied each time.

    sudo python3 lowlevel_demo.py

Runs on any host with /opt/dnv on the path; uses its own devices (ll0/ll1,
md127) so it can sit alongside a live md0.
"""
import mmap
import os
import random
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, "/opt/dnv")
import mdctl
import mdsb

MD = "md127"
LEGS = ["/dev/mapper/ll0", "/dev/mapper/ll1"]
BASE = "/var/tmp/lowlevel"
SECTORS = 2097152                  # 1 GiB


def sh(c):
    return subprocess.run(c, shell=True, capture_output=True, text=True).stdout.strip()


def mdstat():
    return [l.strip() for l in open("/proc/mdstat").read().split("\n") if MD in l]


def written(dev):
    return mdctl.sectors_written(dev)


def wait_clean(limit=180):
    t0 = time.monotonic()
    while time.monotonic() - t0 < limit:
        st = mdctl.read_md(MD)
        if st["degraded"] == 0 and st["sync_action"] in (None, "idle"):
            return time.monotonic() - t0
        time.sleep(0.02)
    return None


def dirty(mib):
    """Write mib MiB of scattered 4 KiB blocks so bitmap chunks go dirty."""
    fd = os.open("/dev/" + MD, os.O_RDWR | os.O_DIRECT)
    buf = mmap.mmap(-1, 4096)
    buf.write(b"x" * 4096)
    rng = random.Random(7)
    for _ in range(mib * 256):
        os.pwritev(fd, [buf], rng.randrange(16384) * 4096)
    os.fsync(fd)
    os.close(fd)


def build():
    sh("modprobe raid1 dm-mod loop")
    os.makedirs(BASE, exist_ok=True)
    for i in (0, 1):
        img = "%s/l%d.img" % (BASE, i)
        if not os.path.exists(img):
            sh("truncate -s 1024M " + img)
        lo = sh("losetup -j %s | cut -d: -f1" % img) or sh("losetup -f --show " + img)
        if not sh("dmsetup info ll%d 2>/dev/null" % i):
            sh("echo '0 %d linear %s 0' | dmsetup create ll%d" % (SECTORS, lo, i))
    print("legs: " + ", ".join("%s -> %s" % (l, os.path.realpath(l)) for l in LEGS))


def assemble():
    mdctl.wr("/sys/module/md_mod/parameters/new_array", MD)
    base = "/sys/block/%s/md" % MD
    mdctl.wr(base + "/metadata_version", "1.2")        # must precede new_dev
    for l in LEGS:
        mdctl.wr(base + "/new_dev", mdctl.devt(l))
    mdctl.wr(base + "/array_state", "active")
    mdctl.tunables(MD)
    print("\n".join(mdstat()))


def fail_and_heal_leg1(dirty_mib=4):
    """Error the leg, wait for md to kick it, dirty some chunks, heal the leg."""
    sh("dmsetup suspend --nolockfs --noflush ll1 && "
       "echo '0 %d error' | dmsetup load ll1 && dmsetup resume ll1" % SECTORS)
    t0 = time.monotonic()
    while time.monotonic() - t0 < 30 and mdctl.read_md(MD)["degraded"] == 0:
        dirty(1)
    print("    md kicked it: %s" % mdstat()[0])
    dirty(dirty_mib)
    lo = sh("losetup -j %s/l1.img | cut -d: -f1" % BASE)
    sh("dmsetup suspend --nolockfs --noflush ll1 && "
       "echo '0 %d linear %s 0' | dmsetup load ll1 && dmsetup resume ll1" % (SECTORS, lo))
    sb, bm = mdsb.examine(LEGS[1]), mdsb.bitmap_sb(LEGS[0])
    print("    ll1 events=%d, ll0 bitmap events_cleared=%d" % (sb["events"], bm["events_cleared"]))


def put_back(verb):
    before = written(LEGS[1])
    t0 = time.monotonic()
    st = mdctl.read_md(MD)
    rdev = st["rdevs"].get(mdctl.devname(LEGS[1]))
    if verb == "re-add":
        print("    %s -> %s" % (verb, mdctl.re_add(rdev)))
    else:
        if rdev:
            mdctl.detach(rdev)
        print("    %s -> %s" % (verb, (mdctl.add_new_disk if verb == "ADD_NEW_DISK"
                                       else mdctl.hot_add)(MD, LEGS[1])))
    dt = wait_clean()
    print("    copied %d sectors in %s" %
          (written(LEGS[1]) - before,
           "%.3fs" % (time.monotonic() - t0) if dt is not None else "TIMEOUT"))
    print("    %s" % mdstat()[0])


def main():
    build()
    print("\n=== metadata")
    print("   ", mdsb.create(LEGS, name="lowlevel:0", data_offset=32768))
    print("\n=== assemble")
    assemble()

    for verb in ("re-add", "ADD_NEW_DISK", "new_dev"):
        print("\n=== array running, leg 1 fails, comes back via %s" % verb)
        wait_clean()
        fail_and_heal_leg1()
        put_back(verb)

    print("\n=== stop, re-assemble, put the stale leg back with ADD_NEW_DISK")
    wait_clean()
    fail_and_heal_leg1()
    for l in LEGS:
        print("    %s: %s" % (l, mdsb.summary(mdsb.examine(l))))
    print("    stop ->", mdctl.stop(MD))
    assemble()
    print("    dmesg:", sh("dmesg | grep -i 'kicking non-fresh' | tail -1"))
    put_back("ADD_NEW_DISK")

    print("\n=== teardown")
    mdctl.stop(MD)
    sh("dmsetup remove ll0 ll1")
    for l in sh("losetup -a | grep lowlevel | cut -d: -f1").split():
        sh("losetup -d " + l)
    sh("rm -rf " + BASE)
    print("    clean")


if __name__ == "__main__":
    main()
