#!/usr/bin/env bash
#
# csi_test.sh — the Kubernetes integration test of doc/csi.md (Integration
# test plan). Three of the lab's guests run Kubernetes: the control-plane
# guest runs the k3s server beside dnv's control plane, and the two hosts run
# the k3s agents, so that the pods that mount dnv volumes run on the two real
# kernel NVMe hosts. The suite deploys the dnv stack itself, as the e2e suite
# does: etcd, dnv-gateway, dnv-worker and dnv-cdc on the control-plane guest,
# one `dnv-agent dn` per disk-node guest over a loop device, one
# `dnv-agent cn` per controller-node guest, and a cluster with every node
# registered. It never builds a pool: the driver does, one per volume.
#
#   bash integtest/csi_test.sh \
#     --cp   yupeng@192.168.122.78 \
#     --cn   yupeng@192.168.122.125 --cn yupeng@192.168.122.229 --cn yupeng@192.168.122.77 \
#     --dn   yupeng@192.168.122.48  --dn yupeng@192.168.122.70  --dn yupeng@192.168.122.49 --dn yupeng@192.168.122.122 \
#     --host yupeng@192.168.122.193 --host yupeng@192.168.122.197
#
#   optional: [--only <case>] [--cleanup-only] [--slice-cnt N] [--keep-logs DIR]
#
# Cases, in order: k8s, volume, failover, many, sanity. `--only` picks one.
# Cleanup runs at the START and, on success only, at the END (CSI5): a
# failing run leaves its pods, pools, processes and work directories in place
# and dumps diagnostics instead.
#
# ---------------------------------------------------------------------------
# ABSOLUTE RULES. Each guards a failure mode of this lab.
# ---------------------------------------------------------------------------
#
#  1. Kubernetes is installed ONLY when it is absent, and NEVER removed by
#     this suite (CSI1). k3s keeps running on the three guests between runs;
#     an operator removes it with the k3s uninstall scripts.
#  2. The identity files /etc/nvme/hostnqn and /etc/nvme/hostid are generated
#     if absent and NEVER overwritten (CSI2): the kernel keeps a strict 1:1
#     hostnqn<->hostid map, and any other user of the guest shares this
#     identity. The two hosts' identities must differ, or preflight dies.
#  3. kubectl runs on the control-plane guest only, as the k3s binary's
#     subcommand over ssh (CSI3). The driver machine installs nothing.
#  4. The suite's images, the driver's and the smoke image, reach a node by
#     import into its k3s image store (CSI4), and no pod pulls them; the IO
#     pods run the driver's own image, which carries the tools they need.
#     The sidecar images the manifests name come from their public registry.
#  5. Every pkill lives in the helper FILE on the guest with a bracketed
#     pattern; `pkill -f` matches the wrapping shell of the ssh command
#     itself, so a pattern in an ssh command STRING kills its own shell
#     (e2e_integtest.md E2E8).
#  6. NEVER pass iflag= or oflag= to dd (uutils dd 0.8.0 on the guests);
#     writes use conv=fsync.
#  7. nvmet teardown order: ana_groups 3 and 2 BEFORE the port, or the rmdir
#     fails and the leftover port fails the NEXT suite's setup.
#  8. NEVER run this suite while any other dnv suite runs anywhere in the
#     lab: it occupies all ten guests.
#  9. NEVER edit this file while it is running: bash re-seeks the script by
#     byte offset, so a length-changing edit kills the run.
#
# Spec: doc/csi.md (rules CSI1..).

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (doc/csi.md, Topology and parameters)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
DEPLOY_DIR="$REPO_ROOT/deploy"

AGENT_BIN="$REPO_ROOT/bin/dnv-agent"
GATEWAY_BIN="$REPO_ROOT/bin/dnv-gateway"
WORKER_BIN="$REPO_ROOT/bin/dnv-worker"
CDC_BIN="$REPO_ROOT/bin/dnv-cdc"
DNVCTL_BIN="$REPO_ROOT/bin/dnvctl"
CSI_BIN="$REPO_ROOT/bin/dnv-csi"
WORKERCTL_BIN="$BIN_DIR/workerctl"
# csi-sanity, the upstream conformance test, built once into integtest/bin
# from its pinned module in a module of its own, so the tree's go.mod never
# carries it.
SANITY_BIN="$BIN_DIR/csi-sanity"
SANITY_MODULE="github.com/kubernetes-csi/csi-test/v5"
SANITY_VERSION="v5.6.0"

# The pinned etcd release and its download come from the shared file.
. "$REPO_ROOT/integtest/fetch_etcd.sh"

# common.EtcdMaxTxnOps and common.EtcdMaxRequestBytes, read at preflight from
# `workerctl constants`, never typed (the other suites do the same).
ETCD_MAX_TXN_OPS=
ETCD_MAX_REQUEST_BYTES=

# The pinned k3s release the lab runs (CSI1).
K3S_VERSION=v1.37.1+k3s1
K3S_INSTALL_URL=https://get.k3s.io
K3S_SERVER_DISABLE="--disable traefik --disable servicelb --disable metrics-server"
CP_TAINT_KEY=node-role.kubernetes.io/control-plane
CP_TAINT_EFFECT=NoSchedule
K3S_API_PORT=6443

# The driver as the manifests deploy it: its name, its namespace, its image
# (read from the controller manifest, so the suite and the manifests agree),
# and the storage class and namespace the cases use.
DRIVER_NAME=csi.dnv.io
DRIVER_NS=dnv-csi
TEST_NS=dnv-csi-test
SC_NAME=dnv-csi-it
IMAGE=$(awk '/image: dnv-csi:/ {print $2; exit}' "$DEPLOY_DIR/controller.yaml")
IMAGE_REF="docker.io/library/$IMAGE"
# The smoke image of the k8s case.
SMOKE_IMAGE=dnv-csi-smoke:it
SMOKE_IMAGE_REF=docker.io/library/dnv-csi-smoke:it
SMOKE_LINE="dnv-csi smoke ok"

# The work directory on every guest, and the shipped helper beside it: no
# other suite's work directory is a prefix of this one and none is a prefix
# of it; the helper is a SIBLING of $WORK, since cleanup removes $WORK through
# the helper.
WORK=/var/tmp/dnv-csi
HELPER=/var/tmp/dnv-csi-helper.sh
NVMET=/sys/kernel/config/nvmet

# The dnv stack's ports: this suite's own block, bound by no other suite
# (e2e_test.sh's inventory lists theirs). k3s holds its own well-known ports
# on the three Kubernetes guests.
ETCD_CLIENT_PORT=17379
ETCD_PEER_PORT=17380
GW_PORT=29870
CDC_PORT=18030
DN_GRPC_PORT=28900
DN_TRSVCID=4400
DN_PORT_ID=1
CN_GRPC_PORT=28950
CN_TRSVCID=4400
CN_PORT_ID=1
# port_drop refuses an nvmet port outside this band: the agents of this suite
# listen on DN_TRSVCID and CN_TRSVCID, the dn and cn suites on 4200, the e2e
# suite on 4300..4349, the cdc suite on 14420..14423.
TRSVCID_MIN=4400
TRSVCID_MAX=4449

ETCD_NAME=dnv-csi-it
CLUSTER=csi
# 64 MiB extents, as the e2e suite's lab, so a small volume costs few.
EXTENT_SIZE=67108864
# Each disk node's loop device: sparse, allocated as the pools write.
BACKING_SIZE=8G
# dnv's own NQN prefix, which the agents mint, and the driver's.
NQN_PREFIX=nqn.2024-01.io.dnv
NQN_CSI=nqn.2024-01.io.dnv.csi
WORKER_ROLES="dn cn sp"
VOTE_INTERVAL=2
VOTE_GRACE=6

# Timeouts, in seconds.
WAIT_K3S_INSTALL=900
WAIT_NODES=300
WAIT_SYSTEM=300
WAIT_SHORT=15
WAIT_CP_READY=30
WAIT_AGENT=30
WAIT_PROVISION=300
WAIT_ROLLOUT=600 # the sidecar images are pulled on the first run
WAIT_PVC=240
WAIT_CAPACITY=180 # the provisioner polls the capacity every minute
WAIT_POD=300
WAIT_DRAIN=300
WAIT_FAILOVER=180
WAIT_NS_GONE=300
WAIT_HOST_CLEAN=120
SANITY_TIMEOUT=2400
CLEANUP_TIMEOUT=600
CTL_TIMEOUT_DEFAULT=30
CTL_TIMEOUT=$CTL_TIMEOUT_DEFAULT
FRAME=@@csi-frame@@

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15
	-o ServerAliveCountMax=4 -o LogLevel=ERROR)

CASES=(k8s volume failover many sanity)

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------

CP=""
CP_IP=""
HOST=()
HOST_IP=()
CN=()
CN_IP=()
DN=()
DN_IP=()
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
CP_NAME=""
HOST_NAME=()
HOST_NQN=()
SMOKE_DIR=""
JQ=""
CTL_RC=""
CTL_ERR=""
CTL_OUT=""
declare -A PID=()
declare -A RUNNING=()
CLUSTER_ID=""

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

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

assert_field() { # <json> <filter> <want> <label>
	assert_eq "$(jq_of "$1" "$2")" "$3" "$4"
}

assert_parses() { # <json> <label>
	printf '%s' "$1" | "$JQ" -e . >/dev/null ||
		die "$2: stdout is not one parseable JSON document: '$1'"
}

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
# Every guest needs passwordless sudo: k3s, kubectl, the agents' dm, md and
# nvmet work, the hosts' nvme connects. The `sudo -n bash -c $(printf '%q' …)`
# shape is the e2e suite's: -n so a sudo that would prompt fails instead of
# hanging, %q so an argument with a space or a metacharacter is not re-split
# by the remote shell. The control-plane daemons run as the login user, as
# in the e2e suite.
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
ssh_host() { # <h> <cmd…>
	local h=$1
	shift
	ssh_sudo "${HOST[$h]}" "$@"
}
ssh_host_user() { # <h> <cmd…>
	local h=$1
	shift
	ssh_to "${HOST[$h]}" "$@"
}
ssh_dn() { # <v> <cmd…>
	local v=$1
	shift
	ssh_sudo "${DN[$v]}" "$@"
}
ssh_cn() { # <v> <cmd…>
	local v=$1
	shift
	ssh_sudo "${CN[$v]}" "$@"
}

helper_cp() { ssh_cp_sudo "bash $HELPER $*"; }
helper_cp_ok() { helper_cp "$@" || true; }
helper_host() { # <h> <verb> [args…]
	local h=$1
	shift
	ssh_host "$h" "bash $HELPER $*"
}
helper_host_ok() { helper_host "$@" || true; }
helper_dn() { # <v> <verb> [args…]
	local v=$1
	shift
	ssh_dn "$v" "bash $HELPER $*"
}
helper_dn_ok() { helper_dn "$@" || true; }
helper_cn() { # <v> <verb> [args…]
	local v=$1
	shift
	ssh_cn "$v" "bash $HELPER $*"
}
helper_cn_ok() { helper_cn "$@" || true; }

# kc runs one kubectl command on the control-plane guest (CSI3); each argument
# is quoted on its own, so a jsonpath expression reaches kubectl as one word.
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

# kc_exec runs a command in a pod of the test namespace and prints its stdout.
kc_exec() { # <pod> <cmd…>
	local pod=$1
	shift
	kc exec -n "$TEST_NS" "$pod" -- "$@"
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

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing on the driver: $1"
}

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

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/csi_test.sh [--only <case>] [--cleanup-only]
           [--slice-cnt N] [--keep-logs DIR]
           --cp user@ip --cn user@ip … --dn user@ip … --host user@ip --host user@ip

roles (repeatable flags; nothing is positional, nothing is hardcoded):
  --cp    exactly 1   the k3s server, beside etcd, dnv-gateway, dnv-worker
                      and dnv-cdc (passwordless sudo)
  --cn    at least 2  one \`dnv-agent cn\` each; a volume's pool has two
                      cntlrs, and the failover case needs the second
  --dn    at least 4  one \`dnv-agent dn\` each; a RAID1 pool of one slice
                      puts its four legs on four disk nodes
  --host  exactly 2   the k3s agents: real kernel NVMe hosts that run the
                      pods (passwordless sudo)

parameters:
  --slice-cnt N       accepted for the runner's sake; every volume's pool has
                      one slice (default $SLICE_CNT)
  --only <case>       run one case: ${CASES[*]}
  --cleanup-only      run the start cleanup on every guest and stop
  --keep-logs DIR     copy the control-plane logs, the agents' logs and the
                      k3s journals into DIR at the end of the run

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
	[ "${#CN[@]}" -ge 2 ] || {
		log "at least two --cn are required (got ${#CN[@]})"
		usage
	}
	[ "${#DN[@]}" -ge 4 ] || {
		log "at least four --dn are required (got ${#DN[@]})"
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
	local all=("$CP" "${HOST[@]}" "${CN[@]}" "${DN[@]}") i j
	for i in "${!all[@]}"; do
		for j in "${!all[@]}"; do
			[ "$i" -lt "$j" ] || continue
			[ "${all[$i]##*@}" != "${all[$j]##*@}" ] ||
				die "the guest ${all[$i]##*@} is named in two roles"
		done
	done
	CP_IP=${CP##*@}
	for i in "${!HOST[@]}"; do HOST_IP[i]=${HOST[$i]##*@}; done
	for i in "${!CN[@]}"; do CN_IP[i]=${CN[$i]##*@}; done
	for i in "${!DN[@]}"; do DN_IP[i]=${DN[$i]##*@}; done
	[ -n "$IMAGE" ] || die "no 'image: dnv-csi:…' in $DEPLOY_DIR/controller.yaml"
}

log_topology() {
	log "=== topology"
	log "  cp    $CP"
	local i
	for i in "${!HOST[@]}"; do log "  host$i ${HOST[$i]}"; done
	for i in "${!CN[@]}"; do log "  cn$i   ${CN[$i]}"; done
	for i in "${!DN[@]}"; do log "  dn$i   ${DN[$i]}"; done
	log "  k3s $K3S_VERSION, driver $DRIVER_NAME, image $IMAGE, cluster $CLUSTER"
	log "  cases ${ONLY:-${CASES[*]}}"
}

dn_addr() { printf '%s:%s' "${DN_IP[$1]}" "$DN_GRPC_PORT"; }
cn_addr() { printf '%s:%s' "${CN_IP[$1]}" "$CN_GRPC_PORT"; }

# ---------------------------------------------------------------------------
# The shipped helper: one script for every guest, generated here,
# syntax-checked on the driver and copied to $HELPER on every run. The
# verbs of the roles a guest does not have are never called there.
# ---------------------------------------------------------------------------

helper_source() {
	cat <<EOF
#!/usr/bin/env bash
#
# dnv-csi-helper.sh — the guest half of integtest/csi_test.sh, GENERATED by
# that suite and shipped to every guest as $HELPER on every run. Editing it
# here changes nothing. No iflag=/oflag= appears in this file, no pkill
# pattern is unbracketed, and no nvmet port is removed before its ana_groups.
set -uo pipefail

WORK="$WORK"
NVMET="$NVMET"
NQN_PREFIX="$NQN_PREFIX"
NQN_CSI="$NQN_CSI"
DM_PREFIX="dnv"
UDEV_RULE=/etc/udev/rules.d/63-dnv-md.rules
ETCD_NAME="$ETCD_NAME"
TRSVCID_MIN="$TRSVCID_MIN"
TRSVCID_MAX="$TRSVCID_MAX"
K3S_INSTALL_URL="$K3S_INSTALL_URL"
SMOKE_IMAGE_REF="$SMOKE_IMAGE_REF"
IMAGE_REF="$IMAGE_REF"
DISC_NQN=nqn.2014-08.org.nvmexpress.discovery
EOF
	cat <<'HELPER_EOF'

# --- the guest's facts and work directory --------------------------------------

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

mkwork() { # <subdir…>
	local d
	mkdir -p "$WORK" || return 1
	chmod 0777 "$WORK" 2>/dev/null || true
	for d in "$@"; do
		mkdir -p "$WORK/$d" || return 1
		chmod 0777 "$WORK/$d" 2>/dev/null || true
	done
	echo mkwork-done
	return 0
}

rmwork() {
	rm -rf "$WORK"
	echo rmwork-done
	return 0
}

tools() { # <tool…>
	local t
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || printf '%s\n' "$t"
	done
	return 0
}

modules() { # <module…>
	local m
	for m in "$@"; do
		modprobe "$m" >/dev/null 2>&1 || printf '%s\n' "$m"
	done
	return 0
}

listening() { # <port>
	local n
	n=$(ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://' |
		grep -cx "$1" || true)
	[ "$n" != 0 ]
}

ports_busy() { # <port…>
	local p
	for p in "$@"; do
		listening "$p" && printf '%s\n' "$p"
	done
	return 0
}

# --- processes -------------------------------------------------------------------

kill_pidfile() { # <pidfile> [secs]
	local f=$1 secs=${2:-15} pid i
	[ -f "$f" ] || {
		echo "no pid file $f"
		return 0
	}
	pid=$(cat "$f" 2>/dev/null) || pid=""
	case "$pid" in
	'' | *[!0-9]*)
		echo "pid file $f holds '$pid'"
		return 0
		;;
	esac
	kill -CONT "$pid" 2>/dev/null
	kill -TERM "$pid" 2>/dev/null
	for ((i = 0; i < secs * 4; i++)); do
		kill -0 "$pid" 2>/dev/null || {
			echo "stopped $pid"
			return 0
		}
		sleep 0.25
	done
	kill -KILL "$pid" 2>/dev/null
	sleep 0.5
	if kill -0 "$pid" 2>/dev/null; then
		echo "STILL_RUNNING $pid"
	else
		echo "killed $pid"
	fi
	return 0
}

# sig_pidfile stops or continues one recorded process and prints its state
# letter afterwards: the failover case cuts a cn agent off with STOP, so
# that every kernel object it built goes on serving, as the e2e suite's
# cutoff case does, and gives it back with CONT.
sig_pidfile() { # <pidfile> <STOP|CONT>
	local pid state
	pid=$(cat "$1" 2>/dev/null) || pid=""
	case "$pid" in
	'' | *[!0-9]*)
		echo "state=none"
		return 0
		;;
	esac
	kill "-$2" "$pid" 2>/dev/null
	sleep 0.5
	state=$(ps -o stat= -p "$pid" 2>/dev/null | cut -c1) || state=""
	printf 'state=%s\n' "${state:-none}"
	return 0
}

# --- k3s --------------------------------------------------------------------------

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

k3s_fetch_installer() {
	mkdir -p "$WORK"
	curl -sfL "$K3S_INSTALL_URL" -o "$WORK/k3s-install.sh" || {
		echo "k3s_fetch_installer: downloading $K3S_INSTALL_URL failed" >&2
		return 1
	}
	chmod 0755 "$WORK/k3s-install.sh"
	echo fetched
}

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

k3s_token() {
	cat /var/lib/rancher/k3s/server/node-token
}

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

# --- images (CSI4) ------------------------------------------------------------------

image_import() {
	k3s ctr images import - >/dev/null || {
		echo "image_import: ctr images import failed" >&2
		return 1
	}
	echo imported
}

image_present() { # <ref>
	k3s ctr images ls -q 2>/dev/null | grep -qx "$1"
}

image_rm() { # <ref>
	if command -v k3s >/dev/null 2>&1 && image_present "$1"; then
		k3s ctr images rm "$1" >/dev/null 2>&1 || true
	fi
	echo image-rm-done
}

journal() { # [lines]
	journalctl -u k3s -u k3s-agent --no-pager -n "${1:-80}" 2>/dev/null || true
}

# --- the agents (dn and cn guests) --------------------------------------------------

kill_agents() { # <dn|cn>
	local pat="$WORK/bin/[d]nv-agent $1" i
	pkill -CONT -f "$pat" >/dev/null 2>&1
	pkill -f "$pat" >/dev/null 2>&1
	for ((i = 0; i < 40; i++)); do
		pgrep -f "$pat" >/dev/null 2>&1 || break
		sleep 0.25
	done
	pkill -9 -f "$pat" >/dev/null 2>&1
	for ((i = 0; i < 20; i++)); do
		pgrep -f "$pat" >/dev/null 2>&1 || break
		sleep 0.25
	done
	if pgrep -f "$pat" >/dev/null 2>&1; then
		echo "STILL_RUNNING dnv-agent $1"
	else
		echo "stopped dnv-agent $1"
	fi
	return 0
}

agent_pids() { # <dn|cn>
	pgrep -af "[d]nv-agent $1" || true
	return 0
}

# --- dm --------------------------------------------------------------------------------

dm_names() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E "^$DM_PREFIX-[0-9a-f]{16}-[0-9a-f]{16}-[cd][0-9a-f]-" || true
	return 0
}

dm_kind_names() { # <kind>
	dm_names | awk -F- -v k="$1" '$4 == k'
	return 0
}

# resume_suspended releases every suspended dnv dm device before anything
# reads one: a read of a suspended device blocks in D state for good.
resume_suspended() {
	local name attr
	dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		while IFS=: read -r name attr; do
			case "$name" in "$DM_PREFIX"-*) ;; *) continue ;; esac
			case "$attr" in
			*s*) timeout 10 dmsetup resume "$name" >/dev/null 2>&1 ;;
			esac
		done
	return 0
}

dm_force_remove() { # <name>
	timeout 10 dmsetup remove "$1" >/dev/null 2>&1 && return 0
	timeout 10 dmsetup resume "$1" >/dev/null 2>&1
	timeout 15 dmsetup remove --force --retry "$1" >/dev/null 2>&1
	return 0
}

dm_remove_kind() { # <kind>
	local name
	for name in $(dm_kind_names "$1"); do
		dm_force_remove "$name"
	done
	return 0
}

dm_remove_all() {
	local name
	for name in $(dm_names); do
		dm_force_remove "$name"
	done
	return 0
}

# --- nvme connections ---------------------------------------------------------------------

disconnect_prefix() { # <nqn prefix>
	local sysfs nqn
	for sysfs in /sys/class/nvme-subsystem/*/subsysnqn; do
		[ -e "$sysfs" ] || continue
		nqn=$(cat "$sysfs" 2>/dev/null) || continue
		case "$nqn" in
		"$1"*) timeout 30 nvme disconnect -n "$nqn" >/dev/null 2>&1 ;;
		esac
	done
	return 0
}

subsys_nqns() {
	local sysfs nqn
	for sysfs in /sys/class/nvme-subsystem/*/subsysnqn; do
		[ -e "$sysfs" ] || continue
		nqn=$(cat "$sysfs" 2>/dev/null) || continue
		printf '%s\n' "$nqn"
	done
	return 0
}

# disconnect_discovery drops the discovery controllers pointing at one cdc
# address, by device: every discovery controller shares one NQN.
disconnect_discovery() { # <traddr>
	local ctrl nqn addr name
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		[ "$nqn" = "$DISC_NQN" ] || continue
		addr=$(cat "$ctrl/address" 2>/dev/null) || addr=""
		case "$addr" in
		*"traddr=$1"*)
			name=${ctrl##*/}
			timeout 30 nvme disconnect -d "$name" >/dev/null 2>&1 || true
			;;
		esac
	done
	return 0
}

# discovery_ctrls prints the cdc address of every discovery controller.
discovery_ctrls() {
	local ctrl nqn addr
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		[ "$nqn" = "$DISC_NQN" ] || continue
		addr=$(cat "$ctrl/address" 2>/dev/null) || addr=""
		printf '%s\n' "$addr"
	done
	return 0
}

# --- nvmet (dn and cn guests) -------------------------------------------------------

drop_subsys_glob() { # <nqn glob>
	local subsys ns host link
	[ -d "$NVMET" ] || return 0
	for link in "$NVMET"/ports/*/subsystems/$1; do
		[ -e "$link" ] && rm -f "$link"
	done
	for subsys in "$NVMET"/subsystems/$1; do
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

subsys_names() {
	ls "$NVMET/subsystems" 2>/dev/null || true
	return 0
}

hosts_drop() { # [extra nqn…]
	local host extra
	[ -d "$NVMET/hosts" ] || return 0
	for host in "$NVMET"/hosts/"$NQN_PREFIX":*; do
		[ -d "$host" ] && rmdir "$host" 2>/dev/null
	done
	for extra in "$@"; do
		[ -n "$extra" ] && [ -d "$NVMET/hosts/$extra" ] && rmdir "$NVMET/hosts/$extra" 2>/dev/null
	done
	return 0
}

port_trsvcid() { # <port id>
	local got
	[ -d "$NVMET/ports/$1" ] || {
		printf 'absent'
		return 0
	}
	got=$(cat "$NVMET/ports/$1/addr_trsvcid" 2>/dev/null | tr -d ' ') || got=""
	if [ -z "$got" ]; then printf 'none'; else printf '%s' "$got"; fi
	return 0
}

# port_drop removes ONE nvmet port and refuses one outside this suite's
# service-id band. ana_groups 3 and 2 go BEFORE the port (rule 7).
port_drop() { # <port id>
	local id=$1 svc grp link
	[ -d "$NVMET/ports/$id" ] || {
		printf 'port %s absent\n' "$id"
		return 0
	}
	svc=$(port_trsvcid "$id")
	case "$svc" in
	none)
		printf 'port %s empty trsvcid, removing as debris\n' "$id"
		;;
	*[!0-9]*)
		printf 'port %s REFUSED addr_trsvcid=%s\n' "$id" "$svc"
		return 0
		;;
	*)
		if [ "$svc" -lt "$TRSVCID_MIN" ] || [ "$svc" -gt "$TRSVCID_MAX" ]; then
			printf 'port %s REFUSED addr_trsvcid=%s outside %s..%s\n' \
				"$id" "$svc" "$TRSVCID_MIN" "$TRSVCID_MAX"
			return 0
		fi
		;;
	esac
	for link in "$NVMET/ports/$id"/subsystems/*; do
		[ -e "$link" ] && rm -f "$link"
	done
	for grp in 3 2; do
		rmdir "$NVMET/ports/$id/ana_groups/$grp" 2>/dev/null
	done
	rmdir "$NVMET/ports/$id" 2>/dev/null
	if [ -d "$NVMET/ports/$id" ]; then
		printf 'port %s STUCK trsvcid=%s\n' "$id" "$svc"
	else
		printf 'port %s removed trsvcid=%s\n' "$id" "$svc"
	fi
	return 0
}

ports_sweep() {
	local id
	[ -d "$NVMET/ports" ] || return 0
	for id in "$NVMET"/ports/*; do
		[ -d "$id" ] || continue
		port_drop "${id##*/}"
	done
	return 0
}

nvmet_tree() {
	ls -R "$NVMET/ports" "$NVMET/subsystems" "$NVMET/hosts" 2>/dev/null || true
	return 0
}

# --- md --------------------------------------------------------------------------

md_name_of() { # <md device>
	local name
	name=$(timeout 10 udevadm info --query=property --name="/dev/$1" 2>/dev/null |
		sed -n 's/^MD_NAME=//p')
	[ -n "$name" ] || name=$(timeout 10 mdadm --detail --no-devices --export \
		"/dev/$1" 2>/dev/null | sed -n 's/^MD_NAME=//p')
	printf '%s' "$name"
}

md_stop_all() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(md_name_of "$d")
		case "$name" in
		dnv-* | *:dnv-*) timeout 15 mdadm --stop "/dev/$d" >/dev/null 2>&1 ;;
		esac
	done
	return 0
}

md_names() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(md_name_of "$d")
		case "$name" in
		dnv-* | *:dnv-*) printf '%s\n' "$name" ;;
		esac
	done
	return 0
}

# --- the md assembly mask -----------------------------------------------------------

install_udev_rule() {
	local want
	want=$(
		cat <<'RULE_EOF'
ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
  IMPORT{program}="/sbin/mdadm --examine --export $devnode"
ENV{MD_NAME}=="dnv-*|*:dnv-*", ENV{SYSTEMD_READY}="0"
RULE_EOF
	)
	if [ -s "$UDEV_RULE" ] && [ "$(cat "$UDEV_RULE" 2>/dev/null)" = "$want" ]; then
		udevadm control --reload >/dev/null 2>&1
		echo present
		return 0
	fi
	printf '%s\n' "$want" >"$UDEV_RULE" || {
		echo "install_udev_rule: writing $UDEV_RULE failed" >&2
		return 1
	}
	udevadm control --reload >/dev/null 2>&1
	echo installed
	return 0
}

remove_udev_rule() {
	rm -f "$UDEV_RULE"
	udevadm control --reload >/dev/null 2>&1
	echo removed
	return 0
}

# --- loop devices ---------------------------------------------------------------

loop_devs() {
	losetup -a 2>/dev/null | grep -F "$WORK/" | cut -d: -f1 || true
	return 0
}

loop_teardown() {
	local dev
	for dev in $(loop_devs); do
		dd if=/dev/zero of="$dev" bs=4096 count=1 conv=fsync >/dev/null 2>&1
		wipefs -a "$dev" >/dev/null 2>&1
		losetup -d "$dev" >/dev/null 2>&1
	done
	return 0
}

# --- the disk node ---------------------------------------------------------------

# dn_up provisions and starts the one dn agent of this guest over a sparse
# loop device, and is idempotent: run twice it reuses the file, the device
# and a live process. <ip> <grpc_port> <trsvcid> <port_id> <size>
dn_up() {
	local ip=$1 gport=$2 svcid=$3 portid=$4 size=$5
	local dir="$WORK/dn" backing="$WORK/dn/backing.img" store="$WORK/dn/store"
	local log="$WORK/dn/agent.log" dev pid pat
	mkdir -p "$dir" "$store" || {
		echo "dn_up: mkdir $dir failed" >&2
		return 1
	}
	install_udev_rule >/dev/null || {
		echo "dn_up: installing the md assembly mask failed" >&2
		return 1
	}
	if [ ! -f "$backing" ]; then
		truncate -s "$size" "$backing" || {
			rm -f "$backing"
			echo "dn_up: truncating $backing to $size failed" >&2
			return 1
		}
	fi
	dev=$(losetup -j "$backing" 2>/dev/null | cut -d: -f1 | head -n 1)
	if [ -z "$dev" ]; then
		dev=$(losetup --find --show "$backing") || {
			echo "dn_up: losetup --find --show $backing failed" >&2
			return 1
		}
	fi
	pat="[-]-grpc-address $ip:$gport "
	pid=$(pgrep -f -- "$pat" | head -n 1) || pid=""
	if [ -z "$pid" ]; then
		nohup "$WORK/bin/dnv-agent" dn \
			--grpc-network tcp --grpc-address "$ip:$gport" \
			--tr-type tcp --adr-fam ipv4 \
			--tr-addr "$ip" --tr-svc-id "$svcid" \
			--nvmet-port-id "$portid" \
			--local-store "$store" --disk "$dev" \
			>>"$log" 2>&1 </dev/null &
		pid=$!
		sleep 0.5
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "dn_up: agent for $ip:$gport exited immediately; last 20 lines of $log:" >&2
			tail -n 20 "$log" >&2 || true
			return 1
		fi
	fi
	echo "$pid" >"$dir/pid"
	printf 'loop=%s pid=%s\n' "$dev" "$pid"
	return 0
}

# dn_cleanup takes a disk-node guest back: the agent, stray arrays, the
# nvmet objects, the dm devices top-down, the loop device, the mask, $WORK.
dn_cleanup() { # [extra host nqn…]
	kill_agents dn >/dev/null
	resume_suspended
	md_stop_all
	drop_subsys_glob "$NQN_PREFIX:2:*"
	dm_remove_kind d1
	dm_remove_kind d3
	disconnect_prefix "$NQN_PREFIX:3:"
	drop_subsys_glob "$NQN_PREFIX:3:*"
	dm_remove_kind d5
	dm_remove_kind d2
	dm_remove_kind d0
	dm_remove_kind d4
	dm_remove_all
	hosts_drop "$@"
	ports_sweep
	resume_suspended
	loop_teardown
	remove_udev_rule >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# dn_residue is empty on a clean disk node: no dm device, no dnv nvmet
# subsystem.
dn_residue() {
	dm_names
	subsys_names | grep -F "$NQN_PREFIX:" || true
	return 0
}

# --- the controller node ------------------------------------------------------------

# cn_up starts the one cn agent of this guest, the md assembly mask before
# it, and is idempotent. <ip> <grpc_port> <trsvcid>
cn_up() {
	local ip=$1 gport=$2 svcid=$3
	local dir="$WORK/cn" store="$WORK/cn/store" log="$WORK/cn/agent.log" pid pat
	mkdir -p "$dir" "$store" || {
		echo "cn_up: mkdir $dir failed" >&2
		return 1
	}
	install_udev_rule >/dev/null || {
		echo "cn_up: installing the md assembly mask failed" >&2
		return 1
	}
	pat="[-]-grpc-address $ip:$gport "
	pid=$(pgrep -f -- "$pat" | head -n 1) || pid=""
	if [ -z "$pid" ]; then
		nohup "$WORK/bin/dnv-agent" cn \
			--grpc-network tcp --grpc-address "$ip:$gport" \
			--tr-type tcp --adr-fam ipv4 \
			--tr-addr "$ip" --tr-svc-id "$svcid" \
			--local-store "$store" --capacity 0 \
			>>"$log" 2>&1 </dev/null &
		pid=$!
		sleep 0.5
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "cn_up: agent for $ip:$gport exited immediately; last 20 lines of $log:" >&2
			tail -n 20 "$log" >&2 || true
			return 1
		fi
	fi
	echo "$pid" >"$dir/pid"
	printf 'pid=%s\n' "$pid"
	return 0
}

# cn_cleanup takes a controller-node guest back: the agent, the driver's and
# dnv's host-facing subsystems, the dm stack top-down, the arrays, the leg
# connections, the port, the mask, $WORK.
cn_cleanup() { # [extra host nqn…]
	local kind
	kill_agents cn >/dev/null
	resume_suspended
	drop_subsys_glob "$NQN_CSI:*"
	drop_subsys_glob "$NQN_PREFIX:4:*"
	for kind in c6 c8 c7 c5 c4 c3 c2 c1 c0; do
		dm_remove_kind "$kind"
	done
	md_stop_all
	dm_remove_kind ca
	dm_remove_kind c9
	disconnect_prefix "$NQN_PREFIX:"
	dm_remove_all
	resume_suspended
	hosts_drop "$@"
	ports_sweep
	remove_udev_rule >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# cn_residue is empty on a clean controller node: no dm device, no array, no
# nvmet subsystem of dnv's or the driver's.
cn_residue() {
	dm_names
	subsys_names | grep -F -e "$NQN_PREFIX:" -e "$NQN_CSI:" || true
	md_names
	return 0
}

# --- the control-plane guest ---------------------------------------------------------

stop_all() {
	local f pid i
	for f in "$WORK"/*/pid; do
		[ -f "$f" ] || continue
		pid=$(cat "$f" 2>/dev/null) || continue
		case "$pid" in
		'' | *[!0-9]*) continue ;;
		esac
		kill -CONT "$pid" 2>/dev/null
		kill -TERM "$pid" 2>/dev/null
		for ((i = 0; i < 40; i++)); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.25
		done
		kill -KILL "$pid" 2>/dev/null
	done
	pkill -f "$WORK/bin/[d]nv-gateway" >/dev/null 2>&1
	pkill -f "$WORK/bin/[d]nv-worker" >/dev/null 2>&1
	pkill -f "$WORK/bin/[d]nv-cdc" >/dev/null 2>&1
	pkill -f "[e]tcd --name $ETCD_NAME" >/dev/null 2>&1
	sleep 0.5
	pkill -9 -f "$WORK/bin/[d]nv-gateway" >/dev/null 2>&1
	pkill -9 -f "$WORK/bin/[d]nv-worker" >/dev/null 2>&1
	pkill -9 -f "$WORK/bin/[d]nv-cdc" >/dev/null 2>&1
	pkill -9 -f "[e]tcd --name $ETCD_NAME" >/dev/null 2>&1
	echo stopped
	return 0
}

cp_pids() {
	pgrep -af "[d]nv-gateway|[d]nv-worker|[d]nv-cdc|[e]tcd --name $ETCD_NAME" || true
	return 0
}

cp_cleanup() {
	stop_all >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# --- the hosts ---------------------------------------------------------------------

# mask neutralises the kernel's own autoconnector for the run, and verifies.
mask() {
	local svc tgt
	systemctl mask nvmf-connect@.service >/dev/null 2>&1 || true
	systemctl mask nvmf-connect.target >/dev/null 2>&1 || true
	svc=$(systemctl is-enabled nvmf-connect@.service 2>/dev/null) || true
	tgt=$(systemctl is-enabled nvmf-connect.target 2>/dev/null) || true
	if [ "$svc" = masked ] && [ "$tgt" = masked ]; then
		echo masked
	else
		printf 'service=%s target=%s\n' "${svc:-unreadable}" "${tgt:-unreadable}"
	fi
	return 0
}

unmask() {
	systemctl unmask nvmf-connect@.service >/dev/null 2>&1 || true
	systemctl unmask nvmf-connect.target >/dev/null 2>&1 || true
	echo unmasked
	return 0
}

# wipe drops every connection to the driver's and dnv's subsystems and the
# discovery controllers pointing at the suite's cdc. Never `disconnect-all`.
wipe() { # <cdc traddr>
	local nqn left=""
	disconnect_prefix "$NQN_CSI:"
	disconnect_prefix "$NQN_PREFIX:"
	disconnect_discovery "$1"
	for nqn in $(subsys_nqns); do
		case "$nqn" in
		"$NQN_CSI:"* | "$NQN_PREFIX:"*) left="$left $nqn" ;;
		esac
	done
	[ -z "$left" ] || printf 'wipe_left=%s\n' "${left# }"
	echo wiped
	return 0
}

# sanity_stop ends the two plugins the sanity case runs as plain processes.
sanity_stop() {
	kill_pidfile "$WORK/sanity/node.pid" >/dev/null
	kill_pidfile "$WORK/sanity/ctl.pid" >/dev/null
	pkill -f "$WORK/bin/[d]nv-csi" >/dev/null 2>&1
	echo sanity-stopped
	return 0
}

host_cleanup() { # <cdc traddr>
	sanity_stop >/dev/null
	wipe "$1" >/dev/null
	unmask >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# host_residue is empty on a clean host: no connection to a subsystem of the
# driver's or dnv's, no discovery controller pointing at the suite's cdc.
host_residue() { # <cdc traddr>
	subsys_nqns | grep -F -e "$NQN_CSI:" -e "$NQN_PREFIX:" || true
	discovery_ctrls | grep -F "traddr=$1" || true
	return 0
}

# sanity_start runs the two plugins as plain processes on this host, for
# csi-sanity. <gateway> <cdc>
sanity_start() {
	local dir="$WORK/sanity"
	# csi-sanity makes its mount and staging directories itself, one mkdir
	# each, and treats a directory that is already there as an error.
	mkdir -p "$dir" || return 1
	nohup "$WORK/bin/dnv-csi" controller --csi-address "$dir/ctl.sock" \
		--gateway-address "$1" --cdc-addresses "$2" >>"$dir/ctl.log" 2>&1 </dev/null &
	echo $! >"$dir/ctl.pid"
	nohup "$WORK/bin/dnv-csi" node --csi-address "$dir/node.sock" \
		--cdc-addresses "$2" >>"$dir/node.log" 2>&1 </dev/null &
	echo $! >"$dir/node.pid"
	local i
	for ((i = 0; i < 40; i++)); do
		[ -S "$dir/ctl.sock" ] && [ -S "$dir/node.sock" ] && break
		sleep 0.25
	done
	if [ ! -S "$dir/ctl.sock" ] || [ ! -S "$dir/node.sock" ]; then
		echo "sanity_start: the plugins did not listen; logs:" >&2
		tail -n 20 "$dir/ctl.log" "$dir/node.log" >&2 || true
		return 1
	fi
	echo sanity-started
	return 0
}

# --- diagnostics ------------------------------------------------------------------------

diag() {
	echo "--- $(hostname): facts"
	facts
	echo "--- $(hostname): k3s"
	k3s_state
	echo "--- $(hostname): processes"
	cp_pids
	agent_pids dn
	agent_pids cn
	pgrep -af "[d]nv-csi" || true
	echo "--- $(hostname): nvme subsystems"
	subsys_nqns
	echo "--- $(hostname): discovery controllers"
	discovery_ctrls
	echo "--- $(hostname): dm"
	dm_names
	echo "--- $(hostname): md"
	cat /proc/mdstat 2>/dev/null || true
	echo "--- $(hostname): nvmet"
	nvmet_tree
	echo "--- $(hostname): loop"
	losetup -a 2>/dev/null || true
	echo "--- $(hostname): memory"
	free -m
	echo "--- $(hostname): k3s journal (last 40 lines)"
	journal 40
	echo "--- $(hostname): agent and plugin logs (last 30 lines each)"
	tail -n 30 "$WORK"/*/agent.log "$WORK"/*/*.log 2>/dev/null || true
	return 0
}

# --- dispatch -------------------------------------------------------------------------

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
	local tmp target
	tmp=$(mktemp) || die "mktemp on the driver failed"
	helper_source >"$tmp" || die "generating the helper failed"
	bash -n "$tmp" || die "the generated helper is not valid bash"
	for target in "$CP" "${HOST[@]}" "${CN[@]}" "${DN[@]}"; do
		scp -q "${SSH_OPTS[@]}" "$tmp" "$target:$HELPER" ||
			die "scp of the helper to ${target##*@} failed"
	done
	rm -f "$tmp"
	log "  helper shipped to $HELPER on all $((3 + ${#CN[@]} + ${#DN[@]})) guests"
}

# ---------------------------------------------------------------------------
# Preflight (doc/csi.md, Preflight)
# ---------------------------------------------------------------------------

read_constants() {
	local json
	json=$("$WORKERCTL_BIN" constants) ||
		die "\`workerctl constants\` failed: this suite reads common.EtcdMaxTxnOps" \
			"and common.EtcdMaxRequestBytes from it and must not guess them"
	ETCD_MAX_TXN_OPS=$(jq_of "$json" .EtcdMaxTxnOps)
	ETCD_MAX_REQUEST_BYTES=$(jq_of "$json" .EtcdMaxRequestBytes)
	case "$ETCD_MAX_TXN_OPS$ETCD_MAX_REQUEST_BYTES" in
	'' | *[!0-9]*) die "workerctl constants answered $json" ;;
	esac
	log "  --max-txn-ops = $ETCD_MAX_TXN_OPS, --max-request-bytes = $ETCD_MAX_REQUEST_BYTES"
}

preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp docker mktemp timeout awk sed grep curl tar sha256sum; do
		need_local "$tool"
	done
	docker info >/dev/null 2>&1 ||
		die "docker on the driver is not usable (docker info failed)"
	resolve_jq
	log "  make build (CGO_ENABLED=0 GOOS=linux GOARCH=amd64)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	local bin
	for bin in "$AGENT_BIN" "$GATEWAY_BIN" "$WORKER_BIN" "$CDC_BIN" "$DNVCTL_BIN" "$CSI_BIN"; do
		[ -x "$bin" ] || die "missing: $bin after make build"
	done
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$WORKERCTL_BIN" ./integtest/workerctl >&2) ||
		die "building workerctl failed"
	read_constants
	fetch_etcd
	log "preflight (driver) ok"
}

fact_of() { # <facts output> <key>
	printf '%s\n' "$1" | awk -F= -v k="$2" '$1==k {sub(/^[^=]*=/, ""); print; exit}'
}

# preflight_guests proves what every guest must have before anything is
# built: passwordless sudo everywhere; on the three Kubernetes guests unique
# hostnames and machine-ids, the memory floor and native multipath on the
# hosts, the hosts' identities present and different (CSI2); on the node
# guests the tools and modules of the agents and this suite's ports free.
preflight_guests() {
	STAGE="preflight (guests)"
	log ""
	log "=== preflight: guests"
	local out h v names="" ids="" nqns="" mem missing busy
	ssh_cp "sudo -n true" >/dev/null ||
		die "cp ($CP): passwordless sudo is required (k3s and kubectl run as root)"
	out=$(helper_cp facts) || die "cp: reading its facts failed"
	CP_NAME=$(fact_of "$out" hostname)
	[ -n "$CP_NAME" ] || die "cp: empty hostname"
	names="$CP_NAME"
	ids=$(fact_of "$out" machine_id)
	missing=$(helper_cp tools ss curl) || die "cp: listing its tools failed"
	[ -z "$missing" ] || die "cp: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"
	busy=$(helper_cp ports_busy "$ETCD_CLIENT_PORT" "$ETCD_PEER_PORT" "$GW_PORT" "$CDC_PORT") ||
		die "cp: reading its ports failed"
	[ -z "$busy" ] || die "cp: port(s) already listening: $(printf '%s' "$busy" | tr '\n' ' ')"
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
		HOST_NQN[$h]=$(fact_of "$out" hostnqn)
		nqns="$nqns ${HOST_NQN[$h]}"
		assert_eq "$(fact_of "$out" multipath)" Y "host$h: nvme_core.multipath"
		mem=$(fact_of "$out" mem_total_kib)
		assert_ge "$mem" 3000000 "host$h: MemTotal in KiB (raise the guest's memory)"
		missing=$(helper_host "$h" tools nvme uuidgen systemctl udevadm) ||
			die "host$h: listing its tools failed"
		[ -z "$missing" ] || die "host$h: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"
		missing=$(helper_host "$h" modules nvme-tcp) || die "host$h: modprobe failed"
		[ -z "$missing" ] || die "host$h: module(s) missing: $missing"
		log "  host$h: hostname ${HOST_NAME[$h]}, $((mem / 1024)) MiB, $(fact_of "$out" hostnqn)"
	done
	assert_eq "$(printf '%s\n' $names | sort -u | wc -l)" 3 \
		"the three Kubernetes guests' hostnames are distinct ($names)"
	assert_eq "$(printf '%s\n' $ids | sort -u | wc -l)" 3 \
		"the three Kubernetes guests' machine-ids are distinct"
	assert_eq "$(printf '%s\n' $nqns | sort -u | wc -l)" 2 \
		"the two hosts' host NQNs differ ($nqns); regenerate one with nvme gen-hostnqn and uuidgen"
	for v in "${!DN[@]}"; do
		ssh_dn "$v" "true" >/dev/null || die "dn$v (${DN[$v]}): passwordless sudo is required"
		missing=$(helper_dn "$v" tools dmsetup losetup truncate wipefs nvme udevadm mdadm ss) ||
			die "dn$v: listing its tools failed"
		[ -z "$missing" ] || die "dn$v: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"
		missing=$(helper_dn "$v" modules nvmet nvmet-tcp nvme-tcp loop dm-mod) ||
			die "dn$v: modprobe failed"
		[ -z "$missing" ] || die "dn$v: module(s) missing: $(printf '%s' "$missing" | tr '\n' ' ')"
		busy=$(helper_dn "$v" ports_busy "$DN_GRPC_PORT" "$DN_TRSVCID") || die "dn$v: ports"
		[ -z "$busy" ] || die "dn$v: port(s) already listening: $(printf '%s' "$busy" | tr '\n' ' ')"
	done
	for v in "${!CN[@]}"; do
		ssh_cn "$v" "true" >/dev/null || die "cn$v (${CN[$v]}): passwordless sudo is required"
		missing=$(helper_cn "$v" tools dmsetup mdadm nvme udevadm ss) ||
			die "cn$v: listing its tools failed"
		[ -z "$missing" ] || die "cn$v: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"
		missing=$(helper_cn "$v" modules nvmet nvmet-tcp nvme-tcp dm-thin-pool raid1) ||
			die "cn$v: modprobe failed"
		[ -z "$missing" ] || die "cn$v: module(s) missing: $(printf '%s' "$missing" | tr '\n' ' ')"
		busy=$(helper_cn "$v" ports_busy "$CN_GRPC_PORT" "$CN_TRSVCID") || die "cn$v: ports"
		[ -z "$busy" ] || die "cn$v: port(s) already listening: $(printf '%s' "$busy" | tr '\n' ' ')"
	done
	log "preflight (guests) ok"
}

# ---------------------------------------------------------------------------
# k3s: present or installed (CSI1), then the cluster's shape (CSI6)
# ---------------------------------------------------------------------------

k3s_version_of() { printf '%s\n' "$1" | awk -F= '$1=="version"{print $2; exit}'; }
k3s_unit_of() { printf '%s\n' "$1" | awk -F= -v u="$2" '$1==u{print $2; exit}'; }

k3s_ensure() {
	STAGE="k3s"
	log ""
	log "=== k3s $K3S_VERSION: present or installed"
	local out ver h token
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
				"sudo -n bash $HELPER k3s_install_agent $K3S_VERSION https://$CP_IP:$K3S_API_PORT" 2>&1) ||
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
	kc taint node "$CP_NAME" "$CP_TAINT_KEY:$CP_TAINT_EFFECT" --overwrite >/dev/null ||
		die "cp: tainting the control-plane node failed"
	kc -n kube-system rollout status deploy/coredns "--timeout=${WAIT_SYSTEM}s" >/dev/null ||
		die "coredns did not become available"
	log "  nodes:"
	kc get nodes -o wide >&2 || true
}

nodes_ready() {
	local out want
	out=$(kc get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{" "}{.status.nodeInfo.kubeletVersion}{"\n"}{end}' 2>/dev/null) || return 1
	for want in "$CP_NAME" "${HOST_NAME[@]}"; do
		printf '%s\n' "$out" | grep -qxF "$want True $K3S_VERSION" || return 1
	done
	[ "$(printf '%s\n' "$out" | grep -c . || true)" -eq $((1 + ${#HOST[@]})) ]
}

# ---------------------------------------------------------------------------
# The dnv stack (doc/csi.md, The suite's dnv stack)
# ---------------------------------------------------------------------------

remote_start() { # <target> <dir> <logfile> <cmd…>
	local target=$1 dir=$2 logf=$3
	shift 3
	local pid
	pid=$(ssh_to "$target" "nohup $* >> $WORK/$dir/$logf 2>&1 < /dev/null &" \
		"echo \$! > $WORK/$dir/pid; cat $WORK/$dir/pid") ||
		die "starting $dir on ${target##*@} failed"
	[ -n "$pid" ] || die "starting $dir on ${target##*@} produced no pid"
	PID[$dir]=$pid
	RUNNING[$dir]=1
	log "  $dir started on ${target##*@}, pid $pid"
}

cp_port_up() { helper_cp listening "$1" >/dev/null 2>&1; }

start_cp_daemons() {
	[ -n "$ETCD_MAX_TXN_OPS" ] || die "start_cp_daemons before read_constants"
	remote_start "$CP" etcd etcd.log \
		"$WORK/bin/etcd --name $ETCD_NAME --data-dir $WORK/etcd" \
		"--listen-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--advertise-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--listen-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-advertise-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster $ETCD_NAME=http://127.0.0.1:$ETCD_PEER_PORT" \
		"--max-txn-ops=$ETCD_MAX_TXN_OPS" \
		"--max-request-bytes=$ETCD_MAX_REQUEST_BYTES"
	wait_until "$WAIT_SHORT" "etcd to listen on $ETCD_CLIENT_PORT" cp_port_up "$ETCD_CLIENT_PORT"
	remote_start "$CP" gateway gateway.log \
		"$WORK/bin/dnv-gateway --grpc-network tcp" \
		"--grpc-address $CP_IP:$GW_PORT" \
		"--etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT"
	wait_until "$WAIT_SHORT" "the gateway to listen on $GW_PORT" cp_port_up "$GW_PORT"
	remote_start "$CP" worker worker.log \
		"$WORK/bin/dnv-worker --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--roles ${WORKER_ROLES// /,}" \
		"--vote-interval $VOTE_INTERVAL --vote-grace-time $VOTE_GRACE"
	remote_start "$CP" cdc cdc.log \
		"$WORK/bin/dnv-cdc --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--tr-type tcp --adr-fam ipv4 --tr-addr $CP_IP --tr-svc-id $CDC_PORT"
	wait_until "$WAIT_SHORT" "the cdc to listen on $CDC_PORT" cp_port_up "$CDC_PORT"
}

# --- dnvctl on cp ---------------------------------------------------------------

ctl_prefix() {
	printf '%s/bin/dnvctl --gateway-address %s:%s --cluster %s --trace-id %s --timeout %s' \
		"$WORK" "$CP_IP" "$GW_PORT" "$CLUSTER" "$TRACE" "$CTL_TIMEOUT"
}

ctl_section() { # <framed> <n>
	printf '%s\n' "$1" | awk -v want="$2" -v frame="$FRAME" '
		BEGIN { sec = 1 }
		$0 == frame { sec++; next }
		sec == want { print }
	'
}

ctl_exec() { # <args…>
	local quoted remote framed
	quoted=$(printf '%q ' "$@")
	remote="$(ctl_prefix) $quoted"
	remote="$remote >$WORK/last.out 2>$WORK/last.err; echo \$? >$WORK/last.rc;"
	remote="$remote cat $WORK/last.rc; echo $FRAME;"
	remote="$remote cat $WORK/last.err; echo $FRAME;"
	remote="$remote cat $WORK/last.out"
	[ "$QUIET" -eq 1 ] || log "[cp] dnvctl $*"
	framed=$(ssh "${SSH_OPTS[@]}" "$CP" "$remote") ||
		die "ssh to cp failed while running: dnvctl $*"
	CTL_RC=$(ctl_section "$framed" 1)
	CTL_ERR=$(ctl_section "$framed" 2)
	CTL_OUT=$(ctl_section "$framed" 3)
	[ -n "$CTL_RC" ] || die "dnvctl $*: the framed reply carried no exit code"
}

ctl_ok() { # <args…>
	ctl_exec "$@"
	assert_eq "$CTL_RC" "0" "exit code of: dnvctl $*"
	assert_eq "$CTL_ERR" "" "stderr of: dnvctl $* must be empty"
	assert_parses "$CTL_OUT" "dnvctl $*"
}

ctl_try() { # <args…>
	ctl_exec "$@"
	[ "$CTL_RC" = 0 ]
}

ctl_create_try() { # <args…>
	ctl_exec "$@"
	if [ "$CTL_RC" = 0 ]; then
		return 0
	fi
	case "$CTL_ERR" in
	"dnvctl: ALREADY_EXISTS: "*) return 0 ;;
	esac
	return 1
}

gateway_serving() {
	if ctl_try cluster get --name "$CLUSTER"; then
		return 0
	fi
	case "$CTL_ERR" in
	"dnvctl: NOT_FOUND: "*) return 0 ;;
	esac
	return 1
}

dn_node_ready() { # <v>
	ctl_try dn inspect --addr "$(dn_addr "$1")" || return 1
	[ "$(jq_of "$CTL_OUT" '.dn_info.disk_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.dn_info.meta_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.dn_info.port_info.status')" = RES_STATUS_OK ]
}

cn_node_ready() { # <v>
	ctl_try cn inspect --addr "$(cn_addr "$1")" || return 1
	[ "$(jq_of "$CTL_OUT" '.cn_info.port_info.status')" = RES_STATUS_OK ]
}

ship_one() { # <target> <label> <file…>
	local target=$1 label=$2
	shift 2
	scp -q "${SSH_OPTS[@]}" "$@" "$target:$WORK/bin/" ||
		die "scp of the binaries to $label failed"
	ssh_to "$target" "chmod 0755 $WORK/bin/*" >/dev/null ||
		die "chmod of $WORK/bin on $label failed"
}

# dnv_up deploys the dnv stack and registers every node (doc/csi.md, The
# suite's dnv stack): the control plane on cp, one dn agent per
# disk-node guest, one cn agent per controller-node guest, the cluster.
dnv_up() {
	CASE=setup
	SETUP_DONE=1
	stage 01 "ship the binaries and create $WORK on every guest"
	local v h out
	helper_cp mkwork bin etcd gateway worker cdc >/dev/null || die "creating $WORK on cp failed"
	for v in "${!DN[@]}"; do
		helper_dn "$v" mkwork bin >/dev/null || die "creating $WORK on dn$v failed"
	done
	for v in "${!CN[@]}"; do
		helper_cn "$v" mkwork bin >/dev/null || die "creating $WORK on cn$v failed"
	done
	for h in "${!HOST[@]}"; do
		helper_host "$h" mkwork bin sanity >/dev/null || die "creating $WORK on host$h failed"
	done
	ship_one "$CP" cp "$ETCD_CACHE_DIR/$ETCD_DIST/etcd" "$GATEWAY_BIN" "$WORKER_BIN" "$CDC_BIN" "$DNVCTL_BIN"
	for v in "${!DN[@]}"; do ship_one "${DN[$v]}" "dn$v" "$AGENT_BIN"; done
	for v in "${!CN[@]}"; do ship_one "${CN[$v]}" "cn$v" "$AGENT_BIN"; done
	ship_one "${HOST[0]}" host0 "$CSI_BIN"
	for h in "${!HOST[@]}"; do
		out=$(helper_host "$h" mask) || die "host$h: the mask verb failed"
		assert_eq "$out" masked "host$h: the nvmf-connect@.service and nvmf-connect.target masks"
	done

	stage 02 "start etcd, dnv-gateway, dnv-worker and dnv-cdc on $CP_IP"
	start_cp_daemons
	wait_until "$WAIT_CP_READY" "the gateway on $CP_IP:$GW_PORT to answer \`cluster get\`" gateway_serving

	stage 03 "start ${#DN[@]} dn agents and ${#CN[@]} cn agents"
	for v in "${!DN[@]}"; do
		out=$(helper_dn "$v" dn_up "${DN_IP[$v]}" "$DN_GRPC_PORT" "$DN_TRSVCID" "$DN_PORT_ID" "$BACKING_SIZE") ||
			die "dn$v: dn_up failed: $out"
		log "  dn$v: $out"
	done
	for v in "${!CN[@]}"; do
		out=$(helper_cn "$v" cn_up "${CN_IP[$v]}" "$CN_GRPC_PORT" "$CN_TRSVCID") ||
			die "cn$v: cn_up failed: $out"
		log "  cn$v: $out"
	done

	stage 04 "cluster create --name $CLUSTER --extent-size $EXTENT_SIZE"
	ctl_ok cluster create --name "$CLUSTER" --extent-size "$EXTENT_SIZE"
	ctl_ok cluster get --name "$CLUSTER"
	CLUSTER_ID=$(jq_of "$CTL_OUT" '.cluster_id')
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.extent_size' "$EXTENT_SIZE" "the stored extent size"

	stage 05 "register ${#DN[@]} disk nodes and ${#CN[@]} controller nodes"
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_AGENT" "dn create $(dn_addr "$v") to be accepted" \
			ctl_create_try dn create --addr "$(dn_addr "$v")" \
			--tr-type tcp --adr-fam ipv4 --tr-addr "${DN_IP[$v]}" --tr-svc-id "$DN_TRSVCID" \
			--location "dn$v"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_AGENT" "cn create $(cn_addr "$v") to be accepted" \
			ctl_create_try cn create --addr "$(cn_addr "$v")" \
			--tr-type tcp --adr-fam ipv4 --tr-addr "${CN_IP[$v]}" --tr-svc-id "$CN_TRSVCID"
	done
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_PROVISION" "dn$v to report disk, header and port OK" dn_node_ready "$v"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_PROVISION" "cn$v to report its nvmet port" cn_node_ready "$v"
	done
	log "  the dnv stack serves cluster $CLUSTER ($CLUSTER_ID)"
}

# ---------------------------------------------------------------------------
# The image and the driver's deployment (doc/csi.md, Deployment)
# ---------------------------------------------------------------------------

# image_build builds the driver's image from the Dockerfile with the binary
# make build produced, and the smoke image of the k8s case.
image_build() {
	local ctx
	ctx=$(mktemp -d) || die "mktemp -d on the driver failed"
	cp "$CSI_BIN" "$ctx/dnv-csi" || die "copying $CSI_BIN failed"
	docker build -q -t "$IMAGE" -f "$DEPLOY_DIR/Dockerfile" "$ctx" >/dev/null 2>"$ctx/build.err" ||
		die "docker build of $IMAGE failed: $(cat "$ctx/build.err")"
	rm -rf "$ctx"
	log "  built $IMAGE on the driver"
}

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

# image_import ships an image into every Kubernetes node's store (CSI4).
image_import() { # <image>
	local target label out
	for target in "$CP" "${HOST[@]}"; do
		label=${target##*@}
		log "[$label] docker save $1 | k3s ctr images import -"
		out=$(docker save "$1" | ssh "${SSH_OPTS[@]}" "$target" "sudo -n bash $HELPER image_import" 2>&1) ||
			die "$label: importing $1 failed: $out"
		assert_eq "$out" imported "$label: image_import's answer"
	done
}

# driver_up applies the manifests, with a config map and a storage class of
# the suite's own in place of the examples, and waits for both plugins.
driver_up() {
	stage 06 "build the image and import it into every node"
	image_build
	image_import "$IMAGE"

	stage 07 "deploy the driver: $DRIVER_NAME in $DRIVER_NS"
	local f
	for f in namespace csidriver controller-serviceaccount node-serviceaccount \
		controller-clusterrole controller-clusterrolebinding; do
		kc_apply <"$DEPLOY_DIR/$f.yaml" || die "applying $f.yaml failed"
	done
	kc_apply <<EOF || die "applying the config map failed"
apiVersion: v1
kind: ConfigMap
metadata:
  name: dnv-csi-config
  namespace: $DRIVER_NS
data:
  gatewayAddress: "$CP_IP:$GW_PORT"
  cdcAddresses: "$CP_IP:$CDC_PORT"
EOF
	for f in controller node; do
		kc_apply <"$DEPLOY_DIR/$f.yaml" || die "applying $f.yaml failed"
	done
	kc_apply <<EOF || die "applying the storage class failed"
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SC_NAME
provisioner: $DRIVER_NAME
parameters:
  cluster: $CLUSTER
  redundancy: raid1
  cntlrs: "2"
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
	kc -n "$DRIVER_NS" rollout status "deploy/dnv-csi-controller" "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the controller plugin did not roll out"
	kc -n "$DRIVER_NS" rollout status "ds/dnv-csi-node" "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the node plugin did not roll out"
	kc_apply <<EOF || die "creating the test namespace failed"
apiVersion: v1
kind: Namespace
metadata:
  name: $TEST_NS
EOF
	log "  driver pods:"
	kc -n "$DRIVER_NS" get pods -o wide >&2 || true
}

k8s_ns_gone() { ! kc get namespace "$1" >/dev/null 2>&1; }

# driver_down removes the driver and the suite's Kubernetes objects; a
# cluster without k3s has nothing to remove. It reports, never dies.
driver_down() {
	local out ver
	out=$(QUIET=1 helper_cp_ok k3s_state 2>/dev/null) || true
	ver=$(k3s_version_of "$out")
	if [ -z "$ver" ] || [ "$ver" = none ] || [ "$(k3s_unit_of "$out" k3s)" != active ]; then
		log "  cp: no running k3s server, nothing to delete in Kubernetes"
		return 0
	fi
	kc_ok delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1
	if ! wait_cleanup "$WAIT_NS_GONE" k8s_ns_gone "$TEST_NS"; then
		log "WARNING: the namespace $TEST_NS is still there after ${WAIT_NS_GONE}s"
		CLEANUP_DIRTY=1
	fi
	kc_ok delete storageclass "$SC_NAME" --ignore-not-found >/dev/null 2>&1
	kc_ok delete namespace "$DRIVER_NS" --ignore-not-found --wait=false >/dev/null 2>&1
	if ! wait_cleanup "$WAIT_NS_GONE" k8s_ns_gone "$DRIVER_NS"; then
		log "WARNING: the namespace $DRIVER_NS is still there after ${WAIT_NS_GONE}s"
		CLEANUP_DIRTY=1
	fi
	kc_ok delete csidriver "$DRIVER_NAME" --ignore-not-found >/dev/null 2>&1
	kc_ok delete clusterrolebinding dnv-csi-controller --ignore-not-found >/dev/null 2>&1
	kc_ok delete clusterrole dnv-csi-controller --ignore-not-found >/dev/null 2>&1
}

# host_nqns reads the hosts' NQNs for the controller nodes' cleanup, which
# drops the nvmet host entries an attach made for them; at the start cleanup
# preflight has not read them yet. A host that does not answer is left out.
host_nqns() {
	local h out
	for h in "${!HOST[@]}"; do
		[ -n "${HOST_NQN[$h]:-}" ] && continue
		out=$(helper_host_ok "$h" facts)
		HOST_NQN[$h]=$(fact_of "$out" hostnqn)
	done
}

# ---------------------------------------------------------------------------
# Cleanup (CSI5): everything this suite started, and nothing else
# ---------------------------------------------------------------------------

# cleanup_verb runs one helper verb under the guest's own timeout and judges
# it by its sentinel; the ssh wrapper is a function of this file, so the
# timeout runs on the guest, not here.
cleanup_verb() { # <label> <sentinel> <ssh wrapper> [<index>] <verb…>
	local label=$1 want=$2 wrapper=$3 out
	shift 3
	if [ "$wrapper" = ssh_cp_sudo ]; then
		out=$(QUIET=1 "$wrapper" "timeout $CLEANUP_TIMEOUT bash $HELPER $*" 2>&1) || true
	else
		local idx=$1
		shift
		out=$(QUIET=1 "$wrapper" "$idx" "timeout $CLEANUP_TIMEOUT bash $HELPER $*" 2>&1) || true
	fi
	if printf '%s\n' "$out" | grep -qx "$want"; then
		printf '%s\n' "$out" | grep -v -x "$want" | grep -E 'wipe_left=|STUCK|REFUSED|STILL_RUNNING' |
			sed "s/^/  $label: /" >&2 || true
		return 0
	fi
	log "WARNING: $label: the cleanup verb did not reach '$want': $out"
	CLEANUP_DIRTY=1
	return 1
}

cleanup_all() {
	local h v
	CLEANUP_DIRTY=0
	# Holder first: the pods and the driver, then the hosts' connections, then
	# the controller nodes, the disk nodes and the control plane.
	driver_down
	for h in "${!HOST[@]}"; do
		cleanup_verb "host$h" cleaned ssh_host "$h" host_cleanup "$CP_IP" || true
	done
	host_nqns
	for v in "${!CN[@]}"; do
		cleanup_verb "cn$v" cleaned ssh_cn "$v" cn_cleanup "${HOST_NQN[@]}" || true
	done
	for v in "${!DN[@]}"; do
		cleanup_verb "dn$v" cleaned ssh_dn "$v" dn_cleanup || true
	done
	cleanup_verb cp cleaned ssh_cp_sudo cp_cleanup || true
	for h in "${!HOST[@]}"; do
		cleanup_verb "host$h" image-rm-done ssh_host "$h" image_rm "$IMAGE_REF" || true
		cleanup_verb "host$h" image-rm-done ssh_host "$h" image_rm "$SMOKE_IMAGE_REF" || true
	done
	cleanup_verb cp image-rm-done ssh_cp_sudo image_rm "$IMAGE_REF" || true
	cleanup_verb cp image-rm-done ssh_cp_sudo image_rm "$SMOKE_IMAGE_REF" || true
	docker image rm "$IMAGE" "$SMOKE_IMAGE" >/dev/null 2>&1 || true
	local dir
	for dir in etcd gateway worker cdc; do unset "RUNNING[$dir]" 2>/dev/null || true; done
	log "  cleanup done (dirty=$CLEANUP_DIRTY)"
}

# ---------------------------------------------------------------------------
# Diagnostics and kept logs
# ---------------------------------------------------------------------------

diagnostics() {
	local h v
	QUIET=1
	log "--- kubectl get nodes -o wide"
	kc_ok get nodes -o wide >&2
	log "--- kubectl get pods -A -o wide"
	kc_ok get pods -A -o wide >&2
	log "--- kubectl get pvc,pv,volumeattachments -A"
	kc_ok get pvc,pv,volumeattachments -A >&2
	log "--- kubectl describe pods -n $TEST_NS"
	kc_ok describe pods -n "$TEST_NS" >&2
	log "--- kubectl get events -n $TEST_NS"
	kc_ok get events -n "$TEST_NS" --sort-by=.lastTimestamp >&2
	log "--- driver pod logs"
	kc_ok -n "$DRIVER_NS" logs deploy/dnv-csi-controller -c dnv-csi --tail=60 >&2
	kc_ok -n "$DRIVER_NS" logs deploy/dnv-csi-controller -c csi-provisioner --tail=30 >&2
	kc_ok -n "$DRIVER_NS" logs ds/dnv-csi-node -c dnv-csi --tail=60 >&2
	log "--- dnvctl sp list"
	# In a subshell: ctl_exec dies when cp cannot run dnvctl, and a dump
	# must not.
	(ctl_exec sp list && printf '%s\n%s\n' "$CTL_OUT" "$CTL_ERR") 2>/dev/null >&2 || true
	helper_cp_ok diag >&2
	for h in "${!HOST[@]}"; do helper_host_ok "$h" diag >&2; done
	for v in "${!CN[@]}"; do helper_cn_ok "$v" diag >&2; done
	for v in "${!DN[@]}"; do helper_dn_ok "$v" diag >&2; done
	QUIET=0
}

keep_logs() {
	local dir h v
	QUIET=1
	dir="$KEEP_LOGS/cp-$CP_IP"
	mkdir -p "$dir" && helper_cp_ok journal 2000 >"$dir/k3s.log" 2>/dev/null || true
	scp -q "${SSH_OPTS[@]}" "$CP:$WORK/*/*.log" "$dir/" 2>/dev/null || true
	for h in "${!HOST[@]}"; do
		dir="$KEEP_LOGS/host$h-${HOST_IP[$h]}"
		mkdir -p "$dir" && helper_host_ok "$h" journal 2000 >"$dir/k3s.log" 2>/dev/null || true
		scp -q "${SSH_OPTS[@]}" "${HOST[$h]}:$WORK/sanity/*.log" "$dir/" 2>/dev/null || true
	done
	for v in "${!CN[@]}"; do
		dir="$KEEP_LOGS/cn$v-${CN_IP[$v]}"
		mkdir -p "$dir" && scp -q "${SSH_OPTS[@]}" "${CN[$v]}:$WORK/cn/agent.log" "$dir/" 2>/dev/null || true
	done
	for v in "${!DN[@]}"; do
		dir="$KEEP_LOGS/dn$v-${DN_IP[$v]}"
		mkdir -p "$dir" && scp -q "${SSH_OPTS[@]}" "${DN[$v]}:$WORK/dn/agent.log" "$dir/" 2>/dev/null || true
	done
	kc_ok -n "$DRIVER_NS" logs deploy/dnv-csi-controller -c dnv-csi --tail=-1 >"$KEEP_LOGS/controller-plugin.log" 2>/dev/null || true
	kc_ok -n "$DRIVER_NS" logs ds/dnv-csi-node -c dnv-csi --tail=-1 >"$KEEP_LOGS/node-plugin.log" 2>/dev/null || true
	QUIET=0
	log "  logs kept under $KEEP_LOGS"
}

# ---------------------------------------------------------------------------
# Kubernetes helpers of the cases
# ---------------------------------------------------------------------------

pod_phase() { kc get pod -n "$TEST_NS" "$1" -o 'jsonpath={.status.phase}' 2>/dev/null; }
pod_running() { [ "$(pod_phase "$1")" = Running ]; }
pod_node() { kc get pod -n "$TEST_NS" "$1" -o 'jsonpath={.spec.nodeName}' 2>/dev/null; }
pvc_bound() { [ "$(kc get pvc -n "$TEST_NS" "$1" -o 'jsonpath={.status.phase}' 2>/dev/null)" = Bound ]; }
pvc_volume() { kc get pvc -n "$TEST_NS" "$1" -o 'jsonpath={.spec.volumeName}' 2>/dev/null; }
pod_gone() { ! kc get pod -n "$TEST_NS" "$1" >/dev/null 2>&1; }
pvc_gone() { ! kc get pvc -n "$TEST_NS" "$1" >/dev/null 2>&1; }
pv_gone() { ! kc get pv "$1" >/dev/null 2>&1; }

# pod_marked answers whether a pod's command wrote its done marker.
pod_marked() { # <pod> <marker path>
	[ "$(kc_exec "$1" sh -c "cat $2 2>/dev/null" 2>/dev/null)" = done ]
}

# claim applies one claim of the suite's class.
claim() { # <name> <size> <Filesystem|Block>
	kc_apply <<EOF || die "applying the claim $1 failed"
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  namespace: $TEST_NS
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $SC_NAME
  volumeMode: $3
  resources:
    requests:
      storage: $2
EOF
}

# io_pod applies a pod on a host that runs a shell script over a claim
# mounted at /data (a filesystem claim) or given as /dev/dnv (a block claim).
io_pod() { # <pod> <claim> <host index> <Filesystem|Block> <script>
	local volume
	if [ "$4" = Block ]; then
		volume="      volumeDevices:
        - name: vol
          devicePath: /dev/dnv"
	else
		volume="      volumeMounts:
        - name: vol
          mountPath: /data"
	fi
	kc_apply <<EOF || die "applying the pod $1 failed"
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $TEST_NS
spec:
  nodeSelector:
    kubernetes.io/hostname: ${HOST_NAME[$3]}
  restartPolicy: Never
  terminationGracePeriodSeconds: 2
  containers:
    - name: io
      image: $IMAGE
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c"]
      args:
        - |
$(printf '%s\n' "$5" | sed 's/^/          /')
$volume
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: $2
EOF
}

# The scripts the IO pods run. A pattern of a size in MiB is written with
# fsync and its digest kept; a check re-reads and compares; the marker says
# the step is done; then the pod sleeps until it is deleted.
write_fs_script() { # <MiB>
	cat <<EOF
set -e
dd if=/dev/urandom of=/data/pattern bs=1M count=$1 conv=fsync status=none
sha256sum /data/pattern > /data/sha
sync
echo done > /data/marker
exec sleep 1000000
EOF
}

check_fs_script() {
	cat <<'EOF'
set -e
sha256sum -c /data/sha > /tmp/check 2>&1 && echo done > /tmp/marker || echo failed > /tmp/marker
exec sleep 1000000
EOF
}

write_block_script() { # <MiB>
	cat <<EOF
set -e
dd if=/dev/urandom of=/tmp/pattern bs=1M count=$1 status=none
dd if=/tmp/pattern of=/dev/dnv bs=1M count=$1 conv=fsync status=none
want=\$(sha256sum < /tmp/pattern | cut -d' ' -f1)
got=\$(dd if=/dev/dnv bs=1M count=$1 status=none | sha256sum | cut -d' ' -f1)
if [ "\$want" = "\$got" ]; then echo done > /tmp/marker; else echo failed > /tmp/marker; fi
exec sleep 1000000
EOF
}

# pod_settled answers once a pod runs or has ended, so a pod whose script
# fails stops the wait at once instead of at its bound.
pod_settled() { # <pod>
	case "$(pod_phase "$1")" in
	Running | Failed | Succeeded) return 0 ;;
	esac
	return 1
}

# run_pod applies a pod and waits for it to run and mark its step done.
run_pod() { # <pod> <claim> <host index> <mode> <script> [marker]
	io_pod "$1" "$2" "$3" "$4" "$5"
	wait_until "$WAIT_POD" "pod $1 Running on host$3" pod_settled "$1"
	if ! pod_running "$1"; then
		kc_ok logs -n "$TEST_NS" "$1" --tail=20 >&2
		die "pod $1 ended with phase $(pod_phase "$1") instead of running"
	fi
	assert_eq "$(pod_node "$1")" "${HOST_NAME[$3]}" "the node of $1"
	wait_until "$WAIT_POD" "pod $1 to finish its step" pod_marked "$1" "${6:-/data/marker}"
}

delete_pod() { # <pod>
	kc delete pod -n "$TEST_NS" "$1" --ignore-not-found --wait=false >/dev/null ||
		die "deleting pod $1 failed"
	wait_until "$WAIT_POD" "pod $1 gone" pod_gone "$1"
}

delete_claim() { # <claim>
	local pv
	pv=$(pvc_volume "$1")
	kc delete pvc -n "$TEST_NS" "$1" --ignore-not-found --wait=false >/dev/null ||
		die "deleting claim $1 failed"
	wait_until "$WAIT_PVC" "claim $1 gone" pvc_gone "$1"
	[ -z "$pv" ] || wait_until "$WAIT_PVC" "volume $pv gone" pv_gone "$pv"
}

# --- residue -------------------------------------------------------------------

no_pools() {
	ctl_try sp list || return 1
	[ "$(jq_of "$CTL_OUT" '.sp_name | length')" = 0 ]
}

# residue_empty answers whether a node's residue listing is empty; the
# agents sweep their devices on the syncup that follows the drain, a few
# seconds after the pool's records are gone, so the callers wait.
residue_empty() { # <helper call…>
	local out
	out=$(QUIET=1 "$@") || return 1
	[ "$(printf '%s' "$out" | grep -c . || true)" = 0 ]
}

# nothing_left is the ending every volume case shares: no pool in the
# cluster, no dm device, array or export on a node, no connection on a host.
nothing_left() {
	local v h
	wait_until "$WAIT_DRAIN" "every pool of the cluster to be drained" no_pools
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_HOST_CLEAN" "cn$v to hold no device, array or export" \
			residue_empty helper_cn "$v" cn_residue
	done
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_HOST_CLEAN" "dn$v to hold no device or export" \
			residue_empty helper_dn "$v" dn_residue
	done
	for h in "${!HOST[@]}"; do
		wait_until "$WAIT_HOST_CLEAN" "host$h to hold no connection of the driver's" host_clean "$h"
	done
}

host_clean() { # <h>
	local out
	out=$(QUIET=1 helper_host "$1" host_residue "$CP_IP") || return 1
	[ "$(printf '%s' "$out" | grep -c . || true)" = 0 ]
}

# ---------------------------------------------------------------------------
# The cases (doc/csi.md, The cases)
# ---------------------------------------------------------------------------

pod_name() { printf 'smoke-host%s' "$1"; }

# case_k8s proves the Kubernetes floor: the smoke image, imported into every
# node, runs as a pod on each host, prints its line, and is removed with its
# pods.
case_k8s() {
	CASE=k8s
	local h pod out
	stage 01 "build the smoke image on the driver and import it into every node"
	smoke_image_build
	image_import "$SMOKE_IMAGE"
	stage 02 "a pod on each host from the imported image"
	for h in "${!HOST[@]}"; do
		kc_apply <<EOF || die "applying the smoke pod failed"
apiVersion: v1
kind: Pod
metadata:
  name: $(pod_name "$h")
  namespace: $TEST_NS
spec:
  nodeSelector:
    kubernetes.io/hostname: ${HOST_NAME[$h]}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: smoke
    image: $SMOKE_IMAGE
    imagePullPolicy: Never
EOF
	done
	for h in "${!HOST[@]}"; do
		pod=$(pod_name "$h")
		wait_until "$WAIT_POD" "pod $pod Running" pod_running "$pod"
		assert_eq "$(pod_node "$pod")" "${HOST_NAME[$h]}" "the node of $pod"
	done
	stage 03 "each pod printed its line, and the pods go"
	for h in "${!HOST[@]}"; do
		pod=$(pod_name "$h")
		out=$(kc logs -n "$TEST_NS" "$pod") || die "reading the logs of $pod failed"
		assert_eq "$out" "$SMOKE_LINE" "the log of $pod"
		delete_pod "$pod"
	done
	log "=== $CASE: PASS"
}

# case_volume: a claim, a pod on host0 that writes and reads, the pod on
# host1 reading the data back, a raw block claim, then every delete and the
# check that nothing remains (doc/csi.md, The cases).
# capacity_published answers whether the provisioner holds a capacity object
# of the suite's class with a capacity above zero (csi.md CSI19): the one
# place Kubernetes shows dnv's free space, and the one the provisioner
# deletes at once for a driver without a topology segment (CSI13).
capacity_published() {
	local out
	out=$(kc get csistoragecapacities -A -o jsonpath="{range .items[?(@.storageClassName==\"$SC_NAME\")]}{.capacity}{\"\n\"}{end}" 2>/dev/null) || return 1
	[ -n "$out" ] && ! printf '%s\n' "$out" | grep -qxE '0|'
}

case_volume() {
	CASE=volume
	stage 01 "a filesystem claim and a pod on host0 that writes a pattern"
	wait_until "$WAIT_CAPACITY" "the capacity object of the class $SC_NAME" capacity_published
	log "  capacity of $SC_NAME: $(kc_ok get csistoragecapacities -A -o jsonpath="{range .items[?(@.storageClassName==\"$SC_NAME\")]}{.capacity} (largest volume {.maximumVolumeSize}){end}" 2>/dev/null)"
	claim v0 1Gi Filesystem
	wait_until "$WAIT_PVC" "claim v0 Bound" pvc_bound v0
	local pv
	pv=$(pvc_volume v0)
	log "  claim v0 is volume $pv"
	ctl_ok sp get --sp "$pv"
	assert_field "$CTL_OUT" '.sp_conf.slice_id_list | length' 1 "the volume's pool has one slice"
	assert_field "$CTL_OUT" '.sp_conf.cntlr_id_list | length' 2 "the volume's pool has two cntlrs"
	run_pod w0 v0 0 Filesystem "$(write_fs_script 64)"
	delete_pod w0

	stage 02 "the pod on host1 reads the pattern back"
	run_pod r1 v0 1 Filesystem "$(check_fs_script)" /tmp/marker
	assert_eq "$(kc_exec r1 cat /tmp/check)" "/data/pattern: OK" "host1 reads host0's pattern"
	delete_pod r1

	stage 03 "a raw block claim, written and read through the device"
	claim b0 512Mi Block
	wait_until "$WAIT_PVC" "claim b0 Bound" pvc_bound b0
	run_pod wb b0 0 Block "$(write_block_script 16)" /tmp/marker
	delete_pod wb

	stage 04 "every delete, and nothing left"
	delete_claim v0
	delete_claim b0
	nothing_left
	log "=== $CASE: PASS"
}

# primary_of prints the addr_port of a pool's primary cntlr.
primary_of() { # <pool>
	ctl_try sp get --sp "$1" || return 1
	jq_of "$CTL_OUT" '.cntlr_list[] | select(.primary == true) | .addr_port'
}

primary_moved() { # <pool> <old addr_port>
	local now
	now=$(primary_of "$1") || return 1
	[ -n "$now" ] && [ "$now" != "$2" ]
}

cn_index_of() { # <addr_port>
	local v
	for v in "${!CN[@]}"; do
		[ "$(cn_addr "$v")" = "$1" ] && {
			printf '%s' "$v"
			return 0
		}
	done
	return 1
}

# case_failover: a pod writes in a loop while the primary's cn agent is cut
# off; after the failover the writer has failed nothing, the stall is
# reported, and the pattern written before checks out (doc/csi.md, The
# cases).
case_failover() {
	CASE=failover
	stage 01 "a claim and a pod on host0 writing a pattern, then a write loop"
	claim f0 1Gi Filesystem
	wait_until "$WAIT_PVC" "claim f0 Bound" pvc_bound f0
	local pv old v stat count fail max
	pv=$(pvc_volume f0)
	run_pod wf f0 0 Filesystem "$(cat <<'EOF'
set -e
dd if=/dev/urandom of=/data/pattern bs=1M count=32 conv=fsync status=none
sha256sum /data/pattern > /data/sha
sync
echo done > /data/marker
i=0; fail=0; max=0
while [ ! -f /data/stop ]; do
  s=$(date +%s%N)
  if dd if=/dev/zero of=/data/w bs=1M count=1 conv=fsync status=none 2>/dev/null; then :; else fail=$((fail+1)); fi
  e=$(date +%s%N)
  d=$(( (e - s) / 1000000 ))
  [ "$d" -gt "$max" ] && max=$d
  i=$((i+1))
  echo "$i $fail $max" > /data/stat.tmp
  mv /data/stat.tmp /data/stat
done
echo "$i $fail $max" > /data/stat
echo done > /data/stopped
exec sleep 1000000
EOF
)"
	old=$(primary_of "$pv") || die "reading the primary of $pv failed"
	v=$(cn_index_of "$old") || die "the primary $old is on no --cn guest"
	log "  the primary of $pv is $old (cn$v)"
	sleep 5

	stage 02 "cut the primary's cn agent off (SIGSTOP) and wait for the failover"
	stat=$(helper_cn "$v" sig_pidfile "$WORK/cn/pid" STOP) || die "cn$v: sig_pidfile failed"
	assert_eq "$stat" "state=T" "cn$v: the agent is stopped"
	wait_until "$WAIT_FAILOVER" "the primary of $pv to move away from $old" primary_moved "$pv" "$old"
	log "  the primary moved to $(primary_of "$pv")"
	# IO goes on against the new primary for a while before the loop stops.
	sleep 20
	kc_exec wf sh -c 'touch /data/stop' >/dev/null || die "stopping the write loop failed"
	wait_until "$WAIT_POD" "the write loop to stop" pod_marked wf /data/stopped
	stat=$(kc_exec wf cat /data/stat) || die "reading the loop's statistics failed"
	read -r count fail max <<<"$stat"
	log "  writes: $count, failed: $fail, longest: $max ms"
	assert_ge "$count" 1 "the write loop wrote"
	assert_eq "$fail" 0 "writes failed during the failover"

	# host0's page cache holds the pattern, so the check reads it on host1,
	# through the new primary, with the old one still stopped.
	stage 03 "the pod goes, and a pod on host1 reads the pattern back"
	delete_pod wf
	run_pod rf f0 1 Filesystem "$(check_fs_script)" /tmp/marker
	assert_eq "$(kc_exec rf cat /tmp/check)" "/data/pattern: OK" \
		"the pattern survives the failover"
	delete_pod rf

	stage 04 "give the agent back, delete, and nothing left"
	stat=$(helper_cn "$v" sig_pidfile "$WORK/cn/pid" CONT) || die "cn$v: sig_pidfile failed"
	log "  cn$v agent $stat"
	delete_claim f0
	nothing_left
	log "=== $CASE: PASS"
}

# case_many: eight claims and eight pods at once, four per host, each
# writing and reading, then all deleted and nothing left.
case_many() {
	CASE=many
	local i h pods=()
	stage 01 "eight claims and eight pods at once, four per host"
	for i in $(seq 0 7); do
		claim "m$i" 1Gi Filesystem
	done
	for i in $(seq 0 7); do
		wait_until "$WAIT_PVC" "claim m$i Bound" pvc_bound "m$i"
	done
	for i in $(seq 0 7); do
		h=$((i % 2))
		io_pod "p$i" "m$i" "$h" Filesystem "$(write_fs_script 16)"
		pods+=("p$i")
	done
	for i in $(seq 0 7); do
		wait_until "$WAIT_POD" "pod p$i Running" pod_running "p$i"
		wait_until "$WAIT_POD" "pod p$i to finish its step" pod_marked "p$i" /data/marker
		assert_eq "$(kc_exec "p$i" sh -c 'sha256sum -c /data/sha 2>&1')" "/data/pattern: OK" \
			"pod p$i reads its pattern"
	done
	ctl_ok sp list
	assert_field "$CTL_OUT" '.sp_name | length' 8 "eight pools in the cluster"
	stage 02 "every delete, and nothing left"
	for i in $(seq 0 7); do delete_pod "p$i"; done
	for i in $(seq 0 7); do delete_claim "m$i"; done
	nothing_left
	log "=== $CASE: PASS"
}

# sanity_build builds csi-sanity once from its pinned module, in a module of
# its own so the tree's go.mod never carries it.
sanity_build() {
	[ -x "$SANITY_BIN" ] && return 0
	local tmp
	tmp=$(mktemp -d) || die "mktemp -d on the driver failed"
	(cd "$tmp" && go mod init sanitybuild >/dev/null 2>&1 &&
		GOFLAGS=-mod=mod go get "$SANITY_MODULE@$SANITY_VERSION" >&2 &&
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=mod go build -o "$SANITY_BIN" "$SANITY_MODULE/cmd/csi-sanity" >&2) ||
		die "building csi-sanity $SANITY_VERSION failed"
	rm -rf "$tmp"
	log "  built $SANITY_BIN"
}

# case_sanity: csi-sanity against both plugins run as plain processes on
# host0, with the driver's pods taken down first, since two node plugins on
# one host would follow each other's subsystems.
case_sanity() {
	CASE=sanity
	stage 01 "take the driver's pods down and build csi-sanity"
	driver_down
	[ "$CLEANUP_DIRTY" -eq 0 ] || die "taking the driver down did not finish"
	wait_until "$WAIT_HOST_CLEAN" "host0 to hold no connection of the driver's" host_clean 0
	sanity_build
	scp -q "${SSH_OPTS[@]}" "$SANITY_BIN" "${HOST[0]}:$WORK/bin/" || die "scp of csi-sanity to host0 failed"
	ssh_host_user 0 "chmod 0755 $WORK/bin/csi-sanity" >/dev/null || die "chmod of csi-sanity failed"

	stage 02 "start both plugins on host0 and run csi-sanity"
	local out
	out=$(helper_host 0 sanity_start "$CP_IP:$GW_PORT" "$CP_IP:$CDC_PORT") ||
		die "host0: starting the plugins failed: $out"
	ssh_host_user 0 "printf 'cluster: %s\nredundancy: raid1\ncntlrs: \"2\"\n' $CLUSTER > $WORK/sanity/params.yaml" >/dev/null ||
		die "writing the sanity parameters failed"
	local rc=0
	timeout "$SANITY_TIMEOUT" ssh "${SSH_OPTS[@]}" "${HOST[0]}" \
		"sudo -n $WORK/bin/csi-sanity --csi.endpoint unix://$WORK/sanity/node.sock --csi.controllerendpoint unix://$WORK/sanity/ctl.sock --csi.mountdir $WORK/sanity/mnt --csi.stagingdir $WORK/sanity/stage --csi.testvolumeparameters $WORK/sanity/params.yaml --csi.testvolumesize 268435456 --ginkgo.v --ginkgo.no-color > $WORK/sanity/sanity.log 2>&1" ||
		rc=$?
	out=$(ssh_host_user 0 "grep -E '^(Ran|FAIL|SUCCESS|\\[FAIL\\]|  \\[FAILED\\])|Summarizing' $WORK/sanity/sanity.log | head -40") || true
	log "$out"
	if [ "$rc" -ne 0 ]; then
		ssh_host_user 0 "grep -B 2 -A 12 '\\[FAILED\\]' $WORK/sanity/sanity.log | head -200" >&2 || true
		die "csi-sanity failed (exit $rc); its log is $WORK/sanity/sanity.log on host0"
	fi
	stage 03 "stop the plugins, and nothing left"
	helper_host 0 sanity_stop >/dev/null || die "host0: stopping the plugins failed"
	nothing_left
	log "=== $CASE: PASS"
}

run_case() { "case_$1"; }

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
		resolve_jq
		ship_helpers
		cleanup_all
		[ "$CLEANUP_DIRTY" -eq 0 ] || return 1
		return 0
	fi

	preflight_driver
	ship_helpers

	# The start cleanup comes BEFORE the guest checks: a port check taken
	# before it would judge the last run's leftovers, which a failed run
	# leaves in place on purpose.
	STAGE="start cleanup"
	log ""
	log "=== start cleanup (CSI5, unconditional, tolerant of total absence)"
	cleanup_all
	[ "$CLEANUP_DIRTY" -eq 0 ] || die "the start cleanup did not finish"

	preflight_guests
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

	dnv_up
	driver_up
	for name in "${RUN_CASES[@]}"; do
		run_case "$name"
	done
	return 0
}

main "$@"
