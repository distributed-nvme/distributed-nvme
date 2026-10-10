#!/usr/bin/env bash
#
# dnagent_test.sh — the `dnv-agent dn` integration test of
# doc/dnagent_integtest.md. Two real VMs, real dm/nvmet/nvme-tcp, driven
# over gRPC from this machine by integtest/dnagentctl.
#
#   bash integtest/dnagent_test.sh [--only <case>] [--cleanup-only] \
#       [--wipe] user1@ip1 user2@ip2
#
# Cases: smoke, zeroing, sides, migr_full, migr_bitmap, teardown, restart
# (dnagent_integtest.md, Cases; for `teardown`, also architecture.md,
# Teardown by sweep). Cleanup runs
# unconditionally at the start and, on success only, at the end: a failing run
# leaves every dm/nvmet object and both agent logs in place and dumps
# diagnostics (dnagent_integtest.md, Teardown and cleanup).
#
# The uutils dd rule (dnagent_integtest.md, Assumptions and preflight checks,
# the first lab fact) is absolute: this script never passes iflag= or
# oflag= to dd. Writes use conv=fsync, reads that must hit the media are
# preceded by a cache drop. Do not "fix" this back to direct IO.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (dnagent_integtest.md, Topology)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CTL="$REPO_ROOT/integtest/bin/dnagentctl"
AGENT_BIN="$REPO_ROOT/bin/dnv-agent"

WORK=/var/tmp/dnv-integtest
HELPER=/var/tmp/dnv-integtest-helper.sh
NVMET=/sys/kernel/config/nvmet

GRPC_PORT=29528
TR_SVC_ID=4200
CLUSTER=0x1
EXTENT_SIZE=67108864 # common.MinDnExtSize; the proto field is raw bytes
# GetDnSize reports the [D13] *data area*, not the raw device: the fixed
# DnDataOffset (256 MiB) prefix — header block, the two volume-table slots and
# the clone-metadata area — is already subtracted, so the CP does no further
# subtraction (architecture.md, Size → extents). 2 GiB backing file:
# 2147483648 - 268435456 = 1879048192.
DATA_SIZE=1879048192 # the exact GetDnSize the setup asserts
# Where the extent area starts on the disk (common.DnDataOffset): the 2 GiB
# backing file less DATA_SIZE, so the setup's GetDnSize assertion catches a
# drift before the zeroing case's fill could write into the header, the
# volume-table slots or the clone-metadata area in front of it.
DATA_OFFSET=$((2147483648 - DATA_SIZE))

# Migration knobs, mirroring the CP defaults.
BLOCK_SIZE=1048576
META_BLOCKS=3
HYDR_THRESHOLD=1
HYDR_BATCH=1

# The length a worker asks a data group's side to zero (side_conf.zero_bytes,
# architecture.md, Side provisioning protocol): its meta region and its first
# data block, in the pool's blocks (model.SideZeroBytes), which the knobs
# above make a RAID1 data group's at the default block size. Every side this
# suite builds serves a data group but the zeroing case's meta-group side, and
# every request for one side carries the same length: one that disagrees with
# the length its record holds is an error row (dnagent.md DN9).
DATA_ZERO_BYTES=$(((META_BLOCKS + 1) * BLOCK_SIZE))

# Per-RPC deadline for the converge RPCs (see ctl).
SYNCUP_TIMEOUT=60

# Polling budget of `dnagentctl wait-zeroed` (architecture.md,
# Side provisioning protocol). On a loop device the kernel maps
# REQ_OP_WRITE_ZEROES onto fallocate, so even the longest length this suite
# zeroes, the zeroing case's two-extent meta-group side, finishes in well
# under a second; the budget only has to cover a stalled retry loop
# (DnZeroRetryInterval = 5 s).
ZERO_TIMEOUT=120

NQN_PREFIX=nqn.2024-01.io.dnv

# Per-VM state, 1-indexed so "vm1"/"vm2" read directly.
VM=("" "" "")
IP=("" "" "")
DNID=("" 0x1 0x2)
REV=("" 0 0)
DNREV=("" 0 0)
LOOP=("" "" "")

# The two concurrent, opposite-direction migrations of cases B and C
# (dnagent_integtest.md, Cases).
# Migration m's primary CN is hosted on the VM opposite its source DN.
MLEG=("" 0x1 0x2)
MSRCDN=("" 1 2)
MSRCSIDE=("" 0x11 0x21)
MDSTDN=("" 2 1)
MDSTSIDE=("" 0x12 0x22)
MID=("" 0x31 0x32)
MCN=("" 0x23 0x24)
MCNVM=("" 2 1)

JQ=jq
ONLY=""
CLEANUP_ONLY=0
WIPE=0
CASE="setup"
TRACE="it-setup"
STAGE="(startup)"
SETUP_DONE=0

CASES=(smoke zeroing sides migr_full migr_bitmap teardown restart)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the stage's trace id
# (dnagent_integtest.md, The driver: `dnagentctl`): one id shared by the
# driver call, both agent handlers and every os command they run.
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

# assert_gated is assert_not_ok's strict twin (dnagent_integtest.md,
# Conventions, Negatives are exact).
# RES_STATUS_PROVISIONING is a *healthy* status, so it
# satisfies a bare assert_not_ok: every "this must not be built" check would
# silently start accepting a side that never provisioned. Where the expectation
# is "deliberately not created", name the two statuses that mean it — the field
# omitted entirely (ABSENT, which is what a converge that never reaches the
# resource produces) or MISSING — and reject OK, ERROR and PROVISIONING alike.
assert_gated() {
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	case "$got" in
	ABSENT | RES_STATUS_MISSING) ;;
	*) die "$3: status is $got, want ABSENT or RES_STATUS_MISSING" ;;
	esac
}

# assert_provisioning_or_ok accepts the two statuses a side may legally hold at
# provisioned = false (the converge matrix's zeroing and zeroed rows with a
# record present, dnagent.md DN9):
# PROVISIONING while the background goroutine still has bytes to zero, and OK
# once its zeroed count reaches the length the request asks for. Zeroing a few
# MiB, or at most 128 MiB, on a loop device is a `fallocate`, so which of the
# two a phase-1 reply carries is a genuine race — do not pick one.
assert_provisioning_or_ok() {
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	case "$got" in
	RES_STATUS_PROVISIONING | RES_STATUS_OK) ;;
	*) die "$3: status is $got, want PROVISIONING or OK" ;;
	esac
}

# assert_provisioning is the exact form, for the rows the matrix pins to
# PROVISIONING with no race: the resources a deferred side deliberately does
# not create.
assert_provisioning() {
	local got
	got=$(jq_of "$1" "$2 // \"ABSENT\"")
	[ "$got" = "RES_STATUS_PROVISIONING" ] ||
		die "$3: status is $got, want RES_STATUS_PROVISIONING"
}

# assert_cn_ok checks one entry of a map<uint64, ResInfo>; protojson renders
# the keys as decimal strings.
assert_cn_ok() {
	local json=$1 field=$2 cn=$3 label=$4 key
	key=$((cn))
	assert_ok "$json" ".side_info.$field[\"$key\"].status" "$label $field[$cn]"
}

assert_dn_info_ok() {
	local json=$1 label=$2 field
	for field in disk_info meta_info port_info; do
		assert_ok "$json" ".dn_info.$field.status" "$label"
	done
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
		log "########## diagnostics (dnagent_integtest.md, Teardown and cleanup) ##########"
		diagnostics || true
		log ""
		log "debris left in place on both VMs; failing stage '$STAGE'"
		log "pull records with: jq 'select(.trace_id==\"$TRACE\")' $WORK/agent.log"
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution
# ---------------------------------------------------------------------------

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# sshv runs one command as root on a VM and returns its stdout. Everything the
# agents touch (dm, configfs, nvme) needs root, so every remote command
# goes through sudo (dnagent_integtest.md, Topology).
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

# ctl drives one agent. Every call carries the current stage's trace id.
#
# The driver's default --timeout suits the read-only RPCs, but one
# converge pass runs dozens of OS commands, each with its own soft and hard
# budget (architecture.md, Common validation) — enabling a migration
# destination alone creates a clone-metadata wrapper, an nvme connection, a
# dm-clone and reloads the export stack, with the two concurrent migrations
# converging on both nodes at once. Syncups get a much larger budget; a
# caller's own --timeout still wins, since it lands later on the command line.
ctl() {
	local idx=$1
	shift
	local sub=$1
	shift
	local extra=()
	case "$sub" in
	syncup-dn | syncup-side) extra=(--timeout "$SYNCUP_TIMEOUT") ;;
	esac
	log "[dn$idx] dnagentctl $sub $*"
	"$CTL" "$sub" \
		--addr "${IP[$idx]}:$GRPC_PORT" \
		--cluster "$CLUSTER" --dn "${DNID[$idx]}" \
		--trace-id "$TRACE" "${extra[@]}" "$@"
}

# bump_rev advances a DN's monotonic revision counter (dnagent_integtest.md,
# Conventions, Revisions). It must run in
# the parent shell — never inside a background job or a command substitution,
# both of which would increment a copy — so the two concurrent migrations of
# cases B/C never race for a value. bump_dn_rev additionally records the
# revision SyncupDn will store, which is what CheckDn echoes back.
bump_rev() { REV[$1]=$((REV[$1] + 1)); }
bump_dn_rev() {
	bump_rev "$1"
	DNREV[$1]=${REV[$1]}
}

# ---------------------------------------------------------------------------
# Derived names (architecture.md, Naming) — the bash mirror of common/name_fmt.go
# ---------------------------------------------------------------------------

hex16() { printf '%016x' "$(($1))"; }

side_to_cn_nqn() { # cluster sp leg cn
	printf '%s:2:%s:%s:%s:%s' "$NQN_PREFIX" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

migr_src_nqn() { # cluster dn sp migr
	printf '%s:3:%s:%s:%s:%s' "$NQN_PREFIX" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

cn_host_nqn() { printf '%s:1:%s:%s' "$NQN_PREFIX" "$(hex16 "$1")" "$(hex16 "$2")"; }

dn_clone_name() { # cluster dn sp migr
	printf 'dnv-%s-%s-d3-%s-%s' \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

# dn_side_name is the [D13] side data device (dm kind d4): the one device
# the `teardown` case pins, and the one whose verified removal is what lets
# the side's allocation record be freed (DN6).
dn_side_name() { # cluster dn sp side
	printf 'dnv-%s-%s-d4-%s-%s' \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

# ns_by_id is the CN-side device node of a leg: the namespace identity is
# deterministic per leg, so both sides of a migrating leg land on one
# multipath device (dnagent_integtest.md, Topology).
ns_by_id() { "$CTL" ns-id --cluster "$CLUSTER" --sp "$1" --leg "$2" | "$JQ" -r .by_id; }

# ---------------------------------------------------------------------------
# CN emulation (dnagent_integtest.md, Topology): this suite exercises the DN
# contract without a CN, so CN identities are plain
# `nvme connect --hostnqn <CnHostNqn>` from the VMs.
# ---------------------------------------------------------------------------

# host_id mirrors common.NvmeHostId. Every emulated connect must pass it: the
# kernel allows exactly one hostnqn per hostid, and each VM plays two CN
# identities on top of its own dn agent's DnHostNqn, so leaving the node-wide
# /etc/nvme/hostid implicit makes the second identity fail EINVAL
# ("found same hostid ... but different hostnqn").
host_id() { "$CTL" host-id --hostnqn "$1"; }

cn_connect() { # cnvm targetdn sp leg cn
	local vm=$1 dn=$2 sp=$3 leg=$4 cn=$5 nqn host hid
	nqn=$(side_to_cn_nqn "$CLUSTER" "$sp" "$leg" "$cn")
	host=$(cn_host_nqn "$CLUSTER" "$cn")
	hid=$(host_id "$host")
	sshv "$vm" "nvme connect -t tcp -a ${IP[$dn]} -s $TR_SVC_ID -n '$nqn' --hostnqn '$host' --hostid '$hid'"
}

cn_disconnect() { # cnvm sp leg cn
	local vm=$1 nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	sshv_ok "$vm" "nvme disconnect -n '$nqn' || true"
}

# cn_path_field reads one field of the path a CN holds to a given DN.
cn_path_field() { # cnvm sp leg cn dn field
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "path_field '$nqn' '${IP[$5]}' '$6'"
}

cn_ana_state() { # cnvm sp leg cn dn
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "ana_state '$nqn' '${IP[$5]}'"
}

cn_wait_ana() { # cnvm sp leg cn dn want secs
	local nqn
	nqn=$(side_to_cn_nqn "$CLUSTER" "$2" "$3" "$4")
	helper "$1" "wait_ana '$nqn' '${IP[$5]}' '$6' '$7'" ||
		die "path to dn$5 of leg $3 never reached ANA state '$6'"
}

# ---------------------------------------------------------------------------
# Data IO on CNs (dnagent_integtest.md, Assumptions and preflight checks, the
# dd lab fact): writes fsync, reads follow a cache drop, never iflag=/oflag=.
# ---------------------------------------------------------------------------

drop_caches() { sshv "$1" "sync; echo 3 > /proc/sys/vm/drop_caches"; }

sha_range() { # vm path countMiB [skipMiB]
	sshv "$1" "dd if=$2 bs=1M count=$3 skip=${4:-0} status=none | sha256sum | cut -d' ' -f1"
}

write_range() { # vm src dst countMiB [seekMiB]
	sshv "$1" "dd if=$2 of=$3 bs=1M count=$4 seek=${5:-0} conv=fsync status=none; sync"
}

# ---------------------------------------------------------------------------
# The VM helper. Shipped to /var/tmp (outside $WORK, which cleanup removes) so
# the quoting of the probing/teardown loops lives in one readable place.
# ---------------------------------------------------------------------------

vm_helper_source() {
	cat <<'HELPER_EOF'
#!/usr/bin/env bash
# Shipped by integtest/dnagent_test.sh. The teardown functions are best-effort
# by design: cleanup must survive a crashed prior run. The absence helpers are
# the opposite (see residue): an assertion that wants their output empty must
# never read a tool that failed as a node that is clean.
WORK=/var/tmp/dnv-integtest
NVMET=/sys/kernel/config/nvmet
NQN_PREFIX=nqn.2024-01.io.dnv
# PIN_PREFIX is where pin_dev records its holders' pids: outside $WORK,
# which cleanup removes wholesale, so a pin always outlives what it pins.
PIN_PREFIX=/var/tmp/dnv-it-pin

# nvme_ctrl_present — does this node hold any nvme controller at all? A glob
# over the class directory cannot fail the way a tool can, which is why it is
# what subsys_json checks an empty listing against.
nvme_ctrl_present() {
	local c
	for c in /sys/class/nvme/nvme[0-9]*; do
		[ -e "$c" ] && return 0
	done
	return 1
}

# subsys_json — the bare `nvme list-subsys -o json`. It prints nothing at all
# on a node that holds no controller, and '[]' stands in for that shape. But
# host_subsys_present, ctrl_of and ana_state answer "absent" out of this
# output, so an empty listing, whatever its exit status, is taken for that
# shape only when sysfs agrees that there is no controller, and the output of
# a listing that failed is never used: either way the call fails instead.
subsys_json() {
	local json rc=0
	json=$(nvme list-subsys -o json 2>/dev/null) || rc=$?
	if [ -z "${json//[[:space:]]/}" ]; then
		if nvme_ctrl_present; then
			echo "subsys_json: nvme list-subsys printed nothing (rc $rc), but this node holds nvme controllers" >&2
			return 1
		fi
		json='[]'
	elif [ "$rc" -ne 0 ]; then
		echo "subsys_json: nvme list-subsys failed (rc $rc)" >&2
		return 1
	fi
	printf '%s' "$json"
}

# path_field <nqn> <traddr> <field> — one field of the path a host holds to
# one target address, e.g. State (live/connecting) or ANAState.
path_field() {
	local json
	json=$(subsys_json) || return 1
	printf '%s' "$json" | jq -r --arg nqn "$1" --arg a "$2" --arg f "$3" '
	    [ .. | objects | select(has("NQN") and .NQN == $nqn) | .Paths[]?
	      | select([.Address | split(",")[] | select(startswith("traddr="))]
	               == ["traddr=" + $a])
	      | .[$f] ] | first // "none"'
}

# ana_state <nqn> <traddr> — the ANA state of the path this host holds to one
# target address. nvme-cli 2.16's `list-subsys -o json` does not carry it
# (only `show-topology` does, keyed by namespace), so it is read straight off
# the per-path sysfs attribute of the controller list-subsys names. `none`
# means that there is no such path, or that the path has no readable
# ana_state, as between a connect and the namespace scan it queued. A
# listing that did not answer is neither, so the call fails instead (see
# residue).
ana_state() {
	local ctrl attr
	ctrl=$(path_field "$1" "$2" Name) || return 1
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
		[ -e "$1" ] || { echo gone; return 0; }
		sleep 0.5
	done
	echo "$1 still present after $2s" >&2
	return 1
}

# fill_extents <dev> <pattern> <fromMiB> <extMiB> <cnt> — the pattern file,
# <extMiB> long, into <cnt> consecutive extent slots from <fromMiB> on, each
# write synced (the zeroing case, dnagent_integtest.md, Cases). It writes the
# loop device, never the backing file, which a dd with seek= and no
# conv=notrunc would cut short at the end of the write, and it passes dd no
# input or output flag (dnagent_integtest.md, Assumptions and preflight
# checks).
fill_extents() {
	local i
	for ((i = 0; i < $5; i++)); do
		dd if="$2" of="$1" bs=1M seek=$(($3 + i * $4)) count="$4" \
			conv=fsync status=none || return 1
	done
	echo filled
}

# read_probe <dev> — prints ok or eio; the caller asserts which. A standby
# export is deliberately backed by dm-error, so its reads must fail.
read_probe() {
	if dd if="$1" of=/dev/null bs=1M count=1 status=none 2>/dev/null; then
		echo ok
	else
		echo eio
	fi
}

allowed_host_cnt() { ls "$NVMET/subsystems/$1/allowed_hosts" 2>/dev/null | wc -l; }

# subsys_present <nqn> — yes/no, so a "this export must NOT exist" assertion
# (the dst_provisioned = false equivalence proof, architecture.md, Migration)
# does not have to parse `residue`.
subsys_present() {
	if [ -d "$NVMET/subsystems/$1" ]; then echo yes; else echo no; fi
}

# host_subsys_present <nqn> — yes/no for the *host* half of the same question:
# does this node hold a controller for one subsystem NQN, at any address?
# subsys_present reads configfs, i.e. what this node exports; this reads the
# nvme driver's own view, i.e. what this node has connected to. It is how the
# "no connection" clause of the gated declaration (dnagent_integtest.md,
# Cases, migr_full and migr_bitmap) is proved on DNdst, where the gated reply
# carries no migr_dst_info at all and so cannot show it.
host_subsys_present() {
	local json n
	json=$(subsys_json) || return 1
	n=$(printf '%s' "$json" | jq -r --arg nqn "$1" '
	    [ .. | objects | select(has("NQN") and .NQN == $nqn) ] | length') ||
		return 1
	if [ "${n:-0}" -gt 0 ]; then echo yes; else echo no; fi
}

# residue <sp16> — everything still on this node for one storage pool; the
# teardown assertions require empty output.
# The dm-name pattern covers the side device too (dm kind d4).
#
# It is one of the absence helpers. residue, export_dms, dm_kind_names,
# fenced_linears, the three agent-log readers below, host_subsys_present and
# path_field over subsys_json, and ctrl_of and ana_state over path_field all
# feed assertions on what they print, most of them that it is EMPTY. So each
# one FAILS — an error on stderr and a non-zero status — when the tool it
# enumerates with did not answer. Printing nothing instead would read "the
# tool failed" as "nothing is there" and pass the assertion. Where the
# driver takes one sample, a failed one fails its stage; wait_ana, which
# polls ana_state, reads a failed sample as a state not reached yet, so a
# listing that never answers times the poll out. (disconnect_kind and diag
# read subsys_json too and go on past a failure: they are cleanup and
# diagnostics, best-effort by design.)
residue() {
	local dms subs
	dms=$(dmsetup ls) || return 1
	subs=$(ls "$NVMET/subsystems") || return 1
	printf '%s\n' "$dms" | awk '{print $1}' |
		grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-$1-" || true
	printf '%s\n' "$subs" | grep -E ":$1:" || true
}

# export_dms <sp16> <side16> — the per-CN *export stack* one side currently
# has on this node: the dm-error (kind d0, DnErrorName) and the dm-linear
# (kind d1, DnLinearName), which are the only two dm kinds that exist per CN.
# It is the kernel-side half of the provisioning gate of architecture.md,
# Side provisioning protocol — nothing is exported before the side's
# zeroing is complete — so it deliberately does NOT match kind d4
# (DnSideName): the side device is exactly what phase (a) is supposed to
# build, and `residue` would report it. See common/name_fmt.go for the kind
# constants (every dn kind is the role letter `d` and one hex digit)
# and DnErrorName/DnLinearName for the field order,
# dnv-<cluster16>-<dn16>-<kind>-<sp16>-<side16>-<cn16>.
#
# The scope is the side, not the storage pool: cases A and B/C provision the
# sides of one sp one after another, so a pool-wide pattern would match a
# sibling side's already-live export and fail a correct run.
export_dms() { # sp16 side16
	local names
	names=$(agent_dm_names) || return 1
	printf '%s\n' "$names" |
		awk -F- -v sp="$1" -v side="$2" \
			'($4 == "d0" || $4 == "d1") && $5 == sp && $6 == side'
}

# fenced_linears <sp16> — the per-CN dm-linears of one storage pool that are
# currently suspended, i.e. inside the cutover grace window (architecture.md,
# Migration, src side step 2). The attr
# column is four positions (live, inactive, suspended, ro/rw), so a suspended
# device matches ':.-s' — name, then '.', '-', 's'.
fenced_linears() {
	local rows
	rows=$(dmsetup info -c --noheadings -o name,attr) || return 1
	printf '%s\n' "$rows" |
		grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-d1-$1-.*:.-s" || true
}

# clone_table <clone dm name> — the live dm-clone table, so the mandatory feature pair
# can be asserted against a real kernel and not only in the unit tests.
clone_table() { dmsetup table "$1" 2>/dev/null || echo MISSING; }

# The agent-log readers — clone_discards, any_clone_discards and mutations —
# run each filter as ONE jq evaluation over the whole log (`jq -n` and
# `inputs`), not once per record. Run once per record, jq reports an error
# on one record and goes on to the next, and a C jq, which is what a
# distro's `jq` package installs, then exits with the status of the last
# record alone. So an error on any record but the last goes unnoticed: the
# call exits 0, whatever that record should have printed is simply missing,
# and an absence assertion reads that as "none". As one evaluation, an error
# on any record, or a line that does not parse, fails the whole call, on a
# C jq and on gojq alike.

# clone_discards <clone dm name> — the blkdiscard records the agent logged
# against one dm-clone device (the log check of migr_bitmap,
# dnagent_integtest.md, Cases).
clone_discards() {
	jq -rn --arg dev "/dev/mapper/$1" '
	    inputs
	    | select(.msg == "os command" and .cmd == "blkdiscard")
	    | select(.args | index($dev)) | (.args | join(" "))' \
		"$WORK/agent.log" || return 1
}

# any_clone_discards — every blkdiscard against any dm-clone device (the
# migr_full check of dnagent_integtest.md, Cases).
any_clone_discards() {
	local lines
	lines=$(jq -rn 'inputs
	       | select(.msg == "os command" and .cmd == "blkdiscard")
	       | (.args | join(" "))' "$WORK/agent.log") || return 1
	printf '%s\n' "$lines" |
		grep -E '/dev/mapper/dnv-[0-9a-f]{16}-[0-9a-f]{16}-d3-' || true
}

# mutations [logfile] — every mutating operation in an agent log (the restart
# case's mutation-free re-sends, dnagent_integtest.md, Cases).
# Probe operations (lsblk, dmsetup info/table/status, ls, and the [D13]
# "os read block") are expected and deliberately absent from the list, exactly
# mirroring the unit tests' readOnlyPrefixes.
#
# The command and its verb are bound to variables BEFORE the verb lists: a
# list piped into index() is the input its argument is evaluated against, so
# `["blkdiscard"] | index(.cmd)` would read `.cmd` off the list and raise an
# error on EVERY command record — jq reports each error and goes on to the
# next record, so behind a `2>/dev/null || true` the helper would print no
# command at all, and a C jq exits with the last record's status (see the
# note above clone_discards), so only a log that happened to end in a command
# record would fail the call. Run as one evaluation, the command pass below
# fails on the first such error.
mutations() {
	local log=${1:-$WORK/agent.log}
	jq -rn '
	    inputs
	    | select(.msg == "os write file direct")
	      | "write file direct " + .path' "$log" || return 1
	jq -rn '
	    inputs
	    | select(.msg == "os command")
	    | .cmd as $cmd | .args[0] as $verb
	    | select(
	        (["blkdiscard"] | index($cmd))
	        or ($cmd == "dmsetup" and (["create","reload","remove","suspend",
	              "resume","message"] | index($verb)))
	        or ($cmd == "nvme" and (["connect","disconnect"] | index($verb)))
	        or (["mkdir","rmdir","ln"] | index($cmd))
	        or ($cmd == "rm")
	      )
	    | .cmd + " " + (.args | join(" "))' "$log" || return 1
	jq -rn '
	    inputs
	    | select(.msg == "os write block") | "write block " + .path' \
		"$log" || return 1
}

# ctrl_of <nqn> <traddr> — the controller device backing one path, so a dead
# source path can be disconnected by device instead of by NQN (dnagent.md SH20;
# dnagent_integtest.md, Cases, migr_full and migr_bitmap).
ctrl_of() { path_field "$1" "$2" Name; }

# --- device pins (architecture.md, Teardown by sweep) -----------------------
#
# A pin is an open file descriptor on a dm device, held by a process OUTSIDE
# the agent. It is the one way this suite can make a `dmsetup remove` fail for
# a reason the agent cannot argue with — dm refuses to remove a device whose
# open count is non-zero — so it is how the `teardown` case proves that a
# removal the sweep could not finish is REPORTED (the leftover reply code,
# naming the device) instead of being forgotten, and that re-sending the same
# request finishes the job once the holder is gone.

# dm_open_cnt <dm name> — the device's open count, or -1 when `dmsetup info`
# did not answer at all, which for this helper's purposes means the device is
# gone. Both answers are needed: "pinned" is open > 0, while "released" is
# satisfied just as well by a device that has since been removed.
dm_open_cnt() {
	local out
	out=$(dmsetup info -c --noheadings -o open "$1" 2>/dev/null) || {
		printf '%s\n' -1
		return 0
	}
	out=${out//[[:space:]]/}
	[ -n "$out" ] || out=-1
	printf '%s\n' "$out"
}

# pin_dev <dm name> — hold one device open until unpin_dev or cleanup kills
# the holder. `setsid nohup` detaches it from the ssh session that started it,
# and the opener execs `sleep`, so what holds the device is a bare fd on a
# process with no other business — nothing the agent can ask to let go.
#
# BOTH stdout and stderr are redirected, and that is not tidiness: the driver
# reads a helper's output through a `$( )`, which waits until the last writer
# of the inherited descriptor closes it, so a background child that kept
# either one would hang the caller for the sleep's whole hour.
#
# The holder records its OWN pid rather than the `$!` of the pipeline that
# started it: `setsid` forks only when its caller is already a process group
# leader, so `$!` is the holder in a non-interactive shell and the wrapper
# that has already exited in an interactive one — and an unpin that killed
# whatever pid had been recycled into that number would be far worse than no
# pin at all.
#
# Both the pidfile and the open count are polled before returning, so a caller
# that gets a 0 back has a device that is provably held by a process this
# helper can provably find again. A stage that asserted a leftover against a
# device nothing actually held would fail with a message pointing at the
# agent.
pin_dev() {
	local name=$1 file pid i
	file=$PIN_PREFIX.$name.pid
	rm -f "$file"
	setsid nohup bash -c \
		"exec 3</dev/mapper/$name; echo \$\$ >$file; exec sleep 3600" \
		</dev/null >/dev/null 2>&1 &
	for ((i = 0; i < 40; i++)); do
		pid=$(cat "$file" 2>/dev/null)
		if [ -n "$pid" ] && [ "$(dm_open_cnt "$name")" -gt 0 ]; then
			echo "pinned $name (pid $pid)"
			return 0
		fi
		sleep 0.25
	done
	echo "pin of $name never took effect" >&2
	return 1
}

# unpin_dev <dm name> — kill the holder and wait for the fd to be closed. The
# close happens when the kernel reaps the process, so returning any earlier
# would hand the caller a device that is still busy for a moment.
unpin_dev() {
	local name=$1 pid i
	pid=$(cat "$PIN_PREFIX.$name.pid" 2>/dev/null)
	rm -f "$PIN_PREFIX.$name.pid"
	[ -n "$pid" ] && kill "$pid" >/dev/null 2>&1
	for ((i = 0; i < 40; i++)); do
		if [ "$(dm_open_cnt "$name")" -le 0 ]; then
			echo "unpinned $name"
			return 0
		fi
		sleep 0.25
	done
	echo "$name is still open after its pin was killed" >&2
	return 1
}

# kill_pins drops every pin on this node, whoever left it and whatever it
# names. cleanup runs it unconditionally, because a stage that failed between
# pin_dev and unpin_dev would otherwise leave an fd on a dm device that the
# next run cannot remove at all: `dmsetup remove --force` only swaps in an
# error table, the device itself stays until the last close.
kill_pins() {
	local file pid i
	for file in "$PIN_PREFIX".*.pid; do
		[ -e "$file" ] || continue
		pid=$(cat "$file" 2>/dev/null)
		rm -f "$file"
		[ -n "$pid" ] || continue
		kill -9 "$pid" >/dev/null 2>&1
		# The fd closes as the process dies, not as the signal is sent, and
		# everything after this in cleanup removes dm devices: a holder that
		# has not gone yet still holds its device open.
		for ((i = 0; i < 20; i++)); do
			kill -0 "$pid" >/dev/null 2>&1 || break
			sleep 0.25
		done
	done
	return 0
}

# --- teardown ---------------------------------------------------------------

# agent_dm_names and dm_kind_names fail when `dmsetup ls` does, like the
# absence helpers (see residue): the teardown case asserts on dm_kind_names
# being empty. cleanup only ever reads them inside a `$( )`, where a failure
# leaves nothing to remove and cleanup goes on.
agent_dm_names() {
	local out
	out=$(dmsetup ls) || return 1
	printf '%s\n' "$out" | awk '{print $1}' |
		grep -E '^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-' || true
}

dm_kind_names() {
	local names
	names=$(agent_dm_names) || return 1
	printf '%s\n' "$names" | awk -F- -v k="$1" '$4 == k'
}

# resume_suspended sweeps up suspended dm devices before anything reads them.
# A migration source holds its per-CN dm-linears suspended for the
# SuspendSeconds grace window (architecture.md, Migration, src side step 2),
# so a run killed mid-cutover leaves
# them that way — and the agent that would have retired them is gone. It also
# catches a crash inside a reload's suspend/load/resume. The failure it
# prevents is severe: anything that reads a suspended device (`dmsetup
# remove`, disabling the nvmet namespace above it, and above all a
# block-device scan) blocks in uninterruptible D state and wedges the node
# until reboot. Resuming first lets the queued IO drain or fail.
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

dm_remove_kind() {
	local name
	for name in $(dm_kind_names "$1"); do
		dm_force_remove "$name"
	done
}

disconnect_kind() { # <nqn kind digit>
	local nqn
	for nqn in $(subsys_json | jq -r --arg p "$NQN_PREFIX:$1:" '
	        [.. | objects | select(has("NQN")) | .NQN] | unique | .[]
	        | select(startswith($p))'); do
		timeout 30 nvme disconnect -n "$nqn" >/dev/null 2>&1
	done
}

# drop_subsystems <kind digit> — unlink from the port, disable and remove the
# namespaces, unlink the allowed hosts, remove the subsystem. Namespaces must
# be disabled before their backing dm devices can go.
drop_subsystems() {
	local subsys ns host link
	[ -d "$NVMET" ] || return 0
	for link in "$NVMET"/ports/*/subsystems/"$NQN_PREFIX":"$1":*; do
		[ -e "$link" ] && rm -f "$link"
	done
	for subsys in "$NVMET"/subsystems/"$NQN_PREFIX":"$1":*; do
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

loop_devs() {
	{
		losetup -j "$WORK/backing.img" 2>/dev/null | cut -d: -f1
		losetup -a 2>/dev/null | grep dnv-integtest | cut -d: -f1
	} | sort -u
}

# cleanup implements dnagent_integtest.md, Teardown and cleanup. The order is
# load-bearing; the one refinement over
# the plain step list is that the migration-source subsystems (:3:) are
# retired after the dm-clones are gone rather than with the side subsystems,
# because a dm-clone flushes to its source on remove and the source is that
# export. Everything the side namespaces back is still disabled first.
cleanup() {
	pkill -x dnv-agent >/dev/null 2>&1
	local i
	for ((i = 0; i < 20; i++)); do
		pgrep -x dnv-agent >/dev/null 2>&1 || break
		sleep 0.25
	done
	pkill -9 -x dnv-agent >/dev/null 2>&1

	# Every device pin, whoever left it (see kill_pins). It comes before
	# everything below because an fd on a dm device outlives the agent that
	# was just killed, and nothing here can remove a device while it is
	# open — not even `dmsetup remove --force`.
	kill_pins

	# Nothing may stay suspended from here on (see resume_suspended).
	resume_suspended

	# nvmet controllers must die before nvmet teardown.
	disconnect_kind 2
	drop_subsystems 2

	# Top-down: the per-CN linears, then the clones (their source connection
	# is still up), then the connections, then the clone-metadata wrappers
	# the clones sat on, then the migr-src linears and dm-errors, and only
	# then the side devices everything above was stacked on. A final sweep
	# retries anything that was busy.
	dm_remove_kind d1
	dm_remove_kind d3
	disconnect_kind 3
	drop_subsystems 3
	dm_remove_kind d5
	dm_remove_kind d2
	dm_remove_kind d0
	dm_remove_kind d4
	for name in $(agent_dm_names); do
		dm_force_remove "$name"
	done

	if [ -d "$NVMET" ]; then
		local host grp
		for host in "$NVMET"/hosts/"$NQN_PREFIX":*; do
			[ -d "$host" ] && rmdir "$host" 2>/dev/null
		done
		for grp in 3 2; do
			rmdir "$NVMET/ports/1/ana_groups/$grp" 2>/dev/null
		done
		rmdir "$NVMET/ports/1" 2>/dev/null
	fi

	# Defensive: nothing the agent builds is ever left suspended ([D12]),
	# but a device a killed agent or a failed dmsetup command left suspended
	# would wedge the reads below.
	resume_suspended

	# Unformat each loop device: zeroing the 4 KiB header is enough, because
	# the volume-table slots are inert without it — a slot only counts when
	# its format_uuid matches the header's ([D13], dnagent.md DN5: magic
	# absent ⇒ the disk is blank). No oflag=, per the dd lab fact of
	# dnagent_integtest.md, Assumptions and preflight checks.
	local dev
	for dev in $(loop_devs); do
		dd if=/dev/zero of="$dev" bs=4096 count=1 conv=fsync >/dev/null 2>&1
		wipefs -a "$dev" >/dev/null 2>&1
		losetup -d "$dev" >/dev/null 2>&1
	done
	rm -rf "$WORK"
	echo "cleaned"
	return 0
}

# lab_wipe — the ONE-TIME lab wipe (the wipe paragraph of dnagent_integtest.md,
# Teardown and cleanup). It is NOT part of a run: only the driver's --wipe
# reaches it.
#
# Why it exists at all: every teardown verb above removes dm devices BY KIND,
# and the kind literals they pass are the role-lettered ones (c0…cb, d0…d5).
# Residue on a shared lab VM whose name carries no such kind — a bare-digit
# kind field, or an LVM `dnv--clone--vg-*` node — is walked straight past by
# those verbs and stays there forever, pinning loop devices and nvmet objects
# the next run needs. This one reads no kind at all — everything named `dnv*`
# goes, whatever its spelling.
#
# That is also why it is not wired into cleanup — though not because it would
# destroy a CONCURRENT run's objects: so does cleanup, which kills every
# dnv-agent on the node whatever its role, removes every dnv dm device of
# either role letter and the shared port's ana_groups. What only this adds is
# reach further: every `dnv*` dm device in either kind spelling; every
# `nqn.2024-01.io.dnv*` subsystem, not only the :2: and :3: ones cleanup drops
# but the cn role's :4: exports and the suites' `nqn.2024-01.io.dnv-it:*` ones
# too; every dnv md array; and, past dnv itself, `nvme disconnect-all`, which
# takes every fabrics controller on the node, dnv's or not. Run it once,
# alone.
#
# The order is that of dnagent_integtest.md, Teardown and cleanup, generalized
# away from the kind list: arrays first (an
# array holds its member wrappers open and is the one holder `dmsetup remove
# --force` cannot argue with), then the controllers, then the nvmet objects
# that pin dm devices from above, then the dm devices themselves — enumerated
# out of `dmsetup ls` once per round instead of from a kind order, because
# "reverse dependency order" is exactly "whatever is still there after the
# round that freed it".
lab_wipe() {
	local d dev nm mdname hit i cnt prev names name

	pkill -x dnv-agent >/dev/null 2>&1
	for ((i = 0; i < 20; i++)); do
		pgrep -x dnv-agent >/dev/null 2>&1 || break
		sleep 0.25
	done
	pkill -9 -x dnv-agent >/dev/null 2>&1

	# Nothing may stay suspended from here on (see resume_suspended): reading
	# a suspended device goes to D state, where `timeout` cannot reach it.
	resume_suspended

	# THREE PASSES, because one is provably not enough. Stopping the arrays
	# frees their member wrappers, but the members still carry md
	# superblocks, and a dm device that reappears — or that udev re-examines
	# while this is running — is re-assembled into a fresh array that pins
	# the wrapper again (this suite installs no udev rule and masks nothing,
	# so udev can re-assemble an array from its members).
	# On a cn guest one pass can leave leg wrappers held open by
	# re-assembled arrays; a second, identical pass removes them. So the
	# sequence runs until the node is clean, not once.
	local pass
	for pass in 1 2 3; do
		if [ "$pass" -gt 1 ] && [ -z "$(dmsetup ls 2>/dev/null |
			awk '$1 ~ /^dnv/ {print $1}')" ]; then
			break
		fi

		# Every md array on this node that is ours, by two independent routes
		# because each is blind to a case the other sees. THE MEMBER ROUTE reads
		# the member's dm name straight out of sysfs, needs no
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
		# can go. drop_subsystems takes ONE nqn kind digit; the wipe wants the
		# plan's whole `nqn.2024-01.io.dnv*` glob, every kind digit included,
		# so the sweep is written out here.
		if [ -d "$NVMET" ]; then
			local subsys ns host link
			for link in "$NVMET"/ports/*/subsystems/"$NQN_PREFIX"*; do
				[ -e "$link" ] && rm -f "$link"
			done
			for subsys in "$NVMET"/subsystems/"$NQN_PREFIX"*; do
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
		fi

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

	# The residue is the report AND the exit status, although this verb is
	# best-effort like every other one here: the driver runs both VMs
	# concurrently and their output interleaves, so a wipe that only printed
	# its report could leave one VM full of debris behind the other VM's
	# clean report, and the run would say PASS. A cleanup that cannot fail is
	# a cleanup nobody can trust, which is the same rule the sweep this suite
	# tests lives by: what is left is reported, and reporting it is not
	# success. Everything this deliberately does not touch — $WORK, the loop
	# devices, the nvmet port — belongs to the ordinary
	# cleanup that runs after it and is not counted here.
	#
	# A `dmsetup ls` that did not answer is no evidence of a clean node: the
	# dm residue is then reported as unknown, which fails the wipe just as a
	# residue does. nvmet_left keeps its 2>/dev/null: like the removal loop
	# above, it takes a missing $NVMET for a node that exports nothing.
	local dms dm_left nvmet_left md_left
	if dms=$(dmsetup ls); then
		dm_left=$(printf '%s\n' "$dms" | awk '$1 ~ /^dnv/ {print $1}' | tr '\n' ' ')
	else
		echo "lab_wipe: dmsetup ls did not answer; the dm residue is unknown" >&2
		dm_left="(unknown: dmsetup ls did not answer)"
	fi
	nvmet_left=$(ls "$NVMET/subsystems" 2>/dev/null | grep -F dnv | tr '\n' ' ')
	md_left=$(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null | tr '\n' ' ')
	echo wiped
	printf 'dm left: %s\n' "$dm_left"
	printf 'nvmet left: %s\n' "$nvmet_left"
	printf 'md on this node (ours or not): %s\n' "$md_left"
	[ -z "$dm_left$nvmet_left" ] || return 1
	return 0
}

diag() {
	echo "--- agent.log (last 120 lines) ---"
	tail -n 120 "$WORK/agent.log" 2>/dev/null
	echo "--- dmsetup ls ---"
	dmsetup ls 2>/dev/null
	echo "--- dmsetup table ---"
	dmsetup table 2>/dev/null
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

# wipe_all runs the one-time lab wipe (dnagent_integtest.md,
# Teardown and cleanup) on both VMs, concurrently
# for cleanup_all's reason: a subsystem on one VM backs a connection on the
# other, so the shorter the window the better. `--wipe` is its only caller and
# it always runs the ordinary start-of-run cleanup afterwards, which takes
# what the wipe deliberately leaves — $WORK, the loop devices and the nvmet
# port.
#
# EACH VM'S STATUS IS READ, because the two VMs' output interleaves and a
# missing report does not stand out.
# A verb whose failure cannot be seen is worse than no
# verb, which is the same rule the sweep this suite tests is built on.
wipe_all() {
	local idx rc pids=() bad=()
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
		die "the lab wipe did not leave ${bad[*]} clean — the residue, or the" \
			"listing that did not answer, is printed above; re-run --wipe, and" \
			"if the same names survive that something outside dnv is holding" \
			"them"
}

# cleanup_all runs the two VMs concurrently: dismantling one node's nvmet
# objects kills the other's migration-source connection, so the shorter that
# window is the better.
cleanup_all() {
	local idx pid pids=()
	for idx in 1 2; do
		helper_ok "$idx" cleanup &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do wait "$pid" || true; done
}

# DIAG_SIDES holds "dnidx sp leg side" for every side a migration case has
# reached so far, so the failure dump of dnagent_integtest.md,
# Teardown and cleanup, can end with the last get-side-info of every
# involved side — the one view of a failure that names the migration's own
# resources (clone, target, per-CN maps) instead of the node's raw dm/nvmet
# state. The migration cases fill it as they set each side up (a side that does
# not exist yet has nothing to report), and clear it once the case has torn its
# sides down again.
DIAG_SIDES=()
diag_add_side() { # dnidx sp leg side
	DIAG_SIDES+=("$1 $2 $3 $4")
}

diagnostics() {
	local idx
	for idx in 1 2; do
		log ""
		log "########## vm$idx ##########"
		helper_ok "$idx" diag >&2
	done
	# Every call here is best-effort: diagnostics runs on the failure path
	# under `set -e`, and a side that is already gone — or an agent that is
	# wedged and lets the RPC deadline expire — must never replace the real
	# failure with its own.
	local entry dn sp leg side
	for entry in "${DIAG_SIDES[@]}"; do
		read -r dn sp leg side <<<"$entry"
		log ""
		log "########## dn$dn get-side-info $sp/$leg/$side ##########"
		ctl "$dn" get-side-info --sp "$sp" --leg "$leg" --side "$side" >&2 ||
			true
	done
}

# ---------------------------------------------------------------------------
# Preflight (dnagent_integtest.md, Assumptions and preflight checks)
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
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$AGENT_BIN" ] || die "missing: $AGENT_BIN after make build"
	(cd "$REPO_ROOT" && go build -o "$CTL" ./integtest/dnagentctl >&2) ||
		die "building dnagentctl failed"
}

# preflight_vms runs after the start-of-run cleanup: the port check can only
# be meaningful once a crashed prior run's agents are gone
# (dnagent_integtest.md, Assumptions and preflight checks).
preflight_vms() {
	STAGE="preflight (vms)"
	log "=== preflight: vms"
	local idx
	for idx in 1 2; do
		ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo -n true" ||
			die "missing: passwordless sudo on vm$idx (${VM[$idx]})"
		local missing
		missing=$(sshv "$idx" "for b in dmsetup nvme losetup blkdiscard lsblk dd fallocate sha256sum cmp pkill jq timeout setsid wipefs ss; do command -v \$b >/dev/null || echo \$b; done")
		[ -z "$missing" ] || die "missing: $missing on vm$idx"
		# The agent hardcodes the configfs path and neither mounts nor
		# modprobes; the harness does both here and nothing else.
		sshv "$idx" "for m in nvmet nvmet-tcp nvme-tcp nvme-fabrics dm-clone loop; do modprobe \$m 2>/dev/null || true; done; grep -q ' /sys/kernel/config ' /proc/mounts || mount -t configfs none /sys/kernel/config; true"
		local got
		got=$(sshv "$idx" "ls -d $NVMET 2>/dev/null || echo MISSING")
		assert_eq "$got" "$NVMET" "nvmet configfs on vm$idx"
		got=$(sshv "$idx" "cat /sys/module/nvme_core/parameters/multipath")
		assert_eq "$got" "Y" "nvme_core.multipath on vm$idx"
		got=$(sshv "$idx" "df -Pk /var/tmp | awk 'NR==2 {print \$4}'")
		[ "$got" -ge 3145728 ] ||
			die "vm$idx: /var/tmp has ${got}K free, want >= 3 GiB"
		got=$(sshv "$idx" "f=/var/tmp/dnv-punch-probe.\$\$; fallocate -l 8M \$f && fallocate -p -o 0 -l 4M \$f && echo PUNCH_OK || echo PUNCH_NO; rm -f \$f")
		assert_eq "$got" "PUNCH_OK" "vm$idx: /var/tmp punch-hole support"
		got=$(sshv "$idx" "ss -ltnH | awk '{print \$4}' | sed 's/.*://' | grep -cE '^($GRPC_PORT|$TR_SVC_ID)\$' || true")
		assert_eq "$got" "0" "vm$idx: ports $GRPC_PORT/$TR_SVC_ID must be free"
	done
	log "preflight ok"
}

# ---------------------------------------------------------------------------
# Setup (dnagent_integtest.md, Cases, the setup paragraph)
# ---------------------------------------------------------------------------

start_agent() { # <idx>
	local idx=$1
	sshv "$idx" "setsid nohup $WORK/dnv-agent dn \
--grpc-network tcp --grpc-address ${IP[$idx]}:$GRPC_PORT \
--tr-type tcp --adr-fam ipv4 --tr-addr ${IP[$idx]} --tr-svc-id $TR_SVC_ID \
--local-store $WORK/store --disk ${LOOP[$idx]} \
>> $WORK/agent.log 2>&1 < /dev/null & echo launched"
}

setup() {
	CASE=setup
	stage vms "creating the backing store and launching both agents"
	local idx out
	for idx in 1 2; do
		# --local-store must pre-exist: startup reconcile fails without it.
		sshv "$idx" "mkdir -p $WORK/store && chmod 0777 $WORK && chmod 0755 $WORK/store"
		sshv "$idx" "fallocate -l 2G $WORK/backing.img"
		LOOP[idx]=$(sshv "$idx" "losetup --find --show $WORK/backing.img")
		log "[vm$idx] loop device ${LOOP[idx]}"
		log "[vm$idx] scp dnv-agent"
		scp -q "${SSH_OPTS[@]}" "$AGENT_BIN" "${VM[$idx]}:$WORK/dnv-agent"
		sshv "$idx" "chmod 0755 $WORK/dnv-agent"
		start_agent "$idx"
	done
	SETUP_DONE=1

	stage waitup "GetDnSize is lock-free, so it doubles as the liveness probe"
	for idx in 1 2; do
		out=$(ctl "$idx" get-dn-size --wait 15)
		assert_eq "$(jq_of "$out" .size)" "$DATA_SIZE" "vm$idx GetDnSize"
	done

	stage base "SyncupDn baseline: disk format (header + table), nvmet port"
	for idx in 1 2; do
		bump_dn_rev "$idx"
		out=$(ctl "$idx" syncup-dn \
			--revision "${REV[$idx]}" --extent-size "$EXTENT_SIZE")
		assert_dn_info_ok "$out" "dn$idx baseline"
	done
}

# ---------------------------------------------------------------------------
# Shared case helpers
# ---------------------------------------------------------------------------

# converge_check runs the check-dn/check-side round pair (dnagent_integtest.md,
# Conventions, Check rounds) against a side and asserts that each reply echoes
# the revision the agent actually stored.
converge_check() { # dnidx sp leg side siderev
	local idx=$1 out
	out=$(ctl "$idx" check-dn --revision "${DNREV[$idx]}" --show-info)
	assert_dn_info_ok "$out" "dn$idx check-dn"
	assert_eq "$(jq_of "$out" '.revision // "0"')" "${DNREV[$idx]}" \
		"dn$idx check-dn revision"
	out=$(ctl "$idx" check-side --revision "$5" --show-info \
		--sp "$2" --leg "$3" --side "$4")
	assert_ok "$out" ".side_info.side_dev_info.status" "dn$idx check-side side_dev"
	assert_eq "$(jq_of "$out" '.revision // "0"')" "$5" \
		"dn$idx check-side revision"
}

assert_no_residue() { # sp
	local idx got
	for idx in 1 2; do
		got=$(helper "$idx" "residue $(hex16 "$1")")
		[ -z "$got" ] || die "vm$idx still holds objects of sp $1: $got"
	done
}

# dn_drop_until_clean re-issues one DN's drop — the syncup-dn whose side list
# is empty, at the revision the caller has already minted — every 2 s until
# the agent replies 0, and gives up after <secs>.
#
# It exists because a teardown that has to remove resources hanging off a dead
# remote is legitimately not finished in one pass (DN6; architecture.md,
# Teardown by sweep).
# The sweep removes what the desired state no longer wants, verifies every
# removal with a probe that cannot block, and replies with the leftover code
# while anything is still there. The first `dmsetup remove` of a dm-clone
# whose source has just died is killed at CmdSoftTimeout; its kernel
# operation completes when the queued hydration IO fails at fast_io_fail_tmo
# (DefaultNvmeFastIoFailTmo), and the pass after that finds the device gone.
#
# Re-driving is the worker's own rule, and the loop is the suite playing that
# part: in production the retry comes from the CHECK round — the agent
# recomputes the verdict on every CheckDn, and a round whose code is non-zero
# issues another SyncupDn (RW4 step 5) — never from the Syncup reply itself.
# No such round is running behind this suite, so it re-sends the request
# itself.
#
# Only code 4 is tolerated in between. Every other non-zero code is a refusal
# (stale revision, invalid conf, unknown object) that repeating cannot fix, so
# it dies immediately rather than after the whole budget. The first reply is
# logged with its details: whether the very first pass was already clean or
# reported the leftover the design predicts is the one thing about this loop
# worth reading afterwards, and neither outcome is a failure.
dn_drop_until_clean() { # dnidx secs
	local idx=$1 secs=$2 out code details deadline first=1
	deadline=$((SECONDS + secs))
	while :; do
		# No --expect-code: 4 is expected here for a while, so the reply is
		# read rather than asserted. `|| true` keeps ctl's own exit status —
		# which enforces --expect-code 0 by default — from ending the run.
		out=$(ctl "$idx" syncup-dn --revision "${REV[$idx]}" \
			--extent-size "$EXTENT_SIZE" || true)
		[ -n "$out" ] || die "dn$idx drop: the RPC produced no reply"
		code=$(jq_of "$out" '.agent_reply.code // 0')
		details=$(jq_of "$out" '.agent_reply.details // ""')
		if [ "$first" -eq 1 ]; then
			log "dn$idx drop: first reply code $code, details: ${details:-(none)}"
			first=0
		fi
		if [ "$code" = 0 ]; then
			assert_dn_info_ok "$out" "dn$idx drop"
			return 0
		fi
		[ "$code" = 4 ] ||
			die "dn$idx drop: agent_reply.code $code ($details)"
		[ "$SECONDS" -lt "$deadline" ] ||
			die "dn$idx drop: still not clean after ${secs}s: $details"
		sleep 2
	done
}

# wait_zeroed blocks until a side's background zeroing goroutine has zeroed
# the length its request asks for (architecture.md, Side provisioning
# protocol). `ctl` adds no --timeout for this subcommand, so the one below is
# wait-zeroed's own polling budget, not an RPC deadline.
#
# The fifth argument is the --zero-bytes the request asked for, and turns the
# wait into the exact zeroed = required = N equality of dnagent_integtest.md,
# Conventions, The provisioned flip: the driver's own loop exits on
# `required != 0 && zeroed >= required`, which a side that recorded a length
# other than the request's also satisfies once it has zeroed all of it. Both
# counts are read through $(( )), which takes a JSON number and a decimal
# string alike.
wait_zeroed() { # dnidx sp leg side zero_bytes
	local idx=$1 want=$5 out zdone ztotal
	out=$(ctl "$idx" wait-zeroed --sp "$2" --leg "$3" --side "$4" \
		--interval 0.5 --timeout "$ZERO_TIMEOUT")
	zdone=$(jq_of "$out" '.zeroed_bytes // 0')
	ztotal=$(jq_of "$out" '.zero_bytes // 0')
	assert_eq "$((zdone))/$((ztotal))" "$((want))/$((want))" \
		"side $2/$3/$4 zeroed/required bytes after wait-zeroed"
}

# sync_side_cns lists every CN id one syncup-side request names — the primary
# and every standby — by scanning the caller's flag list the way Go's flag
# package does (`--flag value`, plus the `--flag=value` spelling). Phase (a)
# must be asserted for *all* of them: dnagent_integtest.md, Conventions,
# The provisioned flip, says "Before the flip every per-CN row must read
# provisioning", and a case A/D side carries a standby, so looking at one map
# entry leaves one of the two per-CN stacks unexamined while it is gated.
sync_side_cns() { # syncup-side flags…
	local arg want=""
	for arg in "$@"; do
		if [ -n "$want" ]; then
			printf '%s\n' "$arg"
			want=""
			continue
		fi
		case "$arg" in
		--primary-cn | --standby-cn) want=cn ;;
		--primary-cn=* | --standby-cn=*) printf '%s\n' "${arg#*=}" ;;
		esac
	done
}

# sync_side_zero_bytes prints the --zero-bytes one syncup-side request asks
# for, by the same flag scan as sync_side_cns (both spellings). It is what
# makes the exact equality of dnagent_integtest.md, Conventions, The
# provisioned flip, checkable from the helper: the expected length is the
# caller's own flag, not a constant this file could drift from. Nothing is
# printed when the request carries no --zero-bytes, which sync_side_2phase
# refuses: the agent's side conf gate refuses a length of zero (dnagent.md
# DN8), so such a request could never provision.
sync_side_zero_bytes() { # syncup-side flags…
	local arg want=""
	for arg in "$@"; do
		if [ -n "$want" ]; then
			printf '%s\n' "$arg"
			return 0
		fi
		case "$arg" in
		--zero-bytes) want=zb ;;
		--zero-bytes=*)
			printf '%s\n' "${arg#*=}"
			return 0
			;;
		esac
	done
}

# sync_side_2phase performs the two-phase side provisioning the sp-worker
# performs in production (architecture.md, Side provisioning protocol).
# Phase 1 syncs the side with
# --provisioned=false: allocate the extent runs, build DnSideName, start the
# zeroing goroutine — and export nothing. wait_zeroed then blocks until the
# side has zeroed the length its request asks for, and phase 2 re-sends the
# identical request at a fresh revision with --provisioned=true, which is the
# worker's flip rule played by the script.
#
# Both revisions are minted by the caller in the parent shell
# (dnagent_integtest.md, Conventions, Revisions), so this is
# safe inside a background job. The phase-2 reply is left in SYNC_SIDE_REPLY:
# a bash function cannot both echo the reply and be called outside a command
# substitution, and the caller must not run bump_rev in one.
SYNC_SIDE_REPLY=""
sync_side_2phase() { # dnidx rev1 rev2 sp leg side [extra syncup-side flags…]
	local idx=$1 rev1=$2 rev2=$3 sp=$4 leg=$5 side=$6 out
	local cns cn key field left zlen sample zdone ztotal
	shift 6
	# The exact equality zeroed_bytes == zero_bytes == the request's
	# --zero-bytes (dnagent_integtest.md, Conventions, The provisioned flip),
	# asserted on both the wait's last sample and the flip's reply: a side
	# that recorded a length other than the request's zeroes all of that
	# length and would satisfy every `zeroed >= required` check on the way.
	# Hard, not tolerant.
	zlen=$(sync_side_zero_bytes "$@")
	[ -n "$zlen" ] ||
		die "provisioning $sp/$leg/$side: the request carries no --zero-bytes"
	out=$(ctl "$idx" syncup-side --revision "$rev1" \
		--sp "$sp" --leg "$leg" --side "$side" --provisioned=false "$@")
	assert_provisioning_or_ok "$out" ".side_info.side_dev_info.status" \
		"provisioning $sp/$leg/$side phase 1"
	# The whole point of the gate: nothing above the side device exists before
	# the bytes are zeroed, whatever the zeroing progress happens to be.
	#
	# All three per-CN maps, for every CN — not one entry of one map. The
	# reply half of this is close to a self-report (reportAboveSideDeferred
	# fills the three maps with PROVISIONING in the same branch that decides
	# not to build), so it is paired below with the kernel-side half, which is
	# the only thing that can catch a stale subsystem surviving from a prior
	# incarnation or a fault in the nvmet/OsClient layer the unit tests' fake
	# node does not model (the gate proof of dnagent_integtest.md,
	# Conventions, The provisioned flip).
	cns=$(sync_side_cns "$@")
	[ -n "$cns" ] ||
		die "provisioning $sp/$leg/$side: the request names no CN"
	for cn in $cns; do
		key=$((cn))
		for field in cn_id_to_dm_error cn_id_to_dm_linear cn_id_to_nvmeof; do
			assert_provisioning "$out" ".side_info.$field[\"$key\"].status" \
				"provisioning $sp/$leg/$side must not export at provisioned=false: $field[$cn]"
		done
		assert_eq "$(helper "$idx" \
			"subsys_present '$(side_to_cn_nqn "$CLUSTER" "$sp" "$leg" "$cn")'")" \
			no \
			"provisioning $sp/$leg/$side: cn $cn has a :2: subsystem at provisioned=false"
	done
	left=$(helper "$idx" "export_dms $(hex16 "$sp") $(hex16 "$side")")
	[ -z "$left" ] ||
		die "provisioning $sp/$leg/$side: export dm devices exist at provisioned=false: $left"
	# The provisioning-window sample (dnagent_integtest.md, Conventions): one
	# get-side-info before the wait, to
	# record whether this run ever observed the side mid-zeroing. Purely an
	# observation and never an assertion — a side here zeroes its length in
	# one or two batches and loop maps Write Zeroes onto `fallocate`, so the
	# window is normally already closed by the time this samples, exactly
	# like the migration cases' grace-window and read-through probes (the
	# same convention). A failed call degrades to a miss
	# for the same reason: this must not be able to fail the suite (the next
	# line's wait-zeroed is where a real problem surfaces). The reply is
	# protojson, which prints a uint64 as a decimal string and omits a zero.
	sample=$(ctl "$idx" get-side-info --sp "$sp" --leg "$leg" --side "$side" ||
		true)
	[ -n "$sample" ] || sample="{}"
	zdone=$(jq_of "$sample" '.side_info.zeroed_bytes // "0"')
	ztotal=$(jq_of "$sample" '.side_info.zero_bytes // "0"')
	if [ "$ztotal" -gt 0 ] && [ "$zdone" -lt "$ztotal" ]; then
		log "provisioning $sp/$leg/$side: provisioning window HIT ($zdone/$ztotal bytes zeroed)"
	else
		log "provisioning $sp/$leg/$side: WARNING provisioning window missed ($zdone/$ztotal bytes zeroed)"
	fi
	wait_zeroed "$idx" "$sp" "$leg" "$side" "$zlen"
	SYNC_SIDE_REPLY=$(ctl "$idx" syncup-side --revision "$rev2" \
		--sp "$sp" --leg "$leg" --side "$side" --provisioned=true "$@")
	zdone=$(jq_of "$SYNC_SIDE_REPLY" '.side_info.zeroed_bytes // "0"')
	ztotal=$(jq_of "$SYNC_SIDE_REPLY" '.side_info.zero_bytes // "0"')
	assert_eq "$((zdone))/$((ztotal))" "$((zlen))/$((zlen))" \
		"provisioning $sp/$leg/$side: zeroed/required bytes at provisioned=true"
}

# ---------------------------------------------------------------------------
# Case S — smoke (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------

case_smoke() {
	CASE=smoke
	local sp=0xa1 leg=0x1 side=0x11 cn=0x21 cnvm=2 dn=1
	local out dev nqn siderev provrev

	stage dn "SyncupDn introduces the side pointer"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE" --side "$sp:$leg:$side")
	assert_dn_info_ok "$out" "smoke syncup-dn"

	stage side "two-phase provisioning, then the dm stack and the nvmet export"
	bump_rev "$dn"
	provrev=${REV[$dn]}
	bump_rev "$dn"
	siderev=${REV[$dn]}
	sync_side_2phase "$dn" "$provrev" "$siderev" "$sp" "$leg" "$side" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "smoke side_dev"
	assert_cn_ok "$out" cn_id_to_dm_error "$cn" smoke
	assert_cn_ok "$out" cn_id_to_dm_linear "$cn" smoke
	assert_cn_ok "$out" cn_id_to_nvmeof "$cn" smoke

	stage connect "the CN identity connects and the path goes live/optimized"
	cn_connect "$cnvm" "$dn" "$sp" "$leg" "$cn"
	dev=$(ns_by_id "$sp" "$leg")
	helper "$cnvm" "wait_dev '$dev' 20" || die "no device node $dev on vm$cnvm"
	assert_eq "$(cn_path_field "$cnvm" "$sp" "$leg" "$cn" "$dn" State)" \
		live "smoke path state"
	cn_wait_ana "$cnvm" "$sp" "$leg" "$cn" "$dn" optimized 20

	stage io "4 MiB write/readback round trip through the exported namespace"
	sshv "$cnvm" "dd if=/dev/urandom of=$WORK/pattern-smoke.bin bs=1M count=4 conv=fsync status=none"
	local want got
	want=$(sha_range "$cnvm" "$WORK/pattern-smoke.bin" 4)
	write_range "$cnvm" "$WORK/pattern-smoke.bin" "$dev" 4
	drop_caches "$cnvm"
	got=$(sha_range "$cnvm" "$dev" 4)
	assert_eq "$got" "$want" "smoke readback sha256"

	stage check "converge check round"
	converge_check "$dn" "$sp" "$leg" "$side" "$siderev"

	stage teardown "an empty side list tears the side down declaratively"
	cn_disconnect "$cnvm" "$sp" "$leg" "$cn"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE")
	assert_dn_info_ok "$out" "smoke teardown syncup-dn"
	out=$(ctl "$dn" get-dn-info)
	assert_dn_info_ok "$out" "smoke teardown get-dn-info"
	assert_no_residue "$sp"
	sshv_ok "$cnvm" "rm -f $WORK/pattern-smoke.bin"
	nqn=$(side_to_cn_nqn "$CLUSTER" "$sp" "$leg" "$cn")
	log "smoke: $nqn is gone"
}

# ---------------------------------------------------------------------------
# Case Z — zeroing (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------
#
# The length a side zeroes is the length its request asks for, and not one
# byte past it (architecture.md, Side provisioning protocol; dnagent.md DN9).
# Only the agent knows where a side's extents lie, so the fill covers the
# node's whole extent area, which holds no side between cases, with one random
# block an extent long written into every extent slot: whatever extents a side
# is given, its byte at offset x then holds the block's byte at x modulo the
# extent size unless zeroing reached it. Every length here is whole MiB and
# the reads are MiB ranges on either side of where it ends, so a byte zeroed
# too many or too few changes a hash.

case_zeroing() {
	CASE=zeroing
	local dn=1 sp=0x91 mleg=0x1 mside=0x11 dleg=0x2 dside=0x12 cn=0x21
	local ext_mib=$((EXTENT_SIZE / 1048576))
	local zero_mib=$((DATA_ZERO_BYTES / 1048576))
	# A meta group's side zeroes its leg span, its meta and data blocks
	# together (model.SideZeroBytes); with whole 1 MiB blocks in 64 MiB
	# extents that is the whole two-extent side.
	local mzero=$((2 * EXTENT_SIZE))
	local out got provrev siderev mdev ddev zero

	stage fill "a random pattern over dn$dn's whole extent area, which holds no side"
	got=$(helper "$dn" "dm_kind_names d4")
	[ -z "$got" ] ||
		die "zeroing: dn$dn holds side devices the fill would overwrite: $got"
	sshv "$dn" "dd if=/dev/urandom of=$WORK/fill.bin bs=1M count=$ext_mib conv=fsync status=none"
	helper "$dn" "fill_extents '${LOOP[$dn]}' '$WORK/fill.bin' \
$((DATA_OFFSET / 1048576)) $ext_mib $((DATA_SIZE / EXTENT_SIZE))" >/dev/null ||
		die "zeroing: the fill of dn$dn's extent area failed"

	stage dn "SyncupDn introduces both side pointers"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE" \
		--side "$sp:$mleg:$mside" --side "$sp:$dleg:$dside")
	assert_dn_info_ok "$out" "zeroing syncup-dn"

	stage meta "a meta group's side asks to zero its whole leg span"
	diag_add_side "$dn" "$sp" "$mleg" "$mside"
	bump_rev "$dn"
	provrev=${REV[$dn]}
	bump_rev "$dn"
	siderev=${REV[$dn]}
	sync_side_2phase "$dn" "$provrev" "$siderev" "$sp" "$mleg" "$mside" \
		--ext-cnt 2 --zero-bytes "$mzero" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	assert_ok "$SYNC_SIDE_REPLY" ".side_info.side_dev_info.status" \
		"zeroing meta side_dev"

	stage data "a data group's side asks to zero its meta region and first data block"
	diag_add_side "$dn" "$sp" "$dleg" "$dside"
	bump_rev "$dn"
	provrev=${REV[$dn]}
	bump_rev "$dn"
	siderev=${REV[$dn]}
	sync_side_2phase "$dn" "$provrev" "$siderev" "$sp" "$dleg" "$dside" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	assert_ok "$SYNC_SIDE_REPLY" ".side_info.side_dev_info.status" \
		"zeroing data side_dev"

	stage read "each side device reads zero over its length and the pattern past it"
	mdev=/dev/mapper/$(dn_side_name "$CLUSTER" "${DNID[$dn]}" "$sp" "$mside")
	ddev=/dev/mapper/$(dn_side_name "$CLUSTER" "${DNID[$dn]}" "$sp" "$dside")
	drop_caches "$dn"
	zero=$(sha_range "$dn" /dev/zero $((2 * ext_mib)))
	assert_eq "$(sha_range "$dn" "$mdev" $((2 * ext_mib)))" "$zero" \
		"zeroing: the meta group's side reads zero throughout"
	assert_eq "$(sha_range "$dn" "$ddev" "$zero_mib")" \
		"$(sha_range "$dn" /dev/zero "$zero_mib")" \
		"zeroing: the data group's side reads zero over its length"
	assert_eq "$(sha_range "$dn" "$ddev" $((ext_mib - zero_mib)) "$zero_mib")" \
		"$(sha_range "$dn" "$WORK/fill.bin" $((ext_mib - zero_mib)) "$zero_mib")" \
		"zeroing: the data group's side holds the pattern past its length"
	assert_eq "$(sha_range "$dn" "$ddev" "$ext_mib" "$ext_mib")" \
		"$(sha_range "$dn" "$WORK/fill.bin" "$ext_mib")" \
		"zeroing: the data group's side holds the pattern to its end"

	stage teardown "an empty side list tears both sides down"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE")
	assert_dn_info_ok "$out" "zeroing teardown syncup-dn"
	assert_no_residue "$sp"
	# The records went with their devices: an orphaned one would make the
	# read-only verdict a leftover, which ctl's default expected code fails.
	ctl "$dn" get-dn-info >/dev/null
	sshv_ok "$dn" "rm -f $WORK/fill.bin"
	DIAG_SIDES=()
}

# ---------------------------------------------------------------------------
# Case A — sides (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------

# The 4 legs of case A: leg, owning DN, side id, primary CN, standby CN.
A_LEG=("" 0x1 0x2 0x3 0x4)
A_DN=("" 1 1 2 2)
A_SIDE=("" 0x11 0x12 0x13 0x14)
A_PRIMARY=("" 0x21 0x21 0x22 0x22)
A_STANDBY=("" 0x22 0x22 0x21 0x21)
# Which VM hosts which CN identity: CN 0x21 on vm2, CN 0x22 on vm1.
a_cn_vm() { [ "$1" = "0x21" ] && echo 2 || echo 1; }

case_sides() {
	CASE=sides
	local sp=0xb1
	local i out dn nqn provrev
	local siderev=("" 0 0 0 0)

	stage dn "both DNs learn their two side pointers"
	bump_dn_rev 1
	out=$(ctl 1 syncup-dn --revision "${REV[1]}" \
		--extent-size "$EXTENT_SIZE" \
		--side "$sp:${A_LEG[1]}:${A_SIDE[1]}" --side "$sp:${A_LEG[2]}:${A_SIDE[2]}")
	assert_dn_info_ok "$out" "sides dn1"
	bump_dn_rev 2
	out=$(ctl 2 syncup-dn --revision "${REV[2]}" \
		--extent-size "$EXTENT_SIZE" \
		--side "$sp:${A_LEG[3]}:${A_SIDE[3]}" --side "$sp:${A_LEG[4]}:${A_SIDE[4]}")
	assert_dn_info_ok "$out" "sides dn2"

	stage sides "4 sides provision two-phase, then export to primary + standby"
	for i in 1 2 3 4; do
		dn=${A_DN[$i]}
		bump_rev "$dn"
		provrev=${REV[$dn]}
		bump_rev "$dn"
		siderev[i]=${REV[$dn]}
		sync_side_2phase "$dn" "$provrev" "${siderev[$i]}" \
			"$sp" "${A_LEG[$i]}" "${A_SIDE[$i]}" \
			--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
			--cntlid-slot 0 \
			--primary-cn "${A_PRIMARY[$i]}" --standby-cn "${A_STANDBY[$i]}" \
			--sp-level readwrite
		out=$SYNC_SIDE_REPLY
		assert_ok "$out" ".side_info.side_dev_info.status" "sides leg ${A_LEG[$i]} side_dev"
		assert_cn_ok "$out" cn_id_to_dm_error "${A_PRIMARY[$i]}" "leg ${A_LEG[$i]}"
		assert_cn_ok "$out" cn_id_to_dm_linear "${A_PRIMARY[$i]}" "leg ${A_LEG[$i]}"
		assert_cn_ok "$out" cn_id_to_nvmeof "${A_PRIMARY[$i]}" "leg ${A_LEG[$i]}"
		assert_cn_ok "$out" cn_id_to_dm_error "${A_STANDBY[$i]}" "leg ${A_LEG[$i]}"
		assert_cn_ok "$out" cn_id_to_dm_linear "${A_STANDBY[$i]}" "leg ${A_LEG[$i]}"
		assert_cn_ok "$out" cn_id_to_nvmeof "${A_STANDBY[$i]}" "leg ${A_LEG[$i]}"
	done

	stage connect "the 8-connection matrix: every CN to every side"
	for i in 1 2 3 4; do
		cn_connect "$(a_cn_vm "${A_PRIMARY[$i]}")" "${A_DN[$i]}" \
			"$sp" "${A_LEG[$i]}" "${A_PRIMARY[$i]}"
		cn_connect "$(a_cn_vm "${A_STANDBY[$i]}")" "${A_DN[$i]}" \
			"$sp" "${A_LEG[$i]}" "${A_STANDBY[$i]}"
	done

	stage primary "primary namespaces are optimized and carry data"
	local vm dev want got
	for i in 1 2 3 4; do
		vm=$(a_cn_vm "${A_PRIMARY[$i]}")
		dev=$(ns_by_id "$sp" "${A_LEG[$i]}")
		helper "$vm" "wait_dev '$dev' 20" || die "no $dev on vm$vm"
		cn_wait_ana "$vm" "$sp" "${A_LEG[$i]}" "${A_PRIMARY[$i]}" "${A_DN[$i]}" \
			optimized 20
		sshv "$vm" "dd if=/dev/urandom of=$WORK/pattern-sides-$i.bin bs=1M count=4 conv=fsync status=none"
		want=$(sha_range "$vm" "$WORK/pattern-sides-$i.bin" 4)
		write_range "$vm" "$WORK/pattern-sides-$i.bin" "$dev" 4
		drop_caches "$vm"
		got=$(sha_range "$vm" "$dev" 4)
		assert_eq "$got" "$want" "sides leg ${A_LEG[$i]} readback"
	done

	stage standby "standby namespaces are non-optimized and read EIO"
	for i in 1 2 3 4; do
		vm=$(a_cn_vm "${A_STANDBY[$i]}")
		cn_wait_ana "$vm" "$sp" "${A_LEG[$i]}" "${A_STANDBY[$i]}" "${A_DN[$i]}" \
			non-optimized 20
		# The standby export is deliberately backed by dm-error until
		# promotion: the node exists but every read fails.
		dev=$(ns_by_id "$sp" "${A_LEG[$i]}")
		helper "$vm" "wait_dev '$dev' 20" ||
			die "sides: standby node $dev missing on vm$vm"
		got=$(helper "$vm" "read_probe '$dev'")
		assert_eq "$got" eio "sides standby read of $dev"
	done

	stage isolation "each subsystem allows exactly one host"
	for i in 1 2 3 4; do
		for cn in "${A_PRIMARY[$i]}" "${A_STANDBY[$i]}"; do
			nqn=$(side_to_cn_nqn "$CLUSTER" "$sp" "${A_LEG[$i]}" "$cn")
			got=$(helper "${A_DN[$i]}" "allowed_host_cnt '$nqn'")
			assert_eq "$got" 1 "allowed_hosts of $nqn"
		done
	done

	stage check "converge check rounds on both DNs"
	converge_check 1 "$sp" "${A_LEG[1]}" "${A_SIDE[1]}" "${siderev[1]}"
	converge_check 2 "$sp" "${A_LEG[3]}" "${A_SIDE[3]}" "${siderev[3]}"

	stage teardown "disconnect all 8, then empty both side lists"
	for i in 1 2 3 4; do
		cn_disconnect "$(a_cn_vm "${A_PRIMARY[$i]}")" "$sp" "${A_LEG[$i]}" \
			"${A_PRIMARY[$i]}"
		cn_disconnect "$(a_cn_vm "${A_STANDBY[$i]}")" "$sp" "${A_LEG[$i]}" \
			"${A_STANDBY[$i]}"
	done
	for dn in 1 2; do
		bump_dn_rev "$dn"
		out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
			--extent-size "$EXTENT_SIZE")
		assert_dn_info_ok "$out" "sides teardown dn$dn"
	done
	assert_no_residue "$sp"
	for i in 1 2 3 4; do
		sshv_ok "$(a_cn_vm "${A_PRIMARY[$i]}")" "rm -f $WORK/pattern-sides-$i.bin"
	done
}

# ---------------------------------------------------------------------------
# Cases B and C — the shared migration choreography (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------

# join_jobs waits for the background jobs of one lockstep stage and fails the
# run if any of them did.
join_jobs() {
	local pid rc=0
	for pid in "$@"; do wait "$pid" || rc=1; done
	[ "$rc" -eq 0 ] || die "a concurrent migration step failed"
}

# migr_dn_sides fills SIDE_ARGS with the --side flags of one DN. with_src=0
# drops the DN's migration-source side, which is how the finish tears it down
# ("then drops the source side", dnagent_integtest.md, Cases).
SIDE_ARGS=()
migr_dn_sides() { # sp dnidx with_src
	local sp=$1 dn=$2 with_src=$3 m
	SIDE_ARGS=()
	for m in 1 2; do
		if [ "${MSRCDN[$m]}" = "$dn" ] && [ "$with_src" = 1 ]; then
			SIDE_ARGS+=(--side "$sp:${MLEG[$m]}:${MSRCSIDE[$m]}")
		fi
		if [ "${MDSTDN[$m]}" = "$dn" ]; then
			SIDE_ARGS+=(--side "$sp:${MLEG[$m]}:${MDSTSIDE[$m]}")
		fi
	done
}

# The per-migration steps. Each takes the migration index and runs against one
# src DN, one dst DN and one CN VM; the caller runs the two of them
# concurrently, so each agent plays src for one leg and dst for the other at
# the same time.

migr_src_side() { # m provrev revision
	local m=$1 provrev=$2 rev=$3 out
	sync_side_2phase "${MSRCDN[$m]}" "$provrev" "$rev" \
		"$SP" "${MLEG[$m]}" "${MSRCSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "migr $m src side_dev"
	assert_cn_ok "$out" cn_id_to_nvmeof "${MCN[$m]}" "migr $m src"
}

migr_prep_data() { # m
	local m=$1
	local vm=${MCNVM[$m]} dev out
	cn_connect "$vm" "${MSRCDN[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}"
	dev=$(ns_by_id "$SP" "${MLEG[$m]}")
	helper "$vm" "wait_dev '$dev' 20" || die "migr $m: no $dev on vm$vm"
	cn_wait_ana "$vm" "$SP" "${MLEG[$m]}" "${MCN[$m]}" "${MSRCDN[$m]}" \
		optimized 20
	# The pattern file is the only verification oracle: nothing re-reads the
	# source once the migration starts.
	sshv "$vm" "dd if=/dev/urandom of=$WORK/pattern-$m.bin bs=1M count=128 conv=fsync status=none"
	# Data prep goes through the CN device — the production path.
	write_range "$vm" "$WORK/pattern-$m.bin" "$dev" 128
	out=$(ctl "${MSRCDN[$m]}" get-side-info \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MSRCSIDE[$m]}")
	assert_eq "$(jq_of "$out" '.side_info.migr_src_info // "ABSENT"')" ABSENT \
		"migr $m baseline migr_src_info"
	assert_eq "$(jq_of "$out" '.side_info.migr_dst_info // "ABSENT"')" ABSENT \
		"migr $m baseline migr_dst_info"
}

# migr_declare_dst declares the destination and later enables it
# (dnagent_integtest.md, Cases, migr_full and migr_bitmap): the same request
# twice, first gated with sp_level no_migration (so bitmap chunks land before
# any region is copied), then at readwrite to build the clone. Passing the
# level in makes the two calls provably identical apart from it.
#
# The destination has already been provisioned by migr_provision_dst, so every
# call here carries --provisioned=true, because the worker's flag is monotone,
# false to true only (dnagent.md DN9). Re-sending false would close the
# provisioned gate, which skips the per-CN stacks and every step of the
# migration destination (dnagent.md DN10, DN13) and tears nothing down (DN10).
migr_declare_dst() { # m revision sp_level
	local m=$1 rev=$2 level=$3 out
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "${MCN[$m]}" \
		--sp-level "$level" --provisioned=true \
		--migr-dst "${MID[$m]}:${MSRCSIDE[$m]}:${DNID[${MSRCDN[$m]}]}" \
		--src-traddr "${IP[${MSRCDN[$m]}]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt "$BM_CNT")
	printf '%s' "$out" >"$REPLY_DIR/dst-$m.json"
	assert_ok "$out" ".side_info.side_dev_info.status" "migr $m dst side_dev"
	assert_cn_ok "$out" cn_id_to_dm_error "${MCN[$m]}" "migr $m dst"
	assert_cn_ok "$out" cn_id_to_dm_linear "${MCN[$m]}" "migr $m dst"
	assert_cn_ok "$out" cn_id_to_nvmeof "${MCN[$m]}" "migr $m dst"
	if [ "$level" = no_migration ]; then
		# DN11 suppression: wantMigr needs `level < SP_LEVEL_NO_MIGRATION`, so
		# at this level migr_dst_info is not emitted at all. assert_gated, not
		# assert_not_ok, so a PROVISIONING dst cannot satisfy it
		# (dnagent_integtest.md, Conventions, Negatives are exact).
		assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
			"migr $m gated clone"
		# The other half of the gated declaration's "no clone and no
		# connection" (dnagent_integtest.md, Cases, migr_full and migr_bitmap).
		# The clone and the connection to the source's :3: subsystem are built by
		# the same enabling converge, so a destination that connected while it
		# was still gated would be pulling data from a source whose bitmap chunks
		# may not all have landed — the race the staged flow exists to avoid.
		# The suppressed migr_dst_info cannot show it, so it is read off the
		# DNdst *host*: the agent connects as DnHostNqn(cluster, DNdst), so its
		# own node is where the controller would appear.
		local srcnqn
		srcnqn=$(migr_src_nqn "$CLUSTER" "${DNID[${MSRCDN[$m]}]}" \
			"$SP" "${MID[$m]}")
		assert_eq "$(helper "${MDSTDN[$m]}" \
			"host_subsys_present '$srcnqn'")" no \
			"migr $m: the gated dst already connected to $srcnqn"
	else
		assert_ok "$out" ".side_info.migr_dst_info.target_info.status" \
			"migr $m dst target"
		assert_ok "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
			"migr $m dst clone"
		# DN13 step 4: every dnv dm-clone carries no_discard_passdown, without
		# exception. The dn migration clone is the call site the rule fixed, and
		# without the feature the "mark this region hydrated" blkdiscard of
		# architecture.md, raid0 bitmap math, would also reach the destination
		# side device and destroy an acknowledged write.
		#
		# The whole feature list is pinned, not just the one word: the exact
		# `2 no_hydration no_discard_passdown` CloneTable (agent/dm.go) emits
		# from its derived feature count, which is the exact pair
		# dnagent_integtest.md, Cases, requires of the clone's table (DN13;
		# architecture.md, [D7]). `dmsetup message …
		# enable_hydration` does NOT weaken it — dm-clone's STATUSTYPE_TABLE
		# reprints the constructor args saved by copy_ctr_args verbatim
		# (drivers/md/dm-clone-target.c), and only `dmsetup status`
		# (STATUSTYPE_INFO) recomputes the live flags. The grep is scoped to
		# this migration's clone device by name, so nothing else on the node
		# can satisfy it. A substring test for no_discard_passdown alone
		# accepts `1 no_discard_passdown`, i.e. a clone created with hydration
		# already enabled, which would copy the very regions the skip bitmap
		# (architecture.md, raid0 bitmap math) asked to skip.
		local ctable
		ctable=$(helper "${MDSTDN[$m]}" \
			"clone_table '$(dn_clone_name "$CLUSTER" \
				"${DNID[${MDSTDN[$m]}]}" "$SP" "${MID[$m]}")'")
		printf '%s\n' "$ctable" |
			grep -qE ' 2 no_hydration no_discard_passdown( |$)' ||
			die "migr $m: dm-clone table is not '2 no_hydration no_discard_passdown': $ctable"
	fi
}

# migr_provision_dst is the dst half of the migration provisioning rule
# (architecture.md, Migration, Phase 0): the destination side provisions
# FIRST, under the ordinary side provisioning (architecture.md,
# Side provisioning protocol) — the dm-linear and the zeroing goroutine only,
# no per-CN stacks, no connect and no dm-clone. The request is byte-for-byte
# migr_declare_dst's gated one except --provisioned=false, which is what makes
# the gate provable.
migr_provision_dst() { # m revision
	local m=$1 rev=$2 out field left nqn
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "${MCN[$m]}" \
		--sp-level no_migration --provisioned=false \
		--migr-dst "${MID[$m]}:${MSRCSIDE[$m]}:${DNID[${MSRCDN[$m]}]}" \
		--src-traddr "${IP[${MSRCDN[$m]}]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt "$BM_CNT")
	assert_provisioning_or_ok "$out" ".side_info.side_dev_info.status" \
		"migr $m dst phase 1"
	# sp_level no_migration already suppresses the clone; the gate must not
	# turn that into anything else.
	assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
		"migr $m dst clone before provisioning"
	# The same three-map + kernel-side gate proof sync_side_2phase runs; this
	# request names one CN (the migration's primary), so the loop over the
	# request's CN ids collapses to it (dnagent_integtest.md, Conventions,
	# The provisioned flip).
	for field in cn_id_to_dm_error cn_id_to_dm_linear cn_id_to_nvmeof; do
		assert_provisioning "$out" \
			".side_info.$field[\"$((${MCN[$m]}))\"].status" \
			"migr $m dst must not export before it is provisioned: $field"
	done
	nqn=$(side_to_cn_nqn "$CLUSTER" "$SP" "${MLEG[$m]}" "${MCN[$m]}")
	assert_eq "$(helper "${MDSTDN[$m]}" "subsys_present '$nqn'")" no \
		"migr $m dst has a :2: subsystem before it is provisioned"
	left=$(helper "${MDSTDN[$m]}" \
		"export_dms $(hex16 "$SP") $(hex16 "${MDSTSIDE[$m]}")")
	[ -z "$left" ] ||
		die "migr $m dst has export dm devices before it is provisioned: $left"
	# Zeroed (dnagent_integtest.md, Conventions, The provisioned flip): the
	# exact equality against the --zero-bytes this request asked for, not
	# merely "whatever length its record happens to hold".
	wait_zeroed "${MDSTDN[$m]}" "$SP" "${MLEG[$m]}" "${MDSTSIDE[$m]}" \
		"$DATA_ZERO_BYTES"
}

migr_connect_dst() { # m
	local m=$1
	local vm=${MCNVM[$m]}
	# Same NQN, the other DN: the CN kernel merges the two connections into
	# one multipath namespace with two paths (dnagent_integtest.md, Topology).
	cn_connect "$vm" "${MDSTDN[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}"
	# Polled, like the other ANA checks that follow a connect: `nvme connect`
	# returns before the namespace scan it only queued has created the path's
	# device, and until then the path's ana_state reads `none`. Nothing moves
	# the destination's ANA group in this window, so a wrong state stays wrong
	# and still fails the poll.
	cn_wait_ana "$vm" "$SP" "${MLEG[$m]}" "${MCN[$m]}" "${MDSTDN[$m]}" \
		inaccessible 20
}

# migr_gate_src is the dst_provisioned = false half of the migration rule of
# architecture.md, Migration, and it runs before the cutover: the request is
# byte-for-byte migr_cutover_src's except --dst-provisioned=false, which the
# spec declares **exactly equivalent** to migr_src_conf being absent. The
# source keeps serving, does not fence its per-CN linears and exports no
# migr-src subsystem; only the would-be migr_src_info.* rows differ, reporting
# PROVISIONING.
#
# Without that equivalence the source would fence the primary's path the moment
# the migration was created, leaving the leg with no serving path for the whole
# destination zeroing window — which is exactly what this stage proves it does
# not do.
migr_gate_src() { # m revision
	local m=$1 rev=$2 out nqn susp dev
	out=$(ctl "${MSRCDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MSRCSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite --provisioned=true \
		--migr-src "${MID[$m]}:${MDSTSIDE[$m]}:${DNID[${MDSTDN[$m]}]}" \
		--dst-provisioned=false)
	assert_provisioning "$out" ".side_info.migr_src_info.dm_linear_info.status" \
		"migr $m gated src dm-linear"
	assert_provisioning "$out" ".side_info.migr_src_info.nvmeof_info.status" \
		"migr $m gated src export"
	# Untouched: the per-CN export stacks still serve.
	assert_cn_ok "$out" cn_id_to_dm_linear "${MCN[$m]}" "migr $m gated src"
	assert_cn_ok "$out" cn_id_to_nvmeof "${MCN[$m]}" "migr $m gated src"
	# Untouched: nothing is fenced and no migr-src subsystem exists.
	susp=$(helper "${MSRCDN[$m]}" "fenced_linears $(hex16 "$SP")")
	[ -z "$susp" ] ||
		die "migr $m: the gated src fenced its linears: $susp"
	nqn=$(migr_src_nqn "$CLUSTER" "${DNID[${MSRCDN[$m]}]}" "$SP" "${MID[$m]}")
	assert_eq "$(helper "${MSRCDN[$m]}" "subsys_present '$nqn'")" no \
		"migr $m: the gated src must export no migr-src subsystem"
	# Untouched: the CN keeps its optimized path to the source.
	assert_eq "$(cn_ana_state "${MCNVM[$m]}" "$SP" "${MLEG[$m]}" \
		"${MCN[$m]}" "${MSRCDN[$m]}")" optimized \
		"migr $m: the gated src keeps serving"
	# …and serving means data, not just an ANA state: an optimized path over a
	# per-CN linear that had been reloaded onto its dm-error would still read
	# optimized here and return EIO. The gate probe (dnagent_integtest.md,
	# Cases, migr_full and migr_bitmap) therefore reads 1 MiB through
	# the CN device, the same production path stage 0 wrote through. Preceded
	# by a cache drop so the read reaches the media (the dd lab fact of
	# dnagent_integtest.md, Assumptions and preflight checks: never iflag=).
	dev=$(ns_by_id "$SP" "${MLEG[$m]}")
	drop_caches "${MCNVM[$m]}"
	assert_eq "$(helper "${MCNVM[$m]}" "read_probe '$dev'")" ok \
		"migr $m: a 1 MiB read through the gated src must still succeed"
}

migr_cutover_src() { # m revision
	local m=$1 rev=$2 out
	out=$(ctl "${MSRCDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MSRCSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite --provisioned=true \
		--migr-src "${MID[$m]}:${MDSTSIDE[$m]}:${DNID[${MDSTDN[$m]}]}" \
		--dst-provisioned=true)
	assert_ok "$out" ".side_info.migr_src_info.dm_linear_info.status" \
		"migr $m src dm-linear"
	assert_ok "$out" ".side_info.migr_src_info.nvmeof_info.status" \
		"migr $m src export"
	# The per-CN linears enter the cutover grace window (architecture.md,
	# Migration, src side step 2): suspended in place for
	# SuspendSeconds, then reloaded onto their dm-errors. Observed, not
	# asserted — a slow step could push this past the window, and the
	# end state is what the teardown checks prove.
	local susp
	susp=$(helper "${MSRCDN[$m]}" "fenced_linears $(hex16 "$SP")")
	if [ -n "$susp" ]; then
		log "migr $m: cutover grace window HIT (suspended: $susp)"
	else
		log "migr $m: WARNING cutover grace window missed (already retired)"
	fi
	# From here the CN's IO is quiesced: the source is inaccessible and the
	# destination is not live yet, so the namespace briefly has no path.
	cn_wait_ana "${MCNVM[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}" \
		"${MSRCDN[$m]}" inaccessible 30
}

# migr_read_through is the read-through probe (dnagent_integtest.md, Cases,
# migr_full and migr_bitmap): the last must-copy MiB read through the
# destination. Correct either way — served by read-through from the source
# when hydration has not reached it — but the window is where the proof is
# strongest, so the sample is logged.
migr_read_through() { # m lastMiB
	local m=$1 skip=$2
	local vm=${MCNVM[$m]} dev out hydrated total want got
	dev=$(ns_by_id "$SP" "${MLEG[$m]}")
	out=$(ctl "${MDSTDN[$m]}" get-side-info \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}")
	local raw pair
	raw=$(jq_of "$out" '.side_info.migr_dst_info.dm_clone_info.details // ""')
	# `dmsetup status` of a dm-clone: <start> <len> clone <meta block size>
	# <used>/<total> <region size> <hydrated>/<total regions> ...
	pair=$(printf '%s' "$raw" | awk '{for (i = 1; i <= NF; i++) if ($i == "clone") { split($(i + 4), a, "/"); print a[1], a[2]; exit }}')
	hydrated=${pair%% *}
	total=${pair##* }
	if [ -n "$hydrated" ] && [ -n "$total" ] && [ "$hydrated" -lt "$total" ]; then
		log "migr $m: read-through window HIT ($hydrated/$total hydrated)"
	else
		log "migr $m: WARNING read-through window missed ($raw)"
	fi
	drop_caches "$vm"
	want=$(sha_range "$vm" "$WORK/pattern-$m.bin" 1 "$skip")
	got=$(sha_range "$vm" "$dev" 1 "$skip")
	assert_eq "$got" "$want" "migr $m read-through of MiB $skip"
}

# migr_first_hydr is case C's skip jump (dnagent_integtest.md, Cases,
# migr_bitmap), asserted on the first hydration sample there is. The enabling
# converge applies the pushed chunks, enables hydration and only then reads
# the clone's `dmsetup status` into its own reply, which migr_declare_dst
# filed under $REPLY_DIR — so the sample is read out of that file, not polled
# for. A poll starts after the destination's ANA wait and the read-through
# probe, when a loop device has often hydrated all 128 regions already, and a
# floor checked against 128/128 proves nothing.
migr_first_hydr() { # m
	local m=$1 out raw pair
	out=$(cat "$REPLY_DIR/dst-$m.json")
	raw=$(jq_of "$out" '.side_info.migr_dst_info.dm_clone_info.details // ""')
	# The same `dmsetup status` fields as migr_read_through, printed only
	# when both counts are numbers.
	pair=$(printf '%s' "$raw" | awk '{for (i = 1; i <= NF; i++) if ($i == "clone") { if (split($(i + 4), a, "/") == 2 && a[1] ~ /^[0-9]+$/ && a[2] ~ /^[0-9]+$/) print a[1], a[2]; exit }}')
	[ -n "$pair" ] ||
		die "migr $m: the enabling converge's reply carries no hydration count: '$raw'"
	log "migr $m: first hydration sample ${pair% *}/${pair#* } (the enabling converge's reply)"
	[ "${pair% *}" -ge "$MIN_FIRST" ] ||
		die "migr $m: first hydration sample is ${pair% *}/${pair#* }, want >= $MIN_FIRST hydrated"
}

migr_finish_dst() { # m revision
	local m=$1 rev=$2 out
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite --provisioned=true)
	# The request drops --migr-dst, so the whole migr_dst_info block goes away
	# with the clone (dnagent_integtest.md, Conventions, Negatives are exact:
	# gated, never merely "not OK").
	assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
		"migr $m clone after finish"
	assert_cn_ok "$out" cn_id_to_dm_linear "${MCN[$m]}" "migr $m finished dst"
	assert_cn_ok "$out" cn_id_to_nvmeof "${MCN[$m]}" "migr $m finished dst"
}

# migr_drop_src drops the source side and the dead source path
# (dnagent_integtest.md, Cases, migr_full and migr_bitmap; dnagent.md SH20):
# the source side leaves its DN's pointer list, which kills the CN's source
# controller with DNR. The host will not reconnect on its own, so the dead
# path is disconnected by device — by NQN would kill the surviving
# destination path too.
migr_drop_src() { # m revision
	local m=$1 rev=$2 out nqn ctrl
	migr_dn_sides "$SP" "${MSRCDN[$m]}" 0
	out=$(ctl "${MSRCDN[$m]}" syncup-dn --revision "$rev" \
		--extent-size "$EXTENT_SIZE" "${SIDE_ARGS[@]}")
	assert_dn_info_ok "$out" "migr $m src drop"
	nqn=$(side_to_cn_nqn "$CLUSTER" "$SP" "${MLEG[$m]}" "${MCN[$m]}")
	ctrl=$(helper "${MCNVM[$m]}" "ctrl_of '$nqn' '${IP[${MSRCDN[$m]}]}'")
	if [ "$ctrl" = none ]; then
		log "migr $m: the dead source controller is already gone"
	else
		sshv "${MCNVM[$m]}" "nvme disconnect -d /dev/$ctrl"
	fi
	cn_wait_ana "${MCNVM[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}" \
		"${MDSTDN[$m]}" optimized 30
}

# migr_write_probe proves the migrated side is live and writable.
migr_write_probe() { # m
	local m=$1
	local vm=${MCNVM[$m]} dev want got
	dev=$(ns_by_id "$SP" "${MLEG[$m]}")
	sshv "$vm" "dd if=/dev/urandom of=$WORK/probe-$m.bin bs=1M count=1 conv=fsync status=none"
	want=$(sha_range "$vm" "$WORK/probe-$m.bin" 1)
	write_range "$vm" "$WORK/probe-$m.bin" "$dev" 1 5
	drop_caches "$vm"
	got=$(sha_range "$vm" "$dev" 1 5)
	assert_eq "$got" "$want" "migr $m post-migration write probe"
	sshv_ok "$vm" "rm -f $WORK/probe-$m.bin"
}

# run_migration_cases is the migration body shared by cases B and C
# (dnagent_integtest.md, Cases). $SP, $MID, $BM_CNT and the case name are set
# by the callers; the per-case differences are the two hooks push_bitmaps and
# verify_data.
declare -a PAT_SHA=("" "" "")
declare -a PAT_SHA_HEAD=("" "" "")
REPLY_DIR=""

run_migration_cases() {
	local m out pids rev1 rev2 prov1 prov2 dn
	REPLY_DIR=$(mktemp -d)
	# A fresh case starts with no sides for the failure dump of
	# dnagent_integtest.md, Teardown and cleanup, to report; the sides
	# below are registered as each stage creates them.
	DIAG_SIDES=()

	stage stage0dn "both DNs learn all four side pointers"
	for dn in 1 2; do
		migr_dn_sides "$SP" "$dn" 1
		bump_dn_rev "$dn"
		out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
			--extent-size "$EXTENT_SIZE" "${SIDE_ARGS[@]}")
		assert_dn_info_ok "$out" "$CASE dn$dn pointers"
	done

	stage stage0src "both source sides provision and come up (concurrently)"
	# The sides exist from here on, so register them for the failure dump of
	# dnagent_integtest.md, Teardown and cleanup — in the
	# parent shell, like the revisions below, because a DIAG_SIDES entry
	# appended inside a background job would be appended to a copy.
	for m in 1 2; do
		diag_add_side "${MSRCDN[$m]}" "$SP" "${MLEG[$m]}" "${MSRCSIDE[$m]}"
	done
	# Two revisions per source: the phase-1 sync of architecture.md,
	# Side provisioning protocol, and the worker's flip. Both are minted here,
	# in the parent shell, because bump_rev inside a background job would
	# increment a copy (dnagent_integtest.md, Conventions, Revisions).
	bump_rev "${MSRCDN[1]}"
	prov1=${REV[${MSRCDN[1]}]}
	bump_rev "${MSRCDN[1]}"
	rev1=${REV[${MSRCDN[1]}]}
	bump_rev "${MSRCDN[2]}"
	prov2=${REV[${MSRCDN[2]}]}
	bump_rev "${MSRCDN[2]}"
	rev2=${REV[${MSRCDN[2]}]}
	pids=()
	migr_src_side 1 "$prov1" "$rev1" &
	pids+=($!)
	migr_src_side 2 "$prov2" "$rev2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage0data "128 MiB of random data written through each CN device"
	pids=()
	migr_prep_data 1 &
	pids+=($!)
	migr_prep_data 2 &
	pids+=($!)
	join_jobs "${pids[@]}"
	# The pattern hashes were computed inside the background jobs, so recompute
	# them in this shell — they are the oracle every later check compares to.
	for m in 1 2; do
		PAT_SHA[m]=$(sha_range "${MCNVM[$m]}" "$WORK/pattern-$m.bin" 128)
		PAT_SHA_HEAD[m]=$(sha_range "${MCNVM[$m]}" "$WORK/pattern-$m.bin" 64)
	done

	stage stage1prov "the destinations provision first (architecture.md, Side provisioning protocol): zero, then gate"
	for m in 1 2; do
		diag_add_side "${MDSTDN[$m]}" "$SP" "${MLEG[$m]}" "${MDSTSIDE[$m]}"
	done
	bump_rev "${MDSTDN[1]}"
	prov1=${REV[${MDSTDN[1]}]}
	bump_rev "${MDSTDN[2]}"
	prov2=${REV[${MDSTDN[2]}]}
	pids=()
	migr_provision_dst 1 "$prov1" &
	pids+=($!)
	migr_provision_dst 2 "$prov2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage1 "destinations declared, gated at sp_level no_migration"
	bump_rev "${MDSTDN[1]}"
	rev1=${REV[${MDSTDN[1]}]}
	bump_rev "${MDSTDN[2]}"
	rev2=${REV[${MDSTDN[2]}]}
	pids=()
	migr_declare_dst 1 "$rev1" no_migration &
	pids+=($!)
	migr_declare_dst 2 "$rev2" no_migration &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage1conn "the CNs add the destination path (still inaccessible)"
	pids=()
	migr_connect_dst 1 &
	pids+=($!)
	migr_connect_dst 2 &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage1bm "bitmap chunks and the equal-revision bm_info read-back"
	push_bitmaps "$rev1" "$rev2"

	stage stage2gate "dst_provisioned=false: the source is provably untouched"
	bump_rev "${MSRCDN[1]}"
	rev1=${REV[${MSRCDN[1]}]}
	bump_rev "${MSRCDN[2]}"
	rev2=${REV[${MSRCDN[2]}]}
	pids=()
	migr_gate_src 1 "$rev1" &
	pids+=($!)
	migr_gate_src 2 "$rev2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage2 "source cutover: the source hands IO over"
	bump_rev "${MSRCDN[1]}"
	rev1=${REV[${MSRCDN[1]}]}
	bump_rev "${MSRCDN[2]}"
	rev2=${REV[${MSRCDN[2]}]}
	pids=()
	migr_cutover_src 1 "$rev1" &
	pids+=($!)
	migr_cutover_src 2 "$rev2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage3 "both destinations enabled at once — the concurrency point"
	bump_rev "${MDSTDN[1]}"
	rev1=${REV[${MDSTDN[1]}]}
	bump_rev "${MDSTDN[2]}"
	rev2=${REV[${MDSTDN[2]}]}
	pids=()
	migr_declare_dst 1 "$rev1" readwrite &
	pids+=($!)
	migr_declare_dst 2 "$rev2" readwrite &
	pids+=($!)
	join_jobs "${pids[@]}"
	# Case C's skip jump (dnagent_integtest.md, Cases, migr_bitmap), read off
	# each enabling converge's reply.
	if [ "$MIN_FIRST" -gt 0 ]; then
		for m in 1 2; do migr_first_hydr "$m"; done
	fi

	stage stage3ana "the destination paths go optimized"
	pids=()
	(cn_wait_ana "${MCNVM[1]}" "$SP" "${MLEG[1]}" "${MCN[1]}" "${MDSTDN[1]}" \
		optimized 30) &
	pids+=($!)
	(cn_wait_ana "${MCNVM[2]}" "$SP" "${MLEG[2]}" "${MCN[2]}" "${MDSTDN[2]}" \
		optimized 30) &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage3read "mid-hydration read-through probe"
	pids=()
	migr_read_through 1 "$READ_THROUGH_MIB" &
	pids+=($!)
	migr_read_through 2 "$READ_THROUGH_MIB" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage3hydr "wait for full hydration"
	pids=()
	(ctl "${MDSTDN[1]}" wait-hydrated --sp "$SP" --leg "${MLEG[1]}" \
		--side "${MDSTSIDE[1]}" --interval 0.5 --timeout 120) &
	pids+=($!)
	(ctl "${MDSTDN[2]}" wait-hydrated --sp "$SP" --leg "${MLEG[2]}" \
		--side "${MDSTSIDE[2]}" --interval 0.5 --timeout 120) &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage4dst "finish the destinations first: clone flushed and removed"
	bump_rev "${MDSTDN[1]}"
	rev1=${REV[${MDSTDN[1]}]}
	bump_rev "${MDSTDN[2]}"
	rev2=${REV[${MDSTDN[2]}]}
	pids=()
	migr_finish_dst 1 "$rev1" &
	pids+=($!)
	migr_finish_dst 2 "$rev2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage stage4src "then tear the source sides down"
	bump_dn_rev "${MSRCDN[1]}"
	rev1=${REV[${MSRCDN[1]}]}
	bump_dn_rev "${MSRCDN[2]}"
	rev2=${REV[${MSRCDN[2]}]}
	pids=()
	migr_drop_src 1 "$rev1" &
	pids+=($!)
	migr_drop_src 2 "$rev2" &
	pids+=($!)
	join_jobs "${pids[@]}"

	stage verify "the migrated data, read through the surviving path"
	for m in 1 2; do
		drop_caches "${MCNVM[$m]}"
		verify_data "$m"
	done

	stage probe "post-migration write probe"
	for m in 1 2; do migr_write_probe "$m"; done

	stage teardown "disconnect the CNs and empty both side lists"
	for m in 1 2; do
		cn_disconnect "${MCNVM[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}"
	done
	for dn in 1 2; do
		bump_dn_rev "$dn"
		out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
			--extent-size "$EXTENT_SIZE")
		assert_dn_info_ok "$out" "$CASE teardown dn$dn"
		out=$(ctl "$dn" check-dn --revision "${DNREV[$dn]}" --show-info)
		assert_dn_info_ok "$out" "$CASE teardown check-dn$dn"
		assert_eq "$(jq_of "$out" '.revision // "0"')" "${DNREV[$dn]}" \
			"$CASE teardown check-dn$dn revision"
	done
	assert_no_residue "$SP"
	for m in 1 2; do
		sshv_ok "${MCNVM[$m]}" "rm -f $WORK/pattern-$m.bin"
	done
	# The sides are gone, so a later case's failure dump (dnagent_integtest.md,
	# Teardown and cleanup) must not ask for them.
	DIAG_SIDES=()
	rm -rf "$REPLY_DIR"
}

# ---------------------------------------------------------------------------
# Case B — migr_full (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------

case_migr_full() {
	CASE=migr_full
	SP=0xc1
	MID=("" 0x31 0x32)
	BM_CNT=0
	READ_THROUGH_MIB=127
	MIN_FIRST=0

	push_bitmaps() { # rev1 rev2 — the destinations' current revisions
		# No chunks at all (dnagent_integtest.md, Cases): the equal-revision
		# re-send of the gated request only proves the reply's applied set is
		# empty.
		local revs=("" "$1" "$2") m out
		for m in 1 2; do
			migr_declare_dst "$m" "${revs[$m]}" no_migration
			out=$(cat "$REPLY_DIR/dst-$m.json")
			assert_eq "$(jq_of "$out" '(.bm_info.bm_idx_list // []) | length')" \
				0 "migr $m bm_idx_list must be empty"
		done
	}

	verify_data() {
		local m=$1 got
		got=$(sha_range "${MCNVM[$m]}" "$(ns_by_id "$SP" "${MLEG[$m]}")" 128)
		assert_eq "$got" "${PAT_SHA[$m]}" "migr $m full-copy sha256"
		# Discard-based skipping must not happen without bitmaps. The
		# provisioning `blkdiscard --zeroout` of architecture.md,
		# Side provisioning protocol, targets the side device
		# (dnv-*-d4-*), never a dm-clone (dnv-*-d3-*), so it does not
		# match this filter either.
		local discards
		discards=$(helper "${MDSTDN[$m]}" any_clone_discards)
		[ -z "$discards" ] ||
			die "migr $m: unexpected dm-clone blkdiscard: $discards"
	}

	run_migration_cases
}

# ---------------------------------------------------------------------------
# Case C — migr_bitmap (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------
#
# 128 regions of 1 MiB, meta_blocks 3. Bits 0..60 = 0 (must copy) map to
# regions 3..63; bits 61..124 = 1 (skip) map to regions 64..127; bits 125..127
# are pad past the region count and the agent clips them. Wire 1 = skippable,
# LSB-first: byte 7 carries bits 61,62,63 (0xe0) and byte 15 bits 120..124
# (0x1f). The result is one clean 64 MiB boundary.

C_BM0=00000000000000e0
C_BM1=ffffffffffffff1f

case_migr_bitmap() {
	CASE=migr_bitmap
	SP=0xd1
	MID=("" 0x41 0x42)
	BM_CNT=2
	READ_THROUGH_MIB=63
	MIN_FIRST=64

	push_bitmaps() { # rev1 rev2 — the destinations' current revisions
		local revs=("" "$1" "$2") m out
		for m in 1 2; do
			# A push carries no revision: it is position-addressed data the
			# agent takes whenever it knows the migration (dnagent.md DN15).
			ctl "${MDSTDN[$m]}" push-migr-bm \
				--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
				--migr "${MID[$m]}" --bm-idx 0 --bitmap-hex "$C_BM0" >/dev/null
			ctl "${MDSTDN[$m]}" push-migr-bm \
				--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
				--migr "${MID[$m]}" --bm-idx 1 --bitmap-hex "$C_BM1" >/dev/null
			# Re-send the gated request verbatim (equal revision, idempotent)
			# purely to read the applied set back out of the reply.
			migr_declare_dst "$m" "${revs[$m]}" no_migration
			out=$(cat "$REPLY_DIR/dst-$m.json")
			assert_eq "$(jq_of "$out" '.bm_info.res_id')" "$((MID[m]))" \
				"migr $m bm_info.res_id"
			assert_eq "$(jq_of "$out" '.bm_info.bm_idx_list | @csv')" '0,1' \
				"migr $m bm_info.bm_idx_list"
		done
	}

	verify_data() {
		local m=$1
		local vm=${MCNVM[$m]} dev got discards want srctail
		dev=$(ns_by_id "$SP" "${MLEG[$m]}")
		# Layer 1a: the meta region and every must-copy region came across.
		got=$(sha_range "$vm" "$dev" 64)
		assert_eq "$got" "${PAT_SHA_HEAD[$m]}" "migr $m first 64 MiB"
		# Layer 1b: the skipped half differs from the source's, which holds
		# this case's fresh random data there — the agent skipped it, it did
		# not copy it. What the destination reads there is whatever its
		# extents held before, as provisioning zeroes a data group's side
		# only up to the end of its first data block (architecture.md, Side
		# provisioning protocol), so the proof is the difference and never a
		# content.
		got=$(sha_range "$vm" "$dev" 64 64)
		srctail=$(sha_range "$vm" "$WORK/pattern-$m.bin" 64 64)
		[ "$got" != "$srctail" ] ||
			die "migr $m: the skipped second 64 MiB holds the source's data"
		# Layer 4: the meta_blocks arithmetic, read straight off the log.
		# First skip bit 61 -> region 3+61 = 64 -> offset 64 MiB; a run of 64
		# bits -> length 64 MiB. A wrong meta_blocks shifts the offset.
		local clone
		clone=$(dn_clone_name "$CLUSTER" "${DNID[${MDSTDN[$m]}]}" "$SP" "${MID[$m]}")
		discards=$(helper "${MDSTDN[$m]}" "clone_discards '$clone'")
		want="--offset 67108864 --length 67108864 /dev/mapper/$clone"
		assert_eq "$(printf '%s\n' "$discards" | grep -c . || true)" 1 \
			"migr $m blkdiscard record count on $clone"
		assert_eq "$discards" "$want" "migr $m blkdiscard range"
	}

	run_migration_cases
}

# ---------------------------------------------------------------------------
# Case E — teardown (architecture.md, Teardown by sweep; DN6)
# ---------------------------------------------------------------------------
#
# Both stages pin the same claim: removal is derived from what the node
# ACTUALLY holds minus what the desired state wants, so a teardown that cannot
# finish REPORTS what is left — an accepted reply carrying the leftover code 4
# and naming the objects — and the next pass of the same revision finishes it.
# A teardown that forgot the object together with the plan that named it
# would leave nothing to look again.
#
# `dead_source` is the DN's own shape of that forgetting. A migration
# destination hydrates through an nvme connection to its source; the source
# export is then yanked with the destination never told, and the destination
# is torn down immediately. The order the sweep has to keep is the whole
# point: the dm-clone comes off BEFORE the connection it hydrates through
# (DN6) — pulling the source out from under a live clone strands its IO — and
# it comes off over a source that is already dead, so its removal blocks on
# the queued hydration IO until fast_io_fail_tmo fails it, which is longer
# than the soft timeout that kills the command. That is why the drop is a
# loop and not a single call. What may not happen in that window is any
# forgetting: the side's allocation record and the migration's clone-metadata
# record are freed only after their devices have been PROBED gone, so the sp's
# extents cannot be handed to the next side while a device still maps them.
#
# `pinned_side` pins the device-level half of the same rule with no remote
# involved at all: an fd held outside the agent makes exactly one removal fail
# for a reason the agent cannot argue with, and the stage reads the reply. The
# record-level half — a record is never freed while its device still exists —
# is pinned by the unit test TestSideRecordFreedOnlyAfterDeviceGone, which can
# script a killed `dmsetup remove`; this stage cannot and does not try.
#
# pinned_side runs second for one reason: it is the only stage here that can
# leave a device pinned when it fails, so nothing after it in this case has to
# work around one. A pin a failed run leaves behind is killed unconditionally
# by the next run's start-of-run cleanup, which is why cleanup kills pins at
# all.

case_teardown() {
	CASE=teardown
	# A migration case's globals: the shared migr_* helpers read $SP, $MID and
	# $BM_CNT, and migr_declare_dst files its reply under $REPLY_DIR. The ids
	# are this case's own, so a failure never leaves debris another case's
	# residue assertions would report.
	SP=0xf1
	MID=("" 0x61 0x62)
	BM_CNT=0
	REPLY_DIR=$(mktemp -d)
	DIAG_SIDES=()

	teardown_dead_source
	teardown_pinned_side

	rm -rf "$REPLY_DIR"
}

# teardown_dead_source builds one migration exactly as case B does, kills the
# source export behind the destination's back, and tears the destination down
# inside the window where its clone's hydration IO is still queued at a
# controller that will never answer.
teardown_dead_source() {
	local m=1
	local src=${MSRCDN[$m]} dst=${MDSTDN[$m]} vm=${MCNVM[$m]}
	local out provrev rev srcnqn got raw pair hydrated total idx

	stage dsptr "the two DNs learn this migration's side pointers"
	# One migration, not case B/C's opposite-direction pair: this case is
	# about what one destination's teardown does, and a second migration
	# converging on the same nodes would only make the failure harder to read.
	bump_dn_rev "$src"
	out=$(ctl "$src" syncup-dn --revision "${REV[$src]}" \
		--extent-size "$EXTENT_SIZE" \
		--side "$SP:${MLEG[$m]}:${MSRCSIDE[$m]}")
	assert_dn_info_ok "$out" "teardown dn$src pointers"
	bump_dn_rev "$dst"
	out=$(ctl "$dst" syncup-dn --revision "${REV[$dst]}" \
		--extent-size "$EXTENT_SIZE" \
		--side "$SP:${MLEG[$m]}:${MDSTSIDE[$m]}")
	assert_dn_info_ok "$out" "teardown dn$dst pointers"

	stage dssrc "the source side provisions two-phase and exports"
	diag_add_side "$src" "$SP" "${MLEG[$m]}" "${MSRCSIDE[$m]}"
	bump_rev "$src"
	provrev=${REV[$src]}
	bump_rev "$src"
	rev=${REV[$src]}
	migr_src_side "$m" "$provrev" "$rev"

	stage dsdata "128 MiB written through the CN device, as case B does"
	migr_prep_data "$m"

	stage dsprov "the destination provisions first (architecture.md, Side provisioning protocol)"
	diag_add_side "$dst" "$SP" "${MLEG[$m]}" "${MDSTSIDE[$m]}"
	bump_rev "$dst"
	migr_provision_dst "$m" "${REV[$dst]}"

	stage dsgate "the destination is declared, gated at sp_level no_migration"
	bump_rev "$dst"
	migr_declare_dst "$m" "${REV[$dst]}" no_migration

	stage dsconn "the CN adds the destination path (still inaccessible)"
	migr_connect_dst "$m"

	stage dscut "source cutover: the source hands the leg over"
	# The dst_provisioned=false gate of the migration cases
	# (dnagent_integtest.md, Cases) is case B's proof, not this one's: the
	# only thing needed here is the state the cutover leaves behind.
	bump_rev "$src"
	migr_cutover_src "$m" "${REV[$src]}"

	stage dshydr "the destination is enabled and hydrates from the source"
	bump_rev "$dst"
	migr_declare_dst "$m" "${REV[$dst]}" readwrite
	cn_wait_ana "$vm" "$SP" "${MLEG[$m]}" "${MCN[$m]}" "$dst" optimized 30
	# Observed, never asserted (dnagent_integtest.md, Conventions), exactly
	# like the migration cases' read-through window: 128
	# regions of 1 MiB over a local TCP link can be through before this
	# samples. Hydration still in flight is what makes the clone's removal
	# below block on the dead source; hydration already finished still
	# exercises the removal order and both record releases. Both are a pass,
	# and the log says which of the two this run got.
	out=$(ctl "$dst" get-side-info \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}")
	raw=$(jq_of "$out" '.side_info.migr_dst_info.dm_clone_info.details // ""')
	pair=$(printf '%s' "$raw" | awk '{for (i = 1; i <= NF; i++) if ($i == "clone") { split($(i + 4), a, "/"); print a[1], a[2]; exit }}')
	hydrated=${pair%% *}
	total=${pair##* }
	if [ -n "$hydrated" ] && [ -n "$total" ] && [ "$hydrated" -lt "$total" ]; then
		log "teardown: hydration window HIT ($hydrated/$total hydrated)"
	else
		log "teardown: WARNING hydration window missed ($raw)"
	fi

	stage dskill "the source export is yanked; the destination is never told"
	# drop_subsystems, not a syncup-side with migr_src_conf dropped: that
	# would be an ORDERLY retirement — the source would take its own migr-src
	# linear with it and report its own leftovers, and a code 4 on that reply
	# would fail this stage for something that is not the destination's
	# business. What the destination has to survive is an export that is
	# simply gone, which is what this verb leaves behind, and it is
	# best-effort, so nothing here can fail for the wrong reason.
	helper "$src" "drop_subsystems 3"
	srcnqn=$(migr_src_nqn "$CLUSTER" "${DNID[$src]}" "$SP" "${MID[$m]}")
	assert_eq "$(helper "$src" "subsys_present '$srcnqn'")" no \
		"teardown: the source export survived drop_subsystems"

	stage dsdrop "and IMMEDIATELY the destination side leaves the pointer list"
	# Nothing sleeps between the yank and this drop: the dm-clone has to be
	# removed inside the window where its hydration IO to the dead source is
	# still queued, which is the window the sweep is written for (the failfast
	# bound of architecture.md, Teardown by sweep). The loop is what lets the
	# pass that is killed at the soft timeout be followed by one that finds the
	# device gone.
	bump_dn_rev "$dst"
	dn_drop_until_clean "$dst" 60

	stage dsgone "nothing of the migration is left on the destination"
	for idx in 1 2; do
		got=$(helper "$idx" "dm_kind_names d3")
		[ -z "$got" ] || die "vm$idx still holds a dm-clone: $got"
		got=$(helper "$idx" "dm_kind_names d5")
		[ -z "$got" ] ||
			die "vm$idx still holds a clone-metadata wrapper: $got"
		assert_eq "$(helper "$idx" "subsys_present '$srcnqn'")" no \
			"vm$idx still exports the migration source"
		# The connection the clone hydrated through. "Gone" is the absence of
		# a CONTROLLER, which is what the agent's own probe asks (dnagent.md
		# DN6): the kernel keeps a subsystem's directory after its last
		# controller while something holds its multipath head open.
		assert_eq "$(helper "$idx" "ctrl_of '$srcnqn' '${IP[$src]}'")" none \
			"vm$idx still holds a controller to the dead source"
	done
	got=$(helper "$dst" "residue $(hex16 "$SP")")
	[ -z "$got" ] || die "dn$dst still holds objects of sp $SP: $got"
	# Both records are gone, and this is the proof: an allocation or
	# clone-metadata record whose owner left the authoritative lists is a
	# leftover in its own right, so the read-only verdict a Get*Info takes
	# would answer 4 while either one survived. ctl enforces the 0 itself.
	ctl "$dst" get-dn-info >/dev/null

	stage dssrcdrop "the source side goes too, taking the orphaned d2 with it"
	# The source was never told its migration ended, so its migr-src linear is
	# still wanted by its own desired state and could not have been in the
	# sweep above. It goes when the side does — the same sweep, on the other
	# node — which is why the d2 assertion lives here and not in dsgone.
	cn_disconnect "$vm" "$SP" "${MLEG[$m]}" "${MCN[$m]}"
	bump_dn_rev "$src"
	dn_drop_until_clean "$src" 60
	for idx in 1 2; do
		got=$(helper "$idx" "dm_kind_names d2")
		[ -z "$got" ] || die "vm$idx still holds a migr-src linear: $got"
	done
	assert_no_residue "$SP"
	ctl "$src" get-dn-info >/dev/null
	sshv_ok "$vm" "rm -f $WORK/pattern-$m.bin"
	# Both sides are gone, so the failure dump of dnagent_integtest.md,
	# Teardown and cleanup, must not ask for them again.
	DIAG_SIDES=()
}

# teardown_pinned_side holds one side device open from outside the agent and
# drops the side. The removal cannot succeed while the fd is there, and the
# stage asserts what the agent does about that: it reports the device, it
# keeps reporting it on the read-only path, and it finishes the job on the
# next pass of the same revision once the fd is gone.
teardown_pinned_side() {
	local dn=2 sp=0xf2 leg=0x1 side=0x11 cn=0x21
	local out provrev siderev name details

	stage pinptr "dn$dn learns one plain side pointer"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE" --side "$sp:$leg:$side")
	assert_dn_info_ok "$out" "teardown pinned pointers"

	stage pinside "the side provisions two-phase and exports"
	diag_add_side "$dn" "$sp" "$leg" "$side"
	bump_rev "$dn"
	provrev=${REV[$dn]}
	bump_rev "$dn"
	siderev=${REV[$dn]}
	sync_side_2phase "$dn" "$provrev" "$siderev" "$sp" "$leg" "$side" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "teardown pinned side_dev"
	# No CN connects: a removal that fails on a held fd needs no remote at
	# all, and leaving the export unconnected keeps the only thing in this
	# stage that can block the sweep the pin itself.

	stage pinhold "an fd outside the agent holds the side device open"
	name=$(dn_side_name "$CLUSTER" "${DNID[$dn]}" "$sp" "$side")
	helper "$dn" "pin_dev '$name'"

	stage pindrop "the drop cannot remove it: code 4, and the reply names it"
	bump_dn_rev "$dn"
	out=$(ctl "$dn" syncup-dn --revision "${REV[$dn]}" \
		--extent-size "$EXTENT_SIZE" --expect-code 4)
	# A leftover is an ACCEPTED request: the desired state was stored and the
	# DN's own base state converged, so the node rows are OK all the same.
	assert_dn_info_ok "$out" "teardown pinned drop"
	details=$(jq_of "$out" '.agent_reply.details // ""')
	case "$details" in
	*"$name"*) ;;
	*) die "the leftover details do not name $name: '$details'" ;;
	esac
	# Everything above the side device came off in the same pass — the export,
	# the per-CN dm-linear and the per-CN dm-error — so what is left is
	# EXACTLY the pinned device. A sweep that stopped at its first failure, or
	# one that never reached the layers above, would leave more than this.
	assert_eq "$(helper "$dn" "residue $(hex16 "$sp")")" "$name" \
		"the residue while the side device is pinned"
	# The verdict is recomputed, never stored: the read-only path enumerates
	# the node again and has to reach the same answer.
	ctl "$dn" get-dn-info --expect-code 4 >/dev/null

	stage pinfree "the fd goes; the same revision, re-sent, finishes the job"
	helper "$dn" "unpin_dev '$name'"
	dn_drop_until_clean "$dn" 30
	assert_no_residue "$sp"
	# And the record went with the device: an orphan side record would make
	# this verdict 4 exactly as the pinned device did.
	ctl "$dn" get-dn-info >/dev/null
	DIAG_SIDES=()
}

# ---------------------------------------------------------------------------
# Case D — restart (dnagent_integtest.md, Cases)
# ---------------------------------------------------------------------------

case_restart() {
	CASE=restart
	local sp=0xe1 leg=0x1 srcside=0x11 dstside=0x12 migr=0x51
	local cn=0x21 cnvm=2
	local out dev want snap idx provrev

	snap=$(mktemp -d)

	stage build "an exported side on dn1 and a gated migration dst on dn2"
	bump_dn_rev 1
	out=$(ctl 1 syncup-dn --revision "${REV[1]}" \
		--extent-size "$EXTENT_SIZE" --side "$sp:$leg:$srcside")
	assert_dn_info_ok "$out" "restart dn1 pointers"
	bump_dn_rev 2
	out=$(ctl 2 syncup-dn --revision "${REV[2]}" \
		--extent-size "$EXTENT_SIZE" --side "$sp:$leg:$dstside")
	assert_dn_info_ok "$out" "restart dn2 pointers"

	bump_rev 1
	provrev=${REV[1]}
	bump_rev 1
	sync_side_2phase 1 "$provrev" "${REV[1]}" "$sp" "$leg" "$srcside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "restart src side_dev"
	assert_cn_ok "$out" cn_id_to_nvmeof "$cn" "restart src"

	bump_rev 2
	provrev=${REV[2]}
	bump_rev 2
	sync_side_2phase 2 "$provrev" "${REV[2]}" "$sp" "$leg" "$dstside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "restart dst side_dev"
	assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
		"restart gated clone"

	stage data "the CN connects and writes its 4 MiB of test data"
	cn_connect "$cnvm" 1 "$sp" "$leg" "$cn"
	dev=$(ns_by_id "$sp" "$leg")
	helper "$cnvm" "wait_dev '$dev' 20" || die "restart: no $dev on vm$cnvm"
	cn_wait_ana "$cnvm" "$sp" "$leg" "$cn" 1 optimized 20
	sshv "$cnvm" "dd if=/dev/urandom of=$WORK/pattern-restart.bin bs=1M count=4 conv=fsync status=none"
	want=$(sha_range "$cnvm" "$WORK/pattern-restart.bin" 4)
	write_range "$cnvm" "$WORK/pattern-restart.bin" "$dev" 4

	stage bitmap "one chunk pushed to the gated destination"
	ctl 2 push-migr-bm \
		--sp "$sp" --leg "$leg" --side "$dstside" \
		--migr "$migr" --bm-idx 0 --bitmap-hex "$C_BM0" >/dev/null

	stage snapshot "record the pre-restart infos and the applied bitmap set"
	ctl 1 get-dn-info >"$snap/dn1.pre.raw"
	ctl 2 get-dn-info >"$snap/dn2.pre.raw"
	ctl 1 get-side-info --sp "$sp" --leg "$leg" --side "$srcside" \
		>"$snap/side1.pre.raw"
	ctl 2 get-side-info --sp "$sp" --leg "$leg" --side "$dstside" \
		>"$snap/side2.pre.raw"
	out=$(ctl 2 syncup-side --revision "${REV[2]}" \
		--sp "$sp" --leg "$leg" --side "$dstside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--provisioned=true \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1)
	assert_eq "$(jq_of "$out" '.bm_info.bm_idx_list | @csv')" '0' \
		"restart pre-restart bm_idx_list"

	stage restart "kill both agents; the kernel-side state is untouched"
	for idx in 1 2; do
		sshv "$idx" "pkill -x dnv-agent; for i in 1 2 3 4 5 6 7 8 9 10; do pgrep -x dnv-agent >/dev/null || break; sleep 0.5; done; pgrep -x dnv-agent >/dev/null && echo STILL_RUNNING || echo stopped"
	done
	# The data path does not depend on the agent process.
	drop_caches "$cnvm"
	assert_eq "$(sha_range "$cnvm" "$dev" 4)" "$want" \
		"restart readback while the agents are down"
	for idx in 1 2; do
		sshv "$idx" "mv $WORK/agent.log $WORK/agent.pre-restart.log"
		start_agent "$idx"
	done
	for idx in 1 2; do
		out=$(ctl "$idx" get-dn-size --wait 20)
		assert_eq "$(jq_of "$out" .size)" "$DATA_SIZE" "restart dn$idx size"
	done
	drop_caches "$cnvm"
	assert_eq "$(sha_range "$cnvm" "$dev" 4)" "$want" \
		"restart readback after the agents are back"

	stage reconcile "the reloaded state deep-equals the pre-restart snapshot"
	# ResInfo.epoch is the time of the last observed status change and the
	# trackers are in-memory, so it legitimately differs after a restart.
	local norm='walk(if type == "object" and has("epoch") then del(.epoch) else . end)'
	ctl 1 get-dn-info >"$snap/dn1.post.raw"
	ctl 2 get-dn-info >"$snap/dn2.post.raw"
	ctl 1 get-side-info --sp "$sp" --leg "$leg" --side "$srcside" \
		>"$snap/side1.post.raw"
	ctl 2 get-side-info --sp "$sp" --leg "$leg" --side "$dstside" \
		>"$snap/side2.post.raw"
	local name when
	for name in dn1 dn2 side1 side2; do
		for when in pre post; do
			"$JQ" "$norm" "$snap/$name.$when.raw" >"$snap/$name.$when.json"
		done
		diff -u "$snap/$name.pre.json" "$snap/$name.post.json" >&2 ||
			die "restart: $name differs across the restart"
	done

	stage bmreload "the persisted chunk survived"
	out=$(ctl 2 syncup-side --revision "${REV[2]}" \
		--sp "$sp" --leg "$leg" --side "$dstside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--provisioned=true \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1)
	assert_eq "$(jq_of "$out" '.bm_info.bm_idx_list | @csv')" '0' \
		"restart post-restart bm_idx_list"

	stage idempotent "unchanged re-applies must mutate nothing"
	# The restart case's mutation-free re-sends (dnagent_integtest.md, Cases):
	# the syncup-side re-sends are equal-revision, the syncup-dn ones
	# higher-revision — the counter is two past what the setup stored.
	DNREV[1]=${REV[1]}
	out=$(ctl 1 syncup-dn --revision "${REV[1]}" --extent-size "$EXTENT_SIZE" \
		--side "$sp:$leg:$srcside")
	assert_dn_info_ok "$out" "restart re-apply dn1"
	DNREV[2]=${REV[2]}
	out=$(ctl 2 syncup-dn --revision "${REV[2]}" --extent-size "$EXTENT_SIZE" \
		--side "$sp:$leg:$dstside")
	assert_dn_info_ok "$out" "restart re-apply dn2"
	out=$(ctl 1 syncup-side --revision "${REV[1]}" \
		--sp "$sp" --leg "$leg" --side "$srcside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite \
		--provisioned=true)
	assert_ok "$out" ".side_info.side_dev_info.status" "restart re-apply side1"
	out=$(ctl 2 syncup-side --revision "${REV[2]}" \
		--sp "$sp" --leg "$leg" --side "$dstside" \
		--ext-cnt 1 --zero-bytes "$DATA_ZERO_BYTES" \
		--cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--provisioned=true \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1)
	assert_ok "$out" ".side_info.side_dev_info.status" "restart re-apply side2"
	# The post-restart log covers the startup reconcile and these re-applies.
	# This is also the resume-at-k net (dnagent.md DN9): `blkdiscard` is in
	# mutations()' verb list, so a reconcile that re-zeroes an already-complete
	# side — the resume logic reading its stored count wrong — fails the case
	# here.
	for idx in 1 2; do
		local muts
		muts=$(helper "$idx" mutations)
		[ -z "$muts" ] ||
			die "restart: dn$idx mutated after the restart:"$'\n'"$muts"
	done

	stage stale "a stale SyncupDn is refused after the restart"
	ctl 1 syncup-dn --revision "$((REV[1] - 1))" \
		--extent-size "$EXTENT_SIZE" --side "$sp:$leg:$srcside" \
		--expect-code 1 >/dev/null

	stage teardown "disconnect, empty both side lists, check the residue"
	cn_disconnect "$cnvm" "$sp" "$leg" "$cn"
	for idx in 1 2; do
		bump_dn_rev "$idx"
		out=$(ctl "$idx" syncup-dn --revision "${REV[$idx]}" \
			--extent-size "$EXTENT_SIZE")
		assert_dn_info_ok "$out" "restart teardown dn$idx"
	done
	assert_no_residue "$sp"
	sshv_ok "$cnvm" "rm -f $WORK/pattern-restart.bin"
	rm -rf "$snap"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/dnagent_test.sh [--only <case>] [--cleanup-only] \\
           [--wipe] <user@vm1> <user@vm2>

cases: ${CASES[*]}

--wipe is the ONE-TIME lab wipe: it removes EVERY dnv object on both VMs,
whatever dm kind spelling its name carries, then runs the ordinary cleanup.
It runs no case. Never run it while another suite is using these VMs.
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
