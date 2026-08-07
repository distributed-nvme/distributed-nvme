# check.sh Plan — Thin-Pool Block-Allocation Audit

## 1. Goal

Create `check.sh` + `poc/ld_bitmap.py` under `poc/` that, after `setup.sh all`
+ a short `host0_io.sh` run, audits the thin-pool's allocated data blocks
against what was physically written to the LDs on cn0. The audit dumps each
leg's thin-pool metadata with `thin_dump`, derives an 8-LD bitmap (one bit per
4 MiB LD block), then on cn0 reads each marked 4 MiB block directly from the
LDs and confirms it is non-zero (i.e. actually written, not thin-pool
zero-fill for an unallocated region).

The plan also retunes three dm constants (raid1 region_size, thin-pool
data_block_size, raid0 chunk_size) to match the 4 MiB bitmap granularity, and
audits that all meta devices are already multiples of 4 MiB.

Workflow (never combined with failover or grow):

```
setup.sh all  ->  host0_io.sh start (~10s) -> host0_io.sh stop  ->  ./check.sh
```

## 2. Decisions resolved (grilling interview)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Requirement #4 (meta sizes multiples of 4MB) | Audit-only: all `SEC_RMETA`/`SEC_TMETA`/`SEC_POOL_META` (and every other `SEC_*`) are already multiples of 4 MiB; no `SEC_*` edits required |
| 2 | The three tunables | Change only `RAID_REGION_SECTORS`/`POOL_BLOCK_SECTORS`/`RAID0_CHUNK_SECTORS` in `common.sh` lines 77-79 + add unit-notation comments. Inlined tables in `common.sh`/`failover.sh`/`grow.sh` reference the variable names and pick up the new values via `_emit_vars` automatically |
| 3 | Number of bitmaps | 8, one per `-real` LD (`dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>-real`): 4 on dn0 (ld0), 4 on dn1 (ld1) |
| 4 | Tool for "blocks written" | `thin_dump` (not `thin_ls`). Emits full b-tree mapping XML |
| 5 | Consistent dump | Suspend pool -> `thin_dump` the concat thinmeta -> resume. Per leg. Pool is idle (host0_io stopped) so no in-flight I/O; suspend forces metadata commit |
| 6 | thin-provisioning-tools install | Skipped: `thin_dump` is already on cn0. Call it directly |
| 7 | Bitmap coverage | Option A: full 125-block LD bitmap (500 MiB / 4 MiB = 125). Bits 0..2 always 0 (raid1-meta + 8 MiB thinmeta), bits 3..124 reflect thindata allocation |
| 8 | Bitmap file format | Bitstring text, 125 ASCII '0'/'1' chars + trailing `\n` (126 bytes). Bit 0 = leftmost (low offset), bit 124 = rightmost. Human-readable, `diff`-able, matches the repo's plain-text artifact convention |
| 9 | Bitmap filenames | `dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>.bit` (matches `-real` LV identity) |
| 10 | XML dump filenames | `thin_dump_leg0.xml`, `thin_dump_leg1.xml` |
| 11 | Working dir | `/tmp/dnv-check/` on the orchestrator (and on cn0 for the Phase 3 bitmaps). Cleaned at start of each `check.sh` run |
| 12 | Step 10 verification | Non-zero check (Option a): for each set bit, read the 4 MiB block, confirm at least one byte is non-zero. Not byte-pattern replay (Option b) — too fragile to host0_io's stripe pattern |
| 13 | Read mechanism on cn0 | Python `os.O_DIRECT` + `preadv` on a 4 MiB page-aligned `mmap` buffer (NOT `dd`; `dd iflag=direct` is broken per AGENTS.md). Consistent with `host0_io.sh` |
| 14 | Phase 3 architecture | Option A: scp the 8 bitmaps to cn0, emit a minimal Python verifier heredoc on cn0, run it there, capture JSON report. One SSH round-trip |
| 15 | Verification scope | Set-bits-only: read only blocks the bitmap marks as written. No reverse check on unset bits |
| 16 | XML parsing | Union of `[data_begin, data_end)` ranges across ALL `<device>` elements (not just dev_id=0). Handles future tds; semantically a data block is "written" if any thin device references it |
| 17 | Empty-pool handling | Defensive: empty XML / no mappings -> all-zero bitmaps + warning, not a crash |
| 18 | Block->bit formula | For leg L, allocated pool data block `N`: `grp = N // 122`, `blk = N % 122` (488 MiB / 4 MiB = 122), set bit `3 + blk` on both `ld0` (dn0) and `ld1` (dn1) for that `(leg,grp)` (raid1 mirror = same blocks on both sides) |
| 19 | check.sh structure | `source ./common.sh`; no args; precondition check; clean `/tmp/dnv-check/` at start; 3 phases each wrapped with `_t` + `slow_summary` |
| 20 | Phase 1 SSH style | Option B: minimal direct SSH (no `_emit_common` heredoc). Only suspend/dump/resume; capture thin_dump stdout; silence dmsetup status noise |
| 21 | Phase 3 SSH style | Minimal heredoc (no `_emit_common`); bake LD device paths + bitmap dir in as Python literals |
| 22 | Verifier output | JSONL: one record per LD `{ld, set, verified, zero, status}` + a final human summary line. Matches `host0_io.sh`'s JSONL convention |
| 23 | Verifier statuses | `ok` (set blocks all non-zero), `empty` (no set blocks; OK), `missing_device` (fail), `read_error` (fail), `fail` (zero mismatch) |
| 24 | Exit code | `check.sh` exit 0 only if all 8 LDs report `ok` or `empty`; non-zero on any failure (precondition missing, thin_dump error, XML parse error, missing device, read error, zero mismatch) |
| 25 | grow.sh stale comment | Fix `grow.sh:145` comment: literal `128` -> `$POOL_BLOCK_SECTORS` so the illustrative comment matches the actual table line and never goes stale again |
| 26 | note.md sync | No note.md edit for the tuning change (architecture unchanged, §4 size table unaffected). Add one row to §11 Scripts table for `check.sh` |
| 27 | ld_bitmap.py interface | One positional arg (the work dir), defaulting to `/tmp/dnv-check/` |
| 28 | Execution flow | Step 5 = `./setup.sh all`; Step 6 = `start`, wait 10 s, `stop` (enough for thousands of I/Os and multiple full rotations of the 64 slots; allocations stable after that) |

## 3. Files changed

### 3.1 `common.sh` -- retune three constants (lines 77-79)

Before:
```bash
POOL_BLOCK_SECTORS=128   #  64K thin-pool allocation block
RAID_REGION_SECTORS=128
RAID0_CHUNK_SECTORS=128
```

After:
```bash
POOL_BLOCK_SECTORS=8192   # 4M thin-pool data_block_size (8192 * 512B = 4MiB)
RAID_REGION_SECTORS=8192  # 4M raid1 region_size      (8192 * 512B = 4MiB)
RAID0_CHUNK_SECTORS=32    # 16K raid0 chunk_size      (32 * 512B = 16KiB)
```

No `SEC_*` changes. The `_emit_vars` block (lines 125-127) ships these to each
node; inlined tables in `common.sh:1153` (raid1), `common.sh:1217`
(thin-pool), `common.sh:1369` (raid0), `failover.sh:276` (cn1 rebuild raid0),
and `grow.sh:147` (leg0 pool extension) reference the variable names, so they
pick up the new values automatically.

Alignment audit (all already multiples of 4 MiB = 8192 sectors):

| object | sectors | MB | /4MB |
|---|---|---|---|
| `SEC_PD` (LD/LV) | 1024000 | 500 | 125 |
| `SEC_RMETA` (raid1 meta) | 8192 | 4 | 1 |
| `SEC_RDATA` (raid1 data) | 1015808 | 496 | 124 |
| `SEC_TMETA` (thinmeta/grp) | 16384 | 8 | 2 |
| `SEC_TDATA` (thindata/grp) | 999424 | 488 | 122 |
| `SEC_POOL_META` (concat) | 32768 | 16 | 4 |
| `SEC_POOL_DATA` (concat) | 1998848 | 976 | 244 |
| `SEC_TD` (thin virt) | 2097152 | 1024 | 256 |
| `SEC_EXP` (raid0) | 4194304 | 2048 | 512 |

The new `data_block_size=4MiB` divides `SEC_POOL_DATA` (976 MiB) and
`SEC_TDATA` (488 MiB) exactly. The new `region_size=4MiB` divides
`SEC_RDATA` (496 MiB) exactly. The raid0 `chunk_size=16KiB` is a power of 2
(no divisibility requirement). All kernel layout constraints satisfied.

Failover safety: `failover.sh` step 6 rebuilds cn1's raid1 by zeroing the 4 MiB
raid1 metadata areas (`zero_head <dev> 4` zeros 4 MiB = the whole metadata
device) and re-assembling with `nosync`. The `region_size` change is safe:
the bitmap structure changes but cn1 zeroes the metadata first and declares
the array in-sync (both sides were mirrored by cn0 up to the handover).

### 3.2 `grow.sh` -- fix stale comment (line 145)

Before:
```bash
#     0 2998272 thin-pool /dev/mapper/...-thinmeta /dev/mapper/...-thindata 128 0 1 skip_block_zeroing
```

After:
```bash
#     0 2998272 thin-pool /dev/mapper/...-thinmeta /dev/mapper/...-thindata $POOL_BLOCK_SECTORS 0 1 skip_block_zeroing
```

The illustrative comment now mirrors the actual table line (line 147) verbatim
and never goes stale on retune.

### 3.3 `note.md` -- add check.sh to §11 Scripts table

One new row:

| script | usage | what it does |
|---|---|---|
| `check.sh` | `./check.sh` | Thin-pool block-allocation audit. Dumps each leg's thin-pool metadata with `thin_dump`, derives 8 LD bitmaps (one bit per 4 MiB LD block), reads each marked block on cn0 and confirms it is non-zero. Run after `setup.sh all` + a short `host0_io.sh` run. Exit 0 = all allocated blocks verified written. |

No other note.md edit: the tuning change does not alter the architecture or
the §4 size table; §13 mentions `region_size`/`skip_block_zeroing`/`nosync`
conceptually but never quotes their numeric values.

### 3.4 `ld_bitmap.py` -- new script (Phase 2, step 9)

Standalone, stdlib-only (`xml.etree.ElementTree`). Interface:

```
python3 poc/ld_bitmap.py [<work_dir>]
    <work_dir> defaults to /tmp/dnv-check/
```

Reads `thin_dump_leg0.xml` + `thin_dump_leg1.xml` from `<work_dir>`, writes 8
bitstring files to `<work_dir>`:

```
dnv-dn0-sp0-leg0-grp0-ld0.bit
dnv-dn0-sp0-leg0-grp1-ld0.bit
dnv-dn0-sp0-leg1-grp0-ld0.bit
dnv-dn0-sp0-leg1-grp1-ld0.bit
dnv-dn1-sp0-leg0-grp0-ld1.bit
dnv-dn1-sp0-leg0-grp1-ld1.bit
dnv-dn1-sp0-leg1-grp0-ld1.bit
dnv-dn1-sp0-leg1-grp1-ld1.bit
```

Each file: 125 ASCII '0'/'1' chars + trailing `\n` (126 bytes). Bit 0 =
leftmost (LD offset `[0,4MiB)` = raid1-meta), bit 124 = rightmost (offset
`[496MiB,500MiB)` = last thindata block).

Parsing logic:

```
for leg in {0,1}:
    parse thin_dump_leg{leg}.xml
    allocated = union of [data_begin, data_end) over all <range_mapping>
                across all <device> elements  (empty pool -> empty set)
    for N in allocated:
        grp = N // 122          # 488MiB / 4MiB = 122 blocks per grp's thindata
        blk = N % 122
        ld_bit = 3 + blk        # skip raid1-meta (1) + thinmeta (2) = 3 blocks
        # raid1 mirror: same block on both sides (ld0 on dn0, ld1 on dn1)
        bitmap[dn0][leg][grp][ld0].set(ld_bit)
        bitmap[dn1][leg][grp][ld1].set(ld_bit)
```

Empty pool -> all-zero bitmaps + a warning on stderr, exit 0 (not a crash).

### 3.5 `check.sh` -- new orchestrator (Phase 1 + 2 + 3, step 8/9/10/11)

```bash
#!/bin/bash
set -uo pipefail
source "$(dirname "$0")/common.sh"
```

No args. Clean `/tmp/dnv-check/` at start (both orchestrator and cn0).

**Phase 1 -- dump thin-pool metadata (step 8):**

Precondition: `dm_exists dnv-cn0-sp0-leg{0,1}-thinpool` on cn0 via SSH,
else `_fail "run setup.sh all first"`.

Per leg (0,1): minimal direct SSH (no `_emit_common` heredoc):
```
sudo dmsetup suspend dnv-cn0-sp0-leg${leg}-thinpool 2>/dev/null
sudo thin_dump /dev/mapper/dnv-cn0-sp0-leg${leg}-thinmeta
sudo dmsetup resume dnv-cn0-sp0-leg${leg}-thinpool 2>/dev/null
```
Capture `thin_dump` stdout (the only thing on stdout) to a mktemp file,
validate non-empty, `mv` to `/tmp/dnv-check/thin_dump_leg${leg}.xml`. Wrap
with `_t "thin_dump leg$leg"`. `slow_summary` at phase end.

**Phase 2 -- derive 8 LD bitmaps (step 9):**

```
_t "ld_bitmap.py" python3 "$(dirname "$0")/ld_bitmap.py" /tmp/dnv-check/
```
Run locally on the orchestrator. Produces 8 `.bit` files in `/tmp/dnv-check/`.
`slow_summary` at phase end.

**Phase 3 -- verify written blocks on cn0 (step 10):**

`scp` the 8 `.bit` files to cn0 `/tmp/dnv-check/`. Emit a minimal Python
verifier heredoc on cn0 (no `_emit_common`); bake the 8 LD device paths
(`/dev/mapper/dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>-cn0`) and the bitmap dir
in as Python literals. Run it, capture JSONL output. Wrap with
`_t "verify LDs on cn0"`. `slow_summary` at phase end.

Verifier logic (per LD):
1. Read the 125-char bitstring from `/tmp/dnv-check/dnv-<dn>-...-ld<ld>.bit`.
2. Open `/dev/mapper/dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>-cn0` with `O_RDWR|O_DIRECT` (or `O_RDONLY`).
3. `mmap.mmap(-1, 4*1024*1024)` a 4 MiB page-aligned buffer.
4. For each set bit B (0..124): `os.preadv(fd, [buf], B * 4MiB)`, check the buffer has at least one non-zero byte.
5. Emit one JSON line: `{"ld":"dnv-...","set":N,"verified":M,"zero":K,"status":"ok|empty|missing_device|read_error|fail"}`.

Statuses:
- `ok`: set blocks all non-zero (set == verified, zero == 0).
- `empty`: no set blocks (set == 0); OK, not a failure.
- `missing_device`: `/dev/mapper/...-cn0` does not exist; failure.
- `read_error`: `preadv` raised `OSError`; failure.
- `fail`: a set block read back all-zeros (zero > 0); failure.

After the verifier runs, `check.sh`:
- Prints each LD's JSON line.
- Prints a final summary line: `[OK] N/8 LDs verified, 0 zero-block mismatches` or `[FAIL] M LDs failed: <list>`.
- Exit 0 only if all 8 LDs report `ok` or `empty`; non-zero otherwise.

## 4. Not changed

- `setup.sh` -- no edits. The verify path reads `dmsetup status` (kernel
  values, not hardcoded), so the new tunables flow through transparently.
- `failover.sh` -- no edits. The inlined raid0 table (line 276) references
  `$RAID0_CHUNK_SECTORS`, which picks up the new value via `_emit_vars`.
- `host0_io.sh` -- no edits. The I/O engine is tunable-agnostic.
- `teardown.sh` -- no edits.
- All `SEC_*` constants -- no edits (audit confirmed all meta devices are
  already multiples of 4 MiB).

## 5. Geometry reference (for the verifier)

Within one LD (`dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>-real`, 500 MiB):

```
offset  0       4MiB    8MiB    12MiB                 500MiB
        |-------|-------|-------|----------------------|
        | raid1 | thinmeta      | thindata (488 MiB)   |
        | meta  | (8 MiB)       | 122 x 4MiB blocks    |
        | (4MiB)|               |                      |
        | bit 0 | bit 1 | bit 2 | bit 3 .. bit 124     |
```

- Block 0 = `[0, 4MiB)` = raid1 metadata (kernel-managed; bit always 0).
- Block 1 = `[4MiB, 8MiB)` = thinmeta part 1 (kernel-managed; bit always 0).
- Block 2 = `[8MiB, 12MiB)` = thinmeta part 2 (kernel-managed; bit always 0).
- Block 3 = `[12MiB, 16MiB)` = thindata block 0 (first host-writable block).
- ...
- Block 124 = `[496MiB, 500MiB)` = thindata block 121 (last).

Per leg, the thin-pool's 976 MiB thindata concat = grp0-thindata (488 MiB) +
grp1-thindata (488 MiB) = 244 data blocks (IDs 0..243, each 4 MiB):
- IDs 0..121 -> grp0-thindata -> LD bit `3 + ID`.
- IDs 122..243 -> grp1-thindata -> LD bit `3 + (ID - 122)`.

Each grp's thindata lives on that grp's raid1, mirrored across ld0 (dn0) and
ld1 (dn1). So a set bit on the `ld0` bitmap for `(leg,grp)` is also set on the
`ld1` bitmap for the same `(leg,grp)`.

## 6. Expected result (10 s of host0_io)

`host0_io.sh` writes 1 MiB at rotating slots 0..63 MiB + a 1 MiB anchor at
512 MiB. Through the raid0 (chunk=16 KiB, 2 legs), each 1 MiB host write
stripes across both legs (~512 KiB per leg per write). The 64 MiB rotating
range stripes to ~32 MiB per leg, fitting entirely within grp0's 488 MiB
thindata. The anchor at 512 MiB -> one 4 MiB block per leg (thin device block
128 -> pool data block 128 -> grp1, since 128 >= 122).

So per leg, ~9-17 allocated 4 MiB blocks (rotating slots + the anchor), all
in grp0's thindata (IDs 0..121) plus one in grp1 (the anchor at ID 128).
grp1's LD bitmaps are nearly all-zero except the anchor block. grp0's LD
bitmaps have ~8-16 set bits.

Total verification work: ~9-17 set bits per non-empty LD x 8 LDs = ~72-136
reads of 4 MiB. Trivial volume.

Every allocated 4 MiB block contains at least one host-written 16 KiB raid0
stripe carrying a non-zero byte pattern (host0_io's printable alphabet). The
non-zero check is valid: it never produces false failures on this write
pattern.

Expected `check.sh` output: exit 0, ~4 LDs with `ok` (grp0 LDs on dn0+dn1 for
both legs, each with ~8-16 set blocks), ~4 LDs with `empty` or near-empty
(grp1 LDs), 0 zero-block mismatches.

## 7. Execution flow (on the live 6-VM testbed)

```
1. (apply the file edits above)
2. ./setup.sh all              # build with new tunables
3. ./host0_io.sh start; sleep 10; ./host0_io.sh stop
4. ./check.sh                  # expects exit 0
```
