#!/usr/bin/python3
"""report.py <io.json> <markers.json> -- summarise the failover IO run."""
import json
import sys
import errno as E

def pct(v, p):
    if not v:
        return float('nan')
    v = sorted(v)
    k = min(len(v) - 1, int(round((p / 100.0) * (len(v) - 1))))
    return v[k]

def ms(x):
    return 'n/a' if x != x else '%8.2f' % (x * 1000)

def main():
    io = json.load(open(sys.argv[1]))
    mk = json.load(open(sys.argv[2]))
    recs = io['recs']
    fo_s, fo_e = mk['fo_start'], mk['fo_end']

    phases = [('before failover', lambda t: t < fo_s),
              ('DURING failover', lambda t: fo_s <= t < fo_e),
              ('after  failover', lambda t: t >= fo_e)]

    print('=' * 78)
    print('device %s   %d threads   %d B O_DIRECT write+read   run %.1fs'
          % (io['dev'], io['threads'], io['bs'], io['end_epoch'] - io['start_epoch']))
    print('failover window: %.3f .. %.3f  (%.3f s wall)' % (fo_s, fo_e, fo_e - fo_s))
    print('=' * 78)
    print()
    hdr = ('%-16s %7s %7s %7s | %9s %9s %9s %9s %9s %9s'
           % ('phase', 'IOs', 'ok', 'failed',
              'min ms', 'p50 ms', 'avg ms', 'p95 ms', 'p99 ms', 'max ms'))
    print(hdr)
    print('-' * len(hdr))
    for name, sel in phases:
        sub = [r for r in recs if sel(r[0])]
        ok = [r for r in sub if r[3] == 0]
        bad = [r for r in sub if r[3] != 0]
        lat = [r[1] for r in ok]
        avg = sum(lat) / len(lat) if lat else float('nan')
        print('%-16s %7d %7d %7d | %9s %9s %9s %9s %9s %9s'
              % (name, len(sub), len(ok), len(bad),
                 ms(min(lat) if lat else float('nan')), ms(pct(lat, 50)), ms(avg),
                 ms(pct(lat, 95)), ms(pct(lat, 99)),
                 ms(max(lat) if lat else float('nan'))))
        if bad:
            cnt = {}
            for r in bad:
                cnt[r[3]] = cnt.get(r[3], 0) + 1
            det = ', '.join('%s(%d)=%d' % (E.errorcode.get(k, '?'), k, v)
                            for k, v in sorted(cnt.items()))
            blat = [r[1] for r in bad]
            print('%-16s   failures: %s ; fail latency avg %s max %s'
                  % ('', det, ms(sum(blat) / len(blat)), ms(max(blat))))
    print()

    # ---- outage analysis: longest gap between successful IO *completions* ---
    comp = sorted(r[0] + r[1] for r in recs if r[3] == 0)
    gap, gs, ge = 0.0, None, None
    for a, b in zip(comp, comp[1:]):
        if b - a > gap:
            gap, gs, ge = b - a, a, b
    print('longest gap with no successful IO completion: %.3f s' % gap)
    if gs:
        print('  from %.3f (%+.3f s vs failover start) to %.3f (%+.3f s)'
              % (gs, gs - fo_s, ge, ge - fo_s))
    slow = [r for r in recs if r[3] == 0 and r[1] > 1.0]
    print('successful IOs that took > 1 s: %d%s' % (
        len(slow), ('  (max %.3f s, started %+.3f s vs failover start)'
                    % (max(r[1] for r in slow),
                       min(r[0] for r in slow) - fo_s)) if slow else ''))
    if io.get('stuck_at_exit'):
        print('threads still blocked in a syscall at exit: %d %s'
              % (len(io['stuck_at_exit']), io['stuck_at_exit']))
    print()

    # ---- 250 ms timeline around the failover -------------------------------
    lo, hi = fo_s - 2.0, fo_e + 4.0
    print('timeline (250 ms buckets, completion time), "." = idle bucket')
    print('%-10s %6s %6s %9s   %s' % ('t(rel)', 'ok', 'err', 'p95 ms', 'bar'))
    b = {}
    for r in recs:
        t = r[0] + r[1]
        if not (lo <= t <= hi):
            continue
        k = int((t - fo_s) * 4)
        b.setdefault(k, []).append(r)
    for k in range(int((lo - fo_s) * 4), int((hi - fo_s) * 4) + 1):
        rs = b.get(k, [])
        ok = [r for r in rs if r[3] == 0]
        bad = [r for r in rs if r[3] != 0]
        p95 = pct([r[1] for r in ok], 95)
        bar = '#' * len(ok) + '!' * len(bad)
        print('%+9.2fs %6d %6d %9s   %s'
              % (k / 4.0, len(ok), len(bad), ms(p95), bar if rs else '.'))
    print()
    print('event log:')
    for e in mk.get('events', []):
        print('  %+9.3fs  %s' % (e[0] - fo_s, e[1]))

main()
