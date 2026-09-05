#!/usr/bin/env bash
#
# worker_test.sh — the `dnv-worker` integration test of doc/dnv-worker.md §14.
# One server, a real single-node etcd, a real three-process worker fleet and
# the fake dn/cn agents of §14.9, driven from this machine over ssh by
# integtest/workerctl (the gateway's write path) and integtest/fakeagent.
#
#   bash integtest/worker_test.sh [--only <case>] [--cleanup-only] user@ip
#
# Cases (§14.11), in order, each in its own cluster `it-<case>` and each after
# a fleet restart (§14.10): smoke, revision, health, bitmap, reaction, vote,
# handoff. Cleanup runs unconditionally at the start and, on success only, at
# the end: a failing run leaves etcd's data, every log and every behavior file
# in place and dumps the §14.13 diagnostics.
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
# Constants (§14.3, §14.5, §14.6)
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

WORK=/var/tmp/dnv-worker-integtest

# §14.3 ports: nine in total, none shared with the two agent suites.
ETCD_CLIENT_PORT=12379
# common.DnvPrefix — the first field of every dnv etcd key (architecture.md §5.1).
DNV_PREFIX=dnv
ETCD_PEER_PORT=12380
DN_PORT_BASE=29600 # dn0..dn3 -> 29600..29603
CN_PORT_BASE=29700 # cn0..cn2 -> 29700..29702
ALL_PORTS=(12379 12380 29600 29601 29602 29603 29700 29701 29702)

# §14.6 test-time constants.
VOTE_INTERVAL=2
VOTE_GRACE=6
HEALTH_INTERVAL=1     # every health_check_conf.*_interval
THRESHOLDS=2,4,3,6    # primary, cntlr, side, leg
LWM=50                # low_water_mark_pct
EXTENT_SIZE=67108864  # 64 MiB = common.MinDnExtSize
BLOCK_SIZE=1048576    # 1 MiB, the dm-thin data block
CHUNK_BLOCKS=128      # bitmap_chunk_block_cnt
WAIT_SHORT=5
WAIT_MEMBERSHIP=20
WAIT_SYNCUP=65

# §3.6 geometry at (extent_size 64 MiB, block_size 1 MiB, chunk 128 blocks).
# It is derived once here because case D's numbers hang off it:
#
#   total_group_blocks = ext_cnt * 64 MiB / 1 MiB          = 64 * ext_cnt
#   bitmap_bits        = ceil(ext_cnt * 64 MiB / 128 MiB)  = 1 for ext_cnt <= 2
#   bitmap_bytes       = 256 + ceil(bits / 8)              = 257
#   bitmap_blocks      = ceil(257 / 1 MiB)                 = 1
#   meta_blocks        = 1 (md sb) + 1 (bitmap) + 1 (health) = 3   [raid1]
#   data_blocks        = total_group_blocks - meta_blocks
#
# so a meta group of ext_cnt 1 has data_blocks 61 and a data group of ext_cnt
# 2 has data_blocks 125. The pool's reported totals follow:
#
#   total_data (1 MiB data blocks) = sum of data_blocks over the DATA groups
#   total_meta (fixed 4 KiB blocks) = sum of data_blocks * 1 MiB / 4096
#                                   = data_blocks * 256 over the META groups
GRP_META_BLOCKS=3
DATA_GRP_DATA_BLOCKS=125 # ext_cnt 2
META_GRP_DATA_BLOCKS=61  # ext_cnt 1
META_BLOCKS_PER_GRP=15616 # 61 * 256, one meta group in 4 KiB blocks

# Sub-object ids the script assigns (§14.5). workerctl advances SpConf.next_id
# past all of them, so a worker reaction allocates ids ABOVE these.
TD_ID=0x10
SS_ID=0x20
NS_ID=0x21
CLONE_ID=0x30

NQN_PREFIX=nqn.2024-01.io.dnv-it

CASES=(smoke revision health bitmap reaction vote handoff)

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

# stage names the step for the failure report and mints the §14.10 trace id
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
		log "########## diagnostics (§14.13) ##########"
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
# Remote execution (§14.10)
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

# ctl is the §14.10 driver wrapper: every call carries the endpoints, the
# case's cluster and the stage's trace id.
#
# Each argument is quoted for the REMOTE shell with printf %q before the
# command string is built. Without that, an argument containing a space is
# re-split by the remote shell: an etcd key is space-joined
# (architecture.md §5.1), so `ctl get --key "dnv cdc <cid> <shard> <sp> <ss>"`
# reached workerctl as `--key dnv` plus five stray words and died — which,
# under `set -e` inside a command substitution, aborted the whole run.
ctl() {
	local quoted
	quoted=$(printf '%q ' "$@")
	sshw "$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CLUSTER --trace-id $TRACE $quoted"
}

# rlog is the §14.10 log reader: `ssh … cat <path>`, so every assertion is
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

count_recs() { recs "$@" | wc -l | tr -d ' \n'; }

# wcount counts matching records over EVERY worker log that holds records for
# the current case (the logs are truncated by each fleet restart, so this is
# per-case by construction).
wcount() { # <filter> [jq args…]
	local filter=$1
	shift
	local w total=0 n
	for w in "${SEEN_WORKERS[@]}"; do
		n=$(count_recs "$(wpath "$w")" "$filter" "$@")
		total=$((total + n))
	done
	printf '%s' "$total"
}

wcount_ge() { # <n> <filter> [jq args…]
	local n=$1
	shift
	[ "$(wcount "$@")" -ge "$n" ]
}

# ---------------------------------------------------------------------------
# Polling (§14.10)
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
# Ownership table (§14.10)
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

# owners is the §14.10 helper: the shards each LIVE worker currently holds for
# one role, taken fresh.
owners() { # <role>
	local live=()
	mapfile -t live < <(running_workers)
	refresh_owners "${live[@]}"
	owners_from "$1"
}

owner_of() { # <role> <shard>
	owners "$1" | awk -v s="$2" '$1 == s { print $2 }'
}

# assert_owners is §14.7 step 5: for every role, the 256 shards are covered
# exactly once, by exactly the expected workers, and no worker holds fewer than
# <min>.
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
# Seeds, requests and other log queries (§14.10)
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
	[ "$(reqs "$@")" -ge "$n" ]
}

reply_ge() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	[ "$(replies "$@")" -ge "$n" ]
}

reply_gt() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	[ "$(replies "$@")" -gt "$n" ]
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
server_now() { sshw "date -u +%Y-%m-%dT%H:%M:%S.%NZ"; }

ts_delta() { # <later> <earlier> -> seconds, one decimal
	awk -v a="$(ts_epoch "$1")" -v b="$(ts_epoch "$2")" 'BEGIN { printf "%.3f", a - b }'
}

ts_lt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a < b) }'; }
ts_ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a >= b) }'; }

# ---------------------------------------------------------------------------
# Node naming (§14.5)
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
# Behaviour and state files of the fakes (§14.9)
# ---------------------------------------------------------------------------

# set_behavior writes one fake's behavior.json from a here-doc on stdin. The
# fake re-reads the file whenever its mtime changes, so the write must be
# atomic: a half-written file would be parsed, rejected and IGNORED, silently
# keeping the previous behaviour.
set_behavior() { # <agent>   (JSON on stdin)
	local agent=$1
	[ "$QUIET" -eq 1 ] || log "[server] behavior -> $agent"
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"cat > $WORK/$agent/behavior.json.tmp &&
		 mv -f $WORK/$agent/behavior.json.tmp $WORK/$agent/behavior.json"
}

clear_behavior() { set_behavior "$1" <<<'{}'; }

# reset_state empties one fake's state.json the same way. The revision gate of
# §14.9 is per object ("dn", "cn", "side …", "cntlr …") and is NOT scoped by
# cluster, so a fake that ended case S holding dn revision 2 would reject case
# A's revision-1 syncup as stale. The per-case reset is what keeps every case
# independent; it runs while the fleet is stopped, so no request is in flight.
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
# Process control (§14.3): launch with >> so an external truncation resets the
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
	[ "$(count_recs "$(wpath "$1")" \
		'select(.msg == "worker stopping")')" -gt "$2" ]
}

# stop_worker is the CM5 graceful stop: SIGTERM, then wait for BOTH the
# `worker stopping` record and the process's exit before anybody restarts it,
# so the pid/port bookkeeping never drifts (§14.10).
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
# Per-case reset (§14.10 fleet restart)
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
# carried the same trap. §14.11 gives each case its own cluster precisely so
# nothing leaks between them; deleting the old keys is what makes that true.
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

# fleet_restart is §14.10's "before every case": stop every worker, wipe the
# per-case state the fakes and the logs carry, start w1..w3 again, wait one
# grace window and re-assert the ownership table.
fleet_restart() { # <case name>
	CASE=$1
	stage restart "fleet restart before case $1 (§14.10)"
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
# Cleanup (§14.12)
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
# Diagnostics (§14.13)
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
# Preflight (§14.4)
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

# fetch_etcd implements the download-and-verify path of §14.2. It is
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
	log "preflight (driver) ok"
}

# preflight_server runs AFTER the start-of-run cleanup: the port check is only
# meaningful once a crashed prior run's processes are gone (§14.4).
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
# Setup (§14.7)
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

	stage layout "create the §14.3 tree and ship the five binaries"
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
		"--initial-cluster dnv-it=http://127.0.0.1:$ETCD_PEER_PORT"
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

sp_rev() { # <sp name>
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
# 0, at any depth — the "err_epoch 0 everywhere" form of §14.11 B8, which a
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
	[ "$(reaction_cnt "$2")" -ge "$1" ]
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
			 | "\(.side_id) \(.revision)"' --arg sp "$1"
	done
}

flip_records_gt() { # <n> <sp_id>
	[ "$(flip_records "$2" | wc -l | tr -d ' ')" -gt "$1" ]
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
# Case S — smoke (§14.11 S)
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

	local before after tail_true
	before=$(reqs dn0 CheckDn)
	log "  counting CheckDn rounds on dn0 over 5s (now $before)"
	sleep 5
	after=$(reqs dn0 CheckDn)
	assert_ge $((after - before)) 3 "dn0: CheckDn rounds in 5s"
	# RW4 sends show_info = false; only a first round may ask for the info.
	tail_true=$(recsr "$(apath dn0)" \
		'select(.msg == "grpc server recv")
		 | select((.method | split("/") | last) == "CheckDn")
		 | select(has("data")) | (.data.show_info // false) | tostring' |
		tail -n +2 | grep -c true || true)
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
		td_created sp0 td0
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
# Case A — revision (§14.11 A)
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

	stage 2 "move-dn: a put, never a delete (§5.5)"
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
	# §14.5 dn_id <-> fake mapping intact (the doc's "put-dn 2 dn2" would
	# either break that mapping or overwrite the DnConf at dn1's endpoint).
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
	[ "$(stream_closes "$1" "$2")" -gt "$3" ]
}

wcount_gt() { # <n> <filter> [jq args…]
	local n=$1
	shift
	[ "$(wcount "$@")" -gt "$n" ]
}

reqs_gt() { # <n> <agent> <method> [<filter>] [jq args…]
	local n=$1
	shift
	[ "$(reqs "$@")" -gt "$n" ]
}

# ---------------------------------------------------------------------------
# Case B — health (§14.11 B)
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

# dn_capacity_gone is HL5's "the capacity key is absent". A `list-keys` that
# FAILED (a dropped ssh, a busy etcd) prints nothing either, and `-z` cannot
# tell the two apart: the exit status is checked first, so a failed read leaves
# the poll running instead of satisfying it.
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

	stage 7 "a plain ERROR meta row sets err_epoch (§9.4)"
	set_behavior dn0 <<'EOF'
{"objects": {"dn": {"rows": {"meta_info": {"status": "ERROR",
  "details": "disk lacks Write Zeroes"}}}}}
EOF
	wait_until "$WAIT_SHORT" "dn 1 err_epoch set by the meta row" dn_err_epoch_set 1
	clear_behavior dn0
	wait_until "$WAIT_SHORT" "dn 1 err_epoch cleared again" dn_err_epoch_clear 1

	stage 8 "the SP object table: cntlr, leg and side err_epochs"
	# The thresholds are deliberately NOT the §14.6 reaction values here: this
	# case injects the very ERROR rows §14.11 D uses as reaction triggers, and
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
# Case C — bitmap (§14.11 C)
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
	ctl put-bitmap --sp sp0 --kind clone --name c0 --bm-idx 0 --hex 0102
	ctl put-bitmap --sp sp0 --kind clone --name c0 --bm-idx 1 --hex 0304
	# The migration chunks go in LAST, and only once the fixture has gone
	# quiet: step 2 asserts that the chunk-1 push carries EXACTLY the SpRev of
	# the put that introduced it (BM3's `revision = synced`), which holds only
	# while nothing else bumps SpRev behind it. Two things still would here —
	# put-migr appends the destination side S3 with `provisioned = false`, so
	# RW18 flips it a round later, and the clone pushes are still in flight —
	# so both are waited out and SpRev is then required to sit still.
	wait_until "$WAIT_SYNCUP" "the migration's destination side provisioned" \
		all_provisioned sp0 1
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap bm_idx 1 (the fixture settling)" \
		req_ge 1 cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "1"'
	local quiet_rev
	quiet_rev=$(sp_rev 1)
	assert_none_for 2 "an SpRev bump before the migration chunks are written" \
		sp_rev_changed 1 "$quiet_rev"
	ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 0 --hex aa00ff
	local migr1_rev
	migr1_rev=$(ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 1 --hex bb11ee |
		"$JQ" -r .sp_rev)

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
	# BM3's `revision = synced`. Step 1 quiesced the fixture before writing
	# the two migration chunks and nothing bumps SpRev behind them, so the
	# chunk-1 push must carry EXACTLY the revision of the put that made the
	# chunk visible — the equality step 4 pins for chunk 2, here too.
	local rev push1_rev
	push1_rev=$(last_req dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "1"' |
		"$JQ" -r '.revision')
	assert_eq "$push1_rev" "$migr1_rev" \
		"the chunk-1 push carries the SpRev of the put that introduced it"
	# `bm_info` rides ONLY on a SyncupSideReply (the proto gives CheckSideReply
	# no such field), and BM6 asks for a re-sync only when a push FAILS — so
	# after a clean push sequence nothing bumps SpRev and no further SyncupSide
	# is issued. Force one re-fan, which is what makes BM2's diff observable:
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

	stage 3 "clone chunks go to the primary's CN only"
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap bm_idx 0" \
		req_ge 1 cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "0"'
	wait_until "$WAIT_SHORT" "cn0: PushCloneBitmap bm_idx 1" \
		req_ge 1 cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "1"'
	assert_eq "$(reqs cn1 PushCloneBitmap)" 0 "cn1 (the standby): clone pushes"

	stage 4 "a new migration chunk is pushed exactly once, at that revision"
	pushes=$(reqs dn1 PushMigrBitmap)
	local migr2_rev
	migr2_rev=$(ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 2 --hex cc22dd |
		"$JQ" -r .sp_rev)
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 2" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "2"'
	assert_eq "$(last_req dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "2"' |
		"$JQ" -r '.revision')" "$migr2_rev" \
		"the chunk-2 push carries the SpRev of the put that introduced it"
	sleep 3
	assert_eq "$(reqs dn1 PushMigrBitmap)" $((pushes + 1)) \
		"dn1: exactly one new migration push"

	stage 5 "a grown clone chunk is re-pushed ([D8], BM5)"
	local clone0 clone1
	clone0=$(reqs cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "0"')
	clone1=$(reqs cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "1"')
	ctl put-bitmap --sp sp0 --kind clone --name c0 --bm-idx 0 --hex 0102030405060708
	wait_until "$WAIT_SHORT" "cn0: a second PushCloneBitmap bm_idx 0, 8 bytes" \
		req_ge $((clone0 + 1)) cn0 PushCloneBitmap \
		'(.bm_idx // 0 | tostring) == "0"'
	assert_ge "$(reqs cn0 PushCloneBitmap \
		'(.bm_idx // 0 | tostring) == "0" and .bitmap == "<8 bytes>"')" 1 \
		"cn0: the re-pushed chunk 0 carries the new length"
	sleep 2
	assert_eq "$(reqs cn0 PushCloneBitmap '(.bm_idx // 0 | tostring) == "1"')" "$clone1" \
		"cn0: chunk 1 was not re-pushed"

	stage 6 "a rejected push arms an equal-revision re-sync (BM6)"
	set_behavior dn1 <<'EOF'
{"objects": {"side 1:1:3": {"reply_code": 1}}}
EOF
	ctl put-bitmap --sp sp0 --kind migr --name m0 --bm-idx 3 --hex dd33cc
	rev=$(sp_rev 1)
	wait_until "$WAIT_SHORT" "dn1: two syncup results at the same revision" \
		wcount_ge 2 'select(.msg == "syncup result")
			| select((.revision | tostring) == $r)
			| select((.side_pointer.side_id // 0 | tostring) == "3")' --arg r "$rev"
	clear_behavior dn1
	wait_until "$WAIT_SHORT" "dn1: PushMigrBitmap bm_idx 3" \
		req_ge 1 dn1 PushMigrBitmap '(.bm_idx // 0 | tostring) == "3"'
	# As in step 2: bm_info rides only on a SyncupSideReply, and once the
	# re-pushed chunk lands nothing bumps SpRev, so the worker correctly issues
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
	wait_until "$WAIT_SHORT" "cn1: PushCloneBitmap bm_idx 0" \
		req_ge $((cn1_before + 1)) cn1 PushCloneBitmap \
		'(.bm_idx // 0 | tostring) == "0"'
	wait_until "$WAIT_SHORT" "cn1: PushCloneBitmap bm_idx 1" \
		req_ge 1 cn1 PushCloneBitmap '(.bm_idx // 0 | tostring) == "1"'
	local cn0_after
	cn0_after=$(reqs cn0 PushCloneBitmap)
	assert_between "$cn0_after" "$cn0_before" $((cn0_before + 1)) \
		"cn0: clone pushes while the primary moved (at most one in flight)"
	assert_none_for 3 "clone pushes to the old primary after the change" \
		reqs_gt "$cn0_after" cn0 PushCloneBitmap
}

# ---------------------------------------------------------------------------
# Case D — reaction (§14.11 D)
# ---------------------------------------------------------------------------

cntlr_is_primary() { [ "$(cntlr_field "$1" "$2" primary)" = true ]; }
cntlr_not_primary() { [ "$(cntlr_field "$1" "$2" primary)" = false ]; }

# cntlr_gone is §14.11 D3's "C1 gone from cntlr_id_list". It reads the LIST
# rather than probing the Cntlr key: `! ctl get-cntlr …` cannot tell "the key
# is not there" from "the read failed", so a dropped ssh would report the
# cntlr gone and the poll would end on it.
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
	# cntlr_unhealthy (4 s) reached and !primary; model.Failover flips Primary
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
	set_behavior cn0 <<EOF
{"objects": {"cntlr 1:1": {"thin_ok": true, "rows": {"ss_id_to_subsystem.$((SS_ID))":
  {"status": "ERROR", "details": "subsystem gone"}}}}}
EOF
	# The negative runs from the instant the row lands: err_epoch is stamped a
	# round later and the threshold is 2s after THAT, so one second in there
	# can be no failover yet, with room to spare.
	assert_none_for 1 "a failover before the 2s primary threshold" \
		reaction_ge $((failovers + 1)) failover
	wait_until "$WAIT_SHORT" "C1 err_epoch set" cntlr_epoch_set sp0 1
	wait_until $((2 + WAIT_SHORT)) "reaction applied kind=failover" \
		reaction_ge $((failovers + 1)) failover
	wait_until "$WAIT_SHORT" "C2 primary true" cntlr_is_primary sp0 2
	assert_eq "$(cntlr_field sp0 1 primary)" false "C1 primary after the failover"
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
	# The CdcEntry was rewritten in the replacement's own STM. Each node's
	# NvmeTrConf carries that node's OWN port as tr_svc_id (workerctl), so the
	# list is attributable per CN and §14.11 D3's "lists cn 2 and cn 3, not
	# cn 1" is checkable as a SET. The expectation is also derived from what
	# the SP actually holds, because put-ss appends one entry per cntlr with
	# no dedupe: the two must agree.
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
	# Exactly two eligible DNs for a two-leg group (§14.5).
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
	# 125 <= 125 and no second grow may start. (The doc's illustrative
	# 600/2048 numbers cannot express this at the real §3.6 geometry.)
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
	# default, so AR8 step 1's switch can land before the assertion runs, and
	# spare_leg_list[0] is then the PARKED OLD LEG (§0 item 17), not the spare.
	legs_before=$(group_legs sp0 1 data 2 | awk '{ print $1 }' | sort -n | tr '\n' ' ')
	set_behavior "$prim_dir" <<EOF
{"objects": {"cntlr 1:$prim_id": {"thin_ok": true, "rows": {"leg_id_to_leg.3":
  {"status": "ERROR", "details": "no healthy path"}}}}}
EOF
	wait_until "$WAIT_SHORT" "L3 err_epoch set" leg_epoch_set sp0 1 3
	local l3_epoch
	l3_epoch=$(leg_epoch sp0 1 3)
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
	assert_ne "$(spare_legs sp0 1 data 2 | awk '{ print $4 }')" 0 \
		"the parked L3 keeps its err_epoch"
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
	# black-listed and the other cntlr's CN is excluded by §6.4), still has a
	# zero budget from step 4. So the order is load-bearing: disable FIRST,
	# then hand cn 1 a budget. The assertion that follows then really tests
	# AR3's "a disabled cntlr is never replaced" and not an empty candidate
	# set.
	ctl set-cntlr --sp sp0 --id "$prim_id" --disabled=true
	ctl set-free cn --id 1 --free-ext 64
	replaces=$(reaction_cnt replace_cntlr)
	assert_none_for 6 "a replacement of the disabled standby" \
		reaction_ge $((replaces + 1)) replace_cntlr
}

reaction_skip_ge() { # <n> <kind> <reason>
	[ "$(reaction_skip_cnt "$2" "$3")" -ge "$1" ]
}

# g2_repair_progressed reports whether ANY further AR8 step ran on sp0's data
# group G2 — another switch, another allocation, or another parked leg. §14.11
# case D step 7's "no further repair of L3" is all three.
g2_repair_progressed() { # <spare_switch cnt> <spare_create cnt> <spare cnt>
	if [ "$(reaction_cnt spare_switch)" -gt "$1" ]; then return 0; fi
	if [ "$(reaction_cnt spare_create)" -gt "$2" ]; then return 0; fi
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
# Case E — vote (§14.11 E)
# ---------------------------------------------------------------------------

membership_committed() { # <w> <seed> <state> [role]
	local role=${4:-dn}
	[ "$(count_recs "$(wpath "$1")" \
		'select(.msg == "membership committed")
		 | select(.seed == $s and .state == $st and .role == $r)' \
		--arg s "$2" --arg st "$3" --arg r "$role")" -ge 1 ]
}

membership_committed_all_roles() { # <w> <seed> <state>
	local role
	for role in dn cn sp; do
		membership_committed "$1" "$2" "$3" "$role" || return 1
	done
	return 0
}

membership_observed() { # <w> <seed> <state>
	[ "$(count_recs "$(wpath "$1")" \
		'select(.msg == "membership observed")
		 | select(.seed == $s and .state == $st)' \
		--arg s "$2" --arg st "$3")" -ge 1 ]
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
	[ "$(count_recs "$(wpath "$1")" \
		'select(.msg == "membership committed")
		 | select(.seed == $s and .state == "member")
		 | select((.member_cnt | tostring) == $c)' \
		--arg s "$2" --arg c "$3")" -ge 1 ]
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
	# requiring one would fail a third of all runs. So the loop now asserts
	# something for EVERY DN instead of skipping the unmoved ones: a shard
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
			# nobody else's. Here we only pin the old owner's syncup.
			wait_until "$WAIT_MEMBERSHIP" "$dir: CheckDn rounds from the new owner $new" \
				trace_req_ge 1 "$dir" CheckDn "$new8"
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
	# must come from the shard's CURRENT owner and nobody else.
	local cutoff
	cutoff=$(ts_epoch "$(server_now)")
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
	# The bounds are DERIVED, not the doc's illustrative 8/9 pair.
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
	# = [8, 10] s at the §14.6 timers. The doc's ">= 9 s" silently assumes the
	# kill lands right after a heartbeat; a kill landing just BEFORE the next
	# one is equally legal and produced 8.372 s here.
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
	# §14.11 E8 leaves w2 and w4 to "the fleet restart of the next case", and
	# §14.10's fleet restart starts w1 w2 w3 — the §14.3 baseline. So w2 comes
	# back and w4 deliberately does NOT: w4 is case E's own extra worker, and
	# every later case asserts its ownership tables over the three-worker
	# fleet. fleet_restart's stop loop covers w4 anyway (it is a no-op for a
	# worker this script already knows is down).
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
	[ "$(count_recs "$(wpath "$1")" \
		'select(.msg == "worker fenced")
		 | select(.old_seed == $s and .reason == "heartbeat_stalled")' \
		--arg s "$2")" -ge 1 ]
}

trace_reqs() { # <agent> <method> <seed8>
	count_recs "$(apath "$1")" \
		"select(.msg == \"grpc server request\" or .msg == \"grpc server recv\")
		 | select((.method | split(\"/\") | last) == \"$2\")
		 | select((.trace_id | split(\"-\")[0]) == \$s)" --arg s "$3"
}

trace_req_ge() { # <n> <agent> <method> <seed8>
	[ "$(trace_reqs "$2" "$3" "$4")" -ge "$1" ]
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
# Case F — handoff (§14.11 F)
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
	# revision other than 1.
	wait_until "$WAIT_MEMBERSHIP" "dn0: CheckDn rounds from the new owner" \
		trace_req_ge 1 dn0 CheckDn "$new8"
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
	# §14.11 F2 asks for "exactly one flip record and SpRev advanced by
	# exactly one" when both sides finish together, but RW18 only says several
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
	[ "$(flip_cnt "$2")" -gt "$1" ]
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
