#!/usr/bin/env bash
#
# tutorial_test.sh — the tutorial suite of tutorial_integtest.md: it builds
# the release archive, unpacks it on one lab guest that holds no dnv state,
# and runs the shell blocks of a tutorial there in the order the tutorial
# gives them, each checked by the HTML comment marker beside it; then it puts
# the guest back as it found it.
#
#   bash integtest/tutorial_test.sh [--part all-in-one|csi] [--archive FILE]
#        [--cleanup-only] user@host
#
# --part all-in-one (the default) runs doc/human/tutorial_all_in_one.md
# whole. --part csi runs doc/human/tutorial_csi.md on the lab's k3s: the
# guest named is its server node, and the prelude the CSI tutorial asks for
# (the all-in-one tutorial from its start through "Cluster and nodes", with
# the CSI tutorial's gateway start in place of the all-in-one's) runs first
# (tutorial_integtest.md, Topology). --archive takes a release archive
# instead of building one. --cleanup-only puts the guest back and runs no
# block.
#
# The markers (tutorial_integtest.md, The markers): an HTML comment that
# starts with "<!-- test" and ends on the line just before a block's opening
# fence binds to that block; each line inside it is one directive: wait
# SECS, timeout SECS, out TEXT, line TEXT, has TEXT, lacks TEXT, word TEXT,
# count N TEXT, same-first-word, save NAME, same NAME, skip REASON, omit
# TEXT, replaces TEXT, run. A marker that binds to no block, or a directive
# the suite does not know, fails the parse before anything runs. Before any
# block runs, the plan of the run checks every block it will run: each omit
# drops exactly one line, the CSI part's replaces takes the place of exactly
# one line of its prelude, and the CSI tutorial has one block with a
# replaces; the prelude's last section exists; every block of the CSI
# tutorial carries a marker; and no block's text names k3s or rancher but in
# `sudo k3s kubectl ` and `sudo k3s ctr ` (k3s_hit), whatever its marker says.
#
# Every block runs as its own file under `bash -e` in the unpacked release
# folder on the guest, with stdin from /dev/null, as the plain user; the
# tutorial's own sudo does the rest. Its stdout is what the directives read;
# its stdout and stderr both go to the transcript.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=/var/tmp/dnv-tutorial
SAVED=$WORK/saved
HELPER=$WORK/helper.sh
AIO_DOC=$REPO_ROOT/doc/human/tutorial_all_in_one.md
CSI_DOC=$REPO_ROOT/doc/human/tutorial_csi.md
BLOCK_TIMEOUT=300

# k3s_hit prints the first line of a block's text that names k3s or rancher,
# in any case, once every `sudo k3s kubectl ` and `sudo k3s ctr ` is taken
# out of it, and nothing when no line does. Those two are the uses the
# tutorials have of the lab's k3s; any other line that names it may install
# a second k3s beside the lab's, or stop or remove the lab's (csi.md CSI1),
# so the guard holds by the shape of a use, not by a list of the harmful
# ones.
k3s_hit() { # <text>
	sed -e 's/sudo k3s kubectl //g' -e 's/sudo k3s ctr //g' <<<"$1" |
		grep -iE 'k3s|rancher' | head -n 1 || true
}

# The packages the tutorials install. In the all-in-one part a guest that has
# one of them at another version than its candidate is refused at
# preflight: the tutorial's install would upgrade a tool the other suites
# run with. The CSI part lets the install upgrade one on the k3s server
# node, where no other suite runs a node's tools, and logs it.
AIO_PKGS=(nvme-cli mdadm nvme-stas dmsetup thin-provisioning-tools)
CSI_PKGS=(docker.io)
# The udev rule files the tutorials install and their teardowns remove.
UDEV_RULES=(58-dnv-dm.rules 63-dnv-md.rules 63-zz-dnv-md-array.rules)
# Every TCP port the all-in-one tutorial listens on, and the one the CSI
# tutorial adds on the server node: 9809, the health port of the CSI driver's
# node plugin, which runs on the host's network (deploy/node.yaml).
PORTS=(2379 2380 29527 8009 29900 29901 29902 29903 29950 4301 4302 4303 4304 4420)
CSI_PORTS=(9809)
# The taint by which the CSI part keeps the lab's other k3s nodes free of the
# tutorial's pods, so that the server node is the one machine the tutorial
# expects; the server's own taint is lifted for the run and put back.
TUT_TAINT_KEY=dnv.io/tutorial
CP_TAINT=node-role.kubernetes.io/control-plane:NoSchedule
# Every Kubernetes object the CSI tutorial makes in the lab's k3s, one
# "<source> <kind> <namespace, - for none> <name>" each: <source> is the
# manifest of the release's deploy/ that holds it, or inline for one the
# tutorial's text makes. The cleanup deletes these by kind and name, in this
# order, and nothing else, as a run with --cleanup-only has no release folder
# and the lab's k3s is permanent; setup checks, before any block, that the
# release's manifests hold the entries that name a manifest, and no other
# object (csi_objects_check). The probe pods the policy check runs with
# kubectl's --rm stay when that kubectl dies first: the one in default is
# listed, and the one in policy-check goes with its namespace.
CSI_OBJECTS=(
	"inline Pod default demo"
	"inline Pod default probe"
	"inline PersistentVolumeClaim default demo"
	"inline Namespace - policy-check"
	"networkpolicy.yaml NetworkPolicy default dnv-keep-off"
	"storageclass.yaml StorageClass - dnv"
	"node.yaml DaemonSet dnv-csi dnv-csi-node"
	"controller.yaml Deployment dnv-csi dnv-csi-controller"
	"controller-rolebinding.yaml RoleBinding dnv-csi dnv-csi-controller"
	"controller-role.yaml Role dnv-csi dnv-csi-controller"
	"controller-clusterrolebinding.yaml ClusterRoleBinding - dnv-csi-controller"
	"controller-clusterrole.yaml ClusterRole - dnv-csi-controller"
	"node-serviceaccount.yaml ServiceAccount dnv-csi dnv-csi-node"
	"controller-serviceaccount.yaml ServiceAccount dnv-csi dnv-csi-controller"
	"csidriver.yaml CSIDriver - csi.dnv.io"
	"config.yaml ConfigMap dnv-csi dnv-csi-config"
	"namespace.yaml Namespace - dnv-csi"
)

PART=all-in-one
ARCHIVE=""
CLEANUP_ONLY=0
TARGET=""
STAGE="(startup)"
SETUP_DONE=0
SAVE_DONE=0
DIR=""
PARSE=""
RELEASE_OUT=""
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

log() { echo "$*" >&2; }

stage() {
	STAGE=$1
	log ""
	log "=== $STAGE"
}

# die prints the failure report the runner reads (layout.md, Directory tree).
# The tutorial's commands carry no trace id of the suite's, so it reads "-".
die() {
	log ""
	log "FAILED at stage '$STAGE' (trace_id -)"
	log "  $*"
	exit 1
}

usage() {
	sed -n '9,10p' "${BASH_SOURCE[0]}" | sed 's/^# //' >&2
	exit 2
}

while [ $# -gt 0 ]; do
	case $1 in
	--part)
		[ $# -ge 2 ] || usage
		case $2 in all-in-one | csi) PART=$2 ;; *) usage ;; esac
		shift 2
		;;
	--archive)
		[ $# -ge 2 ] || usage
		ARCHIVE=$2
		shift 2
		;;
	--cleanup-only)
		CLEANUP_ONLY=1
		shift
		;;
	-*) usage ;;
	*)
		[ -z "$TARGET" ] || usage
		TARGET=$1
		shift
		;;
	esac
done
[ -n "$TARGET" ] || usage
case $TARGET in *@*) ;; *) usage ;; esac

rsh() { ssh "${SSH_OPTS[@]}" "$TARGET" "$@"; }

# helper runs one function of the guest's helper as root.
helper() { rsh "sudo -n bash $HELPER $*"; }

# The guest's helper, shipped at preflight and at a cleanup-only run. Every
# function runs as root and acts only on what the tutorial makes: preflight
# proved that the guest held none of it (dnv's devices, arrays, nvmet
# objects, NVMe-oF controllers and programs) before the run.
helper_text() {
	cat <<'HELPER_EOF'
set -uo pipefail
WORK=/var/tmp/dnv-tutorial
SAVED=$WORK/saved
NVMET=/sys/kernel/config/nvmet
UDEV_RULES="58-dnv-dm.rules 63-dnv-md.rules 63-zz-dnv-md-array.rules"

# state prints what preflight requires absent, one line each; empty means
# the guest holds no dnv state. A listing that fails prints an "unread:"
# line in place of its answer, so that every caller, which takes any line
# for something left, reads a failed listing as a guest not known clean,
# never as an empty one.
state() {
	local p out rc
	if ! command -v dmsetup >/dev/null 2>&1; then
		# No dmsetup, as on a guest the tutorial has yet to install it on:
		# the kernel's own list of dm devices.
		for p in /sys/block/dm-*/dm/name; do
			[ -e "$p" ] || continue
			awk '/^dnv-/ { print "dm device " $1 }' "$p" || echo "unread: $p"
		done
	elif out=$(dmsetup ls 2>&1); then
		awk '$1 ~ /^dnv-/ { print "dm device " $1 }' <<<"$out"
	else
		echo "unread: dmsetup ls failed: $(echo $out)"
	fi
	if [ -e /proc/mdstat ]; then
		awk '/^md[0-9]/ { print "md array " $1 }' /proc/mdstat ||
			echo "unread: /proc/mdstat"
	fi
	for p in "$NVMET"/ports/* "$NVMET"/subsystems/* "$NVMET"/hosts/*; do
		[ -e "$p" ] && echo "nvmet ${p#$NVMET/}"
	done
	for p in /sys/class/nvme-fabrics/ctl/nvme*; do
		[ -e "$p" ] && echo "NVMe-oF controller ${p##*/}"
	done
	out=$(pgrep -a -x 'dnv-agent|dnv-gateway|dnv-worker|dnv-cdc|dnv-csi|etcd')
	rc=$?
	[ "$rc" -le 1 ] || echo "unread: pgrep failed (exit $rc)"
	[ -z "$out" ] || sed 's/^/process /' <<<"$out"
	if out=$(losetup -a 2>&1); then
		grep -F "$WORK" <<<"$out" | sed 's/^/loop /'
	else
		echo "unread: losetup -a failed: $(echo $out)"
	fi
	return 0
}

# The readings save keeps and restored compares, one function each: the
# installed packages, the mask of nvme-cli's connector units, the policy of
# iptables' FORWARD chain, the network links, nvme-stas's two daemons
# (whether each starts at boot and whether it runs), which of docker's and
# containerd's state folders exist, whether the docker group does, and the
# images of k3s's store. Each fails when its reading fails, and never
# prints a partial answer as a whole one.
pkgs_now() {
	dpkg-query -W -f '${db:Status-Abbrev} ${Package}\n' | awk '$1 == "ii" { print $2 }' | LC_ALL=C sort
}
units_now() {
	local u st
	for u in nvmf-connect@.service nvmf-connect.target; do
		# is-enabled prints its answer and exits non-zero for masked, so
		# the answer is read alone.
		st=$(systemctl is-enabled "$u" 2>/dev/null) || true
		[ -n "$st" ] || return 1
		echo "$u $st"
	done
}
forward_now() {
	iptables -S FORWARD | awk '$1 == "-P" { print $3; f = 1 } END { exit !f }'
}
links_now() {
	ip -o link show | awk -F': ' '{ print $2 }' | sed 's/@.*//' | LC_ALL=C sort
}
daemons_now() {
	local u out
	for u in stafd stacd; do
		# systemctl show fails when it cannot read the manager, and names an
		# absent unit with LoadState=not-found.
		out=$(systemctl show -p LoadState -p UnitFileState -p ActiveState "$u") || return 1
		awk -F= -v u="$u" '{ v[$1] = $2 }
			END {
				if (v["LoadState"] == "" || v["ActiveState"] == "") exit 1
				st = v["LoadState"] == "not-found" || v["UnitFileState"] == "" ? "absent" : v["UnitFileState"]
				print u, st, v["ActiveState"]
			}' <<<"$out" || return 1
	done
}
dirs_now() {
	local f
	for f in /var/lib/docker /var/lib/containerd; do
		[ ! -e "$f" ] || echo "$f"
	done
}
group_now() {
	# addgroup and groupdel act on /etc/group, so the reading is that file:
	# grep exits 1 for no line and 2 when it could not read it. getent cannot
	# tell the two apart, as it exits 2 for both.
	local rc=0
	grep -q '^docker:' /etc/group || rc=$?
	case $rc in
	0) echo yes ;;
	1) echo no ;;
	*) return 1 ;;
	esac
}
images_now() {
	if command -v k3s >/dev/null; then
		k3s ctr images ls -q | LC_ALL=C sort
	fi
}
READINGS="pkgs units forward links daemons dirs group images"

# list_to writes a reading into its file, and reads it back, only when the
# reading succeeded, so that a failed one leaves no list, and restore deletes
# nothing by it.
list_to() { # <file> <reading>
	local out
	out=$("$2" 2>/dev/null) || {
		echo "saving ${1##*/} failed: $2 did not answer"
		return 1
	}
	record_to "$1" "${out:+$out$'\n'}" || {
		echo "saving ${1##*/} failed: its record did not read back"
		return 1
	}
}

# record_to writes <text> into <file> and reads it back, and leaves no file
# when either fails, so that a record exists only as it was meant.
record_to() { # <file> <text>
	printf '%s' "$2" >"$1.tmp" && mv -f "$1.tmp" "$1" &&
		printf '%s' "$2" | cmp -s - "$1" && return 0
	rm -f "$1.tmp" "$1"
	return 1
}

# save keeps what the tutorial changes and its teardown may not put back:
# the udev rule files, nvme-stas's files and every reading above. It fails,
# naming it, when any of them cannot be kept, and then writes no "complete"
# record. A file is recorded as there
# before the run only once its copy reads back the same as the original, and
# the udev rules' record (udev.saved, one "<file> present|absent" line each)
# is written only when every rule file was kept, so that the restore acts on
# a whole record or on none and never removes an original it has no copy of.
save() {
	local f bad=0 rec=""
	mkdir -p "$SAVED/udev" "$SAVED/stas" || return 1
	for f in $UDEV_RULES; do
		if [ ! -e "/etc/udev/rules.d/$f" ]; then
			rec+="$f absent"$'\n'
		elif cp -a "/etc/udev/rules.d/$f" "$SAVED/udev/" &&
			cmp -s "/etc/udev/rules.d/$f" "$SAVED/udev/$f"; then
			rec+="$f present"$'\n'
		else
			echo "saving the udev rule $f failed"
			bad=1
		fi
	done
	if [ "$bad" = 0 ]; then
		record_to "$SAVED/udev.saved" "$rec" || {
			echo "recording the udev rules failed"
			bad=1
		}
	fi
	if [ -d /etc/stas ]; then
		if cp -a /etc/stas/. "$SAVED/stas/" &&
			diff -r -q /etc/stas "$SAVED/stas" >/dev/null 2>&1 &&
			record_to "$SAVED/stas.present" yes; then :; else
			rm -f "$SAVED/stas.present"
			echo "saving /etc/stas failed"
			bad=1
		fi
	else
		record_to "$SAVED/stas.present" no || {
			echo "recording that /etc/stas is absent failed"
			bad=1
		}
	fi
	for f in $READINGS; do
		list_to "$SAVED/$f" "${f}_now" || bad=1
	done
	[ "$bad" = 0 ] || return 1
	# The save's last record: the save is complete, every record above
	# written, read back and on disk. It alone lets the wipe stop nvme-stas
	# and the cleanup run the restore, as setup stops the run, before any
	# block, at a save that did not complete, which so changed nothing to
	# put back. A read-back reads the page cache, so the records go to disk
	# before the record that vouches for them, and that one before any block
	# runs: a reset before the write-back would leave it over empty records.
	sync || {
		echo "writing the records to disk failed"
		return 1
	}
	record_to "$SAVED/complete" yes && sync || {
		echo "recording the save as complete failed"
		rm -f "$SAVED/complete"
		return 1
	}
}

# stop_x stops every process whose name the pattern matches, TERM first,
# KILL after 30 s.
stop_x() { # <pgrep -x pattern>
	local i
	pkill -x "$1" 2>/dev/null
	for i in $(seq 30); do
		pgrep -x "$1" >/dev/null || return 0
		sleep 1
	done
	pkill -9 -x "$1" 2>/dev/null
	sleep 1
}

# wipe takes down what a run left and prints what stays. First the
# programs, the worker first, so that no syncup rebuilds what the steps
# after take down and no agent connects a leg again; then nvme-stas, which
# would connect the host again, when the save is complete, as only then did
# a block run; then nvmet, whose enabled namespaces hold
# open the disk nodes' sides and the controller node's namespace devices,
# so that no dm device or md array below them can go before it; then the
# host's NVMe-oF controllers, the md arrays' legs among them; then the dm
# devices and the md arrays in rounds, each round taking what the one
# before freed, until none is left; then the loop devices and the crash
# scan's sample folder a stopped preflight left; and last the read of what
# stays.
wipe() {
	local p pid d i link f
	stop_x dnv-worker
	stop_x dnv-agent
	stop_x 'dnv-gateway|dnv-cdc'
	for pid in $(pgrep -x etcd); do
		[ "$(readlink "/proc/$pid/cwd")" = "$(cat "$SAVED/dir" 2>/dev/null)" ] && kill "$pid"
	done
	if [ -f "$SAVED/complete" ]; then
		for p in stacd stafd; do systemctl stop "$p" 2>/dev/null; done
	fi
	if [ -d "$NVMET" ]; then
		for d in "$NVMET"/ports/*; do
			[ -d "$d" ] || continue
			for link in "$d"/subsystems/*; do [ -e "$link" ] && rm -f "$link"; done
		done
		for d in "$NVMET"/subsystems/*; do
			[ -d "$d" ] || continue
			for f in "$d"/namespaces/*; do
				[ -d "$f" ] || continue
				echo 0 >"$f/enable" 2>/dev/null
				rmdir "$f" 2>/dev/null
			done
			for link in "$d"/allowed_hosts/*; do [ -e "$link" ] && rm -f "$link"; done
			rmdir "$d" 2>/dev/null
		done
		for d in "$NVMET"/ports/*; do
			[ -d "$d" ] || continue
			rmdir "$d/ana_groups/3" "$d/ana_groups/2" 2>/dev/null
			rmdir "$d" 2>/dev/null
		done
		for d in "$NVMET"/hosts/*; do [ -d "$d" ] && rmdir "$d" 2>/dev/null; done
	fi
	nvme disconnect-all >/dev/null 2>&1
	for i in $(seq 30); do
		ls /sys/class/nvme-fabrics/ctl/ 2>/dev/null | grep -q nvme || break
		sleep 1
	done
	for d in $(dmsetup ls 2>/dev/null | awk '$1 ~ /^dnv-/ { print $1 }' | sort -r); do
		dmsetup resume "$d" 2>/dev/null
	done
	for i in 1 2 3 4 5 6; do
		for d in $(dmsetup ls 2>/dev/null | awk '$1 ~ /^dnv-/ { print $1 }'); do
			dmsetup remove "$d" 2>/dev/null
		done
		for d in $(awk '/^md[0-9]/ { print $1 }' /proc/mdstat); do
			mdadm --stop "/dev/$d" >/dev/null 2>&1
		done
		[ -z "$(dmsetup ls 2>/dev/null | awk '$1 ~ /^dnv-/')$(awk '/^md[0-9]/' /proc/mdstat)" ] && break
		sleep 1
	done
	for d in $(losetup -a 2>/dev/null | grep "$WORK" | cut -d: -f1); do
		losetup -d "$d" 2>/dev/null
	done
	# The crash scan's sample folder of a preflight that stopped before it
	# removed it (panic_plant).
	rm -rf /var/tmp/dnv-panic-plant-tutorial.*
	state
}

# udev_diff prints, one line each, a udev rule file that is not as the
# save's record (udev.saved) has it: a kept file that differs from its copy,
# with <how> after its name, or one the record has absent that is there; and
# one line when the record is missing.
udev_diff() { # <how>
	local f st
	if [ ! -f "$SAVED/udev.saved" ]; then
		echo "unread: the udev rules were not saved"
		return 0
	fi
	while read -r f st; do
		case $st in
		present)
			cmp -s "$SAVED/udev/$f" "/etc/udev/rules.d/$f" || echo "udev rule $f $1"
			;;
		*) [ ! -e "/etc/udev/rules.d/$f" ] || echo "udev rule $f left" ;;
		esac
	done <"$SAVED/udev.saved"
}

# after prints what the tutorial's own teardown left that the tutorial
# says it removes, one line each, before the suite's cleanup touches the
# guest: every dnv object state lists, a udev rule file that differs from
# before the run, a connector unit whose mask differs from before the run,
# and, for the all-in-one tutorial, whose teardown puts nvme-stas's files
# back and leaves its daemons stopped, a file of /etc/stas that differs from
# before the run and a daemon that runs; a reading that fails prints a line,
# state's and the daemons' an "unread:" one, so that no failed reading passes
# for a teardown that did what the tutorial says. Empty means the teardown
# did what the tutorial says.
after() { # <all-in-one|csi>
	local f u st cur
	state
	udev_diff "differs from before the run"
	while read -r u st; do
		cur=$(systemctl is-enabled "$u" 2>/dev/null) || true
		[ "$cur" = "$st" ] || echo "unit $u reads ${cur:-nothing}, before the run $st"
	done <"$SAVED/units"
	[ "$1" = all-in-one ] || return 0
	case $(cat "$SAVED/stas.present" 2>/dev/null) in
	yes)
		diff -r -q "$SAVED/stas" /etc/stas >/dev/null 2>&1 ||
			echo "/etc/stas differs from before the run: $(diff -r -q "$SAVED/stas" /etc/stas 2>&1 | head -n 3 | tr '\n' ' ')"
		;;
	no) ;;
	*) echo "unread: whether /etc/stas was there before the run" ;;
	esac
	for u in stafd stacd; do
		st=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || st=""
		case $st in
		inactive | failed) ;;
		"") echo "unread: the state of $u" ;;
		*) echo "$u runs" ;;
		esac
	done
	return 0
}

# restore puts back what save kept, purges every package the run added,
# deletes the bridge docker made, the state folders and the group the
# run's packages left, and the images the run added to k3s's store; a step
# whose reading save could not keep deletes nothing and is reported: it acts
# only on what the save recorded, and never removes an original it has no
# saved copy of. Then it reads everything back (restored). It prints each step
# that failed and each difference, and then exits non-zero.
restore() {
	local f u st act new cur bad=0
	fail() {
		echo "$*"
		bad=1
	}
	if [ -f "$SAVED/udev.saved" ]; then
		while read -r f st; do
			case $st in
			present) cp -a "$SAVED/udev/$f" /etc/udev/rules.d/ || fail "putting the udev rule $f back failed" ;;
			absent) rm -f "/etc/udev/rules.d/$f" || fail "removing the udev rule $f failed" ;;
			esac
		done <"$SAVED/udev.saved"
		udevadm control --reload || fail "reloading udev's rules failed"
	else
		fail "the udev rules were not saved: left as they are"
	fi
	if [ -f "$SAVED/units" ]; then
		while read -r u st; do
			case $st in
			masked) systemctl mask "$u" >/dev/null 2>&1 || fail "masking $u failed" ;;
			*) systemctl unmask "$u" >/dev/null 2>&1 || fail "unmasking $u failed" ;;
			esac
		done <"$SAVED/units"
	else
		fail "the units' masks were not saved: left as they are"
	fi
	if [ -f "$SAVED/pkgs" ]; then
		if cur=$(pkgs_now); then
			new=$(LC_ALL=C comm -13 "$SAVED/pkgs" - <<<"$cur")
			if [ -n "$new" ]; then
				echo "purging the packages the run added: $(echo $new)"
				DEBIAN_FRONTEND=noninteractive apt-get purge -y -q $new >/dev/null || fail "purge failed"
			fi
		else
			fail "listing the packages failed: none purged"
		fi
	else
		fail "the packages were not saved: none purged"
	fi
	# nvme-stas's files after the purge, which can take a package's files
	# away, and only by the save's record of them.
	case $(cat "$SAVED/stas.present" 2>/dev/null) in
	yes)
		{ mkdir -p /etc/stas && find /etc/stas -mindepth 1 -maxdepth 1 -exec rm -rf {} + &&
			cp -a "$SAVED/stas/." /etc/stas/; } || fail "putting /etc/stas back failed"
		;;
	no) rm -rf /etc/stas || fail "removing /etc/stas failed" ;;
	*) fail "nvme-stas's files were not saved: left as they are" ;;
	esac
	if [ -f "$SAVED/dirs" ]; then
		for f in /var/lib/docker /var/lib/containerd; do
			[ -e "$f" ] && ! grep -qxF "$f" "$SAVED/dirs" && { rm -rf "$f" || fail "removing $f failed"; }
		done
	else
		fail "the state folders were not saved: none removed"
	fi
	if [ -f "$SAVED/group" ]; then
		if [ "$(cat "$SAVED/group")" = no ]; then
			cur=$(group_now) || cur=unread
			case $cur in
			yes) groupdel docker || fail "deleting the group docker failed" ;;
			no) ;;
			*) fail "reading /etc/group failed: the group docker left as it is" ;;
			esac
		fi
	else
		fail "the docker group was not saved: left as it is"
	fi
	if [ -f "$SAVED/images" ]; then
		if cur=$(images_now); then
			for f in $(LC_ALL=C comm -13 "$SAVED/images" - <<<"$cur"); do
				k3s ctr images rm "$f" >/dev/null 2>&1 || fail "deleting the image $f from k3s's store failed"
			done
		else
			fail "listing k3s's images failed: none deleted"
		fi
	else
		fail "k3s's images were not saved: none deleted"
	fi
	if [ -f "$SAVED/forward" ]; then
		st=$(cat "$SAVED/forward")
		case $st in ACCEPT | DROP) iptables -P FORWARD "$st" || fail "setting the FORWARD policy failed" ;; esac
	else
		fail "the FORWARD policy was not saved: left as it is"
	fi
	if [ -f "$SAVED/links" ]; then
		if cur=$(links_now); then
			for f in $(LC_ALL=C comm -13 "$SAVED/links" - <<<"$cur" | grep -x 'docker[0-9]*'); do
				ip link delete "$f" 2>/dev/null || fail "deleting the link $f failed"
			done
		else
			fail "listing the network links failed: none deleted"
		fi
	else
		fail "the network links were not saved: none deleted"
	fi
	# stafd first, then stacd: stacd reads a stafd that is away as an empty
	# discovery log.
	if [ -f "$SAVED/daemons" ]; then
		while read -r u st act; do
			case $st in
			absent) continue ;;
			enabled) systemctl enable "$u" >/dev/null 2>&1 || fail "enabling $u failed" ;;
			*) systemctl disable "$u" >/dev/null 2>&1 || fail "disabling $u failed" ;;
			esac
			case $act in
			active) systemctl restart "$u" >/dev/null 2>&1 || fail "starting $u failed" ;;
			*) systemctl stop "$u" >/dev/null 2>&1 || fail "stopping $u failed" ;;
			esac
		done <"$SAVED/daemons"
	else
		fail "nvme-stas's daemons were not saved: left as they are"
	fi
	cur=$(restored)
	[ -z "$cur" ] || fail "$cur"
	return "$bad"
}

# restored prints, one line each, what is not as save kept it: a udev rule
# file, nvme-stas's files, a reading other than the saved one, a docker
# link the run made. Empty means the guest is back.
restored() {
	local f cur
	udev_diff "is not as before the run"
	case $(cat "$SAVED/stas.present" 2>/dev/null) in
	yes) diff -r -q "$SAVED/stas" /etc/stas >/dev/null 2>&1 || echo "/etc/stas is not as before the run" ;;
	no) [ ! -e /etc/stas ] || echo "/etc/stas left" ;;
	*) echo "nvme-stas's files were not saved" ;;
	esac
	for f in pkgs units forward daemons dirs group images; do
		[ -f "$SAVED/$f" ] || continue
		if ! cur=$("${f}_now" 2>/dev/null); then
			echo "reading $f back failed"
		elif [ "$cur" != "$(cat "$SAVED/$f")" ]; then
			echo "$f not as before the run: $(LC_ALL=C diff <(echo "$cur") "$SAVED/$f" | grep '^[<>]' | head -n 3 | tr '\n' ' ')"
		fi
	done
	if [ -f "$SAVED/links" ] && cur=$(links_now 2>/dev/null); then
		f=$(LC_ALL=C comm -13 "$SAVED/links" - <<<"$cur" | grep -x 'docker[0-9]*' | tr '\n' ' ')
		[ -z "$f" ] || echo "docker links left: $f"
	fi
	return 0
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

"$@"
HELPER_EOF
}

# ---------------------------------------------------------------------------
# The crash scan (tutorial_integtest.md, Purpose)
# ---------------------------------------------------------------------------

# PANIC_PAT matches the first line of the report Go's runtime writes to a
# program's stderr, which is its log in the tutorials, when the program dies
# of a panic or a fatal error. A JSON record starts with a brace, so a record
# that holds either word inside a field never matches.
PANIC_PAT='^(panic: |fatal error: )'

# PANIC_LOGS_READ is the most logs of the driver's containers one read of the
# CSI part found (panic_scan); the run fails if no read found one.
PANIC_LOGS_READ=0

# panic_lines prints, as <log>:<line>:<text>, every line of each *.log under
# <dir> on the guest that PANIC_PAT matches, and fails when grep could not
# read them all: a log that was not read is not a log without a crash. The
# lines it found are printed either way. It reads as root, as the agents'
# stores in the release folder are root's.
panic_lines() { # <dir>
	rsh "sudo -n grep -rnHaE --include='*.log' '$PANIC_PAT' $1 || [ \$? -eq 1 ]"
}

# panic_fmt prints hits, each on its own indented line after a line break,
# or nothing for none.
panic_fmt() { # <hits>
	[ -z "$1" ] || printf '\n    %s' "${1//$'\n'/$'\n'    }"
}

# panic_scan fails the run when a log under the work folder, or in the CSI
# part a log of a container of the driver's pods, its previous run's included
# (pod_crash_lines), holds a crash line, naming the log and the line; it
# logs how many container logs it read and the restart count of each
# container that restarted. run_doc calls it after each block, under the
# block's stage, since the tutorial's teardown removes the logs.
panic_scan() {
	local hits out line who count logs
	hits=$(panic_lines "$WORK") ||
		die "the crash scan could not read the logs under $WORK${hits:+; the lines it read:}$(panic_fmt "$hits")"
	if [ "$PART" = csi ]; then
		out=$(helper pod_crash_lines dnv-csi "'$PANIC_PAT'") ||
			die "the crash scan could not read the logs of the driver's containers"
		while IFS= read -r line; do
			case "$line" in
			"restarts "*)
				read -r _ who count <<<"$line"
				log "  container $who has restarted $count time(s) in its pod"
				;;
			"skipped "* | "gone "*) log "  ${line#* }: its pod is being deleted or gone, its log with it" ;;
			"crash "*) hits+=${hits:+$'\n'}${line#crash } ;;
			"read "*)
				log "  the crash scan read ${line#read }"
				read -r logs _ <<<"${line#read }"
				[ "$logs" -le "$PANIC_LOGS_READ" ] || PANIC_LOGS_READ=$logs
				;;
			esac
		done <<<"$out"
	fi
	[ -z "$hits" ] || die "a program crashed; its log holds:$(panic_fmt "$hits")"
}

# panic_dump prints, on the failure path, every crash line the logs under
# the work folder hold, and in the CSI part the driver's containers, so that
# a crash that failed a block through another check is named. It never
# fails.
panic_dump() {
	local hits out=""
	hits=$(panic_lines "$WORK" 2>/dev/null) ||
		hits+=$'\n'"(the logs under $WORK could not all be read)"
	if [ "$PART" = csi ]; then
		out=$(helper pod_crash_lines dnv-csi "'$PANIC_PAT'" 2>&1) ||
			out+=$'\n'"(the read of the driver's containers did not answer)"
		out=$(printf '%s\n' "$out" | grep -v '^restarts \|^read \|^skipped \|^gone \|^nolog ' || true)
	fi
	log "--- crash lines in the logs under $WORK and of the driver's containers"
	log "${hits:-(none in the logs under the work folder)}"
	[ -z "$out" ] || log "$out"
	return 0
}

# panic_plant proves panic_lines at preflight, on a sample log in a scratch
# directory of the guest: a record that holds both prefixes inside a field
# must not match, the two crash lines planted after it must, and nothing else
# may.
panic_plant() {
	local d hits want
	d=$(rsh "mktemp -d /var/tmp/dnv-panic-plant-tutorial.XXXXXX") ||
		die "making the crash scan's scratch directory on the guest failed"
	case "$d" in
	/var/tmp/dnv-panic-plant-tutorial.?*) ;;
	*) die "mktemp on the guest printed '$d', not a scratch directory" ;;
	esac
	printf '%s\n' \
		'{"level":"ERROR","msg":"os command failed","stderr":"panic: in a field\nfatal error: in a field"}' \
		'panic: planted by the preflight' 'goroutine 1 [running]:' \
		'fatal error: planted by the preflight' |
		rsh "mkdir $d/sub && cat > $d/sub/plant.log" || {
			rsh "rm -rf $d" || true
			die "writing the crash scan's sample log on the guest failed"
		}
	hits=$(panic_lines "$d") || hits="(the read failed)"
	rsh "rm -rf $d" || true
	want="$d/sub/plant.log:2:panic: planted by the preflight"
	want+=$'\n'"$d/sub/plant.log:4:fatal error: planted by the preflight"
	[ "$hits" = "$want" ] ||
		die "the crash scan on its planted sample log: got '$hits', want '$want'"
}

# ---------------------------------------------------------------------------
# The parse of a tutorial (tutorial_integtest.md, The markers)
# ---------------------------------------------------------------------------

# parse_doc writes the blocks of a tutorial into <dir>/<nnn>.sh, each with
# <nnn>.sec (its section heading) and, when a marker binds to it, <nnn>.mk
# (one directive per line). It fails on a marker that binds to no block, on
# an unknown directive and on a fence left open.
parse_doc() { # <doc> <dir>
	mkdir -p "$2"
	awk -v dir="$2" '
	function fail(m) { printf "%s:%d: %s\n", FILENAME, FNR, m > "/dev/stderr"; bad = 1; exit 1 }
	function add(d) {
		sub(/^[ \t]+/, "", d); sub(/[ \t]+$/, "", d)
		if (d == "") return
		w = d; sub(/[ \t].*/, "", w)
		if (w !~ /^(wait|timeout|out|line|has|lacks|word|count|same-first-word|save|same|skip|omit|replaces|run)$/)
			fail("unknown marker directive \"" w "\"")
		mk[++mkn] = d
	}
	fence { if ($0 ~ /^```[ \t]*$/) { fence = 0; close(f) } else print > f; next }
	inc {
		if (index($0, "-->")) { l = $0; sub(/-->.*/, "", l); add(l); inc = 0; mend = FNR }
		else add($0)
		next
	}
	/^<!-- test/ {
		if (mkn > 0) fail("a marker that binds to no block")
		l = $0; sub(/^<!-- test:?/, "", l)
		if (index(l, "-->")) { sub(/-->.*/, "", l); add(l); mend = FNR } else { add(l); inc = 1 }
		next
	}
	/^```/ {
		n++; f = sprintf("%s/%03d.sh", dir, n); printf "" > f
		print sec > sprintf("%s/%03d.sec", dir, n)
		if (mkn > 0) {
			if (mend != FNR - 1) fail("a marker that binds to no block")
			for (i = 1; i <= mkn; i++) print mk[i] > sprintf("%s/%03d.mk", dir, n)
			mkn = 0
		}
		fence = 1; next
	}
	/^## / { sec = substr($0, 4) }
	{ if (mkn > 0 && mend != FNR) fail("a marker that binds to no block") }
	END {
		if (bad) exit 1
		if (fence) fail("a block left open")
		if (inc) fail("a marker left open")
		if (mkn > 0) fail("a marker that binds to no block")
	}' "$1"
}

# directive prints the arguments of every directive <name> of block <b>.
directive() { # <block base> <name>
	[ -f "$1.mk" ] || return 0
	awk -v d="$2" '{ w = $0; sub(/[ \t].*/, "", w) } w == d { a = $0; sub(/^[^ \t]+[ \t]*/, "", a); print a }' "$1.mk"
}

# has_word says whether block <b> has a directive <name>, with or without
# words after it.
has_word() { # <block base> <name>
	[ -f "$1.mk" ] && awk -v d="$2" '{ w = $0; sub(/[ \t].*/, "", w) } w == d { f = 1 } END { exit !f }' "$1.mk"
}

# SAVED_WORDS holds what a save directive kept, by name.
declare -A SAVED_WORDS=()

# ---------------------------------------------------------------------------
# Running one block
# ---------------------------------------------------------------------------

# checks_hold reads one run's stdout against the block's out, line, has,
# lacks, word, count, same-first-word and same directives and prints the
# first one that fails.
checks_hold() { # <block base> <stdout file>
	local b=$1 o=$2 want t last n c
	if has_word "$b" out; then
		want=$(directive "$b" out)
		[ "$(cat "$o")" = "$want" ] || {
			echo "stdout is not the marker's out lines"
			return 1
		}
	fi
	while IFS= read -r t; do
		grep -qxF -- "$t" "$o" || {
			echo "stdout has no line \"$t\""
			return 1
		}
	done < <(directive "$b" line)
	while IFS= read -r t; do
		grep -qF -- "$t" "$o" || {
			echo "stdout has no \"$t\""
			return 1
		}
	done < <(directive "$b" has)
	while IFS= read -r t; do
		! grep -qF -- "$t" "$o" || {
			echo "stdout has \"$t\""
			return 1
		}
	done < <(directive "$b" lacks)
	while IFS= read -r t; do
		grep -qwF -- "$t" "$o" || {
			echo "stdout has no word \"$t\""
			return 1
		}
	done < <(directive "$b" word)
	while read -r n t; do
		c=$(grep -cF -- "$t" "$o" || true)
		[ "$c" = "$n" ] || {
			echo "stdout has $c lines with \"$t\", not $n"
			return 1
		}
	done < <(directive "$b" count)
	if has_word "$b" same-first-word; then
		[ "$(awk '{ print $1 }' "$o" | sort -u | wc -l)" = 1 ] && [ "$(wc -l <"$o")" -ge 2 ] || {
			echo "the lines of stdout do not start with one word"
			return 1
		}
	fi
	last=$(awk 'NF { w = $1 } END { print w }' "$o")
	while IFS= read -r t; do
		[ -n "${SAVED_WORDS[$t]:-}" ] || {
			echo "no block before saved \"$t\""
			return 1
		}
		[ "$last" = "${SAVED_WORDS[$t]}" ] || {
			echo "the last line starts with \"$last\", not with the saved $t \"${SAVED_WORDS[$t]}\""
			return 1
		}
	done < <(directive "$b" same)
	return 0
}

# block_text prints the block's text as it runs: without the lines an omit
# directive names, each a line that starts with the directive's text and
# the lines it continues to, and, in the CSI part's prelude, with the
# replacement of a replaces directive (REPL_FROM and REPL_FILE) in place of
# the command it names, the line that starts with REPL_FROM and the lines it
# continues to. Into [count file] it writes how many lines each omit took
# ("omit<TAB><n><TAB><text>") and how many the replacement took
# ("replaces<TAB><n>"). The texts reach awk through its environment, which
# it takes as they are, where -v would read their backslashes as escapes.
REPL_FROM=""
REPL_FILE=""
block_text() { # <block base> [count file]
	local b=$1
	OMITS=$(directive "$b" omit) FROM=$REPL_FROM REPL=$REPL_FILE CNT=${2:-/dev/null} awk '
	BEGIN {
		n = split(ENVIRON["OMITS"], o, "\n")
		from = ENVIRON["FROM"]; repl = ENVIRON["REPL"]; cnt = ENVIRON["CNT"]
	}
	skipping { if ($0 !~ /\\$/) skipping = 0; next }
	{
		for (i = 1; i <= n; i++) if (o[i] != "" && index($0, o[i]) == 1) {
			hit[i]++
			if ($0 ~ /\\$/) skipping = 1
			next
		}
		if (from != "" && index($0, from) == 1) {
			repls++
			while ((getline l < repl) > 0) print l
			close(repl)
			if ($0 ~ /\\$/) skipping = 1
			next
		}
		print
	}
	END {
		for (i = 1; i <= n; i++) if (o[i] != "") printf "omit\t%d\t%s\n", hit[i] + 0, o[i] > cnt
		printf "replaces\t%d\n", repls + 0 > cnt
	}' "$b.sh"
}

# block_plan checks one block the run will run, before any block runs:
# each omit of its marker takes exactly one line, and the text it would run
# has no line k3s_hit prints. It leaves in BLOCK_REPL how many lines the
# replacement took.
BLOCK_REPL=0
block_plan() { # <block base> <label>
	local b=$1 label=$2 cnt text k n o hit
	cnt=$(mktemp)
	text=$(block_text "$b" "$cnt")
	BLOCK_REPL=$(awk -F'\t' '$1 == "replaces" { print $2 }' "$cnt")
	while IFS=$'\t' read -r k n o; do
		[ "$k" = omit ] || continue
		[ "$n" = 1 ] || {
			rm -f "$cnt"
			die "[$label] the omit \"$o\" takes $n lines of the block, not one"
		}
	done <"$cnt"
	rm -f "$cnt"
	hit=$(k3s_hit "$text")
	[ -z "$hit" ] ||
		die "[$label] the block would run \"$hit\", a use of k3s other than its kubectl and ctr, which the suite never runs"
}

run_block() { # <block base> <label>
	local b=$1 label=$2 wait t0 rc why first text b64 out err tmo t
	text=$(block_text "$b")
	# The plan checked every block before the first ran; this is the same
	# check on the text that runs now.
	[ -z "$(k3s_hit "$text")" ] ||
		die "[$label] the block names k3s other than in its kubectl and ctr, which the suite never runs"
	first=$(head -n 1 <<<"$text")
	stage "$label: ${first:0:100}"
	sed 's/^/  | /' <<<"$text" >&2
	wait=$(directive "$b" wait | tail -n 1)
	tmo=$(directive "$b" timeout | tail -n 1)
	tmo=${tmo:-$BLOCK_TIMEOUT}
	b64=$(base64 -w0 <<<"$text")
	out=$(mktemp)
	err=$(mktemp)
	t0=$SECONDS
	while :; do
		rc=0
		rsh "cd $(printf '%q' "$DIR") && printf %s $b64 | base64 -d > .tutorial-block &&
			timeout -k 10 $tmo bash -e .tutorial-block < /dev/null" >"$out" 2>"$err" || rc=$?
		why=""
		if [ "$rc" -ne 0 ]; then
			why="exit $rc"
		else
			why=$(checks_hold "$b" "$out") || true
		fi
		[ -n "$why" ] || break
		if [ -z "$wait" ] || [ $((SECONDS - t0)) -ge "$wait" ]; then
			sed 's/^/  > /' "$out" >&2
			sed 's/^/  ! /' "$err" >&2
			rm -f "$out" "$err"
			die "[$label] $why${wait:+ after ${wait}s of runs}"
		fi
		sleep 1
	done
	sed 's/^/  > /' "$out" >&2
	sed 's/^/  ! /' "$err" >&2
	while IFS= read -r t; do
		SAVED_WORDS[$t]=$(awk 'NF { w = $1 } END { print w }' "$out")
	done < <(directive "$b" save)
	log "  ok in $((SECONDS - t0))s"
	rm -f "$out" "$err"
}

# run_doc walks the blocks of a parsed tutorial in order, from its first
# block through the last one of the section [last section] when one is
# named, which must exist: in mode plan it checks each block it would run
# (block_plan), and every block of the CSI tutorial for a marker; in mode
# run it runs them. A block with a skip directive is not run, and one with
# a replaces directive not in its own place (it ran inside the prelude).
PLAN_REPL=0
run_doc() { # <parse dir> <name> <plan|run> [last section]
	local pd=$1 name=$2 mode=$3 last=${4:-} b sec past=0
	for b in "$pd"/*.sh; do
		b=${b%.sh}
		sec=$(cat "$b.sec")
		if [ -n "$last" ]; then
			[ "$past" = 1 ] && [ "$sec" != "$last" ] && break
			[ "$sec" = "$last" ] && past=1
		fi
		if [ "$mode" = plan ] && [ "$name" = tutorial_csi.md ] && [ ! -f "$b.mk" ]; then
			die "[$name, $sec] a block with no marker: a block of the CSI tutorial runs only when its marker lets it ($(head -n 1 "$b.sh"))"
		fi
		if has_word "$b" replaces; then
			[ "$mode" = plan ] ||
				log "--- [$name, $sec] ran in the prelude in place of the line that starts with \"$(directive "$b" replaces)\""
			continue
		fi
		if has_word "$b" skip; then
			[ "$mode" = plan ] || log "--- [$name, $sec] skipped: $(directive "$b" skip)"
			continue
		fi
		if [ "$mode" = plan ]; then
			block_plan "$b" "$name, $sec"
			PLAN_REPL=$((PLAN_REPL + BLOCK_REPL))
		else
			run_block "$b" "$name, $sec"
			panic_scan
		fi
	done
	[ -z "$last" ] || [ "$past" = 1 ] || die "$name has no section \"$last\""
}

# csi_repl sets REPL_FROM and REPL_FILE from the CSI tutorial's block with a
# replaces directive.
csi_repl() {
	local b
	REPL_FROM="" REPL_FILE=""
	for b in "$PARSE"/csi/*.sh; do
		b=${b%.sh}
		REPL_FROM=$(directive "$b" replaces)
		[ -n "$REPL_FROM" ] && REPL_FILE=$b.sh && break
	done
	[ -n "$REPL_FROM" ] || die "tutorial_csi.md has no block with a replaces directive"
}

# plan checks every block the part will run before any runs (run_doc,
# mode plan): in the CSI part the replacement takes exactly one line of the
# prelude.
plan() {
	local t b
	stage "preflight: the plan of the run"
	if [ "$PART" = all-in-one ]; then
		run_doc "$PARSE/aio" tutorial_all_in_one.md plan
	else
		t=0
		for b in "$PARSE"/csi/*.sh; do
			has_word "${b%.sh}" replaces && t=$((t + 1))
		done
		[ "$t" = 1 ] || die "tutorial_csi.md has $t blocks with a replaces directive, not one"
		csi_repl
		PLAN_REPL=0
		run_doc "$PARSE/aio" tutorial_all_in_one.md plan "Cluster and nodes"
		[ "$PLAN_REPL" = 1 ] ||
			die "the CSI tutorial's replaces \"$REPL_FROM\" takes $PLAN_REPL lines of the prelude, not one"
		REPL_FROM="" REPL_FILE=""
		run_doc "$PARSE/csi" tutorial_csi.md plan
	fi
	log "  every block the run will run passed the plan"
}

# ---------------------------------------------------------------------------
# Preflight, setup, cleanup
# ---------------------------------------------------------------------------

kc() { rsh "sudo -n k3s kubectl $*"; }

preflight() {
	local t pkg line st
	stage "preflight: the driver"
	for t in ssh scp awk base64 tar git make go sha256sum mktemp; do
		command -v "$t" >/dev/null || die "the driver has no $t"
	done
	PARSE=$(mktemp -d)
	parse_doc "$AIO_DOC" "$PARSE/aio" || die "the markers of tutorial_all_in_one.md do not parse"
	parse_doc "$CSI_DOC" "$PARSE/csi" || die "the markers of tutorial_csi.md do not parse"
	log "  tutorial_all_in_one.md: $(ls "$PARSE"/aio/*.sh | wc -l) blocks; tutorial_csi.md: $(ls "$PARSE"/csi/*.sh | wc -l) blocks"
	plan

	stage "preflight: the guest $TARGET holds no dnv state"
	rsh "sudo -n true" || die "no passwordless sudo on $TARGET"
	# ssh exits 255 when it fails, test 1 for an absent file.
	t=0
	rsh "test -e $SAVED" || t=$?
	case $t in
	0) die "$TARGET holds what an earlier run saved under $SAVED: put the guest back with --cleanup-only" ;;
	1) ;;
	*) die "reading $SAVED on the guest failed (exit $t)" ;;
	esac
	rsh "mkdir -p $WORK && cat > $HELPER" < <(helper_text) || die "shipping the helper failed"
	panic_plant
	line=$(helper state) || die "reading the guest's state failed"
	[ -z "$line" ] || die "the guest holds dnv state: $(echo $line)"
	line=$(rsh "modinfo -n nvmet") || die "modinfo -n nvmet failed"
	case $line in */updates/*) die "the guest runs a rebuilt nvmet ($line); the tutorial is for a stock kernel" ;; esac
	local ports=("${PORTS[@]}")
	[ "$PART" != csi ] || ports+=("${CSI_PORTS[@]}")
	for t in "${ports[@]}"; do
		line=$(rsh "ss -Hltn 'sport = :$t'") || die "reading the listeners of port $t failed"
		[ -z "$line" ] || die "port $t is taken on the guest"
	done
	local pkgs=("${AIO_PKGS[@]}")
	[ "$PART" = csi ] && pkgs+=("${CSI_PKGS[@]}")
	local inst cand
	for pkg in "${pkgs[@]}"; do
		# dpkg-query exits 1 for a package it does not know and 2 on an
		# error; each read checks its own command's status.
		inst=$(rsh "dpkg-query -W -f '\${db:Status-Abbrev} \${Version}\n' $pkg 2>/dev/null; [ \$? -le 1 ]") ||
			die "reading the package $pkg failed"
		inst=$(awk '$1 == "ii" { print $NF }' <<<"$inst")
		cand=$(rsh "apt-cache policy $pkg") || die "reading the candidate of $pkg failed"
		cand=$(awk '/Candidate:/ { print $2 }' <<<"$cand")
		if [ -n "$inst" ] && [ "$inst" != "$cand" ]; then
			# The k3s server node runs no node tool for any other suite, so
			# the CSI part lets the tutorial's install upgrade one there.
			[ "$PART" = csi ] ||
				die "the guest's $pkg is $inst, its candidate $cand: the tutorial would upgrade it"
			log "  $pkg: $inst, which the tutorial's install upgrades to $cand"
			continue
		fi
		log "  $pkg: ${inst:-not installed}, candidate $cand"
	done
	if [ "$PART" = csi ]; then
		stage "preflight: the lab's k3s holds no CSI driver"
		kc get nodes >/dev/null || die "the guest is not the k3s server (sudo k3s kubectl get nodes failed)"
		line=$(csi_left) || die "reading the lab's k3s failed"
		[ -z "$line" ] || die "the lab's k3s holds what the tutorial makes or a run leaves: $(echo $line)"
	fi
}

setup() {
	local v ar etcd_dir
	if [ -n "$ARCHIVE" ]; then
		ar=$ARCHIVE
		stage "setup: the archive $ar"
	else
		# The archive goes into a folder of the run's own on the driver,
		# never into the tree's _out/, whose checksum file may be a tagged
		# release's.
		stage "setup: make release"
		RELEASE_OUT=$(mktemp -d)
		if v=$(git -C "$REPO_ROOT" describe --tags --exact-match 2>/dev/null) &&
			[ -z "$(git -C "$REPO_ROOT" status --porcelain)" ]; then
			make -C "$REPO_ROOT" release RELEASE_OUT="$RELEASE_OUT" >&2 || die "make release failed"
		else
			v=test-$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD)
			make -C "$REPO_ROOT" release VERSION="$v" RELEASE_OUT="$RELEASE_OUT" >&2 ||
				die "make release VERSION=$v failed"
		fi
		ar=$RELEASE_OUT/dnv-$v-linux-amd64.tar.gz
	fi
	[ -f "$ar" ] || die "no archive $ar"
	# awk reads the whole listing, so tar never meets a closed pipe.
	DIR=$WORK/$(tar -tzf "$ar" | awk -F/ 'NR == 1 { print $1 }')
	# shellcheck source=fetch_etcd.sh
	. "$REPO_ROOT/integtest/fetch_etcd.sh"
	fetch_etcd >/dev/null
	etcd_dir=$ETCD_CACHE_DIR/$ETCD_DIST

	stage "setup: save the guest's state, unpack the archive, put etcd beside the binaries"
	SAVE_DONE=1
	# The logs an earlier run kept under $WORK/logs for the runner to copy go
	# before this run's first block, whose crash scan reads every log under
	# $WORK, can read them as this run's.
	rsh "sudo -n rm -rf $WORK/logs" ||
		die "removing the logs an earlier run kept under $WORK/logs failed"
	helper save || die "saving the guest's state failed"
	rsh "echo $(printf '%q' "$DIR") | sudo -n tee $SAVED/dir >/dev/null" || die "recording the folder failed"
	scp -q "${SSH_OPTS[@]}" "$ar" "$TARGET:$WORK/" || die "copying the archive failed"
	scp -q "${SSH_OPTS[@]}" "$etcd_dir/etcd" "$TARGET:$WORK/etcd" || die "copying etcd failed"
	rsh "cd $WORK && tar xzf $(basename "$ar") && mv etcd $(printf '%q' "$DIR")/" || die "unpacking failed"
	[ "$PART" != csi ] || csi_objects_check "$ar"
	SETUP_DONE=1
}

# cleanup puts the guest back: it takes down what the run left, restores
# what setup saved, keeps the logs of a failed run under $WORK/logs and
# removes the rest of $WORK, but for $SAVED when the restore failed or did
# not run for a read that failed, so that a run with --cleanup-only can
# still put the guest back (the next run's preflight refuses until it has).
# In the CSI part it first deletes from the lab's k3s the objects of
# CSI_OBJECTS by kind and name, which needs no release folder, and puts the
# taints back (csi_cleanup). It fails when any
# step of it failed or anything is left, on the guest or in the lab's k3s.
cleanup() { # <keep logs: 0|1>
	local keep=$1 left n t bad=0 keep_saved=""
	stage "cleanup: put the guest back"
	if [ "$PART" = csi ]; then
		csi_cleanup || bad=1
	fi
	left=$(helper wipe) || left="the wipe failed"
	[ -z "$left" ] || log "WARNING: left after the wipe: $(echo $left)"
	# Whether the save left anything, and then whether it is complete: ssh
	# exits 255 when it fails, test 1 for an absent file or folder, and
	# $SAVED is readable by the plain user. A save that did not complete ran
	# no block and changed nothing, so its records go with $WORK.
	t=0
	rsh "test -d $SAVED" || t=$?
	if [ "$t" = 0 ]; then
		rsh "test -f $SAVED/complete" || t=$?
		[ "$t" != 1 ] ||
			log "  the save did not complete: nothing to put back, its records go with $WORK"
		[ "$t" != 1 ] || t=nothing
	fi
	case $t in
	0)
		helper restore >&2 || {
			log "WARNING: restoring the guest's state failed (the steps above)"
			bad=1
			keep_saved="! -name saved"
		}
		;;
	1 | nothing) ;;
	*)
		log "WARNING: reading what the save kept failed (exit $t): nothing restored"
		bad=1
		keep_saved="! -name saved"
		;;
	esac
	if [ "$keep" = 1 ]; then
		rsh "cd $WORK && sudo -n mkdir -p logs && sudo -n find . -path ./logs -prune -o -name '*.log' -type f -exec cp {} logs/ \\; ; sudo -n find . -mindepth 1 -maxdepth 1 ! -name logs $keep_saved -exec rm -rf {} +" || {
			log "WARNING: keeping the logs failed"
			bad=1
		}
		log "the run's logs are kept on $TARGET under $WORK/logs"
	elif [ -n "$keep_saved" ]; then
		rsh "sudo -n find $WORK -mindepth 1 -maxdepth 1 $keep_saved -exec rm -rf {} +" || {
			log "WARNING: removing $WORK but its saved state failed"
			bad=1
		}
	else
		rsh "sudo -n rm -rf $WORK" || {
			log "WARNING: removing $WORK failed"
			bad=1
		}
	fi
	[ -z "$keep_saved" ] ||
		log "the saved state stays on $TARGET under $SAVED: run --cleanup-only to put the guest back"
	[ -n "$RELEASE_OUT" ] && rm -rf "$RELEASE_OUT"
	[ -z "$left" ] || return 1
	n=$(rsh "ls /sys/kernel/config/nvmet/ports 2>/dev/null | wc -l") || n=unknown
	[ "$n" = 0 ] || {
		log "WARNING: nvmet ports left: $n"
		return 1
	}
	return "$bad"
}

# csi_left prints what the tutorial makes or a run leaves in the lab's k3s,
# one line each: an object of CSI_OBJECTS, a volume or an attachment of the
# driver's, a node with the suite's taint, the server without its own, and
# the driver's image in the server's store. Empty means none.
csi_left() {
	local e src kind ns name out me
	for e in "${CSI_OBJECTS[@]}"; do
		read -r src kind ns name <<<"$e"
		out=$(kc $(csi_ns "$ns") get "${kind,,}" "$name" --ignore-not-found -o name 2>/dev/null) ||
			return 1
		[ -z "$out" ] || echo "${kind,,} $name"
	done
	out=$(kc get pv -o "'jsonpath={range .items[*]}{.spec.csi.driver} {.metadata.name}{\"\\n\"}{end}'") || return 1
	awk '$1 == "csi.dnv.io" { print "volume " $2 }' <<<"$out"
	out=$(kc get volumeattachment -o "'jsonpath={range .items[*]}{.spec.attacher} {.metadata.name}{\"\\n\"}{end}'") || return 1
	awk '$1 == "csi.dnv.io" { print "attachment " $2 }' <<<"$out"
	out=$(kc get nodes -o "'jsonpath={range .items[*]}{.metadata.name} {.spec.taints[*].key}{\"\\n\"}{end}'") || return 1
	me=$(rsh hostname) || return 1
	awk -v k="$TUT_TAINT_KEY" -v me="$me" -v cp="${CP_TAINT%%:*}" '
		{ for (i = 2; i <= NF; i++) if ($i == k) print "the suite'"'"'s taint on " $1 }
		$1 == me { for (i = 2; i <= NF; i++) if ($i == cp) has = 1 }
		END { if (!has) print "the server " me " without its own taint" }' <<<"$out"
	out=$(rsh "sudo -n k3s ctr images ls -q") || return 1
	grep -x docker.io/library/dnv-csi:0.1.0 <<<"$out" || true
}

# csi_gone answers whether no volume and no attachment of the driver's is
# left in the lab's k3s.
csi_gone() {
	local out
	out=$(kc get pv,volumeattachment -o "'jsonpath={range .items[*]}{.spec.csi.driver}{.spec.attacher}{\"\\n\"}{end}'") ||
		return 1
	! grep -qx csi.dnv.io <<<"$out"
}

# csi_ns prints kubectl's namespace option for a CSI_OBJECTS namespace field.
csi_ns() { [ "$1" = - ] || printf '%s' "-n $1"; }

# csi_delete deletes, by kind and name and in the list's order, the
# CSI_OBJECTS entries of kind Pod or PersistentVolumeClaim, the list's two
# pods, demo and probe in default, and the demo pod's claim (first), or every
# other entry (rest), and fails when a delete fails.
csi_delete() { # <first|rest>
	local e src kind ns name bad=0
	for e in "${CSI_OBJECTS[@]}"; do
		read -r src kind ns name <<<"$e"
		case $kind in
		Pod | PersistentVolumeClaim) [ "$1" = first ] || continue ;;
		*) [ "$1" = rest ] || continue ;;
		esac
		kc $(csi_ns "$ns") delete "${kind,,}" "$name" --ignore-not-found --wait=true \
			--timeout=120s >/dev/null || bad=1
	done
	return "$bad"
}

# csi_manifest_objects prints, for each object of each manifest under
# <deploy dir>, "<file> <kind> <namespace, - for none> <name>", read from
# the manifest's top-level kind and its metadata's name and namespace.
csi_manifest_objects() { # <deploy dir>
	local f
	for f in "$1"/*.yaml; do
		awk -v f="${f##*/}" '
			function flush() { if (kind != "") print f, kind, (ns == "" ? "-" : ns), name; kind = ns = name = ""; meta = 0 }
			/^---/ { flush(); next }
			/^kind:/ { kind = $2 }
			/^metadata:/ { meta = 1; next }
			/^[^ #]/ { meta = 0 }
			meta && /^  name:/ { name = $2 }
			meta && /^  namespace:/ { ns = $2 }
			END { flush() }' "$f" | tr -d "\"'" || return 1
	done
}

# csi_objects_check fails, before any block, unless the manifests of the
# release's deploy/ hold exactly the objects CSI_OBJECTS names from them: an
# object the list lacks would be applied and never deleted. A manifest that
# names no namespace matches the list's, as the tutorial picks it.
csi_objects_check() { # <archive>
	local tmp got bad
	tmp=$(mktemp -d) || die "making a folder for the release's manifests failed"
	tar -xzf "$1" -C "$tmp" --wildcards '*/deploy/*.yaml' ||
		die "unpacking the release's manifests failed"
	got=$(csi_manifest_objects "$tmp"/*/deploy) ||
		die "reading the kinds and names of the release's manifests failed"
	rm -rf "$tmp"
	bad=$(awk 'FNR == NR { if ($1 != "inline") w[$1 " " $2 " " $4] = $3; next }
		{ k = $1 " " $2 " " $4
		  if (!(k in w)) { print "an object of no CSI_OBJECTS entry: " $0; next }
		  if ($3 != "-" && $3 != w[k]) print "namespace " $3 " against " w[k] " in the list: " k
		  seen[k] = 1 }
		END { for (k in w) if (!(k in seen)) print "a CSI_OBJECTS entry no manifest holds: " k }' \
		<(printf '%s\n' "${CSI_OBJECTS[@]}") <(printf '%s\n' "$got"))
	[ -z "$bad" ] || die "the release's manifests and the cleanup's list differ: $(echo $bad)"
	log "  the release's manifests hold the objects of CSI_OBJECTS, and no other"
}

# csi_cleanup deletes the tutorial's objects from the lab's k3s by kind and
# name (CSI_OBJECTS), never through the release's manifests, the pods and the
# claim first, waiting for the driver's volumes and attachments to go before
# the driver does, then the image it imported into the server's store and
# the tutorial's taints, and puts the server's own taint back; then it reads
# the lab's k3s back (csi_left). It fails when a step failed or anything is
# left.
csi_cleanup() {
	local n i bad=0 left
	kc get nodes >/dev/null 2>&1 || {
		log "WARNING: the lab's k3s does not answer"
		return 1
	}
	csi_delete first || bad=1
	# A claim's delete returns before its volume and attachment are gone,
	# and without the driver a volume or an attachment keeps its finalizer.
	for i in $(seq 180); do
		csi_gone && break
		sleep 1
	done
	csi_gone || {
		log "WARNING: a volume or an attachment of the driver is still there after 180 s"
		bad=1
	}
	csi_delete rest || bad=1
	rsh "sudo -n k3s ctr images rm docker.io/library/dnv-csi:0.1.0 >/dev/null 2>&1; true"
	# A node without the taint answers the removal with an error, so the
	# read back below is the verdict.
	for n in $(kc get nodes -o name | sed 's|node/||'); do
		kc taint node "$n" "$TUT_TAINT_KEY-" >/dev/null 2>&1 || true
	done
	kc taint node "$(rsh hostname)" "$CP_TAINT" --overwrite >/dev/null || bad=1
	left=$(csi_left) || left="the read of the lab's k3s failed"
	[ -z "$left" ] || {
		log "WARNING: left in the lab's k3s: $(echo $left)"
		bad=1
	}
	return "$bad"
}

# csi_prepare makes the lab's k3s one machine for the tutorial: the other
# nodes take the suite's taint, which the tutorial's pods do not tolerate,
# and the server's own taint is lifted.
csi_prepare() {
	local n me
	stage "setup: the lab's k3s schedules on the server alone"
	me=$(rsh hostname)
	for n in $(kc get nodes -o name | sed 's|node/||'); do
		[ "$n" = "$me" ] && continue
		kc taint node "$n" "$TUT_TAINT_KEY=run:NoSchedule" --overwrite >/dev/null || die "tainting $n failed"
	done
	kc taint node "$me" "${CP_TAINT%%:*}-" >/dev/null 2>&1 || true
}

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
	# with, and the run exits with it but on a SIGTERM and after a failed end
	# cleanup, as one whose step a Ctrl-C ended, which exits 1 with LAB NOT
	# CLEAN.
	trap - TERM
	trap : INT
	trap '' HUP
	trap '' PIPE
	printf '\n' >&2 2>/dev/null || exec >/dev/null 2>&1
	if [ "$CLEANUP_ONLY" = 1 ]; then
		[ "$rc" = 0 ] && log "" && log "PASS"
		exit "$rc"
	fi
	if [ "$rc" = 0 ]; then
		cleanup 0 || {
			log "LAB NOT CLEAN"
			exit 1
		}
		log ""
		log "PASS"
	else
		local failed=$STAGE
		log ""
		log "########## diagnostics (tutorial_integtest.md, Cleanup) ##########"
		if [ "$SETUP_DONE" = 1 ]; then
			panic_dump
			rsh "cd $(printf '%q' "$DIR") 2>/dev/null && for f in *.log; do echo \"--- \$f\"; tail -n 30 \"\$f\"; done" >&2 || true
		fi
		# A run that failed before it saved the guest's state has changed
		# nothing on the guest but its copy of the helper under $WORK, and
		# its cleanup would put back nothing.
		if [ "$SAVE_DONE" = 1 ]; then
			cleanup 1 || log "LAB NOT CLEAN"
			log ""
			log "the guest is put back; failing stage '$failed'"
		else
			log ""
			log "the guest was not changed; failing stage '$failed'"
		fi
	fi
	[ -n "$PARSE" ] && rm -rf "$PARSE"
	[ -n "$RELEASE_OUT" ] && rm -rf "$RELEASE_OUT"
	exit "$rc"
}

# teardown_read reads the guest after the tutorial's own teardown and before
# the suite's cleanup touches it (the helper's after): anything the
# teardown left that the tutorial says it removes fails the run.
teardown_read() {
	local left
	stage "the guest after the tutorial's teardown"
	left=$(helper after "$PART") || die "reading the guest after the teardown failed"
	[ -z "$left" ] || die "the tutorial's teardown left: $(echo $left)"
	log "  nothing left that the tutorial's teardown removes"
}

trap on_exit EXIT
# A run killed by SIGTERM, SIGINT or SIGHUP exits with the shell's code for
# the signal, so on_exit reads it as the failure it is, never as a PASS. The
# HUP trap ignores every later SIGHUP first: a terminal that closes sends a
# second one right after the first.
trap 'exit 143' TERM
trap 'exit 130' INT
trap 'trap "" HUP; exit 129' HUP

if [ "$CLEANUP_ONLY" = 1 ]; then
	stage "cleanup-only"
	rsh "mkdir -p $WORK && cat > $HELPER" < <(helper_text) || die "shipping the helper failed"
	DIR=$(rsh "cat $SAVED/dir 2>/dev/null") || DIR=""
	cleanup 1 || die "the guest is not clean"
	exit 0
fi

preflight
setup
if [ "$PART" = all-in-one ]; then
	run_doc "$PARSE/aio" tutorial_all_in_one.md run
else
	csi_prepare
	# The prelude: the all-in-one tutorial through "Cluster and nodes", the
	# gateway started by the CSI tutorial's command instead.
	csi_repl
	run_doc "$PARSE/aio" tutorial_all_in_one.md run "Cluster and nodes"
	REPL_FROM="" REPL_FILE=""
	run_doc "$PARSE/csi" tutorial_csi.md run
	# The driver ran in the CSI part, so a read that never found one of
	# its logs read nothing (panic_scan).
	[ "$PANIC_LOGS_READ" -ge 1 ] || {
		STAGE="the crash scan of the CSI part"
		die "the crash scan read no log of the driver's containers in the whole run"
	}
fi
teardown_read
