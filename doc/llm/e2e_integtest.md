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
eleven statements; a smaller shape proves no ceiling, and one without redundancy
no repair, no forced start, no take-over and no cut of a migrating leg's path:

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
* **The operator surface works against a real control plane.** Every `Gateway`
  RPC but the cluster and node deletions and listings, the id step, the
  restore check and the capacity read of the layer above,
  `GetClusterCapacity`, is issued by the
  shipped `dnvctl` against a real gateway, etcd and agents, and no echoed id is
  the whole of an assertion: a mutation is read back from its record and, where
  the device is the point, from its agent, or, for a few, proved by its effect,
  which is what the suite observes.
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
  destination, and the blocks a migration copies last, read through the
  moved leg's new side (The cases, copy).
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
  paths and reads its data through the new primary, and comes back with no
  cntlr of the pool.
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
  repair with a fresh leg is started, finished and copied whole; with the
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
* **The lab is left as it was found.** After every case every disk node has all
  its extents back, the free count of its own disk counts agreeing with its
  record's and the worker having logged no disagreement between the two
  (`dnv-worker.md` RW13), no node guest holds a dnv dm device, md array or
  nvmet subsystem, and allocation stays under a per-file and a whole-run
  cap; the cutoff case also stops nvme-stas on host0 and puts host0's own
  configuration of it back byte for byte.

**What it does not prove.** The RPCs it never issues, the deletions and
listings of clusters, disk nodes and controller nodes, the id step, the
restore check `CheckControllerNodeRestore` (`architecture.md`, Controller
nodes) and the capacity read `GetClusterCapacity` (`architecture.md`,
Clusters): the gateway suite issues each of them, the dnvctl suite each
against its fake gateway, and the csi suite reaches the capacity read
through dnv-csi and through `dnvctl`, whose answer it compares with the
driver's; this run discards the
whole cluster between cases. Error paths, beyond the few refusals the ops case
asserts on purpose, the copy case's refused abort and cancel of a started
repair, react's refused forced start of a group whose array runs and its
refused repair that takes back a retired leg newer than the array, the
`NOT_FOUND` reads after the copy and react cases' deletions and the read the
cutoff case sees refused while etcd is down; validation and CLI semantics are
the gateway and dnvctl suites'. Concurrency: one dnvctl call and one case at a
time, and never a revision token, since a present token changes what the
gateway checks (`gateway.md` GW6; `dnvctl.md` CT3). ANA path selection between
two usable controllers (Known limits). Auto-grow, cntlr replacement, leg
repair, a failover after the primary's cn agent is killed, the forced start and
the take-over of a group's repair, on a pool at the ceiling (`dnv-worker.md`
AR5 to AR8, AR13; `cnagent.md` CN12; `architecture.md`, Repairs), as react's
pool has at most `REACT_SLICE_CAP` slices (Known limits); the cutoff case's
failover, after that agent is stopped, runs at the run's slice count. A
demotion hold that the old primary's report ends early (`dnv-worker.md` RW22):
react's and cutoff's failovers lose an agent that answers nothing, one killed
and one stopped, and copy's disabled primary answers, but the case only logs
which of the two ended its hold, the report or the timer. The worker's own
try-to-abort of a repair of its own, and its cancel of one, which react's
take-over sees only when it comes before the operator's (`dnv-worker.md`, Leg
repair; `architecture.md`, Repair); and the retire timer, as no run waits out a
retired leg's age (`dnv-worker.md` AR12). Guest distinctness of a repair's
fresh new leg in the react case at as many disk-node guests as a group has
legs, where no guest lies outside the group's, and for a leg repair whose fresh
leg react's steer pushes to the second tier (E2E4); and the destination of the
copy case's second migration, cancelled with force after its cutover, which
the case does not check at all. The default location and the pool create's
first preference (`architecture.md`, Disk nodes, Per-operation allocation):
every node passes its guest's role as its location, and no controller node
shares one with a disk node; the Go tests prove both, the gateway suite the
first preference, and the dn and cn suites the host name each agent reports.
The cut of a migrating leg's path while its source serves, and leg repair's
cancel of a migration whose cutover is pending and its wait on a copy not
shown whole (`cnagent.md` CN10; `dnv-worker.md` AR8): react's migrating leg
loses its destination after the cutover, with its copy whole.

## Topology and parameters

Every guest comes from the command line (E2E1), each in one role. The
control-plane guest runs etcd, `dnv-gateway`, `dnv-worker`, `dnv-cdc` and
`dnvctl` as the login user. At least three controller-node guests run one
`dnv-agent cn` each, registered at its guest's role as its location, since
the lab's node guests share one host name, the default location
(`architecture.md`, Controller nodes): two carry the pool's cntlrs and the
rest none, where the ops case's third cntlr and the cntlr replacements land.
The disk-node guests, at
least as many as a group has legs, and more than that in a run with the copy
case, which the suite refuses otherwise, run many `dnv-agent dn` instances
each, every one on its own loop device and its own nvmet port and service id,
as two ports cannot listen on one address, all registered at their guest's
location (E2E4). Two hosts run only the kernel's nvme-tcp stack and nvme-cli,
but for nvme-stas on host0 inside the cutoff case (E2E10). Every dnv daemon
runs at `--log-level debug`, so a failed run's logs hold the reads a fault
needs and the steps that read a converge's commands see its reads too
(`log.md` R6, R9).

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
stage of the suite counts missed rounds. Every node guest carries the udev
rules of `deploy/udev/` (`architecture.md`, Components: invocation reference)
while its agents run, installed before an agent starts from the texts the
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
guest, by default the placement bound (E2E3); one case to run alone; a
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
the bound, a shape above the per-kernel cap is told how many disk-node guests
it needs.

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
pattern, and are asserted after every case; the copy case's run cap also
takes, on every leg, its two fio stamps and its first clone's copy of its
source, which grow with the slices and stay allocated until the case ends.

E2E6. **Cleanup runs unconditionally at the start, and only on success at the
end.** `cleanup_all` runs before anything is built, between cases, at the end
of a passing run and alone in a cleanup-only run, and never dies but reports: a
die would defeat a start sweep, and fail a passing run and skip the other
guests at the end. It tolerates absence, not an unfinished sweep: after the
start and the between-cases sweeps, `cleanup_start_gate` stops the run when any
verb never printed its sentinel, naming guest, verb and status. The end
cleanup's verdict is the exit status: a run whose end cleanup left a verb
unfinished or one of the suite's own nvmet ports standing fails, after
cleaning, while a run that fails an assertion removes nothing but the copy
case's two token buckets, its clone's off the controller node and its
migration's off the disk node that carries it, the react case's reject rule,
which it takes off the controller node that carries it, and the react case's
silencing rule, which it takes off the disk node that carries it, as a
slowed port would slow, a refused one fail and a silenced one stall every
suite that runs before the next cleanup.

E2E7. **No direct-IO flags to dd.** The guests' dd mishandles direct IO
(`dnagent_integtest.md`, Assumptions and preflight checks), so every dd write
is buffered and synced and every dd read that must reach the media follows a
cache drop. Three host processes run detached, as their IO may block and `timeout`
cannot bound a task in uninterruptible sleep: the read-only level's read, which
answers "blocked" within its own budget; the write loop, whose writes are
meant to queue through a failover in the cutoff case, and in react's
migrating leg on md until the primary cuts the leg's path; and the cutoff
case's fence writer on host1, which stops on its first failed write or at
the end of its range. The fence writer runs fio, not dd, and writes and
verifies with direct IO (The cases), and so do the copy case's fio stamps,
each written or read back by one fio run under the watchdog below. The loop
reports through files and keeps to its own namespace: it writes only to the
device the namespace's link named at its start, never creating that device
node, and checks before every write that the device still carries the
namespace's uuid.
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
delivered asynchronously, and stops etcd by its pid file, waiting for it to
exit, before it starts it again on the same data. Every stop of an agent, by
pid or by pattern, sends SIGCONT before SIGTERM, so that an agent a failed
cutoff run left stopped acts on its termination. The cleanup's pattern
sweeps, which also reach a crashed run's processes whose pid files are gone,
sit in helper files on the guest
behind helper verbs, bracketed so they never match the ssh command's own shell,
and qualified by the suite's work directory or its etcd name or, for the fio
of the cutoff case's writer and of a fio stamp, by a job name only this suite
uses, so they never touch another suite's processes.

E2E9. **One dnv suite runs at a time in the lab** (`layout.md`, Directory
tree). The suite occupies every guest it names; no other suite binds its
ports. Nothing can enforce the rule from inside: the suite prints it before
the first ssh and checks what it can, that no port of its block listens and no
nvmet port it needs exists.

E2E10. **Hosts reach namespaces through the cdc, and every path a host holds is
the suite's own, but for nvme-stas's inside the cutoff case.** nvme-cli's
autoconnector is masked at preflight and at every rebuild and read back, and
nvme-stas must be inactive at preflight. The one exception is host0 inside the
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
`DefaultPrimaryUnhealthy`, which the run only prints; `DemotionHoldTimeout`,
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
removes, and a repoint there and back (`architecture.md`, Namespace suspend
semantics); and a disabled disk node and controller node, invisible to the
agents, each proved by a refused allocation that writes nothing
(`architecture.md`, Capacity index keys).

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
`cnagent.md` CN17). Before the clone is created the origin's bitmap is read,
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
two-sided leg alone (`dnv-worker.md` AR8). Two of an operator's repairs
(raid1 only) work on one group, each replacing a leg whose disk node dies as
in react, its agent killed and its nvmet port removed (`architecture.md`,
Repairs, Repair, Retired legs); each dying disk node carries no other side
of the pool — for the first leg `sp get` shows that, since it leaves out
only the sides of repairs, retired legs and clones and the pool then holds
none of them, and for the second the node's own record does — so its death
takes down one leg and its record's side count can be checked. The first
repair takes a fresh new leg, placed outside the group's disk nodes (E2E4),
once the primary's probe has given the dead leg the health epoch a create
requires; it must reach STARTED, read through `repair get` alone, as the
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
primary reports beside its rows, and how md took the leg back is logged
(Known limits). Across the two repairs no cntlr may hold a retired leg;
host0 reads its data while each leg is dead, while md copies, once the group
is whole and at the end, and its kernel log, marked before the first disk
node dies, must report no failed IO, as md fails a member whose IO errs and
serves the read from the other (`cnagent.md` CN12); no failover may happen.
The second retired leg's delete must take its side out of its disk node's
record with the group's extents; that node's returning agent must then
remove the side no record names (`dnagent.md` DN6) until its counts agree
with its record (`dnagent.md` DN18), and the primary must hold every leg
again. The quiet thresholds keep the worker's own leg repair off the first
dead leg, and the group's `operator_in_charge` flag keeps it off the first
repair's new leg once that leg dies, as md's bad-block ranges make it a
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
regions are copied, which fails it over at once (`dnv-worker.md` AR5); the
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
the worker, the cut of a migrating leg whose serving disk node goes silent
and the loss of the primary's machine, on a pool of the run's slice count
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
primary's that reads it running leaves the mark waiting (`cnagent.md`,
Known limits). In the first, a forced start made while the array still runs
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
failover may happen across the take-over. As in the leg repairs before,
host0 makes no IO from the kill until md holds the group whole, so the
stage proves the take-over at the read-write level, not IO during it. The
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
across the stage. The loss of a machine runs last: the primary's
controller-node guest is reset through the kernel's sysrq trigger, with no
sync, which drops every kernel object of the guest as a power loss does, and
the hypervisor starts it again; the guest must answer again under a new boot
id within a bound. Nothing of the old primary survives to advertise a path,
so host0 keeps every path it holds through the failover: the standby must
become primary and build the stack (`dnv-worker.md` AR5), and host0 must
read its data through the path it already held, now optimized. Once the
cdc's records no longer name the reset node, host0 drops its path to it, as
a host that follows the discovery log does, and must hold none; it does so
before it connects the replacement, which keeps the lost cntlr's cntlid
slot, so that a retrying controller of the same controller id does not make
the kernel refuse the new one (`architecture.md`, Known limits). The lost
cntlr must be replaced on a controller node that held none (`dnv-worker.md`
AR7), host0 must connect the replacement through the cdc, its kernel log
since a mark taken before the reset must report no failed IO, and the worker
must have applied one failover and one replacement. Only then does the guest
get its per-boot preparation again, the module loads and the configfs mount
of Preflight, and its cn agent, started from its store, must converge to a
node with no cntlr of the pool and no dnv object (`cnagent.md` CN7): an
agent back before the replacement would answer its rounds and keep the cntlr
it rebuilds.

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

**The ending every case shares** proves that a pool deletes back to nothing:
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
legs — is a logged line, not a proof.

## Preflight

The driver's checks run first; the guest checks run after the start cleanup and
its gate and before the first setup write, since port checks taken before the
cleanup would judge the last run's corpses, and the cleanup writes no suite
state; a cleanup-only run does neither. Preflight dies on the first failure,
naming the guest and the fix. It proves that the driver has its tools, the
binaries, whose build versions it prints, the tree's constants (E2E12) and
a pinned etcd release; that every
guest answers passwordless ssh; that a node guest has what the sweeps and the
agents need — sudo, the tools of both roles (on a controller node, also the
thin metadata reader the cn agent runs, those of the copy case's clone
bucket when that case is in the run and the one of the react case's reject
rule when that case runs with md-raid1, and on a disk node those of the copy
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
rewrite an existing port of its id (`dnagent.md` SH19); that a host is a clean
initiator — sudo, nvme-tcp and multipath, its identity files, generated if
absent and never overwritten, nvme-stas inactive and the masked autoconnector
(E2E10); that host0, when the copy case is in the run, has the tools of its
fio stamps, fio, cmp, readlink and pkill; that host0, when the copy or the
react case runs with md-raid1, has
the tool of the kernel-log witness their repairs read, and, when the react
case runs with md-raid1 and more DN VMs than legs, the tools of the write
loop its migrating leg runs; that host0, when the
cutoff case is in the run, has the tools of the case's write loop and
kernel-log witness and nvme-stas 2.x installed, both its units known to
systemd, as a 1.x one would fail later and silently, never connecting,
nvme-stas being a package the suite never installs; that host1, when the
cutoff case is in the run, has the tools of its fence writer, fio, cmp,
nohup and pkill, which the suite never installs either; and that the
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
sweep's, which runs before them.

## Cleanup, and why the order is what it is

The cleanup (E2E6) is best-effort and tells on itself: every verb prints a
sentinel when it finishes, and the sweep reports every verb that never did,
with the cause read off its status, and every nvmet port it refused or could
not remove, since a leftover port with live ANA groups fails the next suite's
setup. A verb's bound detects a wedge but cannot end a task in uninterruptible
sleep, which a removal or a scan against a suspended device creates, so every
node guest resumes suspended devices before it removes anything. The order
rests on one principle, a holder goes before what it holds, and on what this
lab does when it is broken. First every disk node takes the react case's
silencing rule and the copy case's migration bucket off, whether or not this
run set them, so that the hosts and the controller nodes let go of their
paths to ports that answer and no removal waits on a slowed migration
source, and that step does not finish while a reading still shows either.
Hosts go before every target, as they hold the
controllers, and a subsystem removed under a live controller kills it with DNR
while its port keeps listening and leaves it retrying once the port has
nothing left (`cnagent_integtest.md`, Lab facts). On a host the write loop,
the fence writer and the fio of any fio stamp stop first, so that nothing
writes into a device while its paths go, then nvme-stas, so that nothing
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
(`cnagent.md` L3). The first phase takes the copy case's clone bucket and the
react case's reject rule off first, whether or not this run set them, so
that no removal waits on a slowed source and no later connect is refused,
and does not finish while a reading still shows either. On a disk-node guest
every dnv md array goes before any dm
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
place; a run failed by its end cleanup dumps what that cleanup left. The
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
left stopped, the copy case's clone bucket and the react case's reject rule,
and a disk node's with the react case's silencing rule and the copy case's
migration bucket; the
failure line ends with the
filter that pulls the stage's
records. When the stage connected, the host's kernel log is the record of what
the connect attempted.

## Known limits

* **A multi-case run pays a full rebuild per case** (E2E11).
* **The loss of a machine is a controller-node guest's reset, on react's
  pool.** The suite reaches its guests by ssh alone (E2E1), so react resets
  the primary's guest through the kernel's sysrq trigger, which drops every
  kernel object as a power loss does but leaves the hypervisor's machine
  running; no disk-node guest loses its power, and the step runs at react's
  slice count only.
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
* **Without redundancy the suite is smaller**: copy's forced drop and two
  repair stages and react's two leg repair stages, two forced-start stages,
  take-over stage and migrating-leg stage are skipped.
* **How md takes a retired leg back is logged, not asserted.** md catches a
  retired leg taken back up from its write-intent bitmap only while the
  array has stayed degraded since the leg left, and otherwise copies it
  whole (`architecture.md`, Repair, Known limits). The copy case's array
  stays degraded from the leg's departure to its return, but in between md
  copied part of the data onto the leg that replaced it, which then died,
  and whether md still catches the returned leg up from the bitmap after
  that is not established. No host writes the group meanwhile, so either
  way ends with the same bytes; the case logs which way md went, from
  mdadm's word on the add and the sectors written to the returned leg, and
  asserts the whole group and the data, not the way.
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
* **The cutoff case's writer can meet a known limit.** A failover while
  the old primary still serves can lose a write it acknowledged in the
  cases `architecture.md`, Known limits, A failover while the old primary
  serves can lose an acknowledged write, lists (`dnv-worker.md` RW24). The case's second cut is such a failover, and
  its worker has the old primary's reports, so the round runs; a side of
  the round that reports its fence later than the round's bound can still
  cost the write, and fio's verify then fails the case on a block before
  the failed write, a failure of that limit and not of the suite.
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
  slice count (`cnagent.md`, Known limits). The case bounds that wait only
  by the build's own and logs how long the write took.
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
