#!/usr/bin/env python3
"""Decide whether a leg md has kicked is really dead, and if it is not, put it
back -- the way mdadm would, so the write-intent bitmap is actually used.

One pass:

  1. ask md          which legs it has kicked                      (sysfs)
  2. distrust md     probe each one directly, 3 rounds 10 s apart  (probe.py)
  3. repair          re-add / ADD_NEW_DISK / new_dev, in that order (mdctl.py)

Steps 1 and 2 are unchanged from the first version of this POC: md's verdict is
a trigger, never a diagnosis, because a RAID1 over NVMe-oF gets legs kicked for
reasons that have nothing to do with the disk.

Step 3 is the part that changed. Every repair now aims at
`rdev->saved_raid_disk >= 0`, which is the field raid1_add_disk() reads to
decide between copying the dirty bitmap chunks and copying the whole device.
`new_dev` -- the obvious sysfs hot-add -- can never set it, so it is now only
the last resort. See mdctl.py for the kernel-side detail.
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mdctl
import mdsb
import probe
from rig import MD, NAME, LEGS, PAUSE_FILE

INTERVAL = 60.0


def log(msg):
    sys.stdout.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))
    sys.stdout.flush()


def view():
    """(md state, legs) with md's verdict attached to each leg."""
    st = mdctl.read_md(MD)
    legs = []
    for leg in LEGS:
        L = dict(leg)
        L["rdev"] = st["rdevs"].get(mdctl.devname(leg["path"])) if st["present"] else None
        if not st["present"]:
            L["bad"], L["why"] = True, "array not assembled"
        elif L["rdev"] is None:
            L["bad"], L["why"] = True, "no rdev for slot %d" % leg["slot"]
        elif L["rdev"]["faulty"]:
            L["bad"], L["why"] = True, "md marked it faulty"
        elif not L["rdev"]["in_sync"]:
            rs = mdctl.rd(L["rdev"]["dir"] + "/recovery_start", "")
            L["bad"] = rs in ("", "none")
            L["why"] = "not in_sync" if L["bad"] else "recovering (%s)" % rs
        else:
            L["bad"], L["why"] = False, "in_sync"
        legs.append(L)
    return st, legs


def bitmap_note(leg):
    """What the metadata says about whether the bitmap can cover this leg. The
    kernel makes the same comparison itself inside super_1_validate(); we log it
    so the decision is visible rather than inferred from the outcome."""
    sb = mdsb.examine(leg["path"])
    if sb is None:
        return "no superblock"
    best = None
    for other in LEGS:
        if other["slot"] == leg["slot"]:
            continue
        bm = mdsb.bitmap_sb(other["path"])
        if bm:
            best = bm["events_cleared"] if best is None else max(best, bm["events_cleared"])
    if best is None:
        return "events=%d, no peer bitmap readable" % sb["events"]
    return ("events=%d vs peer events_cleared=%d -> bitmap %s cover the gap"
            % (sb["events"], best, "does" if sb["events"] >= best else "does NOT"))


def put_back(leg, st):
    """re-add, else ADD_NEW_DISK, else new_dev -- mdadm's order (Manage.c:1608
    then attempt_re_add()). Reports the sectors md actually copied, which is the
    only unambiguous way to tell the three apart."""
    log("  %s: %s" % (leg["node"], bitmap_note(leg)))
    before = mdctl.sectors_written(leg["path"])
    t0 = time.monotonic()

    rdev = leg["rdev"]
    ok = False
    if rdev is not None and rdev["faulty"]:
        ok, why = mdctl.re_add(rdev)
        log("  %s: %s" % (leg["node"], why))
    if not ok:
        if rdev is not None:
            mdctl.detach(rdev)
        ok, why = mdctl.add_new_disk(MD, leg["path"])
        log("  %s: %s" % (leg["node"], why))
    if not ok:
        ok, why = mdctl.hot_add(MD, leg["path"])
        log("  %s: %s" % (leg["node"], why))
    if not ok:
        return False

    for _ in range(900):                       # wait for the copy to finish
        s = mdctl.read_md(MD)
        if s["degraded"] == 0 and s["sync_action"] in (None, "idle"):
            break
        time.sleep(0.1)
    after = mdctl.sectors_written(leg["path"])
    mdctl.tunables(MD)
    log("  %s: back in %.2fs, md copied %s sectors onto it"
        % (leg["node"], time.monotonic() - t0,
           "?" if before is None or after is None else after - before))
    return True


def decide_assembly(sbs):
    """Both legs kicked: which superblock may start the array?

    Take the available leg with the highest events. If a member is missing, that
    leg's own dev_roles[] must already record the missing member as faulty --
    md writes the updated role map only to the survivors, so a superblock that
    still believes its absent partner was in_sync was written *before* that
    partner's last write. Starting from it would silently roll the data back.
    """
    if not sbs:
        return None, [], "no healthy leg has a readable superblock"
    best = max(sbs, key=lambda s: sbs[s]["events"])
    b = sbs[best]
    roles = " ".join("%d:%s" % (i, mdsb.role_str(r)) for i, r in enumerate(b["roles"][:len(LEGS)]))
    for leg in LEGS:
        if leg["slot"] in sbs:
            continue
        role = b["roles"][leg["slot"]] if leg["slot"] < len(b["roles"]) else mdsb.ROLE_SPARE
        if role not in (mdsb.ROLE_FAULTY, mdsb.ROLE_SPARE):
            return None, [], ("slot %d is absent, but the freshest leg we have (slot %d, "
                              "events=%d) still records it as role %s -- that leg dropped "
                              "out first and its data is stale; waiting"
                              % (leg["slot"], best, b["events"], mdsb.role_str(role)))
    # mdadm and super_1_validate() both tolerate an off-by-one event count.
    fresh = sorted(s for s in sbs if b["events"] - sbs[s]["events"] <= 1)
    return best, fresh, "freshest slot %d events=%d roles=[%s]; start from %s" % (
        best, b["events"], roles, fresh)


def reassemble(healthy):
    sbs = {}
    for l in healthy:
        sb = mdsb.examine(l["path"])
        if sb and sb["name"] == NAME:
            sbs[l["slot"]] = sb
            log("  %s sb: %s" % (l["node"], mdsb.summary(sb)))
    best, fresh, why = decide_assembly(sbs)
    log("  assembly: %s -- %s" % ("GO" if best is not None and fresh else "WAIT", why))
    if best is None or not fresh:
        return

    open(PAUSE_FILE, "w").close()              # let io_gen drop the array
    try:
        if mdctl.read_md(MD)["present"]:
            ok, why = mdctl.stop(MD)
            log("  stop %s: %s" % (MD, why))
            if not ok:
                return
        paths = [l["path"] for l in LEGS if l["slot"] in fresh]
        ok, why = mdctl.assemble(MD, paths)
        st = mdctl.read_md(MD)
        log("  assembled from slots %s -> %s degraded=%d" % (fresh, st["array_state"], st["degraded"]))
        mdctl.tunables(MD)
        if not ok:
            return
        # Legs that were not fresh enough to start the array are put back the
        # same way as any other returning leg: the kernel decides whether the
        # bitmap covers them.
        st, legs = view()
        for l in legs:
            if l["slot"] in sbs and l["bad"]:
                put_back(l, st)
    finally:
        try:
            os.unlink(PAUSE_FILE)
        except OSError:
            pass


def one_pass():
    fixed = mdctl.tunables(MD)
    if fixed:
        log("md %s: re-applied %s" % (MD, ", ".join(fixed)))
    st, legs = view()
    bad = [l for l in legs if l["bad"]]
    if not bad:
        return
    log("md %s: %s degraded=%d  %s" % (MD, st.get("array_state", "ABSENT"), st["degraded"],
                                       " ".join("%s=%s" % (l["node"], l["why"]) for l in legs)))
    healthy = [l for l in bad if probe.qualify(l, log)]
    if not healthy:
        log("  nothing recovered this pass")
        return

    st, legs = view()                          # md may have moved while we probed
    byslot = {l["slot"]: l for l in legs}
    healthy = [byslot[l["slot"]] for l in healthy if byslot[l["slot"]]["bad"]]
    if not healthy:
        log("  array recovered on its own while we were checking")
        return

    if st["present"] and not st["broken"]:
        for l in healthy:
            put_back(l, st)
    else:
        reassemble(healthy)


def main():
    log("healer up: %s, legs %s" % (MD, ", ".join(l["node"] for l in LEGS)))
    while True:
        try:
            one_pass()
        except Exception as e:                 # a demo must not die on a race
            log("pass failed: %r" % e)
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
