#!/usr/bin/env bash
#
# runner.sh — runs the integration suites one after another from one
# configuration file (layout.md, Directory tree).
#
#   bash integtest/runner.sh [--conf <file>] [--out <dir>]
#
# <file> is integtest/runner.conf unless given; its header gives the format,
# one run per line: a name, the suite's script, the suite's arguments. <dir>
# is a new directory under integtest/bin/runs unless given, and must be new
# or empty. Each run's output goes to <dir>/<nn>-<name>.log; after a failure,
# the logs copied off each of its guests to <dir>/<nn>-<name>.remote/<host>.tgz
# and the output of its cleanup-only run to <dir>/<nn>-<name>.cleanup.log; and
# the summary to <dir>/summary.txt as well as to stdout. Follow a run with
# `tail -f` on the log its start line names.
#
# Each suite stops at its first failure, as it does when run by hand. After
# every failed run, a usage error included, the runner keeps its output,
# copies the logs the suite left on its guests, runs the same suite with the
# same arguments and --cleanup-only, so that the next run finds the guests
# clean, and goes on. Nothing runs in parallel: the suites share guests.
#
# The copy comes first because the cleanup-only run deletes those logs, which
# the suite's failure report points at. From each guest that a user@host word
# among the run's arguments names, alone or as the value of a --<flag>=, once
# per host, it takes the *.log files at any depth under the work directory the
# suite's first WORK=/ line names, over ssh as the suites call it and under
# timeout. It is best effort: its result shows in the output and the summary,
# and changes neither the run's result, nor the runner's exit status, nor what
# runs after it.
#
# The runner names the commit its own tree is on and the tree's state when it
# starts, and the summary names both as read at the start and at the end: every
# suite builds the programs from the working tree when it starts. The state is
# clean when git status lists nothing, untracked files included whatever git's
# configuration says, and dirty when it lists anything. Each of the two is
# unknown when its git call fails, as both do in a copy of the tree without
# .git, so a failed git status never reads as clean. git looks for the
# repository in the runner's own tree alone, so such a copy reads unknown also
# where it sits inside another work tree, and never names that tree's commit.
#
# Exit status: 0 when every run passed, 1 when any failed, 2 on a usage or
# configuration error (before anything runs), and the signal's status when
# interrupted. An interrupted runner starts no further copy, no cleanup-only
# run and no later run, and stops once the running suite or copy has ended:
# Ctrl-C, or a signal to the runner's process group, reaches that suite or
# copy too, while a signal to the runner's process alone waits for it to end.
# The run that was going keeps the result its suite ended with, and the
# summary names the signal. A reader of the runner's output that goes away
# first, as tee does on a Ctrl-C, changes none of this: the runner ignores
# SIGPIPE, so its writes to that reader fail and it goes on, while each suite
# starts with SIGPIPE as the runner found it.
#
# Edit nothing in the tree while the runner runs. bash reads a script by byte
# offset as it goes, so a change of length in the running suite can end its
# run with a syntax error at an unrelated line, once its main returns and bash
# reads on from the old offset; and each suite builds the programs from the
# working tree when it starts, so a later run tests whatever the tree holds
# then.
#
# A run's result is the suite's exit status alone, and a PASS line in its
# output is no proof of a pass: a suite interrupted by a signal can take its
# success branch, run its end cleanup and print PASS before it exits with the
# signal's status. When the command it waits on catches the signal, as ssh
# does, an interrupted suite can instead fail at the stage it was in, or run
# on to its end.

set -uo pipefail

# CDPATH is emptied for the cd: the usage runs this file by a relative path,
# whose directory cd would otherwise look up in CDPATH, printing where it went.
REPO_ROOT=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
CONF="$REPO_ROOT/integtest/runner.conf"
OUT=""

# copy_logs reaches a guest with the ssh options the suites use, ends one
# guest's copy after COPY_SECS seconds, and kills it ten seconds after that if
# it is still there.
COPY_SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)
COPY_SECS=120

# One entry per run, in the configuration file's order.
RUN_NAME=()
RUN_SCRIPT=()
RUN_ARGS=() # the suite's arguments, joined by single spaces
RUN_RC=()
RUN_SECS=()
RUN_STAGE=()
RUN_TRACE=()
RUN_REASON=()
RUN_COPY=()     # copy_logs' account of the guests' logs; empty unless it got past its stop check
RUN_CLEAN_RC=() # the cleanup-only run's exit status, or "interrupted"; empty when none ran
STOP_SIG=""     # the signal that interrupted the runner, empty while none has
STOP_RC=""      # its exit status, 128 plus the signal's number
SUMMARY=""
TREE_START=""   # tree_state as read at the start
LINE_NO=""      # the file line the run being read starts on, for read_conf's refusals

usage() {
	cat >&2 <<EOF
usage: bash integtest/runner.sh [--conf <file>] [--out <dir>]
EOF
	exit 2
}

# conf_error refuses the configuration before anything runs.
conf_error() {
	echo "runner: $CONF: ${LINE_NO:+line $LINE_NO: }$*" >&2
	exit 2
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--conf)
			[ $# -ge 2 ] || usage
			CONF=$2
			shift 2
			;;
		--out)
			[ $# -ge 2 ] || usage
			OUT=$2
			shift 2
			;;
		-h | --help | *) usage ;;
		esac
	done
	[ -n "$OUT" ] || OUT="$REPO_ROOT/integtest/bin/runs/$(date -u +%Y%m%dT%H%M%SZ)"
}

# read_conf reads the whole file before the first run starts, so an edit made
# while the runner works changes nothing, and refuses what cannot be run,
# naming the file line the refused run starts on. A line goes on on the next
# one when it ends with an odd number of backslashes, the last of them dropped:
# the others pair up, each pair one escaped backslash. A comment line, one
# whose first character other than a blank is #, is skipped wherever it is
# and adds nothing to a run: inside a run that goes on, the run goes on past
# it when the comment line itself ends with an odd number of backslashes, and
# ends there when it does not; outside one, its backslashes mean nothing, so
# it never takes in the line after it.
read_conf() {
	[ -f "$CONF" ] || conf_error "no such file"
	local line text="" more=0 file_line=0 trail comment_re='^[[:blank:]]*#'
	while IFS= read -r line || [ -n "$line" ]; do
		file_line=$((file_line + 1))
		trail=${line##*[!\\]}
		if [[ $line =~ $comment_re ]]; then
			if [ "$more" -eq 1 ] && [ $((${#trail} % 2)) -eq 0 ]; then
				more=0
				read_run "$text"
			fi
			continue
		fi
		[ "$more" -eq 1 ] || { text=""; LINE_NO=$file_line; }
		if [ $((${#trail} % 2)) -eq 1 ]; then
			text+=${line%\\}
			more=1
			continue
		fi
		text+=$line
		more=0
		read_run "$text"
	done <"$CONF"
	[ "$more" -eq 0 ] || read_run "$text"
	LINE_NO=""
	[ "${#RUN_NAME[@]}" -ge 1 ] || conf_error "lists no run"
}

# read_run takes one run's text, its continued lines joined, and adds the run.
# read without -r is deliberate: a backslash keeps the character after it, so
# that a pair of backslashes is one, as read_conf's continuation rule has it,
# and an escaped blank stays in its word. Only the script's path may hold a
# blank: the arguments are kept joined by single spaces (RUN_ARGS) and split
# at every blank again when the suite starts, so an argument that holds one
# is refused rather than handed to the suite as two, past the refusal of
# --cleanup-only and --wipe, which looks at the words as read.
read_run() { # <text>
	local words name script arg i
	# shellcheck disable=SC2162
	read -a words <<<"$1"
	[ "${#words[@]}" -gt 0 ] || return 0
	[ "${#words[@]}" -ge 2 ] || conf_error "run '${words[0]}' names no suite script"
	name=${words[0]}
	script=${words[1]}
	case $name in
	*[!A-Za-z0-9_.-]*) conf_error "run name '$name' is not a plain word" ;;
	esac
	for i in "${!RUN_NAME[@]}"; do
		[ "${RUN_NAME[$i]}" != "$name" ] || conf_error "run name '$name' appears twice"
	done
	case $script in
	/*) ;;
	*) script="$REPO_ROOT/$script" ;;
	esac
	[ -f "$script" ] || conf_error "run '$name': no such script $script"
	for arg in "${words[@]:2}"; do
		case $arg in
		--cleanup-only | --wipe) conf_error "run '$name': $arg runs no case" ;;
		*[[:blank:]]*) conf_error "run '$name': the argument '$arg' holds a blank" ;;
		esac
	done
	RUN_NAME+=("$name")
	RUN_SCRIPT+=("$script")
	RUN_ARGS+=("${words[*]:2}")
}

ts() { date -u +%H:%M:%S; }

fmt_secs() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

log_of() { printf '%s/%02d-%s.log' "$OUT" $(($1 + 1)) "${RUN_NAME[$1]}"; }

# run_suite runs a suite's script with SIGPIPE as the runner found it, not
# ignored as main makes it for the runner itself.
run_suite() { # <script> <arg>…
	(
		trap - PIPE
		exec bash "$@"
	)
}

# tree_state names the commit the runner's tree is on and the tree's state.
# Each git call's exit status is checked: a failed one reads as unknown, so a
# failed git status never reads as clean. --no-optional-locks keeps git status
# from taking the index lock to refresh the index, so a git command run beside
# the runner never finds that lock held by the runner. --untracked-files=normal
# lists an untracked file even where git's configuration hides untracked files
# from git status: go builds an untracked file of a package as it builds a
# tracked one. Each call reads the runner's own tree and nothing else:
# GIT_CEILING_DIRECTORIES, set to the tree's parent as a physical path, since
# git compares it with the physical directory it searches from, stops git from
# looking for a repository above the tree, and a GIT_DIR or GIT_WORK_TREE the
# runner was started with is dropped.
tree_state() {
	local ceiling commit state changes
	if ! ceiling=$(CDPATH='' cd -P -- "$REPO_ROOT/.." 2>/dev/null && pwd -P); then
		echo "commit unknown, tree unknown"
		return
	fi
	commit=$(env -u GIT_DIR -u GIT_WORK_TREE GIT_CEILING_DIRECTORIES="$ceiling" \
		git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null) || commit=unknown
	if changes=$(env -u GIT_DIR -u GIT_WORK_TREE GIT_CEILING_DIRECTORIES="$ceiling" \
		git -C "$REPO_ROOT" --no-optional-locks status --porcelain \
		--untracked-files=normal 2>/dev/null); then
		if [ -z "$changes" ]; then state=clean; else state=dirty; fi
	else
		state=unknown
	fi
	echo "commit $commit, tree $state"
}

# failure_of reads a failed run's log. The stage is the one the last
# "; failing stage '<stage>'" line names, which a suite's on_exit prints after
# its diagnostics when the suite fails, and which other lines can follow. The
# trace id and the reason come from the last "FAILED at stage" line before the
# diagnostics banner, and its next line: a die inside the diagnostics comes
# after the banner and is not the failure's. Among the failures that print no
# stage line, and so leave the trace id and the reason unread, are a usage
# error, an end cleanup that did not finish (the e2e suite prints its LAB NOT
# CLEAN banner instead), a signal that made the suite take its success branch,
# and a kill before on_exit ran.
failure_of() { # <run index> <rc>
	local i=$1 rc=$2 log stage last
	log=$(log_of "$i")
	stage=$(grep -a -E "; failing stage '.*'\$" "$log" | tail -n 1 |
		sed -E "s/.*; failing stage '(.*)'\$/\\1/")
	last=$(awk '/^########## diagnostics/ { exit }
		/^FAILED at stage / { f = $0; r = ""; if ((getline r) <= 0) r = "" }
		END { print f; print r }' "$log")
	local fline=${last%%$'\n'*} reason=${last#*$'\n'}
	local fstage="" trace=""
	if [ -n "$fline" ]; then
		fstage=$(sed -E "s/^FAILED at stage '(.*)' \\(trace_id [^)]*\\)\$/\\1/" <<<"$fline")
		trace=$(sed -E "s/^FAILED at stage '.*' \\(trace_id ([^)]*)\\)\$/\\1/" <<<"$fline")
	fi
	if [ -z "$stage" ]; then
		if [ "$rc" -ge 128 ]; then
			stage="(interrupted by signal $((rc - 128)))"
		elif grep -a -q '^LAB NOT CLEAN' "$log"; then
			stage="(the end cleanup did not finish)"
		elif [ "$rc" -eq 2 ]; then
			stage="(usage error: the suite ran nothing)"
		else
			stage="(no failing stage in the output)"
		fi
		trace="" reason=""
	elif [ "$fstage" != "$stage" ]; then
		# The suite left by an unchecked command or by a signal, not by die:
		# no line names this stage's trace id.
		trace="" reason=""
	fi
	RUN_STAGE[$i]=$stage
	RUN_TRACE[$i]=$trace
	RUN_REASON[$i]=$(sed -E 's/^ +//' <<<"$reason" | cut -c1-200)
}

# run_one runs one suite and records its result. It starts nothing once the
# runner has been interrupted, and the run then reads as not run.
run_one() { # <run index>
	local i=$1 log start rc=0 args
	log=$(log_of "$i")
	read -r -a args <<<"${RUN_ARGS[$i]}"
	[ -z "$STOP_RC" ] || return 0
	echo "$(ts) start $((i + 1))/${#RUN_NAME[@]} ${RUN_NAME[$i]}: bash ${RUN_SCRIPT[$i]} ${RUN_ARGS[$i]}"
	echo "         output: $log"
	start=$SECONDS
	run_suite "${RUN_SCRIPT[$i]}" "${args[@]}" </dev/null >"$log" 2>&1 || rc=$?
	RUN_RC[$i]=$rc
	RUN_SECS[$i]=$((SECONDS - start))
	RUN_CLEAN_RC[$i]=""
	if [ "$rc" -eq 0 ]; then
		echo "$(ts) end   ${RUN_NAME[$i]}: PASS in $(fmt_secs "${RUN_SECS[$i]}")"
		return 0
	fi
	failure_of "$i" "$rc"
	echo "$(ts) end   ${RUN_NAME[$i]}: FAIL (exit $rc) in $(fmt_secs "${RUN_SECS[$i]}") at stage ${RUN_STAGE[$i]}"
	return 1
}

# copy_logs copies a failed run's logs off its guests, unless the runner has
# been interrupted: each guest's logs go into <nn>-<name>.remote/<host>.tgz,
# and what ssh, find and tar print on stderr into <host>.err. A copy runs
# under timeout in the runner's process group, so that a signal to the group
# reaches it as it reaches a suite. tar exits 1 when a file changed while it
# read it, as a log that a process the failed suite left running goes on
# writing does, and the archive then holds every log; a work directory that
# cd cannot enter can exit 1 too, but writes nothing. So a copy is done on 0,
# or on 1 with an archive that is not empty, and an empty archive is removed.
# copy_logs records what it did in RUN_COPY and changes no other result.
copy_logs() { # <run index>
	local i=$1 line work args word host seen=" " guests=() dir tgz err rc why see
	local copied=0 missed=""
	local guest_re='^[A-Za-z0-9._][A-Za-z0-9._-]*@[A-Za-z0-9._:-]+$'
	[ -z "$STOP_RC" ] || return 0
	line=$(grep -a -m 1 '^WORK=/' "${RUN_SCRIPT[$i]}" 2>/dev/null) || line=""
	work=${line#WORK=}
	work=${work%%[[:space:]]*}
	read -r -a args <<<"${RUN_ARGS[$i]}"
	for word in "${args[@]}"; do
		# A guest is a user@host word, given alone or as the value of a
		# --<flag>=, which the e2e suite takes for every flag.
		word=${word#--*=}
		[[ $word =~ $guest_re ]] || continue
		host=${word#*@}
		case $seen in *" $host "*) continue ;; esac
		seen+="$host "
		guests+=("$word")
	done
	dir=${OUT}/$(printf '%02d' $((i + 1)))-${RUN_NAME[$i]}.remote
	# The work directory goes into the remote command as it is written, so
	# it must be a plain absolute path.
	if [ -z "$line" ]; then
		RUN_COPY[$i]="none: the suite has no WORK=/ line"
	elif [[ $work == *[!A-Za-z0-9._/-]* ]]; then
		RUN_COPY[$i]="none: the suite's first WORK=/ line names no plain path"
	elif [ "${#guests[@]}" -eq 0 ]; then
		RUN_COPY[$i]="none: no user@host word among the run's arguments"
	elif ! mkdir -p "$dir"; then
		RUN_COPY[$i]="none: $dir cannot be made"
	fi
	if [ -n "${RUN_COPY[$i]:-}" ]; then
		echo "$(ts) logs  ${RUN_NAME[$i]}: ${RUN_COPY[$i]}"
		return 0
	fi
	for word in "${guests[@]}"; do
		[ -z "$STOP_RC" ] || break
		host=${word#*@}
		tgz=$dir/$host.tgz
		err=$dir/$host.err
		rc=0
		timeout --foreground -k 10 "$COPY_SECS" ssh "${COPY_SSH_OPTS[@]}" "$word" \
			"cd $work && find . -type f -name '*.log' -print0 | tar --null -T - -czf -" \
			</dev/null >"$tgz" 2>"$err" || rc=$?
		[ -s "$err" ] || rm -f "$err"
		if [ "$rc" -eq 0 ] || { [ "$rc" -eq 1 ] && [ -s "$tgz" ]; }; then
			copied=$((copied + 1))
			echo "$(ts) logs  ${RUN_NAME[$i]}: $word: copied to $tgz"
			continue
		fi
		[ -s "$tgz" ] || rm -f "$tgz"
		see=""
		[ ! -e "$err" ] || see=", see $err"
		if [ -n "$STOP_RC" ] && [ "$rc" -ge 128 ]; then
			why=interrupted
			echo "$(ts) logs  ${RUN_NAME[$i]}: $word: interrupted (exit $rc)$see"
		else
			why="exit $rc"
			echo "$(ts) logs  ${RUN_NAME[$i]}: $word: FAILED (exit $rc)$see"
		fi
		missed+="${missed:+, }$host ($why)"
	done
	RUN_COPY[$i]="$copied of ${#guests[@]} copied to ${dir##*/}${missed:+; not copied: $missed}"
	return 0
}

# cleanup_one runs a failed run's cleanup-only run, unless the runner has been
# interrupted. A cleanup-only run that ends with a signal's status, 128 or
# more, after the runner was interrupted reads as interrupted, not as failed.
cleanup_one() { # <run index>
	local i=$1 log rc=0 args
	log=${OUT}/$(printf '%02d' $((i + 1)))-${RUN_NAME[$i]}.cleanup.log
	read -r -a args <<<"${RUN_ARGS[$i]}"
	[ -z "$STOP_RC" ] || return 0
	echo "$(ts) clean ${RUN_NAME[$i]}: bash ${RUN_SCRIPT[$i]} ${RUN_ARGS[$i]} --cleanup-only"
	run_suite "${RUN_SCRIPT[$i]}" "${args[@]}" --cleanup-only </dev/null >"$log" 2>&1 || rc=$?
	RUN_CLEAN_RC[$i]=$rc
	if [ -n "$STOP_RC" ] && [ "$rc" -ge 128 ]; then
		RUN_CLEAN_RC[$i]=interrupted
	fi
	case ${RUN_CLEAN_RC[$i]} in
	0) echo "$(ts) clean ${RUN_NAME[$i]}: done" ;;
	interrupted) echo "$(ts) clean ${RUN_NAME[$i]}: interrupted (exit $rc), see $log" ;;
	*) echo "$(ts) clean ${RUN_NAME[$i]}: FAILED (exit $rc), see $log" ;;
	esac
}

# summary reads the tree again, writes the summary file, then prints it. The
# counts are taken in this shell: a `| tee` would run the loop in a subshell
# and lose them.
summary() {
	local i failed=0 ran=0 line tree_end
	tree_end=$(tree_state)
	{
		echo ""
		echo "=== runner summary: $CONF"
		echo "    output: $OUT"
		echo "    start: $TREE_START"
		echo "    end:   $tree_end"
		for i in "${!RUN_NAME[@]}"; do
			if [ -z "${RUN_RC[$i]:-}" ]; then
				printf '%2d %-10s %s\n' $((i + 1)) "${RUN_NAME[$i]}" "NOT RUN"
				continue
			fi
			ran=$((ran + 1))
			if [ "${RUN_RC[$i]}" -eq 0 ]; then
				printf '%2d %-10s PASS %9s\n' $((i + 1)) "${RUN_NAME[$i]}" "$(fmt_secs "${RUN_SECS[$i]}")"
				continue
			fi
			failed=$((failed + 1))
			line=$(printf '%2d %-10s FAIL %9s  exit %s  stage %s' $((i + 1)) "${RUN_NAME[$i]}" \
				"$(fmt_secs "${RUN_SECS[$i]}")" "${RUN_RC[$i]}" "${RUN_STAGE[$i]}")
			[ -z "${RUN_TRACE[$i]}" ] || line="$line (trace_id ${RUN_TRACE[$i]})"
			echo "$line"
			[ -z "${RUN_REASON[$i]}" ] || echo "              ${RUN_REASON[$i]}"
			echo "              guest logs: ${RUN_COPY[$i]:-not copied}"
			case ${RUN_CLEAN_RC[$i]:-} in
			"") echo "              no cleanup-only run" ;;
			0) echo "              cleanup-only run: done" ;;
			interrupted) echo "              cleanup-only run: interrupted" ;;
			*) echo "              cleanup-only run: FAILED (exit ${RUN_CLEAN_RC[$i]})" ;;
			esac
		done
		echo "=== $((ran - failed)) of ${#RUN_NAME[@]} runs passed, $failed failed, $((${#RUN_NAME[@]} - ran)) not run"
		[ -z "$STOP_SIG" ] ||
			echo "=== interrupted by $STOP_SIG: no cleanup-only run and no later run started after it"
	} >"$SUMMARY"
	cat "$SUMMARY"
	[ "$failed" -eq 0 ] && [ "$ran" -eq "${#RUN_NAME[@]}" ]
}

# on_signal records that the runner was interrupted, the first signal only.
# bash runs it once the command in the foreground, most often a suite, has
# ended, before the command's status is read, and gives that status back when
# it returns, so a run keeps its suite's own result; main then starts nothing
# more, prints the summary and exits with the signal's status.
on_signal() { # <signal name> <status>
	[ -z "$STOP_RC" ] || return 0
	STOP_SIG=$1
	STOP_RC=$2
	echo "$(ts) runner: $1 received; starting no cleanup-only run and no later run"
}

main() {
	parse_args "$@"
	read_conf
	[ ! -e "$OUT" ] || [ -z "$(ls -A "$OUT" 2>/dev/null)" ] ||
		{ echo "runner: $OUT exists and is not empty" >&2; exit 2; }
	mkdir -p "$OUT" || exit 2
	SUMMARY="$OUT/summary.txt"
	TREE_START=$(tree_state)
	trap 'on_signal INT 130' INT
	trap 'on_signal TERM 143' TERM
	trap 'on_signal HUP 129' HUP
	# A reader of the runner's output can go away before the runner is done,
	# as tee does on a Ctrl-C, and bash itself writes a notice to stderr when
	# it reaps a suite that TERM or HUP killed. With SIGPIPE ignored, such a
	# write fails instead of killing the runner before it has written the
	# summary.
	trap '' PIPE
	echo "$(ts) runner: ${#RUN_NAME[@]} runs from $CONF, output in $OUT"
	echo "$(ts) runner: $TREE_START"
	local i any_failed=0
	for i in "${!RUN_NAME[@]}"; do
		[ -z "$STOP_RC" ] || break
		if ! run_one "$i"; then
			any_failed=1
			copy_logs "$i"
			cleanup_one "$i"
		fi
	done
	summary || any_failed=1
	[ -z "$STOP_RC" ] || exit "$STOP_RC"
	exit "$any_failed"
}

main "$@"
