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
hierarchy, `ResInfo` tracking, the OS wrappers, the bitmap-chunk store,
the check-stream rules and the observations (SH1 to SH30) — and the
`cmd/dnv-agent` command
(CM1 to CM4) are `dnagent.md`'s and are not restated here. This document
leans on `dnagent.md` for them, on `log.md`, `osclient.md` and `grpc.md`
for the logging, OS-access and interceptor rules, on `schema.proto` for
the service and its messages, and on `architecture.md` for the
controller-node device stacks and the on-leg layout, the names, the common
agent rules, the side provisioning protocol, live-state reporting, the
bitmap push protocol, teardown by sweep, failover, migration, the
transfer-and-clone copy, the raid0 bitmap math, namespace suspend
semantics, the levels, the cntlid slots and the
decisions [D1] to [D4], [D6] to [D8] and [D11] to [D16].

## Scope and placement

Package `cnagent`, at `agent/cnagent/`, is the cn policy: the
`ControllerNodeAgent` service, which decides which leg connections, md
arrays, thin pools, dm and nvmet objects to build and when. It may import
`common`, `pb` and `agent` (`layout.md`, Dependency rules). Everything
mechanism-shaped that both roles use lives in `agent` (`dnagent.md`, Scope
and placement, states the split rule) and is reused verbatim. Tooling only
the cn role runs — mdadm, the thin-metadata reader — is not promoted into
`agent`: by the same split rule a wrapper with a single role is role code
and lives in `agent/cnagent/` (`md.go`, `thinbm.go`). There is no LVM
anywhere in dnv (`architecture.md`, [D14]), and the CN keeps no clone's
metadata: a clone keeps its dm-clone metadata on its clone meta group, on
DNs (CN18). Agents never talk to etcd
(`layout.md`, Dependency rules). The gRPC server carries the server
interceptors and the message limit of `grpc.md`, Wiring; the cn agent's
outbound connections are only `nvme connect`s, never gRPC.

## The cn's part of the shared packages

### Names and constants in `common`

The cn constants live in `common/constants.go` and the cn name helpers in
`common/name_fmt.go`. The parsers that invert the names, `ParseDmName` and
`ParseNqn`, are shared by both roles and are stated in `dnagent.md`,
Names and constants in `common`: strict, because a half-decoded name is one the
caller would remove. The constants this document relies on, whose values
and comments `common/constants.go` holds:

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
* `CnConnectPassBudget`, `CnConnectRetryPause`, `CnNsScanPause` and
  `CnNsHeadPause`, the connect step's one wait budget per converge pass
  (CN10, CN18), shared by every leg and clone source of the pass and drawn
  on by exactly three things: a failed connect's own elapsed time, the
  pause before each in-pass retry, and the steps of the wait for the
  namespace head after a connect this pass made, with a pause of its own
  while a picked head is not complete (CN10).
* `ResDetailsSpLevel`, the details of a `CntlrInfo` row the `sp_level`
  suppresses (CN19): `RES_STATUS_MISSING` that says the resource must not
  exist, where every other `MISSING` the cn agent reports says it does not
  exist yet or, on a clone's `clone_id_to_target`, that the clone's source
  has a fault (CN18). The cn agent writes it and the worker's settle reads it
  (`dnv-worker.md` HL2), so the two take it from one constant; a value
  that drifted on one side would keep every primary whose level suppresses
  a row the settle reads settling for as long as that level stands.

Two dm kinds in the cn namespace are the cn's own — `DmKindCnLeg`, the
leg wrapper; `DmKindCnGrp`, the `RedundNone` group device — with their
name helpers (methods on `NameFmt`), and `architecture.md`, dm-device
kinds, points here for them:

* `CnLegName`, the cn-local leg wrapper (`architecture.md`, [D1], and
  Primary cntlr): one dm-linear over the leg's single nvme multipath
  namespace device, sized from the desired state, never from probing the
  device (CN10). md and groups consume it as the member device instead of
  the namespace device, whose name is the kernel's and can change across
  reconnects: a dnv-named member is what a group finds its array by and
  matches its members against `leg_list` by (CN12), and what the sweep
  attributes an array by (CN21).
* `CnGrpName`, a `RedundNone` group device: a dm-linear over the single
  leg's data region, a slice's group's or a clone meta group's alike,
  since a group id is unique in its pool. `RedundMdRaid1` groups use the md
  names (`architecture.md`, md names) instead, a clone meta group their
  clone form (`CnCloneMdDevName`, `CnCloneMdArrayName`), and have no dm
  name.

[D1] of `architecture.md` leaves the leg wrapper agent-internal; fixing
its name here makes it observable (`dmsetup ls`, cleanup by prefix)
without making it part of any cross-component contract — nothing outside
the cn agent ever addresses these two devices.

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
and the rule — the cn names that carry no cn id, and why placement
does not reliably keep two cntlrs of one SP off one kernel — is
`architecture.md`, Controller node, common. CN21's attribution assumes
the rule as well: a host-facing subsystem that no cntlr this agent holds
claims and no namespace attributes is unowned and removed at node level,
and another cn agent's subsystem passes through that very shape while it
is being built.

CN-CM2. `runCn` has the shape of `runDn` (`dnagent.md` CM4), with two cn
differences. `--capacity` is optional, since zero is a value (CN3). And
`agent.Serve` gets no `dnagent.md` SH27 background waiter: the cn joins
no background task — its CN11 probers, its sweep's background
`nvme disconnect` (CN10), its connect retry (CN10, CN18) and its
background reads are cancelled at `rootCtx` and never joined, for the
reasons `dnagent.md` SH27 gives.

## The cn role — package `cnagent`

### Files

`server.go` holds the `CnAgentServer` type, the lock mapping and the RPC
entry points other than the check streams of `check.go` and the bitmap
reads of `bitmapread.go`; `plan.go` the per-cntlr derived names and sizes
and the ns-dev backing and ANA state machines of CN16; `reading.go` the
reading of CN28 that a cntlr's read takes, and where the helpers both
channels share take a dm device's facts and a device's number from;
`syncup_cn.go` and `syncup_cntlr.go` the two converges;
`leg.go` the CN10 side connections, the walk a cntlr's converge or read
takes and the lookups in it (the walk's code is the one both agents
share, `dnagent.md` SH20), and the wrappers;
`healthcheck.go` the CN11
probers and their `LegProbeIO`
(direct syscalls, never the `OsClient`); `md.go` the mdadm wrapper, the
sysfs reads of the arrays, the assembly cases, the member reconciliation
with a group's repair (CN12) and what the primary reports beside a
group's row (CN28); `pool.go` the
concats, thin pools and thin volumes;
`td.go` the
raid0, dm-error, ns-dev and flakey tables; `clone.go` CN18; `xfer.go` the
transfers; `push_clone_bm.go` the bitmap
push; `sweep.go` the CN21 sweep — the two scopes' enumeration,
attribution, wanted set and layered removal; `dmutil.go` the dm ensure
and probe helpers (`ensureDmSingle`, `ensureDmError`, `ensureDmLinear`,
`ensureDmMulti`, `probeDmTarget`, `probeDmArgs`, `probeDmConcat`,
`removeDm`, `removeExport`) shared by the converge files, `probe.go`,
`sweep.go` and the build phase of `syncup_cntlr.go` (`ensureDmClone`
stays in `clone.go`); `thinbm.go` the thin-metadata snapshot reader
behind `GetThinDeviceBm` and `GetLegBm` (CN25 to CN27) and the
clone-geometry fold of `architecture.md`, raid0 bitmap math;
`bitmapread.go` those two RPC entry points; `check.go` the check rounds;
`observation.go` what the check rounds and the info reads answer from,
the background reads and the converge marks, as the cn holds them
(`dnagent.md` SH28 to SH30); `probe.go` the probes. Package boundaries
are binding, file names are not.

### Server type and lock mapping

`CnAgentServer` embeds the generated unimplemented server and holds the
process's single `OsClient`, the `NameFmt`, the shared `Cmd`, `Store`,
`Dm`, `Nvmet` and `NvmeHost` wrappers, the cn-only `Md` wrapper, the
`LegProbeIO` of the CN11 probers, the `LockSet` (object key:
the `LocalCntlrPath` id tuple), the `--capacity` value and the port conf
(`PortConf`: the transport flags plus `--nvmet-port-id`). A leaf mutex
guards the in-memory mirrors of the local store — two maps: cn requests,
and cntlr states (last accepted request, `ResInfo` tracker, a per-clone
`CloneChunkSet` keyed by the `(src_slice_idx, bm_idx)` pair) — and what
mirrors nothing: in each cntlr's state the connect-retry and prober
registries; in each CN's and each
cntlr's state its observation slot, which holds its latest observation
and its running background read (`dnagent.md` SH28, SH29); the converge
marks (`dnagent.md` SH30); and the CN10 disconnect registry, all in
memory only, never persisted. Three more in-memory-only
fields of a cntlr's state sit outside that mutex, touched only under the
cntlr's object lock or the node write lock: the marks of CN14's
activation sweep (`pendingSweep`), CN14's torn marks (`tornTds`) and
`reqFromRpc`, which says whether the request this process holds arrived
by the revision-gated RPC (CN14). Each state's request is an atomic
pointer, stored and loaded without that mutex: other cntlrs' passes and the
node-level verdict read every cntlr's request holding none of its object
lock, and a request is never modified once stored. The tracker has its
own lock; the chunk sets are touched only under the cntlr's object lock,
or under the node write lock, which excludes it. A buffered channel of
`disconnectConcurrency` slots caps how many of the disconnect registry's
disconnects run at once. `rootCtx` anchors the CN10 and CN18 connect-retry
loops, the CN10 background disconnects, the CN11 probers and the
background reads, so shutdown cancels them and joins none (CN-CM2). No cn
lock is held across OS calls but the `LockSet`'s own.

CN1. Lock mapping (instantiates `dnagent.md` SH10 to SH13): `SyncupCn` and the startup
reconcile — node write; `SyncupCntlr`, `PushCloneBitmap`, one background
read of a cntlr (`dnagent.md` SH29), one background connect-retry
attempt, `GetThinDeviceBm` and `GetLegBm` — node
read plus that cntlr's object lock (key = the cluster, cn, sp and cntlr
ids); one background read of a CN — node read; `GetCnSize`, `GetCnInfo`,
`GetCntlrInfo` and one `CheckCn` or `CheckCntlr` round — no lock, since
the last four answer from the object's latest observation (`dnagent.md`
SH28). How a syncup cancels a read and how every other holder waits for
one is `dnagent.md` SH30. The CN11 leg probers take no
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
owns it, so nothing here may conclude the owner is gone. The reload
reads a store file an older build wrote as it reads any other, since an
upgrade keeps the store (`architecture.md`, Upgrade in lockstep). Loading
the chunks after the converges would lose none of them — a chunk in memory is
advertised as applied by the next reply's `chunk_id_list` (CN20), so the
worker's diff (`dnv-worker.md` BM2) would not push it again — but their `blkdiscard`s would
then wait for the next event that re-applies the whole set: a create of
that dm-clone, a converge reload of its table onto a changed length,
devno or region size (step 4 runs on both — the converge treats a reload
exactly as a create), a build that finds the dm-clone with hydration off
(step 4 runs then too), or another chunk's own push (CN22). Until then
the clone re-copies regions it never needed to.

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
while its pointer is in the list; but its request-derived set — the clone
sources some request names (CN21) — is read from the cntlrs in memory
alone, so a skipped cntlr's clone source that the sweep judges at all, a
transfer of this cluster (CN21), survives a sweep whose listings answered
only while a live dm-clone maps it: the source reads as in use.
Nor is anything hidden: with none of it in memory, the CN and
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
are never reused, so a dropped cntlr never comes back, an etcd restore
aside (`architecture.md`, etcd backup and restore). Every other
cntlr request: re-run the `SyncupCntlr` converge from the stored
request — which, per CN18, finishes by its steps 4 and 5 a clone whose
dm-clone is up with hydration off, as an agent killed between steps 3 and
5 leaves it, and creates the dm-clone again over its clone meta group's
metadata where it is gone; a dm-clone
whose table has drifted — a changed length, devno or region size — is
reloaded by step 3's probe-first converge (`dnagent.md` SH16) with step
4's locally held source chunks re-applied. A reboot clears the dm state,
the md arrays and the connections, but not the clone meta group's legs,
which are sides on disk nodes: the converge connects the legs, assembles
the group (CN12) and creates the dm-clone over the metadata it holds, so
the copy goes on from where it was. A plain agent restart preserves the
whole stack, and the converge is a no-op re-apply for a clone whose build
had enabled hydration. The pass order is therefore: load, then converge
every CN's base state, then drop the orphan cntlrs, then the node-level
sweep per CN, then converge every remaining cntlr.

All under the node write lock, with the `dnagent.md` SH2 trace id. Background retries
(CN10, CN18) and probers (CN11) mint a fresh trace id per attempt.
Each CN's converge and node-level sweep, and each cntlr's converge, end
in the object's observation (`dnagent.md` SH28): its rows, with the code
of its own sweep. Whatever this pass could not remove is remembered only
as the code of the object's observation, which the next read or converge
replaces, and no later pass reads it (CN30): the object's `Check*` rounds
answer from that observation until then, and each later read names
afresh what is still there.

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
node, common, probe-first (`dnagent.md` SH16), building `CnInfo` as it
goes. The base state is the agent's one nvmet port: `EnsurePort`
(`dnagent.md` SH19: the agent's `--nvmet-port-id` port, default
`NvmetPortId`, from the transport flags plus the three fixed ANA groups),
whose outcome is `port_info`. CN host-facing namespaces only ever use the
optimized and the inaccessible group — the non-optimized group exists on
every port (`architecture.md`, [D4]) but no cn code path assigns it. The
CN keeps no clone's metadata: a clone's dm-clone keeps it on the clone's
clone meta group, on DNs (CN18; `architecture.md`, [D14]).

CN6. QoS is accepted, persisted and not applied: the agent persists
`qos_ratio` with the request (`dnagent.md` SH5 does that for free),
programs nothing and reports no QoS row in `CnInfo` (`architecture.md`,
Controller node, common: QoS is not enforced).

CN7. Persist first, then drop, then sweep. The request is written to
`LocalCnPath` before the converge, as `dnagent.md` SH5 states for every
request and for SH5's reason: the node-level sweep below is what removes
the resources of a cntlr whose pointer has just left the list, and it can
block for a whole failfast window on a dead leg.

Then the CN5 base-state converge. Then the `cntlr_pointer_list` diff
against the loaded cntlr states (`architecture.md`, Common agent rules:
full sync) — never against the files: a cntlr whose file CN2 skipped is
not in memory and keeps its file (CN2). A pointer in the request without
local state needs nothing
yet — resources come with its first `SyncupCntlr`; the persisted request
is what makes the pointer known. A local cntlr whose pointer left the
list is forgotten: its cntlr file and chunk files are deleted, its
connect retry, its leg probers and its running read are cancelled, its memory entry and object lock are dropped
(`dnagent.md` SH7) — and nothing of its is removed from the node by this
step. Then the node-level sweep (CN21) finds its resources by name
and removes them. What the sweep could not finish is remembered only as
the code of the CN's observation, which the next read or converge
replaces, and no later pass reads it (CN30); the reply's code is that of
the sweep's own verdict. The base state itself is never swept — like the DN port, it
outlives every cntlr and only lab cleanup removes it. Reply
`agent_reply`, `revision`, `cn_info`, and the `obs_stamp` and
`save_time` of `dnagent.md` SH28: the CN's observation, stored before the
reply, holds that code and that `cn_info`.

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
"invalid stored conf", names the ids and the field. A request the three
gates admit is persisted at once, before it becomes the cntlr's desired
state and before its converge (`dnagent.md` SH5).

Placement is load-bearing: the gate sits before the local-store persist
and before the request becomes this cntlr's desired state, so it skips the
persist, the desired-state promotion and the whole converge — whose sweep
phase alone rewrites ANA states, reloads ns-dev linears and removes dm
devices; the skipped persist is what keeps a refused request from being
replayed by the next startup reconcile. The same check is repeated in the
converge itself for the entrances that do not come through this RPC — the
CN2 startup reconcile,
which converges from a file that may have been persisted with zeros, and
the CN10 and CN18 background connect retry, which re-enters with the
request it already holds.
There it returns an empty `CntlrInfo`,
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
* a group whose `leg_list` contains a provisioning leg is
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
* a clone whose clone meta group is deferred is deferred, and with it,
  below `SP_LEVEL_NO_CLONE`, the ns-devs and namespaces of its
  destination td (CN16, CN18), each reporting `PROVISIONING`: the
  dm-clone has no metadata device yet, and rule 5 would otherwise park
  those namespaces and register the CN10 retry for a wait that ends with
  the sides' provisioned flip, a new revision. At `SP_LEVEL_NO_CLONE` and
  above no dm-clone is wanted, so the level wins and CN16 rule 4 or an
  earlier one backs them (CN19). A transfer of such a namespace maps the
  td's raid0, not the dm-clone, and is not deferred with it (CN17).
* an unprovisioned new leg of a repair defers only itself — a new leg is
  a candidate of the assembly only once it is available (CN12), so the
  group's own assembly is unaffected — and `leg_id_to_leg` reports it
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
  fail (a level change, a finished migration or repair, a failover) are
  exactly those. The layer order, the verification rule and
  the stop rule are CN21's; the pre-steps below are this pass's.
* **Build phase, bottom-up** — legs (CN10) → groups (CN12) → per-slice
  pools (CN13) → thin volumes (CN14) → raid0 and error (CN15) → clones
  (CN18) → transfers (CN17) → ns-devs plus host-facing nvmet (CN16) → ANA
  rewrites to optimized last, each namespace and each transfer only in a
  pass that leaves its device serving, or on the error device the desired
  state selects (CN16, CN17).
  This is the new primary's part of `architecture.md`, Failover.

**The wanted set** is the object-level wanted set of `architecture.md`,
Teardown by sweep — what the build phase would ensure for this plan, with
every object a provisioning deferral merely postpones left in it (a
deferred group's array, a provisioning leg's wrapper). It is read off the
plan's own level and
role flags, and each row is pinned against the ensure that creates it
rather than against any prose: the leg wrapper `CnLegName` and its
side-export connection under `wantLeg`; the md array (or the `CnGrpName`
linear of a `RedundNone` group) under `wantGrp` — a slice's group's and a
clone meta group's alike (CN12), the latter for as long as its clone is in
`clone_list`; the pool concats and thin
pool (`CnPoolMetaName`, `CnPoolDataName`, `CnPoolFinalName`), the
`CnThinDevName` thin volumes and the `CnRaid0Name` raid0 under `wantPool`;
the per-td `CnErrorName`, the `CnNsDevName` ns-dev with its nvmet
namespace, the host-facing subsystems, and the `CnXferFinalName` transfer
with its subsystem and namespace under `wantAny`; the `CnCloneFinalName`
dm-clone and the clone-source connection under `wantClone`. The raid0
sits with
the pool, not with `wantAny`, because at `SP_LEVEL_NO_THINPOOL` there is
no thin volume left to stripe over (CN19) — a standby keeps only what
that list gives it, which is what makes a primary-to-standby flip nothing
but a smaller wanted set.

The legs are every leg the plan builds: each group's `leg_list` and the
new legs of its repair, at every stage of the repair (CN10, CN12). An md
array is wanted only while every member is a wrapper of the legs of one
group the plan wants, so an array that holds an aborted repair's new leg
stays while the request still carries the repair, and the member
reconciliation takes that leg out (CN12). A retired
leg is no leg of any cntlr's request (`architecture.md`, Repair), so the
finish that retires a leg makes its wrapper and its connection unwanted
(CN21's L10), and an array that still held it would be unwanted and
stopped (CN21's L9). That is why the bad leg leaves md before its repair
can start (CN12).

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
   something no plan ever described. An ns-dev whose rules want a serving
   device (CN16) — one still mapping a dm-clone the plan dropped, say —
   has its namespace moved to inaccessible before the reload, and the
   build moves it back in the pass that puts the ns-dev on that device,
   or that finds it still serving there after a park whose load failed
   (CN18's hold);
   an ns-dev of rules 0 to 4 needs no move, since pre-step 1 has moved
   every namespace the plan wants inaccessible and rules 3 and 4 keep
   theirs optimized on purpose (CN16). The reload's own flushing suspend is
   what completes the in-flight host IO — the old primary's next step of
   `architecture.md`, Failover — and without it the removal below fails
   EBUSY behind a live ns-dev. The park creates its error device when it
   is missing, so it does not run at `SP_LEVEL_DISABLE`: the wanted set
   holds no dm-error there, and one created after the sweep's enumeration
   would be in no chain — the pass would remove the ns-dev parked on it,
   reply clean, and leave an unwanted device for the next read to find.
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
The CN's whole part in a migration is a leg's two-sided `side_list`
(CN10) and the path rows it reports for such a leg (CN28): the cntlr
request names no migration.

CN10. Legs (`leg.go`; every leg of every group of every slice in
`id_to_slice` and of the clone meta group of every clone in `clone_list`
(`architecture.md`, Terminology and object model), the new legs of the
group's repair included, at every stage of the repair (CN12), both roles;
a clone record without a group adds none). Per side
in the leg's `side_list`: `nvme connect` to the `SideToCnNqn` of the cluster,
sp, leg and this cn at the side's `nvme_tr_conf`, with the host NQN
`CnHostNqn` (`dnagent.md` SH20 flags). A side with `provisioned` false is skipped: the
dn exports nothing for it yet (`dnagent.md` DN9, DN10), so a connect
could only fail and arm the retry registry. Provisioned sides of the same
leg connect normally, which is what keeps a two-side migrating leg
serving while its destination side zeroes, and what connects every cntlr
to the destination once it is provisioned, before the migration's
cutover (`architecture.md`, Migration). A leg all of whose sides are
unprovisioned is provisioning (CN9): no connection, no `CnLegName`
wrapper, no prober, and `leg_id_to_leg` reports `RES_STATUS_PROVISIONING`
(CN28). All sides of a leg share that one NQN, so the kernel merges them
into one multipath namespace (`architecture.md`, [D1]); the agent finds
its head device without udev by the **sysfs walk**, the listing of
"/sys/class/nvme-subsystem" and each entry's "subsysnqn", and the leg's
**lookup** in it: the entry whose "subsysnqn" matches the leg NQN, then
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
a whole-device dm-linear over the leg's meta and data blocks that names
the head by the number in the head's "/sys/block/<name>/dev" (CN28) —
sizes come from the desired state, never from probing the device.

**Lookups share a walk.** A cntlr's read takes one walk, in its reading
(CN28), and makes all its lookups in it. A
converge takes one right before its sweep, and its lookups share it,
except that it walks again after a connect step that ran a connect
(below), and once more when a clone source's lookup reads no subsystem,
before it connects the source (CN18, step 1); the sweep's check that a
removed connection is gone walks once per connection (Known limits). The
list of connections of a converge's sweep comes from the walk the
converge takes before it, and that of the verdict of a cntlr's read from
the reading's (CN21, CN30). Reason: a walk lists
the class directory and reads every entry's "subsysnqn", so a walk for
each lookup would cost a cntlr with many legs reads that grow with the
square of its leg count. A lookup reads the entry's "subsysnqn" again
just before it reads the entry's directory and controllers. An entry gone
since the walk, or one whose "subsysnqn" names another NQN at that read,
reads as no subsystem, as an absent "subsysnqn" does (below), and so even
beside another entry whose read failed: the walk found this NQN in this
entry, and the host keeps one subsystem per NQN (`architecture.md`,
[D1]). Reason: the walk
can be as old as the pass, and meanwhile the kernel can drop an entry and
give its number to a subsystem that another cntlr, or a dn agent on the
same machine, connects; without the check a converge could build a leg's
wrapper over another pool's namespace and write the group's data there.
The check costs one read per lookup. A connect step that ran a connect,
whether or not the connect worked, makes the converge walk again before
its next lookup, since the subsystem the connect made may hold the number
of an entry freed since the walk.

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
connect made in this pass, the subsystem is looked up again until its
multipath head is complete, as long as the budget covers the pause before
the next lookup: `nvme connect` returns once the controller is live and
only queues the namespace scan that adds the head, and the kernel adds the
head in steps — its directory, its "nsid", its "/sys/block" entry, then
what lets it be opened by its number, and its **ng device** last. The
wait counts the head complete only when a lookup picks the head and the
head's ng device is listed beside it. Reason: a head picked before that
can still lack the "/sys/block" entry its number is read from (CN28), or
not yet open by that number when the wrapper's table names it, and the
leg would fail for the pass on a head a few steps from done; the ng device
is the last step, and the lookup reads it from the listing it already
makes (`dnagent.md` SH20). The pause is `CnNsScanPause` while the lookup
picks no head, none listed or none whose "nsid" reads the one asked for,
and the far shorter `CnNsHeadPause` while it picks one whose ng device is
not listed yet. Reason: the steps left once the head is picked end well
inside one scan pause, and a pass that connects many legs needs the rest
of its budget for the heads not listed yet. The first of those lookups
walks again (above); each later one reads only the entry found, with the
check above, but walks again after a lookup that found no entry or whose
entry failed the check. Only this wait judges the ng device: a subsystem
that was already connected is judged on its one read, whether it has no
head — a leg whose only path is ANA inaccessible never gets one — or a
head whose ng device is not listed. Reason: a head that never gets an ng device, as
on a kernel that makes none, then costs at most what is left of the budget
of the pass that connected it, and is used as it is, never failed for the
missing sign. A pause starts only while it fits in what is left, and a
failed connect is charged after it has run, so a retried connect that runs
long can overdraw the budget; nothing bounds the first connect of a side.
Once the budget is spent the pass behaves exactly as it does without one:
every connect it makes is made once, the first that fails still ends its
leg's converge (the leg's other unconnected sides wait for a later pass), a
leg that did not connect, or whose head did not appear, fails with the
same error, and a head that is picked but not complete is used as it is:
its number is read and the wrapper is created, and a failure of either
fails the leg with its own error. A head wait that the budget stops writes
one record naming the subsystem, the head it picked, if any, and the
pauses it took of each kind. Reason: a leg row that reads that the head's
block device is not there can then be the budget's kept case, and the
record in the same pass tells it from a wait that ended too early. The
waiting is part of a converge, which holds the cntlr's object lock and
keeps the cntlr's reads off while it runs (CN1; `dnagent.md` SH30), so
the cntlr's observation ages for as long as the pass waits; that is why
the budget is one per pass and not one per leg.

A connect failure marks that leg `RES_STATUS_ERROR` and registers the
cntlr in a background retry registry that re-runs the converge every
`CnConnectRetryInterval` under the CN1 locks until a pass registers it no
more, or teardown (the `dnagent.md` DN13 pattern); the RPC itself retries a connect,
or waits for its head, only as far as the pass's budget allows. These
register it: a leg that failed to converge — its connect, its
multipath namespace or its wrapper (above), an unknown controller or a
disconnect of its subsystem still in flight (below) — a clone source whose
connection failed, or whose disconnect is still in flight (below), a
clone failure CN18 lists, such as a clone meta group that is not ready
while no dm-clone of the clone serves,
a `leg_list` member of
a group that is not available, but for an open repair's bad leg while the
group's array runs (CN12: a
promotion whose first converge reads its legs before the sides' ANA flips
have reached this CN's sysfs would otherwise leave its groups unbuilt, and
the stack above them, until
the next revision bump, because the worker does not re-run that
converge), a namespace or a transfer whose ANA group the
build fails to move (CN16, CN17), and a namespace or a transfer whose
desired group is optimized and whose device the build leaves off what the
desired state wants (CN16, CN17), whatever the cause — a stack not built
yet, a clone build not finished, a dm-clone that does not serve, CN18's
hold, a failed step — and for as long as it lasts (Known limits). Every
attempt mints its own trace id
(CN2) and is a whole converge, which decides afresh whether any of them
still holds; the first converge that finds none stops the retry.

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
connected again. The walk and the lookup are read the same way. A
listing of "/sys/class/nvme-subsystem" that did not answer, or a
walk's "subsysnqn" read that failed — any error but ENOENT — on an entry
while no other entry names the leg's NQN, leaves the pass not knowing
whether the leg is connected, and so does the lookup's own "subsysnqn"
read failing that way; a listing of the matching entry's own directory
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
Inline, that would hold the converge, and the cntlr's object lock with
it, for the whole timeout: no read of the cntlr would run meanwhile
(CN1), so its observation would age that long and could become too old
(`dnv-worker.md` HL7), and the worker's `SyncupCntlr` would run into its
own deadline. So the pass probes the
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
reads are refused a slot at the soft timeout and report `ERROR` rows on
healthy objects. The registry is a server-wide set of subsystem
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
the converge's own details, and reads that clone's source as not ready,
as the converge does (CN18) — so the two channels neither flip those
rows, and their epochs, against each other nor disagree on how far the
clone got. Whether a connection is
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
through it can never succeed. Per connected leg, a clone meta group's
included (CN10), the agent keeps one prober goroutine (no locks, CN1):
every `CnLegProbeInterval` it writes the health block — the last
`LegHealthBlockSize` bytes of the meta region, through the leg wrapper,
with the payload `LegHealthMagic`, the `cn_id` and a nanosecond
timestamp, via `LegProbeIO`'s write half — and
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
`PENDING` rather than `OK` there, because an `OK` clears `Leg.err_epoch`:
a dead leg's would be cleared at every promotion, whose probers start
over, a repair's new leg's among them, whose health epoch the worker's leg
repair reads (`dnv-worker.md` HL2, AR8). For a registered prober the
window is one `CnLegProbeInterval` tick plus the probe itself, and a fresh
wrapper, a promotion and an agent restart each open it. A leg whose
wrapper exists but whose converge fails before its prober registers — on a
promoted or restarted primary whose connect is refused, say — reads
`PENDING` in every read until a converge registers one, while each failing
converge marks the leg `RES_STATUS_ERROR` (CN10). A standby
reports the transport probe instead: the sysfs walk (CN10) shows a live
controller per desired (provisioned) side and, for a leg with exactly one
desired side, an "ana_state" of optimized or
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
`architecture.md`, Standby cntlr). The groups are those of every slice and
the clone meta group of every clone in `clone_list` (`architecture.md`,
Terminology and object model). Every rule here holds for a clone meta
group as for a slice's group, its md names aside: its array takes their
clone form (`CnCloneMdDevName`, `CnCloneMdArrayName`; `architecture.md`, md
names), since a slice's md names carry the group's place in its slice.
Reason: the group has legs, an array and a repair as a slice's group has, and
one set of rules for both is one set to get right. A clone meta group is
in no concat (CN13): its device is the metadata device of its clone's
dm-clone (CN18).

* `RedundNone`: the group device is `CnGrpName`, a dm-linear over the
  single leg wrapper at the data region's offset and of the data region's
  length, created or reloaded only while that leg is available (below).
* `RedundMdRaid1`: an md-raid1 array under `CnMdDevName` (`--name` is
  `CnMdArrayName`, an internal bitmap with its chunk from
  `bitmap_chunk_block_cnt` and the block size, the data offset from
  `meta_blocks` and the block size, every member `--failfast`,
  `--homehost any`), whose members are the leg wrappers of `leg_list`,
  and those of an open repair's new legs once the repair's add has run
  (A group's repair, below). Every member carries md's failfast flag, from
  its create or its add, so that an IO error on a member fails that member
  at once, with no retry. Reason: a
  disk node that stays connected but never completes IO then costs the
  array that member instead of hanging the whole array; the fast IO fail
  timeout every connection is made with (`dnagent.md` SH20) covers only a
  path whose controller is lost. The flag also lets a short fault fail a
  member, such as an IO that reaches a migration's source in the moment
  after its export turns inaccessible (`architecture.md`, Known limits),
  and the member reconciliation (below) puts such a member back, unless
  the in-sync guard (below) holds over the array. Assembly
  is the step of `architecture.md`, Failover, that makes every group
  available; its cases are these, decided by probing each available
  member for an md superblock (`mdadm --examine`), and each available new
  leg of an open repair beside them (A group's repair, below):
  1. No member has one, nor any such new leg ⇒ `mdadm --create` with
     `--run --assume-clean` over the `leg_list` members alone —
     but only when every `leg_list` member is available and was probed:
     with any member missing, the group is simply unavailable this pass
     ("only k of n legs available and none carries a superblock"),
     because creating over a subset would mint a fresh array while an
     absent leg may carry the real one. Nor while the `CnMdDevName` node
     already resolves to a device (`Md.NameInUse`, an `lsblk` of the node,
     which reads no member; one that did not answer is an error that
     refuses the create this pass, like a killed `--examine` below): the
     array is found by its members (below), so one that runs under the
     group's name holding none of the wrappers the group keeps (its
     `leg_list` wrappers and its open repair's new legs) reads as absent,
     and with legs that carry no superblock it reaches case 1, where a
     create would put a second array under the name mdadm gives the
     group's array ("an array runs under … holding none of the group's
     legs"). The guard keeps that second array from being
     created; the pool's concat finds the group's array by its legs as
     well (below), never by that name. The guard errs only towards
     refusing, and a false refusal
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
     A clone meta group's legs are zeroed as a data group's are, and the
     same holds for the dm-clone over its array (CN18): it finds the
     zeroed first data block, where its superblock goes, formats its
     metadata there, and reads only metadata blocks it has written
     through the array.
  2. Some have one ⇒ `mdadm --assemble` with those, the new legs after
     the `leg_list` members; then the sysfs read of the array (below) and
     `--add --failfast` any available `leg_list` member the array left
     out, other than an open repair's bad leg, and a repair's new leg
     under its add's conditions (A group's repair, below). Freshly
     provisioned additions and stale-metadata re-adds both land here;
     with a single available member mdadm itself decides whether a
     degraded start is safe, and a refusal leaves the group
     `RES_STATUS_ERROR`.

  A leg is **available** iff its CN10 converge succeeded this pass — its
  provisioned sides connected, every controller's "address" answered, its
  multipath namespace found and its wrapper built — and that namespace
  has a path that is both live and optimized, probed from sysfs (CN10's
  walk). A `leg_list` member that is not available this pass — whether
  its group was left unbuilt ("no available leg", "only k of n legs
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
  groups (CN19); a deferred group (below) never counts, and a held member
  md has failed on a leg that is available is
  not late: while the array runs, the member reconciliation (below) puts
  it back once md has moved it out of its slot and its leg can take it,
  unless the in-sync guard (below) holds over the array. Nor is a
  repair's new leg late, nor an open repair's bad leg while the group's
  array runs (A group's repair, below): the read's verdict names a new
  leg once its add can run (CN30), and no converge adds the bad leg back
  to a running array while its repair is open, so a retry for it would
  bring converges that leave the array as it is. While the array does not
  run, the bad leg is late as any other member is: the assembly takes it
  once it is available (case 2, above), and mdadm can refuse to start the
  array without it. An assembly can still need a new leg, and then waits
  for a converge that comes for another reason (Known limits). A
  `RedundNone` group's leg counts too, and its dm-linear (above) is
  created or reloaded only over an available leg:
  the layers over it do IO through it, and a side that has not flipped to
  this CN yet exports dm-error to it (`architecture.md`, Disk node), so a
  pool, a raid0 and an ns-dev built over a late leg would serve that
  leg's blocks as IO errors, and a promotion would move their namespaces
  to optimized (CN16). A late leg under a group device that is missing,
  or that maps another table, fails the group ("no available leg") and
  touches nothing. A missing device fails the slice's concat that names
  it (CN13). A pool that is not built yet, such as the pools of a
  promotion's first converge, waits for it, and every layer above the
  pool waits too, their namespaces inaccessible (CN16). A pool that
  serves when a grow adds the group keeps serving at its old size, and so
  do the raid0s and ns-devs over it, their namespaces optimized, while
  that concat's row and the pool's row read `ERROR`, until the retry
  builds the group and grows the concat and the pool. A group device
  that is there with the table the group wants is kept whatever the
  leg's availability, with no teardown and no reload: a primary that
  serves keeps serving through a short loss of its leg's path. No log
  record of its own: the rows already report it — a group's error, or
  degraded once a probe reads an md array, and the member's own leg row.
  The retry runs a
  converge every `CnConnectRetryInterval` for as long as a member stays
  unavailable, each holding the cntlr's reads off while it runs (CN1;
  `dnagent.md` SH30): on a wide SP an attempt can outlast the worker's
  bound on an observation's age (`dnv-worker.md` HL7), and the primary
  then reads unreachable (Known limits).

  **The array is read from sysfs.** The **md walk** lists "/sys/block"
  for the array nodes and each array's "md" directory, reading every
  member's "block/dm/name", as the sweep's `ListArrays` does — once per
  converge or read of a cntlr, shared by all of its groups and by the
  pool concats (below): a walk per group would list every array once per
  group. Its listings are made in the agent's own process
  (`osclient.md`, ReadDir). A converge takes it with `Md.Walk` at the
  first group that needs it; a read takes it in its reading (CN28), with
  `Md.Scan`.
  An array node is "md" followed by digits, or "md_" followed by a name:
  the node mdadm puts the "/dev/md" link on when mdadm.conf says to
  create names, which `architecture.md`, md names, sizes `CnMdDevName`
  for. The walk reads every array on the node, other sps' included, so
  an array whose "md" listing or a member's dm name did not answer — a
  listing or a read held past the soft timeout on the agent's `OsClient`
  semaphore, which every cntlr it serves shares, ctx cancelled, or a
  listing or a read failing with an errno other than ENOENT — is
  recorded as unanswered instead of failing the walk: failing this pass
  for another sp's array would turn this sp's md rows `ERROR` — which
  count toward cntlr health — for a fault that is not this sp's. An array
  another cntlr is stopping never reads as unanswered: md removes a
  member's "block" link as it unbinds it, so that member's dm name reads
  ENOENT and the member is recorded with no dm name, which no group's
  names match; an "md" directory that went makes its listing answer
  absent and drops the array. The sweep's `ListArrays` reads such an
  array as foreign while a member is unbound, but not for the whole stop:
  from the moment md marks the array deleted until its "md" directory goes,
  "array_state" reads EBUSY, and `ListArrays`' strict rule fails that
  pass's md enumeration — a Leftover the worker re-drives, from a sweep
  that removed nothing (CN21). By default md marks it when the stopped
  array's last reference goes — a close, or the end of a read of one of
  its "md" attributes, whichever is last — after its member directories
  are gone; with the md module's legacy asynchronous gendisk deletion
  switched off it marks it in the stop itself, while the unbound member
  directories are still there. A converge's md walk reads no
  "array_state" and is unaffected. The md walk of a cntlr's read reads
  every array's "array_state" as well, for its verdict,
  and keeps it apart: the group rows read the walk under `Md.Walk`'s rule
  and never see it, and only the verdict, under `ListArrays`' rule, fails
  on it, as the sweep's enumeration does (CN30). Only a "/sys/block"
  listing that did not answer fails the walk. After an assembly
  `Md.Refresh` lists "/sys/block" again, drops the nodes that went
  (recorded or unanswered), and re-walks only a node
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
  wrapper of a member the group keeps: of its `leg_list`, or a new leg of
  its open repair (A group's repair, below), so that an array a whole new
  leg started alone is found. An aborted repair's new leg is no key: one
  md holds is an extra, found through the member that stays. No such array
  is "absent" and runs the assembly above — unless an array of the walk is
  unanswered, which may be
  the group's own: then no answering array proves absence, the group's error
  names the unanswered array, and nothing is created or assembled. Only a
  listing of an array's "md" directory that failed with anything but ENOENT,
  or a member dm-name read failing with something other than ENOENT, makes
  an array unanswered; a foreign array's non-dm member reads ENOENT and is
  recorded, so a foreign array cannot hold an assembly off for good. Nothing
  re-drives the refused assembly either — unless the same pass registers the
  CN10 retry for something else (any of CN10's triggers, among them a
  `leg_list` member of any group of this cntlr that is not available, above,
  and a namespace that waits for the layers above the group, CN16), whose
  next attempt is a whole converge and tries the group again: the group's
  error is a row, not a reply code (CN29), the error itself registers no
  CN10 background retry, and the cntlr's verdict reports a leftover (a reply
  code the worker re-drives) only while its md enumeration still fails
  (CN30) — so the group stays unassembled, its row `MISSING` on the Check
  rounds after the array answers, until the cntlr's next converge for some
  other reason (Known limits). Beside an answering match an unanswered array
  is left alone (it cannot be told from another sp's array whose read was
  cut off; the one thing it hides is a second array of this group). Two
  answering arrays are an error; a member with no dm name (a foreign member)
  of the matched array is an error. Of that array it reads "array_state"
  first and then, for a running array only, "degraded", "sync_action" and
  "sync_completed" — an inactive array has none of the three, and each of
  its members' "state" reads md's bare "spare" — and each member's "state"
  (a flag list, in_sync and failfast on a healthy dnv member), "slot" (its
  place number in the array, or none for a member that holds no place) and
  "block/dev".
  Running means "array_state" is clean, active, active-idle, write-pending,
  readonly or read-auto; an array in any other state (inactive, broken, …)
  is left exactly as it is and its state is the group's error. A read of the
  matched array that did not answer is an error, never absent. A matched
  array whose "array_state" has gone by that read (stopped since the walk)
  reads absent — unless an array of the walk is unanswered, which makes it
  the same error as no match: absent always means every array of the walk
  answered.

  **The group's device number** comes from the same match: the pool's
  concat (CN13) maps a `RedundMdRaid1` group by the number in
  "/sys/block/<node>/dev" of the array the match finds in the md walk, a
  degraded array included, read once per group in a converge or a read,
  apart from the array's other attributes, so a failed read of one of
  those fails the group's row alone. Reason: the group's row and its
  concat rows then read one array, so a read and a converge check and
  build the concat over the same array, and
  nothing waits for the link udev makes under "/dev/md". A group the
  match gives no array — none holds its legs, two do, or the md walk
  could not read the group's own array (no answering array holds its
  legs while an array of the walk is unanswered, above) or list
  "/sys/block" — gives its concat no number (CN28 states its rows, CN13
  what a converge does).

  **Member reconciliation** takes out a member the array holds that the
  group does not keep, adds a `leg_list` member the array lacks, and puts
  back a member md has failed, unless the in-sync guard (below) holds. A
  member the array holds is compared by its "block/dm/name" with the names
  of the wrappers the group keeps — its `leg_list` wrappers and
  the new legs of its open repair (A group's repair, below) — never by
  device number, because a number read of a wanted wrapper that did not
  answer would otherwise read as "not wanted" and fail and remove an in-sync
  member. A held member the group does not keep — such as an aborted
  repair's new leg (A group's repair, below) — is `--fail`ed and
  `--remove`d by the dm path sysfs named, while another member keeps the
  data (the in-sync guard, below); then a repair's removals; then a
  `leg_list` member md has failed and moved out of its slot ("faulty" in its
  "state", its "slot" none), on a leg that can take it, is `--remove`d; then
  an available `leg_list` member the array lacks, one just removed included,
  is `--add --failfast`ed, an open repair's bad leg in neither of these two
  steps; then a repair's new legs are added (below). md then resyncs: a
  bitmap catch-up for a briefly absent leg or a member put back, a full
  copy for a repair's new leg, short of a retired leg taken back while the
  array has stayed degraded (`architecture.md`,
  Repair). A leg **can
  take** a member back when it is available and its own leg row reads `OK`,
  which on the primary is the CN11 probe's last result. Reason: a leg whose
  disk returns errors usually fails its probe too, and is then left to the
  leg repair (`dnv-worker.md` AR8) instead of being put back on every pass.

  **The in-sync guard.** A member the group does not keep is
  `--fail`ed only while another member keeps the data: one md marks in
  sync ("in_sync" in its "state", and not "faulty"), whose wrapper is in
  `leg_list` and whose leg is available this pass. With none, the
  reconciliation leaves the array exactly as it is — no `--fail`, no
  `--remove` and no `--add` for the group that pass, the put-back
  included — and the group's error names the member and the array. The
  test is made once, before the first `--fail` of the pass: the member
  it asks for stays in `leg_list`, so the pass fails it only as an open
  repair's bad leg, and then only while another member keeps the data (A
  group's repair, below), and the answer is the same for every member
  that leaves, whatever that member's own state. Reason: md answers a
  fail of the last in-sync member of a mirror by marking the whole array
  broken (CN29), so without the guard the fail of a member the group does
  not keep breaks the group whenever that member is md's only copy. The
  member that keeps the data
  must be on an available leg because md learns that a member is dead
  only from IO to it: on an array with no IO a dead member still reads
  in sync, the fail would go through, and the copy onto the added member
  would read the dead member and break the array. And it must stay in
  `leg_list` because a member that leaves is about to be failed as well.
  And it must not read "faulty": md shows "faulty" beside "in_sync" for a
  member whose bad blocks it has not yet acknowledged, and the member
  that leaves may then hold the only good copy of those regions, which
  its fail would lose. A read applies the same test to the running array it reads, with the
  part of the leg's availability a read can see (CN28), so a converge and
  a read report the same row, short of a step only the converge takes
  (Known limits), and the read's verdict names no member of the group
  (CN30). That row reads `RES_STATUS_ERROR`, which counts
  toward the primary's health (`dnv-worker.md` HL2), so a guard that
  holds for the primary-unhealthy threshold can fail the primary over
  (Known limits). The guard lets go once a member that stays is in sync
  on an available leg — its copy ends, or its path returns: the next
  read then reads the row `OK` and names the member the array lacks once
  that member's leg can take it (CN30), and the converge of the
  `SyncupCntlr` that name brings finishes the reconciliation. A `leg_list`
  member md has failed is not put back while the guard holds, since the
  put-back is part of what the guard leaves undone. On a leg that is
  available it is not late either (above), and while its leg is not
  available the CN10 background retry brings only converges the guard
  refuses again, so nothing brings it back in sync while the guard holds.

  The put-back is a remove and then an add, run by mdadm. md keeps its
  claim on a failed member until the member is removed, so an `--add`
  alone is refused; after the remove, mdadm turns the add into a re-add,
  and md resyncs only the regions its write-intent bitmap marks, since
  md keeps every region written while the array is degraded marked in
  that bitmap. It is not a write of "re-add" to the member's "state":
  that write is made in the agent's own process and waits on md's own
  lock, which a superblock write stuck on a member holds for as long as
  that member's IO is held; it cannot be killed and would hold the
  cntlr's object lock with it, while mdadm is a child the soft timeout
  kills. The slot test keeps the remove from being refused: md moves a
  failed member out of its slot by itself once no IO to it is pending,
  and refuses to remove one that still has IO pending, which on a leg
  that can take it is only an IO the cutover window of a migration's
  source absorbed (`dnagent.md` DN12). Nothing limits how
  often one member is put back: a limit would need the agent to remember
  past attempts (Known limits).

  Every `leg_list` name is wanted, available or not: a leg this pass cannot
  use (no path both live and optimized, or its CN10 converge failing: a
  connect, an "address" read that did not answer, its multipath namespace or
  its wrapper) is never added, and a member md still holds for it is neither
  failed nor removed, whatever its state, but for an open repair's bad leg,
  which the repair's add takes out, and which the converge removes when md
  holds it without a place once the repair has started in md (A group's
  repair, below). `--fail` and
  `--remove` open the member's path (no IO) and act on its device number. A
  `--remove` issued within seconds of the member's side dying, while a
  superblock write is stuck on that member, sleeps in md's suspend until the
  write fails at the path's error recovery, so it can be killed at the soft
  timeout with the member still held: that pass reports the group `ERROR`
  and does not reach the `--add --failfast` that would follow either — of
  a `leg_list` member, since extras leave before any member is added, or
  of a repair's new leg, since its bad leg goes out first. A remove or an
  add that fails or is
  killed is the group's error for that pass, a row, not a reply code (CN29),
  and registers no CN10 background retry. What re-drives it is the read-only
  verdict of the cntlr's read, which names each `leg_list` member the
  reconciliation would put back or add, on a leg the read reads as able to
  take it (CN30): on the round that answers from that read the worker
  re-sends the `SyncupCntlr` (`dnv-worker.md` RW4), whose converge tries
  again. So work whose `--remove` was killed is finished by the converge
  that the named member brings, or, for a `leg_list` member that is not
  available, by the CN10 retry its lateness registers (above), cntlr-wide;
  a repair's new leg is never late, so while it is available but its leg
  row does not read `OK`, neither comes (Known limits). A member the group
  does not keep is never named itself. Nothing remembers the failure.

  The cn agent never runs mdadm --zero-superblock. A leg leaves an array
  only as a repair's bad leg, which the finish keeps as a retired leg and
  an abort leaves in `leg_list`; as a repair's new
  leg taken from a retired leg, which a cancel gives back as that retired
  leg; or out of existence with its side. Its superblock stays as md left
  it: that can make a later re-add cheap, and an assembly handed the leg
  reads it, beside the array's own, as no member in sync
  (`architecture.md`, Repair).

  **A group's repair** (`architecture.md`, Repair, owns the procedure: its
  stages, its abort flag and what the sp worker does with the repair
  verdict).
  The cntlr finds a group's repair through the group's `rep_name` in the
  request's `name_to_repair`; a name the request holds no repair under, or a
  repair of another group, is none. Every cntlr connects, wraps, probes and
  reports the repair's new legs as it does the group's legs, at every stage
  (CN10, CN11, CN28), and the sweep keeps them (CN9); a new leg never
  defers its group (CN9) and is never a late member, nor is an open
  repair's bad leg
  while the group's array runs (above). A repair is open while it is CREATED
  or STARTED. While it is open its new legs are members the group keeps —
  keys of the match and never extras — and its bad leg is never added and
  never put back, and the verdict never names it as a member to put back or
  add (CN30), though md may still hold it, or take it in again at an
  assembly (case 2, above), until the add, or the removal once the repair
  has started in md, takes it out (both below). Reason: the sweep runs
  before the build and judges an array by its members, so a bad leg that
  md still held once the finish retires it would make the sweep stop the
  group's array (CN9); and a bad leg put back would hold the place the new
  leg needs. An aborted repair's new leg is no member the group keeps, so
  one md holds is an extra, and its bad leg is a `leg_list` member like
  any other. The repair's steps, after the extras leave:

  * The add. For a CREATED repair whose abort flag the request does not
    carry, a new leg the array lacks is added once it is available, a
    provisioning leg never being so, while another member keeps the data:
    one md marks in sync and not faulty, of a `leg_list` leg other than
    the bad leg, on a leg that can take a member (the in-sync guard's
    test, with the leg row besides). The bad leg goes out first, in the
    same pass: one md holds out of its place is `--remove`d; one in its
    place that cannot take a member — it does not serve, or its leg row
    does not read `OK` — is `--fail`ed and `--remove`d; one in its place
    that serves with an `OK` row stays, and the repair waits, adding
    nothing. So the bad leg is failed only in a pass whose add's
    conditions hold, so that the new leg can follow at once. Reason: the
    leg row made the leg bad and md may never see that, since md learns
    that a member is dead only from IO, so waiting for md could never start
    the repair; failing the bad leg before the add can follow gains nothing
    and leaves the group on one copy; and a copy that reads a dead member
    breaks the array, so the member that keeps the data must be on a leg
    that can take a member. md refuses to remove a failed member while
    IO to it is pending: that refusal is the group's error for the pass
    and leaves the new leg out, and once md has moved the member out of
    its place the next converge removes it and adds the new leg. A
    STARTED repair's new leg that the array lacks is added once available,
    as a `leg_list` member is.
  * The put-back. A new leg md has failed and moved out of its place, of a
    STARTED repair or of a CREATED one whose abort flag the request does
    not carry, is `--remove`d once its leg can take a member, and the add
    above adds it again under its conditions. Reason: as for a `leg_list`
    member, one IO error fails a member at once (failfast, above), and
    while md holds the failed new leg nothing else takes it out, so the
    repair's copy would never resume. For a CREATED repair whose flag is
    set it is not put back, since the abort needs md to hold nothing on
    it.
  * The abort. For a CREATED repair whose abort flag the request carries,
    no new leg is ever added (`architecture.md`, Repair, gives the abort
    protocol), and one md holds without a place — its "slot" none, failed
    or not — is `--remove`d, so that md holds nothing on it and the
    repair verdict confirms the abort (CN28). A new leg that holds a place
    stays, and the repair verdict says started once md holds nothing on
    the bad leg (below); one md has failed that still sits in its place
    stays too, with neither repair verdict, until md moves it out.
  * A bad leg that md holds without a place is `--remove`d once the
    repair has started in md: it is STARTED, or it is CREATED, with or
    without the abort flag, and md holds every new leg in a place. The
    primary reports started only once md holds nothing on the bad leg
    (CN28), so md holds it again only from an assembly that took it back,
    as after a failover that comes before or after the worker's move to
    STARTED. Reason: left there, it would keep a CREATED repair from being
    reported started and, its new leg holding a place, from having its
    abort confirmed, so the repair would never move on; and the finish of
    a STARTED repair would retire a leg md holds, which makes the sweep
    stop the group's array (the reason above). md removes a member that
    holds no place with no IO to it, so the removal runs whatever the
    leg's path.

  The assembly takes an open repair's available new legs as candidates
  beside the `leg_list` members (case 1 and case 2, above). Reason: the
  legs handed to mdadm are candidates, and membership is judged from the
  superblocks, by mdadm and by the kernel, which takes a member's role from
  the freshest superblock when the member's event count is behind; so a
  half-copied new leg resumes its copy after a failover and never starts
  the array alone, a stale one is dropped, and a whole one starts the
  array degraded when both `leg_list` legs are dead only if md recorded
  them as failed, the group waiting for its legs otherwise
  (`architecture.md`, Known limits). Case 1's create holds only when no
  candidate carries a superblock, and its members are the `leg_list` legs
  alone: a new leg that carries one shows that the group's array exists,
  and a new leg that carries none joins an array only by the add, under
  its conditions. A converge reports the repair verdict and the whole
  flag and in-sync list of CN28 from the array as the pass leaves it: a
  pass whose member reconciliation ran an `--add`, `--fail` or `--remove`
  against the array reads the array's node again (`Md.RefreshArray`), since
  `Md.Refresh` sees no member an `--add` made, and a read again that
  fails, or that finds the array not running, gives neither that pass,
  the group's row staying the reconciliation's. A read again whose listing
  of the array's node fails leaves the md walk the pass shares as it was,
  so it fails neither the slice's concat rows nor a later group of the
  pass, which find their arrays in that walk.
  A remove or an add of a repair that fails is re-driven as the put-back's
  is: the read's verdict names the new leg the converge would add, or the
  member it would remove (CN30).

  A group whose `leg_list` holds a provisioning leg (CN9) is
  **deferred**: none of `--examine`, `--create`, `--assemble`, `--add`,
  `--fail` or `--remove` runs, no `CnGrpName` is built, and
  `grp_id_to_md_raid` reports `RES_STATUS_PROVISIONING`. The assembly
  cases above are evaluated only once every `leg_list` leg of the group is
  provisioned. (An unprovisioned new leg of a repair does not defer the
  group (CN9): a new leg joins only once it is available.) Deferral here is
  decided by
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
timeout. A read's probe killed that way would read the group's
row `ERROR` for a member fault the leg row already reports, and that row
counts toward cntlr health: it would fail the primary over. What the
sweeps, the member reconciliation, the md rows and the pool concats need
about an array they read from sysfs, which touches no member device and therefore
cannot block on a dead leg (of the md probes, only the assembly's
`--examine` above still reads a member, and only an available leg's):
`ListArrays` walks "/sys/block" for the array nodes (numbered or named,
above), reads each array's "array_state", and names each member through
its "block/dm/name" under the array's "md" directory — which is what
attributes an array to an sp, and a member with no such attribute is not
a dm device at all. `Md.Walk` makes the same listings and dm-name reads
(not "array_state"), once per converge for the md groups (above), under a
different rule for an array that did not answer: `ListArrays` keeps the
strict one — one array whose "array_state", "md" listing or member dm
name did not answer fails the whole md enumeration, because a removal
decision needs the whole node; that is the sweep's "enumeration failed:
md arrays …" and a `ReplyCodeLeftover`, never a row, and the worker
counts a Leftover reply as accepted, so it does not reach cntlr health —
while `Md.Walk` records such an array as unanswered. The md walk of a
cntlr's read, `Md.Scan`, makes `ListArrays`' reads once
and serves both rules: its group rows and concat rows read it under
`Md.Walk`'s, its verdict under `ListArrays`'. `Md.Gone` verifies a
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

The targets are the slice's own groups; a clone meta group (CN12) is the
target of no concat. Each concat target names its group's device by
number: a `RedundNone` group's `CnGrpName` by its `dmsetup info` number,
an md group by the number CN12 reads from the array the group's match
finds. A group the match gives no array — none holds its legs, two do, or
the md walk could not read the group's own array or list "/sys/block" —
fails its concat
for the pass: the live concat is left as it is, the failure is reported
the way any unbuildable concat is (below; CN28), and the converge skips
that slice's pool step, and with it the slice's snapshot messages, until
its next pass, so a snapshot whose messages that pass sends is torn
(CN14, "A torn snapshot is reported").

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
auto-grow off. In the pool layers `PROVISIONING` marks only the deferred
resources, never the live ones. A concat may only ever grow, and the agent
enforces that rather than trusting the plan: when the live table totals more sectors
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
   after a crashed earlier pass, which the message's refusal says with
   EEXIST, an errno the command wrapper hands back in the error's type
   (`dnagent.md`, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`) and
   the torn rule below reads.

Both clauses are re-derivable from the request alone, which is why
`ensureThin` never messages a td with a non-zero `ori_id` whatever the
pre-pass did, and why no handoff between the two is needed.

A created td is never messaged on any pass — fresh primary after a
failover, startup reconcile from the local store (`dnagent.md` SH1 to SH3), a device
removed by hand, ever. `ensureThin` reads the device's info and, when the
device is absent, runs `dmsetup create` directly. If dm-thin refuses the
thin table — that create, or the reload of a volume whose table differs —
because the pool does not hold the id, with ENODATA, which the command
wrapper hands back in the error's type (`dnagent.md`, OS wrappers —
`dm.go`, `nvmet.go`, `nvmehost.go`), the row reads `RES_STATUS_ERROR` with
the dmsetup output in its details and the reason `RES_REASON_THIN_ID_LOST`
(`architecture.md`, Live-state reporting), which the worker's shared-state
class reads (`dnv-worker.md` HL2); the worker records `err_epoch`, and no
later converge sends a message either: pool-metadata loss surfaces as an
event for an operator instead of an empty volume under a live `dev_id`,
which is what an unconditional thin create would produce. This build is
the one place the reason is set, and only for a created td: an uncreated
snapshot's table meets the same refusal until its message has made the id,
and its row carries no reason, and a read's probe finds a created td's
absent volume `MISSING` with no reason (CN28). Reason: the worker
decides by the field, and a tool's wording is no contract. Standby cntlrs
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
detection, no distinct status and no reason — the origin guarantee is the
gateway's contract to keep, not the agent's to re-check.

**A torn snapshot is reported.** A primary that stops between two slices'
messages, or a slice the pre-pass skips or whose message fails, leaves a
snapshot that a later pass finishes from another instant: a torn snapshot
(`architecture.md`, Thin devices). The pre-pass marks the td torn when one
run of it makes a new thin id in one slice while another slice of the
snapshot already has its id: that slice's snapshot thin device exists, or
its message is refused with EEXIST, read from the error's type, never from
its text; the device of every slice that is not deferred is read,
whether or not its pool is ready this pass. A slice whose device read
fails, one with no device whose pool is not ready, and one whose message
fails otherwise count as neither, and a run that makes every slice's id,
or none, marks nothing. The agent keeps the mark in memory (`tornTds`)
from that run until a request carries the td `created` or the td leaves
`td_list`, and every report carries it in the td's entry of
`td_id_to_thin_info` while the td is uncreated, a converge's and a
read's alike (CN28), so the created flip, which either reply can bring,
writes it into the thin device record
(`dnv-worker.md` RW19). The mark is best effort: between the run that saw
the tear and the flip, an agent restart can lose it, and so can a failover
or a cntlr replacement that moves the primary role off this cntlr (Known
limits). `created` certifies materialization, not point-in-time
consistency: a later pass sends the remaining messages, every row goes
`OK`, and the flag flips, torn only when the mark has lasted until then.
Reason: the one remedy is to delete and re-create the snapshot, and
nothing else tells the operator which one.

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
(`architecture.md`, Known limits, names the etcd restore that can break
that) and thin ids belong to tds alone. An agent restart under a
surviving pool device does not sweep (nothing else writes the pool, and
the CN2 zero-mutation reconcile stays intact), and a startup reconcile that
re-creates the device (a node reboot) arms but does not run it: it
converges from the persisted request, which lags the pool's true
contents after a persist that failed (one that fails is only logged and
the converge runs on, `dnagent.md` SH5), and deleting against a lagging
`td_list` would destroy a live td — the first revision-gated
`SyncupCntlr` after boot runs the sweep instead. A sweep failure is logged
and retried on later converges until it succeeds once; a stray surviving a
crash between creation and sweep waits for the next sweep of the pool's
ids that gets through. A
delete message that fails, refused or unanswered, runs the same sweep of
that pool in the same pass, on the same condition — the request the pass
converges came by the revision-gated RPC — and that sweep keeps every id
that a thin volume of the sp the pass lists still maps in that pool, read
from the volume's live table, since such an id is no stray; a volume
whose table names a device the pass's listing does not resolve keeps its
id in every pool swept, and one whose table cannot be read while it is
still there stops every such sweep of the pass. A sweep that gets through
also ends a pending activation sweep of its slice. Nothing of the failure
is remembered: a sweep that fails too, or a failed delete in a pass whose
request did not come by the RPC, leaves the id to the next sweep of that
pool's ids that gets through — the one after a later failed delete in
that pool, or an activation sweep — and no pass aims at it. Reason: only
a sweep of a pool's ids finds an id that has no dm device, so without the
one after a failed delete, the id's blocks can stay allocated for the
life of the pool device.

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

0. the td is provisioning-deferred, or a clone whose clone meta group is
   (CN9; a side under it is still zeroing) targets the td below
   `SP_LEVEL_NO_CLONE` ⇒ table → the
   td's `CnErrorName` (the ns-dev and the nvmet namespace exist
   throughout, `architecture.md`, [D15] — the "rule 0" the code comments
   cite);
1. the namespace is effectively suspended (below) ⇒ table → the td's
   `CnErrorName`, live — the parked rule (`architecture.md`, Namespace
   suspend semantics, and [D12]); never under dm-flakey;
2. standby or disabled cntlr ⇒ table → the td's `CnErrorName`;
3. `sp_level` at or above `SP_LEVEL_NO_THINPOOL` ⇒ → `CnErrorName`;
4. a clone targets the td (a `clone_list` entry whose `dst_td_id` is the
   namespace's `td_id`) and `sp_level` at or above `SP_LEVEL_NO_CLONE` ⇒
   → `CnErrorName` (a raid0 with holes must never serve);
5. a clone targets the td ⇒ → `CnCloneFinalName` (via dm-flakey when rule
   7 applies) — only while that dm-clone serves: its `dmsetup status`
   shows hydration enabled, the mark CN18 step 5 alone sets, after step
   4's source chunks, and its metadata in read-write mode. Reason:
   hydration on marks a build that finished, so a namespace is optimized
   over a dm-clone only once the dm-clone copies and records what it has
   copied; and a dm-clone whose metadata is read-only or failed cannot
   record a copy, so a write to a region it has not copied fails, and a
   namespace over it would hand its hosts IO errors that a wait avoids.
   While it does not serve — a build that has not
   finished, whatever stopped it; a dm-clone whose metadata is read-only
   or failed; and a dm-clone that does not exist, whether a sweep removed
   it, the build has not reached it, or its clone meta group is not ready
   (CN18 step 2) — the ns-dev gets the td's `CnErrorName` instead, parked
   like rule 1: live, never under dm-flakey, its row `OK`, and its
   namespace waits in the inaccessible group (the ANA rule below), moved
   there before the reload where the ns-dev was serving. Every converge
   and every probe reads that status afresh (`nsDevNow`); nothing is
   remembered. A dm-clone's absence is confirmed, never assumed: only when
   the status read fails does a `dmsetup info` ask whether the device is
   there, and only its answer that it is not reads as not serving
   (`cloneServes`). A read that fails or does not answer, of a dm-clone
   that is there or whose absence that `dmsetup info` did not confirm,
   moves the ns-dev nowhere: it keeps its table, its `ns_id_to_dm_linear`
   row is `RES_STATUS_ERROR`, and its namespace keeps its group unless
   that table is the park (the ANA rule below). The namespace turns
   optimized in the pass whose read here finds the dm-clone serving and
   moves the ns-dev onto it, and every pass before it that leaves the
   namespace waiting registers the CN10 retry that runs such a pass,
   whatever stopped the build (the ANA rule below);
6. otherwise ⇒ → `CnRaid0Name` (via dm-flakey when rule 7 applies);
7. `sp_level` at or above `SP_LEVEL_READONLY` (primary only): the table
   is a dm-flakey table with the error-writes feature over the rule-5 or
   rule-6 backing — reads pass, writes error (`architecture.md`, [D11]).

Rules 4 and 5 and the `auto_resume` override below combine into one flip
worth stating out loud. An `auto_resume` clone's destination namespace
is stored suspended, which on its own would leave it inaccessible and its
host queueing; the override makes it serve, so below `SP_LEVEL_NO_CLONE`
it is optimized over `CnCloneFinalName` once that dm-clone serves (rule
5). At or above
`SP_LEVEL_NO_CLONE` the override still applies (it keys on the
`clone_list` entry, not on whether the clone stack was built), so the
namespace stays exported and stays optimized while rule 4 parks its
ns-dev on `CnErrorName`: what changes is the backing, and the host
therefore takes IO errors instead of queueing. That is deliberate and
consistent with the `SP_LEVEL_NO_THINPOOL` posture (CN19: a host sees IO
errors, not a vanished device); it is not a bug. "Queueing
(inaccessible)" describes only the stored-suspended baseline and rule
5's wait for the dm-clone, never the state the override leaves behind at
that level.

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
while a dm-clone the plan does not want may still write into that td
(CN18's hold): the namespace then stays parked and inaccessible, its
ns-dev row `ERROR`, until a pass that leaves no such dm-clone live (CN18).
No path of a pass leaves a CN device suspended unless a `dmsetup` command
on it fails, and one such failure leaves it so
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
whose table already matches, and one it holds off its raid0 (CN18) as
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

**ANA**: the desired group is `AnaGrpIdOptimized` iff primary and not
disabled and not effectively suspended and its backing chain is not
provisioning-deferred (CN9) and the `sp_level` is below `SP_LEVEL_DISABLE`
(CN9's `wantAny`); else `AnaGrpIdInaccessible` (single "ana_grpid" writes,
`dnagent.md` SH19). The
deferral conjunct is what makes initial provisioning painless for hosts:
while the td's legs are still zeroing, the namespace stays inaccessible
and hosts queue on the path instead of eating IO errors from an
error-backed ns-dev; it flips to optimized on the converge that follows
the last leg's provisioning. A deferred namespace's own rows
(`ns_id_to_dm_linear`, `ns_id_to_namespace`) report
`RES_STATUS_PROVISIONING` — the ns-dev and the nvmet namespace do exist,
on the permanent dm-error, but reporting them `OK` would show a fully
healthy `CntlrInfo` while no host can do IO.

**A namespace turns optimized only over what the rules select.** The
build's last loop moves a namespace whose desired group is optimized there
only in a converge whose ns-dev step put the ns-dev on the backing the
rules select, or found it there — its **serving device** under rules 5 to
7, the raid0 or the dm-clone that serves, through the dm-flakey at the
read-only level, or the td's `CnErrorName` under rules 3 and 4 — or held
it on a device it still serves through (CN18's hold, below). An ns-dev
the step leaves or finds on the td's `CnErrorName` while the rules want a
serving device — rule 5's fallback, CN18's hold, a promotion's stack not
built yet — puts its namespace in the inaccessible group, where it waits:
its hosts queue their IO. An ns-dev
CN18's hold keeps on a device it still serves through puts its namespace
in the desired group: pre-step 2 may have moved it to inaccessible before
a park whose load failed, and the ns-dev serves on, so a wait would pause
its hosts for nothing. Every
park of a planned ns-dev whose rules want a serving device — CN9's
pre-step 2, rule 5's fallback — moves the namespace to inaccessible before
the reload. After the level that removes the clone stacks is lowered, the
ns-dev is still on the `CnErrorName` rule 4 selected, its namespace
optimized over it, so it serves IO errors until the pass that builds the
dm-clone moves it onto it, or, when that build does not finish, until
the end of that pass, which moves the namespace inaccessible (CN19); a
clone whose clone meta group still provisions defers the namespace
instead (rule 0), so CN9's pre-step 1 moves it inaccessible at the
start of the first pass after the level is lowered, which builds no
dm-clone. A
move that fails is logged
and the park goes on, since the park guards the data and the move only
saves the hosts IO errors. An ns-dev step that fails, whatever read or command
failed — the number of a raid0 not built yet included, which is how the
step fails at a promotion that came before the sides' ANA flips
(`architecture.md`, [D16]) — reads the ns-dev's live table afterwards, and
the namespace keeps the group it has unless that read shows the table on
the td's `CnErrorName` while the rules want a serving device: a failed
read never moves an optimized namespace back, since the ns-dev may still
serve and the move would pause its hosts for nothing. A namespace object
the build creates, or finds not enabled, gets `AnaGrpIdInaccessible`
before its enable, never the kernel's default group, so only the last loop
moves a namespace to optimized. A converge that leaves the ns-dev of a
namespace whose desired group is optimized off the backing the rules
select — waiting, held or failed — registers the CN10 retry, whatever the
cause and for as long as it lasts (Known limits). A move of the
last loop that fails registers the CN10 retry too: the namespace stays
out of the group this rule gives it, even over an ns-dev on its serving
device. Reasons: an IO
error the target returns with the do-not-retry bit reaches the application and
cannot be taken back, while a wait ends by itself, and everywhere else the
design makes a host wait when the stack is not there (the deferral
conjunct above, CN18's hold); only the converge knows whether it put the
ns-dev where the desired state wants it; and the worker re-syncs on a
revision or a reply code, never on a row, so without the retry a short
cause would leave the hosts waiting for a revision that may never come.
Rules 3 and 4 serve IO errors on purpose, their namespaces optimized over
the dm-error (above; CN19): each is a level the operator chose, and a wait
there would never end. A stack that never builds keeps its namespaces
inaccessible with no time limit; an operator ends the wait by disabling
the cntlr, by a level step or by a delete.

CN17. Transfers (`xfer.go`; both roles; `architecture.md`, Transfers).
Per `xfer_list` entry: resolve the origin namespace by `ori_nqn` and
`ori_ns_idx` in `nqn_to_subsystem` (unresolvable ⇒ the transfer's
resources report `RES_STATUS_ERROR` and the pass continues, CN29).
`CnXferFinalName`: on the primary a dm-linear over the origin td's raid0
(→ dm-error at `sp_level` at or above `SP_LEVEL_NO_THINPOOL`); on a
standby a plain dm-error table of the same size. Subsystem `XferNqn` on
the node port: "attr_serial" from the transfer id and "attr_model"
`NvmeModel` (the rule of `architecture.md`, [D2], applied to the
transfer — every cntlr exports the same identity so the destination clone
sees one multipath device), "attr_cntlid_min" and "attr_cntlid_max" from
`Cntlr.cntlid_slot`, `allowed_hosts` from the record; one namespace whose
nsid is `ori_ns_idx`, with the origin namespace's "uuid" and "nguid" and
the transfer device as its "device_path"; ANA optimized on the primary,
inaccessible on standbys (below). The origin namespace's own retirement is
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
does at `SP_LEVEL_NO_THINPOOL`: the transfer's desired group follows the
role, the level and deferral only, so the destination is handed IO errors
from the error table, not a queue: an origin gone from the request is
lost for good, a level is the operator's choice, and a wait for either
would never end. The live table is deliberately not the
plan applied last time: it is the same answer, from the kernel, with
nothing remembered. A transfer whose origin td is provisioning-deferred
(CN9) is deferred with it: its three rows report
`RES_STATUS_PROVISIONING` and its namespace stays `AnaGrpIdInaccessible`
— mapping a linear over a raid0 that does not exist yet would only
produce an `ERROR` and an `err_epoch`.

**A transfer turns optimized only over what the desired state wants**, by
CN16's rule and for its reasons. The build's transfer loop moves a
transfer whose desired group is optimized there only in a converge that
put its transfer device where the desired state wants it, or found it
there: the linear over the origin td's raid0 where the transfer is
served, the error table where it is not on purpose, and for an origin
that no longer resolves a demotion that succeeded or a device that is
not there. A transfer whose device step fails keeps its group, except a
served one whose device's live table is an error table, which moves to
inaccessible — at a promotion that came before the sides' ANA flips the
origin's raid0 is not there yet. A transfer namespace the build creates,
or finds not enabled, gets `AnaGrpIdInaccessible` before its enable, and
a converge that leaves the device of a transfer whose desired group is
optimized off what the desired state wants registers the CN10 retry, as
does a transfer's move that fails. The clone in the other pool then waits
rather than reads IO errors.

CN18. Clones (`clone.go`; primary only; `architecture.md`, Clones;
`sp_level` below `SP_LEVEL_NO_CLONE`). Per `clone_list` entry, in order:

1. `nvme connect` to `src_nqn` at every entry of `src_tr_conf_list` (one
   subsystem, one path per source cntlr; the source's own ANA picks the
   serving path), with the host NQN `CnHostNqn` and the `dnagent.md` SH20 flags;
   failures go to the CN10 retry registry. The source namespace device
   is found by a lookup in the pass's sysfs walk like CN10's, by
   `src_nqn` and the nsid `src_ns_idx`. When the walk holds no entry for
   the source, or its entry fails CN10's check, the converge walks once
   more before it connects the source: every cntlr of the node may
   connect a clone source, so another may have connected it since the
   walk, and a source that walk finds connected is adopted. The connect
   step is CN10's, drawing on the same pass budget the legs drew on: a
   failed connect is retried within the pass while the budget covers the
   pause, and after a connect this pass made the source is looked up
   again, as CN10's head wait looks a leg up, until the source namespace's
   head is complete, on CN10's terms. With the budget spent — the legs
   may have spent it — a source that did not connect, or whose namespace
   did not appear, fails step 1 ("no namespace … on …" for the latter),
   and a source whose head is picked but not complete passes step 1 and is
   used as it is: step 1 reads no number, and step 3's table names the
   source by its number (CN28), so a read of that number that fails fails
   step 3. CN10's unknown-controller rule is not applied here: a source
   controller whose "address" did not answer matches no entry, so an
   entry it alone serves reads unconnected and is connected again — a
   duplicate the host refuses while that controller lives, which fails
   step 1 once the budget is spent — and nothing is disconnected, since
   step 1 retires no path. CN10's rule
   for a walk or a lookup that did not answer is applied: it fails step 1
   naming the read, and nothing is connected on it; `clone_id_to_dm_clone`
   then reads, while no dm-clone of the clone is up, `MISSING` "source
   unknown: …" naming the read, where step 1's other failures read "source
   not connected" (the shared rule below). A source whose disconnect a
   sweep has set going is neither adopted nor connected while that
   disconnect runs: step 1 fails naming it, and the retry reads the
   source afresh once the disconnect has returned (CN10's disconnect
   registry).
2. The metadata device: the clone's clone meta group (`architecture.md`,
   Terminology and object model), whose legs CN10 connects and wraps and
   whose md array or `CnGrpName` linear CN12 builds earlier in the same
   pass. The group is ready when its row of the pass (`grp_id_to_md_raid`,
   CN28) reads `OK`. A running array's row reads `OK`, degraded or not,
   unless a read of the array fails, another array also holds legs of the
   group, it holds a foreign member, the in-sync guard holds over it, on
   a converge and a read alike, or, on a converge, its member
   reconciliation fails otherwise (CN12, CN28): each of these reports
   the group `ERROR`, and the group is not ready for that pass. A group that is not
   ready ends the pass for the clone: no dm-clone is created over it, and
   one an earlier pass built is left as it is. While the read of the
   dm-clone shows none that serves, the CN10 retry is registered, whose
   converge runs the group's build, the reconciliation included, again; a
   dm-clone that serves serves on (CN16 rule 5), and the group's fault is
   re-driven as a slice's group's is (CN12). Reason: a dm-clone created
   over a device that is not the group's assembled array would format new
   metadata there and copy the source over what the hosts wrote; with no
   dm-clone that serves no region is copied, and the worker re-syncs on a
   revision or a reply code, never on a row, so nothing else is sure to
   re-drive the copy, whether or not a namespace of the destination td
   waits, and a parked namespace of the td waits for the retry as well,
   while a dm-clone that serves copies what is left and serves its
   namespaces without it; the
   row is the one test of readiness, the one the worker judges and the one
   the probe writes; and the group's own row says what is wrong with it, so the
   clone's row does not say it a second time (below). The build
   then reads the dm-clone, and that read decides steps
   3 and 4. A read that fails or does not answer, of a dm-clone that is
   there, ends the pass the same way: nothing is created or enabled, and
   CN16 reads the status again and moves no ns-dev onto a dm-clone it has
   not seen serve. A dm-clone that a `dmsetup info` after a failed read
   confirms gone reads as absent, and step 3 creates it. A dm-clone whose
   metadata is read-only or failed — dm-clone leaves read-write mode when
   an operation on its metadata device fails — ends the pass too and is
   left as it is: no chunk lands and no enable takes in that mode, its row
   reads `ERROR` naming the mode, CN16 does not serve through it, and
   nothing the agent does takes it back to read-write mode (Known limits).
3. The dm-clone `CnCloneFinalName`: metadata = the clone meta group's
   device, named by its number by the rule the pool concats name a group
   by (CN13: an md array's from the pass's md walk, a `CnGrpName`'s from
   `dmsetup info`), destination = the destination td's `CnRaid0Name`,
   source = the connected device, region size = one block in sectors,
   created with the two features "no_hydration no_discard_passdown" always
   — the second feature is not optional: `blkdiscard` is this design's
   metadata-only "mark hydrated" primitive (`architecture.md`, Bitmap push
   protocol, and raid0 bitmap math), and with passdown enabled dm-clone
   also remaps the discard to the destination, unmapping a block a host
   wrote through the dm-clone; `agent.CloneTable` carries the parameter
   and every caller passes it, because a `blkdiscard` must stay a
   metadata-only mark for the dn migration clone as for the cn clone. Over
   a group that holds the clone's metadata already — a new primary, a
   rebooted node, a dm-clone the level or a sweep removed —
   dm-clone loads that metadata, and the copy goes on from where it was;
   over a fresh group it finds the first data block zeroed
   (`architecture.md`, Side provisioning protocol) and formats new
   metadata there. A dm-clone that is there is converged probe-first: only
   its four positional arguments are compared, since the feature and core
   arguments it prints back follow its live hydration state, and one that
   differs is reloaded, with hydration off. Then the `hydration_threshold`
   and `hydration_batch_size` messages from `dm_clone_conf`, one per
   non-zero member the probed status does not already show (probe-first,
   `dnagent.md` SH16; a zero leaves the target's own default in place,
   `architecture.md`, Common validation).
4. On a pass whose step 3 created or reloaded the dm-clone, or whose read
   before step 3 found hydration off, apply every bitmap chunk of the
   applied set (`dnagent.md` SH21) — the CN22 fold over the
   `(src_slice_idx, bm_idx)`-addressed chunks this node holds, read in
   place. A dm-clone found with hydration off is one a build left short of
   step 5 — an agent killed between steps 3 and 5, a create killed after
   its ioctl ran, a refused enable — and its metadata is on its clone meta
   group, so the build finishes it rather than rebuilds it. Re-applying is
   idempotent: a chunk marks hydrated only a region not copied yet whose
   source is unmapped, and dm-clone drops a discard of a region it has
   copied.
5. The `enable_hydration` message to the dm-clone (`dmsetup message`) —
   only after step 4, so the chunks the node holds land before the copy
   starts.
6. Put the destination td's ns-devs onto the dm-clone and set ANA per
   CN16 — a reload when they already exist; a fresh converge simply
   creates them in CN16 with the clone backing (rule 5), which CN16
   installs only once the dm-clone serves: its status shows the hydration
   step 5 enabled and its metadata read-write. With `auto_resume` false
   the namespaces stay effectively suspended until
   `UpdateNamespaceSuspended`.

**One rule decides the clone's rows.** For a clone whose destination td
resolves, that carries a clone meta group and that is not deferred, the
converge, wherever its pass ends, and the CN28 probe decide the status of
each of its two rows by one rule, from what each reads of the node: the
source's lookup in the walk, the clone meta group's row of the pass, and
the dm-clone's presence and status line. The converge reads after its own
steps and a read only reads, so for the same state the two report the
same status, and a row's epoch moves only when the state does:

* `clone_id_to_target`: `RES_STATUS_ERROR` while a sweep's disconnect of
  the source is in flight or the walk or the lookup did not answer, both
  on this node's own side, not the source's; `RES_STATUS_MISSING` with no
  subsystem entry, and for a fault of the source, which is an endpoint
  with no live controller, its details the path states, or no source
  namespace; and `RES_STATUS_OK` with the path states otherwise;
* `clone_id_to_dm_clone`: `RES_STATUS_ERROR` when the dm-clone's read did
  not answer; `RES_STATUS_MISSING` when the dm-clone is absent, its
  details saying why — "source not connected", or "source unknown: …"
  when the walk or the lookup did not answer, while step 1 does not read
  the source as connected, and "metadata group not ready" while the clone
  meta group is not; `RES_STATUS_OK` with the raw status line while the
  dm-clone serves, its hydration on and its metadata read-write; and
  `RES_STATUS_ERROR`, followed by the line, when it does not: "metadata
  read-only" or "metadata failed" for its metadata mode, then "hydration
  not enabled" for hydration off, and "metadata mode not read-write" for
  a line that shows hydration on and no mode.

A step that fails in a converge decides no status: the error of a failed
connect leads the details of the source's row, and the error of step 3's
converge of the dm-clone or of the enable leads those of the dm-clone's
row when that row is not `OK`. The reads of the dm-clone that the rows are
written from are reads, not such steps, and the row takes them by the
rule above: when one does not answer, its error is the dm-clone row's own
details. `clone_id_to_dm_clone` carries the raw status line whenever the
pass read it, whatever its status, because `DeleteClone`'s hydration
check reads it (`architecture.md`, Clones). Where the namespaces serve
from does not follow the rows: CN16 rule 5 reads the dm-clone's status
itself. A dm-clone up with hydration off therefore reads
`RES_STATUS_ERROR` on every read as on every converge, and a fault of the
source reads `RES_STATUS_MISSING` on both, while a dm-clone that serves
serves on (Known limits); so does a dm-clone that serves while its clone
meta group's row is not `OK`, its row read from its own status line.
Reason: two reports of one device cannot both be right, and rows that
differ between the channels flip their status, and the health epoch with
it, each time a converge's observation and a read's follow each other. A
clone whose destination td does not resolve, or whose record carries no
clone meta group, reads `RES_STATUS_ERROR` on both rows, naming which, on
both channels, and nothing of it is read or built: there is nothing to
clone onto, or no metadata device to clone with.

**A fault of the source does not itself move the role.** It reads
`RES_STATUS_MISSING`, not `ERROR`, and a missing row sets no health epoch
(`dnv-worker.md` HL2), so the row leads to no failover and no cntlr
replacement. Reason: moving the primary cannot repair the source, and the
worker cannot tell a fault in the primary's own path to the source from
one at the source; the operator can move the role by disabling the
primary cntlr (`dnv-worker.md` AR5). A fault that fails step 1 still
keeps the CN10 retry running, whose attempts hold the primary's reads
off while they run, so its observation can become too old, which the worker
counts as a missed round, and missed rounds can move the role (Known
limits). A dm-clone up with hydration off, or with its metadata read-only
or failed, is this node's own fault and reads `ERROR`: the primary that
takes the role assembles the clone meta group and creates the dm-clone
over the metadata it holds (steps 2 and 3). A clone meta group no cntlr
can assemble leaves the clone's dm-clone row `MISSING` and its namespaces
parked on each primary that has to build the dm-clone, wherever the role
goes, at least until a leg of its `leg_list` comes back, since a repair
adds its new leg only to a running array (CN12) and leg repair creates no
repair for a group whose array does not run (`dnv-worker.md` AR8); the
operator may delete the clone. A
dm-clone that serves and whose copy is whole serves on over such a group,
since it writes no metadata (Known limits).

Any failure of step 1 registers the CN10 retry, and so do these later
ones, each of which stops the build before the dm-clone serves: a clone
meta group that is not ready while no dm-clone of the clone serves, a
read of the dm-clone before step 3 that
does not answer or that shows its metadata read-only or failed, anything
step 3's converge of the dm-clone fails on, and the enable (its own
status read included). So does a read after the enable that fails or does
not show a dm-clone that serves, whose line the dm-clone row carries
(`architecture.md`, Live-state reporting). Some failures stop nothing and
are only logged: a failed knob message of step 3 (the knob keeps the
value it had) and a failed source-chunk `blkdiscard` of step 4 (it costs
an extra copy). Step 6 is CN16's, and a namespace of the td that a pass
leaves waiting, whatever stopped it, registers the retry too (CN16).
Whatever stopped a pass, CN16 puts the td's ns-devs on the dm-clone only
while it serves (rule 5), which no dm-clone does before step 5 has run
on it; a later pass — the retry's — finishes the build.

**Removing a clone** is not a step of its own: it is the CN21 sweep
finding the clone's objects unwanted and taking them in layer order,
which is exactly the order this stack needs. A clone that left
`clone_list` loses its dm-clone, its source connection when the source is
a transfer of this cluster (CN21; a source of any other kind stays
connected, Known limits), and its clone meta group's array or group
device and legs; one the role or the level
merely suppresses loses its dm-clone, and of its clone meta group only
what the role and the level take from any group — a standby has no array
or group device (CN12), and the level takes the array or group device
from `SP_LEVEL_NO_REDUND` and the legs from `SP_LEVEL_NO_SIDE` (CN19) —
and the source connection stays, because
L5 reads "in use" off every stored cntlr's `clone_list` (plus the live
dm-clone tables), and a suppressed clone is still in that list.

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
  the plan does not want may still be live — L3 did not run, or did not
  verify it gone; on a pass an unanswered listing stopped, the dm listing
  names it — the build puts no ns-dev onto the raid0 of a td that
  dm-clone may write into, unless the ns-dev is on that raid0 already. A
  lingering dm-clone writes into the td whose raid0 its live table names
  as the destination device: the pass reads that table, resolves the
  device's major:minor to a name through its dm listing and holds only
  that td's namespaces. When the dm listing did not answer, or a
  lingering dm-clone's table read fails while the device is still there,
  is not one dm-clone target or names a device the listing does not hold,
  nothing is known of what it writes, and every namespace of the cntlr is
  held. One held over the dm-clone stays there and serves on through it,
  and its namespace takes the desired group: pre-step 2 may have moved it
  to inaccessible before a park whose load failed, and its hosts should
  not wait while it serves. One parked stays parked and goes
  inaccessible, as every namespace does whose ns-dev is on the dm-error
  while its rules want a serving device (CN16): its hosts should queue
  rather than take IO errors. One the pass cannot tell from the park,
  because the number of its td's `CnErrorName` device cannot be read,
  counts as parked and goes inaccessible, a wait rather than an IO error,
  and its namespace row reads `ERROR` (CN28). A new one is created
  parked. None of them is put on the raid0 on that pass, and none parked
  is moved to optimized; each
  reports its ns-dev row `ERROR` in the probe's words ("table is not the
  desired namespace backing"), and the hold registers the CN10 retry,
  because a hold that an unanswered `dmsetup ls` alone caused leaves
  nothing for the read-only verdict, which lists the node from the
  read's own reading (CN30), to name once that reading answers.
  The first pass whose L3 removes the dm-clone takes the order above — the
  park of one still over the dm-clone, L3, then the reload. Reason: a
  lingering dm-clone writes into one td, and holding every namespace of
  the cntlr for it would park every other volume of the pool for nothing.
* L3 removes the dm-clone, before its source connection dies — dm-clone
  flushes through the source on removal and blocks without it — and
  before its clone meta group goes, since the dm-clone's removal writes
  its last metadata through the group.
* L5 disconnects the source subsystem — the one a clone that has left
  `clone_list` loses beside its clone meta group when its NQN is a
  transfer NQN of this cluster (CN21); by NQN is safe here, as every path
  of it is going. The disconnect runs off the pass (CN10), so
  the connection is that pass's leftover until a later pass finds it
  gone, and a clone created on the same source meanwhile waits for it
  rather than adopt it (step 1). The set of unowned sources is computed
  at L5 rather than when the chain was built, because until L3 has
  actually removed it the dm-clone still maps its own source and would
  keep it claimed.
* L9 and L10 take the clone meta group's array or group device and then
  its legs, as for a slice's group: once the clone has left `clone_list`,
  nothing wants them (CN9's wanted set).

The clone's chunk files are not part of that chain. They are
swept separately, at the end of the cntlr-level sweep (CN21), against
the stored `clone_list` (`dnagent.md` SH7) — a clone that left the list loses them,
one the role or the level merely suppresses keeps them applied-by-file
(CN19, CN22 — deleting them on every standby converge would make the
worker re-push them forever, and a promoted standby's step 4 would have
no chunk to apply). Keying that deletion off the dm-clone's removal
instead would leak every deleted clone's chunks on a standby, which
builds no dm-clone at all. A clone whose destination td or clone meta
group is provisioning-deferred (CN9) is not built: no dm-clone and no
source connect, and below `SP_LEVEL_NO_CLONE` its two rows report
`RES_STATUS_PROVISIONING` and the namespaces of its destination td are
deferred with it (CN16 rule 0) until the provisioned flip of the group's
last zeroing side arrives as a new revision; at that level and above its
rows report the level (CN9, CN19). Reason: the group's sides zero only
their data-group length (`architecture.md`, Side provisioning protocol),
so the wait is short, and a host that queues on an inaccessible path
through it loses nothing.

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
* `SP_LEVEL_NO_CLONE`: no clone stacks: the dm-clone is removed, but a
  clone-source connection made at a lower level stays connected, and is
  not even reported as a leftover — the sweep's in-use test is
  level-blind (CN21: a source is in use while any stored cntlr's
  `clone_list` names it, which `sp_level` does not shorten, or a live
  dm-clone table maps it), so it is disconnected only once the clone
  leaves `clone_list` or the cntlr's stored state goes, and then only
  when its NQN is a transfer NQN of this cluster (CN21); the ns-devs of
  clone-target tds go on `CnErrorName` (CN16 rule 4), so an `auto_resume`
  clone's destination namespace stays exported and optimized and its
  host takes IO errors instead of queueing (CN16). A clone's clone meta
  group is a group, so its array or group device stays under the group
  gate until `SP_LEVEL_NO_REDUND`, and its legs under the leg gate: the
  metadata it holds is there when the level is lowered, and the dm-clone
  the build creates over it goes on from where the copy was (CN18 step 3).
  Lowering the level leaves a destination namespace optimized over the
  dm-error until the pass that builds the dm-clone moves its ns-dev onto
  it; one whose build does not finish in that pass goes inaccessible at
  the end of it (CN16). One whose clone's clone meta group still
  provisions is deferred instead (CN9, CN16 rule 0): the first pass after
  the level is lowered builds no dm-clone and moves the namespace to
  inaccessible at its start (CN9's pre-step 1), and it waits there until
  a dm-clone built after the sides' provisioned flip serves (CN16 rule 5).
* `SP_LEVEL_NO_THINPOOL`: no thin pools, thin volumes, raid0s or pool
  concats; every ns-dev and every primary transfer device on
  `CnErrorName` or an error table; namespaces stay exported with CN16's
  ANA (a host sees IO errors, not a vanished device).
* `SP_LEVEL_NO_REDUND`: no group devices (md arrays stopped, `CnGrpName`
  linears removed), a clone meta group's included; legs stay connected,
  wrapped and probed.
* `SP_LEVEL_NO_MIGRATION`: nothing — the level has no CN-side behavior
  (the migration dm-clone is a DN object; the CN's leg multipath needs no
  gating).
* `SP_LEVEL_NO_SIDE`: no leg connections or wrappers (their sides are no
  longer exported) and no health probes; host-facing and transfer
  subsystems remain, error-backed.
* `SP_LEVEL_DISABLE`: the wanted set is empty, which is the whole of this
  level: the CN21 sweep removes every cntlr-scoped object of the sp, and
  only the base state of `architecture.md`, Controller node, common,
  remains (the port, which is node-wide). The cntlr file and chunk files
  stay — desired state persists. Two things an empty wanted set cannot express are done
  explicitly beside it, because neither is an object on the node: the
  CN10 and CN18 background connect retry is cancelled, and every
  `ResInfo` history of the cntlr is dropped.

CN20. Reply `agent_reply`, `revision`, `cntlr_info`, `bm_info_list`, and
the `obs_stamp` and `save_time` of `dnagent.md` SH28.

**`agent_reply` is the sweep's verdict.** It is `ReplyCodeLeftover` iff
this cntlr's sweep was not clean — the node still holds an object of this
sp that the desired state does not want, or one of the enumerations did
not answer — and zero otherwise. The details list the leftovers by kind
and name, a bounded number of them with a tail saying how many more,
plus one "enumeration failed" entry naming each unanswered enumeration;
the full list goes to the agent log from every pass and every read whose
verdict is not clean, in the leftover record of `log.md`, Leftovers, so
a lingering leftover is visible on every pass and every read that still
finds it, not only once. The code means accepted with residue, not a rejection
(`architecture.md`, Teardown by sweep), and `revision` is the request's
own. No pass remembers it: every converge and every read recomputes it
by enumerating the node, and a `Check*` carries the code of the cntlr's
latest observation (CN30). A cntlr the conf gate refused
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
  what makes judging an unowned object safe: no cntlr converge, no read
  and no push runs beside the pass. A disconnect the pass sets
  going can complete after the lock is released (CN10), so it is CN10's
  registry, read by the CN10 and CN18 connect steps, that keeps a
  converge from adopting that connection meanwhile.
* **cntlr-level**, in `convergeCntlr` as CN9's sweep phase, under that
  converge's CN1 locks — node read plus that cntlr's object lock on the
  `SyncupCntlr` and connect-retry paths, the node
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
steps — the unowned subsystems and source connections — do reach an sp
that is in it: they are keyed on the stored cntlr requests, not on the
pointer list.

**Enumeration.** `dmsetup ls` (name to major:minor, which is also how a
live table's device argument is resolved back to a name); the sysfs md
enumeration of CN12 (`ListArrays`, strict: one array that did not answer
fails it — never `Md.Walk`'s unanswered rule); the sysfs walk of CN10
for nvme host connections — at cntlr level the converge's own, taken
right before its sweep and shared with its build, at node level one of
its own (`ListAllSubsys`) — whose list fails on any entry whose
"subsysnqn" read did not answer, as `ListAllSubsys` does (`dnagent.md`
SH20); and the listing of the nvmet subsystems tree — linked to the port
or not, a partially removed subsystem is unlinked but present. A cntlr's
read takes its verdict's dm, md and nvme listings from its reading
instead (CN30). An enumerator that did
not answer leaves its part of the snapshot empty and is reported as
"enumeration failed", and an
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
plan does not want may still be live it puts no ns-dev onto the raid0 of
a td that dm-clone may write into — the one its table names, or every td
of the cntlr when the dm listing is what did not answer or that
destination cannot be read (CN18) — unless the ns-dev is on it already;
that waits for L3 to remove the dm-clone, as it does after a chain whose
L3 did not run or did not verify one gone. A namespace
that has left `ns_list` it never touches, stopped sweep or not: that is
L1's alone (below), after P0 (CN16). The snapshot is thrown away at the
end of the pass: it decides only what to attempt, and every removal
re-probes its own object.

**Attribution** follows `architecture.md`, Teardown by sweep — by name,
and by what a nameless object is built out of — with these cn rules.
A dm device is ours by its parsed name (`architecture.md`, dm device
names): the cn role letter (`DmRoleCn`), our cluster, our cn; its sp is
the first id. An md array carries no sp in its name and is ours only when
every member is a leg wrapper (`CnLegName`) of ours naming one sp — an
array with a foreign, unparsable or differently-owned member is silently
skipped and never stopped, which is what leaves a co-hosted dn agent's
udev-assembled array alone. A side-export host connection is ours by the
cn id inside the NQN, which is how it is told from one another CN of this
cluster holds. A host connection is a clone source of ours only when its
NQN is a transfer NQN of our cluster (`XferNqn`), the one kind of NQN that
says a clone of this cluster dials it. Such a connection belongs to no sp
— its NQN names the source sp, not the cntlr that dials it — so the only
test there is is "does somebody here still want it": it is in use iff
some cntlr this process holds names it as a clone's `src_nqn` or a live
dm-clone table maps its namespace device, and unowned otherwise. A clone
source under any other NQN — another pool's subsystem, whose NQN its user
chose, or a transfer of another cluster — names no owner here, and the
sweep reads no other mark of who made a connection, so no sweep ever
judges it and it stays connected once its clone has gone (Known limits). A
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
visible here — a local-store save can fail and the converge runs on
(`dnagent.md` SH5), so the guarantee is about this process's state, not
about the files. Second,
for a subsystem no held request claims, its namespaces, in the listing's
order: the first whose "device_path" parses as an ns-dev (`CnNsDevName`)
decides for the whole subsystem — one of ours makes it that sp's; one
that is not ours (another cn's, another cluster's) makes the subsystem
foreign and untouchable — and a subsystem with no such namespace at all
is unowned. Those only the node-level scope removes. Calling anything
unowned assumes at most one cn agent per kernel (`cmd/dnv-agent cn`); a co-hosted dn
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

There is no layer L4: the id is retired and never reused.

L5. clone-source connections of our cluster's transfers (attribution,
above) that nothing maps any more. The set is
evaluated here, not when the chain was built: a snapshot taken before L3
still shows the dm-clone this pass is removing mapping its own source,
and deciding early would never disconnect it. Their disconnects run off
the pass (CN10), so a pass that sets one going leaves it behind and, by
the stop rule below, the layers under L5 wait for the pass that finds it
gone.

L6. per-td raid0s and dm-errors.

L7. each thin volume removed and then, only if its pool is one the
desired state still wants, the pool-side delete of its id under CN14's
rule; a delete that fails has that pool's ids swept later in the same
pass when the pass's request came by the revision-gated RPC (CN14).

L8. thin pools, then the concats under them. An arming of CN14's
activation sweep ends with its pool's life, but the layer drops its
pools' armings together, and only once every pool removal of the layer
is verified; while any is unverified the layer keeps them all, and each
unverified pool is reported as a leftover. An unverified pool may have
survived, and the next converge that wants it again finds the device
present and arms nothing (`ensurePool` arms only a pool it creates), so
dropping the arming there would lose its activation sweep for good:
every id deleted meanwhile could keep its data blocks for the life of the
pool. Keeping an arming too long costs one idempotent sweep; dropping one
too early cannot be repaired.

L9. md arrays (`mdadm --stop` on the node sysfs named, CN12) and
`CnGrpName` linears, a clone meta group's included, which L3 has freed of
the dm-clone over it.

L10. legs — a leg a finish retired among them, which is no leg of any
cntlr's request (CN9) — in the one order that is deliberately not
top-down: set the legs' disconnects going (whole-NQN is fine here: every
path of an unwanted leg is going; off the pass, through the CN10
disconnect registry), then remove the leg wrappers without waiting for
them. Their probers are cancelled already, by the converge's trim (below)
or by CN7's drop step, but a wedged prober still holds an open fd on the wrapper, so
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
effect of removing the clone's dm-clone, because a standby builds no
dm-clone at all and keying the deletion off one would leave a deleted
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

The connect retry's registration and the leg probers are not swept —
they are goroutines, not objects on the node. A cntlr that is dropped
loses all of them in CN7's drop step; a cntlr at `SP_LEVEL_DISABLE` loses
the retry in CN19; and
every converge trims the probers to the legs the plan still
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

CN23. Answer from the object's latest observation (`dnagent.md` SH28),
taking no lock of CN1, reading nothing of the node and starting no read:
reply `agent_reply`, `revision` and the info of that observation, and its
`obs_stamp`; an object held with no observation yet is answered with no
info and no observation time. An unknown CN or cntlr pointer — one this
process holds no request for, as when none was ever accepted, or when
the startup reload could not load or skipped its file (CN2) and none has
been accepted since — ⇒ `ReplyCodeUnknownObject` with a zero `revision` and a stamp
with no observation time. For a known object the `agent_reply` is the
observation's code (CN30), and a `CheckCn` or `CheckCntlr` round at the
same moment answers the same observation (CN24).

What the observation holds was read by the object's background read,
which a round starts (`dnagent.md` SH29), or made by a converge, whose
observation carries the converge's own rows and code (`dnagent.md` SH28).
A cntlr's read takes the node's reading first (CN28), then builds the
rows and then the verdict from that one reading, so its rows and its code
tell of one look at the node. The reading is the read's own, taken under
its CN1 locks and shared with nothing, so every read reads the node
afresh. A read of a cntlr whose stored conf the gate refuses (CN8) takes
no reading: its info is empty and it has no verdict (CN30). A CN's read
takes no reading: its probe runs first and its verdict second. A read
never creates, changes or removes an object of the node, though a
cntlr's read makes the kernel commit the metadata of each dm-clone whose
status it reads; its status reads of thin pools commit nothing
(`dnagent.md` SH17).

### `CheckCn` / `CheckCntlr`

CN24. Instantiate the `dnagent.md` SH24 to SH26 loop: one round answers
from the object's latest observation as the corresponding `Get*Info`
does (CN23), taking no lock of CN1 and reading nothing of the node, and
then starts the object's background read when `dnagent.md` SH29 allows
one. The read runs the probes of Probing and error capture — `probe.go`: a
cntlr's read takes the node's reading (CN28) under its CN1 locks and
builds its rows and its verdict from it (CN23); a CN's read takes none.
The round's `agent_reply` is the observation's code (CN30), which is what
drives the worker's re-sync on each round whose observation is not
too old and names a leftover — a converge's observation names only what its own removing
sweep names (CN30) — and, on a
`CheckCn` round that answers from a read, while a piece of the base
state that a `SyncupCn` would build reads absent or an ANA group that it
would rewrite reads in the wrong state (CN30). Like every `dnagent.md` SH24
round it runs under its request's `trace_id` (the stream's id when that is
empty), and so does the read it starts.

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
retried once after a release; that refusal is read from the error's type,
EBUSY, or EEXIST for an output that words it "already exists", never from
its text (`dnagent.md`, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`).
The XML goes to a file, never to stdout: the OS
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
and a forged dump forges the bitmaps `GetThinDeviceBm` and `GetLegBm`
answer, which mark regions of another pool's clone, or of a migration,
copied that never were. Nobody else can create an entry in
such a directory, and only root can create, replace or rename an entry
of its parent, the system's runtime directory, which is root's. That
parent is the runtime directory and not a world-writable temp directory
because nothing repairs an entry that fails the check: `mkdir -p` leaves
an existing directory's owner and mode as they are. A local user who
created the name first in a world-writable directory would then fail
every dump on the node until an operator removed it — the bitmap reads
and CN14's sweeps of a pool's thin ids. In the runtime directory only
root can have made such an entry. That directory is a tmpfs, so until its `rm` the file holds as much
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
optimization, never a correctness input (`architecture.md`, Clones).
CN14's sweeps of a pool's thin ids — the activation sweep and the sweep
after a failed delete — share this reservation machinery and its
`CmdSoftTimeout` bound: metadata too large to dump inside the bound fails
a sweep the same way it fails a bitmap read; an armed activation sweep
retries on later converges, and the sweep after a failed delete does not
(CN14).

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
it, so the leg identity beyond its group never enters the math; a
repair's new leg is answered with its group's bitmap all the same, which
costs copies at most). Windowed
from `start_block` for `block_cnt` blocks over the group's
`data_blocks`, a zero `block_cnt` ⇒ to the end. Same wire encoding as
CN26 (LSB-first, zero-padded); the CN22 clone-chunk fold uses it too.
The span is summed over the effective `data_grp_list` (CN9), and a leg
whose group is not in that effective prefix fails the RPC with an
Internal status naming it — the provisioning leg's own group, and
equally a group after a deferred one, since deferral is a prefix cut
(CN9): neither is a concat target, so neither has a span. Answering
anyway would report one group's bitmap under another group's offsets and
return silently wrong data. A leg of a clone meta group (CN12) fails the
RPC with an Internal status naming its clone, before any snapshot is
taken: its group backs no pool, so no thin mapping says which of its
blocks are written, and the gateway refuses that read before it asks
(`architecture.md`, Bitmap reads), so the refusal here guards the agent's
own arithmetic, which reads the leg's slice.

### Probing and error capture — `probe.go`

CN28. Probe map (`dnagent.md` SH17 conventions plus the cn probes fixed here:
`ls` for the nvmet configfs tree — except in the
port probe the two agents share (`Nvmet.ProbePort`,
`Nvmet.ProbePortState`), which tests whether the port's own directory is
there by reading the directory's modification time in the agent's own
process (`dnagent.md` SH18), while the port build (`Nvmet.EnsurePort`),
which runs only once that probe found the port short, keeps its `ls` —
`dmsetup info` and `dmsetup table` for dm
devices — over the whole node at once in the reading below, of one
device elsewhere — with `dmsetup status` for thin pools and dm-clones
and, outside the reading, `dmsetup ls` for the sweep (CN21), CN12's sysfs
read of the array's "md" directory
for arrays — never mdadm --detail — the sysfs walk of CN10 — never
nvme list-subsys — for every outbound nvme connection, and
"/sys/block" for the device numbers below). Every row reports against
the effective desired state (CN9): a resource the provisioning gate
excluded is `RES_STATUS_PROVISIONING` with the details "provisioning",
never `RES_STATUS_ERROR`, and it never feeds `err_epoch`.

**The reading.** A cntlr's read (`dnagent.md` SH29) first takes one
reading of the node under its CN1 locks (CN23, CN24): one
`dmsetup info` listing of every dm device of the node, with each one's
number and attr flags (`Dm.ListInfo`); one `dmsetup table` listing of
every dm device's live table (`Dm.ListTables`); the sysfs walk of CN10;
and the md walk of CN12. Its rows and its verdict (CN30) take from
it which dm devices, nvme subsystems and md arrays the node holds, and
each dm device's attr flags, number and table: a row reads its dm device
there, and finds its
leg's or clone source's entry in the walk and its group's array in the
md walk; its other reads stay outside the reading (below). Reason: most
of a read's time goes to starting processes, and one listing of the
whole node costs about what one device's read does, so such a read
starts a few processes whatever the slice count, besides its status
reads and the configfs `ls` of its exports (`dnagent.md` SH18). A
converge reads each of its own dm devices on its own, because it
changes them during the pass, and takes its sysfs walk and its md walk
itself (CN10, CN12).

A partial listing still answers (`osclient.md`, RunCommand): `dmsetup`
gives one when another cntlr, or a dn agent on the same machine, removes a
device during the listing, and read as unanswered it would turn every dm
row of the read `ERROR`, on a standby too, which sets its health epoch and
takes it out of the cdc's answer to hosts (`dnv-worker.md` HL2;
`architecture.md`, [D18]). A device this cntlr
wants that a partial `dmsetup info` listing lacks is read on its own
once, at its first lookup, and that answer — the device, its absence or
the read's error — serves the rest of the read; a device a complete
listing lacks is absent, with no read; and a table the `dmsetup table`
listing lacks is read on its own once in the same way. A listing that
did not answer makes every row whose first read goes to it
`RES_STATUS_ERROR` with the listing's error, and the verdict names it
(CN30).

Outside the reading stay: the `dmsetup status` of each thin pool, and of
each dm-clone once for its rows and once more for each namespace it backs
(CN16 rule 5), because a status read can wait on IO: a thin pool's waits
behind a commit already running on that pool, and a dm-clone's makes the
kernel commit the clone's metadata (`dnagent.md` SH17). Read per device,
a read's status reads wait only on its own cntlr's pools and dm-clones; one
read over the whole node would wait behind a commit on any pool or dm-clone
of the node, another cntlr's too. Also outside stay the configfs reads of
the exports; each walk entry a lookup uses — its "subsysnqn" read again,
its namespace and its controllers (CN10); the attributes of the array the
md walk matches to a group (CN12); the numbers "/sys/block" holds
(below); and the `dmsetup info` that confirms a device gone after its own
status or table read failed, which is always the device's own fresh read,
never the reading's (CN16 rule 5; CN18 step 2).

**Device numbers.** A dm table names each device it maps by its number.
The number the agent writes into a table, or checks a live table
against, is read for an md array or an nvme multipath head from the
device's own "/sys/block/<name>/dev" — an md array's
from the array CN12's match finds — and for a dm device from
`dmsetup info`: from the reading in a cntlr's read, from one
`dmsetup info` of the device elsewhere. The sweep turns a live
table's numbers back into dm names through the dm listing it enumerates
with: `dmsetup ls` (CN21), or in a cntlr's verdict the reading's
`dmsetup info` listing (CN30). No number comes from `lsblk`: the cn
agent's one `lsblk` is CN12's check that no array already runs under a
group's name, which takes no number from its answer. Reason: a file read
in the agent's own process costs far less than a process, and a read
and a converge then read each number from one source, so the two
channels check and build over the same device.

The rows, each with its `res_name` and what its probe checks:

* `CnInfo.port_info`, named by the agent's port id as a decimal string
  (so on a node running several agents the rows differ): the configfs
  transport attributes match the transport flags; the three groups of
  `architecture.md`, [D4], are present with their fixed states. It is the
  one row `CnInfo` carries: the CN holds no clone's metadata
  (`architecture.md`, [D14]), and a clone's metadata health is its clone
  meta group's rows (`grp_id_to_md_raid`, `leg_id_to_leg`) and its
  dm-clone's metadata mode (`clone_id_to_dm_clone`).
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
  `dnagent.md` SH17), and the "ana_grpid" as CN16 wants it for the
  ns-dev's live table: inaccessible while that table maps the td's
  `CnErrorName` and the rules want a serving device, and otherwise the
  desired group, which is inaccessible while the backing chain is
  provisioning-deferred (CN9), the row itself `RES_STATUS_PROVISIONING`
  then. Both channels compare the group by this one rule from the
  ns-dev's live table: a read with the table from its reading,
  which it takes also when the backing's number cannot be read, and a
  converge after its ANA loops, with a read of the namespace and, where
  the rule depends on it, of the table, an error of the namespace
  object's own step winning. The
  converge's ANA move goes by what its ns-dev step did instead (CN16), so
  a held ns-dev whose `CnErrorName` number cannot be read goes
  inaccessible while the row wants the desired group, and the row reads
  `ERROR` on both channels (CN18). So in
  the window of a promotion that came before the sides' ANA flips the
  ns-dev row of a namespace on CN16 rule 6 reads `ERROR` — a clone
  destination's reads `OK` over its park (rule 5) — and the namespace row
  `OK`, in the inaccessible group, on both channels.
* `ns_id_to_dm_linear`, named by the `CnNsDevName`: `dmsetup table`
  matches the CN16 backing (flakey line included) — for a rule-5 ns-dev
  the one CN16 installs now, read the same way (`nsDevNow`): the td's
  `CnErrorName` while the dm-clone does not serve — its status shows
  hydration enabled and its metadata read-write only while it does (CN16
  rule 5) — or is confirmed absent, and `RES_STATUS_ERROR` with
  the read's error when that status read fails on a dm-clone that is there
  or whose absence is not confirmed; an effectively suspended ns-dev is
  expected live on the td's `CnErrorName` and reports `RES_STATUS_OK` with the details
  "parked"; a dm-suspended ns-dev is `RES_STATUS_ERROR` "unexpectedly
  suspended" whatever the plan says; a provisioning-deferred one reports
  `RES_STATUS_PROVISIONING` over its permanent dm-error.
* `td_id_to_raid0` and `td_id_to_dm_error`, named by the `CnRaid0Name`
  and the `CnErrorName`: `dmsetup table`.
* `td_id_to_thin_info` with its `slice_id_to_dm_thin`, named by the
  `CnThinDevName`: `dmsetup table` (pool and `dev_id`); a created td's
  volume a read reads absent is `RES_STATUS_MISSING` with no reason
  (CN14). The td's entry also carries its torn mark, which a read reports
  from the agent's memory as the converge does (CN14).
* `slice_id_to_dm_pool`, named by the `CnPoolFinalName`:
  `dmsetup status` with the `--noflush` flag (`dnagent.md` SH17); the
  details are the raw status line — the worker
  parses data and metadata used and total out of it for the auto-grow of
  `architecture.md`, Automatic reactions. The serving pool stays
  `RES_STATUS_OK` with that raw line even while a deferred group waits to
  be grown in (CN13): `PROVISIONING` never marks the serving pool, because
  it would switch auto-grow off.
* `slice_id_to_meta` and `slice_id_to_data`, named by the `CnPoolMetaName`
  and the `CnPoolDataName`: the multi-target `dmsetup table` matches the
  group concat, each target naming its group's device by number (CN13);
  the comparison is against the effective concat (the list's leading run
  of non-deferred groups, CN9, CN13), so a not-yet-grown concat is `OK`,
  not a mismatch. A group whose number cannot be read makes the row of
  its concat `RES_STATUS_ERROR` naming the group (CN12): when no array
  holds a member the group keeps, when two arrays do, and when the md walk
  could not read the group's own array or could not list "/sys/block",
  the last of which fails both concat rows of every slice of md groups.
  Reason: the concat rows take the array the group's row finds, so a
  concat is never built or checked over an array the md walk did not
  match to the group. A converge reads the numbers first, to build the
  concat's table, so its row reads that error whether the concat is
  there or not; a read looks the concat's own device up first, and an
  absent concat is `RES_STATUS_MISSING` there before any number is
  read. When the md walk could not read the group's
  own array or list "/sys/block", the group's own row reads
  `RES_STATUS_ERROR` too (`grp_id_to_md_raid` below), so the health a
  read's rows give does not change by the concat rows; a converge skips
  the slice's pool step for the pass, so a snapshot whose messages that
  pass sends is torn (CN13; CN14, "A torn snapshot is reported"). A deferred slice's
  rows are `RES_STATUS_PROVISIONING` — deferred meaning either of its two
  group lists is non-empty and has no effective group left (CN9), not
  that every group is deferred.
* `grp_id_to_md_raid`, named by the `CnMdDevName` node, for a clone meta
  group the `CnCloneMdDevName` node, or the `CnGrpName`, a row per group of
  every slice and per clone meta group (CN12) alike: `RedundMdRaid1`: the
  array holding a wrapper of a member the group keeps (CN12), read from
  sysfs — "array_state", and for a running array
  "degraded", "sync_action" and "sync_completed", plus each member's
  "state", "slot", "block/dev" and "block/dm/name" — never mdadm
  --detail, which opens a member and can block on a dead one for the
  length of the path's failfast (CN12). A member md has failed is the
  leg row's to report while its leg does not serve, and the verdict's to
  name once the leg can take it back, an open repair's bad leg aside
  (CN12, CN30); the group's row reads
  `OK`, degraded, either way — unless the in-sync guard holds (below),
  when the row reads `ERROR` and the verdict names no member of the
  group. A
  running array (degraded included) ⇒ `OK`
  with the "array_state" as the details, then the word degraded while "degraded"
  is non-zero, then the word of a sync that is running (recovering,
  resyncing, checking, repairing, reshaping; none while "sync_completed"
  reads none) followed by its done and total sectors in parentheses. The
  words follow mdadm's State line, and repairing is ours; the state and
  the sectors are sysfs's. A running array that holds a member the group
  does not keep (CN12), while no member that stays in `leg_list` is in
  sync on a leg whose path the read finds serving — every controller's
  "address" known, a multipath namespace and a path both live and
  optimized — ⇒ `RES_STATUS_ERROR` with the converge's own words, which
  name the member and the array: CN12's in-sync guard, which a read
  applies as a converge does, so that the two report the same row. No
  answering array
  holds a member the group keeps (none matched, or the match stopped
  between the walk and its read) and no array of the walk is unanswered, or an
  "array_state" of clear ⇒ `RES_STATUS_MISSING`; an array that is not
  running (inactive, broken, …) ⇒ `RES_STATUS_ERROR` with the state as
  the details; a foreign member of the matched array, two answering
  arrays holding members the group keeps, a "/sys/block" listing or a read of
  the matched array that did not answer, or no match — or a match that
  stopped since the walk — while another array of the walk did not
  answer (it may be the group's own; the details name it) ⇒
  `RES_STATUS_ERROR`. Beside a match, an array of the walk that did not
  answer is ignored and the row reads the match (CN12: a read of another
  sp's array that did not answer must not turn this row `ERROR`).
  `RedundNone`: `dmsetup table`. A deferred group (CN9) reports
  `RES_STATUS_PROVISIONING` and no mdadm command runs.
* `leg_id_to_leg`, named by the `CnLegName`, a row per leg and per new
  leg of a repair of every group, a clone meta group's included (CN10):
  the wrapper table plus, on the primary, the CN11 prober outcome
  (`RES_STATUS_PENDING` until its prober's first completed
  round, CN11), or, on a standby, the
  transport per desired side, from sysfs,
  plus an "ana_state" of optimized or non-optimized on single-sided legs
  — two-sided legs liveness only (CN11). A provisioning leg (a non-empty
  `side_list`, every side unprovisioned, CN9) reports
  `RES_STATUS_PROVISIONING` and is neither connected, wrapped nor probed.
  A leg whose subsystem's sweep disconnect is still in flight (CN10's
  disconnect registry) reports `RES_STATUS_ERROR` with the converge's own
  details and is not probed. On a standby, a walk or a lookup that did
  not answer (CN10) reads `RES_STATUS_ERROR` naming the read.
* `side_id_to_path`, keyed by side id and named by the side's transport
  address and service id, joined by a colon: the path rows, one per side
  of each leg whose `side_list` holds two sides — a migration's (CN10) —
  and none for a leg with one side, each saying whether this node holds a
  live controller to that side. Every cntlr reports them, primary and
  standby alike, on both channels, through one function, which reads a
  row in this order: `RES_STATUS_MISSING` "sp_level" while the level
  suppresses legs (CN19), as the leg row does; `RES_STATUS_PROVISIONING`
  "provisioning" while the side is unprovisioned, since nothing is
  exported for it (CN9); `RES_STATUS_ERROR` with the leg row's details
  while a sweep's disconnect of the leg's subsystem is in flight (CN10's
  disconnect registry); otherwise the leg's lookup in the walk:
  `RES_STATUS_OK` when a live controller of the subsystem has the side's
  address, `RES_STATUS_ERROR` when that controller is not live, both with
  the controller's "state" and "ana_state" as the details, and
  `RES_STATUS_MISSING` "no controller" when no controller has it. A walk
  or a lookup that did not answer reads `RES_STATUS_ERROR` naming the
  read, never `MISSING`, and so does a subsystem one of whose
  controllers' "address" read did not answer when no controller has the
  side's address, since that controller may be the side's (CN10). The
  ANA state does not count: while a migration's cutover is pending its
  destination exports inaccessible, and the cutover asks only for a live
  path (`dnv-worker.md` RW18); right after a connect the "ana_state" can
  read empty for a moment, the kernel's scan adding the path device after
  the connect returns. A converge's rows read the lookup its connect step
  made; for a leg whose connect step failed they look the leg up again,
  so they say what the node holds rather than the connect's error. In a
  read they come first in each leg's turn, and a standby's leg row reads
  their lookup, so a primary's read looks up only the legs with two
  sides, and a standby's read each leg once, or twice
  where that lookup did not answer. The
  worker's cutover reads the primary's row of a migration's destination
  side (`dnv-worker.md` RW18), and no health rule judges these rows
  (`dnv-worker.md` HL2). Reason for the rows' scope: the primary's leg
  row carries its prober's verdict, which goes over the serving path and
  says nothing of the destination, while a row for every side of every
  leg would grow the largest report with the pool's legs; legs with two
  sides are bounded by the migrations (`architecture.md`, Live-state
  reporting).
* `xfer_id_to_dm_linear`, `xfer_id_to_subsystem` and
  `xfer_id_to_namespace`, named by the `CnXferFinalName`, the `XferNqn`
  and the `XferNqn` with the origin namespace index: `dmsetup table` and
  configfs, per CN17, the namespace's "ana_grpid" compared with the group
  CN17 wants for the transfer device's live table — inaccessible while a
  transfer that should serve has an error table there, the desired group
  otherwise — on both channels alike: a converge writes the namespace row
  after its ANA loops from a read of the namespace and, for a transfer
  that should serve, of the transfer device's table and, when its device
  step failed, reads the subsystem as a read does; a deferred
  transfer's three rows are `RES_STATUS_PROVISIONING`.
* `clone_id_to_target`, named by the clone's `src_nqn`: the sysfs walk of
  CN10 — the subsystem whose "subsysnqn" is `src_nqn`, each controller's
  "state" and the source namespace's device — not nvme list-subsys; its
  status is CN18's shared rule's, `OK` with a live controller per
  `src_tr_conf_list` entry and the source namespace there, and
  `RES_STATUS_MISSING` with no subsystem entry or for a fault of the
  source. While a sweep's disconnect of `src_nqn` is in flight (CN10's
  disconnect registry) it is `RES_STATUS_ERROR` with the converge's own
  details, whatever the walk shows; otherwise a walk or a lookup that did
  not answer (CN10) makes it `RES_STATUS_ERROR` naming the read, never
  `MISSING`.
* `clone_id_to_dm_clone`, named by the `CnCloneFinalName`:
  `dmsetup info` — the reading's in a read — then
  `dmsetup status` of a dm-clone that is there, and after a status read
  that failed the fresh `dmsetup info` of CN16 rule 5 and CN18 step 2; its
  status is CN18's shared rule's, which reads the dm-clone's hydration
  and its metadata mode from the status line, and the clone meta group's
  row of the same read for a dm-clone that is absent. The details carry
  the raw status line whenever it was read, whatever the row's status
  (`architecture.md`, Live-state reporting — hydration progress;
  `DeleteClone`'s hydration check, which `force` skips, reads it).
* A clone reports these two rows and no other: its clone meta group
  reports in `grp_id_to_md_raid` and its legs and its repair's new legs in
  `leg_id_to_leg`, as a slice's group does (CN12), and none of its legs
  has a second side, so none has a path row.

Beside the rows, and no rows themselves, so no health rule reads them
(`dnv-worker.md` HL2), a primary reports for each group whose
`grp_id_to_md_raid` row reads `OK` — a running array of a group its plan
builds, which the in-sync guard does not hold (CN12) — what it reads of
the array, and its repair verdict on the group's repair
(`architecture.md`, Repair, gives what they are for), short of a converge
whose read of the array after its own changes fails (CN12). What it reads
of the array, in `grp_id_to_grp_info` by group id: `whole` while the
array's "degraded" reads zero, which counts a place with no member, a
member md has failed in its place and one it copies onto, and not a member
without a place; and `in_sync_leg_id_list`, the ids of the group's `leg_list` legs and of
its repair's new legs whose members md marks in sync and not faulty,
ascending, so that an array that has not changed reads the same and
leaves the check stream nothing to send (`dnagent.md` SH26). The repair
verdict, in `rep_id_to_repair_info` by `rep_id`: `started`, for an open
repair, when every new leg holds a place — a "slot" number and not faulty
— and md holds nothing on a bad leg; `abort_confirmed`, for a CREATED
repair whose abort flag is in the cntlr's own request, when md holds
nothing on a new leg. An aborted repair has neither. Any other group gives
neither: an absent, stopped or broken array, the guard's `ERROR`, a
deferred group, a `RedundNone` group, a level that builds no group and a
standby. A read and a converge take both from the array their group row
reads, a converge from the array as the pass leaves it (CN12), through one
function, so the two channels report the same fields for the same array.

CN29. Error capture (`architecture.md`, Common agent rules): a failed
command marks that resource `RES_STATUS_ERROR` with the command output
in its details and the converge pass continues with the remaining
resources. Two things qualify that. The details are for people: where
the worker must tell one failure from another, the row says so in its
reason (`architecture.md`, Live-state reporting), set from the command
wrapper's typed error, never from the text (`dnagent.md`, OS wrappers —
`dm.go`, `nvmet.go`, `nvmehost.go`); the one reason the cn agent sets is
CN14's lost thin id. And a clone's two rows take their status from
CN18's shared rule, from the live state, on a converge and a read
alike, a failed command's output joining their details. Probes create,
change and remove nothing — the
metadata-snapshot reserve runs only inside `dumpThinMetadata` (the CN25
bitmap reads and CN14's sweeps of a pool's thin ids — the activation
sweep and the sweep after a failed delete), never from `probe.go` —
though a status read of a dm-clone makes the kernel commit the clone's
metadata, where a thin pool's commits nothing (`dnagent.md` SH17).
`RES_STATUS_PROVISIONING` is never produced by this path: it is assigned
by the CN9 gate, not by a failed command; and
`RES_STATUS_PENDING` is produced by the CN11 registry alone, never by a
failed command. The group row's probe opens no md member device: it is
CN12's sysfs read, so a dead member of an array that keeps another
in-sync member does not turn the row `ERROR` — it reads `OK`, with
degraded once md has failed the member (an IO to it that errors does,
and so does CN12's reconciliation failing it out as an extra or as a
repair's bad leg; until then
the dead member does not count in "degraded"), and the fault is its leg
row's, from the CN11 prober's own IO, until the leg can take the member
back, when the verdict names it (CN30) and the converge puts it back
(CN12), unless it is an open repair's bad leg, which stays out while its
repair is open. While CN12's in-sync guard holds over the array, the row reads
`ERROR` instead, and the member is neither named nor put back (CN28,
CN30). md does not fail the last in-sync
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
reporting; CN20, CN30). A piece of the CN's base state — the port or one
of its ANA groups — that a read of the CN reads absent, and that a
`SyncupCn` would build, travels in both: the port row reads `ERROR`, and
the verdict names it too (CN30), because the worker re-syncs on a reply's
code and never on its rows. So does an ANA group that the read reads in a
state other than its fixed one while the port's transport attributes
match, which a `SyncupCn` would rewrite: the port row reads `ERROR` with
the probe's words, and the verdict names it. And so does an md member
that a cntlr's converge would put back or add (CN12) on a leg that can
take it, or remove for a repair: its group's row reads `OK`, and a
read's verdict names the member (CN30), unless CN12's in-sync guard
holds over its array, whose row then reads `ERROR` and whose members the
verdict does not name.

### The read-only verdict

CN30. `CheckCn`, `CheckCntlr`, `GetCnInfo` and `GetCntlrInfo` reply the
code of the object's latest observation (`dnagent.md` SH28), which is the
verdict of what made it. A read's (`dnagent.md` SH29) is the read-only
verdict of its scope: `ReplyCodeLeftover` iff that verdict is not clean.
A converge's — a `Syncup*`'s, the startup reconcile's, a CN10 or CN18
retry attempt's — is the code its own reply carries, or would carry where
no reply is sent: that of its own sweep, which removes (CN20, CN21). The
read-only verdict is the CN21 sweep with the removals left out: the same
attribution, the same comparison against the same wanted set, and nothing
removed or built (CN23). Details and log record are CN20's. A CN's read
lists the node with the node-level sweep's enumeration (CN21). A cntlr's
read takes its verdict's dm, md and nvme listings from its reading
(CN28): the dm devices
and their numbers from its `dmsetup info` listing, a `dmsetup table`
listing that did not answer failing the verdict as well; the arrays from
its md walk, under `ListArrays`' strict rule (CN12); and the connections
from its sysfs walk, failing on any entry whose "subsysnqn" read did not
answer. It lists the nvmet tree in configfs itself, and reads the
namespace node and the number of each clone-source connection it checks
against a live dm-clone (CN21). A partial dm listing counts as an answer
here, where the sweep's `dmsetup ls` keeps its strict rule (`osclient.md`,
RunCommand). Reason: only this cntlr's own converges, the node's
`SyncupCn` and the startup reconcile create or remove this cntlr's
devices, and none of them runs during the read, which holds this
cntlr's object lock and the node read lock (CN1); so a device a partial
listing lacks is another cntlr's or a co-hosted dn agent's, which this
verdict never names. A listing partial for another cause can hide a
leftover of this cntlr from one read.

The node-level verdict of a CN's read also names what of the CN's base
state the read's own probe read wrong in a way a `SyncupCn` would cure
(CN5): a missing port directory or ANA group, and an ANA group whose
"ana_state" the probe read as other than the group's fixed one on a port
whose transport attributes all match: nvmet takes an "ana_state" write
whatever is linked to the port, and `EnsurePort` rewrites a state that
differs. Each is rendered the way CN20 renders an enumeration that did
not answer — "enumeration failed" naming the piece as absent with the
probe's own words after it, or a group's state as differing from the
wanted one — and the log record lists it among its failures. Nothing
else re-drives them — the worker re-syncs on a reply's code, never on a
row — and a read cannot build or rewrite them itself (CN23). A probe that
did not answer names nothing, and the next read asks again. Nor does a
port transport attribute that differs, which its row alone reports:
nvmet refuses every transport-attribute write while a subsystem is linked
to the port (EACCES), so while one is every re-send would fail on that
attribute the same way, and `EnsurePort` stops at it, before the groups.
The probe, which reads the attributes first, does not read the groups of
such a port either, so a group state that differs beside it is not named.
The port is read with strict reads for this
(`agent.Nvmet.ProbePortState`), so a group or an attribute whose read did
not answer is an `ERROR` naming the read, not a missing group or a
mismatch. The `SyncupCn` reply does not carry any of them: its converge
has just tried to build the base state, and its rows say how that went.
Nor does the observation that converge stores, or the startup
reconcile's, so the round after it answers without them, and the read
that round starts names them again. So a node whose base state can never
be built — a port `mkdir` that is refused every time, say — is re-sent
its `SyncupCn` on each round that answers from a read, and each attempt
is logged, as a disk node whose disk identity stays unconfirmed is
(`dnagent.md` DN16).

A cntlr's read-only verdict also names each md member that its
converge's member reconciliation would put
back or add (CN12): a `leg_list` member of a wanted group's running
array that md has failed and moved out of its slot, or that the array
lacks, on a leg whose leg row reads `OK` (its wrapper there and its last
CN11 probe passed) and whose subsystem has a multipath namespace, every
controller's "address" known and a path both live and optimized. It
reads them from the arrays and the leg rows of the read's own probe and,
for each such member whose leg row reads `OK`, from one lookup of the
leg's subsystem in the read's walk. An open repair's bad leg is not one
of them (CN12). Beside them it names a repair's members the converge
would change (CN12): a new leg it would add, under the add's conditions
read with the parts of each leg's availability a read can read — the new
leg's own leg row `OK` among them — after the put-back of a new leg md
has failed; a new leg it would remove for an abort; and a bad leg it would
remove once its repair has started in md (CN12). It names no new leg whose
add waits, for its bad leg or for a member that keeps the data, since the
converge it would bring waits again. It lists each among the leftovers
under a kind of its own (`agent.LeftoverKindMdMember`) by the dm name of
the leg's wrapper; the log record lists it there too. Reason: a converge
must first see the failed member, and neither a drop nor a repair's step
that failed changes the desired state, so no `SyncupCntlr` follows it;
the late-member retry counts availability alone and never a repair's new
leg, nor an open repair's bad leg while its group's array runs (CN12),
and a retrier of the agent's own beside the verdict would be invisible to
the control plane (below). The
verdict tests the parts of the leg's availability a read can read without
building anything, so no syncup comes to put back or add a member on a
leg whose path is not live and optimized, none in a migration gap, and
one drop costs one `SyncupCntlr`. A repair's removals are named whatever
the leg's path, since md removes a member that holds no place without
any IO to it. A step only the converge takes can still fail, the
connect of a provisioned side that has no controller among them: the leg
is then not available, the member stays out, and the verdict names it
again on every read (Known limits). The `SyncupCntlr` reply does not name
them: its converge has just tried to put them back, and its group row
says how that went. Nor does the observation a converge of the cntlr
stores, so a member still out shows again on the next read. A member md
keeps failing while its leg's probe passes is named again after each
failure (Known limits). It names no member of a group whose row CN12's
in-sync guard turns `ERROR` (CN28). Reason: the converge would leave
that array as it is, so a name would bring a `SyncupCntlr` on every
round for a converge that refuses again; the first read that finds the
guard let go names the group's members again.

No verdict is carried over: each read and each converge that takes one
computes its own, which is kept only as the code of the object's
observation, until the next read or converge stores another; nothing
else remembers it and no pass reads it. That is the whole retry mechanism for a leftover and it is
deliberately the only one: a leftover that has since gone stops being
reported once a later read or converge stores its absence; one that is
still there is named again by the next read, whose observation's code is
then non-zero, and by each converge whose own removing sweep names it;
and on each round that answers from an observation that names it and
is not too old (`dnv-worker.md` HL7), the worker's "re-sync while the code is non-zero" rule (`dnv-worker.md` RW4)
re-issues the `Syncup*` that sweeps again. A converge's observation
carries only what its own removing sweep names, so what only a read's
verdict names — a connection whose subsystem directory is left
with no controller (CN21), an md member to put back and a piece of the
base state (both above) — brings by itself the `Syncup*` on every second
round at most (`dnv-worker.md` RW4 step 5, and RW5 for the subsystem
directory). No agent-side loop exists for
leftovers, because a second retrier for something the worker already
re-drives would be invisible to the control plane. CN10's connect retry
sweeps only as part of a converge it re-runs for a reason of its own,
and a sweep's `nvme disconnect`
completes off the pass (CN10's disconnect registry), where the next read
or pass finds it gone; one that failed is issued again by the next pass
that still finds the controller. A restart is covered by the same path:
whatever the startup reconcile could not remove is named by the
observation it stores, from which the `Check*` rounds of the object that
owns it answer until a read or converge stores another (CN2). On the `Get*Info` side the code is carried but
read by nobody — the gateway's inspects pass those
replies through and by `gateway.md` AG3 do not apply the `AgentReply`
convention to them — so the operator-visible form of a leftover is the
worker's log and the `Syncup*` reply's details.

**Each scope answers for its own leftovers.** A cn's verdict is the
node-level one — the sps whose pointer has left its list, plus the
unowned objects and the base state above that a `SyncupCn` would build
or rewrite — and a cntlr's is that cntlr's sp alone, apart from the
sp-less clone-source connections below. Neither reports the other's sp
chain, because each drives its own `Syncup*`: reporting a cntlr's chain
leftover on `CheckCn` would re-issue a `SyncupCn` that builds no chain
for that sp at all (CN21: an sp in the pointer list gets none). One kind
of object both verdicts can name: an unowned clone-source connection,
which by the attribution of CN21 is a connection to one of our cluster's
transfers. It belongs to no sp at all, so both scopes enumerate it
node-wide (CN21), the cntlr-level L5 set being the same set the
node-level pass uses: our cluster's transfer connections that no stored
cntlr names as a clone's `src_nqn` and no live dm-clone table maps. Such
an NQN is therefore reported by `CheckCn` and by every
`CheckCntlr` of this node until it goes, and re-driven by both
`SyncupCn` and each cntlr's `SyncupCntlr` — one round of work more than
the minimum, and never a round less, which is the safe direction for a
verdict whose only job is to make somebody try again. A clone source
under any other NQN is in neither set, so neither verdict names it
(Known limits). A cntlr whose stored
conf the gate refuses has no verdict either — it converged nothing and
enumerated nothing, and naming objects this agent is not allowed to remove
would be a leftover report no `Syncup*` could ever clear.

## Known limits

* A member md keeps failing while its leg's probe passes is put back
  again after every failure: md fails a member with the failfast flag on
  any read error from it (CN12), so one bad sector outside the health
  block is enough, while the CN11 probe, which reads the health block
  alone, passes, and the leg can take the member back, unless CN12's
  in-sync guard holds over its array. Each failure then
  costs one `SyncupCntlr` and one converge of the cntlr, which holds the
  cntlr's reads off while it runs (CN1), and a resync of what md's bitmap
  marks. Nothing limits how often, since a limit would need the agent to
  remember past attempts (`architecture.md`, Teardown by sweep); it shows
  as the agent's leftover record naming the same member after each
  failure, each followed by the worker's re-sent `SyncupCntlr` (`log.md`,
  Leftovers). The leg repair does not act on it, since the leg's row
  reads `OK` (`dnv-worker.md` AR8), nor can an operator's repair, which
  refuses a bad leg with no health epoch and points the operator to a
  migration of the leg's side instead (`architecture.md`, Repairs).
* A member can be named on every read on a leg whose converge fails:
  the verdict tests only the parts of the leg's availability a read can
  read (CN30). A leg with two sides, a migration in flight, whose path
  through one side reads live and optimized while the other provisioned
  side has no controller and refuses a new connect — its controller gone
  after a reconnect refused with DNR, say — has its failed member named
  by every read. Each round that answers from such a read then costs one
  `SyncupCntlr` and one converge, which fails the leg at the connect and
  puts nothing back, beside the CN10 retry that failed connect registers,
  until the side connects or leaves the leg. A verdict that also asked
  for a controller on every provisioned side would not name the member
  when that connect succeeds, and that converge is the one that puts the
  member back. CN12's in-sync guard meets the same gap: when such a leg
  carries the only member that stays in sync, the converge reads the leg
  not available and refuses to fail the member the group does not keep,
  while a read finds the leg serving, reads the group's row `OK` and names
  a `leg_list` member the array lacks whose leg can take it, so each round
  that answers from such a read brings a converge that refuses again.
* A killed `--remove` leaves a repair's add half done: an add that runs
  within seconds of the bad leg's side dying can have its `--remove` of
  the bad leg killed at the soft timeout while a superblock write is
  stuck on that member (CN12). The pass reports the group `ERROR` and adds
  no new leg, and the array runs on its other member until the next
  converge: the one the `SyncupCntlr` brings that the worker re-sends once
  a read's verdict names the new leg (CN30). A new leg is never late
  (CN12), so one that is available while its leg row does not read `OK` —
  its probe fails, or has not yet completed — brings neither the verdict's
  name nor the CN10 retry, and the add waits for the next converge that
  comes: the one the verdict brings once the new leg's row reads `OK`, or
  one for some other reason, such as a revision bump.
* An in-sync guard that holds can move the role: while CN12's in-sync
  guard holds, the group's row reads `ERROR`, on a read as on a converge
  (CN28), which sets the primary's health epoch (`dnv-worker.md` HL2),
  so a guard that holds past the primary-unhealthy threshold lets the
  failover reaction fail the primary over (`dnv-worker.md` AR5). The new
  primary assembles the group from its `leg_list` legs and an open
  repair's new legs (CN12), which leave out the member the guard kept.
* An unanswered array can leave a group unassembled: a group with no
  answering array while another array of the walk did not answer is
  neither created nor assembled that pass (CN12), and the error is a
  row, which nothing re-drives by itself; the cntlr's verdict stays
  non-zero only while its md enumeration still fails (CN30).
  Once the array answers, the group waits, its row `MISSING`, for the
  cntlr's next converge for some other reason — and the groups this can
  hit include those a new primary's first converge must assemble, whose
  slice layers cannot be built meanwhile. The late-member retry of CN12
  does not reliably cover it: it runs only while some `leg_list` member
  of the cntlr is not available, and the group's own legs, like every
  other leg, may all be available. The retry a waiting namespace or
  transfer registers (CN16, CN17) covers a group that a td's raid0 not
  built yet needs, while a namespace or a transfer of that td the desired
  state wants optimized waits for it, as at a new primary's first
  converge, and the retry a clone registers while its clone meta group is
  not ready and no dm-clone of it serves covers that group (CN18 step 2).
  It does not cover a group the
  serving stack does not need, such as a grow's new group under a pool
  that serves (CN13), nor a group at the level that removes the thin
  pools, whose namespaces sit on the dm-error on purpose (CN16 rule 3).
* An assembly that needs a repair's new leg registers no retry of its
  own: a repair's new leg is never late (CN12), yet an assembly takes one
  once it is available, and mdadm refuses to start the array from a lone
  leg whose superblock counts another member as working
  (`architecture.md`, "Make sure all groups are available"). So when a
  promotion's first converge reads a STARTED repair's new leg that md
  holds in sync before its side's ANA flip has reached this CN, while
  every `leg_list` leg of the group is available, the bad leg among them,
  the group stays unassembled until a converge for another reason; a
  `leg_list` leg that is not available is late and registers the retry,
  the bad leg too while the array does not run (CN12). The retries that
  cover an unanswered array (above) cover it too, and leave out the same
  groups.
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
  `CnConnectRetryInterval`, which holds the cntlr's reads off while it
  runs: the late-member retry (CN10, CN12) runs the whole converge of the
  cntlr until every `leg_list` member is available, an open repair's bad
  leg aside while its group's array runs. On members that are
  genuinely dead that lasts until the worker's leg repair
  (`dnv-worker.md` AR8) has created a repair for the last of them, whose
  bad leg is then no late member while the group's array runs (CN12),
  since the retry is cntlr-wide and the leg repair takes one step per
  pass of the SP (`dnv-worker.md` AR2): for a dead DN, the side-unhealthy
  threshold, then, for each of the SP's `leg_list` legs on that DN, a
  repair's create, at least one pass each. It has no bound when the leg
  repair creates no repair for a late member: a `RedundNone` leg (a
  repair needs an md-raid1 group, `architecture.md`, Repairs), a leg with
  two sides (a migration in flight, which a repair refuses as its bad
  leg; it has no available path once the destination's DN is dead, or the
  source's before the destination's path is optimized,
  `architecture.md`, Migration), a leg whose group, a slice's group or a
  clone meta group alike, has no other leg of its `leg_list` free of a
  health epoch and held in sync in the primary's latest report (the
  healthy-leg test, `dnv-worker.md` AR8), a leg of a group whose row in
  the primary's latest report does not read `OK` — its array stopped or
  broken, or held by the in-sync guard (CN12) — since the leg repair
  creates no repair for such a group (`dnv-worker.md` AR8), a leg of a
  group that has a repair, an open repair's bad leg among them while the
  group's array does not run, since a group has at most one repair
  (`architecture.md`, Repairs), an SP whose reactions are suppressed
  (`dnv-worker.md` AR3), or no DN to place the new leg on. The retry then
  runs until the DN returns or an operator acts. Each attempt
  is a full converge of a built cntlr — probes, the sweep's enumerations,
  no creates — the profile the connect retry already has while a side's
  connect fails (a leg's controller is connected with no controller-loss
  timeout, `dnagent.md` SH20, so a side that dies while its controller survives is
  late rather than a failed connect; once the controller is gone — its
  reconnect refused with DNR, as when the side's port link is gone while
  the port still listens, or lost to a CN reboot — the next converge's
  `nvme connect` fails while the side stays down, and registers the same
  retry). The cost is not CPU alone: an attempt marks the cntlr's
  converge before it waits for the object lock and holds that lock for
  its whole converge, so no read of the cntlr runs meanwhile (CN1;
  `dnagent.md` SH30), and the cntlr's observation ages until the attempt
  stores its own (`dnagent.md` SH28). A round still answers at once, but
  one whose observation is older than the worker's bound is a missed
  round (`dnv-worker.md` HL7), which stamps `Cntlr.err_epoch`
  (`dnv-worker.md` HL2), and two such rounds in a row, once that epoch has
  aged the primary-unhealthy threshold, let the failover reaction
  (`dnv-worker.md` AR5, AR10) fail over a primary whose only fault is a
  dead leg — and the new
  primary inherits the same late member, and with it the same retry. The
  connect retry holds the reads off too, but only while a side's connect
  fails — and with the CN10 pass budget such an attempt also spends about
  `CnConnectPassBudget` more on in-pass retries and head waits, more when
  a retried connect itself runs long. How long an attempt runs grows with
  the slice count: an attempt that outlasts `CnConnectRetryInterval` finds
  the loop's next tick already due and is followed by the next at once,
  so a round seldom finds the gap between two attempts to start a read
  in, and each round answers from the observation the last attempt
  stored, which becomes too old once one attempt runs longer than the bound.
  The guard against this failover, not an optimization, would be a
  pre-check that runs the converge, and takes the object lock for it,
  only when the converge has something to act on — a late member
  available again, a provisioned side with no controller, CN18's clone
  triggers — read from the desired state and sysfs with nothing
  remembered. It is not done, so that the cn retry loop stays the
  `dnagent.md` DN13 pattern (the comment on `connectRetryLoop` says why
  the two copies must not drift apart).
* A promotion that outruns the sides' ANA flips pauses its hosts until the
  retry builds the stack: the promotion's own converge reads its legs
  before the sides' ANA flips have reached this CN's sysfs (CN12), so its
  late members keep the stack from being built: no pool is built over an
  md group with no available leg, or one md refuses to start from the
  legs that are, nor over a `RedundNone` group whose leg is late (CN12),
  and the ns-dev cannot be put on the raid0 (CN16 rule 6); it stays on the td's
  `CnErrorName`, the standby table of rule 2. Its namespaces stay
  inaccessible and a host's IO waits, the IO it queued while no path
  served included (CN16), and a transfer of the pool waits the same way
  (CN17). The ns-dev row reads `ERROR`, which sets the health epoch, and
  the namespace row `OK` (CN28). The first CN10 retry attempt after those
  flips have reached this CN builds the stack, puts the ns-devs on the
  raid0 and moves the namespaces to optimized (`architecture.md`,
  Failover). So the pause runs past the demotion hold by the sides-first
  hold's wait (`dnv-worker.md` RW14), the promotion's own converge, one
  `CnConnectRetryInterval` and the converge of the attempt after it, more
  when the ANA flips reach this CN only after that attempt has begun; each
  converge grows with the slice count.
* A namespace that waits keeps the CN10 retry running for as long as its
  cause lasts (CN16), so a cause that never goes away — a thin pool that
  cannot be built, an assembly md refuses with every leg available, a
  lost thin id, a dm-clone whose metadata is read-only or failed — re-runs
  the whole converge every `CnConnectRetryInterval`
  until an operator acts, at the cost the late-member retry above states:
  each attempt holds the cntlr's reads off for a whole converge, and the
  cntlr's observation can become too old meanwhile. The retry
  holds no reaction back. A thin pool that cannot be built, a lost thin id
  and an assembly that md refuses for a slice's group leave the td's raid0
  unbuilt, whose row reads `ERROR`, as the ns-dev row of a namespace on
  CN16 rule 6 does too; a dm-clone whose metadata is read-only or failed
  reads `ERROR` on its own `clone_id_to_dm_clone` row (CN18). Each of those
  rows sets the health epoch whether or not the retry runs (`dnv-worker.md`
  HL2, AR5). An assembly that md refuses for a clone meta group reads `ERROR` on
  the group's own row in the reply of every converge (CN12); a background
  read in between (`dnagent.md` SH29) reads that row `ERROR` too when md
  left the array inactive, but `MISSING` when md left no array (CN28), and
  such a reply, when no other row reads `ERROR`, clears the epoch until the
  next converge's reply sets it again (`dnv-worker.md` HL3). The hosts wait
  with no time limit until the cause goes or an operator ends the wait by
  disabling the cntlr, by a level step or by a delete.
* A clone build that keeps failing reads the same on every report, and
  only some ways of failing set a health epoch: CN16 keeps a clone's td
  parked, and its namespaces inaccessible, while its dm-clone does not
  serve (rule 5). A CN18 step that stopped the build before the dm-clone
  serves registers the CN10 retry, which re-runs the converge every
  `CnConnectRetryInterval` until the build finishes. What keeps failing
  may leave the dm-clone up with hydration off — a refused enable, a
  `dmsetup create` killed after its ioctl ran — and its row then reads
  `RES_STATUS_ERROR` "hydration not enabled" on every converge and every
  read (CN18's shared rule), so the primary's health epoch stays set —
  what that costs in failovers is `dnv-worker.md`, Known limits — while a
  host waits on the namespace, which stays inaccessible for as long as
  the build keeps failing (CN16). With the dm-clone gone between attempts
  (a refused create), `clone_id_to_dm_clone` reads `RES_STATUS_MISSING`,
  which sets none, and `ns_id_to_dm_linear` reads `OK` over the park,
  because a dm-clone confirmed absent does not serve (CN16 rule 5,
  `nsDevNow`), so a build that keeps failing that way sets no health
  epoch while a host waits on the namespace; a status read that keeps not
  answering leaves its rows `RES_STATUS_ERROR`; and a source fault reads
  `RES_STATUS_MISSING` in `clone_id_to_target`, which sets no health epoch
  (the next item). Nothing but the retry re-runs the build unless the
  cntlr is converged for another reason (a revision bump, a non-zero reply
  code, an agent restart).
* A dm-clone whose metadata is read-only or failed stays so. dm-clone
  leaves read-write mode when an operation on its metadata device — the
  clone meta group — fails, and nothing the agent does takes it back: the
  build leaves it as it is (CN18 step 2), its row reads `RES_STATUS_ERROR`
  naming the mode, which sets the primary's health epoch, and CN16 parks
  its namespaces, whose hosts wait, as over a slice whose array is broken.
  A new primary that assembles the group creates a dm-clone of its own
  over the metadata the group holds (CN18 step 3), so the role's move at
  the primary's threshold is what brings the clone back once its group
  can be assembled; one whose group cannot be assembled anywhere reads
  `MISSING` and waits at least until a leg of its `leg_list` returns,
  since a repair adds its new leg only to a running array (CN12) and leg
  repair creates no repair for a group whose array does not run
  (`dnv-worker.md` AR8). A dm-clone whose copy
  is whole writes no metadata, so a group whose legs all fail under it
  leaves it in read-write mode and serving; the fault is then the group's
  and the legs' rows' alone.
* A fault of a clone's source does not itself move the role: a source
  with no live controller at one of its endpoints, or with no namespace,
  reads `clone_id_to_target` `RES_STATUS_MISSING` on every converge and
  every read alike (CN18's shared rule), as a source with no
  subsystem entry does, and a missing row sets no health epoch, so the row
  neither fails the primary over nor replaces it. The copy waits until the
  source answers, and a host's read of a block not yet copied waits for
  the source or fails. A fault in the primary's own path to the source
  reads the same, though a new primary would not meet it: the operator can
  move the role by disabling the primary cntlr (`dnv-worker.md` AR5), and
  the new primary creates the dm-clone over the clone meta group's
  metadata (CN18 step 3). A
  fault that fails step 1 — no source namespace, or an endpoint with no
  controller at all that step 1 then fails to connect — keeps the CN10
  retry running for as long as it lasts (CN18), with the profile of the
  late-member retry above: its attempts hold the cntlr's reads off, so
  its observation can become too old, which counts as a missed round, and
  missed rounds can fail the primary over. A source whose every
  endpoint has a controller, live or not, and whose namespace is there
  passes step 1, so its fault registers no retry. A clone that has
  hydrated in full serves on through its dm-clone, its namespace
  optimized, since serving follows the dm-clone's own status and not the
  rows (CN16 rule 5); that rests on a dm-clone keeping hydration enabled
  once it has hydrated in full, and one that turned it off would read
  `RES_STATUS_ERROR` on every finished clone and park its namespaces. A
  dm-clone up with hydration off, by contrast, reads `RES_STATUS_ERROR`
  (above) and moves the role at the primary's threshold, and the new
  primary creates the dm-clone over the clone meta group's metadata and
  finishes the build (CN18). The clone goes out of
  service, even one that has hydrated in full, when its dm-clone must be
  built again while a fault at the source lasts and that build fails
  step 1 (CN18). A role move, a CN reboot, which takes the dm-clone and
  the source's controllers together, and a level that suppresses the
  clone, raised and lowered again (CN19), each need that build; it fails
  on a CN with no controller at an endpoint that it then cannot connect,
  and on any CN while the source has no namespace. CN16 rule 5 then parks
  every ns-dev of the destination td on its `CnErrorName`, and a host
  waits on the namespace, which stays inaccessible, until the source
  comes back or the clone is deleted with `force`. Without `force`
  the delete is refused: its hydration check reads the primary's
  `clone_id_to_dm_clone`, which carries no status line (`architecture.md`,
  Clones).
* A controller delete whose target vanishes mid-delete waits out the
  kernel's admin timeout: when a disk node removes a side's export while
  this CN's `nvme disconnect` of the leg is mid-delete — the two ends of
  a leg removal, or of a pool drain, are separate RPCs to separate agents
  — the kernel cannot enter error recovery on the DELETING controller, so
  its shutdown command waits out the admin timeout, and the disconnect
  with it, in an uninterruptible write that no signal of `dnagent.md` SH15 ends. The
  sweep issues the disconnect off its locks (CN10's disconnect registry),
  so the stall holds none of the CN1 locks and does not hold the worker's
  `SyncupCntlr` past its deadline; the connection reads as a leftover
  until the kernel lets go, and the child keeps one of the node's
  `DefaultOsClientLimit` `OsClient` slots for the whole wait
  (`osclient.md`, RunCommand). The leftover still costs reads: the
  controller being deleted stays listed with its state, so each later
  converge whose removing sweep covers the connection names it again
  (CN10), the observation that converge stores carries the code, and the
  worker re-sends the `Syncup*` on every round that answers from it
  (`dnv-worker.md` RW4); each re-send cancels the read its round has just
  started, if that read still runs (`dnagent.md` SH30). So a cntlr whose
  own converge names the connection has its observation renewed by those
  converges, and by a read only when the read ends before the re-send
  arrives. The pool drain keeps its cross-role race —
  it deletes an sp's cntlrs and then its slices and waits on no agent
  (`dnv-worker.md` SPD13) — so a `SyncupCn` that drops the cntlr from its
  pointer list sets the leg disconnects going in the background and is
  answered at once with `ReplyCodeLeftover` while the disk nodes take the
  exports away; a stalled disconnect then runs the admin timeout out, and
  one still in flight when the agent exits is not waited for
  (`dnagent.md` SH27), so only the leftover every later reply named
  records it. Each `SyncupCn` re-sent for that leftover keeps no cntlr's
  read from ending (`dnagent.md` SH30). A drain that
  catches many deletes in the kernel at once holds at most `disconnectConcurrency` of those slots (CN10), and the
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
* A clone source that is not a transfer of this cluster stays connected
  after its clone has gone: the sweep judges a clone-source connection only
  when its NQN is a transfer NQN of this cluster (CN21), so a connection to
  another pool's subsystem, or to a transfer of another cluster, stays on
  every CN whose build of the clone connected it — a controller per source
  endpoint and the source's namespace head — until an operator disconnects
  it or the node reboots. That includes a primary whose build stopped
  before the dm-clone, since CN18 connects the source in step 1, ahead of
  the clone meta group's check and the dm-clone. A clone created after it
  on the same source adopts it (CN18
  step 1). Like every connection the agent makes it has no controller-loss
  timeout (`dnagent.md` SH20), so once the source's export goes its
  controller can go on reconnecting for as long as it stays.
* The sweep's check that a removed connection is gone walks
  "/sys/class/nvme-subsystem" once per connection: the probe behind CN10's
  disconnect registry — for L10's legs, L5's clone sources and the
  node-level pass's unowned sources — lists it and reads entries'
  "subsysnqn" for each connection it checks (`ListSubsys`). So a pass that
  removes many connections — a pool's teardown, a level that takes the
  legs away (CN19), the node-level sweep under the node write lock — makes
  a number of reads that grows with the square of their count, and so
  does a converge that connects many legs, such as a standby's first
  build, which walks again after each connect step (CN10). Reason: the
  check is each removal's own fresh probe (CN21) under `ListSubsys`'s rule
  (`dnagent.md` SH20); the reads are listings and file reads in the
  agent's own process, which start no process, and only a pass that
  removes or connects many connections makes them.
* A thin `delete` message that fails after its volume's removal is
  re-driven only once, by the sweep of that pool's ids its own pass runs
  under an RPC-delivered request (CN14), and nothing of it is remembered.
  When that sweep fails too, or the pass's request did not come by the
  RPC, no pass aims at the id again: only the next sweep of that pool's
  ids that gets through deletes it — the sweep after a later failed
  delete in that pool, or an activation sweep, which the pool device's
  creation alone arms — and until then its blocks stay allocated, for the
  life of the pool device on this primary if no such sweep comes.
* A base state that cannot be built is re-sent on each round that
  answers from a read: a read of the CN that reads absent a piece of its
  base state that a `SyncupCn` would build, or an ANA group in a state
  other than its fixed one on a port whose transport attributes match,
  names it in its verdict (CN30), so while a port `mkdir` or
  "ana_state" write keeps being refused, the worker re-sends the
  `SyncupCn` on each such round and each attempt is logged — the agent's
  leftover record of each read names what the read read, and each refused
  command or write is logged as it fails — as for a disk node whose disk
  identity stays unconfirmed. The converge's own observation does not
  name the piece (CN30), so the round after each re-send answers without
  it and starts the read that names it again: the piece brings a re-send
  at most every second round (`dnv-worker.md` RW4). The verdict does not
  re-drive a port transport attribute that differs (CN30): its row reads
  `ERROR`, and while a subsystem is linked to the port it waits for an
  operator, since nvmet refuses every transport-attribute write then; one
  that differs while none is linked, which a re-sent `SyncupCn` would
  rewrite, is not re-driven either and waits for the CN's next
  `SyncupCn` for another reason or the agent's next start.
  Each re-sent `SyncupCn` holds the node write lock for its
  converge and its removing node-level sweep (CN1), so a cntlr converge or
  read that comes after it waits it out, once for each re-send; it keeps
  no cntlr's read from ending (`dnagent.md` SH30).
* A torn snapshot's mark is best effort. CN14 keeps it in memory from the
  pre-pass run that saw the tear until a request carries the td `created`,
  and the created flip records it from a reply that completes the td
  (`dnv-worker.md` RW19). An agent restart in between can lose it, and so
  can a failover or a cntlr replacement that moves the primary role off
  the cntlr holding it (`dnv-worker.md` AR5, AR7): a primary whose run
  finds every slice's id made already sees no tear, and a demoted primary
  reports no thin rows. The snapshot then flips with `torn` false
  (`architecture.md`, Known limits).
* A cntlr's save that fails leaves the request before it on disk, the one
  way a stale request stays there: the save comes before the converge, so a
  crash or a cancelled call inside a pass leaves none (`dnagent.md` SH5).
  The converge runs on after a failed save, so an agent restart before the
  cntlr's next accepted save converges that older request. Its sweep
  removes the thin volume of a td that request does not list and, while the
  pool is wanted, sends the delete message of the td's thin id (CN21): a
  thin volume the newer request created is lost. CN14's guard on its two
  thin-id sweeps does not cover that sweep. Nothing tries the save again
  before the cntlr's next accepted `SyncupCntlr`, and its Check rounds
  answer the revision the process holds, so only a round's non-zero reply
  code or a newer desired revision brings one (`dnv-worker.md` RW4).
