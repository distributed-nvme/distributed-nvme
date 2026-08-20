#!/bin/bash
# run.sh -- orchestrate the whole failover experiment from the workstation.
set -u
HOST0=192.168.122.193; DN0=192.168.122.48; CN0=192.168.122.125; CN1=192.168.122.229
DUR=${DUR:-45}; BASELINE=${BASELINE:-12}; THREADS=${THREADS:-8}
OUT=${OUT:-results}; mkdir -p "$OUT"
SSH="ssh -o ControlMaster=auto -o ControlPersist=300"
cp_() { echo "-o ControlPath=/tmp/cmr-$1"; }

for ip in $HOST0 $DN0 $CN0 $CN1; do $SSH $(cp_ $ip) yupeng@$ip true; done

# --- clock offset of each node relative to host0 (median of 7 samples) ------
skew_to_ws() { # ip -> node_clock - workstation_clock
  python3 - "$1" <<'PY'
import subprocess,sys,time,statistics
ip=sys.argv[1]; v=[]
for _ in range(7):
    t0=time.time()
    r=float(subprocess.check_output(["ssh","-o","ControlPath=/tmp/cmr-"+ip,
        "yupeng@"+ip,"date +%s.%N"]))
    t1=time.time()
    v.append(r-(t0+t1)/2)
print("%.6f"%statistics.median(v))
PY
}
SK_HOST0=$(skew_to_ws $HOST0); SK_DN0=$(skew_to_ws $DN0)
SK_CN0=$(skew_to_ws $CN0);     SK_CN1=$(skew_to_ws $CN1)
echo "clock offsets vs workstation: host0=$SK_HOST0 dn0=$SK_DN0 cn0=$SK_CN0 cn1=$SK_CN1"

# --- 1. start the IO load on host0 ------------------------------------------
# Run the ssh in the background rather than trying to detach on the far side:
# a backgrounded remote command still holds the ssh channel open, which silently
# serialised the load ahead of the failover on the first attempt.
$SSH $(cp_ $HOST0) yupeng@$HOST0 \
  "cd ~/failover1 && sudo ./io_load.py /dev/nvme0n1 $DUR /tmp/io.json $THREADS" \
  > "$OUT/io.log" 2>&1 &
IOPID=$!
echo "IO load started (${DUR}s, $THREADS threads, ssh pid $IOPID); baseline ${BASELINE}s"
sleep "$BASELINE"

# --- 2. fire all three failover scripts at the same wall-clock instant ------
WS_T=$(python3 -c "import time;print('%.6f'%(time.time()+1.5))")
t_of() { python3 -c "print('%.6f'%($WS_T + $1))"; }
echo "barrier at workstation epoch $WS_T"

$SSH $(cp_ $DN0) yupeng@$DN0 "cd ~/failover1 && ./barrier.sh $(t_of $SK_DN0) ./dn0_failover.sh" > "$OUT/dn0.stamps" 2>"$OUT/dn0.err" &
P1=$!
$SSH $(cp_ $CN0) yupeng@$CN0 "cd ~/failover1 && ./barrier.sh $(t_of $SK_CN0) ./cn0_failover.sh" > "$OUT/cn0.stamps" 2>"$OUT/cn0.err" &
P2=$!
$SSH $(cp_ $CN1) yupeng@$CN1 "cd ~/failover1 && ./barrier.sh $(t_of $SK_CN1) ./cn1_failover.sh" > "$OUT/cn1.stamps" 2>"$OUT/cn1.err" &
P3=$!
wait $P1 $P2 $P3
echo "all failover scripts returned"

# --- 3. collect -------------------------------------------------------------
wait $IOPID
cat "$OUT/io.log"
scp -q -o ControlPath=/tmp/cmr-$HOST0 yupeng@$HOST0:/tmp/io.json "$OUT/io.json"

# --- 4. normalise every stamp into host0's clock ---------------------------
python3 - "$OUT" "$SK_HOST0" "$SK_DN0" "$SK_CN0" "$SK_CN1" <<'PY'
import json,sys,os
out,skh,skd,skc0,skc1 = sys.argv[1], *map(float, sys.argv[2:6])
off = {'dn0': skd-skh, 'cn0': skc0-skh, 'cn1': skc1-skh}
ev=[]
for node in ('dn0','cn0','cn1'):
    p=os.path.join(out,node+'.stamps')
    for line in open(p):
        line=line.strip()
        if not line: continue
        parts=line.split(None,1)
        try: t=float(parts[0])
        except ValueError: continue
        ev.append([t-off[node], parts[1] if len(parts)>1 else ''])
ev.sort()
json.dump({'fo_start':ev[0][0],'fo_end':ev[-1][0],'events':ev,
           'clock_offsets_vs_host0':off},
          open(os.path.join(out,'markers.json'),'w'), indent=1)
print('fo_start=%.3f fo_end=%.3f span=%.3fs events=%d'
      % (ev[0][0], ev[-1][0], ev[-1][0]-ev[0][0], len(ev)))
PY
python3 report.py "$OUT/io.json" "$OUT/markers.json" | tee "$OUT/report.txt"
