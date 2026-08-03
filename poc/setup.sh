#!/bin/bash
#
# setup.sh -- build the 6-node distributed-nvme failover test environment (t04).
#
#   dn0/dn1 : create_pd (loop file) -> create_vd x4 per dn (leg0/1 x grp0/1,
#             each for cn0 and cn1).  Each virtual disk is exported TWICE, once
#             per CN, through its own NQN:
#                 ...-vd<N>-cn0  ->  dm-linear on  -real        (works)
#                 ...-vd<N>-cn1  ->  dm-linear on  -delay-cn1   (stalls 3600s)
#             so only cn0 can actually reach the data.
#   cn0     : active.  create_da_active (calls create_grp x4 -> create_leg x2)
#             -> create_exp_active (raid0 of snap0 thin-devs + nvmet export,
#             ANA "optimized").
#   cn1     : standby.  disarm_vd_delay -> create_da_standby (connect_vd x4,
#             no stack) -> arm_vd_delay -> create_exp_standby (error/delay/exp
#             stub, ANA "inaccessible").  Same NQN and namespace identity as
#             cn0 so host0 aggregates both.
#   ref0    : referral server.  An nvmet discovery service that exports no
#             storage of its own; its port carries one referral per compute
#             node, so a host that talks to ref0 is pointed at cn0 and cn1.
#   host0   : runs nvme-stas.  stafd is configured with exactly one discovery
#             controller (ref0), follows ref0's referrals to cn0/cn1, and stacd
#             connects the I/O controllers it learns about -- no "nvme connect".
#             Verifies a single multipath device and does I/O.
#
# Usage: ./setup.sh [all|dn0|dn1|cn0|cn1|ref0|host0|verify]
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

NLEGS=2   # number of legs per disk array

# ===================== data node helpers (dn-side pd/vd) =====================
setup_dn() {
    local dn="$1" ip="$2"
    _info "--- setting up DN $dn ($ip) ---"
    create_pd "$dn" "$ip"
    # Create vds for BOTH cns (symmetric fault-injection stack).
    # vd_id encodes which dn it comes from: vd0 -> dn0, vd1 -> dn1.
    create_vd "$dn" "$ip" cn0 "$CN0_IP" "${dn#dn}"
    create_vd "$dn" "$ip" cn1 "$CN1_IP" "${dn#dn}"
}

# ===================== compute node 0 (active) ===============================
setup_cn0() {
    _info "--- setting up CN0 ($CN0_IP) ---"
    { _emit_vars; echo "CN='cn0'; CN_IP='$CN0_IP'; MY_HOSTNQN='$HOSTNQN_CN0'; MY_HOSTID='$HOSTID_CN0'"; _emit_common; cat <<'EOF_CN0'
set -uo pipefail
prep_node
set_host_identity "$MY_HOSTNQN" "$MY_HOSTID"
EOF_CN0
    } | _ssh "${SSH_USER}@${CN0_IP}" bash -s

    create_da_active cn0 "$CN0_IP" da0 "$NLEGS" "$HOSTNQN_CN0" "$HOSTID_CN0"
    create_exp_active cn0 "$CN0_IP" da0 0 "$HOSTNQN_HOST0" "$HOSTID_HOST0" 1 255
}

# ===================== compute node 1 (standby) ==============================
setup_cn1() {
    _info "--- setting up CN1 ($CN1_IP) ---"
    { _emit_vars; echo "CN='cn1'; CN_IP='$CN1_IP'; MY_HOSTNQN='$HOSTNQN_CN1'; MY_HOSTID='$HOSTID_CN1'"; _emit_common; cat <<'EOF_CN1'
set -uo pipefail
prep_node
set_host_identity "$MY_HOSTNQN" "$MY_HOSTID"
EOF_CN1
    } | _ssh "${SSH_USER}@${CN1_IP}" bash -s

    # cn1's namespaces must not be backed by a live 3600s delay while the
    # kernel scans them -- see disarm_vd_delay for the full explanation.
    _info "--- disarming DN -delay devices for cn1's namespace scan ---"
    disarm_vd_delay dn0 "$DN0_IP"
    disarm_vd_delay dn1 "$DN1_IP"

    create_da_standby cn1 "$CN1_IP" da0 "$NLEGS" "$HOSTNQN_CN1" "$HOSTID_CN1"

    _info "--- re-arming DN -delay devices ---"
    arm_vd_delay dn0 "$DN0_IP"
    arm_vd_delay dn1 "$DN1_IP"

    create_exp_standby cn1 "$CN1_IP" da0 0 "$HOSTNQN_HOST0" "$HOSTID_HOST0" 256 511
}

# ===================== referral server ======================================
setup_ref0() {
    _info "--- setting up REF0 ($REF0_IP) ---"
    { _emit_vars; _emit_common; cat <<'EOF_REF0'
set -uo pipefail
prep_node

# The discovery service host0 is pointed at.
nvmet_port 1 "$REF0_IP"

# The anchor subsystem.
#
# nvmet only creates the port's TCP listener when the first subsystem is linked
# to it; a port that carries nothing but referrals never listens and the host
# gets ECONNREFUSED.  So the port needs one linked subsystem to come up.
#
# This one has no namespace and allows no host, so it is filtered out of every
# host's discovery log -- host0 sees ref0's own discovery entry plus the two
# referrals and nothing else.
sudo mkdir -p "$NVMET/subsystems/$NQN_REF_ANCHOR" || _fail "mkdir $NQN_REF_ANCHOR"
cfg_set "$NVMET/subsystems/$NQN_REF_ANCHOR/attr_allow_any_host" "0"
nvmet_link "$NQN_REF_ANCHOR" 1

# One referral per compute node.  Both CNs export the volume under the same
# NQN, so host0 ends up with two paths to one namespace.
nvmet_referral 1 cn0 "$CN0_IP"
nvmet_referral 1 cn1 "$CN1_IP"

if sudo ss -ltn 2>/dev/null | grep -q "$REF0_IP:$NVME_PORT"; then
    _ok "discovery service listening on $REF0_IP:$NVME_PORT"
else
    _warn "nothing listening on $REF0_IP:$NVME_PORT"
fi
_info "referrals: $(ls "$NVMET/ports/1/referrals" 2>/dev/null | tr '\n' ' ')"
slow_summary
_info "REF0 setup complete"
EOF_REF0
    } | _ssh "${SSH_USER}@${REF0_IP}" bash -s
}

# ===================== host =================================================
setup_host0() {
    _info "--- setting up HOST0 ($HOST0_IP) ---"
    { _emit_vars; _emit_common; cat <<'EOF_HOST0'
set -uo pipefail
prep_node
set_host_identity "$HOSTNQN_HOST0" "$HOSTID_HOST0"

# Nothing here connects to a compute node by hand.  stafd is given exactly one
# discovery controller -- ref0 -- and learns cn0 and cn1 from the referrals
# ref0 publishes; stacd then connects every I/O controller that shows up in
# the resulting discovery log page entries.  Because both CNs export the same
# NQN with the same namespace identity, the nvme driver folds the two
# connections into a single multipath namespace, exactly as the two "nvme
# connect" commands this replaces used to.
echo "--- discovery log from ref0 (this is all host0 is told) ---"
_t "nvme discover @ $REF0_IP" sudo nvme discover -t tcp -a "$REF0_IP" -s "$NVME_PORT" 2>&1 \
    | sed 's/^/    /' || true

stas_install
stas_write_config
stas_start

DEV=$(nvme_wait_dev "$NQN_EXP" 900) || _fail "no block device for $NQN_EXP (stacd did not connect it)"
_info "multipath device: $DEV"

# Both referrals are published at once, so the second path may land a moment
# after the first; give it a few seconds before judging the result.
npaths=0
for (( i = 0; i < 300; i++ )); do
    npaths=$(nvme_path_count "$NQN_EXP")
    [ "$npaths" -ge 2 ] && break
    sleep 0.1
done

echo "--- stafd (discovery controllers stafd is talking to) ---"
sudo stafctl ls 2>/dev/null || true
echo "--- stacd (I/O controllers stacd connected) ---"
sudo stacctl ls 2>/dev/null || true
echo "--- nvme list-subsys ---"
sudo nvme list-subsys 2>/dev/null || true

# Every connection on this host must have come from stacd, i.e. from ref0's
# referrals -- if any of them is missing the referral chain did not do its job.
nstas=$(sudo stacctl ls 2>/dev/null | grep -o "'subsysnqn': '$NQN_EXP'" | wc -l)
if [ "$nstas" -eq 2 ]; then
    _ok "both I/O controllers were connected by stacd via ref0's referrals"
else
    _warn "stacd reports $nstas I/O controller(s), expected 2"; VERIFY_FAIL=$((VERIFY_FAIL+1))
fi

nsubsys=$(sudo nvme list-subsys 2>/dev/null | grep -c "NQN=$NQN_EXP" || true)
_info "subsystems advertising the volume NQN: $nsubsys (want 1), controllers/paths: $npaths (want 2)"
if [ "$nsubsys" -eq 1 ] && [ "$npaths" -eq 2 ]; then
    _ok "both paths aggregated into a single multipath subsystem"
else
    _warn "expected 1 subsystem with 2 paths, got $nsubsys / $npaths"
    VERIFY_FAIL=$((VERIFY_FAIL+1))
fi

# Per-path ANA state. In multipath mode each path is its own hidden block
# device (nvme<subsys>c<ctrl>n<nsid>) carrying the ana_state attribute.
echo "--- ANA / path states ---"
n_opt=0; n_inacc=0
for pd in /sys/block/nvme*c*n*; do
    [ -r "$pd/ana_state" ] || continue
    ctrl=$(basename "$(readlink -f "$pd/device")" 2>/dev/null)
    [ -r "/sys/class/nvme/$ctrl/subsysnqn" ] || continue
    [ "$(cat "/sys/class/nvme/$ctrl/subsysnqn")" = "$NQN_EXP" ] || continue
    st=$(cat "$pd/ana_state" 2>/dev/null)
    _info "path ${pd##*/} via $ctrl: ana_state=$st ctrl_state=$(cat "/sys/class/nvme/$ctrl/state" 2>/dev/null) $(cat "/sys/class/nvme/$ctrl/address" 2>/dev/null)"
    case "$st" in
        optimized)    n_opt=$(( n_opt + 1 )) ;;
        inaccessible) n_inacc=$(( n_inacc + 1 )) ;;
    esac
done
if [ "$n_opt" -eq 1 ] && [ "$n_inacc" -eq 1 ]; then
    _ok "ANA states as specified: 1 optimized (cn0), 1 inaccessible (cn1)"
else
    _warn "expected 1 optimized + 1 inaccessible path, got $n_opt / $n_inacc"
    VERIFY_FAIL=$((VERIFY_FAIL+1))
fi

# Prove the aggregated device is usable. Buffered I/O on purpose: this box's dd
# is uutils coreutils and its O_DIRECT flags are unreliable.
_info "size: $(sudo blockdev --getsize64 "$DEV") bytes"
_t "make pattern" sudo dd if=/dev/urandom of=/tmp/dnv-pattern.bin bs=1M count=4 status=none
_t "write to $DEV" sudo dd if=/tmp/dnv-pattern.bin of="$DEV" bs=1M count=4 conv=fsync status=none \
    || _fail "write to $DEV failed"
_ok "write succeeded"
timeout 5 sudo blockdev --flushbufs "$DEV" >/dev/null 2>&1 || true
_t "read from $DEV" sudo dd if="$DEV" of=/tmp/dnv-readback.bin bs=1M count=4 status=none \
    || _fail "read from $DEV failed"
_ok "read succeeded"
if cmp -s /tmp/dnv-pattern.bin /tmp/dnv-readback.bin; then
    _ok "read-back matches written data (end-to-end integrity verified)"
else
    _warn "read-back MISMATCH"; VERIFY_FAIL=$((VERIFY_FAIL+1))
fi
sudo rm -f /tmp/dnv-pattern.bin /tmp/dnv-readback.bin

verify_summary
slow_summary
_info "HOST0 setup complete"
EOF_HOST0
    } | _ssh "${SSH_USER}@${HOST0_IP}" bash -s
}

# ===================== verification ==========================================
verify_dn() {
    local ip="$1" dn="$2"
    local vid="${dn#dn}"   # dn0 -> 0, dn1 -> 1
    _info "--- verifying DN $dn ---"
    { _emit_vars; echo "DN='$dn'; VD='$vid'"; _emit_common; cat <<'EOF_VDN'
set -uo pipefail
for leg in 0 1; do
  for grp in 0 1; do
    base="dnv-${DN}-da0-leg${leg}-grp${grp}-vd${VD}"
    dm_exists "${base}-real" || { _warn "no ${base}-real"; VERIFY_FAIL=$((VERIFY_FAIL+1)); continue; }
    rd_ok  "/dev/mapper/${base}-real" "${base}-real (dm-linear on loop)"
    rd_ok  "/dev/mapper/${base}-cn0"  "${base}-cn0 (dm-linear on -real)"
    for cn in cn0 cn1; do
        rd_eio  "/dev/mapper/${base}-err-${cn}" "${base}-err-${cn} (dm-error)"
        rd_hang "/dev/mapper/${base}-delay-${cn}" "${base}-delay-${cn}" \
                "${base}-delay-${cn} (dm-delay ${DELAY_MS}ms)"
    done
    # -cn1 sits on -delay-cn1, so it must block too
    rd_hang "/dev/mapper/${base}-cn1" "${base}-delay-cn1" \
            "${base}-cn1 (dm-linear on -delay-cn1)"
  done
done
verify_summary
slow_summary
EOF_VDN
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

verify_cn0() {
    _info "--- verifying CN0 ---"
    { _emit_vars; echo "CN='cn0'"; _emit_common; cat <<'EOF_VCN0'
set -uo pipefail
for leg in 0 1; do
  sp="dnv-${CN}-da0-leg${leg}"
  for grp in 0 1; do
    p="${sp}-grp${grp}"
    rd_ok "/dev/mapper/${p}-raid1-meta-side0" "${p}-raid1-meta-side0"
    rd_ok "/dev/mapper/${p}-raid1-data-side0" "${p}-raid1-data-side0"
    rd_ok "/dev/mapper/${p}-raid1-meta-side1" "${p}-raid1-meta-side1"
    rd_ok "/dev/mapper/${p}-raid1-data-side1" "${p}-raid1-data-side1"
    rd_ok "/dev/mapper/${p}-raid1"            "${p}-raid1"
    _info "raid1 status: $(sudo dmsetup status "${p}-raid1")"
    rd_ok "/dev/mapper/${p}-thinmeta"         "${p}-thinmeta"
    rd_ok "/dev/mapper/${p}-thindata"         "${p}-thindata"
  done
  rd_ok "/dev/mapper/${sp}-thinmeta" "${sp}-thinmeta (concat)"
  rd_ok "/dev/mapper/${sp}-thindata" "${sp}-thindata (concat)"
  _info "thin-pool status: $(sudo dmsetup status "${sp}-thinpool")"
  rd_ok "/dev/mapper/${sp}-snap0"   "${sp}-snap0"
done
vp="dnv-${CN}-da0-snap0-exp0"
rd_ok  "/dev/mapper/${vp}-real"  "${vp}-real (dm-raid0)"
_info "raid0 status: $(sudo dmsetup status "${vp}-real")"
rd_eio "/dev/mapper/${vp}-error" "${vp}-error (dm-error)"
rd_hang "/dev/mapper/${vp}-delay" "${vp}-delay" "${vp}-delay (dm-delay ${DELAY_MS}ms)"
rd_ok  "/dev/mapper/${vp}"       "${vp} (exported volume)"
verify_summary
slow_summary
EOF_VCN0
    } | _ssh "${SSH_USER}@${CN0_IP}" bash -s
}

verify_cn1() {
    _info "--- verifying CN1 ---"
    { _emit_vars; echo "CN='cn1'"; _emit_common; cat <<'EOF_VCN1'
set -uo pipefail
vp="dnv-${CN}-da0-snap0-exp0"
rd_eio  "/dev/mapper/${vp}-error" "${vp}-error (dm-error)"
rd_hang "/dev/mapper/${vp}-delay" "${vp}-delay" "${vp}-delay (dm-delay ${DELAY_MS}ms)"
# The exported standby volume sits on the delay device, so it must block too.
rd_hang "/dev/mapper/${vp}" "${vp}-delay" "${vp} (exported standby volume, via delay)"

# The eight virtual disks imported from the DNs are parked on the DN-side
# delay devices, so reading any of them must block as well. The release lever
# lives on the DN, so the readers are started here and left running; the caller
# frees them with a disarm/arm cycle on the DN delay devices right afterwards.
PIDS=""
for leg in 0 1; do
  for grp in 0 1; do
    for d in 0 1; do
      nqn="${NQN_PREFIX}:dn:dn${d}:da0-leg${leg}-grp${grp}-vd${d}-${CN}"
      dev=$(nvme_dev_by_nqn "$nqn") || { _warn "no device for $nqn"; continue; }
      sudo dd if="$dev" of=/dev/null bs=4096 count=1 status=none >/dev/null 2>&1 &
      PIDS="$PIDS $!:$nqn"
    done
  done
done
sleep 1.5
for e in $PIDS; do
    pid=${e%%:*}; nqn=${e#*:}
    if kill -0 "$pid" 2>/dev/null; then
        _ok "read blocks   : $nqn"
    else
        _warn "read did NOT block (expected hang): $nqn"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
done
_info "leaving ${PIDS:+$(echo $PIDS | wc -w)} blocked readers for the DN disarm/arm cycle to release"
verify_summary
slow_summary
EOF_VCN1
    } | _ssh "${SSH_USER}@${CN1_IP}" bash -s
}

# The referral chain, checked from the only place that can see all of it: the
# host.  ref0 must hand out one referral per CN, and every connection host0 has
# must be one stacd made from those referrals.
verify_ref0_host0() {
    _info "--- verifying REF0 referrals and host0's nvme-stas connections ---"
    { _emit_vars; _emit_common; cat <<'EOF_VREF'
set -uo pipefail
LOG=/tmp/dnv-discover.txt
sudo nvme discover -t tcp -a "$REF0_IP" -s "$NVME_PORT" > "$LOG" 2>&1 || \
    { _warn "cannot reach the discovery service on $REF0_IP"; VERIFY_FAIL=$((VERIFY_FAIL+1)); }
sed 's/^/    /' "$LOG"

nref=$(grep -c 'subtype: *discovery subsystem referral' "$LOG" || true)
if [ "$nref" -eq 2 ]; then
    _ok "ref0 publishes 2 referrals"
else
    _warn "ref0 publishes $nref referral(s), expected 2"; VERIFY_FAIL=$((VERIFY_FAIL+1))
fi
for ip in "$CN0_IP" "$CN1_IP"; do
    if grep -q "traddr: *$ip\$" "$LOG"; then
        _ok "referral to $ip present"
    else
        _warn "no referral to $ip"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
done
if grep -q "subnqn: *$NQN_REF_ANCHOR" "$LOG"; then
    _warn "the anchor subsystem is visible to host0 (it should allow no host)"
    VERIFY_FAIL=$((VERIFY_FAIL+1))
else
    _ok "ref0's anchor subsystem is not advertised to host0"
fi
sudo rm -f "$LOG"

for u in stafd stacd; do
    if sudo systemctl is-active --quiet "$u"; then _ok "$u is running"
    else _warn "$u is not running"; VERIFY_FAIL=$((VERIFY_FAIL+1)); fi
done
echo "--- stafd ls ---"; sudo stafctl ls 2>/dev/null || true
echo "--- stacd ls ---"; sudo stacctl ls 2>/dev/null || true

ndc=$(sudo stafctl ls 2>/dev/null | grep -o "'traddr': '$REF0_IP'" | wc -l)
[ "$ndc" -ge 1 ] && _ok "stafd is connected to the referral server" || \
    { _warn "stafd is not connected to $REF0_IP"; VERIFY_FAIL=$((VERIFY_FAIL+1)); }

nioc=$(sudo stacctl ls 2>/dev/null | grep -o "'subsysnqn': '$NQN_EXP'" | wc -l)
npaths=$(nvme_path_count "$NQN_EXP")
if [ "$nioc" -eq 2 ] && [ "$npaths" -eq 2 ]; then
    _ok "both volume paths are stacd connections (no manual nvme connect)"
else
    _warn "stacd I/O controllers=$nioc, kernel paths=$npaths, expected 2/2"
    VERIFY_FAIL=$((VERIFY_FAIL+1))
fi
DEV=$(nvme_dev_by_nqn "$NQN_EXP") && rd_ok "$DEV" "$DEV (multipath volume)" || \
    { _warn "no block device for $NQN_EXP"; VERIFY_FAIL=$((VERIFY_FAIL+1)); }
verify_summary
slow_summary
EOF_VREF
    } | _ssh "${SSH_USER}@${HOST0_IP}" bash -s
}

# ===================== main ==================================================
main() {
    local action="${1:-all}"
    case "$action" in
        all)
            setup_dn dn0 "$DN0_IP"
            setup_dn dn1 "$DN1_IP"
            setup_cn0
            setup_cn1
            setup_ref0
            setup_host0
            ;;
        dn0)    setup_dn dn0 "$DN0_IP" ;;
        dn1)    setup_dn dn1 "$DN1_IP" ;;
        cn0)    setup_cn0 ;;
        cn1)    setup_cn1 ;;
        ref0)   setup_ref0 ;;
        host0)  setup_host0 ;;
        verify)
            verify_dn "$DN0_IP" dn0
            verify_dn "$DN1_IP" dn1
            verify_cn0
            verify_cn1
            verify_ref0_host0
            # verify_cn1 deliberately leaves eight readers blocked on the DN
            # delay devices; flush them so nothing is left in D-state.
            _info "--- releasing cn1's blocked readers ---"
            disarm_vd_delay dn0 "$DN0_IP"
            disarm_vd_delay dn1 "$DN1_IP"
            arm_vd_delay dn0 "$DN0_IP"
            arm_vd_delay dn1 "$DN1_IP"
            ;;
        *)
            echo "Usage: $0 [all|dn0|dn1|cn0|cn1|ref0|host0|verify]" >&2
            exit 1
            ;;
    esac
    _info "setup.sh ($action) finished"
}

main "$@"
