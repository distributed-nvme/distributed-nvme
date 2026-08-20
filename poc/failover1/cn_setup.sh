#!/bin/bash
# cn_setup.sh <cn0|cn1> -- connect the CN to its dn0 subsystem, stack a dm
# linear on top, and re-export it under the SHARED path NQN so host0 can
# aggregate cn0 + cn1 into one multipath namespace.
. "$(dirname "$0")/common.sh"
set -e
ROLE=${1:?usage: cn_setup.sh <cn0|cn1>}

case "$ROLE" in
  cn0) MY_IP=$CN0_IP; MY_HOSTNQN=$HOSTNQN_CN0; UP_NQN=$NQN_TO_CN0
       CNTLID_MIN=1;   CNTLID_MAX=255; NS_GRPID=1 ;;   # optimized-cn0
  cn1) MY_IP=$CN1_IP; MY_HOSTNQN=$HOSTNQN_CN1; UP_NQN=$NQN_TO_CN1
       CNTLID_MIN=256; CNTLID_MAX=511; NS_GRPID=2 ;;   # inaccessible-cn1
  *) die "role must be cn0 or cn1" ;;
esac
D_ERR=$D_ERR_ON-$ROLE
D_LIN=$D_LIN_ON-$ROLE
S=$DEV_SECTORS

sudo modprobe nvme-tcp nvmet nvmet-tcp 2>/dev/null || true
install_udev_rule

# ---- 1. connect upstream to dn0 -------------------------------------------
sudo mkdir -p /etc/nvme
printf '%s\n' "$MY_HOSTNQN" | sudo tee /etc/nvme/hostnqn >/dev/null

CTRL=$(ctrl_for_nqn "$UP_NQN" || true)
if [ -z "$CTRL" ]; then
  sudo nvme connect -t tcp -a "$DN0_IP" -s "$PORT_TCP" -n "$UP_NQN" \
       --hostnqn "$MY_HOSTNQN" >/dev/null
  for i in $(seq 1 50); do
    CTRL=$(ctrl_for_nqn "$UP_NQN" || true)
    if [ -n "$CTRL" ]; then break; fi
    sleep 0.1
  done
fi
[ -n "$CTRL" ] || die "no controller for $UP_NQN"

# The namespace block device: with nvme_core.multipath=Y the usable head is
# nvmeXnY.  A non-optimized path still yields a head device; an inaccessible
# one would not (that is why the cn1 upstream is non-optimized, not inaccessible).
UPDEV=""
for i in $(seq 1 100); do
  h=$(nvme_head_for "$UP_NQN" || true)
  if [ -n "$h" ] && [ -b "/dev/$h" ]; then UPDEV=/dev/$h; break; fi
  sleep 0.1
done
[ -n "$UPDEV" ] || die "no block device for $UP_NQN (ctrl=$CTRL)"
log "$ROLE upstream: ctrl=$CTRL dev=$UPDEV ana=$(local_ana_state "$CTRL") size=$(sudo blockdev --getsz "$UPDEV")"
[ "$(sudo blockdev --getsz "$UPDEV")" = "$S" ] || die "upstream size mismatch"

# ---- 2. local dm stack -----------------------------------------------------
dm_create "$D_ERR" "0 $S error"
if [ "$ROLE" = cn0 ]; then
  # cn0 is the active path: linear straight onto the nvme namespace
  dm_create "$D_LIN" "0 $S linear $UPDEV 0"
else
  # cn1 is the standby: its lane is dead until failover, so park the linear on
  # the error device.  cn1_failover.sh reloads it onto the nvme namespace.
  dm_create "$D_LIN" "0 $S linear /dev/mapper/$D_ERR 0"
fi
sudo dmsetup ls --tree | sed 's/^/  /' >&2

# ---- 3. re-export under the shared NQN ------------------------------------
nvmet_port_create 1 "$MY_IP"
nvmet_ana_set 1 1 optimized       # optimized-$ROLE
nvmet_ana_set 1 2 inaccessible    # inaccessible-$ROLE

sudo mkdir -p "$NVMET/hosts/$HOSTNQN_HOST0"
sudo mkdir -p "$NVMET/subsystems/$NQN_PATH"
P=$NVMET/subsystems/$NQN_PATH
nvmet_write "$P/attr_allow_any_host" 0
# identical model/serial on both CNs: the host uses them to decide the two
# controllers really are one subsystem
nvmet_write "$P/attr_serial" "$SUBSYS_SERIAL"
nvmet_write "$P/attr_model"  "$SUBSYS_MODEL"
# disjoint controller-id ranges, else the two CNs hand out colliding cntlids
nvmet_write "$P/attr_cntlid_min" "$CNTLID_MIN"
nvmet_write "$P/attr_cntlid_max" "$CNTLID_MAX"

N=$P/namespaces/1
sudo mkdir -p "$N"
if [ "$(cat "$N/enable")" = "1" ]; then
  echo 0 | sudo tee "$N/enable" >/dev/null
fi
nvmet_write "$N/device_path"  "/dev/mapper/$D_LIN"
nvmet_write "$N/device_uuid"  "$NS_UUID"
nvmet_write "$N/device_nguid" "$NS_NGUID"
nvmet_write "$N/ana_grpid"    "$NS_GRPID"
nvmet_write "$N/enable" 1

sudo ln -sfn "$NVMET/hosts/$HOSTNQN_HOST0" "$P/allowed_hosts/$HOSTNQN_HOST0"
nvmet_link_subsys 1 "$NQN_PATH"

log "$ROLE ready: ns-path grpid=$(sudo cat $N/ana_grpid) state=$(sudo cat $NVMET/ports/1/ana_groups/$(sudo cat $N/ana_grpid)/ana_state) backing=/dev/mapper/$D_LIN"
