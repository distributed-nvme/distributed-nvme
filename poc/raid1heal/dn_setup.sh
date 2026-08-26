#!/bin/bash
# dn_setup.sh — target-side rig for the RAID1 self-healing test.
#
#   1G file -> loop -> dm-linear (dnv-leg) -> nvmet subsystem over TCP
#
# The dm-linear device is the fault-injection point: reloading its table with
# error/delay/flakey targets is how every scenario in step 4 is produced.
#
# Usage: dn_setup.sh <node-name>        e.g. dn_setup.sh dn0
set -eu

NODE=${1:?usage: dn_setup.sh <node-name>}
BASE=/var/tmp/dnv
IMG=$BASE/$NODE.img
SIZE_MB=1024
LEG=dnv-leg
NQN=nqn.2026-08.org.dnv:$NODE
NVMET=/sys/kernel/config/nvmet
PORT=1
TRSVCID=4420
# Fixed per-node namespace uuid => deterministic /dev/disk/by-id path on cn0.
case $NODE in
  dn0) NSUUID=11111111-1111-1111-1111-000000000000 ;;
  dn1) NSUUID=22222222-2222-2222-2222-000000000000 ;;
  *)   echo "unknown node $NODE" >&2; exit 2 ;;
esac
IP=$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)

say() { printf '[%s] %s\n' "$NODE" "$*"; }

# --- 0. udev rule: keep blkid/bcache probes off our dm devices ---------------
# Without this a dm-delay table (step 4.4) parks systemd-udevd workers in D
# state; they hold the device open and teardown then fails with EBUSY.
sudo tee /etc/udev/rules.d/58-dnv-test.rules >/dev/null <<'EOF'
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
EOF
sudo udevadm control --reload-rules

sudo modprobe loop
sudo modprobe dm-mod
sudo modprobe dm-delay
sudo modprobe dm-flakey
sudo modprobe nvmet
sudo modprobe nvmet-tcp
sudo mountpoint -q /sys/kernel/config || sudo mount -t configfs none /sys/kernel/config

# --- 1.1.1 backing file -----------------------------------------------------
sudo mkdir -p $BASE
[ -f "$IMG" ] || sudo truncate -s ${SIZE_MB}M "$IMG"
say "image $IMG ($(sudo stat -c %s "$IMG") bytes)"

# --- 1.1.2 loop device ------------------------------------------------------
LOOP=$(losetup -j "$IMG" | cut -d: -f1)
if [ -z "$LOOP" ]; then LOOP=$(sudo losetup -f --show "$IMG"); fi
SECTORS=$(sudo blockdev --getsz "$LOOP")
say "loop $LOOP ($SECTORS sectors)"

# --- 1.1.3 dm-linear on top of the loop -------------------------------------
if ! sudo dmsetup info "$LEG" >/dev/null 2>&1; then
    echo "0 $SECTORS linear $LOOP 0" | sudo dmsetup create "$LEG"
fi
say "dm  /dev/mapper/$LEG -> $(sudo dmsetup table $LEG)"

# --- 1.1.4 nvmet subsystem --------------------------------------------------
S=$NVMET/subsystems/$NQN
sudo mkdir -p "$S"
# attr_allow_any_host stays 0: cn0 is whitelisted explicitly below.
echo 0 | sudo tee "$S/attr_allow_any_host" >/dev/null
setattr() {   # write only when the trimmed value really differs (EBUSY once live)
    local f=$1 v=$2 cur
    cur=$(sudo cat "$f" | head -1 | sed -e 's/[[:space:]]*$//')
    [ "$cur" = "$(printf '%s' "$v" | sed -e 's/[[:space:]]*$//')" ] && return 0
    printf '%s' "$v" | sudo tee "$f" >/dev/null
}
setattr "$S/attr_model"  dnv-ld
setattr "$S/attr_serial" "DNV${NODE}"

sudo mkdir -p "$S/namespaces/1"
setattr "$S/namespaces/1/device_path" /dev/mapper/$LEG
setattr "$S/namespaces/1/device_uuid" "$NSUUID"
echo 1 | sudo tee "$S/namespaces/1/enable" >/dev/null

# allowed_hosts: cn0 only
HOSTNQN=nqn.2026-08.org.dnv:host:cn0
sudo mkdir -p "$NVMET/hosts/$HOSTNQN"
[ -e "$S/allowed_hosts/$HOSTNQN" ] || \
    sudo ln -s "$NVMET/hosts/$HOSTNQN" "$S/allowed_hosts/$HOSTNQN"

P=$NVMET/ports/$PORT
sudo mkdir -p "$P"
setattr "$P/addr_adrfam" ipv4
setattr "$P/addr_trtype" tcp
setattr "$P/addr_traddr" "$IP"
setattr "$P/addr_trsvcid" "$TRSVCID"
# Re-linking a *live* port->subsystem symlink bounces every controller, so only
# create it when it is genuinely absent.
{ [ -L "$P/subsystems/$NQN" ] || [ -e "$P/subsystems/$NQN" ]; } || \
    sudo ln -s "$S" "$P/subsystems/$NQN"

say "nvmet $NQN on $IP:$TRSVCID ns1 uuid=$NSUUID"
say "READY"
