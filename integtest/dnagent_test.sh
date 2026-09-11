#!/usr/bin/env bash
#
# dnagent_test.sh — the `dnv-agent dn` integration test of
# doc/dnagent_integtest.md. Two real VMs, real dm/nvmet/nvme-tcp, driven
# over gRPC from this machine by integtest/dnagentctl.
#
#   bash integtest/dnagent_test.sh [--only <case>] [--cleanup-only] \
#       user1@ip1 user2@ip2
#
# Cases: smoke, sides, migr_full, migr_bitmap, restart (§10-§15). Cleanup runs
# unconditionally at the start and, on success only, at the end: a failing run
# leaves every dm/nvmet object and both agent logs in place and dumps
# diagnostics (§17).
#
# The uutils dd rule of §4 is absolute: this script never passes iflag= or
# oflag= to dd. Writes use conv=fsync, reads that must hit the media are
# preceded by a cache drop. Do not "fix" this back to direct IO.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (§3, §5, §6)
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
EXTENT_SIZE=67108864 # 64 MiB — MinDnExtSize; the proto field is raw bytes
# GetDnSize reports the [D13] *data area*, not the raw device: the fixed
# DnDataOffset (256 MiB) prefix — header block, the two volume-table slots and
# the clone-metadata area — is already subtracted, so the CP does no further
# subtraction (§6.1). 2 GiB backing file: 2147483648 - 268435456 = 1879048192.
DATA_SIZE=1879048192 # the exact GetDnSize the setup asserts

# Migration knobs, mirroring the CP defaults (§6).
BLOCK_SIZE=1048576
META_BLOCKS=3
HYDR_THRESHOLD=1
HYDR_BATCH=1

# Per-RPC deadline for the converge RPCs (see ctl).
SYNCUP_TIMEOUT=60

# Polling budget of `dnagentctl wait-zeroed` (the §9.4 protocol). With 64 MiB
# extents on a loop device the kernel maps REQ_OP_WRITE_ZEROES onto fallocate,
# so a 1-2 extent side finishes in well under a second; the budget only has to
# cover a stalled retry loop (DnZeroRetryInterval = 5 s).
ZERO_TIMEOUT=120

NQN_PREFIX=nqn.2024-01.io.dnv

# Per-VM state, 1-indexed so "vm1"/"vm2" read directly.
VM=("" "" "")
IP=("" "" "")
DNID=("" 0x1 0x2)
REV=("" 0 0)
DNREV=("" 0 0)
LOOP=("" "" "")

# The two concurrent, opposite-direction migrations of cases B and C (§5).
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
CASE="setup"
TRACE="it-setup"
STAGE="(startup)"
SETUP_DONE=0

CASES=(smoke sides migr_full migr_bitmap restart)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id the §9
# convention asks for: one id shared by the driver call, both agent handlers
# and every os command they run.
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

# assert_gated is assert_not_ok's strict twin (ruling R4.35).
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
# provisioned = false (the §9.4 converge matrix rows 2 and 3):
# PROVISIONING while the background goroutine still has extents to zero, and OK
# once every bit is set. Zeroing 64-128 MiB on a loop device is a `fallocate`,
# so which of the two a phase-1 reply carries is a genuine race — do not pick
# one.
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
		log "########## diagnostics (§17) ##########"
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

# ctl drives one agent. Every call carries the current stage's trace id.
#
# The §8 default 10 s deadline suits the read-only RPCs, but one converge pass
# runs dozens of OS commands, each with its own 3 s soft / 5 s hard budget
# (§7) — enabling a migration destination alone creates a clone-metadata
# wrapper, an nvme connection, a dm-clone and reloads the export stack, with
# the two concurrent migrations converging on the node at once. Syncups get
# a much larger budget; a caller's own --timeout still wins, since it lands
# later on the command line.
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

# bump_rev advances a DN's monotonic revision counter (§9). It must run in
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
# Derived names (§5) — the bash mirror of common/name_fmt.go
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
	printf 'dnv-%s-%s-3-%s-%s' \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")" "$(hex16 "$4")"
}

# ns_by_id is the CN-side device node of a leg: the namespace identity is
# deterministic per leg, so both sides of a migrating leg land on one
# multipath device (§5).
ns_by_id() { "$CTL" ns-id --cluster "$CLUSTER" --sp "$1" --leg "$2" | "$JQ" -r .by_id; }

# ---------------------------------------------------------------------------
# CN emulation (§3): the cn role is unimplemented, so CN identities are plain
# `nvme connect --hostnqn <CnHostNqn>` from the VMs, cross-connected.
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
# Data IO on CNs (§9): writes fsync, reads follow a cache drop, never
# iflag=/oflag= (§4).
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
# Shipped by integtest/dnagent_test.sh. Every function is best-effort by
# design: cleanup must survive a crashed prior run.
WORK=/var/tmp/dnv-integtest
NVMET=/sys/kernel/config/nvmet
NQN_PREFIX=nqn.2024-01.io.dnv

subsys_json() {
	local json
	json=$(nvme list-subsys -o json 2>/dev/null)
	[ -n "${json//[[:space:]]/}" ] || json='[]'
	printf '%s' "$json"
}

# path_field <nqn> <traddr> <field> — one field of the path a host holds to
# one target address, e.g. State (live/connecting) or ANAState.
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
		[ -e "$1" ] || { echo gone; return 0; }
		sleep 0.5
	done
	echo "$1 still present after $2s" >&2
	return 1
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
# (the dst_provisioned = false equivalence proof, §11.2) does not
# have to parse `residue`.
subsys_present() {
	if [ -d "$NVMET/subsystems/$1" ]; then echo yes; else echo no; fi
}

# host_subsys_present <nqn> — yes/no for the *host* half of the same question:
# does this node hold a controller for one subsystem NQN, at any address?
# subsys_present reads configfs, i.e. what this node exports; this reads the
# nvme driver's own view, i.e. what this node has connected to. It is how the
# "no `nvme connect` issued yet" clause of §12 step 6c is proved on DNdst,
# where the gated reply carries no migr_dst_info at all and so cannot show it.
host_subsys_present() {
	local n
	n=$(subsys_json | jq -r --arg nqn "$1" '
	    [ .. | objects | select(has("NQN") and .NQN == $nqn) ] | length')
	if [ "${n:-0}" -gt 0 ]; then echo yes; else echo no; fi
}

# residue <sp16> — everything still on this node for one storage pool; the
# teardown assertions require empty output.
# The dm-name pattern covers the side device too, now that it is dm kind 4.
residue() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[0-9a-f]-$1-" || true
	ls "$NVMET/subsystems" 2>/dev/null | grep -E ":$1:" || true
}

# export_dms <sp16> <side16> — the per-CN *export stack* one side currently
# has on this node: the dm-error (kind 0, DnErrorName) and the dm-linear
# (kind 1, DnLinearName), which are the only two dm kinds that exist per CN.
# It is the kernel-side half of the §9.4 provisioning gate — nothing is exported
# before the side is fully zeroed — so it deliberately does NOT match kind 4
# (DnSideName): the side device is exactly what phase (a) is supposed to
# build, and `residue` would report it. See common/name_fmt.go:11-15 for the
# kind digits and DnErrorName/DnLinearName for the field order,
# dnv-<cluster16>-<dn16>-<kind>-<sp16>-<side16>-<cn16>.
#
# The scope is the side, not the storage pool: cases A and B/C provision the
# sides of one sp one after another, so a pool-wide pattern would match a
# sibling side's already-live export and fail a correct run.
export_dms() { # sp16 side16
	agent_dm_names |
		awk -F- -v sp="$1" -v side="$2" \
			'($4 == "0" || $4 == "1") && $5 == sp && $6 == side'
}

# fenced_linears <sp16> — the per-CN dm-linears of one storage pool that are
# currently suspended, i.e. inside the §11.2 cutover grace window. The attr
# column is four positions (live, inactive, suspended, ro/rw), so a suspended
# device matches ':.-s' — name, then '.', '-', 's'.
fenced_linears() {
	dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E "^dnv-[0-9a-f]{16}-[0-9a-f]{16}-1-$1-.*:.-s" || true
}

# clone_table <clone dm name> — the live dm-clone table, so the mandatory feature pair
# can be asserted against a real kernel and not only in the unit tests.
clone_table() { dmsetup table "$1" 2>/dev/null || echo MISSING; }

# clone_discards <clone dm name> — the blkdiscard records the agent logged
# against one dm-clone device (§14 layer 4).
clone_discards() {
	jq -r --arg dev "/dev/mapper/$1" '
	    select(.msg == "os command" and .cmd == "blkdiscard")
	    | select(.args | index($dev)) | (.args | join(" "))' \
		"$WORK/agent.log" 2>/dev/null || true
}

# any_clone_discards — every blkdiscard against any dm-clone device (§13).
any_clone_discards() {
	jq -r 'select(.msg == "os command" and .cmd == "blkdiscard")
	       | (.args | join(" "))' "$WORK/agent.log" 2>/dev/null |
		grep -E '/dev/mapper/dnv-[0-9a-f]{16}-[0-9a-f]{16}-3-' || true
}

# mutations [logfile] — every mutating operation in an agent log (§15 step 5).
# Probe operations (lsblk, dmsetup info/table/status, ls, and the [D13]
# "os read block") are expected and deliberately absent from the list, exactly
# mirroring the unit tests' readOnlyPrefixes.
mutations() {
	local log=${1:-$WORK/agent.log}
	jq -r '
	    select(.msg == "os write file direct")
	      | "write file direct " + .path' "$log" 2>/dev/null || true
	jq -r '
	    select(.msg == "os command")
	    | select(
	        (["blkdiscard"] | index(.cmd))
	        or (.cmd == "dmsetup" and (["create","reload","remove","suspend",
	              "resume","message"] | index(.args[0])))
	        or (.cmd == "nvme" and (["connect","disconnect"] | index(.args[0])))
	        or (["mkdir","rmdir","ln"] | index(.cmd))
	        or (.cmd == "rm")
	      )
	    | .cmd + " " + (.args | join(" "))' "$log" 2>/dev/null || true
	jq -r '
	    select(.msg == "os write block") | "write block " + .path' \
		"$log" 2>/dev/null || true
}

# ctrl_of <nqn> <traddr> — the controller device backing one path, so a dead
# source path can be disconnected by device instead of by NQN (§12 step 17).
ctrl_of() { path_field "$1" "$2" Name; }

# --- teardown ---------------------------------------------------------------

agent_dm_names() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E '^dnv-[0-9a-f]{16}-[0-9a-f]{16}-[0-9a-f]-' || true
}

dm_kind_names() { agent_dm_names | awk -F- -v k="$1" '$4 == k'; }

# resume_suspended sweeps up suspended dm devices before anything reads them.
# A migration source holds its per-CN dm-linears suspended for the
# SuspendSeconds grace window of §11.2, so a run killed mid-cutover leaves
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

# cleanup implements §16. The order is load-bearing; the one refinement over
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
	dm_remove_kind 1
	dm_remove_kind 3
	disconnect_kind 3
	drop_subsystems 3
	dm_remove_kind 5
	dm_remove_kind 2
	dm_remove_kind 0
	dm_remove_kind 4
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

	# Defensive: nothing the current agent builds is ever left suspended
	# ([D12]), but pre-[D12] debris would wedge the reads below.
	resume_suspended

	# Unformat each loop device: zeroing the 4 KiB header is enough, because
	# the volume-table slots are inert without it — a slot only counts when
	# its format_uuid matches the header's ([D13] §5.2). No oflag=, per the
	# §4 dd rule.
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
# reached so far, so the §17 dump can end with the last get-side-info of every
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
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$AGENT_BIN" ] || die "missing: $AGENT_BIN after make build"
	(cd "$REPO_ROOT" && go build -o "$CTL" ./integtest/dnagentctl >&2) ||
		die "building dnagentctl failed"
}

# preflight_vms runs after the start-of-run cleanup: the port check can only
# be meaningful once a crashed prior run's agents are gone (§2, §4).
preflight_vms() {
	STAGE="preflight (vms)"
	log "=== preflight: vms"
	local idx
	for idx in 1 2; do
		ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo -n true" ||
			die "missing: passwordless sudo on vm$idx (${VM[$idx]})"
		local missing
		missing=$(sshv "$idx" "for b in dmsetup nvme losetup blkdiscard lsblk dd fallocate sha256sum cmp pkill jq timeout; do command -v \$b >/dev/null || echo \$b; done")
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
# Setup (§7)
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
	local idx out wz
	for idx in 1 2; do
		# --local-store must pre-exist: startup reconcile fails without it.
		sshv "$idx" "mkdir -p $WORK/store && chmod 0777 $WORK && chmod 0755 $WORK/store"
		sshv "$idx" "fallocate -l 2G $WORK/backing.img"
		LOOP[idx]=$(sshv "$idx" "losetup --find --show $WORK/backing.img")
		log "[vm$idx] loop device ${LOOP[idx]}"
		# The §4 fast-Write-Zeroes preflight item, deferred to here because
		# the device only exists now (preflight_vms runs before setup). The
		# §9.4 zeroing assumes fast Write Zeroes; a loop device maps
		# REQ_OP_WRITE_ZEROES onto fallocate, so a 0 here means the kernel
		# would write zero pages at bulk speed and the agent's DN5 fail-fast
		# would refuse the disk outright. Read from /sys/class/block, the same
		# directory agent.Dm.WriteZeroesMaxBytes uses.
		wz=$(sshv "$idx" "cat /sys/class/block/\$(basename ${LOOP[$idx]})/queue/write_zeroes_max_bytes")
		[ "$wz" -gt 0 ] ||
			die "vm$idx: ${LOOP[$idx]} reports write_zeroes_max_bytes=0"
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

# converge_check runs the §9 check-dn/check-side round pair against a side and
# asserts that each reply echoes the revision the agent actually stored.
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

# wait_zeroed blocks until a side's background zeroing goroutine has zeroed
# every logical extent (§9.4). `ctl` adds no --timeout for this subcommand, so
# the one below is wait-zeroed's own polling budget, not an RPC deadline.
#
# The optional fifth argument is the ext_cnt the request asked for, and turns
# the wait into the exact "⇒ N/N" of §9 phase (b) and §10/§12: the driver's own
# loop exits on `total != 0 && zeroed >= total`, which a side allocated with
# the wrong number of extents also satisfies. Callers that have no --ext-cnt to
# compare against omit it and keep the driver's weaker guard.
wait_zeroed() { # dnidx sp leg side [ext_cnt]
	local idx=$1 want=${5:-} out zdone ztotal
	out=$(ctl "$idx" wait-zeroed --sp "$2" --leg "$3" --side "$4" \
		--interval 0.5 --timeout "$ZERO_TIMEOUT")
	[ -n "$want" ] || return 0
	zdone=$(jq_of "$out" '.zeroed // 0')
	ztotal=$(jq_of "$out" '.total // 0')
	assert_eq "$zdone/$ztotal" "$((want))/$((want))" \
		"side $2/$3/$4 zeroed/total extents after wait-zeroed"
}

# sync_side_cns lists every CN id one syncup-side request names — the primary
# and every standby — by scanning the caller's flag list the way Go's flag
# package does (`--flag value`, plus the `--flag=value` spelling). Phase (a)
# must be asserted for *all* of them: dnagent_integtest.md §9 and §11 step 1
# pin "every cn_id_to_* map entry PROVISIONING … for both CN ids", and a case
# A/D side carries a standby, so looking at one map entry leaves one of the
# two per-CN stacks unexamined while it is gated.
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

# sync_side_ext_cnt prints the --ext-cnt one syncup-side request asks for, by
# the same flag scan as sync_side_cns (both spellings). It is what makes the
# §9 phase-(b)/(c) equality checkable from the helper: the expected extent
# count is the caller's own flag, not a constant this file could drift from.
# Nothing is printed when the request carries no --ext-cnt, and every check
# built on it degrades to the driver's own guard rather than failing.
sync_side_ext_cnt() { # syncup-side flags…
	local arg want=""
	for arg in "$@"; do
		if [ -n "$want" ]; then
			printf '%s\n' "$arg"
			return 0
		fi
		case "$arg" in
		--ext-cnt) want=ext ;;
		--ext-cnt=*)
			printf '%s\n' "${arg#*=}"
			return 0
			;;
		esac
	done
}

# sync_side_2phase performs the two-phase side provisioning the sp-worker
# performs in production (§9.4). Phase 1 syncs the side with
# --provisioned=false: allocate the extent runs, build DnSideName, start the
# zeroing goroutine — and export nothing. wait_zeroed then blocks until every
# extent is zeroed, and phase 2 re-sends the identical request at a fresh
# revision with --provisioned=true, which is the worker's flip rule played by
# the script.
#
# Both revisions are minted by the caller in the parent shell (§9), so this is
# safe inside a background job. The phase-2 reply is left in SYNC_SIDE_REPLY:
# a bash function cannot both echo the reply and be called outside a command
# substitution, and the caller must not run bump_rev in one.
SYNC_SIDE_REPLY=""
sync_side_2phase() { # dnidx rev1 rev2 sp leg side [extra syncup-side flags…]
	local idx=$1 rev1=$2 rev2=$3 sp=$4 leg=$5 side=$6 out
	local cns cn key field left ext sample zdone ztotal
	shift 6
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
	# node does not model (dnagent_integtest.md §9 phase (a), §10 step 2).
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
	# The exact §9 phase (b)/(c) equality, zeroed == total == ext_cnt, asserted
	# on both the wait's last sample and the flip's reply: a side allocated
	# with the wrong number of extents zeroes all of them and would satisfy
	# every `zeroed >= total` check on the way. Hard, not tolerant.
	ext=$(sync_side_ext_cnt "$@")
	# The §9 provisioning-window sample: one get-side-info before the wait, to
	# record whether this run ever observed the side mid-zeroing. Purely an
	# observation and never an assertion — with 64 MiB extents a batch is
	# 640 MiB and loop maps Write Zeroes onto `fallocate`, so the window is
	# normally already closed by the time this samples, exactly like the §12
	# grace-window and read-through probes. A failed call degrades to a miss
	# for the same reason: this must not be able to fail the suite (the next
	# line's wait-zeroed is where a real problem surfaces).
	sample=$(ctl "$idx" get-side-info --sp "$sp" --leg "$leg" --side "$side" ||
		true)
	[ -n "$sample" ] || sample="{}"
	zdone=$(jq_of "$sample" '.side_info.zeroed_ext_cnt // "0"')
	ztotal=$(jq_of "$sample" '.side_info.total_ext_cnt // "0"')
	if [ "$ztotal" -gt 0 ] && [ "$zdone" -lt "$ztotal" ]; then
		log "provisioning $sp/$leg/$side: provisioning window HIT ($zdone/$ztotal zeroed)"
	else
		log "provisioning $sp/$leg/$side: WARNING provisioning window missed ($zdone/$ztotal zeroed)"
	fi
	wait_zeroed "$idx" "$sp" "$leg" "$side" "$ext"
	SYNC_SIDE_REPLY=$(ctl "$idx" syncup-side --revision "$rev2" \
		--sp "$sp" --leg "$leg" --side "$side" --provisioned=true "$@")
	if [ -n "$ext" ]; then
		zdone=$(jq_of "$SYNC_SIDE_REPLY" '.side_info.zeroed_ext_cnt // "0"')
		ztotal=$(jq_of "$SYNC_SIDE_REPLY" '.side_info.total_ext_cnt // "0"')
		assert_eq "$zdone/$ztotal" "$((ext))/$((ext))" \
			"provisioning $sp/$leg/$side: zeroed/total extents at provisioned=true"
	fi
}

# ---------------------------------------------------------------------------
# Case S — smoke (§10)
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
		--ext-cnt 1 --cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
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
# Case A — sides (§11)
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
			--ext-cnt 1 --cntlid-slot 0 \
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
# Cases B and C — the shared migration choreography (§12)
# ---------------------------------------------------------------------------

# join_jobs waits for the background jobs of one lockstep stage and fails the
# run if any of them did.
join_jobs() {
	local pid rc=0
	for pid in "$@"; do wait "$pid" || rc=1; done
	[ "$rc" -eq 0 ] || die "a concurrent migration step failed"
}

# migr_dn_sides fills SIDE_ARGS with the --side flags of one DN. with_src=0
# drops the DN's migration-source side, which is how §12 step 16 tears it down.
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
		--ext-cnt 2 --cntlid-slot 0 --primary-cn "${MCN[$m]}" \
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

# migr_declare_dst is §12 step 6/11: the same request twice, first gated with
# sp_level no_migration (so bitmap chunks land before any region is copied),
# then at readwrite to build the clone. Passing the level in makes the two
# calls provably identical apart from it.
#
# The destination has already been provisioned by migr_provision_dst, so every
# call here carries --provisioned=true: re-sending false
# would retract the export stacks a later stage relies on.
migr_declare_dst() { # m revision sp_level
	local m=$1 rev=$2 level=$3 out
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --cntlid-slot 1 --primary-cn "${MCN[$m]}" \
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
		# CN19-style suppression: wantMigr is false below SP_LEVEL_NO_MIGRATION,
		# so migr_dst_info is not emitted at all. assert_gated, not
		# assert_not_ok, so a PROVISIONING dst cannot satisfy it (ruling R4.35).
		assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
			"migr $m gated clone"
		# §12 step 6c's other half: "no `nvme connect` issued yet". The clone
		# and the connection to the source's :3: subsystem are built by the
		# same step 11 converge, so a destination that connected while it was
		# still gated would be pulling data from a source whose bitmap chunks
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
		# without the feature the §11.4 "mark this region hydrated"
		# blkdiscard would also reach the destination side device and destroy
		# an acknowledged write.
		#
		# The whole feature list is pinned, not just the one word: the exact
		# `2 no_hydration no_discard_passdown` of agent.CloneTable's derived
		# feature count (agent/dm.go:405-419), which is the string
		# dnagent_integtest.md §12 step 11 and §20 name. `dmsetup message …
		# enable_hydration` does NOT weaken it — dm-clone's STATUSTYPE_TABLE
		# reprints the constructor args saved by copy_ctr_args verbatim
		# (drivers/md/dm-clone-target.c), and only `dmsetup status`
		# (STATUSTYPE_INFO) recomputes the live flags. The grep is scoped to
		# this migration's clone device by name, so nothing else on the node
		# can satisfy it. A substring test for no_discard_passdown alone
		# accepts `1 no_discard_passdown`, i.e. a clone created with hydration
		# already enabled, which would copy the very regions §11.4 asked to
		# skip.
		local ctable
		ctable=$(helper "${MDSTDN[$m]}" \
			"clone_table '$(dn_clone_name "$CLUSTER" \
				"${DNID[${MDSTDN[$m]}]}" "$SP" "${MID[$m]}")'")
		printf '%s\n' "$ctable" |
			grep -qE ' 2 no_hydration no_discard_passdown( |$)' ||
			die "migr $m: dm-clone table is not '2 no_hydration no_discard_passdown': $ctable"
	fi
}

# migr_provision_dst is the dst half of the §11.2 migration provisioning rule: the
# destination side provisions FIRST, under the ordinary §9.4 protocol — the
# dm-linear and the zeroing goroutine only, no per-CN stacks, no connect and no
# dm-clone. The request is byte-for-byte migr_declare_dst's gated one except
# --provisioned=false, which is what makes the gate provable.
migr_provision_dst() { # m revision
	local m=$1 rev=$2 out field left nqn
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --cntlid-slot 1 --primary-cn "${MCN[$m]}" \
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
	# request's CN ids collapses to it (dnagent_integtest.md §9 phase (a)).
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
	# §12 step 6b's "⇒ 2/2": the exact equality against the --ext-cnt 2 this
	# request asked for, not merely "every extent it happened to allocate".
	wait_zeroed "${MDSTDN[$m]}" "$SP" "${MLEG[$m]}" "${MDSTSIDE[$m]}" 2
}

migr_connect_dst() { # m
	local m=$1
	local vm=${MCNVM[$m]}
	# Same NQN, the other DN: the CN kernel merges the two connections into
	# one multipath namespace with two paths (§3).
	cn_connect "$vm" "${MDSTDN[$m]}" "$SP" "${MLEG[$m]}" "${MCN[$m]}"
	local state
	state=$(cn_ana_state "$vm" "$SP" "${MLEG[$m]}" "${MCN[$m]}" "${MDSTDN[$m]}")
	assert_eq "$state" inaccessible "migr $m dst path before cutover"
}

# migr_gate_src is the dst_provisioned = false half of the §11.2
# migration rule, and it runs before the cutover: the request is byte-for-byte
# migr_cutover_src's except --dst-provisioned=false, which the spec declares
# **exactly equivalent** to migr_src_conf being absent. The source keeps
# serving, does not fence its per-CN linears and exports no migr-src subsystem;
# only the would-be migr_src_info.* rows differ, reporting PROVISIONING.
#
# Without that equivalence the source would fence the primary's path the moment
# the migration was created, leaving the leg with no serving path for the whole
# destination zeroing window — which is exactly what this stage proves it does
# not do.
migr_gate_src() { # m revision
	local m=$1 rev=$2 out nqn susp dev
	out=$(ctl "${MSRCDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MSRCSIDE[$m]}" \
		--ext-cnt 2 --cntlid-slot 0 --primary-cn "${MCN[$m]}" \
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
	# optimized here and return EIO. §12 step 8b therefore reads 1 MiB through
	# the CN device, the same production path stage 0 wrote through. Preceded
	# by a cache drop so the read reaches the media (§9, and never iflag=).
	dev=$(ns_by_id "$SP" "${MLEG[$m]}")
	drop_caches "${MCNVM[$m]}"
	assert_eq "$(helper "${MCNVM[$m]}" "read_probe '$dev'")" ok \
		"migr $m: a 1 MiB read through the gated src must still succeed"
}

migr_cutover_src() { # m revision
	local m=$1 rev=$2 out
	out=$(ctl "${MSRCDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MSRCSIDE[$m]}" \
		--ext-cnt 2 --cntlid-slot 0 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite --provisioned=true \
		--migr-src "${MID[$m]}:${MDSTSIDE[$m]}:${DNID[${MDSTDN[$m]}]}" \
		--dst-provisioned=true)
	assert_ok "$out" ".side_info.migr_src_info.dm_linear_info.status" \
		"migr $m src dm-linear"
	assert_ok "$out" ".side_info.migr_src_info.nvmeof_info.status" \
		"migr $m src export"
	# The per-CN linears enter the §11.2 grace window: suspended in place for
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

# migr_read_through is §12 step 13: the last must-copy MiB read through the
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

migr_finish_dst() { # m revision
	local m=$1 rev=$2 out
	out=$(ctl "${MDSTDN[$m]}" syncup-side --revision "$rev" \
		--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
		--ext-cnt 2 --cntlid-slot 1 --primary-cn "${MCN[$m]}" \
		--sp-level readwrite --provisioned=true)
	# The request drops --migr-dst, so the whole migr_dst_info block goes away
	# with the clone (ruling R4.35: gated, never merely "not OK").
	assert_gated "$out" ".side_info.migr_dst_info.dm_clone_info.status" \
		"migr $m clone after finish"
	assert_cn_ok "$out" cn_id_to_dm_linear "${MCN[$m]}" "migr $m finished dst"
	assert_cn_ok "$out" cn_id_to_nvmeof "${MCN[$m]}" "migr $m finished dst"
}

# migr_drop_src is §12 step 16/17: the source side leaves its DN's pointer
# list, which kills the CN's source controller with DNR. The host will not
# reconnect on its own, so the dead path is disconnected by device — by NQN
# would kill the surviving destination path too.
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

# run_migration_cases is the §12 body shared by cases B and C. $SP, $MID,
# $BM_CNT and the case name are set by the callers; the per-case differences
# are the two hooks push_bitmaps and verify_data.
declare -a PAT_SHA=("" "" "")
declare -a PAT_SHA_HEAD=("" "" "")
REPLY_DIR=""

run_migration_cases() {
	local m out pids rev1 rev2 prov1 prov2 dn
	REPLY_DIR=$(mktemp -d)
	# A fresh case starts with no sides for the §17 dump to report; the sides
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
	# The sides exist from here on, so register them for the §17 dump — in the
	# parent shell, like the revisions below, because a DIAG_SIDES entry
	# appended inside a background job would be appended to a copy.
	for m in 1 2; do
		diag_add_side "${MSRCDN[$m]}" "$SP" "${MLEG[$m]}" "${MSRCSIDE[$m]}"
	done
	# Two revisions per source: the §9.4 phase-1 sync and the worker's flip.
	# Both are minted here, in the parent shell, because bump_rev inside a
	# background job would increment a copy (§9).
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

	stage stage1prov "the destinations provision first (§9.4): zero, then gate"
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
		--side "${MDSTSIDE[1]}" --interval 0.5 --timeout 120 \
		"${MIN_FIRST_ARGS[@]}") &
	pids+=($!)
	(ctl "${MDSTDN[2]}" wait-hydrated --sp "$SP" --leg "${MLEG[2]}" \
		--side "${MDSTSIDE[2]}" --interval 0.5 --timeout 120 \
		"${MIN_FIRST_ARGS[@]}") &
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
	# The sides are gone, so a later case's §17 dump must not ask for them.
	DIAG_SIDES=()
	rm -rf "$REPLY_DIR"
}

# ---------------------------------------------------------------------------
# Case B — migr_full (§13)
# ---------------------------------------------------------------------------

case_migr_full() {
	CASE=migr_full
	SP=0xc1
	MID=("" 0x31 0x32)
	BM_CNT=0
	READ_THROUGH_MIB=127
	MIN_FIRST_ARGS=()

	push_bitmaps() { # rev1 rev2 — the destinations' current revisions
		# No chunks at all (§13): the equal-revision re-send of the gated
		# request only proves the reply's applied set is empty.
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
		# Discard-based skipping must not happen without bitmaps. The §9.4
		# provisioning `blkdiscard --zeroout` that replaced the old side-create
		# trim targets the side device (dnv-*-4-*), never a dm-clone
		# (dnv-*-3-*), so it does not match this filter either.
		local discards
		discards=$(helper "${MDSTDN[$m]}" any_clone_discards)
		[ -z "$discards" ] ||
			die "migr $m: unexpected dm-clone blkdiscard: $discards"
	}

	run_migration_cases
}

# ---------------------------------------------------------------------------
# Case C — migr_bitmap (§14)
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
	MIN_FIRST_ARGS=(--min-first 64)

	push_bitmaps() { # rev1 rev2 — the destinations' current revisions
		local revs=("" "$1" "$2") m out
		for m in 1 2; do
			# A push gates on the side's revision and never advances it.
			ctl "${MDSTDN[$m]}" push-migr-bm --revision "${revs[$m]}" \
				--sp "$SP" --leg "${MLEG[$m]}" --side "${MDSTSIDE[$m]}" \
				--migr "${MID[$m]}" --bm-idx 0 --bitmap-hex "$C_BM0" >/dev/null
			ctl "${MDSTDN[$m]}" push-migr-bm --revision "${revs[$m]}" \
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
		local vm=${MCNVM[$m]} dev got discards want
		dev=$(ns_by_id "$SP" "${MLEG[$m]}")
		# Layer 1a: the meta region and every must-copy region came across.
		got=$(sha_range "$vm" "$dev" 64)
		assert_eq "$got" "${PAT_SHA_HEAD[$m]}" "migr $m first 64 MiB"
		# Layer 1b: the skipped half reads zero even though the source holds
		# random data there — the agent skipped it, it did not copy it.
		# Those zeros are *guaranteed* by the destination's §9.4
		# `blkdiscard --zeroout` provisioning rather than hoped for from
		# discard-reads-zeros, which was never a hardware guarantee ([D15]).
		sshv "$vm" "dd if=$dev of=$WORK/tail-$m.bin bs=1M skip=64 count=64 status=none"
		got=$(sshv "$vm" "dd if=/dev/zero bs=1M count=64 status=none > $WORK/zero-$m.bin; cmp -s $WORK/tail-$m.bin $WORK/zero-$m.bin && echo zeros || echo data")
		assert_eq "$got" zeros "migr $m second 64 MiB must be zeros"
		sshv_ok "$vm" "rm -f $WORK/tail-$m.bin $WORK/zero-$m.bin"
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
# Case D — restart (§15)
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
		--ext-cnt 1 --cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite
	out=$SYNC_SIDE_REPLY
	assert_ok "$out" ".side_info.side_dev_info.status" "restart src side_dev"
	assert_cn_ok "$out" cn_id_to_nvmeof "$cn" "restart src"

	bump_rev 2
	provrev=${REV[2]}
	bump_rev 2
	sync_side_2phase 2 "$provrev" "${REV[2]}" "$sp" "$leg" "$dstside" \
		--ext-cnt 1 --cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
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
	ctl 2 push-migr-bm --revision "${REV[2]}" \
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
		--ext-cnt 1 --cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
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
		--ext-cnt 1 --cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--provisioned=true \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1)
	assert_eq "$(jq_of "$out" '.bm_info.bm_idx_list | @csv')" '0' \
		"restart post-restart bm_idx_list"

	stage idempotent "same-revision re-applies must mutate nothing"
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
		--ext-cnt 1 --cntlid-slot 0 --primary-cn "$cn" --sp-level readwrite \
		--provisioned=true)
	assert_ok "$out" ".side_info.side_dev_info.status" "restart re-apply side1"
	out=$(ctl 2 syncup-side --revision "${REV[2]}" \
		--sp "$sp" --leg "$leg" --side "$dstside" \
		--ext-cnt 1 --cntlid-slot 1 --primary-cn "$cn" --sp-level no_migration \
		--provisioned=true \
		--migr-dst "$migr:$srcside:${DNID[1]}" \
		--src-traddr "${IP[1]}" --src-trsvcid "$TR_SVC_ID" \
		--block-size "$BLOCK_SIZE" --meta-blocks "$META_BLOCKS" \
		--hydr-threshold "$HYDR_THRESHOLD" --hydr-batch "$HYDR_BATCH" \
		--bm-cnt 1)
	assert_ok "$out" ".side_info.side_dev_info.status" "restart re-apply side2"
	# The post-restart log covers the startup reconcile and these re-applies.
	# This is also the §9.4 resume-at-k net: `blkdiscard` is in
	# mutations()' verb list, so a reconcile that re-zeroes an already-complete
	# side — the resume logic reading its bits wrong — fails the case here.
	for idx in 1 2; do
		local muts
		muts=$(helper "$idx" mutations)
		[ -z "$muts" ] ||
			die "restart: dn$idx mutated after the restart:"$'\n'"$muts"
	done

	stage stale "only a stale rejection proves the revision survived"
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
