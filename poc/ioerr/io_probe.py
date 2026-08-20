#!/usr/bin/python3
"""
io_probe.py -- continuous O_DIRECT probe against one block device.

Runs on host0.  A worker thread issues a 4 KiB write + read back every
PERIOD seconds; the main thread samples the worker so a request that never
returns is reported as a "stuck" interval rather than silently stalling the
whole probe.  Every transition (ok -> err, ok -> stuck, err -> ok, ...) is
emitted with both a wall-clock epoch and a probe-relative offset, so the
orchestrator can subtract the epoch at which it injected the fault and get the
time-to-first-IO-error directly.

Note: dd is not used anywhere in this harness -- the uutils dd shipped on these
VMs mishandles iflag/oflag=direct (false failures and silent zero writes).

Usage: io_probe.py <device> <duration_s> <out.json>
"""
import errno
import json
import os
import random
import sys
import threading
import time

BS = 4096
SPAN = 16 * 1024 * 1024      # keep IO inside the first 16 MiB of the lane
PERIOD = 0.1
STUCK_AFTER = 3.0            # a single IO outstanding this long is "stuck"


def main():
    dev, dur, out = sys.argv[1], float(sys.argv[2]), sys.argv[3]

    buf = bytearray(BS)
    for i in range(0, BS, 8):
        buf[i:i + 8] = b'\xa5\x5a\xa5\x5a\xa5\x5a\xa5\x5a'
    # O_DIRECT needs an aligned buffer; a mmap'd anonymous page is page aligned.
    import mmap
    abuf = mmap.mmap(-1, BS)
    abuf.write(bytes(buf))

    st = {
        'dev': dev,
        'start_epoch': time.time(),
        'events': [],        # transitions only
        'ok': 0, 'err': 0,
        'cur_op_start': None,
        'state': 'init',
    }
    lock = threading.Lock()
    t0 = time.monotonic()

    def rel():
        return round(time.monotonic() - t0, 3)

    def emit(ev, **kw):
        # only record transitions, plus every distinct errno
        last = st['events'][-1] if st['events'] else None
        key = (ev, kw.get('name'))
        if last and (last['ev'], last.get('name')) == key:
            last['n'] = last.get('n', 1) + 1
            last['last_t'] = rel()
            return
        e = {'t': rel(), 'epoch': round(time.time(), 3), 'ev': ev, 'n': 1}
        e.update(kw)
        st['events'].append(e)

    def worker():
        fd = None
        while True:
            with lock:
                st['cur_op_start'] = time.monotonic()
            try:
                if fd is None:
                    fd = os.open(dev, os.O_RDWR | os.O_DIRECT)
                off = random.randrange(0, SPAN // BS) * BS
                os.pwrite(fd, abuf, off)
                os.pread(fd, BS, off)
                with lock:
                    st['ok'] += 1
                    st['cur_op_start'] = None
                    st['state'] = 'ok'
                    emit('ok')
            except OSError as e:
                with lock:
                    st['err'] += 1
                    st['cur_op_start'] = None
                    st['state'] = 'err'
                    emit('err', errno=e.errno,
                         name=errno.errorcode.get(e.errno, str(e.errno)))
                if fd is not None:
                    try:
                        os.close(fd)
                    except OSError:
                        pass
                    fd = None
                time.sleep(0.2)
            time.sleep(PERIOD)

    th = threading.Thread(target=worker, daemon=True)
    th.start()

    stuck_reported = False
    end = time.monotonic() + dur
    while time.monotonic() < end:
        time.sleep(0.25)
        with lock:
            cs = st['cur_op_start']
            if cs is not None and time.monotonic() - cs > STUCK_AFTER:
                if not stuck_reported:
                    e = {'t': round(cs - t0, 3),
                         'epoch': round(st['start_epoch'] + (cs - t0), 3),
                         'ev': 'stuck', 'n': 1}
                    st['events'].append(e)
                    st['state'] = 'stuck'
                    stuck_reported = True
            else:
                stuck_reported = False

    with lock:
        st['end_epoch'] = time.time()
        st['duration'] = rel()
        st.pop('cur_op_start', None)
        first_err = next((e for e in st['events'] if e['ev'] == 'err'), None)
        first_stuck = next((e for e in st['events'] if e['ev'] == 'stuck'), None)
        st['first_err'] = first_err
        st['first_stuck'] = first_stuck
        with open(out, 'w') as f:
            json.dump(st, f, indent=1)
    # the worker may be blocked forever inside a syscall on a wedged path
    sys.stdout.flush()
    os._exit(0)


if __name__ == '__main__':
    main()
