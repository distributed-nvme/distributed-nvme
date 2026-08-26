#!/usr/bin/env python3
"""Runs on cn0: print how long md takes to kick a leg, sampled at 50 Hz.

ssh round-trips are ~0.5 s, which is the same order as the fastest detection
times, so this has to be measured locally.

    watch.py <devname> <limit-seconds>
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mdctl
from rig import MD

dev, limit = sys.argv[1], float(sys.argv[2])
t0 = time.monotonic()
while time.monotonic() - t0 < limit:
    st = mdctl.read_md(MD)
    r = st["rdevs"].get(dev)
    if r is None or r["faulty"] or st["degraded"] > 0:
        print("DETECT %.2f" % (time.monotonic() - t0))
        sys.exit(0)
    time.sleep(0.02)
print("DETECT none")
