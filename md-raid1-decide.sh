#!/bin/bash
#
# md-raid1-decide.sh — should this RAID1 array be torn down and re-assembled?
#
# Usage:
#   md-raid1-decide.sh <md> <disk0-path> <disk0-health> <disk1-path> <disk1-health>
#
#   <md>            md0, or /dev/md0
#   <diskN-path>    the stable path used to assemble the array, e.g.
#                   /dev/disk/by-id/nvme-uuid.xxxx  (any path that resolves to
#                   the block device works: by-id, by-uuid, by-path, /dev/nvme0n1)
#   <diskN-health>  your own health-check verdict: "Healthy" or "Unhealthy"
#
# stdout (on success, exactly one line):
#   no-op          leave the array alone
#   re-assemble    md's view contradicts your health check, or the array is unusable
#
# Exit status:
#   0  a decision was printed
#   1  runtime error — nothing is written to stdout
#   2  usage error   — nothing is written to stdout
#
# Never blocks, never sleeps, never retries: one bounded pass over sysfs/procfs,
# reporting the decision for the state observed at that instant. Run it repeatedly
# from a supervising program.
#
# Reads only world-readable attributes; root is not required.
# Assumes /sys/block/<md>/md/fail_last_dev is 0 (the kernel default).

set -u
export LC_ALL=C            # keeps errno strings and number formats predictable

PROG=${0##*/}

die()   { printf '%s: %s\n'    "$PROG" "$*" >&2; exit 1; }
usage() { printf 'usage: %s <md> <disk0-path> Healthy|Unhealthy <disk1-path> Healthy|Unhealthy\n' "$PROG" >&2; exit 2; }
say()   { [ "${MDCHK_VERBOSE:-0}" = 1 ] && printf '%s\n' "$*" >&2; return 0; }

# The single writer to stdout. Nothing else in this script prints there, so an
# error path can never leave a partial decision behind.
decide() { say "==> $1"; printf '%s\n' "$1"; exit 0; }

# ---------------------------------------------------------------------------
# File access. Helpers set a global rather than printing, so die() runs in the
# main shell instead of a command-substitution subshell (where exit is a no-op).
#
# "absent" and "unreadable" are deliberately different results: a member md has
# removed genuinely has no directory, whereas a file that exists but will not
# read is an error we must not silently interpret as a RAID state.
# ---------------------------------------------------------------------------
RD=
rd() {                            # 0 = read (RD set), 1 = absent, 2 = error
    local p=$1
    RD=
    [ -e "$p" ] || return 1
    [ -r "$p" ] || return 2
    RD=$(cat -- "$p" 2>/dev/null) || return 2
    return 0
}

need() {                          # read or die
    rd "$1" || case $? in
        1) die "missing: $1"      ;;
        2) die "cannot read: $1"  ;;
    esac
}

opt() {                           # read, tolerate absence, die on error
    rd "$1" || case $? in
        1) RD=''                  ;;
        2) die "cannot read: $1"  ;;
    esac
}

DEVT=
devt() {                          # 0 = dev_t in DEVT, 1 = not a block device, 2 = error
    local p=$1 t
    DEVT=
    [ -e "$p" ] || return 1
    [ -b "$p" ] || return 1
    t=$(stat -Lc '%Hr:%Lr' -- "$p" 2>/dev/null)
    case $t in
        [0-9]*:[0-9]*) DEVT=$t; return 0 ;;
    esac
    t=$(stat -Lc '%t:%T' -- "$p" 2>/dev/null) || return 2   # older coreutils: hex
    case $t in
        [0-9a-f]*:[0-9a-f]*) DEVT=$(( 16#${t%:*} )):$(( 16#${t#*:} )); return 0 ;;
    esac
    return 2
}

KNAME=
kname() {                         # 0 = kernel name in KNAME, 1 = unresolvable
    local r
    KNAME=
    r=$(readlink -f -- "$1" 2>/dev/null) || return 1
    KNAME=${r##*/}
    [ -n "$KNAME" ] || return 1
    return 0
}

# ---------------------------------------------------------------------------
# 1. Arguments
# ---------------------------------------------------------------------------
[ $# -eq 5 ] || usage

MD=${1#/dev/}; MD=${MD#/sys/block/}
case $MD in ''|*/*) usage ;; esac

PATH_OF=("$2" "$4")
RAW_HEALTH=("$3" "$5")
for i in 0 1; do
    case $(printf '%s' "${RAW_HEALTH[$i]}" | tr '[:upper:]' '[:lower:]') in
        healthy)   HEALTH[$i]=healthy   ;;
        unhealthy) HEALTH[$i]=unhealthy ;;
        *) usage ;;
    esac
done

# ---------------------------------------------------------------------------
# 2. Resolve each path to a dev_t
#
# dev-<name> is named after the kname at add time (md.c:2607), which is not
# stable across nvme-of reconnects, so members are matched on dev_t via the
# "block" symlink md creates (md.c:2611). If a path now resolves to a different
# dev_t than the one md holds, md is pinning a dead namespace and nothing will
# match — which is the correct answer, not a lookup failure.
# ---------------------------------------------------------------------------
for i in 0 1; do
    if devt "${PATH_OF[$i]}"; then
        DEV_T[$i]=$DEVT
        kname "${PATH_OF[$i]}" || die "cannot resolve: ${PATH_OF[$i]}"
        KNAME_OF[$i]=$KNAME
        say "disk $i: ${PATH_OF[$i]} -> $KNAME (${DEV_T[$i]}), you say ${HEALTH[$i]}"
    else
        [ $? -eq 1 ] || die "cannot stat: ${PATH_OF[$i]}"
        # The namespace is gone. Whatever your health check said, this is not a
        # disk anything can be re-assembled onto.
        DEV_T[$i]=''; KNAME_OF[$i]=''
        say "disk $i: ${PATH_OF[$i]} is not a block device — treating as unhealthy"
        HEALTH[$i]=unhealthy
    fi
done

if [ -n "${DEV_T[0]}" ] && [ "${DEV_T[0]}" = "${DEV_T[1]}" ]; then
    printf '%s: both paths resolve to the same device (%s)\n' "$PROG" "${DEV_T[0]}" >&2
    exit 2
fi

# ---------------------------------------------------------------------------
# 3. Is there anything to re-assemble onto?
# ---------------------------------------------------------------------------
if [ "${HEALTH[0]}" = unhealthy ] && [ "${HEALTH[1]}" = unhealthy ]; then
    say "neither disk is usable — re-assembling could only produce another dead array"
    decide no-op
fi

# ---------------------------------------------------------------------------
# 4. Array level
#
# Checked before any per-disk state, because an array that was assembled but
# never started (mddev->pers == NULL) still reports every member as "in_sync":
# super_1_validate() sets In_sync from the superblock at assembly time
# (md.c:2170), long before the personality runs.
# ---------------------------------------------------------------------------
[ -d /sys/block ] && [ -r /sys/block ] && [ -x /sys/block ] \
    || die "/sys/block is not readable — is sysfs mounted?"

SYS=/sys/block/$MD/md

if [ ! -d "$SYS" ]; then
    say "$MD: no array present"
    decide re-assemble
fi
[ -r "$SYS" ] && [ -x "$SYS" ] || die "cannot read: $SYS"

need "$SYS/array_state"; ASTATE=$RD
say "$MD: array_state=$ASTATE"

case $ASTATE in
    clear|inactive)
        say "$MD: assembled but not running"
        decide re-assemble ;;
    broken)
        decide re-assemble ;;
    suspended)
        say "$MD: array suspended, not serving I/O"
        decide re-assemble ;;
    readonly|read-auto|clean|active|active-idle|write-pending)
        : ;;                    # ro is an administrative choice, not a health signal
    *)
        die "unrecognised array_state: $ASTATE" ;;
esac

# array_state renders "broken" only while the array is otherwise clean
# (md.c:4615); /proc/mdstat reports MD_BROKEN unconditionally once the
# personality is running (md.c:8858), so that is the reliable probe.
[ -r /proc/mdstat ] || die "cannot read: /proc/mdstat"
MDSTAT=$(grep "^$MD : " /proc/mdstat) || MDSTAT=
say "$MD: mdstat: ${MDSTAT:-<no line>}"
case $MDSTAT in
    "$MD : broken"*)
        # With fail_last_dev=0 this is the state per-disk flags CANNOT show:
        # raid1_error() set MD_BROKEN and returned without setting Faulty
        # (raid1.c:1757), so the surviving member still reads "in_sync" while
        # md_submit_bio() fails every write (md.c:441).
        say "$MD: MD_BROKEN — md tried to fail the last in-sync member and was refused"
        decide re-assemble ;;
esac

opt "$SYS/sync_action"; SYNC=$RD
opt "$SYS/degraded";    say "$MD: sync_action=${SYNC:-<none>} degraded=${RD:-?}"

# ---------------------------------------------------------------------------
# 5. Locate each disk among the array's members
# ---------------------------------------------------------------------------
MEMBER=
member_of() {                     # 0 = member dir in MEMBER, 1 = not a member
    local want=$1 kn=$2 d
    MEMBER=
    [ -n "$want" ] || return 1
    for d in "$SYS"/dev-*; do
        [ -d "$d" ] || continue
        rd "$d/block/dev" || {
            # sysfs_create_link() for "block" is explicitly best-effort
            # (md.c:2610), so fall back to the name md registered.
            [ $? -eq 1 ] || die "cannot read: $d/block/dev"
            [ -n "$kn" ] && [ "${d##*/}" = "dev-$kn" ] && { MEMBER=$d; return 0; }
            continue
        }
        [ "$RD" = "$want" ] && { MEMBER=$d; return 0; }
    done
    return 1
}

VERDICT=
verdict() {                       # md's opinion of member dir $1 ('' = not a member)
    local d=$1
    VERDICT=absent
    [ -n "$d" ] || return 0
    # A read error here is real: the attribute is world-readable, and -ENODEV
    # would mean rdev->mddev went NULL mid-unbind (md.c:3715). Either way we
    # cannot characterise the disk, so we must not guess.
    rd "$d/state" || case $? in
        1) return 0 ;;                       # raced with removal: not a member
        2) die "cannot read: $d/state" ;;
    esac
    case $RD in
        *in_sync*)
            # state_show() also prints "faulty" for unacknowledged bad blocks
            # while In_sync is still set (md.c:3030) — a pending metadata update,
            # not an ejection. In_sync is the discriminator, not the token.
            case $RD in
                *blocked*|*faulty*) VERDICT=transient ;;
                *)                  VERDICT=in_sync   ;;
            esac ;;
        *faulty*) VERDICT=faulty ;;          # raid1_error cleared In_sync, set Faulty
        *spare*)  VERDICT=spare  ;;
        *)        die "unrecognised member state for $d: $RD" ;;
    esac
    return 0
}

# ---------------------------------------------------------------------------
# 6. Decision
# ---------------------------------------------------------------------------
for i in 0 1; do
    member_of "${DEV_T[$i]}" "${KNAME_OF[$i]}" || MEMBER=
    verdict "$MEMBER"
    say "disk $i: you=${HEALTH[$i]} md=$VERDICT member=${MEMBER:-<none>}"

    # md agreeing that your bad disk is bad is not a disagreement.
    [ "${HEALTH[$i]}" = healthy ] || continue

    case $VERDICT in
        in_sync)
            ;;
        faulty|absent)
            say "disk $i: Healthy to you, $VERDICT to md"
            decide re-assemble ;;
        transient)
            # Blocked clears at the end of md_update_sb() (md.c:2956). Report the
            # array as it stands; the next invocation sees the settled state.
            say "disk $i: mid-metadata-update"
            decide no-op ;;
        spare)
            case $SYNC in
                recover|resync|reshape|repair|check)
                    say "disk $i: rebuilding ($SYNC)"
                    decide no-op ;;
                frozen)
                    say "disk $i: spare, recovery frozen — deliberate"
                    decide no-op ;;
                *)
                    say "disk $i: idle spare md is not recovering onto"
                    decide re-assemble ;;
            esac ;;
    esac
done

say "md's view matches your health check on both disks"
decide no-op
