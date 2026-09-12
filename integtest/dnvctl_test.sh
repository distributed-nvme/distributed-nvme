#!/usr/bin/env bash
#
# dnvctl_test.sh — the `dnvctl` integration test of doc/dnvctl.md §7. One VM,
# one `integtest/fakegateway` process standing in for the whole control plane,
# and the real `bin/dnvctl` driven over ssh. Nothing here tests the gateway:
# the fake records what dnvctl put on the wire and answers whatever
# behavior.json tells it to, so every assertion below is about dnvctl's own
# argv→request marshalling, §3.1 rendering, §3.2 error surface and §2.3 trace
# ids (§7.1).
#
#   bash integtest/dnvctl_test.sh [--only <case>] [--cleanup-only] user@ip
#
# Cases (§7.9-§7.13), in order, each against a RESTARTED fake with an empty
# behavior.json and an empty state.json: smoke, sweep, behavior, errors,
# transport. Cleanup runs unconditionally at the start and, on success only, at
# the end: a failing run leaves $WORK, the fake's log and both JSON files in
# place and dumps the §7.15 diagnostics.
#
# NO SUDO anywhere: nothing in this suite needs root, and no Go toolchain is
# needed on the VM — both binaries are built here and scp'd.
#
# What it proves (§7.1): (a) every one of the 59 commands marshals its argv
# into exactly the intended request proto, asserted FIELD FOR FIELD against
# the fake's state.json — a whole-object comparison, so an unexpected key
# fails as loudly as a wrong value, which is what makes "no sp_rev key" and
# "no dm_raid0_conf key" provable rather than asserted; (b) the §4 token trio
# — absent / `--rev 0` / `--rev 0x1f` — reaches the wire as absent message /
# present-and-empty / revision 31; (c) replies render per §3.1, with byte-exact
# goldens for the four §7.10 representatives; (d) failures render per §3.2 and
# usage errors issue no RPC at all; (e) trace ids arrive on the wire, minted
# when not given. Correctness only: the two timing assertions (§7.13) bound a
# deadline, not a latency.
#
# Log reading: fakegateway.log is one JSON object per line, and a log being
# appended to while we `cat` it can end in a torn line, so every read goes
# through `jq -R 'fromjson? // empty'` (recs/recsr below) — a partial last line
# is dropped, never fails an assertion. state.json needs no such guard: the
# fake writes it temp-file-then-rename, so a reader never sees a partial one.
#
# jq: every filter in this file runs on the DRIVER, never on the VM. resolve_jq
# picks a system jq when there is one and the gojq drop-in otherwise, so the
# filters are deliberately kept to what both accept (no jq-only builtins); the
# VM's own /usr/bin/jq is never invoked, which is why the suite works against a
# target that has none.
#
# Round trips: ctl_exec does ONE ssh per dnvctl invocation and brings back five
# things — state.json before, the exit code, stderr, stdout, state.json after.
# That is what keeps a 59-step sweep to 59 ssh calls, and it is why every
# ctl_ok/ctl_fail/ctl_usage MUST run in the PARENT shell: the five results land
# in globals, and a subshell would drop them (and would swallow a die).
#
# Signals: only one process is ever started, but its pid is still recorded from
# `$!` into $WORK/fgw/pid at launch and every signal goes by that pid. The
# `pkill -f` forms appear in cleanup() only.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (§7.3, §7.6)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
DNVCTL_BIN="$REPO_ROOT/bin/dnvctl"
FAKEGW_BIN="$BIN_DIR/fakegateway"

WORK=/var/tmp/dnv-dnvctl-integtest

# §7.3 ports. 29841 is deliberately never listened on — the transport case
# dials it expecting a refusal — so preflight must prove BOTH free, and
# cleanup must leave BOTH free. Both are outside every range the other five
# suites and production use (15379/15380, 29810-29832, 29527, 2379, 295xx,
# 296xx, 297xx, 298[1-3]x).
FGW_PORT=29840
DEAD_PORT=29841
ALL_PORTS=(29840 29841)

# §7.6 identity plan. Nothing below is dialled: the DN/CN addresses and every
# nqn are payload the fake only ever records.
CLUSTER=itctl
SP=sp0
DN_ADDR=127.0.0.1:29901
CN_ADDR=127.0.0.1:29902
DN_LOCATION=rack0
CN_LOCATION=rack1
NQN=nqn.2025-01.io.dnv:itctl:sp0:ss0
HOST0=nqn.2025-01.io.dnv:host0
SRC_NQN=nqn.2025-01.io.dnv:src:ss0
UUID=6f7d0f3e-0dd6-4f22-9a34-5e0f1a2b3c4d
NGUID=00112233445566778899aabbccddeeff
TD_SIZE=67108864

# §7.10 goldens. Step 03 pins EmitUnpopulated + canonical key order against the
# fake's canned EMPTY reply; step 19 pins uint64-as-string with an injected
# revision above 2^53, which a JSON number could not carry intact; steps 33/34
# pin the §3.1 hex map, the one place dnvctl does not emit a proto message.
GOLD_CLUSTER_GET='{"cluster_conf":null,"cluster_id":"0","cluster_name":"","cn_global":null,"dn_global":null,"sp_global":null}'
SP_GET_REV=9007199254740993
GOLD_SP_GET='{"cntlr_list":[],"slice_list":[],"sp_conf":null,"sp_name":"","sp_rev":{"revision":"9007199254740993","sp_name":"sp0"}}'
GOLD_TD_BM='{"bitmap_hex":"a5","byte_cnt":1}'
GOLD_LEG_BM='{"bitmap_hex":"a5a5","byte_cnt":2}'

# The §7.5 base64 renderings of the two --bm-hex values. protojson renders a
# bytes field as standard base64, so this is what `bitmap` looks like in
# state.json: a5 -> pQ==, a5a5 -> paU=.
BM_A5_B64=pQ==
BM_A5A5_B64=paU=

WAIT_READY=15
CASES=(smoke sweep behavior errors transport)

# FRAME separates the five sections ctl_exec brings back in one ssh. It must
# survive `echo` in the REMOTE shell unquoted, which rules out a leading `#`
# (that would start a comment) and anything the shell expands.
FRAME=@@dnvctl-frame@@

# ---------------------------------------------------------------------------
# Mutable state
# ---------------------------------------------------------------------------

TARGET=""
IP=""
JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE=setup
TRACE=it-setup
STAGE="(startup)"
SETUP_DONE=0
QUIET=0

# The four levers of the §7.6 global argv prefix. ctl_defaults restores them;
# only the three steps that need a different prefix touch them (smoke s2 drops
# --trace-id to watch the §2.3 mint, behavior b6 drops --cluster and adds a
# DNVCTL_* assignment, transport d1 dials the dead port).
CTL_ADDR="127.0.0.1:$FGW_PORT"
CTL_TRACE=1
CTL_CLUSTER=1
CTL_ENV=""

# The five results of the last ctl_exec. They are globals on purpose: see the
# "Round trips" paragraph in the header.
CTL_RC=""
CTL_OUT=""
CTL_ERR=""
CTL_PRE=""
CTL_POST=""

# The sweep's per-step trace id and RPC, for the one end-of-case log audit.
SWEEP_TRACES=()
SWEEP_RPCS=()

declare -A PID=()     # dir -> pid recorded from $! at launch
declare -A RUNNING=() # dir -> 1 while we believe the process is alive

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling (§7.2, verbatim from gateway_test.sh)
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the §7.6 trace id
# `it-<case>-<step>`, which every dnvctl call of the step then stamps on the
# fake's records — the thread that ties a script stage to fakegateway.log.
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

# assert_field reads one field out of a JSON document. protojson renders 64-bit
# fields as JSON STRINGS and 32-bit ones as numbers, so every numeric
# comparison here goes through text.
assert_field() { # <json> <filter> <want> <label>
	assert_eq "$(jq_of "$1" "$2")" "$3" "$4"
}

# assert_jq is the §7.7 spot-check: a `jq -e` filter that must be truthy.
assert_jq() { # <json> <filter> <label>
	printf '%s' "$1" | "$JQ" -e "$2" >/dev/null ||
		die "$3 (filter: $2, document: $1)"
}

# assert_parses is §7.7's "dnvctl stdout must satisfy jq -e ." — run on every
# ctl_ok, because a command that renders unparseable JSON has failed even when
# every field it names is right.
assert_parses() { # <json> <label>
	printf '%s' "$1" | "$JQ" -e . >/dev/null ||
		die "$2: stdout is not one parseable JSON document: '$1'"
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
		log "########## diagnostics (§7.15) ##########"
		diagnostics || true
		log ""
		log "debris left in place on $TARGET; failing stage '$STAGE'"
		log "pull the stage's records with:"
		log "  jq 'select(.trace_id==\"$TRACE\")' $(fgw_log)"
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution (§7.3)
# ---------------------------------------------------------------------------

# sshw echoes the command and runs it on the server as the plain user. QUIET is
# raised while wait_until polls, so a poll does not bury the transcript.
sshw() {
	local cmd="$*"
	[ "$QUIET" -eq 1 ] || log "[server] $cmd"
	ssh "${SSH_OPTS[@]}" "$TARGET" "$cmd"
}

sshw_ok() { sshw "$@" || true; }

# rlog is the §7.15 log reader: `ssh … cat <path>`, so every assertion is
# parsed by the DRIVER's jq and the server needs no jq of its own. A missing
# file yields empty output rather than a failure, because `set -o pipefail`
# would otherwise turn "the file does not exist yet" into a script abort inside
# a polling predicate. A read that fails for any reason OTHER than "the file
# does not exist yet" must NOT read as an empty file, or every count-based
# negative assertion would pass on a broken ssh — so probe first and let a real
# failure propagate.
rlog() {
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"if [ -e $(printf '%q' "$1") ]; then cat -- $(printf '%q' "$1"); fi"
}

fgw_log() { printf '%s/fgw/fakegateway.log' "$WORK"; }
fgw_dir() { printf '%s/fgw' "$WORK"; }

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

# req_recs selects the interceptor's request records of one RPC under one trace
# id (§7.7). The wire method is `/Gateway/<Rpc>` — schema.proto declares no
# proto package, so there is no package prefix — and no Gateway RPC name is a
# suffix of another, which is what makes the spec's `endswith` matcher exact.
# Case S step 1 pins the full method string once, so a future package statement
# would be caught there rather than silently widening this filter.
req_recs() { # <Rpc> <trace id>
	recs "$(fgw_log)" \
		'select(.msg == "grpc server request" and .trace_id == $t and
			(.method | endswith($m)))' --arg m "$1" --arg t "$2"
}

count_req_recs() { req_recs "$@" | wc -l | tr -d ' \n'; }

# ---------------------------------------------------------------------------
# Polling (§7.8: only for process readiness — every dnvctl call is synchronous,
# so no stage ever sleeps waiting for the fake to catch up)
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
# The dnvctl driver (§7.6)
# ---------------------------------------------------------------------------

ctl_defaults() {
	CTL_ADDR="127.0.0.1:$FGW_PORT"
	CTL_TRACE=1
	CTL_CLUSTER=1
	CTL_ENV=""
}

# ctl_prefix is the §7.6 global argv prefix, built from the four levers.
ctl_prefix() {
	local prefix="$WORK/bin/dnvctl --gateway-address $CTL_ADDR"
	if [ "$CTL_CLUSTER" -eq 1 ]; then
		prefix="$prefix --cluster $CLUSTER"
	fi
	prefix="$prefix --sp $SP"
	if [ "$CTL_TRACE" -eq 1 ]; then
		prefix="$prefix --trace-id $TRACE"
	fi
	printf '%s' "$prefix"
}

# ctl_snapshot is the remote fragment that prints state.json if it exists. A
# state.json that is not there yet (nothing has been called) must read as an
# empty document, not as a broken ssh — same rule as rlog.
ctl_snapshot() {
	printf 'if [ -e %s/state.json ]; then cat %s/state.json; fi' \
		"$(fgw_dir)" "$(fgw_dir)"
}

# ctl_section splits one framed reply. Sections are 1-based in the order
# ctl_exec emits them: state before, exit code, stderr, stdout, state after.
ctl_section() { # <framed> <n>
	printf '%s\n' "$1" |
		awk -v want="$2" -v frame="$FRAME" '
			BEGIN { sec = 1 }
			$0 == frame { sec++; next }
			sec == want { print }
		'
}

# ctl_exec runs ONE dnvctl invocation on the server and fills the five CTL_*
# globals. It MUST run in the parent shell (header, "Round trips").
#
# Each argument is quoted for the REMOTE shell with printf %q before the
# command string is built — without that an argument containing a space or a
# shell metacharacter is re-split by the remote shell.
#
# stdout, stderr and the exit code go to three files under $WORK first and are
# catted back afterwards, rather than being interleaved on one stream: that is
# the only way to keep "stdout is empty" and "stderr is exactly one line"
# separable, and it leaves the last invocation's raw output on the VM for the
# §7.15 dump.
ctl_exec() { # <args…>
	local quoted remote framed
	quoted=$(printf '%q ' "$@")
	remote="$(ctl_snapshot); echo $FRAME;"
	remote="$remote $CTL_ENV$(ctl_prefix) $quoted"
	remote="$remote >$WORK/last.out 2>$WORK/last.err; echo \$? >$WORK/last.rc;"
	remote="$remote cat $WORK/last.rc; echo $FRAME;"
	remote="$remote cat $WORK/last.err; echo $FRAME;"
	remote="$remote cat $WORK/last.out; echo $FRAME;"
	remote="$remote $(ctl_snapshot)"
	[ "$QUIET" -eq 1 ] || log "[server] ${CTL_ENV}dnvctl $*"
	framed=$(ssh "${SSH_OPTS[@]}" "$TARGET" "$remote")
	CTL_PRE=$(ctl_section "$framed" 1)
	CTL_RC=$(ctl_section "$framed" 2)
	CTL_ERR=$(ctl_section "$framed" 3)
	CTL_OUT=$(ctl_section "$framed" 4)
	CTL_POST=$(ctl_section "$framed" 5)
	[ -n "$CTL_RC" ] || die "dnvctl $*: the framed reply carried no exit code"
}

# ctl_ok is the success wrapper (§7.6): exit 0, EMPTY stderr — CT7's Warn
# silencing and stdout reservation, asserted on every single success — and
# stdout that parses. The result document stays in $CTL_OUT.
ctl_ok() { # <args…>
	ctl_exec "$@"
	assert_eq "$CTL_RC" "0" "exit code of: dnvctl $*"
	assert_eq "$CTL_ERR" "" "stderr of: dnvctl $* must be empty (CT7)"
	assert_parses "$CTL_OUT" "dnvctl $*"
}

# ctl_fail is the §3.2 exit-1 wrapper: empty stdout and ONE stderr line of the
# exact shape `dnvctl: <CODE>: <message> (trace_id <id>)`. The message is only
# shape-checked here; ctl_fail_msg compares the whole line.
ctl_fail() { # <UPPER_SNAKE code> <args…>
	local code=$1
	shift
	ctl_exec "$@"
	assert_eq "$CTL_RC" "1" "exit code of: dnvctl $*"
	assert_eq "$CTL_OUT" "" "stdout of a failure must be empty"
	local lines matched
	# grep -c, never grep -q: `set -o pipefail` plus a reader that closes the
	# pipe early turns a SIGPIPE on the writer into a script abort.
	lines=$(printf '%s\n' "$CTL_ERR" | grep -c . || true)
	assert_eq "$lines" "1" "stderr of a failure must be exactly one line"
	matched=$(printf '%s\n' "$CTL_ERR" |
		grep -c "^dnvctl: $code: .* (trace_id $TRACE)\$" || true)
	assert_ne "$matched" "0" \
		"stderr must be 'dnvctl: $code: … (trace_id $TRACE)', got '$CTL_ERR'"
}

# ctl_fail_msg is ctl_fail for the failures whose message is ours to predict —
# every behavior.json injection, where the fake's message is either the one we
# wrote or its documented "behavior.json <CODE>" default.
ctl_fail_msg() { # <UPPER_SNAKE code> <message> <args…>
	local code=$1 message=$2
	shift 2
	ctl_fail "$code" "$@"
	assert_eq "$CTL_ERR" "dnvctl: $code: $message (trace_id $TRACE)" \
		"the whole §3.2 stderr line"
}

# ctl_usage is the §3.2 exit-2 wrapper. The "no RPC was issued" half is what
# matters and it is proved, not asserted: the caller names the RPC the command
# would have driven and the fake's own counter must not have moved across the
# call (§7.12 c5/c6).
ctl_usage() { # <Rpc the command would drive> <args…>
	local rpc=$1
	shift
	ctl_exec "$@"
	assert_eq "$CTL_RC" "2" "exit code of: dnvctl $*"
	assert_eq "$CTL_OUT" "" "stdout of a usage error must be empty"
	assert_ne "$CTL_ERR" "" "a usage error must say something on stderr"
	assert_count_delta "$rpc" 0 "a usage error must issue no RPC"
}

# ctl_probe is the readiness form: no framing, no files, just the remote exit
# code, so wait_until can poll it. §7.8 makes dnvctl its own readiness probe.
ctl_probe() { # <args…>
	local quoted
	quoted=$(printf '%q ' "$@")
	sshw "$(ctl_prefix) $quoted"
}

fgw_ready() { ctl_probe cluster list >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# Ground truth: the fake's state.json (§7.7)
# ---------------------------------------------------------------------------

# state_count reads one method's call counter out of a state.json snapshot. A
# snapshot that is empty (the file does not exist yet) counts as 0 rather than
# failing, which is what makes a "count unchanged" assertion expressible before
# the first call of a run.
state_count() { # <state json> <Rpc>
	local out
	out=$(printf '%s' "$1" | "$JQ" -r --arg m "$2" '.methods[$m].count // 0')
	printf '%s' "${out:-0}"
}

state_req() { # <state json> <Rpc>
	printf '%s' "$1" | "$JQ" -c --arg m "$2" '.methods[$m].last_request // empty'
}

# assert_count_delta brackets the last ctl_exec with the fake's own counter.
# Both snapshots came back inside that one ssh, so nothing can have slipped in
# between them.
assert_count_delta() { # <Rpc> <delta> <label>
	local before after
	before=$(state_count "$CTL_PRE" "$1")
	after=$(state_count "$CTL_POST" "$1")
	assert_eq "$after" "$((before + $2))" \
		"$3: $1 count moved from $before to $after, want +$2"
}

# assert_req is the sweep's core assertion (§7.10, "field for field"): the
# recorded request must equal the argv-implied one as a WHOLE object. Equality
# rather than a field walk is deliberate — it is the only form that also proves
# the absences the spec names (no ori_name, no sp_rev, no dm_raid0_conf), and
# jq object equality ignores key order, which protojson does not fix.
assert_req() { # <Rpc> <want json>
	local got want
	got=$(state_req "$CTL_POST" "$1")
	[ -n "$got" ] || die "$1: the fake recorded no last_request"
	# The wanted document is compacted for the failure message only: the
	# tables below wrap their JSON across lines for readability, and a
	# mismatch report that echoed that wrapping back would be unreadable
	# next to the one-line document the fake recorded.
	want=$(printf '%s' "$2" | "$JQ" -c .) ||
		die "$1: the expected request is not valid JSON: $2"
	printf '%s' "$got" | "$JQ" -e --argjson want "$2" '. == $want' >/dev/null ||
		die "$1 last_request mismatch:
     got  $got
     want $want"
}

# ---------------------------------------------------------------------------
# Driving the fake: behavior.json and state.json (§7.5)
# ---------------------------------------------------------------------------

# set_behavior writes behavior.json from a here-doc on stdin. The fake re-reads
# the file whenever its mtime OR SIZE changes, so the write must be atomic: a
# half-written file would be parsed, rejected and IGNORED, silently keeping the
# previous behaviour.
set_behavior() { # (JSON on stdin)
	[ "$QUIET" -eq 1 ] || log "[server] behavior -> $(fgw_dir)"
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"cat > $(fgw_dir)/behavior.json.tmp &&
		 mv -f $(fgw_dir)/behavior.json.tmp $(fgw_dir)/behavior.json"
}

clear_behavior() { set_behavior <<<'{}'; }

# reset_state empties the fake's per-method counters. The shape is the fake's
# own stateFile — {"methods": {…}} — not fakeagent's {"objects": …}.
reset_state() {
	ssh "${SSH_OPTS[@]}" "$TARGET" \
		"printf '%s' '{\"methods\":{}}' > $(fgw_dir)/state.json.tmp &&
		 mv -f $(fgw_dir)/state.json.tmp $(fgw_dir)/state.json"
}

truncate_log() {
	sshw "f=$(fgw_log); [ -f \"\$f\" ] && : > \"\$f\"; true"
}

# ---------------------------------------------------------------------------
# Process control (§7.8): launch with >> so an external truncation resets the
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

start_fgw() {
	remote_start fgw fakegateway.log \
		"$WORK/bin/fakegateway --grpc-address 127.0.0.1:$FGW_PORT" \
		"--dir $(fgw_dir)"
}

stop_fgw() {
	[ "${RUNNING[fgw]-}" = 1 ] || return 0
	sig_dir fgw TERM
	wait_gone fgw 10
}

# case_reset is §7.7's per-case reset: stop the fake, blank both JSON files,
# truncate the log, restore the argv levers and start it again. Restarting
# rather than only rewriting state.json is what guarantees a clean counter map
# — the fake is stateless beyond those two files, so this is purely about
# isolating the CASES from each other.
case_reset() { # <case name>
	CASE=$1
	stage reset "per-case reset before case $1 (§7.7)"
	stop_fgw
	clear_behavior
	reset_state
	truncate_log
	ctl_defaults
	SWEEP_TRACES=()
	SWEEP_RPCS=()
	start_fgw
	wait_until "$WAIT_READY" "the fake to answer a cluster list" fgw_ready
}

# ---------------------------------------------------------------------------
# Cleanup (§7.14)
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
pkill -f 'bin/fakegateway' 2>/dev/null || true
pkill -f 'bin/dnvctl' 2>/dev/null || true
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

# verify_clean is §7.14's closing claim, made provable: after a cleanup both
# ports are free and $WORK is gone. It runs on the --cleanup-only path, which
# is the one place nothing else would notice.
verify_clean() {
	local listening port left
	listening=$(sshw "ss -ltnH | awk '{ print \$4 }' | sed 's/.*://' | sort -u")
	for port in "${ALL_PORTS[@]}"; do
		[ "$(printf '%s\n' "$listening" | grep -cx "$port" || true)" = 0 ] ||
			die "port $port is still listening after cleanup"
	done
	left=$(sshw "if [ -e $WORK ]; then echo PRESENT; else echo ABSENT; fi")
	assert_eq "$left" "ABSENT" "\$WORK after cleanup"
	log "  cleanup verified: ${ALL_PORTS[*]} free, $WORK absent"
}

# ---------------------------------------------------------------------------
# Diagnostics (§7.15)
# ---------------------------------------------------------------------------

diagnostics() {
	local saved=$QUIET
	QUIET=1
	log "failing stage: '$STAGE'   trace_id: $TRACE"
	log "cluster: $CLUSTER  sp: $SP  gateway: $CTL_ADDR"

	log ""
	log "--- last 50 fakegateway records (grpc server *, behavior/state file) ---"
	recs "$(fgw_log)" \
		'select((.msg | startswith("grpc server")) or (.msg | test("file")))' |
		tail -n 50 >&2 || true

	log ""
	log "--- $(fgw_dir)/state.json ---"
	rlog "$(fgw_dir)/state.json" >&2 || true

	log ""
	log "--- $(fgw_dir)/behavior.json ---"
	rlog "$(fgw_dir)/behavior.json" >&2 || true

	log ""
	log "--- the last dnvctl invocation (rc, stderr, stdout) ---"
	rlog "$WORK/last.rc" >&2 || true
	rlog "$WORK/last.err" >&2 || true
	rlog "$WORK/last.out" >&2 || true

	log ""
	log "--- ss -ltn (the two ports) ---"
	sshw_ok "ss -ltn | grep -E ':(${ALL_PORTS[0]}$(printf '|%s' "${ALL_PORTS[@]:1}"))\\b' || true" >&2

	log ""
	log "--- ps -ef | grep \$WORK ---"
	sshw_ok "ps -ef | grep -F '$WORK' | grep -v grep || true" >&2
	QUIET=$saved
}

# ---------------------------------------------------------------------------
# Preflight (§7.4)
# ---------------------------------------------------------------------------

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing: $1 on the driver"
}

# resolve_jq picks the driver's JSON parser exactly as the other suites do: a
# system jq if there is one, else the gojq drop-in built into the gitignored
# integtest/bin with the Go toolchain the driver already needs. Every filter in
# this file is written to the intersection of the two.
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

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp awk sed grep; do need_local "$tool"; done
	resolve_jq
	log "  make build (CGO_ENABLED=0 GOOS=linux GOARCH=amd64)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	[ -x "$DNVCTL_BIN" ] || die "missing: $DNVCTL_BIN after make build"
	log "  building integtest/bin/fakegateway"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$FAKEGW_BIN" ./integtest/fakegateway >&2) ||
		die "building fakegateway failed"
	log "preflight (driver) ok"
}

# preflight_server runs AFTER the start-of-run cleanup: the port check is only
# meaningful once a crashed prior run's processes are gone (§7.4). Both ports
# are checked — 29841 is never listened on by this suite, and the transport
# case's UNAVAILABLE is only a real measurement if nothing else answers there.
preflight_server() {
	STAGE="preflight (server)"
	log "=== preflight: server"
	sshw "true" || die "passwordless ssh to $TARGET failed"
	local missing
	missing=$(sshw "for b in bash nohup pkill ss df awk sed; do" \
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
# Setup (§7.8)
# ---------------------------------------------------------------------------

setup() {
	CASE=setup

	stage layout "create the §7.3 tree and ship dnvctl + fakegateway"
	sshw "mkdir -p $WORK/bin $(fgw_dir)"
	SETUP_DONE=1
	log "[server] scp dnvctl fakegateway -> $WORK/bin"
	scp -q "${SSH_OPTS[@]}" "$DNVCTL_BIN" "$FAKEGW_BIN" \
		"$TARGET:$WORK/bin/" || die "scp of the binaries failed"
	sshw "chmod 0755 $WORK/bin/*"

	stage fake "start fakegateway on 127.0.0.1:$FGW_PORT with an empty behavior"
	clear_behavior
	reset_state
	start_fgw
	ctl_defaults
	wait_until "$WAIT_READY" "the fake to answer a cluster list" fgw_ready

	stage started "the fake logged its startup record"
	local started
	started=$(count_recs "$(fgw_log)" 'select(.msg == "fakegateway started")')
	assert_eq "$started" "1" "'fakegateway started' records"
	local addr
	addr=$(recsr "$(fgw_log)" \
		'select(.msg == "fakegateway started") | .grpc_address')
	assert_eq "$addr" "127.0.0.1:$FGW_PORT" "the fake's grpc_address record"
}

# ---------------------------------------------------------------------------
# Case S — smoke (§7.9)
# ---------------------------------------------------------------------------

case_smoke() {
	local s1_err s2_err minted

	# -------------------------------------------------------------------
	stage 1 "cluster list under an explicit --trace-id, observed on the wire"
	# -------------------------------------------------------------------
	ctl_ok cluster list
	s1_err=$CTL_ERR
	assert_count_delta ListClusters 1 "one ListClusters reached the fake"
	assert_eq "$(count_req_recs ListClusters "$TRACE")" "1" \
		"one 'grpc server request' record under trace_id $TRACE"
	# The full method string, pinned once for the whole suite: schema.proto
	# declares no proto package, so the wire name has no package prefix and
	# req_recs' endswith matcher is exact rather than merely convenient.
	local method
	method=$(recsr "$(fgw_log)" \
		'select(.msg == "grpc server request" and .trace_id == $t) | .method' \
		--arg t "$TRACE")
	assert_eq "$method" "/Gateway/ListClusters" "the wire method string"
	# The canned reply is an EMPTY ListClustersReply, so this is §3.1's
	# EmitUnpopulated on a repeated field and on a string: both visible,
	# neither elided as a proto3 default.
	assert_jq "$CTL_OUT" '.cluster_name == [] and .page_token == ""' \
		"the canned ListClusters reply renders its empty fields (§3.1)"

	# -------------------------------------------------------------------
	stage 2 "the same call WITHOUT --trace-id mints one (§2.3)"
	# -------------------------------------------------------------------
	CTL_TRACE=0
	ctl_ok cluster list
	s2_err=$CTL_ERR
	CTL_TRACE=1
	assert_count_delta ListClusters 1 "one ListClusters reached the fake"
	# Every trace id this script hands out starts with `it-`, so the minted
	# one is exactly the record that does not. It has to be there, it has to
	# be alone, and it has to be non-empty: an empty trace_id would mean the
	# §4 client chain attached nothing at all.
	minted=$(recsr "$(fgw_log)" \
		'select(.msg == "grpc server request" and
			(.method | endswith("ListClusters")) and
			((.trace_id | startswith("it-")) | not)) | .trace_id')
	assert_eq "$(printf '%s\n' "$minted" | grep -c . || true)" "1" \
		"exactly one ListClusters request carried a minted trace id"
	assert_ne "$minted" "" "the minted trace id must not be empty"
	log "  minted trace id on the wire: $minted"

	# -------------------------------------------------------------------
	stage 3 "both invocations kept stderr empty (CT7)"
	# -------------------------------------------------------------------
	# ctl_ok already asserted this for each call; restating it here is §7.9
	# s3 as its own step, and it is the assertion that would catch a stray
	# Info record or a print that escaped the §3.1 emit path.
	assert_eq "$s1_err" "" "stderr of the s1 invocation"
	assert_eq "$s2_err" "" "stderr of the s2 invocation"
}

# ---------------------------------------------------------------------------
# Case A — sweep (§7.10)
# ---------------------------------------------------------------------------

# sweep_step is one row of the §7.10 table: run the command, then make the four
# uniform assertions — exit 0, stdout parses (both inside ctl_ok), the request
# equals the argv-implied one field for field, and the fake's counter for that
# RPC moved by exactly one.
sweep_step() { # <nn> <Rpc> <want json> <argv…>
	local nn=$1 rpc=$2 want=$3
	shift 3
	stage "$nn" "$rpc — dnvctl $*"
	ctl_ok "$@"
	assert_count_delta "$rpc" 1 "step $nn"
	assert_req "$rpc" "$want"
	SWEEP_TRACES+=("$TRACE")
	SWEEP_RPCS+=("$rpc")
}

case_sweep() {
	# The three reply injections the §7.10 goldens need. Everything else in
	# the sweep answers with the canned EMPTY reply, which is what makes
	# step 03's golden a statement about dnvctl's rendering alone.
	stage inject "inject the §7.10 golden replies (sp get, the two bitmaps)"
	set_behavior <<EOF
{"methods":{
  "GetStoragePool":{"reply":{"sp_rev":{"sp_name":"$SP","revision":"$SP_GET_REV"}}},
  "GetThinDeviceBitmap":{"reply":{"bitmap":"$BM_A5_B64"}},
  "GetLegBitmap":{"reply":{"bitmap":"$BM_A5A5_B64"}}
}}
EOF

	# --- cluster (§5.1) ------------------------------------------------
	sweep_step 01 CreateCluster \
		'{"cluster_name":"c1"}' \
		cluster create --name c1
	sweep_step 02 DeleteCluster \
		'{"cluster_name":"c1"}' \
		cluster delete --name c1
	sweep_step 03 GetCluster \
		"{\"cluster_name\":\"$CLUSTER\"}" \
		cluster get
	# Golden 1 of 3: the canned empty reply, rendered. It pins
	# EmitUnpopulated (every field visible), the uint64-as-string of
	# cluster_id, null for an absent sub-message, and the sorted key order
	# json.Marshal gives the re-parsed document (§3.1).
	assert_eq "$CTL_OUT" "$GOLD_CLUSTER_GET" "golden: cluster get"
	sweep_step 04 ListClusters \
		'{"count":2,"page_token":"pt0"}' \
		cluster list --count 2 --page-token pt0

	# --- dn (§5.2) -----------------------------------------------------
	sweep_step 05 CreateDiskNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$DN_ADDR\",
		  \"location\":\"$DN_LOCATION\",
		  \"nvme_tr_conf\":{\"tr_type\":\"tcp\",\"adr_fam\":\"ipv4\",
		    \"tr_addr\":\"127.0.0.1\",\"tr_svc_id\":\"4420\"}}" \
		dn create --addr "$DN_ADDR" --location "$DN_LOCATION"
	sweep_step 06 DeleteDiskNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$DN_ADDR\",
		  \"dn_rev\":{\"revision\":\"7\"}}" \
		dn delete --addr "$DN_ADDR" --rev 7
	sweep_step 07 GetDiskNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$DN_ADDR\"}" \
		dn get --addr "$DN_ADDR"
	sweep_step 08 ListDiskNodes \
		"{\"cluster_name\":\"$CLUSTER\",\"count\":8}" \
		dn list --count 8
	sweep_step 09 UpdateDiskNodeDisabled \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$DN_ADDR\",
		  \"dn_rev\":{\"revision\":\"7\"},\"disabled\":true}" \
		dn set-disabled --addr "$DN_ADDR" --disabled --rev 7
	sweep_step 10 InspectDiskNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$DN_ADDR\"}" \
		dn inspect --addr "$DN_ADDR"

	# --- cn (§5.3): the six dn mirrors, at $CN_ADDR / $CN_LOCATION -----
	sweep_step 11 CreateControllerNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$CN_ADDR\",
		  \"location\":\"$CN_LOCATION\",
		  \"nvme_tr_conf\":{\"tr_type\":\"tcp\",\"adr_fam\":\"ipv4\",
		    \"tr_addr\":\"127.0.0.1\",\"tr_svc_id\":\"4420\"}}" \
		cn create --addr "$CN_ADDR" --location "$CN_LOCATION"
	sweep_step 12 DeleteControllerNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$CN_ADDR\",
		  \"cn_rev\":{\"revision\":\"7\"}}" \
		cn delete --addr "$CN_ADDR" --rev 7
	sweep_step 13 GetControllerNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$CN_ADDR\"}" \
		cn get --addr "$CN_ADDR"
	sweep_step 14 ListControllerNodes \
		"{\"cluster_name\":\"$CLUSTER\",\"count\":8}" \
		cn list --count 8
	sweep_step 15 UpdateControllerNodeDisabled \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$CN_ADDR\",
		  \"cn_rev\":{\"revision\":\"7\"},\"disabled\":true}" \
		cn set-disabled --addr "$CN_ADDR" --disabled --rev 7
	sweep_step 16 InspectControllerNode \
		"{\"cluster_name\":\"$CLUSTER\",\"addr_port\":\"$CN_ADDR\"}" \
		cn inspect --addr "$CN_ADDR"

	# --- sp (§5.4) -----------------------------------------------------
	# CreateStoragePool carries no token, so the --rev 7 the §7.10 row types
	# must leave no trace at all; and the always-present redund_md_raid1 is
	# the one dnvctl-side default (§0 #11), with an EMPTY body because
	# --bitmap-chunk-blocks was not given.
	sweep_step 17 CreateStoragePool \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"bdev_conf\":{\"redund_conf\":{\"redund_md_raid1\":{}}},
		  \"cntlid_slot_list\":[0,1],\"cntlr_cnt\":2,\"slice_cnt\":1,
		  \"init_ext_cnt\":\"2\"}" \
		sp create --cntlr-cnt 2 --slice-cnt 1 --init-ext-cnt 2 \
		--slots 0,1 --rev 7
	sweep_step 18 DeleteStoragePool \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"}}" \
		sp delete --rev 7
	sweep_step 19 GetStoragePool \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\"}" \
		sp get
	# Golden 2 of 3: the injected reply carries a revision above 2^53, so
	# this is the assertion that would fail the day §3.1's re-parse started
	# decoding uint64 into a JSON number.
	assert_eq "$CTL_OUT" "$GOLD_SP_GET" "golden: sp get (uint64 as string)"
	sweep_step 20 ListStoragePools \
		"{\"cluster_name\":\"$CLUSTER\",\"count\":4}" \
		sp list --count 4
	sweep_step 21 UpdateStoragePoolCntlidSlotList \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"cntlid_slot_list\":[0,1,2]}" \
		sp set-cntlid-slots --slots 0,1,2 --rev 7
	sweep_step 22 UpdateStoragePoolLevel \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"sp_level\":\"SP_LEVEL_READONLY\"}" \
		sp set-level --level READONLY --rev 7
	sweep_step 23 FindStoragePoolNames \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_id_list\":[\"1\",\"2\"]}" \
		sp find-names --ids 1,2
	sweep_step 24 GrowSlice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"ext_cnt\":\"2\",
		  \"dn_selector\":{\"white_list\":[\"$DN_ADDR\"]}}" \
		sp grow-slice --slice 0 --ext 2 --dn-white "$DN_ADDR" --rev 7
	sweep_step 25 InspectSide \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",\"side_id\":\"5\"}" \
		sp inspect-side --id 5

	# --- cntlr (§5.5) --------------------------------------------------
	sweep_step 26 CreateCntlr \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"cntlid_slot\":1}" \
		cntlr create --slot 1 --rev 7
	sweep_step 27 DeleteCntlr \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"cntlr_id\":\"3\"}" \
		cntlr delete --id 3 --rev 7
	# --enabled=false, the `=` spelling of §5.0: `--enabled false` would
	# leave `false` as a positional and cobra.NoArgs would reject it.
	sweep_step 28 UpdateCntlrEnabled \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"cntlr_id\":\"3\"}" \
		cntlr set-enabled --id 3 --enabled=false --rev 7
	sweep_step 29 InspectCntlr \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",\"cntlr_id\":\"3\"}" \
		cntlr inspect --id 3

	# --- td (§5.6) -----------------------------------------------------
	sweep_step 30 CreateThinDevice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"td_name\":\"t0\",
		  \"size\":\"$TD_SIZE\"}" \
		td create --name t0 --size "$TD_SIZE" --rev 7
	sweep_step 31 DeleteThinDevice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"td_name\":\"t0\"}" \
		td delete --name t0 --rev 7
	sweep_step 32 ListThinDevices \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\"}" \
		td list
	sweep_step 33 GetThinDeviceBitmap \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"td_name\":\"t0\",\"block_cnt\":\"64\"}" \
		td get-bm --name t0 --slice-idx 0 --start 0 --cnt 64
	# Golden 3 of 3: §3.1's one deviation. The fake was told to answer with
	# the single byte 0xa5 and dnvctl must print it as hex, not base64.
	assert_eq "$CTL_OUT" "$GOLD_TD_BM" "golden: td get-bm hex map"
	sweep_step 34 GetLegBitmap \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"leg_id\":\"9\",\"block_cnt\":\"64\"}" \
		td get-leg-bm --leg 9 --start 0 --cnt 64
	assert_eq "$CTL_OUT" "$GOLD_LEG_BM" "golden: td get-leg-bm hex map"

	# --- ss (§5.7) -----------------------------------------------------
	sweep_step 35 CreateSubsystem \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",
		  \"allowed_hosts\":[\"$HOST0\"]}" \
		ss create --nqn "$NQN" --hosts "$HOST0" --rev 7
	sweep_step 36 DeleteSubsystem \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\"}" \
		ss delete --nqn "$NQN" --rev 7
	sweep_step 37 ListSubsystems \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\"}" \
		ss list
	sweep_step 38 UpdateSubsystemHosts \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",
		  \"allowed_hosts\":[\"a\",\"b\"]}" \
		ss set-hosts --nqn "$NQN" --hosts a,b --rev 7

	# --- ns (§5.8) -----------------------------------------------------
	sweep_step 39 CreateNamespace \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",\"ns_idx\":1,
		  \"dev_uuid\":\"$UUID\",\"dev_nguid\":\"$NGUID\",
		  \"td_name\":\"t0\"}" \
		ns create --nqn "$NQN" --idx 1 --td t0 \
		--uuid "$UUID" --nguid "$NGUID" --rev 7
	sweep_step 40 DeleteNamespace \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",\"ns_idx\":1}" \
		ns delete --nqn "$NQN" --idx 1 --rev 7
	sweep_step 41 UpdateNamespaceDev \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",\"ns_idx\":1,
		  \"td_name\":\"t1\"}" \
		ns set-dev --nqn "$NQN" --idx 1 --td t1 --rev 7
	# The flag's default is TRUE here (§5.8), so the bare form must send
	# suspended = true — the one place in the CLI where an unmentioned
	# boolean is not false.
	sweep_step 42 UpdateNamespaceSuspended \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"nqn\":\"$NQN\",\"ns_idx\":1,
		  \"suspended\":true}" \
		ns set-suspended --nqn "$NQN" --idx 1 --rev 7

	# --- clone (§5.9) --------------------------------------------------
	sweep_step 43 CreateClone \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"clone_name\":\"cl0\",
		  \"src_tr_conf\":[{\"tr_type\":\"tcp\",\"adr_fam\":\"ipv4\",
		    \"tr_addr\":\"127.0.0.1\",\"tr_svc_id\":\"4420\"}],
		  \"src_nqn\":\"$SRC_NQN\",\"src_slice_cnt\":1,
		  \"src_stripe_size\":\"16384\",\"src_block_size\":\"1048576\",
		  \"dst_td_name\":\"t1\"}" \
		clone create --name cl0 --dst-td t1 --src-nqn "$SRC_NQN" \
		--src-idx 0 --src-slices 1 --src-stripe 16384 \
		--src-block 1048576 --rev 7
	sweep_step 44 DeleteClone \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"clone_name\":\"cl0\",
		  \"force\":true}" \
		clone delete --name cl0 --force --rev 7
	sweep_step 45 GetClone \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"clone_name\":\"cl0\"}" \
		clone get --name cl0
	sweep_step 46 UpdateCloneTrConf \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"clone_name\":\"cl0\",
		  \"src_tr_conf\":[{\"tr_type\":\"tcp\",\"adr_fam\":\"ipv4\",
		    \"tr_addr\":\"127.0.0.1\",\"tr_svc_id\":\"4420\"}]}" \
		clone set-tr --name cl0 --src-tr-addr 127.0.0.1 --rev 7
	sweep_step 47 AppendCloneBitmap \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"clone_name\":\"cl0\",
		  \"bitmap\":\"$BM_A5_B64\"}" \
		clone append-bm --name cl0 --slice-idx 0 --bm-hex a5 --rev 7

	# --- xfer (§5.10) --------------------------------------------------
	sweep_step 48 CreateTransfer \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"xfer_name\":\"x0\",
		  \"ori_nqn\":\"$NQN\",\"ori_ns_idx\":1,
		  \"allowed_hosts\":[\"$HOST0\"],\"auto_suspend\":true}" \
		xfer create --name x0 --ori-nqn "$NQN" --ori-idx 1 \
		--hosts "$HOST0" --auto-suspend --rev 7
	sweep_step 49 DeleteTransfer \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"xfer_name\":\"x0\"}" \
		xfer delete --name x0 --rev 7
	sweep_step 50 GetTransfer \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"xfer_name\":\"x0\"}" \
		xfer get --name x0
	sweep_step 51 UpdateTransferHosts \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"xfer_name\":\"x0\",
		  \"allowed_hosts\":[\"c\"]}" \
		xfer set-hosts --name x0 --hosts c --rev 7

	# --- migr (§5.11) --------------------------------------------------
	sweep_step 52 CreateMigration \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"migr_name\":\"m0\",
		  \"src_side_id\":\"5\",
		  \"dm_clone_conf\":{\"hydration_threshold\":8,
		    \"hydration_batch_size\":4}}" \
		migr create --name m0 --src-side 5 \
		--hyd-threshold 8 --hyd-batch 4 --rev 7
	sweep_step 53 FinishMigration \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"migr_name\":\"m0\"}" \
		migr finish --name m0 --rev 7
	sweep_step 54 CancelMigration \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"migr_name\":\"m0\"}" \
		migr cancel --name m0 --rev 7
	sweep_step 55 GetMigration \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"migr_name\":\"m0\"}" \
		migr get --name m0
	sweep_step 56 AppendMigrationBitmap \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"migr_name\":\"m0\",
		  \"bitmap\":\"$BM_A5A5_B64\"}" \
		migr append-bm --name m0 --bm-hex a5a5 --rev 7

	# --- spare (§5.12) -------------------------------------------------
	sweep_step 57 CreateSpareLeg \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"grp_id\":\"1\"}" \
		spare create --grp 1 --rev 7
	sweep_step 58 DeleteSpareLeg \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"grp_id\":\"1\",\"leg_id\":\"2\"}" \
		spare delete --grp 1 --leg 2 --rev 7
	sweep_step 59 SwitchSpareLeg \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"sp_rev\":{\"revision\":\"7\"},\"grp_id\":\"1\",
		  \"spare_leg_id\":\"6\",\"target_leg_id\":\"4\"}" \
		spare switch --grp 1 --spare 6 --target 4 --rev 7

	# -------------------------------------------------------------------
	stage audit "all 59 steps left one request record under their trace id"
	# -------------------------------------------------------------------
	# CT1's completeness claim, made on the WIRE rather than from a table:
	# 59 distinct RPCs, 59 distinct trace ids, one interceptor record each.
	# One log read serves all 59 assertions.
	assert_eq "${#SWEEP_TRACES[@]}" "59" "the sweep ran 59 steps"
	local records idx want
	records=$(recsr "$(fgw_log)" \
		'select(.msg == "grpc server request") | .trace_id + " " + .method')
	for idx in "${!SWEEP_TRACES[@]}"; do
		want="${SWEEP_TRACES[$idx]} /Gateway/${SWEEP_RPCS[$idx]}"
		assert_eq "$(printf '%s\n' "$records" | grep -cxF "$want" || true)" "1" \
			"exactly one request record for '$want'"
	done
	local distinct
	distinct=$(printf '%s\n' "${SWEEP_RPCS[@]}" | sort -u | grep -c . || true)
	assert_eq "$distinct" "59" "the 59 steps drove 59 DISTINCT RPCs"
}

# ---------------------------------------------------------------------------
# Case B — behavior (§7.11)
# ---------------------------------------------------------------------------

case_behavior() {
	# -------------------------------------------------------------------
	stage 1 "td create with NO --rev: the token message must be ABSENT"
	# -------------------------------------------------------------------
	# §4's first presence case. The fake records requests with protojson
	# WITHOUT EmitUnpopulated, so "absent message" and "present but empty"
	# are genuinely different documents here, which is the whole reason the
	# assertion is made against state.json rather than against a reply.
	ctl_ok td create --name t0
	assert_req CreateThinDevice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",\"td_name\":\"t0\"}"
	assert_jq "$(state_req "$CTL_POST" CreateThinDevice)" \
		'(has("sp_rev") | not)' "no --rev must leave no sp_rev key at all"

	# -------------------------------------------------------------------
	stage 2 "td create --rev 0: PRESENT message, revision 0 (the stale probe)"
	# -------------------------------------------------------------------
	ctl_ok td create --name t0 --rev 0
	assert_req CreateThinDevice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",\"td_name\":\"t0\",
		  \"sp_rev\":{}}"
	assert_jq "$(state_req "$CTL_POST" CreateThinDevice)" \
		'has("sp_rev") and (.sp_rev == {})' \
		"--rev 0 must render as a present, empty sp_rev"

	# -------------------------------------------------------------------
	stage 3 "td create --rev 0x1f: base-0 parse, revision 31"
	# -------------------------------------------------------------------
	ctl_ok td create --name t0 --rev 0x1f
	assert_req CreateThinDevice \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",\"td_name\":\"t0\",
		  \"sp_rev\":{\"revision\":\"31\"}}"
	# Spelled out separately because it is the assertion that breaks if the
	# revision ever stops being a uint64-as-string (§7.7).
	assert_field "$(state_req "$CTL_POST" CreateThinDevice)" \
		'.sp_rev.revision' "31" "0x1f parsed base-0 into revision 31"

	# -------------------------------------------------------------------
	stage 4 "sp create --redund none: the other arm of the oneof"
	# -------------------------------------------------------------------
	ctl_ok sp create --redund none --rev 7
	assert_req CreateStoragePool \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\",
		  \"bdev_conf\":{\"redund_conf\":{\"redund_none\":{}}}}"
	assert_jq "$(state_req "$CTL_POST" CreateStoragePool)" \
		'(.bdev_conf.redund_conf | has("redund_md_raid1") | not)' \
		"--redund none must not also carry the raid1 arm"

	# -------------------------------------------------------------------
	stage 5 'td list renders "created" for both values (EmitUnpopulated)'
	# -------------------------------------------------------------------
	# ListThinDevicesReply carries a map<string, ThinDevice> named
	# name_to_td (pb/schema.proto), so the injected reply and the assertion
	# below address the tds by name.
	set_behavior <<'EOF'
{"methods":{"ListThinDevices":{"reply":{"name_to_td":{
  "t0":{"td_id":"7","dev_id":1,"size":"67108864","created":true},
  "t1":{"td_id":"8","dev_id":2,"size":"67108864","created":false}}}}}}
EOF
	ctl_ok td list
	assert_jq "$CTL_OUT" '.name_to_td.t0.created == true' \
		"the injected created:true td renders true"
	# The R13 poll's real requirement: a NOT-yet-created td must still show
	# the key, because proto3 would otherwise elide the false and the poll
	# could not tell "false" from "an older gateway that has no such field".
	assert_jq "$CTL_OUT" '(.name_to_td.t1 | has("created"))' \
		"a created:false td still renders the key (EmitUnpopulated)"
	assert_jq "$CTL_OUT" '.name_to_td.t1.created == false' \
		"and it renders as false"
	clear_behavior

	# -------------------------------------------------------------------
	stage 6 "DNVCTL_CLUSTER fills --cluster, and the flag still wins (CT9)"
	# -------------------------------------------------------------------
	CTL_ENV="DNVCTL_CLUSTER=envclu "
	CTL_CLUSTER=0
	ctl_ok cluster get
	assert_req GetCluster '{"cluster_name":"envclu"}'
	# Flag > env, viper's standard order. Same environment, --cluster back.
	CTL_CLUSTER=1
	ctl_ok cluster get
	assert_req GetCluster "{\"cluster_name\":\"$CLUSTER\"}"
	ctl_defaults

	# -------------------------------------------------------------------
	stage 7 "cluster get --name wins over the global --cluster"
	# -------------------------------------------------------------------
	# The fallback direction (empty --name ⇒ the global) is sweep step 03.
	ctl_ok cluster get --name other
	assert_req GetCluster '{"cluster_name":"other"}'
}

# ---------------------------------------------------------------------------
# Case C — errors (§7.12)
# ---------------------------------------------------------------------------

case_errors() {
	# One behavior file for the whole case: four methods, four codes, and
	# the one custom message an operator will actually meet.
	stage inject "inject the four §7.12 refusals"
	set_behavior <<'EOF'
{"methods":{
  "GetStoragePool":{"code":"NOT_FOUND"},
  "DeleteThinDevice":{"code":"ABORTED","message":"stale revision"},
  "CreateCluster":{"code":"ALREADY_EXISTS"},
  "CreateStoragePool":{"code":"INVALID_ARGUMENT"}}}
EOF

	# -------------------------------------------------------------------
	stage 1 "sp get -> NOT_FOUND"
	# -------------------------------------------------------------------
	# The fake's message default is "behavior.json <CODE>" (§7.5), so the
	# whole §3.2 line is predictable and compared byte for byte.
	ctl_fail_msg NOT_FOUND "behavior.json NOT_FOUND" sp get
	# A refused call is still recorded: §7.5 records BEFORE it applies any
	# behaviour, which is what lets an error case assert the request too.
	assert_count_delta GetStoragePool 1 "a refusal is still recorded"
	assert_req GetStoragePool \
		"{\"cluster_name\":\"$CLUSTER\",\"sp_name\":\"$SP\"}"

	# -------------------------------------------------------------------
	stage 2 "td delete --rev 7 -> ABORTED 'stale revision'"
	# -------------------------------------------------------------------
	# The §4 failure an operator meets when a token goes stale under them.
	ctl_fail_msg ABORTED "stale revision" td delete --name t0 --rev 7
	assert_count_delta DeleteThinDevice 1 "the refused delete was recorded"

	# -------------------------------------------------------------------
	stage 3 "cluster create -> ALREADY_EXISTS"
	# -------------------------------------------------------------------
	ctl_fail_msg ALREADY_EXISTS "behavior.json ALREADY_EXISTS" \
		cluster create --name c1
	assert_count_delta CreateCluster 1 "the refused create was recorded"

	# -------------------------------------------------------------------
	stage 4 "sp create -> INVALID_ARGUMENT"
	# -------------------------------------------------------------------
	ctl_fail_msg INVALID_ARGUMENT "behavior.json INVALID_ARGUMENT" \
		sp create --rev 7
	assert_count_delta CreateStoragePool 1 "the refused create was recorded"

	# -------------------------------------------------------------------
	stage 5 "an unknown flag is exit 2 and issues NO RPC"
	# -------------------------------------------------------------------
	ctl_usage CreateThinDevice td create --no-such-flag
	assert_ne "$(printf '%s\n' "$CTL_ERR" | grep -c 'no-such-flag' || true)" \
		"0" "cobra must name the unknown flag: got '$CTL_ERR'"

	# -------------------------------------------------------------------
	stage 6 "a malformed --bm-hex is exit 2 and issues NO RPC"
	# -------------------------------------------------------------------
	# CT8's dividing line: dnvctl rejects only what fails to PARSE, and it
	# does so before the dial, so the gateway never sees it. An EMPTY
	# --bm-hex is a different thing entirely and IS sent (§5.9).
	ctl_usage AppendCloneBitmap \
		clone append-bm --name cl0 --bm-hex zz --rev 7
	assert_ne "$(printf '%s\n' "$CTL_ERR" | grep -c 'bm-hex' || true)" \
		"0" "the parse error must name --bm-hex: got '$CTL_ERR'"
}

# ---------------------------------------------------------------------------
# Case D — transport (§7.13)
# ---------------------------------------------------------------------------

case_transport() {
	local started elapsed

	# -------------------------------------------------------------------
	stage 1 "a closed port is UNAVAILABLE, and fails fast"
	# -------------------------------------------------------------------
	# $DEAD_PORT is the port preflight proved free and nothing ever binds.
	# The message is gRPC's own ("connection refused"), so only the §3.2
	# shape is asserted here — ctl_fail's regexp.
	CTL_ADDR="127.0.0.1:$DEAD_PORT"
	started=$SECONDS
	ctl_fail UNAVAILABLE cluster list
	elapsed=$((SECONDS - started))
	ctl_defaults
	# "Well inside the 10 s default": a refusal must come back because the
	# connection was refused, not because the deadline expired — and those
	# two are only distinguishable by the clock.
	assert_between "$elapsed" 0 5 "a refused dial must fail fast (seconds)"
	log "  refused dial took ${elapsed}s"

	# -------------------------------------------------------------------
	stage 2 "a listening-but-silent gateway is DEADLINE_EXCEEDED"
	# -------------------------------------------------------------------
	# The hang lever is what distinguishes this from step 1: the port IS
	# open, the request IS recorded, and only the reply never comes.
	set_behavior <<'EOF'
{"methods":{"ListClusters":{"hang":true}}}
EOF
	started=$SECONDS
	ctl_fail DEADLINE_EXCEEDED --timeout 2 cluster list
	elapsed=$((SECONDS - started))
	assert_count_delta ListClusters 1 "the hung call was still recorded"
	# [2 s, 5 s): it must not return early — that would mean --timeout was
	# not the thing that ended it — and it must not run long.
	assert_between "$elapsed" 2 4 "--timeout 2 must end the call (seconds)"
	log "  hung call returned after ${elapsed}s"

	# Clearing the lever must bring the same command back to life, which is
	# what proves the fake (and the connection path) was never broken.
	stage 3 "clearing the hang lever restores the command"
	clear_behavior
	ctl_ok cluster list
	assert_count_delta ListClusters 1 "the recovered call reached the fake"
	assert_parses "$CTL_OUT" "the recovered cluster list"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/dnvctl_test.sh [--only <case>] [--cleanup-only] user@ip

  --only <case>   run one of: ${CASES[*]}
  --cleanup-only  run the §7.14 cleanup on the server and exit
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
		verify_clean
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
