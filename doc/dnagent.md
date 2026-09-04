# dnagent.md — `dnv-agent` and the dn role (dnv)

Status: **normative**. Read `log.md`, `osclient.md` and `grpc.md` first — this
document builds on their rules (one Info record per operation, `OsClient`-only
OS access, the four interceptors, trace-id propagation) and does not restate
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
unchanged; a future `cnagent.md` MUST NOT re-specify them. §4 is **[dn]** —
the `agent/dnagent` policy package. §5 records the amendments this document
made to the companion documents.

---

## 1. Scope and placement

| package | path | role | may import (layout.md §3) |
|---|---|---|---|
| `agent` | `agent/` | shared dn/cn **mechanism**: bootstrap, local store, revision gate, locks, `ResInfo` tracking, OS wrappers, bitmap store, check-loop rules | `common`, `pb` |
| `dnagent` | `agent/dnagent/` | dn **policy**: the `DiskNodeAgent` service — which extent allocations, dm tables and nvmet objects to build and when | `common`, `pb`, `agent` |
| `main` | `cmd/dnv-agent/` | cobra `dn`/`cn` dispatch, viper flags, dependency construction | `agent`, `agent/dnagent`, `agent/cnagent`, `common` |

Agents never talk to etcd (`layout.md` §3); acceptance re-checks it. The
gRPC server carries the **server** interceptors of `grpc.md` §4 and no client
interceptors — the agent's only outbound connections are `nvme connect`, not
gRPC. The split rule for new code: anything both roles need verbatim is
mechanism and belongs in `agent`; anything that knows *which* resource to
build is policy and belongs in `dnagent`/`cnagent`.

## 2. Shared mechanism — package `agent` [shared]

### 2.1 Files

`agent.go` (bootstrap, §2.3), `store.go` (§2.4), `revision.go` (§2.5),
`locks.go` (§2.6), `resinfo.go` (§2.7), `dm.go`/`nvmet.go`/`nvmehost.go`
(OS wrappers, §2.8), `bitmap.go` (§2.9), plus colocated `_test.go` files.
There is no `lvm.go`: **no** dnv agent runs any LVM command at all — [D13]
took LVM off the dn, [D14] took it off the cn too (`update_01.md` U3). This is the `layout.md` §2 recommended split; package
boundaries are binding, file names are not.

### 2.2 Additions to `common`

The following enter the existing files `common/constants.go` and
`common/name_fmt.go` (no new files — `layout.md` §7.5 still holds):

```go
	// The single nvmet port every node exports (architecture.md §3.1/§3.2).
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

	// Seconds between background retries of a pending migration-destination
	// nvme connect (dnagent.md DN8).
	DnMigrConnectRetryInterval = 5

	// Side provisioning ([D15], architecture.md §9.4, dnagent.md DN9): the
	// background zeroing goroutine zeroes DnZeroBatchExtCnt logical extents
	// per `blkdiscard --zeroout` command, through the side's dm-linear, and
	// persists that batch's `zeroed_bits` after each success. The batch size
	// assumes fast hardware Write Zeroes: batch × ext_size must stay inside
	// CmdSoftTimeout. A failed or timed-out batch is retried no sooner than
	// DnZeroRetryInterval seconds later — the zeroing twin of
	// DnMigrConnectRetryInterval, never a hot loop.
	DnZeroBatchExtCnt   = 10
	DnZeroRetryInterval = 5

	// The §11.2 src-cutover grace window: a migration source's per-CN
	// dm-linears stay suspended at least this long before they are reloaded
	// onto their dm-errors (DN12, [D12]).
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
     fresh node has an empty local store, so its reconcile is instant.

SH2. The startup reconcile runs under a ctx carrying a freshly minted trace id
     (`common.NewTraceId`), so every startup `os command` record is
     correlatable.

SH3. Reconcile returns an error only for **fatal** conditions (the local-store
     prefix unreadable); per-resource failures are captured as
     `RES_STATUS_ERROR` (§2.7) and never abort startup.

SH27. **Background tasks and process exit** (added by `update_01.md` U4;
      numbered last because SH rules are append-only — SH1-SH26 are cited
      from code comments and must not shift). A role server MAY run
      goroutines outside any RPC: the DN8 migration-connect retry, the DN12
      fence timer, the DN9 side-zeroing workers, the cn's connect retry and
      the CN11 leg probers. Every one of them derives its ctx from the
      server's **`rootCtx`** — the process-lifetime ctx captured at
      `Reconcile`, which `Serve` derives from its own ctx and cancels before
      returning — and mints a fresh trace id per attempt
      (`common.NewTraceId`), taking the SH11 locks for the attempt only,
      never across the whole task.

      A background task may additionally run a **child process**, and DN9's
      `blkdiscard --zeroout` batches are the first that does. Such a child
      must never outlive the agent, so the server registers every goroutine
      that owns one in a `sync.WaitGroup` before it starts and exposes a
      `WaitBackground()` that waits for them; `Serve` takes it as its
      `waitBackground` parameter, cancels the task ctx after `GracefulStop`
      and then waits. Cancellation kills the in-flight child through the SH15
      soft/hard timeout machinery (`osclient.md` §4.2 sends SIGTERM then
      SIGKILL), so the wait is bounded by `CmdHardTimeout` per in-flight
      command.

      **Only child-owning tasks are waited for.** The cn passes
      `waitBackground = nil`: a CN11 prober's IO is a direct, uninterruptible
      syscall (`cnagent.md` §2.2), so joining it could hang shutdown forever
      — cancelling is the whole contract there. Object-scoped tasks that hold
      a device open are additionally cancelled **and waited for** at teardown,
      before the resources they hold are removed (DN6, DN9): a live child
      keeps an fd on the dm device and `dmsetup remove` would fail EBUSY.

Reference implementation — `agent/agent.go` (complete; the `waitBackground`
parameter and the derived task ctx are SH27's):

```go
// Package agent holds the mechanism shared by the dn and cn agent roles
// (dnagent.md §2): bootstrap, the local store, the revision gate, the lock
// hierarchy, ResInfo tracking, the OS wrappers and the bitmap-chunk store.
// Policy — which dm tables, md arrays and nvmet objects to build and when —
// lives in the role packages agent/dnagent and agent/cnagent. No LVs: [D14]
// removed the clone VG, LVM's last user, so no LVM runs anywhere in dnv
// (update_01.md U3, cnagent.md §1).
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
// RPC, so no background goroutine — and, more to the point, no child process
// one of them owns, such as the §9.4 zeroing `blkdiscard` — outlives the
// agent (update_01.md U4). Only a role whose background work holds a
// long-running child passes one: the dn passes its WaitGroup join, the cn
// passes nil because its CN11 probers are stopped by cancellation and never
// joined (a wedged pread is uninterruptible, so waiting would hang shutdown
// forever — the very starvation U2 exists to prevent).
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
	// waits for them, so no orphan child process outlives the agent (§9.4).
	return grpcServer.Serve(lis)
}
```

### 2.4 Local store — `store.go`

SH4. The store holds exactly what `architecture.md` §9.1/§4.6 prescribes: the
     last fully applied `Syncup*Request` per object and one file per received
     `Push*BitmapRequest` chunk, written via `OsClient.WriteProto` (atomic
     replace) at the `Local*Path` locations, read back via `ReadProto`.

SH5. A `Syncup*Request` is persisted **after** its converge pass completes.
     Per-resource `RES_STATUS_ERROR` outcomes do not block persistence — the
     errors travel in the `*Info` and the worker's health loop drives repair
     (equal-revision re-syncs re-apply idempotently). Protocol rejections
     (stale revision, unknown pointer) never reach persistence.

SH6. Enumeration on startup: `RunCommand(ctx, "ls", []string{"-1", prefix},
     "")`, filtering by the role's `Local*Path` kind prefixes (dn: `dn-`,
     `side-`, `migr-bm-`; cn: `cn-`, `cntlr-`, `clone-bm-`). File names are
     used only for discovery; the ids come from the decoded protos.

SH7. When an object is torn down (removed from its parent's pointer list),
     its request file and its bitmap-chunk files are deleted with
     `RunCommand("rm", ["-f", …])` after the resources are gone — a crash in
     between re-runs the teardown on restart (idempotent).

### 2.5 Revision gate — `revision.go`

SH8. Per `architecture.md` §9.1, with `stored` = revision of the last fully
     applied request for the object (0 when none):

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
     string naming the missing pointer/id. Both rejection kinds return a
     normal gRPC reply (`AgentReply.code != 0`), never a gRPC error status —
     only `GetDnSize`/`GetCnSize` and `Get*Bm` report failure through the
     status (§9.1).

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
      (`SyncupDn`/`SyncupCn` — they diff the pointer list and may tear
      objects down).

SH11. Node **read** lock + the object's lock: every object-scoped RPC
      (`SyncupSide`/`SyncupCntlr`, `Push*Bitmap`, `GetSideInfo`/
      `GetCntlrInfo`, one `CheckSide`/`CheckCntlr` round) and every
      background converge attempt (DN8).

SH12. Node **read** lock only: node-scoped reads (`GetDnInfo`/`GetCnInfo`,
      one `CheckDn`/`CheckCn` round). `GetDnSize`/`GetCnSize` take no lock.

SH13. Lock order is node → object, never nested object locks. Probing under a
      lock is acceptable: every OS call is bounded by `CmdSoftTimeout`/
      `CmdHardTimeout` (§2.8 SH15).

### 2.7 `ResInfo` tracking — `resinfo.go`

SH14. A per-object in-memory tracker turns probe outcomes into
      `pb.ResInfo{res_name, status, details, epoch}` per `architecture.md`
      §9.5: `epoch` = unix seconds of the last **status** change (a `details`
      change alone does not bump it); the agent emits
      `MISSING`/`ERROR`/`OK`/`PROVISIONING` and never `UNKNOWN`
      (worker-only). `RES_STATUS_PROVISIONING` (`update_01.md` U4) means
      *deliberately not created yet, healthy, no action needed*: it is what a
      resource waiting behind DN9's provisioning gate reports, and unlike
      `RES_STATUS_ERROR` it never feeds `err_epoch` (§9.5, §10.2-§10.4).
      Tracker state is in-memory; after a
      restart epochs restart at the reconcile time — acceptable, the epoch
      means "last observed change".

### 2.8 OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`

Thin, mechanism-only wrappers over the process's single
`common.NewLimitedOsClient(...)` instance. They implement exactly the
Appendix A command patterns; policy (which device, which table) stays in the
role packages.

SH15. Every wrapper call wraps its ctx with
      `context.WithTimeout(ctx, common.CmdSoftTimeout*time.Second)` before
      calling `OsClient` (the §7 soft/hard timeout contract; `osclient.md`
      §4.2 handles SIGTERM/SIGKILL). This covers the raw-device
      `ReadBlock`/`WriteBlock` calls of the [D13] metadata path too. A role
      package that calls the `OsClient` directly instead of through a
      wrapper — the `cnagent.md` CN12 sysfs leg walk — takes the same bound
      from the exported `agent.CmdCtx` (`update_02.md` U2).

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
      report is probed anywhere any more ([D14], `update_01.md` U3). Probe
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
      * `EnsurePort` creates `ports/{NvmetPortId}` with the node's
        `NvmeTrConf` attributes **and** the three fixed groups: `mkdir
        ana_groups/2`, `mkdir ana_groups/3`, then write `optimized` /
        `non-optimized` / `inaccessible` into groups 1/2/3 exactly once
        (probe-first: skip when already correct). No code path ever writes an
        `ana_state` after that.
      * every ANA transition is `SetNsAnaGrpId(nqn, nsid, grpid)` — a single
        `WriteFileDirect` of the namespace's `ana_grpid`, valid on a live
        namespace because the target group always exists.
      * subsystem/namespace lifecycle helpers follow the Appendix A order;
        teardown is reverse order (port link removed first, then ns disable,
        rmdir ns, allowed-hosts unlink, rmdir subsystem).

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
      OS touch (`update_02.md` U2): unlike most of sysfs, `/sys/class/nvme*`
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
      never by `list-subsys` (`update_01.md` U5): the
      `/sys/class/nvme-subsystem/nvme-subsys*` directory whose `subsysnqn`
      equals the NQN holds the `nvme{N}` controller entries, and one is
      selected by reading `/sys/class/nvme/{ctrl}/address` and parsing it as
      comma-separated `key=value` pairs (`traddr=…,trsvcid=…`) to match the
      dead side — never by field position.

### 2.9 Bitmap-chunk store — `bitmap.go`

SH21. Implements the agent side of §9.6: persist the received
      `Push*BitmapRequest` verbatim at its `Local*BmPath` (via `WriteProto`)
      **before** applying; the applied set reported in
      `BitmapInfo.bm_idx_list` is always derived from the files present.

SH22. The §11.4 math skeleton lives here: chunk reassembly (concatenated —
      migration — or self-positioned — clone), the single
      wire-convention inversion (**wire 1 = unwritten/skippable**; invert
      exactly once at this boundary), and the fully-skippable-region →
      `blkdiscard` range computation. Role packages supply only the
      positioning parameters (the dn shifts by the leg's `meta_blocks` first,
      §9.6).

SH23. Migration chunks are interpretable only as a contiguous prefix from
      `bm_idx = 0`; the apply computation uses the longest contiguous prefix
      of the files present (the worker's ascending, one-in-flight push makes
      gaps unreachable in practice). The applied set still reports every file
      present.

### 2.10 Check-stream rules

The `Check*` handlers are role code (the stream types differ), but MUST all
follow these rules (`architecture.md` §9.7):

SH24. Rounds are worker-initiated: loop on `Recv` (a `Recv` returning
      `io.EOF`, or the stream ctx ending, ends the handler with `nil`);
      exactly one `Send` per received request; never an unsolicited send.

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
     subcommands, `dn` and `cn`. cobra and viper live only in `cmd/`
     (`layout.md` §3); `github.com/spf13/cobra` and `github.com/spf13/viper`
     enter `go.mod` with this binary.

CM2. Flags (`architecture.md` §13; every flag is also settable via config
     file and environment through viper):

| flag | dn | cn | default | meaning |
|---|---|---|---|---|
| `--grpc-network` | ✓ | ✓ | `tcp` | `net.Listen` network |
| `--grpc-address` | required | required | — | gRPC endpoint; the CP stores it as `DnConf`/`CnConf` `addr_port` |
| `--tr-type` / `--adr-fam` / `--tr-addr` / `--tr-svc-id` | required | required | — | the node's single nvmet port (`NvmeTrConf`), mirrored into `DnConf`/`CnConf` at creation |
| `--local-store` | ✓ | ✓ | `DefaultLocalStorPrefix` | `localStorPrefix` of `common.NewNameFmt` (§4.6 state files) |
| `--disk` | required | — | — | the raw block device that becomes the DN VG |
| `--capacity` | — | ✓ | 0 | capacity budget in bytes this CN is willing to host; `GetCnSize` replies it verbatim, 0 = "use the CP default" (added by `cnagent.md` §3) |
| `--config` | ✓ | ✓ | — | optional viper config file |

CM3. Viper binding per subcommand: `viper.BindPFlags(cmd.Flags())`,
     `viper.SetEnvPrefix("DNV_AGENT")`,
     `viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))`,
     `viper.AutomaticEnv()`; when `--config` is set, `viper.SetConfigFile` +
     `ReadInConfig`. All value reads go through viper (so file/env win per
     viper precedence).

CM4. Each subcommand's `RunE`: `signal.NotifyContext(context.Background(),
     syscall.SIGINT, syscall.SIGTERM)`; construct
     `common.NewNameFmt(localStore)` and the process's **single**
     `common.NewLimitedOsClient(0)`; build the role server
     (`dnagent.NewDnAgentServer(...)` / `cnagent...`); call `agent.Serve`
     with the role's reconcile, a register func that calls
     `pb.RegisterDiskNodeAgentServer` (resp. `...ControllerNode...`), and the
     role's SH27 background waiter — the dn passes `srv.WaitBackground`, the
     cn passes `nil` (its probers are cancelled, never joined).
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
	cmd.Flags().String("disk", "", "raw block device for the DN VG")
	cmd.MarkFlagRequired("disk")
	return cmd
}

func runDn(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	bindViper(cmd)                    // CM3
	nf := common.NewNameFmt(viper.GetString("local-store"))
	oc := common.NewLimitedOsClient(0)
	srv := dnagent.NewDnAgentServer(oc, nf, viper.GetString("disk"),
		trConfFromViper())
	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Reconcile,
		func(g *grpc.Server) { pb.RegisterDiskNodeAgentServer(g, srv) },
		srv.WaitBackground)          // SH27; the cn passes nil
}
```

## 4. The dn role — package `dnagent` [dn]

### 4.1 Files

`server.go` (the `DnAgentServer` type, lock mapping, RPC entry points),
`diskmeta.go` (the [D13] on-disk format: header, A/B volume-table slots,
extent and clone-metadata allocators), `syncup_dn.go`, `syncup_side.go`,
`migr.go` (the §11.2 source/destination choreography), `zeroing.go` (the DN9
side-provisioning registry and its `blkdiscard --zeroout` batches),
`push_migr_bm.go`,
`check.go`, `probe.go` (DnInfo/SideInfo probing). Colocated `_test.go` files.

### 4.2 Server type and lock mapping

```go
type DnAgentServer struct {
	pb.UnimplementedDiskNodeAgentServer
	oc     common.OsClient
	nf     *common.NameFmt
	disk   string          // --disk
	trType, adrFam, trAddr, trSvcId string // --tr-* (the port)
	locks  *agent.LockSet  // object key = LocalSidePath id tuple
	// per-object resinfo trackers, pending-connect retry registry,
	// per-side zeroing registry (DN9), and the SH27 background-task
	// bookkeeping: the rootCtx captured at Reconcile plus a sync.WaitGroup
	// that WaitBackground() waits on, …
}
```

DN1. Lock mapping (instantiates SH10-SH13): `SyncupDn` and the startup
     reconcile — node write; `SyncupSide`, `PushMigrBitmap`, `GetSideInfo`,
     one `CheckSide` round, one DN8 retry attempt — node read + that side's
     object lock (key = `(cluster_id, dn_id, sp_id, side_id)`); `GetDnInfo`
     and one `CheckDn` round — node read; `GetDnSize` — no lock.

### 4.3 Startup reconcile

DN2. Enumerate the store (SH6). For each `dn-*` file: re-run the SyncupDn
     converge (§4.5 steps 2-3) from the stored request. Then each `side-*`
     file: if its pointer is absent from the stored
     `SyncupDnRequest.side_pointer_list`, tear the side down (DN6) — it was
     removed mid-teardown; otherwise re-run the SyncupSide converge (§4.6)
     from the stored request — a converge that finds not-yet-zeroed extents
     (re)starts that side's DN9 zeroing goroutine, which is how provisioning
     resumes after a restart. Then re-apply every `migr-bm-*` chunk (SH21-
     SH23). All under the node write lock, with the SH2 trace id.

### 4.4 `GetDnSize`

DN3. `lsblk --bytes --nodeps --noheadings --output SIZE {--disk}`; reply the
     parsed size **minus `DnDataOffset`** — the byte size of the [D13] data
     area, which is what the CP divides by `extent_size` (§6.1). A size at or
     below `DnDataOffset` is the error `"disk too small: %d <= %d"`. Failures
     (bad disk, parse, too small) return a gRPC `Internal` status (§9.1 —
     this RPC has no `AgentReply`). `dn_id` may be 0 (pre-registration call
     from the gateway); it is for logging only. No locks, no store access.

### 4.5 `SyncupDn`

DN4. Gate the revision (SH8) against the stored `SyncupDnRequest`.

DN5. Converge the once-per-DN base state of `architecture.md` §3.1,
     probe-first (SH16), building `DnInfo` as it goes:
     * `lsblk --bytes` the `--disk` device — both to report `disk_info` and
       to hand the allocator the raw size, the one input the disk format
       itself does not carry (the extent count is that size minus
       `DnDataOffset`, divided by the **header's** `extent_size`).
     * **the [D13] disk format.** Read the header block. Magic absent ⇒ the
       disk is blank: write the header (a fresh `format_uuid` from
       `crypto/rand`, the request's `cluster_id`/`dn_id`/`extent_size`, and
       the three layout offsets) and then slot A with an empty table at
       `seq` 1. Magic present but version or CRC wrong ⇒ **error**, and every
       later operation errors too — a corrupt header is never formatted over.
       Magic present and valid ⇒ verify `cluster_id`/`dn_id`/`extent_size`
       match; a mismatch is the error
       `"foreign disk: cluster/dn/extent is …, want …"` and the disk is
       **never** overwritten (parity with `pvcreate` refusing a foreign PV).
       `extent_size` is therefore immutable for the life of a format. A
       re-call on an already-converged disk issues **zero** writes (SH16).

       A successful identity check is what makes the disk **writable** at all:
       every later mutation (allocate, free, the DN9 zeroed-bit updates, the
       DN6 orphan sweep) refuses on a disk whose identity this node has not
       confirmed.
       The guard has to live at that layer rather than in the caller, because
       a failed DN converge does not stop the side converges that follow
       (DN19) — without it, a node pointed at another node's disk would
       report `meta_info = RES_STATUS_ERROR` and then allocate extents in
       that disk's volume table anyway.
     * `EnsurePort` (SH19: port `NvmetPortId` from the `--tr-*` flags + the
       three fixed ANA groups).
     * **the Write Zeroes fail-fast** (`update_01.md` U4). DN9 zeroes whole
       sides with `blkdiscard --zeroout` under the ordinary SH15 timeouts,
       which only holds on hardware whose Write Zeroes is offloaded; a
       kernel that has to emulate it writes zero pages at bulk speed and no
       batch bound can survive. Resolve the disk's kernel name with
       `lsblk --nodeps --noheadings --output KNAME {--disk}` (the flag is
       documented as a `/dev/disk/by-uuid` symlink, whose basename is not a
       sysfs node) and read
       `/sys/class/block/{kname}/queue/write_zeroes_max_bytes`. A **present
       `0`** is the verdict: `meta_info = RES_STATUS_ERROR` with
       `details = "disk lacks Write Zeroes"`, which flows into the worker's
       `err_epoch` → capacity-key removal (§9.5, §10.2) and takes the
       unsuitable DN out of allocation. An absent or unreadable attribute is
       **not** a verdict — it is logged and the converge continues, because
       failing every kernel that simply does not publish the attribute would
       remove healthy DNs for a reason nothing measured. Unlike the identity
       check this is a **health** signal, never a write gate (DN19): a DN
       already carrying sides must keep serving them.

DN6. Diff `side_pointer_list` against the local `side-*` files (§9.1 full
     sync). A pointer in the request without local state needs nothing yet —
     resources come with its first `SyncupSide`; the persisted request is
     what makes the pointer *known*. A local side file whose pointer left the
     list is torn down **top-down**, and two things happen *before* its first
     step: its DN9 zeroing goroutine is cancelled **and waited for** (a
     running `blkdiscard --zeroout` child holds `DnSideName` open, so
     `dmsetup remove` would fail EBUSY), and every fenced per-CN dm-linear is
     resumed (DN12). Then: nvmet port-link/ns/subsystem(s)
     (including a migration-source export), the per-CN
     `DnLinearName`/`DnErrorName` pair and `DnMigrSrcName`, the dm-clone
     `DnMigrFinalName`, then `nvme disconnect` of a migration-destination
     connection and its retry loop, the `DnMigrMetaDmName` wrapper and the
     release of its metadata slot, the `DnSideName` device and the release of
     its extent record; then delete its `side-*` and `migr-bm-*` files and
     `DropObj` its lock (SH7).

     The per-CN dm-linears go **before** the dm-clone, and the dm-clone
     before the wrapper and the side device, because each of those is a
     table target of the one above it and `dmsetup remove` on a device
     another live dm device still maps fails EBUSY. Getting this backwards
     does not merely log an error: the clone survives, so its wrapper and
     the side device under it survive too, and no later empty side list can
     remove them either — the side leaks until the node is scrubbed by hand.

     **Orphan sweep.** Immediately after the pointer diff (and at the end of
     the startup reconcile), under the node write lock, a volume-table record
     whose owner is **provably** gone has its dm device removed and its space
     freed. This closes the crash window between a teardown's resource
     removal and its table update.

     "Provably" is load-bearing, because the volume table — not the local
     store — is authoritative for extent placement ([D13]): a `SideRecord` is
     an orphan only when its `(sp_id, side_id)` appears in **no** synced DN's
     authoritative `side_pointer_list` and in no locally stored side. Missing
     local state is *not* proof: a node that lost `--local-store` but kept
     its disk still has every side in its DN's pointer list and must rebuild
     those sides from their records — sweeping them would free the extents,
     and the next `SyncupSide` then either re-allocates them and zeroes live
     data away (`provisioned = false`) or, at `provisioned = true`, refuses to
     allocate and reports the side permanently dead (`"record missing"`,
     DN9). Both outcomes lose the data. When no DN has been synced or
     reloaded at all, nothing is authoritative and the sweep does nothing. A
     side the sweep does remove has its zeroing goroutine cancelled and waited
     for first, exactly as in the teardown above.

     A `CloneMetaRecord` is an orphan only when no live destination role
     claims its `(sp_id, migr_id)` — both the currently requested and the
     last applied `migr_dst_conf` count, so a converge that has not run yet
     never loses its slot — **and** every side of that `sp_id` the node may
     host is one whose local state the agent actually holds. Otherwise a side
     it has not heard from yet could still own the slot, and freeing it would
     strand an in-flight migration whose hydration is supposed to resume from
     disk (§11.2). The dm-clone goes **before** the disconnect
     that removes its source device: pulling the source out from under a
     live dm-clone leaves in-flight hydration IO with nowhere to go, and the
     `dmsetup remove` that follows then blocks until the §7 hard timeout.
     (The `sp_level`/end-of-migration teardown of a destination role follows
     the same order — DN11, DN13.) Ids are never reused, so a deleted side
     never comes back.

     **Finishing a migration is not a top-down teardown.** When a
     `SyncupSide` drops `migr_dst_conf` while the side keeps exporting
     (§11.2 dst finish), the per-CN dm-linears are not removed — they are
     *reloaded* off the dm-clone onto the plain `DnSideName`. The
     destination role is therefore retired **after** the per-CN dm layer has
     converged, never in the up-front teardown pass that precedes it: at
     that point every linear still maps the clone. The retirement is
     idempotent and self-healing — a pass that cannot remove the clone
     leaves the applied `migr_dst_conf` in place, so the next converge
     retries the whole sequence rather than continuing past a live clone to
     disconnect its source.

DN7. Persist the request (SH5); reply `agent_reply`, `revision` (= the stored
     revision after this call), `dn_info`.

### 4.6 `SyncupSide`

DN8-DN14 below; the converge order is build bottom-up (side device → dm →
nvmet), tear down top-down, probe-first throughout (SH16).

DN8. **Gating.** The pointer MUST be present in the stored
     `SyncupDnRequest.side_pointer_list` — else `ReplyCodeUnknownObject`
     (`SyncupDn` introduces pointers first, §9.2). Then the SH8 revision gate
     against the stored `SyncupSideRequest`.

DN9. **Side device and the §9.4 side provisioning protocol.** Look up
     `(sp_id, side_id)` in the volume table. An existing record whose extent
     total disagrees with `side_conf.ext_cnt` is an error (resize is out of
     scope) — unchanged by `update_01.md` U4.

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
     gate**, which replaced the trim flag (`update_01.md` U4: `blkdiscard` is
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
        not-yet-zeroed extents in batches of `common.DnZeroBatchExtCnt` (10),
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
       in **parallel**; v1 has no global cap, which the fast Write Zeroes
       assumption (DN5) pays for.
     * each batch mints a fresh trace id (SH2's `common.NewTraceId`); the
       `blkdiscard` itself runs **lock-free** and through the ordinary
       `OsClient` under the standard SH15 timeouts — it is a *killable child
       process*, so a semaphore slot is held for at most `CmdHardTimeout` and
       no `LimitedOsClient` carve-out is needed (unlike the CN11 probe IO,
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
       harmless: the batch's bits stay unset and the batch is redone, so a
       restart simply resumes at the first unset bit.
     * zeroing runs at **every** `sp_level`, `SP_LEVEL_DISABLE` included
       (DN11): it is bottom-layer provisioning, exactly as the trim it
       replaced was.
     * cancellation: side teardown (DN6, which is also where a cancelled
       migration lands — `CancelMigration` reaches the agent as the side
       pointer leaving the list) **cancels the goroutine and waits for it**
       before the dm device is removed, because the running child holds
       `DnSideName` open and `dmsetup remove` would fail EBUSY. Process exit
       is SH27's `WaitBackground`, so no orphan `blkdiscard` ever outlives the
       agent.
     * `SideInfo.zeroed_ext_cnt` / `total_ext_cnt` are filled on every reply
       and every Check round (DN14, DN16, DN18). `total_ext_cnt` is never
       omitted: it comes from the record, or from `side_conf.ext_cnt` when
       there is no record yet. Equality is what the worker's flip rule watches
       — guarded by `> 0`, so "no record at all" (both zero) never reads as
       done.

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
      `DnErrorName` (dm-error sized like the LV),
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
      tables and rewrites the two `ana_grpid`s — nothing else.

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
      dm-linears and the side LV are therefore always writeable, whatever the
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
      (`update_01.md` U4). When the worker flips the destination side, the
      next fan-out carries `dst_provisioned = true` and the sequence above
      runs unchanged; the destination's connect retry (DN13) absorbs any
      cross-side ordering.

      **The step-2 fence ([D12]).** Phase 1: suspend each per-CN dm-linear
      **in place**, leaving its table alone, and record when. Phase 2, on the
      first converge at or after `common.SuspendSeconds` have passed: reload
      it onto its dm-error, which resumes it. Swapping the table in phase 1
      would error the very IO the window exists to absorb; resuming without
      the swap would replay it onto the side's data. The RPC never waits out
      the window — a one-shot timer arms the converge that ends it, the same
      way DN8 arms the connect retry — and every converge is idempotent, so
      an early one simply stays in phase 1.

      Three rules keep the suspension bounded, which is what makes it safe:
      * A side whose linears are suspended but whose window start is unknown
        — an agent restart mid-window — is treated as **elapsed**, and phase 2
        runs on the first converge. Restarting never opens a second window.
      * The role ending (`migr_src_conf` gone) clears the window and returns
        the linears to their normal targets, resumed.
      * `teardownSide` resumes every fenced linear **before** its first step,
        because disabling an nvmet namespace closes its backing device and
        `dmsetup remove` does not succeed on a suspended one.

      While the window is open the per-CN `dm_linear_info` is
      `RES_STATUS_OK` with `details = "suspended (migration cutover grace
      window)"`: it is an expected, time-bounded state, and the probe expects
      the **pre-fence** table there rather than the dm-error, so a healthy
      cutover never reports a table mismatch.

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
      on **every** dnv dm-clone, dn and cn alike (`update_01.md` U1,
      `cnagent.md` CN18 step 3), because §9.6/§11.4 use `blkdiscard` on a
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
      `AnaGrpIdNonOptimized`; re-apply all locally present bitmap chunks
      (SH21). "Retrying until success" (§11.2) is implemented without
      blocking the RPC: a converge pass attempts the connect **once**; on
      failure it records `target_info = RES_STATUS_ERROR` and registers the
      side in a background retry registry that re-runs the destination
      converge every `DnMigrConnectRetryInterval` seconds under the DN1
      locks, until success or teardown.

      **The clone-metadata area is per DN, and it is a real ceiling
      (update_02.md U4).** `DnCloneMetaSize` (192 MiB = 48 `DnCloneMetaUnit`
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

DN14. Persist (SH5); reply `agent_reply`, `revision`, `side_info` — including
      `zeroed_ext_cnt`/`total_ext_cnt`, filled on every reply (DN9) — and
      `bm_info` (the applied set, SH21).

### 4.7 `PushMigrBitmap`

DN15. Gate: the side file must exist and its
      `migr_dst_conf.migr_id` must equal the request's `migr_id`
      (`ReplyCodeUnknownObject` otherwise — only a destination side accepts
      chunks); revision gate (SH8) against the stored side revision (a push
      never updates the stored revision). Then SH21-SH23: persist the chunk
      at `LocalMigrBmPath(cluster, dn, sp, migr_id, bm_idx)`, recompute from
      all present chunks (shift by the leg's `meta_blocks`), `blkdiscard` the
      fully-skippable regions of `DnMigrFinalName`. If the dm-clone does not
      currently exist — not built yet, suppressed by `sp_level`, or still
      behind DN13's provisioning gate — the file still counts as applied;
      chunks are re-applied whenever the dm-clone is (re)created. Reply
      `agent_reply` only.

### 4.8 `GetDnInfo` / `GetSideInfo`

DN16. Read-only: probe fresh under the DN1 locks and reply `agent_reply`,
      `revision`, the info. An unknown DN (no `dn-*` file) or side pointer ⇒
      `ReplyCodeUnknownObject` with `revision = 0`. Never mutates — in
      particular a `Get*Info` or `Check*` round never allocates a record and
      never registers a DN9 zeroing goroutine (registration happens only on a
      converge path), and it reports `RES_STATUS_MISSING` for a side whose
      record the converge has not written yet.

### 4.9 `CheckDn` / `CheckSide`

DN17. Instantiate the SH24-SH26 loop with the §4.10 probes; one round takes
      the DN1 locks of the corresponding `Get*Info`.

### 4.10 Probing and error capture — `probe.go`

DN18. Probe map (all via SH17 conventions; `res_name` and probe per
      resource). `RES_STATUS_PROVISIONING` rows are **healthy**: the resource
      is deliberately not created yet, no action is needed, and the worker
      never turns one into an `err_epoch` (§9.5, `update_01.md` U4);
      `RES_STATUS_ERROR` keeps meaning *needs intervention*.

| `ResInfo` | `res_name` | probe |
|---|---|---|
| `DnInfo.disk_info` | the `--disk` path | `lsblk --bytes --nodeps` succeeds |
| `DnInfo.meta_info` | the `--disk` path | `ReadBlock` of the 4 KiB header: magic, version, CRC and `cluster_id`/`dn_id`/`extent_size` identity, **plus the DN5 Write-Zeroes check** (`/sys/class/block/{kname}/queue/write_zeroes_max_bytes` is absent, unreadable or ≠ 0). `details` = `"seq=%d sides=%d clone_metas=%d free_ext=%d free_meta_units=%d provisioning=%d"` when OK — the last count is sides whose `zeroed_bits` are still incomplete (DN9); on failure the error text instead, `"disk lacks Write Zeroes"` for the WZ case |
| `DnInfo.port_info` | `"{NvmetPortId}"` | configfs `addr_*` reads match the `--tr-*` flags; the three [D4] groups present with their fixed states |
| `SideInfo.side_dev_info` | `DnSideName` | the volume-table record + its `zeroed_bits` + `dmsetup table`, judged by the DN9 matrix: no record ⇒ `RES_STATUS_MISSING` at `provisioned = false` (the converge that allocates has not run) and `RES_STATUS_ERROR`, details `"record missing"`, at `provisioned = true`; bits incomplete ⇒ `RES_STATUS_PROVISIONING`, details `"zeroing {k}/{n}"`, at `false` and `RES_STATUS_ERROR`, details `"not zeroed"`, at `true`; an outstanding batch failure ⇒ `RES_STATUS_ERROR` with the killed command's output; a live table that does not match the record's extent runs ⇒ `RES_STATUS_ERROR`. The same read fills `SideInfo.zeroed_ext_cnt`/`total_ext_cnt` every round |
| `cn_id_to_dm_error[cn]` / `cn_id_to_dm_linear[cn]` | `DnErrorName` / `DnLinearName` | `dmsetup info` + `dmsetup table` (the linear's target — side device vs dm-error vs dm-clone — must match the desired role). Inside the §11.2 grace window the expected target is the **pre-fence** one and `details` is `"suspended (migration cutover grace window)"`; the probe never starts a window (DN16). While DN9's gate is closed no device is expected to exist and both report `RES_STATUS_PROVISIONING`, details `"side provisioning"` |
| `cn_id_to_nvmeof[cn]` | the `SideToCnNqn` | configfs: subsystem present, ns enabled, `ana_grpid` as desired. `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while DN9's gate is closed |
| `migr_src_info.dm_linear_info` / `.nvmeof_info` | `DnMigrSrcName` / the `MigrSrcNqn` | `dmsetup status` / configfs. With `migr_src_conf.dst_provisioned = false` neither object exists by design (DN12) and both report `RES_STATUS_PROVISIONING`, details `"side provisioning"` |
| `migr_dst_info.target_info` | the `MigrSrcNqn` | the SH20 **sysfs walk** (`/sys/class/nvme-subsystem` matched by `subsysnqn`, controller `state` under `/sys/class/nvme` — never `nvme list-subsys`, IR3) shows a live controller for it (liveness only, SH20); `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while the destination side is still zeroing (DN13) |
| `migr_dst_info.dm_clone_info` | `DnMigrFinalName` | `dmsetup status`; `details` carries the raw status line (§9.5 — hydration progress); `RES_STATUS_PROVISIONING`, details `"side provisioning"`, while the destination side is still zeroing (DN13). The `DnMigrMetaDmName` wrapper has no `ResInfo` of its own: like the metadata LV before it, its health folds into this one |

DN19. Error capture (§9.1): a failed command marks that resource
      `RES_STATUS_ERROR` with the command output in `details` and the
      converge pass **continues** with the remaining resources — the agent
      converges as much as it can; protocol-level failures are the only
      things reported through `agent_reply`.

## 5. Amendments applied to companion documents

Recorded for traceability; the edits are already applied.

* `architecture.md` — [D4] replaced: three **fixed** ANA groups per node
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
* `architecture.md` §9.4 + Appendix A and SH17/DN18 above — the LVM JSON
  report option is spelled `--reportformat json` (one word); the earlier
  `--report-format json` matches no LVM build, whose `getopt_long` table only
  carries `--reportformat`.
* `architecture.md` §11.1/§11.2 + `dnagent.md` DN12 and [D12]
  (`dnagent_plan_00.md` [P3]) — fencing is always a table reload onto a
  dm-error target, never a `dmsetup suspend` held across a wait. A suspended
  dm device queues IO forever (no timeout, no error path), which wedges any
  block-device scanner that touches it in unkillable D state, makes
  `dmsetup remove` fail, and defers writes that then replay at resume —
  possibly after hydration already copied that region
  (`dnagent_issue_00.md` issue 2). `Dm.Reload` lost its `keepSuspended`
  parameter, `sidePlan.linearSuspended` is gone, a migration source's per-CN
  dm-linears (the primary's included) now sit on their dm-error, and the
  §11.1 failover grace sleep and its constant are deleted.
* `architecture.md` §1/§2/§3.1/§4/§6.1/§8.11/§9.2/§9.4/§11.2/Appendix A/[D13],
  `dnagent.md` §2.1/§2.2/§2.8/§4.1/DN3/DN5/DN6/DN9/DN10/DN11/DN12/DN13/DN18,
  `layout.md` §2 (`dnagent_plan_00.md` [P4]-[P7]) — **LVM is gone from the dn
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
  `migr-pv` subtraction. Motivated by `dnagent_issue_00.md` issue 2's failure
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
  unchanged and is why the window is bounded, never restarted across an agent
  restart, and always undone before teardown. Requested by the project owner
  after `dnagent_plan_00.md` [P3] had removed the suspension entirely.
* `dnagent.md` DN9/DN11/DN18 + `architecture.md` §8.4/§11.7/[D11]
  (`dnagent_plan_00.md` [P1]/[P2]) — `SP_LEVEL_READONLY` now means exactly
  "every user-facing namespace is read-only: reads served, writes fail with
  an IO error", enforced **on the CN only** by a dm-flakey `error_writes`
  table over the namespace's normal backing. The previous LV-permission gate
  is deleted: `dnagent_issue_00.md` issue 1 measured that a read-only flag
  below the top of a stack does not stop dm-remapped writes, and the DN must
  keep serving md metadata/resync and §3.6 health-check writes anyway. The
  level therefore has **no** DN-side behavior, and clone/migration hydration
  is no longer paused at it — hydration is infrastructure IO, not user IO.
  `agent/lvm.go` lost `LvSetPermission`, `LvEntry.ReadOnly` and
  `lvAttrReadOnly`.
* `dnagent.md` DN13 step (4) + §6 test 12 (`update_01.md` U1) — the dn
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
* `dnagent.md` §2.1/SH17/§7 item 6 (`update_01.md` U3) — LVM left the **cn**
  too ([D14]: the clone VG became a slot allocator over one loop device with
  kind-`b` wrapper linears), so this document's dn-only statements are
  generalized: no dnv agent runs any LVM command, SH17 lists no LVM report
  option, and the acceptance grep is repo-wide instead of `agent/ cmd/`.
* `dnagent.md` SH20 + `cnagent.md` §2.3 (`update_01.md` U5) —
  `DisconnectDevice`'s controller device is located by the **sysfs walk**
  (`/sys/class/nvme-subsystem/nvme-subsys*/subsysnqn` to match the NQN, its
  `nvme{N}` entries as the controllers, `/sys/class/nvme/{ctrl}/address`
  parsed as comma-separated `key=value`), never by `nvme list-subsys -o json`.
  That is what the code has always done (`agent/cnagent/leg.go`
  `readSubsys`/`readCtrl`), and list-subsys cannot serve here: it reports no
  `ANAState` without a namespace device argument and answers an
  all-`inaccessible` namespace with an empty subsystem list.
* `dnagent.md` §2.3 SH27 + the `Serve` reference implementation + CM4/§4.2
  (`update_01.md` U4) — background tasks became part of the specified
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
  DN18 + §6 + §7 (`update_01.md` U4) — **the §9.4 trim protocol is replaced by
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
* `dnagent.md` DN18 + §2.3 + DN13 + SH15/SH20 (`update_02.md` U2/U4/U7) — the
  `migr_dst_info.target_info` probe row now names the SH20 sysfs walk (the
  IR3 amendment had corrected SH17/SH20 but left the old
  `nvme list-subsys -o json` wording in the DN18 table); the §2.3
  `agent.Serve` reference implementation matches the shipped code, whose
  cancel-then-join pair moved into a `defer` covering every return path (a
  reconcile or listen error also winds the background down before Serve
  returns); DN13 records the per-DN clone-metadata slot ceiling (≤ 24
  destination roles); and SH15/SH20 record that the nvme-host sysfs reads
  carry the SH15 soft timeout like every other OS touch — `update_02.md` U2
  closed that gap, wrapping `agent/nvmehost.go` `readTrimmed` through
  `cmdCtx` and the `agent/cnagent/leg.go` sysfs walk through the newly
  exported `agent.CmdCtx`.

## 6. Tests

Unit tests use `common.FakeOsClient` (scripted `RunCommandFn`/`ReadFileFn`/…
recording every call) and, for RPC-level tests, `bufconn` with the generated
`DiskNodeAgent` client — no root, no real devices.

1. **Fresh SyncupDn**: scripted empty probes; assert the DN5 sequence
   (`readblock` of the header, `writeblock` of the header, `writeblock` of
   table slot A, then the port attrs, `mkdir ana_groups/2`+`3`, the three
   one-time `ana_state` writes) and the `WriteProto` to `LocalDnPath`
   afterwards (SH5). No LVM string (`pvcreate`/`vgcreate`/`lvcreate`/…)
   appears anywhere in the recorded calls.
2. **Revision gate**: lower ⇒ `ReplyCodeStaleRevision` and zero mutating
   calls; equal ⇒ full idempotent pass; higher ⇒ apply + persist.
3. **Probe-first idempotency** (SH16): equal-revision `SyncupSide` against
   probes reporting a fully converged side issues no mutating command — which
   now also proves the disk-metadata re-load issues no `writeblock` — and, on
   a side whose `zeroed_bits` are all set, no `blkdiscard` of any form.
4. **Pointer diff**: `SyncupDn` dropping a side ⇒ DN6 top-down teardown
   sequence (exports, per-CN dms, `DnSideName`, the record's slot write) +
   `rm` of its files; `SyncupSide` for an unknown pointer ⇒
   `ReplyCodeUnknownObject`.
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
7. **PushMigrBitmap**: persist-before-apply call order; unknown `migr_id`
   rejected; applied set from files after a simulated restart (fresh server,
   same fake store) matches; chunk without a dm-clone still counts applied.
8. **CheckDn/CheckSide**: first reply carries full info; an unchanged round
   with `show_info = false` omits it; a probe flipped to error re-includes
   it (a wiped disk header is the DN case); reply `revision` echoes the
   stored one; unknown object ⇒ `ReplyCodeUnknownObject` with the stream kept
   open (SH25). A Check round never mutates — `writeblock` included — and
   never registers a zeroing goroutine (DN16).
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
    the window unattended; cancelling the role, tearing the side down, and
    restarting the agent all end it without leaving anything suspended, and a
    teardown inside the window resumes before it disables any namespace. The
    production default is pinned at `common.SuspendSeconds` = 60.
12. **Migration endpoints**: the destination sequence asserts the 8 KiB
    zeroing `writeblock` at the slot offset **before** the record's slot
    write, the wrapper `dmsetup create`, that the dm-clone's meta/dest devices
    resolve to the wrapper and the side device, and that the created table
    carries **`2 no_hydration no_discard_passdown`** — the dn role package's
    copy of the assertion the cn package already makes (`update_01.md` U1);
    teardown asserts
    clone removal → disconnect → wrapper removal → the `FreeCloneMeta` slot
    write → side-device removal. The source sequence asserts
    ana_grpid-inaccessible → the per-CN dm-linear **reload onto its
    dm-error** → the migr-src create, with nothing left suspended ([D12]).
13. **`diskmeta`** (`diskmeta_test.go`, on the fake's segment store): format
    and load round-trip; probe-first idempotency (zero `WriteBlock` on a
    formatted disk); identity mismatch refused; corrupt-header refusal; A/B
    slot alternation across three saves; a torn newest slot falling back to
    the older one; a stale slot rejected after a re-format because of
    `format_uuid`; both slots invalid ⇒ empty table at seq 0 and recovery on
    the next save; a failed save not committing in memory; allocation
    contiguity, the fragmentation fallback, the ext-count-mismatch error,
    `SetSideZeroed` range setting (a partial range, an idempotent re-set that
    issues no write, a range outside the record rejected) and
    exhaustion of both areas; clone-metadata zeroing ordered before the table
    write; free idempotency; `Describe` counts (the trailing `provisioning=`
    one included); and the envelope layout
    itself (magics, version, seq, a non-zero `format_uuid`, and that the
    layout constants tile without overlap up to `DnDataOffset`).
14. **`GetDnSize`**: the reply is `disk size − DnDataOffset`; a device at or
    below `DnDataOffset`, and a failing `lsblk`, both report through the gRPC
    status (DN3).
15. **cmd**: the §13 example `dnv-agent dn …` invocation parses; `--disk` is
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
    and the reply reports `zeroed_ext_cnt = k`, `total_ext_cnt = n`.
18. **Paced retry**: a scripted `blkdiscard` failure leaves that batch's bits
    unset, puts the command output into `side_dev_info`
    (`RES_STATUS_ERROR`, outranking `PROVISIONING`), and the next attempt
    comes no sooner than `common.DnZeroRetryInterval` — shortened through the
    same field-not-constant trick DN12's fence wait uses — never a hot loop;
    the following success clears the error back to `PROVISIONING`.
19. **Cancel and wait**: tearing the side down (DN6) while a batch is in
    flight cancels the goroutine and **waits** for it; the ordering assertion
    is that the `dmsetup remove` of `DnSideName` is recorded strictly after
    the in-flight `blkdiscard` returned, and the teardown's node **write** lock
    never deadlocks against the goroutine's table update (DN9's try-acquire
    rule). Same for a migration cancelled during destination zeroing, and for
    SH27: after `Serve` returns, no zeroing goroutine and no child command is
    still running.
20. **Write Zeroes fail-fast** (DN5): a scripted
    `/sys/class/block/{kname}/queue/write_zeroes_max_bytes` of `0` makes
    `SyncupDn` report `meta_info = RES_STATUS_ERROR` with `"disk lacks Write
    Zeroes"` while the rest of the converge still runs (DN19); a non-zero
    value, an absent attribute and an unreadable one all converge normally.
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

## 7. Acceptance checklist

1. `go build ./...`, `go vet ./...`, `go test ./...` pass.
2. `go list -deps ./cmd/dnv-agent | grep etcd` finds nothing (`layout.md`
   §3).
3. The §2.2 additions exist: `NvmetPortId`, `AnaGrpId*`, `ReplyCode*`,
   `DnMigrConnectRetryInterval`, `DnZeroBatchExtCnt`, `DnZeroRetryInterval` in
   `common/constants.go`; `DnNsIdentity` in `common/name_fmt.go`. `common/`
   still contains exactly the six files of `layout.md` §2.
4. `WriteFileDirect` is implemented per the amended `osclient.md`; a
   repo-wide grep finds no `WriteFile(` call whose path argument is under
   `/sys/kernel/config`, and no `ana_state` write outside `EnsurePort`.
5. `cmd/dnv-agent` wires **server** interceptors only (`grpc.md` §4 table);
   `grep -F "per exported namespace" doc/architecture.md` finds nothing
   (the [D4] amendment is applied).
6. `grep -rnE "pvcreate|vgcreate|lvcreate|lvchange|lvremove|\\blvs\\b|\\bvgs\\b|\\bpvs\\b" agent/ cmd/ common/` finds nothing outside comments and test-guard string literals — **repo-wide**: no dnv agent runs any LVM command ([D13], [D14]; `update_01.md` U3 superseded the dn-only rule).
7. A manual run of the §13 example starts `dnv-agent dn`, serves
   `GetDnSize`, and a `SyncupDn`/`SyncupSide`/`CheckSide` round-trip shows
   one trace id across `grpc server request`, `os command` and
   `os write file direct` records.
8. `grep -rn "trimmed" pb/schema.proto agent/` finds only the `reserved 3;`
   comment in `DnDiskTable.SideRecord`: the trim flag is gone and DN9's
   zeroing protocol replaced it (`update_01.md` U4).
9. `grep -rn "zeroout" agent/` hits only the DN9 zeroing path — never the
   DN13 clone-metadata slot preparation, which stays a plain `WriteBlock` of
   zeros, and never the CN clone-metadata arena, whose recycle guard is a
   plain `blkdiscard` hole punch (`update_01.md` U3/U4).
10. Both `agent.CloneTable` call sites pass `noDiscardPassdown = true`, and a
    test in `agent/dnagent` asserts the dn table's
    `2 no_hydration no_discard_passdown` (`update_01.md` U1).
11. `agent.Serve` takes a `waitBackground func()` and calls it after
    `GracefulStop` (SH27); the dn passes `srv.WaitBackground` and the cn
    `nil`; a shutdown test shows no zeroing goroutine and no `blkdiscard`
    child surviving `Serve`'s return.

### Integration-run fixes (first on-hardware run of the U1-U5 tree)

Found by running `integtest/dnagent_test.sh` and `integtest/cnagent_test.sh`
against two real VMs (kernel 7.0, nvme-cli 2.16, mdadm 4.5) — the first
execution of either suite since `update_01.md` was applied. All five were
real agent defects, not harness problems; every one is now covered by a unit
test that fails without the fix.

* **IR1/IR2 (SH20)** — `Connect` passes `--fast_io_fail_tmo` (nvme-cli's own
  underscore spelling) and an explicit `--hostid common.NvmeHostId(hostnqn)`.
* **IR3 (SH17, SH20)** — `ListSubsys` reads `/sys/class/nvme-subsystem` and
  `/sys/class/nvme` instead of `nvme list-subsys -o json`, which carries neither
  the namespace device nor `ANAState`. Without it a migration destination
  reported `"controller has no namespace"` forever.
* **IR4 (DN6)** — the dm-clone is retired **after** the per-CN dm layer, not
  before it. DN6's prose listed the dm-clone ahead of the per-CN
  `DnLinearName`/`DnErrorName` pair (the code always had it right for a full
  side teardown); the §11.2 *finish* path really did remove it first, hit EBUSY
  and leaked the clone, its metadata wrapper and the side device under them —
  unrecoverable by any later empty side list. DN6 now states the layering rule
  and the finish carve-out, and the retirement is idempotent: a pass that cannot
  remove the clone keeps the applied `migr_dst_conf` so the next converge
  retries, rather than continuing on to disconnect a live clone's source.
* **IR5 (SH17)** — `device_uuid`/`device_nguid` are compared through the shared
  `agent.SameNsId` (strip `-`, fold case), never byte-wise. SH17's "tolerate
  normalized read-back" rule now names both cases it covers.
