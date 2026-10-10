# cnagent.md — the cn role of `dnv-agent`

This document owns the cn role of `dnv-agent`: the cn's part of the
packages both roles share (the cn constants and name helpers of `common`,
the cn side of the probe-IO carve-out that keeps the leg health probes out
of the `OsClient`, which controller `NvmeHost.DisconnectDevice` selects
and the cn-only flag of `cmd/dnv-agent`), and the
role package `cnagent`, which serves
`ControllerNodeAgent`: the server type and its lock mapping, the startup
reconcile, `GetCnSize`, `SyncupCn` with the node's base state,
`SyncupCntlr` with its whole converge (the pass structure and the
effective desired state, the leg connections and their pass budget, the
leg health probes, the md-raid1 groups, the per-slice pools, the thin
volumes and the snapshot pre-pass, the raid0, ns-dev and nvmet stacks, the
transfers and the clones, the level gating and the reply), the sweep,
`PushCloneBitmap`, the info and check handlers, the bitmap reads, probing
and the read-only verdict. The mechanism both roles share — bootstrap and
lifecycle, the local store, the revision and conf gates, the lock
hierarchy, `ResInfo` tracking, the OS wrappers, the bitmap-chunk store and
the check-stream rules (SH1 to SH27) — and the `cmd/dnv-agent` command
(CM1 to CM4) are `dnagent.md`'s and are not restated here. This document
leans on `dnagent.md` for them, on `log.md`, `osclient.md` and `grpc.md`
for the logging, OS-access and interceptor rules, on `schema.proto` for
the service and its messages, and on `architecture.md` for the
controller-node device stacks and the on-leg layout, the names, the common
agent rules, the side provisioning protocol, live-state reporting, the
bitmap push protocol, teardown by sweep, failover, migration, the
transfer-and-clone copy, the raid0 bitmap math, clone crash recovery,
namespace suspend semantics, the levels, the cntlid slots and the
decisions [D1] to [D4], [D6] to [D8] and [D11] to [D16].

## Scope and placement

Package `cnagent`, at `agent/cnagent/`, is the cn policy: the
`ControllerNodeAgent` service, which decides which leg connections, md
arrays, thin pools, dm and nvmet objects to build and when. It may import
`common`, `pb` and `agent` (`layout.md`, Dependency rules). Everything
mechanism-shaped that both roles use lives in `agent` (`dnagent.md`, Scope
and placement, states the split rule) and is reused verbatim. Tooling only
the cn role runs — mdadm, the clone-metadata slot allocator, the
thin-metadata reader — is not promoted into `agent`: by the same split
rule a wrapper with a single role is role code and lives in
`agent/cnagent/` (`md.go`, `clonemeta.go`, `thinbm.go`). There is no LVM
anywhere in dnv (`architecture.md`, [D14]): the clone-metadata arena is a
slot allocator over one loop device whose registry is the clone-metadata
wrapper linears of Names and constants in `common`. Agents never talk to etcd
(`layout.md`, Dependency rules). The gRPC server carries the server
interceptors of `grpc.md`, Wiring; the cn agent's outbound connections are
only `nvme connect`s, never gRPC.

## The cn's part of the shared packages

### Names and constants in `common`

The cn constants live in `common/constants.go` and the cn name helpers in
`common/name_fmt.go`. The parsers that invert the names, `ParseDmName` and
`ParseNqn`, are shared by both roles and are stated in `dnagent.md`,
Names and constants in `common`: strict, because a half-decoded name is one the
caller would remove. The constants this document relies on, whose values
and comments `common/constants.go` holds:

* `DefaultCnTmpfsSize`, the size of the tmpfs that carries the
  clone-metadata arena file (`architecture.md`, Controller node, common),
  sized so that even a fully materialized arena plus slack never hits
  ENOSPC on the mount. The file itself is sparse: pages appear as dm-clone
  writes metadata and are released again by the allocator's hole-punch
  discard.
* `CnCloneMetaAreaSize` and `CnCloneMetaUnit`, the clone-metadata arena
  (`architecture.md`, [D14]; CN5, CN18): one sparse file (`CnTmpFilePath`)
  on the CN tmpfs, attached to a single loop device, carved into fixed
  units by the cn slot allocator whose registry is the clone-metadata wrapper
  dm tables themselves. The first is the `truncate` size of that file, and
  so the arena size; the second is the allocation granularity, the cn
  twin of `DnCloneMetaUnit`, and is deliberately not called an "extent",
  which everywhere else in dnv means the DN/CN allocation unit.
* `CnLegProbeInterval` and `CnLegProbeStallSeconds`, the pace of a
  primary's leg health-probe rounds and how long one probe round — the
  write and its read-back — may stay in flight before the leg is reported
  stalled (CN11). Probes are
  single-flight per leg and run outside every lock.
* `LegHealthBlockSize` and `LegHealthMagic`, the health block at the tail
  of the leg's meta region (`architecture.md`, Group on-leg layout: meta
  region, data region, health block): the payload is the magic, the writer
  id and a timestamp, never interpreted on read (`architecture.md`, [D6]).
* `CnConnectRetryInterval`, the pace of the background retries of a cn
  cntlr's converge after any of CN10's triggers; the cn twin of
  `DnMigrConnectRetryInterval`.
* `CnConnectPassBudget`, `CnConnectRetryPause` and `CnNsScanPause`, the
  connect step's one wait budget per converge pass (CN10, CN18), shared by
  every leg and clone source of the pass and drawn on by exactly three
  things: a failed connect's own elapsed time, the pause before each
  in-pass retry, and the steps of the wait for the namespace head after a
  connect this pass made.
* `ResDetailsSpLevel`, the details of a `CntlrInfo` row the `sp_level`
  suppresses (CN19): `RES_STATUS_MISSING` that says the resource must not
  exist, where every other `MISSING` the cn agent reports says it does not
  exist yet. The cn agent writes it and the worker's settle reads it
  (`dnv-worker.md` HL2), so the two take it from one constant; a value
  that drifted on one side would keep every primary whose level suppresses
  a row the settle reads settling for as long as that level stands.

Three dm kinds in the cn namespace are the cn's own — `DmKindCnLeg`, the
leg wrapper; `DmKindCnGrp`, the `RedundNone` group device;
`DmKindCnCloneMeta`, the clone-metadata wrapper — with their name helpers
(methods on `NameFmt`), and `architecture.md`, dm-device kinds, points here
for them:

* `CnLegName`, the cn-local leg wrapper (`architecture.md`, [D1], and
  Primary cntlr): one dm-linear over the leg's single nvme multipath
  namespace device, sized from the desired state, never from probing the
  device (CN10). md and groups consume it as the member device instead of
  the namespace device, whose name is the kernel's and can change across
  reconnects: a dnv-named member is what a group finds its array by and
  matches its members against `leg_list` by (CN12), and what the sweep
  attributes an array by (CN21).
* `CnGrpName`, a `RedundNone` group device: a dm-linear over the single
  leg's data region. `RedundMdRaid1` groups use the md names
  (`architecture.md`, md names) instead and have no dm name.
* `CnCloneMetaDmName`, the dm-clone metadata wrapper of CN18
  (`architecture.md`, [D14]): a dm-linear over one contiguous run of
  `CnCloneMetaUnit`-sized units of the CN clone-metadata loop device.
  dm-clone reads its superblock from sector zero and takes no offset
  argument, so every slot must be presented as a device of its own — the
  same reason `DnMigrMetaDmName` exists on the dn side.
  `CnCloneMetaDmPrefix` is the `dmsetup ls` filter that enumerates them,
  and it must come from `NameFmt` because the dm prefix is configurable.

[D1] of `architecture.md` leaves the leg wrapper agent-internal; fixing
its name here makes it observable (`dmsetup ls`, cleanup by prefix)
without making it part of any cross-component contract — nothing outside
the cn agent ever addresses these three devices. For the clone-metadata
wrapper the name is more than convenience: its tables are the allocation
registry (CN5, CN18), so the prefix-plus-kind filter of `dmsetup ls` must
be unambiguous.

The role letter of a dm kind, and why a dnv-prefixed NQN that decodes to
nothing (`IsDnvNqn`) is never touched, are `architecture.md`, dm-device
kinds; CN21 removes by the names it reads back, so it attributes by that
letter and leaves such an NQN's subsystem alone rather than attributing
it by its namespaces. NQN kinds keep a bare digit (`architecture.md`,
NQNs): an NQN already carries the dnv prefix, and no kind digit means one
thing to the dn and another to the cn.

One `common/name_fmt.go` formatter is pair-addressed: a clone bitmap chunk
is addressed by the pair `(src_slice_idx, bm_idx)` (`architecture.md`,
Bitmap push protocol), so `LocalCloneBmPath` carries both as its last two
segments, where `LocalMigrBmPath` carries the migration chunks' flat
`bm_idx` (`dnagent.md` SH21 to SH23). Both clone fields fit their segment:
`src_slice_idx` is below the clone's `src_slice_cnt`, itself at most
`MaxSliceCntPerSp` (enforced by `CreateClone`), and `bm_idx` is below
`MaxCloneBmCnt`, which caps the chunks of one source slice's bitmap, each
of the fixed capacity `CloneBmChunkBytes` (`gateway.md`, Constants this
document owns). The file name is only an address: the persisted
`PushCloneBitmapRequest` inside the file is what CN2 decodes the pair
from.

### Leg-probe IO leaves the `OsClient`

The carve-out itself — the two raw helpers `WriteBlockAt` and
`ReadBlockDirectAt`, why the read-back is O_DIRECT, why probe IO may block
indefinitely and must never hold a lock or a semaphore slot, and the two
records the prober writes — is `osclient.md`, Exported raw helpers and the
probe-IO carve-out. The cn's part: the CN11 probers are the only callers
of the helpers outside `common/osclient.go` itself, whose
`LimitedOsClient.WriteBlock` wraps the write half, and they reach the
helpers through `LegProbeIO`, the small fakeable
dependency defined in `healthcheck.go`, with a write half and a direct
read half, so unit tests can still script probe IO. Its real
implementation calls the exported helpers and logs the two records
itself, on the prober's per-attempt trace id (CN2); its fake mirrors
`FakeOsClient`'s fn-field style. The ctx a half takes exists only to carry
that trace id into the record and to fast-fail an attempt whose ctx is
already done (silently — an operation that never happened is not logged);
it cannot interrupt a syscall in flight. So a prober is cancelled and
never joined (`cmd/dnv-agent cn`; CN11), and every round issues its IO:
no transport-health gate skips a round.

### `NvmeHost.DisconnectDevice`

The two sides of a migrating leg share one subsystem NQN
(`architecture.md`, [D1]), so `nvme disconnect --nqn` would kill both
paths. When a leg's desired side set shrinks (after `FinishMigration`),
the cn agent must disconnect only the dead side's controller:
`nvmehost.go`'s `DisconnectDevice` issues `nvme disconnect --device` for
one controller device, found by the sysfs walk of CN10 and never by nvme
list-subsys: match "subsysnqn" under "/sys/class/nvme-subsystem" against
the leg NQN, then for each controller entry inside that subsystem
directory read its "address" and parse it as comma-separated key=value
pairs (traddr, trsvcid) to find the dead side's controller — never by
field position; a controller whose "address" did not answer, or is
absent, is never taken for it (CN10). `dnagent.md` SH20 places the
wrapper among the nvme-host wrappers.

## `cmd/dnv-agent cn`

CN-CM1. The `dnagent.md` CM2 flags gain one cn-only flag, `--capacity`: the capacity
budget in bytes this CN is willing to host; `GetCnSize` replies it
verbatim, and zero means "use the control-plane default" (`DefaultCnCap`).
It is the only cn-only flag. `--nvmet-port-id` is a both-roles flag and
lives in `dnagent.md` CM2 itself: the cn resolves and floor-checks it exactly as the dn
does (`dnagent.md` CM3), and a cn that keeps the default co-owns the default port with
a dn agent on the same node (`architecture.md`, Controller node, common).

Run at most one cn agent per kernel: the flag is not a licence for two,
and the rule — the three cn names that carry no cn id, and why placement
does not reliably keep two cntlrs of one SP off one kernel — is
`architecture.md`, Controller node, common. CN21's attribution assumes
the rule as well: a host-facing subsystem that no cntlr this agent holds
claims and no namespace attributes is unowned and removed at node level,
and another cn agent's subsystem passes through that very shape while it
is being built.

CN-CM2. `runCn` has the shape of `runDn` (`dnagent.md` CM4), with two cn
differences. `--capacity` is optional, since zero is a value (CN3). And
`agent.Serve` gets no `dnagent.md` SH27 background waiter — the cn joins
no background task: a sweep's background `nvme disconnect` (CN10) holds
nothing a restarted agent needs, and its CN11 probers are cancelled at
`rootCtx`, never joined (a wedged direct read would hang shutdown
forever; `osclient.md`, Exported raw helpers and the probe-IO carve-out).

## The cn role — package `cnagent`

### Files

`server.go` holds the `CnAgentServer` type, the lock mapping and the RPC
entry points other than the check streams of `check.go` and the bitmap
reads of `bitmapread.go`; `plan.go` the per-cntlr derived names and sizes
and the ns-dev backing and ANA state machines of CN16; `syncup_cn.go` and
`syncup_cntlr.go` the two converges; `leg.go` the CN10 side connections
and wrappers; `healthcheck.go` the CN11 probers and their `LegProbeIO`
(direct syscalls, never the `OsClient`); `md.go` the mdadm wrapper, the
sysfs reads of the arrays and the assembly cases (CN12); `clonemeta.go`
the base-state wrappers (the tmpfs mount, `truncate`, `losetup`) and the
CN18 clone-metadata slot allocator — unit accounting over the single loop
device, the `blkdiscard` recycle guard, the clone-metadata wrapper linears and
the `dmsetup ls` plus `dmsetup table` enumeration that is the allocation
registry; `pool.go` the concats, thin pools and thin volumes; `td.go` the
raid0, dm-error, ns-dev and flakey tables; `clone.go` CN18 and the clone
crash recovery; `xfer.go` the transfers; `push_clone_bm.go` the bitmap
push; `sweep.go` the CN21 sweep — the two scopes' enumeration,
attribution, wanted set and layered removal; `dmutil.go` the dm ensure
and probe helpers (`ensureDmSingle`, `ensureDmError`, `ensureDmLinear`,
`ensureDmMulti`, `probeDmTarget`, `probeDmArgs`, `probeDmConcat`,
`removeDm`, `removeExport`) shared by the converge files, `probe.go`,
`sweep.go` and the build phase of `syncup_cntlr.go` (`ensureDmClone`
stays in `clone.go`); `thinbm.go` the thin-metadata snapshot reader
behind `GetThinDeviceBm` and `GetLegBm` (CN25 to CN27) and the
clone-geometry fold of `architecture.md`, raid0 bitmap math;
`bitmapread.go` those two RPC entry points; `check.go` and `probe.go` the
check rounds and the probes. Package boundaries are binding, file names
are not.

### Server type and lock mapping

`CnAgentServer` embeds the generated unimplemented server and holds the
process's single `OsClient`, the `NameFmt`, the shared `Cmd`, `Store`,
`Dm`, `Nvmet` and `NvmeHost` wrappers, the cn-only `Md` and `CloneMeta`
wrappers, the `LegProbeIO` of the CN11 probers, the `LockSet` (object key:
the `LocalCntlrPath` id tuple), the `--capacity` value and the port conf
(`PortConf`: the transport flags plus `--nvmet-port-id`). A leaf mutex
guards the in-memory mirrors of the local store — two maps: cn requests,
and cntlr states (last accepted request, `ResInfo` tracker, a per-clone
`CloneChunkSet` keyed by the `(src_slice_idx, bm_idx)` pair) — and three
registries that mirror nothing: in each cntlr's state the connect-retry
and prober registries, and the CN10 disconnect registry, in memory only,
never persisted. Two more in-memory-only fields of a cntlr's state sit
outside that mutex, touched only under the cntlr's object lock or the
node write lock: the marks of CN14's activation sweep (`pendingSweep`)
and `reqFromRpc`, which says whether the request this process holds
arrived by the revision-gated RPC (CN14). Each state's request is an
atomic pointer,
stored and loaded without that mutex: other cntlrs' passes and the
node-level verdict read every cntlr's request holding none of its object
lock, and a request is never modified once stored. The tracker has its
own lock; the chunk sets are touched only under the cntlr's object lock,
or under the node write lock, which excludes it. A buffered channel of
`disconnectConcurrency` slots caps how many of the disconnect registry's
disconnects run at once. `rootCtx` anchors the CN10 and CN18 retry loops,
the CN10 background disconnects and the CN11 probers. `cloneMetaMu`
serializes the CN18 allocator (CN5): the registry is the kernel's dm
tables, so enumerate, discard and create must be one critical section. It
is the one cn lock held across OS calls — a leaf taken below every
`LockSet` lock, never held while another is acquired.

CN1. Lock mapping (instantiates `dnagent.md` SH10 to SH13): `SyncupCn` and the startup
reconcile — node write; `SyncupCntlr`, `PushCloneBitmap`, `GetCntlrInfo`,
one `CheckCntlr` round, one background connect-retry attempt,
`GetThinDeviceBm` and `GetLegBm` — node read plus that cntlr's object lock
(key = the cluster, cn, sp and cntlr ids); `GetCnInfo` and one `CheckCn`
round — node read; `GetCnSize` — no lock. The CN11 leg probers take no
lock at all: their IO can block far past every command timeout (a leg
with no serving path queues IO indefinitely), and nothing that holds a
lock ever waits on them — they only publish results into their own
registry, which probes read. They also take no `OsClient` semaphore slot:
their IO is issued by the direct-syscall path of Leg-probe IO leaves the
`OsClient`, so a wedged prober can starve neither the lock hierarchy nor
the node's OS-command budget — which is what keeps the teardown
`nvme disconnect` that releases it runnable. That disconnect takes no
lock either: the sweep issues it through the CN10 disconnect registry,
and under its locks the pass only probes the connection.

### Startup reconcile

CN2. Enumerate the store (`dnagent.md` SH6) for the cn's three store kinds —
`StoreKindCn`, the CN request file; `StoreKindCntlr`, the cntlr request
file; `StoreKindCloneBm`, the clone bitmap chunk file — and load every CN
and cntlr request into memory first, saving the cntlr files skipped with
their CN (below). Then reload every chunk into the owning cntlr's
`CloneChunkSet` (`dnagent.md` SH21), keyed
by the `(src_slice_idx, bm_idx)` pair the chunk is addressed by — before
any converge runs, so that a dm-clone the converge (re)builds re-applies
in the same pass every chunk the node already holds (CN18 step 4). Owner
and address come out of the file's content: the file is a persisted
`PushCloneBitmapRequest`, and its `cluster_id`, `cn_id` and
`cntlr_pointer`, its `clone_id` and its `src_slice_idx` and `bm_idx` are
what the reload reads. The name carries the same pair (Names and
constants in `common`), but only as an address — nothing here parses it.
A file that
does not decode is logged and skipped; a skipped file is deliberately not
an orphan and is not deleted — a decode failure says nothing about who
owns it, so nothing here may conclude the owner is gone. A chunk file
written under another layout of the message is tolerated nowhere, by
design — binaries upgrade in lockstep — and the operational answer to
such a change is to clear the CN's `--local-store`. Loading the chunks
after the converges would lose none of them — a chunk in memory is
advertised as applied by the next reply's `chunk_id_list` (CN20), so the
worker's diff (`dnv-worker.md` BM2) would not push it again — but their `blkdiscard`s would
then wait for the next event that re-applies the whole set: a create of
that dm-clone, a converge reload of its table onto a changed length,
devno or region size (step 4 runs on both — the converge treats a reload
exactly as a create), a clone crash recovery (step 4 runs on every one,
also over an old dm-clone its removal left in place), or another chunk's
own push (CN22). Until then the clone re-copies regions it never needed
to.

A decoded chunk file whose cntlr is not among the loaded cntlr
requests, or whose `clone_id` is absent from that cntlr's stored
`clone_list`, is an orphan — its cntlr or clone was deleted while the
chunk file survived (`dnagent.md` SH7) — and is deleted here, because it names an
owner no later pass will ever look for, unless it is skipped (below). A
chunk whose cntlr is not loaded is no orphan while a cntlr file does
not load and the chunk's loaded CN still names its cntlr: that file names
no cntlr, so it may be this cntlr's, and the chunk is skipped like the
files of a CN that did not load — neither loaded nor deleted — until that
cntlr's `SyncupCntlr` rewrites its file and the worker pushes the chunk
again (CN20). A chunk whose loaded CN no longer names its cntlr is an
orphan whatever cntlr file failed to decode. A CN file that does
not load — its read fails or it does not decode — is skipped, and while
one is unread, so is every cntlr file and chunk file whose CN
is not loaded: a file that did not load names no CN (`dnagent.md` SH6), so any of
those cntlrs may be one its list still names, and a list that could not
be read proves nothing about which cntlrs left it. Skipped is neither
loaded nor deleted: none of them is converged or applied, and every one
of those files stays on disk exactly as the restart found it — loaded,
each cntlr would reach the pointer-absent branch below, which would
delete its request and its chunks for want of a list that could not be
read. The node-level sweep (CN21) keeps a skipped cntlr's sp stacks
while its pointer is in the list; but its two request-derived sets —
the clone-metadata wrappers some `clone_list` names (the arena step
below) and the clone sources some request names (CN21) — are read from
the cntlrs in memory alone, so a skipped cntlr's wrapper and clone
source survive a sweep whose listings answered only while a live dm-clone
maps them: the wrapper
refuses its removal with EBUSY and is only reported, and the source
reads as in use. A wrapper nothing maps goes, and its clone's next
converge rebuilds it in the recovery an absent dm-clone triggers anyway
(CN18). Nor is anything hidden: with none of it in memory, the CN and
each of those cntlrs answer their Check rounds `ReplyCodeUnknownObject`
(`dnagent.md` SH25), and the worker re-sends each `Syncup*` (`dnv-worker.md` RW4). The
`SyncupCn` rewrites the file, and from there each skipped cntlr the list
still names is where a lost `--local-store` leaves one — known by its
pointer alone (CN21) — until its `SyncupCntlr`, which CN8 admits only
after that `SyncupCn`, converges from the request it carries and rewrites
its file; that reply's `bm_info_list` acknowledges no chunk, so the
worker pushes each chunk of its clones again (CN20). A skipped cntlr the
re-sent list no longer names is in no list and not loaded, so the
node-level sweep removes its resources by name (CN21); its files wait for
a later restart, whose pass finds its pointer absent and drops them
(`dnagent.md` SH7).

Then, for each CN request: re-run the `SyncupCn` converge (CN5). Then
each cntlr request: if its pointer is absent from the stored
`SyncupCnRequest.cntlr_pointer_list` — a CN with no request loaded reads
as a list that names no cntlr, and its cntlrs get this far only while no
CN file is unread — the cntlr is dropped: its cntlr file and
chunk files, its memory entry, its object lock and its goroutines
go, and nothing of its is removed from the node here. Its resources are
found afterwards, by name, by the node-level sweep (CN21) that runs next.
That split is the point: a teardown that deletes the same state after a
best-effort removal pass whose every step only logs its failure forgets a
cntlr whose array would not stop with its devices still live, and nothing
ever enumerates them again (`architecture.md`, Teardown by sweep). Ids
are never reused, so a dropped cntlr never comes back. Every other
cntlr request: re-run the `SyncupCntlr` converge from the stored
request — which, per CN18 step 4, runs the clone crash recovery for a
clone whose dm-clone metadata is missing or unusable, whose dm-clone
device itself vanished while a healthy wrapper stayed behind, or whose
dm-clone is up but does not show hydration enabled — what an agent killed
between CN18 steps 3 and 5 leaves behind; a dm-clone that is present over
a wrapper that still matches, with hydration enabled, but whose table has
drifted — a changed length, devno or region size — is reloaded by step
3's probe-first converge (`dnagent.md` SH16) with step 4's locally held source chunks
re-applied, and is not a recovery; the same drifted table over a missing
or mismatched wrapper is instead removed and rebuilt by the recovery,
which never sees the drift. A reboot clears the tmpfs, the loop device
and every clone-metadata wrapper together — the arena is volatile with the
kernel's dm state — so the reconcile starts from an empty arena; a plain
agent restart preserves both, and the converge is a no-op re-apply for a
clone whose build had enabled hydration, while a build the dead agent
left short of that runs the recovery again — a missing wrapper, a missing
dm-clone and a dm-clone with hydration still disabled are each a recovery
trigger. The pass order is therefore: load, then converge every CN's
base state, then drop the orphan cntlrs, then the node-level sweep per
CN, then converge every remaining cntlr.

Sweeping the arena is one step of that node-level sweep: a clone-metadata
wrapper (prefix `CnCloneMetaDmPrefix`) whose clone appears in no loaded
cntlr's desired state is an orphan and is removed, which frees its
units for the next allocation. It compares against a name set built from
every loaded cntlr's `clone_list` — the wrapper sweep needs no parse of
its own, and a cntlr whose file is skipped (above) is not in that set —
and it runs from the node-level sweep only, which is to say
from here and from `SyncupCn` (CN7), because those are the only two
places that hold the node write lock and see every cntlr this process
holds; a `SyncupCntlr` sees one cntlr. It runs before the cntlr
converges, not after, and that is safe for the same reason the whole
sweep is: the wanted set comes from the requests held, never from what
happens to exist, so a clone this pass is about to (re)build is already
named by its cntlr's `clone_list` and is never mistaken for an orphan.
All under the node write lock, with the `dnagent.md` SH2 trace id. Background retries
(CN10, CN18) and probers (CN11) mint a fresh trace id per attempt.
Whatever this pass could not remove is not remembered anywhere: the first
`Check*` of the object that still holds it recomputes it and reports it
(CN30).

### `GetCnSize`

CN3. Reply `--capacity` verbatim. Zero means "no local opinion" and the
control plane substitutes `DefaultCnCap`; the agent never validates or
clamps — the gateway owns the mapping onto the range from `MinCnCap` to
`MaxCnCap`. `cn_id` may be zero (a pre-registration call); it is for
logging only. No locks, no store, no OS access. This RPC has no
`AgentReply`; it cannot fail.

### `SyncupCn`

CN4. Gate the revision (`dnagent.md` SH8) against the stored `SyncupCnRequest`.

CN5. Converge the once-per-CN base state of `architecture.md`, Controller
node, common, probe-first (`dnagent.md` SH16), building `CnInfo` as it goes:

* **tmpfs** at `CnTmpfsPath`: probe with `findmnt` for the filesystem type
  at the path — a non-zero exit means "nothing mounted there", not a
  failure, and the reported type is what lets the converge insist the
  mount is a tmpfs — followed by a second `findmnt` asking whether the
  path is itself a mountpoint. The
  second call exists because the first resolves to the closest enclosing
  mountpoint, so a path that merely lives under another mount would
  answer that mount's type; the second matches only when the path is
  itself the mountpoint. A mount of the wrong type is an `ERROR` on
  `tmpfs_info`, so what the second call actually saves is the case where
  the enclosing mount is itself a tmpfs: the arena lives under
  `DefaultTmpfsPrefix`, so a tmpfs over that prefix's parent would answer
  for it, the `mount` would be skipped, and the arena would share that
  filesystem instead of getting its own `DefaultCnTmpfsSize`. Absent ⇒
  create the mountpoint and `mount` a tmpfs there sized
  `DefaultCnTmpfsSize`. A `findmnt`
  that did not answer (`dnagent.md` SH15) — either of the two — is not
  "absent": the converge runs the probe once more, from its first call,
  in the same pass and acts on that answer, and when that one does not
  answer either, `tmpfs_info` is an `ERROR` naming the probe and nothing
  is mounted. A mount over the live arena would stack a second, empty
  tmpfs on it: the arena file drops out of sight, so the converge
  truncates a fresh one and attaches a second loop device to it, and
  every clone-metadata wrapper still maps the first loop, so every clone on
  the CN is rebuilt (CN18 step 2). The price, when both askings go
  unanswered, is a tmpfs that really is absent — after a reboot, or on a
  new CN — and is not mounted by that pass. The two steps below then
  create nothing either: they create the file and the loop device only
  while `tmpfs_info` is `OK`, on a tmpfs this pass found or mounted at
  the path. Without one the mountpoint may be a bare directory — kept by
  a reboot, or made for a `mount` that then failed —
  where a `truncate` would put the arena file on the filesystem that
  holds the directory and `losetup` attach the loop device to that
  file; the next converge's `mount` would hide that file and bring the
  cascade above: a fresh file, a second loop device, and every clone
  built on the first loop rebuilt. As after a failed `mount`, the arena
  then waits for a later converge: a check round whose probes answer
  reads what is absent `MISSING` and names it in the `CheckCn` verdict
  (CN30), so the worker re-sends `SyncupCn` every round until a converge
  has built it; a round whose `findmnt` does not answer reads
  `tmpfs_info` `ERROR` and names nothing of the arena.
* **backing file** `CnTmpFilePath`: probe its size with `stat`; absent ⇒
  `truncate` the file to `CnCloneMetaAreaSize` (sparse — tmpfs pages
  materialize only as clone metadata is written, and CN18's hole-punch
  `blkdiscard` frees them again), but only while `tmpfs_info` is `OK`
  (above); otherwise an absent file is an `ERROR` on `tmp_file_info`
  saying no tmpfs is confirmed, and a file that is there is reported as
  the probe finds it. A `stat` that did not answer is not "absent"
  either: the converge asks once more in the same pass, and when that
  does not answer either, `tmp_file_info` is an `ERROR` naming it and
  nothing is truncated — a file that really is absent then waits, as the
  tmpfs does, for a check round that reads it absent and names it (CN30),
  and the `SyncupCn` that round makes the worker re-send.
* **loop device**: probe with `losetup` for the loop devices attached to
  `CnTmpFilePath` (the loop path is re-learned from this probe on every
  converge and every probe pass — it is kernel-assigned state, never
  persisted, and a stale cached path is exactly what CN28's arena check
  must catch); absent ⇒ `losetup` attaches a free loop device to it, only
  while `tmpfs_info` is `OK`, as for the file; otherwise, with none
  attached, `loop_dev_info` is an `ERROR` saying no tmpfs is confirmed.
  Exactly one loop device: multiple attachments of the same file are
  unwanted. "Absent" is an answer with no device in it: the probe exits
  zero with no output when nothing is attached, so any failure of it is
  an `ERROR` on `loop_dev_info` and attaches nothing.
* **clone-metadata arena**: nothing to create. The arena is the loop
  device: `cnCloneMetaUnitCnt` units of `CnCloneMetaUnit`, handed out to
  clones by the CN18 slot allocator. There is no on-disk allocation
  table — enumerating the clone-metadata wrappers (`dmsetup ls` filtered by
  `CnCloneMetaDmPrefix`, then `dmsetup table` of each) reconstructs the
  used map, because every wrapper's linear table records its own
  allocation: the loop device's major:minor and the offset in sectors
  (the name comes from `dmsetup ls`, the major:minor only from
  `dmsetup table` — `ls` formatting varies across versions). The `ls`
  list is a snapshot that is stale the instant it is printed — a
  `SyncupCntlr` holds only the node read lock, so another cntlr's sweep
  (CN21), at `SP_LEVEL_DISABLE` or otherwise, can remove a wrapper
  between the `ls` and its `table` — so a name whose `table` fails is
  dropped when the wrapper has meanwhile vanished: it claims no units,
  and failing the CN-wide enumeration would flip unrelated cntlrs'
  healthy clones to `RES_STATUS_ERROR`, which feeds `err_epoch`. The
  absence is confirmed with `dmsetup info` first, never assumed: a
  `table` failure on a wrapper that is still there stays fatal, because
  reporting a live wrapper's units as free would let the next allocation
  hole-punch a serving dm-clone's superblock — the one thing CN18's
  discard-before-create order exists to prevent. Reconstructing the used
  map from those tables is why the CN needs none of the dn's header, CRC
  and slot machinery: the arena is volatile together with the kernel's
  dm state (`architecture.md`, Controller node, common) — a reboot clears
  both, an agent restart preserves both — so the kernel's dm tables are
  the registry. A reboot therefore leaves an empty arena and clones
  rebuild through the clone crash recovery (CN18). The arena sweep of
  CN2 is not a step of this converge: it runs in the node-level sweep
  that follows the pointer diff (CN7), under the same node write lock.
* `EnsurePort` (`dnagent.md` SH19: the agent's `--nvmet-port-id` port, default
  `NvmetPortId`, from the transport flags plus the three fixed ANA
  groups). CN host-facing namespaces only ever use the optimized and the
  inaccessible group — the non-optimized group exists on every port
  (`architecture.md`, [D4]) but no cn code path assigns it.

CN6. QoS is accepted, persisted and not applied: the agent persists
`qos_ratio` with the request (`dnagent.md` SH5 does that for free),
programs nothing and reports no QoS row in `CnInfo` (`architecture.md`,
Controller node, common: QoS is not enforced).

CN7. Persist first, then drop, then sweep. The request is written to
`LocalCnPath` before the converge, not after it, as `dnagent.md` SH5
states for a parent request and for SH5's reason: the node-level sweep
below is what removes the resources of a cntlr whose pointer has just
left the list, and it can block for a whole failfast window on a dead
leg.

Then the CN5 base-state converge. Then the `cntlr_pointer_list` diff
against the loaded cntlr states (`architecture.md`, Common agent rules:
full sync) — never against the files: a cntlr whose file CN2 skipped is
not in memory and keeps its file (CN2). A pointer in the request without
local state needs nothing
yet — resources come with its first `SyncupCntlr`; the persisted request
is what makes the pointer known. A local cntlr whose pointer left the
list is forgotten: its cntlr file and chunk files are deleted, its
connect retry and leg probers are cancelled, its memory entry and object
lock are dropped (`dnagent.md` SH7) — and nothing of its is removed from the node by
this step. Then the node-level sweep (CN21) finds its resources by name
and removes them. Nothing is remembered about what the sweep could not
finish; the reply's code is recomputed from the sweep's own verdict
(CN30). The base state itself is never swept — like the DN port, it
outlives every cntlr and only lab cleanup removes it. Reply
`agent_reply`, `revision`, `cn_info`.

### `SyncupCntlr`

CN8. Gating. The pointer must be present in the stored
`SyncupCnRequest.cntlr_pointer_list` — else `ReplyCodeUnknownObject`
(`SyncupCn` introduces pointers first; `architecture.md`, Common agent
rules). Then the `dnagent.md` SH8 revision gate against the `SyncupCntlrRequest` this
process holds — zero while it holds none, as for a cntlr whose file CN2's
reload did not load or skipped, whatever revision that file carries.
Then, last and still with zero side effects, the conf gate of
`architecture.md`, Common validation: `agent.ValidateBdevConf`
(`dnagent.md`, Files) refuses a request whose `dm_pool_conf` carries a
zero `data_block_size` or `low_water_mark_pct`, whose `dm_raid0_conf`
carries a zero `stripe_size`, or whose `redund_conf` selected md-raid1
with a zero `bitmap_chunk_block_cnt`. The control plane resolves all four
when it writes the conf, so a zero here is a geometry no agent may invent
a replacement for — those values become the thin-pool's, the raid0's and
the md bitmap's own arguments (CN12, CN13, CN15), and a geometry this
node guessed is one the rest of the cluster does not share. The refusal
is `ReplyCodeInvalidConf` (`dnagent.md` SH9) carrying the validator's
message, and the reply echoes the stored revision, not the request's, so
the worker sees that the request was not accepted. One Error record, msg
"invalid stored conf", names the ids and the field.

Placement is load-bearing: the gate sits before the request becomes this
cntlr's desired state, so it skips the desired-state promotion, the whole
converge — whose sweep phase alone rewrites ANA states, reloads ns-dev
linears and removes dm devices — and CN20's local-store persist, which is
what keeps a refused request from being replayed by the next startup
reconcile. The same check is repeated in the converge itself for the two
entrances that do not come through this RPC — the CN2 startup reconcile,
which converges from a file that may have been persisted with zeros, and
the CN10 and CN18 background connect retry, which re-enters with the
request it already holds. There it returns an empty `CntlrInfo`,
enumerates nothing and converges nothing — and it carries no leftover
verdict either (CN30), because a cntlr this agent deliberately did not
build must not have its objects named as removable.

CN9. Role and pass structure. The effective role is primary iff
`primary` is set and `disabled` is not; anything else converges the
standby shape of `architecture.md`, Standby cntlr (a disabled cntlr
additionally moves every host-facing namespace to inaccessible — which
the standby shape already does, so `disabled` never needs its own
mechanism).

**Effective desired state (the provisioning gate, `architecture.md`,
[D15]).** Before either phase the agent derives an effective desired
state from the raw one, and both converges and probes against it. A
resource present in the raw desired state but excluded from the
effective one reports `RES_STATUS_PROVISIONING` with details
"provisioning" — deliberately not created yet, healthy, no action needed
— never `RES_STATUS_ERROR`, and it never feeds `err_epoch`
(`architecture.md`, Live-state reporting). The details string carries no
progress counter on purpose: a value that changed every round would
defeat the `proto.Equal` suppression of the Check stream (`dnagent.md` SH26) and
re-send `CntlrInfo` on every tick; the dn's zeroing counter is affordable
only because it changes just while its side zeroes and its stream is
per-side. The exclusions:

* a leg is provisioning iff its `side_list` is non-empty and every side
  in it has `provisioned` false. Mid-migration a provisioned source plus
  an unprovisioned destination leaves the leg serving, so it is not
  provisioning; and an empty `side_list` stays the malformed-request error
  it already is (CN10), never a healthy `PROVISIONING` row that would hide
  it.
* a group whose non-spare `leg_list` contains a provisioning leg is
  deferred: no md array, no `CnGrpName`, and the group is excluded from
  the pool-meta and pool-data concat targets and from pool sizing (CN12,
  CN13).
* the concat exclusion is a prefix truncation, never a filter: each of a
  slice's two group lists contributes only its longest leading run of
  non-deferred groups, so a deferred group takes every group after it in
  the same list out of the concat too (those groups still assemble their
  md arrays — CN12 defers an array only on the group's own legs; only
  concat membership is truncated). A concat target's offset is the sum of
  the lengths before it, so dropping a group out of the middle would
  re-base every later target and silently relocate live pool-data blocks
  the moment the deferred group cleared. A prefix leaves the concat
  exactly the one that is already live, which is what makes CN13's "a
  not-yet-grown concat is `OK`, not a mismatch" true and what keeps CN27's
  span arithmetic answering about the live table.
* a slice either of whose two group lists (meta, data) is non-empty in
  the raw desired state and has no effective group left is deferred whole
  — not merely a slice all of whose groups are deferred. An empty concat
  is an error, not a shape: the concat would be built from an empty
  segment list, so the initial `CreateStoragePool` — where every leg
  provisions, and where the two lists routinely clear at different times,
  one DN finishing its zeroing before the other — would report "concat
  has no segments" as an `ERROR` on `slice_id_to_meta` or
  `slice_id_to_data` and "pool concat missing" on `slice_id_to_dm_pool`,
  raising `err_epoch` on a freshly created SP, which is exactly what this
  gate exists to prevent.
* a td backed by a deferred slice is deferred, and with it its ns-devs
  and namespaces (CN16), its transfers (CN17) and its clones (CN18) —
  each reporting `PROVISIONING` for its own rows. Deferral must reach that
  far: building a transfer or a clone over a raid0 that does not exist
  would fail into `ERROR` and `err_epoch`, and promoting a namespace over
  a non-existent device is the very hazard CN16's ANA conjunct prevents.
* an unprovisioned spare leg defers only itself — spares never assemble
  (`architecture.md`, Spare legs), so the group's own assembly is
  unaffected (CN12) — and `leg_id_to_leg` reports it
  `RES_STATUS_PROVISIONING` like any other provisioning leg (CN10).
* bitmap reads follow the effective state as well: CN27's data-group span
  walks the effective `data_grp_list` (leaving a deferred group in the
  walk would silently shift every later group's offset), and `GetLegBm`
  for a leg whose group is not an effective concat target — the
  provisioning leg's own group, or a group after a deferred one — fails
  the RPC (CN27) rather than answering "not in its slice's
  `data_grp_list`".

An initial `CreateStoragePool` in which every leg is still provisioning
therefore converges to an empty-ish cntlr reporting `PROVISIONING`
throughout: no error, no `err_epoch`, no failover flapping. Deferral is
not CN19's `sp_level` suppression, and where both apply `sp_level` wins
(`RES_STATUS_MISSING`, details `ResDetailsSpLevel`): the level is a state
the operator asked for, and claiming a resource "is coming" would
contradict them.

One converge pass has two phases, and the phase order is what implements
`architecture.md`, Failover, without special cases:

* **Sweep phase, top-down** (CN21) — everything of this cntlr's sp that
  exists on the node and the desired state does not want. What to remove
  is derived by enumerating the node and subtracting the wanted set,
  never from a diff of plans (`architecture.md`, Teardown by sweep): a
  removal computed as "the plan applied last time minus the plan applied
  now", with the applied plan overwritten whether or not the removals
  worked, forgets a removal that failed together with the plan that named
  it — and the flows in which the remote end is dead and a removal does
  fail (a level change, a spare switch, a finished migration, a failover)
  are exactly those. The layer order, the verification rule and the stop
  rule are CN21's; the pre-steps below are this pass's.
* **Build phase, bottom-up** — legs (CN10) → groups (CN12) → per-slice
  pools (CN13) → thin volumes (CN14) → raid0 and error (CN15) → clones
  (CN18) → transfers (CN17) → ns-devs plus host-facing nvmet (CN16) → ANA
  rewrites to optimized last. This is the new primary's part of
  `architecture.md`, Failover.

**The wanted set** is the object-level wanted set of `architecture.md`,
Teardown by sweep — what the build phase would ensure for this plan, with
every object a provisioning deferral merely postpones left in it (a
deferred group's array, a provisioning leg's wrapper). It is read off the
plan's own level and
role flags, and each row is pinned against the ensure that creates it
rather than against any prose: the leg wrapper `CnLegName` and its
side-export connection under `wantLeg`; the md array (or the `CnGrpName`
linear of a `RedundNone` group) under `wantGrp`; the pool concats and thin
pool (`CnPoolMetaName`, `CnPoolDataName`, `CnPoolFinalName`), the
`CnThinDevName` thin volumes and the `CnRaid0Name` raid0 under `wantPool`;
the per-td `CnErrorName`, the `CnNsDevName` ns-dev with its nvmet
namespace, the host-facing subsystems, and the `CnXferFinalName` transfer
with its subsystem and namespace under `wantAny`; the `CnCloneFinalName`
dm-clone, the `CnCloneMetaDmName` wrapper and the clone-source connection
under `wantClone`. The raid0 sits with
the pool, not with `wantAny`, because at `SP_LEVEL_NO_THINPOOL` there is
no thin volume left to stripe over (CN19) — a standby keeps only what
that list gives it, which is what makes a primary-to-standby flip nothing
but a smaller wanted set.

**Pre-steps**: the transitions derived from the plan and the live tables
(`architecture.md`, Teardown by sweep, whose attribution and derivation
rules are what this implements). Pre-steps 2 and 3 exist so the layers
below them can remove anything at all, and run only on a pass whose
listings all answered (CN21). Pre-step 1 exists so that a namespace the
plan wants inaccessible, a transfer's included, is moved there before the
device under it is parked or demoted — by pre-step 2 or 3, or by the
build phase — and runs on every converge:

1. **ANA.** Every namespace of the plan whose desired "ana_grpid" is
   `AnaGrpIdInaccessible` — a suspended one, a standby's, a deferred one,
   one at `SP_LEVEL_DISABLE` (CN16), and every transfer namespace the
   same — is moved there first,
   probe-first. The loop is over the plan, not over the wanted set, and
   the difference is the whole of `SP_LEVEL_DISABLE`: `wantAny` is false
   there, so the wanted set holds no namespace and no subsystem at all,
   the chain takes the subsystems whole, and L1 never writes a
   per-namespace "ana_grpid" before the rmdir — this pre-step is that
   level's only ANA move. It is the old primary's first step of
   `architecture.md`, Failover, and it precedes the park below for the
   reason given there: the host is told to stop using the path before the
   path stops working. It is also the one pre-step a converge still runs
   when an unanswered listing has stopped its sweep (CN21): it removes
   nothing, and the build phase that follows parks ns-devs whether or not
   the sweep ran (CN16), so the move has to come first there too.
2. **Park of a planned ns-dev.** A `CnNsDevName` of the plan whose CN16
   backing is its td's `CnErrorName` (rules 0 to 4), or whose live table
   still maps a device this pass is about to remove, is reloaded onto
   that error device; a namespace whose td is not in the plan at all has
   no error device to park on and is skipped. The live table answers
   "does this namespace's previous td still exist?" with no memory, and
   it is also right for a device an interrupted pass left mapping
   something no plan ever described. The reload's own flushing suspend is
   what completes the in-flight host IO — the old primary's next step of
   `architecture.md`, Failover — and without it the removal below fails
   EBUSY behind a live ns-dev. The park creates its error device when it
   is missing, so it does not run at `SP_LEVEL_DISABLE`: the wanted set
   holds no dm-error there, and one created after the sweep's enumeration
   would be in no chain — the pass would remove the ns-dev parked on it,
   reply clean, and leave an unwanted device for the next round to find.
   Nothing is lost by skipping it: the wanted set holds no ns-dev at that
   level either, so P0 parks every one by its own live table (CN21),
   which needs no planned dm-error — where the td's is missing, it
   reloads the device onto an error table of its own size.
3. **Demote of an unserved transfer.** A transfer device
   (`CnXferFinalName`) this cntlr no longer
   serves (`xferServed` false in the plan) is reloaded onto an error table
   of its own size, so that it lets go of the origin td's raid0 (CN17) and
   L6 can remove that raid0.

A fourth park runs inside the chain rather than here, because its set is
the chain's own: every unwanted ns-dev, whether or not a plan names it —
at `SP_LEVEL_DISABLE` the plan names them all and the wanted set holds
none of them. See CN21's P0.

A primary-to-standby flip is therefore nothing but "the desired set
shrank to the standby shape"; standby-to-primary is "it grew".
`migr_list` is carried in the request and read by nothing: the CN's whole
part in a migration is a leg's two-sided `side_list` (CN10).

CN10. Legs (`leg.go`; every leg of every group of every slice in
`id_to_slice`, `spare_leg_list` included, both roles). Per side in the
leg's `side_list`: `nvme connect` to the `SideToCnNqn` of the cluster,
sp, leg and this cn at the side's `nvme_tr_conf`, with the host NQN
`CnHostNqn` (`dnagent.md` SH20 flags). A side with `provisioned` false is skipped: the
dn exports nothing for it yet (`dnagent.md` DN9, DN10), so a connect
could only fail and arm the retry registry. Provisioned sides of the same
leg connect normally, which is what keeps a two-side migrating leg
serving while its destination side zeroes. A leg all of whose sides are
unprovisioned is provisioning (CN9): no connection, no `CnLegName`
wrapper, no prober, and `leg_id_to_leg` reports `RES_STATUS_PROVISIONING`
(CN28). All sides of a leg share that one NQN, so the kernel merges them
into one multipath namespace (`architecture.md`, [D1]); the agent finds
its head device without udev by the **sysfs walk**: the entry under
"/sys/class/nvme-subsystem" whose "subsysnqn" matches the leg NQN, then
the single namespace entry inside that subsystem directory names the
device node; the controller entries inside it are the paths, each read
through its "address" and "state" under "/sys/class/nvme", and ANA state
exists only per path, never under the multipath head; a connecting path
keeps its last-known ANA state, which is why the availability test (CN12)
reads a path's "state" beside its "ana_state". The walk, never
nvme list-subsys, is the one source for every outbound connection's
state (CN11, CN12, CN18, CN28): that command emits no ANA state unless it
is given a namespace block device, and with one it answers an
all-inaccessible namespace with an empty subsystem list, indistinguishable
from "not connected". On top of the head the **leg wrapper** `CnLegName`,
a whole-device dm-linear over the leg's meta and data blocks — sizes come
from the desired state, never from probing the device.

**The connect step waits, briefly and boundedly, for what it has just
asked for.** A primary's connect to a new side can reach the disk node
before its export is linked into the port — the worker's sides-first hold
(`dnv-worker.md` RW14) makes that rarer, not impossible: the hold is
bounded, and a new side whose disk node refuses its first `SyncupSide`
(`dnagent.md` DN8) usually reports only after it, so the cntlrs go
without it — and a single re-read after a connect can equally come before
the kernel has added the head. Each converge pass — one `convergeCntlr`,
whether a `SyncupCntlr`, the startup reconcile or an attempt of the
background retry below runs it — gets one wait budget,
`CnConnectPassBudget`, made at the start of that pass, shared by every
leg and by the clone sources of CN18, and never carried into another pass
or another cntlr. Exactly three things draw on it. (1) Every failed
`nvme connect` is charged its own elapsed time. (2) After a failed connect
to a provisioned side the pass pauses `CnConnectRetryPause` and connects
again — an in-pass retry — as long as the budget still covers the pause.
The error is not classified: a disk node that has not linked the export
into its port yet refuses the connect at TCP, or, when the port already
listens for another export, rejects it with the same fabrics write error
a permanently rejected export gets, and a disk node that is down refuses
it too, or does not answer at all; every failed connect is a candidate
until the budget is gone. A connect to a disk node whose machine is down
runs long (the kernel's SYN retries, or `CmdSoftTimeout`), spends the
whole budget at once and so is never retried in the pass. (3) After a
connect made in this pass, the subsystem is re-read every `CnNsScanPause`
until its multipath head is there, as long as the budget covers the step:
`nvme connect` returns once the controller is live and only queues the
namespace scan that adds the head. A subsystem that was already connected
and has no head — a leg whose only path is ANA inaccessible never gets one
— is judged on its one read. A pause starts only while it fits in what is
left, and a failed connect is charged after it has run, so a retried
connect that runs long can overdraw the budget; nothing bounds the first
connect of a side. Once the budget is spent the pass behaves exactly as it
does without one: every connect it makes is made once, the first that
fails still ends its leg's converge (the leg's other unconnected sides
wait for a later pass), and a leg that did not connect, or whose head did
not appear, fails with the same error. The waiting holds the cntlr's
object lock, which every `CheckCntlr` round takes too (CN1), which is why
the budget is one per pass and not one per leg.

A connect failure marks that leg `RES_STATUS_ERROR` and registers the
cntlr in a background retry registry that re-runs the converge every
`CnConnectRetryInterval` under the CN1 locks until a pass registers it no
more, or teardown (the `dnagent.md` DN13 pattern); the RPC itself retries a connect,
or waits for its head, only as far as the pass's budget allows. These
register it: a leg that failed to converge — its connect, its
multipath namespace or its wrapper (above), an unknown controller or a
disconnect of its subsystem still in flight (below) — a clone source whose
connection failed, or whose disconnect is still in flight (below), a clone
recovery whose destination bitmaps were not applied or another clone
failure CN18 lists (never a failed allocation or replacement of its
metadata wrapper), a `leg_list` member of a group that is not available
(CN12: a promotion whose first converge reads its legs before the sides'
ANA flips have reached this CN's sysfs would otherwise leave its md groups
unassembled — and a `RedundNone` SP's pools unbuilt — until the next
revision bump, because nothing else re-runs that converge), an ns-dev
the build held off its td's raid0 while a dm-clone the plan does not want
may still be live (CN18), and a clone destination's ns-dev whose converge
fails in a pass whose CN18 finished the clone's build (CN16 rule 5). Every
attempt mints its own trace id (CN2) and
is a whole converge, which decides afresh whether any of them still
holds; the first converge that finds none stops the retry.

**Dead paths**: a controller of the leg NQN whose traddr and trsvcid
match no desired side (the source side after `FinishMigration` — its
controller died with DNR and will never reconnect) is disconnected by
device (`NvmeHost.DisconnectDevice`), never by NQN. A controller whose
"address" read did not answer — any error but ENOENT, such as the soft
timeout spent waiting for an `OsClient` slot, which stalled sysfs reads
can hold — is unknown, never unwanted: it may be a desired side's live
path as easily as a dead one. It is never taken for a dead path; while
the pass's first read of the subsystem shows one, no side of the leg is
connected, since it may be that side's; and the leg's converge fails for
the pass — `RES_STATUS_ERROR` naming the read — which registers the
retry. An absent "address" is an answer: a fabrics controller has one for
as long as its device exists, and its subsystem keeps listing it after
the device was deleted, until the last reference to it drops. Such a
controller is gone, no side's path: it is not unknown and never taken
for a dead path, and a side it alone served reads unconnected and is
connected again. The walk that finds the subsystem is read the same way.
A listing of "/sys/class/nvme-subsystem" that did not answer, or a
"subsysnqn" read that failed — any error but ENOENT — on an entry while
no other entry names the leg's NQN, leaves the pass not knowing whether
the leg is connected, and a listing of the matching entry's own directory
that fails, whatever the failure, leaves its controllers and namespace
unread: the lookup fails naming the read, and the leg's converge fails
with it for the pass — `RES_STATUS_ERROR` naming the read, which
registers the retry — connecting nothing on that answer. A
"/sys/class/nvme-subsystem" that answers absent is an answer, no
subsystem at all (nvme-core creates it when it loads, and a host holds no
fabrics controller before it has), and so is an absent "subsysnqn", a
subsystem gone since the listing. An entry whose "subsysnqn" names the
leg's NQN is the answer even beside one whose read failed: the host keeps
every controller of one NQN in the one subsystem (`architecture.md`,
[D1]). CN18's source step reads its source through the same lookup, and
the CN28 rows that come from it — a standby's leg, a clone's target —
read `ERROR` naming the read too.

**The disconnect registry.** The CN21 sweep's `nvme disconnect --nqn` —
L10's legs, L5's clone sources and the node-level pass's unowned sources —
is never run inline: when the target vanishes mid-delete the kernel cannot
enter error recovery on a DELETING controller, the controller's shutdown
command waits out the admin timeout, and `nvme disconnect` waits with it
in an uninterruptible write that no signal of `dnagent.md` SH15 ends.
Inline, that would hold the cntlr's object lock — and every `CheckCntlr`
round of the cntlr with it (CN1) — for the whole timeout, and the worker's
`SyncupCntlr` would run into its own deadline. So the pass probes the
connection under its locks and nothing more: one with no controller is
gone; one the probe could not read is a leftover and nothing is issued for
it; one with a controller is a leftover of that pass, which sets its
disconnect going — unless one already runs — from a goroutine on `rootCtx`
that carries the pass's trace id, or a fresh one when the pass had none,
and takes none of the CN1 locks. A later pass's probe is what finds it
gone. At most `disconnectConcurrency` of these disconnects — a fixed
fraction of `DefaultOsClientLimit` — run at once; the rest wait for a
slot and stay registered while they wait.
Uncapped, one L10 of many legs or one pool drain would set them all going
together, and each delete a vanished target stalls holds an `OsClient`
slot for the admin timeout — enough of them, and the node's converges and
Check rounds are refused a slot at the soft timeout and report `ERROR`
rows on healthy objects. The registry is a server-wide set of subsystem
NQNs under the leaf mutex, and it is bookkeeping of work in progress,
never memory of work that failed: an entry lives exactly as long as its
goroutine, and it is read for two things. A later pass issues no second
disconnect of a subsystem whose first one is still running or waiting for
a slot. And the connect steps read it too: a converge that wants a
subsystem whose disconnect is still in flight neither adopts nor connects
it — the controller it would adopt is about to be deleted under it, and
one connected beside it could go with it, the disconnect being by NQN — so
the step fails, the CN10 or CN18 retry registers, and the first pass after
the goroutine has gone reads the subsystem afresh and connects whatever is
missing. The CN28 probe asks the same question: while the disconnect is in
flight it reports the leg's row, or the clone's target row, `ERROR` with
the converge's own details, and it does not judge that clone's step 2 —
so the two channels neither flip those rows, and their epochs, against
each other nor disagree on how far the clone got. Whether a connection is
left over is never read from the registry, only off the node — so a
disconnect that failed is issued again by the next pass that still finds
the controller. The goroutine is not joined at exit (`dnagent.md` SH27).
The dead-path `nvme disconnect --device` above is the build's, not the
sweep's, and runs in the pass, so it can still meet the same wait under
the object lock (Known limits). `Side.err_epoch` is control-plane
bookkeeping and is ignored.

CN11. Leg health probes (`healthcheck.go`; `architecture.md`, [D6], and
Group on-leg layout: meta region, data region, health block). Only the
primary runs the block probe, and only for legs that are not provisioning
(CN9 — an unprovisioned leg has no connection to probe): a standby's path
deliberately exports dm-error (`architecture.md`, Disk node), so block IO
through it can never succeed. Per connected leg the agent keeps one
prober goroutine (no locks, CN1): every `CnLegProbeInterval` it writes
the health block — the last `LegHealthBlockSize` bytes of the meta
region, through the leg wrapper, with the payload `LegHealthMagic`, the
`cn_id` and a nanosecond timestamp, via `LegProbeIO`'s write half — and
reads it back with its direct read half (O_DIRECT), never interpreting
the content. Both halves are issued as direct syscalls, outside the
`OsClient` and its semaphore, and each logs its own record (`log.md`, OS
commands and file IO) on the prober's fresh per-attempt trace id (CN2).
The loop is single-flight: a blocked IO simply delays the next round.
Probers publish since when an attempt has been in flight, whether any
has completed, and the last error; the CN28 probe reports, per leg: an
attempt in flight longer than `CnLegProbeStallSeconds` ⇒
`RES_STATUS_ERROR` "health probe stalled"; else the last completed
outcome; before any completion — or before a prober is registered — ⇒
`RES_STATUS_PENDING` "health probe pending" when the wrapper exists.
`PENDING` rather than `OK` there, because an `OK` clears
`Leg.err_epoch`: a dead leg's would be cleared at every promotion, whose
probers start over, and an unprobed spare would read ready to the
worker's leg repair (`dnv-worker.md` HL2, AR8). For a registered prober
the window is one `CnLegProbeInterval` tick plus the probe itself, and a
fresh wrapper, a promotion and an agent restart each open it. A leg whose
wrapper exists but whose converge fails before its prober registers — on
a promoted or restarted primary whose connect is refused, say — reads
`PENDING` in every probe round until a converge registers one, while each
failing converge marks the leg `RES_STATUS_ERROR` (CN10). A standby (and
spare legs on it) reports the transport probe instead: the sysfs walk
(CN10) shows a live controller per desired (provisioned) side and, for a
leg with exactly one desired side, an "ana_state" of optimized or
non-optimized — non-optimized is a standby path's designed steady state
(the DN grants the optimized group to the primary CN alone) and optimized
the pre-promote window after the DN's flip, while inaccessible, or any
state the DN never sets, cannot serve a promote and reports
`RES_STATUS_ERROR`. A leg whose `side_list` holds two sides (a migration,
CN10) is checked for liveness only: its ANA is wrong-by-design for the
hydration and the phase is the DN's knowledge, not this CN's. Probers
start when the wrapper converges and are cancelled by the first converge
whose plan no longer probes their leg — every converge trims them before
its sweep, whether or not the sweep's descent reaches the legs (CN21) —
or when the cntlr is dropped (CN7); a goroutine wedged in D state on a
pathless leg is released by the teardown's own disconnect (deleting the
controller errors its queued IO) and is accepted as unreclaimable until
then. Because that wedged probe still holds an open fd on the leg wrapper
after its cancel, CN21's L10 order is: set the leg's disconnect going,
then remove the wrapper — a `dmsetup remove` fails EBUSY until the
disconnect, which runs off the pass (CN10), has errored the queued IO, so
such a wrapper normally goes on a later pass. The cancel itself comes
before L10: in the converge's trim ahead of its sweep, or, for a cntlr
that is forgotten, in the drop step that precedes the node-level sweep
(CN7, CN2). Cancellation is never a join — the direct read is
uninterruptible, so waiting for it would hang shutdown forever (Leg-probe
IO leaves the `OsClient`; `dnagent.md` SH27). A wedged prober costs
nothing else: it holds no lock (CN1) and no `OsClient` slot.

CN12. Groups (`md.go`; primary only — a standby has none,
`architecture.md`, Standby cntlr).

* `RedundNone`: the group device is `CnGrpName`, a dm-linear over the
  single leg wrapper at the data region's offset and of the data region's
  length.
* `RedundMdRaid1`: an md-raid1 array under `CnMdDevName` (`--name` is
  `CnMdArrayName`, an internal bitmap with its chunk from
  `bitmap_chunk_block_cnt` and the block size, the data offset from
  `meta_blocks` and the block size, every member `--failfast`,
  `--homehost any`), whose members are the leg wrappers of `leg_list`
  (never `spare_leg_list`; `architecture.md`, Spare legs). Assembly
  is the step of `architecture.md`, Failover, that makes every group
  available; its cases are these, decided by probing each available
  member for an md superblock (`mdadm --examine`):
  1. No member has one ⇒ `mdadm --create` with `--run --assume-clean` —
     but only when every `leg_list` member is available and was probed:
     with any member missing, the group is simply unavailable this pass
     ("only k of n legs available and none carries a superblock"),
     because creating over a subset would mint a fresh array while an
     absent leg may carry the real one. Nor while the `CnMdDevName` node
     already resolves to a device (`Md.NameInUse`, an `lsblk` of the node,
     which reads no member; one that did not answer is an error that
     refuses the create this pass, like a killed `--examine` below): the
     array is found by its members (below), so one that runs under the
     group's name holding none of its `leg_list` wrappers — both legs
     switched out, parked or released, while this cntlr was not
     converging — reads as absent and reaches case 1 with fresh legs,
     where a create would put a second array under the name the pool's
     concat resolves ("an array runs under … holding none of the group's
     legs"). The guard errs only towards refusing, and a false refusal
     needs a stale node of this very name — which, short of the case the
     guard exists for, a group whose legs carry no superblock has never
     had an array to leave. `--assume-clean` is always correct here,
     short of the failed-read answer below (a member whose read failed
     answers as superblock-free without being so): a side is never
     exported before the provisioning protocol has written zeros over at
     least its meta region and its first data block and the side's
     `provisioned` flag has opened the gate (`architecture.md`, Side
     provisioning protocol), and ids are never reused — so a
     superblock-free leg can only be a freshly provisioned side (though a
     fresh leg can still carry an md superblock at its end,
     `architecture.md`, Known limits). The rest of a data leg's data
     region is never zeroed, and the members need not agree there:
     dm-thin writes a block whole before a host can read any of it
     (`architecture.md`, [D15]; CN13), so nothing dnv builds on the array
     reads a block of it before writing it through the array, and the
     first data block, which the node's udev, partition scan and LVM
     activation read when the array appears, is zeroed on every member.
  2. Some have one ⇒ `mdadm --assemble` with those; then the sysfs read
     of the array (below) and `--add --failfast` any available member the
     array left out (freshly provisioned additions and stale-metadata
     re-adds both land here; with a single available member mdadm itself
     decides whether a degraded start is safe, and a refusal leaves the
     group `RES_STATUS_ERROR`).

  A leg is **available** iff its CN10 converge succeeded this pass — its
  provisioned sides connected, every controller's "address" answered, its
  multipath namespace found and its wrapper built — and that namespace
  has a path that is both live and optimized, probed from sysfs (CN10's
  walk). A `leg_list` member that is not available this pass — whether
  its group was left unassembled ("no available leg", "only k of n legs
  available …"), started degraded without it, holds the array while the
  member reconciliation below skips its `--add`, or runs the array with
  md still holding the member (its side died under the array — the leg
  repair's case, `dnv-worker.md` AR8 — or its path is otherwise no longer
  both live and optimized, or its CN10 converge failed this pass: a
  connect, an "address" read that did not answer, its multipath namespace
  or its wrapper; the reconciliation below leaves such a member held) —
  registers the cntlr for the CN10 background retry, as a failed connect
  does: the worker fans a promotion's `SyncupCntlr` and the sides'
  `SyncupSide` out (`architecture.md`, [D16]; `dnv-worker.md` RW14's
  sides-first hold is bounded and releases the cntlrs whether or not
  every side has reported), so the new primary's first converge can read
  its paths before the sides' ANA flips have reached them, and the worker
  re-syncs on a revision or a reply code, never on a row. Only
  availability counts, and only for a wanted group's members: a standby
  wants no group, nor does a primary whose `sp_level` suppresses its
  groups (CN19); a deferred group (below) never counts, spares are not
  members, and a held member md has failed on a leg that is available is
  not late (Known limits). A `RedundNone` group's leg counts too, although
  its dm-linear (above) is built whatever the leg's availability: the
  layers over it do IO through it — the pool create (CN13) reads the
  pool's metadata through the meta group — and a side that has not
  flipped to this CN yet exports dm-error to it (`architecture.md`, Disk
  node), so a pool over a late meta leg, and
  every layer above that pool, is built only by a later converge. No log
  record of its own: the rows already report it — an md group's error,
  or degraded once a probe reads the array, the error of a layer above a
  `RedundNone` group, and the member's own leg row. The retry runs a
  converge every `CnConnectRetryInterval` for as long as a member stays
  unavailable, each under the cntlr's object lock, which every
  `CheckCntlr` round takes too (CN1): on a wide SP an attempt can hold a
  Check round past the worker's round timeout, and the primary then reads
  unreachable (Known limits).

  **The array is read from sysfs.** `Md.Walk` lists "/sys/block" for the
  array nodes and each array's "md" directory, reading every member's
  "block/dm/name", as the sweep's `ListArrays` does — once per converge
  or Check pass, shared by all of the pass's groups: a listing is an `ls`
  per array, and a walk per group would list every array once per group.
  An array node is "md" followed by digits, or "md_" followed by a name:
  the node mdadm puts the "/dev/md" link on when mdadm.conf says to
  create names, which `architecture.md`, md names, sizes `CnMdDevName`
  for. The walk reads every array on the node, other sps' included, so
  an array whose "md" listing or a member's dm name did not answer — the
  `ls` killed at the soft timeout, a read held past it on the agent's
  `OsClient` semaphore, which every cntlr it serves shares, ctx
  cancelled, or a read failing with an errno other than ENOENT — is
  recorded as unanswered instead of failing the walk: failing this pass
  for another sp's array would turn this sp's md rows `ERROR` — which
  count toward cntlr health — for a fault that is not this sp's. An array
  another cntlr is stopping never reads as unanswered: md removes a
  member's "block" link as it unbinds it, so that member's dm name reads
  ENOENT and the member is recorded with no dm name, which no group's
  names match; an "md" directory that went makes its `ls` answer "no"
  and drops the array. The sweep's `ListArrays` reads such an array as
  foreign while a member is unbound, but not for the whole stop: from the
  moment md marks the array deleted until its "md" directory goes,
  "array_state" reads EBUSY, and `ListArrays`' strict rule fails that
  pass's md enumeration — a Leftover the worker re-drives, from a sweep
  that removed nothing (CN21). By default md marks it when the stopped
  array's last reference goes — a close, or the end of a read of one of
  its "md" attributes, whichever is last — after its member directories
  are gone; with the md module's legacy asynchronous gendisk deletion
  switched off it marks it in the stop itself, while the unbound member
  directories are still there. The walk reads no "array_state" and is
  unaffected. Only a "/sys/block" listing that did not answer fails the
  walk. After an assembly `Md.Refresh` lists "/sys/block" again, drops
  the nodes that went (recorded or unanswered), and re-walks only a node
  that is new, was recorded with no member or as unanswered, or has a
  recorded member directory that is gone, carries another dm name or did
  not answer the check — another cntlr's converge may have stopped an
  array and freed the very node this one's assembly took; a re-walk that
  does not answer drops the node's record and records it as unanswered.
  The check reads each recorded member's "block/dev" and "block/dm/name",
  never its own "state": when another cntlr stops an array or removes a
  member, md unbinds the member by removing its "block" link, and the
  member directory stays until md deletes it, every attribute of its own
  reading ENODEV meanwhile. A check that did not answer is not an error
  of this group: it only makes `Refresh` walk that node again, and a walk
  of it that does not answer records it as unanswered.

  `Md.Detail` takes from the walk the one array whose members include a
  wrapper of the group's `leg_list`. Spares are not keys: a leg switched
  out into `spare_leg_list` is an extra, found through the member that
  stays. No such array is "absent" and runs the assembly above — unless an
  array of the walk is unanswered, which may be the group's own: then no
  answering array proves absence, the group's error names the unanswered
  array, and nothing is created or assembled. Only an `ls` of an array's
  "md" directory that did not report (killed, never started, refused by
  the semaphore), or a member dm-name read failing with something other
  than ENOENT, makes an array unanswered; a foreign array's non-dm member
  reads ENOENT and is recorded, so a foreign array cannot hold an
  assembly off for good. Nothing re-drives the refused assembly either —
  unless the same pass registers the CN10 retry for something else (any
  of CN10's triggers, among them a `leg_list` member of any group of this
  cntlr that is not available, above), whose next attempt is a whole
  converge and tries the group again: the group's error is a row, not a
  reply code (CN29), the error itself registers no CN10 background retry,
  and the Check verdict reports a leftover (a reply code the worker
  re-drives) only while its own enumeration, `ListArrays`, still fails —
  so the group stays unassembled, its row `MISSING` on the Check rounds
  after the array answers, until the cntlr's next converge for some
  other reason (Known limits). Beside an answering match an unanswered
  array is left alone (it cannot be told from another sp's array whose
  read was cut off; the one thing it hides is a second array of this
  group). Two answering arrays are an error; a member with no dm name (a
  foreign member) of the matched array is an error. Of that array it
  reads "array_state" first and then, for a running array only,
  "degraded", "sync_action" and "sync_completed" — an inactive array has
  none of the three, and its members read a bare spare — and each
  member's "state" (a flag list, in_sync and failfast on a healthy dnv
  member) and "block/dev". Running means "array_state" is clean, active,
  active-idle, write-pending, readonly or read-auto; an array in any
  other state (inactive, broken, …) is left exactly as it is and its
  state is the group's error. A read of the matched array that did not
  answer is an error, never absent. A matched array whose "array_state"
  has gone by that read (stopped since the walk) reads absent — unless an
  array of the walk is unanswered, which makes it the same error as no
  match: absent always means every array of the walk answered.

  **Member reconciliation** covers `SwitchSpareLeg` with no extra
  mechanism: a member the array holds — its "block/dm/name", compared
  with the names of the `leg_list` wrappers; no `lsblk`, because an
  `lsblk` of a wanted wrapper that did not answer would otherwise read as
  "not wanted" and fail and remove an in-sync member — that is no longer
  in `leg_list` is `--fail`ed and `--remove`d by the dm path sysfs named;
  an available `leg_list` member the array lacks is `--add --failfast`ed
  (md then resyncs — a bitmap catch-up for a briefly absent leg, a full
  rebuild for a promoted spare). Every `leg_list` name is wanted,
  available or not: a leg this pass cannot use (no path both live and
  optimized, or its CN10 converge failing: a connect, an "address" read
  that did not answer, its multipath namespace or its wrapper) is never
  added, and a member md still holds for it is neither failed nor
  removed. `--fail` and `--remove` open the member's path (no IO) and act
  on its device number. A `--remove` issued within seconds of the
  member's side dying, while a superblock write is stuck on that member,
  sleeps in md's suspend until the write fails at the path's error
  recovery, so it can be killed at the soft timeout with the member still
  held: that pass reports the group `ERROR` and does not reach the
  promoted spare's `--add --failfast` either, because extras leave before
  promotions arrive. Nothing remembers the failure, and nothing schedules
  another converge for it: the group's error is a row, not a reply code
  (CN29), so the worker does not re-drive it, and it registers no CN10
  background retry (a leg that failed to converge does, CN10, and so
  does a clone source whose connection failed or another clone failure
  CN18 lists, and so does a `leg_list` member that is not available —
  above — which is cntlr-wide: any such member of any group of this
  cntlr registers it, and the retry finishes the switch, the first of its
  attempts that finds the promoted spare available adding it; the trigger
  includes the promoted spare if it is not available yet, or, when the
  switched-out leg's whole DN died, another group's leg still on that DN
  (the leg repair switches one leg per pass, `dnv-worker.md` AR8), while
  the switched-out member itself never counts, since it has left
  `leg_list`); later Check rounds read the running array `OK`, degraded.
  The switched-out member stays held and the spare stays out until the
  cntlr's next converge, for whatever other reason it runs (a
  `SyncupCntlr` for a revision bump or a non-zero reply code, an agent
  restart's CN2 re-run, a background retry registered for something
  else), which removes the member in milliseconds and adds the spare —
  or, while the spare is not available yet, leaves its `--add` to the
  retry that the late spare registers (Known limits). The cn agent never
  runs mdadm --zero-superblock: a leg only ever leaves an array into the
  spare list (where a stale superblock makes a later re-add cheap) or out
  of existence with its side.

  A group whose non-spare `leg_list` holds a provisioning leg (CN9) is
  **deferred**: none of `--examine`, `--create`, `--assemble`, `--add`,
  `--fail` or `--remove` runs, no `CnGrpName` is built, and
  `grp_id_to_md_raid` reports `RES_STATUS_PROVISIONING`. The assembly
  cases above are evaluated only once every non-spare leg of the group is
  provisioned. (An unprovisioned spare never defers the group — spares are
  not members, `architecture.md`, Spare legs.) Deferral here is decided by
  the group's own legs only: a group that CN9's prefix truncation keeps
  out of its slice's concat because an earlier group is deferred still
  assembles its array normally — it is the concat that waits, not the md
  layer (CN13).

**What a killed mdadm means.** A run that did not answer (killed at the
soft timeout, never started, ctx cancelled: `agent.Reported` is false) is
an error and must never read as absent. `Md.HasSuperblock`
(`mdadm --examine`) is the sharp case: `--examine` opens and reads the
member, so on a leg whose DN side has gone it blocks until failfast and
is killed, and a killed run read as "no superblock" would send the
assembly into case 1 and `--create --assume-clean` over live data. It
answers with a flag and an error, and an unanswered probe aborts the
whole assembly — with no answer this pass cannot tell case 1 from case
2, and guessing case 1 is destructive. `--examine` has a second unsafe
answer, one this rule does not catch: once the path's failfast has
expired the read fails with an IO error instead of waiting, and mdadm
answers exactly as it does for a member with no superblock, with the
same message and exit status. `HasSuperblock` reads that as "no
superblock", and nothing in the answer tells the two apart. Only a leg
that read available earlier in the same pass is probed, and a path past
its failfast is no longer live, so the answer needs the failfast to
expire between that read and the probe's answer. With a superblock on
another member the assembly takes case 2 without this one. Case 1 needs
no member to answer with one — every `leg_list` member available, each
fresh or answering so — and its `--create --assume-clean` then writes to
the member whose read failed: into the same failing IO or, if a path has
reconnected in between, over its live data (Known limits).

**Nothing in the agent runs mdadm --detail** (CN21, CN28). It loads the
superblock from the first array member that opens; when that member's DN
side has gone, the read sits in the multipath head's requeue list until
the path's failfast expires — the controller stays live until its
keep-alive times out, error recovery starts after it, and the fast IO
fail timeout runs from the reconnect that follows — far past the soft
timeout. A Check round's probe killed that way would read the group's
row `ERROR` for a member fault the leg row already reports, and that row
counts toward cntlr health: it would fail the primary over. What the
sweeps, the member reconciliation and the md rows need about an array
they read from sysfs, which touches no member device and therefore
cannot block on a dead leg (of the md probes, only the assembly's
`--examine` above still reads a member, and only an available leg's):
`ListArrays` walks "/sys/block" for the array nodes (numbered or named,
above), reads each array's "array_state", and names each member through
its "block/dm/name" under the array's "md" directory — which is what
attributes an array to an sp, and a member with no such attribute is not
a dm device at all. `Md.Walk` makes the same listings and dm-name reads
(not "array_state"), once per pass for the md groups (above), under a
different rule for an array that did not answer: `ListArrays` keeps the
strict one — one array whose "array_state", "md" listing or member dm
name did not answer fails the whole md enumeration, because a removal
decision needs the whole node; that is the sweep's "enumeration failed:
md arrays …" and a `ReplyCodeLeftover`, never a row, and the worker
counts a Leftover reply as accepted, so it does not reach cntlr health —
while `Md.Walk` records such an array as unanswered. `Md.Gone` verifies a
stop from the same "array_state": only an absent directory or clear
counts, never inactive, which is an assembled-but-not-running array that
pins its members just as hard. A read that fails is an error, never gone
— the EBUSY a stopped array's "array_state" reads until its "md"
directory goes (above) included: `stopArrayVerified` logs that verifying
an md stop failed and reports the array as a Leftover, which the worker
re-drives. "mdadm --detail --scan" is deliberately not the enumerator: it
loads superblocks. The node a sweep stops is the one sysfs named — never
the `CnMdDevName` link under "/dev/md", which depends on udev having run.

CN13. Per-slice pools (`pool.go`; primary only). Per slice of
`id_to_slice`: the multi-target dm-linears `CnPoolMetaName` (meta group
devices, `meta_grp_list` order) and `CnPoolDataName` (data group
devices), tables via stdin; then the thin-pool `CnPoolFinalName` with the
block size in sectors and the low-water mark derived from the data
device's blocks and `low_water_mark_pct` as `architecture.md`, Primary
cntlr, states — a zero percentage never reaches that arithmetic, since
CN8's conf gate refuses the request before any planning runs; above one
hundred passes a zero mark; nothing is clamped in either direction. A
fresh pool needs its metadata to read zero, and it does: a side of a meta
group is zeroed over its whole leg span before it is ever exported, with
an actual write of zeros rather than a discard whose read-back is
hardware-optional (`architecture.md`, Side provisioning protocol).

The pool table carries no feature arguments, so dm-thin's defaults
apply. dm-thin then writes every block the pool provisions whole before a
host can read any of it, because the table never asks it to skip block
zeroing (the "skip_block_zeroing" feature, which `architecture.md`, [D15],
rules out): that is what keeps the never-zeroed part of a data leg
unreadable, and what CN12's `--assume-clean` rests on. A pool out of data
space queues writes and then fails them when its own timeout runs out
(`architecture.md`, Known limits).

`GrowSlice` arrives as longer group lists: the converge reloads the
concats with the appended targets and reloads the pool table with the
new sizes — dm-thin picks up both data and metadata growth from the
table swap; no pool message is involved. A deferred group (CN9) is left
out of both concats and out of the pool sizing until it clears, and so
is every group after it in the same list — the concat takes each list's
leading run of non-deferred groups, never a filtered subset, because a
target's offset is the sum of the lengths before it and a re-inserted
middle target would move every later group's data under a live pool.
Concat and pool tables are therefore built and probed at the effective
size, so a not-yet-grown pool is `RES_STATUS_OK` and not a mismatch, and
the grow completes on the converge that follows the last leg's
provisioning — out-of-order provisioning of two appended groups simply
waits for the earlier one. The serving pool's `slice_id_to_dm_pool` row
keeps reporting `RES_STATUS_OK` with its raw `dmsetup status` line
throughout — the auto-grow of `architecture.md`, Automatic reactions,
parses that line, and a `PROVISIONING` pool row would silently switch
auto-grow off. `PROVISIONING` marks only the deferred resources, never
the live ones. A concat may only ever grow, and the agent enforces that
rather than trusting the plan: when the live table totals more sectors
than the desired one, `ensureDmMulti` refuses the reload, and the refusal
is reported the way any unbuildable concat is — "refusing to shrink the
concat" with the live and desired sizes, as an `ERROR` on
`slice_id_to_meta` or `slice_id_to_data`, and "pool concat missing" on
`slice_id_to_dm_pool` — because the target list is the physical layout
of every block the pool above has already allocated: reloading a shorter
one remaps live pool data, after which dm-thin either refuses the resume
and leaves the pool suspended or accepts it and serves the wrong device.
That shape can only mean the effective state lost a group that is
already serving, which the provisioning deferral is never allowed to
produce, so the refusal leaves the live concat untouched for the
reactions or an operator to repair the group.

CN14. Thin volumes (`pool.go`; primary only). Per td and slice:
`CnThinDevName`, with the td's size divided evenly over the slices as its
virtual size, attached by `dmsetup create` of the thin table — preceded
by a pool message only when the control plane has not yet seen the td
materialized. A snapshot message requires a quiesced origin: when the
origin's thin volume device is live, the agent suspends it across the
message and resumes immediately after — a second deliberate, bounded
suspension beyond the window of `architecture.md`, [D12], held only for
the duration of one `dmsetup message`, short of a `dmsetup` command on
it that fails (CN16).

Three cases, decided per td by two request fields:

1. `created` true, any `ori_id` — nobody messages. `dmsetup create` of
   the thin table when the device is absent; the id is known to exist in
   every slice pool (`architecture.md`, sp role).
2. `created` false, `ori_id` zero — `ensureThin` messages the thin create
   of `dev_id` when the device is absent, then `dmsetup create`. EEXIST
   is tolerated: a crash between the message and the create, or a message
   a previous primary already sent.
3. `created` false, `ori_id` non-zero — the pre-pass messages, and only
   it: the snapshot message of `dev_id` from `ori_id` per pool-ready
   slice whose snapshot device is absent, inside the quiesce above when
   the origin's raid0 is live; then the thin loop's `dmsetup create`. A
   failed message is logged and tolerated, because the `dmsetup create`
   that follows decides the outcome: the pool may already hold the id
   after a crashed earlier pass.

Both clauses are re-derivable from the request alone, which is why
`ensureThin` never messages a td with a non-zero `ori_id` whatever the
pre-pass did, and why no handoff between the two is needed.

A created td is never messaged on any pass — fresh primary after a
failover, startup reconcile from the local store (`dnagent.md` SH1 to SH3), a device
removed by hand, ever. `ensureThin` reads the device's info and, when the
device is absent, runs `dmsetup create` directly. If that fails because
the pool does not hold the id, the row reads `RES_STATUS_ERROR` with the
dmsetup output in its details, the worker records `err_epoch`, and no
later converge sends a message either: pool-metadata loss surfaces as an
event for an operator instead of an empty volume under a live `dev_id`,
which is what an unconditional thin create would produce. Standby cntlrs
build no thin volumes and are unaffected. The delete message a td leaving
`td_list` triggers is not gated on `created` (below).

**Cross-slice point-in-time.** The per-slice suspend quiesces one pool's
origin only; a striped td snapshots atomically only if no host write
lands between two slices' messages. When any slice still needs this td's
snapshot message and the origin td's raid0 (`CnRaid0Name`) is live, the
agent therefore suspends that raid0 first, issues every needed slice's
snapshot message (each under its own per-slice origin-thin suspend —
dm-thin's own requirement), and resumes the raid0 afterwards, on the
error paths too, so no device outlives the sequence suspended
(`architecture.md`, [D12]) short of a `dmsetup` command on it that fails
(CN16). Only the messages sit inside the window — the snapshots' own thin
devices are created after the resume, since the content is fixed at
message time. The window is bounded by one message per slice under the
`dnagent.md` SH15 timeouts. The trigger is liveness and nothing else: when the origin
td's raid0 is not live — never built on this cntlr, or already removed —
there is no dnv IO path to quiesce and the messages go unquiesced (at a
pool-suppressing `sp_level` the pre-pass does not run at all, so no
snapshot message is sent, CN19). A deferred origin is not a case of that:
deferral suppresses the raid0's converge, not the device, so a raid0 an
earlier revision built stays live and is quiesced like any other.

**The pre-pass owns every message of an uncreated snapshot.** Its claim
set is every slice whose pool is ready and whose snapshot thin device is
absent; an info error skips the slice, because `ensureThin` will fail it
with the same error. The gate is pool readiness, never the td's deferred
flag — that flag is plan-global (`anySliceDeferred`), and gating messages
on it would drop a ready slice's snapshot message whenever some sibling
slice were still provisioning. There is no filter on the origin's own
thin device: the gateway refuses a snapshot whose origin is not
materialized in every slice pool (`architecture.md`, Thin devices), so
the snapshot message cannot be inverted with the origin's create, and
whether this CN has built the origin's dm device is irrelevant to a
message the pool metadata answers. The quiesce is `suspendSnapOrigin`
when the plan holds the origin, which suspends the raid0 iff it is live —
covering a fresh primary, a plan with nothing built yet and a raid0
someone else holds suspended. An origin absent from the plan means there
is nothing to quiesce, not that the messages belong to someone else.

**Order independence.** Neither the thin loop nor the pre-pass depends on
the relative position of an origin and its snapshot in `td_list`, and the
plan keeps request order with no sort. Same-pass creation of an origin
and its snapshot is not a state the gateway can produce.

**A violated precondition is left to dm-thin.** A snapshot message whose
`ori_id` the pool does not hold fails at the message; the `dmsetup
create` behind it fails; the row reads `RES_STATUS_ERROR` with the
dmsetup output; the td stays uncreated, so every converge retries. No
detection, no distinct status — the origin guarantee is the gateway's
contract to keep, not the agent's to re-check.

A primary crash between two slices' messages still tears the snapshot —
delete and re-create a snapshot whose creation raced a crash
(`architecture.md`, Thin devices). `created` certifies materialization,
not point-in-time consistency: the next primary sends the remaining
messages, every row goes `OK`, and the flag flips.

The persisted `SyncupCntlrRequest` carries `created` too (`dnagent.md`
SH5). Between a td's materialization and the flip's re-sync the stored
copy still says false, which is harmless — the devices exist, so nothing
messages — and is corrected by the bump's higher-revision request.

A td leaving `td_list` is deleted by the sweep (CN21), which is where its
layer order already puts the pieces: its namespaces, ns-devs, raid0 and
dm-error go first because they reference it (L1, L2, L6), then the thin
volume devices (L7), then the delete message of its `dev_id` per slice
pool.

Both halves of that message are read from the volume's own live table —
the thin target's pool device and its `dev_id` — immediately before the
removal, never from the request: the table is what the kernel will act
on. The pool is resolved from a devno back to a dm name through the same
`dmsetup ls` snapshot the sweep enumerated with. A table that cannot be
read (the device may already be gone) skips the message rather than
guess an id.

The message is sent only while this cntlr holds the pool — the pool must
be in the sweep's own wanted set, which takes `wantPool`, and it must
still answer a `dmsetup info` probe: a standby has no pool device, only
the primary may write pool metadata, and a delete against a pool that a
later layer is about to remove is both pointless and unsendable. The
fan-outs where that skips every cntlr (a demote and a delete coalesced
into one revision, a delete at a pool-suppressing `sp_level`, a failover
and delete race) are healed by the **activation sweep**: creating a
slice's pool device arms the sweep, and the first converge whose request
arrived by the revision-gated RPC then enumerates the pool's device ids
through the CN25 reserve, `thin_dump` and release machinery and deletes
every id not among `td_list`'s `dev_id`s — correct because the RPC
request is the newest accepted desired state, `dev_id`s are never reused
and thin ids belong to tds alone. An agent restart under a surviving pool
device does not sweep (nothing else writes the pool, and the CN2
zero-mutation reconcile stays intact), and a startup reconcile that
re-creates the device (a node reboot) arms but does not run it: it
converges from the persisted request, which may lag the pool's true
contents (the converge runs before the persist and a failed persist is
only logged), and deleting against a lagging `td_list` would destroy a
live td — the first revision-gated `SyncupCntlr` after boot runs the
sweep instead. A sweep failure is logged and retried on later converges
until it succeeds once; a stray surviving a crash between creation and
sweep, or a failed delete message, is collected at the pool's next
rebuild.

A whole sp leaving this CN — the pointer removed, or `SP_LEVEL_DISABLE` —
instead only deactivates: the pool is unwanted too, so by the rule above
the thin volumes' removal sends no delete message at all, because the
pool metadata lives on the DN legs and the next hosting CN must find the
thin volumes intact.

CN15. Per-td devices (`td.go`; the primary builds both, a standby only
the error): the raid0 `CnRaid0Name` (dm-striped across the td's per-slice
thin volumes in `slice_idx` order, with its chunk from `stripe_size`) and
the permanent reload target `CnErrorName`, both of the td's size.

CN16. Namespaces and host-facing nvmet (`td.go`, `plan.go`). Per
`Namespace` of every `Subsystem` in `nqn_to_subsystem`, the namespace's
own dm-linear `CnNsDevName` (`architecture.md`, Primary cntlr) with a
backing state machine, evaluated in this order (first match wins;
`sp_level` per CN19):

0. the td is provisioning-deferred (CN9; a side under it is still
   zeroing) ⇒ table → the td's `CnErrorName` (the ns-dev and the nvmet
   namespace exist throughout, `architecture.md`, [D15] — the "rule 0"
   the code comments cite);
1. the namespace is effectively suspended (below) ⇒ table → the td's
   `CnErrorName`, live — the parked rule (`architecture.md`, Namespace
   suspend semantics, and [D12]); never under dm-flakey;
2. standby or disabled cntlr ⇒ table → the td's `CnErrorName`;
3. `sp_level` at or above `SP_LEVEL_NO_THINPOOL` ⇒ → `CnErrorName`;
4. a clone targets the td (a `clone_list` entry whose `dst_td_id` is the
   namespace's `td_id`) and `sp_level` at or above `SP_LEVEL_NO_CLONE` ⇒
   → `CnErrorName` (a raid0 with holes must never serve);
5. a clone targets the td ⇒ → `CnCloneFinalName` (via dm-flakey when rule
   7 applies) — only while that dm-clone's `dmsetup status` shows
   hydration enabled, the mark CN18 step 5 alone sets, after step 4's
   bitmaps (`architecture.md`, Clone crash recovery: they must be applied
   before the dm-clone handles any IO). While it does not — a build or a
   recovery that has not finished, whatever stopped it — the ns-dev gets
   the td's `CnErrorName` instead, parked like rule 1: live, never under
   dm-flakey. Every converge and every probe reads that status afresh
   (`nsDevNow`); nothing is remembered. A read that fails or does not
   answer moves the ns-dev nowhere: it keeps its table, and its
   `ns_id_to_dm_linear` row is `RES_STATUS_ERROR`. The ANA rule below
   does not look at any of this, so the namespace is optimized all the
   same and a host takes IO errors — the recovery's own window (CN18
   step 4) — until a pass's read here finds hydration enabled and the
   ns-dev moves onto the dm-clone. Most CN18 failures that stop the build
   before step 5 has enabled hydration register the CN10 retry that runs
   such a pass (CN18 lists which; a failed allocation or replacement of
   the metadata wrapper does not), and so does any failure to converge
   this ns-dev, this read and its reload onto the dm-clone among them, in
   a pass whose CN18 finished the build (the clone's
   `clone_id_to_dm_clone` row `OK`): the worker re-syncs on a revision or
   a reply code, never on a row, so without it the park a recovery left
   would stay until the cntlr's next converge for another reason;
6. otherwise ⇒ → `CnRaid0Name` (via dm-flakey when rule 7 applies);
7. `sp_level` at or above `SP_LEVEL_READONLY` (primary only): the table
   is a dm-flakey table with the error-writes feature over the rule-5 or
   rule-6 backing — reads pass, writes error (`architecture.md`, [D11]).

Rules 4 and 5 and the `auto_resume` override below combine into one flip
worth stating out loud. An `auto_resume` clone's destination namespace
is stored suspended, which on its own would leave it inaccessible and its
host queueing; the override makes it serve, so below `SP_LEVEL_NO_CLONE`
it is already optimized over `CnCloneFinalName`. At or above
`SP_LEVEL_NO_CLONE` the override still applies (it keys on the
`clone_list` entry, not on whether the clone stack was built), so the
namespace stays exported and stays optimized while rule 4 parks its
ns-dev on `CnErrorName`: what changes is the backing, and the host
therefore takes IO errors instead of queueing. That is deliberate and
consistent with the `SP_LEVEL_NO_THINPOOL` posture (CN19: a host sees IO
errors, not a vanished device); it is not a bug. "Queueing
(inaccessible)" describes only the stored-suspended baseline, never the
state the override leaves behind.

**Effective suspend** (`architecture.md`, Namespace suspend semantics,
Transfers, and Clones): a namespace is effectively suspended iff its
`suspended` field is set or an `xfer_list` entry with `auto_suspend`
names it (`ori_nqn`, `ori_ns_idx`) — unless a `clone_list` entry with
`auto_resume` targets its td, which overrides to not-suspended. (That
override is the flow of `architecture.md`, Transfer + clone = cross-SP
live migration: the destination namespace is created suspended and
serves anyway while the clone runs; `DeleteClone` flips the stored field
to false in its latch transaction, together with the `deleting` flag, so
that the override and the clone's disappearance from the plan arrive in
one syncup. Deferred to the end of the teardown it would leave this
namespace effectively suspended with no `clone_list` entry left to
override it.) An effectively suspended namespace is **parked**: its
ns-dev's table is a dm-linear over the td's `CnErrorName` (rule 1 above,
the same table a standby has) and the device is live; the namespace is
in `AnaGrpIdInaccessible` on every cntlr. Parking: the namespace moves to
`AnaGrpIdInaccessible` first (CN9 pre-step 1), then the ns-dev reload —
which CN9 pre-step 2 performs, because rule 1 has already made the
backing the dm-error, and the build phase then finds the table it wants;
on a pass whose sweep an unanswered listing stopped (CN21) pre-step 2
does not run and `ensureNsDev` makes the same reload itself, still after
pre-step 1. Unparking: the reload onto the backing the remaining rules
select first, then ANA per the rule below — except onto the td's raid0
while a dm-clone the plan does not want may still be live (the pass's L3
did not run, or left one): the namespace then stays parked and
inaccessible, its ns-dev row `ERROR`, until a pass that leaves no such
dm-clone live (CN18). No path of a pass leaves a CN device suspended
unless a `dmsetup` command on it fails, and one such failure leaves it so
on purpose: a reload fails closed (`dnagent.md`, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`). A reload
whose load fails returns the error with the device still suspended on
its old table — any reload, since the rule is `Dm.Reload`'s own: for a
park, resuming that table would put the ns-dev back onto the backing it
retires and replay onto it the IO the suspend absorbed. A resume that
fails can leave a device suspended too. A device found suspended is one
that such a failure, an interrupted `Reload`, or an agent
killed inside CN14's quiesce bracket left, and every path that meets it
resumes it — by a reload or by a bare resume, never by leaving it; one
whose reload keeps failing its load stays suspended, queueing its IO,
until a reload or a resume of it succeeds. An unwanted one that no path
of a pass meets — below a layer the chain stopped at, or anywhere on a
pass whose sweep an unanswered listing stopped (CN21) — is met by the
first pass whose chain reaches it. `ensureNsDev` bare-resumes a device
whose table already matches, and one it holds off the raid0 (CN18) as
well. `parkNsDev` — CN9's pre-step 2,
the park of an ns-dev of the plan, which has a plan to reload from —
reloads one whose table does not match, and one whose table does match
while it is suspended, and that reload resumes it as a side effect.
CN21's P0 is neither of those two: it is `parkByTable`, which has no plan
at all for an ns-dev nothing wants, so it bare-resumes an already-parked
one and reloads only an unparked one. `removeDm` resumes any suspended
device it is about to remove, whatever its table: `dmsetup remove` does
not succeed on a suspended device. `UpdateNamespaceDev` arrives as a
changed `td_id` and is exactly one ns-dev reload — the nvmet
"device_path" never changes.

**nvmet objects** on the node port: per `Subsystem` a subsystem with
"attr_cntlid_min" and "attr_cntlid_max" from `Cntlr.cntlid_slot`, written
in the order `architecture.md`, cntlid slots, states (an unchanged range
writes neither; the transfer subsystem's range, CN17, moves the same way),
"attr_serial" and "attr_model"
verbatim from the record (the gateway stamped them, `architecture.md`,
[D2]), the "attr_allow_any_host" write and the host links per
`allowed_hosts` as `architecture.md`, Primary cntlr, states (an empty list
admits no host); per `Namespace` an nvmet
namespace whose nsid is `ns_idx`, whose "device_path" is its own ns-dev,
with "uuid" and "nguid" from the record. A namespace that has left `ns_list`
under a subsystem that stays is not the build's to remove: CN21's L1 alone
removes it (and so nothing does under a subsystem whose NQN carries the dnv
prefix but decodes to nothing, which the sweep never attributes —
`architecture.md`, dm-device kinds, and Common validation), `AnaGrpIdInaccessible`
first, after P0 has parked or resumed the ns-dev under it — or failed to,
which does not stop L1 (CN21) — and only from an enumeration that answered.
A pass in which one did not — one of the four listings, or the sweep's own
listing of that subsystem's namespaces — leaves it where it is with a non-OK
verdict, and the next pass whose listings answer removes it.

**ANA**: `AnaGrpIdOptimized` iff primary and not disabled and not
effectively suspended and its backing chain is not provisioning-deferred
(CN9) and the `sp_level` is below `SP_LEVEL_DISABLE` (CN9's `wantAny`);
else `AnaGrpIdInaccessible` (single "ana_grpid" writes, `dnagent.md` SH19). The
deferral conjunct is what makes initial provisioning painless for hosts:
while the td's legs are still zeroing, the namespace stays inaccessible
and hosts queue on the path instead of eating IO errors from an
error-backed ns-dev; it flips to optimized on the converge that follows
the last leg's provisioning. A deferred namespace's own rows
(`ns_id_to_dm_linear`, `ns_id_to_namespace`) report
`RES_STATUS_PROVISIONING` — the ns-dev and the nvmet namespace do exist,
on the permanent dm-error, but reporting them `OK` would show a fully
healthy `CntlrInfo` while no host can do IO.

CN17. Transfers (`xfer.go`; both roles; `architecture.md`, Transfers).
Per `xfer_list` entry: resolve the origin namespace by `ori_nqn` and
`ori_ns_idx` in `nqn_to_subsystem` (unresolvable ⇒ the transfer's
resources report `RES_STATUS_ERROR` and the pass continues, CN29).
`CnXferFinalName`: on the primary a dm-linear over the origin td's raid0
(→ dm-error at `sp_level` at or above `SP_LEVEL_NO_THINPOOL`); on a
standby a plain dm-error table of the same size. Subsystem `XferNqn` on
the node port: "attr_serial" from the transfer id and "attr_model" the
fixed dnv model (the rule of `architecture.md`, [D2], applied to the
transfer — every cntlr exports the same identity so the destination clone
sees one multipath device), "attr_cntlid_min" and "attr_cntlid_max" from
`Cntlr.cntlid_slot`, `allowed_hosts` from the record; one namespace whose
nsid is `ori_ns_idx`, with the origin namespace's "uuid" and "nguid" and
the transfer device as its "device_path"; ANA optimized on the primary,
inaccessible on standbys. The origin namespace's own retirement is
CN16's effective-suspend rule. A cntlr that keeps the transfer device but
stops serving it — demoted to standby, or at a level from
`SP_LEVEL_NO_THINPOOL` up to but excluding `SP_LEVEL_DISABLE`, or the
origin td provisioning-deferred, or, on a primary too, an origin that no
longer resolves (CN29) — reloads the live `CnXferFinalName` onto an error
table of its own size, as CN9's pre-step 3, before any layer touches what
is under it: a linear still mapping the origin td's raid0 holds it open
and the raid0's removal would fail EBUSY (CN19's no-thin-pool level). On
a pass whose sweep an unanswered listing stopped (CN21) pre-step 3 does
not run, and the build phase makes the same reload when it converges the
device — still after pre-step 1 has made whatever ANA move the plan wants
for the transfer's namespace. At `SP_LEVEL_DISABLE` the pre-step still
runs — nothing serves the device at that level either — but it no longer
decides anything: the wanted set is empty, so the transfer device is
removed outright a layer later with every other cntlr-scoped object
(CN19's disable level, CN21's L2). When the plan cannot size that table —
the origin namespace is not in it at all, or it is but its td left
`td_list` in the same request, which leaves the namespace's size unknown
— the size is summed from the device's own live table instead, by
pre-step 3 and, on a pass whose sweep an unanswered listing stopped, by
the build's converge of the transfer, which records the origin error rows
after it; without that fallback the demotion would be skipped and the
departing raid0 never released. A primary is no exception: an unresolved
origin makes `xferServed` false there as well, so the primary demotes the
device like a standby and the transfer no longer holds the departed raid0
against L6 — a linear left serving would keep that raid0's
`dmsetup remove` failing EBUSY on every pass while the transfer stays in
the request. Its namespace stays `AnaGrpIdOptimized` all the same, as it
does at `SP_LEVEL_NO_THINPOOL`: the transfer's ANA group follows the
role, the level and deferral only, so the destination is handed IO errors
from the error table, not a queue. The live table is deliberately not the
plan applied last time: it is the same answer, from the kernel, with
nothing remembered. A transfer whose origin td is provisioning-deferred
(CN9) is deferred with it: its three rows report
`RES_STATUS_PROVISIONING` and its namespace stays `AnaGrpIdInaccessible`
— mapping a linear over a raid0 that does not exist yet would only
produce an `ERROR` and an `err_epoch`.

CN18. Clones (`clone.go`; primary only; `architecture.md`, Clones;
`sp_level` below `SP_LEVEL_NO_CLONE`). Per `clone_list` entry, in order:

1. `nvme connect` to `src_nqn` at every entry of `src_tr_conf_list` (one
   subsystem, one path per source cntlr; the source's own ANA picks the
   serving path), with the host NQN `CnHostNqn` and the `dnagent.md` SH20 flags;
   failures go to the CN10 retry registry. The source namespace device
   is found by the sysfs walk like CN10, by `src_nqn` and the nsid
   `src_ns_idx`. The connect step is CN10's, drawing on the same pass
   budget the legs drew on: a failed connect is retried within the pass
   while the budget covers the pause, and after a connect this pass made
   the subsystem is re-read until the source namespace is there. With
   the budget spent — the legs may have spent it — a source that did not
   connect, or whose namespace did not appear, fails step 1 ("no
   namespace … on …" for the latter). CN10's unknown-controller rule is
   not applied here: a source controller whose "address" did not answer
   matches no entry, so an entry it alone serves reads unconnected and
   is connected again — a duplicate the host refuses while that
   controller lives, which fails step 1 once the budget is spent — and
   nothing is disconnected, since step 1 retires no path. CN10's rule
   for a walk that did not answer is applied: a listing of
   "/sys/class/nvme-subsystem" or a "subsysnqn" read that did not
   answer, with no other entry naming `src_nqn`, or a failed listing of
   the matching entry's own directory, fails step 1 naming the read, and
   nothing is connected on it; `clone_id_to_dm_clone` then reads
   `MISSING` "source unknown: …" naming the read, where step 1's other
   failures read "source not connected". A source whose disconnect a
   sweep has set going is neither adopted nor connected while that
   disconnect runs: step 1 fails naming it, and the retry reads the
   source afresh once the disconnect has returned (CN10's disconnect
   registry).
2. Allocate the clone's metadata slot from the arena of Names and
   constants in `common`: `cloneMetaUnits` contiguous units — the
   dm-clone superblock
   plus one byte per region, with the region count the td's size in
   blocks, rounded up to whole units (one byte per region over the
   superblock is a generous bound — the dn agent budgets metadata the
   same way) — first-fit over the free ranges of the used map
   reconstructed from the clone-metadata wrapper tables (CN5). Exhaustion of
   the arena ⇒ `clone_id_to_meta` reports `RES_STATUS_ERROR` with the
   allocator's message and `clone_id_to_dm_clone` reports
   `RES_STATUS_ERROR` "metadata wrapper missing". **The arena is per CN.**
   `CnTmpFilePath` and `CnCloneMetaDmPrefix` are keyed by the cluster and
   cn ids, so the arena's units are shared by every clone of every cntlr
   of the node (up to `MaxCntlrCntPerCn` of them), and the CN's clone
   ceiling is the arena's unit count divided by the units per clone,
   summed over the whole CN — the superblock term alone costs one unit, so
   every clone costs at least two, and large tds at small block sizes cost
   far more. The agent reports exhaustion and prevents nothing; admission
   against this ceiling is `architecture.md`, Clones. Enumerate, discard and
   create are one critical section under `cloneMetaMu` (Server type and
   lock mapping): two cntlrs of the same CN converge concurrently under
   the node read lock, and two enumerations could otherwise pick the
   same free run and — because the two `dmsetup create`s use different
   names — silently share one metadata range. Removing a clone-metadata
   wrapper belongs in that same critical section, for the same reason:
   the registry being the kernel's dm tables, a removal is a mutation of
   it — the units are free the moment the table is gone — and a sweep
   that deleted a wrapper in the middle of another cntlr's enumerate,
   discard and create would both invalidate that enumeration and free a
   run under it. So the path that removes a wrapper from outside the
   allocator — the CN21 sweep's L4 — takes `cloneMetaMu` around that
   `dmsetup remove`, while the two that already hold it (this
   allocation's mismatched-wrapper removal, and the arena sweep of CN2)
   remove directly: the mutex is a leaf, held across those OS calls and
   never re-entered. Before creating a newly chosen range, punch it on
   the loop device with a `blkdiscard` of the range.
   That is the recycled-unit guard, because a freed unit still holds the
   previous clone's valid dm-clone superblock, which a new dm-clone would
   misparse. On a tmpfs-backed file a hole punch is zero-guaranteed by
   file semantics (no device discard feature involved) and frees the
   pages; `--zeroout` is forbidden here — it would materialize up to the
   whole arena in RAM and defeat the sparse-file design. A wrapper that
   already exists and matches is never re-discarded: "allocate" strictly
   means "a new unit range was chosen", and re-punching would wipe a live
   superblock. Then `dmsetup create` the wrapper `CnCloneMetaDmName` with
   a linear table over the loop device's major:minor at the range's
   offset (the backing device is written as its major:minor, and read
   back the same way from `dmsetup table`); the dm-clone's metadata
   device is that wrapper, because dm-clone reads its superblock from
   sector zero and takes no offset (the same reason `DnMigrMetaDmName`
   exists). A wrapper whose table no longer matches (wrong length, or
   backed by something other than the currently probed loop path — a
   tmpfs remounted under a live agent) is removed and reallocated by the
   converge, after the dm-clone above it has been removed, so the
   removal does not fail EBUSY (a dm-clone whose removal failed keeps its
   wrapper mapped, which then cannot be replaced — below); that is the
   rebuild path of `architecture.md`, Clone crash recovery. On a recovery
   build (metadata missing or unusable, the dm-clone device gone, or a
   dm-clone that does not show hydration enabled — step 4) this step
   first parks the destination td's ns-devs on `CnErrorName`
   (`parkTdNsDevs`, no ANA move), removes the old dm-clone if one is
   still present, and only then allocates a missing wrapper or replaces
   a mismatched one — a matching wrapper is left alone. An old dm-clone
   whose removal fails is kept: step 3 converges it like any existing
   dm-clone, and step 4 still applies every bitmap to it before step 5
   (its wrapper matches — a mismatched one cannot be replaced while the
   dm-clone still maps it, and that failure ends this step). The
   `dmsetup status` read that decides the third case is step 2's own;
   when it fails or does not answer, step 2 fails exactly as on a failed
   `dmsetup info` of the dm-clone — `clone_id_to_meta` and
   `clone_id_to_dm_clone` `RES_STATUS_ERROR` (CN29) — the pass recovers,
   removes and enables nothing for that clone, and it registers the CN10
   retry. CN16 reads the status again itself and moves no ns-dev onto a
   dm-clone that does not show hydration enabled (rule 5), so a td a
   killed build left parked stays parked until a pass that can read the
   status has recovered the clone.
3. The dm-clone `CnCloneFinalName`: metadata = the step-2 wrapper,
   destination = the destination td's `CnRaid0Name`, source = the
   connected device, region size = one block in sectors, created with
   the two features "no_hydration no_discard_passdown" always — the
   second feature is not optional: `blkdiscard` is this design's
   metadata-only "mark hydrated" primitive (`architecture.md`, Bitmap
   push protocol, and raid0 bitmap math), and with passdown enabled
   dm-clone also remaps the discard to the destination, unmapping the
   very blocks step 4 says are already there; `agent.CloneTable` carries
   the parameter and every caller passes it, because a `blkdiscard` must
   stay a metadata-only mark for the dn migration clone as for the cn
   clone. Then the `hydration_threshold` and
   `hydration_batch_size` messages from `dm_clone_conf`, one per non-zero
   member the probed status does not already show (probe-first, `dnagent.md` SH16; a
   zero leaves the target's own default in place, `architecture.md`,
   Common validation).
4. On a pass whose step 3 created or reloaded the dm-clone, or that is a
   recovery, apply every bitmap chunk of the applied set (`dnagent.md` SH21)
   — the CN22 fold over the `(src_slice_idx, bm_idx)`-addressed chunks
   this node holds, read in place — and, when this build is a **recovery**
   (`architecture.md`, Clone crash recovery: the dm-clone's metadata is
   missing or unusable — a CN reboot takes the tmpfs, the loop device and
   every clone-metadata wrapper together, or a failover to a CN that never ran
   the clone; or the dm-clone device itself vanished while a healthy
   wrapper stayed behind; or the dm-clone is up but its `dmsetup status`
   does not show hydration enabled — "no_hydration" among its feature
   args, or a status that is not a dm-clone's: step 5 alone enables
   hydration, so an agent killed between steps 3 and 5 leaves exactly
   this, with step 4 perhaps half done, and so can a pass that failed
   between them without removing it, for example through a create killed
   after its ioctl ran, a refused enable, or step 4's fail-closed removal
   failing too; a removal that succeeds leaves the dm-clone gone instead,
   the vanished-device case above), first the destination bitmaps: with
   every affected ns-dev parked on `CnErrorName` (by CN9's pre-step 2
   when the namespace is effectively suspended or the cntlr is standby
   and no unanswered listing stopped the sweep (CN21), and otherwise by
   step 2 of a recovery build, before the old dm-clone is removed —
   `parkTdNsDevs` takes a serving namespace off the td with no ANA move,
   and that IO-error window is this recovery's own), read the td's
   mapping bitmap from every slice pool (the CN25 machinery, the
   destination side of `architecture.md`, raid0 bitmap math) and
   `blkdiscard` every mapped region. Mapped means already copied because
   the destination td started empty (`architecture.md`, [D3]).
   Destination bitmaps not applied in full — a read or a `blkdiscard`
   that fails — fail the clone closed: the dm-clone is removed,
   `clone_id_to_dm_clone` reads `RES_STATUS_ERROR` "destination bitmaps
   not applied: …", and the CN10 retry is registered; a dm-clone whose
   removal fails there — as it may when step 2 could not remove it either
   — stays up with hydration off, which CN16 does not serve through (rule
   5).
5. The `enable_hydration` message to the dm-clone (`dmsetup message`) — only after step 4, so a
   recovered clone can never re-fetch a region the destination already
   owns (the staleness hazard of `architecture.md`, Clone crash
   recovery).
6. Put the destination td's ns-devs onto the dm-clone and set ANA per
   CN16 — a reload when they already exist (recovery, enable
   transitions); a fresh converge simply creates them in CN16 with the
   clone backing (rule 5), which CN16 installs only once the dm-clone's
   status shows the hydration step 5 enabled. With `auto_resume` false
   the namespaces stay effectively suspended until
   `UpdateNamespaceSuspended`.

Any failure of step 1 registers the CN10 retry, and so do these later
ones, each of which stops the build before step 5 has enabled hydration:
the arena listing, step 2's `dmsetup info` or `dmsetup status` of the
dm-clone, anything step 3's converge of the dm-clone fails on, step 4's
destination bitmaps, and the enable (its own status read included). So
does a failed `dmsetup status` read after the enable, whose line the
dm-clone row carries (`architecture.md`, Live-state reporting). The one
other failure of steps 2 to 5 that stops the build — of step 2's wrapper
allocation or replacement (`ensureCloneMeta`), the arena's refusal of the
slot among them — registers nothing and is left to its rows:
`clone_id_to_meta` `RES_STATUS_ERROR` and `clone_id_to_dm_clone`
"metadata wrapper missing". Some failures stop nothing and are only
logged: step 2's failed removal of an old dm-clone (which is then kept,
above), a failed knob message of step 3 (the knob keeps the value it
had) and a failed source-chunk `blkdiscard` of step 4 (it costs an extra
copy). Step 6 is CN16's: in a pass that finished steps 1 to 5 and the
read after them (the dm-clone row `OK`), a failure to converge a rule-5
ns-dev of the td registers the retry too (CN16 rule 5). Whatever stopped
a pass, CN16 puts the td's ns-devs on the dm-clone only while its status
shows hydration enabled (rule 5), which no dm-clone shows before step 5
has run on it; a later pass — the retry's, where one was registered —
finishes the build.

**Removing a clone** is not a step of its own: it is the CN21 sweep
finding the clone's objects unwanted and taking them in layer order,
which is exactly the order this stack needs. A clone that left
`clone_list` loses all three; one the role or the level merely
suppresses loses the dm-clone and the wrapper only — the source
connection stays, because L5 reads "in use" off every stored cntlr's
`clone_list` (plus the live dm-clone tables), and a suppressed clone is
still in that list.

* The ns-devs above the destination td go first. A wanted ns-dev whose
  live table still maps the dm-clone is parked on the td's `CnErrorName`
  by CN9's pre-step 2 — it has to be, or the removal below fails EBUSY —
  and the build phase of the same pass then puts it on whatever CN16 now
  wants: the raid0 while this cntlr still serves it (rule 6), the
  `CnErrorName` otherwise (a standby; a still-listed but level-suppressed
  clone at or above `SP_LEVEL_NO_CLONE`, rule 4; or at or above
  `SP_LEVEL_NO_THINPOOL`, rule 3). An ns-dev that is itself unwanted is
  parked by P0 and removed at L2. On a pass whose sweep an unanswered
  listing stopped (CN21) neither pre-step 2 nor L3 runs, so the dm-clone
  stays loaded, and one whose hydration is on and not finished goes on
  copying into the raid0 whether or not anything has it open: its copy
  of a region not yet hydrated would overwrite a write a host made to
  that region of the raid0 directly. A pass whose listings answer can
  leave it loaded too: L3's remove fails or is killed — behind an ns-dev
  whose park failed its load, say, which stays suspended over the
  dm-clone (CN16) — or the descent stops above L3. So while a dm-clone
  the plan does not want may still be live — L3 did not run or left one;
  on a stopped pass, the dm listing did not answer or names one — the
  build puts no ns-dev onto a td's raid0 it is not on already. One over
  the dm-clone stays there and serves on through it, in the ANA group it
  has. One parked stays parked and goes inaccessible: pre-step 2 may
  have parked it on this very pass while it was optimized, and its hosts
  should queue rather than take IO errors. A new one is created parked.
  None of them is moved to optimized on that pass; each reports its
  ns-dev row `ERROR` in the probe's words ("table is not the desired
  namespace backing"), and the hold registers the CN10 retry, because a
  hold that an unanswered `dmsetup ls` alone caused leaves nothing for
  the Check verdict to name once the listing answers again. The first
  pass whose L3 removes the dm-clone takes the order above — the park of
  one still over the dm-clone, L3, then the reload.
* L3 removes the dm-clone, before its source connection dies — dm-clone
  flushes through the source on removal and blocks without it.
* L4 removes the metadata wrapper `CnCloneMetaDmName` under
  `cloneMetaMu`; its units are free again the moment the wrapper is gone,
  because the next registry enumeration simply no longer sees them and
  nothing is written.
* L5 disconnects the source subsystem — the third object, and the one
  only a clone that has left `clone_list` loses (by NQN is safe here:
  every path of it is going). The disconnect runs off the pass (CN10), so
  the connection is that pass's leftover until a later pass finds it
  gone, and a clone created on the same source meanwhile waits for it
  rather than adopt it (step 1). The set of unowned sources is computed
  at L5 rather than when the chain was built, because until L3 has
  actually removed it the dm-clone still maps its own source and would
  keep it claimed.

The clone's chunk files are not part of that chain. They are
swept separately, at the end of the cntlr-level sweep (CN21), against
the stored `clone_list` (`dnagent.md` SH7) — a clone that left the list loses them,
one the role or the level merely suppresses keeps them applied-by-file
(CN19, CN22 — deleting them on every standby converge would make the
worker re-push them forever, and a promoted standby's recovery rebuild
would have nothing to skip with). Keying that deletion off the wrapper's
removal instead would leak every deleted clone's chunks on a standby,
which builds no wrapper at all. A clone whose destination td is
provisioning-deferred (CN9) is never built in the first place: no
metadata slot, no dm-clone, no source connect, and its three rows report
`RES_STATUS_PROVISIONING`.

CN19. `sp_level` gating (`architecture.md`, SpLevel; numeric comparisons
— the enum values are ordered). Levels are desired state, and they act on
the converge through the wanted set alone (CN9): raising a level shrinks
it, so the sweep phase takes the layers down; lowering it grows the set
again and the build phase rebuilds them. There is no level-specific
teardown code anywhere. Bitmap chunks stay applied-by-file throughout
(`dnagent.md` SH21). A resource suppressed by the level is reported
`RES_STATUS_MISSING` with the details `ResDetailsSpLevel` (Names and
constants in `common`; the worker's settle reads it). A resource deferred by CN9's
provisioning gate is a different thing — `RES_STATUS_PROVISIONING` with
the details "provisioning", at every level — and where both apply, the
level wins (CN9). The additional cn behavior per level, each holding at
that level and above:

* `SP_LEVEL_READONLY`: every user-facing ns-dev on the primary carries the
  dm-flakey error-writes table over its normal backing (CN16 rule 7) —
  reads served, writes error (`architecture.md`, [D11]); the exemption of
  an effectively suspended namespace and what the level leaves running
  are `architecture.md`, SpLevel. Transfers and health probes are
  unaffected.
* `SP_LEVEL_NO_CLONE`: no clone stacks: the dm-clone and its metadata
  wrapper are removed (the wrapper's arena units freed), but a
  clone-source connection made at a lower level stays connected, and is
  not even reported as a leftover — the sweep's in-use test is
  level-blind (CN21: a source is in use while any stored cntlr's
  `clone_list` names it, which `sp_level` does not shorten, or a live
  dm-clone table maps it), so it is disconnected only once the clone
  leaves `clone_list` or the cntlr's stored state goes; the ns-devs of
  clone-target tds go on `CnErrorName` (CN16 rule 4), so an `auto_resume`
  clone's destination namespace stays exported and optimized and its
  host takes IO errors instead of queueing (CN16).
* `SP_LEVEL_NO_THINPOOL`: no thin pools, thin volumes, raid0s or pool
  concats; every ns-dev and every primary transfer device on
  `CnErrorName` or an error table; namespaces stay exported with CN16's
  ANA (a host sees IO errors, not a vanished device).
* `SP_LEVEL_NO_REDUND`: no group devices (md arrays stopped, `CnGrpName`
  linears removed); legs stay connected, wrapped and probed.
* `SP_LEVEL_NO_MIGRATION`: nothing — the level has no CN-side behavior
  (the migration dm-clone is a DN object; the CN's leg multipath needs no
  gating).
* `SP_LEVEL_NO_SIDE`: no leg connections or wrappers (their sides are no
  longer exported) and no health probes; host-facing and transfer
  subsystems remain, error-backed.
* `SP_LEVEL_DISABLE`: the wanted set is empty, which is the whole of this
  level: the CN21 sweep removes every cntlr-scoped object of the sp, and
  only the base state of `architecture.md`, Controller node, common,
  remains (tmpfs, backing file, loop device, port — every clone-metadata
  wrapper of this sp went with its clone, so the units they held are free
  again; the arena itself is node-wide and another sp's wrappers stay).
  The cntlr file and chunk files stay — desired state persists. Two
  things an empty wanted set cannot express are done explicitly beside
  it, because neither is an object on the node: the CN10 and CN18 connect
  retry is cancelled, and every `ResInfo` history of the cntlr is dropped.

CN20. Persist (`dnagent.md` SH5); reply `agent_reply`, `revision`, `cntlr_info`,
`bm_info_list`.

**`agent_reply` is the sweep's verdict.** It is `ReplyCodeLeftover` iff
this cntlr's sweep was not clean — the node still holds an object of this
sp that the desired state does not want, or one of the enumerations did
not answer — and zero otherwise. The details list the leftovers by kind
and name, a bounded number of them with a tail saying how many more,
plus one "enumeration failed" entry naming each unanswered enumeration;
the full list goes to the agent log every pass, in the leftover record
of `log.md`, Leftovers, so a lingering leftover is visible every round
and not only once. The code means accepted with residue, not a rejection
(`architecture.md`, Teardown by sweep), and `revision` is the request's
own. Nothing about it is stored: it is recomputed by enumerating the node on
every `Syncup*` and every `Check*` (CN30). A cntlr the conf gate refused
has no verdict at all (CN8): nothing was converged and nothing
enumerated, so the reply carries `ReplyCodeInvalidConf` and the sweep
never ran.

`bm_info_list` is one `BitmapInfo` per `clone_list` entry of the
(now-stored) request, its `res_id` the clone id and its `chunk_id_list`
derived from the chunk files present, save those CN2's reload
left unloaded (`dnagent.md` SH21): one `BmChunkId` of `src_slice_idx` and `bm_idx`
per chunk this node holds for that clone, ascending by the pair.
`bm_idx_list` is left unset — that field is the migration applied set
(`SyncupSideReply.bm_info`), and a flat index cannot name a
pair-addressed chunk. A clone with no chunks reports a `BitmapInfo` with
an empty `chunk_id_list`, which is what tells the worker (`dnv-worker.md` BM2) to push everything
etcd holds.

### The sweep

CN21. Two scopes, one chain. The principle — removal is actual minus
desired, verified by probe, with nothing about a past failure remembered
— is `architecture.md`, Teardown by sweep, and is not restated here. The
cn agent runs it at two scopes:

* **node-level**, in `SyncupCn` (CN7) and the startup reconcile (CN2),
  under the node write lock: every sp that appears on the node and is
  not in the stored `cntlr_pointer_list` — wanted set empty — plus the
  cross-sp objects of the attribution rules below. The write lock is
  what makes judging an unowned object safe: no cntlr converge, no Check
  round and no push runs beside the pass. A disconnect the pass sets
  going can complete after the lock is released (CN10), so it is CN10's
  registry, read by the CN10 and CN18 connect steps, that keeps a
  converge from adopting that connection meanwhile.
* **cntlr-level**, in `convergeCntlr` as CN9's sweep phase, under that
  converge's CN1 locks — node read plus that cntlr's
  object lock on the `SyncupCntlr` and connect-retry paths, the node
  write lock on the startup one, which is only stronger: this cntlr's sp
  minus the CN9 wanted set. It removes only objects attributed to its own
  sp, so two cntlrs of one CN sweep concurrently without meeting. The one
  exception is L5's clone-source connections, which belong to no sp at
  all and which both scopes disconnect — safe because "in use" is read
  from every stored cntlr's request (and from the live tables), and a
  cntlr's request is stored before its converge issues any
  `nvme connect`, so a source another cntlr is about to use is already
  claimed; and a converge that finds a source whose disconnect is still
  in flight neither adopts nor reconnects it until that disconnect has
  returned (CN10). One window stays open. Another cntlr's request can be
  stored after this pass has read the stored requests, and that cntlr's
  CN18 step 1 can run before this pass has set the disconnect going. That
  converge finds the source still connected and adopts it, and the
  disconnect then deletes the source under the new dm-clone (Known
  limits).

An sp that is in the pointer list gets no chain of its own at node
level, even with no cntlr file: CN8 introduces the pointer before the
`SyncupCntlr`, and after a lost `--local-store` the resources exist and
must be re-adopted probe-first by that syncup rather than swept. That is
a statement about the sp-scoped chains only. The node-level scope's other
steps — the arena step (CN2) and the unowned subsystems and source
connections — are keyed on the stored cntlr requests, not on the pointer
list, and they do reach an sp that is in it: with the `--local-store`
lost there is no cntlr file, so no `clone_list` names any clone-metadata
wrapper and the arena step attempts every one of them on the node. What
that costs is bounded rather than prevented — a wrapper a live dm-clone
still maps refuses with EBUSY and is only reported, and one that does go
is rebuilt by CN18 on that sp's next `SyncupCntlr` — and it is why the
arena step is named separately instead of reading as one more thing the
pointer list protects.

**Enumeration.** `dmsetup ls` (name to major:minor, which is also how a
live table's device argument is resolved back to a name), the sysfs md
enumeration of CN12 (`ListArrays`, strict: one array that did not answer
fails it — never `Md.Walk`'s unanswered rule), the sysfs subsystem walk
for nvme host connections, and the listing of the nvmet subsystems tree
— linked to the port or not, a partially removed subsystem is unlinked
but present. An enumerator that did not answer leaves its part of the
snapshot empty and is reported as "enumeration failed", and an
enumeration that did not answer licenses no removal (`architecture.md`,
Teardown by sweep): with any one of the four unanswered the sweep
removes nothing from the node at either scope — no chain runs, neither do
CN9's pre-steps 2 and 3, and what the chain would have removed is only
reported. The md enumeration is the sharpest case: unanswered, it would
leave L9 no array to stop and let L10 disconnect unwanted legs from under
a live one. A cntlr-level converge whose sweep is stopped this way still
runs three things that read no snapshot, besides CN19's two explicit
steps at `SP_LEVEL_DISABLE`. One is CN9's pre-step 1, the ANA move: it
decides from the plan alone and removes nothing, and the build phase
that follows parks whether or not the sweep ran — it reloads an ns-dev
whose CN16 backing is the td's `CnErrorName` onto it, and CN17's converge
demotes a transfer device this cntlr does not serve — so without the move
a demoted or suspended namespace would be served from an error table
while its path still reads optimized. The second is the pair of
local-state sweeps ("Not on the node" below): they decide from the
request alone, so an unanswered listing is no reason for them to wait.
The third, ahead of the sweep, is the trim of the CN11 probers (below),
which decides from the plan alone and removes nothing from the node. The
build phase of such a converge in turn holds back the one thing that is
safe only after a step the stopped sweep skipped: while a dm-clone the
plan does not want may still be live it puts no ns-dev onto a td's raid0
it is not on already — that waits for L3 to remove the dm-clone (CN18),
as it does after a chain whose L3 did not run or left one. A namespace
that has left `ns_list` it never touches, stopped sweep or not: that is
L1's alone (below), after P0 (CN16). The snapshot is thrown away at the
end of the pass: it decides only what to attempt, and every removal
re-probes its own object.

**Attribution** follows `architecture.md`, Teardown by sweep — by name,
and by what a nameless object is built out of — with these cn readings.
A dm device is ours by its parsed name (`architecture.md`, dm device
names): the cn role letter (`DmRoleCn`), our cluster, our cn; its sp is
the first id. An md array carries no sp in its name and is ours only when
every member is a leg wrapper (`CnLegName`) of ours naming one sp — an
array with a foreign, unparsable or differently-owned member is silently
skipped and never stopped, which is what leaves a co-hosted dn agent's
udev-assembled array alone. A side-export host connection is ours by the
cn id inside the NQN, which is how it is told from one another CN of this
cluster holds. A clone-source connection of our cluster belongs to no sp
— its NQN names the source sp, not the cntlr that dials it — so the only
test there is is "does somebody here still want it": it is in use iff
some cntlr this process holds names it as a clone's `src_nqn` or a live
dm-clone table maps its namespace device, and unowned otherwise. A
subsystem in the nvmet tree whose NQN is dnv-format says whose it is
outright: a transfer subsystem of our cluster is the transfer export of
the sp its NQN names, and an NQN that carries the dnv prefix but decodes
to nothing is left alone (`architecture.md`, dm-device kinds) rather than
falling through to the rule below. Only a name that decodes as neither —
no dnv prefix at all — reaches that rule. A host-facing nvmet subsystem
carries a user-chosen NQN and therefore carries no ids at all, so it is
attributed in two steps, the request first for the reason
`architecture.md`, Teardown by sweep, gives. First, a cntlr this process
holds whose `nqn_to_subsystem` still
names it settles it, under that cntlr's sp, with nothing read from
configfs: a cntlr's request is in memory (`putCntlr`) before its converge
builds anything, so no subsystem can exist whose claim is not already
visible here — the local-store save comes after the converge (CN20), so
the guarantee is about this process's state, not about the files. Second,
for a subsystem no held request claims, its namespaces, in the listing's
order: the first whose "device_path" parses as an ns-dev (`CnNsDevName`)
decides for the whole subsystem — one of ours makes it that sp's; one
that is not ours (another cn's, another cluster's) makes the subsystem
foreign and untouchable — and a subsystem with no such namespace at all
is unowned. Those only the node-level scope removes, together with the
orphan clone-metadata wrappers (CN2). Calling anything unowned assumes
at most one cn agent per kernel (`cmd/dnv-agent cn`); a co-hosted dn
agent is fine, because its NQNs are all dnv-format dn kinds, which never
reach that arm.

**P0, before every layer**: every unwanted ns-dev is parked, unless the
park fails (below). It has no plan — nothing wanted names it, and its td
may be leaving in the same pass — so the park is derived from the
device's own live table. Only one backing names a td: a raid0
(`CnRaid0Name`), whose ids are also its td's dm-error's (`CnErrorName`),
so there the target is
CN16's own. Anywhere else — a table this build never wrote, an ns-dev
over a dm-clone — the device is reloaded onto an error table of its own
size, which is the same thing one indirection shorter. A device already
parked is left alone but resumed if it is suspended. Both halves matter:
disabling the nvmet namespace above it in L1 closes its backing device,
and that does not complete on a suspended dm device; and the reload's
own flushing suspend is what completes the in-flight host IO instead of
replaying it at resume onto a stack that is about to go. A park that
fails does not stop the chain: L1 still runs on that pass, and an ns-dev
whose load failed is still suspended on its old table (the reload fails
closed, CN16) — the suspended device the first half is there to keep
from L1 (Known limits).

**Layers, strictly top-down.** Within one sp's chain:

L1. nvmet. Each unwanted namespace under a surviving subsystem attributed
to the sp (above) goes `AnaGrpIdInaccessible` and is then removed (a host
still holding a path is told to stop using it rather than losing it under
IO) — by this layer alone: the build phase's `ensureSubsystem` converges
the wanted namespaces and removes none, so a pass whose enumeration did
not answer removes none either; then the unwanted subsystems whole (port
link, namespace disable, namespace rmdir, allowed-hosts unlink, subsystem
rmdir). First because nvmet must release the dm devices below before
anything can remove them.

L2. ns-devs and transfer finals: what L1 just released.

L3. dm-clones, before their source connections: dm-clone flushes through
the source on removal (CN18).

L4. clone-metadata wrappers, under `cloneMetaMu` (CN18: the tables are
the registry, so a removal is a mutation of it).

L5. clone-source connections that nothing maps any more. The set is
evaluated here, not when the chain was built: a snapshot taken before L3
still shows the dm-clone this pass is removing mapping its own source,
and deciding early would never disconnect it. Their disconnects run off
the pass (CN10), so a pass that sets one going leaves it behind and, by
the stop rule below, the layers under L5 wait for the pass that finds it
gone.

L6. per-td raid0s and dm-errors.

L7. each thin volume removed and then, only if its pool is one the
desired state still wants, the pool-side delete of its id under CN14's
rule.

L8. thin pools, then the concats under them. An arming of CN14's
activation sweep ends with its pool's life, but the layer drops its
pools' armings together, and only once every pool removal of the layer
is verified; while any is unverified the layer keeps them all, and each
unverified pool is reported as a leftover. An unverified pool may have
survived, and the next converge that wants it again finds the device
present and arms nothing (`ensurePool` arms only a pool it creates), so
dropping the arming there would lose the thin-id sweep for good: every
id deleted meanwhile would keep its data blocks for the life of the
pool. Keeping an arming too long costs one idempotent sweep; dropping one
too early cannot be repaired.

L9. md arrays (`mdadm --stop` on the node sysfs named, CN12) and
`CnGrpName` linears.

L10. legs, in the one order that is deliberately not top-down: set the
legs' disconnects going (whole-NQN is fine here: every path of an
unwanted leg is going; off the pass, through the CN10 disconnect
registry), then remove the leg wrappers without waiting for them. Their
probers are cancelled already, by the converge's trim (below) or by CN7's
drop step, but a wedged prober still holds an open fd on the wrapper, so
its removal fails EBUSY until the disconnect has errored the queued IO
and the prober's fd has closed (CN11); setting the disconnects going
first only gives a quick one the chance to release such a wrapper within
the pass — otherwise it goes on a later pass. A wrapper is removed even
when its own connection is still there: they are two different objects,
and the connection is a leftover of this pass that a later pass's probe
finds gone.

No delete message is ever sent for a thin volume whose pool is itself
going (CN14: this is deactivation, the metadata on the legs is the next
CN's to find, and a created td's volumes are re-attached there without
any message), and nothing in any layer runs mdadm --detail (CN12).

**"Gone" is probed, never inferred from an exit status**
(`architecture.md`, Teardown by sweep): `dmsetup info` for a dm device,
"array_state" for an array, a configfs read for a namespace or subsystem,
the sysfs walk for a connection — where "gone" is the absence of any
controller, not of the subsystem directory, and a controller the
subsystem still lists after its device was deleted is no controller
(CN10): read as present, either would hold the descent below its layer
and set a disconnect going on every pass; a probe that itself did not
answer counts as not removed.

**The stop rule** is `architecture.md`, Teardown by sweep: every removal
of a layer is attempted, and the chain does not descend below a layer
that left anything behind; the layers underneath report their objects as
leftovers without being touched.

The same rule applies inside a layer that has an order of its own. L8
names the concats as leftovers without attempting them when any pool of
the layer would not go: they are present and unwanted whether or not a
pool still maps them, and a leftover nothing names is a leftover
nothing re-drives. L5 is the one layer whose set is computed when it is
reached rather than when the chain was built, so a stopped descent still
gets a truthful answer there too: a source still mapped by a clone that
would not go reads as in use and is not named, while the source of a
clone that did go is.

**Not on the node.** Two pieces of the cntlr's local state follow the
same rule and are swept at the end of the cntlr-level sweep of every
converge — one that an unanswered listing stopped included — ahead of the
build phase: the chunk files of every clone no longer in
`clone_list` (`dnagent.md` SH7 — a sweep of the store against the request, not a side
effect of removing the clone's wrapper, because a standby builds no
wrapper at all and keying the deletion off one would leave a deleted
clone's chunks on disk for ever), and the `ResInfo` histories, pruned to
the keys this plan's objects can use (`dnagent.md` SH14) so a later rebuild of the
same id reports a fresh epoch rather than the dead object's. A clone the
role or the level merely suppresses keeps its chunks (CN19, CN22). The
cntlr's in-memory index of its chunk files lets a clone's entry go only
once the `rm` of its files succeeded: one that failed or did not answer
keeps them indexed, and the converge that tried names each as a leftover
of the record kind (`LeftoverKindRecord`), as does the read-only verdict
(CN30) until a pass removes them. Dropping the entry first would leave
the files on disk with nothing short of a restart ever listing them
again, and no reply naming them.

The connect-retry registration and the leg probers are not swept — they
are goroutines, not objects on the node. A cntlr that is dropped loses
both in CN7's drop step; a cntlr at `SP_LEVEL_DISABLE` loses the retry in
CN19; and every converge trims the probers to the legs the plan still
probes (CN11) before its sweep, whether or not the sweep's chain runs or
reaches L10: the trim decides from the plan alone and removes nothing
from the node. Left to L10, it would wait on every layer above, and a
descent stopped at an array that would not stop, or a sweep stopped by
an unanswered listing, would leave a demoted primary's probers probing
through the standby's legs, and a departed leg's through its wrapper,
until some pass got that far. Coming before the sweep, it still precedes
L10's disconnect, which is what releases a prober wedged on a pathless
leg.

### `PushCloneBitmap`

CN22. Gate: this process must hold a request for the cntlr (CN23) whose
`clone_list` contains `clone_id`, and the chunk's two indexes must each
be in range —
`src_slice_idx` below that clone's `src_slice_cnt` and `bm_idx` below
`MaxCloneBmCnt` — bounded independently, in one `ReplyCodeUnknownObject`
naming both indexes and both limits. Neither bound may stand in for the
other: `src_slice_cnt` is the clone's own source geometry,
`MaxCloneBmCnt` the cap on the chunks of one slice's bitmap, and an
in-range `bm_idx` says nothing about `src_slice_idx` or the reverse.
Those three refusals are the whole gate: the request carries no
`revision` field and this handler compares none. A chunk is
position-addressed data at a `(clone_id, src_slice_idx, bm_idx)` whose
ids are never reused, and a push never advances the stored revision, so
a chunk the worker planned against a report the cntlr has since
replaced is still exactly the right bytes at exactly the right offset
— a revision gate here would only throw away work that is about to be
redone (`architecture.md`, Bitmap push protocol; `dnv-worker.md` BM3).
Then `dnagent.md` SH21 with the clone twists of that protocol: persist the chunk at
the `LocalCloneBmPath` of the cluster, cn, sp, clone and the pair —
overwriting when the payload differs, because clone chunks may grow in
place (`architecture.md`, [D8]); a byte-identical payload is already
persisted and already applied, and is skipped outright — then recompute
and apply. Clone chunks are self-positioned by the pair (`dnagent.md` SH22): the chunk
(s, b) holds the bytes of source slice s's bitmap from the offset b times
the chunk capacity `CloneBmChunkBytes` for its own length, at an offset
no other chunk's existence or length can move. They carry raid0
geometry, and `thinbm.go` folds the chunks present in place — never
through a reassembled per-slice bitmap, which a missing chunk would
shift every later chunk inside — into one device-order wire bitmap over
the dm-clone's regions: a region is skippable iff every source bit it
covers under the address mapping of `architecture.md`, raid0 bitmap math
(`src_slice_cnt`, `src_stripe_size`, `src_block_size`, the region size of
one block) reads as set, and a bit of a slice reads as set only when the
chunk holding it is present, the byte holding it is within that chunk's
length, and that bit is one. An absent chunk, the tail of a
present-but-short one (`architecture.md`, [D8]: `AppendCloneBitmap` may
still be growing it) and a slice holding no chunks at all therefore all
count as written — the safe direction, which can only cost an extra
copy. The folded bitmap feeds the shared `agent.SkipRanges` with no
region shift (clones have no meta region — that shift is the dn's) and
is `blkdiscard`ed onto `CnCloneFinalName`. If the dm-clone does not
currently exist (standby, not built yet, level-suppressed), the file
still counts as applied; chunks are re-applied whenever the dm-clone is
(re)created (CN18 step 4). A failed persist is logged and still acked
code zero, with neither the fold nor the `blkdiscard` attempted. For a
pair the node does not hold yet that heals itself: the chunk stays out
of the applied set, so it stays out of the clone's `BitmapInfo`
`chunk_id_list` (CN20) and the worker's diff (`dnv-worker.md` BM2) pushes it again. That
diff is the whole recovery, and it is eventual rather than prompt: no
push outcome arms anything on the worker (`dnv-worker.md` BM6 — a failed push is logged
and ends its plan, and there is no flag to raise), and `bm_info_list`
rides no `CheckCntlr` reply, so the re-push waits for whatever next
makes the worker issue a `SyncupCntlr` (`dnv-worker.md` RW4) — a revision change, a
round this cntlr rejects or answers with another revision, or a round
whose reply carries `ReplyCodeLeftover` (CN30). A failed persist of a
grown chunk (`architecture.md`, [D8], and Bitmap push protocol) is the
one case that does not heal that way: the pair is already in the
applied set with the shorter payload, so `chunk_id_list` keeps
advertising it, and the code-zero ack also refreshes the worker's memo (`dnv-worker.md` BM5)
— which keys on the clone id and the pair, so only that one chunk
is affected — and the node keeps the shorter version until
`AppendCloneBitmap` grows that chunk again, the same bounded,
correctness-neutral loss `architecture.md`, [D8], already accepts. Reply
`agent_reply` only.

### `GetCnInfo` / `GetCntlrInfo`

CN23. Read-only: probe fresh under the CN1 locks and reply `agent_reply`,
`revision`, the info. An unknown CN or cntlr pointer — one this process
holds no request for, as when none was ever accepted, or when the startup
reload could not load or skipped its file (CN2) and none has been
accepted since — ⇒ `ReplyCodeUnknownObject` with a zero `revision`. For a
known object the `agent_reply` is the read-only verdict of CN30. Never
creates, changes or removes an object of the node, though a cntlr's call
makes the kernel commit the metadata of each thin pool and dm-clone whose
status it reads (`dnagent.md` SH17).

### `CheckCn` / `CheckCntlr`

CN24. Instantiate the `dnagent.md` SH24 to SH26 loop with the probes of Probing and
error capture — `probe.go`; one round takes the CN1 locks of the
corresponding `Get*Info`, and its `agent_reply` is the same read-only
verdict (CN30) — which is what drives the worker's re-sync while a
leftover is still there, and, on `CheckCn`, while a piece of the base
state that a `SyncupCn` would build reads absent or an ANA group that it
would rewrite reads in the wrong state. Like every `dnagent.md` SH24 round it runs
under its request's `trace_id` (the stream's id when that is empty).

### `GetThinDeviceBm` / `GetLegBm`

CN25. Both serve the gateway's bitmap reads (`architecture.md`, Bitmap
reads) from a dm-thin metadata snapshot and report failure only through
the gRPC status (`architecture.md`, Common agent rules — they have no
`AgentReply`): an unknown cntlr, a non-primary role, a missing pool, or
a failing command all end the RPC with an Internal status naming the
step. Under the CN1 locks (node read plus object — the snapshot must not
race a converge): the `reserve_metadata_snap` message to the
`CnPoolFinalName` (`dmsetup message`), then `thin_dump --metadata-snap` on
the `CnPoolMetaName`'s device path with `-o` naming the output file (the
thin-provisioning-tools reader), then always the `release_metadata_snap`
message —
on the success path and on every error path, on a context the caller's
cancellation does not reach (still under the soft timeout), because a
leaked reservation blocks the next reserve and pins the pool's metadata
blocks; on the caller's own context, a caller that went away before the
release, such as one whose client gave up in the middle of the dump,
would take the release with it and leave the reservation held until some
later reserve on that pool met it. A reserve refused because a metadata
snapshot is already held — one an earlier reserve made and nothing
released, such as one whose reserve was killed after its message ran — is
retried once after a release. The XML goes to a file, never to stdout: the OS
command record logs a command's stdout in full (`log.md`, OS commands
and file IO), and a dump is one element per mapped run — large on a
fragmented slice, once per slice per read — while the record of reading
the file back carries a truncated excerpt. The file is named after the
`CnPoolMetaName` in the private directory `thinDumpDir`. Before the
reserve, every dump creates that directory with a mode that admits
nobody else and checks with `stat` that it is the agent's own user's,
carries that mode and is a directory — `stat` reads the entry itself, so
a symlink answers as one — and anything else fails the dump before the
pool is touched. The agent writes the dump as root, and `thin_dump`
opens its output with a plain create-and-truncate: under a fixed name in
a world-writable directory, another local user could create the file
first, own it through the dump and rewrite it before it is read back,
and a forged destination bitmap (CN18 step 4) discards regions of a
dm-clone that were never copied. Nobody else can create an entry in
such a directory, and only root can create, replace or rename an entry
of its parent, the system's runtime directory, which is root's. That
parent is the runtime directory and not a world-writable temp directory
because nothing repairs an entry that fails the check: `mkdir -p` leaves
an existing directory's owner and mode as they are. A local user who
created the name first in a world-writable directory would then fail
every dump on the node until an operator removed it — the bitmap reads,
the CN14 activation sweep, and CN18 step 4's recovery, which fails
closed. In the runtime directory only root can have made such an entry.
That directory is a tmpfs, so until its `rm` the file holds as much
memory as the document, which the command timeouts bound. The file is
read back under the soft timeout, and once `thin_dump` has run an
`rm -f` of it follows whatever the outcome, before the release and, like
it, on a context the caller's cancellation does not reach — a dump that
did not answer included, whether the soft timeout killed it or the
caller went away, since a killed `thin_dump` may already have written
it. Only an `rm` that fails, or an agent that dies between the dump and
the `rm`, leaves the file behind; the name is fixed, so the pool's next
dump overwrites it, and one whose pool is gone stays until a reboot
clears the runtime directory. The command timeouts of
`architecture.md`, Common validation, bound the dump; a pool whose
metadata outgrows what `thin_dump` emits inside `CmdSoftTimeout` fails
the RPC, and the caller falls back to a full copy — bitmaps are an
optimization, never a correctness input (`architecture.md`, Clones). The
activation sweep of CN14 shares this reservation machinery and its
`CmdSoftTimeout` bound — metadata too large to dump inside the bound
fails the sweep the same way it fails a bitmap read, and the sweep
retries on later converges.

CN26. `GetThinDeviceBm`: from the `slice_idx` pool's dump, the mapping
bitmap of the td's thin volume (its `dev_id` looked up in the stored
`td_list`) over the virtual blocks from `start_block` for `block_cnt`
blocks (a zero `block_cnt` ⇒ through the volume's per-slice size in
blocks); reply bit k is one iff the block is unmapped — thin metadata
answers mapped, which is written, and this boundary inverts exactly once
(`architecture.md`, raid0 bitmap math). Wire encoding: bits are
LSB-first within each byte — bit k is bit k mod 8 of byte k div 8 — the
reply is the bit count rounded up to whole bytes, and every trailing pad
bit is zero. `agent/bitmap.go` states that convention for the whole
agent (`dnagent.md` SH22); the reply itself is built by `agent/cnagent/thinbm.go`'s
own bit helpers, which is also where the inversion above happens.

CN27. `GetLegBm`: locate the leg's group and slice in the stored
`id_to_slice`. A meta-group leg replies all-zero (nothing skippable): the
pool metadata device's own utilization is not derivable from thin
mappings, and over-copying is always safe. A data-group leg: the group's
span within the `CnPoolDataName` starts at the summed `data_blocks` of
the preceding `data_grp_list` groups; leg data-region block k is
pool-data block span start plus k; bit k is one iff no thin device of
the pool maps that pool-data block (every member leg of the group mirrors
it, so the leg identity beyond its group never enters the math; a spare
leg, which is no member, is answered with its group's bitmap all the
same, which costs copies at most). Windowed
from `start_block` for `block_cnt` blocks over the group's
`data_blocks`, a zero `block_cnt` ⇒ to the end. Same wire encoding as
CN26 (LSB-first, zero-padded); the CN22 clone-chunk fold uses it too.
The span is summed over the effective `data_grp_list` (CN9), and a leg
whose group is not in that effective prefix fails the RPC with an
Internal status naming it — the provisioning leg's own group, and
equally a group after a deferred one, since deferral is a prefix cut
(CN9): neither is a concat target, so neither has a span. Answering
anyway would report one group's bitmap under another group's offsets and
return silently wrong data.

### Probing and error capture — `probe.go`

CN28. Probe map (`dnagent.md` SH17 conventions plus the cn probes fixed here:
`findmnt` for mounts, `stat` for plain files,
`losetup` for loops, `dmsetup ls` plus `dmsetup table` for
the clone-metadata arena and every other dm device, CN12's sysfs read of
the array's "md" directory for arrays — never mdadm --detail — and the
sysfs walk of CN10 — never nvme list-subsys — for every outbound nvme
connection). Every row reports against the effective desired state
(CN9): a resource the provisioning gate excluded is
`RES_STATUS_PROVISIONING` with the details "provisioning", never
`RES_STATUS_ERROR`, and it never feeds `err_epoch`. The rows, each with
its `res_name` and what its probe checks:

* `CnInfo.port_info`, named by the agent's port id as a decimal string
  (so on a node running several agents the rows differ): the configfs
  transport attributes match the transport flags; the three groups of
  `architecture.md`, [D4], are present with their fixed states.
* `CnInfo.tmpfs_info`, named by the `CnTmpfsPath`: `findmnt` shows a
  tmpfs mounted there.
* `CnInfo.tmp_file_info`, named by the `CnTmpFilePath`: the `stat` size is
  `CnCloneMetaAreaSize`.
* `CnInfo.loop_dev_info`, named by the `CnTmpFilePath`:
  `losetup` lists exactly one loop device for the file — this row covers
  the whole arena; `CnInfo` carries no clone-VG row (its field is
  reserved, `architecture.md`, [D14]) and per-clone metadata health lives
  in `clone_id_to_meta`.
* `ss_id_to_subsystem`, named by the subsystem NQN: configfs: present,
  "attr_allow_any_host" "0" (one found with it set reports
  `RES_STATUS_ERROR`), cntlid range, serial and model (trimmed,
  `dnagent.md` SH17), allowed hosts exactly as desired, and linked to the
  port (`probeExport`).
* `ns_id_to_namespace`, named by the NQN and the namespace index: the
  nvmet namespace is enabled, with the "device_path", "uuid" and "nguid"
  as desired (the identity compared through `agent.SameNsId` — configfs
  reads both back dash-separated and lower-cased whichever form was
  written, so a byte-wise compare fails a healthy namespace forever;
  `dnagent.md` SH17), and the "ana_grpid" as CN16 desires — inaccessible
  while the backing chain is provisioning-deferred (CN9), and the row
  itself is `RES_STATUS_PROVISIONING` then.
* `ns_id_to_dm_linear`, named by the `CnNsDevName`: `dmsetup table`
  matches the CN16 backing (flakey line included) — for a rule-5 ns-dev
  the one CN16 installs now, read the same way (`nsDevNow`): the td's
  `CnErrorName` while the dm-clone's status does not show hydration
  enabled, and `RES_STATUS_ERROR` with the read's error when that status
  read fails; an effectively suspended ns-dev is expected live on the
  td's `CnErrorName` and reports `RES_STATUS_OK` with the details
  "parked"; a dm-suspended ns-dev is `RES_STATUS_ERROR` "unexpectedly
  suspended" whatever the plan says; a provisioning-deferred one reports
  `RES_STATUS_PROVISIONING` over its permanent dm-error.
* `td_id_to_raid0` and `td_id_to_dm_error`, named by the `CnRaid0Name`
  and the `CnErrorName`: `dmsetup table`.
* `td_id_to_thin_info` with its `slice_id_to_dm_thin`, named by the
  `CnThinDevName`: `dmsetup table` (pool and `dev_id`).
* `slice_id_to_dm_pool`, named by the `CnPoolFinalName`:
  `dmsetup status`; the details are the raw status line — the worker
  parses data and metadata used and total out of it for the auto-grow of
  `architecture.md`, Automatic reactions. The serving pool stays
  `RES_STATUS_OK` with that raw line even while a deferred group waits to
  be grown in (CN13): `PROVISIONING` never marks the serving pool, because
  it would switch auto-grow off.
* `slice_id_to_meta` and `slice_id_to_data`, named by the `CnPoolMetaName`
  and the `CnPoolDataName`: the multi-target `dmsetup table` matches the
  group concat; the comparison is against the effective concat (the
  list's leading run of non-deferred groups, CN9, CN13), so a
  not-yet-grown concat is `OK`, not a mismatch. A deferred slice's rows
  are `RES_STATUS_PROVISIONING` — deferred meaning either of its two
  group lists is non-empty and has no effective group left (CN9), not
  that every group is deferred.
* `grp_id_to_md_raid`, named by the `CnMdDevName` node or the
  `CnGrpName`: `RedundMdRaid1`: the array holding the group's `leg_list`
  wrappers, read from sysfs — "array_state", and for a running array
  "degraded", "sync_action" and "sync_completed", plus each member's
  "state", "block/dev" and "block/dm/name" — never mdadm --detail, which
  opens a member and can block on a dead one for the length of the path's
  failfast (CN12). A running array (degraded included) ⇒ `OK` with the
  "array_state" as the details, then the word degraded while "degraded"
  is non-zero, then the word of a sync that is running (recovering,
  resyncing, checking, repairing, reshaping; none while "sync_completed"
  reads none) followed by its done and total sectors in parentheses. The
  words follow mdadm's State line, and repairing is ours; the state and
  the sectors are sysfs's. No answering array
  holds the group's legs (none matched, or the match stopped between the
  walk and its read) and no array of the walk is unanswered, or an
  "array_state" of clear ⇒ `RES_STATUS_MISSING`; an array that is not
  running (inactive, broken, …) ⇒ `RES_STATUS_ERROR` with the state as
  the details; a foreign member of the matched array, two answering
  arrays holding the group's legs, a "/sys/block" listing or a read of
  the matched array that did not answer, or no match — or a match that
  stopped since the walk — while another array of the walk did not
  answer (it may be the group's own; the details name it) ⇒
  `RES_STATUS_ERROR`. Beside a match, an array of the walk that did not
  answer is ignored and the row reads the match (CN12: a read of another
  sp's array that did not answer must not turn this row `ERROR`).
  `RedundNone`: `dmsetup table`. A deferred group (CN9) reports
  `RES_STATUS_PROVISIONING` and no mdadm command runs.
* `leg_id_to_leg`, named by the `CnLegName`: the wrapper table plus, on
  the primary, the CN11 prober outcome (`RES_STATUS_PENDING` until its
  prober's first completed round, CN11), or, on a standby, the transport
  per desired side, from sysfs,
  plus an "ana_state" of optimized or non-optimized on single-sided legs
  — two-sided legs liveness only (CN11). A provisioning leg (a non-empty
  `side_list`, every side unprovisioned, CN9) reports
  `RES_STATUS_PROVISIONING` and is neither connected, wrapped nor probed.
  A leg whose subsystem's sweep disconnect is still in flight (CN10's
  disconnect registry) reports `RES_STATUS_ERROR` with the converge's own
  details and is not probed. On a standby, a walk that did not answer
  (CN10) reads `RES_STATUS_ERROR` naming the read.
* `xfer_id_to_dm_linear`, `xfer_id_to_subsystem` and
  `xfer_id_to_namespace`, named by the `CnXferFinalName`, the `XferNqn`
  and the `XferNqn` with the origin namespace index: `dmsetup table` and
  configfs, per CN17; a deferred transfer's three rows are
  `RES_STATUS_PROVISIONING`.
* `clone_id_to_target`, named by the clone's `src_nqn`: the sysfs walk of
  CN10 shows a live controller per `src_tr_conf_list` entry (the
  subsystem whose "subsysnqn" is `src_nqn`, then each controller's
  "state") — not nvme list-subsys. While a sweep's disconnect of
  `src_nqn` is in flight (CN10's disconnect registry) it is
  `RES_STATUS_ERROR` with the converge's own details, whatever the walk
  shows; otherwise a walk that did not answer (CN10) makes it
  `RES_STATUS_ERROR` naming the read, never `MISSING`.
* `clone_id_to_dm_clone`, named by the `CnCloneFinalName`:
  `dmsetup status`; the details carry the raw status line
  (`architecture.md`, Live-state reporting — hydration progress;
  `DeleteClone`'s force check reads it). `RES_STATUS_ERROR` "metadata
  wrapper missing" when the arena could not supply the slot (CN18 step
  2).
* `clone_id_to_meta`, named by the `CnCloneMetaDmName`: `dmsetup table`
  of the clone-metadata wrapper: present, length = the CN18-computed unit count
  times `CnCloneMetaUnit`, in sectors, and the table's backing device equals the currently probed
  loop path; any mismatch (such as a tmpfs remounted under a live agent)
  ⇒ `RES_STATUS_ERROR`, whose repair path is the clone rebuild of CN18
  step 2.

CN29. Error capture (`architecture.md`, Common agent rules): a failed
command marks that resource `RES_STATUS_ERROR` with the command output
in its details and the converge pass continues with the remaining
resources. Probes create, change and remove nothing — the
metadata-snapshot reserve runs only inside `dumpThinMetadata` (the CN25
bitmap reads, the CN14 activation sweep and the destination-bitmap read of
CN18 step 4), never from `probe.go` — though a status read of a thin pool
or a dm-clone makes the kernel commit that target's metadata (`dnagent.md`
SH17). `RES_STATUS_PROVISIONING` is never produced by this path: it
is assigned by the CN9 gate, not by a failed command; and
`RES_STATUS_PENDING` is produced by the CN11 registry alone, never by a
failed command. The group row's probe opens no md member device: it is
CN12's sysfs read, so a dead member of an array that keeps another
in-sync member does not turn the row `ERROR` — it reads `OK`, with
degraded once md has failed the member (an IO to it that errors does,
and so does CN12's reconciliation failing it out as an extra; until then
the dead member does not count in "degraded"), and the fault is its leg
row's, from the CN11 prober's own IO. md does not fail the last in-sync
member of a mirror (dnv never sets md's fail-last-device option): an
error it charges to that member — a failed superblock or bitmap write,
which array writes bring, is one — marks the array broken instead. The
array then refuses every write for as long as it runs, its "array_state"
reads broken wherever it would read clean, and CN28 reports that as
`ERROR`.

Two kinds of thing travel in `agent_reply` rather than in the rows:
protocol failures (the CN8 gates, CN22's), and leftovers — the one
per-resource outcome that cannot ride in a row, carried as
`ReplyCodeLeftover` (`architecture.md`, Teardown by sweep, and Live-state
reporting; CN20, CN30). A piece of the CN's base state that a `CheckCn` round
or a `GetCnInfo` reads absent, and that a `SyncupCn` would build, travels
in both: its row reads `MISSING` (`ERROR` for the port), and the verdict
names it too (CN30), because the worker re-syncs on a reply's code and
never on its rows. So does an ANA group that the round reads in a state
other than its fixed one while the port's transport attributes match,
which a `SyncupCn` would rewrite: the port row reads `ERROR` with the
probe's words, and the verdict names it.

### The read-only verdict

CN30. `CheckCn`, `CheckCntlr`, `GetCnInfo` and `GetCntlrInfo` reply
`ReplyCodeLeftover` iff their scope's verdict is not clean. The verdict
is the CN21 sweep with the removals left out: the same enumeration, the
same attribution, the same comparison against the same wanted set, and
nothing removed or built (CN23). Details and log record
are CN20's.

The node-level verdict of a `CheckCn` round and of a `GetCnInfo` also
names what of the CN's base state the call's own probe read wrong in a
way a `SyncupCn` would cure (CN5). That is each piece it read absent: an
absent tmpfs; an absent arena file or loop device, while the same probe
read the tmpfs there or absent, since the converge creates them only on
a tmpfs it found or mounted; and a missing port directory or ANA group.
And it is an ANA group whose "ana_state" the probe read as other than
the group's fixed one on a port whose transport attributes all match:
nvmet takes an "ana_state" write whatever is linked to the port, and
`EnsurePort` rewrites a state that differs. Each is rendered the way
CN20 renders an enumeration that did not answer — "enumeration failed"
naming the piece as absent, the port's with the probe's own words after
it, and a group's state as differing from the wanted one — and the log
record lists it among its failures. Nothing else re-drives them — the
worker re-syncs on a reply's code, never on a row — and a check round
cannot build or rewrite them itself (CN23). A probe that did not answer
names nothing, and the next round asks again. Nor does anything else
that is there but not as wanted, which its row alone reports: a
filesystem of another type at the arena's path — and with it an absent
arena file or loop device, which the converge would not create on it —
an arena file of another size and more than one loop device, all of
which the converge reports and leaves as they are, so a re-sent
`SyncupCn` could not cure them; and a port transport attribute that
differs: nvmet refuses every transport-attribute write while a subsystem
is linked to the port (EACCES), so while one is every re-send would fail
on that attribute the same way, and `EnsurePort` stops at it, before the
groups. The probe, which reads the attributes first, does not read the
groups of such a port either, so a group state that differs beside it is
not named. The port is read with strict reads for this
(`agent.Nvmet.ProbePortState`), so a group or an attribute whose read did
not answer is an `ERROR` naming the read, not a missing group or a
mismatch. The `SyncupCn` reply does not carry any of them: its converge
has just tried to build the base state, and its rows say how that went.
So a node whose base state can never be built — a `mount` that is
refused every time, say — is re-sent its `SyncupCn` every round, and
each attempt is logged, as a disk node whose disk identity stays
unconfirmed is (`dnagent.md` DN16).

It is recomputed every round and stored nowhere. That is the whole retry
mechanism for a leftover and it is deliberately the only one: a leftover
that has since gone stops being reported by itself, one that is still
there keeps the code non-zero, and the worker's "re-sync while the code
is non-zero" rule (`dnv-worker.md` RW4) re-issues the `Syncup*` that
sweeps again. No agent-side loop exists for leftovers, because a second
retrier for something the worker already re-drives would be invisible to
the control plane. CN10's connect retry sweeps only as part of a converge
it re-runs for a reason of its own, and a sweep's `nvme disconnect`
completes off the pass (CN10's disconnect registry), where the next
probe finds it gone; one that failed is issued again by the next pass
that still finds the controller. A restart is covered by the same path:
whatever the startup reconcile could not remove surfaces on the first
`Check*` of the object that owns it (CN2). On the `Get*Info` side the
code is carried but read by nobody — the gateway's inspects pass those
replies through and by `gateway.md` AG3 do not apply the `AgentReply`
convention to them — so the operator-visible form of a leftover is the
worker's log and the `Syncup*` reply's details.

**Each scope answers for its own leftovers.** A cn's verdict is the
node-level one — the sps whose pointer has left its list, plus the
unowned objects, the arena's wrappers and the base state above that a
`SyncupCn` would build or rewrite — and a cntlr's is that cntlr's sp
alone, apart from the sp-less clone-source connections below. Neither
reports the other's sp chain, because each drives its own `Syncup*`:
reporting a cntlr's chain leftover on `CheckCn` would re-issue a
`SyncupCn` that builds no chain for that sp at all (CN21: an sp in the
pointer list gets none). Two kinds of object both verdicts can name. The
first is an orphan clone-metadata wrapper, and that overlap is correct rather
than a leak: the node-level scope owns the arena step whatever sp the
wrapper belongs to (CN2), while the owning cntlr's own chain meets the
same device at L4. A wrapper whose `dmsetup remove` will not go is
therefore re-driven by both `SyncupCn` and that cntlr's `SyncupCntlr` —
one round of work more than the minimum, and never a round less, which
is the safe direction for a verdict whose only job is to make somebody
try again. The second is an unowned clone-source connection: it belongs
to no sp at all, so both scopes enumerate it node-wide (CN21), the
cntlr-level L5 set being the same "no stored cntlr names it as a clone's
`src_nqn` and no live dm-clone table maps it" set the node-level pass
uses. The same NQN is therefore reported by `CheckCn` and by every
`CheckCntlr` of this node until it goes — the same one-round-extra,
never-a-round-less direction. A cntlr whose stored conf the gate refuses
has no verdict either — it converged nothing and enumerated nothing, and
naming objects this agent is not allowed to remove would be a leftover
report no `Syncup*` could ever clear.

## Known limits

* A member md has failed stays failed: `reconcileMembers` adds only a
  member the array does not hold, so a held faulty member is not
  re-added when its side returns — the array stays degraded until a
  spare switch or a re-assembly. Nor does the late-member retry (CN12)
  reach it: lateness is availability alone, and the held faulty member's
  leg is available again. A re-add would need a `--remove` before the
  `--add` of a held wanted member whose `MdMember.State` carries the
  faulty flag: mdadm opens an added device exclusively, and md keeps its
  claim on a faulty member until the member is removed, so an `--add`
  alone is refused.
* A killed `--remove` leaves a spare switch half done: a switch applied
  within seconds of the switched-out leg's side dying can have its
  `--remove` killed at the soft timeout while a superblock write is
  stuck on that member (CN12). The pass reports the group `ERROR` and
  does not add the promoted spare, and nothing re-drives it: the worker
  re-syncs on a revision or a reply code, never on a row, and a group
  error registers no CN10 background retry (the retry does finish the
  switch when the same pass registers it for something else, such as a
  failed leg connect or any `leg_list` member of the cntlr that is not
  available — the promoted spare or, after a whole DN died, another
  group's leg still on it, CN12). With no such trigger left — a lone
  switched-out leg, or the last switch off a dead DN — the array runs on
  its surviving member until the cntlr's next converge for some other
  reason.
* An unanswered array can leave a group unassembled: a group with no
  answering array while another array of the walk did not answer is
  neither created nor assembled that pass (CN12), and the error is a
  row, which nothing re-drives by itself; the Check verdict stays
  non-zero only while its own enumeration, `ListArrays`, still fails.
  Once the array answers, the group waits, its row `MISSING`, for the
  cntlr's next converge for some other reason — and the groups this can
  hit include those a new primary's first converge must assemble, whose
  slice layers cannot be built meanwhile. The late-member retry of CN12
  does not reliably cover it: it runs only while some `leg_list` member
  of the cntlr is not available, and the group's own legs, like every
  other leg, may all be available.
* A failed `--examine` read answers "no superblock": `Md.HasSuperblock`
  keeps a killed `mdadm --examine` apart from a member without a
  superblock, but not one that answered after its path's failfast
  expired — the read's IO error comes back as the fresh-leg answer
  (CN12). It needs that failfast to expire between the leg's
  availability read and the probe's answer in one pass, and it reaches
  case 1 only when no member of the group answers with a superblock;
  that `--create --assume-clean` lands over live data only if a path of
  that leg reconnects before it runs. Nothing tells the two answers
  apart.
* A park whose load fails can wedge the cntlr: a reload fails closed
  (CN16) and P0 only logs a park that fails (CN21), so an unwanted ns-dev
  that was still serving stays dm-suspended on its old table while L1
  runs over it. A namespace CN9's pre-step 1 has not moved — its loop is
  over the plan, so one dropped from `ns_list`, one under a subsystem
  that left the request, or any of an sp no longer in the
  `cntlr_pointer_list` — is still in the ANA group it had when the park's
  suspend lands, and if that is `AnaGrpIdOptimized`, host IO that arrives
  after the suspend queues in dm. L1's namespace disable (or the one
  inside a whole subsystem's removal) then waits in the kernel for that
  IO: a configfs write returns only when the kernel does (`osclient.md`,
  ReadFile / WriteFile / WriteFileDirect), and the pass holds its CN1
  locks — the cntlr's object lock, or the node write lock at node level
  and at startup (CN21) — so no later pass reaches the reload that would
  release it. A namespace no host IO reaches after the suspend is not
  exposed; only a reload or resume of the ns-dev from outside the agent
  ends it.
* A namespace dropped from a dnv-prefixed subsystem that decodes to
  nothing stays enabled: the sweep is the only remover of a namespace
  (CN21), and it never attributes a subsystem whose NQN carries the dnv
  prefix but decodes to nothing — neither to an sp nor as unowned
  (`architecture.md`, dm-device kinds) — so a namespace `DeleteNamespace` takes out of
  one stays enabled on every CN that had it, and that sp's chain stops
  at the ns-dev layer on every pass (`architecture.md`, Common
  validation).
* A member that stays unavailable costs a converge every
  `CnConnectRetryInterval`, under the lock every Check round needs: the
  late-member retry (CN10, CN12) runs the whole converge of the cntlr
  until every `leg_list` member is available. On members that are
  genuinely dead that lasts until the worker's leg repair
  (`dnv-worker.md` AR8) has switched the last of them out of `leg_list`,
  since the retry is cntlr-wide and the leg repair takes one step per
  pass of the SP (`dnv-worker.md` AR2): for a dead DN, the side-unhealthy threshold,
  then, for each of the SP's `leg_list` legs on that DN, a spare create
  (unless its group has a ready or pending spare already) and a switch,
  at least one pass each. It has no bound when the leg repair does not
  switch a late member: a `RedundNone` leg (the repair only logs for one,
  and the gateway's `CreateSpareLeg` refuses its group a spare), a leg
  with two sides (a migration in flight, which the repair leaves alone;
  it has no available path once the destination's DN is dead, or the
  source's before the destination's path is optimized,
  `architecture.md`, Migration), a full spare list, a spare of the group
  left unprovisioned on a DN that failed while it zeroed, an SP whose
  reactions are suppressed (`dnv-worker.md` AR3), no DN to place a spare on, or a pending
  spare the repair keeps waiting for (one the primary never reports). The
  retry then runs until the DN returns or an operator acts. Each attempt
  is a full converge of a built cntlr — probes, the sweep's enumerations,
  no creates — the profile the connect retry already has while a side's
  connect fails (a leg's controller is connected with no controller-loss
  timeout, `dnagent.md` SH20, so a side that dies while its controller survives is
  late rather than a failed connect; once the controller is gone — its
  reconnect refused with DNR, as when the side's port link is gone while
  the port still listens, or lost to a CN reboot — the next converge's
  `nvme connect` fails while the side stays down, and registers the same
  retry). The cost is not CPU alone: an attempt holds the cntlr's object
  lock for its whole converge, and every `CheckCntlr` round takes that
  lock too (CN1). The worker waits for a round's reply at most one
  `cntlr_interval` (`dnv-worker.md` RW8); a round that waits behind an
  attempt past that is a missed round, which stamps `Cntlr.err_epoch`
  (`dnv-worker.md` HL2), and two such rounds in a row, once that epoch has
  aged the primary-unhealthy threshold, let the failover reaction
  (`dnv-worker.md` AR5, AR10) fail over a primary whose only fault is a
  dead leg — and the new
  primary inherits the same late member, and with it the same retry. The
  connect retry holds that lock too, but only while a side's connect
  fails — and with the CN10 pass budget such an attempt also spends about
  `CnConnectPassBudget` more of it on in-pass retries and head waits,
  more when a retried connect itself runs long. How long an attempt
  holds the lock grows with the slice count: an attempt that outlasts
  `CnConnectRetryInterval` finds the loop's next tick already due and is
  followed by the next at once, so the lock is then almost always held,
  and a Check round waits out the rest of the attempt in progress before
  its own probe. The guard against this failover, not an optimization,
  would be a pre-check that runs the converge, and takes the object lock
  for it, only when the converge has something to act on — a late member
  available again, a provisioned side with no controller, CN18's clone
  triggers — read from the desired state and sysfs with nothing
  remembered. It is not done, so that the cn retry loop stays the `dnagent.md` DN13
  pattern (the comment on `connectRetryLoop` says why the two copies must
  not drift apart).
* A promotion that outruns the sides' flip serves IO errors until the
  retry builds the stack: CN16's ANA rule reads the plan — primary, not
  disabled, not effectively suspended, not deferred, below
  `SP_LEVEL_DISABLE` — and not the stack, so the promotion's own converge
  moves its namespaces to optimized even when its late members kept the
  stack from being built — a group with no available leg, a pool over a
  side that still exports dm-error — and the
  ns-dev reload onto the raid0 (CN16 rule 6) failed with the raid0
  missing: the ns-dev stays on the td's `CnErrorName`, the standby table
  of rule 2. A host's IO on that path fails with target-internal (DNR)
  errors, not path errors — the IO it queued while no path served
  included — until the first CN10 retry attempt after the sides' flips
  have reached this CN's sysfs builds the stack and reloads the ns-dev
  onto the raid0, within about one `CnConnectRetryInterval` of that
  (`architecture.md`, Failover). An ANA conjunct in the style of
  `architecture.md`, [D15] — optimized only once the ns-dev is on the
  backing it wants — is not done here.
* A clone build that keeps failing with its dm-clone up shows only in
  its Syncup rows: CN16 keeps a clone's td parked while its dm-clone
  does not show hydration enabled (rule 5). A failed CN18 step that
  stopped the build before step 5 enabled hydration registers the CN10
  retry (all but a failed allocation or replacement of the metadata
  wrapper, whose `clone_id_to_meta` row does not read `OK`), which
  re-runs the converge every `CnConnectRetryInterval` until the build
  finishes. What keeps failing may leave the dm-clone up with hydration
  off: among others a refused enable, a `dmsetup create` killed after
  its ioctl ran, or a stale dm-clone that will not go while a later step
  fails, such as the read of its destination bitmaps. A Check round in
  between that can read the node and finds the source's paths live then
  reads the parked ns-dev as the table CN16 wants and the clone's rows
  `OK` (the dm-clone row's raw status carries "no_hydration"), so the
  worker's health pass sees a clean primary while a host takes IO errors
  on the namespace, which stays optimized: the recovery's own IO-error
  window (CN18 step 4), drawn out for as long as the build keeps
  failing. These ways of failing do show in the Check rows: with the
  dm-clone gone between attempts (step 4's fail-closed removal, a refused
  create), `clone_id_to_dm_clone` reads `RES_STATUS_MISSING` and
  `ns_id_to_dm_linear` `RES_STATUS_ERROR`, because the status read of an
  absent dm-clone fails (`nsDevNow`); an arena listing or a status read
  that keeps not answering leaves its rows `RES_STATUS_ERROR`; and a
  source whose controllers are gone or not live reads
  `RES_STATUS_MISSING` or `RES_STATUS_ERROR` in `clone_id_to_target`.
  Nothing but the retry re-runs the build unless the cntlr is converged
  for another reason (a revision bump, a non-zero reply code, an agent
  restart).
* A controller delete whose target vanishes mid-delete waits out the
  kernel's admin timeout: when a disk node removes a side's export while
  this CN's `nvme disconnect` of the leg is mid-delete — the two ends of
  a leg removal, or of a pool drain, are separate RPCs to separate agents
  — the kernel cannot enter error recovery on the DELETING controller, so
  its shutdown command waits out the admin timeout, and the disconnect
  with it, in an uninterruptible write that no signal of `dnagent.md` SH15 ends. The
  sweep issues the disconnect off its locks (CN10's disconnect registry),
  so the stall holds neither the cntlr's object lock and its Check
  rounds, nor a `SyncupCn`'s node write lock, nor the worker's
  `SyncupCntlr` past its deadline; the connection reads as a leftover
  until the kernel lets go, and the child keeps one of the node's
  `DefaultOsClientLimit` `OsClient` slots for the whole wait
  (`osclient.md`, RunCommand). The pool drain keeps its cross-role race —
  it deletes an sp's cntlrs and then its slices and waits on no agent
  (`dnv-worker.md` SPD13) — so a `SyncupCn` that drops the cntlr from its
  pointer list sets the leg disconnects going in the background and is
  answered at once with `ReplyCodeLeftover` while the disk nodes take the
  exports away; a stalled disconnect then runs the admin timeout out, and
  one still in flight when the agent exits is not waited for
  (`dnagent.md` SH27), so only the leftover every later reply named
  records it. A drain that catches many deletes in the kernel at once
  holds at most `disconnectConcurrency` of those slots (CN10), and the
  rest of its disconnects wait for one: their connections stay leftovers,
  and a converge that wants one of them back waits as well, about an
  admin timeout for every `disconnectConcurrency` stuck deletes ahead of
  it. Two disconnects still run inline and can meet the same wait under
  their locks: the build's dead-path `nvme disconnect --device` of a
  migration's departed side (CN10) — the source after `FinishMigration`,
  the destination after `CancelMigration` — whose disk node tears that
  side's export down on the same change, and the dn sweep's disconnect of
  a migration-source connection (`dnagent.md` DN6), whose source disk
  node drops the migration export on the same change. One end of each
  pair is a disk node's `SyncupDn`, which the sides-first hold does not
  order (`dnv-worker.md` RW14). Neither is moved off its locks, and the
  second can cost a `SyncupSide` its worker deadline.
* A clone source judged unused can be adopted before its disconnect is
  registered: a cntlr-level L5 reads the stored requests and the live
  dm-clone tables, probes the source, and only then sets its disconnect
  going (CN10). Another cntlr of the same CN converges beside it under
  its own object lock. That cntlr's request can be stored after the
  read, and its CN18 step 1 can pass the registry check before the
  disconnect is registered; it then adopts the still-connected source and
  builds its dm-clone on it, and the disconnect deletes the source
  underneath. Reads of regions not yet hydrated then fail until a later
  converge of that cntlr connects the source afresh and reloads the
  table. The window runs from that read to the registration.
* A thin `delete` message that fails after its volume's removal is not
  re-driven: L7 sends the pool-side delete of a volume's id only once the
  volume is verified gone (CN21), a `dmsetup message` that fails is
  logged and nothing more, and no later pass can enumerate a pool id that
  has no dm device — the only id enumeration is CN14's activation sweep,
  which the pool device's creation alone arms — so the id's blocks stay
  allocated for the life of the pool device on this primary, until a
  rebuild of the pool runs the activation sweep.
* A base state that cannot be built is re-sent every round: a check
  round that reads absent a piece of the CN's base state that a
  `SyncupCn` would build, or an ANA group in a state other than its fixed
  one on a port whose transport attributes match, names it in its verdict
  (CN30), so while a `mount`, `truncate`, `losetup`, port `mkdir`
  or "ana_state" write keeps being refused, the worker re-sends the
  `SyncupCn` every round and each attempt is logged — the agent's
  leftover record of each round names what the round read, and each
  refused command or write is logged as it fails — as for a disk node
  whose disk identity stays unconfirmed. Until the arena is built CN18
  has no loop device to allocate clone metadata from, so no clone on the
  CN can be built. The verdict does not re-drive the rest of what is
  there but not as wanted (CN30): its row reads `ERROR`. The arena's — a
  filesystem of another type at its path, a file of another size or a
  second loop device — wait for an operator, since the converge leaves
  them as they are, and so does a port transport attribute that differs
  while a subsystem is linked to the port, since nvmet refuses every
  transport-attribute write then; one that differs while none is linked,
  which a re-sent `SyncupCn` would rewrite, is not re-driven either and
  waits for the CN's next `SyncupCn` for another reason or the agent's
  next start. Each re-sent `SyncupCn` holds the node write lock for its
  converge and its removing node-level sweep (CN1), so every cntlr's
  Check round and converge on the CN waits it out once a round.
