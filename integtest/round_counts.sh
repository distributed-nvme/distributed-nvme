#!/usr/bin/env bash
#
# round_counts.sh — how a run's check rounds went, counted from copied
# dnv-worker logs (dnv-worker.md, HL7 and Log records).
#
#   bash integtest/round_counts.sh <worker log | directory> …
#
# It reads the worker's JSON-lines log files it is given; a directory stands
# for every worker.log below it, such as the tree `e2e_test.sh --keep-logs DIR`
# copies out (DIR/<case>/cp-<ip>/worker/worker.log). No stage of any suite
# calls it: it is a tool for reading a run's logs afterwards — the copy case
# at 32 slices, say — and it judges nothing, so it exits 0 whatever it counts.
#
# For each kind of object — a disk node (dn_id alone), a side (dn_id with
# side_pointer), a controller node (cn_id alone) and a cntlr (cn_id with
# cntlr_pointer) — it prints:
#
#   * the `check round failed` records, by their error: a round with no reply
#     in time (round timeout), one whose observation was too old (observation
#     too old), a stream that broke (check stream: …, cut at the gRPC code), a
#     stopping loop (context canceled) and any other;
#   * the `check stream open failed` and `check stream send failed` records;
#   * the `health changed` records whose reason is unreachable, by the kind
#     the record names;
#   * the `late observation` records, stale and not, with the largest
#     age_ms of those that answer from an observation; and apart, how many
#     answer from none, told by the no_obs attribute the worker puts on
#     those records alone, with their largest age_ms. The age of a round
#     with no observation counts from the first round after a pause in the
#     loop's rounds (HL7), not from an observation, so it never enters the
#     largest age of an observation.
#
# Records of other messages, and lines that are not JSON (a line torn by a
# write still in progress), are skipped. A grep in front keeps jq's input to
# the five messages, so a large log costs one pass of grep and a small jq
# evaluation. jq, or the gojq drop-in the suites build into integtest/bin,
# does the counting.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

die() {
	echo "round_counts.sh: $*" >&2
	exit 1
}

[ $# -ge 1 ] || die "usage: bash integtest/round_counts.sh <worker log | directory> …"

if command -v jq >/dev/null 2>&1; then
	JQ=jq
elif [ -x "$REPO_ROOT/integtest/bin/gojq" ]; then
	JQ="$REPO_ROOT/integtest/bin/gojq"
else
	die "missing: jq, or the gojq the suites build into integtest/bin"
fi

files=()
for arg in "$@"; do
	if [ -d "$arg" ]; then
		while IFS= read -r -d '' f; do
			files+=("$f")
		done < <(find "$arg" -type f -name worker.log -print0 | sort -z)
	elif [ -f "$arg" ]; then
		files+=("$arg")
	else
		die "no such file or directory: $arg"
	fi
done
[ "${#files[@]}" -ge 1 ] || die "no worker.log under: $*"

echo "worker logs read:"
printf '  %s\n' "${files[@]}"
echo

# grep exits 1 when nothing matches, which here is a count of zero.
{
	grep -hF \
		-e '"msg":"check round failed"' \
		-e '"msg":"check stream open failed"' \
		-e '"msg":"check stream send failed"' \
		-e '"msg":"health changed"' \
		-e '"msg":"late observation"' \
		"${files[@]}" || [ $? -eq 1 ]
} | "$JQ" -R -n -r '
	# The kind of object a record of a revision loop names, from its
	# identity attributes.
	def kind:
		if has("side_pointer") then "side"
		elif has("cntlr_pointer") then "cntlr"
		elif has("dn_id") then "dn"
		elif has("cn_id") then "cn"
		else "other" end;
	# A stream error carries the whole gRPC status; it is cut at its code.
	def cause:
		(.error // "(no error)")
		| if test("rpc error: code = [A-Za-z]+")
		  then sub("rpc error: code = (?<c>[A-Za-z]+).*$";
		      "rpc error: code = \(.c)")
		  else . end;
	def maxof($a; $b): if $a == null or $b > $a then $b else $a end;

	reduce (inputs | fromjson? // empty) as $x ({};
		if $x.msg == "check round failed" then
			.[$x | kind].failed[$x | cause] += 1
		elif $x.msg == "check stream open failed" then
			.[$x | kind].open_failed += 1
		elif $x.msg == "check stream send failed" then
			.[$x | kind].send_failed += 1
		elif $x.msg == "health changed" and $x.reason == "unreachable" then
			# A health record names its kind in its record attribute; its
			# identity attributes are those of the record key.
			.[$x.record // ($x | kind)].unreachable += 1
		elif $x.msg == "late observation" then
			($x | kind) as $k
			| .[$k].late[if $x.stale then "stale" else "not_stale" end] += 1
			# The worker logs no_obs, true, only when the reply has no
			# observation, and leaves age_ms out only when the reply has
			# no answer time.
			| if $x.no_obs == true then
				.[$k].late.no_obs += 1
				| if $x.age_ms == null then .
				  else .[$k].late.no_obs_max_age_ms =
					maxof(.[$k].late.no_obs_max_age_ms; $x.age_ms)
				  end
			  elif $x.age_ms == null then .
			  else .[$k].late.max_age_ms = maxof(.[$k].late.max_age_ms; $x.age_ms)
			  end
		else . end)
	| . as $all
	| (["dn", "side", "cn", "cntlr"] as $order
	   | $order + ((keys - $order) | sort)) as $kinds
	| ([$kinds[] | $all[.].late.max_age_ms // empty] | max) as $top
	| ($kinds[] as $k
	   | ($all[$k] // {}) as $c
	   | "\($k):",
	     "  check round failed: \([$c.failed // {} | .[]] | add // 0)",
	     ($c.failed // {} | to_entries | sort_by(-.value)[]
	      | "    \(.value)  \(.key)"),
	     "  check stream open failed: \($c.open_failed // 0)",
	     "  check stream send failed: \($c.send_failed // 0)",
	     "  health changed to unreachable: \($c.unreachable // 0)",
	     "  late observation: stale \($c.late.stale // 0), not stale \($c.late.not_stale // 0); largest age of an observation \(if $c.late.max_age_ms == null then "none" else "\($c.late.max_age_ms) ms" end)",
	     "    of them with no observation: \($c.late.no_obs // 0); largest age \(if $c.late.no_obs_max_age_ms == null then "none" else "\($c.late.no_obs_max_age_ms) ms" end)",
	     ""),
	  "largest late observation age of any object: \(if $top == null then "none" else "\($top) ms" end)"'
