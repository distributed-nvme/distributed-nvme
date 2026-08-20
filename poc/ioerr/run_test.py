#!/usr/bin/python3
"""
run_test.py -- orchestrator for the NVMe-oF host IO-error test matrix.

Runs from the workstation and drives host0 / cn0 / cn1 over ssh.

Both tests exploit the same trick to stay inside a sane wall-clock budget:
every test case gets its own *lane* -- its own dm slice, its own subsystem NQN,
its own nvmet port on its own TCP port -- so N cases can be faulted and observed
concurrently without interfering.  Faults that are configfs writes are still
applied strictly sequentially inside one ssh call (nvmet serialises them on a
global lock anyway), and each one is bracketed with an epoch so the report can
subtract it from the probe's first-error epoch.

  test A: 8 lanes on cn0, one per scenario a..h, stock fabrics timeouts.
  test B: 16 lanes spanning cn0+cn1, 64 (cn0 fault x cn1 fault) combos per ANA
          matrix, 3 matrices -- run as 12 rounds of 16 lanes.

Usage:
    run_test.py A            [--obs 700]
    run_test.py B            [--obs 60] [--lanes 16] [--matrix X,Y,Z]
    run_test.py smokeB       quick 2-lane sanity round
    run_test.py cleanup
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time

HOST0 = "192.168.122.193"
CN = {"cn0": "192.168.122.125", "cn1": "192.168.122.229"}
USER = "yupeng"
HOSTNQN = "nqn.2014-08.org.nvmexpress:uuid:1a788e0a-02bd-4ae9-9a4b-e4aad8b04c26"

CASES = "abcdefgh"
CASE_TEXT = {
    "a": "iptables DROP all host->target packets",
    "b": "iptables REJECT --reject-with tcp-reset",
    "c": "subsystem off the port, port not listening",
    "d": "subsystem off the port, port still listening (anchor subsys)",
    "e": "host removed from allowed_hosts",
    "f": "namespace disabled",
    "g": "backend returns EIO (dm-error)",
    "h": "backend swallows IO (dmsetup suspend)",
}

RESDIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "results")
SERIAL = "DNVIOTEST"
MODEL = "dnvio"

# Stock fabrics timeouts (what a plain `nvme connect` gets).  ctrl_loss_tmo is
# not a wall clock: the driver turns it into a retry budget,
# max_reconnects = ctrl_loss_tmo / reconnect_delay, and each failed retry also
# costs nvme-tcp's own ~3.4s socket connect timeout.  So 600/10 = 60 retries
# ~= 60 * 13.4s ~= 800s of wall clock, not 600s.
A_CLT, A_RD, A_KATO = 600, 10, 5
# Compressed budget for the 192-case matrix: 10/2 = 5 retries ~= 27s.
B_CLT, B_RD, B_KATO = 10, 2, 5


# ------------------------------------------------------------------ ssh -----
def ssh(ip, cmd, stdin=None, timeout=300):
    p = subprocess.run(
        ["ssh", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=no",
         f"{USER}@{ip}", cmd],
        input=stdin, capture_output=True, text=True, timeout=timeout)
    return p.stdout + p.stderr


def ssh_bg(ip, cmd, stdin=None):
    return subprocess.Popen(
        ["ssh", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=no",
         f"{USER}@{ip}", cmd],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT, text=True), stdin


def run_bg(pairs):
    """pairs: list of (ip, cmd, stdin). Runs all concurrently, returns outputs."""
    procs = []
    for ip, cmd, sin in pairs:
        p = subprocess.Popen(
            ["ssh", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=no",
             f"{USER}@{ip}", cmd],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, text=True)
        p.stdin.write(sin or "")
        p.stdin.close()
        procs.append(p)
    return [p.stdout.read() or "" for p in procs]


CNB = "sudo bash /var/tmp/cnctl.sh batch"
HB = "sudo bash /var/tmp/hostctl.sh"


def parse_batch(out):
    """cnctl batch output -> {(cmd, first_arg): epoch}"""
    ep = {}
    for m in re.finditer(r"T=(\d+\.\d+) CMD=(\S+) ARGS=(\S+)", out):
        ep[(m.group(2), m.group(3))] = float(m.group(1))
    return ep


def push():
    here = os.path.dirname(os.path.abspath(__file__))
    for ip in [HOST0] + list(CN.values()):
        subprocess.run(["scp", "-q",
                        f"{here}/cnctl.sh", f"{here}/hostctl.sh",
                        f"{here}/io_probe.py", f"{USER}@{ip}:/var/tmp/"],
                       check=True)


def log(*a):
    print(time.strftime("[%H:%M:%S]"), *a, flush=True)


# --------------------------------------------------------------- lanes ------
def nqn_of(tag, lane):
    return f"nqn.2026-08.dnvio:{tag}-{lane}"


def uuid_of(tag, lane):
    t = 1 if tag == "a" else 2
    return f"cafe000{t}-0000-4000-8000-{lane:012d}"


def collect_probes(nqns, tag):
    """Read every lane's probe json back in a single ssh call."""
    files = " ".join(f"/var/tmp/probe-{tag}-{i}.json" for i in range(len(nqns)))
    out = ssh(HOST0, "sudo bash -c 'for f in %s; do echo \"===$f\"; "
                     "cat $f 2>/dev/null || echo MISSING; done'" % files)
    res = {}
    for blk in out.split("===/var/tmp/probe-")[1:]:
        name, _, body = blk.partition("\n")
        lane = int(name.replace(".json", "").rsplit("-", 1)[1])
        try:
            res[lane] = json.loads(body)
        except Exception:
            res[lane] = {"parse_error": body[:400]}
    return res


def dmesg_tail(n=120):
    return ssh(HOST0, f"sudo dmesg -T | tail -{n}")


# --------------------------------------------------------------- test A -----
def test_a(obs):
    tag = "a"
    lanes = list(range(8))
    log("test A: init cn0")
    print(ssh(CN["cn0"], f"sudo bash /var/tmp/cnctl.sh init '{HOSTNQN}'").strip())

    create = "".join(
        f"lane-create {i} {4420+i} {nqn_of(tag,i)} {uuid_of(tag,i)} "
        f"{CN['cn0']} 1 100 optimized {SERIAL} {MODEL}\n" for i in lanes)
    log("test A: creating 8 lanes")
    ssh(CN["cn0"], CNB, stdin=create)

    log("test A: connecting")
    ssh(HOST0, f"{HB} disconnect-batch",
        stdin="".join(nqn_of(tag, i) + "\n" for i in lanes))
    ssh(HOST0, f"{HB} connect-batch",
        stdin="".join(f"{nqn_of(tag,i)} {A_CLT} {A_RD} {A_KATO} "
                      f"{CN['cn0']}:{4420+i}\n" for i in lanes))
    time.sleep(3)
    pre = ssh(HOST0, f"{HB} state-batch",
              stdin="".join(nqn_of(tag, i) + "\n" for i in lanes))
    print(pre)

    dur = obs + 20
    log(f"test A: starting probes ({dur}s)")
    ssh(HOST0, f"{HB} probe-batch",
        stdin="".join(f"{nqn_of(tag,i)} {dur} /var/tmp/probe-{tag}-{i}.json\n"
                      for i in lanes))
    time.sleep(10)

    log("test A: injecting faults a..h")
    fout = ssh(CN["cn0"], CNB,
               stdin="".join(f"lane-fault {i} {nqn_of(tag,i)} {CASES[i]} apply\n"
                             for i in lanes))
    print(fout)
    fep = parse_batch(fout)

    log(f"test A: observing {obs}s")
    samples = []
    t_end = time.time() + obs + 12
    while time.time() < t_end:
        time.sleep(60)
        s = ssh(HOST0, f"{HB} state-batch",
                stdin="".join(nqn_of(tag, i) + "\n" for i in lanes))
        samples.append({"dt": round(time.time() - min(fep.values()), 1), "state": s})
        log(f"  ... {int(t_end - time.time())}s left")

    probes = collect_probes([nqn_of(tag, i) for i in lanes], tag)
    post = ssh(HOST0, f"{HB} state-batch",
               stdin="".join(nqn_of(tag, i) + "\n" for i in lanes))
    cnstat = ssh(CN["cn0"], CNB,
                 stdin="".join(f"lane-status {i} {nqn_of(tag,i)}\n" for i in lanes))
    dm = dmesg_tail(200)

    res = {"params": {"ctrl_loss_tmo": A_CLT, "reconnect_delay": A_RD,
                      "kato": A_KATO, "obs": obs},
           "pre_state": pre, "post_state": post, "cn_status": cnstat,
           "samples": samples, "dmesg": dm, "cases": {}}
    for i in lanes:
        c = CASES[i]
        fe = fep.get(("lane-fault", str(i)))
        p = probes.get(i, {})
        res["cases"][c] = {"lane": i, "fault_epoch": fe, "probe": p,
                           "t_first_err": (round(p["first_err"]["epoch"] - fe, 2)
                                           if fe and p.get("first_err") else None),
                           "t_first_stuck": (round(p["first_stuck"]["epoch"] - fe, 2)
                                             if fe and p.get("first_stuck") else None)}
    os.makedirs(RESDIR, exist_ok=True)
    with open(f"{RESDIR}/testA.json", "w") as f:
        json.dump(res, f, indent=1)
    log("test A done ->", f"{RESDIR}/testA.json")
    for c in CASES:
        r = res["cases"][c]
        log(f"  {c}: first_err={r['t_first_err']} stuck={r['t_first_stuck']} "
            f"ok={r['probe'].get('ok')} err={r['probe'].get('err')}")

    log("test A: restoring")
    ssh(CN["cn0"], CNB,
        stdin="".join(f"lane-fault {i} {nqn_of(tag,i)} {CASES[i]} clear\n"
                      for i in lanes))
    return res


# --------------------------------------------------------------- test B -----
MATRIX = {"X": ("optimized", "optimized"),
          "Y": ("inaccessible", "optimized"),
          "Z": ("inaccessible", "inaccessible")}


def b_setup(lanes, tag):
    log("test B: init cn0/cn1")
    for n, ip in CN.items():
        print(ssh(ip, f"sudo bash /var/tmp/cnctl.sh init '{HOSTNQN}'").strip())
    pairs = []
    for n, ip in CN.items():
        cmin, cmax = (1, 100) if n == "cn0" else (101, 200)
        pairs.append((ip, CNB, "".join(
            f"lane-create {i} {4500+i} {nqn_of(tag,i)} {uuid_of(tag,i)} "
            f"{ip} {cmin} {cmax} optimized {SERIAL} {MODEL}\n" for i in lanes)))
    run_bg(pairs)


def b_round(lanes, tag, matrix, combos, obs, prev_faults):
    """One round: `combos[k]` = (cn0_case, cn1_case) for lane k."""
    nqns = [nqn_of(tag, i) for i in lanes]
    ana0, ana1 = MATRIX[matrix]

    # --- restore whatever the previous round broke, then re-assert the lanes --
    pairs = []
    for n, ip in CN.items():
        cmin, cmax = (1, 100) if n == "cn0" else (101, 200)
        s = ""
        for k, i in enumerate(lanes):
            if prev_faults:
                s += f"lane-fault {i} {nqn_of(tag,i)} {prev_faults[k][0 if n=='cn0' else 1]} clear\n"
            s += f"lane-ana {i} optimized\n"
            s += (f"lane-create {i} {4500+i} {nqn_of(tag,i)} {uuid_of(tag,i)} "
                  f"{ip} {cmin} {cmax} optimized {SERIAL} {MODEL}\n")
        pairs.append((ip, CNB, s))
    run_bg(pairs)

    # --- fresh controllers on both paths ------------------------------------
    ssh(HOST0, f"{HB} disconnect-batch", stdin="".join(q + "\n" for q in nqns))
    time.sleep(1)
    ssh(HOST0, f"{HB} connect-batch",
        stdin="".join(f"{nqn_of(tag,i)} {B_CLT} {B_RD} {B_KATO} "
                      f"{CN['cn0']}:{4500+i} {CN['cn1']}:{4500+i}\n"
                      for i in lanes))
    time.sleep(4)
    pre = ssh(HOST0, f"{HB} state-batch", stdin="".join(q + "\n" for q in nqns))

    # --- probes: 5s healthy, ANA flip, 10s, faults, obs ---------------------
    dur = 20 + obs
    ssh(HOST0, f"{HB} probe-batch",
        stdin="".join(f"{nqn_of(tag,i)} {dur} /var/tmp/probe-{tag}-{i}.json\n"
                      for i in lanes))
    time.sleep(5)

    ana_out = run_bg([
        (CN["cn0"], CNB, "".join(f"lane-ana {i} {ana0}\n" for i in lanes)),
        (CN["cn1"], CNB, "".join(f"lane-ana {i} {ana1}\n" for i in lanes))])
    ana_ep = [parse_batch(o) for o in ana_out]
    time.sleep(10)

    fout = run_bg([
        (CN["cn0"], CNB, "".join(
            f"lane-fault {i} {nqn_of(tag,i)} {combos[k][0]} apply\n"
            for k, i in enumerate(lanes))),
        (CN["cn1"], CNB, "".join(
            f"lane-fault {i} {nqn_of(tag,i)} {combos[k][1]} apply\n"
            for k, i in enumerate(lanes)))])
    fep = [parse_batch(o) for o in fout]

    time.sleep(obs + 8)
    probes = collect_probes(nqns, tag)
    post = ssh(HOST0, f"{HB} state-batch", stdin="".join(q + "\n" for q in nqns))

    out = {}
    for k, i in enumerate(lanes):
        e0 = fep[0].get(("lane-fault", str(i)))
        e1 = fep[1].get(("lane-fault", str(i)))
        a0 = ana_ep[0].get(("lane-ana", str(i)))
        base = max(x for x in (e0, e1) if x) if (e0 or e1) else None
        p = probes.get(i, {})
        fe = p.get("first_err")
        fs = p.get("first_stuck")
        out[combos[k]] = {
            "lane": i, "matrix": matrix,
            "fault_epoch_cn0": e0, "fault_epoch_cn1": e1, "ana_epoch": a0,
            "t_first_err": round(fe["epoch"] - base, 2) if (fe and base) else None,
            "t_first_err_vs_ana": round(fe["epoch"] - a0, 2) if (fe and a0) else None,
            "t_first_stuck": round(fs["epoch"] - base, 2) if (fs and base) else None,
            "t_first_stuck_vs_ana": round(fs["epoch"] - a0, 2) if (fs and a0) else None,
            "errno": fe.get("name") if fe else None,
            "ok": p.get("ok"), "err": p.get("err"),
            "nodev": p.get("nodev", False),
            "events": p.get("events"),
        }
    return out, pre, post


def test_b(obs, nlanes, matrices, maxrounds=None):
    tag = "b"
    lanes = list(range(nlanes))
    b_setup(lanes, tag)
    all_combos = [(c0, c1) for c0 in CASES for c1 in CASES]
    res = {"params": {"ctrl_loss_tmo": B_CLT, "reconnect_delay": B_RD,
                      "kato": B_KATO, "obs": obs, "lanes": nlanes},
           "matrices": {}}
    os.makedirs(RESDIR, exist_ok=True)
    for m in matrices:
        res["matrices"][m] = {}
        prev = None
        rounds = [all_combos[x:x + nlanes] for x in range(0, len(all_combos), nlanes)]
        if maxrounds:
            rounds = rounds[:maxrounds]
        for ri, combos in enumerate(rounds):
            lset = lanes[:len(combos)]
            log(f"test B matrix {m} round {ri+1}/{len(rounds)}: {combos}")
            out, pre, post = b_round(lset, tag, m, combos, obs, prev)
            prev = combos
            for k, v in out.items():
                res["matrices"][m]["%s%s" % k] = v
            res["matrices"][m].setdefault("_states", []).append(
                {"round": ri, "pre": pre, "post": post})
            with open(f"{RESDIR}/testB.json", "w") as f:
                json.dump(res, f, indent=1)
            for k, v in sorted(out.items()):
                log(f"   {k[0]}{k[1]}: err@{v['t_first_err']} ({v['errno']}) "
                    f"stuck@{v['t_first_stuck']} ok={v['ok']} err={v['err']}")
        # leave the last round's faults cleared
        run_bg([(ip, CNB, "".join(
            f"lane-fault {i} {nqn_of(tag,i)} {prev[k][0 if n=='cn0' else 1]} clear\n"
            f"lane-ana {i} optimized\n" for k, i in enumerate(lanes[:len(prev)])))
            for n, ip in CN.items()])
    res["dmesg"] = dmesg_tail(200)
    with open(f"{RESDIR}/testB.json", "w") as f:
        json.dump(res, f, indent=1)
    log("test B done ->", f"{RESDIR}/testB.json")
    return res


def cleanup():
    ssh(HOST0, f"{HB} cleanup")
    ssh(HOST0, "sudo pkill -9 -f io_probe.py; true")
    for ip in CN.values():
        print(ssh(ip, "sudo bash /var/tmp/cnctl.sh cleanup").strip())


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("what")
    ap.add_argument("--obs", type=int, default=None)
    ap.add_argument("--lanes", type=int, default=16)
    ap.add_argument("--matrix", default="X,Y,Z")
    ap.add_argument("--rounds", type=int, default=None)
    a = ap.parse_args()
    push()
    if a.what == "A":
        test_a(a.obs or 900)
    elif a.what == "B":
        test_b(a.obs or 60, a.lanes, a.matrix.split(","), a.rounds)
    elif a.what == "smokeB":
        test_b(a.obs or 45, 4, ["X"], 1)
    elif a.what == "cleanup":
        cleanup()
    else:
        sys.exit("unknown: " + a.what)
