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
scope but for the restart case's stale probe, and a leftover reply is no
exception, being an accepted request with residue (`dnagent.md` SH9, DN19).
The cases run in a fixed order, fail-fast — smoke, sides, migr_full,
migr_bitmap, teardown, restart — each under storage-pool ids of its own, and
each ends with neither node holding anything of its pool.

## Topology

The developer machine ships the agent over ssh and drives two lab VMs over
gRPC, never touching the data path. Each VM runs one `dnv-agent dn` as root, a
disk node of one cluster, on a loop device in the suite's work directory with
a dedicated local store, one address serving ssh, gRPC and nvmet. No cn agent
runs: the controller nodes are emulated hosts on the two VMs, each connecting
under its own host NQN with the host id derived from it (`common.NvmeHostId`),
as a VM holds several host NQNs and the kernel keeps one per host id. A
migrating leg's primary connects to both its sides, which export one NQN and
one namespace identity (`architecture.md`, [D1]), so the host merges them into
one multipath namespace; hence the sides' distinct cntlid slots
(`architecture.md`, cntlid slots).

## Assumptions and preflight checks

Assumed and not checked: both VMs are linux/amd64, which the agent is built
for, on one lab image whose kernel has dm-clone. The preflight installs
nothing and fails fast, its per-VM half running after the start-of-run
cleanup, since free agent ports mean something only once a crashed run's
agents are gone. Per VM it checks passwordless ssh and sudo and the tools (no
LVM: the dn agent runs none, `architecture.md`, [D13]); loads the nvmet,
nvme-tcp, dm-clone and loop modules, without verifying each, and mounts
configfs, neither of which the agent does; then checks that nvmet configfs is
present, that native multipath is on, which the standby assertions and the
migration merge need, and the work directory's free space and punch-hole
support. Once each loop device exists, setup checks that it offers Write
Zeroes, whose absence the agent's fail-fast (`dnagent.md` DN5) would report
less legibly. The suite rests on these lab facts:

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
* A dm-clone's table reprints its creation arguments, keeping no-hydration
  after hydration is enabled; only its status shows the live flags.
* A dm-delay device wedges udev's workers unless a "58-*" udev rule turns off
  the device-mapper disk and other rules for it; the suite builds none and
  installs no such rule, so a udev stall here suspects that rule first.

## The driver: `dnagentctl`

`dnagentctl` exists because the agent serves plaintext gRPC without
reflection: one subcommand per `DiskNodeAgent` RPC, each reply printed as
protojson, and a failure on a gRPC error or an unexpected reply code. It sends
the stage's trace id as metadata and in `Check*` requests (`grpc.md`, Drivers
and fakes), runs the polling loops until a side is zeroed or a destination
hydrated, the latter through `agent.ParseCloneStatus`, and prints a leg's
namespace identity (`common.DnNsIdentity`) and a host NQN's host id, which the
script never derives itself.

## Conventions

* **Revisions.** One monotonic counter per disk node, undercut only by the
  stale probe; equal-revision re-sends are deliberate re-applies (`dnagent.md`
  SH8, SH16). A side's pointer precedes its `SyncupSide` (DN8).
* **The provisioned flip.** No worker runs, so the suite plays its flip
  (`architecture.md`, Side provisioning protocol): a new side is synced
  unprovisioned, waited on until its zeroed and total extent counts both equal
  the count it asked for, and re-sent at a higher revision with `provisioned`
  set, which later requests keep (`dnagent.md` DN9); the flip's reply must
  show the same equality, except a migration destination's, whose flip is its
  gated declaration. Before the flip every per-CN row must read provisioning
  and the node hold no export, dm-error or dm-linear of the side (DN10): the
  kernel check, scoped to the side as a sibling's export is live, catches what
  the agent's own report would hide.
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
* **Check rounds.** In smoke and sides at steady state, a `CheckDn` round must
  reply code zero, the stored revision and the node rows OK, and a `CheckSide`
  round the side's revision and its side-device row OK (`dnagent.md` DN17,
  SH24 to SH26); the migration cases run the `CheckDn` round after their
  teardown, the restart case compares snapshots instead, and the teardown case
  reads residue and read-only verdicts.
* **Observed, never asserted.** The provisioning and read-through windows are
  races a loop device usually wins; the cutover window lasts `SuspendSeconds`
  and is sampled once right after the cutover reply, which a slow step can
  miss; and whether the teardown case's clone still hydrates when its source
  vanishes decides only whether its removal blocks. Each is logged as hit or
  missed, never failed on.

## Cases

Setup launches the agents (`dnagent.md` CM1, CM2) and proves each alive by its
first `GetDnSize` reply, exactly the disk's data area (DN1, DN3, SH1), and its
disk format and port by a baseline `SyncupDn` (DN5, SH19; `architecture.md`,
[D4]).

**smoke** proves the plumbing: one side through the flip exported to one host,
its path live and optimized, a data round trip, clean check rounds, and the
side torn down by an empty side list (`dnagent.md` DN6 to DN10, L1, L2, L5,
L7).

**sides** exports sides on both nodes to two controller nodes whose roles
cross: primary paths are optimized and carry data, standby paths are
non-optimized and fail a read, being backed by their dm-errors, and every
export admits exactly one host NQN, its own controller node's (`dnagent.md`
DN10).

**migr_full and migr_bitmap** run two migrations at once in opposite
directions, each node the source of one and the destination of the other, in
lockstep stages whose two calls go to different agents, along
`architecture.md`, Migration: the destination provisions first and is declared
at the no-migration level, with no clone and no connection (`dnagent.md` DN11,
DN13); the source, told its destination is not provisioned, serves as if no
migration existed (DN12); the cutover fences the source (`architecture.md`,
[D12]) while the hosts hold IO; one converge enables the destination, whose
connection exercises `dnagent.md` SH17 and SH20, and whose clone's table,
never reloaded, must carry exactly the no-hydration and no-discard-passdown
pair (DN13; `architecture.md`, [D7]); a read of the last must-copy region
matches the source, hydrated or not; the finish repoints the per-CN linear
before the clone, its connection and its wrapper go, then drops the source
side (`dnagent.md` DN6, L1 to L4, L6); the host disconnects the dead source
path by its controller, as the shared NQN's other path must live (SH20); and
the data verifies through the destination path, which also takes a write. The
teardown's residue check proves the removal order.

**migr_full** pushes no bitmap: the applied set is empty, the whole device
matches the source, and no discard reaches any clone. **migr_bitmap** pushes
skip-bitmap chunks to the gated destination, with no revision (`dnagent.md`
DN15), over the same random source, so every difference is the bitmap's: the
applied set lists exactly the chunks (SH21, DN14), the must-copy head matches
the source, the skipped regions read zero, which provisioning guarantees
(`architecture.md`, [D15]), the enabling converge's own hydration sample
counts every skipped region, and the log holds one discard on the clone, at
the range the meta-region shift implies (`dnagent.md` SH22, SH23;
`architecture.md`, Bitmap push protocol). The discard checks read the agent's
JSON log by design: if its layout drifts, the reader is adjusted, never the
assertion.

**teardown** proves `architecture.md`, Teardown by sweep: what cannot go yet
is reported and finished later, never forgotten (`dnagent.md` DN6, DN7, DN16,
DN19, SH7). One stage yanks a migration's source export and at once drops the
destination side; its clone comes off before the connection it hydrates
through (`dnagent.md` L3), over a source already dead, so while hydration is
still in flight the removal outlasts a pass and the same revision is re-sent
until clean, only the leftover code tolerated. Nothing of the migration may
remain on either node, and a clean read-only `GetDnInfo` proves its allocation
and clone-metadata records went with their devices, an orphaned record being a
leftover itself (DN6's record step); that a record never goes before its
device is left to the unit tests. The other stage holds a side device open
from outside the agent and drops the side: the leftover reply names it, the
residue is exactly that device (DN6's layers), the read-only verdict agrees,
and the same revision, re-sent once the device is free, finishes.

**restart** proves persistence and an idempotent reconcile (`dnagent.md` SH1,
SH4 to SH6, DN2): with a connected side on one node and a gated destination
holding a chunk on the other, both agents restart over untouched kernel state
while the host keeps reading. The reloaded infos, revisions included, equal
the snapshots but for the status epochs (SH14), the zeroed counts with them
(`architecture.md`, [D13]); the chunk is still applied (SH21); unchanged
re-sends leave the new log free of any mutating command or block or configfs
write (SH16, SH17, DN5, DN9); and a revision below the one those re-sends
stored is refused as stale in a normal reply (SH8, SH9, DN4).

**What a pass means.** Exit status zero and PASS mean every case's assertions
held in a run that stops at the first failure: data checked against a pattern
file the agent never sees, removals proved by the node's own listings. The
observed windows are only logged, so a pass need not have caught a source
mid-cutover, a read mid-hydration or a clone removal blocking on its dead
source. The restart case prints, not asserts, whether each old agent stopped;
one that survives keeps its endpoint and answers the later steps, so that pass
is as good as that line.

## Teardown and cleanup

Cleanup runs unconditionally at the start of every run and at its end only on
success; a failing run leaves its debris and dumps the agents' logs, the dm,
nvmet and host state of both VMs, the last info of every migration or teardown
side, and the failing stage's trace id. A cleanup-only run does the scrub
alone. It takes every `dnv-agent` and every dnv dm device on the node, of
either role, so no two dnv runs may share a VM. Each step is best-effort; the
order is load-bearing. The agents go first: a graceful stop ends their
background tasks and joins their children (`dnagent.md` SH27), but an agent
killed outright can leave a zeroing child holding a side device a moment
longer, which the final retry sweep absorbs. Then any process a failed stage
left holding a device open goes, as nothing removes an open device; then every
suspended device is resumed, since a run killed in a cutover window leaves the
source's linears suspended, and a suspended device wedges its removal, the
namespace disable above it and any block scan. Host controllers go before
nvmet, which a live connection wedges; exports come down inside out; dm
devices go top-down, the clones before the migration connections and exports,
as a clone flushes to its source on removal, with a retry sweep last. Since a
node's migration source export backs its peer's clone, that protects the peer
only because both VMs are cleaned at once. Then go the port-level nvmet
objects; each disk's header block is zeroed, which leaves its volume-table
slots inert (`architecture.md`, [D13]); and last the loop devices are detached
and the work directory removed. The helper running all this lives outside it,
and every run ships it afresh.

The wipe is a separate flag that runs no case: it reads no dm kind, taking
dnv-named residue whatever its name decodes to, every dnv nvmet subsystem and
md array and, past dnv, every fabrics controller on the node, a reach that
makes it a flag and not a step; then it runs the ordinary cleanup. It repeats
while dnv dm devices remain, as udev can re-assemble an array from its
members, and fails if a dnv dm device or nvmet subsystem is left or the dm
listing did not answer, as reporting a residue is not success; md arrays are
only reported, a guest's own not being the suite's to fail on.

## Out of scope

Error paths beyond the stale probe, the provisioning error rows and the Write
Zeroes verdict among them (`dnagent.md` DN5, DN9); the cn role; fault
injection under a live stack, the teardown case removing a real remote
instead; performance; TLS and authentication; levels other than read-write and
no-migration (`dnagent.md` DN11); lock contention on one agent; the source
half of a cancelled migration; a zeroing batch cancelled mid-flight.
