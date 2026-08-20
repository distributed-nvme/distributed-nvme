#!/bin/bash
# barrier.sh <local_start_epoch> <script...> -- spin until this node's clock
# reaches the given epoch, then exec the failover script.  Used only to make
# the three failover scripts start together; it does not touch their contents.
t=$1; shift
python3 -c "
import time,sys
t=float('$t')
while time.time() < t - 0.002: time.sleep(0.0005)
while time.time() < t: pass
"
exec "$@"
