#!/bin/bash
#
# grow.sh -- extend leg0's thin pool with a third group (grp2) while host I/O
# is running on the exported volume.
#
# The extension grows the pool's metadata and data devices from 2 groups to 3,
# resuming without I/O errors or stuck commands.  After grow, teardown.sh all
# still cleans up every resource (including grp2) correctly.
#
# Workflow (grow and failover are never combined):
#
#   setup.sh all  ->  host0_io.sh start  ->  grow.sh  ->  host0_io.sh report  ->  teardown.sh all
#
# Sequence:
#   1. dn0/dn1 : create the grp2 VDs (full symmetric fault-injection stack for
#                both cns) -- create_vd per (dn, cn, vd) pair.
#   2. dn0/dn1 : disarm the grp2 -delay devices so cn1's namespace scan does not
#                hang on the 3600s delay (see note.md section 10).
#   3. cn1     : connect the grp2 VDs (vd0 from dn0, vd1 from dn1).  The disarmed
#                delay means the kernel partition scan fails fast (EIO).
#   4. dn0/dn1 : re-arm the grp2 -delay devices (put the real delay table back).
#   5. cn0     : create_grp aggregates the two grp2 VDs into a raid1 mirror +
#                thinmeta/thindata slices (create_grp connects the VDs internally).
#   6. cn0     : extend leg0's thin pool:
#                  a. suspend the pool (noflush) -- queues host I/O.
#                  b. reload leg0-thinmeta  (3-way concat: grp0 + grp1 + grp2).
#                  c. reload leg0-thindata  (3-way concat: grp0 + grp1 + grp2).
#                  d. load  leg0-thinpool  (new table, size = 3 * SEC_TDATA).
#                  e. resume leg0-thinpool (drains queued host I/O).
#
# Sizes are inlined here per the grow_plan (decision 5): meta_sz = SEC_TMETA*3,
# data_sz = SEC_TDATA*3.  They must match the concat tables below exactly.
#
# grow.sh does NOT call host0_io.sh (decision 7): the user starts/stops I/O
# externally.  Verification is the existing mechanisms -- host0_io.sh report for
# I/O errors, _t/slow_summary in every SSH block for stuck commands, and
# teardown.sh all (with dynamic discovery) for resource cleanup.
#
# Usage: ./grow.sh
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

LEG=0
GRP=2

# ===================== step 1: create grp2 VDs on the DNs =====================
step1_create_vds() {
    _info "=== 1. dn0/dn1: create grp2 VDs (vd0 on dn0, vd1 on dn1, both cns) ==="
    # vd_id encodes which dn: vd0 -> dn0, vd1 -> dn1.  Both cns get a symmetric
    # fault-injection stack so failover (if ever run later from this grown state)
    # would find the standby side's -delay-cn1 devices present.
    create_vd dn0 "$DN0_IP" cn0 "$CN0_IP" 0 "$LEG" "$GRP"
    create_vd dn0 "$DN0_IP" cn1 "$CN1_IP" 0 "$LEG" "$GRP"
    create_vd dn1 "$DN1_IP" cn0 "$CN0_IP" 1 "$LEG" "$GRP"
    create_vd dn1 "$DN1_IP" cn1 "$CN1_IP" 1 "$LEG" "$GRP"
}

# ====== steps 2-4: disarm, cn1 connects to grp2 VDs, re-arm ==================
step234_connect_cnvds() {
    _info "=== 2. dn0/dn1: disarm grp2 -delay devices for cn1's namespace scan ==="
    disarm_vd_delay dn0 "$DN0_IP" "$LEG" "$GRP"
    disarm_vd_delay dn1 "$DN1_IP" "$LEG" "$GRP"

    _info "=== 3. cn1: connect the grp2 VDs (vd0 from dn0, vd1 from dn1) ==="
    connect_vd cn1 "$CN1_IP" dn0 "$DN0_IP" 0 "$HOSTNQN_CN1" "$HOSTID_CN1" "$LEG" "$GRP"
    connect_vd cn1 "$CN1_IP" dn1 "$DN1_IP" 1 "$HOSTNQN_CN1" "$HOSTID_CN1" "$LEG" "$GRP"

    _info "=== 4. dn0/dn1: re-arm grp2 -delay devices ==="
    arm_vd_delay dn0 "$DN0_IP" "$LEG" "$GRP"
    arm_vd_delay dn1 "$DN1_IP" "$LEG" "$GRP"
}

# ===================== step 5: cn0 aggregates grp2 =============================
step5_create_grp() {
    _info "=== 5. cn0: create_grp for leg$LEG grp$GRP (raid1 + thinmeta/thindata) ==="
    # create_grp connects both grp2 VDs internally, builds the raid1 mirror and
    # carves the thinmeta/thindata slices.  Already takes leg/grp explicitly.
    create_grp cn0 "$CN0_IP" da0 "$LEG" "$GRP" "$HOSTNQN_CN0" "$HOSTID_CN0"
}

# ===================== step 6: extend leg0 thin pool ==========================
step6_extend_pool() {
    _info "=== 6. cn0: extend leg${LEG}-thinpool with grp$GRP (suspend, reload, resume) ==="
    { _emit_vars; echo "CN='cn0'; LEG='$LEG'; GRP='$GRP'"; _emit_common; cat <<'EOF_GROW'
set -uo pipefail

sp="dnv-${CN}-da0-leg${LEG}"

# New sizes: 3 groups' worth of metadata and data.  These must match the concat
# tables below exactly (grow_plan decision 5).  Inlined here, not new SEC_*
# constants in common.sh, because grow is the only path that uses 3-way concats.
meta_sz=$(( SEC_TMETA * 3 ))     # 3 * 16384 = 49152  (24M)
data_sz=$(( SEC_TDATA * 3 ))     # 3 * 999424 = 2998272 (1464M)

# Precondition: the base 2-group pool and the grp2 slices must exist.
dm_exists "${sp}-thinpool"         || _fail "${sp}-thinpool does not exist -- run setup.sh first"
dm_exists "${sp}-grp${GRP}-thinmeta" || _fail "${sp}-grp${GRP}-thinmeta does not exist -- step 5 (create_grp) did not run?"
dm_exists "${sp}-grp${GRP}-thindata" || _fail "${sp}-grp${GRP}-thindata does not exist -- step 5 (create_grp) did not run?"

# 6.a -- suspend the pool (noflush).  While suspended, dm queues incoming host
# bios instead of failing them, which is what makes the grow graceful: the
# host's in-flight I/O waits inside the suspended pool and drains on resume.
# --nolockfs: there is no filesystem on the pool.  --noflush matches the
# convention used everywhere else in this stack (a flushing suspend could block
# if the old table referenced a dm-delay; the pool's devices are linear slices
# of raid1 over -real here, so flush would be safe, but noflush is consistent).
if [ "$(dm_state "${sp}-thinpool")" != "SUSPENDED" ]; then
    _t "suspend ${sp}-thinpool" sudo dmsetup suspend --nolockfs --noflush --noudevsync "${sp}-thinpool" \
        || _fail "could not suspend ${sp}-thinpool"
fi
_info "${sp}-thinpool state: $(dm_state "${sp}-thinpool")"

# 6.b -- reload leg0-thinmeta: 3-way concat of grp0 + grp1 + grp2 thinmeta.
#   old (2-way, size = SEC_POOL_META = 32768):
#     0      16384 linear .../grp0-thinmeta 0
#     16384  16384 linear .../grp1-thinmeta 0
#   new (3-way, size = 49152):
#     0      16384 linear .../grp0-thinmeta 0
#     16384  16384 linear .../grp1-thinmeta 0
#     32768  16384 linear .../grp2-thinmeta 0
dm_reload "${sp}-thinmeta" \
"0 $SEC_TMETA linear /dev/mapper/${sp}-grp0-thinmeta 0
$SEC_TMETA $SEC_TMETA linear /dev/mapper/${sp}-grp1-thinmeta 0
$(( SEC_TMETA * 2 )) $SEC_TMETA linear /dev/mapper/${sp}-grp${GRP}-thinmeta 0" \
    || _fail "could not reload ${sp}-thinmeta"

# 6.c -- reload leg0-thindata: 3-way concat of grp0 + grp1 + grp2 thindata.
#   old (2-way, size = SEC_POOL_DATA = 1998848):
#     0       999424 linear .../grp0-thindata 0
#     999424  999424 linear .../grp1-thindata 0
#   new (3-way, size = 2998272):
#     0        999424 linear .../grp0-thindata 0
#     999424   999424 linear .../grp1-thindata 0
#     1998848  999424 linear .../grp2-thindata 0
dm_reload "${sp}-thindata" \
"0 $SEC_TDATA linear /dev/mapper/${sp}-grp0-thindata 0
$SEC_TDATA $SEC_TDATA linear /dev/mapper/${sp}-grp1-thindata 0
$(( SEC_TDATA * 2 )) $SEC_TDATA linear /dev/mapper/${sp}-grp${GRP}-thindata 0" \
    || _fail "could not reload ${sp}-thindata"

# 6.d -- load the new thin-pool table (size = data_sz = 3 * SEC_TDATA).  The
# pool is still suspended from 6.a, so this is a raw load (no re-suspend).
#   new table:
#     0 2998272 thin-pool /dev/mapper/...-thinmeta /dev/mapper/...-thindata $POOL_BLOCK_SECTORS 0 1 skip_block_zeroing
_t "load ${sp}-thinpool" sudo dmsetup load "${sp}-thinpool" \
    --table "0 $data_sz thin-pool /dev/mapper/${sp}-thinmeta /dev/mapper/${sp}-thindata $POOL_BLOCK_SECTORS 0 1 skip_block_zeroing" \
    || _fail "could not load ${sp}-thinpool"

# 6.e -- resume the pool: drains the queued host I/O through the now-larger pool.
_t "resume ${sp}-thinpool" sudo dmsetup resume --noudevsync "${sp}-thinpool" \
    || _fail "could not resume ${sp}-thinpool"
_info "${sp}-thinpool state: $(dm_state "${sp}-thinpool"), table: $(sudo dmsetup table "${sp}-thinpool")"
_ok "leg${LEG} thin pool extended to $data_sz sectors (3 groups)"

slow_summary
EOF_GROW
    } | _ssh "${SSH_USER}@${CN0_IP}" bash -s
}

# ===================== main ==================================================
main() {
    local t0 t1
    t0=$(date +%s%N)
    step1_create_vds              || _fail "step 1 (create grp2 VDs) failed"
    step234_connect_cnvds         || _fail "steps 2-4 (disarm, connect, arm) failed"
    step5_create_grp             || _fail "step 5 (create_grp) failed"
    step6_extend_pool            || _fail "step 6 (extend thin pool) failed"
    t1=$(date +%s%N)
    _info "grow leg${LEG} +grp${GRP} complete in $(( (t1 - t0) / 1000000 ))ms"
}

main "$@"
