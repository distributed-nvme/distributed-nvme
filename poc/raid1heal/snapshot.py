#!/usr/bin/env python3
"""One line of JSON describing the array and both legs. Used by scenarios.py."""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mdctl
import mdsb
from rig import MD, LEGS

st = mdctl.read_md(MD)
out = dict(present=st["present"], array_state=st.get("array_state"),
           degraded=st["degraded"], in_sync=st["in_sync"],
           sync_action=st.get("sync_action"), sync_completed=st.get("sync_completed"))
legs = []
for l in LEGS:
    dn = mdctl.devname(l["path"])
    r = st["rdevs"].get(dn)
    ctrl = None
    for c in sorted(os.listdir("/sys/class/nvme")):
        if mdctl.rd("/sys/class/nvme/%s/subsysnqn" % c) == l["nqn"]:
            ctrl = mdctl.rd("/sys/class/nvme/%s/state" % c)
    sb = mdsb.examine(l["path"])
    bm = mdsb.bitmap_sb(l["path"])
    legs.append(dict(node=l["node"], slot=l["slot"], dev=dn, ctrl=ctrl,
                     md_state=",".join(sorted(r["state"])) if r else None,
                     written=mdctl.sectors_written(l["path"]),
                     events=sb["events"] if sb else None,
                     roles=[mdsb.role_str(x) for x in sb["roles"][:len(LEGS)]] if sb else None,
                     events_cleared=bm["events_cleared"] if bm else None))
out["legs"] = legs
out["mdstat"] = open("/proc/mdstat").read().split("\n")[1:3]
print(json.dumps(out))
