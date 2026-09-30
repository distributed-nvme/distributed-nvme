# dnagent.md — `dnv-agent` and the dn role (dnv)

Status: **normative**. Read `log.md`, `osclient.md` and `grpc.md` first — this
document builds on their rules (one Info record per operation, `OsClient`-only
OS access, trace-id propagation, and the four shared `grpc.md` §3 interceptors:
unary client, stream client, unary server, stream server) and does not restate
them.

Required background: `schema.proto` (service `DiskNodeAgent` and its messages,
plus `SpLevel`, `ResInfo`, `DnInfo`, `SideInfo`, `BitmapInfo`, `SidePointer`,
`NvmeTrConf`); `architecture.md` §3.1 (the DN device stack), §4 (names, §4.6
local-store paths), §9 (agent rules — §9.1 is the contract this document
implements), §11.2 (migration), §11.7 (SpLevel), §11.8 (cntlid slots), §13
(invocation), Appendix A (command patterns), [D4] (fixed ANA groups), [D13]
(the disk is authoritative), [D14] (no LVM anywhere in dnv) and [D15]
(whole-side zeroing behind the `provisioned` gate); `layout.md` §2/§3/§5.

Scope split: §2 and §3 are **[shared]** — they specify the mechanism package
`agent` and the `cmd/dnv-agent` binary skeleton, which `dnv-agent cn` reuses
unchanged; `cnagent.md` MUST NOT re-specify them, and does not — its §2/§3
add to and amend them instead. §4 is **[dn]** —
the `agent/dnagent` policy package. §5 records the amendments this document
made to the companion documents.

---

## 1. Scope and placement

| package | path | role | may import (layout.md §3) |
|---|---|---|---|
| `agent` | `agent/` | shared dn/cn **mechanism**: bootstrap, local store, revision gate, locks, `ResInfo` tracking, OS wrappers, bitmap store, check-loop rules | `common`, `pb` |
| `dnagent` | `agent/dnagent/` | dn **policy**: the `DiskNodeAgent` service — which extent allocations, dm tables and nvmet objects to build and when | `common`, `pb`, `agent` |
| `main` | `cmd/dnv-agent/` | cobra `dn`/`cn` dispatch, viper flags, dependency construction | `agent`, `agent/dnagent`, `agent/cnagent`, `common`, `pb` |

Agents never talk to etcd (`layout.md` §3); acceptance re-checks it. The
gRPC server carries the **server** interceptors of `grpc.md` §4 and no client
interceptors — the agent's only outbound connections are `nvme connect`, not
gRPC. The split rule for new code: anything both roles need verbatim is
mechanism and belongs in `agent`; anything that knows *which* resource to
build is policy and belongs in `dnagent`/`cnagent`.

## 2. Shared mechanism — package `agent` [shared]

### 2.1 Files

`agent.go` (bootstrap, §2.3), `store.go` (§2.4), `revision.go` (§2.5),
`conf.go` (the stored-conf validators, below; the rejection they build is
SH9), `locks.go` (§2.6),
`resinfo.go` (§2.7), `oswrap.go`/`dm.go`/`nvmet.go`/
`nvmehost.go` (OS wrappers, §2.8 — `oswrap.go` is the shared command/configfs
plumbing the other three sit on), `bitmap.go` (§2.9), `sweep.go` (the part of
DN6's sweep both roles share: the leftover kinds, the result value a pass
accumulates, its log record and the `AgentReply` it becomes), `waitbudget.go`
(`WaitBudget`, the bounded wait one converge pass may spend on the node
catching up with what that pass asked of it: DN13's wait for the source's
namespace, and the cn connect step's pass budget of `cnagent.md` CN10/CN18),
plus colocated `_test.go` files.

`conf.go` is a **deliberate second copy** of `model`'s stored-conf rules.
`layout.md` §3 forbids the agent packages from importing `model`, which
links the etcd client, so `agent.ValidateBdevConf` and
`agent.ValidateExtentSize` restate the presence checks of
`model.ValidateBdevConf` and `model.ValidateClusterConf` with
**byte-identical error strings** — the same `"invalid stored conf: "`
prefix and the same proto field names — and `agent.InvalidConfReply` turns
one into the SH9 rejection. Identical by construction is the point: one
grep finds every refusal across the control plane and both agents.
`agent/conf_test.go` and `model/capacity_test.go` pin the same five strings
string-exactly, one on each side, so a change made to one copy and not the
other goes red; §6 test 23 and `cnagent.md` §6 test 27 assert them once
more through the two roles that reply with them. Both validators check
**presence only** — the `architecture.md` §7 range checks
belong to the gateway, which sees the request that set the value — and
`low_water_mark_pct` is checked for zero alone, because a value above 100
is the legal "never grow this pool automatically" setting.

The file list above is the `layout.md` §2 recommended split; package
boundaries are binding, file names are not. There is no `lvm.go`: **no** dnv
agent runs any LVM command at all — [D13] took LVM off the dn, [D14] took it
off the cn too.

### 2.2 Additions to `common`

The following enter the existing files `common/constants.go` and
`common/name_fmt.go`, plus one file that joins them,
`common/name_parse.go`: DN6's sweep has to look at a name the kernel hands
back and say whether it is ours, of which kind and of which sp, so the two
name shapes the sweep has to decode gained their inverses there —
`ParseDmName` for the dm device names (every `DmKind`) and `ParseNqn` for the
dnv-format NQNs (every `NqnKind`). The rest of `name_fmt.go` has no inverse:
the `/dev` and local-store paths, the tmpfs paths and the md names are
formatted only — an md array is attributed by its members' dm names instead
(`cnagent.md` CN21) — and the hashed forms (`DnNsIdentity`, `NvmeHostId`,
`CnMdDevName`'s short id) could not have one. Parsing is strict: a name that
does not decode exactly is "not a dnv name", never a half-decoded one, because
the caller's next move is a removal. The const
listing is a condensed, topic-grouped quote rather than a byte-for-byte one:
its three groups live in three separate places of `common/constants.go`'s
single `const` block, in a different order; the `SuspendSeconds` comment there
carries a longer rationale than the one below; and the Go file's comment
wrapping and gofmt's trailing-comment alignment differ from the listing, so the
block cannot be pasted back into Go unchanged. The names, the values and the rules the
comments state are the committed ones, and `common/constants.go` is
authoritative for comment text, wrapping and order (unlike `log.md` §4 /
`grpc.md` §3, whose byte-identity is pinned):

```go
	// The DEFAULT configfs id of the nvmet port an agent converges
	// (architecture.md §3.1/§3.2: exactly one port per agent).
	// `dnv-agent --nvmet-port-id` overrides it, which is what lets several
	// agents share one node's kernel, each converging its own port.
	NvmetPortId = 1

	// The three fixed ANA groups on every node's port (architecture.md
	// [D4]). Group 1 always exists in nvmet and defaults to optimized;
	// groups 2 and 3 are created at port setup. Group states are written
	// once and never changed; every ANA transition rewrites a namespace's
	// ana_grpid instead.
	AnaGrpIdOptimized    = 1
	AnaGrpIdNonOptimized = 2
	AnaGrpIdInaccessible = 3

	// AgentReply.code values (dnagent.md §2.5). 0 = OK. Callers only ever
	// branch on code != 0; the specific values exist for details/log
	// readability and tests.
	ReplyCodeStaleRevision = 1
	ReplyCodeUnknownObject = 2
	// ReplyCodeInvalidConf refuses a request whose conf carries a value the
	// control plane cannot have written — a proto3 zero where §7 requires a
	// concrete geometry. The object is known and the revision is current; it
	// is the conf that is unusable, which is why it is neither of the two
	// above.
	ReplyCodeInvalidConf = 3
	// ReplyCodeLeftover reports an ACCEPTED request with residue: the
	// desired state is stored and every wanted object was converged, but the
	// node still holds objects the desired state does not want, or an
	// enumeration of what exists did not answer. It is the one thing that
	// travels in agent_reply rather than in the *Info rows, because a
	// leftover by definition has no row — nothing wanted names it.
	//
	// It is not a rejection: the worker evaluates the reply's rows exactly as
	// for code 0 and re-issues the Syncup* every round (RW12, no backoff)
	// until the code changes. "Pending" is never stored anywhere; the code is
	// recomputed by enumerating the node on every Syncup* and every Check*.
	ReplyCodeLeftover = 4

	// Seconds between background retries of a pending migration-destination
	// nvme connect (dnagent.md DN13; the loop is SH27's "DN8 retry", so
	// nicknamed for the DN8-gated converge it re-runs).
	DnMigrConnectRetryInterval = 5

	// The migration destination's wait for the source namespace after a
	// connect that succeeded (DN13 step (3)): re-read, pausing
	// DnMigrDstNsPause between reads and DnMigrDstNsWait in all. Not a
	// connect retry — one connect per pass stays the rule. time.Durations,
	// unlike the integers above them.
	DnMigrDstNsWait  = 1 * time.Second
	DnMigrDstNsPause = 50 * time.Millisecond

	// DnExportOrphanGrace is the age a namespace-less :2: export linked to
	// no port but the agent's own must exceed before a sweep removes it (DN6):
	// until then it may be a sibling agent's build in flight. The age is the
	// subsystem directory's mtime, read from the node. A time.Duration.
	DnExportOrphanGrace = 30 * time.Second

	// Side provisioning ([D15], architecture.md §9.4, dnagent.md DN9): the
	// background zeroing goroutine zeroes at most DnZeroBatchExtCnt logical
	// extents per `blkdiscard --zeroout` command, through the side's
	// dm-linear, and persists that batch's `zeroed_bits` after each success.
	// The batch size assumes fast hardware Write Zeroes: batch × ext_size
	// should zero inside CmdSoftTimeout at the disk's rate split
	// DnZeroConcurrency ways. A failed or timed-out batch is retried no
	// sooner than DnZeroRetryInterval seconds later — the zeroing twin of
	// DnMigrConnectRetryInterval, never a hot loop.
	DnZeroBatchExtCnt   = 10
	DnZeroRetryInterval = 5
	// At most DnZeroConcurrency zeroing batches run at once per agent. A
	// batch the soft timeout killed halves the side's next batch, a success
	// doubles it again up to DnZeroBatchExtCnt, and DnZeroKillBackoff kills
	// in a row drop the side to one extent per batch — rate control in the
	// side's goroutine only, never a record of what is zeroed.
	DnZeroConcurrency = 2
	DnZeroKillBackoff = 2

	// The §11.2 src-cutover grace window: a migration source's per-CN
	// dm-linears stay suspended at least this long before they are reloaded
	// onto their dm-errors — sooner where an export above one is to go (DN12,
	// [D12]).
	SuspendSeconds = 60

	// dnv DN disk format ([D13]). All byte offsets on the raw --disk device.
	DnHeaderOffset     = 0                 // 4 KiB header block
	DnHeaderSize       = 4096
	DnTableSlotAOffset = 4 * 1024 * 1024   // volume-table slot A
	DnTableSlotBOffset = 20 * 1024 * 1024  // volume-table slot B
	DnTableSlotSize    = 16 * 1024 * 1024
	DnCloneMetaOffset  = 64 * 1024 * 1024  // dm-clone metadata slot area
	DnCloneMetaSize    = 192 * 1024 * 1024 // 48 units
	DnCloneMetaUnit    = 4 * 1024 * 1024   // slot allocation granularity
	DnDataOffset       = 256 * 1024 * 1024 // extent area start (fixed!)
```

and in `common/name_fmt.go` (package-level function, not a `NameFmt` method):

```go
// DnNsIdentity derives the deterministic namespace identity that both sides
// of a leg MUST present identically (architecture.md §3.1): 16 bytes of
// sha256("dnv-ns:{cluster:%016x}:{sp:%016x}:{leg:%016x}"), rendered as an
// RFC-4122-shaped uuid string and a 32-hex-digit nguid.
func DnNsIdentity(clusterId, spId, legId uint64) (uuid string, nguid string) {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"dnv-ns:%016x:%016x:%016x", clusterId, spId, legId,
	)))
	nguid = hex.EncodeToString(sum[:16])
	uuid = fmt.Sprintf("%s-%s-%s-%s-%s",
		nguid[0:8], nguid[8:12], nguid[12:16], nguid[16:20], nguid[20:32])
	return uuid, nguid
}
```

### 2.3 Bootstrap and lifecycle

SH1. The lifecycle is **reconcile, then serve** (`architecture.md` §9.1): the
     gRPC listener MUST NOT open before the startup reconcile finished. A
     fresh node has an empty local store — an empty directory, which must
     already exist: the agent creates none, and a missing one is SH3's fatal
     case — so its reconcile is instant.

SH2. The startup reconcile runs under a ctx carrying a freshly minted trace id
     (`common.NewTraceId`), so every startup `os command` record is
     correlatable.

SH3. Reconcile returns an error only for **fatal** conditions (the local-store
     prefix unreadable); per-resource failures are captured as
     `RES_STATUS_ERROR` (§2.7) and never abort startup.

SH27. **Background tasks and process exit** (added by amendment;
      numbered last because SH rules are append-only — SH1-SH26 are cited
      from code comments and must not shift). A role server MAY run
      goroutines outside any RPC: the DN8 migration-connect retry (so
      nicknamed for the DN8-gated converge it re-runs; the retry loop itself
      is specified in DN13), the DN12
      fence timer, the DN9 side-zeroing workers, the cn's connect retry, the
      CN11 leg probers and the cn sweep's background `nvme disconnect`
      (`cnagent.md` CN10). Every one of them derives its ctx from the
      server's **`rootCtx`** — the process-lifetime ctx captured at
      `Reconcile`, which `Serve` derives from its own ctx and cancels before
      returning — and, the background disconnect aside, mints a fresh trace
      id per attempt (`common.NewTraceId`), taking the SH11 locks for the
      attempt only, never across the whole task. The background disconnect
      is one command of the pass that set it going: it carries that pass's
      trace id instead of minting one and, like the CN11 probers, takes no
      SH11 lock at all.

      A background task may additionally run a **child process**, and DN9's
      `blkdiscard --zeroout` batches are the first that does. Such a child
      must not outlive the agent — the cn sweep's background
      `nvme disconnect` (below) is allowed to — so the dn server registers
      every goroutine that owns one in a `sync.WaitGroup` before it starts
      and exposes a `WaitBackground()` that waits for them; `Serve` takes it
      as its `waitBackground` parameter, cancels the task ctx after
      `GracefulStop` and then waits. Cancellation kills the in-flight child
      through the SH15 soft/hard timeout machinery (`osclient.md` §4.2 sends
      SIGTERM then SIGKILL), so the wait is bounded by `CmdHardTimeout` per
      in-flight command — unless the child sits in an uninterruptible kernel
      wait, which no signal ends: the join then lasts until the kernel
      returns (SH15).

      **Only child-owning tasks are waited for.** The cn passes
      `waitBackground = nil`: a CN11 prober's IO is a direct, uninterruptible
      syscall (`cnagent.md` §2.2), so joining it could hang shutdown forever
      — cancelling is the whole contract there. Nor is the child of the cn
      sweep's background `nvme disconnect` joined (`cnagent.md` CN10): it
      holds nothing a restarted agent needs, a delete already in the kernel
      finishes whether or not anybody waits for it, and a join could hold
      shutdown for the kernel's whole admin timeout. Object-scoped tasks
      that hold a device open are additionally cancelled **and waited for**
      at teardown, before the resources they hold are removed (DN6, DN9): a
      live child keeps an fd on the dm device and `dmsetup remove` would
      fail EBUSY.

      One deliberate carve-out from "every goroutine that owns one": the
      DN12 fence timer's `AfterFunc` callback — which runs a full side
      converge and so can spawn children — is **not** enrolled, because the
      timer can fire after `WaitBackground` has returned and a `wg.Add`
      after `wg.Wait` panics (the enrollment sites record this). It is
      benign: the callback derives from `rootCtx`, which `Serve` cancels
      before the join, so a post-join firing finds every OS call refused by
      a dead ctx and spawns nothing.

Reference implementation — `agent/agent.go` (complete; the `waitBackground`
parameter and the derived task ctx are SH27's, `CheckRoundCtx` is SH24's):

```go
// Package agent holds the mechanism shared by the dn and cn agent roles
// (dnagent.md §2): bootstrap, the local store, the revision gate, the lock
// hierarchy, ResInfo tracking, the OS wrappers and the bitmap-chunk store.
// Policy — which dm tables, md arrays and nvmet objects to build and when —
// lives in the role packages agent/dnagent and agent/cnagent. No LVs: [D14]
// removed the clone VG, LVM's last user, so no LVM runs anywhere in dnv
// (cnagent.md §1).
package agent

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Serve runs the shared agent lifecycle (SH1-SH3): reconcile the OS to the
// local store, then listen and serve until ctx is canceled (SIGTERM/SIGINT
// via signal.NotifyContext in cmd/dnv-agent).
//
// waitBackground (may be nil) is joined after GracefulStop has drained every
// RPC, so no background goroutine it covers — and, more to the point, no
// child process one of them owns, such as the §9.4 zeroing `blkdiscard` —
// outlives the agent (dnagent.md SH27). Only a role whose background work
// holds a long-running child that must not outlive it passes one: the dn
// passes its WaitGroup join, the cn passes nil because its CN11 probers are
// stopped by cancellation and never joined (a wedged pread is
// uninterruptible, so waiting would hang shutdown forever — the very
// starvation the probe-IO carve-out exists to prevent) and the child of its
// sweep's background `nvme disconnect` holds nothing a restarted agent needs
// (cnagent.md CN10).
//
// The cancel-then-join pair is deferred, so *every* return path takes it, not
// just the one through grpcServer.Serve: reconcile has already armed the
// background goroutines (and forked their children) by the time a reconcile
// error or a net.Listen error returns, and cancellation alone does not reap a
// child — the exec.CommandContext watchdog that turns it into SIGTERM and then
// SIGKILL (SH15, osclient.md §4.2) lives in this process and dies with it, so
// the child would be reparented to init still holding its dm device open.
func Serve(
	ctx context.Context,
	network string,
	address string,
	reconcile func(ctx context.Context) error,
	register func(grpcServer *grpc.Server),
	waitBackground func(),
) error {
	startupCtx := common.WithTraceId(ctx, common.NewTraceId())
	// The role servers capture this ctx as their rootCtx, so cancelling it is
	// what stops every background goroutine. It is derived from startupCtx
	// and cancelled unconditionally — not only on SIGTERM — so a reconcile,
	// listener or serve error also winds the background down and the join
	// paired with it can never hang.
	runCtx, cancelRun := context.WithCancel(startupCtx)
	// Cancel first, then join — in that order, on every return path. The
	// background goroutines only unwind on cancellation, so joining first
	// would hang shutdown for ever.
	defer func() {
		cancelRun()
		if waitBackground != nil {
			waitBackground()
		}
	}()
	if err := reconcile(runCtx); err != nil {
		slog.ErrorContext(startupCtx, "agent reconcile failed",
			slog.String("error", err.Error()))
		return err
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	register(grpcServer)
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	slog.InfoContext(startupCtx, "agent serving",
		slog.String("network", network),
		slog.String("address", address))
	// GracefulStop has drained every RPC by the time Serve returns; the
	// deferred cancel-and-join above then stops the background workers and
	// waits for the ones waitBackground covers, so no orphan child process of
	// theirs outlives the agent (§9.4).
	return grpcServer.Serve(lis)
}

// CheckRoundCtx is the ctx one Check* round runs under (SH24): the stream's,
// re-keyed to the trace id the round's request carries. The stream's own id is
// the one its metadata brought at open (grpc.md T2) — on a worker's stream,
// the id of the round that opened it — so without this every later round's
// records would be filed under that first round. An empty id keeps the
// stream's.
func CheckRoundCtx(streamCtx context.Context, traceId string) context.Context {
	if traceId == "" {
		return streamCtx
	}
	return common.WithTraceId(streamCtx, traceId)
}
```

### 2.4 Local store — `store.go`

SH4. The store holds exactly what `architecture.md` §9.1/§4.6 prescribes: the
     last fully applied `Syncup*Request` per object and one file per received
     `Push*BitmapRequest` chunk, written via `OsClient.WriteProto` (atomic
     replace) at the `Local*Path` locations, read back via `ReadProto`.

SH5. An **object** request (`SyncupSide`/`SyncupCntlr`) is persisted after
     its converge pass completes. A **parent** request
     (`SyncupDn`/`SyncupCn`) is persisted as soon as it has passed the gates
     and become the desired state — before the converge, and so before the
     sweep that removes the resources of a pointer which has just left the
     list (DN6). The parent's pointer list is the authoritative record of
     which objects still exist, and the sweep that acts on a shortened one
     can block for a whole failfast window on a dead remote: a request
     cancelled inside that window used to skip the save entirely, and the
     next startup reconcile then rebuilt the object from the **old** list,
     against sides that no longer exist. With the new list on disk first, a
     crash mid-sweep is nothing worse than a startup sweep.
     Per-resource `RES_STATUS_ERROR` outcomes do not block persistence — the
     errors travel in the `*Info` and the worker's health loop drives repair
     (equal-revision re-syncs re-apply idempotently). Protocol rejections —
     stale revision, unknown pointer, invalid stored conf (SH9) — never
     reach persistence.

SH6. Enumeration on startup: `RunCommand(ctx, "ls", []string{"-1", prefix},
     "")` on a ctx carrying the SH15 soft timeout (the store calls the
     `OsClient` directly, so it applies that bound itself), filtering by the
     role's `Local*Path` kind prefixes (dn: `dn-`, `side-`, `migr-bm-`; cn:
     `cn-`, `cntlr-`, `clone-bm-`). File names are used only for discovery;
     the ids come from the decoded protos.

SH7. When an object's pointer leaves its parent's list, its state is dropped
     **at that moment** and nothing of it is removed from the node yet: its
     request file and its bitmap-chunk files are deleted with
     `RunCommand("rm", ["-f", …])` under the same bound as SH6's `ls`, its
     memory entry and object lock go, and its goroutines are stopped. The
     resources are found afterwards **by name**, by the sweep that runs later
     in the same pass (DN6), so no file has to be kept as a to-be-deleted
     list. Keeping it until the resources were gone is what the sweep
     replaced, and it was the opposite of idempotent: the removal pass only
     logged its failures and the file was deleted regardless, so the one
     record naming the object was destroyed exactly when the object had
     failed to go. The exception is a file no
     memory entry names — a chunk the startup reload skipped (DN2,
     `cnagent.md` CN2) and no push has rewritten since, or the `side-*` or
     `cntlr-*` file of an object it skipped that no `SyncupSide` or
     `SyncupCntlr` has rebuilt: it is not deleted when its object's
     pointer leaves the list. The sweep still removes the object's resources
     by name, and a later restart that decodes the file finds the pointer
     absent and deletes it.

### 2.5 Revision gate — `revision.go`

SH8. Per `architecture.md` §9.1, with `stored` = revision of the last fully
     applied request for the object, as this process holds it (0 when it
     holds none, as for an object whose file the startup reload did not
     load):

```go
// GateRevision returns nil when the request may be applied (incoming >=
// stored; equal means idempotent re-apply) and a rejection AgentReply for a
// stale revision.
func GateRevision(stored uint64, incoming uint64) *pb.AgentReply {
	if incoming < stored {
		return &pb.AgentReply{
			Code: common.ReplyCodeStaleRevision,
			Details: fmt.Sprintf(
				"stale revision %d < stored %d", incoming, stored),
		}
	}
	return nil
}
```

SH9. Unknown-object rejections use `ReplyCodeUnknownObject` with a `details`
     string naming the missing pointer/id. A request whose conf carries a
     value the control plane cannot have written is rejected with
     `ReplyCodeInvalidConf` and the §2.1 validator's message (DN4,
     `cnagent.md` CN8): the object is known and the revision is current, so
     it is neither of the other two, and `details` names the proto field
     rather than an id. All three rejection kinds return a
     normal gRPC reply (`AgentReply.code != 0`), never a gRPC error status —
     only `GetDnSize`/`GetCnSize` and `Get*Bm` report failure through the
     status (§9.1). A rejected request is never persisted (SH5), whichever
     kind it is. `ReplyCodeLeftover` is **not** one of these: it reports an
     accepted request whose node still holds unwanted objects (DN19), so it
     is the one non-zero code that leaves the request stored and its rows
     worth reading.

### 2.6 Concurrency — `locks.go`

The control plane guarantees per-object ordering (sequential `Syncup*` per
object, one `Push*Bitmap` in flight per migration/clone) but different
objects MAY hit the agent concurrently, and `Check*` streams run alongside
`Syncup*` (§9.6/§9.7). The agent therefore serializes with a **two-level
hierarchy**:

```go
// LockSet is the node/object lock hierarchy. Lock order is always node
// before object; object locks are created on first use and dropped only
// during teardown while the node write lock is held.
type LockSet struct { /* sync.RWMutex + map[string]*sync.Mutex */ }

func NewLockSet() *LockSet
func (l *LockSet) Node() *sync.RWMutex
func (l *LockSet) Obj(key string) *sync.Mutex
func (l *LockSet) DropObj(key string)
```

SH10. Node **write** lock: the startup reconcile and the node-level syncup
      (`SyncupDn`/`SyncupCn` — they diff the pointer list and run the
      node-level sweep, which enumerates the whole node and may remove the
      resources of any object **of this agent** on it).

SH11. Node **read** lock + the object's lock: every object-scoped RPC
      (`SyncupSide`/`SyncupCntlr`, `Push*Bitmap`, `GetSideInfo`/
      `GetCntlrInfo`, one `CheckSide`/`CheckCntlr` round) and every
      background converge attempt (the dn's connect retry — DN13; DN1's and
      SH27's "DN8 retry" — and DN12 fence timer, which share
      `reconvergeSide`, and the cn's connect retry,
      `reconvergeCntlr` — the SH27 background tasks that converge; CN1).

SH12. Node **read** lock only: node-scoped reads (`GetDnInfo`/`GetCnInfo`,
      one `CheckDn`/`CheckCn` round). `GetDnSize`/`GetCnSize` take no lock.

SH13. Lock order is node → object, never nested object locks. Probing under a
      lock is acceptable: every OS command is bounded by `CmdSoftTimeout`/
      `CmdHardTimeout` (§2.8 SH15) except a child in an uninterruptible
      kernel wait (SH15) — which is why the cn sweep's `nvme disconnect`
      runs off the locks (`cnagent.md` CN21) — and an in-process `OsClient`
      file or block call is bounded only up to its syscall: its ctx is
      checked once, before it (`osclient.md` §4.3), so a sysfs, configfs or
      device access the kernel holds returns only when the kernel does.

### 2.7 `ResInfo` tracking — `resinfo.go`

SH14. A per-object in-memory tracker turns probe outcomes into
      `pb.ResInfo{res_name, status, details, epoch}` per `architecture.md`
      §9.5: `epoch` = unix seconds of the last **status** change (a `details`
      change alone does not bump it); the agent emits
      `MISSING`/`ERROR`/`OK`/`PROVISIONING`, the cn agent also `PENDING` on
      a primary's leg rows (`cnagent.md` CN11), and never `UNKNOWN`
      (worker-only). *Amended 2026-09-26:* `RES_STATUS_PENDING` means *the
      primary's prober for the leg has not completed a round since it
      started (a build, a promotion or an agent restart) — no verdict*, and
      it neither sets nor clears `Leg.err_epoch` (§9.5, §10.3); the row read
      `OK` before, and an `OK` clears it, so every promotion cleared a dead
      leg's. `RES_STATUS_PROVISIONING` means
      *deliberately not created yet, healthy, no action needed*: it is what a
      resource waiting behind DN9's provisioning gate reports, and unlike
      `RES_STATUS_ERROR` it never feeds `err_epoch` (§9.5, §10.2-§10.4).
      A resource that leaves the desired state must lose its history, or the
      next object built with the same id would inherit a dead one's epoch and
      read as unchanged since. An object whose own converge derives a
      per-resource key set — a side (DN6), a cntlr (`cnagent.md` CN9) —
      therefore has its tracker pruned the way its devices are: `Keep(wanted)`
      drops every key that set does not name, derived from the wanted set
      rather than from a diff against a remembered plan. Tracker state is
      in-memory; after a
      restart epochs restart at the reconcile time — acceptable, the epoch
      means "last observed change".

### 2.8 OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`

Thin, mechanism-only wrappers over the process's single
`common.NewLimitedOsClient(...)` instance. They implement exactly the
Appendix A command patterns; policy (which device, which table) stays in the
role packages.

**A reload fails closed** (*decided 2026-09-29*). `Dm.Reload` and
`Dm.ReloadMulti` swap a live device's table in three commands — `dmsetup
suspend`, `dmsetup reload` (the table on stdin for `ReloadMulti`), `dmsetup
resume` — and return at the first one that fails. A reload whose load fails
therefore returns that error with the device still **suspended** on its old
table: the wrapper does not resume it. One whose resume fails can leave it
suspended too — on its old table or on its new one, depending on where the
resume failed — and so can one whose suspend was killed after the kernel
had carried it out (SH15). That is deliberate. On the dn, a reload that
moves a per-CN dm-linear onto its dm-error is a fence — the old primary's
linear on a primary flip (DN10), DN12's phase 2, the step before the
sweep's first layer (DN6) — and resuming the old table after a failed load
would leave the old primary live on the side device beside the new one, or
replay onto the side's data the IO the cutover window absorbed. Suspended,
the device serves nothing and writes nothing; the cost is liveness. Its
bios queue with no timeout ([D12]) until a later reload or resume of it
succeeds. On the dn, the build phase reloads a device whose live table is
not the one it wants and resumes one whose live table is, a fenced linear
inside its cutover window aside; the step before the sweep's first layer
reloads onto its dm-error a suspended linear it is about to remove or
unexport; and the sweep of a side that plays no migration source resumes
each per-CN dm-linear of the side it finds suspended, on whatever table is
live (DN12 rule 2). The cn's paths are
`cnagent.md` CN16's. That rule-2 resume undoes the flip's fence (*known
limit, 2026-09-29*): when a flip's reload of the old primary's linear fails
its load, the linear stays suspended on its old table only until a later
converge of the side resumes it there, releasing onto the side's data the
IO it queued, ahead of any retry of the reload in that converge's build
phase. The rule is the wrappers', so it holds for every reload of either
role, the ones that fence nothing — a grow, a repoint — included, and
[D12]'s bound on a suspension holds only as far as the reloads succeed.

SH15. Every wrapper call wraps its ctx with
      `context.WithTimeout(ctx, common.CmdSoftTimeout*time.Second)` before
      calling `OsClient` (the `architecture.md` §7 soft/hard timeout contract; `osclient.md`
      §4.2 handles SIGTERM/SIGKILL). This covers the raw-device
      `ReadBlock`/`WriteBlock` calls of the [D13] metadata path too. A role
      package that calls the `OsClient` directly instead of through a
      wrapper — the `cnagent.md` CN12 sysfs leg walk, and CN25's read of
      the `thin_dump` file — takes the same bound from the exported
      `agent.CmdCtx`. The bound holds in full only for a
      child the signals end. An in-process `OsClient` call — a file, proto
      or block read or write — is bounded only until its syscall starts:
      its ctx is checked once, before it (`osclient.md` §4.3). A child
      blocked in an uninterruptible kernel wait is not bounded at all: both
      signals are delivered, and the call returns, and gives back its
      `OsClient` slot, only when the kernel does (an `nvme disconnect` whose
      target vanishes mid-delete waits out the kernel's 60 s admin timeout;
      `cnagent.md` CN21 runs the cn sweep's off the locks, while the cn
      build's dead-path disconnect and the dn sweep's `:3:` one still run
      under them — `cnagent.md` Known limits).

      **A command that was killed did not answer, and "did not answer" is not
      "absent".** `OsClient.RunCommand` returns `exitCode == -1` with a
      non-nil error when the process never reported — killed at the soft
      timeout, killed at the hard timeout, failed to start, ctx cancelled,
      semaphore refused — and `exitCode > 0` when the tool ran and answered
      "no". `agent.Reported(exitCode, err)` (`err == nil || exitCode > 0`) is
      the single test, and `osBase.runProbe` is the one probe wrapper that
      applies it: a probe that did not answer returns an **error**, never a
      "not there". The distinction is not cosmetic. A killed command may still
      have completed in the kernel — the ioctl finishes regardless of the
      signal — so the only thing a caller learns from a kill is that it must
      probe again; and a caller that reads a kill as "absent" skips the
      removal, forgets the object and never enumerates it again, which is the
      leak the sweep exists to end (DN6). `agent.Reported`'s one other
      caller, `Dm.BlkZeroout`, is not a probe: it hands the verdict back as
      `answered`, so DN9's zeroing loop can tell a batch the soft timeout
      killed from one the tool refused.

      These primitives must honour it, because each one's caller answers an
      "absent" with a removal, with a decision that can destroy data, with —
      `CloneMeta.Mounted`'s — a mount that sends every clone on the CN into
      a rebuild, or — `dirMtime`'s — with a clean verdict:
      `Dm.Info` (`agent/dm.go`, whose `nil, nil` gates `meta.FreeSide` and
      `meta.FreeCloneMeta` — see DN6's record rule), `osBase.listDir` and the
      `dirExists` above it (`agent/oswrap.go`, which `Nvmet.RemoveSubsystem`
      and `RemovePortLink` walk their children through), `osBase.dirMtime`
      beside them (the `stat -c %Y` behind `Nvmet.SubsysMtime`, DN6's
      orphan-export age: a subsystem directory that reads absent is passed
      over and is no leftover, so a `stat` that did not answer is named as a
      failed enumeration instead, and the verdict is not clean), `Md.Detail` —
      since 2026-09-26 a lookup in an `Md.Walk` of `/sys/block`, through
      `listDir` and `readAttrStrict` (`cnagent.md` CN12), whose "absent"
      sends `ensureGroup` into an assembly — `Md.HasSuperblock`
      (`cnagent.md` CN12: `--examine` opens the member device, so on a leg
      whose DN side is gone it blocks past the soft timeout and gets killed
      — and a killed `--examine` read as "no superblock" would make
      `assembleGroup` run `mdadm --create --assume-clean` over live data),
      `Md.NameInUse` (`cnagent.md` CN12, since 2026-09-26: an `lsblk`
      of `/dev/md/{CnMdDevName}` whose "not in use" is what lets
      `assembleGroup` run `mdadm --create --assume-clean` beside an array
      already running under that name), and `CloneMeta.Mounted` and
      `CloneMeta.FileSize` (`cnagent.md` CN5, since 2026-09-29: the two
      `findmnt` calls of the tmpfs probe, whose "absent" is answered with a
      `mount`, which over the live clone-metadata arena stacks a second,
      empty tmpfs that hides it — a fresh arena file and a second loop
      device follow, and every clone on the CN is rebuilt — and the `stat`
      of the arena file, whose "absent" is answered with a
      `truncate --size {CnCloneMetaAreaSize}` of it: a no-op on the live
      arena file, which the converge only ever creates at that size, but a
      resize of one of any other size — on a shrink, the loss of its tail —
      where the converge otherwise reports an `ERROR` and leaves the file
      alone).
      `CloneMeta.LoopDevices` (`cnagent.md` CN5) honours it without the split:
      `losetup --associated` answers "none attached" with exit 0 and no
      output, so every failure of it is an error — a "none" read off a
      failure would attach a second loop device to the arena file.
      `NvmeHost.readTrimmed` follows the same rule from the other side of
      the fence: it reads sysfs rather than running a tool, so **only**
      `fs.ErrNotExist` is "absent" and every other read failure is an
      error — `/sys/class/nvme*` can stall while a
      controller is mid-reset, and a stalled read taken as absence would
      report a live subsystem as not connected (SH20). The read under it,
      `osBase.readAttrStrict`, carries the rule for every other strict read
      besides `Md.Detail`'s above:
      `Nvmet.NsDevicePath` (a namespace's `device_path`, by which the cn
      sweep attributes a host-facing subsystem no stored request claims:
      one with no attributable namespace is unowned, and the node-level
      sweep removes it — `cnagent.md` CN21 — and by which DN6 attributes a
      `SideToCnNqn` export: an export whose read did not answer is foreign
      for that pass and named as a failed enumeration), the
      `enable` reads of `Nvmet.RemoveNamespace` and `RemoveSubsystem` (an
      "absent" would skip the `enable = 0` write and `rmdir` a namespace
      the kernel still has enabled) and, through `Cmd.ReadAttr`, the md
      sysfs reads of `Md.ListArrays` and `Md.Gone` (`cnagent.md` CN12: an
      array read as absent, or as gone, would let the sweep disconnect a
      leg under a live array).

SH16. **Convergence is probe-first.** Every `Ensure*` helper reads current
      state and mutates only differences; an equal-revision re-apply on a
      live, converged object issues no mutating command. This is what makes
      re-applies safe: gratuitous re-writes of live objects are not no-ops
      (re-linking a live port↔subsystem link or reloading a live dm table
      stalls host IO).

SH17. Probing follows the Appendix A conventions: `dmsetup status`/`dmsetup
      table`/`dmsetup ls`, the `/sys/class/nvme-subsystem` +
      `/sys/class/nvme` walk for every nvme host fact — the namespace
      device, controller liveness and ANA state alike (SH20,
      `cnagent.md` CN12/CN28) — configfs **reads** for nvmet, and
      `ReadBlock` of the disk header for the DN's [D13] metadata. No LVM
      report is probed anywhere any more ([D14]). Probe
      reads MUST tolerate padded/
      normalized read-back; compare canonically, never byte-wise:

      * `attr_serial` reads back space-padded — compare trimmed.
      * `device_uuid`/`device_nguid` read back **dash-separated and
        lower-cased** whichever of the two accepted forms was written
        (the kernel prints them with `%pUb`), while `DnNsIdentity` supplies
        the uuid dashed and the nguid bare. Both roles compare them through
        the one shared `agent.SameNsId` (strip `-`, fold case). A byte-wise
        comparison reports a permanent difference on the nguid, which makes
        the converge disable the namespace, rewrite the attribute and
        re-enable it on **every** pass — an SH16 violation that drops a live
        export's namespace once per Check round — and makes every probe
        report the healthy namespace `RES_STATUS_ERROR`.

SH18. nvmet configfs access: attribute writes go through
      `OsClient.WriteFileDirect` (added to `osclient.md` by this document —
      the atomic replace of `WriteFile` cannot work on configfs); directory
      and link operations go through `RunCommand` (`mkdir`, `rmdir`,
      `ln -s`, `rm`); probing through `ReadFile`. `WriteFile` MUST NOT be
      used on `/sys/kernel/config` paths.

SH19. `nvmet.go` owns the [D4] fixed-ANA-group model:
      * `EnsurePort(portId, conf)` creates `ports/{portId}` — the calling
        agent's `--nvmet-port-id`, `NvmetPortId` = 1 by default — with that
        agent's `NvmeTrConf` attributes **and** the three fixed groups:
        `mkdir ana_groups/2`, `mkdir ana_groups/3`, then write `optimized` /
        `non-optimized` / `inaccessible` into groups 1/2/3 exactly once
        (probe-first: skip when already correct). No code path ever writes an
        `ana_state` after that. Probe-first is also what lets two agents on
        one node co-own one port id (`architecture.md` §3.1): the second one
        finds every attribute already as it wants it and writes nothing.
        That holds only while their `--tr-*` values agree — two agents
        sharing a port id with different transports would each try to
        rewrite the other's `addr_*` every round — so agents that need
        different transports need different `--nvmet-port-id`s.
      * every ANA transition is `SetNsAnaGrpId(nqn, nsid, grpid)` — a single
        `WriteFileDirect` of the namespace's `ana_grpid`, valid on a live
        namespace because the target group always exists.
      * subsystem/namespace lifecycle helpers follow the Appendix A order;
        teardown is reverse order (port link removed first, then ns disable,
        rmdir ns, allowed-hosts unlink, rmdir subsystem). Absent objects are
        skipped so a re-run after a crash is a no-op, but the `enable` read
        that decides whether the disable is needed is a **strict** one
        (SH15): a read that did not answer must not let the pass skip
        `enable = 0` and then `rmdir` a namespace the kernel still has
        enabled. `RemoveSubsystem` and `RemoveNamespace` both read it that
        way, and both propagate the error instead of continuing.

SH20. `nvmehost.go`: `Connect` always passes
      `--fast_io_fail_tmo {DefaultNvmeFastIoFailTmo} --ctrl-loss-tmo -1`,
      the caller's hostnqn, and `--hostid common.NvmeHostId(hostnqn)` —
      never the node-wide `/etc/nvme/hostid`, which the kernel's 1:1
      hostnqn↔hostid rule turns into an `EINVAL` the moment anything else on
      the node holds it (architecture.md Appendix A). The underscores in
      `--fast_io_fail_tmo` are nvme-cli's own spelling and must not be
      "fixed" to dashes. `Disconnect --nqn`; `ListSubsys` reads **sysfs**
      (`/sys/class/nvme-subsystem`, `/sys/class/nvme`) for the subsystem's
      namespace device, controller liveness and per-path ANA state —
      `nvme list-subsys -o json` carries none of the three, listing no
      namespaces at all and no `ANAState` without a namespace block device
      argument (`cnagent.md` CN12/CN28, which reads the same tree for legs).
      Every read of that walk carries the SH15 soft timeout like any other
      OS touch: unlike most of sysfs, `/sys/class/nvme*`
      can stall while a controller is mid-reset or being torn down, which is
      exactly when these probes run, and no converge — nor, through the
      DN1/CN1 locks, a whole node's RPC surface — may be held on one. The
      bound is on the read's own scheduling, not a magic abort of a read
      already blocked inside the kernel.
      `DisconnectDevice` (added by `cnagent.md` §2.3) is
      `nvme disconnect --device {ctrl}` for retiring **one** controller of an
      NQN whose other paths must live — the two sides of a migrating leg
      share a subsystem NQN ([D1]), so the cn agent cannot use `--nqn` to
      drop a dead side. The controller device is found by the **sysfs walk**,
      never by `list-subsys`: the
      `/sys/class/nvme-subsystem/nvme-subsys*` directory whose `subsysnqn`
      equals the NQN holds the `nvme{N}` controller entries, and one is
      selected by reading `/sys/class/nvme/{ctrl}/address` and parsing it as
      comma-separated `key=value` pairs (`traddr=…,trsvcid=…`) to match the
      dead side — never by field position. A controller whose `address`
      did not answer is never selected — it is unknown, never unwanted —
      and neither is one whose `address` is absent, a controller already
      deleted (`cnagent.md` CN10).

### 2.9 Bitmap-chunk store — `bitmap.go`

SH21. Implements the agent side of §9.6: persist the received
      `Push*BitmapRequest` verbatim at its `Local*BmPath` (via `WriteProto`)
      **before** applying; the applied set is derived from the files
      present, save those the startup reload leaves unloaded (below) —
      `BitmapInfo.bm_idx_list` for a migration, keyed by the append
      index, and `BitmapInfo.chunk_id_list` for a clone, keyed by the
      `(src_slice_idx, bm_idx)` pair that addresses the chunk. The file name
      carries that pair too, but only as an address: the persisted request's
      CONTENT is what the startup reload decodes it from. A file that reload
      leaves unloaded — one that does not decode, and DN2's skips while a
      `dn-*` or `side-*` file does not load — is in no applied set until a
      push rewrites it or a later restart loads it.

SH22. The §11.4 math skeleton lives here: chunk placement (concatenated —
      migration — or self-positioned `(src_slice_idx, bm_idx)` chunk of fixed
      capacity `CloneBmChunkBytes` — clone) and the fully-skippable-region →
      `blkdiscard` range computation. The single wire-convention inversion
      (**wire 1 = unwritten/skippable**) is *not* here: the one place that
      reads the "written/copied = 1" side of the convention is the thin-pool
      metadata reader in `agent/cnagent/thinbm.go`, so that is where it
      inverts, exactly once, and every chunk reaching this file is already in
      wire convention. Role packages supply only the positioning parameters
      (the dn shifts by the leg's `meta_blocks` first, §9.6).

SH23. Migration chunks are interpretable only as a contiguous prefix from
      `bm_idx = 0`; the apply computation uses the longest contiguous prefix
      of the applied set's chunks (SH21) — the worker's ascending,
      one-in-flight push makes gaps unreachable in practice. The applied set
      still reports every chunk in it, not only that prefix.

### 2.10 Check-stream rules

The `Check*` handlers are role code (the stream types differ), but MUST all
follow these rules (`architecture.md` §9.7):

SH24. Rounds are worker-initiated: loop on `Recv` (a `Recv` returning
      `io.EOF`, or the stream ctx ending, ends the handler with `nil`);
      exactly one `Send` per received request; never an unsolicited send.
      Each round runs under the trace id its request carries in `trace_id`
      (`agent.CheckRoundCtx`): the stream ctx holds only the id its
      metadata brought at open (`grpc.md` T2) — on a worker's stream, that
      of the round that opened it (T3) — so without it every later round's
      records would carry that first round's id. An empty `trace_id` keeps
      the stream ctx's id.

SH25. Per round, under the SH11/SH12 locks: validate the object (unknown ⇒
      reply `agent_reply.code = ReplyCodeUnknownObject`, `revision = 0`, no
      info — the stream stays open), probe the **fresh** live state, reply
      `revision` = last fully applied revision.

SH26. Info inclusion: always when `show_info = true`; when `false`, on the
      first reply of the stream and whenever the freshly probed `*Info`
      differs (`proto.Equal`) from the last info actually sent on this
      stream. Otherwise the info field is left unset.

## 3. `cmd/dnv-agent` — cobra + viper [shared]

CM1. The binary is a cobra root command `dnv-agent` with exactly two
     subcommands, `dn` and `cn`. cobra and viper live in `cmd/` and in
     `ctl/`, dnvctl's command tree (`dnvctl.md` §1.2);
     `github.com/spf13/cobra` and `github.com/spf13/viper` enter `go.mod`
     with this binary.

CM2. Flags (`architecture.md` §13; every flag is also settable via config
     file and environment through viper):

| flag | dn | cn | default | meaning |
|---|---|---|---|---|
| `--grpc-network` | ✓ | ✓ | `tcp` | `net.Listen` network |
| `--grpc-address` | required | required | — | gRPC endpoint; the CP stores it as `DnConf`/`CnConf` `addr_port` |
| `--tr-type` / `--adr-fam` / `--tr-addr` / `--tr-svc-id` | required | required | — | this agent's single nvmet port (`NvmeTrConf`), mirrored into `DnConf`/`CnConf` at creation |
| `--nvmet-port-id` | ✓ | ✓ | `NvmetPortId` (1) | configfs id of the nvmet port this agent converges (`/sys/kernel/config/nvmet/ports/{id}`); several agents on one node's kernel take distinct ids (`architecture.md` §3.1). Env `DNV_AGENT_NVMET_PORT_ID`; a value below 1 is refused (CM3) |
| `--local-store` | ✓ | ✓ | `DefaultLocalStorPrefix` (`/var/lib/dnv`) | `localStorPrefix` of `common.NewNameFmt` (`architecture.md` §4.6 state files); the directory must exist before the agent starts (SH3) |
| `--disk` | required | — | — | the raw block device that carries the dnv disk format ([D13]; §4.1 `diskmeta.go`) |
| `--capacity` | — | ✓ | 0 | capacity budget in bytes this CN is willing to host; `GetCnSize` replies it verbatim, 0 = "use the CP default" (added by `cnagent.md` §3) |
| `--config` | ✓ | ✓ | — | optional viper config file |

     Running several agents on one node takes more than distinct port ids.
     Each needs its own `--grpc-address`; two agents of the **same role**
     also need their own `--local-store`, and the dn role needs its own
     `--disk`. The `--tr-*` values follow the port id rather than the agent:
     agents on **distinct** port ids need distinct `--tr-addr`/`--tr-svc-id`
     pairs, because an agent's `NvmeTrConf` is what a CN dials to reach a
     dn's sides and what a host dials to reach a cn's namespaces; agents
     that **co-own** one port id — the dn/cn pair of `architecture.md` §13,
     both on `4200` — must instead pass identical `--tr-*` values, since
     they converge the same `addr_*` files (SH19).

     The `--local-store` rule is the one the file names mislead about. The
     `architecture.md` §4.6 names do carry the owning node's id — `dn-` /
     `side-` / `migr-bm-` key on `(cluster_id, dn_id)`, the cn role's `cn-` /
     `cntlr-` / `clone-bm-` on `(cluster_id, cn_id)` — but the store is never
     read back by name: SH6 enumerates it with `ls -1 {prefix}` and filters
     on the role's three **kind** prefixes alone, with no id filter. Two dn
     agents sharing a prefix would therefore each load and converge the
     other's DN and side records at startup (SH1, DN2): each would allocate
     the other's side out of its **own** `--disk`, since the volume table is
     keyed by `(sp_id, side_id)` alone (DN9), and then link the other's
     `SideToCnNqn` subsystem into its **own** port. The prefix, not the file
     name, is the unit of ownership. A dn and a cn agent may share one,
     because the two roles' kind prefixes are disjoint — which is what lets
     the `architecture.md` §13 pair both run on `/var/lib/dnv`.

     One name a dn agent builds is **not** keyed by `dn_id` and names an
     object two agents would fight over: the side subsystem NQN.
     `SideToCnNqn` is keyed by `leg_id` (`architecture.md` §4.4), and nvmet
     subsystems live beside the ports rather than under them, so two dn
     agents in one kernel must never hold the two sides of one leg. Only a
     migration ever gives a leg two sides, so this is a placement matter
     rather than an agent-flag one: register every DN of one kernel under the
     same failure domain (`dnvctl dn create --location`, `architecture.md`
     §8.2) and `architecture.md` §6.5's tier-1 anti-affinity keeps a
     migration destination off its source's kernel. `architecture.md` §3.1
     carries the full rule and its caveat — tier 2 relaxes that exclusion
     rather than refusing to place. Two further dn-built names carry no
     `dn_id` and are harmless: `CnHostNqn(cluster, cn)`, whose kernel-global
     `hosts/{nqn}` directory both agents merely create with `mkdir -p` and no
     code path anywhere removes, and the ns identity
     `DnNsIdentity(cluster, sp, leg)`, which lives inside the subsystem the
     NQN above already covers.

     The cn role has the mirror-image rule, and a stricter one: run at most
     **one** cn agent per kernel. Three cn names carry no cn id at all — the
     host-facing subsystem NQN is the one the user passed `CreateSubsystem`,
     so every cntlr of that SP exports the identical subsystem
     (`architecture.md` §3.3 step 6, §3.5, §11.8);
     `XferNqn(cluster, sp, xfer)` carries no node id
     (`architecture.md` §4.4); and `CnMdArrayName(sp, slice, grp)`, the
     `mdadm --name` superblock name, carries neither cluster nor cn id
     (`architecture.md` §4.3). Nor is the placement rule the dn falls back on
     enough here: `architecture.md` §6.5 keeps the cntlrs of one SP in
     distinct `location`s only at tier 1, so two cn agents of one kernel
     registered under one `location` can still take two cntlrs of an SP once
     a pick finds no CN with room outside the domains that SP's cntlrs
     already hold (which any SP with more cntlrs than the cluster has
     domains with room reaches), an automatic cntlr replacement may land on
     the other cn agent of the failed cntlr's kernel even at tier 1
     (`dnv-worker.md` AR7 excludes only the surviving cntlrs' domains),
     two left at the default `location` are simply two CNs to the allocator,
     and where the dn's collision needs a migration in flight, the cn's needs
     no more than an SP with more than one cntlr, or an automatic replacement
     of any SP's cntlr.

CM3. Viper binding per subcommand: `viper.BindPFlags(cmd.Flags())`,
     `viper.SetEnvPrefix("DNV_AGENT")`,
     `viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))`,
     `viper.AutomaticEnv()`; when `--config` is set, `viper.SetConfigFile` +
     `ReadInConfig`. All value reads go through viper (so file/env win per
     viper precedence).

     Two checks run **after** that resolution rather than through cobra's
     own flag machinery, so that a config file or an environment variable is
     subject to the same rule as a flag: `requiredCommon` (the five values
     both roles must have, plus `--disk` for dn), and the `--nvmet-port-id`
     floor. Below 1 the subcommand's `RunE` returns
     `--nvmet-port-id must be >= 1, got {n}`; the root command sets
     `SilenceUsage`, so `dnv-agent` exits **1** with no usage dump, printing
     that message **twice** — cobra's own `Error: …` line plus `main`'s
     `fmt.Fprintln(os.Stderr, err)` — which is what anything grepping the
     output sees. It is the same rule, not the same validation: a
     non-numeric **flag** never reaches the floor, because pflag rejects it
     at parse time (`invalid argument "abc" for "--nvmet-port-id" flag:
     strconv.ParseInt: …`), while `viper.GetInt` turns any non-numeric env
     or config value — `abc`, `2abc`, `" 3"` — into 0, which the floor then
     reports as `got 0`. An **empty** env var is ignored by viper, so
     `DNV_AGENT_NVMET_PORT_ID=` leaves the default standing. Every flag's
     env spelling is the prefix plus the flag with `-` replaced by `_`:
     `DNV_AGENT_NVMET_PORT_ID`, `DNV_AGENT_TR_SVC_ID`, and so on.

CM4. Each subcommand's `RunE`: bind viper, then run CM3's two checks —
     `requireValues`, and the `--nvmet-port-id` floor whose resolved value is
     carried on to the constructor; `signal.NotifyContext(
     context.Background(), syscall.SIGINT, syscall.SIGTERM)`; construct
     `common.NewNameFmt(localStore)` and the process's **single**
     `common.NewLimitedOsClient(0)`; build the role server
     (`dnagent.NewDnAgentServer(...)` / `cnagent...`); call `agent.Serve`
     with the role's reconcile, a register func that calls
     `pb.RegisterDiskNodeAgentServer` (resp. `...ControllerNode...`), and the
     role's SH27 background waiter — the dn passes `srv.WaitBackground`, the
     cn passes `nil` (its probers are cancelled, never joined, and its
     sweep's background `nvme disconnect` is not joined either — SH27).
     Transport is plaintext (`grpc.md` §4); log level stays the default Info
     (`log.md` R6 — the agent calls nothing).

Sketch — `cmd/dnv-agent/main.go` (shape, not verbatim):

```go
func main() {
	root := &cobra.Command{Use: "dnv-agent"}
	root.AddCommand(newDnCmd(), newCnCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newDnCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dn", RunE: runDn}
	addCommonFlags(cmd)               // CM2 shared rows
	cmd.Flags().String("disk", "",
		"raw block device that carries the dnv disk format (required)")
	// no MarkFlagRequired: "required" is checked after the viper binding,
	// in runDn, so a config file or env var satisfies it too (CM2/CM3)
	return cmd
}

func runDn(cmd *cobra.Command, args []string) error {
	bindViper(cmd)                    // CM3
	requireValues(append(requiredCommon, "disk")...) // the CM2 required rows
	portId, _ := nvmetPortIdFromViper()              // CM3's second check
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	oc := common.NewLimitedOsClient(0)
	srv := dnagent.NewDnAgentServer(oc, nf, localStore,
		viper.GetString("disk"), trConfFromViper(), portId)
	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Reconcile,
		func(g *grpc.Server) { pb.RegisterDiskNodeAgentServer(g, srv) },
		srv.WaitBackground)          // SH27; the cn passes nil
}
```

## 4. The dn role — package `dnagent` [dn]

### 4.1 Files

`server.go` (the `DnAgentServer` type, lock mapping, the RPC entry points other than the check streams of `check.go`),
`diskmeta.go` (the [D13] on-disk format: header, A/B volume-table slots,
extent and clone-metadata allocators), `syncup_dn.go`, `syncup_side.go`,
`plan.go` (the per-side desired-state plan derived from the request),
`migr.go` (the §11.2 source/destination choreography and the DN13
connect-retry registry), `fence.go` (the DN12 two-phase fence: window
bookkeeping, timer, adoption, settle), `zeroing.go` (the DN9
side-provisioning registry and its `blkdiscard --zeroout` batches),
`push_migr_bm.go`,
`sweep.go` (DN6: the node's enumeration, the two scopes' wanted sets, the
claim rules, the layers and the record gate), `check.go`, `probe.go`
(DnInfo/SideInfo probing). Colocated `_test.go` files.

### 4.2 Server type and lock mapping

```go
type DnAgentServer struct {
	pb.UnimplementedDiskNodeAgentServer
	nf    *common.NameFmt
	store *agent.Store    // the OsClient-backed wrappers — no raw oc field:
	meta  *DiskMeta       // local store, disk metadata, dm, nvmet, nvme host
	dm    *agent.Dm
	nvmet *agent.Nvmet
	host  *agent.NvmeHost
	locks *agent.LockSet  // object key = LocalSidePath id tuple
	disk  string          // --disk
	port  agent.PortConf  // --tr-* + --nvmet-port-id as one value
	// per-object resinfo trackers, pending-connect retry registry,
	// per-side zeroing registry and its zeroing slots (DN9), and the SH27
	// background-task bookkeeping: the rootCtx captured at Reconcile plus a
	// sync.WaitGroup that WaitBackground() waits on, …
}
```

DN1. Lock mapping (instantiates SH10-SH13): `SyncupDn` and the startup
     reconcile — node write; `SyncupSide`, `PushMigrBitmap`, `GetSideInfo`,
     one `CheckSide` round, one DN8 retry attempt — node read + that side's
     object lock (key = `(cluster_id, dn_id, sp_id, side_id)`); `GetDnInfo`
     and one `CheckDn` round — node read; `GetDnSize` — no lock.

### 4.3 Startup reconcile

DN2. Enumerate the store (SH6). For each `dn-*` file: re-run the SyncupDn
     converge (§4.5 steps 2-3) from the stored request — **unless** its
     `extent_size` is 0, which only a build older than DN4's conf gate can
     have persisted. Such a file is **loaded** into the in-memory DN set
     exactly as read — not skipped like an unreadable one — and refused by
     `convergeDn` instead, once per pass: a zero extent size is not a
     geometry any converge may run on, so the converge stops before DN5
     touches anything (no `lsblk`, no header read, the port left as found)
     and reports the `architecture.md` §7 conf fault as one `Error` record (msg `"invalid
     stored conf"`, naming the cluster and the dn). That record is the one
     place the field is named: the `meta_info = RES_STATUS_ERROR` that
     `convergeDn` hands back has no reply to travel in at startup, and the
     next `GetDnInfo`/`CheckDn` round re-probes the header against the zero
     and reports the meta row as `"foreign disk: … want …/0"` on a
     formatted disk, `"unformatted disk"` on a blank one. Refusing
     there rather than converging is what keeps the fault legible: DN5's
     `EnsureFormatted` cannot confirm this disk against a zero (a formatted
     one answers `"foreign disk: …"`, a blank one `"extent_size is 0"`), so
     a converge would bury the conf fault under an identity error on every
     side of that DN instead of one record naming the field. Loading rather
     than skipping is what keeps the fault harmless: with no DN state in
     memory and no `dn-*` file left unread, every `side-*` file of that node
     takes the pointer-absent branch below, and its request and its bitmap
     chunks are deleted on the spot (SH7) — the agent would forget every
     side it hosts, and every bitmap chunk it holds for them, because one
     field of its parent is zero. It
     would not even remove their resources in exchange: a DN that was never
     loaded gets no node-level sweep of its own (DN6), and no other DN's
     sweep reaches them either — that sweep's enumeration is filtered to its
     own dn id, and an export whose namespace names another node's kind-`d1`
     linear reads as **foreign** — so the dm devices, exports and records
     would simply sit there with nothing on the node naming them. A conf
     fault must destroy neither the desired state nor the resources.
     So every side still in that DN's stored pointer list is left **exactly
     as the restart found it**, neither converged nor swept: its
     exports, dm devices, store file and extent record stay, and its stored
     `migr-bm-*` chunks are loaded but applied to nothing. A side whose
     pointer already left the list still has its **state** dropped as usual
     (SH7) — the pointer check runs before the conf check — but the
     node-level sweep of that DN is skipped along with everything else of it,
     so its dm devices and its records are not removed until the conf is
     fixed: a leak, which is the side of the trade a conf fault is allowed to
     fall on. The record step would free nothing here even if it ran: it
     needs a confirmed disk, and a disk is confirmed only against the
     identity a DN5 converge has asked for, which this one stopped before
     asking (the probe above hands `DiskMeta` no identity). There
     is no compat shim: a cluster whose stored conf carries zeros is refused
     loudly everywhere and has to be recreated. A `dn-*` file that does not
     load — its read fails or it does not decode — is skipped, and while one
     is unread, so is every `side-*` file and `migr-bm-*` chunk whose DN is
     not loaded: a file that did not load names no DN (SH6), so any of those
     sides may be one its list still names, and a list that could not be
     read proves nothing about which sides left it. Skipped is neither
     loaded nor deleted: none of them is converged, swept or applied, and
     every one of those files stays on disk exactly as the restart found it
     — loaded, each side would reach the pointer-absent branch below, which
     would delete its request and its chunks for want of a list that could
     not be read. Nor is anything hidden: with none of it in memory, the DN
     and each of those sides answer their Check rounds
     `ReplyCodeUnknownObject` (SH25), and the worker re-sends each `Syncup*`
     (`dnv-worker.md` RW4). The `SyncupDn` rewrites the file, and from there
     each skipped side the list still names is where a lost `--local-store`
     leaves one — known by its pointer alone (DN6) — until its `SyncupSide`,
     which DN8 admits only after that `SyncupDn`, converges from the request
     it carries and rewrites its file; that reply's `bm_info` acknowledges
     no chunk, so the worker pushes each chunk of the migration again
     (DN15). A skipped side the re-sent list no longer names is in no
     list and not loaded, so that DN's node-level sweep removes its resources
     by name (DN6); its files wait for a later restart, whose pass finds its
     pointer absent and drops them (SH7). The order over the rest of the
     store is: every side whose pointer is absent from its DN's stored
     `side_pointer_list` is dropped, then each usable DN's node-level sweep
     removes what those sides left behind by name, then the remaining
     `side-*` files re-run the SyncupSide converge (§4.6) from the stored
     request — a converge that finds not-yet-zeroed extents
     (re)starts that side's DN9 zeroing goroutine, which is how provisioning
     resumes after a restart. Then re-apply every `migr-bm-*` chunk (SH21-
     SH23) — except the orphans, which are **deleted** here: a chunk not
     skipped with its DN whose side is not loaded, and a chunk whose
     `migr_id` no longer matches that side's stored `migr_dst_conf`. A chunk
     whose side is not loaded is no orphan while a `side-*` file does not
     load and the chunk's loaded DN still names its side: that file names no
     side, so it may be this side's, and the chunk is skipped like the files
     of a DN that did not load — neither loaded nor deleted. The chunk's
     side is then where a lost `--local-store` leaves one, as above, until
     its `SyncupSide` rewrites its file and the worker pushes the chunk
     again. A chunk whose loaded DN no longer names its side is an orphan
     whatever `side-*` file failed to decode: the side has left the list,
     and its chunks go with it (SH7).
     Every other deletion — with the side that owns them (DN6, SH7), on a
     destination's `migr_id` change (DN13), and in the side-level sweep of a
     converge (DN6's Scope 2) once the side's request carries no
     `migr_dst_conf` of the chunks' `migr_id` (the §11.2 finish; a level
     that only suppresses the destination role keeps them, DN11) — names
     the files from the side's in-memory chunk set, keyed by the one
     `migr_id` that set is tracking, so a file this process never loaded is
     invisible to all of them and a restart is the only place that can
     collect it. The reconcile holds the node write lock throughout, with
     the SH2 trace id. Outside it, a side's drop runs under `SyncupDn`'s
     node write lock, and DN13's deletions and the sweep's inside a converge
     of the side — a `SyncupSide`'s, a DN8 retry attempt's or the DN12
     fence timer's — under the node read lock and the side's object lock
     (DN1).

### 4.4 `GetDnSize`

DN3. `lsblk --bytes --nodeps --noheadings --output SIZE {--disk}`; reply the
     parsed size **minus `DnDataOffset`** — the byte size of the [D13] data
     area, which is what the CP divides by `extent_size` (§6.1). A size at or
     below `DnDataOffset` is the error `"disk too small: %d <= %d"`. Failures
     (bad disk, parse, too small) return a gRPC `Internal` status (§9.1 —
     this RPC has no `AgentReply`). `dn_id` may be 0 (pre-registration call
     from the gateway); it is for logging only. No locks, no store access.

### 4.5 `SyncupDn`

DN4. Gate the revision (SH8) against the stored `SyncupDnRequest`. Then,
     still with **zero** side effects, the `architecture.md` §7 **conf gate**:
     `agent.ValidateExtentSize(req.extent_size)` (§2.1) refuses a 0 with
     `ReplyCodeInvalidConf` and the message
     `"invalid stored conf: dn_bin_conf.extent_size is zero"`, echoing the
     **stored** revision so the worker sees the request was not accepted,
     and writing one `Error` record (msg `"invalid stored conf"`) naming
     the cluster and the dn. `extent_size` is what this disk's [D13] header
     is formatted with and what every side's runs are carved out of, so a
     value this node substituted would be one the rest of the cluster does
     not share; the control plane resolves it when it *writes* the
     `ClusterConf`, and a zero arriving here is refused rather than
     replaced. Placement is load-bearing: the gate sits **before** the
     request becomes the desired state, so nothing is converged, no dm
     device is removed, no volume-table block is written and no local-store
     file is touched. After the assignment it would instead persist the
     zero and let the next startup reconcile converge it. (DN5's
     header-identity guard is unchanged and is not this gate: it refuses a
     disk formatted for another cluster/dn/extent_size, which is a disk
     fact, not a conf fact.)

DN5. Converge the once-per-DN base state of `architecture.md` §3.1,
     probe-first (SH16), building `DnInfo` as it goes:
     * `lsblk --bytes` the `--disk` device — both to report `disk_info` and
       to hand the allocator the raw size, the one input the disk format
       itself does not carry (the extent count is that size minus
       `DnDataOffset`, divided by the **header's** `extent_size`).
     * **the [D13] disk format.** Read the header block. Magic absent ⇒ the
       disk is blank, and it is formatted only when no side device or
       clone-metadata wrapper maps it — the only devices dnv builds on the
       disk itself: `dmsetup ls`, then `dmsetup table` of every `DnSideName`
       and `DnMigrMetaDmName` on the node, whatever cluster or dn the name
       carries, compared against the disk's `major:minor`. A header can go
       blank under live side devices (one mistaken `dd` over its first 4 KiB
       is enough), and a fresh, empty table would hand their extents to the
       next side, which zeroes them and serves them as its own. So while one
       of them maps the disk, or while any of those reads does not answer, the
       converge writes nothing to the disk and `meta_info` is
       `RES_STATUS_ERROR`, details
       `"blank disk header; refusing to format: …"`. This dn's DN6 sweep
       removes its own such devices once nothing wants them. A device named
       for another cluster or dn is not its to remove: while one maps the
       disk, the disk stays blank, `meta_info` names the first device that
       maps it and every round's verdict replies `ReplyCodeLeftover` (DN6's
       record step), until an operator removes it. Once no side device or
       clone-metadata wrapper maps the disk, the next converge whose reads all
       answer formats. The format writes slot A with an empty table at `seq` 1
       **first**, then the header (a fresh `format_uuid` from `crypto/rand`,
       the request's `cluster_id`/`dn_id`/`extent_size`, and the three layout
       offsets) — slot-A-before-header makes "a valid header implies at least
       one valid table slot" an invariant, so a crash between the two writes
       leaves an inert slot rather than a header with no table.
       Magic present but version or CRC wrong ⇒ **error**, and every
       later operation errors too — a corrupt header is never formatted over.
       Magic present and valid ⇒ verify `cluster_id`/`dn_id`/`extent_size`
       match; a mismatch is the error
       `"foreign disk: cluster/dn/extent is …, want …"` and the disk is
       **never** overwritten (parity with `pvcreate` refusing a foreign PV).
       `extent_size` is therefore immutable for the life of a format. A
       re-call on an already-converged disk issues **zero** writes (SH16).

       The identity check is what makes the volume table this node's to build
       on and to change: handing out a record it already holds (`AllocSide`,
       `AllocCloneMeta` — an existing record's extents are this node's only if
       its table is) and every mutation (allocate, free, the DN9 zeroed-bit
       updates, the DN6 record step) refuse on a disk whose identity this node
       has not confirmed, and so does DN9's re-read of the record before each
       zeroing batch, so no batch is computed from another node's bits.
       Confirming needs no read of its own: this converge hands `DiskMeta` the
       identity it asks for **before** it reads anything, and each of those
       operations compares the header the table in memory was loaded under
       with it. So one header read that did not answer — this converge's at
       startup, say — leaves the disk unconfirmed only until a later read of
       it (a side converge's, a DN18 probe's) has answered, and a disk whose
       header names another cluster, dn or extent size is refused whether or
       not a check has answered. While no read has answered, the DN6 record
       step says so and the verdict is not clean, which re-drives this
       converge. Lookups that only report do not pass the check — the side
       converge's first look at the record, which fills the counters, and the
       DN18 side probe — so on another node's disk a record whose ids collide
       still fills `zeroed_ext_cnt`/`total_ext_cnt`, in a side converge's
       reply beside its foreign-disk refusal and in every probe of the side.
       The guard has to live at that layer rather than in the caller, because
       a failed DN converge does not stop the side converges that follow
       (DN19) — without it, a node pointed at another node's disk would
       report `meta_info = RES_STATUS_ERROR` and then allocate extents in
       that disk's volume table, or build its devices over the records that
       table holds, anyway.
     * `EnsurePort` (SH19: the agent's `--nvmet-port-id` port, default
       `NvmetPortId`, from the `--tr-*` flags + the three fixed ANA groups).
     * **the Write Zeroes fail-fast**. DN9 zeroes whole
       sides with `blkdiscard --zeroout` under the ordinary SH15 timeouts,
       which only holds on hardware whose Write Zeroes is offloaded; a
       kernel that has to emulate it writes zero pages at bulk speed, so
       §9.4's fast-Write-Zeroes assumption cannot hold: `DnZeroBatchExtCnt`
       batches would overrun the soft timeout and the side would crawl in
       DN9's backed-off batches, if it converged at all. Resolve the disk's
       kernel name with
       `lsblk --nodeps --noheadings --output KNAME {--disk}` (the flag is
       documented as a `/dev/disk/by-uuid` symlink, whose basename is not a
       sysfs node) and read
       `/sys/class/block/{kname}/queue/write_zeroes_max_bytes`. A **present
       `0`** is the verdict: `meta_info = RES_STATUS_ERROR` with
       `details = "disk lacks Write Zeroes"`, which flows into the worker's
       `err_epoch` → capacity-key removal (§9.5, §10.2) and takes the
       unsuitable DN out of allocation. An absent or unreadable attribute is
       **not** a verdict — the attribute read reports both as *not present*,
       silently, and the converge continues (only a failed `lsblk`, an empty
       `KNAME` or an unparsable value is logged, as a warning, and the
       converge continues just the same), because
       failing every kernel that simply does not publish the attribute would
       remove healthy DNs for a reason nothing measured. Unlike the identity
       check this is a **health** signal, never a write gate (DN19): a DN
       already carrying sides must keep serving them.

DN6. **Removal is a sweep of actual minus desired, never a memory.**
     `side_pointer_list` is authoritative (§9.1 full sync). A pointer in the
     request without local state needs nothing yet — resources come with its
     first `SyncupSide`; the persisted request is what makes the pointer
     *known*. A side this process holds whose pointer has **left** the list
     is dropped on the spot (SH7): its DN8 connect-retry registration goes,
     its DN9 zeroing goroutine is cancelled **and waited for**, its §11.2
     fence is cleared, and its `side-*` and `migr-bm-*` files, memory entry
     and object lock are deleted. Nothing of it is removed from the node at
     that point. The files deleted are the ones its memory entry names —
     the request's own path and its chunk set's files — so a chunk DN2's
     reload skipped and no push has rewritten stays, as does every file of
     a side DN2 skipped that no `SyncupSide` has rebuilt, which the process
     does not hold at all; they wait for a later restart (SH7).

     What to remove is derived afterwards from the node itself: enumerate
     what exists — `dmsetup ls`, the configfs subsystem listing, the
     `/sys/class/nvme-subsystem` walk — subtract what the desired state
     wants, remove the rest top-down, and verify every removal with a probe.
     `architecture.md` §9.8 states the principle both roles share — including
     why nothing about a failed removal may be remembered — and what follows
     is the dn's instance of it. Every dnv name carries its own ids, so a side
     the agent has already forgotten is still found by name — which is what
     makes dropping the state first safe, and what the old teardown could
     not do: it deleted the same state after a best-effort removal pass whose
     every step only logged its failure, and nothing ever enumerated the
     object again.

     **Scope 1, node-level.** Under the node write lock, in `SyncupDn` after
     the DN converge and in the startup reconcile after every DN has
     converged. Its wanted set is empty for every side that is not in any
     authoritative list: a `DnErrorName`/`DnLinearName`/`DnSideName` device
     whose `(sp_id, side_id)` appears in no synced DN's `side_pointer_list`
     **and** in no side whose stored request this process holds (a side
     whose file DN2's reload did not load is on disk but held by none until
     its `SyncupSide`) goes, together with the migration objects no held
     side claims (once every side of their sp this node may host is held,
     below) and the exports this agent can attribute
     to itself that name no side it must keep — a `SideToCnNqn` export
     carries no dn id, so it is attributed before it is judged (below), or
     the sweep would take a sibling agent's. It never touches a side that
     **is** in a pointer list, even when that side's file is absent (DN8): a
     node that lost `--local-store` but kept its disk must rebuild those
     sides from their records, and sweeping them would free the extents and
     send the next `SyncupSide` through the §9.4 provisioning protocol again,
     zeroing live data. Nor, while such a side is not held — its file
     absent or unreadable — does it judge a migration object of that
     side's sp (*amended 2026-09-29*): the
     migration objects follow the known-with-state rule of the
     `CloneMetaRecord` (the record rule, below). Each names `(sp_id,
     migr_id)` and no side, and the claim rule reads held sides' requests
     only, so while any side of its sp is known by its pointer alone, "no
     held side claims it" proves nothing — that side may be the one playing
     the migration. Judging them anyway, in the `SyncupDn` that brings a lost
     store's pointer list back, took a source's `DnMigrSrcName` and its
     `MigrSrcNqn` export out from under the destination's dm-clone, whose
     reads of every region not yet hydrated then failed on the leg the host
     was using. Such an object is neither removed nor named as a leftover —
     naming it would only have the worker re-send a `SyncupDn` that must not
     remove it — and the wait ends by itself: the side's Check round replies
     `ReplyCodeUnknownObject` until its `SyncupSide` stores the request, and
     that rejection is what brings the `SyncupSide` (`dnv-worker.md` RW4);
     once every side of the sp this node may host is held, the next pass
     judges the object by the claim rule again. A
     destination's `:3:` connection waits with its dm-clone, not only
     behind L3's stop rule: with the clone out of the chain, L3 would find
     no clone of its own to fail on and disconnect the source under the
     live one. It also takes the namespace-less exports nobody can
     attribute, once they are older than `DnExportOrphanGrace` (below). When
     no DN has been synced or reloaded at all nothing is authoritative and
     the sweep does nothing.

     **Scope 2, side-level.** Inside `convergeSide` and ahead of its build
     phase (§4.6), under whatever DN1 locks that caller holds (SH10/SH11).
     Its wanted set is exactly what the build phase would ensure for this
     side's plan, with the §11.2 source deferral already applied:
     `DnSideName` at every level (DN11), the per-CN `DnErrorName` and
     `DnLinearName` while `sp_level` keeps the dm layer, the `SideToCnNqn`
     exports while it keeps the export layer, `DnMigrSrcName` and its
     `MigrSrcNqn` export while an **effective** `migr_src_conf` is set — each
     still under the level gate above it (DN12) — and `DnMigrFinalName`,
     `DnMigrMetaDmName` and the `:3:` host connection while a destination
     role is wanted (DN13). Everything of that side which the node holds and
     that set does not name is removed. Of the `SideToCnNqn` exports it
     judges only its **own leg's** (*amended 2026-09-28*): the NQN names the
     leg, so another leg's export is skipped by name, before anything of it
     is read. It is another side's business — that side's own `SyncupSide`,
     or Scope 1 once the side has left the list — and this pass holds only
     the node read lock, beside every other side's converge on this kernel,
     a sibling agent's included: judging the whole sp is how one agent's
     side-level sweep stripped a sibling's half-built export of another leg
     of its host link and namespace in an e2e run.

     The node write lock excludes every side-level sweep while the node-level
     one runs, so the two scopes never race. Those locks are per process:
     they order nothing between the sibling dn agents of one kernel, whose
     builds can be in flight while this agent sweeps (below). Two side-level
     sweeps of
     different sides of one sp can both see the same unclaimed migration
     object, because a migration belongs to no side (below); a second removal
     is harmless — its probe simply finds the device already gone, which is
     the same answer it gives for any object that vanished between the
     enumeration and the removal.

     A side-level sweep judges the migration objects of its sp only under
     Scope 1's condition (*amended 2026-09-29*): once every side of the sp
     this node may host is held. Short of it, the sp's migration devices,
     `MigrSrcNqn` exports and `:3:` connections are left out of its chain,
     neither removed nor named. A DN may host sides of two groups of one sp
     (`architecture.md` §6.5), and after a lost store the first
     `SyncupSide` may be that of the side not playing the migration. Judging
     them by the claim rule alone, as it used to, its pass removed a live
     source's `DnMigrSrcName` and `MigrSrcNqn` export with a clean reply,
     and on a destination tried to remove the dm-clone — EBUSY under the
     per-CN linears — and named it, its wrapper and its `:3:` connection as
     leftovers, re-driving that side every round until the migrating side's
     own `SyncupSide` stored its request. The condition is read before the
     claims: this scope holds only the node read lock, so another side's
     `SyncupSide` can store its request meanwhile, and read after the claims
     the condition could hold on claims that miss it.

     **Objects whose name carries no side.** A migration device
     (`DnMigrSrcName`, `DnMigrFinalName`, `DnMigrMetaDmName`) is keyed by
     `(sp_id, migr_id)`, a `SideToCnNqn` export by `(cluster, sp, leg, cn)`,
     and the `MigrSrcNqn` a destination connects to carries the **source**
     DN's id — none of them names the side that built it. Two questions have
     to be answered about such an object, and in this order: *is it mine*,
     and *does anything still want it*. The first is not the same as the
     second, and skipping it is how a sweep takes a sibling agent's live
     object (`architecture.md` §9.8).

     For the three migration dm devices the first question is already
     answered by the enumeration — their names carry this cluster and this
     dn, as the `MigrSrcNqn` export's own name does — so for those the claim
     rule below is the whole test, taken once every side of their sp this
     node may host is held (both scopes, above). The `:2:` export carries
     no dn id at all and the `:3:` connection carries only the **source**
     DN's, so neither
     names the agent holding it; both are visible to every agent sharing the
     kernel, so each needs attributing first. A `:2:` export is attributed
     by the per-CN dm-linear its namespace backs: one naming another dn
     agent's kind-`d1` linear is **foreign** and is never touched; one naming
     ours belongs to the side in that name, and survives for as long as that
     side is in an authoritative list, which is what keeps a side that must
     be rebuilt from its record exporting (DN8) even though no held side
     claims it. The namespace is read by its id, `namespaces/1/device_path`
     — every side export has that one namespace and no other (§4.6) — and
     the namespaces are listed only when there is no namespace 1 (*amended
     2026-09-29*); one whose namespace 1 names no per-CN linear of this
     cluster is foreign, whatever else it holds. The cost is the reason.
     Scope 1 attributes, on every pass — each Check round's verdict among
     them — every export of an sp this agent holds a device or a known side
     of that no held side of its own claims, which includes every
     sibling's export of that sp. Listing each one's namespaces cost an
     `ls` apiece, so the pass of every agent holding sides of an sp grew
     with all the other agents' exports of it: about s × (s − 1) × C
     commands a round on a kernel whose s agents hold sides of one sp
     exported to C controller nodes — at 32 slices most of the roughly 617
     commands a second estimated for one disk-node VM. The read runs in
     process, so attributing an export that has its namespace 1 costs a pass
     no command at all. No attribution is kept from one pass to the next,
     and none needs to be: one pass attributes an export at most once, and a
     Check round's verdict and the `SyncupDn` after it each attribute it
     afresh — an owner remembered between them could be stale, the export
     removed and built again by another agent in between. An export holding
     no namespace at all is attributed by the nvmet **port** it is linked
     to, each agent converging exactly one port id: linked to a sibling's
     port, it is that sibling's. One linked to no port, or only to ours,
     exports nothing and holds nothing open — but it is not therefore
     nobody's (*amended 2026-09-28*). It is also the shape
     of **every** export between its subsystem `mkdir` and its namespace
     `mkdir` — an export is built subsystem first, with its attributes and
     then its allowed hosts, then its namespace, then its port link — and
     from the build's host-link step on, that half-built export already
     carries the CN's host link. On a kernel several dn agents share, the
     build in that window may be a sibling's, whose request no claim of
     ours can show. Removing it there takes the sibling's host link — and
     any namespace it adds meanwhile — from under its build, and a broken dn
     export is rebuilt only by some later converge of its side, which the
     breakage itself never triggers: an e2e run lost a leg that way. Only
     age tells an abandoned half-built export from one in flight, so such an
     export is removed only
     once its configfs subsystem directory is older than
     `DnExportOrphanGrace` (30 s) — unless a held side still claims it,
     which is the claim rule again. A younger one is left alone that pass:
     not removed, not a leftover, not a failure, so the reply code is
     unaffected; each later pass judges it again, until it either has a
     namespace whose `device_path` names its owner's linear, or the age of
     an export nobody is building. (A build abandoned between its namespace
     `mkdir` and its `device_path` write leaves a namespace that names
     nobody. Every agent's attribution reads that export as foreign, so no
     sweep removes it; a later converge of the side it was built for
     finishes it, but once no side wants it, it stays: a known gap.) The
     age is read from the node, never remembered: the
     directory's mtime (`stat -c %Y`) against the agent's clock. A `stat`
     that fails makes the export foreign for that pass, as every other
     failed read of the attribution does: one that did not answer is named
     as a failed enumeration, so the verdict is not clean, and one that
     found the directory gone is simply passed over. The mtime is set at
     `mkdir`, and adding an allowed-host link or a namespace directory under
     it does not move it; but on the lab's 7.0 kernel every lookup of one
     of the subsystem's own `attr_*` files does — a read, a write, even a
     `stat` of one — because configfs instantiates an attribute's inode on
     each lookup and, in that kernel, stamps the parent directory when it
     does. So the age reads "since the subsystem was created or an
     attribute of it was last touched". A first build is young from its
     `mkdir` on, and its attribute writes only keep it so; on that kernel
     the stamp also protects a rebuild — an owner rebuilding, more than the
     grace after its first `mkdir`, an export whose first build stopped
     before its namespace makes it young again with the rebuild's attribute
     reads. A kernel that no longer stamps on an attribute lookup (7.3
     stamps the directory only when a directory or a link is created under
     it) reads the plain time since the `mkdir`, and there such a rebuild
     reads old. Nothing a sweep does looks up one of the subsystem's own
     `attr_*` files (it reads namespace 1's `device_path`, which sits in the
     namespace's directory and not the subsystem's, and which a
     namespace-less export does not have; it lists the namespaces and the
     port links and stats the directory), so an abandoned export ages, and
     the first sweep that finds it older than the grace settles it: a
     Syncup's removes it, and a read-only Check round's reports it as a
     leftover, which brings that Syncup. Anything else that keeps looking up
     those files keeps it young for as long as it does. A `:3:` connection
     is attributed by its controller's `hostnqn`
     (`/sys/class/nvme/nvmeN/hostnqn`): the nvme host namespace is per
     kernel, and since the NQN names the **source** DN, the host NQN it was
     opened with is the only field that names the agent holding it.

     What that leaves is judged by the **claim rule**: the object goes unless
     some side this agent holds state for still wants it, read from that
     side's stored request — its **effective** `migr_src_conf` (a source
     whose destination has not provisioned claims nothing, §11.2), its
     `migr_dst_conf` under a wanted destination role, its export under a
     wanted export layer.

     **A claim carries its claimant's gate, and the source needs two of
     them.** A claim is not "this object exists"; it is "somebody still WANTS
     it", so it is recorded only under the same condition that keeps the
     object in the wanted set. The migration source is the one role whose two
     objects part company: the `d2` linear is wanted under the dm layer, its
     `:3:` export under the export layer, and `SP_LEVEL_NO_SIDE` sits exactly
     between them. A single ungated claim made both permanently unsweepable —
     the wanted set dropped them correctly, but every sweep skips what the
     claim map holds, so they were neither removed NOR reported, the reply
     stayed OK, and an sp taken to `NO_SIDE` went on exporting its migration
     source until `migr_src_conf` itself was dropped. Being skipped rather
     than named is what makes this class silent: a leftover at least
     re-drives. A side's request is stored before its converge
     builds anything, so no claim of a role this process started can be
     invisible to the rule; one a lost store took stays invisible until the
     side's `SyncupSide` stores its request again, which both scopes wait
     out (above). And the claim is recomputed from the requests every pass,
     never read from what a converge left behind. That
     is what lets a **finished** migration's objects go in the same pass that
     drops its conf (unless that wait holds them), with no "applied
     destination" to remember — the field
     that used to hold it was overwritten by the very converge that was
     supposed to retry the removal.

     **The layers.** Each scope removes its unwanted objects in one order,
     top-down. Every position is a dependency, not a preference:

     *Before the first layer*, every suspended per-CN dm-linear the pass is
     about to remove, or whose export it is about to remove — wanted or not —
     is put on its dm-error and resumed: DN12's phase 2, brought forward
     (*amended 2026-09-28*). L1 disables the nvmet namespace above it, and
     that write first waits for every request in flight on the namespace;
     one whose bio a suspended dm target holds never completes, since such a
     target queues bios with no timeout and no error path ([D12]), and the
     agent cannot know that none is held. The §11.2 cutover leaves exactly
     such devices behind — holding the old primary's in-flight IO is what
     its window is for — and not only under a side torn down inside the
     grace window: `SP_LEVEL_NO_SIDE` keeps the linears and takes only the
     exports off them. The linear under an export is the one its
     attribution read off the namespace (above). A bare resume would free
     the device too, but against the table it was suspended with — the
     primary CN's maps the side's data — so what the window absorbed would
     be replayed onto it; against the dm-error it fails instead. The one
     exception is a request that also ends the source role: its pre-step
     has already made that bare resume, which is that role ending's own
     rule (DN12), so this step retires only a linear the resume left
     suspended. A linear this step cannot prove out of suspension — a probe
     or its reload failed — stops the descent before L1, as a layer that
     left something behind does: no layer runs, every unwanted object of
     the scope is named as a leftover, and the next pass tries again.

| | removed | why here |
|---|---|---|
| L1 | the `SideToCnNqn` exports, the `MigrSrcNqn` export | an enabled nvmet namespace holds its backing device open, and removing the subsystem is what disables it: nothing below can go while an export still names it |
| L2 | the per-CN `DnLinearName` | every device below is one of its table targets, and `dmsetup remove` on a device another live dm device still maps fails EBUSY |
| L3 | `DnMigrFinalName`, then the `:3:` host connection | the connection is the dm-clone's source device: pulling it out from under a live clone strands the clone's in-flight hydration IO |
| L4 | `DnMigrSrcName` | it maps the side device, so it goes before L7; nothing local maps it once its own export is gone |
| L5 | the per-CN `DnErrorName` | nothing maps a dm-error once the linear that targeted it is gone |
| L6 | `DnMigrMetaDmName`, then its `CloneMetaRecord` | it is the dm-clone's metadata device and cannot go before the clone |
| L7 | `DnSideName`, then its `SideRecord` | everything above maps it |

     Getting this backwards does not merely log an error: a clone that
     survives keeps its metadata wrapper and the side device under it alive
     too, and no later empty side list can remove them either — the side
     leaks until the node is scrubbed by hand (IR4).

     **Finish the layer, never descend below one that left something.** Every
     removal of a layer is attempted; if anything of that layer is still
     there afterwards, the layers below are **reported** as leftovers and not
     touched. Continuing would disconnect a live clone's source, or attempt
     a device something still maps; stopping at the first failure inside a
     layer would lose the rest of its names from the report. Nothing is lost
     by waiting: the pass is re-run on the next round, by which time the
     failfast window has passed and no command blocks.

     **"Gone" is probe-verified.** Every removal is followed by a re-probe —
     `dmsetup info` for a dm device, the configfs directory for a subsystem,
     the sysfs walk for a connection — and that probe, never the removal
     command's exit status, is the evidence. A killed command may have
     completed in the kernel, and a killed probe proves nothing at all
     (SH15). For a connection the question that walk asks is whether any
     **controller** is left, not whether the subsystem is: the kernel keeps
     the `/sys/class/nvme-subsystem` entry after the last controller of an
     NQN is deleted, and an entry with no controller holds nothing open, so
     waiting for the directory itself would report a leftover that never
     goes.

     **The record rule.** An allocation record is released **only** after the
     sweep has verified its device is gone **and** the authoritative pointer
     lists prove its owner is gone. Both halves are load-bearing, and the
     second one is where "provably" has to be taken literally, because the
     volume table — not the local store — is authoritative for extent
     placement ([D13]). A `SideRecord` is an orphan
     only when its `(sp_id, side_id)` appears in no synced DN's
     `side_pointer_list` and in no side whose stored request this process
     holds (a side whose file DN2's reload did not load is held by none
     until its `SyncupSide`). Missing local state
     is *not* proof: a node that lost `--local-store` but kept its disk still
     has every side in its DN's pointer list, and freeing those extents makes
     the next `SyncupSide` either re-allocate them and zero live data away
     (`provisioned = false`) or, at `provisioned = true`, refuse to allocate
     and report the side permanently dead (`"record missing"`, DN9) — both
     outcomes lose the data. A `CloneMetaRecord`
     is an orphan only when no held side claims its `(sp_id, migr_id)`
     **and** every side of that `sp_id` this node may host is one whose local
     state the agent actually holds — otherwise a side it has not heard from
     yet could still own the slot, and freeing it would strand an in-flight
     migration whose hydration is supposed to resume from disk (§11.2).
     Freeing a record while its device still maps those extents is not a leak
     but a corruption path: the next allocation hands the same extents to
     another side, which zeroes them and serves them as its own. In this
     agent that is where reading a killed `dmsetup info` as "the device is
     gone" destroys data rather than leaking a device, and it is why SH15's
     rule is a rule.

     **The record step.** The same proof, run over the volume table itself
     rather than over the devices the enumeration found, closes the crash
     window between a removal and its table update. It runs on every
     node-level pass — after the pointer drop in `SyncupDn`, and after the
     DNs converge in the startup reconcile. It removes **no** device: the
     layers do that, in the order the kernel needs and stopping the descent
     where something above will not go, and a removal issued from here would
     jump that order. What is left for this step is the record, and the only
     record it frees is one whose owner is provably gone **and** whose device
     it has probed and found already gone. A record whose device is still
     there — or whose `dmsetup info` did not answer, which is not an absence
     (SH15) — is reported as a leftover and stays allocated until a later
     pass finds the device gone; freeing extents a live device still maps is
     the corruption path above, not a leak. Cancelling the zeroing goroutine
     is not this step's business either: the node-level pass cancels and
     waits for the goroutine of every side device it is **about to remove**,
     ahead of the layers, because a live `blkdiscard --zeroout` child holds
     `DnSideName` open and `dmsetup remove` on an open device fails EBUSY
     (§9.4). The whole step refuses on a disk whose identity this node has
     not confirmed, as every mutation of the table does (DN5), and it says
     so: the table it cannot use counts as a listing that did not answer
     (`volume table: disk identity is not confirmed for this node`), so the
     verdict is not clean and the worker re-drives the `SyncupDn`, whose
     DN5 converge checks the disk again — every round, for as long as the
     disk stays unreadable, corrupt, another node's, or blank under a live
     side device or clone-metadata wrapper (DN16).

     Ids are never reused, so a swept side never comes back.

     **Finishing a migration is not a teardown of the side.** When a
     `SyncupSide` drops `migr_dst_conf` while the side keeps exporting
     (§11.2 dst finish), the per-CN dm-linears are **wanted** — they are not
     in the chain at all — while the dm-clone they still map is not. The
     sweep therefore repoints before it removes: a wanted per-CN linear whose
     **live table** maps a device this pass is about to remove is reloaded
     onto the backing the plan wants, first, so that L3 does not find the
     clone pinned under it and lose the whole chain to the stop rule for a
     round (DN13). The live table is read, not remembered — the reload's
     target is `linearBacking` with no live clone, which is what the build
     phase would give it anyway. A suspended wanted linear is left alone.
     On a migration source that is the fence's suspension, deliberate, and
     ending it is the fence's own job (DN12); on any other side DN12 rule
     2's resume, which the same pass runs just before, has tried to end it
     (a failed reload can leave a device suspended, §2.8), and one still
     suspended — its probe or its resume failed — is the build phase's to
     reload or resume.

     **The verdict.** Whatever a pass could not remove, plus any enumeration
     that did not answer, is what the reply's `agent_reply` carries (DN19).
     It is one comparison's result, recomputed every round and stored
     nowhere.

DN7. The request was persisted before the converge (SH5) and the node-level
     sweep has run (DN6); reply `agent_reply` — the sweep's verdict (DN19) —
     `revision` (= the stored revision after this call) and `dn_info`.

### 4.6 `SyncupSide`

DN8-DN14 below; the converge order is build bottom-up (side device → dm →
nvmet), tear down top-down, probe-first throughout (SH16).

DN8. **Gating.** The pointer MUST be present in the stored
     `SyncupDnRequest.side_pointer_list` — else `ReplyCodeUnknownObject`
     (`SyncupDn` introduces pointers first, §9.2). Then the SH8 revision gate
     against the `SyncupSideRequest` this process holds — 0 while it holds
     none, as for a side whose file DN2's reload did not load, whatever
     revision that file carries.

     After a lost `--local-store`, the first `SyncupSide` of each listed
     side meets no stored request: there is no revision to gate it against,
     and its converge adopts, probe-first, what the node still holds for it
     (*amended 2026-09-29*). The `SyncupDn` that brought the list back left
     the side's devices and the exports attributed to it in place (DN6's
     Scope 1), and the migration objects of its sp as well: both of DN6's
     scopes judge a migration object only once every side of its sp this
     node may host is held, so neither that pass nor the first
     `SyncupSide` of another listed side of the same sp takes a migration
     source's `DnMigrSrcName` and `MigrSrcNqn` export, or a destination's
     dm-clone, wrapper and `:3:` connection, from under a live migration
     before the migrating side's own request is back.

DN9. **Side device and the §9.4 side provisioning protocol.** Look up
     `(sp_id, side_id)` in the volume table. An existing record whose extent
     total disagrees with `side_conf.ext_cnt` is an error (resize is out of
     scope).

     **Allocation is permitted only while `side_conf.provisioned` is
     `false`.** At `false` with no record: allocate `side_conf.ext_cnt`
     extents — first fit one contiguous run, else free runs largest-first —
     and persist the record with `zeroed_bits` all 0 (an unset field: proto3
     omits an empty `bytes`, and an absent bit reads as 0). An allocation
     failure is reported as it is, never swallowed: the three converge
     outcomes below must stay distinguishable. At `true` with no record the
     agent **never** allocates: the data is gone (a lost or foreign disk), and
     silently re-allocating would present a zeroed impostor as the
     data-bearing leg. That is the hard resource error
     `side_dev_info = RES_STATUS_ERROR`, `details = "record missing"`, which
     feeds `err_epoch` and the replacement flows (§10.4 spare-switch for
     raid1 — automatic after `leg_unhealthy`; effectively delete-SP for
     RedundNone).

     Build `DnSideName` as a multi-target dm-linear concatenating the record's
     runs, run `r` mapping to disk offset `DnDataOffset + r.start*extent_size`
     for `r.count*extent_size` bytes (both /512 for the table).

     Then the §9.4 protocol — **whole-side zeroing behind a `provisioned`
     gate**, which replaced the trim flag ([D15]: `blkdiscard` is
     not a zero guarantee — the kernel dropped `discard_zeroes_data` in 4.12
     and NVMe DLFEAT read-zeroes is optional — so the trim funded neither
     dnv's multi-tenant "no tenant ever reads another tenant's bytes"
     requirement nor the places the design assumes zeros: a recycled extent
     can hold a previous SP's valid thin-pool superblock, or a stale md
     superblock that flips CN12 into the wrong assembly case):

     1. the extent runs are allocated and the record persisted with
        `zeroed_bits` all 0 (above);
     2. `DnSideName` is built (above);
     3. a **background zeroing goroutine** (the registry below) zeroes the
        not-yet-zeroed extents in batches of at most
        `common.DnZeroBatchExtCnt` (10),
        **through the dm-linear** — the side is contiguous in that device's
        address space, so one command covers a whole batch whatever the
        physical fragmentation. Each batch starts at the **first extent whose
        bit is still 0** and covers the run of 0 bits from there, capped at
        the batch size (a first-unset walk, never a count of set bits: the
        bitmap is deliberately more general than a watermark):
        `blkdiscard --zeroout --offset {from × extent_size} --length
        {count × extent_size} {DmPath(DnSideName)}`. After each successful
        batch that batch's bits are persisted in the volume table
        (`SetSideZeroed(ctx, spId, sideId, fromExt, toExt)`, half-open — the
        range setter that replaced `SetSideTrimmed`);
     4. the per-CN export stacks (DN10) and the migration roles (DN12, DN13)
        converge **only** when the request says `provisioned = true` **and**
        every bit is set. The agent always trusts its own bits over the flag:
        the disk is authoritative ([D13]); the etcd flag is a gate, never
        evidence.

     **Logical extent *i*** is the *i*-th extent of the concatenation of the
     record's `run_list`, i.e. bytes `[i, i+1) × extent_size` of the
     `DnSideName` device; `zeroed_bits` is LSB-first within each byte
     (`bitmap[i/8] & (1 << (i%8))`, `agent/bitmap.go`) with trailing pad bits
     0, and every count is taken over the record's own extent total — never
     over `len(bits)*8`, which would make a 10-extent side look 16-extent and
     declare it done early. **Zeroed is a property of the side's allocation,
     not of the disk extent**: extents freed and reallocated to a new side
     start all-not-zeroed again, whatever happened to them before, because
     `AllocSide` is the only constructor of a record and `FreeSide` deletes
     records whole.

     `provisioned` is monotone — the worker only ever flips it `false → true`
     (§9.5 flip rule) and bits are only ever set — so the gate never tears an
     already-exporting stack down.

     **Converge matrix** (`side_conf.provisioned` × local state):

| `provisioned` | record | bits | behavior | `side_dev_info` |
|---|---|---|---|---|
| false | absent | — | allocate (bits 0), build the linear, ensure the goroutine | `PROVISIONING`, `"zeroing 0/n"` |
| false | present | partial | ensure the linear + the goroutine | `PROVISIONING`, `"zeroing k/n"` |
| false | present | complete | linear ensured; no goroutine; **no exports** | `OK` (every per-CN row reports `PROVISIONING`, `"side provisioning"`) |
| true | present | complete | full DN10 export converge | normal |
| true | present | partial | **refuse exports**; keep the goroutine (it self-heals) | `ERROR`, `"not zeroed"` |
| true | absent | — | **never allocate**; no linear, nothing converges | `ERROR`, `"record missing"` |

     Every row above is a *converge* outcome. A read-only round (DN16, SH25)
     allocates nothing, so "no record" at `provisioned = false` stays today's
     `RES_STATUS_MISSING` there — the converge that would allocate has not run
     yet, and `n` must never be taken from the request.

     **The zeroing registry** (the dn twin of the DN8 retry registry):
     * keyed by the side tuple `(cluster_id, dn_id, sp_id, side_id)`,
       single-flight per side, created on demand by any converge — the startup
       reconcile included (DN2) — that finds zeroing still needed. Sides zero
       in **parallel**, but at most `common.DnZeroConcurrency` (2) batches run
       at once per agent: N concurrent batches split the disk's Write Zeroes
       rate N ways, and enough of them would have every batch killed at the
       soft timeout and redone for ever. Only the `blkdiscard` holds a slot;
       waiting for one publishes no error, holds no lock, and a cancel
       reaches it at once.
     * each batch mints a fresh trace id (SH2's `common.NewTraceId`); the
       `blkdiscard` itself runs **lock-free** and through the ordinary
       `OsClient` under the standard SH15 timeouts — it is a *child process*,
       so a semaphore slot is held for at most `CmdHardTimeout`, or for as
       long as an uninterruptible kernel wait holds the child past it (SH15),
       and no `LimitedOsClient` carve-out is needed (unlike the CN11 probe IO,
       `cnagent.md` §2.2). Only the volume-table update afterwards takes the
       DN1 locks (node read + the side's object lock) on top of `diskmeta`'s
       own writer serialization, and it takes them with **try-acquire and a
       short poll, never a blocking wait**: teardown cancels this goroutine
       and waits for it while holding the node **write** lock, and a
       `sync.RWMutex` acquire cannot be released by cancelling a ctx, so a
       blocking `RLock` here would deadlock the agent permanently.
     * a failed or timed-out batch puts the killed command's output into
       `side_dev_info` (`RES_STATUS_ERROR`) and is retried no sooner than
       `common.DnZeroRetryInterval` (5) seconds — never a hot loop. While such
       a failure is outstanding `ERROR` wins over the matrix's
       `PROVISIONING`; the next successful batch clears it. Partial zeros are
       harmless: the batch's bits stay unset and its extents are redone, so a
       restart simply resumes at the first unset bit.
     * **the batch size follows the kills.** A batch the SH15 soft timeout
       killed — the tool did not answer (`agent.Reported`) — makes the side's
       next batch half the killed one (rounded down, at least one extent); a
       success doubles it again, up to `common.DnZeroBatchExtCnt`;
       `common.DnZeroKillBackoff` (2) kills in a row drop it straight to one
       extent per batch, and while that kill is outstanding the bullet
       above's `ERROR` — shown only on a side still at `provisioned = false`;
       at `true` the row reads `"not zeroed"`, per the matrix — has the
       details `"{output} ({k} batches killed in a row: backed off to 1
       extent per batch)"`. A batch the tool refused (it ran and answered no)
       keeps the size and ends the streak.
       The size and the streak are the goroutine's own memory — rate control
       derived from the kills it saw, never a record: the size caps the next
       command and the streak also words the details above, and they decide
       nothing else (which extents to zero, and whether the side is done,
       only the bits decide). Every new goroutine for the side, a restart's
       included, starts again at `common.DnZeroBatchExtCnt` with no kills
       counted.
     * zeroing runs at **every** `sp_level`, `SP_LEVEL_DISABLE` included
       (DN11): it is bottom-layer provisioning, exactly as the trim it
       replaced was.
     * cancellation: the drop of a side whose pointer left the list (DN6,
       which is also where a cancelled
       migration lands — `CancelMigration` reaches the agent as the side
       pointer leaving the list) **cancels the goroutine and waits for it**
       before the sweep removes the dm device, because the running child holds
       `DnSideName` open and `dmsetup remove` would fail EBUSY. Process exit
       is SH27's `WaitBackground`, so no orphan `blkdiscard` ever outlives the
       agent.
     * `SideInfo.zeroed_ext_cnt` / `total_ext_cnt` are filled on every reply
       and every Check round (DN14, DN16, DN18). `total_ext_cnt` is never
       omitted: it comes from the record, or from `side_conf.ext_cnt` when
       there is no record yet — so "no record" reads `0/ext_cnt`, never
       equal counts. Equality is what the worker's flip rule watches,
       guarded by `> 0`, which also keeps the degenerate `0/0` of a
       zero-`ext_cnt` request from reading as done.

DN10. **Per-CN export stacks.** They converge **only** with DN9's gate open —
      `side_conf.provisioned = true` and every `zeroed_bits` bit set. While it
      is closed the whole per-CN stack is skipped (dm-error, dm-linear, nvmet
      subsystem, namespace) and nothing is torn down either, because the gate
      is monotone; each `cn_id_to_dm_error` / `cn_id_to_dm_linear` /
      `cn_id_to_nvmeof` entry reports `RES_STATUS_PROVISIONING` with
      `details = "side provisioning"` (DN18). The fault of a row 5 or row 6
      side stays on `side_dev_info` alone — duplicating one cause across every
      per-CN row would multiply `err_epoch` churn. With the gate open, for
      `primary_cn_id` and every `standby_id_list` entry:
      `DnErrorName` (dm-error sized like `DnSideName`),
      `DnLinearName` (table → the side device for the primary CN, the
      dm-error for standbys), nvmet subsystem `SideToCnNqn(cluster, sp, leg, cn)` on the
      node port with `allowed_hosts = [CnHostNqn(cluster, cn)]`,
      `attr_cntlid_min/max` from `side_conf.cntlid_slot` (§11.8,
      `DnCntlidSlot*`), and one namespace: `nsid = 1`, `device_path` = the
      CN's dm-linear, identity from `common.DnNsIdentity(cluster_id, sp_id,
      leg_id)` (§2.2 — both sides of a migrating leg MUST match),
      `attr_serial = %016x(leg_id)`, `attr_model = "dnv"`. ANA: the primary
      CN's namespace joins `AnaGrpIdOptimized`, standbys join
      `AnaGrpIdNonOptimized` ([D4]; overridden by the migration phases below
      and by `sp_level`). A `primary_cn_id` change reloads the dm-linear
      tables and rewrites the two `ana_grpid`s — nothing else. A reload of
      the old primary's linear whose load fails leaves it suspended on its
      old table (§2.8), where DN12 rule 2's resume can release its queued IO
      onto the side's data (§2.8's known limit).

DN11. **`sp_level` gating** (`architecture.md` §11.7; numeric comparisons —
      the enum values are ordered). Levels are desired state: raising tears
      layers down, lowering rebuilds them; bitmap chunks stay applied-by-file
      throughout (SH21).

| condition | additional dn behavior |
|---|---|
| `level >= SP_LEVEL_READONLY` (16) | nothing — the level has no DN-side behavior; read-only is enforced on the CN's user-facing namespaces only ([D11]) |
| `level >= SP_LEVEL_NO_MIGRATION` (80) | no migration dm-clone: no `DnMigrFinalName`, no `nvme connect`, no `DnMigrMetaDmName`; a destination side keeps its per-CN exports on dm-error, all namespaces `AnaGrpIdInaccessible` |
| `level >= SP_LEVEL_NO_SIDE` (96) | no nvmet exports at all (side subsystems and migration-source subsystem removed); dm devices remain |
| `level >= SP_LEVEL_DISABLE` (112) | only the side device and its allocation record remain; the DN9 zeroing goroutine keeps running at this level too — provisioning sits *below* the level ladder, exactly as the trim it replaced did |

      The CN-only intermediate levels (`SP_LEVEL_NO_CLONE`,
      `SP_LEVEL_NO_THINPOOL`, `SP_LEVEL_NO_REDUND`) have no DN-side behavior
      either — on the DN, every level below `SP_LEVEL_NO_MIGRATION` behaves
      like `SP_LEVEL_READWRITE`.

      **dnv has no DN-side read-only mechanism at all** ([D11]). nvmet opens
      a namespace's backing device with `BLK_OPEN_READ | BLK_OPEN_WRITE`, so
      the top device of any export can never be read-only; and a read-only
      flag *below* the top does not stop writes that device-mapper remaps
      onto it, because the kernel's check runs at top-level bio submission
      only. The DN could not fail writes even if it wanted to: md superblock
      and bitmap writes, resync, failover assembly and the §3.6 health-check
      block writes must keep flowing at every read-only level. Per-CN
      dm-linears and `DnSideName` are therefore always writeable, whatever the
      level. `SP_LEVEL_READONLY` is enforced solely on the CN's user-facing
      namespaces, by reloading each `CnNsDevName` onto a dm-flakey
      `error_writes` table (reads pass, writes error).

DN12. **Migration source** (`migr_src_conf` set): the §11.2 sequence in
      order — (1) move every per-CN namespace to `AnaGrpIdInaccessible`, (2)
      retire every per-CN dm-linear through the two-phase fence below, (3)
      build `DnMigrSrcName` (linear on
      the side device) and export it via subsystem
      `MigrSrcNqn(cluster, dn, sp, migr_id)` on the node port,
      `allowed_hosts = [DnHostNqn(cluster, migr_src_conf.dst_dn_id)]`, its
      namespace in `AnaGrpIdOptimized`.

      **The `dst_provisioned` gate.** `migr_src_conf.dst_provisioned = false`
      means the destination side is still being zeroed (DN9), and it is
      **normative** that the source then behaves *exactly as if
      `migr_src_conf` were absent*: no ANA move, no fence, no
      `DnMigrSrcName`, no migration-source subsystem — the side keeps serving
      its per-CN stacks normally. The only difference is reporting: the
      would-be `migr_src_info.dm_linear_info` and `.nvmeof_info` are
      `RES_STATUS_PROVISIONING` with `details = "side provisioning"` instead
      of absent. Without the gate the source
      would fence the primary's path the moment the migration was created and
      the leg would have **no serving path for the whole zeroing window**
      (§11.2). When the worker flips the destination side, the
      next fan-out carries `dst_provisioned = true` and the sequence above
      runs unchanged; the destination's connect retry (DN13) absorbs any
      cross-side ordering.

      **The step-2 fence ([D12]).** Phase 1: suspend each per-CN dm-linear
      **in place**, leaving its table alone, and record when. Phase 2, on the
      first converge at or after `common.SuspendSeconds` have passed: reload
      it onto its dm-error, which resumes it — unless the load fails, which
      leaves it suspended on its pre-fence table (a reload fails closed,
      §2.8) until a later converge's reload of it succeeds or the role's end
      resumes it (rule 2 below), so the bound the rules below keep holds
      only as far as those reloads succeed. Swapping
      the table in phase 1 would error the very IO the window exists to
      absorb; resuming without the swap would replay it onto the side's
      data. The RPC never waits out
      the window — a one-shot timer arms the converge that ends it, the same
      way DN8 arms the connect retry — and every converge is idempotent, so
      an early one at an exporting level simply stays in phase 1.

      Four rules keep the suspension bounded, which is what makes it safe:
      * A side whose linears are suspended but whose window start is unknown
        — an agent restart mid-window — is treated as **elapsed** once the
        restarted agent finds them so, and phase 2 runs on the first
        converge, so a restart that finds them suspended opens no second
        window. One that does not find them so can (*known
        limit, 2026-09-28*): the window's start, and the mark rule 3 leaves
        when a level ends a window early, live only in memory. After the
        window, with the linears resumed on the dm-errors phase 2 left them
        on, the restarted agent's first converge that builds the side's
        per-CN stacks at an exporting level, with the role still standing,
        opens a second window over those dm-errors — unless a pass at a
        level with no export layer has marked the window over again before
        it. Nothing that window absorbs can reach the side's data — the
        table that holds it is already the dm-error — but the linears sit
        suspended for another `SuspendSeconds`, and the probe, which inside a
        window expects the pre-fence table, reports the primary's linear
        `RES_STATUS_ERROR` until phase 2 runs again. A restart inside the
        window whose probes of the linears all go unanswered does not find
        them suspended either, so it treats no window as elapsed: the
        linears stay suspended on their pre-fence tables with no window
        running and no timer armed, and no verdict names them, since they
        are wanted. At an exporting level, with the role still standing, a
        converge that stops at the DN9 gate leaves them so — rule 4 acts
        only on a window the process knows of — and, unless a pass at a
        level with no export layer has marked the window over and retired
        them before it (rule 3), the first one that builds the side's
        per-CN stacks opens a second window over them, which phase 2 ends
        `SuspendSeconds` later, however long that converge was in coming.
        While they sit so, a probe that gets past the side device expects
        the dm-error and reports the primary's linear `RES_STATUS_ERROR`.
      * The role ending clears the window and returns the linears to their
        normal targets, resumed. The condition is a **state**, not an event:
        every converge of a side whose *effective* `migr_src_conf` is absent
        — dropped, or deferred behind `dst_provisioned = false` — clears the
        window and resumes every per-CN dm-linear of that side it finds
        suspended, so nothing has to remember that a window was ever opened.
        The set comes from the **enumeration** of what the node holds, not
        from the plan's cn list: a linear built for a CN that has since left
        `standby_id_list` is exactly the one a remembered list would miss,
        and leaving it suspended would queue bios with no timeout. The
        resume alone is enough — the queued IO drains against whatever table
        is live, here the pre-fence one, and the build phase then reloads the
        linear onto the target the new desired state wants. The same resume
        also meets a linear that a primary flip's failed reload left
        suspended on its old table (DN10), and there it is not enough: it
        releases the old primary's queued IO onto the side's data ahead of
        any retry of that reload (§2.8's known limit).
      * Tearing the side down, or taking its exports away, inside the window
        ends the window early, with the same reload (*amended 2026-09-28*).
        **Before** its first layer (DN6) the sweep puts every suspended
        per-CN linear it is about to remove, or whose export it is about to
        remove, on its dm-error and resumes it — never a bare resume, which
        would replay the absorbed IO onto the side's data — because
        disabling an nvmet namespace first waits for every request in flight
        on it, and one whose bio a suspended dm target holds never
        completes. A request that also ends the role is rule 2's case
        instead: its pre-step resumes the linears onto their pre-fence
        tables before this step runs. A level with no export
        layer — `SP_LEVEL_NO_SIDE`, which keeps the linears and takes only
        their exports, or any level above it — ends the window outright:
        for a source side at such a level the sweep's pre-step marks it over,
        the way an adopted one is, until the role itself ends (a state again,
        and in memory like the window's start), so a later converge of the
        same process at an exporting level — the level coming back down —
        rebuilds the exports over linears on their dm-errors instead of
        opening a second window over them. A restart forgets the mark (rule
        1's known limit).
      * A converge that stops at the side-device gate — *any* DN9 outcome
        short of ready: still provisioning (zeroing, or zeroed and not yet
        released by the CP), or a side-device fault (an unreadable disk,
        `"record missing"`, `"not zeroed"`, a refused allocation, a table or
        probe that would not converge) — skips the whole per-CN stack, but
        never the fence: inside the window it re-arms the timer; past it, it
        finishes phase 2 itself. Nothing else would end the window there: the
        timer nils itself before converging, the only other unfences are the
        two above and neither applies while the role, the side and its exports
        are still wanted, the agent's one periodic converge — DN8's connect
        retry, armed only while a destination role's connect is failing —
        would take this same gate, and a `CheckSide` round neither converges
        nor bumps a revision. One transient probe failure would otherwise
        leave the linears suspended, queueing bios with no timeout, until the
        worker next happened to re-sync the side.
        Running phase 2 under that gate is safe: the dm-error and the
        dm-linear are the devices the fence itself suspended, not something
        built on top of the side, and retiring them only moves the side
        further from exporting data.

      While the window is open the per-CN `dm_linear_info` is
      `RES_STATUS_OK` with `details = "suspended (migration cutover grace
      window)"`: it is an expected, time-bounded state, and the probe expects
      the **pre-fence** table there rather than the dm-error, so a healthy
      cutover reports no table mismatch — except in rule 1's known-limit
      second window, opened over tables phase 2 has already swapped.

      **Ending the role removes nothing directly.** `DnMigrSrcName` and its
      `MigrSrcNqn` export simply stop being wanted, and the sweep takes them
      (DN6 L4 and L1) once every side of the sp this node may host is
      held. The linear is keyed by `(sp_id, migr_id)` and the
      export by `(cluster, dn, sp_id, migr_id)`; neither names a side, so
      both are judged by the claim rule — no side of this DN whose stored
      request this process holds still names that `migr_src_conf` (it holds
      none for a side DN2's reload skipped) — rather than by an "applied
      source" the converge would have overwritten on its way past.
      Deferral is the same
      condition: while `dst_provisioned` is `false` the effective source conf
      is absent, so anything an earlier pass built for it is unwanted and
      goes, which is what makes "behaves exactly as if `migr_src_conf` were
      absent" true of the removal half as well.

DN13. **Migration destination** (`migr_dst_conf` set).

      **Provisioning first.** While the destination side's own
      `side_conf.provisioned` is `false`, or any of its `zeroed_bits` is
      unset, **none** of steps (1)-(5) run. The side converges to the DN9
      shape only — the extent record, `DnSideName` and the zeroing goroutine —
      with no per-CN stacks, no metadata slot, no `nvme connect` and no
      dm-clone; `migr_dst_info.target_info` and `.dm_clone_info` report
      `RES_STATUS_PROVISIONING` with `details = "side provisioning"`. Bitmap
      chunks pushed meanwhile are still
      persisted and counted as applied (DN15) and are applied when the
      dm-clone is finally created. Cancelling the migration inside this window
      is the DN9 cancel-and-wait path: the goroutine is stopped and waited for
      before `DnSideName` is removed.

      With the gate open, the §11.2 sequence —
      (1) per-CN stacks on dm-error, all namespaces `AnaGrpIdInaccessible`;
      (2) the dm-clone metadata slot — `ceil(size/DnCloneMetaUnit)`
      contiguous units in the [D13] clone-metadata area, its first 8 KiB
      zeroed **before** its record is persisted so a previous tenant's bytes
      cannot be misparsed as a dm-clone superblock — plus its wrapper
      dm-linear `DnMigrMetaDmName` (the dm-clone target reads its metadata
      device from sector 0 and takes no offset argument); (3) `nvme connect` to
      `MigrSrcNqn(cluster, migr_dst_conf.src_dn_id, sp, migr_id)` at
      `src_nvme_tr_conf` with hostnqn `DnHostNqn(cluster, dn_id)` (SH20);
      (4) dm-clone `DnMigrFinalName` (meta = the step-2 wrapper, dest = the
      side device, source = the nvme device, region size = `block_size`,
      features **`2 no_hydration no_discard_passdown`** — both are mandatory
      on **every** dnv dm-clone, dn and cn alike (`cnagent.md` CN18 step 3),
      because §9.6/§11.4 use `blkdiscard` on a
      dm-clone as the metadata-only "mark this region hydrated" primitive:
      dm-clone turns discard passdown on by default whenever the
      destination's discard granularity is no larger than one region — a
      dm-linear over a raw disk always satisfies that — and would then *also*
      remap the discard to the destination. The hazard is **after** the §11.2
      cutover, not before it: host IO already flows through the dst dm-clone,
      a host write hydrates region *r*, and a skip-bitmap chunk whose bit for
      *r* was read from the CN thin metadata before that write arrives later
      — pushes are legal at any time and a restart re-applies every stored
      chunk — so the resulting `blkdiscard` would destroy the only copy of an
      acknowledged write. Without passdown the same discard is the metadata
      no-op that §9.6 and [D7] assume. Knobs from
      `dm_clone_conf`); (5) reload the primary CN's dm-linear onto the
      dm-clone, move its namespace to `AnaGrpIdOptimized`, the standbys' to
      `AnaGrpIdNonOptimized`; re-apply every bitmap chunk of the applied set
      (SH21). The numbering above follows §11.2's logical steps; the
      implemented converge order differs without changing any end state
      (§4.6 builds bottom-up, pinned by §6 test 12's slot → connect → clone
      → linear → ana_grpid): `ensureMigrDst` runs steps (2)-(4) **before**
      the per-CN stacks converge, so on a pass where the connect succeeds
      the per-CN dm-linears are *created* directly on the dm-clone and the
      primary's namespace goes straight to `AnaGrpIdOptimized` — step (1)'s
      dm-error/`AnaGrpIdInaccessible` shape and step (5)'s *reload* occur
      only while the connect is still retrying across passes.
      "Retrying until success" (§11.2) is implemented without
      retrying inside the RPC: a converge pass attempts the connect
      **once**, and after a connect that succeeded it re-reads the subsystem
      until the source's namespace device is there, pausing
      `DnMigrDstNsPause` (50 ms) between reads and `DnMigrDstNsWait` (1 s) in
      all — the kernel returns from the
      connect once the controller is live and only queues the namespace scan
      that adds the device, so a single re-read could fail the target
      `"controller has no namespace"` for a device milliseconds away
      (*amended 2026-09-28*, the dn twin of `cnagent.md` CN10's head wait);
      a read that fails ends that wait at once. When the connect fails, or
      the namespace has still not appeared, the pass records
      `target_info = RES_STATUS_ERROR` and registers the
      side in a background retry registry that re-runs the destination
      converge every `DnMigrConnectRetryInterval` seconds under the DN1
      locks. It is deregistered on success, by the sweep's pre-step as soon
      as a destination role stops being wanted, and by the drop of a side
      whose pointer has left the list (DN6) — always by the state, never by
      remembering that it was once registered.

      **The deregistration runs inside the very converge it is ending, and
      that is the sharp edge.** Registering creates a cancellable context and
      deregistering cancels it — so the attempt the retry loop runs must not
      be the thing that context governs. It used to be: the loop converged on
      its own context, and the pass that finally connected reached the
      deregistration, cancelled itself, and then failed every remaining OS
      call of that pass on the dead context. The dm-clone was never created,
      `retrying` was already `false` so nothing ticked again, and the side sat
      at `dm_clone = RES_STATUS_MISSING, "target not connected"` **for ever**
      — while the controller that reading names was `live`. One transient
      connect failure was enough, which is why it survived: the unit test for
      the retry drove its second converge from an RPC, and an RPC-driven
      converge runs on the gRPC context, which the cancel cannot touch.

      So the loop's context governs the **loop**, not the attempt: each
      attempt reconverges on `rootCtx`, exactly as the §11.2 fence timer
      already did, and the cancel is read by the loop's own check one tick
      later at worst. Shutdown still stops an attempt in flight, because
      `rootCtx` is cancelled before `WaitBackground` joins (SH27). The
      regression test has to drive the successful connect **from the loop**,
      since that is the only shape in which "connect succeeded" and "cancel
      everything" happen in one pass in that order.

      **Retiring the role is the sweep's, and it starts with a repoint.**
      When `migr_dst_conf` goes — the §11.2 finish, a level at or above
      `SP_LEVEL_NO_MIGRATION`, or the side leaving its DN's list —
      `DnMigrFinalName`, `DnMigrMetaDmName` and the `:3:` connection stop
      being wanted and DN6's layers take them, clone before connection and
      wrapper after both, once every side of the sp this node may host is
      held. All three are identified by `(sp_id, migr_id)` —
      the `:3:` NQN carries the **source** DN's id, not this node's, so that
      pair is the only part of it which names the migration. Nothing in that
      NQN names *this* agent either, and the nvme host namespace is per
      kernel, so the connection is attributed first by its controller's
      `hostnqn` (`/sys/class/nvme/nvmeN/hostnqn`, DN6): a controller some
      other dn agent on this node opened is not a candidate at all. What
      that leaves is decided by the claim rule: no side of this DN whose
      stored request this process holds (it holds none for a side DN2's
      reload skipped) still names that `migr_dst_conf` under a wanted
      destination role. That
      is the whole replacement for the "applied destination" this used to
      diff against — a field the converge overwrote on its way past, so a
      removal that failed was forgotten by the pass that was meant to retry
      it.

      On the finish path the per-CN dm-linears are **kept** and still map the
      clone, so the sweep repoints each one onto the plain `DnSideName`
      first, deciding that it needs the repoint by reading the linear's
      **live table** rather than by remembering what it applied last time
      (DN6). Without that the clone's removal
      fails EBUSY under the linear and the whole chain waits a round for the
      build phase to repoint it. The metadata slot is released only after the
      wrapper's removal has been **verified**, and only when the record rule
      (DN6) also proves the slot orphaned; a slot whose wrapper would not go
      stays allocated and is retried by the next pass.

      **Chunks of a previous migration on the same side are quarantined.**
      The side records the `migr_id` its stored chunks belong to, and a
      destination converge whose `migr_id` differs drops them — files
      included — before step (2); a push naming another `migr_id` never gets
      that far (DN15). Migration ids are never reused, so a mismatch is proof
      the chunks describe a different copy, and applying them would
      `blkdiscard` regions this migration never copied, leaving the
      destination serving its own zeroed extents where the source's data
      should be.

      **The clone-metadata area is per DN, and it is a real ceiling.**
      `DnCloneMetaSize` (192 MiB = 48 `DnCloneMetaUnit`
      slots) is one region of the disk shared by every destination role this
      node hosts, across every SP on it. A migration costs
      `ceil((4 MiB + region_cnt bytes) / DnCloneMetaUnit)` slots with
      `region_cnt = side bytes / block_size`; the 4 MiB base alone is one
      whole unit, so every migration costs ≥ 2 — at most **24 concurrent
      destination roles per DN**, fewer for large sides at small block sizes
      (a 1 TiB side at 64 KiB regions costs 5). `MaxMigrCntPerSp` = 4 bounds
      none of this and the CP does not gate against it (architecture.md
      §8.11): exhaustion is reported as `RES_STATUS_ERROR` on the
      `migr_dst_info` rows — the dn twin of the cn arena ceiling in
      `cnagent.md` CN18.

DN14. Persist (SH5); reply `agent_reply` — the side-level sweep's verdict
      (DN19) — `revision`, `side_info` — including
      `zeroed_ext_cnt`/`total_ext_cnt`, filled on every reply (DN9) — and
      `bm_info` (the applied set, SH21).

### 4.7 `PushMigrBitmap`

DN15. Gate: the side must be known and its
      `migr_dst_conf.migr_id` must equal the request's `migr_id`
      (`ReplyCodeUnknownObject` otherwise — only a destination side accepts
      chunks). There is **no revision gate**: the request carries no
      `revision` field at all. A chunk is position-addressed data keyed by
      ids that are never reused, and applying one never advances the stored
      revision, so a push the worker planned against a report the agent has
      since superseded is harmless — while rejecting it threw away work the
      worker had already done and had to be re-driven by a whole re-sync
      (`dnv-worker.md` BM3). Then SH21-SH23: persist the chunk
      at `LocalMigrBmPath(cluster, dn, sp, migr_id, bm_idx)`, recompute from
      every chunk of the applied set (shift by the leg's `meta_blocks`),
      `blkdiscard` the
      fully-skippable regions of `DnMigrFinalName`. If the dm-clone does not
      currently exist — not built yet, suppressed by `sp_level`, or still
      behind DN13's provisioning gate — the file still counts as applied;
      chunks are re-applied whenever the dm-clone is (re)created. A **failed
      persist is still acked code 0**, with the error logged and neither the
      recompute nor the `blkdiscard` attempted (§9.6): not persisted is not
      applied, so the chunk stays out of the applied set — SH21 derives that
      set from the files present — the next `SyncupSide` reply's
      `bm_info.bm_idx_list` omits the index, and the worker pushes it again
      on a later round. Reply `agent_reply` only.

### 4.8 `GetDnInfo` / `GetSideInfo`

DN16. Read-only: probe fresh under the DN1 locks and reply `agent_reply`,
      `revision`, the info. An unknown DN or side pointer — one this
      process holds no request for, as when none was ever applied, or when
      the startup reload could not load or skipped its file (DN2) and none
      has been applied since — ⇒ `ReplyCodeUnknownObject` with
      `revision = 0`.

      For a **known** object the `agent_reply` is the **verdict**: the sweep
      of DN6 run with its removals left out — the same enumeration, the same
      wanted set, the same comparison, nothing touched — so `GetDnInfo`
      replies `ReplyCodeLeftover` while the node holds node-level leftovers,
      while one of its listings does not answer, and while the disk's identity
      is not confirmed (DN6's record step: every round for as long as the disk
      stays unreadable, corrupt, another node's, or blank under a live side
      device or clone-metadata wrapper, and the worker re-sends `SyncupDn`
      each time), and `GetSideInfo` while that side holds leftovers, while one
      of its listings does not answer, and — the one comparison a side's
      verdict makes beyond its sweep's — while its record, read from a table
      this node has confirmed (DN5), still has extents to zero and no DN9
      zeroing goroutine is running for it
      (`zeroing {k}/{n}: nothing is zeroing the side`). Only a converge starts
      that goroutine, and one that could not — the startup reconcile's, while
      no read of the disk answered — would otherwise leave the side reporting
      the same progress every round with nothing to re-send its `SyncupSide`;
      the registry and the record are both read fresh, so nothing about the
      failed converge is remembered. Both reply 0 otherwise. `GetDnInfo` and a
      `CheckDn` round probe before they take the verdict: the probe's read of
      the disk can be the first to answer, which is what confirms the identity
      (DN5). A DN's verdict covers the node-level scope only and a side's
      covers its own side; each drives its own `Syncup*`. The verdict is
      recomputed on every call and stored nowhere, so a leftover that has
      since gone stops being reported without anyone clearing a flag. A DN
      whose stored `extent_size` is unusable has **no** verdict — §7 says it
      converges nothing and sweeps nothing, so naming leftovers there would
      report objects this agent has deliberately refused to touch — and
      neither has a side whose DN is unknown or in that state, since its own
      geometry comes from the DN.

      Never mutates the node — in particular a `Get*Info` or `Check*` round
      never allocates a record and never registers a DN9 zeroing goroutine
      (registration happens only on a converge path), and it reports
      `RES_STATUS_MISSING` for a side whose record the converge has not
      written yet. That holds for the verdict too: it enumerates and compares,
      and removes nothing. What a round does change is the agent's own account
      of what it observed: the SH14 tracker, the disk size the allocator
      reads, and `DiskMeta`'s copy of the volume table, which a read that
      answers loads (that read can be the one that confirms the identity, DN5)
      and which a header other than the one it came from drops (DN18), so the
      converges, probes and DN9 goroutines that follow re-read the disk.

### 4.9 `CheckDn` / `CheckSide`

DN17. Instantiate the SH24-SH26 loop with the §4.10 probes; one round takes
      the DN1 locks of the corresponding `Get*Info` and carries the same
      read-only verdict in its `agent_reply` (DN16). That is what makes the
      retry worker-driven: while leftovers exist every round replies
      `ReplyCodeLeftover`, the worker re-issues the `Syncup*` that owns them
      (`dnv-worker.md` RW4), and the sweep inside it tries again — including
      after a restart, where the first Check round is what reports what the
      startup sweep could not remove. No agent-side retry loop exists,
      because the one the worker already runs is enough.

### 4.10 Probing and error capture — `probe.go`

DN18. Probe map (all via SH17 conventions; `res_name` and probe per
      resource). `RES_STATUS_PROVISIONING` rows are **healthy**: the resource
      is deliberately not created yet, no action is needed, and the worker
      never turns one into an `err_epoch` (§9.5);
      `RES_STATUS_ERROR` keeps meaning *needs intervention*.

| `ResInfo` | `res_name` | probe |
|---|---|---|
| `DnInfo.disk_info` | the `--disk` path | `lsblk --bytes --nodeps` succeeds |
| `DnInfo.meta_info` | the `--disk` path | `ReadBlock` of the 4 KiB header: magic, version, CRC and `cluster_id`/`dn_id`/`extent_size` identity — a header other than the one the loaded volume table came from (blank, corrupt, or valid with another identity or another `format_uuid`) also drops that table, so the next call re-reads the disk and nothing is handed out or written from the old one, and a blank disk is then formatted only as DN5 allows — until then every side's lookup finds no record (the `side_dev_info` row below); a read that did not answer changes nothing — **plus the DN5 Write-Zeroes check** (`/sys/class/block/{kname}/queue/write_zeroes_max_bytes` is absent, unreadable or ≠ 0). `details` = `"seq=%d sides=%d clone_metas=%d free_ext=%d free_meta_units=%d provisioning=%d"` when OK — the last count is sides whose `zeroed_bits` are still incomplete (DN9); on failure the error text instead, `"disk lacks Write Zeroes"` for the WZ case |
| `DnInfo.port_info` | the agent's port id as `%d` — `"1"` unless `--nvmet-port-id` says otherwise, so on a node running several agents the rows differ | configfs `addr_*` reads match the `--tr-*` flags; the three [D4] groups present with their fixed states |
| `SideInfo.side_dev_info` | `DnSideName` | the volume-table record + its `zeroed_bits` + `dmsetup table`, judged by the DN9 matrix: no record ⇒ `RES_STATUS_MISSING` at `provisioned = false` (no converge has allocated one yet, or the header has gone blank under the side: a blank header reads as an unformatted disk with no records, and the meta row's probe drops a table loaded before the header went blank — the side's device is still there all the same, and its DN9 goroutine, if one is running, ends at its next re-read of the record) and `RES_STATUS_ERROR`, details `"record missing"`, at `provisioned = true` (a lost or foreign disk, or that same blank header, under which the side's devices keep serving its data: nothing removes a device the side still wants, and DN5 will not format while they map the disk); bits incomplete ⇒ `RES_STATUS_PROVISIONING`, details `"zeroing {k}/{n}"`, at `false` and `RES_STATUS_ERROR`, details `"not zeroed"`, at `true`; at `false`, an outstanding batch failure ⇒ `RES_STATUS_ERROR` with the killed command's output, followed by the kill streak and the one-extent rate once DN9's backoff holds; a live table that does not match the record's extent runs ⇒ `RES_STATUS_ERROR`. The same read fills `SideInfo.zeroed_ext_cnt`/`total_ext_cnt` every round |
| `cn_id_to_dm_error[cn]` / `cn_id_to_dm_linear[cn]` | `DnErrorName` / `DnLinearName` | `dmsetup info` + `dmsetup table` (the linear's target — side device vs dm-error vs dm-clone — must match the desired role). Inside the §11.2 grace window the expected target is the **pre-fence** one and `details` is `"suspended (migration cutover grace window)"`; the probe never starts a window (DN16). While DN9's gate is closed no device is expected to exist and both report `RES_STATUS_PROVISIONING`, details `"side provisioning"` |
| `cn_id_to_nvmeof[cn]` | the `SideToCnNqn` | configfs: subsystem present, ns enabled, `ana_grpid` as desired. `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while DN9's gate is closed |
| `migr_src_info.dm_linear_info` / `.nvmeof_info` | `DnMigrSrcName` / the `MigrSrcNqn` | `dmsetup status` / configfs. With `migr_src_conf.dst_provisioned = false` neither object exists by design (DN12) and both report `RES_STATUS_PROVISIONING`, details `"side provisioning"` |
| `migr_dst_info.target_info` | the `MigrSrcNqn` | the SH20 **sysfs walk** (`/sys/class/nvme-subsystem` matched by `subsysnqn`, controller `state` under `/sys/class/nvme` — never `nvme list-subsys`, IR3) shows a live controller for it (liveness only, SH20); `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while the destination side is still zeroing (DN13) |
| `migr_dst_info.dm_clone_info` | `DnMigrFinalName` | `dmsetup status`; `details` carries the raw status line (§9.5 — hydration progress); `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while the destination side is still zeroing (DN13). The `DnMigrMetaDmName` wrapper has no `ResInfo` of its own: like the metadata LV before it, its health folds into this one |

DN19. Error capture (§9.1): a failed command marks that resource
      `RES_STATUS_ERROR` with the command output in `details` and the
      converge pass **continues** with the remaining resources — the agent
      converges as much as it can.

      **Leftovers are the one non-protocol outcome that travels in
      `agent_reply`**, and the reason is structural: the `*Info` rows are
      keyed by the ids of **wanted** objects, and a leftover is by definition
      something nothing wanted names, so it has no row to ride in. A pass
      whose sweep (DN6) could not remove everything, or whose enumeration did
      not answer, replies `ReplyCodeLeftover` with `details` = `leftover(n):
      kind:name, kind:name, …` — at most eight names, then `[+k more]` — and
      `enumeration failed: <what>: <err>` for each listing that did not
      answer. Two dn checks that are not listings are folded into the same
      outcome, in that same form, so that they drive the same re-send: DN6's
      record step on a disk whose identity is not confirmed, and — in the
      read-only verdict only — a side with extents still to zero and no
      zeroing goroutine (DN16). The full list goes to the agent log once per
      pass as the `sweep leftover` record (`log.md`), so a lingering leftover
      is visible every round rather than once.

      It is **not** a rejection (SH9). The request was applied, the desired
      state is stored, and the `*Info` rows are the converge's full account
      of every wanted object, so the worker evaluates them exactly as it does
      for code 0 and simply re-issues the `Syncup*` every round until the code
      changes (`dnv-worker.md` RW5, HL1). Everything else reported through
      `agent_reply` is still a protocol-level refusal.

## 5. Amendments applied to companion documents

Recorded for traceability; the edits are already applied.

* `architecture.md` — [D4] replaced: three **fixed** ANA groups per nvmet
  port (ids/states written once at setup; every transition rewrites the ns
  `ana_grpid`), superseding per-namespace group allocation; call-sites in
  §3.1, §3.3, §3.4, §8.7, §8.8, §11.1, §11.6 and the Appendix A nvmet block
  reworded to match.
* `osclient.md` — `WriteFileDirect` added to the `OsClient` interface
  (plain in-place write; nvmet-configfs attribute writes, where `WriteFile`'s
  atomic replace cannot work), with behavior, logging (`os write file
  direct`), reference implementation, fake and test updates.
* `layout.md` — cobra added to the planned dependencies (`dnv-agent` +
  `dnvctl` subcommand trees); the `agent/` recommended file split updated to
  the §2.1 shared-mechanism layout; `doc/` tree lists this document.
* `dependencies.md` / `layout.md` — `github.com/spf13/cobra` and
  `github.com/spf13/viper` moved from the planned list to the current direct
  dependencies: they enter `go.mod` with `cmd/dnv-agent`.
* `dnagent.md` DN9/DN11/DN18 (this document) — the `sp_level` read-only gate
  moved from the per-CN dm-linears to the side LV's permission. Found by the
  `dnagent_integtest.md` case B/C/D gated-destination step on real nvmet: a
  dm-linear created `--readonly` cannot back an enabled namespace (`EACCES`),
  so the previous wording made every level ≥ `SP_LEVEL_READONLY` unexportable.
  `agent/lvm.go` gained `LvSetPermission` and `LvEntry.ReadOnly`.
  *Superseded:* the [P1]/[P2] entry below deleted the LV-permission gate —
  `SP_LEVEL_READONLY` has no DN-side behavior — and the [D13] entry deleted
  the side LV and `agent/lvm.go` with it.
* `architecture.md` §9.4 + Appendix A and SH17/DN18 — the LVM JSON
  report option is spelled `--reportformat json` (one word); the earlier
  `--report-format json` matches no LVM build, whose `getopt_long` table only
  carries `--reportformat`. *Moot since [D13]/[D14]:* no dnv agent runs an
  LVM report, and SH17 names none.
* `architecture.md` §11.1/§11.2 + `dnagent.md` DN12 and [D12]
  (the since-retired `dnagent_plan_00.md`'s [P3]) — fencing is always a table reload onto a
  dm-error target, never a `dmsetup suspend` held across a wait. A suspended
  dm device queues IO forever (no timeout, no error path), which wedges any
  block-device scanner that touches it in unkillable D state, makes
  `dmsetup remove` fail, and defers writes that then replay at resume —
  possibly after hydration already copied that region
  (measured on the lab kernel). `sidePlan.linearSuspended` is gone, a migration source's per-CN
  dm-linears (the primary's included) now sit on their dm-error, and the
  §11.1 failover grace sleep and its constant are deleted.
* `architecture.md` §1/§2/§3.1/§4/§6.1/§8.11/§9.2/§9.4/§11.2/Appendix A/[D13],
  `dnagent.md` §2.1/§2.2/§2.8/§4.1/DN3/DN5/DN6/DN9/DN10/DN11/DN12/DN13/DN18,
  `layout.md` §2 (the retired plan's [P4]-[P7]) — **LVM is gone from the dn
  agent.** The `--disk` device now carries a self-describing dnv format:
  a CRC-protected header block, two alternating CRC-protected volume-table
  slots, a dm-clone metadata slot area, and an extent area at `DnDataOffset`
  — all read and written Go-natively through the new
  `OsClient.ReadBlock`/`WriteBlock` (never `dd`). The side LV becomes one
  aggregate dm-linear `DnSideName` over the side's extent runs; the dm-clone
  metadata LV becomes a slot plus a wrapper dm-linear `DnMigrMetaDmName`. The
  on-disk table is authoritative for placement, so a node that loses
  `--local-store` but keeps its disk recovers its layout. `agent/lvm.go` is
  deleted, `agent/dnagent/diskmeta.go` added, `DnInfo` becomes
  `{disk,meta,port}_info` and `SideInfo.lv_info` becomes `side_dev_info`.
  `GetDnSize` now reports the data area, so the §6.1 formula lost its
  `migr-pv` subtraction. Motivated by the measured failure
  class (an LVM scan that touches a bad dnv device wedges the node) and by the
  `scan_lvs = 1` host prerequisite the old migration VG needed.
* `architecture.md` §11.2 + `[D12]`, `dnagent.md` §2.2/DN12/DN18 — the
  §11.2 src cutover regained a **bounded** suspension: the per-CN dm-linears
  are held suspended for `common.SuspendSeconds` = 60 s before being reloaded
  onto their dm-errors, so IO the old primary still had in flight is absorbed
  instead of instantly failed. It is safe only because the reload installs
  dm-error *before* resuming, so the deferred bios are errored at the end of
  the window rather than replayed onto the side's data — the replay was the
  half of the original hazard that risked src/dst divergence. The other half
  (a suspended device wedging any block-device scanner in D state) is
  unchanged and is why the window is bounded, adopted as elapsed by an
  agent restart that finds its linears suspended, and always undone before
  teardown. Requested by the project owner after the retired plan's [P3] had
  removed the suspension entirely.
* `dnagent.md` DN9/DN11/DN18 + `architecture.md` §8.4/§11.7/[D11]
  (the retired plan's [P1]/[P2]) — `SP_LEVEL_READONLY` now means exactly
  "every user-facing namespace is read-only: reads served, writes fail with
  an IO error", enforced **on the CN only** by a dm-flakey `error_writes`
  table over the namespace's normal backing. The previous LV-permission gate
  is deleted: lab measurement showed that a read-only flag
  below the top of a stack does not stop dm-remapped writes, and the DN must
  keep serving md metadata/resync and §3.6 health-check writes anyway. The
  level therefore has **no** DN-side behavior, and clone/migration hydration
  is no longer paused at it — hydration is infrastructure IO, not user IO.
  `agent/lvm.go` lost `LvSetPermission`, `LvEntry.ReadOnly` and
  `lvAttrReadOnly`.
* `dnagent.md` DN13 step (4) + §6 test 12 (dm-clone features) — the dn
  migration dm-clone now passes **both** feature flags,
  `2 no_hydration no_discard_passdown`, exactly like the cn clone dm-clone
  (`cnagent.md` CN18 step 3); every dnv dm-clone carries the pair, with no
  exceptions. `blkdiscard` on a dnv dm-clone is the design's metadata-only
  "mark this region hydrated" primitive (§9.6, §11.4, §11.5), and dm-clone
  turns passdown on by default whenever the destination's discard granularity
  is ≤ one region — which a dm-linear over a raw disk satisfies. The hazard is
  **after** the §11.2 cutover, not before it: host IO already flows through
  the dst dm-clone, so a late skip-bitmap chunk's discard of an
  already-hydrated region would reach the side device and destroy the only
  copy of an acknowledged write. The previous `migr.go` comment ("freshly
  trimmed, never serves host IO before the cutover") argued about the wrong
  window. `agent.CloneTable`'s `noDiscardPassdown` is now `true` at both call
  sites.
* `dnagent.md` §2.1/SH17/§7 item 6 ([D14]) — LVM left the **cn**
  too ([D14]: the clone VG became a slot allocator over one loop device with
  kind-`cb` wrapper linears), so this document's dn-only statements are
  generalized: no dnv agent runs any LVM command, SH17 lists no LVM report
  option, and the acceptance grep is repo-wide instead of `agent/ cmd/`.
* `dnagent.md` SH20 + `cnagent.md` §2.3 —
  `DisconnectDevice`'s controller device is located by the **sysfs walk**
  (`/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` to match the NQN, its
  `nvme{N}` entries as the controllers, `/sys/class/nvme/{ctrl}/address`
  parsed as comma-separated `key=value`), never by `nvme list-subsys -o json`.
  That is what the code has always done (`agent/cnagent/leg.go`
  `readSubsys`/`readCtrl`), and list-subsys cannot serve here: it reports no
  `ANAState` without a namespace device argument and answers an
  all-`inaccessible` namespace with an empty subsystem list.
* `dnagent.md` §2.3 SH27 + the `Serve` reference implementation + CM4/§4.2
  — background tasks became part of the specified
  lifecycle: every role-server goroutine derives from `rootCtx` (which `Serve`
  now derives from its own ctx and cancels before returning) and mints a fresh
  trace id per attempt, and every goroutine that owns a **child process** is
  registered in a `sync.WaitGroup`. `agent.Serve` gained a
  `waitBackground func()` parameter and calls it after `GracefulStop`; the dn
  passes `srv.WaitBackground`, the cn passes `nil` (a CN11 prober's
  uninterruptible syscall must be cancelled, never joined). Forced by DN9's
  zeroing goroutine — the first background task that runs a long-running child
  (`blkdiscard --zeroout`), which orphaned would keep writing to a device the
  agent no longer manages.
* `dnagent.md` §2.2/SH14/DN2/DN5/DN6/DN9/DN10/DN11/DN12/DN13/DN14/DN15/DN16/
  DN18 + §6 + §7 ([D15]) — **the §9.4 trim protocol is replaced by
  whole-side zeroing behind a `provisioned` gate.** `blkdiscard` is not a zero
  guarantee (the kernel dropped `discard_zeroes_data` in 4.12; NVMe DLFEAT
  read-zeroes is optional), so the trim funded neither dnv's multi-tenant "no
  tenant reads another tenant's bytes" requirement nor the places the design
  assumes zeros — a recycled extent can carry a previous SP's valid thin-pool
  superblock, or a stale md superblock that flips CN12 into the wrong assembly
  case. Every side is now zeroed with `blkdiscard --zeroout` in
  `DnZeroBatchExtCnt`-extent batches by a background goroutine, progress is
  tracked per logical extent in the volume table's `zeroed_bits`, and nothing
  is exported until the request's `side_conf.provisioned` is `true` **and**
  every bit is set; allocation is legal only at `provisioned = false`, so a
  missing record at `true` is data loss and stays a hard `ERROR`. Schema:
  `SideRecord.trimmed` (field 3) reserved, `zeroed_bits = 5` added;
  `Side.provisioned = 6`, `SideConf.provisioned = 6`,
  `MigrSrcConf.dst_provisioned = 4`, `SideInfo.zeroed_ext_cnt = 7`/
  `total_ext_cnt = 8`, `ResStatus.RES_STATUS_PROVISIONING = 4` (healthy, not
  ready, never an `err_epoch` — which is why SH14's status list grew). Code:
  `SetSideTrimmed` becomes the range setter `SetSideZeroed`, the probe detail
  `"not_trimmed"` becomes `"zeroing {k}/{n}"`, `Describe()` gained a trailing
  ` provisioning=%d`, `AllocSide`'s error is no longer discarded by
  `syncup_side.go` (the matrix needs allocated / found / must-not-allocate to
  stay distinguishable, and the swallowed error used to surface as a
  misleading "side size is 0"), `common/constants.go` gains
  `DnZeroBatchExtCnt = 10` and `DnZeroRetryInterval = 5`, and DN5 gained the
  `write_zeroes_max_bytes` fail-fast that keeps the fast-Write-Zeroes hardware
  assumption honest.
* `dnagent.md` DN18 + §2.3 + DN13 + SH15/SH20 — the
  `migr_dst_info.target_info` probe row now names the SH20 sysfs walk (the
  IR3 amendment had corrected SH17/SH20 but left the old
  `nvme list-subsys -o json` wording in the DN18 table); the §2.3
  `agent.Serve` reference implementation matches the shipped code, whose
  cancel-then-join pair moved into a `defer` covering every return path (a
  reconcile or listen error also winds the background down before Serve
  returns); DN13 records the per-DN clone-metadata slot ceiling (≤ 24
  destination roles); and SH15/SH20 record that the nvme-host sysfs reads
  carry the SH15 soft timeout like every other OS touch — the amendment
  closed that gap, wrapping `agent/nvmehost.go` `readTrimmed` through
  `cmdCtx` and the `agent/cnagent/leg.go` sysfs walk through the newly
  exported `agent.CmdCtx`.

## 6. Tests

Unit tests use `common.FakeOsClient` (scripted `RunCommandFn`/`ReadFileFn`/…
recording every call) and, for RPC-level tests, `bufconn` with the generated
`DiskNodeAgent` client — no root, no real devices.

**The fake honours the context**, as of 2026-09-19, because not honouring it
made a whole class of bug invisible by construction. The production client
runs every command through `exec.CommandContext` and tests `ctx.Err()` at the
head of each file operation, so a converge whose context dies part-way stops
doing work there; the fake used to ignore the context entirely, so the same
converge ran to completion under test. A DN8 retry pass that cancelled its
own context (see §5 DN13) therefore stalled a real lab for ever while its
unit test passed. A cancelled context now returns that error from
`runCommand` and `readFile`, which is what makes the regression test for it
able to fail.

1. **Fresh SyncupDn**: scripted empty probes; assert the DN5 sequence
   (`readblock` of the header, `writeblock` of table slot A, `writeblock`
   of the header — DN5's slot-A-first order — then the port attrs,
   `mkdir ana_groups/2`+`3`, the three
   one-time `ana_state` writes), with the `WriteProto` to `LocalDnPath`
   recorded **first**, ahead of the whole converge (SH5). No LVM string
   (`pvcreate`/`vgcreate`/`lvcreate`/…) appears anywhere in the recorded
   calls.
2. **Revision gate**: lower ⇒ `ReplyCodeStaleRevision` and zero mutating
   calls; equal ⇒ full idempotent pass; higher ⇒ apply + persist.
3. **Probe-first idempotency** (SH16): equal-revision `SyncupSide` against
   probes reporting a fully converged side issues no mutating command — which
   now also proves the disk-metadata re-load issues no `writeblock` — and, on
   a side whose `zeroed_bits` are all set, no `blkdiscard` of any form.
4. **Pointer diff**: `SyncupDn` dropping a side ⇒ the `rm` of its files
   **first**, then DN6's sweep finding its resources by name and removing
   them top-down (export, per-CN dm-linear, per-CN dm-error, `DnSideName`,
   the record's slot write) — the whole order is the assertion, because the
   teardown this replaced deleted the file **after** a removal pass whose
   failures it discarded; `SyncupSide` for an unknown pointer ⇒
   `ReplyCodeUnknownObject`.
   **Co-hosted agents** (`cohost_test.go`) pin the other half of DN6's
   attribution rule, the half a suite in which every object belongs to the
   agent under test cannot reach: a node-level sweep run over a configfs tree
   and an nvme host namespace that also hold a SIBLING agent's objects. A
   `:2:` export whose namespace backs onto another dn's kind-`d1` linear, and
   one with no namespace linked to another agent's nvmet port, both survive;
   a `:3:` connection opened with another dn's host NQN is not disconnected.
   Each is paired with the same fixture built under this agent's own ids,
   which **is** removed, so no arm can pass by sweeping nothing — and the
   fixture converges a side of its own first, because the sp of that side is
   what puts the sibling's export past the enumeration's cheap `hosted` gate
   and in front of the attribution rule at all. The namespace-less export on
   this agent's own port is removed only when it is seeded **older** than
   `DnExportOrphanGrace` — the fake reads every directory it did not stamp
   itself as created now, so a fixture that means an old one says so with
   `ageDir` — and its young twin is kept (*amended 2026-09-28*).
   **The age gate** (DN6, *added 2026-09-28*;
   `TestHalfBuiltExportAgeGate`) runs in both scopes — a `SyncupDn` over a
   sibling leg's export, a `SyncupSide` over an export of the side's own leg
   to a cn it does not export to — a namespace-less export linked to no
   port, with and without its allowed-host link: young, it is not removed,
   its host link survives, and neither the pass nor the read-only verdict
   reports it; old, the read-only verdict names it as a leftover and the
   pass removes it, host link and all. The fake models the lab kernel's
   stamp (a read or a write of an `attr_*` moves the subsystem directory's
   mtime), so a sweep that read an orphan's attributes before judging its
   age would keep it young for ever and fail the old arms.
   `TestHalfBuiltExportAgeUnreadIsForeign`: an old export whose `stat` does
   not answer is kept, the verdict names
   the unread age, and the next pass, with the `stat` answering, removes
   it. **The own-leg rule** (`TestSideScopeJudgesItsOwnLegOnly`): a
   side-level pass leaves another leg's export alone in every shape — an
   old orphan with no port link or on this agent's port, a young one, one
   backing this agent's `d1` of a side in no list, one backing a
   sibling's — and issues not one call naming it, while the node-level
   pass that follows removes exactly the three it owns or can prove
   abandoned. **The attribution's cost**
   (`TestNodeSweepCostIsLinearInOwnExports`): four sibling agents of one
   sp hold one export each, then eight each, then thirty-two agents hold
   one each; a Check round's verdict and a `SyncupDn` over them run the
   same number of commands, `ls` and all, every time, each reads every
   sibling export's namespace 1 exactly once, and every sibling export
   survives.
5. **Side provisioning (zeroing) protocol** (DN9): the allocation slot write
   (a record whose `zeroed_bits` are all 0), the `dmsetup create` of
   `DnSideName` (stdin table form), then one
   `blkdiscard --zeroout --offset {o} --length {l} /dev/mapper/{DnSideName}`
   per batch — asserted with the **exact** offsets and lengths of
   `common.DnZeroBatchExtCnt`-extent batches in ascending order, the last one
   short when `ext_cnt` is not a multiple of the batch — each followed by that
   batch's `SetSideZeroed` slot write. In that order, and with **no nvmet
   command anywhere before the final batch's bits are persisted**. A
   `blkdiscard --force` of the side device never appears (the trim protocol is
   gone) and `--zeroout` never appears anywhere else. No test asserts an LV
   permission — dnv sets none ([D11]).
6. **ANA moves**: a `primary_cn_id` flip rewrites exactly the two namespaces'
   `ana_grpid` via `WriteFileDirect` and never writes any
   `ana_groups/*/ana_state`; no `WriteFile` call ever targets a
   `/sys/kernel/config` path (SH18).
6b. **cntlid slots** (DN10, `architecture.md` §11.8;
    `TestCntlidSlotsAreDisjoint`): a side converged on each of the
    `DnCntlidSlotCnt` slots, one fresh node each, writes the same
    `attr_cntlid_min`/`attr_cntlid_max` into both per-CN exports; slot *s*
    reads back `10000 + s×5000` to `10000 + s×5000 + 4999`, and no two
    slots' ranges share a CNTLID. `TestCntlidRangeMovesBetweenSlots` walks
    one side across slots on one node (up by one, up by more, down by one,
    down by more, then unchanged), against a fake that, like nvmet, refuses
    a min above the current max and a max below the current min. Both
    exports converge to the new slot every time; a move up writes
    `attr_cntlid_max` first and a move down `attr_cntlid_min` first, each
    bound exactly once, and an unchanged range writes neither.
7. **PushMigrBitmap**: persist-before-apply call order; unknown `migr_id`
   rejected; applied set from files after a simulated restart (fresh server,
   same fake store) matches; chunk without a dm-clone still counts applied.
8. **CheckDn/CheckSide**: first reply carries full info; an unchanged round
   with `show_info = false` omits it; a probe flipped to error re-includes
   it (a wiped disk header is the DN case); reply `revision` echoes the
   stored one; unknown object ⇒ `ReplyCodeUnknownObject` with the stream kept
   open (SH25). A Check round never mutates — `writeblock` included — and
   never registers a zeroing goroutine (DN16).
   **Each round's trace id** (SH24; `TestCheckRoundsCarryTheRequestTraceId`):
   on a `CheckDn` and a `CheckSide` stream, each opened under a metadata
   id of its own, two rounds run their commands and reads under their own
   requests' `trace_id`s and none under the stream's; a round with an empty
   `trace_id` runs them under the stream's.
   **A header read that did not answer at startup** (DN5, DN6, DN16;
   `TestATransientHeaderReadDoesNotFreezeTheSides`): after a restart whose
   DN converge had its header read killed, the startup side converge's own
   read confirms the identity, a failover flip of a side the table records
   converges before any check round, moving the new primary's namespace to
   the optimized group exactly once, and the first `CheckDn` round is
   clean; after a restart during which no header read answers, a `CheckDn`
   round replies `ReplyCodeLeftover` naming the disk identity, and once the
   disk answers the first `GetDnInfo` replies 0 with `meta_info` `OK` — its
   probe runs before the verdict — and the next flip converges.
   `TestATransientHeaderReadKeepsALiveCloneServing`: the same restart under
   a live migration destination, and a `SyncupSide` after it, issue no
   reload of the primary's dm-linear, which stays on the dm-clone, and
   `dm_clone_info` reads `OK`.
   `TestATransientHeaderReadKeepsZeroing`: a side a stopped process left
   at zeroing 0/n finishes after the same restart with no check round,
   each batch's `blkdiscard --zeroout` issued exactly once.
   `TestASideLeftWithNothingZeroingIsReDriven`: the same side after a
   restart during which no header read answers until the startup
   reconcile is over has nothing zeroing it; the first `CheckSide` round
   replies `ReplyCodeLeftover` naming the zeroing, the `SyncupSide` it
   re-drives starts the goroutine, the side finishes with each batch issued
   exactly once, and the next verdict is clean.
   `TestAForeignRecordIsNotZeroingToReDrive`: a second agent under another
   dn id, finding the owner's record for the same ids still at zeroing
   0/n, gets no zeroing in its side verdict — its `CheckDn` verdict names
   the disk identity instead.
   `TestAZeroingLoopIssuesNoBatchUnderAForeignHeader`: a header rewritten
   with another dn id under a running zeroing loop, found by one `CheckDn`
   round, stops the loop's batches — no `blkdiscard --zeroout` and no block
   write after the round — and `side_dev_info` reads `RES_STATUS_ERROR`
   naming the foreign disk.
   `TestForeignDiskIsNeverWritten`: a second agent syncing the same disk
   under another dn id, its DN converge's header read answered or killed,
   has its side converge refused with a foreign-disk error: no block write,
   no dm device created, reloaded or removed, and the owner's record
   intact.
   **A blank header under a live side** (DN5, DN18;
   `TestABlankHeaderUnderLiveSidesIsNeverFormatted`): a header zeroed under
   a converged side makes the `CheckDn` verdict a leftover; the `SyncupDn`
   it re-drives refuses to format, naming the side device, and a new
   side's converge is refused `"disk is not formatted"`; nothing is
   written to the disk and the live stack is untouched until both sides
   leave the DN, whose sweep removes the live side's devices, and the next
   converge formats with exactly two writes, slot A then the header.
   `TestABlankHeaderIsNotFormattedWhenTheGateCannotRead`: with the live
   side's devices gone, a killed `lsblk` of the disk's device number, a
   killed `dmsetup ls`, or a killed `dmsetup table` of another dn's side
   device each make the converge refuse to format, with no block write;
   the next converge formats with exactly two writes.
   `TestABlankHeaderIsNotFormattedUnderAnotherNodesDevice`: a side device
   of another dn, one of another cluster, and another dn's clone-metadata
   wrapper, each mapping the disk, keep it blank round after round — the
   refusal names the device, the verdict is a leftover, nothing is written
   or removed — and once the device is gone the next converge formats.
9. **sp_level**: `SP_LEVEL_NO_SIDE` removes exports but keeps dm + the side
   device; `SP_LEVEL_DISABLE` keeps only the side device and its record;
   lowering back rebuilds (DN11). `SP_LEVEL_READONLY` and the three CN-only
   levels below `SP_LEVEL_NO_MIGRATION` are no-ops on the dn: a converged
   side re-synced at any of them issues no mutating call and keeps every
   export OK ([D11]). A migration destination converged at
   `SP_LEVEL_READONLY` still ends up hydrating — `disable_hydration` is never
   sent.
10. **Lock mapping smoke** (DN1): a `SyncupSide` blocked in a slow scripted
    command does not block a concurrent `CheckDn` round or a `SyncupSide`
    for a different side, but does block one for the same side.
11. **Cutover fence** (DN12, [D12]): with the window open, the source's
    per-CN dm-linears are **suspended in place** — a `dmsetup suspend`, never
    a reload, and their tables still point at the pre-fence targets — the RPC
    returns without waiting, and the info reports OK with the grace-window
    detail; a probe inside the window mutates nothing. With the window
    elapsed they are reloaded onto their dm-errors and resumed. A timer ends
    the window unattended; cancelling the role, dropping the side from its
    DN's list, raising the level to `SP_LEVEL_NO_SIDE` and restarting the
    agent — one whose probes find the linears suspended — all end it without
    leaving anything suspended, and a sweep inside the window, unless its
    request also ends the source role, puts each fenced linear on its
    dm-error and resumes it before it disables any namespace above it
    (*amended 2026-09-28*; `TestTeardownInsideTheFenceWindow`,
    `TestNoSideInsideTheFenceWindow`).
    The fake names every namespace disable issued over a suspended device —
    a write that does not return while the device holds any of that
    namespace's IO — and records the table each resume of a suspended
    device releases the deferred IO against, which must be the dm-error; at
    `SP_LEVEL_NO_SIDE` the linears stay, nothing suspends them again, and
    the reply and a later probe both read them OK outside the window. A
    linear the sweep cannot bring out of suspension — its reload refused,
    or killed before or after the ioctl — holds every export, named as a
    leftover, until the next pass removes them, and is still retired onto
    its dm-error, never resumed onto its pre-fence table, by the same
    pass's build phase (`TestNoSideRetireFailureHoldsTheExports`) or, when
    that pass stops at the DN9 gate, by the gate's fence bookkeeping
    (`TestNoSideRetireFailureAtTheGate`). A linear whose probe does not
    answer holds the exports too, named as leftovers, until a pass whose
    probe of it answers takes them, the linear retired onto its dm-error
    first (`TestNoSideProbeFailureHoldsTheExports`). A fenced linear whose
    export the pass cannot attribute — its `device_path` read did not
    answer — is still retired onto its dm-error before L2 tries to remove
    it (`TestTeardownInsideTheFenceWindowOverAnUnreadExport`). The window the
    level ended stays over when the level comes back down — nothing
    suspended, the exports rebuilt over the dm-errors, a probe reading every
    row OK (`TestNoSideThenLevelDownOpensNoSecondWindow`) — but not past the
    role: the side's next migration gets its whole window
    (`TestFenceEndedDoesNotOutliveTheRole`). A
    fence **adopted** across a restart settles even when the converge that
    adopts it stops at the DN9 gate: every per-CN linear is reloaded onto its
    dm-error and resumed, and no timer is armed for a window that is already
    elapsed. The production default is pinned at
    `common.SuspendSeconds` = 60.
12. **Migration endpoints**: the destination sequence asserts the 8 KiB
    zeroing `writeblock` at the slot offset **before** the record's slot
    write, the wrapper `dmsetup create`, that the dm-clone's meta/dest devices
    resolve to the wrapper and the side device, and that the created table
    carries **`2 no_hydration no_discard_passdown`** — the dn role package's
    copy of the assertion the cn package already makes;
    removal is asserted on both paths that reach it — the whole order
    clone removal → disconnect → wrapper removal → the `FreeCloneMeta` slot
    write → side-device removal when the side leaves its DN's list, and
    clone-before-disconnect with the wrapper and its slot gone when the
    destination role ends while the side stays, that one with the primary's
    dm-linear repointed onto `DnSideName` first (DN6, DN13).
    The source sequence asserts
    ana_grpid-inaccessible → the per-CN dm-linear **reload onto its
    dm-error** → the migr-src create, with nothing left suspended ([D12]).
    The **DN8 retry** is pinned twice, and the pair is the point. One test
    drives the second converge from an RPC and asserts the transition it
    produces (dm-clone create → dm-linear reload → `ana_grpid` optimized); the
    other issues **no second RPC at all** and waits for the retry LOOP to get
    there, asserting only that the dm-clone exists. Only the second can fail
    when the pass that connects cancels its own context (DN13), because an
    RPC-driven converge runs on the gRPC context, which that cancel cannot
    reach — and it can only fail because the fake honours the context (§6
    preamble). `migrRetryInterval` is a server field for this, the way
    `zeroRetryInterval` already was.
    **The wait for the source namespace** (DN13 step (3), *added
    2026-09-28*; `TestMigrationDestinationAwaitsTheSourceNamespace`) runs on
    a fake clock through the server's `now`/`sleep` seams, with the fake
    host's namespace node deferred by N listings of its subsystem
    directory (`nsMisses`): a namespace that appears after one or after
    three missed reads leaves `target_info` and `dm_clone_info` `OK` on the
    first reply after exactly one connect and exactly that many
    `DnMigrDstNsPause`s, and registers no DN8 retry; one that never appears
    costs exactly `DnMigrDstNsWait` of pauses and ends as the single re-read
    did,
    `"controller has no namespace"` with the retry registered; and a read
    that fails after the connect ends the wait with no pause, the target
    carrying the read's error.
    **A lost store** (DN6, DN8, *added 2026-09-29*;
    `TestLostStoreKeepsAMigrationSourceUntilItsSideIsKnown`,
    `TestLostStoreKeepsAMigrationDestinationUntilItsSideIsKnown`) restarts
    the agent over the same node with an empty store and sends the
    `SyncupDn` that lists the side ahead of the side's own request: no
    mutating call names the source's `DnMigrSrcName` or `MigrSrcNqn`, or
    the destination's dm-clone or wrapper, no `nvme disconnect` runs, all
    of them survive, and both the reply and the read-only verdict are
    clean. The side's `SyncupSide` then finds everything in place and
    issues no mutating call but its own store write, with the migration
    rows `OK` — the §11.2 window off, as `startTestServer` leaves it; with
    it on, the source's pass would also open a second window over the
    dm-errors, DN12 rule 1's known limit. The destination case is what pins
    the connection's gate: with only the dm-clone kept out of the chain, L3
    disconnects the source under it. The startup twin
    (`TestReconcileKeepsAMigrationSourceWhoseSideFileIsLost`) keeps the dn
    file and loses only the source side's, so the startup reconcile's own
    removing sweep meets that side by its pointer alone: no mutating call
    names the source's `DnMigrSrcName` or `MigrSrcNqn`, and both survive.
    With a second side of the sp on the node
    (`TestLostStoreSiblingSideKeepsAMigrationSource`,
    `TestLostStoreSiblingSideKeepsAMigrationDestination`), the `SyncupSide`
    of the side not playing the migration comes first after the restart's
    `SyncupDn`: neither its pass nor its read-only verdict removes or names
    the migration objects — no mutating call names them, no `nvme
    disconnect` runs, both codes are 0 — and the migrating side's own
    `SyncupSide` then issues no mutating call but its store write. The
    destination case pins the side-level connection's gate the same way.
13. **`diskmeta`** (`diskmeta_test.go`, on the fake's segment store): format
    and load round-trip; probe-first idempotency (zero `WriteBlock` on a
    formatted disk); identity mismatch refused; corrupt-header refusal; A/B
    slot alternation across three saves; a torn newest slot falling back to
    the older one; a stale slot rejected after a re-format because of
    `format_uuid`; both slots invalid under a valid header ⇒ a hard
    "corrupt volume table" refusal with no allocation and zero writes (DN5's
    slot-A-first order makes a valid header imply a valid slot, so this is
    corruption, never a fresh-format crash); a failed save not committing in
    memory; allocation
    contiguity, the fragmentation fallback, the ext-count-mismatch error,
    `SetSideZeroed` range setting (a partial range, an idempotent re-set that
    issues no write, a range outside the record rejected) and
    exhaustion of both areas; clone-metadata zeroing ordered before the table
    write; free idempotency; `Describe` counts (the trailing `provisioning=`
    one included); and the envelope layout
    itself (magics, version, seq, a non-zero `format_uuid`, and that the
    layout constants tile without overlap up to `DnDataOffset`).
    **The identity gate** (DN5, DN18):
    `TestDiskMetaIdentityGateComparesTheLoadedHeader` — before any DN
    converge has asked for an identity, neither an existing side record nor
    an existing clone-metadata record is handed out; after a converge whose
    header read was killed, the first read that answers decides: another
    node's header refuses both with a foreign-disk error, this node's hands
    both out without a write and allocates a new record. The confirmed
    lookup DN9's zeroing loop reads through answers the same way in all
    three cases — refused before any converge has asked and on another
    node's disk, handed out on this node's — while the reporting lookup
    still finds the foreign record, and a record the foreign table does not
    hold is absent rather than refused.
    `TestDiskMetaProbeDropsATableTheDiskNoLongerHolds` — a probe that finds
    the header wiped, corrupt, another node's or re-formatted leaves the
    identity unconfirmed and drops the loaded table: the next lookup re-reads
    the header exactly once and answers from what the disk now holds (no
    record once it is wiped or re-formatted, an error once it is corrupt);
    one that did not answer keeps both, with no re-read.
14. **`GetDnSize`**: the reply is `disk size − DnDataOffset`; a device at or
    below `DnDataOffset`, and a failing `lsblk`, both report through the gRPC
    status (DN3).
15. **cmd**: the `architecture.md` §13 example `dnv-agent dn …` invocation
    parses; `--disk` is
    required for `dn` and absent from `cn`; env `DNV_AGENT_GRPC_ADDRESS`
    overrides the flag default (CM3).
16. **Converge matrix** (DN9): one `SyncupSide` per row of the DN9 table
    against a scripted store/volume-table state asserts both the behavior
    column (which commands ran) and the `side_dev_info` status/details column.
    The two hard rows in particular: `provisioned = true` with incomplete bits
    ⇒ `RES_STATUS_ERROR`/`"not zeroed"`, **no** nvmet command, and the
    goroutine still registered; `provisioned = true` with no record ⇒
    `RES_STATUS_ERROR`/`"record missing"`, **no allocation slot write at all**
    and no `DnSideName` create.
17. **Resume at k**: after a simulated restart (fresh server, same fake store
    and disk-segment table) with `k` of `n` bits set, the first `--zeroout`
    covers `[k, k+DnZeroBatchExtCnt)` — the zeroed prefix is never rewritten —
    and the remaining batches follow in order; the restart test asserts the
    batch order and reads no reply. The `zeroed_ext_cnt = k`,
    `total_ext_cnt = n` reply is asserted by test 16's converge matrix, on
    its `provisioned = false`, partial-bits row (`unprovisioned/partial`),
    without a restart.
18. **Paced retry**: a scripted `blkdiscard` failure leaves that batch's bits
    unset, puts the command output into `side_dev_info`
    (`RES_STATUS_ERROR`, outranking `PROVISIONING`), and the next attempt
    comes no sooner than `common.DnZeroRetryInterval` — shortened through the
    same field-not-constant trick DN12's fence wait uses — never a hot loop;
    the following success clears the error back to `PROVISIONING`, read
    while the batch after it is parked by `blockCmd`: a fully zeroed side
    never consults the zeroing error, so a read after the last batch could
    not see the clear.
    **The batch follows the kills** (`TestZeroingBatchHalvesAfterAKill`,
    `TestZeroingBacksOffAfterRepeatedKills`, `TestZeroingHalvingRules`),
    asserted as the exact list of batches, count included: a `killCmd` on a
    batch makes the next one half of it, the success after that doubles it
    back and `DnZeroBatchExtCnt` bounds the doubling, a kill after a success
    halves again rather than backing off, a refused batch is redone at its
    own size, a refusal between two kills ends the streak so the second
    kill halves too, a killed short last batch is halved from its own
    count, a killed one-extent batch is redone at one extent, never zero,
    and `DnZeroKillBackoff` kills in a row drop the side to one extent per
    batch. The side is also read while a batch is parked in its child, and
    the probe and the converge agree every time: after one kill,
    `RES_STATUS_ERROR` with the killed command's output alone, no backoff
    note; after the backoff, `RES_STATUS_ERROR` whose details carry the
    command output, the streak and the rate; and once the backed-off batch
    has succeeded, with the batch after it parked in turn, `PROVISIONING`,
    `"zeroing 1/n"`. **The concurrency cap**
    (`TestZeroingConcurrencyIsCapped`): with every batch parked by
    `blockCmd`, `DnZeroConcurrency + 2` zeroing sides put
    `DnZeroConcurrency` batches in flight and no more within a 500 ms
    window, every side reports `PROVISIONING` meanwhile, and once released
    each side zeroes in its own batches, none redone. **Paced sides hold no
    slot** (`TestZeroingPacedSidesHoldNoSlot`): with the retry pace at an
    hour and the first batch of `DnZeroConcurrency` sides refused whenever
    it runs, each of those sides runs it once and holds no slot while it
    waits in the pace, so another side zeroes to completion in its own
    batches meanwhile. **A queued side**
    (`TestZeroingQueuedSideTearsDownAtOnce`): with every slot held by a
    parked batch and the last side's loop parked in its slot wait (read off
    the runtime's goroutine dump, since that loop has run no command), the
    `SyncupDn` that drops that side returns within 2 s and removes its
    `DnSideName`; the dropped side never runs a batch, and once released the
    kept sides zero in their own batches, none redone.
19. **Cancel and wait**: dropping the side from its DN's list (DN6) while a
    batch is in
    flight cancels the goroutine and **waits** for it; the ordering assertion
    is that the `dmsetup remove` of `DnSideName` is recorded strictly after
    the in-flight `blkdiscard` returned, and the pass's node **write** lock
    never deadlocks against the goroutine's table update (DN9's try-acquire
    rule). The same path reached on a migration destination still zeroing
    (DN13) has no test of its own. SH27's join is asserted at the mechanism
    level only: `agent`'s `TestServeJoinsBackgroundOnEveryReturnPath` drives
    `Serve` with a hand-rolled goroutine rooted at the reconcile ctx and
    asserts that a listener failure and a reconcile failure each cancel and
    then join it before `Serve` returns. No Go test drives a dn server, a
    zeroing goroutine or a `blkdiscard` child through `Serve`; in
    `agent/dnagent` the cancel-then-`WaitBackground` pair is test
    scaffolding (`startTestServer`'s cleanup and `stopTestServer` run it),
    not an assertion.
20. **Write Zeroes fail-fast** (DN5): a scripted
    `/sys/class/block/{kname}/queue/write_zeroes_max_bytes` of `0` makes
    `SyncupDn` report `meta_info = RES_STATUS_ERROR` with `"disk lacks Write
    Zeroes"` while the rest of the converge still runs (DN19); a non-zero
    value and an absent attribute both converge normally (the test scripts
    absent, `0` and `2097152`; an unreadable attribute has no case of its
    own, because the attribute read reports it as absent — DN5).
21. **Export gate and level independence**: at `provisioned = false` with all
    bits set, `side_dev_info` is `RES_STATUS_OK`, every per-CN row is
    `RES_STATUS_PROVISIONING`/`"side provisioning"`, and no nvmet object
    exists (DN9 step 4, DN10); the same side re-synced at `SP_LEVEL_DISABLE`
    while bits are still missing keeps issuing its zeroing batches (DN11).
22. **Migration gates** (DN12/DN13): a request whose `migr_src_conf` carries
    `dst_provisioned = false` produces **byte-for-byte the same recorded call
    set** as the same request with `migr_src_conf` omitted — no `ana_grpid`
    write, no suspend, no `DnMigrSrcName` create — and reports
    `migr_src_info.*` as `RES_STATUS_PROVISIONING`; flipping it to `true` runs
    the DN12 sequence. A destination side whose own `provisioned` is `false`
    allocates, builds the linear and zeroes, and issues **no** metadata-slot
    write, **no** `nvme connect` and **no** dm-clone create, with
    `migr_dst_info.*` `RES_STATUS_PROVISIONING`.
23. **A zero conf member is refused** (DN4, §2.1): a `SyncupDn` whose
    `extent_size` is 0 replies `ReplyCodeInvalidConf` with the **stored**
    revision and the exact string
    `"invalid stored conf: dn_bin_conf.extent_size is zero"`, and records
    **no** `writeblock`, no `dmsetup` or configfs call and no `WriteProto`
    to `LocalDnPath` — the disk is never read for identity against a
    guessed extent size, and the zero never becomes desired state. A
    `Reconcile` over a `dn-*` file carrying that zero loads it, records the
    refusal once, mutates nothing and leaves that DN's side in place, state
    file included (DN2). The string is asserted verbatim here and, for
    `model.ValidateClusterConf`, in `model/capacity_test.go`; the two
    assertions together are what keep the two copies of the rule in step
    (§2.1).
24. **Store commands are bounded** (SH6, SH7, SH15; `agent`'s
    `TestStoreCommandsCarryTheSoftTimeout`): one `List` and one `Remove`
    issue exactly one `ls` and one `rm`, each on a ctx whose deadline is at
    most `common.CmdSoftTimeout` away. The store calls the `OsClient`
    directly, not through an OS wrapper, so the bound is its own to apply
    (`osclient.md` §4.2).
25. **An unreadable store file deletes nothing it might own** (DN2): a
    `Reconcile` over a `dn-*` file that does not decode
    (`TestReconcileKeepsTheSidesOfAnUnreadableDnFile`) mutates nothing: the
    side's state file and its bitmap chunk stay byte for byte, and the DN
    and the side reply `ReplyCodeUnknownObject` to their Check rounds — the
    side still after the re-sent `SyncupDn`, whose save makes the file
    decode again. The skip covers only a DN not loaded, and only while a
    `dn-*` file did not load (`TestReconcileSkipsOnlyTheSidesOfAnUnloadedDn`):
    an undecodable `.tmp-*` file beside a readable `dn-*` file skips none of
    that DN's sides — the side is loaded and its first `CheckSide` round is
    clean — and with no `dn-*` file at all the side's state file and its
    chunk go in one `rm`. One level down
    (`TestReconcileKeepsTheChunksOfAnUnreadableSideFile`), beside a `side-*`
    file that does not decode, the chunk of a side its DN still names is
    kept byte for byte with no `rm` naming it, and the side replies
    `ReplyCodeUnknownObject` to its Check round; the chunk is deleted, in
    exactly one `rm`, when its DN no longer names its side, when its
    side has no state file and no `side-*` file failed to load, and when
    its DN has no file and no `dn-*` file failed to load; and an
    undecodable `.tmp-*` file beside a loaded side's `side-*` file leaves
    that side's chunk loaded, in its applied set, with no `rm` naming it.

## 7. Acceptance checklist

1. `go build ./...`, `go vet ./...`, `go test ./...` pass.
2. `go list -deps ./cmd/dnv-agent | grep etcd` finds nothing (`layout.md`
   §3).
3. The §2.2 additions exist: `NvmetPortId`, `AnaGrpId*`, `ReplyCode*`,
   `DnMigrConnectRetryInterval`, `DnZeroBatchExtCnt`, `DnZeroRetryInterval`,
   `DnZeroConcurrency`, `DnZeroKillBackoff` in `common/constants.go`;
   `DnNsIdentity` in `common/name_fmt.go`. `common/`
   contains the six files of `layout.md` §2 plus `name_parse.go` (§2.2), and
   nothing else.
4. `WriteFileDirect` is implemented per the amended `osclient.md`; a
   repo-wide grep finds no `WriteFile(` call whose path argument is under
   `/sys/kernel/config`, and no `ana_state` write outside `EnsurePort`.
5. `cmd/dnv-agent` wires **server** interceptors only (`grpc.md` §4 table);
   `grep -F "per exported namespace" doc/architecture.md` finds nothing
   (the [D4] amendment is applied).
6. `grep -rnE "pvcreate|vgcreate|lvcreate|lvchange|lvremove|\\blvs\\b|\\bvgs\\b|\\bpvs\\b" agent/ cmd/ common/` finds nothing outside comments and test-guard string literals — **repo-wide**: no dnv agent runs any LVM command ([D13], [D14]).
7. A manual run of the `architecture.md` §13 example starts `dnv-agent dn`,
   serves
   `GetDnSize`, and a `SyncupDn`/`SyncupSide` round-trip shows one trace id
   across `grpc server request`, `os command` and `os write file direct`
   records, and each `CheckSide` round's `os …` records carry the
   `trace_id` in its request (the `data` of its `grpc server recv` record,
   whose own `trace_id` is the one the stream was opened under — SH24,
   `grpc.md` T3).
8. `grep -rn "trimmed" pb/schema.proto agent/` finds only the `reserved 3;`
   comment in `DnDiskTable.SideRecord`: the trim flag is gone and DN9's
   zeroing protocol replaced it ([D15]).
9. `grep -rn "zeroout" agent/` hits only the DN9 zeroing path — never the
   DN13 clone-metadata slot preparation, which stays a plain `WriteBlock` of
   zeros, and never the CN clone-metadata arena, whose recycle guard is a
   plain `blkdiscard` hole punch (CN18).
10. Both `agent.CloneTable` call sites pass `noDiscardPassdown = true`, and a
    test in `agent/dnagent` asserts the dn table's
    `2 no_hydration no_discard_passdown`.
11. `agent.Serve` takes a `waitBackground func()` and calls it after
    `GracefulStop` (SH27); the dn passes `srv.WaitBackground` and the cn
    `nil`; `agent`'s `TestServeJoinsBackgroundOnEveryReturnPath` shows
    `Serve` cancelling and then joining a hand-rolled background goroutine
    on its listener-failure and reconcile-failure return paths — no Go test
    drives a zeroing goroutine or a `blkdiscard` child through `Serve`
    (§6 test 19).
12. None of the probes below reads "did not answer" as "absent" (SH15);
    the cn leg walk still does, which is open: `readSubsys`
    (`agent/cnagent/leg.go`) reads a failed `ls` of
    `/sys/class/nvme-subsystem` as "no subsystem" and passes over a
    subsystem whose `subsysnqn` read failed. `Dm.Info`,
    `osBase.listDir`, `osBase.dirMtime`, `Md.HasSuperblock`,
    `Md.NameInUse` (the `lsblk` of CN12's case-1 guard, since 2026-09-26)
    and `CloneMeta.Mounted` / `CloneMeta.FileSize` (`cnagent.md` CN5's
    `findmnt` and `stat`, since 2026-09-29) take their answer from
    `osBase.runProbe` (exposed to the role packages as `Cmd.RunProbe`), the
    one probe wrapper that applies `agent.Reported` (its only other caller,
    `Dm.BlkZeroout`, is not a probe: it hands the verdict to DN9's zeroing
    loop as `answered`); `NvmeHost.readTrimmed`, `Nvmet.NsDevicePath`, the
    `enable` reads of `Nvmet.RemoveNamespace` and `RemoveSubsystem` and,
    through `Cmd.ReadAttr`, `Md.ListArrays` and `Md.Gone` read through
    `readAttrStrict`, which calls absence only on `fs.ErrNotExist`;
    `CloneMeta.LoopDevices` takes every failure of `losetup --associated`
    as an error; and `Md.Detail` with the `Md.Walk` it reads from, the
    sysfs md read since 2026-09-26 (`cnagent.md` CN12), goes through both —
    `listDir` for the listings, `readAttrStrict` for the attributes. None of
    them turns a non-nil error into a nil-and-not-found.

### Integration-run fixes (first on-hardware run of the amended tree)

Found by running `integtest/dnagent_test.sh` and `integtest/cnagent_test.sh`
against two real VMs (kernel 7.0, nvme-cli 2.16, mdadm 4.5) — the first
execution of either suite since the first amendment pass was applied. All five were
real agent defects, not harness problems; every one is now covered by a unit
test that fails without the fix.

* **IR1/IR2 (SH20)** — `Connect` passes `--fast_io_fail_tmo` (nvme-cli's own
  underscore spelling) and an explicit `--hostid common.NvmeHostId(hostnqn)`.
* **IR3 (SH17, SH20)** — `ListSubsys` reads `/sys/class/nvme-subsystem` and
  `/sys/class/nvme` instead of `nvme list-subsys -o json`, which carries neither
  the namespace device nor `ANAState`. Without it a migration destination
  reported `"controller has no namespace"` forever.
* **IR4 (DN6)** — the dm-clone is retired **after** the per-CN dm-linears,
  not before them. DN6's prose listed the dm-clone ahead of the per-CN
  `DnLinearName` (the code always had it right for a full side teardown; the
  per-CN `DnErrorName` goes after the clone, because nothing maps onto a
  dm-error once its linear is gone); the §11.2 *finish* path really did remove
  it first, hit EBUSY and leaked the clone, its metadata wrapper and the side
  device under them — unrecoverable by any later empty side list. DN6 states
  the layering rule and the finish carve-out, and the removal is idempotent:
  a pass that cannot remove the clone descends no further, so it never
  disconnects a live clone's source, and the next pass derives the same work
  from the node again. The "keeps the applied `migr_dst_conf` so the next
  converge retries" this rule once relied on is gone with the field: what
  makes the retry happen is that nothing was remembered in the first place
  (DN6).
* **IR5 (SH17)** — `device_uuid`/`device_nguid` are compared through the shared
  `agent.SameNsId` (strip `-`, fold case), never byte-wise. SH17's "tolerate
  normalized read-back" rule now names both cases it covers.
