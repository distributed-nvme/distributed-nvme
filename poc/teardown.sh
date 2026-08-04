#!/bin/bash
#
# teardown.sh -- remove everything setup.sh creates, on all six nodes.
#
# Safe to run at any point: every step tolerates resources that were never
# created, were already removed, or are only half built.  It also copes with the
# post-failover layout, where the dm stack lives on cn1 instead of cn0.
#
# Order matters:
#   0. Defuse every dm-delay on every node FIRST.  A bio parked in a 3600s
#      dm-delay keeps an nvmet request outstanding, and that makes
#      "echo 0 > namespaces/1/enable" -- and controller teardown, and
#      "nvme disconnect" -- block indefinitely.
#   1. Remove the targets from the referral server.  host0 runs nvme-stas, so
#      dropping ref0's referrals to cn0/cn1 propagates as a discovery log change:
#      stafd drops those discovery controllers, their log page entries
#      disappear, and stacd disconnects the I/O controllers it had connected.
#      host0 lets go of the volume on its own -- no "nvme disconnect" involved.
#   2. host0 (stop nvme-stas), then cn1/cn0, then dn1/dn0, then ref0, so nothing
#      is still holding a device open when it is removed.
#
# Usage: ./teardown.sh [all|defuse|dn0|dn1|cn0|cn1|ref0|host0]
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

# ===================== phase 0: defuse =======================================
defuse_node() {
    local ip="$1" name="$2"
    _info "--- defusing dm-delay devices on $name ($ip) ---"
    { _emit_vars; _emit_common; cat <<'EOF_DEFUSE'
dnv_defuse_all
slow_summary
EOF_DEFUSE
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

defuse_all() {
    _info "=== phase 0: releasing all parked I/O ==="
    # DNs first: host/CN I/O bottoms out there, so freeing those bios unblocks
    # everything stacked above before the CN delays are touched.
    defuse_node "$DN0_IP" dn0 || _warn "dn0 defuse reported errors"
    defuse_node "$DN1_IP" dn1 || _warn "dn1 defuse reported errors"
    defuse_node "$CN0_IP" cn0 || _warn "cn0 defuse reported errors"
    defuse_node "$CN1_IP" cn1 || _warn "cn1 defuse reported errors"
}

# ===================== phase 1: unexport from the referral server ============
ref0_unexport() {
    _info "=== phase 1: removing the nvme targets from ref0 ($REF0_IP) ==="
    { _emit_vars; _emit_common; cat <<'EOF_REFDROP'
[ -d "$NVMET/ports/1" ] || { _info "ref0 has no port 1 - nothing to unexport"; exit 0; }
nvmet_remove_referrals 1
EOF_REFDROP
    } | _ssh "${SSH_USER}@${REF0_IP}" bash -s
}

# Wait for host0's connections to disappear on their own.  Falls back to an
# explicit disconnect only if nvme-stas has not reacted in time, so teardown
# stays safe on a host where stas is not running (e.g. a half-built system).
host0_wait_disconnect() {
    _info "=== waiting for host0 to disconnect by itself (up to ${STAS_DISCONNECT_WAIT}s) ==="
    { _emit_vars; _emit_common; cat <<'EOF_WAIT'
n=$(nvme_path_count "$NQN_EXP")
if [ "$n" -eq 0 ]; then _info "host0 has no connection to $NQN_EXP"; slow_summary; exit 0; fi
_info "host0 currently has $n path(s) to $NQN_EXP"
t0=$(date +%s)
while :; do
    n=$(nvme_path_count "$NQN_EXP")
    [ "$n" -eq 0 ] && break
    el=$(( $(date +%s) - t0 ))
    [ "$el" -ge "$STAS_DISCONNECT_WAIT" ] && break
    sleep 0.5
done
el=$(( $(date +%s) - t0 ))
if [ "$n" -eq 0 ]; then
    _ok "host0 disconnected automatically after ${el}s (nvme-stas followed ref0)"
else
    _warn "still $n path(s) after ${el}s - falling back to an explicit disconnect"
    nvme_disc "$NQN_EXP"
fi
echo "--- stacd ls ---"; sudo stacctl ls 2>/dev/null || true
slow_summary
EOF_WAIT
    } | _ssh "${SSH_USER}@${HOST0_IP}" bash -s
}

# ===================== per-node teardown =====================================
teardown_host0() {
    _info "=== tearing down HOST0 ($HOST0_IP) ==="
    { _emit_vars; _emit_common; cat <<'EOF_H0'
stas_stop_restore
nvme_disc "$NQN_EXP"
sudo rm -f /tmp/dnv-pattern.bin /tmp/dnv-readback.bin /tmp/dnv-io.log \
           /tmp/dnv-discover.txt 2>/dev/null || true
cleanup_node_common
slow_summary
_info "HOST0 teardown complete"
EOF_H0
    } | _ssh "${SSH_USER}@${HOST0_IP}" bash -s
}

teardown_ref0() {
    _info "=== tearing down REF0 ($REF0_IP) ==="
    { _emit_vars; _emit_common; cat <<'EOF_REF0'
nvmet_remove_referrals 1
nvmet_remove_subsys "$NQN_REF_ANCHOR"
nvmet_remove_port 1
cleanup_node_common
slow_summary
_info "REF0 teardown complete"
EOF_REF0
    } | _ssh "${SSH_USER}@${REF0_IP}" bash -s
}

# CN teardown uses common.sh delete_* primitives (reverse recipe).
# delete_exp_* -> delete_cntlr_* -> (delete_leg/delete_grp/disconnect_vd inside)
teardown_cn() {
    local ip="$1" cn="$2"
    _info "=== tearing down $cn ($ip) ==="
    # Try active delete first (cn0's normal layout), then standby delete (cn1's
    # normal layout).  Both are idempotent and safe on partial systems.
    delete_exp_active  "$cn" "$ip" da0 0  2>/dev/null || true
    delete_exp_standby "$cn" "$ip" da0 0  2>/dev/null || true
    # delete_cntlr_active removes the grp/leg/thinpool stack; delete_cntlr_standby
    # disconnects the vds.  Run both so the right one cleans up the right
    # resources regardless of which side this cn was on.
    delete_cntlr_active  "$cn" "$ip" da0 2>/dev/null || true
    delete_cntlr_standby "$cn" "$ip" da0 2>/dev/null || true
    # Force-clean any stragglers.
    { _emit_vars; echo "CN='$cn'"; _emit_common; cat <<'EOF_CN_CLEAN'
# Remove any leftover dm devices and nvmet exports on this cn.
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_port 1
nvmet_remove_host "$HOSTNQN_HOST0"
dnv_remove_all_dm
# Disconnect any remaining vd connections.
for leg in 0 1; do
  for grp in 0 1; do
    for d in 0 1; do
      nvme_disc "${NQN_PREFIX}:dn:dn${d}:da0-leg${leg}-grp${grp}-vd${d}-${CN}"
    done
  done
done
cleanup_node_common
_info "remaining dm devices: $(dm_count)"
slow_summary
_info "${CN} teardown complete"
EOF_CN_CLEAN
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# DN teardown uses common.sh's delete_vd (for both cns) + delete_pd.
teardown_dn() {
    local ip="$1" dn="$2"
    _info "=== tearing down $dn ($ip) ==="
    # delete_vd removes the dm stack + nvmet subsystems for both cns.
    delete_vd "$dn" "$ip" cn0 "$CN0_IP" "${dn#dn}" 2>/dev/null || true
    delete_vd "$dn" "$ip" cn1 "$CN1_IP" "${dn#dn}" 2>/dev/null || true
    delete_pd "$dn" "$ip"
}

# ===================== main ==================================================
main() {
    local action="${1:-all}"
    case "$action" in
        all)
            # Defusing comes first even though the referrals go next: a bio
            # parked in a 3600s dm-delay would make host0's disconnect --
            # however it is triggered -- block indefinitely.
            defuse_all
            ref0_unexport             || _warn "ref0 unexport reported errors"
            host0_wait_disconnect     || _warn "host0 disconnect wait reported errors"
            teardown_host0            || _warn "host0 teardown reported errors"
            teardown_cn "$CN1_IP" cn1 || _warn "cn1 teardown reported errors"
            teardown_cn "$CN0_IP" cn0 || _warn "cn0 teardown reported errors"
            teardown_dn "$DN1_IP" dn1 || _warn "dn1 teardown reported errors"
            teardown_dn "$DN0_IP" dn0 || _warn "dn0 teardown reported errors"
            teardown_ref0             || _warn "ref0 teardown reported errors"
            ;;
        defuse) defuse_all ;;
        unexport) ref0_unexport; host0_wait_disconnect ;;
        host0)  ref0_unexport; host0_wait_disconnect; teardown_host0 ;;
        ref0)   teardown_ref0 ;;
        cn0)    defuse_node "$CN0_IP" cn0; teardown_cn "$CN0_IP" cn0 ;;
        cn1)    defuse_node "$CN1_IP" cn1; teardown_cn "$CN1_IP" cn1 ;;
        dn0)    defuse_node "$DN0_IP" dn0; teardown_dn "$DN0_IP" dn0 ;;
        dn1)    defuse_node "$DN1_IP" dn1; teardown_dn "$DN1_IP" dn1 ;;
        *)
            echo "Usage: $0 [all|defuse|unexport|dn0|dn1|cn0|cn1|ref0|host0]" >&2
            exit 1
            ;;
    esac
    _info "teardown.sh ($action) finished"
}

main "$@"
