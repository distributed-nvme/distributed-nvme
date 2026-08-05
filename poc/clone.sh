#!/bin/bash
#
# clone.sh -- dm-clone standalone test on cn0.
#
# Sources nothing, never invokes setup/teardown. Builds two 512 MiB loop
# devices (clone_src, clone_dst) plus a 4 MiB metadata loop (clone_meta),
# pre-fills src with 0x55 and dst with 0xaa, writes 0x00 to 8 randomly-
# selected (seed 42) 4 MiB regions on src, creates a dm-clone device with
# no_discard_passdown + no_hydration and 4 MiB region size, blkdiscards the
# 120 non-written regions (so dm-clone skip-hydrates them), enables hydration,
# waits for it to finish, removes the dm-clone device, then verifies that
# src and dst agree on the 8 written regions (both 0x00) and that src is 0x55
# and dst is 0xaa everywhere else, and finally tears down all three loops +
# backing files.
#
# Reference: https://origin.kernel.org/doc/html/latest/admin-guide/device-mapper/dm-clone.html
#
# Usage: ./clone.sh           # run from poc/, SSHes to cn0 for every action
#
set -uo pipefail

CN0_IP="192.168.122.125"
SSH_USER="yupeng"
SEC_CLONE_DATA=1048576    # 512M (src + dst size, in 512-byte sectors)
SEC_CLONE_META=8192       #   4M (meta size)
SEC_CLONE_REGION=8192      #   4M (dm-clone region_size)
REGION_BYTES=$((SEC_CLONE_REGION * 512))   # 4194304
NR_REGIONS=$((SEC_CLONE_DATA / SEC_CLONE_REGION))   # 128
NR_WRITE_REGIONS=8
SEED=42
IMG_DIR=/var/tmp
IMG_SRC=$IMG_DIR/dnv-clone-src.img
IMG_DST=$IMG_DIR/dnv-clone-dst.img
IMG_META=$IMG_DIR/dnv-clone-meta.img
DM_NAME=dnv-clone

_ssh()  { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
               -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }
_t()    { local desc="$1"; shift; local t0 t1 ms rc=0
          t0=$(date +%s%N); "$@" || rc=$?; t1=$(date +%s%N)
          ms=$(( (t1 - t0) / 1000000 ))
          [ "$ms" -gt 3000 ] && echo "[SLOW] ${ms}ms  $desc" >&2
          return $rc; }

# ===================== _cleanup (idempotent, safe on clean cn0) ==============
# Removes any stale dnv-clone dm device, detaches loops holding the imgs, and
# deletes the backing imgs. Run at start (clean slate) and at step 12.
_cleanup() {
    _ssh "${SSH_USER}@${CN0_IP}" "
        sudo dmsetup info ${DM_NAME} >/dev/null 2>&1 && \
            sudo dmsetup remove --force ${DM_NAME} >/dev/null 2>&1 || true
        for img in ${IMG_SRC} ${IMG_DST} ${IMG_META}; do
            for loop in \$(sudo losetup -j \"\$img\" 2>/dev/null | awk -F: '{print \$1}'); do
                sudo losetup -d \"\$loop\" >/dev/null 2>&1 || true
            done
            sudo rm -f \"\$img\" 2>/dev/null || true
        done
        true
    "
}

# ===================== step 1 -- create 3 loops =============================
step1_loops() {
    _info "step 1: creating loops on cn0"
    local out
    out=$(_t "step1" _ssh "${SSH_USER}@${CN0_IP}" "
        sudo truncate -s 512M ${IMG_SRC}
        sudo truncate -s 512M ${IMG_DST}
        sudo truncate -s 4M   ${IMG_META}
        SRC=\$(sudo losetup -f --show ${IMG_SRC})
        DST=\$(sudo losetup -f --show ${IMG_DST})
        META=\$(sudo losetup -f --show ${IMG_META})
        echo \"SRC=\$SRC\"
        echo \"DST=\$DST\"
        echo \"META=\$META\"
    ") || _fail "could not create loops on cn0"
    SRC=$(echo "$out" | sed -n 's/^SRC=//p')
    DST=$(echo "$out" | sed -n 's/^DST=//p')
    META=$(echo "$out" | sed -n 's/^META=//p')
    [ -n "$SRC" ] && [ -n "$DST" ] && [ -n "$META" ] \
        || _fail "could not parse loop paths from: $out"
    _info "step 1: loops created (SRC=$SRC DST=$DST META=$META)"
}

# ===================== steps 2,3,4 -- Python writer (O_DIRECT) ===============
# Fills src with 0x55, dst with 0xaa, then zeroes 8 seed-42 regions on src.
# 4 MiB mmap buffer, O_RDWR|O_DIRECT on both fds, 1 MiB pwritev unit.
steps_2_3_4() {
    _info "steps 2-4: filling src=0x55, dst=0xaa, zeroing 8 regions on src"
    _t "steps2-4" _ssh "${SSH_USER}@${CN0_IP}" "sudo python3 -u - '$SRC' '$DST'" <<'EOF_W'
import mmap, os, random, sys

SRC, DST = sys.argv[1], sys.argv[2]
REGION = 4 * 1024 * 1024            # 4 MiB
DEV_BYTES = 512 * 1024 * 1024       # 512 MiB
CHUNK = 1 * 1024 * 1024             # 1 MiB pwritev unit
NR_REGIONS = DEV_BYTES // REGION    # 128
NR_WRITE = 8
SEED = 42

random.seed(SEED)
write_regions = sorted(random.sample(range(NR_REGIONS), NR_WRITE))

src_fd = os.open(SRC, os.O_RDWR | os.O_DIRECT)
dst_fd = os.open(DST, os.O_RDWR | os.O_DIRECT)
buf = mmap.mmap(-1, CHUNK)          # page-aligned

def fill(fd, byte, dev_bytes):
    buf[:] = bytes([byte]) * CHUNK
    off = 0
    while off < dev_bytes:
        n = os.pwritev(fd, [buf], off)
        assert n == CHUNK, "short write %d/%d at %d" % (n, CHUNK, off)
        off += CHUNK

# Step 2: 0x55 across all of clone_src
fill(src_fd, 0x55, DEV_BYTES)
# Step 3: 0xaa across all of clone_dst
fill(dst_fd, 0xaa, DEV_BYTES)
# Step 4: 0x00 to the 8 seed-42 regions on clone_src only
buf[:] = b"\x00" * CHUNK
for r in write_regions:
    off = r * REGION
    for _ in range(REGION // CHUNK):
        n = os.pwritev(src_fd, [buf], off)
        assert n == CHUNK, "short write at region %d" % r
        off += CHUNK

os.fsync(src_fd); os.fsync(dst_fd)
os.close(src_fd); os.close(dst_fd)
print("write_regions=%s" % write_regions)
EOF_W
    local rc=$?
    [ "$rc" -eq 0 ] || _fail "writer failed (rc=$rc)"
    # Drop the page cache so step 6's metadata zeroing and dm-clone see the
    # device, not cache.
    _ssh "${SSH_USER}@${CN0_IP}" \
        "sudo blockdev --flushbufs '$SRC'; sudo blockdev --flushbufs '$DST'" \
        >/dev/null 2>&1 || _warn "flushbufs failed (continuing)"
    _info "steps 2-4: filled src=0x55, dst=0xaa, 8 regions zeroed on src"
}

# ===================== step 6 -- zero meta, modprobe, create dm-clone =======
step6_create() {
    _info "step 6: creating dm-clone (no_discard_passdown, no_hydration, region 4M)"
    _t "step6" _ssh "${SSH_USER}@${CN0_IP}" "
        sudo modprobe dm-clone
        sudo blockdev --flushbufs ${META}
        sudo dd if=/dev/zero of=${META} bs=1M count=4 conv=fsync status=none
        sudo dmsetup create ${DM_NAME} --table \
            '0 ${SEC_CLONE_DATA} clone ${META} ${DST} ${SRC} ${SEC_CLONE_REGION} 2 no_discard_passdown no_hydration'
        sudo dmsetup info ${DM_NAME} | head -5
    " || _fail "could not create dm-clone device"
    _info "step 6: dm-clone created (no_discard_passdown, no_hydration, region 4M)"
}

# ===================== step 7 -- blkdiscard 120 non-write regions ============
# Re-derive the 8 seed-42 regions, take the complement (120 regions), coalesce
# consecutive indices into runs, one blkdiscard per run against the dm-clone
# device. no_discard_passdown keeps clone_dst's 0xaa on those regions.
step7_discard() {
    _info "step 7: blkdiscarding 120 non-write regions"
    local out
    out=$(_t "step7" _ssh "${SSH_USER}@${CN0_IP}" "sudo python3 -u -" <<'EOF_D'
import random, subprocess, sys

DEV = "/dev/mapper/dnv-clone"
REGION = 4 * 1024 * 1024
NR_REGIONS = 128
NR_WRITE = 8
SEED = 42

random.seed(SEED)
write_regions = set(random.sample(range(NR_REGIONS), NR_WRITE))
discard_regions = sorted(set(range(NR_REGIONS)) - write_regions)

# coalesce consecutive indices into [start, length_in_regions] runs
runs = []
for r in discard_regions:
    if runs and r == runs[-1][0] + runs[-1][1]:
        runs[-1][1] += 1
    else:
        runs.append([r, 1])

for start, nreg in runs:
    off = start * REGION
    length = nreg * REGION
    subprocess.run(["blkdiscard", "-o", str(off), "-l", str(length), DEV],
                   check=True)
print("discard_runs=%s" % runs)
EOF_D
    ) || _fail "blkdiscard step failed"
    _info "step 7: 120 regions discard-skipped ($(echo "$out" | sed -n 's/^discard_runs=//p'))"
}

# ===================== step 8 -- enable hydration ===========================
step8_enable() {
    _info "step 8: enabling hydration"
    _ssh "${SSH_USER}@${CN0_IP}" \
        "sudo dmsetup message ${DM_NAME} 0 enable_hydration" \
        || _fail "could not enable hydration"
    _info "step 8: hydration enabled"
}

# ===================== step 9 -- wait for hydration to complete =============
# Poll dmsetup status every 0.2s; parse field 3 (hydrated/total) and field 4
# (hydrating). Complete when hydrated==128 && hydrating==0. 60s timeout.
step9_wait() {
    _info "step 9: waiting for hydration to complete"
    local out rc
    out=$(_t "step9" _ssh "${SSH_USER}@${CN0_IP}" "sudo python3 -u -" <<'EOF_H'
import subprocess, sys, time

# dm-clone status emits two <n>/<n> tokens: the first is the metadata
# blocks ratio, the second is the regions ratio; the token right after
# the second ratio is the #hydrating count. Use the second ratio.
deadline = time.time() + 60
last = ""
while time.time() < deadline:
    out = subprocess.run(["dmsetup", "status", "dnv-clone"],
                         capture_output=True, text=True).stdout
    last = out
    parts = out.split()
    ratio_idxs = [i for i, t in enumerate(parts) if "/" in t]
    if len(ratio_idxs) >= 2:
        ri = ratio_idxs[1]
        hydrated, total = parts[ri].split("/")
        hydrated, total = int(hydrated), int(total)
        hydrating = int(parts[ri + 1])
        if hydrated == total and hydrating == 0:
            print(last.strip())
            sys.exit(0)
    time.sleep(0.2)
print(last.strip(), file=sys.stderr)
sys.exit(1)
EOF_H
    )
    rc=$?
    [ "$rc" -eq 0 ] || _fail "hydration did not complete in 60s (rc=$rc). last status: $out"
    _info "step 9: hydration complete ($(echo "$out" | tr -s ' '))"
}

# ===================== step 10 -- remove dm-clone ===========================
# Device is idle (hydration done). --force fallback absorbs a brief udev
# probing hold (no udev rule installed -> blkid may open the device).
step10_remove() {
    _info "step 10: removing dm-clone"
    _ssh "${SSH_USER}@${CN0_IP}" "
        for i in \$(seq 1 5); do
            sudo dmsetup remove ${DM_NAME} && break
            sudo dmsetup remove --force ${DM_NAME} && break
            sleep 0.2
        done
        sudo dmsetup info ${DM_NAME} >/dev/null 2>&1 && exit 1 || exit 0
    " || _fail "could not remove ${DM_NAME}"
    _info "step 10: dm-clone removed"
}

# ===================== step 11 -- Python verifier (O_DIRECT) ===============
# Re-derives the 8 seed-42 regions, reads each 4 MiB region from src and dst
# with O_DIRECT+preadv, compares against expected (0x00 on both for write
# regions, 0x55 src / 0xaa dst elsewhere). JSONL + summary, exit non-zero on
# any mismatch.
step11_verify() {
    _info "step 11: verifying src/dst contents"
    _t "step11" _ssh "${SSH_USER}@${CN0_IP}" "sudo python3 -u - '$SRC' '$DST'" <<'EOF_V'
import json, mmap, os, random, sys

SRC, DST = sys.argv[1], sys.argv[2]
REGION = 4 * 1024 * 1024
NR_REGIONS = 128
NR_WRITE = 8
SEED = 42

random.seed(SEED)
write_regions = set(random.sample(range(NR_REGIONS), NR_WRITE))

src_fd = os.open(SRC, os.O_RDONLY | os.O_DIRECT)
dst_fd = os.open(DST, os.O_RDONLY | os.O_DIRECT)
buf = mmap.mmap(-1, REGION)              # 4 MiB, page-aligned

failures = []
for r in range(NR_REGIONS):
    off = r * REGION
    n = os.preadv(src_fd, [buf], off); assert n == REGION
    src = bytes(buf)                      # snapshot (mmap slice -> bytes)
    n = os.preadv(dst_fd, [buf], off); assert n == REGION
    dst = bytes(buf)
    if r in write_regions:
        exp_src = exp_dst = b"\x00" * REGION
        ok = (src == exp_src) and (dst == exp_dst) and (src == dst)
    else:
        ok = (src == b"\x55" * REGION) and (dst == b"\xaa" * REGION)
    sys.stdout.write(json.dumps({
        "region": r, "class": "write" if r in write_regions else "discard",
        "status": "ok" if ok else "fail"}) + "\n")
    sys.stdout.flush()
    if not ok:
        failures.append(r)

os.close(src_fd); os.close(dst_fd)
if failures:
    sys.stdout.write("[FAIL] %d region(s) mismatched: %s\n"
                     % (len(failures), failures))
    sys.exit(1)
sys.stdout.write("[OK] 128/128 regions match\n")
EOF_V
    local rc=$?
    [ "$rc" -eq 0 ] || _fail "verifier reported mismatches (rc=$rc)"
    _info "step 11: 128/128 regions match"
}

# ===================== step 12 -- final cleanup + assertion =================
step12_cleanup() {
    _info "step 12: final cleanup"
    _cleanup
    _ssh "${SSH_USER}@${CN0_IP}" "
        sudo dmsetup ls 2>/dev/null | grep -q ${DM_NAME} && exit 1
        sudo losetup -j ${IMG_SRC} 2>/dev/null | grep -q . && exit 2
        sudo losetup -j ${IMG_DST} 2>/dev/null | grep -q . && exit 3
        sudo losetup -j ${IMG_META} 2>/dev/null | grep -q . && exit 4
        exit 0
    " || _fail "cleanup left leftovers on cn0 (rc=$?)"
    _info "[OK] cleanup complete"
}

# ===================== main =================================================
main() {
    local t0 t1
    t0=$(date +%s%N)

    _info "_cleanup: clean slate"
    _cleanup

    step1_loops
    steps_2_3_4
    step6_create
    step7_discard
    step8_enable
    step9_wait
    step10_remove
    step11_verify
    step12_cleanup

    t1=$(date +%s%N)
    _info "clone test complete in $(( (t1 - t0) / 1000000 ))ms"
    _info "[OK] clone test passed"
}

main "$@"; rc=$?
exit $rc
