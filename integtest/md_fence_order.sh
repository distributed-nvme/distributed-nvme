#!/bin/bash
# md_fence_order.sh — the lab fact the failover's fence order rests on
# (dnv-worker.md RW24; architecture.md, Failover): with md alone, a write
# whose copy to one leg is failed by that leg's fence, while its copy to the
# other leg landed, is acknowledged; when the other leg is fenced before md
# can record the first leg's failure on it, both superblocks read equal and
# the next start copies the member in slot 0 over the other. So the last
# acknowledged write survives only when the slot-0 leg, the fence-last leg, is
# fenced last.
#
# Usage: bash integtest/md_fence_order.sh user@host [runs]
#
# On one lab guest with mdadm and python3, as root through passwordless
# sudo: two 1 GiB loop files, a dm-linear over each, an md RAID1 array over
# the two with dnv's create options (cnagent.md CN12) and md's cache set to
# write-through, so that each O_DIRECT|O_SYNC write is one data write. A
# writer writes 4 KiB blocks in order. Each run begins one leg's fence: it
# holds that leg's IO (suspend) until the writer blocks, fences the other
# leg whole (suspend, error table, resume), then gives the held leg the
# error table, stops the array, puts both legs back on their linears,
# assembles, waits for the resync and reads the last block the writer was
# told was written. "slot0-first" runs begin the slot-0 leg's fence first,
# as a fan-out with no order can; "slot0-last" runs begin the other leg's
# fence first, so that the slot-0 leg is fenced while the other leg still
# holds the write in flight, the case the fence round's bound leaves open.
# Each mode runs once with leg a in slot 0 and once with leg b, runs times
# over.
#
# Exit 0 when every slot0-last run kept the last acknowledged write. The
# slot0-first runs are printed, not judged: their loss needs the write in
# flight at the fence, which the hold makes likely, not certain. No dnv
# program runs; nothing else may use the guest's md or loop devices at the
# same time.
set -euo pipefail
T=${1:?usage: md_fence_order.sh user@host [runs]}
RUNS=${2:-1}

ssh -o BatchMode=yes "$T" sudo -n bash -s -- "$RUNS" <<'REMOTE'
set -u
RUNS=$1
D=/var/tmp/dnv-md-fence-order
MD=/dev/md/dnv-md-fence-order
NAME=dnv-md-fence-order
LEGS="dnv-mfo-a dnv-mfo-b"
SZ=$((1024 * 1024 * 1024))
SECT=$((SZ / 512))
# The array's data offset: 3 MiB, as a 1 GiB group of 1 MiB blocks with a
# 128-block bitmap chunk has.
DOFF=$((3072 * 1024))
mkdir -p "$D"

cat >"$D/writer.py" <<'EOF'
# writer.py DEV LOG: block i (4 KiB at i*4096) holds 256 copies of
# b'W%014d\n' % i, written one at a time with O_DIRECT|O_SYNC; each
# acknowledged index is logged "ACK i", the first failure "ERR i".
import mmap, os, sys, time
dev, log = sys.argv[1], sys.argv[2]
fd = os.open(dev, os.O_WRONLY | os.O_DIRECT | os.O_SYNC)
buf = mmap.mmap(-1, 4096)
lf = open(log, "a", buffering=1)
for i in range(200000):
    buf.seek(0); buf.write((b"W%014d\n" % i) * 256)
    try:
        os.pwrite(fd, buf, i * 4096)
    except OSError as e:
        lf.write("ERR %d %s\n" % (i, e)); sys.exit(1)
    lf.write("ACK %d\n" % i)
    time.sleep(0.001)
EOF
cat >"$D/rd.py" <<'EOF'
# rd.py DEV BYTEOFF IDX: what block IDX at BYTEOFF+IDX*4096 holds (O_DIRECT).
import mmap, os, sys
dev, base, i = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
fd = os.open(dev, os.O_RDONLY | os.O_DIRECT)
buf = mmap.mmap(-1, 4096)
os.preadv(fd, [buf], base + i * 4096)
b = bytes(buf)
print("KEPT" if b == (b"W%014d\n" % i) * 256 else
      "ZERO" if b == bytes(4096) else "OTHER")
EOF

lin() { echo "0 $SECT linear $(cat "$D/loop-$1") 0"; }
lastack() { awk '/^ACK/{n=$2} END{print n+0}' "$D/w.log"; }
cleanup() {
	pkill -f '[d]nv-md-fence-order/writer.py' || true
	mdadm --stop "$MD" >/dev/null 2>&1 || true
	for l in $LEGS; do
		dmsetup remove --retry "$l" >/dev/null 2>&1 || true
		if [ -f "$D/loop-$l" ]; then losetup -d "$(cat "$D/loop-$l")" || true; fi
		rm -f "$D/$l.img" "$D/loop-$l"
	done
}
trap cleanup EXIT

# run MODE SLOT0: one run; prints KEPT, ZERO or OTHER for the last
# acknowledged block after the new start's resync.
run() {
	local mode=$1 s0=$2 s1 held other wp n l
	s1=dnv-mfo-a
	[ "$s0" = dnv-mfo-a ] && s1=dnv-mfo-b
	if [ "$mode" = slot0-first ]; then held=$s0 other=$s1; else held=$s1 other=$s0; fi
	cleanup
	rm -f "$D/w.log"
	for l in $LEGS; do
		truncate -s "$SZ" "$D/$l.img"
		losetup -f --show "$D/$l.img" >"$D/loop-$l"
		dmsetup create "$l" --table "$(lin "$l")"
	done
	mdadm --create "$MD" --name "$NAME" --level 1 --raid-devices 2 \
		--bitmap internal --bitmap-chunk 131072K --data-offset 3072K \
		--homehost any --run --assume-clean --failfast \
		"/dev/mapper/$s0" "/dev/mapper/$s1" </dev/null >/dev/null 2>&1
	echo "write through" >"/sys/block/$(basename "$(readlink -f "$MD")")/queue/write_cache"
	python3 "$D/writer.py" "$MD" "$D/w.log" &
	wp=$!
	sleep 2
	# Begin the held leg's fence: its IO waits until the writer blocks ...
	dmsetup suspend "$held"
	local prev=x cur
	for _ in $(seq 40); do
		sleep 0.5
		cur=$(lastack)
		[ "$cur" = "$prev" ] && break
		prev=$cur
	done
	# ... fence the other leg whole, then end the held leg's fence.
	dmsetup suspend "$other"
	dmsetup load "$other" --table "0 $SECT error"
	dmsetup resume "$other"
	sleep 1
	dmsetup load "$held" --table "0 $SECT error"
	dmsetup resume "$held"
	for _ in $(seq 60); do kill -0 "$wp" 2>/dev/null || break; sleep 0.5; done
	kill "$wp" 2>/dev/null || true
	wait "$wp" 2>/dev/null || true
	n=$(lastack)
	mdadm --stop "$MD" >/dev/null 2>&1
	for l in $LEGS; do
		dmsetup load "$l" --table "$(lin "$l")"
		dmsetup resume "$l"
	done
	mdadm --assemble "$MD" --name "$NAME" /dev/mapper/dnv-mfo-a \
		/dev/mapper/dnv-mfo-b >/dev/null 2>&1
	mdadm --wait "$MD" >/dev/null 2>&1 || true
	echo "$(python3 "$D/rd.py" "$MD" 0 "$n") last-ack=$n"
}

bad=0
for r in $(seq "$RUNS"); do
	for mode in slot0-first slot0-last; do
		for s0 in dnv-mfo-a dnv-mfo-b; do
			res=$(run "$mode" "$s0")
			echo "run $r $mode slot0=$s0: $res"
			if [ "$mode" = slot0-last ] && [ "${res%% *}" != KEPT ]; then
				bad=$((bad + 1))
			fi
		done
	done
done
if [ "$bad" -ne 0 ]; then
	echo "FAIL: $bad slot0-last runs lost the last acknowledged write"
	exit 1
fi
echo "PASS: every slot0-last run kept the last acknowledged write"
REMOTE
