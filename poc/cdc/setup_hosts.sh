#!/bin/bash
#
# setup_hosts.sh -- provision the NVMe initiator side (host0, host1) for the
# CDC test (see cdc/plan.md sections 7, 10, 12).
#
#   For each host:
#     1. set /etc/nvme/hostnqn + hostid (stable per-host constants).
#     2. install nvme-stas (stafd + stacd) if absent.
#     3. write /etc/stas/stafd.conf pointing at the CDC (cdc0:8009) with
#        persistent-connections=true (plan §7), and /etc/stas/stacd.conf with
#        an empty [Controllers] (every I/O controller comes from the discovery
#        log; none is hard-coded).
#     4. start stafd/stacd, reloading instead of restarting when the running
#        identity already matches, so a re-run keeps live connections.
#
#   Stock stas configs are preserved as *.dnv-orig on the first run so
#   teardown_hosts.sh can put them back.
#
# Usage: ./setup_hosts.sh [host0|host1]    (default: both)
#
set -uo pipefail

SSH_USER="yupeng"
CDC0_IP="192.168.122.78"
CDC0_PORT=8009
HOST0_IP="192.168.122.193"
HOST1_IP="192.168.122.197"
HOST0_NQN="nqn.2026-08.org.dnv:host:host0"
HOST1_NQN="nqn.2026-08.org.dnv:host:host1"
HOST0_ID="c0000000-0000-4000-8000-000000000000"
HOST1_ID="d0000000-0000-4000-8000-000000000000"
declare -A HOST_IP=( [host0]=$HOST0_IP [host1]=$HOST1_IP )
declare -A HOST_NQN=( [host0]=$HOST0_NQN [host1]=$HOST1_NQN )
declare -A HOST_ID=( [host0]=$HOST0_ID [host1]=$HOST1_ID )
STAS_DIR=/etc/stas

# ===================== local helpers =========================================
_ssh() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
              -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

# Variable block every remote script starts with (values substituted locally).
_emit_vars() {
    cat <<EOF
STAS_DIR='$STAS_DIR'
CDC_IP='$CDC0_IP'
CDC_PORT='$CDC0_PORT'
EOF
}

# Remote helpers (run ON the host; quoted heredoc -- nothing expanded locally).
_emit_common() {
    cat <<'EOF_COMMON'
SLOW_LIMIT_MS=3000
SLOW_LIST=/tmp/dnv-slow.list
: > "$SLOW_LIST"

_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_ok()   { echo "[ OK ] $*"; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

_t() {
    local desc="$1"; shift
    local t0 t1 ms rc=0
    t0=$(date +%s%N)
    "$@" || rc=$?
    t1=$(date +%s%N)
    ms=$(( (t1 - t0) / 1000000 ))
    if [ "$ms" -gt "$SLOW_LIMIT_MS" ]; then
        echo "[SLOW] ${ms}ms  $desc" >&2
        echo "${ms}ms  $desc" >> "$SLOW_LIST"
    fi
    return $rc
}

slow_summary() {
    if [ -s "$SLOW_LIST" ]; then
        _warn "commands exceeding ${SLOW_LIMIT_MS}ms:"
        cat "$SLOW_LIST" >&2
    else
        _info "timing: no command exceeded ${SLOW_LIMIT_MS}ms"
    fi
}

set_host_identity() {   # <hostnqn> <hostid>
    sudo mkdir -p /etc/nvme
    echo "$1" | sudo tee /etc/nvme/hostnqn >/dev/null
    echo "$2" | sudo tee /etc/nvme/hostid  >/dev/null
}

stas_install() {
    if dpkg -s nvme-stas >/dev/null 2>&1; then
        _info "nvme-stas already installed: $(dpkg-query -W -f='${Version}' nvme-stas 2>/dev/null)"
        return 0
    fi
    _info "installing nvme-stas"
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nvme-stas >/dev/null 2>&1 \
        || _fail "could not install nvme-stas"
    _info "installed nvme-stas: $(dpkg-query -W -f='${Version}' nvme-stas 2>/dev/null)"
}

# Point stafd at the CDC and nothing else. persistent-connections=true keeps the
# discovery connection to cdc0 open so the host receives Discovery Log Change
# AENs (plan §7). stacd has an empty [Controllers]: every I/O controller comes
# from the discovery log page the CDC serves, so dropping an entry from the
# watched file makes stacd disconnect that path again. The stock config files
# are kept as *.dnv-orig on the first run so teardown_hosts.sh can restore them.
stas_write_config() {
    local f
    sudo mkdir -p "$STAS_DIR"
    for f in stafd.conf stacd.conf; do
        [ -f "$STAS_DIR/$f" ] && [ ! -f "$STAS_DIR/$f.dnv-orig" ] && \
            sudo cp -a "$STAS_DIR/$f" "$STAS_DIR/$f.dnv-orig"
    done

    sudo tee "$STAS_DIR/stafd.conf" >/dev/null <<STAFD_EOF
# distributed-nvme CDC test -- written by setup_hosts.sh.
# cdc0 is the only discovery controller (Centralized DC, TP 8013). I/O
# subsystems arrive from its per-host-filtered discovery log page.
[Global]
tron=false
ip-family=ipv4
ignore-iface=true

[Service Discovery]
zeroconf=disabled

[Discovery controller connection management]
persistent-connections=true

[Controllers]
controller=transport=tcp;traddr=${CDC_IP};trsvcid=${CDC_PORT}
STAFD_EOF

    sudo tee "$STAS_DIR/stacd.conf" >/dev/null <<STACD_EOF
# distributed-nvme CDC test -- written by setup_hosts.sh.
# Every I/O controller comes from a discovery log page entry served by cdc0;
# none is hard-coded, so removing a target from cdc0's watched file makes stacd
# disconnect it again.
[Global]
tron=false
ip-family=ipv4
ignore-iface=true

[I/O controller connection management]
disconnect-scope=only-stas-connections
disconnect-trtypes=tcp

[Controllers]
STACD_EOF
    _info "wrote $STAS_DIR/stafd.conf (discovery controller: ${CDC_IP}:${CDC_PORT}) and $STAS_DIR/stacd.conf"
}

# stafd/stacd read the host NQN/ID through sys.conf's file:// references at
# startup, so they have to be (re)started once set_host_identity has changed
# them.  On a re-run the identity is already right and a restart would drop and
# rebuild every connection -- including the live volume paths -- so the config
# is reloaded (SIGHUP) instead.
stas_start() {
    local want_nqn cur_nqn
    want_nqn=$(cat /etc/nvme/hostnqn 2>/dev/null)
    if [ -n "$want_nqn" ] && sudo systemctl is-active --quiet stafd \
                          && sudo systemctl is-active --quiet stacd; then
        cur_nqn=$(sudo stafctl status 2>/dev/null | sed -n "s/.*'hostnqn': '\([^']*\)'.*/\1/p" | head -1)
        if [ "$cur_nqn" = "$want_nqn" ]; then
            _t "reload stafd/stacd" sudo systemctl reload stafd stacd >/dev/null 2>&1 || true
            _info "stafd/stacd already running as $cur_nqn - config reloaded, connections kept"
            return 0
        fi
    fi
    _t "restart stafd/stacd" sudo systemctl restart stafd stacd \
        || _fail "could not start stafd/stacd"
    sudo systemctl is-active --quiet stafd || _fail "stafd is not running"
    sudo systemctl is-active --quiet stacd || _fail "stacd is not running"
    _info "stafd/stacd running with hostnqn $want_nqn"
}

stas_stop_restore() {
    local f
    if ! dpkg -s nvme-stas >/dev/null 2>&1; then return 0; fi
    _t "stop stafd/stacd" sudo systemctl stop stacd stafd >/dev/null 2>&1 || true
    for f in stafd.conf stacd.conf; do
        if [ -f "/etc/stas/$f.dnv-orig" ]; then
            sudo mv -f "/etc/stas/$f.dnv-orig" "/etc/stas/$f" && _info "restored /etc/stas/$f"
        fi
    done
    _info "nvme-stas stopped (stafd: $(systemctl is-active stafd 2>/dev/null), stacd: $(systemctl is-active stacd 2>/dev/null))"
}
EOF_COMMON
}

# ===================== per-host setup =========================================
setup_host() {   # <name>
    local name="$1"
    local ip="${HOST_IP[$name]:-}"
    local nqn="${HOST_NQN[$name]:-}"
    local id="${HOST_ID[$name]:-}"
    [ -n "$ip" ]  || _fail "unknown host: $name"
    _info "=== setting up $name ($ip) ==="
    {
        _emit_vars
        echo "WANT_NQN='$nqn'; WANT_ID='$id'"
        _emit_common
        cat <<'EOF_HOST'
set -uo pipefail
set_host_identity "$WANT_NQN" "$WANT_ID"
stas_install
stas_write_config
stas_start
slow_summary
_info "$WANT_NQN host setup complete"
EOF_HOST
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# ===================== main ==================================================
TARGETS=(host0 host1)
if [ $# -ge 1 ]; then
    case "$1" in
        host0|host1) TARGETS=("$1") ;;
        *) _fail "usage: $0 [host0|host1]" ;;
    esac
fi

for h in "${TARGETS[@]}"; do
    setup_host "$h" || _warn "$h setup reported errors"
done

_info "setup_hosts complete: ${TARGETS[*]}"
exit 0