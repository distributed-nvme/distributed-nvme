#!/bin/bash
# dn0_setup.sh -- build the data node: zero -> delay(50ms) -> linear lanes,
# exported as two subsystems in two ANA groups on one port.
. "$(dirname "$0")/common.sh"
set -e

sudo modprobe dm-zero dm-delay 2>/dev/null || true
sudo modprobe nvmet nvmet-tcp
install_udev_rule

S=$DEV_SECTORS

# 1.1 zero-dev
dm_create $D_ZERO "0 $S zero"
# 1.2 delay-dev: 50ms on reads; with no write/flush params dm-delay reuses the
#     read parameters for writes and flushes, so all IO is delayed 50ms.
dm_create $D_DELAY "0 $S delay /dev/mapper/$D_ZERO 0 50"
# 1.3 two error devices
dm_create $D_ERR_CN0 "0 $S error"
dm_create $D_ERR_CN1 "0 $S error"
# 1.4 the two lanes: cn0 healthy (on the delay), cn1 broken (on its error dev)
dm_create $D_LIN_CN0 "0 $S linear /dev/mapper/$D_DELAY 0"
dm_create $D_LIN_CN1 "0 $S linear /dev/mapper/$D_ERR_CN1 0"

sudo dmsetup ls --tree | sed 's/^/  /' >&2

# 2.1 port-dn0
nvmet_port_create 1 "$DN0_IP"
# 2.2 ana groups: 1 = optimized-dn0 (always present), 2 = non-optimized-dn0
nvmet_ana_set 1 1 optimized
nvmet_ana_set 1 2 non-optimized

mk_host() { sudo mkdir -p "$NVMET/hosts/$1"; }
mk_host "$HOSTNQN_CN0"
mk_host "$HOSTNQN_CN1"

# 2.2/2.3 ss-to-cn0 + ns-to-cn0 on the healthy lane, ANA group 1 (optimized)
mk_subsys() { # nqn serial model
  sudo mkdir -p "$NVMET/subsystems/$1"
  nvmet_write "$NVMET/subsystems/$1/attr_allow_any_host" 0
  nvmet_write "$NVMET/subsystems/$1/attr_serial" "$2"
  nvmet_write "$NVMET/subsystems/$1/attr_model"  "$3"
}
mk_ns() { # nqn nsid devpath grpid
  local n=$NVMET/subsystems/$1/namespaces/$2
  sudo mkdir -p "$n"
  nvmet_write "$n/device_path" "$3"
  nvmet_write "$n/ana_grpid"   "$4"
  nvmet_write "$n/enable" 1
}

mk_subsys "$NQN_TO_CN0" DNVDN0CN0 dnv-dn0
mk_ns     "$NQN_TO_CN0" 1 "/dev/mapper/$D_LIN_CN0" 1
sudo ln -sfn "$NVMET/hosts/$HOSTNQN_CN0" "$NVMET/subsystems/$NQN_TO_CN0/allowed_hosts/$HOSTNQN_CN0"
nvmet_link_subsys 1 "$NQN_TO_CN0"

# 2.4/2.5 ss-to-cn1 + ns-to-cn1 on the broken lane, ANA group 2 (non-optimized)
mk_subsys "$NQN_TO_CN1" DNVDN0CN1 dnv-dn0
mk_ns     "$NQN_TO_CN1" 1 "/dev/mapper/$D_LIN_CN1" 2
sudo ln -sfn "$NVMET/hosts/$HOSTNQN_CN1" "$NVMET/subsystems/$NQN_TO_CN1/allowed_hosts/$HOSTNQN_CN1"
nvmet_link_subsys 1 "$NQN_TO_CN1"

log "dn0 ready:"
log "  ns-to-cn0 grp=$(sudo cat $NVMET/subsystems/$NQN_TO_CN0/namespaces/1/ana_grpid) dev=$(sudo cat $NVMET/subsystems/$NQN_TO_CN0/namespaces/1/device_path)"
log "  ns-to-cn1 grp=$(sudo cat $NVMET/subsystems/$NQN_TO_CN1/namespaces/1/ana_grpid) dev=$(sudo cat $NVMET/subsystems/$NQN_TO_CN1/namespaces/1/device_path)"
log "  grp1=$(sudo cat $NVMET/ports/1/ana_groups/1/ana_state) grp2=$(sudo cat $NVMET/ports/1/ana_groups/2/ana_state)"
