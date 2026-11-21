#!/usr/bin/env bash
#
# size_step.sh — a lab tool, not a suite: it steps one real disk node and one
# real controller node toward the agents' limits (architecture.md,
# Cardinality limits) and prints what each step cost. No suite calls it, and
# it gives no pass or fail: it exits non-zero only when its own setup fails.
#
#   bash integtest/size_step.sh [--sides "64 128 256 512 1024"] \
#       [--cntlrs "16 64 128 256"] [--log-dir <dir>] [--cleanup-only] \
#       <user@dn-guest> <user@cn-guest>
#
# The disk node step: one dn agent over a sparse file on a loop device with
# room for the most sides a disk node holds, each side one extent, shaped as a
# RedundNone data group's (its meta block and first data block zeroed), with
# the cn agent's id as its primary, so that each also exports to it. For each
# count in --sides, the side list grows to that count by one SyncupDn with the
# whole list, then each new side goes through the provisioning protocol
# (architecture.md, Side provisioning protocol): a SyncupSide with the side
# not provisioned, the wait for its zeroing, and a SyncupSide that flips it;
# then one fresh CheckDn round. Once the last count is done the side list is
# emptied, which the agent's sweep tears down (architecture.md, Teardown by
# sweep).
#
# The controller node step: one cn agent on the other guest, with its own
# nvmet port. For each count in --cntlrs, the cntlrs grow to that count: each
# new cntlr is the cn suite's S shape (cnagent_integtest.md, Topology), one
# slice whose RedundNone meta and data groups each have one one-extent side on
# the disk node, one thin device and one host-facing subsystem with one
# namespace. Its two sides are provisioned first, then one SyncupCn with the
# whole cntlr list, then one SyncupCntlr per new cntlr, then one fresh CheckCn
# round and one fresh CheckCntlr round of the last cntlr it sent. Both
# agents run with one IO queue per nvmet subsystem (QID_MAX), no host
# connects to any subsystem and no IO runs, so the step's numbers hold under
# those three conditions only. A fresh round's time has a floor of about
# two of the driver's rounds, 0.2 s (cnagentctl's fresh mode), so a time
# near 0.2 s says only that the node's read took less.
#
# After each step it reads, with a fresh round, the rows of a sample of the
# step's new objects, the first, the middle and the last: each side's
# (CheckSide) or each cntlr's (CheckCntlr), and a sample with a row neither
# OK nor pending, one still pending after SAMPLE_SECS, or with no row, is a
# failed step, as an agent reports a failed build as a row under reply code
# zero; a new leg's health row reads pending until its prober's first
# round. It reads both guests' available memory and free disk before each
# step and after every ROOM_EVERY new objects within one.
# It stops stepping, without failing, at the first call that fails, or that
# answers a code other than zero where a cntlr's SyncupCntlr may also answer
# the leftover code and is then re-sent as the worker re-sends it
# (dnv-worker.md RW4), at the first failed sample, or when a guest's
# available memory falls under MIN_MEM_MIB or its free disk under
# MIN_DISK_GIB; a check round's code is recorded, not judged. Each step
# prints one "SIZE …" line with its times, the check rounds' codes, the
# sample, the dm devices and fabrics controllers the guests hold and their
# room after it; a header line gives each guest's CPU count, memory and
# kernel.
#
# It touches a guest only once both have passed its guard: a guest that
# runs a dnv-agent, holds a dnv dm device, nvmet subsystem or fabrics
# controller, or holds the tool's work directory is refused, and the tool
# then exits having changed nothing on either. Run it only on guests no
# other run holds. Once the guard has passed, everything of dnv on both
# guests goes at the end and on any exit, which the guard makes what the
# tool made: the agents, every dnv dm device, nvmet object and fabrics
# controller, the loop device and its file, the udev rules it installed and
# the shipped files; the agents' logs are first copied into --log-dir. The
# uutils dd rule of the agent suites holds: this tool runs no dd at all.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
AGENT_BIN="$REPO_ROOT/bin/dnv-agent"
DNCTL="$REPO_ROOT/integtest/bin/dnagentctl"
CNCTL="$REPO_ROOT/integtest/bin/cnagentctl"
UDEV_SRC="$REPO_ROOT/deploy/udev"

WORK=/var/tmp/dnv-size-step
NVMET=/sys/kernel/config/nvmet
UDEV_DIR=/etc/udev/rules.d
NQN_PREFIX=nqn.2024-01.io.dnv
HOST_NQN=nqn.2024-01.io.dnv-size:host:0

CLUSTER=0x1
DN_ID=0x1
CN_ID=0x11
DN_GRPC_PORT=29528
CN_GRPC_PORT=29529
TR_SVC_ID=4200
# The agents' IO queue cap per nvmet subsystem: one queue beside the admin
# queue keeps the step's connections to two per leg.
QID_MAX=1
CN_CAPACITY=1099511627776

# The disk: the fixed prefix (common.DnDataOffset) and room for the most
# sides a disk node holds (common.MaxSideCntPerDn), one extent each.
EXTENT_SIZE=67108864
DN_PREFIX=268435456
MAX_SIDES=1024
DISK_SIZE=$((DN_PREFIX + MAX_SIDES * EXTENT_SIZE))

# The S shape's geometry at a 1 MiB block (architecture.md, Group on-leg
# layout: meta region, data region, health block), as the cn suite works it:
# a one-extent group holds one meta block and 63 data blocks; a meta group's
# side zeroes its whole span, a data group's its meta block and first data
# block.
BLOCK_SIZE=1048576
STRIPE_SIZE=65536
META_BLOCKS=1
DATA_BLOCKS=63
MGRP_ZERO=$(((META_BLOCKS + DATA_BLOCKS) * BLOCK_SIZE))
DGRP_ZERO=$(((META_BLOCKS + 1) * BLOCK_SIZE))
TD_SIZE=67108864

MIN_MEM_MIB=${MIN_MEM_MIB:-300}
MIN_DISK_GIB=5
ROOM_EVERY=32
SAMPLE_SECS=60
RPC_TIMEOUT=120

SIDES="64 128 256 512 1024"
CNTLRS="16 64 128 256"
CLEANUP_ONLY=0
VM=("" "" "")
IP=("" "" "")
REV=0
CNREV=0
STOPPED=""
TRACE=it-size-step
UDEV_SRC_NAMES=""
# The udev rules each guest lacked before the tool installed them, the only
# ones its cleanup takes away.
UDEV_MINE=("" "" "")
WORK_LOCAL=""
LOG_DIR=""
# 1 once both guests passed the guard: only then does an exit clean them.
ARMED=0
# Each object's last revision, which its check round names: a side's
# SyncupSide, a cntlr's SyncupCntlr.
declare -A SIDE_REV CNTLR_REV
JQ=jq
# The revisions the last SyncupDn and SyncupCn stored, which their check
# rounds name (dnv-worker.md RW13, RW14).
DN_SYNC=0
CN_SYNC=0
# The last step's sample of rows, as check_sample read it.
SAMPLE=-

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

log() { echo "$*" >&2; }
die() {
	log "size_step: $*"
	exit 1
}

# sshv runs one command as root on guest 1 (the disk node) or 2 (the
# controller node).
sshv() {
	local idx=$1
	shift
	ssh "${SSH_OPTS[@]}" "${VM[$idx]}" "sudo bash -c $(printf '%q' "$*")"
}

hex16() { printf '%016x' "$(($1))"; }

# resolve_jq picks the driver's JSON parser as the agent suites do: a system
# jq, else gojq built into integtest/bin.
resolve_jq() {
	if command -v jq >/dev/null 2>&1; then
		JQ=jq
		return
	fi
	JQ="$REPO_ROOT/integtest/bin/gojq"
	[ -x "$JQ" ] || GOBIN="$REPO_ROOT/integtest/bin" GOFLAGS=-mod=mod \
		go install github.com/itchyny/gojq/cmd/gojq@v0.12.17 >&2 ||
		die "no jq on the driver, and building gojq failed"
}

# code_of <reply> — the reply's AgentReply code, "none" for no reply.
code_of() {
	if [ -z "$1" ]; then
		echo none
	else
		printf '%s' "$1" | "$JQ" -r '.agent_reply.code // 0'
	fi
}
d16() { printf '%d' "$(($1))"; }
now() { date +%s.%N; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", b - a }'; }

# quiet <command…> — runs a command with its output dropped, and logs what
# it printed on stderr only when it fails.
quiet() {
	local err
	err=$("$@" 2>&1 >/dev/null) || {
		log "$err"
		return 1
	}
}

# dm_cnt <idx> — the dnv dm devices of a guest.
dm_cnt() { sshv "$1" "dmsetup ls 2>/dev/null | grep -c '^dnv' || true"; }

dnctl() {
	local sub=$1
	shift
	"$DNCTL" "$sub" --addr "${IP[1]}:$DN_GRPC_PORT" --cluster "$CLUSTER" \
		--dn "$DN_ID" --trace-id "$TRACE" "$@"
}

cnctl() {
	local sub=$1
	shift
	"$CNCTL" "$sub" --addr "${IP[2]}:$CN_GRPC_PORT" --cluster "$CLUSTER" \
		--cn "$CN_ID" --trace-id "$TRACE" "$@"
}

# room <idx> — "<available memory MiB> <free disk GiB of /var/tmp>" of a guest.
room() {
	sshv "$1" "free -m | awk '/^Mem:/ {printf \"%s \", \$7}'; df -BG --output=avail /var/tmp | tail -1 | tr -d ' G'"
}

# room_ok <where> — whether both guests still have room; when one does not,
# the reason is logged and recorded as the reason the stepping stopped.
room_ok() {
	local why
	why=$(low_room)
	[ -n "$why" ] || return 0
	log "size_step: stop $1: $why"
	STOPPED=${STOPPED:-$why}
	return 1
}

# rows_of <reply> <info field> — "<rows> <rows not OK> <rows neither OK
# nor pending> <first not OK>" of one check reply: every row under the
# field, as the agents report each object they build.
rows_of() {
	printf '%s' "$1" | "$JQ" -r --arg f "$2" '
	    [(.[$f] // {}) | .. | objects | select(has("status")) | .status] as $all
	    | [(.[$f] // {}) | .. | objects | select(has("status"))
	        | select(.status != "RES_STATUS_OK")] as $bad
	    | [$bad[] | select(.status != "RES_STATUS_PENDING")] as $hard
	    | "\($all | length) \($bad | length) \($hard | length) \(($hard + $bad)[0] | if . == null then "-" else "\(.status) \(.details // "")" end)"'
}

# sample_of <from> <to> — the first, the middle and the last of the indexes
# from..to-1, each once.
sample_of() {
	printf '%s\n' "$1" "$((($1 + $2 - 1) / 2))" "$(($2 - 1))" | sort -nu
}

# check_sample <kind> <from> <to> — the fresh rows of a sample of the step's
# new sides ("side") or cntlrs ("cntlr"); sets SAMPLE to "<objects
# read>/<rows>" and records a failed step on a sample with no row, a row
# neither OK nor pending, a row still pending after SAMPLE_SECS (a new
# leg's health row reads pending until its prober's first round) or no
# reply.
check_sample() {
	local kind=$1 i out r rows=0 n=0 sp deadline
	for i in $(sample_of "$2" "$3"); do
		deadline=$((SECONDS + SAMPLE_SECS))
		while :; do
			case "$kind" in
			side)
				sp=$((0x1000 + i))
				out=$(dnctl check-side --fresh --show-info --timeout "$RPC_TIMEOUT" \
					--revision "${SIDE_REV["$sp:0x1:0x2"]}" --sp "$sp" --leg 0x1 \
					--side 0x2 2>/dev/null || true)
				;;
			cntlr)
				sp=$((0x2000 + i))
				out=$(cnctl check-cntlr --fresh --show-info --timeout "$RPC_TIMEOUT" \
					--revision "${CNTLR_REV[$i]}" --sp "$sp" --cntlr 0x1 \
					2>/dev/null || true)
				;;
			esac
			r=""
			[ -z "$out" ] || r=$(rows_of "$out" "${kind}_info")
			# Only rows that are all OK or pending, some pending, are read again.
			[ -n "$r" ] && [ "${r%% *}" -ne 0 ] &&
				[ "$(printf '%s' "$r" | cut -d' ' -f3)" -eq 0 ] &&
				[ "$(printf '%s' "$r" | cut -d' ' -f2)" -ne 0 ] &&
				[ "$SECONDS" -lt "$deadline" ] || break
			sleep 1
		done
		n=$((n + 1))
		if [ -z "$r" ] || [ "${r%% *}" -eq 0 ] || [ "$(printf '%s' "$r" | cut -d' ' -f2)" -ne 0 ]; then
			failed "$kind $i's rows: ${r:-no reply}"
			continue
		fi
		rows=$((rows + ${r%% *}))
	done
	SAMPLE="$n/$rows"
}

# low_room — the reason to stop stepping, or nothing.
low_room() {
	local idx r mem disk
	for idx in 1 2; do
		r=$(room "$idx") || {
			echo "guest $idx's room did not read"
			return 0
		}
		read -r mem disk <<<"$r"
		[ "$mem" -ge "$MIN_MEM_MIB" ] ||
			echo "guest $idx has $mem MiB of memory available"
		[ "$disk" -ge "$MIN_DISK_GIB" ] ||
			echo "guest $idx has $disk GiB free"
	done
	return 0
}

# failed <what> — records the first failed call, which ends the stepping.
failed() {
	[ -n "$STOPPED" ] || STOPPED="$1"
	log "size_step: FAILED $1"
}

# cleanup_guest <idx> — takes away everything of dnv on one guest, best
# effort, the holders before what they hold: the agents, nvmet's port links,
# namespaces, subsystems, hosts and ports, then the dm devices by attrition
# (each round removes what nothing holds any more), the fabrics controllers of
# dnv, the dm devices again, the loop device and its file; then the agents'
# logs are copied into LOG_DIR, and the udev rules the tool installed and
# the work directory go, and the root file system is trimmed. Every
# suspended dnv device is resumed first, as a read of one wedges.
cleanup_guest() {
	sshv "$1" "
pkill -x dnv-agent >/dev/null 2>&1
for i in \$(seq 1 40); do pgrep -x dnv-agent >/dev/null || break; sleep 0.25; done
pkill -9 -x dnv-agent >/dev/null 2>&1
for row in \$(dmsetup info -c --noheadings -o name,attr 2>/dev/null | grep -E '^dnv.*:..s'); do
	timeout 10 dmsetup resume \"\${row%%:*}\" >/dev/null 2>&1
done
if [ -d $NVMET ]; then
	for l in $NVMET/ports/*/subsystems/$NQN_PREFIX*; do [ -e \"\$l\" ] && rm -f \"\$l\"; done
	for s in $NVMET/subsystems/$NQN_PREFIX*; do
		[ -d \"\$s\" ] || continue
		for n in \"\$s\"/namespaces/*; do
			[ -d \"\$n\" ] || continue
			echo 0 >\"\$n/enable\" 2>/dev/null; rmdir \"\$n\" 2>/dev/null
		done
		for h in \"\$s\"/allowed_hosts/*; do [ -e \"\$h\" ] && rm -f \"\$h\"; done
		rmdir \"\$s\" 2>/dev/null
	done
	for h in $NVMET/hosts/$NQN_PREFIX*; do [ -d \"\$h\" ] && rmdir \"\$h\" 2>/dev/null; done
	for g in $NVMET/ports/1/ana_groups/[0-9]*; do
		[ \"\${g##*/}\" = 1 ] || rmdir \"\$g\" 2>/dev/null
	done
	rmdir $NVMET/ports/1 2>/dev/null
fi
sweep() {
	local prev=-1 names cnt i n
	for i in \$(seq 1 30); do
		names=\$(dmsetup ls 2>/dev/null | awk '\$1 ~ /^dnv/ {print \$1}')
		[ -n \"\$names\" ] || return 0
		cnt=\$(printf '%s\n' \"\$names\" | wc -l)
		for n in \$names; do
			if [ \"\$cnt\" -eq \"\$prev\" ]; then
				timeout 15 dmsetup remove --force --retry \"\$n\" >/dev/null 2>&1
			else
				timeout 10 dmsetup remove \"\$n\" >/dev/null 2>&1
			fi
		done
		prev=\$cnt
	done
}
sweep
for c in /sys/class/nvme/nvme*; do
	[ -r \"\$c/subsysnqn\" ] || continue
	case \"\$(cat \"\$c/subsysnqn\" 2>/dev/null)\" in
	$NQN_PREFIX*) timeout 30 nvme disconnect -d \"\${c##*/}\" >/dev/null 2>&1 ;;
	esac
done
sweep
for l in \$(losetup -j $WORK/disk.img 2>/dev/null | cut -d: -f1); do losetup -d \"\$l\"; done
" || true
	# The agents' logs, the record of what they did, before the work
	# directory goes.
	if [ -n "$LOG_DIR" ]; then
		mkdir -p "$LOG_DIR/guest$1" &&
			scp -q "${SSH_OPTS[@]}" "${VM[$1]}:$WORK/*.log" "$LOG_DIR/guest$1/" \
				2>/dev/null || log "size_step: guest $1: no agent log copied"
	fi
	sshv "$1" "
for f in ${UDEV_MINE[$1]}; do rm -f $UDEV_DIR/\$f; done
udevadm control --reload >/dev/null 2>&1
rm -rf $WORK
# ext4 frees a removed file's blocks at its next journal commit, so the trim
# waits for one.
sync; sleep 10; fstrim / >/dev/null 2>&1
left=\$(dmsetup ls 2>/dev/null | awk '\$1 ~ /^dnv/ {print \$1}' | wc -l)
subs=\$(ls $NVMET/subsystems 2>/dev/null | grep -c dnv || true)
echo \"guest $1 cleaned: \$left dnv dm devices, \$subs dnv subsystems left\"
" || true
}

cleanup_all() {
	# The controller node first: its leg connections hold the disk node's
	# exports.
	[ -z "${VM[2]}" ] || cleanup_guest 2 >&2
	[ -z "${VM[1]}" ] || cleanup_guest 1 >&2
}

on_exit() {
	[ "$ARMED" -eq 1 ] || return 0
	cleanup_all
	[ -z "$LOG_DIR" ] || log "size_step: the agents' logs are in $LOG_DIR"
}
trap on_exit EXIT

# guard — both guests read before either is touched: each must answer with
# passwordless sudo and hold nothing of dnv. Every reason found is named and
# the tool exits, with nothing armed, so the exit changes neither guest.
guard() {
	local idx why bad=""
	for idx in 1 2; do
		why=$(sshv "$idx" "
p=\$(pgrep -x dnv-agent | tr '\n' ' ')
[ -z \"\$p\" ] || echo \"runs a dnv-agent (pid \$p)\"
n=\$(dmsetup ls 2>/dev/null | grep -c '^dnv')
[ \"\$n\" -eq 0 ] || echo \"holds \$n dnv dm devices\"
n=\$(ls $NVMET/subsystems 2>/dev/null | grep -c dnv)
[ \"\$n\" -eq 0 ] || echo \"holds \$n dnv nvmet subsystems\"
n=0
for c in /sys/class/nvme/nvme*; do
	case \"\$(cat \"\$c/subsysnqn\" 2>/dev/null)\" in $NQN_PREFIX*) n=\$((n + 1)) ;; esac
done
[ \"\$n\" -eq 0 ] || echo \"holds \$n dnv fabrics controllers\"
[ ! -e $WORK ] || echo \"holds $WORK\"
true") || why="answers no passwordless sudo"
		[ -z "$why" ] || bad="$bad guest $idx (${VM[$idx]}): $(printf '%s' "$why" | tr '\n' ';')"
	done
	[ -z "$bad" ] ||
		die "refused, nothing changed on either guest:$bad; run --cleanup-only, or wait for the run that owns it"
}

setup() {
	local idx out f
	resolve_jq
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	(cd "$REPO_ROOT" && go build -o "$DNCTL" ./integtest/dnagentctl >&2 &&
		go build -o "$CNCTL" ./integtest/cnagentctl >&2) ||
		die "building the drivers failed"
	guard
	ARMED=1
	for idx in 1 2; do
		sshv "$idx" "mkdir -p $WORK && chmod 0777 $WORK"
		scp -q "${SSH_OPTS[@]}" "$AGENT_BIN" "${VM[$idx]}:$WORK/dnv-agent"
		sshv "$idx" "for m in nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-thin-pool; do modprobe \$m 2>/dev/null || true; done; grep -q ' /sys/kernel/config ' /proc/mounts || mount -t configfs none /sys/kernel/config; mkdir -p $WORK/dn-store $WORK/cn-store"
		# The udev rules of deploy/udev, as the agent suites install them:
		# the dm rule keeps udev's probe off every dnv dm device. A rule the
		# guest holds already is left as it is.
		for f in "$UDEV_SRC"/*.rules; do
			! sshv "$idx" "test -e $UDEV_DIR/${f##*/}" || continue
			scp -q "${SSH_OPTS[@]}" "$f" "${VM[$idx]}:$WORK/${f##*/}"
			sshv "$idx" "mv -f $WORK/${f##*/} $UDEV_DIR/${f##*/}"
			UDEV_MINE[idx]="${UDEV_MINE[idx]} ${f##*/}"
		done
		sshv "$idx" "udevadm control --reload"
		log "guest $idx: $(sshv "$idx" "echo \$(nproc) CPUs, \$(free -m | awk '/^Mem:/ {print \$2}') MiB of memory, \$(df -BG --output=size /var/tmp | tail -1 | tr -d ' ') disk, kernel \$(uname -r)")"
	done
	printf 'SIZE header dn=%s (%s) cn=%s (%s)\n' "${VM[1]}" \
		"$(sshv 1 "echo \$(nproc) CPUs \$(free -m | awk '/^Mem:/ {print \$2}')MiB \$(uname -r)")" \
		"${VM[2]}" \
		"$(sshv 2 "echo \$(nproc) CPUs \$(free -m | awk '/^Mem:/ {print \$2}')MiB \$(uname -r)")"

	sshv 1 "truncate -s $DISK_SIZE $WORK/disk.img && losetup -f --show $WORK/disk.img" >"$WORK_LOCAL/loop"
	sshv 1 "setsid nohup $WORK/dnv-agent dn \
--grpc-network tcp --grpc-address ${IP[1]}:$DN_GRPC_PORT \
--tr-type tcp --adr-fam ipv4 --tr-addr ${IP[1]} --tr-svc-id $TR_SVC_ID \
--local-store $WORK/dn-store --disk $(cat "$WORK_LOCAL/loop") \
--nvmet-qid-max $QID_MAX --log-level info >> $WORK/dn-agent.log 2>&1 < /dev/null &"
	out=$(dnctl get-dn-size --wait 60 2>/dev/null) || die "the dn agent did not answer"
	log "dn agent: $out"
	sshv 2 "setsid nohup $WORK/dnv-agent cn \
--grpc-network tcp --grpc-address ${IP[2]}:$CN_GRPC_PORT \
--tr-type tcp --adr-fam ipv4 --tr-addr ${IP[2]} --tr-svc-id $TR_SVC_ID \
--local-store $WORK/cn-store --capacity $CN_CAPACITY \
--nvmet-qid-max $QID_MAX --log-level info >> $WORK/cn-agent.log 2>&1 < /dev/null &"
	if ! out=$(cnctl get-cn-size --wait 60 2>/dev/null); then
		sshv 2 "tail -n 20 $WORK/cn-agent.log" >&2 || true
		die "the cn agent did not answer (its log's last lines are above)"
	fi
	log "cn agent: $out"
	REV=$((REV + 1))
	DN_SYNC=$REV
	dnctl syncup-dn --revision "$REV" --extent-size "$EXTENT_SIZE" >/dev/null ||
		die "the first SyncupDn failed"
}

# provision <sp> <leg> <side> <zero bytes> — phase 1 of one side: its
# SyncupSide with the side not provisioned, which starts its zeroing.
provision() {
	REV=$((REV + 1))
	dnctl syncup-side --timeout "$RPC_TIMEOUT" --revision "$REV" --sp "$1" \
		--leg "$2" --side "$3" --ext-cnt 1 --zero-bytes "$4" --cntlid-slot 0 \
		--primary-cn "$CN_ID" --sp-level readwrite --provisioned=false \
		--expect-code 0 >/dev/null
}

# flip <sp> <leg> <side> <zero bytes> — phase 2: the provisioned flip.
flip() {
	REV=$((REV + 1))
	SIDE_REV["$1:$2:$3"]=$REV
	dnctl syncup-side --timeout "$RPC_TIMEOUT" --revision "$REV" --sp "$1" \
		--leg "$2" --side "$3" --ext-cnt 1 --zero-bytes "$4" --cntlid-slot 0 \
		--primary-cn "$CN_ID" --sp-level readwrite --provisioned=true \
		--expect-code 0 >/dev/null
}

# timed <var> <command…> — runs a command, adds its seconds to the named
# total and keeps the slowest in SLOWEST; a failure is recorded.
timed() {
	local var=$1 t0 t1 d rc=0
	shift
	t0=$(now)
	"$@" || rc=$?
	t1=$(now)
	d=$(secs "$t0" "$t1")
	printf -v "$var" '%s' "$(awk -v a="${!var}" -v b="$d" 'BEGIN { printf "%.2f", a + b }')"
	SLOWEST=$(awk -v a="$SLOWEST" -v b="$d" 'BEGIN { print (b > a ? b : a) }')
	[ "$rc" -eq 0 ] || failed "$* (rc $rc)"
	return 0
}

# One side of the disk node step: sp 0x1000+i, leg 1, side 2.
side_ptr() { printf '%s:0x1:0x2' "$((0x1000 + $1))"; }

step_sides() {
	local n=0 want i args t0 dn_t p1 zw p2 chk why out code
	for want in $SIDES; do
		why=$(low_room)
		[ -z "$why" ] || {
			log "size_step: stop before $want sides: $why"
			STOPPED=${STOPPED:-$why}
			return 0
		}
		args=()
		for ((i = 0; i < want; i++)); do args+=(--side "$(side_ptr "$i")"); done
		REV=$((REV + 1))
		DN_SYNC=$REV
		t0=$(now)
		dnctl syncup-dn --timeout "$RPC_TIMEOUT" --revision "$REV" \
			--extent-size "$EXTENT_SIZE" "${args[@]}" >/dev/null ||
			failed "SyncupDn with $want sides"
		dn_t=$(secs "$t0" "$(now)")
		p1=0 zw=0 p2=0 SLOWEST=0
		for ((i = n; i < want; i++)); do
			[ -z "$STOPPED" ] || break
			if [ $(((i - n) % ROOM_EVERY)) -eq $((ROOM_EVERY - 1)) ]; then
				room_ok "at $i sides" || break
			fi
			timed p1 provision $((0x1000 + i)) 0x1 0x2 "$DGRP_ZERO"
		done
		for ((i = n; i < want; i++)); do
			[ -z "$STOPPED" ] || break
			timed zw quiet dnctl wait-zeroed --sp $((0x1000 + i)) --leg 0x1 --side 0x2 \
				--interval 0.2 --timeout "$RPC_TIMEOUT"
		done
		for ((i = n; i < want; i++)); do
			[ -z "$STOPPED" ] || break
			timed p2 flip $((0x1000 + i)) 0x1 0x2 "$DGRP_ZERO"
		done
		# The round's code is recorded, not judged: a round that names a
		# leftover is a failed check round of the step.
		t0=$(now)
		out=$(dnctl check-dn --fresh --timeout "$RPC_TIMEOUT" \
			--revision "$DN_SYNC" --expect-code 0 2>/dev/null || true)
		chk=$(secs "$t0" "$(now)")
		code=$(code_of "$out")
		[ "$code" != none ] || failed "the fresh CheckDn round at $want sides"
		SAMPLE=-
		[ -n "$STOPPED" ] || check_sample side "$n" "$want"
		printf 'SIZE sides=%s syncup_dn=%ss phase1=%ss zero_wait=%ss flip=%ss slowest_side_call=%ss check_dn=%ss/code%s sample_rows=%s failed=%s dn_dm=%s dn_room=[%s] cn_room=[%s]\n' \
			"$want" "$dn_t" "$p1" "$zw" "$p2" "$SLOWEST" "$chk" "$code" \
			"$SAMPLE" "${STOPPED:-none}" "$(dm_cnt 1)" "$(room 1)" "$(room 2)"
		n=$want
		[ -z "$STOPPED" ] || return 0
	done
}

# drop_sides — the side list emptied; the sweep takes every side down.
drop_sides() {
	local t0
	REV=$((REV + 1))
	DN_SYNC=$REV
	t0=$(now)
	dnctl syncup-dn --timeout 600 --revision "$REV" --extent-size "$EXTENT_SIZE" \
		--expect-code 0 >/dev/null || failed "the SyncupDn that empties the side list"
	printf 'SIZE sides=0 syncup_dn=%ss failed=%s\n' "$(secs "$t0" "$(now)")" \
		"${STOPPED:-none}"
}

# req_cntlr_file <j> <file> — cntlr j's request: the cn suite's req_none,
# one primary cntlr of pool 0x2000+j whose meta and data groups each have
# one side on the disk node, one thin device and one subsystem.
req_cntlr_file() {
	local j=$1 sp=$((0x2000 + $1)) side
	side() {
		printf '{"side_id": "%s", "addr_port": "%s:%s", "cntlid_slot": 0, "nvme_tr_conf": {"tr_type": "tcp", "adr_fam": "ipv4", "tr_addr": "%s", "tr_svc_id": "%s"}, "provisioned": true}' \
			"$(d16 "$1")" "${IP[1]}" "$DN_GRPC_PORT" "${IP[1]}" "$TR_SVC_ID"
	}
	cat >"$2" <<EOF
{
  "cluster_id": "$(d16 "$CLUSTER")", "cn_id": "$(d16 "$CN_ID")",
  "cntlr_pointer": {"sp_id": "$(d16 "$sp")", "cntlr_id": "1"},
  "revision": "$CNREV",
  "bdev_conf": {
    "dm_pool_conf": {"data_block_size": "$BLOCK_SIZE", "low_water_mark_pct": 50},
    "dm_raid0_conf": {"stripe_size": "$STRIPE_SIZE"},
    "redund_conf": {"redund_none": {}}
  },
  "sp_level": "SP_LEVEL_READWRITE",
  "cntlr": {"addr_port": "${IP[2]}:$CN_GRPC_PORT", "nvme_tr_conf": {"tr_type": "tcp", "adr_fam": "ipv4", "tr_addr": "${IP[2]}", "tr_svc_id": "$TR_SVC_ID"}, "cntlid_slot": 0, "primary": true, "disabled": false},
  "id_to_slice": {
    "$(hex16 2)": {
      "slice_idx": 0,
      "meta_grp_list": [{"grp_id": "3", "ext_cnt": "1", "meta_blocks": "$META_BLOCKS", "data_blocks": "$DATA_BLOCKS", "leg_list": [{"leg_id": "4", "leg_idx": 0, "side_list": [$(side 5)]}]}],
      "data_grp_list": [{"grp_id": "6", "ext_cnt": "1", "meta_blocks": "$META_BLOCKS", "data_blocks": "$DATA_BLOCKS", "leg_list": [{"leg_id": "7", "leg_idx": 0, "side_list": [$(side 8)]}]}]
    }
  },
  "td_list": [{"td_id": "9", "dev_id": 1, "ori_id": 0, "size": "$TD_SIZE", "created": false}],
  "nqn_to_subsystem": {
    "nqn.2024-01.io.dnv-size:vol$j": {"ss_id": "10", "serial": "$(hex16 10)", "model": "dnv", "allowed_hosts": ["$HOST_NQN"], "ns_list": [{"ns_id": "11", "ns_idx": 1, "td_id": "9", "dev_uuid": "$(printf '77777777-7777-4777-8777-%012x' "$j")", "dev_nguid": "$(printf '77777777777747778777%012x' "$j")", "suspended": false}]}
  }
}
EOF
}

# build_cntlr <j> <file> — cntlr j's first SyncupCntlr, re-sent at the
# same revision while it answers the leftover code, as the worker re-sends
# one (dnv-worker.md RW4), for up to RPC_TIMEOUT seconds; prints the sends
# it took, or fails on any other code or no reply.
build_cntlr() {
	local out code n=0 deadline=$((SECONDS + RPC_TIMEOUT))
	while :; do
		n=$((n + 1))
		out=$(cnctl syncup-cntlr --timeout "$RPC_TIMEOUT" --req "$2" \
			--expect-code 0 2>/dev/null || true)
		code=$(code_of "$out")
		case "$code" in
		0) echo "$n"; return 0 ;;
		4) [ "$SECONDS" -lt "$deadline" ] || break; sleep 1 ;;
		*) break ;;
		esac
	done
	log "size_step: cntlr $1's SyncupCntlr answers code $code after $n send(s)"
	return 1
}

step_cntlrs() {
	local m=0 want j args t0 t1 d sd sc cc chk1 chk2 why ptrs out c1 c2 sends last
	local reqdir resent
	reqdir=$(mktemp -d)
	for want in $CNTLRS; do
		why=$(low_room)
		[ -z "$why" ] || {
			log "size_step: stop before $want cntlrs: $why"
			STOPPED=${STOPPED:-$why}
			break
		}
		# The disk node first: every cntlr's two sides, provisioned.
		ptrs=()
		for ((j = 0; j < want; j++)); do
			ptrs+=(--side "$((0x2000 + j)):0x4:0x5" --side "$((0x2000 + j)):0x7:0x8")
		done
		REV=$((REV + 1))
		DN_SYNC=$REV
		dnctl syncup-dn --timeout "$RPC_TIMEOUT" --revision "$REV" \
			--extent-size "$EXTENT_SIZE" "${ptrs[@]}" >/dev/null ||
			failed "SyncupDn with the sides of $want cntlrs"
		SLOWEST=0 sd=0
		for ((j = m; j < want; j++)); do
			[ -z "$STOPPED" ] || break
			if [ $(((j - m) % ROOM_EVERY)) -eq $((ROOM_EVERY - 1)) ]; then
				room_ok "at the sides of $j cntlrs" || break
			fi
			timed sd provision $((0x2000 + j)) 0x4 0x5 "$MGRP_ZERO"
			timed sd provision $((0x2000 + j)) 0x7 0x8 "$DGRP_ZERO"
		done
		for ((j = m; j < want; j++)); do
			[ -z "$STOPPED" ] || break
			timed sd quiet dnctl wait-zeroed --sp $((0x2000 + j)) --leg 0x4 --side 0x5 \
				--interval 0.2 --timeout "$RPC_TIMEOUT"
			timed sd quiet dnctl wait-zeroed --sp $((0x2000 + j)) --leg 0x7 --side 0x8 \
				--interval 0.2 --timeout "$RPC_TIMEOUT"
			timed sd flip $((0x2000 + j)) 0x4 0x5 "$MGRP_ZERO"
			timed sd flip $((0x2000 + j)) 0x7 0x8 "$DGRP_ZERO"
		done
		[ -z "$STOPPED" ] || break
		# Then the controller node: one SyncupCn with every cntlr, then each
		# new cntlr's first SyncupCntlr.
		args=()
		for ((j = 0; j < want; j++)); do args+=(--cntlr "$((0x2000 + j)):0x1"); done
		CNREV=$((CNREV + 1))
		CN_SYNC=$CNREV
		t0=$(now)
		cnctl syncup-cn --timeout "$RPC_TIMEOUT" --revision "$CNREV" "${args[@]}" \
			>/dev/null || failed "SyncupCn with $want cntlrs"
		sc=$(secs "$t0" "$(now)")
		SLOWEST=0 cc=0 resent=0 last=""
		for ((j = m; j < want; j++)); do
			[ -z "$STOPPED" ] || break
			if [ $(((j - m) % ROOM_EVERY)) -eq $((ROOM_EVERY - 1)) ]; then
				room_ok "at $j cntlrs" || break
			fi
			CNREV=$((CNREV + 1))
			CNTLR_REV[$j]=$CNREV
			last=$j
			req_cntlr_file "$j" "$reqdir/cntlr-$j.json"
			t0=$(now)
			if sends=$(build_cntlr "$j" "$reqdir/cntlr-$j.json"); then
				resent=$((resent + sends - 1))
			else
				failed "cntlr $j's SyncupCntlr"
			fi
			t1=$(now)
			d=$(secs "$t0" "$t1")
			cc=$(awk -v a="$cc" -v b="$d" 'BEGIN { printf "%.2f", a + b }')
			SLOWEST=$(awk -v a="$SLOWEST" -v b="$d" 'BEGIN { print (b > a ? b : a) }')
		done
		t0=$(now)
		out=$(cnctl check-cn --fresh --timeout "$RPC_TIMEOUT" --revision "$CN_SYNC" \
			--expect-code 0 2>/dev/null || true)
		chk1=$(secs "$t0" "$(now)")
		c1=$(code_of "$out")
		# The round of the last cntlr the step sent, which is the step's last
		# unless it stopped early.
		chk2=- c2=-
		if [ -n "$last" ]; then
			t0=$(now)
			out=$(cnctl check-cntlr --fresh --timeout "$RPC_TIMEOUT" \
				--revision "${CNTLR_REV[$last]}" --sp $((0x2000 + last)) --cntlr 0x1 \
				--expect-code 0 2>/dev/null || true)
			chk2=$(secs "$t0" "$(now)")
			c2=$(code_of "$out")
		fi
		[ "$c1" != none ] && [ "$c2" != none ] ||
			failed "a fresh check round at $want cntlrs gave no reply"
		SAMPLE=-
		[ -n "$STOPPED" ] || check_sample cntlr "$m" "$want"
		printf 'SIZE cntlrs=%s sent=%s dn_sides=%s side_calls=%ss syncup_cn=%ss cntlr_syncups=%ss slowest_cntlr=%ss resends=%s check_cn=%ss/code%s check_cntlr=%ss/code%s sample_rows=%s failed=%s dn_dm=%s cn_dm=%s cn_nvme_ctrls=%s dn_room=[%s] cn_room=[%s]\n' \
			"$want" "$((${last:-$((m - 1))} + 1))" "$((2 * want))" "$sd" "$sc" "$cc" \
			"$SLOWEST" "$resent" "$chk1" "$c1" "$chk2" "$c2" "$SAMPLE" "${STOPPED:-none}" "$(dm_cnt 1)" \
			"$(dm_cnt 2)" \
			"$(sshv 2 "ls -d /sys/class/nvme/nvme[0-9]* 2>/dev/null | wc -l")" \
			"$(room 1)" "$(room 2)"
		m=$want
		[ -z "$STOPPED" ] || break
	done
	rm -rf "$reqdir"
}

usage() {
	cat >&2 <<EOF
usage: bash integtest/size_step.sh [--sides "64 128 256 512 1024"] \\
           [--cntlrs "16 64 128 256"] [--log-dir <dir>] [--cleanup-only] \\
           <user@dn-guest> <user@cn-guest>
EOF
	exit 2
}

main() {
	local positional=()
	while [ $# -gt 0 ]; do
		case "$1" in
		--sides) SIDES=$2; shift 2 ;;
		--cntlrs) CNTLRS=$2; shift 2 ;;
		--log-dir) LOG_DIR=$2; shift 2 ;;
		--cleanup-only) CLEANUP_ONLY=1; shift ;;
		-*) usage ;;
		*) positional+=("$1"); shift ;;
		esac
	done
	[ "${#positional[@]}" -eq 2 ] || usage
	VM[1]=${positional[0]}
	VM[2]=${positional[1]}
	IP[1]=${VM[1]##*@}
	IP[2]=${VM[2]##*@}
	UDEV_SRC_NAMES=$(cd "$UDEV_SRC" && echo *.rules)
	WORK_LOCAL=$(mktemp -d)
	[ -n "$LOG_DIR" ] || LOG_DIR=$(mktemp -d "${TMPDIR:-/tmp}/dnv-size-step.XXXXXX")
	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		# Asked for by name: every rule of deploy/udev goes too.
		UDEV_MINE=("" "$UDEV_SRC_NAMES" "$UDEV_SRC_NAMES")
		ARMED=1
		return 0
	fi
	setup
	step_sides
	drop_sides
	if [ -z "$STOPPED" ]; then
		step_cntlrs
	fi
	log "size_step: done; stopped: ${STOPPED:-no}"
	rm -rf "$WORK_LOCAL"
}

main "$@"
