# dnagent.md — `dnv-agent` and the dn role

This document owns the `dnv-agent` binary: the mechanism package `agent`
both roles share (bootstrap and lifecycle, the local store, the revision and
conf gates, the lock hierarchy, `ResInfo` tracking, the dm, nvmet and
nvme-host wrappers, the bitmap-chunk store and the check-stream rules), the
`cmd/dnv-agent` command, and the dn role, package `dnagent`, which serves
`DiskNodeAgent`: the startup reconcile, `SyncupDn` with the on-disk format,
side provisioning and zeroing, the per-CN exports and the sweep, `SyncupSide`
with the migration source and destination roles and the fence, bitmap
pushes, the info and check handlers, and probing. `cnagent.md` adds the cn
role to the same mechanism and command and does not restate them. This
document leans on `log.md`, `osclient.md` and `grpc.md` for the rules it
builds on (one Info record per operation, `OsClient`-only OS access, trace-id
propagation and the four shared interceptors), on `schema.proto` for the
service and its messages, and on `architecture.md` for the disk-node device
stack, the names, the agent local-store paths, the common agent rules, the
migration procedure, the levels, the cntlid slots and the decisions [D4],
[D12], [D13], [D14] and [D15].

## Scope and placement

| package | path | role | may import (`layout.md`, Dependency rules) |
|---|---|---|---|
| `agent` | `agent/` | shared dn/cn **mechanism**: bootstrap, local store, revision gate, locks, `ResInfo` tracking, OS wrappers, bitmap store, check-loop rules | `common`, `pb` |
| `dnagent` | `agent/dnagent/` | dn **policy**: the `DiskNodeAgent` service — which extent allocations, dm tables and nvmet objects to build and when | `common`, `pb`, `agent` |
| `main` | `cmd/dnv-agent/` | cobra `dn`/`cn` dispatch, viper flags, dependency construction | `agent`, `agent/dnagent`, `agent/cnagent`, `common`, `pb` |

Agents never talk to etcd (`layout.md`, Dependency rules). The gRPC server
carries the **server** interceptors of `grpc.md`, Wiring, and no client
interceptors — the agent's only outbound connections are `nvme connect`s,
not gRPC. The split rule for new code: anything both roles need verbatim is
mechanism and belongs in `agent`; anything that knows *which* resource to
build is policy and belongs in `dnagent` or `cnagent`.

## Shared mechanism — package `agent`

### Files

`agent.go` holds the bootstrap (Bootstrap and lifecycle), `store.go` the
local store, `revision.go` the revision gate and the reply builders,
`conf.go` the stored-conf validators (the rejection they build is SH9),
`locks.go` the lock hierarchy, `resinfo.go` the `ResInfo` tracker,
`oswrap.go`, `dm.go`, `nvmet.go` and `nvmehost.go` the OS wrappers
(`oswrap.go` is the shared command and configfs plumbing the other three sit
on), `bitmap.go` the bitmap-chunk store, `sweep.go` the part of DN6's sweep
both roles share (the leftover kinds, the result value a pass accumulates,
its log record and the `AgentReply` it becomes) and `waitbudget.go` the
`WaitBudget`, the bounded wait one converge pass may spend on the node
catching up with what that pass asked of it: DN13's wait for the source's
namespace, and the cn connect step's pass budget (`cnagent.md` CN10 and
CN18).

`conf.go` is a **deliberate second copy** of `model`'s stored-conf rules.
`layout.md`, Dependency rules, forbids the agent packages from importing
`model`, which links the etcd client, so `agent.ValidateBdevConf` and
`agent.ValidateExtentSize` restate the presence checks of
`model.ValidateBdevConf` and `model.ValidateClusterConf` with
**byte-identical error strings** — the same "invalid stored conf" prefix and
the same proto field names — and `agent.InvalidConfReply` turns one into the
SH9 rejection. Identical by construction is the point: one grep finds every
refusal across the control plane and both agents, and the two sides pin the
same strings so that a change made to one copy and not the other goes red.
Both validators check **presence only** — the range checks of
`architecture.md`, Common validation, belong to the gateway, which sees the
request that set the value — and `low_water_mark_pct` is checked for zero
alone, because a mark that cannot be reached is the legal "never grow this
pool automatically" setting.

The file list above is a guide to the package; package boundaries are
binding (`layout.md`, Directory tree), file names are not. There is no LVM
wrapper: **no** dnv agent runs any LVM command at all ([D13] on the dn,
[D14] on the cn).

### Additions to `common`

The agents' constants live in `common/constants.go`, the name helpers in
`common/name_fmt.go`, and their inverses in `common/name_parse.go`: DN6's
sweep has to look at a name the kernel hands back and say whether it is
ours, of which kind and of which sp, so the two name shapes the sweep
decodes have parsers there — `ParseDmName` for the dm device names (every
`DmKind`) and `ParseNqn` for the dnv-format NQNs (every `NqnKind`). The rest
of the name helpers have no inverse: the `/dev` and local-store paths, the
tmpfs paths and the md names are formatted only — an md array is attributed
by its members' dm names instead (`cnagent.md` CN21) — and the hashed forms
(`DnNsIdentity`, `NvmeHostId`, `CnMdDevName`'s short id) could not have one.
Parsing is strict: a name that does not decode exactly is "not a dnv name",
never a half-decoded one, because the caller's next move is a removal.

The constants this document relies on, whose values and comments
`common/constants.go` holds:

* `NvmetPortId`, the default configfs id of the nvmet port an agent
  converges (exactly one port per agent, `architecture.md`, Disk node);
  `dnv-agent --nvmet-port-id` overrides it, which is what lets several
  agents share one node's kernel, each converging its own port.
* `AnaGrpIdOptimized`, `AnaGrpIdNonOptimized` and `AnaGrpIdInaccessible`,
  the three fixed ANA groups on every node's port ([D4]). The first always
  exists in nvmet and defaults to optimized; the other two are created at
  port setup. Group states are written once and never changed; every ANA
  transition rewrites a namespace's "ana_grpid" instead.
* The `AgentReply.code` values. Zero is OK. `ReplyCodeStaleRevision`,
  `ReplyCodeUnknownObject` and `ReplyCodeInvalidConf` are rejections: the
  request was not applied, so the worker reads no verdict from the reply's
  rows. `ReplyCodeInvalidConf` refuses a request whose conf carries a value
  the control plane cannot have written — a proto3 zero where
  `architecture.md`, Common validation, requires a concrete geometry; the
  object is known and the revision is current, it is the conf that is
  unusable, which is why it is neither of the other two.
  `ReplyCodeLeftover` reports an **accepted** request with residue: the
  desired state is stored and every wanted object was converged, but the
  node still holds objects the desired state does not want, or an
  enumeration of what exists did not answer. It is the one thing that
  travels in `agent_reply` rather than in the `*Info` rows, because a
  leftover by definition has no row — nothing wanted names it. An agent also
  reports a few conditions this way so that the worker re-sends the
  `Syncup*` whose converge acts on them (`architecture.md`, Teardown by
  sweep): on a dn a disk identity not yet confirmed or a side with extents
  to zero and nothing zeroing it, on a cn a piece of the node's base state
  that a `CheckCn` round's or `GetCnInfo`'s probe read absent, or an ANA
  group it read in a state other than its fixed one on a port whose
  transport attributes match. It is not a rejection: the worker evaluates
  the reply's rows exactly as for code zero and re-issues the `Syncup*`
  every round (`dnv-worker.md` RW12, no backoff) until the code changes.
  "Pending" is never stored anywhere; the code is recomputed by enumerating
  the node on every `Syncup*` and every `Check*`. Any non-zero code in a
  `Check*` reply makes the worker issue a `Syncup*` (`dnv-worker.md` RW4),
  and in a `Push*Bitmap` reply ends that push plan. Among the rejections
  the specific value only sets the level of the worker's "syncup rejected"
  record and otherwise serves details and tests.
* `DnMigrConnectRetryInterval`, the pace of the background retries of a
  pending migration-destination `nvme connect` (DN13; the loop is SH27's
  "DN8 retry", so nicknamed for the DN8-gated converge it re-runs).
* `DnMigrDstNsWait` and `DnMigrDstNsPause`, the migration destination's
  wait for the source namespace after a connect that succeeded (DN13):
  re-read, pausing the latter between reads and the former in all. Not a
  connect retry — one connect per pass stays the rule.
* `DnExportOrphanGrace`, the age a namespace-less side export linked to no
  port but the agent's own must exceed before a sweep removes it (DN6):
  until then it may be a sibling agent's build in flight. The age is the
  subsystem directory's mtime, read from the node.
* `DnZeroBatchExtCnt`, `DnZeroRetryInterval`, `DnZeroConcurrency` and
  `DnZeroKillBackoff`, the side provisioning knobs ([D15], DN9): the
  background zeroing goroutine zeroes at most `DnZeroBatchExtCnt` logical
  extents per `blkdiscard --zeroout` command, through the side's dm-linear,
  and persists that batch's `zeroed_bits` after each success. The batch size
  assumes fast hardware Write Zeroes: a batch should zero inside
  `CmdSoftTimeout` at the disk's rate split `DnZeroConcurrency` ways. A
  failed or timed-out batch is retried no sooner than
  `DnZeroRetryInterval` later — the zeroing twin of
  `DnMigrConnectRetryInterval`, never a hot loop. At most
  `DnZeroConcurrency` zeroing batches run at once per agent; a batch the
  soft timeout killed halves the side's next batch, a success doubles it
  again up to `DnZeroBatchExtCnt`, and `DnZeroKillBackoff` kills in a row
  drop the side to one extent per batch — rate control in the side's
  goroutine only, never a record of what is zeroed.
* `SuspendSeconds`, the source-cutover grace window of `architecture.md`,
  Migration: a migration source's per-CN dm-linears stay suspended at least
  this long before they are reloaded onto their dm-errors — sooner where an
  export above one is to go (DN12, [D12]).
* The dnv disk format ([D13]), all byte offsets on the raw `--disk`
  device: `DnHeaderOffset` and `DnHeaderSize` (the header block),
  `DnTableSlotAOffset`, `DnTableSlotBOffset` and `DnTableSlotSize` (the two
  volume-table slots), `DnCloneMetaOffset`, `DnCloneMetaSize` and
  `DnCloneMetaUnit` (the dm-clone metadata slot area and its allocation
  granularity) and `DnDataOffset`, where the extent area starts — fixed for
  the life of the format.

`DnNsIdentity`, a package-level function of `common/name_fmt.go`, derives
the deterministic namespace identity both sides of a leg MUST present
identically (`architecture.md`, Disk node): a hash of the cluster, sp and
leg ids, rendered as an RFC-4122-shaped uuid string and as a bare hex nguid.

### Bootstrap and lifecycle

SH1. The lifecycle is **reconcile, then serve** (`architecture.md`, Common
agent rules): the gRPC listener MUST NOT open before the startup reconcile
finished. A fresh node has an empty local store — an empty directory, which
must already exist: the agent creates none, and a missing one is SH3's fatal
case — so its reconcile is instant.

SH2. The startup reconcile runs under a ctx carrying a freshly minted trace
id (`common.NewTraceId`), so every startup "os command" record is
correlatable.

SH3. Reconcile returns an error only for **fatal** conditions (the
local-store prefix unreadable); per-resource failures are captured as
`RES_STATUS_ERROR` (`ResInfo` tracking) and never abort startup.

SH27. **Background tasks and process exit.** (Numbered last because SH rules
are append-only — SH1 to SH26 are cited from code comments and must not
shift.) A role server MAY run goroutines outside any RPC: the DN8
migration-connect retry (so nicknamed for the DN8-gated converge it re-runs;
the retry loop itself is specified in DN13), the DN12 fence timer, the DN9
side-zeroing workers, the cn's connect retry, the CN11 leg probers and the
cn sweep's background `nvme disconnect` (`cnagent.md` CN10). Every one of
them derives its ctx from the server's **`rootCtx`** — the process-lifetime
ctx captured at `Reconcile`, which `Serve` derives from its own ctx and
cancels before returning — and, the background disconnect aside, mints a
fresh trace id per attempt (`common.NewTraceId`), taking the SH11 locks for
the attempt only, never across the whole task. The background disconnect is
one command of the pass that set it going: it carries that pass's trace id
instead of minting one and, like the CN11 probers, takes no SH11 lock at
all.

A background task may additionally run a **child process**, and DN9's
`blkdiscard --zeroout` batches are the first that does. Such a child must
not outlive the agent — the cn sweep's background `nvme disconnect` (below)
is allowed to — so the dn server registers every goroutine that owns one in
a wait group before it starts and exposes a `WaitBackground` that waits for
them; `Serve` takes it as its `waitBackground` parameter, cancels the task
ctx after the graceful stop and then waits. Cancellation kills the in-flight
child through the SH15 soft/hard timeout machinery (`osclient.md`,
RunCommand, sends SIGTERM then SIGKILL), so the wait is bounded by
`CmdHardTimeout` per in-flight command — unless the child sits in an
uninterruptible kernel wait, which no signal ends: the join then lasts until
the kernel returns (SH15).

**Only child-owning tasks are waited for.** The cn passes a nil
`waitBackground`: a CN11 prober's IO is a direct, uninterruptible syscall
(`cnagent.md`, Leg-probe IO leaves the `OsClient`), so joining it could hang
shutdown forever — cancelling is the whole contract there. Nor is the child
of the cn sweep's background `nvme disconnect` joined (`cnagent.md` CN10):
it holds nothing a restarted agent needs, a delete already in the kernel
finishes whether or not anybody waits for it, and a join could hold shutdown
for the kernel's whole admin timeout. Object-scoped tasks that hold a device
open are additionally cancelled **and waited for** at teardown, before the
resources they hold are removed (DN6, DN9): a live child keeps an fd on the
dm device and `dmsetup remove` would fail EBUSY.

One deliberate carve-out from "every goroutine that owns one": the DN12
fence timer's callback — which runs a full side converge and so can spawn
children — is **not** enrolled, because the timer can fire after
`WaitBackground` has returned and a wait-group add after its wait panics
(the enrollment sites record this). It is benign: the callback derives from
`rootCtx`, which `Serve` cancels before the join, so a post-join firing
finds every OS call refused by a dead ctx and spawns nothing.

`Serve` in `agent/agent.go` is the shared lifecycle: it mints the startup
trace id, derives the run ctx the role servers capture as `rootCtx`, runs
the role's reconcile, listens, builds the gRPC server with the two server
interceptors, registers the role's service and serves until its own ctx is
cancelled (SIGTERM or SIGINT, CM4), when the graceful stop drains every RPC.
The cancel-then-join pair is deferred, so **every** return path takes it,
not just the one through serving: the reconcile has already armed the
background goroutines (and forked their children) by the time a reconcile
or listen error returns, and cancellation alone does not reap a child — the
watchdog that turns it into SIGTERM and then SIGKILL lives in this process
and dies with it, so the child would be reparented to init still holding its
dm device open. Cancel first, then join, in that order: the background
goroutines only unwind on cancellation, so joining first would hang shutdown
for ever. `CheckRoundCtx` is SH24's helper: the stream's ctx re-keyed to the
trace id a round's request carries, or the stream's own when that id is
empty.

### Local store — `store.go`

SH4. The store holds exactly what `architecture.md`, Common agent rules and
Agent local-store paths, prescribe: the last fully applied `Syncup*Request`
per object and one file per received `Push*BitmapRequest` chunk, written via
`OsClient.WriteProto` (atomic replace) at the `Local*Path` locations, read
back via `ReadProto`.

SH5. An **object** request (`SyncupSide`/`SyncupCntlr`) is persisted after
its converge pass completes. A **parent** request (`SyncupDn`/`SyncupCn`)
is persisted as soon as it has passed the gates and become the desired
state — before the converge, and so before the sweep that removes the
resources of a pointer which has just left the list (DN6). The parent's
pointer list is the authoritative record of which objects still exist, and
the sweep that acts on a shortened one can block for a whole failfast window
on a dead remote: a save that waited for the sweep would be skipped by a
request cancelled inside that window, and the next startup reconcile would
then rebuild the object from the **old** list, against sides that no longer
exist. With the new list on disk first, a crash mid-sweep is nothing worse
than a startup sweep. Per-resource `RES_STATUS_ERROR` outcomes do not block
persistence — the errors travel in the `*Info` and the worker's health loop
drives repair (equal-revision re-syncs re-apply idempotently). Protocol
rejections — stale revision, unknown pointer, invalid stored conf (SH9) —
never reach persistence.

SH6. Enumeration on startup: one `ls` of the prefix, run through
`RunCommand` on a ctx carrying the SH15 soft timeout (the store calls the
`OsClient` directly, so it applies that bound itself), filtering by the
role's `Local*Path` kind prefixes (`StoreKindDn`, `StoreKindSide` and
`StoreKindMigrBm` for the dn; `StoreKindCn`, `StoreKindCntlr` and
`StoreKindCloneBm` for the cn). File names are used only for discovery; the
ids come from the decoded protos. The committed file is the only truth. A
name of one of those kinds that carries `AtomicWriteTmpInfix` is no store
file but the temp file of a `WriteProto` that did not succeed — one whose
process died before its rename, for instance; `osclient.md`, ReadFile /
WriteFile / WriteFileDirect, lists every way one is left behind. The
enumeration never returns one, so nothing reads it: nothing tells a whole
one from a half-written one, which can decode with its tail missing, and
what it holds was never committed (a save that fails is only logged), so the
restarted agent reports only what was committed, and the worker sends again
whatever that lacks. It sorts right after the file it was meant to replace,
so a reload that decoded it would let it win — and, left on disk, win again
at every later restart, long after later saves had overtaken it. So the
enumeration also deletes every such file of the role's kinds, before
anything is loaded, in one `rm -f` under the same bound as the `ls`. A
delete that fails is logged and never fails the reconcile; the next
startup's enumeration finds the file again. The other role's kinds are left
alone: a dn and a cn agent may share one prefix (CM2), and the other agent's
temp file may be a write in flight. A temp file of the role's own kinds
never is: the enumeration runs before the agent saves anything, and two
agents of one role must not share a prefix (CM2).

SH7. When an object's pointer leaves its parent's list, its state is dropped
**at that moment** and nothing of it is removed from the node yet: its
request file and its bitmap-chunk files are deleted with one `rm -f` under
the same bound as SH6's `ls`, its memory entry and object lock go, and its
goroutines are stopped. The resources are found afterwards **by name**, by
the sweep that runs later in the same pass (DN6), so no file has to be kept
as a to-be-deleted list. Keeping the file until the resources are gone would
be the opposite of idempotent: a removal pass that only logs its failures
and deletes the file regardless destroys the one record naming the object
exactly when the object has failed to go. The exception is a file no memory
entry names — a chunk the startup reload skipped (DN2, `cnagent.md` CN2)
and no push has rewritten since, or the `side-*` or `cntlr-*` file of an
object it skipped that no `SyncupSide` or `SyncupCntlr` has rebuilt: it is
not deleted when its object's pointer leaves the list. The sweep still
removes the object's resources by name, and a later restart that decodes
the file finds the pointer absent and deletes it.

### Revision gate — `revision.go`

SH8. Per `architecture.md`, Common agent rules, with `stored` = revision of
the last fully applied request for the object, as this process holds it
(zero when it holds none, as for an object whose file the startup reload did
not load): `GateRevision` returns nil when the request may be applied —
incoming at or above stored, equal meaning an idempotent re-apply — and a
`ReplyCodeStaleRevision` rejection whose `details` name the incoming and the
stored revision for a stale one.

SH9. Unknown-object rejections use `ReplyCodeUnknownObject` with a
`details` string naming the missing pointer or id. A request whose conf
carries a value the control plane cannot have written is rejected with
`ReplyCodeInvalidConf` and the validator's message of Files (DN4,
`cnagent.md` CN8): the object is known and the revision is current, so it is
neither of the other two, and `details` names the proto field rather than
an id. All three rejection kinds return a normal gRPC reply
(`AgentReply.code` non-zero), never a gRPC error status — only
`GetDnSize`/`GetCnSize` and `Get*Bm` report failure through the status
(`architecture.md`, Common agent rules). A rejected request is never
persisted (SH5), whichever kind it is. `ReplyCodeLeftover` is **not** one
of these: it reports an accepted request whose node still holds unwanted
objects (DN19), so it is the one non-zero code that leaves the request
stored and its rows worth reading.

### Concurrency — `locks.go`

The control plane guarantees per-object ordering (sequential `Syncup*` per
object, one `Push*Bitmap` in flight per migration or clone) but different
objects MAY hit the agent concurrently, and `Check*` streams run alongside
`Syncup*` (`architecture.md`, Bitmap push protocol and Check streams). The
agent therefore serializes with a **two-level hierarchy**, `LockSet`: a
node-level read-write lock (`Node`) and one mutex per object (`Obj`),
created on first use and dropped (`DropObj`) only during teardown while the
node write lock is held. Lock order is always node before object.

SH10. Node **write** lock: the startup reconcile and the node-level syncup
(`SyncupDn`/`SyncupCn` — they diff the pointer list and run the node-level
sweep, which enumerates the whole node and may remove the resources of any
object **of this agent** on it).

SH11. Node **read** lock + the object's lock: every object-scoped RPC
(`SyncupSide`/`SyncupCntlr`, `Push*Bitmap`, `GetSideInfo`/`GetCntlrInfo`,
one `CheckSide`/`CheckCntlr` round) and every background converge attempt
(the dn's connect retry — DN13; DN1's and SH27's "DN8 retry" — and DN12
fence timer, which share `reconvergeSide`, and the cn's connect retry,
`reconvergeCntlr` — the SH27 background tasks that converge; `cnagent.md`
CN1).

SH12. Node **read** lock only: node-scoped reads (`GetDnInfo`/`GetCnInfo`,
one `CheckDn`/`CheckCn` round). `GetDnSize`/`GetCnSize` take no lock.

SH13. Lock order is node then object, never nested object locks. Probing
under a lock is acceptable: every OS command is bounded by
`CmdSoftTimeout`/`CmdHardTimeout` (SH15) except a child in an
uninterruptible kernel wait (SH15) — which is why the cn sweep's
`nvme disconnect` runs off the locks (`cnagent.md` CN21) — and an
in-process `OsClient` file or block call is bounded only up to its syscall:
its ctx is checked once, before it (`osclient.md`, ReadFile / WriteFile /
WriteFileDirect), so a sysfs, configfs or device access the kernel holds
returns only when the kernel does.

### `ResInfo` tracking — `resinfo.go`

SH14. A per-object in-memory tracker (`ResTracker`) turns probe outcomes
into a `pb.ResInfo` of `res_name`, `status`, `details` and `epoch` per
`architecture.md`, Live-state reporting: `epoch` is the unix seconds of the
last **status** change (a `details` change alone does not bump it); the
agent emits `RES_STATUS_MISSING`, `RES_STATUS_ERROR`, `RES_STATUS_OK` and
`RES_STATUS_PROVISIONING`, the cn agent also `RES_STATUS_PENDING` on a
primary's leg rows (`cnagent.md` CN11), and never `RES_STATUS_UNKNOWN`
(worker-only). `RES_STATUS_PENDING` means *the primary's prober for the leg
has not completed a round since it started (a build, a promotion or an agent
restart) — no verdict*, and it neither sets nor clears `Leg.err_epoch`
(`architecture.md`, Live-state reporting and sp role).
`RES_STATUS_PROVISIONING` means *deliberately not created yet, healthy, no
action needed*: it is what a resource waiting behind DN9's provisioning gate
reports, and unlike `RES_STATUS_ERROR` it never feeds `err_epoch`
(`architecture.md`, Live-state reporting, dn / cn roles, sp role and
Automatic reactions). A resource that leaves the desired state must lose its
history, or the next object built with the same id would inherit a dead
one's epoch and read as unchanged since. An object whose own converge
derives a per-resource key set — a side (DN6), a cntlr (`cnagent.md` CN9) —
therefore has its tracker pruned the way its devices are: `Keep` drops
every key the wanted set does not name, derived from the wanted set rather
than from a diff against a remembered plan. Tracker state is in-memory;
after a restart epochs restart at the reconcile time — acceptable, the
epoch means "last observed change".

### OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`

Thin, mechanism-only wrappers over the process's single
`common.NewLimitedOsClient` instance. They issue the commands the device
stacks of `architecture.md`, Data-plane device stacks, need; policy (which
device, which table) stays in the role packages.

**A reload fails closed.** `Dm.Reload` and `Dm.ReloadMulti` swap a live
device's table in three commands — `dmsetup suspend`, `dmsetup reload` (the
table on stdin for `ReloadMulti`), `dmsetup resume` — and return at the
first one that fails. A reload whose load fails therefore returns that error
with the device still **suspended** on its old table: the wrapper does not
resume it. One whose resume fails can leave it suspended too — on its old
table or on its new one, depending on where the resume failed — and so can
one whose suspend was killed after the kernel had carried it out (SH15).
That is deliberate. On the dn, a reload that moves a per-CN dm-linear onto
its dm-error is a fence — the old primary's linear on a primary flip
(DN10), DN12's phase 2, the step before the sweep's first layer (DN6) — and
resuming the old table after a failed load would leave the old primary live
on the side device beside the new one, or replay onto the side's data the
IO the cutover window absorbed. Suspended, the device serves nothing and
writes nothing; the cost is liveness. Its bios queue with no timeout
([D12]) until a later reload or resume of it succeeds. On the dn, the build
phase reloads a device whose live table is not the one it wants and resumes
one whose live table is, a fenced linear inside its cutover window aside;
the step before the sweep's first layer reloads onto its dm-error a
suspended linear it is about to remove or unexport; and the sweep of a side
that plays no migration source resumes each per-CN dm-linear of the side it
finds suspended, on whatever table is live (DN12 rule 2). The cn's paths are
`cnagent.md` CN16's. The rule is the wrappers', so it holds for every reload
of either role, the ones that fence nothing — a grow, a repoint — included,
and [D12]'s bound on a suspension holds only as far as the reloads succeed;
what the rule-2 resume costs when a flip's reload failed its load is a
known limit (Known limits).

SH15. Every wrapper call wraps its ctx with a deadline of `CmdSoftTimeout`
before calling the `OsClient` (the soft/hard timeout contract of
`architecture.md`, Common validation; `osclient.md`, RunCommand, handles
SIGTERM and SIGKILL). This covers the raw-device `ReadBlock`/`WriteBlock`
calls of the [D13] metadata path too. A role package that calls the
`OsClient` directly instead of through a wrapper — the `cnagent.md` CN12
sysfs leg walk, and CN25's read of the `thin_dump` file — takes the same
bound from the exported `agent.CmdCtx`. The bound holds in full only for a
child the signals end. An in-process `OsClient` call — a file, proto or
block read or write — is bounded only until its syscall starts: its ctx is
checked once, before it (`osclient.md`, ReadFile / WriteFile /
WriteFileDirect). A child blocked in an uninterruptible kernel wait is not
bounded at all: both signals are delivered, and the call returns, and gives
back its `OsClient` slot, only when the kernel does (an `nvme disconnect`
whose target vanishes mid-delete waits out the kernel's admin timeout;
`cnagent.md` CN21 runs the cn sweep's off the locks, while the cn build's
dead-path disconnect and the dn sweep's source-connection disconnect still
run under them — Known limits).

**A command that was killed did not answer, and "did not answer" is not
"absent".** `OsClient.RunCommand` returns an exit code of -1 with a non-nil
error when the process never reported — killed at the soft timeout, killed
at the hard timeout, failed to start, ctx cancelled, semaphore refused — and
an exit code above zero when the tool ran and answered "no".
`agent.Reported` (a nil error, or an exit code above zero) is the single
test, and `osBase.runProbe` is the one probe wrapper that applies it: a
probe that did not answer returns an **error**, never a "not there". The
distinction is not cosmetic. A killed command may still have completed in
the kernel — the ioctl finishes regardless of the signal — so the only thing
a caller learns from a kill is that it must probe again; and a caller that
reads a kill as "absent" skips the removal, forgets the object and never
enumerates it again, which is the leak the sweep exists to end (DN6).
`agent.Reported`'s one other caller, `Dm.BlkZeroout`, is not a probe: it
hands the verdict back as `answered`, so DN9's zeroing loop can tell a batch
the soft timeout killed from one the tool refused.

These primitives must honour it, because each one's caller answers an
"absent" with a removal, with a decision that can destroy data, with —
`CloneMeta.Mounted`'s — a mount that sends every clone on the CN into a
rebuild, or — `dirMtime`'s — with a clean verdict:

* `Dm.Info`, whose "absent" answer gates `FreeSide` and `FreeCloneMeta`
  (DN6's record rule);
* `osBase.listDir` and the `dirExists` above it, which
  `Nvmet.RemoveSubsystem` and `RemovePortLink` walk their children through;
* `osBase.dirMtime` beside them, the "stat" behind `Nvmet.SubsysMtime`,
  DN6's orphan-export age: a subsystem directory that reads absent is passed
  over and is no leftover, so a "stat" that did not answer is named as a
  failed enumeration instead, and the verdict is not clean;
* `Md.Detail`, a lookup in an `Md.Walk` of "/sys/block" through `listDir`
  and `readAttrStrict` (`cnagent.md` CN12), whose "absent" sends
  `ensureGroup` into an assembly;
* `Md.HasSuperblock` (`cnagent.md` CN12: `mdadm --examine` opens the member
  device, so on a leg whose DN side is gone it blocks past the soft timeout
  and gets killed — and a killed examine read as "no superblock" would make
  `assembleGroup` run `mdadm --create --assume-clean` over live data);
* `Md.NameInUse` (`cnagent.md` CN12: an `lsblk` of the array's node under
  "/dev/md" whose "not in use" is what lets `assembleGroup` run
  `mdadm --create --assume-clean` beside an array already running under
  that name);
* `CloneMeta.Mounted` and `CloneMeta.FileSize` (`cnagent.md` CN5: the two
  `findmnt` calls of the tmpfs probe, whose "absent" is answered with a
  `mount`, which over the live clone-metadata arena stacks a second, empty
  tmpfs that hides it — a fresh arena file and a second loop device follow,
  and every clone on the CN is rebuilt — and the "stat" of the arena file,
  whose "absent" is answered with a `truncate` of it to
  `CnCloneMetaAreaSize`: a no-op on the live arena file, which the converge
  only ever creates at that size, but a resize of one of any other size —
  on a shrink, the loss of its tail — where the converge otherwise reports
  an error and leaves the file alone).

`CloneMeta.LoopDevices` (`cnagent.md` CN5) honours it without the split:
`losetup --associated` answers "none attached" with exit zero and no output,
so every failure of it is an error — a "none" read off a failure would
attach a second loop device to the arena file. `NvmeHost.readTrimmed`
follows the same rule from the other side of the fence: it reads sysfs
rather than running a tool, so **only** a not-exist error is "absent" and
every other read failure is an error — the nvme class directories under
"/sys/class" can stall while a controller is mid-reset, and a stalled read
taken as absence would report a live subsystem as not connected (SH20). The
read under it, `osBase.readAttrStrict`, carries the rule for every other
strict read besides `Md.Detail`'s above: `Nvmet.NsDevicePath` (a
namespace's "device_path", by which the cn sweep attributes a host-facing
subsystem no stored request claims — one with no attributable namespace is
unowned, and the node-level sweep removes it, `cnagent.md` CN21 — and by
which DN6 attributes a `SideToCnNqn` export: an export whose read did not
answer is foreign for that pass and named as a failed enumeration), the
"enable" reads of `Nvmet.RemoveNamespace` and `RemoveSubsystem` (an
"absent" would skip the disabling write and the rmdir of a namespace the
kernel still has enabled), the attribute reads of `Nvmet.ProbePortState`
(`cnagent.md` CN30: a group whose state read did not answer, read as
absent, would make a `CheckCn` verdict re-send a `SyncupCn` for a port that
is there) and, through `Cmd.ReadAttr`, the md sysfs reads of `Md.ListArrays`
and `Md.Gone` (`cnagent.md` CN12: an array read as absent, or as gone, would
let the sweep disconnect a leg under a live array) and the cn leg walk's
"subsysnqn" read (`cnagent.md` CN10: a subsystem whose read failed, passed
over as another NQN's, would send the connect step after sides whose
controllers are live).

SH16. **Convergence is probe-first.** Every `Ensure*` helper reads current
state and mutates only differences; an equal-revision re-apply on a live,
converged object issues no mutating command. This is what makes re-applies
safe: gratuitous re-writes of live objects are not no-ops (re-linking a
live port-to-subsystem link or reloading a live dm table stalls host IO).

SH17. Probing follows these conventions: `dmsetup status`, `dmsetup table` and `dmsetup ls`, the walk of
"/sys/class/nvme-subsystem" and "/sys/class/nvme" for every nvme host fact
— the namespace device, controller liveness and ANA state alike (SH20,
`cnagent.md` CN12 and CN28) — configfs **reads** for nvmet, and `ReadBlock`
of the disk header for the DN's [D13] metadata. No LVM report is probed
anywhere ([D14]). Probe reads MUST tolerate padded or normalized read-back;
compare canonically, never byte-wise:

* "attr_serial" reads back space-padded — compare trimmed.
* "device_uuid" and "device_nguid" read back **dash-separated and
  lower-cased** whichever of the two accepted forms was written (the kernel
  prints them in its uuid format), while `DnNsIdentity` supplies the uuid
  dashed and the nguid bare. Both roles compare them through the one shared
  `agent.SameNsId` (strip the dashes, fold case). A byte-wise comparison
  reports a permanent difference on the nguid, which makes the converge
  disable the namespace, rewrite the attribute and re-enable it on
  **every** pass — an SH16 violation that drops a live export's namespace
  once per Check round — and makes every probe report the healthy
  namespace `RES_STATUS_ERROR`.

SH18. nvmet configfs access: attribute writes go through
`OsClient.WriteFileDirect` (the atomic replace of `WriteFile` cannot work on
configfs); directory and link operations go through `RunCommand` (`mkdir`,
`rmdir`, `ln -s`, `rm`); probing through `ReadFile`. `WriteFile` MUST NOT be
used on paths under "/sys/kernel/config".

SH19. `nvmet.go` owns the [D4] fixed-ANA-group model:

* `EnsurePort` creates the port directory of the calling agent's
  `--nvmet-port-id` (`NvmetPortId` by default) with that agent's
  `NvmeTrConf` attributes **and** the three fixed groups: it creates the
  group directories that do not exist, then writes "optimized",
  "non-optimized" and "inaccessible" into the three groups exactly once
  (probe-first: skip when already correct). No code path ever writes an
  "ana_state" after that. Probe-first is also what lets two agents on one
  node co-own one port id (`architecture.md`, Disk node): the second one
  finds every attribute already as it wants it and writes nothing. That
  holds only while their `--tr-*` values agree — two agents sharing a port
  id with different transports would each try to rewrite the other's
  address attributes every round — so agents that need different
  transports need different `--nvmet-port-id`s.
* every ANA transition is `SetNsAnaGrpId` — a single `WriteFileDirect` of
  the namespace's "ana_grpid", valid on a live namespace because the
  target group always exists.
* the subsystem and namespace lifecycle helpers follow the build order of
  `architecture.md`, Data-plane device stacks; teardown is reverse order (port link removed first, then the
  namespace disabled and removed, then the allowed-hosts links, then the
  subsystem). Absent objects are skipped so a re-run after a crash is a
  no-op, but the "enable" read that decides whether the disable is needed
  is a **strict** one (SH15): a read that did not answer must not let the
  pass skip the disable and then remove a namespace the kernel still has
  enabled. `RemoveSubsystem` and `RemoveNamespace` both read it that way,
  and both propagate the error instead of continuing.

SH20. `nvmehost.go`: `Connect` always passes `--fast_io_fail_tmo` set to
`DefaultNvmeFastIoFailTmo` and `--ctrl-loss-tmo` set to never give up, the
caller's hostnqn, and `--hostid` set to `common.NvmeHostId` of that hostnqn
— never the node-wide host id file, which the kernel's one-to-one
hostnqn-to-hostid rule turns into an EINVAL the moment anything else on the
node holds it. The
underscores in `--fast_io_fail_tmo` are nvme-cli's own spelling and must
not be "fixed" to dashes. `Disconnect` is `nvme disconnect --nqn`;
`ListSubsys` reads **sysfs** ("/sys/class/nvme-subsystem",
"/sys/class/nvme") for the subsystem's namespace device, controller
liveness and per-path ANA state — `nvme list-subsys` carries none of the
three, listing no namespaces at all and no ANA state without a namespace
block device argument (`cnagent.md` CN12 and CN28, which reads the same
tree for legs). Every read of that walk carries the SH15 soft timeout like
any other OS touch: unlike most of sysfs, the nvme class directories can
stall while a controller is mid-reset or being torn down, which is exactly
when these probes run, and no converge — nor, through the DN1 and CN1
locks, a whole node's RPC surface — may be held on one. The bound is on the
read's own scheduling, not a magic abort of a read already blocked inside
the kernel. `DisconnectDevice` (`cnagent.md`, `NvmeHost.DisconnectDevice`)
is `nvme disconnect --device` for retiring **one** controller of an NQN
whose other paths must live — the two sides of a migrating leg share a
subsystem NQN (`architecture.md`, [D1]), so the cn agent cannot use `--nqn` to drop a dead
side. The controller device is found by the **sysfs walk**, never by
`nvme list-subsys`: the subsystem directory whose "subsysnqn" equals the
NQN holds the controller entries, and one is selected by reading the
controller's "address" and parsing it as comma-separated key-value pairs
(`ParseNvmeAddress`) to match the dead side — never by field position. A
controller whose "address" did not answer is never selected — it is
unknown, never unwanted — and neither is one whose "address" is absent, a
controller already deleted (`cnagent.md` CN10).

### Bitmap-chunk store — `bitmap.go`

SH21. Implements the agent side of `architecture.md`, Bitmap push protocol:
persist the received `Push*BitmapRequest` verbatim at its `Local*BmPath`
(via `WriteProto`) **before** applying; the applied set is derived from the
files present, save those the startup reload leaves unloaded (below) —
`BitmapInfo.bm_idx_list` for a migration, keyed by the append index, and
`BitmapInfo.chunk_id_list` for a clone, keyed by the `src_slice_idx` and
`bm_idx` pair that addresses the chunk. The file name carries that pair too,
but only as an address: the persisted request's CONTENT is what the startup
reload decodes it from. A file that reload leaves unloaded — one that does
not decode, DN2's skips while a `dn-*` or `side-*` file does not load, and
`cnagent.md` CN2's while a `cn-*` or `cntlr-*` file does not — is in no
applied set until a push rewrites it or a later restart loads it.

SH22. The math skeleton of `architecture.md`, raid0 bitmap math, lives
here: chunk placement (concatenated — migration — or self-positioned
`src_slice_idx` and `bm_idx` chunk of fixed capacity `CloneBmChunkBytes` —
clone) and the fully-skippable-region to `blkdiscard` range computation
(`SkipRanges`, `ApplySkipRanges`). The single wire-convention inversion
(**wire 1 = unwritten/skippable**) is *not* here: the one place that reads
the "written/copied = 1" side of the convention is the thin-pool metadata
reader in `agent/cnagent/thinbm.go`, so that is where it inverts, exactly
once, and every chunk reaching this file is already in wire convention.
Role packages supply only the positioning parameters (the dn shifts by the
leg's `meta_blocks` first, `architecture.md`, Bitmap push protocol).

SH23. Migration chunks are interpretable only as a contiguous prefix from
`bm_idx` zero; the apply computation uses the longest contiguous prefix of
the applied set's chunks (`ChunkSet.ContiguousPrefix`, SH21) — the worker's
ascending, one-in-flight push makes gaps unreachable in practice. The
applied set still reports every chunk in it, not only that prefix.

### Check-stream rules

The `Check*` handlers are role code (the stream types differ), but MUST all
follow these rules (`architecture.md`, Check streams):

SH24. Rounds are worker-initiated: loop on the stream's receive (a receive
returning end-of-file, or the stream ctx ending, ends the handler with nil);
exactly one send per received request; never an unsolicited send. Each
round runs under the trace id its request carries in `trace_id`
(`agent.CheckRoundCtx`): the stream ctx holds only the id its metadata
brought at open (`grpc.md` T2) — on a worker's stream, that of the round
that opened it (T3) — so without it every later round's records would carry
that first round's id. An empty `trace_id` keeps the stream ctx's id.

SH25. Per round, under the SH11/SH12 locks: validate the object (unknown ⇒
reply `agent_reply.code` of `ReplyCodeUnknownObject`, `revision` zero, no
info — the stream stays open), probe the **fresh** live state, reply
`revision` = last fully applied revision.

SH26. Info inclusion: always when `show_info` is true; when false, on the
first reply of the stream and whenever the freshly probed `*Info` differs
(a protobuf equality comparison) from the last info actually sent on this
stream. Otherwise the info field is left unset.

## `cmd/dnv-agent` — cobra + viper

CM1. The binary is a cobra root command `dnv-agent` with exactly two
subcommands, `dn` and `cn`. cobra and viper live in `cmd/` and in `ctl/`,
dnvctl's command tree (`dnvctl.md`, Files); both are direct dependencies
(`dependencies.md`, Direct dependencies).

CM2. Flags (`architecture.md`, Components: invocation reference; every flag
is also settable via config file and environment through viper). Both roles
take `--grpc-network` (the listen network) and `--grpc-address` (the gRPC
endpoint, required; the control plane stores it as the `addr_port` of
`DnConf`/`CnConf`); the four transport flags `--tr-type`, `--adr-fam`,
`--tr-addr` and `--tr-svc-id` of this agent's single nvmet port (its
`NvmeTrConf`, required, mirrored into `DnConf`/`CnConf` at creation);
`--nvmet-port-id`, the configfs id of the nvmet port this agent converges
(several agents on one node's kernel take distinct ids, `architecture.md`,
Disk node; a value below one is refused, CM3); `--local-store`, the
`localStorPrefix` of `common.NewNameFmt` (`DefaultLocalStorPrefix` by
default; the directory must exist before the agent starts, SH3); and
`--config`, an optional viper config file. The dn alone takes `--disk`, the
raw block device that carries the dnv disk format ([D13]; `diskmeta.go`),
required. The cn alone takes `--capacity`, the capacity budget in bytes this
CN is willing to host, which `GetCnSize` replies verbatim, zero meaning
"use the control-plane default" (`cnagent.md`, `cmd/dnv-agent cn`).

Running several agents on one node takes more than distinct port ids. Each
needs its own `--grpc-address`; two agents of the **same role** also need
their own `--local-store`, and the dn role needs its own `--disk`. The
`--tr-*` values follow the port id rather than the agent: agents on
**distinct** port ids need distinct `--tr-addr`/`--tr-svc-id` pairs,
because an agent's `NvmeTrConf` is what a CN dials to reach a dn's sides
and what a host dials to reach a cn's namespaces; agents that **co-own**
one port id — the dn/cn pair of `architecture.md`, Components: invocation
reference — must instead pass identical `--tr-*` values, since they
converge the same address attributes (SH19).

The `--local-store` rule is the one the file names mislead about. The names
of `architecture.md`, Agent local-store paths, do carry the owning node's
id — the dn role's kinds key on the cluster and dn ids, the cn role's on
the cluster and cn ids — but the store is never read back by name: SH6
enumerates the prefix and filters on the role's three **kind** prefixes
alone, with no id filter. Two dn agents sharing a prefix would therefore
each load and converge the other's DN and side records at startup (SH1,
DN2): each would allocate the other's side out of its **own** `--disk`,
since the volume table is keyed by `(sp_id, side_id)` alone (DN9), and then
link the other's `SideToCnNqn` subsystem into its **own** port. The prefix,
not the file name, is the unit of ownership. A dn and a cn agent may share
one, because the two roles' kind prefixes are disjoint — which is what lets
the dn/cn pair of `architecture.md`, Components: invocation reference, both
run on the default prefix.

One name a dn agent builds is **not** keyed by `dn_id` and names an object
two agents would fight over: the side subsystem NQN. `SideToCnNqn` is keyed
by `leg_id` (`architecture.md`, NQNs), and nvmet subsystems live beside the
ports rather than under them, so two dn agents in one kernel must never
hold the two sides of one leg. Only a migration ever gives a leg two sides,
so this is a placement matter rather than an agent-flag one: register every
DN of one kernel under the same failure domain (`dnvctl dn create
--location`, `architecture.md`, Disk nodes) and the first-tier
anti-affinity of `architecture.md`, Per-operation allocation, keeps a
migration destination off its source's kernel. `architecture.md`, Disk
node, carries the full rule and its caveat — the second tier relaxes that
exclusion rather than refusing to place. Two further dn-built names carry
no `dn_id` and are harmless: `CnHostNqn`, whose kernel-global hosts
directory both agents merely create with `mkdir -p` and no code path
anywhere removes, and the ns identity `DnNsIdentity`, which lives inside
the subsystem the NQN above already covers.

The cn role has the mirror-image rule, and a stricter one: run at most
**one** cn agent per kernel. Three cn names carry no cn id at all — the
host-facing subsystem NQN is the one the user passed `CreateSubsystem`, so
every cntlr of that SP exports the identical subsystem (`architecture.md`,
Primary cntlr, Host view and cntlid slots); `XferNqn` carries no node id
(`architecture.md`, NQNs); and `CnMdArrayName`, the `mdadm --name`
superblock name, carries neither cluster nor cn id (`architecture.md`, md
names). Nor is the placement rule the dn falls back on enough here:
`architecture.md`, Per-operation allocation, keeps the cntlrs of one SP in
distinct `location`s only at the first tier, so two cn agents of one kernel
registered under one `location` can still take two cntlrs of an SP once a
pick finds no CN with room outside the domains that SP's cntlrs already
hold (which any SP with more cntlrs than the cluster has domains with room
reaches), an automatic cntlr replacement may land on the other cn agent of
the failed cntlr's kernel even at the first tier (`dnv-worker.md` AR7
excludes only the surviving cntlrs' domains), two left at the default
`location` are simply two CNs to the allocator, and where the dn's
collision needs a migration in flight, the cn's needs no more than an SP
with more than one cntlr, or an automatic replacement of any SP's cntlr.

CM3. Viper binding per subcommand (`bindViper`): bind the command's flags,
set the environment prefix with the dash-to-underscore key replacer, enable
automatic environment reads, and when `--config` is set read that config
file. All value reads go through viper (so file and environment win per
viper precedence).

Two checks run **after** that resolution rather than through cobra's own
flag machinery, so that a config file or an environment variable is subject
to the same rule as a flag: `requireValues` over `requiredCommon` (the
values both roles must have, plus `--disk` for dn), and the
`--nvmet-port-id` floor (`nvmetPortIdFromViper`). Below one the
subcommand's `RunE` returns an error naming the floor and the value it got;
the root command sets `SilenceUsage`, so `dnv-agent` exits non-zero with no
usage dump, printing that message **twice** — cobra's own error line plus
`main`'s own print to stderr — which is what anything grepping the output
sees. It is the same rule, not the same validation: a non-numeric **flag**
never reaches the floor, because pflag rejects it at parse time, while
viper's integer read turns any non-numeric env or config value into zero,
which the floor then reports as zero. An **empty** env var is ignored
by viper, so an empty port-id variable leaves the default standing. Every
flag's env spelling is the prefix plus the flag with dashes replaced by
underscores.

CM4. Each subcommand's `RunE`: bind viper, then run CM3's two checks —
`requireValues`, and the `--nvmet-port-id` floor whose resolved value is
carried on to the constructor; a signal-notified ctx for SIGINT and SIGTERM;
construct `common.NewNameFmt` over the local store and the process's
**single** `common.NewLimitedOsClient` (with the default limit); build the
role server (`dnagent.NewDnAgentServer` / `cnagent.NewCnAgentServer`); call
`agent.Serve` with the role's reconcile, a register func that calls
`pb.RegisterDiskNodeAgentServer` (resp.
`pb.RegisterControllerNodeAgentServer`), and the role's SH27 background
waiter — the dn passes its `WaitBackground`, the cn passes nil (its probers
are cancelled, never joined, and its sweep's background `nvme disconnect`
is not joined either — SH27). Transport is plaintext (`grpc.md`, Wiring);
log level stays the default Info (`log.md` R6 — the agent calls nothing).
No flag is marked required through cobra: "required" is checked after the
viper binding, so a config file or env var satisfies it too (CM2, CM3).

## The dn role — package `dnagent`

### Files

`server.go` holds the `DnAgentServer` type, the lock mapping and the RPC
entry points other than the check streams of `check.go`; `diskmeta.go` the
[D13] on-disk format (header, A/B volume-table slots, extent and
clone-metadata allocators); `syncup_dn.go` and `syncup_side.go` the two
converges; `plan.go` the per-side desired-state plan derived from the
request; `migr.go` the source and destination choreography of
`architecture.md`, Migration, and the DN13 connect-retry registry;
`fence.go` the DN12 two-phase fence (window bookkeeping, timer, adoption,
settle); `zeroing.go` the DN9 side-provisioning registry and its
`blkdiscard --zeroout` batches; `push_migr_bm.go` the bitmap pushes;
`sweep.go` DN6 (the node's enumeration, the two scopes' wanted sets, the
claim rules, the layers and the record gate); `check.go` the check streams;
`probe.go` the `DnInfo`/`SideInfo` probing.

### Server type and lock mapping

`DnAgentServer` embeds the generated unimplemented server and holds the name
formatter, the local store, the `DiskMeta`, the `Dm`, `Nvmet` and
`NvmeHost` wrappers — the `OsClient`-backed wrappers, no raw client field —
the `LockSet` (object key = the `LocalSidePath` id tuple), the `--disk`
path, the port conf (`--tr-*` plus `--nvmet-port-id` as one `PortConf`),
the per-object `ResInfo` trackers, the pending-connect retry registry, the
per-side zeroing registry and its zeroing slots (DN9), and the SH27
background-task bookkeeping: the `rootCtx` captured at `Reconcile` plus the
wait group that `WaitBackground` waits on.

DN1. Lock mapping (instantiates SH10 to SH13): `SyncupDn` and the startup
reconcile — node write; `SyncupSide`, `PushMigrBitmap`, `GetSideInfo`, one
`CheckSide` round, one DN8 retry attempt — node read + that side's object
lock (key = `(cluster_id, dn_id, sp_id, side_id)`); `GetDnInfo` and one
`CheckDn` round — node read; `GetDnSize` — no lock.

### Startup reconcile

DN2. Enumerate the store (SH6). For each `dn-*` file: re-run the `SyncupDn`
converge (DN5 and DN6) from the stored request — **unless** its
`extent_size` is zero, which only a build older than DN4's conf gate can
have persisted. Such a file is **loaded** into the in-memory DN set exactly
as read — not skipped like an unreadable one — and refused by `convergeDn`
instead, once per pass: a zero extent size is not a geometry any converge
may run on, so the converge stops before DN5 touches anything (no `lsblk`,
no header read, the port left as found) and reports the conf fault of
`architecture.md`, Common validation, as one Error record (msg "invalid
stored conf", naming the cluster and the dn). That record is the one place
the field is named: the `meta_info` of `RES_STATUS_ERROR` that `convergeDn`
hands back has no reply to travel in at startup, and the next
`GetDnInfo`/`CheckDn` round re-probes the header against the zero and
reports the meta row as a foreign disk on a formatted disk, as an
unformatted disk on a blank one. Refusing there rather than converging is
what keeps the fault legible: DN5's `EnsureFormatted` cannot confirm this
disk against a zero (a formatted one answers "foreign disk", a blank one
"extent_size is 0"), so a converge would bury the conf fault under an
identity error on every side of that DN instead of one record naming the
field. Loading rather than skipping is what keeps the fault harmless: with
no DN state in memory and no `dn-*` file left unread, every `side-*` file
of that node takes the pointer-absent branch below, and its request and its
bitmap chunks are deleted on the spot (SH7) — the agent would forget every
side it hosts, and every bitmap chunk it holds for them, because one field
of its parent is zero. It would not even remove their resources in
exchange: a DN that was never loaded gets no node-level sweep of its own
(DN6), and no other DN's sweep reaches them either — that sweep's
enumeration is filtered to its own dn id, and an export whose namespace
names another node's per-CN linear reads as **foreign** — so the dm
devices, exports and records would simply sit there with nothing on the
node naming them. A conf fault must destroy neither the desired state nor
the resources. So every side still in that DN's stored pointer list is left
**exactly as the restart found it**, neither converged nor swept: its
exports, dm devices, store file and extent record stay, and its stored
`migr-bm-*` chunks are loaded but applied to nothing. A side whose pointer
already left the list still has its **state** dropped as usual (SH7) — the
pointer check runs before the conf check — but the node-level sweep of that
DN is skipped along with everything else of it, so its dm devices and its
records are not removed until the conf is fixed: a leak, which is the side
of the trade a conf fault is allowed to fall on. The record step would free
nothing here even if it ran: it needs a confirmed disk, and a disk is
confirmed only against the identity a DN5 converge has asked for, which
this one stopped before asking (the probe above hands `DiskMeta` no
identity). There is no compatibility path: a cluster whose stored conf
carries zeros is refused loudly everywhere and has to be recreated.

A `dn-*` file that does not load — its read fails or it does not decode — is
skipped, and while one is unread, so is every `side-*` file and `migr-bm-*`
chunk whose DN is not loaded: a file that did not load names no DN (SH6),
so any of those sides may be one its list still names, and a list that
could not be read proves nothing about which sides left it. Skipped is
neither loaded nor deleted: none of them is converged, swept or applied,
and every one of those files stays on disk exactly as the restart found it
— loaded, each side would reach the pointer-absent branch below, which
would delete its request and its chunks for want of a list that could not
be read. Nor is anything hidden: with none of it in memory, the DN and each
of those sides answer their Check rounds `ReplyCodeUnknownObject` (SH25),
and the worker re-sends each `Syncup*` (`dnv-worker.md` RW4). The
`SyncupDn` rewrites the file, and from there each skipped side the list
still names is where a lost `--local-store` leaves one — known by its
pointer alone (DN6) — until its `SyncupSide`, which DN8 admits only after
that `SyncupDn`, converges from the request it carries and rewrites its
file; that reply's `bm_info` acknowledges no chunk, so the worker pushes
each chunk of the migration again (DN15). A skipped side the re-sent list
no longer names is in no list and not loaded, so that DN's node-level sweep
removes its resources by name (DN6); its files wait for a later restart,
whose pass finds its pointer absent and drops them (SH7).

The order over the rest of the store is: every side whose pointer is absent
from its DN's stored `side_pointer_list` is dropped, then each usable DN's
node-level sweep removes what those sides left behind by name, then the
remaining `side-*` files re-run the `SyncupSide` converge (DN8 to DN14)
from the stored request — a converge that finds not-yet-zeroed extents
(re)starts that side's DN9 zeroing goroutine, which is how provisioning
resumes after a restart. Then re-apply every `migr-bm-*` chunk (SH21 to
SH23) — except the orphans, which are **deleted** here: a chunk not skipped
with its DN whose side is not loaded, and a chunk whose `migr_id` no longer
matches that side's stored `migr_dst_conf`. A chunk whose side is not
loaded is no orphan while a `side-*` file does not load and the chunk's
loaded DN still names its side: that file names no side, so it may be this
side's, and the chunk is skipped like the files of a DN that did not load —
neither loaded nor deleted. The chunk's side is then where a lost
`--local-store` leaves one, as above, until its `SyncupSide` rewrites its
file and the worker pushes the chunk again. A chunk whose loaded DN no
longer names its side is an orphan whatever `side-*` file failed to decode:
the side has left the list, and its chunks go with it (SH7). Every other
deletion — with the side that owns them (DN6, SH7), on a destination's
`migr_id` change (DN13), and in the side-level sweep of a converge (DN6's
Scope 2) once the side's request carries no `migr_dst_conf` of the chunks'
`migr_id` (the finish of `architecture.md`, Migration; a level that only
suppresses the destination role keeps them, DN11) — names the files from
the side's in-memory chunk set, keyed by the one `migr_id` that set is
tracking, so a file this process never loaded is invisible to all of them
and a restart is the only place that can collect it. The reconcile holds
the node write lock throughout, with the SH2 trace id. Outside it, a side's
drop runs under `SyncupDn`'s node write lock, and DN13's deletions and the
sweep's inside a converge of the side — a `SyncupSide`'s, a DN8 retry
attempt's or the DN12 fence timer's — under the node read lock and the
side's object lock (DN1).

### `GetDnSize`

DN3. Read the byte size of the `--disk` device with `lsblk`; reply the
parsed size **minus `DnDataOffset`** — the byte size of the [D13] data
area, which is what the control plane divides by `extent_size`
(`architecture.md`, Size → extents). A size at or below `DnDataOffset` is a
"disk too small" error. Failures (bad disk, parse, too small) return a gRPC
Internal status (`architecture.md`, Common agent rules — this RPC has no
`AgentReply`). `dn_id` may be zero (pre-registration call from the
gateway); it is for logging only. No locks, no store access.

### `SyncupDn`

DN4. Gate the revision (SH8) against the stored `SyncupDnRequest`. Then,
still with **zero** side effects, the **conf gate** of `architecture.md`,
Common validation: `agent.ValidateExtentSize` (Files) refuses a zero
`extent_size` with `ReplyCodeInvalidConf` and the message "invalid stored
conf: dn_bin_conf.extent_size is zero", echoing the **stored** revision so
the worker sees the request was not accepted, and writing one Error record
(msg "invalid stored conf") naming the cluster and the dn. `extent_size` is
what this disk's [D13] header is formatted with and what every side's runs
are carved out of, so a value this node substituted would be one the rest
of the cluster does not share; the control plane resolves it when it
*writes* the `ClusterConf`, and a zero arriving here is refused rather than
replaced. Placement is load-bearing: the gate sits **before** the request
becomes the desired state, so nothing is converged, no dm device is
removed, no volume-table block is written and no local-store file is
touched. After the assignment it would instead persist the zero and let the
next startup reconcile converge it. (DN5's header-identity guard is not
this gate: it refuses a disk formatted for another cluster, dn or extent
size, which is a disk fact, not a conf fact.)

DN5. Converge the once-per-DN base state of `architecture.md`, Disk node,
probe-first (SH16), building `DnInfo` as it goes:

* read the `--disk` device's size with `lsblk` — both to report `disk_info`
  and to hand the allocator the raw size, the one input the disk format
  itself does not carry (the extent count is that size minus
  `DnDataOffset`, divided by the **header's** `extent_size`).
* **the [D13] disk format.** Read the header block. Magic absent ⇒ the disk
  is blank, and it is formatted only when no side device or clone-metadata
  wrapper maps it — the only devices dnv builds on the disk itself: a
  `dmsetup ls`, then a `dmsetup table` of every `DnSideName` and
  `DnMigrMetaDmName` on the node, whatever cluster or dn the name carries,
  compared against the disk's device number. A header can go blank under
  live side devices (one mistaken write over its header block is enough),
  and a fresh, empty table would hand their extents to the next side, which
  zeroes them and serves them as its own. So while one of them maps the
  disk, or while any of those reads does not answer, the converge writes
  nothing to the disk and `meta_info` is `RES_STATUS_ERROR` with details
  "blank disk header; refusing to format" and the cause. This dn's DN6
  sweep removes its own such devices once nothing wants them. A device
  named for another cluster or dn is not its to remove: while one maps the
  disk, the disk stays blank, `meta_info` names the first device that maps
  it and every round's verdict replies `ReplyCodeLeftover` (DN6's record
  step), until an operator removes it. Once no side device or
  clone-metadata wrapper maps the disk, the next converge whose reads all
  answer formats. The format writes slot A with an empty table at the first
  sequence number **first**, then the header (a fresh `format_uuid` from
  the kernel's random source, the request's `cluster_id`, `dn_id` and
  `extent_size`, and the three layout offsets) — slot-A-before-header makes
  "a valid header implies at least one valid table slot" an invariant, so a
  crash between the two writes leaves an inert slot rather than a header
  with no table. Magic present but version or CRC wrong ⇒ **error**, and
  every later operation errors too — a corrupt header is never formatted
  over. Magic present and valid ⇒ verify `cluster_id`, `dn_id` and
  `extent_size` match; a mismatch is a "foreign disk" error naming both
  identities, and the disk is **never** overwritten. `extent_size` is
  therefore immutable for the life of a format. A re-call on an
  already-converged disk issues **zero** writes (SH16).

  The identity check is what makes the volume table this node's to build on
  and to change: handing out a record it already holds (`AllocSide`,
  `AllocCloneMeta` — an existing record's extents are this node's only if
  its table is) and every mutation (allocate, free, the DN9 zeroed-bit
  updates, the DN6 record step) refuse on a disk whose identity this node
  has not confirmed, and so does DN9's re-read of the record before each
  zeroing batch (`LookupConfirmedSide`), so no batch is computed from
  another node's bits. Confirming needs no read of its own: this converge
  hands `DiskMeta` the identity it asks for **before** it reads anything,
  and each of those operations compares the header the table in memory was
  loaded under with it. So one header read that did not answer — this
  converge's at startup, say — leaves the disk unconfirmed only until a
  later read of it (a side converge's, a DN18 probe's) has answered, and a
  disk whose header names another cluster, dn or extent size is refused
  whether or not a check has answered. While no read has answered, the DN6
  record step says so and the verdict is not clean, which re-drives this
  converge. Lookups that only report do not pass the check — the side
  converge's first look at the record, which fills the counters, and the
  DN18 side probe (`LookupSide`) — so on another node's disk a record whose
  ids collide still fills `zeroed_ext_cnt`/`total_ext_cnt`, in a side
  converge's reply beside its foreign-disk refusal and in every probe of
  the side. The guard has to live at that layer rather than in the caller,
  because a failed DN converge does not stop the side converges that follow
  (DN19) — without it, a node pointed at another node's disk would report
  `meta_info` as `RES_STATUS_ERROR` and then allocate extents in that
  disk's volume table, or build its devices over the records that table
  holds, anyway.
* `EnsurePort` (SH19: the agent's `--nvmet-port-id` port, default
  `NvmetPortId`, from the `--tr-*` flags + the three fixed ANA groups).
* **the Write Zeroes fail-fast.** DN9 zeroes whole sides with
  `blkdiscard --zeroout` under the ordinary SH15 timeouts, which only holds
  on hardware whose Write Zeroes is offloaded; a kernel that has to emulate
  it writes zero pages at bulk speed, so the fast-Write-Zeroes assumption
  of `architecture.md`, Side provisioning protocol, cannot hold:
  `DnZeroBatchExtCnt` batches would overrun the soft timeout and the side
  would crawl in DN9's backed-off batches, if it converged at all. Resolve
  the disk's kernel name with `lsblk` (the flag is documented as a by-uuid
  symlink, whose basename is not a sysfs node) and read the block queue's
  "write_zeroes_max_bytes" attribute under "/sys/class/block"
  (`WriteZeroesMaxBytes`). A **present zero** is the verdict: `meta_info`
  of `RES_STATUS_ERROR` with details "disk lacks Write Zeroes", which flows
  into the worker's `err_epoch` and the capacity-key removal
  (`architecture.md`, Live-state reporting and dn / cn roles) and takes
  the unsuitable DN out of allocation. An absent or unreadable attribute is
  **not** a verdict — the attribute read reports both as *not present*,
  silently, and the converge continues (only a failed `lsblk`, an empty
  kernel name or an unparsable value is logged, as a warning, and the
  converge continues just the same), because failing every kernel that
  simply does not publish the attribute would remove healthy DNs for a
  reason nothing measured. Unlike the identity check this is a **health**
  signal, never a write gate (DN19): a DN already carrying sides must keep
  serving them.

DN6. **Removal is a sweep of actual minus desired, never a memory.**
`side_pointer_list` is authoritative (full sync, `architecture.md`, Common
agent rules). A pointer in the request without local state needs nothing
yet — resources come with its first `SyncupSide`; the persisted request is
what makes the pointer *known*. A side this process holds whose pointer has
**left** the list is dropped on the spot (SH7): its DN8 connect-retry
registration goes, its DN9 zeroing goroutine is cancelled **and waited
for**, its fence is cleared, and its `side-*` and `migr-bm-*` files, memory
entry and object lock are deleted. Nothing of it is removed from the node
at that point. The files deleted are the ones its memory entry names — the
request's own path and its chunk set's files — so a chunk DN2's reload
skipped and no push has rewritten stays, as does every file of a side DN2
skipped that no `SyncupSide` has rebuilt, which the process does not hold
at all; they wait for a later restart (SH7).

What to remove is derived afterwards from the node itself: enumerate what
exists — `dmsetup ls`, the configfs subsystem listing, the walk of
"/sys/class/nvme-subsystem" — subtract what the desired state wants, remove
the rest top-down, and verify every removal with a probe.
`architecture.md`, Teardown by sweep, states the principle both roles share
— including why nothing about a failed removal may be remembered — and
what follows is the dn's instance of it. Every dnv name carries its own
ids, so a side the agent has already forgotten is still found by name —
which is what makes dropping the state first safe, and what a teardown
that deletes the state after a best-effort removal pass cannot do: when
every step of that pass only logs its failure, nothing ever enumerates the
object again.

**Scope 1, node-level.** Under the node write lock, in `SyncupDn` after the
DN converge and in the startup reconcile after every DN has converged. Its
wanted set is empty for every side that is not in any authoritative list: a
`DnErrorName`/`DnLinearName`/`DnSideName` device whose `(sp_id, side_id)`
appears in no synced DN's `side_pointer_list` **and** in no side whose
stored request this process holds (a side whose file DN2's reload did not
load is on disk but held by none until its `SyncupSide`) goes, together
with the migration objects no held side claims (once every side of their sp
this node may host is held, below) and the exports this agent can attribute
to itself that name no side it must keep — a `SideToCnNqn` export carries
no dn id, so it is attributed before it is judged (below), or the sweep
would take a sibling agent's. It never touches a side that **is** in a
pointer list, even when that side's file is absent (DN8): a node that lost
`--local-store` but kept its disk must rebuild those sides from their
records, and sweeping them would free the extents and send the next
`SyncupSide` through the provisioning protocol again, zeroing live data.
Nor, while such a side is not held — its file absent or unreadable — does
it judge a migration object of that side's sp: the migration objects follow
the known-with-state rule of the `CloneMetaRecord` (the record rule,
below). Each names `(sp_id, migr_id)` and no side, and the claim rule reads
held sides' requests only, so while any side of its sp is known by its
pointer alone, "no held side claims it" proves nothing — that side may be
the one playing the migration. Judging them anyway, in the `SyncupDn` that
brings a lost store's pointer list back, would take a source's
`DnMigrSrcName` and its `MigrSrcNqn` export out from under the
destination's dm-clone, whose reads of every region not yet hydrated then
fail on the leg the host is using. Such an object is neither removed nor
named as a leftover — naming it would only have the worker re-send a
`SyncupDn` that must not remove it — and the wait ends by itself: the
side's Check round replies `ReplyCodeUnknownObject` until its `SyncupSide`
stores the request, and that rejection is what brings the `SyncupSide`
(`dnv-worker.md` RW4); once every side of the sp this node may host is
held, the next pass judges the object by the claim rule again. A
destination's source connection waits with its dm-clone, not only behind
L3's stop rule: with the clone out of the chain, L3 would find no clone of
its own to fail on and disconnect the source under the live one. It also
takes the namespace-less exports nobody can attribute, once they are older
than `DnExportOrphanGrace` (below). When no DN has been synced or reloaded
at all nothing is authoritative and the sweep does nothing.

**Scope 2, side-level.** Inside `convergeSide` and ahead of its build phase
(`SyncupSide`), under whatever DN1 locks that caller holds (SH10/SH11). Its
wanted set is exactly what the build phase would ensure for this side's
plan, with the source deferral of `architecture.md`, Migration, already
applied: `DnSideName` at every level (DN11), the per-CN `DnErrorName` and
`DnLinearName` while `sp_level` keeps the dm layer, the `SideToCnNqn`
exports while it keeps the export layer, `DnMigrSrcName` and its
`MigrSrcNqn` export while an **effective** `migr_src_conf` is set — each
still under the level gate above it (DN12) — and `DnMigrFinalName`,
`DnMigrMetaDmName` and the source connection while a destination role is
wanted (DN13). Everything of that side which the node holds and that set
does not name is removed. Of the `SideToCnNqn` exports it judges only its
**own leg's**: the NQN names the leg, so another leg's export is skipped by
name, before anything of it is read. It is another side's business — that
side's own `SyncupSide`, or Scope 1 once the side has left the list — and
this pass holds only the node read lock, beside every other side's converge
on this kernel, a sibling agent's included: a side-level sweep that judged
the whole sp would strip a sibling's half-built export of another leg of
its host link and namespace.

The node write lock excludes every side-level sweep while the node-level
one runs, so the two scopes never race. Those locks are per process: they
order nothing between the sibling dn agents of one kernel, whose builds can
be in flight while this agent sweeps (below). Two side-level sweeps of
different sides of one sp can both see the same unclaimed migration object,
because a migration belongs to no side (below); a second removal is
harmless — its probe simply finds the device already gone, which is the
same answer it gives for any object that vanished between the enumeration
and the removal.

A side-level sweep judges the migration objects of its sp only under Scope
1's condition: once every side of the sp this node may host is held. Short
of it, the sp's migration devices, `MigrSrcNqn` exports and source
connections are left out of its chain, neither removed nor named. A DN may
host sides of two groups of one sp (`architecture.md`, Per-operation
allocation), and after a lost store the first `SyncupSide` may be that of
the side not playing the migration. Judged by the claim rule alone, its
pass would remove a live source's `DnMigrSrcName` and `MigrSrcNqn` export
with a clean reply, and on a destination try to remove the dm-clone — EBUSY
under the per-CN linears — and name it, its wrapper and its source
connection as leftovers, re-driving that side every round until the
migrating side's own `SyncupSide` stored its request. The condition is read
before the claims: this scope holds only the node read lock, so another
side's `SyncupSide` can store its request meanwhile, and read after the
claims the condition could hold on claims that miss it.

**Objects whose name carries no side.** A migration device
(`DnMigrSrcName`, `DnMigrFinalName`, `DnMigrMetaDmName`) is keyed by
`(sp_id, migr_id)`, a `SideToCnNqn` export by the cluster, sp, leg and cn
ids, and the `MigrSrcNqn` a destination connects to carries the **source**
DN's id — none of them names the side that built it. Two questions have to
be answered about such an object, and in this order: *is it mine*, and
*does anything still want it*. The first is not the same as the second, and
skipping it is how a sweep takes a sibling agent's live object
(`architecture.md`, Teardown by sweep).

For the three migration dm devices the first question is already answered
by the enumeration — their names carry this cluster and this dn, as the
`MigrSrcNqn` export's own name does — so for those the claim rule below is
the whole test, taken once every side of their sp this node may host is
held (both scopes, above). The side export carries no dn id at all and the
source connection carries only the **source** DN's, so neither names the
agent holding it; both are visible to every agent sharing the kernel, so
each needs attributing first. A side export is attributed by the per-CN
dm-linear its namespace backs: one naming another dn agent's per-CN linear
(`DmKindDnLinear`) is **foreign** and is never touched; one naming ours
belongs to the side in that name, and survives for as long as that side is
in an authoritative list, which is what keeps a side that must be rebuilt
from its record exporting (DN8) even though no held side claims it. The
namespace is read by its id — namespace one's "device_path"; every side
export has that one namespace and no other (`SyncupSide`) — and the
namespaces are listed only when there is no namespace one; one whose
namespace one names no per-CN linear of this cluster is foreign, whatever
else it holds. The cost is the reason. Scope 1 attributes, on every pass —
each Check round's verdict among them — every export of an sp this agent
holds a device or a known side of that no held side of its own claims,
which includes every sibling's export of that sp. Listing each one's
namespaces would cost an `ls` apiece, so the pass of every agent holding
sides of an sp would grow with all the other agents' exports of it,
quadratically in the agents of one sp on a kernel. The read runs in
process, so attributing an export that has its namespace one costs a pass
no command at all. No attribution is kept from one pass to the next, and
none needs to be: one pass attributes an export at most once, and a Check
round's verdict and the `SyncupDn` after it each attribute it afresh — an
owner remembered between them could be stale, the export removed and built
again by another agent in between. An export holding no namespace at all is
attributed by the nvmet **port** it is linked to, each agent converging
exactly one port id: linked to a sibling's port, it is that sibling's. One
linked to no port, or only to ours, exports nothing and holds nothing open
— but it is not therefore nobody's. It is also the shape of **every**
export between its subsystem `mkdir` and its namespace `mkdir` — an export
is built subsystem first, with its attributes and then its allowed hosts,
then its namespace, then its port link — and from the build's host-link
step on, that half-built export already carries the CN's host link. On a
kernel several dn agents share, the build in that window may be a
sibling's, whose request no claim of ours can show. Removing it there takes
the sibling's host link — and any namespace it adds meanwhile — from under
its build, and a broken dn export is rebuilt only by some later converge of
its side, which the breakage itself never triggers: a leg is lost that way.
Only age tells an abandoned half-built export from one in flight, so such
an export is removed only once its configfs subsystem directory is older
than `DnExportOrphanGrace` — unless a held side still claims it, which is
the claim rule again. A younger one is left alone that pass: not removed,
not a leftover, not a failure, so the reply code is unaffected; each later
pass judges it again, until it either has a namespace whose "device_path"
names its owner's linear, or the age of an export nobody is building. (A
build abandoned between its namespace `mkdir` and its "device_path" write
leaves a namespace that names nobody; what becomes of it is a known limit,
Known limits.) The age is read from the node, never remembered: the
directory's mtime (`Nvmet.SubsysMtime`, a "stat") against the agent's
clock. A "stat" that fails makes the export foreign for that pass, as every
other failed read of the attribution does: one that did not answer is
named as a failed enumeration, so the verdict is not clean, and one that
found the directory gone is simply passed over. The mtime is set at
`mkdir`, and adding an allowed-host link or a namespace directory under it
does not move it; but on the lab kernel every lookup of one of the
subsystem's own "attr_*" files does — a read, a write, even a "stat" of
one — because configfs instantiates an attribute's inode on each lookup
and, in that kernel, stamps the parent directory when it does. So the age
reads "since the subsystem was created or an attribute of it was last
touched". A first build is young from its `mkdir` on, and its attribute
writes only keep it so; on that kernel the stamp also protects a rebuild —
an owner rebuilding, more than the grace after its first `mkdir`, an export
whose first build stopped before its namespace makes it young again with
the rebuild's attribute reads. A kernel that stamps the directory only when
a directory or a link is created under it reads the plain time since the
`mkdir`, and there such a rebuild reads old. Nothing a sweep does looks up
one of the subsystem's own "attr_*" files (it reads namespace one's
"device_path", which sits in the namespace's directory and not the
subsystem's, and which a namespace-less export does not have; it lists the
namespaces and the port links and stats the directory), so an abandoned
export ages, and the first sweep that finds it older than the grace settles
it: a Syncup's removes it, and a read-only Check round's reports it as a
leftover, which brings that Syncup. Anything else that keeps looking up
those files keeps it young for as long as it does. A source connection is
attributed by its controller's "hostnqn" under "/sys/class/nvme": the nvme
host namespace is per kernel, and since the NQN names the **source** DN,
the host NQN it was opened with is the only field that names the agent
holding it (`NvmeHost.HeldWithHostNqn`).

What that leaves is judged by the **claim rule**: the object goes unless
some side this agent holds state for still wants it, read from that side's
stored request — its **effective** `migr_src_conf` (a source whose
destination has not provisioned claims nothing, `architecture.md`,
Migration), its `migr_dst_conf` under a wanted destination role, its export
under a wanted export layer.

**A claim carries its claimant's gate, and the source needs two of them.**
A claim is not "this object exists"; it is "somebody still WANTS it", so it
is recorded only under the same condition that keeps the object in the
wanted set. The migration source is the one role whose two objects part
company: the `DnMigrSrcName` linear is wanted under the dm layer, its
`MigrSrcNqn` export under the export layer, and `SP_LEVEL_NO_SIDE` sits
exactly between them. A single ungated claim would make both permanently
unsweepable — the wanted set drops them correctly, but every sweep skips
what the claim map holds, so they would be neither removed NOR reported,
the reply would stay OK, and an sp taken to `SP_LEVEL_NO_SIDE` would go on
exporting its migration source until `migr_src_conf` itself was dropped.
Being skipped rather than named is what makes this class silent: a
leftover at least re-drives. A side's request is stored before its converge
builds anything, so no claim of a role this process started can be
invisible to the rule; one a lost store took stays invisible until the
side's `SyncupSide` stores its request again, which both scopes wait out
(above). And the claim is recomputed from the requests every pass, never
read from what a converge left behind. That is what lets a **finished**
migration's objects go in the same pass that drops its conf (unless that
wait holds them), with no "applied destination" to remember — a field
holding one would be overwritten by the very converge that is supposed to
retry the removal.

**The layers.** Each scope removes its unwanted objects in one order,
top-down. Every position is a dependency, not a preference.

*Before the first layer*, every suspended per-CN dm-linear the pass is
about to remove, or whose export it is about to remove — wanted or not — is
put on its dm-error and resumed: DN12's phase 2, brought forward. L1
disables the nvmet namespace above it, and that write first waits for every
request in flight on the namespace; one whose bio a suspended dm target
holds never completes, since such a target queues bios with no timeout and
no error path ([D12]), and the agent cannot know that none is held. The
cutover of `architecture.md`, Migration, leaves exactly such devices behind
— holding the old primary's in-flight IO is what its window is for — and
not only under a side torn down inside the grace window: `SP_LEVEL_NO_SIDE`
keeps the linears and takes only the exports off them. The linear under an
export is the one its attribution read off the namespace (above). A bare
resume would free the device too, but against the table it was suspended
with — the primary CN's maps the side's data — so what the window absorbed
would be replayed onto it; against the dm-error it fails instead. The one
exception is a request that also ends the source role: its pre-step has
already made that bare resume, which is that role ending's own rule (DN12),
so this step retires only a linear the resume left suspended. A linear this
step cannot prove out of suspension — a probe or its reload failed — stops
the descent before L1, as a layer that left something behind does: no layer
runs, every unwanted object of the scope is named as a leftover, and the
next pass tries again.

L1. The `SideToCnNqn` exports and the `MigrSrcNqn` export: an enabled nvmet
namespace holds its backing device open, and removing the subsystem is what
disables it — nothing below can go while an export still names it.

L2. The per-CN `DnLinearName`: every device below is one of its table
targets, and `dmsetup remove` on a device another live dm device still maps
fails EBUSY.

L3. `DnMigrFinalName`, then the source connection: the connection is the
dm-clone's source device, and pulling it out from under a live clone
strands the clone's in-flight hydration IO.

L4. `DnMigrSrcName`: it maps the side device, so it goes before L7; nothing
local maps it once its own export is gone.

L5. The per-CN `DnErrorName`: nothing maps a dm-error once the linear that
targeted it is gone.

L6. `DnMigrMetaDmName`, then its `CloneMetaRecord`: it is the dm-clone's
metadata device and cannot go before the clone.

L7. `DnSideName`, then its `SideRecord`: everything above maps it.

Getting this backwards does not merely log an error: a clone that survives
keeps its metadata wrapper and the side device under it alive too, and no
later empty side list can remove them either — the side leaks until the
node is scrubbed by hand.

**Finish the layer, never descend below one that left something.** Every
removal of a layer is attempted; if anything of that layer is still there
afterwards, the layers below are **reported** as leftovers and not touched.
Continuing would disconnect a live clone's source, or attempt a device
something still maps; stopping at the first failure inside a layer would
lose the rest of its names from the report. Nothing is lost by waiting: the
pass is re-run on the next round, by which time the failfast window has
passed and no command blocks.

**"Gone" is probe-verified.** Every removal is followed by a re-probe —
`dmsetup info` for a dm device, the configfs directory for a subsystem, the
sysfs walk for a connection — and that probe, never the removal command's
exit status, is the evidence. A killed command may have completed in the
kernel, and a killed probe proves nothing at all (SH15). For a connection
the question that walk asks is whether any **controller** is left, not
whether the subsystem is: the kernel keeps the subsystem entry under
"/sys/class/nvme-subsystem" after the last controller of an NQN is deleted,
and an entry with no controller holds nothing open, so waiting for the
directory itself would report a leftover that never goes.

**The record rule.** An allocation record is released **only** after the
sweep has verified its device is gone **and** the authoritative pointer
lists prove its owner is gone. Both halves are load-bearing, and the second
one is where "provably" has to be taken literally, because the volume table
— not the local store — is authoritative for extent placement ([D13]). A
`SideRecord` is an orphan only when its `(sp_id, side_id)` appears in no
synced DN's `side_pointer_list` and in no side whose stored request this
process holds (a side whose file DN2's reload did not load is held by none
until its `SyncupSide`). Missing local state is *not* proof: a node that
lost `--local-store` but kept its disk still has every side in its DN's
pointer list, and freeing those extents makes the next `SyncupSide` either
re-allocate them and zero live data away (`provisioned` false) or, at
`provisioned` true, refuse to allocate and report the side permanently dead
("record missing", DN9) — both outcomes lose the data. A `CloneMetaRecord`
is an orphan only when no held side claims its `(sp_id, migr_id)` **and**
every side of that `sp_id` this node may host is one whose local state the
agent actually holds — otherwise a side it has not heard from yet could
still own the slot, and freeing it would strand an in-flight migration
whose hydration is supposed to resume from disk (`architecture.md`,
Migration). Freeing a record while its device still maps those extents is
not a leak but a corruption path: the next allocation hands the same
extents to another side, which zeroes them and serves them as its own. In
this agent that is where reading a killed `dmsetup info` as "the device is
gone" destroys data rather than leaking a device, and it is why SH15's rule
is a rule.

**The record step.** The same proof, run over the volume table itself
rather than over the devices the enumeration found, closes the crash window
between a removal and its table update (`sweepOrphanRecords`). It runs on
every node-level pass — after the pointer drop in `SyncupDn`, and after the
DNs converge in the startup reconcile. It removes **no** device: the layers
do that, in the order the kernel needs and stopping the descent where
something above will not go, and a removal issued from here would jump that
order. What is left for this step is the record, and the only record it
frees is one whose owner is provably gone **and** whose device it has
probed and found already gone. A record whose device is still there — or
whose `dmsetup info` did not answer, which is not an absence (SH15) — is
reported as a leftover and stays allocated until a later pass finds the
device gone; freeing extents a live device still maps is the corruption
path above, not a leak. Cancelling the zeroing goroutine is not this step's
business either: the node-level pass cancels and waits for the goroutine of
every side device it is **about to remove**, ahead of the layers, because a
live `blkdiscard --zeroout` child holds `DnSideName` open and
`dmsetup remove` on an open device fails EBUSY (`architecture.md`, Side
provisioning protocol). The whole step refuses on a disk whose identity
this node has not confirmed, as every mutation of the table does (DN5), and
it says so: the table it cannot use counts as a listing that did not answer
("volume table: disk identity is not confirmed for this node"), so the
verdict is not clean and the worker re-drives the `SyncupDn`, whose DN5
converge checks the disk again — every round, for as long as the disk stays
unreadable, corrupt, another node's, or blank under a live side device or
clone-metadata wrapper (DN16).

Ids are never reused, so a swept side never comes back.

**Finishing a migration is not a teardown of the side.** When a
`SyncupSide` drops `migr_dst_conf` while the side keeps exporting (the
destination finish of `architecture.md`, Migration), the per-CN dm-linears
are **wanted** — they are not in the chain at all — while the dm-clone they
still map is not. The sweep therefore repoints before it removes: a wanted
per-CN linear whose **live table** maps a device this pass is about to
remove is reloaded onto the backing the plan wants, first, so that L3 does
not find the clone pinned under it and lose the whole chain to the stop
rule for a round (DN13). The live table is read, not remembered — the
reload's target is `linearBacking` with no live clone, which is what the
build phase would give it anyway. A suspended wanted linear is left alone.
On a migration source that is the fence's suspension, deliberate, and
ending it is the fence's own job (DN12); on any other side DN12 rule 2's
resume, which the same pass runs just before, has tried to end it (a failed
reload can leave a device suspended, OS wrappers), and one still suspended
— its probe or its resume failed — is the build phase's to reload or
resume.

**The verdict.** Whatever a pass could not remove, plus any enumeration
that did not answer, is what the reply's `agent_reply` carries (DN19). It
is one comparison's result, recomputed every round and stored nowhere.

DN7. The request was persisted before the converge (SH5) and the node-level
sweep has run (DN6); reply `agent_reply` — the sweep's verdict (DN19) —
`revision` (= the stored revision after this call) and `dn_info`.

### `SyncupSide`

DN8 to DN14 below; the converge order is build bottom-up (side device, then
dm, then nvmet), tear down top-down, probe-first throughout (SH16).

DN8. **Gating.** The pointer MUST be present in the stored
`SyncupDnRequest.side_pointer_list` — else `ReplyCodeUnknownObject`
(`SyncupDn` introduces pointers first, `architecture.md`, `service
DiskNodeAgent`). Then the SH8 revision gate against the `SyncupSideRequest`
this process holds — zero while it holds none, as for a side whose file
DN2's reload did not load, whatever revision that file carries.

After a lost `--local-store`, the first `SyncupSide` of each listed side
meets no stored request: there is no revision to gate it against, and its
converge adopts, probe-first, what the node still holds for it — all but a
migration source's fence: with no request to probe its linears by, the
restart adopted no window, so the source's first converge that builds its
per-CN stacks opens a whole new one, over the linears still suspended on
their pre-fence tables when the store was lost inside the window and over
their dm-errors when after it (Known limits). The `SyncupDn` that brought
the list back left the side's devices and the exports attributed to it in
place (DN6's Scope 1), and the migration objects of its sp as well: both of
DN6's scopes judge a migration object only once every side of its sp this
node may host is held, so neither that pass nor the first `SyncupSide` of
another listed side of the same sp takes a migration source's
`DnMigrSrcName` and `MigrSrcNqn` export, or a destination's dm-clone,
wrapper and source connection, from under a live migration before the
migrating side's own request is back.

DN9. **Side device and the side provisioning protocol** (`architecture.md`,
Side provisioning protocol). Look up `(sp_id, side_id)` in the volume
table. An existing record whose extent total disagrees with
`side_conf.ext_cnt` is an error (resize is out of scope).

**Allocation is permitted only while `side_conf.provisioned` is false.** At
false with no record: allocate `side_conf.ext_cnt` extents — first fit one
contiguous run, else free runs largest-first — and persist the record with
`zeroed_bits` all zero (an unset field: proto3 omits an empty `bytes`, and
an absent bit reads as zero). An allocation failure is reported as it is,
never swallowed: the three converge outcomes below must stay
distinguishable. At true with no record the agent **never** allocates: the
data is gone (a lost or foreign disk), and silently re-allocating would
present a zeroed impostor as the data-bearing leg. That is the hard
resource error `side_dev_info` of `RES_STATUS_ERROR` with details "record
missing", which feeds `err_epoch` and the replacement flows (the
spare-switch of `architecture.md`, Automatic reactions, for raid1 —
automatic after the leg-unhealthy threshold; effectively delete-SP for
`RedundNone`).

Build `DnSideName` as a multi-target dm-linear concatenating the record's
runs, each run mapping to the disk at `DnDataOffset` plus the run's start
times `extent_size`, for the run's count times `extent_size` bytes (both in
sectors for the table).

Then the protocol — **whole-side zeroing behind a `provisioned` gate**
([D15]: a discard is not a zero guarantee — the kernel does not promise
that a discarded region reads as zeros, and NVMe read-zeroes after
deallocate is optional — so a trim funds neither dnv's multi-tenant "no
tenant ever reads another tenant's bytes" requirement nor the places the
design assumes zeros: a recycled extent can hold a previous SP's valid
thin-pool superblock, or a stale md superblock that flips `cnagent.md` CN12
into the wrong assembly case):

1. the extent runs are allocated and the record persisted with
   `zeroed_bits` all zero (above);
2. `DnSideName` is built (above);
3. a **background zeroing goroutine** (the registry below) zeroes the
   not-yet-zeroed extents in batches of at most `DnZeroBatchExtCnt`,
   **through the dm-linear** — the side is contiguous in that device's
   address space, so one command covers a whole batch whatever the
   physical fragmentation. Each batch starts at the **first extent whose
   bit is still unset** and covers the run of unset bits from there,
   capped at the batch size (a first-unset walk, never a count of set
   bits: the bitmap is deliberately more general than a watermark): one
   `blkdiscard --zeroout` at the batch's byte offset and length on the
   `DnSideName` path (`Dm.BlkZeroout`). After each successful batch that
   batch's bits are persisted in the volume table (`SetSideZeroed`, a
   half-open range setter);
4. the per-CN export stacks (DN10) and the migration roles (DN12, DN13)
   converge **only** when the request says `provisioned` true **and**
   every bit is set. The agent always trusts its own bits over the flag:
   the disk is authoritative ([D13]); the etcd flag is a gate, never
   evidence.

**Logical extent *i*** is the *i*-th extent of the concatenation of the
record's `run_list`, i.e. the *i*-th `extent_size` bytes of the
`DnSideName` device; `zeroed_bits` is LSB-first within each byte
(`agent/bitmap.go`) with trailing pad bits zero, and every count is taken
over the record's own extent total — never over the bitmap's byte length
times eight, which would round a side up to the next byte and declare it
done early. **Zeroed is a property of the side's allocation, not of the
disk extent**: extents freed and reallocated to a new side start
all-not-zeroed again, whatever happened to them before, because `AllocSide`
is the only constructor of a record and `FreeSide` deletes records whole.

`provisioned` is monotone — the worker only ever flips it false to true
(the flip rule of `architecture.md`, Live-state reporting) and bits are
only ever set — so the gate never tears an already-exporting stack down.

**Converge matrix** (`side_conf.provisioned` × local state):

| `provisioned` | record | bits | behavior | `side_dev_info` |
|---|---|---|---|---|
| false | absent | — | allocate (bits zero), build the linear, ensure the goroutine | `RES_STATUS_PROVISIONING`, "zeroing 0/n" |
| false | present | partial | ensure the linear + the goroutine | `RES_STATUS_PROVISIONING`, "zeroing k/n" |
| false | present | complete | linear ensured; no goroutine; **no exports** | `RES_STATUS_OK` (every per-CN row reports `RES_STATUS_PROVISIONING`, "side provisioning") |
| true | present | complete | full DN10 export converge | normal |
| true | present | partial | **refuse exports**; keep the goroutine (it self-heals) | `RES_STATUS_ERROR`, "not zeroed" |
| true | absent | — | **never allocate**; no linear, nothing converges | `RES_STATUS_ERROR`, "record missing" |

Every row above is a *converge* outcome. A read-only round (DN16, SH25)
allocates nothing, so "no record" at `provisioned` false reads
`RES_STATUS_MISSING` there — the converge that would allocate has not run
yet, and `n` must never be taken from the request.

**The zeroing registry** (the dn twin of the DN8 retry registry):

* keyed by the side tuple `(cluster_id, dn_id, sp_id, side_id)`,
  single-flight per side, created on demand by any converge — the startup
  reconcile included (DN2) — that finds zeroing still needed. Sides zero
  in **parallel**, but at most `DnZeroConcurrency` batches run at once per
  agent: N concurrent batches split the disk's Write Zeroes rate N ways,
  and enough of them would have every batch killed at the soft timeout and
  redone for ever. Only the `blkdiscard` holds a slot; waiting for one
  publishes no error, holds no lock, and a cancel reaches it at once.
* each batch mints a fresh trace id (SH2's `common.NewTraceId`); the
  `blkdiscard` itself runs **lock-free** and through the ordinary
  `OsClient` under the standard SH15 timeouts — it is a *child process*,
  so a semaphore slot is held for at most `CmdHardTimeout`, or for as long
  as an uninterruptible kernel wait holds the child past it (SH15), and no
  `LimitedOsClient` carve-out is needed (unlike the CN11 probe IO,
  `cnagent.md`, Leg-probe IO leaves the `OsClient`). Only the volume-table
  update afterwards takes the DN1 locks (node read + the side's object
  lock) on top of `diskmeta`'s own writer serialization, and it takes them
  with **try-acquire and a short poll, never a blocking wait**
  (`zeroAcquire`): teardown cancels this goroutine and waits for it while
  holding the node **write** lock, and a read-write mutex acquire cannot be
  released by cancelling a ctx, so a blocking read lock here would deadlock
  the agent permanently.
* a failed or timed-out batch puts the killed command's output into
  `side_dev_info` (`RES_STATUS_ERROR`) and is retried no sooner than
  `DnZeroRetryInterval` later — never a hot loop. While such a failure is
  outstanding `RES_STATUS_ERROR` wins over the matrix's
  `RES_STATUS_PROVISIONING`; the next successful batch clears it. Partial
  zeros are harmless: the batch's bits stay unset and its extents are
  redone, so a restart simply resumes at the first unset bit.
* **the batch size follows the kills.** A batch the SH15 soft timeout
  killed — the tool did not answer (`agent.Reported`) — makes the side's
  next batch half the killed one (rounded down, at least one extent); a
  success doubles it again, up to `DnZeroBatchExtCnt`; `DnZeroKillBackoff`
  kills in a row drop it straight to one extent per batch, and while that
  kill is outstanding the bullet above's `RES_STATUS_ERROR` — shown only on
  a side still at `provisioned` false; at true the row reads "not zeroed",
  per the matrix — has details of the command output followed by the kill
  streak and the one-extent rate. A batch the tool refused (it ran and
  answered no) keeps the size and ends the streak. The size and the streak
  are the goroutine's own memory — rate control derived from the kills it
  saw, never a record: the size caps the next command and the streak also
  words the details above, and they decide nothing else (which extents to
  zero, and whether the side is done, only the bits decide). Every new
  goroutine for the side, a restart's included, starts again at
  `DnZeroBatchExtCnt` with no kills counted.
* zeroing runs at **every** `sp_level`, `SP_LEVEL_DISABLE` included
  (DN11): it is bottom-layer provisioning.
* cancellation: the drop of a side whose pointer left the list (DN6, which
  is also where a cancelled migration lands — `CancelMigration` reaches
  the agent as the side pointer leaving the list) **cancels the goroutine
  and waits for it** before the sweep removes the dm device, because the
  running child holds `DnSideName` open and `dmsetup remove` would fail
  EBUSY. Process exit is SH27's `WaitBackground`, so no orphan
  `blkdiscard` ever outlives the agent.
* `SideInfo.zeroed_ext_cnt` / `total_ext_cnt` are filled on every reply
  and every Check round (DN14, DN16, DN18). `total_ext_cnt` is never
  omitted: it comes from the record, or from `side_conf.ext_cnt` when there
  is no record yet — so "no record" reads zero of `ext_cnt`, never equal
  counts. Equality is what the worker's flip rule watches, guarded by a
  non-zero total, which also keeps the degenerate zero-of-zero of a
  zero-`ext_cnt` request from reading as done.

DN10. **Per-CN export stacks.** They converge **only** with DN9's gate open
— `side_conf.provisioned` true and every `zeroed_bits` bit set. While it is
closed the whole per-CN stack is skipped (dm-error, dm-linear, nvmet
subsystem, namespace) and nothing is torn down either, because the gate is
monotone; each `cn_id_to_dm_error` / `cn_id_to_dm_linear` /
`cn_id_to_nvmeof` entry reports `RES_STATUS_PROVISIONING` with details
"side provisioning" (DN18). The fault of a row 5 or row 6 side stays on
`side_dev_info` alone — duplicating one cause across every per-CN row would
multiply `err_epoch` churn. With the gate open, for `primary_cn_id` and
every `standby_id_list` entry: `DnErrorName` (dm-error sized like
`DnSideName`), `DnLinearName` (table → the side device for the primary CN,
the dm-error for standbys), nvmet subsystem `SideToCnNqn` on the node port
with the allowed hosts set to that CN's `CnHostNqn` alone, the cntlid range
("attr_cntlid_min"/"attr_cntlid_max") from `side_conf.cntlid_slot`
(`architecture.md`, cntlid slots; `DnCntlidSlotBase`, `DnCntlidSlotStep`),
and one namespace: nsid one, "device_path" = the CN's dm-linear, identity
from `common.DnNsIdentity` of the cluster, sp and leg ids (Additions to
`common` — both sides of a migrating leg MUST match), "attr_serial" = the
leg id in `IdKeyFmt`, "attr_model" = `nsModel`. ANA: the primary CN's
namespace joins `AnaGrpIdOptimized`, standbys join `AnaGrpIdNonOptimized`
([D4]; overridden by the migration phases below and by `sp_level`). A
`primary_cn_id` change reloads the dm-linear tables and rewrites the two
"ana_grpid"s — nothing else. A reload of the old primary's linear whose
load fails leaves it suspended on its old table (OS wrappers), where DN12
rule 2's resume can release its queued IO onto the side's data (Known
limits).

DN11. **`sp_level` gating** (`architecture.md`, SpLevel; numeric
comparisons — the enum values are ordered). Levels are desired state:
raising tears layers down, lowering rebuilds them; bitmap chunks stay
applied-by-file throughout (SH21).

| condition | additional dn behavior |
|---|---|
| at or above `SP_LEVEL_READONLY` | nothing — the level has no DN-side behavior; read-only is enforced on the CN's user-facing namespaces only (`architecture.md`, [D11]) |
| at or above `SP_LEVEL_NO_MIGRATION` | no migration dm-clone: no `DnMigrFinalName`, no `nvme connect`, no `DnMigrMetaDmName`; a destination side keeps its per-CN exports on dm-error, all namespaces `AnaGrpIdInaccessible` |
| at or above `SP_LEVEL_NO_SIDE` | no nvmet exports at all (side subsystems and migration-source subsystem removed); dm devices remain |
| at or above `SP_LEVEL_DISABLE` | only the side device and its allocation record remain; the DN9 zeroing goroutine keeps running at this level too — provisioning sits *below* the level ladder |

The CN-only intermediate levels (`SP_LEVEL_NO_CLONE`,
`SP_LEVEL_NO_THINPOOL`, `SP_LEVEL_NO_REDUND`) have no DN-side behavior
either — on the DN, every level below `SP_LEVEL_NO_MIGRATION` behaves like
`SP_LEVEL_READWRITE`.

**dnv has no DN-side read-only mechanism at all** (`architecture.md`, [D11]). nvmet opens a
namespace's backing device for reading and writing, so the top device of
any export can never be read-only; and a read-only flag *below* the top
does not stop writes that device-mapper remaps onto it, because the
kernel's check runs at top-level bio submission only. The DN could not fail
writes even if it wanted to: md superblock and bitmap writes, resync,
failover assembly and the health-check block writes of `architecture.md`,
Group on-leg layout: meta region, data region, health block, must keep
flowing at every read-only level. Per-CN dm-linears and `DnSideName` are
therefore always writeable, whatever the level. `SP_LEVEL_READONLY` is
enforced solely on the CN's user-facing namespaces, by reloading each
`CnNsDevName` onto a dm-flakey table that errors writes (reads pass, writes
error).

DN12. **Migration source** (`migr_src_conf` set): the sequence of
`architecture.md`, Migration, in order — (1) move every per-CN namespace to
`AnaGrpIdInaccessible`, (2) retire every per-CN dm-linear through the
two-phase fence below, (3) build `DnMigrSrcName` (linear on the side
device) and export it via subsystem `MigrSrcNqn` on the node port, its
allowed hosts set to the `DnHostNqn` of `migr_src_conf.dst_dn_id` alone,
its namespace in `AnaGrpIdOptimized`.

**The `dst_provisioned` gate.** `migr_src_conf.dst_provisioned` false means
the destination side is still being zeroed (DN9), and it is **normative**
that the source then behaves *exactly as if `migr_src_conf` were absent*:
no ANA move, no fence, no `DnMigrSrcName`, no migration-source subsystem —
the side keeps serving its per-CN stacks normally. The only difference is
reporting: the would-be `migr_src_info.dm_linear_info` and `.nvmeof_info`
are `RES_STATUS_PROVISIONING` with details "side provisioning" instead of
absent. Without the gate the source would fence the primary's path the
moment the migration was created and the leg would have **no serving path
for the whole zeroing window** (`architecture.md`, Migration). When the
worker flips the destination side, the next fan-out carries
`dst_provisioned` true and the sequence above runs unchanged; the
destination's connect retry (DN13) absorbs any cross-side ordering.

**The step-2 fence ([D12]).** Phase 1: suspend each per-CN dm-linear **in
place**, leaving its table alone, and record when. Phase 2, on the first
converge at or after `SuspendSeconds` have passed: reload it onto its
dm-error, which resumes it — unless the load fails, which leaves it
suspended on its pre-fence table (a reload fails closed, OS wrappers) until
a later converge's reload of it succeeds or the role's end resumes it (rule
2 below), so the bound the rules below keep holds only as far as those
reloads succeed. Swapping the table in phase 1 would error the very IO the
window exists to absorb; resuming without the swap would replay it onto the
side's data. The RPC never waits out the window — a one-shot timer arms the
converge that ends it, the same way DN8 arms the connect retry — and every
converge is idempotent, so an early one at an exporting level simply stays
in phase 1.

Four rules keep the suspension bounded, which is what makes it safe:

* A side whose linears are suspended but whose window start is unknown — an
  agent restart mid-window — is treated as **elapsed** once the restarted
  agent finds them so (`adoptFence`), and phase 2 runs on the first
  converge, so a restart that finds them suspended opens no second window.
  The window's start, and the mark rule 3 leaves when a level ends a
  window early, live only in memory; what a restart that does not find the
  linears suspended, or that holds no state for the side, does with the
  window is a known limit (Known limits).
* The role ending clears the window and returns the linears to their
  normal targets, resumed. The condition is a **state**, not an event:
  every converge of a side whose *effective* `migr_src_conf` is absent —
  dropped, or deferred behind `dst_provisioned` false — clears the window
  and resumes every per-CN dm-linear of that side it finds suspended
  (`unfenceLinears`), so nothing has to remember that a window was ever
  opened. The set comes from the **enumeration** of what the node holds,
  not from the plan's cn list: a linear built for a CN that has since left
  `standby_id_list` is exactly the one a remembered list would miss, and
  leaving it suspended would queue bios with no timeout. The resume alone
  is enough — the queued IO drains against whatever table is live, here
  the pre-fence one, and the build phase then reloads the linear onto the
  target the new desired state wants. The same resume also meets a linear
  that a primary flip's failed reload left suspended on its old table
  (DN10), and there it is not enough: it releases the old primary's queued
  IO onto the side's data ahead of any retry of that reload (Known limits).
* Tearing the side down, or taking its exports away, inside the window
  ends the window early, with the same reload. **Before** its first layer
  (DN6) the sweep puts every suspended per-CN linear it is about to
  remove, or whose export it is about to remove, on its dm-error and
  resumes it — never a bare resume, which would replay the absorbed IO
  onto the side's data — because disabling an nvmet namespace first waits
  for every request in flight on it, and one whose bio a suspended dm
  target holds never completes. A request that also ends the role is rule
  2's case instead: its pre-step resumes the linears onto their pre-fence
  tables before this step runs. A level with no export layer —
  `SP_LEVEL_NO_SIDE`, which keeps the linears and takes only their
  exports, or any level above it — ends the window outright: for a source
  side at such a level the sweep's pre-step marks it over (`endFence`),
  the way an adopted one is, until the role itself ends (a state again,
  and in memory like the window's start), so a later converge of the same
  process at an exporting level — the level coming back down — rebuilds
  the exports over linears on their dm-errors instead of opening a second
  window over them. A restart forgets the mark (Known limits).
* A converge that stops at the side-device gate — *any* DN9 outcome short
  of ready: still provisioning (zeroing, or zeroed and not yet released by
  the control plane), or a side-device fault (an unreadable disk, "record
  missing", "not zeroed", a refused allocation, a table or probe that
  would not converge) — skips the whole per-CN stack, but never the fence
  (`settleFence`): inside the window it re-arms the timer; past it, it
  finishes phase 2 itself. Nothing else would end the window there: the
  timer nils itself before converging, the only other unfences are the two
  above and neither applies while the role, the side and its exports are
  still wanted, the agent's one periodic converge — DN8's connect retry,
  armed only while a destination role's connect is failing — would take
  this same gate, and a `CheckSide` round neither converges nor bumps a
  revision. One transient probe failure would otherwise leave the linears
  suspended, queueing bios with no timeout, until the worker next happened
  to re-sync the side. Running phase 2 under that gate is safe: the
  dm-error and the dm-linear are the devices the fence itself suspended,
  not something built on top of the side, and retiring them only moves the
  side further from exporting data.

While the window is open the per-CN `dm_linear_info` is `RES_STATUS_OK`
with details "suspended (migration cutover grace window)": it is an
expected, time-bounded state, and the probe expects the **pre-fence** table
there rather than the dm-error, so a healthy cutover reports no table
mismatch — except in the second window of Known limits, opened over tables
phase 2 has already swapped.

**Ending the role removes nothing directly.** `DnMigrSrcName` and its
`MigrSrcNqn` export simply stop being wanted, and the sweep takes them (DN6
L4 and L1) once every side of the sp this node may host is held. The
linear is keyed by `(sp_id, migr_id)` and the export by the cluster, dn, sp
and migration ids; neither names a side, so both are judged by the claim
rule — no side of this DN whose stored request this process holds still
names that `migr_src_conf` (it holds none for a side DN2's reload skipped)
— rather than by an "applied source" the converge would have overwritten
on its way past. Deferral is the same condition: while `dst_provisioned`
is false the effective source conf is absent, so anything an earlier pass
built for it is unwanted and goes, which is what makes "behaves exactly as
if `migr_src_conf` were absent" true of the removal half as well.

DN13. **Migration destination** (`migr_dst_conf` set).

**Provisioning first.** While the destination side's own
`side_conf.provisioned` is false, or any of its `zeroed_bits` is unset,
**none** of steps (1) to (5) run. The side converges to the DN9 shape only
— the extent record, `DnSideName` and the zeroing goroutine — with no
per-CN stacks, no metadata slot, no `nvme connect` and no dm-clone;
`migr_dst_info.target_info` and `.dm_clone_info` report
`RES_STATUS_PROVISIONING` with details "side provisioning". Bitmap chunks
pushed meanwhile are still persisted and counted as applied (DN15) and are
applied when the dm-clone is finally created. Cancelling the migration
inside this window is the DN9 cancel-and-wait path: the goroutine is
stopped and waited for before `DnSideName` is removed.

With the gate open, the sequence of `architecture.md`, Migration — (1)
per-CN stacks on dm-error, all namespaces `AnaGrpIdInaccessible`; (2) the
dm-clone metadata slot — contiguous `DnCloneMetaUnit` units in the [D13]
clone-metadata area, sized for the side's region count, its head zeroed
**before** its record is persisted so a previous tenant's bytes cannot be
misparsed as a dm-clone superblock — plus its wrapper dm-linear
`DnMigrMetaDmName` (the dm-clone target reads its metadata device from
sector zero and takes no offset argument); (3) `nvme connect` to the
`MigrSrcNqn` of the cluster, `migr_dst_conf.src_dn_id`, sp and migration
at `src_nvme_tr_conf` with hostnqn `DnHostNqn` of this cluster and dn
(SH20); (4) dm-clone `DnMigrFinalName` (meta = the step-2 wrapper, dest =
the side device, source = the nvme device, region size = `block_size`,
features **no_hydration and no_discard_passdown** — both are mandatory on
**every** dnv dm-clone, dn and cn alike (`cnagent.md` CN18), because the
bitmap protocols of `architecture.md`, Bitmap push protocol and raid0
bitmap math, use `blkdiscard` on a dm-clone as the metadata-only "mark this
region hydrated" primitive: dm-clone turns discard passdown on by default
whenever the destination's discard granularity is no larger than one region
— a dm-linear over a raw disk always satisfies that — and would then *also*
remap the discard to the destination. The hazard is **after** the cutover,
not before it: host IO already flows through the destination dm-clone, a
host write hydrates region *r*, and a skip-bitmap chunk whose bit for *r*
was read from the CN thin metadata before that write arrives later — pushes
are legal at any time and a restart re-applies every stored chunk — so the
resulting `blkdiscard` would destroy the only copy of an acknowledged
write. Without passdown the same discard is the metadata no-op that
`architecture.md`, Bitmap push protocol, and `architecture.md`, [D7] assume. Knobs from
`dm_clone_conf`); (5) reload the primary CN's dm-linear onto the dm-clone,
move its namespace to `AnaGrpIdOptimized`, the standbys' to
`AnaGrpIdNonOptimized`; re-apply every bitmap chunk of the applied set
(SH21). The numbering above follows the logical steps of `architecture.md`,
Migration; the implemented converge order differs without changing any end
state (`SyncupSide` builds bottom-up: slot, connect, clone, linear,
"ana_grpid"): `ensureMigrDst` runs steps (2) to (4) **before** the per-CN
stacks converge, so on a pass where the connect succeeds the per-CN
dm-linears are *created* directly on the dm-clone and the primary's
namespace goes straight to `AnaGrpIdOptimized` — step (1)'s dm-error and
`AnaGrpIdInaccessible` shape and step (5)'s *reload* occur only while the
connect is still retrying across passes. "Retrying until success"
(`architecture.md`, Migration) is implemented without retrying inside the
RPC: a converge pass attempts the connect **once**, and after a connect
that succeeded it re-reads the subsystem until the source's namespace
device is there, pausing `DnMigrDstNsPause` between reads and
`DnMigrDstNsWait` in all (`awaitMigrSrcNs`, a `WaitBudget`) — the kernel
returns from the connect once the controller is live and only queues the
namespace scan that adds the device, so a single re-read could fail the
target "controller has no namespace" for a device milliseconds away (the dn
twin of `cnagent.md` CN10's head wait); a read that fails ends that wait at
once. When the connect fails, or the namespace has still not appeared, the
pass records `target_info` as `RES_STATUS_ERROR` and registers the side in
a background retry registry that re-runs the destination converge every
`DnMigrConnectRetryInterval` under the DN1 locks. It is deregistered on
success, by the sweep's pre-step as soon as a destination role stops being
wanted, and by the drop of a side whose pointer has left the list (DN6) —
always by the state, never by remembering that it was once registered.

**The deregistration runs inside the very converge it is ending, and that
is the sharp edge.** Registering creates a cancellable context and
deregistering cancels it — so the attempt the retry loop runs must not be
the thing that context governs. A loop that converged on its own context
would reach the deregistration in the pass that finally connects, cancel
itself, and then fail every remaining OS call of that pass on the dead
context: the dm-clone would never be created, the registration would
already be cleared so nothing would tick again, and the side would sit at
a `dm_clone_info` of `RES_STATUS_MISSING`, "target not connected", **for
ever** — while the controller that reading names is live. One transient
connect failure is enough to reach that path, and an RPC-driven converge
cannot show it, because an RPC-driven converge runs on the gRPC context,
which the cancel cannot touch.

So the loop's context governs the **loop**, not the attempt: each attempt
reconverges on `rootCtx` (`migrRetryLoop`, `reconvergeSide`), exactly as
the fence timer does, and the cancel is read by the loop's own check one
tick later at worst. Shutdown still stops an attempt in flight, because
`rootCtx` is cancelled before `WaitBackground` joins (SH27).

**Retiring the role is the sweep's, and it starts with a repoint.** When
`migr_dst_conf` goes — the finish of `architecture.md`, Migration, a level
at or above `SP_LEVEL_NO_MIGRATION`, or the side leaving its DN's list —
`DnMigrFinalName`, `DnMigrMetaDmName` and the source connection stop being
wanted and DN6's layers take them, clone before connection and wrapper
after both, once every side of the sp this node may host is held. All
three are identified by `(sp_id, migr_id)` — the connection's NQN carries
the **source** DN's id, not this node's, so that pair is the only part of
it which names the migration. Nothing in that NQN names *this* agent
either, and the nvme host namespace is per kernel, so the connection is
attributed first by its controller's "hostnqn" (DN6): a controller some
other dn agent on this node opened is not a candidate at all. What that
leaves is decided by the claim rule: no side of this DN whose stored
request this process holds (it holds none for a side DN2's reload skipped)
still names that `migr_dst_conf` under a wanted destination role. That is
the whole replacement for an "applied destination" to diff against — a
field the converge would overwrite on its way past, so a removal that
failed would be forgotten by the pass that was meant to retry it.

On the finish path the per-CN dm-linears are **kept** and still map the
clone, so the sweep repoints each one onto the plain `DnSideName` first,
deciding that it needs the repoint by reading the linear's **live table**
rather than by remembering what it applied last time (DN6). Without that
the clone's removal fails EBUSY under the linear and the whole chain waits
a round for the build phase to repoint it. The metadata slot is released
only after the wrapper's removal has been **verified**, and only when the
record rule (DN6) also proves the slot orphaned; a slot whose wrapper would
not go stays allocated and is retried by the next pass.

**Chunks of a previous migration on the same side are quarantined.** The
side records the `migr_id` its stored chunks belong to, and a destination
converge whose `migr_id` differs drops them — files included — before step
(2); a push naming another `migr_id` never gets that far (DN15). Migration
ids are never reused, so a mismatch is proof the chunks describe a
different copy, and applying them would `blkdiscard` regions this migration
never copied, leaving the destination serving its own zeroed extents where
the source's data should be.

**The clone-metadata area is per DN, and it is a real ceiling.**
`DnCloneMetaSize` is one region of the disk shared by every destination
role this node hosts, across every SP on it. A migration costs the units
its dm-clone metadata needs — a fixed base plus one byte per region, rounded
up to whole `DnCloneMetaUnit`s — so the area bounds the concurrent
destination roles per DN, and bounds them tighter for large sides at small
block sizes. `MaxMigrCntPerSp` bounds none of this and the control plane
does not gate against it (`architecture.md`, Migrations): exhaustion is
reported as `RES_STATUS_ERROR` on the `migr_dst_info` rows — the dn twin of
the cn arena ceiling in `cnagent.md` CN18.

DN14. Persist (SH5); reply `agent_reply` — the side-level sweep's verdict
(DN19) — `revision`, `side_info` — including
`zeroed_ext_cnt`/`total_ext_cnt`, filled on every reply (DN9) — and
`bm_info` (the applied set, SH21).

### `PushMigrBitmap`

DN15. Gate: the side must be known and its `migr_dst_conf.migr_id` must
equal the request's `migr_id` (`ReplyCodeUnknownObject` otherwise — only a
destination side accepts chunks). There is **no revision gate**: the
request carries no `revision` field at all. A chunk is position-addressed
data keyed by ids that are never reused, and applying one never advances
the stored revision, so a push the worker planned against a report the
agent has since replaced with a newer one is harmless — while rejecting it
would throw away work the worker has already done and need a whole re-sync
to re-drive (`dnv-worker.md` BM3). Then SH21 to SH23: persist the chunk at
`LocalMigrBmPath`, recompute from every chunk of the applied set (shift by
the leg's `meta_blocks`), `blkdiscard` the fully-skippable regions of
`DnMigrFinalName`. If the dm-clone does not currently exist — not built
yet, suppressed by `sp_level`, or still behind DN13's provisioning gate —
the file still counts as applied; chunks are re-applied whenever the
dm-clone is (re)created. A **failed persist is still acked code zero**,
with the error logged and neither the recompute nor the `blkdiscard`
attempted (`architecture.md`, Bitmap push protocol): not persisted is not
applied, so the chunk stays out of the applied set — SH21 derives that set
from the files present — the next `SyncupSide` reply's `bm_info.bm_idx_list`
omits the index, and the worker pushes it again on a later round. Reply
`agent_reply` only.

### `GetDnInfo` / `GetSideInfo`

DN16. Read-only: probe fresh under the DN1 locks and reply `agent_reply`,
`revision`, the info. An unknown DN or side pointer — one this process
holds no request for, as when none was ever applied, or when the startup
reload could not load or skipped its file (DN2) and none has been applied
since — ⇒ `ReplyCodeUnknownObject` with `revision` zero.

For a **known** object the `agent_reply` is the **verdict**: the sweep of
DN6 run with its removals left out — the same enumeration, the same wanted
set, the same comparison, nothing touched — so `GetDnInfo` replies
`ReplyCodeLeftover` while the node holds node-level leftovers, while one of
its listings does not answer, and while the disk's identity is not
confirmed (DN6's record step: every round for as long as the disk stays
unreadable, corrupt, another node's, or blank under a live side device or
clone-metadata wrapper, and the worker re-sends `SyncupDn` each time), and
`GetSideInfo` while that side holds leftovers, while one of its listings
does not answer, and — the one comparison a side's verdict makes beyond its
sweep's — while its record, read from a table this node has confirmed
(DN5), still has extents to zero and no DN9 zeroing goroutine is running
for it ("zeroing k/n: nothing is zeroing the side"). Only a converge starts
that goroutine, and one that could not — the startup reconcile's, while no
read of the disk answered — would otherwise leave the side reporting the
same progress every round with nothing to re-send its `SyncupSide`; the
registry and the record are both read fresh, so nothing about the failed
converge is remembered. Both reply zero otherwise. `GetDnInfo` and a
`CheckDn` round probe before they take the verdict: the probe's read of
the disk can be the first to answer, which is what confirms the identity
(DN5). A DN's verdict covers the node-level scope only and a side's covers
its own side; each drives its own `Syncup*`. The verdict is recomputed on
every call and stored nowhere, so a leftover that has since gone stops
being reported without anyone clearing a flag. A DN whose stored
`extent_size` is unusable has **no** verdict — the conf gate says it
converges nothing and sweeps nothing, so naming leftovers there would
report objects this agent has deliberately refused to touch — and neither
has a side whose DN is unknown or in that state, since its own geometry
comes from the DN.

Never mutates the node — in particular a `Get*Info` or `Check*` round never
allocates a record and never registers a DN9 zeroing goroutine
(registration happens only on a converge path), and it reports
`RES_STATUS_MISSING` for a side whose record the converge has not written
yet. That holds for the verdict too: it enumerates and compares, and
removes nothing. What a round does change is the agent's own account of
what it observed: the SH14 tracker, the disk size the allocator reads, and
`DiskMeta`'s copy of the volume table, which a read that answers loads
(that read can be the one that confirms the identity, DN5) and which a
header other than the one it came from drops (DN18), so the converges,
probes and DN9 goroutines that follow re-read the disk.

### `CheckDn` / `CheckSide`

DN17. Instantiate the SH24 to SH26 loop with the probes of Probing and
error capture; one round takes the DN1 locks of the corresponding
`Get*Info` and carries the same read-only verdict in its `agent_reply`
(DN16). That is what makes the retry worker-driven: while leftovers exist
every round replies `ReplyCodeLeftover`, the worker re-issues the
`Syncup*` that owns them (`dnv-worker.md` RW4), and the sweep inside it
tries again — including after a restart, where the first Check round is
what reports what the startup sweep could not remove. No agent-side loop
exists for leftovers, because the one the worker already runs is enough;
DN13's migration connect retry and DN12's fence timer sweep only as part
of a converge they re-run for a reason of their own.

### Probing and error capture — `probe.go`

DN18. Probe map (all via SH17 conventions; a `res_name` and a probe per
resource). `RES_STATUS_PROVISIONING` rows are **healthy**: the resource is
deliberately not created yet, no action is needed, and the worker never
turns one into an `err_epoch` (`architecture.md`, Live-state reporting);
`RES_STATUS_ERROR` keeps meaning *needs intervention*.

* `DnInfo.disk_info`, named by the `--disk` path: the disk's size read
  succeeds.
* `DnInfo.meta_info`, named by the `--disk` path: a `ReadBlock` of the
  header checks magic, version, CRC and the `cluster_id`/`dn_id`/
  `extent_size` identity (`DiskMeta.ProbeHeader`) — a header other than
  the one the loaded volume table came from (blank, corrupt, or valid with
  another identity or another `format_uuid`) also drops that table, so the
  next call re-reads the disk and nothing is handed out or written from
  the old one, and a blank disk is then formatted only as DN5 allows —
  until then every side's lookup finds no record (the `side_dev_info` row
  below); a read that did not answer changes nothing — **plus the DN5
  Write-Zeroes check**. The details when OK are `DiskMeta.Describe`'s
  summary (the table sequence, the side and clone-metadata record counts,
  the free extents and units, and the sides whose `zeroed_bits` are still
  incomplete, DN9); on failure the error text instead, "disk lacks Write
  Zeroes" for the Write Zeroes case.
* `DnInfo.port_info`, named by the agent's port id as a decimal — so on a
  node running several agents the rows differ: the configfs address
  attributes match the `--tr-*` flags and the three [D4] groups are
  present with their fixed states (`Nvmet.ProbePort`).
* `SideInfo.side_dev_info`, named `DnSideName`: the volume-table record +
  its `zeroed_bits` + the live table, judged by the DN9 matrix: no record
  ⇒ `RES_STATUS_MISSING` at `provisioned` false (no converge has allocated
  one yet, or the header has gone blank under the side: a blank header
  reads as an unformatted disk with no records, and the meta row's probe
  drops a table loaded before the header went blank — the side's device is
  still there all the same, and its DN9 goroutine, if one is running, ends
  at its next re-read of the record) and `RES_STATUS_ERROR`, details
  "record missing", at `provisioned` true (a lost or foreign disk, or that
  same blank header, under which the side's devices keep serving its data:
  nothing removes a device the side still wants, and DN5 will not format
  while they map the disk); bits incomplete ⇒ `RES_STATUS_PROVISIONING`,
  details "zeroing k/n", at false and `RES_STATUS_ERROR`, details "not
  zeroed", at true; at false, an outstanding batch failure ⇒
  `RES_STATUS_ERROR` with the killed command's output, followed by the
  kill streak and the one-extent rate once DN9's backoff holds; a live
  table that does not match the record's extent runs ⇒ `RES_STATUS_ERROR`.
  The same read fills `SideInfo.zeroed_ext_cnt`/`total_ext_cnt` every
  round.
* `cn_id_to_dm_error` and `cn_id_to_dm_linear` per cn, named `DnErrorName`
  and `DnLinearName`: `dmsetup info` plus `dmsetup table` (`probeDmTarget`;
  the linear's target — side device, dm-error or dm-clone — must match the
  desired role). Inside the grace window of `architecture.md`, Migration,
  the expected target is the **pre-fence** one and the details are
  "suspended (migration cutover grace window)"; the probe never starts a
  window (DN16). While DN9's gate is closed no device is expected to exist
  and both report `RES_STATUS_PROVISIONING`, details "side provisioning".
* `cn_id_to_nvmeof` per cn, named by the `SideToCnNqn`: configfs —
  subsystem present with its attributes, namespace enabled over the right
  "device_path" and in the desired "ana_grpid", linked to the port
  (`probeExport`). `RES_STATUS_PROVISIONING`, details "side provisioning",
  while DN9's gate is closed.
* `migr_src_info.dm_linear_info` and `.nvmeof_info`, named `DnMigrSrcName`
  and the `MigrSrcNqn`: the dm target probe and configfs. With
  `migr_src_conf.dst_provisioned` false neither object exists by design
  (DN12) and both report `RES_STATUS_PROVISIONING`, details "side
  provisioning".
* `migr_dst_info.target_info`, named by the `MigrSrcNqn`: the SH20 **sysfs
  walk** (the subsystem matched by "subsysnqn", the controllers' "state"
  — never `nvme list-subsys`) shows a live controller for it (liveness
  only, SH20); `RES_STATUS_PROVISIONING`, details "side provisioning",
  while the destination side is still zeroing (DN13).
* `migr_dst_info.dm_clone_info`, named `DnMigrFinalName`: `dmsetup
  status`; the details carry the raw status line (`architecture.md`,
  Live-state reporting — hydration progress); `RES_STATUS_PROVISIONING`,
  details "side provisioning", while the destination side is still zeroing
  (DN13). The `DnMigrMetaDmName` wrapper has no `ResInfo` of its own: its
  health folds into this one.

DN19. Error capture (`architecture.md`, Common agent rules): a failed
command marks that resource `RES_STATUS_ERROR` with the command output in
`details` and the converge pass **continues** with the remaining resources
— the agent converges as much as it can.

**Leftovers are the one non-protocol outcome that travels in
`agent_reply`**, and the reason is structural: the `*Info` rows are keyed
by the ids of **wanted** objects, and a leftover is by definition something
nothing wanted names, so it has no row to ride in. A pass whose sweep (DN6)
could not remove everything, or whose enumeration did not answer, replies
`ReplyCodeLeftover` with `details` listing the leftovers as "kind:name"
entries with their count — at most `maxLeftoverNames` names, then a "[+k
more]" tail — and an "enumeration failed" entry naming what did not answer
and the error, for each such listing (`LeftoverReply`). Two dn checks that
are not listings are folded into the same outcome, in that same form, so
that they drive the same re-send: DN6's record step on a disk whose
identity is not confirmed, and — in the read-only verdict only — a side
with extents still to zero and no zeroing goroutine (DN16). The full list
goes to the agent log once per pass as the "sweep leftover" record
(`log.md`, Leftovers), so a lingering leftover is visible every round
rather than once.

It is **not** a rejection (SH9). The request was applied, the desired state
is stored, and the `*Info` rows are the converge's full account of every
wanted object, so the worker evaluates them exactly as it does for code
zero and simply re-issues the `Syncup*` every round until the code changes
(`dnv-worker.md` RW5, HL1). Everything else reported through `agent_reply`
is still a protocol-level refusal.

## Known limits

* A reload that fails leaves its device suspended (OS wrappers), so
  [D12]'s bound on a suspension holds only as far as the reloads succeed.
  When a primary flip's reload of the old primary's linear fails its load,
  the linear stays suspended on its old table only until a later converge
  of the side resumes it there (DN12 rule 2), releasing onto the side's
  data the IO it queued, ahead of any retry of the reload in that
  converge's build phase.
* The fence window's start, and the mark a level with no export layer
  leaves when it ends a window early (DN12 rule 3), live only in memory.
  After the window, with the linears resumed on the dm-errors phase 2 left
  them on, a restarted agent's first converge that builds the side's per-CN
  stacks at an exporting level, with the role still standing, opens a
  second window over those dm-errors — unless a pass at a level with no
  export layer has marked the window over again before it. Nothing that
  window absorbs can reach the side's data — the table that holds it is
  already the dm-error — but the linears sit suspended for another
  `SuspendSeconds`, and the probe, which inside a window expects the
  pre-fence table, reports the primary's linear `RES_STATUS_ERROR` until
  phase 2 runs again.
* A restart inside the window whose probes of the linears all go
  unanswered does not find them suspended either, and one that ends up
  holding no state for the side keeps nothing of what it found: after a
  lost `--local-store`, with the side's `side-*` file missing or not
  loading, or with its `dn-*` file not loading (DN2 skips the side) or
  missing (DN2 drops the side as one whose pointer left the list). A found
  suspension is marked on the side's in-memory state, which such a restart
  does not keep; nothing else touches the side's linears before its
  `SyncupSide` arrives — the node-level sweep keeps those of a side its
  DN's list names (DN6), and a DN not loaded is not swept at all — and that
  side's first converge finds no window this process knows of. Either way
  the process treats no window as elapsed: the linears stay suspended on
  their pre-fence tables with no window running and no timer armed, and no
  verdict names them: they are wanted, their side's pointer is listed, or
  their DN is not loaded. At an exporting level, with the role still
  standing, a converge that stops at the DN9 gate leaves them so — DN12
  rule 4 acts only on a window the process knows of — and, unless a pass at
  a level with no export layer has marked the window over and put them on
  their dm-errors before it (DN12 rule 3), the first one that builds the
  side's per-CN stacks opens a second window over them, which phase 2 ends
  `SuspendSeconds` later, however long that converge was in coming. While
  they sit so, a probe that gets past the side device expects the dm-error
  and reports the primary's linear `RES_STATUS_ERROR`; a side the restart
  does not hold has no probe, and answers its Check rounds
  `ReplyCodeUnknownObject` until that `SyncupSide` arrives.
* A side export whose build was abandoned between its namespace `mkdir` and
  its "device_path" write holds a namespace that names nobody. Every
  agent's attribution (DN6) reads that export as foreign, so no sweep
  removes it; a later converge of the side it was built for finishes it,
  but once no side wants it, it stays.
* The dn sweep's disconnect of a destination's source connection (DN6 L3)
  runs under the DN1 locks, as the cn build's dead-path disconnect does;
  only the cn sweep's runs off them (`cnagent.md` CN10 and CN21). A target
  that vanishes mid-delete therefore holds the side's converge, and the
  `SyncupSide` carrying it, for the kernel's admin timeout (SH15).
