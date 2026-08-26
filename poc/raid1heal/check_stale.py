#!/usr/bin/env python3
"""For a given md0 offset, show what md0 and each raw leg hold there."""
import mmap
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from rig import MD, DATA_OFFSET, LEGS


def peek(dev, off):
    fd = os.open(dev, os.O_RDONLY | os.O_DIRECT)
    b = mmap.mmap(-1, 4096)
    os.preadv(fd, [b], off)
    os.close(fd)
    return bytes(b[:32])


for off in (int(x) for x in sys.argv[1:]):
    print("%s off=%d" % (MD, off))
    print("   %-4s: %r" % (MD, peek("/dev/" + MD, off)))
    for l in LEGS:
        print("   %-4s: %r" % (l["node"], peek(l["path"], DATA_OFFSET * 512 + off)))
