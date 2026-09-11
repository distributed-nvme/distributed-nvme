#!/usr/bin/env bash
#
# gateway_test.sh — the `dnv-gateway` integration test of doc/gateway.md §10.
# One server, a real single-node etcd, THREE real dnv-gateway instances and the
# fake dn/cn agents of dnv-worker.md §14.9, driven from this machine over ssh
# by integtest/gatewayctl (the gRPC driver) with etcd ground truth read back
# through integtest/workerctl (read-only, plus the two §2.4 worker flips).
#
#   bash integtest/gateway_test.sh [--only <case>] [--cleanup-only] user@ip
#
# Cases (§10.13-§10.15), in order, each against a WIPED store: smoke, parallel,
# contention, faults, restart. Cleanup runs unconditionally at the start and,
# on success only, at the end: a failing run leaves etcd's data, every log and
# every behavior file in place and dumps the §10.17 diagnostics.
#
# NO SUDO anywhere: nothing in this suite needs root. Everything the script
# creates lives under $WORK on the server, and cleanup removes exactly that.
#
# What it proves (§10.1): (a) every one of the 59 RPCs maintains exactly the
# §5 etcd schema, verified against ground truth that never goes through the
# code under test; (b) parallel gRPCs across instances are correct — disjoint
# work all succeeds, contended work has exactly one winner and no partial
# writes; (c) refusals of every class write nothing, provably, via before/after
# store-revision brackets; (d) instances are stateless — one can be killed
# mid-load and restarted with no cleanup and no corruption. Correctness only:
# nothing here asserts latency or throughput.
#
# Log reading: every gateway/agent log is one JSON object per line, and a log
# being appended to while we `cat` it can end in a torn line, so every read
# goes through `jq -R 'fromjson? // empty'` (recs/recsr below) — a partial last
# line is dropped, never fails an assertion.
#
# Signals: the three gateways share one command line, so every process's pid is
# recorded from `$!` into $WORK/<dir>/pid at launch and every signal goes by
# pid. The `pkill -f` forms appear in cleanup() only.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (§10.3, §10.5, §10.6)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
CACHE_DIR="$BIN_DIR/cache"
GATEWAY_BIN="$REPO_ROOT/bin/dnv-gateway"
GATEWAYCTL_BIN="$BIN_DIR/gatewayctl"
WORKERCTL_BIN="$BIN_DIR/workerctl"
FAKEAGENT_BIN="$BIN_DIR/fakeagent"

# The pinned etcd release — the same block, the same cache and the same digest
# as worker_test.sh, so the two suites share one download.
ETCD_VERSION=v3.6.14
ETCD_DIST="etcd-$ETCD_VERSION-linux-amd64"
ETCD_URL="https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/$ETCD_DIST.tar.gz"
ETCD_SHA256=ffe840ff9295808e88cce2794a18a5ac87f12a5203c8314d0bf6aa119b41bac5
ETCD_TAR="$CACHE_DIR/$ETCD_DIST.tar.gz"

WORK=/var/tmp/dnv-gateway-integtest

# §10.3 ports: a fresh block, disjoint from every other suite (worker
# 12379/29600s/29700s, cdc 13379/18009-12/14420-23, agent suites 29528/29529,
# production 29527/2379).
ETCD_CLIENT_PORT=15379
ETCD_PEER_PORT=15380
GW_PORT_BASE=29810  # gw0..gw2 -> 29810..29812
DN_PORT_BASE=29820  # dn0..dn3 -> 29820..29823
CN_PORT_BASE=29830  # cn0..cn2 -> 29830..29832
ALL_PORTS=(15379 15380 29810 29811 29812 29820 29821 29822 29823 29830 29831 29832)

# common.DnvPrefix — the first field of every dnv etcd key (§5.1).
DNV_PREFIX=dnv

# §10.5 identity plan.
CLUSTER=itgw
GW_DIRS=(gw0 gw1 gw2)
DN_DIRS=(dn0 dn1 dn2 dn3)
CN_DIRS=(cn0 cn1 cn2)
NQN_PREFIX=nqn.2025-01.io.dnv

# §10.6 test-time constants.
DN_SIZE=68719476736 # 64 GiB = 64 extents at the default 1 GiB extent_size
DN_EXTENTS=64
CN_EXTENTS=4096 # DefaultCnCap 4 TiB / 1 GiB
SP_CNTLR_CNT=2
SP_SLICE_CNT=1
SP_INIT_EXT=2
SP_EXT_COST=6 # meta 1x2 legs + data 2x2 legs
TD_SIZE=67108864
PAR_CLIENTS=10
RACE_N=8
WAIT_SHORT=5
RETRY_BUDGET=20

CASES=(smoke parallel contention faults restart)

# ---------------------------------------------------------------------------
# Mutable state
# ---------------------------------------------------------------------------

TARGET=""
IP=""
JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE=setup
CID=""
TRACE=it-setup
STAGE="(startup)"
SETUP_DONE=0
QUIET=0
# GATEWAY is the instance the plain `gw` wrapper talks to; cases that need a
# specific instance use gw_at.
GATEWAY="127.0.0.1:$GW_PORT_BASE"
# SP_REV caches the current SpRev of the SP the case is working on. §10.10:
# every sp_rev-consuming stage runs in the PARENT shell, never in a subshell,
# or the refreshed token is lost with the subshell that fetched it.
SP_REV=0

declare -A PID=()     # dir -> pid recorded from $! at launch
declare -A RUNNING=() # dir -> 1 while we believe the process is alive

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling (§10.2, verbatim from worker_test.sh)
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the §10.10 trace id
# `it-<case>-<step>`, which every gatewayctl and workerctl call of the step
# then stamps on its records — the thread that ties a script stage to the
# gateway's, the agents' and etcd's log lines.
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
assert_ge() { [ "$1" -ge "$2" ] || die "$3: got $1, want >= $2"; }

assert_between() {
	{ [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]; } ||
		die "$4: got $1, want $2..$3"
}

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

# assert_field reads one field out of a driver reply. protojson renders 64-bit
# fields as JSON STRINGS and 32-bit ones as numbers, so every numeric
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
		log "########## diagnostics (§10.17) ##########"
		diagnostics || true
		log ""
		log "debris left in place on $TARGET; failing stage '$STAGE'"
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution (§10.8, §10.9)
# ---------------------------------------------------------------------------

# sshw echoes the command and runs it on the server as the plain user. QUIET is
# raised while wait_until polls, so a poll does not bury the transcript.
sshw() {
	local cmd="$*"
	[ "$QUIET" -eq 1 ] || log "[server] $cmd"
	ssh "${SSH_OPTS[@]}" "$TARGET" "$cmd"
}

sshw_ok() { sshw "$@" || true; }

# gw is the §10.8 driver wrapper: every call carries the gateway endpoint, the
# cluster and the stage's trace id. Each argument is quoted for the REMOTE
# shell with printf %q before the command string is built — without that an
# argument containing a space (an etcd key is space-joined, §5.1) is re-split
# by the remote shell.
gw() { gw_at "$GATEWAY" "$@"; }

# gw_at is gw against one named instance: case A round-robins waves over the
# three gateways and case D drives a specific survivor.
gw_at() { # <host:port> <args…>
	local endpoint=$1
	shift
	local quoted
	quoted=$(printf '%q ' "$@")
	sshw "$WORK/bin/gatewayctl --gateway $endpoint --cluster $CLUSTER" \
		"--trace-id $TRACE $quoted"
}

# gwx asserts a REFUSAL: the call must come back with exactly that gRPC code.
# gatewayctl exits 0 on the expected code and 1 on any other outcome, so `set
# -e` catches a wrong success and a wrong failure alike.
gwx() { # <UPPER_SNAKE code> <args…>
	local code=$1
	shift
	gw --expect "$code" "$@"
}

gwx_at() { # <host:port> <UPPER_SNAKE code> <args…>
	local endpoint=$1 code=$2
	shift 2
	gw_at "$endpoint" --expect "$code" "$@"
}

# wctl is the ground-truth reader (§10.9): the raw decoded etcd state, read
# back by a binary that is NOT the code under test. Its only writes in this
# suite are set-created and set-provisioned (§2.4).
wctl() {
	local quoted
	quoted=$(printf '%q ' "$@")
	sshw "$WORK/bin/workerctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--cluster $CLUSTER --trace-id $TRACE $quoted"
}

# etcdctl_raw runs the server's etcdctl; the wipe and the store-revision probe
# are its only users.
etcdctl_raw() {
	sshw "$WORK/bin/etcdctl --endpoints 127.0.0.1:$ETCD_CLIENT_PORT $*"
}

# rlog is the §10.17 log reader: `ssh … cat <path>`, so every assertion is
# parsed by the driver's jq and the server needs no jq of its own. A missing
# file yields empty output rather than a failure, because `set -o pipefail`
# would otherwise turn "the log does not exist yet" into a script abort inside
# a polling predicate. A read that fails for any reason OTHER than "the file
# does not exist yet" must NOT read as an empty log, or every count-based
# negative assertion would pass on a broken ssh — so probe first and let a real
# failure propagate.
rlog() {
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"if [ -e $(printf '%q' "$1") ]; then cat -- $(printf '%q' "$1"); fi"
}

gpath() { printf '%s/%s/gateway.log' "$WORK" "$1"; }
apath() { printf '%s/%s/agent.log' "$WORK" "$1"; }

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

# ---------------------------------------------------------------------------
# Polling (§10.10: only for process readiness — the gateway is synchronous, so
# no stage ever sleeps waiting for etcd content)
# ---------------------------------------------------------------------------

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

# ---------------------------------------------------------------------------
# Identity helpers (§10.5)
# ---------------------------------------------------------------------------

dn_dir() { printf 'dn%d' "$1"; }   # 0-based
cn_dir() { printf 'cn%d' "$1"; }   # 0-based
gw_dir() { printf 'gw%d' "$1"; }   # 0-based
dn_addr() { printf '127.0.0.1:%d' $((DN_PORT_BASE + $1)); }
cn_addr() { printf '127.0.0.1:%d' $((CN_PORT_BASE + $1)); }
gw_addr() { printf '127.0.0.1:%d' $((GW_PORT_BASE + $1)); }
dn_port() { printf '%d' $((DN_PORT_BASE + $1)); }
cn_port() { printf '%d' $((CN_PORT_BASE + $1)); }
# Fake transport service ids: tcp/ipv4/127.0.0.1/44<n> (§10.5).
dn_svcid() { printf '442%d' "$1"; }
cn_svcid() { printf '443%d' "$1"; }
dn_loc() { printf 'rack%d' "$1"; }
# CN locations are distinct too: the §6.4 CN scan location-dedupes, so three
# CNs sharing a location would make a two-cntlr SP unallocatable.
cn_loc() { printf 'rack%d' "$1"; }

# ---------------------------------------------------------------------------
# Ground-truth readers (§10.9)
# ---------------------------------------------------------------------------

# store_rev is the etcd store revision — the exact "was anything written?"
# probe the §10.10 refusal brackets need. etcd advances it only for a
# transaction that actually mutates the store, so an RPC that refuses (or an
# idempotent Update* that writes nothing, §0 #17) leaves it untouched.
store_rev() {
	local saved=$QUIET out
	QUIET=1
	out=$(etcdctl_raw "endpoint status -w json")
	QUIET=$saved
	jq_of "$out" '.[0].Status.header.revision'
}

# assert_no_write brackets a refusal: it captures the store revision, runs the
# command, and requires the revision to be unchanged. This is what makes
# "refusals write nothing" provable rather than asserted (§10.10).
assert_no_write() { # <label> <cmd…>
	local label=$1
	shift
	local before after
	before=$(store_rev)
	"$@"
	after=$(store_rev)
	assert_eq "$after" "$before" "$label: store revision must not move"
}

# key_count counts the keys under a prefix — the non-SP-scoped form of the
# refusal bracket, and the §10.11 step 17 "nothing left" audit.
#
# A bare §5.3 kind ("dn_capacity", "cdc", "sp_conf") is scoped to $CLUSTER by
# workerctl, so it needs the cluster to exist; "dnv" scans the WHOLE store and
# is the form to use once the cluster is gone. A failed read degrades to 0
# rather than aborting the stage, which is what makes the post-teardown audit
# expressible either way.
key_count() { # <prefix>
	local saved=$QUIET out
	QUIET=1
	out=$(wctl list-keys --prefix "$1" | grep -c . || true)
	QUIET=$saved
	printf '%s' "$out"
}

sp_json() { wctl get-sp --sp "$1"; }
dn_json() { wctl get-dn --addr "$1"; }
cn_json() { wctl get-cn --addr "$1"; }

sp_shard() { jq_of "$(sp_json "$1")" '.sp_conf.shard_code'; }
sp_id_of() { jq_of "$(sp_json "$1")" '.sp_conf.sp_id'; }
dn_free() { jq_of "$(dn_json "$1")" '.free_ext_cnt'; }
cn_free() { jq_of "$(cn_json "$1")" '.free_ext_cnt'; }
dn_id_of() { jq_of "$(dn_json "$1")" '.dn_id'; }
cn_id_of() { jq_of "$(cn_json "$1")" '.cn_id'; }

# sp_rev_of reads the SP's current revision through the GATEWAY's own read
# path, which is where §10.10 says the script refreshes its cached token from.
sp_rev_of() { # <sp name>
	jq_of "$(gw get-sp --sp "$1")" '.sp_rev.revision'
}

# refresh_rev updates the cached SP token. It MUST run in the parent shell.
refresh_rev() { SP_REV=$(sp_rev_of "$1"); }

dn_rev_of() { jq_of "$(gw get-dn --addr "$1")" '.dn_rev.revision'; }
cn_rev_of() { jq_of "$(gw get-cn --addr "$1")" '.cn_rev.revision'; }

# raw_key prints one etcd value as protojson, for the assertions that name a
# key literally (§10.11 step 1).
raw_key() { wctl get --key "$1"; }

cluster_key() { printf '%s cluster_conf %s' "$DNV_PREFIX" "$CLUSTER"; }

# ---------------------------------------------------------------------------
# Fake-agent behaviour and state (dnv-worker.md §14.9, reused here)
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

reset_state() { # <agent>
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"printf '%s' '{\"objects\":{}}' > $WORK/$1/state.json.tmp &&
		 mv -f $WORK/$1/state.json.tmp $WORK/$1/state.json"
}

# set_state writes one fake's whole state.json from stdin.
#
# It exists because two gateway RPCs — DeleteClone and FinishMigration with
# force = false — read an *Info the fake derives from the LAST Syncup* request
# it applied, and this suite runs no dnv-worker, so no Syncup* ever arrives.
# Hand-writing the state is the fake's documented operator-edit path (§14.9:
# state.json is protojson so it stays human-editable), and it is what lets the
# suite present a cntlr or a side that already knows about a clone or a
# migration destination.
set_state() { # <agent>   (JSON on stdin)
	local agent=$1
	[ "$QUIET" -eq 1 ] || log "[server] state -> $agent"
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"cat > $WORK/$agent/state.json.tmp &&
		 mv -f $WORK/$agent/state.json.tmp $WORK/$agent/state.json"
}

reset_fakes() {
	local d
	for d in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		clear_behavior "$d"
		reset_state "$d"
	done
}

# ---------------------------------------------------------------------------
# Process control (§10.3): launch with >> so an external truncation resets the
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

proc_gone() { ! sshw "kill -0 \$(cat $WORK/$1/pid) 2>/dev/null"; }

wait_gone() { # <dir> <secs>
	wait_until "$2" "$1 to exit" proc_gone "$1"
	unset "RUNNING[$1]"
}

start_fake() { # <kind dn|cn> <dir> <addr> [extra…]
	local kind=$1 dir=$2 addr=$3
	shift 3
	remote_start "$dir" agent.log \
		"$WORK/bin/fakeagent $kind --grpc-address $addr --dir $WORK/$dir" "$@"
}

# gw_ready is the §10.4 readiness probe of one gateway instance: a `ping`,
# which is a ListClusters of count 1 and therefore proves the instance serves
# without needing any state to exist.
gw_ready() { # <index>
	local saved=$QUIET
	QUIET=1
	if gw_at "$(gw_addr "$1")" ping >/dev/null 2>&1; then
		QUIET=$saved
		return 0
	fi
	QUIET=$saved
	return 1
}

start_gw() { # <index>
	local dir
	dir=$(gw_dir "$1")
	remote_start "$dir" gateway.log \
		"$WORK/bin/dnv-gateway --grpc-network tcp" \
		"--grpc-address $(gw_addr "$1")" \
		"--etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT"
}

stop_gw() { # <index>
	local dir
	dir=$(gw_dir "$1")
	[ "${RUNNING[$dir]-}" = 1 ] || return 0
	sig_dir "$dir" TERM
	wait_gone "$dir" "$WAIT_SHORT"
}

kill_gw() { # <index>   -- SIGKILL, no drain (case D)
	local dir
	dir=$(gw_dir "$1")
	sig_dir "$dir" KILL
	wait_gone "$dir" "$WAIT_SHORT"
}

truncate_logs() {
	sshw "for f in $WORK/gw?/gateway.log $WORK/dn?/agent.log $WORK/cn?/agent.log; do" \
		"[ -f \"\$f\" ] && : > \"\$f\"; done; true"
}

# wipe_etcd removes every dnv key while the gateways are stopped, so each case
# starts against a pristine store (§10.5 per-case reset). It is the only
# non-workerctl write this suite performs.
wipe_etcd() {
	etcdctl_raw "del --prefix '$DNV_PREFIX '" >/dev/null
}

# case_reset is §10.5's per-case reset: stop gw0..2 by their recorded pids,
# wipe the store, reset every fake, truncate the logs, start gw0..2 again and
# ping all three.
#
# The gateways are stateless (§0 #3), so this is only about isolating the
# CASES from each other — there is nothing in a gateway to reset.
case_reset() { # <case name>
	CASE=$1
	stage reset "per-case reset before case $1 (§10.5)"
	local i
	for i in 0 1 2; do stop_gw "$i"; done
	wipe_etcd
	reset_fakes
	truncate_logs
	SP_REV=0
	CID=""
	for i in 0 1 2; do start_gw "$i"; done
	for i in 0 1 2; do
		wait_until "$WAIT_SHORT" "gw$i to answer a ping" gw_ready "$i"
	done
}

# ---------------------------------------------------------------------------
# Shared case helpers
# ---------------------------------------------------------------------------

# new_cluster creates the suite's cluster THROUGH THE GATEWAY (§10.5: never
# through workerctl put-cluster) and caches its cluster_id.
new_cluster() {
	local out
	out=$(gw create-cluster)
	CID=$(jq_of "$out" '.cluster_id')
	assert_ne "$CID" "0" "create-cluster cluster_id"
	log "  cluster $CLUSTER, cluster_id $CID"
}

# make_nodes creates the four DNs and the three CNs of §10.5 sequentially and
# asserts nothing beyond the reply; the cases that care assert the write set
# themselves.
make_nodes() {
	local i
	for i in 0 1 2 3; do
		gw create-dn --addr "$(dn_addr "$i")" --location "$(dn_loc "$i")" \
			--tr-svc-id "$(dn_svcid "$i")" >/dev/null
	done
	for i in 0 1 2; do
		gw create-cn --addr "$(cn_addr "$i")" --location "$(cn_loc "$i")" \
			--tr-svc-id "$(cn_svcid "$i")" >/dev/null
	done
}

# make_sp creates one SP with the §10.6 shape and refreshes the cached token.
make_sp() { # <sp name>
	gw create-sp --sp "$1" --cntlr-cnt "$SP_CNTLR_CNT" \
		--slice-cnt "$SP_SLICE_CNT" --init-ext-cnt "$SP_INIT_EXT" --raid1
	refresh_rev "$1"
}

# sp_first_slice / sp_first_data_grp / sp_first_data_leg / sp_first_data_side
# pull one id out of the SP's stored slices. They exist because every id below
# the SP is minted by the gateway, so the script can only learn them by reading
# them back.
sp_first_slice() { # <sp>
	jq_of "$(sp_json "$1")" '.sp_conf.slice_id_list[0]'
}

sp_grp_id() { # <sp> <meta|data> <index>
	jq_of "$(sp_json "$1")" \
		".slices | to_entries[0].value.${2}_grp_list[$3].grp_id"
}

sp_leg_id() { # <sp> <meta|data> <grp index> <leg index>
	jq_of "$(sp_json "$1")" \
		".slices | to_entries[0].value.${2}_grp_list[$3].leg_list[$4].leg_id"
}

sp_side_id() { # <sp> <meta|data> <grp index> <leg index>
	jq_of "$(sp_json "$1")" \
		".slices | to_entries[0].value.${2}_grp_list[$3].leg_list[$4].side_list[0].side_id"
}

sp_side_addr() { # <sp> <meta|data> <grp index> <leg index>
	jq_of "$(sp_json "$1")" \
		".slices | to_entries[0].value.${2}_grp_list[$3].leg_list[$4].side_list[0].addr_port"
}

# sp_primary_cntlr prints "<cntlr_id decimal> <addr_port>" of the SP's primary.
sp_primary_cntlr() { # <sp>
	local out
	out=$(gw get-sp --sp "$1")
	jq_of "$out" '
		[ .sp_conf.cntlr_id_list, .cntlr_list ]
		| transpose[]
		| select(.[1].primary)
		| "\(.[0]) \(.[1].addr_port)"'
}

# sp_side_dn_addrs lists every distinct DN addr_port an SP occupies, one per
# line — the accounting the §10.11/§10.12 free-count assertions walk.
sp_side_dn_addrs() { # <sp>
	jq_of "$(sp_json "$1")" '
		[ .slices[]
		  | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?)
		  | .side_list[]? | .addr_port ] | unique[]'
}

# verify_sp is the §5.4 write-set verifier of a freshly created standard SP
# (§10.6 shape). Case S step 6 asserts it in full, and cases A and D reuse it
# for every SP they create, which is what makes "either the complete write set
# or no key at all" (§10.15 step 3) a mechanical check.
verify_sp() { # <sp name>
	local sp=$1 out shard spId
	out=$(sp_json "$sp")
	assert_eq "$(jq_of "$out" '.missing | length')" "0" \
		"$sp: no listed key may be missing"
	spId=$(jq_of "$out" '.sp_conf.sp_id')
	assert_ne "$spId" "0" "$sp: sp_id"
	shard=$(jq_of "$out" '.sp_conf.shard_code')

	assert_eq "$(jq_of "$out" '.sp_conf.cntlr_id_list | length')" \
		"$SP_CNTLR_CNT" "$sp: cntlr count"
	assert_eq "$(jq_of "$out" '.sp_conf.slice_id_list | length')" \
		"$SP_SLICE_CNT" "$sp: slice count"
	assert_field "$out" '.sp_conf.next_dev_id' "1" "$sp: next_dev_id"
	assert_field "$out" '.sp_conf.deleting' "false" "$sp: deleting"
	assert_eq "$(jq_of "$out" '.sp_conf.td_name_list | length')" "0" \
		"$sp: td_name_list"

	# sp_id_to_name and sp_rev exist and agree with the SpConf.
	assert_field "$(wctl get --key \
		"$DNV_PREFIX sp_id_to_name $(printf '%016x' "$CID") $(printf '%016x' "$spId")")" \
		'.sp_name' "$sp" "$sp: sp_id_to_name"
	assert_field "$(wctl get-rev sp --id "$spId" --shard \
		"$(printf '%02x' "$shard")")" '.sp_name' "$sp" "$sp: sp_rev.sp_name"

	# Exactly one primary among the cntlrs, all slots distinct.
	assert_eq "$(jq_of "$out" '[.cntlrs[] | select(.primary)] | length')" "1" \
		"$sp: exactly one primary cntlr"
	assert_eq "$(jq_of "$out" '[.cntlrs[].cntlid_slot] | unique | length')" \
		"$SP_CNTLR_CNT" "$sp: distinct cntlid slots"

	# One slice: a meta group of 1 extent and a data group of init_ext_cnt,
	# two legs each, every side unprovisioned, all four legs on distinct DNs.
	local slice
	slice=$(jq_of "$out" '.slices | to_entries[0].value | @json')
	assert_eq "$(jq_of "$slice" '.meta_grp_list | length')" "1" \
		"$sp: meta group count"
	assert_eq "$(jq_of "$slice" '.data_grp_list | length')" "1" \
		"$sp: data group count"
	assert_field "$slice" '.meta_grp_list[0].ext_cnt' "1" "$sp: meta ext_cnt"
	assert_field "$slice" '.data_grp_list[0].ext_cnt' "$SP_INIT_EXT" \
		"$sp: data ext_cnt"
	assert_eq "$(jq_of "$slice" \
		'[ (.meta_grp_list[], .data_grp_list[]) | .leg_list | length ] | unique | @json')" \
		'[2]' "$sp: two legs per group (raid1)"
	assert_eq "$(jq_of "$slice" \
		'[ (.meta_grp_list[], .data_grp_list[]) | .leg_list[] | .side_list[] | .provisioned ] | unique | @json')" \
		'[false]' "$sp: every side unprovisioned"
	assert_eq "$(jq_of "$slice" \
		'[ (.meta_grp_list[], .data_grp_list[]) | .leg_list[] | .side_list[] | .addr_port ] | unique | length')" \
		"4" "$sp: four legs on four distinct DNs"
}

# ---------------------------------------------------------------------------
# Cleanup (§10.16)
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
pkill -f 'bin/dnv-gateway' 2>/dev/null || true
pkill -f 'bin/fakeagent' 2>/dev/null || true
pkill -f 'etcd --name dnv-gw-it' 2>/dev/null || true
sleep 0.5
rm -rf $WORK
echo cleaned
EOF
}

cleanup() {
	log "[server] cleanup: signal every recorded pid, then rm -rf $WORK"
	cleanup_script | ssh "${SSH_OPTS[@]}" "$TARGET" "bash -s" || true
	PID=()
	RUNNING=()
}

# ---------------------------------------------------------------------------
# Diagnostics (§10.17)
# ---------------------------------------------------------------------------

diagnostics() {
	local saved=$QUIET
	QUIET=1
	log "failing stage: '$STAGE'   trace_id: $TRACE   cluster: $CLUSTER  cid: $CID"

	log ""
	log "--- last 50 log lines per process ---"
	local d
	log "  ### etcd"
	rlog "$WORK/etcd/etcd.log" | tail -n 50 >&2 || true
	for d in "${GW_DIRS[@]}"; do
		log "  ### $d"
		recs "$(gpath "$d")" 'select(.msg != "etcd get")' | tail -n 50 >&2 || true
	done
	for d in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		log "  ### $d"
		recs "$(apath "$d")" 'select(.msg | startswith("grpc server"))' |
			tail -n 50 >&2 || true
	done

	log ""
	log "--- workerctl list-keys --prefix dnv ---"
	wctl list-keys --prefix dnv >&2 || true

	log ""
	log "--- workerctl get --key \"$(cluster_key)\" ---"
	wctl get --key "$(cluster_key)" >&2 || true

	log ""
	log "--- ss -ltn (the twelve ports) ---"
	sshw_ok "ss -ltn | grep -E ':(${ALL_PORTS[0]}$(printf '|%s' "${ALL_PORTS[@]:1}"))\\b' || true" >&2

	log ""
	log "--- ps -ef | grep \$WORK ---"
	sshw_ok "ps -ef | grep -F '$WORK' | grep -v grep || true" >&2

	log ""
	log "pull the failing stage's records with:"
	for d in "${GW_DIRS[@]}"; do
		log "  jq 'select(.trace_id==\"$TRACE\")' $(gpath "$d")"
	done
	for d in "${DN_DIRS[@]}" "${CN_DIRS[@]}"; do
		log "  jq 'select(.trace_id==\"$TRACE\")' $(apath "$d")"
	done
	QUIET=$saved
}

# ---------------------------------------------------------------------------
# Preflight (§10.4)
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

# fetch_etcd is worker_test.sh's block verbatim, sharing the same cache: a
# cached tarball whose sha256 already matches the pin is never re-downloaded.
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
	[ -x "$GATEWAY_BIN" ] || die "missing: $GATEWAY_BIN after make build"
	log "  building integtest/bin/{gatewayctl,workerctl,fakeagent}"
	local drv
	for drv in gatewayctl workerctl fakeagent; do
		(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
			go build -o "$BIN_DIR/$drv" "./integtest/$drv" >&2) ||
			die "building $drv failed"
	done
	log "preflight (driver) ok"
}

# preflight_server runs AFTER the start-of-run cleanup: the port check is only
# meaningful once a crashed prior run's processes are gone (§10.4).
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
# Setup (§10.7)
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

	stage layout "create the §10.3 tree and ship the six binaries"
	local dirs=("bin" "etcd" "${GW_DIRS[@]}" "${DN_DIRS[@]}" "${CN_DIRS[@]}")
	sshw "mkdir -p $(printf "$WORK/%s " "${dirs[@]}")"
	SETUP_DONE=1
	log "[server] scp etcd etcdctl dnv-gateway gatewayctl workerctl fakeagent -> $WORK/bin"
	scp -q "${SSH_OPTS[@]}" \
		"$CACHE_DIR/$ETCD_DIST/etcd" "$CACHE_DIR/$ETCD_DIST/etcdctl" \
		"$GATEWAY_BIN" "$GATEWAYCTL_BIN" "$WORKERCTL_BIN" "$FAKEAGENT_BIN" \
		"$TARGET:$WORK/bin/" || die "scp of the binaries failed"
	sshw "chmod 0755 $WORK/bin/*"

	stage etcd "start etcd and wait for a workerctl ping"
	remote_start etcd etcd.log \
		"$WORK/bin/etcd --name dnv-gw-it --data-dir $WORK/etcd" \
		"--listen-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--advertise-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--listen-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-advertise-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster dnv-gw-it=http://127.0.0.1:$ETCD_PEER_PORT"
	wait_until "$WAIT_SHORT" "etcd to answer a workerctl ping" etcd_reachable

	stage fakes "start four fake DNs and three fake CNs with an empty behavior"
	local i d
	for i in 0 1 2 3; do
		d=$(dn_dir "$i")
		clear_behavior "$d"
		reset_state "$d"
		start_fake dn "$d" "$(dn_addr "$i")" "--size $DN_SIZE"
	done
	for i in 0 1 2; do
		d=$(cn_dir "$i")
		clear_behavior "$d"
		reset_state "$d"
		# --size 0 is deliberate (§10.6): GetCnSize then replies 0, which
		# is what makes the gateway apply §6.1's "0 => DefaultCnCap 4 TiB"
		# and give every CN 4096 extents. The fakeagent's own default is
		# 1 TiB, which would silently test a different mapping.
		start_fake cn "$d" "$(cn_addr "$i")" "--size 0"
	done
	wait_until "$WAIT_SHORT" "the seven fake-agent ports" ports_up \
		29820 29821 29822 29823 29830 29831 29832

	stage gateways "start gw0, gw1, gw2 and ping each one"
	for i in 0 1 2; do start_gw "$i"; done
	for i in 0 1 2; do
		wait_until "$WAIT_SHORT" "gw$i to answer a ping" gw_ready "$i"
	done
}

# ---------------------------------------------------------------------------
# CASES — the five case functions are spliced in below this marker
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# Case S — smoke (§10.11): the full-lifecycle sweep, sequential, gw0 only
# ---------------------------------------------------------------------------
#
# One stage per numbered step of §10.11, in order. Every mutating stage does
# the three things §10.10 asks for: the gatewayctl call with its expected code,
# `wctl` ground-truth assertions on exact fields, and the gateway's own read
# RPC as the secondary check. Every refusal is bracketed with assert_no_write,
# which is what makes "refusals write nothing" provable rather than asserted.
#
# The local helpers below are all `smoke_`-prefixed so they cannot collide with
# the other four cases spliced into this same script.

# smoke_jq is jq_of with jq arguments — the harness's jq_of takes exactly two
# arguments, and half the accounting below has to pass an addr_port or an id
# into the filter. Interpolating those into the filter text instead would put
# a value that contains ':' and '.' inside a jq program.
smoke_jq() { # <json> <filter> [jq args…]
	local json=$1 filter=$2
	shift 2
	printf '%s' "$json" | "$JQ" -r "$@" "$filter"
}

# smoke_dn_dir / smoke_cn_dir map an addr_port back to the fake's $WORK
# directory. The gateway picks which node an SP lands on, so a stage that
# wants to seed the fake behind a side or a cntlr can only learn the directory
# from the port (§10.5: dn<i> listens on DN_PORT_BASE + i).
smoke_dn_dir() { # <addr_port>
	printf 'dn%d' $((${1##*:} - DN_PORT_BASE))
}

smoke_cn_dir() { # <addr_port>
	printf 'cn%d' $((${1##*:} - CN_PORT_BASE))
}

# smoke_sp_footprint is Σ ext_cnt over every group of the SP — what §6.5 says
# ONE cntlr's CN reserves, because every cntlr stacks every group of every
# slice. It is recomputed from the stored slices rather than tracked in a
# variable so that it stays right across the two GrowSlices of step 7.
smoke_sp_footprint() { # <sp>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.ext_cnt | tonumber) ] | add // 0'
}

# smoke_dn_charge is what one DN owes this SP: the ext_cnt of the GROUP each
# of its sides belongs to, summed over its sides (§5.6 — a side is charged its
# group's size, and a spare leg's side is charged exactly like an active one).
smoke_dn_charge() { # <sp> <addr_port>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | . as $grp | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?
		  | select(.addr_port == $a) | ($grp.ext_cnt | tonumber) ]
		| add // 0' --arg a "$2"
}

# smoke_dn_sides counts this SP's sides on one DN — the length DnConf's
# side_ptr_list must have, since CreateStoragePool charges one pointer per
# side (§8.2).
smoke_dn_sides() { # <sp> <addr_port>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?
		  | select(.addr_port == $a) ] | length' --arg a "$2"
}

# smoke_cn_cntlrs counts this SP's cntlrs on one CN, which is the multiplier on
# the footprint that CN reserves (§6.5) and the length of its cntlr_ptr_list.
smoke_cn_cntlrs() { # <sp> <addr_port>
	smoke_jq "$(sp_json "$1")" \
		'[ .cntlrs[] | select(.addr_port == $a) ] | length' --arg a "$2"
}

# smoke_cap_cnt counts a node's capacity keys. §5.6 says a node has EXACTLY
# one while it is allocatable and none while it is not, so this is both the
# "the index exists" and the "the index was not duplicated" assertion.
smoke_cap_cnt() { # <dn_capacity|cn_capacity> <addr_port>
	wctl list-keys --prefix "$1" |
		awk -v a="$2" '$NF == a { n++ } END { print n + 0 }'
}

# smoke_cap_free reads free_ext_cnt out of the capacity KEY (§5.6 embeds it in
# the key so a descending range returns largest-free first), in decimal, or
# "none" when the node has no key. Reading the key rather than the DnConf is
# the whole point: it proves the allocator's index agrees with the record.
smoke_cap_free() { # <dn_capacity|cn_capacity> <addr_port>
	local hex
	# awk keeps only the FIRST match itself: `| head -n 1` would close the
	# pipe early, and pipefail turns that SIGPIPE into a script abort.
	hex=$(wctl list-keys --prefix "$1" |
		awk -v a="$2" '$NF == a && !seen { print $(NF - 1); seen = 1 }')
	if [ -z "$hex" ]; then
		printf 'none'
		return 0
	fi
	printf '%d' "$((16#$hex))"
}

# smoke_check_dn is the per-DN half of the §5.4/§5.6 write-set audit, in one
# place because steps 6, 7, 14, 15 and 17 all owe exactly these four facts:
# free = total − what the SP's groups charge, one pointer per side, a capacity
# key that agrees with the record, and the DnRev the ledger's flush left.
smoke_check_dn() { # <sp> <addr_port> <want dn_rev>
	local sp=$1 addr=$2 wantRev=$3 charge sides free
	charge=$(smoke_dn_charge "$sp" "$addr")
	sides=$(smoke_dn_sides "$sp" "$addr")
	free=$((DN_EXTENTS - charge))
	assert_eq "$(dn_free "$addr")" "$free" "$addr: free_ext_cnt"
	assert_eq "$(smoke_jq "$(dn_json "$addr")" '.total_ext_cnt')" \
		"$DN_EXTENTS" "$addr: total_ext_cnt"
	assert_eq "$(smoke_jq "$(dn_json "$addr")" '.side_ptr_list | length')" \
		"$sides" "$addr: side_ptr_list length"
	assert_eq "$(smoke_cap_cnt dn_capacity "$addr")" 1 \
		"$addr: dn_capacity key count"
	assert_eq "$(smoke_cap_free dn_capacity "$addr")" "$free" \
		"$addr: free_ext_cnt embedded in the dn_capacity key"
	assert_eq "$(dn_rev_of "$addr")" "$wantRev" "$addr: dn_rev"
}

# smoke_check_cn is smoke_check_dn's CN mirror: every cntlr on the node
# reserves the SP's WHOLE footprint (§6.5), so the charge is footprint × the
# number of this SP's cntlrs the node carries.
smoke_check_cn() { # <sp> <addr_port> <want cn_rev>
	local sp=$1 addr=$2 wantRev=$3 cntlrs footprint free
	cntlrs=$(smoke_cn_cntlrs "$sp" "$addr")
	footprint=$(smoke_sp_footprint "$sp")
	free=$((CN_EXTENTS - cntlrs * footprint))
	assert_eq "$(cn_free "$addr")" "$free" "$addr: free_ext_cnt"
	assert_eq "$(smoke_jq "$(cn_json "$addr")" '.total_ext_cnt')" \
		"$CN_EXTENTS" "$addr: total_ext_cnt"
	assert_eq "$(smoke_jq "$(cn_json "$addr")" '.cntlr_ptr_list | length')" \
		"$cntlrs" "$addr: cntlr_ptr_list length"
	assert_eq "$(smoke_cap_cnt cn_capacity "$addr")" 1 \
		"$addr: cn_capacity key count"
	assert_eq "$(smoke_cap_free cn_capacity "$addr")" "$free" \
		"$addr: free_ext_cnt embedded in the cn_capacity key"
	assert_eq "$(cn_rev_of "$addr")" "$wantRev" "$addr: cn_rev"
}

# smoke_grp_addrs lists the distinct DN addr_ports one group occupies, active
# and spare legs alike — the set whose DnRev a group-touching op bumps, and
# the set a placement must stay out of (§6.5's black list).
smoke_grp_addrs() { # <sp> <grp_id>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g)
		  | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?.addr_port ]
		| unique[]' --arg g "$2"
}

# smoke_grp_field reads one field of one group by grp_id; the group can be a
# meta or a data one and the caller never has to know which list it is in.
smoke_grp_field() { # <sp> <grp_id> <field>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) ][0] | .[$f]' --arg g "$2" --arg f "$3"
}

# smoke_side_field reads one field of one side by side_id, wherever in the SP
# it lives. A side_id is unique inside an SP but carries no hint of its slice
# or leg (§8.6), so every side assertion goes through this scan.
smoke_side_field() { # <sp> <side_id> <field>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?) | .side_list[]?
		  | select(.side_id == $s) ][0] | .[$f]' --arg s "$2" --arg f "$3"
}

# smoke_leg_side_ids lists one leg's side ids in stored order. A leg carries
# two of them only while a migration runs on it (§8.11), so this is how steps
# 14's create/cancel/finish are checked against the leg itself.
smoke_leg_side_ids() { # <sp> <leg_id>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?) | select(.leg_id == $l)
		  | .side_list[]?.side_id ] | .[]' --arg l "$2"
}

# smoke_grp_leg_ids lists a group's ACTIVE leg ids in position order — the md
# member slots, which is what step 15 asserts the switch swapped in place.
smoke_grp_leg_ids() { # <sp> <grp_id>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) | .leg_list[]?.leg_id ] | .[]' --arg g "$2"
}

# smoke_grp_spare_ids is its spare_leg_list twin.
smoke_grp_spare_ids() { # <sp> <grp_id>
	smoke_jq "$(sp_json "$1")" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) | .spare_leg_list[]?.leg_id ] | .[]'  \
		--arg g "$2"
}

# smoke_ns prints one stored namespace as JSON. A namespace is a field of its
# Subsystem value and has no key of its own (§8.8), so every namespace
# assertion digs it out of the subsystem the ground-truth reader returned.
smoke_ns() { # <sp> <nqn> <ns_idx>
	smoke_jq "$(sp_json "$1")" '
		.subsystems[$n].ns_list[] | select(.ns_idx == ($i | tonumber))
		| @json' --arg n "$2" --arg i "$3"
}

# smoke_td_field reads one field of one stored thin device.
smoke_td_field() { # <sp> <td_name> <field>
	smoke_jq "$(sp_json "$1")" '.tds[$t] | .[$f]' --arg t "$2" --arg f "$3"
}

# smoke_dn_free_sum is Σ free_ext_cnt over the four DNs — the aggregate the
# teardown of step 17 must restore exactly, and the cross-check that no
# per-DN assertion above quietly agreed with a wrong topology.
smoke_dn_free_sum() {
	local i total=0
	for i in 0 1 2 3; do
		total=$((total + $(dn_free "$(dn_addr "$i")")))
	done
	printf '%d' "$total"
}

case_smoke() {
	local i out addr rev want
	# The §10.5 identity plan: one subsystem NQN of this SP, four host NQNs
	# and the out-of-cluster NQN a clone names as its source.
	local nqn="$NQN_PREFIX:$CLUSTER:sp0:ss0"
	local host0="$NQN_PREFIX:host:h0"
	local host1="$NQN_PREFIX:host:h1"
	local host2="$NQN_PREFIX:host:h2"
	local host3="$NQN_PREFIX:host:h3"
	local srcNqn="$NQN_PREFIX:src:ss0"

	# -------------------------------------------------------------------
	stage 1 "create-cluster itgw: ClusterConf, the three globals, read-back"
	# -------------------------------------------------------------------
	new_cluster
	local cidHex
	cidHex=$(printf '%016x' "$CID")
	local cc
	cc=$(raw_key "$(cluster_key)")
	# §5.2: cluster_id is derived from name ‖ creation_epoch, so an epoch of 0
	# would make every cluster of this name share one key prefix.
	assert_ne "$(jq_of "$cc" '.creation_epoch')" "0" "ClusterConf creation_epoch"
	# "confs verbatim" (§10.11 step 1): the request carried none of the five
	# sub-messages, and GW11 resolves defaults at USE time, never at write
	# time — so every one of them must still be absent in the store.
	local field
	for field in qos_ratio bdev_conf dn_bin_conf alloc_conf health_check_conf; do
		assert_eq "$(jq_of "$cc" ".$field")" "null" \
			"ClusterConf.$field is stored verbatim (GW11: no write-time defaults)"
	done
	# §5.4: each global starts at next_id 1 with ShardBucketSize zeros, and the
	# three buckets are separate slices — they are independent counters and
	# must never alias.
	local kind global
	for kind in dn_global cn_global sp_global; do
		global=$(raw_key "$DNV_PREFIX $kind $cidHex")
		assert_field "$global" '.next_id' "1" "$kind next_id"
		assert_eq "$(jq_of "$global" '.shard_bucket | length')" "256" \
			"$kind shard_bucket length"
		assert_eq "$(jq_of "$global" '.shard_bucket | unique | @json')" "[0]" \
			"$kind shard_bucket all zero"
	done
	# The gateway's own read path must agree with the store it wrote.
	out=$(gw get-cluster)
	assert_field "$out" '.cluster_name' "$CLUSTER" "get-cluster cluster_name"
	assert_field "$out" '.cluster_id' "$CID" "get-cluster cluster_id"
	assert_field "$out" '.dn_global.next_id' "1" "get-cluster dn_global next_id"
	assert_field "$out" '.cn_global.next_id' "1" "get-cluster cn_global next_id"
	assert_field "$out" '.sp_global.next_id' "1" "get-cluster sp_global next_id"
	assert_eq "$(jq_of "$out" '.dn_global.shard_bucket | unique | @json')" "[0]" \
		"get-cluster dn_global bucket all zero"
	assert_field "$out" '.cluster_conf.creation_epoch' \
		"$(jq_of "$cc" '.creation_epoch')" "get-cluster echoes the stored epoch"
	# §8.1: the name is the key, so a second create is ALREADY_EXISTS and must
	# not stamp a second epoch over the first.
	assert_no_write "duplicate create-cluster" \
		gwx ALREADY_EXISTS create-cluster

	# -------------------------------------------------------------------
	stage 2 "list-clusters pagination over pg0..pg4, a bad token, and cleanup"
	# -------------------------------------------------------------------
	local -a pgCid=()
	for i in 0 1 2 3 4; do
		out=$(gw create-cluster --name "pg$i")
		pgCid[i]=$(jq_of "$out" '.cluster_id')
		assert_ne "${pgCid[i]}" "0" "pg$i cluster_id"
	done
	# ClusterConf is the only name-keyed message (§5.1) and RangeKeys sorts
	# ascending, so the six names page in lexical order: itgw pg0 pg1 … pg4.
	local page token
	page=$(gw list-clusters --count 2)
	assert_eq "$(jq_of "$page" '.cluster_name | @json')" '["itgw","pg0"]' \
		"list-clusters page 1"
	token=$(jq_of "$page" '.page_token')
	assert_ne "$token" "" "list-clusters page 1 token"
	page=$(gw list-clusters --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.cluster_name | @json')" '["pg1","pg2"]' \
		"list-clusters page 2"
	token=$(jq_of "$page" '.page_token')
	assert_ne "$token" "" "list-clusters page 2 token"
	page=$(gw list-clusters --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.cluster_name | @json')" '["pg3","pg4"]' \
		"list-clusters page 3"
	token=$(jq_of "$page" '.page_token')
	# §5.7: the token is empty exactly when the page was NOT full, and page 3
	# was full — so the end of the listing costs one more, empty, call.
	assert_ne "$token" "" "list-clusters page 3 token (a full page always continues)"
	page=$(gw list-clusters --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.cluster_name | length')" "0" \
		"list-clusters page 4 is empty"
	assert_eq "$(jq_of "$page" '.page_token')" "" \
		"list-clusters page 4 token"
	# A page_token that is not base64 is INVALID_ARGUMENT, and a list writes
	# nothing whether it succeeds or not.
	assert_no_write "list-clusters with a malformed page_token" \
		gwx INVALID_ARGUMENT list-clusters --page-token '!!'
	for i in 0 1 2 3 4; do
		out=$(gw delete-cluster --name "pg$i")
		# §8.1: cluster_id is derived, not stored, so the delete reply is the
		# only proof the RPC acted on the cluster the create made.
		assert_field "$out" '.cluster_id' "${pgCid[i]}" \
			"delete-cluster pg$i echoes its cluster_id"
	done
	page=$(gw list-clusters)
	assert_eq "$(jq_of "$page" '.cluster_name | @json')" "[\"$CLUSTER\"]" \
		"only $CLUSTER is left after the pg0..pg4 teardown"

	# -------------------------------------------------------------------
	stage 3 "create-dn x4 and create-cn x3: confs, revs, capacity keys, T3"
	# -------------------------------------------------------------------
	local shard
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		out=$(gw create-dn --addr "$addr" --location "$(dn_loc "$i")" \
			--tr-svc-id "$(dn_svcid "$i")")
		assert_field "$out" '.dn_id' "$((i + 1))" "create-dn $addr dn_id"
		local dn
		dn=$(dn_json "$addr")
		# §6.1: 64 GiB / the default 1 GiB extent_size, rounded down, and a
		# fresh node is entirely free with no side on it yet.
		assert_field "$dn" '.total_ext_cnt' "$DN_EXTENTS" "$addr total_ext_cnt"
		assert_field "$dn" '.free_ext_cnt' "$DN_EXTENTS" "$addr free_ext_cnt"
		assert_field "$dn" '.disabled' "false" "$addr disabled"
		assert_field "$dn" '.err_epoch' "0" "$addr err_epoch"
		assert_field "$dn" '.location' "$(dn_loc "$i")" "$addr location"
		assert_eq "$(jq_of "$dn" '.side_ptr_list | length')" "0" \
			"$addr side_ptr_list"
		assert_field "$dn" '.nvme_tr_conf.tr_type' "tcp" "$addr tr_type"
		assert_field "$dn" '.nvme_tr_conf.adr_fam' "ipv4" "$addr adr_fam"
		assert_field "$dn" '.nvme_tr_conf.tr_addr' "127.0.0.1" "$addr tr_addr"
		assert_field "$dn" '.nvme_tr_conf.tr_svc_id' "$(dn_svcid "$i")" \
			"$addr tr_svc_id"
		# The DnRev key is what makes a dn-worker start syncing the node
		# (§5.5); it is created at revision 1, never bumped by this RPC.
		shard=$(jq_of "$dn" '.shard_code')
		assert_field "$(wctl get-rev dn --id "$((i + 1))" \
			--shard "$(printf '%02x' "$shard")")" '.addr_port' "$addr" \
			"$addr dn_rev.addr_port"
		assert_eq "$(dn_rev_of "$addr")" "1" "$addr dn_rev revision"
		# §5.6: exactly one capacity key, embedding the free count.
		assert_eq "$(smoke_cap_cnt dn_capacity "$addr")" 1 \
			"$addr dn_capacity key count"
		assert_eq "$(smoke_cap_free dn_capacity "$addr")" "$DN_EXTENTS" \
			"$addr dn_capacity key free_ext_cnt"
	done
	# The T3 chain proof (§10.10): CreateDiskNode leaves etcd to ask the node
	# its size (AG1), and the trace id this stage minted must appear on the
	# fake's own record of that call — the thread that ties a script stage to
	# the gateway's log, the agent's log and etcd's.
	assert_ge "$(count_recs "$(apath dn0)" \
		'select(.msg == "grpc server request"
			and .trace_id == "'"$TRACE"'"
			and (.method // "" | endswith("GetDnSize")))')" 1 \
		"dn0 agent.log: a GetDnSize request carrying trace_id $TRACE"
	assert_eq "$(key_count dn_capacity)" "4" "one dn_capacity key per DN"
	# §5.4: next_id counts every id ever drawn, Σbucket the live objects.
	global=$(raw_key "$DNV_PREFIX dn_global $cidHex")
	assert_field "$global" '.next_id' "5" "DnGlobal next_id after four DNs"
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "4" \
		"DnGlobal Σbucket after four DNs"
	# The paged list agrees with the four get-dns: 4 names at count 2 is two
	# FULL pages plus the empty page that ends the listing (§5.7).
	page=$(gw list-dns --count 2)
	assert_eq "$(jq_of "$page" '.addr_port | @json')" \
		"[\"$(dn_addr 0)\",\"$(dn_addr 1)\"]" "list-dns page 1"
	token=$(jq_of "$page" '.page_token')
	page=$(gw list-dns --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.addr_port | @json')" \
		"[\"$(dn_addr 2)\",\"$(dn_addr 3)\"]" "list-dns page 2"
	token=$(jq_of "$page" '.page_token')
	page=$(gw list-dns --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.addr_port | length')" "0" "list-dns page 3"
	assert_eq "$(jq_of "$page" '.page_token')" "" "list-dns page 3 token"
	# The CNs are the same six RPCs with one difference that matters here:
	# the fakes run --size 0, so §6.1's "0 ⇒ DefaultCnCap 4 TiB" gives 4096.
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		out=$(gw create-cn --addr "$addr" --location "$(cn_loc "$i")" \
			--tr-svc-id "$(cn_svcid "$i")")
		assert_field "$out" '.cn_id' "$((i + 1))" "create-cn $addr cn_id"
		local cn
		cn=$(cn_json "$addr")
		assert_field "$cn" '.total_ext_cnt' "$CN_EXTENTS" "$addr total_ext_cnt"
		assert_field "$cn" '.free_ext_cnt' "$CN_EXTENTS" "$addr free_ext_cnt"
		assert_field "$cn" '.disabled' "false" "$addr disabled"
		assert_field "$cn" '.location' "$(cn_loc "$i")" "$addr location"
		assert_eq "$(jq_of "$cn" '.cntlr_ptr_list | length')" "0" \
			"$addr cntlr_ptr_list"
		assert_field "$cn" '.nvme_tr_conf.tr_svc_id' "$(cn_svcid "$i")" \
			"$addr tr_svc_id"
		assert_eq "$(cn_rev_of "$addr")" "1" "$addr cn_rev revision"
		assert_eq "$(smoke_cap_cnt cn_capacity "$addr")" 1 \
			"$addr cn_capacity key count"
		assert_eq "$(smoke_cap_free cn_capacity "$addr")" "$CN_EXTENTS" \
			"$addr cn_capacity key free_ext_cnt"
	done
	assert_eq "$(key_count cn_capacity)" "3" "one cn_capacity key per CN"
	global=$(raw_key "$DNV_PREFIX cn_global $cidHex")
	assert_field "$global" '.next_id' "4" "CnGlobal next_id after three CNs"
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "3" \
		"CnGlobal Σbucket after three CNs"
	# 3 names at count 2 ends on a SHORT page, so its token is empty and no
	# fourth call is needed — the other half of the §5.7 rule.
	page=$(gw list-cns --count 2)
	assert_eq "$(jq_of "$page" '.addr_port | @json')" \
		"[\"$(cn_addr 0)\",\"$(cn_addr 1)\"]" "list-cns page 1"
	token=$(jq_of "$page" '.page_token')
	page=$(gw list-cns --count 2 --page-token "$token")
	assert_eq "$(jq_of "$page" '.addr_port | @json')" "[\"$(cn_addr 2)\"]" \
		"list-cns page 2"
	assert_eq "$(jq_of "$page" '.page_token')" "" \
		"list-cns page 2 token (a short page ends the listing)"

	# -------------------------------------------------------------------
	stage 4 "inspect-dn / inspect-cn: the agent's applied revision and live info"
	# -------------------------------------------------------------------
	# The suite runs NO dnv-worker, so no Syncup* has ever reached a fake and
	# every *Info it derives from "the last applied request" is absent. That
	# is the truthful live state of the node, not a failure of the call (AG3),
	# and the reply's applied_revision is the agent's — the revision of the
	# last applied Syncup*, 0 for a fake that has applied nothing
	# (architecture.md §8.2/§8.3), never the stored rev key.
	out=$(gw inspect-dn --addr "$(dn_addr 0)")
	assert_field "$out" '.applied_revision' "0" \
		"inspect-dn applied_revision of an unsynced fake"
	assert_eq "$(jq_of "$out" '.dn_info')" "null" \
		"inspect-dn dn_info of a fake no worker ever synced"
	out=$(gw inspect-cn --addr "$(cn_addr 0)")
	assert_field "$out" '.applied_revision' "0" \
		"inspect-cn applied_revision of an unsynced fake"
	assert_eq "$(jq_of "$out" '.cn_info')" "null" \
		"inspect-cn cn_info of a fake no worker ever synced"
	# Seeding state.json is the fake's documented operator-edit path (§14.9),
	# and it is what lets this stage also prove the PASS-THROUGH half of the
	# RPC: whatever the agent reports arrives at the caller untouched. The
	# seeded applied revision is a distinctive 7 — the stored rev key is
	# still 1, so the two cannot be confused.
	set_state dn0 <<EOF
{"objects":{"dn":{"revision":7,"request":{
  "cluster_id":"$CID","dn_id":"1","revision":"7","side_pointer_list":[]}}}}
EOF
	out=$(gw inspect-dn --addr "$(dn_addr 0)")
	assert_field "$out" '.applied_revision' "7" \
		"inspect-dn applied_revision is the seeded agent revision"
	assert_field "$out" '.dn_info.disk_info.res_name' "disk_info" \
		"inspect-dn disk_info res_name"
	assert_field "$out" '.dn_info.disk_info.status' "RES_STATUS_OK" \
		"inspect-dn disk_info status"
	assert_field "$out" '.dn_info.port_info.status' "RES_STATUS_OK" \
		"inspect-dn port_info status"
	set_state cn0 <<EOF
{"objects":{"cn":{"revision":7,"request":{
  "cluster_id":"$CID","cn_id":"1","revision":"7","cntlr_pointer_list":[]}}}}
EOF
	out=$(gw inspect-cn --addr "$(cn_addr 0)")
	assert_field "$out" '.applied_revision' "7" \
		"inspect-cn applied_revision is the seeded agent revision"
	assert_field "$out" '.cn_info.port_info.status' "RES_STATUS_OK" \
		"inspect-cn port_info status"
	assert_field "$out" '.cn_info.loop_dev_info.res_name' "loop_dev_info" \
		"inspect-cn loop_dev_info res_name"

	# -------------------------------------------------------------------
	stage 5 "set-dn-disabled / set-cn-disabled: capacity key, no rev, no-op"
	# -------------------------------------------------------------------
	addr=$(dn_addr 3)
	out=$(gw set-dn-disabled --addr "$addr" --rev 1 --disabled)
	assert_field "$out" '.dn_id' "4" "set-dn-disabled reply dn_id"
	# §8.2: `disabled` gates CP scheduling only — the allocator's index goes
	# away, the record says so, and NO revision is bumped, because no agent
	# has to be told and the sides the node hosts keep running.
	assert_field "$(dn_json "$addr")" '.disabled' "true" "$addr disabled true"
	assert_eq "$(smoke_cap_cnt dn_capacity "$addr")" 0 \
		"$addr dn_capacity key gone while disabled"
	assert_eq "$(key_count dn_capacity)" "3" "dn_capacity keys while dn3 is disabled"
	assert_eq "$(dn_rev_of "$addr")" "1" "$addr dn_rev unchanged by the disable"
	# §0 #17: re-sending the state the node already has writes NOTHING and
	# bumps nothing, but the token is still checked first — hence OK, and
	# hence a store revision that must not move.
	assert_no_write "idempotent set-dn-disabled" \
		gw set-dn-disabled --addr "$addr" --rev 1 --disabled
	assert_eq "$(dn_rev_of "$addr")" "1" "$addr dn_rev after the idempotent repeat"
	# Go's flag package never consumes the next argument for a bool, so
	# re-enabling is the `=` form and never `--disabled false`.
	out=$(gw set-dn-disabled --addr "$addr" --rev 1 --disabled=false)
	assert_field "$out" '.dn_id' "4" "re-enable reply dn_id"
	assert_field "$(dn_json "$addr")" '.disabled' "false" "$addr disabled false"
	assert_eq "$(smoke_cap_cnt dn_capacity "$addr")" 1 \
		"$addr dn_capacity key back after re-enabling"
	assert_eq "$(smoke_cap_free dn_capacity "$addr")" "$DN_EXTENTS" \
		"$addr dn_capacity free after re-enabling"
	assert_eq "$(key_count dn_capacity)" "4" "dn_capacity keys after re-enabling"
	assert_eq "$(dn_rev_of "$addr")" "1" "$addr dn_rev after the whole flip"
	# The CN mirror, once, on the node no cntlr will land on later.
	addr=$(cn_addr 2)
	out=$(gw set-cn-disabled --addr "$addr" --rev 1 --disabled)
	assert_field "$out" '.cn_id' "3" "set-cn-disabled reply cn_id"
	assert_field "$(cn_json "$addr")" '.disabled' "true" "$addr disabled true"
	assert_eq "$(smoke_cap_cnt cn_capacity "$addr")" 0 \
		"$addr cn_capacity key gone while disabled"
	assert_eq "$(cn_rev_of "$addr")" "1" "$addr cn_rev unchanged by the disable"
	assert_no_write "idempotent set-cn-disabled" \
		gw set-cn-disabled --addr "$addr" --rev 1 --disabled
	out=$(gw set-cn-disabled --addr "$addr" --rev 1 --disabled=false)
	assert_field "$(cn_json "$addr")" '.disabled' "false" "$addr disabled false"
	assert_eq "$(smoke_cap_cnt cn_capacity "$addr")" 1 \
		"$addr cn_capacity key back after re-enabling"
	assert_eq "$(key_count cn_capacity)" "3" "cn_capacity keys after re-enabling"
	assert_eq "$(cn_rev_of "$addr")" "1" "$addr cn_rev after the whole flip"

	# -------------------------------------------------------------------
	stage 6 "create-sp sp0: the whole §5.4 write set and its accounting"
	# -------------------------------------------------------------------
	make_sp sp0
	verify_sp sp0
	local spOut spId
	spOut=$(sp_json sp0)
	spId=$(sp_id_of sp0)
	# The first SP of a fresh cluster draws id 1 (§5.4: next_id starts at 1).
	assert_eq "$spId" "1" "sp0 sp_id"
	assert_eq "$SP_REV" "1" "sp0 SpRev starts at 1: it is created, not bumped"
	global=$(raw_key "$DNV_PREFIX sp_global $cidHex")
	assert_field "$global" '.next_id' "2" "SpGlobal next_id after one SP"
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "1" \
		"SpGlobal Σbucket after one SP"
	# The gateway's own read of the SP must deep-equal the ground truth. Both
	# drivers re-encode protojson through encoding/json, which sorts map keys,
	# so the two documents are comparable as text once ordered by a field.
	out=$(gw get-sp --sp sp0)
	assert_field "$out" '.sp_rev.revision' "1" "get-sp sp_rev"
	assert_eq "$(smoke_jq "$out" '.sp_conf | @json')" \
		"$(smoke_jq "$spOut" '.sp_conf | @json')" "get-sp SpConf deep-equals the store"
	assert_eq "$(smoke_jq "$out" '[.cntlr_list[]] | sort_by(.cntlid_slot) | @json')" \
		"$(smoke_jq "$spOut" '[.cntlrs[]] | sort_by(.cntlid_slot) | @json')" \
		"get-sp cntlr_list deep-equals the stored cntlrs"
	assert_eq "$(smoke_jq "$out" '[.slice_list[]] | sort_by(.slice_idx) | @json')" \
		"$(smoke_jq "$spOut" '[.slices[]] | sort_by(.slice_idx) | @json')" \
		"get-sp slice_list deep-equals the stored slices"
	# §6.5: 1 meta extent + 2 data extents = a footprint of 3, mirrored over
	# two legs each = 6 DN extents on four distinct DNs.
	assert_eq "$(smoke_sp_footprint sp0)" "3" "sp0 footprint"
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - SP_EXT_COST))" \
		"Σ DN free after create-sp"
	for i in 0 1 2 3; do
		# Every DN carries exactly one side of this SP and is charged that
		# side's group; the ledger bumped each DnRev once, 1 -> 2 (§5.5).
		addr=$(dn_addr "$i")
		assert_eq "$(smoke_dn_sides sp0 "$addr")" "1" "$addr hosts one side of sp0"
		smoke_check_dn sp0 "$addr" 2
	done
	assert_eq "$(key_count dn_capacity)" "4" "dn_capacity keys after create-sp"
	for i in 0 1 2; do
		# Two CNs took a cntlr and reserved the whole footprint (4096 -> 4093,
		# CnRev 1 -> 2); the third was never touched, so it stays at 1.
		addr=$(cn_addr "$i")
		want=1
		[ "$(smoke_cn_cntlrs sp0 "$addr")" = 0 ] || want=2
		smoke_check_cn sp0 "$addr" "$want"
	done
	assert_eq "$(smoke_jq "$spOut" '[.cntlrs[] | .addr_port] | unique | length')" \
		"$SP_CNTLR_CNT" "sp0 cntlrs are on distinct CNs"
	# §8.4's reverse lookup: an id with no sp_id_to_name key is simply left
	# out of the map, so absence IS the answer "no such sp_id".
	out=$(gw find-sp-names --ids 1,99)
	assert_eq "$(jq_of "$out" '.sp_id_to_name | length')" "1" \
		"find-sp-names returns only the live id"
	assert_field "$out" '.sp_id_to_name["1"]' "sp0" "find-sp-names maps 1 to sp0"

	# -------------------------------------------------------------------
	stage 7 "grow-slice: the meta ladder, then a data group"
	# -------------------------------------------------------------------
	local slice grpId spRevBefore
	slice=$(sp_first_slice sp0)
	local -a beforeRev=()
	spRevBefore=$SP_REV
	for i in 0 1 2 3; do beforeRev[i]=$(dn_rev_of "$(dn_addr "$i")"); done
	out=$(gw grow-slice --sp sp0 --rev "$SP_REV" --slice "$slice" --meta)
	refresh_rev sp0
	assert_field "$out" '.slice_id' "$slice" "grow-slice --meta echoes the slice"
	grpId=$(jq_of "$out" '.grp_id')
	assert_ne "$grpId" "0" "grow-slice --meta grp_id"
	# model.GrowSlice bumps SpRev itself, once, in the same transaction as the
	# write; the handler adds none of its own.
	assert_eq "$SP_REV" "$((spRevBefore + 1))" \
		"the meta grow bumps SpRev exactly once"
	# The meta ladder doubles the slice's meta total (§8.5), so from 1 extent
	# the next rung adds exactly 1 more, as a second meta group.
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'[.slices[] | .meta_grp_list[]] | length')" "2" "sp0 meta group count"
	assert_eq "$(smoke_grp_field sp0 "$grpId" ext_cnt)" "1" \
		"the new meta group's ext_cnt (ladder rung 1 -> 2 total)"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '
		[ .slices[] | .meta_grp_list[] | select(.grp_id == $g)
		  | .leg_list[] ] | length' --arg g "$grpId")" "2" \
		"the new meta group has two legs (raid1)"
	local grpAddrs
	grpAddrs=$(smoke_grp_addrs sp0 "$grpId")
	assert_eq "$(printf '%s\n' "$grpAddrs" | wc -l | tr -d ' ')" "2" \
		"the new meta group's legs are on two distinct DNs"
	# D-F: only the DNs the NEW group landed on were charged, so only their
	# DnRev moved — the ledger bumps once per node it touched (§5.5).
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		if printf '%s\n' "$grpAddrs" | grep -qx "$addr"; then
			want=$((want + 1))
		fi
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - SP_EXT_COST - 2))" \
		"Σ DN free after the meta grow"
	# Every cntlr's CN is charged the delta too (§8.5), so the footprint the
	# CN reserves tracks the current geometry: 3 -> 4.
	assert_eq "$(smoke_sp_footprint sp0)" "4" "sp0 footprint after the meta grow"
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		want=1
		[ "$(smoke_cn_cntlrs sp0 "$addr")" = 0 ] || want=3
		smoke_check_cn sp0 "$addr" "$want"
	done
	# D-E: req.ext_cnt is only the "this is a data grow" signal; the size
	# added is the slice's ORIGINAL allocation unit, data_grp_list[0].ext_cnt.
	out=$(gw grow-slice --sp sp0 --rev "$SP_REV" --slice "$slice" --ext 2)
	refresh_rev sp0
	grpId=$(jq_of "$out" '.grp_id')
	assert_eq "$SP_REV" "$((spRevBefore + 2))" \
		"the data grow bumps SpRev exactly once more"
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'[.slices[] | .data_grp_list[]] | length')" "2" "sp0 data group count"
	assert_eq "$(smoke_grp_field sp0 "$grpId" ext_cnt)" "$SP_INIT_EXT" \
		"the new data group inherits the slice's allocation unit"
	grpAddrs=$(smoke_grp_addrs sp0 "$grpId")
	assert_eq "$(printf '%s\n' "$grpAddrs" | wc -l | tr -d ' ')" "2" \
		"the new data group's legs are on two distinct DNs"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		if printf '%s\n' "$grpAddrs" | grep -qx "$addr"; then
			want=$((want + 1))
		fi
		smoke_check_dn sp0 "$addr" "$want"
	done
	assert_eq "$(smoke_sp_footprint sp0)" "6" "sp0 footprint after the data grow"
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12))" \
		"Σ DN free after both grows"
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		want=1
		[ "$(smoke_cn_cntlrs sp0 "$addr")" = 0 ] || want=4
		smoke_check_cn sp0 "$addr" "$want"
	done

	# -------------------------------------------------------------------
	stage 8 "set-sp-level there and back, set-cntlid-slots: one bump each"
	# -------------------------------------------------------------------
	spRevBefore=$SP_REV
	out=$(gw set-sp-level --sp sp0 --rev "$SP_REV" --level READONLY)
	refresh_rev sp0
	assert_field "$out" '.sp_id' "$spId" "set-sp-level reply sp_id"
	assert_field "$(sp_json sp0)" '.sp_conf.sp_level' "SP_LEVEL_READONLY" \
		"sp0 sp_level READONLY"
	# §11.7: the level is not applied here in any sense — the bump IS the
	# mechanism, so exactly one is owed per call.
	assert_eq "$SP_REV" "$((spRevBefore + 1))" \
		"set-sp-level READONLY bumps SpRev exactly once"
	out=$(gw set-sp-level --sp sp0 --rev "$SP_REV" --level READWRITE)
	refresh_rev sp0
	assert_field "$(sp_json sp0)" '.sp_conf.sp_level' "SP_LEVEL_READWRITE" \
		"sp0 sp_level back to READWRITE"
	assert_eq "$SP_REV" "$((spRevBefore + 2))" \
		"set-sp-level READWRITE bumps SpRev exactly once"
	# Slot 2 is added because step 10 creates a third cntlr on it; slots 0 and
	# 1 stay because a cntlr and every side already name them (§11.8).
	out=$(gw set-cntlid-slots --sp sp0 --rev "$SP_REV" --slots 0,1,2)
	refresh_rev sp0
	assert_field "$out" '.sp_id' "$spId" "set-cntlid-slots reply sp_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.cntlid_slot_list | @json')" \
		"[0,1,2]" "sp0 cntlid_slot_list"
	assert_eq "$SP_REV" "$((spRevBefore + 3))" \
		"set-cntlid-slots bumps SpRev exactly once"

	# -------------------------------------------------------------------
	stage 9 "create-td t0, the snapshot gate, the worker flip, then t1"
	# -------------------------------------------------------------------
	spRevBefore=$SP_REV
	out=$(gw create-td --sp sp0 --rev "$SP_REV" --name t0 --size "$TD_SIZE")
	refresh_rev sp0
	assert_eq "$SP_REV" "$((spRevBefore + 1))" \
		"create-td bumps SpRev exactly once"
	local t0Id t0Dev
	t0Id=$(jq_of "$out" '.td_id')
	assert_ne "$t0Id" "0" "create-td t0 td_id"
	# dev_id is uint32, so protojson renders it as a NUMBER, not a string.
	assert_field "$out" '.dev_id' "1" "create-td t0 dev_id"
	t0Dev=1
	assert_eq "$(smoke_td_field sp0 t0 td_id)" "$t0Id" "t0 td_id in the store"
	assert_eq "$(smoke_td_field sp0 t0 dev_id)" "1" "t0 dev_id"
	assert_eq "$(smoke_td_field sp0 t0 ori_id)" "0" "t0 ori_id (no origin)"
	assert_eq "$(smoke_td_field sp0 t0 size)" "$TD_SIZE" "t0 size"
	# §8.7: the gateway always writes created false; only the sp-worker flips
	# it, once a cntlr has reported the thin volume OK in every slice.
	assert_eq "$(smoke_td_field sp0 t0 created)" "false" "t0 created"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.td_name_list | @json')" \
		'["t0"]' "sp0 td_name_list"
	assert_field "$(sp_json sp0)" '.sp_conf.next_dev_id' "2" "sp0 next_dev_id"
	# The snapshot gate. §8.7 requires it to write LITERALLY nothing — no key,
	# no next_id and no next_dev_id consumed, no bump — because ids are never
	# reused and a consumed one would be visible for ever.
	local beforeNextId beforeDevId
	beforeNextId=$(smoke_jq "$(sp_json sp0)" '.sp_conf.next_id')
	beforeDevId=$(smoke_jq "$(sp_json sp0)" '.sp_conf.next_dev_id')
	assert_no_write "snapshot of an origin that is not created yet" \
		gwx FAILED_PRECONDITION create-td --sp sp0 --rev "$SP_REV" \
		--name t1 --ori t0
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.next_id')" \
		"$beforeNextId" "next_id after the refused snapshot"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.next_dev_id')" \
		"$beforeDevId" "next_dev_id after the refused snapshot"
	assert_eq "$(sp_rev_of sp0)" "$SP_REV" "SpRev after the refused snapshot"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.tds | keys | @json')" '["t0"]' \
		"no t1 key after the refused snapshot"
	# Playing the worker, at exactly the step the precondition demands it
	# (§10.9, §2.4). The flip goes through model.FlipCreated, so it also bumps
	# SpRev — which the cached token has to be refreshed from.
	spRevBefore=$SP_REV
	out=$(wctl set-created --sp sp0 --name t0)
	assert_field "$out" '.flipped' "true" "set-created flipped t0"
	refresh_rev sp0
	assert_eq "$(smoke_td_field sp0 t0 created)" "true" "t0 created after the flip"
	assert_eq "$SP_REV" "$((spRevBefore + 1))" \
		"the created flip bumps SpRev exactly once (§2.4)"
	# The flip's own reply carries the new SpRev, so the token the script now
	# caches and the one the flip left behind must be the same number.
	assert_field "$out" '.sp_rev' "$SP_REV" \
		"set-created reports the SpRev the refresh read back"
	out=$(gw create-td --sp sp0 --rev "$SP_REV" --name t1 --ori t0)
	refresh_rev sp0
	local t1Id
	t1Id=$(jq_of "$out" '.td_id')
	assert_field "$out" '.dev_id' "2" "create-td t1 dev_id"
	# [D-H]: a snapshot points at its origin by dev_id and inherits its size.
	assert_eq "$(smoke_td_field sp0 t1 ori_id)" "$t0Dev" "t1 ori_id is t0's dev_id"
	assert_eq "$(smoke_td_field sp0 t1 size)" "$TD_SIZE" "t1 inherits t0's size"
	assert_eq "$(smoke_td_field sp0 t1 created)" "false" "t1 created"
	out=$(gw list-tds --sp sp0)
	assert_eq "$(jq_of "$out" '.name_to_td | keys | @json')" '["t0","t1"]' \
		"list-tds names"
	assert_field "$out" '.name_to_td.t0.created' "true" "list-tds t0 created"
	assert_field "$out" '.name_to_td.t1.created' "false" "list-tds t1 created"
	assert_field "$out" '.name_to_td.t1.td_id' "$t1Id" "list-tds t1 td_id"

	# -------------------------------------------------------------------
	stage 10 "create-ss, its CdcEntry, and the cntlr lifecycle behind it"
	# -------------------------------------------------------------------
	out=$(gw create-ss --sp sp0 --rev "$SP_REV" --nqn "$nqn" --hosts "$host0")
	refresh_rev sp0
	local ssId
	ssId=$(jq_of "$out" '.ss_id')
	assert_ne "$ssId" "0" "create-ss ss_id"
	local ss
	ss=$(smoke_jq "$(sp_json sp0)" '.subsystems[$n] | @json' --arg n "$nqn")
	# [D2]: (serial, model) is what a host identifies the device by, so the
	# serial is derived from the ss_id that outlives every cntlr.
	assert_field "$ss" '.serial' "$(printf '%016x' "$ssId")" \
		"subsystem serial is %016x(ss_id)"
	assert_field "$ss" '.model' "dnv" "subsystem model"
	assert_eq "$(smoke_jq "$ss" '.ns_list | length')" "0" "subsystem ns_list"
	assert_eq "$(smoke_jq "$ss" '.allowed_hosts | @json')" "[\"$host0\"]" \
		"subsystem allowed_hosts"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.nqn_list | @json')" \
		"[\"$nqn\"]" "sp0 nqn_list"
	# §8.8: the CdcEntry IS desired state — it lists the transports of every
	# ENABLED cntlr's CN, which dnv-cdc serves the discovery log from.
	local cdcKey cdc
	cdcKey="$DNV_PREFIX cdc $cidHex $(printf '%02x' "$(sp_shard sp0)")"
	cdcKey="$cdcKey $(printf '%016x' "$spId") $(printf '%016x' "$ssId")"
	cdc=$(raw_key "$cdcKey")
	assert_field "$cdc" '.nqn' "$nqn" "CdcEntry nqn"
	assert_eq "$(jq_of "$cdc" '.allowed_hosts | @json')" "[\"$host0\"]" \
		"CdcEntry allowed_hosts"
	assert_eq "$(jq_of "$cdc" '[.nvme_tr_conf_list[].tr_svc_id] | sort | @json')" \
		"$(smoke_jq "$(sp_json sp0)" \
			'[.cntlrs[] | select(.disabled | not) | .nvme_tr_conf.tr_svc_id]
			 | sort | @json')" \
		"CdcEntry carries both enabled cntlrs' transports"
	assert_eq "$(key_count cdc)" "1" "one cdc key per subsystem"
	# The host list is stored TWICE on purpose and both copies move together:
	# nvmet's allowed_hosts comes from the Subsystem, the discovery filter
	# from the CdcEntry.
	out=$(gw set-ss-hosts --sp sp0 --rev "$SP_REV" --nqn "$nqn" \
		--hosts "$host0,$host1")
	refresh_rev sp0
	assert_field "$out" '.ss_id' "$ssId" "set-ss-hosts reply ss_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'.subsystems[$n].allowed_hosts | @json' --arg n "$nqn")" \
		"[\"$host0\",\"$host1\"]" "subsystem allowed_hosts after set-ss-hosts"
	assert_eq "$(jq_of "$(raw_key "$cdcKey")" '.allowed_hosts | @json')" \
		"[\"$host0\",\"$host1\"]" "CdcEntry allowed_hosts after set-ss-hosts"
	# A third cntlr on the one CN that carries none, in the slot step 8 added.
	#
	# Which CN that is cannot be hardcoded: §6.5 picks the SP's two original
	# cntlrs at RANDOM out of the three, so the free one is whichever the
	# allocator did not take. It is computed here from the SP's own stored
	# cntlrs — which is also what makes the assertion below meaningful, since
	# §6.4 is exactly the rule that two cntlrs of one SP never share a CN.
	local freeCn used
	used=$(smoke_jq "$(sp_json sp0)" '[.cntlrs[].addr_port] | @json')
	freeCn=""
	for i in 0 1 2; do
		if ! printf '%s' "$used" | grep -Fq "\"$(cn_addr "$i")\""; then
			freeCn=$(cn_addr "$i")
		fi
	done
	assert_ne "$freeCn" "" "one of the three CNs carries no cntlr of sp0"
	out=$(gw create-cntlr --sp sp0 --rev "$SP_REV" --slot 2)
	refresh_rev sp0
	local newCntlr
	newCntlr=$(jq_of "$out" '.cntlr_id')
	assert_ne "$newCntlr" "0" "create-cntlr cntlr_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.cntlr_id_list | length')" \
		"3" "sp0 cntlr count after create-cntlr"
	local newCntlrJson
	newCntlrJson=$(smoke_jq "$(sp_json sp0)" '.cntlrs[$c] | @json' \
		--arg c "$(printf '%016x' "$newCntlr")")
	# §8.6: a new cntlr is a standby, enabled from birth — which is what puts
	# its CN into the CdcEntry below.
	assert_field "$newCntlrJson" '.primary' "false" "the new cntlr is a standby"
	assert_field "$newCntlrJson" '.disabled' "false" "the new cntlr is enabled"
	assert_field "$newCntlrJson" '.cntlid_slot' "2" "the new cntlr's cntlid_slot"
	assert_field "$newCntlrJson" '.addr_port' "$freeCn" \
		"the new cntlr landed on the CN carrying none (§6.4)"
	assert_eq "$(jq_of "$(raw_key "$cdcKey")" '.nvme_tr_conf_list | length')" \
		"3" "CdcEntry gained the new cntlr's transport"
	smoke_check_cn sp0 "$freeCn" 2
	# Disabling makes its namespaces ANA-inaccessible, so its CN must stop
	# being advertised at the same instant (§8.8).
	out=$(gw set-cntlr-enabled --sp sp0 --rev "$SP_REV" --id "$newCntlr" \
		--enabled=false)
	refresh_rev sp0
	assert_field "$out" '.enabled' "false" "set-cntlr-enabled reply"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.cntlrs[$c].disabled' \
		--arg c "$(printf '%016x' "$newCntlr")")" "true" "the cntlr is disabled"
	assert_eq "$(jq_of "$(raw_key "$cdcKey")" '.nvme_tr_conf_list | length')" \
		"2" "CdcEntry dropped the disabled cntlr's transport"
	out=$(gw set-cntlr-enabled --sp sp0 --rev "$SP_REV" --id "$newCntlr" --enabled)
	refresh_rev sp0
	assert_eq "$(jq_of "$(raw_key "$cdcKey")" '.nvme_tr_conf_list | length')" \
		"3" "CdcEntry regained the transport on re-enabling"
	# DeleteCntlr refuses an enabled cntlr (disable first, so the failover has
	# already happened by the time the record disappears), so the delete needs
	# one more disable.
	gw set-cntlr-enabled --sp sp0 --rev "$SP_REV" --id "$newCntlr" --enabled=false
	refresh_rev sp0
	out=$(gw delete-cntlr --sp sp0 --rev "$SP_REV" --id "$newCntlr")
	refresh_rev sp0
	assert_field "$out" '.cntlr_id' "$newCntlr" "delete-cntlr reply cntlr_id"
	# "Clean reversal": every item CreateCntlr wrote is gone again — the id,
	# the key, the CN's pointer and footprint, the CdcEntry transport.
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.cntlr_id_list | length')" \
		"2" "sp0 cntlr count after delete-cntlr"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.cntlrs | keys | length')" "2" \
		"sp0 cntlr keys after delete-cntlr"
	assert_eq "$(jq_of "$(raw_key "$cdcKey")" '.nvme_tr_conf_list | length')" \
		"2" "CdcEntry after delete-cntlr"
	smoke_check_cn sp0 "$freeCn" 3
	out=$(gw list-sss --sp sp0)
	assert_eq "$(jq_of "$out" '.nqn_to_subsystem | keys | @json')" "[\"$nqn\"]" \
		"list-sss names"
	assert_field "$out" ".nqn_to_subsystem[\"$nqn\"].serial" \
		"$(printf '%016x' "$ssId")" "list-sss serial"

	# -------------------------------------------------------------------
	stage 11 "create-ns: minted identities, the duplicate idx, dev and suspend"
	# -------------------------------------------------------------------
	out=$(gw create-ns --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1 --td t0)
	refresh_rev sp0
	local nsId ns
	nsId=$(jq_of "$out" '.ns_id')
	assert_ne "$nsId" "0" "create-ns ns_id"
	ns=$(smoke_ns sp0 "$nqn" 1)
	assert_field "$ns" '.ns_id' "$nsId" "namespace ns_id"
	assert_field "$ns" '.ns_idx' "1" "namespace ns_idx"
	assert_field "$ns" '.td_id' "$t0Id" "namespace td_id is t0's"
	assert_field "$ns" '.suspended' "false" "namespace suspended"
	# §8.8 Defaults: an empty dev_uuid/dev_nguid asks the gateway to mint one,
	# and only the canonical forms validateDevIdentity accepts may be stored —
	# a generated identity must be indistinguishable from a supplied one.
	assert_eq "$(smoke_jq "$ns" \
		'.dev_uuid | test("^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")')" \
		"true" "the minted dev_uuid is a canonical RFC 4122 v4 uuid"
	assert_eq "$(smoke_jq "$ns" '.dev_nguid | test("^[0-9a-f]{32}$")')" "true" \
		"the minted dev_nguid is 32 lower-case hex characters"
	# §8.8: a namespace is a FIELD of its subsystem, not a key, so a reused
	# ns_idx is INVALID_ARGUMENT and never ALREADY_EXISTS.
	assert_no_write "create-ns with an ns_idx the subsystem already uses" \
		gwx INVALID_ARGUMENT create-ns --sp sp0 --rev "$SP_REV" \
		--nqn "$nqn" --idx 1 --td t0
	out=$(gw set-ns-dev --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1 --td t1)
	refresh_rev sp0
	assert_field "$out" '.ns_id' "$nsId" "set-ns-dev reply ns_id"
	assert_field "$(smoke_ns sp0 "$nqn" 1)" '.td_id' "$t1Id" \
		"the namespace now exports t1"
	out=$(gw set-ns-suspended --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1 \
		--suspended)
	refresh_rev sp0
	assert_field "$out" '.ns_id' "$nsId" "set-ns-suspended reply ns_id"
	assert_field "$(smoke_ns sp0 "$nqn" 1)" '.suspended' "true" \
		"the namespace is retired"
	out=$(gw list-sss --sp sp0)
	assert_field "$out" ".nqn_to_subsystem[\"$nqn\"].ns_list[0].suspended" "true" \
		"list-sss shows the retired namespace"
	assert_field "$out" ".nqn_to_subsystem[\"$nqn\"].ns_list[0].td_id" "$t1Id" \
		"list-sss shows the repointed td"

	# -------------------------------------------------------------------
	stage 12 "create-xfer x0, its hosts, the abort path and the finalize path"
	# -------------------------------------------------------------------
	out=$(gw create-xfer --sp sp0 --rev "$SP_REV" --name x0 \
		--ori-nqn "$nqn" --ori-idx 1 --hosts "$host2")
	refresh_rev sp0
	local xferId xfer
	xferId=$(jq_of "$out" '.xfer_id')
	assert_ne "$xferId" "0" "create-xfer xfer_id"
	xfer=$(smoke_jq "$(sp_json sp0)" '.xfers.x0 | @json')
	assert_field "$xfer" '.xfer_id' "$xferId" "transfer xfer_id"
	assert_field "$xfer" '.ori_nqn' "$nqn" "transfer ori_nqn"
	assert_field "$xfer" '.ori_ns_idx' "1" "transfer ori_ns_idx"
	assert_field "$xfer" '.auto_suspend' "false" "transfer auto_suspend"
	assert_eq "$(smoke_jq "$xfer" '.allowed_hosts | @json')" "[\"$host2\"]" \
		"transfer allowed_hosts"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.xfer_name_list | @json')" \
		'["x0"]' "sp0 xfer_name_list"
	# §5.9: the xfer subsystem is reached out of band, never advertised, so
	# no CdcEntry appears for it.
	assert_eq "$(key_count cdc)" "1" "no CdcEntry is written for a transfer"
	out=$(gw get-xfer --sp sp0 --name x0)
	assert_field "$out" '.xfer.xfer_id' "$xferId" "get-xfer xfer_id"
	assert_eq "$(jq_of "$out" '.xfer.allowed_hosts | @json')" "[\"$host2\"]" \
		"get-xfer allowed_hosts"
	out=$(gw set-xfer-hosts --sp sp0 --rev "$SP_REV" --name x0 \
		--hosts "$host2,$host3")
	refresh_rev sp0
	assert_field "$out" '.xfer_id' "$xferId" "set-xfer-hosts reply xfer_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.xfers.x0.allowed_hosts | @json')" \
		"[\"$host2\",\"$host3\"]" "transfer allowed_hosts after the update"
	# The abort path leaves the origin EXACTLY as it is, so the next syncup
	# resumes it (§11.3): the whole namespace record must be byte-identical.
	local nsBefore
	nsBefore=$(smoke_ns sp0 "$nqn" 1)
	out=$(gw delete-xfer --sp sp0 --rev "$SP_REV" --name x0 --force)
	refresh_rev sp0
	assert_field "$out" '.xfer_id' "$xferId" "delete-xfer --force reply xfer_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.xfers | keys | length')" "0" \
		"the transfer is gone after the abort"
	assert_eq "$(smoke_ns sp0 "$nqn" 1)" "$nsBefore" \
		"the abort path leaves the origin namespace untouched"
	out=$(gw create-xfer --sp sp0 --rev "$SP_REV" --name x0 \
		--ori-nqn "$nqn" --ori-idx 1 --hosts "$host2")
	refresh_rev sp0
	xferId=$(jq_of "$out" '.xfer_id')
	# Step 11 already retired the namespace, so the finalize below could only
	# be asserted as a tautology. Resuming it first is what makes the flip the
	# finalize owes OBSERVABLE — and resuming a namespace is exactly what the
	# abort path above would have left the operator to do.
	gw set-ns-suspended --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1 \
		--suspended=false
	refresh_rev sp0
	assert_field "$(smoke_ns sp0 "$nqn" 1)" '.suspended' "false" \
		"the origin namespace is live again before the finalize"
	out=$(gw delete-xfer --sp sp0 --rev "$SP_REV" --name x0)
	refresh_rev sp0
	assert_field "$out" '.xfer_id' "$xferId" "delete-xfer reply xfer_id"
	# §8.10: the finalize suspends the origin in the SAME transaction that
	# removes the record, which is what closes the window in which a syncup
	# would resume a source the destination is already serving.
	assert_field "$(smoke_ns sp0 "$nqn" 1)" '.suspended' "true" \
		"the finalize path retired the origin namespace"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.xfer_name_list | length')" \
		"0" "sp0 xfer_name_list after the finalize"

	# -------------------------------------------------------------------
	stage 13 "create-clone cl0 at the geometry bounds, its bitmap, its delete"
	# -------------------------------------------------------------------
	# The three source bounds at their limits (validateCloneGeometry): 16
	# slices, a 256 x 4 KiB stripe and a 16384 x 64 KiB block, which is also
	# an exact multiple of the stripe.
	out=$(gw create-clone --sp sp0 --rev "$SP_REV" --name cl0 --dst-td t1 \
		--src-nqn "$srcNqn" --src-idx 1 --src-slices 16 \
		--src-stripe 1048576 --src-block 1073741824)
	refresh_rev sp0
	local cloneId clone
	cloneId=$(jq_of "$out" '.clone_id')
	assert_ne "$cloneId" "0" "create-clone clone_id"
	clone=$(smoke_jq "$(sp_json sp0)" '.clones.cl0 | @json')
	assert_field "$clone" '.clone_id' "$cloneId" "clone clone_id"
	assert_field "$clone" '.dst_td_id' "$t1Id" "clone dst_td_id is t1's"
	assert_field "$clone" '.src_nqn' "$srcNqn" "clone src_nqn"
	assert_field "$clone" '.src_ns_idx' "1" "clone src_ns_idx"
	assert_field "$clone" '.src_slice_cnt' "16" "clone src_slice_cnt"
	assert_field "$clone" '.src_stripe_size' "1048576" "clone src_stripe_size"
	assert_field "$clone" '.src_block_size' "1073741824" "clone src_block_size"
	assert_eq "$(smoke_jq "$clone" '.src_tr_conf_list | length')" "1" \
		"clone src_tr_conf_list"
	# §8.9: a clone starts with no source bitmap at all; the chunks are a pure
	# optimization that may arrive at any time.
	assert_field "$clone" '.bm_cnt' "0" "clone bm_cnt at creation"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.clone_name_list | @json')" \
		'["cl0"]' "sp0 clone_name_list"
	out=$(gw get-clone --sp sp0 --name cl0)
	assert_field "$out" '.clone.clone_id' "$cloneId" "get-clone clone_id"
	assert_field "$out" '.clone.bm_cnt' "0" "get-clone bm_cnt"
	# bm_idx is the SOURCE slice_idx, not an append sequence, and bm_cnt is
	# the high-water mark DeleteClone deletes the chunk keys from.
	out=$(gw append-clone-bm --sp sp0 --rev "$SP_REV" --name cl0 \
		--slice-idx 0 --bm-hex ff00)
	refresh_rev sp0
	assert_field "$out" '.clone_id' "$cloneId" "append-clone-bm reply clone_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.clones.cl0.bm_cnt')" "1" \
		"clone bm_cnt after one chunk"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.clone_bm_idx.cl0 | @json')" '[0]' \
		"the clone's stored chunk indexes"
	assert_eq "$(key_count clone_bitmap)" "1" "clone_bitmap keys"
	assert_field "$(gw get-clone --sp sp0 --name cl0)" '.clone.bm_cnt' "1" \
		"get-clone bm_cnt after the append"
	# The transport list is a REPLACEMENT, never a merge: an address that is
	# gone must stop being retried.
	out=$(gw set-clone-tr --sp sp0 --rev "$SP_REV" --name cl0 \
		--src-tr-addr 127.0.0.2 --src-tr-svc-id 4421)
	refresh_rev sp0
	assert_field "$out" '.clone_id' "$cloneId" "set-clone-tr reply clone_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'.clones.cl0.src_tr_conf_list | length')" "1" \
		"clone src_tr_conf_list after the update"
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'.clones.cl0.src_tr_conf_list[0].tr_addr')" "127.0.0.2" \
		"clone src_tr_conf tr_addr after the update"
	# --force skips the hydration proof (AG4), which is how a clone whose
	# source never existed is abandoned. The deciding STM also RESUMES every
	# namespace backed by the destination td (§8.9 Action) — here the one
	# step 12's finalize retired — so a delete can never leave a namespace
	# ANA-inaccessible with no clone left to explain why.
	out=$(gw delete-clone --sp sp0 --rev "$SP_REV" --name cl0 --force)
	refresh_rev sp0
	assert_field "$out" '.clone_id' "$cloneId" "delete-clone reply clone_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.clones | keys | length')" "0" \
		"the clone record is gone"
	assert_eq "$(key_count clone)" "0" "clone keys after the delete"
	assert_eq "$(key_count clone_bitmap)" "0" "clone_bitmap keys after the delete"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.clone_name_list | length')" \
		"0" "sp0 clone_name_list after the delete"
	assert_field "$(smoke_ns sp0 "$nqn" 1)" '.suspended' "false" \
		"the destination td's namespace was resumed by the delete"

	# -------------------------------------------------------------------
	stage 14 "migration on the first data group: create, bitmap, cancel, finish"
	# -------------------------------------------------------------------
	local dataGrp srcSide srcLeg srcAddr srcSlot dstSide dstAddr grpExt
	dataGrp=$(sp_grp_id sp0 data 0)
	srcLeg=$(sp_leg_id sp0 data 0 0)
	srcSide=$(sp_side_id sp0 data 0 0)
	srcAddr=$(sp_side_addr sp0 data 0 0)
	srcSlot=$(smoke_side_field sp0 "$srcSide" cntlid_slot)
	grpExt=$(smoke_grp_field sp0 "$dataGrp" ext_cnt)
	# §6.5 seeds the destination scan's black list with every DN the group
	# already occupies, which is what keeps the migration out of the failure
	# domain it is meant to leave.
	local grpBlack
	grpBlack=$(smoke_grp_addrs sp0 "$dataGrp")
	for i in 0 1 2 3; do beforeRev[i]=$(dn_rev_of "$(dn_addr "$i")"); done
	out=$(gw create-migr --sp sp0 --rev "$SP_REV" --name m0 --src-side "$srcSide")
	refresh_rev sp0
	local migrId
	migrId=$(jq_of "$out" '.migr_id')
	assert_ne "$migrId" "0" "create-migr migr_id"
	# §8.11: the destination is a SECOND side hung off the source's own leg —
	# they export one NQN and the CN aggregates them as one namespace's paths.
	assert_eq "$(smoke_leg_side_ids sp0 "$srcLeg" | wc -l | tr -d ' ')" "2" \
		"the source leg now carries two sides"
	dstSide=$(smoke_jq "$(sp_json sp0)" '.migrs.m0.dst_side_id')
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.migrs.m0.src_side_id')" "$srcSide" \
		"migration src_side_id"
	assert_ne "$dstSide" "$srcSide" "migration dst_side_id differs from the source"
	assert_field "$(smoke_jq "$(sp_json sp0)" '.migrs.m0 | @json')" '.bm_cnt' \
		"0" "migration bm_cnt at creation"
	# [D15]: only the sp-worker flips provisioned, after the dn agent has
	# zeroed the side — so a fresh destination is always false.
	assert_eq "$(smoke_side_field sp0 "$dstSide" provisioned)" "false" \
		"the destination side is unprovisioned"
	dstAddr=$(smoke_side_field sp0 "$dstSide" addr_port)
	assert_eq "$(smoke_grp_addrs sp0 "$dataGrp" | grep -cx "$dstAddr")" "1" \
		"the destination side joined the group's DN set"
	assert_eq "$(printf '%s\n' "$grpBlack" | grep -cx "$dstAddr")" "0" \
		"the destination landed on a DN the group did not already occupy"
	# The destination exports under a DIFFERENT cntlid slot than the source,
	# because both sides of the leg are exported at once (§11.2).
	assert_ne "$(smoke_side_field sp0 "$dstSide" cntlid_slot)" "$srcSlot" \
		"the destination side's cntlid_slot differs from the source's"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.migr_name_list | @json')" \
		'["m0"]' "sp0 migr_name_list"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		[ "$addr" != "$dstAddr" ] || want=$((want + 1))
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12 - grpExt))" \
		"Σ DN free after the destination side was charged"
	# A migration's chunks are an immutable append sequence numbered by
	# bm_cnt, unlike a clone's slice-addressed ones (§8.11).
	gw append-migr-bm --sp sp0 --rev "$SP_REV" --name m0 --bm-hex aa
	refresh_rev sp0
	gw append-migr-bm --sp sp0 --rev "$SP_REV" --name m0 --bm-hex bb
	refresh_rev sp0
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.migrs.m0.bm_cnt')" "2" \
		"migration bm_cnt after two appends"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.migr_bm_idx.m0 | @json')" '[0,1]' \
		"the migration's stored chunk indexes"
	assert_eq "$(key_count migration_bitmap)" "2" "migration_bitmap keys"
	out=$(gw get-migr --sp sp0 --name m0)
	assert_field "$out" '.migr.migr_id' "$migrId" "get-migr migr_id"
	assert_field "$out" '.migr.src_side_id' "$srcSide" "get-migr src_side_id"
	assert_field "$out" '.migr.dst_side_id' "$dstSide" "get-migr dst_side_id"
	assert_field "$out" '.migr.bm_cnt' "2" "get-migr bm_cnt"
	# CancelMigration throws the unfinished copy away: the destination's
	# extents come back and the source was never touched.
	out=$(gw cancel-migr --sp sp0 --rev "$SP_REV" --name m0)
	refresh_rev sp0
	assert_field "$out" '.migr_id' "$migrId" "cancel-migr reply migr_id"
	assert_eq "$(smoke_leg_side_ids sp0 "$srcLeg" | wc -l | tr -d ' ')" "1" \
		"the leg is back to one side"
	assert_eq "$(smoke_leg_side_ids sp0 "$srcLeg")" "$srcSide" \
		"the surviving side is the source"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.migrs | keys | length')" "0" \
		"the migration record is gone"
	assert_eq "$(key_count migration)" "0" "migration keys after the cancel"
	assert_eq "$(key_count migration_bitmap)" "0" \
		"migration_bitmap keys after the cancel"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.migr_name_list | length')" \
		"0" "sp0 migr_name_list after the cancel"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		[ "$addr" != "$dstAddr" ] || want=$((want + 1))
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12))" \
		"Σ DN free restored by the cancel"
	# The finish path, on a fresh migration. --force skips the "fully
	# hydrated" proof the destination DN would otherwise have to give (AG4) —
	# no worker ever pushed a dm-clone status line to these fakes.
	out=$(gw create-migr --sp sp0 --rev "$SP_REV" --name m1 --src-side "$srcSide")
	refresh_rev sp0
	migrId=$(jq_of "$out" '.migr_id')
	dstSide=$(smoke_jq "$(sp_json sp0)" '.migrs.m1.dst_side_id')
	dstAddr=$(smoke_side_field sp0 "$dstSide" addr_port)
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		[ "$addr" != "$dstAddr" ] || beforeRev[i]=$((beforeRev[i] + 1))
	done
	out=$(gw finish-migr --sp sp0 --rev "$SP_REV" --name m1 --force)
	refresh_rev sp0
	assert_field "$out" '.migr_id' "$migrId" "finish-migr reply migr_id"
	assert_eq "$(smoke_leg_side_ids sp0 "$srcLeg" | wc -l | tr -d ' ')" "1" \
		"the leg is back to one side after the finish"
	assert_eq "$(smoke_leg_side_ids sp0 "$srcLeg")" "$dstSide" \
		"the surviving side is the DESTINATION"
	assert_eq "$(key_count migration)" "0" "migration keys after the finish"
	assert_eq "$(key_count migration_bitmap)" "0" \
		"migration_bitmap keys after the finish"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.migr_name_list | length')" \
		"0" "sp0 migr_name_list after the finish"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		# The source DN gave its extents back, so its DnRev moved once more.
		[ "$addr" != "$srcAddr" ] || want=$((want + 1))
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12))" \
		"Σ DN free is unchanged by a completed migration"
	assert_eq "$(smoke_jq "$(dn_json "$srcAddr")" \
		'[ .side_ptr_list[] | select(.side_id == $s) ] | length' \
		--arg s "$srcSide")" "0" \
		"the source DN's side pointer went with the released side"

	# -------------------------------------------------------------------
	stage 15 "spare legs: create, the unprovisioned refusal, switch, delete"
	# -------------------------------------------------------------------
	local spareLeg spareSide spareAddr targetLeg targetPos targetAddr
	grpBlack=$(smoke_grp_addrs sp0 "$dataGrp")
	out=$(gw create-spare --sp sp0 --rev "$SP_REV" --grp "$dataGrp")
	refresh_rev sp0
	spareLeg=$(jq_of "$out" '.leg_id')
	assert_ne "$spareLeg" "0" "create-spare leg_id"
	assert_eq "$(smoke_grp_spare_ids sp0 "$dataGrp")" "$spareLeg" \
		"the group's spare_leg_list holds exactly the new leg"
	spareSide=$(smoke_leg_side_ids sp0 "$spareLeg")
	spareAddr=$(smoke_side_field sp0 "$spareSide" addr_port)
	# [D15] again: a fresh spare is unprovisioned, which is exactly why it
	# cannot be switched in yet.
	assert_eq "$(smoke_side_field sp0 "$spareSide" provisioned)" "false" \
		"the spare's side is unprovisioned"
	# The spare exists to survive the loss of a DN the group already uses, so
	# §6.5's scan is black-listed off every one of them.
	assert_eq "$(printf '%s\n' "$grpBlack" | grep -cx "$spareAddr")" "0" \
		"the spare landed on a DN the group did not already occupy"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		[ "$addr" != "$spareAddr" ] || want=$((want + 1))
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12 - grpExt))" \
		"Σ DN free after the spare was charged (a spare occupies a DN like an active leg)"
	targetLeg=$(smoke_jq "$(sp_json sp0)" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) | .leg_list[]?.leg_id ][0]' \
		--arg g "$dataGrp")
	targetPos=$(smoke_jq "$(sp_json sp0)" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) | .leg_list[]?.leg_id ] | index($l) // -1' \
		--arg g "$dataGrp" --arg l "$targetLeg")
	targetAddr=$(smoke_side_field sp0 \
		"$(smoke_leg_side_ids sp0 "$targetLeg")" addr_port)
	# §9.4: switching to a side that has not finished zeroing would put an
	# unwritten member into the md array, so the refusal must write nothing.
	assert_no_write "switch-spare with an unprovisioned spare" \
		gwx FAILED_PRECONDITION switch-spare --sp sp0 --rev "$SP_REV" \
		--grp "$dataGrp" --spare "$spareLeg" --target "$targetLeg"
	assert_eq "$(sp_rev_of sp0)" "$SP_REV" "SpRev after the refused switch"
	# Playing the worker again (§2.4): FlipProvisioned is the same model op
	# the sp-worker calls once the dn agent reports the side zeroed, and it
	# bumps SpRev, so the cached token has to be refreshed.
	out=$(wctl set-provisioned --sp sp0 --slice "$slice" --leg "$spareLeg" \
		--side "$spareSide")
	assert_field "$out" '.flipped' "true" "set-provisioned flipped the spare's side"
	refresh_rev sp0
	assert_eq "$(smoke_side_field sp0 "$spareSide" provisioned)" "true" \
		"the spare's side is provisioned"
	out=$(gw switch-spare --sp sp0 --rev "$SP_REV" --grp "$dataGrp" \
		--spare "$spareLeg" --target "$targetLeg")
	refresh_rev sp0
	assert_field "$out" '.curr_active_leg_id' "$spareLeg" \
		"switch-spare reply curr_active_leg_id"
	assert_field "$out" '.curr_spare_leg_id' "$targetLeg" \
		"switch-spare reply curr_spare_leg_id"
	# §8.12: the spare takes the target's POSITION in leg_list — the md member
	# slot the array is missing — and the target is parked, not deleted.
	assert_eq "$(smoke_grp_leg_ids sp0 "$dataGrp" | sed -n "$((targetPos + 1))p")" \
		"$spareLeg" "the spare took the target's position in leg_list"
	assert_eq "$(smoke_grp_spare_ids sp0 "$dataGrp")" "$targetLeg" \
		"the replaced leg is parked in spare_leg_list"
	# A switch moves no extents: both legs already own their DN's space.
	for i in 0 1 2 3; do smoke_check_dn sp0 "$(dn_addr "$i")" "${beforeRev[i]}"; done
	out=$(gw delete-spare --sp sp0 --rev "$SP_REV" --grp "$dataGrp" \
		--leg "$targetLeg")
	refresh_rev sp0
	assert_field "$out" '.leg_id' "$targetLeg" "delete-spare reply leg_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '
		[ .slices[] | (.meta_grp_list[]?, .data_grp_list[]?)
		  | select(.grp_id == $g) | .spare_leg_list[]? ] | length' \
		--arg g "$dataGrp")" "0" "the group has no spare leg left"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		want=${beforeRev[i]}
		[ "$addr" != "$targetAddr" ] || want=$((want + 1))
		smoke_check_dn sp0 "$addr" "$want"
		beforeRev[i]=$want
	done
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS - 12))" \
		"Σ DN free after the parked leg was released"

	# -------------------------------------------------------------------
	stage 16 "get-td-bm / get-leg-bm and the two inspects"
	# -------------------------------------------------------------------
	# GW14/[D-J]: the agent's bytes travel verbatim, and this fake replies an
	# empty bitmap, so the pass-through is an empty hex string of length 0.
	out=$(gw get-td-bm --sp sp0 --td t0 --slice-idx 0)
	assert_field "$out" '.byte_cnt' "0" "get-td-bm byte_cnt"
	assert_field "$out" '.bitmap_hex' "" "get-td-bm bitmap_hex"
	local metaLeg metaSide metaAddr
	metaLeg=$(sp_leg_id sp0 meta 0 0)
	metaSide=$(sp_side_id sp0 meta 0 0)
	metaAddr=$(sp_side_addr sp0 meta 0 0)
	out=$(gw get-leg-bm --sp sp0 --leg "$metaLeg")
	assert_field "$out" '.byte_cnt' "0" "get-leg-bm byte_cnt"
	assert_field "$out" '.bitmap_hex' "" "get-leg-bm bitmap_hex"
	# The two inspects need a fake that already knows the object, and no
	# worker in this suite ever syncs one up — so the state is hand-written,
	# which is the fake's documented operator-edit path (§14.9). Without it
	# both replies would be a truthful but empty null info (AG3).
	local primary primaryId primaryAddr primaryCnId primaryDir sideDir sideDnId
	primary=$(sp_primary_cntlr sp0)
	primaryId=${primary%% *}
	primaryAddr=${primary##* }
	primaryCnId=$(cn_id_of "$primaryAddr")
	primaryDir=$(smoke_cn_dir "$primaryAddr")
	set_state "$primaryDir" <<EOF
{"objects":{
  "cn":{"revision":7,"request":{"cluster_id":"$CID","cn_id":"$primaryCnId",
        "revision":"7","cntlr_pointer_list":[
          {"sp_id":"$spId","cntlr_id":"$primaryId"}]}},
  "cntlr $spId:$primaryId":{"revision":7,"request":{
        "cluster_id":"$CID","cn_id":"$primaryCnId",
        "cntlr_pointer":{"sp_id":"$spId","cntlr_id":"$primaryId"},
        "revision":"7","cntlr":{"primary":true},
        "td_list":[{"td_id":"$t0Id"}]}}}}
EOF
	sideDir=$(smoke_dn_dir "$metaAddr")
	sideDnId=$(dn_id_of "$metaAddr")
	set_state "$sideDir" <<EOF
{"objects":{
  "dn":{"revision":7,"request":{"cluster_id":"$CID","dn_id":"$sideDnId",
        "revision":"7","side_pointer_list":[
          {"sp_id":"$spId","leg_id":"$metaLeg","side_id":"$metaSide"}]}},
  "side $spId:$metaLeg:$metaSide":{"revision":7,"request":{
        "cluster_id":"$CID","dn_id":"$sideDnId",
        "side_pointer":{"sp_id":"$spId","leg_id":"$metaLeg",
                        "side_id":"$metaSide"},
        "revision":"7",
        "side_conf":{"ext_cnt":"1","primary_cn_id":"$primaryCnId"}}}}}
EOF
	# architecture.md §8.6: the reply's applied_revision is the agent's own
	# last applied revision for the object, passed through with the info. The
	# seeded 7 is distinctive — the stored sp_rev is far past 7 by this
	# stage, so a handler that regressed to the store cannot pass.
	out=$(gw inspect-cntlr --sp sp0 --id "$primaryId")
	assert_field "$out" '.applied_revision' "7" \
		"inspect-cntlr applied_revision is the seeded agent revision"
	assert_ne "$(jq_of "$out" '.cntlr_info')" "null" "inspect-cntlr cntlr_info"
	assert_field "$out" ".cntlr_info.td_id_to_raid0[\"$t0Id\"].status" \
		"RES_STATUS_OK" "inspect-cntlr reports the seeded td's raid0 row"
	out=$(gw inspect-side --sp sp0 --id "$metaSide")
	assert_field "$out" '.applied_revision' "7" \
		"inspect-side applied_revision is the seeded agent revision"
	assert_ne "$(jq_of "$out" '.side_info')" "null" "inspect-side side_info"
	assert_field "$out" '.side_info.side_dev_info.status' "RES_STATUS_OK" \
		"inspect-side side_dev_info status"
	assert_field "$out" '.side_info.total_ext_cnt' "1" \
		"inspect-side total_ext_cnt is the seeded side_conf.ext_cnt"

	# -------------------------------------------------------------------
	stage 17 "reverse teardown down to an empty store"
	# -------------------------------------------------------------------
	out=$(gw delete-ns --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1)
	refresh_rev sp0
	assert_field "$out" '.ns_id' "$nsId" "delete-ns reply ns_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" \
		'.subsystems[$n].ns_list | length' --arg n "$nqn")" "0" \
		"the subsystem has no namespace left"
	out=$(gw delete-ss --sp sp0 --rev "$SP_REV" --nqn "$nqn")
	refresh_rev sp0
	assert_field "$out" '.ss_id' "$ssId" "delete-ss reply ss_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.nqn_list | length')" "0" \
		"sp0 nqn_list after delete-ss"
	assert_eq "$(key_count subsystem)" "0" "subsystem keys after delete-ss"
	# The CdcEntry goes in the SAME transaction: leaving it would keep dnv-cdc
	# advertising a subsystem no cntlr serves, and nvme-stas hosts would keep
	# reconnecting to it (§8.8).
	assert_eq "$(key_count cdc)" "0" "cdc keys after delete-ss"
	# The snapshot-child gate, observed: t1 is an uncreated snapshot of t0, so
	# t0 cannot go first — retire runs before build (CN9), and a delete of the
	# origin now would send `delete {ori dev_id}` before `create_snap`.
	assert_no_write "delete-td of an origin with an uncreated snapshot" \
		gwx FAILED_PRECONDITION delete-td --sp sp0 --rev "$SP_REV" --name t0
	out=$(gw delete-td --sp sp0 --rev "$SP_REV" --name t1)
	refresh_rev sp0
	assert_field "$out" '.td_id' "$t1Id" "delete-td t1 reply td_id"
	out=$(gw delete-td --sp sp0 --rev "$SP_REV" --name t0)
	refresh_rev sp0
	assert_field "$out" '.td_id' "$t0Id" "delete-td t0 reply td_id"
	assert_eq "$(smoke_jq "$(sp_json sp0)" '.sp_conf.td_name_list | length')" \
		"0" "sp0 td_name_list after both deletes"
	assert_eq "$(key_count thin_device)" "0" "thin_device keys"
	out=$(gw delete-sp --sp sp0 --rev "$SP_REV")
	assert_field "$out" '.sp_id' "$spId" "delete-sp reply sp_id"
	# §5.4: every key the SP implied is gone, and the whole footprint returns
	# to the nodes — one write, one capacity key and one revision bump per
	# node however many sides of this SP it carried.
	assert_eq "$(key_count sp_conf)" "0" "sp_conf keys after delete-sp"
	assert_eq "$(key_count sp_rev)" "0" "sp_rev keys after delete-sp"
	assert_eq "$(key_count sp_id_to_name)" "0" "sp_id_to_name keys"
	assert_eq "$(key_count cntlr)" "0" "cntlr keys after delete-sp"
	assert_eq "$(key_count slice)" "0" "slice keys after delete-sp"
	global=$(raw_key "$DNV_PREFIX sp_global $cidHex")
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "0" \
		"SpGlobal Σbucket after delete-sp"
	# GW12: the bucket shrinks, next_id never rewinds — a deleted sp_id must
	# never come back.
	assert_field "$global" '.next_id' "2" "SpGlobal next_id never rewinds"
	assert_eq "$(smoke_dn_free_sum)" "$((4 * DN_EXTENTS))" \
		"Σ DN free fully restored"
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		assert_eq "$(dn_free "$addr")" "$DN_EXTENTS" "$addr free after delete-sp"
		assert_eq "$(smoke_jq "$(dn_json "$addr")" '.side_ptr_list | length')" \
			"0" "$addr side_ptr_list after delete-sp"
		assert_eq "$(smoke_cap_free dn_capacity "$addr")" "$DN_EXTENTS" \
			"$addr dn_capacity key after delete-sp"
	done
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		assert_eq "$(cn_free "$addr")" "$CN_EXTENTS" "$addr free after delete-sp"
		assert_eq "$(smoke_jq "$(cn_json "$addr")" '.cntlr_ptr_list | length')" \
			"0" "$addr cntlr_ptr_list after delete-sp"
		assert_eq "$(smoke_cap_free cn_capacity "$addr")" "$CN_EXTENTS" \
			"$addr cn_capacity key after delete-sp"
	done
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		rev=$(dn_rev_of "$addr")
		out=$(gw delete-dn --addr "$addr" --rev "$rev")
		assert_field "$out" '.dn_id' "$((i + 1))" "delete-dn $addr reply dn_id"
	done
	assert_eq "$(key_count dn_conf)" "0" "dn_conf keys after the DN teardown"
	assert_eq "$(key_count dn_capacity)" "0" "dn_capacity keys after the teardown"
	assert_eq "$(key_count dn_rev)" "0" "dn_rev keys after the teardown"
	global=$(raw_key "$DNV_PREFIX dn_global $cidHex")
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "0" "DnGlobal Σbucket"
	assert_field "$global" '.next_id' "5" "DnGlobal next_id never rewinds"
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		rev=$(cn_rev_of "$addr")
		out=$(gw delete-cn --addr "$addr" --rev "$rev")
		assert_field "$out" '.cn_id' "$((i + 1))" "delete-cn $addr reply cn_id"
	done
	assert_eq "$(key_count cn_conf)" "0" "cn_conf keys after the CN teardown"
	assert_eq "$(key_count cn_capacity)" "0" "cn_capacity keys after the teardown"
	assert_eq "$(key_count cn_rev)" "0" "cn_rev keys after the teardown"
	global=$(raw_key "$DNV_PREFIX cn_global $cidHex")
	assert_eq "$(jq_of "$global" '.shard_bucket | add')" "0" "CnGlobal Σbucket"
	assert_field "$global" '.next_id' "4" "CnGlobal next_id never rewinds"
	# §8.1: the emptiness gate is Σbucket == 0 on all three globals, and the
	# four keys the cluster is made of go together.
	out=$(gw delete-cluster)
	assert_field "$out" '.cluster_id' "$CID" "delete-cluster echoes the cluster_id"
	assert_eq "$(key_count dnv)" "0" "the whole dnv prefix is empty"
}

# ---------------------------------------------------------------------------
# Case A — `parallel` (§10.12): 3 gateways, disjoint resources
# ---------------------------------------------------------------------------
#
# Every wave below is ONE `race` invocation whose --targets names all three
# instances, so the jobs round-robin across gw0/gw1/gw2 and are released
# together against race's barrier (§10.8). That is §10.1(b)'s disjoint half:
# work that shares no object must ALL succeed, and the state the jobs DO share
# — the three *Global counters, the per-DN and per-CN ledgers, the capacity
# index of §5.6 — must land exactly where a sequential run would have left it.
# The gateways are stateless (§0 #3): correctness here rests entirely on
# RunSTM's serializable-snapshot isolation plus the §5.5 tokens.
#
# Token discipline (§10.10): every SP-scoped wave fetches one token PER SP
# through the gateway's own read path, in the PARENT shell, and bakes it into
# that job's line before the wave is released. The harness's cached $SP_REV is
# deliberately never used in this case — it holds ONE SP's token and this case
# drives ten of them at once — so `refresh_rev` has nothing to refresh here.

# parallel_race releases one wave. Jobs arrive as JSON lines on stdin and the
# result array comes back on stdout; ssh forwards our stdin to gatewayctl on
# the server, so the wave never touches a temporary file.
#
# --timeout 30 rather than the 10 s default: ten allocating RPCs contend on the
# same four DN and three CN keys, and GW9 re-runs a whole candidate unit
# (scan + STM) after every "candidate changed", so a job's WALL time is a
# retry queue, not one round trip. The timeout must not be the thing that
# decides a wave (§0 #13: this suite asserts correctness, never latency).
parallel_race() { # jobs on stdin -> the race result array on stdout
	sshw "$WORK/bin/gatewayctl --cluster $CLUSTER --trace-id $TRACE" \
		"--timeout 30 race --targets" \
		"$(gw_addr 0),$(gw_addr 1),$(gw_addr 2)"
}

# parallel_race_codes is the histogram of a wave's outcome codes. It goes into
# the LABEL of the OK-count assertion, so a failing wave names what the other
# jobs actually returned instead of just "got 9, want 10".
parallel_race_codes() { # <race array>
	jq_of "$1" '[.[] | .code] | group_by(.) | map({(.[0]): length}) | add | @json'
}

parallel_ok_cnt() { # <race array>
	jq_of "$1" '[.[] | select(.code == "OK")] | length'
}

# The §10.5 identity plan for this case: spA0..spA9, one td `t0`, one
# subsystem `<prefix>:<cluster>:<sp>:ss0` and one namespace at ns_idx 1 per SP.
parallel_sp() { printf 'spA%d' "$1"; }
parallel_nqn() { printf '%s:%s:%s:ss0' "$NQN_PREFIX" "$CLUSTER" "$(parallel_sp "$1")"; }

# parallel_global reads one of the three cluster-wide counters as ground truth.
parallel_global() { # <dn|cn|sp>
	wctl get --key "$DNV_PREFIX ${1}_global $(printf '%016x' "$CID")"
}

# parallel_bucket_sum is Σ shard_bucket of one global: the number of live
# objects of that kind, spread over the 256 shards. `// 0` because an
# all-consumed bucket is still 256 entries but `add` on an empty list is null.
parallel_bucket_sum() { # <global json>
	jq_of "$1" '((.shard_bucket // []) | add) // 0'
}

# parallel_node_rev reads a node's *Rev key — the §5.5 token — straight from
# etcd. The key is addressed by id + shard, both of which live in the conf the
# caller already fetched, so this stays one extra read per node.
parallel_node_rev() { # <dn|cn> <DnConf|CnConf json>
	wctl get-rev "$1" --id "$(jq_of "$2" ".${1}_id")" \
		--shard "$(printf '%02x' "$(jq_of "$2" '.shard_code')")"
}

# parallel_sp_rev reads an SP's SpRev key as ground truth. (`wctl get-sp`'s
# `store_rev` is etcd's own revision, NOT the §5.5 token, so it can never
# stand in for this.)
parallel_sp_rev() { # <sp name>
	local conf
	conf=$(sp_json "$1")
	wctl get-rev sp --id "$(jq_of "$conf" '.sp_conf.sp_id')" \
		--shard "$(printf '%02x' "$(jq_of "$conf" '.sp_conf.shard_code')")"
}

# parallel_cap_key_cnt counts the capacity keys that name exactly this node at
# exactly this free count. §5.6 puts free_ext_cnt INTO the key (as %016x) so
# the §6.3/§6.4 scans need no point reads, which means a stale key is a silent
# allocator bug that no *Conf assertion would ever catch — hence rebuilding the
# key's tail from the conf and demanding exactly one match. The bin field of a
# dn_capacity key is not rebuilt: matching the " <free> <addr>" tail pins the
# whole distinguishing part of the key without duplicating §6.2's binning.
parallel_cap_key_cnt() { # <list-keys output> <free_ext_cnt> <addr_port>
	printf '%s\n' "$1" | grep -c -F -- "$(printf ' %016x %s' "$2" "$3")" || true
}

case_parallel() {
	CASE=parallel

	local out wave i sp nqn rev addr conf keys used
	local sp_ids="" td_ids=""

	# -------------------------------------------------------------------
	stage 1 "create-cluster itgw on gw0 (§10.12 step 1)"
	# -------------------------------------------------------------------
	# The one sequential step: every later wave needs the cluster's four keys
	# to exist, and §10.11 step 1 already proves the RPC's full write set, so
	# here only the preconditions the waves depend on are re-asserted.
	new_cluster

	conf=$(wctl get --key "$(cluster_key)")
	assert_ne "$(jq_of "$conf" '.creation_epoch')" "0" "cluster_conf creation_epoch"
	for i in dn cn sp; do
		out=$(parallel_global "$i")
		assert_field "$out" '.next_id' "1" "${i}_global next_id at cluster creation"
		assert_eq "$(jq_of "$out" '.shard_bucket | length')" "256" \
			"${i}_global shard_bucket length"
		assert_eq "$(parallel_bucket_sum "$out")" "0" \
			"${i}_global Σ shard_bucket at cluster creation"
	done
	# ③ the gateway's own read path agrees with etcd.
	out=$(gw get-cluster)
	assert_field "$out" '.cluster_id' "$CID" "get-cluster cluster_id"
	assert_field "$out" '.dn_global.next_id' "1" "get-cluster dn_global next_id"

	# -------------------------------------------------------------------
	stage 2 "one wave: 4 create-dn + 3 create-cn across gw0-2 (§10.12 step 2)"
	# -------------------------------------------------------------------
	# Seven jobs on seven disjoint addr_ports, but all four DN jobs mint their
	# dn_id out of the SAME dn_global key and all three CN jobs out of the same
	# cn_global — so this wave is exactly the "disjoint work, shared counter"
	# shape. Ids are asserted as a SET, never per job: which instance served
	# which addr_port is not defined, and a test that pinned it would be
	# asserting the round-robin rather than the invariant.
	wave=""
	for i in 0 1 2 3; do
		wave+=$(printf '{"op":"create-dn","params":{"addr":"%s","location":"%s","tr-svc-id":"%s"}}' \
			"$(dn_addr "$i")" "$(dn_loc "$i")" "$(dn_svcid "$i")")$'\n'
	done
	for i in 0 1 2; do
		wave+=$(printf '{"op":"create-cn","params":{"addr":"%s","location":"%s","tr-svc-id":"%s"}}' \
			"$(cn_addr "$i")" "$(cn_loc "$i")" "$(cn_svcid "$i")")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)

	assert_eq "$(parallel_ok_cnt "$out")" "7" \
		"node wave: OK jobs (codes $(parallel_race_codes "$out"))"
	# The id sets: four DNs got 1..4 and three CNs got 1..3, in some order.
	# A duplicate or a gap here is the classic lost-update of a counter read
	# and written by three instances at once.
	assert_eq "$(jq_of "$out" \
		'[.[] | select(.op == "create-dn") | .reply.dn_id | tonumber] | sort | @json')" \
		'[1,2,3,4]' "node wave: the dn_id set"
	assert_eq "$(jq_of "$out" \
		'[.[] | select(.op == "create-cn") | .reply.cn_id | tonumber] | sort | @json')" \
		'[1,2,3]' "node wave: the cn_id set"

	# ② ground truth per node: the conf, the capacity key and the rev key —
	# the three keys CreateDiskNode/CreateControllerNode write (§5.4).
	keys=$(wctl list-keys --prefix dn_capacity)
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		conf=$(dn_json "$addr")
		assert_ne "$(jq_of "$conf" '.dn_id')" "0" "$addr: dn_id"
		assert_field "$conf" '.total_ext_cnt' "$DN_EXTENTS" "$addr: total_ext_cnt"
		assert_field "$conf" '.free_ext_cnt' "$DN_EXTENTS" "$addr: free_ext_cnt"
		assert_field "$conf" '.location' "$(dn_loc "$i")" "$addr: location"
		assert_field "$conf" '.nvme_tr_conf.tr_svc_id' "$(dn_svcid "$i")" \
			"$addr: tr_svc_id"
		assert_field "$conf" '.disabled' "false" "$addr: disabled"
		assert_eq "$(jq_of "$conf" '.side_ptr_list | length')" "0" \
			"$addr: side_ptr_list on a fresh DN"
		assert_eq "$(parallel_cap_key_cnt "$keys" "$DN_EXTENTS" "$addr")" "1" \
			"$addr: its dn_capacity key at free $DN_EXTENTS"
		# Every rev starts at 1 (§5.5); it is the token the SP waves will
		# consume, and a node created twice would show up here as a 2.
		out=$(parallel_node_rev dn "$conf")
		assert_field "$out" '.revision' "1" "$addr: dn_rev revision"
		assert_field "$out" '.addr_port' "$addr" "$addr: dn_rev addr_port"
		# ③ read-back through the gateway.
		out=$(gw get-dn --addr "$addr")
		assert_field "$out" '.dn_conf.free_ext_cnt' "$DN_EXTENTS" \
			"$addr: get-dn free_ext_cnt"
		assert_field "$out" '.dn_rev.revision' "1" "$addr: get-dn dn_rev"
	done
	keys=$(wctl list-keys --prefix cn_capacity)
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		conf=$(cn_json "$addr")
		assert_ne "$(jq_of "$conf" '.cn_id')" "0" "$addr: cn_id"
		# §6.1: the fakes answer GetCnSize with 0, which is what makes the
		# gateway apply DefaultCnCap (4 TiB / 1 GiB = 4096 extents).
		assert_field "$conf" '.total_ext_cnt' "$CN_EXTENTS" "$addr: total_ext_cnt"
		assert_field "$conf" '.free_ext_cnt' "$CN_EXTENTS" "$addr: free_ext_cnt"
		assert_field "$conf" '.location' "$(cn_loc "$i")" "$addr: location"
		assert_eq "$(jq_of "$conf" '.cntlr_ptr_list | length')" "0" \
			"$addr: cntlr_ptr_list on a fresh CN"
		assert_eq "$(parallel_cap_key_cnt "$keys" "$CN_EXTENTS" "$addr")" "1" \
			"$addr: its cn_capacity key at free $CN_EXTENTS"
		out=$(parallel_node_rev cn "$conf")
		assert_field "$out" '.revision' "1" "$addr: cn_rev revision"
		assert_field "$out" '.addr_port' "$addr" "$addr: cn_rev addr_port"
		out=$(gw get-cn --addr "$addr")
		assert_field "$out" '.cn_conf.free_ext_cnt' "$CN_EXTENTS" \
			"$addr: get-cn free_ext_cnt"
		assert_field "$out" '.cn_rev.revision' "1" "$addr: get-cn cn_rev"
	done
	assert_eq "$(key_count dn_capacity)" "4" "dn_capacity keys after the wave"
	assert_eq "$(key_count cn_capacity)" "3" "cn_capacity keys after the wave"

	# The shared counters, which is where a concurrency bug would actually
	# show: next_id 5/4 means each of the seven jobs advanced it exactly once,
	# Σbucket 4/3 that each also claimed exactly one shard slot (GW12).
	out=$(parallel_global dn)
	assert_field "$out" '.next_id' "5" "dn_global next_id after 4 create-dn"
	assert_eq "$(parallel_bucket_sum "$out")" "4" "dn_global Σ shard_bucket"
	out=$(parallel_global cn)
	assert_field "$out" '.next_id' "4" "cn_global next_id after 3 create-cn"
	assert_eq "$(parallel_bucket_sum "$out")" "3" "cn_global Σ shard_bucket"

	# ③ the gateway lists exactly the seven nodes.
	assert_eq "$(jq_of "$(gw list-dns)" '.addr_port | length')" "4" "list-dns"
	assert_eq "$(jq_of "$(gw list-cns)" '.addr_port | length')" "3" "list-cns"

	# The whole-store baseline step 6 must come back to: 1 cluster_conf + 3
	# globals + 4×(dn_conf, dn_capacity, dn_rev) + 3×(cn_conf, cn_capacity,
	# cn_rev) = 25 keys. Captured here so the final audit compares against
	# what this case actually built, not against a number in a comment.
	local base_keys
	base_keys=$(key_count dnv)
	assert_eq "$base_keys" "25" "keys in the store after the node wave"

	# -------------------------------------------------------------------
	stage 3 "one wave: 10 create-sp spA0..spA9 (§10.12 step 3)"
	# -------------------------------------------------------------------
	# Ten allocating RPCs against four DNs and three CNs at once. Each one
	# scans the capacity index outside its STM and commits inside it, so most
	# of them WILL lose a candidate unit to a neighbour; GW9 (§0 #8) re-runs
	# the unit and never surfaces "candidate changed", so the only correct
	# outcome is ten OKs and an exact ledger.
	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		wave+=$(printf '{"op":"create-sp","params":{"sp":"%s","cntlr-cnt":%s,"slice-cnt":%s,"init-ext-cnt":%s,"raid1":true}}' \
			"$(parallel_sp "$i")" "$SP_CNTLR_CNT" "$SP_SLICE_CNT" \
			"$SP_INIT_EXT")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)

	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"sp wave: OK jobs (codes $(parallel_race_codes "$out"))"
	sp_ids=$(jq_of "$out" '[.[] | .reply.sp_id | tonumber] | sort | @json')
	assert_eq "$(jq_of "$out" '[.[] | .reply.sp_id] | unique | length')" \
		"$PAR_CLIENTS" "sp wave: sp_ids are a $PAR_CLIENTS-set"
	out=$(parallel_global sp)
	assert_field "$out" '.next_id' "$((PAR_CLIENTS + 1))" \
		"sp_global next_id after $PAR_CLIENTS create-sp"
	assert_eq "$(parallel_bucket_sum "$out")" "$PAR_CLIENTS" \
		"sp_global Σ shard_bucket"

	# AGGREGATE accounting. Which DN carries which leg is the allocator's
	# choice (§6.3 walks the bins, and ten concurrent scans see ten different
	# stores), so the per-node numbers are NOT predictable — but the total is:
	# every SP charges SP_EXT_COST extents (meta 1×2 legs + data 2×2 legs) and
	# nothing else may have moved.
	used=0
	for i in 0 1 2 3; do
		used=$((used + DN_EXTENTS - $(dn_free "$(dn_addr "$i")")))
	done
	assert_eq "$used" "$((PAR_CLIENTS * SP_EXT_COST))" \
		"Σ over the DNs of (total − free) after $PAR_CLIENTS SPs"
	# The CN side of the same identity: §6.5 charges every cntlr the SP's
	# WHOLE footprint (meta 1 + data 2 = 3), and each SP has SP_CNTLR_CNT of
	# them, so the CNs carry cntlr_cnt × footprint per SP.
	used=0
	for i in 0 1 2; do
		used=$((used + CN_EXTENTS - $(cn_free "$(cn_addr "$i")")))
	done
	assert_eq "$used" "$((PAR_CLIENTS * SP_CNTLR_CNT * 3))" \
		"Σ over the CNs of (total − free) after $PAR_CLIENTS SPs"

	# §5.6: the capacity key must have been rewritten to the node's new free
	# count by every single one of the ten commits that touched it.
	keys=$(wctl list-keys --prefix dn_capacity)
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		assert_eq "$(parallel_cap_key_cnt "$keys" "$(dn_free "$addr")" "$addr")" \
			"1" "$addr: dn_capacity key matches its DnConf free count"
	done
	assert_eq "$(key_count dn_capacity)" "4" "dn_capacity keys after the sp wave"
	keys=$(wctl list-keys --prefix cn_capacity)
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		assert_eq "$(parallel_cap_key_cnt "$keys" "$(cn_free "$addr")" "$addr")" \
			"1" "$addr: cn_capacity key matches its CnConf free count"
	done
	assert_eq "$(key_count cn_capacity)" "3" "cn_capacity keys after the sp wave"

	# Per SP: the complete §5.4 write set (verify_sp is the mechanical form of
	# §10.15 step 3's "either the whole write set or no key at all"), plus the
	# token, which must be 1 — a second bump would mean something wrote this
	# SP twice.
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		verify_sp "$sp"
		assert_field "$(parallel_sp_rev "$sp")" '.revision' "1" \
			"$sp: sp_rev revision after creation"
		assert_eq "$(sp_rev_of "$sp")" "1" "$sp: get-sp sp_rev read-back"
	done
	assert_eq "$(jq_of "$(gw list-sps)" '.sp_name | length')" "$PAR_CLIENTS" \
		"list-sps after the sp wave"

	# -------------------------------------------------------------------
	stage 4 "one wave: 10 create-td, one per SP (§10.12 step 4)"
	# -------------------------------------------------------------------
	# The first token-carrying wave. Each job gets ITS OWN SP's token, read
	# here in the parent shell one `get-sp` at a time and baked into the job
	# line — §0 #7: a wrong token can never match, so a wave built
	# from one shared token would be nine ABORTEDs. The tokens are sent as
	# JSON strings, not numbers: race's params decode through float64 and a
	# uint64 token has no business going near a float.
	local tdId
	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"create-td","params":{"sp":"%s","rev":"%s","name":"t0","size":%s}}' \
			"$sp" "$rev" "$TD_SIZE")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)

	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"td wave: OK jobs (codes $(parallel_race_codes "$out"))"
	# td_id comes from the SP's OWN next_id counter (§5.4), not from a
	# cluster-wide one, so ten SPs of identical shape mint the SAME td_id —
	# and that is the assertion: one distinct value, not ten. Only the
	# cluster-scoped ids of step 2 (dn_id, cn_id, sp_id) are a set.
	assert_eq "$(jq_of "$out" '[.[] | .reply.td_id] | unique | length')" \
		"1" "td wave: every SP mints the same td_id from its own counter"
	tdId=$(jq_of "$out" '[.[] | .reply.td_id] | unique | .[0]')
	assert_ne "$tdId" "0" "td wave: td_id is never the reserved 0"
	# dev_id is uint32, so protojson renders it as a NUMBER, not a string.
	# Every SP mints its own dev_ids from its own SpConf, so all ten are 1.
	assert_eq "$(jq_of "$out" '[.[] | .reply.dev_id] | unique | @json')" '[1]' \
		"td wave: every dev_id is the SP's first"
	assert_eq "$(key_count thin_device)" "$PAR_CLIENTS" "thin_device keys"

	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		conf=$(sp_json "$sp")
		assert_eq "$(jq_of "$conf" '.tds | keys | @json')" '["t0"]' \
			"$sp: the stored td set"
		assert_field "$conf" '.tds.t0.size' "$TD_SIZE" "$sp: t0 size"
		assert_field "$conf" '.tds.t0.dev_id' "1" "$sp: t0 dev_id"
		# created is the sp-worker's flip (§2.4) and no worker runs in this
		# suite, so it must still be false.
		assert_field "$conf" '.tds.t0.created' "false" "$sp: t0 created"
		assert_eq "$(jq_of "$conf" '.sp_conf.td_name_list | @json')" '["t0"]' \
			"$sp: td_name_list"
		assert_field "$conf" '.sp_conf.next_dev_id' "2" "$sp: next_dev_id"
		# One mutation, one bump (§5.5): the token must be exactly 2.
		assert_eq "$(sp_rev_of "$sp")" "2" "$sp: sp_rev after create-td"
		# ③ the gateway's list agrees with the etcd row.
		assert_field "$(gw list-tds --sp "$sp")" '.name_to_td.t0.td_id' \
			"$(jq_of "$conf" '.tds.t0.td_id')" "$sp: list-tds td_id"
	done

	# -------------------------------------------------------------------
	stage 5 "two waves: 10 create-ss, then 10 create-ns (§10.12 step 5)"
	# -------------------------------------------------------------------
	# Two waves, not one: both mutate the same ten SPs, so the second needs
	# tokens minted by the first. Refetching them between the waves is the
	# documented client protocol (§10.10) and the reason this stage cannot be
	# collapsed.
	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"create-ss","params":{"sp":"%s","rev":"%s","nqn":"%s"}}' \
			"$sp" "$rev" "$(parallel_nqn "$i")")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)

	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"ss wave: OK jobs (codes $(parallel_race_codes "$out"))"
	assert_eq "$(key_count subsystem)" "$PAR_CLIENTS" "subsystem keys"
	# The CdcEntry is written by CreateSubsystem itself, because the entry IS
	# the desired discovery state, not a projection dnv-cdc computes later.
	assert_eq "$(key_count cdc)" "$PAR_CLIENTS" "CdcEntry keys after the ss wave"

	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		nqn=$(parallel_nqn "$i")
		conf=$(sp_json "$sp")
		assert_eq "$(jq_of "$conf" '.subsystems | keys | length')" "1" \
			"$sp: one subsystem"
		# [D2]: serial = %016x(ss_id), model = "dnv" — the host-visible
		# identity is derived from the id that outlives every cntlr. The
		# result array is ordered by input index, so job i IS spA<i>.
		assert_field "$conf" ".subsystems[\"$nqn\"].serial" \
			"$(printf '%016x' "$(jq_of "$out" ".[$i].reply.ss_id")")" \
			"$sp: subsystem serial"
		assert_field "$conf" ".subsystems[\"$nqn\"].model" "dnv" \
			"$sp: subsystem model"
		assert_eq "$(jq_of "$conf" '.sp_conf.nqn_list | length')" "1" \
			"$sp: nqn_list"
		assert_eq "$(sp_rev_of "$sp")" "3" "$sp: sp_rev after create-ss"
	done

	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"create-ns","params":{"sp":"%s","rev":"%s","nqn":"%s","idx":1,"td":"t0"}}' \
			"$sp" "$rev" "$(parallel_nqn "$i")")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)

	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"ns wave: OK jobs (codes $(parallel_race_codes "$out"))"
	# A namespace changes no transport and no host list, so the discovery log
	# is untouched: still exactly one CdcEntry per subsystem.
	assert_eq "$(key_count cdc)" "$PAR_CLIENTS" "CdcEntry keys after the ns wave"

	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		nqn=$(parallel_nqn "$i")
		conf=$(sp_json "$sp")
		assert_eq "$(jq_of "$conf" ".subsystems[\"$nqn\"].ns_list | length")" "1" \
			"$sp: one namespace"
		# ns_idx is uint32 => a JSON number; td_id is uint64 => a string. The
		# namespace stores the td's ID, never its name, so a recreated td can
		# never silently repoint it (§8.8).
		assert_field "$conf" ".subsystems[\"$nqn\"].ns_list[0].ns_idx" "1" \
			"$sp: ns_idx"
		assert_field "$conf" ".subsystems[\"$nqn\"].ns_list[0].td_id" \
			"$(jq_of "$conf" '.tds.t0.td_id')" "$sp: the ns points at t0"
		assert_field "$conf" ".subsystems[\"$nqn\"].ns_list[0].suspended" \
			"false" "$sp: ns suspended default"
		assert_eq "$(sp_rev_of "$sp")" "4" "$sp: sp_rev after create-ns"
		# ③ read-back: the gateway's own view of the subsystem.
		assert_field "$(gw list-sss --sp "$sp")" \
			".nqn_to_subsystem[\"$nqn\"].ns_list[0].ns_idx" "1" \
			"$sp: list-sss ns_idx"
	done

	# -------------------------------------------------------------------
	stage 6 "four reverse waves: delete-ns, -ss, -td, -sp (§10.12 step 6)"
	# -------------------------------------------------------------------
	# Teardown in reverse dependency order, each wave with tokens refetched
	# after the previous one. The order is forced by the RPCs themselves:
	# DeleteSubsystem refuses while a namespace is left, DeleteThinDevice
	# while a namespace backs it, and DeleteStoragePool while ANY of the five
	# name lists is non-empty — which is what makes ten OK delete-sp at the
	# end a proof that all thirty earlier deletes landed, on all ten SPs.
	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"delete-ns","params":{"sp":"%s","rev":"%s","nqn":"%s","idx":1}}' \
			"$sp" "$rev" "$(parallel_nqn "$i")")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)
	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"delete-ns wave: OK jobs (codes $(parallel_race_codes "$out"))"
	# The subsystems (and their CdcEntries) outlive their namespaces.
	assert_eq "$(key_count subsystem)" "$PAR_CLIENTS" \
		"subsystem keys after the delete-ns wave"
	assert_eq "$(key_count cdc)" "$PAR_CLIENTS" \
		"CdcEntry keys after the delete-ns wave"

	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"delete-ss","params":{"sp":"%s","rev":"%s","nqn":"%s"}}' \
			"$sp" "$rev" "$(parallel_nqn "$i")")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)
	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"delete-ss wave: OK jobs (codes $(parallel_race_codes "$out"))"
	assert_eq "$(key_count subsystem)" "0" "subsystem keys after the delete-ss wave"
	assert_eq "$(key_count cdc)" "0" "CdcEntry keys after the delete-ss wave"

	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"delete-td","params":{"sp":"%s","rev":"%s","name":"t0"}}' \
			"$sp" "$rev")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)
	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"delete-td wave: OK jobs (codes $(parallel_race_codes "$out"))"
	# The reply echoes the td_id that was deleted, which step 4 already
	# pinned: every SP mints from its own next_id (§5.4), so ten SPs of one
	# shape produce one value — and it is the one the SPs still hold.
	assert_eq "$(jq_of "$out" '[.[] | .reply.td_id] | unique | length')" "1" \
		"delete-td wave: one td_id across the ten SPs"
	assert_eq "$(jq_of "$out" '[.[] | .reply.td_id] | unique | .[0]')" \
		"$tdId" "delete-td wave: the td_id step 4 minted"
	assert_eq "$(key_count thin_device)" "0" "thin_device keys after the wave"

	wave=""
	for i in $(seq 0 $((PAR_CLIENTS - 1))); do
		sp=$(parallel_sp "$i")
		rev=$(sp_rev_of "$sp")
		wave+=$(printf '{"op":"delete-sp","params":{"sp":"%s","rev":"%s"}}' \
			"$sp" "$rev")$'\n'
	done
	out=$(printf '%s' "$wave" | parallel_race)
	assert_eq "$(parallel_ok_cnt "$out")" "$PAR_CLIENTS" \
		"delete-sp wave: OK jobs (codes $(parallel_race_codes "$out"))"
	assert_eq "$(jq_of "$out" '[.[] | .reply.sp_id | tonumber] | sort | @json')" \
		"$sp_ids" "delete-sp wave: the sp_id set matches step 3's"

	# The final audit: state identical to the end of step 2.
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		conf=$(dn_json "$addr")
		assert_field "$conf" '.free_ext_cnt' "$DN_EXTENTS" \
			"$addr: free_ext_cnt fully restored"
		assert_eq "$(jq_of "$conf" '.side_ptr_list | length')" "0" \
			"$addr: side_ptr_list emptied"
	done
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		conf=$(cn_json "$addr")
		assert_field "$conf" '.free_ext_cnt' "$CN_EXTENTS" \
			"$addr: free_ext_cnt fully restored"
		assert_eq "$(jq_of "$conf" '.cntlr_ptr_list | length')" "0" \
			"$addr: cntlr_ptr_list emptied"
	done
	keys=$(wctl list-keys --prefix dn_capacity)
	for i in 0 1 2 3; do
		addr=$(dn_addr "$i")
		assert_eq "$(parallel_cap_key_cnt "$keys" "$DN_EXTENTS" "$addr")" "1" \
			"$addr: dn_capacity key back at free $DN_EXTENTS"
	done
	keys=$(wctl list-keys --prefix cn_capacity)
	for i in 0 1 2; do
		addr=$(cn_addr "$i")
		assert_eq "$(parallel_cap_key_cnt "$keys" "$CN_EXTENTS" "$addr")" "1" \
			"$addr: cn_capacity key back at free $CN_EXTENTS"
	done
	assert_eq "$(key_count dn_capacity)" "4" "dn_capacity keys at the end"
	assert_eq "$(key_count cn_capacity)" "3" "cn_capacity keys at the end"

	# Nothing SP-shaped may be left: the confs, the ids, the tokens, the
	# per-SP sub-objects.
	assert_eq "$(key_count sp_conf)" "0" "sp_conf keys at the end"
	assert_eq "$(key_count sp_id_to_name)" "0" "sp_id_to_name keys at the end"
	assert_eq "$(key_count sp_rev)" "0" "sp_rev keys at the end"
	assert_eq "$(key_count cntlr)" "0" "cntlr keys at the end"
	assert_eq "$(key_count slice)" "0" "slice keys at the end"

	# GW12: the bucket shrinks back to zero, next_id never rewinds — a
	# deleted sp_id must never be handed out again. The node globals are
	# untouched by any of this.
	out=$(parallel_global sp)
	assert_field "$out" '.next_id' "$((PAR_CLIENTS + 1))" \
		"sp_global next_id does not rewind"
	assert_eq "$(parallel_bucket_sum "$out")" "0" "sp_global Σ shard_bucket"
	out=$(parallel_global dn)
	assert_field "$out" '.next_id' "5" "dn_global next_id at the end"
	assert_eq "$(parallel_bucket_sum "$out")" "4" "dn_global Σ shard_bucket"
	out=$(parallel_global cn)
	assert_field "$out" '.next_id' "4" "cn_global next_id at the end"
	assert_eq "$(parallel_bucket_sum "$out")" "3" "cn_global Σ shard_bucket"

	# The whole store, counted: exactly the keys step 2 left behind, so
	# nothing this case created survived and nothing it deleted took a
	# neighbour with it.
	assert_eq "$(key_count dnv)" "$base_keys" \
		"keys in the store at the end (want step 2's $base_keys)"

	# ③ and the gateway's own listings agree with that store.
	assert_eq "$(jq_of "$(gw list-sps)" '.sp_name | length')" "0" \
		"list-sps at the end"
	assert_eq "$(jq_of "$(gw list-dns)" '.addr_port | length')" "4" \
		"list-dns at the end"
	assert_eq "$(jq_of "$(gw list-cns)" '.addr_port | length')" "3" \
		"list-cns at the end"
}

# ---------------------------------------------------------------------------
# Case B — contention (§10.13): three gateways, barrier races
# ---------------------------------------------------------------------------
#
# The (b) half of §10.1: contended work has exactly one winner and no partial
# writes. Every wave here is ONE `race` invocation whose jobs are round-robined
# over gw0/gw1/gw2 and released together (§10.8), so the contention is between
# gateway INSTANCES and not between goroutines of one process.
#
# Two distinct serializers are under test and the case keeps them apart:
#
#   * the etcd key itself, for a create whose name is the identity —
#     `create-cluster` and `create-dn` answer the losers `ALREADY_EXISTS`
#     (GW7's "create finds the name key present" row) and, crucially, mint no
#     id for them (GW12);
#   * the SpRev token, for every SP-scoped mutator — GW6 checks it before any
#     other state check, so the losers get `ABORTED` "stale revision" and
#     nothing of theirs reaches etcd.
#
# Steps 3-5 use the token as a FENCE (one winner per token) and step 6 uses it
# as a LIVENESS LOOP (the documented client retry protocol, driven by
# gatewayctl's own `retry_stale`), which together are what §10.13 sets out to
# prove.

# contention_race runs one barrier wave, reading its jobs from stdin.
#
# --targets names all three instances, so every job that carries no gateway of
# its own is round-robined across them (§10.8): without it a wave would run
# eight times against gw0 and prove nothing about instances. --retry-budget is
# passed explicitly so step 6's "within RETRY_BUDGET" is the constant §10.6
# declares rather than whatever gatewayctl happens to default to.
contention_race() { # <job lines on stdin> -> the race array on stdout
	gw race --targets "$(gw_addr 0),$(gw_addr 1),$(gw_addr 2)" \
		--retry-budget "$RETRY_BUDGET"
}

# contention_code_cnt counts one gRPC code in a `race` array. The codes are the
# measurement `race` reports (§10.8: it exits 0 whatever they are); the etcd
# outcome is always asserted separately, right below.
contention_code_cnt() { # <race array> <UPPER_SNAKE code>
	jq_of "$1" "[ .[] | select(.code == \"$2\") ] | length"
}

# contention_stale_cnt counts the losers the GW6 TOKEN CHECK refused.
#
# Counting bare ABORTED would not do: GW7 maps the whole §5.9 family — STM
# conflict-budget exhaustion, an etcd error, a failed agent call — onto ABORTED
# as well, so a wave that lost its races for an entirely different reason would
# still satisfy a bare count. The message is what pins it to the token.
contention_stale_cnt() { # <race array>
	jq_of "$1" '[ .[] | select(.code == "ABORTED")
		| select(.message | contains("stale revision")) ] | length'
}

# contention_sp_rev_gt reads an SP's SpRev straight out of etcd — the §10.9
# ground-truth counterpart of the harness's sp_rev_of, which asks the gateway.
# Every "+1 exactly" count below is made against THIS reader: the token the
# gateway hands back is part of what is under test, so it cannot also be the
# evidence that it moved exactly once.
contention_sp_rev_gt() { # <sp name>
	local conf
	conf=$(sp_json "$1")
	jq_of "$(wctl get-rev sp \
		--id "$(jq_of "$conf" '.sp_conf.sp_id')" \
		--shard "$(printf '%02x' "$(jq_of "$conf" '.sp_conf.shard_code')")")" \
		'.revision'
}

case_contention() {
	CASE=contention

	local wave line out conf got glob kind i
	local cidhex addr nqn nsPath winner staleRev winnerIdx
	local nextIdBefore devIdBefore devIdNow tdsBefore rev

	# -----------------------------------------------------------------------
	stage 1 "eight create-cluster itgw at once: one winner, seven losers"
	# -----------------------------------------------------------------------
	# The plain create-cluster subcommand acts on the global --cluster, which
	# gw() already passes to every job of the wave, so a racing job needs no
	# params at all: all eight requests are byte-identical and contend on
	# nothing but the cluster_conf key.
	wave=""
	line='{"op":"create-cluster","params":{}}'
	for ((i = 0; i < RACE_N; i++)); do
		wave+="$line"$'\n'
	done
	out=$(printf '%s' "$wave" | contention_race)

	assert_eq "$(jq_of "$out" 'length')" "$RACE_N" "step 1: jobs that executed"
	assert_eq "$(contention_code_cnt "$out" OK)" "1" \
		"step 1: create-cluster winners"
	assert_eq "$(contention_code_cnt "$out" ALREADY_EXISTS)" \
		"$((RACE_N - 1))" "step 1: create-cluster losers"

	# The winner's cid is the one the rest of the case (and the three global
	# keys) must be addressed by, so it comes out of the OK job's reply, not
	# out of a read that could have picked up a loser's leftovers.
	CID=$(jq_of "$out" '[ .[] | select(.code == "OK") ][0].reply.cluster_id')
	assert_ne "$CID" "0" "step 1: the winner's cluster_id"
	cidhex=$(printf '%016x' "$CID")
	log "  cluster $CLUSTER, cluster_id $CID"

	# Ground truth (§10.9): one cluster, stamped with a real creation_epoch —
	# the epoch is half of ClusterId's input (§5.2), so a zero there would mean
	# the cid the reply carried was never derivable from what etcd holds.
	assert_eq "$(key_count cluster_conf)" "1" "step 1: cluster_conf keys"
	conf=$(raw_key "$(cluster_key)")
	assert_ne "$(jq_of "$conf" '.creation_epoch')" "0" \
		"step 1: ClusterConf.creation_epoch"

	# The three globals are addressed BY THE WINNER'S CID, and `wctl get` exits
	# non-zero on a missing key, so the read succeeding is itself the "the
	# globals belong to the winner" assertion. Each is virgin: next_id 1 and
	# 256 empty shard buckets, i.e. not one loser got as far as minting.
	for kind in dn_global cn_global sp_global; do
		conf=$(raw_key "$DNV_PREFIX $kind $cidhex")
		assert_field "$conf" '.next_id' "1" "step 1: $kind next_id"
		assert_eq "$(jq_of "$conf" '.shard_bucket | length')" "256" \
			"step 1: $kind shard_bucket size"
		assert_eq "$(jq_of "$conf" '[ .shard_bucket[] ] | add')" "0" \
			"step 1: $kind shard_bucket sum"
	done

	# Gateway read-back: GetCluster resolves the cid from the stored epoch, so
	# it agreeing with the reply is the end-to-end form of the same check.
	got=$(gw get-cluster)
	assert_field "$got" '.cluster_id' "$CID" "step 1: get-cluster cluster_id"
	assert_field "$got" '.cluster_name' "$CLUSTER" \
		"step 1: get-cluster cluster_name"
	assert_field "$got" '.dn_global.next_id' "1" \
		"step 1: get-cluster dn_global.next_id"

	# -----------------------------------------------------------------------
	stage 2 "eight create-dn on one addr: GW12 mints exactly one id"
	# -----------------------------------------------------------------------
	addr=$(dn_addr 0)
	wave=""
	for ((i = 0; i < RACE_N; i++)); do
		printf -v line \
			'{"op":"create-dn","params":{"addr":"%s","location":"%s","tr-svc-id":"%s"}}' \
			"$addr" "$(dn_loc 0)" "$(dn_svcid 0)"
		wave+="$line"$'\n'
	done
	out=$(printf '%s' "$wave" | contention_race)

	assert_eq "$(contention_code_cnt "$out" OK)" "1" "step 2: create-dn winners"
	assert_eq "$(contention_code_cnt "$out" ALREADY_EXISTS)" \
		"$((RACE_N - 1))" "step 2: create-dn losers"
	# next_id starts at 1, so the single winner is dn_id 1 — and no loser can
	# have taken 1 for itself and then failed.
	assert_eq "$(jq_of "$out" '[ .[] | select(.code == "OK") ][0].reply.dn_id')" \
		"1" "step 2: the winner's dn_id"

	conf=$(dn_json "$addr")
	assert_field "$conf" '.dn_id' "1" "step 2: stored dn_id"
	assert_field "$conf" '.total_ext_cnt' "$DN_EXTENTS" "step 2: total_ext_cnt"
	assert_field "$conf" '.free_ext_cnt' "$DN_EXTENTS" "step 2: free_ext_cnt"
	assert_field "$conf" '.disabled' "false" "step 2: disabled"
	# One DN's worth of keys, not eight: the losers wrote no conf, no capacity
	# index entry and no revision record.
	assert_eq "$(key_count dn_conf)" "1" "step 2: dn_conf keys"
	assert_eq "$(key_count dn_capacity)" "1" "step 2: dn_capacity keys"
	assert_eq "$(key_count dn_rev)" "1" "step 2: dn_rev keys"

	# THE GW12 ASSERTION. Minting is "id = next_id; next_id += 1;
	# bucket[shard]++" inside the STM, so a loser that had got as far as the
	# mint before losing would leave next_id at 9 or the bucket sum above 1
	# even though only one DnConf exists. next_id 2 with Σbucket 1 is the proof
	# that the seven losers burned no id at all.
	glob=$(raw_key "$DNV_PREFIX dn_global $cidhex")
	assert_field "$glob" '.next_id' "2" "step 2 (GW12): DnGlobal.next_id"
	assert_eq "$(jq_of "$glob" '[ .shard_bucket[] ] | add')" "1" \
		"step 2 (GW12): DnGlobal shard_bucket sum"

	got=$(gw get-dn --addr "$addr")
	assert_field "$got" '.dn_conf.dn_id' "1" "step 2: get-dn dn_id"
	assert_field "$got" '.dn_rev.revision' "1" "step 2: get-dn dn_rev"
	assert_field "$(gw get-cluster)" '.dn_global.next_id' "2" \
		"step 2: get-cluster dn_global.next_id"

	# -----------------------------------------------------------------------
	stage 3 "eight create-td on ONE token: exactly one winner (GW6)"
	# -----------------------------------------------------------------------
	# The §10.6 SP shape needs four DNs (one per leg, all distinct) and three
	# CNs, and step 2 raced only dn0 into existence, so top the fleet up first.
	# make_nodes cannot be reused: it would re-create dn0 and be refused.
	for i in 1 2 3; do
		gw create-dn --addr "$(dn_addr "$i")" --location "$(dn_loc "$i")" \
			--tr-svc-id "$(dn_svcid "$i")" >/dev/null
	done
	for i in 0 1 2; do
		gw create-cn --addr "$(cn_addr "$i")" --location "$(cn_loc "$i")" \
			--tr-svc-id "$(cn_svcid "$i")" >/dev/null
	done
	make_sp sp0

	conf=$(sp_json sp0)
	nextIdBefore=$(jq_of "$conf" '.sp_conf.next_id')
	devIdBefore=$(jq_of "$conf" '.sp_conf.next_dev_id')
	tdsBefore=$(jq_of "$conf" '.tds | length')
	rev=$(contention_sp_rev_gt sp0)
	assert_eq "$tdsBefore" "0" "step 3: tds on a fresh SP"
	assert_eq "$devIdBefore" "1" "step 3: next_dev_id on a fresh SP"
	# make_sp refreshed the cached token from the gateway; etcd must hold the
	# same number, or every delta counted below would be counted against the
	# wrong baseline.
	assert_eq "$SP_REV" "$rev" "step 3: cached token vs the stored SpRev"
	# Step 4 needs a token that is provably one behind, and this is it: the
	# race consumes it, so from step 4 on it is stale by construction rather
	# than by a guess about arithmetic.
	staleRev=$SP_REV

	# Eight DISTINCT names, so nothing here can be refused for existing; the
	# only thing the eight jobs share is the token, which is therefore the only
	# thing that can pick the winner.
	wave=""
	for ((i = 0; i < RACE_N; i++)); do
		printf -v line \
			'{"op":"create-td","params":{"sp":"sp0","rev":"%s","name":"t%d","size":"%s"}}' \
			"$SP_REV" "$i" "$TD_SIZE"
		wave+="$line"$'\n'
	done
	out=$(printf '%s' "$wave" | contention_race)

	assert_eq "$(contention_code_cnt "$out" OK)" "1" "step 3: create-td winners"
	assert_eq "$(contention_code_cnt "$out" ABORTED)" "$((RACE_N - 1))" \
		"step 3: create-td losers"
	assert_eq "$(contention_stale_cnt "$out")" "$((RACE_N - 1))" \
		"step 3 (GW6): losers refused by the token check specifically"

	# Job idx i asked for name t<i>, so the OK job's index names the td that
	# must exist — the reply and etcd have to agree on WHICH job won, not just
	# on how many did.
	winner=t$(jq_of "$out" '[ .[] | select(.code == "OK") ][0].idx')
	assert_eq "$(jq_of "$out" '[ .[] | select(.code == "OK") ][0].reply.dev_id')" \
		"$devIdBefore" "step 3: the winner's dev_id (uint32, a JSON number)"

	conf=$(sp_json sp0)
	assert_eq "$(jq_of "$conf" '.tds | length')" "1" "step 3: stored tds"
	assert_eq "$(jq_of "$conf" '.tds | keys | @json')" "[\"$winner\"]" \
		"step 3: the stored td is the job that replied OK"
	assert_eq "$(jq_of "$conf" '.sp_conf.td_name_list | @json')" \
		"[\"$winner\"]" "step 3: td_name_list"
	# §5.6 mints td_id from SpNextId and dev_id from next_dev_id++ — one of
	# each, for the one td. A loser that reached the mint before its STM lost
	# would show up here as a gap.
	assert_eq "$(jq_of "$conf" '.sp_conf.next_id')" "$((nextIdBefore + 1))" \
		"step 3: SpConf.next_id advanced by exactly 1"
	assert_eq "$(jq_of "$conf" '.sp_conf.next_dev_id')" \
		"$((devIdBefore + 1))" "step 3: SpConf.next_dev_id advanced by exactly 1"
	# GW6: every mutation that changes agent-visible state bumps SpRev exactly
	# once in the same STM, so seven refusals plus one write is +1.
	assert_eq "$(contention_sp_rev_gt sp0)" "$((rev + 1))" \
		"step 3: SpRev advanced by exactly 1"

	got=$(gw list-tds --sp sp0)
	assert_eq "$(jq_of "$got" '.name_to_td | length')" "1" \
		"step 3: list-tds entries"
	assert_field "$got" ".name_to_td[\"$winner\"].created" "false" \
		"step 3: the new td is not created yet (no worker runs here)"
	refresh_rev sp0
	assert_eq "$SP_REV" "$((rev + 1))" "step 3: the refreshed token"

	# -----------------------------------------------------------------------
	stage 4 "the sequential stale probes: refusals write nothing"
	# -----------------------------------------------------------------------
	# (a) The pre-step-3 token. GW6 checks the token BEFORE any other state
	# check, so this is ABORTED even though READONLY is a perfectly legal
	# level — and the bracket proves the refusal cost the store nothing.
	assert_no_write "step 4: set-sp-level with the pre-step-3 token" \
		gwx ABORTED set-sp-level --sp sp0 --rev "$staleRev" --level READONLY

	# (b) The same call with the fresh token. This is what makes (a) evidence
	# about the TOKEN rather than about set-sp-level: same gateway, same SP,
	# same level, one number different.
	rev=$(contention_sp_rev_gt sp0)
	gw set-sp-level --sp sp0 --rev "$SP_REV" --level READONLY
	assert_field "$(sp_json sp0)" '.sp_conf.sp_level' "SP_LEVEL_READONLY" \
		"step 4: stored sp_level"
	assert_eq "$(contention_sp_rev_gt sp0)" "$((rev + 1))" \
		"step 4: SpRev after set-sp-level"
	assert_field "$(gw get-sp --sp sp0)" '.sp_conf.sp_level' \
		"SP_LEVEL_READONLY" "step 4: get-sp read-back of sp_level"
	refresh_rev sp0
	# Put the level back before steps 5-6 create namespaces and thin devices on
	# this SP. Nothing in §5 gates a gateway mutator on sp_level (READONLY is
	# enforced on the CN, [D11]), so this is not needed for the assertions to
	# pass — it keeps the SP the case goes on to exercise in the state a reader
	# of the trace would expect it to be in.
	rev=$(contention_sp_rev_gt sp0)
	gw set-sp-level --sp sp0 --rev "$SP_REV" --level READWRITE
	assert_field "$(sp_json sp0)" '.sp_conf.sp_level' "SP_LEVEL_READWRITE" \
		"step 4: sp_level restored"
	assert_eq "$(contention_sp_rev_gt sp0)" "$((rev + 1))" \
		"step 4: SpRev after the restore"
	refresh_rev sp0

	# (c) A duplicate name with a VALID token: §5.6 refuses on the name before
	# minting anything, so next_dev_id must not move. The store-revision
	# bracket proves nothing at all was written; next_dev_id is the readable
	# statement of the same fact, and the one §10.13 asks for by name.
	devIdNow=$(jq_of "$(sp_json sp0)" '.sp_conf.next_dev_id')
	assert_no_write "step 4: create-td with a duplicate name" \
		gwx ALREADY_EXISTS create-td --sp sp0 --rev "$SP_REV" \
		--name "$winner" --size "$TD_SIZE"
	assert_field "$(sp_json sp0)" '.sp_conf.next_dev_id' "$devIdNow" \
		"step 4: next_dev_id after the duplicate refusal"

	# (d) The zero token. gatewayctl sends --rev 0 as a real zero rather than
	# as "no token" on purpose (§10.8), and that is what makes this probe a
	# refusal: GW6 is presence-based (§0 #7), so a PRESENT zero is compared
	# and can never match a stored revision that starts at 1 — ABORTED, not
	# INVALID_ARGUMENT — while an ABSENT message would be waved through
	# unchecked. This driver cannot send an absent one.
	assert_no_write "step 4: create-td with the zero token" \
		gwx ABORTED create-td --sp sp0 --rev 0 --name tnil --size "$TD_SIZE"
	assert_eq "$(jq_of "$(sp_json sp0)" '.tds | keys | @json')" \
		"[\"$winner\"]" "step 4: tds after both refusals"
	# The two refusals bumped nothing, so the token cached before them is still
	# the live one — steps 5 and 6 spend it without re-reading.
	assert_eq "$(contention_sp_rev_gt sp0)" "$SP_REV" \
		"step 4: the cached token survives the refusals"

	# -----------------------------------------------------------------------
	stage 5 "two different mutators, one token: etcd matches the winner"
	# -----------------------------------------------------------------------
	nqn="$NQN_PREFIX:$CLUSTER:sp0:ss0"
	gw create-ss --sp sp0 --rev "$SP_REV" --nqn "$nqn"
	refresh_rev sp0
	gw create-ns --sp sp0 --rev "$SP_REV" --nqn "$nqn" --idx 1 --td "$winner"
	refresh_rev sp0

	nsPath=".subsystems[\"$nqn\"].ns_list"
	conf=$(sp_json sp0)
	assert_eq "$(jq_of "$conf" "$nsPath | length")" "1" \
		"step 5: the namespace exists before the race"
	assert_eq "$(jq_of "$conf" "${nsPath}[0].suspended")" "false" \
		"step 5: create-ns default suspended"
	rev=$(contention_sp_rev_gt sp0)

	# Two jobs, one token, and — unlike step 3 — two DIFFERENT mutators whose
	# outcomes are mutually exclusive: a suspended namespace and a deleted one
	# cannot both be true. Job 0 suspends, job 1 deletes; the index of the OK
	# job therefore says exactly what etcd is now required to hold.
	wave=""
	printf -v line \
		'{"op":"set-ns-suspended","params":{"sp":"sp0","rev":"%s","nqn":"%s","idx":"1","suspended":true}}' \
		"$SP_REV" "$nqn"
	wave+="$line"$'\n'
	printf -v line \
		'{"op":"delete-ns","params":{"sp":"sp0","rev":"%s","nqn":"%s","idx":"1"}}' \
		"$SP_REV" "$nqn"
	wave+="$line"$'\n'
	out=$(printf '%s' "$wave" | contention_race)

	assert_eq "$(jq_of "$out" 'length')" "2" "step 5: jobs that executed"
	assert_eq "$(contention_code_cnt "$out" OK)" "1" "step 5: winners"
	assert_eq "$(contention_code_cnt "$out" ABORTED)" "1" "step 5: losers"
	assert_eq "$(contention_stale_cnt "$out")" "1" \
		"step 5 (GW6): the loser was refused by the token check"

	winnerIdx=$(jq_of "$out" '[ .[] | select(.code == "OK") ][0].idx')
	conf=$(sp_json sp0)
	got=$(gw list-sss --sp sp0)
	case "$winnerIdx" in
	0)
		log "  set-ns-suspended won; the namespace must still be there"
		assert_eq "$(jq_of "$conf" "$nsPath | length")" "1" \
			"step 5: ns_list after set-ns-suspended won"
		assert_eq "$(jq_of "$conf" "${nsPath}[0].ns_idx")" "1" \
			"step 5: the surviving ns_idx (uint32, a JSON number)"
		assert_eq "$(jq_of "$conf" "${nsPath}[0].suspended")" "true" \
			"step 5: the surviving namespace is suspended"
		assert_eq \
			"$(jq_of "$got" ".nqn_to_subsystem[\"$nqn\"].ns_list[0].suspended")" \
			"true" "step 5: list-sss read-back of suspended"
		;;
	1)
		log "  delete-ns won; the namespace must be gone"
		assert_eq "$(jq_of "$conf" "$nsPath | length")" "0" \
			"step 5: ns_list after delete-ns won"
		assert_eq \
			"$(jq_of "$got" ".nqn_to_subsystem[\"$nqn\"].ns_list | length")" \
			"0" "step 5: list-sss read-back of the empty ns_list"
		;;
	*)
		die "step 5: no job replied OK (winner idx '$winnerIdx')"
		;;
	esac
	# Whichever won, the subsystem itself is untouched and exactly one write
	# happened — the loser did not half-apply on its way to ABORTED.
	assert_eq "$(jq_of "$conf" ".subsystems | keys | @json")" "[\"$nqn\"]" \
		"step 5: the subsystem survives either outcome"
	assert_eq "$(contention_sp_rev_gt sp0)" "$((rev + 1))" \
		"step 5: SpRev advanced by exactly 1"
	refresh_rev sp0

	# -----------------------------------------------------------------------
	stage 6 "eight retry_stale create-td from one token: the loop converges"
	# -----------------------------------------------------------------------
	# The same shape as step 3 — eight distinct names, one shared token — with
	# the documented client protocol switched on: a job refused ABORTED "stale
	# revision" re-reads the SP's token through GetStoragePool and retries
	# (§10.8). Every job therefore carries "sp":"sp0" in its params, which is
	# the only place gatewayctl's refreshToken can learn WHICH SP to re-read;
	# without it the retry fails immediately and the wave looks like step 3.
	conf=$(sp_json sp0)
	nextIdBefore=$(jq_of "$conf" '.sp_conf.next_id')
	devIdBefore=$(jq_of "$conf" '.sp_conf.next_dev_id')
	tdsBefore=$(jq_of "$conf" '.tds | length')
	rev=$(contention_sp_rev_gt sp0)

	wave=""
	for ((i = 0; i < RACE_N; i++)); do
		printf -v line \
			'{"op":"create-td","params":{"sp":"sp0","rev":"%s","name":"t%d","size":"%s"},"retry_stale":true}' \
			"$SP_REV" "$((RACE_N + i))" "$TD_SIZE"
		wave+="$line"$'\n'
	done
	out=$(printf '%s' "$wave" | contention_race)

	assert_eq "$(jq_of "$out" 'length')" "$RACE_N" "step 6: jobs that executed"
	assert_eq "$(contention_code_cnt "$out" OK)" "$RACE_N" \
		"step 6: every job ends OK"
	assert_eq "$(jq_of "$out" '[ .[].reply.td_id ] | unique | length')" \
		"$RACE_N" "step 6: distinct td_ids in the replies"
	# A job only ever retries because some OTHER job's create-td committed and
	# moved the token, and each of the other seven commits exactly once, so no
	# job can lose more than RACE_N-1 times: attempts are bounded by RACE_N,
	# well inside the RETRY_BUDGET contention_race hands the runner. A loop
	# that did NOT converge — a job re-fetching a token that is stale again by
	# the time it lands — shows up here as a max at the budget, which is a far
	# more legible failure than the timeout it would otherwise become.
	assert_between "$(jq_of "$out" '[ .[].tries ] | max')" 1 "$RACE_N" \
		"step 6: attempts of the slowest job"

	conf=$(sp_json sp0)
	for ((i = 0; i < RACE_N; i++)); do
		assert_eq "$(jq_of "$conf" "(.tds | has(\"t$((RACE_N + i))\"))")" \
			"true" "step 6: t$((RACE_N + i)) stored"
	done
	# §10.13 counts the eight this step created; step 3's single winner is
	# still there too, so every total below is a DELTA against tdsBefore.
	assert_eq "$(jq_of "$conf" '.tds | length')" "$((tdsBefore + RACE_N))" \
		"step 6: stored tds"
	assert_eq "$(jq_of "$conf" '.sp_conf.td_name_list | length')" \
		"$((tdsBefore + RACE_N))" "step 6: td_name_list length"
	assert_eq "$(jq_of "$conf" '.sp_conf.next_id')" \
		"$((nextIdBefore + RACE_N))" "step 6: SpConf.next_id advanced by 8"
	assert_eq "$(jq_of "$conf" '.sp_conf.next_dev_id')" \
		"$((devIdBefore + RACE_N))" "step 6: SpConf.next_dev_id advanced by 8"
	# Retries make a job re-run CreateThinDevice from scratch, so this is where
	# a dev_id minted by an attempt that then lost would become visible.
	assert_eq "$(jq_of "$conf" '[ .tds[].dev_id ] | unique | length')" \
		"$((tdsBefore + RACE_N))" "step 6: distinct dev_ids"
	# Eight successful mutations, eight bumps: the retries themselves cost the
	# SP nothing (GW6 refuses before any write).
	assert_eq "$(contention_sp_rev_gt sp0)" "$((rev + RACE_N))" \
		"step 6: SpRev advanced by exactly 8"

	got=$(gw list-tds --sp sp0)
	assert_eq "$(jq_of "$got" '.name_to_td | length')" \
		"$((tdsBefore + RACE_N))" "step 6: list-tds entries"
	refresh_rev sp0
	assert_eq "$SP_REV" "$((rev + RACE_N))" "step 6: the refreshed token"
}

# ---------------------------------------------------------------------------
# Case C — faults (§10.14): gw0 only, refusals and agent failures
# ---------------------------------------------------------------------------
#
# This case is the settled (c) of §10.1: "refusals of every class write
# nothing", made provable rather than asserted. Every battery below runs
# inside an assert_no_write bracket, because etcd advances its store revision
# only for a transaction that actually mutates the store (§10.10) — an RPC
# that returns before its first Put therefore leaves the revision exactly
# where it was. The batteries additionally compare sp0's whole decoded state
# before and after, so a refusal that somehow rewrote a field in place —
# leaving the key count alone — would still be caught.
#
# Steps 4 and 5 are the agent half. AG2 bounds every gateway->agent call at
# common.DefaultGatewayAgentTimeout (10 s), which is what makes a hung agent
# bound an RPC exactly as a hung etcd does; and both force = false proofs
# (DeleteClone §8.9, FinishMigration §8.11) treat an unreachable agent as
# "hydration unproven" rather than as permission to proceed — silence is not
# proof.
#
# Everything runs against gw0 (the default $GATEWAY): nothing here is about
# concurrency, so a second instance would only add noise.

# faults_sp_snapshot prints sp0's decoded ground truth with `store_rev`
# removed. store_rev is the etcd header revision the read was served at, not
# a property of the SP, so leaving it in would make the "content is
# identical" halves of the refusal brackets assert the same thing the
# store_rev bracket already asserts — and nothing else.
faults_sp_snapshot() { # <sp name>
	jq_of "$(sp_json "$1")" 'del(.store_rev) | @json'
}

# faults_nonprimary_cntlr prints the cntlr_id of one cntlr of the SP that is
# NOT the primary — the mirror of the harness's sp_primary_cntlr, over the
# same index-aligned cntlr_id_list / cntlr_list pair (§10.8).
#
# The pick is made INSIDE jq rather than with `head -n 1`: `set -o pipefail`
# plus a reader that closes the pipe early turns a SIGPIPE on jq into a
# script abort, which is the same trap the harness's own grep -c comment
# names.
faults_nonprimary_cntlr() { # <sp name>
	jq_of "$(gw get-sp --sp "$1")" '
		[ [ .sp_conf.cntlr_id_list, .cntlr_list ]
		  | transpose[]
		  | select(.[1].primary | not)
		  | .[0] ][0]'
}

# faults_side_addr prints the addr_port of one side of the SP by side_id,
# searching active and spare legs of both group kinds. CreateMigration
# appends its destination to the SOURCE side's leg (§8.11), so this is the
# only way to learn which DN the gateway picked for it.
faults_side_addr() { # <sp name> <side_id, decimal>
	jq_of "$(sp_json "$1")" "
		[ .slices[]
		  | (.meta_grp_list[]?, .data_grp_list[]?)
		  | (.leg_list[]?, .spare_leg_list[]?)
		  | .side_list[]?
		  | select((.side_id | tostring) == \"$2\")
		  | .addr_port ][0]"
}

# faults_dn_dir / faults_cn_dir map an addr_port back to the fake's directory,
# which is what set_behavior / set_state / sig_dir address. §10.5 assigns the
# ports contiguously from the two bases, so the index is pure arithmetic.
faults_dn_dir() { # <addr_port>
	dn_dir $((${1##*:} - DN_PORT_BASE))
}

faults_cn_dir() { # <addr_port>
	cn_dir $((${1##*:} - CN_PORT_BASE))
}

# faults_gwx_out is gwx with the reply captured into a global, for the
# refusals whose MESSAGE is part of the assertion. assert_no_write runs its
# command in the CURRENT shell, so a global set here survives the bracket —
# a `$( )` around the bracket would lose it, the same subshell hazard §10.10
# calls out for the SP token.
faults_gwx_out() { # <UPPER_SNAKE code> <args…>
	FAULTS_OUT=$(gwx "$@")
}

# faults_seed_cn_state writes the CN fake's state.json so GetCntlrInfo has a
# cntlr to report on at all. Every *Info the fake returns is derived from the
# last Syncup* request it applied (§14.9) and this suite runs no dnv-worker,
# so an unseeded fake answers with a nil CntlrInfo and DeleteClone would
# refuse for the wrong reason. The object keys use DECIMAL ids.
faults_seed_cn_state() { # <cn dir> <cn_id> <sp_id> <cntlr_id> <clone_id>
	set_state "$1" <<EOF
{"objects":{
  "cn":{"revision":1,"request":{"cluster_id":"$CID","cn_id":"$2",
        "revision":"1","cntlr_pointer_list":[{"sp_id":"$3","cntlr_id":"$4"}]}},
  "cntlr $3:$4":{"revision":1,"request":{
        "cluster_id":"$CID","cn_id":"$2",
        "cntlr_pointer":{"sp_id":"$3","cntlr_id":"$4"},
        "revision":"1","cntlr":{"primary":true},
        "clone_list":[{"clone_id":"$5"}]}}}}
EOF
}

# faults_seed_dn_state is the same seeding for the migration destination: the
# side's last request must carry a migr_dst_conf, because the fake only
# builds SideInfo.migr_dst_info when it does (§14.9), and that is the field
# FinishMigration reads the dm-clone status out of.
faults_seed_dn_state() { # <dn dir> <dn_id> <sp_id> <leg> <side> <ext> <cn_id> <migr_id>
	set_state "$1" <<EOF
{"objects":{
  "dn":{"revision":1,"request":{"cluster_id":"$CID","dn_id":"$2",
        "revision":"1","side_pointer_list":[{"sp_id":"$3",
        "leg_id":"$4","side_id":"$5"}]}},
  "side $3:$4:$5":{"revision":1,"request":{
        "cluster_id":"$CID","dn_id":"$2",
        "side_pointer":{"sp_id":"$3","leg_id":"$4","side_id":"$5"},
        "revision":"1","side_conf":{"ext_cnt":"$6","primary_cn_id":"$7"},
        "migr_dst_conf":{"migr_id":"$8"}}}}}
EOF
}

# faults_clone_rows overrides the primary cntlr's clone_id_to_dm_clone row
# with a raw `dmsetup status` line. hydrationComplete reads the SEVENTH field
# ("<hydrated>/<total>") and requires hydrated >= total with total != 0, so
# "256/512" is an unfinished copy and "512/512" a finished one.
faults_clone_rows() { # <cn dir> <sp_id> <cntlr_id> <clone_id> <hydrated/total>
	set_behavior "$1" <<EOF
{"objects":{"cntlr $2:$3":{"rows":{
  "clone_id_to_dm_clone.$4":{"status":"OK","details":
  "0 131072 clone 8 100/1024 2048 $5 0 1 no_hydration 2 hydration_threshold 1 hydration_batch_size 1"}}}}}
EOF
}

# faults_migr_rows is faults_clone_rows for FinishMigration, whose proof comes
# from the DESTINATION side's migr_dst_info.dm_clone_info instead.
faults_migr_rows() { # <dn dir> <sp_id> <leg> <side> <hydrated/total>
	set_behavior "$1" <<EOF
{"objects":{"side $2:$3:$4":{"rows":{
  "migr_dst_info.dm_clone_info":{"status":"OK","details":
  "0 4096 clone 8 10/1024 2048 $5 0 1 no_hydration 2 hydration_threshold 1 hydration_batch_size 1"}}}}}
EOF
}

# ---------------------------------------------------------------------------
# The batteries. Each is one function so the whole batch can sit inside a
# single assert_no_write bracket where §10.14 asks for one.
# ---------------------------------------------------------------------------

# faults_validation_battery is §10.14 step 1: every §7 rule that is checkable
# without stored state, plus the two that are not (the td size unit and the
# cntlid slots) which the handlers evaluate inside their STM and which
# therefore still return before the first Put.
faults_validation_battery() {
	local long65 slice
	# MaxStrSize is 64 bytes, so 65 'a's is the shortest over-long name.
	long65=$(printf 'a%.0s' {1..65})
	slice=$(sp_first_slice sp0)

	# ValidStrPattern is ^[a-zA-Z0-9\-_/.:]+$ — '/' and '.' are IN the set, so
	# "sp/../x" is a perfectly legal dnv name and proves nothing. '!' is not.
	gwx INVALID_ARGUMENT create-sp --sp 'sp!0'
	gwx INVALID_ARGUMENT create-sp --sp "$long65"

	# ValidNqnPattern demands a ':' after the domain part, which is exactly
	# why the well-known discovery NQN can never validate — §7 needs no
	# separate rejection for it, and this asserts that claim rather than
	# assuming it.
	gwx INVALID_ARGUMENT create-ss --sp sp0 --rev "$SP_REV" --nqn bad-nqn
	gwx INVALID_ARGUMENT create-ss --sp sp0 --rev "$SP_REV" \
		--nqn nqn.2014-08.org.nvmexpress.discovery

	# §8.7: a size must be a positive multiple of slice_cnt x stripe_size —
	# here 1 x 64 KiB — because dm-striped takes equal, chunk-aligned members.
	# The check is state-dependent, so it runs inside the transaction; the
	# refusal still lands before the id minter is ever created.
	gwx INVALID_ARGUMENT create-td --sp sp0 --rev "$SP_REV" \
		--name tbad --size 1000

	# §8.5's exclusivity: a meta grow takes its size from the ladder, so
	# ext_cnt must be 0 when is_meta is true.
	gwx INVALID_ARGUMENT grow-slice --sp sp0 --rev "$SP_REV" \
		--slice "$slice" --meta --ext 2

	# §11.8: cntlid slots are distinct and below CnCntlidSlotCnt (8).
	gwx INVALID_ARGUMENT set-cntlid-slots --sp sp0 --rev "$SP_REV" \
		--slots 0,0,1
	gwx INVALID_ARGUMENT set-cntlid-slots --sp sp0 --rev "$SP_REV" --slots 8

	# GW10 pagination: count is clamped at MaxListCnt (1024) and a page token
	# that is not base64 is malformed input, not an empty page.
	gwx INVALID_ARGUMENT list-clusters --count 2000
	gwx INVALID_ARGUMENT list-clusters --page-token '!!'

	# §7's structural bdev rule: bdev_feature_list MUST be empty in v1.
	# --feature-junk appends one empty BdevFeature; the rest of the request is
	# the §10.6 shape, because validateBdevConf runs AFTER the cntlr/slice
	# /init_ext_cnt bounds and a short request would be refused before it.
	gwx INVALID_ARGUMENT create-sp --sp spjunk --cntlr-cnt "$SP_CNTLR_CNT" \
		--slice-cnt "$SP_SLICE_CNT" --init-ext-cnt "$SP_INIT_EXT" \
		--feature-junk

	# The other half of the clamp: 0 is proto3's "unset" and selects
	# DefaultListCnt (64), so it is ACCEPTED. With itgw the only cluster of
	# this case the page cannot be full, which is why the token comes back
	# empty (§5.7: a non-full page ends the listing).
	FAULTS_OUT=$(gw list-clusters --count 0)
	assert_field "$FAULTS_OUT" '.cluster_name | @json' '["itgw"]' \
		"count 0 is accepted and lists the one cluster"
	assert_field "$FAULTS_OUT" '.page_token' "" \
		"count 0: a non-full page ends the listing"
}

# faults_notfound_battery is §10.14 step 2: one probe per resource group.
# Every one of them resolves further than the previous group did — the
# cluster probe never reaches an SP, the sp probe never reaches a td — which
# is what makes "every group" a real statement about GW5's resolution order.
faults_notfound_battery() { # <ss nqn> <data grp id> <data leg id>
	local ssNqn=$1 grp=$2 leg=$3

	# GW5: ClusterConf is the first read of every STM, so a missing cluster is
	# refused before the SP is even looked for. The `gw` wrapper puts
	# --cluster BEFORE the subcommand and gatewayctl binds its globals on the
	# subcommand's flag set as well, so this later --cluster wins.
	gwx NOT_FOUND get-sp --cluster nosuch --sp sp0

	gwx NOT_FOUND get-sp --sp nosuchsp
	gwx NOT_FOUND get-dn --addr 127.0.0.1:29999
	gwx NOT_FOUND get-cn --addr 127.0.0.1:29999

	# The SP-scoped groups. Each carries the CURRENT token, so the NOT_FOUND
	# is the object's own and never GW6's ABORTED "stale revision" — the
	# token check runs first, so a wrong token would mask every one of these.
	gwx NOT_FOUND delete-td --sp sp0 --rev "$SP_REV" --name nosuchtd
	gwx NOT_FOUND delete-ss --sp sp0 --rev "$SP_REV" \
		--nqn "$NQN_PREFIX:nosuchss"
	gwx NOT_FOUND delete-ns --sp sp0 --rev "$SP_REV" --nqn "$ssNqn" --idx 99
	gwx NOT_FOUND get-clone --sp sp0 --name nosuchclone
	gwx NOT_FOUND get-xfer --sp sp0 --name nosuchxfer
	gwx NOT_FOUND get-migr --sp sp0 --name nosuchmigr

	# A side id that is in no leg of the SP (§5.5's id-addressed read).
	gwx NOT_FOUND inspect-side --sp sp0 --id 0xdead

	# The spare ids, both of them. §8.12 makes SwitchSpareLeg check leg
	# membership in its own pre-read precisely so an unknown id is NOT_FOUND
	# here rather than the FAILED_PRECONDITION model.SwitchSpareLeg would
	# raise for it.
	gwx NOT_FOUND delete-spare --sp sp0 --rev "$SP_REV" \
		--grp "$grp" --leg 0xdead
	gwx NOT_FOUND switch-spare --sp sp0 --rev "$SP_REV" \
		--grp "$grp" --spare 0xdead --target "$leg"
}

# faults_agent_battery is §10.14 step 4. It is one batch because none of it
# can write: create-dn calls GetDnSize BEFORE its STM (§8.2), so an agent
# that never answers means the transaction is never opened at all.
#
# FAULTS_HUNG_SECS / FAULTS_FAST_SECS carry the two wall-clock measurements
# out to the stage, which asserts the AG2 bound on them.
faults_agent_battery() {
	local start out dn0Addr cn2Addr
	dn0Addr=$(dn_addr 0)
	cn2Addr=$(cn_addr 2)

	# A hung LISTENING agent — the interesting case, because the connection
	# succeeds and only the call stalls. The addr must differ from dn0's own
	# or the STM would refuse it as ALREADY_EXISTS; "localhost:29820" reaches
	# the same listener under a name the DnConf key does not hold yet.
	# gatewayctl's own deadline defaults to 10 s, the same as the gateway's
	# agent budget, so --timeout 30 keeps the driver from firing first and
	# turning the gateway's ABORTED into a client-side DEADLINE_EXCEEDED.
	set_behavior dn0 <<<'{"objects":{"dn":{"hang":true}}}'
	start=$SECONDS
	gw --timeout 30 --expect ABORTED create-dn --addr localhost:29820 \
		--location rack9 --tr-svc-id 4429
	FAULTS_HUNG_SECS=$((SECONDS - start))

	# The same hang seen through a read: InspectDiskNode's GetDnInfo is an
	# agent call like any other and gets the same 10 s budget (AG3 maps the
	# transport failure to ABORTED).
	gw --timeout 30 --expect ABORTED inspect-dn --addr "$dn0Addr"

	clear_behavior dn0
	# clear_behavior restores: the same read now succeeds. dn_info is null
	# because the fake has applied no SyncupDn — this suite runs no worker —
	# and applied_revision is 0 for the same reason.
	out=$(gw inspect-dn --addr "$dn0Addr")
	assert_field "$out" '.dn_info' "null" "dn0: DnInfo of an unsynced fake"
	assert_field "$out" '.applied_revision' "0" \
		"dn0: the restored fake has still applied nothing"

	# A closed port: grpc.NewClient does not block, so the failure surfaces on
	# the call and is refused immediately — nowhere near the 10 s budget.
	start=$SECONDS
	gwx ABORTED create-dn --addr 127.0.0.1:29999 --location rack9 \
		--tr-svc-id 4429
	FAULTS_FAST_SECS=$((SECONDS - start))

	# A STOPPED fake is the third shape: the process is gone, so the CN's
	# port refuses the connection exactly as a closed one does.
	sig_dir cn2 TERM
	wait_gone cn2 "$WAIT_SHORT"
	gwx ABORTED inspect-cn --addr "$cn2Addr"
	start_fake cn cn2 "$cn2Addr" "--size 0"
	wait_until "$WAIT_SHORT" "cn2's port to listen again" ports_up \
		"$(cn_port 2)"
	# And it is well again afterwards, so nothing later in the case inherits
	# a dead node.
	out=$(gw inspect-cn --addr "$cn2Addr")
	assert_field "$out" '.cn_info' "null" "cn2: CnInfo after the restart"
}

# ---------------------------------------------------------------------------
# The case
# ---------------------------------------------------------------------------

case_faults() {
	CASE=faults

	local ssNqn="$NQN_PREFIX:ss0"
	local out spId slice dataGrp dataLeg metaLeg srcSide dstSide
	local primary cntlrId cnAddr cnDir cnId cloneId migrId
	local dnAddr dnDir dnId spare nonPrimary
	local forceCloneId forceMigrId forceDstSide forceDnAddr forceDnDir

	# -----------------------------------------------------------------
	stage 0 "fixture: the cluster, the nodes and a live sp0 with objects"
	# -----------------------------------------------------------------
	# §10.14's steps 1-3 all speak about "a live sp0": the td-size rule, the
	# ns_idx probe and every precondition need stored state to be checked
	# against, so the whole fixture is arranged first and the five numbered
	# steps below are then exactly the five batteries the section describes.
	new_cluster
	make_nodes
	make_sp sp0
	verify_sp sp0
	assert_eq "$SP_REV" "1" "sp0: SpRev is 1 after create-sp"

	# Four thin devices with three different roles. t0 is never flipped, so
	# it is both the namespace's backing device and step 3's uncreated
	# snapshot origin; tp IS flipped and carries the uncreated snapshot ts.
	gw create-td --sp sp0 --rev "$SP_REV" --name t0 --size "$TD_SIZE" \
		>/dev/null
	refresh_rev sp0
	gw create-td --sp sp0 --rev "$SP_REV" --name t1 --size "$TD_SIZE" \
		>/dev/null
	refresh_rev sp0
	gw create-td --sp sp0 --rev "$SP_REV" --name tp --size "$TD_SIZE" \
		>/dev/null
	refresh_rev sp0
	# §2.4's first flip, the one write workerctl is allowed to make here: the
	# sp-worker's materialization of tp, without which §8.7 refuses to
	# snapshot it. It goes through model.FlipCreated, so it bumps SpRev
	# exactly as the worker's would.
	wctl set-created --sp sp0 --name tp >/dev/null
	refresh_rev sp0
	# A snapshot inherits its origin's size, which is the one case §8.7 lets
	# size be 0 ([D-H]).
	gw create-td --sp sp0 --rev "$SP_REV" --name ts --ori tp >/dev/null
	refresh_rev sp0

	out=$(sp_json sp0)
	assert_field "$out" '.tds | keys | @json' '["t0","t1","tp","ts"]' \
		"sp0: the four thin devices"
	assert_field "$out" '.tds.tp.created' "true" "tp: created after the flip"
	assert_field "$out" '.tds.t0.created' "false" "t0: still uncreated"
	assert_field "$out" '.tds.ts.created' "false" "ts: still uncreated"
	# ori_id is a dev_id, a uint32 — protojson renders it as a NUMBER, so the
	# comparison is made inside jq rather than between two shell strings.
	assert_field "$out" '.tds.ts.ori_id == .tds.tp.dev_id' "true" \
		"ts: ori_id is tp's dev_id"
	assert_field "$(gw list-tds --sp sp0)" '.name_to_td | keys | @json' \
		'["t0","t1","tp","ts"]' "list-tds read-back agrees"

	# One subsystem with one namespace on t0: step 2 needs a live nqn to
	# probe a missing ns_idx in, and step 3 needs both the "subsystem still
	# holds namespaces" and the "td backs a namespace" refusals.
	gw create-ss --sp sp0 --rev "$SP_REV" --nqn "$ssNqn" >/dev/null
	refresh_rev sp0
	gw create-ns --sp sp0 --rev "$SP_REV" --nqn "$ssNqn" --idx 1 --td t0 \
		>/dev/null
	refresh_rev sp0
	out=$(sp_json sp0)
	assert_field "$out" ".subsystems[\"$ssNqn\"].ns_list | length" "1" \
		"ss0: one namespace"
	assert_field "$out" ".subsystems[\"$ssNqn\"].ns_list[0].ns_idx" "1" \
		"ss0: ns_idx 1 (a uint32, so a JSON number)"
	assert_field "$(gw list-sss --sp sp0)" \
		".nqn_to_subsystem[\"$ssNqn\"].model" "dnv" \
		"list-sss read-back: model"

	# One clone whose destination is t1 — step 3's "td is a clone
	# destination" refusal and step 5's DeleteClone subject.
	gw create-clone --sp sp0 --rev "$SP_REV" --name cl0 --dst-td t1 \
		--src-nqn "$NQN_PREFIX:src0" --src-idx 1 --src-slices 1 \
		--src-stripe 65536 --src-block 1048576 >/dev/null
	refresh_rev sp0
	out=$(sp_json sp0)
	assert_field "$out" '.clones.cl0.dst_td_id == .tds.t1.td_id' "true" \
		"cl0: destination is t1"
	assert_field "$out" '.clones.cl0.bm_cnt' "0" \
		"cl0: no source bitmap chunks yet"

	# One migration on the META group's first leg. Meta is deliberate: it
	# leaves the data group's DN set free for the spare leg below, and
	# grpDnAddrs black-lists the whole group either way (§6.5).
	metaLeg=$(sp_leg_id sp0 meta 0 0)
	srcSide=$(sp_side_id sp0 meta 0 0)
	gw create-migr --sp sp0 --rev "$SP_REV" --name m0 \
		--src-side "$srcSide" >/dev/null
	refresh_rev sp0
	out=$(sp_json sp0)
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list | length' \
		"2" "m0: the source leg now carries two sides"
	assert_field "$out" \
		'[ .slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list[].cntlid_slot ] | unique | length' \
		"2" "m0: the destination takes a different cntlid slot (§11.8)"
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list[1].provisioned' \
		"false" "m0: the destination side starts unprovisioned ([D15])"
	out=$(gw get-migr --sp sp0 --name m0)
	migrId=$(jq_of "$out" '.migr.migr_id')
	dstSide=$(jq_of "$out" '.migr.dst_side_id')
	assert_field "$out" '.migr.src_side_id' "$srcSide" \
		"get-migr read-back: src_side_id"
	assert_ne "$dstSide" "$srcSide" "m0: the destination is a new side"

	# One spare leg on the data group — step 3's unprovisioned switch. §8.12
	# writes it parked and unprovisioned, which is exactly the state
	# SwitchSpareLeg must refuse to activate (§9.4).
	dataGrp=$(sp_grp_id sp0 data 0)
	dataLeg=$(sp_leg_id sp0 data 0 0)
	# The reply is captured into a variable of its own rather than read
	# inline: a `$( )` nested in an argument swallows the exit status, and a
	# MUTATION that failed must abort the case rather than yield an empty id.
	out=$(gw create-spare --sp sp0 --rev "$SP_REV" --grp "$dataGrp")
	refresh_rev sp0
	spare=$(jq_of "$out" '.leg_id')
	assert_ne "$spare" "0" "create-spare returned a leg_id"
	out=$(sp_json sp0)
	assert_field "$out" \
		'.slices | to_entries[0].value.data_grp_list[0].spare_leg_list | length' \
		"1" "the data group holds one spare leg"
	assert_field "$out" \
		'.slices | to_entries[0].value.data_grp_list[0].spare_leg_list[0].side_list[0].provisioned' \
		"false" "the spare's side is unprovisioned ([D15])"

	spId=$(sp_id_of sp0)
	slice=$(sp_first_slice sp0)
	assert_ne "$spId" "0" "sp0: sp_id"
	assert_ne "$slice" "0" "sp0: slice_id"

	# -----------------------------------------------------------------
	stage 1 "validation battery: every §7 refusal is INVALID_ARGUMENT"
	# -----------------------------------------------------------------
	# §10.14 asks for ONE shared bracket around the whole batch: the claim
	# being proved is about the batch ("none of these wrote"), and a
	# per-call bracket would only re-assert it more slowly.
	local before after
	before=$(faults_sp_snapshot sp0)
	assert_no_write "step 1: the validation battery" faults_validation_battery
	after=$(faults_sp_snapshot sp0)
	assert_eq "$after" "$before" "step 1: sp0's content must not change"

	# -----------------------------------------------------------------
	stage 2 "NOT_FOUND battery: one probe per resource group"
	# -----------------------------------------------------------------
	before=$(faults_sp_snapshot sp0)
	assert_no_write "step 2: the NOT_FOUND battery" \
		faults_notfound_battery "$ssNqn" "$dataGrp" "$dataLeg"
	after=$(faults_sp_snapshot sp0)
	assert_eq "$after" "$before" "step 2: sp0's content must not change"

	# -----------------------------------------------------------------
	stage 3 "precondition battery on the live sp0"
	# -----------------------------------------------------------------
	# §10.14 wants each of these bracketed on its own: unlike steps 1 and 2
	# every one of them reaches deep into a transaction that WOULD have
	# written had the guard not fired, so which one of them wrote is the
	# question worth being able to answer.
	before=$(faults_sp_snapshot sp0)

	# §8.1: a cluster whose three globals still count objects cannot go.
	assert_no_write "delete-cluster while the cluster is not empty" \
		gwx FAILED_PRECONDITION delete-cluster

	# §8.2: a DN that still hosts sides. The token must be right, or GW6's
	# ABORTED would fire before the guard is reached.
	dnAddr=$(sp_side_addr sp0 data 0 0)
	assert_no_write "delete-dn while it still hosts sides" \
		gwx FAILED_PRECONDITION delete-dn --addr "$dnAddr" \
		--rev "$(dn_rev_of "$dnAddr")"

	# §5.4: an SP that still holds tds, nqns, clones, xfers or migrs.
	assert_no_write "delete-sp while it still holds objects" \
		gwx FAILED_PRECONDITION delete-sp --sp sp0 --rev "$SP_REV"

	# §8.8: a subsystem that still holds namespaces.
	assert_no_write "delete-ss while it still holds a namespace" \
		gwx FAILED_PRECONDITION delete-ss --sp sp0 --rev "$SP_REV" \
		--nqn "$ssNqn"

	# §8.6, both rows: the primary is never deletable, and a non-primary
	# must be disabled first. Both cntlrs of a fresh SP are enabled, so the
	# two ids cover the two guards exactly.
	primary=$(sp_primary_cntlr sp0)
	cntlrId=${primary%% *}
	cnAddr=${primary##* }
	nonPrimary=$(faults_nonprimary_cntlr sp0)
	assert_ne "$nonPrimary" "$cntlrId" "the non-primary cntlr is a different one"
	assert_no_write "delete-cntlr of the primary" \
		gwx FAILED_PRECONDITION delete-cntlr --sp sp0 --rev "$SP_REV" \
		--id "$cntlrId"
	assert_no_write "delete-cntlr of an enabled non-primary" \
		gwx FAILED_PRECONDITION delete-cntlr --sp sp0 --rev "$SP_REV" \
		--id "$nonPrimary"

	# §8.7: create_snap has a kernel-level dependency on the origin's id
	# already being in every slice pool, so a snapshot of an uncreated
	# origin is refused — and refused BEFORE the id minter exists, which is
	# what makes this bracket meaningful (an id consumed here would be
	# visible for ever, since ids are never reused).
	assert_no_write "snapshot of an uncreated origin" \
		gwx FAILED_PRECONDITION create-td --sp sp0 --rev "$SP_REV" \
		--name tsnap --ori t0

	# §8.7's three delete guards, one probe each. All three are evaluated
	# from reads made INSIDE the deleting transaction, which is what makes
	# them race-proof rather than merely correct.
	assert_no_write "delete-td backing a namespace" \
		gwx FAILED_PRECONDITION delete-td --sp sp0 --rev "$SP_REV" --name t0
	assert_no_write "delete-td that is a clone destination" \
		gwx FAILED_PRECONDITION delete-td --sp sp0 --rev "$SP_REV" --name t1
	assert_no_write "delete-td with an uncreated snapshot child" \
		gwx FAILED_PRECONDITION delete-td --sp sp0 --rev "$SP_REV" --name tp

	# §8.11: a leg already carrying two sides has a migration running on it,
	# and a second destination would give the CN three paths to aggregate.
	assert_no_write "create-migr on a leg that already has two sides" \
		gwx FAILED_PRECONDITION create-migr --sp sp0 --rev "$SP_REV" \
		--name m1 --src-side "$srcSide"

	# §9.4: switching in a side that has not finished zeroing would put an
	# unwritten member into the md array.
	assert_no_write "switch-spare with an unprovisioned spare" \
		gwx FAILED_PRECONDITION switch-spare --sp sp0 --rev "$SP_REV" \
		--grp "$dataGrp" --spare "$spare" --target "$dataLeg"

	# The twelfth guard of §10.14 step 3 — an SP-level mutator refused because
	# the SP is `deleting` — has no probe here on purpose: nothing in v1 ever
	# sets SpConf.deleting, so resolveSp's rejectDeleting branch is unit-only
	# and §10.18 records it as such.

	after=$(faults_sp_snapshot sp0)
	assert_eq "$after" "$before" "step 3: sp0's content must not change"
	# The token is still the one the stage started with: a refusal consumes
	# nothing, so every call above was made with a token that was still
	# current when the next one went out.
	assert_eq "$(sp_rev_of sp0)" "$SP_REV" \
		"step 3: SpRev is untouched by twelve refusals"

	# -----------------------------------------------------------------
	stage 4 "agent faults: hung, closed and stopped agents are ABORTED"
	# -----------------------------------------------------------------
	# One bracket for the batch: CreateDiskNode calls GetDnSize before it
	# opens its STM (§8.2), so a hung agent means no transaction is ever
	# started — and InspectDiskNode/InspectControllerNode are read-only to
	# begin with.
	before=$(faults_sp_snapshot sp0)
	FAULTS_HUNG_SECS=0
	FAULTS_FAST_SECS=0
	assert_no_write "step 4: the agent-fault battery" faults_agent_battery
	after=$(faults_sp_snapshot sp0)
	assert_eq "$after" "$before" "step 4: sp0's content must not change"

	# The AG2 bound: DefaultGatewayAgentTimeout is 10 s and §10.14 asks for a
	# generous upper fence rather than a tight one, because the window also
	# contains one ssh round trip and the gateway's own dial.
	assert_between "$FAULTS_HUNG_SECS" 10 15 \
		"a hung listening agent bounds the RPC at the AG2 budget"
	# The other half of the same claim: a refusal that needs no timeout does
	# not spend one.
	assert_between "$FAULTS_FAST_SECS" 0 5 \
		"a closed port is refused without waiting for the budget"

	# -----------------------------------------------------------------
	stage 5 "force = false: hydration must be PROVEN, not assumed"
	# -----------------------------------------------------------------
	# §8.9 / §8.11 make both of these a proof obligation. Removing a
	# dm-clone with regions still unhydrated silently loses every byte that
	# was never pulled from the source, so an incomplete status line, an
	# unparseable one and an unreachable agent are all "not proven" — the
	# same FAILED_PRECONDITION, deliberately. force = true is the other half
	# of the same rule and is proved here too (§10.14 step 5's last clause):
	# an operator whose agent is never coming back must still be able to
	# delete the clone and finish the migration, so each RPC is driven twice
	# against a STOPPED agent — refused unforced, committed forced.

	# --- DeleteClone, on the PRIMARY cntlr's CN.
	cnDir=$(faults_cn_dir "$cnAddr")
	cnId=$(cn_id_of "$cnAddr")
	cloneId=$(jq_of "$(gw get-clone --sp sp0 --name cl0)" '.clone.clone_id')
	faults_seed_cn_state "$cnDir" "$cnId" "$spId" "$cntlrId" "$cloneId"

	# 256/512: the copy is running and unfinished.
	faults_clone_rows "$cnDir" "$spId" "$cntlrId" "$cloneId" "256/512"
	assert_no_write "delete-clone with an unfinished dm-clone" \
		faults_gwx_out FAILED_PRECONDITION delete-clone --sp sp0 \
		--rev "$SP_REV" --name cl0
	assert_field "$FAULTS_OUT" \
		'.message | test("has not finished hydrating")' "true" \
		"cl0: the refusal is the hydration one, not some other precondition"

	# The agent gone. §8.9 is explicit that silence is not proof, so this
	# must be the SAME refusal and never a pass.
	sig_dir "$cnDir" TERM
	wait_gone "$cnDir" "$WAIT_SHORT"
	assert_no_write "delete-clone while the primary's CN agent is stopped" \
		faults_gwx_out FAILED_PRECONDITION delete-clone --sp sp0 \
		--rev "$SP_REV" --name cl0
	assert_field "$FAULTS_OUT" '.message | test("hydration is unproven")' \
		"true" "cl0: an unreachable CN is unproven, not proven-not-done"
	start_fake cn "$cnDir" "$cnAddr" "--size 0"
	wait_until "$WAIT_SHORT" "$cnDir's port to listen again" ports_up \
		"${cnAddr##*:}"

	# 512/512: hydrated >= total with total != 0 is the whole proof.
	faults_clone_rows "$cnDir" "$spId" "$cntlrId" "$cloneId" "512/512"
	out=$(gw delete-clone --sp sp0 --rev "$SP_REV" --name cl0)
	assert_field "$out" '.clone_id' "$cloneId" "delete-clone returns cl0's id"
	refresh_rev sp0
	out=$(sp_json sp0)
	assert_field "$out" '.clones | length' "0" "cl0: the Clone key is gone"
	assert_field "$out" '.sp_conf.clone_name_list | length' "0" \
		"cl0: it left clone_name_list too"
	assert_field "$(gw get-sp --sp sp0)" '.sp_conf.clone_name_list | length' \
		"0" "get-sp read-back agrees the clone is gone"

	# --- DeleteClone with force = true, against the SAME stopped CN.
	# The other half of §8.9: force is not a shortcut past a proof that
	# could be waited for, it is the only exit when the proof can never
	# arrive — a clone whose primary's CN is gone for good. cl0 was spent on
	# the refusals above, so the override needs a clone of its own; t1 is a
	# free destination again now that cl0 is deleted.
	out=$(gw create-clone --sp sp0 --rev "$SP_REV" --name cl-force \
		--dst-td t1 --src-nqn "$NQN_PREFIX:src0" --src-idx 1 \
		--src-slices 1 --src-stripe 65536 --src-block 1048576)
	refresh_rev sp0
	forceCloneId=$(jq_of "$out" '.clone_id')
	assert_ne "$forceCloneId" "0" "cl-force: create-clone returned an id"
	sig_dir "$cnDir" TERM
	wait_gone "$cnDir" "$WAIT_SHORT"
	# The pairing is what §10.14 step 5's last clause asks for: in the very
	# same stopped state, force = false still refuses…
	assert_no_write "delete-clone of cl-force unforced, the CN stopped" \
		faults_gwx_out FAILED_PRECONDITION delete-clone --sp sp0 \
		--rev "$SP_REV" --name cl-force
	assert_field "$FAULTS_OUT" '.message | test("hydration is unproven")' \
		"true" "cl-force: unforced, the stopped CN is still unproven"
	# … and force = true commits anyway, because AG1's GetCntlrInfo is
	# SKIPPED rather than attempted and forgiven — phase 1 does not even
	# resolve the primary, so an unreachable CN cannot fail the call.
	out=$(gw delete-clone --sp sp0 --rev "$SP_REV" --name cl-force --force)
	assert_field "$out" '.clone_id' "$forceCloneId" \
		"delete-clone --force returns cl-force's id"
	refresh_rev sp0
	out=$(sp_json sp0)
	# Ground truth, not the reply code: the override must COMMIT.
	assert_field "$out" '.clones | length' "0" \
		"cl-force: --force deleted the Clone key with the CN unreachable"
	assert_field "$out" '.sp_conf.clone_name_list | length' "0" \
		"cl-force: it left clone_name_list too"
	start_fake cn "$cnDir" "$cnAddr" "--size 0"
	wait_until "$WAIT_SHORT" "$cnDir's port to listen again" ports_up \
		"${cnAddr##*:}"

	# --- FinishMigration, on the DESTINATION side's DN.
	dnAddr=$(faults_side_addr sp0 "$dstSide")
	dnDir=$(faults_dn_dir "$dnAddr")
	dnId=$(dn_id_of "$dnAddr")
	# ext_cnt 1 is the meta group's size; primary_cn_id only has to be a real
	# CN id for the fake to build the side's per-CN rows.
	faults_seed_dn_state "$dnDir" "$dnId" "$spId" "$metaLeg" "$dstSide" \
		"1" "$cnId" "$migrId"

	faults_migr_rows "$dnDir" "$spId" "$metaLeg" "$dstSide" "256/512"
	assert_no_write "finish-migr with an unfinished destination copy" \
		faults_gwx_out FAILED_PRECONDITION finish-migr --sp sp0 \
		--rev "$SP_REV" --name m0
	assert_field "$FAULTS_OUT" \
		'.message | test("has not finished hydrating")' "true" \
		"m0: the refusal is the hydration one"

	sig_dir "$dnDir" TERM
	wait_gone "$dnDir" "$WAIT_SHORT"
	assert_no_write "finish-migr while the destination DN agent is stopped" \
		faults_gwx_out FAILED_PRECONDITION finish-migr --sp sp0 \
		--rev "$SP_REV" --name m0
	assert_field "$FAULTS_OUT" '.message | test("did not report hydration")' \
		"true" "m0: an unreachable destination DN is unproven"
	start_fake dn "$dnDir" "$dnAddr" "--size $DN_SIZE"
	wait_until "$WAIT_SHORT" "$dnDir's port to listen again" ports_up \
		"${dnAddr##*:}"

	faults_migr_rows "$dnDir" "$spId" "$metaLeg" "$dstSide" "512/512"
	out=$(gw finish-migr --sp sp0 --rev "$SP_REV" --name m0)
	assert_field "$out" '.migr_id' "$migrId" "finish-migr returns m0's id"
	refresh_rev sp0
	out=$(sp_json sp0)
	# The commit: the destination is the leg's only side and the source's
	# extents went back to its DN (§8.11).
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list | length' \
		"1" "m0: the leg is back to one side"
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list[0].side_id' \
		"$dstSide" "m0: the survivor is the destination"
	assert_field "$out" '.migrs | length' "0" "m0: the Migration key is gone"
	assert_field "$out" '.sp_conf.migr_name_list | length' "0" \
		"m0: it left migr_name_list too"
	assert_field "$(gw get-sp --sp sp0)" '.sp_conf.migr_name_list | length' \
		"0" "get-sp read-back agrees the migration is gone"

	# --- FinishMigration with force = true, against the SAME stopped DN.
	# §8.11's override, for the migration whose destination DN will never
	# answer again. m0 is spent, so it gets a migration of its own: the leg
	# m0 just finished on carries exactly one side again — m0's destination —
	# and §8.11 refuses only a leg that already has two, so that survivor is
	# a legal source for a second migration.
	out=$(gw create-migr --sp sp0 --rev "$SP_REV" --name m-force \
		--src-side "$dstSide")
	refresh_rev sp0
	forceMigrId=$(jq_of "$out" '.migr_id')
	forceDstSide=$(jq_of "$(gw get-migr --sp sp0 --name m-force)" \
		'.migr.dst_side_id')
	assert_ne "$forceDstSide" "$dstSide" \
		"m-force: the destination is a new side"
	# The gateway picks the destination DN itself (§6.5), so which fake to
	# stop is only knowable from the stored side.
	forceDnAddr=$(faults_side_addr sp0 "$forceDstSide")
	forceDnDir=$(faults_dn_dir "$forceDnAddr")
	sig_dir "$forceDnDir" TERM
	wait_gone "$forceDnDir" "$WAIT_SHORT"
	# Same pairing as the clone above: unforced, the stopped destination
	# leaves hydration unproven…
	assert_no_write "finish-migr of m-force unforced, the dst DN stopped" \
		faults_gwx_out FAILED_PRECONDITION finish-migr --sp sp0 \
		--rev "$SP_REV" --name m-force
	assert_field "$FAULTS_OUT" '.message | test("did not report hydration")' \
		"true" "m-force: unforced, the stopped destination DN is unproven"
	# … and forced, the GetSideInfo is skipped and the finish commits.
	out=$(gw finish-migr --sp sp0 --rev "$SP_REV" --name m-force --force)
	assert_field "$out" '.migr_id' "$forceMigrId" \
		"finish-migr --force returns m-force's id"
	refresh_rev sp0
	out=$(sp_json sp0)
	# Ground truth again: the src side is gone, the destination stands alone
	# and the record is deleted, exactly as on the proven path (§8.11).
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list | length' \
		"1" "m-force: --force finished with the destination DN unreachable"
	assert_field "$out" \
		'.slices | to_entries[0].value.meta_grp_list[0].leg_list[0].side_list[0].side_id' \
		"$forceDstSide" "m-force: the survivor is the destination"
	assert_field "$out" '.migrs | length' "0" \
		"m-force: the Migration key is gone"
	assert_field "$out" '.sp_conf.migr_name_list | length' "0" \
		"m-force: it left migr_name_list too"
	start_fake dn "$forceDnDir" "$forceDnAddr" "--size $DN_SIZE"
	wait_until "$WAIT_SHORT" "$forceDnDir's port to listen again" ports_up \
		"${forceDnAddr##*:}"

	# Leave every fake this case touched exactly as case_reset would: the
	# next case wipes etcd but a stale behavior file would outlive it.
	clear_behavior dn0
	clear_behavior "$dnDir"
	clear_behavior "$cnDir"
	clear_behavior cn2
}

# ---------------------------------------------------------------------------
# Case D — restart (§10.15): kill -9 one instance under load
# ---------------------------------------------------------------------------
#
# The case that proves (d) of §10.1. Two claims, and nothing else:
#
#   crash atomicity — a gateway SIGKILLed with no drain in the middle of a
#   wave of CreateStoragePool calls leaves every SP of that wave either
#   COMPLETE (the whole §5.4 write set) or ABSENT (not one key of it). §5.4
#   puts the entire write set — SpConf, SpName, every Cntlr, every Slice, the
#   per-DN and per-CN bookkeeping and SpRev — into ONE STM, and etcd commits a
#   transaction or it does not, so a third state cannot exist. If one shows up
#   here the case must fail loudly: that is the whole point of the case.
#
#   statelessness — the killed instance restarts on the identical command
#   line with no cleanup step of any kind, because a gateway writes nothing to
#   etcd that describes itself (§0 #3: no leader election, no registration
#   key, no lease, no shard split). Step 5 proves that by enumerating the
#   kinds present in the store and requiring every one of them to be a §5.1
#   kind owned by the DATA.
#
# Every helper below carries the `restart_` prefix so it cannot collide with a
# helper of the four other case files spliced into the same script.

# restart_sp_conf_cnt counts the `sp_conf` keys of one SP NAME inside a full
# key dump. §5.1 joins key fields with a single space and forbids a space in
# any field, so awk's fields ARE the key fields — here
# `dnv sp_conf {cluster_id} {sp_name}` — and $4 is an exact name match rather
# than a substring one (spD1 must never match spD10).
restart_sp_conf_cnt() { # <key dump> <sp name>
	printf '%s\n' "$1" |
		awk -v want="$2" '$2 == "sp_conf" && $4 == want' |
		wc -l | tr -d ' \n'
}

# restart_key_field prints one field of every key of one kind in a dump,
# sorted — the raw material of the "no third state" audit.
#
# It exists because a half-written SP cannot be chased by NAME: only SpConf
# and the capacity keys carry the name, while SpName, SpRev, Cntlr and Slice
# are addressed by the minted sp_id, and an SP with no SpConf has no id the
# script can look up. Comparing ID SETS closes that hole from the other side —
# an id nothing owns is an orphan by construction.
restart_key_field() { # <key dump> <kind> <1-based field index>
	printf '%s\n' "$1" |
		awk -v kind="$2" -v idx="$3" '$2 == kind { print $idx }' | sort
}

# restart_bucket_sum is Σ shard_bucket of one cluster-scoped global. §5.4
# keeps that sum equal to the number of live objects of the role — it is the
# very quantity DeleteCluster uses as its emptiness test instead of a range
# read (§5.1) — so it is an independent count of the SPs that exist, taken
# from a key no SP-scoped STM path touches except through the mint/decrement.
restart_bucket_sum() { # <dn_global|cn_global|sp_global>
	jq_of "$(raw_key "$DNV_PREFIX $1 $(printf '%016x' "$CID")")" \
		'[.shard_bucket[]] | add'
}

# restart_sp_dn_use prints "<dn addr_port> <extents>" once per stored Side of
# one SP. A side occupies its GROUP's ext_cnt on the DN it names — that is the
# quantity §5.4 debits per leg — so the group has to be carried into the side
# iteration. Spare legs are walked too even though CreateStoragePool mints
# none, so the helper stays honest if a later stage ever adds one.
restart_sp_dn_use() { # <sp name>
	jq_of "$(sp_json "$1")" '
		.slices[]
		| (.meta_grp_list[]?, .data_grp_list[]?)
		| . as $grp
		| ($grp.leg_list[]?, $grp.spare_leg_list[]?)
		| .side_list[]?
		| "\(.addr_port) \($grp.ext_cnt)"'
}

# restart_sp_cn_use prints "<cn addr_port> <extents>" once per stored Cntlr of
# one SP. §5.4's CN candidate unit reserves `CandExtCnt = Σ ext_cnt` over ALL
# groups on every CN it picks, so the reservation is the same number for every
# cntlr of the SP; it is derived from the slice records here rather than
# restated as a literal 3.
restart_sp_cn_use() { # <sp name>
	jq_of "$(sp_json "$1")" '
		([ .slices[]
		   | (.meta_grp_list[]?, .data_grp_list[]?)
		   | (.ext_cnt | tostring | tonumber) ] | add) as $cost
		| .cntlrs[]
		| "\(.addr_port) \($cost)"'
}

# restart_assert_accounting recomputes the whole cluster's node bookkeeping
# from the named SPs' own Slice and Cntlr records, then asserts that every
# DnConf, CnConf and capacity index key matches it exactly.
#
# This is a cross-check, not a restatement of what was just read: the extents
# a side occupies live in the Slice VALUE, while free_ext_cnt, side_ptr_list
# and the dn_capacity/cn_capacity index KEYS are separate keys that the same
# §5.4 STM maintains beside it. A partially applied write set shows up here as
# a node debited for an SP that does not exist, or an SP whose DN was never
# debited — which is exactly the "no third state" claim of §10.15 step 3, seen
# from the accounting side.
#
# Called with NO arguments it asserts the pristine state: every DN back to
# DN_EXTENTS free with an empty side_ptr_list, every CN back to CN_EXTENTS.
restart_assert_accounting() { # <sp name…>
	local sp_cnt=$#
	local -A dn_ext dn_sides cn_ext cn_cntlrs
	local sp addr ext i out dump free freehex
	local total_dn=0 total_cn=0

	for sp in "$@"; do
		local sp_dn=0 sp_cn=0
		while read -r addr ext; do
			[ -n "$addr" ] || continue
			dn_ext[$addr]=$((${dn_ext[$addr]:-0} + ext))
			dn_sides[$addr]=$((${dn_sides[$addr]:-0} + 1))
			sp_dn=$((sp_dn + ext))
		done < <(restart_sp_dn_use "$sp")
		# The per-SP totals are the guard that makes a TRUNCATED read fail
		# loudly: a process substitution that produced nothing would
		# otherwise quietly under-charge the cluster and turn a real
		# accounting bug into a passing stage.
		assert_eq "$sp_dn" "$SP_EXT_COST" \
			"$sp: DN extents charged by its own slice records"
		while read -r addr ext; do
			[ -n "$addr" ] || continue
			cn_ext[$addr]=$((${cn_ext[$addr]:-0} + ext))
			cn_cntlrs[$addr]=$((${cn_cntlrs[$addr]:-0} + 1))
			sp_cn=$((sp_cn + 1))
		done < <(restart_sp_cn_use "$sp")
		assert_eq "$sp_cn" "$SP_CNTLR_CNT" \
			"$sp: cntlrs charged by its own cntlr records"
	done

	dump=$(wctl list-keys --prefix "$DNV_PREFIX")

	for i in "${!DN_DIRS[@]}"; do
		addr=$(dn_addr "$i")
		out=$(dn_json "$addr")
		free=$((DN_EXTENTS - ${dn_ext[$addr]:-0}))
		freehex=$(printf '%016x' "$free")
		assert_field "$out" '.total_ext_cnt' "$DN_EXTENTS" \
			"dn$i: total_ext_cnt"
		assert_field "$out" '.free_ext_cnt' "$free" "dn$i: free_ext_cnt"
		assert_eq "$(jq_of "$out" '.side_ptr_list | length')" \
			"${dn_sides[$addr]:-0}" "dn$i: side_ptr_list length"
		# The allocator index key SPELLS the free count (§5.6 zero-pads it so
		# lexical key order is numeric order), so a stale index is visible as
		# a dn_capacity key whose {free_ext_cnt} field disagrees with the
		# DnConf, and a dropped one as no key at all.
		assert_eq "$(printf '%s\n' "$dump" |
			awk -v a="$addr" -v f="$freehex" \
				'$2 == "dn_capacity" && $5 == f && $6 == a' |
			wc -l | tr -d ' \n')" "1" \
			"dn$i: exactly one dn_capacity key at free $free"
		total_dn=$((total_dn + free))
	done

	for i in "${!CN_DIRS[@]}"; do
		addr=$(cn_addr "$i")
		out=$(cn_json "$addr")
		free=$((CN_EXTENTS - ${cn_ext[$addr]:-0}))
		freehex=$(printf '%016x' "$free")
		assert_field "$out" '.total_ext_cnt' "$CN_EXTENTS" \
			"cn$i: total_ext_cnt"
		assert_field "$out" '.free_ext_cnt' "$free" "cn$i: free_ext_cnt"
		assert_eq "$(jq_of "$out" '.cntlr_ptr_list | length')" \
			"${cn_cntlrs[$addr]:-0}" "cn$i: cntlr_ptr_list length"
		assert_eq "$(printf '%s\n' "$dump" |
			awk -v a="$addr" -v f="$freehex" \
				'$2 == "cn_capacity" && $4 == f && $5 == a' |
			wc -l | tr -d ' \n')" "1" \
			"cn$i: exactly one cn_capacity key at free $free"
		total_cn=$((total_cn + free))
	done

	assert_eq "$(printf '%s\n' "$dump" | awk '$2 == "dn_capacity"' |
		wc -l | tr -d ' \n')" "${#DN_DIRS[@]}" "dn_capacity key count"
	assert_eq "$(printf '%s\n' "$dump" | awk '$2 == "cn_capacity"' |
		wc -l | tr -d ' \n')" "${#CN_DIRS[@]}" "cn_capacity key count"

	# And the closed form, straight from the §10.6 constants: the standard SP
	# costs SP_EXT_COST DN extents (meta 1 ext × 2 legs + data init_ext × 2
	# legs) and reserves 1 + init_ext on each of cntlr_cnt CNs. Recomputing
	# the totals from the CONSTANTS as well as from the records is what makes
	# "the survivor set explains the accounting exactly" (§10.15 step 3) a
	# closed statement instead of a tautology over the same read.
	assert_eq "$total_dn" \
		"$((${#DN_DIRS[@]} * DN_EXTENTS - sp_cnt * SP_EXT_COST))" \
		"Σ DN free_ext_cnt with $sp_cnt storage pools alive"
	assert_eq "$total_cn" \
		"$((${#CN_DIRS[@]} * CN_EXTENTS -
			sp_cnt * SP_CNTLR_CNT * (1 + SP_INIT_EXT)))" \
		"Σ CN free_ext_cnt with $sp_cnt storage pools alive"
}

case_restart() {
	CASE=restart

	# §10.5 fixes case D's pool names as spD0..spD9, which is PAR_CLIENTS
	# wide — the same parallel width case A uses.
	local names=() i
	for ((i = 0; i < PAR_CLIENTS; i++)); do names+=("spD$i"); done
	local survivors=() missing=()
	local name sp idx out result=""

	# -----------------------------------------------------------------------
	stage 1 "create the cluster and the seven nodes, sequentially"
	# -----------------------------------------------------------------------
	# §10.15 step 1 explicitly allows the sequential path here: the crash is
	# the subject of the case, the node creation is only its fixture.
	new_cluster
	make_nodes

	# ② ground truth. The globals are the interesting part: `next_id` counts
	# what has been MINTED and Σ shard_bucket what is ALIVE, and step 3 leans
	# on the sp_global pair being trustworthy after a crash, so both are
	# established here while nothing has crashed yet.
	assert_ne "$(jq_of "$(raw_key "$(cluster_key)")" '.creation_epoch')" "0" \
		"cluster_conf creation_epoch"
	local dn_global cn_global sp_global
	dn_global=$(raw_key "$DNV_PREFIX dn_global $(printf '%016x' "$CID")")
	cn_global=$(raw_key "$DNV_PREFIX cn_global $(printf '%016x' "$CID")")
	sp_global=$(raw_key "$DNV_PREFIX sp_global $(printf '%016x' "$CID")")
	assert_field "$dn_global" '.next_id' "$((${#DN_DIRS[@]} + 1))" \
		"DnGlobal next_id after ${#DN_DIRS[@]} DNs"
	assert_field "$cn_global" '.next_id' "$((${#CN_DIRS[@]} + 1))" \
		"CnGlobal next_id after ${#CN_DIRS[@]} CNs"
	assert_field "$sp_global" '.next_id' "1" "SpGlobal next_id, nothing minted"
	assert_eq "$(restart_bucket_sum dn_global)" "${#DN_DIRS[@]}" \
		"Σ DnGlobal shard_bucket"
	assert_eq "$(restart_bucket_sum cn_global)" "${#CN_DIRS[@]}" \
		"Σ CnGlobal shard_bucket"
	assert_eq "$(restart_bucket_sum sp_global)" "0" "Σ SpGlobal shard_bucket"
	assert_eq "$(key_count dn_conf)" "${#DN_DIRS[@]}" "dn_conf key count"
	assert_eq "$(key_count cn_conf)" "${#CN_DIRS[@]}" "cn_conf key count"
	assert_eq "$(key_count dn_rev)" "${#DN_DIRS[@]}" "dn_rev key count"
	assert_eq "$(key_count cn_rev)" "${#CN_DIRS[@]}" "cn_rev key count"
	# No SP yet, so this is the pristine baseline every later step is measured
	# against: 64 free on each DN, 4096 on each CN, one capacity key each.
	restart_assert_accounting

	# ③ gateway read-back.
	assert_eq "$(jq_of "$(gw list-dns)" '.addr_port | length')" \
		"${#DN_DIRS[@]}" "list-dns read-back"
	assert_eq "$(jq_of "$(gw list-cns)" '.addr_port | length')" \
		"${#CN_DIRS[@]}" "list-cns read-back"

	# -----------------------------------------------------------------------
	stage 2 "race ${#names[@]} create-sp over gw0-2 and SIGKILL gw1 mid-flight"
	# -----------------------------------------------------------------------
	# ① the calls. `race` reads one JSON object per line on stdin and turns
	# each params object into the very argv the create-sp flag set parses
	# (§10.8), so this is the §10.6 SP shape spelled as JSON — and `--raid1`
	# is a bool flag, hence a JSON true rather than a string (paramArgs then
	# emits `--raid1=true`, the only spelling Go's flag package accepts).
	local shape jobs=()
	shape=$(printf '"cntlr-cnt":%d,"slice-cnt":%d,"init-ext-cnt":%d,"raid1":true' \
		"$SP_CNTLR_CNT" "$SP_SLICE_CNT" "$SP_INIT_EXT")
	for i in "${!names[@]}"; do
		jobs+=("{\"op\":\"create-sp\",\"params\":{\"sp\":\"${names[$i]}\",$shape}}")
	done

	# The race has to run in the BACKGROUND: its jobs are released against one
	# barrier and then block on their RPCs, so the kill can only land while
	# they are in flight if the driver is free to issue it. `gw race` runs
	# gatewayctl on the SERVER over ssh and the gw wrapper passes stdin
	# through, so the job lines travel down ssh's stdin.
	local racefile racepid
	racefile=$(mktemp)
	(printf '%s\n' "${jobs[@]}" |
		gw race --targets "$(gw_addr 0),$(gw_addr 1),$(gw_addr 2)" \
			>"$racefile") &
	racepid=$!
	# Process control, not a wait for etcd content, so §10.10 permits it: the
	# barrier has to have been released and the jobs have to have reached the
	# gateways before a kill means anything at all.
	sleep 0.5
	kill_gw 1
	# The race's own exit status is deliberately ignored: it exits non-zero
	# only on a HARNESS failure, and a job that came back UNAVAILABLE because
	# we shot its gateway is the measurement, not an error.
	wait "$racepid" || true
	result=$(cat "$racefile")
	rm -f "$racefile"

	[ -n "$result" ] || die "the background race produced no output at all"
	assert_eq "$(jq_of "$result" 'length')" "${#names[@]}" \
		"race results, one per job"
	# kill_gw has already waited for the recorded pid to be gone; this is the
	# other half of "the instance is gone" — it is not serving either.
	if gw_ready 1; then
		die "gw1 still answers a ping after kill -9"
	fi
	# ② and ③ for this stage are step 3 below: §10.15 splits the load from its
	# audit, and the audit is the part that has to run against a settled
	# store, after the last in-flight STM has either committed or died.

	# -----------------------------------------------------------------------
	stage 3 "partition the results and prove every SP is complete or absent"
	# -----------------------------------------------------------------------
	local dump code gw1_jobs=0
	dump=$(wctl list-keys --prefix "$DNV_PREFIX")

	for idx in "${!names[@]}"; do
		name=${names[$idx]}
		# race's output array is ordered by INPUT index (§10.8), and the jobs
		# were emitted in name order, so idx addresses both.
		code=$(jq_of "$result" "[.[] | select(.idx == $idx)][0].code")
		if [ "$((idx % 3))" -eq 1 ]; then
			gw1_jobs=$((gw1_jobs + 1))
			# --targets round-robins a job with no explicit gateway as
			# targets[idx % len(targets)], so idx ≡ 1 (mod 3) is exactly the
			# set of jobs that were pointed at the instance we SIGKILLed.
			# Either the STM committed before the signal landed (OK) or the
			# transport died under the client (UNAVAILABLE, or ABORTED when
			# the gateway had already mapped the failure itself).
			case "$code" in
			OK | UNAVAILABLE | ABORTED) ;;
			*)
				die "job $idx ($name) on gw1: code $code, want one of" \
					"OK/UNAVAILABLE/ABORTED:" \
					"$(jq_of "$result" \
						"[.[] | select(.idx == $idx)][0].message")"
				;;
			esac
		else
			# A surviving instance has no excuse: killing a PEER must not
			# disturb it (§0 #3 — they share nothing but etcd).
			assert_eq "$code" "OK" \
				"job $idx ($name) on gw$((idx % 3)), an untouched instance"
		fi

		if [ "$(restart_sp_conf_cnt "$dump" "$name")" = "1" ]; then
			# COMPLETE is not "an sp_conf exists": verify_sp is §10.11 step
			# 6's full §5.4 write-set check — SpConf lists, SpName, SpRev,
			# both cntlrs with exactly one primary and distinct slots, the
			# slice with its meta and data groups on four distinct DNs, and
			# `missing` empty, which is workerctl reporting that no key the
			# SpConf LISTS is absent.
			verify_sp "$name"
			survivors+=("$name")
		else
			assert_eq "$(restart_sp_conf_cnt "$dump" "$name")" "0" \
				"$name: sp_conf keys (want the complete set or none)"
			# The converse direction: an OK reply is a committed transaction,
			# so an OK job whose write set is gone would be a lost commit.
			assert_ne "$code" "OK" \
				"$name: create-sp replied OK but left no sp_conf key"
			missing+=("$name")
		fi
	done

	# A summary tripwire on what the per-job asserts above already imply:
	# every non-gw1 job was OK and every missing SP's job was not, so an SP
	# can only be missing if it was one of gw1's. It also pins the survivor
	# list as non-empty, which is what makes the unguarded array expansions
	# below safe.
	assert_ge "$gw1_jobs" "${#missing[@]}" \
		"missing SPs must all come from the killed instance's jobs"
	assert_ge "${#survivors[@]}" "$((${#names[@]} - gw1_jobs))" \
		"surviving SPs (only gw1's jobs may fail)"

	# The orphan audit. A name with no sp_conf key has no id to chase, so the
	# check runs over ID SETS instead: every sp_id_to_name, sp_rev, cntlr and
	# slice key in the store must belong to a SURVIVING SP. A write set
	# applied halfway would show as an id no SpConf owns; one truncated the
	# other way as a survivor missing from a set. Field indexes are the §5.1
	# key layouts: sp_id_to_name {cid} {sp_id}, sp_rev {shard} {cid} {sp_id},
	# cntlr/slice {cid} {sp_id} {sub id}.
	local surv_ids
	surv_ids=$(for sp in "${survivors[@]}"; do
		printf '%016x\n' "$(sp_id_of "$sp")"
	done | sort)
	assert_eq "$(restart_key_field "$dump" sp_id_to_name 4)" "$surv_ids" \
		"sp_id_to_name keys must belong exactly to the surviving SPs"
	assert_eq "$(restart_key_field "$dump" sp_rev 5)" "$surv_ids" \
		"sp_rev keys must belong exactly to the surviving SPs"
	assert_eq "$(restart_key_field "$dump" cntlr 4 | uniq)" "$surv_ids" \
		"cntlr keys must belong exactly to the surviving SPs"
	assert_eq "$(restart_key_field "$dump" slice 4)" "$surv_ids" \
		"slice keys must belong exactly to the surviving SPs"
	assert_eq "$(key_count sp_conf)" "${#survivors[@]}" "sp_conf key count"
	assert_eq "$(key_count cntlr)" \
		"$((${#survivors[@]} * SP_CNTLR_CNT))" "cntlr key count"
	assert_eq "$(key_count slice)" \
		"$((${#survivors[@]} * SP_SLICE_CNT))" "slice key count"
	assert_eq "$(restart_bucket_sum sp_global)" "${#survivors[@]}" \
		"Σ SpGlobal shard_bucket must count exactly the survivors"

	# The accounting, recomputed from the survivor set alone.
	restart_assert_accounting "${survivors[@]}"

	# ③ gateway read-back, through a gateway that never died.
	assert_eq "$(jq_of "$(gw list-sps)" '.sp_name[]' | sort)" \
		"$(printf '%s\n' "${survivors[@]}" | sort)" \
		"list-sps read-back must name exactly the surviving SPs"
	log "  ${#survivors[@]} SPs survived the kill, ${#missing[@]} to re-drive"
	if [ "${#missing[@]}" -eq 0 ]; then
		# Not a failure: the either-or audit above is the assertion and it
		# ran in full. But it means the SIGKILL landed after the last job had
		# already committed, so this run exercised no in-flight crash — worth
		# saying out loud rather than passing silently, because the split is
		# timing-dependent and a run that never races proves less.
		log "  NOTE: nothing was in flight when gw1 died; no job was lost"
	fi

	# -----------------------------------------------------------------------
	stage 4 "re-drive the lost SPs against gw0, sequentially"
	# -----------------------------------------------------------------------
	# ① Only the SPs whose write set is genuinely absent are re-driven. A job
	# that reported a transport failure while its STM had already committed is
	# a LOST REPLY, not a lost write, and re-driving it would (correctly) come
	# back ALREADY_EXISTS — so the missing list, not the failed list, is what
	# a client would retry, and step 3 has already proved the two coincide
	# except for that one case.
	if [ "${#missing[@]}" -gt 0 ]; then
		for name in "${missing[@]}"; do
			# make_sp drives gw0 (the harness default GATEWAY) and refreshes
			# the cached token in the PARENT shell, per §10.10.
			make_sp "$name"
		done
	fi

	# ② every one of the ten is now a complete §5.4 write set.
	for name in "${names[@]}"; do verify_sp "$name"; done
	assert_eq "$(key_count sp_conf)" "${#names[@]}" "sp_conf key count"
	assert_eq "$(key_count sp_id_to_name)" "${#names[@]}" \
		"sp_id_to_name key count"
	assert_eq "$(key_count sp_rev)" "${#names[@]}" "sp_rev key count"
	assert_eq "$(restart_bucket_sum sp_global)" "${#names[@]}" \
		"Σ SpGlobal shard_bucket"
	restart_assert_accounting "${names[@]}"

	# ③ read-back.
	assert_eq "$(jq_of "$(gw list-sps)" '.sp_name[]' | sort)" \
		"$(printf '%s\n' "${names[@]}" | sort)" "list-sps read-back"

	# -----------------------------------------------------------------------
	stage 5 "restart gw1 unchanged, drive it, and prove nothing describes it"
	# -----------------------------------------------------------------------
	# start_gw is the same launch line setup() and case_reset() use — there is
	# no recovery flag, no fencing step and no cleanup between the kill and
	# this call, which is the operational half of §0 #3.
	start_gw 1
	wait_until "$WAIT_SHORT" "gw1 to answer a ping after the restart" \
		gw_ready 1

	# ① one full create/delete round trip THROUGH the restarted instance. The
	# token is minted by a read on gw0 and spent on gw1 on purpose: the SpRev
	# token is state in ETCD, not in the instance that issued it (§5.5), so a
	# rejoining gateway must honour a peer's token immediately.
	local gw1_addr
	gw1_addr=$(gw_addr 1)
	refresh_rev "${names[0]}"
	out=$(gw_at "$gw1_addr" create-td --sp "${names[0]}" --rev "$SP_REV" \
		--name t0 --size "$TD_SIZE")
	refresh_rev "${names[0]}"
	# dev_id is a uint32 and therefore a JSON NUMBER; td_id is a uint64 and
	# therefore a STRING. §5.6 hands out dev_id 1 for the SP's first td.
	assert_field "$out" '.dev_id' "1" "create-td through gw1: dev_id"
	assert_ne "$(jq_of "$out" '.td_id')" "0" "create-td through gw1: td_id"
	# ② ground truth, ③ read-back through gw1 itself.
	assert_eq "$(jq_of "$(sp_json "${names[0]}")" '.tds | keys | join(",")')" \
		"t0" "${names[0]}: stored thin devices"
	assert_eq "$(jq_of "$(gw_at "$gw1_addr" list-tds --sp "${names[0]}")" \
		'.name_to_td | keys | join(",")')" "t0" \
		"list-tds read-back through gw1"
	gw_at "$gw1_addr" delete-td --sp "${names[0]}" --rev "$SP_REV" --name t0 \
		>/dev/null
	refresh_rev "${names[0]}"
	assert_eq "$(key_count thin_device)" "0" \
		"thin_device keys after the round trip"
	assert_eq "$(jq_of "$(sp_json "${names[0]}")" \
		'.sp_conf.td_name_list | length')" "0" \
		"${names[0]}: td_name_list after the round trip"

	# The statelessness assertion (§0 #3, GW1). Every key's SECOND field is
	# its §5.1 kind, so the set of kinds present IS the set of things the
	# store describes. All three instances are serving and one of them has
	# just (re)started, so a gateway that registered itself, took a lease or
	# claimed a shard would be visible right now — and `worker`, the one
	# registration kind the schema does define, is named explicitly below so
	# the negative reads as the negative it is.
	local observed allowed unexpected
	dump=$(wctl list-keys --prefix "$DNV_PREFIX")
	observed=$(printf '%s\n' "$dump" | awk '{ print $2 }' | sort -u)
	allowed=$(printf '%s\n' cluster_conf dn_global cn_global sp_global \
		dn_rev cn_rev sp_rev dn_conf cn_conf dn_capacity cn_capacity \
		sp_conf sp_id_to_name cntlr slice thin_device subsystem cdc \
		clone clone_bitmap transfer migration migration_bitmap | sort)
	unexpected=$(printf '%s\n' "$observed" |
		grep -Fxv -f <(printf '%s\n' "$allowed") || true)
	[ -z "$unexpected" ] || die "key kinds the DATA does not own:" \
		"$(printf '%s' "$unexpected" | tr '\n' ' ')"
	assert_eq "$(printf '%s\n' "$observed" | grep -cx worker || true)" "0" \
		"worker-style registration keys (§5.1 kind \"worker\")"
	log "  key kinds in the store: $(printf '%s' "$observed" | tr '\n' ' ')"

	# -----------------------------------------------------------------------
	stage 6 "delete the ten SPs round-robin over gw0-2 and check the reversal"
	# -----------------------------------------------------------------------
	local want_id
	for idx in "${!names[@]}"; do
		name=${names[$idx]}
		want_id=$(sp_id_of "$name")
		# ① a fresh token per SP, read in the parent shell, then spent on a
		# different instance than the one that read it — the same peer-token
		# point as step 5, now across all three.
		refresh_rev "$name"
		out=$(gw_at "$(gw_addr "$((idx % 3))")" delete-sp --sp "$name" \
			--rev "$SP_REV")
		# sp_id is a uint64, so protojson renders it as a STRING.
		assert_field "$out" '.sp_id' "$want_id" "$name: delete-sp reply sp_id"
		# No refresh_rev here: DeleteStoragePool removes SpRev along with the
		# rest of the write set, so there is no token left to cache.
	done

	# ② §5.4's delete is one STM that tears everything down, so every kind the
	# SPs owned must be gone, not merely thinned out.
	assert_eq "$(key_count sp_conf)" "0" "sp_conf keys after the teardown"
	assert_eq "$(key_count sp_id_to_name)" "0" "sp_id_to_name keys"
	assert_eq "$(key_count sp_rev)" "0" "sp_rev keys"
	assert_eq "$(key_count cntlr)" "0" "cntlr keys"
	assert_eq "$(key_count slice)" "0" "slice keys"
	assert_eq "$(restart_bucket_sum sp_global)" "0" \
		"Σ SpGlobal shard_bucket after the teardown"
	# SpGlobal.next_id never rewinds — ids are minted, not recycled — so the
	# ten SPs must still be accounted for in the mint counter even though none
	# of them exists any more.
	assert_field "$(raw_key "$DNV_PREFIX sp_global $(printf '%016x' "$CID")")" \
		'.next_id' "$((${#names[@]} + 1))" "SpGlobal next_id after ten mints"
	# No arguments: every DN back to 64 free with an empty side_ptr_list,
	# every CN back to 4096, every capacity key rewritten to match.
	restart_assert_accounting

	# ③ read-back.
	assert_eq "$(jq_of "$(gw list-sps)" '.sp_name | length')" "0" \
		"list-sps read-back after the teardown"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/gateway_test.sh [--only <case>] [--cleanup-only] user@ip

  --only <case>   run one of: ${CASES[*]}
  --cleanup-only  run the §10.16 cleanup on the server and exit
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
		case_reset "$name"
		"case_$name"
	done
}

main "$@"
