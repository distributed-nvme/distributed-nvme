# e2e_integtest.md — the end-to-end integration suite (`integtest/e2e_test.sh`)

This document owns the intent of the end-to-end suite, `integtest/e2e_test.sh`:
what it proves, its topology and parameters, the rules E2E1 to E2E13, its
cases, preflight, cleanup, diagnostics and known limits. It leans on the
component documents for the behaviour under test — `architecture.md`,
`gateway.md`, `dnvctl.md`, `cnagent.md`, `dnagent.md`, `dnv-worker.md` and
`cdc.md` — and on `dnagent_integtest.md` and `cnagent_integtest.md` for the lab
facts the suites driving real agents share.

## Purpose — what the suite proves

One storage pool at the widest shape the tree builds, on real guests with no
fakes, driven end to end through the shipped operator CLI while two real kernel
NVMe hosts read and write its namespaces. A green run at the default shape is
twelve statements; a smaller shape proves no ceiling, and one without redundancy
no repair, no forced start, no take-over, no cut of a migrating leg's path,
no crash of a migration's destination, no crash of a disk node under writes
md has not flushed and no cut of a whole group:

* **The ceiling is real.** `MaxSliceCntPerSp` slices with md-raid1, every side
  on a disk node of its own, every group's array and every slice's thin pool on
  one primary, and a create that commits on a real etcd the `CreateStoragePool`
  transaction `EtcdMaxTxnOps` is sized by, at two cntlrs rather than the widest
  shape's cntlr ceiling: a pool no other suite builds on real agents.
* **A host's commands stay within what a controller node takes.** The pool
  names no stripe and stores the cluster's narrowed to its slice count, its
  stripe times its slice count within `MaxNvmeTcpCmdBytes` (`architecture.md`,
  Common validation, the stripe's product), and the optimal IO size and the
  largest request host0's block layer takes for the pool's namespace, its
  head device and every path device, and for a clone's namespace while the
  clone copies and after it (`architecture.md`, Clones), are within that
  bound, at every slice count the run builds.
* **The operator surface works against a real control plane.** Every
  `Gateway` RPC but the cluster and node deletions and listings, the
  listing of workers, the id step, the restore check and the capacity read
  of the layer above, `GetClusterCapacity`, is
  issued by the shipped `dnvctl` against a real gateway, etcd and agents,
  and no echoed id is the whole of an assertion: a mutation is read back
  from its record and, where the device is the point, from its agent, or,
  for a few, proved by its effect, which is what the suite observes.
* **Data survives every operation.** A random pattern written through host0 at
  setup is re-read after every step a case marks and keeps its digest, or,
  in the copy case, the digest read again once the clone's fio stamp has
  overwritten setup's pattern, all of it at two slices or more, through
  slice grows and the level ladder, failovers, the worker's leg repairs, an
  operator's repairs, one of which takes a retired leg back, a forced start
  of a group's array from the leg that holds its data, a restore of the
  group from the older copy a retired leg kept, which brings back what that
  leg holds, an operator's take-over of a group's repair from the worker,
  and all between; and fio stamps, data that fio writes and verifies, check
  block by block the stamped part of what a clone copies, read through its
  destination, the same part read through a transfer, the blocks a
  migration copies last, read through the moved leg's new side, and the
  blocks written to a migrating leg before its new side's whole guest is
  reset in the middle of the copy, read through that side once the copy is
  done (The cases, copy).
* **No write the host was told was done is lost under a procedure.** A
  stamped writer, fio passes of random overwrites each verified block by
  block with the seed of the write that put it there, runs on a volume of
  its own across an operator's repair while md copies, across a failover
  in the middle of a clone's copy, while the old primary still serves, and
  across the reset of the primary's machine, and fails its stage on a failed
  write or a block that does not read back as its last pass wrote it; a
  write to a new block followed by an fsync that completed before that
  reset reads back too (The cases, copy, react).
* **A clone's copy survives a failover of its primary** (`architecture.md`,
  Clone metadata durability). A primary disabled in the middle of a slowed
  copy hands the copy to the new primary, which loads the metadata the
  clone's clone meta group holds on disk nodes: it goes on from at least the
  count read before the failover, copies at most what that count left, and
  ends with a destination equal to the whole source.
* **The four reactions fire on real faults** (`dnv-worker.md` AR5 to AR8), on
  a pool of at most `REACT_SLICE_CAP` slices (Topology and parameters): a
  planned restart of the primary's cn agent moves nothing, as the agent
  answers that it is starting until its startup reconcile ends
  (`dnagent.md` SH33; `dnv-worker.md` HL8), a
  killed cn agent moves the primary role and then loses its cntlr, a killed dn
  agent with its nvmet port removed loses its leg to the worker's repair with
  a fresh leg, the dead leg kept as a retired leg, a group whose legs all lose
  their disk nodes gets no repair until one comes back, strided writes
  that push a thin pool past its low water mark gain its slice a data group,
  and a migrating leg whose serving disk node goes silent after the cutover
  is cut down to one path by the primary, so md fails its member while a
  host's writes go on finishing, and the worker finishes the migration and
  repairs the leg; and the primary's machine, reset by its kernel with no
  sync, loses the primary role and then its cntlr while host0 keeps its
  paths, writes on through the failover and reads its data through the new
  primary, and comes back with no cntlr of the pool.
* **A group md will not start comes back from one leg** (`architecture.md`,
  Repair; `cnagent.md` CN12), on react's pool, at the level where reactions
  stop. When the leg md holds in sync dies while md copies onto a repair's
  new leg, an operator's forced start from that leg, written while it is
  still dead, makes the primary stop the array then and, once the leg is
  back, zero the other leg and start the array from the named leg alone,
  and host0 reads its data unchanged; when that leg stays dead, a
  forced start from a retired leg a repair takes back starts the array from
  the older copy that leg kept, which host0 then reads; and a repair that
  takes back a retired leg holding newer data than the array is refused, not
  copied over (`dnv-worker.md` AR11). The worker clears each forced start's mark
  from the primary's report once the start is done (`dnv-worker.md` AR13).
* **An operator takes a group's repair over from the worker at the
  read-write level** (`architecture.md`, Repairs), on react's pool. With the
  group's `operator_in_charge` flag set, the worker's repair of a dead leg,
  held created, is aborted and cancelled, the worker creates no repair of the
  group while the flag stands (`dnv-worker.md` AR8), and the operator's own
  repair with a fresh leg is started, finished and copied whole; host0
  reads its data unchanged while the flag stands, with the group on one
  leg and again while md copies onto the operator's leg; with the
  flag cleared, the pool is still at the read-write level, host0 reads its
  data unchanged with no failed IO, and no failover happens.
* **A primary cut off from the control plane fails over as a pause**
  (`architecture.md`, Failover; `dnv-worker.md` RW22). Its cn agent is
  stopped while every kernel object it built goes on serving host0, which
  runs nvme-stas against the cdc and writes in a loop: the failover lands
  within a bound set by its short primary threshold, the old primary's
  address leaves the discovery records (`architecture.md` [D18]), nvme-stas
  drops host0's path to it before the fence, so not one write fails, and the
  writes run again once the new primary has built its stack; continued, the
  old primary returns as a standby and is listed again.
* **The data plane runs on without the control plane** (`architecture.md`,
  Running without the control plane). With host0's writes running, etcd is
  stopped until the worker has fenced itself and then started again: not
  one write fails, the cdc serves its last records meanwhile, the stored
  state reads the same before and after, and the worker rejoins and drives
  the pool again.
* **Every case but upgrade ends with its pool sound and the lab as it was
  found.** Before its teardown every such case's pool has every thin pool writable, without
  needs_check and with room, by the status read, and every array clean and
  whole on the primary, but for a pool a measuring stage left needs_check on
  purpose. After every such case every disk node has all
  its extents back, the free count of its own disk counts agreeing with its
  record's and the worker having logged no disagreement between the two
  (`dnv-worker.md` RW13), no node guest holds a dnv dm device, md array or
  nvmet subsystem, and allocation stays under a per-file and a whole-run
  cap, which the upgrade case, with no ending of its own, also holds; the
  cutoff case also stops nvme-stas on host0 and puts host0's own
  configuration of it back byte for byte.

**The opt-in cases** prove more, each only in a run that names it (The
cases):

* **A controller node that holds many pools fails over as a pause for every
  one of them** (`architecture.md`, Failover), the layout a CSI cluster has
  by default, where every volume is a pool of its own (`csi.md`, The volume
  model). With many one-slice RAID1 pools over the three controller nodes
  and both hosts writing every pool, every packet of the node that holds the
  most primaries is dropped: every pool whose primary it held fails over
  once and no other pool at all, not one write sent while the node is cut
  off fails, and both hosts see each moved pool optimized through its new
  primary within the measured bound (Known limits); once the node is back,
  and after the cn agent of the node that then holds the most primaries is
  stopped and continued in turn, every pool lists both cntlrs again and
  every write a host saw complete reads back.
* **A cluster of v0.1.0 upgrades in lockstep, and a backup taken under it
  restores with the new programs** (`architecture.md`, Upgrade in lockstep,
  etcd backup and restore). With v0.1.0's programs read running on every
  guest first, the agents stopped node by node and held stopped, and host1
  writing through the upgrade, no write fails, nothing reacts and every pool
  keeps its cntlrs, its primary and its sides, while every program then
  runs the new build. Restoring a backup taken under v0.1.0 keeps the
  untouched pool's data, breaks the pool grown after the backup as the
  design doc says, hands a new pool an id above the pool made after the
  backup and a new thin device an id above every id the grow after the
  backup took, and frees on the disk nodes every side made after it.
* **Nothing reacts while the operator's operations run, a fault past the
  worker's bounds gets the one reaction its rules give, and nothing leaks,
  over hours at the shipped thresholds** (`dnv-worker.md` AR4, RW8, HL7).
  Two pools that name no threshold, one RAID1 and one wide without
  redundancy, take a steady write while a cycle of grows, snapshots,
  migrations, disables and enables and namespace creates and deletes repeats
  with no reaction while its operations run, and slow
  faults come in turn, each with the outcome The cases, soak, states before
  the run; every generation of writes reads back, and no program's memory
  or open files and not etcd's size grow past the stated bound.

**What it does not prove.** The RPCs it never issues, the deletions of
clusters, disk nodes and controller nodes, the listings of disk nodes,
controller nodes and workers, and the capacity read `GetClusterCapacity`
(`architecture.md`, Clusters), and those a default run never issues, the
listing of clusters, the id step and the restore check
`CheckControllerNodeRestore` (`architecture.md`, Controller nodes), which
only the opt-in upgrade case does: the gateway suite issues each of them,
the dnvctl suite each against its fake gateway, and the csi suite reaches the
capacity read through dnv-csi and through `dnvctl`, whose answer it compares
with the driver's; this run discards the whole cluster between cases. Error
paths, beyond the few refusals the ops case asserts on purpose, the copy
case's refused abort and cancel of a started repair, react's refused forced
start of a group whose array runs and its refused repair that takes back a
retired leg newer than the array, the `NOT_FOUND` reads after the copy and
react cases' deletions and the read the cutoff case sees refused while etcd is
down; validation and CLI semantics are the gateway and dnvctl suites'.
Concurrency: one dnvctl call and one case at a time, many pools at once only
in the many-pools case, and never a revision token, since a present token
changes what the gateway checks (`gateway.md` GW6; `dnvctl.md` CT3). ANA path
selection between two usable controllers (Known limits). Auto-grow, cntlr
replacement, leg repair, a failover after the primary's cn agent is killed,
the forced start and the take-over of a group's repair, on a pool at the
ceiling (`dnv-worker.md` AR5 to AR8, AR13; `cnagent.md` CN12;
`architecture.md`, Repairs), as react's pool has at most `REACT_SLICE_CAP`
slices (Known limits); the cutoff case's failover, after that agent is
stopped, runs at the run's slice count. A demotion hold that the old primary's
report ends early (`dnv-worker.md` RW22): react's and cutoff's failovers lose
an agent that answers nothing, one killed and one stopped, and copy's disabled
primary answers, but the case only logs which of the two ended its hold, the
report or the timer. The worker's own try-to-abort of a repair of its own,
and its cancel of one, which react's take-over sees only when it comes
before the operator's (`dnv-worker.md`, Leg repair; `architecture.md`,
Repair); and the retire timer, as no run waits out a retired leg's age
(`dnv-worker.md` AR12). Guest distinctness of a repair's fresh new leg in
the react case at as many disk-node guests as a group has legs, where no
guest lies outside the group's, and for a leg repair whose fresh leg react's
steer pushes to the second tier (E2E4); and the destination of the copy
case's second migration, cancelled with force after its cutover, which the
case does not check at all. The default location and the pool create's first
preference (`architecture.md`, Disk nodes, Per-operation allocation): every
node passes its guest's role as its location, and no controller node shares
one with a disk node; the Go tests prove both, the gateway suite the first
preference, and the dn and cn suites the host name each agent reports. The
cut of a migrating leg's path while its source serves, and leg repair's
cancel of a migration whose cutover is pending and its wait on a copy not
shown whole (`cnagent.md` CN10; `dnv-worker.md` AR8): react's migrating leg
loses its destination after the cutover, with its copy whole.

## Topology and parameters

Every guest comes from the command line (E2E1), each in one role. The
control-plane guest runs etcd, `dnv-gateway`, `dnv-worker`, `dnv-cdc` and
`dnvctl` as the login user. At least three controller-node guests run one
`dnv-agent cn` each, registered at its guest's role as its location, the
default location being the host name, which a lab may share among its
guests (`architecture.md`, Controller nodes): two carry the pool's cntlrs and the
rest none, where the ops case's third cntlr and the cntlr replacements land.
The disk-node guests, at
least as many as a group has legs, and more than that in a run with the copy
case, which the suite refuses otherwise, run many `dnv-agent dn` instances
each, every one on its own loop device and its own nvmet port and service id,
as two ports cannot listen on one address, all registered at their guest's
location (E2E4). Two hosts run only the kernel's nvme-tcp stack and nvme-cli,
but for nvme-stas on host0 inside the cutoff case (E2E10). Every dnv daemon
of the tree's build runs at `--log-level debug`, so a failed run's logs hold
the reads a fault needs and the steps that read a converge's commands see its
reads too (`log.md` R6, R9); the upgrade case's v0.1.0 programs, which lack
the flag, log at their default level (The cases, upgrade).

The control plane has a guest of its own, as in a deployment
(`architecture.md`, System overview): it holds no kernel state and runs without
root, the faults the react and cutoff cases put on node guests never reach the
processes that must observe and repair them, and its address, an IP literal as
the cdc requires, is what the hosts discover against. Every daemon log lives
under a work directory the between-cases rebuild removes, so each case's logs
replace the last and a passing run keeps none of them, unless the run is asked
to keep its logs: then the driver copies the control plane's worker, gateway
and cdc logs and every log in the node guests' work directories into a
directory of its own before the between-cases cleanup and a passing run's end
cleanup remove them, and at the end of a failed run too, whose logs the next
run's start cleanup would remove, a copy that never fails the run. That copy is
what `integtest/round_counts.sh` reads: the worker's failed check rounds by
cause, the check streams it failed to open or to send a round on, its health
changes to unreachable and its late observations with the largest age, as no
stage of the suite asserts on missed rounds; the many-pools case alone runs
the script, over the worker's records of its build, and logs what it counts
(Known limits, The many-pools case's build fails over pools that have
built). Every node guest carries the udev
rules of `deploy/udev/` (`architecture.md`, Components: invocation reference)
while its agents of the tree's build run, installed before an agent starts from the texts the
guest's helper carries: on a controller node the stock udev rule would
otherwise race the agent for an array it is creating, and on a disk node it
would assemble the superblocks the controller node writes there (Known
limits). The suite computes the NQNs the
agents mint, mirroring `common/name_fmt.go`, and never mints one; it renders
ids from dnvctl's decimal strings, never through shell arithmetic, as a cluster
id can exceed the shell's signed range; each namespace has an explicit uuid, so
a host finds it by its uuid link, the one name that survives a reconnect; and
each host connects as itself, under a host NQN distinct from the other's.

**Parameters.** The slice count, by default the ceiling; the redundancy,
md-raid1 or none, which sets the legs per group; the dn agents per disk-node
guest, by default the placement bound, or the count of their own that the
many-pools case and the long run take (E2E3); one case to run alone, a
default case or an opt-in one (The cases); the many-pools case's pool count
and the long run's length, which each case alone takes; a
cleanup-only run that sweeps every guest and stops; and a directory on the
driver to keep the logs in. Every other number derives from these before a
guest is touched, and a slice count `CreateStoragePool` would refuse is refused
at parse time. The react case builds its pool at the slice count but at most
`REACT_SLICE_CAP`, a constant of the suite: a primary's build at the ceiling
holds the reads of its cntlr off (`dnagent.md` SH29, SH30), so the observation
its check rounds answer from ages past the worker's bound and its rounds count
as missed (`dnv-worker.md` HL7) for longer than react's primary and cntlr
thresholds, and the suite cannot shorten that hold (Known limits); and react's
strided writes need its pool to take the cluster's stripe whole, which a pool
does only while its slice count times that stripe is within
`MaxNvmeTcpCmdBytes` (`architecture.md`, Common validation), a product the
driver checks at preflight. The numbers that follow from the slice count are
worked out for react beside the run's, and the dn agents per disk-node guest
stay the run's. A thin device's size is a whole number of the suite's own
unit, `MaxDmRaid0StripeSize` per slice, which every stripe this suite's
pools store divides, as each is a power of two no larger
(`architecture.md`, Common validation, Geometry rules), so the sizes do
not depend on the stripe the pool takes and keep
the pool blocks per slice the copy case's skip needs (The cases, copy).

**The placement bound.** A create puts every side on a disk node of its own and
a scan keeps one disk node per location (`architecture.md`, Per-operation
allocation, Finding DN candidates), so a group's legs come from distinct guests
and a pick fails once fewer guests than a group has legs hold an unpicked node;
hence many agents per guest. The bound is the least count per guest that rules
starvation out by counting, sized by the ops case rather than the create: after
the create and its own two grows, whose scans hand each guest one of its
emptiest disk nodes first and so take side-less nodes first (`architecture.md`,
Finding DN candidates), ops needs a side-less disk node on as many guests as a
group has legs, and guaranteeing that guarantees the create. The guarantee is
sufficient, not necessary: below the bound the proof is lost, not the run. Its
headroom also keeps the allocator's second tier out of reach wherever there are
more disk-node guests than legs per group, which a run with the copy case
needs, save where react's steer leaves no enabled side-less disk node on a
guest outside the group's (E2E4), and as one more guest does not always lower
the bound, a value above the per-kernel cap dies naming what the shape
needs (E2E3).

**Event thresholds.** Only a pool's create sets its thresholds, so each case's
set is chosen just before its pool is built, and a case without one dies rather
than defaulting. smoke, ops and copy build under a quiet set longer than any
wait, a wide margin rather than a proof, as a failover or a repair of the
worker's they did not ask for would invalidate their absolute counts and the
steps they wait on, and copy's repairs need their dead legs left to the
operator, so that the worker's own repair cannot race an operator's create
(`dnv-worker.md` AR8). A leg md records bad-block ranges on can be a candidate
of that repair with no threshold, so copy holds that repair off the leg's
group with the group's `operator_in_charge` flag instead (The cases, copy).
Under the quiet set a failover or a repair the case did not ask for is a
finding about the lab, and copy's one failover is the disable it makes itself,
which waits out no threshold (`dnv-worker.md` AR5). react builds under the
short reacting set, as cntlr replacement and leg repair wait out thresholds whose defaults lie
beyond any wait the suite could afford (`dnv-worker.md` AR7, AR8). cutoff
builds under the cutoff set, as its failover must come within seconds of the
cut: its primary threshold is the shortest the gateway accepts on the suite's
clusters, derived from the tree's default check interval rather than typed
(E2E12). Its other three are the quiet set's, as nothing else may react while
the primary's agent is stopped: cntlr replacement must not replace the stopped
cntlr before the case continues it, leg repair has no fault to repair, and the
elected standby is held to the cntlr threshold while it settles
(`dnv-worker.md` AR5, HL2), so its own build cannot hand the role back.
The many-pools case builds under the cutoff set too, for the same reasons:
its cuts must fail every pool over within seconds, and nothing else may
react while a node is cut off. The
upgrade case's pools take the shipped thresholds, as their creates name none:
a worker that ran while a controller node's agent was stopped and its
standby's ran would then fail a primary over within the default primary
threshold, and the case stops those agents a gap apart that is longer than
that threshold and two check intervals, so the case's read of every pool's
cntlrs, primary and sides after the upgrade reads the order of the upgrade,
as the worker records the case reads are the new worker's alone (The cases,
upgrade). The long run builds under the
shipped set, which names none, as its subject is the shipped thresholds.
Every other
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
invocation, its status and both streams framed apart: a success exits zero
with an empty stderr and one JSON document, or, for the failure dump's status
read, its table (Diagnostics on failure), a refusal with one stderr line
naming its code and trace id (`dnvctl.md` CT4, CT5). `workerctl` runs on the
driver only to print the tree's constants, `cnagentctl` is built for a host-id
helper no step calls, and etcd's client is shipped for read-only diagnostics
and invoked by no case but the upgrade case, which takes etcd's backups with
it and restores one with etcd's restore tool, shipped for that case alone:
the backup and the restore are the operator's acts that case tests
(`architecture.md`, etcd backup and restore), not a write the suite makes in
place of `dnvctl`.

E2E3. **The dn agents per disk-node guest default to the placement bound.**
An override below it is a warning naming the demand it risks, never an
error; a value above the per-kernel cap is a die: an operator's value is
told to come under the cap or go, as no guest count makes it fit, and the
die names the guest count the shape needs when the shape's own bound is
above the cap, and the bound when it is within it. A run of the many-pools
case defaults to the count its pools' sides need, a third more extents than
they take on average, as the allocator spreads them over the guests
unevenly, taking first the disk nodes of the smallest bin of free space that
holds a side (`architecture.md`, Finding DN candidates), and never below the
bound; a run of the long run to the larger of the bound and its wide pool's
own, whose create puts every side, one a group, on a disk node of its own.

E2E4. **Every dn agent of a guest registers that guest as its location**, so
groups straddle guests and, with more disk-node guests than legs per group,
no two sides of a leg share a kernel — by headroom, not by structure. Guest
distinctness is asserted after the create and every grow. A migration
destination or a repair's fresh new leg, a leg repair's included, excludes
the group's locations only at the allocator's first tier (`architecture.md`,
Per-operation allocation), and a migration destination beside its source
would collide with it on the side export's NQN, which names no disk node
(`architecture.md`, NQNs). So a run with the copy case, which migrates a
side and, under md-raid1, repairs a leg, is refused before it builds
anything or touches a guest unless it has more disk-node guests than legs
per group, and the copy case asserts its first migration's destination and
its fresh-leg repair guest-distinct. The react case's leg repairs are
asserted so only with more disk-node guests than legs, the skip logged
otherwise, and a leg repair whose fresh leg react steers onto a side-less
disk node (The cases, react) only while the steer leaves such a node enabled
on a guest outside the group's, since the steer can itself push the pick to
the second tier; the skip is logged then too. React's migrating leg, which
migrates a side, runs only with more disk-node guests than legs per group,
skipped and logged otherwise, and steers its destination onto a side-less
disk node outside the group's guests, failing when the steer leaves none
there.

E2E5. **Sparse backing files and allocation caps.**
Backing files are created sparse but for the backing pattern, one random
pattern for the whole run, written before the agent starts at two places of
the first extent: where a data group's side placed there has its first data
block, and at the end of the extent. A loop device allocates what it zeroes
(`dnagent_integtest.md`, Assumptions and preflight checks). A side of a
slice's meta group is zeroed whole, and a data group's side and a clone meta
group's side only over their first blocks (`architecture.md`, Side
provisioning protocol), so what a host or md writes into a data group's side
allocates too. The allocation caps, one per backing file and one for the
whole run at what the pool's own sides zero plus slack, a part of which
grows with the disk node count for the agents' logs, count both and the
pattern, and are asserted after every case; a case's run cap also takes, on
every leg, some of what its stages write on purpose: the copy case's fio
stamps and its first clone's copy of its source and the range of every
stamped writer, each sized in the suite's thin-device unit and so growing
with the slices, the blocks of the react case's flush and group-cut writers,
a pool block a write, and the ops case's files with a fixed
allowance for its file system's tables; each stays allocated until the case
ends, as a thin delete hands no block back to a backing file. The slack
holds the rest a case writes, react's chunks across the grow mark and the
cutoff case's writers among them, and what the ops case's file system takes
past that allowance at more slices (Known limits, The new data checks ran at
one slice). The many-pools case's disk nodes each carry sides of many
pools, and a guest's disk nodes fill one at a time once each has few
extents left (`architecture.md`, Finding DN candidates), so each of its
backing files has a cap of its own where that is above the default cases'
per-file cap: an extent for each side its disk node
carried at the case's widest, as a meta side is zeroed whole and a data
side holds all a host writes into it, beside the pattern and the agent's
metadata; its run cap counts every pool's sides, both hosts' writes into
every pool and the logs every pool's objects add at debug. These caps guard
space only: a whole extent for every side does not hold a data side's
zeroing to its region, which the default cases' caps do. A pool count whose
run cap would leave less than a margin free on the file system of the lab's
image directory on the driver, where the lab keeps the guests' disk images,
which grow by what the guests allocate, is refused before a guest is
touched. The guard reads that directory's file system whether or not the
driver keeps its guests' images there, and a driver without that directory
is not checked.
The long run moves, grows and copies for hours, and what it zeroes and
writes lands on any disk node, so its files are held to their own size and
its run cap counts each cycle's migration, grow, snapshot and namespace,
both hosts' writes and the hours of logs.

E2E6. **Cleanup runs unconditionally at the start, and only on success at the
end.** `cleanup_all` runs before anything is built, between cases, at the end
of a passing run and alone in a cleanup-only run, and never dies but reports: a
die would defeat a start sweep, and fail a passing run and skip the other
guests at the end. It tolerates absence, not an unfinished sweep: after the
start and the between-cases sweeps, `cleanup_start_gate` stops the run when any
verb never printed its sentinel, naming guest, verb and status. The end
cleanup's verdict is the exit status: a run whose end cleanup left a verb
unfinished or one of the suite's own nvmet ports standing fails, after
cleaning. A run that SIGTERM, SIGINT or SIGHUP stops before its exit handler
exits non-zero and logs no PASS. A run that fails an assertion or is stopped
so removes nothing but the copy
case's two token buckets, its clone's off the controller node and its
migration's off the disk node that carries it, the react case's reject rule,
its group cut and its flush stage's delay, which it takes off the controller
node that carries them, the react case's silencing rule, which it takes off
the disk node that carries it, the many-pools case's cut, which it takes off
the controller node it cuts off, and the long run's slow fault, its delay,
loss or CPU cap off the controller node and its token bucket off the disk node
that carries it, as a slowed or delayed port would slow, a refused one fail
and a silenced or cut one, or a cut node, stall every suite that runs before
the next cleanup, the many-pools case's cut also removing itself on a timer;
and it tells host0's stamped writer to start no more passes, as its passes
would write on into a lab other suites then use. These removals, like a
passing run's end cleanup, are best effort: a signal while the exit handler
runs, a dead terminal or tee or a full disk can end them early. After a
stopped or failed run, run the suite with `--cleanup-only`, as every lab
chain does.

E2E7. **No direct-IO flags to dd.** The guests' dd mishandles direct IO
(`dnagent_integtest.md`, Assumptions and preflight checks), so every dd
write is buffered and synced and every dd read that must reach the media
follows a cache drop. Seven host processes run detached, as their IO may
block and `timeout` cannot bound a task in uninterruptible sleep: the
read-only level's read, which answers "blocked" within its own budget; the
write loop, whose writes are meant to queue through a failover in the cutoff
case, and in react's migrating leg on md until the primary cuts the leg's
path; the fence writer on host1, of the cutoff case and of the upgrade
case, which stops on its first failed write or at the end of its range; the
stamped writer, whose passes run across a procedure on host0 until its stop
file, its first failed pass or a time bound sized to outlive a passing run, a
pass in hand ending before it stops; the react case's writer to new blocks,
each of whose jobs ends at the end of its writes or at its own first failed
one, and the writer once every job has ended; and the many-pools case's
writers, one fio on each host with a job per pool, whose writes are meant to
wait through its cuts and which stop on a signal, once the case has read them,
as the long run's writers, the same fio, stop at the end of each cycle, and
the many-pools case's path sampler, which stops on its stop file or by itself
after a bound. The fence writer, the stamped writer and the many-pools case's
writers run fio, not dd, and write and verify with direct IO (The cases), and
so do the copy case's fio stamps, each written or read back by one fio run
under the watchdog below, as is each verify the detached writers' stages read
after them. The writer to new blocks is a python program of the suite's own,
as fio's latency logs carry no fsync: it writes with direct IO and logs the
time around each write and each fsync it makes, and its blocks are read back
with direct IO under the same watchdog. The loop reports through files and
keeps to its own namespace: it writes only to the device the namespace's link
named at its start, never creating that device node, and checks before every
write that the device still carries the namespace's uuid. It stops on its stop
file, with its work directory, or by itself after a bound sized to outlive a
passing run, so that a failed run left in place stops writing. Every other
host read, cache drop and device write, except the read-only level's refused
write, which dm-flakey fails rather than queues, runs under a driver-side
watchdog (`ssh_host_watched`) that abandons IO blocking where nothing said it
would, so the run dies with diagnostics, never hangs.

E2E8. **Pids in files, signals by pid, `pkill` only from helper files with
bracketed patterns.** Every agent and every control-plane daemon the suite
starts, etcd included, records its pid: the control-plane daemons are stopped
by theirs, the react case stops each agent it kills by that agent's pid file,
and the cutoff case and the many-pools case's second cut stop and continue
the agent they cut off by its pid file, with SIGSTOP and then SIGCONT,
reading the process state back, as a signal is delivered asynchronously,
and the cutoff case stops etcd by its pid file, waiting for it to exit,
before it starts it again on the same data. Every stop of an agent, by
pid or by pattern, sends SIGCONT before SIGTERM, so that an agent a failed
cutoff run left stopped acts on its termination. The cleanup's pattern
sweeps, which also reach a crashed run's processes whose pid files are gone,
sit in helper files on the guest
behind helper verbs, bracketed so they never match the ssh command's own shell,
and qualified by the suite's work directory or its etcd name or, for the fio
of the cutoff case's writer, of a fio stamp and of the stamped writer, by a
job name only this suite uses, and for the writer to new blocks by its own
name word and its directory under the suite's work directory, so they never
touch another suite's processes.

E2E9. **One dnv suite runs at a time in the lab** (`layout.md`, Directory
tree). The suite occupies every guest it names; no other suite binds its
ports. Nothing can enforce the rule from inside: the suite prints it before
the first ssh and checks what it can, that no port of its block listens and no
nvmet port it needs exists.

E2E10. **Hosts reach namespaces through the cdc, and every path a host holds is
the suite's own, but for nvme-stas's inside the cutoff case.** nvme-cli's
autoconnector is masked at preflight and at every rebuild and read back, and
nvme-stas must not be running or starting at preflight. The one exception is host0 inside the
cutoff case, whose subject is what a host running nvme-stas sees: there
nvme-stas runs on the cdc suite's settings (`cdc.md`, Integration test plan)
against this run's cdc alone, the autoconnector still masked, and host0 lets
go of its own paths before nvme-stas starts and gets them back once it stops,
so every path host0 holds meanwhile is one nvme-stas made from a discovery
record; host0's own nvme-stas configuration is copied before and put back
after. A host discovers through the cdc wherever the subsystem has a
`CdcEntry`, expecting one subsystem record per cntlr the listing rule lists
(`architecture.md` [D18]; `cdc.md` DS3), behind the cdc's record of itself
(`cdc.md` DS12). A step that needs every enabled cntlr
in the log first waits until each is listed, as a standby whose health epoch
is set is out of the records until its next clean round. Three connects
are direct by necessity: to the transfer, which has no `CdcEntry`; to the
copy case's second pool, the fallback's source and the source of the
failover in the middle of a copy, where discovery would also connect host1
to the case's own subsystem, which no other step gives host1 a path to,
and for the fallback would bring in a namespace whose uuid host1 already
holds; and the cutoff case's fence writer on host1, first to the old
primary's transport alone and then to the new primary's, where discovery
would connect both cntlrs and the writer must hold the old primary's path
alone. After
react's failover host0 reconnects through the cdc, whose records leave the
dead cntlr out: the failover's own transaction rewrote them, and nothing
clears the dead cntlr's health epoch. Every connect the suite makes sits
between an export gate and a connect verdict, as the discovery log runs ahead
of the data plane and nvme-cli can exit zero having connected nothing.

E2E11. **Each case starts from an empty etcd and a freshly built pool.** An
etcd reset is not enough: a recreated cluster mints new cluster and disk-node
ids (`architecture.md`, cluster_id derivation), the dn agent refuses a disk
whose header names the old ones (`dnagent.md` DN5), and only the cleanup zeroes
the headers and removes the controller nodes' stores. So between
cases the suite runs the whole cleanup and its start gate, then the
infrastructure and the whole of setup, which clears every id the last case
read; that rebuild is also where the next case's thresholds take effect.

E2E12. **The shell literals mirror named constants and say so.** Every number a
Go constant owns is repeated beside a comment naming the constant, except ten
read from `workerctl constants` at the driver's preflight: `EtcdMaxTxnOps` and
`EtcdMaxRequestBytes`, which etcd must be started with and which this suite,
committing the transaction the first is sized by at two cntlrs, short of the
widest shape, must not hand-copy; `MaxAllocLegPerGrp`, cross-checked against
the leg count; `DefaultHealthCheckInterval`, the check interval every cluster
of the suite stores, from which the cutoff set's primary threshold is derived
and against which every set's primary threshold is checked;
`DefaultPrimaryUnhealthy`, the primary threshold the long run's pools
resolve to, which the run prints and holds to the floor every set's
primary threshold is held to; `DemotionHoldTimeout`,
which the run prints beside the cutoff case's timings and from which that
case computes its wait for its writer's failed write and the least time the
writer's range must last; `DefaultStartHoldSeconds`, the start hold every
cluster of the suite stores, as none names one at its create
(`dnv-worker.md` HL8), which bounds the react case's wait for a restarted
agent's start; `DefaultNvmeFastIoFailTmo`, the fast IO fail timeout of the
legs of a pool that sets none, as no pool of the suite does, which the
react case's waits on a dead leg take unless the pool's stored setting
says otherwise; `DefaultStripeSize`, the stripe every cluster of
the suite stores, which the react case's stripe also mirrors, the preflight
holding that copy to the value read; and `MaxNvmeTcpCmdBytes`, which the
pool's stripe times its slice count and the IO sizes host0 reads are held
to. The meta region of the
create's data groups, which the tree computes rather than names, is mirrored
the same way and checked against the stored pool once it is created; the
pool's stripe, which its create does not name, is read back and held to the
narrowing's rule by its properties rather than by a copy of the arithmetic.

E2E13. **A crash fails the case.** As each case ends, before the cleanup
between cases removes the logs, the suite reads every log under the work
directory of the control-plane guest and of each controller node and disk
node guest, the guests that run a dnv program, and a line that starts as Go's
report of a crash starts, `panic: ` or `fatal error: `, fails the case with
the guest, the log and the line, while a record that holds those words inside
a field never matches. The preflight proves the read on a sample log it plants
on the control-plane guest, read as the plain user, and on a disk node guest,
read through sudo. A crash in the cleanup's own stop of a program, between
cases or at the end of a passing run, is not read, as the cleanup removes the
logs with the programs; a crash of `dnvctl` is in no log but in its call's
exit status and stderr, which E2E2 frames apart.

## The cases

The cases run in order, each on a fresh pool (E2E11): smoke, ops, copy, react
and cutoff. An opt-in case runs only when the run names it as the one case to
run alone, and never in that order, as it holds the lab far longer than a
default run or needs parameters of its own. Each stage has its own trace id, sent by every dnvctl call of the
stage and carried by the gateway's records and an agent's records of the reads
the gateway makes for that call, never by the converge it sets off, which runs
under the worker's ids (`dnv-worker.md` RW10). Every wait on a condition is a
bounded poll, never a fixed sleep: a stack built from nothing gets the widest
budget, an increment a narrower one, and a reaction its threshold plus a few
passes. A read that a negative rests on, a node's listing of its paths, a
host's look for a namespace's device and the look for a control-plane
daemon's exit, fails its poll or its assertion when it did not answer,
through ssh, sudo or the tool itself, as a failed read taken for an absence
would pass every negative built on it.

**Setup** proves each layer before the next rests on it: the gateway serving;
the cluster storing an extent-size-only conf on the default bin ladder
(`gateway.md` GW11); every node registered, through the `ABORTED` a
not-yet-serving agent causes (`gateway.md` AG3), with its base rows and its
store row OK and each disk node on its own nvmet port (`dnagent.md` DN5, CM2;
`cnagent.md` CN5); the create read back field by field — each side on its own
disk node, each group across guests, the cntlrs on distinct nodes with one
primary, the slots (`architecture.md`, cntlid slots), and the stripe, the
cluster's narrowed to the slice count, its product with the slice count
within `MaxNvmeTcpCmdBytes` (`architecture.md`, Common validation); every side
provisioned (`architecture.md`, Side provisioning protocol); the primary's
stack complete and the standby holding legs with no group or pool (`cnagent.md`
CN10, CN12, CN13), read from the live shape so that a primary role moving under
the wait is followed and reported; a thin device with its raid0
(`dnv-worker.md` RW19; `cnagent.md` CN15); and a namespace over it, under a
subsystem admitting both hosts, that host0 reaches through the cdc, the export
gate and the connect verdict and sees optimized through the primary and
inaccessible through the standby (`cnagent.md` CN16), host0's block layer
taking an optimal IO size and a largest request within `MaxNvmeTcpCmdBytes` for
the namespace's head device and both path devices. Before any host writes to
the pool, host0 reads the thin device whole as zeros, which dm-thin returns for
every block no host has written (`architecture.md` [D15]), while each side read
through its device on its disk node shows how far its zeroing went: a data
group's side reads zero at its first data block and still holds the backing
pattern at the end of its extent, and a meta group's side reads zero there
(`architecture.md`, Side provisioning protocol; `dnagent.md` DN9). After a
build whose reactions changed its legs, which leaves a repair, a retired leg or
a side count other than the create's, a leg's side may not be the one the
create placed over the pattern, so the disk-node half is skipped and logged.
The random pattern written through the namespace then gives the digest every
later step compares, and the pool's space read (`architecture.md`, Storage
pools, InspectStoragePool) then shows every slice read from the primary's
report, at least the pattern's bytes in use, and the thin device's size as
the pool's virtual size.

**smoke** has setup as its subject: the widest pool comes up whole, and
deleting it gives every extent back and leaves nothing behind, the two
statements the other cases assume. Its counts are absolute, so a moved side
count, or any repair or retired leg, is a reaction under the quiet thresholds
and a finding about the lab. Before the teardown it judges the default log
level by the level field every record carries, with no run at that log level:
in an idle window of five check intervals, the primary's cn agent logs no
check round message, read, command, dial or leg probe above Debug, and the
worker no round message or etcd read above Debug, while both log round
messages at Debug (`log.md` R6, R9; `grpc.md` L6); the bytes of the window
and of its records above Debug, what the default log level would keep, are
logged and not asserted. It runs first, so a lab that cannot build the
shape fails after one build.

**ops** proves the pool-scoped mutators and readers that setup, copy and the
ending do not exercise, with absolute counts and an unchanged digest: a meta
and a data grow (`architecture.md`, GrowSlice); the cntlid slots grown, and
refused wherever a cntlr's slot would go (`architecture.md`, cntlid slots); a
third cntlr's whole life — a standby with every leg, no stack and an
inaccessible namespace, refused deletion while enabled, out of the discovery
log once disabled, no usable path once deleted (`architecture.md`, Cntlrs);
the level ladder down to disabled and back, each rung awaited on the primary's
`applied_revision` (`dnv-worker.md` RW14) and showing exactly the suppressed
rows missing, each with the level's reason and the marker "sp_level" as its
details (`cnagent.md` CN19; `architecture.md`, SpLevel), the read-only rung
reading the data and keeping a write off the media (`cnagent.md` CN16's
read-only rule; `architecture.md`, [D11]); a snapshot and a bitmap read,
refused for the volume a host's namespace serves and answered for the
snapshot, which no namespace exports, its bitmap clear at the block setup
wrote (`architecture.md`, Bitmap reads; `cnagent.md` CN26); a namespace
suspend that parks rather than
removes, and a repoint to a new volume, whose zeros the host reads, to
the snapshot, whose data, setup's pattern, the host then reads and over
whose first MiB it writes zeros and reads them back, and back to the
volume, which still reads setup's pattern whole, so that the write went to
the snapshot alone (`architecture.md`, Namespace suspend semantics); a disabled disk node and controller node, invisible to the
agents, each proved by a refused allocation that writes nothing
(`architecture.md`, Capacity index keys); and last a host's fstrim: an ext4
file system on a volume of its own, its files written and removed, then
`fstrim`, after which the pool's used bytes, read from `sp inspect`, must
drop by the files' whole blocks where the pool's stripe is its block, and
are logged where the stripe is below it, as a discard frees only the
blocks it covers whole on their slice (`architecture.md`, Known limits).

**copy** proves the four RPC groups that move bytes — transfer, clone,
migration and an operator's repairs — and that a clone's copy survives a
failover, each judged by a record or a device read back. A clone and a
migration are also judged block by block, by fio stamps: data fio writes
through host0 with its own verify headers, each block carrying its offset
and a checksum of its content, in requests within `MaxNvmeTcpCmdBytes`, and
reads back whole with fio's verify, through any device that keeps the
offsets. Before the transfer host0 writes a fio stamp over the first half of
the origin's thin device and reads it back. The case then reads the digest
again and holds every later read to it in place of setup's, as the fio stamp
overwrites setup's pattern: all of it at two slices or more, and its first
half at one. The device's second half stays as setup left it: never written
at two slices or more, and at one slice setup's pattern, which there fills
the whole device, so that at one slice no part of the device is left
unwritten and the digest covers all of it. A transfer, auto-suspended, parks
the origin namespace and carries its identity, so host1, the only host
connected to it, reads the origin through it (`architecture.md`, Transfers;
`cnagent.md` CN17), the digest and the fio stamp with fio's verify. Before the clone is created the origin's bitmap is read,
slice by slice, once the primary reports the origin parked, a wait the case
polls, for every source slice one destination region spans, and right after
the create those bitmaps are appended as the clone's chunks, one per slice,
so that a region a narrowed stripe spreads over several slices can still be
skipped (`architecture.md`, raid0 bitmap math). The reads need no clone, so
they all come before the create and the appends follow it at once, as a
small copy can be whole within seconds of the create. The worker latches the
clone once its copy is whole (`dnv-worker.md` CLD13), and a latched clone
refuses a write (`dnv-worker.md` CLD1), so one of the case's writes to the
clone, a chunk or its transport sent again, can be refused. The case accepts
such a refusal only with the clone gone and one more latch in the worker's
log, logs it, sends the clone nothing more and waits for no hydration:
either refusal leaves the transport sent again untested, and a refused chunk
the skip too. A chunk the gateway accepts reaches the clone only through the
worker's push (`dnv-worker.md` BM2), and dm-clone drops a discard of a
region it has copied (`cnagent.md` CN18), so a chunk pushed once the copy is
whole lets the clone skip nothing, which no step checks or logs. The
fallback's source, a namespace the clone reads through and so never parked,
is refused its read and appends none (`architecture.md`, Bitmap reads). A
clone pulls the origin through the transfer into a fresh thin device, whose
namespace the case creates once the copy is whole, through the dm-clone or
the raid0 as the worker's pass falls (`cnagent.md` CN16's clone rule, CN18),
host0's IO sizes for it within `MaxNvmeTcpCmdBytes` then and again once the
clone is gone (`architecture.md`, Clones); once the primary reports the copy
whole the worker latches the clone and drains it with no operator's delete,
its log holding the latch (`dnv-worker.md` CLD13), the destination namespace
staying optimized, and with the transfer aborted the destination, read
through its own raid0 with no source left to read through, holds what
hydration copied (`architecture.md`, Transfer + clone = cross-SP live
migration, [D3]): when the origin is the source, the destination reads the
fio stamp back with fio's verify, and the rest of the device, never written,
as zeros, the regions the appended bitmaps let the clone skip among them; a
source on the primary's own kernel that fails routes to a second pool on
another controller node (Known limits), whose source carries no fio stamp
and is held to its digest alone; and whichever the source, the origin then
reads the fio stamp back again. Before the first migration host0 writes a
fio stamp over the whole of a thin device of its own, exported as a
namespace of its own, and reads it back, once the primary's report of slice
0's pool shows room for the stamp's blocks under the mark of auto-grow
(`dnv-worker.md` AR6), as a grow would change every later count: thin
allocates the stamp's blocks after the origin's, so the leg's copy reaches
them last, and a thin device and namespace of its own keep the case's digest
and every later stage as they were. Over the lab's network a leg's copy is
whole before the worker's bitmap pieces reach the new side, so no piece
would keep a region from being copied; so before the create the source
side's disk node gets a token bucket on what that side's nvmet port sends to
the other disk-node guests, among which the new side lies, at a suite rate
of about a second a region, which leaves the controller nodes' paths to the
side alone. The new side reads a region it has not copied yet from the old
one, for its copy and for any other read of the dm-clone alike. On a node
without the dm rule of `deploy/udev/`, udev reads the dm-clone and each
device its cutover pass puts over it, and the device-mapper command that put
a device there waits for those reads (`cnagent_integtest.md`, Lab facts);
the suite installs that rule, which keeps udev's probe off every dnv dm
device (`architecture.md`, Components: invocation reference), and whether
any read of the pass still waits so with the rule in place is not measured.
Under the bucket a fair queue serves each connection in turn, so such a read
waits only behind a region in flight on its own connection; that wait can
still stop the cutover's pass and fail the case through the gap check below
(Known limits). A migration moves a side to a disk node outside its group,
on a distinct cntlid slot; its cutover comes only once the primary holds a
live path to the new side, and the worker's record of it names the primary
and a reply of the primary's at a revision no older than the new side's
provisioned flip (`dnv-worker.md` RW18). The case prints the migration gap,
read from both dn agents' logs — from the old side's first write of the
inaccessible group to the primary's export of the leg to the new side's
write of the optimized group — with no time limit on it, and fails when
either write failed or when the new side's does not come from its cutover
request: that request's pass waits for the old side's own pass only as long
as its connect budget (`dnagent.md` DN13), so an old side's pass that
outlasts the budget, or any other step of the new side's pass that stops,
leaves the gap to a later pass and fails the case. With raid1 the case fails
the moving leg's md member on the primary by hand — the drop md's failfast
IO makes when it meets the old side just after its export turns inaccessible
(`architecture.md`, Known limits), which no run can force — after the gap
check, so that the syncup the drop brings lies outside the window that check
reads, and, as any converge of the primary puts the member back, with the
primary at the pool's latest revision and, as the case checks, running no
converge of its own (`cnagent.md` CN10), so that the converge that puts the
member back is the re-send the naming verdict brings; the member must come
back in sync with its failfast flag and the array must read clean, and the
logs must join the verdict to that converge through trace ids: the read
whose verdict named the member, logged under the id of the round that
started it (`dnagent.md` SH29); the worker's `syncup result` record of the
`SyncupCntlr` it re-sent on the code of a later round that answered from
that read's observation, which names the read's id as the observation's
trace id while the re-send runs under that later round's own id; and the
converge's remove and then add of the member under the re-send's id
(`cnagent.md` CN12, CN30; `dnv-worker.md` RW4, RW10). The worker reads the
leg's bitmap itself, once, from the primary, once the cutover is clear, the
new side reports its dm-clone at the desired revision and the primary
reports a live path to it (`dnv-worker.md` BM7), and every piece of it must
reach the new side's disk node, whose apply must mark never-written regions
copied on its dm-clone; the record names the leg and the piece count, and
the dm-clone's copied count, read on its disk node right after the apply,
must leave regions to copy: a copy already whole when the pieces landed
fails the case rather than passing, as no piece would then have kept a
region from being copied, and a fault in the pieces would pass with it
(`dnv-worker.md` BM7; `dnagent.md` DN15). The bucket then comes off. The
migration finishes under the same leg id with an optimized path before any
read (`architecture.md`, Migration; `dnagent.md` DN12, DN13), and host0
reads the migration's fio stamp back whole through the moved leg's new side:
with raid1 md reads a group's data from either member, so for that read the
group's other leg's member carries md's write-mostly flag on the primary,
which leaves md the moved leg to read from; the flag must still stand once
the read is done and is cleared before the read is judged, so a read that
fails leaves no flag behind. The fio stamp's namespace and thin device are
then deleted. A second migration, once its cutover has started, is refused a
plain cancel and ended by a forced one, its source serving again, the
primary's path to it optimized, with the time from the cancel to that path
logged, and the volume reading as before, since no host IO ran in between
(`architecture.md`, Migrations); the case relies on leg repair leaving a
two-sided leg alone (`dnv-worker.md` AR8). A third migration of the same
leg (raid1 only, as a leg's one side lost with its guest takes the slice
with it) loses its destination's whole guest in the middle of its copy,
held back by the same bucket. Once its pieces apply, the copy moves only
the leg's meta region and the blocks slice 0's pool maps when the worker
reads the leg's bitmap, before this step the origin's few; before they
apply, a write that covers part of a block not yet copied has that block
copied first, so the copy moves at least these. So before the create, while
the source serves, host0 writes a fio stamp over a volume of its own past
the part the stamp after the cutover takes, the volume's second half at one
slice and its last two thirds above: each part covers the same share of
every slice's thin device whatever the stripe, so the blocks this stamp
gives the copy to move do not shrink as the slices grow, and the volume is
larger above one slice, where the time from the cutover to the check comes
near the copy that a volume of the one-slice size would give. These blocks
are mapped at the worker's read whichever comes first of that read and the
stamp after the cutover, and written again only by the stamp over the whole
volume, with the bucket off. md must then hold that stamp flushed, as below
for the stamp after the cutover, so that md has no write of its own to send
the moved leg in the migration's gap, where one that md sends failfast can
fail the member (`architecture.md`, Known limits, A pool commit in a
migration gap waits for the gap to end) and md's copy of the whole member
back would make the copy whole before the check. Once the destination
serves and the primary's path to it reads optimized, host0 writes a fio
stamp over the volume's first part and reads it back, md must read the
group's array clean on the primary, as md writes its superblocks with a
flush to every member when the array turns clean, and md's bitmap on the
moved leg's member, read with the plain `mdadm --examine-bitmap`, must then
read no dirty chunk, md's passes over its bitmap shortened to half a second
while the step waits, as md clears a chunk's bit only after a flush to
every member: the stamp is durable on every leg, the destination's
dm-clone, whose flush commits its metadata, included, and a re-add of the
member copies nothing onto it. The copy must then still be short of whole,
which rests on the bucket's rate against the lab's time from the cutover to
that check; the step logs that time, from when it saw the cutover start,
the regions left and whether the pieces' apply is logged. Then
the destination's guest is reset through the kernel's sysrq trigger, which
takes every agent of the guest, its loop devices, its dm devices and its
nvmet ports, and any other side of the pool it carries.
host0 makes no IO from the reset until the leg serves again. Once the guest
answers under a new boot id it gets its per-boot preparation (Preflight)
and its agents start again over their backing files and stores, each
reporting its node state; the destination must serve through its dm-clone
again, the primary's path to it read optimized and md hold every group of
the pool whole on the primary, having written to the moved leg's member
since just before the reset, by the member device's own count of written
sectors, no more than a bound with room for its superblock and bitmap pages
and far below the member's data area, so md took the member back by its
bitmap and copied nothing onto it; with the bucket off the copy must end and
the migration be finished. Then host0 reads the first part's stamp back
through the moved leg alone, with the write-mostly flag on the other leg's
member as for the first migration, so every write of it, which fio was
told was done before the reset, reads back from what the reset guest kept,
and writes a stamp over the whole volume, over the stamp before the create
unread, which reads back through that leg too. The same
guest, which now carries the moved leg's one side, is then reset under
writes md has not flushed (raid1 only): host0's stamped writer runs on a
volume of its own, made once the pool has room for it under its grow mark,
and a few seconds into a pass, with the group's array active, the guest is
reset; md fails the member at the IO's first error and the writes go on to
the other leg. Once the guest's agents are back, the primary's path to the
side reads optimized and md holds every group whole again, the writer stops
at the end of its pass in hand and its range is read back through the
moved leg alone, with the seeds of the last pass that passed. The first
block that does not read back as that pass wrote it, where the read stops,
the passes that failed and the sectors md wrote to the member across its
return are logged, not asserted: the suite asserts on flushed writes only,
the crash before, and this one logs what a crash costs a leg md had not
flushed. Two of an
operator's repairs
(raid1 only) work on one group, each replacing a leg whose disk node dies as
in react, its agent killed and its nvmet port removed (`architecture.md`,
Repairs, Repair, Retired legs); each dying disk node carries no other side
of the pool — for the first leg `sp get` shows that, since it leaves out
only the sides of repairs, retired legs and clones and the pool then holds
none of them, and for the second the node's own record does — so its death
takes down one leg and its record's side count can be checked. The first
repair takes a fresh new leg, placed outside the group's disk nodes (E2E4),
once the primary's probe has given the dead leg the health epoch a create
requires, while host0's stamped writer runs on a volume of its own from
before the disk node dies to after the repair's finish, md copying, and
must lose no write; it must reach STARTED, read through `repair get` alone, as the
worker follows the primary's report (`dnv-worker.md` AR11; `cnagent.md`
CN12, CN28), refuse an abort and a cancel, each refusal naming its stage,
and its finish, made while md still copies onto the new leg, must put the
new leg in the bad leg's place with the bad leg's `leg_idx` and keep the bad
leg as the pool's one retired leg, its side still in its disk node's record,
while the primary reports the group degraded, in sync on the leg that stayed
alone. The second repair takes that retired leg back once the first repair's
new leg has died in the middle of md's copy onto it, so that the group has
stayed degraded since the retired leg left, the one whole copy on the leg
that stayed: md's copy onto that new leg is slowed on the primary until its
disk node has died, the case failing rather than passing when the copy has
ended before the kill; the group's `operator_in_charge` flag stands from
before the kill to the step's end, since md records a bad-block range on the
new leg when its disk node dies in md's copy, which makes that leg a
candidate of the worker's own leg repair at once (`dnv-worker.md` AR8, A leg
with bad-block ranges), and the flag holds off the worker's create alone,
not the operator's repair; a create from a retired leg needs the retired
side's health epoch clear, so the retired leg's disk node returns first; the
repair keeps the retired time for a cancel, must reach STARTED, and its
finish must put the retired leg back in its place, with its own `leg_idx`
and side, and retire the leg that died; md must then hold the group whole,
by the array's state words and by the whole flag and in-sync list the
primary reports beside its rows, and mdadm's word on the add and the
sectors md wrote to the leg are logged (Known limits). Across the two repairs
no cntlr may hold a retired leg; host0 reads its data while each leg is dead,
while md copies, once the group is whole and at the end, and its kernel log,
marked before the first disk node dies, must report no failed IO, as md fails
a member whose IO errs and serves the read from the other (`cnagent.md` CN12);
no failover may happen. The second retired leg's delete must take its side out
of its disk node's record with the group's extents; that node's returning
agent must then remove the side no record names (`dnagent.md` DN6) until its
counts agree with its record (`dnagent.md` DN18), and the primary must hold
every leg again. The quiet thresholds keep the worker's own leg repair off the
first dead leg, and the group's `operator_in_charge` flag keeps it off the
first repair's new leg once that leg dies, as md's bad-block ranges make it a
candidate with no threshold, so that the worker's repair cannot race the
operator's creates (`dnv-worker.md` AR8), and the retire timer deletes only
an old retired leg (`dnv-worker.md` AR12), so the worker only moves the
repairs' stages and every other step is the operator's. A second clone's
copy survives a failover of the primary in its middle (`architecture.md`,
Clone metadata durability): its source is a second pool's, on a controller
node with no cntlr of the case's pool, so that the failover moves the clone
and not its source, and of one slice with the case's pool's stripe, so that
the optimal IO size the clone's dm-clone reports stays within
`MaxNvmeTcpCmdBytes` at every slice count (`architecture.md`, Clones);
host0's IO sizes for the destination are read within that bound while it
copies; host1 writes a few MiB at the source's start, which keeps its pool
below the mark of auto-grow (`dnv-worker.md` AR6), and no bitmap chunk is
pushed, so every region is copied and no discard on the dm-clone waits for
reads of the slowed source (`cnagent_integtest.md`, Lab facts); a token
bucket on the source's controller node, kept to the source's address and
port, slows what the source sends to a rate at which the copy takes about
the same time at every slice count, one region in flight, since a copy of a
few MiB over the lab's network would otherwise end in well under a second.
On a node without the dm rule of `deploy/udev/`, which the suite installs,
over the slowed source a device-mapper command after which udev reads the
dm-clone, its create or the resume of a namespace device above it, can
outlast the agent's command timeout, as it waits for udev's reads, which go
through the source, and the converge of the `SyncupCntlr` the worker
re-sends on the next round, which the converge's own name of the failed step
brings, then finishes the build (`cnagent.md` CN16, CN18, CN30), and whether
one still can with the rule in place is not measured, so the case waits for
the dm-clone to serve before it reads a count; the destination's namespace
exists, suspended, before the clone, as in a live migration
(`architecture.md`, Transfer + clone = cross-SP live migration), and the
case logs the host's wait at the clone's creation — the time from the create
to host0 reading that namespace optimized, and within it, when a poll sees
it first, the time to every side of the clone meta group provisioned — with
no limit on either, such a command lengthening the wait by that round and
the re-sent converge. The primary is disabled once a quarter of the copy's
regions are copied, which fails it over at once (`dnv-worker.md` AR5),
host0's stamped writer running on a volume of its own from just before the
disable to after the old primary's return, through a failover while the old
primary serves, and losing no write (Known limits); the
new primary's first count must be at least the last count read on the old
one and leave a region to copy; with the bucket gone and the copy whole, the
new primary's destination raid0, which its promotion created, must have had
at most the regions the old primary's last count left written into it, where
a dm-clone over new metadata would copy every region again, and at least the
regions the new primary's first count left, which a write count that sees
the copy reads, read on the raid0 whose number the dm-clone named while it
served, since the worker latches a whole copy and the drain removes the
dm-clone (`dnv-worker.md` CLD13); the clone's unforced delete must be
accepted, or answer `NOT_FOUND` once the worker has drained the clone, and
the whole destination must read the whole source's digest, which host1 read
before the clone. The worker logs that one failover, the old primary returns
as a standby once enabled, both cntlrs are listed again, and host0 still
reads its data. A controller node keeps its connection to the source after
the clone has gone until its node-level sweep, which a read of the node that
names the source brings, disconnects it (`cnagent.md` CN21), so the removal
of the second pool, here as after the fallback, drops it on every controller
node before the pool's subsystem goes, rather than wait for that sweep.

**react** proves the four reactions on real faults, that a planned restart
of the primary's cn agent moves nothing, an operator's forced start of a
group md will not start, an operator's take-over of a group's repair from
the worker, the cut of a migrating leg whose serving disk node goes silent,
the loss of the primary's machine under host writes, and measures the cut of
a whole group, on a pool of the run's slice count
but at most `REACT_SLICE_CAP`
slices (Topology and parameters), each by the record it writes or, for the
restart, by the records it does not write, one action per pass making every
wait a threshold plus a few passes (`dnv-worker.md` AR2); its own build may
have reacted, so each reaction is judged by a delta from values read just
before the trigger. Auto-grow: strided writes sized from the primary's own
pool usage push slice 0's thin pool past its mark, and the worker must add
exactly one data group and the pool grow on the device (`dnv-worker.md`
AR6); the case's two thin devices together need more blocks in slice 0 than
its data groups hold, asserted before the writes, since the covered rule of
the same AR6 would otherwise stop the grow the case waits for, and a
primary that moves meanwhile stops the run. A planned restart: the
primary's cn agent is stopped by SIGTERM, on which it must exit, not on the
kill that follows the suite's wait (`dnagent.md`, Bootstrap and lifecycle),
and started again with the same flags; its log must show `agent serving`
and then `agent started` (`dnagent.md` SH33), with a startup pass shorter
than the start hold or the run stops, and past the larger of the reacting
set's primary and cntlr thresholds plus a few passes the worker must have
applied no failover and no cntlr replacement, the same cntlr still primary
on the same node and host0's data unchanged (`dnv-worker.md` HL8). When the
agent's startup pass took two check intervals or more, the worker must also
have logged at least one `agent starting` since the step began; a shorter
pass can fall between two rounds, so the step only logs that it tested a
short restart. Failover and cntlr replacement: the primary's cn agent is
killed, host0 having let go first since a killed agent's nvmet objects go
on advertising optimized; the standby must become primary and build the
stack, held to the cntlr threshold while settling (`dnv-worker.md` AR5,
HL2; `architecture.md`, Failover), a second failover stops the run, host0
reconnects through the cdc, whose records the failover's own transaction
rewrote without the dead cntlr (`architecture.md` [D18]), the dead cntlr is
replaced on a node that held none, keeping its slot and role
(`dnv-worker.md` AR7), and the dead agent, restarted, removes what it left
(`cnagent.md` CN7). Leg repair (raid1 only) needs both planes, as kernel
objects outlive their agent: a dn agent is killed and its nvmet port
removed, on a disk node that carries no other leg's side of the pool (a
retired side there is no leg that any cntlr connects) and while the pool
holds no repair, and the worker must repair the dead leg with a repair of
its own and finish it (`dnv-worker.md` AR8, Leg repair; `architecture.md`,
Repair). The repair carries the name the worker gives it, the reserved
prefix followed by the pool's and the group's ids, whose time part is
logged and never asserted (`architecture.md`, Common validation); its new
leg is fresh, none of the group's retired legs, lands outside the group's
disk nodes (E2E4) and alone on its disk node, so that the later deaths of a
leg's disk node take down that leg alone: every disk node that holds a side
is disabled while the worker picks it, since the scan offers one disk node
of each location outside the group's and the worker draws one of them at
random (`architecture.md`, Finding DN candidates, Per-operation
allocation), so a location with no side-less disk node would offer one that
holds a side, and a disabled disk node keeps serving its sides
(`architecture.md`, Disk nodes, UpdateDiskNodeDisabled). The finish puts
the new leg in the dead leg's place with its `leg_idx`, keeps the dead leg
as a retired leg and leaves the group no repair and exactly the retired
legs its give-back rule leaves, judged from the retired legs and their
times read before the kill (`architecture.md`, Repairs, FinishRepair;
Retired legs). The repair is read through dnvctl, and its start through the
finish, which only a started repair takes, and through the worker's one
record of the stage move (`dnv-worker.md` AR11), as a poll can miss the
start; md must then hold the group whole, no cntlr may hold the retired
leg, host0 must read its data with no failed IO in its kernel log, and no
failover may happen meanwhile, proved by an unchanged count of the worker's
failover records. A group with no healthy leg: the disk nodes of both of
the same group's legs die the same way, one right after the other, while
the pool holds no repair, and no host IO runs until the group is whole
again, as md learns of a dead member only from an IO to it
(`cnagent_integtest.md`, Lab facts) or from the primary's early fail, which
never fails the last member md holds in sync (`cnagent.md` CN12), and so
keeps the array running for the leg that comes back; past the side
threshold the group's legs and retired legs and the pool's revision must be
unchanged, no repair of the group may exist, and the worker must have
logged that no leg of the group is healthy (`dnv-worker.md` AR8); with the
disk node that died last back, the worker must repair the other leg in the
same way, with a fresh leg steered alone onto its disk node, and md must
hold the new leg in sync and the replaced one in no array (`cnagent.md`
CN12), as on the idle array only md's members tell the new array from the
old; the group must then read whole and host0 reads its data, again with no
failover and no failed IO. Two forced starts (raid1 only;
`architecture.md`, Repair; `cnagent.md` CN12) each start the same group
when md will not start it: md's copy onto the fresh new leg of the worker's
repair of one leg is slowed on the primary, so that the leg md holds in
sync dies in the middle of it; that fresh leg is steered alone onto its
disk node as before, so a later death of its disk node takes down that leg
alone; and the repairs before each such repair leave the group at the
retired-leg bound, so its finish gives back the group's oldest retired leg
that is not kept and the group must hold exactly the retired legs that rule
leaves, judged as after the first repair (`architecture.md`, Repairs,
FinishRepair). The first forced start keeps every retired leg of the group,
the leg its repair's finish retired before the mark among them
(`architecture.md`, Retired legs): before the second, every retired leg
must read kept, and the step clears the flag of every one but that leg, as
an operator would once they have judged them, so that the second repair's
finish has one to give back. The pool's level is raised to the one where
reactions stop before the leg md holds in sync dies, while every leg row
and the group's row read OK, because an array that does not run turns the
group's row into an error, which sets the primary's health epoch, and a
failover would hand the group to a primary that cannot start it either
(`dnv-worker.md` AR3, HL2; `cnagent.md` CN28), and because at that level no
pool holds the array open (`cnagent.md` CN13, CN19); host0 makes no IO
until the pool is back at the read-write level. Each forced start comes
only once md on the primary has read the array down and the primary's
report has read the group's row down, both on every poll for several of the
primary's check rounds, since while md retries a copy whose only source is
dead the array's state moves between running and broken, and a pass of the
primary's that reads it running leaves the mark waiting for the converge
that a later read's name of the forced start, or of the stop of the array
it begins with, brings (`cnagent.md` CN12, CN30; Known limits). In the
first, a forced start made while the array still runs
is refused (`architecture.md`, Repairs); one from the dead leg that held
the data, retried while the gateway still refuses it, as the primary's
latest report can read the array running (`architecture.md`, Known limits)
and its agent can fail to answer or report no row of the group
(`architecture.md`, Repairs), must leave a mark that `sp get` shows, and
the primary must stop the array that does not run while that leg is still
dead, which leaves that leg's superblock as md last wrote it, with no
record of the regions md could not copy (`cnagent.md`, Known limits); once
the leg is back, the named leg's bad-block list is read from
`group inspect`, as whether md wrote one depends on the run: when the leg
carries one, the dry run with no override must refuse it, list it among
its refusals and pass none of them, the one passing every kind it lists
must refuse nothing and name the drop, and the mark is written again
passing those kinds (`cnagent.md` CN12); the second forced start reads its
named leg's list the same way before its mark. Then the primary's own log must show the other
leg's superblock zeroed, the array assembled from the named leg alone —
with the new identity's update, and when the record names the list's drop
too, assembled once more with that update, after a stop of the array when
the first assemble answered and so started it, as mdadm takes one update a
run — and the agent's record that the array started, and after that record
the zeroed leg added back as a new member, which md copies whole
(`cnagent.md` CN12); the worker must clear the mark from the primary's
report once the start is done (`dnv-worker.md` AR13), and host0 must read
both namespaces' data unchanged. For the second, host0 writes new bytes
over a block the group held before the first forced start's retired leg
left it, and the leg that holds the data stays dead: an operator's repair
takes that retired leg back and stays created over an array that does not
run, a forced start from the repair's new leg must start the array, the
repair must start and be finished, and host0 must read the bytes that
retired leg kept, not the new ones (`architecture.md`, Repairs,
CreateRepair, FinishRepair); a repair that takes back the retired leg
holding the new bytes, whose md superblock, read on the primary, must carry
another identity than the array's, since it left before the forced start,
must be refused — the primary's verdict carries the refusal, the worker
logs it and moves no stage (`cnagent.md` CN12; `dnv-worker.md` AR11), and
the primary neither adds that leg nor takes the bad leg out, its one
command on the bad leg's member, at most, the early fail of a dead leg's
member it keeps in the array (`cnagent.md` CN12, The early fail) — and its
abort and cancel give the leg back as a retired leg with its kept time
(`architecture.md`, Repairs, CancelRepair), host0 still reading the
restored copy. Each forced start is judged by the agent's record of the
start and by the zeroed leg in sync, never by an exit status, as a command
killed at the agent's soft timeout can still have taken effect
(`dnagent.md` SH15), and neither member of the started array may hold an md
bad block, which md serves as an IO error; no failover may happen across
either, and host0's kernel log reports no failed IO. The dead legs' disk
nodes come back in the end, as only a live agent removes the side of a
retired leg the pool's drain gives back. The take-over (raid1 only;
`architecture.md`, Repairs) runs with the pool at the read-write level when
its stage starts and when it ends, as the stage changes no level, and with
the same group below the retired-leg bound, its oldest retired leg deleted,
kept or not, as an operator's repair takes a fresh leg only below it and
the operator's delete takes a kept leg as asked (`architecture.md`,
Repairs, CreateRepair; Retired legs). One of the group's legs dies as in
the leg repairs before, and the worker's repair, its fresh leg steered
alone onto its disk node, must stay created: before the kill the primary's
controller node refuses its own TCP connects to the nvmet port of every
disk node that leg can land on, a reset answering each, so the primary
cannot add the new leg (`cnagent.md` CN12), while those disk nodes stay
healthy and answer, so the worker still places the leg there and flips its
side provisioned — a worker's repair is otherwise created for a few passes
only, and a frozen disk node would not hold it, as the worker marks a disk
node unhealthy at the first round it misses (`dnv-worker.md` HL1) and
allocation passes it over; the rule names each disk node's address and
port, never a product of the two, so it refuses no connect to a leg of the
pool, and it comes off once the worker's repair is gone, as it does in a
failed run and in every cleanup (E2E6). Once the rule has refused a
connect, the repair must read created with its side provisioned, or aborted
with its side provisioned should the worker's own abort have come first
(`dnv-worker.md` AR8); with the group's `operator_in_charge` flag set,
which `sp get` shows, the operator's abort of the worker's repair must be
answered done, or refused as a repair the worker has already aborted or
cancelled; the repair must then read aborted, or be gone with the dead leg
still in its place, and the operator's cancel must take it away or find it
gone, the worker's drive step having cancelled it first, which the worker's
record of that cancel must show (`dnv-worker.md` AR8); a worker's own abort
the case sees come first, as an abort flag already set when the repair
reads held or as the refusal of the operator's abort, must likewise show in
the worker's records. With the rule off, for several passes the group must
keep its legs and get no repair, while the worker logs that it holds its
create of a repair for the dead leg under the flag (`dnv-worker.md` AR8);
the operator's own repair of the dead leg, with a fresh leg, must then
start and be finished, the dead leg kept as a retired leg, the flag
cleared, md must hold the group whole on the fresh leg, which it copies
whole as it was never a member (`architecture.md`, Repair), host0 must read
both namespaces' data unchanged with no failed IO in its kernel log, and no
failover may happen across the take-over. While the flag stands host0
reads both namespaces twice through the primary, once the worker's repair is
gone, with the group on its one live leg, and again right after the
operator's finish, while md copies onto the fresh leg, md's copy slowed on
the group's array from before the operator's create and given md's own
limits back after this read, and md must be copying onto the fresh leg
just before the read and just after it, md's row of the group logged with
each read; a read that meets
the dead member is served by the other, as md fails the member its IO meets,
and the kernel-log witness from the stage's mark covers both. The
migrating leg (raid1 only, and only with more disk-node guests than legs
per group, E2E4) runs after the take-over, on the same group at the
read-write level, and takes away the disk node that serves a leg in the
middle of its migration (`cnagent.md` CN10; `dnv-worker.md` AR8). A side of
one of the group's legs migrates to a disk node that holds no side, on a
guest outside the group's, steered there as the fresh legs before are, so
that its silence takes down that one leg; the stage waits until the cutover
has started, the destination serves through its dm-clone with its copy
whole, the primary's path to it reads optimized and its path to the source
inaccessible, the standby holds both paths live, the worker has read the
leg's bitmap and md holds the group whole, so that the destination is the
leg's serving side and holds every block. With no host IO running, the
destination's nvmet port then drops all its traffic on its guest, both
ways, as a disk node whose power went away looks from the wire, while its
agent goes on answering the worker. Once the primary's controller to the
destination reads connecting, a detached write loop on host0 (E2E7)
rewrites setup's bytes over a range of ns 1 the group holds. It starts only
then: a write already sent when the traffic stops fails back once the
controller's error recovery ends it, and md fails the member on that write
with no cut (seen in the lab on the 7.0 kernel), so only a write sent once
the path is down meets the wait the cut ends. The primary must cut its live
path to the source, which its cn agent logs once and only once, since the
converge connects no side of a leg whose serving path is down (`cnagent.md`
CN10), while the standby keeps its own; md on the primary must fail the
leg's member; and the loop must go on finishing writes, none of them
failing. The worker must then finish the migration, the destination's
latest report showing its copy whole — one record of that finish, naming
the migration, and no cancel — the leg keeping the destination's side
alone, and then repair the leg with a repair of its own and a fresh leg,
created, started and finished as in the leg repairs before, the leg retired
with the destination's side and the group's retired legs held to the
give-back rule, while the loop runs with no write failed, and the loop must
finish more writes once the repair is finished. The traffic comes back once
the repair is finished; md must then hold the group whole on the fresh leg,
the loop must still finish writes, and no cntlr may hold the retired leg.
The loop stops on its stop file with no write failed by its records,
host0's kernel log since a mark taken before the silence must report no
failed IO, host0 reads its data unchanged, and no failover may happen
across the stage. The loss of a machine comes next, twice: the primary's
controller-node guest is reset through the kernel's sysrq trigger, with no
sync, which drops every kernel object of the guest as a power loss does, and
the hypervisor starts it again; the guest must answer again under a new boot
id within a bound. The first time host0's stamped writer runs on a volume of
its own, exported by both cntlrs, from before the reset to after the
replacement, and must lose no write: its writes wait on the multipath head
while no path serves and go on through the new primary. The second time
host0's writer to new blocks runs on a volume of its own: from several jobs
at once, 4 KiB writes a pool block apart, each to a block no write mapped
and each followed by an fsync, whose flush makes the pool commit the
block's mapping (`architecture.md`, Known limits, A write that needs a new
block is durable only after a flush or a commit), the reset coming once
every job has completed writes. While the writer runs, the primary's node
delays what it sends to every meta group leg's nvmet port by
`FSYNC_META_DELAY_MS`, so that each pool commit, and with it each fsync,
lasts a few such round trips and the reset can land in one, while the data
groups' writes keep their speed; the delay goes with the reset guest. The
writer logs the time around each write and each fsync; after the failover
every block is read back and told by its header for its offset from the
zeros of a block never mapped. Each job's first write or fsync that waits seconds is the reset's
stall: the writes before it, whose fsync completed, the write in flight
then, which the host's multipath code sends again to the new primary, and
every write after must read back, and every job must have met the stall; a
write whose fsync was in flight at the reset is the case of the entry of
`scope.md` "A host's fsync completes after the primary died with its
writes", counted held or lost and logged, not asserted (Known limits,
The flush stage's lost fsync is logged, not asserted), and so is the
kernel-log witness of that second reset, which an fsync the reset fails
would fill. A write or an fsync that fails ends its job there and is
logged, held or not, as an error the host was told; the writes before it
must still read back. Nothing of the old primary survives to advertise a path, so
host0 keeps every path it holds through the failover: the standby must
become primary and build the stack (`dnv-worker.md` AR5), and host0 must
read its data through the path it already held, now optimized. Once the
cdc's records no longer name the reset node, host0 drops its path to it, as
a host that follows the discovery log does, and must hold none; it does so
before it connects the replacement, which keeps the lost cntlr's cntlid
slot, so that a retrying controller of the same controller id does not make
the kernel refuse the new one (`architecture.md`, Known limits). The lost
cntlr must be replaced on a controller node that held none (`dnv-worker.md`
AR7), host0 must connect the replacement through the cdc, its kernel log
since a mark taken before the first reset must report no failed IO, and the worker
must have applied one failover and one replacement. Only then does the guest
get its per-boot preparation again, the module loads and the configfs mount
of Preflight with that guest's checks of Preflight, the kernel option's
among them, and its cn agent, started from its store, must converge to a
node with no cntlr of the pool and no dnv object (`cnagent.md` CN7): an
agent back before the replacement would answer its rounds and keep the cntlr
it rebuilds. The cut of a whole group (raid1 only) runs last: every leg of
slice 0's data groups is cut from the primary alone, every packet between
its node and each leg's nvmet address and port, as the pool's record gives
them, dropped while the standby still reaches them, for 3, 8, 20 and 60
seconds in turn, each cut under host0's writer to new blocks with no fsync,
whose mappings the pool commits on its own about once a second. The cut is
timed on the primary's node, from just before its rule goes in to just
after it comes off, and a watch in the background there reads md's array
state and the slice's thin pool line once a second while it stands, so no
read lengthens it. Each cut records its measured length and the packets its
rule dropped, what host0 saw (its longest write, its writer's error, its
blocks read back), the watch's readings, the worker's failovers and leg
repairs since the cut, and the time until the writer is done and the
primary holds the groups whole again or reads the pool needs_check, and,
after a needs_check cut, the time until the groups are whole again. A cut of
3 seconds, shorter than the
leg connections' keep-alive, must make no failed write and no failover, and
every write must read back, at every slice count; and, as the cut's own
proof at one slice, the writer must have met its stall, a write waiting at
least the cut's measured length less a second, which a rule that misses the
legs' packets does not make; above one slice that pause is logged, not
asserted (Known limits). A cut whose data legs share a disk node with a meta
group's leg, which the cut would cut too, is not made, and the cutting ends
there; as the case steers every fresh leg it places onto a disk node that
holds no side, and the pool's create puts every leg on a disk node of its
own (`architecture.md`, Per-operation allocation, CreateStoragePool — DNs),
that happens only where a placement the case does not steer draws a guest
every disk node of which holds a side: AR6's grow, which keeps out no disk
node (Per-operation allocation, GrowSlice), or the fresh leg of a leg repair
the worker makes while the pool builds, which the case allows and which
keeps out only its group's disk nodes and, at the first tier, its group's
guests (Per-operation allocation, CreateRepair). Such a guest is one the
create left with as many of the pool's legs as it has disk nodes: at the
placement bound's disk nodes per guest (E2E3), the create can leave one on
four disk-node guests, as the lab has, in a run of three or four slices, on
none of three guests or fewer, and on five or more at other slice counts
too; or one it left one short, whose last disk node with no side such a
repair took before the grow drew it. The rest is measured, not
asserted (`architecture.md`, Known limits, A group that loses every leg at
once under IO is broken by md). Each cut's blocks are read once the writer has
ended and the primary reads the groups whole or the pool needs_check, through
the primary of that moment, so the blocks of a cut that broke a group can be
read through the array md broke, before any failover, and then count as failed
reads, neither held nor lost. A cut that leaves the slice's pool needs_check
ends the cutting, as no step of dnv repairs such a pool: the stage gives the
worker the primary threshold and a minute more to make the groups whole again,
logs what it did, and otherwise takes the pool to the level that builds no
thin pool, where the primary's re-assembly starts the arrays again
(`cnagent.md` CN12, The re-assembly), and back to the read-write level, where
the pool opens with its mark again; the case's status check then allows that
pool's row (The ending every case shares). That level step is not exercised:
in the lab the worker makes the groups whole first. A cut that leaves the pool
in any other state but OK, with its groups whole again, ends the cutting too,
and the status check then judges the pool. How the stage goes on is decided by
its read of the pool on the primary of the moment the groups read whole or the
pool needs_check.

**cutoff** proves that a primary cut off from the control plane fails over as
a pause for a host that follows the discovery log (`architecture.md`,
Failover; `architecture.md`, Host view), and then that the data plane runs on
while etcd is down (`architecture.md`, Running without the control plane). Its
pool's primary must first have settled, as a settling primary is held to the
long cntlr threshold (`dnv-worker.md` AR5, HL2), and both cntlrs must be
listed, as nvme-stas connects only what the log lists and the standby's path
is the one that carries host0 through the stall. host0 is then handed to
nvme-stas (E2E10), which must connect both cntlrs, and once a marker is
written into host0's kernel log a detached loop of small synced writes runs on
a scratch namespace (E2E7). The primary's cn agent is stopped, not killed
(E2E8): it answers no
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
reconnect it as an inaccessible path, with still one failover in all.

With the loop still writing, etcd is then taken away. The stored state — the
cluster, the pool, its thin devices and subsystems, every controller node and
every disk node of the pool — is read through `dnvctl` until two readings in
a row agree, and etcd is stopped by its pid file and must exit (E2E8). While
it is down a `dnvctl` read is refused (`gateway.md` GW7); the worker must log
its self-fence, naming a stalled heartbeat and the seed it ran under
(`dnv-worker.md` VW8); the loop must go on finishing writes; and host1 must
read the same discovery records from the cdc (`cdc.md` DS10). etcd is then
started again on its data, and the worker must rejoin under the seed its
self-fence minted, with a registration, a commit of itself as a member and an
owned shard in each of its roles (`dnv-worker.md` VW7, VW9). The gateway must
answer again, every reading must equal the one taken before the stop, host0
must hold the same controllers, the scratch namespace still optimized through
the new primary and inaccessible through the old, and a new thin device must
become created, which only a worker driving the pool again does
(`dnv-worker.md` RW19), before the case deletes it. No write may fail, by the
loop's records and by the kernel log after the marker, no failover may
happen, and the worker fences itself once, or twice when the incarnation the
first fence minted fences once more before it reaches etcd, which
`architecture.md`, Running without the control plane, allows; the worker
then rejoins under the seed the second fence minted. Each of these waits is
on a record or a reading, never on a time.

Then the loop stops and its range reads back as its pattern, nvme-stas stops
and host0's own configuration of it is back byte for byte, host0 reconnects
through the cdc, and the case deletes its scratch namespace and thin device.

Last, the cut is taken again with a writer on the path the sides fence
(`architecture.md`, Failover; `dnagent.md` DN10). A new thin device gets a
namespace, and host1, which runs no nvme-stas and so keeps its one path, is
connected to the primary alone with a plain connect. fio writes the device
through it in order, one block at a time, one write in flight, with
direct and synchronous IO so each write returns only once it is durable,
at a fixed
rate, with fio's own verify and its saved verify state; it must be running
with no error before the stop. The primary's cn agent is stopped, the
primary role must move to the standby within the same bound as the first
cut, with one failover more, and fio must then stop on a write error within
the demotion hold, the fence round, one side converge and the driver's
polling slack: the sides fence the old primary once the hold runs to its
timer, the fence-last legs' sides once the round ends, and md completes a
write that fails on a leg the round fences from the fence-last leg, so the
first write that fails is one after the fence-last leg's fence
(`dnv-worker.md` RW24). A writer that never sees an error fails the case, as does one
that finished its range first. host1 then leaves the old primary, its agent
runs again and converges to a standby, the new primary builds its stack, and
host1 connects to it. fio must have issued exactly the writes before the
first failed one and that one; it then verifies, with its saved state, every
block before the failed write's offset — the state reaches one block past
the last write fio completed, so the verify is bounded at that offset; the
failed write's own block reads zeros or that write's own bytes, told by
fio's verify header for that offset; and the range reads zeros after it.
An error tells the host a write's outcome is unknown, not that nothing was
written: a write in flight as the sides fence the old primary can land on
the legs and still be reported failed, as one 32-slice run showed. With one
write in flight the first failed write is the only one the host is told is
in doubt, and these checks show that every write the host saw succeed
landed and that nothing written after the failed one did; a write the old
primary acknowledged can still be lost to a known limit, and the case then
fails (Known limits). Either outcome of the failed write lands before the
new primary serves, whose path opens only once the old one is fenced
(`dnagent.md` DN10). host1 lets go, host0's two plain paths, kept through
both cuts, serve setup's namespace through the new primary, and the case
deletes the writer's namespace and thin device. So the ending finds only setup's
subsystem, namespace and thin device, and only the suite's own paths on
host0, while the primary role stays on the cntlr the second failover
elected.

**many**, an opt-in case, proves that a controller node holding many pools
fails over as a pause for every one of them (`architecture.md`, Failover),
the layout a CSI cluster has by default, where every volume is a pool of its
own (`csi.md`, The volume model). It builds the run's pool count of one-slice
RAID1 pools, setup's the first, each with two cntlrs, a thin device and a
namespace that both hosts reach through the cdc, all under the cutoff set
(Topology and parameters, Event thresholds): every pool is created before any
is waited for, as a burst of volume creates makes them, and each must then
have every side provisioned, its thin device created, its primary settled,
both cntlrs listed and its namespace exported by both. Both hosts must be
served a discovery record for both cntlrs of every pool and must hold, after
one discovery connect each, every namespace optimized through its primary
and inaccessible through its standby on live controllers; the case logs each
host's available memory and TCP connections before and after. Each host then
runs one fio with one job per pool, which writes the pool's namespace in
order over the host's own range, one synchronous direct write in flight at a
low fixed rate, with fio's verify headers and a latency log of every
completed write, and goes on after a failed write, which that log then
lacks; every job must have completed writes before the first cut,
and none may have failed.

The case then cuts the controller node that holds the most primaries, twice,
reading the primaries again before each cut, as the first cut moves every
primary off the node it cuts. The first cut drops every packet of the node's
guest but the driver's ssh, by a table that removes itself on a timer should
the run stop first: the agent answers no check round, the hosts' paths to it
stall unanswered and its connections to its sides stall too. The second, once
the first node is back, stops the cn agent of the node that then holds the
most primaries, as the cutoff case does (E2E8), while its kernel objects go
on serving. In each, every pool whose primary the node holds must fail over
and both hosts must see its namespace optimized through its new primary;
after the packet cut every job must also have written again by then, through
the new primary. Then the node comes back, the table removed or the agent
continued, every pool must list both its cntlrs again, both hosts must see
every pool optimized through its primary and inaccessible through its
standby, and every job must have written after the return. A sampler on each
host reads every path's ANA state twice a second while a cut lasts, so the
times come from the guests' records rather than from the driver's polls,
which take a call per pool: per pool and cut, the case reports the seconds
from the cut, on the cut node's clock, to the worker's record of the fence of
the old primary's sides, logged when the demotion hold runs to its timer
(`dnv-worker.md` RW22), to its record of the failover, and to each host's
first sample optimized through the new primary, the failovers the worker
logged for the pool in the window, each host's longest write completed in
the window and its failed writes, and the load of each controller node after
the cut. On the packet cut no write sent while the node is cut off may
fail, every pool whose primary the node held must fail over exactly once
and no other pool at all, and both hosts must see each of them optimized
within the measured bound (Known limits); the writes that fail once the
node is back are counted, as a host can reach the old primary again before
its agent turns it into a standby (`architecture.md`, Known limits). A
failed write's time is the completion before it plus one write interval for
each failed write from that completion to it, itself included. On the agent
stop the writes a host sends down
the old primary's path fail from the fence until the agent, continued,
turns that path inaccessible, as the cutoff case's writer shows on a plain path
(`architecture.md`, Known limits), so they are counted, not asserted. Last
the writers stop, no write may have failed outside the two cuts' windows,
each from its cut until every job has written again after the node's
return, and every job's range is read back with fio's verify up to its last
completed write: the read-back holds only when that fio ran to its
end and every job answered that it read its whole range, a block whose
write fio saw complete must verify, and a block whose write failed may read
zeros or that write's own bytes, as a failed write can land, and is
counted. The case then deletes every namespace, every
subsystem but setup's, every thin device and every pool it made, the hosts
letting go of its subsystems first, waits for every drain, and reaches the
shared ending with setup's pool alone.

**upgrade**, an opt-in case, proves that a cluster of the v0.1.0 release
upgrades in lockstep to the tree's build with no failed host write, no
reaction and its data unchanged (`architecture.md`, Upgrade in lockstep), and
that an etcd backup taken under v0.1.0 restores with the new programs as
`architecture.md`, etcd backup and restore, says. It runs alone and gets no
setup: v0.1.0's programs lack flags the suite's start lines pass, and its
records lack fields setup reads, so the case builds its cluster itself. The
driver keeps the programs v0.1.0's build makes, the five the case runs and
dnv-csi, in the gitignored `integtest/bin/v010`,
built once from a clone of the tag, not in a git worktree, which Go stamps
with the commit of a checkout around it or with no version, never with the
worktree's own tag (`architecture.md`, Components: invocation reference); a
run reuses that build while each of the five carries the tag's module
version, builds it only with room for it on the driver, and keeps the
programs alone, not the clone. The guests then
run v0.1.0's programs with v0.1.0's start flags, and every node guest the
one udev rule v0.1.0's suites installed, its mask of the stock md assembly,
in place of the rules of `deploy/udev/`, which v0.1.0 did not ship. Every
pool of the case has one slice, RAID1, two cntlrs and the shipped
thresholds, whatever the run's slice count, and a run with `--redund none`
is refused, as only a RAID1 pool gets the array flag's write that
makes A read older (`architecture.md`, Upgrade in lockstep). Before anything
else under v0.1.0 the case reads that v0.1.0 runs: every dnv program on every
guest runs a file whose digest is v0.1.0's build's, read from the process
itself, each shipped program refuses `--version`, no agent log holds the start
record of the versions and no daemon's start record a version, and the cluster
stores v0.1.0's default stripe of 64 KiB. Under v0.1.0 the case makes pool A,
whose thin device host0 writes a pattern into and which nothing touches after,
and pool C; takes a backup with etcd's snapshot tool; and then makes pool B
with two thin devices, the first exported to host1, and grows C's slice by a
data group. The upgrade runs while host1 writes B's volume with the cutoff
case's writer: the worker stops first, then the gateway, the cdc and every
agent, node by node, as an operator's upgrade does: the controller nodes'
agents one at a time, a gap apart longer than the shipped primary threshold
and two check intervals, the node with the most primaries first, then the disk
nodes', all of them then held stopped longer than that too, so that a worker
left running would fail over a primary whose node stopped while its standby's
ran; every dnv binary is replaced while etcd runs on; the rules of `deploy/udev/`
are written over the mask on every node guest; the new build's node check
passes on every controller node; the new agents start, and every one ends
its startup pass;
every binary's `--version` and every agent's start record name the new build
before the gateway, the cdc and, last, the worker start, whose start records
name it too, and every running program's file then has the new build's
digest. Once the worker has run past the default primary threshold, the
writer still runs with no failed write, the new worker has applied no
reaction by its own records, as v0.1.0's worker log is set aside before the
new programs start, and each pool keeps its cntlrs, its primary and its
sides, the one check that shows a failover by a worker left running, and
host0 reads A's pattern back; the writer's whole range then reads back with
fio's verify, and neither host's kernel log reports a failed IO. The restore
runs the steps of `architecture.md`, etcd backup and restore, with the new
programs: the worker, the cdc, the gateway and etcd stop; etcd's restore tool
writes the backup into etcd's data directory, the old one moved aside, and
etcd starts on it as the suite starts it; the gateway answers, read through
the list of clusters alone, one of the two calls the procedure allows before
the id step, the other the list of workers; the id step runs; the gateway
serves A and C and not B; the restore check of every controller node reads
A older with no lost and no returning thin device and its highest held
revision one above the restored one, since the new worker sets the array
flag of A's groups once after the upgrade, a write the backup lacks
(`architecture.md`, Upgrade in lockstep), B new with both its thin devices
lost, and C may break; while no worker runs and before any store is
cleared, host1 reads B's whole range
back and host0 A's pattern; every agent stops, its local store is set aside
and deleted, and it starts again; the worker and the cdc start, and a new
backup is taken. Then a new pool D takes an sp id above B's and comes up
whole, a new thin device of C takes an id above every id C's grow took,
C's primary refuses to shrink its slice's data concat to the restored
layout (`cnagent.md` CN13), every side made after the backup leaves its disk
node, every disk node's agent comes to hold its record's sides and free
count, and host0 reads A's pattern back. The case ends with no teardown of
its own but the space caps: after a pass the end cleanup takes every object
it made, and after a failure the next run's start cleanup or a
`--cleanup-only` run does (E2E6).

**soak**, an opt-in case, is the long run: hours at the shipped thresholds,
with the operations a cluster's life repeats and slow faults that degrade a
path without cutting it, which proves that nothing reacts while the
operator's operations run, that a fault above the worker's bounds gets the
one reaction its rules give, and that no program leaks; the worker's
reactions are read in each operation's and each fault's window alone (Known
limits). Its two pools name no threshold at their create, so they store none
and each resolves to its default when read (`dnv-worker.md` AR4), as a CSI
pool's does; the
worker runs its shipped vote timers and etcd compacts its history, as a
deployment's does (`architecture.md`, Components: invocation reference).
Setup's pool is the RAID1 one, one slice; the case adds a wide pool of several
slices with no redundancy, each pool with a thin device and a namespace both
hosts reach through the cdc. A writer on each host writes both pools at a low
fixed rate, in order, synchronous and direct, with fio's verify headers
(E2E7), and each cycle starts a new generation of the writers under a seed of
its own once the last one has stopped and read back: every block the last
generation saw written must hold its write, which its seed tells from an older
one's.

The run is a whole number of cycles of at least a fixed length, a cycle
whose operations and fault take longer running on into the next, counted at
its start
and at least one per fault, so every fault comes at least once, which the
case checks at its end. Each cycle runs the operator's operations, each but
the snapshot ending with both pools listing both cntlrs and both hosts seeing
each pool optimized through its primary and inaccessible through its standby:
a grow of one slice of the wide pool, the slices in turn; a snapshot of the
RAID1 pool's thin device, made and deleted; a migration of a side of the RAID1
pool's data group, the legs in turn, finished once the destination has
hydrated; the RAID1 pool's standby disabled and enabled, out of the
discovery records while it is disabled, its node keeping the subsystem and
each host its path to it, and each host's connect through the cdc run again
once it is listed; and a new thin device
exported to both hosts and deleted. None may make the worker react, as none
is a fault. Then the cycle's slow fault, the faults in turn, each held for a
part of the cycle and taken off, the pools settling as after an operation,
with the outcome stated here before the run, read from the worker's reaction
records of the fault's window:

* a delay below the check round's timeout on what the controller node of
  the RAID1 pool's primary sends to the worker's guest from its agent's
  port: every round still answers in time (`dnv-worker.md` RW8), so nothing
  reacts;
* a delay above that timeout on the same path: every round of that node is
  missed, so each pool whose primary the node holds fails over once, past
  its primary threshold and two missed rounds in a row (`dnv-worker.md`
  AR4, AR10), as the agent gets the demotion in time and turns its paths
  inaccessible before the fence, so no write fails; its standbys stay out
  of the discovery records while the delay lasts, shorter than the cntlr
  threshold, so no cntlr is replaced, and nothing else reacts;
* a small loss on what that node sends to the disk-node guests: the legs'
  connections retransmit and slow, so nothing reacts;
* a cap on that node's agent's CPU, in a cgroup of the suite's own: measured,
  not bounded; the case logs what the worker did and lets writes fail only
  from the cap's start until both pools have settled after it is off;
* the copy case's token bucket on what a disk node sends from the port of a
  side of the RAID1 pool's data group: the leg slows, so nothing reacts.

Outside that window no write may fail, by the writers' latency logs. A
sampler reads, from the start and every few minutes, each program's resident
memory, open files and threads, the control plane's four daemons, every cn
agent and every dn agent, and the size of etcd's database file. The bound,
stated here before the run: every program the sampler reads is running at
every sample; from the sample at the end of the first hour, or of the first
sixth of a run shorter than six hours, to the last, no program's memory or
open files grow by more than half beyond a small slack for small counts;
and etcd's database file stays under a quarter of a gibibyte at every
sample; threads are logged. Last the writers stop and read back, the
fault's records are logged, and the case deletes the wide pool and the
RAID1 pool's writer namespace and thin device and reaches the shared
ending.

**The ending every case shares**, which the upgrade case alone leaves out
but for its space caps, first checks the pool's state on disk by
its status lines, while the pool still stands: once md runs no sync on any
group, within a bound, as a case can end while md still copies a leg, one
status read (`architecture.md`, Clusters, GetClusterStatus) must name no
fault of the pool's thin pools, none whose report row reads an error, which
the cn agent makes of a failed pool and of one marked needs_check, none
out of data space, as no case fills a pool on purpose, none read-only for
want of metadata space, no row saying the read held no state of the
pool to judge it by, as a read that judged nothing would pass, and no row
saying the pool's cap left rows out, every page of the read taken; and as
the read lists a thin pool's row only when it reads an error, the
primary's report must hold each slice's thin pool row and read it OK, a
missing or unknown row failing the check; the one
needs_check row allowed, in the read and in the report, is that of a slice
a stage of the case left so on purpose and measured, the react case's
group cut. Every group's array must
read clean on the primary: its row OK, md's array state a running word, not
degraded, no sync running and the group reported whole. The check's own
proof runs at every preflight: its filter must name the fault in a saved
status read holding a needs_check pool, allow it only for an allowed slice,
name a saved space, report and cap row, fail a missing, an unknown and an
unallowed pool row of the report, and name a degraded, a copying, a broken
and a not whole array, or the run stops. Then
it proves that a pool deletes back to nothing:
only setup's subsystem, namespace and thin device may remain before it; a
namespace deleted under live controllers loses its head disk; the subsystem
goes with no live controller under it, both hosts having let go first — a
controller left under a removed subsystem is killed with DNR while the port
carries another subsystem, and left retrying when that subsystem was the
port's last, as here (Known limits); the pool's delete latches, the drain's
end showing only as `NOT_FOUND` (`dnv-worker.md`, The sp drain); every disk
node then has its free extents equal its total with an empty side pointer
list, and its agent comes to hold its record's revision with no side record
left, its disk counts then giving the same free extents (`architecture.md`,
Live-state reporting); the worker's log of the case holds no disagreement
between a disk node's own free count and its record (`dnv-worker.md`
RW13); no node guest holds a dnv dm device, nvmet subsystem or md array,
the last proving on a disk node that its udev rules held; and the space guard
holds — each backing file's allocation, which bounds what its disk node
zeroes and what is written into its sides, since both allocate, the run's
allocation within the case's run cap (E2E5), and every node's and the
control-plane guest's free space at preflight's floors.

**What a pass means.** Exit status zero and PASS mean that every assertion of
every case held in a run that stops at its first failure, and that the end
cleanup then finished on every guest with none of the suite's own nvmet ports
left standing. Data is checked against a pattern the system never sees,
removals by the nodes' own listings and `NOT_FOUND`, reactions by the records
they write. A pass may have taken the clone fallback, whose branch is logged.
It may have left `UpdateCloneTrConf` untested, and after a refused chunk the
skip of unwritten regions too, where the clone's copy outran the case's
writes to it, whose refusal is logged too; and it may have left the skip
untested where the copy outran the worker's push of a chunk the gateway
accepted, which no line logs (Known limits). A skipped check — a
raid1-only step without redundancy, a
guest-distinctness check or react's migrating leg without headroom, setup's
disk-node look at side zeroing after a build whose reactions changed its
legs, and a cut of react's group cut that is not made, as slice 0's data
legs share a disk node with a meta group's leg, which ends the cutting
there, the 3 second cut and its asserts included — is a logged line, not a
proof, and so is every reading the cases above log or count without
asserting it, among them the group cut past its 3 seconds and the 3 second
cut's pause above one slice, the writes whose fsync was in flight at the
second reset, lost in the one run that met them, and that reset's kernel-log
witness, the first block the copy case's crash under unflushed writes lost,
how md takes a retired leg back, the fstrim's drop where the stripe is below
the block, the many-pools case's failovers while its pools build and the
writes it counts at a cut node's return and on its agent stop, what the worker
did and which writes failed under the long run's CPU cap, and the pauses the
writers saw.

## Preflight

The driver's checks run first; the guest checks run after the start cleanup and
its gate and before the first setup write, since port checks taken before the
cleanup would judge the last run's corpses, and the cleanup writes no suite
state; a cleanup-only run does neither. Preflight dies on the first failure,
naming the guest and the fix. It proves that the driver has its tools, the
binaries, whose build versions it prints, the tree's constants (E2E12) and
a pinned etcd release; that every
guest answers passwordless ssh; that the disk-node guests' host names
differ, since the gateway keeps a migration's new side off every disk node
of the old side's host name (`architecture.md`, Migrations) and guests
that share one would leave the copy, react and soak cases' migrations no
destination; that a node guest has what the sweeps and the
agents need — sudo, the tools of both roles (on a controller node, also the
thin metadata reader the cn agent runs, those of the copy case's clone
bucket when that case is in the run, those of the react case's flush-stage
delay when that case is, and the one of the react case's reject rule and
group cut when that case runs with md-raid1, and on a disk node those of the copy
case's migration bucket when that case is in the run and the one of the react
case's silencing rule when that case runs with md-raid1 and more DN VMs than
legs),
every module either loaded or built in, dm-flakey among them for the
read-only level, and the nvmet tree, native multipath on a
disk node too, where a migration destination reads ANA through it, md support
and a stock md assembly rule the md mask can switch off (`architecture.md`,
Components: invocation reference), hole punching, memory and free space, none
of this run's ports listening, and no conflicting nvmet port, as a configfs
port does not listen until a subsystem is linked and an agent would adopt and
rewrite an existing port of its id (`dnagent.md` SH19), and on a controller
node, once nvmet is loaded, the other facts the cn agent's start check reads
(`cnagent.md` CN-CM2): debugfs mounted at /sys/kernel/debug, nvmet's debug
directory there, which the kernel option `CONFIG_NVME_TARGET_DEBUGFS` makes,
and no lockdown the lockdown file shows, as no start line of the suite takes
`--ignore-host-disconnection`, a failure naming
`integtest/nvmet_debugfs.sh`, the lab tool that builds and installs nvmet
modules with the option (`layout.md`, Directory tree); that a host is a
clean initiator — sudo, nvme-tcp and multipath, its identity files,
generated if absent and never overwritten, nvme-stas not running or starting
and the masked autoconnector (E2E10); that both hosts, when the copy case is in the run,
have the tools of its fio stamps, fio, cmp, readlink and pkill, as host1
reads a stamp through the transfer; that host0, when the copy or the react
case is in the run, has the tools of the stamped writer and the writer to new
blocks, fio, python3, readlink, pkill and nohup, and when the ops case is,
those of its fstrim step, mkfs.ext4, mount, umount, mountpoint and fstrim;
that host0, when the react case is in the run or the copy case runs with
md-raid1, has the tool of the kernel-log witness their repairs and react's
resets of the primary's machine read, and, when the react
case runs with md-raid1 and more DN VMs than legs, the tools of the write
loop its migrating leg runs; that host0, when the
cutoff case is in the run, has the tools of the case's write loop and
kernel-log witness and nvme-stas 2.x installed, both its units known to
systemd, as a 1.x one would fail later and silently, never connecting,
nvme-stas being a package the suite never installs; that host1, when the
cutoff or the upgrade case is in the run, has the tools of the fence writer,
fio, cmp, nohup and pkill, which the suite never installs either; that both
hosts, when the many-pools case runs, have the tools of its writers and its
path sampler, fio, readlink, nohup and pkill, and every controller node those
of its cut, nft and systemd-run, and when the long run runs, both hosts the
same tools for its writers and every node guest tc and ip for its slow
faults; that both hosts, when the upgrade case is in the run, have the
kernel-log witness's tool; and that the
control-plane
guest has its tools, its ports free and the free-space floor the space guard
holds it to, and needs no sudo: the closing trim is the one root command tried
there and may be refused. The tool check covers only the sweeps after it: the
start sweep runs before it and a cleanup-only run skips it, so on a guest
missing mdadm or udevadm their md stop can silently leave dnv arrays standing.

The node check (`dnagent.md` CM5) runs later, as a stage of the first setup,
since the guests get the binary and the md assembly mask only in the setup's
first stages: once every agent started and before the pool create,
`dnv-agent check dn` on each disk node guest, over the loop device of its
first agent, as a guest's loop devices are made alike, and
`dnv-agent check cn` on each controller node guest. A check that does not
exit zero, or a report with a failed result, fails the run with the report's
failures; each warning is logged. It runs once a run, not before each case,
since nothing between the cases changes a guest's kernel or tools; hosts and
the control-plane guest run no agent and no check. The guest checks above
stay, run before the first setup builds anything: they guard the tools of
the sweeps after them, between the cases and at the end, not the start
sweep's, which runs before them. The upgrade case, which has no setup, runs
the new build's `dnv-agent check cn` on each controller node guest at its
upgrade instead, before the new agents start, and no `check dn`.

## Cleanup, and why the order is what it is

The cleanup (E2E6) is best-effort and tells on itself: every verb prints a
sentinel when it finishes, and the sweep reports every verb that never did,
with the cause read off its status, and every nvmet port it refused or could
not remove, since a leftover port with live ANA groups fails the next suite's
setup. A verb's bound detects a wedge but cannot end a task in uninterruptible
sleep, which a removal or a scan against a suspended device creates, so every
node guest resumes suspended devices before it removes anything. The order
rests on one principle, a holder goes before what it holds, and on what this
lab does when it is broken. First every disk node takes off the react
case's silencing rule and the copy case's migration bucket, which also holds
the long run's token bucket, whether or not this run set them, so that the
hosts and the controller nodes let go of their paths to ports that answer
and no removal waits on a slowed migration source, and that step does not
finish while a reading still shows either.
Hosts go before every target, as they hold the
controllers, and a subsystem removed under a live controller kills it with DNR
while its port keeps listening and leaves it retrying once the port has
nothing left (`cnagent_integtest.md`, Lab facts). On a host the write loop,
the fence writer, the stamped writer, the writer to new blocks, the fio of
any fio stamp and the many-pools case's writers and path sampler stop
first, so that nothing writes into a device while its paths go, and the ops
case's file system is unmounted, so that no mount holds a device whose
paths go, then nvme-stas, so that nothing
reconnects what the sweep drops, the host's own nvme-stas configuration
going back while the work directory still holds its copy and an nvme-stas
the suite never started being left alone; then the
host drops the suite's and dnv's NQNs and its discovery controller, never
every controller, unmasks, and keeps its identity files, node identity rather
than run state, the dnv prefix also taking the cdc suite's subsystems on a
shared host (E2E9). A copy it cannot put back keeps the work directory, which
then holds the host's only copy of its files, and the host's verb ends without
its sentinel, saying where the copy is, so the sweep reports itself unfinished
and the next one tries again. A failed cutoff run, which can leave its loop
and its fence writer running, nvme-stas on the suite's settings and its
agent stopped, and a failed react run, which can leave its loop running
(E2E6), are thus recovered by the next start cleanup or a cleanup-only
run, whose stop continues the agent first (E2E8). Every controller node goes before any disk
node, as its stacks sit on the disk nodes' sides, in the cn agent suite's order
(`cnagent_integtest.md`, Teardown and cleanup) and in two phases across all of
them, so that every clone final is gone, removed while its transfer source is
connected, before any transfer goes, as a dm-clone flushes through its source
(`cnagent.md` L3). The first phase first takes off the many-pools case's
cut, the long run's CPU cap, the copy case's clone bucket, which also holds
the react case's flush-stage delay and the long run's delay and loss, and
the react case's reject rule and group cut, whether or not this run set them,
so that the node answers again, no removal waits on a slowed source and no
later connect is refused or cut, and does not finish while a reading still
shows any of them. On a disk-node guest every dnv md array goes before any dm
device, as an array assembled from the controller node's superblocks pins the
device under it (Known limits), each loop device's header is zeroed before it
is detached, and the udev rules go once no device exposes a dnv superblock. The
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
place; a run failed by its end cleanup dumps what that cleanup left. The dump
starts with every crash line the logs of E2E13 hold, read on every guest past
one whose logs it cannot read, which it names, so that a crash that failed a
case through another check is named. The
control-plane guest goes first, as its dump holds the failing dnvctl call's
streams, which a later read would overwrite, and every dump is tolerant and
bounded, as a guest command hangs exactly when something is wrong. Once
setup is done, and while the gateway and etcd run, the dump then reads the
cluster through dnvctl, the status read's table first (`architecture.md`,
Clusters, GetClusterStatus), which names what is wrong across the cluster,
then the cluster, the pool and the inspects of its cntlrs and nodes. A
node's dump, on a controller node and on a disk node alike, prints its
kernel log first. Last come the two reads that can wait in the kernel there.
One is the status of every device-mapper device, read with the `--noflush`
flag so that it commits neither a thin pool nor a dm-clone: on a controller
node a clone's dm-clone, whose commit flushes the td it writes into and so
commits that td's pools, and on a disk node the dm-clone of a migration
destination. A thin pool's read can still wait behind a commit of that pool
that waits on a leg. This read goes one device at a time and names each
device before its read: dmsetup holds back the last part of what it prints
to a file until it ends, so a read of every device at once that stops on
one device would not name it. The other is md's scan of the node's arrays,
which reads the first member of each. Both run detached from the session
under one deadline, each with its output in a file on the guest. After a
bounded wait the dump prints what they wrote and tells a read that has not
finished from one that ended without recording its exit code, so a read
stuck in the kernel costs neither the kernel log nor the dump's bound. The
context
names the stage, its trace id and the case, and the last ANA state, controller
and path state the probes read, which tell no path from a path without the
namespace and a live path from a retrying one; each guest's kernel, device,
nvmet and log state follows, a host's with its nvme-stas state, configuration
and journal and the write loop's records, a controller node's
with its recorded agent's process state, which shows an agent the cutoff case
or the many-pools case left stopped, the copy case's clone bucket, the
flush stage's delay or the long run's delay or loss, the react case's reject
rule and group cut, the many-pools case's cut and the long run's CPU cap,
and a disk node's with the react case's silencing rule and the copy case's
migration bucket or the long run's; the
failure line ends with the
filter that pulls the stage's
records. When the stage connected, the host's kernel log is the record of what
the connect attempted.

## Known limits

* **A multi-case run pays a full rebuild per case** (E2E11).
* **The loss of a machine is a guest's reset through the kernel's sysrq
  trigger.** The suite reaches its guests by ssh alone (E2E1), so it resets
  a guest that way, which drops every kernel object as a power loss does but
  leaves the hypervisor's machine running, and what reached the guest's
  disk stays: react resets the primary's controller-node guest twice, at
  react's slice count only, and the copy case one disk-node guest twice,
  its migration's destination once host0's writes have stopped and md has
  flushed them, and the same guest once more under host writes md has not
  flushed. A disk that loses what its own cache held is not exercised.
* **The crash under writes md has not flushed reads md's copy.** On the
  lab one chunk of md's bitmap covers a group's whole array, a data group
  being one extent at every slice count,
  so the copy case's second reset of the disk-node guest finds that chunk
  dirty and md copies the whole array back onto the returned member: in
  its one run md wrote 135517 sectors to the member across its return,
  more than the array holds, the writer's writes after the return among
  them, no pass of the writer failed and no block read back through the
  leg other than as the last pass wrote it. That read shows md's copy
  from the other leg, not what the reset guest kept; only the first reset,
  after md's flush, reads what the guest kept, md having written 49
  sectors to the member across its return in that run.
* **The flush stage's lost fsync is logged, not asserted.** The reset lands in
  an fsync only while one is in flight, so the stage delays the primary's
  writes to the meta groups' legs, which lengthens each pool commit and each
  fsync with it. Its jobs keep one schedule, so the reset meets them all at
  the same point of it. Of its four runs at one slice, two ran with the delay.
  The first met every job in a write and none in an fsync. The second met
  every job in its fsync: each of the four fsyncs returned success 4181 to
  4186 ms after it was sent, before the failover to the standby, which waits
  out the reacting set's primary threshold from the worker's first unhealthy
  verdict after the reset (Topology and parameters, Event thresholds), and
  none of the four writes read back from the new primary, held 0 of 4, which is the
  loss of `scope.md`'s entry "A host's fsync completes after the primary died
  with its writes"; the 20 writes whose fsync completed before the reset and
  the 8 after it read back, and no write or fsync failed. The stage counts the
  fsyncs in flight, held or lost, logs each lost one with its times and the
  longest write or fsync of any job, and asserts nothing of them; the loss is
  a known limit, to be fixed later. A commit slowed this way is not the lab's
  own speed. In the first run with the delay the fsyncs of the writes the host
  sent again to the new primary took under a millisecond, as the host sends no
  flush through a path it identified on the standby (`architecture.md`, Known
  limits, A host's flush through a path it identified over the dm-error is not
  sent), so the writes that read back after the stall show the pool's
  mappings, not a commit their fsync made.
* **The new data checks ran at one slice.** The stamped writer, the status
  check, the flush stage, the group cut, the fstrim step and the copy
  case's two crashes of a disk-node guest have run at one slice only;
  their cost at more slices, the copy case's crashes recovering every
  group with a side on the reset guest and the fstrim step's file system
  tables, is not measured, and the run cap's slack covers it untested.
  Above one slice the group cut's writer writes every slice in turn, so the
  write that meets the 3 second cut on slice 0 can be sent late in it, and
  the cut's own proof, the pause, is logged there, not asserted; the cut's
  no failed write, no failover and every write read back are asserted at
  every slice count.
* **The group cut measures one lab.** Its outcomes past the 3 second cut,
  the host's pause, md's state, the pool's mode and the worker's reaction,
  are logged per run and rest on the lab's kernel and the leg connections'
  timers; the meta group (a cut whose data legs share a disk node with one
  of its legs is not made, which a grow, or a repair the worker makes while
  the pool builds, can meet on a guest whose every disk node holds a side,
  as on the lab's four disk-node guests in a run of three or four slices), a
  group of
  one leg and a cut of the standby are
  not exercised, and the stage cuts no more once a cut has left the pool
  needs_check. Timed on the primary's node, the cuts lasted 3.06 s and
  8.05 s in the one run that timed them so, the 3 second cut's rule
  dropping 22 packets the primary sent to the legs and none coming back.
  The blocks of that cut are read before the failover, through
  the broken array, so whether the writes acknowledged before it held is not
  read here; the cn agent suite's group cut reads them after its failover
  (`cnagent_integtest.md`, Cases, groupcut).
* **The many-pools case's bound is measured on the lab's three
  controller-node guests, at 8, 32 and 64 pools.** On the packet cut each
  moved pool's failover record came 6.1 to 9.9 s after the cut at every
  count, its fence record 9.1 to 12.9 s after, and both hosts saw it
  optimized at most 13.6 s after the cut at 8 and 32 pools and 25.8 s at
  64, the longest write a few hundred ms short of that; on the agent stop,
  at most 12.9, 15.4 and 35.5 s. The case asserts a bound on the packet
  cut with room for a slower lab at 64 pools, and refuses a pool count
  above twice the largest it has run or one whose run cap the driver's
  disk cannot hold (E2E5). 64 pools put up to 47 cntlrs on one controller
  node and, in one run, every extent of one disk node to use, and cost each
  host some 44 to 67 MB of available memory for its 128 controllers. The
  case's script passes whole at 64 pools, in 18 minutes.
* **The many-pools case's build fails over pools that have built.** Built at
  once, with the cutoff set's primary threshold, the shipped default, 64
  pools made 22 and 18 failovers in the two builds that finished, in 332 and
  336 s, and in the other, which never finished, one pool failed over twice;
  32 pools made one in one of two runs, and 8 pools none: each of a pool
  whose primary had settled while later pools still built, pools of the
  burst and, in one run, setup's pool, built before the burst. Their rounds
  missed on observations too old, the largest some 20 s old, the disk nodes'
  side rounds too in one run (`architecture.md`, Known limits). The case logs
  the worker's rounds and reactions of that window and asserts none of it. In
  the second run at 64 the pool that failed over twice was left unbuilt by its
  second failover, its data group's array inactive on the new primary: the
  promotion's assembly from one leg was refused, and the next converge's
  from both legs was ended by the cn agent's soft timeout after mdadm had
  added one member (`architecture.md`, Known limits, A promotion that outruns
  its sides can leave a group's array inactive), and the case failed at its
  build wait.
* **The many-pools case's logs are large.** Every agent logs each object's
  reads at debug, so a passing run at 64 pools leaves some 22 GB of logs on
  the node guests, which its run cap counts. A failed run is held to no cap:
  its objects and logs stay on the guests for the diagnosis until the next
  start cleanup or a cleanup-only run, and a run at 64 that failed in its
  build wait left some 43 GB in the guests' work directories, up to 7 GB on
  one guest.
* **The many-pools case's times are the guests' records.** The cut's time is
  read on the cut node's clock, the fence's and the failover's in the
  worker's log, each host's optimized time by its sampler, two samples a
  second, and the guests' clocks, kept by NTP, agree within about a tenth of
  a second, so each time is good to about half a second; each job writes
  once a second, so its longest write shows a pause to within a second.
* **The many-pools case's agent stop fails writes by design**, through the
  suite's own paths (The cases, many), and its packet cut ends with a node
  whose old primaries serve again until they converge to standbys
  (`architecture.md`, Known limits), so the writes a host sends them then
  fail and are counted, not asserted: none at 8 pools; at 32, 15 of one
  host's in one run and none in the other; at 64, 56 of each host's, over
  20 pools, in one of the two runs that reached the cut and none in the
  other; each a few seconds of a host's writes to the pools whose paths came
  back first.
* **The clone source may fall back.** Whether one kernel can be initiator and
  target for the same bytes is the lab kernel's property, so a source that
  never connects or stops hydrating routes to a fallback pool on another
  controller node, which proves the same assertions against its own pattern
  but not the clone's fio stamp, the zeros of its unwritten part or the skip
  of unwritten regions, which only a clone of the origin has (The cases,
  copy).
* **A clone's copy can outrun the case's writes to it.** The bitmap chunks
  and the transport sent again follow the create at once, but one that
  reaches the clone after the worker has latched it, which the worker does
  once its copy is whole, is refused, most often at many slices, where a
  region spans the most source slices. The run then logs the refusal and
  leaves `UpdateCloneTrConf` untested after either refusal, and after a
  refused chunk the skip of unwritten regions too. A chunk the gateway
  accepts reaches the clone only through the worker's push
  (`dnv-worker.md` BM2), so a copy whole before that push lets the clone
  skip nothing either, which no step checks or logs (The cases, copy).
* **The copy case's failover moves the clone, never its source.** The
  source is a second pool's, on a controller node outside the case's pool,
  so a failover of a pool whose own transfer is the source, which moves the
  source with the clone, is not exercised.
* **A standby's namespaces are inaccessible, never non-optimized**
  (`cnagent.md` CN16), so a host holds one usable path per namespace, and ANA
  path selection across two usable controllers is not exercised.
* **The closing trim returns space to the host only where the guests' disks
  pass discards through**; elsewhere the host's free space shrinks by each
  run's real writes.
* **Without redundancy the suite is smaller**: copy's forced drop, its two
  crashes of a disk-node guest and its two repair stages, and react's
  two leg repair stages, two forced-start stages, take-over stage,
  migrating-leg stage and group cut are skipped.
* **How md takes a retired leg back is logged, not asserted.** md catches a
  retired leg taken back up from its write-intent bitmap in some cases,
  such as when the array has stayed degraded since the leg left, and
  otherwise copies it whole (`architecture.md`, Repair, Known limits). The copy case's array
  stays degraded from the leg's departure to its return, but in between md
  began a copy onto the leg that replaced it, which step 6 ends by the kill
  of that leg's disk node, and md copies the returned leg whole after that,
  whatever the bitmap holds: in a run with no host write to the group while
  the leg was away, mdadm answered `re-added` and md then wrote 124975
  sectors to the leg, the whole array, where step 4's catch-up from a clean
  bitmap wrote 25; read in the kernel's raid1 source, the add of a member
  with no place of its own sets a full copy that only a copy that ends
  clears. host0's stamped writer writes the group from just before the
  leg's departure to after the first repair's finish, and its range is read
  back and its volume deleted then, before the leg's return, so a catch-up
  that skipped a region of those writes would leave the returned leg stale
  there with no read to see it. The case logs mdadm's word on the add and
  the sectors written to the returned leg, and asserts the whole group and
  the data, not the way; neither reading tells the way at any slice count:
  mdadm answers `re-added` whenever md takes the member back into the place
  it held, however md then copies it, and a data group is one extent, which
  one chunk of md's bitmap covers whole, so the writer's writes leave that
  chunk dirty and a catch-up from the bitmap writes the whole array too.
* **react's take-over holds the worker's repair created only on the
  primary's controller node.** The reject rule refuses that node's connects
  alone, so a failover inside the hold hands the group to a primary the
  rule does not hold back, and the case then fails rather than passing on a
  wrong premise. The operator's abort comes ahead of the
  worker's own try-to-abort by the new leg's threshold (`dnv-worker.md`
  AR8), which the case's waits keep well inside but nothing forces; a
  worker's abort that comes first is logged and accepted, and the
  operator's abort then meets a repair the worker is already aborting.
* **A disk node holds the controller node's md superblocks**
  (`architecture.md`, Components: invocation reference), as the primary's
  per-CN linear maps the side itself, and on a disk-node guest without the
  udev rules the stock incremental assembly builds degraded, read-only arrays
  from them that pin the suite's devices, the side or that linear, which of
  the two is not established. Hence the rules on both roles, the disk-node md
  stop before any dm removal and the disk-node md residue check.
* **A migration's pieces are tested only while they land during the slowed
  copy.** The bucket holds the leg's copy to about a minute, a suite choice,
  and the worker reads the bitmap a few check rounds after the cutover, so
  pieces that come later than the whole copy fail the copy case rather than
  pass it untested. The blocks of the migration's fio stamp are the leg's
  last written ones, and a fault of the pieces shows in that fio stamp's read
  only for a block the copy had not reached when they landed. md's
  write-mostly flag is read before and after that read, not during it.
* **The migration's bucket can stop the new side's cutover pass.** The fair
  queue under it splits connections, not the requests of one connection, so
  a udev read that shares the connection of a region in flight waits behind
  that region, and a device-mapper command of the pass that waits for enough
  such reads outlasts the dn agent's command timeout (`architecture.md`,
  Common validation), which stops the pass (`dnagent.md` DN13). That holds
  on a node without the dm rule of `deploy/udev/` (`cnagent_integtest.md`,
  Lab facts); the suite installs the rule, which keeps udev's probe off the
  new side's devices, and whether any read of the pass still waits so with
  the rule in place is not measured. The case then fails on the migration
  gap rather than passing. How often this happens under the suite's rate is
  not measured; such a stop shows in the new side's dn log as a killed
  command under the trace of its cutover request.
* **An array both inactive and over members mdadm cannot read is named by
  neither source**, so the cleanup skips it and an operator must find it.
* **A host controller outlives a target that stops listening**
  (`cnagent_integtest.md`, Lab facts), retrying until its loss timeout and
  pinning its namespace head through its hidden path device. So after a cntlr
  delete the suite asserts only that the path is not live, then disconnects the
  survivor by device; a wait after a disconnect, the suite's own or
  nvme-stas's, demands the path gone, which a wrong address also satisfies.
* **A blocked probe, abandoned host IO, or the write a write loop had
  queued when it was stopped leaves an unkillable dd or sync behind** (E2E7)
  until its device or path serves IO again.
* **react is capped below the ceiling.** At the widest shape a primary's build
  holds the reads of its cntlr off, and the observation its check rounds answer
  from ages past the worker's bound, its rounds then counting as missed
  (`dnv-worker.md` HL7) for longer than a short primary threshold, the default
  included, so the role can move and move back; a settling primary is therefore
  held to the cntlr threshold (`dnv-worker.md` AR5, HL2), but the reacting
  set's cntlr threshold is still shorter than the stretch of rounds a build at
  the ceiling makes count as missed, so there react's build can fail over
  again and again. Hence react builds at most `REACT_SLICE_CAP` slices
  (Topology and parameters), which also keeps the cluster's stripe whole for
  its strided writes, and its reactions are not exercised at the ceiling,
  though the cutoff case's failover is. Below the cap its build may
  react too, which each reaction's delta from values read just before its
  trigger absorbs (The cases).
* **The widest build is bounded by the primary's process spawning, not its
  memory**: fewer slices, no redundancy or more cores on the controller nodes
  shorten it.
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
* **The cutoff case's writer and the copy case's stamped writer can meet a
  known limit.** A failover while
  the old primary still serves can lose a write it acknowledged in the
  cases `architecture.md`, Known limits, A failover while the old primary
  serves can lose an acknowledged write, lists (`dnv-worker.md` RW24). The
  cutoff case's second cut is such a failover, and so is the copy case's
  disable of its primary in the middle of a clone's copy under the stamped
  writer, and
  their worker has the old primary's reports, so the round runs; a side of
  the round that reports its fence later than the round's bound can still
  cost a write, and fio's verify then fails the case, a failure of that
  limit and not of the suite.
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
  primary has built the whole stack and turned its namespaces optimized. A
  promotion that outruns the sides' ANA flips builds it only in the converge
  of the `SyncupCntlr` the worker re-sends once a read that starts after the
  flips names the group's absent array, or under `--redund none` its
  `RedundNone` dm-linear (`cnagent.md` CN12, CN30), which adds the rounds
  until that read and one more converge, each converge growing with the
  slice count (`cnagent.md`, Known limits). Where the cn agent's soft
  timeout ends that converge's assembly of a RAID1 group after mdadm has
  added a member and before it has started the array, the group's array
  stays inactive, which no
  read names absent, so no re-send builds it and the case fails at its wait
  for the new primary's build (`architecture.md`, Known limits, A promotion
  that outruns its sides can leave a group's array inactive). The case bounds
  that wait only by the build's own and logs how long the write took.
* **The outage takes etcd away, nothing else, and only once.** The gateway,
  the worker and the cdc run through it, so a program that starts while etcd
  is down (`cdc.md` CM4) is not exercised, nor a fault that waits for a
  reaction meanwhile, nor the cdc's recovery once etcd is back (`cdc.md`
  DS10), since no discovery record moves across the outage; and how long
  the worker takes to fence itself and to rejoin is bounded by the waits
  and logged as the polls see it, not asserted. etcd is started again once
  the checks made while it is down are done; whether the incarnation the
  first fence minted fences once more depends on how long that takes, so
  the case takes one more fence and fails on a third, whose message names
  it and how long etcd stayed away after the first was seen.
* **nvme-stas keeps its connections when it stops.** nvme-stas 2.4.1, the
  lab's version, keeps the kernel connections it made when it stops, its
  connector always and its discovery daemon under persistent connections, so
  the cutoff case disconnects them itself before it hands host0 back, and a
  host's cleanup drops what a failed run left.
* **The upgrade case upgrades from v0.1.0 alone, at one slice, with no CSI
  driver.** It upgrades from the one release and builds every pool at one
  slice and two cntlrs, so it reads what v0.1.0 wrote but not what a wide
  pool's build writes; the suite runs no CSI plugin, so the plugins' part of
  the upgrade and the volume contexts they hand Kubernetes are the csi
  suite's and the Go tests' (`csi.md`, CSI10). It holds no migration, repair,
  clone or forced start open across the upgrade, the states `architecture.md`,
  Upgrade in lockstep, gives readings for; its restore covers one etcd member,
  no node added or deleted since the backup, and no host added to or taken
  off the list of a pool kept from before the backup: B's subsystem, with
  host1 on its list, is lost whole, so the host lists the operator sets
  before the workers start again have nothing to change; and it checks C's
  break by its primary's refusal to shrink the data concat, not by a host
  read of C.
* **The upgrade case's check of no reaction has a time margin, not a
  bound.** Host1's writer fills pool B's one data group past its mark about
  480 s after it starts, and the worker then grows B, a reaction; the check
  reads the worker's records about 170 s after the writer starts on the lab
  (the agents all stopped 72 s in, held 30 s, and the worker started 137 s
  in), so an upgrade window about 300 s longer than the lab's, the worker
  starting past about 450 s after the writer, would read that grow as one.
  The grow lands after the check, so the restore check reads B with the grown
  group's sides, which the restore frees with B's others.
* **The upgrade case's v0.1.0 nodes carry v0.1.0's md mask.** v0.1.0
  shipped no udev rule file, but its requirements and its tutorials have
  every node carry its md mask, `63-dnv-md.rules`, in the text v0.1.0's
  suites installed too. A v0.1.0 node with no rule at all is not exercised:
  there the stock md assembly takes the legs' superblocks on a disk node
  guest, which pins the sides (as the entry on a disk node's md
  superblocks, above, says) and would stop the restore's side frees and the
  cleanup for a cause the case does not test. So the v0.1.0 half runs on
  that mask, and the upgrade writes the rules of `deploy/udev/` over it
  before the new agents start.
* **From a git worktree the new build's version is not the tree's own.** Go
  stamps a build in a worktree with the commit of the checkout whose folder
  holds the worktree, or with no version, "(devel)", when none does
  (`architecture.md`, Components: invocation reference), so the case's
  version checks then hold every new program to that version and tell the
  new build from v0.1.0 by v0.1.0's programs having no `--version` and no
  version in a start record; a run from a checkout whose `.git` is a
  directory names its commit.
* **The long run is long once, and small.** Two pools on the lab's guests
  for the hours the run is given, with one cycle of operations and one slow
  fault at a time; the weeks of a cluster, its pool counts and two faults at
  once are not run.
* **The long run's faults slow one node at a time, and the CPU cap's outcome
  is measured.** The delays and the loss sit on the node of the RAID1 pool's
  primary, which may hold the wide pool's primary too, so a delay above the
  round timeout fails over each pool whose primary the node holds; the CPU
  cap's level is a choice, and what the worker does under it is logged, not
  held to a rule.
* **The delay above the round timeout delays the agent's replies, not its
  requests.** The agent gets its demotion in time and turns its paths
  inaccessible before the fence, so the failover it shows is one whose old
  primary still converges; a node that hears nothing from the worker is the
  many-pools case's cut.
* **The long run reads the worker's reactions only while an operation or a
  fault runs.** The writers' turnover at each cycle's start, the wait before
  each fault, the rest of each cycle after the fault's verdict and the time
  after the last cycle are not read, so a reaction there passes.
* **The long run's sampler reads the programs, not the kernel.** Each
  program's resident memory, open files and threads are read, and etcd's
  database file; the kernel's memory for the arrays, the device-mapper
  devices, the nvmet objects and the page cache is not, nor the growth of
  the programs' log files, which the run cap counts.
