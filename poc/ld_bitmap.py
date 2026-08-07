#!/usr/bin/env python3
# ld_bitmap.py -- derive 8 LD allocation bitmaps from thin-pool metadata dumps.
#
# Reads thin_dump XML for leg0 and leg1, walks the union of every
# <range_mapping data_begin=.. data_end=..> across all <device> elements, and
# for each allocated pool data block N derives which grp/ld/bit it lands on
# per the geometry in check_plan.md section 5:
#
#   grp    = N // 122          # 488 MiB / 4 MiB = 122 blocks per grp's thindata
#   blk    = N % 122
#   ld_bit = 3 + blk           # skip raid1-meta (1) + thinmeta (2) = 3 blocks
#
# A set bit on the ld0 (dn0) bitmap for (leg, grp) is also set on the ld1 (dn1)
# bitmap for the same (leg, grp): the grp's thindata lives on that grp's raid1
# mirror, so both sides hold the same block.
#
# Each .bit file is 125 ASCII '0'/'1' chars + trailing '\n' (126 bytes). Bit 0
# is leftmost (low offset, raid1 meta), bit 124 is rightmost (last thindata
# block of the 500 MiB LD). Empty pool -> all-zero bitmaps + a stderr warning,
# exit 0 (defensive, not a crash).
#
# Interface:  python3 ld_bitmap.py [<work_dir>]   (default /tmp/dnv-check/)

import os
import sys
import xml.etree.ElementTree as ET

# Geometry constants (mirror common.sh sizing; see check_plan.md section 5).
POOL_DATA_BLOCKS_PER_GRP = 122      # 488 MiB / 4 MiB
LD_BITS = 125                       # 500 MiB / 4 MiB
RESERVED_BITS = 3                   # raid1-meta (1) + thinmeta (2) blocks

# The 8 LDs: one per -real device. ld0 lives on dn0, ld1 on dn1; for each
# (leg, grp) pair both ld0 and ld1 get a bitmap (raid1 mirror). Filename
# follows the -real LV identity: dnv-<dn>-sp0-leg<leg>-grp<grp>-ld<ld>.bit
LD_TARGETS = [
    (dn, leg, grp, ld)
    for dn, ld in (("dn0", "ld0"), ("dn1", "ld1"))
    for leg in (0, 1)
    for grp in (0, 1)
]


def parse_allocations(xml_path):
    """Return the set of allocated pool data block IDs across all <device>
    elements in the thin_dump XML. Empty/no mappings -> empty set.

    Handles both forms thin-provisioning-tools emits:
      <range_mapping origin_begin=N data_begin=B length=L ...>   (B..B+L)
      <single_mapping origin_block=N data_block=B ...>            (single block B)
    Older versions used data_begin/data_end; this accepts both that and the
    data_begin/length form used by thin_dump 1.1.0+."""
    allocated = set()
    try:
        tree = ET.parse(xml_path)
    except (FileNotFoundError, ET.ParseError) as e:
        sys.stderr.write("[WARN] could not parse %s: %s\n" % (xml_path, e))
        return allocated
    for rm in tree.iter("range_mapping"):
        try:
            begin = int(rm.get("data_begin"))
            end = rm.get("data_end")
            if end is None:
                length = int(rm.get("length"))
                end = begin + length
            else:
                end = int(end)
        except (TypeError, ValueError):
            continue
        if end <= begin:
            continue
        for n in range(begin, end):
            allocated.add(n)
    for sm in tree.iter("single_mapping"):
        try:
            block = int(sm.get("data_block"))
        except (TypeError, ValueError):
            continue
        allocated.add(block)
    return allocated


def block_to_ld_bit(n):
    """Pool data block N -> (grp, ld_bit). Returns None if out of the
    2-grp x 122-block pool range (244 blocks total)."""
    if n < 0 or n >= 2 * POOL_DATA_BLOCKS_PER_GRP:
        return None
    grp = n // POOL_DATA_BLOCKS_PER_GRP
    blk = n % POOL_DATA_BLOCKS_PER_GRP
    return grp, RESERVED_BITS + blk


def empty_bitmap():
    return ["0"] * LD_BITS


def to_file(bits):
    return "".join(bits) + "\n"


def main(argv):
    work_dir = argv[1] if len(argv) > 1 else "/tmp/dnv-check/"
    work_dir = work_dir.rstrip("/") + "/"

    if not os.path.isdir(work_dir):
        os.makedirs(work_dir)

    # bitmaps[(dn, leg, grp, ld)] -> list of 125 '0'/'1'
    bitmaps = {}
    for dn, leg, grp, ld in LD_TARGETS:
        bitmaps[(dn, leg, grp, ld)] = empty_bitmap()

    total_allocated = 0
    for leg in (0, 1):
        xml_path = os.path.join(work_dir, "thin_dump_leg%d.xml" % leg)
        allocated = parse_allocations(xml_path)
        total_allocated += len(allocated)
        if not allocated:
            sys.stderr.write("[WARN] leg%d: no allocated pool blocks in %s\n"
                             % (leg, xml_path))
            continue
        for n in sorted(allocated):
            res = block_to_ld_bit(n)
            if res is None:
                sys.stderr.write("[WARN] leg%d: pool block %d out of range, "
                                 "skipping\n" % (leg, n))
                continue
            grp, ld_bit = res
            if ld_bit >= LD_BITS:
                sys.stderr.write("[WARN] leg%d: block %d -> ld_bit %d out of "
                                 "range, skipping\n" % (leg, n, ld_bit))
                continue
            # raid1 mirror: set the bit on both ld0 (dn0) and ld1 (dn1) for
            # this (leg, grp).
            bitmaps[("dn0", leg, grp, "ld0")][ld_bit] = "1"
            bitmaps[("dn1", leg, grp, "ld1")][ld_bit] = "1"

    if total_allocated == 0:
        sys.stderr.write("[WARN] empty pool: all 8 bitmaps are zero\n")

    for dn, leg, grp, ld in LD_TARGETS:
        name = "dnv-%s-sp0-leg%d-grp%d-%s" % (dn, leg, grp, ld)
        path = os.path.join(work_dir, name + ".bit")
        with open(path, "w") as f:
            f.write(to_file(bitmaps[(dn, leg, grp, ld)]))

    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
