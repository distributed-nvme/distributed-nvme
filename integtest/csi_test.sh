#!/usr/bin/env bash
#
# csi_test.sh — the Kubernetes integration test of csi.md (Integration
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
# Cases, in order: k8s, policy, volume, failover, restart, detach, many, fill,
# machine, sanity. `--only` picks one.
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
#     subcommand over ssh (CSI3). The driver machine installs no package:
#     besides the tree's own builds and the images it puts only csi-sanity,
#     the pinned etcd release and, with no jq, gojq into integtest/bin.
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
# Spec: csi.md (rules CSI1..).

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants (csi.md, Topology and parameters)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# The udev rules every node that runs a dnv agent carries (layout.md,
# Directory tree): the helper carries their texts as this checkout holds them.
UDEV_SRC="$REPO_ROOT/deploy/udev"
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
# and the storage classes and namespace the cases use: the cases' class, and
# one that asks for xfs.
DRIVER_NAME=csi.dnv.io
DRIVER_NS=dnv-csi
TEST_NS=dnv-csi-test
SC_NAME=dnv-csi-it
SC_XFS=dnv-csi-it-xfs
# The namespace of the policy case's control pod: no network policy holds
# it, so a port it reaches is open, and the test namespace's policy is what
# closes it there (csi.md, The cases, policy).
OPEN_NS=dnv-csi-open
# The sidecar images the manifests name, which driver_up pulls on every node
# so that a controller copy that moves starts without a pull.
SIDECARS=$(awk '/image: registry.k8s.io/ {print $2}' "$DEPLOY_DIR/controller.yaml" "$DEPLOY_DIR/node.yaml" | sort -u | tr '\n' ' ')
# The cut of a host, the machine case's whole one and the detach case's from
# the control-plane guest alone: its nft table and the systemd unit that
# removes it after CUT_LIMIT seconds, whatever becomes of the run.
CUT_TABLE=dnv_csi_cut
CUT_UNIT=dnv-csi-cut-undo
CUT_LIMIT=600
# The taint by which the detach case tells Kubernetes that a node it cannot
# reach is out of service: Kubernetes then force-deletes the node's pods and
# detaches their volumes without waiting for the node.
OOS_TAINT_KEY=node.kubernetes.io/out-of-service
OOS_TAINT=$OOS_TAINT_KEY=nodeshutdown:NoExecute
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
# nvmet's debug directory, one directory per subsystem and one ctrl<cntlid>
# per host's nvmet connection, which a kernel built with
# CONFIG_NVME_TARGET_DEBUGFS shows once nvmet is loaded.
DBG_NVMET=/sys/kernel/debug/nvmet

# The dnv stack's ports: this suite's own block, bound by no other suite
# (e2e_test.sh's inventory lists theirs). k3s holds its own well-known ports
# on the three Kubernetes guests.
ETCD_CLIENT_PORT=17379
ETCD_PEER_PORT=17380
GW_PORT=29870
CDC_PORT=18030
# A port on the control-plane guest no cdc listens on: the failover case
# puts it in the controller plugin's cdc list, so every attach's publish
# context names a dead cdc, which the node plugin must not follow (csi.md
# CSI15).
DEAD_CDC_PORT=18039
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
# How long a host may keep its path to a failed-over primary once the
# primary has moved: the cdc's change notice starts the node plugin's pass
# at once (csi.md CSI15).
WAIT_PATH_DROP=60
# How long a node plugin that comes back after a failover it missed may take
# to drop the old primary's path: its first pass runs at its start, and its
# timer, which would otherwise find the change, is 30 s away (csi.md CSI15).
RESTART_DROP=10
# How long a namespace's network policy may take to hold for a new pod
# (about a second on the lab's k3s); the policy case retries within it.
WAIT_POLICY=30
# How long the controller copy that is not cut off may take to hold both
# leases after the cut: one lease time and one retry period of the sidecars'
# election, about 20 s on the lab, with a margin.
WAIT_LEASE=60
# How long a new controller copy may take to run on another node while its
# host stays cut: the node controller's grace before it marks the node
# unreachable (40 s on the lab's k3s), the copies' 30 s toleration of that
# mark, and a start; the default toleration, 300 s, would pass it.
WAIT_REPLACE=150
# How long a host cut off from the control plane may take to read NotReady:
# the node controller's grace, 40 s on the lab's k3s, with a margin.
WAIT_NODE_LOST=150
# How long the detach case's forced detach may take, from the out-of-service
# taint to its write and, once the worker goes on, to the VolumeAttachment's
# end: the pod's force delete, then the attacher's calls, each of which waits
# for the primary's cut and is repeated with a backoff when its own deadline
# passes.
WAIT_DETACH=240
# How long the detach case keeps the worker stopped once the detach has
# taken host0 off the list, so that no cn agent cuts: longer than one call
# of the attacher, whose deadline is 15 s by default, so that the detach is
# seen to wait through a repeat too, and the attach to host1, which
# Kubernetes asks for once that call has failed, is seen waiting.
HOLD_CUT=20
# How long the cut-off host's writes may go on after the primary's cut ends
# its nvmet connection: its reconnect, after the kernel's default delay of
# 10 s, is refused for good, and its controllers go, about 12 s after the cut
# on the lab, with a margin.
WAIT_HOST_IO_FAIL=90
WAIT_NS_GONE=300
# The fill case's volume (csi.md, The cases): 4 GiB, which keeps what the
# fill writes into the disk node guests to 8 GiB through RAID1, and the
# bound on its fio run, the write of the whole device and the read back.
FILL_SIZE=4Gi
FILL_BYTES=4294967296
WAIT_FILL=1200
WAIT_HOST_CLEAN=120
SANITY_TIMEOUT=2400
CLEANUP_TIMEOUT=600
CTL_TIMEOUT_DEFAULT=30
CTL_TIMEOUT=$CTL_TIMEOUT_DEFAULT
FRAME=@@csi-frame@@

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15
	-o ServerAliveCountMax=4 -o LogLevel=ERROR)

CASES=(k8s policy volume failover restart detach many fill machine sanity)

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
CUT_TRIED=0
TAINT_TRIED=0
RUN_CASES=()
CP_NAME=""
HOST_NAME=()
HOST_NQN=()
SMOKE_DIR=""
JQ=""
CTL_RC=""
CTL_ERR=""
CTL_OUT=""
# The last capacity object and gateway answer capacity_is_the_gateways read.
CAPACITY_SEEN=""
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
	# A signal while this handler runs: a Ctrl-C (SIGINT) reaches the
	# command in hand, which it ends unless that command ignores it, a read
	# of a command's output and an ssh still connecting among them, and the
	# dump and the removals below go on; a SIGHUP, which a terminal that
	# closes sends twice, is ignored here and by every command below, and so
	# is a SIGPIPE, so that a reader of the run's output that the same
	# signal ended, as tee, makes a write fail and does not end the run; a
	# SIGTERM ends the run at once. Output that has failed when the handler
	# starts, as a terminal that has closed or a tee that a Ctrl-C ended,
	# goes to /dev/null from then on: this line tries a write first. Output
	# that fails later, partway through the handler, can still end it at a
	# log line before its removals; a run under the runner writes to a file
	# and does not meet it. rc keeps the code the run was ending
	# with, and the run exits with it but on a SIGTERM and after an end
	# cleanup that did not finish, as one whose verb a Ctrl-C ended, which
	# turns rc to 1 below.
	trap - TERM
	trap : INT
	trap '' HUP
	trap '' PIPE
	printf '\n' >&2 2>/dev/null || exec >/dev/null 2>&1
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
		# A failing run leaves its debris in place, but never a cut or the
		# out-of-service taint: a host cut off or tainted breaks every later
		# run (CSI5). The taint goes first, while the node is still cut, so
		# that the node never comes back under it, which would evict its
		# pods.
		if [ "$TAINT_TRIED" -eq 1 ]; then
			untaint_hosts
		fi
		if [ "$CUT_TRIED" -eq 1 ]; then
			uncut_hosts
		fi
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

# udev_rules_source prints the helper's copy of the udev rules: one
# UDEV_RULES entry per file of $UDEV_SRC, keyed by its name, holding its
# text, which install_udev_rules writes on a node guest. It fails when
# $UDEV_SRC holds no rule file.
udev_rules_source() {
	local f
	printf 'declare -A UDEV_RULES=()\n'
	for f in "$UDEV_SRC"/*.rules; do
		[ -f "$f" ] || {
			echo "udev_rules_source: no rule file in $UDEV_SRC" >&2
			return 1
		}
		printf 'UDEV_RULES[%q]=%q\n' "${f##*/}" "$(cat "$f")"
	done
}

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
UDEV_DIR=/etc/udev/rules.d
ETCD_NAME="$ETCD_NAME"
TRSVCID_MIN="$TRSVCID_MIN"
TRSVCID_MAX="$TRSVCID_MAX"
K3S_INSTALL_URL="$K3S_INSTALL_URL"
SMOKE_IMAGE_REF="$SMOKE_IMAGE_REF"
IMAGE_REF="$IMAGE_REF"
CUT_TABLE="$CUT_TABLE"
CUT_UNIT="$CUT_UNIT"
DBG_NVMET="$DBG_NVMET"
DISC_NQN=nqn.2014-08.org.nvmexpress.discovery
EOF
	udev_rules_source || return 1
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

# listen_addrs <port> prints the local address of every socket that listens
# on the port, one per line.
listen_addrs() { # <port>
	ss -ltnH "sport = :$1" 2>/dev/null | awk '{print $4}' | sed 's/:[0-9]*$//' | sort -u
	return 0
}

# --- the cuts of a host (csi.md, The cases, machine and detach) ---------------

# net_cut drops every packet of the machine but the loopback's and ssh's, so
# that Kubernetes loses the node and the suite keeps its shell, and schedules
# the cut's own removal after <secs> seconds through a systemd timer, so that
# a run killed before its cleanup leaves no cut behind. It refuses on a guest
# that is not a k3s agent, so the k3s server is never cut. Every verb of the
# cuts (net_cut, net_cut_peer, net_uncut, net_cut_state) carries the net_
# prefix: a helper function named after a command, such as cut, would shadow
# that command in every verb (ship_helpers refuses such a name).
net_cut() { # <secs>
	if ! systemctl is-active --quiet k3s-agent || systemctl is-active --quiet k3s; then
		echo "net_cut: refused: this guest is not a k3s agent" >&2
		return 1
	fi
	net_uncut >/dev/null
	systemd-run --quiet --unit="$CUT_UNIT" --on-active="$1" \
		/usr/sbin/nft delete table inet "$CUT_TABLE" || return 1
	nft -f - <<NFT || return 1
table inet $CUT_TABLE {
	chain input {
		type filter hook input priority -400; policy drop;
		iif "lo" accept
		tcp dport 22 accept
	}
	chain output {
		type filter hook output priority -400; policy drop;
		oif "lo" accept
		tcp sport 22 accept
	}
	chain forward {
		type filter hook forward priority -400; policy drop;
	}
}
NFT
	echo cut
	return 0
}

# net_uncut removes the cut and its timer; a guest with none is done.
net_uncut() {
	nft delete table inet "$CUT_TABLE" >/dev/null 2>&1 || true
	systemctl stop "$CUT_UNIT.timer" >/dev/null 2>&1 || true
	systemctl reset-failed "$CUT_UNIT.timer" "$CUT_UNIT.service" >/dev/null 2>&1 || true
	echo uncut
	return 0
}

# net_cut_state prints "cut" while the cut's table stands, "none" otherwise.
net_cut_state() {
	if nft list table inet "$CUT_TABLE" >/dev/null 2>&1; then echo cut; else echo none; fi
	return 0
}

# net_cut_peer drops every packet between this machine and one peer, the
# control-plane guest, but ssh's, and keeps every other: the node's kubelet
# and k3s agent lose the API server there, so Kubernetes loses the node, and
# the node loses the cdc there too, while it keeps its paths to the
# controller nodes and the suite its ssh. It is the detach case's cut, in
# the machine case's table and under its timer, so net_uncut and
# net_cut_state serve both, and it refuses where net_cut does.
net_cut_peer() { # <secs> <peer ip>
	if ! systemctl is-active --quiet k3s-agent || systemctl is-active --quiet k3s; then
		echo "net_cut_peer: refused: this guest is not a k3s agent" >&2
		return 1
	fi
	case "$2" in
	'' | *[!0-9.]*)
		echo "net_cut_peer: refused: '$2' is no IPv4 address" >&2
		return 1
		;;
	esac
	net_uncut >/dev/null
	systemd-run --quiet --unit="$CUT_UNIT" --on-active="$1" \
		/usr/sbin/nft delete table inet "$CUT_TABLE" || return 1
	nft -f - <<NFT || return 1
table inet $CUT_TABLE {
	chain input {
		type filter hook input priority -400; policy accept;
		tcp dport 22 accept
		ip saddr $2 drop
	}
	chain output {
		type filter hook output priority -400; policy accept;
		tcp sport 22 accept
		ip daddr $2 drop
	}
}
NFT
	echo cut
	return 0
}

# container_cat prints a file of a running container of one pod on this
# node, through the container runtime, which answers on the node however its
# link to the API server stands: the detach case reads its writer this way
# while the node is cut off from the control plane, and after Kubernetes
# deleted the pod, which goes on running on the node.
container_cat() { # <namespace> <pod> <container> <path>
	local cid
	cid=$(k3s crictl ps -q --state running --label "io.kubernetes.pod.namespace=$1" \
		--label "io.kubernetes.pod.name=$2" \
		--label "io.kubernetes.container.name=$3" 2>/dev/null | head -n 1) || {
		echo "container_cat: crictl ps did not answer" >&2
		return 1
	}
	if [ -z "$cid" ]; then
		echo "container_cat: no running container $3 of pod $1/$2" >&2
		return 1
	fi
	k3s crictl exec "$cid" cat "$4"
}

# pod_crash_lines reads the log of every container of one namespace's pods
# that has run, and that of its previous run as well when it restarted, and
# prints each line that matches a pattern as "crash <pod>/<container>:<line>:
# <text>", the container's name followed by " (previous)" for the previous
# run's. It also prints "restarts <pod>/<container> <count>" for each
# container that restarted, "skipped <pod>/<container>" for a log of a pod
# being deleted that could not be read, as its containers may be gone
# already, "gone <pod>" for a pod gone between the listing and its read,
# which is skipped, as its logs went with it, "nolog <pod>" for any other pod
# not being deleted of which no log was read, and last "read <logs> logs of
# <containers> containers in <pods> pods". Any other read that fails fails
# the verb.
pod_crash_lines() { # <namespace> <pattern>
	local ns=$1 pat=$2 list pod del rest c name n id prev f label out rc
	local logs=0 ctrs=0 pods=0 read_here gone
	list=$(k3s kubectl -n "$ns" get pods -o 'jsonpath={range .items[*]}{.metadata.name}{" del="}{.metadata.deletionTimestamp}{range .status.containerStatuses[*]}{" "}{.name}{"="}{.restartCount}{"="}{.containerID}{end}{"\n"}{end}') || {
		echo "pod_crash_lines: listing the pods of $ns did not answer" >&2
		return 1
	}
	f=$(mktemp) || return 1
	while read -r pod del rest; do
		[ -n "$pod" ] || continue
		del=${del#del=}
		pods=$((pods + 1))
		read_here=0 gone=0
		for c in $rest; do
			name=${c%%=*}
			c=${c#*=}
			n=${c%%=*}
			id=${c#*=}
			# A container that never ran has no log.
			[ -n "$id" ] || continue
			ctrs=$((ctrs + 1))
			[ "$n" = 0 ] || echo "restarts $pod/$name $n"
			for prev in "" --previous; do
				[ -z "$prev" ] || [ "$n" != 0 ] || continue
				label="$pod/$name${prev:+ (previous)}"
				if ! k3s kubectl -n "$ns" logs "$pod" -c "$name" $prev >"$f" 2>"$f.err"; then
					if [ -n "$del" ]; then
						echo "skipped $label"
						continue
					fi
					if out=$(k3s kubectl -n "$ns" get pod "$pod" --ignore-not-found -o name) &&
						[ -z "$out" ]; then
						gone=1
						continue
					fi
					echo "pod_crash_lines: reading the log of $label failed: $(head -c 300 "$f.err")" >&2
					rm -f "$f" "$f.err"
					return 1
				fi
				logs=$((logs + 1))
				read_here=1
				out=$(grep -naE "$pat" "$f")
				rc=$?
				if [ "$rc" -gt 1 ]; then
					echo "pod_crash_lines: grep could not read the log of $label" >&2
					rm -f "$f" "$f.err"
					return 1
				fi
				[ -z "$out" ] || printf '%s\n' "$out" | sed "s|^|crash $label:|"
			done
		done
		[ "$gone" = 0 ] || echo "gone $pod"
		[ -n "$del" ] || [ "$read_here" = 1 ] || [ "$gone" = 1 ] || echo "nolog $pod"
	done <<<"$list"
	rm -f "$f" "$f.err"
	echo "read $logs logs of $ctrs containers in $pods pods"
	return 0
}

# now_ns prints the guest's clock in nanoseconds, the clock its pods read.
now_ns() {
	date +%s%N
}

# prepull pulls images into the k3s image store, those it lacks alone, so
# that a controller copy that moves to this node starts without a pull and a
# registry that does not answer stops no run whose images are there.
prepull() { # <ref…>
	local r
	for r in "$@"; do
		k3s crictl inspecti "$r" >/dev/null 2>&1 && continue
		k3s crictl pull "$r" >/dev/null 2>&1 || {
			echo "prepull: pulling $r failed" >&2
			return 1
		}
	done
	echo pulled
	return 0
}

# csi_signal sends a signal, as root, to the dnv-csi container's first
# process of one pod on this node: crictl names the container by its pod's
# and its own names and reports its pid, whose command line must be the
# plugin's. A kubectl exec cannot send it: the container runtime's AppArmor
# profile refuses a signal from inside a controller copy's container to its
# first process (csi.md, Integration test plan). Prints the pid signalled.
csi_signal() { # <pod name> <signal>
	local cid n pid cmd
	cid=$(k3s crictl ps -q --label "io.kubernetes.pod.name=$1" \
		--label io.kubernetes.container.name=dnv-csi 2>/dev/null) || {
		echo "csi_signal: crictl ps did not answer" >&2
		return 1
	}
	n=$(printf '%s\n' "$cid" | grep -c .)
	[ "$n" = 1 ] || {
		echo "csi_signal: $n dnv-csi containers of pod $1 on this node" >&2
		return 1
	}
	pid=$(k3s crictl inspect --output go-template \
		--template '{{.info.pid}}' "$cid" 2>/dev/null) || pid=""
	case "$pid" in
	'' | *[!0-9]* | 0)
		echo "csi_signal: no pid for container $cid: '$pid'" >&2
		return 1
		;;
	esac
	cmd=$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null) || cmd=""
	case "$cmd" in
	*/dnv-csi\ *) ;;
	*)
		echo "csi_signal: pid $pid runs '$cmd', not the plugin" >&2
		return 1
		;;
	esac
	kill -"$2" "$pid" || return 1
	echo "$pid"
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
	pgrep -af "$WORK/bin/[d]nv-agent $1" || true
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

# loss_tmos <nqn prefix> prints the distinct controller-loss timeouts of the
# controllers to the subsystems of one NQN prefix, "off" for a reconnect
# without end, or nothing when there is none.
loss_tmos() {
	local ctrl nqn
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		case "$nqn" in
		"$1"*) cat "$ctrl/ctrl_loss_tmo" 2>/dev/null ;;
		esac
	done | sort -u
	return 0
}

# ctrl_addrs <nqn prefix> prints the traddr of every controller to the
# subsystems of one NQN prefix, one per line.
ctrl_addrs() {
	local ctrl nqn addr
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		case "$nqn" in
		"$1"*)
			addr=$(cat "$ctrl/address" 2>/dev/null) || continue
			addr=${addr#*traddr=}
			printf '%s\n' "${addr%%,*}"
			;;
		esac
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

# nvmet_cut_facts prints "ok" when this guest's kernel lets a cn agent end
# the nvmet connection of a host taken off a subsystem's list (cnagent.md
# CN33), else each fact that fails, joined by "; ": debugfs mounted at
# /sys/kernel/debug, nvmet's debug directory there, which the kernel option
# CONFIG_NVME_TARGET_DEBUGFS makes once nvmet is loaded, and no lockdown in
# force, under which the kernel refuses to open a connection's state file.
# A missing lockdown file reads as no lockdown, as the agent's start check
# reads it; the file is a securityfs file, so a lockdown on a node without
# securityfs mounted shows only as a refused cut. These are the facts the cn
# agent's start check reads (cnagent.md CN-CM2) save configfs's mount, which
# no check of this suite reads, read here in shell, as the guests get the
# agent binary only at setup; the cn and e2e suites read them with the same
# lines.
nvmet_cut_facts() {
	local why="" mode
	if [ ! -r /proc/mounts ]; then
		why="$why; /proc/mounts not read"
	elif ! awk '$2 == "/sys/kernel/debug" && $3 == "debugfs" { f = 1 }
		END { exit !f }' /proc/mounts; then
		why="$why; debugfs is not mounted at /sys/kernel/debug"
	fi
	[ -d "$DBG_NVMET" ] ||
		why="$why; no $DBG_NVMET: nvmet is built without CONFIG_NVME_TARGET_DEBUGFS or is not loaded"
	if [ -e /sys/kernel/security/lockdown ]; then
		mode=$(sed -n 's/.*\[\([a-z]*\)\].*/\1/p' /sys/kernel/security/lockdown 2>/dev/null)
		[ "$mode" = none ] || why="$why; lockdown is '${mode:-unread}'"
	fi
	if [ -z "$why" ]; then echo ok; else echo "${why#; }"; fi
	return 0
}

# nvmet_conns prints the nvmet connections of one subsystem, from its
# directory under nvmet's debug directory: one line "ctrl<cntlid> <hostnqn>"
# each, sorted, nothing when it holds none. A connection whose directory goes
# between the listing and the read of its hostnqn is one that is gone; a read
# that fails while the directory is still there is made once more, since the
# kernel answers EIO for a file it removed after its open, as the cn agent's
# listing reads it. It fails, saying so, when the subsystem has no debug
# directory or a read fails twice, so that a read that did not answer never
# passes for no connection.
nvmet_conns() { # <subsystem nqn>
	local d="$DBG_NVMET/$1" c h out=""
	if [ ! -d "$d" ]; then
		echo "nvmet_conns: no debug directory $d" >&2
		return 1
	fi
	for c in "$d"/ctrl*; do
		[ -d "$c" ] || continue
		if ! h=$(cat "$c/hostnqn" 2>/dev/null) && [ -d "$c" ] &&
			! h=$(cat "$c/hostnqn" 2>/dev/null); then
			[ -d "$c" ] || continue
			echo "nvmet_conns: reading $c/hostnqn failed" >&2
			return 1
		fi
		[ -d "$c" ] || [ -n "$h" ] || continue
		out="$out${c##*/} $h"$'\n'
	done
	printf '%s' "$out" | LC_ALL=C sort
	return 0
}

# nvmet_conns_all prints every nvmet connection of this guest's nvmet, one
# line "<subsystem nqn> ctrl<cntlid> <hostnqn>" each, for the diagnostics; a
# guest with no debug directory says so.
nvmet_conns_all() {
	local c s h
	if [ ! -d "$DBG_NVMET" ]; then
		echo "no $DBG_NVMET"
		return 0
	fi
	for c in "$DBG_NVMET"/*/ctrl*; do
		[ -d "$c" ] || continue
		s=${c%/*}
		h=$(cat "$c/hostnqn" 2>/dev/null) || h="(gone or unread)"
		printf '%s %s %s\n' "${s##*/}" "${c##*/}" "$h"
	done
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
		dnv-????????????????-??-?? | *:dnv-????????????????-??-??)
			timeout 15 mdadm --stop "/dev/$d" >/dev/null 2>&1
			;;
		esac
	done
	return 0
}

md_names() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(md_name_of "$d")
		case "$name" in
		dnv-????????????????-??-?? | *:dnv-????????????????-??-??) printf '%s\n' "$name" ;;
		esac
	done
	return 0
}

# --- the udev rules of deploy/udev/ ----------------------------------------------------

# install_udev_rules writes the udev rules every node that runs a dnv agent
# carries, the files of deploy/udev/ whose texts this helper carries
# (UDEV_RULES), into $UDEV_DIR, each only when its text differs and then by
# a rename, and reloads udev (architecture.md, Components: invocation
# reference). It prints "installed" when it wrote a file, "present" when
# every file was already right.
install_udev_rules() {
	local name word=present
	[ "${#UDEV_RULES[@]}" -gt 0 ] || {
		echo "install_udev_rules: the helper carries no udev rule" >&2
		return 1
	}
	for name in "${!UDEV_RULES[@]}"; do
		[ "$(cat "$UDEV_DIR/$name" 2>/dev/null)" = "${UDEV_RULES[$name]}" ] &&
			continue
		if ! printf '%s\n' "${UDEV_RULES[$name]}" >"$UDEV_DIR/.$name.tmp" ||
			! mv -f "$UDEV_DIR/.$name.tmp" "$UDEV_DIR/$name"; then
			rm -f "$UDEV_DIR/.$name.tmp"
			echo "install_udev_rules: writing $UDEV_DIR/$name failed" >&2
			return 1
		fi
		word=installed
	done
	udevadm control --reload >/dev/null 2>&1
	echo "$word"
	return 0
}

remove_udev_rules() {
	local name
	for name in "${!UDEV_RULES[@]}"; do
		rm -f "$UDEV_DIR/$name" "$UDEV_DIR/.$name.tmp"
	done
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
	install_udev_rules >/dev/null || {
		echo "dn_up: installing the udev rules of deploy/udev failed" >&2
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
			--local-store "$store" --disk "$dev" --log-level debug \
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
# nvmet objects, the dm devices top-down, the loop device, the udev rules,
# $WORK, and then a trim of the guest's root file system.
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
	remove_udev_rules >/dev/null
	# The trim: the backing files' blocks, about 2 GiB on a guest after the
	# fill case, stay allocated in the host's image of the guest until the
	# guest discards them, so a guest that held a work directory is synced,
	# given 10 s for its file system to free the blocks, and trimmed.
	if [ -d "$WORK" ]; then
		rm -rf "$WORK"
		sync
		sleep 10
		timeout 120 fstrim / >/dev/null 2>&1 || echo "TRIM_FAILED: fstrim / on $(hostname)"
	fi
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

# cn_up starts the one cn agent of this guest, the udev rules of deploy/udev/
# before it, and is idempotent. <ip> <grpc_port> <trsvcid>
cn_up() {
	local ip=$1 gport=$2 svcid=$3
	local dir="$WORK/cn" store="$WORK/cn/store" log="$WORK/cn/agent.log" pid pat
	mkdir -p "$dir" "$store" || {
		echo "cn_up: mkdir $dir failed" >&2
		return 1
	}
	install_udev_rules >/dev/null || {
		echo "cn_up: installing the udev rules of deploy/udev failed" >&2
		return 1
	}
	pat="[-]-grpc-address $ip:$gport "
	pid=$(pgrep -f -- "$pat" | head -n 1) || pid=""
	if [ -z "$pid" ]; then
		nohup "$WORK/bin/dnv-agent" cn \
			--grpc-network tcp --grpc-address "$ip:$gport" \
			--tr-type tcp --adr-fam ipv4 \
			--tr-addr "$ip" --tr-svc-id "$svcid" \
			--local-store "$store" --capacity 0 --log-level debug \
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
# connections, the port, the udev rules, $WORK.
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
	remove_udev_rules >/dev/null
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
	pkill -f "$WORK/bin/[e]tcd --name $ETCD_NAME" >/dev/null 2>&1
	sleep 0.5
	pkill -9 -f "$WORK/bin/[d]nv-gateway" >/dev/null 2>&1
	pkill -9 -f "$WORK/bin/[d]nv-worker" >/dev/null 2>&1
	pkill -9 -f "$WORK/bin/[d]nv-cdc" >/dev/null 2>&1
	pkill -9 -f "$WORK/bin/[e]tcd --name $ETCD_NAME" >/dev/null 2>&1
	echo stopped
	return 0
}

cp_pids() {
	pgrep -af "$WORK/bin/([d]nv-gateway|[d]nv-worker|[d]nv-cdc|[e]tcd --name $ETCD_NAME)" || true
	return 0
}

cp_cleanup() {
	stop_all >/dev/null
	rm -rf "$WORK" /var/tmp/dnv-panic-plant-csi.*
	echo cleaned
	return 0
}

# --- the hosts ---------------------------------------------------------------------

# mask neutralises nvme-cli's autoconnector for the run, and verifies.
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

# sanity_stop ends csi-sanity, which outlives the suite's ssh to host0 when the
# case's timeout or a signal ends that ssh, and the two plugins the sanity
# case runs as plain processes.
sanity_stop() {
	pkill -f "$WORK/bin/[c]si-sanity" >/dev/null 2>&1
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
		--gateway-address "$1" --cdc-addresses "$2" --log-level debug \
		>>"$dir/ctl.log" 2>&1 </dev/null &
	echo $! >"$dir/ctl.pid"
	nohup "$WORK/bin/dnv-csi" node --csi-address "$dir/node.sock" \
		--cdc-addresses "$2" --log-level debug \
		>>"$dir/node.log" 2>&1 </dev/null &
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
	# Every dnv-csi process, the driver's plugins in their pods and the
	# sanity case's plain ones: a listing signals nothing.
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
	echo "--- $(hostname): nvmet connections"
	nvmet_conns_all
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
	# A helper function named after a command or a builtin would shadow it
	# in every verb that calls it, so no function may take such a name. The
	# child ends with exit 0, since a name that is no command fails its
	# `type -t`, and the last such failure would otherwise be the child's
	# status and end this script under set -e.
	local fns clash
	fns=$(sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)() {.*/\1/p' "$tmp")
	clash=$(env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		bash --norc --noprofile -c 'for f; do t=$(type -t "$f") && printf "%s (%s) " "$f" "$t"; done; exit 0' _ $fns) ||
		die "checking the generated helper's function names failed"
	[ -z "$clash" ] || die "the generated helper names functions after commands or builtins, which it would shadow: $clash"
	for target in "$CP" "${HOST[@]}" "${CN[@]}" "${DN[@]}"; do
		scp -q "${SSH_OPTS[@]}" "$tmp" "$target:$HELPER" ||
			die "scp of the helper to ${target##*@} failed"
	done
	rm -f "$tmp"
	log "  helper shipped to $HELPER on all $((3 + ${#CN[@]} + ${#DN[@]})) guests"
}

# ---------------------------------------------------------------------------
# Preflight (csi.md, Preflight)
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
	for tool in go ssh scp docker mktemp timeout awk sed grep curl tar sha256sum flock; do
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
# guests the tools and modules of the agents and this suite's ports free, and
# on the controller-node guests the kernel option the cn agent's cut needs.
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
	busy=$(helper_cp ports_busy "$ETCD_CLIENT_PORT" "$ETCD_PEER_PORT" "$GW_PORT" "$CDC_PORT" \
		"$DEAD_CDC_PORT") ||
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
		missing=$(helper_host "$h" tools nvme uuidgen systemctl udevadm nft systemd-run ss fio) ||
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
		# The kernel option the cn agent's cut of a removed host's nvmet
		# connection needs (cnagent.md CN33), read once nvmet is loaded:
		# without it the agent refuses to start (cnagent.md CN-CM2), as
		# cn_up's start line takes no --ignore-host-disconnection, and the
		# detach case needs the cut (csi.md, Preflight).
		out=$(helper_cn "$v" nvmet_cut_facts) ||
			die "cn$v: reading the facts of the cut did not answer"
		[ "$out" = ok ] ||
			die "cn$v: the cn agent cannot cut a removed host's nvmet" \
				"connection here: $out. The lab tool" \
				"integtest/nvmet_debugfs.sh builds and installs nvmet" \
				"modules with CONFIG_NVME_TARGET_DEBUGFS, and" \
				"'bash integtest/nvmet_debugfs.sh check ${CN[$v]}' reads" \
				"these facts again"
		busy=$(helper_cn "$v" ports_busy "$CN_GRPC_PORT" "$CN_TRSVCID") || die "cn$v: ports"
		[ -z "$busy" ] || die "cn$v: port(s) already listening: $(printf '%s' "$busy" | tr '\n' ' ')"
	done
	panic_plant
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
		# --foreground keeps timeout in the suite's process group, as in
		# case_sanity, so a signal to the runner's group reaches the ssh.
		out=$(timeout --foreground "$WAIT_K3S_INSTALL" ssh "${SSH_OPTS[@]}" "$CP" \
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
			out=$(printf '%s\n' "$token" | timeout --foreground "$WAIT_K3S_INSTALL" ssh "${SSH_OPTS[@]}" "${HOST[$h]}" \
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
# The dnv stack (csi.md, The suite's dnv stack)
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
		"--etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--log-level debug"
	wait_until "$WAIT_SHORT" "the gateway to listen on $GW_PORT" cp_port_up "$GW_PORT"
	remote_start "$CP" worker worker.log \
		"$WORK/bin/dnv-worker --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--roles ${WORKER_ROLES// /,}" \
		"--vote-interval $VOTE_INTERVAL --vote-grace-time $VOTE_GRACE" \
		"--log-level debug"
	remote_start "$CP" cdc cdc.log \
		"$WORK/bin/dnv-cdc --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--tr-type tcp --adr-fam ipv4 --tr-addr $CP_IP --tr-svc-id $CDC_PORT" \
		"--log-level debug"
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
		[ "$(jq_of "$CTL_OUT" '.dn_info.port_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.dn_info.store_info.status')" = RES_STATUS_OK ]
}

cn_node_ready() { # <v>
	ctl_try cn inspect --addr "$(cn_addr "$1")" || return 1
	[ "$(jq_of "$CTL_OUT" '.cn_info.port_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.cn_info.store_info.status')" = RES_STATUS_OK ]
}

ship_one() { # <target> <label> <file…>
	local target=$1 label=$2
	shift 2
	scp -q "${SSH_OPTS[@]}" "$@" "$target:$WORK/bin/" ||
		die "scp of the binaries to $label failed"
	ssh_to "$target" "chmod 0755 $WORK/bin/*" >/dev/null ||
		die "chmod of $WORK/bin on $label failed"
}

# dnv_up deploys the dnv stack and registers every node (csi.md, The
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
	# Every node passes its guest's role as its location: the lab's
	# disk-node and controller-node guests share one host name, the default
	# location (architecture.md, Disk nodes), so the nodes created with
	# none would all be one location and no RAID1 pool could be placed.
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_AGENT" "dn create $(dn_addr "$v") to be accepted" \
			ctl_create_try dn create --addr "$(dn_addr "$v")" \
			--tr-type tcp --adr-fam ipv4 --tr-addr "${DN_IP[$v]}" --tr-svc-id "$DN_TRSVCID" \
			--location "dn$v"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_AGENT" "cn create $(cn_addr "$v") to be accepted" \
			ctl_create_try cn create --addr "$(cn_addr "$v")" \
			--tr-type tcp --adr-fam ipv4 --tr-addr "${CN_IP[$v]}" --tr-svc-id "$CN_TRSVCID" \
			--location "cn$v"
	done
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_PROVISION" "dn$v to report disk, header, port and store OK" dn_node_ready "$v"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_PROVISION" "cn$v to report its nvmet port and store OK" cn_node_ready "$v"
	done
	log "  the dnv stack serves cluster $CLUSTER ($CLUSTER_ID)"
}

# ---------------------------------------------------------------------------
# The image and the driver's deployment (csi.md, Deployment)
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

# with_debug_log prints the plugin manifest on stdin with --log-level=debug
# added to the dnv-csi container's arguments, the line after its subcommand,
# at that line's indent, so the plugins run at debug as the suites run their
# dnv daemons, save one dn agent in one case of the dn suite, the tutorial
# suite's daemons and the e2e upgrade case's v0.1.0 programs; it fails when
# the manifest has no such line.
with_debug_log() {
	awk '
	    { print }
	    !done && /^[[:space:]]*- (controller|node)$/ {
	        sub(/- .*/, "- --log-level=debug"); print; done = 1
	    }
	    END { exit done ? 0 : 1 }'
}

# driver_up applies the manifests, with a config map and a storage class of
# the suite's own in place of the examples, and the plugins at debug, and
# waits for both plugins.
driver_up() {
	stage 06 "build the image and import it into every node, and pull the sidecars on every node"
	image_build
	image_import "$IMAGE"
	local out h
	out=$(helper_cp prepull $SIDECARS) || die "cp: pulling the sidecar images failed: $out"
	for h in "${!HOST[@]}"; do
		out=$(helper_host "$h" prepull $SIDECARS) || die "host$h: pulling the sidecar images failed: $out"
	done

	stage 07 "deploy the driver: $DRIVER_NAME in $DRIVER_NS"
	local f
	for f in namespace csidriver controller-serviceaccount node-serviceaccount \
		controller-clusterrole controller-clusterrolebinding controller-role \
		controller-rolebinding; do
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
	local manifest
	for f in controller node; do
		manifest=$(with_debug_log <"$DEPLOY_DIR/$f.yaml") ||
			die "$f.yaml has no dnv-csi subcommand line to add --log-level to"
		kc_apply <<<"$manifest" || die "applying $f.yaml failed"
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
	# The same class with xfs, through the key Kubernetes reserves for the
	# filesystem of a class's volumes (csi.md CSI12).
	kc_apply <<EOF || die "applying the xfs storage class failed"
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SC_XFS
provisioner: $DRIVER_NAME
parameters:
  cluster: $CLUSTER
  redundancy: raid1
  cntlrs: "2"
  csi.storage.k8s.io/fstype: xfs
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

	stage 08 "the driver's shape: two controller copies leading through two leases, critical priority, the node probe on the loopback"
	wait_until "$WAIT_LEASE" "both leases held by controller pods" leases_held
	local nodes
	nodes=$(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		-o 'jsonpath={range .items[*]}{.spec.nodeName}{"\n"}{end}') ||
		die "reading the controller copies' nodes failed"
	assert_eq "$(printf '%s\n' "$nodes" | grep -c .)" 2 "controller copies"
	assert_eq "$(printf '%s\n' "$nodes" | sort -u | grep -c .)" 2 \
		"distinct nodes of the controller copies ($(printf '%s' "$nodes" | tr '\n' ' '))"
	assert_eq "$(pods_priority app=dnv-csi-controller)" \
		"$(kc get priorityclass system-cluster-critical -o 'jsonpath={.value}')" \
		"the controller copies' priority"
	assert_eq "$(pods_priority app=dnv-csi-node)" \
		"$(kc get priorityclass system-node-critical -o 'jsonpath={.value}')" \
		"the node pods' priority"
	for h in "${!HOST[@]}"; do
		assert_eq "$(helper_host "$h" listen_addrs 9809 | tr '\n' ' ')" "127.0.0.1 " \
			"host$h: the addresses the node plugin's probe listens on"
	done
	# The test namespace's pods are kept off every dnv machine, which the
	# policy case proves and every later case runs under (csi.md CSI20).
	policy_apply "$TEST_NS"

	stage 09 "a controller plugin copy, its container's first process, takes SIGUSR1 and SIGUSR2"
	plugin_signals
	DRIVER_UP=1
}

# leases_held answers whether both sidecar leases name a controller copy
# that runs.
leases_held() {
	local holders pods l
	holders=$(lease_holders) || return 1
	pods=$(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		--field-selector status.phase=Running -o 'jsonpath={.items[*].metadata.name}') || return 1
	[ "$(printf '%s\n' "$holders" | grep -c .)" = 2 ] || return 1
	for l in $holders; do
		case " $pods " in *" $l "*) ;; *) return 1 ;; esac
	done
}

# lease_holders prints the holder of each of the two sidecar leases, one per
# line: the provisioner's and the attacher's.
lease_holders() {
	local l
	for l in csi-dnv-io external-attacher-leader-csi-dnv-io; do
		kc -n "$DRIVER_NS" get lease "$l" -o 'jsonpath={.spec.holderIdentity}{"\n"}' 2>/dev/null || return 1
	done
}

# pods_priority prints the distinct priorities of the driver's pods of one
# label.
pods_priority() { # <label selector>
	kc -n "$DRIVER_NS" get pods -l "$1" -o 'jsonpath={range .items[*]}{.spec.priority}{"\n"}{end}' |
		sort -u | tr -d '\n'
}

# policy_apply applies the example network policy to a namespace, with every
# address of the suite's dnv machines in its except list and the API server's
# address in its second rule. Every dnv program of the suite listens on its
# guest's one address (etcd on the loopback), so those addresses are all the
# policy must list here.
policy_apply() { # <namespace>
	local addrs a
	addrs=$(printf '%s\n' "$CP_IP" "${CN_IP[@]}" "${DN_IP[@]}" | sort -u)
	awk -v addrs="$addrs" -v api="$CP_IP" '
		/192\.0\.2\.10\/32 # every address/ {
			n = split(addrs, a, "\n")
			for (i = 1; i <= n; i++) if (a[i] != "") print "              - " a[i] "/32"
			next
		}
		/192\.0\.2\.10\/32 # the API server/ { sub(/192\.0\.2\.10\/32 #.*/, api "/32") }
		{ print }
	' "$DEPLOY_DIR/networkpolicy.yaml" |
		ssh "${SSH_OPTS[@]}" "$CP" "sudo -n k3s kubectl apply -n $1 -f -" >/dev/null ||
		die "applying the network policy to $1 failed"
	a=$(kc -n "$1" get networkpolicy dnv-keep-off -o 'jsonpath={.spec.egress[0].to[0].ipBlock.except}') ||
		die "reading the network policy of $1 failed"
	assert_eq "$(printf '%s' "$a" | grep -o '/32' | wc -l)" "$(printf '%s\n' "$addrs" | grep -c .)" \
		"the addresses the policy of $1 keeps its pods off"
}

k8s_ns_gone() { ! kc get namespace "$1" >/dev/null 2>&1; }

# level_changes_of <pod> — the "log level changed" records the dnv-csi
# container of one driver pod has logged, one line each with the old level,
# the new one and the signal (log.md R13); none when the log does not read,
# which the caller's wait asks again.
level_changes_of() {
	{ kc -n "$DRIVER_NS" logs "$1" -c dnv-csi --tail=-1 2>/dev/null || true; } |
		"$JQ" -Rr 'fromjson? | objects | select(.msg == "log level changed")
		    | "\(.from) \(.log_level) \(.signal)"'
}

# plugin_signals sends SIGUSR1, then SIGUSR2, to one controller plugin copy,
# the first process of its container, as the operations doc's Kubernetes step
# does: from the copy's node, as root (csi_signal), since the container
# runtime's AppArmor profile refuses the signal from a kubectl exec in the
# copy's container. It waits for the "log level changed" record of each in
# that copy's log: the plugin runs at debug, so both name debug, SIGUSR2 its
# start log level (log.md R13). The copy is picked by label among the running
# ones; each copy's plugin takes the signals alike.
plugin_signals() {
	local pod node sig before n i want out g
	out=$(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		--field-selector status.phase=Running \
		-o 'jsonpath={.items[0].metadata.name} {.items[0].spec.nodeName}') ||
		die "reading the controller plugin pods failed"
	read -r pod node <<<"$out"
	[ -n "$pod" ] && [ -n "$node" ] || die "no running controller plugin pod"
	g=""
	if [ "$node" = "$CP_NAME" ]; then
		g=cp
	else
		for i in "${!HOST[@]}"; do
			[ "${HOST_NAME[$i]}" = "$node" ] && g=$i
		done
	fi
	[ -n "$g" ] || die "the controller copy $pod runs on $node, no guest of this run"
	log "  signalling $pod on $node"
	for sig in USR1 USR2; do
		before=$(level_changes_of "$pod" | wc -l)
		if [ "$g" = cp ]; then
			out=$(helper_cp csi_signal "$pod" "$sig")
		else
			out=$(helper_host "$g" csi_signal "$pod" "$sig")
		fi || die "SIG$sig to the controller copy $pod on $node failed: $out"
		n=$before
		for ((i = 0; i < 20; i++)); do
			n=$(level_changes_of "$pod" | wc -l)
			[ "$n" -gt "$before" ] && break
			sleep 1
		done
		[ "$n" -gt "$before" ] ||
			die "the controller plugin logged no level change after SIG$sig"
		case $sig in
		USR1) want="debug debug user defined signal 1" ;;
		USR2) want="debug debug user defined signal 2" ;;
		esac
		assert_eq "$(level_changes_of "$pod" | tail -n 1)" "$want" \
			"the controller plugin's level change on SIG$sig"
	done
}

# driver_down removes the driver and the suite's Kubernetes objects; a
# cluster without k3s has nothing to remove. It reports, never dies.
driver_down() {
	local out ver
	DRIVER_UP=0
	out=$(QUIET=1 helper_cp_ok k3s_state 2>/dev/null) || true
	ver=$(k3s_version_of "$out")
	if [ -z "$ver" ] || [ "$ver" = none ] || [ "$(k3s_unit_of "$out" k3s)" != active ]; then
		log "  cp: no running k3s server, nothing to delete in Kubernetes"
		return 0
	fi
	local ns
	for ns in "$TEST_NS" "$OPEN_NS"; do
		kc_ok delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1
		if ! wait_cleanup "$WAIT_NS_GONE" k8s_ns_gone "$ns"; then
			log "WARNING: the namespace $ns is still there after ${WAIT_NS_GONE}s"
			CLEANUP_DIRTY=1
		fi
	done
	kc_ok delete storageclass "$SC_NAME" "$SC_XFS" --ignore-not-found >/dev/null 2>&1
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
		printf '%s\n' "$out" | grep -v -x "$want" | grep -E 'wipe_left=|STUCK|REFUSED|STILL_RUNNING|TRIM_FAILED' |
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
	# The detach case's out-of-service taint first, so that no host comes
	# back under it, which would evict its pods; then the cut of either
	# case: with a host cut off, Kubernetes cannot confirm the deletes of its
	# pods, and the namespaces would not go. Then a worker the detach case
	# left stopped goes on: no cn agent converges without it, so every
	# detach of the deletes below would wait for a cut that never comes.
	untaint_hosts
	uncut_hosts
	QUIET=1 helper_cp_ok sig_pidfile "$WORK/worker/pid" CONT >/dev/null 2>&1
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

# uncut_hosts removes the cut of either case from both hosts, whatever
# state the run left them in.
uncut_hosts() {
	local h
	for h in "${!HOST[@]}"; do
		cleanup_verb "host$h" uncut ssh_host "$h" net_uncut || true
	done
}

# untaint_hosts removes the detach case's out-of-service taint from both
# hosts' nodes. A guest whose name the run has not read yet is asked, and a
# cluster that does not answer, as before k3s is installed, holds no taint
# of the suite's; a taint that stays makes the cleanup dirty.
untaint_hosts() {
	local h name out
	for h in "${!HOST[@]}"; do
		name=${HOST_NAME[$h]:-}
		if [ -z "$name" ]; then
			out=$(QUIET=1 helper_host_ok "$h" facts 2>/dev/null)
			name=$(fact_of "$out" hostname)
		fi
		[ -n "$name" ] || continue
		out=$(QUIET=1 kc get node "$name" \
			-o "jsonpath={.spec.taints[?(@.key==\"$OOS_TAINT_KEY\")].key}" 2>/dev/null) || continue
		[ -n "$out" ] || continue
		if ! QUIET=1 kc taint node "$name" "$OOS_TAINT_KEY-" >/dev/null 2>&1; then
			log "WARNING: host$h: removing the out-of-service taint of $name failed"
			CLEANUP_DIRTY=1
		fi
	done
}

diagnostics() {
	local h v
	QUIET=1
	panic_dump
	log "--- the cut on the hosts, and the nodes' taints"
	for h in "${!HOST[@]}"; do log "  host$h: $(helper_host_ok "$h" net_cut_state 2>/dev/null)"; done
	kc_ok get nodes -o 'jsonpath={range .items[*]}{.metadata.name}: {.spec.taints[*].key}{"\n"}{end}' >&2
	log "--- the sidecars' leases"
	kc_ok -n "$DRIVER_NS" get lease -o wide >&2
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
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-controller -c dnv-csi --prefix --tail=60 >&2
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-controller -c csi-provisioner --prefix --tail=30 >&2
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-controller -c csi-attacher --prefix --tail=30 >&2
	# A sidecar that lost its lease exits, and its last words are in the
	# container's previous run.
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-controller -c csi-provisioner --prefix --previous --tail=10 >&2
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-controller -c csi-attacher --prefix --previous --tail=10 >&2
	kc_ok -n "$DRIVER_NS" logs -l app=dnv-csi-node -c dnv-csi --prefix --tail=60 >&2
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
# The crash scan (CSI21)
# ---------------------------------------------------------------------------

# PANIC_PAT matches the first line of the report Go's runtime writes to a
# program's stderr, which is its log here, when the program dies of a panic or
# a fatal error. A JSON record starts with a brace, so a record that holds
# either word inside a field never matches.
PANIC_PAT='^(panic: |fatal error: )'

# DRIVER_UP is 1 from the end of driver_up to the start of driver_down: while
# it is, panic_pods wants a log of every driver pod. PANIC_HITS and
# PANIC_UNREAD are panic_guests' answers.
DRIVER_UP=0
PANIC_HITS=""
PANIC_UNREAD=""

# panic_lines prints, as <log>:<line>:<text>, every line of each *.log under
# <dir> on one guest that PANIC_PAT matches, and fails when grep could not
# read them all: a log that was not read is not a log without a crash. The
# lines it found are printed either way.
panic_lines() { # <dir> <ssh function> [<guest index>]
	local dir=$1
	shift
	"$@" "grep -rnHaE --include='*.log' '$PANIC_PAT' $dir || [ \$? -eq 1 ]"
}

# panic_label prints one guest's hits, each on its own line behind the
# guest's name, or nothing for none.
panic_label() { # <guest name> <hits>
	[ -z "$2" ] || printf '\n    %s: %s' "$1" "${2//$'\n'/$'\n'    $1: }"
}

# panic_guests reads the logs under the work directory of every guest
# (panic_lines) and leaves their hits, each behind its guest's name, in
# PANIC_HITS; it fails on the first guest whose logs it could not read all
# of, which it names in PANIC_UNREAD, with what it read kept in PANIC_HITS.
# It runs in the caller's shell, for the two variables.
panic_guests() {
	local h v hits
	PANIC_HITS="" PANIC_UNREAD=""
	hits=$(panic_lines "$WORK" ssh_cp_sudo) || PANIC_UNREAD="cp ($CP_IP)"
	PANIC_HITS+=$(panic_label "cp" "$hits")
	[ -z "$PANIC_UNREAD" ] || return 1
	for h in "${!HOST[@]}"; do
		hits=$(panic_lines "$WORK" ssh_host "$h") || PANIC_UNREAD="host$h (${HOST_IP[$h]})"
		PANIC_HITS+=$(panic_label "host$h" "$hits")
		[ -z "$PANIC_UNREAD" ] || return 1
	done
	for v in "${!CN[@]}"; do
		hits=$(panic_lines "$WORK" ssh_cn "$v") || PANIC_UNREAD="cn$v (${CN_IP[$v]})"
		PANIC_HITS+=$(panic_label "cn$v" "$hits")
		[ -z "$PANIC_UNREAD" ] || return 1
	done
	for v in "${!DN[@]}"; do
		hits=$(panic_lines "$WORK" ssh_dn "$v") || PANIC_UNREAD="dn$v (${DN_IP[$v]})"
		PANIC_HITS+=$(panic_label "dn$v" "$hits")
		[ -z "$PANIC_UNREAD" ] || return 1
	done
	return 0
}

# panic_pods reads the logs of every container of the driver's pods, its
# previous run's included (pod_crash_lines), under the caller's stage, and
# fails on a crash line or on a read that did not answer, and, while the
# driver is up (DRIVER_UP), on no pod listed or on a pod not being deleted of
# which no log was read. It logs how many logs it read and the restart count
# of each container that restarted. The suite calls it as each case ends
# (panic_scan) and right before each step of its own that deletes or replaces
# a driver pod: a rollout, a patch of the node plugin's pod template, a
# scale-down, a pod's delete, the cut of a host and driver_down, since a
# deleted pod's logs go with it.
panic_pods() {
	local out line who count hits="" nolog="" summary="" pods=-1 logs=-1
	out=$(helper_cp pod_crash_lines "$DRIVER_NS" "'$PANIC_PAT'") ||
		die "the crash scan could not read the logs of the driver's containers"
	while IFS= read -r line; do
		case "$line" in
		"restarts "*)
			read -r _ who count <<<"$line"
			log "  container $who has restarted $count time(s) in its pod"
			;;
		"skipped "*) log "  ${line#skipped }: its pod is being deleted and its log is gone" ;;
		"gone "*) log "  ${line#gone }: the pod went between the listing and its read" ;;
		"nolog "*) nolog+=" ${line#nolog }" ;;
		"crash "*) hits+=$'\n'"    ${line#crash }" ;;
		"read "*)
			summary=${line#read }
			read -r logs _ _ _ _ _ pods _ <<<"$summary"
			;;
		esac
	done <<<"$out"
	[ -n "$summary" ] || die "the read of the driver's containers printed no count: $out"
	log "  the crash scan read $summary"
	[ -z "$hits" ] || die "a program crashed; its container's log holds:$hits"
	if [ "$DRIVER_UP" = 1 ]; then
		[ "$pods" -ge 1 ] && [ "$logs" -ge 1 ] ||
			die "the crash scan read no log of the driver's pods while the driver is up ($summary)"
		[ -z "$nolog" ] ||
			die "the crash scan read no log of the driver's pod(s)$nolog while the driver is up"
	fi
}

# panic_scan fails the case when a log under the work directory of any guest
# (panic_guests) or a log of a container of the driver's pods (panic_pods)
# holds a crash line, naming the guest or the container, the log and the
# line. run_case calls it as each case ends.
panic_scan() {
	stage crash "the crash scan of every program log and container log"
	panic_guests ||
		die "the crash scan could not read the logs under $WORK on $PANIC_UNREAD${PANIC_HITS:+; the lines it read:$PANIC_HITS}"
	[ -z "$PANIC_HITS" ] || die "a program crashed; its log holds:$PANIC_HITS"
	panic_pods
}

# panic_dump prints, on the failure path, the crash lines panic_guests reads,
# up to the first guest whose logs it cannot read, which it names, and every
# crash line the driver's containers hold, so that a crash that failed a case
# through another check is named. It never fails.
panic_dump() {
	local out
	panic_guests || PANIC_HITS+=$'\n'"    (the logs on $PANIC_UNREAD could not all be read)"
	out=$(QUIET=1 helper_cp pod_crash_lines "$DRIVER_NS" "'$PANIC_PAT'" 2>&1) ||
		out+=$'\n'"(the read of the driver's containers did not answer)"
	log "--- crash lines in the program logs and the driver's container logs"
	log "${PANIC_HITS:-    (none in the logs of the guests)}"
	printf '%s\n' "$out" | grep -v '^restarts \|^read \|^skipped \|^gone \|^nolog ' >&2 || true
	return 0
}

# panic_plant proves panic_lines at preflight, on a sample log in a scratch
# directory of the control-plane guest, read through sudo as every guest's
# logs are: a record that holds both prefixes inside a field must not match,
# the two crash lines planted after it must, and nothing else may.
panic_plant() {
	local d hits want
	d=$(ssh_cp_sudo "mktemp -d /var/tmp/dnv-panic-plant-csi.XXXXXX") ||
		die "making the crash scan's scratch directory on cp failed"
	case "$d" in
	/var/tmp/dnv-panic-plant-csi.?*) ;;
	*) die "mktemp on cp printed '$d', not a scratch directory" ;;
	esac
	printf '%s\n' \
		'{"level":"ERROR","msg":"os command failed","stderr":"panic: in a field\nfatal error: in a field"}' \
		'panic: planted by the preflight' 'goroutine 1 [running]:' \
		'fatal error: planted by the preflight' |
		ssh_cp_sudo "mkdir $d/sub && cat > $d/sub/plant.log" || {
			ssh_cp_sudo "rm -rf $d" || true
			die "writing the crash scan's sample log on cp failed"
		}
	hits=$(panic_lines "$d" ssh_cp_sudo) || hits="(the read failed)"
	ssh_cp_sudo "rm -rf $d" || true
	want="$d/sub/plant.log:2:panic: planted by the preflight"
	want+=$'\n'"$d/sub/plant.log:4:fatal error: planted by the preflight"
	assert_eq "$hits" "$want" "the crash scan on its planted sample log"
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

# claim applies one claim of the suite's class, or of the class named.
claim() { # <name> <size> <Filesystem|Block> [class]
	kc_apply <<EOF || die "applying the claim $1 failed"
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  namespace: $TEST_NS
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${4:-$SC_NAME}
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

# hold_script holds the claim and writes nothing.
hold_script() {
	cat <<'EOF'
echo done > /data/marker
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
# The cases (csi.md, The cases)
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

# probe_pod applies a pod of the driver's image on host0 in a namespace,
# which sleeps so that the policy case can run its connects in it.
probe_pod() { # <namespace> <pod>
	kc_apply <<EOF || die "applying the probe pod $2 in $1 failed"
apiVersion: v1
kind: Pod
metadata:
  name: $2
  namespace: $1
spec:
  nodeSelector:
    kubernetes.io/hostname: ${HOST_NAME[0]}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: probe
      image: $IMAGE
      imagePullPolicy: IfNotPresent
      command: ["sleep", "1000000"]
EOF
	kc -n "$1" wait --for=condition=Ready "pod/$2" "--timeout=${WAIT_POD}s" >/dev/null ||
		die "the probe pod $2 in $1 did not run"
}

# port_probe prints "open" when a pod reaches an address and port within
# three seconds, "closed" when it does not, and nothing when the try itself
# failed, as when the exec into the pod did, so that a failed try is never
# read as either answer.
port_probe() { # <namespace> <pod> <ip> <port>
	local out
	out=$(QUIET=1 kc -n "$1" exec "$2" -- bash -c \
		"timeout 3 bash -c '</dev/tcp/$3/$4' 2>/dev/null; echo rc=\$?" 2>/dev/null) || true
	case "$out" in
	*rc=0*) echo open ;;
	*rc=[1-9]*) echo closed ;;
	esac
}

port_open() { [ "$(port_probe "$@")" = open ]; }
port_closed() { [ "$(port_probe "$@")" = closed ]; }

# case_policy: the example network policy, applied to the test namespace,
# keeps its pods off the gateway's, the cdc's and the agents' ports, which a
# pod of a namespace without the policy reaches, and leaves them the API
# server and the cluster's name service (csi.md, The cases, policy). etcd
# listens on the loopback alone in the suite, where no pod reaches it with or
# without the policy, so it is not probed.
case_policy() {
	CASE=policy
	local t ip port api
	stage 01 "a probe pod on host0 in the test namespace, under the policy, and one in a namespace without it"
	kc_apply <<EOF || die "creating the namespace $OPEN_NS failed"
apiVersion: v1
kind: Namespace
metadata:
  name: $OPEN_NS
EOF
	probe_pod "$OPEN_NS" open-probe
	probe_pod "$TEST_NS" test-probe
	# The agents' ports stand for their machines' addresses, which the policy
	# closes whole. The NVMe/TCP ports are not probed: an nvmet port listens
	# only once a subsystem is linked to it, and no pool exists yet.
	local targets=("$CP_IP:$GW_PORT" "$CP_IP:$CDC_PORT"
		"${CN_IP[0]}:$CN_GRPC_PORT" "${DN_IP[0]}:$DN_GRPC_PORT")

	stage 02 "the gateway's, the cdc's and the agents' ports open from the open namespace and closed from the test one"
	for t in "${targets[@]}"; do
		ip=${t%:*}
		port=${t#*:}
		wait_until "$WAIT_SHORT" "$t open from $OPEN_NS, without which its closing proves nothing" \
			port_open "$OPEN_NS" open-probe "$ip" "$port"
		# A new pod's first connects can pass before the policy holds for
		# it, about a second on the lab's k3s, so the closed answer is
		# waited for.
		wait_until "$WAIT_POLICY" "$t closed from $TEST_NS" port_closed "$TEST_NS" test-probe "$ip" "$port"
		log "  $t: open from $OPEN_NS, closed from $TEST_NS"
	done

	stage 03 "the API server and the name service stay open from the test namespace"
	api=$(kc -n default get service kubernetes -o 'jsonpath={.spec.clusterIP}') ||
		die "reading the API server's service address failed"
	wait_until "$WAIT_SHORT" "the API server's service $api:443 open from $TEST_NS" \
		port_open "$TEST_NS" test-probe "$api" 443
	kc -n "$TEST_NS" exec test-probe -- getent hosts kubernetes.default.svc.cluster.local >/dev/null ||
		die "the cluster's name service does not answer from $TEST_NS"

	stage 04 "the probe pods go"
	kc delete pod -n "$OPEN_NS" open-probe --ignore-not-found --wait=false >/dev/null || true
	kc delete pod -n "$TEST_NS" test-probe --ignore-not-found --wait=false >/dev/null || true
	wait_until "$WAIT_POD" "pod test-probe gone" pod_gone test-probe
	kc delete namespace "$OPEN_NS" --ignore-not-found --wait=false >/dev/null || true
	wait_until "$WAIT_NS_GONE" "the namespace $OPEN_NS gone" k8s_ns_gone "$OPEN_NS"
	log "=== $CASE: PASS"
}

# case_volume: a claim, a pod on host0 that writes and reads, the pod on
# host1 reading the data back, a raw block claim, then every delete and the
# check that nothing remains (csi.md, The cases).
# capacity_published answers whether the provisioner holds a capacity object
# of the suite's class with a capacity above zero (csi.md CSI19): the one
# place Kubernetes shows dnv's free space, and the one the provisioner
# deletes at once for a driver without a topology segment (CSI13).
capacity_published() {
	local out
	out=$(kc get csistoragecapacities -A -o jsonpath="{range .items[?(@.storageClassName==\"$SC_NAME\")]}{.capacity}{\"\n\"}{end}" 2>/dev/null) || return 1
	[ -n "$out" ] && ! printf '%s\n' "$out" | grep -qxE '0|'
}

# quantity_bytes prints the bytes of a Kubernetes quantity as the
# provisioner writes a capacity, a whole number of bytes with a binary suffix
# where the number divides by it ("18Gi"), and nothing for any other form.
quantity_bytes() { # <quantity>
	local q=$1 n mul=1
	case "$q" in
	*Ki) n=${q%Ki} mul=$((1 << 10)) ;;
	*Mi) n=${q%Mi} mul=$((1 << 20)) ;;
	*Gi) n=${q%Gi} mul=$((1 << 30)) ;;
	*Ti) n=${q%Ti} mul=$((1 << 40)) ;;
	*Pi) n=${q%Pi} mul=$((1 << 50)) ;;
	*) n=$q ;;
	esac
	case "$n" in '' | *[!0-9]*) return 0 ;; esac
	printf '%s\n' "$((n * mul))"
}

# capacity_is_the_gateways answers whether the capacity object of the
# suite's class holds the gateway's answer for the class's pool shape, RAID1,
# two cntlrs and one slice: its placeable bytes (csi.md CSI11). Both are read
# each try, so a provisioner poll that lags a change of the cluster is waited
# out rather than failed.
capacity_is_the_gateways() {
	local cap want
	ctl_exec cluster get-capacity --leg-cnt 2 --cntlr-cnt 2 --slice-cnt 1
	[ "$CTL_RC" = 0 ] || return 1
	want=$(jq_of "$CTL_OUT" '.space.placeable_bytes')
	cap=$(kc get csistoragecapacities -A -o jsonpath="{range .items[?(@.storageClassName==\"$SC_NAME\")]}{.capacity}{end}" 2>/dev/null) ||
		return 1
	CAPACITY_SEEN="$cap, the gateway's $want"
	[ -n "$want" ] && [ "$(quantity_bytes "$cap")" = "$want" ]
}

case_volume() {
	CASE=volume
	stage 01 "a filesystem claim and a pod on host0 that writes a pattern"
	wait_until "$WAIT_CAPACITY" "the capacity object of the class $SC_NAME" capacity_published
	log "  capacity of $SC_NAME: $(kc_ok get csistoragecapacities -A -o jsonpath="{range .items[?(@.storageClassName==\"$SC_NAME\")]}{.capacity} (largest volume {.maximumVolumeSize}){end}" 2>/dev/null)"
	# The capacity is the gateway's answer, not a sum of the driver's own.
	wait_until "$WAIT_CAPACITY" "the capacity object of $SC_NAME to hold the gateway's placeable bytes" \
		capacity_is_the_gateways
	log "  capacity object $CAPACITY_SEEN"
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
	# The node plugin's paths reconnect without end (csi.md CSI14): the
	# kernel shows the controller-loss timeout of each as "off".
	assert_eq "$(helper_host 1 loss_tmos "$NQN_CSI:")" off \
		"host1's paths to the volume reconnect without end"
	delete_pod r1

	stage 03 "a raw block claim, written and read through the device"
	claim b0 512Mi Block
	wait_until "$WAIT_PVC" "claim b0 Bound" pvc_bound b0
	run_pod wb b0 0 Block "$(write_block_script 16)" /tmp/marker
	delete_pod wb

	# The node plugin formats xfs with the maker's profile of kernel 6.1
	# (csi.md CSI17), which turns off two features the image's maker turns
	# on by default: both off shows the profile was used. xfs_info needs the
	# mount's device node, which an ordinary pod's /dev lacks, so it runs in
	# host0's node plugin container: privileged, with the host's /dev and
	# the kubelet's pods directory, in whose mount table the plugin's bind
	# at the pod's target stands, and with the image's xfsprogs.
	stage 04 "an xfs claim, formatted with the profile of kernel 6.1"
	claim x0 1Gi Filesystem "$SC_XFS"
	wait_until "$WAIT_PVC" "claim x0 Bound" pvc_bound x0
	run_pod wx x0 0 Filesystem "$(hold_script)"
	local info xpv uid plugin
	xpv=$(pvc_volume x0)
	uid=$(kc get pod -n "$TEST_NS" wx -o 'jsonpath={.metadata.uid}') || die "reading the uid of pod wx failed"
	plugin=$(node_plugin_pod 0) || die "reading host0's node plugin pod failed"
	[ -n "$uid" ] && [ -n "$plugin" ] || die "pod wx has no uid ($uid) or host0 no node plugin pod ($plugin)"
	info=$(QUIET=1 kc -n "$DRIVER_NS" exec "$plugin" -c dnv-csi -- \
		xfs_info "/var/lib/kubelet/pods/$uid/volumes/kubernetes.io~csi/$xpv/mount" 2>&1) ||
		die "xfs_info of x0 in host0's node plugin failed: $info"
	printf '%s\n' "$info" | sed 's/^/  /' >&2
	printf '%s\n' "$info" | grep -q 'nrext64=' ||
		die "xfs_info of x0 names no nrext64 feature: $info"
	printf '%s\n' "$info" | grep -qw 'nrext64=0' ||
		die "the xfs volume x0 has nrext64 on: it was not formatted with the 6.1 profile"
	printf '%s\n' "$info" | grep -qw 'rmapbt=0' ||
		die "the xfs volume x0 has rmapbt on: it was not formatted with the 6.1 profile"
	delete_pod wx

	stage 05 "every delete, and nothing left"
	delete_claim v0
	delete_claim b0
	delete_claim x0
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

# has_path_to answers whether a host holds a path of the driver's volumes to
# one address; no_path_to whether it holds none.
has_path_to() { # <h> <traddr>
	local out
	out=$(QUIET=1 helper_host "$1" ctrl_addrs "$NQN_CSI:") || return 1
	printf '%s\n' "$out" | grep -qxF "$2"
}

no_path_to() { # <h> <traddr>
	local out
	out=$(QUIET=1 helper_host "$1" ctrl_addrs "$NQN_CSI:") || return 1
	! printf '%s\n' "$out" | grep -qxF "$2"
}

# controller_cdcs sets the cdc list of the controller plugin alone: it
# patches the config map and restarts the controller plugin, which reads the
# map at its start. The node plugins keep the list they started with, since
# the environment of a running pod does not follow its config map.
controller_cdcs() { # <ip:port,…>
	kc -n "$DRIVER_NS" patch configmap dnv-csi-config --type merge \
		-p "{\"data\":{\"cdcAddresses\":\"$1\"}}" >/dev/null ||
		die "patching the cdc list of the config map failed"
	panic_pods
	kc -n "$DRIVER_NS" rollout restart deploy/dnv-csi-controller >/dev/null ||
		die "restarting the controller plugin failed"
	kc -n "$DRIVER_NS" rollout status deploy/dnv-csi-controller "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the controller plugin did not roll out"
}

# case_failover: a pod writes in a loop while the primary's cn agent is cut
# off; after the failover the writer has failed nothing, host0 drops its
# path to the old primary, the stall is reported, and the pattern written
# before checks out (csi.md, The cases). The controller plugin's cdc list
# names a dead cdc for the case, so every attach's publish context does:
# the node plugin follows its own list alone (csi.md CSI15), and a node that
# followed the context's would hold every drop.
case_failover() {
	CASE=failover
	stage 01 "a dead cdc in the controller plugin's list; a claim and a pod on host0 writing a pattern, then a write loop"
	controller_cdcs "$CP_IP:$CDC_PORT,$CP_IP:$DEAD_CDC_PORT"
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
	wait_until "$WAIT_PATH_DROP" "host0 to drop its path to the old primary at ${CN_IP[$v]}" \
		no_path_to 0 "${CN_IP[$v]}"
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

	stage 04 "give the agent back, delete, the controller plugin's list back, and nothing left"
	stat=$(helper_cn "$v" sig_pidfile "$WORK/cn/pid" CONT) || die "cn$v: sig_pidfile failed"
	log "  cn$v agent $stat"
	delete_claim f0
	controller_cdcs "$CP_IP:$CDC_PORT"
	nothing_left
	log "=== $CASE: PASS"
}

# node_plugin_pod prints the name of the node plugin pod on a host, empty
# when there is none; node_plugin_gone answers whether there is none;
# node_plugin_ready whether there is one, with its plugin container ready.
node_plugin_pod() { # <h>
	kc get pods -n "$DRIVER_NS" -l app=dnv-csi-node \
		--field-selector "spec.nodeName=${HOST_NAME[$1]}" \
		-o 'jsonpath={.items[*].metadata.name}' 2>/dev/null
}

node_plugin_gone() { # <h>
	local out
	out=$(kc get pods -n "$DRIVER_NS" -l app=dnv-csi-node \
		--field-selector "spec.nodeName=${HOST_NAME[$1]}" \
		-o 'jsonpath={.items[*].metadata.name}' 2>/dev/null) || return 1
	[ -z "$out" ]
}

node_plugin_ready() { # <h>
	[ "$(kc get pods -n "$DRIVER_NS" -l app=dnv-csi-node \
		--field-selector "spec.nodeName=${HOST_NAME[$1]}" \
		-o 'jsonpath={.items[*].status.containerStatuses[?(@.name=="dnv-csi")].ready}' 2>/dev/null)" = true ]
}

# case_restart: host0's node plugin is away while its volume's primary fails
# over, so it misses the cdc's change notice and host0 keeps the old
# primary's path; when the plugin comes back, its first pass drops that path
# at once, well before the plugin's timer would (csi.md CSI15). The old
# primary's cntlr is not replaced within the case (the cntlr threshold is
# minutes), so no later change notice reaches host0 to start a pass.
case_restart() {
	CASE=restart
	local pv old v ip stat
	stage 01 "a claim and a pod on host0 that holds it"
	claim s0 1Gi Filesystem
	wait_until "$WAIT_PVC" "claim s0 Bound" pvc_bound s0
	pv=$(pvc_volume s0)
	run_pod ws s0 0 Filesystem "$(hold_script)"
	old=$(primary_of "$pv") || die "reading the primary of $pv failed"
	v=$(cn_index_of "$old") || die "the primary $old is on no --cn guest"
	ip=${CN_IP[$v]}
	log "  the primary of $pv is $old (cn$v)"
	wait_until "$WAIT_SHORT" "host0 to hold a path to the primary at $ip" has_path_to 0 "$ip"

	stage 02 "take the node plugin off host0; host0 keeps its paths and the mount"
	panic_pods
	kc -n "$DRIVER_NS" patch ds dnv-csi-node --type merge \
		-p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"${HOST_NAME[1]}\"}}}}}" \
		>/dev/null || die "keeping the node plugin to host1 failed"
	wait_until "$WAIT_POD" "the node plugin to leave host0" node_plugin_gone 0
	has_path_to 0 "$ip" || die "host0 lost its path to the primary when its node plugin stopped"

	stage 03 "cut the primary's cn agent off (SIGSTOP); host0 keeps the old path through the failover"
	stat=$(helper_cn "$v" sig_pidfile "$WORK/cn/pid" STOP) || die "cn$v: sig_pidfile failed"
	assert_eq "$stat" "state=T" "cn$v: the agent is stopped"
	wait_until "$WAIT_FAILOVER" "the primary of $pv to move away from $old" primary_moved "$pv" "$old"
	log "  the primary moved to $(primary_of "$pv")"
	# The failover's own write takes the old primary out of the discovery
	# records; the cdc's notice of it reaches no plugin on host0.
	sleep 5
	has_path_to 0 "$ip" || die "host0 dropped the old primary's path with no node plugin on it"

	stage 04 "the node plugin back on host0: its first pass drops the old path"
	panic_pods
	kc -n "$DRIVER_NS" patch ds dnv-csi-node --type json \
		-p '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]' >/dev/null ||
		die "giving the node plugin back to host0 failed"
	wait_until "$WAIT_POD" "the node plugin to run on host0" node_plugin_ready 0
	wait_until "$RESTART_DROP" "host0 to drop its path to the old primary at $ip within ${RESTART_DROP}s of its node plugin's start" \
		no_path_to 0 "$ip"
	kc_exec ws sh -c 'echo after > /data/after && sync' >/dev/null ||
		die "the pod cannot write its volume through the new primary"
	kc -n "$DRIVER_NS" rollout status ds/dnv-csi-node "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the node plugin did not roll out"

	stage 05 "give the agent back, delete, and nothing left"
	stat=$(helper_cn "$v" sig_pidfile "$WORK/cn/pid" CONT) || die "cn$v: sig_pidfile failed"
	log "  cn$v agent $stat"
	delete_pod ws
	delete_claim s0
	nothing_left
	log "=== $CASE: PASS"
}

# va_on answers whether a VolumeAttachment of a volume to a node stands: 0 it
# does, 1 it does not, 2 the listing did not answer, which never passes for
# "does not".
va_on() { # <pv> <node name>
	local out
	out=$(kc get volumeattachments \
		-o 'jsonpath={range .items[*]}{.spec.source.persistentVolumeName} {.spec.nodeName}{"\n"}{end}') ||
		return 2
	printf '%s\n' "$out" | awk -v p="$1" -v n="$2" '$1 == p && $2 == n { f = 1 } END { exit !f }'
}

# host_conn_on answers whether a host holds an nvmet connection to one
# subsystem on a controller-node guest, read from nvmet's debug directory
# there: 0 it does, 1 it does not, 2 the listing did not answer, which never
# passes for "does not".
host_conn_on() { # <cn index> <subsystem nqn> <host nqn>
	local out
	out=$(QUIET=1 helper_cn "$1" nvmet_conns "$2" 2>/dev/null) || return 2
	printf '%s\n' "$out" | awk -v h="$3" '$2 == h { f = 1 } END { exit !f }'
}

# off_list answers whether the gateway's record of a subsystem no longer
# names a host among its allowed hosts, as once a detach's write landed; a
# record that lacks the subsystem, or a read that fails, answers no.
off_list() { # <pool> <subsystem nqn> <host nqn>
	local out
	ctl_try ss list --sp "$1" || return 1
	out=$(jq_of "$CTL_OUT" ".nqn_to_subsystem[\"$2\"].allowed_hosts | if . == null then \"MISSING\" else .[] end") ||
		return 1
	[ "$out" != MISSING ] && ! printf '%s\n' "$out" | grep -qxF "$3"
}

# detach_stat reads the detach case's writer on host0 through the node's
# container runtime: "<writes> <failed> <last ok, ns> <first failure, ns>
# <start of a write in flight, ns, or 0>", host0's clock.
detach_stat() {
	QUIET=1 helper_host 0 container_cat "$TEST_NS" wd io /tmp/stat 2>/dev/null
}

# The detach case's writer: a pattern and its digest, then a small synced
# write every half second, its count, its failures and their times kept in
# the container's own /tmp, which the suite reads over ssh through the
# node's container runtime, since host0 is cut off from the API server when
# it matters. Its marker is in /tmp too, so that the volume carries none for
# the next pod's wait.
detach_writer_script() {
	cat <<'EOF'
dd if=/dev/urandom of=/data/pattern bs=1M count=8 conv=fsync status=none || exit 1
sha256sum /data/pattern > /data/sha || exit 1
sync
echo done > /tmp/marker
i=0; fail=0; lastok=0; firstfail=0
while :; do
  s=$(date +%s%N)
  echo "$i $fail $lastok $firstfail $s" > /tmp/stat.tmp && mv /tmp/stat.tmp /tmp/stat
  if dd if=/dev/zero of=/data/w bs=4k count=1 conv=fsync status=none 2>/dev/null; then
    lastok=$(date +%s%N)
  else
    fail=$((fail+1)); [ "$firstfail" -ne 0 ] || firstfail=$(date +%s%N)
  fi
  i=$((i+1))
  echo "$i $fail $lastok $firstfail 0" > /tmp/stat.tmp && mv /tmp/stat.tmp /tmp/stat
  sleep 0.5
done
EOF
}

# The script of the claim's new pod on host1: it checks the pattern host0
# wrote, which host1's page cache never held, then writes a pattern of its
# own and checks it.
detach_read_script() {
	cat <<'EOF'
set -e
sha256sum -c /data/sha > /tmp/check 2>&1 || true
dd if=/dev/urandom of=/data/after bs=1M count=16 conv=fsync status=none
sha256sum /data/after > /data/sha-after
sync
sha256sum -c /data/sha-after > /tmp/check-after 2>&1 || true
echo done > /tmp/marker
exec sleep 1000000
EOF
}

# va_attached answers whether a VolumeAttachment of a volume to a node reads
# attached: 0 it does, 1 it does not or there is none, 2 the listing did not
# answer.
va_attached() { # <pv> <node name>
	local out
	out=$(kc get volumeattachments \
		-o 'jsonpath={range .items[*]}{.spec.source.persistentVolumeName} {.spec.nodeName} {.status.attached}{"\n"}{end}') ||
		return 2
	printf '%s\n' "$out" | awk -v p="$1" -v n="$2" '$1 == p && $2 == n && $3 == "true" { f = 1 } END { exit !f }'
}

# call_replies prints the controller plugin's replies to one CSI method, of
# the last <secs> seconds, from the log of one controller copy: the time, the
# code and the duration of each.
call_replies() { # <pod> <secs> <method>
	kc -n "$DRIVER_NS" logs "$1" -c dnv-csi "--since=${2}s" 2>/dev/null |
		"$JQ" -R -r --arg m "$3" 'fromjson? | objects | select(.msg == "csi reply" and (.method // "" | endswith($m))) | "\(.time) \(.code) \(.duration_ms) ms"' 2>/dev/null |
		tail -n 12
}

# call_records prints the controller plugin's requests and replies of one
# CSI method, of the last <secs> seconds, so that a call still waiting shows
# as a request with no reply after it.
call_records() { # <pod> <secs> <method>
	kc -n "$DRIVER_NS" logs "$1" -c dnv-csi "--since=${2}s" 2>/dev/null |
		"$JQ" -R -r --arg m "$3" 'fromjson? | objects | select((.msg == "csi request" or .msg == "csi reply") and (.method // "" | endswith($m))) | "\(.time) \(.msg) \(.trace_id) \(.code // "") \(.duration_ms // "")"' 2>/dev/null |
		tail -n 16
}

# host_listed answers whether the gateway's record of a subsystem names a
# host among its allowed hosts: 0 it does, 1 it does not, 2 the read did not
# answer or the record lacks the subsystem.
host_listed() { # <pool> <subsystem nqn> <host nqn>
	local out
	ctl_try ss list --sp "$1" || return 2
	out=$(jq_of "$CTL_OUT" ".nqn_to_subsystem[\"$2\"].allowed_hosts | if . == null then \"MISSING\" else .[] end") ||
		return 2
	[ "$out" != MISSING ] || return 2
	printf '%s\n' "$out" | grep -qxF "$3"
}

# case_detach: a detach Kubernetes forces while the old host still reaches
# the controller nodes ends only once that host's nvmet connection on the
# primary is gone (csi.md CSI10). A pod on host0 writes in a loop; host0's
# node plugin is taken away, so that nothing on host0 drops its paths; one
# controller copy, off host0, holds both leases; host0 is cut off from the
# control-plane guest alone and reads NotReady. The worker is stopped, so
# that no cn agent converges and the primary cannot cut, and host0 takes the
# out-of-service taint: Kubernetes force-deletes the pod and detaches the
# volume, the detach takes host0 off the list, and its VolumeAttachment
# stays while host0's connection does, through a call of the attacher that
# runs out of time and its repeat; the attach to host1, which Kubernetes
# makes once that call has failed, waits for the same cut and stays
# unattached. The worker is given back: the primary cuts host0, and then
# the detach ends and host1 is attached. The detach does not wait for the
# standby (csi.md CSI10), but host0's writes, which its container goes on
# making, fail only once every cntlr has ended host0's connection, the
# standby's included (cnagent.md CN33), since IO queues while a standby's
# path is left (csi.md, The node plugin), and host0's reconnect to each has
# been refused with DNR, which deletes that controller (csi.md CSI14). The
# claim's new pod on host1 reads host0's pattern and writes and reads its
# own. The taint and the cut go at the end, and on every failure (CSI5).
case_detach() {
	CASE=detach
	local pv nqn old v sv keep pod out rc t0 h0ns end now late both taint_at started a b word
	local standbys="" sbend="" sbheld="" toff="" tres="" tcut="" tva="" tatt="" tfail="" thost1=""
	local stat count fail lastok firstfail inflight
	stage 01 "a claim and a pod on host0 that writes a pattern, then writes in a loop"
	claim d0 1Gi Filesystem
	wait_until "$WAIT_PVC" "claim d0 Bound" pvc_bound d0
	pv=$(pvc_volume d0)
	nqn=$(kc get pv "$pv" -o 'jsonpath={.spec.csi.volumeAttributes.nqn}') ||
		die "reading the subsystem NQN of $pv failed"
	[ -n "$nqn" ] || die "the volume $pv names no subsystem NQN"
	run_pod wd d0 0 Filesystem "$(detach_writer_script)" /tmp/marker
	old=$(primary_of "$pv") || die "reading the primary of $pv failed"
	v=$(cn_index_of "$old") || die "the primary $old is on no --cn guest"
	ctl_try sp get --sp "$pv" || die "reading the pool $pv failed"
	for out in $(jq_of "$CTL_OUT" '.cntlr_list[] | select(.primary != true) | .addr_port'); do
		sv=$(cn_index_of "$out") || die "the standby $out is on no --cn guest"
		standbys="$standbys $sv"
	done
	log "  the primary of $pv is $old (cn$v), the standby on cn${standbys# }, its subsystem $nqn"
	wait_until "$WAIT_SHORT" "host0's writer to write" writer_wrote
	# The positive control of the cut: host0's nvmet connection is there
	# before anything is cut.
	rc=0
	host_conn_on "$v" "$nqn" "${HOST_NQN[0]}" || rc=$?
	[ "$rc" = 0 ] || die "cn$v: host0 holds no nvmet connection to $nqn before the cut (rc $rc)"

	stage 02 "take the node plugin off host0; host0 keeps its paths and the mount"
	panic_pods
	kc -n "$DRIVER_NS" patch ds dnv-csi-node --type merge \
		-p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"${HOST_NAME[1]}\"}}}}}" \
		>/dev/null || die "keeping the node plugin to host1 failed"
	wait_until "$WAIT_POD" "the node plugin to leave host0" node_plugin_gone 0
	has_path_to 0 "${CN_IP[$v]}" || die "host0 lost its path to the primary when its node plugin stopped"

	stage 03 "one controller copy, off host0, holds both leases"
	wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
	keep=$(live_copies | awk -v n="${HOST_NAME[0]}" '$2 != n {print $1; exit}')
	[ -n "$keep" ] || die "no controller copy runs off host0: $(live_copies | tr '\n' ',')"
	for pod in $(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller -o 'jsonpath={.items[*].metadata.name}'); do
		if [ "$pod" = "$keep" ]; then
			kc -n "$DRIVER_NS" annotate pod "$pod" --overwrite \
				controller.kubernetes.io/pod-deletion-cost=1000 >/dev/null || die "annotating $pod failed"
		else
			kc -n "$DRIVER_NS" annotate pod "$pod" --overwrite \
				controller.kubernetes.io/pod-deletion-cost=-1000 >/dev/null || die "annotating $pod failed"
		fi
	done
	panic_pods
	kc -n "$DRIVER_NS" scale deploy/dnv-csi-controller --replicas=1 >/dev/null || die "scaling to one copy failed"
	wait_until "$WAIT_ROLLOUT" "one controller copy Ready, none being deleted" copies_ready 1
	wait_until "$WAIT_LEASE" "both leases on $keep, the one copy left" leases_on "$keep"
	log "  both leases on $keep, on $(live_copies | awk -v k="$keep" '$1 == k {print $2}')"

	stage 04 "cut host0 off from the control-plane guest alone; host0 reads NotReady"
	# The cut lets Kubernetes delete host0's pods, and their logs with them.
	panic_pods
	CUT_TRIED=1
	out=$(helper_host 0 net_cut_peer "$CUT_LIMIT" "$CP_IP") || die "host0: the cut failed: $out"
	assert_eq "$out" cut "host0: the cut from $CP_IP"
	wait_until "$WAIT_NODE_LOST" "${HOST_NAME[0]} not Ready" not_ready "${HOST_NAME[0]}"
	has_path_to 0 "${CN_IP[$v]}" || die "host0 lost its path to the primary with the cut from $CP_IP"
	rc=0
	host_conn_on "$v" "$nqn" "${HOST_NQN[0]}" || rc=$?
	[ "$rc" = 0 ] || die "cn$v: host0's nvmet connection went with the cut from $CP_IP alone (rc $rc)"
	writer_wrote || die "host0's writer failed a write or stopped with the cut from $CP_IP: $(detach_stat)"

	stage 05 "the worker stopped and the out-of-service taint: the detach takes host0 off the list and waits while host0's connection stands"
	# No cn agent converges while the worker is stopped, so the primary
	# cannot cut: a detach that did not wait for the cut would end here.
	# The positive control of the hold: the attachment to host0 reads as
	# standing before anything detaches.
	rc=0
	QUIET=1 va_on "$pv" "${HOST_NAME[0]}" || rc=$?
	[ "$rc" = 0 ] || die "the VolumeAttachment of $pv to host0 does not read as standing before the taint (rc $rc)"
	out=$(helper_cp sig_pidfile "$WORK/worker/pid" STOP) || die "cp: sig_pidfile failed"
	assert_eq "$out" "state=T" "cp: the worker is stopped"
	h0ns=$(QUIET=1 helper_host 0 now_ns) || die "host0: reading its clock failed"
	TAINT_TRIED=1
	kc taint node "${HOST_NAME[0]}" "$OOS_TAINT" >/dev/null || die "tainting ${HOST_NAME[0]} out of service failed"
	t0=$SECONDS
	# kubectl stamps a NoExecute taint with its time, which host1's pod's
	# start is measured against.
	taint_at=$(kc get node "${HOST_NAME[0]}" \
		-o "jsonpath={.spec.taints[?(@.key==\"$OOS_TAINT_KEY\")].timeAdded}") || taint_at=""
	# The claim's new pod on host1: Kubernetes asks to attach it once a
	# detach call has failed, while host0's attachment still stands, and
	# that attach waits for the same cut.
	io_pod rd d0 1 Filesystem "$(detach_read_script)"
	wait_until "$WAIT_DETACH" "the detach to take host0 off the list of $nqn" \
		off_list "$pv" "$nqn" "${HOST_NQN[0]}"
	toff=$((SECONDS - t0))
	log "  host0 off the list of $nqn +${toff}s"
	# A pass proves the hold only when both reads answered; the hold ends
	# once it has lasted HOLD_CUT seconds, a pass of its last 5 s, after the
	# attacher's first call ran out of time, read both standing, and a pass
	# read the attach to host1 asked for and waiting. Once that call has
	# failed, Kubernetes asks to attach the volume to host1 while the detach
	# still runs; that attach waits for the same cut, so no pass may read
	# host1 attached or on the subsystem's list.
	end=$((SECONDS + HOLD_CUT))
	both=0
	late=0
	asked=0
	while :; do
		now=$SECONDS
		b=0
		QUIET=1 va_attached "$pv" "${HOST_NAME[1]}" || b=$?
		[ "$b" != 0 ] || die "the attach of $pv to host1 ended while the worker is stopped, so before any cut"
		rc=0
		QUIET=1 va_on "$pv" "${HOST_NAME[1]}" || rc=$?
		# The positive control of the attach's wait: Kubernetes has asked
		# for it, and it has not ended.
		[ "$rc" != 0 ] || [ "$b" != 1 ] || asked=1
		rc=0
		QUIET=1 host_listed "$pv" "$nqn" "${HOST_NQN[1]}" || rc=$?
		[ "$rc" != 0 ] || die "host1 is on the list of $nqn while the worker is stopped, so before any cut"
		rc=0
		QUIET=1 va_on "$pv" "${HOST_NAME[0]}" || rc=$?
		[ "$rc" != 1 ] || die "the detach of $pv from host0 ended while the worker is stopped, so before any cut"
		a=0
		host_conn_on "$v" "$nqn" "${HOST_NQN[0]}" || a=$?
		[ "$a" != 1 ] || die "cn$v: host0's nvmet connection went while the worker is stopped"
		if [ "$rc" = 0 ] && [ "$a" = 0 ]; then
			both=$((both + 1))
			[ "$now" -lt $((end - 5)) ] || late=1
		fi
		[ "$SECONDS" -lt "$end" ] || [ "$late" = 0 ] || [ "$asked" = 0 ] || break
		[ "$SECONDS" -lt $((end + 30)) ] ||
			die "the hold ran past its end with a pass of its last 5 s reading both the attachment and host0's connection" \
				"$([ "$late" = 1 ] && echo yes || echo no) ($both passes did), and the attach to host1 read asked and waiting" \
				"$([ "$asked" = 1 ] && echo yes || echo no)"
		sleep 2
	done
	log "  for ${HOLD_CUT}s with host0 off the list, its VolumeAttachment stood, and so did its connection on the primary" \
		"($both passes read both); the attach to host1 was asked for and stayed unattached, host1 off the list"
	# A call the attacher's own deadline ends replies CANCELED, as the
	# attacher cancels it, or DEADLINE_EXCEEDED.
	out=$(call_replies "$keep" $((SECONDS - t0 + 10)) ControllerUnpublishVolume) || true
	printf '%s\n' "$out" | grep -q ' Canceled \| DeadlineExceeded ' ||
		die "no detach call ran out of time while the worker was stopped; the controller plugin's detach replies since the taint:" \
			"${out:-none read}"
	out=$(call_records "$keep" $((SECONDS - t0 + 10)) ControllerPublishVolume) || true
	log "  the controller plugin's attach requests and replies while the worker was stopped:"
	printf '%s\n' "${out:-none read}" | sed 's/^/    /' >&2

	stage 06 "the worker back: the primary cuts host0, and only then the detach ends"
	out=$(helper_cp sig_pidfile "$WORK/worker/pid" CONT) || die "cp: sig_pidfile failed"
	case $out in
	state=T | state=none) die "cp: the worker did not go on: $out" ;;
	esac
	tres=$((SECONDS - t0))
	# Each pass reads the attachments first and the primary's connections
	# second, so an attachment to host0 gone, or one to host1 attached,
	# while host0 is still connected on the primary is a detach or an
	# attach that ended before the cut. A connection read that does not
	# answer proves nothing: once the cut was read, a refused host cannot
	# come back, so the pass stands; before, the next pass reads again.
	while :; do
		b=0
		QUIET=1 va_attached "$pv" "${HOST_NAME[1]}" || b=$?
		rc=0
		QUIET=1 va_on "$pv" "${HOST_NAME[0]}" || rc=$?
		a=0
		host_conn_on "$v" "$nqn" "${HOST_NQN[0]}" || a=$?
		if [ "$a" = 1 ] && [ -z "$tcut" ]; then
			tcut=$((SECONDS - t0))
			log "  host0's nvmet connection on the primary gone +${tcut}s"
		fi
		if [ "$a" = 2 ] && [ -z "$tcut" ]; then
			[ $((SECONDS - t0)) -lt "$WAIT_DETACH" ] ||
				die "timed out after ${WAIT_DETACH}s waiting for the detach of $pv from host0 (connection unread)"
			sleep 1
			continue
		fi
		if [ "$b" = 0 ]; then
			[ "$a" != 0 ] ||
				die "the attach of $pv to host1 ended while host0's nvmet connection on the primary cn$v is still there"
			if [ -z "$tatt" ]; then
				tatt=$((SECONDS - t0))
				log "  host1 attached +${tatt}s"
			fi
		fi
		# A pass whose read of the attachments did not answer proves
		# nothing; the next one reads again.
		if [ "$rc" = 1 ]; then
			[ "$a" != 0 ] ||
				die "the detach of $pv from host0 ended while host0's nvmet connection on the primary cn$v is still there"
			tva=$((SECONDS - t0))
			log "  the detach done, its VolumeAttachment gone +${tva}s"
			# The detach does not wait for the standbys (CSI10): their
			# connections at its end are logged.
			for sv in $standbys; do
				rc=0
				host_conn_on "$sv" "$nqn" "${HOST_NQN[0]}" || rc=$?
				case $rc in
				0) word="still there" ;;
				1) word=gone ;;
				*) word=unread ;;
				esac
				sbend="$sbend; the standby cn$sv: host0's nvmet connection $word"
			done
			log "  at the detach's end${sbend:-; no standby}"
			break
		fi
		[ $((SECONDS - t0)) -lt "$WAIT_DETACH" ] ||
			die "timed out after ${WAIT_DETACH}s waiting for the detach of $pv from host0 (connection rc $a)"
		sleep 1
	done
	out=$(call_replies "$keep" $((SECONDS - t0 + 10)) ControllerUnpublishVolume) || true
	log "  the controller plugin's detach replies since the taint:"
	printf '%s\n' "${out:-none read}" | sed 's/^/    /' >&2
	# The last call, a repeat of one that ran out of time, ended OK.
	printf '%s\n' "$out" | tail -n 1 | grep -q ' OK ' ||
		die "the last detach reply since the taint is not OK: $(printf '%s\n' "$out" | tail -n 1)"
	# The attach to host1, whose wait the cut now lets through.
	wait_until "$WAIT_DETACH" "the attach of $pv to host1" va_attached "$pv" "${HOST_NAME[1]}"
	[ -n "$tatt" ] || tatt=$((SECONDS - t0))

	stage 07 "host0's writes fail, and host0 holds no controller of the volume"
	wait_until "$WAIT_HOST_IO_FAIL" "host0's writer to fail a write" writer_failed_or_waits
	stat=$(detach_stat) || die "reading host0's writer failed"
	read -r count fail lastok firstfail inflight <<<"$stat"
	if [ "$fail" -eq 0 ]; then
		# host0's IO queues while any of its paths is left, the standby's
		# included (csi.md, The node plugin): name each cntlr that still
		# holds host0's connection.
		for sv in $v $standbys; do
			rc=0
			host_conn_on "$sv" "$nqn" "${HOST_NQN[0]}" || rc=$?
			[ "$rc" = 1 ] || sbheld="$sbheld; cn$sv still shows host0's nvmet connection (rc $rc)"
		done
		[ -n "$sbheld" ] || sbheld="; no cntlr shows host0's nvmet connection"
		die "host0's write waits instead of failing: in flight since" \
			"+$(((inflight - h0ns) / 1000000)) ms after the taint on host0's clock, $count writes," \
			"the last one ok +$(((lastok - h0ns) / 1000000)) ms$sbheld"
	fi
	tfail=$(((firstfail - h0ns) / 1000000))
	log "  host0: $count writes, $fail failed, the first +${tfail} ms after the taint on host0's clock"
	wait_until "$WAIT_HOST_IO_FAIL" "host0 to hold no controller of $nqn" no_ctrl_of 0 "$nqn"
	# With no controller left on host0, no cntlr holds its connection: the
	# standby's cut ended its path there, as nothing on host0 drops one.
	for sv in $standbys; do
		wait_until "$WAIT_SHORT" "cn$sv to show no nvmet connection of host0" conn_gone "$sv" "$nqn" "${HOST_NQN[0]}"
	done

	stage 08 "the claim's new pod on host1 reads host0's pattern, then writes and reads its own"
	wait_until "$WAIT_POD" "pod rd Running on host1" pod_settled rd
	pod_running rd || die "pod rd ended with phase $(pod_phase rd)"
	assert_eq "$(pod_node rd)" "${HOST_NAME[1]}" "the node of rd"
	started=$(kc get pod -n "$TEST_NS" rd \
		-o 'jsonpath={.status.containerStatuses[0].state.running.startedAt}') || started=""
	if [ -n "$started" ] && [ -n "$taint_at" ] &&
		a=$(date -d "$started" +%s 2>/dev/null) && b=$(date -d "$taint_at" +%s 2>/dev/null); then
		thost1="+$((a - b))s"
	else
		thost1="+$((SECONDS - t0))s or less (as seen)"
	fi
	wait_until "$WAIT_POD" "pod rd to finish its step" pod_marked rd /tmp/marker
	assert_eq "$(kc_exec rd cat /tmp/check)" "/data/pattern: OK" "pod rd reads host0's pattern"
	assert_eq "$(kc_exec rd cat /tmp/check-after)" "/data/after: OK" "pod rd reads its own pattern"
	log "  times after the taint: host0 off the list +${toff}s, the worker back +${tres}s," \
		"host0's connection on the primary gone +${tcut}s, the detach done +${tva}s," \
		"host1 attached +${tatt}s, host0's first failed write +${tfail} ms (host0's clock)," \
		"host1's pod running $thost1"

	stage 09 "the taint and the cut go; host0, its node plugin and two copies back; delete, and nothing left"
	kc taint node "${HOST_NAME[0]}" "$OOS_TAINT_KEY-" >/dev/null || die "removing the out-of-service taint failed"
	TAINT_TRIED=0
	out=$(helper_host 0 net_uncut) || die "host0: the uncut failed: $out"
	CUT_TRIED=0
	wait_until "$WAIT_NODES" "${HOST_NAME[0]} Ready" node_ready "${HOST_NAME[0]}"
	panic_pods
	kc -n "$DRIVER_NS" patch ds dnv-csi-node --type json \
		-p '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]' >/dev/null ||
		die "giving the node plugin back to host0 failed"
	wait_until "$WAIT_POD" "the node plugin to run on host0" node_plugin_ready 0
	kc -n "$DRIVER_NS" rollout status ds/dnv-csi-node "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the node plugin did not roll out"
	kc -n "$DRIVER_NS" scale deploy/dnv-csi-controller --replicas=2 >/dev/null || die "scaling to two copies failed"
	wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
	wait_until "$WAIT_LEASE" "both leases held by controller pods" leases_held
	delete_pod rd
	delete_claim d0
	nothing_left
	log "=== $CASE: PASS"
}

# writer_wrote answers whether the detach case's writer has made a write
# that held and has failed none.
writer_wrote() {
	local stat count fail lastok
	stat=$(detach_stat) || return 1
	read -r count fail lastok _ <<<"$stat"
	[ "${lastok:-0}" -gt 0 ] && [ "${fail:-0}" -eq 0 ]
}

# writer_failed_or_waits answers once the writer has failed a write, or a
# write of it has been in flight for more than a minute, which reads as a
# write that waits; the stage tells the two apart.
writer_failed_or_waits() {
	local stat count fail lastok firstfail inflight now
	stat=$(detach_stat) || return 1
	read -r count fail lastok firstfail inflight <<<"$stat"
	[ "${fail:-0}" -gt 0 ] && return 0
	[ "${inflight:-0}" -gt 0 ] || return 1
	now=$(QUIET=1 helper_host 0 now_ns) || return 1
	[ $(((now - inflight) / 1000000000)) -ge 60 ]
}

# no_ctrl_of answers whether a host holds no controller of one subsystem.
no_ctrl_of() { # <h> <subsystem nqn>
	local out
	out=$(QUIET=1 helper_host "$1" ctrl_addrs "$2") || return 1
	[ -z "$out" ]
}

# conn_gone answers whether a cntlr's nvmet debug directory answered and
# shows no connection of the host.
conn_gone() { # <cn index> <subsystem nqn> <host nqn>
	local rc=0
	host_conn_on "$1" "$2" "$3" || rc=$?
	[ "$rc" = 1 ]
}

# not_ready answers whether a node reads anything but Ready.
not_ready() { # <node name>
	local st
	st=$(kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) || return 1
	[ -n "$st" ] && [ "$st" != True ]
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

# fill_dev_ready answers whether host0 shows the fill volume's device at its
# full size.
fill_dev_ready() { # <device> <bytes>
	[ "$(QUIET=1 ssh_host 0 "blockdev --getsize64 $1 2>/dev/null")" = "$2" ]
}

# fill_grow_chain reads the fill's grow records, "<kind> <old total> <new
# total> <seconds> <time>" per line, and prints the count of data grows and
# whether each starts from the total the one before it reached: "<n>
# chained", or "<n> broken".
fill_grow_chain() { # <records>
	printf '%s\n' "$1" | awk '$1 == "grow_data" {
			if (n && $2 != last) bad = 1
			last = $3
			n++
		}
		END { printf "%d %s\n", n, bad ? "broken" : "chained" }'
}

# fill_hold_script holds a block claim and writes nothing.
fill_hold_script() {
	cat <<'EOF'
echo done > /tmp/marker
exec sleep 1000000
EOF
}

# case_fill fills a fresh volume from its first block to its last at the
# device's speed (csi.md, The cases): fio on host0 writes the whole device
# with direct IO and blocks that carry their own check, then reads every
# block back, while the pool grows under it one data group at a time. Any
# write or check error fails the case, and so does a count of the worker's
# data grow records for the pool other than the data groups the pool added;
# the longest write, the write rate and the time of each grow the worker
# logs are measured, not asserted, since no rule bounds a grow's time.
case_fill() {
	CASE=fill
	local pv uuid dev out sp recs grps
	stage 01 "a claim of $FILL_SIZE in block mode, held by a pod on host0"
	claim fl0 "$FILL_SIZE" Block
	wait_until "$WAIT_PVC" "claim fl0 Bound" pvc_bound fl0
	pv=$(pvc_volume fl0)
	run_pod fl fl0 0 Block "$(fill_hold_script)" /tmp/marker
	uuid=$(kc get pv "$pv" -o 'jsonpath={.spec.csi.volumeAttributes.uuid}') ||
		die "reading the namespace uuid of $pv failed"
	[ -n "$uuid" ] || die "the volume $pv names no namespace uuid"
	dev=/dev/disk/by-id/nvme-uuid.$uuid
	wait_until "$WAIT_SHORT" "host0 to show $dev at $FILL_BYTES bytes" fill_dev_ready "$dev" "$FILL_BYTES"
	ctl_ok sp get --sp "$pv"
	sp=$(jq_of "$CTL_OUT" '.sp_conf.sp_id')
	case $sp in '' | *[!0-9]*) die "the pool $pv reads no id ('$sp')" ;; esac
	log "  the pool $pv has id $sp"

	stage 02 "fio writes the whole device with checked blocks at the device's speed, then reads every block back"
	out=$(ssh_host 0 "timeout $WAIT_FILL fio --name=fill --filename=$dev --rw=write --bs=1M" \
		"--direct=1 --ioengine=libaio --iodepth=8 --verify=crc32c --do_verify=1" \
		"--lat_percentiles=1 --continue_on_error=all --output-format=json") ||
		die "fio on host0 failed: $(printf '%s' "$out" | tail -c 2000)"
	# fio's lat, a write's whole time from its submission to its completion,
	# not clat, which leaves the submission out: a write held while it is
	# submitted is part of the stall the case measures.
	log "  fill: $(jq_of "$out" '.jobs[0].write.io_kbytes') KiB written at $(jq_of "$out" '.jobs[0].write.bw') KiB/s" \
		"in $(jq_of "$out" '.jobs[0].write.runtime') ms;" \
		"write latency median $(jq_of "$out" '(.jobs[0].write.lat_ns.percentile["50.000000"] // 0) / 1000000 | floor') ms," \
		"99.9th $(jq_of "$out" '(.jobs[0].write.lat_ns.percentile["99.900000"] // 0) / 1000000 | floor') ms," \
		"longest $(jq_of "$out" '.jobs[0].write.lat_ns.max / 1000000 | floor') ms;" \
		"$(jq_of "$out" '.jobs[0].read.io_kbytes') KiB read back in $(jq_of "$out" '.jobs[0].read.runtime') ms"
	assert_eq "$(jq_of "$out" '.jobs[0].error')" 0 "the first error fio met on the fill"
	assert_eq "$(jq_of "$out" '.jobs[0].total_err')" 0 "the errors fio counted on the fill and its read back"
	assert_eq "$(jq_of "$out" '.jobs[0].write.io_kbytes * 1024')" "$FILL_BYTES" "the bytes fio wrote"
	assert_eq "$(jq_of "$out" '.jobs[0].read.io_kbytes * 1024')" "$FILL_BYTES" "the bytes fio read back"

	stage 03 "the grows the worker logged for the pool, one per data group it added"
	recs=$(ssh_cp "cat $WORK/worker/worker.log" |
		"$JQ" -rR --argjson sp "$sp" 'fromjson? | objects | select(.msg == "grow done" and .sp_id == $sp) |
			"\(.kind) \(.old_total) \(.new_total) \(.seconds) \(.time)"') ||
		die "reading the worker's grow records failed"
	printf '%s\n' "$recs" | awk 'NF { printf "  grow: %s %s -> %s in %s s, logged %s\n", $1, $2, $3, $4, $5 }' >&2
	ctl_ok sp get --sp "$pv"
	grps=$(jq_of "$CTL_OUT" '.slice_list[0].data_grp_list | length')
	case $grps in '' | *[!0-9]* | 0) die "the pool $pv reads no data group count ('$grps')" ;; esac
	# One record per data group the pool added past its first, each from the
	# total the one before it reached: a grow the record missed, or one that
	# logged twice, shows in the count or in the chain.
	assert_eq "$(fill_grow_chain "$recs")" "$((grps - 1)) chained" \
		"the data grows the worker logged for the fill, against the pool's $grps data groups"

	stage 04 "the pod and the claim go, and nothing left"
	delete_pod fl
	delete_claim fl0
	nothing_left
	log "=== $CASE: PASS"
}

# restarts_zero answers whether no container of the driver's pods has
# restarted. A sidecar that loses its lease exits, so the check runs before
# the machine case cuts a host.
restarts_zero() {
	local out
	out=$(kc -n "$DRIVER_NS" get pods \
		-o 'jsonpath={range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}') ||
		return 1
	[ -n "$out" ] && ! printf '%s\n' "$out" | grep -qvx 0
}

# live_copies prints the name and the node of each controller copy that is
# not being deleted, one per line: a copy the ReplicaSet or the case deleted
# stays listed while it terminates, and must not count as a copy.
live_copies() {
	kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		-o 'jsonpath={range .items[*]}{.metadata.name} del={.metadata.deletionTimestamp} {.spec.nodeName}{"\n"}{end}' 2>/dev/null |
		awk '$2 == "del=" {print $1, $3}'
}

# copy_on prints the name of each controller copy on a node that is not
# being deleted, one per line, nothing for none.
copy_on() { # <node name>
	live_copies | awk -v n="$1" '$2 == n {print $1}'
}

# copies_ready answers whether exactly <n> controller copies that are not
# being deleted run, each of them Ready.
copies_ready() { # <n>
	local out
	out=$(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		-o 'jsonpath={range .items[*]}{.metadata.name} del={.metadata.deletionTimestamp} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null) ||
		return 1
	[ "$(printf '%s\n' "$out" | awk '$2 == "del="' | wc -l)" = "$1" ] &&
		[ "$(printf '%s\n' "$out" | awk '$2 == "del=" && $3 == "True"' | wc -l)" = "$1" ]
}

# leases_on answers whether both leases name one pod.
leases_on() { # <pod>
	[ "$(lease_holders | grep -cx "$1")" = 2 ]
}

node_ready() { # <node name>
	[ "$(kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ]
}

# replaced_off answers whether a Ready controller copy that is not being
# deleted runs on a node other than the cut host, other than the copy that
# took the leases.
replaced_off() { # <cut node> <surviving pod>
	kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller \
		-o 'jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName} {.status.conditions[?(@.type=="Ready")].status} {.metadata.deletionTimestamp}{"\n"}{end}' 2>/dev/null |
		awk -v h="$1" -v s="$2" '$1 != s && $2 != h && $3 == "True" && NF == 3 { f = 1 } END { exit !f }'
}

# case_machine: the controller plugin's two copies survive the loss of the
# machine of the copy that leads (csi.md CSI19). Both leases are brought to
# the copy on one host H, which is then cut off the network by a firewall
# table: the other copy takes both leases within a lease time and serves a
# new volume on the other host, and a new copy runs on a third node while H
# stays cut, after the copies' short toleration. H is then healed. cp0, the
# k3s server and dnv's control plane, is never cut.
case_machine() {
	CASE=machine
	local h hn other pod keep t0 out pv
	stage 01 "two controller copies Ready, both leases held, and no driver container restarted"
	wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
	wait_until "$WAIT_LEASE" "both leases held by controller pods" leases_held
	restarts_zero || {
		kc_ok -n "$DRIVER_NS" get pods -o wide >&2
		die "a driver container restarted before the machine case"
	}

	stage 02 "both leases on the copy of one host H: the other copy deleted first, then scaled back"
	h=""
	for hn in 0 1; do
		if [ -n "$(copy_on "${HOST_NAME[$hn]}")" ]; then
			h=$hn
			break
		fi
	done
	[ -n "$h" ] || die "no controller copy runs on a host"
	other=$((1 - h))
	keep=$(copy_on "${HOST_NAME[$h]}" | awk 'NR == 1') || die "reading the copy on ${HOST_NAME[$h]} failed"
	log "  H is host$h (${HOST_NAME[$h]}), its copy $keep"
	for pod in $(kc -n "$DRIVER_NS" get pods -l app=dnv-csi-controller -o 'jsonpath={.items[*].metadata.name}'); do
		if [ "$pod" = "$keep" ]; then
			kc -n "$DRIVER_NS" annotate pod "$pod" --overwrite \
				controller.kubernetes.io/pod-deletion-cost=1000 >/dev/null || die "annotating $pod failed"
		else
			kc -n "$DRIVER_NS" annotate pod "$pod" --overwrite \
				controller.kubernetes.io/pod-deletion-cost=-1000 >/dev/null || die "annotating $pod failed"
		fi
	done
	panic_pods
	kc -n "$DRIVER_NS" scale deploy/dnv-csi-controller --replicas=1 >/dev/null || die "scaling to one copy failed"
	wait_until "$WAIT_LEASE" "both leases on $keep, the one copy left" leases_on "$keep"
	kc -n "$DRIVER_NS" scale deploy/dnv-csi-controller --replicas=2 >/dev/null || die "scaling to two copies failed"
	# The copies are read without the deleted one, which can still be
	# terminating: counted, it would stand for the second copy.
	wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
	out=$(live_copies) || die "reading the controller copies failed"
	log "  copies: $(printf '%s' "$out" | tr '\n' ',')"
	pod=$(printf '%s\n' "$out" | awk -v k="$keep" '$1 != k {print $1}')
	if [ "$(printf '%s\n' "$out" | awk -v k="$keep" '$1 != k {print $2}')" = "${HOST_NAME[$h]}" ]; then
		# The new copy landed on H too: delete it, so that its replacement
		# goes elsewhere. It holds no lease.
		panic_pods
		kc -n "$DRIVER_NS" delete pod "$pod" --wait=false >/dev/null || die "deleting $pod failed"
		wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
		out=$(live_copies) || die "reading the controller copies failed"
		pod=$(printf '%s\n' "$out" | awk -v k="$keep" '$1 != k {print $1}')
	fi
	[ "$(printf '%s\n' "$out" | grep -c .)" = 2 ] && [ "$(printf '%s\n' "$pod" | grep -c .)" = 1 ] &&
		[ "$(printf '%s\n' "$out" | awk -v p="$pod" '$1 == p {print $2}')" != "${HOST_NAME[$h]}" ] ||
		die "the second copy is not one copy on a node other than H: $out"
	leases_on "$keep" || die "the leases left $keep while the second copy started"

	stage 03 "cut H off the network; a new claim and a pod on the other host"
	# Kubernetes deletes the copy on H while H is cut, and its log with it.
	panic_pods
	CUT_TRIED=1
	out=$(helper_host "$h" net_cut "$CUT_LIMIT") || die "host$h: the cut failed: $out"
	assert_eq "$out" cut "host$h: the cut"
	t0=$SECONDS
	claim mc 1Gi Filesystem
	io_pod wm mc "$other" Filesystem "$(write_fs_script 16)"

	stage 04 "the copy $pod takes both leases"
	wait_until "$WAIT_LEASE" "both leases on $pod" leases_on "$pod"
	log "  both leases on $pod $((SECONDS - t0)) s after the cut"

	stage 05 "the claim is served by the copy that took over"
	wait_until "$WAIT_PVC" "claim mc Bound" pvc_bound mc
	pv=$(pvc_volume mc)
	wait_until "$WAIT_POD" "pod wm Running" pod_settled wm
	pod_running wm || die "pod wm ended with phase $(pod_phase wm)"
	wait_until "$WAIT_POD" "pod wm to finish its step" pod_marked wm /data/marker
	assert_eq "$(kc_exec wm sh -c 'sha256sum -c /data/sha 2>&1')" "/data/pattern: OK" "pod wm reads its pattern"
	log "  volume $pv written and read $((SECONDS - t0)) s after the cut"

	stage 06 "a new controller copy Ready off H while H stays cut"
	wait_until "$WAIT_REPLACE" "a new controller copy Ready off ${HOST_NAME[$h]}" replaced_off "${HOST_NAME[$h]}" "$pod"
	log "  a new copy Ready $((SECONDS - t0)) s after the cut"
	assert_eq "$(helper_host "$h" net_cut_state)" cut "host$h: the cut still stands"

	stage 07 "heal H; two copies, the node plugins, and no lease on H"
	out=$(helper_host "$h" net_uncut) || die "host$h: the uncut failed: $out"
	wait_until "$WAIT_NODES" "${HOST_NAME[$h]} Ready" node_ready "${HOST_NAME[$h]}"
	wait_until "$WAIT_ROLLOUT" "two controller copies Ready, none being deleted" copies_ready 2
	kc -n "$DRIVER_NS" rollout status ds/dnv-csi-node "--timeout=${WAIT_ROLLOUT}s" >/dev/null ||
		die "the node plugin did not come back"
	wait_until "$WAIT_POD" "the old copy $keep gone" copy_gone "$keep"
	leases_held || die "the leases are not held by running controller copies after the heal"
	! lease_holders | grep -qx "$keep" || die "the cut copy $keep holds a lease after the heal"

	stage 08 "delete, and nothing left"
	delete_pod wm
	delete_claim mc
	nothing_left
	log "=== $CASE: PASS"
}

copy_gone() { # <pod>
	! kc -n "$DRIVER_NS" get pod "$1" >/dev/null 2>&1
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
	panic_pods
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
	# --foreground keeps timeout in the suite's process group, so a signal to
	# the runner's group reaches the ssh too and the run ends with it.
	# Neither reaches csi-sanity on host0, which outlives the ssh;
	# sanity_stop ends it.
	timeout --foreground "$SANITY_TIMEOUT" ssh "${SSH_OPTS[@]}" "${HOST[0]}" \
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

run_case() {
	"case_$1"
	panic_scan
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
	parse_args "$@"
	trap on_exit EXIT
	# A run killed by SIGTERM, SIGINT or SIGHUP exits with the shell's
	# code for the signal, so on_exit reads it as the failure it is, never
	# as a PASS. The HUP trap ignores every later SIGHUP first: a terminal
	# that closes sends a second one right after the first.
	trap 'exit 143' TERM
	trap 'exit 130' INT
	trap 'trap "" HUP; exit 129' HUP
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
