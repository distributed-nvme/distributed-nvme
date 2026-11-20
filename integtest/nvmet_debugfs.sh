#!/bin/bash
# nvmet_debugfs.sh — a lab tool, not a way for users: builds the kernel's six
# nvmet modules with CONFIG_NVME_TARGET_DEBUGFS, the kernel option that shows
# each host's connection to nvmet under /sys/kernel/debug/nvmet and lets a
# controller node end it (the node check's host cut row, dnagent.md CM5),
# installs them on lab controller nodes, checks them and takes them away
# again.
#
# Usage:
#   bash integtest/nvmet_debugfs.sh install <build user@host> [<user@host>...]
#   bash integtest/nvmet_debugfs.sh check <user@host>...
#   bash integtest/nvmet_debugfs.sh restore <user@host>...
#
# install builds on the first target, which needs the running kernel's
# headers and its compiler, and installs on it and on every other target, in
# order, stopping at the first target that refuses. Every target must run
# the same kernel version. The build downloads the source package of the
# running kernel (apt-get download, no root), keeps only drivers/nvme/target
# and drivers/nvme/host (the fc, loop and passthru code include host
# headers), builds the modules once as the kernel has them and refuses when
# an exported symbol's checksum differs from the kernel's Module.symvers,
# then builds them with the option set for the build file and for the C
# code, and refuses unless that build made debugfs.o and changed an exported
# checksum. The option changes a structure all six modules share, so all six
# are built and installed together, and a stock transport module must never
# load beside the built nvmet. They go under /lib/modules/<version>/updates/,
# which the module tools prefer; the stock modules stay on the disk. A
# target is refused, before its modules or packages change (its copy of the
# built files is already in place), when its nvmet holds a subsystem, a port
# or a host entry (a suite's cleanup removes them; a stopped agent leaves its
# own), when a module of the six is in use, or when its lockdown file shows
# a lockdown or its kernel enforces module signatures, since either refuses
# the built modules. install then loads nvmet and nvmet-tcp, holds the
# kernel's meta packages that were not held yet (every new kernel needs a
# new build; a kernel with no generic or virtual meta package gets a
# warning) and ends with the check.
#
# check exits 0 when every target's nvmet, loaded by modprobe, shows the
# debug directory, debugfs is mounted at /sys/kernel/debug and the lockdown
# file shows no lockdown; it prints one line per target. restore does nothing on
# a target with neither built modules for any kernel nor a hold list;
# elsewhere it removes the built modules of every kernel version and lifts
# the holds install recorded, and where the running kernel has them, or its
# nvmet shows the debug directory, it first refuses a busy target as install
# does and then swaps in the stock nvmet and nvmet-tcp. restore, like
# install, stops at the first target that refuses.
#
# Each target needs passwordless sudo. Nothing else may use the target's
# nvmet while install or restore runs.
set -euo pipefail

MODE=${1:-}
shift || true
[ $# -ge 1 ] || {
	echo "usage: nvmet_debugfs.sh install|check|restore user@host..." >&2
	exit 2
}

# ssh joins its arguments into one shell line, so each remote argument is
# quoted with printf %q.
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=10)
SCP=(scp -q -o BatchMode=yes -o ConnectTimeout=10)
REMOTE_DIR=/var/tmp/dnv-nvmet-debugfs
MODS="nvmet nvmet-tcp nvmet-rdma nvmet-fc nvme-loop nvmet-pci-epf"

# remote_admin <target> <mode> <version>: the part each target runs as root,
# install, check or restore; the version is the one the modules are built
# for (empty for check and restore). The remote script also gets the work
# directory, whose ko/ holds the built modules, and the module list.
remote_admin() {
	"${SSH[@]}" "$1" "sudo -n bash -s -- $(printf '%q ' "$2" "$3" "$REMOTE_DIR" "$MODS")" <<'REMOTE'
set -euo pipefail
mode=$1 want=$2 w=$3 mods=$4
kver=$(uname -r)
name=dnv-nvmet-debugfs
upd=/lib/modules/$kver/updates/$name
held=/var/lib/$name/held
cfs=/sys/kernel/config/nvmet
die() { echo "$(hostname) $kver: $*" >&2; exit 1; }

# The six modules by their kernel names, transports first.
kmods="nvme_loop nvmet_pci_epf nvmet_fc nvmet_rdma nvmet_tcp nvmet"

loaded() { grep -q "^$1 " /proc/modules; }
refcnt() { cat "/sys/module/$1/refcnt" 2>/dev/null || echo 0; }
holders() { ls "/sys/module/$1/holders" 2>/dev/null | tr '\n' ' '; }

# refuse_busy refuses, before anything changes, a target whose nvmet holds
# an object or whose modules something else uses: each configfs entry pins
# nvmet, an enabled port pins its transport, and nvmet's own count holds one
# for each loaded transport.
refuse_busy() {
	local d m n=0
	if [ -d "$cfs" ]; then
		for d in subsystems ports hosts; do
			[ -z "$(ls -A "$cfs/$d" 2>/dev/null)" ] ||
				die "nvmet holds $d entries: remove nvmet's subsystems, ports and hosts first (a suite's cleanup does; a stopped agent leaves its own)"
		done
	fi
	for m in $kmods; do
		[ "$m" = nvmet ] && continue
		loaded "$m" || continue
		n=$((n + 1))
		[ "$(refcnt "$m")" = 0 ] || die "$m is in use (holders: $(holders "$m"))"
	done
	if loaded nvmet && [ "$(refcnt nvmet)" != "$n" ]; then
		die "nvmet is in use beyond its $n transports (holders: $(holders nvmet))"
	fi
	return 0
}

refuse_unsigned() {
	local ld sig
	ld=$(lockdown_state)
	[ "$ld" = none ] || die "the kernel is under lockdown ($ld), which refuses unsigned modules"
	sig=$(cat /sys/module/module/parameters/sig_enforce 2>/dev/null || echo N)
	[ "$sig" != Y ] || die "the kernel enforces module signatures"
}

unload_all() {
	local m
	for m in $kmods; do
		if loaded "$m"; then
			rmmod "$m" || die "cannot unload $m"
		fi
	done
}

# A missing lockdown file is no lockdown, as the node check reads it.
lockdown_state() {
	local f=/sys/kernel/security/lockdown
	[ -r "$f" ] || { echo none; return; }
	sed -n 's/.*\[\([a-z]*\)\].*/\1/p' "$f"
}

check_one() {
	modprobe nvmet || die "modprobe nvmet failed"
	local file ld
	file=$(modinfo -n nvmet)
	awk '$2 == "/sys/kernel/debug" && $3 == "debugfs" {f = 1} END {exit !f}' /proc/mounts ||
		die "debugfs is not mounted at /sys/kernel/debug"
	[ -d /sys/kernel/debug/nvmet ] ||
		die "nvmet ($file) has no /sys/kernel/debug/nvmet: built without CONFIG_NVME_TARGET_DEBUGFS"
	ld=$(lockdown_state)
	[ "$ld" = none ] || die "the kernel is under lockdown ($ld): the cut cannot open the state file"
	echo "$(hostname) $kver: ok, nvmet $file shows /sys/kernel/debug/nvmet"
}

meta_pkgs() {
	{ dpkg-query -W -f='${Package} ${Status}\n' 'linux-generic*' 'linux-image-generic*' \
		'linux-headers-generic*' 'linux-virtual*' 'linux-image-virtual*' 2>/dev/null || true; } |
		awk '$NF == "installed" {print $1}'
}

case $mode in
install)
	[ "$kver" = "$want" ] || die "runs $kver, the modules are built for $want"
	for m in $mods; do
		[ -f "$w/ko/$m.ko" ] || die "missing built $m.ko"
	done
	refuse_unsigned
	refuse_busy
	unload_all
	rm -rf "$upd"
	install -d "$upd"
	for m in $mods; do
		install -m 0644 "$w/ko/$m.ko" "$upd/"
	done
	depmod -a "$kver"
	for m in $mods; do
		f=$(modinfo -n "$m")
		[ "$f" = "$upd/$m.ko" ] || die "modinfo finds $f for $m, not the built module"
	done
	modprobe nvmet
	modprobe nvmet-tcp
	install -d "$(dirname "$held")"
	touch "$held"
	new=""
	holds=$(apt-mark showhold)
	for p in $(meta_pkgs); do
		grep -qxF "$p" <<<"$holds" && continue
		new="$new $p"
		echo "$p" >>"$held"
	done
	[ -z "$new" ] || apt-mark hold $new >/dev/null
	[ -n "$(meta_pkgs)" ] ||
		echo "$(hostname) $kver: warning: no generic or virtual kernel meta package to hold; hold this kernel's own by hand" >&2
	echo "$(hostname) $kver: installed under $upd; held:${new:- nothing new}"
	check_one
	;;
check)
	check_one
	;;
restore)
	dirs=$(ls -d /lib/modules/*/updates/"$name" 2>/dev/null || true)
	if [ -z "$dirs" ] && [ ! -f "$held" ]; then
		echo "$(hostname) $kver: nothing to restore"
		exit 0
	fi
	swap=no
	if [ -d "$upd" ] || [ -d /sys/kernel/debug/nvmet ]; then
		swap=yes
		refuse_busy
		unload_all
	fi
	for d in $dirs; do
		rm -rf "$d"
		v=${d#/lib/modules/}
		v=${v%%/*}
		if [ -f "/lib/modules/$v/modules.order" ]; then
			depmod -a "$v"
		else
			rmdir "/lib/modules/$v/updates" 2>/dev/null || true
		fi
	done
	if [ "$swap" = yes ]; then
		modprobe nvmet
		modprobe nvmet-tcp
	fi
	if [ -f "$held" ]; then
		pkgs=$(sort -u "$held" | tr '\n' ' ')
		[ -z "${pkgs// /}" ] || apt-mark unhold $pkgs >/dev/null
		rm -f "$held"
	fi
	[ -d /sys/kernel/debug/nvmet ] && die "the debug directory is still there after the restore"
	if [ "$swap" = yes ]; then
		echo "$(hostname) $kver: stock nvmet back ($(modinfo -n nvmet)); unheld: ${pkgs:-nothing}"
	else
		echo "$(hostname) $kver: removed [${dirs//$'\n'/ }], modules left as loaded; unheld: ${pkgs:-nothing}"
	fi
	;;
*)
	die "unknown mode $mode"
	;;
esac
REMOTE
}

# The build on the first target, as its own user: prints the kernel version
# on its last line.
remote_build() {
	"${SSH[@]}" "$1" "bash -s -- $(printf '%q ' "$REMOTE_DIR" "$MODS")" <<'REMOTE'
set -euo pipefail
w=$1 mods=$2
kver=$(uname -r)
base=${kver%%-*}
die() { echo "$(hostname) $kver: $*" >&2; exit 1; }
kb=/lib/modules/$kver/build
[ -f "$kb/Module.symvers" ] || die "no kernel headers in $kb"
cc=$(sed -n 's/^CONFIG_CC_VERSION_TEXT="\([^ ]*\) .*/\1/p' "$kb/.config")
[ -n "$cc" ] && command -v "$cc" >/dev/null || die "no compiler ${cc:-?} as the kernel was built with"
pkgver=$(dpkg-query -W -f='${Version}' "linux-image-$kver")
src=$w/$kver/src/linux-source-$base
mkdir -p "$w/$kver"
cd "$w/$kver"
# The source is reused only once a whole unpack finished (src.done).
if [ ! -f src.done ]; then
	rm -rf deb src src.tmp
	mkdir deb src.tmp
	(cd deb && apt-get download -q "linux-source-$base=$pkgver") >/dev/null ||
		die "cannot download linux-source-$base=$pkgver"
	dpkg-deb --fsys-tarfile deb/*.deb |
		tar -xO "./usr/src/linux-source-$base/linux-source-$base.tar.bz2" |
		tar -xj -C src.tmp "linux-source-$base/drivers/nvme/target" \
			"linux-source-$base/drivers/nvme/host" ||
		die "cannot unpack the nvme drivers of linux-source-$base=$pkgver"
	mv src.tmp src
	rm -rf deb
	touch src.done
fi

# build <dir> [make args...]: the six modules from a fresh copy of the source.
build() {
	local d=$1
	shift
	rm -rf "$d"
	mkdir -p "$d/drivers/nvme"
	cp -r "$src/drivers/nvme/target" "$src/drivers/nvme/host" "$d/drivers/nvme/"
	make -C "$kb" M="$PWD/$d/drivers/nvme/target" CC="$cc" "$@" modules \
		>"$d.log" 2>&1 || die "build $d failed; see $PWD/$d.log"
	for m in $mods; do
		[ -f "$d/drivers/nvme/target/$m.ko" ] || die "build $d made no $m.ko"
	done
}

# The source check: every symbol the plain build exports has the kernel's
# checksum.
build plain
awk '{print $2, $1}' "plain/drivers/nvme/target/Module.symvers" | sort >plain.sums
awk '$3 ~ /drivers\/nvme\/target\// {print $2, $1}' "$kb/Module.symvers" | sort >kernel.sums
[ -s plain.sums ] || die "the plain build exports nothing"
if ! diff -q plain.sums kernel.sums >/dev/null; then
	die "the source is not the running kernel's: exported checksums differ (diff $PWD/plain.sums $PWD/kernel.sums)"
fi

build debugfs CONFIG_NVME_TARGET_DEBUGFS=y KCFLAGS=-DCONFIG_NVME_TARGET_DEBUGFS=1
[ -f debugfs/drivers/nvme/target/debugfs.o ] || die "the option did not reach the build file"
awk '{print $2, $1}' "debugfs/drivers/nvme/target/Module.symvers" | sort >debugfs.sums
diff -q plain.sums debugfs.sums >/dev/null && die "the option did not reach the C code: no checksum changed"

rm -rf "$w/ko"
mkdir -p "$w/ko"
for m in $mods; do
	cp "debugfs/drivers/nvme/target/$m.ko" "$w/ko/"
done
echo "$kver"
REMOTE
}

case $MODE in
install)
	BUILD=$1
	echo "== build on $BUILD"
	KVER=$("${SSH[@]}" "$BUILD" uname -r)
	time_start=$(date +%s)
	built=$(remote_build "$BUILD" | tail -1)
	[ "$built" = "$KVER" ] || {
		echo "build on $BUILD gave '$built', not $KVER" >&2
		exit 1
	}
	echo "== built for $KVER in $(($(date +%s) - time_start)) s"
	LOCAL=$(mktemp -d)
	trap 'rm -rf "$LOCAL"' EXIT
	"${SCP[@]}" "$BUILD:$REMOTE_DIR/ko/*.ko" "$LOCAL/"
	for t in "$@"; do
		echo "== install on $t"
		if [ "$t" != "$BUILD" ]; then
			"${SSH[@]}" "$t" "mkdir -p $REMOTE_DIR/ko && rm -f $REMOTE_DIR/ko/*.ko"
			"${SCP[@]}" "$LOCAL"/*.ko "$t:$REMOTE_DIR/ko/"
		fi
		remote_admin "$t" install "$KVER"
	done
	;;
check)
	rc=0
	for t in "$@"; do
		remote_admin "$t" check "" || rc=1
	done
	exit $rc
	;;
restore)
	for t in "$@"; do
		remote_admin "$t" restore ""
	done
	;;
*)
	echo "unknown mode '$MODE'; usage: nvmet_debugfs.sh install|check|restore user@host..." >&2
	exit 2
	;;
esac
