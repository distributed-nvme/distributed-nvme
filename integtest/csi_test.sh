#!/usr/bin/env bash
#
# csi_test.sh — the Kubernetes integration test of doc/csi.md (Integration
# test plan). Three of the lab's guests run Kubernetes: the control-plane
# guest runs the k3s server beside dnv's control plane, and the two hosts run
# the k3s agents, so that the pods that mount dnv volumes run on the two real
# kernel NVMe hosts. The dnv stack and the volume cases come later in the
# suite's life; this version proves the Kubernetes floor: the three guests
# are distinct machines, k3s is present or gets installed, and a pod runs on
# each host from an image the suite imports without a registry.
#
#   bash integtest/csi_test.sh \
#     --cp   yupeng@192.168.122.78 \
#     --host yupeng@192.168.122.193 --host yupeng@192.168.122.197
#
#   optional: [--only <case>] [--cleanup-only] [--slice-cnt N]
#             [--keep-logs DIR] [--cn user@ip …] [--dn user@ip …]
#
# Cases, in order: k8s. `--only` picks one. Cleanup runs at the START and, on
# success only, at the END (CSI5): a failing run leaves its namespace, pods
# and work directories in place and dumps diagnostics instead.
#
# ---------------------------------------------------------------------------
# ABSOLUTE RULES. Each guards a failure mode of this lab.
# ---------------------------------------------------------------------------
#
#  1. Kubernetes is installed ONLY when it is absent, and NEVER removed by
#     this suite (CSI1). k3s keeps running on the three guests between runs;
#     an operator removes it with the k3s uninstall scripts when the lab
#     needs a clean host.
#  2. The identity files /etc/nvme/hostnqn and /etc/nvme/hostid are generated
#     if absent and NEVER overwritten (CSI2), as in the e2e suite: the kernel
#     keeps a strict 1:1 hostnqn<->hostid map, and any other user of the
#     guest shares this identity. The two hosts' identities must differ, or
#     preflight dies naming the fix.
#  3. kubectl runs on the control-plane guest only, as the k3s binary's
#     subcommand over ssh (CSI3). The driver machine installs nothing.
#  4. Every image reaches a node by import into its k3s image store (CSI4),
#     and every pod this suite runs pulls nothing.
#  5. Every pkill lives in a helper FILE on the guest with a bracketed
#     pattern; `pkill -f` matches the wrapping shell of the ssh command
#     itself, so a pattern in an ssh command STRING kills its own shell
#     (e2e_integtest.md E2E8).
#  6. NEVER run this suite while any other dnv suite runs anywhere in the
#     lab: it shares the control-plane guest and both hosts with them, and a
#     run's cleanup cannot tell its own processes from another run's.
#  7. NEVER edit this file while it is running: bash re-seeks the script by
#     byte offset, so a length-changing edit kills the run with a bogus
#     syntax error at an unrelated line.
#
# Spec: doc/csi.md (rules CSI1..).

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (doc/csi.md, Topology and parameters)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"

# The pinned k3s release: the version the lab runs, named in doc/csi.md.
# The install script of the k3s project fetches exactly this release, and a
# guest that already runs another version fails preflight naming it (CSI1).
K3S_VERSION=v1.37.1+k3s1
K3S_INSTALL_URL=https://get.k3s.io
# The server's components the lab does not use, switched off to save the
# control-plane guest's memory: the ingress controller, the service load
# balancer and the metrics server.
K3S_SERVER_DISABLE="--disable traefik --disable servicelb --disable metrics-server"
# The taint that keeps test pods off the control-plane node (CSI6).
CP_TAINT_KEY=node-role.kubernetes.io/control-plane
CP_TAINT_EFFECT=NoSchedule
# The API port of the k3s server, which the agents join through.
K3S_API_PORT=6443

# Every Kubernetes object this suite creates lives in one namespace, so one
# delete removes them all (CSI5).
TEST_NS=dnv-csi-test
# The smoke image: built on the driver from a program the suite writes, and
# imported into every node (CSI4). containerd stores a bare name under the
# docker.io library, which is what `ctr images` lists and removes.
SMOKE_IMAGE=dnv-csi-smoke:it
SMOKE_IMAGE_REF=docker.io/library/dnv-csi-smoke:it
SMOKE_LINE="dnv-csi smoke ok"

# The work directory on every guest, and the shipped helper beside it. The
# name is no prefix of another suite's work directory and none is a prefix
# of it; the helper is a SIBLING of $WORK, never a child, since cleanup
# removes $WORK through the helper.
WORK=/var/tmp/dnv-csi
HELPER=/var/tmp/dnv-csi-helper.sh

# Timeouts, in seconds.
WAIT_K3S_INSTALL=900 # one install of the k3s server or agent, download included
WAIT_NODES=300       # every node Ready at the pinned version
WAIT_SYSTEM=300      # the cluster's own deployments available
WAIT_POD=240         # a smoke pod Running on its host
WAIT_NS_GONE=300     # the test namespace gone after its delete

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15
	-o ServerAliveCountMax=4 -o LogLevel=ERROR)

CASES=(k8s)

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------

CP=""
HOST=()
CN=()
DN=()
ONLY=""
CLEANUP_ONLY=0
SLICE_CNT=1
KEEP_LOGS=""
QUIET=0
STAGE="startup"
TRACE="-"
CASE="-"
SETUP_DONE=0
CLEANUP_DIRTY=0
RUN_CASES=()
# The hostnames of the three Kubernetes guests, read at preflight; k3s names
# a node after its hostname.
CP_NAME=""
HOST_NAME=()
# The smoke image's build directory on the driver, removed at exit.
SMOKE_DIR=""

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling (shapes from e2e_test.sh)
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

stage() { # <nn> <text>
	STAGE="$CASE: $2"
	TRACE="it-csi-$CASE-$1"
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

# on_exit runs the end cleanup of a NORMAL run, and only on success (CSI5).
on_exit() {
	local rc=$?
	trap - EXIT
	[ -z "$SMOKE_DIR" ] || rm -rf "$SMOKE_DIR"
	if [ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ] && [ -n "$KEEP_LOGS" ]; then
		keep_logs
	fi
	if [ "$rc" -eq 0 ] && [ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ]; then
		log ""
		log "=== end-of-run cleanup (success)"
		cleanup_all
		if [ "$CLEANUP_DIRTY" -eq 1 ]; then
			rc=1
		fi
	fi
	if [ "$rc" -eq 0 ]; then
		log ""
		log "PASS"
	else
		log ""
		log "########## diagnostics ##########"
		diagnostics || true
		log ""
		if [ "$CLEANUP_DIRTY" -eq 1 ]; then
			log "the cleanup did not finish; see the WARNING lines above"
		else
			log "debris left in place on the guests; failing stage '$STAGE'"
		fi
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution
#
# Every Kubernetes guest needs passwordless sudo: k3s, its image store and
# kubectl run as root, and the hosts need it for /etc/nvme too. The
# `sudo -n bash -c $(printf '%q' …)` shape is the e2e suite's: -n so a sudo
# that would prompt fails instead of hanging, %q so an argument with a space
# or a metacharacter is not re-split by the remote shell.
# ---------------------------------------------------------------------------

ssh_to() { # <target> <cmd…>
	local target=$1
	shift
	[ "$QUIET" -eq 1 ] || log "[${target##*@}] $*"
	ssh "${SSH_OPTS[@]}" "$target" "$*"
}

ssh_sudo() { # <target> <cmd…>
	local target=$1
	shift
	[ "$QUIET" -eq 1 ] || log "[${target##*@}] $*"
	ssh "${SSH_OPTS[@]}" "$target" "sudo -n bash -c $(printf '%q' "$*")"
}

ssh_cp() { ssh_to "$CP" "$@"; }
ssh_cp_sudo() { ssh_sudo "$CP" "$@"; }
ssh_cp_sudo_ok() { ssh_sudo "$CP" "$@" || true; }

ssh_host() { # <h> <cmd…>
	local h=$1
	shift
	ssh_sudo "${HOST[$h]}" "$@"
}
ssh_host_ok() { ssh_host "$@" || true; }

ssh_host_user() { # <h> <cmd…>
	local h=$1
	shift
	ssh_to "${HOST[$h]}" "$@"
}

helper_cp() { # <verb> [args…]
	ssh_cp_sudo "bash $HELPER $*"
}
helper_cp_ok() { helper_cp "$@" || true; }

helper_host() { # <h> <verb> [args…]
	local h=$1
	shift
	ssh_host "$h" "bash $HELPER $*"
}
helper_host_ok() { helper_host "$@" || true; }

# kc runs one kubectl command on the control-plane guest (CSI3). Each
# argument is quoted on its own, so an argument with spaces, such as a
# jsonpath expression, reaches kubectl as one word.
kc() { # <args…>
	local cmd
	cmd=$(printf '%q ' k3s kubectl "$@")
	ssh_cp_sudo "${cmd% }"
}
kc_ok() { kc "$@" || true; }

# kc_apply applies a manifest read from the driver's stdin.
kc_apply() {
	[ "$QUIET" -eq 1 ] || log "[${CP##*@}] k3s kubectl apply -f - (manifest on stdin)"
	ssh "${SSH_OPTS[@]}" "$CP" "sudo -n k3s kubectl apply -f -"
}

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
		sleep 2
	done
}

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing on the driver: $1"
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/csi_test.sh [--only <case>] [--cleanup-only]
           [--slice-cnt N] [--keep-logs DIR]
           --cp user@ip --host user@ip --host user@ip
           [--cn user@ip …] [--dn user@ip …]

roles (repeatable flags; nothing is positional, nothing is hardcoded):
  --cp    exactly 1   the k3s server, beside dnv's control plane
                      (passwordless sudo)
  --host  exactly 2   the k3s agents: real kernel NVMe hosts that run the
                      pods (passwordless sudo)
  --cn, --dn          controller-node and disk-node guests of the dnv stack
                      the volume cases build (accepted, unused by this
                      version's one case)

parameters:
  --slice-cnt N       slices per storage pool of the volume cases (default
                      $SLICE_CNT)
  --only <case>       run one case: ${CASES[*]}
  --cleanup-only      run the start cleanup on every guest and stop
  --keep-logs DIR     copy the k3s journals of the three Kubernetes guests
                      into DIR/<role>-<ip>/ at the end of the run

Every guest must be a DIFFERENT machine, and no other dnv suite may run
anywhere in the lab while this one does.
EOF
	exit 2
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--cp)
			{ [ $# -ge 2 ] && [ -z "$CP" ]; } || usage
			CP=$2
			shift 2
			;;
		--cp=*)
			[ -z "$CP" ] || usage
			CP=${1#--cp=}
			shift
			;;
		--host)
			[ $# -ge 2 ] || usage
			HOST+=("$2")
			shift 2
			;;
		--host=*)
			HOST+=("${1#--host=}")
			shift
			;;
		--cn)
			[ $# -ge 2 ] || usage
			CN+=("$2")
			shift 2
			;;
		--cn=*)
			CN+=("${1#--cn=}")
			shift
			;;
		--dn)
			[ $# -ge 2 ] || usage
			DN+=("$2")
			shift 2
			;;
		--dn=*)
			DN+=("${1#--dn=}")
			shift
			;;
		--only)
			[ $# -ge 2 ] || usage
			ONLY=$2
			shift 2
			;;
		--only=*)
			ONLY=${1#--only=}
			shift
			;;
		--slice-cnt)
			[ $# -ge 2 ] || usage
			SLICE_CNT=$2
			shift 2
			;;
		--slice-cnt=*)
			SLICE_CNT=${1#--slice-cnt=}
			shift
			;;
		--keep-logs)
			[ $# -ge 2 ] || usage
			KEEP_LOGS=$2
			shift 2
			;;
		--keep-logs=*)
			KEEP_LOGS=${1#--keep-logs=}
			shift
			;;
		--cleanup-only)
			CLEANUP_ONLY=1
			shift
			;;
		-h | --help) usage ;;
		*)
			log "unknown argument: $1"
			usage
			;;
		esac
	done
	[ -n "$CP" ] || {
		log "--cp is required"
		usage
	}
	[ "${#HOST[@]}" -eq 2 ] || {
		log "exactly two --host are required (got ${#HOST[@]})"
		usage
	}
	case "$SLICE_CNT" in
	'' | *[!0-9]* | 0*) die "--slice-cnt must be a positive integer, got '$SLICE_CNT'" ;;
	esac
	if [ -n "$ONLY" ]; then
		local c found=0
		for c in "${CASES[@]}"; do
			[ "$c" = "$ONLY" ] && found=1
		done
		[ "$found" -eq 1 ] || die "--only '$ONLY' names no case of ${CASES[*]}"
	fi
	# Every guest is a different machine.
	local all=("$CP" "${HOST[@]}" "${CN[@]}" "${DN[@]}") i j
	for i in "${!all[@]}"; do
		for j in "${!all[@]}"; do
			[ "$i" -lt "$j" ] || continue
			[ "${all[$i]##*@}" != "${all[$j]##*@}" ] ||
				die "the guest ${all[$i]##*@} is named in two roles"
		done
	done
}

log_topology() {
	log "=== topology"
	log "  cp    $CP"
	local h
	for h in "${!HOST[@]}"; do
		log "  host$h $(printf '%s' "${HOST[$h]}")"
	done
	[ "${#CN[@]}" -eq 0 ] || log "  cn    ${CN[*]} (unused by this version)"
	[ "${#DN[@]}" -eq 0 ] || log "  dn    ${DN[*]} (unused by this version)"
	log "  k3s   $K3S_VERSION, namespace $TEST_NS, image $SMOKE_IMAGE"
	log "  cases ${ONLY:-${CASES[*]}}"
}

# ---------------------------------------------------------------------------
# The shipped helper: one script for every Kubernetes guest, generated here,
# syntax-checked on the driver and copied to $HELPER on every run.
# ---------------------------------------------------------------------------

helper_source() {
	cat <<EOF
#!/usr/bin/env bash
#
# dnv-csi-helper.sh — the guest half of integtest/csi_test.sh, GENERATED by
# that suite and shipped to every Kubernetes guest as $HELPER on every run.
# Editing it here changes nothing.
set -uo pipefail

WORK="$WORK"
K3S_INSTALL_URL="$K3S_INSTALL_URL"
SMOKE_IMAGE_REF="$SMOKE_IMAGE_REF"
EOF
	cat <<'HELPER_EOF'

# --- the guest's facts --------------------------------------------------------

# facts prints what preflight compares across the three guests: the
# hostname, the machine-id, the memory and, when the files exist, the NVMe
# identity.
facts() {
	printf 'hostname=%s\n' "$(hostname)"
	printf 'machine_id=%s\n' "$(cat /etc/machine-id 2>/dev/null)"
	printf 'mem_total_kib=%s\n' "$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
	printf 'multipath=%s\n' "$(cat /sys/module/nvme_core/parameters/multipath 2>/dev/null || echo MISSING)"
	printf 'hostnqn=%s\n' "$(cat /etc/nvme/hostnqn 2>/dev/null)"
	printf 'hostid=%s\n' "$(cat /etc/nvme/hostid 2>/dev/null)"
	return 0
}

# identity generates /etc/nvme/hostnqn and /etc/nvme/hostid if they are
# absent and prints both. GENERATE-IF-ABSENT and never overwrite (CSI2).
identity() {
	mkdir -p /etc/nvme
	[ -s /etc/nvme/hostnqn ] || nvme gen-hostnqn >/etc/nvme/hostnqn
	[ -s /etc/nvme/hostid ] || uuidgen >/etc/nvme/hostid
	if [ ! -s /etc/nvme/hostnqn ] || [ ! -s /etc/nvme/hostid ]; then
		echo "identity: /etc/nvme/hostnqn or /etc/nvme/hostid is still empty" >&2
		return 1
	fi
	printf 'hostnqn=%s\n' "$(cat /etc/nvme/hostnqn)"
	printf 'hostid=%s\n' "$(cat /etc/nvme/hostid)"
	return 0
}

mkwork() {
	mkdir -p "$WORK" && chmod 0777 "$WORK"
	echo mkwork-done
}

rmwork() {
	rm -rf "$WORK"
	echo rmwork-done
}

# --- k3s ---------------------------------------------------------------------

# k3s_state prints the installed version, or none, and the state of the two
# units k3s installs: `k3s` on a server, `k3s-agent` on an agent.
k3s_state() {
	local ver=none u
	if command -v k3s >/dev/null 2>&1; then
		ver=$(k3s --version 2>/dev/null | awk 'NR==1{print $3}')
		[ -n "$ver" ] || ver=unreadable
	fi
	printf 'version=%s\n' "$ver"
	for u in k3s k3s-agent; do
		printf '%s=%s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"
	done
	return 0
}

# k3s_fetch_installer downloads the k3s project's install script once per
# run into $WORK, so the server and the agents run the same script.
k3s_fetch_installer() {
	mkdir -p "$WORK"
	curl -sfL "$K3S_INSTALL_URL" -o "$WORK/k3s-install.sh" || {
		echo "k3s_fetch_installer: downloading $K3S_INSTALL_URL failed" >&2
		return 1
	}
	chmod 0755 "$WORK/k3s-install.sh"
	echo fetched
}

# k3s_install_server installs the pinned k3s server (CSI1). The remaining
# arguments are the server's flags.
k3s_install_server() { # <version> <flags…>
	local ver=$1
	shift
	k3s_fetch_installer >/dev/null || return 1
	INSTALL_K3S_VERSION="$ver" sh "$WORK/k3s-install.sh" server "$@" || {
		echo "k3s_install_server: the install script failed" >&2
		return 1
	}
	echo installed
}

# k3s_token prints the server's join token. The driver pipes it into
# k3s_install_agent's stdin, so it never appears in a command line or a log.
k3s_token() {
	cat /var/lib/rancher/k3s/server/node-token
}

# k3s_install_agent installs the pinned k3s agent (CSI1), joining the server
# at <url> with the token read from stdin.
k3s_install_agent() { # <version> <url>   (token on stdin)
	local ver=$1 url=$2 token
	read -r token
	[ -n "$token" ] || {
		echo "k3s_install_agent: no token on stdin" >&2
		return 1
	}
	k3s_fetch_installer >/dev/null || return 1
	INSTALL_K3S_VERSION="$ver" K3S_URL="$url" K3S_TOKEN="$token" \
		sh "$WORK/k3s-install.sh" agent || {
		echo "k3s_install_agent: the install script failed" >&2
		return 1
	}
	echo installed
}

# --- images -------------------------------------------------------------------

# image_import reads an image archive from stdin into this node's k3s image
# store (CSI4).
image_import() {
	k3s ctr images import - >/dev/null || {
		echo "image_import: ctr images import failed" >&2
		return 1
	}
	echo imported
}

# image_present answers whether the smoke image is in the store.
image_present() {
	k3s ctr images ls -q 2>/dev/null | grep -qx "$SMOKE_IMAGE_REF"
}

# image_rm removes the smoke image from the store, tolerating absence.
image_rm() {
	if command -v k3s >/dev/null 2>&1 && image_present; then
		k3s ctr images rm "$SMOKE_IMAGE_REF" >/dev/null 2>&1 || true
	fi
	echo image-rm-done
}

# --- diagnostics ---------------------------------------------------------------

# journal prints the tail of the k3s units' journal.
journal() { # [lines]
	local n=${1:-80}
	journalctl -u k3s -u k3s-agent --no-pager -n "$n" 2>/dev/null || true
}

diag() {
	echo "--- $(hostname): facts"
	facts
	echo "--- $(hostname): k3s"
	k3s_state
	echo "--- $(hostname): memory"
	free -m
	echo "--- $(hostname): k3s journal (last 40 lines)"
	journal 40
	return 0
}

# --- dispatch -------------------------------------------------------------------

__verb=${1:-}
if [ -n "$__verb" ] && declare -F "$__verb" >/dev/null 2>&1; then
	"$@"
	exit $?
fi
if [ -z "$__verb" ]; then
	echo "dnv-csi-helper.sh: no verb given" >&2
else
	echo "dnv-csi-helper.sh: unknown verb '$__verb'" >&2
fi
echo "known verbs:" >&2
declare -F | awk '{print "  " $3}' >&2
exit 2
HELPER_EOF
}

ship_helpers() {
	local tmp h
	tmp=$(mktemp) || die "mktemp on the driver failed"
	helper_source >"$tmp" || die "generating the helper failed"
	bash -n "$tmp" || die "the generated helper is not valid bash"
	scp -q "${SSH_OPTS[@]}" "$tmp" "$CP:$HELPER" ||
		die "scp of the helper to cp failed"
	for h in "${!HOST[@]}"; do
		scp -q "${SSH_OPTS[@]}" "$tmp" "${HOST[$h]}:$HELPER" ||
			die "scp of the helper to host$h failed"
	done
	rm -f "$tmp"
	log "  helper shipped to $HELPER on cp and both hosts"
}

# ---------------------------------------------------------------------------
# Preflight (doc/csi.md, Preflight)
# ---------------------------------------------------------------------------

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp docker mktemp timeout awk sed grep; do
		need_local "$tool"
	done
	docker info >/dev/null 2>&1 ||
		die "docker on the driver is not usable (docker info failed)"
	log "preflight (driver) ok"
}

# fact_of reads one key out of the helper's facts output.
fact_of() { # <facts output> <key>
	printf '%s\n' "$1" | awk -F= -v k="$2" '$1==k {sub(/^[^=]*=/, ""); print; exit}'
}

# preflight_guests proves that the three Kubernetes guests are distinct
# machines a Kubernetes node can be: passwordless sudo, unique hostnames
# and machine-ids, the memory floor and native multipath on the hosts, and
# the hosts' NVMe identities present and different (CSI2).
preflight_guests() {
	STAGE="preflight (guests)"
	log ""
	log "=== preflight: guests"
	local out h names="" ids="" nqns="" mem
	ssh_cp "sudo -n true" >/dev/null ||
		die "cp ($CP): passwordless sudo is required (k3s and kubectl run as root)"
	out=$(helper_cp facts) || die "cp: reading its facts failed"
	CP_NAME=$(fact_of "$out" hostname)
	[ -n "$CP_NAME" ] || die "cp: empty hostname"
	names="$CP_NAME"
	ids=$(fact_of "$out" machine_id)
	[ -n "$ids" ] || die "cp: empty machine-id"
	log "  cp: hostname $CP_NAME"
	for h in "${!HOST[@]}"; do
		ssh_host "$h" "true" >/dev/null ||
			die "host$h (${HOST[$h]}): passwordless sudo is required"
		out=$(helper_host "$h" identity) ||
			die "host$h: generating /etc/nvme/hostnqn and /etc/nvme/hostid failed: $out"
		out=$(helper_host "$h" facts) || die "host$h: reading its facts failed"
		HOST_NAME[$h]=$(fact_of "$out" hostname)
		[ -n "${HOST_NAME[$h]}" ] || die "host$h: empty hostname"
		names="$names $(fact_of "$out" hostname)"
		ids="$ids $(fact_of "$out" machine_id)"
		nqns="$nqns $(fact_of "$out" hostnqn)"
		assert_eq "$(fact_of "$out" multipath)" Y "host$h: nvme_core.multipath"
		mem=$(fact_of "$out" mem_total_kib)
		# Three GiB: a kubelet, its runtime and the node plugin leave a pod
		# too little below that.
		assert_ge "$mem" 3000000 "host$h: MemTotal in KiB (raise the guest's memory)"
		log "  host$h: hostname ${HOST_NAME[$h]}, $((mem / 1024)) MiB, $(fact_of "$out" hostnqn)"
	done
	assert_eq "$(printf '%s\n' $names | sort -u | wc -l)" 3 \
		"the three guests' hostnames are distinct ($names)"
	assert_eq "$(printf '%s\n' $ids | sort -u | wc -l)" 3 \
		"the three guests' machine-ids are distinct"
	assert_eq "$(printf '%s\n' $nqns | sort -u | wc -l)" 2 \
		"the two hosts' host NQNs differ ($nqns); regenerate one with nvme gen-hostnqn and uuidgen"
	log "preflight (guests) ok"
}

# ---------------------------------------------------------------------------
# k3s: present or installed (CSI1), then the cluster's shape (CSI6)
# ---------------------------------------------------------------------------

k3s_version_of() { # <state output>
	printf '%s\n' "$1" | awk -F= '$1=="version"{print $2; exit}'
}

k3s_unit_of() { # <state output> <unit>
	printf '%s\n' "$1" | awk -F= -v u="$2" '$1==u{print $2; exit}'
}

# k3s_ensure installs the server and the agents where they are absent, and
# refuses a guest that runs another version: the pin is the version this
# suite proves, and a mix of versions proves nothing.
k3s_ensure() {
	STAGE="k3s"
	log ""
	log "=== k3s $K3S_VERSION: present or installed"
	local out ver h token cp_ip=${CP##*@}
	out=$(helper_cp k3s_state) || die "cp: reading the k3s state failed"
	ver=$(k3s_version_of "$out")
	if [ "$ver" = none ]; then
		log "  cp: no k3s, installing the server (up to ${WAIT_K3S_INSTALL}s)"
		# shellcheck disable=SC2086 # the server flags are separate words
		out=$(timeout "$WAIT_K3S_INSTALL" ssh "${SSH_OPTS[@]}" "$CP" \
			"sudo -n bash $HELPER k3s_install_server $K3S_VERSION $K3S_SERVER_DISABLE --node-taint $CP_TAINT_KEY:$CP_TAINT_EFFECT" 2>&1) ||
			die "cp: installing the k3s server failed: $out"
		out=$(helper_cp k3s_state) || die "cp: reading the k3s state failed"
		ver=$(k3s_version_of "$out")
	fi
	assert_eq "$ver" "$K3S_VERSION" \
		"cp: the installed k3s version (uninstall it with k3s-uninstall.sh or change the pin)"
	assert_eq "$(k3s_unit_of "$out" k3s)" active "cp: the k3s server unit"
	token=$(QUIET=1 helper_cp k3s_token) || die "cp: reading the k3s join token failed"
	[ -n "$token" ] || die "cp: the k3s join token is empty"
	for h in "${!HOST[@]}"; do
		out=$(helper_host "$h" k3s_state) || die "host$h: reading the k3s state failed"
		ver=$(k3s_version_of "$out")
		if [ "$ver" = none ]; then
			log "  host$h: no k3s, installing the agent (up to ${WAIT_K3S_INSTALL}s)"
			out=$(printf '%s\n' "$token" | timeout "$WAIT_K3S_INSTALL" ssh "${SSH_OPTS[@]}" "${HOST[$h]}" \
				"sudo -n bash $HELPER k3s_install_agent $K3S_VERSION https://$cp_ip:$K3S_API_PORT" 2>&1) ||
				die "host$h: installing the k3s agent failed: $out"
			out=$(helper_host "$h" k3s_state) || die "host$h: reading the k3s state failed"
			ver=$(k3s_version_of "$out")
		fi
		assert_eq "$ver" "$K3S_VERSION" \
			"host$h: the installed k3s version (uninstall it with k3s-agent-uninstall.sh or change the pin)"
		assert_eq "$(k3s_unit_of "$out" k3s-agent)" active "host$h: the k3s agent unit"
	done
	log "  waiting for the three nodes Ready at $K3S_VERSION (up to ${WAIT_NODES}s)"
	wait_until "$WAIT_NODES" "nodes $CP_NAME ${HOST_NAME[*]} Ready at $K3S_VERSION" nodes_ready
	# The taint (CSI6), ensured on every run: an install that predates it
	# gets it here, and a run after it changes nothing.
	kc taint node "$CP_NAME" "$CP_TAINT_KEY:$CP_TAINT_EFFECT" --overwrite >/dev/null ||
		die "cp: tainting the control-plane node failed"
	log "  waiting for the cluster's own deployments (up to ${WAIT_SYSTEM}s)"
	kc -n kube-system rollout status deploy/coredns "--timeout=${WAIT_SYSTEM}s" >/dev/null ||
		die "coredns did not become available"
	log "  nodes:"
	kc get nodes -o wide >&2 || true
}

# nodes_ready answers whether every expected node is Ready at the pin.
nodes_ready() {
	local out name want
	out=$(kc get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{" "}{.status.nodeInfo.kubeletVersion}{"\n"}{end}' 2>/dev/null) || return 1
	for want in "$CP_NAME" "${HOST_NAME[@]}"; do
		printf '%s\n' "$out" | grep -qxF "$want True $K3S_VERSION" || return 1
	done
	name=$(printf '%s\n' "$out" | grep -c . || true)
	[ "$name" -eq $((1 + ${#HOST[@]})) ]
}

# ---------------------------------------------------------------------------
# The smoke image (CSI4)
# ---------------------------------------------------------------------------

# smoke_image_build writes a program that prints one line and sleeps, builds
# it static for linux/amd64, and wraps it in an image from scratch.
smoke_image_build() {
	SMOKE_DIR=$(mktemp -d) || die "mktemp -d on the driver failed"
	cat >"$SMOKE_DIR/main.go" <<EOF
package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("$SMOKE_LINE")
	for {
		time.Sleep(time.Hour)
	}
}
EOF
	printf 'module smoke\n\ngo 1.26\n' >"$SMOKE_DIR/go.mod"
	(cd "$SMOKE_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o smoke . >&2) ||
		die "building the smoke program failed"
	printf 'FROM scratch\nCOPY smoke /smoke\nENTRYPOINT ["/smoke"]\n' >"$SMOKE_DIR/Dockerfile"
	docker build -q -t "$SMOKE_IMAGE" "$SMOKE_DIR" >/dev/null 2>"$SMOKE_DIR/build.err" ||
		die "docker build of $SMOKE_IMAGE failed: $(cat "$SMOKE_DIR/build.err")"
	log "  built $SMOKE_IMAGE on the driver"
}

# smoke_image_import ships the image into every node's store.
smoke_image_import() {
	local target label out
	for target in "$CP" "${HOST[@]}"; do
		label=${target##*@}
		log "[$label] docker save $SMOKE_IMAGE | k3s ctr images import -"
		out=$(docker save "$SMOKE_IMAGE" | ssh "${SSH_OPTS[@]}" "$target" "sudo -n bash $HELPER image_import" 2>&1) ||
			die "$label: importing $SMOKE_IMAGE failed: $out"
		assert_eq "$out" imported "$label: image_import's answer"
	done
}

# ---------------------------------------------------------------------------
# Cleanup (CSI5): everything this suite started, and nothing else
# ---------------------------------------------------------------------------

ns_gone() {
	! kc get namespace "$TEST_NS" >/dev/null 2>&1
}

cleanup_all() {
	local out h ver
	CLEANUP_DIRTY=0
	out=$(QUIET=1 helper_cp_ok k3s_state 2>/dev/null) || true
	ver=$(k3s_version_of "$out")
	if [ -n "$ver" ] && [ "$ver" != none ] && [ "$(k3s_unit_of "$out" k3s)" = active ]; then
		kc_ok delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1
		if ! wait_cleanup "$WAIT_NS_GONE" ns_gone; then
			log "WARNING: the namespace $TEST_NS is still there after ${WAIT_NS_GONE}s"
			CLEANUP_DIRTY=1
		fi
	else
		log "  cp: no running k3s server, nothing to delete in Kubernetes"
	fi
	# The verbs run quiet: their answer is the sentinel on stdout, and the
	# command's own log line would otherwise be captured with it.
	out=$(QUIET=1 helper_cp_ok image_rm 2>&1) || true
	[ "$out" = image-rm-done ] || {
		log "WARNING: cp: image_rm did not finish: $out"
		CLEANUP_DIRTY=1
	}
	for h in "${!HOST[@]}"; do
		out=$(QUIET=1 helper_host_ok "$h" image_rm 2>&1) || true
		[ "$out" = image-rm-done ] || {
			log "WARNING: host$h: image_rm did not finish: $out"
			CLEANUP_DIRTY=1
		}
	done
	out=$(QUIET=1 helper_cp_ok rmwork 2>&1) || true
	[ "$out" = rmwork-done ] || {
		log "WARNING: cp: rmwork did not finish: $out"
		CLEANUP_DIRTY=1
	}
	for h in "${!HOST[@]}"; do
		out=$(QUIET=1 helper_host_ok "$h" rmwork 2>&1) || true
		[ "$out" = rmwork-done ] || {
			log "WARNING: host$h: rmwork did not finish: $out"
			CLEANUP_DIRTY=1
		}
	done
	docker image rm "$SMOKE_IMAGE" >/dev/null 2>&1 || true
	log "  cleanup done (dirty=$CLEANUP_DIRTY)"
}

# wait_cleanup is wait_until without the die: cleanup reports, never dies.
wait_cleanup() { # <secs> <cmd…>
	local secs=$1
	shift
	local deadline=$((SECONDS + secs)) saved=$QUIET
	QUIET=1
	while :; do
		if "$@"; then
			QUIET=$saved
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			QUIET=$saved
			return 1
		fi
		sleep 2
	done
}

# ---------------------------------------------------------------------------
# Diagnostics and kept logs
# ---------------------------------------------------------------------------

diagnostics() {
	local h
	QUIET=1
	log "--- kubectl get nodes -o wide"
	kc_ok get nodes -o wide >&2
	log "--- kubectl get pods -A -o wide"
	kc_ok get pods -A -o wide >&2
	log "--- kubectl describe pods -n $TEST_NS"
	kc_ok describe pods -n "$TEST_NS" >&2
	log "--- kubectl get events -n $TEST_NS"
	kc_ok get events -n "$TEST_NS" --sort-by=.lastTimestamp >&2
	helper_cp_ok diag >&2
	for h in "${!HOST[@]}"; do
		helper_host_ok "$h" diag >&2
	done
	QUIET=0
}

keep_logs() {
	local dir h
	dir="$KEEP_LOGS/cp-${CP##*@}"
	mkdir -p "$dir" && QUIET=1 helper_cp_ok journal 2000 >"$dir/k3s.log" 2>/dev/null || true
	for h in "${!HOST[@]}"; do
		dir="$KEEP_LOGS/host$h-${HOST[$h]##*@}"
		mkdir -p "$dir" && QUIET=1 helper_host_ok "$h" journal 2000 >"$dir/k3s.log" 2>/dev/null || true
	done
	QUIET=0
	log "  k3s journals kept under $KEEP_LOGS"
}

# ---------------------------------------------------------------------------
# The cases (doc/csi.md, The cases)
# ---------------------------------------------------------------------------

pod_name() { printf 'smoke-host%s' "$1"; }

pod_phase() { # <pod>
	kc get pod -n "$TEST_NS" "$1" -o 'jsonpath={.status.phase}' 2>/dev/null
}

pod_running() { # <pod>
	[ "$(pod_phase "$1")" = Running ]
}

pod_node() { # <pod>
	kc get pod -n "$TEST_NS" "$1" -o 'jsonpath={.spec.nodeName}' 2>/dev/null
}

# case_k8s proves the Kubernetes floor: the smoke image, imported into every
# node, runs as a pod on each host, prints its line, and the namespace's
# delete removes every pod.
case_k8s() {
	CASE=k8s
	local h pod out

	stage 01 "build the smoke image on the driver and import it into every node"
	SETUP_DONE=1
	smoke_image_build
	smoke_image_import

	stage 02 "a pod on each host from the imported image"
	kc_apply <<EOF || die "applying the namespace and the smoke pods failed"
apiVersion: v1
kind: Namespace
metadata:
  name: $TEST_NS
---
apiVersion: v1
kind: Pod
metadata:
  name: $(pod_name 0)
  namespace: $TEST_NS
spec:
  nodeSelector:
    kubernetes.io/hostname: ${HOST_NAME[0]}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: smoke
    image: $SMOKE_IMAGE
    imagePullPolicy: Never
---
apiVersion: v1
kind: Pod
metadata:
  name: $(pod_name 1)
  namespace: $TEST_NS
spec:
  nodeSelector:
    kubernetes.io/hostname: ${HOST_NAME[1]}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: smoke
    image: $SMOKE_IMAGE
    imagePullPolicy: Never
EOF
	for h in "${!HOST[@]}"; do
		pod=$(pod_name "$h")
		wait_until "$WAIT_POD" "pod $pod Running" pod_running "$pod"
		assert_eq "$(pod_node "$pod")" "${HOST_NAME[$h]}" "the node of $pod"
	done

	stage 03 "each pod printed its line"
	for h in "${!HOST[@]}"; do
		pod=$(pod_name "$h")
		out=$(kc logs -n "$TEST_NS" "$pod") || die "reading the logs of $pod failed"
		assert_eq "$out" "$SMOKE_LINE" "the log of $pod"
	done

	stage 04 "the namespace's delete removes both pods"
	kc delete namespace "$TEST_NS" --wait=false >/dev/null ||
		die "deleting the namespace $TEST_NS failed"
	wait_until "$WAIT_NS_GONE" "namespace $TEST_NS gone" ns_gone
	for h in "${!HOST[@]}"; do
		pod=$(pod_name "$h")
		out=$(kc_ok get pod -n "$TEST_NS" "$pod" -o name 2>/dev/null)
		assert_eq "$out" "" "$pod after the namespace's delete"
	done
	log "=== $CASE: PASS"
}

run_case() { # <name>
	"case_$1"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
	parse_args "$@"
	trap on_exit EXIT
	log_topology

	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		log ""
		log "=== --cleanup-only: the start cleanup on every guest, then stop"
		ship_helpers
		cleanup_all
		[ "$CLEANUP_DIRTY" -eq 0 ] || return 1
		return 0
	fi

	preflight_driver
	ship_helpers
	preflight_guests

	STAGE="start cleanup"
	log ""
	log "=== start cleanup (CSI5, unconditional, tolerant of total absence)"
	cleanup_all
	[ "$CLEANUP_DIRTY" -eq 0 ] || die "the start cleanup did not finish"

	k3s_ensure

	local name
	RUN_CASES=()
	for name in "${CASES[@]}"; do
		if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then
			continue
		fi
		RUN_CASES+=("$name")
	done
	[ "${#RUN_CASES[@]}" -ge 1 ] || die "no case to run"
	for name in "${RUN_CASES[@]}"; do
		run_case "$name"
	done
	return 0
}

main "$@"
