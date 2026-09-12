# cnagent.md — the cn role of `dnv-agent` (dnv)

Status: **normative**. Read `log.md`, `osclient.md`, `grpc.md` and
`dnagent.md` first — this document builds on their rules and does not restate
them. In particular `dnagent.md` §2 (the shared mechanism package `agent`:
bootstrap SH1-SH3, local store SH4-SH7, revision gate SH8-SH9, locks
SH10-SH13, `ResInfo` tracking SH14, OS wrappers SH15-SH20, bitmap-chunk store
SH21-SH23, check-stream rules SH24-SH26) and §3 (the `cmd/dnv-agent` cobra
skeleton CM1-CM4) are **[shared]** and are NOT re-specified here; `dnv-agent
cn` reuses them unchanged, plus the small additions of §2/§3 below (recorded
as amendments in §5).

Required background: `schema.proto` (service `ControllerNodeAgent` and its
messages, plus `SpLevel`, `ResInfo`, `CnInfo`, `CntlrInfo`, `BitmapInfo`,
`CntlrPointer`, `Slice`/`Group`/`Leg`/`Side`, `ThinDevice`, `Subsystem`/
`Namespace`, `Clone`, `Transfer`, `Migration`, `QosRatio`, `BdevConf`);
`architecture.md` §3.2-§3.6 (the CN device stacks and the on-leg layout), §4
(names, §4.6 local-store paths), §8.6-§8.12 (the RPCs whose data-plane
choreography this agent implements), §9 (agent rules — §9.1/§9.3 are the
contract this document implements), §10.4 (what the worker reads from the
reported infos), §11.1 (failover), §11.3-§11.6 (transfer/clone, bitmap math,
clone recovery, namespace suspend), §11.7 (SpLevel), §11.8 (cntlid slots),
§13 (invocation), Appendix A (command patterns), [D1] (leg wrapper), [D2]
(subsystem identity), [D3] (empty destination td), [D4] (fixed ANA groups),
[D6] (health-block payload), [D7]/[D8] (bitmap durability), [D11] (read-only
is CN-only), [D12] (bounded suspensions), [D13] (the disk is authoritative),
[D14] (no LVM anywhere in dnv — the CN clone-metadata arena is a slot
allocator over one loop device), [D15] (whole-side zeroing behind the
`provisioned` gate); `layout.md` §2/§3/§5.

Scope split: §2 and §3 below are the cn **additions** to the shared pieces —
new `common` constants and name helpers, the probe-IO carve-out that
**removes** an `OsClient` primitive in favour of two exported raw helpers, one
new `agent` wrapper method, one new cn-only flag. §4 is **[cn]** — the
`agent/cnagent` policy package. §5 records the amendments this document made
to the companion documents.

---

## 1. Scope and placement

| package | path | role | may import (`layout.md` §3) |
|---|---|---|---|
| `cnagent` | `agent/cnagent/` | cn **policy**: the `ControllerNodeAgent` service — which leg connections, md arrays, thin pools, dm and nvmet objects to build and when | `common`, `pb`, `agent` |

Everything mechanism-shaped that **both** roles use lives in `agent`
(`dnagent.md` §1 split rule) and is reused verbatim. Tooling only the cn
role runs — mdadm, the clone-metadata slot allocator, the thin-metadata
reader — is **not** promoted into `agent`: by the same
split rule, a wrapper with a single role is role code and lives in
`agent/cnagent/` (`md.go`, `clonemeta.go`, `thinbm.go`, §4.1). There is **no
LVM anywhere in dnv**: [D14] removed the clone VG, LVM's last user, in favour
of the §2.1 kind-`b` wrapper linears. Agents never talk
to etcd (`layout.md` §3); acceptance re-checks it. The gRPC server carries
the **server** interceptors of `grpc.md` §4; unlike the dn role, the cn
agent's outbound connections are still only `nvme connect` — never gRPC.

## 2. Additions to the shared pieces

### 2.1 Additions to `common`

The following enter the existing files `common/constants.go` and
`common/name_fmt.go` (no new files — `layout.md` §7.5 still holds):

```go
	// CN base state (architecture.md §3.2): the tmpfs that carries the
	// clone-metadata arena file, sized 2 × CnCloneMetaAreaSize so that even
	// a fully materialized arena plus slack never hits ENOSPC on the mount.
	// The file itself is sparse: pages appear as dm-clone writes metadata
	// and are released again by the allocator's hole-punch discard.
	DefaultCnTmpfsSize = 2 * 1024 * 1024 * 1024

	// The CN clone-metadata arena ([D14], CN5/CN18): one sparse file
	// (CnTmpFilePath) on the CN tmpfs, attached to a single loop device,
	// carved into fixed units by the CN slot allocator whose registry is the
	// kind-`b` wrapper dm tables themselves. No LVM.
	// CnCloneMetaAreaSize is the `truncate` size of that file — and so the
	// arena size, 256 units. CnCloneMetaUnit is the allocation granularity,
	// the cn twin of DnCloneMetaUnit; it is deliberately NOT called an
	// "extent", which everywhere else in dnv means the 1 GiB DN/CN
	// allocation unit (DefaultDnExtSize).
	CnCloneMetaAreaSize = 1 * 1024 * 1024 * 1024
	CnCloneMetaUnit     = 4 * 1024 * 1024

	// Seconds between two §3.6 leg health-probe rounds on a primary
	// cntlr, and how long one probe IO may stay in flight before the leg
	// is reported stalled (cnagent.md CN11). Probes are single-flight per
	// leg and run outside every lock.
	CnLegProbeInterval     = 5
	CnLegProbeStallSeconds = 15

	// The §3.6 health block: the last 4 KiB of the leg's meta region.
	// Payload = magic + writer id + timestamp, never interpreted on read
	// ([D6]).
	LegHealthBlockSize = 4096
	LegHealthMagic     = "DNVHLTH1"

	// Seconds between background retries of a pending cn outbound nvme
	// connect — leg side connections and clone source connections
	// (cnagent.md CN10/CN18); the cn twin of DnMigrConnectRetryInterval.
	CnConnectRetryInterval = 5
```

`CnCloneMetaAreaSize` is the rename of the old `DefaultCloneVgSize` (same
1 GiB); `DefaultCloneVgPrefix`, `DefaultCloneVgExtSize` and the
`NameFmt.cloneVgPrefix` field they fed are **deleted** ([D14]).

and in `common/name_fmt.go`, three new dm kinds `9`, `a` and `b` in the CN
namespace of §4.1/§4.2 (methods on `NameFmt`, formats normative):

```go
// CnLegName is the cn-local leg wrapper of architecture.md §3.3 step 1
// ([D1]): one dm-linear over the leg's single nvme multipath namespace
// device, kept as the leg-level indirection point (what a teardown reloads
// onto an error target, and what md/groups consume as the member device).
func (nf *NameFmt) CnLegName(clusterId, cnId, spId, legId uint64) string
	// → dnv-{cluster}-{cn}-9-{sp}-{leg}

// CnGrpName is a RedundNone group device (§3.3 step 2): a dm-linear over
// the single leg's data region. RedundMdRaid1 groups use the md names of
// §4.3 instead and have no dm name.
func (nf *NameFmt) CnGrpName(clusterId, cnId, spId, grpId uint64) string
	// → dnv-{cluster}-{cn}-a-{sp}-{grp}

// CnCloneMetaDmName is the dm-clone metadata wrapper of CN18 ([D14]): a
// dm-linear over one contiguous run of CnCloneMetaUnit-sized units of the CN
// clone-metadata loop device. dm-clone reads its superblock from sector 0 and
// takes no offset argument, so every slot must be presented as a device of
// its own — the same reason DnMigrMetaDmName exists on the dn side.
// CnCloneMetaDmPrefix is the `dmsetup ls` filter that enumerates them, and it
// must come from NameFmt because the dm prefix is configurable.
func (nf *NameFmt) CnCloneMetaDmName(clusterId, cnId, spId, cloneId uint64) string
	// → dnv-{cluster}-{cn}-b-{sp}-{clone}
func (nf *NameFmt) CnCloneMetaDmPrefix(clusterId, cnId uint64) string
	// → dnv-{cluster}-{cn}-b-
```

[D1] left the leg wrapper "agent-internal"; fixing its name here makes it
observable (tests, `dmsetup ls`, cleanup by prefix) without making it part of
any cross-**component** contract — nothing outside the cn agent ever
addresses these three devices. For kind `b` the name is more than
convenience: the kind-`b` **tables are the allocation registry** (CN5, CN18),
so the prefix+kind filter of `dmsetup ls` must be unambiguous.
`architecture.md` §4.1 points here for the CN kinds `9`/`a`/`b`.

### 2.2 Leg-probe IO leaves the `OsClient` (osclient.md §4.5.1 amendment)

The [D6] health probe "reads the block back with O_DIRECT": a buffered read
(`ReadBlock`) of a just-written block would be served from the page cache and
observe no device IO at all, making the read-back vacuous. But the probe must
also be free to **hang forever** — a probe against a pathless leg
(`ctrl_loss_tmo = -1` ⇒ IO queues indefinitely) is *how* a silent target stall
is detected. `LimitedOsClient` is a `DefaultOsClientLimit`-slot (32) semaphore
held across the blocking syscall, and one dead or partitioned DN can back far
more legs on a CN than there are slots (`MaxSideCntPerDn` = 1024): wedged
probes would starve every OS operation on the node — **including the teardown
`nvme disconnect` that is the documented release mechanism for a wedged
probe** (CN11, CN21) and the mdadm/dm commands §10.4 self-healing needs. CN1
kept the probers out of the *lock* hierarchy; this rule completes the
carve-out for the *semaphore*.

The CN11 probers therefore do **not** go through the process's
`LimitedOsClient`. They call the raw block-IO syscalls directly, through two
helpers exported from `common/osclient.go` (recorded in §5, edit applied to
`osclient.md` §4.5.1, which owns their exact semantics):

```go
	// Raw, unlimited, no ctx, no logging, no cached fd — a fresh open per
	// call. WriteBlockAt is a buffered pwrite followed by fsync;
	// ReadBlockDirectAt opens O_RDONLY | O_DIRECT with a 4096-aligned
	// buffer, so offset and length MUST be multiples of 4096 and a short
	// read is an error.
	func WriteBlockAt(path string, offset uint64, data []byte) error
	func ReadBlockDirectAt(path string, offset uint64, length uint64) (data []byte, err error)
```

`ReadBlockDirect` is **removed** from the `OsClient` interface, from
`LimitedOsClient` and from `FakeOsClient` — the prober was its only caller.
The cn agent reaches the helpers through a small fakeable dependency, so unit
tests can still script probe IO:

```go
type LegProbeIO interface {
	Write(ctx context.Context, path string, offset uint64, data []byte) error
	ReadDirect(ctx context.Context, path string, offset, length uint64) ([]byte, error)
}
```

The real implementation calls the exported helpers and logs **one record per
half itself** — msg `probe write block` / `probe read block direct`, attrs
`path`/`offset`/`length` (plus `error` on failure), never `data` — because
these calls no longer pass through the `OsClient` that would otherwise log
them; the fake mirrors `FakeOsClient`'s fn-field style. The `ctx` exists only
to carry the CN2 per-attempt trace id into that record and to fast-fail an
attempt whose ctx is already done (silently — an operation that never happened
is not logged); it cannot interrupt a syscall in flight, which is why the raw
helpers take none. Writes need no direct twin: `WriteBlockAt` ends in
`fsync`, which forces the write to the device and surfaces its error.

**The recorded carve-out**: probe IO is the one sanctioned direct-syscall path
in dnv; it may block indefinitely by design; it must never run under a lock
(CN1) nor through the semaphore. The transport-health gate discussed in review
(skipping a round when the sysfs walk shows no live+optimized path) is **not**
part of this rule; it may be added later as an optimization.

### 2.3 `NvmeHost.DisconnectDevice` (dnagent.md SH20 amendment)

The two sides of a migrating leg share one subsystem NQN ([D1]), so
`nvme disconnect --nqn` would kill **both** paths. When a leg's desired side
set shrinks (post-`FinishMigration`), the cn agent must disconnect only the
dead side's controller: `nvmehost.go` gains
`DisconnectDevice(ctx, dev string)` = `nvme disconnect --device {dev}`, with
the controller device found by the §5 **sysfs walk**, never by
`nvme list-subsys`: match
`/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` against the leg NQN, then
for each `nvme{N}` controller entry inside that subsystem directory read
`/sys/class/nvme/{ctrl}/address` and parse it as comma-separated `key=value`
pairs (`traddr=…,trsvcid=…`) to find the dead side's controller — never by
field position. Recorded in §5.

## 3. `cmd/dnv-agent cn` (dnagent.md §3 amendment)

CN-CM1. The CM2 flag table gains one cn-only row (edit applied to
     `dnagent.md`):

| flag | dn | cn | default | meaning |
|---|---|---|---|---|
| `--capacity` | — | ✓ | 0 | capacity budget in bytes this CN is willing to host; `GetCnSize` replies it verbatim, `0` = "use the CP default" (`DefaultCnCap`, §6.1) |

CN-CM2. `runCn` mirrors `runDn` (CM4): bind viper, require the common
     values (`--capacity` is optional), `signal.NotifyContext`, construct
     `common.NewNameFmt(localStore)` and the process's single
     `common.NewLimitedOsClient(0)`, build
     `cnagent.NewCnAgentServer(oc, nf, localStore, capacity, trConf)`, and
     call `agent.Serve` with the role's reconcile, a register func that
     calls `pb.RegisterControllerNodeAgentServer`, and `nil` for the SH27
     background waiter — the cn owns no long-running child process, and its
     CN11 probers are cancelled at `rootCtx`, never joined (§2.2: a wedged
     direct read would hang shutdown forever). This replaces the
     placeholder `"the cn role is not implemented yet"` error.

## 4. The cn role — package `cnagent` [cn]

### 4.1 Files

`server.go` (the `CnAgentServer` type, lock mapping, RPC entry points),
`plan.go` (per-cntlr derived names/sizes, the ns-dev backing and ANA state
machines of CN16), `syncup_cn.go`, `syncup_cntlr.go`, `leg.go` (CN10 side
connections + wrappers), `healthcheck.go` (the CN11 [D6] probers and their
§2.2 `LegProbeIO` — direct syscalls, never the `OsClient`), `md.go`
(mdadm wrapper + the §11.1.1 assembly cases), `clonemeta.go` (the §3.2
base-state wrappers — tmpfs mount, `truncate`, `losetup
--associated`/`--find --show` — plus the CN18 clone-metadata slot allocator:
unit accounting over the single loop device, the `blkdiscard` recycle guard,
the kind-`b` wrapper linears and the `dmsetup ls`/`dmsetup table` enumeration
that **is** the allocation registry), `pool.go` (concats, thin pools, thin volumes), `td.go` (raid0,
dm-error, ns-dev, flakey), `clone.go` (CN18 + §11.5 recovery), `xfer.go`,
`push_clone_bm.go`, `thinbm.go` (CN25-CN27: the thin-metadata snapshot
reader behind `GetThinDeviceBm`/`GetLegBm` and the §11.4 clone-geometry
fold), `check.go`, `probe.go`. Colocated `_test.go` files. This is the
`layout.md` §2 recommended split (updated by §5); package boundaries are
binding, file names are not.

### 4.2 Server type and lock mapping

```go
type CnAgentServer struct {
	pb.UnimplementedControllerNodeAgentServer
	oc       common.OsClient  // the process's single LimitedOsClient
	nf       *common.NameFmt
	cmd      *agent.Cmd
	store    *agent.Store
	dm       *agent.Dm
	nvmet    *agent.Nvmet
	host     *agent.NvmeHost
	md       *Md              // mdadm (md.go)
	cmeta    *CloneMeta       // base state + clone-metadata allocator (CN5/CN18)
	probeIO  LegProbeIO       // CN11 probe IO: direct syscalls, never oc (§2.2)
	locks    *agent.LockSet   // object key = LocalCntlrPath id tuple
	capacity uint64           // --capacity
	port     agent.PortConf   // --tr-*
	// in-memory mirrors of the local store: cn requests, cntlr states
	// (applied request, ResInfo tracker, per-clone ChunkSets, connect
	// retry registry, leg-prober registry), guarded by a leaf mutex.
	// rootCtx anchors the CN10/CN18 retry loops and CN11 probers.
	// cloneMetaMu serializes the CN18 allocator (CN5): the registry is the
	// kernel's dm tables, so enumerate → discard → create must be one
	// critical section. It is the one cn lock held across OS calls — a leaf
	// taken below every LockSet lock, never held while another is acquired.
}
```

CN1. Lock mapping (instantiates SH10-SH13): `SyncupCn` and the startup
     reconcile — node write; `SyncupCntlr`, `PushCloneBitmap`,
     `GetCntlrInfo`, one `CheckCntlr` round, one background connect-retry
     attempt, `GetThinDeviceBm` and `GetLegBm` — node read + that cntlr's
     object lock (key = `(cluster_id, cn_id, sp_id, cntlr_id)`); `GetCnInfo`
     and one `CheckCn` round — node read; `GetCnSize` — no lock. The CN11
     leg probers take **no** lock at all: their IO can block far past every
     command timeout (a leg with no serving path queues IO indefinitely),
     and nothing that holds a lock ever waits on them — they only publish
     results into their own registry, which probes read. They also take **no
     `OsClient` semaphore slot**: their IO is issued by the §2.2
     direct-syscall path, so a wedged prober can starve neither the lock
     hierarchy nor the node's 32-slot OS-command budget — which is what keeps
     the teardown `nvme disconnect` that releases it runnable.

### 4.3 Startup reconcile

CN2. Enumerate the store (SH6; cn kinds `cn-`, `cntlr-`, `clone-bm-`) and
     load every `cn-*` and `cntlr-*` request into memory first. Then reload
     every `clone-bm-*` chunk into the owning cntlr's `ChunkSet`s (SH21) —
     **before** any converge runs, so that a dm-clone the converge (re)builds
     re-applies in the same pass every chunk the node already holds (CN18
     step 4). Loading them afterwards would lose none of them — a chunk in
     memory is advertised as applied by the next reply's `bm_idx_list`
     (CN20), so the worker's BM2 diff would not push it again — but their
     `blkdiscard`s would then wait for the next event that re-applies the
     whole set: a create of that dm-clone, a converge reload of its table
     onto a changed length, devno or region size (step 4 runs on both —
     the converge treats a reload exactly as a create), or another
     chunk's own push (CN22). Until then the clone re-copies regions it
     never needed to. A chunk file whose cntlr is not among the loaded
     `cntlr-*` requests, or whose `clone_id` is absent from that cntlr's
     stored `clone_list`, is an orphan — its cntlr or clone was deleted
     while the chunk file survived (SH7) — and is deleted here, because
     it names an owner no later pass will ever look for. Then, for each
     `cn-*` request: re-run the SyncupCn converge (§4.5 step CN5). Then
     each `cntlr-*` request: if its pointer is absent from the stored
     `SyncupCnRequest.cntlr_pointer_list`, tear the cntlr down (CN21) —
     it was removed mid-teardown; otherwise re-run the SyncupCntlr
     converge (§4.6) from the stored request — which, per CN18, runs the
     §11.5 recovery for any clone whose **dm-clone or metadata wrapper** is
     gone or mismatched — recovery keys off missing dm-clone *metadata*, not
     off the wrapper alone (`TestCloneRecoveryWhenOnlyTheDmCloneVanished`) —
     (a reboot
     clears the tmpfs, the loop device and every kind-`b` wrapper together —
     the arena is volatile *with* the kernel's dm state — so the reconcile
     starts from an empty arena; a plain agent restart preserves both and the
     converge is a no-op re-apply). Finally, **after** every converge — so a
     clone that was just (re)built already holds its wrapper and is never
     mistaken for an orphan — **sweep the arena**: a kind-`b` wrapper (prefix
     `CnCloneMetaDmPrefix`) whose clone appears in no stored `cntlr-*` desired
     state is an orphan and is removed, which frees its units for the next
     allocation. The sweep compares against a name set built from every stored
     cntlr's `clone_list` — there is no reverse parser for the kind-`b` name —
     and it runs here and at the end of every `SyncupCn` converge (CN5),
     because those are the only two places that hold the node write lock and
     see the complete stored desired state; a `SyncupCntlr` sees one cntlr.
     All under the node write lock, with the SH2 trace id. Background
     retries (CN10/CN18) and probers (CN11) mint a fresh trace id per
     attempt.

### 4.4 `GetCnSize`

CN3. Reply `--capacity` verbatim. `0` means "no local opinion" and the CP
     substitutes `DefaultCnCap` (§6.1); the agent never validates or clamps
     — the gateway owns the `[MinCnCap, MaxCnCap]` mapping. `cn_id` may be 0
     (pre-registration call); it is for logging only. No locks, no store, no
     OS access. This RPC has no `AgentReply`; it cannot fail.

### 4.5 `SyncupCn`

CN4. Gate the revision (SH8) against the stored `SyncupCnRequest`.

CN5. Converge the once-per-CN base state of `architecture.md` §3.2,
     probe-first (SH16), building `CnInfo` as it goes:
     * **tmpfs** at `CnTmpfsPath(cluster_id, cn_id)`: probe with
       `findmnt --noheadings --output FSTYPE --target {path}` — a non-zero
       exit means "nothing mounted there", not a failure, and the reported
       type is what lets the converge insist the mount is a tmpfs — followed
       by a second `findmnt --noheadings --output TARGET --mountpoint {path}`.
       The second call exists because `--target` resolves to the *closest
       enclosing* mountpoint, so a path that merely lives under another mount
       would answer that mount's type; `--mountpoint` matches only when the
       path is itself the mountpoint. A mount of the wrong type is an `ERROR`
       on `tmpfs_info`, so what the second call actually saves is the case
       where the *enclosing* mount is itself a tmpfs: the arena lives under
       `DefaultTmpfsPrefix` = `/tmp/dnv-tmpfs`, so a `/tmp` on tmpfs would
       answer for it, the `mount` would be skipped, and the arena would share
       that filesystem instead of getting its own `DefaultCnTmpfsSize`.
       Absent ⇒ `mkdir -p` the mountpoint and
       `mount -t tmpfs -o size={DefaultCnTmpfsSize} tmpfs {path}`.
     * **backing file** `CnTmpFilePath`: probe `stat --format %s`; absent ⇒
       `truncate --size {CnCloneMetaAreaSize} {path}` (sparse — tmpfs pages
       materialize only as clone metadata is written, and CN18's hole-punch
       `blkdiscard` frees them again).
     * **loop device**: probe `losetup --associated {CnTmpFilePath}` (the
       loop path is re-learned from this probe on every converge **and every
       probe pass** — it is kernel-assigned state, never persisted, and a
       stale cached path is exactly what CN28's arena check must catch);
       absent ⇒ `losetup --find --show {CnTmpFilePath}`. Exactly one loop
       device: multiple attachments of the same file are unwanted.
     * **clone-metadata arena**: nothing to create. The arena *is* the loop
       device: `CnCloneMetaAreaSize / CnCloneMetaUnit` = 256 units of
       `CnCloneMetaUnit`, handed out to clones by the CN18 slot allocator.
       There is **no** on-disk allocation table — enumerating the kind-`b`
       wrappers (`dmsetup ls` filtered by `CnCloneMetaDmPrefix`, then
       `dmsetup table` of each) reconstructs the used map, because every
       wrapper's table `0 {len} linear {loop maj:min} {offset_sectors}`
       records its own allocation (the name comes from `dmsetup ls`, the
       major:minor only from `dmsetup table` — `ls` formatting varies across
       versions). The `ls` list is a snapshot that is stale the instant it is
       printed — a `SyncupCntlr` holds only the node *read* lock, so another
       cntlr's retire or `SP_LEVEL_DISABLE` teardown can remove a wrapper
       between the `ls` and its `table` — so a name whose `table` fails is
       **dropped** when the wrapper has meanwhile vanished: it claims no
       units, and failing the CN-wide enumeration would flip unrelated
       cntlrs' healthy clones to `RES_STATUS_ERROR`, which feeds `err_epoch`.
       The absence is *confirmed* with `dmsetup info` first, never assumed: a
       `table` failure on a wrapper that is still there stays fatal, because
       reporting a live wrapper's units as free would let the next allocation
       hole-punch a serving dm-clone's superblock — the one thing CN18's
       discard-before-create order exists to prevent. Reconstructing the used
       map from those tables is why the CN needs none of the dn's
       header/CRC/A-B machinery: the arena is volatile *together with* the
       kernel's dm state (`architecture.md` §3.2) — a reboot clears both, an
       agent restart preserves both — so the kernel's dm tables **are** the
       registry. A reboot therefore leaves an empty arena and clones rebuild
       per §11.5. The orphan sweep of CN2 also runs at the end of this
       converge, under the same node write lock.
     * `EnsurePort` (SH19: port `NvmetPortId` from the `--tr-*` flags + the
       three fixed ANA groups). CN host-facing namespaces only ever use
       groups 1 (`optimized`) and 3 (`inaccessible`) — group 2 exists on
       every port ([D4]) but no cn code path assigns it.

CN6. **QoS is accepted and deliberately not enforced in this version.** The
     §3.2 step 4 open issue stands: `io.max` written from any agent-created
     cgroup binds the agent's own tools, not the nvmet kernel threads that
     carry host IO, so programming it would only pretend. The agent
     persists `qos_ratio` with the request (SH5 does that for free), applies
     nothing, and reports no QoS resource in `CnInfo`. When the architecture
     decides the enforcement mechanism, it lands as a new converge step
     here; nothing else in this document changes. Recorded in §5
     (`architecture.md` §3.2/§9.3 annotated).

CN7. Diff `cntlr_pointer_list` against the local `cntlr-*` files (§9.1 full
     sync). A pointer in the request without local state needs nothing yet —
     resources come with its first `SyncupCntlr`; the persisted request is
     what makes the pointer *known*. A local cntlr file whose pointer left
     the list is torn down per CN21, then its `cntlr-*` and `clone-bm-*`
     files are deleted and its lock dropped (SH7). The base state itself is
     never torn down — like the DN port, it outlives every cntlr and only
     lab cleanup removes it. Persist the request (SH5); reply `agent_reply`,
     `revision`, `cn_info`.

### 4.6 `SyncupCntlr`

CN8. **Gating.** The pointer MUST be present in the stored
     `SyncupCnRequest.cntlr_pointer_list` — else `ReplyCodeUnknownObject`
     (`SyncupCn` introduces pointers first, §9.1). Then the SH8 revision
     gate against the stored `SyncupCntlrRequest`. Then, last and still
     with **zero** side effects, the §7 **conf gate**:
     `agent.ValidateBdevConf(req.bdev_conf)` (`dnagent.md` §2.1) refuses a
     request whose `dm_pool_conf.data_block_size`,
     `dm_pool_conf.low_water_mark_pct` or `dm_raid0_conf.stripe_size` is 0,
     or whose `redund_conf` selected md-raid1 with a 0
     `bitmap_chunk_block_cnt`. The control plane resolves all four when it
     *writes* the conf (§7), so a zero here is a geometry no agent may
     invent a replacement for — those values become the thin-pool's, the
     raid0's and the md bitmap's own arguments (CN12, CN13, CN15), and a
     geometry this node guessed is one the rest of the cluster does not
     share. The refusal is `ReplyCodeInvalidConf` (`dnagent.md` §2.5)
     carrying the validator's message, and the reply echoes the **stored**
     revision, not the request's, so the worker sees that the request was
     not accepted. One `Error` record, msg `"invalid stored conf"`, names
     the ids and the field.

     Placement is load-bearing: the gate sits **before** the request
     becomes this cntlr's desired state, so it skips the desired-state
     promotion, the whole converge — whose retire phase alone rewrites ANA
     states, reloads ns-dev linears and removes dm devices — and CN20's
     local-store persist, which is what keeps a refused request from being
     replayed by the next startup reconcile. The same check is repeated in
     the converge itself for the two entrances that do not come through
     this RPC — the CN2 startup reconcile, which converges from a file an
     older build may have persisted with zeros, and the CN10/CN18
     background connect retry, which re-enters with the request it already
     holds. There it returns an empty `CntlrInfo` and leaves the applied
     plan untouched, so a later teardown still plans from the last shape
     this agent actually built.

CN9. **Role and pass structure.** The effective role is **primary** iff
     `cntlr.primary && !cntlr.disabled`; anything else converges the §3.4
     standby shape (a disabled cntlr additionally moves every host-facing
     namespace to `inaccessible` — which the standby shape already does, so
     `disabled` never needs its own mechanism).

     **Effective desired state (the provisioning gate, [D15]).**
     Before either phase the agent derives an *effective* desired state from
     the raw one, and both converges and probes against it. A resource
     present in the raw desired state but excluded from the effective one
     reports `RES_STATUS_PROVISIONING` with `details = "provisioning"` —
     *deliberately not created yet, healthy, no action needed* — never
     `RES_STATUS_ERROR`, and it never feeds `err_epoch` (§9.5). The details
     string carries **no** progress counter on purpose: a value that changed
     every round would defeat the `proto.Equal` suppression of the Check
     stream (SH26) and re-send `CntlrInfo` on every tick; the dn's
     `"zeroing k/n"` is affordable only because its counter advances slowly
     and its stream is per-side. The exclusions:
     * a leg is **provisioning** iff its `side_list` is non-empty and
       **every** side in it has `provisioned = false`. Mid-migration a
       provisioned src plus an unprovisioned dst leaves the leg serving, so
       it is *not* provisioning; and an *empty* `side_list` stays the
       malformed-request error it already was (CN10), never a healthy
       `PROVISIONING` row that would hide it.
     * a group whose non-spare `leg_list` contains a provisioning leg is
       **deferred**: no md array, no `CnGrpName`, and the group is excluded
       from the pool-meta and pool-data concat targets and from pool sizing
       (CN12, CN13).
     * the concat exclusion is a **prefix truncation, never a filter**: each
       of a slice's two group lists contributes only its longest leading run
       of non-deferred groups, so a deferred group takes every group *after*
       it in the same list out of the concat too (those groups still assemble
       their md arrays — CN12 defers an array only on the group's own legs;
       only concat membership is truncated). A concat target's offset is the
       sum of the lengths before it, so dropping a group out of the *middle*
       would re-base every later target and silently relocate live pool-data
       blocks the moment the deferred group cleared. A prefix leaves the
       concat exactly the one that is already live, which is what makes
       CN13's "a not-yet-grown concat is `OK`, not a mismatch" true and what
       keeps CN27's span arithmetic answering about the live table.
     * a slice **either** of whose two group lists (meta, data) is non-empty
       in the raw desired state and has no effective group left is
       **deferred whole** — not merely a slice all of whose groups are
       deferred. An empty concat is an **error, not a shape**: the concat
       would be built from an empty segment list, so the initial
       `CreateStoragePool` — where every leg provisions, and where the two
       lists routinely clear at different times, one DN finishing its §9.4
       zeroing before the other — would report `"concat has no segments"` as
       an `ERROR` on `slice_id_to_meta`/`slice_id_to_data` and
       `"pool concat missing"` on `slice_id_to_dm_pool`, raising `err_epoch`
       on a freshly created SP, which is exactly what this gate exists to
       prevent.
     * a td backed by a deferred slice is deferred, and with it its ns-devs
       and namespaces (CN16), its transfers (CN17) and its clones (CN18) —
       each reporting `PROVISIONING` for its own rows. Deferral must reach
       that far: building a transfer or a clone over a raid0 that does not
       exist would fail into `ERROR`/`err_epoch`, and promoting a namespace
       over a non-existent device is the very hazard CN16's ANA conjunct
       prevents.
     * an unprovisioned **spare** leg defers only itself — spares never
       assemble (§8.12) — and is explicitly marked unavailable, so CN12's
       case-1 guard refuses `--create --assume-clean` by rule rather than by
       zero value.
     * bitmap reads follow the effective state as well: CN27's data-group
       span walks the **effective** `data_grp_list` (leaving a deferred group
       in the walk would silently shift every later group's offset), and
       `GetLegBm` on a provisioning leg fails the RPC saying so rather than
       answering "not in its slice's `data_grp_list`".

     An initial `CreateStoragePool` in which every leg is still provisioning
     therefore converges to an empty-ish cntlr reporting `PROVISIONING`
     throughout: no error, no `err_epoch`, no failover flapping. Deferral is
     **not** CN19's `sp_level` suppression, and where both apply **sp_level
     wins** (`RES_STATUS_MISSING`, `details = "sp_level"`): the level is a
     state the operator asked for, and claiming a resource "is coming" would
     contradict them.

     One converge pass has two phases, and the phase order is what
     implements §11.1 without special cases:

     * **Retire phase, top-down** — for every resource that the new desired
       state (level-adjusted, CN19) no longer wants: first rewrite the
       `ana_grpid` of every namespace leaving service to
       `AnaGrpIdInaccessible`, then reload the affected `CnNsDevName`s — the
       survivors that must stop serving *and every removed namespace's*
       (one that never had a backing td has no `CnErrorName` to park on, and
       keeps `removeDm`'s resume as its backstop) — onto their `CnErrorName`s (`Dm.Reload`'s internal suspend flushes the
       in-flight IO — this **is** §11.1 old_primary steps 1-3, in the listed
       order), then remove nvmet objects that must go entirely, then dm
       devices top-down with `mdadm --stop`, and the **leg** disconnects
       last. One outbound disconnect deliberately runs earlier: a removed
       clone's source connection is dropped in the clone's own retire step,
       right after its dm-clone and metadata wrapper are removed (CN18/CN21
       — the dm-clone flushes through its source on removal, so the
       disconnect must directly follow it, mid-retire).
     * **Build phase, bottom-up** — legs (CN10) → groups (CN12) → per-slice
       pools (CN13) → thin volumes (CN14) → raid0/error (CN15) → clones
       (CN18) → transfers (CN17) → ns-devs + host-facing nvmet (CN16) → ANA
       rewrites to `optimized` last. This **is** §11.1 new_primary steps
       1-4.

     A primary→standby flip is therefore nothing but "the desired set
     shrank to the standby shape"; standby→primary is "it grew". `migr_list`
     is carried in the request but read by nothing: the CN's whole part in a
     migration is that a leg's `side_list` temporarily holds two sides
     (CN10) — the field stays reserved for a future consumer.

CN10. **Legs** (`leg.go`; every leg of every group of every slice in
      `id_to_slice`, `spare_leg_list` included, both roles). Per side in the
      leg's `side_list`: `nvme connect` to
      `SideToCnNqn(cluster, sp, leg, cn_id)` at `side.nvme_tr_conf`,
      hostnqn `CnHostNqn(cluster, cn_id)` (SH20 flags). A side with
      `provisioned = false` is **skipped**: the dn exports nothing for it yet
      (`dnagent.md` DN9/DN10), so a connect could only fail and arm the retry
      registry. Provisioned sides of the same leg connect normally, which is
      what keeps a two-side migrating leg serving while its dst side zeroes.
      A leg all of whose sides are unprovisioned is *provisioning* (CN9): no
      connection, no `CnLegName` wrapper, no prober, and `leg_id_to_leg`
      reports `RES_STATUS_PROVISIONING` (CN28). All sides of a leg
      share that one NQN, so the kernel merges them into **one** multipath
      namespace ([D1]); the agent finds its head device without udev by
      scanning sysfs: the `/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn`
      matching the leg NQN, then the single `nvme*n*` entry inside that
      subsystem directory ⇒ `/dev/{entry}`. On top of it the **leg wrapper**
      `CnLegName`, a whole-device dm-linear of
      `(meta_blocks + data_blocks) × block_size / 512` sectors — sizes come
      from the desired state, never from probing the device. A connect
      failure marks that leg `RES_STATUS_ERROR` and registers the cntlr in a
      background retry registry that re-runs the converge every
      `CnConnectRetryInterval` seconds under the CN1 locks until success or
      teardown (the DN13 pattern). **Dead paths**: a controller of the leg
      NQN whose `traddr`/`trsvcid` matches no desired side (the src side
      after `FinishMigration` — its controller died with DNR and will never
      reconnect) is disconnected by **device** (§2.3), never by NQN.
      `Side.err_epoch` is CP bookkeeping and is ignored.

CN11. **Leg health probes** (`healthcheck.go`; [D6], §3.6). Only the
      **primary** runs the block probe, and only for legs that are not
      provisioning (CN9 — an unprovisioned leg has no connection to probe):
      a standby's path deliberately
      exports dm-error (§3.1), so block IO through it can never succeed —
      §3.6 is amended accordingly (§5). Per connected leg the agent keeps
      one prober goroutine (no locks, CN1): every `CnLegProbeInterval`
      seconds it writes the 4 KiB health block — offset
      `meta_blocks × block_size − LegHealthBlockSize` **through the leg
      wrapper**, payload `LegHealthMagic` + `cn_id` + `UnixNano`, via the
      §2.2 `LegProbeIO.Write` — and reads it back with
      `LegProbeIO.ReadDirect` (O_DIRECT), never
      interpreting the content. Both halves are issued as **direct
      syscalls**, outside the `OsClient` and its semaphore, and each logs its
      own record (`probe write block` / `probe read block direct`, with
      `path`/`offset`/`length`, never `data`) on the prober's fresh
      per-attempt trace id (CN2). The loop is single-flight: a blocked IO
      simply delays the next round. Probers publish
      `{lastOk, lastErr, inflightSince}`; the CN28 probe reports, per leg:
      an attempt in flight longer than `CnLegProbeStallSeconds` ⇒
      `RES_STATUS_ERROR` `"health probe stalled"`; else the last completed
      outcome; before any completion ⇒ `RES_STATUS_OK`
      `"health probe pending"` when the wrapper exists. A **standby** (and
      spare legs on it) reports the transport probe instead:
      the sysfs walk of §5 shows a live controller per desired
      (provisioned) side and, **for a leg with exactly one desired side**,
      an `ana_state` of `optimized` or `non-optimized` — `non-optimized` is
      a standby path's designed steady state (the DN grants the optimized
      group to the primary CN alone) and `optimized` the pre-promote window
      after the DN's flip, while `inaccessible`, or any state the DN never
      sets, cannot serve a promote and reports `RES_STATUS_ERROR`. A leg
      whose `side_list` holds two sides (a migration, CN10) is checked for
      liveness only: its ANA is wrong-by-design for the hydration and the
      phase is the DN's knowledge, not this CN's. Probers
      start when the wrapper converges and are cancelled at teardown; a
      goroutine wedged in D state on a pathless leg is released by the
      teardown's own disconnect (deleting the controller errors its queued
      IO) and is accepted as unreclaimable until then. Because that wedged
      probe holds an **open fd on the leg wrapper**, the teardown order is
      cancel the prober → **disconnect the leg** → remove the wrapper
      (CN21): a `dmsetup remove` issued before the disconnect fails EBUSY.
      Cancellation is never a join — the direct read is uninterruptible,
      so waiting for it would hang shutdown forever (§2.2, `dnagent.md`
      SH27). A wedged prober costs nothing else: it holds no lock (CN1) and
      no `OsClient` slot (§2.2).

CN12. **Groups** (`md.go`; primary only — a standby has none, §3.4).
      * RedundNone: the group device is `CnGrpName`, a dm-linear over the
        single leg wrapper at offset `meta_blocks × block_size / 512`,
        length `data_blocks × block_size / 512`.
      * RedundMdRaid1: md-raid1 `/dev/md/{CnMdDevName}` per Appendix A
        (`--name {CnMdArrayName}`, `--bitmap internal`, `--bitmap-chunk` =
        `bitmap_chunk_block_cnt × block_size / 1024`K, `--data-offset` =
        `meta_blocks × block_size / 1024`K, every member `--failfast`,
        `--homehost any`), members = the leg wrappers of `leg_list` (never
        `spare_leg_list`, §8.12). Assembly instantiates §11.1.1 by probing
        each available member for an md superblock (`mdadm --examine`):
        1. No member has one ⇒ `mdadm --create … --run --assume-clean` —
           but **only when every `leg_list` member is available and was
           probed**: with any member missing, the group is simply
           unavailable this pass ("only k of n legs available and none
           carries a superblock"), because creating over a subset would mint
           a fresh array while an absent leg may carry the real one (pinned
           by `TestGroupNeverCreatesOverASubsetOfLegs`).
           `--assume-clean` is **always** correct here: a side is never
           exported before the §9.4 **provisioning** protocol has zeroed it
           whole (`blkdiscard --zeroout` per batch of extents, tracked in the
           volume table's `zeroed_bits` and gated by `Side.provisioned`), and
           ids are never reused — so a superblock-free leg can only be a
           freshly provisioned side, and both members are all-zero because
           zeros were **written**, not because a discard was assumed to read
           back as zeros ([D15]).
        2. Some have one ⇒ `mdadm --assemble` with those; then
           `mdadm --detail` and `--add --failfast` any available member the
           array left out (freshly provisioned additions and stale-metadata
           re-adds both land here; §11.1.1 cases 1.2/1.3/2 — with a single
           available member mdadm itself decides whether a degraded start
           is safe, and a refusal leaves the group `RES_STATUS_ERROR`).
        A leg is **available** iff its multipath namespace has a path that
        is both `live` and `optimized` (§11.1.1, probed from **sysfs** — §5).
        **Member reconciliation** covers `SwitchSpareLeg` with no extra
        mechanism: a `--detail`-listed member that is no longer in
        `leg_list` is `--fail`ed and `--remove`d; a `leg_list` member the
        array lacks is `--add --failfast`ed (md then resyncs — a bitmap
        catch-up for a briefly absent leg, a full rebuild for a promoted
        spare). The cn agent never runs `mdadm --zero-superblock`: a leg
        only ever leaves an array into the spare list (where a stale
        superblock makes a later re-add cheap) or out of existence with its
        side.
        A group whose non-spare `leg_list` holds a provisioning leg (CN9) is
        **deferred**: none of `--examine`, `--create`, `--assemble`, `--add`,
        `--fail` or `--remove` runs, no `CnGrpName` is built, and
        `grp_id_to_md_raid` reports `RES_STATUS_PROVISIONING`. The assembly
        cases above are evaluated only once every non-spare leg of the group
        is provisioned. (An unprovisioned **spare** never defers the group —
        spares are not members, §8.12.) Deferral here is decided by the
        group's **own** legs only: a group that CN9's prefix truncation keeps
        out of its slice's concat because an *earlier* group is deferred
        still assembles its array normally — it is the concat that waits, not
        the md layer (CN13).

CN13. **Per-slice pools** (`pool.go`; primary only). Per slice of
      `id_to_slice`: the multi-target dm-linears `CnPoolMetaName` (meta
      group devices, `meta_grp_list` order) and `CnPoolDataName` (data
      group devices), tables via stdin (Appendix A); then the thin-pool
      `CnPoolFinalName` with `block_sectors = block_size / 512` and
      `low_water_mark` = data-dev blocks × (100 −
      `dm_pool_conf.low_water_mark_pct`) / 100. `pct = 0` is **invalid**
      and never reaches that arithmetic: the control plane resolved an
      omitted percentage to `DefaultPoolLowWatermarkPct` when it *wrote*
      the conf (§7), so a zero arriving here is a value no agent may
      replace, and CN8's conf gate refuses the request before any planning
      runs. `pct > 100` still means auto-grow off and still passes `0` — no
      dm events at all (§3.3). Nothing is clamped in either direction: the
      agent holds no default of its own, and >100 is a legal setting rather
      than an out-of-range one. A fresh pool needs its metadata to
      read zero, and it does: the §9.4 provisioning protocol **writes** zeros
      over every extent of every side (`blkdiscard --zeroout`) before the
      side is ever exported — the same guarantee that funds CN12's
      `--assume-clean`, and now an actual write of zeros rather than a
      discard whose read-back is hardware-optional ([D15]).
      **GrowSlice** arrives as longer group lists: the
      converge reloads the concat(s) with the appended targets and reloads
      the pool table with the new sizes — dm-thin picks up both data and
      metadata growth from the table swap; no pool message is involved. A
      **deferred** group (CN9) is left out of both concats and out of the
      pool sizing until it clears, and so is **every group after it in the
      same list** — the concat takes each list's leading run of non-deferred
      groups, never a filtered subset, because a target's offset is the sum
      of the lengths before it and a re-inserted middle target would move
      every later group's data under a live pool. Concat and pool tables are
      therefore built and probed at the *effective* (old) size, so a
      not-yet-grown pool is `RES_STATUS_OK` and not a mismatch, and the grow
      completes on the converge that follows the last leg's provisioning —
      out-of-order provisioning of two appended groups simply waits for the
      earlier one. The **serving** pool's `slice_id_to_dm_pool` row keeps
      reporting `RES_STATUS_OK` with its raw `dmsetup status` line
      throughout — the §10.4 auto-grow parses that line, and a
      `PROVISIONING` pool row would silently switch auto-grow off.
      `PROVISIONING` marks only the deferred resources, never the live ones.
      A concat may only ever grow, and the agent **enforces** that rather
      than trusting the plan: when the live table totals more sectors than
      the desired one, `ensureDmMulti` refuses the reload, and the refusal
      is reported the way any unbuildable concat is — `refusing to shrink
      the concat from {live} to {desired} sectors` as an `ERROR` on
      `slice_id_to_meta`/`slice_id_to_data`, `"pool concat missing"` on
      `slice_id_to_dm_pool` — because the target list is the physical layout
      of every block the pool above has already allocated: reloading a
      shorter one remaps live pool data, after which dm-thin either refuses
      the resume and leaves the pool suspended or accepts it and serves the
      wrong device. That shape can only mean the effective state lost a
      group that is already serving, which the provisioning deferral is never allowed
      to produce, so the refusal leaves the live concat untouched for the
      §10.4 reactions or an operator to repair the group.

CN14. **Thin volumes** (`pool.go`; primary only). Per td × slice:
      `CnThinDevName`, virtual size `td.size / slice_cnt`, attached by
      `dmsetup create` of the `thin` table — preceded by a pool message only
      when the control plane has not yet seen the td materialized.
      `create_snap` requires a quiesced origin: when the origin's thin
      volume device is live, the agent suspends it across the message and
      resumes immediately after — a second deliberate, bounded suspension
      beyond [D12]'s window, held only for the duration of one
      `dmsetup message`.

      **Three cases, decided per td by two request fields**
      (`ThinDeviceCreated.md` U4-S1):

      1. `created == true`, any `ori_id` — **nobody messages**. `dmsetup
         create` of the `thin` table when the device is absent; the id is
         known to exist in every slice pool (architecture.md §10.3).
      2. `created == false`, `ori_id == 0` — **`ensureThin` messages**
         `create_thin {dev_id}` when the device is absent, then `dmsetup
         create`. `EEXIST` is tolerated: a crash between the message and the
         create, or a message a previous primary already sent.
      3. `created == false`, `ori_id != 0` — **the pre-pass messages, and
         only it**: `create_snap {dev_id} {ori_id}` per pool-ready slice
         whose snapshot device is absent, inside the CN14 quiesce when the
         origin's raid0 is live; then the thin loop's `dmsetup create`.

      Both clauses are re-derivable from the request alone, which is why
      `ensureThin` never messages a td with `ori_id != 0` whatever the
      pre-pass did, and why no handoff between the two is needed.

      **A created td is never messaged** on any pass — fresh primary after a
      failover, startup reconcile from the local store (SH1-SH3), a device
      removed by hand, ever. `ensureThin` reads `dm.Info` and, when the device
      is absent, runs `dmsetup create` directly. If that fails because the pool
      does not hold the id, the row reads `RES_STATUS_ERROR` with the dmsetup
      output in `details`, the worker records `err_epoch`, and **no later
      converge sends a message either**: pool-metadata loss surfaces as an
      intervention event (Appendix D) instead of an empty volume under a live
      `dev_id`, which is what an unconditional `create_thin` would produce.
      Standby cntlrs build no thin volumes and are unaffected.

      **Cross-slice point-in-time.** The per-slice
      suspend quiesces one pool's origin only; a striped td
      snapshots atomically only if no host write lands between two slices'
      messages. When any slice still needs this td's `create_snap` and the
      origin td's raid0 (`CnRaid0Name`) is live, the agent therefore
      suspends that raid0 first, issues every needed slice's `create_snap`
      (each under its own per-slice origin-thin suspend — dm-thin's own
      requirement, unchanged), and resumes the raid0 afterwards, on the
      error paths too, so no device outlives the sequence suspended ([D12]).
      Only the messages sit inside the window — the snapshots' own thin
      *devices* are created after the resume, since the content is fixed at
      message time. The window is bounded by `slice_cnt` messages under the
      SH15 timeouts. The trigger is liveness and nothing else: when the
      origin td's raid0 is not live — never built on this cntlr, already
      removed, or `sp_level` suppressing pools — there is no dnv IO path to
      quiesce and the messages go unquiesced. A *deferred* origin is not a
      case of that: `deferred` suppresses the raid0's converge, not the
      device, so a raid0 an earlier revision built stays live and is
      quiesced like any other (`ThinDeviceCreated.md` U4-S3 removed the
      pre-pass's `origin.deferred` early return; §6 test 19).

      **The pre-pass owns every message of an uncreated snapshot**
      (`ThinDeviceCreated.md` U4-S3). Its claim set is every slice with
      `poolReady` whose snapshot thin device is absent; an `Info` error skips
      the slice, because `ensureThin` will fail it with the same error. The
      gate is pool readiness, never the td's `deferred` flag — that flag is
      plan-global (the deferral's `anySliceDeferred`), and gating messages on it would
      drop a ready slice's `create_snap` whenever some sibling slice were
      still provisioning. There is **no** filter on the origin's own thin
      device: the gateway refuses a snapshot whose origin is not materialized
      in every slice pool (architecture.md §8.7), so `create_snap` can no
      longer be inverted with the origin's `create_thin`, and whether *this*
      CN has built the origin's dm device is irrelevant to a message the pool
      metadata answers. The quiesce is `suspendSnapOrigin` when the plan holds
      the origin, which suspends the raid0 iff it is live — covering a fresh
      primary, a plan with nothing built yet and a raid0 someone else holds
      suspended. An origin absent from the plan means there is nothing to
      quiesce, not that the messages belong to someone else.

      **Order independence.** Neither the thin loop nor the pre-pass depends
      on the relative position of an origin and its snapshot in `td_list`, and
      the plan keeps request order with no sort. Same-pass creation of an
      origin and its snapshot is not a state the gateway can produce.

      **A violated precondition is left to dm-thin.** A `create_snap` whose
      `ori_id` the pool does not hold fails at the message; the `dmsetup
      create` behind it fails; the row reads `RES_STATUS_ERROR` with the
      dmsetup output; the td stays `created == false`, so every converge
      retries. No detection, no distinct status — the origin guarantee is the
      gateway's contract to keep, not the agent's to re-check.

      Residual: a primary crash between two slices' messages still tears
      the snapshot — delete and re-create a snapshot whose creation raced a
      crash (architecture.md §8.7, Appendix D). `created` certifies
      materialization, not point-in-time consistency: the next primary sends
      the remaining messages, every row goes `OK`, and the flag flips.

      The persisted `SyncupCntlrRequest` carries `created` too (SH8: apply,
      then persist). Between a td's materialization and the flip's re-sync the
      stored copy still says `false`, which is harmless — the devices exist, so
      nothing messages — and is corrected by the bump's higher-revision
      request.

      A td leaving `td_list` is **deleted**: remove its
      namespaces'/raid0/error devices (they reference it), remove the thin
      volume devices, then `delete {dev_id}` message per slice pool.

      The message is sent only while this cntlr holds the pool
      (`plan.wantPool`, and the device present): a standby has no pool
      device and only the primary may write pool metadata. The fan-outs
      where that skips every cntlr (demote+delete coalesced into one
      revision, a delete at a pool-suppressing `sp_level`, a
      failover+delete race) are healed by the **activation sweep**:
      creating a slice's pool device arms the sweep, and the first converge
      whose request arrived by the revision-gated RPC then enumerates the
      pool's device ids through the CN25 reserve → `thin_dump` → release
      machinery and deletes every id not in `td_list`'s dev_ids — correct
      because the RPC request is the newest accepted desired state, dev_ids
      are never reused and thin ids belong to tds alone. An agent restart
      under a surviving pool device does not sweep (nothing else writes the
      pool, and the CN2 zero-mutation reconcile stays intact), and a
      startup reconcile that *re-creates* the device (a node reboot) arms
      but does not run it: it converges from the persisted request, which
      may lag the pool's true contents (the converge runs before the
      persist and a failed persist is only logged), and deleting against a
      lagging `td_list` would destroy a live td — the first revision-gated
      `SyncupCntlr` after boot runs the sweep instead. A sweep failure is
      logged and retried on later converges until it succeeds once; a stray
      surviving a crash between creation and sweep, or a failed `delete`
      message, is collected at the pool's next rebuild.

      A cntlr teardown (CN21) instead only **deactivates** — it removes the
      devices and sends no `delete` message, because the pool metadata
      lives on the DN legs and the next hosting CN must find the thin
      volumes intact.

CN15. **Per-td devices** (`td.go`; primary builds both, standby only the
      error): the raid0 `CnRaid0Name` (dm-striped across the td's per-slice
      thin volumes in `slice_idx` order, chunk `stripe_size / 512`) and the
      permanent reload target `CnErrorName`, both `td.size / 512` sectors.

CN16. **Namespaces and host-facing nvmet** (`td.go`, `plan.go`). Per
      `Namespace` of every `Subsystem` in `nqn_to_subsystem`, the
      namespace's own dm-linear `CnNsDevName(cluster, cn, sp, ns_id)`
      (§3.3 step 5) with a **backing state machine**, evaluated in this
      order (first match wins; `sp_level` per CN19):
      0. the td is provisioning-deferred (CN9, [D15] — a side under it is
         still zeroing) ⇒ table → the td's `CnErrorName` (the ns-dev and
         the nvmet namespace exist throughout, [D15] — this is the "rule 0"
         the code comments cite);
      1. standby or disabled cntlr ⇒ table → the td's `CnErrorName`;
      2. `sp_level ≥ SP_LEVEL_NO_THINPOOL` ⇒ → `CnErrorName`;
      3. a clone targets the td (`clone_list` entry with
         `dst_td_id == ns.td_id`) and `sp_level ≥ SP_LEVEL_NO_CLONE` ⇒ →
         `CnErrorName` (a raid0 with holes must never serve);
      4. a clone targets the td ⇒ → `CnCloneFinalName` (via dm-flakey when
         rule 6 applies);
      5. otherwise ⇒ → `CnRaid0Name` (via dm-flakey when rule 6 applies);
      6. `SP_LEVEL_READONLY ≤ sp_level` (primary only): the table is the
         Appendix A flakey `error_writes` line over the rule-4/5 backing —
         reads pass, writes error ([D11]).

      Rules 3-4 and the `auto_resume` override below combine into one flip
      worth stating out loud. An `auto_resume` clone's
      destination namespace is stored `suspended = true`, which on its own
      would leave it `inaccessible` and its host **queueing**; the override
      makes it serve, so *below* `SP_LEVEL_NO_CLONE` it is already
      `optimized` over `CnCloneFinalName`. At `sp_level ≥ SP_LEVEL_NO_CLONE`
      the override still applies (it keys on the `clone_list` entry, not on
      whether the clone stack was built), so the namespace stays exported and
      **stays `optimized`** while rule 3 parks its ns-dev on `CnErrorName`:
      what changes is the backing, and the host therefore takes **IO errors**
      instead of queueing. That is deliberate and consistent with the
      documented `SP_LEVEL_NO_THINPOOL` posture (CN19: "a host sees IO
      errors, not a vanished device"); it is not a bug. "Queueing
      (inaccessible)" describes only the stored-`suspended` baseline, never
      the state the override leaves behind.

      **Effective suspend** (§11.6, §8.10, §8.9): a namespace is
      effectively suspended iff `ns.suspended` **or** an `xfer_list` entry
      with `auto_suspend` names it (`ori_nqn`/`ori_ns_idx`) — **unless** a
      `clone_list` entry with `auto_resume` targets its td, which overrides
      to not-suspended. (That override is the §11.3 flow: the destination
      namespace is *created* `suspended = true` and serves anyway while the
      clone runs; `DeleteClone` flips the stored field to `false`.)
      Suspending: ns → `AnaGrpIdInaccessible` first, then `dmsetup suspend`
      the ns-dev (its table stays the rule-1-6 backing). Resuming: resume,
      then ANA per the rule below. This is the third and last deliberate
      suspension in dnv; every teardown path resumes (via reload onto
      dm-error) before it disables or removes anything above (CN21).
      **Residual ([D12])**: a transfer
      origin's ns-dev suspension is bounded only by the transfer's own
      lifetime — **unbounded** in agent terms — and any external scanner that
      touches the suspended device (udev, `blkid`, an operator's `lsblk` or
      backup tool) blocks in uninterruptible D state until it resumes; a
      transfer hydrating a large td holds that state for hours, so operators
      SHOULD keep block-device scanners away from dnv devices on CNs while
      transfers run. The *agent's* own exposure ended with [D14]: no LVM
      label scan runs on a CN any more. A bounded alternative — a grace
      window, then a reload onto the td's `CnErrorName`, hosts queueing
      against the `inaccessible` ANA state as they already do — was
      considered and left undecided. Recorded as a known residual; no
      mechanism change was decided.
      `UpdateNamespaceDev` arrives as a changed `ns.td_id` and is exactly
      one ns-dev reload — the nvmet `device_path` never changes.

      **nvmet objects** on the node port: per `Subsystem` a subsystem with
      `attr_cntlid_min/max` from `Cntlr.cntlid_slot` (`CnCntlidSlot*`,
      §11.8), `attr_serial`/`attr_model` verbatim from the record (the
      gateway stamped [D2]), `attr_allow_any_host = 1` iff `allowed_hosts`
      is empty, else host links exactly per the list; per `Namespace` an
      nvmet namespace `nsid = ns_idx`, `device_path` = its own ns-dev,
      `uuid`/`nguid` from the record. **ANA**: `AnaGrpIdOptimized` iff
      primary ∧ not disabled ∧ not effectively suspended **∧ its backing
      chain is not provisioning-deferred** (CN9); else
      `AnaGrpIdInaccessible` (single `ana_grpid` writes, SH19). The last
      conjunct is what makes initial provisioning
      painless for hosts: while the td's legs are still zeroing, the
      namespace stays `inaccessible` and hosts **queue** on the path instead
      of eating IO errors from an error-backed ns-dev; it flips to
      `optimized` on the converge that follows the last leg's provisioning. A
      deferred namespace's own rows (`ns_id_to_dm_linear`,
      `ns_id_to_namespace`) report `RES_STATUS_PROVISIONING` — the ns-dev and
      the nvmet namespace do exist, on the permanent dm-error, but reporting
      them `OK` would show a fully healthy `CntlrInfo` while no host can do
      IO.

CN17. **Transfers** (`xfer.go`; both roles, fig. `100Transfer`). Per
      `xfer_list` entry: resolve the origin namespace by
      `ori_nqn`/`ori_ns_idx` in `nqn_to_subsystem` (unresolvable ⇒ the
      xfer's resources report `RES_STATUS_ERROR` and the pass continues,
      CN29). `CnXferFinalName`: on the primary a dm-linear over the origin
      td's raid0 (→ dm-error at `sp_level ≥ SP_LEVEL_NO_THINPOOL`); on a
      standby a plain dm-error table of the same size. Subsystem `XferNqn`
      on the node port: `attr_serial = %016x(xfer_id)`, `attr_model =
      "dnv"` (the [D2] rule applied to the xfer — every cntlr exports the
      same identity so the destination clone sees one multipath device),
      `attr_cntlid_min/max` from `Cntlr.cntlid_slot`, `allowed_hosts` from
      the record; one namespace `nsid = ori_ns_idx`, `uuid`/`nguid` = the
      origin namespace's, `device_path` = the xfer device; ANA optimized on
      the primary, inaccessible on standbys. The origin namespace's own
      retirement is CN16's effective-suspend rule. A cntlr that keeps the
      transfer device but stops serving it — demoted to standby, or
      `SP_LEVEL_NO_THINPOOL ≤ sp_level < SP_LEVEL_DISABLE`, or the origin td
      provisioning-deferred — reloads the live `CnXferFinalName` onto an
      error table of its own size before the retire touches anything under
      it, because a linear still mapping the origin td's raid0 holds it open
      and the raid0's removal would fail EBUSY (CN19's `NO_THINPOOL` row). At
      `SP_LEVEL_DISABLE` there is nothing to demote: the transfer device is
      removed outright with every other cntlr-scoped object (CN19's `DISABLE`
      row, the CN21 order). When the *new* plan cannot size that table — the
      origin namespace is not in it at all, or it is but its td left
      `td_list` in the same request, which leaves the namespace's size
      unknown — the size comes from the **previous** plan's transfer instead;
      without that fallback the demotion would be skipped and the departing
      raid0 never released. A transfer whose origin td is
      provisioning-deferred (CN9) is deferred with it: its three rows report
      `RES_STATUS_PROVISIONING` and its namespace stays
      `AnaGrpIdInaccessible` — mapping a linear over a raid0 that does not
      exist yet would only produce an `ERROR` and an `err_epoch`.

CN18. **Clones** (`clone.go`; primary only, fig. `090Clone`,
      `sp_level < SP_LEVEL_NO_CLONE`). Per `clone_list` entry, in order:
      1. `nvme connect` to `src_nqn` at **every** entry of
         `src_tr_conf_list` (one subsystem, one path per source cntlr; the
         source's own ANA picks the serving path), hostnqn `CnHostNqn`,
         SH20 flags; failures go to the CN10 retry registry. The source
         namespace device is found via sysfs like CN10, by `src_nqn` +
         `nsid = src_ns_idx`.
      2. Allocate the clone's metadata slot from the §2.1 arena:
         `ceil((4 MiB + region_cnt bytes) / CnCloneMetaUnit)` **contiguous**
         units with `region_cnt = td.size / block_size` (one byte per region
         over the dm-clone superblock is a generous bound — the dn agent
         budgets metadata the same way), first-fit over the free ranges of
         the used map reconstructed from the kind-`b` wrapper tables (CN5).
         Exhaustion of the 256-unit arena ⇒ `clone_id_to_meta` reports
         `RES_STATUS_ERROR` with the allocator's message and
         `clone_id_to_dm_clone` reports `RES_STATUS_ERROR`
         `"metadata wrapper missing"` — the old `lvcreate`-ENOSPC pair.
         **The arena is per CN, and it is the real clone ceiling.**
         `CnTmpFilePath` and `CnCloneMetaDmPrefix` are keyed by
         `(cluster_id, cn_id)`, so those 256 units are shared by every clone
         of every one of the node's `MaxCntlrCntPerCn` = 256 cntlrs. The
         ceiling is `256 / units_per_clone` **summed over the whole CN**: at
         the 2-unit floor (the 4 MiB base term is exactly one unit, so every
         real clone costs ≥ 2) that is **128 concurrent clones per CN**, and
         far fewer for large tds at small block sizes — a 1 TiB td at
         `MinDmPoolDataBlockSize` = 64 KiB has `region_cnt` = 16777216, i.e.
         5 units, so 51 such clones fill the arena. `MaxCloneCntPerSp` = 64
         is a per-SP limit and bounds none of this: two fully cloned SPs on
         one CN already sit at the 128-clone floor, and one SP alone can
         exhaust the arena well under its 64. Nothing gates clone count
         against arena capacity — the agent reports exhaustion, it does not
         prevent it — so the control plane must place clones against the
         per-CN budget, not against `MaxCloneCntPerSp`.
         Enumerate → discard → create is **one critical section** under the
         §4.2 `cloneMetaMu`: two cntlrs of the same CN converge concurrently
         under the node *read* lock, and two enumerations could otherwise
         pick the same free run and — because the two `dmsetup create`s use
         different names — silently share one metadata range. **Removing** a
         kind-`b` wrapper belongs in that same critical section, for the same
         reason: the registry being the kernel's dm tables, a removal is a
         mutation of it — the units are free the moment the table is gone —
         and a retire that deleted a wrapper in the middle of another cntlr's
         enumerate → discard → create would both invalidate that enumeration
         and free a run under it. So the paths that remove a wrapper from
         *outside* the allocator — the clone teardown below and the CN21
         cntlr teardown — take `cloneMetaMu` around that `dmsetup remove`,
         while the two that already hold it (this allocation's
         mismatched-wrapper removal, and CN2's arena sweep) remove directly:
         the mutex is a leaf, held across those OS calls and never
         re-entered.
         **Before** creating a *newly chosen* range, punch it on the loop
         device: `blkdiscard --offset {off} --length {len} {loopdev}`. That
         is the recycled-unit guard, because a freed unit still holds the
         previous clone's *valid* dm-clone superblock, which a new dm-clone
         would misparse. On a tmpfs-backed file a hole punch is
         zero-guaranteed by file semantics (no device DLFEAT involved) and
         frees the pages; `--zeroout` is **forbidden** here — it would
         materialize up to the whole 1 GiB arena in RAM and defeat the
         sparse-file design. A wrapper that already exists and matches is
         never re-discarded: "allocate" strictly means "a new unit range was
         chosen", and re-punching would wipe a live superblock. Then
         `dmsetup create` the wrapper
         `CnCloneMetaDmName(cluster, cn, sp_id, clone_id)` with the table
         `0 {units × CnCloneMetaUnit / 512} linear {loop maj:min}
         {off / 512}` (the backing device is written as its major:minor, and
         read back the same way from `dmsetup table`);
         the dm-clone's metadata device is that wrapper, because dm-clone
         reads its superblock from sector 0 and takes no offset (the same
         reason `DnMigrMetaDmName` exists). A wrapper whose table no longer
         matches (wrong length, or backed by something other than the
         **currently probed** loop path — a tmpfs remounted under a live
         agent) is removed and reallocated by the converge, after the
         dm-clone above it is already gone so the removal cannot EBUSY; that
         *is* the §11.5 rebuild path.
      3. dm-clone `CnCloneFinalName`: metadata = the step-2 wrapper, dest = the dst
         td's `CnRaid0Name`, source = the connected device, region size =
         `block_size / 512` sectors, created
         `2 no_hydration no_discard_passdown` **always** — the second
         feature is not optional: `blkdiscard` is this design's
         *metadata-only* "mark hydrated" primitive (§9.6, §11.4), and with
         passdown enabled dm-clone also remaps the discard to the
         destination, unmapping the very blocks step 4 says are already
         there;
         then `hydration_threshold`/`hydration_batch_size` messages from
         `dm_clone_conf`.
      4. Apply every locally present bitmap chunk (CN22 math) — and, when
         this build is a **§11.5 recovery** (the dm-clone's metadata is
         missing or unusable: a CN reboot takes the tmpfs, the loop device
         and every kind-`b` wrapper together; a failover to a CN that never
         ran the clone; or the dm-clone device itself vanished while a
         healthy wrapper stayed behind —
         `TestCloneRecoveryWhenOnlyTheDmCloneVanished`), first the **dst**
         bitmaps: with every affected ns-dev still parked
         on `CnErrorName` (retire phase / initial state — nothing serves
         the td yet), read the td's mapping bitmap from every slice pool
         (the CN25 machinery, B-side of §11.4) and `blkdiscard` every
         mapped region. Mapped ⇔ already copied holds because the dst td
         started empty [D3].
      5. `dmsetup message … enable_hydration` — only after step 4, so a
         recovered clone can never re-fetch a region the destination
         already owns (§11.5's staleness hazard).
      6. Put the dst td's ns-devs onto the dm-clone and set ANA per CN16 —
         a reload when they already exist (recovery, enable transitions); a
         fresh converge simply creates them in CN16 with the clone backing
         (rule 4). With `auto_resume = false` the namespaces stay
         effectively suspended until `UpdateNamespaceSuspended`.
      Teardown of a clone (left `clone_list`, or role/level-suppressed),
      strictly: ns-devs back onto whatever CN16 now wants for the td — the
      raid0 while this cntlr still serves it (rule 5), its `CnErrorName`
      otherwise (a standby; a still-listed but level-suppressed clone at
      `sp_level ≥ NO_CLONE`, rule 3; or `sp_level ≥ NO_THINPOOL`, rule 2 —
      a clone *gone from the list* parks nothing by itself) → remove the dm-clone
      (**before** its source connection dies — dm-clone flushes through the
      source on removal and blocks without it) → remove the metadata wrapper
      `CnCloneMetaDmName` (its units are free again the moment the wrapper is
      gone: the next registry enumeration simply no longer sees them, and
      nothing is written) → disconnect the source subsystem (`--nqn` is safe
      here: every path of it is being retired) → and, **only for a clone that
      actually left `clone_list`**, delete the clone's `clone-bm-*` files
      (SH7): a clone the role or the level merely suppresses keeps them
      applied-by-file (CN19, CN22 — deleting them on every standby converge
      would make the worker re-push them forever, and a promoted standby's
      §11.5 rebuild would have nothing to skip with;
      `TestSuppressedCloneKeepsItsChunks`). A clone whose dst td is
      provisioning-deferred (CN9) is never built in the first place: no
      metadata slot, no dm-clone, no source connect, and its three rows report
      `RES_STATUS_PROVISIONING`.

CN19. **`sp_level` gating** (§11.7; numeric comparisons — the enum values
      are ordered). Levels are desired state: raising tears layers down
      (retire phase), lowering rebuilds them (build phase); bitmap chunks
      stay applied-by-file throughout (SH21). A resource suppressed by the
      level is reported `RES_STATUS_MISSING` with `details = "sp_level"`. A
      resource *deferred* by CN9's provisioning gate is a different thing —
      `RES_STATUS_PROVISIONING` with `details = "provisioning"`, at every
      level — and where both apply, the level wins (CN9).

| condition | additional cn behavior |
|---|---|
| `level >= SP_LEVEL_READONLY` (16) | every user-facing ns-dev on the primary carries the dm-flakey `error_writes` table over its normal backing (CN16 rule 6) — reads served, writes error ([D11]); clone/migration hydration, transfers, health probes all unaffected |
| `level >= SP_LEVEL_NO_CLONE` (32) | no clone stacks (dm-clone, metadata wrapper — its arena units freed — and source connection all absent); ns-devs of clone-target tds on `CnErrorName` (CN16 rule 3), so an `auto_resume` clone's destination namespace stays exported and `optimized` and its host takes IO errors instead of queueing (CN16) |
| `level >= SP_LEVEL_NO_THINPOOL` (48) | no thin pools, thin volumes, raid0s or pool concats; every ns-dev and every primary xfer device on `CnErrorName`/error tables; namespaces stay exported with CN16 ANA (a host sees IO errors, not a vanished device) |
| `level >= SP_LEVEL_NO_REDUND` (64) | no group devices (md arrays stopped, `CnGrpName` linears removed); legs stay connected, wrapped and probed |
| `level >= SP_LEVEL_NO_MIGRATION` (80) | nothing — the level has no CN-side behavior (the migration dm-clone is a DN object; the CN's leg multipath needs no gating) |
| `level >= SP_LEVEL_NO_SIDE` (96) | no leg connections or wrappers (their sides are no longer exported) and no health probes; host-facing and xfer subsystems remain, error-backed |
| `level >= SP_LEVEL_DISABLE` (112) | only the §3.2 base state remains (tmpfs, backing file, loop device, port — every kind-`b` wrapper went with its clone, so the whole arena is free); every cntlr-scoped object is gone. The `cntlr-*`/`clone-bm-*` files stay — desired state persists |

CN20. Persist (SH5); reply `agent_reply`, `revision`, `cntlr_info`,
      `bm_info_list` — one `BitmapInfo{res_id = clone_id}` per `clone_list`
      entry of the (now-stored) request, `bm_idx_list` derived from the
      `clone-bm-*` files present (SH21).

### 4.7 Cntlr teardown

CN21. Used by CN7 (pointer removed), CN2 (orphan file) and CN19's
      `SP_LEVEL_DISABLE`. Strictly top-down, resuming every suspended
      ns-dev by reloading it onto its `CnErrorName` **before** anything
      else (a suspended device blocks both the nvmet disable above it and
      its own removal): host-facing and xfer nvmet objects (port link, ns
      disable, rmdir ns, allowed-hosts unlink, rmdir subsystem — nvmet
      must release the dm devices first); ns-devs; xfer finals; dm-clones,
      then their metadata wrappers, then their source disconnects (the CN18
      order — deliberately *not* the leg order below, because a dm-clone
      flushes through its source on removal); raid0s and per-td errors; thin
      volume devices (no `delete`
      messages — CN14: this is deactivation, the metadata on the legs is
      the next CN's to find; a created td's volumes are re-attached there
      without any message, `ThinDeviceCreated.md` U4); pools; concats; `mdadm --stop` /
      `CnGrpName` removal; then the legs, in the one order that is
      deliberately **not** top-down: **cancel the probers, then disconnect
      the legs** (whole-NQN is fine here), **then remove the leg wrappers**.
      A wedged prober holds an open fd on the wrapper, so removing the
      wrapper first fails EBUSY; the disconnect errors the queued IO, the
      prober's fd closes, and the removal then succeeds (§2.2, CN11).
      (The connect-retry registration is cancelled earlier in the sequence —
      right after the ns-dev and xfer finals, before the clone retire steps;
      the placement cannot race the legs, because the retry loop needs the
      node read lock this teardown's write lock excludes and re-checks the
      cntlr pointer.) Then the file deletions of SH7.

### 4.8 `PushCloneBitmap`

CN22. Gate: the cntlr file must exist and its stored `clone_list` must
      contain `clone_id`, and `bm_idx` must be `< MaxCloneBmCnt` and
      `< that clone's src_slice_cnt` (`ReplyCodeUnknownObject` otherwise);
      revision gate (SH8) against the stored cntlr revision (a push never
      updates the stored revision). Then SH21 with the clone twists of
      §9.6: persist the chunk at
      `LocalCloneBmPath(cluster, cn, sp, clone_id, bm_idx)` —
      **overwriting** when the payload differs, because clone chunks may
      grow ([D8]) — then recompute and apply. Clone chunks are
      self-positioned per source slice (bm_idx = slice_idx) and carry raid0
      geometry: `thinbm.go` folds the present chunks into one device-order
      wire bitmap over the dm-clone's regions — region `r` is skippable iff
      **every** source bit it covers under the §11.4 address mapping
      (`src_slice_cnt`, `src_stripe_size`, `src_block_size`, region size =
      `block_size`) is present **and** set; a bit whose chunk is absent
      counts as written. The folded bitmap feeds the shared
      `agent.SkipRanges` with `shiftRegions = 0` (clones have no meta
      region — that shift is the dn's) and is `blkdiscard`ed onto
      `CnCloneFinalName`. If the dm-clone does not currently exist (standby,
      not built yet, level-suppressed), the file still counts as applied;
      chunks are re-applied whenever the dm-clone is (re)created (CN18 step
      4). A **failed persist** is logged and still acked code 0, with neither
      the fold nor the `blkdiscard` attempted. For an index the node does
      not hold yet that heals itself: the chunk stays out of the applied
      set, so it stays out of the clone's `BitmapInfo.bm_idx_list` (CN20)
      and the worker's BM2 diff pushes it again. That diff is the whole
      recovery, and it is eventual rather than prompt: a code-0 ack leaves
      the worker's BM6 flag down, and `bm_info_list` rides no `CheckCntlr`
      reply, so the re-push waits for whatever next makes the worker issue a
      `SyncupCntlr` (RW4 step 5) — a revision change, a round this cntlr
      rejects or answers with another revision, or another chunk's push
      failure raising BM6. A failed persist of a **grown** chunk ([D8],
      architecture.md §9.6) is the one case that does not heal that way: the
      index is already in the applied set with the shorter payload, so
      `bm_idx_list` keeps advertising it, and the code-0 ack also refreshes
      the worker's BM5 memo — the node keeps the shorter version until
      `AppendCloneBitmap` grows that chunk again, the same bounded,
      correctness-neutral loss [D8] already accepts. Reply `agent_reply`
      only.

### 4.9 `GetCnInfo` / `GetCntlrInfo`

CN23. Read-only: probe fresh under the CN1 locks and reply `agent_reply`,
      `revision`, the info. An unknown CN (no `cn-*` file) or cntlr pointer
      ⇒ `ReplyCodeUnknownObject` with `revision = 0`. Never mutates.

### 4.10 `CheckCn` / `CheckCntlr`

CN24. Instantiate the SH24-SH26 loop with the §4.12 probes; one round takes
      the CN1 locks of the corresponding `Get*Info`.

### 4.11 `GetThinDeviceBm` / `GetLegBm`

CN25. Both serve the §8.13 gateway reads from a **dm-thin metadata
      snapshot** and report failure only through the gRPC status (§9.1 —
      they have no `AgentReply`): unknown cntlr, non-primary role, a
      missing pool, or a failing command all end the RPC with an `Internal`
      status naming the step. Under the CN1 locks (node read + object —
      the snapshot must not race a converge):
      `dmsetup message {CnPoolFinalName} 0 reserve_metadata_snap`, then
      `thin_dump --metadata-snap {DmPath(CnPoolMetaName)}` (the
      `thin-provisioning-tools` reader; XML on stdout), then **always**
      `release_metadata_snap` — on the success path and on every error
      path, because a leaked reservation blocks the next reserve; a
      reserve that fails "already reserved" is released and retried once.
      The §7 command timeouts bound the dump; a pool whose metadata
      outgrows what `thin_dump` emits inside `CmdSoftTimeout` fails the
      RPC, and the caller falls back to a full copy — bitmaps are an
      optimization, never a correctness input (§8.9). The activation sweep
      of CN14 shares this reservation machinery and its `CmdSoftTimeout`
      bound — metadata too large to dump inside the bound fails the sweep
      the same way it fails a bitmap read, and the sweep retries on later
      converges.

CN26. `GetThinDeviceBm`: from the `slice_idx` pool's dump, the mapping
      bitmap of the td's thin volume (`dev_id` looked up in the stored
      `td_list`) over virtual blocks
      `[start_block, start_block + block_cnt)` (`block_cnt = 0` ⇒ through
      `td.size / slice_cnt / block_size`); reply bit *k* = **1 iff
      unmapped** — thin metadata answers mapped = written and this boundary
      inverts exactly once (§11.4). **Wire encoding
      (normative)**: bits are LSB-first within each byte — bit *k* is
      `bitmap[k/8] & (1 << (k%8))` — the reply is `ceil(bit_cnt / 8)` bytes
      and every trailing pad bit is 0. `agent/bitmap.go` states that
      convention for the whole agent (SH22); the reply itself is built by
      `agent/cnagent/thinbm.go`'s own bit helpers, which is also where the
      inversion above happens — and it is what `cnagent_integtest.md`
      asserts in hex.

CN27. `GetLegBm`: locate the leg's group and slice in the stored
      `id_to_slice`. A **meta**-group leg replies all-zero (nothing
      skippable): the pool metadata device's own utilization is not
      derivable from thin mappings, and over-copying is always safe. A
      **data**-group leg: the group's span within `CnPoolDataName` starts
      at the summed `data_blocks` of the preceding `data_grp_list` groups;
      leg data-region block *k* ⇔ pool-data block `span_start + k`; bit *k*
      = **1 iff no thin device of the pool maps that pool-data block**
      (every leg of the group mirrors it, so the leg identity beyond its
      group never enters the math). Windowed by
      `[start_block, start_block + block_cnt)` over the group's
      `data_blocks`, `block_cnt = 0` ⇒ to the end. Same wire encoding as
      CN26 (LSB-first, zero-padded); the CN22 clone-chunk fold uses it too.
      The span is summed over the **effective** `data_grp_list` (CN9), and a
      leg whose group is not in that effective prefix fails the RPC with an
      `Internal` status naming it — the provisioning leg's own group, and
      equally a group *after* a deferred one, since deferral is a prefix cut
      (CN9): neither is a concat target, so neither has a span. Answering
      anyway would report one group's bitmap under another group's offsets
      and return silently wrong data.

### 4.12 Probing and error capture — `probe.go`

CN28. Probe map (SH17 conventions plus the cn probes fixed here: `findmnt`
      for mounts, `stat --format %s` for plain files,
      `losetup --associated` for loops, `dmsetup ls` + `dmsetup table` for
      the clone-metadata arena and every other dm device, `mdadm --detail`
      for arrays, and the §5 **sysfs walk** — never `nvme list-subsys` — for
      every outbound nvme connection). Every row reports against the
      **effective** desired state (CN9): a resource the provisioning gate
      excluded is `RES_STATUS_PROVISIONING` with `details = "provisioning"`,
      never `RES_STATUS_ERROR`, and it never feeds `err_epoch`.

| `ResInfo` | `res_name` | probe |
|---|---|---|
| `CnInfo.port_info` | `"{NvmetPortId}"` | configfs `addr_*` reads match the `--tr-*` flags; the three [D4] groups present with their fixed states |
| `CnInfo.tmpfs_info` | the `CnTmpfsPath` | `findmnt` shows a tmpfs mounted there |
| `CnInfo.tmp_file_info` | the `CnTmpFilePath` | `stat` size = `CnCloneMetaAreaSize` |
| `CnInfo.loop_dev_info` | the `CnTmpFilePath` | `losetup --associated` lists exactly one loop device — this row covers the whole arena; `CnInfo.clone_vg_info` (field 5) is deleted with the clone VG (`reserved 5;`, [D14]) and per-clone metadata health lives in `clone_id_to_meta` |
| `ss_id_to_subsystem[ss]` | the subsystem NQN | configfs: present, cntlid range, serial/model (trimmed, SH17), allowed-hosts exactly as desired |
| `ns_id_to_namespace[ns]` | `"{nqn}/{ns_idx}"` | nvmet ns enabled, `device_path`, `uuid`/`nguid` (compared through `agent.SameNsId` — configfs reads both back dash-separated and lower-cased whichever form was written, so a byte-wise compare fails a healthy namespace forever; `dnagent.md` SH17), `ana_grpid` as CN16 desires — `3` while the backing chain is provisioning-deferred (CN9), and the row itself is `RES_STATUS_PROVISIONING` then |
| `ns_id_to_dm_linear[ns]` | `CnNsDevName` | `dmsetup table` matches the CN16 backing (flakey line included); an effectively suspended ns-dev is expected suspended (`dmsetup info`) and reports `RES_STATUS_OK`, `details = "suspended"`; a provisioning-deferred one reports `RES_STATUS_PROVISIONING` over its permanent dm-error |
| `td_id_to_raid0[td]` / `td_id_to_dm_error[td]` | `CnRaid0Name` / `CnErrorName` | `dmsetup table` |
| `td_id_to_thin_info[td].slice_id_to_dm_thin[slice]` | `CnThinDevName` | `dmsetup table` (pool + dev_id) |
| `slice_id_to_dm_pool[slice]` | `CnPoolFinalName` | `dmsetup status`; `details` = the **raw status line** — the worker parses data and metadata used/total out of it for the §10.4 auto-grow. The serving pool stays `RES_STATUS_OK` with that raw line even while a deferred group waits to be grown in (CN13): `PROVISIONING` never marks the serving pool, because it would switch auto-grow off |
| `slice_id_to_meta[slice]` / `slice_id_to_data[slice]` | `CnPoolMetaName` / `CnPoolDataName` | multi-target `dmsetup table` matches the group concat; the comparison is against the **effective** concat (the list's leading run of non-deferred groups, CN9/CN13), so a not-yet-grown concat is `OK`, not a mismatch. A **deferred** slice's rows are `RES_STATUS_PROVISIONING` — deferred meaning either of its two group lists is non-empty and has no effective group left (CN9), not that every group is deferred |
| `grp_id_to_md_raid[grp]` | `/dev/md/{CnMdDevName}` or `CnGrpName` | RedundMdRaid1: `mdadm --detail` — active (degraded included) ⇒ OK with the state/rebuild line in `details`; RedundNone: `dmsetup table`. A deferred group (CN9) reports `RES_STATUS_PROVISIONING` and no mdadm command runs |
| `leg_id_to_leg[leg]` | `CnLegName` | wrapper table + the CN11 prober outcome (primary) / transport per desired side, from sysfs, plus `ana_state` in {`optimized`, `non-optimized`} on single-sided legs — two-sided legs liveness only (CN11) (standby; §5). A provisioning leg (non-empty `side_list`, every side `provisioned = false`, CN9) reports `RES_STATUS_PROVISIONING` and is neither connected, wrapped nor probed |
| `xfer_id_to_dm_linear[x]` / `xfer_id_to_subsystem[x]` / `xfer_id_to_namespace[x]` | `CnXferFinalName` / the `XferNqn` / `"{XferNqn}/{ori_ns_idx}"` | `dmsetup table` / configfs, per CN17; a deferred transfer's three rows are `RES_STATUS_PROVISIONING` |
| `clone_id_to_target[c]` | the clone `src_nqn` | the §5 **sysfs walk** shows a live controller per `src_tr_conf_list` entry (match `/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` to `src_nqn`, then `/sys/class/nvme/{ctrl}/state`) — **not** `nvme list-subsys -o json`, which §5 already ruled out for CN12 and which the code never used here |
| `clone_id_to_dm_clone[c]` | `CnCloneFinalName` | `dmsetup status`; `details` carries the raw status line (§9.5 — hydration progress; `DeleteClone`'s force check reads it). `RES_STATUS_ERROR` `"metadata wrapper missing"` when the arena could not supply the slot (CN18 step 2) |
| `clone_id_to_meta[c]` | `CnCloneMetaDmName` | `dmsetup table` of the kind-`b` wrapper: present, length = the CN18-computed unit count × `CnCloneMetaUnit` / 512 sectors, and the table's backing device equals the **currently probed** loop path; any mismatch (e.g. a tmpfs remounted under a live agent) ⇒ `RES_STATUS_ERROR`, whose repair path is the §11.5 clone rebuild (CN18 step 2) |

CN29. Error capture (§9.1): a failed command marks that resource
      `RES_STATUS_ERROR` with the command output in `details` and the
      converge pass **continues** with the remaining resources; protocol
      failures are the only things reported through `agent_reply`. Probes
      never mutate — `reserve_metadata_snap` runs only inside CN25, never
      from `probe.go`. `RES_STATUS_PROVISIONING` is never produced by this
      path: it is assigned by the CN9 gate, not by a failed command.

## 5. Amendments applied to companion documents

Recorded for traceability; the edits are already applied. Bullets tagged
"design-review U*n*" apply the September 2026 design-review decisions
(items U1–U5 of that pass's since-retired ledger; a second pass added
U1–U7); they supersede anything earlier in this list that
contradicts them.

* `osclient.md` §4.5.1 (design-review U2) — `ReadBlockDirect` is **removed**
  from the `OsClient` interface, `LimitedOsClient` and `FakeOsClient`; the raw
  helpers are exported instead as `common.WriteBlockAt` /
  `common.ReadBlockDirectAt`, and the recorded carve-out says probe IO is the
  one sanctioned direct-syscall path: it may block indefinitely by design and
  must never run under a lock or through the 32-slot semaphore. The
  `os read block direct` logging row is replaced by the prober-emitted
  `probe write block` / `probe read block direct` records (§2.2, CN11). This
  **supersedes** the original amendment, which *added* `ReadBlockDirect` to
  the interface for the [D6] read-back; the O_DIRECT requirement on the
  read-back itself is unchanged.
* `dnagent.md` §2.8 SH20 — `nvmehost.go` gains `DisconnectDevice`
  (`nvme disconnect --device`): the two sides of a migrating leg share one
  NQN, so the cn agent must be able to disconnect a single dead path
  (CN10) without killing the live one. The controller device is located by
  the §5 sysfs walk (`/sys/class/nvme/{ctrl}/address` parsed as `key=value`
  pairs), never by `nvme list-subsys` (design-review U5).
* `dnagent.md` §3 CM2 — the flag table gains the cn-only `--capacity` row
  (§3 above); `GetCnSize` replies it verbatim.
* `architecture.md` §3.6 + §9.5 — the [D6] health-**block** probe is run by
  the **primary** cntlr only: a standby's path deliberately terminates in
  the side's dm-error (§3.1), so block IO through it can never succeed and
  "including standby cntlrs" was unimplementable as written. Standbys (and
  spare legs seen from them) report transport liveness + expected
  ana_state instead (CN11); the failover-readiness signal survives, the
  false-positive `Leg.err_epoch` storm does not.
* `architecture.md` §3.2 step 4 + §9.3 — QoS enforcement is explicitly
  deferred: until the recorded open issue is decided, `SyncupCn` accepts
  and persists `qos_ratio` and programs no limit (CN6). The previous §9.3
  wording ("apply the §3.2 step 4 QoS limits") described a mechanism the
  same section documents as non-binding for host IO.
* `layout.md` — `doc/` tree lists this document; the `agent/cnagent/`
  recommended file split updated to §4.1 (adds `plan.go`, `lvm.go`,
  `thinbm.go`). `lvm.go` is the pre-design-review name: the design-review U3 bullet below
  renames it `clonemeta.go` when LVM leaves the CN, and §4.1 lists the
  current split.
* `cnagent.md` CN12/CN28 — leg availability and the standby leg report are
  probed from **sysfs**, not from `nvme list-subsys`. Measured: `nvme
  list-subsys -o json` emits no `ANAState` unless it is given a namespace
  block device, and with one it answers an all-`inaccessible` namespace with
  an *empty* subsystem list — indistinguishable from "not connected". So the
  §11.1.1 test cannot be evaluated from that command at all. The walk is:
  `/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` to match the NQN; the
  `nvme<N>` entries inside are controllers and the `nvme<N>n<M>` entries the
  multipath namespace; then `/sys/class/nvme/{ctrl}/{address,state}` and
  `/sys/class/nvme/{ctrl}/nvme*c*n*/ana_state` (ANA state exists **only**
  per path, never under the multipath head). Availability is
  `state == "live" && ana_state == "optimized"`: a `connecting` path keeps
  its last-known ANA state. `clone_id_to_target` uses the same walk.
* `cnagent.md` CN18 step 3 — the dm-clone's two feature args are spelled out
  as `no_hydration no_discard_passdown` (Appendix A's `2 no_hydration …`).
  dm-clone enables discard passdown whenever the destination's discard
  granularity is no larger than a region — which a raid0 over dm-thin
  volumes always satisfies — and then remaps a discard to the destination as
  well as marking the region hydrated. That would make the §11.5 dst-bitmap
  discards unmap exactly the regions the destination already owns, and would
  destroy any host write that landed on a hydrated region while `auto_resume`
  let the namespace serve. `agent.CloneTable` gained the corresponding
  parameter and **both** call sites now pass `noDiscardPassdown = true`
  (design-review U1): `2 no_hydration no_discard_passdown` is mandatory on
  **every** dnv dm-clone — the cn clone and the dn migration clone alike —
  because `blkdiscard` must stay a metadata-only "mark this region hydrated"
  primitive on both (§9.6, §11.4, §11.5, [D7]). The dn hazard is *after* the
  §11.2 cutover, not before it: host IO flows through the dst dm-clone, a host
  write hydrates region *r*, and a migration skip-bitmap chunk whose bit for
  *r* predates that write arrives later (chunk pushes are legal at any time
  and a restart re-applies every stored chunk), so the agent `blkdiscard`s
  *r* — with passdown that discard would reach the dst side device and destroy
  an acknowledged write. This replaces the earlier "the dn migration keeps the
  default it was validated with".
* `common/constants.go` / `common/name_fmt.go` (design-review U3) — the §2.1
  additions (`DefaultCnTmpfsSize`, `CnLegProbe*`, `LegHealth*`,
  `CnConnectRetryInterval`, `CnCloneMetaAreaSize`, `CnCloneMetaUnit`;
  `CnLegName`, `CnGrpName`, `CnCloneMetaDmName`, `CnCloneMetaDmPrefix`) land
  with the implementation, exactly as dnagent.md §2.2's additions did.
  `DefaultCloneVgSize` is renamed `CnCloneMetaAreaSize`;
  `DefaultCloneVgPrefix`, `DefaultCloneVgExtSize`, `CnCloneVgName`,
  `CnCloneMetaName` and `CnCloneMetaPath` are deleted.
* `cnagent.md` §2.2 / §4.1 / §4.2 / CN1 / CN11 / CN21 / §6 test 15 / §7 items
  5 and 7 (design-review U2) — the CN11 leg probers leave the `OsClient`:
  direct syscalls through the fakeable `LegProbeIO`, self-logged as
  `probe write block` / `probe read block direct`, no semaphore slot. A probe
  of a pathless leg queues IO forever by design, and holding one of
  `DefaultOsClientLimit` = 32 slots across that syscall could starve the whole
  node — including the teardown `nvme disconnect` that is the documented
  release mechanism. The CN21 leg teardown order therefore **flips** to cancel
  probers → leg disconnects → wrapper removal: a wedged probe's open fd makes
  an earlier `dmsetup remove` fail EBUSY. (The doc previously listed prober
  cancellation *after* the disconnects; the code always cancelled first.)
  §6 test 15 now spells out how the headline property is actually asserted —
  a converge that completes while a prober is parked mid-`Write` — and marks
  the semaphore half as structural and single flight as a property of
  `legProbeLoop`'s inline round, rather than implying an assertion for each.
* `cnagent.md` §1 / §2.1 / §4.1 / §4.2 / CN2 / CN5 / CN18 / CN19 / CN21 /
  CN28 / §6 tests 1, 3 and 9 / §7 items 3 and 6 (design-review U3) — **LVM
  leaves the CN.** The clone VG is replaced by a slot allocator over the
  single loop device: dm kind `b` (`CnCloneMetaDmName` →
  `dnv-{cluster}-{cn}-b-{sp}-{clone}`), `CnCloneMetaUnit`-sized contiguous
  units out of a `CnCloneMetaAreaSize` arena, a plain `blkdiscard` hole punch
  as the recycled-unit guard (never `--zeroout` — it would materialize the
  arena in RAM), and the kernel's own kind-`b` dm tables as the allocation
  registry. Rationale is the [D13](a) failure class: bare `vgs`/`lvs`
  label-scan every block device on the node, and on a CN that includes
  suspended transfer-origin ns-devs, which wedge LVM in unkillable D state.
  `lvm.go` becomes `clonemeta.go`; `CnInfo.clone_vg_info` (field 5) is deleted
  (`reserved 5;`); `architecture.md` records it as [D14]. CN18 step 2 also
  records the ceiling the arena implies: it is one **per-CN** 256-unit arena,
  so at the 2-unit floor a CN holds at most 128 concurrent clones summed over
  every cntlr on it (fewer for large tds at small block sizes), a bound
  `MaxCloneCntPerSp` = 64 neither expresses nor protects.
* `cnagent.md` CN9 / CN10 / CN11 / CN12 / CN13 / CN16 / CN17 / CN18 / CN19 /
  CN27 / CN28 / CN29 / §6 / §7 (design-review U4) — **side provisioning.**
  Every side is fully zeroed (`blkdiscard --zeroout`, per-extent
  `zeroed_bits`) before its first export, gated by `Side.provisioned`
  (`dnagent.md` DN9). The cn therefore converges and probes an **effective**
  desired state and reports the excluded resources `RES_STATUS_PROVISIONING`
  (new `ResStatus` value 4 — healthy, not ready, never an `err_epoch`).
  CN12's and CN13's "provably all-zero via the §9.4 trim" claims are replaced
  by the zeroout guarantee ([D15]: discard-reads-zeros is not a hardware
  guarantee, and dnv is multi-tenant), and CN16's ANA rule gains the
  not-provisioning-deferred conjunct so hosts queue instead of taking IO
  errors during initial provisioning. The serving pool's `dm_pool` row
  deliberately stays `OK` with its raw status line. CN9/CN12/CN13/CN28 state
  the two shape rules exactly as the plan implements them: a group list
  contributes its **leading run** of non-deferred groups to the concat (a
  prefix truncation — filtering a middle group out would re-base every later
  target and move live pool-data blocks when it cleared), and a slice is
  deferred whole when **either** of its two group lists is non-empty in the
  raw desired state and has no effective group left, because an empty concat
  is an error (`"concat has no segments"`) and not a shape.
* `cnagent.md` §2.3 / §4.2 / CN16 / CN19 / CN26 / CN27 / CN28
  (design-review U5) — consistency fixes: `DisconnectDevice` and the
  `clone_id_to_target` probe are the sysfs walk, not `nvme list-subsys -o
  json` (which is what the code always did); the LSB-first, zero-padded wire
  bitmap encoding is pinned in CN26/CN27; the §4.2 struct sketch lists `oc`,
  `cmd`, `md`, the probe-IO and clone-metadata fields so it matches the real
  struct; the `SP_LEVEL_NO_CLONE` queue→error flip of an `auto_resume`
  destination namespace is stated explicitly (the namespace stays `optimized`
  either way — only its backing flips to `CnErrorName`); and the [D12]
  residual (an unbounded transfer-origin suspension blocks external scanners
  in D state, while the agent's own exposure ended with [D14]) is recorded —
  note only, no mechanism change.
* `cnagent.md` CN14 + CN16 (second-pass U1/U5) — CN14 gains the
  cross-slice point-in-time rule for `create_snap`: quiesce the origin td's
  raid0 around the whole per-slice message sequence (implemented; §6 test
  item 19 carries the assertions);
  CN16's [D12] residual paragraph now states the transfer-origin
  suspension's operational blast radius and the considered-but-undecided
  bounded alternative (reload onto `CnErrorName` after a grace window).
* `architecture.md` §2 / §3.3 / §8.7 / §9.5 / §10.3 / Appendix C / Appendix D
  (`ThinDeviceCreated.md` U1-U4) — the `created` flag and its gateway and
  worker rules: `CreateThinDevice` refuses a snapshot of an uncreated origin,
  `DeleteThinDevice` refuses an origin with an uncreated snapshot,
  `ListThinDevices` is the client's wait primitive, and the sp-worker flips
  the flag from the thin rows of any cntlr reply. This document's CN14 is the
  agent-side spec; §8.7/§10.3 are the gateway/worker spec until `gateway/`
  and `worker/` land.

## 6. Tests

Unit tests use `common.FakeOsClient` (scripted fns recording every call) —
no root, no real devices. RPC-level tests call the `CnAgentServer` methods
in-process instead of dialing a `bufconn` client: everything these tests
assert (the CN1 lock mapping, the recorded OS calls, the in-memory mirrors)
lives below the generated stubs, and a real client would only add a
transport that `common`'s own interceptor tests already cover. The
`CheckCn`/`CheckCntlr` tests go one level further down still: rather than
supply a server-stream argument they call the unexported per-round helpers
(`checkCnRound`/`checkCntlrRound`) and thread `lastSent` by hand, because
every CN24 assertion — the reply codes, the rule for when the info rides
along, the CN1 locks — lives in the round, while the `Recv`/`Send` loop
around it is the SH24-SH26 shape with nothing cn-specific in it.

1. **Fresh SyncupCn**: scripted empty probes; assert the CN5 sequence
   (`findmnt`/`mkdir`/`mount`, `truncate --size {CnCloneMetaAreaSize}`,
   `losetup --associated` then `losetup --find --show`, then the port attrs,
   `mkdir ana_groups/2`+`3`, the three
   one-time `ana_state` writes) and the `WriteProto` to `LocalCnPath`
   afterwards (SH5). No `io.max` write and no cgroup path appears anywhere
   in the recorded calls (CN6), and **no LVM command appears at all** — the
   arena needs none (CN18).
2. **Revision gate**: lower ⇒ `ReplyCodeStaleRevision` and zero mutating
   calls; equal ⇒ full idempotent pass; higher ⇒ apply + persist. Same for
   `SyncupCntlr`; `SyncupCntlr` for a pointer `SyncupCn` has not introduced
   ⇒ `ReplyCodeUnknownObject`.
3. **Probe-first idempotency** (SH16): equal-revision `SyncupCntlr` against
   probes reporting a fully converged primary issues no mutating command —
   no dm/md/nvme/configfs mutation, no `ana_grpid` write.
4. **Standby converge**: legs connected + wrappers built + per-td errors +
   ns-devs on error + subsystems with every ns `ana_grpid = 3`; **no**
   mdadm, pool, thin, raid0 or clone command appears.
5. **Primary converge order**: one fresh primary pass (a RedundNone
   fixture) asserts the CN9 build order — connects → wrapper creates →
   RedundNone group linears (`CnGrpName`) → stdin multi-target concat
   creates → pool create →
   `create_thin` messages (for `created = false` tds only) → thin creates →
   raid0/error → ns-dev → nvmet
   objects → `ana_grpid = 1` writes last. (The md-raid1 create order and
   flags are pinned by tests 6-7's RedundMdRaid1 fixtures.)
6. **Failover**: re-sync to `primary = false` asserts the CN9 retire order —
   every ns `ana_grpid = 3` **before** the ns-dev reloads onto error,
   before pool/thin/raid0 removal and `mdadm --stop`, with **no**
   `nvme disconnect` of a leg; re-sync back to primary rebuilds via
   `mdadm --assemble` (superblocks present — CN12 case 2), never
   `--create`.
7. **§11.1.1 / member reconciliation**: scripted `--examine`/`--detail`
   outcomes drive: no superblocks ⇒ create+assume-clean; one ⇒ assemble +
   add; both-with-one-left-out ⇒ assemble + re-add; single available leg ⇒
   assemble, and a scripted mdadm refusal leaves the group
   `RES_STATUS_ERROR`; a `SwitchSpareLeg`-shaped request (`leg_list`
   swapped with a spare) ⇒ `--fail` + `--remove` + `--add --failfast`, and
   never `--zero-superblock`.
8. **Namespace states** (CN16): `suspended = true` ⇒ `ana_grpid = 3` write
   then `dmsetup suspend`, resume path reversed; an `auto_suspend` transfer
   retires its origin the same way; an `auto_resume` clone overrides a
   `suspended = true` dst namespace to serving; a `td_id` repoint is
   exactly one ns-dev reload; `sp_level = SP_LEVEL_READONLY` reloads
   user-facing ns-devs onto the flakey `error_writes` table and nothing
   else.
9. **Clone build and §11.5 recovery** (CN18): fresh build asserts
   connect(s) → the arena enumeration (`dmsetup ls` + `dmsetup table` of the
   kind-`b` wrappers) → `blkdiscard --offset … --length …` of exactly the
   allocated range on the loop device (**never** `--zeroout`) →
   `dmsetup create` of `CnCloneMetaDmName` with the computed linear table →
   clone table with `2 no_hydration no_discard_passdown` →
   knob messages → chunk `blkdiscard`s → `enable_hydration` → ns-dev
   reload; a rebuild with the metadata wrapper scripted absent additionally
   asserts the dst-bitmap reads (`reserve_metadata_snap` → `thin_dump` →
   `release_metadata_snap`) and their `blkdiscard`s **before**
   `enable_hydration`, with the ns-devs parked on error throughout; a clone
   allocated after another was torn down reuses the freed units and
   re-discards them first, while a surviving matching wrapper is **never**
   re-discarded; an arena with no contiguous free run of the required size ⇒
   `clone_id_to_meta` and `clone_id_to_dm_clone` both `RES_STATUS_ERROR`;
   teardown asserts ns-dev reload → clone remove → **wrapper remove** →
   disconnect, in that order.
10. **PushCloneBitmap**: persist-before-apply call order; unknown
    `clone_id` and out-of-range `bm_idx` rejected; a grown chunk overwrites
    the file and re-applies; the applied set after a simulated restart
    (fresh server, same fake store) matches; a chunk without a live
    dm-clone still counts applied; the geometry fold treats an absent
    slice's bits as written (a region overlapping a missing chunk is never
    discarded).
11. **sp_level ladder** (CN19): each level asserts exactly its row —
    `NO_THINPOOL` keeps namespaces exported on error backing;
    `NO_MIGRATION` is a no-op relative to `NO_REDUND`; `NO_SIDE` drops leg
    connections; `DISABLE` leaves only base state and keeps the store
    files; lowering rebuilds.
12. **Check streams** (CN24): first reply full info; unchanged
    `show_info = false` round omits it; a flipped probe re-includes it;
    unknown object ⇒ code 2 with the stream kept open; a round never
    mutates.
13. **Lock smoke** (CN1): a `SyncupCntlr` blocked in a slow scripted
    command blocks a same-cntlr `SyncupCntlr` but not a `CheckCn` round or
    another cntlr's converge.
14. **Thin bitmaps** (CN25-CN27): scripted `thin_dump` XML drives: reserve
    → dump → release ordering, release also on a scripted dump failure and
    after an "already reserved" retry; the wire inversion (mapped ⇒ 0);
    paging windows; the meta-group all-zero rule; the data-group span
    arithmetic against a two-data-group slice.
15. **Leg prober** (CN11): registry logic under a fake clock with a **fake
    `LegProbeIO`** (§2.2 — the prober never touches the server's `oc`: a
    round records no `writeblock`/`readblockdirect` `OsClient` call at all),
    stall reporting after `CnLegProbeStallSeconds`, `Write` offset =
    `meta_blocks × block_size − 4096`, `ReadDirect` read-back of the same
    range, primary-only (a standby converge starts no prober), and — the
    central carve-out property — a probe scripted to **block indefinitely** does not
    delay a concurrent converge: with a prober parked inside its `Write`
    half, a full `SyncupCntlr` on the same server completes inside a deadline
    and is seen driving the node (its `dmsetup` calls are recorded) while
    that probe's round is still unfinished; releasing the block then lets the
    round complete. That is the half a unit test can observe — CN1's "no lock
    is ever held across probe IO", the property a wedged pathless leg
    (`ctrl_loss_tmo = -1`, IO queued forever) makes an ordinary failure mode
    rather than an exotic one. The other half — the `DefaultOsClientLimit`
    slot the probe never takes — is structural, since `LegProbeIO` is not an
    `OsClient` at all, and is pinned by §7 acceptance 5 plus that
    no-`OsClient`-call assertion. Single flight needs no assertion of its
    own: `legProbeLoop` runs each round inline on its ticker, so a blocked
    round can only delay the next one. The teardown ordering assertion is
    explicit: the leg's `nvme disconnect` is recorded **before** the
    `dmsetup remove` of its wrapper (CN21).
16. **cmd**: the §13 example `dnv-agent cn …` invocation parses; `--disk`
    is rejected for `cn`; `--capacity` reaches `GetCnSize` verbatim; env
    `DNV_AGENT_CAPACITY` overrides the flag default (CM3).
17. **Provisioning deferral** (CN9/CN10/CN12/CN13/CN16): a group with one
    unprovisioned leg produces **no** `nvme connect` for that leg, no mdadm
    command, no concat-growth reload and no pool resize, and reports
    `RES_STATUS_PROVISIONING` for the leg and the group; the serving pool
    stays `RES_STATUS_OK` at the effective size throughout a deferred
    `GrowSlice`; every namespace whose td is backed by a deferred group is
    written `ana_grpid = 3`; an unprovisioned **spare** leg defers only itself
    (the array still assembles); and a leg with one provisioned and one
    unprovisioned side still connects the provisioned side and serves.
18. **`PROVISIONING` has no error semantics** (CN9/CN29): no deferred
    resource is ever `RES_STATUS_ERROR` and no `agent_reply` failure is
    produced; a `CheckCntlr` stream over a fully provisioning cntlr is stable
    (the constant `"provisioning"` details keeps `proto.Equal` suppressing
    repeats, SH26) and mutates nothing; a resource that is both
    level-suppressed and deferred reports `MISSING`/`"sp_level"`.
19. **Snapshot point-in-time** (CN14, amended by
    `ThinDeviceCreated.md` U4): on a primary with 2+ slices, a live origin td
    and an absent snapshot td, a converge records
    `dmsetup suspend {origin CnRaid0Name}` strictly before the first
    `create_snap`, then every needed slice's `create_snap` — each still
    bracketed by its own per-slice origin-thin suspend/resume — before
    `dmsetup resume {origin CnRaid0Name}`, and the snap thin devices'
    `dmsetup create` strictly after that resume. A scripted failure of the
    second slice's `create_snap` still records the raid0 resume: no path out
    of the sequence leaves a device suspended ([D12]). An equal-revision
    re-apply with every snap thin device already present records no suspend
    and no message at all (SH16). A plain `create_thin` td never triggers a
    raid0 suspend at all. The **fresh-primary** shape (U4-T1): an origin
    already `created` whose devices this cntlr has just lost to a CN21
    teardown, plus an uncreated snapshot of it — every slice records
    `create_snap` exactly once, no `dmsetup suspend` at all (nothing is live
    to quiesce), no `create_thin` for the created origin, and both tds' thin
    devices are created and report `OK`; the same `td_list` in the reverse
    order records the same messages, the same absence of suspends and the
    same device creations, because `td_list` order carries no meaning. (Not
    a literal call-for-call multiset: the fake hands out minor numbers in
    creation order, so the raid0 tables cite different ones.) With one slice deferred and one ready, the ready
    slice's `create_snap` still goes out — the gate is pool readiness, never
    the plan-global `deferred` flag — and a live origin raid0 is quiesced
    around it even then, since the pre-pass no longer returns early for a
    deferred origin. Each slice records **exactly one** `create_snap`, none
    of them after the raid0 resume, even when a slice's message failed:
    `ensureThin` never messages a td with `ori_id != 0`, so nothing can
    re-send it outside the quiesce (a count is what catches this — every
    ordering assertion stops at the first match).
20. **A created td is never messaged** (CN14, U4-T2): a plain td with
    `created = true` on a fresh cntlr whose pool already holds its `dev_id`
    records `dmsetup create` of the thin device, no `create_thin`, and a row
    of `RES_STATUS_OK`; an equal-revision re-apply records no mutation
    beyond persisting the request (SH16).
21. **A created snapshot is never messaged** (CN14, U4-T3): origin and
    snapshot both `created` on a fresh cntlr whose pool holds both ids
    records no `create_snap`, no `dmsetup suspend`, both tds' devices created
    and both rows `OK`. Identical with the origin absent from `td_list` —
    deleted after the snapshot's flip — because there is nothing to message
    and nothing to quiesce either way.
22. **A created td whose id the pool lost is an error** (CN14, U4-T4): the
    same td on a pool that does **not** hold its `dev_id` records no message
    at all; the `dmsetup create` fails and the row reads `RES_STATUS_ERROR`
    carrying the node's own output. A second converge at the same revision
    sends no message either and the row stays `ERROR` — a created td is never
    re-created by message, so pool-metadata loss does not self-heal into an
    empty volume.
23. **An uncreated snapshot retries when the origin id is missing** (CN14,
    U4-T5, R11): `td_list` holding only a snapshot whose `ori_id` no pool
    holds records the `create_snap` unquiesced, the `dmsetup create` behind
    it fails, the row reads `ERROR`, and the next converge sends the message
    again — the td is still `created = false`.
24. **A standby leg's `ana_state`** (CN11/CN28,
    `TestTransportHealthAnaState`): a table over `transportHealth`. On a leg
    with **one** desired side, a live controller reading `optimized` or
    `non-optimized` ⇒ `RES_STATUS_OK` (`non-optimized` is the designed
    steady state, so this is the case that must not regress), while
    `inaccessible` or a state the DN never sets (`change`) ⇒
    `RES_STATUS_ERROR` with `"unpromotable"` in `details`. On a leg with
    **two** sides (a migration, CN10) an `inaccessible` side is still `OK`:
    ANA is not judged there at all. The pre-existing rows are unchanged — a
    non-`live` controller and a missing controller are `ERROR` whatever the
    ana_state — and no `OK` row ever calls a path unpromotable.
25. **The thin-id activation sweep** (CN14/CN25;
    `agent/cnagent/sweep_test.go`, scripted `thin_dump` through
    `RunCommandFn` exactly as test 14 does, pool messages asserted by
    `callsMatching` counts). Six cases:
    * `TestRetireSkipsThinDeleteWithoutPool` — the long-missing CN14 pin and
      the leak the sweep heals: a primary converge holding td X, then one converge
      that both demotes to standby and drops X ⇒ **zero** `delete` messages
      and the pool devices removed, with X's dev_id left charged against the
      pool metadata on the legs.
    * `TestActivationSweepDeletesStrays` — a standby→primary converge takes
      the pool Create branch; the scripted dump lists {stray S, the live
      td's dev_id Y} ⇒ exactly one `delete S`, zero `delete Y`,
      `reserve_metadata_snap` before `thin_dump --metadata-snap` before
      `release_metadata_snap` and no leaked reservation. A second identical
      converge probe-matches the device and runs **no** `thin_dump` (the
      count stays 1): once per pool-device creation, not per converge.
    * `TestRestartDoesNotSweep` — a reconcile over an already-existing pool
      device (probe-matched, no Create) ⇒ zero `thin_dump`, zero
      `reserve_metadata_snap`, and a baited stray still in the pool: the
      case-D zero-mutation invariant (CN2/SH16) survives.
    * `TestSweepFailureRetries` — a scripted `thin_dump` error ⇒ no deletes
      and converge rows unaffected (the pool row still `OK`); the next
      converge scripts success and the sweep completes.
    * `TestSweepSurvivesDeleteFailure` — a scripted dump with a stray whose
      `delete` message fails ⇒ the slice stays armed and the next converge
      retries and succeeds.
    * `TestStartupReconcileDefersSweep` — the data-loss pin: a persisted
      primary request whose `td_list` holds only td A, no pool device (the
      reboot shape), and a dump listing {A, B, stray S}. `Reconcile` creates
      the device and runs **zero** `reserve_metadata_snap`/`thin_dump` calls
      and zero `delete`s (armed, not run); a later `SyncupCntlr` at a higher
      revision whose `td_list` holds A and B then runs exactly one
      `thin_dump`, exactly one `delete S`, and **no** `delete` for B — the
      id the stale persisted copy had forgotten.
26. **A removed namespace is parked before its nvmet objects** (CN9/CN21,
    `TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval`): a primary
    serving one `suspended = true` namespace, then a converge that drops it
    from `ns_list`. The ns-dev's reload onto the td's `CnErrorName` and the
    resume inside it are recorded **before** that nsid's `enable = 0` and
    `rmdir`, which are recorded before the ns-dev's own `dmsetup remove`; and
    exactly **one** park of that ns-dev, since an ordering assertion stops at
    its first match and cannot see a second one. The count is over
    `parkNsDev`'s own `dmsetup table` probe, which every call makes before it
    decides anything — counting *reloads* would prove nothing, because
    `parkNsDev` is idempotent (already linear over the `CnErrorName` and
    resumed returns before the reload) and so a repeat park emits no `dmsetup`
    command at all. A second sub-case drops the whole subsystem and asserts
    the same park-first order around `RemoveSubsystem`.
27. **A zero conf member is refused** (CN8, `dnagent.md` §2.1): a
    `SyncupCntlr` whose `bdev_conf` carries a zero `data_block_size`,
    `low_water_mark_pct` or `stripe_size`, or a zero
    `bitmap_chunk_block_cnt` under an md-raid1 `redund_conf`, replies
    `ReplyCodeInvalidConf` with the **stored** revision, records **zero**
    mutating calls (no `dmsetup`, `mdadm`, `nvme` or configfs write) and no
    `WriteProto` to `LocalCntlrPath`, and leaves the applied plan of the
    previous revision in place. A conf whose `redund_conf` selected
    `redund_none` is **accepted** with the other three members concrete,
    because the chunk count is a field of the md-raid1 arm and does not
    exist at all on the other. A `Reconcile` over a persisted request
    carrying such a zero converges nothing for that cntlr and records the
    same refusal without touching the applied plan. The four messages are
    asserted verbatim; they are the same literals `model/capacity_test.go`
    asserts for `model.ValidateBdevConf`, and the two assertions together
    are what keep the two copies of the rule in step (`dnagent.md` §2.1).

## 7. Acceptance checklist

1. `go build ./...`, `go vet ./...`, `go test ./...` pass.
1b. `grep -rn "snapDone" agent/cnagent/` finds nothing;
    `grep -rn "createSnapId(" agent/cnagent/*.go` hits its definition and the
    snapshot pre-pass's call only; and every `s.dm.Message(` site in
    `agent/cnagent/pool.go` is either `create_thin`/`create_snap` reached only
    while `created == false` or the deliberately ungated `delete {dev_id}` of
    a td that left `td_list` (`ThinDeviceCreated.md` U4-T6).
2. `go list -deps ./cmd/dnv-agent | grep etcd` finds nothing (`layout.md`
   §3).
3. The §2 additions exist: the §2.1 constants in `common/constants.go`
   (including `CnCloneMetaAreaSize`/`CnCloneMetaUnit`, and **no**
   `DefaultCloneVg*`), `CnLegName`/`CnGrpName`/`CnCloneMetaDmName`/
   `CnCloneMetaDmPrefix` in `common/name_fmt.go` (and no `CnCloneVgName`/
   `CnCloneMetaName`/`CnCloneMetaPath`), the exported `common.WriteBlockAt` /
   `common.ReadBlockDirectAt` helpers with **no** `ReadBlockDirect` on
   `OsClient` or `FakeOsClient`, `DisconnectDevice` on `NvmeHost`. `common/`
   still contains exactly the six files of `layout.md` §2.
4. A repo-wide grep finds no `WriteFile(` call whose path argument is under
   `/sys/kernel/config` (SH18), no `ana_state` write outside `EnsurePort`,
   and no `io.max`/cgroup write anywhere (CN6).
5. `grep -rn "zero-superblock" agent/cnagent/` finds nothing outside
   comments (CN12); `grep -rn "s.oc" agent/cnagent/healthcheck.go` finds
   nothing, and `grep -rn "common.WriteBlockAt\|common.ReadBlockDirectAt"
   agent/cnagent/` hits only the `LegProbeIO` implementation — the CN11 probe
   IO never passes through the `OsClient` (§2.2).
6. `grep -rnE "pvcreate|vgcreate|lvcreate|lvchange|lvremove|\blvs\b|\bvgs\b|\bpvs\b" agent/ cmd/ common/`
   finds nothing outside comments — **repo-wide**: LVM is gone from dnv
   entirely ([D13]/[D14]), superseding the old cn-only exemption and
   dnagent.md acceptance 6. And `grep -rn "zeroout" agent/cnagent/` finds
   nothing: the clone-metadata arena uses a plain `blkdiscard` hole punch,
   while `--zeroout` belongs only to the dn side-provisioning path (CN18/§9.4).
7. A manual run of the §13 example starts `dnv-agent cn`, serves
   `GetCnSize`, and a `SyncupCn`/`SyncupCntlr`/`CheckCntlr` round-trip
   shows one trace id across its `grpc server request`, `os command` and
   `os write file direct` records; the `probe write block` / `probe read
   block direct` records of the CN11 leg probers appear on their own
   per-attempt trace ids (CN2), never on an RPC's — and they carry no
   `os command` framing, because the prober issues its IO directly (§2.2).
8. A `SyncupCntlr` whose legs are all `provisioned = false` issues zero
   `nvme connect`, `mdadm` and `dmsetup create` calls, and every affected
   `ResInfo` is `RES_STATUS_PROVISIONING` — never `RES_STATUS_ERROR`.
9. §5 records every change the design-review pass made.
10. Both greps of this item carry `--exclude=*_test.go`, because both are
    claims about what the shipped agent code contains and the tests of §6
    test 23 / test 27 and `agent/conf_test.go` deliberately quote the same
    strings back:
    `grep -rn --exclude=*_test.go "invalid stored conf" agent/` finds only
    `agent/conf.go`'s error builder (plus the doc comment above it) and the
    `msgInvalidStoredConf` constant in each role package — one string
    reaches every refusal, in both agents and in the worker. Each of the four
    `ValidateBdevConf` messages in `agent/conf.go` greps byte-identical out
    of `model/capacity.go` (`dnagent.md` §2.1), and
    `grep -rnE --exclude=*_test.go "DefaultDmPoolDataBlockSize|DefaultPoolLowWatermarkPct|DefaultDmRaid0StripeSize|DefaultChunkBlockCnt" agent/`
    finds nothing: the cn agent substitutes no conf default of its own
    (CN13).

### Integration-run fixes (first on-hardware run of the amended tree)

Found by running `integtest/dnagent_test.sh` and `integtest/cnagent_test.sh`
against two real VMs (kernel 7.0, nvme-cli 2.16, mdadm 4.5) — the first
execution of either suite since the design-review pass was applied. All five were
real agent defects, not harness problems; every one is now covered by a unit
test that fails without the fix.

* **IR5 (CN28)** — `ns_id_to_namespace`'s `uuid`/`nguid` probe compares through
  the shared `agent.SameNsId` rather than case-folding alone: nvmet reads both
  attributes back dash-separated, and the CP sends the nguid bare, so a healthy
  namespace was reported `RES_STATUS_ERROR` on every probe and every
  `CheckCntlr` round failed. `equalFoldHex` now delegates to that one function,
  shared with the dn role and with `nvmet.go`'s converge side.
* **IR1/IR2/IR3 (`dnagent.md` SH20)** — the leg connects this role issues inherit
  the corrected `--fast_io_fail_tmo` spelling, the explicit `--hostid`, and the
  sysfs-based `ListSubsys`; the cn's own leg probe already read sysfs (CN12), and
  the shared wrapper has now been brought to the same source of truth.
