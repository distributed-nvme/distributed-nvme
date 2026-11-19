# cnagent_integtest.md — integration test plan for `dnv-agent cn`

This document owns the intent of the cn agent's integration suite,
`integtest/cnagent_test.sh`, and of its driver `cnagentctl`: what the suite
proves, the conventions its assertions rest on, what a pass means and how it
cleans up. It leans on `cnagent.md` for the contract under test, `dnagent.md`
for the dn agents behind the sides, `architecture.md` for the procedures and
decisions it cites, and `dnagent_integtest.md`, whose harness it shares.

## Goal and scope

The suite proves that a real `dnv-agent cn` — nvme-tcp legs, md-raid1,
dm-thin, dm-clone, dm-flakey, nvmet configfs — behaves as `cnagent.md`
specifies on two lab VMs driven over gRPC from the developer machine, with
real `dnv-agent dn` instances behind the sides, the dn role having its own
suite. It exercises every `ControllerNodeAgent` RPC but `GetCnInfo`, whose
reading `CheckCn` answers the same way (`cnagent.md` CN23, CN24) and which
the unit tests and the e2e suite's `InspectControllerNode` call, and reads
the kernel state beside the agent's report. It also pins two of the md
assembly rules the cross-leg half of failover safety rests on, the refusal
before redund builds its pool and the kick in its late flip
(`architecture.md`, [D16]); another, a start from equal superblocks copying
the member in the lowest slot, which the worker's fence round rests on, no
case pins: only `integtest/md_fence_order.sh` does (`layout.md`, Directory
tree). It proves the happy path; error paths are out of scope but for the
stale probe, the dead leg, a host's md, LVM and bcache signatures at an
array's start and a newer md superblock at its legs' end, the promotion that
outruns the sides' flip, md's refusal to start an array from one clean leg,
md's check over sectors that fail reads, and over sectors that fail writes
too, a repair's bad leg that does not serve, the leg that keeps the data
dying while md copies onto a repair's new leg, where a forced start refuses
the half-copied leg and starts from the leg that died, a take-back refused
for a retired leg of another identity, a forced start that drops the named
leg's bad-block list and one refused for a named leg older than a member it
would zero, then forced, a broken array whose legs come back assembled again
with no mark, a group whose array has run whose legs all read blank, the
clone's source lost and given back, a new volume over a thin id that holds
data, a snapshot torn by a primary stopped between its slices' steps, a disk
node that dies under a host's writes, a disk node that leaves new
connections unanswered, a node whose mdadm does not keep its degraded gate,
an md array stalled under the gather, and the leftover, an accepted request
reporting residue (`cnagent.md` CN20). The cases run in a fixed order,
fail-fast — nodecheck, smoke, redund, repair, teardown, thinbm, torn,
discard, clone_xfer, restart, pause — nodecheck building no pool and each
other case under pool ids of its own, and each leaving nothing of its pools,
a teardown per case and per stage that also keeps the run inside a DN's data
area, which the cases together would overdraw.

Beside the suite, the Go tests of `agent/cnagent` run a real cn agent the way
the dn agent's run (`dnagent_integtest.md`, Goal and scope), driven by a fake
worker over the package's own fake OS client, which answers the agent's dm,
md, nvmet, nvme host, thin-tool and local-store calls from memory, while a probe-IO double over the same model (`fakeProbeIO`) answers
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
installs nothing on the VMs — on the machine that runs the suite it builds
`gojq` into `integtest/bin` when that machine has no `jq` (`layout.md`,
Directory tree) — and fails fast, its per-VM half after the start-of-run
cleanup, since free agent ports mean something only once a crashed run's
agents are gone. It loads the kernel modules and mounts configfs, neither of
which the agent does, then checks each module loaded or built in; the tools,
lvm2's and bcache-tools' only for redund's foreign stage, which makes a
host's signatures with them, as the agents run no LVM (`architecture.md`,
[D13], [D14]); nvmet configfs, md support, native multipath, the stock udev
rules the rules of `deploy/udev/` sort against, the stock assembly rule's
honouring of the systemd-ready property the md mask sets, and udevadm's
acceptance of the rules of `deploy/udev/`; and the free space and punch-hole
support of the work directory's filesystem and the free memory. iptables and
tc are left to the stages that use them, the partition stages, redund's dial
and slow leg stages, pause's cut and clone_xfer's slowed copy, so a VM
without one fails in the stage that needs it rather than a run that might
never reach one.

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
* A namespace's "device_path" that was never written reads back "(null)",
  not an empty string, which smoke asserts on a namespace it makes by hand
  (`cnagent.md` CN21).
* nvmet checks admission only when a host connects (`architecture.md`, Primary
  cntlr), so removing a host's link from a subsystem leaves the host's
  controller to it live, which smoke asserts after revoking the host.
* The suite installs the udev rules of `deploy/udev/` (`architecture.md`,
  Components: invocation reference) for the run, from the texts the VM helper
  carries, checks that each file holds its text, and removes them at cleanup,
  the VMs being shared; the preflight checks that the stock incremental-assembly
  rule honours the systemd-ready property the md mask sets.
* mdadm takes the 1.2 superblock format (`--metadata=1.2`) in its create,
  assemble and misc modes and refuses it in its manage mode, and never as its
  first option (`cnagent.md` CN12, The superblock format). Its bitmap
  examine (`--examine-bitmap`) aborts with a heap error, exit status 134,
  and its bad-block examine (`--examine-badblocks`) with a segmentation
  fault, exit status 139, when they name the format; both read a member
  without it. So the suite's bitmap and bad-block reads name no format,
  take a failed run as an error, and run only after the foreign
  superblocks are zeroed. Without it, an
  examine and an assembly read, of the superblocks on a member, the one whose
  array was created last: a 1.0 or 0.90 superblock of an array made after
  dnv's, at the member's end, makes the plain examine show that array and the
  plain assembly refuse the member for its name ("has wrong name."), while
  the examine and the assembly that name the 1.2 format read dnv's and start
  the array. The examine that names the format answers a device with no 1.2
  superblock with exit status 0 and nothing on its output. A plain add of
  such a member re-adds it and keeps the other superblock; a create over a
  member that carries only a 1.0 superblock removes it. Seen with mdadm 4.5
  on loop devices, kernel 7.0.0-42.
* A write through a leg device reaches the leg past the page cache of the
  NVMe device below it, and that cache keeps what an earlier read of the
  NVMe device got while the leg device holds it open. udev's blkid probe
  reads through that cache: after the foreign stage wrote the leg's end
  through the leg device, the probe of the NVMe device found no signature
  at all, while mdadm, which reads past the cache, found both superblocks.
  Dropping the cache (`blockdev --flushbufs`) makes the probe read the
  disk. Seen with util-linux 2.41.3, kernel 7.0.0-42.
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
  in flight on the path fails at the teardown the controller's keep-alive
  expiry triggers, a quarter keep-alive timeout later on the lab's kernel,
  and one that reaches the leg's multipath head after the expiry waits there
  until the fast IO fail timeout (`architecture.md`, Known limits), as the
  pause case measures.
* nvme-cli does not pass a fast IO fail timeout of zero to the kernel: the
  controller it connects keeps the kernel's default, which never fails IO.
* dm-flakey counts the feature name among its feature arguments and prints them
  back in a kernel-dependent order, so the read-only check matches the target
  and the error-writes feature apart, as `nsDevTableMatches` does.
* A fresh dm-thin pool allocates data blocks in order from the start, so the
  data-leg bitmap check is positional; the contract is the count of zero bits.
* A dm device counts the sectors written to it from its creation, so the
  write count of a destination raid0 a rebuild has just created is what a
  rebuilt dm-clone copied into it, as long as no host writes there. A
  region's copy writes one block, and a dm-clone counts the region copied
  once that write has ended, so while one region at most is in flight and no
  copy fails, a destination that only the copy writes into holds one whole
  block for each region copied, beside at most part of the one in flight. A
  copy that fails can leave part of its block written, the region uncounted:
  dm-clone copies through dm-kcopyd, which splits a region larger than its
  sub-job into pieces and writes each piece whose own read succeeded,
  whatever the other pieces' reads do, so a source that goes between the
  reads of one region leaves part of a block that the region's next copy
  writes again whole. A copy whose reads all fail writes nothing.
* On a node without the dm rule of `deploy/udev/`, a new dm-clone is read
  through its source at once, as udev probes the device, and a discard on it
  would wait for those reads, so the apply sends no discard while they are
  in flight (`architecture.md`, Bitmap push protocol, The apply pauses the
  writers): over a slowed source the chunk would not land. The suite
  installs that rule, which keeps udev's probe off every dnv dm device
  (`architecture.md`, Components: invocation reference), and whether a new
  dm-clone is still read through its source with the rule in place is not
  measured, so clone_xfer slows its copy on the write side, which holds
  either way: a VM's cn agent reaches the sides of the dn agent beside it
  over the loopback device, and a token bucket there, kept to the nvme-tcp
  port, slows what its legs send those sides, the copy's writes into the
  destination among them, and none of the source's reads.
* md holds a member added to an array whose every place holds a member in
  sync without a place, its "slot" none, and copies nothing onto it:
  repair's abort stage gives the converge a new leg held without a place
  that way, and asserts the slot rather than assume it. Its add stage rests
  on the dead-leg fact above: md fails the bad leg, whose path no longer
  serves the primary, only on an IO to it, so the stage takes the bad leg
  in its place or out of it.
* A source that fails every read holds a dm-clone's copy at a fixed count,
  its metadata read-write: the region in flight ends, and no other is copied
  while the source fails. A namespace removed from the source's export takes
  the destination's multipath head with it, though the dm-clone holds it
  open, so a namespace that comes back does so under a new device number,
  which the dm-clone does not read until it is reloaded onto it. clone_xfer
  measures both rather than assume them.
* md's version-1.2 superblock sits 4096 bytes into a member, in the
  kernel's version-1.2 superblock layout, with a checksum the agent's own
  read checks; a member md was copying onto carries a copy position (the
  feature map's value 2, bit 1), and a bad-block list that is not empty
  sets the feature map's value 8, bit 3, the list 16 sectors after the
  superblock on the lab's arrays. A
  running array's "md/uuid" spells the identity the superblock carries,
  dashed. mdadm's examine answers a read of the member that fails as it
  answers a member with no superblock (`cnagent.md` CN12, The superblock
  read). A zero of a superblock leaves md's write-intent bitmap beside it.
* A start with the run and force flags under a new identity keeps a member's
  bad-block list; a stop right after it and a second start with the list
  dropped (`--update=force-no-bbl`) start the array from the member under
  the new identity with no list, and the drop takes the member's bad-block
  log away for good, so md fails it on an error instead of recording one;
  mdadm's no-degraded flag works only with a scan, so no start can drop the
  list without starting the array. Forty such stops right after a start
  never met udev holding the array (`cnagent.md` CN12, the forced
  start).
* When the leg md copies from dies, md can retry the copy onto the
  half-copied member for as long as that member stays in the array, and
  writes its superblock to the dead member as it does: the array reads
  broken only while it is otherwise clean, and write-pending during each
  such write, active after one, so its state can keep moving between broken
  and those running states until the array is stopped. The half-copied
  member's recovery offset can read the largest sector number, once md has
  run the copy to its end over the regions it could not read. A broken
  array holds its members, and mdadm refuses to zero a member any array
  holds; nothing holding it open, it stops. Stopped
  while the leg it copied from is still dead, the half-copied member keeps
  its recovery offset, and md starts no array from it alone. Once that leg
  is back before the stop, md can instead record each region it could not
  read as a bad block on the half-copied member and mark it in sync, its
  superblock then showing no recovery offset (`cnagent.md`, Known limits),
  and the stop can leave the leg that came back with a bad-block log: one
  of empty entries the kernel refuses to import, or one of real entries,
  the regions md could not read, which an assembly takes in; the forced
  start drops either (`cnagent.md` CN12, the forced start). Writing "frozen" to the array's "md/sync_action" stops those retries,
  and the array then reads broken until it is stopped, the half-copied
  member keeping its copy position. repair's forced-start stages therefore make
  the leg die by an error table under the disk node's export of it to the
  primary, freeze md's copy once the array reads broken, wait for broken to
  hold, and mark while the leg is still dead,
  so that the forced start stops the array before the leg is back.
* A broken RAID1 array whose members come back stays broken, and a read
  of it gets an IO error; a stop writes nothing to the members, and a
  plain assemble of them, with no run and no force flag, starts the array
  again. When both members died at once under a write, both superblocks
  still record a clean array of both at one change count, and the
  assemble starts it from both, in sync. When one died, writes went on,
  and then the other died, the last one's superblock records itself alone
  at a higher change count, and the assemble starts the array from it
  alone and leaves the older one out, for the member reconciliation's add
  (`cnagent.md` CN12, The re-assembly). Seen on loop devices with dnv's
  create options, kernel 7.0.0-42, mdadm 4.5.
* A few seconds after md's copy onto a member ends, md clears the bits of
  its write-intent bitmap in a write to every member, so a leg cut then
  gets an IO and md fails its member by itself, seen about 8 s after such a
  cut on kernel 7.0.0-42: a stage that needs an array with no IO
  waits until the bitmap on every member reads no dirty chunk. Once it does,
  and the array reads clean, only the agent's probe writes to the legs, and
  none goes through md.
* An assembly with the run and force flags from one member whose superblock
  counts two working members starts the array degraded; a zero that names the
  1.2 format fails on a device with no 1.2 superblock, with exit status 2, and
  on a member that carries a 1.2 superblock and another format's takes the 1.2
  one alone; and a member zeroed and then added is added fresh and copied
  whole.
* An assembly that gives the array a new identity makes a member of the old
  history added later a fresh member, copied whole; without it, mdadm
  re-adds a member whose events count is below the array's from the bitmap,
  so one whose history diverged leaves the array serving two histories, and
  one whose count is above is re-added and copied over, its newer data lost
  with no error. An assembly handed members of both identities refuses, in
  every order (`cnagent.md` CN12).
* A read of /proc/mdstat waits behind a stuck md member in one of the two
  shapes tried. With the member suspended under a write, md's thread waits
  in the superblock update that marks the array active, which holds no
  lock the read takes: /proc/mdstat, md's sysfs attributes and `mdadm
  --detail` all answer at once. In the other shape, an array written with
  both members up, marked clean again within a second, and then a member
  suspended held a /proc/mdstat read 12 s after the suspend, until a
  resume freed it. Seen with kernel 7.0.0-42. By md's bitmap code at 7.0:
  /proc/mdstat takes the bitmap's mutex, which md's bitmap daemon holds
  through each of its passes, 5 s apart; a pass sends its bitmap page
  writes without waiting for them, but a pass with work waits, with the
  mutex held and before it sends its own, for every md metadata write
  still out. After the write, the first pass writes the bitmap
  superblock's count of cleared events; with the member suspended by then,
  that write is stuck on it, and the next pass, which clears the written
  chunk's bit, waits for it. Suspended after that first pass, the member
  holds only the write of the cleared bit, which no later pass waits for.
* mdadm writes an assembly's superblock update into each member before the
  kernel takes it: a run that gives the array a new identity and that the
  kernel refuses for the member's bad-block log, or that mdadm refuses for
  too few members, leaves the member under the new identity, and an
  assembly that then drops the log starts the array under it; a run that
  cannot open the member, held by another device, skips it and leaves its
  old identity (`cnagent.md` CN12, the forced start).

## The driver: `cnagentctl`

`cnagentctl` exists because the agent serves plaintext gRPC without reflection:
one subcommand per `ControllerNodeAgent` RPC, replies printed as protojson and
bitmaps as hex; a call fails on a gRPC error or a code other than the expected
one, zero unless the stage names another. It sends the stage's trace id as
metadata and in `Check*` requests (`grpc.md`, Drivers and fakes), takes a
cntlr's request whole from a file, and holds what the script must not
re-derive: the hydration poller (`common.ParseCloneStatus`), which reads
each sample's metadata mode and stops at a dm-clone whose metadata is not in
read-write mode, as the agent serves no namespace through one (`cnagent.md`
CN18); a group's hashed md names (`CnMdDevName`); and a host NQN's host id.
It reads the agent as the dn driver does
(`dnagent_integtest.md`, The driver: `dnagentctl`): its round commands have
the fresh mode, its hydration poller samples with rounds on one stream, the
first sample a fresh one, so its one-sample probe takes a fresh sample, and
its info-read commands are plain memory reads, but for a cntlr's info read
that asks for the counters, which reads them at the call as
`InspectCntlr`'s does (`cnagent.md` CN23).

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
* **The created flip.** A request's tds go out with `created` false at
  first, and the suite plays the sp worker's created flip (`dnv-worker.md`
  RW19): once a primary's reply carries every slice's thin row of a td
  `RES_STATUS_OK`, the case sets that td `created` in each of its cntlrs'
  request files (`flip_created`), and each file's next syncup carries it.
  The worker flips a td soon after its first build, and a primary that
  builds the thin volume of a td not yet created again over an id that maps
  blocks refuses it (`cnagent.md` CN14; `architecture.md`, Known limits).
  redund, repair and clone_xfer flip after their first primary build, so
  their promotions, the forced starts whose levels take the thin pool down,
  and clone_xfer's rebuild after the wipe build a created td over the data
  the host or the copy wrote. thinbm and torn set `created` by hand, and
  keep a td not created where the stage is about that state: thinbm's
  snapshot, and torn's refused volume and half-made snapshot. smoke,
  teardown, discard, restart and pause send `created` false throughout,
  since none builds a written td's thin volume again: they run no failover,
  wipe no kernel state, and the restart case keeps the kernel's devices.
* **Negatives are exact.** Provisioning and pending are healthy statuses, so an
  error row compares against error, never "not OK", and a suppressed one also
  reads the level's details and its reason. A failed read taken for an absence
  passes every negative on it (`cnagent.md` CN21): a residue check's failed dm
  or nvmet listing prints a failing line, and so does a failed listing of
  clone_xfer's token bucket once it is removed, a status-less check reply
  fails, the agent-log readers fail their stage on a log that is missing or
  cannot be read, thinbm's checks, clone_xfer's enable and finalize checks
  that a converge ran no such command and the late flip's check that its
  promotion wrote no optimized group fail when that converge logged nothing at
  all, the long-dead stage counts a path absent only once the same read saw it
  live, an admission refusal needs nvmet's own refusal record behind a fresh
  mark, the park's open check, the dead-leg "no mdadm", the bound on what
  a rebuilt clone copies, and clone_xfer's checks that its agent's restart
  over the held copy mutated nothing and that the source's return created no
  dm-clone carry positive controls, the reads of the dm-clone by the
  restart's reconcile, under a trace id of its own, and by a round of the
  stage's after it, and the reload in the same reading, clone_xfer's
  giveback and finalize checks that the agent ran no command under a trace
  id of its own have the stage's own commands in the same reading (the
  reload, and the finalize's order) — the early, late-flip and srcgone
  checks of that kind carry none, so a log reader that stops matching
  passes them — and in
  redund's md-refusal stage the checks that no array holds a leg and that no
  loop device is left fail on a read that fails and have the running array
  and the listed loops as their positive controls, and repair's forced-start
  stages' counts carry theirs from the same stream: refuse's counts of
  zeroes and assemblies have the stop, which it counts once, as theirs,
  and takeback's count of mdadm commands against the data array has the
  take-back check's two superblock reads.
  Other negatives pass on a read that failed, and so prove less: the
  partition stages' waits for a path to leave live and the finalize's
  checks for a controller of the clone's
  source or of its clone meta group's leg accept the "none" a failed listing
  returns, clone_xfer's checks that a dm-clone, a group device or a leg
  wrapper is absent and repair's checks that a leg wrapper is absent
  accept the "missing" a failed `dmsetup info` returns, and repair's
  checks that md holds no new leg after the abort and no bad leg after the
  add, and takeback's check that md does not hold the refused leg, take a
  read that failed for a member md does not hold, beside the converge's
  verdict and its commands; so
  do the no-path check after an admission refusal and the check that a
  subsystem lists no host, both backed by the refusal record; and the array counts redund's later stages and the teardown
  case read from /proc/mdstat, the standby's no-array check and the md part
  of the residue check take a failure for no array. A log that was read can
  still hold none of a stage's records, so restart's zero-mutation and mdadm
  checks, repair's check that the converge of a started repair ran no mdadm
  against the data array, and the count in repair's abort stage that the
  converge after the one carrying the abort flag, finding the new leg no
  member, added none, have no positive control; the gated clone's count has one
  in the `dmsetup create` records the check before it requires, and the late
  flip's "assembled nothing" in the records it refuses to accept empty.
* **Steady state.** Stages read the agent as the dn suite's stages do
  (`dnagent_integtest.md`, Conventions, Check rounds). One fresh `CheckCn`
  and one fresh `CheckCntlr` round per CN reply code zero, the revision that
  RPC stored and every row OK (`cnagent.md` CN24, CN30), the cntlr round
  first polling with rounds until no leg row reads pending, as a primary's
  legs do until their probers' first round (CN11). The teardown case runs no
  `CheckCntlr` round, since its cntlrs are gone; each of its stages ends with
  the fresh `CheckCn` round per CN, code zero and the CN's two rows OK.
* **Mutation-free.** The mutation reader lists every mutating command, every
  configfs or block write ("os write block" by name) and every discard tried
  ("os discard ranges", one line per range it lists, a range past the
  record's done count read as failed) in a cn agent log, whole
  or for one trace; the probers' "probe …" records fall outside it by
  construction (`osclient.md`, Exported raw helpers and the probe-IO
  carve-out). Records no stage RPC caused carry trace ids of their own
  (`cnagent.md` CN2), so clone_xfer's restart and rebuild checks and the
  restart case's checks read a fresh log, and clone_xfer's check of the
  source's return reads the log after a mark.

## Cases

Setup proves each dn agent by its exact data area and a baseline `SyncupDn`
(`dnagent.md` DN3, DN5), each cn agent by a `GetCnSize` echoing its capacity
(`cnagent.md` CN3, CN-CM1) and a baseline `SyncupCn` with its base row and
its store row OK (CN5, CN28). As an agent's gRPC listener opens before its
startup pass (`dnagent.md` SH1, SH33), the drivers' wait for the size reply,
here and after every restart, goes on reading the node's info until the
agent no longer answers that it is starting. Both agents of a VM start with
an IO queue cap below the VMs' CPU count, so that the cap and not the CPU
count sets each path's queues (`dnagent.md` CM2, SH32).

**nodecheck** runs first, while neither VM holds a dnv object, and proves
`dnv-agent`'s node check, md replay and gather on the lab's kernels and
tools (`dnagent.md` CM5, CM6). The check stage runs `check dn` over each
VM's loop device and `check cn` on each VM, as the agents run: each exits
zero with no failed result, and its warnings are logged. The replay stage
runs `check cn --md-replay` on vm1: it exits zero, every step of the replay
holds, and no loop device, array or directory of the replay is left. The
replaywrap stage puts an mdadm first in the check's PATH that adds the
start-anyway flag to every assembly, as a node whose mdadm does not keep
its degraded gate: the check exits one, the replay fails at the lone
leg's refusal and stops there, and its teardown leaves nothing. The gather
stage builds an md array with an internal bitmap over two dm-linears, named
outside dnv's dm prefix and with a dnv array name, so that no agent and no
replay reads them as dnv's while the md assembly mask keeps udev off them,
writes the array once with both members up, suspends one member a second
later, which is most often before md's bitmap daemon's next pass, and
first proves its premise: a read of /proc/mdstat
that has not finished after 2 s, seen within 30 s, a read started every
2 s (Lab facts); then
`collect` with a short deadline returns within that deadline and a margin,
exits zero, and its archive's read list names the /proc/mdstat read as not
finished, while its in-process md read shows the array.
The stage then resumes the member and removes what it built, and
/proc/mdstat reads again. The cleanup's first phase, and a failing run's
exit, release the stall and remove the mdadm wrapper (Teardown and
cleanup).

**smoke** proves the plumbing: the pointer before the `SyncupCntlr` that builds
a `RedundNone` primary stack (`cnagent.md` CN8 to CN16), all rows OK but the
legs, pending until their probers' first round (CN11); admission on real
nvmet (CN16): built with no allowed host the subsystem admits none, probes
clean and refuses the emulated host's connect; granted, it admits exactly that
host; the host's path and the CN's leg paths each hold the cap's IO queues
and the admin queue (`dnagent.md` SH32); revoked again, the connected host keeps its path and its next connect is
refused; host IO; clean check rounds, the namespace identity included
(`dnagent.md` SH17); the raw thin-pool status line the auto-grow parses (CN28);
a raid0 suspended by hand, which a check round's read names with the leftover
code and the `SyncupCntlr` sent again resumes (CN30); four subsystems made by
hand (CN21): one with a namespace over a device that is no ns-dev, one empty
with the kernel's default model, one with dnv's model and a namespace whose
device was never written, and one empty with the kernel's default model and an
allowed host. Through the grace the cntlr is read every round and those reads
leave the directories' times where they were, and the node's read while they
are young names none; past the grace the node's read names the two with no
host and no port link, and a fifth made just then with dnv's model and an
allowed host, which its model alone decides with no age, and the `SyncupCn`
that brings removes the three and leaves the other two. A subsystem made by hand on the other VM, which is no transfer
of the cluster, connected from the CN once with the cn agent's host NQN and
once with another: the node's read names it, and the `SyncupCn` that brings
disconnects the controller of the cn's host NQN and leaves the other's (CN21).
A subsystem whose NQN carries dnv's prefix and decodes to nothing, which the
request names: the namespace the request drops goes, and the subsystem, once
the request no longer names it, stays, as the known limit says, until the
stage removes it. Then a teardown by an empty pointer list, re-sent while it
replies leftover (CN7, CN21), leaving the CN no dm device or host-facing
subsystem but the two made by hand that it leaves alone, which the stage then
removes itself, and a fresh `CheckCn` round replying clean (CN30).

**redund** first pins, with no agent and no NVMe-oF path, the refusal the
cross-leg half of failover safety rests on (`architecture.md`, [D16]): on
one VM, a clean two-leg array over two loop devices, built and stopped with
the agent's create and stop commands under a dnv name the md udev mask
matches (Lab facts), so that both superblocks still count both legs at one
event count. Started from one leg with the agent's assemble command
(`cnagent.md` CN12's assembly case), mdadm refuses, its own message saying
it needs every member to start the array, writes nothing to that leg's
superblock and leaves no array holding either leg; started from both legs,
the array runs clean, both members in sync, with nothing to rebuild. With
one leg then failed and the array stopped, the survivor's superblock counts
itself alone and the failed leg's still counts both: the failed leg alone is
refused, and the survivor alone starts the array, degraded, its member in
sync, the fact the early fail rests on (`cnagent.md` CN12, The early fail).
The command is the agent's: the promotion below holds each assemble command
the agent runs from both legs to the arguments the same rule builds for that
group, and the unit tests hold the agent's start from one leg to the same
rule. It then builds md-raid1 groups across both DNs under a primary and a
standby. The primary creates both with `--assume-clean`: provisioning zeroed
every leg's meta region, so none carries a superblock, and no read reaches a
data block the pool has not written (`cnagent.md` CN12's create case;
`architecture.md`, [D15]); the standby keeps every leg connected and
non-optimized, assembles no two-member array, and backs its ns-dev with the
td's dm-error, the namespace inaccessible (CN10, CN11, CN16's standby rule);
each CN's subsystem admits exactly the host's NQN (CN16). With new
connections from the standby's VM to DN1's port dropped there, and the
standby's controllers of its two legs into DN1 disconnected, a converge of
the standby dials DN1's address once and runs no `nvme connect` into it:
both legs read `ERROR` with the dial's timeout, every other row reads OK,
and the converge takes at least the dial's timeout and no more than one dial
timeout plus a second over a converge that connects nothing (CN10). With the
rule taken away, the next converge connects both legs again and a fresh
check round names nothing; its dials leave no nvmet-tcp error in DN1's
kernel log (`osclient.md`, DialTcp). Before the host writes, the foreign
stage plants, one at a time, a host's md version-1.2 superblock, an LVM
physical volume with a volume group and a volume, a bcache superblock and an
ext4 file system at the start of the data group's array on the primary,
where a fresh pool's first host write lands, each made with a host's tools,
the md and LVM ones on a scratch loop device that a rule of the stage's own
keeps the system's rules off and the bcache and ext4 ones in a plain file
udev never sees, and sends the array a change event, as its assembly does: a
low-level blkid probe reads each signature, and udev's record of the array
carries the md array rule's type, with nothing holding the array but dnv's
devices, no link by uuid or label leading to it, no volume of the planted
group active, no physical volume recorded online and no bcache device
(`architecture.md`, Components: invocation reference); the array's start is
zeroed again after each. It then writes the end of an md 1.0 array made just
then over the end of both data legs, through the primary's leg devices, as a
host can make one at its volume's end; the standby's paths end in dm-error
on the disk nodes (`dnagent.md` DN10), so only the primary's reach the legs.
Through each VM's leg devices once they serve — the primary's at once, the
standby's after the flip — mdadm's plain examine of each leg reads that
newer superblock, the examine naming the 1.2 format reads dnv's (Lab facts),
and a change event leaves each leg's NVMe device marked not ready under
dnv's name by the md mask, held by nothing but dnv's leg device. Before that
event the stage drops the NVMe device's page cache: its writes reach the leg
through the leg device above it, past that cache, which keeps an earlier
read's data while the leg device holds the NVMe device open, and udev's
probe reads through it (Lab facts). The demote moves ANA before the park
(CN9's ANA pre-step before its park pre-step), stops the arrays and keeps
the legs; the promote assembles both (CN12's assembly case), each with the
arguments the first stage's rule builds for its own group, the data
surviving, the data group's from both legs, which the newer 1.0 superblocks
would make a plain assembly refuse (CN12, The superblock format). After the
failover neither VM holds an array, a volume group or a bcache device made
from the planted signatures, and the stage zeroes the legs' ends again
through the new primary's leg devices. With what the primary's machine sends
to DN1's port delayed (`tc netem`, no device changed), the primary's inspect
read reports a write half of the probe of each of DN1's legs, from a round
made under the delay, at no less than the delay, and of DN2's legs, once
each has a round, below it; every leg's row reads OK on a fresh round, and
DN1's legs' counters, read at the call, have moved (CN11, CN23); with the
delay gone, a later round of DN1's legs is fast again. With the primary's
legs into one DN cut under a host write, every fresh check round reads both
md rows OK until the data row reports degraded and the dead legs' rows error
(CN12, CN28, CN29); once a dead leg's probe has failed enough rounds, a
round may also name, with the leftover code, a member of that leg md still
holds in its place, since the early fail would fail it (CN12, CN30); no
converge of that stage runs, and the round names nothing else. The stage
first checks that the host write is stuck on the dead member; if it is not,
the stage prints each md member's state, each controller's state and the
partition rule's packet counters before it fails, which tell a partition
that did not take from a member md had failed before the write. Once the
write completes on the surviving leg, the agent's log holds no mdadm command
under the stage's trace id, under which each read those rounds start logs
(`dnagent.md` SH29): that covers every such read that has ended by then, but
not the read the last fresh round leaves running, which the stage does not
wait for. The leg rows recover once the partition lifts, and the write reads
back. A fresh check round then names the member md failed, with the leftover
code, and the `SyncupCntlr` re-sent as the worker would puts it back: the
arrays read clean and the member is in sync with its failfast flag (CN12,
CN30). The same put-back of a member failed by hand, on a whole array with
no IO while the member is out, is one converge whose only mdadm commands are
the remove and then the add, and the sectors it writes to the member are a
small part of the array: that count, never a time, tells md's re-add from
its bitmap from the full copy mdadm makes when it cannot re-add. With the
same legs cut again and no IO to the arrays — the cut waits until md's
write-intent bitmap on every member reads no dirty chunk and both arrays
read clean, since md clears the bitmap a few seconds after a copy ends in a
write to every member, which a cut leg would fail — a fresh check round
names each member md has not failed by itself once its leg's probe has
failed twice (CN11, CN30), the stage logging how long after the cut, and the
`SyncupCntlr` re-sent as the worker would fails each named member in md once
and removes none (CN12, The early fail): both md rows read OK, and the
survivor's superblock records the death, its array state counting itself
alone. While the cut holds, the agent runs no command under a trace id of
its own (CN10, No converge in the background; CN30): no converge can make
the late members serve, and no read names them. With the legs back, the
`SyncupCntlr` re-sent on the rounds that name the members puts them back
under the stage's trace id, and the data member takes back a small part of
the array, counted in sectors as before. With the port link of one of the
primary's data sides taken away on its DN while the port listens, the
primary's reconnect is refused with DNR and its kernel deletes the
controller; with the link back, a fresh check round names the side with no
controller (CN10, CN30), and the `SyncupCntlr` re-sent as the worker would
connects it, after which the arrays read clean again; a member md fails some
seconds later, once IO reaches it, is named by a later round and put back by
the request re-sent on that round's code. Read-only fails writes through
dm-flakey (CN16's read-only rule, CN19; `architecture.md`, [D11]); the data
array's member on the DN the partition cut is then failed by hand, so the
stage's last write reaches the other leg alone, and no converge of the
primary puts it back before the late flip demotes it. The late flip promotes
before any side flips, so the promotion assembles no array and builds no
raid0: both md rows read "no available leg", the ns-dev stays parked with
its row in error, and the promotion writes no optimized group, so the host's
path stays inaccessible and its IO would wait rather than fail; the
namespace row reads OK on the converge and alike in the observation a fresh
round then reports, made after the converge by a read, which names nothing a
converge could act on, and the agent runs no command under a trace id of its
own while the members are late (CN12, CN16's ANA rule, CN28, CN30). Once the
sides flip, a read names both groups' absent arrays, which the assembly
would start, and nothing else; the `SyncupCntlr` re-sent on that code, the
second the primary gets, assembles both arrays, puts the ns-dev on the raid0
and moves the namespace optimized, all under the stage's trace id. A block
only the newer leg holds reads as that leg's, the stale member kicked (CN12,
CN30; `architecture.md`, [D16]). The stale member goes back in the first
converge after the flips whose copy source's leg row no longer reads `ERROR`
(CN12): the probers start at the promotion and their rounds through the
standby's dm-error export fail (CN11), so that is the re-send's own converge
when a probe round of the newer leg made after the flips has passed by then,
and otherwise a converge the stage brings once a read names the put-back,
after the stale leg's row reads `OK` on such a round. Either way the add
runs under the stage's trace id, and the stage plays the worker for a
bounded number of re-sends until a round answers zero with both arrays whole
and the member in sync; through it all the agent runs no command under an id
of its own. Last, md's check (CN12, The md check): eight sectors of one data
side fail in its disk node's export to the primary alone, and the suite,
playing the worker, puts an md check request on the data group and reads the
cntlr until its reading names the request done. Over sectors that fail reads
and take writes (dm-flakey), the check rewrites them and records no
bad-block range: the reading names it done, which needs md to run it to its
end with the array whole, so it read those sectors and md failed no member;
the leg's row stays OK. Sectors that fail both (dm error) leave a bad-block
range on the member, which the reading counts (CN28), the leg's row still
OK. The export's table comes back after; the range stays with the array,
which the teardown removes.

**repair** proves on real md the cn agent's part of a repair
(`architecture.md`, Repair; `cnagent.md` CN12, CN28, CN30), on a pool of its
own shaped as redund's, md-raid1 meta and data groups across both DNs, whose
first two repairs have the data group's DN1 leg as their bad leg and new legs
with their sides on DN1 too; each later repair names its own legs. The
primary reports beside its rows, for each group whose row reads OK, whether
the array is whole and which legs md holds in sync, and a standby reports
none.
While the bad leg serves with an OK row, a CREATED repair waits: its new leg
is connected, wrapped and probed on both cntlrs, the converge runs no mdadm
against the array, and its repair verdict says neither started nor abort
confirmed, while the standby gives no repair verdict. The abort: with the
new leg added by hand to the whole array, where md holds it without a
place, a converge whose
request carries the abort flag removes it, adds and fails nothing, and
confirms the abort; a converge after it, finding the new leg no member, adds
nothing and confirms again; and the request without the repair, as a cancel
leaves it, lets the new leg's wrapper and connection go, re-sent while it
replies leftover. The add: a second repair's new leg, while the bad leg does
not serve the primary — its side's optimized path moved to the standby, md
usually still holding it in its place, with no IO to it — is added in the
converge that first connects it, after the bad leg is failed and then
removed, or removed alone when md has moved it out of its place already, and
no other mdadm command; md copies the whole data region onto the new leg,
which ends in sync, the array whole, and the repair verdict says started.
The standby, sent each request of the add and of the move to STARTED as
the worker's fan-out sends it, connects and wraps the new leg and gives no
repair verdict, and the primary's converge of the STARTED repair runs no
mdadm against the array. The assembly after a failover, the bad leg not
available to the new primary, starts the data array from leg 2 and the new
leg, each with a superblock, in that order and with no create; the data reads
back, and reads back from the new leg alone with leg 2 failed by hand, which
the verdict then names and the re-sent `SyncupCntlr` puts back. The finish's
request, the new leg in the bad leg's place and the bad leg gone, lets the
retired leg's wrapper and connection go on both cntlrs with no mdadm command,
and the array stays whole with its two legs in sync.
Then the forced start (`cnagent.md` CN12), on the same pool, its new primary
at the level that takes the thin pools away, so that nothing holds the
array the death breaks. A third repair, whose bad leg is leg 2, adds a new
leg on DN2 and the worker's move makes it STARTED, while md copies slowly
from the leg the finish put in place, the one leg that keeps the data; that
leg's export to the primary then fails every IO mid-copy, as a disk that
returns errors does, until md reads the array broken, when the case freezes
md's copy so that its retries do not move the state again, the new leg's
superblock carrying a recovery offset. A mark naming the half-copied new leg
stops the broken array and is refused: the group's row reads ERROR naming
the recovery offset, and nothing is zeroed or assembled. With the dead leg's
export back and its leg row no longer in error, so that it can feed a copy
(`cnagent.md` CN12), the inspect's dry run from that leg says what the start
does, the new leg zeroed under the identity update, with no mdadm command
run, and its read of the leg matches md's own examine (`cnagent.md` CN31);
a mark naming it reads its superblock, zeroes the new leg alone, the bad
leg left as it is, starts the array from the named leg alone with either
form of the forced assembly, and the repair's add adds the blank new leg, in
that order, under one forced start record naming the leg and the zeroed
member; md copies it whole, the reading names both legs in sync and the
mark's revision as the mark the primary acted on, the repair verdict says
started and names the new leg as held, and with the mark withdrawn, as the worker's clear withdraws it,
and the pools back, the reading names no acted mark and the host reads its
data unchanged.
The finish makes leg 2 a retired leg. Then a restore from an older copy:
new data over the host's first block, the level that stops group arrays,
and a fourth repair whose bad leg is the leg holding that data and whose new
leg is leg 1, the retired leg from before it, taken back, with a mark naming
leg 1. At the level that builds no thin pool the forced start zeroes the
other leg of `leg_list` alone, a full member of another identity, whose
change count it does not compare, and starts the array from leg 1, the repair
verdict says started, and after the finish and with the pools back the host
reads the first block from before the new data. Then the take-back: a fifth
repair takes back the leg with the newer data while its bad leg does not
serve the primary, and the converge refuses it, as its superblock carries
the identity of the array before the restore's forced start — no mdadm
command against the array, md still holding the bad leg, the repair verdict
neither started nor abort confirmed and its refusal naming the leg with its
identity beside the array's, as mdadm's examine reads both, and leg 1 as
the leg that keeps the data — and a fresh read, once the taken-back leg's
prober has read it, reports the same refusal with a clean verdict, though
md can fail the bad leg in between, which raises the array's count
(`cnagent.md` CN28, CN30); the abort confirms with no refusal, and
after the cancel, the bad leg serving again and put back if md failed it,
the host reads the restored copy. Then a bad-block list: the suite writes
a bad block on the new leg of the third repair, in sync beside leg 1,
through md's own bad-block file of that member, and md writes it into the
leg's superblock; at the level that stops group arrays, and
then the one that builds no thin pool, a mark naming that leg with no
force is refused for its list, its row naming the list, with nothing
zeroed or assembled, and the dry run refuses alike and, with the force,
names the drop; the same mark with the force reads its
superblock and its list, zeroes leg 1, starts the array from the leg under
a new identity, stops it and starts it again with the list dropped, then
adds leg 1, under one record naming both updates, and the leg's superblock
shows no list; the host reads its data unchanged. Then an older named leg:
leg 1 dies for the primary alone and the host's write of its first block,
its own bytes again, makes md fail it, so the other leg's change count
moves past leg 1's; with the arrays stopped and leg 1 back, both
superblocks of one identity, a mark naming leg 1 is refused, the row
naming the change count, with nothing zeroed or assembled, and the same
mark with the operator's force zeroes the other leg and starts the array
from leg 1; the host reads its data unchanged (`cnagent.md` CN12). The marks that name a leg whose bad-block list depends
on the run — the forced start from the leg that kept the data, and the one
from the retired leg taken back — read the named leg's list at run time and,
when it carries one, meet the refusal with no force and go on with the
force. Then
the re-assembly, the way the forced-start guide gives an operator: both
legs' exports to the primary error every IO, and a host write to a region
it wrote before fails, which leaves the data group's array broken under
the pool; with both exports back and both legs' rows no longer `ERROR`,
a converge leaves the array broken, its row naming the pool's device that
holds it open, and a check round does not name it; the step to the level
that takes the pools away is a converge that sweeps the pool's devices
before it builds the groups, so it stops the array and assembles it
again, with no zeroed superblock and no run flag, both members in sync
after md's copy (`cnagent.md` CN12, The re-assembly). Broken once more at
that level by a write through the array itself, with the exports back, the
array is named by a fresh check round, by its node under its own kind
(`cnagent.md` CN30), and the `SyncupCntlr` re-sent as the worker would
assembles it again. The pool comes back at the level that builds it, and
the host reads its data unchanged. Before
its teardown, with the arrays stopped, every member of the data group is
zeroed by hand, as a forced start cut short after its zeroing leaves them,
and the request carries the flag that the group's array has run: the
primary creates no array over the blank legs and reads the group's row
`ERROR` "has run before" (`cnagent.md` CN12).

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

**torn** builds a pool of two slices on DN1, each the S shape's two
RedundNone groups (the S shape is smoke's pool: one RedundNone meta group
and one RedundNone data group on DN1), so a volume stripes over two thin
volumes (`cnagent.md` CN14). A thin id planted by hand in slice 1's pool,
one block written through a device made by hand and then removed, as an etcd
restore can leave it, makes a new volume over that id read
`RES_STATUS_ERROR` with `RES_REASON_THIN_ID_NOT_ADOPTED` and the block in
its details in that slice, with no thin volume built over it and one header
read of that pool, while the other slice builds; the converge's reply names
nothing, and a read names the absent volume (CN30). Once the id is removed
by hand, the re-send builds the volume. A snapshot's id made by hand in
slice 2's pool, inside the origin's per-slice quiesce, then an agent
restart, stand for a primary that stopped between the two slices' steps: the
next converge makes slice 1's id beside it and reports the snapshot torn,
the torn marks file of the cntlr is in the local store, a fresh read after a
second restart still reports the mark, and a request that lists the snapshot
torn clears the mark and the file (CN2, CN14; `dnv-worker.md` RW19 is the
worker's half).

**discard** proves that a host's discard reaches the thin pool and that a
deleted volume's discard reaches the thin pool and the disk node through the
whole stack, on the S shape on DN1 with one slice, a data group many extents
long and a volume of a gibibyte, which maps nothing it was not written. An 8
MiB host write raises the pool's used data blocks by eight; an aligned 4 MiB
`blkdiscard` drops them by four and a 512 KiB one by none, as one slice
hands the thin volume each discard whole (`architecture.md`, Known limits,
the entry on a host discard). A request that drops the volume and its
namespace then brings the discard of `cnagent.md` CN14: the agent's log
shows the volume's discard, one chunk from the end of its highest mapped
block down to its first sector, before its removal, which comes before its
id's delete; the pool's used blocks go back to their baseline, and DN1's
side device counts among its discards at least the blocks the volume still
mapped. The case plays the worker's re-send of that request, since a removal
that fails busy leaves the volume to the next pass (CN14), and takes the
first reply clean or naming the volume alone. It logs the pool's status with
its discard passdown word, the discard counters of the side device and of
the per-CN dm-linear above it, the backing file's allocated blocks before
and after, and the agent's record of the discard with its time. With
`DISCARD_MODE=measure` the host writes the whole volume before the delete,
so that record times the discard of a written gibibyte.

**clone_xfer** proves `architecture.md`, Transfer + clone = cross-SP live
migration, and Clone metadata durability, between pools on different nodes
exporting one NQN and namespace identity from disjoint cntlid slots. The
stored-suspended destination and the transfer's origin are parked, live on the
td's dm-error with the origin's ANA moved first, so an open fails at once and
nothing is dm-suspended (`cnagent.md` CN9's ANA pre-step before its park
pre-step, CN16's parked rule; `architecture.md`, Namespace suspend semantics,
[D12]); the transfer admits only the destination CN (CN17) and refuses the
emulated host, which its list does not name. The clone carries its clone meta
group (`architecture.md`, Terminology and object model), whose one side on the
destination's DN the suite provisions, over a data group's side's length,
before any request names the clone (Conventions). Gated at the no-clone level
(CN19), the clone's two rows reading the level, the group is built as any
group is, its leg connected and wrapped and its device a dm-linear over the
wrapper, while no dm-clone is, and two chunks of one source slice push
race-free and prove the pair addressing (CN20, CN22). At read-write one
converge keeps that group as it is, builds the dm-clone with the group's
device as its metadata device and with exactly the no-hydration and
no-discard-passdown pair, and lands the chunk as one discard at its skip range
before hydration is enabled (CN18's metadata, dm-clone, bitmap and hydration
steps); the first hydration sample, a fresh one, already counts the skipped
half, and the last region the source wrote reads back right, through
read-through if need be. The copy runs slowed, the destination CN's writes
to the sides on its own VM held to a low rate (Lab facts), so that the case
acts in its middle. The chunk, grown by one byte past the clone's regions
(`architecture.md`, [D8]), is then pushed again while the namespace device
maps the dm-clone: the log shows the copy stopped, the namespace device
suspended, the chunk's discard landed, the device resumed and the copy
started, in that order, the pause's length logged, and the host's path
stays optimized (`architecture.md`, Bitmap push protocol, The apply pauses
the writers). Once the copy has passed the chunk, the source loses its
namespace behind both agents' backs, as it does again once the copy is whole
(below), and the copy holds at a count past the chunk's (Lab facts). The
destination's agent then restarts over the held copy and leaves it as it is
(CN2, CN18): the count reads the held one, the dm-clone, its group's device
and its leg's wrapper keep their device numbers and tables, the fresh log,
read once a round of the stage's has read the dm-clone after the reconcile,
so that it holds the whole reconcile, holds no mutation, the dm-clone is read
under no trace id but the reconcile's and the stage's, six seconds later as
well, the source's row reads missing with the source-fault reason (CN18) and
the dm-clone's and the group's ok. The source gets its namespace back from the
transfer's own converge of a re-sent request (CN17), under a new device number
on the destination; the destination changes nothing by itself, and a read of
it names the dm-clone, whose table names the old number (CN18, CN30). The
request re-sent to the destination on that code, the one `SyncupCntlr` it
gets, reloads the dm-clone onto the new number in place, under the stage's
trace id like every command the destination runs meanwhile, tries the chunk
again with the ns-devs paused, which marks nothing new since the first build
marked the chunk's regions (`architecture.md`, Bitmap push protocol, The
apply pauses the writers), and enables hydration (CN18), its count at least
the held one, and the copy goes on. The startup reconcile rebuilds a
destination CN wiped in the middle of that copy from the request it stored,
and the request re-sent while a read names what a step of it left undone
finishes the rebuild, the group's leg and device before the dm-clone over
them and the chunk again before hydration (CN2, CN18, CN30), its dead host
path replaced by device as the pools share the NQN. The wipe-time sample
counts more than the held count,
and between the destination raid0's write count read while the copy held,
which holds any part of a block that a copy failing as the source went
left, and one read between two samples that read the same count, the copy
alone wrote one whole block for each region copied past the held count and
no more (Lab facts): no region was copied twice, so the reload loaded the
metadata its group holds, where a reload over metadata formatted anew would
copy the regions before the hold again, whatever the
timing of its first sample; the rebuilt clone's first sample counts at least
the regions that sample did, and until hydration completes the clone writes
into the destination at most the regions that sample left to copy, fewer
than the source's written half: a dm-clone that loaded the metadata its
group holds does so, while one over freshly formatted metadata copies that
half whole. The hydrated clone's
source then loses its namespace behind both agents' backs, the transfer's
export and the destination CN's controller of it staying: the clone's two
rows, its group's row and its destination namespace's two read one status,
with one epoch, on a converge, in the observations a fresh round and then an
info read report, each made after that converge by a read, and on a second
converge (`cnagent.md` CN18, CN28), the source's row missing with the
source-fault reason, which sets no health epoch, and the other four ok, the
dm-clone's with no reason, while the namespace serves on, optimized, through
the hydrated dm-clone (CN16's clone rule) and the dm-clone's row still carries
the status line the gateway's hydration check reads. Over a live controller
with no namespace every reply answers code zero, since a converge connects
nowhere beside a controller; with no controller the fresh round and the info
read name the source's endpoint, which only a converge connects, and the
converges answer zero, naming no endpoint themselves (CN18, CN30); the
driver samples hydration only from a round that answers zero, so the status
line is sampled over the live controller alone. Either way the agent runs no
command under a trace id of its own between the stage's calls. The state
compared is measured as the dn suite's lost source measures it
(`dnagent_integtest.md`, Cases), and a reading that moves before the
comparison ends fails the stage. The finalize takes the clone down in layer
order, the clone, its source connection, then its group's device and its leg
(`cnagent.md` L3, L5, L9, L10), leftover while the leg's disconnect runs and
while the source's does, unless the source's controller was already gone, and
the group waits for a pass that finds the source's connection gone. That pass
is the finalize's first when the source's controller was already gone, and
one of the stage's re-sends otherwise; every command the controller node runs
after the finalize begins, the disconnects off the passes included, is under
the stage's trace id (`cnagent.md` CN10), the order read from them all. The
half the source wrote reads back intact through the destination, which then
takes a write, while the half it never wrote stays unmapped and zero there, the
destination starting empty (`architecture.md`, [D3]); and at teardown both CNs
pass smoke's per-CN check.

**restart** proves persistence and an idempotent reconcile (`cnagent.md` CN2;
`dnagent.md` SH1, SH4 to SH6): both cn agents restart over untouched kernel
state as the host reads on; every snapshot, on both sides of the restart, is a
fresh round's reply, and the reloaded infos, revisions included, equal the
snapshots but for the status epochs (`dnagent.md` SH14) and the replies'
stamps, which differ by nature; the agent's log shows no mdadm command
the check counts, which leaves an examine out, though the agent runs none
and reads superblocks itself (CN12), and leaves out the start record's
`mdadm --version` (`log.md`, Agent start), which reads no member;
equal-revision re-sends leave the new logs free of any mutation, an
activation sweep's included (`dnagent.md` SH16; `cnagent.md` CN14); and a
`SyncupCn` below the revision those re-sends stored is refused as stale in a
normal reply (`dnagent.md` SH8, SH9; CN4).

**pause** is the write pause when a disk node dies (`architecture.md`, Known
limits, the entry on a disk node that dies): DN1's VM is cut off from CN2's
both ways, so DN1 answers nothing, as a dead node does, under a host writing
on CN2's VM. It builds redund's pool with the primary on CN2 and the standby
on CN1, its requests setting the legs' fast IO fail timeout below the
default (`cnagent.md` CN10), and connects the host to both. The writer, the
sampler of CN2's leg paths and md members, CN2's kernel log and the agent's
stamps read CN2's VM's clock; the rounds and the cut are timed by the
driver's clock, the cut also by vm1's, and vm1's kernel log by vm1's; the
case refuses a clock chrony reads as off on either VM, since chrony slews a
clock that is far off and a slewed clock runs fast or slow by several
percent. A writer of synced 4 KiB blocks, one at a time, writes over a
window it wrote first, so no write maps a new block. Through the cut the
case plays the worker: check rounds at the check interval, and the
`SyncupCntlr` re-sent once CN2's paths into DN1 have left live. Each of
CN2's leg controllers reads the request's timeout, no write fails, and the
longest write stays under the teardown's bound plus the timeout plus a
margin; once the cut lifts, the paths come back and the members md failed
are put back, as in redund. With `PAUSE_MODE=measure` and `--only pause` the
case is the measurement instead: it first logs what a controller nvme-cli
connects with a fast IO fail timeout of zero reads, then cuts the pool at
the agent's default and with the leg controllers' timeout written shorter
and longer in sysfs, then cuts a `RedundNone` pool at the default and at the
longer timeout, timing the write that fails, and prints and keeps each cut's
writes, samples, kernel logs, rounds and summary on the driver.

**What a pass means.** Exit status zero and PASS mean every assertion held in a
run that stops at the first failure: data checked against patterns the agents
never see, removals proved by the nodes' own listings, every stop a case makes
confirmed; the race a loop device usually wins, the provisioning window, is
logged, while clone_xfer's copy, which a loop device would finish at once, is
slowed, so its read-through window, logged too, is open when its probe reads,
and the case fails unless the copy holds once its source goes and the wipe
comes in the middle of it.

**What the stock rules do without the rules of `deploy/udev/`** is a manual
check the suite never runs, as it never removes rules it did not install: on
a pair of lab VMs, after setup, the files of `deploy/udev/` are removed from
the rules directory and udev reloaded, and redund's foreign stage is run with
its checks of udev's records left out. What each stock rule then makes of
each signature — an array over the data group's array or a leg's NVMe device,
an active volume of the planted group, a physical volume recorded online, a
bcache device — is read from the node; the cleanup's sweep of what the system
made of the foreign signatures removes it.

## Teardown and cleanup

Cleanup runs unconditionally at the start of every run and at its end only
on success; a failing run leaves its debris and dumps the agents' logs, both
VMs' dm, md, nvmet and host state, each read bounded, and the failing
stage's trace id, but for clone_xfer's token bucket, which it removes, as
the bucket would slow any suite that runs on the VMs before the next
cleanup, and pause's cut and its writer, which it removes and stops, as the
cut would black-hole another suite's connections, redund's drop of new
connections, which it removes for the same reason, and its netem delay,
which it removes for the bucket's reason, and nodecheck's stalled array and
mdadm wrapper, which it resumes and removes, as a suspended member wedges
another suite's udev and LVM scans. A cleanup-only run does the scrub alone.
It takes every `dnv-agent` and every dnv dm device, md array and nvmet
subsystem on the node, of either role, which is why one dnv suite runs at a
time in the lab (`layout.md`, Directory tree) — but for two blind spots the
wipe exists for: its dm steps see only names with a role-lettered kind, and
its md stop skips an inactive array over unreadable members, which neither
udev nor mdadm names. Each step is best-effort, and the order is
load-bearing on one principle, a holder goes before what it holds, across
both VMs at once: the agents, their retries and probers with them, before
the devices; the fault injections and clone_xfer's token bucket
unconditionally, as each outlives its stage and would pass for another fault
next run; host connections before the subsystems they hold, exports inside
out, cn devices top-down and the dn objects in the dn suite's order
(`dnagent_integtest.md`, Teardown and cleanup); a dm-clone while its source
is still connected (`cnagent.md` L3), which makes the cleanup two phases
across both VMs, every clone of either VM gone before any transfer, as a
clone flushes through a source the other VM exports; each disk's header
block zeroed (`architecture.md`, [D13]) and the udev rules removed last; and
before the arrays stop, whatever the system made of redund's foreign
signatures, an array, an active volume or a bcache device over one of them,
should a rule have failed, together with the stage's scratch devices. Every
suspended dnv device is resumed before anything over it is removed, as one
wedges its removal, the namespace disable above it and any block scan
(`architecture.md`, [D12]). md arrays are named through udev, else by
mdadm's export, never by its scan, which prints no name on these guests;
only a dnv-named one is stopped, but for an array over the loop devices of
redund's md-refusal stage, which is found by what holds those devices,
whatever its name or state, and stopped before they are detached.

The wipe is a separate flag that runs no case. Kind-blind, it takes every
dnv-named dm device, every dnv nvmet subsystem and md array, an array named
also by its members' dm names, and every fabrics controller on the node, a
reach that makes it a flag; then, when no residue fails it (below), the
ordinary cleanup runs. It repeats while
dnv dm devices remain, as udev can re-assemble an array from its members, and
any dm or nvmet residue fails it, since reporting it is not success.

## Out of scope

Error paths beyond those that Goal and scope lists; QoS, not enforced
(`cnagent.md` CN6); `GrowSlice`; a leg gaining or losing a second side,
whose disk node part the dn suite's migrations prove, but for a cancel,
which only the e2e suite's cancelled migration exercises, and whose cn agent
part — the connect per side, the disconnect of a dead path by device and the
cut of a migrating leg's path (`cnagent.md` CN10) — the unit tests prove,
the e2e suite's migrations exercising it, and the path rows of a leg with
two sides (`cnagent.md` CN28), which the unit tests cover; levels but
read-write, read-only and the no-clone gate, save what repair's forced-start
stages read of the two levels they use to free the data array: the pool row
the level that builds no thin pool suppresses, and the group array the level
that builds no group array stops (`cnagent.md` CN19); more than one slice
but torn's two RedundNone slices on one disk node; several namespaces per
subsystem; snapshots of snapshots; `UpdateNamespaceDev`; a host-facing
namespace or subsystem dropped short of a cntlr teardown, leaving the park
before its removal (`cnagent.md` CN21's park before every layer and its
nvmet layer L1) to the unit tests; cntlid-slot exhaustion; the cdc; TLS and
authentication; performance; faults beyond those the Cases name;
provisioning deferral (`cnagent.md` CN9), a clone's for its clone meta
group's sides included; a clone meta group under md-raid1 or one that is not
ready, and a dm-clone whose metadata is not in read-write mode, all of which
the unit tests cover (CN18); of a repair, the put-back of a new leg md has
failed, a bad leg that md holds without a place once its repair has started
in md, the add's wait for a member that keeps the data, a half-copied new
leg after a failover, a new leg that is not available, which is no late
member short of a forced start's wait and no candidate of an assembly, a
converge's read of the array again that fails or finds it not running, an
aborted repair's new leg as an extra and a repair on a clone meta group, all
of which the unit tests cover (CN12); of a forced start, a second run of its
assembly after a first run that failed, the stop a pool's concat refuses, a
zero that fails or is killed or that its read back does not prove, a
superblock read that fails, a new leg of a CREATED repair whose abort flag
is set, which it does not zero and starts from only when the mark names it,
a STARTED repair's new leg, which it keeps whatever the flag says, its wait
for a late member and its other errors, and of the take-back check, a new
leg with no superblock, a STARTED repair's add with no member to compare
with, a keeper whose superblock read finds none and a superblock read that
did not answer, and the identity check of a `leg_list` member's add with the
reasons it and an assembly of two identities put on the legs' rows, all of
which the unit tests cover (CN12); a snapshot torn by a quiesce that fails,
and a header read that waits or fails, whose rules the unit tests cover
(CN14); the lingering-clone hold's scope per thin device, left to the unit
tests (CN18); the other states of the clone's shared row rule (CN18); and
whether a refused request leaves a lock entry (`dnagent.md`, Concurrency —
`locks.go`; `cnagent.md` CN7), left to the unit tests.
