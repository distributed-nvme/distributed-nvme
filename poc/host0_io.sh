#!/bin/bash
#
# host0_io.sh -- drive continuous verified O_DIRECT I/O against the aggregated
# volume on host0 and record every single I/O, so a failover can be judged by
# exactly what the host saw and when.
#
# The I/O engine is a Python script (dd is uutils coreutils 0.8.0 on these VMs
# and its iflag/oflag=direct are broken -- EINVAL on plain linear devices and
# silent zero-writes through raid0-over-thin).  Python opens the block device
# with O_DIRECT and does preadv/pwritev straight out of page-aligned anonymous
# mmap buffers, so every read really reaches the device instead of the page
# cache and no `blockdev --flushbufs` is needed.
#
# Each iteration issues three I/Os and writes one JSON line per I/O to the log:
#
#   write       1MiB of a per-iteration byte pattern at a rotating slot (0..63MiB)
#   read        the same 1MiB back and byte-compare it
#   anchor-read the 1MiB "anchor" block at 512MiB, written once before the loop
#               and never rewritten, byte-compared every iteration -- this is
#               what catches data lost or rolled back by the failover
#
# Every record carries: sequence number, op, offset, start time, stop time,
# latency in ms, ok/fail and the error.  `mark` drops a labelled marker record
# into the same log using host0's own clock, so the report can split the run
# into before / during / after the failover with no cross-host clock skew.
#
# Usage: ./host0_io.sh start|stop|status|mark <label>|report
#
set -uo pipefail

HOST0_IP="192.168.122.193"
SSH_USER="yupeng"
NQN_VOL="nqn.2026-07.org.dnv:sp:sp0:td0:exp0"

# 1MiB blocks at offsets 0..63MiB, cycling, plus a 1MiB anchor at 512MiB.  The
# 2G raid0 is thin-provisioned over 2 x 976M, so all of this stays well inside
# the pool and it never runs out.
IO_SIZE=1048576
SLOTS=64
ANCHOR_SLOT=512

LOG=/tmp/dnv-io.jsonl
ERRF=/tmp/dnv-io.err
PIDF=/tmp/dnv-io.pid
RUNNER=/tmp/dnv-io-runner.py

_ssh() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
             -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

# ===================== the I/O engine (runs on host0) ========================
emit_runner() {
    cat <<EOF
NQN_VOL     = '$NQN_VOL'
LOG         = '$LOG'
PIDF        = '$PIDF'
BLOCK       = $IO_SIZE
SLOTS       = $SLOTS
ANCHOR_SLOT = $ANCHOR_SLOT
EOF
    cat <<'EOF_PY'
import json, mmap, os, re, signal, stat, sys, time

NS_RE = re.compile(r'^nvme\d+n\d+$')          # excludes hidden nvmeXcYnZ paths


def find_dev(nqn):
    """Resolve an NQN to its block device through sysfs.

    The udev rule that keeps the test devices from being probed also suppresses
    /dev/disk/by-id, so symlinks are not an option.  The multipath (subsystem)
    node is preferred; a bare controller is the fallback for the non-ANA case.
    """
    for base in ('/sys/class/nvme-subsystem', '/sys/class/nvme'):
        try:
            entries = sorted(os.listdir(base))
        except OSError:
            continue
        for e in entries:
            p = os.path.join(base, e)
            try:
                with open(os.path.join(p, 'subsysnqn')) as fh:
                    if fh.read().strip() != nqn:
                        continue
            except OSError:
                continue
            for n in sorted(os.listdir(p)):
                if not NS_RE.match(n):
                    continue
                dev = '/dev/' + n
                try:
                    if stat.S_ISBLK(os.stat(dev).st_mode):
                        return dev
                except OSError:
                    pass
    return None


def iso(t):
    return time.strftime('%Y-%m-%dT%H:%M:%S', time.localtime(t)) + '.%03d' % ((t % 1) * 1000)


class Log:
    def __init__(self, path):
        self.f = open(path, 'a', buffering=1)
        self.seq = 0

    def io(self, op, offset, t0, t1, ok, err=''):
        self.seq += 1
        self.f.write(json.dumps({
            'seq':      self.seq,
            'op':       op,
            'offset':   offset,
            'bytes':    BLOCK,
            'start':    iso(t0),
            'stop':     iso(t1),
            'start_ts': round(t0, 6),
            'stop_ts':  round(t1, 6),
            'lat_ms':   round((t1 - t0) * 1000.0, 3),
            'ok':       bool(ok),
            'err':      err,
        }) + '\n')

    def event(self, kind, msg):
        t = time.time()
        self.f.write(json.dumps({
            'op': 'event', 'kind': kind, 'msg': msg,
            'start': iso(t), 'start_ts': round(t, 6),
        }) + '\n')


def timed(log, op, offset, fn):
    """Run one I/O, time it, log it.  fn returns '' on success, else an error."""
    t0 = time.time()
    try:
        err = fn()
    except OSError as e:
        t1 = time.time()
        log.io(op, offset, t0, t1, False, '%s (errno %d)' % (e.strerror, e.errno))
        return False
    except Exception as e:                      # noqa: BLE001 -- must never die
        t1 = time.time()
        log.io(op, offset, t0, t1, False, repr(e))
        return False
    t1 = time.time()
    log.io(op, offset, t0, t1, err == '', err)
    return err == ''


running = True


def on_signal(signum, frame):
    global running
    running = False


def main():
    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    log = Log(LOG)
    with open(PIDF, 'w') as f:
        f.write('%d\n' % os.getpid())

    dev = find_dev(NQN_VOL)
    if dev is None:
        log.event('fatal', 'no block device for %s' % NQN_VOL)
        return 1

    fd = os.open(dev, os.O_RDWR | os.O_DIRECT)
    log.event('start', 'device=%s block=%d slots=%d anchor_slot=%d pid=%d'
                       % (dev, BLOCK, SLOTS, ANCHOR_SLOT, os.getpid()))

    # Page-aligned buffers: O_DIRECT needs the user buffer, the file offset and
    # the length all aligned to the device's logical block size.  An anonymous
    # mmap is page-aligned, which is stricter than any of them.
    src = mmap.mmap(-1, BLOCK)
    dst = mmap.mmap(-1, BLOCK)
    anchor = mmap.mmap(-1, BLOCK)
    anchor[:] = b'A' * BLOCK
    anchor_off = ANCHOR_SLOT * BLOCK

    def _w(buf, off):
        n = os.pwritev(fd, [buf], off)
        return '' if n == BLOCK else 'short write %d/%d' % (n, BLOCK)

    def _r(off):
        n = os.preadv(fd, [dst], off)
        return '' if n == BLOCK else 'short read %d/%d' % (n, BLOCK)

    # The anchor is written once, before the loop, and re-verified for the rest
    # of the run.  If the failover lost or rolled back committed data, the
    # anchor comparison is what catches it.
    timed(log, 'anchor-write', anchor_off, lambda: _w(anchor, anchor_off))

    # Printable alphabet on purpose: a NUL or newline pattern is easy to
    # confuse with an empty buffer when something goes wrong.
    alphabet = b'0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ'
    i = 0
    while running:
        off = (i % SLOTS) * BLOCK
        src[:] = alphabet[i % len(alphabet):i % len(alphabet) + 1] * BLOCK

        if timed(log, 'write', off, lambda: _w(src, off)):
            def read_and_check():
                err = _r(off)
                if err:
                    return err
                return '' if dst[:] == src[:] else 'data mismatch'
            timed(log, 'read', off, read_and_check)

        def anchor_check():
            err = _r(anchor_off)
            if err:
                return err
            return '' if dst[:] == anchor[:] else 'anchor mismatch'
        timed(log, 'anchor-read', anchor_off, anchor_check)

        i += 1

    log.event('stop', 'iterations=%d records=%d' % (i, log.seq))
    os.close(fd)
    return 0


sys.exit(main())
EOF_PY
}

# ===================== control ===============================================
start_io() {
    _info "starting O_DIRECT I/O generator on host0"
    emit_runner | _ssh "${SSH_USER}@${HOST0_IP}" "cat > $RUNNER" || _fail "could not upload runner"
    _ssh "${SSH_USER}@${HOST0_IP}" "
        sudo rm -f $LOG $ERRF $PIDF
        sudo nohup setsid python3 -u $RUNNER >$ERRF 2>&1 &
        sleep 1
        echo '--- pid: '\$(sudo cat $PIDF 2>/dev/null)
        echo '--- first records:'; sudo head -3 $LOG 2>/dev/null
        [ -s $ERRF ] && { echo '--- runner stderr:'; sudo cat $ERRF; }
        true
    "
}

stop_io() {
    _info "stopping I/O generator on host0"
    _ssh "${SSH_USER}@${HOST0_IP}" "
        sudo pkill -f dnv-io-runner.py
        for i in \$(seq 20); do pgrep -f dnv-io-runner.py >/dev/null || break; sleep 0.2; done
        sudo pkill -9 -f dnv-io-runner.py 2>/dev/null
        sudo rm -f $PIDF
        true
    "
}

status_io() {
    _ssh "${SSH_USER}@${HOST0_IP}" "
        pgrep -af dnv-io-runner.py | head -3
        echo '--- records so far: '\$(sudo wc -l < $LOG 2>/dev/null)
        echo '--- last 3:'; sudo tail -3 $LOG 2>/dev/null
    "
}

# Drop a labelled marker into the log using host0's clock, so the report can
# split the run without worrying about clock skew between here and host0.
mark_io() {
    local label="${1:-mark}"
    _ssh "${SSH_USER}@${HOST0_IP}" "
        python3 -c \"
import json, time
t = time.time()
print(json.dumps({'op': 'mark', 'label': '$label',
                  'start': time.strftime('%Y-%m-%dT%H:%M:%S', time.localtime(t)) + '.%03d' % ((t % 1) * 1000),
                  'start_ts': round(t, 6)}))
\" | sudo tee -a $LOG >/dev/null" || _fail "could not write marker"
    _info "marked: $label"
}

# ===================== report ================================================
report_io() {
    local tmp
    tmp=$(mktemp)
    _ssh "${SSH_USER}@${HOST0_IP}" "sudo cat $LOG" > "$tmp" || _fail "could not fetch $LOG"

    python3 - "$tmp" <<'EOF_REPORT'
import json, sys
from collections import OrderedDict

recs, marks, events = [], [], []
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except ValueError:
        continue
    op = d.get('op')
    if op == 'mark':
        marks.append(d)
    elif op == 'event':
        events.append(d)
    else:
        recs.append(d)

start = next((m for m in marks if m['label'] == 'failover_start'), None)
end   = next((m for m in marks if m['label'] == 'failover_end'), None)

phases = OrderedDict()
if start and end:
    t0, t1 = start['start_ts'], end['start_ts']
    phases['before failover'] = [r for r in recs if r['stop_ts'] <= t0]
    phases['during failover'] = [r for r in recs if r['stop_ts'] > t0 and r['start_ts'] < t1]
    phases['after failover']  = [r for r in recs if r['start_ts'] >= t1]
else:
    phases['whole run'] = recs
phases['TOTAL'] = recs


def pct(vals, p):
    if not vals:
        return 0.0
    k = min(len(vals) - 1, int(round((p / 100.0) * (len(vals) - 1))))
    return vals[k]


def stats(rs):
    lat = sorted(r['lat_ms'] for r in rs)
    ok = sum(1 for r in rs if r['ok'])
    span = (max(r['stop_ts'] for r in rs) - min(r['start_ts'] for r in rs)) if rs else 0.0
    return {
        'n': len(rs), 'ok': ok, 'fail': len(rs) - ok,
        'rate': (100.0 * ok / len(rs)) if rs else 0.0,
        'span': span, 'iops': (len(rs) / span) if span > 0 else 0.0,
        'min': lat[0] if lat else 0.0,
        'avg': (sum(lat) / len(lat)) if lat else 0.0,
        'p50': pct(lat, 50), 'p95': pct(lat, 95), 'p99': pct(lat, 99),
        'max': lat[-1] if lat else 0.0,
    }


HDR = ('%-18s %-13s %7s %7s %6s %8s %9s %9s %9s %9s %9s %9s'
       % ('phase', 'op', 'IOs', 'ok', 'fail', 'succ%',
          'min ms', 'avg ms', 'p50 ms', 'p95 ms', 'p99 ms', 'max ms'))
ROW = ('%-18s %-13s %7d %7d %6d %7.2f%% %9.3f %9.3f %9.3f %9.3f %9.3f %9.3f')

print('=' * len(HDR))
print('I/O report -- %d I/Os, %d marker(s)' % (len(recs), len(marks)))
print('=' * len(HDR))
for e in events:
    print('  event %-6s %s  %s' % (e.get('kind'), e.get('start'), e.get('msg')))
for m in marks:
    print('  mark  %-14s %s' % (m['label'], m['start']))
print()
print(HDR)
print('-' * len(HDR))
for name, rs in phases.items():
    if name == 'TOTAL':
        print('-' * len(HDR))
    ops = sorted({r['op'] for r in rs})
    for op in ops:
        s = stats([r for r in rs if r['op'] == op])
        print(ROW % (name, op, s['n'], s['ok'], s['fail'], s['rate'],
                     s['min'], s['avg'], s['p50'], s['p95'], s['p99'], s['max']))
    s = stats(rs)
    print(ROW % (name, '(all)', s['n'], s['ok'], s['fail'], s['rate'],
                 s['min'], s['avg'], s['p50'], s['p95'], s['p99'], s['max']))
    print('%-18s %-13s window %.3fs, %.1f IO/s' % ('', '', s['span'], s['iops']))
print('=' * len(HDR))

fails = [r for r in recs if not r['ok']]
print('\nfailed I/Os: %d' % len(fails))
for r in fails[:50]:
    print('  seq=%-7d %-13s off=%-11d start=%s stop=%s lat=%9.3fms  %s'
          % (r['seq'], r['op'], r['offset'], r['start'], r['stop'], r['lat_ms'], r['err']))
if len(fails) > 50:
    print('  ... and %d more' % (len(fails) - 50))

# The longest I/Os are how the failover window shows up when nothing fails.
slow = sorted(recs, key=lambda r: -r['lat_ms'])[:10]
print('\nslowest 10 I/Os:')
for r in slow:
    print('  seq=%-7d %-13s off=%-11d start=%s lat=%9.3fms  ok=%s'
          % (r['seq'], r['op'], r['offset'], r['start'], r['lat_ms'], r['ok']))
EOF_REPORT
    local rc=$?
    rm -f "$tmp"

    echo
    _info "kernel messages on host0 (nvme/dm/blk):"
    _ssh "${SSH_USER}@${HOST0_IP}" \
        "sudo dmesg -T 2>/dev/null | grep -iE 'nvme|device-mapper|blk_update|I/O error' | tail -40; true"
    return $rc
}

case "${1:-status}" in
    start)  start_io ;;
    stop)   stop_io ;;
    status) status_io ;;
    mark)   mark_io "${2:-mark}" ;;
    report) report_io ;;
    *) echo "Usage: $0 start|stop|status|mark <label>|report" >&2; exit 1 ;;
esac
