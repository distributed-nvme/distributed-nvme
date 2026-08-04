#!/usr/bin/env python3
# vd_bitmap.py -- derive 8 VD allocation bitmaps from thin-pool metadata dumps.
#
# Reads thin_dump XML for leg0 and leg1, walks the union of every
# <range_mapping data_begin=.. data_end=..> across all <device> elements, and
# for each allocated pool data block N derives which grp/vd/bit it lands on
# per the geometry in check_plan.md section 5:
#
#   grp    = N // 122          # 488 MiB / 4 MiB = 122 blocks per grp's thindata
#   blk    = N % 122
#   vd_bit = 3 + blk           # skip raid1-meta (1) + thinmeta (2) = 3 blocks
#
# A set bit on the vd0 (dn0) bitmap for (leg, grp) is also set on the vd1 (dn1)
# bitmap for the same (leg, grp): the grp's thindata lives on that grp's raid1
# mirror, so both sides hold the same block.
#
# Each .bit file is 125 ASCII '0'/'1' chars + trailing '\n' (126 bytes). Bit 0
# is leftmost (low offset, raid1 meta), bit 124 is rightmost (last thindata
# block of the 500 MiB VD). Empty pool -> all-zero bitmaps + a stderr warning,
# exit 0 (defensive, not a crash).
#
# Interface:  python3 vd_bitmap.py [<work_dir>]   (default /tmp/dnv-check/)

import os
import sys
import xml.etree.ElementTree as ET

# Geometry constants (mirror common.sh sizing; see check_plan.md section 5).
POOL_DATA_BLOCKS_PER_GRP = 122      # 488 MiB / 4 MiB
VD_BITS = 125                       # 500 MiB / 4 MiB
RESERVED_BITS = 3                   # raid1-meta (1) + thinmeta (2) blocks

# The 8 VDs: one per -real device. vd0 lives on dn0, vd1 on dn1; for each
# (leg, grp) pair both vd0 and vd1 get a bitmap (raid1 mirror). Filename
# follows the -real LV identity: dnv-<dn>-da0-leg<leg>-grp<grp>-vd<vd>.bit
VD_TARGETS = [
    (dn, leg, grp, vd)
    for dn, vd in (("dn0", "vd0"), ("dn1", "vd1"))
    for leg in (0, 1)
    for grp in (0, 1)
]


def parse_allocations(xml_path):
    """Return the set of allocated pool data block IDs across all <device>
    elements in the thin_dump XML. Empty/no mappings -> empty set."""
    allocated = set()
    try:
        tree = ET.parse(xml_path)
    except (FileNotFoundError, ET.ParseError) as e:
        sys.stderr.write("[WARN] could not parse %s: %s\n" % (xml_path, e))
        return allocated
    for rm in tree.iter("range_mapping"):
        try:
            begin = int(rm.get("data_begin"))
            end = int(rm.get("data_end"))
        except (TypeError, ValueError):
            continue
        if end <= begin:
            continue
        for n in range(begin, end):
            allocated.add(n)
    return allocated


def block_to_vd_bit(n):
    """Pool data block N -> (grp, vd_bit). Returns None if out of the
    2-grp x 122-block pool range (244 blocks total)."""
    if n < 0 or n >= 2 * POOL_DATA_BLOCKS_PER_GRP:
        return None
    grp = n // POOL_DATA_BLOCKS_PER_GRP
    blk = n % POOL_DATA_BLOCKS_PER_GRP
    return grp, RESERVED_BITS + blk


def empty_bitmap():
    return ["0"] * VD_BITS


def to_file(bits):
    return "".join(bits) + "\n"


def main(argv):
    work_dir = argv[1] if len(argv) > 1 else "/tmp/dnv-check/"
    work_dir = work_dir.rstrip("/") + "/"

    if not os.path.isdir(work_dir):
        os.makedirs(work_dir)

    # bitmaps[(dn, leg, grp, vd)] -> list of 125 '0'/'1'
    bitmaps = {}
    for dn, leg, grp, vd in VD_TARGETS:
        bitmaps[(dn, leg, grp, vd)] = empty_bitmap()

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
            res = block_to_vd_bit(n)
            if res is None:
                sys.stderr.write("[WARN] leg%d: pool block %d out of range, "
                                 "skipping\n" % (leg, n))
                continue
            grp, vd_bit = res
            if vd_bit >= VD_BITS:
                sys.stderr.write("[WARN] leg%d: block %d -> vd_bit %d out of "
                                 "range, skipping\n" % (leg, n, vd_bit))
                continue
            # raid1 mirror: set the bit on both vd0 (dn0) and vd1 (dn1) for
            # this (leg, grp).
            bitmaps[("dn0", leg, grp, "vd0")][vd_bit] = "1"
            bitmaps[("dn1", leg, grp, "vd1")][vd_bit] = "1"

    if total_allocated == 0:
        sys.stderr.write("[WARN] empty pool: all 8 bitmaps are zero\n")

    for dn, leg, grp, vd in VD_TARGETS:
        name = "dnv-%s-da0-leg%d-grp%d-%s" % (dn, leg, grp, vd)
        path = os.path.join(work_dir, name + ".bit")
        with open(path, "w") as f:
            f.write(to_file(bitmaps[(dn, leg, grp, vd)]))

    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
