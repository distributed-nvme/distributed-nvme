#!/usr/bin/env bash
# The pinned etcd release every suite that starts an etcd runs, and the one
# download-and-verify path that puts it into integtest/bin/cache. The worker,
# gateway, cdc and end-to-end suites source this file and call fetch_etcd at
# preflight; `make fetch-etcd` runs it, and `make test` hands the unit tests
# the binary it leaves there when ETCD_BIN is empty or unset and the PATH
# holds no etcd (dnv-worker.md EU7). A cached tarball whose sha256 already
# matches the pin is never re-downloaded, so a repeat run needs no network
# and a fresh checkout still works.
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

# fetch_etcd leaves etcd and etcdctl of the pinned release under
# $ETCD_CACHE_DIR/$ETCD_DIST. It reports through the caller's log and die.
fetch_etcd() {
	mkdir -p "$ETCD_CACHE_DIR"
	if [ -f "$ETCD_TAR" ] &&
		[ "$(sha256sum "$ETCD_TAR" | cut -d' ' -f1)" = "$ETCD_SHA256" ]; then
		log "  etcd $ETCD_VERSION tarball cached and verified"
	else
		log "  downloading $ETCD_URL"
		curl -fsSL -o "$ETCD_TAR.part" "$ETCD_URL" ||
			die "downloading the etcd tarball failed"
		mv -f "$ETCD_TAR.part" "$ETCD_TAR"
		local got
		got=$(sha256sum "$ETCD_TAR" | cut -d' ' -f1)
		[ "$got" = "$ETCD_SHA256" ] ||
			die "etcd tarball sha256 is $got, want $ETCD_SHA256"
	fi
	# tar gives each file its mode before it writes the bytes, so an
	# extraction cut short, by a signal or a full disk, can leave a truncated
	# etcd that is executable. The release is therefore extracted into a
	# scratch directory of the cache and moved into place only once tar has
	# finished: its directory is whole or absent, and an absent one is
	# extracted again.
	if [ ! -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcd" ] ||
		[ ! -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcdctl" ]; then
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
		[ -x "$ETCD_CACHE_DIR/$ETCD_DIST/etcd" ] || exit 1
		echo "$ETCD_CACHE_DIR/$ETCD_DIST/etcd"
		;;
	*) die "usage: fetch_etcd.sh [path]" ;;
	esac
fi
