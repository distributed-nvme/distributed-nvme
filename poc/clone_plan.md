# clone.sh Plan — dm-clone Standalone Test

## 1. Goal

Create `clone.sh` under `poc/` that exercises `dm-clone` end-to-end on cn0 as a
standalone test (sources nothing, never invokes setup/teardown). It builds two
512 MiB loop devices (`clone_src`, `clone_dst`) plus a 4 MiB metadata loop
(`clone_meta`), pre-fills src with `0x55` and dst with `0xaa`, writes `0x00`
to 8 randomly-selected (seed 42) 4 MiB regions on src, creates a dm-clone
device with `no_discard_passdown` + `no_hydration` and 4 MiB region size,
`blkdiscard`s the 120 non-written regions (so dm-clone skip-hydrates them),
enables hydration, waits for it to finish, removes the dm-clone device, then
verifies that src and dst agree on the 8 written regions (both `0x00`) and
that src is `0x55` and dst is `0xaa` everywhere else, and finally tears down
all three loops + backing files.

Reference: https://origin.kernel.org/doc/html/latest/admin-guide/device-mapper/dm-clone.html

Workflow (standalone; no POC state required on cn0 beyond ssh + sudo):

```
./clone.sh                # runs from poc/, SSHes to cn0 for every action
```

## 2. Decisions resolved (grilling interview)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Sourcing `common.sh` vs. self-contained | Fully self-contained (own `_ssh`/`_info`/`_warn`/`_fail`/`_t`/`slow_summary`, source nothing). Matches `host0_io.sh`; avoids pulling the 6-node topology's machinery into a single-node test |
| 2 | Invocation model | (b): `clone.sh` lives in `poc/`, runs locally, SSHes to cn0 (`CN0_IP=192.168.122.125`, `SSH_USER=yupeng`) for every action. Consistent with AGENTS.md "All scripts are run from `poc/` and SSH to the nodes" and with `host0_io.sh` |
| 3 | Naming prefix + udev rule | `dnv-clone-*` dm device name (`dnv-clone`) and `/var/tmp/dnv-clone-{src,dst,meta}.img` backing files. No udev rule installed: probing a plain loop-backed dm-clone is harmless (no dm-delay, no wedge); if a prior `setup.sh` run installed `58-dnv-test.rules`, the `dnv-` prefix gets the suppression for free |
| 4 | Idempotency / re-run safety | (a) teardown-at-start: an idempotent `_cleanup` runs first (remove stale `dnv-clone`, `losetup -d` any loops holding the imgs, `rm -f` the imgs) so every run starts from a clean slate. Required because the random-address step regenerates each run; stale state would corrupt verification. Step 12 re-runs `_cleanup` for symmetry |
| 5 | Geometry + step-4 write granularity | 512 MiB src/dst (1 048 576 sectors), 4 MiB meta (8 192 sectors), region_size 4 MiB (8 192 sectors) -> 128 regions (0..127). Step 4 = 8 regions chosen by `random.seed(42); sorted(random.sample(range(128), 8))`, each **whole 4 MiB region** zeroed on clone_src. Region-aligned because dm-clone operates per-region; "the regions you wrote in step 4" maps directly to region indices |
| 6 | Pre-fill (steps 2 & 3) mechanism | Python O_DIRECT `pwritev` with a page-aligned `mmap` buffer filled with `0x55`/`0xaa`, looped over 1 MiB chunks across 512 MiB. Uniform with step 4 and note.md §13 ("use Python O_DIRECT, not dd"). Followed by `blockdev --flushbufs` |
| 7 | Steps 2-4 + step 11 I/O engine | One Python writer (steps 2-4) + one Python verifier (step 11), both re-deriving the 8 seed-42 regions. O_DIRECT throughout (`os.O_RDWR\|os.O_DIRECT` for writes, `os.O_RDONLY\|os.O_DIRECT` for reads), 4 MiB `mmap` buffer, per-region full-buffer compare, JSONL + summary output, exit non-zero on any mismatch (mirrors `check.sh` phase 3) |
| 8 | dm-clone table (step 6) + metadata prep | Zero clone_meta's first 4 MiB (buffered dd like `zero_head`), then `dmsetup create dnv-clone --table "0 1048576 clone <meta> <dst> <src> 8192 2 no_discard_passdown no_hydration"`. size 1 048 576 sectors; region 8 192; 2 feature args. Metadata zeroing mirrors `create_grp`'s raid1-meta zeroing |
| 9 | Step 7 blkdiscard | (b): re-derive 8 step-4 regions, take complement (120 regions), **coalesce consecutive indices in Python**, one `blkdiscard -o <bytes> -s <bytes*run_len>` per coalesced run against `/dev/mapper/dnv-clone`. Discards go to the dm-clone device (dm-clone interprets them); `no_discard_passdown` keeps clone_dst's `0xaa` untouched on those regions |
| 10 | Steps 8 & 9 | Step 8 = `dmsetup message dnv-clone 0 enable_hydration`. Step 9 = poll `dmsetup status dnv-clone` every 0.2 s, parse field 3 (`#hydrated/#total`) and field 4 (`#hydrating`), complete when `hydrated==128 && hydrating==0`, 60 s timeout -> `_fail` with last status, final status print |
| 11 | Steps 10 & 12 | Step 10 = `dmsetup remove dnv-clone` with `--force` fallback on EBUSY (no udev rule -> possible brief probing hold). Step 12 = re-run `_cleanup`, then assert no `dnv-clone*` dm devices and no loops attached to the imgs; print `[OK] cleanup complete` or `[FAIL]` |
| 12 | Script shape + error handling + pass/fail | `set -uo pipefail` (no `-e`); no-trap; `_fail` exits non-zero leaving state for inspection (re-run sweeps via start-of-run `_cleanup`). Step 7's `blkdiscard` calls invoked from inside the Python step via `subprocess.run` (Python runs as `sudo python3 -u -`, so `blkdiscard` needs no extra sudo). Final exit = verifier's result; success prints `[OK] clone test passed` |

## 3. Files changed

### 3.1 `clone.sh` -- new script (poc/clone.sh)

```bash
#!/bin/bash
set -uo pipefail
```

Self-contained (no `source`), defines its own:

```
CN0_IP="192.168.122.125"
SSH_USER="yupeng"
SEC_CLONE_DATA=1048576   # 512M (src + dst size, in 512-byte sectors)
SEC_CLONE_META=8192      #   4M (meta size)
SEC_CLONE_REGION=8192    #   4M (dm-clone region_size)
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
```

No `slow_summary` file (`_t` warns inline); the run is short. No `_emit_common`
heredoc -- each step is one `_ssh` invocation running a small bash or Python
snippet on cn0.

## 4. Conventions

- `set -uo pipefail` (no `-e`); errors handled explicitly with `_fail`/`_warn`
  (AGENTS.md "Conventions that differ from defaults").
- Self-contained (sources nothing; own helpers), like `host0_io.sh`.
- Runs locally in `poc/`, SSHes to cn0 for every action.
- I/O via Python `O_DIRECT` + `preadv`/`pwritev` on page-aligned `mmap`
  buffers (note.md §13: dd's `iflag=direct` is broken on these devices).
- Idempotent: `_cleanup` at start AND at step 12; safe on partial/clean cn0.
- No-trap; `_fail` exits non-zero leaving state for inspection (re-run sweeps
  via start-of-run `_cleanup`). Matches the POC orchestrators which don't trap.
- No setup/teardown scripts invoked; no udev rule installed; no `prep_node`
  call (we don't need nvmet/nvme modules, only `dm-clone`).
- dm device name `dnv-clone` and backing files `/var/tmp/dnv-clone-*.img`
  match the repo's `dnv-` prefix and `/var/tmp/dnv-*.img` convention
  (note.md §6, `create_pd`).

## 5. Geometry

| object | sectors | bytes | notes |
|---|---|---|---|
| clone_src loop | 1 048 576 | 512 MiB | backing `/var/tmp/dnv-clone-src.img` |
| clone_dst loop | 1 048 576 | 512 MiB | backing `/var/tmp/dnv-clone-dst.img` |
| clone_meta loop | 8 192 | 4 MiB | backing `/var/tmp/dnv-clone-meta.img` |
| dm-clone device | 1 048 576 | 512 MiB | size = src size; dst >= src (equal) |
| region_size | 8 192 | 4 MiB | valid: 8..2 097 152 sectors, power of two |
| total regions | 128 | -- | `SEC_CLONE_DATA / SEC_CLONE_REGION` |
| write regions | 8 | -- | `random.seed(42); random.sample(range(128), 8)` |
| discard regions | 120 | -- | complement of the 8 write regions |

All sizes in 512-byte sectors (AGENTS.md "All sizes are 512-byte sectors").
`region_size` 4 MiB divides the 512 MiB device exactly (128 regions).

dm-clone table (sector units, per the doc's constructor):

```
0 1048576 clone <meta_loop> <dst_loop> <src_loop> 8192 2 no_discard_passdown no_hydration
```

Field layout per the doc:
- `0 1048576` -- start sector + length
- `clone` -- target
- `<meta_loop> <dst_loop> <src_loop>` -- metadata / destination / source
- `8192` -- region size (sectors)
- `2` -- #feature args
- `no_discard_passdown no_hydration` -- feature args

`no_hydration` starts the device with background hydration disabled (so the
120 discards in step 7 land on un-hydrated regions and become skip markers
before any hydration begins). `no_discard_passdown` prevents dm-clone from
forwarding the discards to clone_dst, so clone_dst keeps its `0xaa` on the
discarded regions.

dm-clone status (per the doc):

```
<metadata block size> <#used metadata blocks>/<#total metadata blocks>
<region size> <#hydrated regions>/<#total regions> <#hydrating regions>
<#feature args> <feature args>* <#core args> <core args>*
<clone metadata mode>
```

Whitespace-split (0-indexed): field 3 = `"<hydrated>/<total>"` (e.g.
`128/128`), field 4 = `"<hydrating>"`. Step 9 parses these.

## 6. Step-by-step

### `_cleanup` (start) -- idempotent, safe on clean cn0

Over SSH on cn0:

```
sudo dmsetup info dnv-clone >/dev/null 2>&1 && sudo dmsetup remove --force dnv-clone >/dev/null 2>&1 || true
for img in /var/tmp/dnv-clone-src.img /var/tmp/dnv-clone-dst.img /var/tmp/dnv-clone-meta.img; do
    for loop in $(sudo losetup -j "$img" 2>/dev/null | awk -F: '{print $1}'); do
        sudo losetup -d "$loop" >/dev/null 2>&1 || true
    done
    sudo rm -f "$img" 2>/dev/null || true
done
```

### Step 1 -- create 3 loops

Over SSH on cn0:

```
sudo truncate -s 512M /var/tmp/dnv-clone-src.img
sudo truncate -s 512M /var/tmp/dnv-clone-dst.img
sudo truncate -s 4M   /var/tmp/dnv-clone-meta.img
SRC=$(sudo losetup -f --show /var/tmp/dnv-clone-src.img)
DST=$(sudo losetup -f --show /var/tmp/dnv-clone-dst.img)
META=$(sudo losetup -f --show /var/tmp/dnv-clone-meta.img)
echo "SRC=$SRC DST=$DST META=$META"   # parsed by the orchestrator
```

Orchestrator parses `SRC=`/`DST=`/`META=` from stdout for use in later steps.
(`losetup -f --show` finds a free loop and prints its path; the backing file
is freshly truncated so all three are zero-initialized.)

### Steps 2, 3, 4 -- Python writer (one `sudo python3 -u -` via quoted heredoc on cn0)

Receives `$SRC`, `$DST` as argv (the resolved loop paths). Uses a 4 MiB
page-aligned `mmap` buffer and `os.O_RDWR | os.O_DIRECT` on both fds.

```python
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
print("write_regions=%s" % write_regions)   # for the run log
```

O_DIRECT alignment: buffer (mmap, page-aligned), length (1 MiB, multiple of
512), and offset (1 MiB or 4 MiB, multiples of 512) all satisfy the loop
device's logical block size. After the writer returns, the orchestrator runs
`sudo blockdev --flushbufs $SRC; sudo blockdev --flushbufs $DST` to drop the
page cache so step 6's metadata zeroing and dm-clone see the device, not cache.

### Step 6 -- zero clone_meta, modprobe dm-clone, create dm-clone

Over SSH on cn0 (using the `$SRC`/`$DST`/`$META` resolved in step 1):

```
sudo modprobe dm-clone                         # not in prep_node; load it ourselves
sudo blockdev --flushbufs $META               # drop any page cache
sudo dd if=/dev/zero of=$META bs=1M count=4 conv=fsync status=none   # zero 4M, like zero_head
sudo dmsetup create dnv-clone --table "0 1048576 clone $META $DST $SRC 8192 2 no_discard_passdown no_hydration"
sudo dmsetup info dnv-clone | head -5         # sanity: State, Blkdev, etc.
```

`modprobe dm-clone`: the POC's `prep_node` loads `dm-mod dm-delay
dm-thin-pool dm-raid` but NOT `dm-clone`; clone.sh must load it itself.
Zeroing the 4 MiB metadata device matches `create_grp`'s raid1-meta zeroing
(`zero_head ... 4`) -- dm-clone reuses the thin-provisioning metadata library,
which formats a fresh superblock only when the metadata device is all zeros.

### Step 7 -- blkdiscard the 120 non-step-4 regions (Python on cn0, sudo)

Re-derive the 8 seed-42 regions (identical to step 4), compute the complement
(the 120 non-write regions), coalesce consecutive indices into runs, and for
each run invoke `blkdiscard` against `/dev/mapper/dnv-clone`:

```python
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
    subprocess.run(["blkdiscard", "-o", str(off), "-s", str(length), DEV],
                   check=True)
print("discard_runs=%s" % runs)
```

`blkdiscard -o`/`-s` take **bytes**. `no_discard_passdown` (step 6) prevents
dm-clone from forwarding the discards to clone_dst, so clone_dst keeps `0xaa`
on the discarded regions. The 8 step-4 regions are NOT discarded, so they stay
un-hydrated and will be hydrated (copied src->dst) when step 8 enables
hydration.

### Step 8 -- enable hydration

```
sudo dmsetup message dnv-clone 0 enable_hydration
```

(`0` is the sector-offset argument `dmsetup message` requires; for dm-clone
messages are not position-specific.)

### Step 9 -- wait for hydration to complete

Poll `dmsetup status dnv-clone` every 0.2 s; parse the
`<#hydrated regions>/<#total regions>` (field 3) and `<#hydrating regions>`
(field 4); complete when `hydrated == 128 && hydrating == 0`. 60 s timeout ->
`_fail` with the last status line. Print final status.

```python
import re, subprocess, sys, time

status_re = re.compile(r"(\d+)/(\d+)\s+(\d+)")
deadline = time.time() + 60
last = ""
while time.time() < deadline:
    out = subprocess.run(["dmsetup", "status", "dnv-clone"],
                         capture_output=True, text=True).stdout
    last = out
    m = status_re.search(out)
    if m:
        hydrated, total, hydrating = int(m.group(1)), int(m.group(2)), int(m.group(3))
        if hydrated == total and hydrating == 0:
            print(last.strip())
            sys.exit(0)
    time.sleep(0.2)
print(last.strip(), file=sys.stderr)
sys.exit(1)
```

Only 8 regions (32 MiB) actually need copying; the 120 are skip-hydrated via
the step-7 discards, so on local loop devices hydration finishes in well under
a second. 60 s is a generous safety margin.

### Step 10 -- remove dm-clone

```
for i in $(seq 1 5); do
    sudo dmsetup remove dnv-clone && break
    sudo dmsetup remove --force dnv-clone && break
    sleep 0.2
done
sudo dmsetup info dnv-clone >/dev/null 2>&1 && _fail "could not remove dnv-clone"
```

The device is idle (hydration done, no in-flight I/O). `--force` fallback
handles a brief udev probing hold (no udev rule installed -> `blkid` may open
the device on create/remove).

### Step 11 -- Python verifier (one `sudo python3 -u -` via quoted heredoc on cn0)

Receives `$SRC`, `$DST` (resolved loop paths) as argv. With a 4 MiB `mmap`
buffer and `os.O_RDONLY | os.O_DIRECT` on both:

```python
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
    src = bytes(buf)                      # copy (mmap slice -> bytes)
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
```

Copies each 4 MiB read into `bytes` before the second preadv reuses the
buffer. Full-region compare catches partial overwrites; 128 regions x 4 MiB =
512 MiB of reads total, trivial.

### Step 12 -- final cleanup + assertion

Re-run `_cleanup` (same function as at start). Then assert no leftovers:

```
sudo dmsetup ls 2>/dev/null | grep -q dnv-clone && _fail "dm device dnv-clone still present"
sudo losetup -j /var/tmp/dnv-clone-src.img 2>/dev/null | grep -q . && _fail "loop still attached to src img"
sudo losetup -j /var/tmp/dnv-clone-dst.img 2>/dev/null | grep -q . && _fail "loop still attached to dst img"
sudo losetup -j /var/tmp/dnv-clone-meta.img 2>/dev/null | grep -q . && _fail "loop still attached to meta img"
_info "[OK] cleanup complete"
```

### Final

`main "$@"; rc=$?; exit $rc`. On success (verifier exit 0 + cleanup OK), print
`[OK] clone test passed`.

## 7. Expected per-region post-state (the invariant step 11 checks)

After step 10 (dm-clone removed), clone_src and clone_dst hold:

| region class | count | clone_src | clone_dst | why |
|---|---|---|---|---|
| step-4 (write) regions | 8 | `0x00` | `0x00` | step 4 zeroed src; not discarded -> hydrated (copied src->dst) in step 9; src is read-only, dst received the copy |
| non-step-4 (discard) regions | 120 | `0x55` | `0xaa` | src untouched from step 2; dst untouched from step 3; step-7 discard skip-hydrated them (metadata-only, `no_discard_passdown` left dst alone) |

This is the dm-clone contract: discards to un-hydrated regions skip the copy
(metadata-only), and `no_discard_passdown` prevents the discard from reaching
the destination device. The 8 non-discarded regions are hydrated normally
(src->dst copy). Step 11 verifies both classes byte-for-byte.

## 8. Risks / dependencies

- **`dm-clone` module**: `prep_node` loads `dm-mod dm-delay dm-thin-pool
  dm-raid` but NOT `dm-clone`. Step 6 runs `sudo modprobe dm-clone` before
  `dmsetup create`. If the module is unavailable (kernel without
  `CONFIG_DM_CLONE`), `modprobe` fails and step 6 `_fail`s clearly.
- **`blkdiscard`**: from util-linux; expected present on cn0. If missing,
  step 7 fails clearly on the first `subprocess.run` (`FileNotFoundError`).
- **O_DIRECT on loop**: loop devices forward O_DIRECT to the backing file
  (supported in modern kernels). Should work here; if it failed, the writer
  would error on the first `pwritev` (no silent corruption). `check.sh` and
  `host0_io.sh` already use the same `os.O_DIRECT` + `preadv`/`pwritev` +
  page-aligned `mmap` pattern on this testbed.
- **`losetup -f --show`**: finds a free loop device and prints its path; the
  orchestrator parses `SRC=`/`DST=`/`META=` from the ssh stdout. If a loop is
  already attached to one of the imgs (shouldn't be, `_cleanup` ran first),
  `losetup -f` skips it and picks a fresh one for the freshly-truncated file.
- **udev probing**: no udev rule installed. `dmsetup create`/`remove` may
  trigger a brief `blkid` probe (harmless, no dm-delay). The `--force`
  fallback in step 10 and the retry loop absorb any EBUSY.

## 9. Execution flow

```
1. (apply: create poc/clone.sh as specified above)
2. ./clone.sh                 # from poc/, expects exit 0
```

Expected output (abridged):

```
[INFO] _cleanup: clean slate
[INFO] step 1: loops created (SRC=... DST=... META=...)
[INFO] steps 2-4: filled src=0x55, dst=0xaa, 8 regions zeroed on src
[INFO] step 6: dm-clone created (no_discard_passdown, no_hydration, region 4M)
[INFO] step 7: 120 regions discard-skipped (N coalesced runs)
[INFO] step 8: hydration enabled
[INFO] step 9: hydration complete (128/128, 0 hydrating)
[INFO] step 10: dm-clone removed
[INFO] step 11: 128/128 regions match
[INFO] step 12: _cleanup: clean slate
[INFO] [OK] clone test passed
```

Exit 0 only if every step succeeded AND the verifier reported all 128 regions
matching AND `_cleanup` left no leftovers. On any `_fail`, exits non-zero with
`[FAIL] <msg>` on stderr, leaving state on cn0 for inspection (re-run
recovers via start-of-run `_cleanup`).
