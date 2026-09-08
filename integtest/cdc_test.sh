#!/usr/bin/env bash
#
# cdc_test.sh — the `dnv-cdc` integration test of doc/cdc.md §9. Four servers,
# a real single-node etcd, a real four-instance dnv-cdc fleet, real nvmet
# targets over dm-zero and two real kernel NVMe hosts, driven from this machine
# over ssh by integtest/cdcctl (the gateway's write path for CdcEntry keys).
#
#   bash integtest/cdc_test.sh [--only <case>] [--cleanup-only] \
#       user@<s1> user@<s2> user@<h1> user@<h2>
#
# Cases (§9.1), in order, each in its own cluster ids and each after a fleet
# restart and a `cdcctl wipe` (§9.9): smoke, matrix, lowlevel, stas, ha.
# Cleanup runs unconditionally at the start and, on success only, at the end:
# a failing run leaves etcd's data, every cdc log, the nvmet/dm state and the
# host connections in place and dumps the §9.16 diagnostics.
#
# SUDO: s1 needs none — etcd, the cdc fleet and cdcctl are plain user
# processes. s2 and both hosts need passwordless sudo: nvmet configfs, device
# mapper, `nvme connect`, udev and systemd are all root.
#
# Log reading: every cdc log is one JSON object per line, and a log being
# appended to while we `cat` it can end in a torn line, so every read goes
# through `jq -R 'fromjson? // empty'` (recs below) — a partial last line is
# dropped, never fails an assertion.
#
# FOUR THINGS THIS SUITE ESTABLISHED ON REAL HARDWARE (Linux 7.0, nvme-cli
# 2.16, nvme-stas 2.4.1). doc/cdc.md was corrected against each of them at
# implementation time; they are repeated here because they are the reason
# several assertions below look the way they do:
#
#  1. The discovery AEN reaches a host as the udev property
#     NVME_AEN=0x70f002 (the AER completion's dword 0 verbatim), NOT as
#     `NVME_EVENT=discovery` — no kernel emits that. It is also exactly what
#     /usr/lib/udev/rules.d/70-nvmf-autoconnect.rules matches on.
#  2. That autoconnect rule fires nvmf-connect@.service, so setup MASKS that
#     unit on both hosts and cleanup unmasks it (nvme-stas 2.x has no knob of
#     its own). It is masked for EVERY case: it would otherwise connect behind
#     case L's back the instant an AEN landed, which is what case L measures.
#  3. ssC uses shard code `80`, not `08`: DS2 gives range digit h the codes
#     h0..hf, so 08 belongs to range 0 and would land in the LOW half,
#     contradicting §9.11's grid.
#  4. nvme-stas sends the TP-8010 DIM command (opcode 21h) WITH in-capsule
#     data to every discovery controller. Terminating the connection over that
#     — the original reading of NP2 — put stas in a permanent connect/reset
#     loop; refusing the command with invalid opcode is what it expects. Case
#     T is what caught it, and what keeps it caught.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (§9.3, §9.5, §9.9)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
CACHE_DIR="$BIN_DIR/cache"
CDC_BIN="$REPO_ROOT/bin/dnv-cdc"
CDCCTL_BIN="$BIN_DIR/cdcctl"

# The pinned etcd release, shared with the worker suite: same version, same
# digest, same integtest/bin/cache. A tarball already verified there is never
# re-downloaded.
ETCD_VERSION=v3.6.14
ETCD_DIST="etcd-$ETCD_VERSION-linux-amd64"
ETCD_URL="https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/$ETCD_DIST.tar.gz"
ETCD_SHA256=ffe840ff9295808e88cce2794a18a5ac87f12a5203c8314d0bf6aa119b41bac5
ETCD_TAR="$CACHE_DIR/$ETCD_DIST.tar.gz"

WORK=/var/tmp/dnv-cdc-integtest

# §9.3 ports: none shared with the worker suite (12379/12380, 296xx/297xx) or
# the agent suites (29528/29529/4200).
ETCD_CLIENT_PORT=13379
ETCD_PEER_PORT=13380
CDC_PORT_BASE=18009               # cdc0..cdc3 -> 18009..18012
NVMET_PORT_BASE=14420             # port1..port4 -> 14420..14423
S1_PORTS=(13379 13380 18009 18010 18011 18012)
S2_PORTS=(14420 14421 14422 14423)

# common.DnvPrefix — the first field of every dnv etcd key.
DNV_PREFIX=dnv

CDC_DIRS=(cdc0 cdc1 cdc2 cdc3)
# The §9.3 range split: two twins of the low half, two of the high half.
CDC_RANGE_LOW=0,1,2,3,4,5,6,7
CDC_RANGE_HIGH=8,9,a,b,c,d,e,f

# §9.9 polling budgets.
WAIT_SHORT=5
WAIT_AEN=15
WAIT_CONN=20
WAIT_STAS=30
# How long a "nothing must happen" assertion settles for before it is checked.
SETTLE=3

NQN_PREFIX=nqn.2024-01.io.dnv-it:cdc
# §9.5: the ghost identity, used only through an explicit -q/-I pair from h1.
GHOST_NQN="$NQN_PREFIX:ghost"

# The seven subsystems of §9.5, created once at setup. SS_UUID is the
# namespace uuid a host waits for; SS_PORTS is the setup-shape port link list.
SS_NAMES=(ssa ssb ssc ssd sse ssf ssx)
UUID_PREFIX=0000cdc0-0000-4000-8000-0000000000
declare -A SS_UUID=(
	[ssa]="${UUID_PREFIX}01" [ssb]="${UUID_PREFIX}02" [ssc]="${UUID_PREFIX}03"
	[ssd]="${UUID_PREFIX}04" [sse]="${UUID_PREFIX}05" [ssf]="${UUID_PREFIX}06"
	[ssx]="${UUID_PREFIX}07"
)
declare -A SS_PORTS=(
	[ssa]="1" [ssb]="2" [ssc]="3" [ssd]="1 2" [sse]="3" [ssf]="4" [ssx]="4"
)

# The §9.5 entry set: shard code, sp id and ss id per subsystem. ssC is on 80
# rather than the document's 08 (see deviation 3 in the header).
declare -A E_SHARD=(
	[ssa]=00 [ssb]=07 [ssc]=80 [ssd]=ff [sse]=3c [ssf]=81 [ssx]=05
)
declare -A E_SP=(
	[ssa]=0x1 [ssb]=0x1 [ssc]=0x2 [ssd]=0x2 [sse]=0x3 [ssf]=0x4 [ssx]=0x5
)
declare -A E_SS=(
	[ssa]=0xa [ssb]=0xb [ssc]=0xc [ssd]=0xd [sse]=0xe [ssf]=0xf [ssx]=0x11
)

# §9.5 cluster ids, one per case. The cross-cluster entry (ssF) lives in the
# case id with a leading 1 nibble, as §9.5's own example spells it (M ->
# 0x1cdc2).
declare -A CASE_CID=(
	[smoke]=0xcdc1 [matrix]=0xcdc2 [lowlevel]=0xcdc3 [stas]=0xcdc4 [ha]=0xcdc5
)

CASES=(smoke matrix lowlevel stas ha)

# The uevent property the discovery-log-change AEN reaches a host as (header
# deviation 1).
AEN_PROP='NVME_AEN=0x70f002'

# ---------------------------------------------------------------------------
# Mutable state
# ---------------------------------------------------------------------------

S1="" S2="" H1="" H2=""
IP1="" IP2="" IPH1="" IPH2=""
# H1NQN/H2NQN are the hosts' own /etc/nvme/hostnqn, read at setup (§9.5).
H1NQN="" H2NQN=""
GHOST_ID=""
JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE=setup
CID=""
CID2=""
STAGE="(startup)"
SETUP_DONE=0
QUIET=0
STAS_RUNNING=0
RUN_START=""

declare -A PID=()      # cdc dir -> pid recorded from $! at launch
declare -A RUNNING=()  # cdc dir -> 1 while we believe the process is alive

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id
# `it-<case>-<step>`, which every cdcctl call of the step then stamps on its
# etcd records.
stage() {
	STAGE="$CASE: $2"
	TRACE="it-$CASE-$1"
	log ""
	log "=== $CASE: $2   [trace_id $TRACE]"
}
TRACE=it-setup

die() {
	log ""
	log "FAILED at stage '$STAGE' (trace_id $TRACE)"
	log "  $*"
	exit 1
}

assert_eq() { [ "$1" = "$2" ] || die "$3: got '$1', want '$2'"; }
assert_ne() { [ "$1" != "$2" ] || die "$3: got '$1', want anything else"; }
assert_ge() { [ "$1" -ge "$2" ] || die "$3: got $1, want >= $2"; }
assert_gt() { [ "$1" -gt "$2" ] || die "$3: got $1, want > $2"; }

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

on_exit() {
	local rc=$?
	trap - EXIT
	if [ "$rc" -eq 0 ]; then
		if [ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ]; then
			log ""
			log "=== end-of-run cleanup (success)"
			cleanup 1
		fi
		log ""
		log "PASS"
	else
		log ""
		log "########## diagnostics (§9.16) ##########"
		diagnostics || true
		log ""
		log "debris left in place on all four servers; failing stage '$STAGE'"
	fi
	[ -n "$TMPDIR_LOCAL" ] && rm -rf "$TMPDIR_LOCAL"
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution (§9.2)
# ---------------------------------------------------------------------------

# ssh_to runs a command on one server. QUIET is raised while wait_until and
# assert_none poll, so a twenty second poll does not bury the transcript.
ssh_to() { # <target> <cmd…>
	local target=$1
	shift
	[ "$QUIET" -eq 1 ] || log "[${target##*@}] $*"
	ssh "${SSH_OPTS[@]}" "$target" "$*"
}

s1() { ssh_to "$S1" "$@"; }
s2() { ssh_to "$S2" "sudo -n bash -c $(printf '%q' "$*")"; }
h() { # <h1|h2> <cmd…>
	local which=$1
	shift
	ssh_to "$(host_target "$which")" "sudo -n bash -c $(printf '%q' "$*")"
}
# hu is h without sudo, for reads that do not need it.
hu() { # <h1|h2> <cmd…>
	local which=$1
	shift
	ssh_to "$(host_target "$which")" "$*"
}

host_target() { # <h1|h2>
	case "$1" in
	h1) printf '%s' "$H1" ;;
	h2) printf '%s' "$H2" ;;
	*) die "unknown host $1" ;;
	esac
}

host_ip() { # <h1|h2>
	case "$1" in
	h1) printf '%s' "$IPH1" ;;
	h2) printf '%s' "$IPH2" ;;
	*) die "unknown host $1" ;;
	esac
}

host_nqn() { # <h1|h2>
	case "$1" in
	h1) printf '%s' "$H1NQN" ;;
	h2) printf '%s' "$H2NQN" ;;
	*) die "unknown host $1" ;;
	esac
}

ok_or_true() { "$@" || true; }

# ctl is the §9.6 driver wrapper: every call carries the endpoint, the case's
# cluster and the stage's trace id. Each argument is quoted for the REMOTE
# shell with printf %q, because an etcd key and an NQN both contain characters
# the remote shell would otherwise re-split.
ctl() {
	local quoted
	quoted=$(printf '%q ' "$@")
	s1 "$WORK/bin/cdcctl --etcd 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CID --trace-id $TRACE $quoted"
}

# ctl2 is ctl against the second cluster id (the cross-cluster entry, §9.5).
ctl2() {
	local quoted
	quoted=$(printf '%q ' "$@")
	s1 "$WORK/bin/cdcctl --etcd 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CID2 --trace-id $TRACE $quoted"
}

# rlog reads one remote file, treating "not there yet" as empty and any OTHER
# failure as a failure: every count-based negative assertion below would
# silently pass on a broken ssh otherwise.
rlog() { # <target> <path>
	ssh "${SSH_OPTS[@]}" "$1" \
		"if [ -e $(printf '%q' "$2") ]; then cat -- $(printf '%q' "$2"); fi"
}

cpath() { printf '%s/%s/cdc.log' "$WORK" "$1"; }

# recs/recsr stream one cdc log through a jq filter. `-R` plus `fromjson?`
# makes a torn last line disappear instead of failing the whole read.
recs() { # <cdc dir> <filter> [jq args…]
	local dir=$1 filter=$2
	shift 2
	rlog "$S1" "$(cpath "$dir")" |
		"$JQ" -cR "$@" "fromjson? // empty | $filter"
}

recsr() { # <cdc dir> <filter> [jq args…]
	local dir=$1 filter=$2
	shift 2
	rlog "$S1" "$(cpath "$dir")" |
		"$JQ" -rR "$@" "fromjson? // empty | $filter"
}

# count_recs is deliberately not a bare pipeline: `recs | wc -l` prints "0"
# when the READ failed, and every `assert_eq "$(count_recs …)" 0` below would
# then pass on a broken ssh. The assignment carries the pipeline's status
# (pipefail), so a failed read dies here instead.
count_recs() { # <cdc dir> <filter> [jq args…]
	local out
	out=$(recs "$@" | wc -l | tr -d ' \n') ||
		die "reading $1's cdc log failed"
	printf '%s' "$out"
}

# ccount counts matching records over every cdc log of the fleet.
ccount() { # <filter> [jq args…]
	local filter=$1
	shift
	local d total=0 n
	for d in "${CDC_DIRS[@]}"; do
		n=$(count_recs "$d" "$filter" "$@")
		total=$((total + n))
	done
	printf '%s' "$total"
}

# ---------------------------------------------------------------------------
# Polling (§9.9)
# ---------------------------------------------------------------------------

# wait_until polls once a second until the command exits 0, else dies.
wait_until() { # <secs> <label> <cmd…>
	local secs=$1 label=$2
	shift 2
	local deadline=$((SECONDS + secs)) saved=$QUIET
	QUIET=1
	while :; do
		if "$@"; then
			QUIET=$saved
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			QUIET=$saved
			die "timed out after ${secs}s waiting for: $label"
		fi
		sleep 1
	done
}

# assert_none is wait_until's negative twin: settle, then require the command
# to exit NON-zero. It is how every "and the other host saw nothing" assertion
# of §0 #5 is made.
assert_none() { # <secs> <label> <cmd…>
	local secs=$1 label=$2
	shift 2
	log "  (nothing must happen for ${secs}s: $label)"
	sleep "$secs"
	local saved=$QUIET
	QUIET=1
	if "$@"; then
		QUIET=$saved
		die "$label: it happened, want nothing"
	fi
	QUIET=$saved
	return 0
}

# ---------------------------------------------------------------------------
# The cdc fleet (§9.3)
# ---------------------------------------------------------------------------

cdc_port() { printf '%d' $((CDC_PORT_BASE + $1)); }

cdc_range() { # <instance index 0..3>
	if [ "$1" -lt 2 ]; then printf '%s' "$CDC_RANGE_LOW"; else
		printf '%s' "$CDC_RANGE_HIGH"
	fi
}

# start_cdc launches one instance with >> so an external truncation resets the
# write offset, records the pid from $! and signals by that pid afterwards.
start_cdc() { # <instance index 0..3>
	local dir="cdc$1" pid
	pid=$(s1 "nohup $WORK/bin/dnv-cdc" \
		"--etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--range $(cdc_range "$1")" \
		"--tr-type tcp --adr-fam ipv4 --tr-addr $IP1" \
		"--tr-svc-id $(cdc_port "$1")" \
		">> $WORK/$dir/cdc.log 2>&1 < /dev/null &" \
		"echo \$! > $WORK/$dir/pid; cat $WORK/$dir/pid")
	[ -n "$pid" ] || die "starting $dir produced no pid"
	PID[$dir]=$pid
	RUNNING[$dir]=1
	log "  $dir started, pid $pid, range $(cdc_range "$1"), port $(cdc_port "$1")"
}

sig_cdc() { # <dir> <signal>
	s1 "kill -$2 \$(cat $WORK/$1/pid)"
}

cdc_gone() { ! s1 "kill -0 \$(cat $WORK/$1/pid) 2>/dev/null"; }

# scans_at counts the `cdc scan complete` records one instance's log holds; a
# relaunch appends to a log the per-case reset truncated, so counting past a
# baseline is what proves THIS launch scanned.
scans_at() { # <dir>
	count_recs "$1" 'select(.msg == "cdc scan complete")'
}

scanned_since() { # <dir> <baseline>
	[ "$(scans_at "$1")" -gt "$2" ]
}

wait_scan() { # <dir> <baseline>
	wait_until "$WAIT_SHORT" "$1: cdc scan complete" scanned_since "$1" "$2"
}

# start_fleet launches all four instances and waits for one fresh scan each.
start_fleet() {
	local i dir
	declare -A base=()
	for i in 0 1 2 3; do
		dir="cdc$i"
		base[$dir]=$(scans_at "$dir")
	done
	for i in 0 1 2 3; do start_cdc "$i"; done
	for i in 0 1 2 3; do
		dir="cdc$i"
		wait_scan "$dir" "${base[$dir]}"
	done
}

stop_fleet() {
	local dir
	for dir in "${CDC_DIRS[@]}"; do
		[ "${RUNNING[$dir]-}" = 1 ] || continue
		ok_or_true sig_cdc "$dir" KILL
		unset "RUNNING[$dir]"
	done
	# Not wait_until: it dies on timeout, and `|| true` cannot catch an
	# exit. A fleet member that will not die is reported, not fatal — the
	# per-case reset that follows relaunches everything anyway.
	local deadline
	for dir in "${CDC_DIRS[@]}"; do
		deadline=$((SECONDS + WAIT_SHORT))
		while [ "$SECONDS" -lt "$deadline" ]; do
			if cdc_gone "$dir"; then break; fi
			sleep 1
		done
		cdc_gone "$dir" || log "  warning: $dir is still alive after ${WAIT_SHORT}s"
	done
}

# ---------------------------------------------------------------------------
# Driving etcd (§9.6)
# ---------------------------------------------------------------------------

# tr_arg renders one --tr value for a nvmet port number.
tr_arg() { printf 'tcp,ipv4,%s,%d' "$IP2" $((NVMET_PORT_BASE + $1 - 1)); }

# put_entry writes one CdcEntry of the §9.5 set. Ports is a space-separated
# list of nvmet port numbers, in the order they must appear (it is the DS5
# tr-conf index). Every remaining argument is an allowed hostnqn; none means
# an open entry, visible to everyone (DS4).
put_entry() { # <ss> <"ports"> [allowed…]
	local ss=$1 ports=$2
	shift 2
	local args=(put --shard "${E_SHARD[$ss]}" --sp "${E_SP[$ss]}"
		--ss "${E_SS[$ss]}" --nqn "$NQN_PREFIX:$ss")
	local p a
	for p in $ports; do args+=(--tr "$(tr_arg "$p")"); done
	for a in "$@"; do args+=(--allowed "$a"); done
	ctl "${args[@]}" >/dev/null
}

# put_entry2 is put_entry against the second cluster id: the cross-cluster
# entry that proves DS1's all-clusters watch.
put_entry2() { # <ss> <"ports"> [allowed…]
	local ss=$1 ports=$2
	shift 2
	local args=(put --shard "${E_SHARD[$ss]}" --sp "${E_SP[$ss]}"
		--ss "${E_SS[$ss]}" --nqn "$NQN_PREFIX:$ss")
	local p a
	for p in $ports; do args+=(--tr "$(tr_arg "$p")"); done
	for a in "$@"; do args+=(--allowed "$a"); done
	ctl2 "${args[@]}" >/dev/null
}

del_entry() { # <ss>
	ctl del --shard "${E_SHARD[$1]}" --sp "${E_SP[$1]}" \
		--ss "${E_SS[$1]}" >/dev/null
}

wipe_entries() { ctl wipe >/dev/null; }

# put_matrix writes the whole §9.5 entry set for the current case.
put_matrix() {
	put_entry ssa "1"
	put_entry ssb "2" "$H1NQN"
	put_entry ssc "3" "$H2NQN"
	put_entry ssd "1 2" "$H1NQN" "$H2NQN"
	put_entry sse "3" "$H1NQN"
	put_entry2 ssf "4" "$H2NQN"
}

# ---------------------------------------------------------------------------
# Discovery assertions (§9.9)
# ---------------------------------------------------------------------------

# disc_raw runs one `nvme discover` and PROPAGATES its exit status. identity
# "ghost" adds the §9.5 ghost's -q/-I pair, which the kernel's 1:1 hostnqn rule
# makes mandatory together (§9.4 item 7).
#
# The status matters: an empty discovery log is `{"genctr":N,"records":[]}`
# with status 0, while an instance that is down, a refused connection or a
# broken ssh are all non-zero. Swallowing the difference would make every
# "serves nothing" assertion below pass on a dead fleet.
disc_raw() { # <h1|h2> <instance> [ghost]
	local hostsel=$1 inst=$2 ident=${3-}
	local extra=""
	if [ "$ident" = ghost ]; then
		extra="-q $GHOST_NQN -I $GHOST_ID"
	fi
	h "$hostsel" "nvme discover -t tcp -a $IP1 -s $(cdc_port "$inst")" \
		"$extra -o json"
}

# json_or_empty passes JSON through and swallows anything else. It is applied
# only to output that already came back with status 0.
json_or_empty() {
	local out
	out=$(cat)
	case "$out" in
	'{'* | '['*) printf '%s' "$out" ;;
	*) : ;;
	esac
}

# disc_try echoes one discover's JSON, or returns non-zero when the command
# failed or printed something that is not JSON. It is the form the polling
# predicates use, where a transient failure must retry rather than abort.
disc_try() { # <h1|h2> <instance> [ghost]
	local out
	out=$(disc_raw "$@" 2>/dev/null) || return 1
	case "$out" in
	'{'*) printf '%s' "$out" ;;
	*) return 1 ;;
	esac
}

# disc_json is disc_try for a one-shot assertion: a failure is the run's
# failure, not an empty log.
disc_json() { # <h1|h2> <instance> [ghost]
	local out rc=0
	# stdout only: stderr carries ssh_to's own transcript line as well as
	# nvme-cli's, and folding it in would make every answer "not JSON".
	out=$(disc_raw "$@" 2>/dev/null) || rc=$?
	if [ "$rc" != 0 ]; then
		die "$1: nvme discover against cdc$2 failed (rc $rc)"
	fi
	case "$out" in
	'{'*) printf '%s' "$out" ;;
	*) die "$1: nvme discover against cdc$2 printed no JSON: $out" ;;
	esac
}

# recs_of turns one discover's JSON into the set of (subnqn, traddr, trsvcid)
# triples, one per line and sorted — the §9.9 comparison unit.
recs_of() {
	"$JQ" -r '[.records[]? |
		"\(.subnqn)|\(.traddr)|\(.trsvcid)"] | sort | .[]'
}

# disc_set is the assertion form: it dies if the discover did not answer.
disc_set() { # <h1|h2> <instance> [ghost]
	disc_json "$@" | recs_of
}

disc_genctr() { # <h1|h2> <instance> [ghost]
	disc_json "$@" | "$JQ" -r '.genctr // empty'
}

# rec renders one expected triple: a subsystem seen through one nvmet port.
rec() { # <ss> <nvmet port number>
	printf '%s:%s|%s|%d\n' "$NQN_PREFIX" "$1" "$IP2" \
		$((NVMET_PORT_BASE + $2 - 1))
}

# want_set renders an expected discovery result from "<ss>:<port>" pairs.
want_set() {
	local pair
	for pair in "$@"; do
		rec "${pair%%:*}" "${pair##*:}"
	done | sort
}

assert_disc() { # <h1|h2> <instance> <identity|""> <label> <ss:port…>
	local hostsel=$1 inst=$2 ident=$3 label=$4
	shift 4
	local got want
	got=$(disc_json "$hostsel" "$inst" "$ident" | recs_of)
	want=$(want_set "$@")
	if [ "$got" != "$want" ]; then
		die "$label: discovery log mismatch
    got:  [$(printf '%s' "$got" | tr '\n' ' ')]
    want: [$(printf '%s' "$want" | tr '\n' ' ')]"
	fi
	log "  ok: $label"
}

# disc_matches / wait_disc are assert_disc's polling twins. They exist for
# case H, whose one-shot discovers share their endpoint with a stafd that is
# reconnecting to the very instance the case just restarted: the kernel refuses
# a second controller with the same base options while stas's own is still
# coming up, so a single-shot assertion there is racing a daemon, not the
# service under test.
disc_matches() { # <h1|h2> <instance> <identity|""> <ss:port…>
	local hostsel=$1 inst=$2 ident=$3 out
	shift 3
	out=$(disc_try "$hostsel" "$inst" "$ident") || return 1
	[ "$(printf '%s' "$out" | recs_of)" = "$(want_set "$@")" ]
}

wait_disc() { # <secs> <h1|h2> <instance> <identity|""> <label> <ss:port…>
	local secs=$1 hostsel=$2 inst=$3 ident=$4 label=$5
	shift 5
	wait_until "$secs" "$label" disc_matches "$hostsel" "$inst" "$ident" "$@"
	log "  ok: $label"
}

# twins_agree is the §9.14 step 3 assertion: two instances of one range serve
# byte-identical, NON-EMPTY content. Emptiness is excluded on purpose — two
# failed discovers also "agree".
twins_agree() { # <h1|h2> <instance a> <instance b>
	local ja jb a b
	ja=$(disc_try "$1" "$2") || return 1
	jb=$(disc_try "$1" "$3") || return 1
	a=$(printf '%s' "$ja" | recs_of)
	b=$(printf '%s' "$jb" | recs_of)
	[ -n "$a" ] && [ "$a" = "$b" ]
}

# disc_dev_set / disc_dev_genctr re-read the log through an EXISTING
# persistent controller (§9.9: GENCTR is only meaningful on one).
disc_dev_json() { # <h1|h2> <nvmeN>
	local out rc=0
	# nvme-cli prints an "ignoring non matching command-line options" note
	# on stderr for a --device re-read; stdout is the JSON.
	out=$(h "$1" "nvme discover --device $2 -o json 2>/dev/null") || rc=$?
	if [ "$rc" != 0 ]; then
		die "$1: nvme discover --device $2 failed (rc $rc)"
	fi
	case "$out" in
	'{'*) printf '%s' "$out" ;;
	*) die "$1: nvme discover --device $2 printed no JSON: $out" ;;
	esac
}

disc_dev_set() { # <h1|h2> <nvmeN>
	disc_dev_json "$@" | recs_of
}

disc_dev_genctr() { # <h1|h2> <nvmeN>
	disc_dev_json "$@" | "$JQ" -r '.genctr // empty'
}

assert_dev_disc() { # <h1|h2> <nvmeN> <label> <ss:port…>
	local hostsel=$1 dev=$2 label=$3
	shift 3
	local got want
	got=$(disc_dev_set "$hostsel" "$dev")
	want=$(want_set "$@")
	if [ "$got" != "$want" ]; then
		die "$label: discovery log mismatch on $dev
    got:  [$(printf '%s' "$got" | tr '\n' ' ')]
    want: [$(printf '%s' "$want" | tr '\n' ' ')]"
	fi
	log "  ok: $label"
}

# ---------------------------------------------------------------------------
# Host state (§9.8, §9.9)
# ---------------------------------------------------------------------------

uuid_path() { printf '/dev/disk/by-id/nvme-uuid.%s' "${SS_UUID[$1]}"; }

# have_dev answers a three-valued question with two values, so it has to say
# which: a bare `test -e` over ssh returns non-zero for BOTH "the node is not
# there" and "the ssh failed", and every assert_no_dev below would then pass on
# a broken connection.
have_dev() { # <h1|h2> <ss>
	local out
	out=$(hu "$1" "if [ -e $(uuid_path "$2") ]; then echo YES; else echo NO; fi")
	case "$out" in
	YES) return 0 ;;
	NO) return 1 ;;
	*) die "$1: probing $2 answered '$out', want YES or NO" ;;
	esac
}

wait_dev() { # <h1|h2> <ss>
	wait_until "$WAIT_CONN" "$1: $2 device node" have_dev "$1" "$2"
	log "  ok: $1 has $2 ($(uuid_path "$2"))"
}

wait_dev_gone() { # <h1|h2> <ss>
	wait_until "$WAIT_CONN" "$1: $2 device node to go away" \
		not_have_dev "$1" "$2"
	log "  ok: $1 no longer has $2"
}

not_have_dev() { ! have_dev "$1" "$2"; }

assert_dev() { # <h1|h2> <label> <ss…>
	local hostsel=$1 label=$2
	shift 2
	local ss
	for ss in "$@"; do
		have_dev "$hostsel" "$ss" ||
			die "$label: $hostsel is missing $ss ($(uuid_path "$ss"))"
	done
	log "  ok: $label ($hostsel has $*)"
}

assert_no_dev() { # <h1|h2> <label> <ss…>
	local hostsel=$1 label=$2
	shift 2
	local ss
	for ss in "$@"; do
		if have_dev "$hostsel" "$ss"; then
			die "$label: $hostsel has $ss, want absent"
		fi
	done
	log "  ok: $label ($hostsel does not have $*)"
}

# subsys_json is the bare, no-argument `nvme list-subsys -o json`. It is
# always the bare form: with a device argument the output shape changes and
# becomes ambiguous.
subsys_json() { # <h1|h2>
	local out rc=0
	out=$(h "$1" "nvme list-subsys -o json 2>/dev/null") || rc=$?
	if [ "$rc" != 0 ]; then
		die "$1: nvme list-subsys failed (rc $rc)"
	fi
	# An empty listing is a valid answer ('[]'); anything that is not JSON
	# is not, and reading it as "no subsystems" would make every teardown
	# assertion pass on a broken host.
	case "$out" in
	'['* | '{'*) printf '%s' "$out" ;;
	'') die "$1: nvme list-subsys printed nothing" ;;
	*) die "$1: nvme list-subsys printed no JSON: $out" ;;
	esac
}

# test_subsystems lists the live dnv-it subsystem NQNs on one host, sorted.
test_subsystems() { # <h1|h2>
	subsys_json "$1" | "$JQ" -r '[.[]?.Subsystems[]? |
		select(.NQN | startswith("nqn.2024-01.io.dnv-it:")) | .NQN] |
		sort | unique | .[]'
}

# subsys_trsvcids lists the transport service ids of one subsystem's live
# paths, sorted — the "two paths, one subsystem" assertion of §9.11 step 4.
subsys_trsvcids() { # <h1|h2> <ss>
	subsys_json "$1" | "$JQ" -r --arg n "$NQN_PREFIX:$2" \
		'[.[]?.Subsystems[]? | select(.NQN == $n) | .Paths[]? |
		 .Address | capture("trsvcid=(?<p>[0-9]+)").p] | sort | .[]'
}

# disc_ctrl_count is how many LIVE discovery controllers point at the cdc
# fleet — four per host once stas has converged (§9.13 step 1), three while one
# instance is down (§9.14 step 1). The state matters: a controller whose
# instance died stays listed, in `connecting`, until stas's ctrl_loss_tmo.
disc_ctrl_count() { # <h1|h2>
	subsys_json "$1" | "$JQ" -r --arg ip "$IP1" \
		'[.[]?.Subsystems[]? |
		 select(.NQN == "nqn.2014-08.org.nvmexpress.discovery") | .Paths[]? |
		 select(.Address | contains("traddr=" + $ip)) |
		 select(.State == "live")] | length'
}

connect_all() { # <h1|h2> <instance>
	h "$1" "nvme connect-all -t tcp -a $IP1 -s $(cdc_port "$2") 2>&1 || true"
}

host_wipe() { # <h1|h2>
	h "$1" "$WORK/cdc_host.sh wipe $IP1"
}

# host_wipe_data leaves the discovery controllers alone — see the wipe_data
# verb in cdc_host.sh.
host_wipe_data() { # <h1|h2>
	h "$1" "$WORK/cdc_host.sh wipe_data"
}

# ---------------------------------------------------------------------------
# Uevent capture (§9.12)
# ---------------------------------------------------------------------------

uevents_start() { # <h1|h2>
	h "$1" "$WORK/cdc_host.sh uev_start $WORK"
}

uevents_stop() { # <h1|h2>
	ok_or_true h "$1" "$WORK/cdc_host.sh uev_stop"
}

# aen_count is how many discovery-log-change AENs one host's capture holds for
# one controller. `udevadm monitor --property` prints one blank-line-separated
# block per event, so the count is the number of blocks whose DEVPATH ends in
# that controller AND that carry the AEN property (header deviation 1).
aen_count() { # <h1|h2> <nvmeN>
	# Same guard as count_recs: awk prints 0 on empty input, so a capture
	# that could not be READ would otherwise read as "no AEN arrived" — the
	# exact answer every §0 #5 negative below is looking for.
	local out
	out=$(rlog "$(host_target "$1")" "$WORK/uevents.log" |
		awk -v dev="$2" -v prop="$AEN_PROP" '
			BEGIN { RS = ""; n = 0 }
			$0 ~ ("DEVPATH=[^\n]*/" dev "\n") && index($0, prop) > 0 { n++ }
			END { printf "%d", n }') ||
		die "reading $1's uevent capture failed"
	printf '%s' "$out"
}

aen_ge() { # <h1|h2> <nvmeN> <n>
	[ "$(aen_count "$1" "$2")" -ge "$3" ]
}

wait_aen() { # <h1|h2> <nvmeN> <n>
	wait_until "$WAIT_AEN" "$1: $3 discovery AEN(s) on $2" \
		aen_ge "$1" "$2" "$3"
	log "  ok: $1 got $3 discovery AEN(s) on $2"
}

assert_aen_count() { # <h1|h2> <nvmeN> <want> <label>
	assert_eq "$(aen_count "$1" "$2")" "$3" "$4"
}

# ---------------------------------------------------------------------------
# nvme-stas (§9.8)
# ---------------------------------------------------------------------------

stas_start() { # <h1|h2>
	h "$1" "$WORK/cdc_host.sh stas_start $IP1 $WORK" \
		"$(cdc_port 0) $(cdc_port 1) $(cdc_port 2) $(cdc_port 3)"
}

stas_stop() { # <h1|h2>
	ok_or_true h "$1" "$WORK/cdc_host.sh stas_stop $WORK"
}

stas_start_both() {
	local hostsel
	for hostsel in h1 h2; do stas_start "$hostsel"; done
	STAS_RUNNING=1
}

stas_stop_both() {
	local hostsel
	for hostsel in h1 h2; do stas_stop "$hostsel"; done
	STAS_RUNNING=0
}

disc_ctrls_ge() { # <h1|h2> <n>
	[ "$(disc_ctrl_count "$1")" -ge "$2" ]
}

wait_stas_converged() { # <h1|h2>
	wait_until "$WAIT_STAS" "$1: four discovery connections to the fleet" \
		disc_ctrls_ge "$1" 4
	log "  ok: $1 holds four discovery connections to $IP1"
}

# no_test_subsystems is the §9.13 step 6 assertion: stacd disconnected
# everything whose DLPE vanished.
no_test_subsystems() { # <h1|h2>
	[ -z "$(test_subsystems "$1")" ]
}

# ---------------------------------------------------------------------------
# The two remote helper scripts (§9.7, §9.8)
# ---------------------------------------------------------------------------
#
# They are generated here and scp'd rather than kept as files of their own, so
# integtest/ still holds exactly the two artifacts §9.2 names. Everything they
# do needs root on the server they run on, so both are invoked through sudo.

TMPDIR_LOCAL=""

write_target_helper() { # <path>
	cat > "$1" <<'TARGET_EOF'
#!/usr/bin/env bash
#
# cdc_target.sh — the nvmet side of the dnv-cdc integration suite (cdc.md
# §9.7). Runs on server 2 under sudo. Verbs: setup <ip>, link <ss> <port>,
# unlink <ss> <port>, teardown.
#
# `setup` is rerun-tolerant, with the nvmet configfs rules baked in: every
# mkdir is create-guarded and every attribute is written ONLY when the object
# is first created. Re-reading an attribute and comparing is wrong by
# construction here — attr_serial reads back space-padded and device_uuid
# reads back reformatted — so existence, never equality, is the guard.
set -uo pipefail

NVMET=/sys/kernel/config/nvmet
NQN_PREFIX="nqn.2024-01.io.dnv-it:cdc"
PORT_BASE=14420
DM_PREFIX=dnv-cdc-it-
DM_SECTORS=131072 # 64 MiB
SS_NAMES="ssa ssb ssc ssd sse ssf ssx"

die() { echo "cdc_target.sh: $*" >&2; exit 1; }

uuid_of() {
	case "$1" in
	ssa) echo 0000cdc0-0000-4000-8000-000000000001 ;;
	ssb) echo 0000cdc0-0000-4000-8000-000000000002 ;;
	ssc) echo 0000cdc0-0000-4000-8000-000000000003 ;;
	ssd) echo 0000cdc0-0000-4000-8000-000000000004 ;;
	sse) echo 0000cdc0-0000-4000-8000-000000000005 ;;
	ssf) echo 0000cdc0-0000-4000-8000-000000000006 ;;
	ssx) echo 0000cdc0-0000-4000-8000-000000000007 ;;
	*) die "unknown subsystem $1" ;;
	esac
}

# ports_of is the §9.5 setup shape: which nvmet ports a subsystem is linked to
# when nothing has moved. ssD deliberately sits on two, which is what gives one
# subsystem two paths on the host.
ports_of() {
	case "$1" in
	ssa) echo 1 ;;
	ssb) echo 2 ;;
	ssc) echo 3 ;;
	ssd) echo "1 2" ;;
	sse) echo 3 ;;
	ssf) echo 4 ;;
	ssx) echo 4 ;;
	*) die "unknown subsystem $1" ;;
	esac
}

subsys_dir() { echo "$NVMET/subsystems/$NQN_PREFIX:$1"; }
link_path() { echo "$NVMET/ports/$2/subsystems/$NQN_PREFIX:$1"; }

do_link() {
	local target link
	target=$(subsys_dir "$1")
	link=$(link_path "$1" "$2")
	[ -d "$target" ] || die "subsystem $1 does not exist"
	# Never re-create a link that is already there: removing and re-adding a
	# live port link stalls every host that is using it.
	if [ ! -e "$link" ]; then
		ln -s "$target" "$link" || die "linking $1 to port $2 failed"
	fi
}

do_unlink() {
	local link
	link=$(link_path "$1" "$2")
	if [ -e "$link" ]; then
		rm -f "$link" || die "unlinking $1 from port $2 failed"
	fi
}

setup() {
	local ip=$1 n ss dir ns want got
	[ -n "$ip" ] || die "setup needs the server's ip"
	modprobe nvmet || die "modprobe nvmet failed"
	modprobe nvmet-tcp || die "modprobe nvmet-tcp failed"
	modprobe dm-zero 2>/dev/null || true
	[ -d "$NVMET" ] || die "no $NVMET after modprobe"
	dmsetup targets | grep -q '^zero ' || die "no dm-zero target"

	for ss in $SS_NAMES; do
		if ! dmsetup info "$DM_PREFIX$ss" >/dev/null 2>&1; then
			dmsetup create "$DM_PREFIX$ss" \
				--table "0 $DM_SECTORS zero" ||
				die "dmsetup create $DM_PREFIX$ss failed"
		fi
	done

	for n in 1 2 3 4; do
		dir="$NVMET/ports/$n"
		want=$((PORT_BASE + n - 1))
		if [ -d "$dir" ]; then
			# Never ADOPT a port this suite did not create: teardown
			# deletes what setup claimed, links and all, and silently
			# taking over an operator's port 1 would delete their
			# subsystem links with it.
			got=$(cat "$dir/addr_trsvcid" 2>/dev/null | tr -d ' ')
			[ "$got" = "$want" ] || die \
				"nvmet port $n already exists with trsvcid '$got'," \
				"not this suite's $want — refusing to touch it"
			continue
		fi
		mkdir "$dir" || die "creating port $n failed"
		echo tcp > "$dir/addr_trtype" || die "port $n trtype"
		echo ipv4 > "$dir/addr_adrfam" || die "port $n adrfam"
		echo "$ip" > "$dir/addr_traddr" || die "port $n traddr"
		echo "$want" > "$dir/addr_trsvcid" || die "port $n trsvcid"
	done

	for ss in $SS_NAMES; do
		dir=$(subsys_dir "$ss")
		if [ ! -d "$dir" ]; then
			mkdir "$dir" || die "creating subsystem $ss failed"
			# Deliberately allow any host: if dnv-cdc ever leaks an entry
			# to the wrong host the resulting connect SUCCEEDS and the
			# wrong-device assertion catches it, instead of nvmet's own ACL
			# masking the leak (§0 #14).
			echo 1 > "$dir/attr_allow_any_host" || die "$ss allow_any_host"
			echo dnv-it > "$dir/attr_model" 2>/dev/null || true
		fi
		ns="$dir/namespaces/1"
		if [ ! -d "$ns" ]; then
			mkdir "$ns" || die "creating $ss namespace failed"
			echo "/dev/mapper/$DM_PREFIX$ss" > "$ns/device_path" ||
				die "$ss device_path"
			echo "$(uuid_of "$ss")" > "$ns/device_uuid" || die "$ss uuid"
			echo 1 > "$ns/enable" || die "enabling $ss namespace failed"
		fi
	done

	for ss in $SS_NAMES; do
		for n in $(ports_of "$ss"); do do_link "$ss" "$n"; done
	done
	echo "cdc_target.sh: setup ok"
}

# ours_port reports whether nvmet port n is one this suite created, judged by
# its transport service id. Teardown removes only those.
ours_port() {
	local got
	[ -d "$NVMET/ports/$1" ] || return 1
	got=$(cat "$NVMET/ports/$1/addr_trsvcid" 2>/dev/null | tr -d ' ')
	[ "$got" = "$((PORT_BASE + $1 - 1))" ]
}

teardown() {
	local n ss dir ns link
	# Links FIRST. Hosts are already disconnected by the caller's ordering:
	# an unlink under a live connection kills the controller with DNR and the
	# host never reconnects by itself.
	for n in 1 2 3 4; do
		ours_port "$n" || continue
		[ -d "$NVMET/ports/$n/subsystems" ] || continue
		for link in "$NVMET/ports/$n/subsystems"/*; do
			[ -e "$link" ] || continue
			rm -f "$link" 2>/dev/null || true
		done
	done
	for dir in "$NVMET/subsystems/$NQN_PREFIX":*; do
		[ -d "$dir" ] || continue
		for ns in "$dir/namespaces"/*; do
			[ -d "$ns" ] || continue
			echo 0 > "$ns/enable" 2>/dev/null || true
			rmdir "$ns" 2>/dev/null || true
		done
		rmdir "$dir" 2>/dev/null || true
	done
	for n in 1 2 3 4; do
		ours_port "$n" || continue
		rmdir "$NVMET/ports/$n" 2>/dev/null || true
	done
	for ss in $SS_NAMES; do
		dmsetup remove "$DM_PREFIX$ss" 2>/dev/null || true
	done
	echo "cdc_target.sh: teardown ok"
}

case "${1-}" in
setup) setup "${2-}" ;;
link) do_link "${2-}" "${3-}" && echo "linked ${2-} to port ${3-}" ;;
unlink) do_unlink "${2-}" "${3-}" && echo "unlinked ${2-} from port ${3-}" ;;
teardown) teardown ;;
*) die "usage: cdc_target.sh setup <ip> | link <ss> <port> | unlink <ss> <port> | teardown" ;;
esac
TARGET_EOF
	chmod 0755 "$1"
}

write_host_helper() { # <path>
	cat > "$1" <<'HOST_EOF'
#!/usr/bin/env bash
#
# cdc_host.sh — the host side of the dnv-cdc integration suite (cdc.md §9.8,
# §9.15). Runs on h1/h2 under sudo. Verbs:
#
#   wipe <cdc-ip>          disconnect every dnv-it subsystem and every
#                          discovery controller pointing at the cdc fleet
#   mask / unmask          neutralise the kernel's own nvmf autoconnect
#   stas_start <ip> <work> <p0> <p1> <p2> <p3>
#   stas_stop <work>
#   uev_start <work> / uev_stop
#
# It never uses `nvme disconnect-all`: that would take down subsystems this
# suite has nothing to do with.
set -uo pipefail

DISC_NQN=nqn.2014-08.org.nvmexpress.discovery
TEST_NQN_PREFIX="nqn.2024-01.io.dnv-it:"

disconnect_test_subsystems() {
	local sysfs nqn
	for sysfs in /sys/class/nvme-subsystem/*/subsysnqn; do
		[ -e "$sysfs" ] || continue
		nqn=$(cat "$sysfs" 2>/dev/null) || continue
		case "$nqn" in
		"$TEST_NQN_PREFIX"*)
			nvme disconnect -n "$nqn" >/dev/null 2>&1 || true
			;;
		esac
	done
}

disconnect_discovery() {
	local ip=$1 ctrl name nqn addr
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		[ "$nqn" = "$DISC_NQN" ] || continue
		addr=$(cat "$ctrl/address" 2>/dev/null) || addr=""
		case "$addr" in
		*"traddr=$ip"*)
			name=$(basename "$ctrl")
			nvme disconnect -d "$name" >/dev/null 2>&1 || true
			;;
		esac
	done
}

uev_stop() {
	pkill -f 'udevadm monitor --kernel --property --subsystem-match=nvme' \
		>/dev/null 2>&1 || true
}

# The kernel ships /usr/lib/udev/rules.d/70-nvmf-autoconnect.rules, which
# starts nvmf-connect@.service on the very AEN this suite measures. Masking the
# unit leaves exactly one connector active: the operator (cases S, M, L) or
# stacd (cases T, H). nvme-stas 2.4.1 offers no knob of its own for this.
do_mask() {
	systemctl mask nvmf-connect@.service >/dev/null 2>&1 || true
	systemctl mask nvmf-connect.target >/dev/null 2>&1 || true
	echo masked
}

do_unmask() {
	systemctl unmask nvmf-connect@.service >/dev/null 2>&1 || true
	systemctl unmask nvmf-connect.target >/dev/null 2>&1 || true
	echo unmasked
}

backup_stas() { # <work>
	local work=$1 f
	mkdir -p "$work/stas-backup"
	for f in stafd.conf stacd.conf; do
		if [ -f "/etc/stas/$f" ] && [ ! -f "$work/stas-backup/$f" ]; then
			cp -p "/etc/stas/$f" "$work/stas-backup/$f"
		fi
	done
	# A marker distinguishes "there was nothing to back up" from "the backup
	# is missing", so restore never invents a file the host never had.
	touch "$work/stas-backup/.taken"
}

restore_stas() { # <work>
	local work=$1 f
	[ -f "$work/stas-backup/.taken" ] || return 0
	for f in stafd.conf stacd.conf; do
		if [ -f "$work/stas-backup/$f" ]; then
			cp -p "$work/stas-backup/$f" "/etc/stas/$f"
		else
			rm -f "/etc/stas/$f"
		fi
	done
}

stas_start() { # <ip> <work> <p0> <p1> <p2> <p3>
	local ip=$1 work=$2
	shift 2
	command -v stafd >/dev/null 2>&1 || {
		echo "cdc_host.sh: stafd is not installed" >&2
		exit 1
	}
	backup_stas "$work"
	mkdir -p /etc/stas
	{
		echo '[Global]'
		echo 'tron=false'
		echo 'hdr-digest=false'
		echo 'data-digest=false'
		echo 'kato=10'
		echo 'ignore-iface=false'
		echo 'ip-family=ipv4'
		# dnv-cdc advertises no PLEOS, so stafd would not use PLEO anyway;
		# saying so explicitly keeps the request shape pinned across stas
		# versions.
		echo 'pleo=disabled'
		echo
		echo '[Service Discovery]'
		# No mDNS: the suite's endpoints are static (cdc.md §1 out of scope).
		echo 'zeroconf=disabled'
		echo
		echo '[Discovery controller connection management]'
		echo 'persistent-connections=true'
		echo
		echo '[Controllers]'
		local port
		for port in "$@"; do
			echo "controller = transport=tcp;traddr=$ip;trsvcid=$port"
		done
	} > /etc/stas/stafd.conf
	{
		echo '[Global]'
		echo 'tron=false'
		echo 'hdr-digest=false'
		echo 'data-digest=false'
		echo 'ip-family=ipv4'
		echo
		echo '[I/O controller connection management]'
		# Connect every DLPE, and disconnect the connections stacd itself
		# made once their DLPE vanishes. Scoping to stas's own connections
		# is deliberate: a wider scope would audit connections this suite
		# does not own.
		echo 'disconnect-scope=only-stas-connections'
		echo 'disconnect-trtypes=tcp'
		echo 'connect-attempts-on-ncc=0'
		echo
		echo '[Controllers]'
	} > /etc/stas/stacd.conf
	systemctl restart stafd stacd || {
		echo "cdc_host.sh: restarting stafd/stacd failed" >&2
		exit 1
	}
	systemctl is-active --quiet stafd || {
		echo "cdc_host.sh: stafd is not active" >&2
		exit 1
	}
	systemctl is-active --quiet stacd || {
		echo "cdc_host.sh: stacd is not active" >&2
		exit 1
	}
	echo "cdc_host.sh: stas started"
}

stas_stop() { # <work>
	systemctl stop stafd stacd >/dev/null 2>&1 || true
	restore_stas "$1"
	echo "cdc_host.sh: stas stopped"
}

case "${1-}" in
wipe)
	uev_stop
	disconnect_test_subsystems
	disconnect_discovery "${2-}"
	echo "cdc_host.sh: wiped"
	;;
wipe_data)
	# Data connections only. Never touch the discovery controllers while
	# stafd owns them: it treats one disconnected behind its back as gone
	# for good and does not re-create it, so the host silently stops
	# receiving AENs for the rest of the run.
	uev_stop
	disconnect_test_subsystems
	echo "cdc_host.sh: data connections wiped"
	;;
mask) do_mask ;;
unmask) do_unmask ;;
uev_start)
	uev_stop
	rm -f "${2-}/uevents.log"
	mkdir -p "${2-}"
	nohup udevadm monitor --kernel --property --subsystem-match=nvme \
		> "${2-}/uevents.log" 2>&1 < /dev/null &
	sleep 1
	echo "cdc_host.sh: uevent capture started"
	;;
uev_stop)
	uev_stop
	echo "cdc_host.sh: uevent capture stopped"
	;;
stas_start)
	shift
	stas_start "$@"
	;;
stas_stop) stas_stop "${2-}" ;;
*)
	echo "usage: cdc_host.sh wipe <ip> | mask | unmask | uev_start <work> |" \
		"uev_stop | stas_start <ip> <work> <p…> | stas_stop <work>" >&2
	exit 2
	;;
esac
HOST_EOF
	chmod 0755 "$1"
}

# ship_helpers pushes the two generated helpers and creates the per-server
# tree. It is idempotent and cheap, and cleanup calls it first so that even a
# start-of-run cleanup on a machine that has never seen this suite has the
# tools it needs.
ship_helpers() {
	[ -n "$TMPDIR_LOCAL" ] || die "ship_helpers before the temp dir exists"
	write_target_helper "$TMPDIR_LOCAL/cdc_target.sh"
	write_host_helper "$TMPDIR_LOCAL/cdc_host.sh"
	bash -n "$TMPDIR_LOCAL/cdc_target.sh" || die "cdc_target.sh is malformed"
	bash -n "$TMPDIR_LOCAL/cdc_host.sh" || die "cdc_host.sh is malformed"
	local target
	ssh_to "$S2" "mkdir -p $WORK" || die "creating $WORK on s2 failed"
	scp -q "${SSH_OPTS[@]}" "$TMPDIR_LOCAL/cdc_target.sh" \
		"$S2:$WORK/cdc_target.sh" || die "scp of cdc_target.sh failed"
	for target in "$H1" "$H2"; do
		ssh_to "$target" "mkdir -p $WORK" ||
			die "creating $WORK on ${target##*@} failed"
		scp -q "${SSH_OPTS[@]}" "$TMPDIR_LOCAL/cdc_host.sh" \
			"$target:$WORK/cdc_host.sh" ||
			die "scp of cdc_host.sh to ${target##*@} failed"
	done
}

# ---------------------------------------------------------------------------
# Cleanup (§9.15)
# ---------------------------------------------------------------------------

s1_cleanup_script() {
	cat <<EOF
set -u
pids=""
for f in $WORK/*/pid; do
	[ -f "\$f" ] || continue
	pids="\$pids \$(cat "\$f" 2>/dev/null)"
done
for p in \$pids; do kill -TERM "\$p" 2>/dev/null || true; done
for i in \$(seq 1 20); do
	alive=0
	for p in \$pids; do
		if kill -0 "\$p" 2>/dev/null; then alive=1; fi
	done
	[ "\$alive" = 0 ] && break
	sleep 0.25
done
for p in \$pids; do kill -KILL "\$p" 2>/dev/null || true; done
pkill -f 'bin/dnv-cdc' 2>/dev/null || true
pkill -f 'etcd --name dnv-cdc-it' 2>/dev/null || true
sleep 0.5
echo cleaned
EOF
}

# cleanup is §9.15, in order: hosts first (their reconnect loops must die
# before their targets vanish), then the nvmet target, then s1. end removes
# $WORK everywhere and puts the hosts' autoconnect back.
cleanup() { # <end 0|1>
	local end=$1
	log "[cleanup] hosts: stop stas, stop captures, disconnect everything ours"
	ship_helpers
	local hostsel
	for hostsel in h1 h2; do
		ok_or_true h "$hostsel" "$WORK/cdc_host.sh stas_stop $WORK"
		ok_or_true h "$hostsel" "$WORK/cdc_host.sh wipe $IP1"
	done
	STAS_RUNNING=0

	log "[cleanup] s2: cdc_target.sh teardown"
	ok_or_true s2 "$WORK/cdc_target.sh teardown"

	log "[cleanup] s1: kill the cdc fleet and etcd"
	s1_cleanup_script | ssh "${SSH_OPTS[@]}" "$S1" "bash -s" >/dev/null ||
		true
	PID=()
	RUNNING=()

	if [ "$end" = 1 ]; then
		log "[cleanup] end of run: unmask the kernel autoconnect, remove $WORK"
		for hostsel in h1 h2; do
			ok_or_true h "$hostsel" "$WORK/cdc_host.sh unmask"
		done
		ok_or_true s1 "rm -rf $WORK"
		ok_or_true s2 "rm -rf $WORK"
		for hostsel in h1 h2; do
			ok_or_true h "$hostsel" "rm -rf $WORK"
		done
	fi
	# nvme-stas stays installed and the kernel modules stay loaded (§9.15).
}

# ---------------------------------------------------------------------------
# Diagnostics (§9.16)
# ---------------------------------------------------------------------------

banner() {
	log ""
	log "--- $* ---"
}

diagnostics() {
	local saved=$QUIET
	QUIET=1
	log "failing stage: '$STAGE'   trace_id: $TRACE   cluster: $CID/$CID2"

	local dir
	for dir in "${CDC_DIRS[@]}"; do
		banner "s1 $dir: last 100 records, msg != \"etcd get\""
		recs "$dir" 'select(.msg != "etcd get")' | tail -n 100 >&2 || true
	done

	banner "s1: cdcctl list"
	ok_or_true ctl list >&2

	banner "s1: ss -ltn"
	ok_or_true s1 "ss -ltn | grep -E ':(1337[89]|1800[9]|1801[012])' || true" >&2

	banner "s2: nvmet configfs"
	ok_or_true s2 "find /sys/kernel/config/nvmet -maxdepth 4 | sort" >&2
	banner "s2: dmsetup"
	ok_or_true s2 "dmsetup ls --target zero; dmsetup table" >&2

	local hostsel
	for hostsel in h1 h2; do
		banner "$hostsel: nvme list-subsys"
		ok_or_true h "$hostsel" "nvme list-subsys -o json" >&2
		banner "$hostsel: nvme list"
		ok_or_true h "$hostsel" "nvme list -o json" >&2
		banner "$hostsel: uevent capture (last 120 lines)"
		ok_or_true rlog "$(host_target "$hostsel")" "$WORK/uevents.log" |
			tail -n 120 >&2 || true
		banner "$hostsel: journalctl -u stafd -u stacd"
		ok_or_true h "$hostsel" \
			"journalctl -u stafd -u stacd --since '$RUN_START' --no-pager |" \
			"tail -n 80" >&2
		banner "$hostsel: stafctl / stacctl"
		ok_or_true h "$hostsel" "stafctl ls 2>&1; stafctl status 2>&1" >&2
		ok_or_true h "$hostsel" "stacctl ls 2>&1; stacctl status 2>&1" >&2
		banner "$hostsel: dmesg tail"
		ok_or_true h "$hostsel" "dmesg | tail -50" >&2
	done

	log ""
	log "pull the failing stage's records with:"
	for dir in "${CDC_DIRS[@]}"; do
		log "  ssh $S1 cat $(cpath "$dir") | jq 'select(.trace_id==\"$TRACE\")'"
	done
	QUIET=$saved
}

# ---------------------------------------------------------------------------
# Preflight (§9.4)
# ---------------------------------------------------------------------------

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing: $1 on the driver"
}

# resolve_jq picks the driver's JSON parser exactly as the other suites do: a
# system jq if there is one, else the gojq drop-in built into the gitignored
# integtest/bin with the Go toolchain the driver already needs.
resolve_jq() {
	if command -v jq >/dev/null 2>&1; then
		JQ=jq
		return
	fi
	JQ="$BIN_DIR/gojq"
	[ -x "$JQ" ] || GOBIN="$BIN_DIR" GOFLAGS=-mod=mod \
		go install github.com/itchyny/gojq/cmd/gojq@v0.12.17 >&2 ||
		die "missing: jq on the driver, and building gojq failed"
	log "driver json parser: $JQ (no system jq)"
}

sha256_of() { sha256sum "$1" | cut -d' ' -f1; }

# fetch_etcd is the download-and-verify path of §9.2, shared with the worker
# suite: a cached tarball whose sha256 already matches the pin is never
# re-downloaded, so a repeat run costs nothing and a fresh checkout still works.
fetch_etcd() {
	mkdir -p "$CACHE_DIR"
	if [ -f "$ETCD_TAR" ] && [ "$(sha256_of "$ETCD_TAR")" = "$ETCD_SHA256" ]; then
		log "  etcd $ETCD_VERSION tarball cached and verified"
	else
		log "  downloading $ETCD_URL"
		curl -fsSL -o "$ETCD_TAR.part" "$ETCD_URL" ||
			die "downloading the etcd tarball failed"
		mv -f "$ETCD_TAR.part" "$ETCD_TAR"
		local got
		got=$(sha256_of "$ETCD_TAR")
		[ "$got" = "$ETCD_SHA256" ] ||
			die "etcd tarball sha256 is $got, want $ETCD_SHA256"
	fi
	if [ ! -x "$CACHE_DIR/$ETCD_DIST/etcd" ] ||
		[ ! -x "$CACHE_DIR/$ETCD_DIST/etcdctl" ]; then
		tar -xzf "$ETCD_TAR" -C "$CACHE_DIR" ||
			die "extracting the etcd tarball failed"
	fi
	[ -x "$CACHE_DIR/$ETCD_DIST/etcd" ] || die "no etcd binary after extraction"
	[ -x "$CACHE_DIR/$ETCD_DIST/etcdctl" ] || die "no etcdctl after extraction"
}

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp curl tar sha256sum awk sed; do need_local "$tool"; done
	resolve_jq
	fetch_etcd
	log "  make build (CGO_ENABLED=0 GOOS=linux GOARCH=amd64)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$CDC_BIN" ] || die "missing: $CDC_BIN after make build"
	log "  building integtest/bin/cdcctl"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$CDCCTL_BIN" ./integtest/cdcctl >&2) ||
		die "building cdcctl failed"
	log "preflight (driver) ok"
}

ports_free() { # <target> <port…>
	local target=$1
	shift
	local listening port
	listening=$(ssh_to "$target" \
		"ss -ltnH | awk '{ print \$4 }' | sed 's/.*://' | sort -u") ||
		die "listing listening ports on ${target##*@} failed"
	for port in "$@"; do
		if [ "$(printf '%s\n' "$listening" | grep -cx "$port" || true)" != 0 ]; then
			die "port $port is already listening on ${target##*@}"
		fi
	done
}

# preflight_servers runs AFTER the start-of-run cleanup: the port checks are
# only meaningful once a crashed prior run's processes are gone (§9.4).
preflight_servers() {
	STAGE="preflight (servers)"
	log "=== preflight: servers"

	# 1. ssh everywhere; sudo on s2/h1/h2 and deliberately NOT on s1.
	local target
	for target in "$S1" "$S2" "$H1" "$H2"; do
		ssh_to "$target" "true" ||
			die "passwordless ssh to $target failed"
	done
	for target in "$S2" "$H1" "$H2"; do
		ssh_to "$target" "sudo -n true" ||
			die "passwordless sudo on $target failed"
	done

	# 2. s1: the six ports and a writable $WORK.
	ports_free "$S1" "${S1_PORTS[@]}"
	s1 "mkdir -p $WORK && test -w $WORK" || die "$WORK is not writable on s1"
	local free
	free=$(s1 "df -Pk /var/tmp | awk 'NR==2 { print \$4 }'")
	assert_ge "$free" 524288 "/var/tmp free space in KiB on s1"

	# 3. s2: nvmet, configfs, dm-zero and the four nvmet ports.
	s2 "modprobe nvmet && modprobe nvmet-tcp" ||
		die "modprobe nvmet/nvmet-tcp failed on s2"
	s2 "test -d /sys/kernel/config/nvmet" ||
		die "/sys/kernel/config/nvmet is missing on s2"
	s2 "modprobe dm-zero 2>/dev/null; dmsetup targets | grep -q '^zero '" ||
		die "no dm-zero target on s2"
	ports_free "$S2" "${S2_PORTS[@]}"

	# 4. h1/h2: nvme-tcp, nvme-cli, the host identity files, udevadm.
	local hostsel version
	for hostsel in h1 h2; do
		h "$hostsel" "modprobe nvme-tcp" ||
			die "modprobe nvme-tcp failed on $hostsel"
		version=$(hu "$hostsel" "nvme version 2>/dev/null | head -1") ||
			die "nvme-cli is missing on $hostsel"
		log "  $hostsel nvme-cli: $version"
		hu "$hostsel" "command -v udevadm >/dev/null" ||
			die "udevadm is missing on $hostsel"
		h "$hostsel" "mkdir -p /etc/nvme;" \
			"test -s /etc/nvme/hostnqn || nvme gen-hostnqn > /etc/nvme/hostnqn;" \
			"test -s /etc/nvme/hostid || uuidgen > /etc/nvme/hostid" ||
			die "creating the host identity on $hostsel failed"
	done
	H1NQN=$(hu h1 "cat /etc/nvme/hostnqn")
	H2NQN=$(hu h2 "cat /etc/nvme/hostnqn")
	[ -n "$H1NQN" ] || die "h1 has an empty /etc/nvme/hostnqn"
	[ -n "$H2NQN" ] || die "h2 has an empty /etc/nvme/hostnqn"
	assert_ne "$H1NQN" "$H2NQN" "the two hosts' hostnqn"
	log "  h1 hostnqn: $H1NQN"
	log "  h2 hostnqn: $H2NQN"

	# 5. h1/h2: nvme-stas, installed if it is not there. It is the ONE step
	#    that needs outbound network, and cleanup leaves the package
	#    installed (§9.4 item 5).
	for hostsel in h1 h2; do
		if ! hu "$hostsel" "command -v stafd >/dev/null"; then
			log "  installing nvme-stas on $hostsel"
			h "$hostsel" "DEBIAN_FRONTEND=noninteractive apt-get install -y" \
				"nvme-stas" >/dev/null ||
				die "installing nvme-stas on $hostsel failed"
		fi
		version=$(hu "$hostsel" "stafd --version 2>&1 | head -1") ||
			die "stafd is not runnable on $hostsel"
		log "  $hostsel nvme-stas: $version"
		# The stafd.conf/stacd.conf keys cdc_host.sh writes are pinned for
		# the 2.x line; assert the major so a 1.x host fails here rather
		# than silently never connecting.
		case "$version" in
		*" 2."*) ;;
		*) die "$hostsel: nvme-stas $version is not the pinned 2.x line" ;;
		esac
		h "$hostsel" "systemctl status stafd >/dev/null 2>&1;" \
			"systemctl status stacd >/dev/null 2>&1; true" ||
			die "stafd/stacd units are not resolvable on $hostsel"
	done
	log "preflight (servers) ok"
}

# ---------------------------------------------------------------------------
# Setup (§9.3, §9.5)
# ---------------------------------------------------------------------------

etcd_reachable() {
	s1 "$WORK/bin/cdcctl --etcd 127.0.0.1:$ETCD_CLIENT_PORT ping" >/dev/null
}

setup() {
	CASE=setup

	stage layout "create the §9.3 tree and ship the binaries"
	s1 "mkdir -p $WORK/bin $WORK/etcd $(printf "$WORK/%s " "${CDC_DIRS[@]}")"
	SETUP_DONE=1
	log "[s1] scp etcd etcdctl dnv-cdc cdcctl -> $WORK/bin"
	scp -q "${SSH_OPTS[@]}" \
		"$CACHE_DIR/$ETCD_DIST/etcd" "$CACHE_DIR/$ETCD_DIST/etcdctl" \
		"$CDC_BIN" "$CDCCTL_BIN" "$S1:$WORK/bin/" ||
		die "scp of the s1 binaries failed"
	s1 "chmod 0755 $WORK/bin/*"
	ship_helpers

	stage etcd "start etcd and wait for a cdcctl ping"
	local pid
	pid=$(s1 "nohup $WORK/bin/etcd --name dnv-cdc-it --data-dir $WORK/etcd" \
		"--listen-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--advertise-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--listen-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-advertise-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster dnv-cdc-it=http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster-token dnv-cdc-it" \
		">> $WORK/etcd/etcd.log 2>&1 < /dev/null &" \
		"echo \$! > $WORK/etcd/pid; cat $WORK/etcd/pid")
	[ -n "$pid" ] || die "starting etcd produced no pid"
	PID[etcd]=$pid
	RUNNING[etcd]=1
	CID=0x1 # any cluster: ping does not use it
	wait_until "$WAIT_SHORT" "etcd to answer a cdcctl ping" etcd_reachable

	stage target "build the §9.5 nvmet topology on s2"
	s2 "$WORK/cdc_target.sh setup $IP2" || die "cdc_target.sh setup failed"

	stage hosts "mask the kernel autoconnect and mint the ghost identity"
	local hostsel
	for hostsel in h1 h2; do
		h "$hostsel" "$WORK/cdc_host.sh mask" >/dev/null ||
			die "masking nvmf-connect on $hostsel failed"
	done
	GHOST_ID=$(hu h1 "uuidgen")
	[ -n "$GHOST_ID" ] || die "minting the ghost hostid failed"
	log "  ghost identity: $GHOST_NQN / $GHOST_ID"

	stage fleet "start cdc0..cdc3 and wait for one scan each"
	start_fleet
}

# ---------------------------------------------------------------------------
# Per-case reset (§9.9)
# ---------------------------------------------------------------------------

# case_reset gives each case a pristine fleet, an empty {p} cdc prefix, its own
# cluster ids and two hosts with no test connections left. The order matters:
# the fleet is stopped BEFORE the wipe, so no instance ever observes a
# half-erased matrix.
case_reset() { # <case>
	CASE=$1
	CID=${CASE_CID[$1]}
	CID2=$(printf '0x1%s' "${CID#0x}")
	stage reset "fleet restart, wipe, host disconnect (cluster $CID/$CID2)"
	stop_fleet
	wipe_entries
	s1 "for d in ${CDC_DIRS[*]}; do : > $WORK/\$d/cdc.log; done"
	start_fleet
	local hostsel
	for hostsel in h1 h2; do
		if [ "$STAS_RUNNING" = 1 ]; then
			# stafd owns the discovery connections here; the fleet restart
			# above already broke and let it re-establish them, and taking
			# them down a second time behind its back is not something it
			# recovers from.
			host_wipe_data "$hostsel"
		else
			host_wipe "$hostsel"
		fi
	done
}

# ---------------------------------------------------------------------------
# Shared case helpers
# ---------------------------------------------------------------------------

# disc_ctrl_name is the kernel's name (nvmeN) for one host's discovery
# controller to one cdc instance — the handle every GENCTR re-read of case L
# goes through.
disc_ctrl_name() { # <h1|h2> <instance>
	subsys_json "$1" | "$JQ" -r \
		--arg ip "$IP1" --arg port "$(cdc_port "$2")" \
		'[.[]?.Subsystems[]? |
		  select(.NQN == "nqn.2014-08.org.nvmexpress.discovery") | .Paths[]? |
		  select(.Address | contains("traddr=" + $ip)) |
		  select(.Address | contains("trsvcid=" + $port)) | .Name] |
		 first // empty'
}

ctrl_live() { # <h1|h2> <nvmeN>
	hu "$1" "test -e /sys/class/nvme/$2/subsysnqn"
}

# read_zeros is the §9.10 step 4 data-path check: 4 KiB off the namespace must
# be 4 KiB of zeros, because the nvmet backend is a dm-zero device. dd is
# invoked WITHOUT iflag=: the lab's uutils dd mishandles it.
read_zeros() { # <h1|h2> <ss>
	local out
	out=$(h "$1" "dd if=$(uuid_path "$2") bs=4096 count=1 2>/dev/null |" \
		"cmp -s - <(head -c 4096 /dev/zero) && echo ZEROS_OK || echo MISMATCH")
	assert_eq "$out" ZEROS_OK "$1: 4 KiB read of $2 against zeros"
	log "  ok: $1 read 4 KiB of zeros from $2"
}

# cdc_msg_count counts one §7 record across one instance's log, with optional
# jq bindings.
cdc_msg_count() { # <dir> <msg> [jq args…]
	local dir=$1 msg=$2
	shift 2
	count_recs "$dir" "select(.msg == \"$msg\")" "$@"
}

connected_hosts() { # <dir> -> the hostnqns that connected, sorted unique
	recsr "$1" 'select(.msg == "host connected") | .hostnqn' | sort -u
}

# ---------------------------------------------------------------------------
# Case S — smoke (§9.10)
# ---------------------------------------------------------------------------

case_smoke() {
	stage put "put ssA: cluster $CID, shard ${E_SHARD[ssa]}, open, port1"
	put_entry ssa "1"
	wait_until "$WAIT_SHORT" "cdc0 to apply the put" \
		cdc_applied_ge cdc0 1

	stage discover "cdc0 serves exactly {ssA}, cdc2 (high half) serves nothing"
	assert_disc h1 0 "" "cdc0 (range $CDC_RANGE_LOW) serves ssA" ssa:1
	assert_disc h1 2 "" "cdc2 (range $CDC_RANGE_HIGH) serves nothing"

	stage noview "no host was active at the put, so no view changed record"
	# The discovers above created and dropped host states, but they happened
	# AFTER the put: DS7 is explicit that a host without a connection is
	# never notified, so the put itself produced no view movement.
	local views
	views=$(recsr cdc0 'select(.msg == "view changed") | .genctr')
	[ -z "$views" ] ||
		die "cdc0 logged a view change with no active host: [$views]"
	log "  ok: cdc0 has no 'view changed' record"

	stage connect "h1 connect-all through cdc0, then read the dm-zero backend"
	connect_all h1 0 >/dev/null
	wait_dev h1 ssa
	local ids
	ids=$(subsys_trsvcids h1 ssa)
	assert_eq "$ids" "$((NVMET_PORT_BASE))" "h1: ssA path trsvcid"
	read_zeros h1 ssa

	stage records "cdc0 logged the connect and the disconnect"
	local n
	n=$(recsr cdc0 'select(.msg == "host connected") | .hostnqn' |
		grep -cx "$H1NQN" || true)
	assert_ge "$n" 1 "cdc0 host connected records for h1"
	n=$(count_recs cdc0 \
		'select(.msg == "host disconnected" and .reason == "closed")')
	assert_ge "$n" 1 "cdc0 host disconnected reason=closed records"

	stage disconnect "disconnect ssA on h1"
	host_wipe h1
	wait_dev_gone h1 ssa

	stage del "delete ssA; cdc0 serves nothing"
	del_entry ssa
	wait_until "$WAIT_SHORT" "cdc0 to serve an empty log" disc_empty h1 0
	assert_disc h1 0 "" "cdc0 after the delete"
}

cdc_applied_ge() { # <dir> <n>
	[ "$(count_recs "$1" \
		'select(.msg == "cdc entry applied" and .op == "put")')" -ge "$2" ]
}

# disc_empty needs the instance to ANSWER with an empty log, which is not the
# same thing as failing to answer.
disc_empty() { # <h1|h2> <instance>
	local out
	out=$(disc_try "$1" "$2") || return 1
	[ "$(printf '%s' "$out" | "$JQ" -r '.records | length')" = 0 ]
}

# ---------------------------------------------------------------------------
# Case M — matrix (§9.11)
# ---------------------------------------------------------------------------

case_matrix() {
	stage put "put the whole §9.5 entry set (ssF in cluster $CID2)"
	put_matrix
	# Each half owns exactly three of the six entries, so three put events
	# per instance is the whole matrix having landed. All FOUR instances are
	# waited for: the grid below asserts against the twins too, and a twin
	# that has not caught up yet would read as a filtering bug.
	local dir
	for dir in "${CDC_DIRS[@]}"; do
		wait_until "$WAIT_SHORT" "$dir to apply its three entries" \
			cdc_applied_ge "$dir" 3
	done

	stage grid "the 4 instances x 3 identities discover grid"
	# H1 sees A (open), B and E (named) in the low half, D in the high half.
	assert_disc h1 0 "" "cdc0 / H1" ssa:1 ssb:2 sse:3
	assert_disc h1 1 "" "cdc1 / H1" ssa:1 ssb:2 sse:3
	assert_disc h1 2 "" "cdc2 / H1" ssd:1 ssd:2
	assert_disc h1 3 "" "cdc3 / H1" ssd:1 ssd:2
	# H2 sees only the open A in the low half; C, D and the cross-cluster F
	# in the high half.
	assert_disc h2 0 "" "cdc0 / H2" ssa:1
	assert_disc h2 1 "" "cdc1 / H2" ssa:1
	assert_disc h2 2 "" "cdc2 / H2" ssc:3 ssd:1 ssd:2 ssf:4
	assert_disc h2 3 "" "cdc3 / H2" ssc:3 ssd:1 ssd:2 ssf:4
	# The ghost is allowed nowhere, so it sees the open entry and nothing
	# else — and a genuinely empty log in the high half.
	assert_disc h1 0 ghost "cdc0 / ghost" ssa:1
	assert_disc h1 1 ghost "cdc1 / ghost" ssa:1
	assert_disc h1 2 ghost "cdc2 / ghost"
	assert_disc h1 3 ghost "cdc3 / ghost"

	stage twins "the twins of a range are byte-identical"
	local a b
	a=$(disc_set h1 0)
	b=$(disc_set h1 1)
	assert_eq "$a" "$b" "cdc0 and cdc1 disagree for H1"
	a=$(disc_set h2 2)
	b=$(disc_set h2 3)
	assert_eq "$a" "$b" "cdc2 and cdc3 disagree for H2"

	stage twopaths "ssD renders TWO records, one per nvmet port"
	local svcids
	svcids=$(disc_raw h2 2 | json_or_empty | "$JQ" -r \
		--arg n "$NQN_PREFIX:ssd" \
		'[.records[]? | select(.subnqn == $n) | .trsvcid] | sort | .[]')
	assert_eq "$svcids" "$(printf '%d\n%d' $((NVMET_PORT_BASE)) \
		$((NVMET_PORT_BASE + 1)))" "ssD trsvcids at cdc2"

	stage connect "both hosts connect-all against all four instances"
	local i ss
	for i in 0 1 2 3; do
		connect_all h1 "$i" >/dev/null
		connect_all h2 "$i" >/dev/null
	done
	# Await every expected node (§9.9) rather than sampling once: connect-all
	# returns before udev has published the by-id links.
	for ss in ssa ssb ssd sse; do wait_dev h1 "$ss"; done
	for ss in ssa ssc ssd ssf; do wait_dev h2 "$ss"; done
	# The absences are checked only after the presences AND a settle, so a
	# device that was merely slow cannot read as a device that was filtered.
	sleep "$SETTLE"
	assert_no_dev h1 "h1 has neither of H2's" ssc ssf
	assert_no_dev h2 "h2 has neither of H1's" ssb sse

	stage multipath "ssD shows two live paths on h2: one subsystem, two CNs"
	svcids=$(subsys_trsvcids h2 ssd)
	assert_eq "$svcids" "$(printf '%d\n%d' $((NVMET_PORT_BASE)) \
		$((NVMET_PORT_BASE + 1)))" "h2: ssD path trsvcids"

	stage disconnect "disconnect every dnv-it subsystem on both hosts"
	host_wipe h1
	host_wipe h2
	wait_dev_gone h1 ssa
	wait_dev_gone h2 ssa
}

# ---------------------------------------------------------------------------
# Case L — lowlevel (§9.12), stas stopped
# ---------------------------------------------------------------------------

case_lowlevel() {
	[ "$STAS_RUNNING" = 0 ] ||
		die "case lowlevel needs the stas daemons stopped"

	stage put "put ssA (open, port1) and ssB (H1, port2)"
	put_entry ssa "1"
	put_entry ssb "2" "$H1NQN"
	wait_until "$WAIT_SHORT" "cdc0 to apply both entries" cdc_applied_ge cdc0 2

	stage persist "open one persistent discovery connection per host to cdc0"
	h h1 "nvme discover -t tcp -a $IP1 -s $(cdc_port 0)" \
		"--persistent --keep-alive-tmo 5 -o json" >/dev/null ||
		die "h1: persistent discover failed"
	h h2 "nvme discover -t tcp -a $IP1 -s $(cdc_port 0)" \
		"--persistent --keep-alive-tmo 5 -o json" >/dev/null ||
		die "h2: persistent discover failed"
	local devx devy
	devx=$(disc_ctrl_name h1 0)
	devy=$(disc_ctrl_name h2 0)
	[ -n "$devx" ] || die "h1 has no persistent discovery controller to cdc0"
	[ -n "$devy" ] || die "h2 has no persistent discovery controller to cdc0"
	log "  h1 -> $devx, h2 -> $devy"

	local g1 g2
	g1=$(disc_dev_genctr h1 "$devx")
	g2=$(disc_dev_genctr h2 "$devy")
	assert_dev_disc h1 "$devx" "h1 baseline" ssa:1 ssb:2
	assert_dev_disc h2 "$devy" "h2 baseline" ssa:1
	log "  baseline genctr: h1 $g1, h2 $g2"

	stage capture "start the udev captures on both hosts"
	uevents_start h1 >/dev/null
	uevents_start h2 >/dev/null

	stage gain "put ssE (H1 only): h1 is impacted, h2 must see nothing"
	put_entry sse "3" "$H1NQN"
	wait_aen h1 "$devx" 1
	local g1b g2b
	g1b=$(disc_dev_genctr h1 "$devx")
	assert_gt "$g1b" "$g1" "h1 genctr after gaining ssE"
	assert_dev_disc h1 "$devx" "h1 after gaining ssE" ssa:1 ssb:2 sse:3
	# The §0 #5 negative: a change invisible to h2 before AND after is not an
	# AEN and not even a GENCTR move for it.
	sleep "$SETTLE"
	assert_aen_count h2 "$devy" 0 "h2 AENs after an h1-only change"
	g2b=$(disc_dev_genctr h2 "$devy")
	assert_eq "$g2b" "$g2" "h2 genctr after an h1-only change"
	assert_dev_disc h2 "$devy" "h2 unchanged" ssa:1

	stage widen "widen ssB to [H1, H2]: h2 gains it, h1's BYTES do not move"
	put_entry ssb "2" "$H1NQN" "$H2NQN"
	wait_aen h2 "$devy" 1
	local g2c
	g2c=$(disc_dev_genctr h2 "$devy")
	assert_gt "$g2c" "$g2b" "h2 genctr after gaining ssB"
	assert_dev_disc h2 "$devy" "h2 after gaining ssB" ssa:1 ssb:2
	# DS6 is defined on RENDERED CONTENT: allowed_hosts is not log-page
	# content, so an edit that keeps h1's membership moves nothing for h1.
	# This is the only place the rule is observable.
	sleep "$SETTLE"
	assert_aen_count h1 "$devx" 1 "h1 AENs after a membership-preserving edit"
	assert_eq "$(disc_dev_genctr h1 "$devx")" "$g1b" \
		"h1 genctr after a membership-preserving edit"

	stage move "hand ssE from H1 to H2: h1 loses it, h2 gains it"
	put_entry sse "3" "$H2NQN"
	wait_aen h1 "$devx" 2
	wait_aen h2 "$devy" 2
	assert_dev_disc h1 "$devx" "h1 after losing ssE" ssa:1 ssb:2
	assert_dev_disc h2 "$devy" "h2 after gaining ssE" ssa:1 ssb:2 sse:3
	assert_gt "$(disc_dev_genctr h1 "$devx")" "$g1b" "h1 genctr after the move"
	assert_gt "$(disc_dev_genctr h2 "$devy")" "$g2c" "h2 genctr after the move"

	stage open "delete the open ssA: an empty allowed_hosts impacts everyone"
	del_entry ssa
	wait_aen h1 "$devx" 3
	wait_aen h2 "$devy" 3
	assert_dev_disc h1 "$devx" "h1 after deleting ssA" ssb:2
	assert_dev_disc h2 "$devy" "h2 after deleting ssA" ssb:2 sse:3

	stage keepalive "3 x the 5 s KATO: both controllers stay live"
	sleep 16
	ctrl_live h1 "$devx" || die "h1: $devx died during the keep-alive window"
	ctrl_live h2 "$devy" || die "h2: $devy died during the keep-alive window"
	assert_eq "$(count_recs cdc0 \
		'select(.msg == "host disconnected" and .reason == "keep_alive")')" \
		0 "cdc0 keep-alive expiries"
	log "  ok: both persistent controllers survived 3 x KATO"

	stage teardown "stop the captures and drop both persistent controllers"
	uevents_stop h1
	uevents_stop h2
	local closed_before
	closed_before=$(count_recs cdc0 \
		'select(.msg == "host disconnected" and .reason == "closed")')
	h h1 "nvme disconnect -d $devx" >/dev/null || true
	h h2 "nvme disconnect -d $devy" >/dev/null || true
	wait_until "$WAIT_SHORT" "cdc0 to log two more closed disconnects" \
		closed_ge cdc0 $((closed_before + 2))
}

closed_ge() { # <dir> <n>
	[ "$(count_recs "$1" \
		'select(.msg == "host disconnected" and .reason == "closed")')" \
		-ge "$2" ]
}

# ---------------------------------------------------------------------------
# Case T — stas (§9.13)
# ---------------------------------------------------------------------------

case_stas() {
	stage put "put ssA, ssB, ssC, ssD, ssE and the cross-cluster ssF"
	put_matrix
	wait_until "$WAIT_SHORT" "cdc0 to apply its three entries" \
		cdc_applied_ge cdc0 3

	stage start "start stafd and stacd on both hosts"
	stas_start_both

	stage converge "each host holds four persistent discovery connections"
	wait_stas_converged h1
	wait_stas_converged h2
	local dir
	for dir in "${CDC_DIRS[@]}"; do
		wait_until "$WAIT_STAS" "$dir: both hostnqns connected" \
			both_hosts_connected "$dir"
	done
	log "  ok: all four instances saw both hosts"

	stage autoconnect "the data connections appear with no nvme command at all"
	local ss
	for ss in ssa ssb ssd sse; do wait_dev h1 "$ss"; done
	for ss in ssa ssc ssd ssf; do wait_dev h2 "$ss"; done
	assert_no_dev h1 "h1 got nothing of H2's" ssc ssf
	assert_no_dev h2 "h2 got nothing of H1's" ssb sse
	local svcids
	svcids=$(subsys_trsvcids h1 ssd)
	assert_eq "$svcids" "$(printf '%d\n%d' $((NVMET_PORT_BASE)) \
		$((NVMET_PORT_BASE + 1)))" "h1: ssD path trsvcids"
	svcids=$(subsys_trsvcids h2 ssd)
	assert_eq "$svcids" "$(printf '%d\n%d' $((NVMET_PORT_BASE)) \
		$((NVMET_PORT_BASE + 1)))" "h2: ssD path trsvcids"

	stage gain "put ssX (H2 only): h2 auto-connects it, h1 stays put"
	put_entry ssx "4" "$H2NQN"
	wait_dev h2 ssx
	sleep "$SETTLE"
	assert_no_dev h1 "h1 after ssX appeared for H2" ssx

	stage repoint "move ssE from nvmet port3 to port4 the ReplaceCntlr way"
	# New path first, old path last: the graceful order an unlink under a
	# live connection would otherwise punish with a DNR-refused controller.
	s2 "$WORK/cdc_target.sh link sse 4" >/dev/null ||
		die "linking sse to port 4 failed"
	put_entry sse "4" "$H1NQN"
	# The predicate checks the device on every poll, so a node that vanishes
	# for even one second during the move never satisfies it: "the uuid-5
	# device survives throughout" is measured, not assumed.
	wait_until "$WAIT_STAS" "h1: ssE re-pointed to $((NVMET_PORT_BASE + 3))" \
		ss_repointed h1 sse "$((NVMET_PORT_BASE + 3))"
	log "  ok: h1's ssE moved to $((NVMET_PORT_BASE + 3)) and the device survived"
	s2 "$WORK/cdc_target.sh unlink sse 3" >/dev/null ||
		die "unlinking sse from port 3 failed"

	stage autodisconnect "delete ssB: h1's connection to it goes away"
	del_entry ssb
	wait_dev_gone h1 ssb

	stage wipe "wipe the prefix: both hosts converge to zero dnv-it subsystems"
	wipe_entries
	wait_until "$WAIT_STAS" "h1 to hold no dnv-it subsystem" \
		no_test_subsystems h1
	wait_until "$WAIT_STAS" "h2 to hold no dnv-it subsystem" \
		no_test_subsystems h2

	stage restore "put s2 back into the §9.5 setup shape"
	s2 "$WORK/cdc_target.sh link sse 3" >/dev/null ||
		die "restoring the sse port-3 link failed"
	s2 "$WORK/cdc_target.sh unlink sse 4" >/dev/null ||
		die "removing the sse port-4 link failed"
}

both_hosts_connected() { # <dir>
	local got
	got=$(connected_hosts "$1")
	printf '%s\n' "$got" | grep -qx "$H1NQN" || return 1
	printf '%s\n' "$got" | grep -qx "$H2NQN" || return 1
	return 0
}

ss_paths_are() { # <h1|h2> <ss> <trsvcid…>
	local hostsel=$1 ss=$2
	shift 2
	local want got
	want=$(printf '%s\n' "$@" | sort)
	got=$(subsys_trsvcids "$hostsel" "$ss")
	[ "$got" = "$want" ]
}

# ss_repointed is ss_paths_are plus the survival half: the device node must be
# there on EVERY poll, so a subsystem that was disconnected and reconnected
# rather than re-pointed is caught.
ss_repointed() { # <h1|h2> <ss> <trsvcid…>
	local hostsel=$1 ss=$2
	have_dev "$hostsel" "$ss" ||
		die "$hostsel lost the $ss device during the re-point"
	ss_paths_are "$@"
}

# ---------------------------------------------------------------------------
# Case H — ha (§9.14)
# ---------------------------------------------------------------------------

connected_hosts_since() { # <dir> <baseline record count>
	recsr "$1" 'select(.msg == "host connected") | .hostnqn' |
		tail -n +$(($2 + 1)) | sort -u
}

both_hosts_connected_since() { # <dir> <baseline>
	local got
	got=$(connected_hosts_since "$1" "$2")
	printf '%s\n' "$got" | grep -qx "$H1NQN" || return 1
	printf '%s\n' "$got" | grep -qx "$H2NQN" || return 1
	return 0
}

connects_at() { # <dir>
	count_recs "$1" 'select(.msg == "host connected")'
}

disc_ctrls_are() { # <h1|h2> <n>
	[ "$(disc_ctrl_count "$1")" = "$2" ]
}

case_ha() {
	if [ "$STAS_RUNNING" != 1 ]; then
		stage start "start stafd and stacd on both hosts"
		stas_start_both
	fi

	stage put "put ssA (open, port1) and ssC (H2, port3)"
	put_entry ssa "1"
	put_entry ssc "3" "$H2NQN"
	wait_stas_converged h1
	wait_stas_converged h2
	wait_dev h1 ssa
	wait_dev h2 ssa
	wait_dev h2 ssc
	assert_no_dev h1 "h1 was never allowed ssC" ssc

	stage kill "SIGKILL cdc0: each host keeps three live discovery paths"
	sig_cdc cdc0 KILL
	unset "RUNNING[cdc0]"
	wait_until "$WAIT_SHORT" "cdc0 to exit" cdc_gone cdc0
	wait_until "$WAIT_STAS" "h1 down to three live discovery paths" \
		disc_ctrls_are h1 3
	wait_until "$WAIT_STAS" "h2 down to three live discovery paths" \
		disc_ctrls_are h2 3

	stage twin "the surviving twin alone serves the low half"
	put_entry ssb "2" "$H1NQN"
	wait_dev h1 ssb
	wait_disc "$WAIT_STAS" h1 1 "" "cdc1 alone while cdc0 is down" ssa:1 ssb:2

	stage restart "restart cdc0: the twins re-converge from etcd alone"
	local base
	base=$(scans_at cdc0)
	start_cdc 0
	wait_scan cdc0 "$base"
	# Let stafd finish reclaiming its fourth connection before asking the
	# kernel for a one-shot controller to the same endpoint.
	wait_stas_converged h1
	wait_until "$WAIT_STAS" "cdc0 and cdc1 to agree after cdc0's restart" \
		twins_agree h1 0 1
	wait_disc "$WAIT_STAS" h1 0 "" "the restarted cdc0 serves the whole range" \
		ssa:1 ssb:2

	stage fleet "SIGKILL the whole fleet under live hosts and relaunch it"
	declare -A conn_base=() scan_base=()
	local dir i
	for dir in "${CDC_DIRS[@]}"; do
		conn_base[$dir]=$(connects_at "$dir")
		scan_base[$dir]=$(scans_at "$dir")
	done
	for dir in "${CDC_DIRS[@]}"; do
		ok_or_true sig_cdc "$dir" KILL
		unset "RUNNING[$dir]"
	done
	for dir in "${CDC_DIRS[@]}"; do
		wait_until "$WAIT_SHORT" "$dir to exit" cdc_gone "$dir"
	done
	# The data connections are to s2's nvmet, not to the cdc fleet, so they
	# must be untouched by the fleet dying (DS11: nothing about a discovery
	# outage disturbs an established I/O path).
	assert_dev h1 "h1 kept its data connections across the fleet outage" ssa ssb
	assert_dev h2 "h2 kept its data connections across the fleet outage" ssa ssc
	for i in 0 1 2 3; do start_cdc "$i"; done
	for dir in "${CDC_DIRS[@]}"; do
		wait_scan "$dir" "${scan_base[$dir]}"
	done
	for dir in "${CDC_DIRS[@]}"; do
		wait_until "$WAIT_STAS" "$dir: both hosts reconnected after the restart" \
			both_hosts_connected_since "$dir" "${conn_base[$dir]}"
	done
	log "  ok: stas re-established 4 x 2 discovery connections"
	assert_dev h1 "h1's data connections are still up" ssa ssb
	assert_dev h2 "h2's data connections are still up" ssa ssc

	stage aen "the AEN path works again after the restart"
	put_entry sse "3" "$H1NQN"
	wait_dev h1 sse
	sleep "$SETTLE"
	assert_no_dev h2 "h2 was never allowed ssE" sse
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/cdc_test.sh [--only <case>] [--cleanup-only] \\
           user@<s1> user@<s2> user@<h1> user@<h2>

  s1  etcd + the dnv-cdc fleet   (plain user, NO sudo needed)
  s2  the nvmet target server    (passwordless sudo)
  h1  host 1                     (passwordless sudo)
  h2  host 2                     (passwordless sudo)

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
	[ "${#positional[@]}" -eq 4 ] || usage
	S1=${positional[0]}
	S2=${positional[1]}
	H1=${positional[2]}
	H2=${positional[3]}
	IP1=${S1##*@}
	IP2=${S2##*@}
	IPH1=${H1##*@}
	IPH2=${H2##*@}
	[ -n "$IP1" ] && [ -n "$IP2" ] && [ -n "$IPH1" ] && [ -n "$IPH2" ] || usage
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
	TMPDIR_LOCAL=$(mktemp -d)
	trap on_exit EXIT
	RUN_START=$(date -u +'%Y-%m-%d %H:%M:%S')
	log "driver: $(hostname)"
	log "  s1 (etcd + cdc fleet): $S1   ip $IP1"
	log "  s2 (nvmet target):     $S2   ip $IP2"
	log "  h1:                    $H1   ip $IPH1"
	log "  h2:                    $H2   ip $IPH2"
	log "NOTE: this suite occupies all four lab VMs; no other dnv suite may"
	log "      run anywhere in the lab while it does (§9.2)."

	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		STAGE="cleanup-only"
		log ""
		log "=== cleanup only"
		local target
		for target in "$S1" "$S2" "$H1" "$H2"; do
			ssh_to "$target" "true" || die "passwordless ssh to $target failed"
		done
		cleanup 1
		return 0
	fi

	preflight_driver
	STAGE="start cleanup"
	log ""
	log "=== start-of-run cleanup (unconditional)"
	cleanup 0
	preflight_servers
	setup

	local name
	for name in "${CASES[@]}"; do
		if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then continue; fi
		case_reset "$name"
		"case_$name"
	done
}

main "$@"
