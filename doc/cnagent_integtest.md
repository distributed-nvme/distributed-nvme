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
report. It also pins the md assembly rules the cross-leg half of failover
safety rests on, the refusal before redund builds its pool and the kick in
its late flip (`architecture.md`, [D16]). It proves the happy path; error
paths are out of scope but for the stale probe, the dead leg, the promotion
that outruns the sides' flip, md's refusal to start an array from one clean
leg, the clone's lost source, and the leftover, an accepted request reporting
residue (`cnagent.md` CN20). The cases
run in a fixed order, fail-fast — smoke, redund, teardown, thinbm, clone_xfer,
restart — each under pool ids of its own and each leaving nothing of its pools,
a teardown per case and per stage that also keeps the run inside a DN's data
area, which the cases together would overdraw.

Beside the suite, the Go tests of `agent/cnagent` run a real cn agent the way
the dn agent's run (`dnagent_integtest.md`, Goal and scope), driven by a fake
worker over the package's own fake OS client, which answers the agent's dm,
md, nvmet, nvme host, tmpfs and loop, thin-tool and local-store calls from
memory, while a probe-IO double over the same model (`fakeProbeIO`) answers
its leg probers' block IO, which leaves the OS client (`osclient.md`, Exported
raw helpers and the probe-IO carve-out). They prove a controller node of the
most cntlrs one holds, every one converged with every round clean; the widest
cntlr the fake converges inside a test's time bound, its request holding every
list at its limit but the groups of a slice; and orderings across many syncups
and rounds: a restart with the local store kept and with it lost (`cnagent.md`
CN2, CN21), a leg connect that hangs across rounds and past the pass budget
(`cnagent.md` CN10), a syncup that arrives during a converge (`dnagent.md`
SH30), and a sweep whose `dmsetup ls` names devices already gone (`cnagent.md`
CN21, CN30).

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
native multipath and the udev rule the md mask needs; and the free space and
punch-hole support of the work directory's filesystem and the free memory.
iptables is left to the partition stages, so a VM without it fails in the
stage that needs it rather than a run that might never reach one.

## Lab facts

The suite rests on the dn suite's lab facts (`dnagent_integtest.md`,
Assumptions and preflight checks) on dd, a synced write being also how one that
must fail reports it; on loop zero-out; on IO with no serving path; on DNR
under a port that keeps listening and on a port that stops listening; on a
namespace removed under a live connection, which clone_xfer's lost source
measures as the dn suite's does; and on the dm-clone table. This suite adds:

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
  member failed by hand before it is kicked as stale while the other,
  whose array state excludes it, starts the array; and a lone member whose
  superblock still counts both is refused, leaving no inactive array for
  `ensureGroup` to hold in error. redund asserts the refusal before it builds
  its pool and the kick in its late flip (Cases).
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
re-derive: the hydration poller (`common.ParseCloneStatus`), a group's hashed
md names (`CnMdDevName`) and a host NQN's host id. It reads the agent as the dn
driver does (`dnagent_integtest.md`, The driver: `dnagentctl`): its round
commands have the fresh mode, its hydration poller samples with rounds on one
stream, the first sample a fresh one, so its one-sample probe takes a fresh
sample, and its info-read commands are plain memory reads.

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
  listing prints a failing line, a status-less check reply fails, the
  agent-log readers fail their stage on a log that is missing or cannot be
  read, thinbm's and the finalize's checks that a converge ran no such
  command and the late flip's check that its promotion wrote no optimized
  group fail when that converge logged nothing at all, the long-dead stage
  counts a path absent only once the same read saw it live, an admission
  refusal needs nvmet's own refusal record behind a fresh mark, the park's
  open check and the dead-leg "no mdadm" carry positive controls, and in
  redund's md-refusal stage the checks that no array holds a leg and that no
  loop device is left fail on a read that fails and have the running array
  and the listed loops as their positive controls. Other negatives pass on a
  read that failed, and so prove less: the partition stages' waits for a path
  to leave live and the finalize's check for a source controller accept the
  "none" a failed listing returns; so do the no-path check after an admission
  refusal and the check that a subsystem lists no host, both backed by the
  refusal record; and the array counts redund's later stages and the teardown
  case read from /proc/mdstat, the standby's no-array check and the md part
  of the residue check take a failure for no array. A log that was read can
  still hold none of a stage's records, so restart's zero-mutation and mdadm
  checks, the late flip's "assembled nothing" and the gated clone's
  "no source connection" have no positive control.
* **Steady state.** Stages read the agent as the dn suite's stages do
  (`dnagent_integtest.md`, Conventions, Check rounds). One fresh `CheckCn`
  and one fresh `CheckCntlr` round per CN reply code zero, the revision that
  RPC stored and every row OK (`cnagent.md` CN24, CN30), the cntlr round
  first polling with rounds until no leg row reads pending, as a primary's
  legs do until their probers' first round (CN11). The teardown case runs no
  `CheckCntlr` round, since its cntlrs are gone; each of its stages ends with
  the fresh `CheckCn` round per CN, code zero and the base-state rows OK.
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
host-facing subsystem and a fresh `CheckCn` round replying clean (CN30).

**redund** first pins, with no agent and no NVMe-oF path, the refusal the
cross-leg half of failover safety rests on (`architecture.md`, [D16]): on one
VM, a clean two-leg array over two loop devices, built and stopped with the
agent's create and stop commands under a dnv name the md udev mask matches (Lab
facts), so that both superblocks still count both legs at one event count.
Started from one leg with the agent's assemble command (`cnagent.md` CN12's
assembly case), mdadm refuses, its own message saying it needs every member to
start the array, writes nothing to that leg's superblock and leaves no array
holding either leg; started from both legs, the array runs clean, both members
in sync, with nothing to rebuild. The command is the agent's: the promotion
below holds each assemble command the agent runs from both legs to the
arguments the same rule builds for that group, and the unit tests hold the
agent's start from one leg to the same rule. It then builds md-raid1 groups
across both DNs under a primary and a standby. The primary creates both with
`--assume-clean`: provisioning zeroed every leg's meta region, so none carries
a superblock, and no read reaches a data block the pool has not written
(`cnagent.md` CN12's create case; `architecture.md`, [D15]); the standby keeps
every leg connected and non-optimized, assembles no two-member array, and backs
its ns-dev with the td's dm-error, the namespace inaccessible (CN10, CN11,
CN16's standby rule); each CN's subsystem admits exactly the host's NQN (CN16).
The demote moves ANA before the park (CN9's ANA pre-step before its park
pre-step), stops the arrays and keeps the legs; the promote assembles both
(CN12's assembly case), each with the arguments the first stage's rule builds
for its own group, the data surviving. With the primary's legs into one DN cut
under a host write, every fresh check round reads both md rows OK until the
data row reports degraded and the dead legs' rows error (CN12, CN28, CN29).
Once the write completes on the surviving leg, the agent's log holds no mdadm
command under the stage's trace id, under which each read those rounds start
logs (`dnagent.md` SH29): that covers every such read that has ended by then,
but not the read the last fresh round leaves running, which the stage does not
wait for. The leg rows recover once the partition lifts, and the write reads
back. A fresh check round then names the member md failed, with the leftover
code, and the `SyncupCntlr` re-sent as the worker would puts it back: the
arrays read clean and the member is in sync with its failfast flag (CN12,
CN30). The same put-back of a member failed by hand,
on a whole array with no IO while the member is out, is one converge whose only
mdadm commands are the remove and then the add, and the sectors it writes to
the member are a small part of the array: that count, never a time, tells md's
re-add from its bitmap from the full copy mdadm makes when it cannot re-add.
Read-only fails writes through dm-flakey (CN16's read-only rule, CN19;
`architecture.md`, [D11]); the data array's member on the DN the partition cut
is then failed by hand, so the stage's last write reaches the other leg alone,
and no converge of the primary puts it back before the late flip demotes it.
The late flip promotes before any side flips, so the promotion assembles no
array and builds no raid0: both md rows read "no available leg", the ns-dev
stays parked with its row in error, and the promotion writes no optimized
group, so the host's path stays inaccessible and its IO would wait rather than
fail; the namespace row reads OK on the converge and alike in the observation
a fresh round then reports, made after the converge by a read or by an
attempt of the connect retry (CN12, CN16's ANA rule, CN28). The connect
retry, which the late members and the waiting namespace keep registered, then
assembles the arrays under its own trace ids, with no second `SyncupCntlr`,
puts the ns-dev on the raid0 and moves the namespace optimized, then stops; and
a block only the newer leg holds reads as that leg's, the stale member kicked
(CN10, CN12; `architecture.md`, [D16]).

**teardown** proves `architecture.md`, Teardown by sweep, on a CN: a teardown
finishes when what lies under it is gone, long gone, under load or unreachable,
and what cannot go is reported and retried, never forgotten (`cnagent.md` CN7,
CN20, CN21, CN30). Its request is what a pool drain sends while the sides
vanish: an empty pointer list, re-sent at the stored revision. It proves this
under the conditions the lab can stage: sides already gone; paths long dead,
live before and gone after the refused reconnect; IO in flight, where no write
succeeds after the sides go and one fails or stays queued on the host, as a
park killed at its timeout leaves it, while the parks, namespace removals and
pool commits complete (`cnagent.md` CN21: its park before every layer, its
nvmet layer L1 and its thin-pool layer L8) — in these three the DN drops finish
in one pass while the primary still holds every leg (`dnagent.md` DN6); a
partitioned DN, its sides dropped later under a CN already gone (`dnagent.md`
DN6); and a leg device held open from outside the agent, a leftover with no row
(CN29), reached again by a read made after the drop, which a fresh `CheckCn`
round reports (CN30), the residue exactly that device since every layer above
the legs' is gone (`cnagent.md` L10), cleared by the same request at the same
revision. Each condition is built under its own pool and namespace identity, so
residue names its stage, and is first proved up, both arrays running and the
host on both paths, since a teardown of a stack that never came up passes every
residue check; each ends with nothing of its pool or either CN left, base
states OK.

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
and hydration steps); the first hydration sample, a fresh one, already counts
the skipped half, and the last region the source wrote reads back right,
through read-through if need be. The startup reconcile alone rebuilds a wiped
destination CN, bitmaps first, the destination's own discard from offset zero
included when the wipe-time sample shows a region copied (CN2, CN18), its dead
host path replaced by device as the pools share the NQN, and hydration runs to
completion. The hydrated clone's source then loses its namespace behind both
agents' backs, the transfer's export and the destination CN's controller of it
staying: the clone's three rows and its destination namespace's two read one
status, with one epoch, on a converge, in the observations a fresh round and
then an info read report, each made after that converge by a read or by an
attempt of the connect retry, which a clone whose source is not ready keeps
registered, and on a second converge (`cnagent.md` CN10, CN18, CN28), the
source's row missing, which sets no health epoch, and the other four ok, while the namespace serves on, optimized, through the hydrated
dm-clone (CN16's clone rule) and the dm-clone's row still carries the status
line the gateway's hydration check reads. The state compared is measured as the
dn suite's lost source measures it (`dnagent_integtest.md`, Cases), and a
reading that moves before the comparison ends fails the stage. The finalize
takes the clone down in layer order, the clone, its wrapper, then its source
connection (`cnagent.md` L3 to L5), leftover while the source's disconnect runs
or clean if its controller was already gone. The half the source wrote reads
back intact through the destination, which then takes a write, while the half
it never wrote stays unmapped and zero there, the destination starting empty
(`architecture.md`, [D3]); and at teardown both CNs pass smoke's per-CN check.

**restart** proves persistence and an idempotent reconcile (`cnagent.md` CN2;
`dnagent.md` SH1, SH4 to SH6): both cn agents restart over untouched kernel
state as the host reads on; every snapshot, on both sides of the restart, is a
fresh round's reply, and the reloaded infos, revisions included, equal the
snapshots but for the status epochs (`dnagent.md` SH14) and the replies'
stamps, which differ by nature; mdadm runs nothing but its superblock examine
(CN12); equal-revision re-sends leave the new logs free of any mutation, an
activation sweep's included (`dnagent.md` SH16; `cnagent.md` CN14); and a
`SyncupCn` below the revision those re-sends stored is refused as stale in a
normal reply (`dnagent.md` SH8, SH9; CN4).

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
id. A cleanup-only run does the scrub alone. It takes every `dnv-agent` and
every dnv dm device, md array and nvmet subsystem on the node, of either
role, which is why one dnv suite runs at a
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
dnv-named one is stopped, but for an array over the loop devices of redund's
md-refusal stage, which is found by what holds those devices, whatever its
name or state, and stopped before they are detached.

The wipe is a separate flag that runs no case. Kind-blind, it takes every
dnv-named dm device, every dnv nvmet subsystem and md array, an array named
also by its members' dm names, and every fabrics controller on the node, a
reach that makes it a flag; then the ordinary cleanup runs. It repeats while
dnv dm devices remain, as udev can re-assemble an array from its members, and
any dm or nvmet residue fails it, since reporting it is not success.

## Out of scope

Error paths beyond the six above; QoS, not enforced (`cnagent.md` CN6);
`GrowSlice`; spare legs; a leg gaining or losing a second side, which the dn
suite's migrations prove, and the path rows of a leg with two sides
(`cnagent.md` CN28), which the unit tests cover; levels but read-write,
read-only and the no-clone gate; more than one slice; several namespaces per
subsystem; snapshots of snapshots; `UpdateNamespaceDev`; a host-facing
namespace or subsystem dropped short of a cntlr teardown, leaving the park
before its removal (`cnagent.md` CN21's park before every layer and its nvmet
layer L1) to the unit tests;
cntlid-slot exhaustion; the cdc; TLS and authentication; performance; faults
beyond the partition, the pin, the writer and the lost source; provisioning deferral
(`cnagent.md` CN9); an arena with no free run for a clone, whose
provisioning rows and slow retry the unit tests cover (CN18); a torn
snapshot, which needs two slices, whose mark the unit tests cover (CN14);
the lingering-clone hold's scope per thin device and the replacement of an
arena file of another size, both left to the unit tests (CN18, CN5); the
other states of the clone's shared row rule (CN18); and the lock entry a
refused request must not leave (`dnagent.md`, Concurrency — `locks.go`).
