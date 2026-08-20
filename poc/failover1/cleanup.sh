#!/bin/bash
# cleanup.sh -- tear this node down to a blank slate.
#   phase 1 (disconnect) : drop nvme host connections
#   phase 2 (nvmet)      : remove every nvmet port/subsystem
#   phase 3 (dm)         : defuse and remove every dnv-* dm device
# The orchestrator runs the phases across nodes in the right order.
. "$(dirname "$0")/common.sh"

phase=${1:-all}

do_disconnect() {
  local c
  for c in /sys/class/nvme/nvme*; do
    [ -e "$c/subsysnqn" ] || continue
    local nqn; nqn=$(cat "$c/subsysnqn")
    case "$nqn" in
      nqn.2026-08.org.dnv:*|nqn.2026-08.dnv.test:*)
        log "disconnect $(basename "$c") ($nqn)"
        sudo nvme disconnect -d "$(basename "$c")" 2>&1 | sed 's/^/  /' ;;
    esac
  done
  # catch anything left over from older tests
  sudo nvme disconnect-all >/dev/null 2>&1 || true
}

do_nvmet() {
  [ -d "$NVMET" ] || return 0
  local p n l
  for p in "$NVMET"/ports/*; do
    [ -d "$p" ] || continue
    for l in "$p"/subsystems/*; do
      [ -L "$l" ] && { log "unlink $l"; sudo rm -f "$l"; }
    done
    for l in "$p"/referrals/*; do
      [ -d "$l" ] && sudo rmdir "$l"
    done
    for l in "$p"/ana_groups/*; do
      [ -d "$l" ] || continue
      [ "$(basename "$l")" = "1" ] && continue   # group 1 is not removable
      sudo rmdir "$l" 2>/dev/null || true
    done
  done
  for n in "$NVMET"/subsystems/*; do
    [ -d "$n" ] || continue
    for l in "$n"/namespaces/*; do
      [ -d "$l" ] || continue
      echo 0 | sudo tee "$l/enable" >/dev/null 2>&1 || true
      sudo rmdir "$l" 2>/dev/null || true
    done
    for l in "$n"/allowed_hosts/*; do
      [ -L "$l" ] && sudo rm -f "$l"
    done
    sudo rmdir "$n" 2>/dev/null || true
  done
  for p in "$NVMET"/ports/*; do
    [ -d "$p" ] && sudo rmdir "$p" 2>/dev/null || true
  done
  for l in "$NVMET"/hosts/*; do
    [ -d "$l" ] && sudo rmdir "$l" 2>/dev/null || true
  done
}

do_dm() {
  # 1. defuse: every dnv-* device gets an error table so nothing can block
  local d
  for d in $(sudo dmsetup ls 2>/dev/null | awk '/^dnv-/{print $1}'); do
    local sz; sz=$(sudo blockdev --getsz "/dev/mapper/$d" 2>/dev/null || echo $DEV_SECTORS)
    sudo dmsetup suspend --nolockfs --noflush --noudevsync "$d" 2>/dev/null || true
    printf '0 %s error\n' "$sz" | sudo dmsetup load "$d" 2>/dev/null || true
    sudo dmsetup resume --noudevsync "$d" 2>/dev/null || true
  done
  # 2. remove, retrying so stacked devices come off top-down
  local i
  for i in 1 2 3 4 5; do
    local left=0
    for d in $(sudo dmsetup ls 2>/dev/null | awk '/^dnv-/{print $1}'); do
      sudo dmsetup remove --noudevsync "$d" 2>/dev/null || left=1
    done
    [ $left -eq 0 ] && break
    sleep 0.3
  done
  sudo dmsetup ls | grep -q '^dnv-' && log "WARN: dm devices left: $(sudo dmsetup ls | grep '^dnv-' | tr '\n' ' ')"
  return 0
}

case "$phase" in
  disconnect) do_disconnect ;;
  nvmet)      do_nvmet ;;
  dm)         do_dm ;;
  all)        do_disconnect; do_nvmet; do_dm ;;
  *) die "unknown phase $phase" ;;
esac
log "cleanup phase=$phase done on $(hostname)"
