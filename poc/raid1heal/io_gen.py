#!/usr/bin/env python3
"""Continuous verified read/write load on the array -- and the correctness oracle.

Every write records the token it wrote at that offset; every read of a
previously-written offset is compared against it. So a wrong repair does not
show up as an IO error, it shows up as STALE DATA. That is what caught the
silent-write-drop result.

O_DIRECT throughout: buffered IO would let the page cache answer from a leg
that is no longer there.
"""
import mmap
import os
import random
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from rig import MD, PAUSE_FILE

BS = 4096
REGION = 64 << 20            # keep the working set small so reads hit written blocks
IOPS = 20.0
REPORT = 30.0


def log(msg):
    sys.stdout.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))
    sys.stdout.flush()


def main():
    dev = "/dev/" + MD
    nblk = REGION // BS
    rng = random.Random(1234)
    expect = {}                                  # offset -> token last known on disk
    st = dict(w=0, r=0, werr=0, rerr=0, stale=0, paused=0)
    buf = mmap.mmap(-1, BS)                      # page-aligned, as O_DIRECT requires
    fd = None
    seq = 0
    last = time.monotonic()
    log("verified IO on %s: %d x %dB blocks at %g IOPS" % (dev, nblk, BS, IOPS))

    while True:
        time.sleep(1.0 / IOPS)
        if os.path.exists(PAUSE_FILE):
            if fd is not None:
                os.close(fd)                     # release the array so it can be stopped
                fd = None
            st["paused"] += 1
            time.sleep(0.5)
            continue
        if fd is None:
            try:
                fd = os.open(dev, os.O_RDWR | os.O_DIRECT)
            except OSError as e:
                log("open %s: %s" % (dev, e.strerror))
                time.sleep(1.0)
                continue
        off = rng.randrange(nblk) * BS
        seq += 1
        if rng.random() < 0.5:
            token = ("w%08d-off%09d-" % (seq, off)).encode()
            buf.seek(0)
            buf.write((token * (BS // len(token) + 1))[:BS])
            try:
                os.pwritev(fd, [buf], off)
                expect[off] = token
                st["w"] += 1
            except OSError as e:
                expect.pop(off, None)            # outcome unknown after an error
                st["werr"] += 1
                log("WRITE off=%d: %s" % (off, e.strerror))
                os.close(fd); fd = None
        else:
            try:
                os.preadv(fd, [buf], off)
                st["r"] += 1
                exp = expect.get(off)
                if exp is not None and bytes(buf[:len(exp)]) != exp:
                    st["stale"] += 1
                    log("STALE DATA off=%d: expected %s got %s"
                        % (off, exp, bytes(buf[:len(exp)])))
            except OSError as e:
                st["rerr"] += 1
                log("READ off=%d: %s" % (off, e.strerror))
                os.close(fd); fd = None
        now = time.monotonic()
        if now - last >= REPORT:
            log("stats " + " ".join("%s=%d" % kv for kv in st.items()) +
                " tracked=%d" % len(expect))
            last = now


if __name__ == "__main__":
    main()
