#!/bin/bash
# common.sh -- shared constants and helpers for the failover1 two-level ANA test.
#
# Topology:
#   dn0            data node, owns the real (dm-zero + dm-delay 50ms) storage
#    |-- ss-to-cn0 (ana grp 1 optimized)     -> cn0
#    `-- ss-to-cn1 (ana grp 2 non-optimized) -> cn1
#   cn0 / cn1      connection nodes, re-export the same NQN (ss-path) to host0
#   host0          runs parallel O_DIRECT IO against the aggregated mpath device

set -o pipefail

HOST0_IP=192.168.122.193
DN0_IP=192.168.122.48
CN0_IP=192.168.122.125
CN1_IP=192.168.122.229

NVMET=/sys/kernel/config/nvmet
PORT_TCP=4420

# ---- sizes -----------------------------------------------------------------
DEV_SECTORS=2097152          # 1 GiB in 512B sectors

# ---- device names (dnv- prefix is what the udev rule below matches) --------
D_ZERO=dnv-zero
D_DELAY=dnv-delay
D_ERR_CN0=dnv-err-cn0
D_ERR_CN1=dnv-err-cn1
D_LIN_CN0=dnv-lin-cn0
D_LIN_CN1=dnv-lin-cn1

D_ERR_ON=dnv-err-on           # + node suffix, e.g. dnv-err-on-cn0
D_LIN_ON=dnv-lin-on

# ---- NQNs ------------------------------------------------------------------
NQN_TO_CN0=nqn.2026-08.org.dnv:dn0.cn0
NQN_TO_CN1=nqn.2026-08.org.dnv:dn0.cn1
NQN_PATH=nqn.2026-08.org.dnv:vol0          # shared by cn0 and cn1

# host NQNs (explicit: these VMs are clones and would otherwise share a
# DMI-derived default hostnqn, which collides on the target)
HOSTNQN_HOST0=nqn.2014-08.org.nvmexpress:uuid:1a788e0a-02bd-4ae9-9a4b-e4aad8b04c26
HOSTNQN_CN0=nqn.2026-08.org.dnv:host:cn0
HOSTNQN_CN1=nqn.2026-08.org.dnv:host:cn1

# shared namespace identity -- must be byte-identical on cn0 and cn1 or the
# host refuses to aggregate the two controllers into one namespace head
NS_UUID=6f1a2b3c-4d5e-4f60-8a1b-2c3d4e5f6071
NS_NGUID=6f1a2b3c4d5e4f608a1b2c3d4e5f6071
SUBSYS_MODEL=dnv-path
SUBSYS_SERIAL=DNVPATHVOL0

log() { printf '[%s] %s\n' "$(date +%H:%M:%S.%3N)" "$*" >&2; }
die() { log "FATAL: $*"; exit 1; }

# ---------------------------------------------------------------------------
# udev: keep blkid/bcache probes off our dm devices.  OPTIONS+="ignore_device"
# is obsolete; the DM_UDEV_DISABLE_* flags are what actually work, and the file
# must sort after 55-dm.rules and before 60-persistent-storage-dm.rules.
install_udev_rule() {
  local f=/etc/udev/rules.d/58-dnv-test.rules
  sudo tee "$f" >/dev/null <<'RULE'
ACTION=="remove", GOTO="dnv_end"
SUBSYSTEM!="block", GOTO="dnv_end"
KERNEL!="dm-[0-9]*", GOTO="dnv_end"
ENV{DM_NAME}!="dnv-*", GOTO="dnv_end"
ENV{DM_UDEV_DISABLE_SUBSYSTEM_RULES_FLAG}="1"
ENV{DM_UDEV_DISABLE_DISK_RULES_FLAG}="1"
ENV{DM_UDEV_DISABLE_OTHER_RULES_FLAG}="1"
ENV{UDEV_DISABLE_PERSISTENT_STORAGE_RULES_FLAG}="1"
ENV{.DM_NOSCAN}="1"
OPTIONS+="nowatch"
LABEL="dnv_end"
RULE
  sudo udevadm control --reload-rules
}

# ---------------------------------------------------------------------------
# dmsetup helpers
dm_exists() { sudo dmsetup info "$1" >/dev/null 2>&1; }

dm_create() { # name, table
  local n=$1; shift
  if dm_exists "$n"; then
    log "dm $n already exists, reloading table"
    sudo dmsetup suspend --nolockfs --noflush --noudevsync "$n" || true
    printf '%s\n' "$*" | sudo dmsetup load "$n" || die "load $n"
    sudo dmsetup resume --noudevsync "$n" || die "resume $n"
  else
    printf '%s\n' "$*" | sudo dmsetup create --noudevsync "$n" || die "create $n"
  fi
}

# dm_reload <name> <table> [deadline_s]
# Suspend/load/resume with an escape hatch: a suspend can block forever on bios
# already handed to an underlying nvme path that has no usable route.  We run
# the suspend in the background and report if it misses the deadline.
dm_reload() {
  local n=$1 tbl=$2 deadline=${3:-20}
  local t0 t1
  t0=$(date +%s.%N)
  sudo dmsetup suspend --nolockfs --noflush --noudevsync "$n" &
  local pid=$!
  local waited=0
  while kill -0 $pid 2>/dev/null; do
    sleep 0.05
    waited=$((waited+1))
    if [ $waited -gt $((deadline*20)) ]; then
      log "WARN: suspend of $n exceeded ${deadline}s deadline"
      break
    fi
  done
  wait $pid 2>/dev/null
  printf '%s\n' "$tbl" | sudo dmsetup load "$n" || die "load $n"
  sudo dmsetup resume --noudevsync "$n" || die "resume $n"
  t1=$(date +%s.%N)
  log "dm_reload $n -> [$tbl] took $(echo "$t1 - $t0" | bc)s"
}

# ---------------------------------------------------------------------------
# nvmet helpers
nvmet_write() { # file value  -- skip if already equal (attrs read back padded)
  local f=$1 v=$2 cur
  cur=$(sudo cat "$f" 2>/dev/null | head -1 | sed -e 's/[[:space:]]*$//')
  [ "$cur" = "$(printf '%s' "$v" | sed -e 's/[[:space:]]*$//')" ] && return 0
  printf '%s' "$v" | sudo tee "$f" >/dev/null || die "write $v -> $f"
}

nvmet_port_create() { # id ip
  local id=$1 ip=$2 p=$NVMET/ports/$1
  sudo mkdir -p "$p"
  nvmet_write "$p/addr_trtype" tcp
  nvmet_write "$p/addr_adrfam" ipv4
  nvmet_write "$p/addr_traddr" "$ip"
  nvmet_write "$p/addr_trsvcid" "$PORT_TCP"
}

nvmet_ana_set() { # portid grpid state
  local p=$NVMET/ports/$1/ana_groups/$2
  sudo mkdir -p "$p" 2>/dev/null || true
  nvmet_write "$p/ana_state" "$3"
}

nvmet_link_subsys() { # portid nqn   -- relinking a live link bounces controllers
  local l=$NVMET/ports/$1/subsystems/$2
  { [ -L "$l" ] || [ -e "$l" ]; } && return 0
  sudo ln -s "$NVMET/subsystems/$2" "$l" || die "link $2 -> port $1"
}

# ---------------------------------------------------------------------------
# host-side nvme helpers
# Resolve the multipath head block device for a subsystem NQN.
nvme_head_for() { # nqn
  local nqn=$1 s
  for s in /sys/class/nvme-subsystem/nvme-subsys*; do
    [ -e "$s/subsysnqn" ] || continue
    [ "$(cat "$s/subsysnqn")" = "$nqn" ] || continue
    local n
    for n in "$s"/nvme*n*; do
      [ -e "$n/dev" ] || continue
      basename "$n"; return 0
    done
  done
  return 1
}

# Path (per-controller) devices under a subsystem: nvmeXcYnZ
nvme_path_devs_for() { # ctrl name e.g. nvme0
  local c=$1
  ls -d /sys/class/nvme/$c/${c}c*n* 2>/dev/null
}

# ANA state of the single upstream namespace on a CN, read purely from local
# sysfs (no query to the target).
local_ana_state() { # ctrl
  local d
  for d in $(nvme_path_devs_for "$1"); do
    [ -e "$d/ana_state" ] && { cat "$d/ana_state"; return 0; }
  done
  # non-multipath fallback
  [ -e /sys/block/$1n1/ana_state ] && { cat /sys/block/$1n1/ana_state; return 0; }
  return 1
}

# Find the controller (nvmeX) connected to a given subsystem NQN.
ctrl_for_nqn() { # nqn
  local c
  for c in /sys/class/nvme/nvme*; do
    [ -e "$c/subsysnqn" ] || continue
    [ "$(cat "$c/subsysnqn")" = "$1" ] && { basename "$c"; return 0; }
  done
  return 1
}
