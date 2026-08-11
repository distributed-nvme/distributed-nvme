#!/bin/bash
#
# check.sh -- thin-pool block-allocation audit.
#
# After `setup.sh all` + a short `host0_io.sh` run, dumps each slice's thin-pool
# metadata with `thin_dump`, derives 8 LD bitmaps (one bit per 4 MiB LD block)
# via ld_bitmap.py, then on cn0 reads each marked block directly from the LDs
# (O_DIRECT + preadv, consistent with host0_io.sh -- NOT dd) and confirms it
# is non-zero (i.e. actually written, not thin-pool zero-fill).
#
# Workflow (never combined with failover or grow):
#
#   setup.sh all  ->  host0_io.sh start (~10s) -> host0_io.sh stop  ->  ./check.sh
#
# Phases (each wrapped with _t + slow_summary):
#   1. cn0: suspend each thin-pool, thin_dump the thinmeta, resume.  Capture
#      XML to /tmp/dnv-check/thin_dump_slice{0,1}.xml.
#   2. local: run ld_bitmap.py to derive the 8 .bit files.
#   3. cn0: scp the 8 bitmaps over, run a minimal Python verifier that reads
#      each set block on the 8 -cn0 LD devices and checks non-zero.  Emits one
#      JSONL record per LD plus a final summary line.
#
# Exit 0 only if all 8 LDs report `ok` or `empty`; non-zero otherwise.
#
# Usage: ./check.sh
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

WORK_DIR=/tmp/dnv-check

# Local _t/slow_summary: common.sh defines these only inside the _emit_common
# heredoc (for remote use), so check.sh -- which uses _t at the orchestrator
# level -- needs its own. keep them in sync with the remote versions.
_t() {
    local desc="$1"; shift
    local t0 t1 ms rc=0
    t0=$(date +%s%N); "$@" || rc=$?; t1=$(date +%s%N)
    ms=$(( (t1 - t0) / 1000000 ))
    [ "$ms" -gt 3000 ] && echo "[SLOW] ${ms}ms  $desc" >&2
    return $rc
}
slow_summary() { :; }

# ===================== Phase 1: dump thin-pool metadata ======================
# Precondition: the cn0 thin-pools must exist (i.e. setup.sh all was run).
# Per slice: minimal direct SSH (no _emit_common heredoc).  Reserving a metadata
# snapshot makes the thinmeta readable while the pool is active (thin_dump on a
# live pool's metadata device fails with EBUSY otherwise); thin_dump reads the
# metadata-snap of the concat thinmeta; the snap is released afterwards.
# thin_dump writes its XML to stdout (the only thing there); dmsetup noise is
# silenced on the remote.  Capture stdout to a mktemp file, validate non-empty,
# then mv to thin_dump_slice${slice}.xml.
phase1_dump() {
    _info "=== 1. cn0: dump thin-pool metadata ==="
    local slice thinpool thinmeta tmp rc
    for slice in 0 1; do
        thinpool="dnv-cn0-sp0-slice${slice}-thinpool"
        if ! _ssh "${SSH_USER}@${CN0_IP}" "sudo dmsetup info '${thinpool}' >/dev/null 2>&1"; then
            _fail "run setup.sh all first (${thinpool} missing on cn0)"
        fi
        thinmeta="dnv-cn0-sp0-slice${slice}-thinmeta"
        tmp="$(mktemp -p "${WORK_DIR}")"
        _t "thin_dump slice${slice}" _ssh "${SSH_USER}@${CN0_IP}" "
            sudo dmsetup message ${thinpool} 0 reserve_metadata_snap 2>/dev/null
            sudo thin_dump --metadata-snap /dev/mapper/${thinmeta}
            rc=\$?
            sudo dmsetup message ${thinpool} 0 release_metadata_snap 2>/dev/null
            exit \$rc
        " >"$tmp" 2>/dev/null
        rc=$?
        if [ "$rc" -ne 0 ] || [ ! -s "$tmp" ]; then
            rm -f "$tmp"
            _fail "thin_dump for slice${slice} failed or produced empty output (rc=$rc)"
        fi
        mv "$tmp" "${WORK_DIR}/thin_dump_slice${slice}.xml"
        _info "slice${slice}: $(wc -c < "${WORK_DIR}/thin_dump_slice${slice}.xml") bytes of thin_dump XML"
    done
    slow_summary
}

# ===================== Phase 2: derive 8 LD bitmaps ===========================
phase2_bitmap() {
    _info "=== 2. derive 8 LD bitmaps ==="
    _t "ld_bitmap.py" python3 "$(dirname "$0")/ld_bitmap.py" "${WORK_DIR}/" \
        || _fail "ld_bitmap.py failed"
    local n=0 f
    for f in "${WORK_DIR}"/dnv-*.bit; do
        [ -e "$f" ] || continue
        n=$((n + 1))
    done
    if [ "$n" -ne 8 ]; then
        _fail "ld_bitmap.py produced $n .bit files (expected 8)"
    fi
    _info "wrote $n .bit files to ${WORK_DIR}/"
    slow_summary
}

# ===================== Phase 3: verify written blocks on cn0 ================
# scp the 8 bitmaps to cn0, then run a minimal Python verifier on cn0 (no
# _emit_common).  The 8 LD device paths and the bitmap dir are baked in as
# Python literals (quoted heredoc -- no local expansion).  The verifier reads
# each set 4 MiB block via O_DIRECT + preadv (NOT dd; uutils coreutils' dd
# iflag=direct is broken on these devices -- see host0_io.sh) and checks the
# block is non-zero.  Output: one JSONL record per LD plus a final summary
# line.  The verifier exits 0 only if all 8 LDs report ok|empty.
phase3_verify() {
    _info "=== 3. cn0: verify LD blocks are non-zero ==="
    local f base rc
    for f in "${WORK_DIR}"/dnv-*.bit; do
        base="$(basename "$f")"
        _t "scp ${base}" scp -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -q \
            "$f" "${SSH_USER}@${CN0_IP}:${WORK_DIR}/${base}" \
            || _fail "could not scp ${base} to cn0"
    done

    # ssh runs `python3 -u -` on cn0; the heredoc body is the script.  Quoted
    # delimiter -> no local expansion, so the LD paths/dir are Python literals.
    _t "verify LDs on cn0" _ssh "${SSH_USER}@${CN0_IP}" sudo python3 -u - <<'EOF_VRFY'
import json
import mmap
import os
import re
import stat
import sys

WORK_DIR = "/tmp/dnv-check"
BLOCK = 4 * 1024 * 1024          # 4 MiB, matches POOL_BLOCK_SECTORS
NQN_PREFIX = "nqn.2026-07.org.dnv"

# 8 LDs (one per -real device).  ld0 lives on dn0, ld1 on dn1; for each
# (slice, grp) pair both ld0 and ld1 get a bitmap (raid1 mirror = same blocks
# on both legs).  On cn0 each LD is imported as an nvme namespace under the
# NQN nqn...:dn:<dn>:sp0-slice<slice>-grp<grp>-ld<ld>-cn0; the verifier resolves
# that namespace through sysfs and reads its 4 MiB blocks (the bitmap offsets
# are relative to the LD start, which is exactly the namespace offset 0).
LDS = [
    "dnv-dn0-sp0-slice0-grp0-ld0",
    "dnv-dn0-sp0-slice0-grp1-ld0",
    "dnv-dn0-sp0-slice1-grp0-ld0",
    "dnv-dn0-sp0-slice1-grp1-ld0",
    "dnv-dn1-sp0-slice0-grp0-ld1",
    "dnv-dn1-sp0-slice0-grp1-ld1",
    "dnv-dn1-sp0-slice1-grp0-ld1",
    "dnv-dn1-sp0-slice1-grp1-ld1",
]

NS_RE = re.compile(r"^nvme\d+n\d+$")

def label_to_nqn(label):
    # dnv-<dn>-sp0-slice<L>-grp<G>-ld<V> -> nqn...:dn:<dn>:sp0-slice<L>-grp<G>-ld<V>-cn0
    parts = label.split("-")
    dn = parts[1]
    rest = "-".join(parts[2:])     # sp0-slice<L>-grp<G>-ld<V>
    return "%s:dn:%s:%s-cn0" % (NQN_PREFIX, dn, rest)

def find_dev(nqn):
    """Resolve a subsys NQN to its nvme block node on cn0 via sysfs (the
    udev rule suppresses /dev/disk/by-id, so go through the subsystem path,
    like common.sh's nvme_dev_by_nqn). The controller path /sys/class/nvme
    only carries the hidden nvmeXcYnZ nodes; the real nvmeXnY block nodes
    live under /sys/class/nvme-subsystem/nvme-subsys*/."""
    for s in sorted(os.listdir("/sys/class/nvme-subsystem")):
        sp = "/sys/class/nvme-subsystem/" + s
        try:
            with open(sp + "/subsysnqn") as fh:
                if fh.read().strip() != nqn:
                    continue
        except OSError:
            continue
        for n in sorted(os.listdir(sp)):
            if not NS_RE.match(n):
                continue
            dev = "/dev/" + n
            try:
                if stat.S_ISBLK(os.stat(dev).st_mode):
                    return dev
            except OSError:
                pass
    # Fallback: controller path (non-multipath case).
    for c in sorted(os.listdir("/sys/class/nvme")):
        cp = "/sys/class/nvme/" + c
        try:
            with open(cp + "/subsysnqn") as fh:
                if fh.read().strip() != nqn:
                    continue
        except OSError:
            continue
        for n in sorted(os.listdir(cp)):
            m = re.match(r"^nvme\d+c\d+n\d+$", n) or NS_RE.match(n)
            if not m:
                continue
            dev = "/dev/" + n
            try:
                if stat.S_ISBLK(os.stat(dev).st_mode):
                    return dev
            except OSError:
                pass
    return None

def verify(label):
    bit_path = "%s/%s.bit" % (WORK_DIR, label)
    rec = {"ld": label, "set": 0, "verified": 0, "zero": 0, "status": "ok"}
    if not os.path.exists(bit_path):
        rec["status"] = "missing_bitmap"
        return rec
    with open(bit_path) as f:
        bits = f.read().rstrip("\n")
    # bit 0 = leftmost char (low offset); bit 124 = rightmost (high offset).
    for b, c in enumerate(bits):
        if c == "1":
            rec["set"] += 1
    if rec["set"] == 0:
        rec["status"] = "empty"
        return rec
    nqn = label_to_nqn(label)
    dev_path = find_dev(nqn)
    if dev_path is None:
        rec["status"] = "missing_device"
        rec["nqn"] = nqn
        return rec
    rec["dev"] = dev_path
    try:
        fd = os.open(dev_path, os.O_RDWR | os.O_DIRECT)
    except OSError as e:
        rec["status"] = "read_error"
        rec["error"] = "open: %s" % e
        return rec
    try:
        # Page-aligned 4 MiB buffer: O_DIRECT needs the user buffer aligned to
        # the device logical block size; an anonymous mmap is page-aligned,
        # which is stricter (mirrors host0_io.sh's approach).
        buf = mmap.mmap(-1, BLOCK)
        for b, c in enumerate(bits):
            if c != "1":
                continue
            try:
                n = os.preadv(fd, [buf], b * BLOCK)
            except OSError as e:
                rec["status"] = "read_error"
                rec["error"] = "block %d: %s" % (b, e)
                return rec
            if n != BLOCK:
                rec["status"] = "read_error"
                rec["error"] = "block %d: short read %d/%d" % (b, n, BLOCK)
                return rec
            # Non-zero check: an all-zero block means thin-pool zero-fill for a
            # region the bitmap says is allocated -> mismatch.
            if buf[:] == b"\x00" * BLOCK:
                rec["zero"] += 1
            else:
                rec["verified"] += 1
    finally:
        os.close(fd)
    if rec["zero"] > 0:
        rec["status"] = "fail"
    return rec

failures = []
for label in LDS:
    rec = verify(label)
    sys.stdout.write(json.dumps(rec) + "\n")
    sys.stdout.flush()
    if rec["status"] not in ("ok", "empty"):
        failures.append("%s=%s" % (label, rec["status"]))

if not failures:
    sys.stdout.write("[OK] %d/8 LDs verified, 0 zero-block mismatches\n" % len(LDS))
else:
    sys.stdout.write("[FAIL] %d LDs failed: %s\n" % (len(failures), ", ".join(failures)))

sys.exit(1 if failures else 0)
EOF_VRFY
    rc=$?
    if [ "$rc" -ne 0 ]; then
        _fail "cn0 verifier reported failures (rc=$rc)"
    fi
    slow_summary
}

# ===================== main ==================================================
main() {
    local t0 t1
    t0=$(date +%s%N)

    # Clean working dir at start (orchestrator + cn0).
    rm -rf "$WORK_DIR"
    mkdir -p "$WORK_DIR"
    _ssh "${SSH_USER}@${CN0_IP}" "rm -rf '${WORK_DIR}' && mkdir -p '${WORK_DIR}'" \
        || _fail "could not prepare ${WORK_DIR} on cn0"

    phase1_dump    || _fail "phase 1 (thin_dump) failed"
    phase2_bitmap  || _fail "phase 2 (ld_bitmap.py) failed"
    phase3_verify  || _fail "phase 3 (verify on cn0) failed"

    t1=$(date +%s%N)
    _info "check complete in $(( (t1 - t0) / 1000000 ))ms"
}

main "$@"
