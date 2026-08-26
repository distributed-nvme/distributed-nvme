#!/usr/bin/env python3
"""Is this leg actually working? Ask it directly, not through md.

A leg whose writes are silently discarded, or whose reads are silently
corrupted, returns success to every IO. md is structurally blind to that, and
so is any check that only asks "did the IO complete?". Only a read-back
comparison sees it, so the probe writes a fresh random token into the 4 KiB
scratch slot just below data_offset and reads it straight back.

The deadline is enforced by forking and *never* waiting. A child blocked on a
wedged nvme queue is in uninterruptible D state; SIGKILL will not land until
the transport gives up, so subprocess.run(timeout=...) would block the caller
for the full nvme_core.io_timeout -- exactly when it most needs to stay
responsive. The parent polls, kills the process group, and abandons the child.
"""
import mmap
import os
import signal
import subprocess
import sys
import tempfile
import time

from mdctl import rd, devname
from rig import PROBE_BYTES, PROBE_OFFSET

TIMEOUT = 1.0
ROUNDS = 3
ROUND_GAP = 10.0
_orphans = []


def _child(mode, dev, token):
    """Runs in the forked child. O_DIRECT so no page cache can fake a result."""
    pat = (token.encode() * (PROBE_BYTES // len(token) + 1))[:PROBE_BYTES]
    fd = os.open(dev, (os.O_RDWR if mode == "write" else os.O_RDONLY) | os.O_DIRECT)
    try:
        buf = mmap.mmap(-1, PROBE_BYTES)
        if mode == "write":
            buf.write(pat)
            os.pwritev(fd, [buf], PROBE_OFFSET)
            os.fsync(fd)
        else:
            os.preadv(fd, [buf], PROBE_OFFSET)
            if bytes(buf[:PROBE_BYTES]) != pat:
                sys.stderr.write("read-back mismatch (silent corruption)\n")
                return 2
    finally:
        os.close(fd)
    return 0


def _timed(mode, dev, token):
    for p in list(_orphans):                       # reap anything that finally died
        if p.poll() is not None:
            _orphans.remove(p)
    out = tempfile.TemporaryFile()
    p = subprocess.Popen([sys.executable, os.path.abspath(__file__), mode, dev, token],
                         stdout=out, stderr=subprocess.STDOUT, start_new_session=True)
    t0 = time.monotonic()
    while time.monotonic() - t0 < TIMEOUT:
        rc = p.poll()
        if rc is not None:
            out.seek(0)
            msg = out.read().decode(errors="replace").strip()
            dt = time.monotonic() - t0
            if rc == 0:
                return True, "%s ok in %.3fs" % (mode, dt)
            return False, "%s failed in %.3fs: %s" % (mode, dt, msg or "rc=%d" % rc)
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except OSError:
        pass
    _orphans.append(p)
    return False, "%s did not finish within %.1fs" % (mode, TIMEOUT)


def _nvme_ok(leg):
    for c in sorted(os.listdir("/sys/class/nvme")):
        if rd("/sys/class/nvme/%s/subsysnqn" % c) == leg["nqn"]:
            state = rd("/sys/class/nvme/%s/state" % c, "?")
            if state != "live":
                return False, "%s state=%s" % (c, state)
            dn = devname(leg["path"])
            if not dn or not os.path.exists("/sys/block/%s/dev" % dn):
                return False, "%s has no block device" % c
            # An ANA-inaccessible path keeps nvme_available_path() true and
            # swallows IO forever without ever erroring.
            ana = rd("/sys/block/%s/ana_state" % dn)
            if ana not in (None, "optimized"):
                return False, "%s ana_state=%s" % (dn, ana)
            return True, "%s live, %s" % (c, dn)
    return False, "no controller for %s" % leg["nqn"]


def once(leg):
    ok, why = _nvme_ok(leg)
    det = ["nvmeof: " + why]
    if not ok:
        return False, det
    token = os.urandom(8).hex()
    for mode in ("write", "read"):
        ok, why = _timed(mode, leg["path"], token)
        det.append("probe " + why)
        if not ok:
            return False, det
    return True, det


def qualify(leg, log):
    """ROUNDS checks, ROUND_GAP apart. One failure condemns the leg: a leg that
    is flapping is not a leg you want back."""
    for i in range(1, ROUNDS + 1):
        ok, det = once(leg)
        log("    round %d/%d %s: %s" % (i, ROUNDS, "PASS" if ok else "BAD", "; ".join(det)))
        if not ok:
            return False
        if i < ROUNDS:
            time.sleep(ROUND_GAP)
    log("  %s: healthy across all %d rounds" % (leg["node"], ROUNDS))
    return True


if __name__ == "__main__":
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    try:
        sys.exit(_child(sys.argv[1], sys.argv[2], sys.argv[3]))
    except OSError as e:
        sys.stderr.write("%s\n" % e.strerror)
        sys.exit(4)
