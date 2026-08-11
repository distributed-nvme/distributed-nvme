#!/bin/bash
#
# failover.sh -- hand the exported volume over from cn0 to cn1 without the
# host losing data.
#
# Sequence (matches the test spec):
#   1. cn1  : suspend its exported volume, then advertise ANA "optimized".
#             Host I/O now lands on cn1 and queues inside the suspended dm
#             device instead of erroring out -- this is what makes it graceful.
#   2. ref0 : remove the cn0 referral from the referral server.  host0 runs
#             nvme-stas, so the discovery log change propagates: stafd drops
#             cn0's discovery controller, its log page entry disappears, and
#             stacd disconnects host0 from cn0.  The host lets go of the path
#             being retired before anything on cn0 is taken apart.
#   3. cn0  : advertise ANA "inaccessible", suspend its exported volume (a
#             flushing suspend, so everything in flight lands on disk), then
#             delete the whole raid1/thin/raid0 stack in reverse creation order.
#             Removing the thin-pool commits its metadata, which is what lets
#             cn1 pick the pool up.
#   4./5. dn0/dn1 : swap the per-CN linear devices over -- cn0's now point at
#             the 3600s delay, cn1's at the real data.
#   6. cn1  : rebuild the same stack via create_cntlr_active, repoint its exported
#             volume at the fresh raid0 and resume it.  Queued host I/O drains
#             through.
#   7. cn0  : repoint its exported volume at its delay device.
#
# Usage: ./failover.sh [force]
#
#   force   skip steps 3 and 7 -- i.e. do not touch cn0 at all.  This models cn0
#           having died outright: it never lowers its ANA state, never does the
#             flushing suspend and never dismantles its stack, so the thin-pool
#             metadata is *not* committed on the way out.  cn1 then has to come
#             up on whatever cn0 last wrote to disk.  Everything else (steps 1,
#             2, 4, 5 and 6) runs unchanged.
#
#           Step 2 runs in force mode too: it is an action on the referral
#           server, not on cn0, and a dead controller is exactly the case where
#           the host needs to be told to stop using it.
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

FORCE=0
NSLICES=2

# How long to give nvme-stas to drop cn0's path after step 2 removes the
# referral.  Observed: ~1s.  Nothing downstream depends on it, so this is a
# report, not a gate.
STAS_DROP_WAIT=20

# ===================== step 1: cn1 takes the optimized role ==================
step1_cn1() {
    _info "=== 1. cn1: suspend exported volume, ANA -> optimized ==="
    { _emit_vars; echo "CN='cn1'"; _emit_common; cat <<'EOF_S1'
set -uo pipefail
vp="dnv-${CN}-sp0-td0-exp0"

# 1.1 -- suspend the exported volume. --noflush is required: it currently sits
# on a 3600s dm-delay, so a flushing suspend could block forever. While
# suspended, dm queues incoming bios instead of failing them, which is exactly
# what we want for the handover window.
dm_exists "$vp" || _fail "$vp does not exist -- run setup.sh first"
if [ "$(dm_state "$vp")" != "SUSPENDED" ]; then
    _t "suspend $vp" sudo dmsetup suspend --nolockfs --noflush --noudevsync "$vp" \
        || _fail "could not suspend $vp"
fi
_info "$vp state: $(dm_state "$vp")"

# 1.2 -- start advertising this path as the good one
ana_set optimized
slow_summary
EOF_S1
    } | _ssh "${SSH_USER}@${CN1_IP}" bash -s
}

# ===================== step 2: ref0 stops referring host0 to cn0 =============
step2_ref0() {
    _info "=== 2. ref0: remove the cn0 target so host0 drops that path ==="
    { _emit_vars; _emit_common; cat <<'EOF_S2R'
set -uo pipefail
d="$NVMET/ports/1/referrals/cn0"
if [ -d "$d" ]; then
    sudo bash -c "echo 0 > '$d/enable'" >/dev/null 2>&1 || true
    _t "rmdir referral cn0" sudo rmdir "$d" >/dev/null 2>&1 \
        && _info "removed referral: cn0" || _warn "could not remove referral cn0"
else
    _warn "ref0 has no cn0 referral (was setup.sh run?)"
fi
_info "referrals left on ref0: $(ls "$NVMET/ports/1/referrals" 2>/dev/null | tr '\n' ' ')"
slow_summary
EOF_S2R
    } | _ssh "${SSH_USER}@${REF0_IP}" bash -s || return 1

    # Watch it land on the host. Nothing downstream waits for this -- the rest
    # of the failover is correct either way -- so a timeout is a warning.
    { _emit_vars; _emit_common; cat <<'EOF_S2H'
set -uo pipefail
n0=$(ctrl_count "$NQN_EXP" "$CN0_IP")
if [ "$n0" -eq 0 ]; then _info "host0 already has no path to cn0"; slow_summary; exit 0; fi
t0=$(date +%s%N)
while :; do
    n0=$(ctrl_count "$NQN_EXP" "$CN0_IP")
    [ "$n0" -eq 0 ] && break
    [ $(( ($(date +%s%N) - t0) / 1000000000 )) -ge "$STAS_DROP_WAIT" ] && break
    sleep 0.2
done
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
n1=$(ctrl_count "$NQN_EXP" "$CN1_IP")
if [ "$n0" -eq 0 ]; then
    _ok "host0 disconnected from cn0 by itself after ${ms}ms (nvme-stas followed ref0)"
else
    _warn "host0 still has $n0 path(s) to cn0 after ${ms}ms"
fi
if [ "$n1" -ge 1 ]; then
    _info "surviving path to cn1: $n1 controller(s)"
else
    _warn "no path to cn1 left either -- the host has lost the volume"
fi
sudo nvme list-subsys 2>/dev/null | grep -A4 "NQN=$NQN_EXP" || true
slow_summary
EOF_S2H
    } | _ssh "${SSH_USER}@${HOST0_IP}" bash -s
}

# ===================== step 3: cn0 stands down ===============================
step3_cn0() {
    _info "=== 3. cn0: ANA -> inaccessible, suspend volume, dismantle the stack ==="
    { _emit_vars; echo "CN='cn0'"; _emit_common; cat <<'EOF_S3'
set -uo pipefail
vp="dnv-${CN}-sp0-td0-exp0"

# 3.1 -- stop advertising this path. host0 is already gone (step 2), so this is
# for the sake of any other host that might still be looking at cn0.
ana_set inaccessible
# give the host a moment to read the ANA log and steer new I/O to cn1
sleep 1

# 3.2 -- flushing suspend: everything the host already handed us must reach the
# thin-pool before we take the stack apart. --nolockfs only skips the fs freeze
# (there is no filesystem here); in-flight bios are still drained.
dm_exists "$vp" || _fail "$vp does not exist"
if [ "$(dm_state "$vp")" != "SUSPENDED" ]; then
    _t "suspend(flush) $vp" sudo dmsetup suspend --nolockfs --noudevsync "$vp" \
        || _fail "could not suspend $vp"
fi
_info "$vp state: $(dm_state "$vp")"

# The exported volume's live table still references ${vp}-real, which keeps the
# raid0 open and makes step 3.3 impossible. Perform step 7.1's reload now to
# drop that reference, then park the device suspended again so no I/O can reach
# the 3600s delay while the rest of the failover runs. Step 7 resumes it.
_t "load $vp (-> delay)" sudo dmsetup load "$vp" --table "0 $SEC_EXP linear /dev/mapper/${vp}-delay 0" \
    || _fail "could not load delay table into $vp"
_t "resume $vp" sudo dmsetup resume --noudevsync "$vp" || _fail "could not resume $vp"
_t "re-suspend $vp" sudo dmsetup suspend --nolockfs --noflush --noudevsync "$vp" \
    || _warn "could not re-suspend $vp"
_info "$vp now maps: $(sudo dmsetup table "$vp")"

# 3.3 -- delete everything created by setup.sh steps (create_grp, create_slice,
# create_exp_active), in reverse creation order. Removing a thin-pool commits
# its metadata; that commit is what lets cn1 open the same pool in step 6.
#
# Inline dmsetup here (not delete_cntlr_active) because the table-swap reload above
# means the exp is still suspended and partially dismantled; the exact reverse
# order matters. The names/tables/sizes match common.sh exactly.
# 3.3.1 exp -real (raid0)
dm_rm() {
    local n="$1"
    dm_exists "$n" || { _info "absent  : $n"; return 0; }
    [ "$(dm_state "$n")" = "SUSPENDED" ] && sudo dmsetup resume --noudevsync "$n" >/dev/null 2>&1 || true
    _t "remove $n" sudo dmsetup remove --noudevsync "$n" >/dev/null 2>&1 && _info "removed : $n" \
        || _warn "could not remove $n"
}
dm_rm "${vp}-real"

# 3.3.2 per slice: td0 -> thinpool -> thindata/thinmeta -> per grp: thindata/thinmeta/raid1/leg0/leg1
for slice in 1 0; do
  lp="dnv-${CN}-sp0-slice${slice}"
  dm_rm "${lp}-td0"        # create_slice: thin device
  dm_rm "${lp}-thinpool"     # create_slice: thin pool (commits metadata)
  dm_rm "${lp}-thindata"     # create_slice: concat thindata
  dm_rm "${lp}-thinmeta"     # create_slice: concat thinmeta
  for grp in 1 0; do
    p="${lp}-grp${grp}"
    dm_rm "${p}-thindata"           # create_grp: thin data slice
    dm_rm "${p}-thinmeta"           # create_grp: thin meta slice
    dm_rm "${p}-raid1"              # create_grp: dm-raid1
    dm_rm "${p}-raid1-data-leg1"   # create_grp: data leg1
    dm_rm "${p}-raid1-meta-leg1"
    dm_rm "${p}-raid1-data-leg0"   # create_grp: data leg0
    dm_rm "${p}-raid1-meta-leg0"
  done
done

left=$(sudo dmsetup ls 2>/dev/null | awk '{print $1}' | grep "^dnv-${CN}-sp0-slice" | wc -l)
[ "$left" -eq 0 ] && _ok "cn0 slice stack fully removed" || _warn "$left slice device(s) left on cn0"
_info "cn0 dm devices left: $(sudo dmsetup ls | awk '{print $1}' | grep ^dnv- | tr '\n' ' ')"
slow_summary
EOF_S3
    } | _ssh "${SSH_USER}@${CN0_IP}" bash -s
}

# ===================== steps 4/5: the data nodes swap the paths ==============
step45_dn() {
    local ip="$1" dn="$2" step="$3"
    _info "=== $step. $dn: point -cn0 at the delay, -cn1 at the real data ==="
    { _emit_vars; echo "DN='$dn'"; _emit_common; cat <<'EOF_S45'
set -uo pipefail
for slice in 0 1; do
  for grp in 0 1; do
    for lid in 0 1; do
      base="dnv-${DN}-sp0-slice${slice}-grp${grp}-ld${lid}"
      VG="dnv-${DN}-sp0-vg"
      REAL_DEV="/dev/${VG}/slice${slice}-grp${grp}-ld${lid}-real"
      # x.1 cn0 loses access
      dm_reload "${base}-cn0" "0 $SEC_PD linear /dev/mapper/${base}-delay-cn0 0" \
          || _warn "reload failed: ${base}-cn0"
      # x.2 cn1 gains access (the -real backing device is now an LVM LV)
      dm_reload "${base}-cn1" "0 $SEC_PD linear $REAL_DEV 0" \
          || _warn "reload failed: ${base}-cn1"
    done
  done
done
slow_summary
EOF_S45
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# ===================== step 6: cn1 builds the stack and goes live ============
step6_cn1() {
    _info "=== 6. cn1: build the stack, repoint the exported volume, resume ==="
    # Use create_cntlr_active to rebuild the full grp/slice/thinpool stack on cn1.
    # create_td cn1 sp0 0 0 should fail => thin 0 is inherited from cn0's
    # committed metadata.
    create_cntlr_active cn1 "$CN1_IP" sp0 "$NSLICES" "$HOSTNQN_CN1" "$HOSTID_CN1"

    # create_exp_active builds the raid0+real+error+delay+exp stack. But we need
    # to REPOINT the existing suspended exp device at the new -real, not create
    # a new one. So we build -real/-error/-delay manually (matching common.sh
    # names) and reload the suspended exp.
    { _emit_vars; echo "CN='cn1'"; _emit_common; cat <<'EOF_S6'
set -uo pipefail

# Drop any page-cache entries left over from the period when these namespaces
# were backed by the DN error/delay devices.
declare -A LDDEV
for slice in 0 1; do
  for grp in 0 1; do
    for d in 0 1; do
      nqn="${NQN_PREFIX}:dn:dn${d}:sp0-slice${slice}-grp${grp}-ld${d}-${CN}"
      dev=$(nvme_dev_by_nqn "$nqn") || _fail "no block device for $nqn"
      sudo blockdev --flushbufs "$dev" >/dev/null 2>&1 || true
      LDDEV["$slice,$grp,$d"]="$dev"
    done
  done
done

# 6.1 -- create_grp already built raid1+thinmeta+thindata; create_slice already
# built thinpool+td0. The thin-pool metadata is cn0's, committed when cn0
# removed the pool. It must NOT be zeroed -- it holds the mapping for thin
# device 0 and therefore the data the host has already written. thin 0 already
# exists, so create_thin 0 should fail (inherited).
for slice in 0 1; do
    lp="dnv-${CN}-sp0-slice${slice}"
    dm_exists "${lp}-thinpool" || _fail "no ${lp}-thinpool -- create_cntlr_active did not run?"
    sudo dmsetup message "/dev/mapper/${lp}-thinpool" 0 "create_thin 0" >/dev/null 2>&1 \
        && _warn "thin 0 did not exist in ${lp}-thinpool -- pool metadata was not inherited" \
        || _info "thin 0 already present in ${lp}-thinpool (inherited from cn0)"
done

vp="dnv-${CN}-sp0-td0-exp0"

# 6.2 -- build the exp -real (raid0 of per-slice td0 thin-devs). This matches
# create_exp_active's table exactly.
dm_create "${vp}-real" \
"0 $SEC_EXP raid raid0 1 $RAID0_CHUNK_SECTORS 2 \
- /dev/mapper/dnv-${CN}-sp0-slice0-td0 \
- /dev/mapper/dnv-${CN}-sp0-slice1-td0"

# 6.3 / 6.4 -- repoint the exported volume at the fresh raid0 and let the
# queued host I/O through. dm_reload does load + resume; the device is already
# suspended from step 1.1.
dm_reload "$vp" "0 $SEC_EXP linear /dev/mapper/${vp}-real 0" || _fail "could not reload $vp"
_info "$vp state: $(dm_state "$vp"), table: $(sudo dmsetup table "$vp")"
_ok "cn1 is now serving the volume"
slow_summary
EOF_S6
    } | _ssh "${SSH_USER}@${CN1_IP}" bash -s
}

# ===================== step 7: cn0's volume parks on its delay ===============
step7_cn0() {
    _info "=== 7. cn0: exported volume -> dm-delay ==="
    { _emit_vars; echo "CN='cn0'"; _emit_common; cat <<'EOF_S7'
set -uo pipefail
vp="dnv-${CN}-sp0-td0-exp0"
dm_reload "$vp" "0 $SEC_EXP linear /dev/mapper/${vp}-delay 0" || _fail "could not reload $vp"
_info "$vp state: $(dm_state "$vp"), table: $(sudo dmsetup table "$vp")"
slow_summary
EOF_S7
    } | _ssh "${SSH_USER}@${CN0_IP}" bash -s
}

# ===================== main ==================================================
main() {
    case "${1:-}" in
        "")      ;;
        force)   FORCE=1 ;;
        *)       _fail "usage: $0 [force]" ;;
    esac

    local t0 t1
    t0=$(date +%s%N)
    step1_cn1                      || _fail "step 1 (cn1) failed"
    # Runs in force mode as well: it touches ref0, not cn0.
    step2_ref0                     || _warn "step 2 (ref0) reported errors"
    if [ "$FORCE" -eq 1 ]; then
        _info "=== 3. cn0: SKIPPED (force) -- no ANA change, no suspend, no teardown ==="
    else
        step3_cn0                  || _fail "step 3 (cn0) failed"
    fi
    step45_dn "$DN0_IP" dn0 4       || _fail "step 4 (dn0) failed"
    step45_dn "$DN1_IP" dn1 5       || _fail "step 5 (dn1) failed"
    step6_cn1                      || _fail "step 6 (cn1) failed"
    if [ "$FORCE" -eq 1 ]; then
        _info "=== 7. cn0: SKIPPED (force) -- exported volume left as it was ==="
    else
        step7_cn0                  || _fail "step 7 (cn0) failed"
    fi
    t1=$(date +%s%N)
    _info "failover cn0 -> cn1 complete in $(( (t1 - t0) / 1000000 ))ms$( [ "$FORCE" -eq 1 ] && echo ' (force: steps 3 and 7 skipped)')"
}

main "$@"
