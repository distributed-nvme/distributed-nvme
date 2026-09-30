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
of the §2.1 kind-`cb` wrapper linears. Agents never talk
to etcd (`layout.md` §3); acceptance re-checks it. The gRPC server carries
the **server** interceptors of `grpc.md` §4; unlike the dn role, the cn
agent's outbound connections are still only `nvme connect` — never gRPC.

## 2. Additions to the shared pieces

### 2.1 Additions to `common`

The following enter the existing files `common/constants.go` and
`common/name_fmt.go`; the name **parser** below is the one file `common`
gains, `common/name_parse.go`. The const listing is a condensed quote rather
than a byte-for-byte one: `CnCloneMetaAreaSize` and `CnCloneMetaUnit` sit
apart from the rest in `common/constants.go`'s single `const` block, the rest
following one another there in the listing's order, and some comments are
condensed or wrapped differently — the connect budget's, for one, leaves out
the limits the file's states. The names, the values and the rules the
comments state are the committed ones, and `common/constants.go` is
authoritative for comment text, wrapping and order (as `dnagent.md` §2.2 says
of its own listing):

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
	// kind-`cb` wrapper dm tables themselves. No LVM.
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

	// Seconds between background retries of a cn cntlr's converge
	// (cnagent.md CN10): after a leg or a clone source failed to converge, a
	// later step of a clone failed (CN18 says which) — a recovery's
	// destination bitmaps not applied among them — or a leg_list member that
	// is not available (CN12); the cn twin of DnMigrConnectRetryInterval.
	CnConnectRetryInterval = 5

	// The connect step's one wait budget per converge pass (cnagent.md CN10,
	// CN18), shared by every leg and clone source of the pass and drawn on
	// by exactly three things: a failed connect's own elapsed time, the
	// pause before each in-pass retry, and the steps of the wait for the
	// namespace head after a connect this pass made. time.Durations, unlike
	// the whole-second integers around them.
	CnConnectPassBudget = 1 * time.Second
	CnConnectRetryPause = 100 * time.Millisecond
	CnNsScanPause       = 50 * time.Millisecond

	// ResDetailsSpLevel is the details of a CntlrInfo row the sp_level
	// suppresses (cnagent.md CN19): RES_STATUS_MISSING that says the
	// resource must not exist, where every other MISSING the cn agent reports
	// says it does not exist yet. The cn agent writes it and the worker's
	// settle reads it (dnv-worker.md HL2), so the two take it from here.
	ResDetailsSpLevel = "sp_level"
```

`ResDetailsSpLevel` was *added 2026-09-26* (the failover ping-pong): CN19's
`"sp_level"` had been the cn agent's own constant, and the worker's settle
now tells a suppressed row from an unbuilt one by it, so a value that
drifted on one side would keep every primary whose level suppresses a row
the settle reads settling for as long as that level stands.

`CnCloneMetaAreaSize` is the rename of the old `DefaultCloneVgSize` (same
1 GiB); `DefaultCloneVgPrefix`, `DefaultCloneVgExtSize` and the
`NameFmt.cloneVgPrefix` field they fed are **deleted** ([D14]).

and in `common/name_fmt.go`, three new dm kinds `c9`, `ca` and `cb` in the CN
namespace of §4.1/§4.2 (methods on `NameFmt`, formats normative):

```go
// CnLegName is the cn-local leg wrapper of architecture.md §3.3 step 1
// ([D1]): one dm-linear over the leg's single nvme multipath namespace
// device, kept as the leg-level indirection point (what a teardown reloads
// onto an error target, and what md/groups consume as the member device).
func (nf *NameFmt) CnLegName(clusterId, cnId, spId, legId uint64) string
	// → dnv-{cluster}-{cn}-c9-{sp}-{leg}

// CnGrpName is a RedundNone group device (§3.3 step 2): a dm-linear over
// the single leg's data region. RedundMdRaid1 groups use the md names of
// §4.3 instead and have no dm name.
func (nf *NameFmt) CnGrpName(clusterId, cnId, spId, grpId uint64) string
	// → dnv-{cluster}-{cn}-ca-{sp}-{grp}

// CnCloneMetaDmName is the dm-clone metadata wrapper of CN18 ([D14]): a
// dm-linear over one contiguous run of CnCloneMetaUnit-sized units of the CN
// clone-metadata loop device. dm-clone reads its superblock from sector 0 and
// takes no offset argument, so every slot must be presented as a device of
// its own — the same reason DnMigrMetaDmName exists on the dn side.
// CnCloneMetaDmPrefix is the `dmsetup ls` filter that enumerates them, and it
// must come from NameFmt because the dm prefix is configurable.
func (nf *NameFmt) CnCloneMetaDmName(clusterId, cnId, spId, cloneId uint64) string
	// → dnv-{cluster}-{cn}-cb-{sp}-{clone}
func (nf *NameFmt) CnCloneMetaDmPrefix(clusterId, cnId uint64) string
	// → dnv-{cluster}-{cn}-cb-
```

[D1] left the leg wrapper "agent-internal"; fixing its name here makes it
observable (tests, `dmsetup ls`, cleanup by prefix) without making it part of
any cross-**component** contract — nothing outside the cn agent ever
addresses these three devices. For kind `cb` the name is more than
convenience: the kind-`cb` **tables are the allocation registry** (CN5, CN18),
so the prefix+kind filter of `dmsetup ls` must be unambiguous.
`architecture.md` §4.1 points here for the CN kinds `c9`/`ca`/`cb`.

**The kind field is a role letter plus the hex digit** — `common.DmKind`, a
string: `DmKindCnPoolMeta = "c0"` … `DmKindCnCloneMeta = "cb"` on this side,
`DmKindDnError = "d0"` … `DmKindDnMigrMeta = "d5"` on the dn's. The letter is
not decoration. cn ids and dn ids come from separate counters
(`CnGlobal.next_id`, `DnGlobal.next_id`) and can collide numerically, so on a
node running both agents — every lab VM — a bare digit left
`dnv-{cluster}-{node}-9-…` ambiguous between a cn leg wrapper and whatever dn
kind `9` might one day be. That ambiguity became load-bearing when CN21 made
removal name-driven: a sweep decides what to remove by reading back the names
the kernel hands it, and a name it decodes into the **wrong role** is a device
torn down by the wrong agent. The hazard is mis-attribution, not
non-attribution: a name that decodes to nothing at all is dropped by every
attribution path there is — a failed `ParseDmName` keeps the device out of the
snapshot's *ours* set, an md array with one such member is silently dropped
and never stopped, and `IsDnvNqn` holds the subsystem back — so it is a
device this agent can never sweep rather than one it might sweep by mistake
(`architecture.md` §4.1: "a name it cannot attribute is a device it cannot
sweep"). NQN kinds keep their bare digit (`:0:` … `:4:`) — an NQN
already carries the dnv prefix, and neither role enumerates the other's NQN
kinds.

`common/name_parse.go` is the reverse of `name_fmt.go` and is shared by both
roles: `ParseDmName(name) (DmName, bool)` → `{ClusterId, NodeId, Kind, Ids}`
with `Ids[0]` the sp id for every kind of either role, `ParseNqn(nqn)
(NqnParts, bool)` → the kind and its own id order (`:2:` = cluster, sp, leg,
cn; `:3:` = cluster, dn, sp, migr; `:4:` = cluster, sp, xfer), and
`IsDnvNqn`. Both parsers are **strict** — an unknown kind, an id count that
does not match the kind, an id that is not exactly 16 lower-case hex digits
all yield `false` — because a half-decoded name is one the caller would
remove. `IsDnvNqn` exists for the case strictness creates: an NQN that
carries the dnv prefix but decodes to nothing is **not** the user-chosen
host-facing NQN that a `false` from `ParseNqn` otherwise means, and CN21
leaves it alone rather than attributing it by its namespaces (a malformed
name must not get a subsystem deleted).

One existing `common/name_fmt.go` formatter changes **arity**: a clone bitmap
chunk is addressed by the PAIR `(src_slice_idx, bm_idx)` (architecture.md
§9.6), so `LocalCloneBmPath` carries two `%02x` segments where it used to
carry one, and that one used to be the source `slice_idx`:

```go
func (nf *NameFmt) LocalCloneBmPath(
	clusterId, cnId, spId, cloneId uint64,
	srcSliceIdx, bmIdx uint32,
) string
	// → {prefix}/clone-bm-{cluster:%016x}-{cn:%016x}-{sp:%016x}-{clone:%016x}-{slice:%02x}-{bm:%02x}
```

`LocalMigrBmPath` is untouched — migration chunks keep their flat `bm_idx`
(dnagent.md SH21-SH23). Both clone fields fit two hex digits:
`src_slice_idx < src_slice_cnt ≤ MaxSliceCntPerSp` = 32 (enforced by
`CreateClone`), and `bm_idx < MaxCloneBmCnt` = 16 — a constant that keeps its
name and value but now caps the chunks of **one** source slice's bitmap, each
of the fixed capacity `CloneBmChunkBytes` = 1 MiB (the constants themselves
live in `common/constants.go`; `gateway.md` §2.1 carries them). The file name
is only an address: the persisted `PushCloneBitmapRequest` **inside** the file
is what CN2 decodes the pair from.

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
	// ReadBlockDirectAt opens O_RDONLY | O_DIRECT | O_CLOEXEC with a
	// 4096-aligned buffer, so offset and length MUST be multiples of 4096
	// and a short read is an error.
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
field position; a controller whose `address` did not answer, or is absent,
is never taken for it (CN10). Recorded in §5.

## 3. `cmd/dnv-agent cn` (dnagent.md §3 amendment)

CN-CM1. The CM2 flag table gains one cn-only row (edit applied to
     `dnagent.md`):

| flag | dn | cn | default | meaning |
|---|---|---|---|---|
| `--capacity` | — | ✓ | 0 | capacity budget in bytes this CN is willing to host; `GetCnSize` replies it verbatim, `0` = "use the CP default" (`DefaultCnCap`, §6.1) |

     `--capacity` is the only cn-only flag, so this stays a one-row table.
     In particular `--nvmet-port-id` (`NvmetPortId` = 1 by default, env
     `DNV_AGENT_NVMET_PORT_ID`, refused below 1) is a **both-roles** row and
     lives in CM2 itself, not here: the cn resolves and floor-checks it
     exactly as the dn does (CM3), and a cn that keeps the default co-owns
     `ports/1` with a dn agent on the same node (`architecture.md` §3.2).
     The flag is not a licence to run two **cn** agents on one kernel: a
     distinct port id separates their ports and nothing else, while the
     host-facing subsystem NQN, `XferNqn` and `CnMdArrayName` carry no cn id
     and CN placement keeps two cntlrs of one SP off one kernel only while
     that kernel's cn agents share one `location`, and even then only at its
     tier 1 and not reliably between a failed cntlr and its automatic
     replacement — never under the default `location` (CM2's cn paragraph;
     `architecture.md` §3.2, §6.5).

CN-CM2. `runCn` mirrors `runDn` (CM4): bind viper, require the common
     values (`--capacity` is optional), resolve and floor-check
     `--nvmet-port-id` (CM3), `signal.NotifyContext`, construct
     `common.NewNameFmt(localStore)` and the process's single
     `common.NewLimitedOsClient(0)`, build
     `cnagent.NewCnAgentServer(oc, nf, localStore, capacity, trConf,
     portId)`, and call `agent.Serve` with the role's reconcile, a
     register func that
     calls `pb.RegisterControllerNodeAgentServer`, and `nil` for the SH27
     background waiter — the cn joins no background task: a sweep's
     background `nvme disconnect` (CN10) holds nothing a restarted agent
     needs, and its CN11 probers are cancelled at `rootCtx`, never joined
     (§2.2: a wedged direct read would hang shutdown forever). This
     replaces the placeholder `"the cn role is not implemented yet"` error.

## 4. The cn role — package `cnagent` [cn]

### 4.1 Files

`server.go` (the `CnAgentServer` type, lock mapping, and the RPC entry
points other than the check streams of `check.go` and the bitmap reads of
`bitmapread.go`),
`plan.go` (per-cntlr derived names/sizes, the ns-dev backing and ANA state
machines of CN16), `syncup_cn.go`, `syncup_cntlr.go`, `leg.go` (CN10 side
connections + wrappers), `healthcheck.go` (the CN11 [D6] probers and their
§2.2 `LegProbeIO` — direct syscalls, never the `OsClient`), `md.go`
(mdadm wrapper, the `/sys/block` reads of the arrays — CN12 — and the
§11.1.1 assembly cases), `clonemeta.go` (the §3.2
base-state wrappers — tmpfs mount, `truncate`, `losetup
--associated`/`--find --show` — plus the CN18 clone-metadata slot allocator:
unit accounting over the single loop device, the `blkdiscard` recycle guard,
the kind-`cb` wrapper linears and the `dmsetup ls`/`dmsetup table` enumeration
that **is** the allocation registry), `pool.go` (concats, thin pools, thin volumes), `td.go` (raid0,
dm-error, ns-dev, flakey), `clone.go` (CN18 + §11.5 recovery), `xfer.go`,
`push_clone_bm.go`, `sweep.go` (CN21: the two scopes' enumeration,
attribution, wanted set and layered removal — it is what replaced
`syncup_cntlr.go`'s retire phase and the old per-object teardown helpers),
`dmutil.go` (the dm ensure/probe helpers —
`ensureDmSingle`/`ensureDmError`/`ensureDmLinear`/`ensureDmMulti`,
`probeDmTarget`/`probeDmArgs`/`probeDmConcat`, `removeDm`/`removeExport` —
shared by the converge files, `probe.go`, `sweep.go` and `syncup_cntlr.go`'s
build phase; `ensureDmClone` stays in `clone.go`),
`thinbm.go` (CN25-CN27: the thin-metadata snapshot
reader behind `GetThinDeviceBm`/`GetLegBm` and the §11.4 clone-geometry
fold), `bitmapread.go` (the `GetThinDeviceBm`/`GetLegBm` RPC entry points),
`check.go`, `probe.go`. Colocated `_test.go` files. This is the
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
	port     agent.PortConf   // --tr-* + --nvmet-port-id
	// in-memory mirrors of the local store: cn requests, cntlr states
	// (applied request, ResInfo tracker, a per-clone agent.CloneChunkSet
	// keyed by (src_slice_idx, bm_idx), connect
	// retry registry, leg-prober registry), guarded by a leaf mutex, which
	// also guards the CN10 disconnect registry — in memory only, never
	// persisted. A buffered channel of disconnectConcurrency slots caps how
	// many of the registry's disconnects run at once.
	// rootCtx anchors the CN10/CN18 retry loops, the CN10 background
	// disconnects and the CN11 probers.
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
     the teardown `nvme disconnect` that releases it runnable. That
     disconnect takes no lock either: the sweep issues it through the CN10
     disconnect registry, and under its locks the pass only probes the
     connection.

### 4.3 Startup reconcile

CN2. Enumerate the store (SH6; cn kinds `cn-`, `cntlr-`, `clone-bm-`) and
     load every `cn-*` and `cntlr-*` request into memory first. Then reload
     every `clone-bm-*` chunk into the owning cntlr's `agent.CloneChunkSet`s
     (SH21), keyed by the `(src_slice_idx, bm_idx)` pair the chunk is
     addressed by — **before** any converge runs, so that a dm-clone the
     converge (re)builds re-applies in the same pass every chunk the node
     already holds (CN18 step 4). Owner *and* address come out of the file's
     **content**: the file is a persisted `PushCloneBitmapRequest`, and its
     `cluster_id`/`cn_id`/`cntlr_pointer`, its `clone_id` and its
     `src_slice_idx`/`bm_idx` are what the reload reads. The name carries the
     same pair (§2.1), but only as an address — nothing here parses it. A
     file that does not decode is logged and **skipped**; a skipped file is
     deliberately **not** an orphan and is not deleted — a decode failure
     says nothing about who owns it, so nothing here may conclude the owner
     is gone.

     A chunk file written by an **older** binary does NOT take that path, and
     the difference is worth stating because it is counter-intuitive.
     Deleting `revision` (field 4) shifted every later tag down one, so each
     tag in an old file is now read as the field that used to sit one tag
     higher. The owner *triple* survives —
     `cluster_id`/`cn_id`/`cntlr_pointer` (1, 2, 3) never moved — but
     `clone_id` does not. A **pre-pair** file (`revision` 4, `clone_id` 5,
     `bm_idx` 6, `bitmap` 7) decodes with no error and no unknown fields at
     all, because every wire type still matches, into `{clone_id: <the old
     revision>, src_slice_idx: <the old clone_id>, bm_idx: <the old bm_idx>,
     bitmap: <intact>}` — only the two leading scalars are displaced;
     `bm_idx` and `bitmap` land where they belong. A **post-pair,
     pre-deletion** file (`revision` 4, `clone_id` 5, `src_slice_idx` 6,
     `bm_idx` 7, `bitmap` 8) is displaced the same way at 4/5/6, and
     additionally loses its bitmap: field 7 is now `bytes` against the file's
     varint `bm_idx`, and field 8 no longer exists, so protobuf-go moves both
     to **unknown fields** rather than erroring. In both cases `clone_id`
     reads back as a revision number, so `findClone` on the owning cntlr's
     stored `clone_list` almost never matches and the file is deleted as an
     orphan by the rule below — it is not loaded, and no wrong chunk is ever
     installed. The residual risk is only the freak case where an old
     revision number happens to equal a live `clone_id`, which would install
     one chunk at a displaced pair. No toleration code exists anywhere, by
     design (there are no real users and binaries upgrade in lockstep); the
     operational answer is to clear the CN's `--local-store` when upgrading
     past this change. Loading them
     afterwards would lose none of them — a chunk in memory is advertised as
     applied by the next reply's `chunk_id_list`
     (CN20), so the worker's BM2 diff would not push it again — but their
     `blkdiscard`s would then wait for the next event that re-applies the
     whole set: a create of that dm-clone, a converge reload of its table
     onto a changed length, devno or region size (step 4 runs on both —
     the converge treats a reload exactly as a create), a §11.5 recovery
     (step 4 runs on every one, also over an old dm-clone its removal left
     in place), or another
     chunk's own push (CN22). Until then the clone re-copies regions it
     never needed to. A **decoded** chunk file whose cntlr is not among the
     loaded `cntlr-*` requests, or whose `clone_id` is absent from that cntlr's
     stored `clone_list`, is an orphan — its cntlr or clone was deleted
     while the chunk file survived (SH7) — and is deleted here, because
     it names an owner no later pass will ever look for. Then, for each
     `cn-*` request: re-run the SyncupCn converge (§4.5 step CN5). Then
     each `cntlr-*` request: if its pointer is absent from the stored
     `SyncupCnRequest.cntlr_pointer_list`, the cntlr is **dropped** — its
     `cntlr-*` and `clone-bm-*` files, its memory entry, its object lock and
     its goroutines go, and **nothing of its is removed from the node here**.
     Its resources are found afterwards, by name, by the node-level sweep
     (CN21) that runs next. That split is the point: the teardown this
     replaced deleted the same state *after* a best-effort removal pass whose
     every step only logged its failure, so a cntlr whose array would not stop
     was forgotten with its devices still live and nothing ever enumerated
     them again (`architecture.md` §9, teardown by sweep). Ids are never
     reused, so a dropped cntlr never comes back. Every other `cntlr-*`
     request: re-run the SyncupCntlr
     converge (§4.6) from the stored request — which, per CN18 step 4, runs
     the §11.5 recovery for a clone whose **dm-clone metadata** is missing
     or unusable, whose **dm-clone device** itself vanished while a
     healthy wrapper stayed behind
     (`TestCloneRecoveryWhenOnlyTheDmCloneVanished`), or whose dm-clone is
     up but does not show hydration enabled — what an agent killed between
     CN18 steps 3 and 5 leaves behind
     (`TestCloneRecoveryResumesAfterAKillBetweenCreateAndBitmaps`); a
     dm-clone that is present over a wrapper that still matches, with
     hydration enabled, but whose table has drifted
     — a changed length, devno or region size — is reloaded by step 3's
     probe-first converge (SH16) with step 4's locally held source chunks
     re-applied, and is not a §11.5 recovery; the same drifted table over a
     missing or mismatched wrapper is instead removed and rebuilt by the
     recovery, which never sees the drift.
     (A reboot
     clears the tmpfs, the loop device and every kind-`cb` wrapper together —
     the arena is volatile *with* the kernel's dm state — so the reconcile
     starts from an empty arena; a plain agent restart preserves both, and
     the converge is a no-op re-apply for a clone whose build had enabled
     hydration, while a build the dead agent left short of that runs the
     recovery again — a missing wrapper, a missing dm-clone and a dm-clone
     with hydration still disabled are each a recovery trigger).
     The pass order is therefore: load, then
     converge every `cn-*` base state, then drop the orphan cntlrs, then the
     node-level sweep per CN, then converge every remaining cntlr.

     **Sweeping the arena** is one step of that node-level sweep: a
     kind-`cb` wrapper (prefix `CnCloneMetaDmPrefix`) whose clone appears in
     no stored `cntlr-*` desired state is an orphan and is removed, which
     frees its units for the next allocation. It compares against a name set
     built from every stored cntlr's `clone_list` — the wrapper sweep needs no
     parse of its own — and it runs from the node-level sweep only, which is
     to say from here and from `SyncupCn` (CN7), because those are the only
     two places that hold the node write lock and see the complete stored
     desired state; a `SyncupCntlr` sees one cntlr. It runs **before** the
     cntlr converges, not after, and that is safe for the same reason the
     whole sweep is: the wanted set comes from the stored requests, never from
     what happens to exist, so a clone this pass is about to (re)build is
     already named by its cntlr's `clone_list` and is never mistaken for an
     orphan. All under the node write lock, with the SH2 trace id. Background
     retries (CN10/CN18) and probers (CN11) mint a fresh trace id per
     attempt. Whatever this pass could not remove is not remembered
     anywhere: the first `Check*` of the object that still holds it
     recomputes it and reports it (CN30).

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
       A `findmnt` that did not answer (`dnagent.md` SH15) — either of the
       two — is not "absent": the converge runs the probe once more, from
       its first call, in the same pass and acts on that answer, and when
       that one does not answer either, `tmpfs_info` is an `ERROR` naming
       the probe and nothing is mounted. A mount over the live arena would
       stack a second, empty tmpfs on it: the arena file drops out of sight,
       so the converge truncates a fresh one and attaches a second loop
       device to it, and every kind-`cb` wrapper still maps the first loop,
       so every clone on the CN is rebuilt (CN18 step 2). The price, when
       both askings go unanswered, is a tmpfs that really is absent — after
       a reboot, or on a new CN — and is not mounted by that pass. The two
       steps below then create nothing either: they create the file and the
       loop device only while `tmpfs_info` is `OK`, on a tmpfs this pass
       found or mounted at the path. Without one the mountpoint may be a
       bare directory — left in a `/tmp` that the reboot kept, or made by
       the `mkdir -p` of a `mount` that then failed — where a `truncate`
       would put the arena file on the filesystem that holds the directory
       and `losetup --find` attach the loop device to that file; the next
       converge's `mount` would hide that file and bring the cascade above:
       a fresh file, a second loop device, and every clone built on the
       first loop rebuilt. As after a failed `mount`, the arena then waits
       for the CN's next `SyncupCn` or the agent's next start: a check round
       reads what is absent `MISSING`, and the `CheckCn` verdict (CN30),
       which is the sweep's alone, stays clean for it, so it does not make
       the worker re-send `SyncupCn` — an open issue (§7, known limits).
     * **backing file** `CnTmpFilePath`: probe `stat --format %s`; absent ⇒
       `truncate --size {CnCloneMetaAreaSize} {path}` (sparse — tmpfs pages
       materialize only as clone metadata is written, and CN18's hole-punch
       `blkdiscard` frees them again), but only while `tmpfs_info` is `OK`
       (above); otherwise an absent file is an `ERROR` on `tmp_file_info`
       saying no tmpfs is confirmed, and a file that is there is reported as
       the probe finds it. A `stat` that did not answer is not
       "absent" either: the converge asks once more in the same pass, and
       when that does not answer either, `tmp_file_info` is an `ERROR`
       naming it and nothing is truncated — a file that really is absent
       then waits, as the tmpfs does, for the CN's next `SyncupCn` or the
       agent's next start.
     * **loop device**: probe `losetup --associated {CnTmpFilePath}` (the
       loop path is re-learned from this probe on every converge **and every
       probe pass** — it is kernel-assigned state, never persisted, and a
       stale cached path is exactly what CN28's arena check must catch);
       absent ⇒ `losetup --find --show {CnTmpFilePath}`, only while
       `tmpfs_info` is `OK`, as for the file; otherwise, with none attached,
       `loop_dev_info` is an `ERROR` saying no tmpfs is confirmed. Exactly
       one loop device: multiple attachments of the same file are unwanted.
       "Absent" is an answer with no device in it: `losetup --associated`
       exits 0 with no output when nothing is attached, so any failure of it
       is an `ERROR` on `loop_dev_info` and attaches nothing.
     * **clone-metadata arena**: nothing to create. The arena *is* the loop
       device: `CnCloneMetaAreaSize / CnCloneMetaUnit` = 256 units of
       `CnCloneMetaUnit`, handed out to clones by the CN18 slot allocator.
       There is **no** on-disk allocation table — enumerating the kind-`cb`
       wrappers (`dmsetup ls` filtered by `CnCloneMetaDmPrefix`, then
       `dmsetup table` of each) reconstructs the used map, because every
       wrapper's table `0 {len} linear {loop maj:min} {offset_sectors}`
       records its own allocation (the name comes from `dmsetup ls`, the
       major:minor only from `dmsetup table` — `ls` formatting varies across
       versions). The `ls` list is a snapshot that is stale the instant it is
       printed — a `SyncupCntlr` holds only the node *read* lock, so another
       cntlr's sweep (CN21), at `SP_LEVEL_DISABLE` or otherwise, can remove
       a wrapper between the `ls` and its `table` — so a name whose `table`
       fails is **dropped** when the wrapper has meanwhile vanished: it
       claims no units, and failing the CN-wide enumeration would flip unrelated
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
     * `EnsurePort` (SH19: the agent's `--nvmet-port-id` port, default
       `NvmetPortId`, from the `--tr-*` flags + the three fixed ANA
       groups). CN host-facing namespaces only ever use
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

CN7. **Persist first, then drop, then sweep.** The request is written to
     `LocalCnPath` **before** the converge, not after it — the one place this
     agent deviates from SH5, and only for the pointer list. The node-level
     sweep below is what removes the resources of a cntlr whose pointer has
     just left the list, and it can block for a whole failfast window on a
     dead leg; a request cancelled inside that window used to skip the save
     entirely, so the next startup reconcile rebuilt the cntlr from the OLD
     list against sides that no longer exist. With the new list on disk
     first, a crash mid-sweep is nothing worse than a startup sweep.

     Then the CN5 base-state converge. Then the `cntlr_pointer_list` diff
     against the local `cntlr-*` files (§9.1 full sync). A pointer in the
     request without local state needs nothing yet — resources come with its
     first `SyncupCntlr`; the persisted request is what makes the pointer
     *known*. A local cntlr whose pointer left the list is **forgotten**: its
     `cntlr-*` and `clone-bm-*` files are deleted, its connect retry and leg
     probers are cancelled, its memory entry and object lock are dropped
     (SH7) — and **nothing of its is removed from the node by this step**.
     Then the node-level sweep (CN21) finds its resources by name and removes
     them. Nothing is remembered about what the sweep could not finish; the
     reply's code is recomputed from the sweep's own verdict (CN30). The base
     state itself is never swept — like the DN port, it outlives every cntlr
     and only lab cleanup removes it. Reply `agent_reply`, `revision`,
     `cn_info`.

### 4.6 `SyncupCntlr`

CN8. **Gating.** The pointer MUST be present in the stored
     `SyncupCnRequest.cntlr_pointer_list` — else `ReplyCodeUnknownObject`
     (`SyncupCn` introduces pointers first, §9.1). Then the SH8 revision
     gate against the stored `SyncupCntlrRequest`. Then, last and still
     with **zero** side effects, the `architecture.md` §7 **conf gate**:
     `agent.ValidateBdevConf(req.bdev_conf)` (`dnagent.md` §2.1) refuses a
     request whose `dm_pool_conf.data_block_size`,
     `dm_pool_conf.low_water_mark_pct` or `dm_raid0_conf.stripe_size` is 0,
     or whose `redund_conf` selected md-raid1 with a 0
     `bitmap_chunk_block_cnt`. The control plane resolves all four when it
     *writes* the conf (`architecture.md` §7), so a zero here is a geometry no agent may
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
     promotion, the whole converge — whose sweep phase alone rewrites ANA
     states, reloads ns-dev linears and removes dm devices — and CN20's
     local-store persist, which is what keeps a refused request from being
     replayed by the next startup reconcile. The same check is repeated in
     the converge itself for the two entrances that do not come through
     this RPC — the CN2 startup reconcile, which converges from a file an
     older build may have persisted with zeros, and the CN10/CN18
     background connect retry, which re-enters with the request it already
     holds. There it returns an empty `CntlrInfo`, enumerates nothing and
     converges nothing — and it carries no leftover verdict either (CN30),
     because a cntlr this agent deliberately did not build must not have its
     objects named as removable.

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
       assemble (§8.12), so the group's own assembly is unaffected (CN12) —
       and `leg_id_to_leg` reports it `RES_STATUS_PROVISIONING` like any
       other provisioning leg (CN10).
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

     * **Sweep phase, top-down** (CN21) — everything of this cntlr's sp that
       exists on the node and the desired state does not want. What to remove
       is derived by **enumerating the node and subtracting the wanted set**,
       never from a diff of plans (`architecture.md` §9, teardown by sweep).
       The retire phase this replaced computed removals as "the plan I
       applied last time minus the plan I am applying now" and then
       overwrote the applied plan whether or not the removals worked, so a
       removal that failed was forgotten together with the plan that named
       it — and these are exactly the flows (a level change, a spare switch,
       a finished migration, a failover) in which the remote end is dead and
       a removal *does* fail. The layer order, the verification rule and the
       stop rule are CN21's; the pre-steps below are this pass's.
     * **Build phase, bottom-up** — legs (CN10) → groups (CN12) → per-slice
       pools (CN13) → thin volumes (CN14) → raid0/error (CN15) → clones
       (CN18) → transfers (CN17) → ns-devs + host-facing nvmet (CN16) → ANA
       rewrites to `optimized` last. This **is** §11.1 new_primary steps
       1-4.

     **The wanted set** is exactly the set of objects the build phase would
     ensure for this plan with every [D15] deferral removed — a deferred
     group's array is wanted although build skips it, or the sweep would
     remove what the next converge is about to create. It is read off the
     plan's own level/role flags, and each row is pinned against the `ensure*`
     that creates it rather than against any prose: `c9` wrapper and its `:2:`
     connection under `wantLeg`; md array (or the `ca` linear of a RedundNone
     group) under `wantGrp`; `c0`/`c1`/`c2` pool devices, `c3` thin volumes
     **and the `c4` raid0** under `wantPool`; `c5` per-td dm-error, `c6`
     ns-dev with its nvmet namespace, the host-facing subsystems, and the
     `c8` transfer with its subsystem and namespace under `wantAny`; `c7`
     dm-clone, `cb` metadata wrapper and `:4:` source connection under
     `wantClone`. The raid0 sits with the pool, not with `wantAny`, because
     at `SP_LEVEL_NO_THINPOOL` there is no thin volume left to stripe over
     (CN19) — a standby keeps only what that list gives it, which is what
     makes a primary→standby flip nothing but a smaller wanted set.

     **Pre-steps** (the transitions the retire phase used to compute from the
     previously applied plan, and this one derives from the plan and the live
     tables — `architecture.md` §9.8, whose attribution and derivation
     bullets are what this implements). Pre-steps 2 and 3 exist so the layers
     below them can remove anything at all, and run only on a pass whose
     listings all answered (CN21). Pre-step 1 exists so that a namespace the
     plan wants `inaccessible`, a transfer's included, is moved there before
     the device under it is parked or demoted — by pre-step 2 or 3, or by
     the build phase — and runs on every converge:
     1. **ANA.** Every namespace **of the plan** whose desired `ana_grpid` is
        `AnaGrpIdInaccessible` — a suspended one, a standby's, a deferred one
        (CN16), and every transfer namespace the same — is moved there first,
        probe-first. The loop is over the plan, **not** over the wanted set,
        and the difference is the whole of `SP_LEVEL_DISABLE`: `wantAny` is
        false there, so the wanted set holds no namespace and no subsystem at
        all, the chain takes the subsystems whole, and L1 never writes a
        per-namespace `ana_grpid` before the `rmdir` — this pre-step is that
        level's only ANA move. It **is** §11.1 old_primary step 1, and it
        precedes the park below for the reason §11.1 gives: the host is
        told to stop using the path before the path stops working. It is
        also the one pre-step a converge still runs when an unanswered
        listing has stopped its sweep (CN21): it removes nothing, and the
        build phase that follows parks ns-devs whether or not the sweep ran
        (CN16), so the move has to come first there too.
     2. **Park of a planned ns-dev.** A `CnNsDevName` **of the plan** whose
        CN16 backing is its td's `CnErrorName` (rules 0-4), or whose **live
        table** still maps a device this pass is about to remove, is reloaded
        onto that error device; a namespace whose td is not in the plan at
        all has no error device to park on and is skipped. The old retire
        phase asked the previous plan
        ("does this namespace's *old* td still exist?"); the live table is
        the same answer with no memory, and it is also right for a device an
        interrupted pass left mapping something no plan ever described. The
        reload's own flushing suspend is what completes the in-flight host
        IO — §11.1 old_primary steps 2-3 — and without it the removal below
        fails EBUSY behind a live ns-dev.
        The park creates its error device when it is missing, so it does
        not run at `SP_LEVEL_DISABLE`: the wanted set holds no `c5` there,
        and one created after the sweep's enumeration would be in no chain
        — the pass would remove the ns-dev parked on it, reply clean, and
        leave an unwanted device for the next round to find. Nothing is
        lost by skipping it: the wanted set holds no ns-dev at that level
        either, so P0 parks every one by its own live table (CN21), which
        needs no `c5` — where the td's is missing, it reloads the device
        onto an error table of its own size.
     3. **Demote of an unserved transfer.** A `c8` device this cntlr no
        longer serves (`plan.xferServed` false) is reloaded onto an error
        table of its own size, so that it lets go of the origin td's raid0
        (CN17) and L6 can remove **that raid0**.
     A fourth park runs inside the chain rather than here, because its set is
     the chain's own: every **unwanted** ns-dev, whether or not a plan names
     it — at `SP_LEVEL_DISABLE` the plan names them all and the wanted set
     holds none of them. See CN21's P0.

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
      from the desired state, never from probing the device.
      **The connect step waits, briefly and boundedly, for what it has just
      asked for** (*amended 2026-09-28*: an e2e react run's automatic grow
      failed a settled primary over because the primary's connect to a new
      side reached the disk node 18–26 ms before its export was linked into
      the port — the worker then fanned the side's `SyncupSide` and the
      primary's `SyncupCntlr` out unordered, [D16]; `dnv-worker.md` RW14's
      sides-first hold now sends the sides first, which makes that rarer,
      not impossible: the hold lasts one `cntlr_interval` at most, and a new
      side whose disk node refuses its first `SyncupSide` (`dnagent.md` DN8)
      usually reports only after that, so the cntlrs go without it — and
      the single re-read after a connect can equally come before the kernel
      has added the head). Each
      converge pass — one `convergeCntlr`, whether a `SyncupCntlr`, the
      startup reconcile or an attempt of the background retry below runs it
      — gets **one wait budget**, `CnConnectPassBudget` (1 s), made at the
      start of that pass, shared by every leg and by the clone sources of
      CN18, and never carried into another pass or another cntlr. Exactly
      three things draw on it. (1) Every failed `nvme connect` is charged
      its own elapsed time. (2) After a failed connect to a provisioned
      side the pass pauses `CnConnectRetryPause` (100 ms) and connects
      again — an **in-pass retry** — as long as the budget still covers the
      pause. The error is not classified: a disk node that has not linked
      the export into its port yet refuses the connect at TCP, or, when the
      port already listens for another export, rejects it with the same
      `failed to write to nvme-fabrics device` a permanently rejected export
      gets, and a disk node that is down refuses it too, or does not answer
      at all; every failed connect is a candidate until the budget is gone.
      A connect to a disk node whose VM is down takes about 3 s (the
      kernel's SYN retries, or `CmdSoftTimeout`), spends the whole budget
      at once and so is never retried in the pass. (3) After a connect made
      **in this pass**, the
      subsystem is re-read every `CnNsScanPause` (50 ms) until its
      multipath head is there, as long as the budget covers the step:
      `nvme connect` returns once the controller is live and only queues
      the namespace scan that adds the head. A subsystem that was already
      connected and has no head — a leg whose only path is ANA inaccessible
      never gets one — is judged on its one read. A pause starts only while
      it fits in what is left, and a failed connect is charged after it has
      run, so a retried connect that runs long can overdraw the budget;
      nothing bounds the first connect of a side, which a pass makes as it
      always did. Once the budget is spent the pass behaves exactly as it
      did without one: every connect it makes is made once, the first that
      fails still ends its leg's converge (the leg's other unconnected sides
      wait for a later pass), and a leg that did not connect, or whose head
      did not appear, fails with the same error as before. The waiting holds
      the cntlr's object lock, which every `CheckCntlr` round takes too
      (CN1), which is why the budget is one per pass and not one per leg.
      A connect failure marks that leg `RES_STATUS_ERROR` and registers the
      cntlr in a background retry registry that re-runs the converge every
      `CnConnectRetryInterval` seconds under the CN1 locks until a pass
      registers it no more, or teardown (the DN13 pattern); the RPC itself
      retries a connect, or waits for its head, only as far as the pass's
      budget allows. Five things register it: a leg that failed to converge
      — its connect, its multipath namespace or its wrapper (above), an
      unknown controller or a disconnect of its subsystem still in flight
      (below) — a clone source whose connection failed, or whose disconnect
      is still in flight (below), a clone recovery whose destination bitmaps
      were not applied or another clone failure CN18 lists (never a failed
      allocation or replacement of its metadata wrapper), and a `leg_list`
      member of a group that is not available (CN12;
      *amended 2026-09-26*, the failover ping-pong: a promotion whose first
      converge read its legs before the sides' ANA flips had reached this
      CN's sysfs left its md groups unassembled — and a RedundNone SP's
      pools unbuilt — until the next revision bump, because nothing re-ran
      that converge), and an ns-dev the build held off its td's raid0
      while a dm-clone the plan does not want may still be live (CN18).
      Every attempt mints its own
      trace id (CN2) and is a whole converge, which decides afresh whether
      any of the five still holds; the first converge that finds none stops
      the retry. **Dead paths**: a controller of the leg
      NQN whose `traddr`/`trsvcid` matches no desired side (the src side
      after `FinishMigration` — its controller died with DNR and will never
      reconnect) is disconnected by **device** (§2.3), never by NQN. A
      controller whose `address` read did not answer — any error but
      ENOENT, such as the soft timeout spent waiting for an `OsClient` slot,
      which stalled sysfs reads can hold — is **unknown, never unwanted**:
      it may be a desired side's live path as easily as a dead one. It is
      never retired as a dead path; while the pass's first read of the
      subsystem shows one, no side of the leg is connected, since it may be
      that side's; and the leg's converge fails for the pass —
      `RES_STATUS_ERROR` naming the read — which registers the retry. An
      absent `address` is an answer: a fabrics controller has one for as
      long as its device exists, and its subsystem keeps listing it after
      the device was deleted, until the last reference to it drops. Such a
      controller is **gone**, no side's path: it is not unknown and never
      retired as a dead path, and a side it alone served reads unconnected
      and is connected again.
      **The disconnect registry.** The CN21 sweep's `nvme disconnect --nqn` —
      L10's legs, L5's clone sources and the node-level pass's unowned sources
      — is never run inline: when the target vanishes mid-delete the kernel
      cannot enter error recovery on a DELETING controller, the controller's
      shutdown command waits out the admin timeout (60 s), and
      `nvme disconnect` waits with it in an uninterruptible write that no
      SH15 signal ends (`dnagent.md` SH15). Inline, that held the cntlr's object
      lock — and every `CheckCntlr` round of the cntlr with it (CN1) — for the
      whole minute, and the worker's `SyncupCntlr` ran into its own 60 s
      deadline. So the pass probes the connection under its locks and nothing
      more: one with no controller is gone; one the probe could not read is a
      leftover and nothing is issued for it; one with a controller is a
      leftover of that pass, which sets its disconnect going — unless one
      already runs — from a goroutine on `rootCtx` that carries the pass's
      trace id and takes none of the CN1 locks. A later pass's probe is what
      finds it gone. At most `disconnectConcurrency` of these disconnects —
      a quarter of `DefaultOsClientLimit`, 8 — run at once; the rest wait
      for a slot and stay registered while they wait. Inline, a pass issued
      its disconnects one at a time; uncapped, one L10 of many legs or one
      pool drain would set them all going together, and each delete a
      vanished target stalls holds an `OsClient` slot for the admin timeout
      — enough of them, and the node's converges and Check rounds are
      refused a slot at the soft timeout and report `ERROR` rows on healthy
      objects. The registry is a server-wide set of subsystem NQNs under
      the leaf mutex, and it is bookkeeping of work in progress, never memory
      of work that failed: an entry lives exactly as long as its goroutine,
      and it is read for two things. A later pass issues no second disconnect
      of a subsystem whose first one is still running or waiting for a slot.
      And the connect steps read it too: a converge that wants a subsystem
      whose disconnect is still in flight neither adopts nor connects it —
      the controller it would adopt is about to be deleted under it, and one
      connected beside it could go with it, the disconnect being by NQN — so
      the step fails, the CN10/CN18 retry registers, and the first pass after
      the goroutine has gone reads the subsystem afresh and connects whatever
      is missing. The CN28 probe asks the same question: while the
      disconnect is in flight it reports the leg's row, or the clone's
      target row, `ERROR` with the converge's own details, and it does not
      judge that clone's step 2 — so the two channels neither flip those
      rows, and their epochs, against each other nor disagree on how far
      the clone got. Whether a connection is left over is never read from
      the registry, only off the node — so a disconnect that failed is
      issued again by the next pass that still finds the controller. The
      goroutine is not joined at exit (`dnagent.md` SH27).
      The dead-path `nvme disconnect --device` above is the build's, not the
      sweep's, and runs in the pass, so it can still meet the same wait under
      the object lock (Known limits).
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
      `{inflightSince, completed, lastErr}`; the CN28 probe reports, per leg:
      an attempt in flight longer than `CnLegProbeStallSeconds` ⇒
      `RES_STATUS_ERROR` `"health probe stalled"`; else the last completed
      outcome; before any completion — or before a prober is registered —
      ⇒ `RES_STATUS_PENDING` `"health probe pending"` when the wrapper
      exists (*amended 2026-09-26*, was `RES_STATUS_OK`: an `OK` clears
      `Leg.err_epoch`, so a dead leg's was cleared at every promotion, whose
      probers start over, and an unprobed spare read ready to the worker's
      leg repair — `dnv-worker.md` HL2, AR8). For a registered prober the
      window is one `CnLegProbeInterval` tick plus the probe itself, and a
      fresh wrapper, a promotion and an agent restart each open it. A leg
      whose wrapper exists but whose converge fails before its prober
      registers — on a promoted or restarted primary whose connect is
      refused, say — reads `PENDING` in every probe round until a converge
      registers one, while each failing converge marks the leg
      `RES_STATUS_ERROR` (CN10). A **standby** (and
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
      start when the wrapper converges and are cancelled by the first
      converge whose plan no longer probes their leg — every converge trims
      them before its sweep, whether or not the sweep's descent reaches the
      legs (CN21) — or when the cntlr is dropped (CN7); a
      goroutine wedged in D state on a pathless leg is released by the
      teardown's own disconnect (deleting the controller errors its queued
      IO) and is accepted as unreclaimable until then. Because that wedged
      probe still holds an **open fd on the leg wrapper** after its cancel,
      CN21's L10 order is **set the leg's disconnect going** → remove the
      wrapper: a `dmsetup remove` fails EBUSY until the disconnect, which
      runs off the pass (CN10), has errored the queued IO, so such a
      wrapper normally goes on a later pass. The cancel itself comes before
      L10: in the converge's trim ahead of its sweep, or, for a cntlr that
      is forgotten, in the drop step that precedes the node-level sweep
      (CN7, CN2).
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
           by `TestGroupNeverCreatesOverASubsetOfLegs`). Nor while
           `/dev/md/{CnMdDevName}` already resolves to a device
           (`Md.NameInUse`, an `lsblk` of the node, which reads no member;
           one that did not answer is an error that refuses the create this
           pass, like a killed `--examine` below): the array is found by its
           members (below), so one that runs under the group's name holding
           none of its `leg_list` wrappers — both legs switched out, parked
           or released, while this cntlr was not converging — reads as
           absent and reaches case 1 with fresh legs, where a create would
           put a second array under the name the pool's concat resolves
           ("an array runs under … holding none of the group's legs", pinned
           by `TestGroupNeverCreatesBesideARunningArray`, a killed `lsblk`
           included, and `TestMdNameInUseKilledIsAnError`; *amended
           2026-09-26*, when finding the array by member replaced finding it
           by name). The guard errs only towards refusing, and a false
           refusal needs a stale node of this very name — which, short of
           the case the guard exists for, a group whose legs carry no
           superblock has never had an array to leave.
           `--assume-clean` is **always** correct here, short of the
           failed-read answer below (a member whose read failed answers as
           superblock-free without being so): a side is never exported
           before the §9.4 **provisioning** protocol has zeroed it
           whole (`blkdiscard --zeroout` per batch of extents, tracked in the
           volume table's `zeroed_bits` and gated by `Side.provisioned`), and
           ids are never reused — so a superblock-free leg can only be a
           freshly provisioned side, and both members are all-zero because
           zeros were **written**, not because a discard was assumed to read
           back as zeros ([D15]).
        2. Some have one ⇒ `mdadm --assemble` with those; then the sysfs
           read of the array (below) and `--add --failfast` any available
           member the array left out (freshly provisioned additions and
           stale-metadata re-adds both land here; §11.1.1 cases 1.2/1.3/2 —
           with a single available member mdadm itself decides whether a
           degraded start is safe, and a refusal leaves the group
           `RES_STATUS_ERROR`).
        A leg is **available** iff its CN10 converge succeeded this pass —
        its provisioned sides connected, every controller's `address`
        answered, its multipath namespace found and its wrapper built — and
        that namespace has a path that is both `live` and `optimized`
        (§11.1.1, probed from **sysfs** — §5).
        A `leg_list` member that is **not** available this pass — whether
        its group was left unassembled (`no available leg`, `only k of n
        legs available …`), started degraded without it, holds the array
        while the member reconciliation below skips its `--add`, or runs the
        array with md still holding the member (its side died under the
        array — AR8's case — or its path is otherwise no longer both `live`
        and `optimized`, or its CN10 converge failed this pass: a connect,
        an `address` read that did not answer, its multipath namespace or
        its wrapper; the reconciliation below leaves such a member held) —
        registers the cntlr for the CN10 background retry, as a failed
        connect does
        (*amended 2026-09-26*, the failover ping-pong: the worker fanned a
        promotion's `SyncupCntlr` and the sides' `SyncupSide` out unordered,
        [D16], so the new primary's first converge could read its paths
        before the sides' ANA flips had reached them — rarer since
        `dnv-worker.md` RW14's sides-first hold, not impossible, since the
        hold is bounded and releases the cntlrs whether or not every side
        has reported — and the worker re-syncs on a revision or a reply
        code, never on a row). Only
        availability counts, and only for a wanted group's members: a
        standby wants no group, nor does a primary whose `sp_level`
        suppresses its groups (CN19); a deferred group (below) never
        counts, spares are not members, and a held member md has failed on
        a leg that is available is not late (§7, known limits). A RedundNone
        group's leg counts too, although its dm-linear (above) is built
        whatever the leg's availability: the layers over it do IO through
        it — the pool create (CN13) reads the pool's metadata through the
        meta group — and a side that has not flipped to this CN yet exports
        dm-error to it (§11.1.1), so a pool over a late meta leg, and every
        layer above that pool, is built only by a later converge. No log
        record of its own: the rows already report it — an
        md group's error, or `degraded` once a probe reads the array, the
        error of a layer above a RedundNone group, and the member's own leg
        row. The retry runs a converge every `CnConnectRetryInterval`
        seconds for as long as a member stays unavailable, each under the
        cntlr's object lock, which every `CheckCntlr` round takes too
        (CN1): on a wide SP an attempt can hold a Check round past the
        worker's round timeout, and the primary then reads unreachable
        (§7, known limits).
        **The array is read from sysfs** (*amended 2026-09-26*, the failover
        ping-pong; it was `mdadm --detail /dev/md/{CnMdDevName}`, which
        opens a member — see below). `Md.Walk` lists `/sys/block` for the
        array nodes and each array's `md/` directory, reading every member's
        `md/dev-*/block/dm/name`, as the sweep's `ListArrays` does — once
        per converge or Check pass, shared by all of the pass's groups: a
        listing is an `ls` per array, and a walk per group would list every
        array once per group (4160 listings a round at 32 slices, 64 groups
        over 64 arrays). An array node is `md[0-9]+`, or `md_<name>`: the
        node mdadm puts `/dev/md/<name>` on when mdadm.conf says `CREATE
        names=yes`, which `architecture.md` §4.3 sizes `CnMdDevName` for
        (*amended 2026-09-26*: both listings took `md[0-9]+` alone, and once
        the md rows were read from the walk a named node would have read
        every group's running array as absent — `MISSING` on every Check
        round, a refused re-assembly on every converge); `mdN` below stands
        for either. The walk reads every array on the node, other sps'
        included, so an array whose `md/` listing or a member's dm name did
        not answer — the `ls` killed at the soft timeout, a read held past it
        on the agent's `OsClient` semaphore, which every cntlr it serves
        shares, ctx cancelled, or a read failing with an errno other than
        `ENOENT` — is recorded as **unanswered** instead of failing the
        walk: failing this pass for another sp's array would turn this sp's
        md rows `ERROR` — which count toward cntlr health — for a fault that
        is not this sp's (*amended 2026-09-26*; the reason first given, that
        an array another cntlr is stopping reads `ENODEV`, was false). An
        array another cntlr is stopping never reads as unanswered: md
        removes a member's `block` link as it unbinds it, so that member's
        dm name reads `ENOENT` and the member is recorded with no dm name,
        which no group's names match; an `md/` that went makes its `ls`
        answer "no" and drops the array. The sweep's `ListArrays` reads such
        an array as foreign while a member is unbound, but not for the
        whole stop (*amended 2026-09-26*: this claimed it for the whole
        stop, and the kernel says otherwise): from the moment md marks the
        array deleted until its `md/` goes, `array_state` reads
        `EBUSY`, and `ListArrays`' strict rule fails that pass's md
        enumeration — a Leftover the worker re-drives, from a sweep that
        removed nothing (CN21). By default md marks
        it when the stopped array's last reference goes — a close, or the
        end of a read of one of its `md/` attributes, whichever is last —
        after its `dev-*` directories are gone; with the md module's
        `legacy_async_del_gendisk=0` it marks it in the stop itself, while
        the unbound `dev-*` directories are still there. The walk reads no
        `array_state` and is unaffected. Only a `/sys/block` listing that
        did not answer fails the walk. After an assembly `Md.Refresh` lists
        `/sys/block` again, drops the nodes that went (recorded or
        unanswered), and re-walks only a node that is new, was recorded with
        no member or as unanswered, or has a recorded member directory that
        is gone, carries another dm name or did not answer the check —
        another cntlr's converge may have stopped an array and freed the
        very `mdN` this one's assembly took; a re-walk that does not answer
        drops the node's record and records it as unanswered. The check
        reads each recorded member's `dev-*/block/dev` and
        `dev-*/block/dm/name`, never its own `state`: when another cntlr
        stops an array or removes a member, md unbinds the member by
        removing its `block` link, and the `dev-*` directory stays until md
        deletes it, every attribute of its own reading `ENODEV` meanwhile. A
        check that did not answer is not an error of this group: it only
        makes `Refresh` walk that node again, and a walk of it that does not
        answer records it as unanswered.
        `Md.Detail` takes from the walk the one array whose members include
        a wrapper of the group's `leg_list`. Spares are not keys: a leg
        switched out into `spare_leg_list` is an extra, found through the
        member that stays. No such array is "absent" and runs the assembly
        above — unless an array of the walk is unanswered, which may be the
        group's own: then no answering array proves absence, the group's
        error names the unanswered array, and nothing is created or
        assembled. Only an `ls` of an array's `md/` that did not report
        (killed, never started, refused by the semaphore), or a member
        dm-name read failing with something other than `ENOENT`, makes an
        array unanswered; a foreign array's non-dm member reads `ENOENT` and
        is recorded, so a foreign array cannot hold an assembly off for
        good. Nothing re-drives the refused assembly either
        (*amended 2026-09-26*) — unless the same pass registers the CN10
        retry for something else (any of CN10's four, among them a
        `leg_list` member of any group of this cntlr that is not available,
        above), whose next attempt is a whole converge and tries the group
        again: the group's error is a row, not a reply code (CN29), the
        error itself registers no CN10 background retry, and the Check
        verdict reports a leftover (a reply code the worker re-drives) only while
        its own enumeration, `ListArrays`, still fails — so the group stays
        unassembled, its row `MISSING` on the Check rounds after the array
        answers, until the cntlr's next converge for some other reason (§7,
        known limits). Beside an answering match an unanswered array is
        left alone (it cannot be told from another sp's array whose read was
        cut off; the one thing it hides is a second array of this group).
        Two answering arrays are an error; a member with no dm name (a
        foreign member) of the matched array is an error. Of that array it
        reads `md/array_state` first and then, for a **running** array
        only, `md/degraded`, `md/sync_action` and `md/sync_completed` — an
        `inactive` array has none of the three, and its members read a bare
        `spare` — and each
        member's `dev-*/state` (a flag list, `in_sync,failfast` on a healthy
        dnv member) and `dev-*/block/dev`. Running means `array_state` is
        `clean`, `active`, `active-idle`, `write-pending`, `readonly` or
        `read-auto`; an array in any other state (`inactive`, `broken`, …)
        is left exactly as it is and its state is the group's error. A
        read of the matched array that did not answer is an error, never
        absent. A matched array whose `array_state` has gone by that read
        (stopped since the walk) reads absent — unless an array of the walk
        is unanswered, which makes it the same error as no match (*amended
        2026-09-26*): absent always means every array of the walk answered.
        **Member reconciliation** covers `SwitchSpareLeg` with no extra
        mechanism: a member the array holds — its `dev-*/block/dm/name`,
        compared with the names of the `leg_list` wrappers; no `lsblk` —
        that is no longer in `leg_list` is `--fail`ed and `--remove`d by
        the dm path sysfs named; an available `leg_list` member the array
        lacks is `--add --failfast`ed (md then resyncs — a bitmap catch-up
        for a briefly absent leg, a full rebuild for a promoted spare).
        Every `leg_list` name is wanted, available or not: a leg this pass
        cannot use (no path both `live` and `optimized`, or its CN10
        converge failing: a connect, an `address` read that did not answer,
        its multipath namespace or its wrapper) is never added, and a member
        md still holds for it is neither failed nor removed.
        *Amended 2026-09-26:* the comparison was by device number, and an
        `lsblk` of a `leg_list` wrapper that did not answer read as "not
        wanted" and failed and removed that in-sync member. `--fail` and
        `--remove` open the member's path (no IO) and act on its device
        number. A `--remove` issued within seconds of the member's side
        dying, while a superblock write is stuck on that member, sleeps in
        md's suspend until the write fails at the path's error recovery
        (up to ~8 s after the side died, measured), so it can be killed at
        the soft timeout with the member still held: that pass reports the
        group `ERROR` and does not reach the promoted spare's `--add
        --failfast` either, because extras leave before promotions arrive.
        Nothing remembers the failure, and nothing schedules another
        converge for it: the group's error is a row, not a reply code
        (CN29), so the worker does not re-drive it, and it registers no
        CN10 background retry (a leg that failed to converge does, CN10,
        and so does a clone source whose connection failed or another
        clone failure CN18 lists, and so does a
        `leg_list` member that is not available — above,
        *amended 2026-09-26* — which is cntlr-wide: any such member of any
        group of this cntlr registers it, and the retry finishes the switch,
        the first of its attempts that finds the promoted spare available
        adding it; the trigger includes the promoted spare if it is not
        available yet, or, when the switched-out leg's whole DN died,
        another group's leg still on that DN (AR8 switches one leg per
        pass), while the switched-out member itself never counts, since it
        has left `leg_list`); later Check rounds read the running array
        `OK`, degraded. The switched-out member stays held and the spare
        stays out until the cntlr's next converge, for whatever other reason
        it runs (a `SyncupCntlr` for a revision bump or a non-zero reply
        code, an agent restart's CN2 re-run, a background retry registered
        for something else), which removes the member in milliseconds and
        adds the spare — or, while the spare is not available yet, leaves
        its `--add` to the retry that the late spare registers (§7, known
        limits). The cn agent never runs
        `mdadm --zero-superblock`: a leg only ever leaves an array into the
        spare list (where a stale superblock makes a later re-add cheap) or
        out of existence with its side.
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

      **What a killed mdadm means.** A run that did **not answer** (killed
      at the soft timeout, never started, ctx cancelled: `agent.Reported`
      is false) is an **error** and must never read as absent.
      `Md.HasSuperblock` (`mdadm --examine`) is the sharp case: `--examine`
      opens and reads the member, so on a leg whose DN side has gone it
      blocks until failfast and is killed, and a killed run read as "no
      superblock" would send the assembly into case 1 and `--create
      --assume-clean` over live data. It returns `(bool, error)`, and an
      unanswered probe aborts the whole assembly — with no answer this pass
      cannot tell case 1 from case 2, and guessing case 1 is destructive.
      The rule started at `Md.Detail`, which ran `mdadm --detail` until
      2026-09-26: a killed run read as "the array is not there" is
      precisely what let the old teardown skip `mdadm --stop` and leave a
      live array pinning its two leg wrappers for ever. It now reads sysfs,
      under the same rule.
      `--examine` has a second unsafe answer, one this rule does not catch
      (measured 2026-09-27): once the path's failfast has expired — ~13 s
      after the side died, the timing measured below — the read fails with
      an IO error instead of waiting, and mdadm answers exactly as it does
      for a member with no superblock, `No md superblock detected` with
      exit status 1. `HasSuperblock` reads that as "no superblock", and
      nothing in the answer tells the two apart. Only a leg that read
      **available** earlier in the same pass is probed, and a path past its
      failfast is no longer `live`, so the answer needs the failfast to
      expire between that read and the probe's answer. With a superblock
      on another member the assembly takes case 2 without this one. Case 1
      needs no member to answer with one — every `leg_list` member
      available, each fresh or answering so — and its `--create
      --assume-clean` then writes to the member whose read failed: into
      the same failing IO or, if a path has reconnected in between, over
      its live data (§7, known limits).

      **Nothing in the agent runs `mdadm --detail`** (CN21, CN28;
      *amended 2026-09-26*: no sweep ever did, and `ensureGroup` and
      `probeGroup` did until then). It loads the superblock from the first
      array member that opens; when that member's DN side has gone, the
      read sits in the multipath head's requeue list until the path's
      failfast expires — measured ~13 s after the side died: the
      controller stays `live` until its keep-alive times out, error
      recovery starts a second later, and `fast_io_fail_tmo` runs from the
      reconnect that follows — far past the 3 s soft timeout. A Check
      round's probe was killed, the group's row read `ERROR` for a member
      fault the leg row already reports, and that row counts toward cntlr
      health: it failed the primary over, the first failover of the
      failover ping-pong found 2026-09-24. What the sweeps, the member
      reconciliation and the md rows need about an array they read from
      **sysfs**, which touches no member device and therefore cannot block
      on a dead leg (of the md probes, only the assembly's `--examine`
      above still reads a member, and only an available leg's):
      `ListArrays` walks `/sys/block` for the array nodes (`md[0-9]+` or
      `md_<name>`, CN12; *amended 2026-09-26*: it took `md[0-9]+` alone, so
      a sweep never stopped an array on a named node), reads each array's
      `array_state`, and names each member
      through `/sys/block/mdN/md/dev-*/block/dm/name` — which is what
      attributes an array to an sp, and a member with no such attribute is
      not a dm device at all. `Md.Walk` makes the same listings and dm-name
      reads (not `array_state`), once per pass for the md groups (above),
      under a different rule for an array that did not answer (*amended
      2026-09-26*, so that another sp's array cannot turn this sp's md rows
      `ERROR`): `ListArrays` keeps the strict one — one array whose
      `array_state`, `md/` listing or member dm name did not answer fails
      the whole md enumeration, because a removal decision needs the whole
      node; that is the sweep's `enumeration failed: md arrays …` and a
      `ReplyCodeLeftover`, never a row, and the worker counts a Leftover
      reply as accepted, so it does not reach cntlr health — while
      `Md.Walk` records such an array as unanswered.
      `Gone` verifies a stop from the same `array_state`: only an absent
      directory or `clear` counts, never `inactive`, which is an
      assembled-but-not-running array that pins its members just as hard.
      A read that fails is an error, never gone — the `EBUSY` a stopped
      array's `array_state` reads until its `md/` goes (above) included:
      `stopArrayVerified` logs `verifying an md stop failed` and reports the
      array as a Leftover, which the worker re-drives.
      `mdadm --detail --scan` is deliberately not the enumerator: it loads
      superblocks. The node a sweep stops is the one sysfs named,
      `/dev/mdN` or `/dev/md_<name>` — never `/dev/md/{CnMdDevName}`, which
      depends on udev having run.

CN13. **Per-slice pools** (`pool.go`; primary only). Per slice of
      `id_to_slice`: the multi-target dm-linears `CnPoolMetaName` (meta
      group devices, `meta_grp_list` order) and `CnPoolDataName` (data
      group devices), tables via stdin (Appendix A); then the thin-pool
      `CnPoolFinalName` with `block_sectors = block_size / 512` and
      `low_water_mark` = data-dev blocks × (100 −
      `dm_pool_conf.low_water_mark_pct`) / 100. `pct = 0` is **invalid**
      and never reaches that arithmetic: the control plane resolved an
      omitted percentage to `DefaultPoolLowWatermarkPct` when it *wrote*
      the conf (`architecture.md` §7), so a zero arriving here is a value no agent may
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
      `dmsetup message`, short of a `dmsetup` command on it that fails
      (CN16).

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
      error paths too, so no device outlives the sequence suspended ([D12])
      short of a `dmsetup` command on it that fails (CN16).
      Only the messages sit inside the window — the snapshots' own thin
      *devices* are created after the resume, since the content is fixed at
      message time. The window is bounded by `slice_cnt` messages under the
      SH15 timeouts. The trigger is liveness and nothing else: when the
      origin td's raid0 is not live — never built on this cntlr, or already
      removed — there is no dnv IO path to quiesce and the messages go
      unquiesced (at a pool-suppressing `sp_level` the pre-pass does not run
      at all, so no `create_snap` is sent, CN19). A *deferred* origin is not a
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

      A td leaving `td_list` is **deleted** by the sweep (CN21), which is
      where its layer order already puts the pieces: its namespaces, ns-devs,
      raid0 and dm-error go first because they reference it (L1, L2, L6),
      then the thin volume devices (L7), then the `delete {dev_id}` message
      per slice pool.

      Both halves of that message are read from the volume's **own live
      table** — `0 {sectors} thin {pool devno} {dev_id}` — immediately before
      the removal, never from the request: the table is what the kernel will
      act on. The `dev_id` is its second argument, and the pool is its first,
      resolved from a devno back to a dm name through the same `dmsetup ls`
      snapshot the sweep enumerated with. A table that cannot be read (the
      device may already be gone) skips the message rather than guess an id.

      The message is sent only while this cntlr holds the pool — the pool
      must be in the sweep's own wanted set, which takes `plan.wantPool`, and
      it must still answer a `dmsetup info` probe: a standby has no pool
      device, only the primary may write pool metadata, and a `delete`
      against a pool that a later layer is about to remove is both pointless
      and unsendable. The fan-outs
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

      A whole sp leaving this CN — the pointer removed, or
      `SP_LEVEL_DISABLE` — instead only **deactivates**: the pool is
      unwanted too, so by the rule above the thin volumes' removal sends no
      `delete` message at all, because the pool metadata lives on the DN legs
      and the next hosting CN must find the thin volumes intact.

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
      1. the namespace is effectively suspended (below) ⇒ table → the td's
         `CnErrorName`, live — the **parked** rule (§11.6, [D12]); never
         under dm-flakey;
      2. standby or disabled cntlr ⇒ table → the td's `CnErrorName`;
      3. `sp_level ≥ SP_LEVEL_NO_THINPOOL` ⇒ → `CnErrorName`;
      4. a clone targets the td (`clone_list` entry with
         `dst_td_id == ns.td_id`) and `sp_level ≥ SP_LEVEL_NO_CLONE` ⇒ →
         `CnErrorName` (a raid0 with holes must never serve);
      5. a clone targets the td ⇒ → `CnCloneFinalName` (via dm-flakey when
         rule 7 applies) — only while that dm-clone's `dmsetup status` shows
         hydration enabled, the mark CN18 step 5 alone sets, after step 4's
         bitmaps (§11.5: they must be applied before the dm-clone handles
         any IO). While it does not — a build or a recovery that has not
         finished, whatever stopped it — the ns-dev gets the td's
         `CnErrorName` instead, parked like rule 1: live, never under
         dm-flakey. Every converge and every probe reads that status afresh
         (`nsDevNow`); nothing is remembered. A read that fails or does not
         answer moves the ns-dev nowhere: it keeps its table, and its
         `ns_id_to_dm_linear` row is `RES_STATUS_ERROR`. The ANA rule below
         does not look at any of this, so the namespace is `optimized` all
         the same and a host takes IO errors — the recovery's own window
         (CN18 step 4) — until a pass's read here finds hydration enabled
         and the ns-dev moves onto the dm-clone. Most CN18 failures that
         stop the build before step 5 has enabled hydration register the
         CN10 retry that runs such a pass (CN18 lists which; a failed
         allocation or replacement of the metadata wrapper does not), and
         so does any failure to converge this ns-dev, this read and its
         reload onto the dm-clone among them, in a pass whose CN18 finished
         the build (the clone's `clone_id_to_dm_clone` row `OK`): the worker
         re-syncs on a revision or a reply code, never on a row, so without
         it the park a recovery left would stay until the cntlr's next
         converge for another reason;
      6. otherwise ⇒ → `CnRaid0Name` (via dm-flakey when rule 7 applies);
      7. `SP_LEVEL_READONLY ≤ sp_level` (primary only): the table is the
         Appendix A flakey `error_writes` line over the rule-5/6 backing —
         reads pass, writes error ([D11]).

      Rules 4-5 and the `auto_resume` override below combine into one flip
      worth stating out loud. An `auto_resume` clone's
      destination namespace is stored `suspended = true`, which on its own
      would leave it `inaccessible` and its host **queueing**; the override
      makes it serve, so *below* `SP_LEVEL_NO_CLONE` it is already
      `optimized` over `CnCloneFinalName`. At `sp_level ≥ SP_LEVEL_NO_CLONE`
      the override still applies (it keys on the `clone_list` entry, not on
      whether the clone stack was built), so the namespace stays exported and
      **stays `optimized`** while rule 4 parks its ns-dev on `CnErrorName`:
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
      clone runs; `DeleteClone` flips the stored field to `false` — in its
      LATCH transaction, together with the `deleting` flag, so that the
      override and the clone's disappearance from the plan arrive in one
      syncup (architecture.md §8.9, amended 2026-09-16). Deferred to the end
      of the teardown it would leave this namespace effectively suspended
      with no `clone_list` entry left to override it.)
      An effectively suspended namespace is **parked**: its ns-dev's table is
      a dm-linear over the td's `CnErrorName` (rule 1 above, the same table a
      standby has) and the device is **live**; the ns is in
      `AnaGrpIdInaccessible` on every cntlr. Parking: ns →
      `AnaGrpIdInaccessible` first (CN9 pre-step 1), then the ns-dev reload —
      which CN9 pre-step 2 performs, because rule 1 has
      already made the backing the dm-error, and the build phase then finds
      the table it wants; on a pass whose sweep an unanswered listing
      stopped (CN21) pre-step 2 does not run and `ensureNsDev` makes the
      same reload itself, still after pre-step 1. Unparking: the reload
      onto the backing the remaining
      rules select first, then ANA per the rule below — except onto the
      td's raid0 while a dm-clone the plan does not want may still be live
      (the pass's L3 did not run, or left one): the namespace then stays
      parked and `inaccessible`, its ns-dev row `ERROR`, until a pass that
      leaves no such dm-clone live (CN18).
      No path of a pass leaves a CN device suspended unless a `dmsetup`
      command on it fails, and one such failure leaves it so on purpose: a
      reload **fails closed** (*decided 2026-09-29*, `dnagent.md` §2.8). A
      reload whose load fails returns the error with the device still
      suspended on its old table — any reload, since the rule is
      `Dm.Reload`'s own: for a park, resuming that table would put the
      ns-dev back onto the backing it retires and replay onto it the IO the
      suspend absorbed. A resume that fails can leave a device suspended too.
      A device found suspended is one that such a failure, an older build,
      an interrupted `Reload`, or an agent killed inside CN14's quiesce
      bracket left, and every path that meets it resumes it — by a reload
      or by a bare resume, never by leaving it; one whose reload keeps
      failing its load stays suspended, queueing its IO, until a reload or
      a resume of it succeeds. An
      unwanted one that no path of a pass meets — below a layer the chain
      stopped at, or anywhere on a pass whose sweep an unanswered listing
      stopped (CN21) — is met by the first pass whose chain reaches it.
      `ensureNsDev` and `removeDm` bare-resume a device whose table already
      matches, and `ensureNsDev` one it holds off the raid0 (CN18) as well.
      `parkNsDev` — CN9's pre-step 2, the park of an ns-dev **of the
      plan**, which has a plan to reload from — reloads one whose table does
      not match, *and* one whose table does match while it is suspended, and
      that reload resumes it as a side effect. CN21's P0 is neither of those
      two: it is `parkByTable`, which has no plan at all for an ns-dev
      nothing wants, so it bare-resumes an already-parked one and reloads
      only an unparked one.
      *Decided 2026-09-16: parked, see [D12] and `architecture.md` §11.6.*
      `UpdateNamespaceDev` arrives as a changed `ns.td_id` and is exactly
      one ns-dev reload — the nvmet `device_path` never changes.

      **nvmet objects** on the node port: per `Subsystem` a subsystem with
      `attr_cntlid_min/max` from `Cntlr.cntlid_slot` (`CnCntlidSlot*`,
      §11.8), `attr_serial`/`attr_model` verbatim from the record (the
      gateway stamped [D2]), `attr_allow_any_host = 1` iff `allowed_hosts`
      is empty, else host links exactly per the list; per `Namespace` an
      nvmet namespace `nsid = ns_idx`, `device_path` = its own ns-dev,
      `uuid`/`nguid` from the record. A namespace that has left `ns_list`
      under a subsystem that stays is not the build's to remove: CN21's L1
      alone removes it (and so nothing does under a subsystem whose NQN
      carries the dnv prefix but decodes to nothing, which the sweep never
      attributes, §2.1; `architecture.md` §7), `AnaGrpIdInaccessible`
      first, after P0 has parked
      or resumed the ns-dev under it — or failed to, which does not stop L1
      (CN21) — and only from an enumeration that answered. A pass in which one did not — one of the four listings, or
      the sweep's own listing of that subsystem's namespaces — leaves it
      where it is with a non-OK verdict, and the next pass whose listings
      answer removes it.
      **ANA**: `AnaGrpIdOptimized` iff
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
      provisioning-deferred, or, on a primary too, an origin that no longer
      resolves (CN29) — reloads the live `CnXferFinalName` onto an
      error table of its own size, as CN9's pre-step 3, before any layer
      touches what is under it: a linear still mapping the origin td's raid0
      holds it open and the raid0's removal would fail EBUSY (CN19's
      `NO_THINPOOL` row). On a pass whose sweep an unanswered listing
      stopped (CN21) pre-step 3 does not run, and the build phase makes the
      same reload when it converges the device — still after pre-step 1
      has made whatever ANA move the plan wants for the transfer's
      namespace. At `SP_LEVEL_DISABLE` the pre-step still runs —
      nothing serves the device at that level either — but it no longer
      decides anything: the wanted set is empty, so the transfer device is
      removed outright a layer later with every other cntlr-scoped object
      (CN19's `DISABLE` row, CN21's L2).
      When the plan cannot size that table — the origin namespace is not in
      it at all, or it is but its td left `td_list` in the same request,
      which leaves the namespace's size unknown — the size is summed from the
      device's **own live table** instead, by pre-step 3 and, on a pass
      whose sweep an unanswered listing stopped, by the build's converge of
      the transfer, which records the origin error rows after it; without
      that fallback the demotion would be skipped and the departing raid0
      never released. A
      primary is no exception: an unresolved origin makes `plan.xferServed`
      false there as well, so the primary demotes the device like a standby
      and the transfer no longer holds the departed raid0 against L6 — a
      linear left serving would keep that raid0's `dmsetup remove` failing
      EBUSY on every pass while the transfer stays in the request. Its
      namespace stays `AnaGrpIdOptimized` all the same, as it does at
      `SP_LEVEL_NO_THINPOOL`: the transfer's ANA group follows the role, the
      level and deferral only, so the destination is handed IO errors from
      the error table, not a queue. The
      live table is deliberately not the *previously applied* plan the old
      retire phase read it from: it is the same answer, from the kernel, with
      nothing remembered. A transfer whose origin td is
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
         `nsid = src_ns_idx`. The connect step is CN10's, drawing on the
         same pass budget the legs drew on (*amended 2026-09-28*): a failed
         connect is retried within the pass while the budget covers the
         pause, and after a connect this pass made the subsystem is re-read
         until the source namespace is there. With the budget spent — the
         legs may have spent it — a source that did not connect, or whose
         namespace did not appear, fails step 1 exactly as before
         (`no namespace {src_ns_idx} on {src_nqn}` for the latter).
         CN10's unknown-controller rule is not applied here: a source
         controller whose `address` did not answer matches no entry, so an
         entry it alone serves reads unconnected and is connected again — a
         duplicate the host refuses while that controller lives, which
         fails step 1 once the budget is spent — and nothing is
         disconnected, since step 1 retires no path. A source whose
         disconnect a sweep has set going is neither adopted nor connected
         while that disconnect runs (*amended 2026-09-29*): step 1 fails
         naming it, and the retry reads the source afresh once the
         disconnect has returned (CN10's disconnect registry).
      2. Allocate the clone's metadata slot from the §2.1 arena:
         `ceil((4 MiB + region_cnt bytes) / CnCloneMetaUnit)` **contiguous**
         units with `region_cnt = td.size / block_size` (one byte per region
         over the dm-clone superblock is a generous bound — the dn agent
         budgets metadata the same way), first-fit over the free ranges of
         the used map reconstructed from the kind-`cb` wrapper tables (CN5).
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
         kind-`cb` wrapper belongs in that same critical section, for the same
         reason: the registry being the kernel's dm tables, a removal is a
         mutation of it — the units are free the moment the table is gone —
         and a sweep that deleted a wrapper in the middle of another cntlr's
         enumerate → discard → create would both invalidate that enumeration
         and free a run under it. So the path that removes a wrapper from
         *outside* the allocator — the CN21 sweep's L4 — takes `cloneMetaMu`
         around that `dmsetup remove`, while the two that already hold it
         (this allocation's mismatched-wrapper removal, and the arena sweep
         of CN2) remove directly: the mutex is a leaf, held across those OS
         calls and never re-entered.
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
         dm-clone above it has been removed, so the removal does not EBUSY
         (a dm-clone whose removal failed keeps its wrapper mapped, which
         then cannot be replaced — below); that *is* the §11.5 rebuild
         path. On a recovery build (metadata missing or unusable, the
         dm-clone device gone, or a dm-clone that does not show hydration
         enabled — step 4) this step first parks the
         dst td's ns-devs on `CnErrorName` (`parkTdNsDevs`, no ANA move),
         removes the old dm-clone if one is still present, and only then
         allocates a missing wrapper or replaces a mismatched one — a
         matching wrapper is left alone. An old dm-clone whose removal fails
         is kept: step 3 converges it like any existing dm-clone, and step 4
         still applies every bitmap to it before step 5 (its wrapper
         matches — a mismatched one cannot be replaced while the dm-clone
         still maps it, and that failure ends this step). The
         `dmsetup status` read that decides the third case is step 2's own;
         when it fails or does not answer, step 2 fails exactly as on a
         failed `dmsetup info` of the dm-clone — `clone_id_to_meta` and
         `clone_id_to_dm_clone` `RES_STATUS_ERROR` (CN29) — the pass
         recovers, removes and enables nothing for that clone, and it
         registers the CN10 retry. CN16 reads the status again itself and
         moves no ns-dev onto a dm-clone that does not show hydration
         enabled (rule 5), so a td a killed build left parked stays parked
         until a pass that can read the status has recovered the clone.
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
         `dm_clone_conf`, one per non-zero member the probed status does
         not already show (probe-first, SH16; a zero leaves the target's
         own default in place, `architecture.md` §7).
      4. Apply every locally present bitmap chunk — the CN22 fold over the
         `(src_slice_idx, bm_idx)`-addressed chunks this node holds, read in
         place — and, when
         this build is a **§11.5 recovery** (the dm-clone's metadata is
         missing or unusable: a CN reboot takes the tmpfs, the loop device
         and every kind-`cb` wrapper together; a failover to a CN that never
         ran the clone; or the dm-clone device itself vanished while a
         healthy wrapper stayed behind —
         `TestCloneRecoveryWhenOnlyTheDmCloneVanished`; or the dm-clone is
         up but its `dmsetup status` does not show hydration enabled —
         `no_hydration` among its feature args, or a status that is not a
         dm-clone's: step 5 alone enables hydration, so an agent killed
         between steps 3 and 5 leaves exactly this, with step 4 perhaps half
         done — `TestCloneRecoveryResumesAfterAKillBetweenCreateAndBitmaps` —
         and so can a pass that failed between them without removing it,
         for example through a create killed after its ioctl ran, a refused
         enable, or step 4's fail-closed removal failing too —
         `TestCloneRecoveryNeverServesAnUnfinishedDmClone`; a removal that
         succeeds leaves the dm-clone gone instead, the vanished-device case
         above),
         first the **dst** bitmaps: with every affected ns-dev parked
         on `CnErrorName` (by CN9's pre-step 2 when the namespace is
         effectively suspended or the cntlr is standby and no unanswered
         listing stopped the sweep (CN21), and otherwise by
         step 2 of a recovery build, before the old dm-clone is removed —
         `parkTdNsDevs` takes a *serving* namespace off
         the td with no ANA move, and that IO-error window is this
         recovery's own), read the td's mapping bitmap from every slice pool
         (the CN25 machinery, B-side of §11.4) and `blkdiscard` every
         mapped region. Mapped ⇔ already copied holds because the dst td
         started empty [D3]. Dst bitmaps not applied in full — a read or a
         `blkdiscard` that fails — fail the clone closed: the dm-clone is
         removed, `clone_id_to_dm_clone` reads `RES_STATUS_ERROR`
         `"destination bitmaps not applied: …"`, and the CN10 retry is
         registered; a dm-clone whose removal fails there — as it may when
         step 2 could not remove it either — stays up with hydration off,
         which CN16 does not serve through (rule 5).
      5. `dmsetup message … enable_hydration` — only after step 4, so a
         recovered clone can never re-fetch a region the destination
         already owns (§11.5's staleness hazard).
      6. Put the dst td's ns-devs onto the dm-clone and set ANA per CN16 —
         a reload when they already exist (recovery, enable transitions); a
         fresh converge simply creates them in CN16 with the clone backing
         (rule 5), which CN16 installs only once the dm-clone's status shows
         the hydration step 5 enabled. With `auto_resume = false` the
         namespaces stay effectively suspended until
         `UpdateNamespaceSuspended`.
      Any failure of step 1 registers the CN10 retry, and so do these later
      ones, each of which stops the build before step 5 has enabled
      hydration: the arena listing, step 2's `dmsetup info` or
      `dmsetup status` of the dm-clone, anything step 3's converge of the
      dm-clone fails on, step 4's dst bitmaps, and the enable (its own
      status read included). So does a failed `dmsetup status` read after
      the enable, whose line the dm-clone row carries (§9.5). The one other
      failure of steps 2-5 that stops the build — of step 2's wrapper
      allocation or replacement (`ensureCloneMeta`), the arena's refusal of
      the slot among them — registers nothing and is left to its rows:
      `clone_id_to_meta` `RES_STATUS_ERROR` and `clone_id_to_dm_clone`
      `"metadata wrapper missing"`. Some failures stop nothing and are only
      logged: step 2's failed removal of an old dm-clone (which is then
      kept, above), a failed knob message of step 3 (the knob keeps the
      value it had) and a failed source-chunk `blkdiscard` of step 4 (it
      costs an extra copy). Step 6 is CN16's: in a pass that finished
      steps 1-5 and the read after them (the dm-clone row `OK`), a failure
      to converge a rule-5 ns-dev of the td registers the retry too
      (CN16 rule 5). Whatever stopped a pass, CN16 puts the td's ns-devs on
      the dm-clone only while its status shows hydration enabled (rule 5),
      which no dm-clone shows before step 5 has run on it; a later pass —
      the retry's, where one was registered — finishes the build.
      **Removing a clone** is not a step of its own: it is the CN21 sweep
      finding the clone's objects unwanted and taking them in layer order,
      which is exactly the order this stack needs. A clone that **left
      `clone_list`** loses all three; one the role or the level merely
      suppresses loses the dm-clone and the wrapper only — the source
      connection stays, because L5 reads "in use" off every stored cntlr's
      `clone_list` (plus the live kind-`c7` tables), and a suppressed clone
      is still in that list.
      * The ns-devs above the dst td go first. A *wanted* ns-dev whose live
        table still maps the dm-clone is parked on the td's `CnErrorName` by
        CN9's pre-step 2 — it has to be, or the removal below fails EBUSY —
        and the build phase of the same pass then puts it on whatever CN16
        now wants: the raid0 while this cntlr still serves it (rule 6), the
        `CnErrorName` otherwise (a standby; a still-listed but
        level-suppressed clone at `sp_level ≥ NO_CLONE`, rule 4; or
        `sp_level ≥ NO_THINPOOL`, rule 3). An ns-dev that is itself unwanted
        is parked by P0 and removed at L2.
        On a pass whose sweep an unanswered listing stopped (CN21) neither
        pre-step 2 nor L3 runs, so the dm-clone stays loaded, and one whose
        hydration is on and not finished goes on copying into the raid0
        whether or not anything has it open: its copy of a region not yet
        hydrated would overwrite a write a host made to that region of the
        raid0 directly. A pass whose listings answer can leave it loaded
        too: L3's remove fails or is killed — behind an ns-dev whose park
        failed its load, say, which stays suspended over the dm-clone
        (CN16) — or the descent stops above L3. So while a dm-clone the
        plan does not want may still be live — L3 did not run or left one;
        on a stopped pass, the dm listing did not answer or names one — the
        build puts no ns-dev onto a td's raid0 it is not on already. One
        over the dm-clone stays there and serves on through it, in the ANA
        group it has. One parked stays parked and goes `inaccessible`:
        pre-step 2 may have parked it on this very pass while it was
        optimized, and its hosts should queue rather than take IO errors. A
        new one is created parked. None of them is moved to `optimized` on
        that pass; each reports its ns-dev row `ERROR` in the probe's words
        (`table is not the desired namespace backing`), and the hold
        registers the CN10 retry, because a hold that an unanswered
        `dmsetup ls` alone caused leaves nothing for the Check verdict to
        name once the listing answers again. The first pass whose L3
        removes the dm-clone takes the order above — the park of one still
        over the dm-clone, L3, then the reload.
      * **L3** removes the dm-clone, **before** its source connection dies —
        dm-clone flushes through the source on removal and blocks without it.
      * **L4** removes the metadata wrapper `CnCloneMetaDmName` under
        `cloneMetaMu`; its units are free again the moment the wrapper is
        gone, because the next registry enumeration simply no longer sees
        them and nothing is written.
      * **L5** disconnects the source subsystem — the third object, and the
        one only a clone that has left `clone_list` loses (`--nqn` is safe
        here: every path of it is going). The disconnect runs off the pass
        (CN10), so the connection is that pass's leftover until a later pass
        finds it gone, and a clone created on the same source meanwhile
        waits for it rather than adopt it (step 1). The set of unowned
        sources is computed at L5 rather than when the chain was built,
        because until L3 has actually removed it the dm-clone still maps its
        own source and would keep it claimed.
      The clone's `clone-bm-*` files are **not** part of that chain. They are
      swept separately, at the end of the cntlr-level sweep (CN21), against
      the stored `clone_list` (SH7) — a clone that left the list loses them, one
      the role or the level merely suppresses keeps them applied-by-file
      (CN19, CN22 — deleting them on every standby converge would make the
      worker re-push them forever, and a promoted standby's §11.5 rebuild
      would have nothing to skip with; `TestSuppressedCloneKeepsItsChunks`).
      Keying that deletion off the wrapper's removal instead would leak every
      deleted clone's chunks on a **standby**, which builds no wrapper at
      all. A clone whose dst td is
      provisioning-deferred (CN9) is never built in the first place: no
      metadata slot, no dm-clone, no source connect, and its three rows report
      `RES_STATUS_PROVISIONING`.

CN19. **`sp_level` gating** (§11.7; numeric comparisons — the enum values
      are ordered). Levels are desired state, and they act on the converge
      through the **wanted set** alone (CN9): raising a level shrinks it, so
      the sweep phase takes the layers down; lowering it grows the set again
      and the build phase rebuilds them. There is no level-specific teardown
      code anywhere. Bitmap chunks stay applied-by-file throughout (SH21).
      A resource suppressed by the level is reported `RES_STATUS_MISSING`
      with `details = "sp_level"` (`common.ResDetailsSpLevel`, §2.1, which
      the worker's settle reads). A resource *deferred* by CN9's
      provisioning gate is a different thing —
      `RES_STATUS_PROVISIONING` with `details = "provisioning"`, at every
      level — and where both apply, the level wins (CN9).

| condition | additional cn behavior |
|---|---|
| `level >= SP_LEVEL_READONLY` (16) | every user-facing ns-dev on the primary carries the dm-flakey `error_writes` table over its normal backing (CN16 rule 7) — reads served, writes error ([D11]); an **effectively suspended** namespace is exempt, since rule 1 parks it on the td's dm-error above the flakey rule and it is `inaccessible` anyway; clone/migration hydration, transfers, health probes all unaffected |
| `level >= SP_LEVEL_NO_CLONE` (32) | no clone stacks: the dm-clone and its metadata wrapper are removed (the wrapper's arena units freed), but a `:4:` source connection made at a lower level **stays connected**, and is not even reported as a leftover — the sweep's in-use test is level-blind (CN21: a source is in use while any stored cntlr's `clone_list` names it, which `sp_level` does not shorten, or a live kind-`c7` table maps it), so it is disconnected only once the clone leaves `clone_list` or the cntlr's stored state goes; ns-devs of clone-target tds on `CnErrorName` (CN16 rule 4), so an `auto_resume` clone's destination namespace stays exported and `optimized` and its host takes IO errors instead of queueing (CN16) |
| `level >= SP_LEVEL_NO_THINPOOL` (48) | no thin pools, thin volumes, raid0s or pool concats; every ns-dev and every primary xfer device on `CnErrorName`/error tables; namespaces stay exported with CN16 ANA (a host sees IO errors, not a vanished device) |
| `level >= SP_LEVEL_NO_REDUND` (64) | no group devices (md arrays stopped, `CnGrpName` linears removed); legs stay connected, wrapped and probed |
| `level >= SP_LEVEL_NO_MIGRATION` (80) | nothing — the level has no CN-side behavior (the migration dm-clone is a DN object; the CN's leg multipath needs no gating) |
| `level >= SP_LEVEL_NO_SIDE` (96) | no leg connections or wrappers (their sides are no longer exported) and no health probes; host-facing and xfer subsystems remain, error-backed |
| `level >= SP_LEVEL_DISABLE` (112) | the wanted set is **empty**, which is the whole of this row: the CN21 sweep removes every cntlr-scoped object of the sp, and only the §3.2 base state remains (tmpfs, backing file, loop device, port — every kind-`cb` wrapper **of this sp** went with its clone, so the units they held are free again; the arena itself is node-wide and another sp's wrappers stay). The `cntlr-*`/`clone-bm-*` files stay — desired state persists. Two things an empty wanted set cannot express are done explicitly beside it, because neither is an object on the node: the CN10/CN18 connect retry is cancelled, and every `ResInfo` history of the cntlr is dropped |

CN20. Persist (SH5); reply `agent_reply`, `revision`, `cntlr_info`,
      `bm_info_list`.

      **`agent_reply` is the sweep's verdict.** It is `ReplyCodeLeftover`
      (4) iff this cntlr's sweep was not clean — the node still holds an
      object of this sp that the desired state does not want, or one of the
      enumerations did not answer — and `0` otherwise. `details` is
      `leftover({n}): {kind}:{name}, …` with at most 8 names and a
      `[+k more]` tail, plus one `enumeration failed: {what}: {err}` per
      unanswered enumeration; the full list goes to the agent log every
      pass, one `Info` record `"sweep leftover"`, so a lingering leftover is
      visible every round and not only once. The code is **not** a
      rejection: the request was applied, the desired state is stored, every
      wanted object was converged, and `revision` is the request's own — the
      worker evaluates
      the rows exactly as for code 0 and re-issues the `SyncupCntlr` until
      the code changes. Nothing about it is stored: it is recomputed by
      enumerating the node on every `Syncup*` and every `Check*` (CN30).
      A cntlr the §7 conf gate refused has no verdict at all (CN8): nothing
      was converged and nothing enumerated, so the reply carries
      `ReplyCodeInvalidConf` and the sweep never ran.

      `bm_info_list` is one `BitmapInfo{res_id = clone_id}` per `clone_list`
      entry of the (now-stored) request, its `chunk_id_list` derived from the
      `clone-bm-*` files present (SH21): one `BmChunkId{src_slice_idx,
      bm_idx}` per chunk this node holds for that clone, ascending by the
      pair. `bm_idx_list` is left **unset** — that field is the migration
      applied set (`SyncupSideReply.bm_info`), and a flat index cannot name a
      pair-addressed chunk. A clone with no chunks reports a `BitmapInfo`
      with an empty `chunk_id_list`, which is what tells BM2 to push
      everything etcd holds.

### 4.7 The sweep

CN21. **Two scopes, one chain.** The principle — removal is actual minus
      desired, verified by probe, with nothing about a past failure
      remembered — is `architecture.md` §9's (teardown by sweep) and is not
      restated here. The cn agent runs it at two scopes:
      * **node-level**, in `SyncupCn` (CN7) and the startup reconcile (CN2),
        under the node **write** lock: every sp that appears on the node and
        is **not** in the stored `cntlr_pointer_list` — wanted set empty —
        plus the cross-sp objects of the attribution rules below. The write
        lock is what makes judging an *unowned* object safe: no cntlr
        converge, no Check round and no push runs beside the pass. A
        disconnect the pass sets going can complete after the lock is
        released (CN10), so it is CN10's registry, read by the CN10/CN18
        connect steps, that keeps a converge from adopting that connection
        meanwhile.
      * **cntlr-level**, in `convergeCntlr` in the old retire phase's place,
        under that converge's CN1 locks — node **read** plus that cntlr's
        object lock on the `SyncupCntlr` and connect-retry paths, the node
        write lock on the startup one, which is only stronger: this
        cntlr's sp minus the CN9 wanted set. It removes only objects
        attributed to its own sp, so two cntlrs of one CN sweep concurrently
        without meeting. The one exception is L5's clone-source connections,
        which belong to no sp at all and which **both** scopes disconnect —
        safe because "in use" is read from **every** stored cntlr's request
        (and from the live tables), and a cntlr's request is stored before
        its converge issues any `nvme connect`, so a source another cntlr is
        about to use is already claimed; and a converge that finds a source
        whose disconnect is still in flight neither adopts nor reconnects it
        until that disconnect has returned (CN10). One window stays open.
        Another cntlr's request can be stored after this pass has read the
        stored requests, and that cntlr's CN18 step 1 can run before this
        pass has set the disconnect going. That converge finds the source
        still connected and adopts it, and the disconnect then deletes the
        source under the new dm-clone (Known limits).
      An sp that **is** in the pointer list gets **no chain of its own** at
      node level, even with no cntlr file: CN8 introduces the pointer before
      the `SyncupCntlr`, and after a lost `--local-store` the resources exist
      and must be re-adopted probe-first by that syncup rather than swept.
      That is a statement about the sp-scoped chains only. The node-level
      scope's other steps — the arena step (CN2) and the unowned subsystems
      and source connections — are keyed on the **stored cntlr requests**,
      not on the pointer list, and they do reach an sp that is in it: with
      the `--local-store` lost there is no `cntlr-*` file, so no `clone_list`
      names any kind-`cb` wrapper and the arena step attempts every one of
      them on the node. What that costs is bounded rather than prevented —
      a wrapper a live dm-clone still maps refuses with EBUSY and is only
      reported, and one that does go is rebuilt by CN18 on that sp's next
      `SyncupCntlr` — and it is why the arena step is named separately
      instead of reading as one more thing the pointer list protects.

      **Enumeration.** `dmsetup ls` (name → `major:minor`, which is also how
      a live table's device argument is resolved back to a name), the sysfs
      md enumeration of CN12 (`ListArrays`, strict: one array that did not
      answer fails it — never `Md.Walk`'s unanswered rule), the sysfs
      subsystem walk for nvme host connections, and `ls` of the nvmet
      `subsystems` tree — linked to the port or not, a partially removed
      subsystem is unlinked but present. An enumerator that
      **did not answer** leaves its part of the snapshot empty and is
      reported as `enumeration failed`; it never reads as "there is nothing
      there". An empty part is exactly what "nothing there" looks like to
      every removal gated on absence (`architecture.md` §9.8), so with any
      one of the four unanswered the sweep removes nothing from the node at
      either scope: no chain runs, neither do CN9's pre-steps 2 and 3, and
      what the chain would have removed is only reported. The md
      enumeration is the sharpest case: unanswered, it would leave L9 no
      array to stop and let L10 disconnect unwanted legs from under a live
      one. A cntlr-level converge whose sweep is stopped this way still
      runs three things that read no snapshot, besides CN19's two explicit
      steps at `SP_LEVEL_DISABLE`. One is CN9's pre-step 1, the
      ANA move: it decides from the plan alone and removes nothing, and the build
      phase that follows parks whether or not the sweep ran — it reloads
      an ns-dev whose CN16 backing is the td's `CnErrorName` onto it, and
      CN17's converge demotes a transfer device this cntlr does not serve
      — so without the move a demoted or suspended namespace would be
      served from an error table while its path still reads `optimized`.
      The second is the pair of local-state sweeps ("Not on the node"
      below): they decide from the request alone, so an unanswered listing
      is no reason for them to wait. The third, ahead of the sweep, is the
      trim of the CN11 probers (below), which decides from the plan alone
      and removes nothing from the node. The build phase of such a converge in
      turn holds back the one thing that is safe only after a step the
      stopped sweep skipped: while a dm-clone the plan does not want may
      still be live it puts no ns-dev onto a td's raid0 it is not on
      already — that waits for L3 to remove the dm-clone (CN18), as it does
      after a chain whose L3 did not run or left one. A
      namespace that has left `ns_list` it never touches, stopped sweep or
      not: that is L1's alone (below), after P0 (CN16).
      The snapshot is thrown away at the end of the pass: it decides
      only what to *attempt*, and every removal re-probes its own object.

      **Attribution.** A dm device is ours by its parsed name (§2.1): role
      `c`, our cluster, our cn; its sp is `Ids[0]`. An **md array** carries
      no sp in its name and is ours only when *every* member is a kind-`c9`
      wrapper of ours naming one sp — an array with a foreign, unparsable or
      differently-owned member is silently skipped and never stopped, which
      is what leaves a co-hosted dn agent's udev-assembled array alone. A
      `:2:` host connection is ours by the cn id **inside the NQN**, which is
      how it is told from one another CN of this cluster holds. A `:4:`
      clone-source connection of our cluster belongs to no sp — its NQN names
      the **source** sp, not the cntlr that dials it — so the only test there
      is is "does somebody here still want it": it is **in use** iff some
      stored cntlr of this CN names it as a clone's `src_nqn` **or** a live
      kind-`c7` table maps its namespace device, and unowned otherwise. A
      subsystem in the nvmet tree whose NQN is **dnv-format** says whose it
      is outright: a `:4:` subsystem of our cluster is the transfer export of
      the sp its second id names, and an NQN that carries the dnv prefix but
      decodes to nothing is left alone (§2.1) rather than falling through to
      the rule below. Only a name that decodes as neither — no dnv prefix at
      all — reaches that rule. A
      **host-facing** nvmet subsystem carries a user-chosen NQN and therefore
      carries no ids at all, so it is attributed in two steps. **First**, a
      stored cntlr whose `nqn_to_subsystem` still names it settles it, under
      that cntlr's sp, with nothing read from configfs: a cntlr's request is
      in memory (`putCntlr`) before its converge builds anything, so no
      subsystem can exist whose claim is not already visible here — the
      local-store `Save` comes after the converge (CN20), so the guarantee is
      about this process's state, not about the files. That order is
      load-bearing rather than a saved read — a subsystem whose last
      namespace has just been removed has nothing left to attribute it by,
      and the namespace rule would call it unowned and take the host's
      subsystem away while `nqn_to_subsystem` is still asking for it.
      **Second**, for a subsystem no stored request claims, the namespaces: a
      `device_path` that parses as a kind-`c6` ns-dev of ours makes it that
      sp's; one that parses as a kind-`c6` ns-dev that is **not** ours —
      another cn's, another cluster's — makes the subsystem **foreign** and
      untouchable, foreign rather than unowned because the unowned arm
      *removes*; and one with no attributable namespace at all is unowned.
      *Those* only the node-level scope removes, together with the orphan
      kind-`cb` wrappers (CN2).
      Calling anything unowned assumes **at most one cn agent per kernel**; a
      co-hosted dn agent is fine, because its NQNs are all dnv-format kinds
      `2` and `3`, which never reach that arm.

      **P0, before every layer**: every **unwanted** ns-dev is parked,
      unless the park fails (below). It has no plan — nothing wanted names
      it, and its td may be leaving in the
      same pass — so the park is derived from the device's **own live
      table**. Only one backing names a td: a kind-`c4` raid0, whose ids are
      also its td's kind-`c5` dm-error's, so there the target is CN16's own.
      Anywhere else — a table this build never wrote, an ns-dev over a
      dm-clone — the device is reloaded onto an error table of its own size,
      which is the same thing one indirection shorter. A device already
      parked is left alone but **resumed** if it is suspended. Both halves
      matter: disabling
      the nvmet namespace above it in L1 closes its backing device, and that
      does not complete on a suspended dm device; and the reload's own
      flushing suspend is what completes the in-flight host IO instead of
      replaying it at resume onto a stack that is about to go.
      A park that fails does not stop the chain: L1 still runs on that pass,
      and an ns-dev whose load failed is still suspended on its old table
      (the reload fails closed, CN16) — the suspended device the first half
      is there to keep from L1 (Known limits).

      **Layers, strictly top-down.** Within one sp's chain:
      * **L1** — nvmet. Each unwanted namespace under a surviving subsystem
        attributed to the sp (above)
        goes `AnaGrpIdInaccessible` and is then removed (a host still holding
        a path is told to stop using it rather than losing it under IO) —
        by this layer alone: the build phase's `ensureSubsystem` converges
        the wanted namespaces and removes none, so a pass whose enumeration
        did not answer removes none either;
        then the unwanted subsystems whole (port link, ns disable, rmdir ns,
        allowed-hosts unlink, rmdir subsystem). First because nvmet must
        release the dm devices below before anything can remove them.
      * **L2** — ns-devs and xfer finals: what L1 just released.
      * **L3** — dm-clones, **before** their source connections: dm-clone
        flushes through the source on removal (CN18).
      * **L4** — clone-metadata wrappers, under `cloneMetaMu` (CN18: the
        tables are the registry, so a removal is a mutation of it).
      * **L5** — `:4:` source connections that nothing maps any more. The set
        is evaluated **here**, not when the chain was built: a snapshot taken
        before L3 still shows the dm-clone this pass is removing mapping its
        own source, and deciding early would never disconnect it. Their
        disconnects run off the pass (CN10), so a pass that sets one going
        leaves it behind and, by the stop rule below, the layers under L5
        wait for the pass that finds it gone.
      * **L6** — per-td raid0s and dm-errors.
      * **L7** — each thin volume removed and then, only if its pool is one
        the desired state still wants, the pool-side `delete` of its id
        under CN14's rule.
      * **L8** — thin-pools, then the concats under them.
      * **L9** — md arrays (`mdadm --stop` on the node **sysfs** named, CN12)
        and `CnGrpName` linears.
      * **L10** — legs, in the one order that is deliberately **not**
        top-down: **set the legs' disconnects going** (whole-NQN is fine
        here: every path of an unwanted leg is going; off the pass, through
        the CN10 disconnect registry), **then remove the leg wrappers**
        without waiting for them. Their probers are cancelled already, by
        the converge's trim (below) or by CN7's drop step, but a wedged
        prober still holds an open fd on the wrapper, so its removal fails
        EBUSY until the disconnect has errored the queued IO and the
        prober's fd has closed (§2.2, CN11); setting the disconnects going
        first only gives a quick one the chance to release such a wrapper
        within the pass — otherwise it goes on a later pass. A wrapper is
        removed even when its own connection is still there: they are two
        different objects, and the connection is a leftover of this pass
        that a later pass's probe finds gone.
      No `delete` message is ever sent for a thin volume whose pool is itself
      going (CN14: this is deactivation, the metadata on the legs is the next
      CN's to find, and a created td's volumes are re-attached there without
      any message — `ThinDeviceCreated.md` U4), and nothing in any layer runs
      `mdadm --detail` (CN12).

      **"Gone" is probed, never inferred from an exit status**: `dmsetup
      info` for a dm device, `array_state` for an array, a configfs read for
      a namespace or subsystem, the sysfs walk for a connection. A command
      killed at the soft timeout may well have completed in the kernel — the
      ioctl finishes whatever happens to the process — so only a fresh probe
      can say, and a probe that itself did not answer counts as **not
      removed**. For a connection, "gone" is the absence of any
      **controller**, not of the subsystem directory: the kernel keeps
      `/sys/class/nvme-subsystem/nvme-subsysN` after its last controller is
      deleted, attributes still readable, controller and namespace nodes
      gone. Such a subsystem holds nothing open — no block device, no path —
      so waiting for the directory itself would report a leftover for ever
      and re-drive the worker every round over a connection that no longer
      exists.

      **The stop rule** (`architecture.md` §9.8): every removal of a layer is
      attempted, but the chain does **not** descend below a layer that left
      anything behind; the layers underneath report their objects as
      leftovers without being touched. Continuing would disconnect a leg
      under a live array, which is destructive on the migration path — and a
      pass that simply re-runs next round costs nothing, because by then the
      failfast window has passed and the same order succeeds.

      The same rule applies **inside** a layer that has an order of its own.
      L8 names the concats as leftovers without attempting them when the pool
      above them would not go: they are present and unwanted whether or not
      the pool still maps them, and a leftover nothing names is a leftover
      nothing re-drives. L5 is the one layer whose set is computed when it is
      *reached* rather than when the chain was built, so a stopped descent
      still gets a truthful answer there too: a source still mapped by a
      clone that would not go reads as **in use** and is not named, while the
      source of a clone that did go is.

      **Not on the node.** Two pieces of the cntlr's *local* state follow the
      same rule and are swept at the end of the cntlr-level sweep of every
      converge — one that an unanswered listing stopped included — ahead of
      the build phase: the
      `clone-bm-*` files of every clone no longer in `clone_list` (SH7 — a
      sweep of the store against the **request**, not a side effect of
      removing the clone's wrapper, because a standby builds no wrapper at
      all and keying the deletion off one left a deleted clone's chunks on
      disk for ever), and the `ResInfo` histories, pruned to the keys this
      plan's objects can use (SH14) so a later rebuild of the same id reports
      a fresh epoch rather than the dead object's. A clone the role or the
      level merely *suppresses* keeps its chunks (CN19, CN22). The cntlr's
      in-memory index of its chunk files lets a clone's entry go only once
      the `rm` of its files succeeded: one that failed or did not answer
      keeps them indexed, and the converge that tried names each as a `record`
      leftover, as does the read-only verdict (CN30) until a pass removes
      them. Dropping the entry first would leave the files on disk with
      nothing short of a restart ever listing them again, and no reply
      naming them.

      The connect-retry registration and the leg probers are not swept —
      they are goroutines, not objects on the node. A cntlr that is dropped
      loses both in CN7's drop step; a cntlr at `SP_LEVEL_DISABLE` loses the
      retry in CN19; and every converge trims the probers to the legs the
      plan still probes (CN11) before its sweep, whether or not the sweep's
      chain runs or reaches L10: the trim decides from the plan alone and
      removes nothing from the node. Left to L10, it would wait on every
      layer above, and a descent stopped at an array that would not stop,
      or a sweep stopped by an unanswered listing, would leave a demoted
      primary's probers probing through the standby's legs, and a departed
      leg's through its wrapper, until some pass got that far. Coming
      before the sweep, it still precedes L10's disconnect, which is what
      releases a prober wedged on a pathless leg.

### 4.8 `PushCloneBitmap`

CN22. Gate: the cntlr file must exist and its stored `clone_list` must
      contain `clone_id`, and the chunk's two indexes must each be in range
      — `src_slice_idx < that clone's src_slice_cnt` **and** `bm_idx <
      MaxCloneBmCnt` — bounded **independently**, in one
      `ReplyCodeUnknownObject` naming both indexes and both limits. Neither
      bound may stand in for the other: `src_slice_cnt` is the clone's own
      source geometry, `MaxCloneBmCnt` the cap on the chunks of one slice's
      bitmap, and an in-range `bm_idx` says nothing about `src_slice_idx` or
      the reverse. Those three refusals are the **whole** gate: the request
      carries no `revision` field and this handler compares none. A chunk is
      position-addressed data at a `(clone_id, src_slice_idx, bm_idx)` whose
      ids are never reused, and a push never advances the stored revision, so
      a chunk the worker planned against a report the cntlr has since
      superseded is still exactly the right bytes at exactly the right offset
      — the gate this replaced only ever threw away work that was about to be
      redone (§9.6, `dnv-worker.md` BM3; `TestPushHasNoRevisionGate`). Then
      SH21 with the clone twists of §9.6: persist the chunk at
      `LocalCloneBmPath(cluster, cn, sp, clone_id, src_slice_idx, bm_idx)` —
      **overwriting** when the payload differs, because clone chunks may
      grow in place ([D8]); a byte-identical payload is already persisted and
      already applied, and is skipped outright — then recompute and apply.
      Clone chunks are self-positioned by the pair (SH22): chunk `(s, b)`
      holds bytes `[b·C, b·C + len)` of source slice `s`'s bitmap,
      `C = CloneBmChunkBytes`, at an offset no other chunk's existence or
      length can move. They carry raid0 geometry, and `thinbm.go` folds the
      chunks present **in place** — never through a reassembled per-slice
      bitmap, which a missing chunk would shift every later chunk inside —
      into one device-order wire bitmap over the dm-clone's regions: region
      `r` is skippable iff **every** source bit it covers under the §11.4
      address mapping (`src_slice_cnt`, `src_stripe_size`, `src_block_size`,
      region size = `block_size`) reads as set, and bit `k` of slice `s`
      reads as set only when chunk `b = k/(8·C)` of that slice is present,
      byte `(k mod 8·C)/8` is within its length, **and** that bit is 1. An
      absent chunk, the tail of a present-but-short one ([D8]:
      `AppendCloneBitmap` may still be growing it) and a slice holding no
      chunks at all therefore all count as written — the safe direction,
      which can only cost an extra copy. The folded bitmap feeds the shared
      `agent.SkipRanges` with `shiftRegions = 0` (clones have no meta
      region — that shift is the dn's) and is `blkdiscard`ed onto
      `CnCloneFinalName`. If the dm-clone does not currently exist (standby,
      not built yet, level-suppressed), the file still counts as applied;
      chunks are re-applied whenever the dm-clone is (re)created (CN18 step
      4). A **failed persist** is logged and still acked code 0, with neither
      the fold nor the `blkdiscard` attempted. For a pair the node does
      not hold yet that heals itself: the chunk stays out of the applied
      set, so it stays out of the clone's `BitmapInfo.chunk_id_list` (CN20)
      and the worker's BM2 diff pushes it again. That diff is the whole
      recovery, and it is eventual rather than prompt: no push outcome arms
      anything on the worker (BM6 — a failed push is logged and ends its
      plan, and there is no flag to raise), and `bm_info_list` rides no
      `CheckCntlr` reply, so the re-push waits for whatever next makes the
      worker issue a `SyncupCntlr` (RW4 step 5) — a revision change, a round
      this cntlr rejects or answers with another revision, or a round whose
      reply carries `ReplyCodeLeftover` (CN30). A failed persist of a
      **grown** chunk ([D8], architecture.md §9.6) is the one case that does
      not heal that way: the
      pair is already in the applied set with the shorter payload, so
      `chunk_id_list` keeps advertising it, and the code-0 ack also refreshes
      the worker's BM5 memo — which keys on `(clone_id, src_slice_idx,
      bm_idx)`, so only that one chunk is affected — and the node keeps the
      shorter version until `AppendCloneBitmap` grows that chunk again, the
      same bounded,
      correctness-neutral loss [D8] already accepts. Reply `agent_reply`
      only.

### 4.9 `GetCnInfo` / `GetCntlrInfo`

CN23. Read-only: probe fresh under the CN1 locks and reply `agent_reply`,
      `revision`, the info. An unknown CN (no `cn-*` file) or cntlr pointer
      ⇒ `ReplyCodeUnknownObject` with `revision = 0`. For a known object the
      `agent_reply` is the read-only verdict of CN30. Never mutates.

### 4.10 `CheckCn` / `CheckCntlr`

CN24. Instantiate the SH24-SH26 loop with the §4.12 probes; one round takes
      the CN1 locks of the corresponding `Get*Info`, and its `agent_reply`
      is the same read-only verdict (CN30) — which is what drives the
      worker's re-sync while a leftover is still there. Like every SH24
      round it runs under its request's `trace_id` (the stream's id when
      that is empty).

### 4.11 `GetThinDeviceBm` / `GetLegBm`

CN25. Both serve the §8.13 gateway reads from a **dm-thin metadata
      snapshot** and report failure only through the gRPC status (§9.1 —
      they have no `AgentReply`): unknown cntlr, non-primary role, a
      missing pool, or a failing command all end the RPC with an `Internal`
      status naming the step. Under the CN1 locks (node read + object —
      the snapshot must not race a converge):
      `dmsetup message {CnPoolFinalName} 0 reserve_metadata_snap`, then
      `thin_dump --metadata-snap {DmPath(CnPoolMetaName)} -o {file}` (the
      `thin-provisioning-tools` reader), then **always**
      `release_metadata_snap` — on the success path and on every error
      path, on a context the caller's cancellation does not reach (still
      under the soft timeout), because a leaked reservation blocks the next
      reserve and pins the pool's metadata blocks; on the caller's own
      context, a caller that went away before the release, such as one
      whose client gave up in the middle of the dump, would take the
      release with it and leave the reservation held until some later
      reserve on that pool met it. A reserve that fails "already reserved"
      — a reservation an earlier reserve made and nothing released, such
      as one whose reserve was killed after its message ran — is released
      and retried once.
      The XML goes to `{file}`, never to stdout: the `os command` record
      logs a command's stdout in full (`log.md`), and a dump is one element
      per mapped run — tens of MB on a fragmented slice, once per slice per
      read — while the `os read file` record of reading it back carries a
      truncated excerpt. `{file}` is `{CnPoolMetaName}.thin_dump.xml` in
      the private directory `/run/dnv-thin-dump`. Before the reserve,
      every dump runs `mkdir -p -m 0700` of that directory and
      checks with `stat --format "%u %a %F"` that it is the agent's own
      user's, mode `700`, a directory — `stat` reads the entry itself, so a
      symlink answers as one — and anything else fails the dump before the
      pool is touched. The agent writes the dump as root, and `thin_dump`
      opens its output with a plain create-and-truncate: under a fixed name
      in a world-writable directory, another local user could create the
      file first, own it through the dump and rewrite it before it is read
      back, and a forged destination bitmap (CN18 step 4) discards regions
      of a dm-clone that were never copied. Nobody else can create an entry
      in a `0700` directory, and only root can create, replace or rename an
      entry of `/run` (root's, mode `0755`). The directory's parent is
      `/run`, not a world-writable temp directory such as `/tmp`, because
      nothing repairs an entry that fails the check: `mkdir -p` leaves an
      existing directory's owner and mode as they are. A local user who
      created the name first in `/tmp` would then fail every dump on the
      node until an operator removed it — the bitmap reads, the CN14
      activation sweep, and CN18 step 4's recovery, which fails closed. In
      `/run` only root can have made such an entry. `/run` is a tmpfs, so
      until its `rm` the file holds as much memory as the document, which
      the command timeouts bound. The file is read back under the soft
      timeout, and once `thin_dump` has run an `rm -f` of it follows
      whatever the outcome, before the release and, like it, on a context
      the caller's cancellation does not reach — a dump that did not
      answer included, whether the soft timeout killed it or the caller
      went away, since a killed `thin_dump` may already have written it.
      Only an `rm` that fails, or an agent that dies between the dump and
      the `rm`, leaves the file behind; the name is fixed, so the pool's
      next dump overwrites it, and one whose pool is gone stays until a
      reboot clears `/run`.
      The `architecture.md` §7 command timeouts bound the dump; a pool whose metadata
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
      the clone-metadata arena and every other dm device, CN12's
      `/sys/block/mdN/md/` read for arrays — never `mdadm --detail`
      (*amended 2026-09-26*) — and the §5 **sysfs walk** — never `nvme
      list-subsys` — for every outbound nvme connection). Every row reports
      against the **effective** desired state (CN9): a resource the
      provisioning gate excluded is `RES_STATUS_PROVISIONING` with `details
      = "provisioning"`, never `RES_STATUS_ERROR`, and it never feeds
      `err_epoch`.

| `ResInfo` | `res_name` | probe |
|---|---|---|
| `CnInfo.port_info` | the agent's port id as `%d` — `"1"` unless `--nvmet-port-id` says otherwise, so on a node running several agents the rows differ | configfs `addr_*` reads match the `--tr-*` flags; the three [D4] groups present with their fixed states |
| `CnInfo.tmpfs_info` | the `CnTmpfsPath` | `findmnt` shows a tmpfs mounted there |
| `CnInfo.tmp_file_info` | the `CnTmpFilePath` | `stat` size = `CnCloneMetaAreaSize` |
| `CnInfo.loop_dev_info` | the `CnTmpFilePath` | `losetup --associated` lists exactly one loop device — this row covers the whole arena; `CnInfo.clone_vg_info` (field 5) is deleted with the clone VG (`reserved 5;`, [D14]) and per-clone metadata health lives in `clone_id_to_meta` |
| `ss_id_to_subsystem[ss]` | the subsystem NQN | configfs: present, cntlid range, serial/model (trimmed, SH17), allowed-hosts exactly as desired |
| `ns_id_to_namespace[ns]` | `"{nqn}/{ns_idx}"` | nvmet ns enabled, `device_path`, `uuid`/`nguid` (compared through `agent.SameNsId` — configfs reads both back dash-separated and lower-cased whichever form was written, so a byte-wise compare fails a healthy namespace forever; `dnagent.md` SH17), `ana_grpid` as CN16 desires — `3` while the backing chain is provisioning-deferred (CN9), and the row itself is `RES_STATUS_PROVISIONING` then |
| `ns_id_to_dm_linear[ns]` | `CnNsDevName` | `dmsetup table` matches the CN16 backing (flakey line included) — for a rule-5 ns-dev the one CN16 installs now, read the same way (`nsDevNow`): the td's `CnErrorName` while the dm-clone's status does not show hydration enabled, and `RES_STATUS_ERROR` with the read's error when that status read fails; an effectively suspended ns-dev is expected **live on the td's `CnErrorName`** and reports `RES_STATUS_OK`, `details = "parked"`; a dm-suspended ns-dev is `RES_STATUS_ERROR` `"unexpectedly suspended"` whatever the plan says; a provisioning-deferred one reports `RES_STATUS_PROVISIONING` over its permanent dm-error |
| `td_id_to_raid0[td]` / `td_id_to_dm_error[td]` | `CnRaid0Name` / `CnErrorName` | `dmsetup table` |
| `td_id_to_thin_info[td].slice_id_to_dm_thin[slice]` | `CnThinDevName` | `dmsetup table` (pool + dev_id) |
| `slice_id_to_dm_pool[slice]` | `CnPoolFinalName` | `dmsetup status`; `details` = the **raw status line** — the worker parses data and metadata used/total out of it for the §10.4 auto-grow. The serving pool stays `RES_STATUS_OK` with that raw line even while a deferred group waits to be grown in (CN13): `PROVISIONING` never marks the serving pool, because it would switch auto-grow off |
| `slice_id_to_meta[slice]` / `slice_id_to_data[slice]` | `CnPoolMetaName` / `CnPoolDataName` | multi-target `dmsetup table` matches the group concat; the comparison is against the **effective** concat (the list's leading run of non-deferred groups, CN9/CN13), so a not-yet-grown concat is `OK`, not a mismatch. A **deferred** slice's rows are `RES_STATUS_PROVISIONING` — deferred meaning either of its two group lists is non-empty and has no effective group left (CN9), not that every group is deferred |
| `grp_id_to_md_raid[grp]` | `/dev/md/{CnMdDevName}` or `CnGrpName` | RedundMdRaid1: the array holding the group's `leg_list` wrappers, read from `/sys/block/mdN/md/` — `array_state`, and for a running array `degraded`, `sync_action` and `sync_completed`, plus `dev-*/{state,block/dev,block/dm/name}` — never `mdadm --detail`, which opens a member and can block on a dead one for ~13 s (CN12; *amended 2026-09-26*, the failover ping-pong: the killed probe read `ERROR` for a leg's fault). A running array (degraded included) ⇒ OK with `details` = `array_state`, then `degraded` while `md/degraded` is non-zero, then the word of a sync that is running (`recovering`, `resyncing`, `checking`, `repairing`, `reshaping`; none while `sync_completed` reads `none`) followed by its `(<done> / <total>)` sectors — e.g. `clean, degraded, recovering (32768 / 2093056)`. The words follow mdadm's State line, which the suites grep, and `repairing` is ours; the state and the sectors are sysfs's (mdadm prints its progress as a separate `Rebuild Status` line). No answering array holds the group's legs (none matched, or the match stopped between the walk and its read) and no array of the walk is unanswered, or `array_state` `clear` ⇒ `RES_STATUS_MISSING`; an array that is not running (`inactive`, `broken`, …) ⇒ `RES_STATUS_ERROR` with the state as `details`; a foreign member of the matched array, two answering arrays holding the group's legs, a `/sys/block` listing or a read of the matched array that did not answer, or no match — or a match that stopped since the walk — while another array of the walk did not answer (it may be the group's own; `details` name it) ⇒ `RES_STATUS_ERROR`. Beside a match, an array of the walk that did not answer is ignored and the row reads the match (CN12, *amended 2026-09-26*: a read of another sp's array that did not answer must not turn this row `ERROR`). RedundNone: `dmsetup table`. A deferred group (CN9) reports `RES_STATUS_PROVISIONING` and no mdadm command runs |
| `leg_id_to_leg[leg]` | `CnLegName` | wrapper table + the CN11 prober outcome (primary; `RES_STATUS_PENDING` `"health probe pending"` until its prober's first completed round — a fresh wrapper, a promotion and an agent restart each start a fresh prober, CN11; *amended 2026-09-26*, was `RES_STATUS_OK`) / transport per desired side, from sysfs, plus `ana_state` in {`optimized`, `non-optimized`} on single-sided legs — two-sided legs liveness only (CN11) (standby; §5). A provisioning leg (non-empty `side_list`, every side `provisioned = false`, CN9) reports `RES_STATUS_PROVISIONING` and is neither connected, wrapped nor probed. A leg whose subsystem's sweep disconnect is still in flight (CN10's disconnect registry) reports `RES_STATUS_ERROR` with the converge's own details and is not probed |
| `xfer_id_to_dm_linear[x]` / `xfer_id_to_subsystem[x]` / `xfer_id_to_namespace[x]` | `CnXferFinalName` / the `XferNqn` / `"{XferNqn}/{ori_ns_idx}"` | `dmsetup table` / configfs, per CN17; a deferred transfer's three rows are `RES_STATUS_PROVISIONING` |
| `clone_id_to_target[c]` | the clone `src_nqn` | the §5 **sysfs walk** shows a live controller per `src_tr_conf_list` entry (match `/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` to `src_nqn`, then `/sys/class/nvme/{ctrl}/state`) — **not** `nvme list-subsys -o json`, which §5 already ruled out for CN12 and which the code never used here. While a sweep's disconnect of `src_nqn` is in flight (CN10's disconnect registry) it is `RES_STATUS_ERROR` with the converge's own details, whatever the walk shows |
| `clone_id_to_dm_clone[c]` | `CnCloneFinalName` | `dmsetup status`; `details` carries the raw status line (§9.5 — hydration progress; `DeleteClone`'s force check reads it). `RES_STATUS_ERROR` `"metadata wrapper missing"` when the arena could not supply the slot (CN18 step 2) |
| `clone_id_to_meta[c]` | `CnCloneMetaDmName` | `dmsetup table` of the kind-`cb` wrapper: present, length = the CN18-computed unit count × `CnCloneMetaUnit` / 512 sectors, and the table's backing device equals the **currently probed** loop path; any mismatch (e.g. a tmpfs remounted under a live agent) ⇒ `RES_STATUS_ERROR`, whose repair path is the §11.5 clone rebuild (CN18 step 2) |

CN29. Error capture (§9.1): a failed command marks that resource
      `RES_STATUS_ERROR` with the command output in `details` and the
      converge pass **continues** with the remaining resources. Probes
      never mutate — `reserve_metadata_snap` runs only inside
      `dumpThinMetadata` (the CN25 bitmap reads, the CN14 activation sweep
      and the §11.5 dst-bitmap read of CN18 step 4), never
      from `probe.go`. `RES_STATUS_PROVISIONING` is never produced by this
      path: it is assigned by the CN9 gate, not by a failed command; and
      `RES_STATUS_PENDING` is produced by the CN11 registry alone, never by
      a failed command (*amended 2026-09-26*). The group row's probe opens
      no md member device: it is CN12's sysfs read, so a dead member of an
      array that keeps another in-sync member no longer turns the row
      `ERROR` — it reads `OK`, with `degraded` once md has failed the member
      (an IO to it that errors does, and so does CN12's reconciliation
      failing it out as an extra; until then the dead member does not count
      in `md/degraded`), and the fault is its leg row's, from the CN11
      prober's own IO. md does not fail the last in-sync member of a mirror
      (dnv never sets md's `fail_last_dev`): an error it charges to that
      member — a failed superblock or bitmap write, which array writes
      bring, is one — marks the array broken instead. The array then
      refuses every write for as long as it runs, its `array_state` reads
      `broken` wherever it would read `clean`, and CN28 reports that as
      `ERROR` (*amended 2026-09-26*, the failover ping-pong).

      Two kinds of thing travel in `agent_reply` rather than in the rows:
      protocol failures (the CN8 gates, CN22's), and **leftovers**. A
      leftover is the one per-resource outcome that *cannot* ride in a row,
      and the reason is structural rather than a matter of taste: every
      `*Info` row is keyed by the id of a **wanted** object, and a leftover is
      by definition something nothing wanted names — the sweep found it by
      enumerating the node, not by walking a plan, and there is no id under
      which to file it. That is also why it must not be silent: the old
      teardown's per-resource failures had rows until the teardown dropped
      exactly the keys that would have carried them, after which nothing
      reported the object at all. `ReplyCodeLeftover` is where that outcome
      goes (CN20, CN30).

### 4.13 The read-only verdict

CN30. `CheckCn`, `CheckCntlr`, `GetCnInfo` and `GetCntlrInfo` reply
      `ReplyCodeLeftover` iff their scope's **verdict** is not clean. The
      verdict is the CN21 sweep with the removals left out: the same
      enumeration, the same attribution, the same comparison against the same
      wanted set, and **nothing touched** (CN23 — a probe never mutates).
      Details and log record are CN20's.

      It is recomputed every round and stored nowhere. That is the whole
      retry mechanism for a leftover and it is deliberately the only one: a
      leftover that has since gone stops being reported by itself, one that
      is still there keeps the code non-zero, and the worker's existing
      "re-sync while the code is non-zero" rule (RW4 step 5) re-issues the
      `Syncup*` that sweeps again. No agent-side loop exists for leftovers,
      because a second retrier for something the worker already re-drives
      would be invisible to the control plane. CN10's connect retry sweeps
      only as part of a converge it re-runs for a reason of its own, and a
      sweep's `nvme disconnect` completes off the pass (CN10's disconnect
      registry), where the next probe finds it gone; one that failed is
      issued again by the next pass that still finds the controller. A
      restart is covered by the same path: whatever the
      startup reconcile could not remove surfaces on the first `Check*` of
      the object that owns it (CN2). On the `Get*Info` side the code is
      carried but read by nobody — the gateway's Inspect passes those replies
      through and by AG3 does not apply the `AgentReply` convention to them —
      so the operator-visible form of a leftover is the worker's log and the
      `Syncup*` reply's `details`.

      **Each scope answers for its own leftovers.** A `cn`'s verdict is the
      node-level one — the sps whose pointer has left its list, plus the
      unowned objects and the arena — and a `cntlr`'s is that cntlr's sp
      alone, apart from the sp-less clone-source connections below. Neither
      reports the other's **sp chain**, because each drives
      its own `Syncup*`: reporting a cntlr's chain leftover on `CheckCn`
      would re-issue a `SyncupCn` that builds no chain for that sp at all
      (CN21: an sp in the pointer list gets none). Two kinds of object both
      verdicts can name. The first is an **orphan kind-`cb` wrapper**, and
      that overlap is correct rather than a leak: the node-level scope owns
      the arena step whatever sp the wrapper belongs to (CN2), while the
      owning cntlr's own chain meets the same device at L4. A wrapper whose
      `dmsetup remove` will not go is therefore re-driven by both `SyncupCn`
      and that cntlr's `SyncupCntlr` — one round of work more than the
      minimum, and never a round less, which is the safe direction for a
      verdict whose only job is to make somebody try again. The second is an
      **unowned `:4:` clone-source connection**: it belongs to no sp at all,
      so both scopes enumerate it node-wide (CN21), the cntlr-level L5 set
      being the same "no stored cntlr names it as a clone's `src_nqn` and no
      live kind-`c7` table maps it" set the node-level pass uses. The same
      NQN is therefore reported by `CheckCn` and by every `CheckCntlr` of
      this node until it goes — the same one-round-extra, never-a-round-less
      direction. A cntlr whose stored conf the §7 gate
      refuses has no verdict either — it converged nothing and enumerated
      nothing, and naming objects this agent is not allowed to remove would
      be a leftover report no `Syncup*` could ever clear.

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
* `cnagent.md` CN12/CN28/CN29 (2026-09-26, the failover ping-pong) — the md
  arrays of `ensureGroup` and `probeGroup` are read from **sysfs** too, as the
  sweep's always were: one walk of the `/sys/block` array nodes per pass
  (`Md.Walk`; an array node is `md[0-9]+`, or `md_<name>` under mdadm.conf
  `CREATE names=yes` — the walk and the sweep's `ListArrays` both took
  `md[0-9]+` alone at first, which would have read a named node's running
  array as absent on the md rows), from which `Md.Detail` takes the array
  holding a wrapper of the group's `leg_list` (by
  `md/dev-*/block/dm/name`) and reads
  `md/array_state`, then for a running array `md/degraded`,
  `md/sync_action` and `md/sync_completed`, and per member
  `md/dev-*/state` and `md/dev-*/block/dev`. It replaced `mdadm --detail`,
  which loads the superblock from the first member that opens and, when that
  member's side had gone, blocked until the path's failfast expired (~13 s
  after the side died), was killed at the soft timeout and turned the group
  row `ERROR` — a row that counts toward cntlr health. Measured on the lab
  kernel (7.0, mdadm 4.5) before coding: an `inactive` array has no
  `md/degraded`, `md/sync_action` or `md/sync_completed` at all and its
  members read a bare `spare`; `dev-*/state` is a flag list and, while the
  array runs, dnv's members carry `failfast` (`in_sync,failfast`,
  `faulty,failfast`, `faulty,blocked,failfast`, `spare,failfast`);
  `md/sync_completed` reads
  `none` exactly when no sync runs, while `md/sync_action` can read
  `recover` for seconds with nothing to rebuild onto; `md/degraded` counts a
  failed member still held, a member being rebuilt and an empty slot alike;
  `array_state` read `clean`, `active`, `write-pending`, `readonly` and
  `inactive` there, never `clear` (a stopped array's `/sys/block/mdN` goes at
  once). Finding the array by member instead of by name adds the case-1
  guard of CN12 against an array that runs under the group's name holding
  none of its legs.
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
  single loop device: dm kind `cb` (`CnCloneMetaDmName` →
  `dnv-{cluster}-{cn}-cb-{sp}-{clone}`), `CnCloneMetaUnit`-sized contiguous
  units out of a `CnCloneMetaAreaSize` arena, a plain `blkdiscard` hole punch
  as the recycled-unit guard (never `--zeroout` — it would materialize the
  arena in RAM), and the kernel's own kind-`cb` dm tables as the allocation
  registry. Rationale is the [D13](a) failure class: bare `vgs`/`lvs`
  label-scan every block device on the node, and on a CN that, at the time,
  held transfer-origin ns-devs dm-suspended, which wedged LVM in unkillable
  D state (that instance is gone with the 2026-09-16 park; the label-scan
  class is not).
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
  note only, no mechanism change. (That residual note is superseded by the
  2026-09-16 park bullet below, which resolved it.)
* `cnagent.md` CN14 + CN16 (second-pass U1/U5) — CN14 gains the
  cross-slice point-in-time rule for `create_snap`: quiesce the origin td's
  raid0 around the whole per-slice message sequence (implemented; §6 test
  item 19 carries the assertions);
  CN16's [D12] residual paragraph now states the transfer-origin
  suspension's operational blast radius and the considered-but-undecided
  bounded alternative (reload onto `CnErrorName` after a grace window). (The
  CN16 half is superseded by the 2026-09-16 park bullet below: that
  paragraph is gone from CN16, the reload onto `CnErrorName` became the rule
  and no grace window exists.)
* `architecture.md` §2 / §3.3 / §8.7 / §9.5 / §10.3 / Appendix C / Appendix D
  (`ThinDeviceCreated.md` U1-U4) — the `created` flag and its gateway and
  worker rules: `CreateThinDevice` refuses a snapshot of an uncreated origin,
  `DeleteThinDevice` refuses an origin with an uncreated snapshot,
  `ListThinDevices` is the client's wait primitive, and the sp-worker flips
  the flag from the thin rows of any cntlr reply. This document's CN14 is the
  agent-side spec; §8.7/§10.3 are the gateway/worker spec, implemented in
  `gateway/` (`gateway/thindevice.go`) and `worker/` (`worker/sprole.go`'s
  created flip, `dnv-worker.md` RW19).
* `architecture.md` §2 / §8.8 / §8.10 / §11.3 / §11.5 / §11.6 / [D12] / [D14]
  + this document's CN16, CN21, the `ns_id_to_dm_linear` probe row and §6
  items 8 / 26 / 28 / 29 (decided 2026-09-16; recorded in [D12] and
  `architecture.md` §11.6) — an effectively
  suspended namespace is **parked**, not dm-suspended: its ns-dev is a live
  dm-linear over the td's `CnErrorName` (the new CN16 rule 1, which renumbers
  the rest of the backing state machine) and the namespace is `inaccessible`
  as before. [D12]'s unbounded-suspension residual is resolved rather than
  restated — the grace window it floated is unnecessary, because ANA
  `inaccessible` is written before the device is touched and nvmet refuses IO
  to an inaccessible namespace at the target. The CN holds no suspension
  across a converge pass any more, short of a `dmsetup` command that fails
  (a reload fails closed, CN16); `keepSuspended` is gone from
  `ensureDmSingle`, and the resumes left in `ensureNsDev`, `parkNsDev`,
  `removeDm` and CN21 are guards for a device an older build or an
  interrupted or failed reload left suspended.
* `dnagent.md` §2.7 SH14 + `architecture.md` §9.5 / §10.3 + `dnv-worker.md`
  HL2 / AR8 / §14.9 (2026-09-26) — `ResStatus` gains
  `RES_STATUS_PENDING = 5` for the CN11 row of a primary's leg whose
  prober has not completed a round, which used to read `RES_STATUS_OK`: an
  `OK` clears `Leg.err_epoch`, so every promotion cleared a dead leg's and
  an unprobed spare read ready to the AR8 leg repair. `PENDING` neither sets
  nor clears it, and SH14's status list grew by it — the cn agent emits it
  on a primary's leg rows alone.
* `dnagent.md` §2.8 SH15 + §7 item 12, `osclient.md` §4.2, `architecture.md`
  §11.1.1 case 1.3 + Appendix A (2026-09-26, the failover ping-pong) —
  `Md.Detail` no longer runs `mdadm --detail` through `runProbe`: it is a
  lookup in an `Md.Walk` of `/sys/block`, whose listings go through
  `listDir` and whose attributes through `readAttrStrict`, under the same
  "did not answer" rule (CN12) — except that another array that does not
  answer is recorded as unanswered rather than failing the walk, and can
  only turn a lookup that found nothing into an error. `Md.NameInUse`, the
  `lsblk` of CN12's case-1 guard, joins SH15's primitives (five became six)
  and item 12's, and takes `Md.Detail`'s place in `osclient.md`'s list of
  `runProbe` users; `Md.Walk` / `Md.Detail` join that section's reads that
  never answer "nothing there", with the unanswered-array exception stated
  there too. Case 1.3 reads the assembled array's members from sysfs, and
  the crib sheet's probing list names `mdadm --examine` and
  `/sys/block/md*` in place of `mdadm --detail`.
* `architecture.md` §11.1 **new_primary** steps 1 and 4 and the
  "Host-visible errors" paragraph (2026-09-26, the failover ping-pong) — a
  group whose member is not yet available is retried by the agent until it
  is (CN10/CN12), and the worker is not involved: the promotion's first
  converge can run before the sides' ANA flips have reached the new
  primary's sysfs ([D16]'s fan-out, unordered when this was written; rarer
  since `dnv-worker.md` RW14's sides-first hold, which is bounded), and the
  worker re-syncs on a revision or a reply code, never on a row. Step 4
  does not wait for steps 1-3: when the late members kept the stack from
  being built, that first converge moves the namespaces to `optimized`
  over the td's `CnErrorName` all the same (CN16's ANA rule), so a dead-CN
  failover whose promotion outruns the sides' flip is not clean from the
  host's side until the retry has built the stack (§7, known limits).
* `dnagent.md` §2.8 SH20 + §2.3 above (2026-09-28) — a controller whose
  `address` did not answer is never selected as a dead side's: it is
  unknown, never unwanted (CN10); nor is one whose `address` is absent,
  a controller already deleted that its subsystem still lists. The cn
  agent's own walk (`agent/cnagent/leg.go` `readCtrl`) had left both with
  an empty address, which matches no side: an unanswered one could, on
  the re-read after a connect of the pass, have a live path retired by
  device, and a gone one drew a disconnect of a device no longer there.
  An unanswered address now fails the leg's converge for the pass
  instead, naming the read, which registers the CN10 retry; a gone
  controller is passed over.
* `dnagent.md` §2.8 SH15 + §7 item 12, `osclient.md` §4.2 (2026-09-29) —
  `CloneMeta.Mounted` and `CloneMeta.FileSize`, the `findmnt` and `stat` of
  CN5's clone-metadata arena, probe through `runProbe`: a run that did not
  answer is an error, never "absent", and the converge asks once more in
  the same pass; only when that asking goes unanswered too is the row an
  `ERROR`, with nothing mounted or truncated. SH15's list ("six" became
  "these"; its lead now names `Mounted`'s rebuild and `dirMtime`'s clean
  verdict beside removals and data loss) and item 12 now also name
  `osBase.dirMtime` and `CloneMeta.Mounted` / `FileSize`; SH15 adds
  `CloneMeta.LoopDevices`, which needs no split, and names
  `readAttrStrict`'s other readers: `Nvmet.NsDevicePath`, the `enable`
  reads of `RemoveNamespace` / `RemoveSubsystem`, and `Md.ListArrays` /
  `Md.Gone`; item 12 names `NsDevicePath` beside `NvmeHost.readTrimmed` as
  a `readAttrStrict` reader, and records the cn leg walk's `readSubsys` as
  still reading a failed listing as "no subsystem", which is open.
  `osclient.md` §4.2 adds `dirMtime` and `Mounted` / `FileSize` to the
  `runProbe` users, `LoopDevices` beside `Dm.List`, and `NsDevicePath` to
  the strict readers.

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
every CN24 assertion but the per-round trace id — the reply codes, the rule for when the info rides
along, the CN1 locks — lives in the round, while the `Recv`/`Send` loop
around it is the SH24-SH26 shape with nothing cn-specific in it. The one
test of that loop, `TestCheckRoundsCarryTheRequestTraceId`, does supply
in-process fake server streams: the per-round trace id is derived there,
between `Recv` and the round (SH24).

1. **Fresh SyncupCn**: scripted empty probes; assert the `WriteProto` to
   `LocalCnPath` **first** (CN7's persist-first order, `architecture.md`
   §9.8 — the pointer list the sweep removes against is on disk before
   anything is converged or swept), then the CN5 sequence
   (`findmnt`/`mkdir`/`mount`, `truncate --size {CnCloneMetaAreaSize}`,
   `losetup --associated` then `losetup --find --show`, then the port attrs,
   `mkdir ana_groups/2`+`3`, the one-time `ana_state` writes — the test pins
   groups 1 and 3 of the three). No `io.max` write and no cgroup path
   appears anywhere in the recorded calls (CN6), and **no LVM command
   appears at all** — the
   arena needs none (CN18).
1b. **A base-state probe that did not answer is not "absent"** (CN5/CN28,
   `dnagent.md` SH15). The fake models a mount over a mounted path the way
   the kernel stacks it: the arena file under it drops out of sight and
   `losetup --associated` stops listing the loop it backs.
   `TestKilledFindmntDoesNotRemountTheArena` kills either of `Mounted`'s two
   `findmnt` calls on a converged CN, once and on every run: the `SyncupCn`
   runs the killed call exactly twice, issues no `mount` and no `truncate`
   and keeps the one loop device, and `tmpfs_info` reads `OK` when the
   second asking answered and `ERROR` naming the killed `findmnt` when it
   did not, while the file and loop rows read `OK`; a check round with the
   same kill reads `ERROR`, not `MISSING`.
   `TestKilledStatDoesNotTruncateTheArenaFile` is the arena file's half: a
   killed `stat` runs exactly twice and issues no `truncate`,
   `tmp_file_info` reads `OK` or `ERROR` naming it in the same way, and a
   check round with the kill reads `ERROR`.
   `TestAskingAgainBuildsAnAbsentArena` is the other side: after a reboot, a
   startup reconcile whose first `findmnt` is killed mounts the tmpfs
   exactly once, and the first check round reads the tmpfs, file and loop
   rows `OK` with code 0; on a fresh CN, a first `SyncupCn` whose first
   `stat` of the arena file is killed truncates it exactly once and
   attaches the one loop device. `TestUnconfirmedTmpfsCreatesNoArena` pins
   the gate of the file and loop steps on a confirmed tmpfs: after a reboot
   that left the mountpoint a bare directory, with every run of the `FSTYPE`
   `findmnt` killed, neither the startup reconcile nor a `SyncupCn` issues
   a `mount`, a `truncate` or a `losetup --find`, and the `SyncupCn`'s file
   and loop rows read `ERROR` saying no tmpfs is confirmed; on a fresh CN
   whose `mount` is refused after its `mkdir -p`, the `SyncupCn` issues no
   `truncate` and no `losetup --find`, with the same rows. In both, the
   next `SyncupCn`, whose commands answer, mounts, truncates and attaches
   exactly once each. A failed `losetup --associated` is
   `TestFailedLosetupNeverAttachesASecondLoop`.
2. **Revision gate**: lower ⇒ `ReplyCodeStaleRevision` and zero mutating
   calls; equal ⇒ full idempotent pass; higher ⇒ apply + persist. Same for
   `SyncupCntlr`; `SyncupCntlr` for a pointer `SyncupCn` has not introduced
   ⇒ `ReplyCodeUnknownObject`.
3. **Probe-first idempotency** (SH16): equal-revision `SyncupCntlr` against
   probes reporting a fully converged primary issues no mutating command —
   no dm/md/nvme/configfs mutation, no `ana_grpid` write.
4. **Standby converge**: legs connected + wrappers built + per-td errors +
   ns-devs on error + subsystems with every ns `ana_grpid = 3`; **no**
   mdadm, pool, thin, raid0 or clone command appears (RedundNone fixture;
   on a raid1 one a standby's sweep would `mdadm --stop` the array its
   wanted set no longer holds — but never `mdadm --detail`, which no sweep
   runs at all, CN12).
5. **Primary converge order**: one fresh primary pass (a RedundNone
   fixture) asserts the CN9 build order — connects → wrapper creates →
   RedundNone group linears (`CnGrpName`) → stdin multi-target concat
   creates → pool create →
   `create_thin` messages (for `created = false` tds only) → thin creates →
   raid0/error → ns-dev → nvmet
   objects → `ana_grpid = 1` writes last. (The md-raid1 create order and
   flags are pinned by tests 6-7's RedundMdRaid1 fixtures.)
6. **Failover**: re-sync to `primary = false` asserts the CN9 pre-step and
   CN21 layer order — every ns `ana_grpid = 3` **before** the ns-dev reloads
   onto error, before pool/thin/raid0 removal and `mdadm --stop`, with **no**
   `nvme disconnect` of a leg. The `--stop` names the node **sysfs** gave
   (`/dev/mdN`, read from the fake before the flip, because a stopped array
   has no node left to name), never `/dev/md/{CnMdDevName}`, and **no
   `mdadm --detail` is recorded at all** — the assertion that pins CN12's
   "no sweep reads a member device". Re-sync back to primary rebuilds via
   `mdadm --assemble` (superblocks present — CN12 case 2), never
   `--create`. The transfer demotion of CN9's pre-step 3 holds on a cntlr
   that stays primary too (`TestPrimaryTransferLetsGoOfADepartedOrigin`),
   for both shapes of an origin that no longer resolves. When the origin
   namespace and td leave the request together, the converge replies code
   0, reloads the transfer device exactly once, onto an error table the
   size of its own live table, before the departed raid0's `dmsetup
   remove`, and the raid0 is gone. When only the td leaves, the transfer
   device is reloaded exactly once onto that error table all the same;
   the still-wanted ns-dev, which has no td to park onto, keeps the raid0
   open itself there, so that case asserts neither a clean reply nor the
   raid0's removal. In both, an equal-revision re-sync reloads the
   transfer device no more (CN17). On a pass whose sweep an unanswered
   listing stopped, which runs no pre-step 3, the build makes that one
   reload itself (`TestStoppedPassDemotesATransferWithADepartedOrigin`):
   each of the four listings killed once, for both shapes and for a
   cntlr demoted to standby in the same revision, the pass replies the
   Leftover code and reloads the transfer device exactly once onto the
   error table of its live size, and the next pass, its listings
   answering, reloads it no more.
7. **§11.1.1 / member reconciliation**: scripted `--examine` outcomes and
   the fake's sysfs view of the array drive: no superblocks ⇒
   create+assume-clean; one ⇒ assemble + add; both-with-one-left-out ⇒
   assemble + re-add, whichever leg is left out (the array is found through
   the one it holds, `TestGroupFoundByItsHigherLeg`); single available leg ⇒
   assemble, and a scripted mdadm refusal leaves the group
   `RES_STATUS_ERROR` and, with every leg available, registers no CN10
   retry (*amended 2026-09-26*, CN12: a group's error by itself is not a
   late member); a `SwitchSpareLeg`-shaped request (one leg of the
   two-leg group swapped with a spare) ⇒ `--fail` + `--remove` of the
   switched-out leg + `--add --failfast` of the spare, in that order and
   never `--zero-superblock`, whether md still holds the switched-out member
   `in_sync` or has already failed it (`faulty,failfast`, what an errored IO
   to a dead side leaves), and when that leg is unavailable too — AR8's
   switch, whose switched-out side is dead: its controller `connecting`
   with its last-known `optimized` ana_state kept (what a dead side's path
   reads), or its connect failing, as when the side's nvmet port is gone —
   because a parked member is an extra whatever its leg's availability
   (`TestSwitchSpareLeg`; the unavailable cases *added 2026-09-26*); a
   killed `--fail`, `--remove` or `--add` in that switch — killed before
   the tool touched anything, or after the kernel completed it — leaves
   the group `RES_STATUS_ERROR` with the kill's error, a killed `--fail` or
   `--remove` runs no `--add` in that pass, and the next converge completes
   the switch (`TestSwitchSpareLegKilledVerb`, *added 2026-09-26*); both legs
   swapped for fresh ones, the old ones parked or gone, while the array runs
   under the group's name ⇒ `RES_STATUS_ERROR` "an array runs under …
   holding none of the group's legs", no `--create` and no
   `--fail`/`--remove`/`--add` against it — and when the `lsblk` that asks
   whether an array runs under the name did not answer, `RES_STATUS_ERROR`
   with the kill's error as details and the same absence of
   `--create`/`--fail`/`--remove`/`--add`
   (`TestGroupNeverCreatesBesideARunningArray`; the primitive's own answer
   is `TestMdNameInUseKilledIsAnError`'s: a killed `lsblk` is an error,
   never "not in use"). Each group test here also asserts that no
   `mdadm --detail` is recorded (*amended 2026-09-26*, CN12).
8. **Namespace states** (CN16): `suspended = true` ⇒ `ana_grpid = 3` write
   then the ns-dev reload onto the td's dm-error, live; resume path reversed
   (reload onto the raid0, then `ana_grpid = 1`); a device found dm-suspended
   is resumed (older build); an `auto_suspend` transfer
   retires its origin the same way; an `auto_resume` clone overrides a
   `suspended = true` dst namespace to serving; a `td_id` repoint is
   exactly one ns-dev reload; `sp_level = SP_LEVEL_READONLY` reloads
   user-facing ns-devs onto the flakey `error_writes` table and nothing
   else.
8b. **cntlid slots** (CN16/CN17, `architecture.md` §11.8;
    `TestCntlidSlotsAreDisjoint`): a primary converged on each of the
    `CnCntlidSlotCnt` slots, one fresh node each, writes the same
    `attr_cntlid_min`/`attr_cntlid_max` into its host-facing subsystem and
    its transfer's; slot *s* reads back `10000 + s×5000` to
    `10000 + s×5000 + 4999`, and no two slots' ranges share a CNTLID.
    `TestCntlidRangeMovesBetweenSlots` walks one primary across slots on one
    node (up by one, up by more, down by one, down by more, then unchanged),
    against a fake that, like nvmet, refuses a min above the current max and
    a max below the current min. Both subsystems converge to the new slot
    every time; a move up writes `attr_cntlid_max` first and a move
    down `attr_cntlid_min` first, each bound exactly once, and an unchanged
    range writes neither.
9. **Clone build and §11.5 recovery** (CN18): fresh build asserts
   connect(s) → the arena enumeration (`dmsetup ls` + `dmsetup table` of the
   kind-`cb` wrappers) → `blkdiscard --offset … --length …` of exactly the
   allocated range on the loop device (**never** `--zeroout`) →
   `dmsetup create` of `CnCloneMetaDmName` with the computed linear table →
   clone table with `2 no_hydration no_discard_passdown` →
   knob messages → chunk `blkdiscard`s → `enable_hydration` → ns-dev
   reload; a rebuild with the metadata wrapper scripted absent additionally
   asserts the dst-bitmap reads (`reserve_metadata_snap` → `thin_dump` →
   `release_metadata_snap`) and their `blkdiscard`s **before**
   `enable_hydration`, with the ns-devs parked on error throughout; a
   rebuild over what an agent killed between CN18 steps 3 and 5 leaves —
   the matching wrapper, the dm-clone up with hydration still disabled,
   the ns-dev parked, a chunk file in the store — asserts the dst-bitmap
   `thin_dump`, its `blkdiscard` and the chunk's before
   `enable_hydration` and the ns-dev reload after it, the `thin_dump`, that
   `blkdiscard` and `enable_hydration` exactly once each, the dm-clone,
   meta and ns-dev rows OK, no CN10 retry left registered and the wrapper
   neither re-created nor re-discarded, with
   the stale dm-clone removed and created afresh first — and all of it
   again, but with nothing re-created, when the stale dm-clone refuses its
   `dmsetup remove` and is kept, and all of it once more, removal and
   create included, when the device under the clone's name carries an
   error table, whose status is not a dm-clone's
   (`TestCloneRecoveryResumesAfterAKillBetweenCreateAndBitmaps`); from the
   same state, a second fault that stops the pass before hydration is
   enabled — the arena listing not answering, the dm-clone's status read
   not answering once or every time, the recovery's `dmsetup create`
   killed after its ioctl ran, the dst-bitmap `thin_dump` failing while
   the stale dm-clone refuses both of its removals, the enable refused,
   and the enable refused at `SP_LEVEL_READONLY` —
   leaves a dm-clone up with hydration off and the ns-dev parked (a plain
   dm-linear over the dm-error, never rule 7's dm-flakey) and never
   reloaded, registers the CN10 retry, and a Check round then reads the
   ns-dev row as the pass did — OK, or `RES_STATUS_ERROR` naming the read
   when no status read answers — and mutates nothing; each case also pins
   how far its pass got (the arena and the status cases removed, created,
   read and enabled nothing, with both clone rows `RES_STATUS_ERROR`), and
   one retry attempt with the fault gone then recovers the clone: one
   `thin_dump` and one dst `blkdiscard` before the one `enable_hydration`,
   then the ns-dev reload onto the dm-clone (through dm-flakey at
   `SP_LEVEL_READONLY`), and no retry left
   (`TestCloneRecoveryNeverServesAnUnfinishedDmClone`); from the same
   state again, a pass that runs the recovery through the enable, but in
   which one of the two `dmsetup status` reads of the dm-clone after it
   does not answer (CN18's own read for the dm-clone row, or CN16's for
   the ns-dev), runs the `thin_dump`, the dst `blkdiscard` and
   `enable_hydration` once each and registers the CN10 retry, with the
   dm-clone row `RES_STATUS_ERROR` and the ns-dev moved onto the dm-clone
   in the first case, and the dm-clone row OK, the ns-dev still parked
   and its row `RES_STATUS_ERROR` in the second; one retry attempt then
   runs no `thin_dump`, `blkdiscard`, `enable_hydration`, dm-clone
   removal or create, reloads the parked ns-dev onto the dm-clone once
   (the moved one not at all), reads both rows OK and leaves no retry
   (`TestCloneRecoveryRetriesAReadAfterTheEnable`); a clone
   allocated after another was torn down reuses the freed units and
   re-discards them first, while a surviving matching wrapper is **never**
   re-discarded; an arena with no contiguous free run of the required size ⇒
   `clone_id_to_meta` and `clone_id_to_dm_clone` both `RES_STATUS_ERROR`
   (the filler wrapper that exhausts it belongs to **another sp**, and has
   to: the arena is per CN, but a wrapper of *this* sp that no clone of this
   sp's request names is exactly what CN21's L4 removes); removal asserts
   CN9's pre-step park and CN21's L3 → L4 → L5 — ns-dev reload → clone
   remove → **wrapper remove** → disconnect — in that order, the pass's
   `rm -f` of the clone's chunk file (CN18) after the wrapper removal, the
   source connection as the pass's one leftover (its disconnect runs off the
   pass, CN10), and leaves the ns-dev back on the raid0 where the build
   phase put it. The fresh build's reply also pins
   the CN20 shape: one `BitmapInfo` for the clone whose `chunk_id_list` is
   exactly the pushed pair and whose `bm_idx_list` is **empty** — a clone
   never fills the migration field.
9b. **Teardown by sweep** (CN7/CN21, `TestDeclarativeCntlrTeardown`): a
    `SyncupCn` whose `cntlr_pointer_list` has lost the cntlr records, **in
    this order**, the `rm -f` of the `cntlr-*` file first — the cntlr is
    forgotten before anything is removed (CN7's drop-then-sweep split,
    `architecture.md` §9.8) — then the ns-dev park, the subsystem `rmdir`,
    the ns-dev, the raid0, the thin volume, the pool and a
    leg wrapper; the two leg connections, and nothing else, named as the
    pass's leftovers (their disconnects run off the pass, CN10 and CN21
    L10); each leg disconnected exactly once; and the re-sync once the
    disconnects have returned replying 0. No `0 delete ` message is sent at all
    (CN14: an sp leaving the node deactivates, it never deletes thin ids), no
    dm device of the node survives, and the §3.2 base state does — the tmpfs
    is still mounted and its single loop device still attached, which is the
    assertion that keeps "the base state is never swept" honest.
9c. **An unanswered enumeration stops every removal of the sweep** (CN21,
    `agent/cnagent/cnsweep_test.go`): `TestUnansweredMdListingStopsTheDescent`
    — a raid1 cntlr (two legs in its data group) whose arrays and legs the
    pass no longer wants, the cntlr gone from `cntlr_pointer_list`
    (node-level) or its level raised to `SP_LEVEL_NO_SIDE` (cntlr-level),
    and the `/sys/block` listing killed once: at either scope the reply is
    a Leftover naming `enumeration failed: md arrays`, no `nvme
    disconnect`, `mdadm --stop` or `dmsetup remove` is issued, both arrays
    stay assembled and every leg keeps its wrapper and its connection; the
    re-drive at the same revision, the listing answering, no longer names
    the md enumeration and stops both arrays.
    `TestUnansweredSubsystemListingRemovesNothing` — `dmsetup ls`, the
    nvme host walk or the nvmet `ls` killed, with the cntlr forgotten: a
    Leftover naming that enumeration and not one teardown command
    (`dmsetup remove`/`reload`/`message`, `mdadm --stop`, `nvme
    disconnect`, `rmdir`, `rm -f`). `TestUnansweredEnumerationRemovesNothing`
    is the first, narrower pin of the `dmsetup ls` case: the Leftover code,
    no `dmsetup remove`, `mdadm --stop` or `nvme disconnect`, and every dm
    device, the subsystem and the connections still there. What a
    cntlr-level converge whose sweep was stopped still does, and what it
    holds back, is pinned for each of the four listings, killed once:
    `TestUnansweredListingStillMovesAnaBeforeThePark` — a raid1 primary
    with a transfer demoted to standby, and a steady primary whose
    namespace becomes suspended: a Leftover naming the enumeration, no
    `dmsetup remove`, `mdadm --stop` or `nvme disconnect`, and exactly one
    `ana_grpid` write for the namespace, to `3`, ahead of the build's
    reload of its ns-dev onto the td's dm-error — for the demotion the
    same for the transfer's namespace, ahead of the transfer device's
    reload onto its error table; the next check round reads the namespace
    `OK`, and for the suspend, which leaves nothing to remove, the verdict
    `OK` as well.
    `TestUnansweredListingStillSweepsCloneChunks` — a standby drops a
    clone whose chunk was pushed: the chunk file is gone after that pass
    and the reply lists no applied chunks, and the next check round's
    verdict is `OK`. The third thing CN21 says such a converge still does,
    the CN11 prober trim, is pinned for the four listings as well, by test
    15's `TestProbersTrimmedWhenTheDescentStops`.
    `TestUnansweredListingDoesNotParkAServingNamespace` — a namespace
    moves to a second td while its first leaves `td_list`: no reload onto
    the new td's dm-error, and exactly one reload, onto its raid0 — none
    at all with `dmsetup ls` killed, which leaves no snapshot to rule out
    a lingering dm-clone.
    `TestUnansweredListingKeepsTheNsDevOffALeavingClone` — a clone whose
    hydration is on leaves `clone_list` while its namespace serves through
    it (also with that ns-dev held dm-suspended by an older build), or
    while it is parked and the delete's latch resumes it, or while a new
    namespace joins the same td: after the stopped pass the dm-clone is
    still there, each ns-dev is live over the dm-clone or parked on the
    td's dm-error, and no `ana_grpid` is written `1`; each held ns-dev's
    row reads `ERROR` in the reply and in `GetCntlrInfo` alike, and the
    CN10 retry is registered; the next pass removes the dm-clone before
    any reload onto the raid0 — parking one still over it first — writes
    a parked namespace's `ana_grpid` `1` only after that reload, reads
    the row `OK` and stops the retry.
    `TestAHeldNsDevIsRedrivenWithoutARevision` — with no dm-clone at all,
    a standby promoted and a namespace resumed, each with `dmsetup ls`
    killed once: the ns-dev stays on the td's dm-error with `ana_grpid`
    `3`, its row `ERROR` in the reply and in `GetCntlrInfo`, and the CN10
    retry is registered; one retry attempt, at the same revision, makes
    exactly one reload onto the raid0 and one `ana_grpid` write of `1`,
    in that order, and stops the retry.
    `TestUnansweredListingLeavesADroppedNamespaceToTheSweep` — the only
    namespace leaves `ns_list`, its ns-dev left dm-suspended over the
    raid0 by an older build and its `ana_grpid` already `3`: no
    `ana_grpid` write, no `enable = 0` and no `rmdir` of it on the
    stopped pass, whose reply names it beside the failed enumeration (the
    enumeration alone when the nvmet listing is the one killed); the next
    pass reloads and resumes the ns-dev before the `enable = 0`, its one
    `rmdir` and the `dmsetup remove`, and writes its `ana_grpid` nothing
    but `3`.
    With the four listings answering,
    `TestALeftDmCloneKeepsTheNsDevOffItsRaid0` — the hold on a pass
    whose chain leaves the dm-clone, by three routes: the park of the
    ns-dev over it fails its load, its `dmsetup remove` is killed before
    it acts, and a sibling namespace dropped in the same revision whose
    ns-dev will not go stops the descent at L2. On that pass no reload
    onto the raid0 and no `ana_grpid` write of `1`: the ns-dev whose park
    failed serves on through the dm-clone, still `1`; a parked one stays
    on the td's dm-error and goes `3`; the row reads `ERROR` in the reply
    and in `GetCntlrInfo`, and the CN10 retry is registered. The pass
    after the fault removes the dm-clone — parking first the ns-dev still
    over it — before the one reload onto the raid0, writes a parked
    namespace's `ana_grpid` `1` only after that reload, and stops the
    retry.
    `TestUnansweredNamespaceListingRemovesNoNamespace` — nsid 2 leaves
    `ns_list` under a subsystem that stays while the sweep's own listing
    of that subsystem's namespaces is killed once: a Leftover naming
    `enumeration failed: nvmet namespaces of {nqn}`, no `ana_grpid`
    write, `enable = 0` or `rmdir` for nsid 2, and no `ana_grpid` write
    for nsid 1; the next pass, the listing answering, replies 0 with
    nsid 2 gone — exactly one `ana_grpid` write for it, to `3`, before
    its `enable = 0` and its one `rmdir` — and still no write for nsid 1.
    And `TestUnremovedChunkFileIsRedriven` — the `rm -f`
    of a dropped clone's chunk file killed before it acted, once with the
    listings answering and once with `dmsetup ls` killed as well: the pass
    and the next check round name `record:{path}`, and the re-drive issues
    exactly one `rm -f` of it, removes the file and reads `OK`, as does the
    check round after it.
10. **PushCloneBitmap** (CN22, `agent/cnagent/clone_test.go`):
    `TestPushCloneBitmapGates` — unknown `clone_id`, and the two index
    bounds asserted **separately** so neither can stand in for the other: an
    in-range `src_slice_idx` with `bm_idx = MaxCloneBmCnt` is rejected, an
    out-of-range `src_slice_idx` with `bm_idx = 0` is rejected, and the pair
    `(0, MaxCloneBmCnt−1)` — in range on both axes — is accepted, which is
    what keeps the two rejections bounds rather than blanket refusals.
    `TestPushCloneBitmapPersistBeforeApply` — the `WriteProto` to the
    two-`%02x` `LocalCloneBmPath` precedes the `blkdiscard`, a grown chunk
    overwrites the same file and re-applies, a byte-identical re-push writes
    and discards nothing. `TestPushCloneBitmapSurvivesRestart` — chunks
    `(0, 0)` and `(1, 1)`, the second a pair neither index alone could name,
    are persisted under distinct names and a fresh server over the same fake
    store reports both back in `chunk_id_list`, ascending, with nothing
    re-pushed. `TestPushCloneBitmapWithoutDmClone` — a chunk pushed while
    the clone is `SP_LEVEL_NO_CLONE` runs no `blkdiscard`, still reports
    applied, and is `blkdiscard`ed when the converge builds the dm-clone
    (CN18 step 4).
10b. **The clone chunk store and the §11.4 chunk math**
    (`agent/agent_test.go`, `agent/cnagent/bitmap_test.go`):
    `TestCloneChunkSetPairKeyed` — `agent.CloneChunkSet` Put/Get/Delete by
    `CloneChunkKey` and `Ids()` ascending by `(SliceIdx, BmIdx)`, with the
    same `bm_idx` on two slices held as two chunks that neither shadow nor
    delete each other, and an absent pair simply absent (a gap is legal);
    `TestChunkSetContiguousPrefix` is untouched, because `agent.ChunkSet`
    stays migration-only. `TestFoldRegions` keeps the absent-chunk row (a
    region overlapping a slice with no chunks is never discarded) and
    `TestRegionSkippableMatchesTheAddressMapping` still differentially
    checks the cycle-walking fold against a literal replay of the §11.4
    mapping. Four cases pin the chunk arithmetic itself:
    `TestSliceBitmapChunkBoundary` (bit `8C−1` is the last bit of chunk 0
    and bit `8C` the first of chunk 1, and adding chunk 1 moves nothing in
    chunk 0), `TestShortChunkTailReadsAsWritten` (a bit inside a present
    chunk's span but past its length is written, never skippable),
    `TestAbsentMiddleChunkKeepsLaterChunksInPlace` (chunks 0 and 2 present,
    1 missing — chunk 2's bits stay at `16C` bits in; this is the case a
    concatenation model fails, and it fails it in the **unsafe** direction),
    and `TestChunksOfCutsAtChunkBoundaries` +
    `TestDstBitmapLongerThanAChunkIsSplit` (the §11.5 recovery builds its
    per-slice bitmaps locally and must cut them at `C`: wrapping a whole
    bitmap as chunk 0 would silently stop skipping past the first MiB of
    bitmap, and the second test checks that through `regionSkippable`, the
    entry point `foldRegions` actually uses).
10c. **A push carries no revision** (CN22, `architecture.md` §9.6,
    `dnv-worker.md` BM3; `TestPushHasNoRevisionGate`): the cntlr's stored
    revision is advanced past the one the push was planned from, and the
    chunk is still persisted
    **and** applied (`WriteProto` then `blkdiscard`) and appears in the next
    reply's `chunk_id_list`. The field's absence is asserted on the
    **descriptor** — `PushCloneBitmapRequest` has no `revision` field — so no
    later edit can reintroduce the gate without changing the proto. The one
    refusal that survives is the object one: an unknown `clone_id` is
    `ReplyCodeUnknownObject` with a non-empty `details`, which is the message
    the worker logs (BM6).
11. **sp_level ladder** (CN19): each level asserts exactly its row —
    `NO_THINPOOL` keeps namespaces exported on error backing;
    `NO_MIGRATION` is a no-op relative to `NO_REDUND`; `NO_SIDE` drops leg
    connections; `DISABLE` leaves only base state and keeps the store
    files; lowering rebuilds. `TestDisableCreatesNoParkTarget`: over an
    ns-dev whose td dm-error an earlier build could not create, the
    `DISABLE` converge names only the legs' connections as leftovers
    (their disconnects run off the pass, CN10) and leaves no dm device,
    and the re-sync once they have returned replies code 0; neither
    attempts a create of that `c5`, and the ns-dev is reloaded exactly
    once, onto an error table, before its removal (P0's park by live
    table; CN9 pre-step 2). `TestSuppressedCloneReportsSpLevel` (*added
    2026-09-26*, the failover ping-pong): a clone in `clone_list` reads its
    three rows `MISSING` `"sp_level"` at `NO_CLONE` and at `DISABLE`, on
    the converge and on the probe alike — rows the worker's settle
    (`dnv-worker.md` HL2) reads, where any other `MISSING` holds it.
12. **Check streams** (CN24): first reply full info; unchanged
    `show_info = false` round omits it; `show_info = true` re-includes it;
    unknown object ⇒ code 2 with the stream kept open; a round never
    mutates. `TestCheckRoundsCarryTheRequestTraceId` (SH24): on a
    `CheckCn` and a `CheckCntlr` stream whose ctx carries the stream's
    id, two rounds run their commands and reads under their own requests'
    `trace_id`s and none under the stream's; a round with an empty
    `trace_id` runs them under the stream's.
13. **Lock smoke** (CN1): a `SyncupCntlr` blocked in a slow scripted
    command blocks a same-cntlr `SyncupCntlr` but not a `CheckCn` round or
    another cntlr's converge. `TestAStuckDisconnectDoesNotHoldTheCheckRound`
    (CN10's disconnect registry): with every `nvme disconnect` parked by the
    fake's `blockCmd` — a child that ignores its ctx the way one in an
    uninterruptible kernel wait ignores SIGTERM and SIGKILL — the pass that
    drops a standby's two legs (`SP_LEVEL_NO_SIDE`) returns with both
    connections named as leftovers, a `CheckCntlr` round of the same cntlr
    gets through, a re-sync issues no second disconnect of either subsystem
    (counted), and once the kernel lets go the next Check round and the
    next re-sync reply 0. `TestAFailedDisconnectIsIssuedAgain`: a background
    disconnect that fails leaves the registry with its goroutine, the
    connection stays a leftover because the node still holds its
    controller, and the next pass issues the disconnect again — exactly
    once more. `TestAStuckDisconnectDoesNotHoldTheNodeLock`: the same
    parked disconnects under a `SyncupCn` that drops the cntlr's pointer
    (the pool drain's shape, under the node write lock) — the pass replies
    naming both connections, a `CheckCn` round gets through, and the
    re-sync once the kernel lets go replies 0 with each leg disconnected
    exactly once. `TestADisconnectOutlivesTheRpcCtx`: a disconnect held on
    the fake's ctx-honouring gate still completes after the RPC's ctx is
    cancelled, as gRPC cancels it when the handler returns — the goroutine
    runs on `rootCtx`. `TestARecreatedCloneNeverAdoptsADyingSource` and
    `TestARestoredLegNeverAdoptsADyingConnection`: a clone re-created on a
    source whose disconnect is parked, and a level lowered back below
    `SP_LEVEL_NO_SIDE` while the legs' disconnects are parked, build
    nothing and connect nothing while the disconnect runs — the leg rows
    are `ERROR` naming it, in the converge's reply and a Check round's
    alike — and once it has returned the connect retry builds each over a
    fresh connection, connected exactly once.
    `TestTheCheckRoundMirrorsTheDisconnectGate`: with that source's
    disconnect parked and the arena then filled, a Check round reports the
    dm-clone `MISSING` as the converge did, not the step 2 refusal pair,
    and the clone's target row with the converge's status and details.
    `TestABackgroundDisconnectCarriesThePassTraceId`: each leg's background
    disconnect runs under the trace id of the pass that set it going.
    `TestTheBackgroundDisconnectsRunAFewAtATime`: the cap is a quarter of
    `DefaultOsClientLimit`; shrunk to one with both leg disconnects parked,
    one is issued and the other waits, registered — the re-sync issues no
    disconnect — and once they are released each subsystem is disconnected
    exactly once and the next re-sync replies 0.
14. **Thin bitmaps** (CN25-CN27): scripted `thin_dump` XML drives: reserve
    → dump → release ordering, release also on a scripted dump failure and
    after an "already reserved" retry; the wire inversion (mapped ⇒ 0);
    paging windows; the meta-group all-zero rule; the data-group span
    arithmetic on the fixture's single data group (`TestLegBitmap`).
    `TestThinDumpGoesThroughAFile`: one bitmap read runs `thin_dump` once,
    with `-o {file}` in the private `/run/dnv-thin-dump` directory, and its
    answer carries no stdout — the fake records each command's stdout, the
    attribute the `os command` record logs in full — then reads the file
    exactly once and `rm -f`s it before the release, leaving no file; a
    `thin_dump` the soft timeout killed fails the RPC, reads nothing and
    still leaves no file; and one whose caller's ctx is cancelled while it
    runs, after writing the file, fails the RPC, reads nothing, and is
    followed by exactly one `rm -f` of the file, which is gone, and then
    by exactly one `release_metadata_snap` (CN25).
    `TestThinDumpDirIsPrivate`: a read runs `mkdir -p -m 0700` and `stat`
    of `/run/dnv-thin-dump` before its `reserve_metadata_snap`; with the
    `stat` answering another user's directory, a mode-`777` one, or a
    symlink, the RPC fails with no reserve and no `thin_dump` (CN25).
15. **Leg prober** (CN11): registry logic under a fake clock with a **fake
    `LegProbeIO`** (§2.2 — the prober never touches the server's `oc`: a
    round records no `writeblock`/`readblockdirect` `OsClient` call at all),
    `RES_STATUS_PENDING` `"health probe pending"` for a fresh prober, for
    its first round in flight inside the stall bound and for a leg with no
    registered prober (*amended 2026-09-26*, was `RES_STATUS_OK`), the last
    completed outcome, never `PENDING`, for a round in flight after the
    first completion, the converge replies of a just-built primary carrying
    `PENDING` leg rows, stall reporting after `CnLegProbeStallSeconds`,
    `Write` offset =
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
    round can only delay the next one. The teardown ordering of CN21's L10
    has no completion order left to assert: the leg's `nvme disconnect` is
    set going before the `dmsetup remove` of its wrapper and runs off the
    pass (test 13), so a wedged prober's wrapper normally goes on a later
    pass.
    `TestProbersTrimmedWhenTheDescentStops`: a raid1 primary demoted to
    standby, raised to `SP_LEVEL_NO_SIDE`, or at `SP_LEVEL_NO_THINPOOL`
    losing a grown data group with its one leg, on a pass that stops short
    of L10 — every unwanted array's `mdadm --stop` killed before it acted,
    or one of the four listings killed once: the reply is a Leftover naming
    the array or the enumeration, and no `nvme disconnect` and no
    leg-wrapper `dmsetup remove` is issued, yet each prober of a leg the
    plan no longer probes is cancelled exactly once in that pass, before
    the sweep's first command (its `dmsetup ls`), and deregistered, while
    each wanted one is never cancelled and is the same prober afterwards
    (CN21).
16. **cmd**: the `architecture.md` §13 example `dnv-agent cn …` invocation parses; `--disk`
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
    of the sequence leaves a device suspended ([D12]) short of a `dmsetup`
    command on it that fails (CN16). An equal-revision
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
    * `TestUnverifiedPoolRemovalKeepsArming` — the stop rule applied to a
      STATE mutation rather than to a device. The sweep's L8 drops a pool's
      arming on the reasoning that the pool's life ended, but in the branch
      where the removal was **not verified** the pool is reported as a
      leftover in the same breath, and dropping the arming there loses the
      thin-id sweep for good: once the device does go and the level comes
      back up, `ensurePool` probe-matches the surviving device and arms
      nothing, so every id deleted meanwhile keeps its data blocks for the
      life of the pool. A killed-with-no-effect `dmsetup remove` of the pool
      at `SP_LEVEL_DISABLE` ⇒ `ReplyCodeLeftover`, the pool still present,
      and the slice still armed. Keeping an arming too long costs one
      idempotent sweep; dropping one too early cannot be repaired.
26. **A removed namespace is parked before its nvmet objects** (CN9/CN21,
    `TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval`): a primary
    serving one `suspended = true` namespace — which this build leaves
    *parked*, live on the td's `CnErrorName` — then a converge that drops it
    from `ns_list`. Two sub-cases put the ns-dev back into the state a
    pre-2026-09-16 agent left it in (dm-suspended, still on the raid0) — one
    of the four ways a sweep can still meet a suspended device, beside an
    interrupted `Reload`, an agent killed inside CN14's quiesce bracket and a
    `dmsetup` command on it that failed (CN16's fail-closed reload): its
    reload onto the td's `CnErrorName` and the
    resume inside it are recorded **before** that nsid's `enable = 0` and
    `rmdir`, which are recorded before the ns-dev's own `dmsetup remove`. The
    target is pinned as well as the order — the recorded `--table` must name
    the td's dm-error — because a reload onto the wrong backing satisfies the
    order and still leaves the ns-dev serving data; CN21's P0 derives that
    target from the ns-dev's **own live table**, the raid0 it still maps
    naming the td whose error device it is parked on. And exactly **one**
    park of that ns-dev, since an ordering assertion stops at its first match
    and cannot see a second one. The count is over the park's own `dmsetup
    table` probe, which it makes before it decides anything — counting
    *reloads* would prove nothing, because a park is idempotent (a device
    already linear over the `CnErrorName` is resumed if suspended and
    returned before the reload) and so a repeat park issues no reload at
    all. The second of those sub-cases drops the whole subsystem
    and asserts the same park-first order around `RemoveSubsystem`. A third
    is the steady state: an already parked namespace is removed with **no**
    reload, suspend or resume of its ns-dev at all.
    `TestNamespaceRemovalIsTheChainsAlone`: the same drop from `ns_list`
    under a surviving subsystem, in a converge whose `dmsetup ls` does not
    answer, replies `ReplyCodeLeftover` and leaves the nvmet namespace in
    place with no `rmdir` of it — the build phase removes none — and the
    next converge, whose listing answers, removes it through L1: its
    `ana_grpid = 3` write before its one `rmdir` (CN21).
27. **A zero conf member is refused** (CN8, `dnagent.md` §2.1): a
    `SyncupCntlr` whose `bdev_conf` carries a zero `data_block_size`,
    `low_water_mark_pct` or `stripe_size`, or a zero
    `bitmap_chunk_block_cnt` under an md-raid1 `redund_conf`, replies
    `ReplyCodeInvalidConf` with the **stored** revision, records **zero**
    mutating calls (no `dmsetup`, `mdadm`, `nvme` or configfs write) and no
    `WriteProto` to `LocalCntlrPath`, and leaves the stored request of the
    previous revision as this cntlr's desired state. The configfs half of
    that claim is about **writes** only — no `mkdir`, `rmdir`, `ln -s`,
    `rm -f` or attribute write under `subsystems/` — because the node-level
    sweep does *read* the nvmet tree on every `SyncupCn`, and must, to find
    what an sp whose pointer has left the list still holds. A conf whose
    `redund_conf` selected
    `redund_none` is **accepted** with the other three members concrete,
    because the chunk count is a field of the md-raid1 arm and does not
    exist at all on the other. A `Reconcile` over a persisted request
    carrying such a zero converges nothing for that cntlr, starts no probers
    and records the same refusal; so does the connect-retry re-entry, which
    is the sharper case, because that cntlr has already been converged once
    and a sweep of it would have plenty to enumerate. The four messages are
    asserted verbatim; they are the same literals `model/capacity_test.go`
    asserts for `model.ValidateBdevConf`, and the two assertions together
    are what keep the two copies of the rule in step (`dnagent.md` §2.1).
28. **A parked ns-dev probes `OK, parked`** (CN16/CN28,
    `TestParkedNamespaceProbe`): an effectively suspended namespace's ns-dev
    reports `RES_STATUS_OK` with `details = "parked"` — from the read-only
    probe **and** from the converge reply, which are two different code paths
    and each need their own pin. The same device dm-suspended is
    `RES_STATUS_ERROR "unexpectedly suspended"` whether or not the plan says
    suspended: this build leaves an ns-dev suspended only when a `dmsetup`
    command on it failed (CN16's fail-closed reload) or the agent died
    inside a reload, so finding one is a fault and not a steady state.
29. **A suspended ns-dev from an older build converges on the first pass**
    (CN16, §11.6, `TestSuspendedNsDevFromAnOlderBuildIsResumed`): the three
    shapes an upgrade can meet. Effectively suspended and still holding the
    raid0 ⇒ exactly **one** reload, onto the td's `CnErrorName`, ending live;
    effectively suspended and already on the `CnErrorName` but held suspended
    (an interrupted `Reload`) ⇒ one reload, ending live; **serving**, with the
    table it wants, suspended ⇒ a bare `dmsetup resume` and **no** reload —
    which is the one remaining reason `ensureNsDev` reads `dmsetup info`'s
    suspend bit at all.
30. **The md array is read from sysfs** (CN12/CN28, *added 2026-09-26*;
    `agent/cnagent/mdprobe_test.go` and `cnagent_test.go`). The fake
    publishes what the lab kernel does — `md/degraded`, `md/sync_action` and
    `md/sync_completed` for a running array only, `dev-*/state` as a flag
    list (`in_sync,failfast` by default, a bare `spare` in an array that has
    not started) and `dev-*/block/dev`. `TestMdDetailFromSysfsOnly`: `Detail`
    finds the array by either leg's dm name and by no other, reads state,
    degraded count, sync action and progress and each member's devno, dm
    name and state, and runs no mdadm; a member with no dm name makes it
    foreign; two arrays holding legs of the group are an error; an
    unanswered listing or read — of `/sys/block`, of every array's `md/`
    or dm names at once (so no array answers and matches), or of the
    matched array's `array_state`, each of `degraded`, `sync_action` and
    `sync_completed`, a member's `state` or a member's `block/dev` — is an
    error, never absent, and so is a member's `block/dev` that vanished
    between the walk and the read (another array that did not answer
    beside a match is `TestMdWalkUnansweredArray`'s case, below).
    `TestMdListArraysFromSysfsOnly`: the sweep's enumerator runs no mdadm,
    reads an array with a non-dm member as foreign, drops a node listed
    with no `md/`, and fails — never drops the array — when the
    `/sys/block` listing, one array's `md/` listing, one member's dm name
    or one array's `array_state` does not answer (CN21's strict rule);
    for the `md/` listing and the dm name, `Md.Walk` over the same node
    records the array as unanswered instead.
    `TestMdNamedKernelNode`: an array on the kernel node `md_<name>`
    (mdadm.conf `CREATE names=yes`) beside a numbered one is found by
    `Detail`, listed by `ListArrays` and not `Gone`.
    `TestSweepStopsANamedArrayNode`: the sweep stops such an array by
    `mdadm --stop /dev/md_<name>`, never the `/dev/md/<name>` symlink, and
    verifies the stop from `/sys/block/md_<name>` — for the hex
    `md_<CnMdDevName>` and for the non-hex `md_<CnMdArrayName>` an
    `--assemble --scan` or incremental assembly names from the superblock.
    `TestMdBlockEntryPattern`: `md0`, `md127`, `md_d0` and `md_<name>` are
    array nodes, a non-hex name included (`md_dnv-0000000000000002-00-00`,
    and `md_dnv-0000000000000002-00-00_0` after a name conflict); `md`,
    `mdp`, `md0p1`, `dm-0`, `nvme0n1`, `nvme0c0n1`, `sda`, `loop0` and
    `xmd0` are not.
    `TestMdDetailInactiveArray`: an `inactive` array reads present with
    that state and no running-only attribute read. `TestMdStateLine`: the
    details composition, including `sync_action` `recover` with
    `sync_completed` `none` (no word) and a finished re-add (`recovering
    (2093056 / 2093056)`).
    `TestGroupProbeIsSysfsOnly`: a primary Check round over an array with
    one member `faulty,failfast` and `md/degraded` 1 reports `OK` `clean,
    degraded`, a rebuild `clean, degraded, recovering (32768 / 2093056)`,
    an `inactive` array `ERROR` `inactive` — each round with no mdadm
    command at all — and a converge over the inactive array reports its
    state and runs no mdadm.
    `TestGroupProbeArrayStates`: every `array_state` through a Check round,
    and every one but `clear` through a converge — `clean`, `active`,
    `active-idle`, `write-pending`, `readonly` and `read-auto` read `OK`
    `<state>, degraded` and the converge `--add`s the `leg_list` member the
    array lacks; `inactive`, `suspended`, `broken` and an unknown value read
    `ERROR` with the state, and the converge runs no mdadm; `clear` reads
    `MISSING` on the Check round (md reads `clear` only for an array with no
    member, which no group's `Detail` can match).
    `TestGroupForeignMemberIsAnError`: a Check round and a converge over an
    array holding the group's wrappers plus a non-dm member both report
    `ERROR` naming that member's devno, and neither runs mdadm.
    `TestGroupUnansweredSysfsReadIsAnError`: a killed read of every
    array's member dm names (the walk, which leaves no array answering),
    or of the matched array's `md/array_state`, `md/degraded` or a
    member's `dev-*/block/dev`, reads `ERROR`, never
    `MISSING`, on a Check round and on a converge, which runs no mdadm —
    read as absent, the first two would assemble beside the running array.
    `TestGroupMembersComparedByName`: with the `lsblk` of either leg
    wrapper killed, an equal-revision converge of a healthy two-leg array
    runs no `--fail`, `--remove` or `--add` and reads `OK` — keyed by
    device number, the unanswered `lsblk` left that wrapper out of the
    wanted set, and the converge failed and removed its in-sync member.
    `TestGroupUnavailableLegMemberStaysWanted`: with leg 2's path
    `inaccessible` or `non-optimized`, or its connect failing with the
    wrapper of the earlier pass still there, or with leg 1's path
    `non-optimized` beside an available leg 2, an equal-revision converge
    runs no `--fail`, `--remove` or `--add`, leaves the array's members as
    they were and reads `OK` — a held member of an unavailable `leg_list`
    leg stays wanted — and registers the CN10 retry, which the converge
    before it, with both legs available, did not (*amended 2026-09-26*; the
    leg 1 case because the late verdict reads the group's whole
    `leg_list`, not its last member).
    `TestGroupHeldFaultyMemberStaysHeld`: with leg 2's
    member `faulty,failfast` and its leg available, an equal-revision
    converge runs no `--fail`, `--remove` or `--add` and reads `OK` — every
    member sysfs lists is held, whatever its state (§7's "a member md
    failed stays failed") — and registers no retry: the member is not late
    (CN12). `TestGroupNeverAddsAnUnavailableLeg`: with leg 2
    missing from the array and its path `non-optimized`, the converge runs
    no `--add` and no `lsblk` of either leg wrapper, reads `OK` and
    registers the retry; with the path `optimized` again, the next
    converge adds it and stops the retry.
    `TestGroupProbeWalksOnce`: a Check round over four arrays lists
    `/sys/block` and each array's `md/` exactly twice — the verdict's
    enumeration and the one walk the md rows share — and lists or reads
    nothing of the `dm-N`, `nvme*`, `sda` and `loop0` entries beside them.
    `TestGroupConvergeWalksOnce`: an equal-revision converge over the same
    four arrays lists `/sys/block` and each `md/` exactly twice too — the
    sweep's enumeration and the one walk the groups share — and touches no
    entry that is not an array node either.
    `TestMdWalkRefresh`: a stale walk misses, and `Refresh` finds, an array
    assembled under an `mdN` another array held, under an `mdN` recorded
    with no member, and under a member directory that now carries another
    dm name; it picks up a new node, drops one that went (so the array now
    holding its leg is the only match) and re-lists no unchanged array.
    `TestMdWalkRefreshUnanswered`: an unanswered node that went is dropped
    (a name no array holds reads absent again); a node still listed whose
    `md/` has gone — unseen, recorded (its leg since assembled under a new
    node, which is then the one match) or unanswered — is dropped and not
    recorded as unanswered; and a recorded node whose
    check fails and whose re-walk does not answer — its member's dm name
    unreadable, or its dm minor reused by another wrapper under a killed
    `md/` listing — loses its record: the lookup of the old dm name is a
    did-not-answer error, never the stale array.
    `TestGroupAssemblyBesideAnArrayMidStop`: an assembly beside another
    sp's array whose member's `state` reads an error with its `block` link
    still there (md between clearing the member's array pointer and
    removing the link), or whose check does not answer at all, reports
    every md row `OK`; the second case re-walks that array.
    `TestMdWalkUnboundMember` / `TestGroupBesideAnUnboundMember`: the
    member md has fully unbound — `block/dev` and `block/dm/name` reading
    `ENOENT`, its `state` an error — is recorded with no dm name, never the
    array as unanswered: the lookup of its old name reads absent (a
    republish of the array keeps the member unbound), in that shape — not
    for the whole stop, CN12 — the sweep's `ListArrays` answers and reads
    the array foreign, a group not
    built yet is created beside it with every md row `OK` and a clean
    reply, and a built group's Check round and converge read `OK`, reply
    clean and run no mdadm.
    `TestMdWalkUnansweredArray` / `TestGroupBesideAnUnansweredArray`:
    another array whose member dm name does not answer (a read error other
    than `ENOENT`) or whose `md/` listing is killed leaves the walk
    standing; beside the group's own array a Check round and a converge
    read every md row `OK` and run no mdadm — both replying a Leftover
    naming the md enumeration, because the sweep's `ListArrays` fails on
    the same array (CN21) — and with the group's array not built yet the
    lookup is an error naming the unanswered array — no create, no
    assembly and no CN10 retry registered; the test's next `SyncupCntlr`,
    after the array answers, assembles it (with every leg available nothing
    in the agent re-drives it, CN12). A match whose
    `md/` went after the walk (a stop removes it whole) is that same error
    beside an unanswered array; once every array answers, `Refresh` drops
    it and its names read absent, and with no `Refresh` in between a walk
    every array answered reads such a match absent too.
31. **A late `leg_list` member registers the CN10 retry** (CN10/CN12,
    *added 2026-09-26*, the failover ping-pong;
    `agent/cnagent/cnagent_test.go`). The registration is
    `cntlrState.retrying`, read under `s.mu`; an attempt is run by hand
    with `reconvergeCntlr` on `rootCtx`, exactly the call the loop makes,
    and the tests set a `retryInterval` no test outlives, so every attempt
    is one the test ran. `TestLateMembersRegisterTheRetry`: a standby
    promoted while the paths of both groups' legs — or of the meta group's
    alone, which the pass converges before the data group — read
    `non-optimized` (the sides not flipped yet) reports each group left
    with no available leg `ERROR` `no available leg`, runs no `mdadm
    --create`, and no `--assemble` but the data group's where that group
    is whole, and registers the retry; an attempt while the paths still
    read `non-optimized` assembles nothing and keeps it; with the paths
    `optimized`, one attempt runs one `mdadm --assemble` per late group,
    the probed md, raid0 and ns-dev rows read `OK`, and the retry has
    stopped. `TestLateMembersProbeMissingWithoutTd` (*added 2026-09-26*):
    the same promotion, both groups late, in an SP with no td and no
    subsystem reads both groups `ERROR` `no available leg` and registers
    the retry, and a Check round at the driven revision before any attempt
    answers an accepted code and reads the groups, the pool concats and the
    pool `MISSING` `""`, the probe finding them absent, with no row outside
    `leg_id_to_leg` `ERROR` or `PROVISIONING`: the reply the worker's
    settle must not take for a built stack (`dnv-worker.md` HL2).
    `TestLateMemberRefusedStartIsAssembledByTheRetry`: the same
    promotion after a clean demote with only one of a two-leg group's
    members unavailable — mdadm refuses the start from the other alone
    ([D16]; the fake has no Array State gate, so the test injects the
    refusal), the row reads `ERROR` with mdadm's words, no `mdadm
    --create` runs and the retry is registered; an attempt before the flip
    is refused again, runs no `--create` either and keeps the retry; the
    attempt after the path reads `optimized` assembles the array from both
    members, runs no `--add` and stops the retry.
    `TestLateMemberIsAddedByTheRetry`: the same with the late member
    failed and removed before the demote, so the start from the other
    alone is one mdadm allows — the row reads `OK`, no `--add`
    runs and the retry is registered, an attempt before the flip adds
    nothing and keeps it, and the attempt after the flip adds the member
    and stops the retry. `TestLateRedundNoneLegRegistersTheRetry`: a
    RedundNone standby promoted while its legs' paths read `non-optimized`
    builds the group linear, reads the group `OK` and registers the retry
    (on a real node the pool create over a side that has not flipped
    fails; the fake does not model dm-error IO, so the test pins the
    registration); an attempt before the flip keeps it, and the attempt
    after the flip stops it. `TestLateMemberRetryScope`: an unavailable
    leg registers nothing on a standby (every path `non-optimized`), on a
    primary whose `sp_level` (`NO_REDUND`, `NO_SIDE`) suppresses its
    groups, in a deferred group (its provisioned member `non-optimized`
    beside the provisioning one), or as a spare (`non-optimized`).
    `TestLateMemberRetryScopeWantedGroups`: a late member does register the
    retry at an `sp_level` that wants the groups but no pool
    (`NO_THINPOOL`; the group reads `ERROR` `only 1 of 2 legs available`),
    and in a group past [D15]'s prefix cut (CN9) — built and `OK`, not yet
    a concat target — because only a deferred group is excluded, not every
    group outside the effective concat.
    `TestFailedConnectRegistersTheRetryWithoutALateMember`: a refused
    connect of a standby's leg, or of a primary's spare, registers the
    retry although neither leg is a late member — build ORs its
    late-member verdict into `ensureLegs`' and never overwrites it.
31b. **An unanswered `address` read keeps the controller** (CN10,
    *added 2026-09-28*; `agent/cnagent/cnagent_test.go`,
    `TestUnansweredAddressReadKeepsTheController`). Once one leg
    controller's `address` stops answering, every read of it fails for the
    rest of the pass — refused (`failReadAlways`) or cut off by the soft
    timeout (`killReadAlways`). An equal-revision converge of a standby,
    and of an md-raid1 primary whose member that leg is, runs no
    `nvme disconnect`, no `nvme connect` and no mdadm, reads the leg
    `RES_STATUS_ERROR` naming the read and registers the retry; the retry's
    next attempt, the read answering, connects and disconnects nothing,
    runs no mdadm, stops the retry and finds the controllers the pass
    left, the leg reading `OK` (standby) or `PENDING` (primary). Two more
    cases, a standby and a primary, lose the leg's controller first, so the
    pass connects the side once and the read that does not answer is the
    new controller's, on the re-read after that connect; a last one
    provisions a standby's migrating leg's dst side in the pass — its next
    revision — and the src controller's `address` stops answering while
    that connect runs (the fake's `duringConnect`). Each otherwise ends as
    above. The cn fake refuses a connect to an endpoint it already holds a
    controller for — in any state but deleting or dead — as nvme-cli and
    the kernel do (`EALREADY`), so a pass that connected beside an unknown
    controller would spend its budget on refused connects, as on a real
    node.
31c. **An absent `address` is a gone controller** (CN10, *added
    2026-09-28*; `agent/cnagent/cnagent_test.go`,
    `TestAbsentAddressIsAGoneController`). A two-sided leg's dst
    controller is deleted while its subsystem still lists it (the fake's
    `deleteCtrlDevice`: every read under `/sys/class/nvme/{ctrl}` answers
    ENOENT). An equal-revision converge of a standby, and of an md-raid1
    primary, connects that side once, runs no `nvme disconnect` and no
    mdadm, reads the leg `OK` (standby) or `PENDING` (primary) and
    registers no retry; the next pass, the link still listed, connects
    and disconnects nothing.
32. **The connect step's pass budget** (CN10/CN18, *added 2026-09-28*;
    `agent/cnagent/connstep_test.go`). Every test runs the pass on a fake
    clock through the server's `now`/`sleep` seams (`withPassClock`): a
    pause moves it by exactly its length, and a connect the fake node
    refuses (`connectFail`, N refusals per endpoint and then success) takes
    no time unless the test makes it slow (`connectTakes`); the fake also
    defers a subsystem's namespace head to the Nth listing of its directory
    (`nsHeadAfter`). The other tests' servers keep the real clock and a
    sleep that does not wait, so a connect that fails for good costs them
    its retries but no wall time. `TestConnectRetriedWithinThePass`: a
    standby leg refused twice is `OK` on the first reply after three
    connects and two pauses, with no retry registered; a primary's grown
    group whose one leg is refused once — the e2e react grow — gives a first
    reply with no `ERROR` row at all, the grown group, the data concat and
    the pool `OK`. `TestNsHeadAwaitedAfterConnect`: a head on the second or
    third read leaves a standby leg `OK` after one or two scan pauses, and
    the grow's first reply clean. `TestSlowFailedConnectIsNotRetried`: a
    connect that takes `CmdSoftTimeout` and fails is made once, the leg
    reads `ERROR` with the refusal and registers the retry, and the pass
    takes exactly the connect's time with no pause.
    `TestPassBudgetSharedByEveryLeg`: the meta leg, refused for good, is
    connected eleven times (one, then one after each of the ten pauses 1 s
    holds), so the data leg, refused once, is not retried and reads `ERROR`
    — and a background retry attempt, a pass of its own, gives the meta leg
    its eleven connects again.
    `TestNoHeadWaitWithoutAConnect`: a connected leg whose head is gone
    reads `ERROR` `no multipath namespace` with no connect, no pause and no
    more listings of its subsystem than the same pass with the head there.
    `TestCloneSourceConnectStep`: a clone source refused once, or whose
    namespace appears on the second read, builds the dm-clone on the first
    reply. `TestCloneSourceSharesThePassBudget`: with the meta leg having
    spent the budget, the source's refused connect is made once and the
    clone reports its step-1 failure: `clone_id_to_target` `ERROR` with the
    refusal, `clone_id_to_dm_clone` `MISSING` `source not connected`.

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
   `CnCloneMetaName`/`CnCloneMetaPath`), `LocalCloneBmPath` taking six
   arguments — `(cluster, cn, sp, clone, src_slice_idx, bm_idx)` — and
   formatting two `%02x` segments while `LocalMigrBmPath` keeps its five and
   its one, the exported `common.WriteBlockAt` /
   `common.ReadBlockDirectAt` helpers with **no** `ReadBlockDirect` on
   `OsClient` or `FakeOsClient`, `DisconnectDevice` on `NvmeHost`, and
   `ParseDmName`/`ParseNqn`/`IsDnvNqn` in `common/name_parse.go` — one of
   the seven `common` files `layout.md` §2 lists, and the one §2.1 adds.
4. A repo-wide grep finds no `WriteFile(` call whose path argument is under
   `/sys/kernel/config` (SH18), no `ana_state` write outside `EnsurePort`,
   and no `io.max`/cgroup write anywhere (CN6).
5. `grep -rn "zero-superblock" agent/cnagent/` finds nothing outside
   comments and test-guard string literals (CN12);
   `grep -rn "s.oc" agent/cnagent/healthcheck.go` finds
   nothing, and `grep -rn "common.WriteBlockAt\|common.ReadBlockDirectAt"
   agent/cnagent/` hits only the `LegProbeIO` implementation — the CN11 probe
   IO never passes through the `OsClient` (§2.2).
6. `grep -rnE "pvcreate|vgcreate|lvcreate|lvchange|lvremove|\blvs\b|\bvgs\b|\bpvs\b" agent/ cmd/ common/`
   finds nothing outside comments and test-guard string literals —
   **repo-wide**: LVM is gone from dnv entirely ([D13]/[D14]), superseding
   the old cn-only exemption (`dnagent.md` acceptance 6 is the same
   repo-wide grep). And `grep -rn "zeroout" agent/cnagent/` finds
   nothing: the clone-metadata arena uses a plain `blkdiscard` hole punch,
   while `--zeroout` belongs only to the dn side-provisioning path (CN18/§9.4).
7. A manual run of the `architecture.md` §13 example starts `dnv-agent cn`, serves
   `GetCnSize`, and a `SyncupCn`/`SyncupCntlr` round-trip shows one trace id
   across its `grpc server request`, `os command` and `os write file direct`
   records, and each `CheckCntlr` round's `os …` records carry the
   `trace_id` in its request (the `data` of its `grpc server recv` record;
   the record's own `trace_id` is the stream's — SH24, `grpc.md` T3); the
   `probe write block` / `probe read block direct` records of the CN11 leg
   probers appear on their own per-attempt trace ids (CN2), never on an
   RPC's — and they carry no `os command` framing, because the prober
   issues its IO directly (§2.2).
8. A **primary**'s `SyncupCntlr` whose legs are all `provisioned = false`
   issues zero `nvme connect` and `mdadm` calls (a standby's sweep issues
   `mdadm --stop` for an array its empty group set no longer wants, and
   `mdadm --detail` never, CN12) — the dm devices it does create are the
   error-backed shape: each td's `CnErrorName`, the ns-devs on it (CN16
   rule 0, [D15]) and any transfer's `CnXferFinalName` as an error table
   (CN17) — and every affected `ResInfo` is `RES_STATUS_PROVISIONING` —
   never `RES_STATUS_ERROR`.
9. §5 records every change the design-review pass made.
10. Both greps of this item carry `--exclude=*_test.go`, because both are
    claims about what the shipped agent code contains and the tests of §6
    test 27, `dnagent.md`'s §6 test 23 (`agent/dnagent/conf_test.go`) and
    `agent/conf_test.go` deliberately quote the same strings back:
    `grep -rn --exclude=*_test.go "invalid stored conf" agent/` finds only
    `agent/conf.go`'s error builder (plus the doc comment above it) and the
    `msgInvalidStoredConf` constant in each role package — one string
    reaches every refusal, in both agents and in the worker. Each of the four
    `ValidateBdevConf` messages in `agent/conf.go` greps byte-identical out
    of `model/capacity.go` (`dnagent.md` §2.1), and
    `grep -rnE --exclude=*_test.go "DefaultDmPoolDataBlockSize|DefaultPoolLowWatermarkPct|DefaultDmRaid0StripeSize|DefaultChunkBlockCnt" agent/`
    finds nothing: the cn agent substitutes no conf default of its own
    (CN13).
11. Clone chunks are pair-addressed with no migration residue left in the
    role: `grep -rn --exclude=*_test.go "BmIdxList" agent/cnagent/` finds
    **nothing** — CN20 fills `chunk_id_list` only — and the lowercase
    `grep -rn --exclude=*_test.go "bm_idx_list" agent/cnagent/` finds only
    the doc comment in `push_clone_bm.go` saying why the field stays unset.
    `grep -rn "ContiguousPrefix\|agent.NewChunkSet" agent/cnagent/` finds
    nothing at all — concatenation is the migration placement rule (SH23),
    and applying it to clone chunks would put every chunk past a gap at the
    wrong offset. `agent.ChunkSet` is reachable only from `agent/dnagent/`.
12. The md reads are sysfs (CN12, *added 2026-09-26*): `grep -rn
    '"--detail"' agent/cnagent/*.go` finds nothing — tests included — and
    `grep -rn 'parseMdExportDevices\|devNoSet' agent/cnagent/` finds
    nothing at all.

### Known limits

* **A member md failed stays failed** (2026-09-26): `reconcileMembers` adds
  only a member the array does not hold, so a held `faulty` member is not
  re-added when its side returns (the array stays degraded until a spare
  switch or a re-assembly; pinned by `TestGroupHeldFaultyMemberStaysHeld`);
  the follow-up is `--remove` then `--add` of a held wanted member whose
  `MdMember.State` has the `faulty` flag, tested by membership
  (`faulty,failfast`). The `--remove` must come first: mdadm opens an
  `--add`ed device `O_EXCL`, and md keeps its claim on a faulty member
  until the member is removed, so an `--add` alone is refused. Nor does the
  late-member retry (CN12) reach it: lateness is availability alone, and
  the held faulty member's leg is available again.
* **A killed `--remove` leaves a spare switch half done** (2026-09-26): a
  switch applied within seconds of the switched-out leg's side dying can
  have its `--remove` killed at the soft timeout while a superblock write is
  stuck on that member (CN12). The pass reports the group `ERROR` and does
  not add the promoted spare, and nothing re-drives it: the worker re-syncs
  on a revision or a reply code, never on a row, and a group error
  registers no CN10 background retry (the CN10 retry does finish the
  switch when the same pass registers it for something else, such as a
  failed leg connect or any `leg_list` member of the cntlr that is not
  available — the promoted spare or, after a whole DN died, another
  group's leg still on it, CN12). With no such trigger left — a lone switched-out
  leg, or the last switch off a dead DN — the array runs on its surviving
  member until the cntlr's next converge for some other reason; the
  follow-up is to register the background retry when member reconciliation
  fails.
* **An unanswered array can leave a group unassembled** (2026-09-26): a
  group with no answering array while another array of the walk did not
  answer is neither created nor assembled that pass (CN12), and the error
  is a row, which nothing re-drives by itself; the Check verdict stays
  non-zero only while its own enumeration, `ListArrays`, still fails.
  Once the array
  answers, the group waits, its row `MISSING`, for the cntlr's next
  converge for some other reason — and the groups this can hit include
  those a new primary's first converge must assemble, whose slice layers
  cannot be built meanwhile. The follow-up is the same background retry
  as the killed `--remove`'s, registered when a group lookup or assembly
  fails for a reason that is not the group's own — the late-member retry
  of CN12 does not reliably cover it: it runs only while some `leg_list`
  member of the cntlr is not available, and the group's own legs, like
  every other leg, may all be available.
* **A failed `--examine` read answers "no superblock"** (2026-09-29):
  `Md.HasSuperblock` keeps a killed `mdadm --examine` apart from a member
  without a superblock, but not one that answered after its path's
  failfast expired — the read's IO error comes back as `No md superblock
  detected`, exit status 1, the fresh-leg answer (CN12). It needs that
  failfast to expire between the leg's availability read and the probe's
  answer in one pass, and it reaches case 1 only when no member of the
  group answers with a superblock; that `--create --assume-clean` lands
  over live data only if a path of that leg reconnects before it runs.
  Nothing tells the two answers apart yet.
* **A park whose load fails can wedge the cntlr** (2026-09-29): a reload
  fails closed (CN16) and P0 only logs a park that fails (CN21), so an
  unwanted ns-dev that was still serving stays dm-suspended on its old
  table while L1 runs over it. A namespace CN9's pre-step 1 has not moved
  — its loop is over the plan, so one dropped from `ns_list`, one under a
  subsystem that left the request, or any of an sp no longer in the
  `cntlr_pointer_list` — is still in the ANA group it had when the park's
  suspend lands, and if that is `AnaGrpIdOptimized`, host IO that arrives
  after the suspend queues in dm. L1's `enable = 0` (or the one inside a
  whole subsystem's removal) then waits in the kernel for that IO: a
  configfs write returns only when the kernel does (`osclient.md` §4.3),
  and the pass holds its CN1 locks — the cntlr's object lock, or the node
  write lock at node level and at startup (CN21) — so no later pass
  reaches the reload that would release it. A namespace no host IO reaches
  after the suspend is not exposed; today only a reload or resume of the
  ns-dev from outside the agent ends it.
* **A namespace dropped from a legacy dnv-prefixed subsystem stays
  enabled** (2026-09-29): the sweep is the only remover of a namespace
  (CN21), and it never attributes a subsystem whose NQN carries the dnv
  prefix but decodes to nothing — neither to an sp nor as unowned (§2.1)
  — so a namespace `DeleteNamespace` takes out of one stays enabled on
  every CN that had it, and that sp's chain stops at the ns-dev layer on
  every pass (`architecture.md` §7).
* **A member that stays unavailable costs a converge every 5 s, under the
  lock every Check round needs** (2026-09-26): the late-member retry
  (CN10/CN12) runs the whole converge
  of the cntlr every `CnConnectRetryInterval` seconds until every
  `leg_list` member is available. On members that are genuinely dead
  that lasts until the worker's leg repair (`dnv-worker.md` AR8) has
  switched the last of them out of `leg_list`, since the retry is
  cntlr-wide and AR8 takes one step per pass of the SP (AR2), one pass
  every `cntlr_interval`: for a dead DN, `side_unhealthy` (600 s by
  default), then, for each of the SP's `leg_list` legs on that DN, a spare
  create (unless its group has a ready or pending spare already) and a
  switch, at least one pass each (the spares' provisioning overlaps). It
  has no bound when AR8 does not switch a late member: a RedundNone leg
  (AR8 only logs for one, and the gateway's `CreateSpareLeg` refuses its
  group a spare: "no redundancy to repair"), a leg with two sides (a
  migration in flight, which AR8 leaves alone, `leg_has_two_sides`; it has
  no available path once the dst's DN is dead, or the src's before the
  dst's path is `optimized`, `architecture.md` §11.2 dst step 5), a full
  spare list (`spare_list_full`, AR8 step 4), a spare of the group left
  unprovisioned on a DN that failed while it zeroed (`spare_unprovisioned`,
  step 3), an SP whose reactions are
  suppressed (AR3), no DN to place a spare on (step 3), or a pending spare
  that step 2 keeps waiting for (one the primary never reports). The retry
  then runs until the DN returns or an operator acts.
  Each attempt is a full converge of a built cntlr — probes, the sweep's
  enumerations, no creates — the profile the connect retry already has
  while a side's connect fails (a leg's controller is connected with
  `--ctrl-loss-tmo -1`, SH20, so a side that dies while its controller
  survives is late rather than a failed connect; once the controller is
  gone — its reconnect refused with DNR, as when the side's port link is
  gone while the port still listens, or lost to a CN reboot — the next
  converge's `nvme connect` fails while the side stays down, and registers
  the same retry). The cost is not CPU alone (*amended 2026-09-26*, found
  reviewing the late-member retry): an attempt holds the cntlr's object
  lock for its whole converge, and every `CheckCntlr` round takes that
  lock too (CN1). The worker waits for a round's reply at most
  `cntlr_interval` (`dnv-worker.md` RW8; 5 s by default, as
  `CnConnectRetryInterval` is); a round that waits behind an attempt past
  that is a missed round, which stamps `Cntlr.err_epoch` (HL2), and once
  that has aged `primary_unhealthy` (5 s by default) AR5 can fail over a
  primary whose only fault is a dead leg — and the new primary inherits
  the same late member, and with it the same retry. The connect retry has
  always held that lock too, but only while a side's connect fails — and
  since the CN10 pass budget (2026-09-28) such an attempt also spends about
  `CnConnectPassBudget` more of it on in-pass retries and head waits — more
  when a retried connect itself runs long; a dead
  DN whose controllers survive registers the retry only as a late member,
  so before the late-member retry such a DN caused no attempt at all. How
  long an attempt holds the lock grows with the slice count. Counted on
  the unit tests' fake node at the e2e suite's shape (md-raid1, two legs
  per group, one leg late, one thin device), an attempt spawns 21 + 53
  processes per slice (1,717 at 32 slices) and a Check round 18 + 28 per
  slice (914); each further thin device adds about 5 per slice to both.
  At the one spawn rate measured on a lab cn guest, about 240 a second
  (`e2e_integtest.md` §8 item 12), that is about 7 s for an attempt and
  about 4 s for a Check round at 32 slices, and an attempt plus a Check
  round passes 5 s from about 15 slices on. An attempt that outlasts
  `CnConnectRetryInterval` finds the loop's next tick already due and is
  followed by the next at once, so the lock is then almost always held,
  and a Check round waits out the rest of the attempt in progress before
  its own probe. None of this is measured on the lab yet: the one-slice
  suites cannot show it (an attempt there is about 75 spawns), and the
  e2e `react` case's stage 05 — a dead DN at 32 slices, asserting no
  failover during it (`e2e_integtest.md` §4.5) — is the run that will.
  The guard against this failover, not an optimization, is a pre-check
  that runs the converge, and takes the object lock for it, only when the
  converge has something to act on — a late member available again, a
  provisioned side with no controller, CN18's clone triggers — read from
  the desired state and sysfs with nothing remembered. It is not done now,
  so that the cn retry loop stays the DN13 pattern (the comment on
  `connectRetryLoop` says why the two copies must not drift apart); that
  stage 05 run decides whether it must be.
* **A promotion that outruns the sides' flip serves IO errors until the
  retry builds the stack** (2026-09-26): CN16's ANA rule reads the plan —
  primary, not disabled, not effectively suspended, not deferred — and not
  the stack, so the promotion's own converge moves its namespaces to
  `optimized` even when its late members kept the stack from being built
  — a group with no available leg, a pool over a side that still exports
  dm-error — and the ns-dev reload onto the raid0 (CN16 rule 6) failed
  with the raid0 missing: the ns-dev stays on the td's `CnErrorName`, the
  standby table of rule 2. A host's IO on that path fails with
  target-internal (DNR) errors, not path errors — the IO it queued while
  no path served included — until the first CN10 retry attempt after the
  sides' flips have reached this CN's sysfs builds the stack and reloads
  the ns-dev onto the raid0, within about one `CnConnectRetryInterval` of
  that. Before the late-member retry it lasted until the cntlr's next
  converge for some other reason, usually the next revision bump
  (`architecture.md` §11.1 new_primary step 4). The follow-up is a
  [D15]-style ANA conjunct — `optimized` only once the ns-dev is on the
  backing it wants — which is not done here.
* **A clone build that keeps failing with its dm-clone up shows only in its
  Syncup rows** (2026-09-29): CN16 keeps a clone's td parked while its
  dm-clone does not show hydration enabled (rule 5). A failed CN18 step that
  stopped the build before step 5 enabled hydration registers the CN10 retry
  (all but a failed allocation or replacement of the metadata wrapper, whose
  `clone_id_to_meta` row does not read `OK`), which re-runs the converge
  every `CnConnectRetryInterval` seconds until the build finishes. What
  keeps failing may leave the dm-clone up with hydration off: among others a
  refused enable, a `dmsetup create` killed after its ioctl ran, or a stale
  dm-clone that will not go while a later step fails, such as the read of
  its dst bitmaps. A Check round in between that can read the node and finds
  the source's paths live then reads the parked ns-dev as the table CN16
  wants and the clone's rows `OK` (the dm-clone row's raw status carries
  `no_hydration`), so the worker's health pass sees a clean primary while a
  host takes IO errors on the namespace, which stays `optimized`: the
  recovery's own IO-error window (CN18 step 4), drawn out for as long as the
  build keeps failing. These ways of failing do show in the Check rows: with
  the dm-clone gone between attempts (step 4's fail-closed removal, a
  refused create), `clone_id_to_dm_clone` reads `RES_STATUS_MISSING` and
  `ns_id_to_dm_linear` `RES_STATUS_ERROR`, because the status read of an
  absent dm-clone fails (`nsDevNow`); an arena listing or a status read that
  keeps not answering leaves its rows `RES_STATUS_ERROR`; and a source whose
  controllers are gone or not live reads `RES_STATUS_MISSING` or
  `RES_STATUS_ERROR` in `clone_id_to_target`. Nothing but the retry re-runs
  the build unless the cntlr is converged for another reason (a revision
  bump, a non-zero reply code, an agent restart). The follow-up is a
  `clone_id_to_dm_clone` row that says the build is unfinished, which needs
  a decision on how the worker's health pass treats it.
* **A controller delete whose target vanishes mid-delete waits out the
  kernel's admin timeout** (2026-09-29): when a disk node removes a side's
  export while this CN's `nvme disconnect` of the leg is mid-delete — the
  two ends of a leg removal, or of a pool drain, are separate RPCs to
  separate agents — the kernel cannot enter error recovery on the DELETING
  controller, so its shutdown command waits out `nvme_core.admin_timeout`
  (60 s), and the disconnect with it, in an uninterruptible write that no
  SH15 signal ends. The e2e runs of 2026-09-28 hit it three times in four at
  slice counts up to 4. The sweep issues the disconnect off its locks
  (CN10's disconnect registry), so the stall no longer holds the cntlr's
  object lock and its Check rounds, a `SyncupCn`'s node write lock, or the
  worker's `SyncupCntlr` past its deadline; the connection reads as a
  leftover until the kernel lets go, and the child keeps one of the node's
  `DefaultOsClientLimit` `OsClient` slots for the whole wait
  (`osclient.md` §4.2). The pool drain keeps its cross-role race for now —
  it deletes an sp's cntlrs and then its slices and waits on no agent
  (`dnv-worker.md` SPD13) — and how often it hits the stall is to be
  measured. A drain that catches many deletes in the kernel at once holds
  at most `disconnectConcurrency` (8) of those slots (CN10), and the rest
  of its disconnects wait for one: their connections stay leftovers, and a
  converge that wants one of them back waits as well, about a minute for
  every eight stuck deletes ahead of it. Two
  disconnects still run inline and can meet the same wait under their
  locks: the build's dead-path `nvme disconnect --device` of a migration's
  retired side (CN10) — the source after `FinishMigration`, the
  destination after `CancelMigration` — whose disk node tears that side's
  export down in the same fan-out, and the dn sweep's disconnect of a `:3:`
  migration-source connection (`dnagent.md` DN6), whose source disk node
  drops the migration export in the same fan-out. Neither is moved off its
  locks yet.
* **A clone source judged unused can be adopted before its disconnect is
  registered** (2026-09-29): a cntlr-level L5 reads the stored requests and
  the live dm-clone tables, probes the source, and only then sets its
  disconnect going (CN10). Another cntlr of the same CN converges beside it
  under its own object lock. That cntlr's request can be stored after the
  read, and its CN18 step 1 can pass the registry check before the
  disconnect is registered; it then adopts the still-connected source and
  builds its dm-clone on it, and the disconnect deletes the source
  underneath. Reads of regions not yet hydrated then fail until a later
  converge of that cntlr connects the source afresh and reloads the table.
  The window runs from that read to the registration; while the disconnect
  ran inline under the lock, it lasted the whole disconnect as well. Not
  closed here.
* **An absent clone-metadata arena waits for the next `SyncupCn`**
  (2026-09-29): a tmpfs that really is absent — after a reboot, or on a new
  CN — whose `findmnt` goes unanswered on both askings of a converge is not
  mounted by that pass, and the arena file and loop device are not created
  without it (CN5); an absent arena file whose `stat` goes unanswered on
  both askings is not created either. A refused `mount`, `truncate` or
  `losetup --find` leaves the arena short the same way. The error is a row,
  which nothing re-drives by itself: a check round reads what is absent
  `MISSING`, and the `CheckCn` verdict, the sweep's alone, stays clean
  (CN30). The arena then waits for the CN's next `SyncupCn` or the agent's
  next start, and until then CN18 has no loop device to allocate clone
  metadata from, so no clone on the CN can be built. The follow-up is a
  `CheckCn` verdict that is not clean while a base-state row reads
  `MISSING`, which needs a decision.

### Integration-run fixes (first on-hardware run of the amended tree)

Found by running `integtest/dnagent_test.sh` and `integtest/cnagent_test.sh`
against two real VMs (kernel 7.0, nvme-cli 2.16, mdadm 4.5) — the first
execution of either suite since the design-review pass was applied. All five were
real agent defects, not harness problems; every one is now covered by a unit
test that fails without the fix. The four that touch this role are listed
below; IR4 (DN6, the dm-clone retire order) is dn-only and is recorded in
`dnagent.md`'s Integration-run fixes.

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
