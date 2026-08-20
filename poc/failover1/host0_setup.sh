#!/bin/bash
# host0_setup.sh -- connect to the shared path NQN on both cn0 and cn1 and
# confirm the kernel aggregated them into a single multipath namespace.
. "$(dirname "$0")/common.sh"
set -e
sudo modprobe nvme-tcp 2>/dev/null || true
sudo mkdir -p /etc/nvme
printf '%s\n' "$HOSTNQN_HOST0" | sudo tee /etc/nvme/hostnqn >/dev/null

for ip in $CN0_IP $CN1_IP; do
  if ! sudo nvme list-subsys 2>/dev/null | grep -q "traddr=$ip"; then
    log "connect -> $ip"
    sudo nvme connect -t tcp -a "$ip" -s "$PORT_TCP" -n "$NQN_PATH" \
         --hostnqn "$HOSTNQN_HOST0" >/dev/null
  fi
done

DEV=""
for i in $(seq 1 100); do
  h=$(nvme_head_for "$NQN_PATH" || true)
  if [ -n "$h" ] && [ -b "/dev/$h" ]; then DEV=/dev/$h; break; fi
  sleep 0.1
done
[ -n "$DEV" ] || die "no multipath device for $NQN_PATH"

SUBSYS=$(dirname "$(readlink -f /sys/class/nvme-subsystem/*/"${DEV#/dev/}" 2>/dev/null | head -1)")
npaths=$(ls -d "$SUBSYS"/nvme[0-9]* 2>/dev/null | grep -c 'nvme[0-9]*$')
log "mpath dev=$DEV size=$(sudo blockdev --getsz "$DEV") paths=$npaths"
sudo nvme list-subsys | sed 's/^/  /' >&2
for p in /sys/class/nvme/nvme*/nvme*c*n*; do
  [ -e "$p/ana_state" ] || continue
  log "  path $(basename "$p"): ana_state=$(cat "$p/ana_state") grpid=$(cat "$p/ana_grpid")"
done
[ "$npaths" -ge 2 ] || die "expected 2 paths, got $npaths"
echo "$DEV"
