# dnagent_integtest.md — integration test plan for `dnv-agent dn`

This document owns the intent of the dn agent's integration suite,
`integtest/dnagent_test.sh`, and of its driver `dnagentctl`: what the suite
proves, the conventions its assertions rest on, what a pass means and how it
cleans up. It leans on `dnagent.md` for the contract under test, on
`architecture.md` for the procedures and decisions it cites, on `grpc.md` for
the driver's trace-id rule, and on `log.md`, OS commands and file IO, for the
record names its log checks read.

## Goal and scope

The suite proves that a real `dnv-agent dn` — real device-mapper, the dnv disk
format on a block device, nvmet configfs, nvme-tcp — behaves as `dnagent.md`
specifies on two lab VMs driven over gRPC from the developer machine,
exercising every `DiskNodeAgent` RPC and reading the kernel state itself
beside the agent's report. It proves the happy path: error paths are out of
scope but for the restart case's stale probe, migr_full's lost source,
store_full's full local store and blocksize's disks of another block size,
and a leftover reply is no exception, being an accepted request with residue
(`dnagent.md` SH9, DN19).
The cases run in a fixed order, fail-fast — smoke, zeroing, sides, migr_full,
migr_bitmap, teardown, restart, loglevel, store_full, blocksize — each that
builds a pool under storage-pool ids of its own, and each ends with neither
node holding anything of its pool; loglevel and blocksize build none, and
blocksize ends with nothing of its own left on its node.

Beside the suite, the Go tests of `agent/dnagent` run a real dn agent inside
the test process, behind the real server interceptors over an in-memory gRPC
connection, and drive it with a fake worker over the package's fake OS
client, which answers the agent's device-mapper, nvmet, nvme host, raw-disk
and local-store calls from memory and records each one. They prove what the
lab cannot hold or time: a disk node of the most sides one holds, built and
exported with every round clean; the disk header at the room of its block
and the volume table at the room of its slot, and one byte past each; the
bound the dn agent puts on a migration chunk's index (`dnagent.md` DN15);
and orderings across many syncups and rounds that need a scripted node: a
restart with the local store kept and with it lost (`dnagent.md` DN2, DN6),
an OS command that hangs across rounds and past the pass budget
(`dnagent.md` SH15, DN13), a syncup that arrives during a converge
(`dnagent.md` SH30), and a sweep whose listings name objects already gone
(`dnagent.md` DN6, DN16).

## Topology

The developer machine ships the agent over ssh and drives two lab VMs over
gRPC, never touching the data path. Each VM runs one `dnv-agent dn` as root, a
disk node of one cluster, on a loop device in the suite's work directory with
a dedicated local store, one address serving ssh, gRPC and nvmet. The
blocksize case runs one more on the first VM, with a gRPC port, an nvmet
port and a local store of its own, over disks of its own (Cases). No cn agent
runs: the controller nodes are emulated hosts on the two VMs, each connecting
under its own host NQN with the host id derived from it (`common.NvmeHostId`),
as a VM holds several host NQNs and the kernel keeps one per host id. A
migrating leg's primary connects to both its sides, which export one NQN and
one namespace identity (`architecture.md`, [D1]), so the host merges them into
one multipath namespace; hence the sides' distinct cntlid slots
(`architecture.md`, cntlid slots), and a dead path whose NQN a live path
shares is dropped by its controller (`dnagent.md` SH20).

## Assumptions and preflight checks

Assumed and not checked: both VMs are linux/amd64, which the agent is built
for, on one lab image whose kernel has dm-clone. The preflight installs nothing
on the VMs — on the machine that runs the suite it builds `gojq` into
`integtest/bin` when that machine has no `jq` (`layout.md`, Directory tree) —
and fails fast, its per-VM half running after the start-of-run cleanup, since
free agent ports mean something only once a crashed run's agents are gone. It
supplies the kernel modules and the configfs mount, which the agent neither
loads nor mounts, without verifying each module, and checks what the suite and
the agent need and the agent does not check itself: passwordless ssh and sudo,
the tools (no LVM: the dn agent runs none, `architecture.md`, [D13]; udevadm,
for the udev rules), nvmet configfs present, native multipath on, which the standby assertions and the
migration merge need, the work directory's free space and punch-hole support,
and the agent ports free. The suite rests on these lab facts:

* The VMs' uutils dd silently mishandles direct IO, with false failures and
  dropped writes, so the suite never passes dd an input or output flag at all:
  writes are synced, and media reads follow a cache drop.
* Zero-out on a loop device allocates the file's range, neither IO nor a hole
  punch, so a side zeroes at once and side zeroing never rests on the
  punch-hole probe: the suite exercises the zeroing protocol, not its timing.
* An inaccessible namespace has no device node, and IO to a namespace with no
  serving path queues. A connect returns before the namespace scan makes the
  path's device, so every ANA check that follows a connect or an ANA move is a
  bounded poll, read per path from sysfs (`dnagent.md` SH20): nvme-cli's
  listing carries no ANA state, and it prints nothing at all on a node with no
  controller.
* A port or subsystem removed under a live connection, while the port keeps
  listening, kills the host's controller for good once its reconnect attempt
  finds the subsystem still gone (a port left with no subsystem stops
  listening instead, and its controllers retry). While something holds a
  subsystem's multipath head open, as a dm table over it does, the kernel
  keeps the subsystem's directory after its last controller, with no
  controller and no namespace node in it: "disconnected" means no controller
  (`dnagent.md` DN6).
* A namespace removed from an export under a live connection leaves the
  host's controller live, which the stage that removes one checks before
  it compares anything. That the namespace node goes too while a dm table
  holds it open is not taken for granted: where the node stays, the stage
  removes the whole export instead, with the effect the item above gives
  (Cases, migr_full).
* A dm-clone's table reprints its creation arguments, keeping no-hydration
  after hydration is enabled; only its status shows the live flags.

## The driver: `dnagentctl`

`dnagentctl` exists because the agent serves plaintext gRPC without
reflection: one subcommand per `DiskNodeAgent` RPC, each reply printed as
protojson, and a failure on a gRPC error or an unexpected reply code. It sends
the stage's trace id as metadata and in `Check*` requests (`grpc.md`, Drivers
and fakes), and prints a leg's namespace identity (`common.DnNsIdentity`) and
a host NQN's host id, which the script never derives itself.

The agent answers a check round and an info read from its latest observation of
the object, and only a round starts a read of the node (`dnagent.md` SH28,
SH29). No worker runs in this suite, so no rounds but the suite's own start
reads, and the driver's two round commands have a fresh mode: they send rounds
on one stream until one answers from an observation read wholly after the agent
answered the command's first round, with no converge of the object in flight,
and print and judge that reply alone; this document calls such a command a
fresh round. An observation's time is when its reading ended, so the first one
to end after that answer may have begun before it; an object's reads run one
at a time, each counting as the running read until it stores, and a converge
excludes a read by its locks (`dnagent.md` SH14, SH29), and a disk node's read
holds only the node read lock, which lets the next read start before the first
has returned. An observation newer than that one was therefore read wholly
after that answer, at the cost of about one read more. Where a background
converge of the object can run, the DN12 fence timer's on a migration
source (`dnagent.md` SH28), the observation a fresh round reports can be
that converge's rather than a read's. The pollers, which wait until a side is zeroed
or a destination hydrated, the latter through `common.ParseCloneStatus`, sample
with rounds on one stream: the first sample is a fresh round's reply and each
later one a reply with a newer observation, since neither the zeroing loop nor
the kernel's hydration stores an observation and a read a round starts brings
their progress in. The info-read commands are plain memory reads.

## Conventions

* **Revisions.** One monotonic counter per disk node, undercut only by the
  stale probe and by store_full's re-send of its refused disk node request,
  which stays above the revision the agent holds for the disk node (Cases,
  store_full); equal-revision re-sends are deliberate re-applies (`dnagent.md`
  SH8, SH16). A side's pointer precedes its `SyncupSide` (DN8), except once
  in store_full, where a side's `SyncupSide` comes when the only request that
  had carried its pointer was refused, so the agent holds no pointer for it.
* **The provisioned flip.** No worker runs, so the suite plays its flip
  (`architecture.md`, Side provisioning protocol): a new side is synced
  unprovisioned, waited on until it reports the length to zero it asked for,
  all of it zeroed, and re-sent at a higher revision with `provisioned` set,
  which later requests keep (`dnagent.md` DN9); the flip's reply must report
  the same, except a migration destination's, whose flip is its gated
  declaration. Every request for a side asks for the length a worker derives
  from the side's group, a data group's for every side but the zeroing case's
  meta-group side. Before the flip every per-CN row must read provisioning
  and the node hold no export, dm-error or dm-linear of the side (DN10): the
  kernel check, scoped to the side as a sibling's export is live, catches what
  the agent's own report would hide.
* **The cutover.** The suite plays the worker's cutover too (`architecture.md`,
  Migration): in the migration cases every request that carries a
  migration's conf carries its cutover flag until the cutover, as the
  worker's do (`dnv-worker.md` RW15), and the cutover is the source's request
  with its destination provisioned and the flag cleared, the destination's
  following it without the flag.
* **Negatives are exact.** Provisioning is healthy (`dnagent.md` DN18), so no
  check is a bare "not OK": each names its exact status — absent or missing
  where nothing may be built, which at the no-migration level covers a
  destination's migration rows even while it provisions, the level winning
  over the gate (DN11); provisioning where the gate holds a resource back; and
  provisioning or OK only for a new side's own row right after its
  unprovisioned sync, a genuine race. The residue and dm listings, the
  agent-log readers and the host's controller lookups fail their stage when
  their tool did not answer, since a failed read taken for an absence passes
  every negative built on it (SH15); the pin release alone reads an unanswered
  `dmsetup info` as a device already gone.
* **Check rounds.** A stage that reads the agent after a fault, behind a
  converge, for progress no converge stores — a zeroing, a hydration — or to
  count what a read does sends a fresh round, and a poll samples with rounds
  (The driver: `dnagentctl`); an info read answers from whatever observation
  the agent holds, so a stage uses one only where the last converge's
  observation, or the one a round of the stage just answered from, holds what
  it reads. In smoke, sides and store_full at steady state, a fresh `CheckDn`
  round must reply code zero, the stored revision and the node rows OK, and a
  fresh `CheckSide` round the side's revision and its side-device row OK
  (`dnagent.md` DN17, SH24 to SH26), fresh because the disk node's last
  observation is its `SyncupDn`'s, older than the side converges that followed
  it; the migration cases run a plain `CheckDn` round after their teardown,
  which answers from the observation the teardown's `SyncupDn` stored, the
  restart case compares snapshots instead, as store_full does across its
  restart, the teardown case reads residue and the verdicts of reads made
  after its drops, and store_full sends one plain round right after a
  refused request, which answers from the observation the agent held before
  it.
* **Log level.** Every agent of the suite runs at `--log-level debug`, the
  blocksize case's too, so its log holds its reads, which the cases read
  (`log.md` R6, R9), save one node's agent in the loglevel case, the one
  case at the default log level.
* **Observed, never asserted.** The provisioning and read-through windows are
  races a loop device usually wins; the cutover window lasts `SuspendSeconds`
  and is sampled once right after the cutover reply, which a slow step can
  miss; and whether the teardown case's clone still hydrates when its source
  vanishes decides only whether its removal blocks. Each is logged as hit or
  missed, never failed on.

## Cases

Setup installs the udev rules of `deploy/udev/` on both VMs before the agents
(`architecture.md`, Components: invocation reference): a disk node's side
devices and their exports carry a controller node's md superblocks and
tenants' data, which the dm rule keeps the system's rules off. Setup then
launches the agents (`dnagent.md` CM1, CM2) and proves each alive by
its first `GetDnSize` reply, exactly the disk's data area (DN1, DN3, SH1)
and the VM's host name as the VM reads it (SH34), and its disk format and
port by a baseline `SyncupDn` (DN5, SH19;
`architecture.md`, [D4]). As the agent's gRPC listener opens before its
startup pass (SH1, SH33), the driver's wait for that reply, here and after
every restart, goes on reading `GetDnInfo` until the agent no longer answers
that it is starting.

**smoke** proves the plumbing: one side through the flip exported to one host,
its path live and optimized, a data round trip, clean check rounds, and the
side torn down by an empty side list (`dnagent.md` DN6 to DN10, L1, L2, L5,
L7). Before the teardown it proves the age gate on a real kernel (`dnagent.md`
DN6, SH18): two exports of the side's sp for legs no side holds, made by hand
with no port link, one with no namespace and one with namespace one whose
"device_path" was never written, which DN6 counts as none. While they are
young the fresh rounds stay clean, the agent's log shows each one's age read
in the agent's own process at least as many times as the stage sent fresh
rounds under its trace id, and no listing of their namespaces or of the
ports, listings the agent makes in its own process too (`dnagent.md` SH18),
and their directories' times, read from the shell before and after, do not
move. Once they are older than the orphan grace, the real one, as nothing
backdates them, a fresh round reports both as leftovers, the agent's log
shows each one's namespaces, and the ports, listed as many times as its age
was read, at least once, and the next syncup removes both, which the case's
residue check proves too. Each count is a total over the
reads the stage's own rounds started, not a count per read, as a read logs
under the trace id of the round that started it (`dnagent.md` SH29) and a
fresh round sends several rounds, each of which can start a read; the stage
counts once every such read has ended.

**zeroing** proves that a side zeroes the length its request asks for and not
a byte past it (`architecture.md`, Side provisioning protocol; `dnagent.md`
DN9). Only the agent knows where a side's extents lie, so a random pattern
first covers the whole extent area of one node, which holds no side between
cases, the same block in every extent. A side of a meta group, which asks to
zero all of it, and a side of a data group, which asks to zero its meta region
and first data block, then provision through the flip. Read through their side
devices after a cache drop, the first reads zero throughout, and the second
reads zero over the length it asked for and holds the pattern from there to
its end. While the two sides exist, the node's disk counts (`dnagent.md`
DN18) count their extents as taken, with side records that match the side
pointers; once the sides are gone, they count the whole data area free.

**sides** exports sides on both nodes to two controller nodes whose roles
cross: primary paths are optimized and carry data, standby paths are
non-optimized and fail a read, being backed by their dm-errors, and every
export admits exactly one host NQN, its own controller node's (`dnagent.md`
DN10). A standby's dm-linear suspended by hand with the side device's table
waiting, as a reload stopped after its load leaves it, comes back resumed on
its dm-error with no table waiting once its syncup is sent again, by a
clear and a resume and no reload: a resume keeps the live table
(`dnagent.md`, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`). The same
dm-linear suspended by hand then makes a check round's read answer the
leftover code naming it, and the `SyncupSide` sent again resumes it
(`dnagent.md` SH31, DN16).

**migr_full and migr_bitmap** prove `architecture.md`, Migration, with two
migrations at once in opposite directions, each node the source of one and the
destination of the other, so that every stage's two calls go to different
agents: a destination declared at the no-migration level provisions with no
clone and no connection (`dnagent.md` DN11, DN13); at the read-write level,
the cutover still pending, it builds its metadata wrapper and its export
over its dm-error and nothing past them, no connection and no clone, the
host's path to it staying live and inaccessible, and its two migration rows
read provisioning, waiting for the cutover, with the reason the worker's
cutover reads, on a converge and in a read made after it alike, which a
fresh round and then an info read report (DN13, DN18); a source told its
destination is not provisioned, the cutover pending, serves as if no
migration existed, its migration rows reading the same wait with no reason
(DN12); the cutover fences the source
(`architecture.md`, [D12]), whose path the host then reads inaccessible; one
converge enables the destination, whose connection
exercises `dnagent.md` SH17 and SH20, and whose clone's table carries exactly
the no-hydration and no-discard-passdown pair (DN13; `architecture.md`, [D7]);
the last must-copy region reads from the destination as the source holds it,
hydrated or not; the finish leaves nothing of the migration on either node,
the per-CN linear repointed and the source side dropped (`dnagent.md` DN6, L1
to L4, L6), which the teardown's residue check proves; and the data verifies
through the destination path, which also takes a write.

**migr_full** pushes no bitmap: the destination's reply reports an applied
set for its own migration, and the set is empty (`architecture.md`, Bitmap
push protocol; `dnagent.md` DN14), the whole device matches the source, and
no discard reaches any clone. Its first migration,
once hydrated, also loses its source's namespace behind both agents' backs,
the export and the destination's controller of it staying: the
destination's rows — its side device, its migration rows and its per-CN
rows — then read one status, with one epoch, on a converge, in the
observations a fresh round and then an info read report, each made after
that converge by a read, and on a second converge (`dnagent.md` DN13,
DN18). Each answers code zero over a live controller with no namespace,
which no converge connects beside, and the leftover code naming the
destination's two rows where the controller is gone, which the re-sent
syncup's connect cures (`dnagent.md` DN16). The agent runs no command on
the destination's dm-clone or per-CN dm-linear under a trace id of its own
between the stage's calls (`dnagent.md` SH31). The converge
takes the primary's stack
off the dm-clone and, with no source to read from, does not read the
dm-clone's status line, which the agent's reads still record: only a read's
observation then proves the hydration the gateway reads, and the stage's
hydration poll, which samples until an observation carries that line, finds
one showing every region hydrated. The state compared is measured, never
assumed, and one that moves before the comparison ends fails the stage.
**migr_bitmap** pushes skip-bitmap chunks of its own making, not a bitmap
read from a pool, with no revision (`dnagent.md` DN15), over the same random
source, so every difference is the bitmap's. The first migration's chunks go
to the gated destination: the applied set lists exactly the chunks (SH21,
DN14), and the enabling converge's own hydration sample counts every skipped
region. The second migration's chunk, its whole bitmap, comes only once its
destination serves, the primary's per-CN dm-linear on the dm-clone, with
its copy slowed on its source's node from before the enable so that the
regions the chunk marks are still ahead of the copy (the late push): the
count of regions the dm-clone holds as copied jumps by at least the regions
the chunk marks, and the log shows that dm-linear suspended and resumed
around the apply, the time between logged (`architecture.md`, Bitmap push
protocol, The apply pauses the writers). For both, the must-copy head
matches the source, the skipped regions differ from the source's fresh
random data, keeping what the destination's extents held before
(`architecture.md`, Side provisioning protocol), and the log holds one
discard that landed on the clone, at the range the meta-region shift
implies (`dnagent.md` SH22, SH23; `architecture.md`, Bitmap push protocol).
The discard and pause checks read the agent's JSON log by design: if its
layout drifts, the reader is adjusted, never the assertion. The copy is
slowed by a token bucket on what the source's node sends from its nvme-tcp
port to the destination's address, and nothing else; the bucket is taken
off once the chunk is checked, a failing run takes it off at its exit, and
the cleanup takes it off first, whatever else fails.

**teardown** proves `architecture.md`, Teardown by sweep: what cannot go yet
is reported and finished later, never forgotten (`dnagent.md` DN6, DN7, DN16,
DN19, SH7), under the two shapes a blocked removal takes. A removal that
outlasts a pass: a migration's source export is yanked and the destination
side dropped at once, so its clone comes off before the connection it hydrates
through (`dnagent.md` L3), over a source already dead; the same revision is
re-sent until clean, only the leftover code tolerated, nothing of the migration
may remain on either node, and a fresh `CheckDn` round, whose verdict is
that of a read made after the drop, replies clean, which proves its
allocation and clone-metadata records went with their devices, an orphaned
record being a leftover itself (DN6's record step); that a record never goes
before its device is left to the unit tests. A removal blocked from outside
the agent: a side device is held open and the side dropped; the leftover
reply names it, the residue is exactly that device (DN6's layers), the
verdict of a read made after the drop, which a fresh round reports, agrees,
and the same revision, re-sent once the device is free, finishes.

**restart** proves persistence and an idempotent reconcile (`dnagent.md` SH1,
SH4 to SH6, DN2): with a connected side on one node and a gated destination
holding a chunk on the other, both agents restart over untouched kernel state
while the host keeps reading. Every snapshot, on both sides of the restart,
is a fresh round's reply, so it shows the node as a read after the command
began found it. The reloaded infos, revisions and the zeroing's byte counts
included — the counts are stored in the volume table on the disk
(`architecture.md`, Side provisioning protocol) — equal the snapshots but for
the status epochs (SH14) and the replies' stamps, which name an observation's
times and trace id and so differ by nature; the chunk is still applied (SH21);
unchanged re-sends leave the new log free of any mutating command or block or
configfs write (SH16, SH17, DN5, DN9); and a revision below the one those
re-sends stored is refused as stale in a normal reply (SH8, SH9, DN4).

**loglevel** proves at the default log level what a log holds (`log.md` R6,
R9, R13; `grpc.md` L6): one node's agent restarts with no `--log-level`; a
fresh round that changes nothing logs, under its trace id, no record but its
stream's open and close — none of its messages and none of the reads it
starts; a disk node request still logs its call and its save to the local
store at Info, and no Debug record; SIGUSR1 to the agent's process logs one
`log level changed` record naming debug and info, after which a fresh round
logs its messages and its reads at Debug; SIGUSR2 logs one naming info and
debug, after which a round is quiet again. The agent then restarts at
`--log-level debug` for the cases after it. The other node takes no part.

**store_full** proves on a real local store that a request whose save fails is
refused, that the node's store row reads the refusal until a probe holds, and
that a restart keeps what the agent accepted (`dnagent.md` SH4 to SH6, DN2,
DN8, DN18). One node's agent runs over a small tmpfs that holds a copy of its
local store, so it holds what it held before, and one file fills the tmpfs
until a write fails. Every request the case expects refused for its save
carries a revision above the one the agent holds for its object, so the
stale-revision gate cannot be what refuses it, and the reply's details name the
failed save. A disk node request that adds a side is refused with
`common.ReplyCodeSaveFailed` at the held revision, the store's files keep their
bytes and no temp file stays; the side's own syncup is an unknown object, and
nothing of the side is built, by the next round's revision, the disk counts of
a read made after the refusal (DN18) and the kernel's listings. A fresh round
after the refusal reads the node's store row in error, with the full store's
error, and its read's refused probe leaves the store's files as they were; with
the file removed, fresh rounds alone, with no request sent, read the row OK
within a few seconds, which proves the probe (DN18). Then the refused request
sent again at its own revision, as a worker's next round sends it while the
desired state stays the same (`dnv-worker.md` RW4, RW5), is accepted, and the
side provisions through the flip. Filled again, a side request and a disk node
request that drops a side are refused alike, and the node keeps every device
and export of the pool. The agent then restarts over the full store: fresh
rounds before and after the restart agree as in the restart case, the store row
aside, at the revisions of the last accepted requests, the node's devices and
exports are the same, the new log holds no mutating command, and the store row
reads the error before the restart and OK after it, since a restart forgets the
refusal (`dnagent.md`, Known limits). Once the pool is torn down, the agent
goes back to its disk store, holding the last request it accepted. The other
node takes no part.

**blocksize** proves on real loop devices that a disk node takes a disk of
512-byte logical blocks alone, and that it reads a partition's block size
from its disk (`dnagent.md` DN3, DN5, SH17). Its agent runs over one disk
at a time, each a loop device over a file of its own or a partition of
one, whose block size and shape the case first reads back from the kernel. Over a loop device of
4096-byte blocks, `GetDnSize` is refused with an error that names the disk,
the size and both fixes, and a `SyncupDn` replies the leftover code, its disk
row in error with the same text and its meta row refusing the format, and
writes nothing: the head of the disk, where the header and both volume-table
slots lie, still reads zeros. Over a partition of a loop device of 512-byte
blocks, `GetDnSize` answers the partition's data area and a `SyncupDn`
formats it, every row OK; over a partition of a loop device of 4096-byte
blocks, `GetDnSize` is refused. The case removes its nvmet port, its loop
devices and their files at its end. A disk formatted at 512 that reads 4096
later, which converges as it is, is left to the unit tests.

**What a pass means.** Exit status zero and PASS mean every case's assertions
held in a run that stops at the first failure: data checked against a pattern
file the agent never sees or against zeros, removals proved by the node's own
listings. The observed windows are only logged, so a pass need not have caught
a source mid-cutover, a read mid-hydration or a clone removal blocking on its
dead source. The restart case prints, not asserts, whether each old agent
stopped; one that survives keeps its endpoint and answers the later steps, so
that pass is as good as that line.

## Teardown and cleanup

Cleanup runs unconditionally at the start of every run and at its end only on
success, its first step taking off migr_bitmap's token bucket; a failing run
leaves its debris, save that bucket, which it takes off at its exit since
other suites share the VMs, and dumps the tail of each agent's
log, the dm, nvmet and host state of both VMs, a listing of each local store
and of any file system mounted under the work directory, the failing stage's
trace id and, when the failing case is zeroing, a migration case, teardown or
store_full, the last info of every side that case has set up since it last
tore all its sides down, an info read's, which shows the side as the last
converge or read left it and answers even while a converge is wedged. A
cleanup-only run does the scrub alone. It takes every `dnv-agent` and every
dnv dm device on the node, of either role, which is why one dnv suite runs at
a time in the lab (`layout.md`, Directory tree), and the blocksize case's
nvmet port and loop devices beside the suite's own. Each step is best-effort,
and the order is load-bearing on one principle, a holder goes before what it
holds: the agents
go first, a graceful stop joining their children (`dnagent.md` SH27) while one
killed outright can leave a zeroing child holding a side device a moment
longer, which a final retry sweep absorbs; any process a failed stage left
holding a device goes before the devices too, as nothing removes an open
device; host controllers before the nvmet objects they are connected to; dm
devices top-down, a clone before the migration connection and export it
flushes to on removal; and a disk's header block zeroed before its loop device
is detached, which leaves its volume-table slots inert (`architecture.md`,
[D13]; `dnagent.md` DN5); the udev rules go last. Two more rules keep the removals from wedging:
every suspended device is resumed first, since a run killed in a
cutover window leaves the source's linears suspended and a suspended device
wedges its removal, the namespace disable above it and any block scan; and
both VMs are cleaned at once, since a node's migration source export backs its
peer's clone. The helper running all this lives outside the work directory it
removes, and every run ships it afresh.

A tmpfs over a local store, which store_full mounts, comes off before the work
directory goes, once the agents are gone and so hold no file there: the
directory's removal cannot remove a mount point, and the next run's agent
would keep its store on that small tmpfs. An unmount that finds it busy
detaches it lazily rather than wait. A failing store_full run takes its tmpfs
off itself, once the dump has listed the store's files, so no mount of the
run outlives it over the local store; the requests the agent accepted after
the move lived only on the tmpfs, and the dump is what keeps their names.
That node's agent stays stopped: started over the disk store below, it would
converge the older requests held there and remove the debris the run leaves.

The wipe is a separate flag that runs no case: it reads no dm kind, taking
dnv-named residue whatever its name decodes to, every dnv nvmet subsystem and
md array and, past dnv, every fabrics controller on the node, a reach that
makes it a flag and not a step; then, when it does not fail (below), it runs
the ordinary cleanup. It repeats
while dnv dm devices remain, as udev can re-assemble an array from its
members, and fails if a dnv dm device or nvmet subsystem is left or the dm
listing did not answer, as reporting a residue is not success; md arrays are
only reported, a guest's own not being the suite's to fail on.

## Out of scope

Error paths beyond the stale probe, migr_full's lost source, store_full's
full local store and blocksize's disks of another block size, the
provisioning error rows among them (`dnagent.md` DN9); a migration
destination that waits for a free run in the clone-metadata area,
and the other states of the destination's shared row rule, whose rows the unit
tests cover (DN13); the destination's retries of a failed connect within its
pass, left to the unit tests (DN13); the cn role; fault injection under a live
stack beyond that lost source, the teardown case removing a real remote
instead; a shutdown that meets a background task starting or a fence timer
armed (`dnagent.md` SH27) and whether a refused request leaves a lock entry
(`dnagent.md`, Concurrency — `locks.go`), both left to the unit tests;
performance; TLS and authentication; levels other than read-write and
no-migration (`dnagent.md` DN11); lock contention on one agent; the source
half of a cancelled migration; a zeroing batch cancelled mid-flight.
