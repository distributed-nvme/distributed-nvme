#!/usr/bin/python3
"""
report.py -- render results/testA.json + results/testB.json into the tables
that go into nvme_io_error_test.md.  Emits markdown on stdout.
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RES = os.path.join(HERE, "results")
CASES = "abcdefgh"


def fmt(v, suffix="s"):
    return "--" if v is None else f"{v:g}{suffix}"


def cell(v):
    """
    One test-B matrix cell.  Four outcomes, and the difference between the last
    two matters: a probe that stalls and then resumes has failed over, while one
    that stalls and never resumes is a path-less device requeueing bios forever.

        <number>  seconds from fault injection to the first EIO
        hang      no error, and I/O never resumed inside the window
        cycle     no error, but I/O alternates stall / brief service
        stall/ok  I/O paused (>3s) then resumed for good -- clean failover
        ok        no error and no stall longer than 3s
    """
    if v is None:
        return "n/a"
    if v.get("nodev"):
        return "no dev"
    e = v.get("t_first_err")
    if e is not None:
        return f"{e:g}"
    ev = v.get("events") or []
    if v.get("t_first_stuck") is None:
        return "ok"
    if ev and ev[-1]["ev"] == "ok":
        return "stall/ok"
    # served some I/O after the first stall but ended stalled again
    first_stuck = next(i for i, x in enumerate(ev) if x["ev"] == "stuck")
    if any(x["ev"] == "ok" for x in ev[first_stuck:]):
        return "cycle"
    return "hang"


def table_a():
    p = os.path.join(RES, "testA.json")
    if not os.path.exists(p):
        return "_testA.json missing_\n"
    d = json.load(open(p))
    out = []
    pr = d["params"]
    out.append(f"`ctrl_loss_tmo={pr['ctrl_loss_tmo']} reconnect_delay={pr['reconnect_delay']} "
               f"kato={pr['kato']}`, observed {pr['obs']}s after injection.\n")
    out.append("| case | 1st IO error | errno | 1st stall | ok IOs | failed IOs |")
    out.append("|---|---|---|---|---|---|")
    for c in CASES:
        r = d["cases"][c]
        pb = r.get("probe", {})
        fe = pb.get("first_err") or {}
        out.append(f"| {c} | {fmt(r['t_first_err'])} | {fe.get('name','--')} | "
                   f"{fmt(r['t_first_stuck'])} | {pb.get('ok','?')} | {pb.get('err','?')} |")
    return "\n".join(out) + "\n"


def table_b(m):
    p = os.path.join(RES, "testB.json")
    if not os.path.exists(p):
        return "_testB.json missing_\n"
    d = json.load(open(p))
    mm = d["matrices"].get(m)
    if not mm:
        return f"_matrix {m} not run_\n"
    out = ["| cn0 \\ cn1 | " + " | ".join(CASES) + " |",
           "|---" * (len(CASES) + 1) + "|"]
    for c0 in CASES:
        row = [f"| **{c0}** "]
        for c1 in CASES:
            row.append(f"| {cell(mm.get(c0 + c1))} ")
        out.append("".join(row) + "|")
    return "\n".join(out) + "\n"


if __name__ == "__main__":
    what = sys.argv[1] if len(sys.argv) > 1 else "all"
    if what in ("all", "A"):
        print("### Test A\n")
        print(table_a())
    if what in ("all", "B"):
        p = os.path.join(RES, "testB.json")
        if os.path.exists(p):
            d = json.load(open(p))
            pr = d["params"]
            print(f"\n### Test B  (`ctrl_loss_tmo={pr['ctrl_loss_tmo']} "
                  f"reconnect_delay={pr['reconnect_delay']} kato={pr['kato']}`, "
                  f"observed {pr['obs']}s)\n")
            for m in d["matrices"]:
                print(f"\n#### matrix {m}\n")
                print(table_b(m))
