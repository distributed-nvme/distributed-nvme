#!/usr/bin/env bash
# The pinned etcd release every suite that starts an etcd runs, and the one
# download-and-verify path that puts it into integtest/bin/cache. The worker,
# gateway, cdc, end-to-end and csi suites source this file and call
# fetch_etcd at preflight, and the tutorial suite at setup; `make
# fetch-etcd` runs it, and `make test` hands
# the unit tests the binary it leaves there when ETCD_BIN is empty or unset
# and the PATH holds no etcd (dnv-worker.md EU7). A cached tarball whose
# sha256 already matches the pin is never re-downloaded, so a repeat run
# needs no network and a fresh checkout still works.
#
# Several fetches, by suites or by `make fetch-etcd`, may start at once on
# one cache: fetch_etcd holds an exclusive flock on the cache directory from
# its first look at the cache to its last check, so one caller at a time
# checks, downloads and extracts. The others wait, at most ETCD_LOCK_WAIT seconds,
# and then find the release in place, or fetch it themselves when the caller
# before them failed. A reader that takes no lock, such as `path` below,
# never sees a half-extracted release: the release directory is moved into
# place whole, and a directory without both binaries counts as absent.
#
# Run as a command: with no argument it fetches and prints the etcd binary;
# with `path` it prints the cached etcd binary, or fails when there is none,
# and fetches nothing.

ETCD_VERSION=v3.6.14
ETCD_DIST="etcd-$ETCD_VERSION-linux-amd64"
ETCD_URL="https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/$ETCD_DIST.tar.gz"
ETCD_SHA256=ffe840ff9295808e88cce2794a18a5ac87f12a5203c8314d0bf6aa119b41bac5
# CDPATH is emptied for the cd: the Makefile runs this file by a relative
# path, whose directory cd would otherwise look up in CDPATH, printing where it
# went.
ETCD_CACHE_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/bin/cache"
ETCD_TAR="$ETCD_CACHE_DIR/$ETCD_DIST.tar.gz"
# ETCD_LOCK is what fetch_etcd locks: the cache directory itself, opened for
# reading, so a whole cache the caller cannot write, such as one another user
# made, still serves that caller. ETCD_LOCK_WAIT, in seconds, bounds the wait
# for another caller's fetch, so a caller behind a holder that hangs or
# downloads very slowly fails with the reason instead of waiting forever.
ETCD_LOCK="$ETCD_CACHE_DIR"
ETCD_LOCK_WAIT=600

# etcd_cached says whether the release directory holds both binaries: the one
# test of a whole release, which the fetch and the `path` reader share, so a
# reader never takes a binary from a directory the fetch would replace.
etcd_cached() {
	[ -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcd" ] &&
		[ -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcdctl" ]
}

# fetch_etcd leaves etcd and etcdctl of the pinned release under
# $ETCD_CACHE_DIR/$ETCD_DIST. It reports through the caller's log and die.
#
# The work runs in a subshell that holds the lock on a descriptor of its own.
# A die in it ends only the subshell, whose exit closes the descriptor and so
# frees the lock before the caller's exit handling runs: that handling can
# take minutes and start processes, which would inherit an open descriptor
# and hold the lock with it. The caller then exits with the subshell's
# status, the die in the subshell having reported the failure. Every step
# that can fail the fetch tests its own status, since errexit does not reach
# into a subshell whose status is tested.
fetch_etcd() {
	(
		mkdir -p "$ETCD_CACHE_DIR" ||
			die "creating the etcd cache $ETCD_CACHE_DIR failed"
		local lockfd rc=0
		exec {lockfd}<"$ETCD_LOCK" ||
			die "opening the etcd cache $ETCD_LOCK to lock it failed"
		flock -w "$ETCD_LOCK_WAIT" "$lockfd" || rc=$?
		case $rc in
		0) ;;
		1) die "waited ${ETCD_LOCK_WAIT}s for the lock on the etcd cache $ETCD_LOCK, which another fetch holds" ;;
		*) die "locking the etcd cache $ETCD_LOCK failed (flock exit $rc)" ;;
		esac

		if [ -f "$ETCD_TAR" ] &&
			[ "$(sha256sum "$ETCD_TAR" | cut -d' ' -f1)" = "$ETCD_SHA256" ]; then
			log "  etcd $ETCD_VERSION tarball cached and verified"
		else
			log "  downloading $ETCD_URL"
			# curl gives up on a connection that does not open and on a
			# transfer that stalls, so a holder whose download stalls fails
			# and frees the lock, and the next caller downloads again.
			curl -fsSL --connect-timeout 30 --speed-limit 1024 --speed-time 60 \
				-o "$ETCD_TAR.part" "$ETCD_URL" ||
				die "downloading the etcd tarball failed"
			mv -f "$ETCD_TAR.part" "$ETCD_TAR" ||
				die "moving the downloaded etcd tarball into place failed"
			local got
			got=$(sha256sum "$ETCD_TAR" | cut -d' ' -f1)
			[ "$got" = "$ETCD_SHA256" ] ||
				die "etcd tarball sha256 is $got, want $ETCD_SHA256"
		fi
		# tar gives each file its mode before it writes the bytes, so an
		# extraction cut short, by a signal or a full disk, can leave a
		# truncated etcd that is executable. The release is therefore
		# extracted into a scratch directory of the cache and moved into place
		# only once tar has finished: its directory is whole or absent, and an
		# absent one is extracted again.
		if ! etcd_cached; then
			local tmp="$ETCD_CACHE_DIR/.extract"
			rm -rf "$tmp" && mkdir "$tmp" ||
				die "preparing $tmp for the etcd tarball failed"
			tar -xzf "$ETCD_TAR" -C "$tmp" || {
				rm -rf "$tmp"
				die "extracting the etcd tarball failed"
			}
			rm -rf "${ETCD_CACHE_DIR:?}/$ETCD_DIST" &&
				mv -T "$tmp/$ETCD_DIST" "$ETCD_CACHE_DIR/$ETCD_DIST" ||
				die "moving the extracted etcd release into place failed"
			rm -rf "$tmp"
		fi
		[ -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcd" ] ||
			die "no etcd binary after extraction"
		[ -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcdctl" ] ||
			die "no etcdctl after extraction"
	) || exit
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -euo pipefail
	log() { echo "$*" >&2; }
	die() {
		echo "fetch_etcd.sh: $*" >&2
		exit 1
	}
	case "${1:-}" in
	"")
		fetch_etcd
		echo "$ETCD_CACHE_DIR/$ETCD_DIST/etcd"
		;;
	path)
		etcd_cached || exit 1
		echo "$ETCD_CACHE_DIR/$ETCD_DIST/etcd"
		;;
	*) die "usage: fetch_etcd.sh [path]" ;;
	esac
fi
