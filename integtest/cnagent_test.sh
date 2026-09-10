#!/usr/bin/env bash
#
# cnagent_test.sh — the `dnv-agent cn` integration test of
# doc/cnagent_integtest.md. Two real VMs, real nvme-tcp/md-raid1/dm-thin/
# dm-clone/nvmet, driven over gRPC from this machine by integtest/cnagentctl,
# with real `dnv-agent dn` instances (driven by integtest/dnagentctl) as the
# side backing.
#
#   bash integtest/cnagent_test.sh [--only <case>] [--cleanup-only] \
#       user1@ip1 user2@ip2
#
# Cases: smoke, redund, thinbm, clone_xfer, restart (§10-§14). Cleanup runs
# unconditionally at the start and, on success only, at the end: a failing run
# leaves every dm/md/nvmet object and all four agent logs in place and
# dumps diagnostics (§17).
#
# The uutils dd rule of §4 is absolute: this script never passes iflag= or
# oflag= to dd. Writes use conv=fsync, reads that must hit the media are
# preceded by a cache drop. Do not "fix" this back to direct IO.
#
# Both roles run from one binary named `dnv-agent`, so `pkill -x dnv-agent`
# would kill both (§3, Appendix A). Every kill here is
# `pkill -f 'dnv-agent dn'` / `pkill -f 'dnv-agent cn'`. Do not "simplify" it.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (§3, §5, §6)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CTL_DN="$REPO_ROOT/integtest/bin/dnagentctl"
CTL_CN="$REPO_ROOT/integtest/bin/cnagentctl"
AGENT_BIN="$REPO_ROOT/bin/dnv-agent"

# Deliberately not the dn suite's directory, so the two suites never share
# debris (§3). The driver uses it too, for the §8 request files.
WORK=/var/tmp/dnv-cn-integtest
HELPER=/var/tmp/dnv-cn-integtest-helper.sh
DN_LOG="$WORK/dn-agent.log"
CN_LOG="$WORK/cn-agent.log"
NVMET=/sys/kernel/config/nvmet

DN_GRPC_PORT=29528
CN_GRPC_PORT=29529
TR_SVC_ID=4200
CLUSTER=0x1
EXTENT_SIZE=67108864 # 64 MiB — MinDnExtSize; the proto field is raw bytes
# GetDnSize reports the [D13] data area, not the raw device (§6): 2 GiB
# backing file, 2147483648 - 268435456 = 1879048192.
DATA_SIZE=1879048192
# --capacity: an arbitrary exact value GetCnSize must echo back (CN-CM1, §6).
CN_CAPACITY=1099511627776

# Bdev parameters, everywhere (§6).
BLOCK_SIZE=1048576
STRIPE_SIZE=65536
BM_CHUNK_BLOCKS=128
LOW_WATER_PCT=50
TD_SIZE=67108864 # 64 MiB = 64 thin blocks

# The worked §3.6 arithmetic of §6. The request carries these; the agent
# consumes them and never recomputes.
NONE_META_BLOCKS=1
NONE_EXT1_DATA=63
NONE_EXT2_DATA=127
RAID1_META_BLOCKS=3
RAID1_EXT1_DATA=61
RAID1_EXT2_DATA=125

# Per-RPC deadlines for the converge RPCs (see dnctl/cnctl).
DN_SYNCUP_TIMEOUT=60
CN_SYNCUP_TIMEOUT=180

# Polling budget of `dnagentctl wait-zeroed` (update_01.md U4). With 64 MiB
# extents on a loop device the kernel maps REQ_OP_WRITE_ZEROES onto fallocate,
# so a 1-2 extent side finishes in well under a second; the budget only has to
# cover a stalled retry loop (DnZeroRetryInterval = 5 s).
ZERO_TIMEOUT=120

NQN_PREFIX=nqn.2024-01.io.dnv
NQN_IT_PREFIX=nqn.2024-01.io.dnv-it
HOST_NQN=nqn.2024-01.io.dnv-it:host:0

# Per-VM state, 1-indexed so "vm1"/"vm2" read directly. Each VM runs both
# agents (§3): dn 0x1/0x2 on :29528, cn 0x11/0x12 on :29529.
VM=("" "" "")
IP=("" "" "")
DNID=("" 0x1 0x2)
CNID=("" 0x11 0x12)
DNREV=("" 0 0)
CNREV=("" 0 0)
CNSYNC=("" 0 0)
LOOP=("" "" "")

# The S-shaped SP of §5: one RedundNone meta group and one RedundNone data
# group on one DN, one td, one subsystem with one namespace. Cases S, B and
# both halves of C use these sub-ids verbatim — only the sp, the nodes, the
# cntlid slot and the namespace identity differ.
S_SLICE=0x2
S_MGRP=0x3
S_MLEG=0x4
S_MSIDE=0x5
S_DGRP=0x6
S_DLEG=0x7
S_DSIDE=0x8
S_TD=0x9
S_SS=0xa
S_NS=0xb

# The A-shaped SP of §5: md-raid1 meta and data groups across both DNs, a
# primary and a standby cntlr. Cases A and D share the sub-ids; leg 1 of every
# group lives on DN1, leg 2 on DN2.
A_SLICE=0x3
A_MGRP=0x4
A_MLEG=("" 0x5 0x7)
A_MSIDE=("" 0x6 0x8)
A_DGRP=0x9
A_DLEG=("" 0xa 0xc)
A_DSIDE=("" 0xb 0xd)
A_TD=0xe
A_SS=0xf
A_NS=0x10

JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE="setup"
TRACE="it-setup"
STAGE="(startup)"
SETUP_DONE=0
DIAG_CNTLRS=()

CASES=(smoke redund thinbm clone_xfer restart)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id the §9
# convention asks for: one id shared by the driver call, the agent handler and
# every os command it runs.
stage() {
	STAGE="$CASE: $2"
	TRACE="it-$CASE-$1"
	log ""
	log "=== $CASE: $2   [trace_id $TRACE]"
}

die() {
	log ""
	log "FAILED at stage '$STAGE' (trace_id $TRACE)"
	log "  $*"
	exit 1
}

assert_eq() { [ "$1" = "$2" ] || die "$3: got '$1', want '$2'"; }

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

# assert_ok/assert_not_ok read a ResInfo.status out of a reply. An absent
# message renders as ABSENT — which is exactly what "missing/gated" looks like
# over protojson, where unset fields are omitted.
assert_ok() {
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	[ "$got" = "RES_STATUS_OK" ] || die "$3: status is $got, want RES_STATUS_OK"
}

assert_not_ok() {
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	[ "$got" != "RES_STATUS_OK" ] || die "$3: status is OK, want not OK"
}

# assert_provisioning_or_ok accepts the two statuses a DN side may legally hold
# at provisioned = false (update_01.md U4's converge matrix rows 2 and 3):
# PROVISIONING while the background zeroing goroutine still has extents to go,
# and OK once every bit is set. Zeroing 64-128 MiB on a loop device is a
# `fallocate`, so which of the two a phase-1 reply carries is a genuine race —
# do not pick one.
assert_provisioning_or_ok() { # json path label
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	case "$got" in
	RES_STATUS_PROVISIONING | RES_STATUS_OK) ;;
	*) die "$3: status is $got, want PROVISIONING or OK" ;;
	esac
}

# assert_provisioning is the exact form, for the rows update_01.md pins to
# PROVISIONING with no race: the resources a deferred side or leg deliberately
# does not create. RES_STATUS_PROVISIONING is a *healthy* status, so it
# satisfies a bare assert_not_ok and trips every assert_all_ok — naming it
# explicitly is what keeps both honest (ruling R4.35).
assert_provisioning() { # json path label
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	[ "$got" = "RES_STATUS_PROVISIONING" ] ||
		die "$3: status is $got, want RES_STATUS_PROVISIONING"
}

# assert_suppressed reads a resource CN19 gated off: not OK, with "sp_level" as
# the reason. The details check is what keeps it exact now that
# RES_STATUS_PROVISIONING also satisfies "not OK" — a deferred resource reports
# "provisioning", never "sp_level" (ruling R4.35, R4.28).
assert_suppressed() { # json path label
	assert_not_ok "$1" "$2.status" "$3"
	assert_eq "$(jq_of "$1" "$2.details // \"ABSENT\"")" sp_level "$3 details"
}

# assert_map_ok checks one entry of a CntlrInfo map<uint64, ResInfo>;
# protojson renders the keys as decimal strings.
assert_map_ok() { # json field id label
	local key
	key=$(d16 "$3")
	assert_ok "$1" ".cntlr_info.$2[\"$key\"].status" "$4 $2[$3]"
}

# assert_thin_ok checks td_id_to_thin_info[td].slice_id_to_dm_thin[slice].
assert_thin_ok() { # json td slice label
	assert_ok "$1" \
		".cntlr_info.td_id_to_thin_info[\"$(d16 "$2")\"].slice_id_to_dm_thin[\"$(d16 "$3")\"].status" \
		"$4 thin[$2][$3]"
}

# assert_cn_info_ok covers the four §3.2 base-state resources of CnInfo. The
# fifth — the old clone-VG row — went away with LVM: the clone-metadata arena
# is now the loop device itself (loop_dev_info) plus the kind-b wrapper dm
# tables, and the proto field is `reserved 5` (update_01.md U3 spec 5).
assert_cn_info_ok() { # json label
	local field
	for field in port_info tmpfs_info tmp_file_info loop_dev_info; do
		assert_ok "$1" ".cn_info.$field.status" "$2 $field"
	done
}

# assert_all_ok fails on any ResInfo under a reply subtree whose status is not
# OK — the §9 converge-check rule "every expected status == RES_STATUS_OK".
assert_all_ok() { # json path label
	local bad
	bad=$(jq_of "$1" \
		"[$2 | .. | objects | select(has(\"status\")) | select(.status != \"RES_STATUS_OK\") | .res_name] | join(\",\")")
	[ -z "$bad" ] || die "$3: not-OK resources: $bad"
}

on_exit() {
	local rc=$?
	trap - EXIT
	if [ "$rc" -eq 0 ]; then
		if [ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ]; then
			log ""
			log "=== end-of-run cleanup (success)"
			cleanup_all
		fi
		log ""
		log "PASS"
	else
		log ""
		log "########## diagnostics (§17) ##########"
		diagnostics || true
		log ""
		log "debris left in place on both VMs; failing stage '$STAGE'"
		log "pull records with: jq 'select(.trace_id==\"$TRACE\")' $CN_LOG"
		log "                   jq 'select(.trace_id==\"$TRACE\")' $DN_LOG"
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution
# ---------------------------------------------------------------------------

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# sshv runs one command as root on a VM and returns its stdout. Everything the
# agents touch (md, dm, configfs, nvme) needs root, so every remote command
# goes through sudo (§3).
sshv() {
	local idx=$1
	shift
	log "[vm$idx] $*"
	ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo bash -c $(printf '%q' "$*")"
}

sshv_ok() { sshv "$@" || true; }

# helper invokes one function of the shipped VM helper (below).
helper() {
	local idx=$1
	shift
	sshv "$idx" "bash $HELPER $*"
}

helper_ok() {
	local idx=$1
	shift
	sshv_ok "$idx" "bash $HELPER $*"
}

# dnctl drives one dn agent, cnctl one cn agent. Every call carries the current
# stage's trace id.
#
# The §8 default 10 s deadline suits the read-only RPCs, but one converge pass
# runs dozens of OS commands, each with its own soft/hard budget: a cn primary
# converge connects four legs, creates two md arrays, a pool, thin volumes, a
# raid0 and the nvmet objects. Syncups therefore get a much larger budget; a
# caller's own --timeout still wins, since it lands later on the command line.
dnctl() {
	local idx=$1
	shift
	local sub=$1
	shift
	local extra=()
	case "$sub" in
	syncup-dn | syncup-side) extra=(--timeout "$DN_SYNCUP_TIMEOUT") ;;
	esac
	log "[dn$idx] dnagentctl $sub $*"
	"$CTL_DN" "$sub" \
		--addr "${IP[$idx]}:$DN_GRPC_PORT" \
		--cluster "$CLUSTER" --dn "${DNID[$idx]}" \
		--trace-id "$TRACE" "${extra[@]}" "$@"
}

cnctl() {
	local idx=$1
	shift
	local sub=$1
	shift
	local extra=()
	case "$sub" in
	syncup-cn | syncup-cntlr) extra=(--timeout "$CN_SYNCUP_TIMEOUT") ;;
	esac
	log "[cn$idx] cnagentctl $sub $*"
	"$CTL_CN" "$sub" \
		--addr "${IP[$idx]}:$CN_GRPC_PORT" \
		--cluster "$CLUSTER" --cn "${CNID[$idx]}" \
		--trace-id "$TRACE" "${extra[@]}" "$@"
}

# bump_dn_rev/bump_cn_rev advance a node's monotonic revision counter (§9).
# They must run in the parent shell — never inside a command substitution or a
# background job, both of which would increment a copy. On a CN one counter is
# shared by SyncupCn and SyncupCntlr (the single CnRev of §5.5), so bump_cn_sync
# additionally records the revision SyncupCn stored: that, not the counter, is
# what CheckCn echoes back.
bump_dn_rev() { DNREV[$1]=$((DNREV[$1] + 1)); }
bump_cn_rev() { CNREV[$1]=$((CNREV[$1] + 1)); }
bump_cn_sync() {
	bump_cn_rev "$1"
	CNSYNC[$1]=${CNREV[$1]}
}

# ---------------------------------------------------------------------------
# Derived names (§5) — the bash mirror of common/name_fmt.go
# ---------------------------------------------------------------------------

hex16() { printf '%016x' "$(($1))"; }

# d16 is hex16's twin for protojson, which renders 64-bit fields as decimal
# strings (§8).
d16() { printf '%u' "$(($1))"; }

# cn_dm_name mirrors every cn dm formatter: dnv-{cluster}-{cn}-{kind}-{ids…}
# with the §4.2 kind digits — 0 pool-meta, 1 pool-data, 2 pool-final, 3 thin,
# 4 raid0, 5 error, 6 ns-dev, 7 clone-final, 8 xfer-final, 9 leg, a group,
# b clone-meta.
cn_dm_name() { # kind cnidx id…
	local kind=$1 cn=$2 id
	shift 2
	printf 'dnv-%s-%s-%s' "$(hex16 "$CLUSTER")" "$(hex16 "${CNID[$cn]}")" "$kind"
	for id in "$@"; do printf -- '-%s' "$(hex16 "$id")"; done
	printf '\n'
}

side_to_cn_nqn() { # cluster sp leg cn
	printf '%s:2:%s:%s:%s:%s' "$NQN_PREFIX" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

cn_host_nqn() { printf '%s:1:%s:%s' "$NQN_PREFIX" "$(hex16 "$1")" "$(hex16 "$2")"; }

xfer_nqn() { # cluster sp xfer
	printf '%s:4:%s:%s:%s' "$NQN_PREFIX" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")"
}

# clone_meta_dm mirrors common.CnCloneMetaDmName (update_01.md U3 spec 1): the
# kind-b dm-linear wrapper that carries one dm-clone's metadata, allocated out
# of the loop-device arena. It replaced the clone VG's metadata LV, and with it
# the doubled-dash `dnv--clone--vg-*` gotcha — a wrapper is created by the
# agent with `dmsetup create` and carries single dashes.
clone_meta_dm() { # cnidx sp clone
	cn_dm_name b "$1" "$2" "$3"
}

# host_dev is the host-side multipath node of a namespace. The ns uuids are
# fixed inputs of §5, so no lookup is needed.
host_dev() { printf '/dev/disk/by-id/nvme-uuid.%s' "$1"; }

# ---------------------------------------------------------------------------
# Host emulation (§3): the host role is plain nvme-tcp with one fixed hostnqn,
# cross-connected from whichever VM the case names.
# ---------------------------------------------------------------------------

# host_id mirrors common.NvmeHostId — see the dn suite's copy. The host
# identity shares each VM with two agents that connect under their own
# hostnqns, so the node-wide /etc/nvme/hostid must never be left implicit.
host_id() { "$CTL_CN" host-id --hostnqn "$1"; }

host_connect() { # hostvm cnidx nqn
	local hid
	hid=$(host_id "$HOST_NQN")
	sshv "$1" "nvme connect -t tcp -a ${IP[$2]} -s $TR_SVC_ID -n '$3' --hostnqn '$HOST_NQN' --hostid '$hid'"
}

host_disconnect() { # hostvm nqn
	sshv_ok "$1" "nvme disconnect -n '$2' || true"
}

host_state() { # hostvm nqn cnidx
	helper "$1" "path_field '$2' '${IP[$3]}' State"
}

host_ana() { # hostvm nqn cnidx
	helper "$1" "ana_state '$2' '${IP[$3]}'"
}

host_wait_ana() { # hostvm nqn cnidx want secs
	helper "$1" "wait_ana '$2' '${IP[$3]}' '$4' '$5'" ||
		die "the host path to cn$3 never reached ANA state '$4'"
}

# leg_ana is the same probe one hop down: the ANA state a CN sees on its own
# connection to one side of a leg.
leg_ana() { # cnvm sp leg cn dnidx
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "ana_state '$nqn' '${IP[$5]}'"
}

leg_state() { # cnvm sp leg cn dnidx
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "path_field '$nqn' '${IP[$5]}' State"
}

# leg_wait_ana is the §9 ordering barrier of a failover: a promoted CN may only
# converge once its legs have optimized paths, or md would find them
# unavailable (§11.1.1).
leg_wait_ana() { # cnvm sp leg cn dnidx want secs
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "wait_ana '$nqn' '${IP[$5]}' '$6' '$7'" ||
		die "cn$4's path to dn$5 of leg $3 never reached ANA state '$6'"
}

# ---------------------------------------------------------------------------
# Data IO on the host VM (§9): writes fsync, reads follow a cache drop, never
# iflag=/oflag= (§4).
# ---------------------------------------------------------------------------

drop_caches() { sshv "$1" "sync; echo 3 > /proc/sys/vm/drop_caches"; }

sha_range() { # vm path countMiB [skipMiB]
	sshv "$1" "dd if=$2 bs=1M count=$3 skip=${4:-0} status=none | sha256sum | cut -d' ' -f1"
}

write_range() { # vm src dst countMiB [seekMiB]
	sshv "$1" "dd if=$2 of=$3 bs=1M count=$4 seek=${5:-0} conv=fsync status=none; sync"
}

make_pattern() { # vm path countMiB
	sshv "$1" "dd if=/dev/urandom of=$2 bs=1M count=$3 conv=fsync status=none"
}

# ---------------------------------------------------------------------------
# The §8 --req request files
# ---------------------------------------------------------------------------
#
# SyncupCntlrRequest is far too deep for flags, so every converge is a
# protojson file under $WORK **on the driver** (cnagentctl runs here, not on
# the VMs). A case writes the file once with req_none/req_raid1 and from then
# on edits only the fields that change (§8) with req_set: revision, sp_level,
# cntlr.primary, ns_list[].suspended, and the clone and xfer lists.
#
# protojson wants original snake_case names, 64-bit integers as decimal strings
# and enum value names; the id_to_slice key is sprintf("%016x", slice_id) per
# §9.3 and the nqn_to_subsystem key is the literal NQN.

req_tr() { # traddr
	printf '{"tr_type": "tcp", "adr_fam": "ipv4", "tr_addr": "%s", "tr_svc_id": "%s"}' \
		"$1" "$TR_SVC_ID"
}

req_cntlr() { # cnidx cntlid_slot primary
	printf '{"addr_port": "%s:%s", "nvme_tr_conf": %s, "cntlid_slot": %s, "primary": %s, "disabled": false}' \
		"${IP[$1]}" "$CN_GRPC_PORT" "$(req_tr "${IP[$1]}")" "$2" "$3"
}

# req_side renders one Side. provisioned is always true here: dn_side has
# already run the two-phase §9.4 provisioning before any CN sees the side, so
# the desired state the sp-worker would publish at this point already carries
# its flip. Without the flag every leg would be provisioning-deferred and no
# case would build anything at all (update_01.md U4, ruling R4.34).
req_side() { # sideid dnidx
	printf '{"side_id": "%s", "addr_port": "%s:%s", "cntlid_slot": 0, "nvme_tr_conf": %s, "provisioned": true}' \
		"$(d16 "$1")" "${IP[$2]}" "$DN_GRPC_PORT" "$(req_tr "${IP[$2]}")"
}

req_leg() { # legid leg_idx side_json…
	local leg=$1 idx=$2
	shift 2
	printf '{"leg_id": "%s", "leg_idx": %s, "side_list": [%s]}' \
		"$(d16 "$leg")" "$idx" "$(join_json "$@")"
}

req_grp() { # grpid ext_cnt meta_blocks data_blocks leg_json…
	local grp=$1 ext=$2 meta=$3 data=$4
	shift 4
	printf '{"grp_id": "%s", "ext_cnt": "%s", "meta_blocks": "%s", "data_blocks": "%s", "leg_list": [%s]}' \
		"$(d16 "$grp")" "$ext" "$meta" "$data" "$(join_json "$@")"
}

# created defaults to false — the state a td is in between CreateThinDevice
# and the sp-worker's materialization flip (ThinDeviceCreated.md U3). A
# request carrying `created: true` is the one a real worker publishes after
# the flip, and the agent then sends no pool message for that td at all.
req_td() { # tdid dev_id ori_id [created]
	printf '{"td_id": "%s", "dev_id": %s, "ori_id": %s, "size": "%s", "created": %s}' \
		"$(d16 "$1")" "$2" "$3" "$TD_SIZE" "${4:-false}"
}

# req_ns renders one Namespace. nguid is always the uuid with the dashes
# removed (§5).
req_ns() { # nsid ns_idx tdid uuid suspended
	printf '{"ns_id": "%s", "ns_idx": %s, "td_id": "%s", "dev_uuid": "%s", "dev_nguid": "%s", "suspended": %s}' \
		"$(d16 "$1")" "$2" "$(d16 "$3")" "$4" "${4//-/}" "$5"
}

# req_subsys renders one Subsystem. serial/model are what the gateway stamps
# per [D2]; the agent consumes them verbatim.
req_subsys() { # ssid allowed_hosts_json ns_json…
	local ss=$1 hosts=$2
	shift 2
	printf '{"ss_id": "%s", "serial": "%s", "model": "dnv", "allowed_hosts": %s, "ns_list": [%s]}' \
		"$(d16 "$ss")" "$(hex16 "$ss")" "$hosts" "$(join_json "$@")"
}

req_xfer() { # xferid ori_nqn ori_ns_idx allowed_hosts_json auto_suspend
	printf '{"xfer_id": "%s", "ori_nqn": "%s", "ori_ns_idx": %s, "allowed_hosts": %s, "auto_suspend": %s}' \
		"$(d16 "$1")" "$2" "$3" "$4" "$5"
}

req_clone() { # cloneid src_nqn src_vm dst_tdid bm_cnt auto_resume
	printf '{"clone_id": "%s", "src_tr_conf_list": [%s], "src_nqn": "%s", "src_ns_idx": 1, "src_slice_cnt": 1, "src_stripe_size": "%s", "src_block_size": "%s", "dst_td_id": "%s", "dm_clone_conf": {"hydration_threshold": 1, "hydration_batch_size": 1}, "auto_resume": %s, "bm_cnt": %s}' \
		"$(d16 "$1")" "$(req_tr "${IP[$3]}")" "$2" "$STRIPE_SIZE" \
		"$BLOCK_SIZE" "$(d16 "$4")" "$6" "$5"
}

join_json() {
	local IFS=,
	printf '%s' "$*"
}

# req_none writes the §8 request verbatim: the S-shaped RedundNone SP, one
# primary cntlr, one td, one subsystem with one namespace.
req_none() { # file cnidx dnidx sp cntlr slot ss_nqn uuid suspended
	local file=$1 cn=$2 dn=$3 sp=$4 cntlr=$5 slot=$6 nqn=$7 uuid=$8 susp=$9
	cat >"$file" <<EOF
{
  "cluster_id": "$(d16 "$CLUSTER")", "cn_id": "$(d16 "${CNID[$cn]}")",
  "cntlr_pointer": {"sp_id": "$(d16 "$sp")", "cntlr_id": "$(d16 "$cntlr")"},
  "revision": "0",
  "bdev_conf": {
    "dm_pool_conf": {"data_block_size": "$BLOCK_SIZE",
                     "low_water_mark_pct": $LOW_WATER_PCT},
    "dm_raid0_conf": {"stripe_size": "$STRIPE_SIZE"},
    "redund_conf": {"redund_none": {}}
  },
  "sp_level": "SP_LEVEL_READWRITE",
  "cntlr": $(req_cntlr "$cn" "$slot" true),
  "id_to_slice": {
    "$(hex16 "$S_SLICE")": {
      "slice_idx": 0,
      "meta_grp_list": [$(req_grp "$S_MGRP" 1 "$NONE_META_BLOCKS" "$NONE_EXT1_DATA" \
		"$(req_leg "$S_MLEG" 0 "$(req_side "$S_MSIDE" "$dn")")")],
      "data_grp_list": [$(req_grp "$S_DGRP" 2 "$NONE_META_BLOCKS" "$NONE_EXT2_DATA" \
			"$(req_leg "$S_DLEG" 0 "$(req_side "$S_DSIDE" "$dn")")")]
    }
  },
  "td_list": [$(req_td "$S_TD" 1 0)],
  "nqn_to_subsystem": {
    "$nqn": $(req_subsys "$S_SS" "[]" \
			"$(req_ns "$S_NS" 1 "$S_TD" "$uuid" "$susp")")
  }
}
EOF
}

# req_raid1 writes the A-shaped request: md-raid1 meta and data groups whose
# legs live one per DN, and one cntlr that is either the primary or a standby.
# allowed_hosts is the emulated host role's NQN (§5 case A).
req_raid1() { # file cnidx sp cntlr slot primary ss_nqn uuid
	local file=$1 cn=$2 sp=$3 cntlr=$4 slot=$5 primary=$6 nqn=$7 uuid=$8
	cat >"$file" <<EOF
{
  "cluster_id": "$(d16 "$CLUSTER")", "cn_id": "$(d16 "${CNID[$cn]}")",
  "cntlr_pointer": {"sp_id": "$(d16 "$sp")", "cntlr_id": "$(d16 "$cntlr")"},
  "revision": "0",
  "bdev_conf": {
    "dm_pool_conf": {"data_block_size": "$BLOCK_SIZE",
                     "low_water_mark_pct": $LOW_WATER_PCT},
    "dm_raid0_conf": {"stripe_size": "$STRIPE_SIZE"},
    "redund_conf": {"redund_md_raid1":
                    {"bitmap_chunk_block_cnt": "$BM_CHUNK_BLOCKS"}}
  },
  "sp_level": "SP_LEVEL_READWRITE",
  "cntlr": $(req_cntlr "$cn" "$slot" "$primary"),
  "id_to_slice": {
    "$(hex16 "$A_SLICE")": {
      "slice_idx": 0,
      "meta_grp_list": [$(req_grp "$A_MGRP" 1 "$RAID1_META_BLOCKS" "$RAID1_EXT1_DATA" \
		"$(req_leg "${A_MLEG[1]}" 0 "$(req_side "${A_MSIDE[1]}" 1)")" \
		"$(req_leg "${A_MLEG[2]}" 1 "$(req_side "${A_MSIDE[2]}" 2)")")],
      "data_grp_list": [$(req_grp "$A_DGRP" 2 "$RAID1_META_BLOCKS" "$RAID1_EXT2_DATA" \
			"$(req_leg "${A_DLEG[1]}" 0 "$(req_side "${A_DSIDE[1]}" 1)")" \
			"$(req_leg "${A_DLEG[2]}" 1 "$(req_side "${A_DSIDE[2]}" 2)")")]
    }
  },
  "td_list": [$(req_td "$A_TD" 1 0)],
  "nqn_to_subsystem": {
    "$nqn": $(req_subsys "$A_SS" "[\"$HOST_NQN\"]" \
			"$(req_ns "$A_NS" 1 "$A_TD" "$uuid" false)")
  }
}
EOF
}

# req_set edits one request file in place. Only the fields a step changes are
# ever touched (§8), which is why the edit is a jq assignment and not a
# regenerated file.
req_set() { # file jq-filter
	local tmp="$1.tmp"
	"$JQ" "$2" "$1" >"$tmp" || die "editing $1 failed: $2"
	mv "$tmp" "$1"
}

# cn_syncup_cntlr posts one request file at the CN's current revision. The
# caller bumps the counter first, in the parent shell (§9), so this is safe
# inside a command substitution.
cn_syncup_cntlr() { # cnidx file
	req_set "$2" ".revision = \"${CNREV[$1]}\""
	cnctl "$1" syncup-cntlr --req "$2"
}

# ---------------------------------------------------------------------------
# The VM helper. Shipped to /var/tmp (outside $WORK, which cleanup removes) so
# the quoting of the probing/teardown loops lives in one readable place.
# ---------------------------------------------------------------------------

vm_helper_source() {
	cat <<'HELPER_EOF'
#!/usr/bin/env bash
# Shipped by integtest/cnagent_test.sh. Every function is best-effort by
# design: cleanup must survive a crashed prior run.
WORK=/var/tmp/dnv-cn-integtest
DN_LOG=$WORK/dn-agent.log
CN_LOG=$WORK/cn-agent.log
NVMET=/sys/kernel/config/nvmet
NQN_PREFIX=nqn.2024-01.io.dnv
NQN_IT_PREFIX=nqn.2024-01.io.dnv-it
UDEV_RULE=/etc/udev/rules.d/63-dnv-md.rules
TMPFS_DIR=/tmp/dnv-tmpfs

subsys_json() {
	local json
	json=$(nvme list-subsys -o json 2>/dev/null)
	[ -n "${json//[[:space:]]/}" ] || json='[]'
	printf '%s' "$json"
}

# path_field <nqn> <traddr> <field> — one field of the path a host holds to
# one target address, e.g. State (live/connecting) or Name.
path_field() {
	subsys_json | jq -r --arg nqn "$1" --arg a "$2" --arg f "$3" '
	    [ .. | objects | select(has("NQN") and .NQN == $nqn) | .Paths[]?
	      | select([.Address | split(",")[] | select(startswith("traddr="))]
	               == ["traddr=" + $a])
	      | .[$f] ] | first // "none"'
}

# ana_state <nqn> <traddr> — the ANA state of the path this host holds to one
# target address. nvme-cli 2.16's `list-subsys -o json` does not carry it
# (only `show-topology` does, keyed by namespace), so it is read straight off
# the per-path sysfs attribute of the controller list-subsys names.
ana_state() {
	local ctrl attr
	ctrl=$(path_field "$1" "$2" Name)
	[ "$ctrl" != none ] || {
		echo none
		return 0
	}
	for attr in /sys/class/nvme/"$ctrl"/nvme*n*/ana_state; do
		[ -r "$attr" ] || continue
		cat "$attr"
		return 0
	done
	echo none
}

wait_ana() { # <nqn> <traddr> <want> <secs>
	local got=none i
	for ((i = 0; i < $4 * 2; i++)); do
		got=$(ana_state "$1" "$2")
		if [ "$got" = "$3" ]; then
			echo "$got"
			return 0
		fi
		sleep 0.5
	done
	echo "ana state via $2 is '$got', want '$3'" >&2
	return 1
}

wait_dev() { # <path> <secs>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if [ -e "$1" ]; then
			echo "$1"
			return 0
		fi
		sleep 0.5
	done
	echo "device $1 did not appear within $2s" >&2
	return 1
}

wait_gone() { # <path> <secs>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		[ -e "$1" ] || {
			echo gone
			return 0
		}
		sleep 0.5
	done
	echo "$1 still present after $2s" >&2
	return 1
}

# read_probe <dev> — prints ok or eio; the caller asserts which.
read_probe() {
	if dd if="$1" of=/dev/null bs=1M count=1 status=none 2>/dev/null; then
		echo ok
	else
		echo eio
	fi
}

# write_probe <dev> <seekMiB> — prints ok or eio. The [D11] readonly table
# errors writes; conv=fsync is what turns that into a non-zero dd (§4).
write_probe() {
	if dd if=/dev/zero of="$1" bs=1M count=1 seek="$2" conv=fsync \
		status=none 2>/dev/null; then
		echo ok
	else
		echo eio
	fi
}

# zero_probe <dev> <skipMiB> <countMiB> — prints zeros or data.
zero_probe() {
	local tmp=/var/tmp/dnv-cn-zero.$$ out=data
	dd if=/dev/zero of="$tmp" bs=1M count="$3" status=none 2>/dev/null
	if dd if="$1" bs=1M skip="$2" count="$3" status=none 2>/dev/null |
		cmp -s - "$tmp"; then
		out=zeros
	fi
	rm -f "$tmp"
	echo "$out"
}

# dm_state <name> — live, suspended or missing.
dm_state() {
	local attr
	attr=$(dmsetup info -c --noheadings -o attr "$1" 2>/dev/null | tr -d ' ')
	case "$attr" in
	"") echo missing ;;
	*s*) echo suspended ;;
	*) echo live ;;
	esac
}

dm_table() { dmsetup table "$1" 2>/dev/null || echo MISSING; }

# dm_devno <name> — the major:minor of one dm device. dm tables reference
# their backing devices this way, so it is what a backing assertion compares.
dm_devno() {
	local mm
	mm=$(dmsetup info -c --noheadings -o major,minor "$1" 2>/dev/null | tr -d ' ')
	if [ -n "$mm" ]; then echo "$mm"; else echo none; fi
}

# dm_backing <name> — the device a single-target linear or flakey table points
# at: `0 <len> linear <major:minor> <offset>` and `0 <len> flakey <major:minor>
# …` both carry it in field 4.
dm_backing() { dm_table "$1" | awk '{print $4}'; }

ss_attr() { cat "$NVMET/subsystems/$1/$2" 2>/dev/null || echo MISSING; }

ns_attr() { cat "$NVMET/subsystems/$1/namespaces/$2/$3" 2>/dev/null || echo MISSING; }

allowed_hosts() { ls "$NVMET/subsystems/$1/allowed_hosts" 2>/dev/null || true; }

port_linked() {
	if [ -e "$NVMET/ports/1/subsystems/$1" ]; then echo linked; else echo no; fi
}

mdstat() { cat /proc/mdstat 2>/dev/null || true; }

# ctrl_of <nqn> <traddr> — the controller device backing one path, so a dead
# path can be disconnected by device instead of by NQN (Appendix A).
ctrl_of() { path_field "$1" "$2" Name; }

# --- log readers -------------------------------------------------------------
#
# The agent stamps every record with the trace id of the request that caused
# it, so an ordered event stream of one converge is exactly one trace id. The
# startup reconcile and the CN11 probers mint their own ids, which is why the
# recovery and restart assertions read a freshly rotated log with no filter.

# events <log> [trace] — os commands as "cmd args…" and configfs attribute
# writes as "write <path> <data>", in log order.
events() {
	jq -r --arg t "${2:-}" '
	    select($t == "" or .trace_id == $t)
	    | if .msg == "os command" then
	        .cmd + " " + ((.args // []) | join(" "))
	      elif .msg == "os write file direct" then
	        "write " + .path + " " + (.data // "")
	      else empty end' "$1" 2>/dev/null || true
}

cn_events() { events "$CN_LOG" "${1:-}"; }

# mutations [trace] [log] — every mutating operation in a cn agent log (§9,
# case D step 5). An empty trace means the whole log; naming one scopes the
# answer to a single converge, which is what lets a stage prove that *its own*
# syncup mutated nothing but dm devices (`ThinDeviceCreated.md` U5-T1). The CN11 leg health probers left the OsClient in update_01.md U2:
# they call the raw block-IO syscalls directly and log their own records as
# `probe write block` / `probe read block direct`, which are not in this grep
# list by construction. So no path-based exemption is needed any more, and
# `os write block` — the one block-IO msg an OsClient still emits
# (common/osclient.go:335) — is a mutation without qualification. There is no
# read-side msg left to grep for: U2 deleted `OsClient.ReadBlockDirect` and
# its log record outright, and the package-level `ReadBlockDirectAt` that
# replaced it logs nothing at all (osclient.md §4.5.1, §8 acceptance).
# Probe commands (lsblk, dmsetup info|table|status|ls, ls, findmnt, stat,
# losetup --associated, mdadm --detail|--examine, nvme list-subsys) are
# expected and deliberately not in the list.
mutations() {
	local trace=${1:-} log=${2:-$CN_LOG}
	jq -r --arg t "$trace" '
	    select($t == "" or .trace_id == $t)
	    | select(.msg == "os write file direct")
	      | "write file direct " + .path' "$log" 2>/dev/null || true
	# The verb tests bind .args first: a `[…] | index(.cmd)` would evaluate
	# .cmd against the literal array, not the record.
	jq -r --arg t "$trace" '
	    select($t == "" or .trace_id == $t)
	    | select(.msg == "os command")
	    | (.args // []) as $a
	    | select(
	        (.cmd == "dmsetup" and ($a[0] | IN("create","reload","remove",
	              "suspend","resume","message")))
	        or (.cmd == "mdadm" and ($a | any(IN("--create","--assemble",
	              "--add","--fail","--remove","--stop","--zero-superblock"))))
	        or (.cmd == "nvme" and ($a[0] | IN("connect","disconnect")))
	        or (.cmd == "losetup" and (($a | index("--associated")) | not))
	        or (.cmd | IN("blkdiscard","mount","umount","truncate",
	              "mkdir","rmdir","ln","rm"))
	      )
	    | .cmd + " " + ($a | join(" "))' "$log" 2>/dev/null || true
	jq -r --arg t "$trace" '
	    select($t == "" or .trace_id == $t)
	    | select(.msg == "os write block")
	    | .msg + " " + .path' "$log" 2>/dev/null || true
}

# --- residue -----------------------------------------------------------------

# residue <sp16> — everything either agent still holds for one storage pool: dm
# devices, md arrays and nvmet subsystems. The teardown assertions require empty
# output. The dm pattern's [0-9a-f] kind digit already covers the kind-b
# clone-metadata wrappers that replaced the clone VG's LVs (update_01.md U3), so
# a leaked arena allocation is caught here for free.
residue() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[0-9a-f]-$1-" || true
	ls "$NVMET/subsystems" 2>/dev/null | grep -E ":$1:" || true
	mdadm --detail --scan 2>/dev/null | grep -oE "name=[^ ]*dnv-$1-[0-9a-f]+-[0-9a-f]+" || true
}

# cn_residue <cn16> — every dm device of one CN plus the test's host-facing
# subsystems (the §5 ss NQNs carry no id, so they are matched by prefix).
cn_residue() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E "^dnv-[0-9a-f]{16}-$1-" || true
	ls "$NVMET/subsystems" 2>/dev/null | grep -F "$NQN_IT_PREFIX" || true
}

# --- setup / teardown --------------------------------------------------------

# install_udev_rule masks the stock incremental md assembly for dnv arrays
# (§7 step 2, Appendix A): the stock rule honors SYSTEMD_READY, so setting it
# to 0 leaves the agent as the only assembler. Removed again by cleanup — the
# VMs are shared lab machines.
install_udev_rule() {
	cat >"$UDEV_RULE" <<'RULE_EOF'
ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
  IMPORT{program}="/sbin/mdadm --examine --export $devnode"
ENV{MD_NAME}=="dnv-*|*:dnv-*", ENV{SYSTEMD_READY}="0"
RULE_EOF
	udevadm control --reload >/dev/null 2>&1
	echo installed
}

kill_role() { # <dn|cn>
	local i
	pkill -f "dnv-agent $1" >/dev/null 2>&1
	for ((i = 0; i < 20; i++)); do
		pgrep -f "dnv-agent $1" >/dev/null 2>&1 || break
		sleep 0.25
	done
	pkill -9 -f "dnv-agent $1" >/dev/null 2>&1
	if pgrep -f "dnv-agent $1" >/dev/null 2>&1; then
		echo STILL_RUNNING
	else
		echo stopped
	fi
}

agent_dm_names() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E '^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[0-9a-f]-' || true
}

# dm_kind_names <kind> [node16] — the dm devices of one kind, optionally of one
# node only. Both roles name their devices dnv-{cluster}-{node}-{kind}-…, and
# the kind digits overlap, so every teardown pass that must not touch the other
# role's devices passes the node id.
dm_kind_names() {
	agent_dm_names | awk -F- -v k="$1" -v n="${2:-}" \
		'$4 == k && (n == "" || $3 == n)'
}

# clone_meta_wrappers [cn16] — the kind-b clone-metadata dm-linears, which are
# the arena's allocation registry itself (update_01.md U3 spec 3: there is no
# on-file allocation table, the dm tables are it). Empty output means every
# unit of the arena is free.
clone_meta_wrappers() { dm_kind_names b "${1:-}"; }

# resume_suspended sweeps up suspended dm devices before anything reads them.
# It is load-bearing, not defensive: a transfer's origin ns-dev is deliberately
# suspended (CN16) and a dn cutover may hold linears suspended. Anything that
# reads a suspended device (`dmsetup remove`, disabling the nvmet namespace
# above it, and above all a block-device scan) blocks in uninterruptible D
# state and wedges the node until reboot. The pattern is deliberately looser
# than agent_dm_names' so it also sweeps up debris a pre-U3 run left behind on
# a shared lab VM — including LVM's own doubled-dash nodes (dnv--clone--vg-*),
# which no current run can produce.
resume_suspended() {
	local row name
	for row in $(dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E '^dnv[-a-z0-9]*.*:.-s'); do
		name=${row%%:*}
		timeout 10 dmsetup resume "$name" >/dev/null 2>&1
	done
	return 0
}

# dm_force_remove removes one device, falling back to --force (which swaps in
# an error table when the device is still open) rather than blocking.
dm_force_remove() {
	timeout 10 dmsetup remove "$1" >/dev/null 2>&1 && return 0
	timeout 10 dmsetup resume "$1" >/dev/null 2>&1
	timeout 15 dmsetup remove --force --retry "$1" >/dev/null 2>&1
	return 0
}

dm_remove_kind() { # <kind> [node16]
	local name
	for name in $(dm_kind_names "$1" "${2:-}"); do
		dm_force_remove "$name"
	done
}

# disconnect_prefix <nqn prefix> — every connection whose subsystem NQN starts
# with the prefix. Whole-NQN disconnects are only ever used here, where every
# path of the NQN is being retired (Appendix A).
disconnect_prefix() {
	local nqn
	for nqn in $(subsys_json | jq -r --arg p "$1" '
	        [.. | objects | select(has("NQN")) | .NQN] | unique | .[]
	        | select(startswith($p))'); do
		timeout 30 nvme disconnect -n "$nqn" >/dev/null 2>&1
	done
}

# drop_subsys_glob <nqn glob> — unlink from the port, disable and remove the
# namespaces, unlink the allowed hosts, remove the subsystem. Namespaces must
# be disabled before their backing dm devices can go.
drop_subsys_glob() {
	local subsys ns host link
	[ -d "$NVMET" ] || return 0
	for link in "$NVMET"/ports/*/subsystems/$1; do
		[ -e "$link" ] && rm -f "$link"
	done
	for subsys in "$NVMET"/subsystems/$1; do
		[ -d "$subsys" ] || continue
		for ns in "$subsys"/namespaces/*; do
			[ -d "$ns" ] || continue
			echo 0 >"$ns/enable" 2>/dev/null
			rmdir "$ns" 2>/dev/null
		done
		for host in "$subsys"/allowed_hosts/*; do
			[ -e "$host" ] && rm -f "$host"
		done
		rmdir "$subsys" 2>/dev/null
	done
	return 0
}

# md_stop_all stops every array this suite created: mdadm --detail --scan
# names them, and the agent's --homehost any means the name may or may not
# carry a homehost prefix.
md_stop_all() {
	local kw dev rest
	while read -r kw dev rest; do
		[ "$kw" = ARRAY ] || continue
		case "$rest" in
		*name=dnv-* | *name=*:dnv-*) ;;
		*) continue ;;
		esac
		timeout 15 mdadm --stop "$dev" >/dev/null 2>&1
	done < <(mdadm --detail --scan 2>/dev/null)
	return 0
}

tmpfs_teardown() {
	local dev mnt
	for dev in $(losetup -a 2>/dev/null | grep "$TMPFS_DIR/" | cut -d: -f1); do
		timeout 15 losetup -d "$dev" >/dev/null 2>&1
	done
	for mnt in "$TMPFS_DIR"/*; do
		[ -d "$mnt" ] || continue
		timeout 15 umount "$mnt" >/dev/null 2>&1
		rmdir "$mnt" 2>/dev/null
	done
	rmdir "$TMPFS_DIR" 2>/dev/null
	return 0
}

loop_devs() {
	{
		losetup -j "$WORK/backing.img" 2>/dev/null | cut -d: -f1
		losetup -a 2>/dev/null | grep dnv-cn-integtest | cut -d: -f1
	} | sort -u
}

# wipe_cn simulates the §13 stage 6 CN reboot: every kernel object of one CN
# goes, including the volatile clone-metadata arena, while the dn objects, the
# shared port and $WORK/cn-store stay. The order is the §16 order, and the
# dm-clone is removed while its :4: source connection is still up — a clone
# flushes through its source on removal and blocks without it.
#
# Kind b (the clone-metadata wrappers that replaced the clone VG) joins the dm
# passes rather than getting a teardown of its own (update_01.md U3, ruling
# R5.7). It must come after kind 7: the dm-clone holds its wrapper open, and a
# wrapper left behind holds the loop device open, wedging tmpfs_teardown's
# `losetup -d` with EBUSY.
wipe_cn() { # <cn16>
	resume_suspended
	drop_subsys_glob "$NQN_IT_PREFIX:*"
	dm_remove_kind 6 "$1"
	dm_remove_kind 8 "$1"
	dm_remove_kind 7 "$1"
	disconnect_prefix "$NQN_PREFIX:4:"
	disconnect_prefix "$NQN_PREFIX:2:"
	local kind
	for kind in 5 4 3 2 1 0 a 9 b; do
		dm_remove_kind "$kind" "$1"
	done
	md_stop_all
	tmpfs_teardown
	echo wiped
}

# cleanup_phase1 and cleanup_phase2 implement §16. The split is what makes the
# cross-VM ordering safe: a case C clone on one VM holds an nvme connection to
# a transfer on the other, so every clone is removed (phase 1) before any
# transfer subsystem is (phase 2).
cleanup_phase1() { # <cn16>
	kill_role cn >/dev/null
	kill_role dn >/dev/null

	# Nothing may stay suspended from here on (see resume_suspended).
	resume_suspended

	# nvmet controllers must die before nvmet teardown.
	disconnect_prefix "$NQN_IT_PREFIX:"
	drop_subsys_glob "$NQN_IT_PREFIX:*"

	dm_remove_kind 6 "${1:-}"
	dm_remove_kind 8 "${1:-}"
	dm_remove_kind 7 "${1:-}"
	echo phase1
}

cleanup_phase2() { # <cn16> <dn16>
	local cn16=${1:-} dn16=${2:-} kind name

	# The transfers the clones of phase 1 were sourced from.
	disconnect_prefix "$NQN_PREFIX:4:"
	drop_subsys_glob "$NQN_PREFIX:4:*"

	# cn dm pass 2, top-down, then the arrays, then the leg wrappers.
	for kind in 5 4 3 2 1 0; do
		dm_remove_kind "$kind" "$cn16"
	done
	md_stop_all
	dm_remove_kind a "$cn16"
	dm_remove_kind 9 "$cn16"
	# The clone-metadata wrappers, after their dm-clones went in phase 1: a
	# leftover kind-b holds the loop device open and wedges the `losetup -d`
	# below with EBUSY (update_01.md U3, ruling R5.7).
	dm_remove_kind b "$cn16"
	disconnect_prefix "$NQN_PREFIX:2:"

	tmpfs_teardown
	resume_suspended

	# The dn suite's §16 sequence, now that nothing connects to the sides.
	drop_subsys_glob "$NQN_PREFIX:2:*"
	dm_remove_kind 1 "$dn16"
	dm_remove_kind 3 "$dn16"
	disconnect_prefix "$NQN_PREFIX:3:"
	drop_subsys_glob "$NQN_PREFIX:3:*"
	dm_remove_kind 5 "$dn16"
	dm_remove_kind 2 "$dn16"
	dm_remove_kind 0 "$dn16"
	dm_remove_kind 4 "$dn16"
	for name in $(agent_dm_names); do
		dm_force_remove "$name"
	done

	if [ -d "$NVMET" ]; then
		local host grp
		for host in "$NVMET"/hosts/"$NQN_PREFIX":* "$NVMET"/hosts/"$NQN_IT_PREFIX":*; do
			[ -d "$host" ] && rmdir "$host" 2>/dev/null
		done
		for grp in 3 2; do
			rmdir "$NVMET/ports/1/ana_groups/$grp" 2>/dev/null
		done
		rmdir "$NVMET/ports/1" 2>/dev/null
	fi

	resume_suspended

	# Unformat each dn loop device: zeroing the 4 KiB header is enough,
	# because the volume-table slots are inert without it ([D13] §5.2). No
	# oflag=, per the §4 dd rule.
	local dev
	for dev in $(loop_devs); do
		dd if=/dev/zero of="$dev" bs=4096 count=1 conv=fsync >/dev/null 2>&1
		wipefs -a "$dev" >/dev/null 2>&1
		losetup -d "$dev" >/dev/null 2>&1
	done
	rm -rf "$WORK"
	rm -f "$UDEV_RULE"
	udevadm control --reload >/dev/null 2>&1
	echo cleaned
	return 0
}

diag() {
	echo "--- dn-agent.log (last 120 lines) ---"
	tail -n 120 "$DN_LOG" 2>/dev/null
	echo "--- cn-agent.log (last 120 lines) ---"
	tail -n 120 "$CN_LOG" 2>/dev/null
	echo "--- dmsetup ls ---"
	dmsetup ls 2>/dev/null
	echo "--- dmsetup table ---"
	dmsetup table 2>/dev/null
	echo "--- dmsetup status ---"
	dmsetup status 2>/dev/null
	echo "--- /proc/mdstat ---"
	cat /proc/mdstat 2>/dev/null
	echo "--- mdadm --detail --scan ---"
	mdadm --detail --scan 2>/dev/null
	echo "--- clone-meta wrappers (kind b) ---"
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E '^dnv-[0-9a-f]{16}-[0-9a-f]{16}-b-' || true
	echo "--- losetup -a ---"
	losetup -a 2>/dev/null
	echo "--- tmpfs mounts ---"
	findmnt 2>/dev/null | grep dnv-tmpfs
	echo "--- nvmet configfs ---"
	ls -R "$NVMET/ports" "$NVMET/subsystems" 2>/dev/null
	echo "--- nvme list-subsys ---"
	subsys_json
	return 0
}

"$@"
HELPER_EOF
}

ship_helper() {
	local tmp
	tmp=$(mktemp)
	vm_helper_source >"$tmp"
	local idx
	for idx in 1 2; do
		log "[vm$idx] scp helper -> $HELPER"
		scp -q "${SSH_OPTS[@]}" "$tmp" "${VM[$idx]}:$HELPER"
	done
	rm -f "$tmp"
}

# cleanup_all runs §16 as two cross-VM phases: within a phase the two VMs run
# concurrently, but no VM starts phase 2 until both finished phase 1, because
# a clone on one VM flushes through a transfer export on the other.
cleanup_all() {
	local idx pid pids=()
	for idx in 1 2; do
		helper_ok "$idx" "cleanup_phase1 $(hex16 "${CNID[$idx]}")" &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do wait "$pid" || true; done
	pids=()
	for idx in 1 2; do
		helper_ok "$idx" \
			"cleanup_phase2 $(hex16 "${CNID[$idx]}") $(hex16 "${DNID[$idx]}")" &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do wait "$pid" || true; done
}

diagnostics() {
	local idx spec sp cntlr rest
	for idx in 1 2; do
		log ""
		log "########## vm$idx ##########"
		helper_ok "$idx" diag >&2
	done
	if [ "${#DIAG_CNTLRS[@]}" -gt 0 ]; then
		log ""
		log "########## cntlr infos ##########"
		for spec in "${DIAG_CNTLRS[@]}"; do
			idx=${spec%%:*}
			rest=${spec#*:}
			sp=${rest%%:*}
			cntlr=${rest##*:}
			cnctl "$idx" get-cntlr-info --sp "$sp" --cntlr "$cntlr" >&2 || true
		done
	fi
}

# diag_cntlr registers one cntlr for the §17 dump. Cases reset the list.
diag_cntlr() { # cnidx sp cntlr
	DIAG_CNTLRS+=("$1:$2:$3")
}

# ---------------------------------------------------------------------------
# Preflight (§4)
# ---------------------------------------------------------------------------

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing: $1 on the driver"
}

# resolve_jq picks the driver's JSON parser. A system jq is used as-is; on a
# box without one, the drop-in gojq is built into the gitignored
# integtest/bin with the Go toolchain the driver already needs, rather than
# installing a package.
resolve_jq() {
	if command -v jq >/dev/null 2>&1; then
		JQ=jq
		return
	fi
	JQ="$REPO_ROOT/integtest/bin/gojq"
	[ -x "$JQ" ] || GOBIN="$REPO_ROOT/integtest/bin" GOFLAGS=-mod=mod \
		go install github.com/itchyny/gojq/cmd/gojq@v0.12.17 >&2 ||
		die "missing: jq on the driver, and building gojq failed"
	log "driver json parser: $JQ (no system jq)"
}

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp sha256sum; do need_local "$tool"; done
	resolve_jq
	mkdir -p "$WORK"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$AGENT_BIN" ] || die "missing: $AGENT_BIN after make build"
	(cd "$REPO_ROOT" && go build -o "$CTL_DN" ./integtest/dnagentctl >&2) ||
		die "building dnagentctl failed"
	(cd "$REPO_ROOT" && go build -o "$CTL_CN" ./integtest/cnagentctl >&2) ||
		die "building cnagentctl failed"
}

# preflight_vms runs after the start-of-run cleanup: the port check can only
# be meaningful once a crashed prior run's agents are gone (§4).
preflight_vms() {
	STAGE="preflight (vms)"
	log "=== preflight: vms"
	local idx
	for idx in 1 2; do
		ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo -n true" ||
			die "missing: passwordless sudo on vm$idx (${VM[$idx]})"
		local missing
		# No LVM binaries: U3 removed LVM from the CN entirely. thin_dump stays
		# — it is the §12 thin-metadata oracle, not an LVM command.
		missing=$(sshv "$idx" "for b in dmsetup nvme losetup blkdiscard lsblk dd fallocate sha256sum cmp pkill jq timeout mdadm truncate stat findmnt thin_dump; do command -v \$b >/dev/null || echo \$b; done")
		[ -z "$missing" ] || die "missing: $missing on vm$idx"
		# The agents hardcode the configfs path and neither mount nor
		# modprobe; the harness does both here and nothing else.
		sshv "$idx" "for m in nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-clone dm-thin-pool dm-flakey raid1; do modprobe \$m 2>/dev/null || true; done; grep -q ' /sys/kernel/config ' /proc/mounts || mount -t configfs none /sys/kernel/config; true"
		local got
		got=$(sshv "$idx" "ls -d $NVMET 2>/dev/null || echo MISSING")
		assert_eq "$got" "$NVMET" "nvmet configfs on vm$idx"
		got=$(sshv "$idx" "ls /proc/mdstat 2>/dev/null || echo MISSING")
		assert_eq "$got" /proc/mdstat "md support on vm$idx"
		got=$(sshv "$idx" "cat /sys/module/nvme_core/parameters/multipath")
		assert_eq "$got" "Y" "nvme_core.multipath on vm$idx"
		# The §7 udev mask works by setting SYSTEMD_READY, which only helps
		# if the stock incremental-assembly rule honors it (Appendix A).
		got=$(sshv "$idx" "for d in /usr/lib/udev/rules.d /lib/udev/rules.d; do f=\$d/64-md-raid-assembly.rules; if [ -r \$f ] && grep -q SYSTEMD_READY \$f; then echo FOUND; break; fi; done; true")
		assert_eq "$got" FOUND \
			"vm$idx: 64-md-raid-assembly.rules honoring SYSTEMD_READY"
		got=$(sshv "$idx" "df -Pk /var/tmp | awk 'NR==2 {print \$4}'")
		[ "$got" -ge 3145728 ] ||
			die "vm$idx: /var/tmp has ${got}K free, want >= 3 GiB"
		got=$(sshv "$idx" "awk '/MemAvailable/ {print \$2}' /proc/meminfo")
		[ "$got" -ge 1572864 ] ||
			die "vm$idx: MemAvailable is ${got}K, want >= 1.5 GiB"
		got=$(sshv "$idx" "f=/var/tmp/dnv-punch-probe.\$\$; fallocate -l 8M \$f && fallocate -p -o 0 -l 4M \$f && echo PUNCH_OK || echo PUNCH_NO; rm -f \$f")
		assert_eq "$got" "PUNCH_OK" "vm$idx: /var/tmp punch-hole support"
		got=$(sshv "$idx" "ss -ltnH | awk '{print \$4}' | sed 's/.*://' | grep -cE '^($DN_GRPC_PORT|$CN_GRPC_PORT|$TR_SVC_ID)\$' || true")
		assert_eq "$got" "0" \
			"vm$idx: ports $DN_GRPC_PORT/$CN_GRPC_PORT/$TR_SVC_ID must be free"
	done
	log "preflight ok"
}

# ---------------------------------------------------------------------------
# Setup (§7)
# ---------------------------------------------------------------------------

start_dn_agent() { # <idx>
	local idx=$1
	sshv "$idx" "setsid nohup $WORK/dnv-agent dn \
--grpc-network tcp --grpc-address ${IP[$idx]}:$DN_GRPC_PORT \
--tr-type tcp --adr-fam ipv4 --tr-addr ${IP[$idx]} --tr-svc-id $TR_SVC_ID \
--local-store $WORK/dn-store --disk ${LOOP[$idx]} \
>> $DN_LOG 2>&1 < /dev/null & echo launched"
}

# The cn agent takes the same --tr-* flags as the dn one: both converge the
# same ports/1 with the same attributes, and EnsurePort is probe-first, so
# whichever runs first creates it and the other issues zero writes (§3).
start_cn_agent() { # <idx>
	local idx=$1
	sshv "$idx" "setsid nohup $WORK/dnv-agent cn \
--grpc-network tcp --grpc-address ${IP[$idx]}:$CN_GRPC_PORT \
--tr-type tcp --adr-fam ipv4 --tr-addr ${IP[$idx]} --tr-svc-id $TR_SVC_ID \
--local-store $WORK/cn-store --capacity $CN_CAPACITY \
>> $CN_LOG 2>&1 < /dev/null & echo launched"
}

setup() {
	CASE=setup
	stage vms "backing store, md udev mask, both agents on both VMs"
	local idx out wz
	for idx in 1 2; do
		# Both --local-store dirs must pre-exist: startup reconcile fails
		# without them.
		sshv "$idx" "mkdir -p $WORK/dn-store $WORK/cn-store && chmod 0777 $WORK && chmod 0755 $WORK/dn-store $WORK/cn-store"
		helper "$idx" install_udev_rule
		sshv "$idx" "fallocate -l 2G $WORK/backing.img"
		LOOP[idx]=$(sshv "$idx" "losetup --find --show $WORK/backing.img")
		log "[vm$idx] loop device ${LOOP[idx]}"
		# The §4 preflight item of update_01.md U4, deferred to here because
		# the device only exists now (preflight_vms runs before setup). The
		# §9.4 zeroing assumes fast Write Zeroes; a loop device maps
		# REQ_OP_WRITE_ZEROES onto fallocate, so a 0 here means the kernel
		# would write zero pages at bulk speed and the dn agent's DN5
		# fail-fast would refuse the disk outright. Read from /sys/class/block,
		# the same directory agent.Dm.WriteZeroesMaxBytes uses.
		wz=$(sshv "$idx" "cat /sys/class/block/\$(basename ${LOOP[$idx]})/queue/write_zeroes_max_bytes")
		[ "$wz" -gt 0 ] ||
			die "vm$idx: ${LOOP[$idx]} reports write_zeroes_max_bytes=0"
		log "[vm$idx] scp dnv-agent"
		scp -q "${SSH_OPTS[@]}" "$AGENT_BIN" "${VM[$idx]}:$WORK/dnv-agent"
		sshv "$idx" "chmod 0755 $WORK/dnv-agent"
		start_dn_agent "$idx"
		start_cn_agent "$idx"
	done
	SETUP_DONE=1

	stage dnbase "the dn backing is healthy before any cn assertion runs"
	for idx in 1 2; do
		out=$(dnctl "$idx" get-dn-size --wait 15)
		assert_eq "$(jq_of "$out" .size)" "$DATA_SIZE" "vm$idx GetDnSize"
		bump_dn_rev "$idx"
		out=$(dnctl "$idx" syncup-dn \
			--revision "${DNREV[$idx]}" --extent-size "$EXTENT_SIZE")
		local field
		for field in disk_info meta_info port_info; do
			assert_ok "$out" ".dn_info.$field.status" "dn$idx baseline"
		done
	done

	stage cnbase "GetCnSize is lock-free, so it doubles as the liveness probe"
	for idx in 1 2; do
		out=$(cnctl "$idx" get-cn-size --wait 15)
		assert_eq "$(jq_of "$out" .size)" "$CN_CAPACITY" "vm$idx GetCnSize"
		bump_cn_sync "$idx"
		out=$(cnctl "$idx" syncup-cn --revision "${CNREV[$idx]}")
		assert_cn_info_ok "$out" "cn$idx baseline"
	done
}

# ---------------------------------------------------------------------------
# Shared case helpers
# ---------------------------------------------------------------------------

# dn_pointers introduces a DN's full side list (§9 ordering: a side pointer
# exists before its first SyncupSide).
dn_pointers() { # dnidx sp:leg:side…
	local idx=$1 spec args=() out field
	shift
	for spec in "$@"; do args+=(--side "$spec"); done
	bump_dn_rev "$idx"
	out=$(dnctl "$idx" syncup-dn --revision "${DNREV[$idx]}" \
		--extent-size "$EXTENT_SIZE" "${args[@]}")
	for field in disk_info meta_info port_info; do
		assert_ok "$out" ".dn_info.$field.status" "dn$idx pointers"
	done
}

# SIDE_PROVISIONED remembers which sides have already been through the §9.4
# two-phase provisioning, keyed dnidx:sp:leg:side. The memo is load-bearing,
# not tidiness: cases A and D call dn_side again at the failover flip to move
# the primary, and re-converging an already-serving side with
# provisioned = false is a perfectly legal request that the converge matrix
# answers with "no exports" (update_01.md U4 row 3) — i.e. it would retract the
# live export stacks in the middle of a failover. Phase 1 therefore runs
# exactly once per side. Each case resets the map.
declare -A SIDE_PROVISIONED=()

# dn_side converges one side: the DN-side backing every CN leg connects to.
#
# The first converge of a side is two-phase, this script playing the sp-worker's
# flip rule (update_01.md U4): sync it unprovisioned — which allocates the
# extent runs, builds DnSideName and starts the background zeroing goroutine,
# and exports nothing — wait for every logical extent to be zeroed, then re-sync
# it provisioned at a fresh revision. Every later converge of the same side goes
# straight to phase 2.
dn_side() { # dnidx sp leg side ext_cnt primary_cn [standby_cn]
	local idx=$1 out extra=() key="$1:$2:$3:$4" cn field
	[ -z "${7:-}" ] || extra=(--standby-cn "$7")
	if [ -z "${SIDE_PROVISIONED[$key]:-}" ]; then
		bump_dn_rev "$idx"
		out=$(dnctl "$idx" syncup-side --revision "${DNREV[$idx]}" \
			--sp "$2" --leg "$3" --side "$4" --ext-cnt "$5" --cntlid-slot 0 \
			--primary-cn "$6" --sp-level readwrite --provisioned=false \
			"${extra[@]}")
		assert_provisioning_or_ok "$out" ".side_info.side_dev_info.status" \
			"dn$idx sp $2 leg $3 phase 1"
		# The whole point of the gate: nothing above the side device exists
		# before the bytes are zeroed — all three per-CN maps, for the primary
		# *and* the standby (cnagent_integtest.md §9). Cases A and D give the
		# sides of the failover pair a standby, so checking one map of one CN
		# would leave one of the two per-CN export stacks unexamined exactly
		# while it is supposed to be gated.
		for cn in "$6" ${7:+"$7"}; do
			for field in cn_id_to_dm_error cn_id_to_dm_linear cn_id_to_nvmeof; do
				assert_provisioning "$out" \
					".side_info.$field[\"$(d16 "$cn")\"].status" \
					"dn$idx sp $2 leg $3 must not export at provisioned=false: $field[$cn]"
			done
		done
		dnctl "$idx" wait-zeroed --sp "$2" --leg "$3" --side "$4" \
			--interval 0.5 --timeout "$ZERO_TIMEOUT" >/dev/null
		SIDE_PROVISIONED[$key]=1
	fi
	bump_dn_rev "$idx"
	out=$(dnctl "$idx" syncup-side --revision "${DNREV[$idx]}" \
		--sp "$2" --leg "$3" --side "$4" --ext-cnt "$5" --cntlid-slot 0 \
		--primary-cn "$6" --sp-level readwrite --provisioned=true \
		"${extra[@]}")
	assert_ok "$out" ".side_info.side_dev_info.status" \
		"dn$idx sp $2 leg $3 side_dev"
	assert_ok "$out" ".side_info.cn_id_to_nvmeof[\"$(d16 "$6")\"].status" \
		"dn$idx sp $2 leg $3 export to cn $6"
}

# dn_drop empties a DN's side list, which tears its sides down declaratively.
dn_drop() { # dnidx
	local out field
	bump_dn_rev "$1"
	out=$(dnctl "$1" syncup-dn --revision "${DNREV[$1]}" \
		--extent-size "$EXTENT_SIZE")
	for field in disk_info meta_info port_info; do
		assert_ok "$out" ".dn_info.$field.status" "dn$1 teardown"
	done
}

# cn_drop empties a CN's cntlr pointer list, which is the declarative cntlr
# teardown of CN7/CN21.
cn_drop() { # cnidx
	local out
	bump_cn_sync "$1"
	out=$(cnctl "$1" syncup-cn --revision "${CNREV[$1]}")
	assert_cn_info_ok "$out" "cn$1 teardown"
}

# converge_check runs the §9 check-cn/check-cntlr round pair and asserts that
# each reply echoes the revision the agent actually stored.
converge_check() { # cnidx sp cntlr cntlrrev
	local idx=$1 out
	out=$(cnctl "$idx" check-cn --revision "${CNSYNC[$idx]}" --show-info)
	assert_cn_info_ok "$out" "cn$idx check-cn"
	assert_eq "$(jq_of "$out" '.revision // "0"')" "${CNSYNC[$idx]}" \
		"cn$idx check-cn revision"
	out=$(cnctl "$idx" check-cntlr --revision "$4" --show-info \
		--sp "$2" --cntlr "$3")
	assert_eq "$(jq_of "$out" '.revision // "0"')" "$4" \
		"cn$idx check-cntlr revision"
	assert_all_ok "$out" .cntlr_info "cn$idx check-cntlr"
}

# bitmap_hex runs one of the bitmap RPCs and returns just the hex line; the
# second line of the reply is the bit count (§8). Taking it here rather than
# through `| head -1` keeps the ctl from ever writing into a closed pipe.
bitmap_hex() { # cnidx subcommand args…
	local out
	out=$(cnctl "$@")
	printf '%s' "${out%%$'\n'*}"
}

assert_no_residue() { # sp
	local idx got
	for idx in 1 2; do
		got=$(helper "$idx" "residue $(hex16 "$1")")
		[ -z "$got" ] || die "vm$idx still holds objects of sp $1: $got"
	done
}

# event_line prints the 1-based position of the first event matching a regex,
# or the empty string. The event stream of one trace id is the ordered record
# of one converge, which is how the CN9/CN18/§11.5 orderings are asserted.
event_line() { # stream regex
	printf '%s\n' "$1" | grep -nE -- "$2" | head -1 | cut -d: -f1 || true
}

event_cnt() { # stream regex
	printf '%s\n' "$1" | grep -cE -- "$2" || true
}

assert_before() { # stream regex_first regex_second label
	local first second
	first=$(event_line "$1" "$2")
	second=$(event_line "$1" "$3")
	[ -n "$first" ] || die "$4: no event matching '$2'"
	[ -n "$second" ] || die "$4: no event matching '$3'"
	[ "$first" -lt "$second" ] ||
		die "$4: '$2' is at $first, '$3' at $second — wrong order"
}

# assert_absent is assert_before's negative twin: the event must not occur
# anywhere in the stream. It is how a stage proves an omission — a converge
# that sends *no* pool message (ThinDeviceCreated.md U5-S3) leaves nothing
# behind for an ordering assertion to anchor on.
assert_absent() { # stream regex label
	local at
	at=$(event_line "$1" "$2")
	[ -z "$at" ] || die "$3: unexpected event matching '$2' at $at"
}

# ---------------------------------------------------------------------------
# Case S — smoke (§10)
# ---------------------------------------------------------------------------

case_smoke() {
	CASE=smoke
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	local sp=0x3a1 cntlr=0x1 cn=1 dn=1 hv=2 slot=0
	local nqn="$NQN_IT_PREFIX:s:vol1"
	local uuid=11111111-1111-4111-8111-111111111111
	local req="$WORK/req-smoke-cn1.json"
	local out dev want got cntlrrev name
	diag_cntlr "$cn" "$sp" "$cntlr"

	stage dn "DN1 exports the meta and data sides of the S-shaped SP"
	dn_pointers "$dn" "$sp:$S_MLEG:$S_MSIDE" "$sp:$S_DLEG:$S_DSIDE"
	dn_side "$dn" "$sp" "$S_MLEG" "$S_MSIDE" 1 "${CNID[$cn]}"
	dn_side "$dn" "$sp" "$S_DLEG" "$S_DSIDE" 2 "${CNID[$cn]}"

	stage cn "SyncupCn introduces the pointer, SyncupCntlr builds the stack"
	bump_cn_sync "$cn"
	out=$(cnctl "$cn" syncup-cn --revision "${CNREV[$cn]}" --cntlr "$sp:$cntlr")
	assert_cn_info_ok "$out" "smoke syncup-cn"
	req_none "$req" "$cn" "$dn" "$sp" "$cntlr" "$slot" "$nqn" "$uuid" false
	bump_cn_rev "$cn"
	cntlrrev=${CNREV[$cn]}
	out=$(cn_syncup_cntlr "$cn" "$req")
	assert_map_ok "$out" leg_id_to_leg "$S_MLEG" smoke
	assert_map_ok "$out" leg_id_to_leg "$S_DLEG" smoke
	assert_map_ok "$out" grp_id_to_md_raid "$S_MGRP" smoke
	assert_map_ok "$out" grp_id_to_md_raid "$S_DGRP" smoke
	assert_map_ok "$out" slice_id_to_meta "$S_SLICE" smoke
	assert_map_ok "$out" slice_id_to_data "$S_SLICE" smoke
	assert_map_ok "$out" slice_id_to_dm_pool "$S_SLICE" smoke
	assert_map_ok "$out" td_id_to_raid0 "$S_TD" smoke
	assert_map_ok "$out" td_id_to_dm_error "$S_TD" smoke
	assert_thin_ok "$out" "$S_TD" "$S_SLICE" smoke
	assert_map_ok "$out" ns_id_to_namespace "$S_NS" smoke
	assert_map_ok "$out" ns_id_to_dm_linear "$S_NS" smoke
	assert_map_ok "$out" ss_id_to_subsystem "$S_SS" smoke
	# The kind-9 leg wrappers and the kind-a RedundNone group devices are the
	# two dm layers the reply names only indirectly.
	for name in "$(cn_dm_name 9 "$cn" "$sp" "$S_MLEG")" \
		"$(cn_dm_name 9 "$cn" "$sp" "$S_DLEG")" \
		"$(cn_dm_name a "$cn" "$sp" "$S_MGRP")" \
		"$(cn_dm_name a "$cn" "$sp" "$S_DGRP")"; do
		assert_eq "$(helper "$cn" "dm_state $name")" live "smoke $name"
	done
	assert_eq "$(helper "$cn" "ss_attr '$nqn' attr_allow_any_host")" 1 \
		"smoke $nqn attr_allow_any_host"
	assert_eq "$(helper "$cn" "allowed_hosts '$nqn'")" "" \
		"smoke $nqn allowed_hosts"

	stage host "the host connects and the path goes live/optimized"
	host_connect "$hv" "$cn" "$nqn"
	dev=$(host_dev "$uuid")
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	assert_eq "$(host_state "$hv" "$nqn" "$cn")" live "smoke path state"
	host_wait_ana "$hv" "$nqn" "$cn" optimized 20

	stage io "4 MiB round trip: host -> CN thin/raid0 stack -> DN side"
	make_pattern "$hv" "$WORK/pattern-smoke.bin" 4
	want=$(sha_range "$hv" "$WORK/pattern-smoke.bin" 4)
	write_range "$hv" "$WORK/pattern-smoke.bin" "$dev" 4
	drop_caches "$hv"
	got=$(sha_range "$hv" "$dev" 4)
	assert_eq "$got" "$want" "smoke readback sha256"

	stage check "converge check round and the pool status format"
	converge_check "$cn" "$sp" "$cntlr" "$cntlrrev"
	out=$(cnctl "$cn" get-cntlr-info --sp "$sp" --cntlr "$cntlr")
	got=$(jq_of "$out" \
		".cntlr_info.slice_id_to_dm_pool[\"$(d16 "$S_SLICE")\"].details // \"\"")
	# The §10.4 auto-grow parses metadata and data used/total out of this raw
	# `dmsetup status` line.
	printf '%s\n' "$got" |
		grep -qE 'thin-pool .*[0-9]+/[0-9]+[[:space:]]+[0-9]+/[0-9]+' ||
		die "smoke pool details is not a thin-pool status line: $got"

	stage teardown "empty cntlr list, then empty side list"
	host_disconnect "$hv" "$nqn"
	cn_drop "$cn"
	dn_drop "$dn"
	got=$(helper "$cn" "cn_residue $(hex16 "${CNID[$cn]}")")
	[ -z "$got" ] || die "smoke: vm$cn still holds cn objects: $got"
	got=$(helper "$cn" "clone_meta_wrappers $(hex16 "${CNID[$cn]}")")
	[ -z "$got" ] ||
		die "smoke: the clone-metadata arena still holds wrappers: $got"
	out=$(cnctl "$cn" get-cn-info)
	assert_cn_info_ok "$out" "smoke base state after teardown"
	assert_no_residue "$sp"
	sshv_ok "$hv" "rm -f $WORK/pattern-smoke.bin"
}

# ---------------------------------------------------------------------------
# Case A — redund (§11): raid1, failover, readonly
# ---------------------------------------------------------------------------

case_redund() {
	CASE=redund
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	local sp=0x3b1 c1=0x1 c2=0x2 hv=2
	local nqn="$NQN_IT_PREFIX:a:vol1"
	local uuid=22222222-2222-4222-8222-222222222222
	local req1="$WORK/req-redund-cn1.json" req2="$WORK/req-redund-cn2.json"
	local out dev want got seq rev1 rev2 i nsdev
	diag_cntlr 1 "$sp" "$c1"
	diag_cntlr 2 "$sp" "$c2"
	dev=$(host_dev "$uuid")
	nsdev=$(cn_dm_name 6 2 "$sp" "$A_NS")

	stage dn "4 sides, one per leg, primary CN1 with CN2 as the standby"
	dn_pointers 1 "$sp:${A_MLEG[1]}:${A_MSIDE[1]}" "$sp:${A_DLEG[1]}:${A_DSIDE[1]}"
	dn_pointers 2 "$sp:${A_MLEG[2]}:${A_MSIDE[2]}" "$sp:${A_DLEG[2]}:${A_DSIDE[2]}"
	dn_side 1 "$sp" "${A_MLEG[1]}" "${A_MSIDE[1]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 1 "$sp" "${A_DLEG[1]}" "${A_DSIDE[1]}" 2 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_MLEG[2]}" "${A_MSIDE[2]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_DLEG[2]}" "${A_DSIDE[2]}" 2 "${CNID[1]}" "${CNID[2]}"

	stage cn "the same request to both CNs, primary on CN1"
	local cntrace=$TRACE
	bump_cn_sync 1
	out=$(cnctl 1 syncup-cn --revision "${CNREV[1]}" --cntlr "$sp:$c1")
	assert_cn_info_ok "$out" "redund syncup-cn 1"
	bump_cn_sync 2
	out=$(cnctl 2 syncup-cn --revision "${CNREV[2]}" --cntlr "$sp:$c2")
	assert_cn_info_ok "$out" "redund syncup-cn 2"
	req_raid1 "$req1" 1 "$sp" "$c1" 0 true "$nqn" "$uuid"
	req_raid1 "$req2" 2 "$sp" "$c2" 1 false "$nqn" "$uuid"
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" grp_id_to_md_raid "$A_MGRP" "redund primary"
	assert_map_ok "$out" grp_id_to_md_raid "$A_DGRP" "redund primary"
	assert_map_ok "$out" slice_id_to_dm_pool "$A_SLICE" "redund primary"
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "redund primary"
	assert_map_ok "$out" ns_id_to_namespace "$A_NS" "redund primary"
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	for i in 1 2; do
		assert_map_ok "$out" leg_id_to_leg "${A_MLEG[$i]}" "redund standby"
		assert_map_ok "$out" leg_id_to_leg "${A_DLEG[$i]}" "redund standby"
	done
	assert_map_ok "$out" ns_id_to_namespace "$A_NS" "redund standby"
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund standby"

	stage assert "primary md state, standby shape, host isolation"
	# CN12 case 1: neither member carries an md superblock — which after the
	# §9.4 whole-side zeroing is exactly the freshly-provisioned case
	# (update_01.md U4 replaced the old trim) — so the arrays are created,
	# never assembled.
	seq=$(helper 1 "cn_events $cntrace")
	assert_eq "$(event_cnt "$seq" '^mdadm --create .*--run .*--assume-clean')" 2 \
		"redund: mdadm --create --run --assume-clean for both groups"
	assert_eq "$(event_cnt "$seq" '^mdadm --assemble ')" 0 \
		"redund: no --assemble on a fresh create"
	local mdmeta mddata
	mdmeta=$(cnctl 1 md-name --sp "$sp" --slice-idx 0 --grp-idx 0 --meta)
	mddata=$(cnctl 1 md-name --sp "$sp" --slice-idx 0 --grp-idx 0)
	for got in "$(jq_of "$mdmeta" .array_name)" "$(jq_of "$mddata" .array_name)"; do
		[ -n "$(event_line "$seq" "^mdadm --create .*--name $got ")" ] ||
			die "redund: no create of array $got"
	done
	for got in "$(jq_of "$mdmeta" .dev_path)" "$(jq_of "$mddata" .dev_path)"; do
		helper 1 "wait_dev '$got' 20" || die "redund: no md node $got on vm1"
	done
	got=$(helper 1 mdstat | grep -c '\[UU\]' || true)
	assert_eq "$got" 2 "redund: both arrays up ([UU])"
	# The standby holds every leg connected but builds nothing above them.
	for i in 1 2; do
		assert_eq "$(leg_state 2 "$sp" "${A_MLEG[$i]}" "${CNID[2]}" "$i")" live \
			"redund standby meta leg $i state"
		assert_eq "$(leg_ana 2 "$sp" "${A_MLEG[$i]}" "${CNID[2]}" "$i")" \
			non-optimized "redund standby meta leg $i ana"
		assert_eq "$(leg_state 2 "$sp" "${A_DLEG[$i]}" "${CNID[2]}" "$i")" live \
			"redund standby data leg $i state"
		assert_eq "$(leg_ana 2 "$sp" "${A_DLEG[$i]}" "${CNID[2]}" "$i")" \
			non-optimized "redund standby data leg $i ana"
	done
	got=$(helper 2 mdstat | grep -c '\[UU\]' || true)
	assert_eq "$got" 0 "redund: the standby has no md arrays"
	# CN16 rule 1: a standby's ns-dev is a linear over the td's dm-error.
	assert_eq "$(helper 2 "dm_backing $nsdev")" \
		"$(helper 2 "dm_devno $(cn_dm_name 5 2 "$sp" "$A_TD")")" \
		"redund: standby ns-dev is backed by the td's dm-error"
	assert_eq "$(helper 2 "ns_attr '$nqn' 1 ana_grpid")" 3 \
		"redund: standby ns ana_grpid"
	for i in 1 2; do
		assert_eq "$(helper "$i" "allowed_hosts '$nqn'")" "$HOST_NQN" \
			"redund cn$i allowed_hosts"
	done

	stage host "one namespace, two paths: CN1 optimized, CN2 inaccessible"
	host_connect "$hv" 1 "$nqn"
	host_connect "$hv" 2 "$nqn"
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	host_wait_ana "$hv" "$nqn" 1 optimized 20
	assert_eq "$(host_ana "$hv" "$nqn" 2)" inaccessible \
		"redund standby path before failover"
	make_pattern "$hv" "$WORK/pattern-redund.bin" 8
	want=$(sha_range "$hv" "$WORK/pattern-redund.bin" 8)
	write_range "$hv" "$WORK/pattern-redund.bin" "$dev" 8
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 8)" "$want" "redund pre-failover readback"

	stage demote "failover step 1: the old primary hands IO over (CN9 order)"
	# Host IO is quiesced from here until the new primary reports optimized:
	# in between the namespace can have no serving path.
	req_set "$req1" '.cntlr.primary = false'
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund demoted"
	seq=$(helper 1 "cn_events $TRACE")
	assert_before "$seq" '^write .*/ana_grpid 3$' \
		"^dmsetup reload $(cn_dm_name 6 1 "$sp" "$A_NS") " \
		"redund demote: ANA inaccessible before the ns-dev retires"
	assert_eq "$(event_cnt "$seq" '^mdadm --stop ')" 2 \
		"redund demote: mdadm --stop for both arrays"
	assert_eq "$(event_cnt "$seq" "^nvme disconnect .*$NQN_PREFIX:2:")" 0 \
		"redund demote: a standby keeps its legs connected"

	stage flip "failover step 2: every side moves its primary to CN2"
	dn_side 1 "$sp" "${A_MLEG[1]}" "${A_MSIDE[1]}" 1 "${CNID[2]}" "${CNID[1]}"
	dn_side 1 "$sp" "${A_DLEG[1]}" "${A_DSIDE[1]}" 2 "${CNID[2]}" "${CNID[1]}"
	dn_side 2 "$sp" "${A_MLEG[2]}" "${A_MSIDE[2]}" 1 "${CNID[2]}" "${CNID[1]}"
	dn_side 2 "$sp" "${A_DLEG[2]}" "${A_DSIDE[2]}" 2 "${CNID[2]}" "${CNID[1]}"
	# The promote may only run once the legs are optimized on CN2 (§9).
	for i in 1 2; do
		leg_wait_ana 2 "$sp" "${A_MLEG[$i]}" "${CNID[2]}" "$i" optimized 20
		leg_wait_ana 2 "$sp" "${A_DLEG[$i]}" "${CNID[2]}" "$i" optimized 20
	done

	stage promote "failover step 3: CN2 assembles what CN1 built"
	req_set "$req2" '.cntlr.primary = true'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" grp_id_to_md_raid "$A_MGRP" "redund promoted"
	assert_map_ok "$out" grp_id_to_md_raid "$A_DGRP" "redund promoted"
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "redund promoted"
	seq=$(helper 2 "cn_events $TRACE")
	# CN12 case 2: both members carry superblocks now.
	assert_eq "$(event_cnt "$seq" '^mdadm --assemble ')" 2 \
		"redund promote: mdadm --assemble for both groups"
	assert_eq "$(event_cnt "$seq" '^mdadm --create ')" 0 \
		"redund promote: no --create over existing superblocks"

	stage failover "the data crossed the failover through md"
	host_wait_ana "$hv" "$nqn" 2 optimized 30
	assert_eq "$(host_ana "$hv" "$nqn" 1)" inaccessible \
		"redund old primary path after failover"
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 8)" "$want" "redund post-failover readback"
	make_pattern "$hv" "$WORK/probe-redund.bin" 1
	got=$(sha_range "$hv" "$WORK/probe-redund.bin" 1)
	write_range "$hv" "$WORK/probe-redund.bin" "$dev" 1 9
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 1 9)" "$got" "redund post-failover write"

	stage readonly "SP_LEVEL_READONLY: reads served, writes error ([D11])"
	req_set "$req2" '.sp_level = "SP_LEVEL_READONLY"'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund readonly"
	local table
	table=$(helper 2 "dm_table $nsdev")
	printf '%s\n' "$table" | grep -q 'flakey' ||
		die "redund readonly: ns-dev table is not flakey: $table"
	printf '%s\n' "$table" | grep -q 'error_writes' ||
		die "redund readonly: ns-dev table has no error_writes: $table"
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 1 9)" "$got" "redund readonly read"
	assert_eq "$(helper "$hv" "write_probe '$dev' 9")" eio "redund readonly write"
	assert_eq "$(host_ana "$hv" "$nqn" 2)" optimized "redund readonly ana"
	req_set "$req2" '.sp_level = "SP_LEVEL_READWRITE"'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund readwrite again"
	assert_eq "$(helper "$hv" "write_probe '$dev' 9")" ok \
		"redund write after readwrite"

	stage check "converge check rounds on both CNs"
	converge_check 1 "$sp" "$c1" "$rev1"
	converge_check 2 "$sp" "$c2" "$rev2"

	stage teardown "empty cntlr lists, then empty side lists"
	host_disconnect "$hv" "$nqn"
	cn_drop 1
	cn_drop 2
	dn_drop 1
	dn_drop 2
	assert_no_residue "$sp"
	sshv_ok "$hv" "rm -f $WORK/pattern-redund.bin $WORK/probe-redund.bin"
}

# ---------------------------------------------------------------------------
# Case B — thinbm (§12): snapshots and bitmap reads
# ---------------------------------------------------------------------------
#
# The td is 64 MiB of 1 MiB blocks, so every bitmap is exactly 64 bits, and the
# §11.4 wire inversion makes a written block a 0 bit. Blocks {0,5,6,7} written
# ⇒ byte 0 = 0x1e, bytes 1-7 = 0xff.

B_TD2=0xc
B_SS2=0xd
B_NS2=0xe

case_thinbm() {
	CASE=thinbm
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	local sp=0x3c1 cntlr=0x1 cn=1 dn=1 hv=2 slot=0
	local nqn="$NQN_IT_PREFIX:b:vol1" snapnqn="$NQN_IT_PREFIX:b:snap1"
	local uuid=44444444-4444-4444-8444-444444444444
	local snapuuid=55555555-5555-4555-8555-555555555555
	local req="$WORK/req-thinbm-cn1.json"
	local out dev snapdev cntlrrev got seq blk muts
	diag_cntlr "$cn" "$sp" "$cntlr"
	dev=$(host_dev "$uuid")
	snapdev=$(host_dev "$snapuuid")

	stage dn "the S-shaped SP again, on DN1"
	dn_pointers "$dn" "$sp:$S_MLEG:$S_MSIDE" "$sp:$S_DLEG:$S_DSIDE"
	dn_side "$dn" "$sp" "$S_MLEG" "$S_MSIDE" 1 "${CNID[$cn]}"
	dn_side "$dn" "$sp" "$S_DLEG" "$S_DSIDE" 2 "${CNID[$cn]}"

	stage cn "CN1 builds the primary stack"
	bump_cn_sync "$cn"
	out=$(cnctl "$cn" syncup-cn --revision "${CNREV[$cn]}" --cntlr "$sp:$cntlr")
	assert_cn_info_ok "$out" "thinbm syncup-cn"
	req_none "$req" "$cn" "$dn" "$sp" "$cntlr" "$slot" "$nqn" "$uuid" false
	bump_cn_rev "$cn"
	cntlrrev=${CNREV[$cn]}
	out=$(cn_syncup_cntlr "$cn" "$req")
	assert_map_ok "$out" slice_id_to_dm_pool "$S_SLICE" thinbm
	assert_thin_ok "$out" "$S_TD" "$S_SLICE" thinbm
	assert_map_ok "$out" ns_id_to_namespace "$S_NS" thinbm

	stage write "the host writes td blocks 0, 5, 6 and 7"
	host_connect "$hv" "$cn" "$nqn"
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	host_wait_ana "$hv" "$nqn" "$cn" optimized 20
	for blk in 0 5 6 7; do
		make_pattern "$hv" "$WORK/pattern-b-$blk.bin" 1
		write_range "$hv" "$WORK/pattern-b-$blk.bin" "$dev" 1 "$blk"
	done

	stage tdbm "GetThinDeviceBm: 1 = unmapped, LSB-first (§11.4)"
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 1effffffffffffff "thinbm td bitmap"

	stage legbm "GetLegBm: the data group's span, the meta group's zero rule"
	got=$(bitmap_hex "$cn" get-leg-bm --sp "$sp" --cntlr "$cntlr" --leg "$S_DLEG" \
		--start-block 0 --block-cnt 0)
	# 127 data-region bits: pool-data blocks 0..3 are mapped, bit 127 is pad.
	# A fresh dm-thin pool allocates data blocks sequentially from 0; if a
	# kernel ever breaks that, relax to "exactly four 0-bits" — the count, not
	# the position, is the contract.
	assert_eq "$got" f0ffffffffffffffffffffffffffff7f "thinbm data leg bitmap"
	got=$(bitmap_hex "$cn" get-leg-bm --sp "$sp" --cntlr "$cntlr" --leg "$S_MLEG" \
		--start-block 0 --block-cnt 0)
	assert_eq "$got" 0000000000000000 "thinbm meta leg bitmap (CN27)"

	stage paging "one-byte window proves the window math end to end"
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 4 --block-cnt 8)
	assert_eq "$got" f1 "thinbm paged td bitmap"

	stage snapshot "create_snap needs a quiesced origin (CN14)"
	# The origin is re-sent with created = true: the gateway refuses a
	# snapshot of a td it has not seen materialized in every slice pool
	# (ThinDeviceCreated.md U2-S1), so this is the only td_list a real worker
	# could publish here. The snapshot itself is uncreated, which is what
	# still puts its create_snap inside the origin's quiesce below.
	req_set "$req" ".td_list = [$(req_td "$S_TD" 1 0 true),
		$(req_td "$B_TD2" 2 1)]
		| .nqn_to_subsystem[\"$snapnqn\"] = $(req_subsys "$B_SS2" "[]" \
		"$(req_ns "$B_NS2" 1 "$B_TD2" "$snapuuid" false)")"
	bump_cn_rev "$cn"
	cntlrrev=${CNREV[$cn]}
	out=$(cn_syncup_cntlr "$cn" "$req")
	assert_thin_ok "$out" "$B_TD2" "$S_SLICE" thinbm
	assert_map_ok "$out" ns_id_to_namespace "$B_NS2" thinbm
	assert_map_ok "$out" ss_id_to_subsystem "$B_SS2" thinbm
	seq=$(helper "$cn" "cn_events $TRACE")
	local orithin pool oriraid0 snapthin
	orithin=$(cn_dm_name 3 "$cn" "$sp" "$S_TD" "$S_SLICE")
	pool=$(cn_dm_name 2 "$cn" "$sp" "$S_SLICE")
	assert_before "$seq" "^dmsetup suspend $orithin\$" \
		"^dmsetup message $pool 0 create_snap 2 1\$" \
		"thinbm: the origin is suspended across create_snap"
	assert_before "$seq" "^dmsetup message $pool 0 create_snap 2 1\$" \
		"^dmsetup resume $orithin\$" \
		"thinbm: the origin resumes right after create_snap"
	# update_02.md U1: the per-slice suspend above stays, nested inside one
	# quiesce of the origin td's raid0 that spans every slice's message. This
	# SP has one slice, so the log is the only on-hardware evidence of the
	# bracket; the cross-slice property is a unit test (cnagent.md §6 test 19).
	oriraid0=$(cn_dm_name 4 "$cn" "$sp" "$S_TD")
	snapthin=$(cn_dm_name 3 "$cn" "$sp" "$B_TD2" "$S_SLICE")
	assert_before "$seq" "^dmsetup suspend $oriraid0\$" \
		"^dmsetup message $pool 0 create_snap 2 1\$" \
		"thinbm: the origin raid0 is quiesced across create_snap (U1)"
	assert_before "$seq" "^dmsetup message $pool 0 create_snap 2 1\$" \
		"^dmsetup resume $oriraid0\$" \
		"thinbm: the origin raid0 resumes after the messages (U1)"
	assert_before "$seq" "^dmsetup resume $oriraid0\$" \
		"^dmsetup create $snapthin( |\$)" \
		"thinbm: the snapshot thin device is created after the resume (U1)"

	stage snapio "the snapshot carries the origin's blocks and nothing else"
	host_connect "$hv" "$cn" "$snapnqn"
	helper "$hv" "wait_dev '$snapdev' 20" ||
		die "no snapshot node $snapdev on vm$hv"
	host_wait_ana "$hv" "$snapnqn" "$cn" optimized 20
	drop_caches "$hv"
	for blk in 0 5 6 7; do
		got=$(sha_range "$hv" "$WORK/pattern-b-$blk.bin" 1)
		assert_eq "$(sha_range "$hv" "$snapdev" 1 "$blk")" "$got" \
			"thinbm snapshot block $blk"
	done
	assert_eq "$(helper "$hv" "zero_probe '$snapdev' 3 1")" zeros \
		"thinbm snapshot block 3"

	stage isolation "a write to the origin never reaches the snapshot"
	make_pattern "$hv" "$WORK/pattern-b-9.bin" 1
	write_range "$hv" "$WORK/pattern-b-9.bin" "$dev" 1 9
	drop_caches "$hv"
	assert_eq "$(helper "$hv" "zero_probe '$snapdev' 9 1")" zeros \
		"thinbm snapshot block 9 after the origin write"
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$B_TD2" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 1effffffffffffff "thinbm snapshot bitmap unchanged"
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 1efdffffffffffff "thinbm origin bitmap gained block 9"

	stage drop "CN21 tears the cntlr down; the pool metadata keeps both ids"
	# ThinDeviceCreated.md U5-S3 steps 1-3. The teardown removes every dm
	# device and sends no `delete`, so both thin ids stay in the pool
	# metadata on DN1's legs, and SH7 deletes the cntlr's local file — the
	# next SyncupCntlr is a fresh cntlr at a higher revision. Its own stage,
	# because parking the ns-devs onto dm-error is a `dmsetup reload` (=
	# suspend + load + resume) and the rebuild stage below asserts that *it*
	# suspends nothing.
	host_disconnect "$hv" "$snapnqn"
	host_disconnect "$hv" "$nqn"
	cn_drop "$cn"
	bump_cn_sync "$cn"
	out=$(cnctl "$cn" syncup-cn --revision "${CNREV[$cn]}" --cntlr "$sp:$cntlr")
	assert_cn_info_ok "$out" "thinbm rebuild syncup-cn"

	stage rebuild "a created td is re-attached with no device-set message"
	# ThinDeviceCreated.md U5-S3 steps 4-5 / R14: the desired state the
	# sp-worker publishes once both tds have flipped. The rebuilt cntlr must
	# re-attach the existing volumes with a bare `dmsetup create` — no
	# create_thin, no create_snap, nothing to quiesce — and the mappings must
	# come back intact. This is the only place a real dm-thin pool proves
	# that a bare create on an existing id works.
	req_set "$req" '.td_list |= map(.created = true)'
	bump_cn_rev "$cn"
	cntlrrev=${CNREV[$cn]}
	out=$(cn_syncup_cntlr "$cn" "$req")
	assert_thin_ok "$out" "$S_TD" "$S_SLICE" "thinbm rebuild"
	assert_thin_ok "$out" "$B_TD2" "$S_SLICE" "thinbm rebuild"
	assert_map_ok "$out" slice_id_to_dm_pool "$S_SLICE" "thinbm rebuild"
	assert_map_ok "$out" ns_id_to_namespace "$S_NS" "thinbm rebuild"
	assert_map_ok "$out" ns_id_to_namespace "$B_NS2" "thinbm rebuild"
	seq=$(helper "$cn" "cn_events $TRACE")
	assert_absent "$seq" "^dmsetup message .* create_thin" \
		"thinbm rebuild: a created td was re-created by message"
	assert_absent "$seq" "^dmsetup message .* create_snap" \
		"thinbm rebuild: a created snapshot was re-created by message"
	assert_absent "$seq" "^dmsetup suspend" \
		"thinbm rebuild: nothing needed quiescing"
	# U5-T1 as amended by update_05.md U3: the rebuild re-creates the pool
	# device, so the activation sweep runs — its reserve/release pair is
	# the only `dmsetup message` traffic, and nothing changes the device
	# set (no create_thin, no create_snap, no delete; the bitmap proof
	# below is what shows the bare creates attached the existing ids).
	muts=$(helper "$cn" "mutations $TRACE")
	[ -n "$(event_line "$muts" '^dmsetup create ')" ] ||
		die "thinbm rebuild: the converge activated no dm device"
	assert_absent "$muts" "^dmsetup message .* delete" \
		"thinbm rebuild: the sweep deleted a live id"
	assert_eq "$(event_cnt "$muts" '^dmsetup message .* reserve_metadata_snap$')" 1 \
		"thinbm rebuild: one sweep reservation"
	assert_eq "$(event_cnt "$muts" '^dmsetup message .* release_metadata_snap$')" 1 \
		"thinbm rebuild: one sweep release"
	# The mapping proof: both bitmaps read exactly what they read before the
	# teardown, so the bare `dmsetup create` attached the *existing* ids —
	# not a fresh, empty volume under the same dev_id.
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$B_TD2" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 1effffffffffffff "thinbm rebuilt snapshot bitmap"
	got=$(bitmap_hex "$cn" get-td-bm --sp "$sp" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 1efdffffffffffff "thinbm rebuilt origin bitmap"

	stage check "converge check round"
	converge_check "$cn" "$sp" "$cntlr" "$cntlrrev"

	stage teardown "empty cntlr list, then empty side list"
	# The two host_disconnects the rebuild stage moved up are harmless
	# repeats if the host reconnected; nothing here depends on them.
	cn_drop "$cn"
	dn_drop "$dn"
	assert_no_residue "$sp"
	sshv_ok "$hv" "rm -f $WORK/pattern-b-*.bin"
}

# ---------------------------------------------------------------------------
# Case C — clone_xfer (§13): the §11.3 live move and the §11.5 recovery
# ---------------------------------------------------------------------------
#
# 32 of the td's 64 MiB are written, so the pushed bitmap is 00000000ffffffff:
# bits 32..63 = 1 = skippable ⇒ exactly one 32 MiB blkdiscard at offset 32 MiB
# **on the clone device**. Since update_01.md U3 the same trace also carries an
# earlier, unrelated blkdiscard on the arena loop device (the allocator's
# recycled-unit hole punch), so every assertion here is scoped by target.

C_XFER=0xc
C_CLONE=0xc

case_clone_xfer() {
	CASE=clone_xfer
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	local sp1=0x3d1 sp2=0x3d2 cntlr=0x1 hv=1
	local nqn="$NQN_IT_PREFIX:c:vol1"
	local uuid=33333333-3333-4333-8333-333333333333
	local req1="$WORK/req-clone_xfer-cn1.json"
	local req2="$WORK/req-clone_xfer-cn2.json"
	local out dev want got seq rev1 rev2 xnqn clonedm metadm nsdev1 ctrl sample
	diag_cntlr 1 "$sp1" "$cntlr"
	diag_cntlr 2 "$sp2" "$cntlr"
	dev=$(host_dev "$uuid")
	xnqn=$(xfer_nqn "$CLUSTER" "$sp1" "$C_XFER")
	clonedm=$(cn_dm_name 7 2 "$sp2" "$C_CLONE")
	metadm=$(clone_meta_dm 2 "$sp2" "$C_CLONE")
	nsdev1=$(cn_dm_name 6 1 "$sp1" "$S_NS")

	stage sp1 "stage 0: sp1 comes up on DN1/CN1 and takes the data"
	dn_pointers 1 "$sp1:$S_MLEG:$S_MSIDE" "$sp1:$S_DLEG:$S_DSIDE"
	dn_side 1 "$sp1" "$S_MLEG" "$S_MSIDE" 1 "${CNID[1]}"
	dn_side 1 "$sp1" "$S_DLEG" "$S_DSIDE" 2 "${CNID[1]}"
	bump_cn_sync 1
	out=$(cnctl 1 syncup-cn --revision "${CNREV[1]}" --cntlr "$sp1:$cntlr")
	assert_cn_info_ok "$out" "clone_xfer syncup-cn 1"
	req_none "$req1" 1 1 "$sp1" "$cntlr" 0 "$nqn" "$uuid" false
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" ns_id_to_namespace "$S_NS" "clone_xfer sp1"
	host_connect "$hv" 1 "$nqn"
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	host_wait_ana "$hv" "$nqn" 1 optimized 20
	make_pattern "$hv" "$WORK/pattern-c.bin" 32
	want=$(sha_range "$hv" "$WORK/pattern-c.bin" 32)
	write_range "$hv" "$WORK/pattern-c.bin" "$dev" 32
	# The production bitmap source (§8.13): the suite pushes exactly what it
	# read. The second 32 MiB was never written, so the skip range is real.
	got=$(bitmap_hex 1 get-td-bm --sp "$sp1" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 00000000ffffffff "clone_xfer sp1 td bitmap"

	stage sp2 "stage 1: sp2 comes up with the identical namespace, suspended"
	dn_pointers 2 "$sp2:$S_MLEG:$S_MSIDE" "$sp2:$S_DLEG:$S_DSIDE"
	dn_side 2 "$sp2" "$S_MLEG" "$S_MSIDE" 1 "${CNID[2]}"
	dn_side 2 "$sp2" "$S_DLEG" "$S_DSIDE" 2 "${CNID[2]}"
	bump_cn_sync 2
	out=$(cnctl 2 syncup-cn --revision "${CNREV[2]}" --cntlr "$sp2:$cntlr")
	assert_cn_info_ok "$out" "clone_xfer syncup-cn 2"
	req_none "$req2" 2 2 "$sp2" "$cntlr" 2 "$nqn" "$uuid" true
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_namespace "$S_NS" "clone_xfer sp2"
	host_connect "$hv" 2 "$nqn"
	assert_eq "$(host_state "$hv" "$nqn" 2)" live "clone_xfer sp2 path state"
	assert_eq "$(host_ana "$hv" "$nqn" 2)" inaccessible \
		"clone_xfer sp2 path before the flip"

	stage xfer "stage 2: the transfer retires the origin (host IO quiesced)"
	req_set "$req1" ".xfer_list = [$(req_xfer "$C_XFER" "$nqn" 1 \
		"[\"$(cn_host_nqn "$CLUSTER" "${CNID[2]}")\"]" true)]"
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" xfer_id_to_dm_linear "$C_XFER" clone_xfer
	assert_map_ok "$out" xfer_id_to_subsystem "$C_XFER" clone_xfer
	assert_map_ok "$out" xfer_id_to_namespace "$C_XFER" clone_xfer
	assert_eq "$(helper 1 "dm_state $nsdev1")" suspended \
		"clone_xfer: the origin ns-dev is effectively suspended (CN16)"
	host_wait_ana "$hv" "$nqn" 1 inaccessible 30
	assert_eq "$(helper 1 "port_linked '$xnqn'")" linked \
		"clone_xfer: the xfer subsystem is on the port"
	assert_eq "$(helper 1 "allowed_hosts '$xnqn'")" \
		"$(cn_host_nqn "$CLUSTER" "${CNID[2]}")" \
		"clone_xfer: the xfer allows exactly CN2"

	stage gate "stage 3: the clone is declared gated, then the chunk is pushed"
	req_set "$req2" ".sp_level = \"SP_LEVEL_NO_CLONE\"
		| .clone_list = [$(req_clone "$C_CLONE" "$xnqn" 1 "$S_TD" 1 true)]"
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_suppressed "$out" \
		".cntlr_info.clone_id_to_dm_clone[\"$(d16 "$C_CLONE")\"]" \
		"clone_xfer gated clone"
	seq=$(helper 2 "cn_events $TRACE")
	assert_eq "$(event_cnt "$seq" "^nvme connect .*$NQN_PREFIX:4:")" 0 \
		"clone_xfer: the gate holds the source connection back (CN19)"
	cnctl 2 push-clone-bm --revision "$rev2" --sp "$sp2" --cntlr "$cntlr" \
		--clone "$C_CLONE" --bm-idx 0 --bitmap-hex 00000000ffffffff >/dev/null
	# An equal-revision re-send is a legal full re-apply; here it is only a
	# way to read the applied set back out of the reply (§9).
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_eq "$(jq_of "$out" '(.bm_info_list // []) | length')" 1 \
		"clone_xfer bm_info_list length"
	assert_eq "$(jq_of "$out" '.bm_info_list[0].res_id')" "$(d16 "$C_CLONE")" \
		"clone_xfer bm_info_list res_id"
	assert_eq "$(jq_of "$out" '.bm_info_list[0].bm_idx_list | @csv')" '0' \
		"clone_xfer bm_info_list bm_idx_list"

	stage enable "stage 4: one converge builds the clone and flips the host"
	req_set "$req2" '.sp_level = "SP_LEVEL_READWRITE"'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" clone_id_to_target "$C_CLONE" clone_xfer
	assert_map_ok "$out" clone_id_to_dm_clone "$C_CLONE" clone_xfer
	assert_map_ok "$out" clone_id_to_meta "$C_CLONE" clone_xfer
	seq=$(helper 2 "cn_events $TRACE")
	# Two different blkdiscards now share the verb in one trace (update_01.md
	# U3), so every anchor below is scoped by target device: the allocator's
	# arena hole punch on the loop device comes *first*, and the clone's
	# hydration marking on /dev/mapper/{clone} after it.
	#
	# The arena range is punched before the wrapper is created — the
	# recycled-unit guard, so a new dm-clone can never misparse the previous
	# clone's still-valid superblock — and it is a plain discard: `--zeroout`
	# would materialize the whole 1 GiB tmpfs arena in RAM and is forbidden
	# here.
	assert_eq "$(event_cnt "$seq" '^blkdiscard .*--zeroout')" 0 \
		"clone_xfer: the clone-meta arena is never zeroed out"
	assert_eq "$(event_cnt "$seq" '^blkdiscard .*/dev/loop[0-9]')" 1 \
		"clone_xfer: exactly one arena discard"
	assert_before "$seq" '^blkdiscard .*/dev/loop[0-9]' \
		"^dmsetup create $metadm( |\$)" \
		"clone_xfer: the arena range is discarded before the wrapper"
	assert_before "$seq" "^dmsetup create $metadm( |\$)" \
		"^dmsetup create $clonedm( |\$)" \
		"clone_xfer: the metadata wrapper precedes the dm-clone"
	# The log-arithmetic proof that geometry and inversion are right: skip
	# bits 32..63 coalesce into one 32 MiB range at 32 MiB.
	assert_eq "$(event_cnt "$seq" "^blkdiscard .*/dev/mapper/$clonedm\$")" 1 \
		"clone_xfer: exactly one blkdiscard on the clone"
	assert_eq "$(event_line "$seq" \
		"^blkdiscard --offset 33554432 --length 33554432 /dev/mapper/$clonedm\$")" \
		"$(event_line "$seq" "^blkdiscard .*/dev/mapper/$clonedm\$")" \
		"clone_xfer: the blkdiscard range"
	assert_before "$seq" "^dmsetup create $clonedm( |\$)" \
		"^blkdiscard .*/dev/mapper/$clonedm\$" \
		"clone_xfer: the chunk lands after the clone is created"
	assert_before "$seq" "^blkdiscard .*/dev/mapper/$clonedm\$" \
		"^dmsetup message $clonedm 0 enable_hydration\$" \
		"clone_xfer: hydration is enabled after the chunk (CN18 step 5)"
	# The wrapper's table IS the allocation record (update_01.md U3 spec 3):
	# a single linear over the arena loop device, whose offset and length are
	# the units the allocator handed out.
	assert_eq "$(helper 2 "dm_state $metadm")" live \
		"clone_xfer: the clone-meta wrapper is live"
	got=$(helper 2 "dm_table $metadm")
	printf '%s\n' "$got" | grep -qE '^0 [0-9]+ linear [0-9]+:[0-9]+ [0-9]+$' ||
		die "clone_xfer: the clone-meta wrapper is not a single linear: $got"
	# U1: every dnv dm-clone carries no_discard_passdown, without exception —
	# it is what keeps the hydration blkdiscard above metadata-only instead of
	# also erasing the destination.
	#
	# The whole feature list is pinned, not just the one word: the exact
	# `2 no_hydration no_discard_passdown` agent.CloneTable emits from its
	# derived feature count (agent/dm.go:405-419), the same string CN18 step 3
	# and Appendix A name. The `dmsetup message $clonedm 0 enable_hydration`
	# asserted a few lines above does NOT weaken it — dm-clone's
	# STATUSTYPE_TABLE reprints the constructor args saved by copy_ctr_args
	# verbatim (drivers/md/dm-clone-target.c), and only `dmsetup status`
	# (STATUSTYPE_INFO) recomputes the live flags. The grep is scoped to this
	# clone's device by name, so nothing else on the node can satisfy it. A
	# substring test for no_discard_passdown alone accepts
	# `1 no_discard_passdown`, i.e. a clone created with hydration already
	# enabled, which would copy the very regions the §9.6 chunk asked to skip.
	got=$(helper 2 "dm_table $clonedm")
	printf '%s\n' "$got" |
		grep -qE ' 2 no_hydration no_discard_passdown( |$)' ||
		die "clone_xfer: dm-clone table is not '2 no_hydration no_discard_passdown': $got"
	host_wait_ana "$hv" "$nqn" 2 optimized 30
	assert_eq "$(host_ana "$hv" "$nqn" 1)" inaccessible \
		"clone_xfer: the source stays retired"

	stage sample "stage 5: the bitmap jump, then a read-through probe"
	sample=$(cnctl 2 wait-hydrated --sp "$sp2" --cntlr "$cntlr" \
		--clone "$C_CLONE" --min-first 32 --sample-only)
	if [ "${sample%%/*}" -lt "${sample##*/}" ]; then
		log "clone_xfer: read-through window HIT ($sample hydrated)"
	else
		log "clone_xfer: WARNING read-through window missed ($sample)"
	fi
	drop_caches "$hv"
	want=$(sha_range "$hv" "$WORK/pattern-c.bin" 1 31)
	assert_eq "$(sha_range "$hv" "$dev" 1 31)" "$want" \
		"clone_xfer read-through of MiB 31"

	stage wipe "stage 6: CN2 loses its kernel state and rebuilds (§11.5)"
	assert_eq "$(helper 2 "kill_role cn")" stopped "clone_xfer: cn2 stopped"
	sshv 2 "mv $CN_LOG $WORK/cn-agent.pre-wipe.log"
	helper 2 "wipe_cn $(hex16 "${CNID[2]}")"
	start_cn_agent 2
	# The listener opens only after the reconcile returns, so the wait-up
	# budget is the recovery's: a rebuilt clone re-connects, re-reads the
	# destination bitmaps and re-applies the chunk before it answers.
	out=$(cnctl 2 get-cn-size --wait 120)
	assert_eq "$(jq_of "$out" .size)" "$CN_CAPACITY" "clone_xfer cn2 size"
	out=$(cnctl 2 get-cntlr-info --sp "$sp2" --cntlr "$cntlr")
	assert_all_ok "$out" .cntlr_info "clone_xfer post-wipe cntlr"
	# The reconcile mints its own trace id, so the freshly rotated log is the
	# filter: it holds the recovery and nothing else.
	seq=$(helper 2 cn_events)
	assert_before "$seq" '^dmsetup message .* 0 reserve_metadata_snap$' \
		'^thin_dump ' "clone_xfer recovery: the metadata snapshot precedes the dump"
	assert_before "$seq" '^thin_dump ' \
		'^dmsetup message .* 0 release_metadata_snap$' \
		"clone_xfer recovery: the snapshot is always released"
	# Scoped to the clone device, because the rebuild re-allocates an arena
	# unit and so emits its own earlier blkdiscard on the loop device
	# (update_01.md U3, ruling R5.5).
	assert_before "$seq" "^blkdiscard .*/dev/mapper/$clonedm\$" \
		"^dmsetup message $clonedm 0 enable_hydration\$" \
		"clone_xfer recovery: every bitmap lands before hydration"
	# The registry is the dm table set, so a rebuilt CN reconstructs exactly
	# one allocation from an arena that was wiped along with the kernel state.
	assert_eq "$(helper 2 "clone_meta_wrappers $(hex16 "${CNID[2]}")")" \
		"$metadm" "clone_xfer recovery: exactly one kind-b wrapper"
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_eq "$(jq_of "$out" '.bm_info_list[0].bm_idx_list | @csv')" '0' \
		"clone_xfer: the chunk files survived the wipe"
	# The wipe killed the host's sp2 controller with DNR; it never reconnects
	# on its own, and -n would take the live sp1 path with it (Appendix A).
	ctrl=$(helper "$hv" "ctrl_of '$nqn' '${IP[2]}'")
	if [ "$ctrl" = none ]; then
		log "clone_xfer: the dead sp2 controller is already gone"
	else
		sshv "$hv" "nvme disconnect -d /dev/$ctrl"
	fi
	host_connect "$hv" 2 "$nqn"
	host_wait_ana "$hv" "$nqn" 2 optimized 30

	stage hydrate "stage 7: hydration runs to completion"
	cnctl 2 wait-hydrated --sp "$sp2" --cntlr "$cntlr" --clone "$C_CLONE" \
		--interval 0.5 --timeout 120 >/dev/null

	stage finalize "stage 8: DeleteTransfer, then DeleteClone (CN18 order)"
	req_set "$req1" ".xfer_list = []
		| .nqn_to_subsystem[\"$nqn\"].ns_list[0].suspended = true"
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" ns_id_to_dm_linear "$S_NS" "clone_xfer source retired"
	req_set "$req2" ".clone_list = []
		| .nqn_to_subsystem[\"$nqn\"].ns_list[0].suspended = false"
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" td_id_to_raid0 "$S_TD" "clone_xfer finalized"
	assert_map_ok "$out" ns_id_to_dm_linear "$S_NS" "clone_xfer finalized"
	seq=$(helper 2 "cn_events $TRACE")
	local nsdev2
	nsdev2=$(cn_dm_name 6 2 "$sp2" "$S_NS")
	assert_before "$seq" "^dmsetup reload $nsdev2 " "^dmsetup remove $clonedm\$" \
		"clone_xfer: the ns-dev leaves the clone before the clone goes"
	# CN18 teardown order (update_01.md U3 spec 3): the dm-clone goes first,
	# then its metadata wrapper — whose removal is what frees the arena units
	# again — and only then the source connection.
	assert_before "$seq" "^dmsetup remove $clonedm\$" \
		"^dmsetup remove $metadm\$" \
		"clone_xfer: the metadata wrapper goes after the dm-clone"
	assert_before "$seq" "^dmsetup remove $metadm\$" \
		"^nvme disconnect .*$NQN_PREFIX:4:" \
		"clone_xfer: the source connection dies last"
	# CN16 rule 5: with the clone gone the ns-dev sits on the raid0 again.
	assert_eq "$(helper 2 "dm_backing $nsdev2")" \
		"$(helper 2 "dm_devno $(cn_dm_name 4 2 "$sp2" "$S_TD")")" \
		"clone_xfer: the ns-dev is back on the raid0"
	assert_eq "$(helper 2 "dm_state $clonedm")" missing "clone_xfer dm-clone gone"
	got=$(helper 2 "clone_meta_wrappers $(hex16 "${CNID[2]}")")
	[ -z "$got" ] ||
		die "clone_xfer: the arena still holds kind-b wrappers: $got"

	stage verify "stage 9: the moved volume, read through the sp2 path"
	drop_caches "$hv"
	want=$(sha_range "$hv" "$WORK/pattern-c.bin" 32)
	assert_eq "$(sha_range "$hv" "$dev" 32)" "$want" "clone_xfer first 32 MiB"
	assert_eq "$(helper "$hv" "zero_probe '$dev' 32 32")" zeros \
		"clone_xfer second 32 MiB"
	# The differential proof: dm-clone copies regions unconditionally, so only
	# the skip leaves the second half unmapped.
	got=$(bitmap_hex 2 get-td-bm --sp "$sp2" --cntlr "$cntlr" --td "$S_TD" \
		--slice-idx 0 --start-block 0 --block-cnt 0)
	assert_eq "$got" 00000000ffffffff "clone_xfer: blocks 32..63 never copied"
	make_pattern "$hv" "$WORK/probe-c.bin" 1
	want=$(sha_range "$hv" "$WORK/probe-c.bin" 1)
	write_range "$hv" "$WORK/probe-c.bin" "$dev" 1 40
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 1 40)" "$want" "clone_xfer write probe"

	stage check "converge check rounds on both CNs"
	converge_check 1 "$sp1" "$cntlr" "$rev1"
	converge_check 2 "$sp2" "$cntlr" "$rev2"

	stage teardown "empty cntlr lists, then empty side lists"
	host_disconnect "$hv" "$nqn"
	cn_drop 1
	cn_drop 2
	dn_drop 1
	dn_drop 2
	assert_no_residue "$sp1"
	assert_no_residue "$sp2"
	sshv_ok "$hv" "rm -f $WORK/pattern-c.bin $WORK/probe-c.bin"
}

# ---------------------------------------------------------------------------
# Case D — restart (§14)
# ---------------------------------------------------------------------------

case_restart() {
	CASE=restart
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	local sp=0x3e1 c1=0x1 c2=0x2 hv=2
	local nqn="$NQN_IT_PREFIX:d:vol1"
	local uuid=66666666-6666-4666-8666-666666666666
	local req1="$WORK/req-restart-cn1.json" req2="$WORK/req-restart-cn2.json"
	local out dev want got rev1 rev2 snap idx name when muts
	diag_cntlr 1 "$sp" "$c1"
	diag_cntlr 2 "$sp" "$c2"
	dev=$(host_dev "$uuid")
	snap=$(mktemp -d)

	stage build "the A-shaped SP with a primary on CN1 and a standby on CN2"
	dn_pointers 1 "$sp:${A_MLEG[1]}:${A_MSIDE[1]}" "$sp:${A_DLEG[1]}:${A_DSIDE[1]}"
	dn_pointers 2 "$sp:${A_MLEG[2]}:${A_MSIDE[2]}" "$sp:${A_DLEG[2]}:${A_DSIDE[2]}"
	dn_side 1 "$sp" "${A_MLEG[1]}" "${A_MSIDE[1]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 1 "$sp" "${A_DLEG[1]}" "${A_DSIDE[1]}" 2 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_MLEG[2]}" "${A_MSIDE[2]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_DLEG[2]}" "${A_DSIDE[2]}" 2 "${CNID[1]}" "${CNID[2]}"
	bump_cn_sync 1
	out=$(cnctl 1 syncup-cn --revision "${CNREV[1]}" --cntlr "$sp:$c1")
	assert_cn_info_ok "$out" "restart syncup-cn 1"
	bump_cn_sync 2
	out=$(cnctl 2 syncup-cn --revision "${CNREV[2]}" --cntlr "$sp:$c2")
	assert_cn_info_ok "$out" "restart syncup-cn 2"
	req_raid1 "$req1" 1 "$sp" "$c1" 0 true "$nqn" "$uuid"
	req_raid1 "$req2" 2 "$sp" "$c2" 1 false "$nqn" "$uuid"
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "restart primary"
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "restart standby"

	stage data "the host connects both paths and writes its test data"
	host_connect "$hv" 1 "$nqn"
	host_connect "$hv" 2 "$nqn"
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	host_wait_ana "$hv" "$nqn" 1 optimized 20
	make_pattern "$hv" "$WORK/pattern-restart.bin" 4
	want=$(sha_range "$hv" "$WORK/pattern-restart.bin" 4)
	write_range "$hv" "$WORK/pattern-restart.bin" "$dev" 4
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 4)" "$want" "restart pre-restart readback"

	stage snapshot "record the pre-restart infos"
	cnctl 1 get-cn-info >"$snap/cn1.pre.raw"
	cnctl 2 get-cn-info >"$snap/cn2.pre.raw"
	cnctl 1 get-cntlr-info --sp "$sp" --cntlr "$c1" >"$snap/cntlr1.pre.raw"
	cnctl 2 get-cntlr-info --sp "$sp" --cntlr "$c2" >"$snap/cntlr2.pre.raw"

	stage restart "kill both cn agents; the kernel-side state is untouched"
	for idx in 1 2; do
		assert_eq "$(helper "$idx" "kill_role cn")" stopped \
			"restart: cn$idx stopped"
	done
	# The data path does not depend on the agent process; the dn agents never
	# stop.
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 4)" "$want" \
		"restart readback while the cn agents are down"
	for idx in 1 2; do
		sshv "$idx" "mv $CN_LOG $WORK/cn-agent.pre-restart.log"
		start_cn_agent "$idx"
	done
	for idx in 1 2; do
		# The listener opens only after the startup reconcile returns.
		out=$(cnctl "$idx" get-cn-size --wait 60)
		assert_eq "$(jq_of "$out" .size)" "$CN_CAPACITY" "restart cn$idx size"
	done
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 4)" "$want" \
		"restart readback after the cn agents are back"

	stage reconcile "the reloaded state deep-equals the pre-restart snapshot"
	# ResInfo.epoch is the time of the last observed status change and the
	# trackers are in-memory; leg_id_to_leg[].details carries the CN11 prober's
	# own timestamps, and the probers restart with the agent.
	local norm='walk(if type == "object" and has("epoch") then del(.epoch) else . end)
		| if .cntlr_info.leg_id_to_leg then
		    .cntlr_info.leg_id_to_leg |= with_entries(.value |= del(.details))
		  else . end'
	cnctl 1 get-cn-info >"$snap/cn1.post.raw"
	cnctl 2 get-cn-info >"$snap/cn2.post.raw"
	cnctl 1 get-cntlr-info --sp "$sp" --cntlr "$c1" >"$snap/cntlr1.post.raw"
	cnctl 2 get-cntlr-info --sp "$sp" --cntlr "$c2" >"$snap/cntlr2.post.raw"
	for name in cn1 cn2 cntlr1 cntlr2; do
		for when in pre post; do
			"$JQ" "$norm" "$snap/$name.$when.raw" >"$snap/$name.$when.json"
		done
		diff -u "$snap/$name.pre.json" "$snap/$name.post.json" >&2 ||
			die "restart: $name differs across the restart"
	done
	# An active array is recognized, not re-assembled.
	for idx in 1 2; do
		got=$(helper "$idx" cn_events | grep -E '^mdadm ' |
			grep -Ev -- '--detail|--examine' || true)
		[ -z "$got" ] ||
			die "restart: cn$idx ran a mutating mdadm:"$'\n'"$got"
	done

	stage idempotent "same-revision re-applies must mutate nothing"
	# SyncupCn and SyncupCntlr share one counter but store their revisions
	# separately (§9), so the equal-revision re-send of each is the revision
	# that call stored: CNSYNC for the node, the case's own rev for the cntlr.
	out=$(cnctl 1 syncup-cn --revision "${CNSYNC[1]}" --cntlr "$sp:$c1")
	assert_cn_info_ok "$out" "restart re-apply cn1"
	out=$(cnctl 2 syncup-cn --revision "${CNSYNC[2]}" --cntlr "$sp:$c2")
	assert_cn_info_ok "$out" "restart re-apply cn2"
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "restart re-apply cntlr1"
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "restart re-apply cntlr2"
	# The post-restart log covers the startup reconcile and these re-applies.
	for idx in 1 2; do
		muts=$(helper "$idx" mutations)
		[ -z "$muts" ] ||
			die "restart: cn$idx mutated after the restart:"$'\n'"$muts"
	done

	stage stale "only a stale rejection proves the revision survived"
	cnctl 1 syncup-cn --revision "$((CNSYNC[1] - 1))" --cntlr "$sp:$c1" \
		--expect-code 1 >/dev/null

	stage check "converge check rounds on both CNs"
	converge_check 1 "$sp" "$c1" "$rev1"
	converge_check 2 "$sp" "$c2" "$rev2"

	stage teardown "empty cntlr lists, then empty side lists"
	host_disconnect "$hv" "$nqn"
	cn_drop 1
	cn_drop 2
	dn_drop 1
	dn_drop 2
	assert_no_residue "$sp"
	sshv_ok "$hv" "rm -f $WORK/pattern-restart.bin"
	rm -rf "$snap"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/cnagent_test.sh [--only <case>] [--cleanup-only] \\
           <user@vm1> <user@vm2>

cases: ${CASES[*]}
EOF
	exit 2
}

parse_args() {
	local positional=()
	while [ $# -gt 0 ]; do
		case "$1" in
		--only)
			ONLY=$2
			shift 2
			;;
		--only=*)
			ONLY=${1#*=}
			shift
			;;
		--cleanup-only)
			CLEANUP_ONLY=1
			shift
			;;
		-h | --help) usage ;;
		-*) usage ;;
		*)
			positional+=("$1")
			shift
			;;
		esac
	done
	[ "${#positional[@]}" -eq 2 ] || usage
	VM[1]=${positional[0]}
	VM[2]=${positional[1]}
	IP[1]=${VM[1]##*@}
	IP[2]=${VM[2]##*@}
	if [ -n "$ONLY" ]; then
		local known=0 name
		for name in "${CASES[@]}"; do
			[ "$name" = "$ONLY" ] && known=1
		done
		[ "$known" -eq 1 ] || usage
	fi
}

main() {
	parse_args "$@"
	trap on_exit EXIT
	log "driver: $(hostname), vm1=${VM[1]} (${IP[1]}), vm2=${VM[2]} (${IP[2]})"

	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		ship_helper
		STAGE="cleanup-only"
		log "=== cleanup only"
		cleanup_all
		return 0
	fi

	preflight_driver
	ship_helper
	STAGE="start cleanup"
	log ""
	log "=== start-of-run cleanup (unconditional)"
	cleanup_all
	preflight_vms
	setup

	local name
	for name in "${CASES[@]}"; do
		if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then continue; fi
		"case_$name"
	done
}

main "$@"
