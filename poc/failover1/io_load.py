#!/usr/bin/python3
"""
io_load.py <device> <duration_s> <out.json> [threads] [queue_depth_note]

Parallel O_DIRECT load generator for host0.

dd is deliberately not used: the uutils dd 0.8.0 shipped on these VMs mishandles
iflag/oflag=direct (false EINVAL failures and silent zero writes), so every
verification here goes through os.O_DIRECT with an mmap'd page-aligned buffer.

Each worker thread loops: pwrite(4K) then pread(4K) at a random 4K-aligned
offset inside the first SPAN bytes.  Every single IO is recorded as
(start_epoch, latency_s, op, status) where status is 0 for success or the
errno for a failure -- that is enough to bucket by phase and compute latency
distributions afterwards.
"""
import errno
import json
import mmap
import os
import random
import sys
import threading
import time

BS = 4096
SPAN = 64 * 1024 * 1024


def main():
    dev = sys.argv[1]
    dur = float(sys.argv[2])
    out = sys.argv[3]
    nthreads = int(sys.argv[4]) if len(sys.argv) > 4 else 8

    pattern = bytes(bytearray([0xA5, 0x5A] * (BS // 2)))
    recs = []                      # (start_epoch, lat, op, status)
    lock = threading.Lock()
    stop = threading.Event()
    inflight = {}                  # tid -> (start_epoch, op)

    def worker(tid):
        buf = mmap.mmap(-1, BS)    # anonymous mmap is page aligned -> O_DIRECT ok
        buf.write(pattern)
        rnd = random.Random(1000 + tid)
        fd = None
        local = []
        while not stop.is_set():
            for op in ('w', 'r'):
                if stop.is_set():
                    break
                off = rnd.randrange(0, SPAN // BS) * BS
                t0 = time.time()
                with lock:
                    inflight[tid] = (t0, op)
                try:
                    if fd is None:
                        fd = os.open(dev, os.O_RDWR | os.O_DIRECT)
                    if op == 'w':
                        os.pwrite(fd, buf, off)
                    else:
                        os.pread(fd, BS, off)
                    st = 0
                except OSError as e:
                    st = e.errno or -1
                    if fd is not None:
                        try:
                            os.close(fd)
                        except OSError:
                            pass
                        fd = None
                t1 = time.time()
                local.append((round(t0, 6), round(t1 - t0, 6), op, st))
                with lock:
                    inflight.pop(tid, None)
                if st:
                    time.sleep(0.02)     # don't spin on a hard-failing device
                if len(local) >= 64:
                    with lock:
                        recs.extend(local)
                    local = []
        with lock:
            recs.extend(local)

    ths = [threading.Thread(target=worker, args=(i,), daemon=True)
           for i in range(nthreads)]
    start = time.time()
    for t in ths:
        t.start()
    end = time.monotonic() + dur
    while time.monotonic() < end:
        time.sleep(0.2)
    stop.set()
    # give threads a moment to flush; anything still in a syscall is stuck on a
    # wedged path and will never come back -- record it as such.
    time.sleep(1.5)
    with lock:
        stuck = [{'tid': k, 'start_epoch': round(v[0], 6), 'op': v[1],
                  'age_s': round(time.time() - v[0], 3)}
                 for k, v in inflight.items()]
        data = {
            'dev': dev,
            'threads': nthreads,
            'bs': BS,
            'start_epoch': round(start, 6),
            'end_epoch': round(time.time(), 6),
            'stuck_at_exit': stuck,
            'recs': sorted(recs),
        }
    with open(out, 'w') as f:
        json.dump(data, f)
    print(json.dumps({'total': len(data['recs']),
                      'ok': sum(1 for r in data['recs'] if r[3] == 0),
                      'err': sum(1 for r in data['recs'] if r[3] != 0),
                      'stuck': len(stuck)}))
    sys.stdout.flush()
    os._exit(0)     # workers may be blocked forever in an uninterruptible syscall


if __name__ == '__main__':
    main()
