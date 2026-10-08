#!/usr/bin/env bash
#
# worker_test.sh — the `dnv-worker` integration test of doc/dnv-worker.md,
# Integration test plan. One server, a real single-node etcd, a real
# three-process worker fleet and the plan's fake dn/cn agents, driven from this
# machine over ssh by integtest/workerctl (the gateway's write path) and
# integtest/fakeagent.
#
#   bash integtest/worker_test.sh [--only <case>] [--cleanup-only] user@ip
#
# The plan's cases, in order, each in its own cluster `it-<case>` and each
# after a fleet restart: smoke, revision, health, bitmap, reaction, failover,
# drain, vote, handoff. Cleanup runs unconditionally at the start and, on
# success only, at the end: a failing run leaves etcd's data, every log and
# every behavior file in place and dumps the diagnostics.
#
# NO SUDO anywhere: nothing in this suite needs root. Everything the script
# creates lives under $WORK on the server, and cleanup removes exactly that.
#
# Log reading: every worker/agent log is one JSON object per line, and a log
# being appended to while we `cat` it can end in a torn line, so every read
# goes through `jq -R 'fromjson? // empty'` (recs/recsr below) — a partial
# last line is dropped, never fails an assertion.
#
# Signals: the three workers share one command line, so every process's pid is
# recorded from `$!` into $WORK/<dir>/pid at launch and every signal goes by
# pid. The `pkill -f` forms appear in cleanup() only.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
CACHE_DIR="$BIN_DIR/cache"
WORKER_BIN="$REPO_ROOT/bin/dnv-worker"
WORKERCTL_BIN="$BIN_DIR/workerctl"
FAKEAGENT_BIN="$BIN_DIR/fakeagent"

# The pinned etcd release. The tarball is downloaded once into
# integtest/bin/cache and verified against this digest; a cached tarball whose
# digest already matches is not re-downloaded, so a fresh checkout works and a
# repeat run costs nothing.
ETCD_VERSION=v3.6.14
ETCD_DIST="etcd-$ETCD_VERSION-linux-amd64"
ETCD_URL="https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/$ETCD_DIST.tar.gz"
ETCD_SHA256=ffe840ff9295808e88cce2794a18a5ac87f12a5203c8314d0bf6aa119b41bac5
ETCD_TAR="$CACHE_DIR/$ETCD_DIST.tar.gz"
# Every etcd serving dnv MUST run with --max-txn-ops at least
# common.EtcdMaxTxnOps (dnv-worker.md, Integration test plan, Topology). The
# value is NOT typed here — read_constants() fills it at preflight from
# `workerctl constants`, which prints the Go constants as JSON — so the suite
# cannot drift from the deployment requirement it enforces.
#
# Several transactions in dnv are above etcd's default cap of 128 with a size
# that named constants bound. The one that SIZES the requirement is
# CreateStoragePool at its widest shape — MaxSliceCntPerSp slices,
# MaxAllocLegPerGrp legs per group (raid1) and MaxCntlrCntPerSp cntlrs
# (architecture.md, Storage pools); the sp drain's D2 batch (SPD10) and a
# created-flip transaction of MaxFlipCreatedPerTxn tds (RW19) are two more.
# Their compare counts, and the factors that multiply into them, are asserted
# from the named constants in gateway/txnbudget_test.go, not restated here.
# This suite runs no gateway — `wctl put-sp` plants its pools directly — so
# the create never happens here, and case G commits batches of the drain
# family well below the ceiling — its widest slice is 21 groups over two DNs,
# where a maximum batch touches a DN per leg and per spare of MaxDelGrpPerTxn
# groups — and clone-drain batches, which fit etcd's default anyway (CLD11).
# Its created flips carry one td each: no pool here holds a second.
ETCD_MAX_TXN_OPS=

# The timers case H runs at, read the same way by read_constants(): the
# check interval a cluster created without one stores, the primary threshold
# a pool created without one reads, and the wait of the demotion hold
# (dnv-worker.md RW22). Case H sizes from them every wait and every bound that
# rests on a check round, a threshold or the demotion hold.
DEFAULT_HC_INTERVAL=
DEFAULT_PRIMARY_UNHEALTHY=
DEMOTION_HOLD=

WORK=/var/tmp/dnv-worker-integtest

# Ports: nine in total, none shared with the two agent suites.
ETCD_CLIENT_PORT=12379
# common.DnvPrefix — the first field of every dnv etcd key (architecture.md,
# Key grammar).
DNV_PREFIX=dnv
ETCD_PEER_PORT=12380
DN_PORT_BASE=29600 # dn0..dn3 -> 29600..29603
CN_PORT_BASE=29700 # cn0..cn2 -> 29700..29702
ALL_PORTS=(12379 12380 29600 29601 29602 29603 29700 29701 29702)

# Test-time constants (dnv-worker.md, Integration test plan, Topology).
VOTE_INTERVAL=2
VOTE_GRACE=6
HEALTH_INTERVAL=1     # every health_check_conf.*_interval
THRESHOLDS=2,4,3,6    # primary, cntlr, side, leg
LWM=50                # low_water_mark_pct
EXTENT_SIZE=67108864  # common.MinDnExtSize
BLOCK_SIZE=1048576    # 1 MiB, the dm-thin data block
CHUNK_BLOCKS=128      # bitmap_chunk_block_cnt
# dm-thin's metadata block size, fixed by the kernel at 4 KiB: the FIRST
# fraction of a thin-pool status line (`<used_meta>/<total_meta>`, see
# thin_pool_line) counts THESE; the SECOND fraction is in BLOCK_SIZE blocks.
THIN_META_BLOCK_SIZE=4096
WAIT_SHORT=5
WAIT_MEMBERSHIP=20
WAIT_SYNCUP=65

# The group geometry (architecture.md, Group on-leg layout: meta region, data
# region, health block), filled by read_geometry() at preflight from `workerctl
# geometry`, which calls model.GroupBlocks — the one implementation of the
# formula (MD6) — at this suite's EXTENT_SIZE / BLOCK_SIZE / CHUNK_BLOCKS and
# the ext_cnt of each group kind. Case D is the only case that reads them, and
# its sp0 is built `--group 1:1:meta:1:raid1 --group 1:2:data:2:raid1`: that is
# where the two ext_cnts asked for, and the raid1, come from.
#
# The formula, as EXPLANATION only (the shell computes none of it):
#
#   total_group_blocks = ext_cnt * extent_size / block_size
#   bitmap_bits        = ceil(ext_cnt * extent_size / (chunk * block_size))
#   bitmap_bytes       = 256 + ceil(bitmap_bits / 8)
#   meta_blocks        = 1 (md sb) + ceil(bitmap_bytes / block_size)
#                          + 1 (health)                            [raid1]
#   data_blocks        = total_group_blocks - meta_blocks
#
# At (64 MiB, 1 MiB, 128 blocks) that is meta_blocks 3 for both kinds, with
# data_blocks 125 for a data group (ext_cnt 2) and 61 for a meta group
# (ext_cnt 1) — the values case D's inline comments quote when they work out
# which used/total pair breaches a watermark, so changing any of the three
# parameters above means re-reading those comments too.
#
# META_BLOCKS_PER_GRP is what one meta group contributes to the pool's REPORTED
# metadata total, which dm-thin counts in its own fixed blocks; the data half
# of that line is in BLOCK_SIZE blocks and case D sums DATA_GRP_DATA_BLOCKS for
# it directly.
GRP_META_BLOCKS=
DATA_GRP_DATA_BLOCKS=  # ext_cnt 2
META_GRP_DATA_BLOCKS=  # ext_cnt 1
META_BLOCKS_PER_GRP=   # META_GRP_DATA_BLOCKS * BLOCK_SIZE / THIN_META_BLOCK_SIZE

# Sub-object ids the script assigns. workerctl advances SpConf.next_id
# past all of them, so a worker reaction allocates ids ABOVE these.
TD_ID=0x10
SS_ID=0x20
NS_ID=0x21
CLONE_ID=0x30

NQN_PREFIX=nqn.2024-01.io.dnv-it

CASES=(smoke revision health bitmap reaction failover drain vote handoff)

DN_DIRS=(dn0 dn1 dn2 dn3)
CN_DIRS=(cn0 cn1 cn2)
WORKER_DIRS=(w1 w2 w3 w4)

# ---------------------------------------------------------------------------
# Mutable state
# ---------------------------------------------------------------------------

TARGET=""
IP=""
JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE=setup
CLUSTER=dnv-it-none # never empty: `ctl` interpolates it into --cluster
CID=""
TRACE=it-setup
STAGE="(startup)"
SETUP_DONE=0
QUIET=0

declare -A PID=()      # dir -> pid recorded from $! at launch
declare -A RUNNING=()  # dir -> 1 while we believe the process is alive
declare -A REG_BASE=() # dir -> `worker registered` records held before a launch
SEEN_WORKERS=()        # worker dirs whose current log holds records

OWN_FILE=""        # driver-side snapshot of every shard owned/released record
OWN_BEFORE_FILE="" # a saved ownership table, for case E's stability check
OWN_JOIN_FILE=""   # the raw owned/released snapshot taken before case E's join

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id
# `it-<case>-<step>`, which every workerctl call of the step then stamps on its
# etcd records.
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
assert_ne() { [ "$1" != "$2" ] || die "$3: got '$1', want anything else"; }

assert_ge() {
	[ "$1" -ge "$2" ] || die "$3: got $1, want >= $2"
}

assert_between() {
	{ [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]; } ||
		die "$4: got $1, want $2..$3"
}

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

# assert_field reads one field out of a workerctl reply. protojson renders
# 64-bit fields as JSON STRINGS and 32-bit ones as numbers, so every numeric
# comparison below goes through `tostring` and compares text.
assert_field() { # <json> <filter> <want> <label>
	assert_eq "$(jq_of "$1" "$2")" "$3" "$4"
}

on_exit() {
	local rc=$?
	trap - EXIT
	if [ "$rc" -eq 0 ]; then
		if [ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ]; then
			log ""
			log "=== end-of-run cleanup (success)"
			cleanup
		fi
		log ""
		log "PASS"
	else
		log ""
		log "########## diagnostics ##########"
		diagnostics || true
		log ""
		log "debris left in place on $TARGET; failing stage '$STAGE'"
	fi
	[ -n "$OWN_FILE" ] && rm -f "$OWN_FILE"
	[ -n "$OWN_BEFORE_FILE" ] && rm -f "$OWN_BEFORE_FILE"
	[ -n "$OWN_JOIN_FILE" ] && rm -f "$OWN_JOIN_FILE"
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution
# ---------------------------------------------------------------------------

# sshw echoes the command and runs it on the server as the plain user. QUIET is
# raised while wait_until/assert_none_for poll, so a 20 s poll does not bury
# the transcript; nothing else changes.
sshw() {
	local cmd="$*"
	[ "$QUIET" -eq 1 ] || log "[server] $cmd"
	ssh "${SSH_OPTS[@]}" "$TARGET" "$cmd"
}

sshw_ok() { sshw "$@" || true; }

# ctl is the driver wrapper: every call carries the endpoints, the
# case's cluster and the stage's trace id.
#
# Each argument is quoted for the REMOTE shell with printf %q before the
# command string is built. Without that, an argument containing a space is
# re-split by the remote shell: an etcd key is space-joined (architecture.md,
# Key grammar), so `ctl get --key "dnv cdc <cid> <shard> <sp> <ss>"`
# reached workerctl as `--key dnv` plus five stray words and died — which,
# under `set -e` inside a command substitution, aborted the whole run.
ctl() {
	local quoted
	quoted=$(printf '%q ' "$@")
	sshw "$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CLUSTER --trace-id $TRACE $quoted"
}

# rlog is the log reader: `ssh … cat <path>`, so every assertion is
# parsed by the driver's jq and the server needs no jq of its own. A missing
# file yields empty output rather than a failure, because `set -o pipefail`
# would otherwise turn "the log does not exist yet" into a script abort inside
# a polling predicate.
# A read that fails for any reason OTHER than "the file does not exist yet"
# must not read as an empty log: every count-based negative assertion
# (`assert_eq "$(reqs dn0 PushMigrBitmap)" 0`, every assert_none_for) would
# then pass on a broken ssh. So probe first and let a real failure propagate.
rlog() {
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"if [ -e $(printf '%q' "$1") ]; then cat -- $(printf '%q' "$1"); fi"
}

wpath() { printf '%s/%s/worker.log' "$WORK" "$1"; }
apath() { printf '%s/%s/agent.log' "$WORK" "$1"; }

# recs/recsr stream one log through a jq filter. `-R` plus `fromjson?` makes a
# torn last line disappear instead of failing the whole read.
recs() { # <path> <filter> [jq args…]
	local path=$1 filter=$2
	shift 2
	rlog "$path" | "$JQ" -cR "$@" "fromjson? // empty | $filter"
}

recsr() { # <path> <filter> [jq args…]  — raw output
	local path=$1 filter=$2
	shift 2
	rlog "$path" | "$JQ" -rR "$@" "fromjson? // empty | $filter"
}

# count_recs prints how many records of one log match a filter — or NOTHING,
# with a non-zero status, when the read failed (the ssh, the cat or the jq).
# `wc -l` counts an empty stream as 0 either way, so a failure printed as a
# count would read as "no such record" and satisfy every negative built on
# it; pipefail carries rlog's and jq's status through the pipe, and it is
# checked here. A `die` here would end only the caller's `$( )` subshell,
# so the callers check the status instead: every comparison goes through
# count_ge/count_gt below, a count sampled into a variable in a case's body
# stops the run under `set -e`, and one compared in place (`assert_eq
# "$(reqs …)" 0`) fails on its empty output.
count_recs() { # <path> <filter> [jq args…]
	local n
	n=$(recs "$@" | wc -l | tr -d ' \n') || return 1
	printf '%s' "$n"
}

# wcount counts matching records over EVERY worker log that holds records for
# the current case (the logs are truncated by each fleet restart, so this is
# per-case by construction). A failed read of any one log fails the count.
wcount() { # <filter> [jq args…]
	local filter=$1
	shift
	local w total=0 n
	for w in "${SEEN_WORKERS[@]}"; do
		n=$(count_recs "$(wpath "$w")" "$filter" "$@") || return 1
		total=$((total + n))
	done
	printf '%s' "$total"
}

# count_ge / count_gt compare what one of the counters prints with a bound,
# and are where a FAILED read becomes a failure: the counter's status is
# checked in the predicate's own shell, and a failure dies there. Compared as
# it stands, the empty output would merely be "false" — which wait_until polls
# through, but which assert_none_for takes for "nothing happened", passing a
# negative on a dropped ssh. Every predicate that compares such a count is
# built on them.
count_cmp() { # <-ge|-gt> <n> <counter> [args…]
	local op=$1 n=$2 cnt
	shift 2
	cnt=$("$@") ||
		die "a count could not be read ($(printf '%s' "$*" | tr -s ' \t\n' ' ')): a read that failed is not a count of 0"
	[ "$cnt" "$op" "$n" ]
}

count_ge() { count_cmp -ge "$@"; }
count_gt() { count_cmp -gt "$@"; }

wcount_ge() { # <n> <filter> [jq args…]
	local n=$1
	shift
	count_ge "$n" wcount "$@"
}

# ---------------------------------------------------------------------------
# Polling (dnv-worker.md, Integration test plan, What a pass means)
# ---------------------------------------------------------------------------

# wait_until polls twice a second until the command exits 0, else dies.
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
		sleep 0.5
	done
}

# assert_none_for is wait_until's negative twin: sleep, then require the
# command to exit NON-zero. It carries a label for the same reason wait_until
# does — a bare "assertion failed" in a suite this size is unusable.
assert_none_for() { # <secs> <label> <cmd…>
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
# Ownership table
# ---------------------------------------------------------------------------

# running_workers lists the workers this script believes are alive, in w1..w4
# order. A SIGKILLed worker's log still ends in `shard owned` records — it
# never got to release anything — so ownership is always computed over an
# EXPLICIT set of workers, never over every log that happens to exist.
running_workers() {
	local w
	for w in "${WORKER_DIRS[@]}"; do
		if [ "${RUNNING[$w]-}" = 1 ]; then printf '%s\n' "$w"; fi
	done
}

# refresh_owners snapshots every `shard owned` / `shard released` record of the
# named workers' logs into one driver-side file, tagged with the worker.
refresh_owners() { # <worker…>
	: >"$OWN_FILE"
	local w
	for w in "$@"; do
		recsr "$(wpath "$w")" \
			'select(.msg == "shard owned" or .msg == "shard released")
			 | "\(.role) \(.shard) \(if .msg == "shard owned" then "owned" else "released" end)"' |
			sed "s/^/$w /" >>"$OWN_FILE"
	done
}

# owners_from FOLDS the snapshot in log order: a shard can be owned, released
# and owned again inside one case (a fence, a join), so the answer is the final
# state of the fold, never a count of records.
owners_from() { # <role> -> "<shard> <worker>" lines, shard-sorted
	awk -v role="$1" '
		$2 == role && $4 == "owned"    { held[$1 SUBSEP $3] = 1; next }
		$2 == role && $4 == "released" { held[$1 SUBSEP $3] = 0; next }
		END {
			for (k in held) {
				if (held[k]) {
					split(k, a, SUBSEP)
					print a[2], a[1]
				}
			}
		}
	' "$OWN_FILE" | sort
}

# owners is the ownership helper: the shards each LIVE worker currently holds
# for one role, taken fresh.
owners() { # <role>
	local live=()
	mapfile -t live < <(running_workers)
	refresh_owners "${live[@]}"
	owners_from "$1"
}

owner_of() { # <role> <shard>
	owners "$1" | awk -v s="$2" '$1 == s { print $2 }'
}

# assert_owners checks the ownership table: for every role, the 256 shards are
# covered exactly once, by exactly the expected workers, and no worker holds
# fewer than <min>.
assert_owners() { # <min> <worker…>
	local min=$1
	shift
	local want
	want=$(printf '%s\n' "$@" | sort | tr '\n' ' ')
	refresh_owners "$@"
	local role table rows uniq got w cnt
	for role in dn cn sp; do
		table=$(owners_from "$role")
		rows=$(printf '%s\n' "$table" | awk 'NF' | wc -l | tr -d ' ')
		uniq=$(printf '%s\n' "$table" | awk 'NF { print $1 }' | sort -u | wc -l | tr -d ' ')
		assert_eq "$rows" 256 "role $role: owned (role, shard) rows"
		assert_eq "$uniq" 256 "role $role: distinct shards owned (no shard twice)"
		got=$(printf '%s\n' "$table" | awk 'NF { print $2 }' | sort -u | tr '\n' ' ')
		assert_eq "$got" "$want" "role $role: the set of owners"
		for w in "$@"; do
			cnt=$(printf '%s\n' "$table" | awk -v w="$w" 'NF && $2 == w' | wc -l | tr -d ' ')
			assert_ge "$cnt" "$min" "role $role: shards owned by $w"
		done
	done
	log "  ownership table ok (256 shards per role over: $want)"
}

# ---------------------------------------------------------------------------
# Seeds, requests and other log queries (dnv-worker.md, Integration test plan,
# What a pass means)
# ---------------------------------------------------------------------------

# seed_of is the seed of the LATEST `worker registered` record — a fenced
# worker re-registers under a new seed and that is the one that matters.
seed_of() { # <w>
	recsr "$(wpath "$1")" 'select(.msg == "worker registered") | .seed' | tail -n 1
}

seed8() { seed_of "$1" | cut -c1-8; }

# reqs counts the requests one fake received for a method: `grpc server
# request` for the unary calls, `grpc server recv` for the Check* streams. The
# optional filter is applied to `.data`, the logged request message.
reqs() { # <agent> <method> [<filter over .data>] [jq args…]
	local agent=$1 method=$2 filter=${3:-true}
	if [ $# -ge 3 ]; then shift 3; else shift 2; fi
	count_recs "$(apath "$agent")" \
		"select(.msg == \"grpc server request\" or .msg == \"grpc server recv\")
		 | select((.method | split(\"/\") | last) == \"$method\")
		 | select(has(\"data\")) | .data | select($filter)" "$@"
}

# last_req prints the newest matching request's `data`.
last_req() { # <agent> <method> [<filter>] [jq args…]
	local agent=$1 method=$2 filter=${3:-true}
	if [ $# -ge 3 ]; then shift 3; else shift 2; fi
	recs "$(apath "$agent")" \
		"select(.msg == \"grpc server request\" or .msg == \"grpc server recv\")
		 | select((.method | split(\"/\") | last) == \"$method\")
		 | select(has(\"data\")) | .data | select($filter)" "$@" | tail -n 1
}

# replies counts one fake's `grpc server reply` / `grpc server send` records.
replies() { # <agent> <method> [<filter over .data>] [jq args…]
	local agent=$1 method=$2 filter=${3:-true}
	if [ $# -ge 3 ]; then shift 3; else shift 2; fi
	count_recs "$(apath "$agent")" \
		"select(.msg == \"grpc server reply\" or .msg == \"grpc server send\")
		 | select((.method | split(\"/\") | last) == \"$method\")
		 | select(has(\"data\")) | .data | select($filter)" "$@"
}

req_ge() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	count_ge "$n" reqs "$@"
}

reply_ge() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	count_ge "$n" replies "$@"
}

reply_gt() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	count_gt "$n" replies "$@"
}

# stream_closes counts `grpc server stream close` records for one method.
stream_closes() { # <agent> <method>
	count_recs "$(apath "$1")" \
		"select(.msg == \"grpc server stream close\")
		 | select((.method | split(\"/\") | last) == \"$2\")"
}

# rec_time prints the `time` of the n-th matching record of a log.
rec_time() { # <path> <filter> <index from 1> [jq args…]
	local path=$1 filter=$2 idx=$3
	shift 3
	recsr "$path" "select($filter) | .time" "$@" | sed -n "${idx}p"
}

# ts_epoch turns one slog RFC3339Nano stamp into epoch seconds with the
# fraction, so two stamps can be compared numerically (a string compare would
# order ".05" after ".1").
ts_epoch() { date -u -d "$1" +%s.%N; }

# server_now is one reading of the SERVER's clock — the clock every log
# timestamp comes from. A "within N s of the trigger" assertion anchors on this
# and on the records' own stamps, never on the driver's wall clock and never on
# the moment a POLL happened to observe something (each poll costs a full ssh).
# Take it in a statement of its own (`t=$(server_now)`), never inside another
# command's argument: there a failed read is an empty stamp, which ts_epoch
# turns into midnight today, and nothing stops the run.
server_now() { sshw "date -u +%Y-%m-%dT%H:%M:%S.%NZ"; }

ts_delta() { # <later> <earlier> -> seconds, one decimal
	awk -v a="$(ts_epoch "$1")" -v b="$(ts_epoch "$2")" 'BEGIN { printf "%.3f", a - b }'
}

ts_lt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a < b) }'; }
ts_ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a >= b) }'; }

# ---------------------------------------------------------------------------
# Node naming
# ---------------------------------------------------------------------------

dn_dir() { printf 'dn%d' $(($1 - 1)); }
cn_dir() { printf 'cn%d' $(($1 - 1)); }
dn_addr() { printf '%s:%d' "$IP" $((DN_PORT_BASE + $1 - 1)); }
cn_addr() { printf '%s:%d' "$IP" $((CN_PORT_BASE + $1 - 1)); }

# cn_port is a CN's own port, which workerctl uses as the `tr_svc_id` of that
# node's NvmeTrConf — so a CdcEntry's transport list is attributable per CN.
cn_port() { printf '%d' $((CN_PORT_BASE + $1 - 1)); }

# dir_of_addr maps an endpoint the worker allocated back to its fake's dir.
dir_of_addr() { # <addr_port>
	local i
	for i in 1 2 3 4; do
		if [ "$(dn_addr "$i")" = "$1" ]; then
			dn_dir "$i"
			return 0
		fi
	done
	for i in 1 2 3; do
		if [ "$(cn_addr "$i")" = "$1" ]; then
			cn_dir "$i"
			return 0
		fi
	done
	die "no fake agent listens on '$1'"
}

put_dn() { # <id> <free-ext> [extra…]
	local id=$1 free=$2
	shift 2
	ctl put-dn --id "$id" --shard 00 --addr "$(dn_addr "$id")" \
		--location "loc-dn$((id - 1))" --free-ext "$free" "$@"
}

put_cn() { # <id> <free-ext> [extra…]
	local id=$1 free=$2
	shift 2
	ctl put-cn --id "$id" --shard 00 --addr "$(cn_addr "$id")" \
		--location "loc-cn$((id - 1))" --free-ext "$free" "$@"
}

# ---------------------------------------------------------------------------
# The fakes' behaviour and state files (dnv-worker.md, Integration test plan,
# The fake agent)
# ---------------------------------------------------------------------------

# set_behavior writes one fake's behavior.json from a here-doc on stdin. The
# fake re-reads the file whenever its mtime or size changes, so the write must
# be atomic: a half-written file would be parsed, rejected and IGNORED,
# silently keeping the previous behaviour.
set_behavior() { # <agent>   (JSON on stdin)
	local agent=$1
	[ "$QUIET" -eq 1 ] || log "[server] behavior -> $agent"
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"cat > $WORK/$agent/behavior.json.tmp &&
		 mv -f $WORK/$agent/behavior.json.tmp $WORK/$agent/behavior.json"
}

clear_behavior() { set_behavior "$1" <<<'{}'; }

# reset_state empties one fake's state.json the same way. The fakes' revision
# gate (dnv-worker.md, Integration test plan, The fake agent) is per object
# ("dn", "cn", "side …", "cntlr …") and is NOT scoped by cluster, so a fake that
# ended case S holding dn revision 2 would reject case A's revision-1 syncup as
# stale. The per-case reset is what keeps every case independent; it runs while
# the fleet is stopped, so no request is in flight.
reset_state() { # <agent>
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"printf '%s' '{\"objects\":{}}' > $WORK/$1/state.json.tmp &&
		 mv -f $WORK/$1/state.json.tmp $WORK/$1/state.json"
}

# set_state_revision rewrites one stored revision in place, atomically — case A
# step 3's hand edit of a live fake's state.json.
set_state_revision() { # <agent> <object key> <revision>
	local agent=$1 key=$2 rev=$3 cur
	log "[server] state.json of $agent: objects[\"$key\"].revision = $rev"
	cur=$(rlog "$WORK/$agent/state.json")
	printf '%s' "$cur" |
		"$JQ" --arg k "$key" --argjson r "$rev" '.objects[$k].revision = $r' |
		ssh "${SSH_OPTS[@]}" "$TARGET" \
			"cat > $WORK/$agent/state.json.tmp &&
			 mv -f $WORK/$agent/state.json.tmp $WORK/$agent/state.json"
}

state_revision() { # <agent> <object key>
	rlog "$WORK/$1/state.json" | "$JQ" -r --arg k "$2" '.objects[$k].revision // "none"'
}

# ---------------------------------------------------------------------------
# Process control: launch with >> so an external truncation resets the
# write offset, record the pid from $!, and signal by that pid.
# ---------------------------------------------------------------------------

remote_start() { # <dir> <logfile> <command…>
	local dir=$1 logf=$2
	shift 2
	local pid
	pid=$(sshw "nohup $* >> $WORK/$dir/$logf 2>&1 < /dev/null &" \
		"echo \$! > $WORK/$dir/pid; cat $WORK/$dir/pid")
	[ -n "$pid" ] || die "starting $dir produced no pid"
	PID[$dir]=$pid
	RUNNING[$dir]=1
	log "  $dir started, pid $pid"
}

sig_dir() { # <dir> <signal>
	sshw "kill -$2 \$(cat $WORK/$1/pid)"
}

# sig_dir_at signals and returns the SERVER's own clock reading taken
# immediately before the signal, so a latency measured against a log timestamp
# never involves the driver's wall clock (case E step 6).
sig_dir_at() { # <dir> <signal>
	sshw "date -u +%Y-%m-%dT%H:%M:%S.%NZ && kill -$2 \$(cat $WORK/$1/pid)"
}

proc_gone() { ! sshw "kill -0 \$(cat $WORK/$1/pid) 2>/dev/null"; }

wait_gone() { # <dir> <secs>
	wait_until "$2" "$1 to exit" proc_gone "$1"
	unset "RUNNING[$1]"
}

seen_worker() {
	local w
	for w in "${SEEN_WORKERS[@]}"; do
		[ "$w" = "$1" ] && return 0
	done
	SEEN_WORKERS+=("$1")
}

# start_worker records the `worker registered` count the log ALREADY holds
# before the launch. A worker restarted inside a case appends to a log that was
# truncated only by the fleet restart, so it still carries the previous
# launch's three registrations (case S stops w2/w3 in step 0 and starts them
# again in step 9): an unbaselined count is satisfied instantly and proves
# nothing about the process just started.
start_worker() { # <w>
	REG_BASE[$1]=$(count_recs "$(wpath "$1")" \
		'select(.msg == "worker registered")')
	remote_start "$1" worker.log \
		"$WORK/bin/dnv-worker --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--roles dn,cn,sp --vote-interval $VOTE_INTERVAL" \
		"--vote-grace-time $VOTE_GRACE"
	seen_worker "$1"
}

# registered3_since is "three FRESH registrations, one per role": the roles of
# the records past the baseline, never the roles of the whole log.
registered3_since() { # <w> <records held before the launch>
	local n
	n=$(recsr "$(wpath "$1")" 'select(.msg == "worker registered") | .role' |
		tail -n +$(($2 + 1)) | sort -u | wc -l | tr -d ' ')
	[ "$n" -eq 3 ]
}

wait_registered() { # <w>
	wait_until "$WAIT_SHORT" "$1: worker registered for all three roles" \
		registered3_since "$1" "${REG_BASE[$1]-0}"
}

# worker_stopped_since is CM5's `worker stopping`, counted past a baseline
# captured just before the signal — for the same reason wait_registered is
# baseline-relative.
worker_stopped_since() { # <w> <records held before the signal>
	count_gt "$2" count_recs "$(wpath "$1")" 'select(.msg == "worker stopping")'
}

# stop_worker is the CM5 graceful stop: SIGTERM, then wait for BOTH the
# `worker stopping` record and the process's exit before anybody restarts it,
# so the pid/port bookkeeping never drifts.
stop_worker() { # <w>
	[ "${RUNNING[$1]-}" = 1 ] || return 0
	local stops
	stops=$(count_recs "$(wpath "$1")" 'select(.msg == "worker stopping")')
	sig_dir "$1" TERM
	wait_until "$WAIT_MEMBERSHIP" "$1: worker stopping" \
		worker_stopped_since "$1" "$stops"
	wait_gone "$1" "$WAIT_MEMBERSHIP"
}

start_fake() { # <kind dn|cn> <dir> <addr>
	remote_start "$2" agent.log \
		"$WORK/bin/fakeagent $1 --grpc-address $3 --dir $WORK/$2"
}

# ---------------------------------------------------------------------------
# Per-case reset (the fleet restart of dnv-worker.md, Integration test plan,
# Cases)
# ---------------------------------------------------------------------------

truncate_logs() {
	sshw "for f in $WORK/w?/worker.log $WORK/dn?/agent.log $WORK/cn?/agent.log; do" \
		"[ -f \"\$f\" ] && : > \"\$f\"; done; true"
	SEEN_WORKERS=()
}

# wipe_etcd removes every dnv key while the fleet is stopped, so each case
# starts against a pristine store.
#
# Without it the previous cases' clusters stay in etcd and stay driven: their
# rev keys still live on the shards this case's workers own, so their objects
# keep emitting `revision worker started`/`stopped`, `syncup result`, `health
# changed` and flip records into the current case's logs. That broke case A
# step 2 outright — the two `revision worker stopped` records it counted for
# the dn role belonged to it-smoke's DNs, released when w1 handed shard 00 over
# during this case's grace window — and every other count-based assertion
# carried the same trap. The plan gives each case its own cluster precisely so
# nothing leaks between them (dnv-worker.md, Integration test plan, Cases);
# deleting the old keys is what makes that true.
#
# Safe because the workers are down: their registrations go with the wipe and
# are re-put on restart.
wipe_etcd() {
	sshw "$WORK/bin/etcdctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"del --prefix '$DNV_PREFIX '" >/dev/null
}

reset_fakes() {
	local d
	for d in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		clear_behavior "$d"
		reset_state "$d"
	done
}

# fleet_restart is the fleet restart before every case (dnv-worker.md,
# Integration test plan, Cases): stop every worker, wipe the per-case state the
# fakes and the logs carry, start w1..w3 again, wait one grace window and
# re-assert the ownership table.
fleet_restart() { # <case name>
	CASE=$1
	stage restart "fleet restart before case $1"
	local w
	for w in "${WORKER_DIRS[@]}"; do
		stop_worker "$w"
	done
	reset_fakes
	wipe_etcd
	truncate_logs
	for w in w1 w2 w3; do start_worker "$w"; done
	for w in w1 w2 w3; do wait_registered "$w"; done
	log "  waiting $((VOTE_GRACE + 2))s for the first grace window (VW7)"
	sleep $((VOTE_GRACE + 2))
	assert_owners 50 w1 w2 w3
}

# ---------------------------------------------------------------------------
# Cleanup (dnv-worker.md, Integration test plan, Cleanup)
# ---------------------------------------------------------------------------

cleanup_script() {
	cat <<EOF
set -u
pids=""
for f in $WORK/*/pid; do
	[ -f "\$f" ] || continue
	pids="\$pids \$(cat "\$f" 2>/dev/null)"
done
for p in \$pids; do kill -CONT "\$p" 2>/dev/null || true; done
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
pkill -f 'bin/dnv-worker' 2>/dev/null || true
pkill -f 'bin/fakeagent' 2>/dev/null || true
pkill -f 'etcd --name dnv-it' 2>/dev/null || true
sleep 0.5
rm -rf $WORK
echo cleaned
EOF
}

# cleanup signals every recorded pid (SIGTERM, SIGKILL after 5 s), falls back
# to the three pkill -f forms and removes $WORK. Nothing outside $WORK is
# touched. A SIGSTOPped worker is SIGCONTed first, or it would never see the
# SIGTERM.
cleanup() {
	log "[server] cleanup: signal every recorded pid, then rm -rf $WORK"
	cleanup_script | ssh "${SSH_OPTS[@]}" "$TARGET" "bash -s" || true
	PID=()
	RUNNING=()
	SEEN_WORKERS=()
}

# ---------------------------------------------------------------------------
# Diagnostics (dnv-worker.md, Integration test plan, Cleanup)
# ---------------------------------------------------------------------------

diagnostics() {
	local saved=$QUIET
	QUIET=1
	log "failing stage: '$STAGE'   trace_id: $TRACE   cluster: $CLUSTER"

	log ""
	log "--- workerctl list-workers (every role) ---"
	ctl list-workers >&2 || true

	log ""
	log "--- ownership table ---"
	refresh_owners "${SEEN_WORKERS[@]}" || true
	local role
	for role in dn cn sp; do
		log "  role $role:"
		owners_from "$role" | awk '{ printf "    %s %s\n", $1, $2 }' >&2 || true
	done

	log ""
	log "--- worker logs: last 40 records, msg != \"etcd get\" ---"
	local w
	for w in "${SEEN_WORKERS[@]}"; do
		log "  ### $w"
		recs "$(wpath "$w")" 'select(.msg != "etcd get")' | tail -n 40 >&2 || true
	done

	log ""
	log "--- fake logs: last 20 \"grpc server *\" records ---"
	local a
	for a in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		log "  ### $a"
		recs "$(apath "$a")" 'select(.msg | startswith("grpc server"))' |
			tail -n 20 >&2 || true
	done

	log ""
	log "--- workerctl list-keys --prefix dnv ---"
	ctl list-keys --prefix dnv >&2 || true

	log ""
	log "--- ss -ltn (the nine ports) ---"
	sshw_ok "ss -ltn | grep -E ':(${ALL_PORTS[0]}$(printf '|%s' "${ALL_PORTS[@]:1}"))\\b' || true" >&2

	log ""
	log "pull the failing stage's records with:"
	for w in "${SEEN_WORKERS[@]}"; do
		log "  jq 'select(.trace_id==\"$TRACE\")' $(wpath "$w")"
	done
	for a in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		log "  jq 'select(.trace_id==\"$TRACE\")' $(apath "$a")"
	done
	QUIET=$saved
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing: $1 on the driver"
}

# resolve_jq picks the driver's JSON parser exactly as the dn suite does: a
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

# fetch_etcd implements the download-and-verify path. It is
# idempotent: a cached tarball whose sha256 already matches the pin is never
# re-downloaded, so the server needs no internet and a repeat run costs
# nothing, while a fresh checkout still works.
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

# read_constants and read_geometry are how this shell reads a Go value instead
# of copying it. Both run workerctl LOCALLY, on the binary preflight_driver has
# just built: `constants` and `geometry` open no etcd client and read no key,
# so neither wants a server, an etcd or a --cluster. A workerctl too old to
# carry them exits 2 on `unknown subcommand`, which is one thing the dies
# below report; the other is the driver itself, since the binary they run is
# cross-built GOOS=linux GOARCH=amd64 like everything else preflight_driver
# builds and will not exec on a driver that is not the linux/amd64 host the
# suite assumes.
read_constants() {
	local json
	json=$("$WORKERCTL_BIN" constants) ||
		die "\`workerctl constants\` failed: this suite reads" \
			"common.EtcdMaxTxnOps from it and must not guess it"
	ETCD_MAX_TXN_OPS=$(constant_of "$json" EtcdMaxTxnOps)
	log "  --max-txn-ops = common.EtcdMaxTxnOps = $ETCD_MAX_TXN_OPS"
	DEFAULT_HC_INTERVAL=$(constant_of "$json" DefaultHealthCheckInterval)
	DEFAULT_PRIMARY_UNHEALTHY=$(constant_of "$json" DefaultPrimaryUnhealthy)
	DEMOTION_HOLD=$(constant_of "$json" DemotionHoldTimeout)
	log "  case H timers: common.DefaultHealthCheckInterval" \
		"$DEFAULT_HC_INTERVAL s, common.DefaultPrimaryUnhealthy" \
		"$DEFAULT_PRIMARY_UNHEALTHY s, common.DemotionHoldTimeout" \
		"$DEMOTION_HOLD s"
}

# constant_of prints one value of `workerctl constants` and dies unless it
# is a plain non-negative number: an absent key reads as "null", and every
# use of these values is arithmetic.
constant_of() { # <json> <Go identifier>
	local value
	value=$(jq_of "$1" ".$2")
	case "$value" in
	'' | *[!0-9]*)
		die "workerctl constants: $2 is '$value' in $1"
		;;
	esac
	printf '%s' "$value"
}

# geometry_json prints `workerctl geometry`'s reply for one group of this
# suite's shape: raid1, and the EXTENT_SIZE / BLOCK_SIZE / CHUNK_BLOCKS every
# case's cluster is created with.
geometry_json() { # <ext_cnt>
	"$WORKERCTL_BIN" geometry --raid1 --ext-cnt "$1" \
		--extent-size "$EXTENT_SIZE" --block-size "$BLOCK_SIZE" \
		--chunk-blocks "$CHUNK_BLOCKS"
}

read_geometry() {
	local data meta value
	data=$(geometry_json 2) ||
		die "\`workerctl geometry --ext-cnt 2\` failed: this suite reads the" \
			"group geometry from it and must not re-derive it"
	meta=$(geometry_json 1) ||
		die "\`workerctl geometry --ext-cnt 1\` failed: this suite reads the" \
			"group geometry from it and must not re-derive it"
	# meta_blocks is taken from the DATA group because that is the only group
	# case D asserts it on.
	GRP_META_BLOCKS=$(jq_of "$data" .meta_blocks)
	DATA_GRP_DATA_BLOCKS=$(jq_of "$data" .data_blocks)
	META_GRP_DATA_BLOCKS=$(jq_of "$meta" .data_blocks)
	for value in "$GRP_META_BLOCKS" "$DATA_GRP_DATA_BLOCKS" \
		"$META_GRP_DATA_BLOCKS"; do
		case "$value" in
		'' | *[!0-9]*)
			die "workerctl geometry: '$value' is not a block count" \
				"(data $data, meta $meta)"
			;;
		esac
	done
	META_BLOCKS_PER_GRP=$((META_GRP_DATA_BLOCKS * BLOCK_SIZE /
		THIN_META_BLOCK_SIZE))
	log "  group geometry: meta_blocks $GRP_META_BLOCKS," \
		"data_blocks $DATA_GRP_DATA_BLOCKS (data, ext_cnt 2) /" \
		"$META_GRP_DATA_BLOCKS (meta, ext_cnt 1);" \
		"meta blocks per meta group $META_BLOCKS_PER_GRP"
}

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp curl tar sha256sum date awk sed; do need_local "$tool"; done
	resolve_jq
	fetch_etcd
	log "  make build (CGO_ENABLED=0 GOOS=linux GOARCH=amd64)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$WORKER_BIN" ] || die "missing: $WORKER_BIN after make build"
	log "  building integtest/bin/{workerctl,fakeagent}"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$WORKERCTL_BIN" ./integtest/workerctl >&2) ||
		die "building workerctl failed"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$FAKEAGENT_BIN" ./integtest/fakeagent >&2) ||
		die "building fakeagent failed"
	read_constants
	read_geometry
	log "preflight (driver) ok"
}

# preflight_server runs AFTER the start-of-run cleanup: the port check is only
# meaningful once a crashed prior run's processes are gone.
preflight_server() {
	STAGE="preflight (server)"
	log "=== preflight: server"
	sshw "true" || die "passwordless ssh to $TARGET failed"
	local missing
	missing=$(sshw "for b in bash nohup pkill ss tar df sed awk; do" \
		"command -v \$b >/dev/null || echo \$b; done")
	[ -z "$missing" ] || die "missing on $TARGET: $missing"
	local free
	free=$(sshw "df -Pk /var/tmp | awk 'NR==2 { print \$4 }'")
	assert_ge "$free" 1048576 "/var/tmp free space in KiB on $TARGET"
	local listening port
	listening=$(sshw "ss -ltnH | awk '{ print \$4 }' | sed 's/.*://' | sort -u")
	for port in "${ALL_PORTS[@]}"; do
		# grep -c, never grep -q: `set -o pipefail` plus a reader that closes
		# the pipe early turns a SIGPIPE on the writer into a script abort.
		if [ "$(printf '%s\n' "$listening" | grep -cx "$port" || true)" != 0 ]; then
			die "port $port is already listening on $TARGET"
		fi
	done
	log "preflight (server) ok"
}

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

etcd_reachable() {
	sshw "$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT ping" >/dev/null
}

ports_up() { # <port…>
	local listening port
	listening=$(sshw "ss -ltnH | awk '{ print \$4 }' | sed 's/.*://' | sort -u") || return 1
	for port in "$@"; do
		[ "$(printf '%s\n' "$listening" | grep -cx "$port" || true)" != 0 ] || return 1
	done
	return 0
}

setup() {
	CASE=setup

	stage layout "create the work tree and ship the five binaries"
	local dirs=("bin" "etcd" "${WORKER_DIRS[@]}" "${DN_DIRS[@]}" "${CN_DIRS[@]}")
	sshw "mkdir -p $(printf "$WORK/%s " "${dirs[@]}")"
	SETUP_DONE=1
	log "[server] scp etcd etcdctl dnv-worker fakeagent workerctl -> $WORK/bin"
	scp -q "${SSH_OPTS[@]}" \
		"$CACHE_DIR/$ETCD_DIST/etcd" "$CACHE_DIR/$ETCD_DIST/etcdctl" \
		"$WORKER_BIN" "$FAKEAGENT_BIN" "$WORKERCTL_BIN" \
		"$TARGET:$WORK/bin/" || die "scp of the binaries failed"
	sshw "chmod 0755 $WORK/bin/*"

	stage etcd "start etcd and wait for a workerctl ping"
	remote_start etcd etcd.log \
		"$WORK/bin/etcd --name dnv-it --data-dir $WORK/etcd" \
		"--listen-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--advertise-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--listen-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-advertise-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster dnv-it=http://127.0.0.1:$ETCD_PEER_PORT" \
		"--max-txn-ops=$ETCD_MAX_TXN_OPS"
	wait_until "$WAIT_SHORT" "etcd to answer a workerctl ping" etcd_reachable

	stage fakes "start four fake DNs and three fake CNs with an empty behavior"
	local i d
	for i in 1 2 3 4; do
		d=$(dn_dir "$i")
		clear_behavior "$d"
		reset_state "$d"
		start_fake dn "$d" "$(dn_addr "$i")"
	done
	for i in 1 2 3; do
		d=$(cn_dir "$i")
		clear_behavior "$d"
		reset_state "$d"
		start_fake cn "$d" "$(cn_addr "$i")"
	done
	wait_until "$WAIT_SHORT" "the seven fake-agent ports" ports_up \
		29600 29601 29602 29603 29700 29701 29702

	stage workers "start w1, w2, w3 and wait one grace window"
	local w
	for w in w1 w2 w3; do start_worker "$w"; done
	for w in w1 w2 w3; do wait_registered "$w"; done
	log "  waiting $((VOTE_GRACE + 2))s for the first grace window (VW7)"
	sleep $((VOTE_GRACE + 2))
	assert_owners 50 w1 w2 w3
}

# ---------------------------------------------------------------------------
# Shared case helpers
# ---------------------------------------------------------------------------

new_cluster() { # <case> [extra put-cluster flags…]
	CLUSTER="it-$1"
	shift
	local out
	out=$(ctl put-cluster --name "$CLUSTER" \
		--dn-interval "$HEALTH_INTERVAL" --cn-interval "$HEALTH_INTERVAL" \
		--side-interval "$HEALTH_INTERVAL" --cntlr-interval "$HEALTH_INTERVAL" \
		--extent-size "$EXTENT_SIZE" --lwm "$LWM" \
		--block-size "$BLOCK_SIZE" --chunk-blocks "$CHUNK_BLOCKS" "$@")
	CID=$(jq_of "$out" .cluster_id)
	log "  cluster $CLUSTER, cluster_id $CID"
}

sp_rev() { # <sp id>
	ctl get-rev sp --id "$1" --shard 00 | "$JQ" -r .revision
}

dn_rev() { ctl get-rev dn --id "$1" --shard 00 | "$JQ" -r .revision; }
cn_rev() { ctl get-rev cn --id "$1" --shard 00 | "$JQ" -r .revision; }
dn_free() { ctl get-dn --id "$1" | "$JQ" -r .free_ext_cnt; }
cn_free() { ctl get-cn --id "$1" | "$JQ" -r .free_ext_cnt; }
dn_err_epoch() { ctl get-dn --id "$1" | "$JQ" -r .err_epoch; }
cn_err_epoch() { ctl get-cn --id "$1" | "$JQ" -r .err_epoch; }

slice_json() { ctl get-slice --sp "$1" --id "$2"; }

# leg_epoch / side_epoch read one Leg's / Side's err_epoch out of a Slice.
leg_epoch() { # <sp> <slice> <leg id>
	slice_json "$1" "$2" | "$JQ" -r --arg l "$3" '
		[ (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?)
		  | select((.leg_id | tostring) == $l) | .err_epoch ] | first // "none"'
}

side_epoch() { # <sp> <slice> <side id>
	slice_json "$1" "$2" | "$JQ" -r --arg s "$3" '
		[ (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?
		  | select((.side_id | tostring) == $s) | .err_epoch ] | first // "none"'
}

# nonzero_epochs counts the err_epoch fields of a workerctl reply that are NOT
# 0, at any depth — the "err_epoch 0 everywhere" form of case B step 8, which a
# spot check of three named objects cannot give. protojson renders uint64 as a
# JSON string, so the compare goes through tostring like every numeric one here.
nonzero_epochs() { # <json>
	jq_of "$1" '[.. | objects | select(has("err_epoch")) | .err_epoch]
		| map(select((. | tostring) != "0")) | length'
}

cntlr_field() { # <sp> <cntlr id> <field>
	ctl get-cntlr --sp "$1" --id "$2" | "$JQ" -r ".$3"
}

# sp_primary prints "<cntlr_id in decimal> <addr_port>" of the SP's primary.
# get-cntlr keys its map by common.IdKeyFmt ("%016x"), so the id is converted
# here rather than in awk: strtonum() is a gawk extension and the driver may
# well have mawk.
sp_primary() { # <sp>
	local line key addr
	line=$(ctl get-cntlr --sp "$1" | "$JQ" -r '
		to_entries[] | select(.value.primary) | "\(.key) \(.value.addr_port)"' |
		sed -n 1p)
	[ -n "$line" ] || return 1
	key=${line%% *}
	addr=${line##* }
	printf '%d %s\n' "$((16#$key))" "$addr"
}

# is_provisioned reports whether every side of a slice is provisioned.
all_provisioned() { # <sp> <slice>
	local got
	got=$(slice_json "$1" "$2" | "$JQ" -r '
		[ (.meta_grp_list[]?, .data_grp_list[]?) | .leg_list[]? | .side_list[]?
		  | .provisioned ] | all') || return 1
	[ "$got" = true ]
}

td_created() { # <sp> <td name>
	local got
	got=$(ctl get-td --sp "$1" --name "$2" | "$JQ" -r .created) || return 1
	[ "$got" = true ]
}

# td_created_or_die is td_created for a NEGATIVE. td_created answers "not
# created" to a failed read, which keeps a wait_until polling but would let
# assert_none_for pass on a dropped ssh; this one dies on it, as count_cmp
# does for the counts.
td_created_or_die() { # <sp> <td name>
	local got
	got=$(ctl get-td --sp "$1" --name "$2" | "$JQ" -r .created) ||
		die "td $2 of sp $1 could not be read: a read that failed is not \"not created\""
	[ "$got" = true ]
}

# first_side_provisioned prints the `provisioned` of the FIRST SyncupSide
# request one fake ever received for a side. It is the race-free evidence that
# etcd held provisioned = false when the side was created ([D15]): the side
# itself flips to true within a round or two of instantly-zeroing fakes.
first_side_provisioned() { # <agent> <side id>
	recsr "$(apath "$1")" \
		'select(.msg == "grpc server request")
		 | select((.method | split("/") | last) == "SyncupSide")
		 | select(has("data")) | .data
		 | select((.side_pointer.side_id | tostring) == $sid)
		 | (.side_conf.provisioned // false) | tostring' --arg sid "$2" | sed -n 1p
}

reaction_cnt() { # <kind>
	wcount 'select(.msg == "reaction applied") | select(.kind == $k)' --arg k "$1"
}

reaction_skip_cnt() { # <kind> <reason>
	wcount 'select(.msg == "reaction skipped") | select(.kind == $k and .reason == $r)' \
		--arg k "$1" --arg r "$2"
}

reaction_ge() { # <n> <kind>
	count_ge "$1" reaction_cnt "$2"
}

# reaction_epochs prints the server-clock epoch of every `reaction applied`
# record of one kind, over every worker log of the case, ascending. The n-th
# line is the n-th such reaction of the case, so a count baseline indexes
# straight into it. The stamps are CONVERTED before sorting: slog renders
# RFC3339Nano with a variable-length fraction, so ".05" would sort after ".1"
# as text.
reaction_epochs() { # <kind>
	local w t
	for w in "${SEEN_WORKERS[@]}"; do
		recsr "$(wpath "$w")" \
			'select(.msg == "reaction applied") | select(.kind == $k) | .time' \
			--arg k "$1"
	done | while read -r t; do
		if [ -n "$t" ]; then ts_epoch "$t"; fi
	done | sort -n
}

flip_cnt() { # <kind>
	wcount 'select(.msg == "flip applied") | select(.kind == $k)' --arg k "$1"
}

# flip_records prints "<side_id> <revision>" for every `flip applied
# kind=provisioned` record of one SP, over every worker log of the case, in log
# order. RW18 says several sides reported in one round MAY share one STM, and
# the coordinator logs ONE record per side it wrote — all carrying that STM's
# revision (worker/sprole.go) — so these records are what tells "two sides in
# one STM" apart from "one side flipped twice".
flip_records() { # <sp_id>
	local w
	for w in "${SEEN_WORKERS[@]}"; do
		recsr "$(wpath "$w")" \
			'select(.msg == "flip applied") | select(.kind == "provisioned")
			 | select((.sp_id | tostring) == $sp)
			 | "\(.side_id) \(.revision)"' --arg sp "$1" || return 1
	done
}

# flip_record_cnt counts flip_records' lines, and fails when any read did.
flip_record_cnt() { # <sp_id>
	local out
	out=$(flip_records "$1") || return 1
	printf '%s' "$out" | grep -c . || true
}

flip_records_gt() { # <n> <sp_id>
	count_gt "$1" flip_record_cnt "$2"
}

health_changed_cnt() { # <record> <reason>
	wcount 'select(.msg == "health changed") | select(.record == $rec and .reason == $rea)' \
		--arg rec "$1" --arg rea "$2"
}

# thin_pool_line renders one `dmsetup status` thin-pool line for AR6's parser:
#   <start> <len> thin-pool <txn> <used_meta>/<total_meta> <used_data>/<total_data> …
thin_pool_line() { # <used_meta> <total_meta> <used_data> <total_data>
	printf '0 16384 thin-pool 1 %s/%s %s/%s - rw discard_passdown queue_if_no_space - 1024' \
		"$1" "$2" "$3" "$4"
}

# ---------------------------------------------------------------------------
# Case S — smoke (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

w1_owns_everything() {
	refresh_owners w1
	local role table
	for role in dn cn sp; do
		table=$(owners_from "$role")
		[ "$(printf '%s\n' "$table" | awk 'NF' | wc -l | tr -d ' ')" -eq 256 ] || return 1
		[ "$(printf '%s\n' "$table" | awk 'NF { print $2 }' | sort -u)" = "w1" ] || return 1
	done
	return 0
}

case_smoke() {
	CASE=smoke

	# The case is specified for a single worker, so w2/w3 go away first and
	# come back at the end. w1 must own EVERY shard before anything is
	# written, or another (now dead) worker would be the owner of shard 00.
	stage 0 "reduce the fleet to w1 alone"
	stop_worker w2
	stop_worker w3
	wait_until "$WAIT_MEMBERSHIP" "w1 to own all 256 shards of every role" \
		w1_owns_everything

	stage 1 "cluster it-smoke, two DNs and two CNs"
	new_cluster smoke
	put_dn 1 8
	put_dn 2 8
	put_cn 1 64
	put_cn 2 64

	stage 2 "the node syncups and the CheckDn rounds"
	local dn_filter='(.revision | tostring) == "1"
		and ((.side_pointer_list // []) | length) == 0
		and ((.extent_size | tostring) == "67108864")'
	wait_until "$WAIT_SHORT" "dn0: SyncupDn revision 1" \
		req_ge 1 dn0 SyncupDn "$dn_filter"
	wait_until "$WAIT_SHORT" "dn1: SyncupDn revision 1" \
		req_ge 1 dn1 SyncupDn "$dn_filter"
	wait_until "$WAIT_SHORT" "cn0: SyncupCn revision 1" \
		req_ge 1 cn0 SyncupCn '(.revision | tostring) == "1"'
	wait_until "$WAIT_SHORT" "cn1: SyncupCn revision 1" \
		req_ge 1 cn1 SyncupCn '(.revision | tostring) == "1"'

	local before after show_infos tail_true
	before=$(reqs dn0 CheckDn)
	log "  counting CheckDn rounds on dn0 over 5s (now $before)"
	sleep 5
	after=$(reqs dn0 CheckDn)
	assert_ge $((after - before)) 3 "dn0: CheckDn rounds in 5s"
	# Every CheckDn carries show_info false (RW4 step 2); no round asks for
	# the info, a fresh stream's first reply carries it unasked (HL5). The
	# count below skips the first record and asserts every later round.
	# The read is a statement of its own, so a failed read stops the run:
	# piped straight into `grep -c … || true`, it would count 0 and pass
	# this negative on a dropped ssh.
	show_infos=$(recsr "$(apath dn0)" \
		'select(.msg == "grpc server recv")
		 | select((.method | split("/") | last) == "CheckDn")
		 | select(has("data")) | (.data.show_info // false) | tostring')
	tail_true=$(printf '%s\n' "$show_infos" | tail -n +2 | grep -c true || true)
	assert_eq "$tail_true" 0 "dn0: CheckDn rounds after the first with show_info true"

	stage 3 "put-sp sp0 with a meta and a data group, a td and a subsystem"
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 \
		--group 1:1:meta:1:raid1 --group 1:2:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 --leg 2:3:0 --leg 2:4:1 \
		--side 1:1:1:0 --side 2:2:2:0 --side 3:3:1:0 --side 4:4:2:0
	ctl put-td --sp sp0 --name td0 --id "$TD_ID" --size 10737418240
	ctl put-ss --sp sp0 --nqn "$NQN_PREFIX:ss0" --id "$SS_ID" \
		--ns "$NS_ID:1:$TD_ID"

	stage 4 "the SP fans out to both DNs and both CNs"
	wait_until "$WAIT_SYNCUP" "dn0: SyncupDn revision 2 with two side pointers" \
		req_ge 1 dn0 SyncupDn \
		'(.revision | tostring) == "2" and ((.side_pointer_list // []) | length) == 2'
	wait_until "$WAIT_SYNCUP" "dn1: SyncupDn revision 2 with two side pointers" \
		req_ge 1 dn1 SyncupDn \
		'(.revision | tostring) == "2" and ((.side_pointer_list // []) | length) == 2'
	# S1/S3 live on dn 1 (fake dn0), S2/S4 on dn 2 (fake dn1). ext_cnt is 1
	# for the meta group's sides and 2 for the data group's. Earlier replies
	# with code 2 (a side reaching the DN before its SyncupDn listed it) are
	# tolerated by construction: only the final state is asserted.
	local side_filter='(.side_pointer.side_id | tostring) == $sid
		and ((.side_conf.provisioned // false) == false)
		and (.side_conf.primary_cn_id | tostring) == "1"
		and ((.side_conf.standby_id_list // []) | map(tostring)) == ["2"]
		and (.side_conf.ext_cnt | tostring) == $ext
		and ((.side_conf.sp_level // "SP_LEVEL_READWRITE") == "SP_LEVEL_READWRITE")'
	wait_until "$WAIT_SYNCUP" "dn0: SyncupSide S1 (meta, ext 1)" \
		req_ge 1 dn0 SyncupSide "$side_filter" --arg sid 1 --arg ext 1
	wait_until "$WAIT_SYNCUP" "dn1: SyncupSide S2 (meta, ext 1)" \
		req_ge 1 dn1 SyncupSide "$side_filter" --arg sid 2 --arg ext 1
	wait_until "$WAIT_SYNCUP" "dn0: SyncupSide S3 (data, ext 2)" \
		req_ge 1 dn0 SyncupSide "$side_filter" --arg sid 3 --arg ext 2
	wait_until "$WAIT_SYNCUP" "dn1: SyncupSide S4 (data, ext 2)" \
		req_ge 1 dn1 SyncupSide "$side_filter" --arg sid 4 --arg ext 2
	local cntlr_filter='(.id_to_slice | has("0000000000000001"))
		and ((.td_list[0].td_id | tostring) == "16")
		and (.nqn_to_subsystem | has($nqn))'
	wait_until "$WAIT_SYNCUP" "cn0: SyncupCntlr, cntlr.primary true" \
		req_ge 1 cn0 SyncupCntlr "(.cntlr.primary == true) and $cntlr_filter" \
		--arg nqn "$NQN_PREFIX:ss0"
	wait_until "$WAIT_SYNCUP" "cn1: SyncupCntlr, cntlr.primary false" \
		req_ge 1 cn1 SyncupCntlr \
		"((.cntlr.primary // false) == false) and $cntlr_filter" \
		--arg nqn "$NQN_PREFIX:ss0"

	stage 5 "the provisioned flip (RW18)"
	wait_until "$WAIT_SHORT" "every side of slice 1 provisioned" \
		all_provisioned sp0 1
	local rev
	rev=$(sp_rev 1)
	assert_ge "$rev" 2 "SpRev after the provisioned flip"
	assert_ge "$(flip_cnt provisioned)" 1 "flip applied kind=provisioned records"
	wait_until "$WAIT_SHORT" "dn0: SyncupSide with provisioned true" \
		req_ge 1 dn0 SyncupSide '(.side_conf.provisioned // false) == true'
	wait_until "$WAIT_SHORT" "dn1: SyncupSide with provisioned true" \
		req_ge 1 dn1 SyncupSide '(.side_conf.provisioned // false) == true'
	wait_until "$WAIT_SHORT" "cn0: SyncupCntlr at the new revision" \
		req_ge 1 cn0 SyncupCntlr '(.revision | tostring) == $r' --arg r "$rev"
	wait_until "$WAIT_SHORT" "cn1: SyncupCntlr at the new revision" \
		req_ge 1 cn1 SyncupCntlr '(.revision | tostring) == $r' --arg r "$rev"

	stage 6 "the created flip (RW19), negative first"
	# thin_ok with a partial slice map: RW19's "the key set equals the SP's
	# slice ids exactly" must not hold, so nothing flips.
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"thin_ok": true, "thin_missing_slices": [1]}}}
EOF
	assert_none_for 3 "td0 created while a slice is missing from thin_info" \
		td_created_or_die sp0 td0
	rev=$(sp_rev 1)
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"thin_ok": true}}}
EOF
	wait_until "$WAIT_SHORT" "td0 created true" td_created sp0 td0
	assert_ge "$(flip_cnt created)" 1 "flip applied kind=created records"
	assert_eq "$(sp_rev 1)" "$((rev + 1))" "SpRev bumped exactly once by the created flip"
	rev=$(sp_rev 1)
	assert_none_for 5 "a further SpRev bump (no flip loop)" \
		sp_rev_changed 1 "$rev"

	stage 7 "the Check* rounds of every object keep running"
	local a m b1 a1
	for a in dn0 dn1; do
		b1=$(reqs "$a" CheckSide)
		sleep 3
		a1=$(reqs "$a" CheckSide)
		assert_ge $((a1 - b1)) 1 "$a: CheckSide rounds in 3s"
	done
	for a in cn0 cn1; do
		m=CheckCntlr
		b1=$(reqs "$a" "$m")
		sleep 3
		a1=$(reqs "$a" "$m")
		assert_ge $((a1 - b1)) 1 "$a: $m rounds in 3s"
	done

	stage 8 "clean health and the capacity keys"
	assert_eq "$(dn_err_epoch 1)" 0 "dn 1 err_epoch"
	assert_eq "$(cn_err_epoch 1)" 0 "cn 1 err_epoch"
	assert_eq "$(ctl list-keys --prefix dn_capacity | wc -l | tr -d ' ')" 2 \
		"dn_capacity keys"
	assert_eq "$(ctl list-keys --prefix cn_capacity | wc -l | tr -d ' ')" 2 \
		"cn_capacity keys"

	stage 9 "restart w2 and w3"
	start_worker w2
	start_worker w3
	wait_registered w2
	wait_registered w3
	log "  waiting $((VOTE_GRACE + 2))s for the join to commit"
	sleep $((VOTE_GRACE + 2))
	assert_owners 50 w1 w2 w3
}

sp_rev_changed() { # <sp id> <old revision>
	[ "$(sp_rev "$1")" != "$2" ]
}

# ---------------------------------------------------------------------------
# Case A — revision (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

case_revision() {
	CASE=revision

	stage 1 "bump-rev re-syncs the DN in place"
	new_cluster revision
	put_dn 1 8
	wait_until "$WAIT_SHORT" "dn0: SyncupDn revision 1" \
		req_ge 1 dn0 SyncupDn '(.revision | tostring) == "1"'
	ctl bump-rev dn --id 1 --shard 00
	wait_until "$WAIT_SHORT" "dn0: SyncupDn revision 2" \
		req_ge 1 dn0 SyncupDn '(.revision | tostring) == "2"'
	wait_until "$WAIT_SHORT" "dn0: CheckDn carrying revision 2" \
		req_ge 1 dn0 CheckDn '(.revision | tostring) == "2"'

	stage 2 "move-dn: a put, never a delete"
	local closes_before syncups_dn0
	closes_before=$(stream_closes dn0 CheckDn)
	syncups_dn0=$(reqs dn0 SyncupDn)
	ctl move-dn --id 1 --shard 00 --addr "$(dn_addr 2)"
	wait_until "$WAIT_SHORT" "dn1: SyncupDn revision 3 after the move" \
		req_ge 1 dn1 SyncupDn '(.revision | tostring) == "3"'
	wait_until "$WAIT_SHORT" "dn0: the CheckDn stream closed" \
		stream_closed_since dn0 CheckDn "$closes_before"
	assert_eq "$(reqs dn0 SyncupDn)" "$syncups_dn0" \
		"dn0: SyncupDn requests after the move (want none)"
	assert_eq "$(wcount 'select(.msg == "revision worker stopped") | select(.role == "dn")')" 0 \
		"revision worker stopped records for the dn role (a put is not a delete)"

	stage 3 "a stale agent revision is rejected and retried, never health"
	set_state_revision dn1 dn 9
	wait_until "$WAIT_SHORT" "two syncup rejected records at Error" \
		wcount_ge 2 'select(.msg == "syncup rejected")
			| select(.level == "ERROR") | select((.code | tostring) == "1")'
	assert_eq "$(dn_err_epoch 1)" 0 "dn 1 err_epoch while syncups are rejected"
	# The `rejects` baseline is only meaningful once the worker has seen a
	# CLEAN ROUND again. Polling the fake's STORED revision would re-read the
	# value this script itself writes one line below — true on the first poll,
	# with a rejected round possibly still in flight.
	#
	# The evidence is a CheckDn reply, NOT a SyncupDn one. Once the restore
	# puts the fake back at revision 3, the round's reply matches the desired
	# revision and carries code 0, so RW4 step 5's re-sync condition
	# (`code != 0` OR `reply.revision != desired.revision`) is false and the
	# worker correctly issues NO further SyncupDn at all. Waiting for a syncup
	# reply here can therefore never succeed — which is exactly how this
	# assertion first failed.
	local accept_filter clean_before rejects
	accept_filter='((.agent_reply.code // 0) | tostring) == "0"
		and ((.revision // 0) | tostring) == "3"'
	clean_before=$(replies dn1 CheckDn "$accept_filter")
	set_state_revision dn1 dn 3
	wait_until "$WAIT_SHORT" "dn1 to serve a clean CheckDn round at revision 3" \
		reply_gt "$clean_before" dn1 CheckDn "$accept_filter"
	assert_eq "$(state_revision dn1 dn)" 3 \
		"dn1's stored revision after the clean round"
	rejects=$(wcount 'select(.msg == "syncup rejected")')
	assert_none_for 3 "further syncup rejections after the restore" \
		wcount_gt "$rejects" 'select(.msg == "syncup rejected")'

	stage 4 "an unknown-object reply re-issues the syncup for ever"
	put_cn 1 64
	wait_until "$WAIT_SHORT" "cn0: SyncupCn revision 1" \
		req_ge 1 cn0 SyncupCn '(.revision | tostring) == "1"'
	local cn_before
	set_behavior cn0 <<'EOF'
{"objects": {"cn": {"reply_code": 2}}}
EOF
	cn_before=$(reqs cn0 SyncupCn)
	sleep 5
	assert_ge $(($(reqs cn0 SyncupCn) - cn_before)) 3 \
		"cn0: SyncupCn re-issues in 5s while the reply code is 2"
	assert_eq "$(cn_err_epoch 1)" 0 "cn 1 err_epoch while replies carry code 2"
	# "clear ⇒ stops" needs evidence that the worker saw a CLEAN ROUND after
	# the clear. Two things it is NOT:
	#
	#   * the fake's stored revision — already 1 before the code was injected,
	#     and a forced non-zero code SUPPRESSES the apply (fakeagent's
	#     gateSyncupLocked), so that predicate is constant for the whole step;
	#   * a SyncupCn reply with code 0 — once the code is cleared, the CheckCn
	#     reply carries code 0 at the desired revision, so RW4 step 5's re-sync
	#     condition is false and the worker correctly issues NO further
	#     SyncupCn. Waiting for one can never succeed (as case A step 3 also
	#     found).
	#
	# The evidence is a clean CheckCn round at the desired revision.
	local clean_filter clean_before
	clean_filter='((.agent_reply.code // 0) | tostring) == "0"
		and ((.revision // 0) | tostring) == "1"'
	clean_before=$(replies cn0 CheckCn "$clean_filter")
	clear_behavior cn0
	wait_until "$WAIT_SHORT" "cn0 to serve a clean CheckCn round" \
		reply_gt "$clean_before" cn0 CheckCn "$clean_filter"
	cn_before=$(reqs cn0 SyncupCn)
	assert_none_for 3 "further SyncupCn after the code is cleared" \
		reqs_gt "$cn_before" cn0 SyncupCn

	stage 5 "del-rev stops the revision worker"
	local dn1_checks
	closes_before=$(stream_closes dn1 CheckDn)
	ctl del-rev dn --id 1 --shard 00
	wait_until "$WAIT_SHORT" "revision worker stopped for the dn role" \
		wcount_ge 1 'select(.msg == "revision worker stopped") | select(.role == "dn")'
	wait_until "$WAIT_SHORT" "dn1: the CheckDn stream closed" \
		stream_closed_since dn1 CheckDn "$closes_before"
	dn1_checks=$(reqs dn1 CheckDn)
	assert_none_for 3 "further CheckDn rounds at dn1 after the delete" \
		reqs_gt "$dn1_checks" dn1 CheckDn

	stage 6 "bump-rev on an SP re-fans every child"
	# dn 1's rev key is gone and its DnConf still owns dn1's endpoint, so the
	# SP's side goes on a THIRD node: dn 3 at the fake dn2, which keeps the
	# suite's dn_id <-> fake mapping intact (a DN 2 would have to go either on
	# dn2, breaking that mapping, or on dn1, whose endpoint dn 1's DnConf owns).
	put_dn 3 8
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true \
		--slice 1:0 \
		--group 1:1:data:1:none \
		--leg 1:1:0 \
		--side 1:1:3:0
	wait_until "$WAIT_SYNCUP" "dn2: SyncupSide at revision 1" \
		req_ge 1 dn2 SyncupSide '(.revision | tostring) == "1"'
	wait_until "$WAIT_SYNCUP" "cn0: SyncupCntlr at revision 1" \
		req_ge 1 cn0 SyncupCntlr '(.revision | tostring) == "1"'
	local out newrev
	out=$(ctl bump-rev sp --id 1 --shard 00)
	newrev=$(jq_of "$out" .revision)
	wait_until "$WAIT_SHORT" "dn2: SyncupSide at revision $newrev" \
		req_ge 1 dn2 SyncupSide '(.revision | tostring) == $r' --arg r "$newrev"
	wait_until "$WAIT_SHORT" "cn0: SyncupCntlr at revision $newrev" \
		req_ge 1 cn0 SyncupCntlr '(.revision | tostring) == $r' --arg r "$newrev"
	wait_until "$WAIT_SHORT" "dn2: CheckSide following to revision $newrev" \
		req_ge 1 dn2 CheckSide '(.revision | tostring) == $r' --arg r "$newrev"
}

stream_closed_since() { # <agent> <method> <count before>
	count_gt "$3" stream_closes "$1" "$2"
}

wcount_gt() { # <n> <filter> [jq args…]
	local n=$1
	shift
	count_gt "$n" wcount "$@"
}

reqs_gt() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	count_gt "$n" reqs "$@"
}

# ---------------------------------------------------------------------------
# Case B — health (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

dn_err_epoch_set() { [ "$(dn_err_epoch "$1")" != 0 ]; }
dn_err_epoch_clear() { [ "$(dn_err_epoch "$1")" = 0 ]; }
leg_epoch_set() { [ "$(leg_epoch "$1" "$2" "$3")" != 0 ]; }
leg_epoch_clear() { [ "$(leg_epoch "$1" "$2" "$3")" = 0 ]; }
side_epoch_set() { [ "$(side_epoch "$1" "$2" "$3")" != 0 ]; }
side_epoch_clear() { [ "$(side_epoch "$1" "$2" "$3")" = 0 ]; }
cntlr_epoch_set() { [ "$(cntlr_field "$1" "$2" err_epoch)" != 0 ]; }
cntlr_epoch_clear() { [ "$(cntlr_field "$1" "$2" err_epoch)" = 0 ]; }

dn_capacity_key() { ctl list-keys --prefix dn_capacity; }

# dn_capacity_gone is MD4's presence rule as HL1 applies it: the capacity key
# is deleted in the STM that sets err_epoch. A `list-keys` that FAILED (a
# dropped ssh, a busy etcd) prints nothing either, and `-z` cannot tell the
# two apart: the exit status is checked first, so a failed read leaves the
# poll running instead of satisfying it.
dn_capacity_gone() {
	local out
	out=$(dn_capacity_key) || return 1
	[ -z "$out" ]
}
dn_capacity_is() { [ "$(dn_capacity_key)" = "$1" ]; }

case_health() {
	CASE=health

	stage 1 "cluster it-health, one DN and one CN"
	new_cluster health
	put_dn 1 8
	put_cn 1 64
	wait_until "$WAIT_SHORT" "dn0: SyncupDn revision 1" \
		req_ge 1 dn0 SyncupDn '(.revision | tostring) == "1"'
	wait_until "$WAIT_SHORT" "cn0: SyncupCn revision 1" \
		req_ge 1 cn0 SyncupCn '(.revision | tostring) == "1"'
	local capkey
	capkey=$(dn_capacity_key)
	[ -n "$capkey" ] || die "the dn_capacity key is missing"
	assert_eq "$(ctl list-keys --prefix cn_capacity | wc -l | tr -d ' ')" 1 \
		"cn_capacity keys"

	stage 2 "an ERROR row sets err_epoch and drops the capacity key"
	set_behavior dn0 <<'EOF'
{"objects": {"dn": {"rows": {"disk_info": {"status": "ERROR", "details": "io"}}}}}
EOF
	wait_until "$WAIT_SHORT" "dn 1 err_epoch set" dn_err_epoch_set 1
	wait_until "$WAIT_SHORT" "the dn_capacity key to disappear" dn_capacity_gone
	assert_ge "$(health_changed_cnt dn error_row)" 1 \
		"health changed record=dn reason=error_row"

	stage 3 "clearing the row restores err_epoch and the capacity key"
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "dn 1 err_epoch cleared" dn_err_epoch_clear 1
	wait_until "$WAIT_SHORT" "the dn_capacity key to come back unchanged" \
		dn_capacity_is "$capkey"
	assert_ge "$(health_changed_cnt dn recovered)" 1 \
		"health changed record=dn reason=recovered"

	stage 4 "a hung agent is unreachable, and recovers"
	local closes
	closes=$(stream_closes dn0 CheckDn)
	set_behavior dn0 <<'EOF'
{"objects": {"dn": {"hang": true}}}
EOF
	wait_until "$WAIT_SHORT" "dn 1 err_epoch set by the missed reply" \
		dn_err_epoch_set 1
	assert_ge "$(health_changed_cnt dn unreachable)" 1 \
		"health changed record=dn reason=unreachable"
	sleep 3
	assert_ge $(($(stream_closes dn0 CheckDn) - closes)) 2 \
		"dn0: CheckDn stream closes while hanging"
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "dn 1 err_epoch cleared after the hang" \
		dn_err_epoch_clear 1

	stage 5 "a killed and restarted fake replies with its stored revision"
	sig_dir dn0 KILL
	wait_gone dn0 "$WAIT_SHORT"
	wait_until "$WAIT_SHORT" "dn 1 err_epoch set after the kill" dn_err_epoch_set 1
	local syncups
	syncups=$(reqs dn0 SyncupDn)
	start_fake dn dn0 "$(dn_addr 1)"
	wait_until "$WAIT_SHORT" "dn 1 err_epoch cleared after the restart" \
		dn_err_epoch_clear 1
	assert_none_for 3 "a re-sync of the restarted fake (its revision is intact)" \
		reqs_gt "$syncups" dn0 SyncupDn

	stage 6 "PROVISIONING neither sets nor clears ([D15])"
	set_behavior dn0 <<'EOF'
{"objects": {"dn": {"rows": {"meta_info": {"status": "PROVISIONING"}}}}}
EOF
	assert_none_for 3 "err_epoch set by a PROVISIONING row" dn_err_epoch_set 1
	assert_eq "$(dn_capacity_key)" "$capkey" "the dn_capacity key under PROVISIONING"

	stage 7 "a plain ERROR meta row sets err_epoch"
	set_behavior dn0 <<'EOF'
{"objects": {"dn": {"rows": {"meta_info": {"status": "ERROR",
  "details": "disk lacks Write Zeroes"}}}}}
EOF
	wait_until "$WAIT_SHORT" "dn 1 err_epoch set by the meta row" dn_err_epoch_set 1
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "dn 1 err_epoch cleared again" dn_err_epoch_clear 1

	stage 8 "the SP object table: cntlr, leg and side err_epochs"
	# The thresholds are deliberately NOT the suite's reaction values here: this
	# case injects the very ERROR rows case D uses as reaction triggers, and
	# an AR5 failover or an AR8 spare would bump SpRev — which step 8 asserts
	# never happens ("health never bumps"). Case D is where the small
	# thresholds belong.
	put_dn 2 8
	put_cn 2 64
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1 --level 0 \
		--thresholds 3600,3600,3600,3600 --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 \
		--group 1:1:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 \
		--side 1:1:1:0 --side 2:2:2:0
	wait_until "$WAIT_SYNCUP" "every side of slice 1 provisioned" \
		all_provisioned sp0 1
	# "a clean state" is EVERY err_epoch of the snapshot, at any depth (the
	# slice's groups, legs, spare legs and sides) plus every cntlr's — not the
	# three objects the injections below happen to touch.
	assert_eq "$(nonzero_epochs "$(slice_json sp0 1)")" 0 \
		"slice 1: non-zero err_epochs before the injections"
	assert_eq "$(nonzero_epochs "$(ctl get-cntlr --sp sp0)")" 0 \
		"cntlrs: non-zero err_epochs before the injections"
	local sprev
	sprev=$(sp_rev 1)

	log "  8a: the PRIMARY's leg row sets Leg.err_epoch"
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"rows": {"leg_id_to_leg.1":
  {"status": "ERROR", "details": "probe timeout"}}}}}
EOF
	wait_until "$WAIT_SHORT" "L1 err_epoch set" leg_epoch_set sp0 1 1
	assert_ge "$(health_changed_cnt leg error_row)" 1 \
		"health changed record=leg reason=error_row"
	clear_behavior cn0
	wait_until "$WAIT_SHORT" "L1 err_epoch cleared" leg_epoch_clear sp0 1 1

	log "  8b: a STANDBY's leg row is logged, never recorded"
	set_behavior cn1 <<'EOF'
{"objects": {"cntlr 1:2": {"rows": {"leg_id_to_leg.1":
  {"status": "ERROR", "details": "standby probe"}}}}}
EOF
	assert_none_for 3 "L1 err_epoch set by a standby's leg row" \
		leg_epoch_set sp0 1 1
	clear_behavior cn1

	log "  8c: the side's own rows set Side.err_epoch"
	set_behavior dn0 <<'EOF'
{"objects": {"side 1:1:1": {"rows": {"side_dev_info":
  {"status": "ERROR", "details": "dm suspended"}}}}}
EOF
	wait_until "$WAIT_SHORT" "S1 err_epoch set" side_epoch_set sp0 1 1
	assert_ge "$(health_changed_cnt side error_row)" 1 \
		"health changed record=side reason=error_row"
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "S1 err_epoch cleared" side_epoch_clear sp0 1 1
	set_behavior dn0 <<'EOF'
{"objects": {"side 1:1:1": {"hang": true}}}
EOF
	wait_until "$WAIT_SHORT" "S1 err_epoch set by the hang" side_epoch_set sp0 1 1
	assert_ge "$(health_changed_cnt side unreachable)" 1 \
		"health changed record=side reason=unreachable"
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "S1 err_epoch cleared after the hang" \
		side_epoch_clear sp0 1 1

	log "  8d: a non-leg CntlrInfo row sets Cntlr.err_epoch"
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"rows": {"slice_id_to_dm_pool.1":
  {"status": "ERROR", "details": "pool failed"}}}}}
EOF
	wait_until "$WAIT_SHORT" "C1 err_epoch set" cntlr_epoch_set sp0 1
	assert_ge "$(health_changed_cnt cntlr error_row)" 1 \
		"health changed record=cntlr reason=error_row"
	clear_behavior cn0
	wait_until "$WAIT_SHORT" "C1 err_epoch cleared" cntlr_epoch_clear sp0 1

	log "  8e: none of it bumped SpRev (HL3)"
	assert_eq "$(sp_rev 1)" "$sprev" "SpRev after every health transition"
}

# ---------------------------------------------------------------------------
# Case C — bitmap (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

# push_time prints the timestamp of the n-th PushMigrBitmap / PushCloneBitmap
# request or reply of one fake.
push_req_time() { # <agent> <method> <bm_idx>
	recsr "$(apath "$1")" \
		'select(.msg == "grpc server request")
		 | select((.method | split("/") | last) == $m)
		 | select(has("data")) | select((.data.bm_idx // 0 | tostring) == $i)
		 | .time' --arg m "$2" --arg i "$3" | sed -n 1p
}

push_reply_time() { # <agent> <method> <index from 1>
	recsr "$(apath "$1")" \
		'select(.msg == "grpc server reply")
		 | select((.method | split("/") | last) == $m) | .time' --arg m "$2" |
		sed -n "${3}p"
}

# clone_pair prints a jq filter over a PushCloneBitmap request's `.data` that
# matches exactly one (src_slice_idx, bm_idx) pair — the address of one clone
# bitmap chunk. Both indexes are dropped from the logged message when they are
# 0 (common.PbToLogValue renders only populated proto3 fields), so each is
# defaulted before the comparison, exactly as the migration filters default
# `.bm_idx`.
clone_pair() { # <src_slice_idx> <bm_idx>
	printf '(.src_slice_idx // 0) == %s and (.bm_idx // 0) == %s' "$1" "$2"
}

# push_clone_req_time prints the timestamp of the FIRST PushCloneBitmap
# request one fake received at one pair.
push_clone_req_time() { # <agent> <src_slice_idx> <bm_idx>
	recsr "$(apath "$1")" \
		"select(.msg == \"grpc server request\")
		 | select((.method | split(\"/\") | last) == \"PushCloneBitmap\")
		 | select(has(\"data\")) | select(.data | $(clone_pair "$2" "$3"))
		 | .time" | sed -n 1p
}

case_bitmap() {
	CASE=bitmap

	stage 1 "an SP with a migration and a clone, both with two chunks"
	new_cluster bitmap
	put_dn 1 8
	put_dn 2 8
	put_cn 1 64
	put_cn 2 64
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 \
		--group 1:1:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 \
		--side 1:1:1:0 --side 2:2:2:0
	ctl put-td --sp sp0 --name td0 --id "$TD_ID" --size 10737418240
	wait_until "$WAIT_SYNCUP" "every side of slice 1 provisioned" \
		all_provisioned sp0 1
	# The migration's destination side S3 lands on dn 2, so dn1 is the only
	# legal target of a migration chunk (BM4) and dn0, the source, gets none.
	ctl put-migr --sp sp0 --migr "m0:3:1:3" --dst-dn 2 --dst-slot 1
	ctl put-clone --sp sp0 --name c0 --id "$CLONE_ID" --dst-td td0 --src-slice-cnt 2
	# Hold the clone pushes back while the three chunks are seeded: the fake's
	# chunk_id_list lever (dnv-worker.md, Integration test plan, The fake agent)
	# forces cn0 to report exactly the pairs about to be written as already
	# applied, so BM2's diff stays empty. Seeded one at a time without it, each
	# chunk would be pushed as it appeared and the observed order would be the
	# WRITE order — which says nothing about BM3's. Stage 3 clears the lever and
	# the whole clone becomes missing at once, which is what makes the
	# lexicographic order observable.
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"chunk_id_list": ["0:0", "0:1", "1:0"]}}}
EOF
	# Three chunks at three PAIRS: (0,0) and (0,1) are two chunks of source
	# slice 0's bitmap, and (1,0) shares bm_idx 0 with (0,0) on another
	# source slice. Three pairs, three keys — a key built from either index
	# alone would merge two of them.
	ctl put-bitmap --sp sp0 --kind clone --name c0 --src-slice-idx 0 --bm-idx 0 --hex 0102
	ctl put-bitmap --sp sp0 --kind clone --name c0 --src-slice-idx 0 --bm-idx 1 --hex 0304
	local clone_put
	clone_put=$(ctl put-bitmap --sp sp0 --kind clone --name c0 \
		--src-slice-idx 1 --bm-idx 0 --hex 0506)
	assert_eq "$(key_cnt clone_bitmap)" 3 \
		"c0 chunk keys after (0,0), (0,1), (1,0)"
	# The output key follows the record: a Clone carries no chunk count, so a
	# clone put emits no bm_cnt at all. The migration half is asserted
	# below, so the two directions of the same branch are both pinned.
	assert_eq "$("$JQ" -r 'has("bm_cnt")' <<<"$clone_put")" false \
		"a clone put-bitmap must emit no bm_cnt"
	# The lever must really hold the clone pushes back: seeded in this order
	# and pushed as they appeared, the three chunks would reach cn0 in the
	# lexicographic order too, and stage 3's order check would pass on the
	# WRITE order.
	assert_none_for 3 "clone pushes while cn0's applied set is forced" \
		reqs_gt 0 cn0 PushCloneBitmap
	local migr0_out
	migr0_out=$(ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 0 --hex aa00ff)
	# A MIGRATION's parent does carry the counter, and put-bitmap raises it —
	# the other direction of the clone assert above.
	assert_eq "$("$JQ" -r '.bm_cnt' <<<"$migr0_out")" 1 \
		"a migr put-bitmap emits the parent's raised bm_cnt"
	ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 1 --hex bb11ee
	# --src-slice-idx addresses a CLONE chunk; a migration's chunks name no
	# source slice, so a non-zero one with --kind migr is a driver usage
	# error and writes nothing — it is refused on the flags alone,
	# before etcd is opened, so it cannot disturb the pushes step 2 counts.
	if ctl put-bitmap --sp sp0 --kind migr --name m0 --src-slice-idx 1 \
		--bm-idx 9 --hex ff >/dev/null 2>&1; then
		die "put-bitmap --kind migr --src-slice-idx 1: want a usage error"
	fi

	stage 2 "migration chunks: ordered, one in flight, to the destination only"
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 0" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "0"'
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 1" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "1"'
	local t_req1 t_reply0
	t_req1=$(push_req_time dn1 PushMigrBitmap 1)
	t_reply0=$(push_reply_time dn1 PushMigrBitmap 1)
	ts_ge "$(ts_epoch "$t_req1")" "$(ts_epoch "$t_reply0")" ||
		die "chunk 1's request ($t_req1) is not after chunk 0's reply ($t_reply0): BM3 wants one push in flight"
	# BM3: a push carries no revision at all — the field is gone from both
	# push requests (pb/schema.proto) — so no request dn1 has logged may hold
	# one. The two waits above are what make this count of 0 evidence: both
	# chunks' requests are in the log.
	assert_eq "$(reqs dn1 PushMigrBitmap 'has("revision")')" 0 \
		"dn1: migration push requests carrying a revision field (BM3: none)"
	# `bm_info` rides ONLY on a SyncupSideReply (the proto gives CheckSideReply
	# no such field), and a push arms no re-sync of its own (BM6) — so after a
	# clean push sequence nothing bumps SpRev and no further SyncupSide is
	# issued. Force one re-fan, which is what makes BM2's diff observable:
	# the reply must now report the COMPLETE set [0,1], and the worker must
	# push nothing more because the diff is empty. That is a stronger check
	# than merely seeing the set appear on its own.
	local pushes_before_refan
	pushes_before_refan=$(reqs dn1 PushMigrBitmap)
	ctl bump-rev sp --id 1 --shard 00 >/dev/null
	wait_until "$WAIT_SHORT" "dn1: a SyncupSide reply reporting bm_idx_list [0,1]" \
		reply_ge 1 dn1 SyncupSide '(.bm_info.bm_idx_list // []) == [0, 1]'
	assert_eq "$(reqs dn1 PushMigrBitmap)" "$pushes_before_refan" \
		"migration pushes after a re-fan whose diff is empty"
	local pushes
	pushes=$(reqs dn1 PushMigrBitmap)
	assert_none_for 3 "further migration pushes once the set matches" \
		reqs_gt "$pushes" dn1 PushMigrBitmap
	assert_eq "$(reqs dn0 PushMigrBitmap)" 0 "dn0 (the source): migration pushes"

	stage 3 "clone chunks: one push per PAIR, lexicographic, to the primary only"
	# Release the lever of stage 1. Clearing behavior.json bumps no revision
	# and bm_info_list rides only on a SyncupCntlrReply (CheckCntlrReply has
	# no such field), so the re-fan is what makes the fake report the DERIVED
	# — still empty — applied set and the whole clone missing in one diff.
	clear_behavior cn0
	ctl bump-rev sp --id 1 --shard 00 >/dev/null
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap (0,0)" \
		req_ge 1 cn0 PushCloneBitmap "$(clone_pair 0 0)"
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap (0,1)" \
		req_ge 1 cn0 PushCloneBitmap "$(clone_pair 0 1)"
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap (1,0)" \
		req_ge 1 cn0 PushCloneBitmap "$(clone_pair 1 0)"
	assert_eq "$(reqs cn1 PushCloneBitmap)" 0 "cn1 (the standby): clone pushes"
	# BM3's order for a clone is ascending (src_slice_idx, bm_idx)
	# LEXICOGRAPHIC. (1,0) after (0,1) is the discriminating pair: an order
	# taken on bm_idx alone would send (1,0) second, before (0,1).
	local t00 t01 t10
	t00=$(push_clone_req_time cn0 0 0)
	t01=$(push_clone_req_time cn0 0 1)
	t10=$(push_clone_req_time cn0 1 0)
	[ -n "$t00" ] && [ -n "$t01" ] && [ -n "$t10" ] ||
		die "cn0: one of the three clone pushes has no request record"
	ts_ge "$(ts_epoch "$t01")" "$(ts_epoch "$t00")" ||
		die "(0,1) ($t01) was pushed before (0,0) ($t00): BM3 wants ascending pairs"
	ts_ge "$(ts_epoch "$t10")" "$(ts_epoch "$t01")" ||
		die "(1,0) ($t10) was pushed before (0,1) ($t01): BM3 orders the PAIR, not bm_idx"
	# The applied set comes back as chunk_id_list — the pair-addressed field —
	# and NOT as bm_idx_list, which carries migration chunks only. A second
	# re-fan is what makes it observable, as in step 2, and its diff must be
	# empty.
	local clone_pushes
	clone_pushes=$(reqs cn0 PushCloneBitmap)
	ctl bump-rev sp --id 1 --shard 00 >/dev/null
	# Every `// []` below is load-bearing: an unset repeated field is absent
	# from the logged message (common.PbToLogValue), and iterating a null
	# would abort the whole jq read rather than fail one record. The replies
	# of stage 1, before the clone existed, carry no bm_info_list at all.
	wait_until "$WAIT_SHORT" "cn0: a SyncupCntlr reply listing the three pairs" \
		reply_ge 1 cn0 SyncupCntlr \
		'[(.bm_info_list // [])[] | select(.res_id == $c)
		  | (.chunk_id_list // [])[] | [(.src_slice_idx // 0), (.bm_idx // 0)]]
		 == [[0, 0], [0, 1], [1, 0]]' --argjson c "$((CLONE_ID))"
	assert_ge "$(replies cn0 SyncupCntlr \
		'[(.bm_info_list // [])[] | select(.res_id == $c)
		  | ((.bm_idx_list // []) | length)] == [0]' \
		--argjson c "$((CLONE_ID))")" 1 \
		"cn0: a SyncupCntlr reply leaving the clone's bm_idx_list unset"
	assert_eq "$(reqs cn0 PushCloneBitmap)" "$clone_pushes" \
		"clone pushes after a re-fan whose diff is empty"
	assert_none_for 3 "further clone pushes once the set matches" \
		reqs_gt "$clone_pushes" cn0 PushCloneBitmap

	stage 4 "a new migration chunk is pushed exactly once, carrying no revision"
	pushes=$(reqs dn1 PushMigrBitmap)
	ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 2 --hex cc22dd
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 2" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "2"'
	assert_eq "$(reqs dn1 PushMigrBitmap 'has("revision")')" 0 \
		"dn1: migration push requests carrying a revision field, chunk 2's included (BM3: none)"
	sleep 3
	assert_eq "$(reqs dn1 PushMigrBitmap)" $((pushes + 1)) \
		"dn1: exactly one new migration push"

	stage 5 "a grown clone chunk is re-pushed at the same pair ([D8], BM5)"
	# The growth stays at the pair (0,0): the memo is per (clone_id,
	# src_slice_idx, bm_idx), so neither (0,1) — the same slice, another
	# chunk — nor (1,0) — the same bm_idx, another slice — may move.
	local clone00 clone01 clone10
	clone00=$(reqs cn0 PushCloneBitmap "$(clone_pair 0 0)")
	clone01=$(reqs cn0 PushCloneBitmap "$(clone_pair 0 1)")
	clone10=$(reqs cn0 PushCloneBitmap "$(clone_pair 1 0)")
	ctl put-bitmap --sp sp0 --kind clone --name c0 --src-slice-idx 0 --bm-idx 0 \
		--hex 0102030405060708
	wait_until "$WAIT_SHORT" "cn0: a second PushCloneBitmap (0,0), 8 bytes" \
		req_ge $((clone00 + 1)) cn0 PushCloneBitmap "$(clone_pair 0 0)"
	assert_ge "$(reqs cn0 PushCloneBitmap \
		"$(clone_pair 0 0) and .bitmap == \"<8 bytes>\"")" 1 \
		"cn0: the re-pushed chunk (0,0) carries the new length"
	sleep 2
	assert_eq "$(reqs cn0 PushCloneBitmap "$(clone_pair 0 1)")" "$clone01" \
		"cn0: chunk (0,1) was not re-pushed"
	assert_eq "$(reqs cn0 PushCloneBitmap "$(clone_pair 1 0)")" "$clone10" \
		"cn0: chunk (1,0) — the same bm_idx on another slice — was not re-pushed"

	stage 6 "a rejected SyncupSide is re-driven by the CheckSide reply's code (RW4 step 5)"
	set_behavior dn1 <<'EOF'
{"objects": {"side 1:1:3": {"reply_code": 1}}}
EOF
	ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 3 --hex dd33cc
	local rev
	rev=$(sp_rev 1)
	wait_until "$WAIT_SHORT" "dn1: two syncup results at the same revision" \
		wcount_ge 2 'select(.msg == "syncup result")
			| select((.revision | tostring) == $r)
			| select((.side_pointer.side_id // 0 | tostring) == "3")' --arg r "$rev"
	clear_behavior dn1
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 3" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "3"'
	# As in step 2: bm_info rides only on a SyncupSideReply, and once chunk
	# 3's push lands nothing bumps SpRev, so the worker correctly issues
	# no further SyncupSide. Force one re-fan to make the completed set
	# observable, and require that the now-empty diff pushes nothing more.
	local pushes_after_retry
	pushes_after_retry=$(reqs dn1 PushMigrBitmap)
	ctl bump-rev sp --id 1 --shard 00 >/dev/null
	wait_until "$WAIT_SHORT" "dn1: a SyncupSide reply reporting [0,1,2,3]" \
		reply_ge 1 dn1 SyncupSide '(.bm_info.bm_idx_list // []) == [0, 1, 2, 3]'
	assert_eq "$(reqs dn1 PushMigrBitmap)" "$pushes_after_retry" \
		"migration pushes after the recovery re-fan (the diff is empty)"

	stage 7 "a primary change moves the clone pushes with it (BM4)"
	# cn0's baseline is taken BEFORE the two set-cntlr calls: taken after cn1
	# has already received both re-pushes, it would forgive every push the old
	# primary got in between, which is exactly what "cn0 none after the change"
	# forbids. At most one push can have been in flight when the role moved.
	local cn1_before cn0_before
	cn1_before=$(reqs cn1 PushCloneBitmap)
	cn0_before=$(reqs cn0 PushCloneBitmap)
	ctl set-cntlr --sp sp0 --id 1 --primary=false
	ctl set-cntlr --sp sp0 --id 2 --primary=true
	wait_until "$WAIT_SHORT" "cn1: PushCloneBitmap (0,0)" \
		req_ge $((cn1_before + 1)) cn1 PushCloneBitmap "$(clone_pair 0 0)"
	wait_until "$WAIT_SHORT" "cn1: PushCloneBitmap (0,1)" \
		req_ge 1 cn1 PushCloneBitmap "$(clone_pair 0 1)"
	wait_until "$WAIT_SHORT" "cn1: PushCloneBitmap (1,0)" \
		req_ge 1 cn1 PushCloneBitmap "$(clone_pair 1 0)"
	local cn0_after
	cn0_after=$(reqs cn0 PushCloneBitmap)
	assert_between "$cn0_after" "$cn0_before" $((cn0_before + 1)) \
		"cn0: clone pushes while the primary moved (at most one in flight)"
	assert_none_for 3 "clone pushes to the old primary after the change" \
		reqs_gt "$cn0_after" cn0 PushCloneBitmap
}

# ---------------------------------------------------------------------------
# Case D — reaction (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

cntlr_is_primary() { [ "$(cntlr_field "$1" "$2" primary)" = true ]; }
cntlr_not_primary() { [ "$(cntlr_field "$1" "$2" primary)" = false ]; }
# cntlr_settled is the stored half of HL2's settle: `settling` false. get-cntlr
# prints protojson with unpopulated fields, so a settled cntlr reads a literal
# false rather than a missing key.
cntlr_settled() { [ "$(cntlr_field "$1" "$2" settling)" = false ]; }

# settled_cnt counts the `cntlr settled` records (dnv-worker.md, Log records)
# of one cntlr — the worker's own statement that it observed the cntlr clean
# as primary, its stack built, at the revision it drives (HL2). The sp_id and
# the pointer's cntlr_id are JSON numbers in the worker log, hence tostring.
settled_cnt() { # <sp id> <cntlr id>
	wcount 'select(.msg == "cntlr settled")
		| select((.sp_id | tostring) == $sp
			and (.cntlr_pointer.cntlr_id | tostring) == $c)' \
		--arg sp "$1" --arg c "$2"
}

settled_ge() { # <sp id> <cntlr id> <n>
	count_ge "$3" settled_cnt "$1" "$2"
}

# cntlr_gone is case D step 3's "C1 gone from cntlr_id_list". It reads the
# LIST rather than probing the Cntlr key: `! ctl get-cntlr …` cannot tell
# "the key is not there" from "the read failed", so a dropped ssh would report
# the cntlr gone and the poll would end on it.
cntlr_gone() { # <sp> <cntlr id>
	local ids
	ids=$(ctl get-sp --sp "$1" | "$JQ" -r '.sp_conf.cntlr_id_list[]?') || return 1
	[ "$(printf '%s\n' "$ids" | grep -cx "$2" || true)" = 0 ]
}

# grp_count / grp_field address the groups of one slice by kind and position.
grp_count() { # <sp> <slice> <meta|data>
	slice_json "$1" "$2" | "$JQ" -r "(.${3}_grp_list // []) | length"
}

grp_count_is() { [ "$(grp_count "$1" "$2" "$3")" = "$4" ]; }

last_grp() { # <sp> <slice> <meta|data>  -> the newest group as JSON
	slice_json "$1" "$2" | "$JQ" -c ".${3}_grp_list | last"
}

spare_count() { # <sp> <slice> <meta|data> <grp id>
	slice_json "$1" "$2" | "$JQ" -r --arg g "$4" "
		[ .${3}_grp_list[] | select((.grp_id | tostring) == \$g)
		  | (.spare_leg_list // []) | length ] | first // 0"
}

spare_count_is() { [ "$(spare_count "$1" "$2" "$3" "$4")" = "$5" ]; }

# active_legs prints "<leg_id> <side_id> <addr_port>" for a group's leg_list.
active_legs() { # <sp> <slice> <meta|data> <grp id>
	slice_json "$1" "$2" | "$JQ" -r --arg g "$4" "
		.${3}_grp_list[] | select((.grp_id | tostring) == \$g) | .leg_list[]
		| \"\\(.leg_id) \\(.side_list[0].side_id) \\(.side_list[0].addr_port)\""
}

spare_legs() { # <sp> <slice> <meta|data> <grp id>
	slice_json "$1" "$2" | "$JQ" -r --arg g "$4" "
		.${3}_grp_list[] | select((.grp_id | tostring) == \$g)
		| (.spare_leg_list // [])[]
		| \"\\(.leg_id) \\(.side_list[0].side_id) \\(.side_list[0].addr_port) \\(.err_epoch)\""
}

# group_legs lists EVERY leg of a group — leg_list and spare_leg_list together —
# as "leg_id side_id addr_port". A leg repair moves legs between the two lists
# (AR8 step 1 switches the spare in and parks the old leg), so an assertion that
# wants "the leg this pass created" must find it by id across both, never by
# position in one of them.
group_legs() { # <sp> <slice> <meta|data> <grp id>
	slice_json "$1" "$2" | "$JQ" -r --arg g "$4" "
		.${3}_grp_list[] | select((.grp_id | tostring) == \$g)
		| ((.leg_list // []) + (.spare_leg_list // []))[]
		| \"\\(.leg_id) \\(.side_list[0].side_id) \\(.side_list[0].addr_port)\""
}

# new_group_leg prints the one leg of a group whose id is absent from the
# space-separated <before> set — i.e. the leg the pass under test created.
new_group_leg() { # <sp> <slice> <meta|data> <grp id> <before ids>
	local before=" $5 " line id
	group_legs "$1" "$2" "$3" "$4" | while read -r line; do
		id=$(printf '%s' "$line" | awk '{ print $1 }')
		case "$before" in
		*" $id "*) ;;
		*) printf '%s\n' "$line" ;;
		esac
	done
}

group_has_new_leg() { # <sp> <slice> <meta|data> <grp id> <before ids>
	[ -n "$(new_group_leg "$1" "$2" "$3" "$4" "$5")" ]
}

# pool_behavior writes the primary cntlr's dm-pool status row on the fake that
# hosts it. Every AR6 step drives the reaction through this one line.
pool_behavior() { # <cn dir> <sp id> <cntlr id> <line>
	set_behavior "$1" <<EOF
{"objects": {"cntlr $2:$3": {"thin_ok": true, "rows": {"slice_id_to_dm_pool.1":
  {"status": "OK", "details": "$4"}}}}}
EOF
}

case_reaction() {
	CASE=reaction

	stage 1 "the reaction fixture: four DNs, three CNs, sp0 with td0 and ss0"
	new_cluster reaction --dn-batch 16 --cn-batch 16
	local i
	for i in 1 2 3 4; do put_dn "$i" 8; done
	for i in 1 2 3; do put_cn "$i" 64; done
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1,2 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 \
		--group 1:1:meta:1:raid1 --group 1:2:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 --leg 2:3:0 --leg 2:4:1 \
		--side 1:1:1:0 --side 2:2:2:0 --side 3:3:1:0 --side 4:4:2:0
	ctl put-td --sp sp0 --name td0 --id "$TD_ID" --size 10737418240
	ctl put-ss --sp sp0 --nqn "$NQN_PREFIX:ss0" --id "$SS_ID" --ns "$NS_ID:1:$TD_ID"
	local cdc_key
	cdc_key=$(ctl list-keys --prefix cdc | sed -n 1p)
	[ -n "$cdc_key" ] || die "no CdcEntry key after put-ss"
	wait_until "$WAIT_SYNCUP" "every side of slice 1 provisioned" \
		all_provisioned sp0 1
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"thin_ok": true}}}
EOF
	wait_until "$WAIT_SHORT" "td0 created true" td_created sp0 td0
	local sprev
	sprev=$(sp_rev 1)
	log "  SpRev after the fixture: $sprev"

	stage 2 "AR5 failover: the primary's own row fails it"
	# Step 3's AR7 replacement hangs off the SAME err_epoch this step stamps:
	# worker/reaction.go's replaceTarget wants only !disabled, err_epoch +
	# cntlr_unhealthy (4 s) reached, !primary and this coordinator's own
	# unhealthy verdict on the cntlr (dnv-worker.md AR10), which C1's ERROR row
	# keeps giving; model.Failover flips Primary
	# and leaves ErrEpoch alone; and cn 3 has had a budget for the 3-extent
	# footprint since step 1. So the replacement lands ~2 s after the failover
	# — while this step is still polling — and every baseline step 3 compares
	# against has to be sampled HERE, before the ERROR row exists.
	#
	# `set-free cn 1 64` moves up for the same reason: cn 1 has to be healthy
	# and allocatable BEFORE the replacement runs, or what excluded it from the
	# pick was an empty budget and not AR7's black list of the old cntlr's
	# endpoint — which is the whole point of step 3.
	ctl set-free cn --id 1 --free-ext 64
	local failovers next_id replaces
	local cn1_free_before cn3_free_before cn3_rev_before
	failovers=$(reaction_cnt failover)
	next_id=$(ctl get-sp --sp sp0 | "$JQ" -r .sp_conf.next_id)
	replaces=$(reaction_cnt replace_cntlr)
	cn1_free_before=$(cn_free 1)
	cn3_free_before=$(cn_free 3)
	cn3_rev_before=$(cn_rev 3)
	assert_eq "$cn1_free_before" 64 "cn 1 free_ext_cnt before the failing row"
	# put-sp creates C1 settling (HL2) and a settling primary is held to
	# cntlr_unhealthy, not primary_unhealthy (AR5). Its first clean reply as
	# primary settled it during step 1; this step times the SETTLED threshold,
	# so it says so before the row lands. Step 12 is the settling one.
	wait_until "$WAIT_SHORT" "C1 settled (settling false)" cntlr_settled sp0 1
	set_behavior cn0 <<EOF
{"objects": {"cntlr 1:1": {"thin_ok": true, "rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	wait_until "$WAIT_SHORT" "C1 err_epoch set" cntlr_epoch_set sp0 1
	# A failed read meets that wait too (an empty read is not 0), so this
	# read can be the first one that answered, before the worker stamped the
	# epoch. A 0 would put the threshold below in 1970 and pass any failover,
	# so it dies here like a read that is not a number at all.
	local c1_epoch
	c1_epoch=$(cntlr_field sp0 1 err_epoch)
	case "$c1_epoch" in
	'' | 0 | *[!0-9]*) die "C1's err_epoch read back as '$c1_epoch'" ;;
	esac
	wait_until $((2 + WAIT_SHORT)) "reaction applied kind=failover" \
		reaction_ge $((failovers + 1)) failover
	# "Not before the 2s primary threshold" is asserted against the epoch the
	# worker itself stamped, not by a live negative: err_epoch is whole
	# seconds, so a correct failover can land as early as 1s after the row
	# does, while a worker that ignored the threshold would fail over only a
	# round or two after it — no window timed from the row's landing tells
	# the two apart. AR5 fires once now - err_epoch >= primary_unhealthy on
	# the server's clock, the clock the record's own stamp comes from.
	local failover_at
	failover_at=$(reaction_epochs failover | sed -n "$((failovers + 1))p")
	[ -n "$failover_at" ] || die "no failover record to time"
	ts_ge "$failover_at" $((c1_epoch + 2)) ||
		die "the failover ran at $failover_at, before C1's err_epoch $c1_epoch + the 2s primary threshold"
	wait_until "$WAIT_SHORT" "C2 primary true" cntlr_is_primary sp0 2
	# C1 is no longer primary. The replacement above hangs off the same
	# err_epoch, cntlr_unhealthy - primary_unhealthy = 2 s after the
	# failover, so by the time these polls return it may already have
	# deleted C1's record — a C1 that AR7 replaced is as demoted as one that
	# reads primary false, and only a record gone with no replacement is a
	# failure (the settle write that follows a failover can race this read).
	local c1_primary
	c1_primary=$(cntlr_field sp0 1 primary 2>/dev/null || true)
	if [ -z "$c1_primary" ]; then
		reaction_ge $((replaces + 1)) replace_cntlr ||
			die "C1's record is gone and no replace_cntlr reaction was applied"
		log "  C1 already replaced (AR7) when read: demoted"
	else
		assert_eq "$c1_primary" false "C1 primary after the failover"
	fi
	wait_until "$WAIT_SHORT" "dn0: SyncupSide with primary_cn_id 2" \
		req_ge 1 dn0 SyncupSide '(.side_conf.primary_cn_id | tostring) == "2"'
	wait_until "$WAIT_SHORT" "dn1: SyncupSide with primary_cn_id 2" \
		req_ge 1 dn1 SyncupSide '(.side_conf.primary_cn_id | tostring) == "2"'
	wait_until "$WAIT_SHORT" "cn1: SyncupCntlr with cntlr.primary true" \
		req_ge 1 cn1 SyncupCntlr '.cntlr.primary == true'

	stage 3 "AR7 replacement: the failed cntlr moves to the only other CN"
	# cn 1 is healthy and allocatable — step 2 gave it a 64-extent budget
	# before the ERROR row landed — so AR7's black list of the OLD cntlr's
	# endpoint is the only thing that can keep it out of the pick. Every
	# baseline below was sampled in step 2, before the reaction could fire.
	wait_until $((4 + WAIT_SHORT)) "reaction applied kind=replace_cntlr" \
		reaction_ge $((replaces + 1)) replace_cntlr
	wait_until "$WAIT_SHORT" "C1 gone from cntlr_id_list" cntlr_gone sp0 1
	local new_cntlr
	new_cntlr=$(ctl get-sp --sp sp0 | "$JQ" -r '.sp_conf.cntlr_id_list[]' |
		grep -vx 2 | sed -n 1p)
	assert_eq "$new_cntlr" "$next_id" "the new cntlr id (SpConf.next_id before the pass)"
	assert_eq "$(cntlr_field sp0 "$new_cntlr" addr_port)" "$(cn_addr 3)" \
		"the new cntlr's CN"
	assert_field "$(ctl get-cntlr --sp sp0 --id "$new_cntlr")" .cntlid_slot 0 \
		"the new cntlr keeps C1's cntlid_slot"
	assert_field "$(ctl get-cntlr --sp sp0 --id "$new_cntlr")" .primary false \
		"the new cntlr is not the primary"
	assert_eq "$(ctl get-cn --id 1 | "$JQ" -r '.cntlr_ptr_list | length')" 0 \
		"cn 1 cntlr_ptr_list after the replacement"
	assert_eq "$(cn_free 1)" $((cn1_free_before + 3)) \
		"cn 1 free_ext_cnt restored by the SP footprint"
	assert_eq "$(ctl get-cn --id 3 | "$JQ" -r '.cntlr_ptr_list | length')" 1 \
		"cn 3 cntlr_ptr_list after the replacement"
	assert_eq "$(cn_free 3)" $((cn3_free_before - 3)) "cn 3 budget debited"
	assert_ge "$(cn_rev 3)" $((cn3_rev_before + 1)) "cn 3 CnRev bumped"
	# The CdcEntry follows the listing rule (architecture.md [D18]): step 2's
	# failover took cn 1's transport out in its own STM, C1 being a standby
	# with an err_epoch from then on, and the replacement's own STM listed
	# the new cntlr, a clean standby. Each node's NvmeTrConf carries that
	# node's OWN port as tr_svc_id (workerctl), so the list is attributable
	# per CN and step 3's "lists cn 2 and cn 3, not cn 1" is checkable as a
	# SET. The expectation is also derived from what the SP actually holds —
	# the primary and a clean standby, both listed: the two must agree.
	local cdc cdc_ports want_ports
	cdc=$(ctl get --key "$cdc_key")
	assert_field "$cdc" .nqn "$NQN_PREFIX:ss0" "the CdcEntry's nqn"
	assert_field "$cdc" '.nvme_tr_conf_list | length' 2 \
		"the CdcEntry advertises one transport per surviving cntlr"
	cdc_ports=$(jq_of "$cdc" '[.nvme_tr_conf_list[].tr_svc_id] | sort | join(",")')
	want_ports=$(ctl get-cntlr --sp sp0 |
		"$JQ" -r '[.[] | .addr_port | split(":") | last] | sort | join(",")')
	assert_eq "$cdc_ports" "$(cn_port 2),$(cn_port 3)" \
		"the CdcEntry lists cn 2's and cn 3's transports and not cn 1's"
	assert_eq "$cdc_ports" "$want_ports" \
		"the CdcEntry's transports are exactly the surviving cntlrs' CNs"
	wait_until "$WAIT_SHORT" "cn0: SyncupCn with an empty cntlr_pointer_list" \
		req_ge 1 cn0 SyncupCn '((.cntlr_pointer_list // []) | length) == 0'
	wait_until "$WAIT_SHORT" "cn2: SyncupCn with one cntlr pointer" \
		req_ge 1 cn2 SyncupCn '((.cntlr_pointer_list // []) | length) == 1'
	wait_until "$WAIT_SHORT" "cn2: SyncupCntlr" req_ge 1 cn2 SyncupCntlr

	stage 4 "AR7 sole primary: no failover candidate ⇒ a new PRIMARY"
	ctl set-free cn --id 1 --free-ext 0
	ctl put-sp --name sp1 --id 2 --shard 00 --slots 0,1,2 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:2:0:true \
		--slice 1:0 \
		--group 1:1:data:1:none \
		--leg 1:1:0 \
		--side 1:1:3:0
	ctl put-td --sp sp1 --name td1 --id "$TD_ID" --size 1073741824
	ctl put-ss --sp sp1 --nqn "$NQN_PREFIX:ss1" --id "$SS_ID" --ns "$NS_ID:1:$TD_ID"
	wait_until "$WAIT_SYNCUP" "sp1: the side provisioned" all_provisioned sp1 1
	local sp1_next sp1_replaces sp1_skips
	sp1_next=$(ctl get-sp --sp sp1 | "$JQ" -r .sp_conf.next_id)
	sp1_replaces=$(reaction_cnt replace_cntlr)
	sp1_skips=$(reaction_skip_cnt failover "no candidate")
	# As in step 2: the skip below is timed against the settled primary's
	# primary_unhealthy, and sp1's sole cntlr was created settling.
	wait_until "$WAIT_SHORT" "sp1: its primary settled (settling false)" \
		cntlr_settled sp1 1
	set_behavior cn1 <<EOF
{"objects": {"cntlr 2:1": {"rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	wait_until "$WAIT_SHORT" "sp1: reaction skipped kind=failover reason=no candidate" \
		reaction_skip_ge $((sp1_skips + 1)) failover "no candidate"
	wait_until $((4 + WAIT_SHORT)) "sp1: reaction applied kind=replace_cntlr" \
		reaction_ge $((sp1_replaces + 1)) replace_cntlr
	wait_until "$WAIT_SHORT" "sp1: the old cntlr is gone" cntlr_gone sp1 1
	assert_eq "$(cntlr_field sp1 "$sp1_next" addr_port)" "$(cn_addr 3)" \
		"sp1: the replacement's CN (cn 3 was the only candidate)"
	assert_field "$(ctl get-cntlr --sp sp1 --id "$sp1_next")" .primary true \
		"sp1: the replacement is the new primary"
	assert_field "$(ctl get-cntlr --sp sp1 --id "$sp1_next")" .cntlid_slot 0 \
		"sp1: the replacement keeps the cntlid_slot"

	stage 5 "AR6 data grow, the pending rule and the watermark"
	# sp0's primary is C2 on cn 2 (fake cn1) after step 2's failover.
	local prim prim_id prim_addr prim_dir
	prim=$(sp_primary sp0)
	prim_id=${prim%% *}
	prim_addr=${prim##* }
	prim_dir=$(dir_of_addr "$prim_addr")
	log "  sp0's primary is cntlr $prim_id on $prim_addr ($prim_dir)"
	ctl set-lwm --sp sp0 --pct "$LWM"
	# Exactly two eligible DNs for a two-leg group.
	ctl set-free dn --id 3 --free-ext 8
	ctl set-free dn --id 4 --free-ext 8
	ctl set-free dn --id 1 --free-ext 0
	ctl set-free dn --id 2 --free-ext 0
	local grows dn3_rev dn4_rev cn2_rev cn3_rev cn2_free cn3_free
	grows=$(reaction_cnt grow_data)
	dn3_rev=$(dn_rev 3)
	dn4_rev=$(dn_rev 4)
	cn2_rev=$(cn_rev 2)
	cn3_rev=$(cn_rev 3)
	cn2_free=$(cn_free 2)
	cn3_free=$(cn_free 3)
	assert_eq "$(grp_count sp0 1 data)" 1 "data groups before the grow"

	# used_data 80 of 125 -> 80*100 > 50*125, so the pool breaches; the meta
	# pair stays far below its own watermark (100 of 15616).
	local line
	line=$(thin_pool_line 100 "$META_BLOCKS_PER_GRP" 80 "$DATA_GRP_DATA_BLOCKS")
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	wait_until "$WAIT_SHORT" "reaction applied kind=grow_data" \
		reaction_ge $((grows + 1)) grow_data
	wait_until "$WAIT_SHORT" "a second data group" grp_count_is sp0 1 data 2
	local grp
	grp=$(last_grp sp0 1 data)
	assert_field "$grp" .ext_cnt 2 "the new data group's ext_cnt"
	assert_field "$grp" .meta_blocks "$GRP_META_BLOCKS" "the new data group's meta_blocks"
	assert_field "$grp" .data_blocks "$DATA_GRP_DATA_BLOCKS" \
		"the new data group's data_blocks"
	assert_eq "$(jq_of "$grp" '[.leg_list[].side_list[0].addr_port] | sort | join(",")')" \
		"$(printf '%s,%s' "$(dn_addr 3)" "$(dn_addr 4)")" \
		"the new data group's two sides"
	# [D15]: the sides are created provisioned = false. The fakes zero
	# instantly and RW18 flips them within a round, so the evidence is the
	# FIRST SyncupSide the worker built for each new side.
	local sid dir
	for sid in $(jq_of "$grp" '.leg_list[].side_list[0].side_id'); do
		dir=$(dir_of_addr "$(jq_of "$grp" \
			"[.leg_list[].side_list[0] | select((.side_id|tostring)==\"$sid\") | .addr_port] | first")")
		wait_until "$WAIT_SHORT" "the first SyncupSide for side $sid at $dir" \
			req_ge 1 "$dir" SyncupSide '(.side_pointer.side_id | tostring) == $s' --arg s "$sid"
		assert_eq "$(first_side_provisioned "$dir" "$sid")" false \
			"side $sid was created with provisioned false"
	done
	assert_eq "$(dn_free 3)" 6 "dn 3 free_ext_cnt after the grow"
	assert_eq "$(dn_free 4)" 6 "dn 4 free_ext_cnt after the grow"
	assert_ge "$(dn_rev 3)" $((dn3_rev + 1)) "dn 3 DnRev bumped"
	assert_ge "$(dn_rev 4)" $((dn4_rev + 1)) "dn 4 DnRev bumped"
	assert_ge "$(cn_rev 2)" $((cn2_rev + 1)) "cn 2 CnRev bumped"
	assert_ge "$(cn_rev 3)" $((cn3_rev + 1)) "cn 3 CnRev bumped"
	assert_eq "$(cn_free 2)" $((cn2_free - 2)) "cn 2 budget debited by the grow"
	assert_eq "$(cn_free 3)" $((cn3_free - 2)) "cn 3 budget debited by the grow"

	# PENDING (AR6, stateless). The reported total is still 125 while the
	# slice's data groups excluding the newest already total 125, so
	# 125 <= 125 and no second grow may start.
	grows=$(reaction_cnt grow_data)
	local pendings
	pendings=$(reaction_skip_cnt grow_data grow_pending)
	assert_none_for 5 "a second data grow while the first is pending" \
		reaction_ge $((grows + 1)) grow_data
	assert_ge "$(reaction_skip_cnt grow_data grow_pending)" $((pendings + 1)) \
		"reaction skipped kind=grow_data reason=grow_pending"

	# The totals catch up: 250 > 125, so nothing is pending any more — and
	# 80*100 = 8000 is below 50*250 = 12500, so nothing grows either.
	line=$(thin_pool_line 100 "$META_BLOCKS_PER_GRP" 80 $((2 * DATA_GRP_DATA_BLOCKS)))
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	assert_none_for 5 "a data grow below the watermark" \
		reaction_ge $((grows + 1)) grow_data

	# Two more prepared DNs and a real breach: 140*100 > 50*250.
	ctl set-free dn --id 1 --free-ext 8
	ctl set-free dn --id 2 --free-ext 8
	ctl set-free dn --id 3 --free-ext 0
	ctl set-free dn --id 4 --free-ext 0
	line=$(thin_pool_line 100 "$META_BLOCKS_PER_GRP" 140 $((2 * DATA_GRP_DATA_BLOCKS)))
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	wait_until "$WAIT_SHORT" "a third data group" grp_count_is sp0 1 data 3
	assert_ge "$(reaction_cnt grow_data)" $((grows + 1)) \
		"reaction applied kind=grow_data for the third group"
	grp=$(last_grp sp0 1 data)
	assert_eq "$(jq_of "$grp" '[.leg_list[].side_list[0].addr_port] | sort | join(",")')" \
		"$(printf '%s,%s' "$(dn_addr 1)" "$(dn_addr 2)")" \
		"the third data group's two sides"

	stage 6 "AR6 meta grow, the ladder and the pending rule"
	local total_data metagrows
	total_data=$((3 * DATA_GRP_DATA_BLOCKS))
	metagrows=$(reaction_cnt grow_meta)
	assert_eq "$(grp_count sp0 1 meta)" 1 "meta groups before the grow"
	# used_meta 9000 of 15616 -> 900000 > 50*15616 = 780800; the data pair is
	# kept far below its own watermark so AR6's data-first rule does not fire.
	line=$(thin_pool_line 9000 "$META_BLOCKS_PER_GRP" 10 "$total_data")
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	wait_until "$WAIT_SHORT" "reaction applied kind=grow_meta" \
		reaction_ge $((metagrows + 1)) grow_meta
	wait_until "$WAIT_SHORT" "a second meta group" grp_count_is sp0 1 meta 2
	grp=$(last_grp sp0 1 meta)
	assert_field "$grp" .ext_cnt 1 "the ladder value of the first meta grow"
	assert_field "$grp" .data_blocks "$META_GRP_DATA_BLOCKS" \
		"the new meta group's data_blocks"

	metagrows=$(reaction_cnt grow_meta)
	pendings=$(reaction_skip_cnt grow_meta grow_pending)
	assert_none_for 5 "a second meta grow while the first is pending" \
		reaction_ge $((metagrows + 1)) grow_meta
	assert_ge "$(reaction_skip_cnt grow_meta grow_pending)" $((pendings + 1)) \
		"reaction skipped kind=grow_meta reason=grow_pending"

	# The metadata totals catch up: 31232 > 15616 (not pending) and
	# 9000*100 = 900000 < 50*31232 = 1561600 (below the watermark).
	line=$(thin_pool_line 9000 $((2 * META_BLOCKS_PER_GRP)) 10 "$total_data")
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	assert_none_for 5 "a meta grow below the watermark" \
		reaction_ge $((metagrows + 1)) grow_meta

	# 20000*100 = 2000000 > 1561600: the ladder doubles, ext_cnt 2.
	line=$(thin_pool_line 20000 $((2 * META_BLOCKS_PER_GRP)) 10 "$total_data")
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	wait_until "$WAIT_SHORT" "a third meta group" grp_count_is sp0 1 meta 3
	grp=$(last_grp sp0 1 meta)
	assert_field "$grp" .ext_cnt 2 "the ladder value of the second meta grow"

	# lwm > 100 switches the automation off at any usage.
	ctl set-lwm --sp sp0 --pct 101
	grows=$(reaction_cnt grow_data)
	metagrows=$(reaction_cnt grow_meta)
	line=$(thin_pool_line 30000 $((2 * META_BLOCKS_PER_GRP)) 300 "$total_data")
	pool_behavior "$prim_dir" 1 "$prim_id" "$line"
	assert_none_for 3 "a data grow at lwm 101" reaction_ge $((grows + 1)) grow_data
	assert_eq "$(reaction_cnt grow_meta)" "$metagrows" "meta grows at lwm 101"

	stage 7 "AR8 leg repair, case 1 (the long leg threshold)"
	# The pool row goes away with the grows: from here the primary reports
	# only leg rows, so AR6 has nothing to parse and AR8 owns the pass.
	# Exactly one DN outside G2 is eligible for a two-extent spare.
	ctl set-free dn --id 3 --free-ext 8
	ctl set-free dn --id 4 --free-ext 0
	ctl set-free dn --id 1 --free-ext 0
	ctl set-free dn --id 2 --free-ext 0
	local creates switches legs_before
	creates=$(reaction_cnt spare_create)
	switches=$(reaction_cnt spare_switch)
	# The leg ids G2 holds BEFORE the repair, across both lists. The new spare
	# is identified against this set rather than by reading spare_leg_list[0]:
	# the fakes provision instantly and the primary reports the spare OK by
	# default, so AR8's switch step can land before the assertion runs, and
	# spare_leg_list[0] is then the PARKED OLD LEG (the switch step of AR8
	# parks it there), not the spare.
	legs_before=$(group_legs sp0 1 data 2 | awk '{ print $1 }' | sort -n | tr '\n' ' ')
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {"leg_id_to_leg.3":
  {"status": "ERROR", "details": "no healthy path"}}}}}
EOF
	wait_until "$WAIT_SHORT" "L3 err_epoch set" leg_epoch_set sp0 1 3
	local l3_epoch
	l3_epoch=$(leg_epoch sp0 1 3)
	# As in step 2: the wait is met by a failed read too, and an unstamped 0
	# would pass the compare below for any spare_create.
	case "$l3_epoch" in
	'' | 0 | *[!0-9]*) die "L3's err_epoch read back as '$l3_epoch'" ;;
	esac
	# The live negative is short BECAUSE it is timed from when this poll saw
	# the epoch, not from when the worker stamped it: every poll costs a full
	# ssh (SSH_OPTS has no ControlMaster), so the observation can lag a second
	# or more and a legitimate spare at err_epoch + 6 s would land inside a
	# longer window. "Not before the 6 s leg threshold" is asserted right
	# after, against the epoch the worker itself stamped.
	assert_none_for 3 "a spare before the 6s leg threshold" \
		reaction_ge $((creates + 1)) spare_create
	wait_until $((6 + WAIT_SHORT)) "reaction applied kind=spare_create on G2" \
		reaction_ge $((creates + 1)) spare_create
	local l3_create_at
	l3_create_at=$(reaction_epochs spare_create | sed -n "$((creates + 1))p")
	[ -n "$l3_create_at" ] || die "no spare_create record to time"
	ts_ge "$l3_create_at" $((l3_epoch + 6)) ||
		die "the G2 spare_create ran at $l3_create_at, before L3's err_epoch $l3_epoch + the 6s leg threshold"
	wait_until "$WAIT_SHORT" "G2 to hold a leg it did not have before" \
		group_has_new_leg sp0 1 data 2 "$legs_before"
	local spare spare_leg spare_side spare_addr spare_dir
	spare=$(new_group_leg sp0 1 data 2 "$legs_before")
	spare_leg=$(printf '%s' "$spare" | awk '{ print $1 }')
	spare_side=$(printf '%s' "$spare" | awk '{ print $2 }')
	spare_addr=$(printf '%s' "$spare" | awk '{ print $3 }')
	spare_dir=$(dir_of_addr "$spare_addr")
	assert_eq "$spare_addr" "$(dn_addr 3)" "the spare's DN (the only eligible one)"
	wait_until "$WAIT_SHORT" "the first SyncupSide for the spare's side" \
		req_ge 1 "$spare_dir" SyncupSide \
		'(.side_pointer.side_id | tostring) == $s' --arg s "$spare_side"
	assert_eq "$(first_side_provisioned "$spare_dir" "$spare_side")" false \
		"the spare's side was created with provisioned false"
	wait_until "$WAIT_SYNCUP" "RW18 to flip the spare's side" \
		side_provisioned sp0 1 "$spare_side"
	wait_until $((6 + WAIT_SHORT)) "reaction applied kind=spare_switch" \
		reaction_ge $((switches + 1)) spare_switch
	wait_until "$WAIT_SHORT" "the spare to take L3's place in leg_list" \
		leg_in_list sp0 1 data 2 "$spare_leg"
	assert_eq "$(active_legs sp0 1 data 2 | awk 'NR==1 { print $1 }')" "$spare_leg" \
		"the spare sits at L3's POSITION in leg_list (MD6 SwitchSpareLeg swaps list slots; the stored leg_idx is assigned at creation and is deliberately not renumbered)"
	assert_eq "$(spare_legs sp0 1 data 2 | awk '{ print $1 }')" 3 \
		"L3 is parked in spare_leg_list"
	# Sampled into a variable, so that a failed read stops the run under
	# `set -e`: compared in place, the substitution is an argument, whose
	# status nothing checks, and the empty output of a dropped ssh is not 0.
	local l3_parked_epoch
	l3_parked_epoch=$(spare_legs sp0 1 data 2 | awk '$1 == 3 { print $4 }')
	case "$l3_parked_epoch" in
	'' | 0 | *[!0-9]*) die "the parked L3 keeps its err_epoch: got '$l3_parked_epoch'" ;;
	esac
	wait_until "$WAIT_SHORT" "SyncupSide for the spare's side carries primary_cn_id" \
		req_ge 1 "$spare_dir" SyncupSide \
		'(.side_pointer.side_id | tostring) == $s and (.side_conf.primary_cn_id | tostring) != "0"' \
		--arg s "$spare_side"
	# "No further repair of L3" is every AR8 step, not only the switch: a
	# worker that kept allocating spares for the parked leg — or parked a
	# second one — would sail through a switch-only check.
	switches=$(reaction_cnt spare_switch)
	creates=$(reaction_cnt spare_create)
	local g2_spares
	g2_spares=$(spare_count sp0 1 data 2)
	assert_none_for 4 "a further repair of the parked L3" \
		g2_repair_progressed "$switches" "$creates" "$g2_spares"

	stage 8 "AR8 leg repair, case 2 (leg AND side, the short threshold)"
	# The negative first: a side error alone must never repair anything.
	ctl set-free dn --id 4 --free-ext 8
	ctl set-free dn --id 3 --free-ext 0
	creates=$(reaction_cnt spare_create)
	# BOTH counters are baselined BEFORE the trigger. spare_create and
	# spare_switch land about a second apart (the fakes provision instantly
	# and the primary reports the fresh spare OK by default), so a baseline
	# taken after the create — with several ssh round trips in between — is
	# taken after the SWITCH as well, and the wait below would then be asking
	# for a second switch that never comes.
	switches=$(reaction_cnt spare_switch)
	# G1's leg ids before the repair, across BOTH lists — the new spare is
	# found against this set, not by reading spare_leg_list[0], which after
	# AR8 step 1's switch holds the PARKED OLD LEG (see stage 7).
	local g1_legs_before
	g1_legs_before=$(group_legs sp0 1 meta 1 | awk '{ print $1 }' | sort -n | tr '\n' ' ')
	set_behavior dn0 <<'EOF'
{"objects": {"side 1:1:1": {"rows": {"side_dev_info":
  {"status": "ERROR", "details": "dn is gone"}}}}}
EOF
	wait_until "$WAIT_SHORT" "S1 err_epoch set" side_epoch_set sp0 1 1
	assert_none_for 5 "a spare from a side error with a healthy leg" \
		reaction_ge $((creates + 1)) spare_create
	# Now the leg row too: case 2 fires at the 3s side threshold, and the
	# whole point of the case is that it fires BEFORE the 6s leg threshold —
	# which a 3 + WAIT_SHORT = 8 s budget cannot express, since a worker that
	# ignored the combined leg+side rule and waited for the plain leg
	# threshold would fit inside it. So the server clock is stamped before the
	# row is written and the spare_create record's OWN time has to be less
	# than 6 s after that reading: the leg's err_epoch cannot precede the row,
	# so the leg-threshold-only worker could not react before it either.
	local leg_row_at
	leg_row_at=$(server_now)
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {
  "leg_id_to_leg.3": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.1": {"status": "ERROR", "details": "no healthy path"}}}}}
EOF
	wait_until "$WAIT_SHORT" "L1 err_epoch set" leg_epoch_set sp0 1 1
	wait_until $((3 + WAIT_SHORT)) "reaction applied kind=spare_create on G1" \
		reaction_ge $((creates + 1)) spare_create
	local g1_create_at g1_deadline
	g1_create_at=$(reaction_epochs spare_create | sed -n "$((creates + 1))p")
	[ -n "$g1_create_at" ] || die "no spare_create record to time"
	g1_deadline=$(awk -v t="$(ts_epoch "$leg_row_at")" 'BEGIN { printf "%.3f", t + 6 }')
	ts_lt "$g1_create_at" "$g1_deadline" ||
		die "the G1 spare_create ran at $g1_create_at, not before the 6s leg threshold at $g1_deadline (leg row written at $leg_row_at)"
	wait_until "$WAIT_SHORT" "G1 to hold a leg it did not have before" \
		group_has_new_leg sp0 1 meta 1 "$g1_legs_before"
	local g1_spare g1_leg g1_side g1_addr g1_dir
	g1_spare=$(new_group_leg sp0 1 meta 1 "$g1_legs_before")
	g1_leg=$(printf '%s' "$g1_spare" | awk '{ print $1 }')
	g1_side=$(printf '%s' "$g1_spare" | awk '{ print $2 }')
	g1_addr=$(printf '%s' "$g1_spare" | awk '{ print $3 }')
	g1_dir=$(dir_of_addr "$g1_addr")
	assert_eq "$g1_addr" "$(dn_addr 4)" "the G1 spare's DN (the only eligible one)"
	wait_until "$WAIT_SYNCUP" "RW18 to flip the G1 spare's side" \
		side_provisioned sp0 1 "$g1_side"
	wait_until $((3 + WAIT_SHORT)) "reaction applied kind=spare_switch on G1" \
		reaction_ge $((switches + 1)) spare_switch
	wait_until "$WAIT_SHORT" "the G1 spare to take L1's place" \
		leg_in_list sp0 1 meta 1 "$g1_leg"
	clear_behavior dn0

	stage 9 "AR8 spare_list_full: only an operator frees a slot"
	# Park a second leg by repairing G1's surviving original leg L2 (its side
	# S2 lives on dn 2), then fail an active leg with the list full.
	ctl set-free dn --id 3 --free-ext 8
	ctl set-free dn --id 4 --free-ext 0
	creates=$(reaction_cnt spare_create)
	switches=$(reaction_cnt spare_switch)
	# G1's leg ids before this second repair, so the fresh spare is found by
	# id rather than by excluding the two ORIGINAL leg ids: by now the group
	# also holds stage 8's spare (switched in) and its parked leg, so an
	# id-exclusion filter is ambiguous the moment a third repair is added.
	local g1_legs_before2
	g1_legs_before2=$(group_legs sp0 1 meta 1 | awk '{ print $1 }' | sort -n | tr '\n' ' ')
	set_behavior dn1 <<'EOF'
{"objects": {"side 1:2:2": {"rows": {"side_dev_info":
  {"status": "ERROR", "details": "dn is gone"}}}}}
EOF
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {
  "leg_id_to_leg.3": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.1": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.2": {"status": "ERROR", "details": "no healthy path"}}}}}
EOF
	wait_until "$WAIT_SHORT" "L2 err_epoch set" leg_epoch_set sp0 1 2
	wait_until $((3 + WAIT_SHORT)) "a second spare_create on G1" \
		reaction_ge $((creates + 1)) spare_create
	wait_until "$WAIT_SHORT" "G1 to hold a leg it did not have before" \
		group_has_new_leg sp0 1 meta 1 "$g1_legs_before2"
	local g1_spare2 g1_side2
	g1_spare2=$(new_group_leg sp0 1 meta 1 "$g1_legs_before2")
	[ -n "$g1_spare2" ] || die "no fresh spare in G1's spare_leg_list"
	g1_side2=$(printf '%s' "$g1_spare2" | awk '{ print $2 }')
	wait_until "$WAIT_SYNCUP" "RW18 to flip the second G1 spare's side" \
		side_provisioned sp0 1 "$g1_side2"
	wait_until $((3 + WAIT_SHORT)) "a second spare_switch on G1" \
		reaction_ge $((switches + 1)) spare_switch
	wait_until "$WAIT_SHORT" "G1 to hold two parked legs" \
		spare_count_is sp0 1 meta 1 2
	# Both slots are taken: failing an active leg can only be skipped now.
	local active fail_leg fail_side fail_dir fulls
	active=$(active_legs sp0 1 meta 1 | sed -n 1p)
	fail_leg=$(printf '%s' "$active" | awk '{ print $1 }')
	fail_side=$(printf '%s' "$active" | awk '{ print $2 }')
	fail_dir=$(dir_of_addr "$(printf '%s' "$active" | awk '{ print $3 }')")
	creates=$(reaction_cnt spare_create)
	fulls=$(reaction_skip_cnt spare_create spare_list_full)
	set_behavior "$fail_dir" <<EOF
{"objects": {"side 1:$fail_leg:$fail_side": {"rows": {"side_dev_info":
  {"status": "ERROR", "details": "dn is gone"}}}}}
EOF
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {
  "leg_id_to_leg.3": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.1": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.2": {"status": "ERROR", "details": "no healthy path"},
  "leg_id_to_leg.$fail_leg": {"status": "ERROR", "details": "no healthy path"}}}}}
EOF
	wait_until $((6 + WAIT_SHORT)) "reaction skipped reason=spare_list_full" \
		reaction_skip_ge $((fulls + 1)) spare_create spare_list_full
	assert_eq "$(reaction_cnt spare_create)" "$creates" \
		"spare allocations while spare_leg_list is full"
	assert_eq "$(spare_count sp0 1 meta 1)" 2 "G1 spare_leg_list length"

	stage 10 "AR3 suppression: sp_level and a disabled cntlr"
	prim=$(sp_primary sp0)
	prim_id=${prim%% *}
	prim_dir=$(dir_of_addr "${prim##* }")
	log "  sp0's primary is cntlr $prim_id on $prim_dir"
	ctl set-level --sp sp0 --level 48
	failovers=$(reaction_cnt failover)
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	wait_until "$WAIT_SHORT" "the primary's err_epoch set under suppression" \
		cntlr_epoch_set sp0 "$prim_id"
	assert_none_for 5 "a failover at sp_level NO_THINPOOL" \
		reaction_ge $((failovers + 1)) failover

	ctl set-level --sp sp0 --level 0
	wait_until "$WAIT_SHORT" "reaction applied kind=failover once suppression lifts" \
		reaction_ge $((failovers + 1)) failover
	wait_until "$WAIT_SHORT" "the failed cntlr to lose the primary role" \
		cntlr_not_primary sp0 "$prim_id"

	# The demoted cntlr is now an unhealthy STANDBY well past the 4 s
	# cntlr threshold, so AR7 would replace it on the very next pass — the
	# only reason it does not is that cn 1, the sole candidate (the old CN is
	# black-listed and the other cntlr's CN is excluded by architecture.md,
	# Finding CN candidates), still has a zero budget from step 4. So the
	# order is load-bearing: disable FIRST, then hand cn 1 a budget. The
	# assertion that follows then really tests AR3's "a disabled cntlr is
	# never replaced" and not an empty candidate set.
	ctl set-cntlr --sp sp0 --id "$prim_id" --disabled=true
	ctl set-free cn --id 1 --free-ext 64
	replaces=$(reaction_cnt replace_cntlr)
	assert_none_for 6 "a replacement of the disabled standby" \
		reaction_ge $((replaces + 1)) replace_cntlr

	stage 11 "AR5 disabled primary: the flag alone is the trigger"
	# Step 10 leaves sp0 with the step-3 replacement as a healthy primary and
	# ONE other cntlr: the cntlr it demoted, now disabled and still unhealthy.
	# AR5's candidate rule is unchanged — a disabled or unhealthy cntlr is
	# never ELECTED — so that standby has to be healthy and enabled again
	# before a disabled primary has anywhere to go.
	#
	# The order is load-bearing for the same reason step 10's is, in reverse:
	# cn 1 has had a budget since then, so a standby that is enabled while
	# still past the 4 s cntlr threshold is replaced by AR7 on the next pass.
	# Clearing its row and waiting err_epoch back to 0 FIRST keeps AR3's
	# hands-off protection on it until there is nothing left to replace.
	local standby_id=$prim_id standby_dir=$prim_dir
	clear_behavior "$standby_dir"
	wait_until "$WAIT_SHORT" "C$standby_id err_epoch cleared" \
		cntlr_epoch_clear sp0 "$standby_id"
	ctl set-cntlr --sp sp0 --id "$standby_id" --disabled=false
	local dis dis_id standby_syncups
	dis=$(sp_primary sp0)
	dis_id=${dis%% *}
	log "  sp0's primary is cntlr $dis_id; C$standby_id is the enabled healthy standby"
	standby_syncups=$(reqs "$standby_dir" SyncupCntlr '.cntlr.primary == true')
	failovers=$(reaction_cnt failover)
	# The disable is workerctl's direct etcd write — what the gateway's
	# UpdateCntlrEnabled does to the store: `disabled` plus one SpRev bump,
	# which is what wakes the sp-worker. Nothing here is unhealthy, so the
	# err_epoch trigger cannot fire at ANY threshold and the flag is the only
	# thing a failover can come from; the two err_epoch assertions below are
	# that proof in the stored state. A pass runs every cntlr_interval (1 s),
	# so one pass fits inside WAIT_SHORT with no threshold added to it — the
	# 2 s that step 2 had to budget for.
	ctl set-cntlr --sp sp0 --id "$dis_id" --disabled=true
	wait_until "$WAIT_SHORT" "reaction applied kind=failover within one pass" \
		reaction_ge $((failovers + 1)) failover
	wait_until "$WAIT_SHORT" "C$standby_id primary true" \
		cntlr_is_primary sp0 "$standby_id"
	assert_eq "$(cntlr_field sp0 "$dis_id" primary)" false \
		"the disabled cntlr's primary flag after the failover"
	assert_eq "$(cntlr_field sp0 "$dis_id" err_epoch)" 0 \
		"the disabled primary's err_epoch (no threshold was waited out)"
	assert_eq "$(cntlr_field sp0 "$standby_id" err_epoch)" 0 \
		"the new primary's err_epoch"
	wait_until "$WAIT_SHORT" "$standby_dir: SyncupCntlr with cntlr.primary true" \
		req_ge $((standby_syncups + 1)) "$standby_dir" SyncupCntlr \
		'.cntlr.primary == true'

	stage 12 "settling: a promoted primary is held to cntlr_unhealthy (HL2, AR5)"
	# A fresh SP on thresholds of its own (primary 2, cntlr 15) so the
	# settling window is wide enough to watch: C1 on cn 1 (fake cn0) primary,
	# C2 on cn 2 (fake cn1) standby, one RedundNone data group on dn 4.
	#
	# cn 3 gets a zero budget first, which takes AR7 out of the picture for
	# sp2: cn 1 and cn 2 already host its cntlrs (architecture.md, Finding CN
	# candidates), so cn 3 is its only replacement candidate. Without that, C2
	# — an unhealthy STANDBY already past cntlr_unhealthy the moment the
	# fail-back below demotes it — would race its own err_epoch clear against
	# the next pass's AR7. sp0's cntlr on cn 3 is disabled and sp1's is
	# healthy, so neither needs cn 3's budget.
	ctl set-free cn --id 1 --free-ext 64
	ctl set-free cn --id 2 --free-ext 64
	ctl set-free cn --id 3 --free-ext 0
	ctl set-free dn --id 4 --free-ext 8
	ctl put-sp --name sp2 --id 3 --shard 00 --slots 0,1 --level 0 \
		--thresholds 2,15,3,6 --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 \
		--group 1:1:data:1:none \
		--leg 1:1:0 \
		--side 1:1:4:0
	# put-sp writes C1 settling, as CreateStoragePool does (dnv-worker.md,
	# Integration test plan, The driver). The worker's first round may already
	# have settled it by the time this read lands, so it is logged, not
	# asserted: the `cntlr settled` record below is the proof, because the
	# worker writes one only for a record it read as settling.
	log "  sp2 C1 settling right after put-sp: $(cntlr_field sp2 1 settling)"
	wait_until "$WAIT_SYNCUP" "sp2: the side provisioned" all_provisioned sp2 1
	wait_until "$WAIT_SYNCUP" "sp2: cntlr settled for C1 (the creation settle)" \
		settled_ge 3 1 1
	assert_eq "$(cntlr_field sp2 1 settling)" false \
		"sp2 C1 settling after its creation settle"
	assert_eq "$(cntlr_field sp2 2 settling)" false \
		"sp2 C2, a standby, is never settling"

	log "  12.2: a row that bites only as primary, planted on the standby"
	# The fake's when_primary (dnv-worker.md,
	# Integration test plan, The fake agent): the fake reports a standby's pool
	# rows too, so an ungated ERROR row would make C2 unhealthy — and no
	# failover candidate — before the failover this step needs.
	set_behavior cn1 <<'EOF'
{"objects": {"cntlr 3:2": {"rows": {"slice_id_to_dm_pool.1":
  {"status": "ERROR", "details": "settling test", "when_primary": true}}}}}
EOF
	assert_none_for 3 "sp2 C2 err_epoch set by a when_primary row on a standby" \
		cntlr_epoch_set sp2 2

	log "  12.3: fail the settled C1; C2 is promoted settling and reports ERROR"
	# C1 fails on a row other than C2's: AR5 does not hand the role back to a
	# cntlr while the new primary fails only on rows it failed on (AR5's same
	# error), and 12.5 needs the fail-back.
	local sp2_failovers c2_epoch c1_settles hold back_at
	sp2_failovers=$(reaction_cnt failover)
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 3:1": {"rows": {"slice_id_to_data.1":
  {"status": "ERROR", "details": "data concat gone"}}}}}
EOF
	wait_until $((2 + WAIT_SHORT)) "sp2: reaction applied kind=failover" \
		reaction_ge $((sp2_failovers + 1)) failover
	wait_until "$WAIT_SHORT" "sp2 C2 primary true" cntlr_is_primary sp2 2
	# Not a race: C2 cannot settle while every primary-shape reply it sends
	# carries the planted ERROR row.
	assert_eq "$(cntlr_field sp2 2 settling)" true \
		"sp2 C2 settling after its promotion (model.Failover sets it)"
	wait_until "$WAIT_SHORT" "sp2 C2 err_epoch set by its first primary-shape reply" \
		cntlr_epoch_set sp2 2
	c2_epoch=$(cntlr_field sp2 2 err_epoch)
	# As in step 2: the wait is met by a failed read too, and an unstamped 0
	# would skip the hold below and pass 12.5's compare for any fail-back.
	case "$c2_epoch" in
	'' | 0 | *[!0-9]*) die "sp2 C2's err_epoch read back as '$c2_epoch'" ;;
	esac
	clear_behavior cn0
	wait_until "$WAIT_SHORT" "sp2 C1 err_epoch cleared" cntlr_epoch_clear sp2 1

	log "  12.4: the hold — no fail-back inside cntlr_unhealthy (15 s)"
	# C1 is a healthy, enabled standby now, so a worker that judged C2 by
	# primary_unhealthy (2 s) would already have failed back. The window runs
	# until 2 s short of C2's err_epoch + 15 s on the SERVER's clock — a fixed
	# length would have to guess how long the polls above took — and the
	# fail-back's own record is checked against the same epoch in 12.5.
	# The clock is read in a statement of its own, so a failed read stops the
	# run. Read inside an argument, a failed read would be an empty stamp,
	# which `date -u -d ""` takes for midnight today, and the hold would sleep
	# for about as many seconds as the day had run.
	local now_at now_epoch
	now_at=$(server_now)
	now_epoch=$(ts_epoch "$now_at")
	hold=$(awk -v e="$c2_epoch" -v n="$now_epoch" \
		'BEGIN { printf "%d", e + 15 - n - 2 }')
	if [ "$hold" -ge 1 ]; then
		assert_none_for "$hold" \
			"a fail-back inside cntlr_unhealthy (15 s) while sp2 C2 is settling" \
			reaction_ge $((sp2_failovers + 2)) failover
	else
		log "  NOTE: the polls above ran past err_epoch + 13 s, so the live" \
			"negative is skipped; the fail-back's record time is still" \
			"checked against err_epoch + 15 s in 12.5."
	fi

	log "  12.5: the release — the fail-back at cntlr_unhealthy, C1 settles again"
	c1_settles=$(settled_cnt 3 1)
	wait_until $((15 + WAIT_SHORT)) "sp2: the fail-back (reaction applied kind=failover)" \
		reaction_ge $((sp2_failovers + 2)) failover
	back_at=$(reaction_epochs failover | sed -n "$((sp2_failovers + 2))p")
	[ -n "$back_at" ] || die "no fail-back record to time"
	ts_ge "$back_at" $((c2_epoch + 15)) ||
		die "the sp2 fail-back ran at $back_at, before C2's err_epoch $c2_epoch + the 15s cntlr_unhealthy a settling primary is held to"
	wait_until "$WAIT_SHORT" "sp2 C1 primary again" cntlr_is_primary sp2 1
	assert_eq "$(cntlr_field sp2 2 settling)" false \
		"sp2 C2 settling after the fail-back (model.Failover clears the demoted one)"
	wait_until "$WAIT_SHORT" "sp2 C2 err_epoch cleared (the planted row is inert on a standby)" \
		cntlr_epoch_clear sp2 2
	wait_until "$WAIT_SHORT" "sp2: cntlr settled for C1 after the fail-back" \
		settled_ge 3 1 $((c1_settles + 1))
	assert_eq "$(cntlr_field sp2 1 settling)" false \
		"sp2 C1 settling after its second settle"
	assert_none_for 3 "a third sp2 failover once both cntlrs are clean" \
		reaction_ge $((sp2_failovers + 3)) failover

	log "  12.6: clean up"
	clear_behavior cn1
}

reaction_skip_ge() { # <n> <kind> <reason>
	count_ge "$1" reaction_skip_cnt "$2" "$3"
}

# g2_repair_progressed reports whether ANY further AR8 step ran on sp0's data
# group G2 — another switch, another allocation, or another parked leg. The
# "no further repair of L3" of case D step 7 is all three.
g2_repair_progressed() { # <spare_switch cnt> <spare_create cnt> <spare cnt>
	if count_gt "$1" reaction_cnt spare_switch; then return 0; fi
	if count_gt "$2" reaction_cnt spare_create; then return 0; fi
	if [ "$(spare_count sp0 1 data 2)" != "$3" ]; then return 0; fi
	return 1
}

side_provisioned() { # <sp> <slice> <side id>
	local got
	got=$(slice_json "$1" "$2" | "$JQ" -r --arg s "$3" '
		[ (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?
		  | select((.side_id | tostring) == $s) | .provisioned ] | first // false') || return 1
	[ "$got" = true ]
}

leg_in_list() { # <sp> <slice> <meta|data> <grp id> <leg id>
	active_legs "$1" "$2" "$3" "$4" | awk -v l="$5" '$1 == l { found = 1 }
		END { exit !found }'
}

# ---------------------------------------------------------------------------
# Case H — failover (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------
#
# Failovers at the shipped default timers, watched through the discovery
# records and the fakes' request records. The CdcEntry lists a cntlr while it
# is enabled and either is the primary or has a zero err_epoch
# (architecture.md [D18]); a fan-out that demotes a primary hands that cntlr
# its demotion at once and holds the sides and the other cntlrs until the
# demotion is applied or DemotionHoldTimeout passes (dnv-worker.md RW22); a
# threshold reaction fires only on an object this coordinator itself last
# judged unhealthy (dnv-worker.md AR10).

# cntlr_key and sp_rev_key spell two keys for mod_rev (architecture.md, Key
# grammar): ids in common.IdKeyFmt, which is how $CID is already spelled, and
# every SP of this suite on shard 00.
cntlr_key() { # <sp id> <cntlr id>
	printf '%s cntlr %s %016x %016x' "$DNV_PREFIX" "$CID" "$1" "$2"
}

sp_rev_key() { # <sp id>
	printf '%s sp_rev 00 %s %016x' "$DNV_PREFIX" "$CID" "$1"
}

# mod_rev prints the etcd mod_revision of one key — the revision of the
# transaction that last wrote it — read with the shipped etcdctl, or fails
# when the read did or the key is absent.
mod_rev() { # <key>
	local out rev
	out=$(sshw "$WORK/bin/etcdctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"get $(printf '%q' "$1") -w json") || return 1
	rev=$(jq_of "$out" '.kvs[0].mod_revision // empty') || return 1
	case "$rev" in
	'' | *[!0-9]*) return 1 ;;
	esac
	printf '%s' "$rev"
}

# same_txn asserts that two keys were last written by one transaction: they
# carry one mod_revision.
same_txn() { # <key> <key> <label>
	local first second
	first=$(mod_rev "$1") || die "$3: the mod_revision of '$1' could not be read"
	second=$(mod_rev "$2") || die "$3: the mod_revision of '$2' could not be read"
	assert_eq "$second" "$first" "$3: the mod_revision of '$2' against '$1'"
}

# cdc_ports prints a CdcEntry's transports as its sorted, comma-joined
# tr_svc_id list — each a CN's own port (cn_port) — or fails when the read
# did; cn_ports spells a set of CN ids the same way.
cdc_ports() { # <cdc key>
	local out
	out=$(ctl get --key "$1") || return 1
	jq_of "$out" '[.nvme_tr_conf_list[]?.tr_svc_id] | sort | join(",")'
}

cn_ports() { # <cn id…>
	local id ports=()
	for id in "$@"; do ports+=("$(cn_port "$id")"); done
	printf '%s\n' "${ports[@]}" | sort | tr '\n' ',' | sed 's/,$//'
}

# cdc_is is the wait_until predicate "the CdcEntry lists exactly these CNs";
# a read that failed keeps the poll running.
cdc_is() { # <cdc key> <cn id…>
	local key=$1 got
	shift
	got=$(cdc_ports "$key") || return 1
	[ "$got" = "$(cn_ports "$@")" ]
}

# cdc_lacks_or_die is the predicate of a negative: whether the CdcEntry lacks
# one CN. It dies on a failed read, as td_created_or_die does: read as an
# empty list, a dropped ssh would decide the negative.
cdc_lacks_or_die() { # <cdc key> <cn id>
	local got
	got=$(cdc_ports "$1") ||
		die "the CdcEntry could not be read: a read that failed lists nothing"
	case ",$got," in
	*",$(cn_port "$2"),"*) return 1 ;;
	esac
	return 0
}

# req_first prints "<epoch> <trace_id>" of the FIRST request one fake received
# for a method whose `.data` matches a filter, or fails while there is none.
# The server interceptor logs a unary request on arrival, before the fake's
# handler runs, so a request the fake then holds (hang_syncup) is timed when
# it arrived.
req_first() { # <agent> <method> <filter over .data> [jq args…]
	local agent=$1 method=$2 filter=$3 line
	shift 3
	line=$(recsr "$(apath "$agent")" \
		'select(.msg == "grpc server request")
		 | select((.method | split("/") | last) == $m)
		 | select(has("data")) | select(.data | '"$filter"')
		 | "\(.time) \(.trace_id)"' --arg m "$method" "$@" | sed -n 1p) ||
		return 1
	[ -n "$line" ] || return 1
	printf '%s %s\n' "$(ts_epoch "${line%% *}")" "${line##* }"
}

# reply_for prints "<epoch> <code>" of the reply one fake logged to the
# request of one trace id — a unary request and its reply share it — the
# code being the reply's agent_reply.code, or fails while there is none: a
# held request has no reply yet, and one that ended on its caller's deadline
# logged an error instead of a message.
reply_for() { # <agent> <method> <trace_id>
	local line
	line=$(recsr "$(apath "$1")" \
		'select(.msg == "grpc server reply")
		 | select((.method | split("/") | last) == $m)
		 | select(.trace_id == $t) | select(has("data"))
		 | "\(.time) \(.data.agent_reply.code // 0)"' \
		--arg m "$2" --arg t "$3" | sed -n 1p) || return 1
	[ -n "$line" ] || return 1
	printf '%s %s\n' "$(ts_epoch "${line%% *}")" "${line##* }"
}

# replied_before asserts that one fake answered a request, with code 0, no
# later than a given epoch: the barrier between two requests, read from the
# records of the fakes that received them.
replied_before() { # <agent> <method> <"epoch trace_id" of the request> <epoch> <label>
	local reply
	reply=$(reply_for "$1" "$2" "${3##* }") ||
		die "$5: $1 has no reply to the $2 of trace ${3##* }"
	assert_eq "${reply##* }" 0 "$5: the code of $1's reply"
	ts_ge "$4" "${reply%% *}" ||
		die "$5: $1 replied at ${reply%% *}, after $4"
}

# epoch_delta and epoch_min compute on the epochs req_first and reply_for
# print. printf keeps the fraction, which awk's print would round to six
# significant digits.
epoch_delta() { # <later> <earlier>
	awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", a - b }'
}

epoch_min() { # <epoch> <epoch>
	awk -v a="$1" -v b="$2" 'BEGIN { printf "%.6f", (a < b ? a : b) }'
}

# reaction_revision prints the `revision` of the n-th `reaction applied`
# record of one kind, over every worker log of the case in time order: the
# SpRev the coordinator read back right after the reaction's STM. It fails
# when there is no n-th record, and when any one log could not be read, as
# wcount does: a log left out of the merge would move the n-th record.
reaction_revision() { # <kind> <n from 1>
	local w t r out all= line
	for w in "${SEEN_WORKERS[@]}"; do
		out=$(recsr "$(wpath "$w")" \
			'select(.msg == "reaction applied") | select(.kind == $k)
			 | "\(.time) \(.revision)"' --arg k "$1") || return 1
		all+="$out"$'\n'
	done
	line=$(printf '%s' "$all" | while read -r t r; do
		if [ -n "$t" ]; then printf '%s %s\n' "$(ts_epoch "$t")" "$r"; fi
	done | sort -n | sed -n "${2}p") || return 1
	[ -n "$line" ] || return 1
	printf '%s\n' "${line##* }"
}

# want_number dies unless a revision or an err_epoch read back is a plain
# non-zero number: a wait for "set" is met by a failed read too, and a 0
# would put every compare against it in 1970 (case D step 2).
want_number() { # <value> <label>
	case "$1" in
	'' | 0 | *[!0-9]*) die "$2 read back as '$1'" ;;
	esac
}

reaction_any_cnt() { wcount 'select(.msg == "reaction applied")'; }

# cntlr_health_cnt counts the `health changed` records (dnv-worker.md, Log
# records) of one cntlr for one reason, over every worker log of the case
# or, given a worker, over that worker's log alone.
cntlr_health_cnt() { # <sp id> <cntlr id> <reason> [worker]
	local filter='select(.msg == "health changed" and .record == "cntlr")
		| select((.sp_id | tostring) == $sp and (.cntlr_id | tostring) == $c
			and .reason == $rea)'
	if [ $# -ge 4 ]; then
		count_recs "$(wpath "$4")" "$filter" \
			--arg sp "$1" --arg c "$2" --arg rea "$3"
	else
		wcount "$filter" --arg sp "$1" --arg c "$2" --arg rea "$3"
	fi
}

# demotion_unsynced_cnt counts the `sp demotion unsynced` records of one SP
# at one revision (dnv-worker.md RW22: the demotion hold ended by its wait)
# and, given a list, only those whose cntlr_ids — the demoted cntlrs that had
# not reported — are exactly that comma-joined list.
demotion_unsynced_cnt() { # <sp id> <revision> [cntlr ids]
	wcount 'select(.msg == "sp demotion unsynced")
		| select((.sp_id | tostring) == $sp and (.revision | tostring) == $r)
		| select($ids == "*"
			or ((.cntlr_ids // []) | map(tostring) | join(",")) == $ids)' \
		--arg sp "$1" --arg r "$2" --arg ids "${3-*}"
}

demotion_unsynced_all() { wcount 'select(.msg == "sp demotion unsynced")'; }

# stored_interval prints one health_check_conf member of the case's
# ClusterConf as workerctl stored it.
stored_interval() { # <member>
	ctl get --key "$DNV_PREFIX cluster_conf $CLUSTER" |
		"$JQ" -r --arg f "$1" '.health_check_conf[$f]'
}

case_failover() {
	CASE=failover
	local hc=$DEFAULT_HC_INTERVAL pt=$DEFAULT_PRIMARY_UNHEALTHY
	local hold=$DEMOTION_HOLD
	local at_rev='(.revision | tostring) == $r'
	local promotes="$at_rev and (.cntlr.primary // false)"
	local demotes="$at_rev and ((.cntlr.primary // false) | not)"
	local i member

	stage 1 "the fixture at the default timers: sp0 with three cntlrs, td0 and ss0"
	new_cluster failover --dn-interval 0 --cn-interval 0 \
		--side-interval 0 --cntlr-interval 0
	for member in dn_interval cn_interval side_interval cntlr_interval; do
		assert_eq "$(stored_interval "$member")" "$hc" \
			"health_check_conf.$member stored for a 0 (the default)"
	done
	# Stage 6 rests on it: one missed round followed by a prompt clean round
	# cannot reach the default primary threshold (architecture.md, Common
	# validation; dnv-worker.md, Known limits).
	assert_ge "$pt" $((2 * hc)) \
		"the default primary threshold against two default check intervals"
	put_dn 1 8
	put_dn 2 8
	for i in 1 2 3; do put_cn "$i" 64; done
	# No --thresholds: the pool reads every default. The cntlr, side and leg
	# ones are minutes long, so nothing replaces a cntlr or repairs a leg
	# inside the case: only failovers move a role.
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1,2 --level 0 \
		--lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false --cntlr 3:3:2:false \
		--slice 1:0 \
		--group 1:1:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 \
		--side 1:1:1:0 --side 2:2:2:0
	assert_field "$(ctl get-sp --sp sp0)" \
		'.sp_conf.event_threshold.primary_unhealthy | tostring' 0 \
		"sp0's stored primary threshold (0: the default applies)"
	ctl put-td --sp sp0 --name td0 --id "$TD_ID" --size 10737418240
	local ss_out cdc_key
	ss_out=$(ctl put-ss --sp sp0 --nqn "$NQN_PREFIX:ss0" --id "$SS_ID" \
		--ns "$NS_ID:1:$TD_ID")
	cdc_key=$(jq_of "$ss_out" .cdc_key)
	case "$cdc_key" in
	"$DNV_PREFIX cdc "*) ;;
	*) die "put-ss printed no CdcEntry key: $ss_out" ;;
	esac
	wait_until "$WAIT_SYNCUP" "every side of slice 1 provisioned" \
		all_provisioned sp0 1
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"thin_ok": true}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "td0 created true" td_created sp0 td0
	# A settling primary is held to the cntlr threshold (AR5); every
	# threshold this case times is a settled primary's.
	wait_until $((2 * hc + WAIT_SHORT)) "C1 settled (settling false)" \
		cntlr_settled sp0 1
	assert_eq "$(cdc_ports "$cdc_key")" "$(cn_ports 1 2 3)" \
		"the CdcEntry lists the primary and both clean standbys"
	mod_rev "$(sp_rev_key 1)" >/dev/null ||
		die "no SpRev key at '$(sp_rev_key 1)': the spelling mod_rev compares by"

	stage 2 "a hung standby leaves the CdcEntry and comes back, with no bump and no reaction"
	# The listing rule rides the health write: the STM that sets or clears
	# a standby's err_epoch rewrites the entry, so the two keys carry one
	# mod_revision, and no health write bumps SpRev (HL3).
	local sprev reactions c2_unreach c2_recov
	sprev=$(sp_rev 1)
	reactions=$(reaction_any_cnt)
	c2_unreach=$(cntlr_health_cnt 1 2 unreachable)
	c2_recov=$(cntlr_health_cnt 1 2 recovered)
	set_behavior cn1 <<'EOF'
{"objects": {"cntlr 1:2": {"hang": true}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "C2 err_epoch set by its missed round" \
		cntlr_epoch_set sp0 2
	same_txn "$(cntlr_key 1 2)" "$cdc_key" "C2 turned unhealthy"
	assert_eq "$(cdc_ports "$cdc_key")" "$(cn_ports 1 3)" \
		"the CdcEntry while C2 is unhealthy"
	assert_ge "$(cntlr_health_cnt 1 2 unreachable)" $((c2_unreach + 1)) \
		"health changed record=cntlr reason=unreachable for C2"
	clear_behavior cn1
	wait_until $((2 * hc + WAIT_SHORT)) "C2 err_epoch cleared" \
		cntlr_epoch_clear sp0 2
	same_txn "$(cntlr_key 1 2)" "$cdc_key" "C2 clean again"
	assert_eq "$(cdc_ports "$cdc_key")" "$(cn_ports 1 2 3)" \
		"the CdcEntry once C2 is clean again"
	assert_ge "$(cntlr_health_cnt 1 2 recovered)" $((c2_recov + 1)) \
		"health changed record=cntlr reason=recovered for C2"
	assert_eq "$(sp_rev 1)" "$sprev" "SpRev across the standby's round trip"
	assert_eq "$(reaction_any_cnt)" "$reactions" \
		"reactions while a standby was unhealthy"

	stage 3 "an unhealthy primary with no candidate keeps its record (AR5)"
	# Both standbys go first, each its own way. C3 reports an ERROR row,
	# which every reply of it carries — its Check rounds and the syncup the
	# failover of stage 4 sends it alike — so it stays unhealthy and unlisted
	# through that failover; a hung standby would answer the syncup and read
	# clean for a round.
	set_behavior cn1 <<'EOF'
{"objects": {"cntlr 1:2": {"hang": true}}}
EOF
	set_behavior cn2 <<'EOF'
{"objects": {"cntlr 1:3": {"rows": {"slice_id_to_dm_pool.1":
  {"status": "ERROR", "details": "pool failed"}}}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "C2 err_epoch set" cntlr_epoch_set sp0 2
	wait_until $((2 * hc + WAIT_SHORT)) "C3 err_epoch set" cntlr_epoch_set sp0 3
	wait_until "$WAIT_SHORT" "the CdcEntry to list the primary alone" \
		cdc_is "$cdc_key" 1
	local skips failovers cdc_rev
	skips=$(reaction_skip_cnt failover "no candidate")
	failovers=$(reaction_cnt failover)
	cdc_rev=$(mod_rev "$cdc_key") ||
		die "the CdcEntry's mod_revision could not be read"
	# C1 stalls the way a stopped cn agent does: neither its Check rounds
	# nor its syncups are answered.
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"hang": true, "hang_syncup": true}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "C1 err_epoch set by its missed round" \
		cntlr_epoch_set sp0 1
	wait_until $((pt + 2 * hc + WAIT_SHORT)) \
		"reaction skipped kind=failover reason=no candidate" \
		reaction_skip_ge $((skips + 1)) failover "no candidate"
	# A serving primary is never taken away from the hosts: listed while it
	# is the primary, whatever its err_epoch — and the entry was not even
	# rewritten for a moment, since no transaction has touched it.
	assert_none_for $((pt + hc)) "the unhealthy primary leaving the CdcEntry" \
		cdc_lacks_or_die "$cdc_key" 1
	assert_eq "$(mod_rev "$cdc_key")" "$cdc_rev" \
		"the CdcEntry's mod_revision while the primary is unhealthy"
	assert_eq "$(reaction_cnt failover)" "$failovers" \
		"failovers with no candidate"
	assert_eq "$(cntlr_field sp0 1 primary)" true \
		"C1 primary with no candidate"

	stage 4 "a hung primary fails over: its demotion, the sides a DemotionHoldTimeout later, then the promotion"
	# C2 comes back clean, a candidate again, and the next pass fails C1
	# over (AR5): C1's missed rounds are this coordinator's own verdict
	# (dnv-worker.md AR10).
	local unsynced rev
	unsynced=$(demotion_unsynced_all)
	clear_behavior cn1
	wait_until $((2 * hc + WAIT_SHORT)) "C2 err_epoch cleared" \
		cntlr_epoch_clear sp0 2
	wait_until $((2 * hc + WAIT_SHORT)) "reaction applied kind=failover" \
		reaction_ge $((failovers + 1)) failover
	rev=$(reaction_revision failover $((failovers + 1))) ||
		die "failover $((failovers + 1)) of the case has no revision:" \
			"a worker log could not be read, or it holds no such record"
	want_number "$rev" "the failover record's revision"
	log "  the failover's STM bumped SpRev to $rev"
	assert_eq "$(sp_rev 1)" "$rev" "SpRev right after the failover"
	# The failover's own STM took C1 out of the entry (architecture.md
	# [D18]): a demoted cntlr with an err_epoch is a standby the rule does
	# not list, and C1, stalled, has written nothing since its missed round.
	same_txn "$(sp_rev_key 1)" "$cdc_key" "the failover"
	assert_eq "$(cdc_ports "$cdc_key")" "$(cn_ports 2)" \
		"the CdcEntry after the failover (C1 demoted and unhealthy, C3 unhealthy)"
	assert_eq "$(cntlr_field sp0 1 primary)" false \
		"C1 primary after the failover"
	assert_eq "$(cntlr_field sp0 2 primary)" true \
		"C2 primary after the failover"
	# Read, not cntlr_epoch_set: that predicate takes a failed read for "set".
	local stalled_epoch
	stalled_epoch=$(cntlr_field sp0 1 err_epoch)
	want_number "$stalled_epoch" "C1's err_epoch while it stalls"

	log "  4.1: the order, read from the fakes' request records"
	wait_until $((hold + 2 * hc + WAIT_SHORT)) \
		"cn1: the SyncupCntlr promoting C2 at revision $rev" \
		req_ge 1 cn1 SyncupCntlr "$promotes" --arg r "$rev"
	wait_until "$WAIT_SHORT" "cn2: a SyncupCntlr at revision $rev" \
		req_ge 1 cn2 SyncupCntlr "$at_rev" --arg r "$rev"
	local dem pro c3 side0 side1 first_side gap
	dem=$(req_first cn0 SyncupCntlr "$demotes" --arg r "$rev") ||
		die "cn0 received no demotion of C1 at revision $rev"
	side0=$(req_first dn0 SyncupSide \
		"$at_rev and (.side_conf.primary_cn_id | tostring) == \"2\"" \
		--arg r "$rev") ||
		die "dn0 received no SyncupSide naming cn 2 primary at revision $rev"
	side1=$(req_first dn1 SyncupSide \
		"$at_rev and (.side_conf.primary_cn_id | tostring) == \"2\"" \
		--arg r "$rev") ||
		die "dn1 received no SyncupSide naming cn 2 primary at revision $rev"
	pro=$(req_first cn1 SyncupCntlr "$promotes" --arg r "$rev") ||
		die "cn1 received no promotion of C2 at revision $rev"
	c3=$(req_first cn2 SyncupCntlr "$at_rev" --arg r "$rev") ||
		die "cn2 received no SyncupCntlr at revision $rev"
	first_side=$(epoch_min "${side0%% *}" "${side1%% *}")
	gap=$(epoch_delta "$first_side" "${dem%% *}")
	log "  C1's demotion at ${dem%% *}, the first side $gap s later"
	# Nobody answers the demotion, so RW22's demotion hold keeps the sides back
	# for its whole wait: not less — its timer is armed a moment before the
	# demotion leaves, and RW14's interval-long bound is shorter — and not the
	# syncup deadline (RW5) the demotion call itself waits out; the upper bound
	# allows a busy coordinator.
	awk -v g="$gap" -v h="$hold" 'BEGIN { exit !(g >= h - 0.3 && g <= h + 1.5) }' ||
		die "the sides were told the failover $gap s after C1's demotion," \
			"want DemotionHoldTimeout ($hold s)"
	assert_eq "$(demotion_unsynced_cnt 1 "$rev" 1)" 1 \
		"sp demotion unsynced records at revision $rev naming C1"
	assert_eq "$(demotion_unsynced_all)" $((unsynced + 1)) \
		"sp demotion unsynced records of the stage"
	# RW14 then holds the cntlrs until both sides answered the revision.
	replied_before dn0 SyncupSide "$side0" "${pro%% *}" \
		"side 1 before C2's promotion"
	replied_before dn1 SyncupSide "$side1" "${pro%% *}" \
		"side 2 before C2's promotion"
	replied_before dn0 SyncupSide "$side0" "${c3%% *}" \
		"side 1 before C3's syncup"
	replied_before dn1 SyncupSide "$side1" "${c3%% *}" \
		"side 2 before C3's syncup"

	log "  4.2: C1 resumes: it applies the held demotion and is listed again as a clean standby"
	local c1_recov reply
	c1_recov=$(cntlr_health_cnt 1 1 recovered)
	clear_behavior cn0
	wait_until $((2 * hc + WAIT_SHORT)) "C1 err_epoch cleared" \
		cntlr_epoch_clear sp0 1
	reply=$(reply_for cn0 SyncupCntlr "${dem##* }") ||
		die "cn0: the held demotion of C1 was never answered"
	assert_eq "${reply##* }" 0 "cn0: the code of the reply to the held demotion"
	assert_ge "$(cntlr_health_cnt 1 1 recovered)" $((c1_recov + 1)) \
		"health changed record=cntlr reason=recovered for C1"
	wait_until "$WAIT_SHORT" "the CdcEntry to list C1 and C2" \
		cdc_is "$cdc_key" 1 2
	clear_behavior cn2
	wait_until $((2 * hc + WAIT_SHORT)) "C3 err_epoch cleared" \
		cntlr_epoch_clear sp0 3
	wait_until "$WAIT_SHORT" "the CdcEntry to list all three cntlrs" \
		cdc_is "$cdc_key" 1 2 3

	stage 5 "a reachable primary's demotion releases its sides at once (dnv-worker.md RW22)"
	# The disabled trigger fails C2 over at the next pass, with no
	# threshold (AR5). The disable is workerctl's raw write, which leaves the
	# CdcEntry as it is; the failover's STM unlists the disabled cntlr.
	local dis_rev
	failovers=$(reaction_cnt failover)
	unsynced=$(demotion_unsynced_all)
	dis_rev=$(ctl set-cntlr --sp sp0 --id 2 --disabled=true | "$JQ" -r .sp_rev)
	want_number "$dis_rev" "the disable's SpRev"
	wait_until $((2 * hc + WAIT_SHORT)) \
		"reaction applied kind=failover (C2 disabled)" \
		reaction_ge $((failovers + 1)) failover
	rev=$(reaction_revision failover $((failovers + 1))) ||
		die "failover $((failovers + 1)) of the case has no revision:" \
			"a worker log could not be read, or it holds no such record"
	want_number "$rev" "the failover record's revision"
	[ "$rev" -gt "$dis_rev" ] ||
		die "the failover's revision $rev is not past the disable's $dis_rev"
	assert_eq "$(cntlr_field sp0 1 primary)" true \
		"C1, the lowest clean standby, primary after the failover"
	same_txn "$(sp_rev_key 1)" "$cdc_key" "the failover of the disabled primary"
	assert_eq "$(cdc_ports "$cdc_key")" "$(cn_ports 1 3)" \
		"the CdcEntry after the failover (C2 disabled)"
	wait_until $((2 * hc + WAIT_SHORT)) \
		"cn0: the SyncupCntlr promoting C1 at revision $rev" \
		req_ge 1 cn0 SyncupCntlr "$promotes" --arg r "$rev"
	dem=$(req_first cn1 SyncupCntlr "$demotes" --arg r "$rev") ||
		die "cn1 received no demotion of C2 at revision $rev"
	side0=$(req_first dn0 SyncupSide \
		"$at_rev and (.side_conf.primary_cn_id | tostring) == \"1\"" \
		--arg r "$rev") ||
		die "dn0 received no SyncupSide naming cn 1 primary at revision $rev"
	side1=$(req_first dn1 SyncupSide \
		"$at_rev and (.side_conf.primary_cn_id | tostring) == \"1\"" \
		--arg r "$rev") ||
		die "dn1 received no SyncupSide naming cn 1 primary at revision $rev"
	pro=$(req_first cn0 SyncupCntlr "$promotes" --arg r "$rev") ||
		die "cn0 received no promotion of C1 at revision $rev"
	first_side=$(epoch_min "${side0%% *}" "${side1%% *}")
	replied_before cn1 SyncupCntlr "$dem" "$first_side" \
		"C2's demotion answered before any side was told"
	gap=$(epoch_delta "$first_side" "${dem%% *}")
	log "  C2's demotion at ${dem%% *}, answered, the first side $gap s later"
	awk -v g="$gap" -v h="$hold" 'BEGIN { exit !(g < h / 2) }' ||
		die "the sides waited $gap s for an answered demotion," \
			"want well under DemotionHoldTimeout ($hold s)"
	replied_before dn0 SyncupSide "$side0" "${pro%% *}" \
		"side 1 before C1's promotion"
	replied_before dn1 SyncupSide "$side1" "${pro%% *}" \
		"side 2 before C1's promotion"
	assert_eq "$(demotion_unsynced_cnt 1 "$rev")" 0 \
		"sp demotion unsynced records at revision $rev"
	assert_eq "$(demotion_unsynced_all)" "$unsynced" \
		"sp demotion unsynced records of the stage"

	stage 6 "the primary threshold is two rounds: one missed round followed by a prompt answer never fails over, every round missed does (AR5)"
	wait_until $((2 * hc + WAIT_SHORT)) "C1 settled after its promotion" \
		cntlr_settled sp0 1
	log "  6.1: one missed round, the next one answered"
	local c1_unreach
	failovers=$(reaction_cnt failover)
	c1_unreach=$(cntlr_health_cnt 1 1 unreachable)
	c1_recov=$(cntlr_health_cnt 1 1 recovered)
	# hang_rounds holds exactly one round until the worker times it out, so
	# the miss does not depend on how fast this script polls.
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"hang_rounds": 1}}}
EOF
	wait_until $((3 * hc + WAIT_SHORT)) "C1 unreachable for the held round" \
		count_gt "$c1_unreach" cntlr_health_cnt 1 1 unreachable
	wait_until $((2 * hc + WAIT_SHORT)) "C1 recovered at the round after it" \
		count_gt "$c1_recov" cntlr_health_cnt 1 1 recovered
	# The err_epoch is whole seconds, and the next round goes one interval
	# after the missed one timed out (RW8): its clean verdict replaces the
	# unhealthy one in the memo, and clears the epoch, before now -
	# err_epoch can reach the threshold.
	assert_none_for $((2 * pt + 2 * hc)) "a failover on one missed round" \
		reaction_ge $((failovers + 1)) failover
	assert_eq "$(cntlr_health_cnt 1 1 unreachable)" $((c1_unreach + 1)) \
		"C1's unreachable records (one round held)"
	assert_eq "$(cntlr_field sp0 1 primary)" true \
		"C1 primary after one missed round"
	assert_eq "$(cntlr_field sp0 1 err_epoch)" 0 \
		"C1's err_epoch after one missed round"

	log "  6.2: every round missed"
	local c1_epoch failover_at
	set_behavior cn0 <<'EOF'
{"objects": {"cntlr 1:1": {"hang": true}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "C1 err_epoch set" cntlr_epoch_set sp0 1
	c1_epoch=$(cntlr_field sp0 1 err_epoch)
	want_number "$c1_epoch" "C1's err_epoch"
	wait_until $((pt + 2 * hc + WAIT_SHORT)) "reaction applied kind=failover" \
		reaction_ge $((failovers + 1)) failover
	# Timed against the epoch the worker stamped, as case D step 2 times
	# its failover: AR5 fires at the first pass once now - err_epoch reaches
	# the threshold, so within one interval of it.
	failover_at=$(reaction_epochs failover | sed -n "$((failovers + 1))p")
	[ -n "$failover_at" ] || die "no failover record to time"
	ts_ge "$failover_at" $((c1_epoch + pt)) ||
		die "the failover ran at $failover_at, before C1's err_epoch" \
			"$c1_epoch + the ${pt}s primary threshold"
	ts_lt "$failover_at" $((c1_epoch + pt + hc + 2)) ||
		die "the failover ran at $failover_at, later than one interval" \
			"past C1's err_epoch $c1_epoch + the ${pt}s primary threshold"
	wait_until "$WAIT_SHORT" "C3 primary true" cntlr_is_primary sp0 3
	clear_behavior cn0
	wait_until $((2 * hc + WAIT_SHORT)) "C1 err_epoch cleared" \
		cntlr_epoch_clear sp0 1
	wait_until "$WAIT_SHORT" "the CdcEntry to list C1 and C3" \
		cdc_is "$cdc_key" 1 3

	stage 7 "after a handoff a stale err_epoch fires nothing before the new owner's own verdict (dnv-worker.md AR10)"
	wait_until $((2 * hc + WAIT_SHORT)) "C3 settled after its promotion" \
		cntlr_settled sp0 3
	# The epoch is the current owner's own verdict, written while sp_level
	# suppresses every reaction (AR3).
	ctl set-level --sp sp0 --level 48
	set_behavior cn2 <<EOF
{"objects": {"cntlr 1:3": {"rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "C3 err_epoch set under suppression" \
		cntlr_epoch_set sp0 3
	local c3_epoch refused
	c3_epoch=$(cntlr_field sp0 3 err_epoch)
	want_number "$c3_epoch" "C3's err_epoch"
	# From here on C3 answers nothing anyone can judge: a rejected reply
	# neither sets nor clears (HL2), so the epoch outlives its writer.
	refused=$(replies cn2 SyncupCntlr '(.agent_reply.code // 0) == 2')
	set_behavior cn2 <<'EOF'
{"objects": {"cntlr 1:3": {"reply_code": 2}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "cn2: a SyncupCntlr refused with code 2" \
		reply_gt "$refused" cn2 SyncupCntlr '(.agent_reply.code // 0) == 2'
	local owner new_owner new8 lift level new_recov now_at
	reactions=$(reaction_any_cnt)
	owner=$(owner_of sp 00)
	[ -n "$owner" ] || die "no owner for (sp, 00)"
	log "  (sp, 00) is owned by $owner"
	sig_dir "$owner" KILL
	wait_gone "$owner" "$WAIT_SHORT"
	# Lifted while (sp, 00) has no owner: the next owner finds the SP
	# unsuppressed, C3's err_epoch past the threshold, C3 settled and C1 a
	# clean candidate — everything a pass reads but a verdict of its own.
	lift=$(ctl set-level --sp sp0 --level 0)
	level=$(jq_of "$lift" .sp_level)
	wait_until "$WAIT_MEMBERSHIP" "another worker to own (sp, 00)" \
		shard_owner_changed sp 00 "$owner"
	new_owner=$(owner_of sp 00)
	new8=$(seed8 "$new_owner")
	log "  (sp, 00) moved to $new_owner"
	wait_until "$WAIT_MEMBERSHIP" "cn2: CheckCntlr rounds from the new owner" \
		trace_req_ge 1 cn2 CheckCntlr "$new8"
	assert_none_for $((3 * hc + 2)) \
		"a reaction before the new owner judged C3 itself" \
		count_gt "$reactions" reaction_any_cnt
	# Not vacuous: every other input of AR5's threshold arm says fail over.
	assert_eq "$(cntlr_field sp0 3 err_epoch)" "$c3_epoch" \
		"C3's err_epoch across the handoff"
	assert_eq "$(cntlr_field sp0 3 primary)" true "C3 primary across the handoff"
	assert_eq "$(cntlr_field sp0 3 settling)" false "C3 settled across the handoff"
	assert_eq "$(cntlr_field sp0 1 err_epoch)" 0 "C1, the candidate, clean"
	assert_eq "$(cntlr_field sp0 1 disabled)" false "C1, the candidate, enabled"
	assert_field "$(ctl get-sp --sp sp0)" .sp_conf.sp_level "$level" \
		"sp0's sp_level after the lift"
	now_at=$(server_now)
	ts_ge "$(ts_epoch "$now_at")" $((c3_epoch + pt)) ||
		die "C3's err_epoch $c3_epoch is not past the threshold at $now_at"

	log "  7.2: C3 answers again: the new owner's first verdict clears it"
	new_recov=$(cntlr_health_cnt 1 3 recovered "$new_owner")
	clear_behavior cn2
	wait_until $((2 * hc + WAIT_SHORT)) "C3 err_epoch cleared" \
		cntlr_epoch_clear sp0 3
	assert_ge "$(cntlr_health_cnt 1 3 recovered "$new_owner")" \
		$((new_recov + 1)) \
		"health changed record=cntlr reason=recovered for C3 in $new_owner's log"
	assert_eq "$(reaction_any_cnt)" "$reactions" "reactions across the handoff"

	stage 8 "a verdict that writes nothing still counts (dnv-worker.md AR10, HL3)"
	# A planted err_epoch — what another observer would have written — is
	# no verdict of this coordinator's and fires nothing on its own, even
	# right after the coordinator judged C3 unhealthy and then clean: its
	# memo keeps each object's latest verdict alone. Its passes hand the
	# record to C3's monitor (HL3), so C3's next unhealthy reply is no
	# transition and writes nothing, and the failover follows all the same.
	# The age is past the primary threshold and short of the cntlr one.
	local c3_unreach c3_recov
	c3_unreach=$(cntlr_health_cnt 1 3 unreachable "$new_owner")
	c3_recov=$(cntlr_health_cnt 1 3 recovered "$new_owner")
	set_behavior cn2 <<'EOF'
{"objects": {"cntlr 1:3": {"hang_rounds": 1}}}
EOF
	wait_until $((3 * hc + WAIT_SHORT)) "C3 unreachable for one held round" \
		count_gt "$c3_unreach" cntlr_health_cnt 1 3 unreachable "$new_owner"
	wait_until $((2 * hc + WAIT_SHORT)) "C3 recovered at the round after it" \
		count_gt "$c3_recov" cntlr_health_cnt 1 3 recovered "$new_owner"
	refused=$(replies cn2 SyncupCntlr '(.agent_reply.code // 0) == 2')
	set_behavior cn2 <<'EOF'
{"objects": {"cntlr 1:3": {"reply_code": 2}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) "cn2: a SyncupCntlr refused with code 2" \
		reply_gt "$refused" cn2 SyncupCntlr '(.agent_reply.code // 0) == 2'
	local planted c3_rows
	failovers=$(reaction_cnt failover)
	planted=$(ctl set-epoch cntlr --sp sp0 --id 3 --age $((pt + 30)) |
		"$JQ" -r .err_epoch)
	want_number "$planted" "the planted err_epoch"
	assert_eq "$(cntlr_field sp0 3 err_epoch)" "$planted" \
		"C3's err_epoch after the plant"
	assert_none_for $((3 * hc + 1)) \
		"a failover on an err_epoch this coordinator did not judge" \
		reaction_ge $((failovers + 1)) failover
	c3_rows=$(cntlr_health_cnt 1 3 error_row)
	set_behavior cn2 <<EOF
{"objects": {"cntlr 1:3": {"rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	wait_until $((2 * hc + WAIT_SHORT)) \
		"reaction applied kind=failover on C3's own verdict" \
		reaction_ge $((failovers + 1)) failover
	assert_eq "$(cntlr_health_cnt 1 3 error_row)" "$c3_rows" \
		"C3's error_row records: its verdict wrote nothing"
	assert_eq "$(cntlr_field sp0 3 err_epoch)" "$planted" \
		"C3's err_epoch is still the planted one"
	wait_until "$WAIT_SHORT" "C1 primary true" cntlr_is_primary sp0 1
	clear_behavior cn2
	wait_until $((2 * hc + WAIT_SHORT)) "C3 err_epoch cleared" \
		cntlr_epoch_clear sp0 3
	wait_until "$WAIT_SHORT" "the CdcEntry to list C1 and C3" \
		cdc_is "$cdc_key" 1 3
	log "  leaving one worker killed; the next fleet restart returns to w1 w2 w3"
}

# ---------------------------------------------------------------------------
# Case E — vote (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

membership_committed() { # <w> <seed> <state> [role]
	local role=${4:-dn}
	count_ge 1 count_recs "$(wpath "$1")" \
		'select(.msg == "membership committed")
		 | select(.seed == $s and .state == $st and .role == $r)' \
		--arg s "$2" --arg st "$3" --arg r "$role"
}

membership_committed_all_roles() { # <w> <seed> <state>
	local role
	for role in dn cn sp; do
		membership_committed "$1" "$2" "$3" "$role" || return 1
	done
	return 0
}

membership_observed() { # <w> <seed> <state>
	count_ge 1 count_recs "$(wpath "$1")" \
		'select(.msg == "membership observed")
		 | select(.seed == $s and .state == $st)' \
		--arg s "$2" --arg st "$3"
}

# max_member_cnt_is checks that, for EVERY role, the largest member_cnt this
# worker ever committed equals <cnt> — i.e. its effective set reached that size.
# See the note at the call site on why a per-seed member_cnt is the wrong shape
# for a joiner's own log.
max_member_cnt_is() { # <w> <cnt>
	local role got
	for role in dn cn sp; do
		got=$(recs "$(wpath "$1")" \
			'select(.msg == "membership committed")
			 | select(.role == $r and .state == "member")
			 | .member_cnt' --arg r "$role" | sort -n | tail -1)
		[ "$got" = "$2" ] || return 1
	done
	return 0
}

member_cnt_seen() { # <w> <seed> <cnt>
	count_ge 1 count_recs "$(wpath "$1")" \
		'select(.msg == "membership committed")
		 | select(.seed == $s and .state == "member")
		 | select((.member_cnt | tostring) == $c)' \
		--arg s "$2" --arg c "$3"
}

# worker_seeds prints the seeds `list-workers` reports and FAILS if the read
# itself failed — "no seed is listed" and "the read did not happen" are
# different answers and only the first is evidence.
worker_seeds() { ctl list-workers | "$JQ" -r .seed; }

# worker_not_listed is VW6's delete: the seed's registrations are gone. It runs
# the read itself rather than negating a `worker_listed` that also returns
# non-zero when the read fails — that negation turned every transient ssh
# failure into a pass.
worker_not_listed() { # <seed>
	local seeds
	seeds=$(worker_seeds) || return 1
	[ "$(printf '%s\n' "$seeds" | grep -cx "$1" || true)" = 0 ]
}

# ---------------------------------------------------------------------------
# Case G — drain (dnv-worker.md, Integration test plan, Cases; The sp drain):
# the sp coordinator tears a LATCHED storage pool down in bounded steps
# ---------------------------------------------------------------------------
#
# The gateway suite owns the LATCH — that `delete-sp` writes `deleting = true`,
# bumps once and returns — and stands the drain in with `wctl drain-sp`, because
# it runs no dnv-worker. This case is the other half: the REAL sp coordinator,
# reacting to the flag on its own ticker, running SPD8's derivation to the end.

# key_cnt counts the keys under one prefix, and FAILS LOUDLY when the read
# itself failed. `grep -c .` prints 0 on empty stdin either way, so a swallowed
# failure would make "the drain finished" and every post-drain count assertion
# pass on a dead etcd or a dropped ssh — the exact trap `rlog` and
# `dn_capacity_gone` already carry comments about. The read is therefore done
# first, on its own, and its status is checked before anything is counted.
key_cnt() { # <prefix>
	local out
	out=$(ctl list-keys --prefix "$1") || return 1
	printf '%s' "$out" | grep -c . || true
}

# drain_finished is the `wait_until` predicate for "the SP is gone". A failed
# read returns non-zero from key_cnt and leaves the poll running, rather than
# satisfying it.
drain_finished() {
	local cnt
	cnt=$(key_cnt sp_conf) || return 1
	[ "$cnt" = 0 ]
}

# drained_records counts the `sp drained` records across every worker that
# has run in this case (dnv-worker.md, Log records): the drain may finish
# under a different owner than the one that started it, and either way
# exactly one final STM commits.
drained_records() { wcount 'select(.msg == "sp drained")'; }
drain_step_records() { # <phase>
	wcount 'select(.msg == "sp drain step") | select(.phase == $p)' --arg p "$1"
}
drain_failed_records() { wcount 'select(.msg == "sp drain failed")'; }
clone_drain_step_records() { wcount 'select(.msg == "clone drain step")'; }

# clone_gone is the wait_until predicate of the clone drain, with key_cnt's
# fail-loudly contract: a failed read leaves the poll running.
clone_gone() {
	local cnt
	cnt=$(key_cnt clone) || return 1
	[ "$cnt" = 0 ]
}

# drain_put_sp plants the case fixture: one SP with TWO slices, each a meta and
# a data group, every leg on one of the two DNs. Two slices is the smallest
# shape that proves a batch never spans slices (SPD10) — the drain must take one
# D2 step per slice.
drain_put_sp() { # <name> <sp id>
	ctl put-sp --name "$1" --id "$2" --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true --cntlr 2:2:1:false \
		--slice 1:0 --slice 2:1 \
		--group 1:1:meta:1:raid1 --group 1:2:data:2:raid1 \
		--group 2:3:meta:1:raid1 --group 2:4:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 --leg 2:3:0 --leg 2:4:1 \
		--leg 3:5:0 --leg 3:6:1 --leg 4:7:0 --leg 4:8:1 \
		--side 1:1:1:0 --side 2:2:2:0 --side 3:3:1:0 --side 4:4:2:0 \
		--side 5:5:1:0 --side 6:6:2:0 --side 7:7:1:0 --side 8:8:2:0
}

# drain_assert_restored is the leak check: every extent the SP charged is back,
# every pointer list is empty, every node carries exactly one capacity key at
# its full free count, and the cluster's SpGlobal bucket is empty again while
# next_id never rewinds (GW12).
drain_assert_restored() { # <free-per-dn> <free-per-cn> <next-id>
	local free_dn=$1 free_cn=$2 next=$3 i
	for i in 1 2; do
		assert_eq "$(dn_free "$i")" "$free_dn" "dn $i free_ext_cnt restored"
		assert_eq "$(ctl get-dn --id "$i" | "$JQ" -r \
			'(.side_ptr_list // []) | length')" "0" \
			"dn $i side_ptr_list emptied"
		assert_eq "$(cn_free "$i")" "$free_cn" "cn $i free_ext_cnt restored"
		assert_eq "$(ctl get-cn --id "$i" | "$JQ" -r \
			'(.cntlr_ptr_list // []) | length')" "0" \
			"cn $i cntlr_ptr_list emptied"
	done
	assert_eq "$(key_cnt dn_capacity)" "2" "one dn_capacity key per DN"
	assert_eq "$(key_cnt cn_capacity)" "2" "one cn_capacity key per CN"
	local global
	# $CID is already the %016x form put-cluster emitted (idHex), not a
	# decimal: the gateway suite's CID is the other one, and printf-ing this
	# one is "invalid number".
	global=$(ctl get --key "$DNV_PREFIX sp_global $CID")
	assert_eq "$("$JQ" -r '(.shard_bucket // []) | add // 0' <<<"$global")" "0" \
		"SpGlobal Σ shard_bucket after the drain"
	assert_field "$global" '.next_id' "$next" "SpGlobal next_id never rewinds"
}

case_drain() {
	CASE=drain

	stage 1 "the drain fixture: two DNs, two CNs, sp0 with two slices"
	new_cluster drain
	local i
	# 64 free extents per node, not the 8 the other cases use: step 4 plants a
	# slice wider than one drain batch, which needs 21 extents per DN.
	for i in 1 2; do put_dn "$i" 64; done
	for i in 1 2; do put_cn "$i" 64; done
	drain_put_sp sp0 1
	# Each DN carries four sides of 1+2+1+2 = 6 extents; each CN reserves the
	# SP's whole footprint, also 6 (architecture.md, Per-operation allocation).
	assert_eq "$(dn_free 1)" "58" "dn 1 free after the fixture"
	assert_eq "$(cn_free 1)" "58" "cn 1 free after the fixture"
	wait_until "$WAIT_SYNCUP" "dn0: SyncupSide for side 1" \
		req_ge 1 dn0 SyncupSide '(.side_pointer.side_id | tostring) == "1"'
	wait_until "$WAIT_SYNCUP" "cn0: SyncupCntlr for cntlr 1" \
		req_ge 1 cn0 SyncupCntlr '(.cntlr_pointer.cntlr_id | tostring) == "1"'

	stage 2 "the latch, and the coordinator drains sp0 to nothing"
	local latched
	latched=$(ctl set-deleting --sp sp0)
	assert_field "$latched" '.latched' "true" "set-deleting latched sp0"
	# AR3 splits (SPD6): a latched SP runs the drain instead of nothing at
	# all, and its own SpRev bumps carry it from step to step with no timer.
	wait_until "$WAIT_SYNCUP" "sp0 to drain away" drain_finished
	assert_eq "$(key_cnt sp_rev)" "0" "sp_rev keys after the drain"
	assert_eq "$(key_cnt sp_id_to_name)" "0" "sp_id_to_name keys after the drain"
	assert_eq "$(key_cnt cntlr)" "0" "cntlr keys after the drain"
	assert_eq "$(key_cnt slice)" "0" "slice keys after the drain"
	drain_assert_restored 64 64 2
	# The phases the records must show: D1 once for every cntlr at once,
	# one D2 batch per SLICE — never one spanning both — and exactly one
	# final STM.
	assert_eq "$(drain_step_records cntlrs)" "1" "D1 ran exactly once"
	assert_eq "$(drain_step_records slice)" "2" \
		"one D2 batch per slice (a batch never spans slices)"
	assert_eq "$(drained_records)" "1" "exactly one \`sp drained\` record"
	assert_eq "$(drain_failed_records)" "0" "no drain step failed"

	stage 3 "a PARTIALLY drained SP resumes after a worker restart"
	# The window a real restart has to hit is sub-second, so it is built
	# instead of raced: with no worker running, sp1 is latched and advanced by
	# exactly ONE step (D1). What the restarted fleet then finds is the state
	# SPD8 has to resume from — latched, no cntlrs, every slice still there —
	# and nothing but the SpConf tells it where it is.
	local w
	for w in w1 w2 w3; do stop_worker "$w"; done
	drain_put_sp sp1 2
	ctl set-deleting --sp sp1 >/dev/null
	local partial
	partial=$(ctl drain-sp --sp sp1 --max-steps 1)
	assert_field "$partial" '.sp_deleted' "false" \
		"one step must NOT finish a two-slice drain"
	assert_eq "$(jq_of "$partial" '.cntlr_cnt')" "2" "the one step was D1"
	assert_eq "$(key_cnt cntlr)" "0" "cntlr keys after the partial drain"
	assert_eq "$(key_cnt slice)" "2" "slice keys survive the partial drain"
	assert_eq "$(key_cnt sp_conf)" "1" "sp_conf survives the partial drain"
	# D1 credited the CNs and nothing else, which is what makes the resume
	# observable: the DNs are still charged.
	assert_eq "$(cn_free 1)" "64" "cn 1 credited by the partial drain's D1"
	assert_eq "$(dn_free 1)" "58" "dn 1 still charged after D1"
	for w in w1 w2 w3; do start_worker "$w"; done
	for w in w1 w2 w3; do wait_registered "$w"; done
	log "  waiting $((VOTE_GRACE + 2))s for the grace window (VW7)"
	sleep $((VOTE_GRACE + 2))
	assert_owners 50 w1 w2 w3
	wait_until "$WAIT_SYNCUP" "sp1 to finish draining after the restart" \
		drain_finished
	assert_eq "$(key_cnt slice)" "0" "slice keys after the resumed drain"
	drain_assert_restored 64 64 3
	# The restarted fleet re-derived the position from the SpConf alone: it
	# ran no D1 of its own, because D1 had already committed.
	assert_eq "$(drain_step_records cntlrs)" "1" \
		"the resumed drain must not re-run D1"
	assert_eq "$(drained_records)" "2" "sp1 committed its own final STM"
	assert_eq "$(drain_failed_records)" "0" "no drain step failed"

	stage 4 "a slice WIDER than one batch drains in two, data groups first"
	# SPD10's batch bound and its pop order, against the real coordinator: one
	# slice of 1 meta + 20 data groups, so the first batch spends its whole
	# MaxDelGrpPerTxn budget on the DATA tail and the second takes the meta
	# group and finishes the slice. Every group is one extent on both DNs, so
	# the charge is 21 extents per DN and the arithmetic is the group COUNT.
	local -a wide=(--group "1:1:meta:1:raid1" --leg "1:1:0" --leg "1:2:1"
		--side "1:101:1:0" --side "2:102:2:0")
	local g leg1 leg2
	for g in $(seq 2 21); do
		leg1=$((g * 2 - 1))
		leg2=$((g * 2))
		wide+=(--group "1:$g:data:1:raid1"
			--leg "$g:$leg1:0" --leg "$g:$leg2:1"
			--side "$leg1:$((100 + leg1)):1:0"
			--side "$leg2:$((100 + leg2)):2:0")
	done
	ctl put-sp --name sp2 --id 3 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true \
		--slice 1:0 \
		"${wide[@]}" >/dev/null
	assert_eq "$(dn_free 1)" "43" "dn 1 free after the wide fixture"
	local slice_before
	slice_before=$(drain_step_records slice)
	ctl set-deleting --sp sp2 >/dev/null
	wait_until "$WAIT_SYNCUP" "sp2 to drain away" drain_finished
	assert_eq "$((  $(drain_step_records slice) - slice_before ))" "2" \
		"a 21-group slice takes exactly two batches"
	assert_eq "$(wcount 'select(.msg == "sp drain step")
		| select(.phase == "slice") | select(.grp_cnt == 20)')" "1" \
		"the first batch spent the whole MaxDelGrpPerTxn budget"
	assert_eq "$(drained_records)" "3" "sp2 committed its own final STM"
	assert_eq "$(drain_failed_records)" "0" "no drain step failed"
	drain_assert_restored 64 64 4

	stage 5 "the CLONE drain: a latched clone leaves the plan and its keys go"
	# The clone drain (CLD1 to CLD12) alongside the sp drain, in the same case
	# because they share the coordinator and the fixture machinery. 65 chunks
	# is one more than MaxDelBmPerTxn, so the drain provably needs TWO batches:
	# the bound is what makes a batch's size independent of the clone's shape,
	# and a single-batch fixture could not tell the two apart.
	drain_put_sp sp3 4
	ctl put-td --sp sp3 --name td0 --id "$TD_ID" --size 10737418240 >/dev/null
	ctl put-clone --sp sp3 --name c0 --id "$CLONE_ID" --dst-td td0 \
		--src-slice-cnt 5 >/dev/null
	# One ssh for all 65 puts: 65 round trips would dominate the case.
	sshw "set -e; for s in 0 1 2 3 4; do for b in \$(seq 0 12); do" \
		"$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CLUSTER --trace-id $TRACE put-bitmap --sp sp3" \
		"--kind clone --name c0 --src-slice-idx \$s --bm-idx \$b --hex 0102" \
		"> /dev/null; done; done"
	assert_eq "$(key_cnt clone_bitmap)" "65" "the clone's chunk keys"
	wait_until "$WAIT_SYNCUP" "cn0: SyncupCntlr carrying the clone" \
		req_ge 1 cn0 SyncupCntlr '((.clone_list // []) | length) == 1'
	local clone_steps_before excluded_before
	clone_steps_before=$(clone_drain_step_records)
	# BASELINED, because every SyncupCntlr this SP sent BEFORE the clone
	# existed also carried an empty clone_list: without a baseline the
	# exclusion assertion below is satisfied by a request from stage 5's first
	# line and proves nothing at all.
	excluded_before=$(reqs cn0 SyncupCntlr '((.clone_list // []) | length) == 0')
	ctl set-clone-deleting --sp sp3 --name c0 >/dev/null
	wait_until "$WAIT_SYNCUP" "c0 to drain away" clone_gone
	assert_eq "$(key_cnt clone_bitmap)" "0" "chunk keys after the clone drain"
	assert_eq "$(key_cnt clone)" "0" "clone keys after the clone drain"
	assert_eq "$(ctl get-sp --sp sp3 | "$JQ" -r \
		'(.sp_conf.clone_name_list // []) | length')" "0" \
		"the name left clone_name_list with the key, in one STM"
	assert_eq "$((  $(clone_drain_step_records) - clone_steps_before ))" "2" \
		"65 chunks take exactly two batches"
	assert_eq "$(wcount 'select(.msg == "clone drained")')" "1" \
		"exactly one \`clone drained\` record"
	assert_eq "$(wcount 'select(.msg == "clone drain failed")')" "0" \
		"no clone drain step failed"
	# CLD5, observed from the agent's side: the latched clone left every
	# cntlr's plan. That exclusion IS the teardown — the cn agent's sweep
	# (cnagent.md CN21) drops the stack and the local chunk files precisely
	# when the id stops appearing — so the SP must keep syncing while
	# carrying no clone at all.
	wait_until "$WAIT_SYNCUP" "cn0: a NEW SyncupCntlr with the clone excluded" \
		reqs_gt "$excluded_before" cn0 SyncupCntlr \
		'((.clone_list // []) | length) == 0'
	# And the SP itself is untouched by any of it: the clone drain is
	# ledger-free and the SP outlives the clone.
	assert_eq "$(key_cnt slice)" "2" "sp3's slices are untouched"
	assert_eq "$(dn_free 1)" "58" "the clone drain moved no DN budget"

	stage 6 "a PARTIALLY drained clone resumes after a worker restart"
	# CLD7's "crash, restart and ownership handoff all resume through this same
	# derivation", built rather than raced for stage 3's reason: the window a
	# real restart would have to hit is sub-second. With the fleet stopped, c1
	# is latched and advanced by exactly ONE batch; what the restarted fleet
	# finds is a clone with some chunks gone and some left, and nothing but the
	# surviving keys tells it where it is.
	local w
	ctl put-clone --sp sp3 --name c1 --id "$((CLONE_ID + 1))" --dst-td td0 \
		--src-slice-cnt 5 >/dev/null
	sshw "set -e; for s in 0 1 2 3 4; do for b in \$(seq 0 12); do" \
		"$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CLUSTER --trace-id $TRACE put-bitmap --sp sp3" \
		"--kind clone --name c1 --src-slice-idx \$s --bm-idx \$b --hex 0102" \
		"> /dev/null; done; done"
	assert_eq "$(key_cnt clone_bitmap)" "65" "c1's chunk keys"
	for w in w1 w2 w3; do stop_worker "$w"; done
	ctl set-clone-deleting --sp sp3 --name c1 >/dev/null
	local partial
	partial=$(ctl drain-clone --sp sp3 --name c1 --max-steps 1)
	assert_field "$partial" '.clone_deleted' "false" \
		"one batch must NOT finish a 65-chunk drain"
	assert_eq "$(jq_of "$partial" '.chunk_cnt')" "64" \
		"the one batch spent the whole MaxDelBmPerTxn budget"
	assert_eq "$(key_cnt clone_bitmap)" "1" "one chunk key survives the batch"
	assert_eq "$(key_cnt clone)" "1" "the clone key survives the batch"
	clone_steps_before=$(clone_drain_step_records)
	for w in w1 w2 w3; do start_worker "$w"; done
	for w in w1 w2 w3; do wait_registered "$w"; done
	log "  waiting $((VOTE_GRACE + 2))s for the grace window (VW7)"
	sleep $((VOTE_GRACE + 2))
	assert_owners 50 w1 w2 w3
	wait_until "$WAIT_SYNCUP" "c1 to finish draining after the restart" \
		clone_gone
	assert_eq "$(key_cnt clone_bitmap)" "0" "chunk keys after the resumed drain"
	# The restarted fleet took exactly ONE more batch — the single surviving
	# chunk — and then the final STM: it re-derived the position from the
	# surviving keys.
	assert_eq "$((  $(clone_drain_step_records) - clone_steps_before ))" "1" \
		"the resumed drain took one batch, for the one chunk that was left"
	assert_eq "$(wcount 'select(.msg == "clone drained")')" "2" \
		"c1 committed its own final STM"
	assert_eq "$(wcount 'select(.msg == "clone drain failed")')" "0" \
		"no clone drain step failed"
}

case_vote() {
	CASE=vote

	stage 1 "cluster it-vote with DNs spread over four shards"
	new_cluster vote
	local shards=(00 55 aa ff) i
	for i in 1 2 3 4; do
		ctl put-dn --id "$i" --shard "${shards[$((i - 1))]}" \
			--addr "$(dn_addr "$i")" --location "loc-dn$((i - 1))" --free-ext 8
	done
	for i in 1 2 3 4; do
		wait_until "$WAIT_SHORT" "$(dn_dir "$i"): SyncupDn revision 1" \
			req_ge 1 "$(dn_dir "$i")" SyncupDn '(.revision | tostring) == "1"'
	done

	stage 2 "the three-member ownership table is exact"
	assert_owners 50 w1 w2 w3
	refresh_owners w1 w2 w3
	local role
	: >"$OWN_BEFORE_FILE"
	for role in dn cn sp; do
		owners_from "$role" | sed "s/^/$role /" >>"$OWN_BEFORE_FILE"
	done
	# The RAW owned/released records are kept as well, not only the folded
	# table: the fleet restart at the head of the case ramps up from one
	# worker to three, so many shards ALREADY carry a `shard released` record
	# from that — and step 3's "released by exactly one old owner" is about
	# the join alone.
	cp "$OWN_FILE" "$OWN_JOIN_FILE"

	stage 3 "a join moves about a quarter of the shards and nothing else"
	start_worker w4
	wait_registered w4
	local seed4 w
	seed4=$(seed_of w4)
	log "  w4 seed $seed4"
	for w in w1 w2 w3; do
		wait_until "$WAIT_MEMBERSHIP" "$w: membership committed member for w4" \
			membership_committed_all_roles "$w" "$seed4" member
		wait_until "$WAIT_MEMBERSHIP" "$w: member_cnt 4 for w4's seed" \
			member_cnt_seen "$w" "$seed4" 4
	done
	# "w4 logs the same for the three others and itself" — the same record,
	# w4's own view of the four-member fleet is what its ownership share is
	# computed from, and only w1..w3's views were checked above.
	#
	# member_cnt is the size of the effective set AFTER each commit (VW6), and
	# a joiner commits all four registrations within one grace window, so its
	# records ramp 1,2,3,4 per role in an arbitrary order — only the
	# last-committed seed of a role ever carries 4. Asserting member_cnt 4 for
	# a NAMED seed is therefore wrong in w4's own log (it is right in w1..w3's,
	# where w4's commit is the one that takes the set from 3 to 4). What must
	# hold here is that w4 commits all four as members and that its final view
	# per role is a four-member set.
	for w in w1 w2 w3; do
		wait_until "$WAIT_MEMBERSHIP" "w4: membership committed member for $w" \
			membership_committed_all_roles w4 "$(seed_of "$w")" member
	done
	wait_until "$WAIT_MEMBERSHIP" "w4: membership committed member for itself" \
		membership_committed_all_roles w4 "$seed4" member
	wait_until "$WAIT_MEMBERSHIP" "w4: a four-member effective set in every role" \
		max_member_cnt_is w4 4
	wait_until "$WAIT_MEMBERSHIP" "the four-member ownership table" \
		owners_settled 40 w1 w2 w3 w4
	assert_owners 40 w1 w2 w3 w4
	refresh_owners w1 w2 w3 w4
	local moved=0 stable=0 shard old new
	for role in dn cn sp; do
		local cnt
		cnt=$(owners_from "$role" | awk '$2 == "w4"' | wc -l | tr -d ' ')
		assert_between "$cnt" 40 90 "role $role: shards owned by the joiner w4"
		while read -r shard new; do
			old=$(awk -v r="$role" -v s="$shard" \
				'$1 == r && $2 == s { print $3 }' "$OWN_BEFORE_FILE")
			if [ "$new" = w4 ]; then
				moved=$((moved + 1))
				# EXACTLY ONE worker released the shard since the join, and it
				# is the old owner. Counting the old owner's own records alone
				# would miss a second worker releasing the same shard; counting
				# over the whole log would count the fleet restart's ramp-up,
				# hence the pre-join baseline.
				local rel
				rel=$(awk -v r="$role" -v s="$shard" '
					FNR == NR {
						if ($2 == r && $3 == s && $4 == "released") {
							before[$1]++
						}
						next
					}
					$2 == r && $3 == s && $4 == "released" { after[$1]++ }
					END {
						for (w in after) {
							if (after[w] > before[w] + 0) print w
						}
					}' "$OWN_JOIN_FILE" "$OWN_FILE" | sort -u | tr '\n' ' ')
				assert_eq "$rel" "$old " \
					"role $role shard $shard: released by exactly one worker, the old owner $old"
			else
				assert_eq "$new" "$old" \
					"role $role shard $shard: owner unchanged by the join"
				stable=$((stable + 1))
			fi
		done < <(owners_from "$role")
	done
	log "  $moved shards moved to w4, $stable kept their owner"
	assert_ge "$moved" 1 "shards moved by the join"

	stage 4 "attribution: the fake's trace ids name the owner"
	# `checked` counts the DNs whose shard the join actually moved, and it can
	# legitimately be 0: the four DN shards are fixed (00, 55, aa, ff) while
	# w4's seed is random, so P(none of the four moved) = (3/4)^4 ≈ 32 % and
	# requiring one would fail a third of all runs. So the loop asserts
	# something for EVERY DN, the unmoved ones included: a shard
	# that did not move must never have been driven by anybody but its
	# unchanged owner, and every SyncupDn of every DN carries revision 1. The
	# deterministic form of the moved-shard proof is case F step 1, which
	# kills the owner of (dn, 00) and asserts exactly this handoff.
	local checked=0 dshard downer dseed8 dir
	for i in 1 2 3 4; do
		dshard=${shards[$((i - 1))]}
		dir=$(dn_dir "$i")
		downer=$(awk -v r=dn -v s="$dshard" '$1 == r && $2 == s { print $3 }' "$OWN_BEFORE_FILE")
		new=$(owners_from dn | awk -v s="$dshard" '$1 == s { print $2 }')
		assert_ge "$(reqs "$dir" SyncupDn)" 1 "$dir: SyncupDn requests"
		local old8 new8 seeds
		old8=$(seed_of "$downer" | cut -c1-8)
		new8=$(seed8 "$new")
		if [ "$downer" = "$new" ]; then
			# Unmoved: the DnConf was written after the three-member table had
			# settled, so its owner is the ONLY worker that may ever have
			# driven it — including across the join.
			seeds=$(recsr "$(apath "$dir")" \
				'select(.msg == "grpc server request")
				 | select((.method | split("/") | last) == "SyncupDn")
				 | (.trace_id | split("-")[0])' | sort -u | tr '\n' ',')
			assert_eq "$seeds" "$new8," \
				"$dir (shard $dshard, unmoved): every SyncupDn came from $new alone"
		else
			checked=$((checked + 1))
			log "  dn $i (shard $dshard) moved from $downer to $new"
			# The same revision, from the old owner's seed and then the new.
			assert_ge "$(trace_reqs "$dir" SyncupDn "$old8")" 1 \
				"$dir: a SyncupDn from the old owner $downer"
			# NOT "a SyncupDn from the new owner": RW4 step 5 says the first
			# sync after a shard handoff happens THROUGH THE ROUND — the new
			# owner issues a Syncup* only when the reply's code != 0 or its
			# revision differs from the desired one. Here the agent is already
			# at the desired revision, so a correct worker sends NO SyncupDn
			# and simply advances `synced` from the clean Check reply (RW2).
			# Verified against a real run: the fake's log carried exactly one
			# SyncupDn, from the OLD owner's seed8, and the new owner drove it
			# by Check rounds alone.
			#
			# The handoff evidence is therefore the Check stream, which the
			# per-DN loop below asserts for every DN: after settling, the
			# rounds of the last 3 s must carry the CURRENT owner's seed8 and
			# nobody else's. Here we pin the old owner's syncup and the new
			# owner's rounds, and that none of those rounds led to a SyncupDn.
			# The handover is seconds old by now — step 3 waited for the
			# ownership table to settle — so a re-sync that followed the new
			# owner's first reply would already be in the log.
			wait_until "$WAIT_MEMBERSHIP" "$dir: CheckDn rounds from the new owner $new" \
				trace_req_ge 1 "$dir" CheckDn "$new8"
			assert_eq "$(trace_reqs "$dir" SyncupDn "$new8")" 0 \
				"$dir: SyncupDn requests from the new owner $new (RW4 step 5: none)"
		fi
		local revs
		revs=$(recsr "$(apath "$dir")" \
			'select(.msg == "grpc server request")
			 | select((.method | split("/") | last) == "SyncupDn")
			 | select(has("data")) | (.data.revision // 0 | tostring)' |
			sort -u | tr '\n' ',')
		assert_eq "$revs" "1," "$dir: every SyncupDn carried the same revision"
	done
	# Whether or not a DN's shard moved, every round in the last three seconds
	# must come from the shard's CURRENT owner and nobody else. The clock is
	# read in a statement of its own, as in case D step 12: read inside an
	# argument, a failed read would be an empty stamp, midnight today to
	# `date`, and the check would take in every one of the last 20 rounds
	# recent_seeds reads, not only those of the last three seconds.
	local now_at cutoff
	now_at=$(server_now)
	cutoff=$(ts_epoch "$now_at")
	sleep 3
	for i in 1 2 3 4; do
		dshard=${shards[$((i - 1))]}
		dir=$(dn_dir "$i")
		downer=$(owners_from dn | awk -v s="$dshard" '$1 == s { print $2 }')
		dseed8=$(seed8 "$downer")
		assert_eq "$(recent_seeds "$dir" CheckDn "$cutoff")" "$dseed8," \
			"$dir: the CheckDn rounds of the last 3s come from $downer only"
	done
	log "  $checked of the four DN shards moved and were checked end to end"
	if [ "$checked" -eq 0 ]; then
		log "  (no DN shard moved this run — the moved-shard attribution is"
		log "   proven deterministically by case F step 1)"
	fi

	stage 5 "SIGKILL: dead after 2x the interval, nonmember one grace later"
	local seed2 kill_at
	seed2=$(seed_of w2)
	kill_at=$(sig_dir_at w2 KILL)
	wait_gone w2 "$WAIT_SHORT"
	# VW6 is per worker: each survivor observes the death and commits it in
	# its own log, so all three are inspected. w1's record is additionally the
	# one the latency comparison of step 6 uses.
	for w in w1 w3 w4; do
		wait_until $((4 + WAIT_SHORT)) "$w: membership observed dead for w2" \
			membership_observed "$w" "$seed2" dead
		wait_until "$WAIT_MEMBERSHIP" "$w: membership committed nonmember for w2" \
			membership_committed_all_roles "$w" "$seed2" nonmember
	done
	wait_until "$WAIT_MEMBERSHIP" "w2's registrations to be garbage-collected" \
		worker_not_listed "$seed2"
	wait_until "$WAIT_MEMBERSHIP" "the three-member table over w1 w3 w4" \
		owners_settled 50 w1 w3 w4
	assert_owners 50 w1 w3 w4
	local kill_commit kill_latency
	kill_commit=$(rec_time "$(wpath w1)" \
		'.msg == "membership committed" and .seed == $s and .state == "nonmember" and .role == "dn"' \
		1 --arg s "$seed2")
	kill_latency=$(ts_delta "$kill_commit" "$kill_at")
	log "  SIGKILL -> nonmember commit: ${kill_latency}s"

	stage 6 "SIGTERM: the owner deletes its own keys, so the commit is sooner"
	local seed4_now term_at stops4
	seed4_now=$(seed_of w4)
	stops4=$(count_recs "$(wpath w4)" 'select(.msg == "worker stopping")')
	term_at=$(sig_dir_at w4 TERM)
	wait_until "$WAIT_MEMBERSHIP" "w4: worker stopping" \
		worker_stopped_since w4 "$stops4"
	wait_gone w4 "$WAIT_MEMBERSHIP"
	assert_eq "$(recsr "$(wpath w4)" '.msg' | tail -n 1)" "worker stopping" \
		"w4's log ends with worker stopping"
	wait_until "$WAIT_SHORT" "w4's registrations to disappear at once" \
		worker_not_listed "$seed4_now"
	# Both survivors commit it, each inside one grace window; w1's record is
	# then the one the latency comparison below reads.
	for w in w1 w3; do
		wait_until $((VOTE_GRACE + 2)) "$w: membership committed nonmember for w4" \
			membership_committed_all_roles "$w" "$seed4_now" nonmember
	done
	local term_commit term_latency
	term_commit=$(rec_time "$(wpath w1)" \
		'.msg == "membership committed" and .seed == $s and .state == "nonmember" and .role == "dn"' \
		1 --arg s "$seed4_now")
	term_latency=$(ts_delta "$term_commit" "$term_at")
	log "  SIGTERM -> nonmember commit: ${term_latency}s"
	# The bounds are DERIVED here, from the rules this step tests (CM5, VW3).
	#
	# SIGTERM deletes the registrations at once (CM5), so a peer sees the
	# disappear immediately and commits one grace window later:
	#     term_latency ~ VOTE_GRACE, and always < VOTE_GRACE + 2.
	# SIGKILL deletes nothing, so a peer must first watch the registration go
	# stale at lastSeen + 2*VOTE_INTERVAL and only then run the grace window.
	# Measured from the SIGNAL, that is
	#     2*VOTE_INTERVAL + VOTE_GRACE - (time since the victim's last put)
	# and the victim heartbeats every VOTE_INTERVAL, so the true window is
	#     [VOTE_INTERVAL + VOTE_GRACE, 2*VOTE_INTERVAL + VOTE_GRACE]
	# = [8, 10] s at this suite's timers. Where inside that window a run lands
	# depends on where the kill fell in the victim's heartbeat cycle, so the
	# floor asserted below is VOTE_INTERVAL + VOTE_GRACE and the ceiling
	# carries the same 2 s of slack as SIGTERM's.
	#
	# What the step actually proves is the ORDERING — a graceful stop is
	# committed measurably sooner because it skips the dead-detection window —
	# so that is asserted explicitly alongside the derived bounds.
	local kill_floor
	kill_floor=$((VOTE_INTERVAL + VOTE_GRACE))
	ts_lt "$term_latency" $((VOTE_GRACE + 2)) ||
		die "SIGTERM commit latency ${term_latency}s, want < $((VOTE_GRACE + 2))s"
	ts_ge "$kill_latency" "$kill_floor" ||
		die "SIGKILL commit latency ${kill_latency}s, want >= ${kill_floor}s (VOTE_INTERVAL + VOTE_GRACE)"
	ts_lt "$kill_latency" $((2 * VOTE_INTERVAL + VOTE_GRACE + 2)) ||
		die "SIGKILL commit latency ${kill_latency}s exceeds 2*VOTE_INTERVAL + VOTE_GRACE"
	ts_lt "$term_latency" "$kill_latency" ||
		die "SIGTERM (${term_latency}s) was not committed sooner than SIGKILL (${kill_latency}s): the CM5 key delete bought nothing"
	wait_until "$WAIT_MEMBERSHIP" "the two-member table over w1 w3" \
		owners_settled 50 w1 w3
	assert_owners 50 w1 w3

	stage 7 "SIGSTOP / SIGCONT: the self-fence of VW8 (a)"
	local seed3 held3 rel_before
	seed3=$(seed_of w3)
	# Every shard w3 holds under the old seed, ALL THREE ROLES: VW8 (a) is
	# "w3 logged `shard released` for every shard it held before driving
	# anything under the new seed", so the SET is what has to match — one
	# release record in the dn role proves almost nothing.
	#
	# The releases are counted from HERE: w3 has already released, under this
	# same seed, every shard the join of step 3 moved to w4, and those are
	# neither held now nor part of the fence.
	refresh_owners w1 w3
	held3=$(for role in dn cn sp; do
		owners_from "$role" | awk -v r="$role" '$2 == "w3" { print r, $1 }'
	done | sort)
	[ -n "$held3" ] || die "w3 held no shard before the SIGSTOP"
	rel_before=$(count_recs "$(wpath w3)" \
		'select(.msg == "shard released") | select(.seed == $s)' --arg s "$seed3")
	sig_dir w3 STOP
	wait_until $((4 + WAIT_SHORT)) "w1: membership observed dead for w3" \
		membership_observed w1 "$seed3" dead
	wait_until "$WAIT_MEMBERSHIP" "w1: membership committed nonmember for w3" \
		membership_committed_all_roles w1 "$seed3" nonmember
	wait_until "$WAIT_MEMBERSHIP" "w1 to own all 256 shards of every role" \
		w1_owns_everything
	sig_dir w3 CONT
	wait_until "$WAIT_MEMBERSHIP" "w3: worker fenced reason=heartbeat_stalled" \
		fenced_since w3 "$seed3"
	local seed3_new
	seed3_new=$(seed_of w3)
	assert_ne "$seed3_new" "$seed3" "w3's seed after the fence"
	wait_until "$WAIT_MEMBERSHIP" "w1: membership committed member for w3's new seed" \
		membership_committed_all_roles w1 "$seed3_new" member
	wait_until "$WAIT_MEMBERSHIP" "the two-member table over w1 w3 again" \
		owners_settled 50 w1 w3
	assert_owners 50 w1 w3
	# The read is a statement of its own: `$(ctl … | grep -c …)` would count 0
	# on a FAILED read too and the assertion would pass on a dropped ssh.
	local live_seeds
	live_seeds=$(worker_seeds)
	assert_eq "$(printf '%s\n' "$live_seeds" | grep -cx "$seed3" || true)" 0 \
		"the fenced seed's registrations"
	# VW8: EVERY shard held under the old seed is released, and all of it
	# before anything is driven under the new one.
	local released3 first_new_owned last_old_released
	released3=$(recsr "$(wpath w3)" \
		'select(.msg == "shard released") | select(.seed == $s)
		 | "\(.role) \(.shard)"' --arg s "$seed3" |
		tail -n +$((rel_before + 1)) | sort -u)
	assert_eq "$released3" "$held3" \
		"w3: the shards released under the fenced seed are exactly the ones it held"
	last_old_released=$(recsr "$(wpath w3)" \
		'select(.msg == "shard released") | select(.seed == $s) | .time' --arg s "$seed3" |
		tail -n 1)
	first_new_owned=$(recsr "$(wpath w3)" \
		'select(.msg == "shard owned") | select(.seed == $s) | .time' --arg s "$seed3_new" |
		sed -n 1p)
	[ -n "$last_old_released" ] ||
		die "w3 released no shard under its old seed (it held: $held3)"
	[ -n "$first_new_owned" ] || die "w3 owned no shard under its new seed"
	ts_lt "$(ts_epoch "$last_old_released")" "$(ts_epoch "$first_new_owned")" ||
		die "w3 owned a shard under the new seed before releasing the old ones"

	stage 8 "w2 and w4 stay stopped until the next fleet restart"
	# This step leaves w2 and w4 to the fleet restart of the next case (VW7),
	# and that restart starts w1 w2 w3, the three-worker baseline of
	# dnv-worker.md, Integration test plan, Topology. So w2 comes back and w4
	# deliberately does NOT: w4 is case E's own extra worker, and every later
	# case asserts its ownership tables over the three-worker fleet.
	# fleet_restart's stop loop covers w4 anyway (it is a no-op for a worker
	# this script already knows is down).
	log "  leaving w2 and w4 stopped; the next fleet restart returns to w1 w2 w3"
}

owners_settled() { # <min> <worker…>
	local min=$1
	shift
	local want
	want=$(printf '%s\n' "$@" | sort | tr '\n' ' ')
	refresh_owners "$@"
	local role table
	for role in dn cn sp; do
		table=$(owners_from "$role")
		[ "$(printf '%s\n' "$table" | awk 'NF' | wc -l | tr -d ' ')" -eq 256 ] || return 1
		[ "$(printf '%s\n' "$table" | awk 'NF { print $2 }' | sort -u | tr '\n' ' ')" = "$want" ] ||
			return 1
		local w cnt
		for w in "$@"; do
			cnt=$(printf '%s\n' "$table" | awk -v w="$w" 'NF && $2 == w' | wc -l | tr -d ' ')
			[ "$cnt" -ge "$min" ] || return 1
		done
	done
	return 0
}

fenced_since() { # <w> <old seed>
	count_ge 1 count_recs "$(wpath "$1")" \
		'select(.msg == "worker fenced")
		 | select(.old_seed == $s and .reason == "heartbeat_stalled")' \
		--arg s "$2"
}

trace_reqs() { # <agent> <method> <seed8>
	count_recs "$(apath "$1")" \
		"select(.msg == \"grpc server request\" or .msg == \"grpc server recv\")
		 | select((.method | split(\"/\") | last) == \"$2\")
		 | select((.trace_id | split(\"-\")[0]) == \$s)" --arg s "$3"
}

trace_req_ge() { # <n> <agent> <method> <seed8>
	count_ge "$1" trace_reqs "$2" "$3" "$4"
}

# recent_seeds prints the distinct seed8 prefixes of one method's records that
# are newer than a cutoff. The cutoff is a SERVER clock reading, and each
# record's own timestamp is converted the same way, so "in the last N seconds"
# never depends on the driver's clock. Only the last 20 records are considered:
# a round is one second, so that is far more history than any window asked for.
recent_seeds() { # <agent> <method> <cutoff epoch>
	local pairs t seed out=""
	pairs=$(recsr "$(apath "$1")" \
		'select(.msg == "grpc server recv" or .msg == "grpc server request")
		 | select((.method | split("/") | last) == $m)
		 | "\(.time) \(.trace_id | split("-")[0])"' --arg m "$2" | tail -n 20)
	while read -r t seed; do
		[ -n "$t" ] || continue
		ts_ge "$(ts_epoch "$t")" "$3" || continue
		out="$out$seed"$'\n'
	done <<<"$pairs"
	printf '%s' "$out" | sort -u | tr '\n' ','
}

# ---------------------------------------------------------------------------
# Case F — handoff (dnv-worker.md, Integration test plan, Cases)
# ---------------------------------------------------------------------------

case_handoff() {
	CASE=handoff

	stage 1 "a killed owner's DN shard is re-driven at the same revision"
	new_cluster handoff
	put_dn 1 8
	wait_until "$WAIT_SHORT" "dn0: SyncupDn revision 1" \
		req_ge 1 dn0 SyncupDn '(.revision | tostring) == "1"'
	local owner survivors=() w
	owner=$(owner_of dn 00)
	[ -n "$owner" ] || die "no owner for (dn, 00)"
	log "  (dn, 00) is owned by $owner"
	local old8
	old8=$(seed8 "$owner")
	assert_ge "$(trace_reqs dn0 SyncupDn "$old8")" 1 \
		"dn0: a SyncupDn from the owner before the kill"
	sig_dir "$owner" KILL
	wait_gone "$owner" "$WAIT_SHORT"
	wait_until "$WAIT_MEMBERSHIP" "another worker to own (dn, 00)" \
		shard_owner_changed dn 00 "$owner"
	local new_owner new8
	new_owner=$(owner_of dn 00)
	new8=$(seed8 "$new_owner")
	log "  (dn, 00) moved to $new_owner"
	# The new owner re-drives the DN through its Check stream, not through a
	# SyncupDn: RW4 step 5 issues a Syncup* only when the reply's code != 0 or
	# its revision differs from the desired one, and a SIGKILLed owner leaves
	# the agent AT the desired revision — so a correct worker sends none, and
	# `synced` advances from the clean Check reply (RW2). "Re-driven at the
	# same revision" is therefore proven by the Check rounds below plus the
	# revision check here, which still pins that no syncup ever carried a
	# revision other than 1 — and none of those rounds may lead to a SyncupDn
	# from the new owner. This is the deterministic twin of case E step 4's
	# moved-shard branch; a re-sync issued on the new owner's first reply
	# follows it at once, well before the read below, which costs a full ssh.
	wait_until "$WAIT_MEMBERSHIP" "dn0: CheckDn rounds from the new owner" \
		trace_req_ge 1 dn0 CheckDn "$new8"
	assert_eq "$(trace_reqs dn0 SyncupDn "$new8")" 0 \
		"dn0: SyncupDn requests from the new owner $new_owner (RW4 step 5: none)"
	assert_eq "$(recsr "$(apath dn0)" \
		'select(.msg == "grpc server request")
		 | select((.method | split("/") | last) == "SyncupDn")
		 | select(has("data")) | (.data.revision // 0 | tostring)' |
		sort -u | tr '\n' ',')" "1," \
		"dn0: every SyncupDn carried revision 1 (the same revision, re-driven)"
	# "err_epoch 0 THROUGHOUT", not "0 once the handoff has finished": one
	# read after the fact cannot see an epoch that was set and cleared while
	# the shard was moving. The health bookkeeping can — HL1 writes a `health
	# changed` record on every transition, and neither the killed owner nor
	# the new one may have written one for this DN.
	assert_eq "$(dn_err_epoch 1)" 0 "dn 1 err_epoch across the handoff"
	assert_eq "$(health_changed_cnt dn unreachable)" 0 \
		"health changed record=dn reason=unreachable across the handoff"
	assert_eq "$(health_changed_cnt dn error_row)" 0 \
		"health changed record=dn reason=error_row across the handoff"

	stage 2 "a flip mid-handoff happens once"
	put_dn 2 8
	put_cn 1 64
	# Neither side ever finishes provisioning while the override is in place.
	set_behavior dn0 <<'EOF'
{"objects": {"side 1:1:1": {"zeroed_ext_cnt": 0}}}
EOF
	set_behavior dn1 <<'EOF'
{"objects": {"side 1:2:2": {"zeroed_ext_cnt": 0}}}
EOF
	ctl put-sp --name sp0 --id 1 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true \
		--slice 1:0 \
		--group 1:1:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 \
		--side 1:1:1:0 --side 2:2:2:0
	wait_until "$WAIT_SYNCUP" "dn0: SyncupSide with provisioned false" \
		req_ge 1 dn0 SyncupSide \
		'(.side_pointer.side_id | tostring) == "1" and ((.side_conf.provisioned // false) == false)'
	wait_until "$WAIT_SYNCUP" "dn1: SyncupSide with provisioned false" \
		req_ge 1 dn1 SyncupSide \
		'(.side_pointer.side_id | tostring) == "2" and ((.side_conf.provisioned // false) == false)'
	local sp_owner
	sp_owner=$(owner_of sp 00)
	[ -n "$sp_owner" ] || die "no owner for (sp, 00)"
	log "  (sp, 00) is owned by $sp_owner"
	local rev
	rev=$(sp_rev 1)
	sig_dir "$sp_owner" KILL
	wait_gone "$sp_owner" "$WAIT_SHORT"
	survivors=()
	for w in w1 w2 w3; do
		if [ "${RUNNING[$w]-}" = 1 ]; then survivors+=("$w"); fi
	done
	wait_until "$WAIT_MEMBERSHIP" "another worker to own (sp, 00)" \
		shard_owner_changed sp 00 "$sp_owner"
	local sp_new
	sp_new=$(owner_of sp 00)
	log "  (sp, 00) moved to $sp_new"
	# As in stage 1: the new owner re-drives the side through its Check
	# stream. RW4 step 5 issues a Syncup* only on a code != 0 or a revision
	# mismatch, and the killed owner left the agent at the desired revision,
	# so a correct worker sends no SyncupSide here. What must hold is that the
	# new owner is driving the side and that the revision did not move.
	wait_until "$WAIT_MEMBERSHIP" "dn0: CheckSide rounds from the new sp owner" \
		trace_req_ge 1 dn0 CheckSide "$(seed8 "$sp_new")"
	assert_eq "$(sp_rev 1)" "$rev" "SpRev across the sp handoff (the same revision)"
	assert_eq "$(last_req dn0 SyncupSide \
		'(.side_pointer.side_id | tostring) == "1"' | "$JQ" -r \
		'"\(.revision) \(.side_conf.provisioned // false)"')" "$rev false" \
		"dn0: the new owner re-drove side 1 at the same revision, still unprovisioned"

	# Released one side at a time, so "exactly one flip and exactly one SpRev
	# bump" is checkable per side rather than depending on whether the
	# coordinator happened to batch both into one STM (RW18's "MAY").
	local flips
	flips=$(flip_cnt provisioned)
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "side 1 provisioned" side_provisioned sp0 1 1
	assert_eq "$(flip_cnt provisioned)" $((flips + 1)) \
		"flip applied kind=provisioned records for side 1 (exactly one)"
	assert_eq "$(sp_rev 1)" $((rev + 1)) "SpRev advanced by exactly one"
	rev=$((rev + 1))
	flips=$(flip_cnt provisioned)
	clear_behavior dn1
	wait_until "$WAIT_SHORT" "side 2 provisioned" side_provisioned sp0 1 2
	assert_eq "$(flip_cnt provisioned)" $((flips + 1)) \
		"flip applied kind=provisioned records for side 2 (exactly one)"
	assert_eq "$(sp_rev 1)" $((rev + 1)) "SpRev advanced by exactly one"
	flips=$(flip_cnt provisioned)
	assert_none_for 5 "a repeated flip after the handoff" \
		flip_gt "$flips" provisioned

	stage 3 "both sides released together: one flip per side, one bump per STM"
	# The handoff case (dnv-worker.md, Integration test plan, Cases) asks of
	# the flip only that one in the middle of a handoff is applied exactly
	# once, and for sides that finish together RW18 says only that several
	# sides reported within one round MAY share one STM — and the coordinator
	# logs one record per side it WROTE, all carrying that STM's revision. So
	# the batched round is run (step 2 releases one side at a time and never
	# exercises it) and the invariants RW18 actually guarantees are asserted:
	# each side flips exactly once, and SpRev advances by exactly the number
	# of distinct revisions those records carry — one if the STM was shared,
	# two if it was not, never more, and never a side twice.
	#
	# A second SP is needed because a provisioned side never goes back.
	set_behavior dn0 <<'EOF'
{"objects": {"side 2:1:1": {"zeroed_ext_cnt": 0}}}
EOF
	set_behavior dn1 <<'EOF'
{"objects": {"side 2:2:2": {"zeroed_ext_cnt": 0}}}
EOF
	ctl put-sp --name sp1 --id 2 --shard 00 --slots 0,1 --level 0 \
		--thresholds "$THRESHOLDS" --lwm "$LWM" \
		--cntlr 1:1:0:true \
		--slice 1:0 \
		--group 1:1:data:2:raid1 \
		--leg 1:1:0 --leg 1:2:1 \
		--side 1:1:1:0 --side 2:2:2:0
	local unprov='(.side_pointer.sp_id | tostring) == "2"
		and (.side_pointer.side_id | tostring) == $sid
		and ((.side_conf.provisioned // false) == false)'
	wait_until "$WAIT_SYNCUP" "dn0: sp1's side 1, provisioned false" \
		req_ge 1 dn0 SyncupSide "$unprov" --arg sid 1
	wait_until "$WAIT_SYNCUP" "dn1: sp1's side 2, provisioned false" \
		req_ge 1 dn1 SyncupSide "$unprov" --arg sid 2
	local sp1_rev sp1_flips
	sp1_rev=$(sp_rev 2)
	sp1_flips=$(flip_records 2 | wc -l | tr -d ' ')
	assert_eq "$sp1_flips" 0 "sp1: flips before the release"
	# Back-to-back writes, so both sides report zeroed == total in the round
	# that follows — whether the coordinator batches them is its choice.
	clear_behavior dn0
	clear_behavior dn1
	wait_until "$WAIT_SHORT" "sp1's side 1 provisioned" side_provisioned sp1 1 1
	wait_until "$WAIT_SHORT" "sp1's side 2 provisioned" side_provisioned sp1 1 2
	local batch sides bumps
	batch=$(flip_records 2)
	sides=$(printf '%s\n' "$batch" | awk 'NF { print $1 }' | sort | tr '\n' ',')
	assert_eq "$sides" "1,2," "sp1: exactly one flip record per side, neither twice"
	bumps=$(printf '%s\n' "$batch" | awk 'NF { print $2 }' | sort -u | wc -l | tr -d ' ')
	assert_between "$bumps" 1 2 "sp1: distinct revisions across the flip batch"
	assert_eq "$(sp_rev 2)" $((sp1_rev + bumps)) \
		"sp1: SpRev advanced by exactly one per flip STM"
	sp1_flips=$(flip_records 2 | wc -l | tr -d ' ')
	assert_none_for 5 "a repeated flip of sp1's sides" \
		flip_records_gt "$sp1_flips" 2

	stage 4 "the ownership table over the surviving workers"
	assert_owners $((256 / ${#survivors[@]})) "${survivors[@]}"
}

shard_owner_changed() { # <role> <shard> <old owner>
	local now
	now=$(owner_of "$1" "$2")
	[ -n "$now" ] && [ "$now" != "$3" ]
}

flip_gt() { # <n> <kind>
	count_gt "$1" flip_cnt "$2"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/worker_test.sh [--only <case>] [--cleanup-only] <user@ip>

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
	[ "${#positional[@]}" -eq 1 ] || usage
	TARGET=${positional[0]}
	IP=${TARGET##*@}
	[ -n "$IP" ] || usage
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
	OWN_FILE=$(mktemp)
	OWN_BEFORE_FILE=$(mktemp)
	OWN_JOIN_FILE=$(mktemp)
	trap on_exit EXIT
	log "driver: $(hostname), server: $TARGET (ip $IP)"

	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		STAGE="cleanup-only"
		log ""
		log "=== cleanup only"
		sshw "true" || die "passwordless ssh to $TARGET failed"
		cleanup
		return 0
	fi

	preflight_driver
	STAGE="start cleanup"
	log ""
	log "=== start-of-run cleanup (unconditional)"
	cleanup
	preflight_server
	setup

	local name
	for name in "${CASES[@]}"; do
		if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then continue; fi
		fleet_restart "$name"
		"case_$name"
	done
}

main "$@"
