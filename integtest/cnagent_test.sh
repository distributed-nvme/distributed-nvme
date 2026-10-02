#!/usr/bin/env bash
#
# cnagent_test.sh — the `dnv-agent cn` integration test of
# doc/cnagent_integtest.md. Two real VMs, real nvme-tcp/md-raid1/dm-thin/
# dm-clone/nvmet, driven over gRPC from this machine by integtest/cnagentctl,
# with real `dnv-agent dn` instances (driven by integtest/dnagentctl) as the
# side backing.
#
#   bash integtest/cnagent_test.sh [--only <case>] [--cleanup-only] \
#       [--wipe] user1@ip1 user2@ip2
#
# Cases (cnagent_integtest.md, Cases): smoke, redund, teardown, thinbm,
# clone_xfer, restart; `teardown`, the teardown-by-sweep case, injects the
# faults (a pinned dm device, an iptables partition of the nvme-tcp port, a
# background writer); case A's `degrade` stage reuses the partition. Cleanup
# runs unconditionally at the start and, on success only, at the end: a
# failing run leaves every dm/md/nvmet object and all four agent logs in place
# and dumps diagnostics (cnagent_integtest.md, Teardown and cleanup).
# cleanup_phase1 releases every fault injector
# unconditionally, so a failed run of either case cannot poison the next one.
#
# The uutils dd rule (dnagent_integtest.md, Assumptions and preflight checks)
# is absolute: this script never passes iflag= or
# oflag= to dd. Writes use conv=fsync, reads that must hit the media are
# preceded by a cache drop. Do not "fix" this back to direct IO.
#
# Both roles run from one binary named `dnv-agent`, so `pkill -x dnv-agent`
# would kill both (cnagent_integtest.md, Topology). Every kill here is
# `pkill -f 'dnv-agent dn'` / `pkill -f 'dnv-agent cn'`. Do not "simplify" it.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CTL_DN="$REPO_ROOT/integtest/bin/dnagentctl"
CTL_CN="$REPO_ROOT/integtest/bin/cnagentctl"
AGENT_BIN="$REPO_ROOT/bin/dnv-agent"

# Deliberately not the dn suite's directory, so the two suites never share
# debris. The driver uses it too, for the request files (cnagent_integtest.md,
# The driver: `cnagentctl`).
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
# GetDnSize reports the [D13] data area, not the raw device
# (cnagent_integtest.md, Cases, the setup paragraph): 2 GiB
# backing file, 2147483648 - 268435456 = 1879048192.
DATA_SIZE=1879048192
# --capacity: an arbitrary exact value GetCnSize must echo back (CN-CM1;
# cnagent_integtest.md, Cases, the setup paragraph).
CN_CAPACITY=1099511627776

# Bdev parameters, everywhere.
BLOCK_SIZE=1048576
STRIPE_SIZE=65536
BM_CHUNK_BLOCKS=128
LOW_WATER_PCT=50
TD_SIZE=67108864 # 64 MiB = 64 thin blocks

# The arithmetic of architecture.md,
# Group on-leg layout: meta region, data region, health block, worked for
# these sizes. The request carries these; the agent consumes them and never
# recomputes.
NONE_META_BLOCKS=1
NONE_EXT1_DATA=63
NONE_EXT2_DATA=127
RAID1_META_BLOCKS=3
RAID1_EXT1_DATA=61
RAID1_EXT2_DATA=125

# Per-RPC deadlines for the converge RPCs (see dnctl/cnctl).
DN_SYNCUP_TIMEOUT=60
CN_SYNCUP_TIMEOUT=180

# Polling budget of `dnagentctl wait-zeroed` (architecture.md,
# Side provisioning protocol). With 64 MiB
# extents on a loop device the kernel maps REQ_OP_WRITE_ZEROES onto fallocate,
# so a 1-2 extent side finishes in well under a second; the budget only has to
# cover a stalled retry loop (DnZeroRetryInterval = 5 s).
ZERO_TIMEOUT=120

NQN_PREFIX=nqn.2024-01.io.dnv
NQN_IT_PREFIX=nqn.2024-01.io.dnv-it
HOST_NQN=nqn.2024-01.io.dnv-it:host:0

# Per-VM state, 1-indexed so "vm1"/"vm2" read directly. Each VM runs both
# agents (cnagent_integtest.md, Topology): dn 0x1/0x2 on :29528, cn 0x11/0x12
# on :29529.
VM=("" "" "")
IP=("" "" "")
DNID=("" 0x1 0x2)
CNID=("" 0x11 0x12)
DNREV=("" 0 0)
CNREV=("" 0 0)
CNSYNC=("" 0 0)
LOOP=("" "" "")

# The S-shaped SP (cnagent_integtest.md, Cases, smoke): one RedundNone meta
# group and one RedundNone data
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

# The A-shaped SP (cnagent_integtest.md, Cases, redund): md-raid1 meta and
# data groups across both DNs, a
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
WIPE=0
CASE="setup"
TRACE="it-setup"
STAGE="(startup)"
SETUP_DONE=0
DIAG_CNTLRS=()

CASES=(smoke redund teardown thinbm clone_xfer restart)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id the
# driver sends (cnagent_integtest.md, The driver: `cnagentctl`): one id shared
# by the driver call, the agent handler and
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

# assert_parked pins the park (architecture.md, Namespace suspend semantics)
# of one effectively suspended namespace:
# the ns-dev is **live** and its table is a plain dm-linear over its td's
# `CnErrorName`, offset 0 (CN16 rule 1). Before 2026-09-16 that namespace was
# held `dmsetup suspend`ed on its ordinary backing instead; asserting `live`
# alone would not catch a park onto the wrong device, and asserting the table
# alone would not catch one left suspended, so both halves are here.
assert_parked() { # vm nsdev tderrdev label
	local vm=$1 nsdev=$2 errdev=$3 label=$4 errno got
	assert_eq "$(helper "$vm" "dm_state $nsdev")" live "$label: ns-dev is live"
	errno=$(helper "$vm" "dm_devno $errdev")
	[ "$errno" != none ] || die "$label: the td's dm-error $errdev is gone"
	got=$(helper "$vm" "dm_table $nsdev")
	case "$got" in
	"0 "*" linear $errno 0") ;;
	*) die "$label: ns-dev table is '$got', want '0 <sectors> linear $errno 0'" ;;
	esac
}

# assert_opens_eio is the observable the park exists for: a local block-device
# walker that opens a parked ns-dev gets an IO error *within the timeout*
# instead of wedging in uninterruptible D state, which is what the old held
# suspension did to it ([D12]).
assert_opens_eio() { # vm nsdev label
	local rc
	rc=$(helper "$1" "open_rc $2")
	[ "$rc" != 124 ] || die "$3: reading the parked ns-dev blocked (timed out)"
	[ "$rc" != 0 ] || die "$3: reading the parked ns-dev succeeded"
}

# assert_opens_ok is assert_opens_eio's positive control, and the reason the
# suite can trust it: `open_rc` reports a raw `dd` status, so a helper broken
# end to end — a device name that does not exist, a dd that rejects an
# argument — returns non-zero and would make every assert_opens_eio pass
# vacuously. One call on a device that MUST serve keeps the pair honest.
assert_opens_ok() { # vm dev label
	local rc
	rc=$(helper "$1" "open_rc $2")
	[ "$rc" = 0 ] || die "$3: a serving device did not read back (rc $rc)"
}

# assert_no_suspended_dm is the operator-visible promise of architecture.md,
# Namespace suspend semantics: on a CN, outside
# a DN cutover window, `dmsetup info` shows no suspended dnv device at all.
# Before the park a transfer origin's ns-dev sat here for the whole hydration.
assert_no_suspended_dm() { # vm label
	local got seen
	# An empty answer only means something once we know the listing produced
	# rows at all: the helper swallows dmsetup's stderr, so a broken pipeline
	# and a clean node look identical from here.
	seen=$(helper "$1" "dnv_dm_cnt")
	[ "${seen:-0}" -gt 0 ] ||
		die "$2: vm$1's dm listing shows no dnv device at all — the sweep " \
			"cannot be trusted"
	got=$(helper "$1" "suspended_agent_dms")
	[ -z "$got" ] || die "$2: vm$1 holds suspended dnv devices: $got"
}

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
# at provisioned = false (architecture.md, Side provisioning protocol, the
# Converge matrix rows 2 and 3):
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

# assert_leg_pending reads a primary's leg row in the reply of the SyncupCntlr
# that started the leg's CN11 prober, and pins it exactly: RES_STATUS_PENDING
# "health probe pending", with no race. ensureLegs builds the row one
# wrapper-table check after startLegProber registers the prober, whose first
# round runs one CnLegProbeInterval (5 s) later, and nothing later in the
# converge rewrites the row. OK comes only from a prober an earlier converge
# registered; converge_check asserts it once the round is in.
assert_leg_pending() { # json legid label
	local row
	row=".cntlr_info.leg_id_to_leg[\"$(d16 "$2")\"]"
	assert_eq "$(jq_of "$1" "$row.status // \"ABSENT\"")" \
		RES_STATUS_PENDING "$3 leg_id_to_leg[$2]"
	assert_eq "$(jq_of "$1" "$row.details // \"ABSENT\"")" \
		"health probe pending" "$3 leg_id_to_leg[$2] details"
}

# assert_provisioning is the exact form, for the rows the matrix pins to
# PROVISIONING with no race: the resources a deferred side or leg deliberately
# does not create. RES_STATUS_PROVISIONING is a *healthy* status, so it
# satisfies a bare assert_not_ok and trips every assert_all_ok — naming it
# explicitly is what keeps both honest (cnagent_integtest.md, Conventions,
# Negatives are exact).
assert_provisioning() { # json path label
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	[ "$got" = "RES_STATUS_PROVISIONING" ] ||
		die "$3: status is $got, want RES_STATUS_PROVISIONING"
}

# assert_suppressed reads a resource CN19 gated off: not OK, with "sp_level" as
# the reason. The details check is what keeps it exact now that
# RES_STATUS_PROVISIONING also satisfies "not OK" — a deferred resource reports
# "provisioning", never "sp_level" (CN19; cnagent_integtest.md, Conventions,
# Negatives are exact).
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

# assert_map_status is assert_map_ok's exact form for a row that must NOT be
# OK: the status itself, and a substring its details must hold.
assert_map_status() { # json field id status details-substring label
	local key got det
	key=$(d16 "$3")
	got=$(jq_of "$1" ".cntlr_info.$2[\"$key\"].status // \"ABSENT\"")
	det=$(jq_of "$1" ".cntlr_info.$2[\"$key\"].details // \"\"")
	[ "$got" = "$4" ] || die "$6 $2[$3]: status is $got ($det), want $4"
	case "$det" in
	*"$5"*) ;;
	*) die "$6 $2[$3]: details '$det' do not hold '$5'" ;;
	esac
}

# assert_thin_ok checks td_id_to_thin_info[td].slice_id_to_dm_thin[slice].
assert_thin_ok() { # json td slice label
	assert_ok "$1" \
		".cntlr_info.td_id_to_thin_info[\"$(d16 "$2")\"].slice_id_to_dm_thin[\"$(d16 "$3")\"].status" \
		"$4 thin[$2][$3]"
}

# assert_cn_info_ok covers the four base-state resources of CnInfo
# (architecture.md, Controller node, common). The
# fifth — the old clone-VG row — went away with LVM: the clone-metadata arena
# is now the loop device itself (loop_dev_info) plus the kind-cb wrapper dm
# tables, and the proto field is `reserved 5` ([D14]).
assert_cn_info_ok() { # json label
	local field
	for field in port_info tmpfs_info tmp_file_info loop_dev_info; do
		assert_ok "$1" ".cn_info.$field.status" "$2 $field"
	done
}

# assert_all_ok fails on any ResInfo under a reply subtree whose status is not
# OK — the converge-check rule "every expected status == RES_STATUS_OK"
# (cnagent_integtest.md, Conventions, Steady state).
# It also fails on a subtree that holds no status at all: an empty selection
# has no not-OK member, so without the count a reply with code 0, the right
# revision and no cntlr_info whatsoever passed every check round (`..` over
# an absent path selects nothing rather than failing). A not-OK row with no
# res_name is listed as "(unnamed)": protojson omits an empty string, join
# turns the resulting null into "", and so a bare .res_name let such a row
# pass as no failure at all.
assert_all_ok() { # json path label
	local cnt bad
	cnt=$(jq_of "$1" "[$2 | .. | objects | select(has(\"status\"))] | length")
	[ "${cnt:-0}" -gt 0 ] || die "$3: no resource status at all under $2"
	bad=$(jq_of "$1" \
		"[$2 | .. | objects | select(has(\"status\")) | select(.status != \"RES_STATUS_OK\") | (.res_name // \"(unnamed)\")] | join(\",\")")
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
		log "########## diagnostics (cnagent_integtest.md, Teardown and cleanup) ##########"
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
# goes through sudo (cnagent_integtest.md, Topology).
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
# The drivers' 10 s default suits the read-only RPCs, but one converge pass
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

# bump_dn_rev/bump_cn_rev advance a node's monotonic revision counter
# (cnagent_integtest.md, Conventions, Revisions).
# They must run in the parent shell — never inside a command substitution or a
# background job, both of which would increment a copy. On a CN one counter is
# shared by SyncupCn and SyncupCntlr (the single CnRev of architecture.md,
# Revision keys and the sync fan-out), so bump_cn_sync
# additionally records the revision SyncupCn stored: that, not the counter, is
# what CheckCn echoes back.
bump_dn_rev() { DNREV[$1]=$((DNREV[$1] + 1)); }
bump_cn_rev() { CNREV[$1]=$((CNREV[$1] + 1)); }
bump_cn_sync() {
	bump_cn_rev "$1"
	CNSYNC[$1]=${CNREV[$1]}
}

# ---------------------------------------------------------------------------
# Derived names (architecture.md, Naming) — the bash mirror of
# common/name_fmt.go
# ---------------------------------------------------------------------------

hex16() { printf '%016x' "$(($1))"; }

# d16 is hex16's twin for protojson, which renders 64-bit fields as decimal
# strings (cnagent_integtest.md, The driver: `cnagentctl`).
d16() { printf '%u' "$(($1))"; }

# cn_dm_name mirrors every cn dm formatter: dnv-{cluster}-{cn}-{kind}-{ids…}
# with the kind fields of architecture.md, dm device names — every cn kind is
# the role letter `c` in front of
# the old digit: c0 pool-meta, c1 pool-data, c2 pool-final, c3 thin, c4 raid0,
# c5 error, c6 ns-dev, c7 clone-final, c8 xfer-final, c9 leg, ca group,
# cb clone-meta.
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

# clone_meta_dm mirrors common.CnCloneMetaDmName (CN18): the
# kind-cb dm-linear wrapper that carries one dm-clone's metadata, allocated out
# of the loop-device arena. It replaced the clone VG's metadata LV, and with it
# the doubled-dash `dnv--clone--vg-*` gotcha — a wrapper is created by the
# agent with `dmsetup create` and carries single dashes.
clone_meta_dm() { # cnidx sp clone
	cn_dm_name cb "$1" "$2" "$3"
}

# host_dev is the host-side multipath node of a namespace. The ns uuids are
# fixed inputs, so no lookup is needed.
host_dev() { printf '/dev/disk/by-id/nvme-uuid.%s' "$1"; }

# ---------------------------------------------------------------------------
# Host emulation (cnagent_integtest.md, Topology): the host role is plain
# nvme-tcp with one fixed hostnqn,
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

# leg_wait_ana waits for one leg's path on one CN to reach an ANA state. In a
# failover it is the ordering barrier of cnagent_integtest.md, Conventions,
# Sides first: a promote that converges before its legs have optimized paths
# finds them unavailable (architecture.md,
# "Make sure all groups are available") and fails the md
# assembly of its own converge. Since 2026-09-26 the agent's CN10 retry
# finishes that assembly once they are optimized (cnagent.md CN12), but case
# A's promote stage asserts its md rows OK and both --assembles under the
# promote's own trace id, which holds only behind the barrier. Case A's
# lateflip stage promotes before any side flips on purpose and calls this
# afterwards, only to wait for the flips to land.
leg_wait_ana() { # cnvm sp leg cn dnidx want secs
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "wait_ana '$nqn' '${IP[$5]}' '$6' '$7'" ||
		die "cn$4's path to dn$5 of leg $3 never reached ANA state '$6'"
}

# leg_wait_not_live is leg_wait_ana's negative twin and the only barrier in the
# suite that waits for a path to DIE. It exists for the partition stages (case
# T's S4, case A's degrade): an ANA probe is the wrong instrument there,
# because a leg is connected with ctrl_loss_tmo = -1 (agent/nvmehost.go) and a
# partitioned DN answers its reconnects with nothing at all — no DNR refusal,
# which is what deletes a controller whatever that timeout says (case T's
# S2) — so its controller never goes away and its per-path ana_state attribute
# keeps reading whatever the last ANA log carried, while `State` moves to
# `connecting` within one keep-alive interval. The polling itself is the
# VM's (one ssh round trip, not one per sample), exactly as wait_ana's is.
leg_wait_not_live() { # cnvm sp leg cn dnidx secs
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "wait_path_not_live '$nqn' '${IP[$5]}' '$6'" ||
		die "cn$4's path to dn$5 of leg $3 never left the 'live' state"
}

# leg_wait_live is leg_wait_not_live's positive twin: the barrier that a
# partitioned path came back (case A's degrade). It reads `State` for the
# same reason — ana_state kept reading `optimized` through the partition, so
# a wait on it passes at its first sample.
leg_wait_live() { # cnvm sp leg cn dnidx secs
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "wait_path_live '$nqn' '${IP[$5]}' '$6'" ||
		die "cn$4's path to dn$5 of leg $3 never came back to 'live'"
}

# ---------------------------------------------------------------------------
# Data IO on the host VM: writes fsync, reads follow a cache drop, never
# iflag=/oflag= (dnagent_integtest.md, Assumptions and preflight checks).
# ---------------------------------------------------------------------------

drop_caches() { sshv "$1" "sync; echo 3 > /proc/sys/vm/drop_caches"; }

sha_range() { # vm path countMiB [skipMiB]
	sshv "$1" "dd if=$2 bs=1M count=$3 skip=${4:-0} status=none | sha256sum | cut -d' ' -f1"
}

# write_range's status is dd's: `&& sync`, never `; sync`, whose 0 would
# hide a write that failed (case A's degrade waits on one in the background).
write_range() { # vm src dst countMiB [seekMiB]
	sshv "$1" "dd if=$2 of=$3 bs=1M count=$4 seek=${5:-0} conv=fsync status=none && sync"
}

make_pattern() { # vm path countMiB
	sshv "$1" "dd if=/dev/urandom of=$2 bs=1M count=$3 conv=fsync status=none"
}

# ---------------------------------------------------------------------------
# The --req request files (cnagent_integtest.md, The driver: `cnagentctl`)
# ---------------------------------------------------------------------------
#
# SyncupCntlrRequest is far too deep for flags, so every converge is a
# protojson file under $WORK **on the driver** (cnagentctl runs here, not on
# the VMs). A case writes the file once with req_none/req_raid1 and from then
# on edits only the fields that change with req_set (cnagent_integtest.md,
# The driver: `cnagentctl`): revision, sp_level,
# cntlr.primary, ns_list[].suspended, and the clone and xfer lists.
#
# protojson wants original snake_case names, 64-bit integers as decimal strings
# and enum value names; the id_to_slice key is sprintf("%016x", slice_id) per
# architecture.md, `service ControllerNodeAgent`, and the nqn_to_subsystem
# key is the literal NQN.

req_tr() { # traddr
	printf '{"tr_type": "tcp", "adr_fam": "ipv4", "tr_addr": "%s", "tr_svc_id": "%s"}' \
		"$1" "$TR_SVC_ID"
}

req_cntlr() { # cnidx cntlid_slot primary
	printf '{"addr_port": "%s:%s", "nvme_tr_conf": %s, "cntlid_slot": %s, "primary": %s, "disabled": false}' \
		"${IP[$1]}" "$CN_GRPC_PORT" "$(req_tr "${IP[$1]}")" "$2" "$3"
}

# req_side renders one Side. provisioned is always true here: dn_side has
# already run the two-phase provisioning of architecture.md,
# Side provisioning protocol, before any CN sees the side, so
# the desired state the sp-worker would publish at this point already carries
# its flip. Without the flag every leg would be provisioning-deferred and the
# cases would build the error-backed shape of cnagent.md CN9, CN16 rule 0 and
# CN17 — each td's
# dm-error, the ns-devs on it and any transfer's `CnXferFinalName` as an error
# table — instead of their real stacks.
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
# and the sp-worker's materialization flip (dnv-worker.md RW19). A
# request carrying `created: true` is the one a real worker publishes after
# the flip, and the agent then sends no pool message for that td at all.
req_td() { # tdid dev_id ori_id [created]
	printf '{"td_id": "%s", "dev_id": %s, "ori_id": %s, "size": "%s", "created": %s}' \
		"$(d16 "$1")" "$2" "$3" "$TD_SIZE" "${4:-false}"
}

# req_ns renders one Namespace. nguid is always the uuid with the dashes
# removed.
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

# The Clone record carries no chunk count (architecture.md, Clones): how many
# chunks a clone holds is how many CloneBitmap keys it has. cnagentctl parses
# this request with strict protojson, so a leftover "bm_cnt" key would fail
# the call outright rather than being ignored.
req_clone() { # cloneid src_nqn src_vm dst_tdid auto_resume
	printf '{"clone_id": "%s", "src_tr_conf_list": [%s], "src_nqn": "%s", "src_ns_idx": 1, "src_slice_cnt": 1, "src_stripe_size": "%s", "src_block_size": "%s", "dst_td_id": "%s", "dm_clone_conf": {"hydration_threshold": 1, "hydration_batch_size": 1}, "auto_resume": %s}' \
		"$(d16 "$1")" "$(req_tr "${IP[$3]}")" "$2" "$STRIPE_SIZE" \
		"$BLOCK_SIZE" "$(d16 "$4")" "$5"
}

join_json() {
	local IFS=,
	printf '%s' "$*"
}

# req_none writes a whole request file (cnagent_integtest.md,
# The driver: `cnagentctl`): the S-shaped RedundNone SP, one
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
# allowed_hosts is the emulated host role's NQN (cnagent.md CN16).
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
# ever touched (cnagent_integtest.md, The driver: `cnagentctl`), which is why
# the edit is a jq assignment and not a
# regenerated file.
req_set() { # file jq-filter
	local tmp="$1.tmp"
	"$JQ" "$2" "$1" >"$tmp" || die "editing $1 failed: $2"
	mv "$tmp" "$1"
}

# cn_syncup_cntlr posts one request file at the CN's current revision. The
# caller bumps the counter first, in the parent shell (cnagent_integtest.md,
# Conventions, Revisions), so this is safe
# inside a command substitution. Extra arguments go to cnagentctl.
cn_syncup_cntlr() { # cnidx file [cnagentctl args...]
	req_set "$2" ".revision = \"${CNREV[$1]}\""
	cnctl "$1" syncup-cntlr --req "$2" "${@:3}"
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
# TR_SVC_ID mirrors the driver's constant of the same name: the nvme-tcp port
# every agent listens on, and so the only port the partition stages block
# (case T's S4, case A's degrade).
# It is duplicated here because this heredoc is quoted — nothing of the
# driver's expands into it — and a partition aimed at the wrong port would
# black-hole nothing and report success.
TR_SVC_ID=4200
# The case-T fault-injection state files, all under /var/tmp rather than $WORK
# so the end-of-run `rm -rf $WORK` is not what releases them: cleanup_phase1 is
# (see unpin_all / stop_writer).
PIN_PREFIX=/var/tmp/dnv-it-pin
WRITER_FLAG=/var/tmp/dnv-it-writer.run
WRITER_PID=/var/tmp/dnv-it-writer.pid
WRITER_RES=/var/tmp/dnv-it-writer.res

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

# wait_path_not_live <nqn> <traddr> <secs> — polls one path's State until it is
# anything but `live`, and prints what it became. See leg_wait_not_live on the
# driver for why State and not ana_state.
wait_path_not_live() {
	local got=none i
	for ((i = 0; i < $3 * 2; i++)); do
		got=$(path_field "$1" "$2" State)
		if [ "$got" != live ]; then
			echo "$got"
			return 0
		fi
		sleep 0.5
	done
	echo "the path via $2 is still '$got' after $3s" >&2
	return 1
}

# wait_path_live <nqn> <traddr> <secs> — wait_path_not_live's positive twin:
# polls one path's State until it is `live` again, and prints it.
wait_path_live() {
	local got=none i
	for ((i = 0; i < $3 * 2; i++)); do
		got=$(path_field "$1" "$2" State)
		if [ "$got" = live ]; then
			echo "$got"
			return 0
		fi
		sleep 0.5
	done
	echo "the path via $2 is '$got' after $3s" >&2
	return 1
}

# wait_write_blocked <dev> <secs> — polls until the dd writing to <dev> sits
# in uninterruptible sleep (a `D` in ps's stat column), i.e. its IO has not
# come back yet, and prints that stat. Case A's degrade reads it on the host
# VM for the background write the partition is there to catch. It matches the
# dd's own `of=<dev>` argument in D state: the `bash -c` wrapper around it
# carries the same argument but only ever sleeps interruptibly. It screens
# rather than proves: a write to a live array sits in D for milliseconds,
# which a 0.2 s poll rarely lands in, and one caught there passes the stage
# only if its ssh session is still running at the kill -0 that follows.
wait_write_blocked() {
	local st i
	for ((i = 0; i < $2 * 5; i++)); do
		st=$(ps -eo stat=,args= 2>/dev/null |
			awk -v w="of=$1" '$1 ~ /^D/ && index($0, w) { print $1; exit }')
		if [ -n "$st" ]; then
			echo "$st"
			return 0
		fi
		sleep 0.2
	done
	echo "no write to $1 in D state on this VM after $2s" >&2
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
# errors writes; conv=fsync is what turns that into a non-zero dd
# (cnagent_integtest.md, Lab facts).
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

# open_rc <name> — the exit status of a bounded 512-byte read of one dm device,
# after a cache drop so the read has to reach the target. 0 means the device
# served it, **124 means the read BLOCKED** — the uninterruptible D state a
# dm-suspended device puts any opener into — and anything else is an IO error.
# A parked ns-dev (architecture.md, Namespace suspend semantics) must give an
# IO error, promptly; that difference is
# the whole reason the park replaced the suspension. No iflag=/oflag= here:
# the VMs' uutils dd mis-handles direct IO
# (dnagent_integtest.md, Assumptions and preflight checks).
#
# `timeout 5 dd` is NOT enough and must not be "simplified" back to it
# (measured on the lab kernel): against a dm-suspended device `timeout` fires,
# its SIGTERM does nothing to a task in uninterruptible sleep, and `timeout`
# then waits for that child — so the helper blocks until somebody resumes the
# device, and a regression would HANG the suite instead of failing it. The
# reader therefore runs detached and its status arrives through a file; after
# the deadline this returns 124 and leaves the wedged `dd` for
# `resume_suspended` to release at cleanup.
open_rc() {
	local out=/tmp/dnv-open-rc.$$ i
	rm -f "$out"
	# `drop_caches` alone, deliberately: it drops CLEAN pages, which is all a
	# raw dm device this suite only ever reads can hold, and it is what makes
	# the read below reach the target. A `sync` here would be the one call in
	# the function that no deadline covers — `timeout` cannot end a task in
	# uninterruptible sleep, which is the whole finding recorded above — so on
	# the very regression this helper reports it would hang ahead of the
	# bounded reader.
	echo 3 >/proc/sys/vm/drop_caches 2>/dev/null
	# The stdout/stderr redirection on the GROUP is load-bearing, not tidiness:
	# every caller reads this function through `$(…)`, and command substitution
	# waits for EOF on its pipe — which a background child still holding that
	# fd never gives. Redirecting only `dd` leaves the subshell holding it, and
	# the `$(…)` blocks exactly as long as the read does.
	{
		dd if="/dev/mapper/$1" of=/dev/null bs=512 count=1 status=none
		echo $? >"$out"
	} >/dev/null 2>&1 &
	for ((i = 0; i < 50; i++)); do
		[ -s "$out" ] && break
		sleep 0.1
	done
	if [ -s "$out" ]; then
		cat "$out"
	else
		echo 124
	fi
	rm -f "$out"
	return 0
}

ss_attr() { cat "$NVMET/subsystems/$1/$2" 2>/dev/null || echo MISSING; }

ns_attr() { cat "$NVMET/subsystems/$1/namespaces/$2/$3" 2>/dev/null || echo MISSING; }

allowed_hosts() { ls "$NVMET/subsystems/$1/allowed_hosts" 2>/dev/null || true; }

port_linked() {
	if [ -e "$NVMET/ports/1/subsystems/$1" ]; then echo linked; else echo no; fi
}

mdstat() { cat /proc/mdstat 2>/dev/null || true; }

# ctrl_of <nqn> <traddr> — the controller device backing one path, so a dead
# path can be disconnected by device instead of by NQN (cnagent_integtest.md,
# Cases, clone_xfer).
ctrl_of() { path_field "$1" "$2" Name; }

# --- fault injection (case T; the partition also case A's degrade) -----------
#
# The teardown-by-sweep case needs three things the other cases do not (case
# A's degrade stage reuses the second, the partition): a dm device that cannot
# be removed, a leg whose remote stops answering without ever being taken down
# cleanly, and host IO in flight across a teardown. All three live here rather
# than in the case, because all three are VM-side state that outlives the
# stage that created it — cleanup_phase1 releases every one of
# them unconditionally, on every run, whether this run used them or not.

# pin_dev <dm name> — holds an open fd on one dm device, so `dmsetup remove`
# fails EBUSY. That is the one leftover shape a sweep can produce with no
# remote dead anywhere, which is what makes it the right instrument for
# "a leftover is reported and retried" as opposed to "a teardown survives a
# dead remote".
#
# The open is read-only and NOT exclusive, deliberately: md holds its member
# devices exclusively, and an exclusive open here would also block the
# `mdadm --stop` in the layer above the wrapper, so the stage would be pinning
# the wrong failure. `exec 3<dev; exec sleep 3600` carries the descriptor
# across the exec, so the holder is a bare sleep with no shell behind it and
# with the device still open.
#
# BOTH stdout and stderr go to /dev/null, and not for tidiness: every caller
# reads this function through `$(…)` over ssh, and command substitution waits
# for EOF on its pipe — which a detached child still holding that descriptor
# never gives. This is the same trap open_rc documents at length.
#
# The pid comes from the CHILD and not from `$!`, and it is written AFTER the
# open. `$!` names setsid, which forks instead of execing when it is already a
# process-group leader — in that case the recorded pid would be a process that
# has already exited and unpin_dev would release nothing. Writing it from
# inside, once fd 3 is open and just before the exec that keeps both the pid
# and the descriptor, makes the pidfile's existence the proof that the device
# really is held: a failed open ends that shell and leaves no file, which is
# what turns a mistyped device name into an error here rather than into a
# teardown that mysteriously succeeds two stages later.
pin_dev() {
	local dev="/dev/mapper/$1" file="$PIN_PREFIX.$1.pid" i
	if [ ! -e "$dev" ]; then
		echo "missing $dev" >&2
		return 1
	fi
	rm -f "$file"
	setsid nohup bash -c \
		"exec 3<'$dev'; echo \$\$ >'$file'; exec sleep 3600" \
		</dev/null >/dev/null 2>&1 &
	for ((i = 0; i < 50; i++)); do
		[ -s "$file" ] && break
		sleep 0.1
	done
	if [ ! -s "$file" ]; then
		echo "nothing reported holding $dev" >&2
		return 1
	fi
	echo pinned
}

# unpin_dev <dm name> — releases one pin and waits for the holder to actually
# go. The wait is load-bearing: the caller removes the device on its very next
# round trip, and a process that has been signalled but not yet reaped still
# holds its descriptor, so without it the retry would race the release and the
# stage would fail on its own instrument.
unpin_dev() {
	local file="$PIN_PREFIX.$1.pid" pid i
	pid=$(cat "$file" 2>/dev/null || true)
	if [ -n "$pid" ]; then
		kill "$pid" >/dev/null 2>&1
		for ((i = 0; i < 50; i++)); do
			kill -0 "$pid" >/dev/null 2>&1 || break
			sleep 0.1
		done
		kill -9 "$pid" >/dev/null 2>&1
	fi
	rm -f "$file"
	echo unpinned
}

# unpin_all releases every pin on this VM, whoever left it behind. A pin that
# survived a failed stage would make the NEXT run's teardown fail on a device
# nothing is testing, and the failure would name the sweep rather than the pin.
unpin_all() {
	local file pid
	for file in "$PIN_PREFIX".*.pid; do
		[ -e "$file" ] || continue
		pid=$(cat "$file" 2>/dev/null || true)
		[ -z "$pid" ] || kill -9 "$pid" >/dev/null 2>&1
		rm -f "$file"
	done
	return 0
}

# iptables_bin resolves the binary, or prints nothing. It exists because the
# binary lives in /usr/sbin and every command here arrives through `sudo bash
# -c`, whose secure_path is not the invoking user's: a partition that silently
# ran nothing would look exactly like a fabric that never noticed the loss,
# which is the one failure a partition stage must not mistake for a pass.
iptables_bin() {
	if command -v iptables >/dev/null 2>&1; then
		echo iptables
	elif [ -x /usr/sbin/iptables ]; then
		echo /usr/sbin/iptables
	else
		echo ""
	fi
}

# have_iptables reports the partition stages' one lab prerequisite (case T's
# S4 on vm2, case A's degrade on vm1). Each stage asks before it partitions
# anything, so a VM without the binary fails with a sentence instead of with
# a path that simply never goes down.
have_iptables() {
	if [ -n "$(iptables_bin)" ]; then echo yes; else echo no; fi
}

# partition_from <ip> — drops every packet this VM receives from <ip> aimed at
# the nvme-tcp port. One direction and one port, deliberately: it takes the
# legs the other VM's cn agent holds into THIS VM's sides, and the gRPC
# control plane both drivers need and this VM's own outbound connections
# (their replies carry the port as the SOURCE, not the destination) keep
# working. The emulated host's paths keep working when the host runs on THIS
# VM (case T's S4); partitioned from the host's VM (case A's degrade), the
# host's path into this VM's cn agent goes as well.
partition_from() {
	local ipt
	ipt=$(iptables_bin)
	if [ -n "$ipt" ] &&
		"$ipt" -I INPUT -s "$1" -p tcp --dport "$TR_SVC_ID" -j DROP \
			>/dev/null 2>&1; then
		echo partitioned
	else
		echo failed
	fi
}

# unpartition_from <ip> — removes the rule, and keeps removing it until there
# is none left: `-I` run twice leaves two identical rules, `-D` takes one, and
# a single rule left behind would black-hole the next case's legs with nothing
# in any log to say why.
unpartition_from() {
	local i ipt
	ipt=$(iptables_bin)
	if [ -n "$ipt" ]; then
		for ((i = 0; i < 16; i++)); do
			"$ipt" -D INPUT -s "$1" -p tcp --dport "$TR_SVC_ID" -j DROP \
				>/dev/null 2>&1 || break
		done
	fi
	echo unpartitioned
}

# partition_rules <ip> — the rules unpartition_from is supposed to have taken.
# Empty output is the assertion, and reading it after the removal is what turns
# "we ran -D" into "the rule is gone".
partition_rules() {
	local ipt
	ipt=$(iptables_bin)
	[ -n "$ipt" ] || return 0
	"$ipt" -S INPUT 2>/dev/null |
		grep -F -- "-s $1" | grep -F -- "--dport $TR_SVC_ID" || true
}

# writer_loop <dev> — the body start_writer detaches. It writes through the
# suite's own write_probe, which is where the dd rule (dnagent_integtest.md,
# Assumptions and preflight checks) lives: no iflag=, no
# oflag=, and a refused write reported as a word instead of as an exit status.
# Every write here is EXPECTED to fail, or to hang queued at the host's
# multipath head, once the teardown starts, so nothing in the loop may read a
# failure as a reason to stop. Only the flag file and the iteration cap end it
# — the cap because a stage that died between start and stop takes the flag
# file's removal with it, and a writer left running would hold a namespace
# open across the next case — and a hanging write holds it at that write until
# stop_writer kills it. Each word lands in $WRITER_RES, one per line, which is
# what S3 counts (writer_counts).
writer_loop() {
	local i
	# Its own pid, for stop_writer, and for pin_dev's reason: `$!` in the
	# starter names setsid, which forks instead of execing when it is already
	# a process-group leader, and a pidfile naming a process that has already
	# exited would leave the writer running with nothing able to name it.
	echo "$$" >"$WRITER_PID"
	for ((i = 0; i < 900; i++)); do
		[ -e "$WRITER_FLAG" ] || break
		write_probe "$1" 0 >>"$WRITER_RES"
		write_probe "$1" 1 >>"$WRITER_RES"
		write_probe "$1" 2 >>"$WRITER_RES"
		write_probe "$1" 3 >>"$WRITER_RES"
		sleep 0.2
	done
	rm -f "$WRITER_FLAG"
	return 0
}

# start_writer <dev> — writer_loop, detached, with both descriptors redirected
# for pin_dev's reason: the caller reads this through `$(…)` over ssh. The loop
# re-enters this same file by name, which is what keeps the writing itself in
# write_probe rather than in a second copy of a dd command line that the dd
# rule (dnagent_integtest.md, Assumptions and preflight checks) would then
# have to be remembered for twice.
start_writer() {
	local i
	rm -f "$WRITER_PID"
	: >"$WRITER_RES"
	: >"$WRITER_FLAG"
	setsid nohup bash "$0" writer_loop "$1" </dev/null >/dev/null 2>&1 &
	for ((i = 0; i < 50; i++)); do
		[ -s "$WRITER_PID" ] && break
		sleep 0.1
	done
	if [ ! -s "$WRITER_PID" ]; then
		rm -f "$WRITER_FLAG"
		echo "nothing reported writing to $1" >&2
		return 1
	fi
	echo started
}

# stop_writer takes the flag away, which is how the loop ends on its own at its
# next check, and signals the recorded pid as the backstop for a loop sitting
# between two checks — including one whose dd is queued at the multipath head.
# The recorded pid is the loop's shell, which waits for its dd interruptibly,
# so the SIGKILL ends the LOOP at once: no further write starts. It does not
# end that dd. The dd is never signalled, and no signal would end it while it
# sleeps uninterruptibly; it keeps its queued write until host_disconnect
# deletes the host's controllers, which fails the write and lets it exit (S3
# checks that it does, wait_writer_gone). Its standard descriptors are the
# results file and /dev/null, never the ssh pipe, so it does not hold this
# function's `$(…)` open either.
#
# It never WAITS for the writer to die, and it signals by PID and never by
# pattern. Both are deliberate. A write still queued against a namespace whose
# last path has gone is exactly the IO the stage wanted in flight, and a task
# in uninterruptible sleep cannot be ended by any deadline (the finding open_rc
# records) — so waiting here would hang the `$(…)` that reads this function. And
# `pkill -f` matches the whole command line of every process on the node,
# INCLUDING the `sudo bash -c` wrapper the suite's own ssh puts this call
# inside: a pattern that named the loop would be a pattern that can kill the
# shell issuing the kill. The bracket trick does not help, because what appears
# in that wrapper's argv is the plain word, not the bracketed one.
stop_writer() {
	local pid
	rm -f "$WRITER_FLAG"
	pid=$(cat "$WRITER_PID" 2>/dev/null || true)
	[ -z "$pid" ] || kill -9 "$pid" >/dev/null 2>&1
	rm -f "$WRITER_PID" "$WRITER_RES"
	echo stopped
}

# writer_counts — the writer's results so far, as `ok=<n> eio=<m>`. A results
# file that cannot be read FAILS the call instead of reading as zeros: S3
# compares these counts across the teardown, and an `ok=0` from a file that
# is not there would satisfy "no write succeeded since" by seeing nothing.
writer_counts() {
	local res ok eio
	if ! res=$(cat "$WRITER_RES" 2>/dev/null); then
		echo "the writer's results ($WRITER_RES) cannot be read" >&2
		return 1
	fi
	ok=$(printf '%s\n' "$res" | grep -cx ok) || true
	eio=$(printf '%s\n' "$res" | grep -cx eio) || true
	echo "ok=$ok eio=$eio"
}

# wait_writer <ok|eio> <min> <secs> — polls until the writer has reported at
# least <min> writes of that result, then prints writer_counts. S3 reads it
# once, for an `ok` before the sides go, because a writer is only an
# instrument once it is seen to work: one whose every write failed — a node
# that is not the namespace, a dd refusing its arguments — would reduce the
# stage to S1 with a loop beside it. A read that fails ends the wait at once
# (writer_counts). Each read is local to this VM: one ssh round trip.
wait_writer() {
	local got n i
	for ((i = 0; i < $3 * 5; i++)); do
		got=$(writer_counts) || return 1
		n=${got#*"$1="}
		n=${n%% *}
		if [ "$n" -ge "$2" ]; then
			echo "$got"
			return 0
		fi
		sleep 0.2
	done
	echo "the writer reported $got after $3s, want $1 >= $2" >&2
	return 1
}

# writer_in_flight <dev> <okmax> <eio0> <age> <secs> — S3's closing read of
# the writer, taken once both CNs are clean, against the counts S3 read as the
# sides went. It asks two things, polling for up to <secs>:
#
#   * No write SUCCEEDED since: `ok` above <okmax> fails at once, exit 2. S3's
#     comment says why the bound it passes is one above what it read.
#   * One write FAILED since (`eio` above <eio0>) or is still OUTSTANDING: the
#     writer's own current dd to <dev>, in uninterruptible sleep and running
#     for at least <age> seconds. With both CNs clean nothing below the host
#     can complete that write any more — it is queued at the host's multipath
#     head, which keeps IO without a path while any controller of it is
#     still reconnecting, and host_disconnect is what will fail it. The age
#     keeps a dd seen in D for the instant every write spends there from
#     counting: it must have gone unanswered for seconds, which no answered
#     write does.
#
# Prints the evidence it found and exits 0; exits 1 when neither shows within
# <secs>; exits 3 when a read fails — the counts, the writer's pid, the
# process listing — because a read that fails must not pass for "nothing
# happened". The dd is the writer's child (the loop's shell forks each dd of
# write_probe directly, and $WRITER_PID is that shell), named `dd` in its argv
# and carrying its own `of=<dev>`, as wait_write_blocked matches it.
writer_in_flight() {
	local dev=$1 okmax=$2 eio0=$3 age=$4 secs=$5
	local got ok eio wpid procs dd kind n i
	for n in "$okmax" "$eio0" "$age" "$secs"; do
		case "$n" in
		'' | *[!0-9]*)
			echo "writer_in_flight: '$n' is not a count" >&2
			return 3
			;;
		esac
	done
	for ((i = 0; i < secs * 5; i++)); do
		got=$(writer_counts) || return 3
		ok=${got#ok=}
		ok=${ok%% *}
		eio=${got##*eio=}
		if [ "$ok" -gt "$okmax" ]; then
			# What the node is, for the report: dd creates its output, so a
			# write after the host lost the namespace's node goes to a regular
			# file in its place and reports `ok` without reaching any stack.
			kind="NOT a block device"
			[ ! -b "$dev" ] || kind="a block device"
			echo "a write succeeded after the sides went: $got, want ok <=" \
				"$okmax ($dev is $kind)" >&2
			return 2
		fi
		if [ "$eio" -gt "$eio0" ]; then
			echo "failed: $got"
			return 0
		fi
		wpid=$(cat "$WRITER_PID" 2>/dev/null) || wpid=
		case "$wpid" in
		'' | *[!0-9]*)
			echo "the writer's pid ($WRITER_PID) cannot be read: '$wpid'" >&2
			return 3
			;;
		esac
		procs=$(ps -eo pid=,ppid=,stat=,etimes=,wchan:32=,args= 2>/dev/null)
		if [ -z "$procs" ]; then
			echo "the process listing cannot be read" >&2
			return 3
		fi
		dd=$(printf '%s\n' "$procs" |
			awk -v p="$wpid" -v a="$age" -v w="of=$dev" '
			    $2 == p && $3 ~ /^D/ && $4 >= a && $6 == "dd" && index($0, w) {
			        print "pid " $1 ", " $3 " for " $4 "s in " $5; exit }')
		if [ -n "$dd" ]; then
			echo "outstanding: dd $dd; $got"
			return 0
		fi
		sleep 0.2
	done
	echo "no write failed and none is outstanding after ${secs}s: $got;" \
		"writer $wpid $(kill -0 "$wpid" 2>/dev/null && echo running ||
			echo gone), its children:" \
		"$(printf '%s\n' "$procs" | awk -v p="$wpid" '$2 == p' |
			tr '\n' ';')" >&2
	return 1
}

# wait_writer_gone <dev> <secs> — polls until no dd writing to <dev> is left
# on this VM, and prints `gone`. S3 reads it after host_disconnect, because
# stop_writer ends only the loop: a dd whose write is queued at the multipath
# head outlives it, and deleting the host's controllers is what fails that
# write and lets the dd exit. A dd still there after <secs> is a write the
# disconnect did not end, still holding the namespace open, and fails the
# call; so does a process listing that cannot be read. It matches the dd as
# writer_in_flight does — `dd` in its argv, its own `of=<dev>` — but by no
# pid: stop_writer has removed the pid file, and the dd, orphaned, is no
# longer the loop's child anyway.
wait_writer_gone() {
	local procs left i
	for ((i = 0; i < $2 * 5; i++)); do
		procs=$(ps -eo pid=,stat=,etimes=,args= 2>/dev/null)
		if [ -z "$procs" ]; then
			echo "the process listing cannot be read" >&2
			return 1
		fi
		left=$(printf '%s\n' "$procs" | awk -v w="of=$1" \
			'$2 !~ /^Z/ && $4 == "dd" && index($0, w)')
		if [ -z "$left" ]; then
			echo gone
			return 0
		fi
		sleep 0.2
	done
	echo "a write to $1 is still running $2s after the disconnect: $left" >&2
	return 1
}

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

# cn_log_lines — the cn log's length in lines, a mark the two readers below
# start after. A stage reads "since" a mark instead of by trace id when the
# records it wants were not caused by its own RPCs: the CN10 background retry
# mints a fresh trace id per attempt (CN2), which no stage knows in advance.
cn_log_lines() {
	local n
	n=$({ wc -l <"$CN_LOG"; } 2>/dev/null) || n=0
	echo $((n))
}

# cn_cmds_since <mark> <prefix> — the os command records after line <mark>
# whose "cmd args…" starts with <prefix> (empty: every one), each printed as
# "<trace_id> cmd args…", in log order.
cn_cmds_since() {
	tail -n "+$(($1 + 1))" "$CN_LOG" 2>/dev/null | jq -r --arg p "$2" '
	    select(.msg == "os command")
	    | (.cmd + " " + ((.args // []) | join(" "))) as $e
	    | select($e | startswith($p))
	    | .trace_id + " " + $e' 2>/dev/null || true
}

# cn_requests_since <mark> <method> <sp> <cntlr> — how many `grpc server
# request` records of one unary method for one cntlr follow line <mark>. The
# ids are decimal, as the log renders a request's uint64 fields.
cn_requests_since() {
	local n
	n=$(tail -n "+$(($1 + 1))" "$CN_LOG" 2>/dev/null | jq -r --arg m "$2" \
	    --argjson sp "$3" --argjson c "$4" '
	    select(.msg == "grpc server request")
	    | select((.method | split("/") | last) == $m)
	    | select(.data.cntlr_pointer.sp_id == $sp
	        and .data.cntlr_pointer.cntlr_id == $c)
	    | .trace_id' 2>/dev/null | wc -l)
	echo $((n))
}

# mutations [trace] [log] — every mutating operation in a cn agent log
# (cnagent_integtest.md, Conventions, Mutation-free, and Cases, restart). An
# empty trace means the whole log; naming one scopes the
# answer to a single converge, which is what lets a stage prove that *its own*
# syncup mutated nothing but dm devices (`cnagent_integtest.md`, Cases,
# thinbm). The CN11 leg health probers do not use the OsClient (osclient.md,
# Exported raw helpers and the probe-IO carve-out):
# they call the raw block-IO syscalls directly and log their own records as
# `probe write block` / `probe read block direct`, which are not in this grep
# list by construction. So no path-based exemption is needed any more, and
# `os write block` — the one block-IO msg an OsClient still emits
# (common/osclient.go:335) — is a mutation without qualification. There is no
# read-side msg left to grep for: the probe-IO carve-out deleted `OsClient.ReadBlockDirect` and
# its log record outright, and the package-level `ReadBlockDirectAt` that
# replaced it logs nothing at all
# (osclient.md, Exported raw helpers and the probe-IO carve-out).
# Probe commands (lsblk, dmsetup info|table|status|ls, ls, findmnt, stat,
# losetup --associated, mdadm --examine, nvme list-subsys) are expected and
# deliberately not in the list; the cn agent no longer runs `mdadm --detail`
# at all (its md reads are sysfs since 2026-09-26), which is a read too.
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
# output, so neither `dmsetup ls` nor the `ls` of the nvmet subsystems may
# print nothing when it fails: each prints one line saying so instead, its
# error text folded into it, and that line fails the assertion — before, the
# failure printed nothing and read as a clean node nobody had listed. The dm
# pattern's [cd][0-9a-f] kind field already covers the kind-cb clone-metadata
# wrappers that replaced the clone VG's LVs ([D14]), so a leaked arena
# allocation is caught here for free. An md array is named as well as its
# members: md_names by the array's own name, md_member_names by its members'
# dm names, which carry the sp id too — the one md route that still names an
# `inactive` array whose members neither udev nor mdadm can read. The member
# route adds no detection over the dm line above: while `dmsetup ls` answers,
# every member it reports is an sp dm device that the listing already shows,
# and a `dmsetup ls` that fails already fails the assertion through its own
# line. It says which array holds a member.
residue() {
	local dms subs nl=$'\n'
	if dms=$(dmsetup ls 2>&1); then
		printf '%s\n' "$dms" | awk '{print $1}' |
			grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-$1-" || true
	else
		echo "dmsetup ls failed, the dm residue is unknown: ${dms//$nl/; }"
	fi
	if subs=$(ls "$NVMET/subsystems" 2>&1); then
		printf '%s\n' "$subs" | grep -E ":$1:" || true
	else
		echo "ls $NVMET/subsystems failed, the nvmet residue is unknown: ${subs//$nl/; }"
	fi
	md_names | grep -E "dnv-$1-[0-9a-f]+-[0-9a-f]+" || true
	md_member_names |
		grep -E " dnv-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-$1-" || true
}

# cn_residue <cn16> — every dm device of one CN plus the test's host-facing
# subsystems (their NQNs carry no id, so they are matched by prefix;
# cnagent_integtest.md, Cases, smoke). A
# `dmsetup ls` or an `ls` of the nvmet subsystems that fails prints one line
# saying so, for residue's reason.
# agent_dm_names keeps printing nothing on a failed `dmsetup ls`: cleanup
# hands every name it prints to dm_force_remove, which would take the words
# of such a line for device names.
cn_residue() {
	local dms subs nl=$'\n'
	if dms=$(dmsetup ls 2>&1); then
		printf '%s\n' "$dms" | awk '{print $1}' |
			grep -E "^dnv-[0-9a-f]{16}-$1-" || true
	else
		echo "dmsetup ls failed, the dm residue is unknown: ${dms//$nl/; }"
	fi
	if subs=$(ls "$NVMET/subsystems" 2>&1); then
		printf '%s\n' "$subs" | grep -F "$NQN_IT_PREFIX" || true
	else
		echo "ls $NVMET/subsystems failed, the nvmet residue is unknown: ${subs//$nl/; }"
	fi
}

# --- setup / teardown --------------------------------------------------------

# install_udev_rule masks the stock incremental md assembly for dnv arrays
# (cnagent_integtest.md, Lab facts): the stock rule honors SYSTEMD_READY, so
# setting it
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
		grep -E '^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-' || true
}

# suspended_agent_dms — the agent dm devices currently held suspended. The cn
# agent leaves none across a converge pass (architecture.md,
# Namespace suspend semantics: an effectively suspended
# namespace is parked, live), so on a CN VM outside a DN cutover window this
# prints nothing; `resume_suspended` uses the same `attr` column to sweep.
suspended_agent_dms() {
	local row name
	# `:..s` and NOT `:.-s`: attr is L/I/s/r-w, so pinning the second column to
	# `-` would skip a device that also has an INACTIVE TABLE loaded — `LIsw`,
	# exactly what an interrupted `Dm.Reload` (suspend, load, resume) leaves,
	# which is the state this sweep exists for. agent/dm.go reads index 2 alone.
	for row in $(dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E '^dnv[-a-z0-9]*.*:..s'); do
		name=${row%%:*}
		case "$name" in
		dnv-*) printf '%s\n' "$name" ;;
		esac
	done
	return 0
}

# dnv_dm_cnt — how many dnv dm devices `dmsetup info`'s name,attr listing shows
# at all. suspended_agent_dms answers with the empty string both when nothing
# is suspended and when its pipeline is broken (a failed dmsetup, a changed
# separator, a wrong prefix), so the assertion built on it needs this second
# number to tell those apart.
dnv_dm_cnt() {
	dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -cE '^dnv[-a-z0-9]*.*:' || true
}

# dm_kind_names <kind> [node16] — the dm devices of one kind, optionally of one
# node only. Both roles name their devices dnv-{cluster}-{node}-{kind}-…, and
# the kind now carries the role letter (c0…cb, d0…d5), so it no longer overlaps
# between the roles; the node id is kept because it still scopes a teardown
# pass to one of the two agents sharing this VM.
dm_kind_names() {
	agent_dm_names | awk -F- -v k="$1" -v n="${2:-}" \
		'$4 == k && (n == "" || $3 == n)'
}

# clone_meta_wrappers [cn16] — the kind-cb clone-metadata dm-linears, which are
# the arena's allocation registry itself (CN18: there is no
# on-file allocation table, the dm tables are it). Empty output means every
# unit of the arena is free.
clone_meta_wrappers() { dm_kind_names cb "${1:-}"; }

# clone_bm_files <cluster16> <cn16> <sp16> <clone16> — the basenames of one
# clone's bitmap chunk files in the cn store, sorted. A clone chunk is
# addressed by the PAIR (src_slice_idx, bm_idx), so LocalCloneBmPath ends in
# TWO %02x segments (cnagent.md, Additions to `common`) where the single-index
# format had one.
# The name is only an address — the reconcile decodes the pair from the
# persisted PushCloneBitmapRequest inside the file — which is exactly why it
# needs asserting here: a wrong name would still reload correctly, so no reply
# assertion can catch it. Empty output means no chunk file at all.
clone_bm_files() {
	local f base
	for f in "$WORK"/cn-store/clone-bm-"$1"-"$2"-"$3"-"$4"-*; do
		[ -e "$f" ] || continue
		base=${f##*/}
		# The agent writes through a "{name}.tmp-XXXX" file in the same
		# directory and removes it on every path; skip one a crash left
		# behind rather than reporting it as a chunk.
		case "$base" in *.tmp-*) continue ;; esac
		printf '%s\n' "$base"
	done | sort
}

# resume_suspended sweeps up suspended dm devices before anything reads them.
# A dn cutover window holds linears suspended (DN12's fence, bounded by
# SuspendSeconds), and an interrupted reload can leave anything so. On CN VMs
# it is debris cleanup only since 2026-09-16: the cn agent no longer suspends
# a transfer origin — an effectively suspended namespace is *parked*, live on
# the td's dm-error (CN16; architecture.md, Namespace suspend semantics) — so
# the only suspended CN device a run can
# meet is one an older build or a killed agent left behind. Anything that
# reads a suspended device (`dmsetup remove`, disabling the nvmet namespace
# above it, and above all a block-device scan) blocks in uninterruptible D
# state and wedges the node until reboot. The pattern is deliberately looser
# than agent_dm_names' so it also sweeps up debris an older pre-arena run left behind on
# a shared lab VM — including LVM's own doubled-dash nodes (dnv--clone--vg-*),
# which no current run can produce.
resume_suspended() {
	local row name
	# `:..s`, not `:.-s` — see suspended_agent_dms: `.-` would skip `LIsw`, the
	# device an interrupted reload leaves, which is the one this sweep most
	# needs to release.
	for row in $(dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E '^dnv[-a-z0-9]*.*:..s'); do
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
# path of the NQN is being retired (cnagent_integtest.md, Cases, clone_xfer).
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

# md_stop_all stops every array this suite created: /proc/mdstat enumerates
# them and udev holds each one's name, with mdadm asked directly when udev does
# not. The agent's --homehost any means the name may or may not carry a
# homehost prefix, so both forms are matched — the same `dnv-*|*:dnv-*` pair
# install_udev_rule writes into the mask, against the same MD_NAME property.
#
# IT USED TO FILTER ON `mdadm --detail --scan`, WHICH PRINTS NO NAME on these
# guests (measured on the lab, kernel 7.0.0-31 / Ubuntu 26.04 mdadm,
# 2026-09-17):
# the scan line is `ARRAY /dev/md/<hex MD_DEVNAME> metadata=1.2` and nothing
# more, so every line hit `continue` and the function stopped nothing, ever.
# `udevadm info --query=property` has it.
#
# THE TWO READS GO BLIND ON DIFFERENT ARRAYS, which is why both are here.
# udev's MD_NAME is imported from `mdadm --detail --no-devices --export` on the
# array (/usr/lib/udev/rules.d/63-md-raid-arrays.rules), so the udev read is
# that answer cached and the fallback is it live. udev has nothing for an array
# in state `clear` or `inactive`, because the line before that import in the
# same file jumps past it on exactly those states; mdadm has nothing when it
# cannot read a member's superblock, which is what the 59 wedged arrays of
# 2026-09-17 were: `--detail --export` had lost MD_NAME there and `--examine`
# failed on the member, and the explanation to hand is that dm_force_remove's
# `--force` had just put error targets under their legs. An array that is both
# — inactive over unreadable members — is named by neither and is still
# skipped.
#
# An unreadable name matches neither pattern, so the array is left alone: this
# verb never stops an array that is not a dnv one, and a guest without udevadm
# or without mdadm gets the old no-op back rather than a wrong stop — which is
# why both are in the preflight tool list. Note where that list is NOT reached:
# preflight_vms runs after the unconditional start cleanup, and --cleanup-only
# skips it entirely, so those two sweeps each get one unguarded pass.
#
# The /proc/mdstat pattern is `^md[^ :]+`: `^md[0-9]*` also matches the bare
# `md` of a `md_<name> : active` line (mdadm.conf `CREATE names=yes`), and
# /dev/md is the by-name directory, so that token reads nothing and skips an
# array we own. These VMs run the default names=no; this is a trap, not a live
# failure.
md_stop_all() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(timeout 10 udevadm info --query=property \
			--name="/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		[ -n "$name" ] || name=$(timeout 10 mdadm --detail \
			--no-devices --export "/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		case "$name" in
		dnv-* | *:dnv-*) ;;
		*) continue ;;
		esac
		timeout 15 mdadm --stop "/dev/$d" >/dev/null 2>&1
	done
	return 0
}

# md_names prints the MD_NAME of every assembled dnv array, one per line, by
# md_stop_all's route and for md_stop_all's reason: `mdadm --detail --scan`
# prints no `name=` field on these guests, so the `grep -oE "name=…dnv-…"` that
# `residue` used matched NOTHING and the teardown's md assertion passed
# vacuously. Empty output is still the pass; the difference is that it is now
# empty because there is no array, not because the field was never there.
md_names() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(timeout 10 udevadm info --query=property \
			--name="/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		[ -n "$name" ] || name=$(timeout 10 mdadm --detail \
			--no-devices --export "/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		case "$name" in
		dnv-* | *:dnv-*) printf '%s\n' "$name" ;;
		esac
	done
	return 0
}

# md_member_names prints one `mdN <dm name>` line per dm member of every md
# array on the node, read straight out of sysfs
# (/sys/block/mdN/md/dev-*/block/dm/name). It is lab_wipe's member route: no
# superblock read, so it names what md_names cannot — an `inactive` array
# whose members udev holds no MD_NAME for and mdadm can no longer read — and
# a member's dm name carries its sp id, which is how residue attributes the
# array. Empty output means no array has a dm member at all.
md_member_names() {
	local name d
	for name in /sys/block/md*/md/dev-*/block/dm/name; do
		[ -r "$name" ] || continue
		d=${name#/sys/block/}
		printf '%s %s\n' "${d%%/*}" "$(cat "$name" 2>/dev/null)"
	done
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

# wipe_cn simulates the CN reboot of clone_xfer's stage 6 (cnagent_integtest.md,
# Cases, clone_xfer): every kernel object of one CN
# goes, including the volatile clone-metadata arena, while the dn objects, the
# shared port and $WORK/cn-store stay. The order is that of
# cnagent_integtest.md, Teardown and cleanup, and the
# dm-clone is removed while its :4: source connection is still up — a clone
# flushes through its source on removal and blocks without it.
#
# Kind cb (the clone-metadata wrappers that replaced the clone VG) joins the dm
# passes rather than getting a teardown of its own.
# It must come after kind c7: the dm-clone holds its wrapper open, and a
# wrapper left behind holds the loop device open, wedging tmpfs_teardown's
# `losetup -d` with EBUSY.
wipe_cn() { # <cn16>
	resume_suspended
	drop_subsys_glob "$NQN_IT_PREFIX:*"
	dm_remove_kind c6 "$1"
	dm_remove_kind c8 "$1"
	dm_remove_kind c7 "$1"
	disconnect_prefix "$NQN_PREFIX:4:"
	disconnect_prefix "$NQN_PREFIX:2:"
	local kind
	for kind in c5 c4 c3 c2 c1 c0 ca c9 cb; do
		dm_remove_kind "$kind" "$1"
	done
	md_stop_all
	tmpfs_teardown
	echo wiped
}

# cleanup_phase1 and cleanup_phase2 implement cnagent_integtest.md,
# Teardown and cleanup. The split is what makes the
# cross-VM ordering safe: a case C clone on one VM holds an nvme connection to
# a transfer on the other, so every clone is removed (phase 1) before any
# transfer subsystem is (phase 2).
cleanup_phase1() { # <cn16> [other vm ip]
	kill_role cn >/dev/null
	kill_role dn >/dev/null

	# The fault injectors (case T's three, and the partition case A's
	# degrade stage reuses), first and unconditionally. Each of them
	# outlives the stage that installed it and each would be diagnosed as
	# something else entirely: a pin makes the dm passes below fail on a
	# device nothing is testing, a partition rule black-holes the next run's
	# legs, and a writer holds a namespace open across the disconnects. None
	# of the three may be conditional on this run having used them — the run
	# that leaves them behind is by definition the one that failed.
	stop_writer >/dev/null
	unpin_all
	[ -z "${2:-}" ] || unpartition_from "$2" >/dev/null

	# Nothing may stay suspended from here on (see resume_suspended).
	resume_suspended

	# nvmet controllers must die before nvmet teardown.
	disconnect_prefix "$NQN_IT_PREFIX:"
	drop_subsys_glob "$NQN_IT_PREFIX:*"

	dm_remove_kind c6 "${1:-}"
	dm_remove_kind c8 "${1:-}"
	dm_remove_kind c7 "${1:-}"
	echo phase1
}

cleanup_phase2() { # <cn16> <dn16>
	local cn16=${1:-} dn16=${2:-} kind name

	# The transfers the clones of phase 1 were sourced from.
	disconnect_prefix "$NQN_PREFIX:4:"
	drop_subsys_glob "$NQN_PREFIX:4:*"

	# cn dm pass 2, top-down, then the arrays, then the leg wrappers.
	for kind in c5 c4 c3 c2 c1 c0; do
		dm_remove_kind "$kind" "$cn16"
	done
	md_stop_all
	dm_remove_kind ca "$cn16"
	dm_remove_kind c9 "$cn16"
	# The clone-metadata wrappers, after their dm-clones went in phase 1: a
	# leftover kind-cb holds the loop device open and wedges the `losetup -d`
	# below with EBUSY.
	dm_remove_kind cb "$cn16"
	disconnect_prefix "$NQN_PREFIX:2:"

	tmpfs_teardown
	resume_suspended

	# The dn suite's sequence (dnagent_integtest.md, Teardown and cleanup), now
	# that nothing connects to the sides.
	drop_subsys_glob "$NQN_PREFIX:2:*"
	dm_remove_kind d1 "$dn16"
	dm_remove_kind d3 "$dn16"
	disconnect_prefix "$NQN_PREFIX:3:"
	drop_subsys_glob "$NQN_PREFIX:3:*"
	dm_remove_kind d5 "$dn16"
	dm_remove_kind d2 "$dn16"
	dm_remove_kind d0 "$dn16"
	dm_remove_kind d4 "$dn16"
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
	# because the volume-table slots are inert without it ([D13],
	# dnagent.md DN5: magic absent ⇒ the disk is blank). No oflag=, per
	# the dd rule (dnagent_integtest.md, Assumptions and preflight checks).
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

# lab_wipe — the ONE-TIME lab wipe of cnagent_integtest.md,
# Teardown and cleanup. It is
# NOT part of a run: only the driver's --wipe reaches it.
#
# Why it exists at all: every teardown verb above removes dm devices BY KIND,
# and the kind literals they pass are the new, role-lettered ones (c0…cb,
# d0…d5). Residue an older binary left on a shared lab VM carries the old
# single-digit spelling (0…b), so those verbs walk straight past it and it
# stays there forever, pinning loop devices and nvmet objects the next run
# needs. This one reads no kind at all — everything named `dnv*` goes, both
# spellings and the pre-arena `dnv--clone--vg-*` LVM debris with it.
#
# That is also why it is not wired into cleanup: on a shared VM it would
# destroy a CONCURRENT run's objects, and `nvme disconnect-all` takes every
# fabrics controller on the node, dnv's or not. Run it once, alone.
#
# The order is that of cnagent_integtest.md, Teardown and cleanup, generalized
# away from the kind list: arrays first (an
# array holds its member wrappers open and is the one holder `dmsetup remove
# --force` cannot argue with), then the controllers, then the nvmet objects
# that pin dm devices from above, then the dm devices themselves — enumerated
# out of `dmsetup ls` once per round instead of from a kind order, because
# "reverse dependency order" is exactly "whatever is still there after the
# round that freed it".
lab_wipe() {
	local d dev nm mdname hit i cnt prev names name

	kill_role cn >/dev/null
	kill_role dn >/dev/null

	# Nothing may stay suspended from here on (see resume_suspended): reading
	# a suspended device goes to D state, where `timeout` cannot reach it.
	resume_suspended

	# THREE PASSES, because one is provably not enough. Stopping the arrays
	# frees their member wrappers, but the members still carry md
	# superblocks, and a dm device that reappears — or that udev re-examines
	# while this is running — is re-assembled into a fresh array that pins
	# the wrapper again ([[dn-guest-auto-assembles-md]] is the same mechanism
	# on a DN). Measured on cn0 2026-09-18: one pass reported `dm left:`
	# EMPTY and left 8 kind-9 wrappers held open by 4 re-assembled arrays; a
	# second, identical invocation removed all of them. So the sequence runs
	# until the node is clean, not once.
	local pass
	for pass in 1 2 3; do
		if [ "$pass" -gt 1 ] && [ -z "$(dmsetup ls 2>/dev/null |
			awk '$1 ~ /^dnv/ {print $1}')" ]; then
			break
		fi

		# Every md array on this node that is ours, by two independent routes
		# because each is blind to a case the other sees. THE MEMBER ROUTE reads
		# the member's dm name straight out of sysfs (cnagent_integtest.md,
		# Teardown and cleanup), needs no
		# superblock read, and is the only one that works for the `inactive`
		# one-member assemblies udev leaves on a DN — udev has no MD_NAME for
		# those. THE NAME ROUTE is the only one left once the members themselves
		# are already gone. Neither can name an array that is not a dnv one, so
		# this never stops the guest's own.
		for d in /sys/block/md*; do
			[ -d "$d/md" ] || continue
			hit=0
			for dev in "$d"/md/dev-*; do
				[ -e "$dev/block/dm/name" ] || continue
				nm=$(cat "$dev/block/dm/name" 2>/dev/null)
				case "$nm" in dnv*) hit=1 ;; esac
			done
			if [ "$hit" -eq 0 ]; then
				mdname=$(timeout 10 udevadm info --query=property \
					--name="/dev/${d##*/}" 2>/dev/null |
					sed -n 's/^MD_NAME=//p')
				[ -n "$mdname" ] || mdname=$(timeout 10 mdadm --detail \
					--no-devices --export "/dev/${d##*/}" 2>/dev/null |
					sed -n 's/^MD_NAME=//p')
				case "$mdname" in dnv-* | *:dnv-*) hit=1 ;; esac
			fi
			if [ "$hit" -eq 1 ]; then
				timeout 15 mdadm --stop "/dev/${d##*/}" >/dev/null 2>&1
			fi
		done

		# Every fabrics controller this node holds, whoever opened it: a live
		# controller keeps the subsystem below it alive, and that subsystem's
		# namespace keeps its backing dm device open.
		timeout 60 nvme disconnect-all >/dev/null 2>&1

		# nvmet, port links first — a subsystem still linked to a port cannot be
		# removed, and a namespace must be disabled before its backing dm device
		# can go. The glob is the plan's `nqn.2024-01.io.dnv*`, so it takes the
		# suite's own host-facing prefix (that same string plus "-it") too.
		drop_subsys_glob "$NQN_PREFIX*"

		# The dm devices, bottom-up by attrition rather than by a kind order: a
		# device that is still open fails and is listed again next round, by
		# which time whatever held it has gone. A round that frees nothing means
		# a holder outside this set, and --force (an error table swapped in) is
		# the only answer to that; the round cap keeps a wedge from spinning here
		# forever.
		prev=-1
		for ((i = 0; i < 12; i++)); do
			names=$(dmsetup ls 2>/dev/null | awk '$1 ~ /^dnv/ {print $1}')
			[ -n "$names" ] || break
			cnt=$(printf '%s\n' "$names" | wc -l)
			if [ "$cnt" -eq "$prev" ]; then
				for name in $names; do dm_force_remove "$name"; done
			else
				for name in $names; do
					timeout 10 dmsetup remove "$name" >/dev/null 2>&1
				done
			fi
			prev=$cnt
		done

		resume_suspended
	done

	# The residue is the report AND the exit status. It used to be the report
	# alone, on the reasoning that this verb is best-effort like every other
	# one here — but the driver runs both VMs concurrently and discards their
	# status, so a wipe that left one VM full of debris printed the other
	# VM's clean report and the run said PASS. A cleanup that cannot fail is
	# a cleanup nobody can trust, which is the same rule the sweep this suite
	# tests lives by: what is left is reported, and reporting it is not
	# success. Everything this deliberately does not touch — $WORK, the loop
	# devices, the tmpfs, the udev rule, the nvmet port — belongs to the
	# ordinary cleanup that runs after it and is not counted here.
	local dm_left nvmet_left md_left
	dm_left=$(dmsetup ls 2>/dev/null | awk '$1 ~ /^dnv/ {print $1}' | tr '\n' ' ')
	nvmet_left=$(ls "$NVMET/subsystems" 2>/dev/null | grep -F dnv | tr '\n' ' ')
	md_left=$(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null | tr '\n' ' ')
	echo wiped
	printf 'dm left: %s\n' "$dm_left"
	printf 'nvmet left: %s\n' "$nvmet_left"
	printf 'md on this node (ours or not): %s\n' "$md_left"
	[ -z "$dm_left$nvmet_left" ] || return 1
	return 0
}

# diag_cmd <cmd> [args…] — one diag command, bounded at 20 s. The bound has
# two halves because one is not enough (open_rc's finding, measured on the lab
# kernel): `timeout 20` ends a command that sleeps interruptibly, but not one
# in uninterruptible D state — the SIGTERM does nothing there, and `timeout`
# then waits for its child. `dmsetup status` (a pool's metadata commit) and
# `mdadm --detail --scan` (a member's superblock read) enter exactly that over
# a device left suspended, and diag runs from the failure handler, before
# anything has resumed it. So the command runs detached with its output in a
# file, and one still running after the bound is abandoned with a note — left
# for resume_suspended to release at cleanup, as open_rc leaves its reader —
# and the dump goes on instead of hanging the failure handler short of its
# closing report. None of the group's descriptors is the ssh session's pipe
# (open_rc's other finding), and both files are opened once, by the group
# itself, so an abandoned command that finishes later writes into files
# already unlinked rather than leaving new ones behind.
diag_cmd() {
	local out i
	out=$(mktemp /tmp/dnv-diag.XXXXXX) || return 0
	{
		timeout 20 "$@" >&3 2>/dev/null
		echo $?
	} </dev/null >"$out.rc" 3>"$out" 2>/dev/null &
	for ((i = 0; i < 220; i++)); do
		[ -s "$out.rc" ] && break
		sleep 0.1
	done
	cat "$out" 2>/dev/null
	if [ ! -s "$out.rc" ]; then
		echo "(still running after 20 s, abandoned: a read of a suspended device?)"
	elif [ "$(cat "$out.rc")" = 124 ]; then
		echo "(timed out after 20 s)"
	fi
	rm -f "$out" "$out.rc"
	return 0
}

diag() {
	echo "--- dn-agent.log (last 120 lines) ---"
	tail -n 120 "$DN_LOG" 2>/dev/null
	echo "--- cn-agent.log (last 120 lines) ---"
	tail -n 120 "$CN_LOG" 2>/dev/null
	# Everything from here reads kernel device state, which a suspended or
	# dead device can block: each command goes through diag_cmd. A pipeline
	# or helper runs as one `bash -c` or re-enters this file by name
	# (start_writer's idiom), so the bound covers the whole of it.
	echo "--- dmsetup ls ---"
	diag_cmd dmsetup ls
	echo "--- dmsetup table ---"
	diag_cmd dmsetup table
	echo "--- dmsetup status ---"
	diag_cmd dmsetup status
	echo "--- /proc/mdstat ---"
	diag_cmd cat /proc/mdstat
	echo "--- mdadm --detail --scan ---"
	diag_cmd mdadm --detail --scan
	echo "--- clone-meta wrappers (kind cb) ---"
	diag_cmd bash "$0" clone_meta_wrappers
	echo "--- losetup -a ---"
	diag_cmd losetup -a
	echo "--- tmpfs mounts ---"
	diag_cmd bash -c 'findmnt | grep dnv-tmpfs'
	echo "--- nvmet configfs ---"
	diag_cmd ls -R "$NVMET/ports" "$NVMET/subsystems"
	echo "--- nvme list-subsys ---"
	diag_cmd bash "$0" subsys_json
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

# wipe_all runs the one-time lab wipe (cnagent_integtest.md,
# Teardown and cleanup) on both VMs, concurrently
# for cleanup_all's reason: a subsystem on one VM backs a connection on the
# other, so the shorter the window the better. `--wipe` is its only caller and
# it always runs the ordinary start-of-run cleanup afterwards, which takes
# what the wipe deliberately leaves — $WORK, the loop devices, the tmpfs, the
# udev rule and the nvmet port.
#
# EACH VM'S STATUS IS READ, and that is the whole point of the rewrite. This
# used to be `helper_ok … &` with `wait "$pid" || true`, which discards both:
# on 2026-09-18 a wipe left cn0 holding 8 kind-9 wrappers and 4 md arrays,
# printed only the other VM's clean residue report — the two VMs' output
# interleaves, so a missing report does not stand out — and exited PASS. The
# next run then died in a residue stage on debris the wipe had claimed to
# remove. A verb whose failure cannot be seen is worse than no verb, which is
# the same rule the sweep this suite tests is built on.
wipe_all() {
	local idx pid rc pids=() bad=()
	for idx in 1 2; do
		helper "$idx" lab_wipe &
		pids+=($!)
	done
	for idx in 1 2; do
		rc=0
		wait "${pids[$((idx - 1))]}" || rc=$?
		[ "$rc" -eq 0 ] || bad+=("vm$idx (${IP[$idx]}) rc=$rc")
	done
	[ ${#bad[@]} -eq 0 ] ||
		die "the lab wipe left objects behind on ${bad[*]} — the residue is" \
			"printed above; re-run --wipe, and if the same names survive that" \
			"something outside dnv is holding them"
}

# cleanup_all runs cnagent_integtest.md, Teardown and cleanup, as two cross-VM
# phases: within a phase the two VMs run
# concurrently, but no VM starts phase 2 until both finished phase 1, because
# a clone on one VM flushes through a transfer export on the other.
cleanup_all() {
	local idx pid pids=()
	for idx in 1 2; do
		# The other VM's ip is what a partition rule names (case T's S4 on
		# vm2, case A's degrade on vm1), and only this VM can remove it, so
		# each VM is told which address to clear.
		helper_ok "$idx" \
			"cleanup_phase1 $(hex16 "${CNID[$idx]}") ${IP[$((3 - idx))]}" &
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

# diag_cntlr registers one cntlr for the failure dump (cnagent_integtest.md,
# Teardown and cleanup). Cases reset the list.
diag_cntlr() { # cnidx sp cntlr
	DIAG_CNTLRS+=("$1:$2:$3")
}

# ---------------------------------------------------------------------------
# Preflight (cnagent_integtest.md, Assumptions and preflight checks)
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
# be meaningful once a crashed prior run's agents are gone
# (cnagent_integtest.md, Assumptions and preflight checks).
preflight_vms() {
	STAGE="preflight (vms)"
	log "=== preflight: vms"
	local idx
	for idx in 1 2; do
		ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo -n true" ||
			die "missing: passwordless sudo on vm$idx (${VM[$idx]})"
		local missing
		# No LVM binaries: [D14] removed LVM from the CN entirely. thin_dump stays
		# — it is the thin-metadata oracle of cnagent_integtest.md, Cases, thinbm,
		# not an LVM command.
		# udevadm: install_udev_rule's and cleanup_phase2's `udevadm control
		# --reload`, and md_stop_all's MD_NAME read — the first of its two
		# name sources, and the only one that answers for an array whose
		# members mdadm can no longer read. mdadm is the other source and is
		# also what does the stopping, so a VM without it makes that verb the
		# silent no-op it was until 2026-09-17. Note this list is reached
		# AFTER the start-of-run cleanup_all and not at all under
		# --cleanup-only, so those sweeps run unchecked.
		missing=$(sshv "$idx" "for b in dmsetup nvme losetup blkdiscard lsblk dd fallocate sha256sum cmp pkill jq timeout mdadm udevadm truncate stat findmnt thin_dump; do command -v \$b >/dev/null || echo \$b; done")
		[ -z "$missing" ] || die "missing: $missing on vm$idx"
		# The agents hardcode the configfs path and neither mount nor
		# modprobe; the harness does both here and nothing else.
		sshv "$idx" "for m in nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-clone dm-thin-pool dm-flakey raid1; do modprobe \$m 2>/dev/null || true; done; grep -q ' /sys/kernel/config ' /proc/mounts || mount -t configfs none /sys/kernel/config; true"
		# Then verified, module by module: the loop above ignores every modprobe
		# error and the checks below see only nvmet and md, so a dm-clone or a
		# dm-flakey that never loaded would surface stages later as a missing
		# dm target. Loaded means a /sys/module entry; built in means a line in
		# modules.builtin, since a built-in module can lack a /sys/module entry
		# (one without parameters has none).
		missing=$(sshv "$idx" "for m in nvmet nvmet_tcp nvme_tcp nvme_fabrics loop dm_clone dm_thin_pool dm_flakey raid1; do [ -d /sys/module/\$m ] || tr - _ </lib/modules/\$(uname -r)/modules.builtin 2>/dev/null | grep -qF /\$m.ko || echo \$m; done")
		[ -z "$missing" ] || die "missing: kernel module(s) $missing on vm$idx"
		local got
		got=$(sshv "$idx" "ls -d $NVMET 2>/dev/null || echo MISSING")
		assert_eq "$got" "$NVMET" "nvmet configfs on vm$idx"
		got=$(sshv "$idx" "ls /proc/mdstat 2>/dev/null || echo MISSING")
		assert_eq "$got" /proc/mdstat "md support on vm$idx"
		got=$(sshv "$idx" "cat /sys/module/nvme_core/parameters/multipath")
		assert_eq "$got" "Y" "nvme_core.multipath on vm$idx"
		# The udev mask works by setting SYSTEMD_READY, which only helps
		# if the stock incremental-assembly rule honors it (cnagent_integtest.md,
		# Lab facts).
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
# Setup (cnagent_integtest.md, Cases, the setup paragraph)
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
# whichever runs first creates it and the other issues zero writes
# (cnagent_integtest.md, Topology).
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
		# The fast-Write-Zeroes preflight item (cnagent_integtest.md,
		# Assumptions and preflight checks), deferred to here because
		# the device only exists now (preflight_vms runs before setup). The
		# zeroing of architecture.md, Side provisioning protocol, assumes fast
		# Write Zeroes; a loop device maps
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

# dn_pointers introduces a DN's full side list (dnagent.md DN8: a side pointer
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

# SIDE_PROVISIONED remembers which sides have already been through the
# two-phase provisioning (architecture.md, Side provisioning protocol), keyed
# dnidx:sp:leg:side. The memo is load-bearing,
# not tidiness: cases A and D call dn_side again at the failover flip to move
# the primary, and re-converging an already-serving side with
# provisioned = false is a perfectly legal request that the converge matrix
# answers with "no exports" (architecture.md, Side provisioning protocol,
# Converge matrix row 3) — i.e. it would retract the
# live export stacks in the middle of a failover. Phase 1 therefore runs
# exactly once per side. Each case resets the map.
declare -A SIDE_PROVISIONED=()

# dn_side converges one side: the DN-side backing every CN leg connects to.
#
# The first converge of a side is two-phase, this script playing the sp-worker's
# flip rule (architecture.md, sp role, Provisioning gate): sync it
# unprovisioned — which allocates the
# extent runs, builds DnSideName and starts the background zeroing goroutine,
# and exports nothing — wait for every logical extent to be zeroed, then re-sync
# it provisioned at a fresh revision. Every later converge of the same side goes
# straight to phase 2.
dn_side() { # dnidx sp leg side ext_cnt primary_cn [standby_cn]
	local idx=$1 out extra=() key="$1:$2:$3:$4" cn field zeroed total
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
		# *and* the standby (cnagent_integtest.md, Conventions, Sides first).
		# Cases A and D give the
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
		# The provisioning window, observed and never asserted
		# (cnagent_integtest.md, Cases, What a pass means; the dn suite's twin is
		# dnagent_integtest.md, Conventions, "Observed, never asserted"): phase 1
		# only *starts* the background zeroing
		# goroutine, so a sample taken here normally still catches the side
		# mid-flight, which is what makes the gate assertions above a reading
		# of a genuinely closed gate rather than of an already-finished one.
		# With 64 MiB extents on a loop device the kernel maps Write Zeroes
		# onto fallocate, so a whole side can also finish before this line
		# runs: a miss is timing, not a fault, and the hard proof stays the
		# wait-zeroed below plus the phase-2 OK statuses. A failed call
		# degrades to a miss for the same reason — under `set -euo pipefail`
		# an unguarded one-shot RPC would turn a transient dial error into a
		# suite abort, and this helper runs on every first converge of every
		# side. get-side-info prints the reply before it checks the reply
		# code, so only a call that never reached the agent leaves nothing to
		# read.
		out=$(dnctl "$idx" get-side-info --sp "$2" --leg "$3" --side "$4" ||
			true)
		[ -n "$out" ] || out="{}"
		zeroed=$(jq_of "$out" '.side_info.zeroed_ext_cnt // "0"')
		total=$(jq_of "$out" '.side_info.total_ext_cnt // "0"')
		if [ "$total" -gt 0 ] && [ "$zeroed" -lt "$total" ]; then
			log "dn$idx sp $2 leg $3: provisioning window HIT ($zeroed/$total zeroed)"
		else
			log "dn$idx sp $2 leg $3: WARNING provisioning window missed ($zeroed/$total)"
		fi
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
	# The gate open: every per-CN row of all three maps is OK, for the
	# primary and any standby alike — phase 1's loop with the status turned
	# round. Reading the primary's export alone left the standby's stack, the
	# one a failover promotes onto, unexamined.
	for cn in "$6" ${7:+"$7"}; do
		for field in cn_id_to_dm_error cn_id_to_dm_linear cn_id_to_nvmeof; do
			assert_ok "$out" ".side_info.$field[\"$(d16 "$cn")\"].status" \
				"dn$idx sp $2 leg $3 export to cn $cn: $field[$cn]"
		done
	done
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

# dn_drop_until_clean is dn_drop's retrying twin, for the case-T stages where
# the DN's own sweep may legally need more than one pass. It re-sends the SAME
# empty side list at the SAME revision until the reply carries code 0, which is
# exactly what the worker does every round while the code is non-zero (RW12):
# the agent stores nothing at all about a leftover — the verdict is an
# enumeration of the node, recomputed from scratch on every request — so the
# retry carries no new information and needs none.
#
# Unlike dn_drop it does NOT bump the revision. The revision is the desired
# state's, not the attempt's, and an equal-revision re-send is a legal full
# re-apply (cnagent_integtest.md, Conventions, Revisions); bumping per attempt
# would mean the suite could no longer tell
# a sweep that re-ran from one that only ran because something looked new.
#
# Only ReplyCodeLeftover (4) is tolerated in between. Every other non-zero code
# is a REFUSAL — the request never applied at all — and must fail the stage
# rather than be retried into a timeout that names the wrong thing.
dn_drop_until_clean() { # dnidx secs
	local idx=$1 secs=$2 out code details deadline first=1 field
	deadline=$((SECONDS + secs))
	while :; do
		# No --expect-code: the code is what is being read, not asserted, and
		# dnagentctl prints the reply before it checks it, so a code-4 exit
		# still leaves the details on stdout.
		out=$(dnctl "$idx" syncup-dn --revision "${DNREV[$idx]}" \
			--extent-size "$EXTENT_SIZE" || true)
		[ -n "$out" ] ||
			die "dn$idx syncup-dn printed no reply — the call never reached the agent"
		code=$(jq_of "$out" '.agent_reply.code // 0')
		details=$(jq_of "$out" '.agent_reply.details // ""')
		# The FIRST reply is the evidence this helper exists to produce: a
		# sweep that REPORTED its leftover is a different animal from the old
		# teardown, which dropped the failure, deleted the object's state file
		# anyway and replied OK. Only this reply tells the two apart, and it
		# is gone by the time the loop ends.
		if [ "$first" -eq 1 ]; then
			log "[$STAGE] dn$idx first drop reply: code $code, details '$details'"
			first=0
		fi
		if [ "$code" = 0 ]; then
			for field in disk_info meta_info port_info; do
				assert_ok "$out" ".dn_info.$field.status" "dn$idx teardown"
			done
			return 0
		fi
		[ "$code" = 4 ] ||
			die "dn$idx syncup-dn replied code $code ($details), want 0 or 4"
		[ "$SECONDS" -lt "$deadline" ] ||
			die "dn$idx still reports leftovers after ${secs}s: $details"
		sleep 2
	done
}

# cn_drop empties a CN's cntlr pointer list, which is the declarative cntlr
# teardown of CN7/CN21. The sweep sets its leg disconnects going off its locks
# (CN21), so the pass that does replies ReplyCodeLeftover naming the
# connections, and the same request is clean once they have returned: the
# worker's re-send, which is cn_drop_until_clean at the new revision.
cn_drop() { # cnidx
	bump_cn_sync "$1"
	cn_drop_until_clean "$1" 30
}

# cn_drop_until_clean is the retrying half of cn_drop — see
# dn_drop_until_clean for why it re-sends rather than bumps, why only code 4 is
# tolerated in between, and why the first reply is logged.
#
# The request it sends is cn_drop's, an empty cntlr pointer list, at ${CNSYNC},
# the revision SyncupCn last stored on that node. Called directly (case T's CN1
# drops), that is the revision the shape's own SyncupCn used, so the first call
# is an equal-revision re-apply whose body happens to have lost the cntlr
# pointer — legal, and precisely the shape the sp-worker produces when it
# deletes a cntlr record without waiting for anybody (cnagent_integtest.md,
# Cases, teardown). Called from
# cn_drop, CNSYNC has just been bumped: the first call is the new revision and
# the rest are the worker's re-sends of it.
cn_drop_until_clean() { # cnidx secs
	local idx=$1 secs=$2 out code details deadline first=1
	deadline=$((SECONDS + secs))
	while :; do
		out=$(cnctl "$idx" syncup-cn --revision "${CNSYNC[$idx]}" || true)
		[ -n "$out" ] ||
			die "cn$idx syncup-cn printed no reply — the call never reached the agent"
		code=$(jq_of "$out" '.agent_reply.code // 0')
		details=$(jq_of "$out" '.agent_reply.details // ""')
		if [ "$first" -eq 1 ]; then
			log "[$STAGE] cn$idx first drop reply: code $code, details '$details'"
			first=0
		fi
		if [ "$code" = 0 ]; then
			assert_cn_info_ok "$out" "cn$idx teardown"
			return 0
		fi
		[ "$code" = 4 ] ||
			die "cn$idx syncup-cn replied code $code ($details), want 0 or 4"
		[ "$SECONDS" -lt "$deadline" ] ||
			die "cn$idx still reports leftovers after ${secs}s: $details"
		sleep 2
	done
}

# wait_legs_probed polls a cntlr's GetCntlrInfo until no leg row reads
# RES_STATUS_PENDING — CN11's "the prober has not completed a round", which a
# primary reports for every leg whose prober has just started (a build, a
# promotion, an agent restart) until the first CnLegProbeInterval tick plus the
# probe itself. A standby never reports it, so there the first poll returns.
wait_legs_probed() { # cnidx sp cntlr secs label
	local deadline=$((SECONDS + $4)) out pending
	while :; do
		out=$(cnctl "$1" get-cntlr-info --sp "$2" --cntlr "$3")
		pending=$(jq_of "$out" \
			'[(.cntlr_info.leg_id_to_leg // {})[] | select(.status == "RES_STATUS_PENDING")] | length')
		[ "$pending" = 0 ] && return 0
		[ "$SECONDS" -lt "$deadline" ] ||
			die "$5: cn$1 still reports $pending leg(s) RES_STATUS_PENDING after $4s"
		sleep 1
	done
}

# converge_check runs the check-cn/check-cntlr round pair
# (cnagent_integtest.md, Conventions, Steady state) and asserts that
# each reply echoes the revision the agent actually stored. A primary's steady
# state includes a completed probe round on every leg, so the cntlr round waits
# out CN11's PENDING window first (wait_legs_probed) — a stage that built or
# promoted a primary a few seconds earlier would otherwise race it.
converge_check() { # cnidx sp cntlr cntlrrev
	local idx=$1 out
	out=$(cnctl "$idx" check-cn --revision "${CNSYNC[$idx]}" --show-info)
	assert_cn_info_ok "$out" "cn$idx check-cn"
	assert_eq "$(jq_of "$out" '.revision // "0"')" "${CNSYNC[$idx]}" \
		"cn$idx check-cn revision"
	wait_legs_probed "$idx" "$2" "$3" 20 "cn$idx check-cntlr"
	out=$(cnctl "$idx" check-cntlr --revision "$4" --show-info \
		--sp "$2" --cntlr "$3")
	assert_eq "$(jq_of "$out" '.revision // "0"')" "$4" \
		"cn$idx check-cntlr revision"
	assert_all_ok "$out" .cntlr_info "cn$idx check-cntlr"
}

# bitmap_hex runs one of the bitmap RPCs and returns just the hex line; the
# second line of the reply is the bit count
# (cnagent_integtest.md, The driver: `cnagentctl`). Taking it here rather than
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
# of one converge, which is how the orderings of CN9, CN18 and
# architecture.md, Clone crash recovery, are asserted.
event_line() { # stream regex
	printf '%s\n' "$1" | grep -nE -- "$2" | head -1 | cut -d: -f1 || true
}

# event_last is event_line's twin for the LAST match: the anchor when an
# earlier event of the same shape can belong to something else (case C stage
# 6's destination-bitmap read against the pool's activation sweep).
event_last() { # stream regex
	printf '%s\n' "$1" | grep -nE -- "$2" | tail -1 | cut -d: -f1 || true
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
# that sends *no* pool message (cnagent_integtest.md, Cases, thinbm) leaves
# nothing behind for an ordering assertion to anchor on.
assert_absent() { # stream regex label
	local at
	at=$(event_line "$1" "$2")
	[ -z "$at" ] || die "$3: unexpected event matching '$2' at $at"
}

# ---------------------------------------------------------------------------
# Case S — smoke (cnagent_integtest.md, Cases)
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
	assert_leg_pending "$out" "$S_MLEG" smoke
	assert_leg_pending "$out" "$S_DLEG" smoke
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
	# The kind-c9 leg wrappers and the kind-ca RedundNone group devices are the
	# two dm layers the reply names only indirectly.
	for name in "$(cn_dm_name c9 "$cn" "$sp" "$S_MLEG")" \
		"$(cn_dm_name c9 "$cn" "$sp" "$S_DLEG")" \
		"$(cn_dm_name ca "$cn" "$sp" "$S_MGRP")" \
		"$(cn_dm_name ca "$cn" "$sp" "$S_DGRP")"; do
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
	# The thin-pool auto-grow (architecture.md, Automatic reactions) parses
	# metadata and data used/total out of this raw
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
# Case A — redund (cnagent_integtest.md, Cases): raid1, failover, dead leg,
# readonly, late flip
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
	local writer deadline mkey dkey lkey mst dst ldet n rules mark cmds name
	diag_cntlr 1 "$sp" "$c1"
	diag_cntlr 2 "$sp" "$c2"
	dev=$(host_dev "$uuid")
	nsdev=$(cn_dm_name c6 2 "$sp" "$A_NS")

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
	# whole-side zeroing (architecture.md, Side provisioning protocol) is
	# exactly the freshly-provisioned case
	# ([D15] replaced the old trim) — so the arrays are created,
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
	# CN16 rule 2: a standby's ns-dev is a linear over the td's dm-error.
	assert_eq "$(helper 2 "dm_backing $nsdev")" \
		"$(helper 2 "dm_devno $(cn_dm_name c5 2 "$sp" "$A_TD")")" \
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
		"^dmsetup reload $(cn_dm_name c6 1 "$sp" "$A_NS") " \
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
	# The promote may only run once the legs are optimized on CN2
	# (cnagent_integtest.md, Conventions, Sides first).
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

	stage degrade "a dead leg: the md rows stay OK, read from sysfs"
	# CN28 as amended 2026-09-26: the md row is read from /sys/block/mdN/md,
	# never from `mdadm --detail`, which loads the superblock from a member
	# device and, when that member's DN side has gone, blocks until the
	# path's failfast expires — ~13 s after the side died, past the 3 s soft
	# timeout. The kill turned the md row ERROR, an md row counts toward
	# cntlr health, and that failed the primary over: the first failover of
	# the ping-pong found 2026-09-24. The partition (case T's, on vm1's
	# INPUT from vm2 to the nvme-tcp port) takes CN2's two legs into DN1
	# away without the DN agent: meta leg 1 and data leg 1 — leg_idx 0, so
	# disk 0 of their arrays, the member the old `--detail` loaded its
	# superblock from. It also cuts the emulated host's path to CN1, the
	# standby's, inaccessible since the failover; it reconnects once the
	# rule goes, and the lateflip stage waits for it to be live before it
	# reads through it.
	assert_eq "$(helper 1 have_iptables)" yes \
		"degrade: vm1 needs iptables to partition the nvme-tcp port"
	assert_eq "$(helper 1 "partition_from ${IP[2]}")" partitioned \
		"degrade: the partition rule was not installed"
	# md learns that a member is dead only from an IO to it. This write goes
	# to both data legs; the one into DN1 hangs on the black-holed connection
	# until CN2's controller gives up on it at its keep-alive expiry. md sends
	# member writes failfast, so the controller's error recovery fails that
	# one then, rather than parking it at the multipath head for the failfast
	# interval as it parks the plain read an `mdadm --detail` makes; md fails
	# that member and the write completes on DN2's. The MiB
	# is a fresh one, so the readback at the end of the stage can tell it
	# from the one stage failover left at 9.
	make_pattern "$hv" "$WORK/probe-redund.bin" 1
	got=$(sha_range "$hv" "$WORK/probe-redund.bin" 1)
	write_range "$hv" "$WORK/probe-redund.bin" "$dev" 1 9 &
	writer=$!
	# The write is seen stuck, not assumed: the host's dd in D state and the
	# writer still running, while CN2's path into DN1 has not yet left
	# `live`. A write that completed instead — a partition that did not
	# take, a member md had failed before it — would leave the stage with
	# no IO on the dead member, and md with no reason to fail it. The pair
	# is a screen, not a proof (wait_write_blocked says how it can miss):
	# past it, a partition that did not take is still stopped where the
	# path must leave `live`, but a member md had failed before it is not.
	helper "$hv" "wait_write_blocked '$dev' 20" >/dev/null ||
		die "degrade: the background host write never blocked on the dead member"
	kill -0 "$writer" 2>/dev/null ||
		die "degrade: the background host write completed although DN1's" \
			"member is partitioned away"
	# The rounds begin once that path has left `live` (its State, for
	# leg_wait_not_live's reason), which is when the controller's error
	# recovery fails the write above: from there on a plain member read, the
	# old `mdadm --detail` probe's, sits at the multipath head until the
	# failfast interval expires, so these rounds are the ones it would fail.
	leg_wait_not_live 2 "$sp" "${A_DLEG[1]}" "${CNID[2]}" 1 60
	mkey=$(d16 "$A_MGRP")
	dkey=$(d16 "$A_DGRP")
	deadline=$((SECONDS + 90))
	while :; do
		out=$(cnctl 2 check-cntlr --revision "$rev2" --show-info \
			--sp "$sp" --cntlr "$c2")
		mst=$(jq_of "$out" \
			".cntlr_info.grp_id_to_md_raid[\"$mkey\"].status // \"ABSENT\"")
		dst=$(jq_of "$out" \
			".cntlr_info.grp_id_to_md_raid[\"$dkey\"].status // \"ABSENT\"")
		# Every round, not just the last: the window this stage exists for
		# is the one where the member is dead and md has not failed it yet.
		[ "$mst" = RES_STATUS_OK ] && [ "$dst" = RES_STATUS_OK ] ||
			die "degrade: an md row read meta $mst / data $dst with a dead" \
				"member: $(jq_of "$out" '.cntlr_info.grp_id_to_md_raid')"
		ldet=$(jq_of "$out" \
			".cntlr_info.grp_id_to_md_raid[\"$dkey\"].details // \"\"")
		n=0
		for lkey in "$(d16 "${A_MLEG[1]}")" "$(d16 "${A_DLEG[1]}")"; do
			[ "$(jq_of "$out" \
				".cntlr_info.leg_id_to_leg[\"$lkey\"].status // \"\"")" = \
				RES_STATUS_ERROR ] && n=$((n + 1))
		done
		case "$ldet" in
		*degraded*) [ "$n" = 2 ] && break ;;
		esac
		[ "$SECONDS" -lt "$deadline" ] ||
			die "degrade: after 90 s the data md row reads '$ldet' and" \
				"$n of the two dead legs read RES_STATUS_ERROR"
		sleep 1
	done
	log "  degraded: data md row '$ldet'"
	deadline=$((SECONDS + 60))
	while kill -0 "$writer" 2>/dev/null; do
		[ "$SECONDS" -lt "$deadline" ] ||
			die "degrade: the host write never completed on the surviving leg"
		sleep 1
	done
	wait "$writer" || die "degrade: the host write through DN2's leg failed"
	# No md probe of this stage ran mdadm — not `--detail`, not anything:
	# the check rounds read /sys/block. The listing is the positive control
	# that the rounds' commands are under this trace at all.
	seq=$(helper 2 "cn_events $TRACE")
	[ "$(event_cnt "$seq" '^ls -1 /sys/block$')" != 0 ] ||
		die "degrade: no sysfs walk of the check rounds is under this trace"
	assert_eq "$(event_cnt "$seq" '^mdadm ')" 0 \
		"degrade: a check round ran mdadm (the md row must come from sysfs)"
	# Restored before the readonly stage, which needs every leg serving.
	assert_eq "$(helper 1 "unpartition_from ${IP[2]}")" unpartitioned \
		"degrade: the partition rule was not removed"
	rules=$(helper 1 "partition_rules ${IP[2]}")
	[ -z "$rules" ] || die "degrade: vm1 still holds partition rules: $rules"
	leg_wait_live 2 "$sp" "${A_MLEG[1]}" "${CNID[2]}" 1 60
	leg_wait_live 2 "$sp" "${A_DLEG[1]}" "${CNID[2]}" 1 60
	# The leg rows clear on their probers' next completed round. The md
	# member md failed stays failed — CN12 re-adds a leg only when the array
	# lacks it (cnagent.md, Known limits) — so the data array stays
	# degraded, and OK, on CN2 through the readonly stage; the lateflip
	# stage assembles the arrays anew on CN1.
	deadline=$((SECONDS + 60))
	while :; do
		out=$(cnctl 2 get-cntlr-info --sp "$sp" --cntlr "$c2")
		n=0
		for lkey in "$(d16 "${A_MLEG[1]}")" "$(d16 "${A_DLEG[1]}")"; do
			[ "$(jq_of "$out" \
				".cntlr_info.leg_id_to_leg[\"$lkey\"].status // \"\"")" = \
				RES_STATUS_OK ] && n=$((n + 1))
		done
		[ "$n" = 2 ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "degrade: $n of the two restored legs read RES_STATUS_OK" \
				"after 60 s"
		sleep 1
	done
	# $got is the digest of the fresh MiB the background write carried,
	# which could land on DN2's member alone: md had failed DN1's.
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 1 9)" "$got" "redund degraded readback"

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

	stage lateflip "a promotion that outruns the sides' flip completes on the retry"
	# CN12 as amended 2026-09-26, link 1 of the failover ping-pong: a
	# failover's SyncupCntlr waits for the sides' SyncupSide answers, but one
	# cntlr_interval at most ([D16]), so a promoted standby can still read its
	# legs before any side has flipped. Every path is then live but still
	# non-optimized, no leg is available, and both groups report "no available
	# leg". Nothing re-drives that converge — the worker re-syncs on a
	# revision or a reply code, never on a row — so the agent's CN10 retry,
	# registered for the late members, must finish it. This stage promotes
	# CN1 BEFORE it flips any side, then
	# flips them all: the arrays must be assembled by a retry attempt, under a
	# trace id of its own, and CN1 must see no second SyncupCntlr. CN1
	# assembles from the members' superblocks, where the data array's DN1
	# leg is the member md failed on CN2 in the degrade stage: md kicks that
	# stale member from the assembly, and CN12's member reconciliation adds
	# it back. An attempt that runs between two flips sees one member of a
	# group available: mdadm refuses to start the array from a member whose
	# superblock still counts both ([D16]), or starts it degraded from the
	# fresher one and a later attempt adds the other. Either way the rows
	# end OK, degraded or not, and that is all this stage asserts of them.
	# The readonly stage restored READWRITE on req2, and req1 never left it.
	# Host IO is quiesced from the demote until the rows read OK: the build
	# moves CN1's namespace to optimized with the promotion itself (CN16's
	# ANA rule does not wait for the stack), over the td's dm-error until a
	# converge has built the raid0. The host's path to CN1 went down with the
	# degrade stage's partition; it must be back for the readback below.
	helper "$hv" "wait_path_live '$nqn' '${IP[1]}' 30" ||
		die "lateflip: the host path to cn1 is not live again"
	mark=$(helper 1 cn_log_lines)
	req_set "$req2" '.cntlr.primary = false'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund lateflip demoted"
	req_set "$req1" '.cntlr.primary = true'
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_status "$out" grp_id_to_md_raid "$A_MGRP" RES_STATUS_ERROR \
		"no available leg" "redund lateflip promoted"
	assert_map_status "$out" grp_id_to_md_raid "$A_DGRP" RES_STATUS_ERROR \
		"no available leg" "redund lateflip promoted"
	seq=$(helper 1 "cn_events $TRACE")
	assert_eq "$(event_cnt "$seq" '^mdadm --(assemble|create) ')" 0 \
		"redund lateflip: the promotion assembled nothing"
	dn_side 1 "$sp" "${A_MLEG[1]}" "${A_MSIDE[1]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 1 "$sp" "${A_DLEG[1]}" "${A_DSIDE[1]}" 2 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_MLEG[2]}" "${A_MSIDE[2]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_DLEG[2]}" "${A_DSIDE[2]}" 2 "${CNID[1]}" "${CNID[2]}"
	for i in 1 2; do
		leg_wait_ana 1 "$sp" "${A_MLEG[$i]}" "${CNID[1]}" "$i" optimized 20
		leg_wait_ana 1 "$sp" "${A_DLEG[$i]}" "${CNID[1]}" "$i" optimized 20
	done
	# The retry re-runs the converge every CnConnectRetryInterval (5 s); an
	# attempt holds the cntlr's lock, which GetCntlrInfo takes too, so a row
	# read here is never one from the middle of an attempt.
	mkey=$(d16 "$A_MGRP")
	dkey=$(d16 "$A_DGRP")
	deadline=$((SECONDS + 15))
	while :; do
		out=$(cnctl 1 get-cntlr-info --sp "$sp" --cntlr "$c1")
		mst=$(jq_of "$out" \
			".cntlr_info.grp_id_to_md_raid[\"$mkey\"].status // \"ABSENT\"")
		dst=$(jq_of "$out" \
			".cntlr_info.grp_id_to_md_raid[\"$dkey\"].status // \"ABSENT\"")
		[ "$mst" = RES_STATUS_OK ] && [ "$dst" = RES_STATUS_OK ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "lateflip: 15 s after the flip the md rows read meta $mst /" \
				"data $dst: $(jq_of "$out" '.cntlr_info.grp_id_to_md_raid')"
		sleep 1
	done
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "redund lateflip retried"
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "redund lateflip retried"
	log "  after the retry: data md row" \
		"'$(jq_of "$out" ".cntlr_info.grp_id_to_md_raid[\"$dkey\"].details")'"
	# Who assembled: every `mdadm --assemble` CN1 ran since the demote, with
	# the trace id it ran under. None may be the promotion's, and each array
	# must have been assembled (an attempt between two flips may add a run
	# that mdadm refused, above).
	cmds=$(helper 1 "cn_cmds_since $mark 'mdadm --assemble '")
	[ -n "$cmds" ] || die "lateflip: cn1 ran no mdadm --assemble since the demote"
	[ "$(event_cnt "$cmds" "^$TRACE ")" = 0 ] ||
		die "lateflip: an mdadm --assemble ran under the promotion's" \
			"trace $TRACE: $cmds"
	for name in "$(jq_of "$mdmeta" .array_name)" \
		"$(jq_of "$mddata" .array_name)"; do
		[ "$(event_cnt "$cmds" " --name $name ")" != 0 ] ||
			die "lateflip: no retry attempt assembled array $name: $cmds"
	done
	assert_eq "$(helper 1 "cn_requests_since $mark SyncupCntlr $(d16 "$sp") $(d16 "$c1")")" \
		1 "lateflip: SyncupCntlr requests for cntlr $c1 on cn1 since the demote"
	# The retry stops once no member is late (and no connect fails): after
	# the attempt that is still due, if any, CN1 runs no command under any
	# trace id but this stage's own for a whole retry interval.
	deadline=$((SECONDS + 30))
	while :; do
		mark=$(helper 1 cn_log_lines)
		sleep 7
		cmds=$(helper 1 "cn_cmds_since $mark ''" | grep -v "^$TRACE " || true)
		[ -z "$cmds" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "lateflip: cn1 still runs converges of its own: $cmds"
	done
	host_wait_ana "$hv" "$nqn" 1 optimized 30
	assert_eq "$(host_ana "$hv" "$nqn" 2)" inaccessible \
		"redund lateflip demoted primary path"
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 8)" "$want" "redund lateflip readback"
	make_pattern "$hv" "$WORK/probe-redund.bin" 1
	got=$(sha_range "$hv" "$WORK/probe-redund.bin" 1)
	write_range "$hv" "$WORK/probe-redund.bin" "$dev" 1 9
	drop_caches "$hv"
	assert_eq "$(sha_range "$hv" "$dev" 1 9)" "$got" "redund lateflip write"

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
# Case T — teardown (cnagent_integtest.md, Cases, teardown)
# ---------------------------------------------------------------------------
#
# The bug this case exists for (cnagent_integtest.md, Cases, teardown):
# `dnvctl sp delete` drains an sp by
# deleting the cntlr records and, 12 ms later, the slice records, and by design
# never waits on an agent. So a CN is told to tear its stack down while the DN
# sides under it are already vanishing. In that window `mdadm --detail` blocked
# on a member read that sat in the multipath head's requeue list, was killed at
# the 3 s soft timeout, and the old `Md.Detail` read the kill as "the array is
# absent" — so no `mdadm --stop` was issued, the un-stopped array pinned its two
# leg wrappers, both `dmsetup remove`s failed EBUSY, the failures were
# discarded, the cntlr's state file and memory entry were deleted anyway and
# the reply was OK. Nothing ever looked at those two wrappers again.
#
# What replaced it (architecture.md, Teardown by sweep; cnagent.md CN21, CN30):
# removal is a SWEEP. Whatever the node
# actually holds, minus what the desired state wants, is removed top-down;
# every removal is verified by a probe that cannot block on a dead remote; and
# "something is left" is recomputed from scratch on every Syncup* and every
# Check*/Get*Info and travels as agent_reply.code = ReplyCodeLeftover (4),
# details `leftover(<n>): kind:name, …`. Nothing about a failure is stored, so
# the retry is nothing more than the same request at the same revision — which
# is what cn_drop_until_clean/dn_drop_until_clean do here and what the worker
# does in production.
#
# Five stages on one shape, case_redund's A-shaped raid1 SP: a primary cntlr on
# CN1, a standby on CN2, four sides across both DNs, one host-facing subsystem
# and the emulated host connected to both CNs. S1-S4 ask the dead-remote
# question — does a teardown FINISH when the objects under it are gone,
# long-gone, under load, or unreachable — and S5 asks the complementary one
# that no amount of dead-remote testing can answer: when a removal genuinely
# cannot be done, is it REPORTED and retried rather than forgotten. S5 runs
# last on purpose, because it is the only stage that pins a dm device open, and
# a pin left behind by an earlier stage's failure could otherwise pass for its
# own.
#
# The bound every stage's patience comes from (cnagent_integtest.md, Cases,
# teardown; dnagent.md SH20): legs are connected
# with fast_io_fail_tmo = 5 and ctrl_loss_tmo = -1, so from 5 s after a path
# loss every IO queued at that multipath head fails at once, and a controller
# stuck in `connecting` can always be disconnected. The bound is an absolute
# deadline from the path loss, not a per-command budget, so a pass blocks for
# about one failfast interval plus a few soft timeouts however many commands it
# issues. A partition is the same shape with the host-side keep-alive timeout
# added in front, which is why S4 alone is given 90 s and not 60. Those
# seconds are retry windows, not deadlines: cn_drop_until_clean and
# dn_drop_until_clean read the clock only between calls, so one more call
# can start as a window closes, and it carries the syncup's own deadline.

# One sp per stage. The stages run in order and each one ends with its shape
# fully torn down, so a shared sp would work — but a distinct one is what makes
# a residue report name the stage that made the debris instead of the stage
# that found it. The uuids differ for the same reason: host_dev addresses a
# namespace by uuid, so a stale /dev/disk/by-id node from an earlier stage
# would otherwise be indistinguishable from this stage's own.
T_C1=0x1
T_C2=0x2
T_SP=("" 0x3f1 0x3f2 0x3f3 0x3f4 0x3f5)
T_UUID=(""
	77777777-7777-4777-8777-777777777771
	77777777-7777-4777-8777-777777777772
	77777777-7777-4777-8777-777777777773
	77777777-7777-4777-8777-777777777774
	77777777-7777-4777-8777-777777777775)

# teardown_shape builds the case's one shape. It is case_redund's `dn` and `cn`
# stages (cnagent_integtest.md, Cases, redund) with the failover half left off
# — the same builders in the same
# order, not a second shape — because what five teardowns have to be compared
# against is one stack, built identically every time.
teardown_shape() { # sp nqn uuid req1 req2 hostvm
	local sp=$1 nqn=$2 uuid=$3 req1=$4 req2=$5 hv=$6 out dev
	dn_pointers 1 "$sp:${A_MLEG[1]}:${A_MSIDE[1]}" "$sp:${A_DLEG[1]}:${A_DSIDE[1]}"
	dn_pointers 2 "$sp:${A_MLEG[2]}:${A_MSIDE[2]}" "$sp:${A_DLEG[2]}:${A_DSIDE[2]}"
	dn_side 1 "$sp" "${A_MLEG[1]}" "${A_MSIDE[1]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 1 "$sp" "${A_DLEG[1]}" "${A_DSIDE[1]}" 2 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_MLEG[2]}" "${A_MSIDE[2]}" 1 "${CNID[1]}" "${CNID[2]}"
	dn_side 2 "$sp" "${A_DLEG[2]}" "${A_DSIDE[2]}" 2 "${CNID[1]}" "${CNID[2]}"
	bump_cn_sync 1
	out=$(cnctl 1 syncup-cn --revision "${CNREV[1]}" --cntlr "$sp:$T_C1")
	assert_cn_info_ok "$out" "$STAGE syncup-cn 1"
	bump_cn_sync 2
	out=$(cnctl 2 syncup-cn --revision "${CNREV[2]}" --cntlr "$sp:$T_C2")
	assert_cn_info_ok "$out" "$STAGE syncup-cn 2"
	req_raid1 "$req1" 1 "$sp" "$T_C1" 0 true "$nqn" "$uuid"
	req_raid1 "$req2" 2 "$sp" "$T_C2" 1 false "$nqn" "$uuid"
	bump_cn_rev 1
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" grp_id_to_md_raid "$A_MGRP" "$STAGE primary"
	assert_map_ok "$out" grp_id_to_md_raid "$A_DGRP" "$STAGE primary"
	assert_map_ok "$out" slice_id_to_dm_pool "$A_SLICE" "$STAGE primary"
	assert_map_ok "$out" td_id_to_raid0 "$A_TD" "$STAGE primary"
	assert_map_ok "$out" ns_id_to_namespace "$A_NS" "$STAGE primary"
	bump_cn_rev 2
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" leg_id_to_leg "${A_DLEG[1]}" "$STAGE standby"
	assert_map_ok "$out" ns_id_to_dm_linear "$A_NS" "$STAGE standby"
	# Both md arrays really are assembled before anything is torn down: a
	# stage that tore down a stack that had never come up would pass every
	# residue assertion below and prove nothing at all.
	out=$(helper 1 mdstat | grep -c '\[UU\]' || true)
	assert_eq "$out" 2 "$STAGE: both arrays up ([UU]) before the teardown"
	# The host is connected because a real teardown has a host on it: its
	# controller is what the nvmet removals in L1 have to kill, and in S3 it
	# is what carries the in-flight IO.
	host_connect "$hv" 1 "$nqn"
	host_connect "$hv" 2 "$nqn"
	dev=$(host_dev "$uuid")
	helper "$hv" "wait_dev '$dev' 20" || die "no device node $dev on vm$hv"
	host_wait_ana "$hv" "$nqn" 1 optimized 20
}

# teardown_assert_clean is the closing assertion every stage of this case
# shares. It is the union of the checks the suite already ends its teardowns
# with, in one place because five copies of it would drift: nothing of the sp
# may be left on either VM in dm, nvmet or md — which covers the DN's side
# devices and :2: exports as well as the CN's stack — neither CN may hold a dm
# device or a host-facing subsystem of its own, the clone arena must be empty,
# and both CNs must still serve their four base-state resources
# (architecture.md, Controller node, common). That last
# one is not redundant: a teardown that took the port, the tmpfs or the loop
# arena with it would satisfy every residue check above and still leave the CN
# unable to build the next SP.
#
# get-cn-info is also the node-level verdict (cnagent.md CN30), and cnagentctl
# defaults to --expect-code 0, so each of these two calls additionally asserts
# that the CN's own read-only sweep finds nothing — the same property from the
# other side of the lock.
teardown_assert_clean() { # sp label
	local idx got out
	assert_no_residue "$1"
	for idx in 1 2; do
		got=$(helper "$idx" "cn_residue $(hex16 "${CNID[$idx]}")")
		[ -z "$got" ] || die "$2: vm$idx still holds cn objects: $got"
		got=$(helper "$idx" "clone_meta_wrappers $(hex16 "${CNID[$idx]}")")
		[ -z "$got" ] ||
			die "$2: vm$idx's clone arena still holds wrappers: $got"
		out=$(cnctl "$idx" get-cn-info)
		assert_cn_info_ok "$out" "$2 cn$idx base state"
	done
}

# s1_sides_gone is the issue's own shape: the DN sides go and the CN is told to
# tear down while they are still vanishing, with nothing in between. The host
# is disconnected first so that no host IO is in flight — that is what makes
# the first command to touch a dead leg the array's own member read rather than
# a flush, and it is why this stage is the teardown-by-sweep plan's one-time
# mutation check (cnagent_integtest.md, Cases, teardown): on the pre-fix binary
# it is expected to fail at its residue
# assertion, with the two kind-c9 wrappers of a group pinned by an array that
# was never stopped.
s1_sides_gone() {
	local sp=${T_SP[1]} uuid=${T_UUID[1]} hv=2
	local nqn="$NQN_IT_PREFIX:t:s1"
	local req1="$WORK/req-teardown-s1-cn1.json"
	local req2="$WORK/req-teardown-s1-cn2.json"
	DIAG_CNTLRS=()
	diag_cntlr 1 "$sp" "$T_C1"
	diag_cntlr 2 "$sp" "$T_C2"

	stage s1build "S1: the raid1 shape, primary on CN1, standby on CN2"
	teardown_shape "$sp" "$nqn" "$uuid" "$req1" "$req2" "$hv"

	stage s1 "S1: both DNs drop their sides, then CN1 tears down at once"
	host_disconnect "$hv" "$nqn"
	# The DN teardown runs while CN1 still holds every leg connected, and
	# dn_drop's own expect-code 0 is the assertion that it finishes in one
	# pass: everything the DN has to remove is local to the DN, so a live
	# remote above it is not supposed to hold anything back.
	dn_drop 1
	dn_drop 2
	cn_drop_until_clean 1 60
	cn_drop 2
	teardown_assert_clean "$sp" "s1_sides_gone"
}

# s2_paths_long_dead is S1 with the window moved. S1 catches the sweep inside
# the failfast interval, where the first command that touches a dead leg is the
# one that waits; this stage catches it after the interval has expired and
# after the host's own reconnect attempt, which the DN refused with DNR — so
# the leg controllers are deleted and each leg's namespace head with them: IO
# through a leg wrapper fails at once, and a probe for the leg's controller
# finds none. The two are different code paths through every probe in the
# sweep — one returns late, the other at once, failing or finding nothing —
# and only running both says the verdict is the same either way.
s2_paths_long_dead() {
	local sp=${T_SP[2]} uuid=${T_UUID[2]} hv=2 i st stuck
	local nqn="$NQN_IT_PREFIX:t:s2"
	local req1="$WORK/req-teardown-s2-cn1.json"
	local req2="$WORK/req-teardown-s2-cn2.json"
	DIAG_CNTLRS=()
	diag_cntlr 1 "$sp" "$T_C1"
	diag_cntlr 2 "$sp" "$T_C2"

	stage s2build "S2: the same shape again, on its own sp"
	teardown_shape "$sp" "$nqn" "$uuid" "$req1" "$req2" "$hv"

	stage s2 "S2: the legs have been dead for 20 s before the CN is told"
	# The instrument first: path_field answers `none` for a path it cannot
	# find, a listing that failed included, so the `none` asserted after the
	# drop means something only once these same four reads have seen the
	# paths `live`.
	for i in 1 2; do
		assert_eq "$(leg_state 1 "$sp" "${A_MLEG[$i]}" "${CNID[1]}" "$i")" live \
			"s2: cn1 meta leg $i -> dn$i before the sides go"
		assert_eq "$(leg_state 1 "$sp" "${A_DLEG[$i]}" "${CNID[1]}" "$i")" live \
			"s2: cn1 data leg $i -> dn$i before the sides go"
	done
	host_disconnect "$hv" "$nqn"
	dn_drop 1
	dn_drop 2
	# 20 s: past the legs' fast_io_fail_tmo of 5 s (agent/nvmehost.go,
	# common.DefaultNvmeFastIoFailTmo), so every IO queued at those multipath
	# heads has already been failed, and past the controllers' own reconnect
	# attempt — which a removed subsystem refuses with DNR, so by now the
	# paths are not merely failing, they are gone.
	sleep 20
	# Logged, then asserted `none`. ctrl_loss_tmo = -1 does not keep a leg's
	# controller here: each DN's port still listens, because it also carries
	# the host-facing subsystem of the CN on the same VM, so the reconnect to a
	# removed subsystem is refused with DNR and the kernel deletes the
	# controller at that first refusal, whatever ctrl_loss_tmo says
	# (cnagent_integtest.md, Lab facts). A path still `connecting` would mean a
	# reconnect that was
	# not refused with DNR — a port that stopped listening, say, whose refused
	# TCP connect retries for ever at -1 — and a sweep that meets a leg
	# controller still there to disconnect, over a namespace head that still
	# exists, instead of neither: S4's path, not this stage's. The
	# states are logged before they are judged, because what the four paths
	# looked like at this instant is the one thing a failure report needs and
	# cannot reconstruct afterwards — by the time the run fails, the CN has
	# disconnected them all.
	stuck=()
	for i in 1 2; do
		st=$(leg_state 1 "$sp" "${A_MLEG[$i]}" "${CNID[1]}" "$i")
		log "s2: cn1 meta leg $i -> dn$i: state $st"
		[ "$st" = none ] || stuck+=("meta leg $i -> dn$i: $st")
		st=$(leg_state 1 "$sp" "${A_DLEG[$i]}" "${CNID[1]}" "$i")
		log "s2: cn1 data leg $i -> dn$i: state $st"
		[ "$st" = none ] || stuck+=("data leg $i -> dn$i: $st")
	done
	log "s2: cn1 list-subsys: $(helper 1 subsys_json)"
	[ "${#stuck[@]}" -eq 0 ] ||
		die "s2: 20 s after the sides went, cn1 still holds leg paths no DNR" \
			"refusal deleted: ${stuck[*]}"
	cn_drop_until_clean 1 60
	cn_drop 2
	teardown_assert_clean "$sp" "s2_paths_long_dead"
}

# s3_io_in_flight is S1 with host writes running across the whole teardown. It
# is the stage that exercises the waits (cnagent_integtest.md, Cases, teardown;
# dnagent.md SH20) for real: the park of an
# ns-dev is a flushing suspend, the nvmet `enable = 0` above it is an
# uncancellable configfs write, and the thin-pool's postsuspend commit is a
# metadata write — each of them has in-flight host IO through thin -> md -> leg
# to complete against, and each of them is on the removal path. What the stage
# pins is that they complete anyway.
s3_io_in_flight() {
	local sp=${T_SP[3]} uuid=${T_UUID[3]} hv=2 dev got ok eio snap rc
	local nqn="$NQN_IT_PREFIX:t:s3"
	local atdrop="$WORK/s3-writer-at-drop"
	local req1="$WORK/req-teardown-s3-cn1.json"
	local req2="$WORK/req-teardown-s3-cn2.json"
	DIAG_CNTLRS=()
	diag_cntlr 1 "$sp" "$T_C1"
	diag_cntlr 2 "$sp" "$T_C2"

	stage s3build "S3: the same shape again, on its own sp"
	teardown_shape "$sp" "$nqn" "$uuid" "$req1" "$req2" "$hv"

	stage s3 "S3: the sides go and the CN tears down under live host writes"
	dev=$(host_dev "$uuid")
	# The writer is detached on the host VM and runs from before the sides go
	# until after the CN is clean. From the moment the sides are dropped its
	# writes are EXPECTED to fail or to hang, and neither stops it: write_probe
	# reports a refused write as a word and never as an exit status, so nothing
	# a failing write does can end the loop, and a hanging one holds it only
	# until stop_writer. It writes the first 4 MiB in a
	# cycle, which is enough to keep the pool allocating and the arrays
	# writing without turning the stage into a throughput test.
	assert_eq "$(helper "$hv" "start_writer '$dev'")" started \
		"s3: the background writer did not start"
	# Counted, not assumed: a write lands before the sides go (wait_writer
	# says why), and the closing read below finds one that met the dying
	# stack after they went.
	got=$(helper "$hv" "wait_writer ok 1 20") ||
		die "s3: no write of the background writer succeeded before the sides went"
	log "s3: the writer before the sides go: $got"
	dn_drop 1
	dn_drop 2
	# The counts as the sides went, which the closing read is judged against.
	# The read runs in the background so that the CN is still told at once,
	# as in S1: an ssh round trip in between would move the CN teardown later
	# in the legs' failfast window than the stage has always put it.
	helper "$hv" writer_counts >"$atdrop" &
	snap=$!
	cn_drop_until_clean 1 60
	cn_drop 2
	wait "$snap" ||
		die "s3: the background writer's counts could not be read as the sides went"
	got=$(cat "$atdrop")
	[[ $got =~ ^ok=([0-9]+)\ eio=([0-9]+)$ ]] ||
		die "s3: unreadable writer counts '$got'"
	ok=${BASH_REMATCH[1]}
	eio=${BASH_REMATCH[2]}
	log "s3: the writer as the sides went: $got"
	# The closing read (writer_in_flight), with both CNs clean. It asks two
	# things.
	#
	# No write succeeds after the sides went, and the bound is ONE above the
	# `ok` read as they went. From dn_drop 2's return no side is left to
	# complete a write. The writer runs one dd at a time and appends a write's
	# word after its dd exits and before the next one starts, so when that count
	# was read at most one write was still to report — and that one may have
	# been issued before the second side went and been completed by it. Every dd
	# after it started with both sides gone. The count itself lands an ssh round
	# trip after the drop, so a success inside that round trip would go unseen;
	# no read from here can be taken closer.
	#
	# And one write met the dying stack: it FAILED since that read, or it is
	# still OUTSTANDING — the writer's dd, running for 5 s or more and in
	# uninterruptible sleep with both CNs already clean, which leaves it queued
	# on the host. Both are legitimate, and which one a run sees is a race
	# inside the CN teardown. The write in flight as the sides went sits on the
	# legs until their 5 s failfast expires, and the park's flushing suspend
	# waits for it. If it fails while the subsystem is still on the host-facing
	# port, the host gets EIO. If the park is killed at its command timeout
	# first, the teardown unlinks the subsystem from the port with the write
	# still in flight, the host loses its path, and it queues the write: a
	# multipath head keeps IO while any controller of it is still reconnecting,
	# and host_connect leaves their loss timeout at the default 600 s. A lab run
	# met the second, and the earlier form of this check, one `eio` within 20 s,
	# failed on it.
	rc=0
	got=$(helper "$hv" "writer_in_flight '$dev' $((ok + 1)) $eio 5 20") ||
		rc=$?
	case "$rc" in
	0) ;;
	1) die "s3: no write of the background writer failed after the sides" \
		"went and none is outstanding — nothing was in flight across the" \
		"teardown" ;;
	2) die "s3: a write of the background writer succeeded after the sides went" ;;
	*) die "s3: the background writer's state could not be read (rc $rc)" ;;
	esac
	log "s3: the writer after the teardown: $got"
	assert_eq "$(helper "$hv" stop_writer)" stopped \
		"s3: the background writer did not stop"
	# Only now, and in this order. The writer needs the host connected, and
	# host_disconnect is what takes its device node away: a loop still
	# running then would open a path that no longer names the namespace, and
	# dd creates its output, so it would write a regular file into /dev and
	# report `ok`. stop_writer ends the loop, not a write queued at the
	# multipath head; deleting the host's controllers is what fails that one,
	# and wait_writer_gone checks that its dd did exit — a write the
	# disconnect left running would hold the namespace open into S4.
	host_disconnect "$hv" "$nqn"
	helper "$hv" "wait_writer_gone '$dev' 20" >/dev/null ||
		die "s3: a write of the background writer outlived the host's disconnect"
	teardown_assert_clean "$sp" "s3_io_in_flight"
}

# s4_partitioned_dn is the one stage where the remote is not removed but
# unreachable. That is a different window from S1's — the sides, their exports
# and their extents all still exist, and the loss has to be discovered by a
# keep-alive rather than announced by a subsystem going away — and it is the
# window the case header's bound says is the longest, which is why this stage
# alone gets 90 s
# of patience. It also pins the other half: once the partition is lifted, the
# DN sweeps its own side away cleanly with the CN above it already gone.
s4_partitioned_dn() {
	local sp=${T_SP[4]} uuid=${T_UUID[4]} hv=2 got
	local nqn="$NQN_IT_PREFIX:t:s4"
	local req1="$WORK/req-teardown-s4-cn1.json"
	local req2="$WORK/req-teardown-s4-cn2.json"
	DIAG_CNTLRS=()
	diag_cntlr 1 "$sp" "$T_C1"
	diag_cntlr 2 "$sp" "$T_C2"

	stage s4build "S4: the same shape again, on its own sp"
	teardown_shape "$sp" "$nqn" "$uuid" "$req1" "$req2" "$hv"

	stage s4 "S4: DN2 is partitioned away from CN1, then CN1 tears down"
	# The lab prerequisite, asked here rather than in preflight_vms so that a
	# VM without the binary names the stage that needs it instead of failing a
	# run that was never going to reach this case.
	assert_eq "$(helper 2 have_iptables)" yes \
		"s4: vm2 needs iptables to partition the nvme-tcp port"
	host_disconnect "$hv" "$nqn"
	# The rule is vm2's INPUT, from vm1, to the nvme-tcp port: it takes CN1's
	# two paths into DN2's sides and nothing else. The emulated host runs on
	# vm2 and reaches CN1 outbound (the replies carry the port as their SOURCE,
	# which --dport does not match), and CN2's own legs into DN1 are vm1's
	# INPUT, so neither is touched.
	assert_eq "$(helper 2 "partition_from ${IP[1]}")" partitioned \
		"s4: the partition rule was not installed"
	# Both of CN1's paths into DN2, not just one: the meta group's leg is what
	# the array's superblock writes go through and the data group's is what
	# the pool's commit goes through, and the stage wants the sweep to meet
	# both of them dead.
	leg_wait_not_live 1 "$sp" "${A_MLEG[2]}" "${CNID[1]}" 2 60
	leg_wait_not_live 1 "$sp" "${A_DLEG[2]}" "${CNID[1]}" 2 60
	cn_drop_until_clean 1 90
	cn_drop 2
	assert_eq "$(helper 2 "unpartition_from ${IP[1]}")" unpartitioned \
		"s4: the partition rule was not removed"
	# Asserted, not assumed: a rule left behind would black-hole the next
	# case's legs, and the next case would report the wrong cause.
	got=$(helper 2 "partition_rules ${IP[1]}")
	[ -z "$got" ] || die "s4: vm2 still holds partition rules: $got"
	# DN2's sides come down with their CN already gone and their exports still
	# holding a controller record for it, which is the DN-side half of the same
	# question; DN1 was never partitioned and needs no loop.
	dn_drop_until_clean 2 60
	dn_drop 1
	teardown_assert_clean "$sp" "s4_partitioned_dn"
}

# s5_pinned_wrapper is the complementary stage, and the only one that does not
# need a dead remote: everything here is alive and one dm device simply cannot
# be removed. It pins the three properties the reply code exists for — the
# leftover is NAMED, the verdict is RECOMPUTED by a read-only path that issued
# no syncup, and the retry is the same request at the same revision — plus the
# stop rule of cnagent.md CN21 that makes the residue readable at all.
#
# It runs last because it is the only stage that pins a dm device open. A pin
# left behind by an earlier stage's failure would be released by cleanup before
# the next run, but within one run it would sit under whatever came after it,
# so nothing may come after it.
s5_pinned_wrapper() {
	local sp=${T_SP[5]} uuid=${T_UUID[5]} hv=2 pinned out details got
	local nqn="$NQN_IT_PREFIX:t:s5"
	local req1="$WORK/req-teardown-s5-cn1.json"
	local req2="$WORK/req-teardown-s5-cn2.json"
	DIAG_CNTLRS=()
	diag_cntlr 1 "$sp" "$T_C1"
	diag_cntlr 2 "$sp" "$T_C2"

	stage s5build "S5: the same shape again, on its own sp"
	teardown_shape "$sp" "$nqn" "$uuid" "$req1" "$req2" "$hv"

	stage s5 "S5: a pinned leg wrapper is reported, then retried away"
	host_disconnect "$hv" "$nqn"
	pinned=$(cn_dm_name c9 1 "$sp" "${A_DLEG[1]}")
	assert_eq "$(helper 1 "pin_dev $pinned")" pinned "s5: the pin did not take"
	# cn_drop's request, sent by hand because this one must NOT reply 0. The
	# bump is on its own line and in the parent shell for the reason of
	# cnagent_integtest.md, Conventions, Revisions: inside
	# the command substitution it would increment a copy.
	bump_cn_sync 1
	out=$(cnctl 1 syncup-cn --revision "${CNREV[1]}" --expect-code 4)
	# The request was ACCEPTED — the desired state is stored and every wanted
	# object converged — and it still reports the one object that would not go.
	# That is what ReplyCodeLeftover is for: a leftover can have no *Info row,
	# because the rows are keyed by the ids of WANTED objects and nothing
	# wanted names this wrapper any more.
	details=$(jq_of "$out" '.agent_reply.details // ""')
	case "$details" in
	*"$pinned"*) ;;
	*) die "s5: the reply does not name the pinned wrapper: '$details'" ;;
	esac
	assert_cn_info_ok "$out" "s5 cn1 base state while pinned"
	# Recomputed, never stored (cnagent.md CN30): a read-only path that issued no
	# syncup at all reaches the same verdict, because the verdict IS an
	# enumeration of the node and not a flag the syncup left behind. The reply
	# still carries CnInfo, because code 4 is not a refusal.
	out=$(cnctl 1 get-cn-info --expect-code 4)
	assert_eq "$(jq_of "$out" '.agent_reply.code // 0')" 4 \
		"s5: get-cn-info must report the leftover too"
	assert_cn_info_ok "$out" "s5 cn1 get-cn-info while pinned"
	# The stop rule of cnagent.md CN21 on hardware: a layer that leaves
	# something behind stops the
	# descent, and the leg wrappers are the LAST layer — so everything above
	# this wrapper is already gone and the wrapper is all that is left. Its own
	# leg's disconnect was set going in that same layer: the connection and
	# the wrapper are two different objects, and only one of them is stuck. An
	# assert_eq and not a grep, because "exactly this and nothing else" is the
	# assertion.
	got=$(helper 1 "cn_residue $(hex16 "${CNID[1]}")")
	assert_eq "$got" "$pinned" \
		"s5: the residue while pinned must be exactly the pinned wrapper"
	assert_eq "$(helper 1 "unpin_dev $pinned")" unpinned "s5: the unpin failed"
	# The retry is the same request at the same revision — the agent kept no
	# note of the failure, so there is nothing else it could be, and this is
	# exactly what the worker sends every round while the code is non-zero.
	cn_drop_until_clean 1 30
	cn_drop 2
	dn_drop 1
	dn_drop 2
	teardown_assert_clean "$sp" "s5_pinned_wrapper"
}

case_teardown() {
	CASE=teardown
	DIAG_CNTLRS=()
	SIDE_PROVISIONED=()
	s1_sides_gone
	s2_paths_long_dead
	s3_io_in_flight
	s4_partitioned_dn
	s5_pinned_wrapper
}

# ---------------------------------------------------------------------------
# Case B — thinbm (cnagent_integtest.md, Cases): snapshots and bitmap reads
# ---------------------------------------------------------------------------
#
# The td is 64 MiB of 1 MiB blocks, so every bitmap is exactly 64 bits, and the
# wire inversion of architecture.md, raid0 bitmap math, makes a written block a
# 0 bit. Blocks {0,5,6,7} written
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

	stage tdbm "GetThinDeviceBm: 1 = unmapped, LSB-first (architecture.md, raid0 bitmap math)"
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
	# (architecture.md, Thin devices), so this is the only td_list a real worker
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
	orithin=$(cn_dm_name c3 "$cn" "$sp" "$S_TD" "$S_SLICE")
	pool=$(cn_dm_name c2 "$cn" "$sp" "$S_SLICE")
	assert_before "$seq" "^dmsetup suspend $orithin\$" \
		"^dmsetup message $pool 0 create_snap 2 1\$" \
		"thinbm: the origin is suspended across create_snap"
	assert_before "$seq" "^dmsetup message $pool 0 create_snap 2 1\$" \
		"^dmsetup resume $orithin\$" \
		"thinbm: the origin resumes right after create_snap"
	# CN14's quiesce: the per-slice suspend above stays, nested inside one
	# quiesce of the origin td's raid0 that spans every slice's message. This
	# SP has one slice, so the log is the only on-hardware evidence of the
	# bracket; the cross-slice property (cnagent.md CN14) is left to a unit test.
	oriraid0=$(cn_dm_name c4 "$cn" "$sp" "$S_TD")
	snapthin=$(cn_dm_name c3 "$cn" "$sp" "$B_TD2" "$S_SLICE")
	assert_before "$seq" "^dmsetup suspend $oriraid0\$" \
		"^dmsetup message $pool 0 create_snap 2 1\$" \
		"thinbm: the origin raid0 is quiesced across create_snap (CN14)"
	assert_before "$seq" "^dmsetup message $pool 0 create_snap 2 1\$" \
		"^dmsetup resume $oriraid0\$" \
		"thinbm: the origin raid0 resumes after the messages (CN14)"
	assert_before "$seq" "^dmsetup resume $oriraid0\$" \
		"^dmsetup create $snapthin( |\$)" \
		"thinbm: the snapshot thin device is created after the resume (CN14)"

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
	# The rebuild's first half (cnagent_integtest.md, Cases, thinbm;
	# cnagent.md CN14, CN21). The teardown removes every dm
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
	# The rebuild's second half (cnagent_integtest.md, Cases, thinbm;
	# cnagent.md CN14, CN21): the desired state the
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
	# cnagent_integtest.md, Cases, thinbm, and cnagent.md CN14 (the activation
	# sweep): the rebuild re-creates the pool
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
# Case C — clone_xfer (cnagent_integtest.md, Cases): the live move
# (architecture.md, Transfer + clone = cross-SP live migration) and the
# recovery (architecture.md, Clone crash recovery)
# ---------------------------------------------------------------------------
#
# 32 of the td's 64 MiB are written, so the pushed bitmap is 00000000ffffffff:
# bits 32..63 = 1 = skippable ⇒ exactly one 32 MiB blkdiscard at offset 32 MiB
# **on the clone device**. The same trace also carries an
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
	local idx bmargs bmfiles nsdev2 err1 err2 hyd dump at upto from rsv rel deadline
	local code details
	diag_cntlr 1 "$sp1" "$cntlr"
	diag_cntlr 2 "$sp2" "$cntlr"
	dev=$(host_dev "$uuid")
	xnqn=$(xfer_nqn "$CLUSTER" "$sp1" "$C_XFER")
	clonedm=$(cn_dm_name c7 2 "$sp2" "$C_CLONE")
	metadm=$(clone_meta_dm 2 "$sp2" "$C_CLONE")
	nsdev1=$(cn_dm_name c6 1 "$sp1" "$S_NS")
	nsdev2=$(cn_dm_name c6 2 "$sp2" "$S_NS")
	# The two tds' permanent dm-errors (kind c5): what an effectively suspended
	# namespace's ns-dev is parked on (CN16 rule 1; architecture.md,
	# Namespace suspend semantics).
	err1=$(cn_dm_name c5 1 "$sp1" "$S_TD")
	err2=$(cn_dm_name c5 2 "$sp2" "$S_TD")
	# The two chunk files this case creates, as clone_bm_files sorts them:
	# LocalCloneBmPath ends in {src_slice_idx:%02x}-{bm_idx:%02x}, so the
	# pair (0, 0) and the pair (0, 1) differ only in the last segment.
	bmargs="$(hex16 "$CLUSTER") $(hex16 "${CNID[2]}") $(hex16 "$sp2") $(hex16 "$C_CLONE")"
	bmfiles=$(printf 'clone-bm-%s-00-00\nclone-bm-%s-00-01' \
		"${bmargs// /-}" "${bmargs// /-}")

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
	# The production bitmap source (architecture.md, Bitmap reads): the suite
	# pushes exactly what it
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
	# The positive control first, on the still-serving sp1 ns-dev: without it a
	# globally broken `open_rc` would make every assert_opens_eio below pass
	# vacuously, since its whole test is "non-zero and not a timeout".
	assert_opens_ok 1 "$nsdev1" "clone_xfer: the serving origin reads"
	# `suspended = true` on sp2's namespace means **parked**, not
	# dm-suspended: the ns-dev is live over the td's dm-error and the host
	# queues against the ANA state above (architecture.md,
	# Namespace suspend semantics, [D12]).
	assert_parked 2 "$nsdev2" "$err2" "clone_xfer sp2 stored-suspended ns-dev"
	assert_opens_eio 2 "$nsdev2" "clone_xfer sp2 stored-suspended ns-dev"

	stage xfer "stage 2: the transfer retires the origin (host IO quiesced)"
	req_set "$req1" ".xfer_list = [$(req_xfer "$C_XFER" "$nqn" 1 \
		"[\"$(cn_host_nqn "$CLUSTER" "${CNID[2]}")\"]" true)]"
	bump_cn_rev 1
	rev1=${CNREV[1]}
	out=$(cn_syncup_cntlr 1 "$req1")
	assert_map_ok "$out" xfer_id_to_dm_linear "$C_XFER" clone_xfer
	assert_map_ok "$out" xfer_id_to_subsystem "$C_XFER" clone_xfer
	assert_map_ok "$out" xfer_id_to_namespace "$C_XFER" clone_xfer
	# The origin is effectively suspended (CN16), which is a **park**: the
	# ns-dev is live on the td's dm-error, and a block-device walker that
	# opens it gets EIO at once instead of wedging in D state ([D12]).
	assert_parked 1 "$nsdev1" "$err1" "clone_xfer: the transfer origin"
	assert_opens_eio 1 "$nsdev1" "clone_xfer: the transfer origin"
	# The ordering rule of architecture.md, Namespace suspend semantics, on
	# hardware: the ANA move to `inaccessible` is
	# written before the ns-dev is touched, which is why the park needs no
	# grace window — nvmet refuses IO to an inaccessible namespace at the
	# target, so nothing of the host's is in flight when the reload lands.
	seq=$(helper 1 "cn_events $TRACE")
	# The path is anchored on the ORIGIN's namespace: this same converge also
	# creates the transfer's own nvmet namespace, which is born inaccessible and
	# writes `ana_grpid 3` too, so an unanchored regex would be satisfied by the
	# wrong write and the ordering rule would go unpinned.
	assert_before "$seq" \
		"^write .*/subsystems/$nqn/namespaces/1/ana_grpid 3\$" \
		"^dmsetup reload $nsdev1 " \
		"clone_xfer: ANA inaccessible before the origin is parked"
	# And the whole point, node-wide: the transfer is running and NOTHING on
	# either CN is dm-suspended. This is the state a block-device scanner used
	# to wedge on for the length of the hydration.
	assert_no_suspended_dm 1 "clone_xfer: mid-transfer"
	assert_no_suspended_dm 2 "clone_xfer: mid-transfer"
	host_wait_ana "$hv" "$nqn" 1 inaccessible 30
	assert_eq "$(helper 1 "port_linked '$xnqn'")" linked \
		"clone_xfer: the xfer subsystem is on the port"
	assert_eq "$(helper 1 "allowed_hosts '$xnqn'")" \
		"$(cn_host_nqn "$CLUSTER" "${CNID[2]}")" \
		"clone_xfer: the xfer allows exactly CN2"

	stage gate "stage 3: the clone is declared gated, then the chunk is pushed"
	req_set "$req2" ".sp_level = \"SP_LEVEL_NO_CLONE\"
		| .clone_list = [$(req_clone "$C_CLONE" "$xnqn" 1 "$S_TD" true)]"
	bump_cn_rev 2
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_suppressed "$out" \
		".cntlr_info.clone_id_to_dm_clone[\"$(d16 "$C_CLONE")\"]" \
		"clone_xfer gated clone"
	seq=$(helper 2 "cn_events $TRACE")
	assert_eq "$(event_cnt "$seq" "^nvme connect .*$NQN_PREFIX:4:")" 0 \
		"clone_xfer: the gate holds the source connection back (CN19)"
	cnctl 2 push-clone-bm --sp "$sp2" --cntlr "$cntlr" \
		--clone "$C_CLONE" --src-slice-idx 0 --bm-idx 0 \
		--bitmap-hex 00000000ffffffff >/dev/null
	# A second chunk of the SAME source slice at a non-zero bm_idx. It is the
	# suite's proof that bm_idx addresses a chunk within one slice's bitmap
	# and no longer names the slice itself: src_slice_cnt is 1 here, so the
	# single-index gate would have refused this push as an unknown source
	# slice (code 2, a hard failure of the call below). Chunk (0, 1) covers
	# bits 8*CloneBmChunkBytes upward — far past this clone's 64 regions — so
	# it is stored, reported and reloaded, and changes no blkdiscard anywhere
	# in the case; the stage 4 arithmetic below stays exactly as it was.
	cnctl 2 push-clone-bm --sp "$sp2" --cntlr "$cntlr" \
		--clone "$C_CLONE" --src-slice-idx 0 --bm-idx 1 \
		--bitmap-hex ff >/dev/null
	# An equal-revision re-send is a legal full re-apply; here it is only a
	# way to read the applied set back out of the reply (cnagent_integtest.md,
	# Conventions, Revisions).
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_eq "$(jq_of "$out" '(.bm_info_list // []) | length')" 1 \
		"clone_xfer bm_info_list length"
	assert_eq "$(jq_of "$out" '.bm_info_list[0].res_id')" "$(d16 "$C_CLONE")" \
		"clone_xfer bm_info_list res_id"
	# A clone reports its applied set as chunk_id_list, ascending by the pair.
	# protojson omits a field at its zero value, so the entry for (0, 0) is
	# the empty object {} — hence the // 0 defaults
	# (cnagent_integtest.md, The driver: `cnagentctl`).
	assert_eq "$(jq_of "$out" \
		'[.bm_info_list[0].chunk_id_list[]?
		  | "\(.src_slice_idx // 0):\(.bm_idx // 0)"] | join(",")')" \
		'0:0,0:1' "clone_xfer bm_info_list chunk_id_list"
	# bm_idx_list is the migration applied set; a clone never fills it.
	assert_eq "$(jq_of "$out" '.bm_info_list[0].bm_idx_list // "unset"')" \
		unset "clone_xfer bm_info_list bm_idx_list is unset for a clone"
	# The chunk file names carry the pair as two %02x segments
	# (cnagent_integtest.md, Cases, clone_xfer; architecture.md,
	# Agent local-store paths).
	assert_eq "$(helper 2 "clone_bm_files $bmargs")" "$bmfiles" \
		"clone_xfer: both chunk files are pair-named"

	stage enable "stage 4: one converge builds the clone and flips the host"
	req_set "$req2" '.sp_level = "SP_LEVEL_READWRITE"'
	bump_cn_rev 2
	rev2=${CNREV[2]}
	out=$(cn_syncup_cntlr 2 "$req2")
	assert_map_ok "$out" clone_id_to_target "$C_CLONE" clone_xfer
	assert_map_ok "$out" clone_id_to_dm_clone "$C_CLONE" clone_xfer
	assert_map_ok "$out" clone_id_to_meta "$C_CLONE" clone_xfer
	seq=$(helper 2 "cn_events $TRACE")
	# Two different blkdiscards share the verb in one trace,
	# so every anchor below is scoped by target device: the allocator's
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
	# The wrapper's table IS the allocation record (CN18):
	# a single linear over the arena loop device, whose offset and length are
	# the units the allocator handed out.
	assert_eq "$(helper 2 "dm_state $metadm")" live \
		"clone_xfer: the clone-meta wrapper is live"
	got=$(helper 2 "dm_table $metadm")
	printf '%s\n' "$got" | grep -qE '^0 [0-9]+ linear [0-9]+:[0-9]+ [0-9]+$' ||
		die "clone_xfer: the clone-meta wrapper is not a single linear: $got"
	# CN18 step 3: every dnv dm-clone carries no_discard_passdown, without exception —
	# it is what keeps the hydration blkdiscard above metadata-only instead of
	# also erasing the destination.
	#
	# The whole feature list is pinned, not just the one word: the exact
	# `2 no_hydration no_discard_passdown` agent.CloneTable emits from its
	# derived feature count (agent/dm.go:405-419), the same string CN18 step 3
	# and architecture.md, [D7], name. The
	# `dmsetup message $clonedm 0 enable_hydration`
	# asserted a few lines above does NOT weaken it — dm-clone's
	# STATUSTYPE_TABLE reprints the constructor args saved by copy_ctr_args
	# verbatim (drivers/md/dm-clone-target.c), and only `dmsetup status`
	# (STATUSTYPE_INFO) recomputes the live flags. The grep is scoped to this
	# clone's device by name, so nothing else on the node can satisfy it. A
	# substring test for no_discard_passdown alone accepts
	# `1 no_discard_passdown`, i.e. a clone created with hydration already
	# enabled, which would copy the very regions the pushed chunk
	# (architecture.md, Bitmap push protocol) asked to skip.
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

	stage wipe "stage 6: CN2 loses its kernel state and rebuilds (architecture.md, Clone crash recovery)"
	# The wipe-time sample (cnagent_integtest.md, Cases, clone_xfer and
	# What a pass means): the recovery contract of architecture.md,
	# Clone crash recovery, covers both a clone
	# that was still hydrating when its CN lost its kernel state and one that
	# had already finished, and which of the two this run wiped decides what
	# the rebuilt clone has left to copy. It is recorded, never asserted — the
	# race is the loop device's speed — and the sample is taken as late as
	# possible, immediately before the kill. A missing or unparseable status is
	# logged too rather than failing the suite; the one thing below that reads
	# it, the destination's offset-0 discard, is then simply not required.
	sample=$(cnctl 2 wait-hydrated --sp "$sp2" --cntlr "$cntlr" \
		--clone "$C_CLONE" --sample-only) || sample=""
	hyd=""
	case "$sample" in
	[0-9]*/[0-9]*)
		hyd=${sample%%/*}
		if [ "${sample%%/*}" -lt "${sample##*/}" ]; then
			log "clone_xfer: hydration was still running at wipe time ($sample)"
		else
			log "clone_xfer: hydration had already finished at wipe time ($sample)"
		fi
		;;
	*)
		log "clone_xfer: WARNING no hydration sample before the wipe"
		;;
	esac
	assert_eq "$(helper 2 "kill_role cn")" stopped "clone_xfer: cn2 stopped"
	sshv 2 "mv $CN_LOG $WORK/cn-agent.pre-wipe.log"
	helper 2 "wipe_cn $(hex16 "${CNID[2]}")"
	start_cn_agent 2
	# The listener opens only after the reconcile returns, so the wait-up
	# budget is the recovery's: a rebuilt clone re-connects, re-reads the
	# destination bitmaps and re-applies the chunk before it answers.
	out=$(cnctl 2 get-cn-size --wait 120)
	assert_eq "$(jq_of "$out" .size)" "$CN_CAPACITY" "clone_xfer cn2 size"
	# The restarted agent's probers start over, and cn2 is sp2's primary.
	wait_legs_probed 2 "$sp2" "$cntlr" 20 "clone_xfer post-wipe"
	out=$(cnctl 2 get-cntlr-info --sp "$sp2" --cntlr "$cntlr")
	assert_all_ok "$out" .cntlr_info "clone_xfer post-wipe cntlr"
	# The reconcile mints its own trace id, so the freshly rotated log is the
	# filter: it holds the recovery and nothing else.
	seq=$(helper 2 cn_events)
	# The destination-bitmap read is anchored on the LAST thin_dump, which must
	# follow the dm-clone's create: the recovery reads the destination only
	# once the clone exists (created with hydration off; architecture.md,
	# Clone crash recovery), while the
	# re-created pool's CN14 activation sweep dumps the same metadata through
	# the same reserve → thin_dump → release helper before any clone does. A
	# startup reconcile arms that sweep and skips it, so today the fresh log
	# holds one triplet; a first-match anchor would take the sweep's the day
	# it held two, and pass with no destination read at all.
	dump=$(event_last "$seq" '^thin_dump ')
	[ -n "$dump" ] || die "clone_xfer recovery: no thin_dump in the fresh log"
	at=$(event_line "$seq" "^dmsetup create $clonedm( |\$)")
	[ -n "$at" ] && [ "$at" -lt "$dump" ] ||
		die "clone_xfer recovery: the last thin_dump (event $dump) does not" \
			"follow the dm-clone's create (event ${at:-none}), so no destination" \
			"bitmap was read for it"
	# sed and tail both read to the end: a `head` here could close the pipe
	# on printf, and under pipefail that SIGPIPE would end the run.
	upto=$(printf '%s\n' "$seq" | sed -n "1,${dump}p")
	from=$(printf '%s\n' "$seq" | tail -n "+$dump")
	rsv=$(event_last "$upto" '^dmsetup message .* 0 reserve_metadata_snap$')
	rel=$(event_last "$upto" '^dmsetup message .* 0 release_metadata_snap$')
	[ -n "$rsv" ] && [ "${rel:-0}" -lt "$rsv" ] ||
		die "clone_xfer recovery: the last thin_dump (event $dump) ran with no" \
			"metadata snapshot reserved (reserve ${rsv:-none}, release ${rel:-none})"
	assert_before "$from" '^thin_dump ' \
		'^dmsetup message .* 0 release_metadata_snap$' \
		"clone_xfer recovery: the snapshot is always released"
	assert_before "$from" '^thin_dump ' \
		"^dmsetup message $clonedm 0 enable_hydration\$" \
		"clone_xfer recovery: the destination is read before hydration"
	# Scoped to the clone device, because the rebuild re-allocates an arena
	# unit and so emits its own earlier blkdiscard on the loop device.
	assert_before "$seq" "^blkdiscard .*/dev/mapper/$clonedm\$" \
		"^dmsetup message $clonedm 0 enable_hydration\$" \
		"clone_xfer recovery: every bitmap lands before hydration"
	# The destination's own contribution. Hydration copies regions upward from
	# 0 and the pushed chunk skips 32..63, so a wipe-time sample above 32 means
	# region 0 was already copied onto the destination: the dump maps it, and
	# the recovery must discard from offset 0 on the clone — the re-applied
	# source chunk never discards below 32 MiB, so it cannot stand in for it.
	# At 32 or below, or with no sample, nothing needs to have been copied.
	# Its length is left open: a clone still hydrating at the sample can go on
	# copying until the wipe removes it, so the sample bounds the length only
	# from below.
	if [ -n "$hyd" ] && [ "$hyd" -gt 32 ]; then
		assert_before "$from" '^thin_dump ' \
			"^blkdiscard --offset 0 --length [0-9]+ /dev/mapper/$clonedm\$" \
			"clone_xfer recovery: $hyd hydrated, the destination discards from 0"
		assert_before "$from" \
			"^blkdiscard --offset 0 --length [0-9]+ /dev/mapper/$clonedm\$" \
			"^dmsetup message $clonedm 0 enable_hydration\$" \
			"clone_xfer recovery: the destination bitmap lands before hydration"
	else
		log "clone_xfer: no region copied at the wipe (${sample:-no sample});" \
			"the destination's offset-0 discard is not required"
	fi
	# The registry is the dm table set, so a rebuilt CN reconstructs exactly
	# one allocation from an arena that was wiped along with the kernel state.
	assert_eq "$(helper 2 "clone_meta_wrappers $(hex16 "${CNID[2]}")")" \
		"$metadm" "clone_xfer recovery: exactly one kind-cb wrapper"
	out=$(cn_syncup_cntlr 2 "$req2")
	# The store was untouched by the wipe, so the reconcile rebuilt the
	# applied set from the files — both pairs, decoded out of the persisted
	# requests rather than parsed out of the names.
	assert_eq "$(jq_of "$out" \
		'[.bm_info_list[0].chunk_id_list[]?
		  | "\(.src_slice_idx // 0):\(.bm_idx // 0)"] | join(",")')" \
		'0:0,0:1' "clone_xfer: the chunk files survived the wipe"
	assert_eq "$(helper 2 "clone_bm_files $bmargs")" "$bmfiles" \
		"clone_xfer: both pair-named chunk files survived the wipe"
	# The wipe killed the host's sp2 controller with DNR; it never reconnects
	# on its own, and -n would take the live sp1 path with it
	# (cnagent_integtest.md, Cases, clone_xfer).
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
	# "Retired" is now a parked device, not a suspended one: the transfer is
	# gone and the stored `suspended` flag carries the state on (architecture.md,
	# Namespace suspend semantics).
	assert_parked 1 "$nsdev1" "$err1" "clone_xfer: the retired source"
	assert_opens_eio 1 "$nsdev1" "clone_xfer: the retired source"
	req_set "$req2" ".clone_list = []
		| .nqn_to_subsystem[\"$nqn\"].ns_list[0].suspended = false"
	bump_cn_rev 2
	rev2=${CNREV[2]}
	# The source's disconnect runs off the sweep's locks (CN21), so a pass
	# that still finds a controller of the :4: connection names it as a
	# leftover (code 4), and the worker's re-send of the same request is clean
	# once the disconnect has returned. It may find none: CN1's pass above
	# unlinked the :4: subsystem from a port that keeps listening — it still
	# carries sp1's subsystem — so cn2's controller lost its connection there,
	# and its first reconnect, after the kernel's default 10 s reconnect delay,
	# is refused with DNR and deletes the controller (cnagent_integtest.md,
	# Lab facts), about 11 s
	# after that unlink. A pass that lands later finds no controller, issues
	# no disconnect and replies 0. Both endings are CN21's, and the stage
	# takes whichever the timing gives it: --expect-code 4 names the usual
	# one, and `|| true` keeps the printed reply of the other.
	out=$(cn_syncup_cntlr 2 "$req2" --expect-code 4 || true)
	[ -n "$out" ] ||
		die "clone_xfer: the finalize printed no reply — the call never reached the agent"
	code=$(jq_of "$out" '.agent_reply.code // 0')
	details=$(jq_of "$out" '.agent_reply.details // ""')
	assert_map_ok "$out" td_id_to_raid0 "$S_TD" "clone_xfer finalized"
	assert_map_ok "$out" ns_id_to_dm_linear "$S_NS" "clone_xfer finalized"
	case "$code" in
	4)
		case "$details" in
		*"$NQN_PREFIX:4:"*) ;;
		*) die "clone_xfer: the finalize leftover is not the source connection: $details" ;;
		esac
		deadline=$((SECONDS + 30))
		until out=$(cn_syncup_cntlr 2 "$req2" --expect-code 0 2>/dev/null); do
			[ "$SECONDS" -lt "$deadline" ] ||
				die "clone_xfer: the source connection outlived 30 s"
			sleep 2
		done
		;;
	0)
		log "clone_xfer: cn2's :4: controller was already gone — CN1's unlink" \
			"dropped it and its reconnect was refused with DNR — so the" \
			"finalize had no disconnect to issue"
		;;
	*) die "clone_xfer: the finalize replied code $code ($details), want 0 or 4" ;;
	esac
	assert_eq "$(helper 2 "ctrl_of '$xnqn' '${IP[1]}'")" none \
		"clone_xfer: cn2 holds no controller of the source connection"
	seq=$(helper 2 "cn_events $TRACE")
	assert_before "$seq" "^dmsetup reload $nsdev2 " "^dmsetup remove $clonedm\$" \
		"clone_xfer: the ns-dev leaves the clone before the clone goes"
	# CN18 teardown order: the dm-clone goes first,
	# then its metadata wrapper — whose removal is what frees the arena units
	# again — and only then the source connection, when there was one left to
	# disconnect.
	assert_before "$seq" "^dmsetup remove $clonedm\$" \
		"^dmsetup remove $metadm\$" \
		"clone_xfer: the metadata wrapper goes after the dm-clone"
	if [ "$code" = 4 ]; then
		assert_before "$seq" "^dmsetup remove $metadm\$" \
			"^nvme disconnect .*$NQN_PREFIX:4:" \
			"clone_xfer: the source connection dies last"
	else
		assert_absent "$seq" "^nvme disconnect .*$NQN_PREFIX:4:" \
			"clone_xfer: no disconnect of a source connection that was already gone"
	fi
	# CN16 rule 6: with the clone gone the ns-dev sits on the raid0 again.
	assert_eq "$(helper 2 "dm_backing $nsdev2")" \
		"$(helper 2 "dm_devno $(cn_dm_name c4 2 "$sp2" "$S_TD")")" \
		"clone_xfer: the ns-dev is back on the raid0"
	assert_eq "$(helper 2 "dm_state $clonedm")" missing "clone_xfer dm-clone gone"
	got=$(helper 2 "clone_meta_wrappers $(hex16 "${CNID[2]}")")
	[ -z "$got" ] ||
		die "clone_xfer: the arena still holds kind-cb wrappers: $got"

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
	# The closing base-state probe (cnagent_integtest.md, Cases, clone_xfer),
	# case S's teardown probe run on both
	# CNs: with its cntlr gone each CN must hold no dm device and no
	# host-facing subsystem of its own, and the arena must be empty again —
	# the kind-cb tables *are* the allocation registry (CN18), so
	# reading them by name is what reports a leaked clone unit as an arena
	# leak instead of as one more anonymous dm device. The four base resources
	# (architecture.md, Controller node, common) must still probe OK afterwards,
	# because a teardown that took
	# the port, the tmpfs or the loop arena with it would satisfy every
	# residue check above and still leave the CN unable to serve the next SP.
	for idx in 1 2; do
		got=$(helper "$idx" "cn_residue $(hex16 "${CNID[$idx]}")")
		[ -z "$got" ] || die "clone_xfer: vm$idx still holds cn objects: $got"
		got=$(helper "$idx" "clone_meta_wrappers $(hex16 "${CNID[$idx]}")")
		[ -z "$got" ] ||
			die "clone_xfer: vm$idx's arena still holds wrappers: $got"
		out=$(cnctl "$idx" get-cn-info)
		assert_cn_info_ok "$out" "clone_xfer cn$idx base state after teardown"
	done
	assert_no_residue "$sp1"
	assert_no_residue "$sp2"
	sshv_ok "$hv" "rm -f $WORK/pattern-c.bin $WORK/probe-c.bin"
}

# ---------------------------------------------------------------------------
# Case D — restart (cnagent_integtest.md, Cases)
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
	# The post-restart snapshot is taken once cn1's legs have been probed
	# again, so this one must be too: a leg reads RES_STATUS_PENDING until
	# its prober's first round (CN11), and the build was seconds ago.
	wait_legs_probed 1 "$sp" "$c1" 20 "restart pre-snapshot"
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
	# trackers are in-memory, so it is the one field left out. The leg rows
	# are compared whole, status and details, which is the stage's point: the
	# restarted primary's probers start over and read RES_STATUS_PENDING
	# "health probe pending" until their first post-restart round, so the
	# snapshot waits that out first; after it a primary's OK row reads "" and
	# a standby's the sysfs transport report of kernel state the restart
	# leaves alone. (Until 2026-09-26 the leg details were left out too, for
	# the restarted primary's old OK "health probe pending" row.)
	wait_legs_probed 1 "$sp" "$c1" 20 "restart post-snapshot"
	local norm='walk(if type == "object" and has("epoch") then del(.epoch) else . end)'
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
	# An active array is recognized, not re-assembled. It is read from
	# /sys/block (CN12, 2026-09-26), so a `--detail` here is as wrong as a
	# mutation and is no longer excluded.
	for idx in 1 2; do
		got=$(helper "$idx" cn_events | grep -E '^mdadm ' |
			grep -Ev -- '--examine' || true)
		[ -z "$got" ] ||
			die "restart: cn$idx ran an mdadm other than --examine:"$'\n'"$got"
	done

	stage idempotent "same-revision re-applies must mutate nothing"
	# SyncupCn and SyncupCntlr share one counter but store their revisions
	# separately (cnagent_integtest.md, Conventions, Revisions), so the
	# equal-revision re-send of each is the revision
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
           [--wipe] <user@vm1> <user@vm2>

cases: ${CASES[*]}

--wipe is the ONE-TIME lab wipe: it removes EVERY dnv object on both VMs,
including residue an older binary left under the pre-role-letter dm kind
spelling, then runs the ordinary cleanup. It runs no case. Never run it
while another suite is using these VMs.
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
		--wipe)
			WIPE=1
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

	if [ "$WIPE" -eq 1 ]; then
		ship_helper
		STAGE="lab wipe"
		log ""
		log "=== one-time lab wipe: EVERY dnv object on both VMs"
		wipe_all
		log ""
		log "=== post-wipe cleanup"
		cleanup_all
		return 0
	fi

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
