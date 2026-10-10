# cnagent_integtest.md — integration test plan for `dnv-agent cn`

This document owns the intent of the cn agent's integration suite,
`integtest/cnagent_test.sh`, and of its driver `cnagentctl`: what the suite
proves, the conventions its assertions rest on, what a pass means and how it
cleans up. It leans on `cnagent.md` for the contract under test, `dnagent.md`
for the dn agents behind the sides, `architecture.md` for the procedures and
decisions it cites, and `dnagent_integtest.md`, whose harness it shares.

## Goal and scope

The suite proves that a real `dnv-agent cn` — nvme-tcp legs, md-raid1, dm-thin,
dm-clone, dm-flakey, nvmet configfs — behaves as `cnagent.md` specifies on two
lab VMs driven over gRPC from the developer machine, with real `dnv-agent dn`
instances behind the sides, the dn role having its own suite. It exercises
every `ControllerNodeAgent` RPC and reads the kernel state beside the agent's
report. It proves the happy path; error paths are out of scope but for the
stale probe, the dead leg, the promotion that outruns the sides' flip, and the
leftover, an accepted request reporting residue (`cnagent.md` CN20). The cases
run in a fixed order, fail-fast — smoke, redund, teardown, thinbm, clone_xfer,
restart — each under pool ids of its own and each leaving nothing of its pools,
a teardown per case and per stage that also keeps the run inside a DN's data
area, which the cases together would overdraw.

## Topology

The developer machine ships one agent binary over ssh and drives four agents
over gRPC, never touching the data path. Each VM runs both roles, the only way
two VMs yield the two disk nodes and two controller nodes raid1 and failover
need, each dn agent on a loop device. A VM's pair co-owns one nvmet port with
identical transport values, so whichever converges second writes nothing
(`architecture.md`, Components: invocation reference; `dnagent.md` CM2, SH19),
and a check asserts a subsystem's membership on the port, never that it holds
exactly a case's. The host is emulated on the VMs, under a fixed host NQN with
its derived host id (`common.NvmeHostId`; `dnagent.md` SH20). Agents run as
root under one process name, so the suite stops a role by its command line, and
a case fails a stop that leaves an agent alive to race its successor.

## Assumptions and preflight checks

Both VMs run one lab image with nvme native multipath, md raid1, dm-thin,
dm-clone and dm-flakey, and accept passwordless ssh and sudo. The preflight
installs nothing and fails fast, its per-VM half after the start-of-run
cleanup, since free agent ports mean something only once a crashed run's agents
are gone. It loads the kernel modules and mounts configfs, neither of which the
agent does, then checks each module loaded or built in; the tools, no LVM one
among them (`architecture.md`, [D13], [D14]); nvmet configfs, md support,
native multipath and the udev rule the md mask needs; the free space and
punch-hole support of the work directory's filesystem and the free memory; and,
once each loop device exists, its Write Zeroes (`dnagent.md` DN5). iptables is
left to the partition stages, so a VM without it fails in the stage that
needs it rather than a run that might never reach one.

## Lab facts

The suite rests on the dn suite's lab facts (`dnagent_integtest.md`,
Assumptions and preflight checks) on dd, a synced write being also how one that
must fail reports it; on loop zero-out; on IO with no serving path; on DNR
under a port that keeps listening and on a port that stops listening; and on
the dm-clone table. This suite adds:

* Each VM's port keeps listening while the other role's subsystems sit on it,
  so a side removed under a CN draws the refusal that deletes the CN's leg
  controllers, though a leg's loss timeout never gives up (`dnagent.md` SH20),
  and the host's controller to a wiped CN must be replaced.
* When a subsystem on the port does not admit a host, nvmet refuses the host's
  Connect to it and leaves a kernel-log record naming that host and that
  subsystem, a record no other connect failure leaves, so the suite reads an
  admission refusal from that record (Conventions).
* nvmet checks admission only when a host connects, so removing a host's link
  from a subsystem leaves the host's controller to it live, which smoke
  asserts after revoking the host.
* The suite installs the md udev mask of `architecture.md`, Components:
  invocation reference, for the run and removes it at cleanup, the VMs being
  shared; the preflight checks that the stock incremental-assembly rule honours
  the systemd-ready property the mask depends on.
* mdadm gates a degraded assembly from a device list by the survivor's recorded
  array state (`cnagent.md` CN12), and the late flip relies on both halves: the
  member md failed in the dead-leg stage is kicked as stale while the other,
  whose array state excludes it, starts the array; and a lone member whose
  superblock still counts both is refused, leaving no inactive array for
  `ensureGroup` to hold in error. Re-verify both when mdadm changes
  (`architecture.md`, [D16]).
* The dead-leg stage cuts each array's first member, the one mdadm's detail
  read would block on, under a host write, since md learns of a dead member
  only from an IO to it (`cnagent.md` CN12, CN29); a failfast member write
  fails at the controller's keep-alive expiry.
* dm-flakey counts the feature name among its feature arguments and prints them
  back in a kernel-dependent order, so the read-only check matches the target
  and the error-writes feature apart, as `nsDevTableMatches` does.
* A fresh dm-thin pool allocates data blocks in order from the start, so the
  data-leg bitmap check is positional; the contract is the count of zero bits.
* The arena's tmpfs always takes the clone-metadata hole punch, which no
  preflight probes (`cnagent.md` CN18); arena and registry are volatile
  together (`architecture.md`, [D14]).

## The driver: `cnagentctl`

`cnagentctl` exists because the agent serves plaintext gRPC without reflection:
one subcommand per `ControllerNodeAgent` RPC, replies printed as protojson and
bitmaps as hex; a call fails on a gRPC error or a code other than the expected
one, zero unless the stage names another. It sends the stage's trace id as
metadata and in `Check*` requests (`grpc.md`, Drivers and fakes), takes a
cntlr's request whole from a file, and holds what the script must not
re-derive: the hydration poller (`agent.ParseCloneStatus`), a group's hashed md
names (`CnMdDevName`) and a host NQN's host id.

## Conventions

* **Revisions.** One monotonic counter per node, which on a CN feeds both
  `SyncupCn` and `SyncupCntlr`, though the worker labels them with `CnRev` and
  with the pool's `SpRev` (`dnv-worker.md` RW13, RW14); each RPC is gated
  against what it last stored, so the suite also tracks the last `SyncupCn`
  revision, the one a `CheckCn` round echoes and the stale probe undercuts.
  Re-sends at an equal revision are deliberate re-applies (`dnagent.md` SH8,
  SH16).
* **Sides first.** The suite plays the provisioned flip, a side's per-CN rows
  reading provisioning until then (`dnagent.md` DN9, DN10), and converges sides
  before cntlrs, so no leg is provisioning-deferred (`cnagent.md` CN9) and legs
  are optimized when a primary assembles. Each side asks to zero the length a
  worker derives from its group (`architecture.md`, Side provisioning
  protocol), which a fresh thin pool and a fresh array rely on. A failover
  demotes, flips the sides, waits for the new primary's paths to read
  optimized, then promotes (`architecture.md`, Failover), host IO quiesced
  across the window with no serving path (Lab facts); only the late flip
  promotes first.
* **Negatives are exact.** Provisioning and pending are healthy statuses, so an
  error row compares against error, never "not OK", and a suppressed one also
  reads the level's details. A failed read taken for an absence passes every
  negative on it (`cnagent.md` CN21): a residue check's failed dm or nvmet
  listing prints a failing line, a status-less check reply fails, the long-dead
  stage counts a path absent only once the same read saw it live, an admission
  refusal needs nvmet's own refusal record behind a fresh mark, and the park's
  open check and the dead-leg "no mdadm" carry positive controls. Other
  negatives pass on a read that failed, and so prove less: the partition
  stages' waits for a path to leave live and the finalize's check for a source
  controller accept the "none" a failed listing returns; so do the no-path
  check after an admission refusal and the check that a subsystem lists no
  host, both backed by the refusal record; the mutation, event and command
  readers print nothing for a log they cannot read, so restart's
  zero-mutation and mdadm checks and the late flip's "assembled nothing" have
  no positive control; and the reads of
  /proc/mdstat, the standby's no-array check and the md part of the residue
  check take a failure for no array.
* **Steady state.** One `CheckCn` and one `CheckCntlr` round per CN reply code
  zero, the revision that RPC stored and every row OK (`cnagent.md` CN24,
  CN30), the cntlr round first waiting out a fresh primary's pending legs
  (CN11). The teardown case runs none: it pins the removal.
* **Mutation-free.** The mutation reader lists every mutating command and every
  configfs or block write ("os write block" by name) in a cn agent log, whole
  or for one trace; the probers' "probe …" records fall outside it by
  construction (`osclient.md`, Exported raw helpers and the probe-IO
  carve-out). Records no stage RPC caused carry trace ids of their own
  (`cnagent.md` CN2), so the recovery and restart checks read a fresh log.

## Cases

Setup proves each dn agent by its exact data area and a baseline `SyncupDn`
(`dnagent.md` DN3, DN5), each cn agent by a `GetCnSize` echoing its capacity
(`cnagent.md` CN3, CN-CM1) and a baseline `SyncupCn` with base rows OK (CN5).

**smoke** proves the plumbing: the pointer before the `SyncupCntlr` that builds
a `RedundNone` primary stack (`cnagent.md` CN8 to CN16), all rows OK but the
legs, pending until their probers' first round (CN11); admission on real
nvmet (CN16): built with no allowed host the subsystem admits none, probes
clean and refuses the emulated host's connect; granted, it admits exactly that
host; revoked again, the connected host keeps its path and its next connect is
refused; host IO; clean check rounds, the namespace identity included
(`dnagent.md` SH17); the raw thin-pool status line the auto-grow parses (CN28);
and a teardown by an empty pointer list, re-sent while it replies leftover
(CN7, CN21), leaving the CN no dm device, clone-metadata wrapper (CN18) or
host-facing subsystem and a clean `GetCnInfo` (CN30).

**redund** builds md-raid1 groups across both DNs under a primary and a
standby. The primary creates both with `--assume-clean`: provisioning zeroed
every leg's meta region, so none carries a superblock, and no read reaches a
data block the pool has not written (`cnagent.md` CN12's create case;
`architecture.md`, [D15]); the standby keeps every leg connected and
non-optimized, assembles no two-member array, and backs its ns-dev with the
td's dm-error, the namespace inaccessible (CN10, CN11, CN16's standby rule);
each CN's subsystem admits exactly the host's NQN (CN16). The demote moves
ANA before the park (CN9's ANA pre-step before its park pre-step), stops the
arrays and keeps the legs; the promote assembles both (CN12's assembly case),
the data surviving. With the primary's legs into one
DN cut under a host write, every check round reads both md rows OK until the
data row reports degraded and the dead legs' rows error, and no mdadm runs
(CN12, CN28, CN29); the write completes on the surviving leg and reads back,
the leg rows recover once the partition lifts, and the member md failed
staying failed is the known limit the stage tolerates (`cnagent.md`, Known
limits). Read-only fails writes through dm-flakey (CN16's read-only rule,
CN19; `architecture.md`, [D11]). The late flip promotes before any side flips:
both md rows read "no available leg" until the late members' connect retry
assembles them under its own trace ids, with no second `SyncupCntlr`, then
stops (CN10, CN12; `architecture.md`, [D16]).

**teardown** proves `architecture.md`, Teardown by sweep, on a CN: a teardown
finishes when what lies under it is gone, long gone, under load or unreachable,
and what cannot go is reported and retried, never forgotten (`cnagent.md` CN7,
CN20, CN21, CN30). Its request is what a pool drain sends while the sides
vanish: an empty pointer list, re-sent at the stored revision. It proves this
under the conditions the lab can stage: sides already gone; paths long
dead, live before and gone after the refused reconnect; IO in flight, where no
write succeeds after the sides go and one fails or stays queued on the host,
as a park killed at its timeout leaves it, while the parks, namespace removals
and pool commits complete (`cnagent.md` CN21: its park before every layer, its
nvmet layer L1 and its thin-pool layer L8) — in these three the DN drops
finish in one pass while the primary still holds every leg (`dnagent.md`
DN6); a partitioned DN, its sides dropped later under a CN already gone
(`dnagent.md` DN6); and a leg device held open from outside the agent, a
leftover with no row (CN29), recomputed by a read-only `GetCnInfo` (CN30), the
residue exactly that device since every layer above the legs' is gone
(`cnagent.md` L10), cleared by the same request at the same revision. Each
condition is built under its own pool and namespace identity, so residue names
its stage, and is first proved up, both arrays running and the host on both
paths, since a teardown of a stack that
never came up passes every residue check; each ends with nothing of its pool
or either CN left, base states OK.

**thinbm** proves the bitmap reads exact over known writes, a meta leg all zero
(`cnagent.md` CN25 to CN27; `architecture.md`, raid0 bitmap math), and the
snapshot: sent with the only `td_list` the gateway can produce, origin created
and snapshot not (`architecture.md`, Thin devices), its message sits inside the
origin's quiesce (`cnagent.md` CN14) and it holds the origin's blocks, no later
write. A teardown and a converge with every td created then prove on a real
dm-thin pool that a bare `dmsetup create` attaches the existing ids, both
mappings intact, with no suspend and no create, snapshot or delete message, the
activation sweep sending its reserve and release once each (CN14, CN21).

**clone_xfer** proves `architecture.md`, Transfer + clone = cross-SP live
migration, and Clone crash recovery, between pools on different nodes exporting
one NQN and namespace identity from disjoint cntlid slots. The stored-suspended
destination and the transfer's origin are parked, live on the td's dm-error
with the origin's ANA moved first, so an open fails at once and nothing is
dm-suspended (`cnagent.md` CN9's ANA pre-step before its park pre-step, CN16's
parked rule; `architecture.md`, Namespace suspend semantics, [D12]); the
transfer admits only the destination CN (CN17) and refuses the emulated host,
which its list does not name. Gated at the no-clone level (CN19), two chunks of
one source slice push race-free and prove the pair addressing (CN20, CN22). At
read-write one converge hole-punches an arena range for the wrapper (CN18's
metadata-slot step; `architecture.md`, [D14]), builds the dm-clone with exactly
the no-hydration and no-discard-passdown pair, and lands the chunk as one
discard at its skip range before hydration is enabled (CN18's dm-clone, bitmap
and hydration steps); the first hydration sample already counts the skipped
half, and the last region the source wrote reads back right, through
read-through if need be. The startup reconcile alone rebuilds a wiped
destination CN, bitmaps first, the destination's own discard from offset zero
included when the wipe-time sample shows a region copied (CN2, CN18), its dead
host path replaced by device as the pools share the NQN, and hydration runs to
completion. The finalize takes the clone down in layer order, the clone, its
wrapper, then its source connection (`cnagent.md` L3 to L5), leftover while
the source's disconnect runs or clean if its controller was already gone. The
half the source wrote reads back intact through the destination, which then
takes a write, while the half it never wrote stays unmapped and zero there, the
destination starting empty (`architecture.md`, [D3]); and at teardown both CNs
pass smoke's per-CN check.

**restart** proves persistence and an idempotent reconcile (`cnagent.md` CN2;
`dnagent.md` SH1, SH4 to SH6): both cn agents restart over untouched kernel
state as the host reads on; the reloaded infos, revisions included, equal the
snapshots but for the status epochs (`dnagent.md` SH14); mdadm runs nothing but
its superblock examine (CN12); equal-revision re-sends leave the new logs free
of any mutation, an activation sweep's included (`dnagent.md` SH16;
`cnagent.md` CN14); and a `SyncupCn` below the revision those re-sends stored
is refused as stale in a normal reply (`dnagent.md` SH8, SH9; CN4).

**What a pass means.** Exit status zero and PASS mean every assertion held in a
run that stops at the first failure: data checked against patterns the agents
never see, removals proved by the nodes' own listings, every stop a case makes
confirmed; the races a loop device usually wins — the provisioning and
read-through windows, hydration at the wipe — are logged, the last deciding
just whether the recovery's discard from offset zero is checked.

## Teardown and cleanup

Cleanup runs unconditionally at the start of every run and at its end only on
success; a failing run leaves its debris and dumps the agents' logs, both VMs'
dm, md, nvmet and host state, each read bounded, and the failing stage's trace
id. It takes every `dnv-agent` and every dnv dm device, md array and nvmet
subsystem on the node, of either role, which is why one dnv suite runs at a
time in the lab (`layout.md`, Directory tree) — but for two blind spots the
wipe exists for: its dm steps see only names with a role-lettered kind, and its
md stop skips an inactive array over unreadable members, which neither udev nor
mdadm names. Each step is best-effort, and the order is load-bearing on one
principle, a holder goes before what it holds, across both VMs at once: the
agents, their retries and probers with them, before the devices; the fault
injections unconditionally, as each outlives its stage and would pass for
another fault next run; host connections before the subsystems they hold,
exports inside out, cn devices top-down and the dn objects in the dn suite's
order (`dnagent_integtest.md`, Teardown and cleanup); a dm-clone while its
source is still connected (`cnagent.md` L3), which makes the cleanup two phases
across both VMs, every clone of either VM gone before any transfer, as a clone
flushes through a source the other VM exports; a clone-metadata wrapper after
its clone, as one left holds the arena's loop device; each disk's header block
zeroed (`architecture.md`, [D13]) and the udev mask removed last. Every
suspended dnv device is resumed before anything over it is removed, as one
wedges its removal, the namespace disable above it and any block scan
(`architecture.md`, [D12]). md arrays are named through udev, else by mdadm's
export, never by its scan, which prints no name on these guests; only a
dnv-named one is stopped.

The wipe is a separate flag that runs no case. Kind-blind, it takes every
dnv-named dm device, every dnv nvmet subsystem and md array, an array named
also by its members' dm names, and every fabrics controller on the node, a
reach that makes it a flag; then the ordinary cleanup runs. It repeats while
dnv dm devices remain, as udev can re-assemble an array from its members, and
any dm or nvmet residue fails it, since reporting it is not success.

## Out of scope

Error paths beyond the four above; QoS, not enforced (`cnagent.md` CN6);
`GrowSlice`; spare legs; a leg gaining or losing a second side, which the dn
suite's migrations prove; levels but read-write, read-only and the no-clone
gate; more than one slice; several namespaces per subsystem; snapshots of
snapshots; `UpdateNamespaceDev`; a host-facing namespace or subsystem dropped
short of a cntlr teardown, leaving the park before its removal (`cnagent.md`
CN21's park before every layer and its nvmet layer L1) to the unit tests;
cntlid-slot exhaustion; the cdc; TLS and authentication; performance; faults
beyond the partition, the pin and the writer; provisioning deferral
(`cnagent.md` CN9); and arena exhaustion (CN18).
