#!/usr/bin/env python3
"""Drive the fault matrix from the orchestrator; everything else over ssh.

    python3 scenarios.py single      # 5.1  one leg, one fault, ten times
    python3 scenarios.py double      # 5.2 / 5.3  both legs down, both orders
    python3 scenarios.py gate        # 5.4  the events_cleared gate really fires

Each run appends to results_<what>.json.
"""
import json
import subprocess
import sys
import time

HOST = {"cn0": "192.168.122.125", "dn0": "192.168.122.48", "dn1": "192.168.122.70"}
SSH = ["ssh", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=10",
       "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"]


def log(m):
    print("%s %s" % (time.strftime("%H:%M:%S"), m), flush=True)


def sh(node, cmd, timeout=120):
    try:
        p = subprocess.run(SSH + ["yupeng@" + HOST[node], cmd],
                           capture_output=True, text=True, timeout=timeout)
        return (p.stdout + p.stderr).strip()
    except subprocess.TimeoutExpired:
        return "SSH-TIMEOUT"


def fault(node, *args):
    out = sh(node, "bash /tmp/fault.sh " + " ".join(str(a) for a in args))
    log("  [%s] fault %s -> %s" % (node, " ".join(map(str, args)), out.replace("\n", " | ")))


def start_watch(node):
    """ssh round-trips are ~0.5s, the same order as the fastest detection times,
    so t_detect is measured on cn0 itself."""
    dev = "nvme0n1" if node == "dn0" else "nvme1n1"
    return subprocess.Popen(SSH + ["yupeng@" + HOST["cn0"],
                                   "sudo python3 /opt/dnv/watch.py %s 150" % dev],
                            stdout=subprocess.PIPE, text=True)


def read_watch(p):
    try:
        out, _ = p.communicate(timeout=200)
    except subprocess.TimeoutExpired:
        p.kill()
        return None
    for line in out.split("\n"):
        if line.startswith("DETECT "):
            v = line.split()[1]
            return None if v == "none" else float(v)
    return None


def snap():
    out = sh("cn0", "sudo python3 /opt/dnv/snapshot.py")
    try:
        return json.loads(out.strip().splitlines()[-1])
    except Exception:
        return dict(present=False, error=out[:200])


def leg(st, node):
    for l in st.get("legs", []):
        if l["node"] == node:
            return l
    return {}


def wait(pred, limit, label):
    """Poll until pred(snapshot) or `limit` seconds. Returns (elapsed|None, last)."""
    t0 = time.monotonic()
    last = None
    while time.monotonic() - t0 < limit:
        last = snap()
        if pred(last):
            dt = time.monotonic() - t0
            log("  %s after %.1fs" % (label, dt))
            return dt, last
        time.sleep(2)
    log("  %s NOT reached within %ds" % (label, limit))
    return None, last


def healer_log_since(mark):
    return sh("cn0", "sudo awk 'f{print} /%s/{f=1}' /var/log/dnv-healer.log | tail -60" % mark)


def mark():
    m = "MARK-%d" % int(time.time())
    sh("cn0", "echo %s | sudo tee -a /var/log/dnv-healer.log >/dev/null" % m)
    return m


def kicked(node):
    return lambda s: (leg(s, node).get("md_state") or "").find("faulty") >= 0 \
        or leg(s, node).get("md_state") is None


def healthy(s):
    return s.get("present") and s.get("degraded") == 0 and s.get("in_sync") == 2


def settle(limit=240):
    log("  settling...")
    wait(healthy, limit, "array healthy")
    time.sleep(5)


# --------------------------------------------------------------------------
FAULTS = [
    ("4.1  iptables DROP",              "dn1", ["net-drop"],            ["net-clear"]),
    ("4.2  iptables tcp-reset",         "dn1", ["net-reset"],           ["net-clear"]),
    ("4.3  dm-linear suspended",        "dn1", ["suspend"],             ["resume"]),
    ("4.4  dm-delay 20s",               "dn1", ["delay", "20000"],      ["heal"]),
    ("4.5a dm-flakey error_writes",     "dn1", ["flakey", "error_writes"],  ["heal"]),
    ("4.5b dm-flakey error_reads",      "dn1", ["flakey", "error_reads"],   ["heal"]),
    ("4.5c dm-flakey drop_writes",      "dn1", ["flakey", "drop_writes"],   ["heal"]),
    ("4.5d dm-flakey corrupt_bio_byte", "dn1", ["flakey", "corrupt_read"],  ["heal"]),
    ("4.5e dm-flakey both directions",  "dn1", ["flakey", "plain"],     ["heal"]),
    ("4.x  dm error table",             "dn0", ["error"],               ["heal"]),
]


def scenario_single():
    out = []
    for name, node, inj, fix in FAULTS:
        log("=== %s on %s" % (name, node))
        settle()
        m = mark()
        before = leg(snap(), node).get("written")
        w = start_watch(node)
        time.sleep(1.0)                          # let the watcher get going
        fault(node, *inj)
        t_detect = read_watch(w)
        log("  md kicked %s after %s" % (node, "no" if t_detect is None else "%.2fs" % t_detect))
        time.sleep(30 if t_detect is None else 10)
        fault(node, *fix)
        t_recover, st = wait(healthy, 300, "array back to [2/2]")
        after = leg(st, node).get("written")
        rec = dict(name=name, node=node, fault=inj,
                   detected=t_detect is not None,
                   t_detect=None if t_detect is None else round(t_detect, 2),
                   t_recover=None if t_recover is None else round(t_recover, 1),
                   sectors_copied=None if (before is None or after is None) else after - before,
                   healer=healer_log_since(m))
        out.append(rec)
        log("  -> detected=%s t_detect=%s t_recover=%s sectors_copied=%s"
            % (rec["detected"], rec["t_detect"], rec["t_recover"], rec["sectors_copied"]))
    json.dump(out, open("results_single.json", "w"), indent=1)
    log("wrote results_single.json")


def scenario_double(order):
    """order='reverse': A,B fail -> B,A return (B was last standing, it may start).
       order='same':    A,B fail -> A,B return (A dropped out first, must WAIT)."""
    log("=== both legs down, return order: %s" % order)
    settle()
    m = mark()
    b0 = {l["node"]: l["written"] for l in snap()["legs"]}
    fault("dn0", "error")
    wait(kicked("dn0"), 120, "md kicked dn0")
    time.sleep(15)                      # let writes accumulate on dn1 alone
    fault("dn1", "error")
    _, st = wait(lambda s: s.get("in_sync", 2) == 0, 180, "both legs kicked")
    log("  state: %s" % json.dumps({k: st.get(k) for k in ("array_state", "degraded", "in_sync")}))

    first, second = ("dn1", "dn0") if order == "reverse" else ("dn0", "dn1")
    fault(first, "heal")
    t0 = time.monotonic()
    if order == "same":
        # dn0 dropped out first: its role map still says dn1 is in_sync, so the
        # healer must refuse to start the array from it.
        time.sleep(200)
        st = snap()
        refused = not st.get("present") or st.get("in_sync", 0) == 0
        log("  refused to start from the stale leg: %s" % refused)
    else:
        refused = None
        wait(lambda s: s.get("present") and s.get("in_sync") >= 1, 240, "array up degraded")
    fault(second, "heal")
    t_recover, st = wait(healthy, 400, "array back to [2/2]")
    b1 = {l["node"]: l["written"] for l in st["legs"]}
    rec = dict(order=order, refused_correctly=refused,
               t_recover=None if t_recover is None else round(time.monotonic() - t0, 1),
               sectors_copied={k: (b1[k] - b0[k]) if b1.get(k) and b0.get(k) else None
                               for k in b0},
               healer=healer_log_since(m))
    log("  -> %s" % json.dumps({k: rec[k] for k in ("order", "refused_correctly",
                                                    "t_recover", "sectors_copied")}))
    return rec


def scenario_gate():
    """Age a leg's superblock below the peer's events_cleared and confirm the
    kernel refuses the bitmap-scoped path instead of silently doing it wrong."""
    log("=== events_cleared gate")
    settle()
    m = mark()
    before = leg(snap(), "dn1")["written"]
    fault("dn1", "error")
    wait(kicked("dn1"), 120, "md kicked dn1")
    time.sleep(10)
    fault("dn1", "heal")
    # Detach the rdev so the healer cannot take the re-add path, then rewrite
    # dn1's events to 1 -- far below any events_cleared the peer can hold.
    sh("cn0", "sudo python3 - <<'PY'\n"
              "import sys; sys.path.insert(0,'/opt/dnv')\n"
              "import mdctl, mdsb\n"
              "from rig import MD, LEGS\n"
              "st = mdctl.read_md(MD)\n"
              "dn = mdctl.devname(LEGS[1]['path'])\n"
              "r = st['rdevs'].get(dn)\n"
              "print('detach', mdctl.detach(r) if r else 'no rdev')\n"
              "mdsb.set_events(LEGS[1]['path'], 1)\n"
              "print('aged', mdsb.summary(mdsb.examine(LEGS[1]['path'])))\n"
              "PY")
    t_recover, st = wait(healthy, 400, "array back to [2/2]")
    after = leg(st, "dn1")["written"]
    hl = healer_log_since(m)
    rec = dict(t_recover=None if t_recover is None else round(t_recover, 1),
               sectors_copied=after - before if (after and before) else None,
               gate_fired="bitmap no longer covers" in hl or "EINVAL" in hl,
               healer=hl)
    log("  -> gate_fired=%s sectors_copied=%s" % (rec["gate_fired"], rec["sectors_copied"]))
    json.dump(rec, open("results_gate.json", "w"), indent=1)


if __name__ == "__main__":
    what = sys.argv[1] if len(sys.argv) > 1 else "single"
    if what == "single":
        scenario_single()
    elif what == "double":
        out = [scenario_double("reverse"), scenario_double("same")]
        json.dump(out, open("results_double.json", "w"), indent=1)
        log("wrote results_double.json")
    elif what == "gate":
        scenario_gate()
    else:
        sys.exit("unknown scenario %s" % what)
