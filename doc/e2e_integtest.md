# e2e_integtest.md — the end-to-end integration suite (`integtest/e2e_test.sh`)

This document owns the intent of the end-to-end suite, `integtest/e2e_test.sh`:
what it proves, its topology and parameters, the rules E2E1 to E2E12, its
cases, preflight, cleanup, diagnostics and known limits. It leans on the
component documents for the behaviour under test — `architecture.md`,
`gateway.md`, `dnvctl.md`, `cnagent.md`, `dnagent.md`, `dnv-worker.md` and
`cdc.md` — and on `dnagent_integtest.md` and `cnagent_integtest.md` for the lab
facts the suites driving real agents share.

## Purpose — what the suite proves

One storage pool at the widest shape the tree builds, on real guests with no
fakes, driven end to end through the shipped operator CLI while two real kernel
NVMe hosts read and write its namespaces. A green run at the default shape is
six statements; a smaller shape proves no ceiling, and one without redundancy
no leg repair:

* **The ceiling is real.** `MaxSliceCntPerSp` slices with md-raid1, every side
  on a disk node of its own, every group's array and every slice's thin pool on
  one primary, and a create that commits on a real etcd the `CreateStoragePool`
  transaction `EtcdMaxTxnOps` is sized by, at two cntlrs rather than the widest
  shape's cntlr ceiling: a pool no other suite builds on real agents.
* **The operator surface works against a real control plane.** Every `Gateway`
  RPC but the cluster and node deletions and listings is issued by the
  shipped `dnvctl` against a real gateway, etcd and agents, and no echoed id
  is the whole of an assertion: a mutation is read back from its record and,
  where the device is the point, from its agent, or, for a few, proved by its
  effect, which is what the suite observes.
* **Data survives every operation.** A random pattern written through host0 at
  setup is re-read after every step a case marks and keeps its digest, through
  slice grows and the level ladder, failovers, a leg repair and all between.
* **The four reactions fire on real faults** (`dnv-worker.md` AR5 to AR8): a
  killed cn agent moves the primary role and then loses its cntlr, a killed dn
  agent with its nvmet port removed loses its leg to a spare, and strided
  writes that push a thin pool past its low water mark gain its slice a data
  group.
* **A primary cut off from the control plane fails over as a pause**
  (`architecture.md`, Failover; `dnv-worker.md` RW22). Its cn agent is
  stopped while every kernel object it built goes on serving host0, which
  runs nvme-stas against the cdc and writes in a loop: the failover lands
  within a bound set by its short primary threshold, the old primary's
  address leaves the discovery records (`architecture.md` [D18]), nvme-stas
  drops host0's path to it before the fence, so not one write fails, and the
  writes run again once the new primary has built its stack; continued, the
  old primary returns as a standby and is listed again.
* **The lab is left as it was found.** After every case every disk node has all
  its extents back, no node guest holds a dnv dm device, md array or nvmet
  subsystem, and allocation stays under a per-file and a whole-run cap; the
  cutoff case also stops nvme-stas on host0 and puts host0's own configuration
  of it back byte for byte.

**What it does not prove.** The RPCs it never issues, the deletions and
listings of clusters, disk nodes and controller nodes: the gateway suite covers
the node deletions, and this run discards the whole cluster between cases.
Error paths, beyond the few refusals the ops case asserts on purpose and the
`NOT_FOUND` reads after the copy case's deletions; validation and CLI semantics
are the gateway and dnvctl suites'. Concurrency: one dnvctl call and one case
at a time, and never a revision token, since a present token changes what the
gateway checks (`gateway.md` GW6; `dnvctl.md` CT3). ANA path selection between
two usable controllers (Known limits). A failover of a primary that still
answers: the two failovers the cases cause lose an agent that answers nothing,
one killed and one stopped, so neither shows a demotion hold that the old
primary's report ends early (`dnv-worker.md` RW22).

## Topology and parameters

Every guest comes from the command line (E2E1), each in one role. The
control-plane guest runs etcd, `dnv-gateway`, `dnv-worker`, `dnv-cdc` and
`dnvctl` as the login user. At least three controller-node guests run one
`dnv-agent cn` each: two carry the pool's cntlrs and the rest none, where the
ops case's third cntlr and the cntlr replacement land. The disk-node guests, at
least as many as a group has legs, run many `dnv-agent dn` instances each,
every one on its own loop device and its own nvmet port and service id, as two
ports cannot listen on one address, all registered at their guest's location
(E2E4). Two hosts run only the kernel's nvme-tcp stack and nvme-cli, but for
nvme-stas on host0 inside the cutoff case (E2E10).

The control plane has a guest of its own, as in a deployment
(`architecture.md`, System overview): it holds no kernel state and runs without
root, the react and cutoff cases' faults never reach the processes that must
observe and repair them, and its address, an IP literal as the cdc requires, is
what the hosts discover against. Every daemon log lives under a work directory
the between-cases rebuild removes, so each case's logs replace the last and a
passing run leaves none. Every node guest carries the suite's md assembly mask
while its agents run, installed before an agent starts: on a controller node
the stock udev rule would otherwise race the agent for an array it is creating,
and on a disk node it would assemble the superblocks the controller node writes
there (Known limits). The suite computes the NQNs the agents mint, mirroring
`common/name_fmt.go`, and never mints one; it renders ids from dnvctl's decimal
strings, never through shell arithmetic, as a cluster id can exceed the shell's
signed range; each namespace has an explicit uuid, so a host finds it by its
uuid link, the one name that survives a reconnect; and each host connects as
itself, under a host NQN distinct from the other's.

**Parameters.** The slice count, by default the ceiling; the redundancy,
md-raid1 or none, which sets the legs per group; the dn agents per disk-node
guest, by default the placement bound (E2E3); one case to run alone; and a
cleanup-only run that sweeps every guest and stops. Every other number derives
from these before a guest is touched, and a slice count `CreateStoragePool`
would refuse is refused at parse time.

**The placement bound.** A create puts every side on a disk node of its own and
a scan keeps one disk node per location (`architecture.md`, Per-operation
allocation, Finding DN candidates), so a group's legs come from distinct guests
and a pick fails once fewer guests than a group has legs hold an unpicked node;
hence many agents per guest. The bound is the least count per guest that rules
starvation out by counting, sized by the ops case rather than the create: after
the create and its own two grows, whose scans hand each guest its emptiest disk
node first and so take side-less nodes first (`architecture.md`, Finding DN
candidates), ops needs a side-less disk node on as many guests as a group has
legs, and guaranteeing that guarantees the create. The guarantee is sufficient,
not necessary: below the bound the proof is lost, not the run. Its headroom
also keeps the allocator's second tier out of reach (E2E4), and as one more
guest does not always lower the bound, a shape above the per-kernel cap is told
how many disk-node guests it needs.

**Event thresholds.** Only a pool's create sets its thresholds, so each case's
set is chosen just before its pool is built, and a case without one dies rather
than defaulting. smoke, ops and copy build under a quiet set longer than any
wait, a wide margin rather than a proof, as a failover or a spare leg
mid-operation would invalidate their absolute counts and digests; under it such
a reaction is a finding about the lab. react builds under the short reacting
set, as cntlr replacement and leg repair wait out thresholds whose defaults lie
beyond any wait the suite could afford (`dnv-worker.md` AR7, AR8). cutoff
builds under the cutoff set, as its failover must come within seconds of the
cut: its primary threshold is the shortest the gateway accepts on the suite's
clusters, derived from the tree's default check interval rather than typed
(E2E12). Its other three are the quiet set's, as nothing else may react while
the primary's agent is stopped: cntlr replacement must not replace the stopped
cntlr before the case continues it, leg repair has no fault to repair, and the
elected standby is held to the cntlr threshold while it settles
(`dnv-worker.md` AR5, HL2), so its own build cannot hand the role back. Every
set names all four, as an omitted or zero threshold resolves to its default
when read (`gateway.md` GW11; `dnv-worker.md` AR4) and the default primary
threshold is short; in every set the leg threshold exceeds the side threshold
and the primary threshold meets the gateway's floor (`architecture.md`, Common
validation). The driver checks every set's primary threshold against that
floor at preflight, so a set below it stops the run before anything is built,
not at its case's create.

## The rules

E2E1. **Servers come only from argv.** Every guest is named on the command line
and nothing is hardcoded: the role counts are enforced, a target given two
roles or a control-plane guest named by anything but an IPv4 address is a die,
and a run uses only the addresses it was given and the control-plane loopback
to etcd.

E2E2. **The control path is the shipped `dnvctl` only.** Every control-plane
read and write is the shipped `dnvctl`, never a `workerctl` or `gatewayctl`
write or a direct etcd write, one ssh to the control-plane guest per
invocation, its status and both streams framed apart: a success exits zero with
an empty stderr and one JSON document, a refusal with one stderr line naming
its code and trace id (`dnvctl.md` CT4, CT5). `workerctl` runs on the driver
only to print the tree's constants, `cnagentctl` is built for a host-id helper
no step calls, and etcd's client is shipped for read-only diagnostics and never
invoked.

E2E3. **The dn agents per disk-node guest default to the placement bound.** An
override below it is a warning naming the demand it risks, never an error; a
value above the per-kernel cap is a die naming the guest count the shape needs.

E2E4. **Every dn agent of a guest registers that guest as its location**, so
groups straddle guests and no two sides of a leg share a kernel — by headroom,
not by structure. Guest distinctness is asserted after the create and every
grow. A migration destination or a spare leg excludes the group's locations
only at the allocator's first tier (`architecture.md`, Per-operation
allocation), and beside its source the side export's NQN, which names no disk
node, would collide on one kernel (`architecture.md`, NQNs); so those
placements, a leg repair's included, are asserted guest-distinct only when
there are more disk-node guests than legs per group, the skip logged otherwise.

E2E5. **Sparse backing files, a Write Zeroes gate, and allocation caps.**
Backing files are created sparse but for the backing pattern, one random
pattern for the whole run, written before the agent starts at two places of
the first extent: where a data group's side placed there has its first data
block, and at the end of the extent. A loop device without Write Zeroes is
refused before its agent starts and again before the first pool create, which
also requires every agent's device on record: the dn agent only tags such a
disk (`dnagent.md` DN5), and side zeroing would write zero pages at bulk speed.
The gate guards speed, not space, as a loop device allocates what it zeroes
either way (`dnagent_integtest.md`, Assumptions and preflight checks). A meta
group's side is zeroed whole and a data group's side only over its first
blocks (`architecture.md`, Side provisioning protocol), so what a host or md
writes into a data group's side allocates too. The allocation caps, one per
backing file and one for the whole run at what the pool's own sides zero plus
slack, count both and the pattern, and are asserted after every case.

E2E6. **Cleanup runs unconditionally at the start, and only on success at the
end.** `cleanup_all` runs before anything is built, between cases, at the end
of a passing run and alone in a cleanup-only run, and never dies but reports: a
die would defeat a start sweep, and fail a passing run and skip the other
guests at the end. It tolerates absence, not an unfinished sweep: after the
start and the between-cases sweeps, `cleanup_start_gate` stops the run when any
verb never printed its sentinel, naming guest, verb and status. The end
cleanup's verdict is the exit status: a run whose end cleanup left a verb
unfinished or one of the suite's own nvmet ports standing fails, after
cleaning, while a run that fails an assertion removes nothing.

E2E7. **No direct-IO flags to dd.** The guests' dd mishandles direct IO
(`dnagent_integtest.md`, Assumptions and preflight checks), so every write is
buffered and synced and every read that must reach the media follows a cache
drop. Two host processes run detached, as their IO may block and `timeout`
cannot bound a task in uninterruptible sleep: the read-only level's read, which
answers "blocked" within its own budget, and the cutoff case's write loop,
whose writes are meant to queue through a failover. The loop reports through
files and keeps to its own namespace: it writes only to the device the
namespace's link named at its start, never creating that device node, and
checks before every write that the device still carries the namespace's uuid.
It stops on its stop file, with its work directory, or by itself after a bound
sized to outlive a passing run, so that a failed run left in place stops
writing. Every other host read, cache drop and device write, except the
read-only level's refused write, which dm-flakey fails rather than queues, runs
under a driver-side watchdog (`ssh_host_watched`) that abandons IO blocking
where nothing said it would, so the run dies with diagnostics, never hangs.

E2E8. **Pids in files, signals by pid, `pkill` only from helper files with
bracketed patterns.** Every agent and every control-plane daemon the suite
starts, etcd included, records its pid: the control-plane daemons are stopped
by theirs, the react case stops each agent it kills by that agent's pid file,
and the cutoff case stops and continues the agent it cuts off by its pid file,
with SIGSTOP and then SIGCONT, reading the process state back, as a signal is
delivered asynchronously. Every stop of an agent, by pid or by pattern, sends
SIGCONT before SIGTERM, so that an agent a failed cutoff run left stopped acts
on its termination. The cleanup's pattern sweeps, which also reach a crashed
run's processes whose pid files are gone, sit in helper files on the guest
behind helper verbs, bracketed so they never match the ssh command's own shell,
and qualified by the suite's work directory or its etcd name so they never
touch another suite's processes.

E2E9. **One dnv suite runs at a time in the lab** (`layout.md`, Directory
tree). The suite occupies every guest it names; no other suite binds its
ports. Nothing can enforce the rule from inside: the suite prints it before
the first ssh and checks what it can, that no port of its block listens and no
nvmet port it needs exists.

E2E10. **Hosts reach namespaces through the cdc, and every path a host holds is
the suite's own, but for nvme-stas's inside the cutoff case.** The kernel's
autoconnector is masked at preflight and at every rebuild and read back, and
nvme-stas must be inactive at preflight. The one exception is host0 inside the
cutoff case, whose subject is what a host running nvme-stas sees: there
nvme-stas runs on the cdc suite's settings (`cdc.md`, Integration test plan)
against this run's cdc alone, the autoconnector still masked, and host0 lets
go of its own paths before nvme-stas starts and gets them back once it stops,
so every path host0 holds meanwhile is one nvme-stas made from a discovery
record; host0's own nvme-stas configuration is copied before and put back
after. A host discovers through the cdc wherever the subsystem has a
`CdcEntry`, expecting one record per cntlr the listing rule lists
(`architecture.md` [D18]; `cdc.md` DS3). A step that needs every enabled cntlr
in the log first waits until each is listed, as a standby whose health epoch
is set is out of the records until its next clean round. Two connects are
direct by necessity: to the transfer, which has no `CdcEntry`; and to the copy
case's fallback source, where discovery would also bring in a namespace whose
uuid host1 already holds. After react's failover host0 reconnects through the
cdc, whose records leave the dead cntlr out: the failover's own transaction
rewrote them, and nothing clears the dead cntlr's health epoch. Every
connect the suite makes sits between an export gate and a connect verdict, as
the discovery log runs ahead of the data plane and nvme-cli can exit zero
having connected nothing.

E2E11. **Each case starts from an empty etcd and a freshly built pool.** An
etcd reset is not enough: a recreated cluster mints new cluster and disk-node
ids (`architecture.md`, cluster_id derivation), the dn agent refuses a disk
whose header names the old ones (`dnagent.md` DN5), and only the cleanup zeroes
the headers and removes the controller nodes' stores and arenas. So between
cases the suite runs the whole cleanup and its start gate, then the
infrastructure and the whole of setup, which clears every id the last case
read; that rebuild is also where the next case's thresholds take effect.

E2E12. **The shell literals mirror named constants and say so.** Every number a
Go constant owns is repeated beside a comment naming the constant, except five
read from `workerctl constants` at the driver's preflight: `EtcdMaxTxnOps`,
which etcd must be started with and which this suite, committing the
transaction it is sized by at two cntlrs, short of the widest shape, must not
hand-copy; `MaxAllocLegPerGrp`, cross-checked against the leg count;
`DefaultHealthCheckInterval`, the check interval every cluster of the suite
stores, from which the cutoff set's primary threshold is derived and against
which every set's primary threshold is checked; and `DefaultPrimaryUnhealthy`
and `DemotionHoldTimeout`, which the run only prints, the second beside the
cutoff case's timings. The meta region of the create's data groups, which the
tree computes rather than names, is mirrored the same way and checked against
the stored pool once it is created.

## The cases

The cases run in order, each on a fresh pool (E2E11): smoke, ops, copy, react
and cutoff. Each stage has its own trace id, sent by every dnvctl call of the
stage and carried by the gateway's records and an agent's records of the reads
the gateway makes for that call, never by the converge it sets off, which runs
under the worker's ids (`dnv-worker.md` RW10). Every wait on a condition is a
bounded poll, never a fixed sleep: a stack built from nothing gets the widest
budget, an increment a narrower one, and a reaction its threshold plus a few
passes.

**Setup** proves each layer before the next rests on it: the gateway serving;
the cluster storing an extent-size-only conf on the default bin ladder
(`gateway.md` GW11); every node registered, through the `ABORTED` a
not-yet-serving agent causes (`gateway.md` AG3), with its base rows OK and each
disk node on its own nvmet port (`dnagent.md` DN5, CM2; `cnagent.md` CN5); the
create read back field by field — each side on its own disk node, each group
across guests, the cntlrs on distinct nodes with one primary, and the slots
(`architecture.md`, cntlid slots); every side provisioned (`architecture.md`,
Side provisioning protocol); the primary's stack complete and the standby
holding legs with no group or pool (`cnagent.md` CN10, CN12, CN13), read from
the live shape so that a primary role moving under the wait is followed and
reported; a thin device with its raid0 (`dnv-worker.md` RW19; `cnagent.md`
CN15); and a namespace over it, under a subsystem admitting both hosts, that
host0 reaches through the cdc, the export gate and the connect verdict and
sees optimized through the primary and inaccessible through the standby
(`cnagent.md` CN16). Before any host writes to the pool, host0 reads the thin
device whole as zeros, which dm-thin returns for every block no host has
written (`architecture.md` [D15]), while each side read through its device on
its disk node shows how far its zeroing went: a data group's side reads zero
at its first data block and still holds the backing pattern at the end of its
extent, and a meta group's side reads zero there (`architecture.md`, Side
provisioning protocol; `dnagent.md` DN9). After a build whose reactions
changed its legs, which leaves a spare leg or a side count other than the
create's, a leg's side may not be the one the create placed over the pattern,
so the disk-node half is skipped and logged. The random pattern written
through the namespace then gives the digest every later step compares.

**smoke** has setup as its subject: the widest pool comes up whole, and
deleting it gives every extent back and leaves nothing behind, the two
statements the other cases assume. Its counts are absolute, so a moved side
count or any spare leg is a reaction under the quiet thresholds and a finding
about the lab. It runs first, so a lab that cannot build the shape fails after
one build.

**ops** proves the pool-scoped mutators and readers that setup, copy and the
ending do not exercise, with absolute counts and an unchanged digest: a meta
and a data grow (`architecture.md`, GrowSlice); the cntlid slots grown, and
refused wherever a cntlr's slot would go (`architecture.md`, cntlid slots); a
third cntlr's whole life — a standby with every leg, no stack and an
inaccessible namespace, refused deletion while enabled, out of the discovery
log once disabled, no usable path once deleted (`architecture.md`, Cntlrs);
the level ladder down to disabled and back, each rung awaited on the primary's
`applied_revision` (`dnv-worker.md` RW14) and showing exactly the suppressed
rows missing with the level marker (`ResDetailsSpLevel`) as their details
(`cnagent.md` CN19; `architecture.md`, SpLevel), the read-only rung reading
the data and keeping a write off the media (`cnagent.md` CN16's read-only
rule; `architecture.md`, [D11]); a snapshot and both bitmap reads, the thin
bitmap clear at the block setup wrote (`architecture.md`, Bitmap reads;
`cnagent.md` CN26, CN27); a namespace suspend that parks rather than removes,
and a repoint there and back (`architecture.md`, Namespace suspend semantics);
and a disabled disk node and controller node, invisible to the agents, each
proved by a refused allocation that writes nothing (`architecture.md`,
Capacity index keys).

**copy** proves the four RPC groups that move bytes. A transfer, auto-suspended,
parks the origin namespace and carries its identity, so host1, the only host
connected to it, reads the origin through it (`architecture.md`, Transfers;
`cnagent.md` CN17). A clone pulls the origin through the transfer into a fresh
thin device served live through the dm-clone (`cnagent.md` CN16's clone rule,
CN18); its unforced delete rests on the gateway's proof of hydration, and with
the transfer aborted the destination, read through its own raid0 with no
source left to read through, holds what hydration copied (`architecture.md`,
Transfer + clone = cross-SP live migration, [D3]); a source on the primary's
own kernel that fails routes to a second pool on another controller node
(Known limits). A migration moves
a side to a disk node outside its group, on a distinct cntlid slot, its leg
bitmap read and pushed only once the primary reaches the leg through the new
side, and finishes under the same leg id with an optimized path before any
read (`architecture.md`, Migration; `dnagent.md` DN12, DN13); a cancelled one
leaves its source untouched, the case relying on leg repair leaving a
two-sided leg alone (`dnv-worker.md` AR8). A spare leg (raid1 only) is
connected by both cntlrs and switched in, the replaced leg parked and then
deleted, md's rebuild onto it judged by the array's state words rather than
its OK status (`architecture.md`, Spare legs; `cnagent.md` CN10, CN12, CN28).

**react** proves the four reactions on real faults, each by the record it
writes, one action per pass making every wait a threshold plus a few passes
(`dnv-worker.md` AR2); its own build may have reacted, so each reaction is
judged by a delta from readings taken just before the trigger. Strided writes
sized from the primary's own pool usage push slice 0's thin pool past its
mark, and the worker must add exactly one data group and the pool grow on the
device (`dnv-worker.md` AR6); a primary that moves meanwhile stops the run.
The primary's cn agent is killed, host0 having let go first since a killed
agent's nvmet objects go on advertising optimized, and the standby must become
primary and build the stack, held to the cntlr threshold while settling
(`dnv-worker.md` AR5, HL2; `architecture.md`, Failover); a second failover
stops the run. host0 then reconnects through the cdc, whose records the
failover's own transaction rewrote without the dead cntlr
(`architecture.md` [D18]). The dead cntlr is replaced on a node that held
none, keeping its slot and role (`dnv-worker.md` AR7), and the dead agent,
restarted, removes what it left (`cnagent.md` CN7). Leg repair (raid1 only)
needs both planes, as kernel objects outlive their agent: a dn agent is killed
and its nvmet port removed, on a disk node with no other side of the pool, and
the group must gain a spare outside its nodes, park the dead leg and rebuild
md (`dnv-worker.md` AR8; `cnagent.md` CN11, CN28), with no failover
meanwhile, proved by an unchanged count of the worker's failover records.

**cutoff** proves that a primary cut off from the control plane fails over as
a pause for a host that follows the discovery log (`architecture.md`,
Failover; `architecture.md`, Host view). Its pool's primary must first have
settled, as a settling primary is held to the long cntlr threshold
(`dnv-worker.md` AR5, HL2), and both cntlrs must be listed, as nvme-stas
connects only what the log lists and the standby's path is the one that
carries host0 through the stall. host0 is then handed to nvme-stas (E2E10),
which must connect both cntlrs, and once a marker is written into host0's
kernel log a detached loop of small synced writes runs on a scratch namespace
(E2E7). The primary's cn agent is stopped, not killed (E2E8): it answers no
check round while every kernel object it built goes on serving host0, one of
the two cases the demotion hold serves and the one its wait is sized for
(`dnv-worker.md` RW22). Unlike in react, host0 is not disconnected by hand, as
taking its path away is the part of the cdc and of nvme-stas this case is
about. Writes must go on finishing between the stop and the failover, as the
stopped agent's kernel objects serve on until the fence: that is the positive
control of every zero-failure check, since a loop stalled before the cut would
pass them with nothing measured. The primary
role must move to the standby within a bound of the cutoff set's primary
threshold, the check rounds the worker needs to judge the stop and to act on
it, and the driver's polling slack, with one failover more in the worker's
records, naming the two (`dnv-worker.md` AR5, AR10); the old primary's address
must leave the discovery records, read through host1, which runs no nvme-stas
(`architecture.md` [D18]); nvme-stas must drop host0's path to it; and the
worker must record that the demotion hold ran to its timer, as the stopped
agent never reports its demotion applied. Not one write may fail while the
loop runs, by the loop's records and by the kernel log after the marker, and
that is what shows the path went before the fence: from the fence the old
primary answers writes on its path with errors (`architecture.md`, Known
limits). The writes must run again once the new primary has built its stack
and its namespaces have turned optimized on host0; from the stop until then
the driver does no device IO on host0, not even a cache drop, whose sync would
wait on the stalled head. Whether the kernel requeues the write in flight when
nvme-stas deletes the only optimized path, and whether the demotion hold outlasts
nvme-stas's delay, are measured by these assertions, not presumed: a failed
write fails the run no later than the driver's next reading of the loop's
records.

Continued, the old primary must converge to a standby, its health epoch clear
(`dnv-worker.md` HL2), its address return to the records and nvme-stas
reconnect it as an inaccessible path, with still one failover in all. Then the
loop stops and its range reads back as its pattern, nvme-stas stops and
host0's own configuration of it is back byte for byte, host0 reconnects
through the cdc, and the case deletes its scratch namespace and thin device.
So the ending finds only setup's subsystem, namespace and thin device, and
only the suite's own paths on host0, while the primary role stays on the cntlr
the failover elected.

**The ending every case shares** proves that a pool deletes back to nothing:
only setup's subsystem, namespace and thin device may remain before it; a
namespace deleted under live controllers loses its head disk; the subsystem
goes with no live controller under it, both hosts having let go first — a
controller left under a removed subsystem is killed with DNR while the port
carries another subsystem, and left retrying when that subsystem was the
port's last, as here (Known limits); the pool's delete latches, the drain's
end showing only as `NOT_FOUND` (`dnv-worker.md`, The sp drain); every disk
node then has its free extents equal its total with an empty side pointer
list; no node guest holds a dnv dm device, nvmet subsystem or md array, the
last proving on a disk node that its mask held; and the space guard holds —
each backing file's allocation, which bounds what its disk node zeroes and
what is written into its sides, since both allocate, the run's allocation at
what the pool's own sides zero and the backing pattern plus slack, and every
node's and the control-plane guest's free space at preflight's floors.

**What a pass means.** Exit status zero and PASS mean that every assertion of
every case held in a run that stops at its first failure, and that the end
cleanup then finished on every guest with none of the suite's own nvmet ports
left standing. Data is checked against a pattern the system never sees,
removals by the nodes' own listings and `NOT_FOUND`, reactions by the records
they write. A pass may have taken the clone fallback, whose branch is logged,
and a skipped check — a raid1-only step without redundancy, a
guest-distinctness check without headroom, setup's disk-node look at side
zeroing after a build whose reactions changed its legs — is a logged line, not
a proof.

## Preflight

The driver's checks run first; the guest checks run after the start cleanup and
its gate and before the first setup write, since port checks taken before the
cleanup would judge the last run's corpses, and the cleanup writes no suite
state; a cleanup-only run does neither. Preflight dies on the first failure,
naming the guest and the fix. It proves that the driver has its tools, the
binaries, the tree's constants (E2E12) and a pinned etcd release; that every
guest answers passwordless ssh; that a node guest has what the sweeps and the
agents need — sudo, the tools of both roles, the modules and the nvmet tree,
native multipath on a disk node too, where a migration destination reads ANA
through it, md support and a stock md assembly rule the mask can switch off
(`architecture.md`, Components: invocation reference), hole punching, memory
and free space, none of this run's ports listening, and no conflicting nvmet
port, as a configfs port does not listen until a subsystem is linked and an
agent would adopt and rewrite an existing port of its id (`dnagent.md` SH19);
that a host is a clean initiator — sudo, nvme-tcp and multipath, its identity
files, generated if absent and never overwritten, nvme-stas inactive and the
masked autoconnector (E2E10); that host0, when the cutoff case is in the run,
has the tools of the case's write loop and kernel-log witness and nvme-stas 2.x
installed, both its units known to systemd, as a 1.x one would fail later and
silently, never connecting, nvme-stas being a package the suite never
installs; and that the control-plane guest has its tools, its ports free and
the free-space floor the space guard holds it to, and needs no sudo: the
closing trim is the one root command tried there and may be refused. The loop
devices are checked once the agents have made them (E2E5).
The tool check covers only the sweeps after it: the start sweep runs before it
and a cleanup-only run skips it, so on a guest missing mdadm or udevadm their
md stop can silently leave dnv arrays standing.

## Cleanup, and why the order is what it is

The cleanup (E2E6) is best-effort and tells on itself: every verb prints a
sentinel when it finishes, and the sweep reports every verb that never did,
with the cause read off its status, and every nvmet port it refused or could
not remove, since a leftover port with live ANA groups fails the next suite's
setup. A verb's bound detects a wedge but cannot end a task in uninterruptible
sleep, which a removal or a scan against a suspended device creates, so every
node guest resumes suspended devices before it removes anything. The order
rests on one principle, a holder goes before what it holds, and on what this
lab does when it is broken. Hosts go before every target, as they hold the
controllers, and a subsystem removed under a live controller kills it with DNR
while its port keeps listening and leaves it retrying once the port has
nothing left (`cnagent_integtest.md`, Lab facts). On a host the cutoff case's
write loop stops first, so that nothing writes into a device while its paths
go, then nvme-stas, so that nothing reconnects what the sweep drops, the host's
own nvme-stas configuration going back while the work directory still holds
its copy and an nvme-stas the suite never started being left alone; then the
host drops the suite's and dnv's NQNs and its discovery controller, never
every controller, unmasks, and keeps its identity files, node identity rather
than run state, the dnv prefix also taking the cdc suite's subsystems on a
shared host (E2E9). A copy it cannot put back keeps the work directory, which
then holds the host's only copy of its files, and the host's verb ends without
its sentinel, saying where the copy is, so the sweep reports itself unfinished
and the next one tries again. A failed cutoff run, which can leave its loop
running, nvme-stas on the suite's settings and its agent stopped (E2E6), is
thus recovered by the next start cleanup or a cleanup-only run, whose stop
continues the agent first (E2E8). Every controller node goes before any disk
node, as its stacks sit on the disk nodes' sides, in the cn agent suite's order
(`cnagent_integtest.md`, Teardown and cleanup) and in two phases across all of
them, so that every clone final is gone, removed while its transfer source is
connected, before any transfer goes, as a dm-clone flushes through its source
(`cnagent.md` L3). On a disk-node guest every dnv md array goes before any dm
device, as an array assembled from the controller node's superblocks pins the
device under it (Known limits), each loop device's header is zeroed before it
is detached, and the mask goes once no device exposes a dnv superblock. The
control plane goes by recorded pids, the pattern sweep as fallback (E2E8), and
a best-effort filesystem trim on every guest closes.

md arrays are named by udev's recorded name, else by mdadm's export, never by
mdadm's scan, which prints no name on these guests; only dnv arrays are
stopped. The port drop refuses a port whose service id lies outside the suite's
band, naming its likely owner — the band is the disk nodes' on both roles, so on
a controller node, whose cleanup drops only the cn agent's own port id, a
stranger's port under that id bound inside the band is removed, not refused —
and removes one with no service id, the debris of an agent killed mid-creation,
which refusing would leave forever. A dm device whose name carries no
role-lettered kind is invisible to the cleanup and the residue check; the agent
suites' wipe clears it (`dnagent_integtest.md`, Teardown and cleanup).

## Diagnostics on failure

A run that fails an assertion dumps instead of cleaning, so everything stays in
place; a run failed by its end cleanup dumps what that cleanup left. The
control-plane guest goes first, as its dump holds the failing dnvctl call's
streams, which a later read would overwrite, and every dump is tolerant and
bounded, as a guest command hangs exactly when something is wrong. The context
names the stage, its trace id and the case, and the last ANA state, controller
and path state the probes read, which tell no path from a path without the
namespace and a live path from a retrying one; each guest's kernel, device,
nvmet and log state follows, a host's with its nvme-stas state, configuration
and journal and the cutoff case's write-loop records, and a controller node's
with its recorded agent's process state, which shows an agent the cutoff case
left stopped; the failure line ends with the filter that pulls the stage's
records. When the stage connected, the host's kernel log is the record of what
the connect attempted.

## Known limits

* **A multi-case run pays a full rebuild per case** (E2E11).
* **The clone source may fall back.** Whether one kernel can be initiator and
  target for the same bytes is the lab kernel's property, so a source that
  never connects or stops hydrating routes to a fallback pool on another
  controller node, which proves the same assertions against its own pattern.
* **A standby's namespaces are inaccessible, never non-optimized**
  (`cnagent.md` CN16), so a host holds one usable path per namespace, and ANA
  path selection across two usable controllers is not exercised.
* **The closing trim returns space to the host only where the guests' disks
  pass discards through**; elsewhere the host's free space shrinks by each
  run's real writes.
* **Without redundancy the suite is smaller**: ops' leg bitmap read, copy's
  spare stage and react's leg repair are skipped.
* **A disk node holds the controller node's md superblocks**
  (`architecture.md`, Components: invocation reference), as the primary's
  per-CN linear maps the side itself, and an unmasked disk-node guest's stock
  incremental assembly builds degraded, read-only arrays from them that pin the
  suite's devices, the side or that linear, which of the two is not
  established. Hence the mask on both roles, the disk-node md stop before any
  dm removal and the disk-node md residue check.
* **An array both inactive and over members mdadm cannot read is named by
  neither source**, so the cleanup skips it and an operator must find it.
* **A host controller outlives a target that stops listening**
  (`cnagent_integtest.md`, Lab facts), retrying until its loss timeout and
  pinning its namespace head through its hidden path device. So after a cntlr
  delete the suite asserts only that the path is not live, then disconnects the
  survivor by device; a wait after a disconnect, the suite's own or
  nvme-stas's, demands the path gone, which a wrong address also satisfies.
* **A blocked probe, abandoned host IO, or the write the cutoff case's loop had
  queued when it was stopped leaves an unkillable dd or sync behind** (E2E7)
  until its device or path serves IO again.
* **react's build may react.** At the widest shape a building primary cannot
  answer a health check within a short primary threshold, the default
  included, so the role can move and move back; a settling primary is
  therefore held to the cntlr threshold (`dnv-worker.md` AR5, HL2); the
  reacting set's cntlr threshold is still shorter than that build, so react's
  build can fail over and mint spares repeatedly, and whether it converges at
  that shape is not established.
* **The widest build is bounded by the primary's process spawning, not its
  memory**: fewer slices, or no redundancy, shorten it; whether more cores
  would is untested.
* **The grow race is absorbed, not removed.** The worker's sides-first hold
  (`dnv-worker.md` RW14) and the cn agent's in-pass connect retry (`cnagent.md`
  CN10) absorb a connect that reaches a grown group's side before its export
  exists, but an export linked later leaves the group unbuilt, and with it the
  primary unhealthy, until a later converge builds it; should that outlast the
  reacting set's primary threshold, the settled primary is failed over
  (`dnv-worker.md` AR5), and react then stops the run.
* **The cutoff case shows the margin of the path's drop over the fence by its
  outcome only.** nvme-stas's delay before it disconnects a withdrawn path and
  the demotion hold leave that margin (`dnv-worker.md` RW22); the case
  asserts that no write fails and logs when its polls saw the path gone, but
  times neither the drop nor the fence. On a host that drops the path after
  the fence, a write sent in between fails (`architecture.md`, Known limits),
  and with it the case.
* **The drop is asserted only on paths nvme-stas made.** host0 lets go of its
  own paths before nvme-stas starts, so a path nvme-stas took over is not
  exercised, and one it does not manage it never drops (`architecture.md`,
  Known limits).
* **dd's status alone does not prove a write landed.** The loop judges each
  write by dd's exit status, which rests on the guests' dd reporting a failed
  fsync, and nothing here proves that it does; so the kernel log after the
  case's marker is the second witness, and a marker the log no longer holds
  fails the case rather than passing it. The range's final digest shows that no
  write landed wrong bytes, not that each one landed, as every slot already
  holds what the loop writes there.
* **The write loop's uuid check and its write are two steps** (E2E7), so a
  device node reborn under the same name for another namespace in the instant
  between them could take one write; a node goes only with host0's last path
  to its namespace, which the case never expects.
* **The cutoff case's pause lasts the new primary's build.** A standby holds no
  group and no pool (`cnagent.md` CN12, CN13), so from the moment host0's path
  to the old primary goes, the loop's write in flight waits until the new
  primary has built the whole stack and turned its namespaces optimized, which
  at the widest shape takes minutes; the case bounds that wait only by the
  build's own and logs how long the write took.
* **nvme-stas keeps its connections when it stops.** nvme-stas 2.4.1, the
  lab's version, keeps the kernel connections it made when it stops, its
  connector always and its discovery daemon under persistent connections, so
  the cutoff case disconnects them itself before it hands host0 back, and a
  host's cleanup drops what a failed run left.
