#!/usr/bin/env bash
#
# e2e_test.sh — the end-to-end integration test of doc/e2e_integtest.md. Ten
# real guests and no fakes anywhere: a real single-node etcd, a real
# dnv-gateway, a real dnv-worker and a real dnv-cdc on the control-plane VM,
# many real `dnv-agent dn` instances per DN VM over loop devices, one real
# `dnv-agent cn` per CN VM (md-raid1 + dm-thin + dm-striped + nvmet), and two
# real kernel NVMe hosts that reach their namespaces only through the cdc.
# Every control-plane call is the SHIPPED `bin/dnvctl`, run on the cp guest
# over ssh (E2E2) — no workerctl, no gatewayctl, no direct etcd write.
#
#   bash integtest/e2e_test.sh \
#     --cp   yupeng@192.168.122.78 \
#     --cn   yupeng@192.168.122.125 --cn yupeng@192.168.122.229 --cn yupeng@192.168.122.77 \
#     --dn   yupeng@192.168.122.48  --dn yupeng@192.168.122.70  --dn yupeng@192.168.122.49 --dn yupeng@192.168.122.122 \
#     --host yupeng@192.168.122.193 --host yupeng@192.168.122.197
#
#   optional: [--only <case>] [--cleanup-only] [--slice-cnt N]
#             [--redund raid1|none] [--dns-per-vm N]
#
# Cases, in order, each from an EMPTY etcd and a freshly built storage pool
# (E2E11): smoke, ops, copy, react. `--only` picks one. Cleanup runs
# unconditionally at the START and, on success only, at the END (E2E6): a
# failing run leaves every process, dm/md/nvmet object, loop device, host
# connection and log in place and dumps diagnostics instead. The START
# cleanup tolerates finding nothing, but not a verb that never finished — that
# stops the run at the end of the sweep (cleanup_start_gate, which lets the
# other guests be swept first), instead of letting preflight misname the
# surviving debris three steps later.
#
# The default shape is the widest storage pool this tree can build: 32 slices
# (common.MaxSliceCntPerSp), md-raid1, so 2 x 32 x 2 = 128 sides on 128
# DISTINCT disk nodes and 64 md arrays plus 32 thin pools on one CN.
#
# ---------------------------------------------------------------------------
# ABSOLUTE RULES. Every one of these is a failure this lab has already
# produced; none of them is a style preference.
# ---------------------------------------------------------------------------
#
#  1. NEVER pass iflag= or oflag= to dd, here or in any shipped helper. The
#     guests ship uutils dd 0.8.0, whose direct flags produce both false
#     failures and SILENT ZERO WRITES. Writes use conv=fsync; a read that must
#     hit the media is preceded by a cache drop (E2E7).
#  2. `nvme connect`/`connect-all`: --fast_io_fail_tmo has UNDERSCORES, and
#     --hostid is always passed explicitly — an implicit one fails EINVAL under
#     the kernel's 1:1 hostnqn<->hostid rule.
#  3. Every pkill lives in a helper FILE on the guest and its pattern is
#     bracketed ([d]nv-agent). `pkill -f` matches the wrapping `bash -lc` argv
#     of the ssh command itself, so a pattern in an ssh command STRING kills
#     its own shell (E2E8). Processes this suite starts are signalled by the
#     pid it recorded, not by pattern; the pkill sweeps are the cleanup
#     fallback for a pid file that is gone.
#  4. nvmet teardown order: rmdir ports/<id>/ana_groups/3 and .../2 BEFORE
#     rmdir ports/<id>, or the rmdir fails with "Directory not empty" and the
#     leftover port fails the NEXT suite's setup.
#  5. An ANA-inaccessible namespace has NO /dev node and its requeued bios
#     wedge a dm suspend until the controller is deleted. No host IO between a
#     `--auto-suspend`/`ns set-suspended` and the resume, and never a dm
#     suspend over one. `timeout` does not bound a read of a suspended dm
#     device — it waits for the unkillable child — so a read that may block
#     runs detached, with the GROUP's stdout redirected, or command
#     substitution hangs with it.
#  6. `nvmf-connect@.service` is masked on both hosts for the whole run
#     (E2E10): the kernel's own autoconnector matches the discovery AEN
#     (NVME_AEN=0x70f002) and would connect behind the suite's back. Every
#     connect here is the suite's own act.
#  7. The `63-dnv-md.rules` mask goes on the CN VMs AND on the DN VMs. md runs
#     only on a CN, but the md SUPERBLOCK that CN writes travels down the side
#     export and lands on the DN's own storage, so a `linux_raid_member` shows
#     up on the DN — on the side dm device and, at the same offset, on the
#     per-CN linear that maps the side 1:1. Unmasked, the stock incremental
#     rule (`mdadm -I`, which on that path WILL assemble a degraded array
#     read-only) assembles it on the DN, and the array holds whichever of the
#     two udev probed first; either way the side cannot be removed (a pinned
#     linear holds it open too) and dn_cleanup grinds past CLEANUP_TIMEOUT.
#     Observed 2026-09-17: 35 stray arrays on one DN VM, 28 on another —
#     exactly the two DN VMs that carried no md udev rule at all. THIS mask
#     was on none of the four: it went on the CN VMs only.
#  8. NEVER run this suite while any other dnv suite runs anywhere in the lab
#     (E2E9). It occupies all ten guests, and its CN agents mount their tmpfs
#     at /tmp/dnv-tmpfs — the very path cnagent_test.sh owns.
#  9. NEVER edit this file while it is running: bash re-seeks the script by
#     byte offset, so a length-changing edit kills the run with a bogus syntax
#     error at an unrelated line.
#
# Spec: doc/e2e_integtest.md (rules E2E1..). Where a comment here cites a Go
# file:line it was re-derived from the tree, not copied from a plan.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants: binaries and the pinned etcd (§7.2)
# ---------------------------------------------------------------------------

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN_DIR="$REPO_ROOT/integtest/bin"
CACHE_DIR="$BIN_DIR/cache"

# The five shipped binaries `make build` produces (Makefile CMDS = cmd/*).
AGENT_BIN="$REPO_ROOT/bin/dnv-agent"
GATEWAY_BIN="$REPO_ROOT/bin/dnv-gateway"
WORKER_BIN="$REPO_ROOT/bin/dnv-worker"
CDC_BIN="$REPO_ROOT/bin/dnv-cdc"
DNVCTL_BIN="$REPO_ROOT/bin/dnvctl"

# cnagentctl is built for ONE subcommand, `host-id --hostnqn` (common.NvmeHostId
# at common/name_fmt.go:726), so the suite can pass the kernel's required
# explicit --hostid. workerctl is built for ONE subcommand, `constants`, which
# prints the Go constants as JSON on the driver. Neither is ever shipped to a
# guest — the same arrangement cdc_test.sh uses for workerctl.
CNAGENTCTL_BIN="$BIN_DIR/cnagentctl"
WORKERCTL_BIN="$BIN_DIR/workerctl"

# The pinned etcd release, shared with the worker, gateway and cdc suites:
# same version, same digest, same integtest/bin/cache. A tarball already
# verified there is never re-downloaded, so this suite needs no network.
ETCD_VERSION=v3.6.14
ETCD_DIST="etcd-$ETCD_VERSION-linux-amd64"
ETCD_URL="https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/$ETCD_DIST.tar.gz"
ETCD_SHA256=ffe840ff9295808e88cce2794a18a5ac87f12a5203c8314d0bf6aa119b41bac5
ETCD_TAR="$CACHE_DIR/$ETCD_DIST.tar.gz"

# common.EtcdMaxTxnOps — a DEPLOYMENT requirement of every etcd serving dnv,
# not a knob of this suite, and the one constant this suite MUST NOT type out:
# read_constants() fills it at preflight from `workerctl constants`, exactly as
# worker_test.sh:955-968, gateway_test.sh and cdc_test.sh do since 6995e5a.
# (The plan's §7.1 asks for a literal 1024 "as in the other suites"; the other
# suites carry no literal any more, and a hand-copied one is precisely what
# `workerctl constants` was added to end — integtest/workerctl/main.go:1216.)
#
# It matters here more than in any other suite: THIS suite is the one that
# actually issues CreateStoragePool at its widest shape, whose compare count is
# 7 + slice_cnt + 7 x (2 x slice_cnt x MaxAllocLegPerGrp) + 8 x cntlr_cnt
# (common/constants.go:334-336, the constant itself at :365) = 951 for the
# default run (32 slices, raid1, cntlr_cnt 2) and 967 at MaxCntlrCntPerSp. An
# etcd started below that fails the create with "too many operations in txn
# request".
ETCD_MAX_TXN_OPS=
# common.MaxAllocLegPerGrp, from the same JSON: the legs per group the
# allocator actually uses for md-raid1 (gateway/alloc.go:32-37 returns it, or 1
# for redund_none). read_constants cross-checks LEGS against it, because every
# number below — DN picks, DNS_PER_VM, the create's size — is a multiple of it.
MAX_ALLOC_LEG_PER_GRP=

# ---------------------------------------------------------------------------
# Constants: paths (§7.2)
# ---------------------------------------------------------------------------

# One work directory on every guest, shared by no other suite: worker_test.sh
# uses /var/tmp/dnv-worker-integtest, gateway_test.sh dnv-gateway-integtest,
# cdc_test.sh dnv-cdc-integtest, cnagent_test.sh dnv-cn-integtest,
# dnagent_test.sh dnv-integtest, dnvctl_test.sh dnv-dnvctl-integtest. None is a
# prefix of this one, and no suite does `rm -rf /var/tmp/dnv-*`.
WORK=/var/tmp/dnv-e2e

# The shipped helper is a SIBLING of $WORK, never a child: cleanup runs
# `rm -rf $WORK` through the helper, and a script may not delete itself while
# bash is reading it. Each guest has exactly one role, so one path carries
# whichever role family's helper that guest needs.
HELPER=/var/tmp/dnv-e2e-helper.sh

NVMET=/sys/kernel/config/nvmet

# common.DefaultTmpfsPrefix (common/constants.go:139). A cn agent's tmpfs is
# NOT derived from --local-store: NewNameFmt sets tmpfsPrefix unconditionally
# and CnTmpfsPath returns <prefix>/<cluster_id>-<cn_id>, both %016x
# (common/name_fmt.go:57-67, :393-402). No flag moves it. So CN state lives
# BOTH under $WORK and here —
# which is why the space guard and cleanup must both look here, and why rule 8
# above forbids running while the cn suite (same path) runs.
TMPFS_DIR=/tmp/dnv-tmpfs

# ---------------------------------------------------------------------------
# Constants: ports (§7.2, D9/D10/D12/D25)
# ---------------------------------------------------------------------------
#
# Bound-port inventory of the six existing suites, verified against their own
# declaration blocks: worker_test.sh 12379/12380, 29600-29603, 29700-29702;
# gateway_test.sh 15379/15380, 29810-29812, 29820-29823, 29830-29832;
# cdc_test.sh 13379/13380, 18009-18012, 14420-14423; cnagent_test.sh
# 29528/29529 + trsvcid 4200; dnagent_test.sh 29528 + trsvcid 4200;
# dnvctl_test.sh 29840/29841. Production, per those suites' comments: gateway
# 29527, etcd 2379.
#
# Nothing below is BOUND by any of them. Two numbers do APPEAR elsewhere:
# dnvctl_test.sh:82-83 carries 127.0.0.1:29901 and :29902 as payload for its
# fake to record — that suite never dials them, and its own port list is
# (29840 29841). The plan's flat "no port here appears in any other suite's
# block" is false as written; "no port here is bound by another suite" is what
# holds.
ETCD_CLIENT_PORT=16379
ETCD_PEER_PORT=16380
GW_PORT=29850
CDC_PORT=18020

# DN VM v, instance k (k = 0..DNS_PER_VM-1): gRPC 29900+k, nvme-tcp trsvcid
# 4300+k, nvmet port id k+1. Each agent needs its own trsvcid — two listeners
# cannot share one TCP port — and therefore its own configfs port, which is
# what `dnv-agent --nvmet-port-id` buys. ana_groups nest UNDER the port
# (agent/nvmet.go:55-61), so distinct port ids also give each agent its own
# groups 1/2/3. MAX_DNS_PER_VM keeps the gRPC block below CN_GRPC_PORT:
# 29900 + 49 = 29949 < 29950.
DN_PORT_BASE=29900
DN_TRSVCID_BASE=4300
MAX_DNS_PER_VM=50

# One cn agent per CN VM. Its trsvcid repeats the DN base, which is safe
# because it is a different guest; the §7.7 port sweep therefore accepts 4300
# on a CN VM and 4300..4349 on a DN VM, and refuses to touch an nvmet port
# outside that band (it belongs to another suite, or to a human).
CN_GRPC_PORT=29950
CN_TRSVCID=4300
# The cn agent is launched without --nvmet-port-id, so it converges
# ports/common.NvmetPortId. Named here because cleanup must rmdir exactly that
# port (after its ana_groups 3 and 2) on a CN VM.
CN_PORT_ID=1
TRSVCID_MIN=$DN_TRSVCID_BASE
TRSVCID_MAX=$((DN_TRSVCID_BASE + MAX_DNS_PER_VM - 1))

# ---------------------------------------------------------------------------
# Constants: the shape of the run (§7.1)
#
# A shell suite cannot import package common, so every number a Go constant
# owns is repeated here WITH the constant it mirrors. Two of them are not
# repeated at all but read from `workerctl constants` above, because they are
# deployment requirements rather than choices (EtcdMaxTxnOps, MaxAllocLegPerGrp).
# ---------------------------------------------------------------------------

# common.MaxSliceCntPerSp (common/constants.go:57) — the widest sp the gateway
# accepts, enforced at gateway/storagepool.go:346-349 (create) and
# gateway/validate.go:399-402 (clone geometry). It is the DEFAULT here because
# the point of this suite is to prove every operation at the ceiling.
SLICE_CNT_DEFAULT=32

# common.MinDnExtSize (common/constants.go:16) — 64 MiB, the smallest extent
# gateway/validate.go:180-185 accepts. Small extents are what keep the run's
# real allocation inside the §7.8 caps: a group costs one extent per leg.
# Cluster-scoped and WRITE-ONCE (no UpdateCluster RPC), so it is only ever
# applied by `cluster create --extent-size` against an EMPTY etcd.
EXTENT_SIZE=67108864

# The sp's dm-striped chunk. dm-striped maps chunk c of a td to slice
# c mod slice_cnt (doc/architecture.md §11.4 at :3022-3023,
# agent/cnagent/thinbm.go:406-413), and stripe index i is slice_idx i because
# the plan sorts its slices by slice_idx (agent/cnagent/plan.go:447-452) and
# raid0Args emits them in that order (agent/cnagent/td.go:17-19), so with a
# 1 MiB stripe every offset k x (slice_cnt x 1 MiB) lands in slice 0 —
# which is how the react case drives ONE slice's pool over its low-water mark
# (D18).
#
# 1 MiB is also the hard ceiling for the COPY case, and that is the binding
# limit, not the sp's: validateCloneGeometry refuses src_stripe_size above
# 256 x 4 KiB = 1048576 (gateway/validate.go:403-410), while the sp itself
# would accept up to common.MaxDmRaid0StripeSize = 64 MiB. Raising this value
# creates the sp happily and then fails `clone create` with INVALID_ARGUMENT.
STRIPE_SIZE=1048576

# `sp create --init-ext-cnt`: extents per data group. 0 is refused by the
# gateway, and this value also fixes AR6's grow size — a data grow appends a
# group of the FIRST data group's ext_cnt (worker/reaction.go:1015-1020).
INIT_EXT_CNT=1

# `sp create --cntlr-cnt`: one primary and one standby, on two of the three CN
# VMs. The third CN is deliberately spare, so AR7 has somewhere to put a
# replacement (cn placement dedupes by location the same way dn placement does,
# model/alloc.go:280-284).
CNTLR_CNT=2

# `sp create --slots`: the cntlid slot list D20's set-cntlid-slots case grows to
# 0,1,2 before it creates a third cntlr in slot 2.
SLOTS=0,1

# event_threshold, in seconds (ctl/sp.go:213-219 declares the four flags;
# worker/reaction.go consumes them). TWO SETS, ONE PER KIND OF CASE, and
# sp_thresholds picks between them before each sp is built.
#
# WHY THE CHOICE IS MADE AT CREATE TIME AND NOWHERE ELSE. There is no RPC that
# changes a threshold after CreateStoragePool: the four flags exist only on
# `sp create`, the handler stores the message verbatim
# (gateway/storagepool.go:457, and model.ResolveEventThreshold is deliberately
# exempt from resolve-at-write, model/ops.go:148-192), and the sp's whole life
# runs on whatever that one call wrote. Every other sp-scoped mutator leaves
# event_threshold alone. So the set a case needs has to be chosen before its sp
# exists, which is why this is a parameter of setup_create_sp and not of a case.
#
# WHAT THE FIRST REAL RUN MEASURED (2026-09-17, commit deca203, all ten lab
# guests). Building the default shape — 32 slices, raid1, 64 md arrays over 128
# legs, 32 thin pools — kept the primary CN spawning 126,657 processes over
# 8m45s: 82,099 dmsetup, 20,127 mdadm, 16,795 lsblk, about 240 a second
# sustained on a 2-vCPU guest. (That is the whole window, restarts included —
# see WAIT_BUILD.) Under that load the primary cannot answer a health check inside
# five seconds, the worker sets its err_epoch, and AR5 moves the role. The new
# primary then starts the SAME build from scratch, goes unresponsive
# in its turn and hands the role back: the first run recorded
# failover 1->2, a spare_create, and failover 2->1, all inside setup.
#
# NOTE CAREFULLY, because it is what decides the shape of the fix:
# common.DefaultPrimaryUnhealthy is ALSO 5 (common/constants.go:206), and
# ResolveEventThreshold turns an absent flag into it (model/ops.go:179-181). So
# the failover loop is NOT caused by an aggressive suite value — omitting
# --thr-primary would produce exactly the same five seconds. The only way out is
# a LONG value, passed explicitly, at create.
#
# THE QUIET SET — smoke, ops and copy. These three cases test operations, not
# reactions: a failover or a spare in the middle of one is noise that
# invalidates its assertions (an absolute side count, a "no spare leg yet", a
# digest read through a controller that is no longer the primary). So every
# threshold here is longer than any build can run. 1800 s is 3.4 x the 525 s
# window the first run measured (see WAIT_BUILD for what that number is and is
# not), on a shape that is already the widest this tree can build; leg is
# doubled because gateway/validate.go:297-305 refuses leg_unhealthy <=
# side_unhealthy after the defaults are resolved. Nothing caps them from above:
# ResolveEventThreshold's own comment says "No upper bound applies: §7 only
# requires each value to be >= 1" (model/ops.go:155-156).
THR_QUIET_PRIMARY=1800
THR_QUIET_CNTLR=1800
THR_QUIET_SIDE=1800
THR_QUIET_LEG=3600

# THE REACTING SET — react alone. AR7 waits cntlr_unhealthy and AR8 waits
# side_unhealthy/leg_unhealthy, and at the gateway defaults (600, 600, 1200 —
# common/constants.go:207-209) neither is observable inside any bound this
# suite could sanely wait out. react therefore keeps D17's short values and
# pays for them: its OWN build can produce a failover and a spare before the
# case starts, which is why every react assertion is written against a shape
# snapshot rather than against a count (see the react section's header).
THR_REACT_PRIMARY=5
THR_REACT_CNTLR=20
THR_REACT_SIDE=20
THR_REACT_LEG=30

# The two argv strings, composed once so log_topology can print both and
# sp_thresholds only has to choose. Each splits into eight words at the call
# site (see setup_create_sp).
THR_QUIET="--thr-primary $THR_QUIET_PRIMARY --thr-cntlr $THR_QUIET_CNTLR"
THR_QUIET="$THR_QUIET --thr-side $THR_QUIET_SIDE --thr-leg $THR_QUIET_LEG"
THR_REACT="--thr-primary $THR_REACT_PRIMARY --thr-cntlr $THR_REACT_CNTLR"
THR_REACT="$THR_REACT --thr-side $THR_REACT_SIDE --thr-leg $THR_REACT_LEG"

# The ACTIVE set: the four numbers the sp now being built will carry, and the
# argv that carries them. They exist as four variables and not as one opaque
# string for two reasons, neither of which is arithmetic: setup_create_sp
# asserts them back one FIELD at a time out of `sp get`, which is also the proof
# that $THR split into eight words instead of arriving as one; and react's
# progress messages name the individual threshold the operator is waiting on
# (primary_unhealthy in step 3, cntlr_unhealthy in step 4, side/leg_unhealthy in
# step 5). No wait bound in this file is computed from any of them — WAIT_REACT
# is a flat number sized by hand against react's own short set.
# sp_thresholds is the only writer; the initial value is the quiet set so that
# nothing is ever unset under `set -u`, and main overwrites it before the first
# sp is created.
THR_SET=quiet
THR_PRIMARY=$THR_QUIET_PRIMARY
THR_CNTLR=$THR_QUIET_CNTLR
THR_SIDE=$THR_QUIET_SIDE
THR_LEG=$THR_QUIET_LEG
THR=$THR_QUIET

# dnv-worker's vote loop (D9). The worker's reactions cannot fire faster than
# its vote interval, which is why every react bound is threshold + intervals.
VOTE_INTERVAL=2
VOTE_GRACE=6

# Backing files (D15): sparse, `truncate -s`, NEVER `fallocate -l` — the whole
# space argument of this suite rests on the file staying sparse while
# blkdiscard --zeroout (agent/dm.go:250-261) punches holes through the loop
# device instead of writing zeros. agent/dnagent/syncup_dn.go:505-517 only
# TAGS a disk whose write_zeroes_max_bytes is 0; it does not refuse it, so
# preflight must die on that itself, before the first sp create.
BACKING_SIZE=2G

# Space guard (D16, E2E5), asserted after every case: allocated bytes of one
# backing file, and allocated bytes of everything this run wrote on all guests.
#
# RUN_CAP_BYTES is DERIVED in derive_params, not set here, because D16's flat
# 8 GiB is below the floor of the very shape this suite exists to test. §7.8
# built that figure out of the INCREMENTAL writes — clone hydration, migration,
# the spare switch, AR6, host patterns, thin metadata — and never counted the
# storage pool's own data. The sp's sides are
# LEGS x GRP_CNT x INIT_EXT_CNT x EXTENT_SIZE, which at the default shape is
# 2 x 64 x 1 x 64 MiB = 8 GiB EXACTLY, so the cap equalled the floor and the
# guard could not pass: run 5 measured 9204092928 bytes against 8589934592 and
# failed with every per-file allocation well inside its own cap.
DN_CAP_BYTES=$((256 << 20))
RUN_CAP_BYTES=0

# The cluster and sp every case works in, and the dnvctl globals that carry
# them (ctl/root.go:187-197: --gateway-address, --cluster, --sp, --trace-id).
CLUSTER=e2e
SP=sp0

# common.NqnPrefix (common/constants.go:137) — the prefix of every NQN the
# TREE mints (side-to-cn `:2:`, migration source `:3:`, transfer `:4:`). The
# suite computes some of those (F12) and sweeps all of them in cleanup; it
# never mints one.
NQN_PREFIX=nqn.2024-01.io.dnv

# The prefix of every subsystem THIS SUITE creates with `ss create --nqn`. A
# host-facing subsystem NQN is literally that flag's string (ctl/ss.go:53 sends
# it unmunged; gateway/subsystem.go:385-387 only format-validates it), so this
# is a suite choice, not a tree format. Distinct from cdc_test.sh's
# nqn.2024-01.io.dnv-it:cdc.
NQN_IT=nqn.2024-01.io.dnv-it:e2e

# Fixed v4 uuids for the namespaces the suite creates. `ns create --uuid` is
# always passed: an empty dev_uuid makes the gateway mint a RANDOM one
# (gateway/subsystem.go:426-431), and the host resolves its device as
# /dev/disk/by-id/nvme-uuid.<uuid>, which a random value would make
# unpredictable.
UUID1=2b6f0cc9-04d2-4f1a-9c3e-1d0a5e7b8c01
UUID2=2b6f0cc9-04d2-4f1a-9c3e-1d0a5e7b8c02

# Polling budgets, in seconds. Every one of them bounds a wait_until; none of
# them is a sleep.
#
# THE BIG ONES ARE SIZED AGAINST A MEASURED WINDOW, NOT AN ESTIMATE. The plan's
# "expect ~5 min per build" is optimistic for this shape and the first real run
# proved it: the primary CN's agent log spans 8m45s (525 s) of build work and
# records 126,657 process spawns in it — 82,099 dmsetup, 20,127 mdadm, 16,795
# lsblk, about 240 a second sustained on a 2-vCPU guest.
#
# READ 525 s FOR WHAT IT IS. It is the longest build WINDOW the lab has
# produced, and not the cost of one uninterrupted build. Two failovers fired
# inside it (§7.1), and each one starts the build again from nothing on the
# other CN, so the 126,657 spawns are the sum over everything that agent did in
# those 8m45s — its own attempts, its demotion teardown, its legs. Nor is it a
# completion time: the suite had already died at its 300 s bound with the
# primary still climbing, at 21 of 32 pools and 49 of 64 groups. No
# uninterrupted 32-slice build has been timed yet, so every margin below is a
# ratio against that window, which is the only number there is to size from.
#
# The constraint is the process-spawn rate of a 2-vCPU guest; it is NOT memory
# (all three CN guests held ~2.8 GiB of 3.4 GiB free throughout, with load
# averages under 1), so the plan's §9 first fallback of raising the CN guests
# to 8 GiB addresses the wrong resource.
#
# THE LINE BETWEEN THE TWO BIG BUDGETS IS "FROM NOTHING" vs "AN INCREMENT", not
# "a cntlr stack" vs "everything else". WAIT_BUILD is a WHOLE cntlr stack built
# from nothing — and setup's sides wait, which is the other from-nothing
# convergence in the file: all 128 sides, each zeroed whole before it can be
# exported, all at once. How long that one takes has not been measured on its
# own (the first run passed it, then died in the stack wait), so it carries the
# same generous budget rather than a number nobody has. WAIT_PROVISION is an
# INCREMENTAL convergence on something that already exists, the sides a grow
# adds among them. They are separate so that a stuck td-create does not cost
# twenty minutes.
WAIT_SHORT=15          # a process to answer at all
WAIT_CP_READY=30       # the four cp daemons to serve (§7.4 step 2)
WAIT_AGENT=30          # `dn create`/`cn create` to stop returning UNAVAILABLE
WAIT_BUILD=1200        # 32 pools + 64 md arrays + 128 legs on ONE CN, from
                       # nothing — and setup's own first wait, all 128 sides
                       # blkdiscard-zeroed from nothing across the DN agents,
                       # the other from-nothing convergence in the file. 2.3x
                       # the 8m45s / 126,657-spawn window above, which leaves
                       # margin for a lab under more load and for the react
                       # case, whose build may lose a failover's worth of work
                       # and start again (§7.1's reacting set)
WAIT_PROVISION=600     # an incremental convergence on something that already
                       # exists: the handful of sides a grow adds, one grown
                       # group's md + pool reload, a td's thin volumes and
                       # raid0, one leg reconnecting. 2x the old 300 s, which
                       # was sized against the same optimistic estimate
WAIT_DELETE=900        # `sp delete` to drain to NOT_FOUND. The drain is the
                       # build run backwards on the same 2-vCPU CN — 32 pools,
                       # 64 arrays and 128 legs torn down and every side
                       # retired on its DN — so it is sized against the 525 s
                       # window, not against the old 300 s
WAIT_REACT=120         # an automatic reaction to land after its threshold.
                       # Unchanged: it is threshold + a few 5s worker passes
                       # (D9's vote loop), and every threshold it bounds is
                       # react's own short set
WAIT_HOST=60           # a host device/ANA state to appear

# How many times setup's stage 07 will wait out a whole stack before it calls
# the sp non-convergent. It is a COUNT and not a budget: each round is two
# WAIT_BUILDs that SUCCEEDED and were then invalidated by the role moving again
# (a timeout inside either one dies on the spot, so this never multiplies the
# wall clock by three). Under react's set one move during setup is ordinary —
# the first run had two — and a lab that cannot get past three is one where the
# build loses the race against primary_unhealthy every time, which is a finding
# and not a wait to sit through.
SETUP_STACK_ROUNDS=3

CASES=(smoke ops copy react)

# The cases this run will actually execute, in CASES order with --only applied.
# main fills it BEFORE setup, because setup builds the FIRST case's sp and
# sp_thresholds has to know whose sp that is.
RUN_CASES=()

# ---------------------------------------------------------------------------
# Mutable state
# ---------------------------------------------------------------------------

# Servers, from argv only (E2E1). The CN/DN/HOST arrays are 0-indexed so that
# index v is role name cn<v>/dn<v>/host<v>, which is exactly what D5's
# `dn create --location dn<v>` needs.
CP=""
CP_IP=""
CN=()
CN_IP=()
DN=()
DN_IP=()
HOST=()
HOST_IP=()

# Parsed parameters and the numbers derived from them (derive_params).
#
# GRP_CNT is the plan's "GROUPS" under another name, and the name is not a
# preference: bash's own GROUPS is a special array of the user's gids and an
# assignment to it is SILENTLY IGNORED, so `GROUPS=$((2 * SLICE_CNT))` leaves
# $GROUPS as the primary gid (1000 on these guests) and every number derived
# from it is wrong without one word of complaint. Do not reintroduce it.
SLICE_CNT=$SLICE_CNT_DEFAULT
REDUND=raid1
DNS_PER_VM_OPT=""
LEGS=0
GRP_CNT=0
DNS_PER_VM=0
DNS_PER_VM_BOUND=0
DN_TOTAL=0
TD_UNIT=0
CN_CNT=0
DN_VM_CNT=0
SP_DATA_BYTES=0
RUN_SLACK_BYTES=0

JQ=jq
ONLY=""
CLEANUP_ONLY=0
CASE=setup
TRACE=it-setup
STAGE="(startup)"
SETUP_DONE=0
QUIET=0

# Raised by cleanup_report when a guest's cleanup did not finish — a verb that
# never printed its sentinel, or an nvmet port the helper reported STUCK.
# cleanup_all clears it on entry, so it always describes the LAST sweep that
# ran, whichever that was; on_exit reads it only where that sweep was an END
# cleanup, because at the START leftovers are the point.
# A REFUSED port does NOT raise it: port_drop refuses a port
# whose trsvcid is outside this suite's band, i.e. one that was never ours to
# remove. on_exit and --cleanup-only turn it into a non-zero exit, because a
# leftover port with live ana_groups is what fails the NEXT suite's setup
# (memory note nvmet-port-teardown-ana-groups) and a WARNING in the middle of an
# hours-long transcript is not how the operator finds that out.
CLEANUP_DIRTY=0

# The verbs of the LAST sweep that never printed their sentinel, one per line
# as "<label> <verb> <rc>". cleanup_all clears it beside CLEANUP_DIRTY, so it
# too describes only the sweep that just ran.
#
# CLEANUP_DIRTY and this are not the same question. CLEANUP_DIRTY asks "is the
# lab dirty", which only matters at the END. This asks "did the sweep actually
# run", which is what the START has to know: E2E6 makes the start cleanup
# tolerant of ABSENCE — a guest with nothing on it prints its sentinel and the
# run goes on — but a verb that timed out is not absence, it is debris that
# survived, and continuing past it puts the run into a preflight failure about
# whatever the debris collides with first. On 2026-09-17 that was an nvmet
# port, and the port message blamed a refusal that had never happened.
# cleanup_start_gate turns this list into a die.
CLEANUP_UNFINISHED=""

# --timeout on every dnvctl call. dnvctl's own default is 10 s
# (ctl/root.go:58, a per-INVOCATION deadline), which is not enough for this
# suite's heaviest calls: `sp create` at 32 slices runs 2 x slice_cnt = 64 DN
# candidate scans, each a full descending range over the capacity index with
# one proto decode per DN (model/alloc.go:113-141), before it commits a
# 951-compare transaction. The default here is raised once, and a single call
# that needs more gets it from ctl_timeout without changing the file's default.
CTL_TIMEOUT_DEFAULT=30
CTL_TIMEOUT=$CTL_TIMEOUT_DEFAULT

# The three results of the last dnvctl call (ctl_exec). Globals on purpose:
# every ctl_ok/ctl_fail must run in the PARENT shell or the results are lost
# with the subshell — and a die inside a subshell would be swallowed too.
CTL_RC=""
CTL_OUT=""
CTL_ERR=""

# Cluster identity, read back from `cluster get` at setup: F12's NQNs are
# formatted from the cluster id, so nothing can be computed before this is
# filled.
CLUSTER_ID=""

# Processes this suite started on cp, by directory name (etcd, gateway, worker,
# cdc): pid recorded from $! at launch, signalled by that pid. The agents on
# the DN/CN VMs are killed from the guest helper instead (rule 3).
declare -A PID=()
declare -A RUNNING=()

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o ConnectTimeout=15 -o ServerAliveInterval=15)

# FRAME separates the sections ctl_exec brings back in one ssh. It must survive
# `echo` in the REMOTE shell unquoted, which rules out a leading `#` (that
# would start a comment) and anything the shell expands.
FRAME=@@e2e-frame@@

# ---------------------------------------------------------------------------
# Logging, assertions, failure handling (shapes from cnagent_test.sh:140-160
# and dnvctl_test.sh:163-212)
# ---------------------------------------------------------------------------

log() { echo "$*" >&2; }

# stage names the step for the failure report and mints the trace id: one id
# shared by the dnvctl call, the gateway handler, the worker and every agent os
# command it causes, so `jq 'select(.trace_id=="…")'` over any log pulls the
# whole stage.
stage() { # <nn> <text>
	STAGE="$CASE: $2"
	TRACE="it-$CASE-$1"
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
assert_le() { [ "$1" -le "$2" ] || die "$3: got $1, want <= $2"; }

assert_between() {
	{ [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]; } ||
		die "$4: got $1, want $2..$3"
}

jq_of() { printf '%s' "$1" | "$JQ" -r "$2"; }

# assert_field reads one field out of a JSON document. dnvctl renders 64-bit
# proto fields as JSON STRINGS and 32-bit ones as bare numbers (ctl/root.go:357-360,
# doc/dnvctl.md:210-213), and `byte_cnt` in the two bitmap replies is a Go int,
# so it is a number too — every comparison here is textual, through jq -r.
assert_field() { # <json> <filter> <want> <label>
	assert_eq "$(jq_of "$1" "$2")" "$3" "$4"
}

# assert_jq is the spot-check form: a `jq -e` filter that must be truthy.
assert_jq() { # <json> <filter> <label>
	printf '%s' "$1" | "$JQ" -e "$2" >/dev/null ||
		die "$3 (filter: $2, document: $1)"
}

# assert_parses runs on every successful dnvctl call: a command that renders
# unparseable JSON has failed even when every field it names is right.
assert_parses() { # <json> <label>
	printf '%s' "$1" | "$JQ" -e . >/dev/null ||
		die "$2: stdout is not one parseable JSON document: '$1'"
}

# on_exit is the only place that runs the end cleanup of a NORMAL run, and it
# runs it ONLY on success (E2E6); --cleanup-only's sweep is main's. SETUP_DONE
# guards the case where the run died before it built anything, where a full
# cleanup would just be noise.
#
# It is also where "the lab is dirty" becomes an exit code: a cleanup that did
# not finish turns a PASS into a failure, because the next suite is otherwise
# the one that discovers it. See CLEANUP_DIRTY.
#
# cleanup_all, cleanup_dirty_banner and diagnostics are defined by later
# sections of this file; they are called by name here, so their names are
# fixed.
on_exit() {
	local rc=$? dirty=0
	trap - EXIT
	# CLEANUP_DIRTY is only read where an END cleanup actually ran: cleanup_all
	# clears it on entry, so after a mid-run failure it still holds the START
	# sweep's verdict, where a warning is expected and means nothing about what
	# this run leaves behind.
	if [ "$rc" -eq 0 ] &&
		[ "$CLEANUP_ONLY" -eq 0 ] && [ "$SETUP_DONE" -eq 1 ]; then
		log ""
		log "=== end-of-run cleanup (success)"
		cleanup_all
		# cleanup_all reports and never dies, so the verdict is read here. rc
		# was 0 — every assertion passed — and it is the LAB, not the suite,
		# that fails now. E2E6 is untouched: this runs only on the path where
		# the end cleanup was already meant to happen.
		if [ "$CLEANUP_DIRTY" -eq 1 ]; then
			dirty=1
			rc=1
		fi
	elif [ "$rc" -ne 0 ] && [ "$CLEANUP_ONLY" -eq 1 ] &&
		[ "$CLEANUP_DIRTY" -eq 1 ]; then
		# --cleanup-only's own sweep is an end cleanup. The CLEANUP_DIRTY test
		# is what separates it from the other way that mode can fail (a guest
		# ship_helpers could not reach), which leaves CLEANUP_DIRTY at 0.
		dirty=1
	fi
	if [ "$rc" -eq 0 ]; then
		log ""
		log "PASS"
	else
		# The §7.9 dump is worth taking in BOTH failure shapes: after a failed
		# assertion it is the evidence, and after a failed cleanup it is the
		# list of what is still on the guests.
		log ""
		log "########## diagnostics ##########"
		diagnostics || true
		log ""
		if [ "$dirty" -eq 1 ]; then
			# Last, not first: the dump above is long, and this is the part
			# the operator has to act on. There is no failing stage to name.
			cleanup_dirty_banner
		else
			log "debris left in place on all ten guests; failing stage '$STAGE'"
			log "pull the stage's records on the guest that carries the log with:"
			log "  jq 'select(.trace_id==\"$TRACE\")' <the log under $WORK>"
		fi
	fi
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Remote execution (§7.3)
#
# One ssh helper per role family. cp needs no root at all — etcd, the gateway,
# the worker, the cdc and dnvctl are plain user processes — while the DN, CN
# and host guests need passwordless sudo for configfs, dm, md, loop devices and
# `nvme connect`. The `sudo -n bash -c $(printf '%q' …)` shape is cdc_test.sh's
# (:246-258): -n so a sudo that would prompt fails instead of hanging, and %q
# so an argument containing a space or a metacharacter is not re-split by the
# remote shell.
# ---------------------------------------------------------------------------

# ssh_to runs one command on one target. QUIET is raised while wait_until
# polls, so a poll that may run for WAIT_BUILD does not bury the transcript.
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
ssh_cp_ok() { ssh_to "$CP" "$@" || true; }

# ssh_cp_sudo exists for exactly one thing: `fstrim -a` at end cleanup (D24),
# which is root-only everywhere. Preflight does NOT require sudo on cp — cp is
# the one guest this suite can drive as a plain user — so a caller must
# tolerate its failure (ssh_cp_sudo_ok), not die on it. NO STEP OF THIS SUITE
# CALLS THE DYING FORM, and that is worth saying rather than leaving to be
# discovered: it is kept only so the pair reads like the four families below.
ssh_cp_sudo() { ssh_sudo "$CP" "$@"; }
ssh_cp_sudo_ok() { ssh_sudo "$CP" "$@" || true; }

ssh_dn() { # <v> <cmd…>
	local v=$1
	shift
	ssh_sudo "${DN[$v]}" "$@"
}
ssh_dn_ok() { ssh_dn "$@" || true; }

ssh_cn() { # <v> <cmd…>
	local v=$1
	shift
	ssh_sudo "${CN[$v]}" "$@"
}
ssh_cn_ok() { ssh_cn "$@" || true; }

ssh_host() { # <h> <cmd…>
	local h=$1
	shift
	ssh_sudo "${HOST[$h]}" "$@"
}
ssh_host_ok() { ssh_host "$@" || true; }

# ssh_host_user is ssh_host without sudo, for reads that do not need it.
ssh_host_user() { # <h> <cmd…>
	local h=$1
	shift
	ssh_to "${HOST[$h]}" "$@"
}

# helper_* invoke one verb of the shipped guest helper. EVERY pkill, and every
# command that must not be re-parsed by an ssh command string, goes through
# these (rule 3). The helper is shipped to $HELPER on each guest by the setup
# section; a guest has exactly one role, so one path suffices.
#
# Of the four `_ok` twins only helper_cp_ok has a caller today; the cleanup
# path reaches the dn, cn and host helpers through cleanup_verb instead, which
# adds the `timeout` those wrappers cannot express. The other three are kept
# for symmetry, and saying so beats leaving it to be discovered.
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

helper_host() { # <h> <verb> [args…]
	local h=$1
	shift
	ssh_host "$h" "bash $HELPER $*"
}
helper_host_ok() { helper_host "$@" || true; }

helper_cp() { # <verb> [args…]
	ssh_cp "bash $HELPER $*"
}
helper_cp_ok() { helper_cp "$@" || true; }

# ---------------------------------------------------------------------------
# Per-instance layout accessors (D10)
#
# Instance k of a DN VM, k = 0..DNS_PER_VM-1. These are the ONLY places the
# 29900/4300/k+1 arithmetic and the on-guest directory layout appear; nothing
# downstream recomputes either.
# ---------------------------------------------------------------------------

dn_grpc_port() { printf '%s' "$((DN_PORT_BASE + $1))"; }
dn_trsvcid() { printf '%s' "$((DN_TRSVCID_BASE + $1))"; }

# nvmet port ids are 1-based (common.NvmetPortId = 1 is the default of
# `dnv-agent --nvmet-port-id`, cmd/dnv-agent/main.go:88, common/constants.go:226),
# so instance 0 keeps the historical port 1 and instance k takes k+1.
dn_port_id() { printf '%s' "$(($1 + 1))"; }

# One directory per process, each holding that process's pid file, log, store
# and (on a DN) its backing file — the shape gateway_test.sh's remote_start
# expects ($WORK/<dir>/pid). The plan's §7.2 writes the cn store as
# $WORK/cn-store; it lives at $WORK/cn/store here so the cn agent has a
# directory like every other process.
dn_dir() { printf '%s/dn%s' "$WORK" "$1"; }
dn_backing() { printf '%s/dn%s/backing.img' "$WORK" "$1"; }
dn_store() { printf '%s/dn%s/store' "$WORK" "$1"; }
dn_log() { printf '%s/dn%s/agent.log' "$WORK" "$1"; }
dn_location() { printf 'dn%s' "$1"; }

cn_dir() { printf '%s/cn' "$WORK"; }
cn_store() { printf '%s/cn/store' "$WORK"; }
cn_log() { printf '%s/cn/agent.log' "$WORK"; }

# ---------------------------------------------------------------------------
# Polling (dnvctl_test.sh:300-316)
#
# wait_until runs its predicate in the PARENT shell, so a predicate that fills
# globals (ctl_try) keeps them.
# ---------------------------------------------------------------------------

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
		sleep 0.5
	done
}

# ---------------------------------------------------------------------------
# The dnvctl driver (E2E2; wrappers from dnvctl_test.sh:322-457)
#
# dnvctl runs ON cp (D28) — one ssh per call, a few hundred per run. Its three
# streams are kept separable by writing them to files on cp first: that is the
# only way "stdout is empty" and "stderr is exactly one line" can both be
# asserted, and it leaves the last call's raw output on cp for the diagnostics
# dump.
# ---------------------------------------------------------------------------

# ctl_prefix is the global argv every call carries. --rev is deliberately NOT
# in it: the revision token is PRESENCE-based (an omitted --rev sends no token
# message at all, ctl/root.go:193-194), so passing one would change what the
# gateway checks on every mutator. A case that means to test the token passes
# --rev itself.
ctl_prefix() {
	printf '%s/bin/dnvctl --gateway-address %s:%s --cluster %s --sp %s --trace-id %s --timeout %s' \
		"$WORK" "$CP_IP" "$GW_PORT" "$CLUSTER" "$SP" "$TRACE" "$CTL_TIMEOUT"
}

# ctl_timeout raises --timeout for ONE invocation and restores the default
# afterwards, whatever that invocation did:
#
#	ctl_timeout 180 ctl_ok sp create --slice-cnt "$SLICE_CNT" …
#
# It returns the wrapped call's own status, so it composes with ctl_try inside
# a wait_until as well as with ctl_ok.
ctl_timeout() { # <secs> <ctl_ok|ctl_fail|ctl_try …>
	local secs=$1 rc=0
	shift
	CTL_TIMEOUT=$secs
	"$@" || rc=$?
	CTL_TIMEOUT=$CTL_TIMEOUT_DEFAULT
	return "$rc"
}

# ctl_section splits one framed reply. Sections are 1-based in the order
# ctl_exec emits them: exit code, stderr, stdout.
ctl_section() { # <framed> <n>
	printf '%s\n' "$1" |
		awk -v want="$2" -v frame="$FRAME" '
			BEGIN { sec = 1 }
			$0 == frame { sec++; next }
			sec == want { print }
		'
}

# ctl_exec runs ONE dnvctl invocation on cp and fills the three CTL_* globals.
# It MUST run in the parent shell. Each argument is quoted for the REMOTE shell
# with printf %q: an NQN, a uuid list and a bitmap hex all reach it as one word
# only because of that.
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

# ctl_ok is the success wrapper: exit 0, EMPTY stderr (dnvctl reserves stdout
# for the reply and says nothing else on success), stdout that parses. The
# reply document stays in $CTL_OUT.
ctl_ok() { # <args…>
	ctl_exec "$@"
	assert_eq "$CTL_RC" "0" "exit code of: dnvctl $*"
	assert_eq "$CTL_ERR" "" "stderr of: dnvctl $* must be empty"
	assert_parses "$CTL_OUT" "dnvctl $*"
}

# ctl_fail is the failure wrapper (ctl/root.go:117-132): exit 1, EMPTY stdout,
# and exactly ONE stderr line of the shape
# `dnvctl: <UPPER_SNAKE CODE>: <message> (trace_id <id>)`. The message is only
# shape-checked here.
ctl_fail() { # <UPPER_SNAKE code> <args…>
	local code=$1
	shift
	ctl_exec "$@"
	assert_eq "$CTL_RC" "1" "exit code of: dnvctl $*"
	assert_eq "$CTL_OUT" "" "stdout of a failure must be empty"
	local lines matched
	# grep -c, never grep -q: `set -o pipefail` plus a reader that closes the
	# pipe early turns a SIGPIPE on the writer into a script abort.
	lines=$(printf '%s\n' "$CTL_ERR" | grep -c . || true)
	assert_eq "$lines" "1" "stderr of a failure must be exactly one line"
	matched=$(printf '%s\n' "$CTL_ERR" |
		grep -c "^dnvctl: $code: .* (trace_id $TRACE)\$" || true)
	assert_ne "$matched" "0" \
		"stderr must be 'dnvctl: $code: … (trace_id $TRACE)', got '$CTL_ERR'"
}

# ctl_fail_msg is ctl_fail for a failure whose whole message is predictable.
# Against a REAL gateway that is rare — most messages carry ids — so
# ctl_fail_grep below is usually the right one, and in the end it is the only
# one: NO STEP OF THIS SUITE CALLS ctl_fail_msg today. It is kept because it
# is the stricter assertion of the pair and the next refusal to be covered may
# have a fixed message; ops step 3's comment already names it as the form it
# cannot use.
ctl_fail_msg() { # <UPPER_SNAKE code> <message> <args…>
	local code=$1 message=$2
	shift 2
	ctl_fail "$code" "$@"
	assert_eq "$CTL_ERR" "dnvctl: $code: $message (trace_id $TRACE)" \
		"the whole stderr line"
}

# ctl_fail_grep is ctl_fail plus a FIXED-STRING substring of the message, for
# the gateway's refusals whose text names an id the suite cannot predict. It is
# this suite's own helper: dnvctl_test.sh has only the shape check and the
# whole-line equality, and neither can express "the message says which slot".
ctl_fail_grep() { # <UPPER_SNAKE code> <substring> <args…>
	local code=$1 want=$2
	shift 2
	ctl_fail "$code" "$@"
	local hit
	hit=$(printf '%s\n' "$CTL_ERR" | grep -cF -- "$want" || true)
	assert_ne "$hit" "0" \
		"the failure message must contain '$want', got '$CTL_ERR'"
}

# ctl_try is the polling form: it fills the CTL_* globals and returns the call's
# success as an exit status instead of dying, which is what wait_until needs.
ctl_try() { # <args…>
	ctl_exec "$@"
	[ "$CTL_RC" = 0 ]
}

# ---------------------------------------------------------------------------
# Driver tooling: jq and the Go constants
# ---------------------------------------------------------------------------

need_local() {
	command -v "$1" >/dev/null 2>&1 || die "missing: $1 on the driver"
}

# resolve_jq picks the driver's JSON parser exactly as the other six suites do:
# a system jq if there is one, else the gojq drop-in built into the gitignored
# integtest/bin with the Go toolchain the driver already needs. Every filter in
# this file is written to the intersection of the two (no jq-only builtins), and
# every filter runs on the DRIVER — no guest is ever asked for a jq.
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

# read_constants is how this shell reads a Go value instead of copying it
# (worker_test.sh:955-968). `workerctl constants` opens no etcd client and
# reads no key, so it runs LOCALLY on the binary preflight has just built. It
# must run AFTER resolve_jq and AFTER parse_args, because it also cross-checks
# LEGS.
read_constants() {
	local json
	json=$("$WORKERCTL_BIN" constants) ||
		die "\`workerctl constants\` failed: this suite reads" \
			"common.EtcdMaxTxnOps from it and must not guess it"
	ETCD_MAX_TXN_OPS=$(jq_of "$json" .EtcdMaxTxnOps)
	case "$ETCD_MAX_TXN_OPS" in
	'' | *[!0-9]*)
		die "workerctl constants: EtcdMaxTxnOps is" \
			"'$ETCD_MAX_TXN_OPS' in $json"
		;;
	esac
	MAX_ALLOC_LEG_PER_GRP=$(jq_of "$json" .MaxAllocLegPerGrp)
	case "$MAX_ALLOC_LEG_PER_GRP" in
	'' | *[!0-9]*)
		die "workerctl constants: MaxAllocLegPerGrp is" \
			"'$MAX_ALLOC_LEG_PER_GRP' in $json"
		;;
	esac
	# gateway/alloc.go:32-37 returns common.MaxAllocLegPerGrp for an md-raid1
	# group and 1 otherwise. If that constant ever moves, every number derived
	# from LEGS — the DN picks, DNS_PER_VM, the create's compare count — is
	# wrong, and the suite would fail much later with an unrelated message.
	if [ "$REDUND" = raid1 ] && [ "$LEGS" != "$MAX_ALLOC_LEG_PER_GRP" ]; then
		die "LEGS is $LEGS but common.MaxAllocLegPerGrp is" \
			"$MAX_ALLOC_LEG_PER_GRP: re-derive the §7.1 numbers"
	fi
	log "  --max-txn-ops = common.EtcdMaxTxnOps = $ETCD_MAX_TXN_OPS"
}

# ---------------------------------------------------------------------------
# Command line (§7.1, E2E1)
# ---------------------------------------------------------------------------

usage() {
	cat >&2 <<EOF
usage: bash integtest/e2e_test.sh [--only <case>] [--cleanup-only]
           [--slice-cnt N] [--redund raid1|none] [--dns-per-vm N]
           --cp user@ip --cn user@ip … --dn user@ip … --host user@ip …

roles (repeatable flags; nothing is positional, nothing is hardcoded):
  --cp    exactly 1   etcd + dnv-gateway + dnv-worker + dnv-cdc + dnvctl
                      (no sudo needed)
  --cn    at least 3  one \`dnv-agent cn\` each; $CNTLR_CNT carry the sp's
                      cntlrs and the rest is the spare AR7 lands on
                      (passwordless sudo)
  --dn    at least LEGS (2 for raid1, 1 for none), and in practice 4:
                      \$DNS_PER_VM \`dnv-agent dn\` instances each
                      (passwordless sudo)
  --host  exactly 2   real kernel NVMe hosts, reached only through the cdc
                      (passwordless sudo)

parameters:
  --slice-cnt N       slices per sp (default $SLICE_CNT_DEFAULT =
                      common.MaxSliceCntPerSp; 1..$SLICE_CNT_DEFAULT)
  --redund raid1|none redundancy of every group (default raid1)
  --dns-per-vm N      dnagents per DN VM; the default is the placement bound
                      ceil(legs x 2 x slice_cnt / (V - legs + 1)) over the
                      V --dn guests, 43 for the default shape on 4 DN VMs.
                      Below the bound is allowed and warns.
  --only <case>       run one case: ${CASES[*]}
  --cleanup-only      run the start cleanup on every guest and stop

The ten guests must be ten DIFFERENT machines, and no other dnv suite may run
anywhere in the lab while this one does.
EOF
	exit 2
}

# parse_args follows the argument idiom of cnagent_test.sh:2906-2942 and
# cdc_test.sh:2300-2341, extended to repeatable role flags. Both `--flag value`
# and `--flag=value` are accepted for every flag; anything else, including any
# positional word, is a usage error.
parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--cp)
			# Exactly one cp: a second one would otherwise win silently and
			# the run would drive a control plane on a guest the operator
			# thought was idle.
			{ [ $# -ge 2 ] && [ -z "$CP" ]; } || usage
			CP=$2
			shift 2
			;;
		--cp=*)
			[ -z "$CP" ] || usage
			CP=${1#*=}
			shift
			;;
		--cn)
			[ $# -ge 2 ] || usage
			CN+=("$2")
			shift 2
			;;
		--cn=*)
			CN+=("${1#*=}")
			shift
			;;
		--dn)
			[ $# -ge 2 ] || usage
			DN+=("$2")
			shift 2
			;;
		--dn=*)
			DN+=("${1#*=}")
			shift
			;;
		--host)
			[ $# -ge 2 ] || usage
			HOST+=("$2")
			shift 2
			;;
		--host=*)
			HOST+=("${1#*=}")
			shift
			;;
		--only)
			[ $# -ge 2 ] || usage
			ONLY=$2
			shift 2
			;;
		--only=*)
			ONLY=${1#*=}
			shift
			;;
		--cleanup-only)
			CLEANUP_ONLY=1
			shift
			;;
		--slice-cnt)
			[ $# -ge 2 ] || usage
			SLICE_CNT=$2
			shift 2
			;;
		--slice-cnt=*)
			SLICE_CNT=${1#*=}
			shift
			;;
		--redund)
			[ $# -ge 2 ] || usage
			REDUND=$2
			shift 2
			;;
		--redund=*)
			REDUND=${1#*=}
			shift
			;;
		--dns-per-vm)
			[ $# -ge 2 ] || usage
			DNS_PER_VM_OPT=$2
			shift 2
			;;
		--dns-per-vm=*)
			DNS_PER_VM_OPT=${1#*=}
			shift
			;;
		-h | --help) usage ;;
		*) usage ;;
		esac
	done

	# --redund decides LEGS, and LEGS decides how many --dn guests are the
	# minimum, so the arms are checked before the counts.
	case "$REDUND" in
	raid1) LEGS=2 ;; # common.MaxAllocLegPerGrp, cross-checked in read_constants
	none) LEGS=1 ;;  # gateway/alloc.go:32-37 returns 1 for every other arm
	*) usage ;;
	esac

	case "$SLICE_CNT" in
	'' | *[!0-9]*) usage ;;
	esac
	# gateway/storagepool.go:346-349 refuses slice_cnt outside
	# [1, common.MaxSliceCntPerSp]; SLICE_CNT_DEFAULT mirrors that ceiling, so
	# an out-of-range value is caught here instead of 200 lines into setup.
	{ [ "$SLICE_CNT" -ge 1 ] && [ "$SLICE_CNT" -le "$SLICE_CNT_DEFAULT" ]; } || usage

	if [ -n "$DNS_PER_VM_OPT" ]; then
		case "$DNS_PER_VM_OPT" in
		'' | *[!0-9]*) usage ;;
		esac
		[ "$DNS_PER_VM_OPT" -ge 1 ] || usage
	fi

	[ -n "$CP" ] || usage
	CN_CNT=${#CN[@]}
	DN_VM_CNT=${#DN[@]}
	[ "$CN_CNT" -ge 3 ] || usage
	[ "$DN_VM_CNT" -ge "$LEGS" ] || usage
	[ "${#HOST[@]}" -eq 2 ] || usage

	local t
	for t in "$CP" "${CN[@]}" "${DN[@]}" "${HOST[@]}"; do
		[ -n "${t##*@}" ] || usage
	done

	# Ten roles on nine machines would look like a working run and then fail
	# in ways nobody can read: two agents of different roles sharing a kernel,
	# a cleanup wiping another role's $WORK, a port bound twice.
	local dup
	dup=$(printf '%s\n' "$CP" "${CN[@]}" "${DN[@]}" "${HOST[@]}" |
		sort | uniq -d | tr '\n' ' ')
	[ -z "$dup" ] || die "the same target is used for two roles: $dup"

	CP_IP=${CP##*@}
	local i
	CN_IP=()
	for i in "${!CN[@]}"; do CN_IP[i]=${CN[$i]##*@}; done
	DN_IP=()
	for i in "${!DN[@]}"; do DN_IP[i]=${DN[$i]##*@}; done
	HOST_IP=()
	for i in "${!HOST[@]}"; do HOST_IP[i]=${HOST[$i]##*@}; done

	if [ -n "$ONLY" ]; then
		local known=0 name
		for name in "${CASES[@]}"; do
			[ "$name" = "$ONLY" ] && known=1
		done
		[ "$known" -eq 1 ] || usage
	fi

	derive_params
}

# derive_params computes every number the rest of the suite uses from the
# parsed parameters. It is called at the end of parse_args, before anything
# touches a guest.
derive_params() {
	# Two groups per slice — one meta, one data — for every slice, from
	# planSpGroups (gateway/storagepool.go:254-263), and LEGS sides per group.
	GRP_CNT=$((2 * SLICE_CNT))

	# The space guard's run cap, derived from the shape rather than D16's flat
	# 8 GiB (see RUN_CAP_BYTES's comment for why that could never pass).
	#
	# SP_DATA_BYTES is the floor: every one of the LEGS*GRP_CNT sides is
	# INIT_EXT_CNT extents of EXTENT_SIZE, and a raid1 leg's initial resync
	# writes its whole data area, so those extents MATERIALISE in the backing
	# file however sparse it started. That is not waste and not a leak — it is
	# the storage pool.
	#
	# RUN_SLACK_BYTES is everything else on the ten guests: the cp's etcd and
	# four daemon logs, 175 agent logs, the CN thin metadata and md bitmaps,
	# the host pattern files, and each case's own writes, which §7.8 bounds at
	# under 1 GiB. Run 5 measured 586 MiB of it after the smoke case, the
	# heaviest contributors being the cp at 296 MiB and cn0 at 79 MiB; 4 GiB
	# leaves room for the copy and react cases, which write more, while still
	# being a number a real leak would cross.
	SP_DATA_BYTES=$((LEGS * GRP_CNT * INIT_EXT_CNT * EXTENT_SIZE))
	RUN_SLACK_BYTES=$((4 << 30))
	RUN_CAP_BYTES=$((SP_DATA_BYTES + RUN_SLACK_BYTES))

	# Every one of the LEGS*GRP_CNT sides lands on a DISTINCT disk node: the
	# create's black list starts as the request's and grows with every pick
	# (gateway/storagepool.go:393-403), so 128 sides at the default shape need
	# 128 DNs that have never been picked.
	#
	# A scan returns at most ONE candidate per LOCATION (model/alloc.go:137-141),
	# and D5 makes a DN VM's location its role name, so a group draws its LEGS
	# sides from LEGS DIFFERENT VMs and a pick fails the moment fewer than LEGS
	# VMs still hold an unpicked DN (gateway/alloc.go:99-103).
	#
	# COUNTING ARGUMENT for the bound. Emptying (V - LEGS + 1) VMs takes
	# (V - LEGS + 1) x N picks. Only LEGS*GRP_CNT - LEGS picks happen before the
	# last group's scan. So if (V - LEGS + 1) x N > LEGS*GRP_CNT - LEGS, no scan
	# can ever see fewer than LEGS VMs with a DN left, and the create cannot be
	# starved. N = ceil(LEGS*GRP_CNT / (V - LEGS + 1)) satisfies that with at
	# most one DN per VM to spare — the exact minimum is
	# ceil((LEGS*GRP_CNT - LEGS + 1) / (V - LEGS + 1)) — and both give 43 for
	# 4 VMs, 32 slices, raid1. (Do NOT justify this with a "two-VM tail"
	# argument: when two VMs remain both are drawn every step, so they drain in
	# lockstep and the one that entered the tail behind stays behind. Lockstep
	# preserves the difference, it does not close it; the counting argument
	# above is the whole proof.)
	local divisor=$((DN_VM_CNT - LEGS + 1))
	DNS_PER_VM_BOUND=$(((LEGS * GRP_CNT + divisor - 1) / divisor))

	if [ -n "$DNS_PER_VM_OPT" ]; then
		DNS_PER_VM=$DNS_PER_VM_OPT
		if [ "$DNS_PER_VM" -lt "$DNS_PER_VM_BOUND" ]; then
			# E2E3: below the bound is a warning, not an error — but say what
			# it risks. At exactly LEGS*GRP_CNT/V per VM (32 here) a simulation
			# of the real algorithm fails about four creates in five, and a run
			# that does succeed leaves every VM at zero free DNs, so every
			# later migration destination and spare leg has nowhere anti-affine
			# to go: FindDnCandidatesAntiAffine then RELAXES (model/alloc.go:202-228)
			# and can put both sides of one leg on one kernel, where
			# SideToCnNqn — which carries no dn_id (common/name_fmt.go:541-557)
			# — collides between the two agents.
			log "WARNING: --dns-per-vm $DNS_PER_VM is below the placement" \
				"bound $DNS_PER_VM_BOUND for $DN_VM_CNT DN VMs,"
			log "         $SLICE_CNT slices and $REDUND. The create may fail" \
				"RESOURCE_EXHAUSTED, and a create that"
			log "         succeeds may leave no DN for a migration or spare" \
				"leg, which lets the allocator put two"
			log "         sides of one leg on one kernel."
		fi
	else
		DNS_PER_VM=$DNS_PER_VM_BOUND
	fi

	# The cap is a per-kernel sanity limit (D10), and it is also what keeps the
	# DN gRPC block below CN_GRPC_PORT. More VMs is the answer: the divisor
	# above grows with V, so the bound falls.
	if [ "$DNS_PER_VM" -gt "$MAX_DNS_PER_VM" ]; then
		die "$DNS_PER_VM dnagents per VM exceeds MAX_DNS_PER_VM" \
			"($MAX_DNS_PER_VM): this shape needs $((LEGS * GRP_CNT))" \
			"distinct DNs spread over $DN_VM_CNT DN VM(s) —" \
			"add --dn guests"
	fi

	DN_TOTAL=$((DN_VM_CNT * DNS_PER_VM))

	# gateway/thindevice.go:154-171: a td's size must be a positive multiple of
	# slice_cnt x stripe_size, computed from the SP's stored values. Every
	# `td create --size` in this suite is a multiple of TD_UNIT.
	TD_UNIT=$((SLICE_CNT * STRIPE_SIZE))
}

# ---------------------------------------------------------------------------
# Derived names (F12) — the bash mirror of common/name_fmt.go
#
# Every formatter below was re-derived from common/name_fmt.go in THIS tree.
# The kind digit is a constant there, not a literal
# (common/name_fmt.go:31-35: nqnKindDnHost = 0x0, nqnKindCnHost = 0x1,
# nqnKindSideToCn = 0x2, nqnKindMigrSrc = 0x3, nqnKindXfer = 0x4), printed with
# %01x; every id is printed with %016x. The prefix is nf.nqnPrefix, which
# NewNameFmt loads unconditionally from common.NqnPrefix
# (common/name_fmt.go:57-67) — no flag and no request field moves it — so
# $NQN_PREFIX is the whole story, and it is the same literal
# integtest/cnagent_test.sh:81 carries.
#
# THE ARGUMENT ORDER IS NOT UNIFORM, and that is the easy mistake here:
#
#   DnHostNqn   (cluster, dn)                common/name_fmt.go:511-522
#   CnHostNqn   (cluster, cn)                common/name_fmt.go:524-535
#   SideToCnNqn (cluster, sp, LEG, cn)       common/name_fmt.go:541-556
#   MigrSrcNqn  (cluster, DN, sp, migr)      common/name_fmt.go:558-573
#   XferNqn     (cluster, sp, xfer)          common/name_fmt.go:575-588
#
# SideToCnNqn keys on leg_id and carries NO dn id — its comment at :537-540
# says why: both sides of a migrating leg export the same subsystem NQN from
# their two DNs, so the CN's kernel aggregates them into one multipath
# namespace and ANA picks the live path. MigrSrcNqn does carry a dn id, and
# puts it BEFORE the sp id. Each wrapper below takes its own Go function's
# arguments in its own Go function's order, minus the cluster id, which is a
# run-wide constant read from $CLUSTER_ID.
#
# The suite never MINTS one of these: where it uses one, it computes it to
# discover, connect, grep and sweep what the agents minted. Three of the five
# are used today — side_to_cn_nqn, cn_host_nqn and xfer_nqn; dn_host_nqn and
# migr_src_nqn have no caller, and each carries a line saying so rather than
# leaving it to be found. A host-facing subsystem NQN is a
# different thing entirely — it is literally the `ss create --nqn` string
# (ctl/ss.go:53 sends it unmunged; gateway/subsystem.go:385-387 only
# format-validates it), which is why $NQN_IT is a suite choice and these are
# not.
# ---------------------------------------------------------------------------

# Every NQN above starts with the cluster id, and the cluster id only exists
# after `cluster create`. Setup fills CLUSTER_ID from `cluster get`; computing
# a name before that would silently produce a well-formed NQN for cluster 0.
require_cluster_id() {
	[ -n "$CLUSTER_ID" ] ||
		die "CLUSTER_ID is empty: setup must read it from \`cluster get\`" \
			"before any NQN is computed"
}

# hex16 renders one id the way every Go formatter does, with %016x. Its input
# is a DECIMAL id, which is exactly what dnvctl prints: protojson renders a
# uint64 proto field as a JSON STRING of decimal digits and a uint32 as a bare
# number (ctl/root.go:357-360, doc/dnvctl.md:210-213), so an id arrives here as
# text either way and is handed to printf as text.
#
# It is deliberately NOT `printf '%016x' "$(($1))"` (cnagent_test.sh:417): that
# suite's ids are small literals it chose itself, but a cluster id is a 64-bit
# fnv1a of name+epoch (model/keys.go:112-119) and has an even chance of landing
# above 2^63, where bash's signed arithmetic wraps to a negative number. Both
# forms print the same 16 hex digits on bash 5.2, but only the direct one never
# represents the value as negative on the way.
#
# The guards are not decoration. bash's printf parses a numeric argument in
# BASE 0, so "010" would be octal 8 and "0x10" hexadecimal 16 — a leading zero
# is refused rather than silently reinterpreted. And a non-numeric argument
# (the usual cause: a jq filter that matched nothing and returned null) makes
# printf emit 0000000000000000, which looks exactly like a real name for
# cluster 0. hex16 cannot die usefully — every caller reads it through $( ),
# where `exit` ends only the subshell — so it logs the bug and returns a poison
# string instead.
#
# Nothing downstream re-checks that string: common.ValidNqnPattern is
# `^nqn\.\d{4}-(0[1-9]|1[0-2])\.[A-Za-z0-9\.-]+:.+$` (common/constants.go:9),
# whose tail is `.+`, so an NQN carrying the poison still passes the gateway's
# validateNqn (gateway/validate.go:72-84). What makes it findable is the log
# line naming the caller plus a name that then matches nothing on any guest —
# which is still far better than a silent cluster-0 name that matches the
# WRONG thing.
hex16() { # <decimal id>
	case "$1" in
	'')
		log "BUG: hex16 got an empty id — a jq filter probably matched nothing"
		printf 'notanid-empty'
		return 1
		;;
	*[!0-9]*)
		log "BUG: hex16 got '$1', which is not a decimal id" \
			"(dnvctl renders a uint64 as a decimal JSON string)"
		printf 'notanid-%s' "$1"
		return 1
		;;
	0) ;;
	0*)
		log "BUG: hex16 got '$1': a leading zero makes bash's printf read it" \
			"as octal"
		printf 'notanid-%s' "$1"
		return 1
		;;
	esac
	printf '%016x' "$1"
}

# NO STEP OF THIS SUITE CALLS IT: a DN is an nvme HOST only while it is a
# migration destination, and nothing here greps for that connection by name.
dn_host_nqn() { # <dn_id>
	require_cluster_id
	printf '%s:0:%s:%s' "$NQN_PREFIX" "$(hex16 "$CLUSTER_ID")" "$(hex16 "$1")"
}

cn_host_nqn() { # <cn_id>
	require_cluster_id
	printf '%s:1:%s:%s' "$NQN_PREFIX" "$(hex16 "$CLUSTER_ID")" "$(hex16 "$1")"
}

side_to_cn_nqn() { # <sp_id> <leg_id> <cn_id>
	require_cluster_id
	printf '%s:2:%s:%s:%s:%s' "$NQN_PREFIX" "$(hex16 "$CLUSTER_ID")" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")"
}

# NO STEP OF THIS SUITE CALLS IT: the copy case's migration is asserted
# through `sp inspect-side` and the side lists, never by looking the source
# export up by NQN on the DN. The dn cleanup sweeps the :3: family by prefix.
migr_src_nqn() { # <dn_id> <sp_id> <migr_id>
	require_cluster_id
	printf '%s:3:%s:%s:%s:%s' "$NQN_PREFIX" "$(hex16 "$CLUSTER_ID")" \
		"$(hex16 "$1")" "$(hex16 "$2")" "$(hex16 "$3")"
}

xfer_nqn() { # <sp_id> <xfer_id>
	require_cluster_id
	printf '%s:4:%s:%s:%s' "$NQN_PREFIX" "$(hex16 "$CLUSTER_ID")" \
		"$(hex16 "$1")" "$(hex16 "$2")"
}

# host_id mirrors common.NvmeHostId (common/name_fmt.go:726-731):
# sha256("dnv-hostid:" + hostnqn), rendered 8-4-4-4-12. It runs on the DRIVER —
# `cnagentctl host-id --hostnqn <nqn>` (integtest/cnagentctl/main.go:747-757) is
# a pure function of its argument that opens no client and reads no key — and
# that one subcommand is the only reason this suite builds cnagentctl.
#
# It is NOT what the two hosts connect with: D13 gives them their own
# /etc/nvme/hostnqn, whose partner id is their own /etc/nvme/hostid (see
# read_host_identity below). host_id is for a connect made under a DNV-minted
# nqn, where rule 2 forbids leaving --hostid implicit: the kernel keeps a
# strict 1:1 hostnqn<->hostid map and refuses a second hostnqn under a known
# hostid with EINVAL, and nvme-cli fills an omitted --hostid from the node-wide
# /etc/nvme/hostid.
#
# NO STEP OF THIS SUITE CALLS IT TODAY, and that is worth saying rather than
# leaving to be discovered: every `nvme connect`/`connect-all` here — host0 to
# $SS0, host1 to $XNQN, host1 to the fallback source — passes the host's OWN
# ${HOST_NQN[h]}/${HOST_HOSTID[h]}, and the only connects made under a minted
# nqn are the CN's and the DN's, which the agents make for themselves. It stays
# because it is the one piece a step that connects a HOST under a DNV-minted
# nqn would need, and because preflight_driver builds cnagentctl anyway (the
# build is ~2 s of a package that is already in this repo).
host_id() { # <hostnqn>
	"$CNAGENTCTL_BIN" host-id --hostnqn "$1"
}

# ---------------------------------------------------------------------------
# Host identity (D13)
#
# Both hosts reach their namespaces as themselves: the nqn in
# /etc/nvme/hostnqn, which is what `ss set-hosts --hosts` must name, and the id
# in /etc/nvme/hostid, which every `nvme connect`/`connect-all` passes
# explicitly (rule 2). cdc_test.sh:1644-1655 is the model — it generates both
# files when they are absent and asserts the two hosts differ — and the guest
# preflight section owns that generation. Everything here only READS.
#
# HOST_NQN and HOST_HOSTID are 0-indexed like HOST[]: host0's identity is
# ${HOST_NQN[0]} / ${HOST_HOSTID[0]}. They are arrays, so a bare $HOST_NQN
# would silently expand to element 0 — always subscript them.
# ---------------------------------------------------------------------------

HOST_NQN=()
HOST_HOSTID=()

read_host_identity() { # <h>
	local h=$1 out nqn hid
	# One ssh, one line per file, in argv order: awk's print always terminates
	# its line, so a file with no trailing newline cannot merge the two values
	# the way `cat` would.
	out=$(ssh_host_ok "$h" "awk 'FNR==1{print}' /etc/nvme/hostnqn /etc/nvme/hostid")
	nqn=$(printf '%s\n' "$out" | sed -n 1p)
	hid=$(printf '%s\n' "$out" | sed -n 2p)
	case "$nqn" in
	nqn.*) ;;
	*)
		die "host$h: /etc/nvme/hostnqn reads '$nqn', which is not an nqn." \
			"Guest preflight must create it:" \
			"test -s /etc/nvme/hostnqn || nvme gen-hostnqn > /etc/nvme/hostnqn"
		;;
	esac
	[ -n "$hid" ] ||
		die "host$h: /etc/nvme/hostid is empty. Guest preflight must create" \
			"it: test -s /etc/nvme/hostid || uuidgen > /etc/nvme/hostid"
	HOST_NQN[h]=$nqn
	HOST_HOSTID[h]=$hid
	log "  host$h identity: $nqn / $hid"
}

# ---------------------------------------------------------------------------
# Device resolution on a host (F12)
#
# A namespace's dev_uuid is RANDOM unless `ns create --uuid` supplies one: the
# gateway mints an RFC 4122 v4 uuid for an empty dev_uuid
# (gateway/subsystem.go:426-431, newDevUuid at :83-101). So the suite always
# passes --uuid $UUID1/$UUID2 and resolves the device by that uuid — the by-id
# idiom of cnagent_test.sh:458 and cdc_test.sh:719.
#
# The multipath head node under /dev/disk/by-id is the only name to use. The
# per-controller path device (nvme<X>c<Y>n<Z>) is hidden and has no /dev node,
# and the head's own /dev/nvme<X>n<Y> number is assigned in discovery order, so
# it moves between runs and between reconnects.
# ---------------------------------------------------------------------------

host_dev() { # <uuid>
	printf '/dev/disk/by-id/nvme-uuid.%s' "$1"
}

# host_dev_present is the predicate; wait_dev and wait_dev_gone are the bounded
# waits. `test -e` FOLLOWS the symlink, so what it answers is "the head disk
# exists", not "a dangling link exists".
#
# THE TWO DIRECTIONS ARE NOT SYMMETRIC, and getting that backwards costs a
# whole run in timeouts:
#
#  - APPEARING. A namespace whose only path has never been usable gets no head
#    disk at all — the multipath head is added the first time a path goes live
#    — so setup must wait_dev AFTER the path's ANA state is optimized, not
#    before. (Measured in this lab, memory note ana-inaccessible-ns-no-blockdev.)
#  - DISAPPEARING. An ANA change does NOT take away a head disk that already
#    exists. `ns set-suspended` and an xfer's --auto-suspend are a PARK, not a
#    removal: the cn agent keeps the nvmet namespace, points the ns-dev's table
#    at the td's dm-error and moves the namespace to the inaccessible ANA group
#    (agent/cnagent/plan.go:307-315 "a **park**, not a dm suspension … and the
#    device stays live", :781-790). The host keeps its node and requeues IO.
#
# So wait_dev_gone is for `ns delete`, where the nvmet namespace really goes
# away. After a SUSPEND, assert with host_wait_ana … inaccessible — never with
# wait_dev_gone, which would simply run out its budget and die.
host_dev_present() { # <h> <uuid>
	ssh_host "$1" "test -e $(host_dev "$2")"
}

host_dev_absent() { # <h> <uuid>
	! host_dev_present "$1" "$2"
}

wait_dev() { # <h> <uuid> [secs]
	wait_until "${3:-$WAIT_HOST}" \
		"host$1 to see $(host_dev "$2")" \
		host_dev_present "$1" "$2"
}

# wait_dev_gone is the other direction — after `ns delete` or an `ss delete`
# that takes the namespace with it, NOT after a suspend (see above). It is
# deliberately not named wait_gone: that
# name belongs to the process-control copy from gateway_test.sh:508-511, which
# waits for a pid.
wait_dev_gone() { # <h> <uuid> [secs]
	wait_until "${3:-$WAIT_HOST}" \
		"host$1 to lose $(host_dev "$2")" \
		host_dev_absent "$1" "$2"
}

# ---------------------------------------------------------------------------
# NVMe paths and ANA state (cnagent_test.sh:741-789, with three changes)
#
# Change 1: the parsing runs on the DRIVER. cnagent_test.sh's path_field is a
# GUEST function that pipes `nvme list-subsys -o json` through the guest's jq;
# this suite asks no guest for a jq (resolve_jq builds one for the driver
# only), so the json crosses the ssh and "$JQ" reads it here. The filter is
# cnagent_test.sh:752-755 with two additions: change 2 below, and `.Address //
# ""` so a path object without an Address is skipped instead of aborting jq.
#
# Change 2: the selector takes a trsvcid as well as a traddr. On a CN, two
# paths of one subsystem can share a traddr and differ only in trsvcid — a DN
# VM runs DNS_PER_VM agents on one IP, each with its own service id (D10) —
# which cnagent_test.sh, with one agent per VM, never had to separate. Pass ""
# to accept any trsvcid; that is the host case, where one CN VM runs one cn
# agent on $CN_TRSVCID.
#
# Change 3: ana_of selects the NAMESPACE. See its own comment.
# ---------------------------------------------------------------------------

# subsys_json_of normalises `nvme list-subsys -o json` into something jq can
# read: the command prints nothing at all when the node holds no controller,
# and an empty string is not JSON.
subsys_json_of() { # <raw>
	if [ -z "${1//[[:space:]]/}" ]; then
		printf '[]'
	else
		printf '%s' "$1"
	fi
}

host_subsys_json() { # <h>
	subsys_json_of "$(ssh_host_ok "$1" "nvme list-subsys -o json 2>/dev/null")"
}

cn_subsys_json() { # <v>
	subsys_json_of "$(ssh_cn_ok "$1" "nvme list-subsys -o json 2>/dev/null")"
}

# path_field reads one field of the path a node holds to one target, e.g. State
# (live/connecting/resetting/deleting) or Name (the nvme<X> controller). It
# answers "none" when there is no such path — never an empty string, so an
# assert_eq failure says which of the two it got.
#
# A path's Address is a comma-joined key=value list
# ("traddr=…,trsvcid=…,src_addr=…"); the filter compares the SINGLETON LIST of
# its traddr= element against the wanted one, which is an exact match that
# needs no anchoring. Every construct here is in the jq/gojq intersection.
path_field() { # <json> <nqn> <traddr> <trsvcid|""> <field>
	printf '%s' "$1" | "$JQ" -r \
		--arg nqn "$2" --arg a "$3" --arg s "$4" --arg f "$5" '
		[ .. | objects | select(has("NQN") and .NQN == $nqn) | .Paths[]?
		  | select([(.Address // "") | split(",")[]
		            | select(startswith("traddr="))] == ["traddr=" + $a])
		  | select($s == "" or
		           ([(.Address // "") | split(",")[]
		             | select(startswith("trsvcid="))] == ["trsvcid=" + $s]))
		  | .[$f] ] | first // "none"'
}

host_path_field() { # <h> <nqn> <traddr> <field>
	path_field "$(host_subsys_json "$1")" "$2" "$3" "" "$4"
}

# host_path_state is "live" once the controller is connected and "connecting"
# while it retries. It is NOT an ANA state and says nothing about whether any
# particular namespace is reachable.
host_path_state() { # <h> <nqn> <traddr>
	host_path_field "$1" "$2" "$3" State
}

host_ctrl() { # <h> <nqn> <traddr>   → nvme<X>, or "none"
	host_path_field "$1" "$2" "$3" Name
}

cn_path_field() { # <v> <nqn> <traddr> <trsvcid> <field>
	path_field "$(cn_subsys_json "$1")" "$2" "$3" "$4" "$5"
}

# ANA_LAST carries the state the last host_ana_is/cn_ana_is saw. wait_until's
# timeout message names the wait, not the observation, so this is where the
# failure report learns what was actually there.
#
# It is READ only by the diagnostics dump, and written in three ways: the two
# *_ana_is predicates set it from a reading, the two *_ctrl_is_present ones
# CLEAR it (they take no reading, and a stale value beside their fresh
# ANA_CTRL would be a claim nobody measured), and nothing else touches it — in
# particular there is no per-stage or per-case reset, which is why the clear
# has to live in the predicate.
#
# The *_ana_is form is `ANA_LAST=$(ana_of …)`: an assignment keeps its value
# in the shell that ran
# it even though the right-hand side is a subshell, and wait_until runs its
# predicate in the PARENT shell, so the value survives the timeout. That is
# also why the predicates call ana_of directly rather than through a probe
# function of their own — every caller would read such a function through $( ),
# where the assignment would die with the subshell.
ANA_LAST=""

# ANA_CTRL is the CONTROLLER the same predicate resolved, or "none" when there
# was not one. It exists because "none" is an answer ana_of gives for two
# different faults — no controller at all, and a controller whose namespaces do
# not include this uuid — and collapsing them is what cost run 3 (2026-09-17)
# its diagnosis: setup step 10 died with "timed out waiting for host0 ANA
# 'optimized' … via 192.168.122.77" when host0 held NO CONTROLLER AT ALL, and
# the message sent the reader to ANA and to the cdc, neither of which had
# anything wrong with it. host_wait_ana/cn_wait_ana split the two waits so each
# fault gets its own sentence; this global is what the diagnostics dump reads.
# Filled by the same *_is predicates and under the same rule as ANA_LAST.
ANA_CTRL=""

# ana_of reads the ANA state of ONE namespace on ONE controller.
#
# It has to come from sysfs: nvme-cli 2.16's `list-subsys -o json` carries no
# ANA state (only `show-topology` does, and that one is keyed by namespace and
# needs a device argument), which is why cnagent_test.sh:762-775 reads the
# attribute directly too.
#
# The ADDITION is the namespace selector. cnagent_test.sh takes the first
# readable /sys/class/nvme/<ctrl>/nvme*n*/ana_state, which is exactly right
# with one namespace per controller and wrong the moment there are two — and
# the copy and react cases put ns idx 1 and ns idx 2 on the same subsystem
# $NQN_IT:ss0, where a level change, `ns set-suspended` or an xfer's
# --auto-suspend can leave them in different ANA groups. So one grep brings
# back the uuid AND the ana_state of every namespace of the controller, and awk
# picks the directory whose uuid matches.
#
# Whether a hidden path device exposes `uuid` next to `ana_state` is a kernel
# property this tree cannot prove, so the code does not rest on it: when no
# namespace of the controller exposes a uuid the answer is a named sentinel,
# never a guess — except for the one unambiguous case, a controller with a
# single namespace, where it answers exactly what cnagent_test.sh's copy would.
#
# Three answers that are not ANA states, each a distinct fault rather than a
# guess:
#   none           no such controller, or no namespace with that uuid
#   no-uuid-attr   several namespaces and not one uuid attribute among them:
#                  the selector cannot work, and a first-match answer would be
#                  a coin flip
#   ambiguous-ns   <uuid> was "" — the caller expects a single namespace — but
#                  the controller has several
# Pass an empty <uuid> only where exactly one namespace is certain: a CN's
# controller to a side, whose nsid is a fixed 1 (agent/dnagent/plan.go:51).
ana_of() { # <ssh-wrapper> <idx> <ctrl> <uuid|"">
	local fn=$1 idx=$2 ctrl=$3 uuid=$4 pat lines
	if [ "$ctrl" = none ] || [ -z "$ctrl" ]; then
		printf 'none'
		return 0
	fi
	pat="/sys/class/nvme/$ctrl/nvme*n*"
	# -H because a glob that matches exactly one file would otherwise print no
	# name, -s because a glob that matches none is left literal by the remote
	# shell. That case also makes grep exit 2, which is why <ssh-wrapper> must
	# be an _ok form (ssh_host_ok / ssh_cn_ok) and never the bare one.
	lines=$("$fn" "$idx" "grep -sH . $pat/uuid $pat/ana_state")
	printf '%s\n' "$lines" | awk -F: -v want="$uuid" '
		{
			d = $1
			sub(/\/[^\/]+$/, "", d)
			k = $1
			sub(/^.*\//, "", k)
			if (k == "uuid") { u[d] = $2; nu++ }
			else if (k == "ana_state") { a[d] = $2; na++ }
		}
		END {
			if (want != "") {
				for (d in a) {
					if ((d in u) && u[d] == want) { print a[d]; exit }
				}
				if (nu == 0 && na == 1) {
					for (d in a) { print a[d]; exit }
				}
				if (nu == 0 && na > 1) { print "no-uuid-attr"; exit }
				print "none"
				exit
			}
			if (na == 1) { for (d in a) { print a[d]; exit } }
			if (na > 1) { print "ambiguous-ns"; exit }
			print "none"
		}'
}

# host_ana_is is the host-side probe AND the predicate in one: the ANA state
# host <h> sees for namespace <uuid> on its path to <traddr>. Two ssh calls —
# one to name the controller, one to read the attributes — which is why it is a
# predicate for wait_until and not something to spin on directly.
#
# The two steps are INLINE here rather than behind a `host_ana` helper, and
# that is the reason the helper is gone: both answers have to survive into the
# failure report, and only an assignment made in the PARENT shell does that.
# A helper read through $( ) runs in a subshell and could fill neither global.
host_ana_is() { # <h> <nqn> <traddr> <uuid> <want>
	ANA_CTRL=$(host_ctrl "$1" "$2" "$3")
	ANA_LAST=$(ana_of ssh_host_ok "$1" "$ANA_CTRL" "$4")
	[ "$ANA_LAST" = "$5" ]
}

# host_ctrl_is_present is the FIRST of host_wait_ana's two waits. It resolves
# ANA_CTRL and takes NO ANA reading — so it CLEARS ANA_LAST rather than leaving
# it.
#
# Leaving it alone was the first attempt and it was backwards. ANA_LAST is
# written in exactly two places (host_ana_is and cn_ana_is) and reset nowhere,
# so it survives across stages and across cases: a wait that times out here
# after any earlier successful ANA read would print a FRESH `last ctrl: none`
# beside a STALE `last ANA: optimized`, which is the diagnostics claiming a
# reading this wait never took — the very thing the split exists to stop.
# Clearing it makes the dump print `(none read)`, which is what happened.
host_ctrl_is_present() { # <h> <nqn> <traddr>
	ANA_LAST=""
	ANA_CTRL=$(host_ctrl "$1" "$2" "$3")
	[ -n "$ANA_CTRL" ] && [ "$ANA_CTRL" != none ]
}

# host_wait_ana bounds the wait and dies with the stage and trace id. The
# states the kernel prints are optimized, non-optimized, inaccessible,
# persistent-loss and change.
#
# TWO WAITS, ONE BUDGET, AND THE SPLIT IS THE POINT. ana_of answers `none` for
# a controller that is not there and for a controller whose namespaces do not
# include this uuid, and run 3 (2026-09-17) died on the first while the message
# described the second: "timed out waiting for host0 ANA 'optimized' for ns
# 2b6f0cc9-… via 192.168.122.77" was emitted when host0 held no controller at
# all, because `nvme connect-all` had exited 0 having connected nothing. The
# reader was sent to ANA and to the cdc; the fault was in neither.
#
# So the no-controller case gets its own wait and its own sentence, and the ANA
# wait that follows runs on the REMAINDER of the same budget — the caller asked
# for <secs> in total, not for two of them. The remainder is floored at 1s
# rather than 0: wait_until tests its predicate before it tests the clock, so a
# 1s budget is still one honest attempt and never an immediate die.
host_wait_ana() { # <h> <nqn> <traddr> <uuid> <want> [secs]
	local secs=${6:-$WAIT_HOST} left deadline
	deadline=$((SECONDS + secs))
	wait_until "$secs" \
		"host$1 to hold ANY nvme controller for $2 via $3 (there is no ANA state without one; \`nvme connect-all\` can exit 0 having connected nothing)" \
		host_ctrl_is_present "$1" "$2" "$3"
	left=$((deadline - SECONDS))
	[ "$left" -ge 1 ] || left=1
	wait_until "$left" \
		"host$1 ANA '$5' for ns $4 via $3 (on controller $ANA_CTRL, as this wait begins)" \
		host_ana_is "$1" "$2" "$3" "$4" "$5"
}

# cn_ana_is is the same probe one hop down: the state a CN sees on its own
# connection to ONE side of a leg. A traddr alone does not identify that path,
# because DNS_PER_VM dn agents share a DN VM's IP, so this one also takes the
# instance's service id (dn_trsvcid <k>). The uuid is empty on purpose: the
# side namespace sits at the fixed nsid 1 (agent/dnagent/plan.go:51) and is the
# only one the dn agent puts in that subsystem — and if that ever stops being
# true, ana_of answers ambiguous-ns instead of picking one.
cn_ana_is() { # <v> <nqn> <traddr> <trsvcid> <want>
	ANA_CTRL=$(cn_path_field "$1" "$2" "$3" "$4" Name)
	ANA_LAST=$(ana_of ssh_cn_ok "$1" "$ANA_CTRL" "")
	[ "$ANA_LAST" = "$5" ]
}

# It clears ANA_LAST for host_ctrl_is_present's reason: these two globals are
# shared by the host and CN probes alike, and a CN wait that takes no ANA
# reading must not leave an earlier one standing beside its fresh ANA_CTRL.
cn_ctrl_is_present() { # <v> <nqn> <traddr> <trsvcid>
	ANA_LAST=""
	ANA_CTRL=$(cn_path_field "$1" "$2" "$3" "$4" Name)
	[ -n "$ANA_CTRL" ] && [ "$ANA_CTRL" != none ]
}

# cn_wait_ana is host_wait_ana's split, one hop down and for the same reason:
# a CN that holds no controller to the side and a CN whose controller reports
# the wrong state are different faults, and `none` is ana_of's answer to both.
cn_wait_ana() { # <v> <nqn> <traddr> <trsvcid> <want> [secs]
	local secs=${6:-$WAIT_HOST} left deadline
	deadline=$((SECONDS + secs))
	wait_until "$secs" \
		"cn$1 to hold ANY nvme controller for $2 on its path to $3:$4 (there is no ANA state without one)" \
		cn_ctrl_is_present "$1" "$2" "$3" "$4"
	left=$((deadline - SECONDS))
	[ "$left" -ge 1 ] || left=1
	wait_until "$left" \
		"cn$1 ANA '$5' on its path to $3:$4 (on controller $ANA_CTRL, as this wait begins)" \
		cn_ana_is "$1" "$2" "$3" "$4" "$5"
}

# ---------------------------------------------------------------------------
# Host IO (cnagent_test.sh:522-534)
#
# RULE 1 IS ABSOLUTE HERE: no iflag=, no oflag=, anywhere, ever. The guests
# ship uutils dd 0.8.0, whose iflag=direct fails with EINVAL on a plain
# dm-linear and whose oflag=direct can report success while writing ZEROS
# through a raid0-over-thin stack — a false PASS, which is worse than a false
# failure. Every write below is buffered plus conv=fsync, and a read that must
# reach the media follows host_drop_caches.
#
# RULE 5 covers these too. host_drop_caches issues a `sync` and every dd here
# is host IO, so none of them may run between an `ns set-suspended` or an
# xfer's --auto-suspend and the matching resume. A read that might block anyway
# is host_sha_probe's job, never host_sha_range's.
# ---------------------------------------------------------------------------

# host_drop_caches makes the next read reach the device instead of the page
# cache. `sync` first, because drop_caches never discards a DIRTY page.
host_drop_caches() { # <h>
	ssh_host "$1" "sync; echo 3 > /proc/sys/vm/drop_caches"
}

# host_make_pattern writes <countMiB> of /dev/urandom to a FILE on the host.
# That file is the reference the suite writes and re-compares; §7.4 step 11's
# SHA0 is its digest.
host_make_pattern() { # <h> <path> <countMiB>
	ssh_host "$1" \
		"dd if=/dev/urandom of=$2 bs=1M count=$3 conv=fsync status=none"
}

# host_write_range copies <countMiB> from <src> to <dst> at <seekMiB> MiB. The
# seek is what the react case's AR6 trigger needs: with STRIPE_SIZE = 1 MiB a
# 1 MiB write at every offset k x (SLICE_CNT x STRIPE_SIZE) lands in slice 0,
# because dm-striped maps chunk c of a td to slice c mod slice_cnt.
host_write_range() { # <h> <src> <dst> <countMiB> [seekMiB]
	ssh_host "$1" \
		"dd if=$2 of=$3 bs=1M count=$4 seek=${5:-0} conv=fsync status=none; sync"
}

# host_write_probe is host_write_range for a write that is SUPPOSED to fail:
# it echoes "rc=<dd's status>" and succeeds whatever dd did, so the caller can
# judge the status instead of dying on it. dd's own stderr is left alone and
# reaches the transcript.
#
# It is synchronous, unlike host_sha_probe, and that is a claim about one
# level only: the sole place this suite writes into a device it expects to
# refuse is SP_LEVEL_READONLY, where the ns-dev carries dm-flakey's
# `error_writes` table (agent/dm.go:322-327, doc/cnagent.md:1281). dm-flakey
# fails such a bio with an error — it does not requeue it — so nothing here
# can leave a task in D state, which is the one thing that would need the
# detached shape. Do NOT reuse it against a suspended or ANA-inaccessible
# namespace (rule 5): those requeue, and this would hang the run.
host_write_probe() { # <h> <src> <dst> <countMiB> [seekMiB]
	ssh_host "$1" \
		"dd if=$2 of=$3 bs=1M count=$4 seek=${5:-0} conv=fsync status=none; echo rc=\$?"
}

# host_sha_range digests <countMiB> from <skipMiB> MiB in. Call
# host_drop_caches first whenever the answer must come from the device.
#
# This form is for a read that CANNOT block. If the device might be suspended,
# parked on a dm-error or ANA-inaccessible, use host_sha_probe: this one would
# hang the whole run, and `timeout` would not save it — timeout's SIGTERM does
# nothing to a task in D state and timeout then waits for its child.
host_sha_range() { # <h> <path> <countMiB> [skipMiB]
	ssh_host "$1" \
		"dd if=$2 bs=1M count=$3 skip=${4:-0} status=none | sha256sum | cut -d' ' -f1"
}

# host_sha_probe is host_sha_range for a read that may block: the measured
# shape for a device whose IO can wedge. The dd runs DETACHED on the guest and
# reports through a file, and the redirection ON THE GROUP is load-bearing
# twice over — a background child that still holds the ssh pipe keeps both the
# remote shell and this driver's $( ) waiting exactly as long as the read does.
#
# Its three answers:
#   <64 hex digits>  the read completed; compare it like host_sha_range's
#   ioerror          dd failed — an error target under the device, e.g. a
#                    parked ns-dev
#   blocked          nothing came back within <secs>: the reader is in D state
#
# When it says blocked it leaves an unkillable dd behind (and its stderr in
# $WORK/sha-probe.err). That is the price of learning the answer at all; the
# resume that unwedges the device reaps it. One probe at a time per host: the
# three files are named per host, not per call, and they need $WORK to exist
# there, which setup creates.
host_sha_probe() { # <h> <path> <countMiB> <skipMiB> [secs]
	local h=$1 path=$2 cnt=$3 skip=$4 secs=${5:-$WAIT_HOST} tag cmd
	tag="$WORK/sha-probe"
	cmd="rm -f $tag.sha $tag.rc $tag.err;"
	cmd="$cmd { dd if=$path bs=1M count=$cnt skip=$skip status=none"
	cmd="$cmd | sha256sum | cut -d' ' -f1 >$tag.sha;"
	cmd="$cmd echo \${PIPESTATUS[0]} >$tag.rc; }"
	cmd="$cmd </dev/null >/dev/null 2>$tag.err &"
	cmd="$cmd for ((i = 0; i < $secs * 10; i++)); do"
	cmd="$cmd [ -s $tag.rc ] && break; sleep 0.1; done;"
	cmd="$cmd if [ ! -s $tag.rc ]; then echo blocked;"
	cmd="$cmd elif [ \"\$(cat $tag.rc)\" != 0 ]; then echo ioerror;"
	cmd="$cmd else cat $tag.sha; fi"
	ssh_host "$h" "$cmd"
}

# ---------------------------------------------------------------------------
# How a disk node is addressed (D10)
# ---------------------------------------------------------------------------
#
# TWO indices, and confusing them is the easy mistake in everything below:
#
#   v  the DN VM index, 0..DN_VM_CNT-1, in the order the --dn flags appeared.
#      It names a GUEST: ${DN[$v]} is the ssh target, ${DN_IP[$v]} its address,
#      and dn_location <v> = dn<v> is the string every agent of that VM
#      registers as --location (D5) — which is what makes the allocator spread
#      a group's legs across VMs, because a scan returns at most one candidate
#      per location (model/alloc.go:137-141).
#   k  the instance index WITHIN one VM, 0..DNS_PER_VM-1. It names a PROCESS:
#      dn_grpc_port, dn_trsvcid, dn_port_id, dn_dir, dn_backing, dn_store and
#      dn_log are functions of k ALONE, because they are ports and paths on VM
#      v's own kernel and filesystem and every VM runs the same k range. So
#      /var/tmp/dnv-e2e/dn7 exists on all four DN VMs and means a different
#      agent on each.
#
# An agent is therefore the PAIR (v, k), and its gRPC endpoint — the string
# `dn create --addr` takes — is dn_addr v k. Neither index is a dn_id: the
# gateway mints that at `dn create` and dnvctl prints it as a decimal string
# (F11), and nothing here may assume the two orders agree.
#
# CN VMs need no second index: one cn agent per VM (D12), so a CN is just v.
# ---------------------------------------------------------------------------

# dn_addr is the only place (v, k) is turned into an endpoint.
dn_addr() { # <v> <k>
	printf '%s:%s' "${DN_IP[$1]}" "$(dn_grpc_port "$2")"
}

cn_addr() { # <v>
	printf '%s:%s' "${CN_IP[$1]}" "$CN_GRPC_PORT"
}

# The pid files sit in the same per-process directory as that process's log,
# derived from the section-1 accessors rather than spelled out again.
dn_pid_file() { printf '%s/pid' "$(dn_dir "$1")"; }
cn_pid_file() { printf '%s/pid' "$(cn_dir)"; }

# DN_LOOP[<v>:<k>] is the loop device dn_up attached over that instance's
# backing file. Filled by start_dn_instance, read by anything that needs the
# --disk an agent was given (the space guard, diagnostics, a restart).
declare -A DN_LOOP=()

dn_key() { printf '%s:%s' "$1" "$2"; }

# ---------------------------------------------------------------------------
# The guest helper (§7.3)
# ---------------------------------------------------------------------------
#
# One generated script per ROLE FAMILY — dn, cn, host, cp — shipped to
# $HELPER on every guest of that family and invoked through helper_dn /
# helper_cn / helper_host / helper_cp. A guest has exactly one role, so one
# path carries whichever family it needs.
#
# Four reasons everything multi-statement lives here instead of in an ssh
# command string:
#
#  1. RULE 3. `pkill -f` matches the wrapping `bash -lc` argv of the ssh
#     command itself, so a pattern typed into an ssh string kills its own
#     shell. Every pkill in this suite is in a helper file AND bracketed
#     ([d]nv-agent), which is belt and braces: the helper's own argv carries
#     the VERB (`kill_agents dn`), never the pattern.
#  2. ssh_sudo %q-quotes the whole command for the remote login shell, which
#     hands it to `bash -c`. A newline in that string comes back as $'…' and
#     only bash can parse it, so a driver-side command must stay ONE LINE —
#     which a teardown loop cannot.
#  3. A helper function is readable, and a run's cleanup is the part of a
#     suite that must work when everything else has already failed.
#  4. Quoting once, here, means the guest's own bash does the globbing.
#
# The script is generated as an EXPANDED preamble (the run's paths and
# numbers, straight from this file's constants, so nothing is re-typed) plus a
# LITERAL body (`<<'EOF'`, which expands nothing). cnagent_test.sh:727-1388
# re-types its globals inside the quoted heredoc; the split here is the same
# idea with one source of truth. Every function in the body may use only
# variables the preamble sets — the guest runs with `set -u`, so a forgotten
# one is an immediate, named failure rather than an empty string.
#
# A LATER SECTION THAT ADDS A VERB adds it to the role's *_source function and
# to no other place, and may use only preamble variables plus its own
# arguments. Helper arguments are joined with spaces by helper_* ("$*"), so no
# argument may contain a space; every path, port, id and nqn this suite passes
# is space-free.
#
# The body is best-effort by design: cleanup must survive a crashed prior run,
# so a verb reports what it could not do and returns 0 rather than aborting
# half way. The exceptions are the two provisioning verbs (dn_up, cn_up),
# which return non-zero so the driver can die with a message.
# ---------------------------------------------------------------------------

# The etcd --name on cp. It is in the preamble because the cp helper's last
# resort kill matches on it: a bare `pkill -f etcd` on a shared guest is not
# something this suite may do.
ETCD_NAME=dnv-e2e-it

helper_preamble() { # <role: dn|cn|host|cp>
	cat <<EOF
#!/usr/bin/env bash
#
# dnv-e2e-helper.sh — the $1 half of integtest/e2e_test.sh, GENERATED by that
# suite and shipped to every $1 guest as
#   $HELPER
# on every run, including --cleanup-only. Editing it here changes nothing.
#
# It lives OUTSIDE \$WORK on purpose — cleanup runs \`rm -rf \$WORK\` through
# this very script, and a script may not delete itself while bash is still
# reading it.
#
# No iflag=/oflag= appears anywhere in this file (uutils dd 0.8.0), no pkill
# pattern is unbracketed, and no nvmet port is removed before its ana_groups.
set -uo pipefail

WORK="$WORK"
NVMET="$NVMET"
TMPFS_DIR="$TMPFS_DIR"
NQN_PREFIX="$NQN_PREFIX"
NQN_IT="$NQN_IT"
# common.DmPrefix (common/constants.go:136): every dm device either agent
# creates is dnv-<cluster16>-<node16>-<kind1>-… (common/name_fmt.go:69-86).
DM_PREFIX="dnv"
# The md assembly mask cnagent_test.sh:1033-1041 installs, by the same path.
# BOTH node roles install it here (rule 7, install_udev_rule), and the verbs
# that do live in the shared node body. This preamble is shared by all four
# roles, so a host and a cp helper define the name and never use it.
UDEV_RULE=/etc/udev/rules.d/63-dnv-md.rules
ETCD_NAME="$ETCD_NAME"
CN_PORT_ID="$CN_PORT_ID"
MAX_DNS_PER_VM="$MAX_DNS_PER_VM"
TRSVCID_MIN="$TRSVCID_MIN"
TRSVCID_MAX="$TRSVCID_MAX"
EOF
}

# --- the body every role shares --------------------------------------------
helper_common_source() {
	cat <<'HELPER_COMMON_EOF'

# --- processes ---------------------------------------------------------------

# kill_pidfile stops ONE recorded process: the react case kills a single
# agent, and `pkill -f` cannot express "instance 7 of this VM". CONT first, in
# case a previous step stopped it.
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

# alive reports whether one recorded pid is still there, as a word rather than
# an exit status, so a driver-side assert_eq names what it found.
alive() { # <pidfile>
	local pid
	[ -f "$1" ] || {
		echo nofile
		return 0
	}
	pid=$(cat "$1" 2>/dev/null) || pid=""
	case "$pid" in
	'' | *[!0-9]*)
		echo nopid
		return 0
		;;
	esac
	if kill -0 "$pid" 2>/dev/null; then echo alive; else echo dead; fi
	return 0
}

# --- ports -------------------------------------------------------------------

# listening is a PREDICATE: exit 0 when something holds that TCP port. It is
# `grep -c`, never `grep -q`: `set -o pipefail` plus a reader that closes the
# pipe early turns a SIGPIPE on the writer into an abort.
listening() { # <port>
	local n
	n=$(ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://' |
		grep -cx "$1" || true)
	[ "$n" != 0 ]
}

# ports_busy prints the subset of its arguments that is already listening, so
# preflight can assert on an empty answer and name the offenders otherwise.
ports_busy() { # <port…>
	local p
	for p in "$@"; do
		listening "$p" && printf '%s\n' "$p"
	done
	return 0
}

# --- work directory ----------------------------------------------------------

# mkwork creates $WORK and the named subdirectories. The 0777 is what lets the
# plain-user scp of ship_binaries write into a tree the sudo helper created
# (cnagent_test.sh:1565 does the same); on cp, where the helper is already the
# plain user, it changes nothing that matters.
mkwork() { # <subdir…>
	local d
	mkdir -p "$WORK" || return 1
	chmod 0777 "$WORK" 2>/dev/null || true
	for d in "$@"; do
		mkdir -p "$WORK/$d" || return 1
		chmod 0777 "$WORK/$d" 2>/dev/null || true
	done
	echo ok
	return 0
}

# --- space (§7.8) ------------------------------------------------------------

# alloc prints "<allocated bytes> <path>" per argument — ALLOCATED, not
# apparent: the whole space argument of this suite is that a sparse backing
# file stays sparse because blkdiscard --zeroout punches holes
# (agent/dm.go:250-261), and an apparent size would report 2 GiB for a file
# that costs nothing. stat's %b is in %B-sized units.
alloc() { # <path…>
	local p out b bs
	for p in "$@"; do
		if [ ! -e "$p" ]; then
			printf '0 %s\n' "$p"
			continue
		fi
		out=$(stat -c '%b %B' "$p" 2>/dev/null) || out=""
		b=${out%% *}
		bs=${out##* }
		case "$b" in '' | *[!0-9]*) b="" ;; esac
		case "$bs" in '' | *[!0-9]*) bs="" ;; esac
		if [ -n "$b" ] && [ -n "$bs" ]; then
			printf '%s %s\n' "$((b * bs))" "$p"
		else
			# Never a silent 0: a stat this suite cannot read is a fault to
			# report, not a file that costs nothing.
			printf 'unknown %s\n' "$p"
		fi
	done
	return 0
}

# dir_bytes is `du -s` in bytes, with a -k fallback for a du that does not
# take --block-size. The KiB-to-byte multiply is done in BASH, not in awk:
# awk's default output format is %.6g, so a directory of 59 GiB would print as
# 5.9392e+10 and every numeric comparison built on it would be a string
# comparison against nonsense.
dir_bytes() { # <dir>
	local n
	[ -d "$1" ] || {
		printf '0'
		return 0
	}
	n=$(du -s --block-size=1 "$1" 2>/dev/null | awk 'NR==1{print $1}')
	case "$n" in
	'' | *[!0-9]*)
		n=$(du -sk "$1" 2>/dev/null | awk 'NR==1{print $1}')
		case "$n" in
		'' | *[!0-9]*) n=0 ;;
		*) n=$((n * 1024)) ;;
		esac
		;;
	esac
	case "$n" in
	'' | *[!0-9]*) n=0 ;;
	esac
	printf '%s' "$n"
}

# space reports this guest's three §7.8 numbers as key=value lines. tmpfs is
# reported everywhere and is 0 where no cn agent runs: $TMPFS_DIR is NOT under
# $WORK (common/constants.go:139 fixes it at /tmp/dnv-tmpfs and no flag moves
# it), so `rm -rf $WORK` never touches it and a guard that only looked at
# $WORK would miss a CN's whole clone-metadata arena.
space() {
	local free
	free=$(df -Pk /var/tmp 2>/dev/null | awk 'NR==2{print $4}')
	case "$free" in
	'' | *[!0-9]*) free=0 ;;
	*) free=$((free * 1024)) ;;
	esac
	printf 'work=%s\n' "$(dir_bytes "$WORK")"
	printf 'tmpfs=%s\n' "$(dir_bytes "$TMPFS_DIR")"
	printf 'free=%s\n' "$free"
	return 0
}

# --- logs --------------------------------------------------------------------

# logtail prints the tail of each named file with a banner. It takes the files
# from the driver rather than globbing $WORK itself, because a DN VM can hold
# 43 agent logs and dumping all of them is not a diagnostic, it is a flood.
logtail() { # <lines> <file…>
	local n=$1 f
	shift
	for f in "$@"; do
		printf -- '--- tail -n %s %s ---\n' "$n" "$f"
		if [ -f "$f" ]; then
			tail -n "$n" "$f" 2>/dev/null
		else
			echo "(no such file)"
		fi
	done
	return 0
}

# grep_log is logtail's filter: a FIXED-STRING match, last <lines> hits. The
# dnv binaries log slog JSON to STDERR (common/log.go:99-113), which is why
# every launch here redirects 2>&1 into the log file; an error record is
# `"level":"ERROR"` in that JSON.
grep_log() { # <fixed-string> <lines> <file…>
	local pat=$1 n=$2 f
	shift 2
	for f in "$@"; do
		printf -- '--- %s matching %s ---\n' "$f" "$pat"
		if [ -f "$f" ]; then
			grep -F -- "$pat" "$f" 2>/dev/null | tail -n "$n" || true
		else
			echo "(no such file)"
		fi
	done
	return 0
}
HELPER_COMMON_EOF
}

# --- the body the two agent roles share ------------------------------------
#
# Everything below is copied from integtest/cnagent_test.sh's shipped helper
# (the functions of `vm_helper_source`, :727-1388) and dnagent_test.sh's
# cleanup (:661), with four deliberate changes:
#
#   a. disconnect_prefix reads /sys/class/nvme-subsystem/*/subsysnqn instead of
#      piping `nvme list-subsys -o json` through the guest's jq
#      (cnagent_test.sh:1174). This suite installs no jq on any guest —
#      resolve_jq builds one for the DRIVER — and cdc_test.sh:1142-1153
#      already does it from sysfs.
#   b. dm_kind_names takes no node id. cnagent_test.sh passes one because both
#      roles share a VM there and the kind digits overlap between roles
#      (common/name_fmt.go:11-29). Here a DN VM runs only dn agents and a CN VM
#      only the cn agent, so filtering by kind alone is both sufficient and
#      what catches an instance whose ids the driver no longer knows.
#   c. the nvmet port teardown is a guarded sweep over many ports, not
#      `rmdir ports/1`. See ports_sweep.
#   d. install_udev_rule / remove_udev_rule are HERE and not in the cn body.
#      cnagent_test.sh runs both roles on one VM, so the question never arose;
#      here the roles are on different guests and both need the mask — the CN
#      because it assembles arrays, the DN because the CN's superblocks land on
#      it (rule 7). It also writes only when the content differs, because dn_up
#      calls it once per instance. See its own comment.
helper_node_source() {
	cat <<'HELPER_NODE_EOF'

# --- the agent processes -----------------------------------------------------

# kill_agents stops every dnv-agent of one role THIS RUN STARTED on this guest.
# The pattern is bracketed so it cannot match the argv of anything in this
# suite's own ssh chain, and pkill and pgrep both exclude themselves.
#
# IT IS QUALIFIED BY $WORK, exactly as the cp helper's etcd pattern is
# qualified by --name. dn_up and cn_up launch "$WORK/bin/dnv-agent <role> …",
# so every agent this run owns carries that absolute path in its argv, while a
# `dnv-agent` of cnagent_test.sh or dnagent_test.sh does not — and those
# suites run the same binaries as the same login user on these very guests
# (the lab note dnv-integtest-lab-vms lists the shared VMs). An unqualified
# sweep would kill them. A previous e2e run used the same $WORK, so the
# fallback still reaches the corpses it is for.
#
# TERM first: both roles install signal.NotifyContext for SIGINT and SIGTERM
# (cmd/dnv-agent/main.go:166-168, :204-206), which unwinds agent.Serve. The dn
# additionally JOINS its background goroutines on the way out (:187,
# srv.WaitBackground — the §9.4 zeroing loop owns a `blkdiscard --zeroout`
# child); the cn passes nil (:225) and deliberately does not, so a dn can take
# noticeably longer to go than a cn. KILL only after that.
kill_agents() { # <dn|cn>
	local pat="$WORK/bin/[d]nv-agent $1" i
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

# agent_pids lists what is still running, for the §7.9 diagnostics dump. It is
# deliberately NOT qualified by $WORK, unlike kill_agents: it only READS, and
# on a shared guest an agent of another suite is precisely what the dump should
# show. The "did the kill take" question is kill_agents' own pgrep, which is
# qualified, so nothing asserts against this broader list.
agent_pids() { # <dn|cn>
	pgrep -af "[d]nv-agent $1" || true
	return 0
}

# --- dm ----------------------------------------------------------------------

dm_names() {
	dmsetup ls 2>/dev/null | awk '{print $1}' |
		grep -E "^$DM_PREFIX-[0-9a-f]{16}-[0-9a-f]{16}-[0-9a-f]-" || true
	return 0
}

# dm_kind_names <kind>: the dm devices of one kind digit. A name is
# dnv-<cluster>-<node>-<kind>-…, so the kind is field 4 under -F-.
dm_kind_names() { # <kind hex digit>
	dm_names | awk -F- -v k="$1" '$4 == k'
	return 0
}

# NOTHING IN THIS SUITE INVOKES THIS VERB: the residue checks go through
# dn_residue / cn_residue, which list the NAMES, and the driver's
# dn_residue_empty / cn_residue_empty test that list for emptiness — because a
# count of 0 and a list of what is left are the same answer only when the
# answer is 0. It is kept as the cheap form for a caller that needs only "how
# many".
dm_cnt() {
	dm_names | grep -c . || true
	return 0
}

# resume_suspended releases every suspended dnv dm device before anything
# reads one. Anything that touches a suspended device — `dmsetup remove`,
# disabling the nvmet namespace above it, above all a block-device scan —
# blocks in uninterruptible D state and wedges the guest until reboot, and
# `timeout` does not help (it cannot end a task in D state).
#
# `:..s` and NOT `:.-s`: attr is L/I/s/r-w, so pinning the second column to
# `-` would skip a device that also has an INACTIVE TABLE loaded — `LIsw`,
# exactly what an interrupted suspend/load/resume leaves behind, which is the
# state this sweep exists for. The pattern is looser than dm_names' so it also
# sweeps debris an older run left on a shared lab guest.
resume_suspended() {
	local row name
	for row in $(dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E '^dnv[-a-z0-9]*.*:..s'); do
		name=${row%%:*}
		timeout 10 dmsetup resume "$name" >/dev/null 2>&1
	done
	return 0
}

suspended_dms() {
	local row
	for row in $(dmsetup info -c --noheadings -o name,attr 2>/dev/null |
		grep -E '^dnv[-a-z0-9]*.*:..s'); do
		printf '%s\n' "${row%%:*}"
	done
	return 0
}

# dm_force_remove removes one device, falling back to --force (which swaps in
# an error table when the device is still open) rather than blocking.
#
# THE THREE CALLS WERE RE-EXAMINED ON 2026-09-17 AND LEFT ALONE, because the
# case that made them expensive was never this function's. What made the
# 2026-09-17 cleanup grind was md_stop_all stopping nothing, so ~128 devices
# were still pinned under live arrays — the kind-9 leg wrappers on a CN, a
# side or the per-CN linear over it on a DN — and every one of them took all
# three calls; with md_stop_all fixed the busy case should not arise from md.
# The two shapes, read separately:
#   - A DEVICE THAT IS ALREADY GONE costs three immediate failures and not
#     three timeouts: each call is one device-mapper ioctl the kernel answers
#     with "Device does not exist", so nothing waits and no bound is reached.
#     It is also close to unreachable — dm_remove_kind and dm_remove_all both
#     enumerate live names out of `dmsetup ls` (dm_names, dm_kind_names), so a
#     gone device here means something removed it between the listing and the
#     call.
#   - A DEVICE THAT IS GENUINELY BUSY needs all three, and each earns its
#     place: `remove` is the fast path and the only one that does not touch
#     the table first, `resume` is rule 5's guard (a device left suspended
#     must be resumed before anything else touches it, or it goes to D
#     state where `timeout` cannot reach it), and `--force --retry` is the only
#     one that does anything at all to a device that will not go — `--retry`
#     retries the removal, which wins against a TRANSIENT holder such as a udev
#     worker still probing, and `--force` swaps in an error target when it
#     still cannot go, so the device stops backing our storage even though (doc
#     §6) the device itself stays until its holder lets go. Dropping any of
#     them trades a bounded grind for a wedge or for debris, and
#     CLEANUP_TIMEOUT is the wedge detector that already bounds the grind.
dm_force_remove() { # <name>
	timeout 10 dmsetup remove "$1" >/dev/null 2>&1 && return 0
	timeout 10 dmsetup resume "$1" >/dev/null 2>&1
	timeout 15 dmsetup remove --force --retry "$1" >/dev/null 2>&1
	return 0
}

dm_remove_kind() { # <kind hex digit>
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

# --- nvme connections --------------------------------------------------------

# disconnect_prefix drops every connection whose subsystem NQN starts with the
# prefix. It reads sysfs, not `nvme list-subsys -o json`, because no guest in
# this suite has a jq (cdc_test.sh:1142-1153 is the same shape). A whole-NQN
# disconnect is only ever used where every path of that NQN is being retired.
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

# --- nvmet -------------------------------------------------------------------

# drop_subsys_glob unlinks the subsystem from every port, disables and removes
# its namespaces, drops its allowed_hosts links and removes it. Namespaces
# must be disabled before the dm devices under them can go.
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

# hosts_drop removes the nvmet hosts/ entries this suite is responsible for:
# every tree-minted one ($NQN_PREFIX:*), every one of its own host-facing
# subsystems ($NQN_IT*), and any extra nqn the caller names — which is how the
# two real hosts' /etc/nvme/hostnqn entries get swept, since those follow the
# kernel's own nqn.2014-08.org.nvmexpress format and match no prefix of ours.
# configfs refuses to remove a group an allowed_hosts symlink still points at,
# which is why this runs after drop_subsys_glob. A leftover empty entry harms
# nothing (the agent's ensure path is probe-first), so failure is silent.
hosts_drop() { # [extra nqn…]
	local host extra
	[ -d "$NVMET/hosts" ] || return 0
	for host in "$NVMET"/hosts/"$NQN_PREFIX":* "$NVMET"/hosts/"$NQN_IT"*; do
		[ -d "$host" ] && rmdir "$host" 2>/dev/null
	done
	for extra in "$@"; do
		[ -d "$NVMET/hosts/$extra" ] && rmdir "$NVMET/hosts/$extra" 2>/dev/null
	done
	return 0
}

# port_trsvcid prints one port's service id with spaces stripped. The strip is
# not cosmetic: nvmet reads several addr_* attributes back space-padded
# (memory note nvmet-configfs-idempotency), and cdc_test.sh:1072 strips
# it for the same comparison.
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

# port_drop removes ONE nvmet port, and refuses to touch one that is not this
# suite's. The judgement is the service id, exactly as cdc_test.sh:1069-1074
# judges its own: this suite's agents listen on $TRSVCID_MIN..$TRSVCID_MAX
# (4300..4349), the dn and cn suites use 4200 and the cdc suite 14420..14423,
# so a port outside the band belongs to another suite or to a human and
# removing it would break them.
#
# ORDER IS LOAD-BEARING. ana_groups 3 and 2 are removed BEFORE the port, or
# the rmdir fails with "Directory not empty" and the leftover port fails the
# NEXT suite's setup (memory note nvmet-port-teardown-ana-groups). Group 1
# already exists when the port is created and is not one the agent made; the
# agent creates 2 and 3 and writes the three fixed ana_states
# (agent/nvmet.go:23-28, :138-156).
port_drop() { # <port id>
	local id=$1 svc grp
	[ -d "$NVMET/ports/$id" ] || {
		printf 'port %s absent\n' "$id"
		return 0
	}
	svc=$(port_trsvcid "$id")
	case "$svc" in
	none)
		# A port whose addr_trsvcid is empty is debris — an agent killed
		# between `mkdir ports/<id>` and the first attribute write. It is
		# removed rather than refused: refusing would leave it forever, and a
		# leftover port is exactly what fails the next suite's setup.
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
	local link
	for link in "$NVMET/ports/$id"/subsystems/*; do
		[ -e "$link" ] && rm -f "$link"
	done
	for grp in 3 2; do
		rmdir "$NVMET/ports/$id/ana_groups/$grp" 2>/dev/null
	done
	rmdir "$NVMET/ports/$id" 2>/dev/null
	if [ -d "$NVMET/ports/$id" ]; then
		printf 'port %s STUCK trsvcid=%s ana_groups=%s subsystems=%s\n' \
			"$id" "$svc" \
			"$(ls "$NVMET/ports/$id/ana_groups" 2>/dev/null | tr '\n' ' ')" \
			"$(ls "$NVMET/ports/$id/subsystems" 2>/dev/null | tr '\n' ' ')"
	else
		printf 'port %s removed trsvcid=%s\n' "$id" "$svc"
	fi
	return 0
}

# ports_sweep drops every port in 1..$MAX_DNS_PER_VM. It walks the whole range
# and not just this run's DNS_PER_VM because an aborted earlier run — or a run
# with a larger --dns-per-vm — may have left a higher one behind, and one
# leftover port with live ana_groups is enough to fail the next suite.
ports_sweep() {
	local id
	[ -d "$NVMET/ports" ] || return 0
	for ((id = 1; id <= MAX_DNS_PER_VM; id++)); do
		[ -d "$NVMET/ports/$id" ] || continue
		port_drop "$id"
	done
	return 0
}

nvmet_tree() {
	ls -R "$NVMET/ports" "$NVMET/subsystems" "$NVMET/hosts" 2>/dev/null || true
	return 0
}

# --- md ----------------------------------------------------------------------

# md_stop_all stops every array this suite created. It enumerates the arrays
# from /proc/mdstat and reads each one's NAME — from udev, and from mdadm when
# udev has none — because the agent's `--homehost any` means the name may or
# may not carry a homehost prefix, and because the name is the only thing that
# separates one of our arrays from a lab guest's own.
#
# IT USED TO READ `mdadm --detail --scan` AND THAT COMMAND HAS NO NAME IN IT.
# Measured on cn2 (192.168.122.77, kernel 7.0.0-31, Ubuntu 26.04 mdadm) on
# 2026-09-17: the scan prints
#
#   ARRAY /dev/md/6030def500000000000000010800 metadata=1.2
#
# and nothing else — the path carries MD_DEVNAME, which is hex, not the name —
# so the old `*name=dnv-*` case could never fire. A `bash -x` trace of the real
# verb: 59 ARRAY lines, 59 `case`s, 59 `continue`s, zero `mdadm --stop`. The
# function had never stopped anything on this lab. udev has the name on the
# assembled device:
#
#   $ udevadm info --query=property --name=/dev/md88
#   MD_DEVNAME=6030def500000000000000018800
#   MD_NAME=any:dnv-0000000000000001-88-00
#
# Walking /proc/mdstat and reading MD_NAME that way matched 59 of 59 arrays on
# that guest and stopped all 59 in 1.9 s.
#
# TWO SOURCES FOR ONE NAME, BECAUSE THEY GO BLIND ON DIFFERENT ARRAYS. udev's
# MD_NAME is not udev's own answer: the stock
# /usr/lib/udev/rules.d/63-md-raid-arrays.rules IMPORTs it from
# `mdadm --detail --no-devices --export $devnode` on the ARRAY device, so the
# udev read is that mdadm answer cached at the last event that reached the
# import, and the fallback is the same answer live. Each misses a case the
# other has:
#
#   - udev has nothing while the array is `clear` or `inactive`. The line
#     above that import in the same file —
#     ATTR{md/array_state}=="clear*|inactive", ENV{SYSTEMD_READY}="0",
#     GOTO="md_end" — jumps past it, so such an array has no MD_ property in
#     the db at all and the udev read comes back empty. That state is not
#     hypothetical here: `mdadm -I` on ONE member of a two-member raid1 lands
#     exactly there, which is the stray DN assembly of §8 item 15 before
#     mdadm-last-resort promotes it — and for good, if nothing does.
#   - mdadm has nothing when it cannot read a member's superblock. That is
#     what the 59 wedged arrays of 2026-09-17 looked like: `mdadm --detail
#     --export` printed MD_UUID and MD_DEVNAME but no MD_NAME and
#     `mdadm --examine --export` answered "No md superblock detected on
#     /dev/dm-12" — an unreadable member, and the explanation to hand is that
#     the same failed sweep had just run ~128 dm_force_remove fallbacks, whose
#     `--force` swaps an error target in under exactly those legs (that last
#     step is inference; the unreadable member is what was measured). That is
#     a property of those particular arrays and not of
#     mdadm from the assembled side: measured on the same guest with the same
#     mdadm, `--detail --no-devices --export` prints MD_NAME for a live dnv
#     array AND for an inactive one whose one member is readable.
#
# NEITHER SOURCE REACHES AN ARRAY THAT IS BOTH inactive and standing over
# members mdadm cannot read — which is precisely what a cleanup that fell
# through to `dmsetup remove --force` leaves behind, and it is permanent:
# `mdadm --run` on an array over an error target was measured failing with EIO
# out of `array_state`, so it never leaves `inactive`. That array is
# skipped, it still pins its members, and stopping it needs an operator. The
# udevadm line the banner and the start gate hand over does not name this one
# either, by construction; what identifies it is /proc/mdstat — an `inactive`
# array whose member devices are this suite's dm names. The STOP is not the
# part that fails: `mdadm --stop` was measured working on an inactive array on
# that guest. It is the name read that does.
#
# THE MASK WAS NEVER IMPLICATED and needs no change, but it is TWO rules and
# only the first is scoped. The first — ACTION=="add|change",
# SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member" — IMPORTs MD_NAME
# from `mdadm --examine --export` on the MEMBER's devnode, the event whose
# assembly it then suppresses. The second, ENV{MD_NAME}=="dnv-*|*:dnv-*",
# ENV{SYSTEMD_READY}="0", carries no subsystem and no fs-type match at all;
# what keeps it off an ARRAY device is the file name. 63-dnv-md.rules sorts
# ahead of 63-md-raid-arrays.rules, which is what sets MD_NAME on an array, so
# on the array's own event the property is not there yet to match — measured
# with the rule installed on cn2: the array's db held MD_NAME and no
# SYSTEMD_READY, the member held ID_FS_TYPE=linux_raid_member, MD_NAME and
# SYSTEMD_READY=0. Should a later event ever carry MD_NAME in from the db and
# make it match there, it is still not this function's problem: SYSTEMD_READY
# is a systemd readiness flag and 64-md-raid-assembly.rules reads it to skip
# INCREMENTAL assembly, which is done to members; `mdadm --stop` and the
# agent's own `--create`/`--assemble` do not go through udev at all.
#
# WHAT IT MATCHES, because it runs on DN VMs too (dn_cleanup) where no dnv
# array is ever supposed to exist: an MD_NAME of `dnv-…` or `<homehost>:dnv-…`,
# and nothing else. That is now the SAME pair of patterns install_udev_rule
# writes into the mask (ENV{MD_NAME}=="dnv-*|*:dnv-*"), against the same udev
# property — the two are no longer two different readings of "the name". It is
# the name the cn agent gives every array it creates
# (common.NameFmt.CnMdArrayName, common/name_fmt.go:208-229, passed as `--name`
# at agent/cnagent/md.go:117 with `--homehost any` at :125), so a guest's own
# root or data array is never touched — only an array minted by a dnv cn agent,
# whether it was assembled here on purpose or by the stock udev rule behind our
# back.
#
# AN UNREADABLE NAME LEAVES THE ARRAY ALONE. An answer empty from BOTH reads —
# no udevadm or no mdadm, no udev db entry for that array, an array state that
# kept MD_NAME out of the db over members mdadm cannot read, either command
# hitting its bound — matches neither pattern, so the loop skips it. That is
# the safe direction of the two and the one this function may not lose; it is
# also the direction that makes the verb silently do nothing, which is exactly
# the failure above, so mdadm and udevadm are both required tools for BOTH
# node roles (DN_TOOLS, CN_TOOLS) and preflight fails on a guest without
# either.
#
# PREFLIGHT DOES NOT COVER EVERY CALL, and the gap is where the 2026-09-17
# failure sat: preflight_guests runs AFTER the unconditional start cleanup
# (main, and §5), and `--cleanup-only` does not preflight at all. So on a guest
# missing one of the two tools, the start sweep — the one that recovers a
# crashed run — and `--cleanup-only` each get one silent no-op pass, and only
# the sweeps after preflight are covered.
#
# THE /proc/mdstat PATTERN IS `^md[^ :]+` and not `^md[0-9]*`, which would
# match the bare `md` of a line like `md_dnv-… : active` — mdadm names the
# device that way when mdadm.conf carries `CREATE names=yes`, since the agent
# creates through /dev/md/<name> (NameFmt.MdPath). `/dev/md` is the by-name
# DIRECTORY, so that token would read nothing and skip an array we own. The
# lab guests run the default `names=no` and show md88/md127, so this is a trap
# and not a live failure.
md_stop_all() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(timeout 10 udevadm info --query=property \
			--name="/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		[ -n "$name" ] || name=$(timeout 10 mdadm --detail \
			--no-devices --export "/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		case "$name" in
		dnv-* | *:dnv-*) ;;
		*) continue ;;
		esac
		timeout 15 mdadm --stop "/dev/$d" >/dev/null 2>&1
	done
	return 0
}

# md_names prints the MD_NAME of every assembled dnv array, one per line, by
# md_stop_all's route and for md_stop_all's reason: `mdadm --detail --scan`
# prints no `name=` field on these guests, so the `grep -oE 'name=…dnv-…'` this
# replaces matched NOTHING and every residue assertion built on it passed
# vacuously — a teardown check that could not fail. Empty output is the pass,
# as it was before; the difference is that it is now empty because there is no
# array, not because the field was never there.
md_names() {
	local d name
	for d in $(grep -oE '^md[^ :]+' /proc/mdstat 2>/dev/null); do
		name=$(timeout 10 udevadm info --query=property \
			--name="/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		[ -n "$name" ] || name=$(timeout 10 mdadm --detail \
			--no-devices --export "/dev/$d" 2>/dev/null |
			sed -n 's/^MD_NAME=//p')
		case "$name" in
		dnv-* | *:dnv-*) printf '%s\n' "$name" ;;
		esac
	done
	return 0
}

mdstat() {
	cat /proc/mdstat 2>/dev/null || true
	return 0
}

# --- the md assembly mask ----------------------------------------------------
#
# install_udev_rule masks the stock incremental md assembly for dnv arrays, so
# the agent is the only assembler. The rule text is cnagent_test.sh:1033-1041
# verbatim: the stock 64-md-raid-assembly.rules skips a device whose
# SYSTEMD_READY is 0, and this file is 63-, so it runs first and sets it. It is
# scoped by MD_NAME, so it suppresses ONLY arrays a dnv cn agent minted and
# leaves the guest's own arrays — a root filesystem raid above all — assembling
# normally.
#
# IT GOES ON BOTH NODE ROLES, which is why it lives in the shared body and not
# in helper_cn_source (rule 7):
#
#   - on a CN because that is where md RUNS, and the stock rule would race the
#     agent for an array it is in the middle of creating;
#   - on a DN because that is where the md METADATA ends up. The cn writes each
#     leg's superblock through the side's nvme-tcp export, so it physically
#     lands on the DN's local storage and the DN's own udev sees a
#     linux_raid_member. On TWO devices, and the lab evidence does not say
#     which one it took: the nvmet namespace exports the per-CN linear
#     (agent/dnagent/syncup_side.go:597), which ensureDmLinear builds over the
#     side at offset 0 for its whole length (:529-566), so the superblock sits
#     at the same offset in the kind-1 linear and in the kind-4 side and udev
#     probes both. `mdadm -I` assembles it there — degraded, auto-read-only,
#     over one of the DN's own dm devices — and that array holds that device
#     open. Either way the side stays: a pinned kind-1 linear holds the kind-4
#     side open in its turn, so dn_cleanup's dm_remove_kind runs past its bound
#     whichever of the two it is. Measured on 2026-09-17: the two DN VMs with
#     no rule file in /etc/udev/rules.d carried 35 and 28 such arrays and both
#     timed out; the two that did carried none and cleaned up fine. What those
#     two had was 58-dnv-test.rules, left behind by the dnagent lab work — a
#     different rule, not this mask, and not in this tree; that it also kept
#     the arrays away was incidental, and is not something to rely on.
#
# The DN does not get a BROADER mask, although it owns no dnv array and could
# in principle take one: a rule that masked every linux_raid_member would also
# mask the guest's own root array, and one that suppressed dm's udev rules
# outright would take the /dev/disk/by-id symlinks and the blkid properties
# with it. The narrow rule is exactly as wide as the damage.
#
# THE WRITE IS CONDITIONAL AND THE RELOAD IS NOT, and the asymmetry is the
# point. dn_up calls this once per instance — 43 times on one DN VM in the
# default shape — and systemd-udevd watches the rules directories, so an
# unconditional `cat >` would truncate and rewrite a watched file 43 times
# during agent startup, each truncation a window in which the mask is empty.
# (That is the one difference from cnagent_test.sh's copy, which installs once
# per VM and needs no such care.) But the reload is exactly what those 43 calls
# used to retry for free: its status is discarded here, so if the write became
# conditional AND the reload went with it, one failed reload would leave the
# mask byte-correct on disk and stale in udevd for the rest of the run, with
# dn_up reporting udev=present. It is cheap and idempotent, so it runs on both
# paths and a transient failure gets 42 more chances on a DN VM.
install_udev_rule() {
	local want
	want=$(
		cat <<'RULE_EOF'
ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
  IMPORT{program}="/sbin/mdadm --examine --export $devnode"
ENV{MD_NAME}=="dnv-*|*:dnv-*", ENV{SYSTEMD_READY}="0"
RULE_EOF
	)
	if [ -s "$UDEV_RULE" ] &&
		[ "$(cat "$UDEV_RULE" 2>/dev/null)" = "$want" ]; then
		udevadm control --reload >/dev/null 2>&1
		echo present
		return 0
	fi
	printf '%s\n' "$want" >"$UDEV_RULE" || {
		echo "install_udev_rule: writing $UDEV_RULE failed" >&2
		return 1
	}
	[ -s "$UDEV_RULE" ] || {
		echo "install_udev_rule: $UDEV_RULE is missing or empty" >&2
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

# --- loop devices ------------------------------------------------------------

# loop_devs names every loop device this suite attached, found by the backing
# path rather than by an index: that catches an instance the driver no longer
# knows about (a previous run with a larger --dns-per-vm) and a file that has
# already been deleted, which losetup reports as "(deleted)".
loop_devs() {
	losetup -a 2>/dev/null | grep -F "$WORK/" | cut -d: -f1 || true
	return 0
}

# loop_teardown unformats and detaches them. Zeroing the 4 KiB header is
# enough to unformat: the volume-table slots are inert without it (dnagent.md
# DN5, agent/dnagent/diskmeta.go:128 reads exactly common.DnHeaderSize at
# offset 0). conv=fsync and no oflag=, per rule 1.
loop_teardown() {
	local dev
	for dev in $(loop_devs); do
		dd if=/dev/zero of="$dev" bs=4096 count=1 conv=fsync >/dev/null 2>&1
		wipefs -a "$dev" >/dev/null 2>&1
		losetup -d "$dev" >/dev/null 2>&1
	done
	return 0
}

losetup_list() {
	losetup -a 2>/dev/null || true
	return 0
}

# write_zeroes prints one device's write_zeroes_max_bytes, from the same sysfs
# file agent.Dm reads. A 0 is fatal to this suite and not to the agent:
# agent/dnagent/syncup_dn.go:503-517 only TAGS such a disk, so the zeroing
# would fall back to writing real zero pages and materialise every sparse
# backing file in full.
write_zeroes() { # <device>
	local n
	n=$(cat "/sys/class/block/${1##*/}/queue/write_zeroes_max_bytes" \
		2>/dev/null) || n=""
	case "$n" in
	'' | *[!0-9]*) printf '0' ;;
	*) printf '%s' "$n" ;;
	esac
	return 0
}
HELPER_NODE_EOF
}

# --- dn guests --------------------------------------------------------------
helper_dn_source() {
	cat <<'HELPER_DN_EOF'

# dn_up provisions and starts ONE dn agent, and is idempotent: run twice it
# reuses the backing file, the loop device and a live process. Every path and
# every number is an ARGUMENT — the driver computes them with dn_dir /
# dn_backing / dn_store / dn_log / dn_grpc_port / dn_trsvcid / dn_port_id, so
# the D10 layout lives in exactly one place and this side never recomputes it.
#
# It prints one line of key=value pairs and returns non-zero on anything the
# driver must die on. Never run two dn_up concurrently ON ONE GUEST:
# `losetup --find` races with itself.
#
# It installs the md assembly mask first, exactly as cn_up does and for the
# reason set out at install_udev_rule: the leg superblocks the CN writes land
# on THIS guest's storage, and an unmasked DN assembles them behind the suite's
# back. The mask is per GUEST, not per instance — install_udev_rule is a no-op
# once the file is right, so every later call on this VM costs one compare.
# <dir> <backing> <store> <log> <size> <ip> <grpc_port> <trsvcid> <port_id>
dn_up() {
	local dir=$1 backing=$2 store=$3 log=$4 size=$5
	local ip=$6 gport=$7 svcid=$8 portid=$9
	local dev wz pid pat udev

	mkdir -p "$dir" "$store" || {
		echo "dn_up: mkdir $dir / $store failed" >&2
		return 1
	}
	udev=$(install_udev_rule) || {
		echo "dn_up: installing the md assembly mask failed;" \
			"without it this guest's own udev assembles the md legs whose" \
			"superblocks the cn writes through the side export, and the" \
			"arrays pin the dm devices under them against dn_cleanup" >&2
		return 1
	}

	# truncate, NEVER fallocate -l (D15): the file must stay SPARSE, and
	# fallocate would allocate all 2 GiB up front.
	[ -f "$backing" ] || truncate -s "$size" "$backing" || {
		echo "dn_up: truncate -s $size $backing failed" >&2
		return 1
	}
	dev=$(losetup -j "$backing" 2>/dev/null | cut -d: -f1 | head -n 1)
	if [ -z "$dev" ]; then
		dev=$(losetup --find --show "$backing") || {
			echo "dn_up: losetup --find --show $backing failed" >&2
			return 1
		}
	fi
	wz=$(write_zeroes "$dev")
	if [ "$wz" = 0 ]; then
		echo "dn_up: $dev reports write_zeroes_max_bytes=0;" \
			"side zeroing would write real zero pages and" \
			"materialise every sparse backing file on this guest" >&2
		return 1
	fi

	# One agent per gRPC endpoint. The pattern's first character is bracketed
	# so it cannot match this helper's own argv, which carries the ip and the
	# port as separate words and never the flag. The trailing space pins the
	# end of the argument, so the pattern can never match a port that merely
	# starts with these digits.
	pat="[-]-grpc-address $ip:$gport "
	pid=$(pgrep -f -- "$pat" | head -n 1) || pid=""
	if [ -z "$pid" ]; then
		# The agent logs slog JSON to STDERR (common/log.go:99-113), so
		# 2>&1 is what fills the log file at all. >> and not >, so an
		# external truncation resets the write offset.
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
			echo "dn_up: agent for $ip:$gport exited immediately;" \
				"last 20 lines of $log:" >&2
			tail -n 20 "$log" >&2 || true
			return 1
		fi
	fi
	echo "$pid" >"$dir/pid"
	# start_dn_instance reads loop=, wz= and pid= and ignores any other word,
	# so udev= is a report and nothing asserts on it.
	printf 'loop=%s wz=%s pid=%s udev=%s\n' "$dev" "$wz" "$pid" "$udev"
	return 0
}

# dn_cleanup is §7.6's DN half. The order is integtest/dnagent_test.sh's
# cleanup (:661) with one change: the side subsystems' CONTROLLERS are not
# disconnected here, because they belong to the CN guests. That is why
# cleanup_all must finish the CN phases BEFORE it calls this — a subsystem
# unlinked from its port under a live controller kills that controller with
# DNR and the host never reconnects by itself (memory note
# nvmet-port-unlink-dnr-kills-host-ctrl), which is acceptable during teardown
# and not before it.
dn_cleanup() {
	kill_agents dn >/dev/null
	resume_suspended

	# STRAY md ARRAYS FIRST, before any dm device of ours is removed. A DN
	# never assembles an array on purpose — nothing below this line creates
	# one — but the leg superblocks the cn writes through the side export land
	# on this guest, and an unmasked udev assembles them here (see
	# install_udev_rule). Such an array sits ON TOP of the DN stack, holding
	# one of this suite's own dm devices open — the side (kind 4), or the
	# per-CN linear (kind 1) that maps the side 1:1 and carries the same
	# superblock at the same offset; the lab reading named a dm minor and not a
	# kind, and a pinned kind-1 linear holds the kind-4 side open anyway. Either
	# way `dmsetup remove` on an open device fails, and dm_force_remove's
	# fallback — resume, then `remove --force --retry`, bounded at 10 + 10 + 15 s
	# — only swaps in an error table and leaves the device there. So each pinned
	# device can cost the best part of half a minute and survive anyway — twice
	# over where the array sits on the linear, once for the linear and once for
	# the side it goes on holding. On 2026-09-17 dn2 (35 arrays) and dn3
	# (28) both ran past CLEANUP_TIMEOUT that way, and stopping the arrays by
	# hand was what let the identical --cleanup-only finish on all ten
	# guests.
	#
	# It is kept even though install_udev_rule now runs on DN VMs: a guest
	# that ran an older version of this suite, or one whose rule did not take,
	# must still be cleanable BY THE SUITE. md_stop_all matches only an
	# MD_NAME of `dnv-…` or `<homehost>:dnv-…` (see its own comment), so it
	# cannot touch an array of the guest's own.
	#
	# IT ONLY STARTED WORKING ON 2026-09-17. Until then it filtered on a
	# `name=` field `mdadm --detail --scan` does not print on these guests, so
	# it stopped nothing, and the arrays described above were still standing
	# when the dm removals below ran — which is what made every one of them
	# take dm_force_remove's fallback. The cost figure above is that no-op's,
	# not this verb's.
	md_stop_all

	# nvmet first: a namespace must be disabled before the dm device under it
	# can go.
	drop_subsys_glob "$NQN_PREFIX:2:*"
	dm_remove_kind 1 # per-CN linears
	dm_remove_kind 3 # migration final

	# The migration sources are retired after the devices above, because a
	# migration target flushes through its source.
	disconnect_prefix "$NQN_PREFIX:3:"
	drop_subsys_glob "$NQN_PREFIX:3:*"
	dm_remove_kind 5 # migration metadata
	dm_remove_kind 2 # migration source linear
	dm_remove_kind 0 # dn error
	dm_remove_kind 4 # side
	dm_remove_all

	hosts_drop "$@"
	ports_sweep
	resume_suspended
	loop_teardown
	# AFTER loop_teardown, in the same position cn_cleanup_phase2 removes it:
	# the mask may only go once no BLOCK DEVICE on this guest still exposes a
	# dnv md superblock, because that is what a udev event is raised for. Every
	# dm device of ours is gone above, and loop_teardown has just detached
	# every loop device — the superblocks themselves sit at inner offsets of
	# the backing FILES, which the `rm -rf $WORK` below takes, not at the 4 KiB
	# header loop_teardown zeroes. These are shared lab machines and the rule
	# does not belong to them.
	remove_udev_rule >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# dn_residue is what §7.5's smoke teardown asserts on: once the sp is gone, a
# DN guest must hold no dm device and no tree-minted nvmet subsystem of this
# suite. Empty output is the pass.
#
# The loop devices are deliberately NOT in it: they belong to the run, not to
# the sp — the agents keep serving on them until cleanup — so listing them
# here would make the assertion fail on a perfectly clean teardown.
dn_residue() {
	dm_names
	subsys_names | grep -F "$NQN_PREFIX:" || true
	return 0
}

diag() {
	echo "--- dnv-agent processes ---"
	agent_pids dn
	echo "--- dmsetup ls --tree ---"
	dmsetup ls --tree 2>/dev/null
	echo "--- dmsetup status ---"
	dmsetup status 2>/dev/null
	echo "--- suspended dnv dm devices ---"
	suspended_dms
	# md on a DISK NODE is always a fault, and it is one this dump used to
	# hide: the 2026-09-17 stray arrays had to be found by hand, with
	# /proc/mdstat over ssh, after dn_cleanup had already timed out twice.
	echo "--- /proc/mdstat (a DN must show none) ---"
	mdstat
	echo "--- mdadm --detail --scan (a DN must show none) ---"
	mdadm --detail --scan 2>/dev/null
	echo "--- 63-dnv-md.rules ---"
	ls -l "$UDEV_RULE" 2>/dev/null || echo "$UDEV_RULE absent"
	echo "--- nvme subsystems ---"
	subsys_nqns
	echo "--- nvmet configfs ---"
	nvmet_tree
	echo "--- losetup -a ---"
	losetup_list
	echo "--- df -h /var/tmp ---"
	df -h /var/tmp 2>/dev/null
	echo "--- space ---"
	space
	echo "--- dmesg | tail -100 ---"
	dmesg 2>/dev/null | tail -n 100
	return 0
}
HELPER_DN_EOF
}

# --- cn guests --------------------------------------------------------------
helper_cn_source() {
	cat <<'HELPER_CN_EOF'

# tmpfs_teardown releases the cn agent's clone-metadata arena: the loop
# devices over its tmpfs files first, then the mounts, then the directory.
# Copied from integtest/cnagent_test.sh:1223.
tmpfs_teardown() {
	local dev mnt
	for dev in $(losetup -a 2>/dev/null | grep -F "$TMPFS_DIR/" | cut -d: -f1); do
		timeout 15 losetup -d "$dev" >/dev/null 2>&1
	done
	for mnt in "$TMPFS_DIR"/*; do
		[ -d "$mnt" ] || continue
		timeout 15 umount "$mnt" >/dev/null 2>&1
		rmdir "$mnt" 2>/dev/null
	done
	rmdir "$TMPFS_DIR" 2>/dev/null
	return 0
}

# cn_up installs the md mask and starts THIS guest's single cn agent (D12).
# The mask is installed before the agent, never after: the stock rule would
# otherwise assemble an array the agent is in the middle of creating.
#
# No --nvmet-port-id: the cn agent takes the default, common.NvmetPortId = 1
# (cmd/dnv-agent/main.go:88), which is the port cn_cleanup removes. Only the
# dn agents need distinct ids, because only they share a kernel.
cn_up() { # <dir> <store> <log> <ip> <grpc_port> <trsvcid> <capacity>
	local dir=$1 store=$2 log=$3 ip=$4 gport=$5 svcid=$6 cap=$7 pid pat udev

	mkdir -p "$dir" "$store" || {
		echo "cn_up: mkdir $dir / $store failed" >&2
		return 1
	}
	udev=$(install_udev_rule) || {
		echo "cn_up: installing the md assembly mask failed;" \
			"starting the agent without it would let the stock udev rule" \
			"assemble an array the agent is building" >&2
		return 1
	}

	pat="[-]-grpc-address $ip:$gport "
	pid=$(pgrep -f -- "$pat" | head -n 1) || pid=""
	if [ -z "$pid" ]; then
		nohup "$WORK/bin/dnv-agent" cn \
			--grpc-network tcp --grpc-address "$ip:$gport" \
			--tr-type tcp --adr-fam ipv4 \
			--tr-addr "$ip" --tr-svc-id "$svcid" \
			--local-store "$store" --capacity "$cap" \
			>>"$log" 2>&1 </dev/null &
		pid=$!
		sleep 0.5
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "cn_up: agent for $ip:$gport exited immediately;" \
				"last 20 lines of $log:" >&2
			tail -n 20 "$log" >&2 || true
			return 1
		fi
	fi
	echo "$pid" >"$dir/pid"
	# `udev=` is install_udev_rule's own word — `installed` when this call
	# wrote the mask, `present` when it was already exactly right — and not a
	# constant, so the line cannot claim an install that did not happen.
	printf 'pid=%s udev=%s\n' "$pid" "$udev"
	return 0
}

# cn_cleanup_phase1 and cn_cleanup_phase2 are integtest/cnagent_test.sh's
# :1276 / :1293 split, minus its dn half. The split is what makes the ordering
# safe across guests: a dm-clone flushes through its transfer source on
# removal, and that source is an nvmet export on one of these CN guests (D21
# makes the clone source a transfer of the same sp). So every clone on every
# CN goes in phase 1 before any transfer subsystem is dropped in phase 2.
cn_cleanup_phase1() {
	kill_agents cn >/dev/null
	resume_suspended

	# This suite's own host-facing subsystems. The two real hosts hold the
	# controllers, so cleanup_all disconnects them first; this drops what is
	# left and unlinks the subsystems from the port.
	disconnect_prefix "$NQN_IT"
	drop_subsys_glob "$NQN_IT:*"

	dm_remove_kind 6 # ns-dev
	dm_remove_kind 8 # transfer final
	dm_remove_kind 7 # clone final, while its :4: source connection is up
	echo phase1
	return 0
}

cn_cleanup_phase2() { # [extra host nqn…]
	local kind

	# The transfers the phase-1 clones were sourced from.
	disconnect_prefix "$NQN_PREFIX:4:"
	drop_subsys_glob "$NQN_PREFIX:4:*"

	# Top-down through the cn stack, then the arrays, then the leg and group
	# wrappers, then the clone-metadata wrappers (kind b holds the arena's
	# loop device open and would wedge tmpfs_teardown's losetup -d with EBUSY).
	for kind in 5 4 3 2 1 0; do
		dm_remove_kind "$kind"
	done
	md_stop_all
	dm_remove_kind a
	dm_remove_kind 9
	dm_remove_kind b
	disconnect_prefix "$NQN_PREFIX:2:"
	dm_remove_all

	tmpfs_teardown
	resume_suspended

	hosts_drop "$@"
	port_drop "$CN_PORT_ID"
	remove_udev_rule >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

# cn_residue is the CN half of smoke's teardown assertion: no dm device, no
# md array, no nvmet subsystem of this suite may survive the sp.
#
# ITS md LINE IS BLIND AND IS NOT FIXED HERE. `mdadm --detail --scan` prints no
# `name=` field on these guests (md_stop_all's comment has the measurement), so
# the grep below matches nothing whatever this guest holds, and the md third of
# this assertion passes for free. The dm and nvmet lines are unaffected. The
# fix is the one md_stop_all took — /proc/mdstat plus MD_NAME out of `udevadm
# info` — and it belongs with dn_md_residue, which carries the identical grep;
# doc §8 items 8 and 17 record it as open.
cn_residue() {
	dm_names
	subsys_names | grep -F -e "$NQN_PREFIX:" -e "$NQN_IT" || true
	md_names
	return 0
}

diag() {
	echo "--- dnv-agent processes ---"
	agent_pids cn
	echo "--- dmsetup ls --tree ---"
	dmsetup ls --tree 2>/dev/null
	echo "--- dmsetup status ---"
	dmsetup status 2>/dev/null
	echo "--- suspended dnv dm devices ---"
	suspended_dms
	echo "--- /proc/mdstat ---"
	mdstat
	echo "--- mdadm --detail --scan ---"
	mdadm --detail --scan 2>/dev/null
	echo "--- nvme subsystems ---"
	subsys_nqns
	echo "--- nvmet configfs ---"
	nvmet_tree
	echo "--- losetup -a ---"
	losetup_list
	echo "--- tmpfs mounts ---"
	findmnt 2>/dev/null | grep -F "$TMPFS_DIR" || true
	echo "--- df -h /var/tmp ---"
	df -h /var/tmp 2>/dev/null
	echo "--- space ---"
	space
	echo "--- dmesg | tail -100 ---"
	dmesg 2>/dev/null | tail -n 100
	return 0
}
HELPER_CN_EOF
}

# --- host guests ------------------------------------------------------------
helper_host_source() {
	cat <<'HELPER_HOST_EOF'

DISC_NQN=nqn.2014-08.org.nvmexpress.discovery

# identity generates /etc/nvme/hostnqn and /etc/nvme/hostid if they are
# absent and prints both. cdc_test.sh:1644-1646 is the model. It is
# deliberately GENERATE-IF-ABSENT and never overwrite: the kernel keeps a
# strict 1:1 hostnqn<->hostid map, so a file replaced under a live association
# is how the EINVAL of memory note nvme-connect-flag-and-hostid-traps happens,
# and any other user of the guest shares this identity.
#
# For the same reason nothing here ever REMOVES them. §7.6 offers to remove an
# identity the suite created; cdc_test.sh leaves them too, and a hostnqn is
# node identity rather than run state.
identity() {
	mkdir -p /etc/nvme
	[ -s /etc/nvme/hostnqn ] || nvme gen-hostnqn >/etc/nvme/hostnqn
	[ -s /etc/nvme/hostid ] || uuidgen >/etc/nvme/hostid
	if [ ! -s /etc/nvme/hostnqn ] || [ ! -s /etc/nvme/hostid ]; then
		echo "identity: /etc/nvme/hostnqn or /etc/nvme/hostid is still" \
			"empty after gen-hostnqn/uuidgen" >&2
		return 1
	fi
	printf 'hostnqn=%s\n' "$(cat /etc/nvme/hostnqn 2>/dev/null)"
	printf 'hostid=%s\n' "$(cat /etc/nvme/hostid 2>/dev/null)"
	return 0
}

# mask neutralises the kernel's own autoconnector for the whole run (rule 6).
# /usr/lib/udev/rules.d/70-nvmf-autoconnect.rules starts nvmf-connect@.service
# on the discovery AEN this suite's own connects cause (NVME_AEN=0x70f002,
# memory note nvme-discovery-aen-uevent), and a connection made behind the
# suite's back would carry the node's default host id and appear in
# list-subsys as a path nothing here created. cdc_test.sh:1180-1184.
#
# IT VERIFIES RATHER THAN ANNOUNCING. The two `systemctl mask` calls carry
# `|| true` — a mask can fail on a read-only /etc, on a unit systemd does not
# know, or under a systemd that is not running at all — so an unconditional
# `echo masked` would make the driver's `assert_eq "$out" masked` prove nothing
# at all, and rule 6 is the whole basis of "every connect here is the suite's
# own act". `systemctl is-enabled` on a masked unit prints `masked` and exits
# 1, which is why each read carries its own `|| true`; the word is the
# evidence, not the status. Anything else comes back as the two states it
# actually read, so the driver's failure names them.
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

# stas_state reports the two nvme-stas daemons. They must be inactive for the
# whole run: stacd connects on its own and stafd owns discovery controllers,
# and nvme-stas sends a DIM in-capsule to every discovery controller it knows
# (memory note nvme-stas-sends-dim-in-capsule).
stas_state() {
	printf 'stafd=%s stacd=%s\n' \
		"$(systemctl is-active stafd 2>/dev/null || true)" \
		"$(systemctl is-active stacd 2>/dev/null || true)"
	return 0
}

# subsys_nqns / disconnect_prefix, the host copies: this role does not get the
# node body, and a host holds connections, not configfs objects.
subsys_nqns() {
	local sysfs nqn
	for sysfs in /sys/class/nvme-subsystem/*/subsysnqn; do
		[ -e "$sysfs" ] || continue
		nqn=$(cat "$sysfs" 2>/dev/null) || continue
		printf '%s\n' "$nqn"
	done
	return 0
}

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

# disconnect_discovery drops the discovery controllers pointing at one
# address, by DEVICE (-d): a discovery subsystem's NQN is the same well-known
# string for every target, so -n would take down discovery controllers this
# suite never created. cdc_test.sh:1155-1169.
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

# wipe drops every connection to a subsystem whose NQN starts with $NQN_IT or
# $NQN_PREFIX, plus the discovery controllers pointing at the cdc it is given.
# It never uses `nvme disconnect-all`, which would take down subsystems no dnv
# suite has anything to do with (cdc_test.sh:1135-1136).
#
# "EVERY CONNECTION THIS SUITE COULD HAVE MADE AND NOTHING ELSE" IS WHAT THIS
# USED TO CLAIM, AND IT IS FALSE. $NQN_PREFIX is `nqn.2024-01.io.dnv` with no
# terminator, so the prefix test also matches `nqn.2024-01.io.dnv-it:cdc:*` —
# cdc_test.sh's own host-facing subsystems, on the very host guests that suite
# shares with this one (lab note dnv-integtest-lab-vms: .193 and .197 are in
# both). That is convenient at the START, where a dead cdc suite's leftovers
# are debris to be swept, and it is another reason rule 8 forbids running two
# dnv suites at once. The discovery sweep is NOT affected: disconnect_discovery
# is keyed by traddr, so a discovery controller pointing at another suite's cdc
# survives this.
#
# WHAT IS LEFT IS REPORTED. Every `nvme disconnect` above is `|| true` with its
# output thrown away — a disconnect that fails is otherwise completely silent,
# and the one caller that depends on the result unlinks a subsystem right
# afterwards, which kills any surviving controller with DNR. So the sweep ends
# by re-reading sysfs, and a `wipe_left=` line is the evidence that it did not
# finish. The `wiped` sentinel stays LAST: cleanup_report matches it with
# `grep -x` and tolerates other lines, but a reader should still find it where
# it has always been.
wipe() { # [cdc traddr]
	local nqn left=""
	disconnect_prefix "$NQN_IT"
	disconnect_prefix "$NQN_PREFIX"
	if [ -n "${1:-}" ]; then
		disconnect_discovery "$1"
	fi
	for nqn in $(subsys_nqns); do
		case "$nqn" in
		"$NQN_IT"* | "$NQN_PREFIX"*) left="$left $nqn" ;;
		esac
	done
	[ -z "$left" ] || printf 'wipe_left=%s\n' "${left# }"
	echo wiped
	return 0
}

# discover runs ONE `nvme discover` against the cdc and prints its JSON log,
# propagating the exit status: an empty log is {"genctr":N,"records":[]} with
# status 0, while an instance that is down is non-zero, and swallowing the
# difference would make a "serves nothing" assertion pass on a dead cdc
# (cdc_test.sh:552-560).
#
# -q and -I are the host's own identity and are passed explicitly (rule 2):
# nvme-cli fills an omitted -I from /etc/nvme/hostid, so a -q that is not that
# file's partner fails EINVAL under the kernel's 1:1 rule. cdc_test.sh:552-560
# passes the pair for the same reason.
discover() { # <traddr> <trsvcid> <hostnqn> <hostid>
	nvme discover -t tcp -a "$1" -s "$2" -q "$3" -I "$4" -o json
}

# ctrls_of prints one line per nvme controller of subsystem <nqn>, and nothing
# at all when the node holds none. It is the question `nvme connect` and
# `nvme connect-all` DO NOT ANSWER with their exit status (see connect_all).
#
# sysfs and not `nvme list-subsys`: that command prints NOTHING AT ALL on a
# node with no controller (the driver's subsys_json_of exists for exactly that
# shape) and no guest in this suite has a jq to parse it with. This is
# disconnect_discovery's walk with the NQN comparison turned around.
ctrls_of() { # <nqn>
	local ctrl nqn
	for ctrl in /sys/class/nvme/nvme*; do
		[ -e "$ctrl/subsysnqn" ] || continue
		nqn=$(cat "$ctrl/subsysnqn" 2>/dev/null) || continue
		[ "$nqn" = "$1" ] || continue
		printf 'ctrl=%s state=%s addr=%s\n' "${ctrl##*/}" \
			"$(cat "$ctrl/state" 2>/dev/null)" \
			"$(cat "$ctrl/address" 2>/dev/null | tr '\n' ' ')"
	done
	return 0
}

# report_ctrls is the tail both connect verbs print: every controller that
# exists for <nqn> NOW, one per line for the transcript, then the count as a
# machine-readable last line. The driver reads `ctrl_cnt=` and dies on 0.
#
# grep -c and never grep -q, and with its own `|| true`: the preamble sets
# `-o pipefail`, and a reader that closes the pipe early turns SIGPIPE on the
# writer into an abort.
report_ctrls() { # <nqn>
	local found cnt=0
	found=$(ctrls_of "$1")
	if [ -n "$found" ]; then
		printf '%s\n' "$found"
		cnt=$(printf '%s\n' "$found" | grep -c . || true)
	fi
	printf 'ctrl_cnt=%s\n' "$cnt"
	return 0
}

# connect_all is the suite's own act (rule 6): the autoconnector is masked, so
# every path a host holds was made here. --hostnqn and --hostid are always
# both given, for the reason above; --fast_io_fail_tmo, if a caller ever adds
# it through <extra…>, has UNDERSCORES.
#
# `nvme connect-all` EXITS 0 HAVING CONNECTED NOTHING, measured on host0 in
# this lab on 2026-09-17 in TWO shapes, and neither of them is a target
# refusing a connect:
#
#   * NOTHING LISTENING on an address the discovery log advertised. This is
#     run 3's own failure: exit 0, nothing on stdout, nothing on stderr, and
#     `failed to connect socket: -111` — ECONNREFUSED — in the host's dmesg,
#     once per discovery record. nvme-cli reported neither and returned 0.
#   * AN EMPTY DISCOVERY LOG. Presenting a hostnqn that is not in the
#     subsystem's allowed_hosts, DS4 hides the entry (cdc/view.go, doc/cdc.md
#     DS4), the log page comes back empty and there is nothing to connect to:
#     exit 0, nothing on stdout, nothing on stderr, nothing in dmesg, and
#     `nvme list-subsys` empty afterwards.
#
# Read those as two observations rather than a law about nvme-cli: what is
# established is that on this lab's nvme-cli an empty log and a socket that
# refuses both leave rc 0 — not that every failure does. (Run as a non-root
# user the same command fails loudly with EACCES on /dev/nvme-fabrics and
# rc=1, so a silent success is not a sudo question.) So
# the status is reported as a word rather than propagated — cdc_test.sh:818
# swallows it with `|| true` for the same reason — and the SUBSYSTEM NQN is
# taken as an argument purely so report_ctrls can answer the only question that
# matters: does a controller for it exist now. The driver dies on zero
# (connect_verdict); nothing here interprets the number.
connect_all() { # <traddr> <trsvcid> <subnqn> <hostnqn> <hostid> [extra…]
	local a=$1 s=$2 sub=$3 nqn=$4 hid=$5 rc=0
	shift 5
	nvme connect-all -t tcp -a "$a" -s "$s" \
		--hostnqn "$nqn" --hostid "$hid" "$@" 2>&1 || rc=$?
	printf 'rc=%s\n' "$rc"
	report_ctrls "$sub"
	return 0
}

# connect is the single-subsystem form, for a namespace reached without the
# discovery log. It already took the subsystem NQN, because -n needs it; the
# verification tail is the same one for the same reason.
connect() { # <traddr> <trsvcid> <subnqn> <hostnqn> <hostid> [extra…]
	local a=$1 s=$2 sub=$3 nqn=$4 hid=$5 rc=0
	shift 5
	nvme connect -t tcp -a "$a" -s "$s" -n "$sub" \
		--hostnqn "$nqn" --hostid "$hid" "$@" 2>&1 || rc=$?
	printf 'rc=%s\n' "$rc"
	report_ctrls "$sub"
	return 0
}

host_cleanup() { # [cdc traddr]
	wipe "${1:-}" >/dev/null
	unmask >/dev/null
	rm -rf "$WORK"
	echo cleaned
	return 0
}

diag() {
	echo "--- nvme subsystems ---"
	subsys_nqns
	echo "--- nvme list ---"
	nvme list 2>/dev/null
	echo "--- nvme list-subsys ---"
	nvme list-subsys 2>/dev/null
	echo "--- controllers ---"
	local c
	for c in /sys/class/nvme/nvme*; do
		[ -e "$c/subsysnqn" ] || continue
		printf '%s state=%s addr=%s nqn=%s\n' "${c##*/}" \
			"$(cat "$c/state" 2>/dev/null)" \
			"$(cat "$c/address" 2>/dev/null | tr '\n' ' ')" \
			"$(cat "$c/subsysnqn" 2>/dev/null)"
	done
	echo "--- ana states ---"
	local n
	for n in /sys/class/nvme/nvme*/nvme*n*; do
		[ -d "$n" ] || continue
		printf '%s uuid=%s ana_state=%s\n' "$n" \
			"$(cat "$n/uuid" 2>/dev/null)" \
			"$(cat "$n/ana_state" 2>/dev/null)"
	done
	# BOTH units, because the run now depends on both: preflight_host and
	# setup_infra assert the `mask` verb's answer, and that verb fails on
	# either one being unmasked. Dumping only the service would show the unit
	# that is fine and say nothing about the one that failed.
	echo "--- nvmf-connect mask ---"
	systemctl is-enabled nvmf-connect@.service 2>&1 || true
	systemctl is-enabled nvmf-connect.target 2>&1 || true
	echo "--- stas ---"
	stas_state
	echo "--- blocked tasks ---"
	ps -eo stat,pid,comm 2>/dev/null | awk '$1 ~ /D/' || true
	echo "--- space ---"
	space
	echo "--- dmesg | tail -100 ---"
	dmesg 2>/dev/null | tail -n 100
	return 0
}
HELPER_HOST_EOF
}

# --- the cp guest -----------------------------------------------------------
#
# The cp helper is the ONE that runs as a plain user (ssh_cp, not ssh_sudo):
# etcd, the gateway, the worker, the cdc and dnvctl are all unprivileged. So
# nothing here may need root.
#
# Running unprivileged does NOT make the pkill sweeps safe on its own, and
# saying it did would be false: worker_test.sh, gateway_test.sh and
# cdc_test.sh start the very same dnv-worker, dnv-gateway and dnv-cdc binaries
# as the same login user on this same cp guest (the cdc suite's four VMs
# include it — lab note dnv-integtest-lab-vms). What makes them safe is that
# every pattern names something only THIS run's processes carry: $WORK for the
# three dnv binaries, which remote_start launches as "$WORK/bin/dnv-…", and
# `--name $ETCD_NAME` for etcd, whose name is dnv-e2e-it against the other
# suites' dnv-it, dnv-gw-it and dnv-cdc-it and none of which is a substring of
# another. (etcd carries $WORK in its argv too — it is launched as
# "$WORK/bin/etcd" — so either qualification would do; the --name one is kept
# because it is also what keeps a NON-dnv etcd on this guest out of reach.)
helper_cp_source() {
	cat <<'HELPER_CP_EOF'

# stop_all ends the four control-plane processes: first by the pid each
# remote_start recorded, then, only as a fallback for a pid file a crash lost,
# by BRACKETED pattern. CONT before TERM, because a stopped process would
# otherwise never see the TERM.
#
# Every fallback pattern is qualified: the three dnv ones by $WORK, the
# absolute path remote_start launches them by, and etcd by this suite's own
# --name, which is what keeps a bare etcd belonging to somebody else on this
# guest out of reach. A previous e2e run shared this $WORK and this
# $ETCD_NAME, so its corpses — the case these patterns exist for — are still
# swept.
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

# Read-only, and deliberately UNqualified for the same reason agent_pids is:
# a dnv-gateway or dnv-cdc of another suite on this cp guest is exactly what
# the §7.9 dump should make visible. Nothing kills by this list.
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

# reset_etcd is §7.6's between-cases step: every case starts from an EMPTY
# etcd (E2E11), and the data directory is what carries the state. The caller
# stops the four processes first — removing a live etcd's data directory is
# not a reset, it is a corruption.
reset_etcd() {
	if pgrep -f "[e]tcd --name $ETCD_NAME" >/dev/null 2>&1; then
		# Non-zero on purpose: the caller must die rather than run the next
		# case against a half-removed data directory.
		echo "REFUSED: etcd --name $ETCD_NAME is still running" >&2
		return 1
	fi
	rm -rf "$WORK/etcd"
	mkdir -p "$WORK/etcd"
	echo reset
	return 0
}

diag() {
	echo "--- control-plane processes ---"
	cp_pids
	echo "--- listening ports ---"
	ss -ltn 2>/dev/null
	echo "--- df -h /var/tmp ---"
	df -h /var/tmp 2>/dev/null
	echo "--- space ---"
	space
	echo "--- last dnvctl call ---"
	local f
	for f in "$WORK"/last.rc "$WORK"/last.err "$WORK"/last.out; do
		printf -- '--- %s ---\n' "$f"
		[ -f "$f" ] && cat "$f" 2>/dev/null
	done
	return 0
}
HELPER_CP_EOF
}

# --- the dispatcher, last in every role's script ----------------------------
#
# `"$@"` alone (cnagent_test.sh:1389) turns a typo into `command not found`
# with status 127 and no hint. This one names the verb and lists what the
# guest actually has, which is the difference between "the helper is stale"
# and "the driver called the wrong thing".
helper_dispatch_source() {
	cat <<'HELPER_DISPATCH_EOF'

# --- dispatch ---------------------------------------------------------------

__verb=${1:-}
if [ -n "$__verb" ] && declare -F "$__verb" >/dev/null 2>&1; then
	"$@"
	exit $?
fi
if [ -z "$__verb" ]; then
	echo "dnv-e2e-helper.sh: no verb given" >&2
else
	echo "dnv-e2e-helper.sh: unknown verb '$__verb'" >&2
fi
echo "known verbs:" >&2
declare -F | awk '{print "  " $3}' >&2
exit 2
HELPER_DISPATCH_EOF
}

# helper_source writes the whole script for one role to stdout. dn and cn
# share the node body; host and cp do not get it (a host holds no configfs
# object and cp holds no block device, and a verb a role cannot use is a verb
# that can be called there by mistake).
helper_source() { # <dn|cn|host|cp>
	helper_preamble "$1"
	helper_common_source
	case "$1" in
	dn)
		helper_node_source
		helper_dn_source
		;;
	cn)
		helper_node_source
		helper_cn_source
		;;
	host) helper_host_source ;;
	cp) helper_cp_source ;;
	*) die "helper_source: unknown role '$1'" ;;
	esac
	helper_dispatch_source
}

# ship_helpers generates the four scripts, SYNTAX-CHECKS each one on the
# driver and copies it to $HELPER on every guest of that role. It is cheap and
# idempotent, and it runs before every cleanup — including --cleanup-only —
# so that a guest which has never seen this suite still has the tools its own
# teardown needs.
#
# The copy is a plain-user scp: $HELPER is in /var/tmp, not under the
# root-owned $WORK, and `sudo bash $HELPER` reads it perfectly well.
ship_helpers() {
	local tmp role v
	tmp=$(mktemp) || die "mktemp on the driver failed"
	for role in dn cn host cp; do
		helper_source "$role" >"$tmp" || die "generating the $role helper failed"
		# A helper that does not parse would only be found on the guest, by
		# ssh, as a syntax error at a line number of a file nobody has.
		bash -n "$tmp" || die "the generated $role helper is not valid bash"
		case "$role" in
		dn)
			for v in "${!DN[@]}"; do
				scp -q "${SSH_OPTS[@]}" "$tmp" "${DN[$v]}:$HELPER" ||
					die "scp of the dn helper to dn$v failed"
			done
			;;
		cn)
			for v in "${!CN[@]}"; do
				scp -q "${SSH_OPTS[@]}" "$tmp" "${CN[$v]}:$HELPER" ||
					die "scp of the cn helper to cn$v failed"
			done
			;;
		host)
			for v in "${!HOST[@]}"; do
				scp -q "${SSH_OPTS[@]}" "$tmp" "${HOST[$v]}:$HELPER" ||
					die "scp of the host helper to host$v failed"
			done
			;;
		cp)
			scp -q "${SSH_OPTS[@]}" "$tmp" "$CP:$HELPER" ||
				die "scp of the cp helper to cp failed"
			;;
		esac
	done
	rm -f "$tmp"
	local guests=$((1 + CN_CNT + DN_VM_CNT + ${#HOST[@]}))
	log "  helpers shipped to $HELPER on all $guests guests"
}

# ---------------------------------------------------------------------------
# Binaries (§7.2)
# ---------------------------------------------------------------------------

sha256_of() { sha256sum "$1" | cut -d' ' -f1; }

# fetch_etcd is worker_test.sh:923-944 verbatim, byte for byte (only this
# comment differs): the download-and-verify path of the pinned release. It is
# idempotent — a cached tarball whose sha256 already matches the pin is never
# re-downloaded — so this suite needs no network on any repeat run, while a
# fresh checkout still works.
#
# The line number is worth a word: this plan's own commit 1 (99c4688) added
# the workerctl-constants block to worker_test.sh and pushed fetch_etcd down
# by 28 lines, so the ":895-920" every earlier document cites is that file
# BEFORE commit 1. sha256_of, which fetch_etcd calls, is now :917.
fetch_etcd() {
	mkdir -p "$CACHE_DIR"
	if [ -f "$ETCD_TAR" ] && [ "$(sha256_of "$ETCD_TAR")" = "$ETCD_SHA256" ]; then
		log "  etcd $ETCD_VERSION tarball cached and verified"
	else
		log "  downloading $ETCD_URL"
		curl -fsSL -o "$ETCD_TAR.part" "$ETCD_URL" ||
			die "downloading the etcd tarball failed"
		mv -f "$ETCD_TAR.part" "$ETCD_TAR"
		local got
		got=$(sha256_of "$ETCD_TAR")
		[ "$got" = "$ETCD_SHA256" ] ||
			die "etcd tarball sha256 is $got, want $ETCD_SHA256"
	fi
	if [ ! -x "$CACHE_DIR/$ETCD_DIST/etcd" ] ||
		[ ! -x "$CACHE_DIR/$ETCD_DIST/etcdctl" ]; then
		tar -xzf "$ETCD_TAR" -C "$CACHE_DIR" ||
			die "extracting the etcd tarball failed"
	fi
	[ -x "$CACHE_DIR/$ETCD_DIST/etcd" ] || die "no etcd binary after extraction"
	[ -x "$CACHE_DIR/$ETCD_DIST/etcdctl" ] || die "no etcdctl after extraction"
}

# preflight_driver is everything that must hold on THIS machine before a guest
# is touched. The order is fixed by two dependencies: read_constants runs the
# workerctl this function builds, and it parses its JSON with the jq
# resolve_jq picks — and ETCD_MAX_TXN_OPS stays empty until it has run, which
# would make start_etcd pass `--max-txn-ops=` and etcd refuse to start.
#
# cnagentctl and workerctl run on the DRIVER and are built for exactly one
# subcommand each (`host-id --hostnqn`, `constants`). They are cross-built
# GOOS=linux GOARCH=amd64 like everything else here, so they will not exec on
# a driver that is not linux/amd64 — which is one of the things the dies
# below report.
preflight_driver() {
	STAGE="preflight (driver)"
	log "=== preflight: driver"
	local tool
	for tool in go ssh scp curl tar sha256sum awk sed mktemp; do
		need_local "$tool"
	done
	resolve_jq
	log "  make build (CGO_ENABLED=0 GOOS=linux GOARCH=amd64)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build >&2) ||
		die "make build failed"
	local bin
	for bin in "$AGENT_BIN" "$GATEWAY_BIN" "$WORKER_BIN" "$CDC_BIN" \
		"$DNVCTL_BIN"; do
		[ -x "$bin" ] || die "missing: $bin after make build"
	done
	log "  building integtest/bin/{workerctl,cnagentctl}"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$WORKERCTL_BIN" ./integtest/workerctl >&2) ||
		die "building workerctl failed"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o "$CNAGENTCTL_BIN" ./integtest/cnagentctl >&2) ||
		die "building cnagentctl failed"
	read_constants
	fetch_etcd
	log "preflight (driver) ok"
}

# prepare_work creates $WORK on every guest. It must run AFTER the start
# cleanup, which removes $WORK, and BEFORE ship_binaries, which scps into
# $WORK/bin as a plain user.
#
# The per-process directories (one per dn instance, one for the cn, one each
# for etcd/gateway/worker/cdc) are created by the launchers themselves —
# dn_up and cn_up mkdir theirs, and remote_start needs its dir to exist, which
# is why cp gets its four here.
prepare_work() {
	local v
	helper_cp mkwork bin etcd gateway worker cdc >/dev/null ||
		die "creating $WORK on cp failed"
	for v in "${!DN[@]}"; do
		helper_dn "$v" mkwork bin >/dev/null || die "creating $WORK on dn$v failed"
	done
	for v in "${!CN[@]}"; do
		helper_cn "$v" mkwork bin >/dev/null || die "creating $WORK on cn$v failed"
	done
	for v in "${!HOST[@]}"; do
		# The hosts hold no binary; $WORK is where host_sha_probe leaves its
		# three files and where a pattern file lives.
		helper_host "$v" mkwork >/dev/null || die "creating $WORK on host$v failed"
	done
}

# ship_one copies files into one guest's $WORK/bin and makes them executable.
# The scp is the plain user (the account the ssh targets name), which is why
# mkwork chmods the directory 0777.
ship_one() { # <target> <label> <file…>
	local target=$1 label=$2
	shift 2
	log "  scp -> $label:$WORK/bin ($#: $(basename_list "$@"))"
	scp -q "${SSH_OPTS[@]}" "$@" "$target:$WORK/bin/" ||
		die "scp of the binaries to $label failed"
	ssh_to "$target" "chmod 0755 $WORK/bin/*" >/dev/null ||
		die "chmod of $WORK/bin on $label failed"
}

basename_list() {
	local f out=""
	for f in "$@"; do out="$out ${f##*/}"; done
	printf '%s' "${out# }"
}

# ship_binaries puts each guest's share of `make build` where it runs:
#
#   cp        etcd + etcdctl from the pinned cache, dnv-gateway, dnv-worker,
#             dnv-cdc and dnvctl. etcdctl is shipped for READ-ONLY diagnostics
#             only — every control-plane call of this suite is dnvctl (E2E2),
#             and a direct etcd write would be a rule violation, not a
#             shortcut.
#   dn VMs    dnv-agent (one binary, DNS_PER_VM processes).
#   cn VMs    dnv-agent.
#   hosts     nothing: they run the kernel's nvme stack and nvme-cli, no dnv
#             binary at all (D13).
#
# It is sequential on purpose: a parallel scp would report a failure without
# saying which guest it was.
ship_binaries() {
	local v
	ship_one "$CP" cp \
		"$CACHE_DIR/$ETCD_DIST/etcd" "$CACHE_DIR/$ETCD_DIST/etcdctl" \
		"$GATEWAY_BIN" "$WORKER_BIN" "$CDC_BIN" "$DNVCTL_BIN"
	for v in "${!DN[@]}"; do
		ship_one "${DN[$v]}" "dn$v" "$AGENT_BIN"
	done
	for v in "${!CN[@]}"; do
		ship_one "${CN[$v]}" "cn$v" "$AGENT_BIN"
	done
}

# ---------------------------------------------------------------------------
# Process control on cp (gateway_test.sh:490-511, with a target argument)
# ---------------------------------------------------------------------------
#
# The four control-plane processes are launched with `nohup … >> log 2>&1
# </dev/null &`, their pid is recorded from $! in $WORK/<dir>/pid, and they
# are signalled by THAT pid (E2E8). >> and not >, so an external truncation
# resets the write offset instead of leaving a hole; 2>&1 because every dnv
# binary logs to STDERR (common/log.go:99-113) and stdout is reserved for a
# payload; and the redirections together release the ssh channel, which a
# background child still holding the session's pipes would keep open for as
# long as it runs.
#
# PID and RUNNING are keyed by the DIRECTORY name — etcd, gateway, worker,
# cdc — which is also the log's parent and the pid file's parent, so one key
# names all three.
#
# The DN and CN agents deliberately do NOT come through here: they need root,
# they are started through the helper, and they are stopped by bracketed
# pattern or by the pid file the helper wrote.

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

sig_dir() { # <target> <dir> <signal>
	ssh_to "$1" "kill -$3 \$(cat $WORK/$2/pid)"
}

proc_gone() { # <target> <dir>
	! ssh_to "$1" "kill -0 \$(cat $WORK/$2/pid) 2>/dev/null"
}

# NO STEP OF THIS SUITE CALLS IT. Every place a cp daemon is stopped is either
# a teardown or a restart, and both use stop_proc below, which escalates
# instead of dying. wait_gone is kept because it is the form a FUTURE case
# would want — one that signals a daemon and must fail if it does not exit —
# and because stop_proc's comment reads against it.
wait_gone() { # <target> <dir> <secs>
	wait_until "$3" "$2 on ${1##*@} to exit" proc_gone "$1" "$2"
	unset "RUNNING[$2]"
}

# stop_proc is wait_gone's cleanup-safe twin: it escalates instead of dying,
# because it is called from teardown paths where a die would skip the rest of
# the cleanup. QUIET is raised while it polls, exactly as wait_until does, so
# a slow shutdown does not bury the transcript in `kill -0` lines.
stop_proc() { # <target> <dir> [secs]
	local target=$1 dir=$2 secs=${3:-$WAIT_SHORT} i saved=$QUIET
	[ "${RUNNING[$dir]:-0}" = 1 ] || return 0
	QUIET=1
	sig_dir "$target" "$dir" CONT >/dev/null 2>&1 || true
	sig_dir "$target" "$dir" TERM >/dev/null 2>&1 || true
	for ((i = 0; i < secs * 2; i++)); do
		if proc_gone "$target" "$dir"; then
			unset "RUNNING[$dir]"
			QUIET=$saved
			log "  $dir stopped"
			return 0
		fi
		sleep 0.5
	done
	sig_dir "$target" "$dir" KILL >/dev/null 2>&1 || true
	sleep 1
	if proc_gone "$target" "$dir"; then
		unset "RUNNING[$dir]"
		QUIET=$saved
		log "  $dir killed"
		return 0
	fi
	QUIET=$saved
	log "  WARNING: $dir on ${target##*@} survived SIGKILL"
	return 1
}

# ---------------------------------------------------------------------------
# The control plane on cp (D9)
# ---------------------------------------------------------------------------

# start_etcd. --max-txn-ops is NOT a tuning knob: CreateStoragePool at this
# suite's default shape commits 951 compares (967 at MaxCntlrCntPerSp), and an
# etcd below that fails the create with "too many operations in txn request".
# The value is common.EtcdMaxTxnOps, read from `workerctl constants` by
# read_constants — which must therefore have run, or the flag arrives empty
# and etcd refuses to start at all.
start_etcd() {
	[ -n "$ETCD_MAX_TXN_OPS" ] ||
		die "start_etcd before read_constants: --max-txn-ops would be empty"
	remote_start "$CP" etcd etcd.log \
		"$WORK/bin/etcd --name $ETCD_NAME --data-dir $WORK/etcd" \
		"--listen-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--advertise-client-urls http://127.0.0.1:$ETCD_CLIENT_PORT" \
		"--listen-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-advertise-peer-urls http://127.0.0.1:$ETCD_PEER_PORT" \
		"--initial-cluster $ETCD_NAME=http://127.0.0.1:$ETCD_PEER_PORT" \
		"--max-txn-ops=$ETCD_MAX_TXN_OPS"
}

# The three dnv daemons all reach etcd over the loopback, because all four run
# on cp. The gateway binds $CP_IP, not 127.0.0.1: dnvctl runs on cp too (D28)
# but --gateway-address is the address the run was told, and a loopback-only
# gateway would make a later two-machine layout silently impossible.
start_gateway() {
	remote_start "$CP" gateway gateway.log \
		"$WORK/bin/dnv-gateway --grpc-network tcp" \
		"--grpc-address $CP_IP:$GW_PORT" \
		"--etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT"
}

# One worker with all three role loops (D9). --vote-interval and
# --vote-grace-time are short because the react case's four reactions cannot
# fire faster than the vote loop, and every react bound is threshold plus a
# few intervals.
start_worker() {
	remote_start "$CP" worker worker.log \
		"$WORK/bin/dnv-worker --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--roles dn,cn,sp" \
		"--vote-interval $VOTE_INTERVAL --vote-grace-time $VOTE_GRACE"
}

# One cdc, serving every shard: --range is left at its default
# (common.CdcRangeAll, cmd/dnv-cdc/main.go:75-77), which is what makes a
# single instance answer for the whole cluster. Its listener is the address
# both hosts discover against (D13).
start_cdc() {
	remote_start "$CP" cdc cdc.log \
		"$WORK/bin/dnv-cdc --etcd-endpoints 127.0.0.1:$ETCD_CLIENT_PORT" \
		"--tr-type tcp --adr-fam ipv4 --tr-addr $CP_IP --tr-svc-id $CDC_PORT"
}

# cp_port_up is the readiness predicate the launches need between them. It
# answers "something holds that TCP port", which is all the ordering below
# needs: etcd is proven to be listening before the three dnv daemons are
# started, so none of them depends on how its own first etcd dial behaves.
cp_port_up() { # <port>
	helper_cp listening "$1" >/dev/null 2>&1
}

# start_cp_daemons brings the control plane up in dependency order and proves
# each listener before the next process needs it. The worker is the one with
# no wait: it binds no port at all (cmd/dnv-worker/main.go:77-89 declares only
# etcd and vote flags — there is no --grpc-address), so what proves it is
# alive is the work it does, not a socket.
#
# This does NOT prove the gateway SERVES — that is `cluster get` returning,
# which is the setup section's step and needs dnvctl.
start_cp_daemons() {
	start_etcd
	wait_until "$WAIT_SHORT" "etcd to listen on $ETCD_CLIENT_PORT" \
		cp_port_up "$ETCD_CLIENT_PORT"
	start_gateway
	wait_until "$WAIT_SHORT" "the gateway to listen on $GW_PORT" \
		cp_port_up "$GW_PORT"
	start_worker
	start_cdc
	wait_until "$WAIT_SHORT" "the cdc to listen on $CDC_PORT" \
		cp_port_up "$CDC_PORT"
}

# stop_cp_daemons ends them in the reverse order, then sweeps with the
# helper's bracketed pkill for anything whose pid file is gone. It never dies:
# it is called from teardown.
stop_cp_daemons() {
	stop_proc "$CP" cdc || true
	stop_proc "$CP" worker || true
	stop_proc "$CP" gateway || true
	stop_proc "$CP" etcd || true
	helper_cp_ok stop_all >/dev/null
	local dir
	for dir in etcd gateway worker cdc; do unset "RUNNING[$dir]"; done
}

# ---------------------------------------------------------------------------
# The agents (D10, D12)
# ---------------------------------------------------------------------------

# start_dn_instance provisions and starts agent (v, k) and records its loop
# device. It is idempotent: dn_up reuses the backing file, the loop device and
# a live process, so it is also the restart after a react case killed one.
#
# dn_up also installs the 63-dnv-md.rules mask, as cn_up does and before the
# agent for the same reason — on a DN it is the CN's leg superblocks, arriving
# through the side export, that the stock rule would assemble (rule 7). It is
# per guest, so every later instance on a VM finds it already right and does
# nothing.
#
# The write_zeroes gate is dn_up's, not this function's — it must refuse
# BEFORE the agent is launched, since an agent that formats a disk with no
# fast Write Zeroes would materialise the whole sparse file (43 x 2 GiB on a
# 80 GiB guest) and the dn agent itself only TAGS that case
# (agent/dnagent/syncup_dn.go:503-517).
start_dn_instance() { # <v> <k>
	local v=$1 k=$2 out kv dev="" wz="" pid=""
	out=$(helper_dn "$v" dn_up \
		"$(dn_dir "$k")" "$(dn_backing "$k")" "$(dn_store "$k")" \
		"$(dn_log "$k")" "$BACKING_SIZE" "${DN_IP[$v]}" \
		"$(dn_grpc_port "$k")" "$(dn_trsvcid "$k")" "$(dn_port_id "$k")") ||
		die "dn$v instance $k: dn_up failed (its message is above)"
	for kv in $out; do
		case "$kv" in
		loop=*) dev=${kv#loop=} ;;
		wz=*) wz=${kv#wz=} ;;
		pid=*) pid=${kv#pid=} ;;
		esac
	done
	case "$dev" in
	/dev/loop*) ;;
	*) die "dn$v instance $k: dn_up named no loop device, said '$out'" ;;
	esac
	case "$wz" in
	'' | *[!0-9]*)
		die "dn$v instance $k: write_zeroes_max_bytes is '$wz', not a number"
		;;
	esac
	# dn_up already refuses a 0; this is the driver-side echo of that gate, so
	# an edited helper cannot quietly reintroduce it.
	[ "$wz" -gt 0 ] ||
		die "dn$v instance $k: $dev reports write_zeroes_max_bytes=0"
	case "$pid" in
	'' | *[!0-9]*)
		die "dn$v instance $k: no agent pid, said '$out';" \
			"see $(dn_log "$k") on ${DN_IP[$v]}"
		;;
	esac
	DN_LOOP[$(dn_key "$v" "$k")]=$dev
}

# start_dn_vm starts every instance of one VM, sequentially: `losetup --find`
# races with itself on one guest, so the k loop must not be parallelised.
# Different VMs are different kernels and may be run in parallel by the caller.
start_dn_vm() { # <v>
	local v=$1 k
	for ((k = 0; k < DNS_PER_VM; k++)); do
		start_dn_instance "$v" "$k"
	done
	log "  dn$v: $DNS_PER_VM agents, $(dn_addr "$v" 0) .." \
		"$(dn_addr "$v" $((DNS_PER_VM - 1)))"
}

# start_cn_agent starts the one cn agent of a CN VM and, before it, the
# 63-dnv-md.rules mask (cn_up does both, in that order: the stock rule would
# otherwise assemble an array the agent is in the middle of creating).
# --capacity 0 means "no local opinion" and the CP substitutes its default
# (D12).
start_cn_agent() { # <v>
	local v=$1 out kv pid=""
	out=$(helper_cn "$v" cn_up \
		"$(cn_dir)" "$(cn_store)" "$(cn_log)" "${CN_IP[$v]}" \
		"$CN_GRPC_PORT" "$CN_TRSVCID" 0) ||
		die "cn$v: cn_up failed (its message is above)"
	for kv in $out; do
		case "$kv" in
		pid=*) pid=${kv#pid=} ;;
		esac
	done
	case "$pid" in
	'' | *[!0-9]*)
		die "cn$v: no agent pid, said '$out'; see $(cn_log) on ${CN_IP[$v]}"
		;;
	esac
	log "  cn$v: agent on $(cn_addr "$v"), pid $pid"
}

# stop_dn_instance / stop_cn_agent stop ONE agent by the pid its launcher
# recorded — what the react case needs, and what a bracketed `pkill -f` cannot
# express, since every agent of a VM shares the same binary path.
stop_dn_instance() { # <v> <k> [secs]
	helper_dn "$1" kill_pidfile "$(dn_pid_file "$2")" "${3:-15}"
}

stop_cn_agent() { # <v> [secs]
	helper_cn "$1" kill_pidfile "$(cn_pid_file)" "${2:-15}"
}

# ---------------------------------------------------------------------------
# Preflight on the guests (§7.3)
# ---------------------------------------------------------------------------
#
# WHEN THIS RUNS. main's order is preflight_driver -> ship_helpers ->
# cleanup_all -> preflight_guests -> setup, so every check below runs AFTER the
# unconditional start cleanup and BEFORE the first setup write. §7.3's wording
# is "before any cleanup or setup writes"; the port checks and the nvmet-port
# check cannot mean anything there, and the two suites that already do this say
# so in the same place — cnagent_test.sh:1489-1490 ("the port check can only be
# meaningful once a crashed prior run's agents are gone") and
# cdc_test.sh:1601-1602. Nothing in the start cleanup writes suite state: it
# only removes, so no check below reads something this run made.
#
# It dies on the FIRST failure, naming the guest and the fix. A preflight that
# collected three problems and reported them together would still have to be
# re-run after the first one was fixed.
#
# TWO STEPS HERE WRITE, and both are preconditions rather than suite state:
# `modprobe` of the module list, and `mount -t configfs` if /sys/kernel/config
# is not mounted. The agents hardcode the configfs path and neither mount nor
# modprobe anything (agent/nvmet.go:13 `NvmetRoot`), so the harness does it —
# cnagent_test.sh:1505 in its own preflight, for the same reason.
# ---------------------------------------------------------------------------

# Floors, in BYTES (the guest helper's `space` verb answers in bytes, and
# /proc/meminfo's KiB is converted on the driver — one unit everywhere).
#
# MEM_MIN_BYTES is §7.3's 2 GiB. cnagent_test.sh:1522 asks 1.5 GiB for two
# agents; a DN VM here runs DNS_PER_VM of them (43 in the default lab shape)
# and a CN VM holds 64 md arrays and 32 thin pools, so the floor is raised
# rather than copied.
MEM_MIN_BYTES=$((2 << 30))

# §7.3's free-space floors under /var/tmp. FREE_MIN_NODE + DNS_PER_VM x
# FREE_PER_DN on a DN VM, FREE_MIN_NODE on a CN VM, FREE_MIN_CP on cp.
#
# This is a FLOOR, not a bound on what the run may write: §7.8's per-file cap
# is DN_CAP_BYTES (256 MiB) x DNS_PER_VM, which at the default shape is 11 GiB
# if every backing file ran to its cap. The lab's guests hold 56-67 GiB free,
# and F13 is why the real figure is far below the cap — a sparse file over a
# loop device turns the agent's `blkdiscard --zeroout` into a hole punch, so
# only what hosts, md rebuilds and dm-clone hydration really write is
# allocated.
FREE_MIN_NODE=$((4 << 30))
FREE_PER_DN=$((64 << 20))
FREE_MIN_CP=$((2 << 30))

# A cleanup verb that wedges is worse than one that fails: this suite owns all
# ten guests of a shared lab, and a hung ssh leaves the operator to find it.
# Every cleanup verb is therefore invoked as `timeout N bash $HELPER <verb>`
# and its sentinel line is checked — the helper's own internals already bound
# each dmsetup/losetup/nvme call (`timeout 10`/`15`/`30`), and this bounds the
# verb as a whole.
#
# IT IS A WEDGE DETECTOR, NOT A BUDGET, and it is sized for the debris of a
# FAILED run at the widest shape. That is the heavy case: a successful run
# cleans up after itself, so the START cleanup usually finds either nothing or
# the remains of a run that stopped part way.
#
# TWO VERBS ARE HEAVY, and only one of them is understood.
#   - dn_cleanup, on one DN VM: up to DNS_PER_VM instances' worth — 43 in the
#     default shape — of nvmet ports with their ana_groups, 43 loop teardowns
#     (a 4 KiB dd, a wipefs and a losetup -d each), the dm devices of every
#     kind, and now md_stop_all over any stray array the mask did not catch —
#     a bounded `udevadm info` read per array in /proc/mdstat, a bounded
#     `mdadm --detail` read for each array udev could not name, and a bounded
#     `mdadm --stop` for each dnv one.
#   - cn_cleanup_phase2, at least as heavy and the verb that actually blew the
#     old bound twice. On the CN carrying the stack it is disconnect_prefix
#     over every side connection that CN holds (up to 128 in the default shape
#     — 64 arrays x 2 legs — each a `timeout 30 nvme disconnect`), md_stop_all
#     over the 64 arrays at `timeout 15` each, then nine dm kinds plus
#     dm_remove_all, where a device that will not go costs dm_force_remove's
#     10 + 10 + 15 s.
# On 2026-09-17 cn_cleanup_phase2 ran past the 300 s then in force on cn0 and
# cn2. That was written down here as having NO explanation, on the ground that
# the md chain is a DN story and this verb runs before any DN is touched (doc
# §8 item 15, §9). Half of that ground is gone: md_stop_all is ONE function in
# the shared node body and it was a no-op on BOTH roles, so this verb's own
# `md_stop_all` — which sits between the top-of-stack dm kinds and the kind
# a/9/b wrappers precisely to unpin the LEG wrappers — stopped nothing either,
# and every kind-9 leg wrapper under a live array (up to 128 in the default
# shape) would then have gone the long way round through dm_force_remove. Kind
# 9 and not kind a: an md member is a CnLegName device, and CnGrpName is the
# RedundNone group device, which a raid1 group does not have at all
# (common/name_fmt.go:352-391). That is a mechanism, not a finding: what it
# still does not explain is the STANDBY. "Two CNs and not the third" needs no
# explaining: CNTLR_CNT is 2 and --cn is at least 3 (three in the lab), so at
# least one CN carries no cntlr of this sp at all (SPARE_CN_LIST, logged on
# every run) and its
# cn_cleanup_phase2 is a walk over empty `dmsetup ls` output. The other two are
# both heavy — 128 kind-9 leg wrappers and 128 `:2:` connections each — but
# only the PRIMARY has arrays (CN12: "Groups (md.go; primary only — a standby
# has none)"), and the mask is what keeps a stray one off the standby's leg
# wrappers, which carry md superblocks of their own. So the md no-op is a
# mechanism for the CN that was primary and not for the other one. The number
# below was chosen with the standby's overrun unexplained and stays where it is
# until a run measures it.
#
# THE MEASURED FIGURE, and it is the only one there is: on 2026-09-17, after
# the stray arrays of the first run had been stopped by hand, one
# `--cleanup-only` finished on all ten guests inside the 300 s that was in
# force, with no warning and no timeout, leaving ports=0 dm=0 loop=0 md=0 on
# every DN. READ IT NARROWLY: it was the SECOND sweep over that debris. The
# first had run to the end on six of the ten guests and part way on the other
# four (dn2, dn3, cn0, cn2), so the only guests still holding a DN's whole
# port-and-loop debris — 43 and 43, which ports_sweep and loop_teardown sit too
# late in dn_cleanup to have reached — were dn2 and dn3. The per-verb times
# were not recorded either, so 300 s is an upper bound on what was seen and not
# a reading of it.
#
# 600 s is that bound doubled: enough headroom for a failed run at 32 slices to
# leave more than the successful one did, and for a CN timeout nobody has
# explained yet.
#
# WHAT IT DOES NOT BOUND, and the distinction is the whole of rule 5: a task in
# uninterruptible D state. `timeout` sends SIGTERM and then waits for the child
# to be reaped, so an unkillable one is never reaped, there is no 124, no
# WARNING line and no cleanup_start_gate verdict — the run simply stops here.
# That class is reachable inside these verbs (a `dmsetup remove` or a
# block-device scan against a suspended dm device), and resume_suspended
# running first is the only defence there is; this bound catches the KILLABLE
# grind, which is what dm_force_remove against a pinned device is.
#
# The bound is also a wait, and doubling it doubled that too. cleanup_all runs
# its verbs strictly serially — 13 of them in the ten-guest shape (2 host, 3 cn
# phase1, 3 cn phase2, 4 dn, 1 cp), none backgrounded — so a sweep in which
# every one hits the bound is 130 minutes, up from 65, before
# cleanup_start_gate says anything. That is not the operator's first news,
# which is what makes it bearable: cleanup_report prints its WARNING for each
# verb as that verb returns, and the gate afterwards is the summary and the
# verdict, not the first sign.
CLEANUP_TIMEOUT=600
# `fstrim -a` walks every mounted filesystem. It is optional (D24, see
# cleanup_all) so a timeout here is not an error.
FSTRIM_TIMEOUT=120
# One diagnostic dump per guest. Bounded because diagnostics runs when
# something is already wrong, which is exactly when a guest command hangs.
DIAG_TIMEOUT=60
# Lines of each log the failure dump carries (§7.9 says 200).
DIAG_LOG_LINES=200
# How many of one DN VM's agent logs the dump tails in full. A DN VM runs
# DNS_PER_VM agents (43 in the default shape); tailing all of them on all four
# VMs is 34 000 lines, which is a flood and not a diagnostic. Every log that
# holds an ERROR record is still NAMED, whatever this is.
DIAG_MAX_DN_LOGS=6

# The tools each role needs, as a plain word list. These are the ones this
# suite's HELPER and the AGENTS actually run — not cnagent_test.sh:1501's list
# copied over:
#   - no `jq`: no guest in this suite parses JSON. resolve_jq builds one for
#     the DRIVER, and the helper reads sysfs instead (disconnect_prefix,
#     port_trsvcid).
#   - no `thin_dump`: that is cnagent_test.sh's §12 thin-metadata oracle; this
#     suite never reads thin metadata.
#   - no `cmp`: every comparison here is a sha256 string compared on the
#     driver.
# The check runs under the same shell the work runs under — `sudo -n bash -c`
# for dn/cn/host, the plain login user for cp — so it sees the PATH the agents
# will actually inherit.
#
# lsblk and blkdiscard are in the SHARED list because both roles really run
# them: agent/dm.go's DevNo, WriteZeroesMaxBytes and DiskSize are `lsblk`
# (:208, :282, :362), and blkdiscard has one caller per role —
# agent/dnagent/zeroing.go:171 (BlkZeroout, §9.4 side provisioning) and
# agent/cnagent/clonemeta.go:346 (BlkDiscardRange, the clone-metadata arena).
# fallocate is the punch-hole probe below, not anything the run does.
#
# THE STANDARD THESE FOUR LISTS ARE HELD TO, because it is the only way to keep
# them honest: a tool is listed for a role when this suite RUNS it there and
# some other list already declares it worth naming. `cut`, `tr`, `cat`, `ls`,
# `mkdir`, `rm` and `chmod` are run by the helper on every role and appear in no
# list at all — the file treats them as part of a working shell, and changing
# that would mean listing them four times over, not twice.
NODE_TOOLS="dmsetup nvme losetup lsblk blkdiscard stat du df"
# tail: logtail, which the failure dump calls on every role (§7.9).
NODE_TOOLS="$NODE_TOOLS awk sed grep ss pgrep pkill timeout fallocate tail"
# truncate: dn_up's sparse backing file (D15). wipefs and dd: loop_teardown,
# which is the only place either is used and runs on DN VMs alone.
#
# mdadm and udevadm are on a DN for the same verbs they are on a CN for, and
# they are NOT decoration, and udevadm is no longer the milder of the two,
# which is the one thing this note used to get wrong. md_stop_all takes the
# array's name from `udevadm info` and falls back to `mdadm --detail
# --no-devices --export` (its own comment says which array each read misses).
# So mdadm is doubly load-bearing — it is the fallback name source AND the
# thing that does the stopping, and a node without it makes the verb the silent
# no-op of 2026-09-17 outright — while a node without udevadm keeps the stop
# and loses the one name source that survives members mdadm cannot read.
# Preflight is what makes either loud, FOR THE SWEEPS THAT COME AFTER IT:
# cleanup_all runs before preflight_guests (main, §5) and `--cleanup-only`
# never preflights at all, so on a guest missing a tool the start sweep — which
# is where the 2026-09-17 failure happened, and which is also what recovers a
# crashed run — gets one unguarded pass, and so does every `--cleanup-only`
# invocation. From the first between-cases sweep on, the guest has been
# checked. udevadm has two more callers besides:
# install_udev_rule and remove_udev_rule both run `udevadm control --reload`,
# and what that buys is that the mask takes effect AT ONCE. systemd-udevd
# notices a changed rules directory on its own — that is the same property
# install_udev_rule's conditional write is written around — so a missing reload
# leaves the mask stale for as long as udevd takes to see it, with the agent
# already starting, and not unloaded for ever. (The mask's own IMPORT program
# is mdadm as well, though by the absolute path udev rules use; `command -v` is
# the proxy for it here, exactly as it is on a CN.)
DN_TOOLS="$NODE_TOOLS truncate wipefs dd mdadm udevadm"
# mdadm: the cn agent (agent/cnagent/md.go:44-168) and md_stop_all. udevadm:
# md_stop_all's name read and install_udev_rule's reload. findmnt: the cn
# diag's tmpfs listing.
CN_TOOLS="$NODE_TOOLS mdadm udevadm findmnt"
# The hosts run no dnv binary at all (D13). nvme: every connect and disconnect.
# uuidgen: /etc/nvme/hostid when it is absent. systemctl: the nvmf-connect mask
# and the stafd/stacd check. dd + sha256sum: the §7.4 IO helpers. udevadm is
# the proxy for a working udev, because /dev/disk/by-id/nvme-uuid.<uuid> is a
# udev symlink and host_dev has no other stable name to use (cdc_test.sh:1642
# requires it on a host for the same reason).
#
# du/df/tail are here because a HOST runs the same common helper body the nodes
# do: case_space_guard and space_note_case call `space` on both hosts (du for
# $WORK, df for the free-space line) and the failure dump calls `logtail` on
# them for $WORK/sha-probe.err. They were missing while the verbs were not.
HOST_TOOLS="nvme uuidgen systemctl udevadm dd sha256sum awk sed grep timeout"
HOST_TOOLS="$HOST_TOOLS du df tail"
# cp runs four unprivileged daemons and dnvctl. `ss` is what the helper's
# `listening` predicate uses; pkill/pgrep are stop_all's fallback sweep.
#
# grep and timeout are cp's too, and for reasons outside the helper body:
# cleanup_verb and dump_cp wrap EVERY cp verb in `timeout N bash $HELPER …`
# (a missing timeout would make the cp cleanup exit 127 and leave $WORK), and
# the failure dump runs `grep_log` there over the four daemon logs.
CP_TOOLS="ss pgrep pkill awk sed du df tail nohup grep timeout"

# The module list of cnagent_test.sh:1505 minus dm-flakey, which only that
# suite's fault injection needs. modprobe is best-effort on purpose: a module
# built into the kernel makes `modprobe` fail, and what matters is the assert
# that follows (the nvmet configfs tree, /proc/mdstat, multipath).
NODE_MODULES="nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-clone dm-thin-pool raid1"

# tools_cmd builds the ONE-LINE guest command that prints the missing tools,
# one per line, and nothing when they are all there. `$b` survives the
# single-quoted format and is expanded by the guest's shell.
tools_cmd() { # <tool…>
	printf 'for b in %s; do command -v $b >/dev/null 2>&1 || echo $b; done; true' "$*"
}

# modules_cmd is cnagent_test.sh:1505 as a function. Best-effort modprobe, then
# the configfs mount if it is missing, then `true` so the caller's status is
# the ssh's and not the last modprobe's.
modules_cmd() { # <module…>
	printf 'for m in %s; do modprobe $m 2>/dev/null || true; done;' "$*"
	printf ' grep -q " /sys/kernel/config " /proc/mounts ||'
	printf ' mount -t configfs none /sys/kernel/config; true'
}

# space_field pulls one key out of the helper's `space` output (work=, tmpfs=,
# free=), all three in bytes.
space_field() { # <space output> <key>
	printf '%s\n' "$1" | sed -n "s/^$2=//p"
}

# assert_bytes dies unless a value read off a guest is a number at or above a
# floor. The explicit numeric case is what makes an empty answer — a `df` that
# failed, a stale helper — say so: `[ "" -ge 4 ]` reports "integer expression
# expected" and the run would die naming the FLOOR instead of the missing
# value.
assert_bytes() { # <got> <floor> <label>
	case "$1" in
	'' | *[!0-9]*) die "$3: '$1' is not a byte count" ;;
	esac
	[ "$1" -ge "$2" ] || die "$3: $1 bytes, want >= $2"
}

# port_owner_hint names the suite an nvmet port seems to belong to, from its
# transport service id. §7.7 requires the refusal to say that, and the answer
# is only useful if it is derived from the suites' own declarations:
# dnagent_test.sh:34 and cnagent_test.sh:46 both set TR_SVC_ID=4200;
# cdc_test.sh:85 sets NVMET_PORT_BASE=14420 for its four ports 14420..14423.
# gateway_test.sh's 4420/4421/4429 are payload its fake records and never
# binds, so a real port on 4420 is the nvme-tcp default and not that suite's.
port_owner_hint() { # <trsvcid>
	case "$1" in
	'' | none)
		printf 'the port carries no addr_trsvcid, so it cannot be attributed'
		return 0
		;;
	*[!0-9]*)
		printf 'addr_trsvcid reads %q, which is not a number' "$1"
		return 0
		;;
	4200)
		printf 'trsvcid 4200 is the dn/cn agent suites'
		printf ' (dnagent_test.sh:34, cnagent_test.sh:46)'
		return 0
		;;
	1442[0-3])
		printf 'trsvcid %s is the cdc suite' "$1"
		printf ' (cdc_test.sh:85, NVMET_PORT_BASE=14420)'
		return 0
		;;
	4420 | 4421)
		printf 'trsvcid %s is the nvme-tcp default port:' "$1"
		printf ' a hand-made target, not a dnv suite'
		return 0
		;;
	esac
	# Everything else is judged by this suite's own band, so that a port of
	# OURS that the cleanup could not remove is not reported as a stranger's.
	if [ "$1" -ge "$TRSVCID_MIN" ] && [ "$1" -le "$TRSVCID_MAX" ]; then
		printf "trsvcid %s is inside THIS suite's own band %s..%s," \
			"$1" "$TRSVCID_MIN" "$TRSVCID_MAX"
		printf ' so it is a port this run left behind or could not remove'
	else
		printf 'trsvcid %s matches no dnv suite in this repo' "$1"
	fi
	return 0
}

# port_cleanup_cause says why a port is STILL THERE after the start cleanup has
# already swept this guest. It is written against port_drop's own branches and
# not against an impression of them, because the second run of 2026-09-17 died
# on exactly that difference: it told the operator the cleanup had "refused" a
# port whose trsvcid was 4300 — inside this suite's own band, which is the one
# case port_drop REMOVES.
#
# port_drop (the helper) decides on the service id alone, and has FOUR outcomes,
# not three:
#   empty      -> accepted, removed as debris (an agent killed between the
#                 mkdir and the first attribute write)
#   in band    -> accepted, removed; it is ours
#   otherwise  -> REFUSED, and left exactly as it was found
#   accepted, and the rmdir did not take -> `port N STUCK …`, and port_drop
#                 still returns 0, so the verb prints its sentinel and
#                 cleanup_start_gate does NOT die on it (cleanup_report raises
#                 CLEANUP_DIRTY and prints a `!!!` line instead)
# so only the third is a deliberate refusal. STUCK is the one that reaches a
# preflight die, and naming it is the whole point of this function.
#
# WHY IT IS THE ONLY LIVE CAUSE at the id-collision die. port_drop is called for
# every id a sweep of this run would visit — ports_sweep walks 1..MAX_DNS_PER_VM
# on a DN, cn_cleanup_phase2 drops CN_PORT_ID on a CN — and cleanup_start_gate
# has already killed the run if any verb failed to reach its sentinel. So for a
# port whose id is inside this run's range and whose trsvcid port_drop accepts,
# "the sweep never got there" is ruled out, and so is "no sweep visits that id":
# what is left is a port_drop that tried and failed. The unswept-id alternative
# is real only at the SECOND die, where the id may be outside every range this
# suite sweeps (a CN port 7, a DN port past MAX_DNS_PER_VM) and the collision is
# on the service id — which is why <swept> is a parameter and not a sentence.
port_cleanup_cause() { # <trsvcid, possibly empty> <swept: yes|unknown>
	case "$1" in
	'')
		printf 'The start cleanup did NOT refuse it: port_drop removes a'
		printf ' port with an empty addr_trsvcid as debris, so this one was'
		printf ' not left alone on purpose.'
		;;
	*[!0-9]*)
		printf 'The start cleanup refused to remove it, which is deliberate:'
		printf ' its addr_trsvcid is not a number, so it is not a port this'
		printf ' suite can claim. Remove it by hand, or stop the suite that'
		printf ' owns it.'
		return 0
		;;
	*)
		if [ "$1" -ge "$TRSVCID_MIN" ] && [ "$1" -le "$TRSVCID_MAX" ]; then
			printf 'The start cleanup did NOT refuse it: %s is inside' "$1"
			printf ' %s..%s, the band port_drop removes, so this one was' \
				"$TRSVCID_MIN" "$TRSVCID_MAX"
			printf ' not left alone on purpose.'
		else
			printf 'The start cleanup refused to remove it, which is'
			printf ' deliberate: %s is outside %s..%s, so the port was never' \
				"$1" "$TRSVCID_MIN" "$TRSVCID_MAX"
			printf " this suite's to remove. Remove it by hand, or stop the"
			printf ' suite that owns it.'
			return 0
		fi
		;;
	esac
	# The two "port_drop would have removed this" cases share one remedy, and it
	# names the causes in the order the evidence leaves them.
	printf ' The likeliest cause by far is that port_drop ACCEPTED it and the'
	printf ' rmdir did not take: search the sweep just above for a `!!!` line'
	printf ' and a `port <id> STUCK` for THIS id — that line names the'
	printf ' ana_groups and subsystems still in the directory, which is what'
	printf ' would not go.'
	if [ "${2:-unknown}" != yes ]; then
		printf ' Failing that, the port id may be one no sweep of this suite'
		printf ' visits (dn: 1..%s, cn: %s), in which case nothing here has' \
			"$MAX_DNS_PER_VM" "$CN_PORT_ID"
		printf ' ever looked at it.'
	fi
	# NOT "look for a WARNING line": by this point there cannot be one.
	# cleanup_report prints WARNING exactly when a verb missed its sentinel,
	# that is what fills CLEANUP_UNFINISHED, and cleanup_start_gate dies on it
	# before preflight runs. Sending the operator to grep for a line the
	# control flow excludes is the 2026-09-17 mistake in a new place.
	printf ' A cleanup verb that stopped part way is NOT a candidate here:'
	printf ' cleanup_start_gate would have killed the run before preflight.'
	printf ' One thing it cannot rule out is a stranger: if another dnv suite'
	printf ' or a human made this port AFTER the sweep, it is E2E9 that was'
	printf ' broken, not the cleanup.'
	printf ' Re-run with --cleanup-only; if it survives that, rmdir it by'
	printf ' hand, ana_groups/3 and /2 first.'
	return 0
}

# nvmet_ports_of lists one guest's nvmet ports as "<id>:<trsvcid>" lines, with
# the service id space-stripped (nvmet reads several addr_* attributes back
# space-padded — memory note nvmet-configfs-idempotency, and cdc_test.sh:1072
# strips it in the same comparison). An absent /sys/kernel/config/nvmet leaves
# the glob unmatched and the output empty, which is the right answer: nvmet
# state lives in the module and cannot outlive it.
#
# <ssh-wrapper> is ssh_dn or ssh_cn — the sudo forms, since the port
# attributes are root-readable only.
nvmet_ports_of() { # <ssh-wrapper> <index>
	local cmd
	cmd="for p in $NVMET/ports/*; do [ -d \"\$p\" ] || continue;"
	cmd="$cmd printf '%s:%s\\n' \"\${p##*/}\""
	cmd="$cmd \"\$(cat \"\$p/addr_trsvcid\" 2>/dev/null | tr -d ' ')\"; done"
	"$1" "$2" "$cmd"
}

# assert_no_nvmet_conflict refuses to start on a guest that already carries an
# nvmet port this run would collide with. Two distinct collisions, and neither
# is caught by the ports_busy check (a configfs port is not a listening socket
# until a subsystem is linked to it):
#
#   1. An id this run will use. EnsurePort is probe-first but NOT read-only on
#      a port that already exists: it reuses the directory and REWRITES
#      addr_trtype/addr_adrfam/addr_traddr/addr_trsvcid through ensureAttr
#      (agent/nvmet.go:114 EnsurePort, the rewrite loop at :129-137). So an
#      agent given --nvmet-port-id k+1 would hijack a stranger's port rather
#      than fail, and break whatever owns it.
#   2. A service id this run will bind. Two nvmet ports cannot listen on one
#      ip:port, so the agent's own port would come up dead.
#
# The start cleanup has already run to the end on every guest at this point —
# cleanup_start_gate stops the run otherwise — so a port that is still here is
# foreign (port_drop REFUSED it), stuck (port_drop said STUCK — a `!!!` line,
# which the start gate does not die on), outside every sweep's id range, or
# made after the sweep by something that should not be running (E2E9). It is
# NOT one the cleanup skipped, and port_cleanup_cause is where each message
# gets that distinction right — getting it wrong here is what sent the
# 2026-09-17 diagnosis after the wrong cause.
#
# THE TWO DIES PASS DIFFERENT <swept> ARGUMENTS, and the difference is load
# bearing. The first fires only for 1 <= id <= idmax, and idmax is DNS_PER_VM
# on a DN (never above MAX_DNS_PER_VM, parse_args refuses that) or CN_PORT_ID
# on a CN — every one of which a sweep of this run walked, so `yes`. The second
# fires on the service id whatever the port id is, so an id no sweep visits is
# a real answer there and it gets `unknown`.
assert_no_nvmet_conflict() { # <label> <idmax> <svclo> <svchi> <listing>
	local label=$1 idmax=$2 lo=$3 hi=$4 listing=$5 tok id svc
	for tok in $listing; do
		id=${tok%%:*}
		svc=${tok#*:}
		case "$id" in
		'' | *[!0-9]*)
			log "  $label: nvmet port '$id' has a non-numeric name, ignored"
			continue
			;;
		esac
		if [ "$id" -ge 1 ] && [ "$id" -le "$idmax" ]; then
			die "$label: $NVMET/ports/$id already exists" \
				"(addr_trsvcid=${svc:-none}) and this run needs that id." \
				"$(port_owner_hint "$svc")." \
				"An agent given that id would rewrite its addr_*" \
				"attributes (agent/nvmet.go:129-137), so the run stops here." \
				"$(port_cleanup_cause "$svc" yes)"
		fi
		case "$svc" in
		'' | *[!0-9]*) continue ;;
		esac
		if [ "$svc" -ge "$lo" ] && [ "$svc" -le "$hi" ]; then
			# The same cause sentence as the id collision above, from the
			# same function: this port's id is out of the range this run
			# uses, but its service id is one of ours, so port_drop would
			# have removed it too and "refused" would be just as wrong here.
			# `unknown` and not `yes`: the id that got here may be one no
			# sweep of this suite walks, which is an answer the first die
			# cannot have.
			die "$label: $NVMET/ports/$id listens on $svc, which is inside" \
				"this run's band $lo..$hi, so one of its agents could not" \
				"bind. $(port_owner_hint "$svc")." \
				"$(port_cleanup_cause "$svc" unknown)"
		fi
	done
}

# preflight_node is the §7.3 dn/cn block. <role> picks the ssh and helper
# wrappers by name, the way ana_of takes a wrapper name, so the two roles share
# every check they share and differ only where the design differs.
preflight_node() { # <role: dn|cn> <index>
	local role=$1 v=$2 label="$1$2"
	local sshw=ssh_dn helperw=helper_dn target="" want_tools=$DN_TOOLS
	if [ "$role" = dn ]; then
		target=${DN[$v]}
	else
		sshw=ssh_cn
		helperw=helper_cn
		target=${CN[$v]}
		want_tools=$CN_TOOLS
	fi

	ssh_to "$target" "sudo -n true" >/dev/null ||
		die "$label ($target): passwordless sudo is required" \
			"(configfs, dm, md, loop devices and nvme all need root)"

	local missing
	missing=$("$sshw" "$v" "$(tools_cmd $want_tools)") ||
		die "$label: listing the installed tools failed"
	[ -z "$missing" ] ||
		die "$label: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"

	"$sshw" "$v" "$(modules_cmd $NODE_MODULES)" >/dev/null ||
		die "$label: loading the kernel modules / mounting configfs failed"

	# Every read below carries `|| echo MISSING` on the guest and `|| die` on
	# the driver: the first turns an absent file into a readable assert
	# failure, the second catches an ssh that did not run at all — without it
	# `set -e` would end the run on the assignment, with no message but the
	# stage name.
	local got
	got=$("$sshw" "$v" "ls -d $NVMET 2>/dev/null || echo MISSING") ||
		die "$label: reading $NVMET failed"
	assert_eq "$got" "$NVMET" "$label: the nvmet configfs tree"

	# nvme_core.multipath is load-bearing on BOTH roles, and on the dn for a
	# reason that is easy to miss: a migration DESTINATION is an nvme host —
	# agent/dnagent/migr.go:116 calls s.host.Connect — and the agent reads its
	# source's state out of /sys/class/nvme-subsystem, where the namespace head
	# (nvme0n1) and the hidden per-path device that carries ana_state
	# (nvme0c1n1) only exist when multipath is on (agent/nvmehost.go:113-130).
	got=$("$sshw" "$v" \
		"cat /sys/module/nvme_core/parameters/multipath 2>/dev/null" \
		"|| echo MISSING") ||
		die "$label: reading nvme_core.multipath failed"
	assert_eq "$got" "Y" "$label: nvme_core.multipath"

	# BOTH node roles, and the DN is not the weaker case of the two. On a CN
	# md support is what the agent needs to build an array at all. On a DN
	# /proc/mdstat is what proves the guest CAN assemble one — which is
	# precisely the hazard: the leg superblocks the cn writes land on the DN
	# through the side export, so an unmasked DN assembles them behind the
	# suite's back (install_udev_rule, rule 7).
	got=$("$sshw" "$v" "ls /proc/mdstat 2>/dev/null || echo MISSING") ||
		die "$label: reading /proc/mdstat failed"
	assert_eq "$got" /proc/mdstat "$label: md support"
	# The 63-dnv-md.rules mask dn_up and cn_up install works by setting
	# SYSTEMD_READY=0 (cnagent_test.sh:1037), which only suppresses the stock
	# incremental assembly if the stock rule honours it. Both roles install
	# the mask, so both roles need the precondition checked: on a DN a stock
	# rule that ignored SYSTEMD_READY would leave the mask inert and the stray
	# arrays would come straight back.
	got=$("$sshw" "$v" \
		"for d in /usr/lib/udev/rules.d /lib/udev/rules.d; do" \
		"f=\$d/64-md-raid-assembly.rules;" \
		"if [ -r \$f ] && grep -q SYSTEMD_READY \$f; then echo FOUND; break; fi;" \
		"done; true") ||
		die "$label: reading 64-md-raid-assembly.rules failed"
	assert_eq "$got" FOUND \
		"$label: the stock 64-md-raid-assembly.rules honours SYSTEMD_READY"

	# The punch-hole probe. `fallocate` is the right tool here and the D15 ban
	# does not touch it: the ban is on `fallocate -l` for a BACKING file, which
	# must stay sparse, while this probe's whole purpose is to prove that
	# FALLOC_FL_PUNCH_HOLE works on /var/tmp — which is what turns the agent's
	# `blkdiscard --zeroout` over a loop device into a hole punch (F13) and
	# what the whole space argument of §7.8 rests on.
	got=$("$sshw" "$v" "f=/var/tmp/dnv-e2e-punch-probe;" \
		"fallocate -l 8M \$f && fallocate -p -o 0 -l 4M \$f" \
		"&& echo PUNCH_OK || echo PUNCH_NO; rm -f \$f") ||
		die "$label: the punch-hole probe did not run"
	assert_eq "$got" PUNCH_OK "$label: /var/tmp supports hole punching"

	got=$("$sshw" "$v" "awk '/MemAvailable/ {print \$2}' /proc/meminfo") ||
		die "$label: reading /proc/meminfo failed"
	case "$got" in
	'' | *[!0-9]*) die "$label: /proc/meminfo MemAvailable reads '$got'" ;;
	esac
	assert_bytes "$((got * 1024))" "$MEM_MIN_BYTES" "$label: MemAvailable"

	local out free
	out=$("$helperw" "$v" space) || die "$label: the space verb failed"
	free=$(space_field "$out" free)
	local floor=$FREE_MIN_NODE
	if [ "$role" = dn ]; then
		floor=$((FREE_MIN_NODE + DNS_PER_VM * FREE_PER_DN))
	fi
	assert_bytes "$free" "$floor" "$label: free bytes under /var/tmp"

	# Ports. On a DN VM that is this run's whole gRPC and trsvcid block —
	# 29900..29900+DNS_PER_VM-1 and 4300..4300+DNS_PER_VM-1, both from the
	# layout accessors so the arithmetic stays in one place. On a CN VM it is
	# the two fixed ports.
	local ports="" k
	if [ "$role" = dn ]; then
		for ((k = 0; k < DNS_PER_VM; k++)); do
			ports="$ports $(dn_grpc_port "$k") $(dn_trsvcid "$k")"
		done
	else
		ports="$CN_GRPC_PORT $CN_TRSVCID"
	fi
	local busy
	busy=$("$helperw" "$v" ports_busy $ports) ||
		die "$label: the ports_busy verb failed"
	[ -z "$busy" ] ||
		die "$label: port(s) already listening after the start cleanup:" \
			"$(printf '%s' "$busy" | tr '\n' ' ')— something else is using" \
			"them, or an agent of a previous run survived the cleanup"

	# The nvmet port conflict (§7.3 has it as "the port ranges free"; a
	# configfs port is not a socket, so it needs its own check).
	local listing idmax svclo svchi
	listing=$(nvmet_ports_of "$sshw" "$v") ||
		die "$label: listing the nvmet ports failed"
	if [ "$role" = dn ]; then
		idmax=$DNS_PER_VM
		svclo=$(dn_trsvcid 0)
		svchi=$(dn_trsvcid $((DNS_PER_VM - 1)))
	else
		idmax=$CN_PORT_ID
		svclo=$CN_TRSVCID
		svchi=$CN_TRSVCID
	fi
	assert_no_nvmet_conflict "$label" "$idmax" "$svclo" "$svchi" "$listing"

	log "  $label ok"
}

# preflight_host is the §7.3 host block (D13). The hosts run no dnv binary:
# what they must have is the kernel's nvme stack, an identity, no competing
# autoconnector and no nvme-stas.
preflight_host() { # <h>
	local h=$1 label="host$1" target=${HOST[$1]} got out

	ssh_to "$target" "sudo -n true" >/dev/null ||
		die "$label ($target): passwordless sudo is required" \
			"(nvme connect, /etc/nvme and systemctl all need root)"

	local missing
	missing=$(ssh_host "$h" "$(tools_cmd $HOST_TOOLS)") ||
		die "$label: listing the installed tools failed"
	[ -z "$missing" ] ||
		die "$label: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"

	ssh_host "$h" "modprobe nvme-tcp" >/dev/null ||
		die "$label: modprobe nvme-tcp failed"
	got=$(ssh_host_user "$h" "nvme version 2>/dev/null | head -1") ||
		die "$label: nvme-cli is not runnable"
	log "  $label nvme-cli: $got"

	# Without multipath the host gets one block device per PATH instead of one
	# head per namespace, and host_dev's /dev/disk/by-id/nvme-uuid.<uuid> —
	# the only stable name (the head's own number moves between reconnects) —
	# would not name the thing this suite reads and writes.
	got=$(ssh_host "$h" \
		"cat /sys/module/nvme_core/parameters/multipath 2>/dev/null" \
		"|| echo MISSING") ||
		die "$label: reading nvme_core.multipath failed"
	assert_eq "$got" "Y" "$label: nvme_core.multipath"

	# The identity files, generated if absent and NEVER overwritten (the
	# kernel's 1:1 hostnqn<->hostid rule). This is the generation §7.4 and
	# read_host_identity depend on; read_host_identity only reads.
	out=$(helper_host "$h" identity) ||
		die "$label: generating /etc/nvme/hostnqn and /etc/nvme/hostid" \
			"failed: $out"
	read_host_identity "$h"

	# stafd/stacd must not be running for the whole run: stacd connects on its
	# own, stafd owns discovery controllers, and nvme-stas sends a DIM
	# in-capsule to every discovery controller it learns about (memory note
	# nvme-stas-sends-dim-in-capsule). "unknown" or "failed" is fine — the
	# failure mode is a RUNNING one, and `active`/`activating` are the words
	# systemctl uses for that.
	out=$(helper_host "$h" stas_state) || die "$label: the stas_state verb failed"
	case "$out" in
	*stafd=active* | *stacd=active*)
		die "$label: nvme-stas is running ($out). Stop it for the run:" \
			"systemctl stop stafd stacd"
		;;
	esac
	log "  $label nvme-stas: $out"

	# Rule 6: the kernel's own autoconnector matches the discovery AEN this
	# suite's connects cause and would make paths nothing here created.
	# The word is the evidence: `mask` re-reads both units with
	# `systemctl is-enabled` and prints `service=… target=…` when either is not
	# masked, so this assert_eq now fails with the states it actually found
	# instead of passing on an unconditional echo.
	out=$(helper_host "$h" mask) || die "$label: masking nvmf-connect failed"
	assert_eq "$out" masked \
		"$label: the nvmf-connect@.service and nvmf-connect.target masks (rule 6)"

	log "  $label ok"
}

# preflight_cp is the §7.3 cp block. cp is the one guest this suite drives as a
# plain user, so there is deliberately no sudo check here — `fstrim -a` at end
# cleanup is the only root thing cp is ever asked for, and its caller tolerates
# a refusal (ssh_cp_sudo_ok).
preflight_cp() {
	local missing busy out free

	ssh_cp "true" >/dev/null || die "cp ($CP): passwordless ssh failed"

	missing=$(ssh_cp "$(tools_cmd $CP_TOOLS)") ||
		die "cp: listing the installed tools failed"
	[ -z "$missing" ] ||
		die "cp: missing tool(s): $(printf '%s' "$missing" | tr '\n' ' ')"

	busy=$(helper_cp ports_busy \
		"$ETCD_CLIENT_PORT" "$ETCD_PEER_PORT" "$GW_PORT" "$CDC_PORT") ||
		die "cp: the ports_busy verb failed"
	[ -z "$busy" ] ||
		die "cp: port(s) already listening after the start cleanup:" \
			"$(printf '%s' "$busy" | tr '\n' ' ')— etcd needs" \
			"$ETCD_CLIENT_PORT/$ETCD_PEER_PORT, the gateway $GW_PORT," \
			"the cdc $CDC_PORT"

	out=$(helper_cp space) || die "cp: the space verb failed"
	free=$(space_field "$out" free)
	assert_bytes "$free" "$FREE_MIN_CP" "cp: free bytes under /var/tmp"

	log "  cp ok"
}

# preflight_guests is what main calls. Order: reachability of all ten first (a
# guest that is simply down should say so before a tool list does), then cp,
# then the DN VMs, the CN VMs and the hosts.
preflight_guests() {
	STAGE="preflight (guests)"
	TRACE=it-preflight
	log ""
	log "=== preflight: guests"

	local t v
	for t in "$CP" "${DN[@]}" "${CN[@]}" "${HOST[@]}"; do
		ssh_to "$t" "true" >/dev/null ||
			die "passwordless ssh to $t failed (BatchMode is on: an agent," \
				"a key or a known_hosts entry is missing)"
	done

	preflight_cp
	for v in "${!DN[@]}"; do preflight_node dn "$v"; done
	for v in "${!CN[@]}"; do preflight_node cn "$v"; done
	for v in "${!HOST[@]}"; do preflight_host "$v"; done

	# cdc_test.sh:1649-1655's assertion: two hosts that share a hostnqn are
	# one host to the target, `ss set-hosts` would name the same entry twice,
	# and the copy case's "host1 sees the transfer, host0 does not" could not
	# be told apart.
	assert_ne "${HOST_NQN[0]}" "${HOST_NQN[1]}" "the two hosts' hostnqn"

	log "preflight (guests) ok"
}

# preflight_loop_devices is §7.3's last item, deferred because the devices only
# exist once the agents have been started: every loop device this run attached
# must report a non-zero write_zeroes_max_bytes.
#
# It is the THIRD gate on that number, on purpose. dn_up refuses to launch an
# agent over a 0 (it is the only one that can, since the agent must not exist
# yet), start_dn_instance re-asserts what dn_up reported, and this one re-reads
# every device from the driver's own DN_LOOP record. What it adds is that the
# record is COMPLETE — DN_TOTAL devices, one per (v, k) — so a setup that
# silently started fewer agents than DNS_PER_VM cannot reach `sp create` and
# fail there as RESOURCE_EXHAUSTED.
#
# A 0 is fatal to this suite and not to the agent: agent/dnagent/syncup_dn.go's
# disk syncup only TAGS such a disk, so side zeroing would fall back to writing
# real zero pages and materialise every sparse backing file on the guest.
#
# WHY EARLY AND NOT MERELY EVENTUALLY. ensureDiskMeta does return
# `t.Err(resKeyMeta, s.disk, details)` for a tagged disk
# (agent/dnagent/syncup_dn.go:490-491), so dn_node_ready's meta_info row would
# never reach RES_STATUS_OK and setup WOULD fail at its own wait — after
# WAIT_PROVISION per disk, with a message about a header rather than
# about a kernel attribute, and with the agent free to have been writing real
# zero pages the whole time (checkWriteZeroes "never gates converging", its own
# comment at :502-505). This check turns that into one named line.
preflight_loop_devices() {
	local v k key dev devs out kv n
	assert_eq "${#DN_LOOP[@]}" "$DN_TOTAL" \
		"loop devices recorded by start_dn_instance"
	for v in "${!DN[@]}"; do
		devs=""
		for ((k = 0; k < DNS_PER_VM; k++)); do
			key=$(dn_key "$v" "$k")
			dev=${DN_LOOP[$key]:-}
			[ -n "$dev" ] ||
				die "dn$v instance $k: no loop device recorded;" \
					"start_dn_vm $v did not run"
			devs="$devs $dev"
		done
		# One read-only ssh per VM, not one per device: 43 devices x 4 VMs
		# would be 172 round trips for a gate that is already held twice.
		out=$(ssh_dn "$v" "for d in $devs; do" \
			"printf '%s=%s\\n' \"\$d\"" \
			"\"\$(cat /sys/class/block/\${d##*/}/queue/write_zeroes_max_bytes" \
			"2>/dev/null)\"; done") ||
			die "dn$v: reading write_zeroes_max_bytes failed"
		for kv in $out; do
			dev=${kv%%=*}
			n=${kv#*=}
			case "$n" in
			'' | *[!0-9]*)
				die "dn$v: $dev reports write_zeroes_max_bytes='$n';" \
					"the device is gone or sysfs cannot be read"
				;;
			esac
			[ "$n" -gt 0 ] ||
				die "dn$v: $dev reports write_zeroes_max_bytes=0, so side" \
					"zeroing would write real zero pages and materialise" \
					"every sparse backing file on this guest"
		done
	done
	log "  write_zeroes_max_bytes > 0 on all $DN_TOTAL loop devices"
}

# ---------------------------------------------------------------------------
# Cleanup (§7.7 at the start, §7.6 at the end — ONE function, run at both)
# ---------------------------------------------------------------------------
#
# cleanup_all is called from four sites over a run's life: unconditionally by
# main before anything is built (§7.7), by setup_between_cases before each case
# after the first (§7.5, E2E11 — that one is a START cleanup too, for the case
# it precedes), by on_exit on SUCCESS (§7.6), and by `--cleanup-only` on its
# own. So a four-case run calls it five times. It is written to be tolerant of
# total absence — every guest call tolerates a non-zero status, either as an
# _ok form or, for the verbs, through cleanup_verb's `|| rc=$?` — and never dies:
#
#   - at the START a die would be wrong (leftovers are what it is for, and
#     preflight is the thing that judges whether the guest is now usable);
#   - at the END a die would skip the rest of the cleanup on the other nine
#     guests, which is the opposite of what a dirty lab needs.
#
# cleanup_all itself therefore still never dies — but a verb that never ran is
# not a leftover, and the START caller (main, setup_between_cases) follows this
# with cleanup_start_gate, which does. See CLEANUP_UNFINISHED.
#
# What it does instead is REPORT, loudly, through cleanup_report: a missing
# sentinel line, and any REFUSED or STUCK nvmet port. Those two words are the
# ones that matter to the next person, because a leftover port with live ana
# groups is what fails the NEXT suite's setup (memory note
# nvmet-port-teardown-ana-groups).
#
# Reporting is not the end of it. cleanup_report raises CLEANUP_DIRTY for the
# two findings that mean THIS run left something behind — a missing sentinel
# and a STUCK port — and on_exit's success branch and --cleanup-only both turn
# that into a non-zero exit and a banner naming the guests. A run whose every
# assertion passed but whose cleanup did not is not a PASS: the next suite is
# the one that would discover it, hours later, as a setup failure it did not
# cause.
#
# THE ORDER IS LOAD-BEARING, and it is the plan's:
#
#   1. HOSTS FIRST. They hold the controllers over this suite's own
#      subsystems. A subsystem unlinked from its port under a live controller
#      kills that controller with DNR and the host never reconnects by itself
#      (memory note nvmet-port-unlink-dnr-kills-host-ctrl) — acceptable during
#      teardown, but only after the host has stopped issuing IO.
#   2. ALL CNs, cleanup_phase1. Every clone goes before any transfer
#      subsystem is dropped: a dm-clone flushes through its transfer source on
#      removal, and D21 makes that source an nvmet export on a CN.
#   3. ALL CNs, cleanup_phase2 — and only then
#   4. the DN VMs. A CN's dm stack sits on nvme connections to the DN sides;
#      dropping the sides first would leave the CN's md legs on dead paths.
#   5. cp: the four daemons by their recorded pids, then the helper's
#      bracketed pkill sweep and `rm -rf $WORK`.
#   6. `fstrim -a` everywhere (D24).
#
# Both CN phases must finish on EVERY CN before the first DN VM is touched,
# which is why steps 2 and 3 are separate loops rather than one loop doing both.
# ---------------------------------------------------------------------------

# cleanup_report judges one guest's cleanup output. <sentinel> is the word that
# verb echoes when it ran to the end (dn_cleanup/cn_cleanup_phase2/host_cleanup
# /cp_cleanup say "cleaned", cn_cleanup_phase1 says "phase1"). <rc> is the
# status of the whole `timeout N bash $HELPER …` — 124 is `timeout`'s own,
# which is the one finding that names its cause exactly.
cleanup_report() { # <label> <verb> <sentinel> <rc> <output>
	local label=$1 verb=$2 want=$3 rc=$4 out=$5 hits svc why
	if [ "$(printf '%s\n' "$out" | grep -cx "$want" || true)" = 0 ]; then
		# One string per branch, never a continuation: `why="a" "b"` would
		# run `b` with why in its environment instead of assigning both.
		case "$rc" in
		124) why="it timed out after ${CLEANUP_TIMEOUT}s" ;;
		255) why="rc 255: the ssh to this guest failed, or the verb exited 255" ;;
		0) why="it exited 0 without the line, so the helper there is stale" ;;
		*) why="it exited $rc" ;;
		esac
		log "  WARNING: $label cleanup verb '$verb' did not reach its" \
			"'$want' line — $why. Output:"
		printf '%s\n' "$out" >&2
		CLEANUP_DIRTY=1
		CLEANUP_UNFINISHED="$CLEANUP_UNFINISHED$label $verb $rc"$'\n'
	fi
	hits=$(printf '%s\n' "$out" | grep -F -e REFUSED -e STUCK || true)
	# STUCK raises CLEANUP_DIRTY and REFUSED does not: port_drop refuses a port
	# whose addr_trsvcid is outside TRSVCID_MIN..TRSVCID_MAX, i.e. one that was
	# never this suite's to remove, while STUCK is a port of ours that would
	# not go. Both are printed; only one is this run's fault.
	if [ "$(printf '%s\n' "$hits" | grep -cF STUCK || true)" != 0 ]; then
		CLEANUP_DIRTY=1
	fi
	if [ -n "$hits" ]; then
		log "  !!! $label: the nvmet port sweep did not finish cleanly."
		printf '%s\n' "$hits" >&2
		for svc in $(printf '%s\n' "$hits" |
			sed -n 's/.*trsvcid=\([0-9][0-9]*\).*/\1/p'); do
			log "      $(port_owner_hint "$svc")"
		done
		log "      REFUSED means the port is not this suite's and was left" \
			"alone on purpose (§7.7); STUCK means it would not go."
		log "      Either way a leftover nvmet port fails the NEXT suite's" \
			"setup — clear it by hand: rmdir ana_groups/3 and /2 first," \
			"then the port."
	fi
}

# cleanup_dirty_banner is what an operator sees when the suite's own work was
# fine and the lab is not. It names the two findings that raise CLEANUP_DIRTY
# and the hand commands for the one that breaks other suites; the per-guest
# detail is already above it in the transcript, printed by cleanup_report as
# it happened.
cleanup_dirty_banner() {
	log ""
	log "##############################################################"
	log "LAB NOT CLEAN — nothing this run TESTED failed, but the end"
	log "cleanup did not finish on at least one of the ten guests."
	log ""
	log "Search this transcript upwards for 'WARNING:' (a cleanup verb that"
	log "never printed its sentinel: it timed out after ${CLEANUP_TIMEOUT}s,"
	log "the guest was unreachable, or the helper is stale) and for 'STUCK'"
	log "(an nvmet port of this suite that would not go)."
	log ""
	log "A leftover nvmet port fails the NEXT suite's setup. On the guest:"
	log "  sudo rmdir $NVMET/ports/<id>/ana_groups/3"
	log "  sudo rmdir $NVMET/ports/<id>/ana_groups/2"
	log "  sudo rmdir $NVMET/ports/<id>"
	log "in that order — the port first says 'Directory not empty'."
	log ""
	log "\`$0 … --cleanup-only\` re-runs the whole §7.7 sweep and exits"
	log "non-zero again if it still cannot finish."
	log ""
	log "On a DN VM the known cause of a verb that will not finish is a stray"
	log "md array over one of this suite's dm devices — the side, or the"
	log "per-CN linear that maps it: \`cat /proc/mdstat\` there, then, for"
	log "each mdN it lists,"
	log "  udevadm info --query=property --name=/dev/mdN | grep MD_NAME"
	log "and \`sudo mdadm --stop /dev/mdN\` for every one whose MD_NAME is"
	log "\`dnv-…\` or \`<homehost>:dnv-…\`. Neither /proc/mdstat nor"
	log "\`mdadm --detail --scan\` prints the name on these guests, which is"
	log "why the udev property is the one to read. udev has no MD_NAME for an"
	log "array whose state is \`inactive\` — which is what a one-member"
	log "assembly is — so for those ask mdadm instead:"
	log "  mdadm --detail --no-devices --export /dev/mdN | grep MD_NAME"
	log "and if THAT is empty too, mdadm cannot read the member superblock"
	log "(an error target under it, after a \`dmsetup remove --force\`): the"
	log "array is ours if its members in /proc/mdstat are this suite's dm"
	log "devices, and \`mdadm --stop\` is still the way out. dn_cleanup stops"
	log "them itself, so an array that is still there means mdadm or udevadm"
	log "is missing on that guest, or neither read named it, or"
	log "\`mdadm --stop\` would not take."
	log "##############################################################"
}

# cleanup_start_gate is the START cleanup's verdict, and the one place in this
# file where a cleanup kills the run.
#
# E2E6 makes the start cleanup tolerant of ABSENCE, and it is: a guest with
# nothing on it runs every verb to the end and prints its sentinel, and
# CLEANUP_UNFINISHED stays empty. A verb that never printed its sentinel is the
# opposite of absence — it means the sweep did not run to the end and the
# debris it was supposed to remove is still there. Going on from that puts the
# run into a preflight failure about whatever the debris collides with FIRST,
# which is a misleading place to stop: on 2026-09-17 two DN VMs timed out in
# dn_cleanup (35 and 28 stray md arrays, each pinning a dm device of ours), the
# run continued, and preflight died on an nvmet port with a message that
# blamed a refusal the cleanup had never made — three steps and one wrong
# explanation away from the actual fault.
#
# THE END CLEANUP IS NOT GATED HERE AND MUST NOT BE. on_exit runs it only after
# every assertion has passed, and there a die would skip the other nine guests;
# it reports instead, and CLEANUP_DIRTY carries the verdict into the exit code
# and cleanup_dirty_banner. --cleanup-only is an end cleanup by the same
# argument and is left alone too.
#
# THE REMEDY IS BUILT FROM WHAT WAS RECORDED, not from the one case that has
# been seen. This gate runs BEFORE preflight_guests, so it is now the first
# thing an unreachable guest, or one without passwordless sudo, runs into: a
# fixed DN-md remedy would send that operator to `cat /proc/mdstat` on a guest
# they cannot ssh to, when what they need is preflight's own sentence about
# ssh and sudo. Each row of CLEANUP_UNFINISHED carries a label and a status,
# and the paragraphs below are keyed off exactly those two.
cleanup_start_gate() { # <what this cleanup was: for the message>
	local what=$1 label verb rc remedy=""
	local saw_dn="" saw_cn="" saw_255="" saw_0="" saw_other_to=""
	[ -n "$CLEANUP_UNFINISHED" ] || return 0
	log ""
	log "  the $what cleanup did not finish on:"
	# A here-string and not a pipe, so these assignments are made in THIS
	# shell and survive the loop.
	while read -r label verb rc; do
		[ -n "$label" ] || continue
		if [ "$rc" = 124 ]; then
			log "    $label: '$verb' timed out after ${CLEANUP_TIMEOUT}s"
		else
			log "    $label: '$verb' exited $rc without its sentinel"
		fi
		case "$label:$rc" in
		dn*:124) saw_dn=1 ;;
		cn*:124) saw_cn=1 ;;
		*:124) saw_other_to=1 ;;
		esac
		case "$rc" in
		255) saw_255=1 ;;
		0) saw_0=1 ;;
		esac
	done <<<"$CLEANUP_UNFINISHED"
	if [ -n "$saw_dn" ]; then
		remedy="$remedy THE KNOWN CAUSE OF A DN TIMEOUT is a stray md array"
		remedy="$remedy over one of this suite's own dm devices — the cn's leg"
		remedy="$remedy superblocks travel down the side export and an unmasked"
		remedy="$remedy udev assembles them on the DN, where the array pins the"
		remedy="$remedy side (kind 4) or the per-CN linear over it (kind 1) and"
		remedy="$remedy dm_remove_kind cannot remove either. Check with"
		remedy="$remedy \`cat /proc/mdstat\` on that guest (a DN must show"
		remedy="$remedy no dnv array), and read each one's name with"
		remedy="$remedy \`udevadm info --query=property --name=/dev/mdN |"
		remedy="$remedy grep MD_NAME\` — neither /proc/mdstat nor"
		remedy="$remedy \`mdadm --detail --scan\` prints it on these guests."
		remedy="$remedy An \`inactive\` array (a one-member assembly) has no"
		remedy="$remedy MD_NAME in udev at all: for those, \`mdadm --detail"
		remedy="$remedy --no-devices --export /dev/mdN\`."
	fi
	if [ -n "$saw_cn" ]; then
		remedy="$remedy A CN TIMEOUT HAS NO CONFIRMED CAUSE: cn_cleanup_phase2"
		remedy="$remedy ran past the bound on two CN VMs on 2026-09-17 (doc"
		remedy="$remedy §9). The candidate is the same md no-op the DNs had —"
		remedy="$remedy md_stop_all is one function on both roles and stopped"
		remedy="$remedy nothing until 2026-09-17, so this verb's kind-9 leg"
		remedy="$remedy wrappers stayed pinned under live arrays —"
		remedy="$remedy but it covers the PRIMARY only, since only a primary"
		remedy="$remedy assembles arrays (CN12); the standby holds as many leg"
		remedy="$remedy wrappers and no array, and a spare CN holds no cntlr"
		remedy="$remedy at all (CNTLR_CNT=2), so it is the standby's overrun"
		remedy="$remedy that is"
		remedy="$remedy open. On the CN carrying the stack"
		remedy="$remedy the verb is as heavy as anything here — up to 128"
		remedy="$remedy \`nvme disconnect\`s, 64 \`mdadm"
		remedy="$remedy --stop\`s and a whole 32-slice dm stack — so start with"
		remedy="$remedy \`dmsetup ls --tree\` and \`cat /proc/mdstat\` there,"
		remedy="$remedy and record what you find."
	fi
	if [ -n "$saw_other_to" ]; then
		remedy="$remedy A TIMEOUT ON A host OR cp VERB is unrecorded: those"
		remedy="$remedy verbs are small — a few nvme disconnects and an unmask"
		remedy="$remedy on a host, pids and an rm -rf on cp — so the guest is"
		remedy="$remedy wedged rather than the sweep being long. Note that"
		remedy="$remedy \`timeout\` cannot end a task in D state, so a verb"
		remedy="$remedy that hit THAT would not have reported 124 at all."
	fi
	if [ -n "$saw_255" ]; then
		remedy="$remedy AN rc OF 255 IS THE ssh ITSELF, not the sweep: that"
		remedy="$remedy guest is unreachable or has no passwordless sudo."
		remedy="$remedy Preflight's own check for that has not run yet — it is"
		remedy="$remedy the next step — so fix the guest and re-run."
	fi
	if [ -n "$saw_0" ]; then
		remedy="$remedy AN EXIT OF 0 WITHOUT THE SENTINEL means the helper on"
		remedy="$remedy that guest is stale: ship_helpers did not land, or an"
		remedy="$remedy older copy of $HELPER is there. It is not a wedge."
	fi
	die "the $what cleanup did not run to the end on the guest(s) named" \
		"above, so their debris is still there and nothing preflight or" \
		"any case says next would be about this run.$remedy" \
		"Re-run \`$0 … --cleanup-only\`: it sweeps every guest again and" \
		"exits non-zero if it still cannot finish."
}

# cleanup_verb runs one cleanup verb on one guest, bounded, and reports it. The
# `timeout` is why this does not go through helper_*: those wrappers build
# `bash $HELPER …` themselves and there is nowhere to put a bound. A wedged
# cleanup would otherwise hang the run with no message at all, on a lab the
# operator has to reach by hand.
#
# Only STDOUT is captured: the guest's stderr and the wrapper's own `[ip] …`
# line stay live in the transcript, and every line this reads — the sentinel,
# `port N REFUSED …`, `port N STUCK …` — is printed on stdout by the helper.
#
# THE WRAPPER IS THE DYING FORM (ssh_dn, not ssh_dn_ok) and the status is
# caught here instead: `|| rc=$?` keeps cleanup_all's "never dies" promise
# exactly as the _ok form did, and it keeps `timeout`'s 124, which is the
# difference between "this guest was already clean" and "this guest still has
# everything on it". The _ok forms threw that away.
cleanup_verb() { # <ssh-wrapper> <index|""> <label> <sentinel> <verb…>
	local sshw=$1 idx=$2 label=$3 want=$4 out rc=0
	shift 4
	# ${1:-} and not $1: `set -u` is on, and a caller that forgot the verb
	# would otherwise die here instead of reporting an empty one.
	local verb=${1:-}
	if [ -n "$idx" ]; then
		out=$("$sshw" "$idx" "timeout $CLEANUP_TIMEOUT bash $HELPER $*") ||
			rc=$?
	else
		out=$("$sshw" "timeout $CLEANUP_TIMEOUT bash $HELPER $*") || rc=$?
	fi
	cleanup_report "$label" "$verb" "$want" "$rc" "$out"
}

cleanup_all() {
	local v extra=""

	# Cleared here and not at the top of the file: cleanup_all runs five times
	# in a full four-case run (the start sweep, one per between-cases step, the
	# end one), and only the LAST says anything about the state this run leaves
	# the lab in. See CLEANUP_DIRTY's own comment. CLEANUP_UNFINISHED goes with
	# it, and for the same reason: cleanup_start_gate must judge THIS sweep.
	CLEANUP_DIRTY=0
	CLEANUP_UNFINISHED=""

	# The two real hosts' /etc/nvme/hostnqn entries, so the nvmet hosts/ groups
	# the agents made for them are swept too. They follow the kernel's own
	# nqn.2014-08.org.nvmexpress format and match no prefix of ours, so they
	# have to be named. They are only known at the END: at the start
	# HOST_NQN is still empty, and a leftover empty hosts/ entry is harmless
	# (the agent's ensure path is probe-first).
	if [ "${#HOST_NQN[@]}" -gt 0 ]; then
		for v in "${!HOST_NQN[@]}"; do
			if [ -n "${HOST_NQN[$v]}" ]; then
				extra="$extra ${HOST_NQN[$v]}"
			fi
		done
	fi

	log "--- cleanup: hosts (they hold the controllers)"
	for v in "${!HOST[@]}"; do
		cleanup_verb ssh_host "$v" "host$v" cleaned host_cleanup "$CP_IP"
	done

	log "--- cleanup: cn phase 1 (clones, before any transfer source goes)"
	for v in "${!CN[@]}"; do
		cleanup_verb ssh_cn "$v" "cn$v" phase1 cn_cleanup_phase1
	done

	log "--- cleanup: cn phase 2"
	for v in "${!CN[@]}"; do
		cleanup_verb ssh_cn "$v" "cn$v" cleaned cn_cleanup_phase2 $extra
	done

	# Only now the DN VMs. dn_cleanup kills every [d]nv-agent from the helper,
	# then per instance drops the :2:/:3: subsystems and the dm kinds, sweeps
	# ports 1..MAX_DNS_PER_VM (which is §7.7's "any leftover ports/<k> for k in
	# 2..MAX_DNS_PER_VM", plus port 1), zeroes each loop's first 4 KiB with
	# conv=fsync, wipefs, losetup -d, and removes $WORK.
	log "--- cleanup: dn VMs"
	for v in "${!DN[@]}"; do
		cleanup_verb ssh_dn "$v" "dn$v" cleaned dn_cleanup $extra
	done

	log "--- cleanup: cp"
	stop_cp_daemons
	cleanup_verb ssh_cp "" cp cleaned cp_cleanup

	# D24. Without the operator's `discard='unmap'` change to each domain's
	# vda <driver> line (plan §1.3), the guest filesystem has nothing to
	# forward a discard to and this is a harmless no-op — which is the lab's
	# state as of 2026-09-17, checked then: no `discard=` on any domain. With
	# it, every block this run dirtied is returned to the qcow2 image. It is
	# best-effort on every guest, cp included, where this suite otherwise
	# never asks for root.
	log "--- cleanup: fstrim -a (a no-op until the images get discard='unmap')"
	for v in "${!DN[@]}"; do
		ssh_dn_ok "$v" "timeout $FSTRIM_TIMEOUT fstrim -a" >/dev/null 2>&1
	done
	for v in "${!CN[@]}"; do
		ssh_cn_ok "$v" "timeout $FSTRIM_TIMEOUT fstrim -a" >/dev/null 2>&1
	done
	for v in "${!HOST[@]}"; do
		ssh_host_ok "$v" "timeout $FSTRIM_TIMEOUT fstrim -a" >/dev/null 2>&1
	done
	ssh_cp_sudo_ok "timeout $FSTRIM_TIMEOUT fstrim -a" >/dev/null 2>&1

	log "--- cleanup done"
}

# reset_control_plane recycles the control plane ALONE: stop the four daemons,
# throw the etcd data directory away, start them again. The agents and the
# backing files are deliberately left running.
#
# IT IS NOT THE BETWEEN-CASES STEP AND NOTHING CALLS IT. §7.6 asks for exactly
# this between cases, and it is not enough: a second `cluster create` mints a
# new cluster_id (fnv64a over the name and a fresh creation_epoch,
# gateway/cluster.go:59 + model/keys.go:112-120) and new dn_ids, and every DN's
# 4 KiB disk header still names the old ones, so EnsureFormatted refuses each
# one as a "foreign disk" and never re-formats
# (agent/dnagent/diskmeta.go:299-324). main therefore calls
# setup_between_cases, which is cleanup_all + setup_infra + setup_case. This
# stays defined
# because it is the right tool for a case that wants a fresh etcd WITHOUT
# rebuilding the data plane, and no case in §7.5 is one.
#
# The order is forced: reset_etcd REFUSES while etcd runs (removing a live
# etcd's data directory is a corruption, not a reset), so the daemons stop
# first. CLUSTER_ID is cleared with the data it came from: the next case's
# `cluster create` mints a new one, and every NQN builder calls
# require_cluster_id, so a case that forgot to re-read it dies with that
# message instead of computing an NQN for a cluster that no longer exists.
reset_control_plane() {
	stop_cp_daemons
	CLUSTER_ID=""
	helper_cp reset_etcd >/dev/null ||
		die "resetting etcd on cp failed: the four daemons are stopped," \
			"so this is not the 'etcd is still running' refusal"
	start_cp_daemons
}

# ---------------------------------------------------------------------------
# Diagnostics on failure (§7.9)
# ---------------------------------------------------------------------------
#
# "Keep everything": on_exit calls this INSTEAD of the cleanup, so every
# process, dm/md/nvmet object, loop device, host connection and log is still
# there when this runs and stays there afterwards.
#
# Three things shape it.
#
#  a. ORDER. cp's helper `diag` prints $WORK/last.{rc,err,out} — the failing
#     dnvctl call's three streams — and ctl_exec OVERWRITES those three files
#     on every call. So cp's dump runs BEFORE any dnvctl read below it, or the
#     suite would print the diagnostics' own last call instead of the one that
#     failed.
#  b. IT MUST NOT DIE. on_exit runs `diagnostics || true`, which suspends
#     set -e inside it, but `die` calls `exit` and would end the run with the
#     other eight guests undumped. Every guest call is an _ok form and every
#     dnvctl read goes through diag_ctl, which runs in a SUBSHELL so that a
#     die inside ctl_exec (its one die, on a failed ssh) is contained.
#  c. IT MUST BE BOUNDED. Diagnostics runs when something is already wrong,
#     which is when a guest command hangs; each dump is `timeout
#     $DIAG_TIMEOUT bash $HELPER …`.
#
# A DN VM holds DNS_PER_VM agent logs (43 in the default shape). Dumping all
# 172 is not a diagnostic, so the DN logs are selected: every log with an ERROR
# record is named, and the instances a case registered with diag_note_dn are
# dumped in full.
# ---------------------------------------------------------------------------

# DIAG_DN holds "<v>:<k>" keys — the dn instances a case knows are involved in
# what it is doing, so the failure dump can show their logs. A case registers
# one with diag_note_dn before the step that might fail; it is advisory, and an
# empty list simply means the dump falls back to instance 0 of every DN VM.
DIAG_DN=()

diag_note_dn() { # <v> <k>
	DIAG_DN+=("$(dn_key "$1" "$2")")
}

# The four bounded dump wrappers. They are ssh_*_ok plus a `timeout`, which is
# the one thing helper_* cannot express.
dump_dn() { # <v> <verb…>
	local v=$1
	shift
	ssh_dn_ok "$v" "timeout $DIAG_TIMEOUT bash $HELPER $*"
}

dump_cn() { # <v> <verb…>
	local v=$1
	shift
	ssh_cn_ok "$v" "timeout $DIAG_TIMEOUT bash $HELPER $*"
}

dump_host() { # <h> <verb…>
	local h=$1
	shift
	ssh_host_ok "$h" "timeout $DIAG_TIMEOUT bash $HELPER $*"
}

dump_cp() { # <verb…>
	ssh_cp_ok "timeout $DIAG_TIMEOUT bash $HELPER $*"
}

# diag_ctl is a read-only dnvctl call for the dump. The subshell is what makes
# it safe: ctl_exec dies if the ssh itself fails, and a die here would end the
# run before the rest of the diagnostics.
#
# The CTL_* globals it fills are the SUBSHELL's, so they do not come back — a
# caller that needs the reply text uses diag_ctl_out instead. (The copy on cp
# is a different matter: ctl_exec rewrites $WORK/last.{rc,err,out} there, which
# is why cp's dump runs before any of this.)
diag_ctl() { # <args…>
	(
		ctl_try "$@" || true
		printf -- '--- dnvctl %s --- rc=%s\n' "$*" "$CTL_RC"
		if [ -n "$CTL_ERR" ]; then printf 'stderr: %s\n' "$CTL_ERR"; fi
		if [ -n "$CTL_OUT" ]; then printf '%s\n' "$CTL_OUT"; fi
	) 2>&1 || true
}

# diag_ctl_out is diag_ctl for a reply the dump must also PARSE. The command
# substitution is the same containment, and the only thing on its stdout is the
# reply document, so the caller can hand it to jq_of. An empty answer means the
# call failed or the ssh died; the caller must handle that, not assume JSON.
diag_ctl_out() { # <args…>
	local out
	out=$(ctl_try "$@" >/dev/null 2>&1; printf '%s' "$CTL_OUT") || out=""
	printf '%s' "$out"
}

# diag_banner keeps the transcript readable: nine guests' dumps run together.
diag_banner() { # <text>
	log ""
	log "########## $1"
}

# The slog ERROR selector for grep_log, PRE-QUOTED for the guest's own shell,
# and this is a trap worth naming: every wrapper here hands ONE STRING to a
# shell on the guest that parses it — ssh_to gives it to the remote login
# shell, ssh_sudo %q-quotes it for that shell and then `bash -c` parses it —
# so exactly one layer of quoting is consumed on the way. A plain
# '"level":"ERROR"' written here therefore arrives as level:ERROR and matches
# nothing (the dnv binaries log slog JSON, common/log.go:99-113, where an
# error record really is the seven characters "level":"ERROR" with the quotes
# in the text). The value below is the token WITH its single quotes, so what
# survives the guest's parse is the pattern.
DIAG_ERR_PAT="'\"level\":\"ERROR\"'"

diagnostics() {
	local v h key dev out ids id addr logs f spout n

	diag_banner "failure context"
	log "  stage:      $STAGE"
	log "  trace_id:   $TRACE"
	log "  case:       $CASE"
	log "  setup done: $SETUP_DONE"
	# ANA_LAST is what the last host_ana_is/cn_ana_is actually saw. After a
	# host_wait_ana timeout in the ANA half it still holds the state that was
	# there, which is the difference between "it never became optimized" and
	# "it became inaccessible".
	#
	# ANA_CTRL is the controller the same predicates resolved, and it is here
	# because `none` is an ANA_LAST that says nothing on its own: it is what
	# ana_of answers both for a controller that is not there and for one whose
	# namespaces do not carry the wanted uuid. READ THE PAIR IN THIS ORDER:
	#
	#   * `last ctrl: none` is the no-path answer WHATEVER `last ANA` says —
	#     that is the *_ctrl_is_present half of the wait, which takes no ANA
	#     reading and clears ANA_LAST, so the line beside it reads
	#     `(none read)`.
	#   * `last ANA: none` beside a CONTROLLER NAME is the other fault: the
	#     path is up and the namespace is not on it.
	#
	# Both globals are shared by the host and CN probes, so the pair is "the
	# last reading either took", not "a host reading". The stage on the failure
	# line above says which wait it came from.
	log "  last ANA:   ${ANA_LAST:-(none read)}"
	log "  last ctrl:  ${ANA_CTRL:-(none read)}"
	log "  shape:      slice_cnt=$SLICE_CNT redund=$REDUND legs=$LEGS" \
		"groups=$GRP_CNT dns_per_vm=$DNS_PER_VM dn_total=$DN_TOTAL"
	log "  cluster_id: ${CLUSTER_ID:-(not read yet)}"
	local running=""
	for key in etcd gateway worker cdc; do
		if [ "${RUNNING[$key]:-0}" = 1 ]; then
			running="$running $key(${PID[$key]:-?})"
		fi
	done
	log "  cp daemons:${running:- none recorded}"
	for key in "${!DN_LOOP[@]}"; do
		dev=${DN_LOOP[$key]}
		log "  loop dn${key%%:*} instance ${key#*:} -> $dev"
	done

	# The failing dnvctl call's three streams as the DRIVER holds them. cp's
	# own copy of the same three ($WORK/last.{rc,err,out}) is dumped below and
	# is the authority on what dnvctl wrote; this one is here because it
	# survives a cp that has become unreachable, which is one of the ways a
	# run fails. The stdout cut is a parameter expansion and not `head -c`: a
	# reader that closes the pipe early turns SIGPIPE plus `set -o pipefail`
	# into an abort, and aborting inside the failure dump loses the dump.
	log "  last dnvctl rc: ${CTL_RC:-(no call yet)}"
	if [ -n "$CTL_ERR" ]; then
		log "  last dnvctl stderr: $CTL_ERR"
	fi
	if [ -n "$CTL_OUT" ]; then
		log "  last dnvctl stdout (first 4000 bytes): ${CTL_OUT:0:4000}"
	fi

	# (a) cp FIRST: its diag cats $WORK/last.{rc,err,out}, and every dnvctl
	# call below would overwrite them.
	diag_banner "cp ($CP_IP)"
	dump_cp diag
	dump_cp logtail "$DIAG_LOG_LINES" \
		"$WORK/etcd/etcd.log" "$WORK/gateway/gateway.log" \
		"$WORK/worker/worker.log" "$WORK/cdc/cdc.log"
	dump_cp grep_log "$DIAG_ERR_PAT" 50 \
		"$WORK/gateway/gateway.log" "$WORK/worker/worker.log" \
		"$WORK/cdc/cdc.log"

	# (b) the control-plane reads of §7.9. Only worth attempting once setup
	# has built something and the gateway is still the process this suite
	# started; otherwise every call is a connection refused with a 30 s
	# timeout attached.
	if [ "$SETUP_DONE" = 1 ] && [ "${RUNNING[gateway]:-0}" = 1 ]; then
		diag_banner "control plane (dnvctl reads)"
		diag_ctl cluster get
		# `sp get` is read through diag_ctl_out because the dump also parses
		# it; diag_ctl's own reply would be stranded in its subshell.
		spout=$(diag_ctl_out sp get)
		log "--- dnvctl sp get ---"
		if [ -n "$spout" ]; then
			printf '%s\n' "$spout"
		else
			log "  (no reply: the call failed — see cp's last.err above)"
		fi
		# The cntlr ids are NOT in the cntlr_list entries: pb/schema.proto's
		# Cntlr (:395-402) carries addr_port, nvme_tr_conf, cntlid_slot,
		# primary, disabled and err_epoch and no id at all. The ids live in
		# sp_conf.cntlr_id_list (:385) as JSON strings of decimal digits
		# (F11), and that list is what `cntlr inspect --id` takes
		# (ctl/cntlr.go:32, :146).
		ids=$(jq_of "$spout" '.sp_conf.cntlr_id_list[]' 2>/dev/null) || ids=""
		for id in $ids; do
			diag_ctl cntlr inspect --id "$id"
		done
		for v in "${!CN[@]}"; do
			diag_ctl cn inspect --addr "$(cn_addr "$v")"
		done
		# "the DNs involved" — the instances a case registered, and instance 0
		# of every VM when it registered none.
		if [ "${#DIAG_DN[@]}" -gt 0 ]; then
			for key in "${DIAG_DN[@]}"; do
				addr=$(dn_addr "${key%%:*}" "${key#*:}")
				diag_ctl dn inspect --addr "$addr"
			done
		else
			for v in "${!DN[@]}"; do
				diag_ctl dn inspect --addr "$(dn_addr "$v" 0)"
			done
		fi
	else
		diag_banner "control plane (dnvctl reads)"
		log "  skipped: setup_done=$SETUP_DONE," \
			"gateway running=${RUNNING[gateway]:-0}"
	fi

	# (c) the hosts. Their diag carries the controllers, every namespace's
	# uuid and ana_state, the mask, stas and any task in D state — which is
	# what a wedged read looks like from the outside.
	for h in "${!HOST[@]}"; do
		diag_banner "host$h (${HOST_IP[$h]})"
		dump_host "$h" diag
		dump_host "$h" logtail 40 "$WORK/sha-probe.err"
	done

	# (d) the CN VMs: one agent each, so its whole log is fair game.
	#
	# `nvme list-subsys` is §7.9's own item and is NOT in the helper's diag,
	# which lists subsystem NQNs out of sysfs instead (no guest here has a jq
	# for `-o json`). On a CN the difference matters: a 32-slice raid1 sp
	# gives it 2 x 32 x LEGS paths, and their states are what a hung md leg
	# looks like from the host side of the connection.
	for v in "${!CN[@]}"; do
		diag_banner "cn$v (${CN_IP[$v]})"
		dump_cn "$v" diag
		log "--- cn$v nvme list-subsys ---"
		ssh_cn_ok "$v" "timeout $DIAG_TIMEOUT nvme list-subsys"
		dump_cn "$v" logtail "$DIAG_LOG_LINES" "$(cn_log)"
		dump_cn "$v" grep_log "$DIAG_ERR_PAT" 50 "$(cn_log)"
	done

	# (e) the DN VMs. The log selection is the point (see the header): one
	# grep -c over the whole glob names the instances that logged an ERROR,
	# and only those — plus anything a case registered — are tailed. The glob
	# is expanded by the guest's shell, not the driver's.
	for v in "${!DN[@]}"; do
		diag_banner "dn$v (${DN_IP[$v]})"
		dump_dn "$v" diag
		# A DN holds connections too — a migration destination is an nvme
		# host (agent/dnagent/migr.go:116) — so its path list belongs in the
		# dump for the same reason as the CN's.
		log "--- dn$v nvme list-subsys ---"
		ssh_dn_ok "$v" "timeout $DIAG_TIMEOUT nvme list-subsys"
		log "--- dn$v agent logs with ERROR records (path:count) ---"
		# -H so the path is printed even when the glob matches ONE file
		# (grep drops the prefix for a single input, and then there is
		# nothing for the sed below to strip). ONE line: a newline inside an
		# ssh command string comes back as $'…' from the %q quoting.
		out=$(ssh_dn_ok "$v" \
			"grep -c -H -F '\"level\":\"ERROR\"' $WORK/dn*/agent.log 2>/dev/null" \
			"| grep -v ':0\$' || true")
		if [ -n "$out" ]; then
			printf '%s\n' "$out" >&2
		else
			log "  (none)"
		fi
		logs=""
		n=0
		for f in $(printf '%s\n' "$out" | sed -n 's/:[0-9][0-9]*$//p'); do
			n=$((n + 1))
			if [ "$n" -le "$DIAG_MAX_DN_LOGS" ]; then
				logs="$logs $f"
			fi
		done
		if [ "$n" -gt "$DIAG_MAX_DN_LOGS" ]; then
			log "  ($n logs carry errors; tailing the first" \
				"$DIAG_MAX_DN_LOGS — the rest are named above)"
		fi
		if [ "${#DIAG_DN[@]}" -gt 0 ]; then
			for key in "${DIAG_DN[@]}"; do
				if [ "${key%%:*}" = "$v" ]; then
					logs="$logs $(dn_log "${key#*:}")"
				fi
			done
		fi
		if [ -z "$logs" ]; then
			logs=$(dn_log 0)
		fi
		dump_dn "$v" logtail "$DIAG_LOG_LINES" $logs
	done
}

# ---------------------------------------------------------------------------
# Setup (§7.4 steps 1-11)
# ---------------------------------------------------------------------------
#
# §7.4 step 1 — the unconditional start cleanup and the guest preflight — is
# main's, not this section's (cleanup_all must run before preflight_guests, or
# the port check answers about the PREVIOUS run's corpses). Everything from
# step 2 on is here, split into two phases because the two have different
# lifetimes:
#
#   setup_infra   steps 1-3.  The PROCESSES and the files under them: $WORK,
#                 the binaries, the four cp daemons, DN_TOTAL dn agents,
#                 CN_CNT cn agents, and the deferred loop-device gate.
#                 Idempotent, and it runs once per BUILD, not once per run —
#                 it is also the rebuild half of setup_between_cases.
#   setup_case    steps 4-11. Everything that lives in ETCD or on the host,
#                 built from an EMPTY etcd (E2E11): the cluster, every node
#                 record, the sp, the td, the subsystem, the namespace,
#                 host0's connection and the SHA0 baseline. Once per case.
#   setup         setup_infra + setup_case — what main calls before the first
#                 case.
#   setup_between_cases
#                 cleanup_all + setup_infra + setup_case — what main calls
#                 before every later one. Both halves, because cleanup_all
#                 removed both.
#
# ===========================================================================
# WHY THE SPLIT IS NOT `reset_control_plane` ALONE, AND WHY THAT MATTERS
# ===========================================================================
# §7.6 says the between-cases step is "stop the four cp daemons, rm -rf
# $WORK/etcd, restart". That is NOT sufficient, and the suite would fail on
# the SECOND case with a message about a foreign disk:
#
#   * `cluster create` stamps creation_epoch = time.Now().UnixNano()
#     (gateway/cluster.go:59) and the cluster id is fnv64a(name ‖ epoch)
#     (model/keys.go:112-120), so a second create of the SAME name after an
#     etcd reset mints a DIFFERENT cluster_id. The dn_ids are re-minted from
#     a fresh DnGlobal too.
#   * A dn agent writes cluster_id, dn_id and extent_size into the 4 KiB disk
#     header at format time, and EnsureFormatted REFUSES a disk whose header
#     names another cluster/dn/extent ("foreign disk: cluster/dn/extent is
#     …, want …", agent/dnagent/diskmeta.go:299-324); ProbeHeader reports the
#     same (:534-540) and mutableLocked then refuses to touch the volume
#     table at all (:408-420). It never re-formats.
#
# So every case must start on disks with NO header, which means the backing
# files and the local stores have to go — i.e. dn_cleanup / the two cn phases,
# which is exactly what cleanup_all already does correctly and in the right
# order. setup_between_cases is therefore cleanup_all + setup_infra +
# setup_case — the whole of `setup` with a teardown in front — and the case
# loop calls it BETWEEN cases (never after the last — on_exit owns that).
# reset_control_plane stays useful for a case that wants a fresh etcd WITHOUT
# rebuilding the data plane; no case in §7.5 needs that.
#
# ===========================================================================
# CORRECTIONS TO §7.4 THAT ARE LOAD-BEARING HERE
# ===========================================================================
#  a. §7.4 step 2 says "`dnvctl cluster get` until OK". Against an EMPTY etcd
#     `cluster get` is NOT ok — resolveCluster answers NOT_FOUND
#     (gateway/common.go:191-193) — so an
#     "until rc 0" poll would burn its whole budget every run. gateway_serving
#     below treats `dnvctl: NOT_FOUND:` as the healthy answer and anything
#     else (UNAVAILABLE from the dial, above all) as not-yet.
#  b. §7.4 step 5 says to "retry UNAVAILABLE" while an agent is still
#     starting. The gateway does not produce UNAVAILABLE there: a transport
#     failure or non-OK status from the agent's GetDnSize/GetCnSize is AG3's
#     ABORTED (gateway/disknode.go:100-104, gateway/controllernode.go:134-138,
#     errAborted = codes.Aborted at gateway/common.go:68-70). ctl_create_try
#     therefore retries ANY failure and stops on rc 0 — plus ALREADY_EXISTS,
#     which is what a retry sees when dnvctl's own --timeout fired on a call
#     the gateway had already committed.
#  c. §7.4 step 6 asks for a `cntlr_list | length == 2` check. The ids for
#     `cntlr inspect --id` are NOT in cntlr_list — pb/schema.proto's Cntlr
#     (:395-402) has addr_port, nvme_tr_conf, cntlid_slot, primary, disabled
#     and err_epoch and no id field. They live in sp_conf.cntlr_id_list, and
#     the two lists are PARALLEL because loadCntlrs walks cntlr_id_list in
#     order (gateway/alloc.go:496-511). sp_read_roles asserts that the two
#     lengths agree before it pairs them.
#  d. Nothing here asserts `sp_rev.revision == 1`. The create writes revision
#     1 (gateway/storagepool.go:594-598) but the sp-worker's provisioned flip
#     bumps it (model/ops.go:876-888 ends in BumpSpRev), so by the time the
#     first `sp get` returns the number has already moved.
# ---------------------------------------------------------------------------

# --- state this section fills; the cases read it ----------------------------

# SP_ID is the sp_id `sp create` minted, recorded once by setup_create_sp.
# SP_JSON is the whole reply of the last `sp get`, written ONLY by sp_refresh
# (and by setup_wait_stack, which reuses the poll's own last answer). Every
# accessor below reads SP_JSON rather than making its own call, so one
# `sp get` serves a whole step.
SP_ID=""
SP_JSON=""

# The cntlrs, as five PARALLEL arrays indexed by position in cntlr_list (see
# correction (c) above). CNTLR_IDS[i] is the id `cntlr inspect --id` takes,
# and CNTLR_DISABLED[i] is that cntlr's `disabled` flag as the text true or
# false — the one a disabled cntlr's discovery entry turns on (a disabled
# cntlr's transport is dropped from the CdcEntry, enabledCntlrTrConfs).
CNTLR_IDS=()
CNTLR_ADDRS=()
CNTLR_TRADDRS=()
CNTLR_TRSVCIDS=()
CNTLR_DISABLED=()

# The two roles, resolved by sp_read_roles. *_POS is the position in those
# arrays and *_CN is the CN VM index (an index into CN[]/CN_IP[]). Both are -1
# until sp_read_roles has run; an addr_port that names no --cn guest makes
# sp_read_roles DIE rather than leave a sentinel behind, because every later
# `ssh_cn $PRIMARY_CN` would otherwise index the array with garbage.
# With CNTLR_CNT > 2 there is more than one standby and STANDBY_* names the
# first; the arrays above are the general answer.
PRIMARY_POS=-1
PRIMARY_CNTLR_ID=""
PRIMARY_ADDR=""
PRIMARY_TRADDR=""
PRIMARY_TRSVCID=""
PRIMARY_CN=-1
STANDBY_POS=-1
STANDBY_CNTLR_ID=""
STANDBY_ADDR=""
STANDBY_TRADDR=""
STANDBY_TRSVCID=""
STANDBY_CN=-1

# The CN VMs carrying no cntlr — D12's spare, where AR7 lands and where §7.5
# ops step 3's `cntlr create --slot 2 --cn-white <cn2 addr>` points. SPARE_CN
# is the first of them, or -1 if every CN carries one.
SPARE_CN_LIST=()
SPARE_CN=-1

# The host-facing subsystem and the thin device setup builds. Both names are
# bare jq identifiers ON PURPOSE: `td list`'s reply is keyed by td_name and
# `.name_to_td.t0` is how every filter here reaches it, so any name a later
# section adds (s0, t1, c0, a0 …) must stay [A-Za-z_][A-Za-z0-9_]*.
SS0="$NQN_IT:ss0"
SS0_ID=""
TD0=t0
TD0_ID=""
TD0_SIZE=""
NS1_ID=""

# The baseline (§7.4 step 11). SHA0 is the digest every later stage compares
# against; PATTERN0 is the file on host0 that produced it, so a case can
# re-write the same bytes without regenerating them.
BASELINE_MIB=4
PATTERN0=""
SHA0=""

# Scratch used by the progress-reporting predicates below, so a wait that may
# run for WAIT_BUILD says what it is waiting for instead of going silent.
SIDES_LEFT=-1
STACK_LAST=""
DISC_WANT=""
DISC_LAST=""
DISC_JSON=""

# --- jq path fragments, so no filter spells the walk out twice --------------
#
# These are the SP walk every assertion in this file and in §7.5 needs. They
# are single-quoted fragments composed into a filter by the shell, e.g.
#
#   assert_field "$SP_JSON" "[$SP_SIDE_PATH] | length" 128 "sides"
#
# SP_GRP_PATH walks the slices outermost and, within each slice, meta groups
# before data groups — the order allGroups uses for one slice
# (gateway/alloc.go:513-518). SP_LEG_PATH is leg_list
# ONLY — spare_leg_list is deliberately a separate fragment, because md
# members come from leg_list alone (doc/cnagent.md CN12, §8.12).
SP_GRP_PATH='.slice_list[] | (.meta_grp_list[], .data_grp_list[])'
SP_LEG_PATH="$SP_GRP_PATH | .leg_list[]"
SP_SPARE_LEG_PATH="$SP_GRP_PATH | .spare_leg_list[]"
SP_SIDE_PATH="$SP_LEG_PATH | .side_list[]"

# Every side of the sp, PARKED AND SPARE LEGS INCLUDED. It exists for exactly
# one thing — the "is anything still zeroing?" poll below — because a spare
# leg's side is created provisioned = false like any other and only the
# sp-worker flips it, and model.SwitchSpareLeg refuses a spare whose side is
# not provisioned ("spare side is not provisioned", model/ops.go:1878-1880).
# A poll over SP_SIDE_PATH alone would answer "nothing left to do" the instant
# `spare create` returned and the switch would then be refused.
SP_ANY_SIDE_PATH="$SP_GRP_PATH | (.leg_list[], .spare_leg_list[]) | .side_list[]"

# A side's DN VM. addr_port is "<ip>:<29900+k>" (dn_addr), and the suite only
# ever forms IPv4 endpoints — parse_args takes the address from `--dn user@ip`
# and every agent is launched with --adr-fam ipv4 — so splitting on ":" and
# taking the first field is the VM. It would be wrong for an IPv6 traddr, and
# this suite has none.
SP_SIDE_VM='.addr_port | split(":") | .[0]'

# How many sides are not provisioned yet (§7.4 step 7's poll). `provisioned`
# is a proto3 bool and EmitUnpopulated keeps the false visible, so `not` is a
# real test and not a null probe. It walks SP_ANY_SIDE_PATH for the reason
# given there; at setup, where no spare exists, that is the same set as
# SP_SIDE_PATH.
SP_UNPROV_CNT="[$SP_ANY_SIDE_PATH | select(.provisioned | not)] | length"

# The four CntlrInfo row counts §7.4 step 7 waits on. Maps are keyed by
# decimal-string ids (F11), so `[ .map[] | select(...) ] | length` counts rows
# without ever naming one.
#
# The `// {}` is not decoration. InspectCntlr answers with a NULL cntlr_info
# when the CN agent does not know the controller yet: the agent answers
# UnknownObjectReply with a nil CntlrInfo (agent/cnagent/server.go:368-376)
# and the gateway copies the reply's revision and cntlr_info across without
# reading its agent_reply (gateway/cntlr.go:536-546). `null[]` is a jq ERROR,
# not an empty iteration — so without the guard every poll before
# the CN has accepted its first SyncupCntlr would print a jq error to stderr.
# With it the count is simply 0 and the poll goes round again.
CNTLR_OK_TAIL=' | select(.status == "RES_STATUS_OK")] | length'
CNTLR_OK_POOLS="[(.cntlr_info.slice_id_to_dm_pool // {})[]$CNTLR_OK_TAIL"
CNTLR_OK_GRPS="[(.cntlr_info.grp_id_to_md_raid // {})[]$CNTLR_OK_TAIL"
CNTLR_OK_LEGS="[(.cntlr_info.leg_id_to_leg // {})[]$CNTLR_OK_TAIL"
CNTLR_OK_RAID0="[(.cntlr_info.td_id_to_raid0 // {})[]$CNTLR_OK_TAIL"

# ---------------------------------------------------------------------------
# Address <-> index (the inverse of dn_addr / cn_addr)
# ---------------------------------------------------------------------------
#
# §7.5's migration, spare and AR8 steps all start from a side's `addr_port`
# and have to reach the guest it names. These three are the only place that
# inversion happens. Each answers a named sentinel rather than "" so an
# assert_eq failure says which fault it was.

dn_vm_of_addr() { # <ip:port> → v | none
	local ip=${1%:*} v
	for v in "${!DN_IP[@]}"; do
		if [ "${DN_IP[$v]}" = "$ip" ]; then
			printf '%s' "$v"
			return 0
		fi
	done
	printf 'none'
}

dn_inst_of_addr() { # <ip:port> → k | none
	local port=${1##*:}
	case "$port" in
	'' | *[!0-9]*)
		printf 'none'
		return 0
		;;
	esac
	if [ "$port" -lt "$DN_PORT_BASE" ] ||
		[ "$port" -ge "$((DN_PORT_BASE + MAX_DNS_PER_VM))" ]; then
		printf 'none'
		return 0
	fi
	printf '%s' "$((port - DN_PORT_BASE))"
}

cn_vm_of_addr() { # <ip:port> → v | none
	local ip=${1%:*} v
	for v in "${!CN_IP[@]}"; do
		if [ "${CN_IP[$v]}" = "$ip" ]; then
			printf '%s' "$v"
			return 0
		fi
	done
	printf 'none'
}

# ---------------------------------------------------------------------------
# Reading the storage pool
# ---------------------------------------------------------------------------

# sp_refresh is the one `sp get` the rest of this file shares. It dies through
# ctl_ok, so a caller that needs a non-fatal read uses ctl_try itself.
sp_refresh() {
	ctl_ok sp get
	SP_JSON=$CTL_OUT
}

sp_field() { # <filter>
	jq_of "$SP_JSON" "$1"
}

# sp_read_roles resolves SP_JSON into the arrays and the PRIMARY_*/STANDBY_*/
# SPARE_CN globals. Call it after every sp_refresh whose answer a case is
# about to act on — AR5 moves the primary, AR7 moves a whole cntlr to another
# CN, and both leave the previous values pointing at the wrong guest.
#
# It DIES when the cntlr set is not something a caller can act on (no primary,
# two primaries, the two lists out of step). A case that expects a transient
# state — the window between an old primary being declared unhealthy and a new
# one being elected — must wait for the state it wants with its own ctl_try
# predicate and call this afterwards.
sp_read_roles() {
	local n ids i prim addr v
	[ -n "$SP_JSON" ] ||
		die "sp_read_roles was called before sp_refresh filled SP_JSON"
	n=$(sp_field '.cntlr_list | length')
	case "$n" in
	'' | *[!0-9]*) die "sp get carried no cntlr_list: $SP_JSON" ;;
	esac
	ids=$(sp_field '.sp_conf.cntlr_id_list | length')
	# The pairing this whole function rests on: loadCntlrs reads the cntlrs
	# in sp_conf.cntlr_id_list order (gateway/alloc.go:496-511), so position
	# i of cntlr_list is the cntlr whose id is cntlr_id_list[i]. If that ever
	# stops holding, every `cntlr inspect --id` below would inspect the wrong
	# controller and still return a plausible document.
	assert_eq "$ids" "$n" \
		"sp_conf.cntlr_id_list and cntlr_list must be parallel lists"
	CNTLR_IDS=()
	CNTLR_ADDRS=()
	CNTLR_TRADDRS=()
	CNTLR_TRSVCIDS=()
	CNTLR_DISABLED=()
	PRIMARY_POS=-1
	STANDBY_POS=-1
	for ((i = 0; i < n; i++)); do
		CNTLR_IDS[i]=$(sp_field ".sp_conf.cntlr_id_list[$i]")
		CNTLR_ADDRS[i]=$(sp_field ".cntlr_list[$i].addr_port")
		CNTLR_TRADDRS[i]=$(sp_field ".cntlr_list[$i].nvme_tr_conf.tr_addr")
		CNTLR_TRSVCIDS[i]=$(sp_field ".cntlr_list[$i].nvme_tr_conf.tr_svc_id")
		CNTLR_DISABLED[i]=$(sp_field ".cntlr_list[$i].disabled")
		case "${CNTLR_IDS[$i]}" in
		'' | *[!0-9]*)
			die "cntlr_id_list[$i] is '${CNTLR_IDS[$i]}', not a decimal id"
			;;
		esac
		prim=$(sp_field ".cntlr_list[$i].primary")
		if [ "$prim" = true ]; then
			[ "$PRIMARY_POS" -lt 0 ] ||
				die "two cntlrs of $SP claim primary" \
					"(positions $PRIMARY_POS and $i)"
			PRIMARY_POS=$i
		elif [ "$STANDBY_POS" -lt 0 ]; then
			STANDBY_POS=$i
		fi
	done
	[ "$PRIMARY_POS" -ge 0 ] || die "no cntlr of $SP is primary"
	[ "$STANDBY_POS" -ge 0 ] || die "$SP has no standby cntlr"
	PRIMARY_CNTLR_ID=${CNTLR_IDS[$PRIMARY_POS]}
	PRIMARY_ADDR=${CNTLR_ADDRS[$PRIMARY_POS]}
	PRIMARY_TRADDR=${CNTLR_TRADDRS[$PRIMARY_POS]}
	PRIMARY_TRSVCID=${CNTLR_TRSVCIDS[$PRIMARY_POS]}
	STANDBY_CNTLR_ID=${CNTLR_IDS[$STANDBY_POS]}
	STANDBY_ADDR=${CNTLR_ADDRS[$STANDBY_POS]}
	STANDBY_TRADDR=${CNTLR_TRADDRS[$STANDBY_POS]}
	STANDBY_TRSVCID=${CNTLR_TRSVCIDS[$STANDBY_POS]}
	PRIMARY_CN=$(cn_vm_of_addr "$PRIMARY_ADDR")
	STANDBY_CN=$(cn_vm_of_addr "$STANDBY_ADDR")
	assert_ne "$PRIMARY_CN" none \
		"the primary's addr_port $PRIMARY_ADDR names one of the --cn guests"
	assert_ne "$STANDBY_CN" none \
		"the standby's addr_port $STANDBY_ADDR names one of the --cn guests"

	# The spare CNs: every --cn guest carrying no cntlr of this sp. cn
	# placement dedupes by location (model/alloc.go:280-284) and an omitted
	# location becomes the node's own addr_port
	# (gateway/controllernode.go:104-107), so with one cn agent per VM the
	# CNTLR_CNT cntlrs are on CNTLR_CNT different VMs and the rest are spare.
	SPARE_CN_LIST=()
	SPARE_CN=-1
	for v in "${!CN[@]}"; do
		addr=$(cn_addr "$v")
		local used=0 j
		for j in "${!CNTLR_ADDRS[@]}"; do
			if [ "${CNTLR_ADDRS[$j]}" = "$addr" ]; then
				used=1
			fi
		done
		if [ "$used" -eq 0 ]; then
			SPARE_CN_LIST+=("$v")
			[ "$SPARE_CN" -ge 0 ] || SPARE_CN=$v
		fi
	done
	log "  primary cntlr $PRIMARY_CNTLR_ID on cn$PRIMARY_CN ($PRIMARY_ADDR)," \
		"standby $STANDBY_CNTLR_ID on cn$STANDBY_CN ($STANDBY_ADDR)," \
		"spare cn(s): ${SPARE_CN_LIST[*]:-none}"
}

# ---------------------------------------------------------------------------
# Polling predicates (every one of them runs in the PARENT shell under
# wait_until, so the CTL_* globals it fills survive into the assertions that
# follow the wait — that is how the steps below read a reply without paying
# for a second call)
# ---------------------------------------------------------------------------

# gateway_serving is §7.4 step 2's readiness test. See correction (a): against
# an empty etcd the healthy answer is NOT_FOUND, not OK. Anything else — the
# UNAVAILABLE of a gateway that is not listening yet, an ABORTED from an etcd
# that has not elected itself — is not-yet.
gateway_serving() {
	if ctl_try cluster get --name "$CLUSTER"; then
		return 0
	fi
	case "$CTL_ERR" in
	"dnvctl: NOT_FOUND: "*) return 0 ;;
	esac
	return 1
}

# ctl_create_try is the retrying create of §7.4 step 5. See correction (b):
# the code to wait through is ABORTED, and ALREADY_EXISTS is a SUCCESS — the
# record is there, which is all the caller wanted, and the only way to see it
# is a create whose reply dnvctl gave up on after the gateway had committed.
ctl_create_try() { # <args…>
	ctl_exec "$@"
	if [ "$CTL_RC" = 0 ]; then
		return 0
	fi
	case "$CTL_ERR" in
	"dnvctl: ALREADY_EXISTS: "*)
		log "  note: '$*' answered ALREADY_EXISTS — the record is there," \
			"so a previous attempt committed after dnvctl stopped waiting"
		return 0
		;;
	esac
	return 1
}

# dn_node_ready is §7.4 step 5's "the dn-worker has formatted the disk" poll.
# All three DnInfo rows are checked, not just disk_info, because each proves a
# different thing (agent/dnagent/probe.go:15-56):
#   disk_info  the agent can measure --disk at all
#   meta_info  ProbeHeader accepted the 4 KiB header for THIS cluster_id,
#              dn_id and extent_size, and checkWriteZeroes did not find a
#              PRESENT write_zeroes_max_bytes reading 0 (an absent attribute
#              or a failed read pass, agent/dnagent/syncup_dn.go:505-517 —
#              which is why preflight_loop_devices gates the number itself)
#   port_info  ProbePort found ports/<--nvmet-port-id> carrying the four
#              addr_* attributes this agent was launched with AND the three
#              fixed ANA groups in their fixed states (agent/nvmet.go:163-207)
# A dn_info of `null` (the agent has never been told about this dn) reads as
# null through jq and simply is not RES_STATUS_OK, so the poll keeps going.
dn_node_ready() { # <v> <k>
	if ! ctl_try dn inspect --addr "$(dn_addr "$1" "$2")"; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" '.dn_info.disk_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.dn_info.meta_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.dn_info.port_info.status')" = RES_STATUS_OK ]
}

# cn_node_ready is the CN twin. CnInfo's four rows are the §3.2 base state:
# the tmpfs, the clone-metadata file, its loop device and the nvmet port
# (agent/cnagent/syncup_cn.go:305-360).
cn_node_ready() { # <v>
	if ! ctl_try cn inspect --addr "$(cn_addr "$1")"; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" '.cn_info.port_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.cn_info.tmpfs_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.cn_info.tmp_file_info.status')" = RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" '.cn_info.loop_dev_info.status')" = RES_STATUS_OK ]
}

# sp_sides_provisioned is §7.4 step 7's first wait. It reports progress when
# the count moves: 128 sides zeroing over 128 nvme connections is minutes of
# silence otherwise, and a wait that never changes its number is the symptom
# worth seeing early. Reset SIDES_LEFT to -1 before each use.
sp_sides_provisioned() {
	local left total
	if ! ctl_try sp get; then
		return 1
	fi
	left=$(jq_of "$CTL_OUT" "$SP_UNPROV_CNT")
	case "$left" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$left" != "$SIDES_LEFT" ]; then
		SIDES_LEFT=$left
		# The denominator comes out of the SAME reply the count did, and not
		# from GRP_CNT x LEGS: three of the four callers run after the sp has
		# grown (ops step 2 adds two groups, copy's spare adds a leg, react's
		# AR6 adds a group), and a total smaller than the count is exactly the
		# number an operator reads while deciding whether a long wait is
		# stuck. SP_ANY_SIDE_PATH is the same set SP_UNPROV_CNT filters, so
		# the two numbers are always commensurable.
		total=$(jq_of "$CTL_OUT" "[$SP_ANY_SIDE_PATH] | length")
		log "  sides still provisioning: $left of $total"
	fi
	[ "$left" = 0 ]
}

# THE TWO FRESH-SP PREDICATES THAT USED TO LIVE HERE ARE GONE, and this note is
# their headstone rather than a style change. cntlr_stack_ready and
# cntlr_legs_ready took their expected leg count from the constant
# GRP_CNT x LEGS — the shape a FRESH sp has — and their own comment already
# warned that CN10 walks spare_leg_list too, so a `spare create`, a
# `sp grow-slice` or an automatic reaction makes the real count higher and the
# predicate waits for ever. The first real run walked into exactly that: AR8
# created one spare during setup step 7 and the wait sat at `legs 129/128` until
# its budget ran out. A warning in a comment is not a guard, so setup now uses
# the same live-shape predicates every case uses — cntlr_full_ready and
# cntlr_legs_full_ready, which re-read [$SP_LEG_PATH] plus [$SP_SPARE_LEG_PATH]
# on every poll. The two facts the old comments carried are kept where they are
# still true:
#
#   * the three counts are what doc/cnagent.md CN10/CN12/CN13 say a primary
#     holds at READWRITE — one leg row per leg of every group (both roles), one
#     md/linear row per group, one thin-pool row per slice;
#   * grp_id_to_md_raid is expected for `none` as well as raid1, because the
#     probe's switch is on plan.wantGrp and not on the redundancy arm, and
#     probeGroup answers for a RedundNone dm-linear exactly as it does for an
#     array (agent/cnagent/probe.go:62-77, agent/cnagent/md.go:371-394);
#   * a standby's legs DO reach RES_STATUS_OK: CN11 gives it the transport probe
#     (a live controller per desired side, plus an ana_state of optimized or
#     non-optimized on a single-sided leg) in place of the primary's block
#     probe.

# cntlr_raid0_ready waits for the primary to hold <cnt> td raid0 devices — the
# row `td list`'s `created` does NOT cover. `created` flips when a cntlr has
# reported this td's THIN VOLUME RES_STATUS_OK in every slice
# (ThinDeviceCreated.md U1-S1's field comment, §10.3); the raid0 CN15 builds
# one step later in the same build phase is a different resource. CN16 rule 6
# makes a live namespace's dm-linear point AT that raid0, so it is the row
# step 10's `wait_ana optimized` really depends on.
cntlr_raid0_ready() { # <cntlr id> <count>
	local n
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	n=$(jq_of "$CTL_OUT" "$CNTLR_OK_RAID0")
	[ "$n" = "$2" ]
}

# td_created polls ListThinDevices for the flip only the sp-worker makes
# (ThinDeviceCreated.md R13; the gateway always writes `created` false). The
# td name is interpolated into the filter, which is why §7.4's names are all
# bare jq identifiers.
td_created() { # <td name>
	if ! ctl_try td list; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" ".name_to_td.$1.created")" = true ]
}

# ---------------------------------------------------------------------------
# The data-plane gate: has the AGENT built what the host is about to connect to
# ---------------------------------------------------------------------------
#
# WHY THIS EXISTS — run 3, 2026-09-17, commit 7721516. The cdc serves its
# discovery log out of etcd, so it advertises a subsystem and every one of its
# transports the instant the GATEWAY commits the record, which is ahead of any
# CN agent having built the nvmet objects behind it. Setup step 10 discovered
# $SS0 and connected 0.4 s later. Measured on 2026-09-17, host0's dmesg against
# the two CN agent logs (dmesg converted to wall clock on the audit record that
# carries both a kernel timestamp and a unix one):
#
#   20:09:13.56  host0 `nvme discover` — two records, served correctly
#   20:09:13.94  host0 `nvme connect-all` — the discovery controller comes up,
#                then BOTH subsystem connects are refused at the TCP level
#                (`nvme nvme1: failed to connect socket: -111`, ECONNREFUSED),
#                and connect-all exits 0
#   20:09:15.70  the standby cn0 creates the nvmet subsystem and links it to
#                its port — 1.75 s TOO LATE
#   20:09:17.64  cn0 enables its ns 1 at ana_grpid 3
#   20:09:18.13  the primary cn2 creates the subsystem and links it — 4.19 s
#                too late
#   20:09:23.30  cn2 writes allowed_hosts, creates and enables ns 1 and moves
#                it to ana_grpid 1 — 9.38 s too late
#   20:10:13     host_wait_ana's 60 s runs out, blaming ANA
#
# The same instants are in the reply this gate reads: run 3's `cntlr inspect`
# dump carries ss_id_to_subsystem["356"].epoch 1789675755 for the standby and
# 1789675758 for the primary, and ns_id_to_namespace["357"].epoch 1789675757
# and 1789675763 — 20:09:15/18 and 20:09:17/23. BOTH cntlrs report both rows
# RES_STATUS_OK, standby included, which is why one predicate serves both roles.
#
# So nothing was listening on either advertised address: an nvmet port with no
# subsystem linked to it does not listen at all (memory note
# nvmet-referral-port-needs-subsystem). It was NOT an allowed_hosts race — in
# cn2's first window allowed_hosts was empty and attr_allow_any_host was 1,
# which admits everybody — and it was NOT the cdc, the NQNs, the hostid or the
# transports: a manual connect-all minutes later, with the same arguments,
# brought up both paths.
#
# WHAT THE GATE ASKS, and why it is the agent's own words rather than a sleep.
# The two rows are doc/cnagent.md's probe table (:1478-1479):
#
#   ss_id_to_subsystem[ss] OK ⇒ the nvmet subsystem exists, its cntlid range,
#     serial and model match, its allowed_hosts are EXACTLY the desired set
#     (agent/nvmet.go:360-377 — it checks both inclusions), and it is LINKED TO
#     THE NVMET PORT (probeExport, agent/cnagent/td.go:388-394). The link is the
#     conjunct that matters most here: it is what makes the port listen.
#   ns_id_to_namespace[ns] OK ⇒ the nvmet namespace exists, is enabled, carries
#     the desired device_path/uuid/nguid and sits in the ana_grpid CN16 wants
#     for THIS cntlr's role (probeNamespaceObject, td.go:399-427) — which is 1
#     on a primary and 3 on a standby, so the gate is the same one for both.
#     A provisioning-deferred backing chain reports RES_STATUS_PROVISIONING
#     ([D15], CN9), which is not ready and must not pass; that is why the test
#     is `= RES_STATUS_OK` and never "not MISSING".
#
# The ns row is also the FRESHNESS proof, and without it this gate could pass
# on a stale reply: it is keyed by an ns_id that did not exist before
# `ns create`, so a row that is present at all came from a syncup carrying the
# current record — allowed_hosts included, since `ss set-hosts` commits before
# `ns create` does.
#
# WHAT IT DOES NOT PROVE. It is the target's half. It says nothing about the
# host's own connect, which is what connect_verdict checks afterwards.
#
# The `// {}` and the `// "none"` are the same guard CNTLR_OK_* carries:
# InspectCntlr answers with a NULL cntlr_info while the CN agent has not
# accepted a SyncupCntlr for this controller yet, and indexing a null is a jq
# error rather than an empty answer. The optional <sp name> reaches a pool that
# is not $SP through the same trailing `--sp` src_cntlr_ready uses: it is a
# persistent string flag of the root command, so the second occurrence wins.
cntlr_ns_exported() { # <cntlr id> <ss_id> <ns_id> [sp name]
	local args=(cntlr inspect --id "$1")
	[ -z "${4:-}" ] || args=(--sp "$4" "${args[@]}")
	if ! ctl_try "${args[@]}"; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" \
		"(.cntlr_info.ss_id_to_subsystem // {}) | .[\"$2\"] | .status // \"none\"")" \
		= RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" \
			"(.cntlr_info.ns_id_to_namespace // {}) | .[\"$3\"] | .status // \"none\"")" \
			= RES_STATUS_OK ]
}

# wait_ns_exported bounds it. The DEFAULT is WAIT_PROVISION and not WAIT_HOST:
# the 60 s of WAIT_HOST is sized for a kernel-side device or ANA transition,
# not for an agent that may be in the middle of a build.
#
# WAIT_PROVISION IS THE INCREMENTAL BUDGET, AND IT IS ONLY RIGHT WHERE THE
# CONVERGENCE IS INCREMENTAL — i.e. where what is left for that cntlr to do is
# the subsystem, its allowed_hosts and one namespace. Which holds at six of the
# seven sites, for two different reasons:
#
#   * the cntlr's own build has ALREADY been waited out before the gate —
#     setup's stage 10 (setup_wait_stack spends a WAIT_BUILD on both cntlrs)
#     and §4.4's fallback source pool (its own stack waits, and that pool is
#     two sides rather than $SP_LEG_TOTAL legs), §4.3 stage 03 and §4.5 stage
#     04 (a WAIT_BUILD on cntlr_legs_full_ready first);
#   * or nothing was torn down at all — §4.4's and §4.5's second namespaces,
#     where the cntlr has been serving since setup.
#
# THE SEVENTH IS §4.3 STAGE 05, and it is why [secs] exists: there the ladder
# walked down to DISABLE and back, the STANDBY rebuilt from nothing, and this
# gate is the only wait covering it — so that caller passes WAIT_BUILD. <what>
# is the human phrase the timeout names.
wait_ns_exported() { # <cntlr id> <what> <ss_id> <ns_id> [sp name] [secs]
	wait_until "${6:-$WAIT_PROVISION}" \
		"cntlr $1's agent to export $2 — ss_id_to_subsystem[$3] RES_STATUS_OK (the subsystem, its allowed_hosts and the port link that makes the port listen) and ns_id_to_namespace[$4] RES_STATUS_OK (the enabled nvmet namespace behind it)" \
		cntlr_ns_exported "$1" "$3" "$4" "${5:-}"
}

# wait_ns_exported_all is the connect-all form. `nvme connect-all` connects
# EVERY transport the discovery log offers, and the log carries one record per
# non-disabled cntlr (enabledCntlrTrConfs, gateway/subsystem.go:188-194), so
# every one of them has to be listening — not just the primary. Run 3 lost both.
#
# It reads the CNTLR_* arrays, so the caller must have run sp_read_roles for
# the shape it is about to connect to; disc_want_of_sp, which every connect-all
# site already calls, has the same requirement.
#
# [secs] is forwarded verbatim to every wait_ns_exported below, for the caller
# whose cntlrs are rebuilding from nothing rather than converging incrementally
# — see wait_ns_exported's header and §4.3 stage 05.
wait_ns_exported_all() { # <nqn> <ss_id> <ns_id> [secs]
	local i
	[ "${#CNTLR_IDS[@]}" -gt 0 ] ||
		die "wait_ns_exported_all before sp_read_roles"
	for i in "${!CNTLR_IDS[@]}"; do
		# A disabled cntlr's transport is not in the CdcEntry, so connect-all
		# never reaches it and it must not be waited for either — the same skip
		# disc_want_of_sp makes, for the same reason.
		[ "${CNTLR_DISABLED[$i]}" = false ] || continue
		wait_ns_exported "${CNTLR_IDS[$i]}" \
			"$1 (ns_id $3) over its own transport ${CNTLR_TRADDRS[$i]}:${CNTLR_TRSVCIDS[$i]}" \
			"$2" "$3" "" "${4:-}"
	done
}

# cntlr_xfer_exported is the transfer twin. A transfer has no CdcEntry and no
# ss_id/ns_id of its own: CN17 gives it one subsystem and one namespace, and
# both InspectCntlr rows are keyed by the XFER ID (doc/cnagent.md :1487, and a
# deferred transfer's rows are RES_STATUS_PROVISIONING, which again must not
# pass).
cntlr_xfer_exported() { # <cntlr id> <xfer_id>
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" \
		"(.cntlr_info.xfer_id_to_subsystem // {}) | .[\"$2\"] | .status // \"none\"")" \
		= RES_STATUS_OK ] &&
		[ "$(jq_of "$CTL_OUT" \
			"(.cntlr_info.xfer_id_to_namespace // {}) | .[\"$2\"] | .status // \"none\"")" \
			= RES_STATUS_OK ]
}

wait_xfer_exported() { # <cntlr id> <what> <xfer_id> [secs]
	wait_until "${4:-$WAIT_PROVISION}" \
		"cntlr $1's agent to export $2 — its xfer_id_to_subsystem[$3] and xfer_id_to_namespace[$3] rows RES_STATUS_OK" \
		cntlr_xfer_exported "$1" "$3"
}

# ---------------------------------------------------------------------------
# Discovery through the cdc (§7.4 step 10)
# ---------------------------------------------------------------------------

# disc_records renders one discovery log as the comparison unit cdc_test.sh
# uses (:601-605): one "<subnqn>|<traddr>|<trsvcid>" line per record, sorted.
# Driver-side, like every other parse here — no guest has a jq.
disc_records() { # <discovery json>
	printf '%s' "$1" | "$JQ" -r \
		'[.records[]? | "\(.subnqn)|\(.traddr)|\(.trsvcid)"] | sort | .[]'
}

# host_disc runs ONE `nvme discover` against the cdc as host <h> itself and
# echoes the JSON, or "" when the command failed. The helper's `discover`
# propagates nvme's status on purpose (an empty log is
# {"genctr":N,"records":[]} with status 0, a dead cdc is non-zero), so the two
# cases stay distinguishable here.
host_disc() { # <h>
	local out
	if ! out=$(helper_host "$1" discover "$CP_IP" "$CDC_PORT" \
		"${HOST_NQN[$1]}" "${HOST_HOSTID[$1]}" 2>/dev/null); then
		printf ''
		return 0
	fi
	case "$out" in
	'{'*) printf '%s' "$out" ;;
	*) printf '' ;;
	esac
}

# disc_want_of_sp builds the record set the cdc MUST serve for the subsystems
# of this sp, from the transports `ss create` actually wrote into the CdcEntry:
# enabledCntlrTrConfs(cntlrs), i.e. one record per non-disabled cntlr
# (gateway/subsystem.go:188-194). Comparing against that — rather than against
# a hand-written pair of IPs — is what makes the assertion prove the cdc
# serves what the gateway stored.
disc_want_of_sp() { # <subsystem nqn> → DISC_WANT
	local lines="" i
	[ "${#CNTLR_TRADDRS[@]}" -gt 0 ] ||
		die "disc_want_of_sp before sp_read_roles"
	for i in "${!CNTLR_TRADDRS[@]}"; do
		# A DISABLED cntlr's transport is not in the entry, so it must not be
		# in the expectation either. Nothing is disabled at setup; §7.5 ops
		# step 3 is where it matters, which is why the skip is here rather
		# than in a comment saying it cannot happen.
		[ "${CNTLR_DISABLED[$i]}" = false ] || continue
		lines="$lines$1|${CNTLR_TRADDRS[$i]}|${CNTLR_TRSVCIDS[$i]}"$'\n'
	done
	DISC_WANT=$(printf '%s' "$lines" | sort)
}

# host_disc_is is the polling predicate: the host's whole discovery log equals
# DISC_WANT. It is an EXACT set comparison, which is right while the sp has
# one subsystem; a case that adds a transfer subsystem must extend DISC_WANT
# (disc_want_of_sp + its own line) rather than switch to a subset test.
# DISC_LAST keeps what was actually there for the failure report, and
# DISC_JSON the undigested document the winning poll read — the only value in
# this area that disc_records and DISC_WANT have not both touched, which is
# what setup step 10 counts `.records` out of.
host_disc_is() { # <h>
	local json
	json=$(host_disc "$1")
	[ -n "$json" ] || return 1
	DISC_LAST=$(disc_records "$json") || return 1
	DISC_JSON=$json
	[ "$DISC_LAST" = "$DISC_WANT" ]
}

# ---------------------------------------------------------------------------
# Connecting a host — the two verbs, and why neither trusts an exit status
# ---------------------------------------------------------------------------
#
# host_connect_all and host_connect are the ONLY places the driver reaches
# nvme-cli's connect verbs. Every connect in this suite is the suite's own act
# (rule 6): the kernel's autoconnector is masked for the whole run.
#
# THEY VERIFY; THEY DO NOT TRUST rc. `nvme connect-all` EXITS 0 HAVING
# CONNECTED NOTHING — measured on host0 against this very lab on 2026-09-17 in
# two shapes, NEITHER of which is a target refusing a connect: with nothing
# listening on an address the discovery log advertised (run 3's own failure,
# `failed to connect socket: -111` in dmesg, once per record) and with an EMPTY
# discovery log (a hostnqn outside allowed_hosts, so DS4 hid the entry and
# nothing was attempted at all — nothing on stdout, nothing on stderr, nothing
# in dmesg, `nvme list-subsys` empty afterwards). Those are two observations
# and not a law about nvme-cli, and the plain `nvme connect` behind
# host_connect was not one of them — which is exactly why neither wrapper
# judges by rc. Run 3 took an rc=0 as evidence that a path existed, and died
# 60 s later in host_wait_ana with a message about ANA. So the helper reports
# the controllers that exist for the subsystem AFTER the command and
# connect_verdict dies on zero.
#
# THE CHECK IS INSTANTANEOUS AND MUST STAY THAT WAY. Both nvme verbs are
# synchronous — when they return the controller either exists or does not, and
# nvme-cli never retries a connect it failed — so polling here would silently
# paper over the very race this exists to catch. Whether the TARGET was ready
# is a separate question, and it is asked BEFORE the connect, by
# wait_ns_exported / wait_ns_exported_all.
#
# connect_verdict is shared because most of the sentence a reader needs is the
# same in both cases, and it is long on purpose: the point of it is that the
# next person to see this failure is not sent to look at ANA or at the cdc.
#
# <verb> IS THE NVME-CLI VERB THE HELPER ACTUALLY RAN, and it exists because
# two of the sentences are NOT the same in both cases:
#
#   * what an rc of 0 means. The silent zero was measured for `connect-all`
#     and NOT for `connect`, so only the connect-all branch may cite a
#     measurement; the other branch says why the count is what the check
#     stands on without claiming a reading nobody took.
#   * what the failure looks like on the wire. `connect-all` walks a discovery
#     log and reaches a port that may have no subsystem linked to it at all,
#     which is ECONNREFUSED — run 3's shape. `connect` names ONE address, and
#     an agent has exactly ONE nvmet port (agent/nvmet.go:80-99, "the one port
#     per agent") shared by every subsystem that agent exports, while cn_up
#     starts exactly one cn agent per CN guest (D12) — so at host_connect's
#     sites the port can already be listening for a DIFFERENT subsystem while
#     the one being connected does not exist yet, and printing the ECONNREFUSED
#     hint there would send the reader at the wrong thing.
connect_verdict() { # <h> <subnqn> <verb> <rc> <ctrl_cnt> <what was offered>
	local h=$1 sub=$2 verb=$3 rc=$4 cnt=$5 offered=$6 rcsaid mech
	case "$cnt" in
	'' | *[!0-9]*)
		# NOT "the helper is stale": ship_helpers regenerates all four helpers
		# and scps them to every guest on every invocation, dying on a failed
		# scp, and main runs it before cleanup_all and before anything is
		# built — so by the time any connect runs, the helper on that guest is
		# this run's. report_ctrls always prints a ctrl_cnt= line, and the
		# helper preamble is `set -uo pipefail` with no -e, so nothing in it
		# short-circuits before that line either.
		die "host$h: the connect helper printed no usable ctrl_cnt line" \
			"(got '$cnt'). The helper's raw output is above and is the first" \
			"thing to read: what can produce this is a TRUNCATED ssh reply or" \
			"a helper that died before its verification tail. A stale helper" \
			"is not a candidate — ship_helpers re-ships it to every guest on" \
			"every run, before anything connects — so re-shipping is a last" \
			"resort here, not the first move."
		;;
	esac
	if [ "$cnt" = 0 ]; then
		# The rc sentence is branched on purpose. "An exit status of 0 is not
		# evidence" is the finding this check exists for, and printing it over
		# a NON-zero rc would be a false note: there nvme-cli did say so.
		if [ "$rc" = 0 ]; then
			if [ "$verb" = connect-all ]; then
				rcsaid="AND NVME-CLI EXITED 0. THAT IS NOT EVIDENCE THAT"
				rcsaid="$rcsaid ANYTHING CONNECTED: \`nvme connect-all\` was"
				rcsaid="$rcsaid measured in this lab on 2026-09-17 exiting 0,"
				rcsaid="$rcsaid with nothing on stdout and nothing on stderr,"
				rcsaid="$rcsaid both when nothing was listening on an address"
				rcsaid="$rcsaid the discovery log advertised and when the"
				rcsaid="$rcsaid discovery log it was served was empty."
			else
				rcsaid="AND NVME-CLI EXITED 0, WHICH IS NOT EVIDENCE THAT"
				rcsaid="$rcsaid ANYTHING CONNECTED. The silent zero measured in"
				rcsaid="$rcsaid this lab on 2026-09-17 was"
				rcsaid="$rcsaid \`nvme connect-all\`'s, not this verb's, so take"
				rcsaid="$rcsaid it as a reason to distrust rc rather than as a"
				rcsaid="$rcsaid measurement of \`nvme connect\` — the controller"
				rcsaid="$rcsaid count below is what this check stands on."
			fi
		else
			rcsaid="and nvme-cli exited $rc, so its own output above is the"
			rcsaid="$rcsaid first thing to read."
		fi
		if [ "$verb" = connect-all ]; then
			mech="An nvmet port with no subsystem linked to it does not listen"
			mech="$mech at all — the host then sees ECONNREFUSED, which dmesg"
			mech="$mech prints as \`failed to connect socket: -111\`, and that"
			mech="$mech is what run 3 (2026-09-17) took on both of its records."
		else
			mech="This connect named ONE address. That CN runs one cn agent,"
			mech="$mech an agent has one nvmet port, and every subsystem it"
			mech="$mech exports shares that port — so the port"
			mech="$mech may well have been listening for ANOTHER"
			mech="$mech subsystem while $sub did not exist on it yet — do not"
			mech="$mech expect \`failed to connect socket: -111\` in that case."
			mech="$mech The host's dmesg is the record of which it was: a"
			mech="$mech \`new ctrl: NQN\` line for a path that came up, a"
			mech="$mech \`-111\` for an address nothing was listening on."
		fi
		die "host$h holds NO nvme controller for $sub after the connect," \
			"$rcsaid" \
			"$offered." \
			"WHERE TO LOOK: at the CN agent, not at ANA and not at the host." \
			"There is no ANA state without a controller, and the transports" \
			"this connect used are the ones the gateway stored. The nvmet" \
			"subsystem, its allowed_hosts and its namespace are built by the" \
			"agent AFTER the gateway commits the record. $mech" \
			"Read the CN guest's /var/tmp/dnv-e2e/cn/agent.log for when it" \
			"created /sys/kernel/config/nvmet/subsystems/$sub and linked it to" \
			"its port; run 3 (2026-09-17) lost exactly that race by 1.75 s and" \
			"4.19 s."
	fi
	[ "$rc" = 0 ] ||
		log "  WARNING: nvme $verb exited $rc on host$h, although $cnt" \
			"controller(s) for $sub exist"
	log "  host$h holds $cnt nvme controller(s) for $sub"
}

# host_connect_all connects host <h> to everything the cdc offers it, then
# proves a controller for <subnqn> exists. The cdc endpoint is not a parameter
# because E2E10 admits no other one: a host reaches its namespaces ONLY through
# the cdc.
host_connect_all() { # <h> <subnqn> [extra…]
	local h=$1 sub=$2 out rc cnt
	shift 2
	out=$(helper_host "$h" connect_all "$CP_IP" "$CDC_PORT" "$sub" \
		"${HOST_NQN[$h]}" "${HOST_HOSTID[$h]}" "$@") ||
		die "host$h: the connect_all helper verb itself failed"
	printf '%s\n' "$out" >&2
	rc=$(printf '%s\n' "$out" | sed -n 's/^rc=//p' | tail -n 1)
	cnt=$(printf '%s\n' "$out" | sed -n 's/^ctrl_cnt=//p' | tail -n 1)
	connect_verdict "$h" "$sub" connect-all "$rc" "$cnt" \
		"The cdc on $CP_IP:$CDC_PORT offered these records, and connect-all tried every one of them: ${DISC_LAST:-(DISC_LAST is empty — no host_disc_is wait ran before this connect, which is itself a bug at the call site)}"
}

# connect_added_ctrl is the PER-ADDRESS half of the verdict, and it exists
# because connect_verdict's test is "host$h holds at least one controller for
# this subsystem", which at the two RECONNECT-ONTO-AN-EXISTING-PATH sites is
# already true before the connect runs. §4.3 stage 03 and §4.5 stage 04 both
# connect-all to pick up ONE NEW transport while host0 still holds a live path
# to the primary — each asserts that path is still `live` two lines later — so
# there ctrl_cnt is >= 1 whatever the new transport did, connect_verdict cannot
# fire, and a connect-all that silently added nothing would fall through to the
# host_path_live wait and die WAIT_HOST later naming the path instead of the
# connect. That is the same class of defect run 3 died of.
#
# It is INSTANTANEOUS for the reason the file's header gives: both nvme verbs
# are synchronous, so when the command returns the controller either exists or
# does not, and polling here would paper over the race. Whether that controller
# reaches `live` is the NEXT question and stays a poll at the call sites.
connect_added_ctrl() { # <h> <subnqn> <traddr>
	local c
	c=$(host_ctrl "$1" "$2" "$3")
	if [ -z "$c" ] || [ "$c" = none ]; then
		die "host$1 holds no nvme controller for $2 on $3 after the connect," \
			"although it does hold one or more for $2 on OTHER transports —" \
			"which is why the count above did not catch this. The controllers" \
			"it holds are listed in the helper output above. WHERE TO LOOK:" \
			"at the agent of the cntlr whose transport is $3, exactly as for" \
			"a zero count — the subsystem, its allowed_hosts and its namespace" \
			"are the last rows that cntlr builds, and its port does not listen" \
			"until the subsystem is linked to it."
	fi
	log "  host$1 holds $c for $2 on $3"
}

# host_connect is the single-subsystem form, for a namespace reached without
# the discovery log: a transfer (which is in no CdcEntry) and a re-connect to
# one named transport while the log still advertises others.
host_connect() { # <h> <traddr> <trsvcid> <subnqn> [extra…]
	local h=$1 a=$2 s=$3 sub=$4 out rc cnt
	shift 4
	out=$(helper_host "$h" connect "$a" "$s" "$sub" \
		"${HOST_NQN[$h]}" "${HOST_HOSTID[$h]}" "$@") ||
		die "host$h: the connect helper verb itself failed for $sub"
	printf '%s\n' "$out" >&2
	rc=$(printf '%s\n' "$out" | sed -n 's/^rc=//p' | tail -n 1)
	cnt=$(printf '%s\n' "$out" | sed -n 's/^ctrl_cnt=//p' | tail -n 1)
	connect_verdict "$h" "$sub" connect "$rc" "$cnt" \
		"No discovery log is involved: this connect names one transport, tcp $a:$s, and that is the only address it tried"
}

# ---------------------------------------------------------------------------
# §7.4 steps 1-3 — the infrastructure phase
# ---------------------------------------------------------------------------

setup_infra() {
	# From here on the run has built something, so on_exit must DUMP rather
	# than clean if anything below fails (§7.6: cleanup at the end only on
	# success). prepare_work is the first write, so the flag goes up first.
	SETUP_DONE=1
	CASE=setup

	stage 01 "ship the binaries and create $WORK on all ten guests"
	prepare_work
	ship_binaries
	# The hosts are re-masked here and not only in preflight_guests, because
	# host_cleanup's last act is `unmask` and setup_infra is also the rebuild
	# half of setup_between_cases. Rule 6 has to hold for the WHOLE run: the
	# kernel's autoconnector matches the discovery AEN that this suite's own
	# connect-all causes and would connect behind its back.
	# The result is ASSERTED and not merely status-checked: `mask` returns 0
	# whatever systemctl did, and reports the two units' real is-enabled states
	# when either is not masked. A `>/dev/null ||` here proved nothing.
	local h maskout
	for h in "${!HOST[@]}"; do
		maskout=$(helper_host "$h" mask) ||
			die "host$h: the mask verb itself failed;" \
				"rule 6 cannot be satisfied and the run would race the" \
				"kernel's own autoconnector"
		assert_eq "$maskout" masked \
			"host$h: the nvmf-connect@.service and nvmf-connect.target masks (rule 6)"
	done

	stage 02 "start etcd, dnv-gateway, dnv-worker and dnv-cdc on $CP_IP"
	start_cp_daemons
	# start_cp_daemons proved four listeners. This proves the gateway SERVES,
	# which is a different thing: it has to have dialled etcd and answered an
	# RPC. NOT_FOUND is the healthy answer here — see correction (a).
	wait_until "$WAIT_CP_READY" \
		"the gateway on $CP_IP:$GW_PORT to answer \`cluster get\`" \
		gateway_serving

	stage 03 "start $DN_TOTAL dn agents ($DNS_PER_VM per VM) and $CN_CNT cn agents"
	# Sequential on purpose, and not only because `losetup --find` races
	# within one guest: start_dn_instance fills DN_LOOP, an associative array
	# in THIS shell, and a backgrounded VM loop would fill a subshell's copy
	# and lose it. The cost is one ssh per agent.
	local v
	for v in "${!DN[@]}"; do
		start_dn_vm "$v"
	done
	for v in "${!CN[@]}"; do
		start_cn_agent "$v"
	done
	# §7.3's deferred item, and it must run BEFORE the first `sp create`: a
	# loop device with write_zeroes_max_bytes = 0 is only TAGGED by the agent
	# (checkWriteZeroes, agent/dnagent/syncup_dn.go:505-517, whose own comment
	# at :502-504 says it "never gates converging"), so side zeroing would
	# fall back to writing real zero pages and materialise every sparse
	# backing file on the guest.
	preflight_loop_devices
}

# ---------------------------------------------------------------------------
# §7.4 step 4 — the cluster
# ---------------------------------------------------------------------------

setup_create_cluster() {
	stage 04 "cluster create --name $CLUSTER --extent-size $EXTENT_SIZE"
	local minted
	ctl_ok cluster create --name "$CLUSTER" --extent-size "$EXTENT_SIZE"
	minted=$(jq_of "$CTL_OUT" '.cluster_id')

	ctl_ok cluster get --name "$CLUSTER"
	CLUSTER_ID=$(jq_of "$CTL_OUT" '.cluster_id')
	case "$CLUSTER_ID" in
	'' | *[!0-9]*)
		die "cluster get returned cluster_id '$CLUSTER_ID', which is not a" \
			"decimal id; every NQN of this run is formatted from it"
		;;
	esac
	# The create reply and the read-back must name the same cluster. They can
	# differ only if something else created a cluster of this name between the
	# two calls, which on a lab that §8.2 forbids sharing means the etcd was
	# not empty.
	assert_eq "$CLUSTER_ID" "$minted" \
		"cluster get's cluster_id vs the CreateCluster reply's"
	assert_field "$CTL_OUT" '.cluster_name' "$CLUSTER" "cluster get's name"

	# What `--extent-size` bought, and the whole reason commit 3 exists.
	# model.ResolveDnBinConf takes the four bin shifts as a SET: an
	# extent-size-only request leaves them all zero, binLadderOk rejects that
	# as a ladder, and all four are replaced together with
	# DefaultDnBin0..3Shift = 0/4/8/12 (model/capacity.go:71-89,
	# common/constants.go:18-21). So the stored conf is the operator's size on
	# the default ladder, and a cluster conf is WRITE-ONCE — there is no
	# UpdateCluster — which is why a wrong number here is permanent.
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.extent_size' \
		"$EXTENT_SIZE" "cluster_conf.dn_bin_conf.extent_size (uint64, a JSON string)"
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.bin0_shift' 0 \
		"the default bin ladder survives an extent-size-only create (bin0)"
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.bin1_shift' 4 \
		"the default bin ladder survives an extent-size-only create (bin1)"
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.bin2_shift' 8 \
		"the default bin ladder survives an extent-size-only create (bin2)"
	assert_field "$CTL_OUT" '.cluster_conf.dn_bin_conf.bin3_shift' 12 \
		"the default bin ladder survives an extent-size-only create (bin3)"
	log "  cluster_id $CLUSTER_ID = $(hex16 "$CLUSTER_ID") in every NQN"
}

# ---------------------------------------------------------------------------
# §7.4 step 5 — the node records
# ---------------------------------------------------------------------------

setup_register_nodes() {
	stage 05 "register $DN_TOTAL disk nodes and $CN_CNT controller nodes"
	local v k addr

	# D5: every dnagent of one VM registers --location dn<v>, the VM's role.
	# That is what makes the allocator spread a group's legs across VMs — a
	# scan returns at most one candidate per location (model/alloc.go:137-141)
	# — and therefore what keeps two sides of one leg off one kernel, where
	# SideToCnNqn (which carries no dn_id, common/name_fmt.go:541-557) would
	# collide between the two agents.
	#
	# The four --tr-* values are passed explicitly and identically to the
	# agent's own launch flags (dn_up). They are not decoration: the CN dials
	# DnConf.nvme_tr_conf, while the agent's port carries its --tr-* flags, so
	# a mismatch would build an sp whose sides no cntlr can reach.
	for v in "${!DN[@]}"; do
		for ((k = 0; k < DNS_PER_VM; k++)); do
			addr=$(dn_addr "$v" "$k")
			wait_until "$WAIT_AGENT" \
				"dn create $addr to be accepted (the gateway calls that agent's GetDnSize inline)" \
				ctl_create_try dn create --addr "$addr" \
				--tr-type tcp --adr-fam ipv4 \
				--tr-addr "${DN_IP[$v]}" --tr-svc-id "$(dn_trsvcid "$k")" \
				--location "$(dn_location "$v")"
		done
		log "  dn$v: $DNS_PER_VM disk nodes at location $(dn_location "$v")"
	done

	# CNs keep the default location (D5), which the gateway fills with the
	# node's own addr_port, so with one cn agent per VM every CN is its own
	# failure domain and the CNTLR_CNT cntlrs land on CNTLR_CNT VMs.
	for v in "${!CN[@]}"; do
		addr=$(cn_addr "$v")
		wait_until "$WAIT_AGENT" \
			"cn create $addr to be accepted (the gateway calls that agent's GetCnSize inline)" \
			ctl_create_try cn create --addr "$addr" \
			--tr-type tcp --adr-fam ipv4 \
			--tr-addr "${CN_IP[$v]}" --tr-svc-id "$CN_TRSVCID"
	done
	log "  $CN_CNT controller nodes registered"

	# Now the converge. The budget is WAIT_PROVISION and not WAIT_AGENT: this
	# waits for the dn-worker to walk DN_TOTAL fresh DnConfs, not for one
	# agent to answer. A poll that is already true costs one call, so the long
	# budget is only ever spent when something is actually wrong.
	#
	# meta_info is the row that would catch the between-cases trap described
	# at the top of this section: a disk still carrying the PREVIOUS case's
	# cluster_id reports RES_STATUS_ERROR with "foreign disk: …" in details,
	# and the agent will never re-format it.
	for v in "${!DN[@]}"; do
		for ((k = 0; k < DNS_PER_VM; k++)); do
			wait_until "$WAIT_PROVISION" \
				"dn$v instance $k at $(dn_addr "$v" "$k") to report disk, header and port all OK" \
				dn_node_ready "$v" "$k"
			# wait_until ran its predicate in THIS shell and returned on the
			# call that succeeded, so $CTL_OUT is that instance's reply.
			# port_info's res_name is the agent's own port id as %d
			# (agent/dnagent/probe.go:47; doc/dnagent.md:1404 pins the same
			# string — '"1" unless --nvmet-port-id says otherwise, so on a
			# node running several agents the rows differ'), so this is the
			# end-to-end proof that --nvmet-port-id reached the agent and
			# that the DNS_PER_VM agents of one kernel are not all
			# converging ports/1.
			assert_field "$CTL_OUT" '.dn_info.port_info.res_name' \
				"$(dn_port_id "$k")" \
				"dn$v instance $k converged its own nvmet port id"
		done
		log "  dn$v: $DNS_PER_VM disks formatted, ports 1..$(dn_port_id $((DNS_PER_VM - 1)))"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_PROVISION" \
			"cn$v ($(cn_addr "$v")) to report its tmpfs, arena file, loop device and nvmet port" \
			cn_node_ready "$v"
		assert_field "$CTL_OUT" '.cn_info.port_info.res_name' "$CN_PORT_ID" \
			"cn$v converged nvmet port $CN_PORT_ID (no --nvmet-port-id, so the default)"
	done
}

# ---------------------------------------------------------------------------
# §7.4 step 6 — the storage pool, and every assertion on its shape
# ---------------------------------------------------------------------------

# sp_thresholds selects the event_threshold set the NEXT `sp create` will carry
# (§7.1's two sets and the measurements behind them). main calls it once before
# setup and once before each setup_between_cases, so the sp a case works in is
# always built with that case's set — the only moment the choice can be made,
# because no RPC changes a threshold afterwards.
#
# An unknown name DIES rather than defaulting: a fifth case added to $CASES is a
# decision about whether it may tolerate a reaction mid-run, and silently giving
# it the quiet set would make that decision invisibly.
sp_thresholds() { # <case name>
	case "$1" in
	react)
		THR_SET=reacting
		THR_PRIMARY=$THR_REACT_PRIMARY
		THR_CNTLR=$THR_REACT_CNTLR
		THR_SIDE=$THR_REACT_SIDE
		THR_LEG=$THR_REACT_LEG
		THR=$THR_REACT
		;;
	smoke | ops | copy)
		THR_SET=quiet
		THR_PRIMARY=$THR_QUIET_PRIMARY
		THR_CNTLR=$THR_QUIET_CNTLR
		THR_SIDE=$THR_QUIET_SIDE
		THR_LEG=$THR_QUIET_LEG
		THR=$THR_QUIET
		;;
	*)
		die "sp_thresholds has no event_threshold set for case '$1'." \
			"Every case must choose one AT CREATE TIME — no RPC changes a" \
			"threshold afterwards — so a new case has to be named here"
		;;
	esac
	log ""
	log "=== thresholds for the $1 case's sp: the $THR_SET set, $THR"
}

setup_create_sp() {
	stage 06 "sp create — $SLICE_CNT slices, $REDUND, $GRP_CNT groups, $((GRP_CNT * LEGS)) sides"
	local slot0=${SLOTS%%,*}

	# ctl_timeout, not the file default: dnvctl's --timeout is a
	# per-invocation deadline and this is the heaviest call in the suite.
	# CreateStoragePool runs 2 x slice_cnt = $GRP_CNT DN candidate scans
	# before the transaction, each a full descending walk of the capacity
	# index with one proto decode per DN (model/alloc.go:113-141), and then
	# commits a transaction whose compare list is
	# 7 + slice_cnt + 7 x sides + 8 x cntlr_cnt — 951 at the default shape,
	# which is why etcd runs with --max-txn-ops=$ETCD_MAX_TXN_OPS.
	#
	# $THR is UNQUOTED so it splits into its eight words. That is deliberate,
	# and it is why the four thresholds also exist as separate variables: the
	# read-backs below assert them one field at a time, and react's messages
	# name the one threshold each of its waits is watching. Nothing computes a
	# wait bound from them.
	#
	# WHICH eight words is sp_thresholds' answer, and main has already given it
	# — the $THR_SET set (§7.1). This is the ONLY place the choice can take
	# effect: no RPC changes an event_threshold after CreateStoragePool, so the
	# set this call carries is the set the case lives with. The four read-backs
	# below are against the ACTIVE variables, so they follow the choice.
	# shellcheck disable=SC2086
	ctl_timeout 180 ctl_ok sp create \
		--cntlr-cnt "$CNTLR_CNT" --slice-cnt "$SLICE_CNT" \
		--init-ext-cnt "$INIT_EXT_CNT" --slots "$SLOTS" \
		--redund "$REDUND" --stripe-size "$STRIPE_SIZE" $THR
	SP_ID=$(jq_of "$CTL_OUT" '.sp_id')
	case "$SP_ID" in
	'' | *[!0-9]*) die "sp create returned sp_id '$SP_ID'" ;;
	esac

	sp_refresh
	assert_field "$SP_JSON" '.sp_name' "$SP" "sp get's sp_name"
	assert_field "$SP_JSON" '.sp_conf.sp_id' "$SP_ID" \
		"sp_conf.sp_id vs the CreateStoragePool reply"
	# Not `== 1`: the create writes revision 1, but the sp-worker's
	# provisioned flip bumps it (model/ops.go:876-888), so this races.
	assert_ge "$(sp_field '.sp_rev.revision')" 1 "sp_rev.revision"

	# --- the slice / group / leg / side shape (planSpGroups + the STM) ------
	#
	# planSpGroups emits, per slice, one META group of exactly one extent
	# followed by one DATA group of init_ext_cnt (gateway/storagepool.go:
	# 254-263), and the STM builds legs 0..legs-1 under each with exactly one
	# side each (:513-542). slice_list comes back in sp_conf.slice_id_list
	# order, i.e. slice_idx ascending (gateway/alloc.go:478-493).
	assert_field "$SP_JSON" '.slice_list | length' "$SLICE_CNT" "slices"
	assert_field "$SP_JSON" '.sp_conf.slice_id_list | length' "$SLICE_CNT" \
		"sp_conf.slice_id_list"
	assert_jq "$SP_JSON" \
		"[.slice_list[].slice_idx] | sort == [range(0; $SLICE_CNT)]" \
		"the slices are slice_idx 0..$((SLICE_CNT - 1)) with no gap"
	# A filter may span lines: it is jq source, it runs on the DRIVER and it
	# never crosses an ssh, so the newline that would break a guest command
	# string is only whitespace here (section 2's path_field does the same).
	assert_jq "$SP_JSON" \
		'[.slice_list[]
		  | select((.meta_grp_list | length) != 1
		           or (.data_grp_list | length) != 1)] | length == 0' \
		"every slice has exactly one meta group and one data group"
	assert_field "$SP_JSON" "[$SP_GRP_PATH] | length" "$GRP_CNT" \
		"groups (2 per slice)"
	assert_jq "$SP_JSON" \
		"[.slice_list[] | .meta_grp_list[] | select(.ext_cnt != \"1\")] | length == 0" \
		"every meta group is one extent (planSpGroups' first rung)"
	assert_jq "$SP_JSON" \
		"[.slice_list[] | .data_grp_list[] | select(.ext_cnt != \"$INIT_EXT_CNT\")] | length == 0" \
		"every data group is --init-ext-cnt = $INIT_EXT_CNT extents"
	assert_jq "$SP_JSON" \
		"[$SP_GRP_PATH | select((.leg_list | length) != $LEGS)] | length == 0" \
		"every group has $LEGS leg(s) — legCntOf for $REDUND"
	assert_jq "$SP_JSON" \
		"[$SP_LEG_PATH | select((.side_list | length) != 1)] | length == 0" \
		"every leg has exactly one side at create (a second side is a migration)"
	assert_field "$SP_JSON" "[$SP_SPARE_LEG_PATH] | length" 0 \
		"a fresh sp has no spare legs"

	# F2: the DN black list starts as the request's and grows with every pick
	# (gateway/storagepool.go:393-403), so every side of the WHOLE sp — not
	# merely of one group — is on a DISTINCT disk node.
	assert_field "$SP_JSON" "[$SP_SIDE_PATH] | length" "$((GRP_CNT * LEGS))" \
		"sides"
	assert_field "$SP_JSON" "[$SP_SIDE_PATH | .addr_port] | unique | length" \
		"$((GRP_CNT * LEGS))" \
		"DISTINCT disk nodes carrying a side (F2: the black list grows with every pick)"

	# D5: and the legs of one group are on different DN VMs, because a scan
	# returns at most one candidate per LOCATION and a VM's agents all
	# register --location dn<v>. The test is written for any LEGS — at
	# --redund none it says "1 leg, 1 VM", which is true and costs nothing.
	assert_jq "$SP_JSON" \
		"[$SP_GRP_PATH
		  | select(([.leg_list[] | .side_list[] | $SP_SIDE_VM]
		            | unique | length) != $LEGS)] | length == 0" \
		"the $LEGS leg(s) of every group are on $LEGS different DN VMs (D5)"

	# --- the cntlrs --------------------------------------------------------
	assert_field "$SP_JSON" '.cntlr_list | length' "$CNTLR_CNT" "cntlrs"
	assert_field "$SP_JSON" '[.cntlr_list[] | select(.primary)] | length' 1 \
		"exactly one primary cntlr"
	assert_field "$SP_JSON" '[.cntlr_list[] | select(.disabled)] | length' 0 \
		"no cntlr is created disabled"
	assert_field "$SP_JSON" '[.cntlr_list[].addr_port] | unique | length' \
		"$CNTLR_CNT" "the cntlrs are on distinct controller nodes"
	assert_jq "$SP_JSON" ".sp_conf.cntlid_slot_list == [$SLOTS]" \
		"sp_conf.cntlid_slot_list is --slots as given"
	# §11.8: the slots of one sp's cntlrs are all distinct, and the STM hands
	# out slots[idx] in cntlr order (gateway/storagepool.go:567-577).
	assert_jq "$SP_JSON" \
		"[.cntlr_list[].cntlid_slot] == .sp_conf.cntlid_slot_list[0:$CNTLR_CNT]" \
		"cntlr i carries cntlid_slot_list[i]"
	# Every SIDE carries slots[0] (gateway/storagepool.go:526). §7.5 ops step
	# 3's "--slots 1,2 drops slot 0" refusal is exactly this fact, so it is
	# worth pinning here where a failure is still readable.
	assert_jq "$SP_JSON" \
		"[$SP_SIDE_PATH | select(.cntlid_slot != $slot0)] | length == 0" \
		"every side carries cntlid_slot_list[0] = $slot0"

	# --- the conf that was actually stored ---------------------------------
	# ResolveBdevConf keeps a non-zero stripe_size verbatim
	# (model/capacity.go:156-185), so this is the value the react case's
	# strided writes depend on: with a 1 MiB stripe, every offset
	# k x (slice_cnt x 1 MiB) lands in slice 0.
	assert_field "$SP_JSON" '.sp_conf.bdev_conf.dm_raid0_conf.stripe_size' \
		"$STRIPE_SIZE" "the sp's dm-striped chunk"
	case "$REDUND" in
	raid1)
		assert_jq "$SP_JSON" \
			'.sp_conf.bdev_conf.redund_conf | has("redund_md_raid1")' \
			"the sp's redundancy arm is md-raid1"
		;;
	none)
		assert_jq "$SP_JSON" \
			'.sp_conf.bdev_conf.redund_conf | has("redund_none")' \
			"the sp's redundancy arm is none"
		;;
	esac
	# EventThreshold is stored exactly as sent (gateway/storagepool.go:457) —
	# nothing resolves it — so this is also the proof that $THR split into its
	# eight words instead of arriving as one.
	assert_field "$SP_JSON" '.sp_conf.event_threshold.primary_unhealthy' \
		"$THR_PRIMARY" "event_threshold.primary_unhealthy (uint32, a bare number)"
	assert_field "$SP_JSON" '.sp_conf.event_threshold.cntlr_unhealthy' \
		"$THR_CNTLR" "event_threshold.cntlr_unhealthy"
	assert_field "$SP_JSON" '.sp_conf.event_threshold.side_unhealthy' \
		"$THR_SIDE" "event_threshold.side_unhealthy"
	assert_field "$SP_JSON" '.sp_conf.event_threshold.leg_unhealthy' \
		"$THR_LEG" "event_threshold.leg_unhealthy"
	assert_field "$SP_JSON" '.sp_conf.sp_level' SP_LEVEL_READWRITE \
		"a fresh sp is at SP_LEVEL_READWRITE"
	assert_field "$SP_JSON" '.sp_conf.deleting' false "a fresh sp is not deleting"

	sp_read_roles
}

# ---------------------------------------------------------------------------
# §7.4 step 7 — provisioning, the primary's stack, the standby's shape
# ---------------------------------------------------------------------------

# setup_assert_standby is the step the plan refuses to guess at: "the standby's
# inspect reports whatever doc/cnagent.md specifies for a standby — assert
# that, do not assume". What doc/cnagent.md specifies, rule by rule:
#
#   CN10 (legs)   "every leg of every group of every slice … both roles", so a
#                 standby DOES report leg_id_to_leg, one row per leg.
#   CN12 (groups) "Groups (md.go; primary only — a standby has none, §3.4)".
#   CN13 (pools)  "a standby has no pool device and only the primary may write
#                 pool metadata".
#   CN15 (per-td) "primary builds both, standby only the error".
#   CN19          a resource the sp_level suppresses is reported
#                 RES_STATUS_MISSING with details = "sp_level"; a resource a
#                 standby simply does not have is a DIFFERENT case.
#
# Which of those two a standby is, only the code says, and the answer is the
# second — the row is LEFT OUT. The mechanism is not one shared arm, though,
# and saying so would be the false universal this project keeps producing;
# it is four different shapes in agent/cnagent/probe.go, each of which happens
# to be false on a standby:
#
#   :62-77   groups — a three-armed switch, `case plan.wantGrp && deferred` /
#            `case plan.wantGrp` / `case plan.primary`; none matches.
#   :79-113  pools and both concats — `if !plan.wantPool { if plan.primary {
#            …MISSING… }; continue }`; the inner guard is what is false.
#   :115-141 thin volumes — `for _, tp := range plan.tds { if !plan.primary {
#            continue } … }`; no ThinInfo entry is created at all.
#   :143-181 raid0 — the same three-armed switch as groups.
#
# So the maps are EMPTY, not full of MISSING rows, and protojson's
# EmitUnpopulated renders an empty map as {} — `| length` is 0, not null.
#
# td_id_to_dm_error is deliberately NOT asserted below. CN15 gives the standby
# the per-td dm-error, and that loop is gated on plan.wantAny (true at
# READWRITE for BOTH roles, agent/cnagent/plan.go:404), not on plan.primary.
#
# Asserting emptiness rather than "no OK rows" is the point: it is the only
# form that would catch a standby which had started building a pool. Note that
# at the moment setup calls this the sp has no thin device yet, so the two
# td_* assertions are vacuous here; a case that re-checks the standby AFTER
# `td create` is where they start carrying weight.
setup_assert_standby() { # <the standby's cntlr inspect reply>
	local doc=$1
	# First, that there IS a CntlrInfo. Every emptiness check below would
	# otherwise pass on a null cntlr_info, because jq's `null | length` is 0 —
	# the one shape in which "the standby builds nothing" and "the agent has
	# never heard of this controller" look identical.
	assert_field "$doc" '.cntlr_info | type' object \
		"the standby's InspectCntlr reply carries a CntlrInfo"
	assert_field "$doc" '.cntlr_info.grp_id_to_md_raid | length' 0 \
		"CN12: a standby has no group devices, so grp_id_to_md_raid is empty"
	assert_field "$doc" '.cntlr_info.slice_id_to_dm_pool | length' 0 \
		"CN13: a standby has no pool device, so slice_id_to_dm_pool is empty"
	assert_field "$doc" '.cntlr_info.slice_id_to_meta | length' 0 \
		"CN13: a standby builds no pool meta concat"
	assert_field "$doc" '.cntlr_info.slice_id_to_data | length' 0 \
		"CN13: a standby builds no pool data concat"
	assert_field "$doc" '.cntlr_info.td_id_to_raid0 | length' 0 \
		"CN15: a standby builds no td raid0"
	assert_field "$doc" '.cntlr_info.td_id_to_thin_info | length' 0 \
		"CN14: a standby builds no thin volumes"
	# The LIVE leg total, not GRP_CNT x LEGS. A build under the reacting
	# threshold set can leave a spare leg behind (react's, §7.1), CN10 walks
	# spare_leg_list as well as leg_list, and the fresh-sp constant would then be
	# one short of what the agent correctly reports. The winning poll's own
	# sp_totals_poll ran in this same shell (wait_until does not fork), so these
	# are the numbers the reply in $doc was judged against.
	assert_field "$doc" '.cntlr_info.leg_id_to_leg | length' \
		"$((SP_LEG_TOTAL + SP_SPARE_TOTAL))" \
		"CN10: a standby connects every leg, so leg_id_to_leg is full"
}

setup_wait_stack() {
	stage 07 "wait: $((GRP_CNT * LEGS)) sides, then the primary's stack"

	# [D15]: a side is exported only after the DN agent has zeroed it whole
	# (blkdiscard --zeroout per batch), and the sp-worker then flips
	# Side.provisioned. Until then the CN skips the side entirely (CN10), so
	# nothing above it can build.
	SIDES_LEFT=-1
	wait_until "$WAIT_BUILD" \
		"every one of the $((GRP_CNT * LEGS)) sides of $SP to be provisioned" \
		sp_sides_provisioned
	# The predicate ran in this shell, so its last (successful) `sp get` is
	# still in $CTL_OUT — no second call.
	SP_JSON=$CTL_OUT
	assert_field "$SP_JSON" "$SP_UNPROV_CNT" 0 "unprovisioned sides"
	sp_read_roles
	# THE TARGET OF THE TWO WAITS BELOW IS THE LIVE SHAPE, and this is where the
	# first real run died. Setup used to wait against the fresh-sp constant
	# GRP_CNT x LEGS; a spare leg created by AR8 while the wait ran made the
	# agent report 129 leg rows against a target of 128, and the poll could never
	# pass however long it was given (the transcript's last line was
	# `legs 129/128`). primary_stack_ready and standby_shape_ready both go
	# through sp_totals_poll, which re-reads the totals on every poll and shouts
	# when the shape moves — the only form that can both finish and say what
	# happened.
	#
	# Under the quiet threshold set — smoke, ops and copy — nothing may move at
	# all, so the shout is the signal that a reaction fired when none should
	# have. Under react's reacting set it is expected and the waits survive it.
	#
	# This reading buys nothing the predicates do not refresh; it is here so the
	# SP_*_TOTAL globals are this sp's, and not the previous case's, before the
	# first poll runs. Every value they hold after a wait is the WINNING poll's.
	sp_totals

	# standby_shape_ready resolves "the cntlr that is not the primary" out of its
	# own poll, which names one controller only at §7.1's cntlr_cnt 2. Asserted
	# here, the way react step 3 asserts the same assumption, so that a changed
	# §7.1 fails with this sentence instead of timing out later.
	assert_eq "$CNTLR_CNT" 2 \
		"setup's standby wait is written for §7.1's cntlr_cnt 2"

	# NEITHER WAIT BELOW IS PINNED TO A CNTLR ID, and the stage exits only when
	# the two agree. primary_stack_ready and standby_shape_ready each resolve
	# their role from the `sp get` of their own poll, because under react's
	# threshold set a failover during this build is expected — the first real run
	# had two — and either wait pinned to an id would end up watching the node in
	# the OTHER role, whose target it can then never reach: the stack target
	# against a standby that by CN12/CN13 builds nothing, or the emptiness target
	# against a primary that is building everything.
	#
	# Following the role inside a wait is not enough by itself, because the two
	# waits are consecutive and the role can move in the SECOND one, after the
	# first has already asserted a complete stack. The standby wait is exactly
	# where that is most likely: a demoted old primary tears down 32 pools and 64
	# md arrays before it looks like a standby, and the instant that teardown ends
	# its err_epoch clears and it is a failover candidate again
	# (worker/reaction.go:645-649, failoverEligible). What every step after this
	# one needs — setup_create_td's raid0 wait, step 09's namespace, step 10's
	# host connect, all of which read PRIMARY_* — is a primary that holds a
	# complete stack NOW. So the pair runs in a loop whose exit condition is that
	# statement and not an id comparison — see the check at the bottom of it —
	# bounded by SETUP_STACK_ROUNDS so that a lab which ping-pongs for ever fails
	# loudly instead of spinning.
	local round=0 built=""
	while :; do
		round=$((round + 1))
		if [ "$round" -gt "$SETUP_STACK_ROUNDS" ]; then
			die "the primary role of $SP moved after every one of" \
				"$SETUP_STACK_ROUNDS complete builds. Each move starts the" \
				"whole build again on the other CN (see WAIT_BUILD), so this" \
				"sp is not converging: the build is losing the race against" \
				"the $THR_SET threshold set ($THR) every time."
		fi
		stack_wait_reset
		wait_until "$WAIT_BUILD" \
			"the primary of $SP (cntlr $PRIMARY_CNTLR_ID on cn$PRIMARY_CN as this wait begins) to build its pools, groups and legs" \
			primary_stack_ready
		# The cntlr the winning poll inspected, i.e. the one the assertions
		# below are about. stack_wait_reset clears PRIMARY_WAIT_ID before the
		# next wait, so it is saved here rather than read again later.
		built=$PRIMARY_WAIT_ID
		assert_field "$CTL_OUT" '.cntlr_info.slice_id_to_dm_pool | length' \
			"$SLICE_CNT" "the primary reports one thin-pool row per slice"
		assert_field "$CTL_OUT" '.cntlr_info.grp_id_to_md_raid | length' \
			"$SP_GRP_TOTAL" "the primary reports one group row per group"
		assert_field "$CTL_OUT" "$CNTLR_OK_GRPS" "$SP_GRP_TOTAL" \
			"every group of the primary is RES_STATUS_OK"
		assert_field "$CTL_OUT" "$CNTLR_OK_POOLS" "$SLICE_CNT" \
			"every thin pool of the primary is RES_STATUS_OK"
		assert_field "$CTL_OUT" "$CNTLR_OK_LEGS" \
			"$((SP_LEG_TOTAL + SP_SPARE_TOTAL))" \
			"every leg of the primary is RES_STATUS_OK"
		# applied_revision is the agent's own view of how far it has converged
		# (InspectCntlrReply.applied_revision comes from the agent, not from the
		# rev key), so a zero here would mean the CN has never accepted a
		# revision-gated SyncupCntlr and every row above was read off a node that
		# is converging blind.
		assert_ge "$(jq_of "$CTL_OUT" '.applied_revision')" 1 \
			"the primary has applied at least one SyncupCntlr revision"

		# THE ROLES ARE RE-READ HERE, because the wait above may have followed a
		# failover: PRIMARY_*/STANDBY_* still name the controllers the sides-
		# provisioned reply described.
		sp_refresh
		sp_read_roles
		sp_totals
		if [ "$PRIMARY_CNTLR_ID" != "$built" ]; then
			log "!!! the primary moved again the moment its stack was" \
				"complete: cntlr $built built it, cntlr $PRIMARY_CNTLR_ID" \
				"holds the role now and has the same build ahead of it." \
				"Waiting for THAT one; that was round $round of" \
				"$SETUP_STACK_ROUNDS."
			continue
		fi

		stack_wait_reset
		wait_until "$WAIT_BUILD" \
			"the standby of $SP (cntlr $STANDBY_CNTLR_ID on cn$STANDBY_CN as this wait begins) to connect its $((SP_LEG_TOTAL + SP_SPARE_TOTAL)) legs and hold no group or pool" \
			standby_shape_ready
		setup_assert_standby "$CTL_OUT"
		assert_field "$CTL_OUT" "$CNTLR_OK_LEGS" \
			"$((SP_LEG_TOTAL + SP_SPARE_TOTAL))" \
			"every leg of the standby is RES_STATUS_OK (CN11's transport probe)"

		# THE EXIT CONDITION, and it is not "the same cntlr is still primary".
		# It is "the primary HOLDS a complete stack now", which is what every
		# step below assumes. The id can be unchanged and the statement false: a
		# demotion inside the standby wait is a teardown (CN12/CN13), so a cntlr
		# that was demoted and promoted again inside that window is primary under
		# its old id with its pools and groups gone. One extra `cntlr inspect`
		# settles it, and in the ordinary case it passes on the first try.
		sp_refresh
		sp_read_roles
		sp_totals
		if [ "$PRIMARY_CNTLR_ID" != "$built" ]; then
			log "!!! the primary moved while the standby's shape was being" \
				"waited on: cntlr $built built the stack, cntlr" \
				"$PRIMARY_CNTLR_ID holds the role now. The steps below read" \
				"PRIMARY_*, so the stack is waited out again; that was" \
				"round $round of $SETUP_STACK_ROUNDS."
			continue
		fi
		STACK_LAST=""
		if ! cntlr_stack_matches "$PRIMARY_CNTLR_ID"; then
			log "!!! cntlr $PRIMARY_CNTLR_ID is primary again but no longer" \
				"holds the stack it built: it was demoted and re-promoted" \
				"while the standby wait ran, and a demotion tears the pools" \
				"and groups down (CN12/CN13). Waiting for the rebuild; that" \
				"was round $round of $SETUP_STACK_ROUNDS."
			continue
		fi
		break
	done
	# The shape this build actually produced, said out loud. Under the quiet set
	# it must be the fresh-sp shape; under react's set it may already carry a
	# spare, and every later assertion of that case is written against THIS
	# number rather than against GRP_CNT x LEGS.
	if [ "$SP_SPARE_TOTAL" != 0 ] ||
		[ "$SP_LEG_TOTAL" != "$((GRP_CNT * LEGS))" ]; then
		log "!!! this build did not produce the fresh-sp shape:" \
			"$SP_LEG_TOTAL legs and $SP_SPARE_TOTAL spare leg(s) against" \
			"$((GRP_CNT * LEGS)) legs and 0 spares. A reaction fired during" \
			"setup; the sp carries the $THR_SET threshold set ($THR)."
	fi
}

# ---------------------------------------------------------------------------
# §7.4 step 8 — the thin device
# ---------------------------------------------------------------------------

setup_create_td() {
	# gateway/thindevice.go:154-171: a td's size must be a positive multiple
	# of slice_cnt x stripe_size, which is TD_UNIT (32 MiB at the default
	# shape). D27 makes t0 four of them, 128 MiB.
	TD0_SIZE=$((4 * TD_UNIT))
	stage 08 "td create --name $TD0 --size $TD0_SIZE ($((TD0_SIZE / 1048576)) MiB, 4 x TD_UNIT)"
	ctl_ok td create --name "$TD0" --size "$TD0_SIZE"
	TD0_ID=$(jq_of "$CTL_OUT" '.td_id')
	case "$TD0_ID" in
	'' | *[!0-9]*) die "td create returned td_id '$TD0_ID'" ;;
	esac

	# The gateway always writes `created` false; only the sp-worker flips it,
	# once the primary has materialised the thin volumes
	# (ThinDeviceCreated.md R13, ctl/td.go:104-108).
	wait_until "$WAIT_PROVISION" "$TD0 to report created" td_created "$TD0"
	assert_field "$CTL_OUT" '.name_to_td | length' 1 \
		"the sp holds exactly one thin device"
	assert_field "$CTL_OUT" ".name_to_td.$TD0.td_id" "$TD0_ID" \
		"td list's td_id vs the CreateThinDevice reply"
	assert_field "$CTL_OUT" ".name_to_td.$TD0.size" "$TD0_SIZE" \
		"$TD0's stored size (uint64, a JSON string)"
	assert_field "$CTL_OUT" ".name_to_td.$TD0.ori_id" 0 \
		"$TD0 is a fresh device, not a snapshot (uint32, a bare number)"

	# And the primary now carries its raid0. That is a DIFFERENT row from
	# `created`, which is defined on the thin volumes alone (see
	# cntlr_raid0_ready): CN16 rule 6 backs a live namespace's dm-linear with
	# the td's CnRaid0Name, so step 10 needs the raid0 to exist and not
	# merely the record to say created. In practice one build phase produces
	# both and this poll returns at once.
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to build $TD0's raid0" \
		cntlr_raid0_ready "$PRIMARY_CNTLR_ID" 1
}

# ---------------------------------------------------------------------------
# §7.4 step 9 — the subsystem, its hosts and the namespace
# ---------------------------------------------------------------------------

setup_export_ns() {
	stage 09 "ss create $SS0, set-hosts, ns create --idx 1 --td $TD0 --uuid $UUID1"

	# `ss create` is issued WITHOUT --hosts and the list is set afterwards,
	# which is §7.4 step 9's order and also exercises UpdateSubsystemHosts.
	# It leaves a window in which the subsystem's CdcEntry has an empty
	# allowed_hosts and is therefore visible to EVERY host (cdc/view.go:
	# 103-115, DS4). Nothing connects in that window: the hosts are masked
	# (rule 6) and every connect in this suite is the suite's own act.
	ctl_ok ss create --nqn "$SS0"
	SS0_ID=$(jq_of "$CTL_OUT" '.ss_id')
	case "$SS0_ID" in
	'' | *[!0-9]*) die "ss create returned ss_id '$SS0_ID'" ;;
	esac

	# Both hosts are allowed from here on: host0 consumes the sp's namespace
	# and host1 the transfer namespace (D13). These are the hosts' own
	# /etc/nvme/hostnqn values, read by read_host_identity in preflight.
	ctl_ok ss set-hosts --nqn "$SS0" --hosts "${HOST_NQN[0]},${HOST_NQN[1]}"

	# --uuid is always passed: an empty dev_uuid makes the gateway mint a
	# RANDOM v4 (gateway/subsystem.go:426-431), and the host resolves the
	# device as /dev/disk/by-id/nvme-uuid.<uuid>.
	ctl_ok ns create --nqn "$SS0" --idx 1 --td "$TD0" --uuid "$UUID1"
	NS1_ID=$(jq_of "$CTL_OUT" '.ns_id')
	case "$NS1_ID" in
	'' | *[!0-9]*) die "ns create returned ns_id '$NS1_ID'" ;;
	esac

	# The read-back. ListSubsystems is keyed by NQN, which is not a jq
	# identifier, so every filter here goes through `[.nqn_to_subsystem[]][0]`
	# — correct while the sp holds exactly one subsystem, which the first
	# assertion pins.
	ctl_ok ss list
	assert_field "$CTL_OUT" '.nqn_to_subsystem | length' 1 \
		"the sp holds exactly one subsystem"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem | keys[]][0]' "$SS0" \
		"the subsystem's nqn is the --nqn string, unmunged (CT8)"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ss_id' "$SS0_ID" \
		"ss list's ss_id vs the CreateSubsystem reply"
	assert_jq "$CTL_OUT" \
		"([.nqn_to_subsystem[]][0].allowed_hosts | sort)
		 == ([\"${HOST_NQN[0]}\", \"${HOST_NQN[1]}\"] | sort)" \
		"allowed_hosts is exactly the two hosts' own hostnqn"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list | length' 1 \
		"the subsystem holds one namespace"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list[0].ns_idx' 1 \
		"the namespace is nsid 1"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list[0].dev_uuid' \
		"$UUID1" "the namespace carries the uuid the suite chose"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list[0].td_id' \
		"$TD0_ID" "the namespace exports $TD0"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list[0].suspended' \
		false "the namespace is created live, not suspended"
}

# ---------------------------------------------------------------------------
# §7.4 step 10 — host0 reaches the namespace, and only through the cdc
# ---------------------------------------------------------------------------

setup_connect_host0() {
	stage 10 "host0 discovers $SS0 through the cdc on $CP_IP:$CDC_PORT and connects"

	disc_want_of_sp "$SS0"
	DISC_LAST=""
	wait_until "$WAIT_HOST" \
		"the cdc to serve $SS0 to host0 over its $CNTLR_CNT cntlr transports" \
		host_disc_is 0
	log "  host0 discovery log:"
	printf '%s\n' "$DISC_LAST" >&2
	# Counted on the RAW discovery document, not on DISC_LAST. host_disc_is
	# has just proved DISC_LAST = DISC_WANT as whole strings, and DISC_WANT is
	# built one line per non-disabled cntlr, so counting DISC_LAST's lines
	# would re-count what the wait already compared and could never fail.
	# `.records | length` is the one number in this step that nothing here
	# derived: it is what the cdc's reply actually carried, before
	# disc_records rendered it (enabledCntlrTrConfs writes one transport per
	# non-disabled cntlr into the CdcEntry, gateway/subsystem.go:188-194).
	assert_field "$DISC_JSON" '.records | length' "$CNTLR_CNT" \
		"one discovery record per non-disabled cntlr (enabledCntlrTrConfs)"

	# THE DISCOVERY LOG IS NOT THE DATA PLANE, and this is the wait run 3 did
	# not have. The cdc serves the log out of etcd, so it advertised both
	# transports the instant `ss create` committed — 1.75 s and 4.19 s before
	# the two CN agents had created the nvmet subsystem and linked it to their
	# ports, which is what makes those ports listen at all. Both connects took
	# ECONNREFUSED and connect-all exited 0 anyway. wait_ns_exported_all's
	# header has the measured timeline; it covers EVERY non-disabled cntlr,
	# because connect-all connects every transport the log offers.
	wait_ns_exported_all "$SS0" "$SS0_ID" "$NS1_ID"

	# E2E10: the host reaches its namespaces ONLY through the cdc, and this
	# connect is the suite's own act — nvmf-connect@.service is masked, so
	# nothing else can have made a path. host_connect_all proves a controller
	# for $SS0 exists afterwards; nvme-cli's exit code proves nothing.
	host_connect_all 0 "$SS0"

	# THE ORDER HERE IS NOT INTERCHANGEABLE. A namespace whose only path has
	# never been usable gets NO head disk — the multipath head is added the
	# first time a path goes live — so the ANA wait comes FIRST and wait_dev
	# after it. Doing it the other way round burns the whole WAIT_HOST budget
	# on a device that cannot appear yet.
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" optimized
	wait_dev 0 "$UUID1"

	# The standby's path to the SAME namespace is inaccessible, and that is
	# desired state rather than a fault: CN16 as amended by [D15] gives a
	# namespace AnaGrpIdOptimized only when the cntlr is primary, not
	# disabled, not effectively suspended and not provisioning-deferred
	# (agent/cnagent/plan.go:784-791); everything else stays in
	# AnaGrpIdInaccessible, whose port group carries ana_state
	# "inaccessible" (agent/nvmet.go:24-40, common/constants.go:233-235).
	host_wait_ana 0 "$SS0" "$STANDBY_TRADDR" "$UUID1" inaccessible

	# Both CONTROLLERS are live even though only one path serves IO: an ANA
	# state is per namespace, a path state is per controller, and confusing
	# the two is how a failover test asserts nothing.
	assert_eq "$(host_path_state 0 "$SS0" "$PRIMARY_TRADDR")" live \
		"host0's path to the primary cn$PRIMARY_CN"
	assert_eq "$(host_path_state 0 "$SS0" "$STANDBY_TRADDR")" live \
		"host0's path to the standby cn$STANDBY_CN"
	log "  host0 device $(host_dev "$UUID1") via nvme controller" \
		"$(host_ctrl 0 "$SS0" "$PRIMARY_TRADDR") (optimized)"
}

# ---------------------------------------------------------------------------
# §7.4 step 11 — the IO baseline every later stage compares against
# ---------------------------------------------------------------------------

setup_io_baseline() {
	stage 11 "host0 IO baseline: $BASELINE_MIB MiB written and read back as SHA0"
	local got dev
	dev=$(host_dev "$UUID1")
	PATTERN0="$WORK/pattern0"

	# The reference is a FILE of /dev/urandom on host0, so the same bytes can
	# be rewritten later without regenerating them. Its digest is SHA0.
	host_make_pattern 0 "$PATTERN0" "$BASELINE_MIB"
	SHA0=$(host_sha_range 0 "$PATTERN0" "$BASELINE_MIB") ||
		die "host0: digesting the reference pattern failed"
	case "$SHA0" in
	'' | *[!0-9a-f]*)
		die "host0: the reference digest is '$SHA0', not 64 hex digits"
		;;
	esac
	assert_eq "${#SHA0}" 64 "the reference digest is 64 hex digits"

	# Write with conv=fsync (rule 1: never iflag=/oflag=, the guests' uutils
	# dd 0.8.0 can report a successful oflag=direct write of ZEROS), then drop
	# caches so the read is answered by the raid0-over-thin stack and not by
	# host0's page cache.
	host_write_range 0 "$PATTERN0" "$dev" "$BASELINE_MIB" 0
	host_drop_caches 0
	got=$(host_sha_range 0 "$dev" "$BASELINE_MIB") ||
		die "host0: reading $dev back failed"
	assert_eq "$got" "$SHA0" \
		"host0's read-back of the first $BASELINE_MIB MiB of $dev (SHA0)"
	log "  SHA0 = $SHA0 ($BASELINE_MIB MiB at offset 0 of $dev)"
}

# ---------------------------------------------------------------------------
# The phases
# ---------------------------------------------------------------------------

# setup_case is §7.4 steps 4-11: everything that lives in etcd, built from an
# EMPTY one (E2E11).
#
# Every id below is minted by THIS build, so the identity globals are cleared
# before it starts rather than left to be overwritten. CLUSTER_ID matters most
# — require_cluster_id is what stops an NQN being computed for a cluster that
# no longer exists — and the cntlr arrays matter second: disc_want_of_sp only
# refuses an EMPTY one, so a stale pair of transports would quietly become the
# next case's discovery expectation if the run ever reached it before
# sp_read_roles.
setup_case() {
	SETUP_DONE=1
	CASE=setup
	CLUSTER_ID=""
	SP_ID=""
	SP_JSON=""
	CNTLR_IDS=()
	CNTLR_ADDRS=()
	CNTLR_TRADDRS=()
	CNTLR_TRSVCIDS=()
	CNTLR_DISABLED=()
	SS0_ID=""
	TD0_ID=""
	NS1_ID=""
	SHA0=""
	# The diagnostic registrations go too. They name (v, k) pairs of the sp
	# THIS case is about to build; a pair a previous case registered points at
	# a dn instance whose sp no longer exists, and the failure dump would tail
	# its log as if it were relevant.
	DIAG_DN=()

	setup_create_cluster
	setup_register_nodes
	setup_create_sp
	setup_wait_stack
	setup_create_td
	setup_export_ns
	setup_connect_host0
	setup_io_baseline
	log ""
	log "=== setup complete: $SLICE_CNT slices, $GRP_CNT groups," \
		"$((GRP_CNT * LEGS)) sides on $DN_TOTAL disk nodes," \
		"$CNTLR_CNT cntlrs, host0 on $(host_dev "$UUID1")"
}

# setup is what main calls before the first case.
setup() {
	setup_infra
	setup_case
}

# setup_between_cases is the step the case loop must run BETWEEN cases — not
# after the last one, where on_exit's cleanup_all already does the right thing.
# It is cleanup_all + setup_infra + setup_case, i.e. the whole of `setup` with
# a teardown in front: E2E11 says every case starts from an EMPTY etcd AND a
# freshly built sp, and cleanup_all has just removed the sp along with
# everything else, so the case half has to be rebuilt here too. (An earlier
# draft of this function stopped after setup_infra; the second case then ran
# `sp get` against an etcd with no cluster in it and died in its first stage.)
#
# It is deliberately the big hammer, for the reason argued at the top of this
# section: a new `cluster create` mints a new cluster_id and new dn_ids, and a
# DN's disk header names the OLD ones, so the agent would refuse every disk as
# foreign for the rest of the run. Only a real dn_cleanup removes that header
# — loop_teardown zeroes each loop's first 4 KiB, wipefs's it and detaches it,
# and the `rm -rf $WORK` that follows takes the backing file and the agent's
# --local-store with it — and only the two cn phases remove the CN's store and
# its tmpfs arena. So the between-cases step is cleanup_all followed by a
# fresh setup_infra and a fresh setup_case, and the run pays for a second full
# build per case. That build is the 8m45s window of the WAIT_BUILD comment, not
# the ~5 minutes §7.4 budgets, so a four-case run is well over an hour.
#
# cleanup_all never dies, but cleanup_start_gate after it does: a verb that
# never reached its sentinel here means the previous case's debris is still on
# that guest, and the next case would be built on top of it. A guest that was
# swept and is merely still dirty (a REFUSED or STUCK port) is a loud warning
# here and a failure at the next assertion, which is the right order for a
# finding preflight already explains better.
# cleanup_all also unmasks the hosts and removes $WORK everywhere, which is why
# setup_infra re-masks and re-ships.
setup_between_cases() {
	CASE=setup
	# stage() and not a bare log line, although this step issues no dnvctl call
	# of its own. STAGE and TRACE are what die() reports, and cleanup_start_gate
	# below is the first die site in this function — before setup_infra's
	# `stage 01` — so without this a cleanup failure between cases would be
	# reported at the PREVIOUS case's last stage and trace id, filed under an
	# assertion it has nothing to do with.
	stage 00 "between cases: tearing the data plane down and rebuilding it"
	log "    (a fresh cluster_id makes every existing DN disk header foreign,"
	log "     agent/dnagent/diskmeta.go:299-324 — an etcd reset alone is not"
	log "     enough)"
	cleanup_all
	# The between-cases sweep is a START cleanup for the case that follows —
	# E2E11 wants that case to begin from an EMPTY etcd and a freshly built
	# sp, which a guest that was not swept cannot give it. Same gate, same
	# reason as main's.
	cleanup_start_gate "between-cases"
	setup_infra
	# setup_case is re-entrant by construction: its first act is to clear
	# CLUSTER_ID, SP_ID, SP_JSON, the five CNTLR_* arrays, SS0_ID, TD0_ID,
	# NS1_ID, SHA0 and DIAG_DN, so nothing of the previous case can be read by
	# mistake.
	setup_case
}

# ---------------------------------------------------------------------------
# The ending every case shares (§7.5's "teardown as in smoke", §7.6, §7.8)
# ---------------------------------------------------------------------------
#
# Each of the four cases ends the same way: delete what the case built, delete
# the sp, wait for the sp-worker's drain to make `sp get` answer NOT_FOUND,
# assert that nothing of the sp survives on any guest, and assert the two D16
# allocation caps. smoke IS that ending — its whole content is setup plus this —
# and ops, copy and react put their own steps in front of it. The three pieces
# are `case_teardown`, `case_residue` and `case_space_guard`, and `case_finish`
# is the three in order.
#
# THREE ORDERING FACTS, each re-derived rather than copied:
#
#  a. The NAMESPACE goes first and the SUBSYSTEM only after the hosts have
#     disconnected. `ns delete` removes an nvmet namespace under a live
#     controller and the host answers by dropping the head disk — that is the
#     one direction in which `wait_dev_gone` means anything, because a SUSPEND
#     never removes the node (it parks the ns-dev on the td's dm-error,
#     agent/cnagent/plan.go:307-315). `ss delete` is a different act: a
#     subsystem unlinked from its port under a live controller kills that
#     controller with DNR and the host never reconnects by itself (memory note
#     nvmet-port-unlink-dnr-kills-host-ctrl), so the hosts stop first. That is
#     the same order, for the same reason, that cleanup_all uses.
#  b. `sp delete` is REFUSED while the sp still holds a thin device, a
#     subsystem, a clone, a transfer or a migration: those are objects a user
#     made and must remove first, while cntlrs, slices, groups, legs and sides
#     are created implicitly and drained implicitly
#     (gateway/storagepool.go:616-648, "The five name lists are the whole
#     precondition"). So case_teardown ASSERTS first that the case left exactly
#     one subsystem, one namespace and one thin device. A case that forgot to
#     remove its own objects then fails here with that sentence rather than
#     with a bare FAILED_PRECONDITION from the gateway three calls later.
#  c. `sp delete` LATCHES and returns (same comment, and gateway.md §5.4 as
#     amended 2026-09-15): the sp still exists when the reply arrives and the
#     worker takes it apart in bounded steps, so NOT_FOUND from `sp get` is the
#     only completion signal there is — "an observer polls GetStoragePool until
#     NOT_FOUND" is the handler's own wording.
# ---------------------------------------------------------------------------

# The digest the last host_sha_is read, kept for the failure message the way
# ANA_LAST is (section 2): wait_until's timeout names the wait, not the
# observation. Written ONLY by host_sha_is, and only as an assignment in the
# predicate's own shell, because wait_until runs its predicates in the parent.
SHA_LAST=""

# The last residue listing a *_residue_empty predicate saw, for the same reason.
# Both of these are reported by LOGGING from inside the predicate when the value
# changes, and never by interpolating them into a wait_until label: a label is
# expanded once, at the call, so it can only ever carry what was there BEFORE
# the first poll.
RESIDUE_LAST=""

# host_sha_is is the polling form of "the device reads back as <want>". It
# drops caches first, so the answer comes from the raid0-over-thin stack and
# not from host <h>'s page cache, and it is for a read that CANNOT block — a
# parked, suspended or ANA-inaccessible device needs host_sha_probe instead
# (rule 5: `timeout` does not bound a wedged read).
host_sha_is() { # <h> <path> <countMiB> <want>
	host_drop_caches "$1" >/dev/null || return 1
	SHA_LAST=$(host_sha_range "$1" "$2" "$3") || return 1
	[ "$SHA_LAST" = "$4" ]
}

# host_wait_sha is the bounded version. A read taken the instant a dnvctl call
# returns is a read of a stack the CN has not converged yet — every mutator
# below returns as soon as the gateway has written etcd — so every data check
# in this file is a WAIT with a budget and not a single dd.
host_wait_sha() { # <h> <path> <countMiB> <want> <secs> <label>
	SHA_LAST=""
	wait_until "$5" "$6" host_sha_is "$1" "$2" "$3" "$4"
	log "  host$1: sha256 of the first $3 MiB of $2 = $SHA_LAST"
}

# check_sha0 is §7.5's "✓" in one line: host0's namespace still holds the
# BASELINE_MIB MiB setup wrote, byte for byte.
check_sha0() { # <what just happened>
	local dev
	dev=$(host_dev "$UUID1")
	host_wait_sha 0 "$dev" "$BASELINE_MIB" "$SHA0" "$WAIT_HOST" \
		"host0 to read SHA0 back from $dev $1"
}

# sp_gone is the drain's completion signal (c). NOT_FOUND is the ONLY answer
# that counts: an UNAVAILABLE from a gateway that died mid-drain, or an ABORTED
# from etcd, must keep the poll going rather than end it, or a dead control
# plane would read as a finished teardown.
sp_gone() {
	if ctl_try sp get; then
		return 1
	fi
	case "$CTL_ERR" in
	"dnvctl: NOT_FOUND: "*) return 0 ;;
	esac
	return 1
}

# dn_capacity_free is §7.5 smoke's "every DN's free_ext_cnt is back to its
# post-format value". The post-format value is not recorded anywhere and does
# not need to be: CreateDiskNode computes total_ext_cnt from the node's disk
# size and the cluster's extent_size and a DN that carries nothing has
# free_ext_cnt equal to it, so the invariant is free == total with an empty
# side_ptr_list (pb/schema.proto:332-342). Checking both is what separates
# "the capacity came back" from "the pointer list was cleared but the number
# was not" — the two are written by the same ledger flush and a mismatch
# between them is the bug this assertion exists for.
DN_CAP_LAST=""
dn_capacity_free() { # <v> <k>
	local free total sides sig
	if ! ctl_try dn get --addr "$(dn_addr "$1" "$2")"; then
		return 1
	fi
	free=$(jq_of "$CTL_OUT" '.dn_conf.free_ext_cnt')
	total=$(jq_of "$CTL_OUT" '.dn_conf.total_ext_cnt')
	sides=$(jq_of "$CTL_OUT" '.dn_conf.side_ptr_list | length')
	sig="free_ext_cnt $free of $total, $sides side pointer(s)"
	# DN_CAP_LAST is NOT reset between disk nodes: every DN of this run has the
	# same disk and the same extent size, so a clean sweep logs this line once
	# and a DN that still holds an extent logs itself.
	if [ "$sig" != "$DN_CAP_LAST" ]; then
		DN_CAP_LAST=$sig
		log "  dn$1 instance $2: $sig"
	fi
	case "$total" in
	'' | *[!0-9]* | 0) return 1 ;;
	esac
	[ "$free" = "$total" ] && [ "$sides" = 0 ]
}

# The two residue predicates. Both verbs echo one line per surviving object and
# nothing at all when the guest is clean, so "empty" is the pass — and the
# emptiness test strips whitespace, because a verb that printed only a newline
# would otherwise read as a residue named "".
dn_residue_empty() { # <v>
	local out
	out=$(helper_dn "$1" dn_residue) || return 1
	if [ -n "${out//[[:space:]]/}" ] && [ "$out" != "$RESIDUE_LAST" ]; then
		RESIDUE_LAST=$out
		log "  dn$1 still holds:"
		printf '%s\n' "$out" >&2
	fi
	[ -z "${out//[[:space:]]/}" ]
}

cn_residue_empty() { # <v>
	local out
	out=$(helper_cn "$1" cn_residue) || return 1
	if [ -n "${out//[[:space:]]/}" ] && [ "$out" != "$RESIDUE_LAST" ]; then
		RESIDUE_LAST=$out
		log "  cn$1 still holds:"
		printf '%s\n' "$out" >&2
	fi
	[ -z "${out//[[:space:]]/}" ]
}

# dn_md_residue lists any md array on a DN VM whose name carries the dnv
# prefix. It is the same filter cn_residue applies on a CN, and it is now the
# same CODE: both call the shared node body's `md_names`, which reads MD_NAME
# per array the way md_stop_all does.
#
# WHAT IT IS MEANT TO PROVE, EXACTLY: only the CN assembles arrays ON PURPOSE
# (doc/cnagent.md CN12: "Groups (md.go; primary only)"), so a dnv-named array
# on a DN VM means something else assembled one — which is not hypothetical:
# the cn writes each leg's md superblock through the side's nvme-tcp export, so
# it lands on the DN's storage, and an unmasked DN assembles it (rule 7,
# install_udev_rule). This is therefore the assertion that the DN's md mask
# WORKED, not a formality. It is still not a general "no md on this guest"
# check: it names dnv arrays only, and the guest's own arrays are none of its
# business.
#
# THREE WAYS IT COULD ANSWER EMPTY WITHOUT PROVING ANYTHING. Two are closed by
# preflight and by the wrapper: a guest without mdadm or udevadm answers empty,
# and both are in DN_TOOLS now — dn_up's mask, dn_cleanup's md_stop_all and
# md_names all need them — so preflight fails first on a DN that lacks either;
# and an ssh or sudo that fails at this moment also answers empty, which no
# tool list covers, so the wrapper is the DYING helper_dn (over ssh_dn, not
# ssh_dn_ok) and the caller dies on a non-zero status, the shape
# dn_residue_empty and cn_residue_empty already have.
#
# The third was the one that mattered, and it is now CLOSED. This probe used to
# run `mdadm --detail --scan | grep -oE 'name=…dnv-…'`, and on these guests that
# scan prints no `name=` field at all (md_stop_all's comment carries the
# measurement and the bash -x trace), so the grep matched nothing on every DN,
# always, and the assertion passed whatever the guest held — while the DN md
# mask it exists to verify was exactly the thing that had been wrong. It reads
# MD_NAME per array now, the way md_stop_all does. cn_residue's md line had the
# identical hole and takes the identical fix, through the same `md_names`.
dn_md_residue() { # <v>
	helper_dn "$1" md_names
}

case_teardown() {
	stage 90 "teardown: ns, hosts, ss, td, then \`sp delete\` and the drain"
	local h wout left

	# (b): prove the case cleaned up after itself BEFORE anything is deleted.
	ctl_ok ss list
	assert_field "$CTL_OUT" '.nqn_to_subsystem | length' 1 \
		"the case must delete its own subsystems; only $SS0 may reach the teardown"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem | keys[]][0]' "$SS0" \
		"the one surviving subsystem is $SS0"
	assert_field "$CTL_OUT" '[.nqn_to_subsystem[]][0].ns_list | length' 1 \
		"the case must delete its own namespaces; only ns_idx 1 may reach the teardown"
	ctl_ok td list
	assert_field "$CTL_OUT" '.name_to_td | length' 1 \
		"the case must delete its own thin devices; only $TD0 may reach the teardown"
	assert_jq "$CTL_OUT" ".name_to_td | has(\"$TD0\")" \
		"the one surviving thin device is $TD0"

	# (a), first half: the namespace, under the live controllers, and the one
	# assertion that distinguishes a real removal from §11.6's park.
	ctl_ok ns delete --nqn "$SS0" --idx 1
	assert_field "$CTL_OUT" '.ns_id' "$NS1_ID" "DeleteNamespaceReply.ns_id"
	wait_dev_gone 0 "$UUID1"

	# (a), second half: now the hosts let go, before any subsystem is unlinked.
	# `wipe` drops every $NQN_IT and $NQN_PREFIX subsystem this suite could have
	# connected plus the discovery controller pointing at the cdc, and never
	# `nvme disconnect-all`.
	#
	# THE RESULT IS READ, not merely status-checked. Every `nvme disconnect`
	# inside wipe is `|| true` with its output discarded, so the verb exits 0
	# whether the connections went or not — and this is the one place where
	# that matters: `ss delete` below unlinks the subsystem, and a subsystem
	# that disappears under a live controller kills it with DNR (memory note
	# nvmet-port-unlink-dnr-kills-host-ctrl). wipe re-reads sysfs and prints
	# `wipe_left=` when anything survived.
	for h in "${!HOST[@]}"; do
		wout=$(helper_host "$h" wipe "$CP_IP") ||
			die "host$h: the wipe verb failed, so the subsystem below would be" \
				"unlinked under a live controller"
		case "$wout" in
		*wipe_left=*)
			printf '%s\n' "$wout" >&2
			# The surviving NQNs are NAMED rather than assumed to be $SS0.
			# `wipe` sweeps $NQN_IT and $NQN_PREFIX, and $NQN_PREFIX is
			# `nqn.2024-01.io.dnv` with no terminator, so a `wipe_left=` line
			# can just as well carry cdc_test.sh's `nqn.2024-01.io.dnv-it:cdc:*`
			# from the host guests the two suites share (see wipe's header).
			# The DNR sentence is about $SS0 and belongs only to a survivor
			# that IS $SS0, so it is said only then.
			left=$(printf '%s\n' "$wout" | sed -n 's/^wipe_left=//p' | tail -n 1)
			case " $left " in
			*" $SS0 "*)
				die "host$h still holds a connection to: $left." \
					"\`wipe\` swept both dnv NQN prefixes and these survived." \
					"$SS0 is among them, and \`ss delete\` below would unlink" \
					"it under a live controller and kill that controller with" \
					"DNR, so this stops here instead."
				;;
			*)
				die "host$h still holds a connection to: $left." \
					"\`wipe\` swept both dnv NQN prefixes and these survived." \
					"$SS0 is NOT among them, so the DNR hazard \`ss delete\`" \
					"poses is not the immediate one — but a sweep that did not" \
					"finish is itself the fault to read, and another dnv" \
					"suite holding paths on this guest breaks rule 8."
				;;
			esac
			;;
		esac
	done

	ctl_ok ss delete --nqn "$SS0"
	assert_field "$CTL_OUT" '.ss_id' "$SS0_ID" "DeleteSubsystemReply.ss_id"
	ctl_ok td delete --name "$TD0"
	assert_field "$CTL_OUT" '.td_id' "$TD0_ID" "DeleteThinDeviceReply.td_id"

	# (c): the latch, then the poll. WAIT_DELETE, not WAIT_PROVISION: the drain
	# has to retire every side on every DN, which is the same order of work the
	# create did.
	#
	# This one call does not go through ctl_ok, and the reason is the failure
	# message. ctl_ok's first assertion is on the exit code, so a refusal would
	# report "got '1', want '0'" and throw away the sentence that says WHICH
	# object is still there — and the pre-checks above cover only subsystems and
	# thin devices, while DeleteStoragePool's precondition is five name lists
	# (clones, transfers and migrations too, gateway/storagepool.go:623-628).
	ctl_exec sp delete
	if [ "$CTL_RC" != 0 ]; then
		die "\`sp delete\` was refused, so the case left an object behind that" \
			"the teardown does not pre-check (a clone, a transfer or a" \
			"migration): $CTL_ERR"
	fi
	assert_eq "$CTL_ERR" "" "stderr of: dnvctl sp delete must be empty"
	assert_parses "$CTL_OUT" "dnvctl sp delete"
	assert_field "$CTL_OUT" '.sp_id' "$SP_ID" "DeleteStoragePoolReply.sp_id"
	wait_until "$WAIT_DELETE" \
		"the sp-worker's drain to make \`sp get\` answer NOT_FOUND" \
		sp_gone
	SP_JSON=""

	# The name is reusable again only once the drain's last step has removed the
	# sp_conf key, which is exactly what the NOT_FOUND above proves; this reads
	# the other index the drain maintains.
	ctl_ok sp list
	assert_jq "$CTL_OUT" "[.sp_name[]? | select(. == \"$SP\")] | length == 0" \
		"$SP is gone from \`sp list\` too"
	log "  $SP drained: gone from both \`sp get\` and \`sp list\`"
}

case_residue() {
	# READ THE md THIRD OF THIS STAGE AS "NOT ASKED" (doc §8 items 8 and 17):
	# both probes below still grep `mdadm --detail --scan` for a name that
	# command does not print on these guests, so their md half matches nothing
	# on every guest and passes whatever is held. The dm and nvmet thirds are
	# real. The stage banner below still names md because that is what the
	# stage is meant to assert; every line that reports a RESULT and names md
	# — the DN assertion text, the CN wait label and the closing log — says
	# the md third was not asked, and they go back to plain claims when the
	# probes move to MD_NAME the way md_stop_all did.
	stage 91 "residue: no DN capacity held, no dm/md/nvmet object of $SP left"
	local v k out

	# The capacity first. One `dn get` per instance — DN_TOTAL calls, which is
	# the price of checking every node rather than a sample, and each poll
	# normally returns on its first try.
	DN_CAP_LAST=""
	for v in "${!DN[@]}"; do
		for ((k = 0; k < DNS_PER_VM; k++)); do
			wait_until "$WAIT_DELETE" \
				"dn$v instance $k ($(dn_addr "$v" "$k")) to get all its extents back" \
				dn_capacity_free "$v" "$k"
		done
		log "  dn$v: all $DNS_PER_VM disk nodes back to free_ext_cnt == total_ext_cnt"
	done

	# Then the guests themselves. dn_residue is dm devices plus tree-minted
	# nvmet subsystems; cn_residue adds the dnv md arrays — blindly, see the
	# note on the stage line and on dn_md_residue. Loop devices are
	# deliberately in neither: the agents keep serving on them until cleanup, so
	# they belong to the run and not to the sp.
	RESIDUE_LAST=""
	for v in "${!DN[@]}"; do
		wait_until "$WAIT_DELETE" \
			"dn$v to hold no dm device and no $NQN_PREFIX:* subsystem" \
			dn_residue_empty "$v"
		# A failure here is a failure, not an empty answer: the probe cannot
		# pass by being unable to ask.
		out=$(dn_md_residue "$v") ||
			die "dn$v: listing the md arrays failed (ssh or sudo), so the" \
				"'no dnv md array on a DN' assertion could not be taken"
		# Only a CN assembles one (CN12) — but this probe is the blind
		# one, so a pass here is "not asked" (doc §8 items 8 and 17).
		assert_eq "${out//[[:space:]]/}" "" \
			"dn$v holds no dnv md array (NOT ASKED, §8 item 17)"
	done
	for v in "${!CN[@]}"; do
		wait_until "$WAIT_DELETE" \
			"cn$v to hold no dm device or subsystem of this suite (its md list is the blind probe)" \
			cn_residue_empty "$v"
	done
	log "  every dn and cn guest is free of this sp's dm and nvmet objects" \
		"(the md third was NOT ASKED — doc §8 item 17)"
}

# read_space fills the three globals from one guest's `space` verb. It exists
# so that the numeric validation happens ONCE and in the PARENT shell: a die
# inside a $( ) would end only the subshell, and `$((total + ""))` on an empty
# answer is a syntax error rather than a readable failure.
SPACE_WORK=0
SPACE_TMPFS=0
SPACE_FREE=0
read_space() { # <helper wrapper> <index|""> <label>
	local out key
	if [ -n "$2" ]; then
		out=$("$1" "$2" space) || die "$3: the space verb failed"
	else
		out=$("$1" space) || die "$3: the space verb failed"
	fi
	SPACE_WORK=$(space_field "$out" work)
	SPACE_TMPFS=$(space_field "$out" tmpfs)
	SPACE_FREE=$(space_field "$out" free)
	for key in "$SPACE_WORK" "$SPACE_TMPFS" "$SPACE_FREE"; do
		case "$key" in
		'' | *[!0-9]*)
			die "$3: the space verb answered '$out', which is not three byte counts"
			;;
		esac
	done
}

# case_space_guard is D16 / E2E5, run after every case.
#
# Two caps, and they measure different things:
#
#   DN_CAP_BYTES  ALLOCATED bytes of ONE backing file. The whole space argument
#                 of this suite is that a `truncate`d sparse file stays sparse
#                 because side zeroing is `blkdiscard --zeroout`
#                 (agent/dm.go:250-261) and the loop device turns WRITE_ZEROES
#                 into a hole punch. A file that has materialised is the
#                 symptom of exactly one thing — write_zeroes_max_bytes gone to
#                 0 on that loop device, which the agent only TAGS and never
#                 refuses (agent/dnagent/syncup_dn.go:505-517) — so the per-file
#                 cap is the check that names it.
#   RUN_CAP_BYTES everything this run wrote on all ten guests, $WORK plus
#                 $TMPFS_DIR. The tmpfs is counted separately because it is NOT
#                 under $WORK: CnTmpfsPath is fixed at common.DefaultTmpfsPrefix
#                 and no flag moves it (common/name_fmt.go:57-67, :393-402), so
#                 a guard that only looked at $WORK would miss a CN's whole
#                 clone-metadata arena.
#
# The free-space floors are preflight's own, deliberately rather than §7.8's
# flat 10 GiB: the statement worth making is "the run left the guest as usable
# as preflight demanded it be", and one number in two places that disagree is
# how a floor stops meaning anything. The hosts have no preflight floor (they
# run no dnv binary and hold only the pattern file), so their numbers are
# reported and counted but not asserted against a floor of their own.
case_space_guard() {
	stage 92 "space guard (D16/E2E5): the per-file and whole-run allocation caps"
	local total=0 v k paths out bytes path worst

	for v in "${!DN[@]}"; do
		paths=""
		for ((k = 0; k < DNS_PER_VM; k++)); do
			paths="$paths $(dn_backing "$k")"
		done
		# Unquoted on purpose: `alloc` takes one path per argument and helper_dn
		# joins them with spaces. Every path here is space-free.
		# shellcheck disable=SC2086
		out=$(helper_dn "$v" alloc $paths) || die "dn$v: the alloc verb failed"
		worst=0
		while read -r bytes path; do
			[ -n "$path" ] || continue
			case "$bytes" in
			'' | *[!0-9]*)
				die "dn$v: alloc reported '$bytes' for $path —" \
					"'unknown' means stat failed, which is a fault to chase," \
					"not a file that costs nothing"
				;;
			esac
			assert_le "$bytes" "$DN_CAP_BYTES" \
				"dn$v: allocated bytes of $path (a sparse file that materialised)"
			[ "$bytes" -le "$worst" ] || worst=$bytes
		done < <(printf '%s\n' "$out")
		log "  dn$v: largest backing file allocates $worst bytes (cap $DN_CAP_BYTES)"
	done

	for v in "${!DN[@]}"; do
		read_space helper_dn "$v" "dn$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
		assert_bytes "$SPACE_FREE" \
			"$((FREE_MIN_NODE + DNS_PER_VM * FREE_PER_DN))" \
			"dn$v: free bytes under /var/tmp after the case"
		log "  dn$v: work=$SPACE_WORK tmpfs=$SPACE_TMPFS free=$SPACE_FREE"
	done
	for v in "${!CN[@]}"; do
		read_space helper_cn "$v" "cn$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
		assert_bytes "$SPACE_FREE" "$FREE_MIN_NODE" \
			"cn$v: free bytes under /var/tmp after the case"
		log "  cn$v: work=$SPACE_WORK tmpfs=$SPACE_TMPFS free=$SPACE_FREE"
	done
	for v in "${!HOST[@]}"; do
		read_space helper_host "$v" "host$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
		log "  host$v: work=$SPACE_WORK tmpfs=$SPACE_TMPFS free=$SPACE_FREE"
	done
	read_space helper_cp "" cp
	total=$((total + SPACE_WORK + SPACE_TMPFS))
	assert_bytes "$SPACE_FREE" "$FREE_MIN_CP" \
		"cp: free bytes under /var/tmp after the case"
	log "  cp: work=$SPACE_WORK tmpfs=$SPACE_TMPFS free=$SPACE_FREE"

	assert_le "$total" "$RUN_CAP_BYTES" \
		"allocated bytes this run holds on all ten guests"
	log "  total allocated on all ten guests: $total bytes (cap $RUN_CAP_BYTES)"
}

case_finish() {
	case_teardown
	case_residue
	case_space_guard
}

# ---------------------------------------------------------------------------
# Case: smoke (§7.5)
# ---------------------------------------------------------------------------
#
# "setup 1-11, then teardown … then the residue assertions." Setup has already
# run when this is called — main builds the sp before the case loop — so smoke
# adds no operation of its own. What it is FOR is the pair of statements the
# other three cases each assume and none of them proves on its own: that the
# widest sp this tree can build comes up whole, and that deleting it gives
# every extent back and leaves no dm, md or nvmet object behind on any of the
# seven data-plane guests.
#
# It is the cheapest case and it runs first (CASES=(smoke ops copy react)), so
# a lab that cannot build the shape at all fails in one build rather than four.
case_smoke() {
	CASE=smoke
	stage 01 "smoke: the sp setup built is the subject; this case tears it down"
	sp_refresh
	sp_read_roles
	assert_field "$SP_JSON" '.sp_conf.sp_id' "$SP_ID" \
		"\`sp get\` still answers for the sp setup created"
	assert_field "$SP_JSON" '.slice_list | length' "$SLICE_CNT" "slices"
	# THESE TWO ABSOLUTES ARE DELIBERATE, and after §7.1's per-case thresholds
	# they carry a second statement as well as the first. smoke's sp is built
	# with the QUIET set, so no reaction can fire during or after its build: a
	# side count that is not GRP_CNT x LEGS, or a spare leg at all, means one
	# did — which is a finding about the lab, not a shape to accommodate. This
	# is the opposite choice from the react case's, for the opposite reason.
	assert_field "$SP_JSON" "[$SP_SIDE_PATH] | length" "$((GRP_CNT * LEGS))" \
		"sides (nothing has grown the sp, and the quiet thresholds let nothing react)"
	assert_field "$SP_JSON" "[$SP_SPARE_LEG_PATH] | length" 0 \
		"spare legs (the quiet thresholds mean AR8 cannot have created one)"
	assert_field "$SP_JSON" '.sp_conf.sp_level' SP_LEVEL_READWRITE "sp_level"
	check_sha0 "before the teardown"
	case_finish
}

# ---------------------------------------------------------------------------
# Case: ops (§7.5) — the nine numbered steps
# ---------------------------------------------------------------------------
#
# Every sp-scoped mutator and reader dnvctl has, against the real gateway, on
# the sp setup built; host0's data is re-read after each step §7.5 marks, and
# the digest must still be SHA0.
#
# The three names ops adds to the sp. All three are bare jq identifiers,
# because `td list` is keyed by td_name and `.name_to_td.<name>` is how every
# filter here reaches a row.
SNAP0=s0
TD1=t1

# The current shape of the sp, recomputed from a `sp get` rather than from
# GRP_CNT. Step 2 grows two slices, so from that point on the constants setup
# asserted against are stale; a spare leg or an automatic reaction moves them
# too. Setup uses these same general forms — the fresh-sp predicates that once
# sat beside cntlr_raid0_ready were deleted for the reason their headstone
# there gives.
SP_GRP_TOTAL=0
SP_LEG_TOTAL=0
SP_SPARE_TOTAL=0
SP_SIDE_TOTAL=0

sp_totals() {
	sp_totals_of "$SP_JSON" "SP_JSON"
}

# sp_totals_of is the one parser both forms share. <what> names the document for
# the failure message, because the two callers hold different ones: sp_totals
# parses the last `sp get` sp_refresh stored, and sp_totals_poll parses a reply
# it fetched itself, one poll ago.
sp_totals_of() { # <sp get reply> <what>
	SP_GRP_TOTAL=$(jq_of "$1" "[$SP_GRP_PATH] | length")
	SP_LEG_TOTAL=$(jq_of "$1" "[$SP_LEG_PATH] | length")
	SP_SPARE_TOTAL=$(jq_of "$1" "[$SP_SPARE_LEG_PATH] | length")
	SP_SIDE_TOTAL=$(jq_of "$1" "[$SP_SIDE_PATH] | length")
	local n
	for n in "$SP_GRP_TOTAL" "$SP_LEG_TOTAL" "$SP_SPARE_TOTAL" "$SP_SIDE_TOTAL"; do
		case "$n" in
		'' | *[!0-9]*)
			die "sp_totals read '$n' out of \`sp get\`; $2 is not an sp"
			;;
		esac
	done
}

# ---------------------------------------------------------------------------
# The shape target, re-read on every poll
# ---------------------------------------------------------------------------
#
# WHY IT CANNOT BE READ ONCE, from the first real run (2026-09-17, deca203).
# Two predicates had the same defect in two shapes: setup's cntlr_stack_ready
# took its leg target from the CONSTANT GRP_CNT x LEGS, and cntlr_full_ready
# took its from a `sp_totals` reading made once, before the wait began. Setup
# step 7 hit the first of them. While it waited, AR8 created a spare leg — the
# reacting thresholds were still in force for every case then — and the agent
# correctly reported 129 leg rows, because CN10 walks spare_leg_list as well as
# leg_list. The target stayed 128. The transcript's last line is
#
#     cntlr 1: pools 21/32, groups 49/64, legs 129/128
#
# and no amount of time could have made that poll pass: the sp really did hold
# 128 legs and exactly one spare, and the agent's leg_id_to_leg held exactly
# those 129 ids. A wait whose target cannot be reached is not a slow wait, it is
# a hang with a stopwatch on it.
#
# So the two predicates below re-read the totals on EVERY poll, with one extra
# `sp get` per poll as the price, and they SHOUT when the shape moves under
# them. The shout is not decoration: while one of these waits is running the
# suite itself is blocked, so nothing it did can have changed the shape — and
# after §7.1's per-case thresholds a smoke, ops or copy build can no longer
# produce one either. A moved total in those three cases means a reaction fired
# when none should have, and that is a finding about the run, not a hiccup to
# absorb.

# The "<legs>+<spares>" of the last live reading, per wait. stack_wait_reset
# clears it at the start of every such wait, so the baseline is that wait's own
# first poll and never a number a previous step left behind.
SP_SHAPE_LAST=""

stack_wait_reset() {
	STACK_LAST=""
	SP_SHAPE_LAST=""
	PRIMARY_WAIT_ID=""
	STANDBY_WAIT_ID=""
	STANDBY_SHAPE_LAST=""
}

# sp_totals_poll refreshes the four totals from a fresh `sp get` and answers
# false when the gateway could not be reached — a poll that cannot read the sp
# must go round again, never compare against a half-filled target. It does NOT
# touch SP_JSON: its callers are predicates whose successful poll must leave
# $CTL_OUT holding the `cntlr inspect` reply the assertions after the wait read,
# which is also why they call this FIRST and inspect second.
sp_totals_poll() {
	if ! ctl_try sp get; then
		return 1
	fi
	sp_totals_of "$CTL_OUT" "the \`sp get\` of a shape poll"
	local sig="$SP_LEG_TOTAL legs + $SP_SPARE_TOTAL spare leg(s)"
	if [ -n "$SP_SHAPE_LAST" ] && [ "$sig" != "$SP_SHAPE_LAST" ]; then
		log "!!! $SP's shape MOVED while a wait was running:" \
			"$SP_SHAPE_LAST -> $sig."
		log "!!! The suite is blocked in that wait, so it did not do this: an" \
			"automatic reaction did. In the $CASE case the sp carries the" \
			"$THR_SET threshold set ($THR)."
	fi
	SP_SHAPE_LAST=$sig
	return 0
}

# cntlr_stack_matches is the comparison itself: one thin-pool row per slice, one
# group row per group, one leg row per leg, all RES_STATUS_OK. The leg count
# includes the SPARE legs because CN10 walks spare_leg_list too. It ASSUMES the
# SP_*_TOTAL globals were refreshed by the caller's own sp_totals_poll in this
# same poll, which is why it is not called from anywhere else.
cntlr_stack_matches() { # <cntlr id>
	local pools grps legs want sig
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	pools=$(jq_of "$CTL_OUT" "$CNTLR_OK_POOLS")
	grps=$(jq_of "$CTL_OUT" "$CNTLR_OK_GRPS")
	legs=$(jq_of "$CTL_OUT" "$CNTLR_OK_LEGS")
	want=$((SP_LEG_TOTAL + SP_SPARE_TOTAL))
	sig="pools $pools/$SLICE_CNT, groups $grps/$SP_GRP_TOTAL, legs $legs/$want"
	if [ "$sig" != "$STACK_LAST" ]; then
		STACK_LAST=$sig
		log "  cntlr $1: $sig"
	fi
	[ "$pools" = "$SLICE_CNT" ] && [ "$grps" = "$SP_GRP_TOTAL" ] &&
		[ "$legs" = "$want" ]
}

# cntlr_full_ready is that comparison against a NAMED cntlr, for every step that
# knows which controller it means and would want a failover to fail the wait.
cntlr_full_ready() { # <cntlr id>
	if ! sp_totals_poll; then
		return 1
	fi
	cntlr_stack_matches "$1"
}

# ---------------------------------------------------------------------------
# primary_stack_ready — the same comparison against WHICHEVER cntlr is primary
# ---------------------------------------------------------------------------
#
# Every wait that watches a WHOLE STACK BEING BUILT needs this rather than
# cntlr_full_ready, and the reason is the react case. Its sp carries the reacting
# threshold set, primary_unhealthy 5, and the first real run's build moved the
# role TWICE while setup's own build wait was running (failover 1->2, then 2->1,
# both inside setup). A wait pinned to the cntlr that was primary when the wait
# started would then be watching a node that is now a standby: a standby builds
# no pools and no groups at all (CN12, CN13), so the poll could never pass and
# the wait would burn its whole WAIT_BUILD before dying with a message about a
# controller that is doing exactly what a standby should.
#
# TWO WAITS USE IT, and they want different things from a move. Setup's
# (setup_wait_stack) absorbs it: any of the sp's cntlrs may build the stack, so
# the wait follows the role and the caller re-reads the roles afterwards.
# react's stage 03 (react_new_primary_ready) cannot absorb it: its step 04 is
# written about the cntlr AR5 elected, so it wraps this and stops the run the
# moment the role leaves that cntlr. The pinned form, cntlr_full_ready, is for
# the steps that watch an INCREMENTAL convergence on a named controller under
# the quiet set, where nothing may move the role at all.
#
# So this follows the role and SAYS SO when it moves. It does not make a
# failover invisible — a move is logged with both ids and the progress line
# restarts under the new one — it only stops a legitimate failover from turning
# into a twenty-minute timeout. The cntlr the winning poll inspected is left in
# PRIMARY_WAIT_ID, and $CTL_OUT is its reply, so the assertions after the wait
# read the node that actually finished the build.
#
# THE THREE SHAPE GUARDS BELOW ARE INSURANCE, NOT A TRANSIENT. A reply with no
# primary, with two, or whose two parallel lists disagree sends the poll round
# again rather than failing — but none of those states is reachable through
# `sp get` today, and the comment must not claim the election passes through
# them. GetStoragePool answers out of one `Snapshot`, so the whole reply is a
# single store revision (gateway/storagepool.go:714-759); every writer of the
# `primary` flag leaves exactly one primary in that revision (`idx == 0` at
# create, gateway/storagepool.go:574; false at CreateCntlr, gateway/cntlr.go:271;
# the old cntlr's own flag at ReplaceCntlr, which deletes the old key in the same
# STM, model/ops.go:1516; and model.Failover flips both booleans in one STM,
# model/ops.go:1062-1066); and loadCntlrs walks cntlr_id_list and returns ABORTED
# on a missing key (gateway/alloc.go:496-511), so the two lists cannot come back
# different lengths. The guards are cheap, and they are what keeps a future
# non-atomic writer — or a reply this code could not otherwise tell from a valid
# one — from being read as a stack that is simply not finished yet.
PRIMARY_WAIT_ID=""

primary_stack_ready() {
	local n ids prim
	if ! sp_totals_poll; then
		return 1
	fi
	# $CTL_OUT is still the `sp get` sp_totals_poll just read.
	n=$(jq_of "$CTL_OUT" '.cntlr_list | length')
	ids=$(jq_of "$CTL_OUT" '.sp_conf.cntlr_id_list | length')
	case "$n$ids" in
	'' | *[!0-9]*) return 1 ;;
	esac
	# The same pairing sp_read_roles rests on: loadCntlrs reads the cntlrs in
	# sp_conf.cntlr_id_list order (gateway/alloc.go:496-511), so position i of
	# cntlr_list is the cntlr whose id is cntlr_id_list[i]. transpose would pad
	# the shorter list with nulls, so the lengths are checked first.
	[ "$n" = "$ids" ] || return 1
	prim=$(jq_of "$CTL_OUT" \
		'[.sp_conf.cntlr_id_list, [.cntlr_list[].primary]]
		 | transpose | map(select(.[1] == true) | .[0]) | join(",")')
	# One id and nothing else: "" is no primary and "3,7" is two, and both are
	# states to poll through rather than to judge.
	case "$prim" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$prim" != "$PRIMARY_WAIT_ID" ]; then
		if [ -n "$PRIMARY_WAIT_ID" ]; then
			log "!!! the primary role MOVED while this wait was running:" \
				"cntlr $PRIMARY_WAIT_ID -> cntlr $prim."
			log "!!! AR5 fired. The new primary builds the whole stack from" \
				"nothing (CN12/CN13: a standby had neither groups nor pools)," \
				"so the progress below starts again. In the $CASE case the sp" \
				"carries the $THR_SET threshold set ($THR)."
		fi
		PRIMARY_WAIT_ID=$prim
		STACK_LAST=""
	fi
	cntlr_stack_matches "$prim"
}

# cntlr_legs_match is the leg comparison itself, with no `sp get` of its own. It
# ASSUMES the SP_*_TOTAL globals were refreshed by the caller's own
# sp_totals_poll in this same poll, exactly as cntlr_stack_matches does, and it
# leaves that cntlr's inspect reply in $CTL_OUT.
cntlr_legs_match() { # <cntlr id>
	local legs want sig
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	legs=$(jq_of "$CTL_OUT" "$CNTLR_OK_LEGS")
	want=$((SP_LEG_TOTAL + SP_SPARE_TOTAL))
	sig="legs $legs/$want"
	if [ "$sig" != "$STACK_LAST" ]; then
		STACK_LAST=$sig
		log "  cntlr $1: $sig"
	fi
	[ "$legs" = "$want" ]
}

# cntlr_legs_full_ready is that comparison against a NAMED cntlr: a standby
# builds no groups and no pools at all (CN12, CN13), so its legs are the only
# rows to wait on. Unlike a stack target, a leg target does NOT become
# unreachable when the role moves — a promoted controller has every leg too — so
# a failover under one of these waits does not hang it; it only stops the wait
# from meaning what its caller meant, which is why react's step 04 tests the
# role itself before it asserts the emptiness rules.
cntlr_legs_full_ready() { # <cntlr id>
	if ! sp_totals_poll; then
		return 1
	fi
	cntlr_legs_match "$1"
}

# standby_shape_ready is the leg comparison against WHICHEVER cntlr is not the
# primary, plus the two EMPTINESS rules setup_assert_standby then asserts: CN12
# gives a standby no group devices and CN13 no pool, so grp_id_to_md_raid and
# slice_id_to_dm_pool are empty maps rather than maps of MISSING rows
# (setup_assert_standby's header has the four code shapes).
#
# THE EMPTINESS IS WHY IT WAITS AT ALL. A standby is not always a controller
# that was BORN one. If AR5 fired during this build — react's threshold set
# makes that likely — the standby is the DEMOTED old primary, and it tears its
# groups and pools down on its next syncup rather than at the instant of the
# demotion. Waiting on the leg count alone would reach setup_assert_standby
# while those rows were still there and fail a node that was doing the right
# thing a second too slowly.
#
# AND THE EMPTINESS IS ALSO WHY IT FOLLOWS THE ROLE, as primary_stack_ready
# does. Pinned to the id that was the standby when the wait began, a failover
# mid-wait would leave it insisting that the new PRIMARY hold no groups and no
# pools — a target that node spends the next 8m45s making less reachable, so the
# wait would spend its whole WAIT_BUILD and die about a controller doing exactly
# what a primary should. That is the same unsatisfiable-target shape the pinned
# stack wait had, one role over.
#
# It is setup's wait alone, and setup runs at CNTLR_CNT 2 — its caller asserts
# that before the wait — so "the cntlr that is not the primary" names exactly
# one controller. A reply that does not show exactly one primary AND exactly one
# non-primary is polled through, for the reason primary_stack_ready's header
# gives. The winning poll's id is left in STANDBY_WAIT_ID and $CTL_OUT is that
# cntlr's inspect reply, so the assertions after the wait read the node the
# predicate actually judged.
#
# STANDBY_SHAPE_LAST is its own scratch and NOT STACK_LAST, because this
# predicate also writes STACK_LAST through cntlr_legs_match: sharing one
# variable would make each of them see the other's signature as a change and log
# a line per poll, twice a second, for as long as the teardown took.
STANDBY_SHAPE_LAST=""
STANDBY_WAIT_ID=""

standby_shape_ready() {
	local roles prim stby grps pools sig
	if ! sp_totals_poll; then
		return 1
	fi
	# $CTL_OUT is still the `sp get` sp_totals_poll just read. The same pairing
	# sp_read_roles rests on: position i of cntlr_list is the cntlr whose id is
	# cntlr_id_list[i] (gateway/alloc.go:496-511). transpose would pad the
	# shorter list with nulls, so unequal lengths answer " " and both ids come
	# out empty.
	roles=$(jq_of "$CTL_OUT" \
		'if (.cntlr_list | length) != (.sp_conf.cntlr_id_list | length)
		 then " "
		 else [.sp_conf.cntlr_id_list, [.cntlr_list[].primary]] | transpose
		      | [(map(select(.[1] == true) | .[0]) | join(",")),
		         (map(select(.[1] != true) | .[0]) | join(","))]
		      | join(" ")
		 end')
	prim=${roles% *}
	stby=${roles#* }
	# One id each and nothing else: "" is none and "3,7" is two, and both are
	# states to poll through rather than to judge.
	case "$prim" in
	'' | *[!0-9]*) return 1 ;;
	esac
	case "$stby" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$stby" != "$STANDBY_WAIT_ID" ]; then
		if [ -n "$STANDBY_WAIT_ID" ]; then
			log "!!! the standby role MOVED while this wait was running:" \
				"cntlr $STANDBY_WAIT_ID -> cntlr $stby (the primary is now" \
				"cntlr $prim)."
			log "!!! AR5 fired. The new standby is the DEMOTED old primary," \
				"so it has a whole stack to tear down before it can look" \
				"like one (CN12/CN13), and the progress below starts again." \
				"In the $CASE case the sp carries the $THR_SET threshold" \
				"set ($THR)."
		fi
		STANDBY_WAIT_ID=$stby
		STACK_LAST=""
		STANDBY_SHAPE_LAST=""
	fi
	if ! cntlr_legs_match "$stby"; then
		return 1
	fi
	# $CTL_OUT is that cntlr's inspect reply; `// {}` because InspectCntlr
	# answers a NULL cntlr_info for a controller the agent does not know yet.
	grps=$(jq_of "$CTL_OUT" '(.cntlr_info.grp_id_to_md_raid // {}) | length')
	pools=$(jq_of "$CTL_OUT" '(.cntlr_info.slice_id_to_dm_pool // {}) | length')
	case "$grps$pools" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$grps" != 0 ] || [ "$pools" != 0 ]; then
		sig="$grps group(s) and $pools pool(s)"
		if [ "$sig" != "$STANDBY_SHAPE_LAST" ]; then
			STANDBY_SHAPE_LAST=$sig
			log "  cntlr $stby: legs ok, but still holding $sig — a demoted" \
				"primary tearing down what CN12/CN13 say a standby has" \
				"none of"
		fi
		return 1
	fi
	return 0
}

# cntlr_pos_of_addr finds a cntlr's position in the parallel CNTLR_* arrays by
# the CN address it runs on, so a step that created a controller can read back
# that controller's transport without assuming it is the one STANDBY_* names
# (with three cntlrs, STANDBY_POS is simply the first non-primary).
cntlr_pos_of_addr() { # <addr_port> → index | -1
	local i
	for i in "${!CNTLR_ADDRS[@]}"; do
		if [ "${CNTLR_ADDRS[$i]}" = "$1" ]; then
			printf '%s' "$i"
			return 0
		fi
	done
	printf -- '-1'
}

# host_path_gone is the negation host0 needs after a `cntlr delete`: the CN's
# agent removes the host-facing subsystem with the controller, and a subsystem
# that disappears under a live controller refuses the reconnect with DNR, so
# the kernel deletes the controller instead of retrying (memory note
# nvmet-port-unlink-dnr-kills-host-ctrl). path_field answers the word "none"
# when there is no such path, never "", which is why this compares text.
host_path_gone() { # <h> <nqn> <traddr>
	[ "$(host_path_state "$1" "$2" "$3")" = none ]
}

host_path_live() { # <h> <nqn> <traddr>
	[ "$(host_path_state "$1" "$2" "$3")" = live ]
}

# --- step 1 -----------------------------------------------------------------

ops_reads() {
	stage 01 "sp get, sp list, sp find-names"
	sp_refresh
	sp_read_roles
	assert_field "$SP_JSON" '.sp_name' "$SP" "\`sp get\` sp_name"
	assert_field "$SP_JSON" '.sp_conf.sp_id' "$SP_ID" \
		"\`sp get\` sp_conf.sp_id vs the CreateStoragePool reply"
	sp_totals

	ctl_ok sp list
	assert_jq "$CTL_OUT" "[.sp_name[]? | select(. == \"$SP\")] | length == 1" \
		"\`sp list\` names $SP exactly once"

	# FindStoragePoolNames answers a map keyed by the uint64 sp_id, and
	# protojson renders a uint64 MAP KEY as a QUOTED decimal string (F11,
	# pinned by ctl/render_test.go:108-117) — so the lookup is by the same
	# decimal text `sp create` handed back, not by a number.
	ctl_ok sp find-names --ids "$SP_ID"
	assert_field "$CTL_OUT" ".sp_id_to_name[\"$SP_ID\"]" "$SP" \
		"\`sp find-names --ids $SP_ID\` maps the id back to its name"
	assert_field "$CTL_OUT" '.sp_id_to_name | length' 1 \
		"find-names answered exactly the one id it was asked about"
}

# --- step 2 -----------------------------------------------------------------
#
# Two grows, on two different slices so that the meta ladder and the data rule
# are visible apart from each other:
#
#   --meta        appends a meta group of MetaLadderExtCnt(current total),
#                 i.e. the slice's CURRENT meta total, so 1 → 2 → 4 …
#                 (model/ops.go:276-306). --ext must be absent: the gateway
#                 refuses `--meta` with a non-zero ext_cnt outright
#                 (validateGrowExclusivity, gateway/validate.go:437-447).
#   --ext N       appends a DATA group — and N is NOT the size. The handler
#                 recomputes the size as the slice's FIRST data group's ext_cnt
#                 and says so (gateway/storagepool.go:1102-1104 "A data grow
#                 adds the slice's original allocation unit, not the caller's
#                 ext_cnt"); ext_cnt is only the exclusivity signal, and a zero
#                 is refused by the same validator.
#
# WHAT MUST NOT BE ASSERTED HERE: that every side of the sp is still on a
# DISTINCT DN. That is a CREATE property — CreateStoragePool grows its black
# list with every pick (F2) — and GrowSlice deliberately passes a `nil` black
# list and a nil ExcludeLocs (gateway/storagepool.go:1132-1139, with the D-F
# comment at :1122-1131), so a grown group MAY land on a DN that already carries
# another group's side. What does still hold, and is asserted, is the
# per-group rule: one scan keeps at most one candidate per location
# (model/alloc.go:137-141), so the LEGS legs of the new group are on LEGS
# different DN VMs.
ops_grow() {
	stage 02 "sp grow-slice: one meta grow and one data grow"
	local midx=0 didx=1 mslice dslice grp before_grps
	[ "$SLICE_CNT" -gt 1 ] || didx=0

	before_grps=$SP_GRP_TOTAL
	# The id comes from sp_conf.slice_id_list and NOT from slice_list: pb.Slice
	# carries slice_idx, meta_grp_list and data_grp_list and no id at all
	# (pb/schema.proto:405-409). loadSlices walks conf.GetSliceIdList() in list
	# order and appends (gateway/alloc.go:478-493), so slice_list[i] is the
	# slice whose id is slice_id_list[i] — the same pairing sp_read_roles
	# relies on for cntlrs, and setup pinned both lists to SLICE_CNT entries.
	# `--slice` takes an id, not an index (ctl/sp.go:422, "slice_id to grow").
	mslice=$(sp_field ".sp_conf.slice_id_list[$midx]")
	dslice=$(sp_field ".sp_conf.slice_id_list[$didx]")
	case "$mslice$dslice" in
	'' | *[!0-9]*) die "slice_id_list carried a non-decimal id" ;;
	esac

	ctl_ok sp grow-slice --slice "$mslice" --meta
	assert_field "$CTL_OUT" '.slice_id' "$mslice" "GrowSliceReply.slice_id (meta)"
	grp=$(jq_of "$CTL_OUT" '.grp_id')
	case "$grp" in
	'' | *[!0-9]* | 0) die "the meta grow returned grp_id '$grp'" ;;
	esac

	ctl_ok sp grow-slice --slice "$dslice" --ext "$INIT_EXT_CNT"
	assert_field "$CTL_OUT" '.slice_id' "$dslice" "GrowSliceReply.slice_id (data)"
	grp=$(jq_of "$CTL_OUT" '.grp_id')
	case "$grp" in
	'' | *[!0-9]* | 0) die "the data grow returned grp_id '$grp'" ;;
	esac

	sp_refresh
	assert_field "$SP_JSON" ".slice_list[$midx].meta_grp_list | length" 2 \
		"slice $midx now has two meta groups"
	assert_field "$SP_JSON" ".slice_list[$didx].data_grp_list | length" 2 \
		"slice $didx now has two data groups"
	# The ladder's own step: the first meta group is one extent, so the second
	# is one too and the total becomes two.
	assert_field "$SP_JSON" ".slice_list[$midx].meta_grp_list[1].ext_cnt" 1 \
		"the meta ladder appended currentTotal = 1 extent (uint64, a JSON string)"
	assert_field "$SP_JSON" ".slice_list[$didx].data_grp_list[1].ext_cnt" \
		"$INIT_EXT_CNT" \
		"the data grow appended the slice's first data group's ext_cnt, not --ext"
	assert_jq "$SP_JSON" \
		"[$SP_GRP_PATH | select((.leg_list | length) != $LEGS)] | length == 0" \
		"every group, grown ones included, still has $LEGS leg(s)"
	assert_jq "$SP_JSON" \
		"[$SP_GRP_PATH
		  | select(([.leg_list[] | .side_list[] | $SP_SIDE_VM]
		            | unique | length) != $LEGS)] | length == 0" \
		"the $LEGS leg(s) of every group are still on $LEGS different DN VMs"

	sp_totals
	assert_eq "$SP_GRP_TOTAL" "$((before_grps + 2))" \
		"two grows added exactly two groups"
	assert_eq "$SP_SIDE_TOTAL" "$(((before_grps + 2) * LEGS))" \
		"and exactly $LEGS side(s) per new group"

	# The new sides must zero before anything above them builds, and the
	# primary then has two more md groups and two wider pool concats.
	SIDES_LEFT=-1
	wait_until "$WAIT_PROVISION" \
		"the $((2 * LEGS)) new side(s) of the two grown groups to be provisioned" \
		sp_sides_provisioned
	SP_JSON=$CTL_OUT
	sp_totals
	stack_wait_reset
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to carry $SP_GRP_TOTAL groups and $SLICE_CNT pools" \
		cntlr_full_ready "$PRIMARY_CNTLR_ID"
	assert_field "$CTL_OUT" "$CNTLR_OK_POOLS" "$SLICE_CNT" \
		"every thin pool of the primary is RES_STATUS_OK after the grows"
	check_sha0 "after a meta grow and a data grow"
}

# --- step 3 -----------------------------------------------------------------
#
# D20. The two refusals are the point of the step, so their MESSAGES are
# asserted and not merely their code. Both come from the same handler, and the
# order of its two loops decides which one fires:
# UpdateStoragePoolCntlidSlotList checks every CNTLR first (:850-857) and only
# then every SIDE (:858-872, gateway/storagepool.go), so
#
#   --slots 1,2   drops slot 0, which cntlr <the first cntlr> uses
#   --slots 0,2   drops slot 1, which cntlr <the second cntlr> uses
#
# and the side loop is never reached in either case. §7.5's gloss on the first
# one ("slot 0 used by every side") names a fact that is TRUE — every side
# carries cntlid_slot_list[0], gateway/storagepool.go:526, which setup pins —
# but not the message that comes back. The id in each message is minted, so
# ctl_fail_grep and its fixed-string substring are what this needs;
# ctl_fail_msg cannot express it.
ops_slots() {
	stage 03 "sp set-cntlid-slots, then a third cntlr in the new slot"
	local spare spareaddr c3 pos traddr3

	# Everything below is written for §7.1's fixed two-slot list. Deriving the
	# three literals from $SLOTS would hide, not remove, that dependency.
	assert_eq "$SLOTS" "0,1" \
		"this step is written for the fixed --slots 0,1 of §7.1"

	ctl_ok sp set-cntlid-slots --slots 0,1,2
	sp_refresh
	assert_jq "$SP_JSON" '.sp_conf.cntlid_slot_list == [0, 1, 2]' \
		"cntlid_slot_list is the new list (uint32s, so bare numbers)"

	ctl_fail_grep INVALID_ARGUMENT "cntlid_slot_list drops slot 0, which cntlr" \
		sp set-cntlid-slots --slots 1,2
	ctl_fail_grep INVALID_ARGUMENT "cntlid_slot_list drops slot 1, which cntlr" \
		sp set-cntlid-slots --slots 0,2
	sp_refresh
	assert_jq "$SP_JSON" '.sp_conf.cntlid_slot_list == [0, 1, 2]' \
		"a refused set-cntlid-slots changed nothing"

	if [ "$SPARE_CN" -lt 0 ]; then
		log "  every --cn guest already carries a cntlr of $SP;" \
			"skipping the third-cntlr half of step 3 (it needs a spare CN)"
		check_sha0 "after set-cntlid-slots"
		return 0
	fi
	spare=$SPARE_CN
	spareaddr=$(cn_addr "$spare")

	ctl_ok cntlr create --slot 2 --cn-white "$spareaddr"
	c3=$(jq_of "$CTL_OUT" '.cntlr_id')
	case "$c3" in
	'' | *[!0-9]* | 0) die "cntlr create returned cntlr_id '$c3'" ;;
	esac
	sp_refresh
	sp_read_roles
	assert_field "$SP_JSON" '.cntlr_list | length' "$((CNTLR_CNT + 1))" \
		"the sp now has one more cntlr"
	assert_jq "$SP_JSON" \
		"[.cntlr_list[]
		  | select(.addr_port == \"$spareaddr\" and .cntlid_slot == 2)]
		 | length == 1" \
		"the third cntlr took cntlid_slot 2 on cn$spare ($spareaddr)"
	assert_jq "$SP_JSON" \
		"[.cntlr_list[] | select(.addr_port == \"$spareaddr\" and .primary)]
		 | length == 0" \
		"a cntlr is created as a standby, never primary (gateway/cntlr.go:263-270)"
	assert_jq "$SP_JSON" \
		"[.cntlr_list[] | select(.addr_port == \"$spareaddr\" and .disabled)]
		 | length == 0" \
		"and enabled from birth, which is what puts its CN in the CdcEntry"

	pos=$(cntlr_pos_of_addr "$spareaddr")
	assert_ne "$pos" -1 "the new cntlr's position in cntlr_list"
	traddr3=${CNTLR_TRADDRS[$pos]}

	sp_totals
	stack_wait_reset
	# WAIT_BUILD: a cntlr created now has nothing, so this is $SP_LEG_TOTAL
	# fresh nvme-tcp connections on a CN that held none — the standby half of
	# the measured build, not an incremental convergence.
	wait_until "$WAIT_BUILD" \
		"the third cntlr $c3 on cn$spare to connect every leg as a standby" \
		cntlr_legs_full_ready "$c3"
	assert_field "$CTL_OUT" '.cntlr_info | type' object \
		"the third cntlr's InspectCntlr reply carries a CntlrInfo"
	assert_field "$CTL_OUT" '.cntlr_info.grp_id_to_md_raid | length' 0 \
		"CN12: a standby has no group devices"
	assert_field "$CTL_OUT" '.cntlr_info.slice_id_to_dm_pool | length' 0 \
		"CN13: a standby has no thin pools"

	# The cdc must advertise the new transport before host0 can reach it, and
	# disc_want_of_sp derives the expectation from the cntlrs themselves — one
	# record per NON-disabled cntlr, which is what `ss create` wrote into the
	# CdcEntry (enabledCntlrTrConfs, gateway/subsystem.go:188-194).
	disc_want_of_sp "$SS0"
	DISC_LAST=""
	wait_until "$WAIT_HOST" \
		"the cdc to advertise all $((CNTLR_CNT + 1)) cntlr transports of $SS0 to host0" \
		host_disc_is 0
	# And the new cntlr's AGENT must have built the export before host0 tries
	# it. cntlr_legs_full_ready above says only that it connected its legs; the
	# subsystem and the namespace are the LAST rows a cntlr builds, and an
	# nvmet port with no subsystem linked to it does not listen (run 3's
	# failure — see wait_ns_exported). The other two cntlrs have been exporting
	# since setup, so the gate covers all three and returns at once for them.
	wait_ns_exported_all "$SS0" "$SS0_ID" "$NS1_ID"
	host_connect_all 0 "$SS0"
	# host0 already held controllers for $SS0 when that connect ran — the
	# "still live" assertion below says so — so connect_verdict's count cannot
	# catch a connect-all that added nothing HERE. This is the per-address
	# half of it, and it is what makes the wait below a wait for `live` rather
	# than a wait for a controller that was never made.
	connect_added_ctrl 0 "$SS0" "$traddr3"
	wait_until "$WAIT_HOST" "host0's third path, to cn$spare ($traddr3), to go live" \
		host_path_live 0 "$SS0" "$traddr3"
	assert_eq "$(host_path_state 0 "$SS0" "$PRIMARY_TRADDR")" live \
		"host0's path to the primary is still live"
	# A standby's namespaces are ANA-inaccessible: CN16 as amended by [D15]
	# gives AnaGrpIdOptimized only to a primary's (agent/cnagent/plan.go:784-791).
	host_wait_ana 0 "$SS0" "$traddr3" "$UUID1" inaccessible

	# DeleteCntlr refuses an enabled cntlr — "disabling is what triggers the
	# §10.4 re-election and takes the controller's namespaces
	# ANA-inaccessible, so requiring the disable first means a failover has
	# already happened by the time the record disappears"
	# (gateway/cntlr.go:300-350). That refusal is worth one call.
	ctl_fail_grep FAILED_PRECONDITION "is enabled; disable it first" \
		cntlr delete --id "$c3"

	ctl_ok cntlr set-enabled --id "$c3" --enabled=false
	sp_refresh
	sp_read_roles
	assert_jq "$SP_JSON" \
		"[.cntlr_list[] | select(.addr_port == \"$spareaddr\" and .disabled)]
		 | length == 1" \
		"the third cntlr is disabled"
	# A disabled cntlr's address leaves every CdcEntry at the same instant
	# (§8.8), so the discovery log host0 sees must shrink back to the two.
	disc_want_of_sp "$SS0"
	DISC_LAST=""
	wait_until "$WAIT_HOST" \
		"the cdc to drop the disabled cntlr's transport from $SS0's log" \
		host_disc_is 0

	ctl_ok cntlr delete --id "$c3"
	assert_field "$CTL_OUT" '.cntlr_id' "$c3" "DeleteCntlrReply.cntlr_id"
	sp_refresh
	sp_read_roles
	assert_field "$SP_JSON" '.cntlr_list | length' "$CNTLR_CNT" \
		"the sp is back to its $CNTLR_CNT cntlrs"
	wait_until "$WAIT_HOST" \
		"host0 to lose its path to the deleted cntlr on cn$spare ($traddr3)" \
		host_path_gone 0 "$SS0" "$traddr3"
	check_sha0 "after a third cntlr was created, disabled and deleted"
}

# --- step 4 -----------------------------------------------------------------
#
# The four "ask the agent, not etcd" reads. Each goes to a different agent and
# each proves a different thing, so all four are here even though three of them
# also run during setup: InspectSide reaches the DN that hosts one side,
# InspectDiskNode that same DN's node-wide state, InspectControllerNode the
# primary's CN, and InspectCntlr the primary cntlr itself.
ops_inspect() {
	stage 04 "sp inspect-side, dn inspect, cn inspect, cntlr inspect"
	local sid saddr zeroed total v k

	sid=$(sp_field "[$SP_SIDE_PATH] | .[0].side_id")
	saddr=$(sp_field "[$SP_SIDE_PATH] | .[0].addr_port")
	case "$sid" in
	'' | *[!0-9]* | 0) die "the first side of $SP has side_id '$sid'" ;;
	esac
	v=$(dn_vm_of_addr "$saddr")
	k=$(dn_inst_of_addr "$saddr")
	assert_ne "$v" none "side $sid's addr_port $saddr names one of the --dn guests"
	assert_ne "$k" none "side $sid's addr_port $saddr names a dn instance index"
	# Tell the failure dump which DN this step is about (§7.9).
	diag_note_dn "$v" "$k"

	ctl_ok sp inspect-side --id "$sid"
	assert_field "$CTL_OUT" '.side_info.side_dev_info.status' RES_STATUS_OK \
		"side $sid's data device on dn$v instance $k"
	zeroed=$(jq_of "$CTL_OUT" '.side_info.zeroed_ext_cnt')
	total=$(jq_of "$CTL_OUT" '.side_info.total_ext_cnt')
	case "$total" in
	'' | *[!0-9]* | 0) die "side $sid reports total_ext_cnt '$total'" ;;
	esac
	# pb/schema.proto:224 says it in the field's own comment: "always filled;
	# equal => fully zeroed". A side is exported only once it is, which is what
	# Side.provisioned then records.
	assert_eq "$zeroed" "$total" \
		"side $sid is fully zeroed (zeroed_ext_cnt == total_ext_cnt)"
	assert_ge "$(jq_of "$CTL_OUT" '.applied_revision')" 1 \
		"the DN agent has applied at least one SyncupSide revision"

	ctl_ok dn inspect --addr "$saddr"
	assert_field "$CTL_OUT" '.dn_info.disk_info.status' RES_STATUS_OK \
		"dn$v instance $k disk_info"
	assert_field "$CTL_OUT" '.dn_info.meta_info.status' RES_STATUS_OK \
		"dn$v instance $k meta_info"
	assert_field "$CTL_OUT" '.dn_info.port_info.status' RES_STATUS_OK \
		"dn$v instance $k port_info"
	assert_field "$CTL_OUT" '.dn_info.port_info.res_name' "$(dn_port_id "$k")" \
		"dn$v instance $k is still converging its own nvmet port id"

	ctl_ok cn inspect --addr "$PRIMARY_ADDR"
	assert_field "$CTL_OUT" '.cn_info.port_info.status' RES_STATUS_OK \
		"cn$PRIMARY_CN port_info"
	assert_field "$CTL_OUT" '.cn_info.tmpfs_info.status' RES_STATUS_OK \
		"cn$PRIMARY_CN tmpfs_info"
	assert_field "$CTL_OUT" '.cn_info.tmp_file_info.status' RES_STATUS_OK \
		"cn$PRIMARY_CN tmp_file_info"
	assert_field "$CTL_OUT" '.cn_info.loop_dev_info.status' RES_STATUS_OK \
		"cn$PRIMARY_CN loop_dev_info"

	ctl_ok cntlr inspect --id "$PRIMARY_CNTLR_ID"
	assert_field "$CTL_OUT" '.cntlr_info | type' object \
		"the primary's InspectCntlr reply carries a CntlrInfo"
	assert_field "$CTL_OUT" "$CNTLR_OK_POOLS" "$SLICE_CNT" \
		"the primary's $SLICE_CNT thin pools are RES_STATUS_OK"
	assert_field "$CTL_OUT" "$CNTLR_OK_GRPS" "$SP_GRP_TOTAL" \
		"the primary's $SP_GRP_TOTAL group devices are RES_STATUS_OK"
}

# --- step 5 -----------------------------------------------------------------
#
# D19: the whole §11.7 ladder down to DISABLE and back to READWRITE, asserting
# the DOCUMENTED shape at each rung rather than assuming one. The shape comes
# from doc/cnagent.md CN19's table and was re-derived from
# agent/cnagent/probe.go, which is the only place that decides what
# InspectCntlr reports:
#
#   the gates            agent/cnagent/plan.go:404-412
#     wantAny   = level <  DISABLE
#     wantLeg   =           level < NO_SIDE
#     wantGrp   = primary && level < NO_REDUND
#     wantPool  = primary && level < NO_THINPOOL
#     wantClone = primary && level < NO_CLONE
#   what a gate that is false produces, on the PRIMARY
#     legs       probe.go:50-53    Missing(details "sp_level")
#     groups     probe.go:63-77    Missing on the `case plan.primary` arm (:73-75)
#     pools and both concats
#                probe.go:84-94    Missing, inside `if plan.primary`
#     td raid0   probe.go:161-180  Missing on the `case plan.primary` arm (:177-179)
#     td dm-error
#                probe.go:145-160  Missing on the `else` of `if plan.wantAny`,
#                                  so DISABLE only — and its own comment says
#                                  the row is reported MISSING rather than left
#                                  out, because "omitted" and "never looked at"
#                                  must not be the same document
#     subsystems, namespaces, ns-devs and xfers
#                probe.go:264-271 → syncup_cntlr.go:779-806 reportSuppressed,
#                                   reached only at DISABLE (!wantAny)
#
# So a suppressed row is PRESENT and RES_STATUS_MISSING with details
# "sp_level" — CN19's own words — and never an absent row. (An absent row is
# what a STANDBY produces for groups and pools, which is a different thing and
# is why setup_assert_standby asserts emptiness there and this asserts
# MISSING here.)
#
# READONLY is the one rung with no CntlrInfo signature at all: CN16 rule 7
# reloads each user-facing ns-dev onto a dm-flakey `error_writes` table over
# its NORMAL backing, which probeNsDev compares as the expected table, so every
# row stays OK. What changes is only what the HOST sees, so that is what the
# rung asserts.
#
# Two consequences of the DISABLE rung that the steps below depend on:
#  - the host-facing subsystem goes with everything else, and a subsystem that
#    disappears under a live controller kills that controller with DNR, so
#    host0 must CONNECT AGAIN on the way back up. It is not a reconnect the
#    kernel can make by itself.
#  - none of this is a health event: worker/health.go's cntlrObservation
#    (:463-501) reacts to RES_STATUS_ERROR rows only, so a ladder full of
#    MISSING rows never makes the primary look unhealthy and no re-election
#    happens under it.
LEVEL_WANT=()
LEVEL_LAST=""

ops_level_want() { # <level, without the SP_LEVEL_ prefix>
	local legs=RES_STATUS_OK grps=RES_STATUS_OK pools=RES_STATUS_OK
	local raid0=RES_STATUS_OK base=RES_STATUS_OK
	case "$1" in
	READWRITE | READONLY | NO_CLONE) ;;
	NO_THINPOOL)
		pools=RES_STATUS_MISSING
		raid0=RES_STATUS_MISSING
		;;
	NO_REDUND | NO_MIGRATION)
		pools=RES_STATUS_MISSING
		raid0=RES_STATUS_MISSING
		grps=RES_STATUS_MISSING
		;;
	NO_SIDE)
		pools=RES_STATUS_MISSING
		raid0=RES_STATUS_MISSING
		grps=RES_STATUS_MISSING
		legs=RES_STATUS_MISSING
		;;
	DISABLE)
		pools=RES_STATUS_MISSING
		raid0=RES_STATUS_MISSING
		grps=RES_STATUS_MISSING
		legs=RES_STATUS_MISSING
		base=RES_STATUS_MISSING
		;;
	*) die "ops_level_want: unknown sp_level '$1'" ;;
	esac
	# `base` is the four rows that survive every rung but DISABLE: CN19's
	# NO_SIDE row says "host-facing and xfer subsystems remain, error-backed",
	# and the per-td dm-error is what they are backed BY.
	LEVEL_WANT=(
		"leg_id_to_leg:$legs"
		"grp_id_to_md_raid:$grps"
		"slice_id_to_dm_pool:$pools"
		"slice_id_to_meta:$pools"
		"slice_id_to_data:$pools"
		"td_id_to_raid0:$raid0"
		"td_id_to_dm_error:$base"
		"ss_id_to_subsystem:$base"
		"ns_id_to_namespace:$base"
		"ns_id_to_dm_linear:$base"
	)
}

# cntlr_level_ready is true when every row of every map LEVEL_WANT names carries
# the status that level demands. A map with no rows at all is NOT ready: an
# InspectCntlr answered before the CN has accepted its first SyncupCntlr comes
# back with a null cntlr_info, and `null | length` is 0 in jq — the one shape in
# which "the level has been applied" and "the agent has never heard of this
# controller" would look identical.
cntlr_level_ready() { # <cntlr id>
	local spec map want total hit sig="" ok=1
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	for spec in "${LEVEL_WANT[@]}"; do
		map=${spec%%:*}
		want=${spec#*:}
		total=$(jq_of "$CTL_OUT" "(.cntlr_info.$map // {}) | length")
		hit=$(jq_of "$CTL_OUT" \
			"[(.cntlr_info.$map // {})[] | select(.status == \"$want\")] | length")
		sig="$sig $map=$hit/$total"
		case "$total" in
		'' | *[!0-9]* | 0) ok=0 ;;
		*) [ "$hit" = "$total" ] || ok=0 ;;
		esac
	done
	if [ "$sig" != "$LEVEL_LAST" ]; then
		LEVEL_LAST=$sig
		log "  cntlr $1:$sig"
	fi
	[ "$ok" = 1 ]
}

ops_set_level() { # <level, without the SP_LEVEL_ prefix>
	local lvl=$1 spec map want total
	log ""
	log "  --- sp_level $lvl"
	ctl_ok sp set-level --level "$lvl"
	sp_refresh
	assert_field "$SP_JSON" '.sp_conf.sp_level' "SP_LEVEL_$lvl" \
		"the stored sp_level after \`sp set-level --level $lvl\`"
	ops_level_want "$lvl"
	LEVEL_LAST=""
	# WAIT_BUILD, not WAIT_PROVISION: this ladder walks all the way down to
	# DISABLE and back, and the rungs around it are not incremental at all.
	# DISABLE suppresses every resource CN19 names, so the CN tears the whole
	# stack down; the step back up rebuilds all $SLICE_CNT pools and every md
	# array from nothing, which is the same piece of work setup pays for, at the
	# 8m45s / 126,657-spawn scale of the WAIT_BUILD comment. One budget for every rung,
	# because the cheap rungs return on their first poll and cost nothing.
	wait_until "$WAIT_BUILD" \
		"the primary cntlr $PRIMARY_CNTLR_ID to reach CN19's shape for SP_LEVEL_$lvl" \
		cntlr_level_ready "$PRIMARY_CNTLR_ID"
	# The predicate ran in this shell, so its last reply is still in $CTL_OUT.
	for spec in "${LEVEL_WANT[@]}"; do
		map=${spec%%:*}
		want=${spec#*:}
		[ "$want" = RES_STATUS_MISSING ] || continue
		total=$(jq_of "$CTL_OUT" "(.cntlr_info.$map // {}) | length")
		assert_field "$CTL_OUT" \
			"[(.cntlr_info.$map // {})[] | select(.details == \"sp_level\")] | length" \
			"$total" \
			"CN19: every suppressed row of $map reports details \"sp_level\" at SP_LEVEL_$lvl"
	done
}

ops_levels() {
	stage 05 "sp set-level: the whole §11.7 ladder down to DISABLE and back"
	local dev got out lvl
	dev=$(host_dev "$UUID1")
	sp_refresh
	sp_read_roles
	sp_totals

	ops_set_level READONLY
	# CN19's READONLY row: reads are served and writes error, enforced on the
	# CN only, by the dm-flakey table over the ns-dev's NORMAL backing
	# (architecture.md §11.7, [D11]). So the data must still be readable — and
	# the read goes through host_sha_probe rather than host_sha_range because
	# if this kernel's flakey target wedged the read instead of serving it,
	# `timeout` could not bound it and the run would hang for ever; the probe
	# answers `blocked` inside its budget and the assertion names what it got.
	#
	# The cache drop before it is host IO too, and it is safe here for the one
	# reason rule 5 cares about: nothing is suspended at READONLY. `sync` has
	# nothing dirty to flush — the last write was setup's, followed by its own
	# sync — so it cannot turn into the write this level is supposed to fail.
	host_drop_caches 0 || die "host0: dropping caches at SP_LEVEL_READONLY failed"
	got=$(host_sha_probe 0 "$dev" "$BASELINE_MIB" 0) ||
		die "host0: the sha probe verb failed at SP_LEVEL_READONLY"
	assert_eq "$got" "$SHA0" \
		"host0 still READS its data at SP_LEVEL_READONLY"

	# AND THE OTHER HALF. Reads being served is not what distinguishes
	# READONLY from READWRITE — a regression that simply stopped reloading the
	# dm-flakey table would pass the assertion above, and no other rung of this
	# ladder covers it either: cntlr_level_ready has no CntlrInfo signature at
	# READONLY (the note at the top of this step), so the host is the only
	# place the level is visible at all.
	#
	# The write is /dev/zero and not PATTERN0 on purpose: it must be bytes the
	# device does NOT already hold, or "the media did not change" would prove
	# nothing. One MiB at offset 0 is inside the SHA0 window, so a write that
	# reached the media moves the digest and the assertion below names it.
	#
	# dd's own status is REPORTED and not asserted, the way step 10's
	# connect-all rc is: the failing write is buffered into host0's page cache
	# and only fsync can see the error, so the exit code depends on uutils dd
	# 0.8.0 propagating a conv=fsync failure — which this lab has never
	# measured. What IS asserted is the media, which is the property CN19
	# states.
	#
	# The second cache drop's `sync` DOES have something dirty to flush, unlike
	# the one above it, and that is the point: it is what forces the refused
	# write out of host0's page cache so the re-read below is answered by the
	# CN and not by the bytes dd just put in memory. Rule 5 is untouched —
	# nothing is suspended at READONLY, and dm-flakey `error_writes` fails the
	# bio rather than requeueing it, so no task can end up in D state.
	out=$(host_write_probe 0 /dev/zero "$dev" 1 0) ||
		die "host0: the write probe verb failed at SP_LEVEL_READONLY"
	printf '%s\n' "$out" >&2
	case "$out" in
	*rc=0*)
		log "  WARNING: dd reported a SUCCESSFUL 1 MiB write to $dev at" \
			"SP_LEVEL_READONLY; the media check below is what decides"
		;;
	*)
		log "  the 1 MiB write to $dev at SP_LEVEL_READONLY failed, as CN19" \
			"says it must ($out)"
		;;
	esac
	host_drop_caches 0 ||
		die "host0: dropping caches after the refused write failed"
	got=$(host_sha_probe 0 "$dev" "$BASELINE_MIB" 0) ||
		die "host0: the sha probe verb failed after the refused write"
	assert_eq "$got" "$SHA0" \
		"the refused write changed nothing on the media (CN19: reads served, writes error)"

	for lvl in NO_CLONE NO_THINPOOL NO_REDUND NO_MIGRATION NO_SIDE DISABLE; do
		ops_set_level "$lvl"
	done
	for lvl in NO_SIDE NO_MIGRATION NO_REDUND NO_THINPOOL NO_CLONE READONLY READWRITE; do
		ops_set_level "$lvl"
	done

	# Back at READWRITE the whole stack is OK again (the last ops_set_level
	# waited for exactly that, and cntlr_level_ready's LEVEL_WANT includes
	# ss_id_to_subsystem and ns_id_to_namespace). What is left is the host,
	# which lost its controller when DISABLE removed the subsystem under it.
	sp_refresh
	sp_read_roles
	disc_want_of_sp "$SS0"
	DISC_LAST=""
	wait_until "$WAIT_HOST" \
		"the cdc to serve $SS0 to host0 after the ladder" \
		host_disc_is 0
	# ops_set_level's wait covers the PRIMARY only. The ladder is sp-scoped, so
	# the standby tore its export down and rebuilt it too, and connect-all
	# connects its transport as well — so the export gate runs over every
	# non-disabled cntlr here as everywhere else. It costs one `cntlr inspect`
	# per cntlr and returns on the first poll for the primary.
	#
	# WAIT_BUILD AND NOT THE DEFAULT WAIT_PROVISION, because for the STANDBY
	# this gate is the only wait in the step and what it is waiting for is a
	# FROM-NOTHING rebuild, not an incremental convergence: DISABLE suppressed
	# every resource CN19 names on both cntlrs, and build() is sequential with
	# legs first and the subsystem among the last rows
	# (agent/cnagent/syncup_cntlr.go:435-438, then the subsystem loop), so the
	# standby's ss_id_to_subsystem row cannot go RES_STATUS_OK until all
	# $SP_LEG_TOTAL legs have been reconnected. That is the WAIT_BUILD comment's
	# piece of work, and it is the same budget ops_set_level's own wait spends
	# on the primary one line up. The primary returns on the first poll here,
	# so the wider budget costs nothing when nothing is wrong.
	wait_ns_exported_all "$SS0" "$SS0_ID" "$NS1_ID" "$WAIT_BUILD"
	host_connect_all 0 "$SS0"
	# ANA first, device second: a namespace whose only path has never been
	# usable gets no head disk at all (section 2's rule, measured in this lab).
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" optimized
	wait_dev 0 "$UUID1"
	check_sha0 "after the whole sp_level ladder down to DISABLE and back"
}

# --- step 6 -----------------------------------------------------------------
#
# A snapshot, the two bitmap reads and a second thin device.
#
# `td create --ori <origin> --size 0` is the ONE case in which a zero size is
# legal: the pre-STM check refuses `size == 0 && ori_name == ""`
# (gateway/thindevice.go:88-92) and the STM then inherits the origin's size and
# re-checks it against the same slice_cnt x stripe_size unit (:143-171).
#
# `td get-bm --cnt 0` asks for the WHOLE slice — dnvctl substitutes no window
# of its own (ctl/td.go:169-170, CT8). The reply is not a proto message but the
# §3.1 hex map `{"bitmap_hex","byte_cnt"}`, and byte_cnt is a Go `int`
# (ctl/root.go:400-405), so it renders as a BARE NUMBER while every uint64 in
# this file is a quoted string.
ops_snapshot() {
	stage 06 "td create --ori (snapshot), td get-bm, td get-leg-bm, a second td"
	local snapid hex bytes legid t0dev

	ctl_ok td create --name "$SNAP0" --ori "$TD0" --size 0
	snapid=$(jq_of "$CTL_OUT" '.td_id')
	case "$snapid" in
	'' | *[!0-9]* | 0) die "td create --ori returned td_id '$snapid'" ;;
	esac
	wait_until "$WAIT_PROVISION" "$SNAP0 to report created" td_created "$SNAP0"
	assert_field "$CTL_OUT" '.name_to_td | length' 2 \
		"the sp holds $TD0 and its snapshot $SNAP0"
	assert_field "$CTL_OUT" ".name_to_td.$SNAP0.size" "$TD0_SIZE" \
		"a snapshot inherits its origin's size"
	t0dev=$(jq_of "$CTL_OUT" ".name_to_td.$TD0.dev_id")
	assert_field "$CTL_OUT" ".name_to_td.$SNAP0.ori_id" "$t0dev" \
		"$SNAP0's ori_id is $TD0's dev_id (uint32s, so bare numbers)"

	ctl_ok td get-bm --name "$TD0" --slice-idx 0 --start 0 --cnt 0
	bytes=$(jq_of "$CTL_OUT" '.byte_cnt')
	assert_jq "$CTL_OUT" '(.byte_cnt | type) == "number"' \
		"byte_cnt is a Go int and renders as a bare number, not a quoted uint64"
	assert_ge "$bytes" 1 "td get-bm returned a non-empty bitmap for slice 0"
	hex=$(jq_of "$CTL_OUT" '.bitmap_hex')
	assert_eq "${#hex}" "$((bytes * 2))" \
		"bitmap_hex is exactly two hex digits per byte_cnt byte"
	# THE WIRE CONVENTION IS INVERTED, and asserting "some bit is set" would
	# assert the opposite of what this step is for. architecture.md:2097 and
	# agent/cnagent/thinbm.go:277-279 both say bit k = 1 iff block start+k is
	# UNMAPPED: thinDeviceBitmap starts from allUnmapped (every real bit 1,
	# :286) and CLEARS the range of every mapped extent (:291). A td nobody
	# ever wrote therefore answers all-ones, and "some bit set" would pass on
	# it — and a slice that happened to be FULLY written would answer all
	# zeros and fail it.
	#
	# What setup's write pins is block 0: dm-striped maps chunk c of a td to
	# slice c mod slice_cnt, so the first STRIPE_SIZE bytes host0 wrote at
	# offset 0 are block 0 of slice 0's thin volume, whatever SLICE_CNT is.
	# Bits are LSB-first within a byte (clearBitRange, thinbm.go:233-235:
	# `bitmap[lo/8] &^= 1 << (lo % 8)`), so block 0 is bit 0 of byte 0, i.e.
	# the low bit of the first two hex digits. bitmap_hex is at least two
	# digits here — the two assertions above pin byte_cnt >= 1 and the length.
	assert_eq "$((0x${hex:0:2} & 1))" 0 \
		"block 0 of slice 0 of $TD0 is MAPPED (host0 wrote $BASELINE_MIB MiB at offset 0; on the wire 1 = unmapped)"

	if [ "$REDUND" = raid1 ]; then
		legid=$(sp_field "[.slice_list[0].data_grp_list[] | .leg_list[]] | .[0].leg_id")
		case "$legid" in
		'' | *[!0-9]* | 0) die "slice 0's first data leg has leg_id '$legid'" ;;
		esac
		ctl_ok td get-leg-bm --leg "$legid" --start 0 --cnt 0
		assert_jq "$CTL_OUT" '(.byte_cnt | type) == "number"' \
			"td get-leg-bm renders the same §3.1 hex map"
		assert_jq "$CTL_OUT" '(.bitmap_hex | type) == "string"' \
			"td get-leg-bm's bitmap_hex"
		# `>= 1`, not `>= 0`: byte_cnt is len() of a Go slice, so `>= 0` holds
		# for every possible reply including an empty one and could never
		# fail. It really is non-empty here — `--cnt 0` means "the whole leg"
		# (ctl/td.go:208) and bitmapWindow turns that into gp.dataBlocks
		# (agent/cnagent/bitmapread.go:141-142, :181-183), which for a DATA
		# group of INIT_EXT_CNT x $EXTENT_SIZE at a 1 MiB pool block is 64
		# blocks, 8 bytes. The leg is a data leg: it came out of
		# data_grp_list, and a META leg would answer all-zero instead
		# (bitmapread.go:146-153).
		bytes=$(jq_of "$CTL_OUT" '.byte_cnt')
		assert_ge "$bytes" 1 "leg $legid's bitmap byte_cnt"
		hex=$(jq_of "$CTL_OUT" '.bitmap_hex')
		assert_eq "${#hex}" "$((bytes * 2))" \
			"td get-leg-bm's bitmap_hex is two hex digits per byte_cnt byte"
	else
		log "  --redund none: a group has one leg and no md bitmap;" \
			"skipping td get-leg-bm"
	fi

	# The second thin device is deliberately $TD0's size and not §7.5's one
	# TD_UNIT. Step 7 repoints a LIVE namespace at it, and nvmet fixes a
	# namespace's capacity when it enables it: agent.NsConf carries nqn, nsid,
	# device_path, uuid, nguid and ana_grpid and NO size (agent/nvmet.go:387-394),
	# and nothing here writes the kernel's revalidate_size. A td of another size
	# would therefore leave the host's reported capacity stale — a separate
	# mechanism, and not what step 7 is about. Equal sizes make
	# UpdateNamespaceDev exactly what its handler says it is: "invisible to the
	# host" (gateway/subsystem.go:513-521).
	ctl_ok td create --name "$TD1" --size "$TD0_SIZE"
	wait_until "$WAIT_PROVISION" "$TD1 to report created" td_created "$TD1"
	assert_field "$CTL_OUT" '.name_to_td | length' 3 \
		"the sp holds $TD0, $SNAP0 and $TD1"
	# `created` is defined on the thin VOLUMES alone (ThinDeviceCreated.md R13);
	# the raid0 CN15 builds one step later is a different resource, and it is the
	# one step 7's `ns set-dev --td $TD1` repoints the namespace's dm-linear AT
	# (CN16 rule 6). Waiting for it here is what keeps step 7 deterministic.
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to carry a raid0 for all three thin devices" \
		cntlr_raid0_ready "$PRIMARY_CNTLR_ID" 3
	check_sha0 "after a snapshot of $TD0 and a second thin device"
}

# --- step 7 -----------------------------------------------------------------
#
# The two namespace mutators, and the one trap in this file that costs a run:
# NO HOST IO BETWEEN THE SUSPEND AND THE RESUME. A suspended namespace is
# parked on the td's dm-error and moved to the inaccessible ANA group, so its
# requeued bios wedge anything that touches it — including host_drop_caches,
# which issues a `sync`. The assertion that it happened is therefore an ANA
# read and never a dd, and never wait_dev_gone either: a park keeps the device
# (agent/cnagent/plan.go:307-315 "a **park**, not a dm suspension … and the
# device stays live"), so wait_dev_gone would burn its whole budget and die.
ops_namespace() {
	stage 07 "ns set-suspended (a park, not a removal), then ns set-dev"
	local dev zero

	dev=$(host_dev "$UUID1")

	ctl_ok ns set-suspended --nqn "$SS0" --idx 1 --suspended
	assert_field "$CTL_OUT" '.ns_id' "$NS1_ID" "UpdateNamespaceSuspendedReply.ns_id"
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" inaccessible
	if host_dev_present 0 "$UUID1"; then
		log "  $dev is still there while the namespace is parked, as [D12] says"
	else
		die "host0 lost $dev while the namespace was merely SUSPENDED." \
			"A suspend is a park — the cn agent keeps the nvmet namespace and" \
			"points the ns-dev at the td's dm-error — so the head disk must stay"
	fi
	ctl_ok ns set-suspended --nqn "$SS0" --idx 1 --suspended=false
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" optimized
	check_sha0 "after a suspend/resume round trip"

	# ns set-dev. $TD1 is a fresh thin device, so every block of it is
	# unprovisioned and reads as zeros — which is a digest this suite can
	# compute rather than merely assert to be "different": 4 MiB of /dev/zero
	# read on host0 itself.
	zero=$(host_sha_range 0 /dev/zero "$BASELINE_MIB") ||
		die "host0: digesting $BASELINE_MIB MiB of /dev/zero failed"
	assert_eq "${#zero}" 64 "the all-zero reference digest is 64 hex digits"
	assert_ne "$zero" "$SHA0" \
		"the all-zero digest differs from SHA0 (setup's pattern is urandom)"

	ctl_ok ns set-dev --nqn "$SS0" --idx 1 --td "$TD1"
	host_wait_sha 0 "$dev" "$BASELINE_MIB" "$zero" "$WAIT_HOST" \
		"host0 to read $TD1's unprovisioned zeros through the same namespace"
	ctl_ok ns set-dev --nqn "$SS0" --idx 1 --td "$TD0"
	check_sha0 "after the namespace was repointed back at $TD0"
}

# --- step 8 -----------------------------------------------------------------
#
# `disabled` on a node is a SCHEDULING flag and nothing else: it decides whether
# the allocator may see the node, i.e. whether its §5.6 capacity key exists,
# and it is invisible to the agent — sides and cntlrs the node already hosts
# keep running (gateway/disknode.go:372-383). So the way to prove it took
# effect is to ask the allocator for that node by name and be refused.
#
# The refusal is RESOURCE_EXHAUSTED because the scan found too few candidates:
# pickDns draws plan.Legs of them and errExhausted's message is
# "<what>: %d disk nodes with %d free extents, need %d"
# (gateway/alloc.go:98-102), with `what` = "grow slice"
# (gateway/storagepool.go:1139). The white list is
# exactly LEGS free DNs on LEGS DIFFERENT VMs, one of them the disabled one:
# a scan keeps at most one candidate per location (model/alloc.go:137-141), so
# LEGS DNs on LEGS locations would yield LEGS candidates and SUCCEED if the
# disabled one were still visible, and yields LEGS-1 and fails because it is
# not. One DN fewer, or two on one VM, and the step would prove nothing.
OPS_FREE=()
ops_pick_free_dns() {
	local used v k addr hit
	used=$(sp_field "[$SP_SIDE_PATH | .addr_port] | .[]")
	OPS_FREE=()
	for v in "${!DN[@]}"; do
		for ((k = 0; k < DNS_PER_VM; k++)); do
			addr=$(dn_addr "$v" "$k")
			# grep -c, never grep -q: an early close plus `set -o pipefail`
			# turns a SIGPIPE on the writer into an abort.
			hit=$(printf '%s\n' "$used" | grep -cxF -- "$addr" || true)
			[ "$hit" = 0 ] || continue
			OPS_FREE+=("$addr")
			break
		done
	done
}

ops_disabled() {
	stage 08 "dn set-disabled and cn set-disabled, each proven by a refusal"
	local victim white gslice spare spareaddr v k before_grps before_cntlrs

	sp_refresh
	sp_read_roles
	sp_totals
	before_grps=$SP_GRP_TOTAL
	before_cntlrs=$(sp_field '.cntlr_list | length')

	ops_pick_free_dns
	[ "${#OPS_FREE[@]}" -ge "$LEGS" ] ||
		die "step 8 needs $LEGS disk nodes on $LEGS different VMs that carry no" \
			"side of $SP, and only ${#OPS_FREE[@]} VM(s) have one free." \
			"Raise --dns-per-vm or add a --dn guest"
	victim=${OPS_FREE[0]}
	white=$(
		IFS=,
		printf '%s' "${OPS_FREE[*]:0:$LEGS}"
	)
	v=$(dn_vm_of_addr "$victim")
	k=$(dn_inst_of_addr "$victim")
	assert_ne "$v" none "the victim DN $victim names one of the --dn guests"
	assert_ne "$k" none "the victim DN $victim names a dn instance index"
	diag_note_dn "$v" "$k"
	log "  disabling dn$v instance $k ($victim); white list for the grow: $white"

	# A slice to aim the refused grow at. Any slice will do — the call must
	# fail before it changes anything — so this picks one the earlier steps did
	# not grow, where there is one.
	# Again from sp_conf.slice_id_list: pb.Slice has no slice_id field (step 2's
	# comment has the citations).
	gslice=$(sp_field ".sp_conf.slice_id_list[$((SLICE_CNT > 2 ? 2 : SLICE_CNT - 1))]")
	case "$gslice" in
	'' | *[!0-9]*) die "the slice chosen for the refused grow has id '$gslice'" ;;
	esac

	ctl_ok dn set-disabled --addr "$victim" --disabled
	ctl_ok dn get --addr "$victim"
	assert_field "$CTL_OUT" '.dn_conf.disabled' true \
		"dn$v instance $k is stored disabled"
	ctl_fail_grep RESOURCE_EXHAUSTED "disk nodes with" \
		sp grow-slice --slice "$gslice" --ext "$INIT_EXT_CNT" --dn-white "$white"
	ctl_ok dn set-disabled --addr "$victim" --disabled=false
	ctl_ok dn get --addr "$victim"
	assert_field "$CTL_OUT" '.dn_conf.disabled' false \
		"dn$v instance $k is stored enabled again"

	# The CN mirror. The spare CN carries no cntlr of this sp, so disabling it
	# takes nothing out of service, and `cntlr create` white-listing it is then
	# the one allocation that can only be answered by that node. pickCn draws
	# exactly one candidate and answers "<op>: no controller node with %d free
	# extents" when it cannot (gateway/alloc.go:137-141).
	if [ "$SPARE_CN" -lt 0 ]; then
		log "  every --cn guest carries a cntlr of $SP; skipping the cn half of step 8"
	else
		spare=$SPARE_CN
		spareaddr=$(cn_addr "$spare")
		ctl_ok cn set-disabled --addr "$spareaddr" --disabled
		ctl_ok cn get --addr "$spareaddr"
		assert_field "$CTL_OUT" '.cn_conf.disabled' true \
			"cn$spare is stored disabled"
		ctl_fail_grep RESOURCE_EXHAUSTED "no controller node" \
			cntlr create --slot 2 --cn-white "$spareaddr"
		ctl_ok cn set-disabled --addr "$spareaddr" --disabled=false
		ctl_ok cn get --addr "$spareaddr"
		assert_field "$CTL_OUT" '.cn_conf.disabled' false \
			"cn$spare is stored enabled again"
	fi

	# Neither refusal may have changed the sp: a RESOURCE_EXHAUSTED from
	# pickDns/pickCn happens before the transaction, so nothing was written.
	sp_refresh
	sp_totals
	assert_eq "$SP_GRP_TOTAL" "$before_grps" \
		"the refused grow-slice added no group"
	assert_field "$SP_JSON" '.cntlr_list | length' "$before_cntlrs" \
		"the refused cntlr create added no controller"
}

# --- step 9 -----------------------------------------------------------------

ops_delete_tds() {
	stage 09 "td delete $SNAP0 and $TD1, leaving only $TD0 for the teardown"
	# The snapshot goes first. Not because the gateway demands that order — a
	# CREATED snapshot does not block its origin; only an uncreated one does
	# (tdUncreatedSnapshots, gateway/thindevice.go:255-261) — but because
	# deleting the origin of a live snapshot is the copy case's business, not
	# this one's.
	ctl_ok td delete --name "$SNAP0"
	ctl_ok td delete --name "$TD1"
	ctl_ok td list
	assert_field "$CTL_OUT" '.name_to_td | length' 1 \
		"only $TD0 is left for the teardown"
	assert_jq "$CTL_OUT" ".name_to_td | has(\"$TD0\")" "and it is $TD0"
	check_sha0 "after the two extra thin devices were deleted"
}

case_ops() {
	CASE=ops
	ops_reads
	ops_grow
	ops_slots
	ops_inspect
	ops_levels
	ops_snapshot
	ops_namespace
	ops_disabled
	ops_delete_tds
	case_finish
}

# ---------------------------------------------------------------------------
# Case: copy (§7.5) — transfer, clone, migration and spare
# ---------------------------------------------------------------------------
#
# The four RPC groups that move bytes between objects rather than creating
# them: `xfer` (§8.10), `clone` (§8.9), `migr` (§8.11) and `spare` (§8.12).
# Every one of them is exercised against the sp setup built, in the order the
# dependencies force: the transfer exports $TD0, the clone copies that export
# into a second thin device, the transfer is then aborted, and the last two
# steps move one leg's side between disk nodes.
#
# ===========================================================================
# THE THREE DEVIATIONS FROM §7.5's LITERAL WORDING, AND WHY EACH IS FORCED
# ===========================================================================
#
#  a. §7.5 copy step 1 says "no host0 IO from here until step 4" and step 2
#     then asks for a host0 `sha_range` while the origin namespace is still
#     parked. Those two sentences cannot both be obeyed. The trap is real and
#     recorded (memory note ana-inaccessible-ns-no-blockdev, rule 5 of the
#     header): host_drop_caches issues a `sync`, and every read helper in this
#     file drops caches first. So this case does ALL of its host0 IO after the
#     transfer is deleted, and the clone's data is proved there. It costs
#     nothing: the destination thin device keeps the bytes whether or not the
#     dm-clone is still above it.
#     What is gained is a STRICTLY STRONGER assertion. Reading $TD_CLONE while
#     the clone still exists reads through `CnCloneFinalName` (CN16 rule 5),
#     which serves an unhydrated region FROM THE SOURCE — so a clone that
#     copied nothing would still answer correctly. Reading it after the clone
#     is gone reads `CnRaid0Name` over $TD_CLONE's own thin volumes (CN16
#     rule 6), which only holds the bytes hydration actually wrote.
#
#  b. §7.5 copy step 1 says host1 "`discover`s" the transfer and runs
#     `connect-all`. It cannot. Every CdcEntry key is keyed on a SUBSYSTEM's
#     ss_id and is written only where a subsystem is (gateway/subsystem.go:190,
#     :248, :344; model/ops.go:1608-1632 rewrites the entries of nqn_list), a
#     Transfer has no ss_id and is in no nqn_list, and
#     `grep -rn 'Xfer\|xfer' cdc/*.go gateway/subsystem.go` is empty. ctl/xfer.go
#     says the same in the command's own doc comment ("the xfer subsystem is
#     reached directly and is never advertised through a CdcEntry"). host1
#     therefore connects DIRECTLY to the primary cntlr's transport with
#     `-n $XNQN`, through the helper's `connect` verb.
#     host1 and not host0 consumes it for a second reason worth writing down:
#     CN17 gives the transfer's namespace the ORIGIN namespace's `uuid` and
#     `nguid`, so $XNQN's ns 1 and $SS0's ns 1 are two namespaces of two
#     subsystems carrying one identity. They are never put on one kernel here.
#
#  c. §7.5 copy step 5's `spare switch` and `spare delete` are written without
#     `--grp`. ctl/spare.go declares `--grp` on ALL THREE leaves (create:
#     --grp; delete: --grp --leg; switch: --grp --spare --target), because
#     CreateSpareLegRequest/DeleteSpareLegRequest/SwitchSpareLegRequest each
#     carry grp_id and the handlers locate the slice from it
#     (openGrpForSpareLeg). The flags below are the file's, not the plan's.
#
# ===========================================================================
# D21 AND ITS FALLBACK, WHICH IS A REAL BRANCH AND NOT A COMMENT
# ===========================================================================
# D21 makes the clone SOURCE a transfer of the SAME sp: the primary CN opens
# an nvme-tcp connection to its own nvmet port and reads $TD0's raid0 back
# through it. Nothing in the tree refuses that — CreateClone's only check on
# `src_nqn` is validateNqn's format test (gateway/clone.go:111) and its STM
# checks four things, none of them about the source's location
# (gateway/clone.go:124-186) — so it is legal by construction. Whether one
# kernel being both initiator and target for the same bytes WORKS is a
# property of the lab's kernel, not of this tree, so the suite does not assume
# it.
#
# Two conditions, either of which means "this source is unusable", each read
# from the agent's OWN report rather than inferred by the suite:
#
#   1. the connect never came up. `clone_id_to_target` is the CN's verdict on
#      the source subsystem: Ok with the path states when a live controller is
#      there, Missing when the subsystem is not, Err with the connect error
#      otherwise (agent/cnagent/probe.go:221-234, agent/cnagent/clone.go:60-71).
#      A row that is still not RES_STATUS_OK $WAIT_SRC_CONNECT seconds after
#      the clone was created is the "connect refused" case.
#   2. hydration froze. The dm-clone's `details` is the raw `dmsetup status`
#      line (agent/cnagent/clone.go:455-472), whose seventh field is
#      `<hydrated>/<total>` — the same field the gateway parses in
#      hydrationComplete (gateway/common.go:916-949). A same-kernel loopback
#      that deadlocks under writeback pressure HANGS rather than failing, so a
#      status line that has not moved for $WAIT_HYDRATE_STALL seconds while
#      the device itself is RES_STATUS_OK is the second trigger. Without it
#      the case would burn its whole budget and never reach the fallback.
#
# What the fallback builds is §7.5's own: a second storage pool $SP_SRC with
# one slice, `--redund none` and ONE cntlr pinned to a CN that is NOT sp0's
# primary, carrying its own thin device, subsystem and namespace, which host1
# writes a pattern into. The clone then reads it over a real network hop.
# THE SUITE REFUSES TO GUESS: a fallback that also fails is a die naming both
# faults, never a third attempt.
#
# The fallback needs two more disk nodes than sp0 took, and they are there
# without changing --dns-per-vm: every DN is a $BACKING_SIZE loop device and a
# side of sp0 costs it $INIT_EXT_CNT extents of $EXTENT_SIZE, so a DN that
# already carries one still reports free_ext_cnt > 0 (`dn get`), and
# CreateStoragePool's black list is per-request — it excludes the DNs THIS
# create picked, not the DNs another sp uses (gateway/storagepool.go:393-403).
# §7.5's "add +2 to DNS_PER_VM in that branch" is therefore unnecessary here;
# what would actually fail is a create with no candidate at all, and that
# comes back as RESOURCE_EXHAUSTED from `sp create`, through ctl_ok, as a die.
#
# ===========================================================================
# WHAT IS NOT ASSERTED, DELIBERATELY
# ===========================================================================
#  * That `clone append-bm` made the copy skip anything. The fold counts an
#    absent chunk, a short chunk and a slice with no chunk at all as WRITTEN
#    (CN22, architecture.md §11.4 step 2: "the safe direction, which can only
#    cost an extra copy"), so one chunk of one slice is legal and correct but
#    its effect on the region set is not something `cntlr inspect` reports.
#    What IS asserted is that the RPC is accepted and echoes the clone id.
#  * That a grown sp still puts every side on a distinct DN. GrowSlice passes
#    a nil black list (gateway/storagepool.go:1132-1139) — but nothing in this
#    case grows the sp, so the create-time property still holds and the
#    per-group rule is what the migration and spare steps check.
# ---------------------------------------------------------------------------

# --- the objects this case creates ------------------------------------------
#
# $TD_CLONE must be a bare jq identifier for the same reason $TD0 is: `td
# list` is keyed by td_name and every filter reaches a row as
# `.name_to_td.<name>`. c0, m0, m1, x0 and k0 are §7.5's own names; ops used
# s0 and t1 and they died with its sp.
XFER0=x0
CLONE0=k0
TD_CLONE=c0
MIGR0=m0
MIGR1=m1

# The fallback source pool's names. $SP_SRC is a second sp_name inside the
# SAME cluster, so it needs no cluster of its own; $TD_SRC may repeat $TD0's
# name because a thin device key is (cluster, sp, name)
# — model.ThinDeviceKey — and the two sps have different sp_ids.
SP_SRC=sp1
SS_SRC="$NQN_IT:ss1"
TD_SRC=t0

# The third fixed v4 uuid, for the fallback source's namespace. It must differ
# from $UUID1 and $UUID2: host1 holds the transfer's namespace (which carries
# $UUID1, deviation (b)) at the same time as this one.
UUID3=2b6f0cc9-04d2-4f1a-9c3e-1d0a5e7b8c03

# --- budgets ----------------------------------------------------------------
#
# These three live here rather than in the constants block at the top because
# this file is appended to section by section and that block is closed; they
# are WAIT_* like every other budget and every one of them bounds a
# wait_until, never a sleep.
#
# WAIT_HYDRATE — the whole copy of a $TD0_SIZE destination. At the default
#   shape that is 128 MiB pulled over one nvme-tcp connection into 32 thin
#   pools. It is set to WAIT_PROVISION's number and not to its NAME, so that
#   the two can be argued about separately: a hydration is bounded by the
#   nvme-tcp path and by dm-clone's own copy threads, not by the dmsetup/mdadm
#   spawn rate that decides a build. The 300 s it carried before was sized
#   against the old 300 s WAIT_PROVISION; that budget doubled on the 8m45s
#   window the first run measured, and this one follows because the copy runs on the SAME 2-vCPU
#   CN and competes with the same work.
# WAIT_SRC_CONNECT — how long the CN may take to bring the source connection
#   up before "it never will" is the honest reading. The CN retries a failed
#   source connect from its own registry (agent/cnagent/clone.go:66
#   startConnectRetry), so this must be several retries wide, not one.
# WAIT_HYDRATE_STALL — how long a LIVE dm-clone's status line may stand still
#   before the copy is called frozen. It is deliberately smaller than
#   WAIT_HYDRATE: the stall is what routes to the fallback, and a stall
#   detector that fires only at the budget would never route anywhere.
WAIT_HYDRATE=600
WAIT_SRC_CONNECT=60
WAIT_HYDRATE_STALL=90

# --- state this case fills --------------------------------------------------

# The transfer and its computed subsystem NQN (F12: XferNqn carries cluster,
# sp and xfer, in that order — common/name_fmt.go:575-588).
XFER0_ID=""
XNQN=""

# The clone, its destination thin device and the namespace that exports it.
CLONE0_ID=""
TD_CLONE_ID=""
NS2_ID=""

# Every CN's CnHostNqn, as the comma list `xfer set-hosts --hosts` takes, plus
# the primary's own — which is the one the clone's connect will present
# (agent/cnagent/plan.go:921-923 hostNqn() = CnHostNqn(cluster, cn_id)).
CN_HOST_NQNS=""
PRIMARY_CN_NQN=""
PRIMARY_CN_ID=""

# The sp's stored geometry, read back rather than assumed: --src-stripe and
# --src-block describe the SOURCE, and for the D21 branch the source IS this
# sp's raid0 over its own thin pools.
SP_STRIPE_SIZE=""
SP_BLOCK_SIZE=""

# The clone source, whichever branch filled it. COPY_SRC_SHA is the digest the
# destination must read back over its first $BASELINE_MIB MiB; COPY_SRC_SP and
# COPY_SRC_TD name the sp and thin device whose slice-0 bitmap is appended.
COPY_SRC_BRANCH=""
COPY_SRC_NQN=""
COPY_SRC_IDX=1
COPY_SRC_SLICES=""
COPY_SRC_STRIPE=""
COPY_SRC_BLOCK=""
COPY_SRC_TRADDR=""
COPY_SRC_TRSVCID=""
COPY_SRC_SHA=""
COPY_SRC_SP=""
COPY_SRC_TD=""

# The fallback pool, and the flag that decides whether it must be torn down.
COPY_SRC_SP_BUILT=0
SRC_SP_ID=""
SRC_CNTLR_ID=""
SRC_TRADDR=""
SRC_TRSVCID=""

# Scratch for the hydration predicate. COPY_SRC_FAULT is the whole protocol
# between copy_clone_progress and copy_clone_build: empty means "hydrated",
# non-empty means "this source is unusable, and here is the sentence that says
# why". Both are written ONLY by that predicate and its caller.
COPY_SRC_FAULT=""
COPY_HYD_SIG=""
COPY_HYD_MARK=0
COPY_SRC_START=0

# The two migrations, and the "last observation" scratch of the three
# predicates below. Each of those is logged from inside its predicate when the
# value CHANGES and is never interpolated into a wait_until label: a label is
# expanded once, at the call, so it can only carry what was there before the
# first poll.
MIGR0_ID=""
MIGR1_ID=""
MIGR_HYD_LAST=""
MD_STATE_LAST=""
SRC_STACK_LAST=""

# The group every placement step in this case works on: slice 0's first (and,
# since nothing here grows a slice, only) data group. Written as a fragment so
# no filter spells the walk twice, exactly as $SP_GRP_PATH is.
COPY_GRP0='.slice_list[0].data_grp_list[0]'

# ---------------------------------------------------------------------------
# dm-clone status, the one number both hydration proofs rest on
# ---------------------------------------------------------------------------
#
# `dmsetup status <dm-clone>` prints
#
#   <start> <len> clone <meta block size> <used>/<total> <region size>
#   <hydrated>/<total> <hydrating> <#feature args> …
#
# and the agents put that line verbatim into a ResInfo's details (§9.5) — the
# CN for a clone (agent/cnagent/clone.go:455-472), the DN for a migration's
# destination side. The gateway reads field 7 of it, counting from <start> as
# field 1, in hydrationComplete (gateway/common.go:932-949), and refuses both
# `clone delete` and `migr finish` unless it parses and says done >= total.
# These two helpers read the same field, so a wait that succeeds here is a
# wait after which those two RPCs cannot answer FAILED_PRECONDITION for the
# reason this case cares about.
#
# awk and not `set --`: the details of a multi-target device is several lines,
# and the gateway walks them all looking for the `clone` one.

hyd_fraction() { # <raw dmsetup status details> → "<hydrated>/<total>" | none
	local out
	out=$(printf '%s\n' "$1" |
		awk '$3 == "clone" && NF >= 7 { print $7; exit }')
	printf '%s' "${out:-none}"
}

# hyd_complete is hydrationComplete's own rule, including its two refusals: a
# value that does not parse and a total of zero are both "not complete",
# because neither is PROOF that the copy finished.
hyd_complete() { # <"<hydrated>/<total>">
	local hyd tot
	case "$1" in
	*/*) ;;
	*) return 1 ;;
	esac
	hyd=${1%%/*}
	tot=${1##*/}
	case "$hyd$tot" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$tot" -gt 0 ] || return 1
	[ "$hyd" -ge "$tot" ]
}

# ---------------------------------------------------------------------------
# Predicates
# ---------------------------------------------------------------------------

# The two "gone" predicates, both the same shape as sp_gone: NOT_FOUND is the
# only answer that counts, because an UNAVAILABLE from a gateway that died
# mid-drain must keep the poll going rather than read as a finished teardown.
clone_gone() {
	if ctl_try clone get --name "$CLONE0"; then
		return 1
	fi
	case "$CTL_ERR" in
	"dnvctl: NOT_FOUND: "*) return 0 ;;
	esac
	return 1
}

src_sp_gone() {
	if ctl_try --sp "$SP_SRC" sp get; then
		return 1
	fi
	case "$CTL_ERR" in
	"dnvctl: NOT_FOUND: "*) return 0 ;;
	esac
	return 1
}

# copy_xfer_host_linked is the observable that `xfer set-hosts` has REACHED the
# CN, as opposed to having been written to etcd. nvmet's allowed_hosts is a
# directory of symlinks, one per permitted host nqn
# (agent/nvmet.go:296-322 links them under
# <subsys>/allowed_hosts/<hostnqn>), so its presence is the kernel's own
# answer. Waiting for it is what keeps the clone's first connect attempt from
# being refused and pushed into the CN's retry registry — which would still
# heal, but only after a retry interval this case would then have to budget
# for.
#
# One line, read-only, root-only: that is the shape section 4 allows through
# ssh_cn_ok directly. The _ok form is required — a `test -e` that is false
# exits 1 and must not end the run.
copy_xfer_host_linked() { # <cn v> <hostnqn>
	local out
	out=$(ssh_cn_ok "$1" \
		"test -e $NVMET/subsystems/$XNQN/allowed_hosts/$2 && echo linked || echo missing")
	[ "$out" = linked ]
}

# copy_clone_progress is the hydration wait AND the fallback's trigger, in one
# predicate, because wait_until needs a single answer and the two outcomes are
# both "stop waiting".
#
#   returns 0 with COPY_SRC_FAULT empty      the copy is complete
#   returns 0 with COPY_SRC_FAULT set        the source is unusable, and the
#                                            string says which of the two ways
#   returns 1                                keep polling
#
# Reading the two rows separately is deliberate. `clone_id_to_target` is the
# SOURCE CONNECTION's row and `clone_id_to_dm_clone` the DEVICE's, and CN18's
# step-1 failure branch sets the first to Err and the second to MISSING
# "source not connected" (agent/cnagent/clone.go:60-71) — so a source fault
# and a device fault are distinguishable, and only the first one routes to the
# fallback. The stall test is additionally gated on the device being
# RES_STATUS_OK: a dm-clone that was never built is somebody else's problem
# (the metadata arena, a missing raid0), and building a second storage pool
# would not fix it. That case simply runs out $WAIT_HYDRATE and dies with the
# last signature in the transcript.
copy_clone_progress() { # <cntlr id> <clone id>
	local tgt tgtdet dm det frac sig
	COPY_SRC_FAULT=""
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	tgt=$(jq_of "$CTL_OUT" \
		".cntlr_info.clone_id_to_target[\"$2\"].status // \"absent\"")
	tgtdet=$(jq_of "$CTL_OUT" \
		".cntlr_info.clone_id_to_target[\"$2\"].details // \"\"")
	dm=$(jq_of "$CTL_OUT" \
		".cntlr_info.clone_id_to_dm_clone[\"$2\"].status // \"absent\"")
	det=$(jq_of "$CTL_OUT" \
		".cntlr_info.clone_id_to_dm_clone[\"$2\"].details // \"\"")
	frac=$(hyd_fraction "$det")
	sig="target=$tgt dm_clone=$dm hydrated=$frac"
	# The mark moves only when the SIGNATURE moves, which is what makes this a
	# no-progress detector and not a timer: a status line that keeps printing
	# the same fraction is exactly the frozen copy the fact check warns about.
	if [ "$sig" != "$COPY_HYD_SIG" ]; then
		COPY_HYD_SIG=$sig
		COPY_HYD_MARK=$SECONDS
		log "  clone $2 on cntlr $1: $sig"
	fi
	if hyd_complete "$frac"; then
		return 0
	fi
	if [ "$tgt" != RES_STATUS_OK ] &&
		[ "$((SECONDS - COPY_SRC_START))" -ge "$WAIT_SRC_CONNECT" ]; then
		COPY_SRC_FAULT="the primary's clone_id_to_target row is still $tgt"
		COPY_SRC_FAULT="$COPY_SRC_FAULT ${WAIT_SRC_CONNECT}s after the clone"
		COPY_SRC_FAULT="$COPY_SRC_FAULT was created (details: $tgtdet)"
		return 0
	fi
	if [ "$dm" = RES_STATUS_OK ] &&
		[ "$((SECONDS - COPY_HYD_MARK))" -ge "$WAIT_HYDRATE_STALL" ]; then
		COPY_SRC_FAULT="the dm-clone is RES_STATUS_OK but its hydration has"
		COPY_SRC_FAULT="$COPY_SRC_FAULT not moved from $frac for ${WAIT_HYDRATE_STALL}s"
		return 0
	fi
	return 1
}

# side_hydrated is the migration's twin of the above, one layer down: the
# destination SIDE's dm-clone lives on its DN, so the row is InspectSide's
# migr_dst_info.dm_clone_info (pb/schema.proto:213-216) and not a CntlrInfo
# map. It is exactly what FinishMigration's own proof reads — a GetSideInfo of
# the DESTINATION side, then hydrationComplete on
# migr_dst_info.dm_clone_info.details (gateway/migration.go:461-480) — so a
# `migr finish` issued after this wait cannot be refused for lack of proof.
#
# Until the destination side has finished zeroing there is no dm-clone at all
# and the row reports PROVISIONING or nothing; both keep the poll going.
side_hydrated() { # <side id>
	local st det frac sig
	if ! ctl_try sp inspect-side --id "$1"; then
		return 1
	fi
	st=$(jq_of "$CTL_OUT" \
		'.side_info.migr_dst_info.dm_clone_info.status // "absent"')
	det=$(jq_of "$CTL_OUT" \
		'.side_info.migr_dst_info.dm_clone_info.details // ""')
	frac=$(hyd_fraction "$det")
	sig="dm_clone=$st hydrated=$frac"
	if [ "$sig" != "$MIGR_HYD_LAST" ]; then
		MIGR_HYD_LAST=$sig
		log "  migration destination side $1: $sig"
	fi
	[ "$st" = RES_STATUS_OK ] || return 1
	hyd_complete "$frac"
}

# grp_md_clean is "md has finished rebuilding onto the promoted spare".
#
# WHAT THE ROW ACTUALLY CARRIES: probeGroup returns mdadm's `State:` line
# verbatim as the details of grp_id_to_md_raid, and RES_STATUS_ERROR only when
# that line contains "inactive" (agent/cnagent/md.go:371-394 for probeGroup,
# :55 for the `State:` value it reports). So a rebuilding array is
# RES_STATUS_OK with a details of "clean, degraded, recovering" — the status
# alone proves nothing about the resync, which is why this reads the words.
#
# The three words below are mdadm's own: `degraded` while a member is missing
# or not yet in sync, `recovering` while it is being rebuilt, `resyncing`
# while the array re-reads itself. Absence of all three plus RES_STATUS_OK is
# the array md prints when it is whole.
#
# The final non-empty test is not decoration either: an EMPTY details means
# the `State:` line was not found in mdadm's output at all, and "we could not
# read the state" must never pass for "the rebuild finished". It is only
# reachable under raid1, which is the only arm that calls this.
#
# THE REVISION ARGUMENT IS WHAT MAKES THIS A WAIT AND NOT A COIN FLIP.
# `spare switch` changes WHICH legs the group has, not HOW MANY, so in the
# window between the RPC returning and the CN converging the array is still
# the old, whole pair and reports "clean". A bare md-state test would pass
# there and prove nothing. InspectCntlr's applied_revision is the agent's last
# fully applied SyncupCntlr revision (gateway/cntlr.go:536-546 copies the
# agent's own GetCntlrInfo reply revision), and the worker builds that request
# with the SP's current SpRev (worker/sprole.go:736, :930, carried into the
# cntlr child at :1032-1035). So "applied_revision >= the SpRev the switch
# bumped to" is the proof that the array being read is the NEW one.
grp_md_clean() { # <cntlr id> <grp id> <min applied revision>
	local st det rev sig
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	rev=$(jq_of "$CTL_OUT" '.applied_revision')
	st=$(jq_of "$CTL_OUT" ".cntlr_info.grp_id_to_md_raid[\"$2\"].status // \"absent\"")
	det=$(jq_of "$CTL_OUT" ".cntlr_info.grp_id_to_md_raid[\"$2\"].details // \"\"")
	sig="applied_revision $rev (want >= $3), md $st [$det]"
	if [ "$sig" != "$MD_STATE_LAST" ]; then
		MD_STATE_LAST=$sig
		log "  group $2 on cntlr $1: $sig"
	fi
	case "$rev" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$rev" -ge "$3" ] || return 1
	[ "$st" = RES_STATUS_OK ] || return 1
	case "$det" in
	*degraded* | *recovering* | *resyncing*) return 1 ;;
	esac
	[ -n "$det" ]
}

# --- the fallback pool's own predicates -------------------------------------
#
# Four near-twins of setup's, taking the sp name as an argument, because every
# one of setup's reads $SP through ctl_prefix. `--sp` is a persistent string
# flag of the root command, so a second occurrence in the argv simply wins
# (verified against the built binary: the last --trace-id is the one that
# reaches the trace, and spOf/traceId read through the same viper binding).
# That is the whole mechanism by which this case addresses a second sp.

src_sides_provisioned() { # <sp name>
	local left
	if ! ctl_try --sp "$1" sp get; then
		return 1
	fi
	left=$(jq_of "$CTL_OUT" "$SP_UNPROV_CNT")
	case "$left" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$left" = 0 ]
}

src_td_created() { # <sp name> <td name>
	if ! ctl_try --sp "$1" td list; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" ".name_to_td.$2.created")" = true ]
}

# src_cntlr_ready is cntlr_full_ready for a pool whose shape is known from its
# arguments rather than from the SP_* globals (which describe sp0 and must not
# be clobbered while this case is mid-flight).
#
# IT IS THE ONE SHAPE TARGET IN THIS FILE THAT IS STILL A CONSTANT, and that is
# safe here rather than an oversight. $SP_SRC is built once by
# copy_src_sp_build, with one slice, --redund none and ONE cntlr, and nothing in
# this file grows it, spares it or fails it over:
#
#   * a spare needs raid1 — CreateSpareLeg and AR8 both refuse a RedundNone
#     group ("there is no redundancy to repair", gateway/spareleg.go:194-198;
#     reason=redund_none, worker/reaction.go:1174-1183), so no spare_leg_list
#     entry can appear and the leg count cannot move;
#   * AR5 needs a failover candidate and this pool has exactly one cntlr;
#   * AR6 would need its pool over the low water mark, and this pool holds one
#     copy of $TD0_SIZE with nothing else written into it;
#   * and it carries the QUIET threshold set anyway, because the copy case does.
#
# A shape that cannot move may be compared against a constant. sp0's can move,
# which is why it is not.
src_cntlr_ready() { # <sp name> <cntlr id> <pools> <grps> <legs>
	local pools grps legs sig
	if ! ctl_try --sp "$1" cntlr inspect --id "$2"; then
		return 1
	fi
	pools=$(jq_of "$CTL_OUT" "$CNTLR_OK_POOLS")
	grps=$(jq_of "$CTL_OUT" "$CNTLR_OK_GRPS")
	legs=$(jq_of "$CTL_OUT" "$CNTLR_OK_LEGS")
	sig="pools $pools/$3, groups $grps/$4, legs $legs/$5"
	if [ "$sig" != "$SRC_STACK_LAST" ]; then
		SRC_STACK_LAST=$sig
		log "  $1 cntlr $2: $sig"
	fi
	[ "$pools" = "$3" ] && [ "$grps" = "$4" ] && [ "$legs" = "$5" ]
}

src_raid0_ready() { # <sp name> <cntlr id> <count>
	if ! ctl_try --sp "$1" cntlr inspect --id "$2"; then
		return 1
	fi
	[ "$(jq_of "$CTL_OUT" "$CNTLR_OK_RAID0")" = "$3" ]
}

# ---------------------------------------------------------------------------
# The CN host NQNs (F12)
# ---------------------------------------------------------------------------
#
# `xfer set-hosts --hosts` on a transfer carries the DESTINATION cntlrs' host
# NQNs, and a cntlr's CN presents CnHostNqn(cluster_id, cn_id) on every
# connection it makes (agent/cnagent/plan.go:921-923). The cn_id is minted by
# the gateway, so it is read back from `cn get`, never computed.
#
# All $CN_CNT of them go in, not just the primary's: the list is what an
# nvmet subsystem's allowed_hosts becomes, and a failover between now and the
# clone's connect would otherwise lock the new primary out.
copy_cn_host_nqns() {
	local v addr id nqn list=""
	PRIMARY_CN_NQN=""
	for v in "${!CN[@]}"; do
		addr=$(cn_addr "$v")
		ctl_ok cn get --addr "$addr"
		id=$(jq_of "$CTL_OUT" '.cn_conf.cn_id')
		case "$id" in
		'' | *[!0-9]* | 0) die "cn$v ($addr) reports cn_id '$id'" ;;
		esac
		nqn=$(cn_host_nqn "$id")
		case "$nqn" in
		notanid-*) die "cn_host_nqn could not format cn_id '$id'" ;;
		esac
		if [ -z "$list" ]; then
			list=$nqn
		else
			list="$list,$nqn"
		fi
		if [ "$v" = "$PRIMARY_CN" ]; then
			PRIMARY_CN_NQN=$nqn
			PRIMARY_CN_ID=$id
		fi
		log "  cn$v ($addr) cn_id $id host nqn $nqn"
	done
	CN_HOST_NQNS=$list
	[ -n "$PRIMARY_CN_NQN" ] ||
		die "the primary cn$PRIMARY_CN is not one of the --cn guests"
}

# copy_primary_cn_id re-reads just the primary's cn_id. It exists as its own
# call because SideToCnNqn is keyed on (cluster, sp, LEG, cn) — a stale cn_id
# would name a subsystem that CN does not export, and the wait built on it
# would time out against a name nothing serves.
copy_primary_cn_id() {
	ctl_ok cn get --addr "$PRIMARY_ADDR"
	PRIMARY_CN_ID=$(jq_of "$CTL_OUT" '.cn_conf.cn_id')
	case "$PRIMARY_CN_ID" in
	'' | *[!0-9]* | 0)
		die "cn$PRIMARY_CN ($PRIMARY_ADDR) reports cn_id '$PRIMARY_CN_ID'"
		;;
	esac
}

# ---------------------------------------------------------------------------
# §7.5 copy step 1 — the transfer
# ---------------------------------------------------------------------------
#
# `xfer create --auto-suspend` does two things in one RPC: it exports $TD0's
# raid0 under $XNQN from every cntlr (CN17), and it makes the ORIGIN namespace
# effectively suspended without writing `suspended` on it — the xfer_list
# entry itself is what CN16's effective-suspend rule reads. So host0's ns 1
# goes ANA-inaccessible and its ns-dev is parked on $TD0's dm-error, while the
# transfer's own namespace serves the same bytes to host1.
#
# The park is NOT a removal, so the assertion is an ANA read plus
# host_dev_present, never wait_dev_gone (section 2's rule; wait_dev_gone would
# burn its budget and die).
copy_transfer() {
	stage 01 "xfer create --auto-suspend, then host1 reads $TD0 through it"
	local dev

	# PRIMARY_* were filled by setup, but a case that starts from whatever the
	# last one left behind is how a failover test asserts nothing: re-read the
	# roles here so every transport below is this case's own observation.
	sp_refresh
	sp_read_roles

	ctl_ok xfer create --name "$XFER0" --ori-nqn "$SS0" --ori-idx 1 \
		--hosts "${HOST_NQN[1]}" --auto-suspend
	XFER0_ID=$(jq_of "$CTL_OUT" '.xfer_id')
	case "$XFER0_ID" in
	'' | *[!0-9]* | 0) die "xfer create returned xfer_id '$XFER0_ID'" ;;
	esac

	ctl_ok xfer get --name "$XFER0"
	assert_field "$CTL_OUT" '.xfer.xfer_id' "$XFER0_ID" \
		"GetTransfer's xfer_id vs the CreateTransfer reply"
	assert_field "$CTL_OUT" '.xfer.ori_nqn' "$SS0" "the transfer's origin subsystem"
	assert_field "$CTL_OUT" '.xfer.ori_ns_idx' 1 \
		"the transfer's origin ns_idx (uint32, a bare number)"
	assert_field "$CTL_OUT" '.xfer.auto_suspend' true \
		"auto_suspend was stored"
	assert_jq "$CTL_OUT" \
		"(.xfer.allowed_hosts) == [\"${HOST_NQN[1]}\"]" \
		"allowed_hosts is host1's own hostnqn and nothing else yet"
	sp_refresh
	assert_jq "$SP_JSON" \
		"(.sp_conf.xfer_name_list) == [\"$XFER0\"]" \
		"xfer_name_list names $XFER0"

	# F12: XferNqn(cluster, sp, xfer) — three ids, in that order, and the
	# cluster one is implicit. A `notanid-` in the result would mean the id
	# handed to hex16 was not decimal, which is the caller's bug, not the
	# gateway's (section 2).
	XNQN=$(xfer_nqn "$SP_ID" "$XFER0_ID")
	case "$XNQN" in
	*notanid-*) die "xfer_nqn produced '$XNQN' for sp $SP_ID xfer $XFER0_ID" ;;
	esac
	log "  transfer $XFER0 is xfer_id $XFER0_ID, subsystem $XNQN"

	# The origin retires. THE PARK: the head disk stays, the ANA state moves.
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" inaccessible
	if host_dev_present 0 "$UUID1"; then
		log "  host0 keeps $(host_dev "$UUID1") while the origin is parked," \
			"as CN16 rule 1 says (the ns-dev is reloaded onto the td's" \
			"dm-error and the device stays live)"
	else
		die "host0 lost $(host_dev "$UUID1") when the transfer suspended the" \
			"origin namespace. An effective suspend is a PARK — the nvmet" \
			"namespace and the ns-dev both stay — so the head disk must stay too"
	fi
	log "  NO host0 IO from here until the transfer is deleted (rule 5):" \
		"a parked ns-dev requeues, and host_drop_caches issues a sync"

	# AND THE PARK IS NOT THE GATE. It is tempting to read the ANA move above
	# as proof that the agent has converged the transfer, and it is not: the
	# park is done by the RETIRE phase and the transfer's subsystem and
	# namespace by the BUILD phase that follows it (convergeCntlr is "one
	# retire phase top-down, then one build phase bottom-up",
	# agent/cnagent/syncup_cntlr.go:94-95; doc/cnagent.md :1231-1233 puts the
	# park in the retire phase by name). So host0's ns 1 can be inaccessible
	# while $XNQN does not exist yet, and a connect issued then would not
	# produce a controller. What that failure would look like is deliberately
	# not asserted here: the silent rc=0 measured in this lab was
	# `nvme connect-all`'s and not this site's plain `nvme connect`, and the
	# primary's one nvmet port is already listening for $SS0, so it need not
	# even be ECONNREFUSED. The gate below stands on its own without a claim
	# about nvme-cli. CN17's rows are keyed by xfer_id.
	wait_xfer_exported "$PRIMARY_CNTLR_ID" \
		"$XFER0's subsystem $XNQN on cn$PRIMARY_CN ($PRIMARY_TRADDR:$PRIMARY_TRSVCID)" \
		"$XFER0_ID"

	# Deviation (b): a direct connect, to the PRIMARY's transport, because the
	# transfer is not in any CdcEntry. The standby exports $XNQN too, over a
	# plain dm-error table and ANA inaccessible (CN17); this case does not
	# connect it, so the one path host1 holds is the serving one.
	host_connect 1 "$PRIMARY_TRADDR" "$PRIMARY_TRSVCID" "$XNQN"

	# ANA first, device second (section 2's rule): a namespace whose only path
	# has never been usable gets no head disk at all.
	#
	# The uuid is $UUID1 and that is not a copy-paste error: CN17 gives the
	# transfer's namespace the ORIGIN namespace's uuid and nguid, so host1
	# resolves it under the same by-id name host0 uses for $SS0's ns 1. The
	# two are on different kernels, which is deviation (b)'s second reason.
	host_wait_ana 1 "$XNQN" "$PRIMARY_TRADDR" "$UUID1" optimized
	wait_dev 1 "$UUID1"
	assert_eq "$(host_path_state 1 "$XNQN" "$PRIMARY_TRADDR")" live \
		"host1's path to the transfer on cn$PRIMARY_CN"

	# host1 IO is legal here and host0's is not: host1 holds exactly one
	# namespace, the transfer's, and it is optimized. Nothing of host1's is
	# parked, so the `sync` inside host_sha_is has nothing wedged to flush.
	dev=$(host_dev "$UUID1")
	host_wait_sha 1 "$dev" "$BASELINE_MIB" "$SHA0" "$WAIT_HOST" \
		"host1 to read SHA0 through the transfer's namespace on $dev"
	log "  the transfer exports $TD0's raid0: host1 reads the same" \
		"$BASELINE_MIB MiB host0 wrote in setup"
}

# ---------------------------------------------------------------------------
# §7.5 copy step 2 — the clone
# ---------------------------------------------------------------------------

# copy_src_self is D21: the source is the transfer this case just made, and
# the transport is the PRIMARY's own — the CN connects to its own nvmet port.
copy_src_self() {
	COPY_SRC_BRANCH=self
	COPY_SRC_NQN=$XNQN
	# CN17 gives the transfer's namespace nsid = ori_ns_idx, and the origin is
	# ns 1.
	COPY_SRC_IDX=1
	# The source is $TD0's raid0, so the source geometry is THIS sp's: its
	# slice count, its stored stripe and its stored pool data block size.
	COPY_SRC_SLICES=$SLICE_CNT
	COPY_SRC_STRIPE=$SP_STRIPE_SIZE
	COPY_SRC_BLOCK=$SP_BLOCK_SIZE
	COPY_SRC_TRADDR=$PRIMARY_TRADDR
	COPY_SRC_TRSVCID=$PRIMARY_TRSVCID
	COPY_SRC_SHA=$SHA0
	COPY_SRC_SP=$SP
	COPY_SRC_TD=$TD0
}

# copy_src_sp_build is the decided fallback, run only when copy_clone_build
# reported a source fault. It builds $SP_SRC — one slice, --redund none, one
# cntlr on a CN that is NOT sp0's primary — gives it a thin device, a
# subsystem and a namespace, and has host1 write a pattern into it. That
# pattern's digest becomes COPY_SRC_SHA, so the destination is still proved
# byte for byte; it is simply no longer proved against SHA0.
copy_src_sp_build() {
	local cnv addr dev pat unit srcss srcns
	cnv=$SPARE_CN
	if [ "$cnv" -lt 0 ]; then
		# Every CN already carries a cntlr of sp0. The STANDBY's CN is still
		# the right pick: the point of the fallback is that the clone's
		# INITIATOR (the primary) and its TARGET are different kernels, and
		# pickCn only excludes CNs that already hold a cntlr of the SAME sp
		# (gateway/alloc.go:108-142), so sp1 may land there.
		cnv=$STANDBY_CN
	fi
	[ "$cnv" -ge 0 ] && [ "$cnv" != "$PRIMARY_CN" ] ||
		die "the fallback source needs a CN other than the primary cn$PRIMARY_CN," \
			"and this run has none"
	addr=$(cn_addr "$cnv")
	log "!!! copy: building the fallback source storage pool $SP_SRC on" \
		"cn$cnv ($addr) — one slice, --redund none, one cntlr"

	# --slots 0: cntlr_cnt must not exceed len(cntlid_slot_list)
	# (gateway/storagepool.go:319-324), and one cntlr needs one slot.
	# $THR is UNQUOTED so its eight words split, exactly as setup_create_sp
	# passes it — and it is the same set, because sp_thresholds chose it for the
	# copy case and this pool lives inside that case. That is the QUIET set, and
	# it is the right one here for the same reason: nothing about this pool is a
	# reaction test, and a failover in the middle of a hydration would invalidate
	# the digest comparison the whole fallback exists to make.
	# shellcheck disable=SC2086
	ctl_timeout 120 ctl_ok --sp "$SP_SRC" sp create \
		--cntlr-cnt 1 --slice-cnt 1 --init-ext-cnt "$INIT_EXT_CNT" \
		--slots 0 --redund none --stripe-size "$STRIPE_SIZE" \
		--cn-white "$addr" $THR
	SRC_SP_ID=$(jq_of "$CTL_OUT" '.sp_id')
	case "$SRC_SP_ID" in
	'' | *[!0-9]* | 0) die "sp create --sp $SP_SRC returned sp_id '$SRC_SP_ID'" ;;
	esac
	COPY_SRC_SP_BUILT=1

	ctl_ok --sp "$SP_SRC" sp get
	assert_field "$CTL_OUT" '.sp_name' "$SP_SRC" "the source pool's name"
	assert_field "$CTL_OUT" '.slice_list | length' 1 "$SP_SRC has one slice"
	assert_field "$CTL_OUT" '.cntlr_list | length' 1 "$SP_SRC has one cntlr"
	assert_field "$CTL_OUT" '.cntlr_list[0].primary' true \
		"the single cntlr of $SP_SRC is its primary"
	assert_field "$CTL_OUT" '.cntlr_list[0].addr_port' "$addr" \
		"--cn-white put it on cn$cnv"
	SRC_CNTLR_ID=$(jq_of "$CTL_OUT" '.sp_conf.cntlr_id_list[0]')
	SRC_TRADDR=$(jq_of "$CTL_OUT" '.cntlr_list[0].nvme_tr_conf.tr_addr')
	SRC_TRSVCID=$(jq_of "$CTL_OUT" '.cntlr_list[0].nvme_tr_conf.tr_svc_id')
	case "$SRC_CNTLR_ID" in
	'' | *[!0-9]* | 0) die "$SP_SRC reports cntlr_id '$SRC_CNTLR_ID'" ;;
	esac
	# The source geometry is read back from the pool that will serve it, never
	# assumed to match sp0's.
	COPY_SRC_SLICES=$(jq_of "$CTL_OUT" '.slice_list | length')
	COPY_SRC_STRIPE=$(jq_of "$CTL_OUT" \
		'.sp_conf.bdev_conf.dm_raid0_conf.stripe_size')
	COPY_SRC_BLOCK=$(jq_of "$CTL_OUT" \
		'.sp_conf.bdev_conf.dm_pool_conf.data_block_size')
	case "$COPY_SRC_STRIPE$COPY_SRC_BLOCK" in
	'' | *[!0-9]*) die "$SP_SRC reports a non-numeric bdev geometry" ;;
	esac

	# planSpGroups gives one slice a meta group and a data group, and
	# --redund none is one leg each (legCntOf), so: 2 groups, 2 legs, 1 pool.
	wait_until "$WAIT_PROVISION" "$SP_SRC's 2 sides to be provisioned" \
		src_sides_provisioned "$SP_SRC"
	SRC_STACK_LAST=""
	wait_until "$WAIT_PROVISION" \
		"$SP_SRC's cntlr $SRC_CNTLR_ID to build 1 pool, 2 groups, 2 legs" \
		src_cntlr_ready "$SP_SRC" "$SRC_CNTLR_ID" 1 2 2

	# The source thin device is made exactly the destination's size. The clone
	# maps the whole of $TD_CLONE and reads the source through it, so the
	# source must be at LEAST that big; equal is the simplest way to be sure,
	# and it keeps the digest comparison an identity rather than a prefix.
	# gateway/thindevice.go:145-171 wants a positive multiple of slice_cnt x
	# stripe_size, which for this pool is one stripe.
	[ "$COPY_SRC_STRIPE" -gt 0 ] && [ "$COPY_SRC_SLICES" -gt 0 ] ||
		die "$SP_SRC reports slice_cnt $COPY_SRC_SLICES and stripe" \
			"$COPY_SRC_STRIPE; neither may be zero"
	unit=$((COPY_SRC_SLICES * COPY_SRC_STRIPE))
	assert_eq "$((TD0_SIZE % unit))" 0 \
		"$TD0_SIZE is a multiple of $SP_SRC's slice_cnt x stripe_size ($unit)"
	ctl_ok --sp "$SP_SRC" td create --name "$TD_SRC" --size "$TD0_SIZE"
	wait_until "$WAIT_PROVISION" "$SP_SRC/$TD_SRC to report created" \
		src_td_created "$SP_SRC" "$TD_SRC"
	wait_until "$WAIT_PROVISION" \
		"$SP_SRC's cntlr to build $TD_SRC's raid0" \
		src_raid0_ready "$SP_SRC" "$SRC_CNTLR_ID" 1

	ctl_ok --sp "$SP_SRC" ss create --nqn "$SS_SRC"
	srcss=$(jq_of "$CTL_OUT" '.ss_id')
	case "$srcss" in
	'' | *[!0-9]* | 0) die "$SP_SRC ss create returned ss_id '$srcss'" ;;
	esac
	# host1 plus every CN: host1 writes the pattern, and the primary CN of sp0
	# is what will read it back as the clone's source.
	ctl_ok --sp "$SP_SRC" ss set-hosts --nqn "$SS_SRC" \
		--hosts "${HOST_NQN[1]},$CN_HOST_NQNS"
	ctl_ok --sp "$SP_SRC" ns create --nqn "$SS_SRC" --idx 1 \
		--td "$TD_SRC" --uuid "$UUID3"
	srcns=$(jq_of "$CTL_OUT" '.ns_id')
	case "$srcns" in
	'' | *[!0-9]* | 0) die "$SP_SRC ns create returned ns_id '$srcns'" ;;
	esac

	# The three calls above are control-plane writes and nothing more: this
	# pool's own cntlr still has to create the nvmet subsystem, link it to its
	# port and enable the namespace before anything can connect (run 3's
	# failure — see wait_ns_exported). One cntlr, so one gate, and it is
	# addressed through --sp like every other read of this pool.
	wait_ns_exported "$SRC_CNTLR_ID" \
		"$SS_SRC ns 1 on $SRC_TRADDR:$SRC_TRSVCID" \
		"$srcss" "$srcns" "$SP_SRC"

	# A DIRECT connect, not `connect-all`: the cdc advertises $SS0 to host1 as
	# well (setup allowed both hosts), and $SS0's ns 1 carries $UUID1 — the
	# same identity the transfer's namespace host1 already holds. Connecting
	# host1 through the discovery log would put both on one kernel.
	host_connect 1 "$SRC_TRADDR" "$SRC_TRSVCID" "$SS_SRC"
	host_wait_ana 1 "$SS_SRC" "$SRC_TRADDR" "$UUID3" optimized
	wait_dev 1 "$UUID3"

	dev=$(host_dev "$UUID3")
	pat="$WORK/pattern-src"
	host_make_pattern 1 "$pat" "$BASELINE_MIB"
	COPY_SRC_SHA=$(host_sha_range 1 "$pat" "$BASELINE_MIB") ||
		die "host1: digesting the fallback source pattern failed"
	assert_eq "${#COPY_SRC_SHA}" 64 \
		"the fallback source's reference digest is 64 hex digits"
	assert_ne "$COPY_SRC_SHA" "$SHA0" \
		"the fallback source's pattern differs from SHA0 (both are urandom)"
	host_write_range 1 "$pat" "$dev" "$BASELINE_MIB" 0
	host_wait_sha 1 "$dev" "$BASELINE_MIB" "$COPY_SRC_SHA" "$WAIT_HOST" \
		"host1 to read the fallback source's pattern back from $dev"

	COPY_SRC_BRANCH=sp1
	COPY_SRC_NQN=$SS_SRC
	COPY_SRC_IDX=1
	COPY_SRC_TRADDR=$SRC_TRADDR
	COPY_SRC_TRSVCID=$SRC_TRSVCID
	COPY_SRC_SP=$SP_SRC
	COPY_SRC_TD=$TD_SRC
	log "!!! copy: the fallback source is ready — $SS_SRC ns 1 on" \
		"$SRC_TRADDR:$SRC_TRSVCID, digest $COPY_SRC_SHA"
}

# copy_src_sp_teardown removes everything copy_src_sp_build made. It runs only
# when that branch ran, and it runs while the run is still healthy — on a
# failure nothing is removed and §7.9's dump keeps it all.
#
# The order is case_teardown's, for case_teardown's reasons: the namespace goes
# first UNDER the live controller (the one direction wait_dev_gone means
# something), the host lets go before the subsystem is unlinked (a subsystem
# unlinked under a live controller kills it with DNR), and only then the
# subsystem, the thin device and the pool.
copy_src_sp_teardown() {
	log "  removing the fallback source pool $SP_SRC"
	ctl_ok --sp "$SP_SRC" ns delete --nqn "$SS_SRC" --idx 1
	wait_dev_gone 1 "$UUID3"
	# The `||` can only catch ssh or the dispatch: disconnect_prefix throws
	# every `nvme disconnect`'s stdout, stderr and status away and ends
	# `return 0`, so the verb cannot report a disconnect that did not land.
	# The wait below is the check that actually holds.
	helper_host 1 disconnect_prefix "$SS_SRC" >/dev/null ||
		die "host1: the disconnect_prefix verb could not be run for $SS_SRC"
	wait_until "$WAIT_HOST" "host1 to drop its path to $SS_SRC" \
		host_path_gone 1 "$SS_SRC" "$SRC_TRADDR"
	ctl_ok --sp "$SP_SRC" ss delete --nqn "$SS_SRC"
	ctl_ok --sp "$SP_SRC" td delete --name "$TD_SRC"
	ctl_ok --sp "$SP_SRC" sp delete
	assert_field "$CTL_OUT" '.sp_id' "$SRC_SP_ID" \
		"DeleteStoragePoolReply.sp_id for $SP_SRC"
	wait_until "$WAIT_DELETE" "$SP_SRC to drain to NOT_FOUND" src_sp_gone
	COPY_SRC_SP_BUILT=0
	log "  $SP_SRC drained; its disk nodes are back in the pool of free DNs"
}

# copy_clone_build creates the clone from whatever COPY_SRC_* currently
# describes, feeds it one bitmap chunk, re-points it at the same transport and
# waits for hydration.
#
# It returns 1 — WITHOUT dying — when and only when copy_clone_progress named
# a source fault. Every other failure is a die, because every other failure is
# a bug the fallback cannot fix.
copy_clone_build() {
	local hex bytes
	log ""
	log "  --- clone source branch: $COPY_SRC_BRANCH"
	log "      src_nqn       $COPY_SRC_NQN, ns $COPY_SRC_IDX"
	log "      src geometry  $COPY_SRC_SLICES slice(s)," \
		"stripe $COPY_SRC_STRIPE, block $COPY_SRC_BLOCK"
	log "      src transport tcp/ipv4 $COPY_SRC_TRADDR:$COPY_SRC_TRSVCID"
	log "      expected digest of the first $BASELINE_MIB MiB: $COPY_SRC_SHA"

	# All four --src-tr-* are passed. trConfFlags declares DEFAULTS
	# (tcp/ipv4/127.0.0.1/4420, ctl/root.go:617-622) rather than empty
	# strings, so an omitted --src-tr-addr would silently send 127.0.0.1 and
	# the CN would connect to its own loopback on the wrong port.
	ctl_ok clone create --name "$CLONE0" --dst-td "$TD_CLONE" \
		--src-nqn "$COPY_SRC_NQN" --src-idx "$COPY_SRC_IDX" \
		--src-slices "$COPY_SRC_SLICES" --src-stripe "$COPY_SRC_STRIPE" \
		--src-block "$COPY_SRC_BLOCK" \
		--src-tr-type tcp --src-adr-fam ipv4 \
		--src-tr-addr "$COPY_SRC_TRADDR" --src-tr-svc-id "$COPY_SRC_TRSVCID" \
		--auto-resume
	CLONE0_ID=$(jq_of "$CTL_OUT" '.clone_id')
	case "$CLONE0_ID" in
	'' | *[!0-9]* | 0) die "clone create returned clone_id '$CLONE0_ID'" ;;
	esac

	ctl_ok clone get --name "$CLONE0"
	assert_field "$CTL_OUT" '.clone.clone_id' "$CLONE0_ID" \
		"GetClone's clone_id vs the CreateClone reply"
	assert_field "$CTL_OUT" '.clone.src_nqn' "$COPY_SRC_NQN" "the clone's source"
	assert_field "$CTL_OUT" '.clone.dst_td_id' "$TD_CLONE_ID" \
		"the clone's destination is $TD_CLONE (uint64, a JSON string)"
	assert_field "$CTL_OUT" '.clone.src_slice_cnt' "$COPY_SRC_SLICES" \
		"src_slice_cnt (uint32, a bare number)"
	assert_field "$CTL_OUT" '.clone.src_stripe_size' "$COPY_SRC_STRIPE" \
		"src_stripe_size (uint64, a JSON string)"
	assert_field "$CTL_OUT" '.clone.src_block_size' "$COPY_SRC_BLOCK" \
		"src_block_size (uint64, a JSON string)"
	assert_field "$CTL_OUT" '.clone.auto_resume' true "auto_resume was stored"
	assert_field "$CTL_OUT" '.clone.deleting' false \
		"a fresh clone is not latched for deletion"
	assert_jq "$CTL_OUT" \
		"(.clone.src_tr_conf_list | length) == 1
		 and .clone.src_tr_conf_list[0].tr_addr == \"$COPY_SRC_TRADDR\"
		 and .clone.src_tr_conf_list[0].tr_svc_id == \"$COPY_SRC_TRSVCID\"" \
		"src_tr_conf_list is the one transport the flags described"

	# One bitmap chunk, from the SOURCE thin device's slice 0. The wire
	# convention of every bitmap RPC is 1 = unwritten/skippable and
	# GetThinDeviceBitmap already answers in it (architecture.md §8.13,
	# ":2097-2098 agents and callers invert once at the boundary"), so the
	# reply is fed straight through with no inversion here.
	#
	# --cnt 0 is the whole slice, and an EMPTY bitmap would be refused by the
	# gateway on purpose ("bitmap must not be empty"), so the byte count is
	# asserted before it is sent rather than after it is refused.
	ctl_ok --sp "$COPY_SRC_SP" td get-bm --name "$COPY_SRC_TD" \
		--slice-idx 0 --start 0 --cnt 0
	bytes=$(jq_of "$CTL_OUT" '.byte_cnt')
	assert_jq "$CTL_OUT" '(.byte_cnt | type) == "number"' \
		"byte_cnt is a Go int and renders as a bare number (ctl/root.go:400-405)"
	assert_ge "$bytes" 1 \
		"slice 0 of $COPY_SRC_SP/$COPY_SRC_TD has a non-empty mapping bitmap"
	assert_le "$bytes" 1048576 \
		"one chunk may not exceed common.CloneBmChunkBytes (gateway/clone.go:588-591)"
	hex=$(jq_of "$CTL_OUT" '.bitmap_hex')
	assert_eq "${#hex}" "$((bytes * 2))" \
		"bitmap_hex is two hex digits per byte_cnt byte"

	# The chunk is addressed by the PAIR (src_slice_idx, bm_idx), never by
	# either alone (ctl/clone.go's append-bm comment; CN22 bounds them
	# independently). Chunk (0, 0) is the first CloneBmChunkBytes of source
	# slice 0's bitmap, which is the whole of it at every shape this suite
	# builds.
	ctl_ok clone append-bm --name "$CLONE0" --src-slice-idx 0 --bm-idx 0 \
		--bm-hex "$hex"
	assert_field "$CTL_OUT" '.clone_id' "$CLONE0_ID" \
		"AppendCloneBitmapReply.clone_id"

	# UpdateCloneTrConf with the SAME transport: the RPC exists for a source
	# that moved, and re-sending the current one is the only way to exercise
	# it without moving anything. ensureCloneSource skips an entry it is
	# already connected to (agent/cnagent/clone.go:197-200), so this cannot
	# disturb a connection that is already up.
	ctl_ok clone set-tr --name "$CLONE0" \
		--src-tr-type tcp --src-adr-fam ipv4 \
		--src-tr-addr "$COPY_SRC_TRADDR" --src-tr-svc-id "$COPY_SRC_TRSVCID"
	assert_field "$CTL_OUT" '.clone_id' "$CLONE0_ID" \
		"UpdateCloneTrConfReply.clone_id"

	COPY_SRC_START=$SECONDS
	COPY_HYD_MARK=$SECONDS
	COPY_HYD_SIG=""
	COPY_SRC_FAULT=""
	wait_until "$WAIT_HYDRATE" \
		"clone $CLONE0 (clone_id $CLONE0_ID) to finish hydrating on cntlr $PRIMARY_CNTLR_ID" \
		copy_clone_progress "$PRIMARY_CNTLR_ID" "$CLONE0_ID"
	if [ -n "$COPY_SRC_FAULT" ]; then
		return 1
	fi
	log "  clone $CLONE0 is fully hydrated: $COPY_HYD_SIG"
	return 0
}

# copy_clone_abandon undoes a clone whose source proved unusable, so the
# fallback starts from the same state the first attempt did.
#
# $TD_CLONE is REBUILT and not reused, and that is not tidiness: [D3] makes
# "the destination td was never written before the clone" a contract the CP
# cannot verify, and §11.5 recovery equates "mapped in the destination thin
# pool" with "already copied". A td that a failed clone partially hydrated
# violates both. `td delete` also refuses while the clone still exists
# ("thin device %s is the destination of clone %s",
# gateway/thindevice.go:246-253), which is why the drain is waited out first.
copy_clone_abandon() {
	log "!!! copy: abandoning clone $CLONE0 — $COPY_SRC_FAULT"
	log "!!! copy: the $COPY_SRC_BRANCH source is unusable in this lab;" \
		"falling back to §7.5's second storage pool"
	# --force: hydration is unproven by definition here, and without it the
	# gateway would refuse ("clone %q has not finished hydrating").
	ctl_ok clone delete --name "$CLONE0" --force
	assert_field "$CTL_OUT" '.clone_id' "$CLONE0_ID" "DeleteCloneReply.clone_id"
	wait_until "$WAIT_DELETE" \
		"the sp coordinator to drain clone $CLONE0 to NOT_FOUND" clone_gone
	CLONE0_ID=""
	ctl_ok td delete --name "$TD_CLONE"
	assert_field "$CTL_OUT" '.td_id' "$TD_CLONE_ID" \
		"DeleteThinDeviceReply.td_id for the abandoned $TD_CLONE"
	copy_make_clone_td
}

# copy_make_clone_td creates the destination thin device and waits until the
# primary carries its raid0 — the row the dm-clone's `dest` argument needs
# (CN18 step 3: dest = the dst td's CnRaid0Name), which `created` does not
# cover (`created` is defined on the thin volumes alone, ThinDeviceCreated.md
# R13).
copy_make_clone_td() {
	ctl_ok td create --name "$TD_CLONE" --size "$TD0_SIZE"
	TD_CLONE_ID=$(jq_of "$CTL_OUT" '.td_id')
	case "$TD_CLONE_ID" in
	'' | *[!0-9]* | 0) die "td create returned td_id '$TD_CLONE_ID'" ;;
	esac
	wait_until "$WAIT_PROVISION" "$TD_CLONE to report created" \
		td_created "$TD_CLONE"
	assert_field "$CTL_OUT" ".name_to_td.$TD_CLONE.size" "$TD0_SIZE" \
		"$TD_CLONE is the same size as $TD0 (the clone copies the whole device)"
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to build raid0s for $TD0 and $TD_CLONE" \
		cntlr_raid0_ready "$PRIMARY_CNTLR_ID" 2
}

copy_clone() {
	stage 02 "clone create into $TD_CLONE from the transfer, hydrate, then delete it"

	# The geometry the source describes, read back from the sp rather than
	# assumed. STRIPE_SIZE is asserted rather than merely read because
	# validateCloneGeometry's ceiling for src_stripe_size is 256 x 4 KiB =
	# 1 MiB (gateway/validate.go:403-410) while the sp itself would accept
	# 64 MiB: a larger stripe creates the sp happily and fails here.
	sp_refresh
	sp_read_roles
	SP_STRIPE_SIZE=$(sp_field '.sp_conf.bdev_conf.dm_raid0_conf.stripe_size')
	SP_BLOCK_SIZE=$(sp_field '.sp_conf.bdev_conf.dm_pool_conf.data_block_size')
	case "$SP_STRIPE_SIZE$SP_BLOCK_SIZE" in
	'' | *[!0-9]*) die "$SP reports a non-numeric bdev_conf geometry" ;;
	esac
	assert_eq "$SP_STRIPE_SIZE" "$STRIPE_SIZE" \
		"the sp's stored stripe_size is what --stripe-size asked for"
	assert_le "$SP_STRIPE_SIZE" 1048576 \
		"src_stripe_size may not exceed 256 x 4 KiB (gateway/validate.go:403-410)"

	copy_make_clone_td

	# The CN host NQNs go into the transfer's allowed_hosts. `xfer set-hosts`
	# REPLACES the list (ctl/xfer.go's set-hosts comment), so host1's own nqn
	# is repeated or it would lose its connection.
	copy_cn_host_nqns
	ctl_ok xfer set-hosts --name "$XFER0" \
		--hosts "${HOST_NQN[1]},$CN_HOST_NQNS"
	assert_field "$CTL_OUT" '.xfer_id' "$XFER0_ID" \
		"UpdateTransferHostsReply.xfer_id"
	ctl_ok xfer get --name "$XFER0"
	assert_field "$CTL_OUT" '.xfer.allowed_hosts | length' \
		"$((CN_CNT + 1))" \
		"allowed_hosts is host1 plus all $CN_CNT CN host nqns"
	assert_jq "$CTL_OUT" \
		"[.xfer.allowed_hosts[] | select(. == \"$PRIMARY_CN_NQN\")] | length == 1" \
		"the primary CN's own host nqn is in the transfer's allowed_hosts"

	# The kernel's answer, not etcd's: the clone's first connect must not be
	# refused for want of an allowed_hosts link that is still only a record.
	wait_until "$WAIT_HOST" \
		"cn$PRIMARY_CN to link $PRIMARY_CN_NQN into $XNQN's allowed_hosts" \
		copy_xfer_host_linked "$PRIMARY_CN" "$PRIMARY_CN_NQN"

	copy_src_self
	if ! copy_clone_build; then
		copy_clone_abandon
		copy_src_sp_build
		copy_clone_build ||
			die "the fallback source $SS_SRC also failed: $COPY_SRC_FAULT." \
				"Both §7.5 sources are exhausted; this is not a lab" \
				"limitation the suite knows how to work around"
	fi
	log "  clone source branch that ran: $COPY_SRC_BRANCH"

	# The destination namespace, created while the clone is still live. CN16
	# rule 5 backs it with CnCloneFinalName; when the clone goes, rule 6 moves
	# the same ns-dev onto $TD_CLONE's own raid0. Both halves are asserted —
	# this one by the ANA state and the device node, the next stage's by the
	# digest, which is read only after the transfer is gone (deviation (a)).
	ctl_ok ns create --nqn "$SS0" --idx 2 --td "$TD_CLONE" --uuid "$UUID2"
	NS2_ID=$(jq_of "$CTL_OUT" '.ns_id')
	case "$NS2_ID" in
	'' | *[!0-9]* | 0) die "ns create returned ns_id '$NS2_ID'" ;;
	esac
	# NOTHING CONNECTS HERE — host0 has held the controller to $SS0 since setup
	# and the kernel picks the new namespace up on the AEN — and the gate is
	# still the same one, for the reason §4.5 stage 01's identical site gives:
	# `ns create` returning says the gateway committed the record, not that the
	# primary's agent has created and enabled the nvmet namespace. Without it a
	# slow converge spends the whole WAIT_HOST of the host_wait_ana below and
	# then blames ANA. This namespace has MORE to build than react's, not less:
	# CN16 rule 5 backs it with the live dm-clone. PRIMARY_CNTLR_ID and
	# PRIMARY_TRADDR are this case's own, from the sp_read_roles at the top of
	# copy_clone; $SS0_ID is setup's.
	wait_ns_exported "$PRIMARY_CNTLR_ID" \
		"$SS0 ns 2 ($TD_CLONE, over the live dm-clone) on cn$PRIMARY_CN" \
		"$SS0_ID" "$NS2_ID"
	# Neither of these is IO: host_wait_ana reads sysfs and host_dev_present
	# is a `test -e`, so both are legal while host0's ns 1 is parked.
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID2" optimized
	wait_dev 0 "$UUID2"
	log "  host0 sees $(host_dev "$UUID2") over the clone (CN16 rule 5);" \
		"its digest is read in stage 03, after the transfer is aborted"

	# `clone delete` WITHOUT --force is the point: the gateway must prove
	# hydration from the primary's own clone_id_to_dm_clone row before it
	# latches (gateway/clone.go:369-408), so a successful call is a second,
	# independent confirmation of the wait above.
	ctl_ok clone get --name "$CLONE0"
	assert_field "$CTL_OUT" '.clone.deleting' false \
		"the clone is still live before the delete"
	ctl_ok clone delete --name "$CLONE0"
	assert_field "$CTL_OUT" '.clone_id' "$CLONE0_ID" "DeleteCloneReply.clone_id"

	# CLD4: DeleteClone LATCHES and returns. The record survives with
	# deleting = true until the sp coordinator has drained its chunk keys, so
	# the latch is observable — and then the drain's final STM removes it.
	#
	# The latch is read through ctl_try, not ctl_ok: the drain is a background
	# pass and the record may already be gone by the time this line runs, in
	# which case NOT_FOUND is the right answer and not a failure.
	if ctl_try clone get --name "$CLONE0"; then
		assert_field "$CTL_OUT" '.clone.deleting' true \
			"CLD4: a deleted clone is latched before it is drained"
		log "  clone $CLONE0 is latched (deleting = true); waiting for the drain"
	else
		log "  clone $CLONE0 was already drained when the latch was read"
	fi
	wait_until "$WAIT_DELETE" \
		"the sp coordinator to drain clone $CLONE0 to NOT_FOUND" clone_gone
	sp_refresh
	assert_field "$SP_JSON" '.sp_conf.clone_name_list | length' 0 \
		"clone_name_list is empty again"
	log "  $TD_CLONE's namespaces are now backed by its own raid0 (CN16 rule 6)"
}

# ---------------------------------------------------------------------------
# §7.5 copy step 3 — abort the transfer, then prove the copy
# ---------------------------------------------------------------------------
#
# `xfer delete --force` is the ABORT path and the flag is not optional here:
# without it the STM also writes `suspended = true` on the origin namespace,
# FINALIZING the hand-over so the source stays retired (ctl/xfer.go's delete
# comment; the write itself is gateway/transfer.go:192-207). This case wants
# the origin back.
#
# host1 lets go BEFORE the subsystem is unlinked, which is case_teardown's
# rule (a) applied here: a subsystem unlinked from its port under a live
# controller refuses the reconnect with DNR and the kernel deletes the
# controller (memory note nvmet-port-unlink-dnr-kills-host-ctrl). Doing it in
# this order means host1 ends the stage with no path at all rather than with
# one the kernel tore down behind us.
copy_xfer_delete() {
	stage 03 "xfer delete --force, then host0 reads both namespaces back"
	local dev

	# The `||` catches ssh or the dispatch and NOTHING ELSE: disconnect_prefix
	# discards each `nvme disconnect`'s output and status and ends `return 0`.
	# What proves host1 let go — and so that `xfer delete` below does not
	# unlink the subsystem under a live controller — is the wait after it.
	helper_host 1 disconnect_prefix "$XNQN" >/dev/null ||
		die "host1: the disconnect_prefix verb could not be run for $XNQN," \
			"so nothing has been asked to let go of the subsystem below"
	wait_until "$WAIT_HOST" "host1 to drop its path to $XNQN" \
		host_path_gone 1 "$XNQN" "$PRIMARY_TRADDR"

	ctl_ok xfer delete --name "$XFER0" --force
	assert_field "$CTL_OUT" '.xfer_id' "$XFER0_ID" "DeleteTransferReply.xfer_id"
	ctl_fail NOT_FOUND xfer get --name "$XFER0"
	sp_refresh
	assert_field "$SP_JSON" '.sp_conf.xfer_name_list | length' 0 \
		"xfer_name_list is empty again"

	# The origin comes back. The abort path left `suspended` false, so the
	# next syncup finds nothing effectively suspending ns 1 and CN16 gives it
	# AnaGrpIdOptimized on the primary again.
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" optimized
	wait_dev 0 "$UUID1"
	log "  host0's origin namespace is live again; host0 IO is legal from here"

	# Deviation (a): the destination's digest, read now — through
	# $TD_CLONE's own raid0, with no dm-clone above it and no source
	# connection anywhere — so it can only be what hydration wrote.
	dev=$(host_dev "$UUID2")
	host_wait_sha 0 "$dev" "$BASELINE_MIB" "$COPY_SRC_SHA" "$WAIT_HOST" \
		"host0 to read the copied bytes back from $dev ($TD_CLONE)"
	log "  the $COPY_SRC_BRANCH source's first $BASELINE_MIB MiB are in" \
		"$TD_CLONE, byte for byte"
	check_sha0 "after the transfer was aborted"

	# This case's own objects go before case_finish, whose pre-check allows
	# exactly one subsystem, one namespace and one thin device through.
	ctl_ok ns delete --nqn "$SS0" --idx 2
	assert_field "$CTL_OUT" '.ns_id' "$NS2_ID" "DeleteNamespaceReply.ns_id"
	wait_dev_gone 0 "$UUID2"
	ctl_ok td delete --name "$TD_CLONE"
	assert_field "$CTL_OUT" '.td_id' "$TD_CLONE_ID" "DeleteThinDeviceReply.td_id"

	if [ "$COPY_SRC_SP_BUILT" = 1 ]; then
		copy_src_sp_teardown
	fi
}

# ---------------------------------------------------------------------------
# §7.5 copy step 4 — migration
# ---------------------------------------------------------------------------
#
# A migration moves ONE leg's side to another disk node: the gateway gives the
# leg a second Side, the destination DN drives a dm-clone from the first, and
# the operator commits with `migr finish` or rolls back with `migr cancel`.
#
# WHY NOTHING ABOVE THE LEG NOTICES. Both sides of a migrating leg export the
# SAME subsystem NQN — SideToCnNqn keys on (cluster, sp, LEG, cn) and carries
# no dn id (common/name_fmt.go:541-556, and its own comment says why) — so
# every CN's kernel aggregates the two as two paths of one namespace and ANA
# picks the live one. md never sees a member change, which is why this step
# asserts that the host still reads SHA0 across the commit and does NOT wait
# for an md resync.
#
# WHAT IT DOES HAVE TO WAIT FOR, AND THE TRAP THAT MAKES IT MANDATORY. "ANA
# picks the live one" is true only once there IS one. agent/dnagent/plan.go's
# anaGrpId (:267-283) puts a migration SOURCE in AnaGrpIdInaccessible the
# moment the migration exists, and a DESTINATION in AnaGrpIdInaccessible until
# `cloneLive` — which is nothing more than "the dm-clone device is present"
# (agent/dnagent/probe.go:215-218). So between `migr create` and the
# destination's dm-clone being built, EVERY path of that leg is inaccessible,
# and an inaccessible nvme namespace REQUEUES rather than errors. A host read
# issued in that window blocks in D state, where `timeout` cannot reach it
# (memory notes ana-inaccessible-ns-no-blockdev and
# timeout-does-not-bound-suspended-dm-read).
#
# This step therefore issues NO host IO between `migr create` and the wait
# that proves the leg is serving again, and it proves that from the PRIMARY
# CN's own view of its path (cn_wait_ana), not from the gateway's record — the
# record going away is what the DN acts on, one syncup later.
#
# The worker will not interfere, and that holds for a reason that does not
# depend on a number: AR8 skips a leg with two sides outright ("two sides means
# a user migration is in flight on this leg", worker/reaction.go:1185), so the
# whole window is invisible to leg repair however long it lasts. The copy case's
# sp also carries the QUIET threshold set (§7.1: leg_unhealthy $THR_LEG s), so
# nothing here is even close to a threshold — but the migration would be safe at
# the reacting set too, which is why the two-sides rule is the argument and the
# threshold is only the belt.
#
# PLACEMENT, and the one assertion that needs a guard: the destination's DN is
# excluded by the BLACK LIST, always — grpDnAddrs names every DN the group
# occupies and seeds it (gateway/migration.go's CreateMigration planning read)
# — so "not a DN of this group" is unconditional. The destination's LOCATION
# is a weaker promise: §6.5 tier 1 excludes the group's failure domains, and
# tier 2 RESCANS WITHOUT that exclusion whenever tier 1 yields fewer
# candidates than the legs being placed (gateway/alloc.go:67-104, and the
# fact check's own warning: assert LOCATIONS, not just addr_ports, and know
# that tier 2 relaxes them).
#
# Tier 1 falls short only when no DN outside the group's locations can take
# the group's ext_cnt — which at a shape with more DN VMs than legs means the
# cluster is out of extents, not that the rule was waived. So the location
# assertion below is made only when DN_VM_CNT > LEGS, i.e. exactly when tier 1
# has somewhere anti-affine to go; with DN_VM_CNT == LEGS it is skipped with a
# log line instead of failing a run that is behaving correctly.
copy_migration() {
	stage 04 "migr create, append-bm, finish — then a second one, cancelled"
	local gid legid sideid saddr dstside dstaddr dstvm before_addrs before_vms
	local hex v k legnqn straddr strsvcid

	sp_refresh
	sp_read_roles
	copy_primary_cn_id
	gid=$(sp_field "$COPY_GRP0.grp_id")
	legid=$(sp_field "$COPY_GRP0.leg_list[0].leg_id")
	sideid=$(sp_field "$COPY_GRP0.leg_list[0].side_list[0].side_id")
	saddr=$(sp_field "$COPY_GRP0.leg_list[0].side_list[0].addr_port")
	case "$gid$legid$sideid" in
	'' | *[!0-9]*) die "slice 0's data group carried a non-decimal id" ;;
	esac
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list | length" 1 \
		"the leg to migrate has exactly one side before the migration"
	v=$(dn_vm_of_addr "$saddr")
	k=$(dn_inst_of_addr "$saddr")
	assert_ne "$v" none "side $sideid's addr_port $saddr names one of the --dn guests"
	assert_ne "$k" none "side $sideid's addr_port $saddr names a dn instance index"
	diag_note_dn "$v" "$k"

	# What the group occupies now, so the destination can be checked against
	# both exclusions.
	before_addrs=$(sp_field \
		"$COPY_GRP0 | [.leg_list[] | .side_list[] | .addr_port] | unique | join(\",\")")
	before_vms=$(sp_field \
		"$COPY_GRP0 | [.leg_list[] | .side_list[] | $SP_SIDE_VM] | unique | join(\",\")")
	log "  migrating side $sideid of leg $legid (group $gid) off dn$v" \
		"instance $k ($saddr); the group holds $before_addrs on VMs $before_vms"

	# THE LEG BITMAP IS READ BEFORE THE MIGRATION EXISTS, and that is not a
	# stylistic ordering. `migr create` takes the SOURCE side ANA-inaccessible
	# at once — agent/dnagent/plan.go:267-283, `migrSrc != nil` =>
	# AnaGrpIdInaccessible — and the DESTINATION stays inaccessible until its
	# dm-clone exists (`migrDst != nil && !cloneLive`, where cloneLive is just
	# "the dm-clone device is there", agent/dnagent/probe.go:215-218). So from
	# the create until hydration begins, this leg has no usable path on any CN.
	# GetLegBm goes to the PRIMARY CN and walks the slice's thin-pool METADATA,
	# which lives on the slice's META groups and not on this DATA group, so it
	# would very likely be safe inside that window — reading it before the
	# window opens costs nothing and removes the question. The value is still
	# the right one when it is appended: nothing writes the leg in between.
	ctl_ok td get-leg-bm --leg "$legid" --start 0 --cnt 0
	assert_jq "$CTL_OUT" '(.byte_cnt | type) == "number"' \
		"td get-leg-bm renders the same §3.1 hex map"
	hex=$(jq_of "$CTL_OUT" '.bitmap_hex')

	ctl_ok migr create --name "$MIGR0" --src-side "$sideid"
	MIGR0_ID=$(jq_of "$CTL_OUT" '.migr_id')
	case "$MIGR0_ID" in
	'' | *[!0-9]* | 0) die "migr create returned migr_id '$MIGR0_ID'" ;;
	esac

	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list | length" 2 \
		"the migrating leg now has two sides"
	dstside=$(sp_field \
		"$COPY_GRP0.leg_list[0] | [.side_list[] | select(.side_id != \"$sideid\")] | .[0].side_id")
	dstaddr=$(sp_field \
		"$COPY_GRP0.leg_list[0] | [.side_list[] | select(.side_id != \"$sideid\")] | .[0].addr_port")
	case "$dstside" in
	'' | *[!0-9]* | 0) die "the migration destination side has side_id '$dstside'" ;;
	esac
	dstvm=${dstaddr%:*}
	case ",$before_addrs," in
	*",$dstaddr,"*)
		die "the migration destination $dstaddr is a disk node the group" \
			"already occupies; grpDnAddrs seeds the black list" \
			"(gateway/migration.go:251, gateway/alloc.go:545-556), so this" \
			"cannot happen"
		;;
	esac
	if [ "$DN_VM_CNT" -gt "$LEGS" ]; then
		case ",$before_vms," in
		*",$dstvm,"*)
			die "the migration destination $dstaddr is on a DN VM the group" \
				"already occupies ($before_vms). §6.5 tier 1 excludes the" \
				"group's locations and relaxes only when no DN outside them" \
				"can take the group's ext_cnt — with $DN_VM_CNT DN VMs and" \
				"$LEGS leg(s) per group that would mean the cluster is out" \
				"of extents, not that the rule was waived"
			;;
		esac
	else
		log "  $DN_VM_CNT DN VM(s) and $LEGS leg(s) per group: §6.5 tier 1 has" \
			"nowhere anti-affine to go, so the destination VM is not asserted"
	fi
	# [D-I]: the destination side takes the first cntlid slot that differs
	# from the source's, because the two sides of ONE leg are aggregated by
	# every CN and must occupy different CNTLID ranges
	# (gateway/migration.go:84-93).
	assert_jq "$SP_JSON" \
		"($COPY_GRP0.leg_list[0]
		  | ([.side_list[] | .cntlid_slot] | unique | length)) == 2" \
		"the two sides of the migrating leg hold different cntlid slots"
	v=$(dn_vm_of_addr "$dstaddr")
	k=$(dn_inst_of_addr "$dstaddr")
	assert_ne "$v" none "the destination $dstaddr names one of the --dn guests"
	assert_ne "$k" none "the destination $dstaddr names a dn instance index"
	diag_note_dn "$v" "$k"
	log "  destination side $dstside on dn$v instance $k ($dstaddr)"

	ctl_ok migr get --name "$MIGR0"
	assert_field "$CTL_OUT" '.migr.migr_id' "$MIGR0_ID" \
		"GetMigration's migr_id vs the CreateMigration reply"
	assert_field "$CTL_OUT" '.migr.src_side_id' "$sideid" "the migration's source side"
	assert_field "$CTL_OUT" '.migr.dst_side_id' "$dstside" "the migration's destination side"
	assert_field "$CTL_OUT" '.migr.bm_cnt' 0 \
		"a fresh migration holds no bitmap chunks (uint32, a bare number)"

	# One skip-bitmap chunk, from the reading taken above. A migration's chunks
	# are a sequence, not a rectangle: `migr append-bm` has no slice index and
	# stores at bm_idx = bm_cnt (ctl/migr.go's file header). The bitmap is the
	# LEG's projection, so it came from `td get-leg-bm` and not `td get-bm`.
	if [ -n "$hex" ]; then
		ctl_ok migr append-bm --name "$MIGR0" --bm-hex "$hex"
		assert_field "$CTL_OUT" '.migr_id' "$MIGR0_ID" \
			"AppendMigrationBitmapReply.migr_id"
		ctl_ok migr get --name "$MIGR0"
		assert_field "$CTL_OUT" '.migr.bm_cnt' 1 \
			"the migration now holds one bitmap chunk"
	else
		# An empty --bm-hex sends an EMPTY bitmap on purpose and the gateway
		# refuses it, so an empty leg bitmap is reported rather than sent.
		log "  leg $legid's bitmap is empty; skipping migr append-bm" \
			"(an empty --bm-hex is refused on purpose, ctl/migr.go)"
	fi

	# FinishMigration without --force proves hydration with a live
	# GetSideInfo of the DESTINATION side, so the wait below is the same
	# measurement the RPC will make.
	MIGR_HYD_LAST=""
	wait_until "$WAIT_PROVISION" \
		"the migration's destination side $dstside to zero and hydrate" \
		side_hydrated "$dstside"

	ctl_ok migr finish --name "$MIGR0"
	assert_field "$CTL_OUT" '.migr_id' "$MIGR0_ID" "FinishMigrationReply.migr_id"
	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list | length" 1 \
		"the leg is back to one side"
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list[0].side_id" \
		"$dstside" "and that side is the migration's destination"
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].leg_id" "$legid" \
		"the leg itself is unchanged: a migration moves a side, not a leg"
	assert_field "$SP_JSON" '.sp_conf.migr_name_list | length' 0 \
		"migr_name_list is empty again"
	ctl_fail NOT_FOUND migr get --name "$MIGR0"

	# THE LEG MUST BE SERVING AGAIN BEFORE ANY HOST IO TOUCHES IT. A migration
	# SOURCE side is ANA-inaccessible from the moment the migration exists, and
	# a destination side is inaccessible until its dm-clone exists
	# (agent/dnagent/plan.go:267-283). Both conditions are cleared by now — the
	# record is gone — but the DN rewrites `ana_grpid` on its NEXT syncup, so
	# this reads the primary CN's own view of its path to the surviving side
	# instead of assuming the rewrite has landed. An inaccessible namespace
	# REQUEUES rather than errors, and a requeued read is exactly the thing
	# `timeout` cannot bound (rule 5), so this wait is what keeps check_sha0 a
	# read and not a gamble.
	straddr=$(sp_field "$COPY_GRP0.leg_list[0].side_list[0].nvme_tr_conf.tr_addr")
	strsvcid=$(sp_field "$COPY_GRP0.leg_list[0].side_list[0].nvme_tr_conf.tr_svc_id")
	legnqn=$(side_to_cn_nqn "$SP_ID" "$legid" "$PRIMARY_CN_ID")
	case "$legnqn" in
	*notanid-*) die "side_to_cn_nqn produced '$legnqn'" ;;
	esac
	cn_wait_ana "$PRIMARY_CN" "$legnqn" "$straddr" "$strsvcid" optimized
	check_sha0 "after a migration committed onto a new disk node"

	# The rollback. The destination side of the FIRST migration is now the
	# leg's only side, so it is the source of the second — which proves that
	# a migrated-onto side is an ordinary side.
	ctl_ok migr create --name "$MIGR1" --src-side "$dstside"
	MIGR1_ID=$(jq_of "$CTL_OUT" '.migr_id')
	case "$MIGR1_ID" in
	'' | *[!0-9]* | 0) die "migr create returned migr_id '$MIGR1_ID'" ;;
	esac
	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list | length" 2 \
		"the second migration gave the leg two sides again"
	ctl_ok migr get --name "$MIGR1"
	assert_field "$CTL_OUT" '.migr.src_side_id' "$dstside" \
		"the second migration's source is the first one's destination"

	# Cancelled at once, deliberately: `migr cancel` throws an unfinished copy
	# away and needs no proof about it (ctl/migr.go's file header — the
	# request has no force field at all), so waiting for the destination to
	# zero first would test nothing this step is about.
	ctl_ok migr cancel --name "$MIGR1"
	assert_field "$CTL_OUT" '.migr_id' "$MIGR1_ID" "CancelMigrationReply.migr_id"
	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list | length" 1 \
		"the cancelled migration's destination side is gone"
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list[0].side_list[0].side_id" \
		"$dstside" "and the source side is untouched"
	assert_field "$SP_JSON" '.sp_conf.migr_name_list | length' 0 \
		"migr_name_list is empty again"
	ctl_fail NOT_FOUND migr get --name "$MIGR1"

	# The same wait, for the same reason: `migr create` took this side
	# ANA-inaccessible and the cancel only removed the record — the DN's next
	# syncup is what puts the namespace back in the optimized group.
	cn_wait_ana "$PRIMARY_CN" "$legnqn" "$straddr" "$strsvcid" optimized
	check_sha0 "after a migration was cancelled"
}

# ---------------------------------------------------------------------------
# §7.5 copy step 5 — spare legs (raid1 only)
# ---------------------------------------------------------------------------
#
# A spare leg is a leg of a raid1 group that every cntlr connects to and
# health-checks but that md never sees, until SwitchSpareLeg trades it for an
# active one (architecture.md §8.12). §7.5 skips this step under --redund
# none, and the gateway would refuse it anyway: CreateSpareLeg answers
# INVALID_ARGUMENT "group %d is RedundNone: there is no redundancy to repair"
# (gateway/spareleg.go:194-198).
#
# THE LEG-ROW ARITHMETIC IS WHY cntlr_full_ready EXISTS. CN10 walks
# spare_leg_list as well as leg_list, so a spare adds a leg_id_to_leg row on
# every cntlr, and a target taken from the fresh-sp constant GRP_CNT x LEGS
# would be one short for ever. cntlr_full_ready computes the number from the
# CURRENT `sp get` on every poll, which is what this step — and setup itself —
# uses.
#
# THE SWITCH IS A FULL REBUILD, not a bitmap-scoped repair: a fresh spare has
# never been an md member, so md recovers the whole group onto it
# (architecture.md §8.12's own note, "a fresh spare gets a full resync"). At
# $INIT_EXT_CNT x $EXTENT_SIZE per group that is 64 MiB at the default shape.
copy_spare() {
	if [ "$REDUND" != raid1 ]; then
		log ""
		log "=== copy step 5 (spare legs) SKIPPED: --redund $REDUND." \
			"A RedundNone group has one leg and no md array, and" \
			"CreateSpareLeg refuses it with INVALID_ARGUMENT" \
			"(gateway/spareleg.go:194-198)."
		return 0
	fi
	stage 05 "spare create, spare switch and spare delete on slice 0's data group"
	local gid target spare spareaddr sparevm before_addrs before_vms rev

	sp_refresh
	sp_read_roles
	sp_totals
	gid=$(sp_field "$COPY_GRP0.grp_id")
	target=$(sp_field "$COPY_GRP0.leg_list[0].leg_id")
	case "$gid$target" in
	'' | *[!0-9]*) die "slice 0's data group carried a non-decimal id" ;;
	esac
	assert_field "$SP_JSON" "$COPY_GRP0.spare_leg_list | length" 0 \
		"the group has no spare leg yet"
	before_addrs=$(sp_field \
		"$COPY_GRP0 | [.leg_list[] | .side_list[] | .addr_port] | unique | join(\",\")")
	before_vms=$(sp_field \
		"$COPY_GRP0 | [.leg_list[] | .side_list[] | $SP_SIDE_VM] | unique | join(\",\")")

	ctl_ok spare create --grp "$gid"
	spare=$(jq_of "$CTL_OUT" '.leg_id')
	case "$spare" in
	'' | *[!0-9]* | 0) die "spare create returned leg_id '$spare'" ;;
	esac

	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.spare_leg_list | length" 1 \
		"the group holds one spare leg"
	assert_field "$SP_JSON" "$COPY_GRP0.spare_leg_list[0].leg_id" "$spare" \
		"and it is the leg CreateSpareLeg minted"
	assert_field "$SP_JSON" "$COPY_GRP0.spare_leg_list[0].side_list | length" 1 \
		"a spare leg has exactly one side"
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list | length" "$LEGS" \
		"the ACTIVE leg list is unchanged: a spare is not an md member"
	spareaddr=$(sp_field "$COPY_GRP0.spare_leg_list[0].side_list[0].addr_port")
	sparevm=${spareaddr%:*}
	case ",$before_addrs," in
	*",$spareaddr,"*)
		die "the spare landed on $spareaddr, a disk node the group already" \
			"occupies; grpDnAddrs seeds the black list" \
			"(gateway/spareleg.go:225, gateway/alloc.go:545-556)"
		;;
	esac
	if [ "$DN_VM_CNT" -gt "$LEGS" ]; then
		case ",$before_vms," in
		*",$sparevm,"*)
			die "the spare landed on a DN VM the group already occupies" \
				"($before_vms). §6.5 tier 1 excludes the group's locations" \
				"(gateway/spareleg.go:215) and relaxes only when no DN" \
				"outside them can take the group's ext_cnt"
			;;
		esac
	else
		log "  $DN_VM_CNT DN VM(s) and $LEGS leg(s) per group: §6.5 tier 1" \
			"has nowhere anti-affine to go, so the spare's VM is not asserted"
	fi
	log "  spare leg $spare on $spareaddr; the group's active legs are" \
		"on $before_addrs"

	# A spare's side is created provisioned = false and only the sp-worker
	# flips it, after the DN has zeroed the whole side ([D15]) — which is
	# exactly why model.SwitchSpareLeg refuses an unprovisioned spare ("spare
	# side is not provisioned", model/ops.go:1878-1880). So the wait is a
	# precondition of the next call, not a nicety — and it is the reason
	# SP_UNPROV_CNT walks SP_ANY_SIDE_PATH: a poll over the active legs alone
	# would answer 0 the instant `spare create` returned.
	SIDES_LEFT=-1
	wait_until "$WAIT_PROVISION" "the spare leg's side to be provisioned" \
		sp_sides_provisioned
	SP_JSON=$CTL_OUT
	sp_totals
	assert_eq "$SP_SPARE_TOTAL" 1 "the sp holds exactly one spare leg"
	stack_wait_reset
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to connect the spare leg too" \
		cntlr_full_ready "$PRIMARY_CNTLR_ID"
	stack_wait_reset
	wait_until "$WAIT_PROVISION" \
		"the standby cntlr $STANDBY_CNTLR_ID to connect the spare leg too" \
		cntlr_legs_full_ready "$STANDBY_CNTLR_ID"

	# The swap. Naming both sides explicitly is what lets the reply be
	# asserted: curr_active_leg_id is the spare and curr_spare_leg_id the
	# target, read back from the request by the handler
	# (gateway/spareleg.go:374-382).
	ctl_ok spare switch --grp "$gid" --spare "$spare" --target "$target"
	assert_field "$CTL_OUT" '.curr_active_leg_id' "$spare" \
		"SwitchSpareLegReply.curr_active_leg_id"
	assert_field "$CTL_OUT" '.curr_spare_leg_id' "$target" \
		"SwitchSpareLegReply.curr_spare_leg_id"
	sp_refresh
	assert_jq "$SP_JSON" \
		"($COPY_GRP0
		  | [.leg_list[] | select(.leg_id == \"$spare\")] | length) == 1" \
		"the promoted spare is in the group's ACTIVE leg list"
	assert_jq "$SP_JSON" \
		"($COPY_GRP0 | [.spare_leg_list[] | .leg_id]) == [\"$target\"]" \
		"and the leg it replaced is the one parked in spare_leg_list"
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list | length" "$LEGS" \
		"the group still has $LEGS active legs"

	# md rebuilds onto the promoted spare. grp_md_clean reads mdadm's own
	# State line out of the row's details, because RES_STATUS_OK alone is
	# true throughout a recovery — and it is given the SpRev the switch
	# bumped to, because the leg COUNT did not change and the pre-switch
	# array also reads "clean".
	rev=$(sp_field '.sp_rev.revision')
	case "$rev" in
	'' | *[!0-9]* | 0) die "\`sp get\` reports sp_rev.revision '$rev'" ;;
	esac
	MD_STATE_LAST=""
	wait_until "$WAIT_PROVISION" \
		"cn$PRIMARY_CN to apply revision $rev and md to finish rebuilding group $gid onto leg $spare" \
		grp_md_clean "$PRIMARY_CNTLR_ID" "$gid" "$rev"
	check_sha0 "after a spare leg was switched in and md rebuilt onto it"

	# Releasing the parked leg. DeleteSpareLeg names both ids: --grp says
	# which group holds the spare list and --leg which entry goes away, and a
	# leg a switch just parked is exactly what it is for (ctl/spare.go).
	ctl_ok spare delete --grp "$gid" --leg "$target"
	assert_field "$CTL_OUT" '.leg_id' "$target" "DeleteSpareLegReply.leg_id"
	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.spare_leg_list | length" 0 \
		"the group holds no spare leg again"
	sp_totals
	assert_eq "$SP_SPARE_TOTAL" 0 "the sp holds no spare leg"
	stack_wait_reset
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to drop the released leg's row" \
		cntlr_full_ready "$PRIMARY_CNTLR_ID"
	check_sha0 "after the parked leg was released"
}

case_copy() {
	CASE=copy
	copy_transfer
	copy_clone
	copy_xfer_delete
	copy_migration
	copy_spare
	case_finish
}

# ---------------------------------------------------------------------------
# Case: react (§7.5) — the four automatic reactions, AR5 to AR8
# ---------------------------------------------------------------------------
#
# The worker's reactions, each triggered by a real fault and each asserted from
# the record the reaction actually writes. One pass per SP per cntlr_interval
# (5 s, common.DefaultHealthCheckInterval through
# health_check_conf.cntlr_interval) and AT MOST ONE ACTION PER PASS
# (worker/reaction.go:1-31), so every wait below is "threshold + a few passes"
# and never a sleep.
#
# ===========================================================================
# WHAT EACH REACTION NEEDS TO BE TRIGGERED AT ALL — and this is where §7.5's
# wording is wrong twice, in ways that would have cost a whole lab run
# ===========================================================================
#
#  AR6 (thin-pool auto-grow). tryGrow walks the slices in slice_id_list order
#  and compares the PRIMARY's own `dmsetup status` numbers:
#  `usedData * 100 > lwm * totalData`, lwm =
#  bdev_conf.dm_pool_conf.low_water_mark_pct (worker/reaction.go:857-900, the
#  comparison itself at :890). So the trigger is a THIN-POOL occupancy rather
#  than a byte count, and this case computes how many 1 MiB chunks it has to
#  write from the pool's own used/total pair instead of trusting §7.5's
#  literal "40".
#
#  AR5 (primary failover). tryFailover fires once the primary's err_epoch is
#  primary_unhealthy seconds old (worker/reaction.go:718-721). A cntlr's
#  err_epoch is set by the health monitor when the CN agent's stream cannot be
#  opened, so killing the agent is enough — the control path is what AR5
#  watches.
#
#  AR7 (cntlr replacement). replaceTarget takes the smallest cntlr_id that has
#  been unhealthy for cntlr_unhealthy, is not disabled, and is either not the
#  primary or is the primary of an SP with no failover candidate
#  (worker/reaction.go:1086-1105). THAT IS THE CHAIN §9's risk note names: with
#  a healthy standby present, AR7 refuses to touch the primary until AR5 has
#  moved the role away — and model.ReplaceCntlr refuses it a second time inside
#  its own STM ("failover candidate exists", model/ops.go:1464-1466). So this
#  case asserts AR5 by THE PRIMARY FLAG MOVING (the old record survives, with
#  primary = false — model.Failover writes the two `primary` fields at
#  model/ops.go:1062-1065 and then BumpSpRev, and touches nothing else) and
#  only then asserts AR7, which is what makes the two distinguishable at all:
#  a cntlr COUNT never changes at AR5 and changes at AR7.
#
#  AR8 (leg repair). legNeedsRepair REQUIRES `Leg.err_epoch != 0` before either
#  threshold is even looked at, and its own comment says why: "a side the
#  worker cannot reach while the primary still sees the leg healthy triggers
#  nothing" (worker/reaction.go:1267-1281). A leg's err_epoch comes from the
#  PRIMARY's §3.6 probe, which is a real write+O_DIRECT read of the leg's
#  health block through the leg wrapper (agent/cnagent/healthcheck.go:216-239,
#  CN28 at :254-280).
#  ⇒ **KILLING THE DN AGENT IS NOT ENOUGH.** An agent's nvmet subsystem, its
#  port and its dm-linear live in the KERNEL and outlive the process that
#  created them, so with the agent dead the CN's probe IO still succeeds, the
#  leg stays RES_STATUS_OK, err_epoch stays 0 and AR8 never fires: §7.5's
#  "kill one dnagent … after 30 s + intervals the worker has created a spare"
#  would simply run out its budget. This case therefore kills the agent AND
#  drops that instance's nvmet port (the helper's `port_drop`, which is exactly
#  what the start cleanup would do to it), which is what a dead DN VM looks
#  like from both planes: the gRPC rounds fail (side err_epoch, side_unhealthy
#  = $THR_SIDE) and the data path goes away (leg err_epoch, and the CNs connect
#  with --fast_io_fail_tmo common.DefaultNvmeFastIoFailTmo = 5 and
#  --ctrl-loss-tmo -1, agent/nvmehost.go:47-56, so the probe fails fast instead
#  of hanging).
#
# ===========================================================================
# THE ONE HOST-SIDE HAZARD THIS CASE HAS TO DISARM ITSELF
# ===========================================================================
# The same fact — nvmet objects outlive their agent — means the dead CN KEEPS
# EXPORTING $SS0 with ns 1 in the optimized ANA group, because nothing is left
# to rewrite `ana_grpid`. The moment AR5 promotes the standby, host0 would hold
# TWO optimized paths to one namespace: one to the new primary and one into a
# thin pool whose agent is gone. A read would be answered by either; a WRITE
# down the stale path would allocate blocks in a dm-thin metadata image the new
# primary also owns, which is corruption, not a failed assertion.
#
# So this case drops host0's connection to $SS0 BEFORE it kills the agent and
# reconnects it to the new primary alone (the helper's `disconnect_prefix` and
# `connect` verbs, the same pair copy step 1 uses for host1). A real node
# failure would have taken that path down by itself; the suite does it by hand
# because it only killed a process. From AR7 on, `connect_all` is safe again —
# ReplaceCntlr rewrites the CdcEntries (model/ops.go:1536-1541), so the dead
# CN's transport is no longer in any discovery log and cannot be reconnected.
#
# ===========================================================================
# WHAT IS NOT ASSERTED, DELIBERATELY
# ===========================================================================
#  * That AR6's new group avoids the DNs the slice already occupies. §7.5 says
#    "the new group's legs sit on two DNs outside the old group" and the code
#    says the opposite in as many words: runGrow passes a NIL black list and a
#    nil location exclusion, "the black list starts empty — a new group may
#    perfectly well land on a DN that already carries another group of this SP"
#    (worker/reaction.go:946-953). What DOES hold, and is asserted, is the
#    per-group rule: one scan keeps at most one candidate per location
#    (model/alloc.go:137-141) and pickDistinct then dedupes by addr_port
#    (worker/reaction.go:1523-1542), so the LEGS legs of the NEW group are on
#    LEGS different DNs on LEGS different DN VMs.
#  * That the old primary's namespace goes ANA-inaccessible after AR5. Nothing
#    can make it: its agent is dead (see above). The failover is asserted on the
#    record and on the NEW primary serving the data.
#  * That the parked leg AR8 leaves behind is removed. `sp delete` does not
#    list spare legs among its five blockers, and the drain releases them
#    explicitly (model/drain.go:369-375 legsOf, "active legs first, spares
#    after … both are released"), so case_residue's capacity check covers it.
#
# ===========================================================================
# THIS CASE'S OWN BUILD MAY HAVE REACTED BEFORE THE CASE STARTS
# ===========================================================================
# react is the one case whose sp carries the REACTING threshold set (§7.1):
# primary_unhealthy 5, cntlr_unhealthy 20, side_unhealthy 20, leg_unhealthy 30.
# It has to — AR7 and AR8 are unobservable at the gateway defaults of 600 and
# 1200 inside any bound this suite could wait out, and no RPC changes a
# threshold after CreateStoragePool, so the values the case needs are the values
# its build runs under.
#
# The consequence is measured, not hypothetical. Building this shape kept the
# primary CN spawning 126,657 processes over 8m45s (82,099 dmsetup, 20,127
# mdadm, 16,795 lsblk) on a 2-vCPU guest — the whole window, restarts included,
# see WAIT_BUILD — and under that load it cannot answer a health check inside
# five seconds. The first real run recorded, all inside setup:
#
#     "kind":"failover","old_cntlr_id":1,"new_cntlr_id":2
#     "kind":"spare_create","slice_id":47,"grp_id":53,"leg_id":56,"spare_leg_id":355
#     "kind":"failover","old_cntlr_id":2,"new_cntlr_id":1
#
# So when this case starts, the sp MAY already hold a spare leg it did not ask
# for, and the primary MAY be a different controller than the one the create
# elected. That is expected and it is not a bug. Every assertion below is
# therefore written against a SNAPSHOT of the shape the case actually starts
# from (react_snapshot, and the per-step `before` readings each step takes for
# itself), never against a count that assumes a pristine build:
#
#   * the primary is read through PRIMARY_CNTLR_ID after a fresh sp_read_roles,
#     so which controller it is has never mattered here;
#   * AR6's proof is "slice 0 gained one data group", not "slice 0 has two";
#   * AR8's proof is "the group gained one spare and then parked the dead leg
#     in it", not "the group holds exactly one spare".
#
# The one thing a snapshot cannot rescue is a build that never converges: if the
# failover loop keeps handing the role back and forth, each new primary starts
# the whole build again. That is a real risk of running this case at 32 slices
# and it is bounded, not hidden — setup_wait_stack follows the role through both
# of its waits and re-runs the pair when the role moves under them, up to
# SETUP_STACK_ROUNDS times, and then dies saying the sp is not converging rather
# than timing out on a target that has moved. It shouts when a build did not
# produce the fresh-sp shape, and the shape-poll shouts when the shape moves
# under a wait.
# ---------------------------------------------------------------------------

# --- the objects this case creates ------------------------------------------
#
# a0 is §7.5's own name for the AR6 target and is a bare jq identifier like
# every other td name here (`td list` is keyed by td_name). t0/s0/t1/c0 are
# taken by setup, ops and copy; a0 was left free for this case.
TD_REACT=a0
REACT_TD_ID=""
REACT_TD_SIZE=""

# The namespace that carries it. It is ns idx 2 of $SS0, like copy's, but the
# id gets its OWN global: copy's NS2_ID still holds the id its case minted, and
# a stale id silently compared against a fresh reply is the kind of assertion
# that passes for the wrong reason.
REACT_NS2_ID=""

# host0's two reference patterns. PATTERN0/SHA0 (setup's) stay the baseline
# check_sha0 reads; these are this case's own.
#   REACT_PAT   ONE MiB of /dev/urandom, written at every strided offset of
#               the AR6 trigger, so the read-back is the digest of that one
#               chunk repeated — computed on the host from the file itself.
#   REACT_PAT2  $BASELINE_MIB MiB, the fresh write AR5 makes through the NEW
#               primary.
REACT_PAT=""
REACT_PAT_SHA=""
REACT_PAT2=""
REACT_PAT2_SHA=""

# --- what the steps hand each other -----------------------------------------

# The AR6 target: slice 0, its id, and the numbers the trigger is computed from.
REACT_SLICE0_ID=""
REACT_LWM=""
REACT_CHUNKS=0
REACT_STRIDE_MIB=0

# The thin-pool row react_pool_read last read (its three fields, in the parent
# shell so the arithmetic below can use them).
REACT_POOL_STATUS=""
REACT_POOL_USED=""
REACT_POOL_TOTAL=""

# AR5: the primary as it was BEFORE the kill. Everything about the reaction is
# asserted against these, so they are read once and never recomputed.
REACT_OLD_PRIMARY_ID=""
REACT_OLD_PRIMARY_ADDR=""
REACT_OLD_PRIMARY_TRADDR=""
REACT_OLD_PRIMARY_TRSVCID=""
REACT_OLD_PRIMARY_SLOT=""
REACT_OLD_PRIMARY_CN=-1
# The standby AR5 must elect: with $CNTLR_CNT cntlrs it is the only candidate,
# and AR5 takes the smallest cntlr_id among the healthy, enabled, non-primary
# ones (worker/reaction.go:623-636, failoverEligible at :643-650).
REACT_OLD_STANDBY_ID=""
# The CN(s) that carried no cntlr when the kill happened — where AR7's
# replacement must land, because a CN with a cntlr of this SP is excluded by
# §6.4 (otherCntlrAddrs) and the dead one is black-listed by AR7 itself
# (worker/reaction.go:1049-1055). REACT_SPARE_CN is the first of them, for the
# log line; REACT_SPARE_ADDRS is the whole set as a ,-delimited string with
# leading and trailing commas, because with more than three --cn guests
# model.PickRandom may take any of them and an equality against one would be a
# false failure.
REACT_SPARE_CN=-1
REACT_SPARE_ADDRS=""

# The cntlr AR5 elected in step 3. Steps 3 and 4 are both written about it —
# step 3 waits for ITS rebuild and connects host0 to ITS transport, step 4
# asserts that the replacement is the OTHER cntlr and is a standby — so a second
# AR5 during either one takes the case's subject away. Recorded so that both
# steps can say so and stop, rather than wait out a target that has moved.
REACT_NEW_PRIMARY_ID=""

# AR7: the cntlr that replaced the dead one.
REACT_REPL_POS=-1
REACT_REPL_ID=""
REACT_REPL_ADDR=""
REACT_REPL_TRADDR=""

# AR8: the leg this case kills, and the spare the worker puts in its place.
REACT_GRP_ID=""
REACT_LEG_ID=""
REACT_LEG_POS=-1
REACT_SIDE_ID=""
REACT_SIDE_ADDR=""
REACT_DN_VM=-1
REACT_DN_INST=-1
REACT_SPARE_LEG=""

# The "last observation" scratch of this section's predicates, same discipline
# as ANA_LAST / SHA_LAST / MIGR_HYD_LAST: each is logged from inside its own
# predicate when the value CHANGES and is never interpolated into a wait_until
# label, because a label is expanded once, at the call.
REACT_CHUNK_LAST=""
REACT_POOL_LAST=""
REACT_ROLE_LAST=""
REACT_SPARE_LAST=""

# --- the shape this case starts from (see the header) -----------------------
#
# react_snapshot fills these from one `sp get` at the top of the case: the
# answer to "what did this case's own build leave behind", which under the
# reacting threshold set is not necessarily the fresh-sp shape.
#
# WHAT READS THEM, exactly — because "every later assertion is written against
# the snapshot" would be the wrong sentence. ONE of the five is read by a later
# assertion: REACT_BASE_SLICE0_DATA, by step 01. The other four are recorded for
# the log line and for react_snapshot's own `!!!` fresh-shape shout, and nothing
# else touches them. Every step that is judged on a DELTA takes its own `before`
# reading immediately before the act it judges — react_grow's before_grps and
# before_data, react_leg_repair's before_spares/before_spare_cnt/before_sp_spares
# — because the shape can move between stage 00 and that act under this case's
# thresholds, and a delta measured from stage 00 would then be a delta against
# the wrong baseline.
#
# REACT_BASE_SPARE_TOTAL   spare legs in the WHOLE sp (log and shout only)
# REACT_BASE_GRP_TOTAL     groups in the whole sp (log and shout only)
# REACT_BASE_LEG_TOTAL     active legs in the whole sp (log and shout only)
# REACT_BASE_PRIMARY_ID    the cntlr that is primary when the case starts. It is
#                          recorded for the log and for the one sentence a
#                          reader needs — which controller built the stack every
#                          early step reads — and NOT compared against anything:
#                          every step re-reads the roles for itself, because AR5
#                          is the thing under test.
# REACT_BASE_SLICE0_DATA   slice 0's data-group count, i.e. the index the group
#                          AR6 appends will occupy (GrowSlice appends). The one
#                          value a later step asserts against (step 01).
REACT_BASE_SPARE_TOTAL=0
REACT_BASE_GRP_TOTAL=0
REACT_BASE_LEG_TOTAL=0
REACT_BASE_PRIMARY_ID=""
REACT_BASE_SLICE0_DATA=0

# Slice 0's data group AR6 appends. The one setup created is $COPY_GRP0, which
# the copy section defines as '.slice_list[0].data_grp_list[0]' and whose
# contract says to reuse it rather than spell the walk again. A grow APPENDS
# (model.GrowSlice), so the new group's index is the count BEFORE the grow —
# 1 on a pristine build, and react_grow computes it rather than assuming it,
# because a build that grew slice 0 by itself would make the literal wrong.
REACT_GRP1=""

# A sanity cap on the AR6 write. The number of chunks is computed from the
# pool, so this only catches a shape whose pool is so large that filling it to
# the low-water mark would blow the §7.8 per-file cap ($DN_CAP_BYTES, 256 MiB):
# every chunk is 1 MiB on EVERY leg of the group, so 128 chunks is 128 MiB per
# backing file — half the cap, with the md bitmap and thin metadata still to
# come.
REACT_MAX_CHUNKS=128

# ---------------------------------------------------------------------------
# Strided host IO (D18) — the shape that puts every chunk in ONE slice
# ---------------------------------------------------------------------------
#
# WHY THE OFFSETS LAND IN SLICE 0, re-derived from the table the cn agent
# builds rather than from §7.5: raid0Args emits one dm-striped stripe per
# slice, in slice_idx order, with a chunk size of
# dm_raid0_conf.stripe_size / 512 sectors (agent/cnagent/td.go:19-39, over
# plan.slices which cntlrPlan sorts by slice_idx, agent/cnagent/plan.go:447-452).
# dm-striped sends chunk c of the device to stripe c mod N, so with N =
# $SLICE_CNT stripes the td offsets k x ($SLICE_CNT x stripe) are chunks
# k x $SLICE_CNT and every one of them is stripe index 0 — the slice with
# slice_idx 0, which setup pinned as .slice_list[0] (gateway/alloc.go:478-493
# returns the slices in slice_id_list order, i.e. slice_idx ascending, and
# setup asserts the 0..n-1 numbering).
#
# Inside that slice's thin volume the same offsets are k x stripe apart, so
# with stripe == the pool's data_block_size == 1 MiB each chunk is exactly one
# new thin block. This step asserts both equalities rather than deriving a
# general formula for a shape this file cannot produce.
#
# These three are host_sha_range / host_write_range (section 2, from
# cnagent_test.sh:522-534) with a stride: ONE ssh per operation, a C-style bash
# loop (no `seq`: nothing outside $HOST_TOOLS may be used on a host), `bs=1M
# count=1` per chunk, conv=fsync on the writes and NEVER an iflag=/oflag=
# (rule 1). `\$((…))` is what reaches the guest as `$((…))`; the driver expands
# only $cnt, $stride and $start.

react_chunk_write() { # <h> <src> <dst> <cnt> <strideMiB> <startMiB>
	local h=$1 src=$2 dst=$3 cnt=$4 stride=$5 start=$6 cmd
	cmd="for ((k = 0; k < $cnt; k++)); do"
	cmd="$cmd dd if=$src of=$dst bs=1M count=1"
	cmd="$cmd seek=\$(($start + k * $stride))"
	cmd="$cmd conv=fsync status=none || exit 1;"
	cmd="$cmd done; sync"
	ssh_host "$h" "$cmd" ||
		die "host$h: writing $cnt x 1 MiB to $dst at ${start}+k*${stride} MiB failed"
}

# react_chunk_sha digests the CONCATENATION of the same chunks. A stride of 0
# reads the first MiB <cnt> times, which is how the reference digest of "one
# pattern written at <cnt> offsets" is computed from the pattern file itself.
#
# A dd that fails inside the loop leaves the pipeline's digest short, so the
# comparison fails; it cannot report success on a partial read.
react_chunk_sha() { # <h> <path> <cnt> <strideMiB> <startMiB>
	local h=$1 path=$2 cnt=$3 stride=$4 start=$5 cmd
	cmd="for ((k = 0; k < $cnt; k++)); do"
	cmd="$cmd dd if=$path bs=1M count=1"
	cmd="$cmd skip=\$(($start + k * $stride)) status=none;"
	cmd="$cmd done | sha256sum | cut -d' ' -f1"
	ssh_host "$h" "$cmd"
}

# The predicate + bounded wait, because every data check in this file is a WAIT:
# a mutator returns as soon as the gateway has written etcd, and an automatic
# reaction is observed even later.
react_chunks_are() { # <h> <path> <cnt> <strideMiB> <startMiB> <want>
	host_drop_caches "$1" >/dev/null || return 1
	REACT_CHUNK_LAST=$(react_chunk_sha "$1" "$2" "$3" "$4" "$5") || return 1
	[ "$REACT_CHUNK_LAST" = "$6" ]
}

react_wait_chunks() { # <h> <path> <cnt> <stride> <start> <want> <secs> <label>
	REACT_CHUNK_LAST=""
	wait_until "$7" "$8" react_chunks_are "$1" "$2" "$3" "$4" "$5" "$6"
	log "  host$1: $3 x 1 MiB from ${5} MiB of $2, stride $4 MiB," \
		"digest $REACT_CHUNK_LAST"
}

# ---------------------------------------------------------------------------
# The thin-pool status line — AR6's own numbers
# ---------------------------------------------------------------------------
#
# The primary puts the raw `dmsetup status` line of each pool into the details
# of its slice_id_to_dm_pool row, and the worker parses it with
# parseThinPoolStatus (worker/reaction.go:770-797): it LOCATES the `thin-pool`
# token rather than counting a fixed column, because the line may or may not
# carry the device name and the start/length pair, and then reads
#
#	<transaction_id> <used_meta>/<total_meta> <used_data>/<total_data> …
#
# as the three fields after it. This awk is that rule, one-for-one — offset 2
# is the metadata ratio and offset 3 the data ratio — and it answers the
# sentinel `none` rather than "" when the line does not parse, which is exactly
# the case AR6 skips and logs ("an unparsable line is skipped", :815-841).
react_pool_frac() { # <raw dmsetup status details> <2 = meta, 3 = data>
	local out
	out=$(printf '%s\n' "$1" | awk -v off="$2" '
		{
			for (i = 1; i <= NF; i++) {
				if ($i == "thin-pool" && i + off <= NF) {
					print $(i + off)
					exit
				}
			}
		}')
	printf '%s' "${out:-none}"
}

# react_pool_read fills the three globals from ONE `cntlr inspect`. It runs in
# the parent shell (ctl_ok), so the arithmetic that follows can use them; a row
# that is missing or unparsable is a die, because every number this step
# computes afterwards would otherwise be built on a guess.
react_pool_read() { # <cntlr id> <slice id>
	local det frac
	ctl_ok cntlr inspect --id "$1"
	REACT_POOL_STATUS=$(jq_of "$CTL_OUT" \
		".cntlr_info.slice_id_to_dm_pool[\"$2\"].status // \"absent\"")
	det=$(jq_of "$CTL_OUT" \
		".cntlr_info.slice_id_to_dm_pool[\"$2\"].details // \"\"")
	frac=$(react_pool_frac "$det" 3)
	case "$frac" in
	*/*) ;;
	*)
		die "cntlr $1 slice $2: the thin-pool row's details do not carry a" \
			"<used>/<total> data ratio — status $REACT_POOL_STATUS," \
			"details '$det'"
		;;
	esac
	REACT_POOL_USED=${frac%%/*}
	REACT_POOL_TOTAL=${frac##*/}
	case "$REACT_POOL_USED$REACT_POOL_TOTAL" in
	'' | *[!0-9]*)
		die "cntlr $1 slice $2: thin-pool data ratio '$frac' is not numeric"
		;;
	esac
	[ "$REACT_POOL_TOTAL" -gt 0 ] ||
		die "cntlr $1 slice $2: thin-pool data total is 0"
	log "  slice $2's pool on cntlr $1: $REACT_POOL_STATUS," \
		"data $REACT_POOL_USED/$REACT_POOL_TOTAL blocks"
}

# ---------------------------------------------------------------------------
# Predicates
# ---------------------------------------------------------------------------
#
# Every one of them polls through ctl_try in the PARENT shell, so the reply the
# successful poll returned is still in $CTL_OUT for the assertions that follow,
# and every number is validated with a case guard before it reaches `[ -ge ]` —
# a jq filter that matched nothing answers `null`, and `[ null -ge 2 ]` is a
# shell error, not a false.

# react_grow_progress is AR6's wait AND its diagnosis in one predicate: the
# answer is the slice's data-group count, and every poll that is still waiting
# re-reads the pool row the worker itself is looking at, so a timeout leaves
# the used/total pair that explains it in the transcript rather than in a
# `cntlr inspect` somebody has to run afterwards. On the poll that SUCCEEDS
# $CTL_OUT is the `sp get` reply, because the pool read is skipped there.
react_grow_progress() { # <cntlr id> <slice id> <want data grps>
	local n st det frac sig
	if ! ctl_try sp get; then
		return 1
	fi
	n=$(jq_of "$CTL_OUT" '.slice_list[0].data_grp_list | length')
	case "$n" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$n" -ge "$3" ]; then
		return 0
	fi
	if ctl_try cntlr inspect --id "$1"; then
		st=$(jq_of "$CTL_OUT" \
			".cntlr_info.slice_id_to_dm_pool[\"$2\"].status // \"absent\"")
		det=$(jq_of "$CTL_OUT" \
			".cntlr_info.slice_id_to_dm_pool[\"$2\"].details // \"\"")
		frac=$(react_pool_frac "$det" 3)
		sig="$n data group(s), pool $st, data $frac"
		if [ "$sig" != "$REACT_POOL_LAST" ]; then
			REACT_POOL_LAST=$sig
			log "  slice $2: $sig"
		fi
	fi
	return 1
}

# react_note_grp_dns registers every disk node of one group with the failure
# dump (§7.9), so a step that is about to depend on those DNs gets their
# `dn inspect` and their whole agent log if anything below it fails.
react_note_grp_dns() { # <jq group fragment>
	local addrs addr v k
	addrs=$(sp_field "[$1.leg_list[] | .side_list[] | .addr_port] | .[]")
	for addr in $addrs; do
		v=$(dn_vm_of_addr "$addr")
		k=$(dn_inst_of_addr "$addr")
		if [ "$v" = none ] || [ "$k" = none ]; then
			die "a side's addr_port '$addr' names no --dn guest and instance"
		fi
		diag_note_dn "$v" "$k"
		log "  side on dn$v instance $k ($addr)"
	done
}

# react_pool_grew is the proof that the grow reached the DEVICE and not only
# etcd: dm-thin reports the pool's data total in its own status line, so a
# bigger total is the CN having reloaded the pool over the wider data concat.
react_pool_grew() { # <cntlr id> <slice id> <old total>
	local st det frac total sig
	if ! ctl_try cntlr inspect --id "$1"; then
		return 1
	fi
	st=$(jq_of "$CTL_OUT" \
		".cntlr_info.slice_id_to_dm_pool[\"$2\"].status // \"absent\"")
	det=$(jq_of "$CTL_OUT" \
		".cntlr_info.slice_id_to_dm_pool[\"$2\"].details // \"\"")
	frac=$(react_pool_frac "$det" 3)
	sig="pool $st, data $frac (want a total above $3)"
	if [ "$sig" != "$REACT_POOL_LAST" ]; then
		REACT_POOL_LAST=$sig
		log "  slice $2 on cntlr $1: $sig"
	fi
	[ "$st" = RES_STATUS_OK ] || return 1
	case "$frac" in
	*/*) ;;
	*) return 1 ;;
	esac
	total=${frac##*/}
	case "$total" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$total" -gt "$3" ]
}

# react_primary_moved is AR5's whole observable: EXACTLY one cntlr is primary
# and it is not the one whose agent was killed. The old record is deliberately
# not required to be gone — AR5 writes the two `primary` fields
# (model/ops.go:1062-1065) and bumps SpRev, and nothing of the record itself
# goes away; its disappearance is AR7's signature.
react_primary_moved() { # <old cntlr id>
	local prim ids i id isprim sig
	if ! ctl_try sp get; then
		return 1
	fi
	prim=$(jq_of "$CTL_OUT" '[.cntlr_list[] | select(.primary)] | length')
	case "$prim" in
	'' | *[!0-9]*) return 1 ;;
	esac
	ids=$(jq_of "$CTL_OUT" '.sp_conf.cntlr_id_list | length')
	case "$ids" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$ids" = "$(jq_of "$CTL_OUT" '.cntlr_list | length')" ] || return 1
	sig="$prim primary cntlr(s) among $ids"
	if [ "$sig" != "$REACT_ROLE_LAST" ]; then
		REACT_ROLE_LAST=$sig
		log "  $SP: $sig"
	fi
	[ "$prim" = 1 ] || return 1
	for ((i = 0; i < ids; i++)); do
		isprim=$(jq_of "$CTL_OUT" ".cntlr_list[$i].primary")
		[ "$isprim" = true ] || continue
		id=$(jq_of "$CTL_OUT" ".sp_conf.cntlr_id_list[$i]")
		if [ "$id" = "$1" ]; then
			return 1
		fi
		return 0
	done
	return 1
}

# react_new_primary_ready is step 3's rebuild wait: primary_stack_ready, which
# follows the role, plus the one thing this case cannot do with a move.
#
# WHY IT IS NOT cntlr_full_ready. The rebuild is a whole stack on a node that had
# only legs — the WAIT_BUILD comment's piece of work — and this sp's
# primary_unhealthy is $THR_REACT_PRIMARY
# seconds: the first run proved a CN doing that work misses health rounds and
# gets its err_epoch stamped. Meanwhile AR7 has minted the replacement on an idle
# CN with err_epoch 0, which is all failoverEligible asks for
# (worker/reaction.go:645-649), so AR5 has a candidate again and can move the
# role off the cntlr it just elected. Pinned to that cntlr, the wait would then
# be comparing a STANDBY against $SLICE_CNT pools and $SP_GRP_TOTAL groups —
# rows CN12 and CN13 say a standby never has — and would spend its whole
# WAIT_BUILD before dying about a node doing exactly what a standby should.
#
# WHY IT DIES INSTEAD OF FOLLOWING THE MOVE, which is where it parts company
# with setup. Setup does not care which cntlr builds the stack, so it waits for
# the new one. Step 4 does care: it is written about the cntlr AR5 elected here
# — it resolves the replacement as "the cntlr that is neither this one nor the
# dead one" and asserts the replacement is a standby — and after a second AR5
# neither sentence is true of the tree's correct behaviour. So the run stops
# here, in seconds, naming what happened, instead of timing out in twenty minutes
# and then failing step 4 on an assertion that has become a false statement.
react_new_primary_ready() { # <the cntlr AR5 elected>
	local rc=0
	primary_stack_ready || rc=1
	# PRIMARY_WAIT_ID is empty until a poll has read a judgeable reply, and
	# primary_stack_ready has already logged the move itself.
	if [ -n "$PRIMARY_WAIT_ID" ] && [ "$PRIMARY_WAIT_ID" != "$1" ]; then
		die "AR5 fired a SECOND time while cntlr $1 was rebuilding the stack:" \
			"the primary is cntlr $PRIMARY_WAIT_ID now. That is legal —" \
			"AR7's replacement is healthy, so it is a failover candidate" \
			"(failoverEligible), and the rebuild makes cntlr $1 miss the" \
			"${THR_PRIMARY}s primary_unhealthy of the $THR_SET set ($THR)." \
			"But step 4 is written about cntlr $1, so this case cannot" \
			"judge AR7 after it. Re-run react; if it repeats, the 2-vCPU CN" \
			"cannot build this shape inside primary_unhealthy at all."
	fi
	return $rc
}

# react_cntlr_replaced is AR7's: the dead cntlr's id is gone from
# cntlr_id_list (ReplaceCntlr deletes the old key and appends a NEW id,
# model/ops.go:1502-1532) and the SP is back to its full cntlr count.
react_cntlr_replaced() { # <old cntlr id>
	local gone n
	if ! ctl_try sp get; then
		return 1
	fi
	gone=$(jq_of "$CTL_OUT" \
		"[.sp_conf.cntlr_id_list[] | select(. == \"$1\")] | length")
	n=$(jq_of "$CTL_OUT" '.cntlr_list | length')
	case "$gone$n" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$gone" = 0 ] && [ "$n" = "$CNTLR_CNT" ]
}

# react_cn_has_no_cntlr reads the NODE record, not the sp: releaseCn takes the
# (sp_id, cntlr_id) pointer out of the CN's cntlr_ptr_list and gives the
# footprint back (model/ops.go:1549-1575), so an empty list is the CN's own
# answer that it no longer hosts a controller of any sp.
react_cn_has_no_cntlr() { # <cn addr_port>
	local n
	if ! ctl_try cn get --addr "$1"; then
		return 1
	fi
	n=$(jq_of "$CTL_OUT" '.cn_conf.cntlr_ptr_list | length')
	case "$n" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$n" = 0 ]
}

react_spare_cnt_is() { # <want>
	local n
	if ! ctl_try sp get; then
		return 1
	fi
	n=$(jq_of "$CTL_OUT" "$COPY_GRP0.spare_leg_list | length")
	case "$n" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$n" != "$REACT_SPARE_LAST" ]; then
		REACT_SPARE_LAST=$n
		log "  group $REACT_GRP_ID holds $n spare leg(s)"
	fi
	[ "$n" = "$1" ]
}

# react_spare_switched is AR8 step 1 seen from the record: SwitchSpareLeg puts
# the spare in the target's POSITION in leg_list and parks the target in
# spare_leg_list, keeping its err_epoch (model/ops.go:1824-1830). So the test is
# "the dead leg is no longer active AND is now parked" — both halves, because
# either one alone is also what a half-applied transaction would look like.
react_spare_switched() { # <dead leg id>
	local act park
	if ! ctl_try sp get; then
		return 1
	fi
	act=$(jq_of "$CTL_OUT" \
		"[$COPY_GRP0.leg_list[] | select(.leg_id == \"$1\")] | length")
	park=$(jq_of "$CTL_OUT" \
		"[$COPY_GRP0.spare_leg_list[] | select(.leg_id == \"$1\")] | length")
	case "$act$park" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$act" = 0 ] && [ "$park" = 1 ]
}

# --- step 0: the shape this case starts from --------------------------------
#
# One `sp get`, recorded before anything is done to the sp, and said out loud.
# Everything below compares against these numbers rather than against the
# fresh-sp constants, for the reason the section header gives: this case's own
# build runs under the reacting threshold set and may have produced a failover
# or a spare before the case begins.
react_snapshot() {
	stage 00 "the shape this case starts from (its build ran under the ${THR_SET} thresholds)"
	sp_refresh
	sp_read_roles
	sp_totals
	REACT_BASE_SPARE_TOTAL=$SP_SPARE_TOTAL
	REACT_BASE_GRP_TOTAL=$SP_GRP_TOTAL
	REACT_BASE_LEG_TOTAL=$SP_LEG_TOTAL
	REACT_BASE_PRIMARY_ID=$PRIMARY_CNTLR_ID
	REACT_BASE_SLICE0_DATA=$(sp_field '.slice_list[0].data_grp_list | length')
	case "$REACT_BASE_SLICE0_DATA" in
	'' | *[!0-9]* | 0)
		die "slice 0 reports '$REACT_BASE_SLICE0_DATA' data groups;" \
			"every slice is created with exactly one (planSpGroups," \
			"gateway/storagepool.go:254-263)"
		;;
	esac
	log "  $SP starts this case with $REACT_BASE_GRP_TOTAL groups," \
		"$REACT_BASE_LEG_TOTAL active legs and" \
		"$REACT_BASE_SPARE_TOTAL spare leg(s); slice 0 has" \
		"$REACT_BASE_SLICE0_DATA data group(s); the primary is cntlr" \
		"$REACT_BASE_PRIMARY_ID on cn$PRIMARY_CN"
	if [ "$REACT_BASE_SPARE_TOTAL" != 0 ] ||
		[ "$REACT_BASE_LEG_TOTAL" != "$((GRP_CNT * LEGS))" ] ||
		[ "$REACT_BASE_SLICE0_DATA" != 1 ]; then
		log "!!! that is NOT the fresh-sp shape ($((GRP_CNT * LEGS)) legs," \
			"0 spares, 1 data group in slice 0): a reaction fired during this" \
			"case's own build, which the reacting threshold set allows." \
			"Every assertion below is written against the numbers above."
	fi
}

# --- step 1 -----------------------------------------------------------------
#
# §7.5's "setup, plus `td create --name a0 --size $((64*TD_UNIT))` (2 GiB) and
# `ns create … --idx 2 --td a0 --uuid $UUID2` on host0", plus the four
# geometry facts the next step's arithmetic rests on — every one of them read
# back from the sp and asserted, not assumed.

react_target() {
	stage 01 "td create $TD_REACT (2 GiB, the AR6 target) and ns idx 2 for host0"
	local stripe block slicemib msg

	sp_refresh
	sp_read_roles
	sp_totals

	# "Slice 0" means slice_idx 0, and that is what the strided writes reach:
	# raid0Args fills stripe i from plan.slices[i], which cntlrPlan sorts by
	# slice_idx. sp get returns the slices in slice_id_list order, i.e.
	# slice_idx ascending (gateway/alloc.go:478-493), which setup pinned as
	# 0..slice_cnt-1 with no gap.
	assert_field "$SP_JSON" '.slice_list[0].slice_idx' 0 \
		"slice_list[0] is slice_idx 0 — the stripe every strided write lands in"
	# The id is read from sp_conf.slice_id_list, which loadSlices walks in list
	# order to build slice_list (gateway/alloc.go:478-493): position 0 of one
	# is position 0 of the other. pb.Slice itself carries no id
	# (pb/schema.proto:405-409), and slice_id is what
	# cntlr_info.slice_id_to_dm_pool is keyed by (pb/schema.proto:237).
	REACT_SLICE0_ID=$(sp_field '.sp_conf.slice_id_list[0]')
	case "$REACT_SLICE0_ID" in
	'' | *[!0-9]* | 0)
		die "sp_conf.slice_id_list[0] is '$REACT_SLICE0_ID', not a decimal id"
		;;
	esac
	# Slice 0's data-group count is the SNAPSHOT's, not the literal 1: step 02
	# asserts that AR6 appended ONE group to whatever was there, which is the
	# statement about AR6, and a build that had already grown slice 0 by itself
	# must not fail the case here. react_snapshot has already shouted if the
	# count is not 1.
	assert_field "$SP_JSON" '.slice_list[0].data_grp_list | length' \
		"$REACT_BASE_SLICE0_DATA" \
		"slice 0's data-group count is still the one react_snapshot recorded"
	REACT_GRP_ID=$(sp_field "$COPY_GRP0.grp_id")
	case "$REACT_GRP_ID" in
	'' | *[!0-9]* | 0)
		die "slice 0's first data group has grp_id '$REACT_GRP_ID'"
		;;
	esac

	# The two equalities the chunk arithmetic needs. They are ASSERTED rather
	# than generalised: with stripe == data_block_size == 1 MiB one strided
	# 1 MiB write is exactly one new thin block, which is the unit AR6's
	# used/total pair counts in. A different stripe would still land in slice 0
	# (the mapping is chunk c -> stripe c mod N for any chunk size) but would
	# no longer make the count a block count. Both values are shape choices of
	# THIS file rather than immovable facts: $STRIPE_SIZE is the constant
	# setup passes as `sp create --stripe-size`, and the block size is what the
	# gateway resolves when no --block-size is given — dnvctl has that flag
	# (ctl/sp.go:211-212) and this suite deliberately never passes it, so the
	# stored value is common.DefaultDmPoolDataBlockSize.
	stripe=$(sp_field '.sp_conf.bdev_conf.dm_raid0_conf.stripe_size')
	block=$(sp_field '.sp_conf.bdev_conf.dm_pool_conf.data_block_size')
	assert_eq "$stripe" "$STRIPE_SIZE" "the sp's stored dm-striped chunk"
	assert_eq "$stripe" 1048576 \
		"the react case's strided writes are written for a 1 MiB stripe"
	assert_eq "$block" 1048576 \
		"the pool's data_block_size (one strided write must be one thin block)"
	REACT_STRIDE_MIB=$((SLICE_CNT * stripe / 1048576))
	assert_eq "$REACT_STRIDE_MIB" "$SLICE_CNT" \
		"the stride in MiB: slice_cnt x stripe, which is $((SLICE_CNT * stripe)) bytes"

	# The percentage the worker compares against, read from the sp because that
	# is where tryGrow reads it (worker/reaction.go:860-867); above 100 switches
	# AR6 off entirely, which would make this whole step wait for nothing.
	REACT_LWM=$(sp_field '.sp_conf.bdev_conf.dm_pool_conf.low_water_mark_pct')
	case "$REACT_LWM" in
	'' | *[!0-9]*)
		die "low_water_mark_pct reads '$REACT_LWM' (uint32, a bare number)"
		;;
	esac
	msg="dm_pool_conf.low_water_mark_pct: a 0 is refused by the pass gate and"
	msg="$msg anything above 100 switches AR6 off (worker/reaction.go:860-867)"
	assert_between "$REACT_LWM" 1 100 "$msg"

	# D27: a0 is 2 GiB = 64 x TD_UNIT, so each slice's thin volume is
	# TD_REACT_SIZE / slice_cnt — 64 MiB at the default shape, which is the
	# ceiling on how many 1 MiB chunks the next step may write into slice 0.
	REACT_TD_SIZE=$((64 * TD_UNIT))
	slicemib=$((REACT_TD_SIZE / SLICE_CNT / 1048576))
	assert_ge "$slicemib" 1 \
		"$TD_REACT's per-slice thin volume must hold at least one 1 MiB chunk"

	ctl_ok td create --name "$TD_REACT" --size "$REACT_TD_SIZE"
	REACT_TD_ID=$(jq_of "$CTL_OUT" '.td_id')
	case "$REACT_TD_ID" in
	'' | *[!0-9]* | 0) die "td create returned td_id '$REACT_TD_ID'" ;;
	esac
	wait_until "$WAIT_PROVISION" "$TD_REACT to report created" \
		td_created "$TD_REACT"
	assert_field "$CTL_OUT" '.name_to_td | length' 2 \
		"the sp holds $TD0 and $TD_REACT"
	assert_field "$CTL_OUT" ".name_to_td.$TD_REACT.size" "$REACT_TD_SIZE" \
		"$TD_REACT's stored size (uint64, a JSON string)"
	# `created` is defined on the thin VOLUMES alone (ThinDeviceCreated.md R13);
	# the raid0 CN15 builds is the row a namespace's dm-linear points AT (CN16
	# rule 6), so the namespace below needs that one and not merely `created`.
	wait_until "$WAIT_PROVISION" \
		"the primary cntlr $PRIMARY_CNTLR_ID to build a raid0 for both thin devices" \
		cntlr_raid0_ready "$PRIMARY_CNTLR_ID" 2

	ctl_ok ns create --nqn "$SS0" --idx 2 --td "$TD_REACT" --uuid "$UUID2"
	REACT_NS2_ID=$(jq_of "$CTL_OUT" '.ns_id')
	case "$REACT_NS2_ID" in
	'' | *[!0-9]* | 0) die "ns create returned ns_id '$REACT_NS2_ID'" ;;
	esac
	# NOTHING CONNECTS HERE — host0 already holds the controller to $SS0 from
	# setup and the kernel picks the new namespace up on the AEN — but this is
	# still a control-plane write the host is about to depend on, so the gate
	# is the same one: `ns create` returning says the gateway committed the
	# record, not that the primary's agent has created and enabled the nvmet
	# namespace. Without it a slow converge spends host_wait_ana's whole
	# WAIT_HOST and reports an ANA state for a namespace that does not exist
	# yet; with it the failure names the row that is missing.
	wait_ns_exported "$PRIMARY_CNTLR_ID" \
		"$SS0 ns 2 ($TD_REACT) on cn$PRIMARY_CN" "$SS0_ID" "$REACT_NS2_ID"
	# ANA first, device second: a namespace whose only path has never been
	# usable gets no head disk at all (section 2's rule).
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID2" optimized
	wait_dev 0 "$UUID2"
	log "  host0 sees $(host_dev "$UUID2") for $TD_REACT;" \
		"slice 0's thin volume of it is $slicemib MiB, and one strided 1 MiB" \
		"write every $REACT_STRIDE_MIB MiB of the device lands in it"
}

# --- step 2 -----------------------------------------------------------------
#
# AR6. The chunk count is COMPUTED from the pool's own used/total pair and the
# sp's low_water_mark_pct, because that is what the worker compares
# (`usedData * 100 > lwm * totalData`, worker/reaction.go:890-891): §7.5's
# literal 40 is right for the default shape and silently wrong for any other,
# while `floor(lwm x total / 100) + 1 - used` is the number that crosses the
# mark at every shape. Four blocks of margin cover the metadata the pool
# allocates alongside the data and any block the earlier cases' writes did not
# account for.

react_grow() {
	stage 02 "AR6: fill slice 0's thin pool past low_water_mark_pct = $REACT_LWM"
	local dev need total0 ext0 slicemib msg before_grps before_data

	dev=$(host_dev "$UUID2")
	slicemib=$((REACT_TD_SIZE / SLICE_CNT / 1048576))

	# THE TWO `BEFORE` READINGS THIS STEP ASSERTS AGAINST, taken here rather
	# than inherited from step 01: everything AR6 is judged on is a DELTA of
	# one, and a delta needs a reading from just before the act that causes it.
	# before_data is also the INDEX the appended group will occupy, because
	# GrowSlice appends — which is what $REACT_GRP1 becomes, instead of the
	# literal `[1]` it used to be.
	#
	# sp_read_roles comes with them: every read below goes to $PRIMARY_CNTLR_ID
	# — the pool status AR6 itself compares, and the row react_pool_grew waits
	# on — and this case's sp carries the reacting thresholds, so the role may
	# have moved since step 01 read it. Inspecting a cntlr that is now a standby
	# would find no pool row at all and die on a missing used/total ratio.
	sp_refresh
	sp_read_roles
	sp_totals
	before_grps=$SP_GRP_TOTAL
	before_data=$(sp_field '.slice_list[0].data_grp_list | length')
	case "$before_data" in
	'' | *[!0-9]* | 0)
		die "slice 0 reports '$before_data' data groups before the AR6 write"
		;;
	esac
	REACT_GRP1=".slice_list[0].data_grp_list[$before_data]"

	react_pool_read "$PRIMARY_CNTLR_ID" "$REACT_SLICE0_ID"
	msg="slice 0's thin pool on the primary: AR6 never grows an ERROR,"
	msg="$msg PROVISIONING or absent pool (worker/reaction.go:877-881)"
	assert_eq "$REACT_POOL_STATUS" RES_STATUS_OK "$msg"
	total0=$REACT_POOL_TOTAL

	need=$((REACT_LWM * total0 / 100 + 1))
	REACT_CHUNKS=$((need - REACT_POOL_USED + 4))
	[ "$REACT_CHUNKS" -ge 1 ] || REACT_CHUNKS=1
	msg="the $REACT_CHUNKS chunk(s) that cross the low water mark must fit in"
	msg="$msg $TD_REACT's $slicemib MiB slice-0 thin volume — make $TD_REACT bigger"
	assert_le "$REACT_CHUNKS" "$slicemib" "$msg"
	msg="the write must stay INSIDE the pool: filling it would put dm-thin into"
	msg="$msg out-of-space mode instead of tripping AR6"
	assert_le "$((REACT_POOL_USED + REACT_CHUNKS))" "$((total0 - 1))" "$msg"
	msg="crossing the low water mark would write $REACT_CHUNKS MiB to EVERY leg"
	msg="$msg of the group, and §7.8 caps one backing file at $DN_CAP_BYTES bytes"
	assert_le "$REACT_CHUNKS" "$REACT_MAX_CHUNKS" "$msg"
	log "  slice 0's pool holds $REACT_POOL_USED of $total0 data blocks;" \
		"$REACT_CHUNKS x 1 MiB at every ${REACT_STRIDE_MIB} MiB of $dev takes" \
		"it past ${REACT_LWM}% (AR6 needs used x 100 > $REACT_LWM x $total0)"

	REACT_PAT="$WORK/pattern-react"
	host_make_pattern 0 "$REACT_PAT" 1
	# The reference is the SAME MiB $REACT_CHUNKS times over, digested from the
	# file itself: stride 0 makes react_chunk_sha read chunk 0 that many times.
	REACT_PAT_SHA=$(react_chunk_sha 0 "$REACT_PAT" "$REACT_CHUNKS" 0 0) ||
		die "host0: digesting the strided reference failed"
	assert_eq "${#REACT_PAT_SHA}" 64 \
		"the strided reference digest is 64 hex digits"

	react_chunk_write 0 "$REACT_PAT" "$dev" "$REACT_CHUNKS" \
		"$REACT_STRIDE_MIB" 0

	REACT_POOL_LAST=""
	msg="AR6 to append one more data group to slice 0 (the worker reads the"
	msg="$msg primary's pool status once per 5s pass)"
	wait_until "$WAIT_REACT" "$msg" \
		react_grow_progress "$PRIMARY_CNTLR_ID" "$REACT_SLICE0_ID" \
		"$((before_data + 1))"
	sp_refresh
	assert_field "$SP_JSON" '.slice_list[0].data_grp_list | length' \
		"$((before_data + 1))" \
		"slice 0 gained exactly one data group"
	assert_field "$SP_JSON" "$COPY_GRP0.grp_id" "$REACT_GRP_ID" \
		"the grow APPENDED: data_grp_list[0] is still the group setup created"
	ext0=$(sp_field "$COPY_GRP0.ext_cnt")
	msg="a data grow appends the slice's FIRST data group's ext_cnt (growExtCnt,"
	msg="$msg worker/reaction.go:1010-1036, and model.GrowSlice recomputes it)"
	assert_field "$SP_JSON" "$REACT_GRP1.ext_cnt" "$ext0" "$msg"
	assert_field "$SP_JSON" "$REACT_GRP1.leg_list | length" "$LEGS" \
		"the new group has $LEGS leg(s)"
	assert_field "$SP_JSON" "$REACT_GRP1.spare_leg_list | length" 0 \
		"and no spare leg"
	assert_jq "$SP_JSON" \
		"[$REACT_GRP1.leg_list[] | select((.side_list | length) != 1)] | length == 0" \
		"every leg of the new group has exactly one side"
	# WHERE IT LANDED. What the code guarantees is the per-group rule: a scan
	# keeps at most one candidate per location (model/alloc.go:137-141) and
	# pickDistinct then dedupes by addr_port (worker/reaction.go:1523-1542).
	# What it does NOT guarantee — and what §7.5 claims — is that the new group
	# avoids the DNs the slice already occupies: runGrow passes a nil black
	# list and a nil location exclusion (worker/reaction.go:946-953), so that
	# is deliberately not asserted here.
	assert_jq "$SP_JSON" \
		"([$REACT_GRP1.leg_list[] | .side_list[] | .addr_port]
		  | unique | length) == $LEGS" \
		"the new group's $LEGS side(s) are on $LEGS DISTINCT disk nodes"
	assert_jq "$SP_JSON" \
		"([$REACT_GRP1.leg_list[] | .side_list[] | $SP_SIDE_VM]
		  | unique | length) == $LEGS" \
		"and on $LEGS different DN VMs (one candidate per location per scan)"
	react_note_grp_dns "$REACT_GRP1"

	# The new sides have to zero before the CN can widen the pool over them.
	SIDES_LEFT=-1
	wait_until "$WAIT_PROVISION" \
		"the $LEGS new side(s) of the grown group to be provisioned" \
		sp_sides_provisioned
	SP_JSON=$CTL_OUT
	sp_totals
	# A DELTA against this step's own `before` reading, not against GRP_CNT: the
	# statement AR6 earns is "one group more than there was", and the constant
	# would be wrong for any build that had already grown a slice by itself.
	assert_eq "$SP_GRP_TOTAL" "$((before_grps + 1))" \
		"AR6 added exactly one group to the sp"

	# THE GROW REACHED THE DEVICE, not merely etcd: dm-thin reports the pool's
	# own data total, so a bigger total is the CN having reloaded the pool over
	# the wider data concat.
	REACT_POOL_LAST=""
	wait_until "$WAIT_PROVISION" \
		"the primary's slice-0 thin pool to report a data total above $total0 blocks" \
		react_pool_grew "$PRIMARY_CNTLR_ID" "$REACT_SLICE0_ID" "$total0"
	react_pool_read "$PRIMARY_CNTLR_ID" "$REACT_SLICE0_ID"
	assert_ge "$REACT_POOL_TOTAL" "$((total0 + 1))" \
		"slice 0's pool is wider than it was"
	# And the pool is no longer over its mark: the same comparison the worker
	# makes, against the NEW total, so slice 0's data is not grown a second
	# time. It says nothing about the other slices or about metadata, and it
	# does not have to: this run writes a handful of blocks into slices 1..4
	# (setup's baseline and step 3's fresh write) against the same per-slice
	# total, and metadata is counted in dm-thin's fixed 4 KiB blocks over the
	# slice's whole meta group (worker/reaction.go:112-115).
	assert_le "$((REACT_POOL_USED * 100))" "$((REACT_LWM * REACT_POOL_TOTAL))" \
		"the grown pool is back under ${REACT_LWM}% of its new total"

	# The data. Every chunk is read back from the device after a cache drop and
	# compared against the reference digest, and ns 1's baseline is re-read too
	# (§7.5's "host0 ns 1 too").
	react_wait_chunks 0 "$dev" "$REACT_CHUNKS" "$REACT_STRIDE_MIB" 0 \
		"$REACT_PAT_SHA" "$WAIT_HOST" \
		"host0 to read every strided chunk of $TD_REACT back after the grow"
	check_sha0 "after AR6 grew slice 0"
}

# --- step 3 -----------------------------------------------------------------
#
# AR5. Everything about the reaction is asserted against what the primary was
# BEFORE the kill, and the assertion is the FLAG, not a count (see the header).

react_failover() {
	stage 03 "AR5: kill the primary's cn agent and watch the standby be elected"
	local out dev2 msg spare

	sp_refresh
	sp_read_roles
	sp_totals
	REACT_OLD_PRIMARY_ID=$PRIMARY_CNTLR_ID
	REACT_OLD_PRIMARY_ADDR=$PRIMARY_ADDR
	REACT_OLD_PRIMARY_TRADDR=$PRIMARY_TRADDR
	REACT_OLD_PRIMARY_TRSVCID=$PRIMARY_TRSVCID
	REACT_OLD_PRIMARY_CN=$PRIMARY_CN
	REACT_OLD_STANDBY_ID=$STANDBY_CNTLR_ID
	REACT_OLD_PRIMARY_SLOT=$(sp_field ".cntlr_list[$PRIMARY_POS].cntlid_slot")
	REACT_SPARE_CN=$SPARE_CN
	REACT_SPARE_ADDRS=","
	for spare in "${SPARE_CN_LIST[@]:-}"; do
		[ -n "$spare" ] || continue
		REACT_SPARE_ADDRS="$REACT_SPARE_ADDRS$(cn_addr "$spare"),"
	done
	case "$REACT_OLD_PRIMARY_SLOT" in
	'' | *[!0-9]*)
		die "the primary's cntlid_slot reads '$REACT_OLD_PRIMARY_SLOT'"
		;;
	esac
	# AR7 (step 4) has to have somewhere to put the replacement: the dead CN is
	# black-listed by AR7 itself and every other cntlr's CN by §6.4, so the SP
	# needs a CN carrying no cntlr. That is D12's third CN, and §7.1 requires at
	# least three --cn guests for exactly this.
	[ "$REACT_SPARE_CN" -ge 0 ] ||
		die "every --cn guest carries a cntlr of $SP, so AR7 would have no" \
			"controller node to replace the dead cntlr on. §7.1 wants" \
			"cntlr_cnt $CNTLR_CNT and at least three --cn guests"
	log "  primary cntlr $REACT_OLD_PRIMARY_ID is on cn$REACT_OLD_PRIMARY_CN" \
		"($REACT_OLD_PRIMARY_ADDR), cntlid_slot $REACT_OLD_PRIMARY_SLOT;" \
		"the standby is $REACT_OLD_STANDBY_ID on cn$STANDBY_CN;" \
		"carrying no cntlr:${REACT_SPARE_ADDRS//,/ }"

	# THE HOST LETS GO FIRST, and this is the one act in this case the plan
	# does not ask for. Killing the agent does not remove the nvmet subsystem,
	# namespace or ANA group it created — those are kernel objects — so the
	# dead CN goes on advertising ns 1 and ns 2 as `optimized` with nothing
	# left to rewrite ana_grpid. The instant AR5 promotes the standby host0
	# would hold two optimized paths to one namespace, and a write down the
	# stale one would allocate blocks in a dm-thin metadata image the new
	# primary also owns. A real node failure takes that path down; a killed
	# process does not, so the suite does it here and reconnects to the new
	# primary alone.
	# The `||` catches ssh or the dispatch and nothing more — disconnect_prefix
	# discards every disconnect's status and ends `return 0` — so the invariant
	# above is carried by the two host_path_gone waits below, not by this line.
	helper_host 0 disconnect_prefix "$SS0" >/dev/null ||
		die "host0: the disconnect_prefix verb could not be run for $SS0," \
			"so host0 has not even been asked to let go before the kill"
	wait_until "$WAIT_HOST" \
		"host0 to drop its path to cn$REACT_OLD_PRIMARY_CN ($REACT_OLD_PRIMARY_TRADDR)" \
		host_path_gone 0 "$SS0" "$REACT_OLD_PRIMARY_TRADDR"
	wait_until "$WAIT_HOST" \
		"host0 to drop its path to the standby cn$STANDBY_CN ($STANDBY_TRADDR)" \
		host_path_gone 0 "$SS0" "$STANDBY_TRADDR"

	out=$(stop_cn_agent "$REACT_OLD_PRIMARY_CN") ||
		die "cn$REACT_OLD_PRIMARY_CN: kill_pidfile failed"
	printf '%s\n' "$out" >&2
	case "$out" in
	*STILL_RUNNING*)
		die "cn$REACT_OLD_PRIMARY_CN's agent survived SIGKILL: $out"
		;;
	esac
	out=$(helper_cn "$REACT_OLD_PRIMARY_CN" alive "$(cn_pid_file)") ||
		die "cn$REACT_OLD_PRIMARY_CN: the alive verb failed"
	assert_eq "$out" dead "cn$REACT_OLD_PRIMARY_CN's agent after the kill"

	REACT_ROLE_LAST=""
	msg="AR5 to move the primary role off cntlr $REACT_OLD_PRIMARY_ID"
	msg="$msg (primary_unhealthy ${THR_PRIMARY}s after its err_epoch, then the"
	msg="$msg next 5s pass)"
	wait_until "$WAIT_REACT" "$msg" \
		react_primary_moved "$REACT_OLD_PRIMARY_ID"
	# The predicate ran in this shell and returned on the call that succeeded.
	SP_JSON=$CTL_OUT
	sp_read_roles
	# The id equality below is only predictable at §7.1's cntlr_cnt 2, where the
	# standby is AR5's only candidate; with more cntlrs the election takes the
	# smallest cntlr_id and STANDBY_CNTLR_ID is merely the first non-primary in
	# cntlr_list order, which need not be the same one. Asserted the way ops
	# step 3 asserts its fixed --slots, so a changed §7.1 fails HERE with that
	# sentence instead of failing later as a wrong-looking election.
	assert_eq "$CNTLR_CNT" 2 \
		"this step is written for §7.1's cntlr_cnt 2 (one standby to elect)"
	msg="the elected primary is the cntlr that was the standby: AR5 takes the"
	msg="$msg smallest cntlr_id among the healthy, enabled, non-primary cntlrs"
	assert_eq "$PRIMARY_CNTLR_ID" "$REACT_OLD_STANDBY_ID" "$msg"
	assert_ne "$PRIMARY_ADDR" "$REACT_OLD_PRIMARY_ADDR" \
		"and it is not on the CN whose agent was killed"
	# The subject of the rest of this step AND of step 4. Both check it against
	# the live primary, because AR5 can fire again while the rebuild below runs
	# (react_new_primary_ready's header says why, and why this case stops rather
	# than follows).
	REACT_NEW_PRIMARY_ID=$PRIMARY_CNTLR_ID

	# WHAT AR5 IS, EXACTLY: the two `primary` fields plus the SpRev bump
	# (model/ops.go:1062-1066). The dead cntlr's RECORD survives — its
	# disappearance is AR7's signature, which is why a cntlr count can never be
	# this step's assertion.
	#
	# THE DEMOTED-BUT-STILL-LISTED STATE IS TRANSIENT, and its whole lifetime
	# is THR_CNTLR - THR_PRIMARY, 15s at §7.1's values: AR5 fires
	# primary_unhealthy seconds after the err_epoch and AR7 fires
	# cntlr_unhealthy seconds after the SAME one (worker/reaction.go:719 and
	# :1095 both call reached() on that cntlr's err_epoch). $SP_JSON here is
	# the winning poll's own reply, not a re-read, and each poll costs one ssh
	# plus wait_until's 0.5s sleep — so it normally lands seconds after AR5 and
	# the assertion below is the real check. But a lab where the ssh stalls
	# could first observe the sp AFTER AR7, and failing a correctly-behaving
	# lab is worse than the coverage that one line buys.
	#
	# So it is skipped, loudly, in exactly that case — and skipping it
	# loses nothing about AR5, because AR7 cannot have run unless AR5 ran
	# first: replaceTarget skips a primary while a failover candidate exists
	# (worker/reaction.go:1098-1102) and model.ReplaceCntlr refuses one again
	# inside its STM ("failover candidate exists", model/ops.go:1465),
	# and at CNTLR_CNT 2 the standby is always such a candidate.
	if [ "$(jq_of "$SP_JSON" \
		"[.sp_conf.cntlr_id_list[] | select(. == \"$REACT_OLD_PRIMARY_ID\")] | length")" = 1 ]; then
		assert_jq "$SP_JSON" \
			"[.cntlr_list[]
			  | select(.addr_port == \"$REACT_OLD_PRIMARY_ADDR\" and (.primary | not))]
			 | length == 1" \
			"the dead cntlr $REACT_OLD_PRIMARY_ID is still in cntlr_id_list, listed on cn$REACT_OLD_PRIMARY_CN as a NON-primary now"
	else
		log "  NOTE: AR7 (cntlr_unhealthy ${THR_CNTLR}s) had already replaced" \
			"cntlr $REACT_OLD_PRIMARY_ID by the time this poll read the sp," \
			"so AR5's intermediate 'demoted but still listed' state was not" \
			"observed. AR7 cannot act on a primary while a failover candidate" \
			"exists, so the demotion above still happened; step 04 asserts the" \
			"replacement itself."
	fi
	# This one holds either way: AR7 deletes the old cntlr key and appends a
	# new id in the same STM (model/ops.go:1502-1532), so the count is
	# CNTLR_CNT before and after it.
	assert_field "$SP_JSON" '.cntlr_list | length' "$CNTLR_CNT" \
		"AR5 changed no cntlr count — it moved a flag"

	# The new primary has to build everything a standby never had: CN12 gives a
	# standby no groups and CN13 no pools, so this is the whole $SP_GRP_TOTAL
	# md arrays and $SLICE_CNT thin pools, on a node that already had every leg
	# connected.
	sp_totals
	stack_wait_reset
	msg="the new primary cntlr $PRIMARY_CNTLR_ID on cn$PRIMARY_CN to build"
	msg="$msg $SLICE_CNT pools, $SP_GRP_TOTAL groups and"
	msg="$msg $((SP_LEG_TOTAL + SP_SPARE_TOTAL)) legs"
	# WAIT_BUILD: this is a WHOLE stack, built from nothing on a node that had
	# only legs — the same piece of work setup pays for, at the 8m45s /
	# 126,657-spawn scale of the WAIT_BUILD comment, and the reason the failover
	# loop is self-defeating in the first place.
	#
	# react_new_primary_ready, NOT cntlr_full_ready: for the whole of that build
	# AR5 can fire again, and a wait pinned to cntlr $REACT_NEW_PRIMARY_ID would
	# then be watching a standby for pools it will never hold — twenty minutes of
	# WAIT_BUILD and a misleading message at the end of them. The predicate
	# follows the role for its progress line and stops the run the moment the
	# role leaves this cntlr; its header has the reasoning, including why this
	# case stops where setup follows.
	wait_until "$WAIT_BUILD" "$msg" \
		react_new_primary_ready "$REACT_NEW_PRIMARY_ID"
	# The two waits below stay pinned to the same cntlr, and deliberately: they
	# run AFTER the stack is complete, when the spawn storm that trips
	# primary_unhealthy is over, and a leg/raid0/level target on a node that got
	# demoted anyway fails in WAIT_PROVISION rather than in WAIT_BUILD.
	wait_until "$WAIT_PROVISION" \
		"the new primary to build the raid0 of $TD0 and $TD_REACT" \
		cntlr_raid0_ready "$PRIMARY_CNTLR_ID" 2
	# AND THE HOST-FACING HALF, which the two counts above do not cover: the
	# cntlr builds bottom-up and the subsystem, the namespaces and their ns-devs
	# are the LAST rows to appear, so a connect issued on the strength of the
	# raid0 alone can be refused by a target that has not created the subsystem
	# yet. ops_level_want READWRITE + cntlr_level_ready is CN19's full shape —
	# every row of all ten maps RES_STATUS_OK — and it is ops's helper, reused.
	ops_level_want READWRITE
	LEVEL_LAST=""
	wait_until "$WAIT_PROVISION" \
		"the new primary to report CN19's READWRITE shape, subsystem and namespace rows included" \
		cntlr_level_ready "$PRIMARY_CNTLR_ID"

	# host0 comes back on the new primary's transport ALONE — a direct
	# `connect`, not `connect-all`, because the cdc still advertises the dead
	# CN's transport until AR7 rewrites the CdcEntries in step 4.
	#
	# The export gate for this connect is the cntlr_level_ready wait directly
	# above, whose LEVEL_WANT includes ss_id_to_subsystem and
	# ns_id_to_namespace: it is the same question wait_ns_exported asks, over
	# every row of the cntlr rather than two of them, so there is no second
	# poll here. host_connect still verifies afterwards.
	host_connect 0 "$PRIMARY_TRADDR" "$PRIMARY_TRSVCID" "$SS0"
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID1" optimized
	wait_dev 0 "$UUID1"
	host_wait_ana 0 "$SS0" "$PRIMARY_TRADDR" "$UUID2" optimized
	wait_dev 0 "$UUID2"
	assert_eq "$(host_path_state 0 "$SS0" "$REACT_OLD_PRIMARY_TRADDR")" none \
		"host0 holds NO path to the dead cn$REACT_OLD_PRIMARY_CN"
	check_sha0 "after AR5 elected cntlr $PRIMARY_CNTLR_ID on cn$PRIMARY_CN"

	# §7.5's "a fresh 4 MiB write + read-back succeeds", written through the new
	# primary at 1 MiB into $TD_REACT: chunks 1..$BASELINE_MIB of the device,
	# which are slices 1..$BASELINE_MIB and therefore touch neither slice 0's
	# pool (AR6's accounting) nor the strided chunks.
	dev2=$(host_dev "$UUID2")
	REACT_PAT2="$WORK/pattern-react2"
	host_make_pattern 0 "$REACT_PAT2" "$BASELINE_MIB"
	REACT_PAT2_SHA=$(host_sha_range 0 "$REACT_PAT2" "$BASELINE_MIB") ||
		die "host0: digesting the post-failover pattern failed"
	assert_eq "${#REACT_PAT2_SHA}" 64 \
		"the post-failover reference digest is 64 hex digits"
	assert_ne "$REACT_PAT2_SHA" "$SHA0" \
		"it differs from SHA0 (both are /dev/urandom)"
	host_write_range 0 "$REACT_PAT2" "$dev2" "$BASELINE_MIB" 1
	react_wait_chunks 0 "$dev2" "$BASELINE_MIB" 1 1 "$REACT_PAT2_SHA" \
		"$WAIT_HOST" \
		"host0 to read back the $BASELINE_MIB MiB it wrote through the new primary"
}

# --- step 4 -----------------------------------------------------------------
#
# AR7, on the SAME err_epoch AR5 acted on: the agent is left dead, and
# cntlr_unhealthy ($THR_CNTLR s) later the record is replaced.

react_replace() {
	stage 04 "AR7: the dead cntlr is replaced on the CN that carried none"
	local out i cnt=0 idx=-1 hits msg

	# The chain §9's risk note names: replaceTarget skips a PRIMARY while a
	# failover candidate exists, and model.ReplaceCntlr refuses it again inside
	# its STM, so AR7 can only act on a cntlr AR5 has already demoted.
	out=$(helper_cn "$REACT_OLD_PRIMARY_CN" alive "$(cn_pid_file)") ||
		die "cn$REACT_OLD_PRIMARY_CN: the alive verb failed"
	msg="cn$REACT_OLD_PRIMARY_CN's agent is still dead: AR7 acts on the same"
	msg="$msg unhealthy cntlr AR5 demoted, cntlr_unhealthy = ${THR_CNTLR}s"
	assert_eq "$out" dead "$msg"

	msg="AR7 to replace cntlr $REACT_OLD_PRIMARY_ID (${THR_CNTLR}s after its"
	msg="$msg err_epoch, and only after AR5 demoted it)"
	wait_until "$WAIT_REACT" "$msg" \
		react_cntlr_replaced "$REACT_OLD_PRIMARY_ID"
	SP_JSON=$CTL_OUT
	sp_read_roles

	# EVERY ASSERTION BELOW IS ABOUT THE CNTLR AR5 ELECTED IN STEP 3, so it is
	# checked before any of them is made. The replacement is resolved as "the
	# cntlr that is neither the primary's nor the dead one's", and it is asserted
	# to be a standby with no groups and no pools; a second AR5 — legal, and
	# possible for as long as the replacement is healthy and this build keeps
	# missing primary_unhealthy — makes the first sentence resolve to the WRONG
	# cntlr and the second one false of a tree that did the right thing. Step 3's
	# rebuild wait catches that during the build; this catches it in the window
	# between the two steps.
	if [ "$PRIMARY_CNTLR_ID" != "$REACT_NEW_PRIMARY_ID" ]; then
		die "AR5 fired again between step 3 and step 4: cntlr" \
			"$REACT_NEW_PRIMARY_ID was the primary this case elected and" \
			"rebuilt, cntlr $PRIMARY_CNTLR_ID holds the role now. AR7's" \
			"replacement is healthy, so it is a failover candidate" \
			"(failoverEligible, worker/reaction.go:645-649), and the" \
			"$THR_SET set's ${THR_PRIMARY}s primary_unhealthy ($THR) is" \
			"short enough for a busy CN to trip. This step cannot judge AR7" \
			"after that: it resolves the replacement by elimination from the" \
			"primary, and asserts the replacement is a STANDBY."
	fi

	for i in "${!CNTLR_ADDRS[@]}"; do
		if [ "${CNTLR_ADDRS[$i]}" = "$PRIMARY_ADDR" ]; then
			continue
		fi
		if [ "${CNTLR_ADDRS[$i]}" = "$REACT_OLD_PRIMARY_ADDR" ]; then
			continue
		fi
		cnt=$((cnt + 1))
		idx=$i
	done
	msg="exactly one cntlr of $SP is neither the new primary's nor on the dead"
	msg="$msg cn$REACT_OLD_PRIMARY_CN"
	assert_eq "$cnt" 1 "$msg"
	REACT_REPL_POS=$idx
	REACT_REPL_ID=${CNTLR_IDS[$idx]}
	REACT_REPL_ADDR=${CNTLR_ADDRS[$idx]}
	REACT_REPL_TRADDR=${CNTLR_TRADDRS[$idx]}
	# On a CN that carried NO cntlr of this sp when the kill happened: AR7
	# black-lists the dead CN itself and §6.4 excludes the CNs of the sp's other
	# cntlrs, so those are the only candidates. The test is membership and not
	# equality, because with more than one such CN model.PickRandom chooses.
	case "$REACT_SPARE_ADDRS" in
	*",$REACT_REPL_ADDR,"*) ;;
	*)
		die "the replacement cntlr $REACT_REPL_ID landed on $REACT_REPL_ADDR," \
			"which carried a cntlr of $SP when the kill happened." \
			"The CNs that carried none were ${REACT_SPARE_ADDRS//,/ }"
		;;
	esac
	assert_ne "$REACT_REPL_ID" "$REACT_OLD_PRIMARY_ID" \
		"ReplaceCntlr deletes the old key and mints a NEW cntlr_id"
	assert_field "$SP_JSON" ".cntlr_list[$idx].cntlid_slot" \
		"$REACT_OLD_PRIMARY_SLOT" \
		"the replacement inherits the dead cntlr's cntlid_slot"
	msg="a replacement carries the OLD cntlr's role (asPrimary = old.GetPrimary()),"
	msg="$msg and AR5 had already demoted it, so this one is a standby"
	assert_field "$SP_JSON" ".cntlr_list[$idx].primary" false "$msg"
	assert_field "$SP_JSON" ".cntlr_list[$idx].disabled" false \
		"and it is enabled from birth, which is what puts its CN in the CdcEntry"
	log "  cntlr $REACT_OLD_PRIMARY_ID on cn$REACT_OLD_PRIMARY_CN was replaced" \
		"by cntlr $REACT_REPL_ID on cn$(cn_vm_of_addr "$REACT_REPL_ADDR")" \
		"($REACT_REPL_ADDR)"

	sp_totals
	stack_wait_reset
	msg="the replacement cntlr $REACT_REPL_ID to connect all"
	msg="$msg $((SP_LEG_TOTAL + SP_SPARE_TOTAL)) legs as a standby"
	# WAIT_BUILD, for the same reason ops step 3's third cntlr gets it: a cntlr
	# born now holds nothing, so this is every leg connected from scratch.
	wait_until "$WAIT_BUILD" "$msg" cntlr_legs_full_ready "$REACT_REPL_ID"
	assert_field "$CTL_OUT" '.cntlr_info | type' object \
		"the replacement's InspectCntlr reply carries a CntlrInfo"
	assert_field "$CTL_OUT" '.cntlr_info.grp_id_to_md_raid | length' 0 \
		"CN12: a standby has no group devices"
	assert_field "$CTL_OUT" '.cntlr_info.slice_id_to_dm_pool | length' 0 \
		"CN13: a standby has no thin pools"

	# The discovery log moved with the record: ReplaceCntlr rewrites every
	# CdcEntry of the sp, swapping the old transport for the new one. That is
	# also what makes `connect-all` safe again — the dead CN is not in any log,
	# so nothing can reconnect to the stack its dead agent left behind.
	disc_want_of_sp "$SS0"
	DISC_LAST=""
	wait_until "$WAIT_HOST" \
		"the cdc to serve $SS0 over the new primary and the replacement" \
		host_disc_is 0
	hits=$(printf '%s\n' "$DISC_LAST" |
		grep -cF -- "$REACT_OLD_PRIMARY_TRADDR" || true)
	assert_eq "$hits" 0 \
		"the dead CN's transport is out of host0's discovery log"
	hits=$(printf '%s\n' "$DISC_LAST" | grep -cF -- "$REACT_REPL_TRADDR" || true)
	assert_ne "$hits" 0 "and the replacement's transport is in it"

	# In the log is not the same as listening. cntlr_legs_full_ready above says
	# the replacement connected its legs; its host-facing subsystem and
	# namespace are the last rows it builds, and until the subsystem is linked
	# to its nvmet port that port does not listen (run 3's failure — see
	# wait_ns_exported). The gate covers the new primary too, and returns on
	# the first poll for it.
	wait_ns_exported_all "$SS0" "$SS0_ID" "$NS1_ID"
	host_connect_all 0 "$SS0"
	# Same as §4.3 stage 03: host0 still holds its path to the primary (the
	# assertion below), so a non-zero ctrl_cnt says nothing about the
	# REPLACEMENT's transport. This is the per-address half of the verdict.
	connect_added_ctrl 0 "$SS0" "$REACT_REPL_TRADDR"
	wait_until "$WAIT_HOST" \
		"host0's path to the replacement cntlr $REACT_REPL_ID ($REACT_REPL_TRADDR) to go live" \
		host_path_live 0 "$SS0" "$REACT_REPL_TRADDR"
	# A standby's namespaces are ANA-inaccessible: CN16 as amended by [D15]
	# gives AnaGrpIdOptimized only to a primary's.
	host_wait_ana 0 "$SS0" "$REACT_REPL_TRADDR" "$UUID1" inaccessible
	assert_eq "$(host_path_state 0 "$SS0" "$PRIMARY_TRADDR")" live \
		"host0's path to the primary is still live"

	# The killed agent comes back and finds it owns nothing. CN7 is what makes
	# that observable: SyncupCn tears down every cntlr whose pointer is gone
	# from the node's cntlr_ptr_list (agent/cnagent/syncup_cn.go:138 ->
	# teardownRemovedCntlrs, and the same test on the restart path at :97-106).
	# It is also what clears the md arrays, dm devices and nvmet exports the
	# dead agent left in this guest's kernel — the suite's own cleanup_all
	# would clear them too, but only at the end of the run, and case_residue
	# runs before that.
	start_cn_agent "$REACT_OLD_PRIMARY_CN"
	wait_until "$WAIT_PROVISION" \
		"cn$REACT_OLD_PRIMARY_CN to report its node state again" \
		cn_node_ready "$REACT_OLD_PRIMARY_CN"
	wait_until "$WAIT_PROVISION" \
		"cn$REACT_OLD_PRIMARY_CN's record to show an empty cntlr_ptr_list" \
		react_cn_has_no_cntlr "$REACT_OLD_PRIMARY_ADDR"
	RESIDUE_LAST=""
	wait_until "$WAIT_PROVISION" \
		"cn$REACT_OLD_PRIMARY_CN to tear down the stack its dead agent left behind (CN7)" \
		cn_residue_empty "$REACT_OLD_PRIMARY_CN"
	check_sha0 "after AR7 replaced the dead cntlr and its CN came back empty"
}

# --- step 5 -----------------------------------------------------------------
#
# AR8, on slice 0's FIRST data group — the one that carries the first stripe of
# every thin device, so the md rebuild covers the bytes check_sha0 reads.
#
# THE TRIGGER IS BOTH PLANES (see the header): the agent is killed, which makes
# the SIDE unhealthy (the worker's gRPC rounds fail), and the instance's nvmet
# port is dropped, which makes the LEG unhealthy (the primary's §3.6 probe IO
# fails). legNeedsRepair looks at side_unhealthy only AFTER Leg.err_epoch is
# non-zero, so the kill alone — which leaves the kernel's nvmet objects serving
# — would trigger nothing at all.

react_leg_repair() {
	if [ "$REDUND" != raid1 ]; then
		log ""
		log "=== react step 05 (AR8 leg repair) SKIPPED: --redund $REDUND." \
			"A RedundNone group has no redundancy to re-home, so AR8 only" \
			"logs it (reason=redund_none, worker/reaction.go:1174-1183) and" \
			"an operator moves the data."
		return 0
	fi
	stage 05 "AR8: a disk node dies and the worker spares its leg out"
	local n i sideid saddr hits out rev grpid msg
	local before_addrs before_vms spareaddr sparevm
	local before_spares before_spare_cnt before_sp_spares fresh

	sp_refresh
	sp_read_roles
	sp_totals
	grpid=$(sp_field "$COPY_GRP0.grp_id")
	assert_eq "$grpid" "$REACT_GRP_ID" \
		"slice 0's first data group is still the one step 01 recorded"

	# THE SPARE READING THIS WHOLE STEP IS JUDGED AGAINST. It is a snapshot and
	# not the literal 0 it used to be: this case's build runs under the reacting
	# threshold set and the first real run produced a spare_create during setup
	# (see the section header). Asserting "the group holds no spare leg" would
	# fail a run that behaved exactly as designed — and, worse, the wait below
	# used to be `react_spare_cnt_is 1`, which on a group that ALREADY held one
	# would have returned true on its first poll and passed this step without
	# AR8 having done anything at all.
	#
	# before_spares is a jq array literal of the group's spare leg ids, sorted,
	# so the assertions after the switch can be set comparisons. `tojson` on an
	# empty list gives `[]`, which is a valid filter fragment too.
	before_spares=$(sp_field \
		"$COPY_GRP0 | [.spare_leg_list[].leg_id] | sort | tojson")
	before_spare_cnt=$(sp_field "$COPY_GRP0.spare_leg_list | length")
	before_sp_spares=$SP_SPARE_TOTAL
	case "$before_spare_cnt$before_sp_spares" in
	'' | *[!0-9]*)
		die "the spare counts read '$before_spare_cnt' (group) and" \
			"'$before_sp_spares' (sp), which are not numbers"
		;;
	esac
	if [ "$before_spare_cnt" != 0 ] || [ "$before_sp_spares" != 0 ]; then
		log "!!! AR8 starts with spare legs already present:" \
			"$before_spare_cnt in group $REACT_GRP_ID (ids $before_spares)," \
			"$before_sp_spares in the sp. This case's build may create one" \
			"(the $THR_SET threshold set); every assertion below is a delta" \
			"on these numbers."
	fi

	# THE LEG TO KILL: one whose single side sits on a disk node that carries
	# exactly ONE side of this whole sp. AR8 repairs the unhealthy leg with the
	# smallest leg_id, so a DN carrying two sides would make two legs unhealthy
	# and this step could not name in advance which one the worker takes. Every
	# side of the sp is on a distinct DN at create (F2), but AR6's grow black-
	# lists nothing, so its new group MAY have landed on an occupied node —
	# hence the count rather than an assumption.
	n=$(sp_field "$COPY_GRP0.leg_list | length")
	case "$n" in
	'' | *[!0-9]*) die "slice 0's data group has leg_list length '$n'" ;;
	esac
	REACT_LEG_POS=-1
	for ((i = 0; i < n; i++)); do
		if [ "$(sp_field "$COPY_GRP0.leg_list[$i].side_list | length")" != 1 ]; then
			continue
		fi
		sideid=$(sp_field "$COPY_GRP0.leg_list[$i].side_list[0].side_id")
		saddr=$(sp_field "$COPY_GRP0.leg_list[$i].side_list[0].addr_port")
		hits=$(sp_field "[$SP_SIDE_PATH | select(.addr_port == \"$saddr\")] | length")
		if [ "$hits" != 1 ]; then
			continue
		fi
		REACT_LEG_POS=$i
		REACT_LEG_ID=$(sp_field "$COPY_GRP0.leg_list[$i].leg_id")
		REACT_SIDE_ID=$sideid
		REACT_SIDE_ADDR=$saddr
		break
	done
	msg="no leg of slice 0's first data group has a single side on a disk node"
	msg="$msg that carries exactly one side of $SP; killing a node with two"
	msg="$msg sides would make two legs unhealthy and AR8 repairs the smallest"
	msg="$msg leg_id first, which this step could not name in advance"
	[ "$REACT_LEG_POS" -ge 0 ] || die "$msg"
	case "$REACT_LEG_ID$REACT_SIDE_ID" in
	'' | *[!0-9]*)
		die "the leg to repair carried a non-decimal id" \
			"(leg '$REACT_LEG_ID', side '$REACT_SIDE_ID')"
		;;
	esac
	REACT_DN_VM=$(dn_vm_of_addr "$REACT_SIDE_ADDR")
	REACT_DN_INST=$(dn_inst_of_addr "$REACT_SIDE_ADDR")
	assert_ne "$REACT_DN_VM" none \
		"side $REACT_SIDE_ID's addr_port $REACT_SIDE_ADDR names a --dn guest"
	assert_ne "$REACT_DN_INST" none \
		"side $REACT_SIDE_ID's addr_port $REACT_SIDE_ADDR names a dn instance"
	diag_note_dn "$REACT_DN_VM" "$REACT_DN_INST"
	# The group's occupied DNs and VMs, SPARE LEGS INCLUDED. That is not caution,
	# it is what the code black-lists: grpAddrs walks GetLegList() and
	# GetSpareLegList() (worker/reaction.go:1418-1432) and grpLocations resolves
	# a location for every address grpAddrs names (:1398-1416). A `before` set
	# taken over the active legs alone would be a weaker statement than the one
	# AR8 actually makes, and on a group that already carries a spare it would
	# be the wrong set.
	before_addrs=$(sp_field \
		"$COPY_GRP0 | [(.leg_list[], .spare_leg_list[]) | .side_list[] | .addr_port]
		 | unique | join(\",\")")
	before_vms=$(sp_field \
		"$COPY_GRP0 | [(.leg_list[], .spare_leg_list[]) | .side_list[] | $SP_SIDE_VM]
		 | unique | join(\",\")")
	log "  killing dn$REACT_DN_VM instance $REACT_DN_INST" \
		"($REACT_SIDE_ADDR), which carries side $REACT_SIDE_ID of leg" \
		"$REACT_LEG_ID (position $REACT_LEG_POS of group $REACT_GRP_ID);" \
		"the group holds $before_addrs on VMs $before_vms"

	# Both planes. The agent first, so the worker's side rounds start failing,
	# then the port, which is what takes the DATA path away: nvmet objects
	# outlive their agent, so without this the primary's leg probe would keep
	# succeeding and Leg.err_epoch would stay 0.
	out=$(stop_dn_instance "$REACT_DN_VM" "$REACT_DN_INST") ||
		die "dn$REACT_DN_VM instance $REACT_DN_INST: kill_pidfile failed"
	printf '%s\n' "$out" >&2
	case "$out" in
	*STILL_RUNNING*)
		die "dn$REACT_DN_VM instance $REACT_DN_INST survived SIGKILL: $out"
		;;
	esac
	out=$(helper_dn "$REACT_DN_VM" alive "$(dn_pid_file "$REACT_DN_INST")") ||
		die "dn$REACT_DN_VM: the alive verb failed"
	assert_eq "$out" dead \
		"dn$REACT_DN_VM instance $REACT_DN_INST's agent after the kill"
	out=$(helper_dn "$REACT_DN_VM" port_drop "$(dn_port_id "$REACT_DN_INST")") ||
		die "dn$REACT_DN_VM: the port_drop verb failed"
	printf '%s\n' "$out" >&2
	hits=$(printf '%s\n' "$out" | grep -cF -e REFUSED -e STUCK || true)
	assert_eq "$hits" 0 \
		"port_drop refused or could not remove nvmet port $(dn_port_id "$REACT_DN_INST")"
	hits=$(printf '%s\n' "$out" | grep -cF " removed " || true)
	assert_ne "$hits" 0 \
		"port_drop removed the nvmet port of the instance it was told to"

	# AR8 step 3, then step 2's wait, then step 1 — one action per pass, so the
	# create and the switch can never be the same pass and the create is
	# observable on its own.
	REACT_SPARE_LAST=""
	msg="AR8 to create a spare leg for group $REACT_GRP_ID (side_unhealthy"
	msg="$msg ${THR_SIDE}s or leg_unhealthy ${THR_LEG}s, and the leg's err_epoch"
	msg="$msg only starts when the primary's probe fails)"
	# ONE MORE than the group had, so a group that already carried a spare
	# cannot satisfy this on the first poll.
	wait_until "$WAIT_REACT" "$msg" \
		react_spare_cnt_is "$((before_spare_cnt + 1))"
	SP_JSON=$CTL_OUT
	# `fresh` is the spare AR8 just minted: the entries whose leg_id was not in
	# the before set. Naming it that way rather than by index is what makes the
	# two assertions below about AR8's spare and not about whichever entry
	# happens to sit at position 0.
	#
	# ONE HONEST LIMIT, and it is the same one the previous form had: AR8 takes
	# at most one action per pass, so the create and the switch are different
	# passes — but if the poll that wins happens to land after BOTH, the entry
	# that is "new since AR8 started" is the DEAD leg the switch parked, not the
	# spare it promoted. The two assertions below hold either way (exactly one
	# new entry, with exactly one side); only the log line would name the dead
	# leg. The promotion itself is proved after the switch, from the POSITION in
	# leg_list, which is where REACT_SPARE_LEG is read.
	#
	# The test is jq's ARRAY DIFFERENCE and not `index`: in `A | index(.leg_id)`
	# the argument is evaluated against A, so `.leg_id` would be read off the
	# array rather than off the leg. `[.leg_id] - <before>` is empty exactly
	# when the id was already there, and it needs no jq variable — which also
	# keeps a `$` out of a double-quoted shell string.
	fresh="[$COPY_GRP0.spare_leg_list[]
	        | select((([.leg_id] - $before_spares) | length) == 1)]"
	assert_jq "$SP_JSON" "($fresh | length) == 1" \
		"exactly one spare leg of group $REACT_GRP_ID is new since AR8 started"
	assert_field "$SP_JSON" "$fresh[0].side_list | length" 1 \
		"the new spare leg has exactly one side"
	log "  group $REACT_GRP_ID now holds a new spare leg" \
		"$(sp_field "$fresh[0].leg_id") on" \
		"$(sp_field "$fresh[0].side_list[0].addr_port")"

	msg="AR8 to switch that spare in for the dead leg $REACT_LEG_ID (its side"
	msg="$msg must zero and the primary must report the leg OK first)"
	wait_until "$WAIT_PROVISION" "$msg" react_spare_switched "$REACT_LEG_ID"
	sp_refresh
	assert_field "$SP_JSON" "$COPY_GRP0.leg_list | length" "$LEGS" \
		"the group still has $LEGS active leg(s)"
	# THE SET AFTER THE SWITCH, derived rather than assumed: SwitchSpareLeg
	# takes the new spare OUT of spare_leg_list and puts the dead leg IN
	# (model/ops.go:1824-1830), so the group's spare set is exactly what it was
	# before AR8 plus the dead leg — and its size is unchanged by the switch,
	# which is why the sp-wide count below is before + 1 and not before + 2.
	assert_jq "$SP_JSON" \
		"($COPY_GRP0 | [.spare_leg_list[].leg_id] | sort)
		 == (($before_spares + [\"$REACT_LEG_ID\"]) | sort)" \
		"group $REACT_GRP_ID's parked legs are the ones it started with plus the dead leg $REACT_LEG_ID"
	sp_totals
	assert_eq "$SP_SPARE_TOTAL" "$((before_sp_spares + 1))" \
		"the sp holds one more parked leg than when AR8 started"

	# THE PROMOTED LEG IS READ OUT OF leg_list AFTER THE SWITCH, not out of
	# spare_leg_list before it: SwitchSpareLeg puts the spare in the target's
	# POSITION and parks the target in its place (model/ops.go:1824-1830), so
	# after the switch spare_leg_list holds the DEAD leg and only the position
	# names the promotion. Reading it here also makes the step independent of
	# how fast the two AR8 passes followed each other.
	REACT_SPARE_LEG=$(sp_field "$COPY_GRP0.leg_list[$REACT_LEG_POS].leg_id")
	case "$REACT_SPARE_LEG" in
	'' | *[!0-9]* | 0)
		die "the promoted leg has leg_id '$REACT_SPARE_LEG'"
		;;
	esac
	assert_ne "$REACT_SPARE_LEG" "$REACT_LEG_ID" \
		"the leg in the dead leg's position is a different leg"
	assert_field "$SP_JSON" \
		"$COPY_GRP0.leg_list[$REACT_LEG_POS].side_list | length" 1 \
		"the promoted leg has exactly one side"
	spareaddr=$(sp_field \
		"$COPY_GRP0.leg_list[$REACT_LEG_POS].side_list[0].addr_port")
	sparevm=${spareaddr%:*}
	assert_ne "$spareaddr" "$REACT_SIDE_ADDR" \
		"the spare did not land on the disk node that just died"
	# createSpare black-lists every DN of every leg and every spare of the
	# group (grpAddrs) and, as §6.5 tier 1, their locations (grpLocations).
	# Tier 2 relaxes the LOCATION rule when tier 1 finds nobody, so the VM
	# assertion is made only where tier 1 has somewhere anti-affine to go —
	# the same guard copy's migration and spare steps use.
	case ",$before_addrs," in
	*",$spareaddr,"*)
		die "the spare landed on $spareaddr, a disk node the group already" \
			"occupies; grpAddrs seeds AR8's black list" \
			"(worker/reaction.go:1420-1432)"
		;;
	esac
	if [ "$DN_VM_CNT" -gt "$LEGS" ]; then
		case ",$before_vms," in
		*",$sparevm,"*)
			die "the spare landed on a DN VM the group already occupies" \
				"($before_vms). AR8 passes grpLocations as §6.5's tier-1" \
				"exclusion (worker/reaction.go:1398-1418) and tier 2 relaxes" \
				"it only when no DN outside those domains can take the" \
				"group's ext_cnt"
			;;
		esac
	else
		log "  $DN_VM_CNT DN VM(s) and $LEGS leg(s) per group: §6.5 tier 1 has" \
			"nowhere anti-affine to go, so the spare's VM is not asserted"
	fi
	log "  leg $REACT_SPARE_LEG on $spareaddr took the dead leg's place;" \
		"$REACT_LEG_ID is parked"

	# md rebuilds onto the promoted spare. A fresh spare has never been an md
	# member, so this is a FULL recovery of the group (§8.12), and
	# RES_STATUS_OK alone proves nothing about it: probeGroup reports mdadm's
	# `State:` line verbatim and only "inactive" is an ERROR, so a rebuilding
	# array is OK with "clean, degraded, recovering". grp_md_clean reads the
	# words, and takes the SpRev the switch bumped to so that the array it
	# reads is the new one.
	rev=$(sp_field '.sp_rev.revision')
	case "$rev" in
	'' | *[!0-9]* | 0) die "\`sp get\` reports sp_rev.revision '$rev'" ;;
	esac
	MD_STATE_LAST=""
	msg="cn$PRIMARY_CN to apply revision $rev and md to finish rebuilding"
	msg="$msg group $REACT_GRP_ID onto leg $REACT_SPARE_LEG"
	wait_until "$WAIT_PROVISION" "$msg" \
		grp_md_clean "$PRIMARY_CNTLR_ID" "$REACT_GRP_ID" "$rev"
	check_sha0 "after AR8 rebuilt slice 0's data group onto a spare leg"

	# The disk node comes back. It has to: the parked leg's side still occupies
	# an extent on it, and only a live agent can retire that side when the
	# teardown drains the sp — case_residue would otherwise wait out
	# $WAIT_DELETE on a node that cannot answer.
	start_dn_instance "$REACT_DN_VM" "$REACT_DN_INST"
	wait_until "$WAIT_PROVISION" \
		"dn$REACT_DN_VM instance $REACT_DN_INST to report disk, header and port OK again" \
		dn_node_ready "$REACT_DN_VM" "$REACT_DN_INST"
	assert_field "$CTL_OUT" '.dn_info.port_info.res_name' \
		"$(dn_port_id "$REACT_DN_INST")" \
		"the restarted agent recreated its OWN nvmet port, not ports/1"
	sp_refresh
	sp_totals
	stack_wait_reset
	msg="the primary to report all $((SP_LEG_TOTAL + SP_SPARE_TOTAL)) legs"
	msg="$msg again, the parked one included"
	wait_until "$WAIT_PROVISION" "$msg" cntlr_full_ready "$PRIMARY_CNTLR_ID"
	log "  the parked leg $REACT_LEG_ID is connected again; the sp drain at" \
		"teardown is what releases its extent (model/drain.go:369-375" \
		"releases spare legs with the active ones)"
}

# --- step 6 -----------------------------------------------------------------

react_drop_target() {
	stage 06 "delete ns idx 2 and $TD_REACT, leaving $SS0 ns 1 and $TD0"
	ctl_ok ns delete --nqn "$SS0" --idx 2
	assert_field "$CTL_OUT" '.ns_id' "$REACT_NS2_ID" \
		"DeleteNamespaceReply.ns_id"
	# A real removal, so the head disk goes: this is the one direction in which
	# wait_dev_gone means anything (a suspend is a park and keeps the node).
	wait_dev_gone 0 "$UUID2"
	ctl_ok td delete --name "$TD_REACT"
	assert_field "$CTL_OUT" '.td_id' "$REACT_TD_ID" \
		"DeleteThinDeviceReply.td_id"
	ctl_ok td list
	assert_field "$CTL_OUT" '.name_to_td | length' 1 \
		"only $TD0 is left for the teardown"
	assert_jq "$CTL_OUT" ".name_to_td | has(\"$TD0\")" "and it is $TD0"
	check_sha0 "after the react case deleted its own namespace and thin device"
}

case_react() {
	CASE=react
	react_snapshot
	react_target
	react_grow
	react_failover
	react_replace
	react_leg_repair
	react_drop_target
	case_finish
}

# ---------------------------------------------------------------------------
# §7.8 — the space guard's run summary
# ---------------------------------------------------------------------------
#
# case_space_guard (section 6) is the ASSERTION half of D16/E2E5 and runs
# inside case_finish: per backing file `stat -c '%b %B'` <= $DN_CAP_BYTES
# through the `alloc` verb, the sum of every guest's work= and tmpfs= <=
# $RUN_CAP_BYTES, and the free-space floors preflight used. This is the other
# half §7.8 asks for — "print the totals in the run summary" — and it is
# deliberately a SEPARATE, read-only pass rather than an edit to that function:
#
#   * it is an independent reading: the summary's numbers are measured again,
#     by this function, and are not values smuggled out of an assertion that
#     may have been reached by a different path;
#   * and section 6's contract fixes case_space_guard's behaviour for every
#     case; redefining it here would silently keep the LAST definition of the
#     name and nobody would see which one ran.
#
# THE COST IS KNOWN AND ACCEPTED, and it is not small: this repeats, in full,
# the sweep case_space_guard ran seconds earlier in the same case_finish — one
# `alloc` over every backing file of each DN VM (DNS_PER_VM paths per VM, 43 at
# the default shape) plus one `space` verb, a `du -s --block-size=1` over
# $WORK, on all ten guests. At the §1.2 shape that is fourteen extra ssh round
# trips per case (4 alloc + 4 + 3 + 2 + 1 space). It buys
# only the independence above; anyone who decides that is not worth it should
# have case_space_guard record its own `total` and `worst` and reduce this to
# formatting them, and should delete this paragraph rather than leave it
# describing a function that no longer measures anything.
#
# It asserts NOTHING: every cap is case_space_guard's, and a summary that could
# fail a run would be a second, quieter copy of the same rule.

# The rows, in case order, plus the running maxima. Written only by
# space_note_case and read only by space_summary.
SPACE_CASE_ROWS=()
SPACE_RUN_MAX_TOTAL=0
SPACE_RUN_MAX_BACKING=0

# space_note_case takes one reading of the whole lab. One `alloc` per DN VM
# (every backing file of that VM in ONE ssh, as case_space_guard does) and one
# `space` per guest through read_space, which validates all three numbers in
# the parent shell.
space_note_case() { # <case name>
	local name=$1 total=0 worst=0 v k paths out bytes path row

	# QUIET is deliberately NOT raised around this: read_space dies on a guest
	# that cannot answer, and a die inside a quiet window would take the `[ip]
	# …` command echo out of the failure transcript as well. A dozen extra log
	# lines per case is the cheaper half of that trade.
	for v in "${!DN[@]}"; do
		paths=""
		for ((k = 0; k < DNS_PER_VM; k++)); do
			paths="$paths $(dn_backing "$k")"
		done
		# Unquoted on purpose: `alloc` takes one path per argument and every
		# path here is space-free.
		# shellcheck disable=SC2086
		out=$(helper_dn "$v" alloc $paths) ||
			die "dn$v: the alloc verb failed while summarising the space"
		while read -r bytes path; do
			[ -n "$path" ] || continue
			case "$bytes" in
			'' | *[!0-9]*) continue ;;
			esac
			[ "$bytes" -le "$worst" ] || worst=$bytes
		done < <(printf '%s\n' "$out")
		read_space helper_dn "$v" "dn$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
	done
	for v in "${!CN[@]}"; do
		read_space helper_cn "$v" "cn$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
	done
	for v in "${!HOST[@]}"; do
		read_space helper_host "$v" "host$v"
		total=$((total + SPACE_WORK + SPACE_TMPFS))
	done
	read_space helper_cp "" cp
	total=$((total + SPACE_WORK + SPACE_TMPFS))

	[ "$total" -le "$SPACE_RUN_MAX_TOTAL" ] || SPACE_RUN_MAX_TOTAL=$total
	[ "$worst" -le "$SPACE_RUN_MAX_BACKING" ] || SPACE_RUN_MAX_BACKING=$worst
	row=$(printf '  %-8s allocated %14s bytes on all guests, largest backing file %11s bytes' \
		"$name" "$total" "$worst")
	SPACE_CASE_ROWS+=("$row")
	log "  space after $name: $total bytes under \$WORK + \$TMPFS_DIR on all" \
		"guests, largest backing file $worst bytes"
}

# space_summary prints what was recorded. main calls it once, after the case
# loop and before the end-of-run cleanup removes the very files it counted.
space_summary() {
	local row
	log ""
	log "=== space summary (D16/E2E5), measured after each case's teardown"
	if [ "${#SPACE_CASE_ROWS[@]}" -eq 0 ]; then
		log "  (no case completed, so nothing was measured)"
		return 0
	fi
	for row in "${SPACE_CASE_ROWS[@]}"; do
		log "$row"
	done
	log "  peak:    $SPACE_RUN_MAX_TOTAL bytes on all guests" \
		"(cap $RUN_CAP_BYTES), largest backing file" \
		"$SPACE_RUN_MAX_BACKING bytes (cap $DN_CAP_BYTES per file)"
	log "  the caps themselves are asserted by case_space_guard, per case;" \
		"these are the readings, taken again afterwards"
}

# run_case is what main's loop should call for each name in $CASES: the case
# itself, then the §7.8 reading of what it left behind. They are one function
# so that a case can never be run without being measured.
run_case() { # <case name>
	"case_$1"
	space_note_case "$1"
}

# ---------------------------------------------------------------------------
# main (§7.1, §7.4 step 1, §7.6, §7.7)
# ---------------------------------------------------------------------------
#
# The order every section above was written against, and the two places it is
# NOT the plan's literal wording:
#
#   parse_args -> trap on_exit EXIT -> log_topology
#     -> [--cleanup-only: ship_helpers, cleanup_all, stop]
#     -> preflight_driver -> ship_helpers -> cleanup_all -> cleanup_start_gate
#     -> preflight_guests
#     -> setup -> the case loop -> run_summary
#
#  a. PREFLIGHT RUNS AFTER THE START CLEANUP, not before it. §7.3 says "before
#     any cleanup or setup writes", but a port check, a `ports_busy` and an
#     nvmet-port conflict check taken before the cleanup answer about the
#     PREVIOUS run's corpses, not about whether this run can start.
#     cnagent_test.sh:1489-1490 and cdc_test.sh:1601-1602 put theirs in the same
#     place for the same reason. Nothing in cleanup_all writes suite state — it
#     only removes — so no check is reading something this run made.
#
#     cleanup_start_gate sits between them for that argument to hold: those
#     checks are only about this run if the cleanup they follow actually ran.
#     A verb that never reached its sentinel makes every one of them a question
#     about corpses again, and the answer arrives as a preflight failure that
#     blames whatever the corpses collide with first. §7.7's tolerance is of
#     ABSENCE, not of a sweep that did not finish.
#
#  b. THE BETWEEN-CASES STEP IS setup_between_cases (cleanup_all + setup_infra
#     + setup_case — it rebuilds the sp as well as the infrastructure, because
#     cleanup_all has just deleted both),
#     NOT §7.6's "stop the four cp daemons, rm -rf $WORK/etcd, restart". That
#     narrower step is what reset_control_plane does and it is genuinely not
#     enough here; re-derived from the tree today rather than taken from the
#     setup section's word:
#       * CreateCluster stamps creation_epoch = uint64(time.Now().UnixNano())
#         (gateway/cluster.go:59) and model.ClusterId is fnv64a over the name
#         bytes followed by that epoch as 8 big-endian bytes (model/keys.go:
#         112-120), so a second `cluster create --name e2e` against a wiped etcd
#         mints a DIFFERENT cluster_id. The dn_ids are re-minted too.
#       * A dn agent wrote cluster_id, dn_id and extent_size into the 4 KiB disk
#         header at format time, and EnsureFormatted REFUSES a disk whose header
#         names another one — "foreign disk: cluster/dn/extent is %d/%d/%d, want
#         %d/%d/%d" (agent/dnagent/diskmeta.go:299-324). It never re-formats.
#     So case 2 would come up with every one of its DN_TOTAL disks reporting
#     RES_STATUS_ERROR, and setup's meta_info wait would burn WAIT_PROVISION on
#     each of them. Only dn_cleanup's loop_teardown (zero the first 4 KiB,
#     wipefs, losetup -d) plus its `rm -rf $WORK` removes that header, and only
#     the two cn phases remove a CN's store and its tmpfs arena — which is
#     exactly cleanup_all, in exactly the order it already gets right. The price
#     is a second full build per case — the 8m45s window of the WAIT_BUILD
#     comment, not §7.4's optimistic ~5 min; the alternative is a suite that
#     cannot run its second case.
#
#     §7.6 offers gateway_test.sh as the precedent for the narrow reset. It is
#     not one, in either direction. That suite's per-case reset is `wipe_etcd`
#     (integtest/gateway_test.sh:563-568): `etcdctl del --prefix` against a
#     RUNNING etcd with the gateways stopped, not a data-directory wipe at all
#     — and its own comment at :564-565 calls it
#     "the only non-workerctl write this suite performs". Here that would break
#     E2E2: every control-plane write in this file is the shipped dnvctl, and
#     etcdctl is put on cp for READ-ONLY diagnostics. It also would not help,
#     because the cluster_id problem above is on the DISKS, not in etcd.
#
# E2E6 lives in on_exit, not here: the END cleanup runs only on success, and a
# failing run keeps every process, dm/md/nvmet object, loop device, host
# connection and log and dumps §7.9 instead.
# ---------------------------------------------------------------------------

# log_topology prints, once and before the first ssh, exactly what this run will
# do to which machines. An operator who mistyped a --dn or pointed --cp at a
# guest another suite is using sees it in the first twenty lines rather than
# after a nine minute build.
log_topology() {
	local v last
	log ""
	log "=== integtest/e2e_test.sh"
	log "  shape:     slice_cnt $SLICE_CNT, redund $REDUND," \
		"legs/group $LEGS, groups $GRP_CNT (meta + data per slice),"
	log "             sides $((GRP_CNT * LEGS)), cntlrs $CNTLR_CNT," \
		"cntlid slots $SLOTS"
	# Both threshold sets, because the choice is per case and is made at the one
	# moment it can be made — `sp create` (§7.1, sp_thresholds).
	log "  thresholds: smoke/ops/copy  $THR_QUIET"
	log "              (quiet: no reaction may fire during a build, and the" \
		"measured build window is 8m45s)"
	log "              react           $THR_REACT"
	log "              (reacting: AR5/AR7/AR8 must fire inside a bound, so" \
		"react's own build may react too)"
	log "  placement: $DNS_PER_VM dnagent(s) per DN VM (F4 bound" \
		"$DNS_PER_VM_BOUND), $DN_TOTAL disk nodes on $DN_VM_CNT DN VM(s)"
	log "  sizes:     extent $EXTENT_SIZE, stripe $STRIPE_SIZE," \
		"td unit $TD_UNIT, backing $BACKING_SIZE per disk node"
	log "  cases:     ${ONLY:-${CASES[*]}}"
	log "  cp:        $CP  etcd $ETCD_CLIENT_PORT/$ETCD_PEER_PORT," \
		"gateway $GW_PORT, cdc $CDC_PORT (all as the login user)"
	for v in "${!CN[@]}"; do
		log "  cn$v:       ${CN[$v]}  gRPC $CN_GRPC_PORT," \
			"trsvcid $CN_TRSVCID, nvmet port $CN_PORT_ID"
	done
	# The last instance index of every DN VM: the ranges below are functions of
	# k alone (D10), so they are the same on each of them.
	last=$((DNS_PER_VM - 1))
	for v in "${!DN[@]}"; do
		log "  dn$v:       ${DN[$v]}  location $(dn_location "$v")," \
			"gRPC $(dn_grpc_port 0)..$(dn_grpc_port "$last")," \
			"trsvcid $(dn_trsvcid 0)..$(dn_trsvcid "$last")," \
			"nvmet ports $(dn_port_id 0)..$(dn_port_id "$last")"
	done
	for v in "${!HOST[@]}"; do
		log "  host$v:     ${HOST[$v]}  kernel nvme-tcp only, no dnv binary"
	done
	log "  paths:     $WORK on every guest, helper $HELPER," \
		"cn tmpfs $TMPFS_DIR"
	log ""
	log "  E2E9: this run occupies all" \
		"$((1 + CN_CNT + DN_VM_CNT + ${#HOST[@]})) guests and its cn agents" \
		"mount their tmpfs at $TMPFS_DIR, the path cnagent_test.sh owns."
	log "  Do not start another dnv suite anywhere in the lab, and do not edit" \
		"this file while it runs (bash re-seeks by byte offset)."
}

# run_summary is §7.8's "print the totals in the run summary", plus the shape
# the totals belong to. It ASSERTS nothing — every cap is case_space_guard's,
# per case — and it must run BEFORE main returns, because on_exit's end cleanup
# removes the very files space_note_case counted.
run_summary() { # <the case names that ran, as one word list>
	STAGE="run summary"
	log ""
	log "=== run summary"
	log "  shape:   slice_cnt $SLICE_CNT, redund $REDUND, $GRP_CNT groups," \
		"$((GRP_CNT * LEGS)) sides on $DN_TOTAL disk nodes, $CNTLR_CNT cntlrs"
	log "  cases:   ${1:-(none)}"
	log "  elapsed: $((SECONDS / 60))m $((SECONDS % 60))s"
	space_summary
	log ""
	log "  the end-of-run cleanup runs next (on_exit, success only) and removes" \
		"the files these numbers counted, so they are read out here first."
}

main() {
	# parse_args validates argv and derives every number the rest of the run
	# uses; it runs BEFORE the trap on purpose, because a usage error must exit 2
	# without a cleanup pass over guests this run has never touched.
	parse_args "$@"
	trap on_exit EXIT
	log_topology

	# §7.7 on its own. ship_helpers first: a guest that has never seen this
	# suite still needs the tools its own teardown is written in, and the
	# helper is re-shipped on every run including this one (§7.2).
	if [ "$CLEANUP_ONLY" -eq 1 ]; then
		log ""
		log "=== --cleanup-only: the §7.7 start cleanup on every guest, then stop"
		ship_helpers
		cleanup_all
		log ""
		log "  cleanup_all never dies (§7.6): a verb that never reached its" \
			"sentinel is a \`WARNING:\` line above, and a port it REFUSED or" \
			"found STUCK is a \`!!!\` line. This mode is an END cleanup, so" \
			"it reports them all and exits non-zero rather than stopping at" \
			"the first — cleanup_start_gate is not called here."
		log "  Nothing was preflighted and nothing was built."
		# Same verdict as on_exit's, for the same reason: the whole point of
		# this mode is to leave the lab usable, so "it did not finish" has to
		# be the exit code and not only a line in the log. A REFUSED port is
		# not counted — it was never this suite's to remove. on_exit turns
		# this non-zero return into the §7.9 dump plus the banner.
		[ "$CLEANUP_DIRTY" -eq 0 ] || return 1
		return 0
	fi

	# The driver: the tools, the jq, `make build`, the two integtest binaries,
	# `workerctl constants` (which fills ETCD_MAX_TXN_OPS — etcd will not start
	# without it) and the pinned etcd tarball.
	preflight_driver
	ship_helpers

	# §7.7, unconditional and before anything is built. It is also what makes
	# the port and nvmet-port checks below mean something: they judge the guest
	# this run is about to use, not the corpses of the last one.
	log ""
	log "=== start cleanup (§7.7, unconditional, tolerant of total absence)"
	cleanup_all
	# Tolerant of absence, NOT of a sweep that did not run: preflight below
	# judges the guest this run is about to use, and it can only do that if
	# the guest was really swept. See cleanup_start_gate.
	cleanup_start_gate start

	preflight_guests

	# THE RUN LIST IS COMPUTED BEFORE ANYTHING IS BUILT, because `setup` builds
	# the FIRST case's sp and sp_thresholds has to know whose sp that is: the
	# event_threshold set is chosen at `sp create` and no RPC changes it
	# afterwards (§7.1). parse_args has already refused an --only that names no
	# case, so this list cannot come out empty; it is checked anyway, because an
	# empty list would otherwise index an empty array under `set -u`.
	local name i ran=""
	RUN_CASES=()
	for name in "${CASES[@]}"; do
		if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then
			continue
		fi
		RUN_CASES+=("$name")
	done
	[ "${#RUN_CASES[@]}" -ge 1 ] ||
		die "no case to run: --only '$ONLY' matched none of ${CASES[*]}"

	# §7.4 steps 1-11, built for the FIRST case that will run. setup_infra raises
	# SETUP_DONE the moment it writes anything, which is what switches on_exit
	# from "clean up" to "dump".
	sp_thresholds "${RUN_CASES[0]}"
	setup

	# §7.5 / D14 / E2E11. Every case starts from an EMPTY etcd and a freshly
	# built sp: the first one uses the setup above, and every later one gets
	# setup_between_cases (cleanup_all + setup_infra + setup_case) in front of
	# it — see (b) in this section's header for why an etcd reset alone is not
	# that, and why the sp has to be rebuilt and not merely re-read.
	# setup_between_cases never runs after the LAST case; on_exit owns the end.
	#
	# sp_thresholds comes BEFORE setup_between_cases for the same reason it
	# comes before setup: the rebuild inside it is what runs `sp create`.
	for i in "${!RUN_CASES[@]}"; do
		name=${RUN_CASES[$i]}
		if [ "$i" -gt 0 ]; then
			sp_thresholds "$name"
			setup_between_cases
		fi
		# run_case, never `case_$name` directly: it is the case plus its §7.8
		# reading, so a case can never be run without being measured.
		run_case "$name"
		ran="$ran $name"
	done

	run_summary "${ran# }"
	return 0
}

main "$@"
