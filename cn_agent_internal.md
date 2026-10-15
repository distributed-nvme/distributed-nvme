# The cn agent inside: how the code works

This doc explains how the cn agent works, as the code does it at commit
33c07d3 on the dev branch. It is written for a reader who wants to
understand the implementation and then make it faster. It describes; it
proposes nothing.

Part 1, the sections "What this doc is" to "Where the time goes, in short",
can be read alone. Part 2 follows the agent through its life: the start of
the program, the node syncup, the cntlr converge, the check rounds, the
background loops, the rare paths, three worked examples, and where the time
goes in full. The last section lists where the code and the tracked docs
differ. An appendix maps the files.

## 1. What this doc is

**How it is written.** The core words of `doc/core_glossary.md` are used as
they are. Every other project word is explained in the next section, and
the prose uses it only after that. Where the code's name for a thing
differs from the word used here, the "In the code" paragraph at the end of
the section gives the code's name. Each such paragraph names the files and
functions of the section, so the reader can jump to the code. File paths
are relative to the repository root. Three packages come up again and
again: `agent/cnagent` is called the cn package, `agent` the shared agent
code, and `common` the common code. There are no line numbers: they go
stale with the next edit. Constants are named with their value at this
commit.

**Pointers to the tracked docs.** Where a rule of a tracked doc applies, the
sentence states the rule and then gives the pointer in brackets, for
example: a converge sweeps first and builds afterwards (`cnagent.md`, CN9).
The tracked docs own the rules; this doc describes the code. Where the two
disagree, the section "Where the code and the tracked docs differ" says so.

**Numbers.** Counts of processes and file reads are read from the code and
given as formulas of the pool's shape. Three unit costs were measured on
the lab's controller-node guests on 2026-10-12 and 2026-10-14 and are quoted
as such. Nothing was run for this doc.

**Shape used in the examples.** When a number needs a concrete shape, the
doc uses the e2e suite's default: one storage pool of 32 slices, RAID1
groups of two legs, one meta group and one data group per slice, so 64
groups and 128 legs, two cntlrs, one thin device and one namespace in one
subsystem.

## 2. Words used here

The words are in reading order. Each entry says what the thing is in core
words and ordinary Linux words.

**pass** — One run of the agent over one cntlr while it holds that cntlr's
lock: a converge, a check round or an info read. The full glossary uses
"pass" for a converge only; this doc uses the wider sense because the three
share the lock, the plan and most of the reads.

**info read** — The read of one cntlr's rows, or one node's rows, that the
gateway asks the agent for when an operator runs `dnvctl cntlr inspect` or
`dnvctl cn inspect`. It does the same work as a check round and holds the
same locks.

**info** — The set of rows of one cntlr or one node, one map of rows per
kind of device, export or connection. The proto messages are `CntlrInfo`
and `CnInfo`.

**reading** — What a check round or an info read of a cntlr takes of the
whole node first: one device-mapper listing of every device with its number
and flags, one listing of every device's table, one NVMe walk and one md
walk. It is taken once per pass and shared with nothing else. A converge
takes no reading; it reads each of its own devices one at a time.

**NVMe walk** — The agent's read of every NVMe-oF connection the node holds.
The kernel keeps one numbered entry under `/sys/class/nvme-subsystem` for
each connected subsystem, and gives a freed number to the next new
subsystem. The walk lists the entries and reads each entry's NQN. A
**lookup** then reads one entry's directory: the namespace node in it and
each controller's address, state and ANA state.

**md walk** — The read of the md arrays under `/sys/block`: for each array,
its member devices and their device-mapper names.

**verdict** — The read-only comparison of the node with the desired state.
It is clean, or it names the leftovers and the listings that did not
answer. In a converge it is what the sweep's removals left behind; in a
check round and an info read it is computed without removing anything. It
becomes the reply's code: clean is zero, anything else is the leftover
code.

**OS client** — The one door through which the agent runs a command, lists
a directory or reads or writes a file. It has a fixed number of slots; one
operation holds one slot from its start to its end, a command's wait for
its child included. It writes one log record per operation.

**time limit** — Every operation runs under a limit of 3 s. A command gets
SIGTERM at 3 s and SIGKILL at 5 s. A command stuck in an uninterruptible
kernel wait outlives both, and the agent waits for it. For a file read or a
directory listing in the agent's own process the limit bounds only the wait
for a slot: the read itself cannot be ended, and a sysfs read the kernel
holds, as the NVMe tree can during a controller reset, holds the pass
without bound.

**did not answer** — A read or a command that told the agent nothing: it was
killed, never started, got no slot in time, or failed with any error other
than "not there". It is never read as "absent": an absent object licenses a
removal, and an unanswered read must not. This is the rule of every read
that decides a removal or an enumeration. The probe-first attribute writes
and the nvmet probes read tolerantly instead: there a read that failed
counts as a missing or differing attribute, which the converge then writes
and a probe reports as a mismatch.

**partial listing** — A whole-node `dmsetup` listing that printed some lines
and exited non-zero because a device went away during it. Its lines count.
A device of this cntlr that it lacks is read on its own, once, and the
answer is kept for the pass.

**plan** — The desired state of one cntlr turned into device names, sizes,
tables and ANA groups. It is a pure function of the stored request, built
afresh for every pass; the converge and the check round work from the same
plan, so the two cannot drift apart.

**effective desired state, deferred, provisioning** — The plan with every
part that still waits for a side's zeroing taken out. A leg all of whose
sides are unprovisioned is *provisioning*: it has no connection, no wrapper
and no row but "provisioning". A group with such a member is *deferred*: it
is not built and reads provisioning. The groups after it in the same list
are built and read ok, but stay out of the concat until it clears, because
a concat's targets sit at offsets that are the sum of the lengths before
them. A slice that has no effective group left on one of its two sides is
deferred whole, and so are every thin device of the pool, since the raid0
stripes across every slice, and the namespaces and clones over such a thin
device. A deferred thing is not built and is reported as provisioning, which
sets no health epoch.

**wanted set** — The names of the devices, md arrays, exports and
connections the plan wants to exist, deferred ones included. The sweep
removes what the node holds and the wanted set does not name.

**chain, layers** — The sweep's list of one storage pool's unwanted things,
grouped by the layer that removes them: a park step first, then ten layers
from the exports down to the leg connections. Removal runs top-down and
stops at the first layer that left something behind.

**attribution** — Deciding whose a device, array, export or connection is:
from its name where the name carries the cluster, node and pool ids, and
from what it is built of where it does not.

**base state** — What a controller node holds once, for every pool on it:
a tmpfs mount, the metadata area as a sparse file in it with one loop
device over the file, and the nvmet port with its three ANA groups. The
code calls the metadata area's loop device the arena.

**wrapper** — The leg wrapper: a one-target dm-linear the agent puts over a
leg's NVMe multipath device, sized from the desired state. md and the group
devices use the wrapper as the leg, never the kernel device.

**concat** — One of the two multi-target dm-linears under a slice's thin
pool. The meta concat joins the slice's meta groups end to end and is the
pool's metadata device; the data concat joins its data groups and is the
pool's data device.

**namespace device** — The dm-linear that nvmet exports for one namespace.
Its table points at the thin device's raid0 while the namespace serves, at
the thin device's dm-error while it is parked, or at a clone's dm-clone
while a clone fills the thin device; under a read-only level a serving one
is a dm-flakey table that fails writes instead of a dm-linear. The code
calls it the ns-dev.

**park, parked** — A namespace device reloaded onto its thin device's
dm-error. It stays live, never suspended: the standby's state, the old
primary's state after a demotion, and the state of a suspended namespace.

**hydration** — dm-clone's word for its background copy of the source into
the destination, region by region. A region that has been copied, or that
the agent has marked with a discard as needing no copy, is hydrated. A
dm-clone's status line says whether hydration is on.

**late member** — A leg in a group's member list that is not available when
the primary converges the group: no path that is both live and ANA
optimized, or its connect, its namespace or its wrapper failed this pass.
The group is left unbuilt, or assembled without the leg.

**connect retry** — The background loop that re-runs a cntlr's whole
converge every 5 s while something transient stopped the last one: a leg
or a clone source failed, a group has a late member, a namespace device was
held off its raid0, or a clone build stopped before its last step.

**clone-metadata retry** — The same loop at 60 s, for a clone that waits for
a free run in the metadata area.

**health probe** — The primary's test of a leg: every 5 s a goroutine of its
own writes 4 KiB to the leg's health block, the last 4 KiB of the leg's
meta region, and reads it back with O_DIRECT. The code calls the goroutine
a prober.

**disconnect registry** — The set of subsystem NQNs whose `nvme disconnect`
is running, or waiting to run, in the background. A connect step refuses to
touch a connection that is in it.

**local store** — The directory of files, by default `/var/lib/dnv`, that
holds the last accepted request of the node and of each cntlr, and one file
per received bitmap chunk.

**tracker** — The per-cntlr memory of each row's last status and of the
time the status last changed, the row's epoch. The epoch moves only when
the status changes, not when the row's text changes.

**torn snapshot** — A snapshot whose per-slice create messages were sent in
different runs, so its slices describe different instants. The agent marks
it until the worker records the mark.

**stale walk** — After every `nvme connect`, the pass marks its NVMe walk
stale, so the next lookup walks again: the new subsystem may have taken the
number of an entry freed since the walk.

## 3. What the worker expects of the agent

The agent never talks to etcd. Everything it knows comes from the worker,
and the worker reads the agent through two channels: syncups and check
rounds. The costs of the agent matter because of what the worker does with
late or missing answers.

**Check rounds.** The worker keeps one check stream open per controller node
and one per cntlr. On each it sends one request per check interval. The
interval is a cluster setting with one value per object kind; the default
is 2 s (`DefaultHealthCheckInterval`). The worker waits for the reply at
most one interval, and a reply that comes later counts as no reply
(`dnv-worker.md`, RW8). The request carries the desired revision, a flag
that asks for the full info, and the round's trace id, under which the
agent runs the round (`architecture.md`, Check streams).

**What a missed round does.** For a cntlr, a round with no reply in time, a
stream that cannot be opened or breaks, or any error row of the reply other
than a leg row sets the cntlr's health epoch, and the next clean round
clears it (`dnv-worker.md`, HL2). A leg row that reads error sets the leg's
health epoch, from the primary's report only. A set epoch on an enabled
standby takes it out of the discovery log that the cdc gives hosts, and a
clear epoch puts it back; a primary stays listed (`architecture.md`,
[D18]). So a standby whose rounds are slow flaps in and out of the hosts'
view although it is healthy.

**What a reply makes the worker do.** After each round, when the reply's
code is not zero or the reply's revision differs from the desired one, the
worker sends the syncup again (`dnv-worker.md`, RW4, step 5). A leftover
reply is such a code. So a leftover, or a listing that did not answer, costs
one syncup per round until the node reads clean. The worker remembers no
re-send flag: the code in each reply is the whole state.

**Syncups.** A syncup carries the whole desired state of the node or the
cntlr and runs under a deadline of 60 s (`DefaultWorkerSyncupTimeout`;
`dnv-worker.md`, RW5). The agent refuses a syncup whose revision is below
the one it holds, one for a cntlr whose pointer its node request does not
list, and one whose conf carries a zero where the control plane always
writes a value; the worker logs a refusal and tries again at the next
round. An accepted reply's rows are read for health; its thin rows drive the
created flip, which the worker sets once a thin device's row reads ok in
every slice (`dnv-worker.md`, RW19); its list of applied bitmap chunks
tells the worker which chunks to push again.

**The reactions that read the cntlr.** A failover fires when the primary is
disabled, or when its health epoch is older than the primary threshold
(default 4 s, `DefaultPrimaryUnhealthy`; the cntlr threshold while the
primary is still settling) and the coordinator's own last two round
verdicts on it were both unhealthy (`dnv-worker.md`, AR5, AR10). Two missed
rounds in a row behind a long converge are two unhealthy verdicts. A cntlr
replacement fires when a cntlr's epoch is older than the cntlr threshold,
default 600 s (`dnv-worker.md`, AR7). Leg repair reads the primary's leg
rows (`dnv-worker.md`, AR8). The auto-grow reads the thin pool row's text,
which is the raw `dmsetup status` line of the pool (`dnv-worker.md`, AR6).

**The hold.** A fan-out that demotes a primary hands the old primary its
demotion first and holds the sides and the other cntlrs until it reports
the demotion applied, at most 3 s (`DemotionHoldTimeout`; `dnv-worker.md`,
RW22). Then the sides get their syncups, and the other cntlrs get theirs
once the healthy sides have reported, or after one check interval
(`dnv-worker.md`, RW14). Both waits are bounded, so a promotion can reach
the new primary before the sides' ANA flips have reached its kernel
(`architecture.md`, [D16]). The agent is written for that: a leg whose path
is not yet optimized is a late member, and the connect retry re-runs the
converge until the member is available.

**Bitmap pushes.** The worker pushes a clone's source bitmaps one chunk at a
time. The agent stores each chunk in its local store before it applies it,
and lists the chunks it holds in every syncup reply.

**Trace ids.** Every request carries a trace id. The agent runs the request
under it, so one id joins the worker's send, the agent's log records and
the agent's reply. A round's id is in its request, not in the stream's
metadata. Work the agent starts on its own, a retry attempt or a health
probe, runs under an id the agent makes.

**In the code.** The worker side is outside this doc. On the agent side, the
check streams are served in `check.go` of the cn package (`CheckCn`,
`CheckCntlr`); the per-round trace id comes from `agent.CheckRoundCtx` in
`agent/agent.go`. The reply codes are the `ReplyCode*` constants of
`common/constants.go`, and the replies are built in `agent/revision.go`
(`GateRevision`, `UnknownObjectReply`, `InvalidConfReply`, `LeftoverReply`).
The syncup deadline and the thresholds are constants of
`common/constants.go` (`DefaultWorkerSyncupTimeout`,
`DefaultPrimaryUnhealthy`, `DefaultCntlrUnhealthy`, `DemotionHoldTimeout`).

## 4. What a controller node holds

A controller node runs one cn agent. The agent builds and removes devices
in four kernel subsystems: device-mapper, md, the NVMe host side (the
connections to the sides) and the NVMe target side (nvmet, the exports to
hosts). Everything it builds has a name that says whose it is.

### The device stack of a primary

The picture below is one thin device with one namespace, bottom-up. Every
box is one kernel object; the arrows point from the device that is used to
the device that uses it.

```
   host  <==  NVMe-oF, path ANA optimized  ==  nvmet subsystem, namespace nsid = ns_idx
                                                  |
                                         namespace device  (dm-linear)
                                   table: -> raid0   while the namespace serves
                                          -> dm-error  while it is parked
                                          -> dm-clone  while a clone fills the thin device
                                                  |
                            raid0 (dm-striped) over the thin device's volumes, in slice order
                            +---------------------+--------- ... ---------+
                            |                     |                       |
                    thin volume, slice 0   thin volume, slice 1    thin volume, slice N-1
                            |
                    thin pool of the slice (dm thin-pool)
                       metadata device = meta concat      data device = data concat
                            |                                     |
                    meta group(s), end to end             data group(s), end to end
                    each group: md RAID1 array over its legs' wrappers,
                                or one dm-linear over the one leg's data region
                            |              |
                       leg wrapper     leg wrapper   (dm-linear over the multipath device)
                            |
                    NVMe multipath device of the leg: one controller per connected side
                            |
                    nvme connect to each provisioned side's export (the NQN names the leg)
```

Beside this stack a primary holds, per thin device, a dm-error of the thin
device's size: the permanent target that a namespace device is parked on.
For a clone it holds the dm-clone, whose metadata device is a small
dm-linear over a run of the metadata area, and a connection to the clone's
source. For a transfer it holds a dm-linear over the origin's raid0 and a
subsystem of its own with one namespace.

Three facts of the stack drive most of the agent's work:

1. **Every leg is one NVMe subsystem on the host side.** All sides of a leg
   export the same NQN, so the kernel joins them into one multipath device
   with one controller per side, and a migration is an ANA flip between two
   paths of that device; nothing above the leg changes (`architecture.md`,
   [D1]). The agent finds the device by reading sysfs, never by running
   `nvme list-subsys`, because that command shows no ANA state without a
   device argument, and with one it answers a namespace whose every path is
   inaccessible with an empty list, which looks like "not connected".
2. **The wrapper is the leg.** The agent puts a dm-linear over the multipath
   device and names it with the cluster, node, pool and leg ids. md and the
   group devices use the wrapper. A sweep can then attribute an array by its
   members' names, and the wrapper's table is the one place that says which
   kernel device a leg is.
3. **A dm table names a device by its number.** Every table the agent
   writes, and every table it compares a live device against, holds device
   numbers of the form major:minor. The agent reads a dm device's number
   with `dmsetup info`, and an md array's, a multipath device's or a loop
   device's from `/sys/block/<name>/dev`. Reading numbers is a large part of
   what a pass does.

### What a standby holds

A standby keeps the leg connections and the wrappers, so it can take over
without a connect; every path of a standby's leg is ANA non-optimized,
because the disk node exports a dm-error to every controller node but the
primary. It keeps the per-thin-device dm-error, every namespace device
parked on it, and the subsystems and namespaces with every namespace in the
inaccessible ANA group. It has no md array, no concat, no thin pool, no thin
volume, no raid0, no dm-clone and no health probe. For a transfer it keeps
the transfer device as a dm-error of the right size, and the transfer's
subsystem and namespace, inaccessible (`architecture.md`, Standby cntlr).

### The base state

Once per controller node, for every pool on it, the agent holds the base
state (`architecture.md`, Controller node, common):

* a tmpfs mounted at `/tmp/dnv-tmpfs/<cluster>-<cn>`, 2 GiB
  (`DefaultCnTmpfsSize`);
* in it a sparse file named `tmp-file` of 1 GiB (`CnCloneMetaAreaSize`),
  with exactly one loop device over it: the metadata area. It is carved into
  256 units of 4 MiB (`CnCloneMetaUnit`); a clone's metadata device is a
  dm-linear over a run of whole units. The dm tables of those dm-linears are
  the allocation record: there is no file that lists the used units. The
  area is empty after a reboot, and so is the kernel's dm state, so the two
  are always in step;
* one nvmet port, at configfs id 1 unless the flag says otherwise, with the
  node's transport address and three ANA groups whose states never change:
  group 1 optimized, group 2 non-optimized, group 3 inaccessible
  (`architecture.md`, [D4]). Every ANA move the agent makes rewrites a
  namespace's group id; no move ever writes a group's state.

### Names

Every device-mapper device the cn agent makes is named
`dnv-<cluster>-<cn>-<kind>-<sp>-...`, with the ids as 16 hex digits and the
kind as the letter `c` plus one hex digit (`architecture.md`, dm device
names). The pool id comes right after the kind, so a sweep groups a node's
devices by pool from the names alone. The kinds:

| kind | device | one per |
|---|---|---|
| c0 | meta concat | slice |
| c1 | data concat | slice |
| c2 | thin pool | slice |
| c3 | thin volume | thin device and slice |
| c4 | raid0 | thin device |
| c5 | dm-error | thin device |
| c6 | namespace device | namespace |
| c7 | dm-clone | clone |
| c8 | transfer device | transfer |
| c9 | leg wrapper | leg |
| ca | group device of a group without RAID1 | group |
| cb | clone metadata device | clone |

An md array has three names. The kernel node, `mdN` or `md_<name>` under
`/sys/block`, is what the agent reads the array by, takes its number from
and stops. The array name `dnv-<sp>-<slice>-<grp>` goes into the superblock
as the name of `mdadm --create` and `--assemble`; its slice field carries a
high bit for a meta group. The device path under `/dev/md`, a short hash of
the cluster and node ids followed by the pool, slice and group fields, is
the array device that `mdadm --create`, `--assemble`, `--add`, `--fail` and
`--remove` take, and the one `lsblk` probes it before a create. The agent
never reads the array through that path, which depends on udev having run
(`architecture.md`, md names).

The NQNs the cn agent uses carry a kind digit and ids too: a side's export
to this controller node is `nqn.2024-01.io.dnv:2:<cluster>:<sp>:<leg>:<cn>`,
the agent's own host NQN is kind 1 with the cluster and cn ids, and a
transfer's subsystem is kind 4 with the cluster, pool and transfer ids
(`architecture.md`, NQNs). A host-facing subsystem has the NQN the operator
chose, which carries nothing; the agent attributes it by the namespace
devices behind its namespaces, or by the request of a cntlr that still
names it.

The local store, by default `/var/lib/dnv`, holds `cn-<cluster>-<cn>` with
the node's last accepted request, `cntlr-<cluster>-<cn>-<sp>-<cntlr>` with
each cntlr's, and `clone-bm-<cluster>-<cn>-<sp>-<clone>-<slice>-<idx>` for
each received bitmap chunk (`architecture.md`, Agent local-store paths).

**In the code.** The names are made by `common.NameFmt` in
`common/name_fmt.go` (`CnLegName`, `CnGrpName`, `CnPoolMetaName`,
`CnPoolDataName`, `CnPoolFinalName`, `CnThinDevName`, `CnRaid0Name`,
`CnErrorName`, `CnNsDevName`, `CnCloneFinalName`, `CnCloneMetaDmName`,
`CnXferFinalName`, `CnMdDevName`, `CnMdArrayName`, `SideToCnNqn`,
`CnHostNqn`, `XferNqn`, `LocalCnPath`, `LocalCntlrPath`, `LocalCloneBmPath`)
and read back by `common.ParseDmName` and `common.ParseNqn` in
`common/name_parse.go`. The kinds are
the `DmKind` constants of the same file. The base state is built by
`convergeCn` in `syncup_cn.go` of the cn package with the wrappers of
`clonemeta.go` (`CloneMeta`); the metadata area is the `cloneMetaArena`
type there, the code's "arena". The tables of the stack are built by
`dmutil.go` (`dmTable`, `ensureDmSingle`, `ensureDmLinear`,
`ensureDmMulti`, `buildConcat`), `td.go` (`raid0Args`, `nsDevTable`),
`pool.go` (`poolArgs`) and `agent/dm.go` (`LinearTable`, `ErrorTable`,
`FlakeyErrorWritesTable`, `CloneTable`).

## 5. How the process is organized

### One server, one OS client

The program `dnv-agent cn` is one process with one gRPC server that serves
ten calls: the capacity call, the node syncup, the cntlr syncup, the bitmap
push, the node info read, the cntlr info read, the two bitmap reads, and
the two check streams. Every call runs on its own goroutine, as gRPC does
it; the locks below are what serializes them.

The process has one OS client. Every command, directory listing, file read
and file write goes through it. It has 32 slots (`DefaultOsClientLimit`),
and the number is not configurable. One operation takes one slot when it
starts and gives it back when it ends; for a command that is the whole life
of the child process. The wait for a slot runs under the same 3 s limit as
the operation, so when every slot is busy an operation fails after 3 s
without having run. The limit is per operation, not per pass: a pass of
a thousand commands may take an hour as long as each command answers within
3 s.

The leg health probes do not use the OS client. Their reads can block for
as long as a leg has no serving path, which can be for ever, and a blocked
probe must not hold one of the 32 slots that the teardown commands need
(`osclient.md`, Exported raw helpers and the probe-IO carve-out).

### The locks

There are two levels of locks, and a pass holds them for its whole run.

```
   node lock (read/write)
       write: the start of the agent, a node syncup
       read:  every other call that touches a cntlr or the node
                |
                +-- cntlr lock, one per cntlr (plain mutex)
                        held by: a cntlr syncup, a retry attempt, a cntlr check round,
                                 a cntlr info read, a bitmap push, a bitmap read
                |
       read only (no cntlr lock): a node check round, a node info read
       no lock at all: the capacity call, the health probes, the background disconnects
```

| who | lock | held for |
|---|---|---|
| the start of the agent | node, write | until every node request and every cntlr has converged; the agent listens only afterwards |
| node syncup | node, write | the base state, dropping removed cntlrs, the node-level sweep with its teardown chains |
| cntlr syncup | node read + the cntlr's lock | the whole converge, then the store save |
| retry attempt (connect retry, clone-metadata retry) | node read + the cntlr's lock | the whole converge; no deadline, since no RPC is behind it |
| cntlr check round, cntlr info read | node read + the cntlr's lock | the reading, the rows and the verdict |
| bitmap read of a thin device or a leg | node read + the cntlr's lock | one metadata dump of one thin pool |
| bitmap push | node read + the cntlr's lock | the store save, the fold of every chunk and the discards |
| node check round, node info read | node read | the base-state probe and the node-level verdict |
| capacity call | none | nothing is read |
| health probes, background disconnects | none | they only publish into registries the passes read |

Three consequences follow (`cnagent.md`, CN1, the lock mapping).

1. **A check round waits for the converge in front of it.** A cntlr syncup
   or a retry attempt holds the cntlr's lock for its whole converge, and the
   round of the same cntlr cannot start before it ends. The worker waits one
   check interval for the round's reply, 2 s by default, so a converge of
   more than 2 s costs one missed round, and two missed rounds in a row are
   two unhealthy verdicts.
2. **Different cntlrs run side by side.** Each has its own lock, and the
   node lock is shared for reading. What they share is the OS client's 32
   slots and the metadata-area lock below.
3. **A node syncup stops everything.** It takes the node lock for writing.
   Go's read/write mutex gives a waiting writer priority: once a node syncup
   waits for the lock, no new read holder gets in. So a node syncup that
   arrives during a long converge stalls every new check round, push and
   retry attempt on the node until that converge ends and the syncup itself
   is done. The syncup's own sweep may run teardown chains, which can be
   long.

Two small locks sit below these. A leaf mutex guards the in-memory maps and
registries; it is never held across an OS operation. The metadata-area lock
serializes the allocation of a clone's metadata run across the cntlrs of
the node and is held across the OS operations of one allocation: list the
area, discard the run, create the dm-linear. It is also taken when a sweep
removes a clone metadata device. Each cntlr's stored request is an atomic
pointer, so other cntlrs' passes and the node-level verdict read it without
any lock.

### Time limits

Every OS operation runs under the soft limit of 3 s (`CmdSoftTimeout`). For a
command the limit sends SIGTERM; 2 s later, at the hard limit of 5 s
(`CmdHardTimeout`), SIGKILL follows. For a file read, a directory listing or a
file write in the agent's own process the limit bounds only the wait for a
slot: the context is checked once before the call, and the call itself cannot
be ended. Most of a pass's operations are such reads, and a sysfs read the
kernel holds, as the NVMe tree can while a controller resets or is torn down,
holds the pass and its locks without bound. A command that sits in an
uninterruptible kernel wait, such as a controller delete whose target vanished
or a configfs operation that waits for a directory lock, survives both
signals, and the agent waits for it. That is why the sweep issues its `nvme
disconnect` in the background and not under the locks.

A command that was killed or never started "did not answer". The agent
never reads that as "the object is gone": a killed `dmsetup remove` may
still have completed in the kernel, so only a fresh probe can say. Every
removal is therefore followed by a probe of the removed object.

### Background goroutines

| goroutine | how many | started by | stopped by |
|---|---|---|---|
| connect retry | at most one per cntlr | a converge that found a transient failure | a converge that finds none; the disable level; the cntlr's drop |
| clone-metadata retry | at most one per cntlr | a converge whose clone found no free run in the metadata area | a converge with no clone waiting; the disable level; the drop |
| health probe | one per leg of a primary, spares included | the leg's converge on a primary | the next converge that no longer wants it; the drop; the agent's exit |
| background disconnect | one per subsystem NQN being disconnected | a sweep that found a connection unwanted | its own end |
| stop watcher | one | the start of the agent | the agent's exit |

The retry loops tick on a timer. An attempt that outlasts its interval
finds the next tick already waiting and starts the next attempt at once, so
under a slow converge the cntlr's lock is held almost without a gap. The
two retries of one cntlr can run at the same time, each attempt a whole
converge. At most eight background disconnects run at once
(`disconnectConcurrency`, a quarter of the OS client's slots); each holds
an OS client slot for its whole run, up to the kernel's admin timeout of
60 s when the target has vanished.

### What lives across passes, and what does not

Nothing a pass reads of the node is kept for the next pass: the reading,
the NVMe walk, the md walk, the view of the metadata area and the plan are
all built afresh and thrown away. The rule is that a read-only call reads
the node afresh under its locks (`cnagent.md`, CN23).

What does live across passes, in memory only:

* the stored requests of the node and of each cntlr, which are also in the
  local store;
* the tracker of each cntlr and of the node, with every row's last status
  and epoch;
* the mirror of each cntlr's bitmap chunk files;
* three marks of a cntlr: the slices whose thin pool this process created
  and has not yet swept for orphan thin ids, the thin devices whose
  snapshot came out torn, and whether the stored request came by the RPC
  of this process;
* the registries: the health probes of each cntlr with their last outcome,
  the retry flags, and the disconnect registry;
* per check stream, the last info sent, so a round sends the info only when
  it changed.

### Logging

Every OS operation writes one log record. A command's record carries its
arguments, its exit code and its whole standard output and error: a
whole-node `dmsetup table` listing lands in the log in full, once per check
round. A file read's record carries the first 128 characters of the data
(`LogStrDataLimit`); a directory listing's carries every name. Every gRPC
request and reply, and every message of a check stream, is logged as the
message rendered to JSON, with every bytes field, a bitmap, replaced by its
length. A cntlr syncup's request at 32 slices is
large, and it is logged twice: once on receive, once more when the store
saves it. Every record carries the trace id of its pass.

**In the code.** The server type is `CnAgentServer` in `server.go` of the
cn package; its ten calls are the methods of that file, `check.go`
(`CheckCn`, `CheckCntlr`) and `bitmapread.go` (`GetThinDeviceBm`,
`GetLegBm`). The gRPC server is started by `agent.Serve` in
`agent/agent.go`, after `Reconcile` has returned. The OS client is
`common.LimitedOsClient` in `common/osclient.go`, made once in `runCn` of
`cmd/dnv-agent/main.go`; its slot is the semaphore in `RunCommand`,
`ReadFile`, `ReadDir` and the write methods. The time limits are applied by
`cmdCtx` in `agent/oswrap.go` and by `cmd.Cancel` and `cmd.WaitDelay` in
`RunCommand`. The locks are `agent.LockSet` in `agent/locks.go` (`Node`,
`Obj`, `DropObj`), the leaf mutex is `CnAgentServer.mu`, and the
metadata-area lock is `CnAgentServer.cloneMetaMu`, taken in
`ensureCloneMeta` and `reconcileCloneMeta` of `clonemeta.go` and in
`removeCloneMetaDmVerified` of `sweep.go`. The retry loops are
`connectRetryLoop` and `reconvergeCntlr` in `leg.go`, started by
`startConnectRetry` and `startCloneMetaRetry`; the probes are
`legProbeLoop` and `runLegProbe` in `healthcheck.go`; the disconnects are
`startDisconnect` in `leg.go`. The per-stream last info is `lastSent` in
`check.go`. The log records come from `common/osclient.go` and from the
interceptors in `common/interceptor.go`; `PbToLogValue` in `common/log.go`
renders a message.

## 6. Where the time goes, in short

A pass spends its time in three places: starting processes, reading files
in its own process, and waiting for the kernel inside a command. Starting a
process is the expensive one. Three unit costs were measured on the lab's
controller-node guests on 2026-10-12 and 2026-10-14:

| operation | measured cost |
|---|---|
| one process (`dmsetup`, `ls`, `nvme`, ...) | 2 to 5 ms |
| one file read or directory listing in the agent's process | about 0.04 ms |
| one `dmsetup status` of a thin pool | 9 to 33 ms |

A thin pool's status read is slow because the kernel first commits the
pool's metadata, and flushes its data device when the metadata changed
since the pool was set up (`dnagent.md`, SH17). A dm-clone's status read
commits that clone's metadata the same way.

The table below gives, for the example shape of the opening section, what
each kind of pass costs and who waits for it. The counts are read from the
code and are approximate; the section "Where the time goes, in full" gives
the formulas and the steps. The times are the lab's: those marked "before"
were measured at commit 8bb1f25, before the check round took one reading
of the node, and those marked "after" at 33c07d3. The converge itself was
not re-measured after that change; the change took about 190 `lsblk` and
about 500 `ls` processes out of it.

| pass | lock held | processes | file reads | lab time | who waits behind it |
|---|---|---|---|---|---|
| primary converge, nothing to change | node read + cntlr | about 1,010, nearly all `dmsetup` | about 2,200 | 4.5 to 5.4 s before, at 8 vCPUs | the cntlr's rounds, info reads, pushes, bitmap reads, the other retry |
| standby converge, nothing to change | node read + cntlr | about 535 | about 1,200 | about 3 s before | the same |
| primary check round or info read | node read + cntlr | about 41, of which 32 thin-pool status reads | about 970 | 0.36 to 0.54 s after | the cntlr's syncup and retry |
| standby check round or info read | node read + cntlr | about 9 | about 1,030 | 0.03 to 0.08 s after | the same |
| node check round or info read | node read | about 7 | about 400 | about 0.3 s before | a node syncup |
| connect retry attempt | node read + cntlr | a whole converge | a whole converge | 21 to 25 s per attempt during a build, back to back | the cntlr's rounds |
| node syncup | node, write | the base state, about 6, plus the node sweep's 4, one `dmsetup table` per clone metadata device and per live dm-clone on the node, and its removals | the node's arrays and connections | not measured | every new pass on the node |

What the numbers mean for the worker's 2 s wait:

1. **A converge of the primary is longer than a check interval.** Every
   change of the pool's desired state, a thin device created, a namespace
   added, a host granted, costs one converge on every cntlr, and a round
   that arrives meanwhile waits it out. At 32 slices one converge is about
   a thousand processes; at 2 to 5 ms each that is 2 to 5 s under the lock.
2. **The converge is serial.** Nothing inside one cntlr's converge runs in
   parallel: legs, groups, slices, thin volumes, namespaces and subsystems
   are handled one after another, and so are the sweep's removals.
   Parallelism exists only between cntlrs.
3. **The converge grows with thin devices times slices.** Each thin device
   costs about four `dmsetup` runs in every slice, plus one per slice for its
   raid0, even when nothing changes. A pool with 64 thin devices at 32
   slices is near 11,000 processes per converge.
4. **A retry attempt is a converge without a deadline.** While a side is
   dead, a leg is late or a clone source is missing, the connect retry
   re-runs the whole converge every 5 s. An attempt longer than 5 s is
   followed by the next one at once, so the lock is held almost all the
   time, and every round of that cntlr misses.
5. **The check round is now cheap, except for the status reads.** After the
   change at 33c07d3 a round starts a few processes whatever the slice
   count, plus one `dmsetup status` per thin pool. Those 32 reads of 9 to
   33 ms each are most of a primary round.
6. **The same device is read several times in one converge.** A leg's
   wrapper is read twice, a thin pool's device number once per thin device,
   a thin volume four times, a namespace's nvmet attributes three times.
   The section "Where the time goes, in full" lists them.
7. **The log is a cost of its own.** Every one of those operations writes a
   record, a command's with its whole output, and every request and reply
   is logged as JSON. One check round of a standby at 32 slices writes
   about a thousand records.

**In the code.** The counts come from the loops of `build` in
`syncup_cntlr.go` and `probeCntlr` in `probe.go` of the cn package, and
from the enumeration in `sweep.go` (`enumerateCn`, `buildChain`). The
per-device reads of a converge are `Dm.Info`, `Dm.InfoDevNo`, `Dm.Table`
and `Dm.Status` in `agent/dm.go`; the whole-node reads of a round are
`Dm.ListInfo` and `Dm.ListTables` there, taken by `takeReading` in
`reading.go`. The measurements are recorded in
`tmp_doc/large_slice_cnt_issue.md` and `tmp_doc/large_slice_cnt_plan.md`.

# Part 2: the passes in detail

## 7. The shape of the desired state

Every pass over a cntlr starts from the cntlr's request: the last cntlr
syncup the agent accepted, held in memory and in the local store. The
request is the whole desired state of the cntlr, never a change
(`architecture.md`, Common agent rules). In words, it carries:

* **the ids**: the cluster, the controller node, the pool and the cntlr,
  and the revision;
* **the pool's geometry**: the thin pool's block size and its low-water
  percentage, the raid0's stripe size, and the redundancy kind of the
  groups, none or md RAID1 with the bitmap chunk, the number of pool blocks
  one bit of the array's write-intent bitmap covers. A zero block size,
  percentage or stripe size, or a zero chunk with RAID1, makes the agent
  refuse the request,
  since the control plane always writes a value and a geometry the agent
  invented would not be the one the rest of the cluster uses
  (`architecture.md`, Common validation);
* **the level** of the pool;
* **the cntlr itself**: its address and transport, its cntlid slot, whether
  it is the primary, whether it is disabled, its health epoch and whether
  it is still settling after a promotion;
* **the slices**, each with its index, its list of meta groups and its list
  of data groups. A group carries its id, its sizes in blocks, its member
  legs and its spare legs. A leg carries its id, its index in the group and
  its sides. A side carries its id, its disk node's address and transport,
  its cntlid slot and whether it is provisioned;
* **the thin devices**, each with its id, its thin id inside the pools, the
  thin id of its origin when it is a snapshot, its size, whether the worker
  has seen it created in every slice, and whether its snapshot came out
  torn;
* **the subsystems**, by NQN, each with its id, serial, model, allowed
  hosts and namespaces. A namespace carries its id, its nsid, its thin
  device, its uuid and nguid, and whether it is suspended;
* **the clones**, each with its id, the source's addresses, NQN and nsid, the
  source pool's slice count, stripe size and block size, the destination
  thin device, the dm-clone's two knobs, whether the destination namespace
  serves while the copy runs, and two fields the agent does not read, the
  deleting mark and the metadata units;
* **the transfers**, each with its id, the origin namespace by NQN and nsid,
  its allowed hosts, and whether the origin is suspended while it runs.

The node's request is small: the ids, the revision, the list of cntlr
pointers this node holds, and a QoS setting the agent stores and does not
apply.

### From the request to the plan

The first thing every pass does is turn the request into the plan: the
names, sizes, tables and ANA groups of every object the cntlr should have.
The plan is a pure function of the request and the node's name formatter;
it reads nothing from the node. Both the converge and the check round work
from it, which is what keeps the two from judging the same device
differently (`cnagent.md`, CN9).

The plan derives:

1. **The effective role.** The cntlr is primary when the request says
   primary and not disabled; anything else converges the standby shape. A
   disabled primary is therefore a standby, and every namespace of it goes
   inaccessible (`cnagent.md`, CN9).
2. **The level gates.** Below the disable level the cntlr wants anything at
   all; below the no-side level it wants its legs; a primary below the
   no-redundancy level wants its groups, below the no-thin-pool level its
   pools, thin volumes and raid0s, below the no-clone level its clones; and
   a primary at or above the read-only level, and below the disable level,
   puts a dm-flakey table that fails writes on every namespace device that
   serves from its raid0 or from a dm-clone, while a parked one keeps its
   plain dm-linear (`architecture.md`, SpLevel; `cnagent.md`, CN19).
3. **The effective desired state.** A leg all of whose sides are
   unprovisioned is provisioning. A group with such a member is deferred, and
   a deferred group takes every group after it in the same list out of the
   concat, because a concat's targets sit at offsets that are the sum of the
   lengths before them and dropping one from the middle would move live
   data. A slice with no effective group left on one of its two sides is
   deferred whole, and so are every thin device of the pool, since the raid0
   stripes across every slice, and the namespaces and clones over those thin
   devices (`architecture.md`, [D15]; `cnagent.md`, CN9).
4. **The sizes.** A leg's wrapper spans the leg's meta and data regions,
   their blocks times the block size. A group device without RAID1 starts at
   the leg's data region, that is at the meta blocks' offset. A thin volume's
   size is the thin device's size divided by the slice count. The raid0's
   chunk is the stripe size. The pool's low-water mark is the data blocks
   times the complement of the percentage, or zero when the percentage is
   above a hundred, which switches the auto-grow off.
5. **Each namespace's backing and ANA group.** The backing is decided in
   this order, first match wins: a deferred thin device gives the dm-error;
   a suspended namespace gives the dm-error; a standby, or a level at or
   above the no-thin-pool level, gives the dm-error; a clone over the thin
   device under a level that suppresses clones gives the dm-error; a clone
   gives the dm-clone; otherwise the raid0. The namespace is ANA optimized
   only on a primary below the disable level whose namespace is not
   suspended and not deferred; otherwise inaccessible (`cnagent.md`, CN16).
   A namespace is suspended when its flag says so or a transfer with
   auto-suspend names it, unless a clone with auto-resume targets its thin
   device.

### The info the agent answers with

A cntlr's info has one map of rows per kind: subsystems, namespaces,
namespace devices, raid0s, dm-errors, thin volumes per thin device and
slice with the torn mark, thin pools, meta concats, data concats, groups,
legs, and for transfers and clones three maps each. A row has the object's
name, a status, a text, an epoch and a reason. The statuses the agent
writes are ok, missing, error, provisioning and, on a primary's leg rows
only, pending. The epoch moves only when the status changes.

**In the code.** The request is `pb.SyncupCntlrRequest` in
`pb/schema.proto`, with `Cntlr`, `Slice`, `Group`, `Leg`, `Side`,
`ThinDevice`, `Subsystem`, `Namespace`, `Clone` and `Transfer`; the node's
is `pb.SyncupCnRequest`; the info is `pb.CntlrInfo` and `pb.CnInfo`, a row
`pb.ResInfo`. The conf check is `agent.ValidateBdevConf` in
`agent/conf.go`. The plan is `cntlrPlan` in `plan.go` of the cn package,
built by `newCntlrPlan` from `buildSlices`, `computeEffective`, `buildTds`,
`buildClones`, `buildSubsystems` and `buildXfers`; the backing rule is
`nsBacking`, the suspend rule `effectiveSuspend`, the level gates the
`want*` and `level*` fields, the low-water mark `lowWaterMark`.

## 8. The start of the agent

### Flags and wiring

`dnv-agent cn` takes the gRPC address it listens on and the kind of network of
that address, by default tcp, the four transport flags of its nvmet port, the
port's configfs id (default 1), the local store directory (default
`/var/lib/dnv`), an optional capacity in bytes and an optional config file.
Every flag can also come from the environment as `DNV_AGENT_<FLAG>`. The
program builds the one OS client, the name formatter with the store prefix,
the store, the device-mapper, nvmet and NVMe host wrappers, the md wrapper and
the metadata area wrapper, and hands the server to the shared start routine.

### Reconcile, then serve

The start routine runs the startup reconcile first and opens the listener
only when it has returned (`dnagent.md`, SH1). The reconcile runs under the
node write lock, under a trace id it makes for itself, and does this in
order:

1. **List the local store.** One directory listing. A temp file left by a
   save that did not finish is removed; the committed file is the only
   truth. A store directory that is missing or unreadable is the one fatal
   error: the agent exits (`dnagent.md`, SH3).
2. **Load the files.** Every node file, then every cntlr file, then every
   chunk file, one read and one decode each, and each decoded message goes
   into the log, a chunk's bitmap by its length only. While any node file
   does not decode, every cntlr and chunk whose node is not loaded is left
   alone: neither loaded nor deleted, since nothing read proves they left a
   node's list. A chunk whose cntlr is not loaded is left alone only while
   some cntlr file did not decode and the loaded node still names that
   cntlr; otherwise it is an orphan, and so is a chunk of a clone its loaded
   cntlr no longer lists. Orphans' files are removed (`cnagent.md`, CN2).
3. **Converge the base state** of every node request held: the tmpfs, the
   metadata area file and its loop device, the port and its ANA groups.
   After a reboot all of it is gone and is built again; after a plain
   restart all of it is found and nothing is written.
4. **Drop the cntlrs whose pointer left** their node's list: their files
   go with one `rm`, their memory entry and lock go, and nothing is removed
   from the node yet. The sweep finds their devices by name.
5. **Sweep the node**, once per node request, with removals on: every
   pool the node holds that no pointer names is torn down layer by layer,
   unowned exports and connections go, and the metadata area is cleared of
   the clone metadata devices no stored cntlr's clone list names.
6. **Converge every cntlr**, one after another, each a whole converge from
   the stored request. On a primary that means finding or assembling every
   array, the pools, the thin volumes, the raid0s, the namespace devices and
   the exports; after a reboot it also rebuilds every clone from the
   destination pools' bitmaps. The request came from a file, not from the
   worker, so this converge sweeps no thin ids and trusts the request only
   for what it builds.

Only then does the agent listen. While it does not, the worker's check
streams cannot open, which the worker records as unhealthy for the node and
every cntlr on it (`dnv-worker.md`, HL1, HL2). On a primary with many
slices the reconcile is a few seconds of `dmsetup` and md reads plus the
array assembly, and the first check round after the listener opens may wait
behind a retry attempt the reconcile registered.

The reconcile also fixes the root context: every background goroutine the
agent starts later, the retries, the probes and the disconnects, hangs off
it, so cancelling it at shutdown stops them all.

### Shutdown

On SIGTERM or SIGINT the gRPC server stops taking new calls and drains the
calls in flight. Then the root context is cancelled: the retry loops and
the probes end at their next check, and nothing is joined. A probe stuck
in an uninterruptible read would hang the shutdown for ever, and a
background `nvme disconnect` still in the kernel holds nothing a restarted
agent needs (`dnagent.md`, SH27).

**In the code.** The command is `runCn` in `cmd/dnv-agent/main.go`; the
flags are bound by `bindViper`. The start routine is `agent.Serve` in
`agent/agent.go`; the reconcile is `Reconcile` in `syncup_cn.go` of the cn
package, which uses `Store.List` and `Store.Load` of `agent/store.go`,
`convergeCn`, `dropCntlrState`, `sweepCn` and `convergeCntlr`. The root
context is `CnAgentServer.rootCtx`.

## 9. The node syncup

The node syncup tells the agent which cntlrs this controller node holds. The
worker sends it when the list changes, when the revision of the node moves,
and again every round while the node's check round reports a leftover, a piece
of the base state that is absent, an ANA group in a state other than its fixed
one, or a metadata area file of another size that nothing uses. It runs under
the node write lock, so nothing else runs on the agent meanwhile.

### Steps

1. **Gate the revision.** A request older than the stored one is refused;
   an equal one is applied again and must change nothing
   (`dnagent.md`, SH8, SH16).
2. **Store and save the request first.** The request goes into memory and
   then into its file, with a fsync of the file and one of the directory,
   before anything is converged. The sweep below may block on a dead leg
   for a long time, and a crash in that window must leave a node whose
   next start finds the new list, not the old one (`cnagent.md`, CN7).
3. **Converge the base state**, probe-first, each piece with its own row:
   * the tmpfs: one `findmnt` for the filesystem type and, when something
     is mounted, a second one to confirm that the path itself is the
     mountpoint. A `findmnt` that did not answer is run once more. An
     absent mount is made with `mkdir -p` and `mount`. A mount over the
     live area would hide the metadata area and start a second one, so an
     unanswered probe mounts nothing (`cnagent.md`, CN5);
   * the file: one `stat`, retried once; an absent file is made with
     `truncate` only on a confirmed tmpfs. A file of another size is
     replaced only when nothing uses it: no clone metadata device maps one
     of its loop devices and no device holds a loop device open; then the
     loop devices are detached, the file removed and made afresh;
   * the loop device: one `losetup --associated`; none makes `losetup
     --find --show` attach one; more than one is an error row;
   * the port: one `ls` of the port directory and seven attribute reads,
     the four transport attributes and the three group states. A port that
     does not match is written and probed again. A port whose transport
     attributes differ cannot be rewritten while a subsystem is linked to
     it, and stays an error row.
4. **Drop the cntlrs whose pointer left the list.** Each one's retries and
   probes are stopped, its request file and chunk files go with one `rm`,
   its memory entry and its lock go. Nothing is removed from the node here:
   the sweep finds the devices by their names, so a removal that fails is
   found again next time instead of being forgotten with the file
   (`cnagent.md`, CN7).
5. **Sweep the node**, with removals on. The enumeration lists the whole
   node: `dmsetup ls`, the md arrays from sysfs, an NVMe walk of its own,
   and `ls` of the nvmet subsystems tree with one attribution per
   subsystem. If any of the four did not answer, the pass removes nothing
   and reports the failure. Otherwise every pool that appears on the node
   and is not in the pointer list gets a chain with an empty wanted set and
   is torn down top-down; a host-facing subsystem no stored cntlr names and
   no namespace device of this node backs is removed; a clone-source
   connection no stored cntlr and no live dm-clone uses is disconnected in
   the background; and under the metadata-area lock, the area's clone
   metadata devices are listed again, one `losetup`, one `dmsetup ls` and
   one `dmsetup table` per device, and every one whose clone no stored
   cntlr's clone list names is removed (`cnagent.md`, CN21).
6. **Reply** with the sweep's verdict, the revision and the four base-state
   rows.

### Cost

With everything in place the base state costs about six processes: one or
two `findmnt`, one `stat`, one `losetup`, one `ls`, and seven file reads.
The enumeration costs `dmsetup ls`, the nvmet `ls`, the NVMe walk, one listing
and one NQN read per connected subsystem on the node, and the md arrays,
one listing of `/sys/block` and per array one listing, one state read and
one read per member. The metadata-area reconcile costs one more `losetup`
and one more `dmsetup ls`, one `dmsetup table` per clone metadata device on
the node, and the clone-source check one `dmsetup table` per live dm-clone.
A teardown
chain costs its removals, each verified by a fresh probe, and a stuck layer
stops the chain for this pass. Every pass of every cntlr on the node waits
for the write lock meanwhile, and the writer's priority keeps new readers
out from the moment the syncup arrives.

**In the code.** `SyncupCn` in `server.go` of the cn package takes the lock;
`syncupCn` in `syncup_cn.go` runs the steps: `agent.GateRevision`,
`Store.Save`, `convergeCn` with `ensureTmpfs`, `ensureTmpFile`,
`ensureLoopDev` and `ensurePort`, `dropRemovedCntlrs` with
`dropCntlrState`, and `sweepCn` in `sweep.go` with `enumerateCn`,
`buildChain`, `runChain`, `removeExportVerified`, `unownedSrcNqns`,
`disconnectVerified` and `orphanCloneMetaNames`, and `reconcileCloneMeta` in
`clonemeta.go`. The
base-state wrappers are `CloneMeta.Mounted`, `MountTmpfs`, `FileSize`,
`Truncate`, `LoopDevices`, `LoopAttach`, `LoopDetach` and `LoopHolders` in
`clonemeta.go`, and `Nvmet.ProbePort` and `EnsurePort` in `agent/nvmet.go`.

## 10. The cntlr converge

The cntlr converge is the pass that makes a cntlr match its request. It
runs on three entrances: the cntlr syncup, the startup reconcile and a
retry attempt. All three run the same function; only the locks and the
request's origin differ. The pass has two halves in a fixed order: the
sweep first, top-down, then the build, bottom-up (`cnagent.md`, CN9). That
order is what makes a failover need no special case: a demotion is a
desired state that shrank to the standby shape, so the sweep takes the
primary's layers down, and a promotion is one that grew, so the build puts
them up.

```
   SyncupCntlr                      retry tick                 startup reconcile
   (node read lock)                 (node read lock)           (node write lock)
       |  gates: pointer, revision, conf   |                          |
       |  store the request                |                          |
       v                                   v                          v
   +------------------------- convergeCntlr --------------------------------+
   |  conf check  ->  plan  ->  pass budget  ->  trim the health probes      |
   |  NVMe walk                                                               |
   |  sweep:  enumerate the node  ->  wanted set  ->  chain                   |
   |          P1 ANA to inaccessible                                          |
   |          [if every listing answered]  P2 park, P3 demote, P0 + L1..L10   |
   |          chunk files, row histories                                      |
   |  build:  legs -> groups -> slices (+ thin-id sweep) -> snapshot messages |
   |          -> thin volumes -> dm-error, raid0 -> clones -> transfers        |
   |          -> namespace devices -> subsystems, namespaces -> ANA optimized  |
   |          -> start or stop the retries                                     |
   +--------------------------------------------------------------------------+
       |  save the request (fsync)
       v  reply: verdict, revision, info, chunks held
```

### Entry and gates

The syncup takes the node read lock. For a cntlr this process does not hold
yet it runs the gates before it takes the cntlr's lock, so a refused request
leaves no lock behind (`dnagent.md`, SH11). Then it takes the cntlr's lock
and runs the gates again: the cntlr's pointer must be in the node's stored
request (`cnagent.md`, CN8); the revision must not be below the stored one,
an equal one being a re-apply; the geometry must carry no zero. A refusal
replies with its code and converges nothing; the revision and conf refusals
also carry the stored revision.

An accepted request replaces the stored one at once, before anything is
built, so other passes that read every cntlr's request already see it; it
is marked as delivered by the worker in this process, which is what later
allows the thin-id sweeps to delete against its thin device list.

### The frame of the pass

1. **The geometry check**, once more, for the two entrances that bring no gate
   of their own.
2. **The plan**, from the request alone.
3. **The torn marks**: the marks of thin devices the request no longer lists,
   or lists created, are dropped.
4. **The pass budget**: a wait budget of 1 s (`CnConnectPassBudget`), made for
   this pass and shared by every leg and every clone source it connects. It is
   an account, not a deadline: only failed connects, the pauses after them and
   the waits for a namespace head draw on it, and it never bounds a side's
   first connect.
5. **The health probes are trimmed** to the legs the plan still probes: the
   non-provisioning legs of a primary below the no-side level. A demoted
   primary's probes stop here, before the sweep, since a probe stuck on a
   pathless leg is released only by the sweep's disconnect of that leg.
6. **The pass's NVMe walk**: one listing of the subsystem entries and one NQN
   read per entry. Every lookup of the pass uses it, and so does the sweep's
   list of connections.
7. **The sweep**, below.
8. At the disable level, whether or not the sweep could remove everything,
   both retries are stopped and every row history is dropped, so a later
   rebuild reports fresh epochs (`cnagent.md`, CN19).
9. **The build**, below.

### The sweep

**Enumeration.** The sweep lists the node four ways: `dmsetup ls` for the
device-mapper devices, strict, so a non-zero exit is "did not answer"; the
md arrays from sysfs, one listing of `/sys/block`, then per array its state,
its member directory and each member's device-mapper name, strict as well;
the connections from the pass's walk, which fails if any entry's NQN read
did not answer; and `ls` of the nvmet subsystems tree, followed by one
attribution per subsystem. A transfer subsystem's NQN says which pool it
belongs to. An NQN with the dnv prefix that does not decode is left alone.
A host-facing subsystem is attributed first by the stored requests, which
cost no read, and only when no held cntlr names it by its namespaces: one
`ls` of them and one `device_path` read each, until one names a namespace
device of this node; none makes it unowned, which only the node-level sweep
removes (`cnagent.md`, CN21).

**An unanswered listing licenses no removal.** Removal is "what the node
holds, minus what the plan wants". When a listing did not answer, its part
of the snapshot is empty, which looks like "nothing there" to every check
that is gated on absence: the layer below an unanswered md listing would
disconnect legs from under a live array. So with any of the four listings
unanswered the pass removes nothing, reports the failure as a leftover, and
the worker re-sends the syncup (`architecture.md`, Teardown by sweep).

**The wanted set** is read from the plan with the deferrals left in, so a
sweep never removes what the next build is about to create. Below the
no-side level it holds the leg wrappers and the leg connections; a primary
below the no-redundancy level adds the group devices and the arrays whose
members are all one wanted group's wrappers; below the no-thin-pool level
the concats, pools, thin volumes and raid0s; below the disable level the
per-thin-device dm-errors, the namespace devices, the subsystems, the
namespaces and the transfers; below the no-clone level the dm-clones, their
metadata devices and their source connections.

**The chain** is the pool's unwanted things, grouped by layer: the
device-mapper devices of this pool by kind, the arrays attributed to this
pool that are not wanted, the subsystems of this pool that are not wanted,
the namespaces of staying subsystems that are not wanted, found with one
`ls` per staying subsystem, and the leg connections of this pool to this
node that are not wanted. The clone-source connections are judged only when
their layer runs: a source is unused only once the dm-clone over it is gone.

**P1, on every converge.** Every namespace the plan wants inaccessible, and
every transfer namespace it wants inaccessible, is moved there before
anything under it is touched: one probe per namespace,
an `ls` and five attribute reads, and a write of the group when it differs.
It runs even when the listings did not answer, because the build that
follows parks on its strength (`cnagent.md`, CN9). On a standby every
namespace is such a namespace, so a standby converge probes each of its
namespaces here.

**P2 and P3, only when removals are licensed.** Every planned namespace
device of a thin device the request still lists is parked when its plan
backing is the dm-error, or when its live table maps something this pass is
about to remove, which one `dmsetup table` tells. The park is a reload onto
the thin device's dm-error, whose own suspend completes the in-flight host
IO; the park's own reads, the device, the dm-error with its number and the
device's table, are five `dmsetup` even when the device is parked already.
P2 does not run at the disable level. A transfer device this cntlr does not
serve is reloaded onto an error table of its size, so it lets go of the
raid0 under it.

**P0 and the ten layers.** Then the chain runs. First every unwanted
namespace device is parked by its own live table, since no plan describes
it. Then, top-down:

| layer | removes | verified by |
|---|---|---|
| L1 | unwanted namespaces (moved inaccessible, disabled, removed) and unwanted subsystems (port link, namespaces, host links, directory) | `ls` of the directory |
| L2 | namespace devices and transfer devices | `dmsetup info` |
| L3 | dm-clones, before their sources go | `dmsetup info` |
| L4 | clone metadata devices, under the metadata-area lock | `dmsetup info` |
| L5 | clone-source connections nobody uses | a walk for the NQN; the `nvme disconnect` runs in the background |
| L6 | raid0s and dm-errors | `dmsetup info` |
| L7 | thin volumes; then, when the pool stays, a `delete` message of the volume's thin id, read from its live table | `dmsetup info` |
| L8 | thin pools, then the concats under them | `dmsetup info` |
| L9 | md arrays with `mdadm --stop`, and group devices | the array's state in sysfs; `dmsetup info` |
| L10 | leg connections, then wrappers | a walk for the NQN, the disconnect in the background; `dmsetup info` |

Every removal is followed by a fresh probe of the object, because a killed
command may still have completed in the kernel. A layer that leaves
anything behind stops the descent: the layers below report their objects
as leftovers without touching them, and the next pass starts again from a
fresh listing. The reason is safety, not speed: removing a leg from under
an array that would not stop is destructive on the migration path, and a
pass that runs again next round costs nothing (`cnagent.md`, CN21). The
sweep's disconnects never run under the locks: a controller delete whose
target vanished sits in the kernel for the admin timeout of 60 s, so the
sweep only probes the connection and sets the disconnect going in the
background. The one disconnect that runs inline, under the locks, is the
build's retirement of a dead path by its controller device.
Until a later pass finds the controller gone, the connection is a leftover.

**After the chain.** The pass works out which unwanted dm-clones may still
be live, from their tables, and holds the build off the raid0s they write
into. When a `delete` message of L7 failed, the pass dumps that pool's
metadata and deletes every thin id that no thin device of the request and
no live volume owns; this runs only for a request the worker delivered, and
costs the pool's dump, six processes, one `dmsetup table` per thin volume of
the pool's storage pool that the listing still names, read once for all
failed pools, and one `delete` message per stray id. In every converge the
chunk files of
clones no longer in the request are removed, the row histories of objects
no longer named are dropped, and the pass logs one record naming every
leftover.

### The build

The build goes bottom-up. Each step writes its rows; a step that fails
writes an error row with the command's output as its text and the pass goes
on with the next object (`cnagent.md`, CN29). Nothing in it runs in
parallel. The table after the steps gives the cost of each on a steady
primary.

**1. Legs** — every leg of every group, spares included, on both roles,
below the no-side level. For each leg:

* a provisioning leg gets its row and nothing else;
* a leg whose subsystem is in the disconnect registry fails for the pass,
  since the controllers it would adopt are being deleted;
* the lookup: the entry's NQN is read again, then its directory, the
  nsid of each namespace node until the one with nsid 1, and for each
  controller its address, its state, its directory and the ANA state of
  its path device. A controller whose address is gone was deleted and is
  no path; one whose address did not answer makes the leg fail, since it
  may be any side's live path;
* each provisioned side with no controller at its address is connected:
  the walk is marked stale and `nvme connect` runs with the agent's host
  NQN, a fast-io-fail of 5 s and no controller-loss timeout. The first
  attempt always runs; a failed one is charged its own time to the budget,
  and is tried again after a 100 ms pause while the budget covers it. A
  connect to a dead disk node takes about 3 s and spends the whole budget
  at once (`cnagent.md`, CN10);
* after a connect of this pass, the subsystem is looked up again until its
  namespace head appears, with 50 ms pauses from the budget; the kernel
  returns from the connect before the scan that adds the head;
* a controller that matches no side is a dead path of a finished or
  cancelled migration and is dropped by device, never by NQN, since the
  surviving side shares the NQN;
* the head's number is read from `/sys/block`, and the wrapper is ensured:
  one `dmsetup info`, one `dmsetup table`, and a create or a reload only on
  a difference;
* on a primary the leg's health probe is started if none runs;
* the row: the wrapper is probed again, info and table, and on a primary
  the probe's last outcome decides the status, pending until the first
  round completes; on a standby the transport view decides: every
  provisioned side has a live controller, and a single-side leg's path is
  optimized or non-optimized, since inaccessible cannot serve a promotion
  (`cnagent.md`, CN11).

A leg is available when its subsystem was found and some controller is
both live and ANA optimized. A leg that failed any step registers the
connect retry.

**2. Groups** — a primary below the no-redundancy level. A deferred group
gets a provisioning row. For a RAID1 group:

* the first group of the pass takes the md walk: one listing of
  `/sys/block`, and per array one listing of its member directory and one
  device-mapper name read per member;
* the array whose members include one of the group's member wrappers is
  the group's; spares do not count, and two such arrays are an error;
* its state is read: array state, degraded count, sync action and sync
  progress, and per member its number and state. The agent never runs
  `mdadm --detail`, which opens a member and blocks on a dead leg
  (`cnagent.md`, CN12);
* no array: assemble. Each available member is examined with
  `mdadm --examine`. When none carries a superblock and every member is
  available and one `lsblk` shows no array under the group's name,
  `mdadm --create --assume-clean` makes the array; the sides were zeroed
  where the superblock sits, so a clean create is right. Otherwise
  `mdadm --assemble` starts it from the members with a superblock, and mdadm
  decides whether a degraded start is safe. Then the walk is refreshed and
  the array read again;
* a running array has its members reconciled: a member not in the list is
  failed and removed, an available list member the array lacks is added,
  and md resyncs it. An array that is not running is an error row left as
  it is.

A group without RAID1 is one `dmsetup info` of the leg's wrapper for its
number and a dm-linear over the leg's data region, info and table. A group
with a late member registers the connect retry: nothing else would re-run
the converge that assembles it, since the worker re-sends on a revision or
a reply code, never on a row.

**3. Slices** — a primary below the no-thin-pool level. A deferred slice
gets provisioning rows. Otherwise the meta concat and the data concat are
ensured: per segment the group's device number, from the md walk's match
and one sysfs read for an array, from one `dmsetup info` for a dm group,
then one info and one table of the concat, and a create or a reload. A
concat only grows: a desired table shorter than the live one is an error,
since shortening would remap live pool data. Then the pool: its two numbers,
two `dmsetup info`; one info of the pool; then info and table once more,
and a create or a reload; a concat
that grew forces the reload even when the pool's four arguments did not
change, since dm-thin sees a resized device only at a resume. Then one
`dmsetup status`, whose raw line is the pool row's text; this is the read
that commits the pool's metadata (`dnagent.md`, SH17). A pool created in
this pass arms the thin-id sweep of the slice. After all slices, every
armed slice whose pool is ready and whose request came by the worker has
its ids swept: the pool's metadata is dumped, six processes, and every id no
thin device of the request owns gets a `delete` message (`cnagent.md`,
CN14).

**4. The snapshot messages** — for every snapshot the worker has not yet
seen created: one `dmsetup info` per slice of the snapshot's volume; when
some slice still needs its id, the origin's raid0 is suspended, each such
slice gets its `create_snap` message with the origin's volume suspended
around it, and the raid0 is resumed on every path out. One suspension for
all slices is what makes the slices describe one instant. A run that made
the id in some slices while other slices already held it marks the snapshot
torn (`cnagent.md`, CN14).

**5. Thin volumes** — per thin device and slice: one `dmsetup info` of the
volume; when it is absent and the thin device is a plain one the worker has
not seen created, a `create_thin` message; the pool's number, one
`dmsetup info`; then info and table of the volume and a create on absence. A
created thin device whose pool no longer holds the id gets an error row with
the lost-id reason, and no later pass sends a message for it.

**6. Per thin device** — the dm-error of the thin device's size, info and
table; then the raid0: one `dmsetup info` per slice for the volumes'
numbers, then info and table, a create or a reload.

**7. Clones** — a primary below the no-clone level, medium depth here. For
each clone, in order:

1. the source: a lookup of the source NQN in the pass's walk; when the walk
   holds no entry for it, one fresh walk, since another cntlr of the node
   may have connected it; each source address without a controller is
   connected with the pass budget, and the namespace head awaited;
2. the metadata: the metadata area is listed once per pass, one `losetup`,
   one `dmsetup ls` and one `dmsetup table` per clone metadata device on
   the node, and cached on the plan; the clone's device must be a
   dm-linear of exactly the budgeted size over the current loop device.
   When it is not, the pass first asks whether the area can supply a fresh
   run: no free run makes both rows provisioning and registers the
   clone-metadata retry; a run larger than the whole area is an error
   that registers nothing;
3. the dm-clone is probed, info and status. A build is a recovery whenever
   the metadata device does not match, the dm-clone is absent, or its
   status shows hydration off: the thin device's namespace devices are
   parked first, the dm-clone removed, and a mismatched metadata device
   replaced under the metadata-area lock, the area listed again inside the
   lock, the run hole-punched with `blkdiscard` and the dm-linear created;
4. the dm-clone is ensured, created with hydration off, and its two knobs
   set by message when they differ;
5. when the dm-clone was created or reloaded in this pass, or the build is a
   recovery: on a recovery the destination's bitmaps are read first, every
   slice's pool dumped, six processes each, the thin device's mapped blocks
   folded into dm-clone regions, and every fully mapped region discarded on
   the dm-clone, which marks it hydrated; then every source bitmap chunk
   held is applied the same way;
6. hydration is enabled by message, and the status read once more; a status
   that does not show hydration on leaves the build unfinished and
   registers the connect retry.

The clone's rows are written by one function on both channels, from one
read of the source and one of the dm-clone, so a check round reports what
the converge reported (`cnagent.md`, CN18).

**8. Transfers** — below the disable level, on both roles. A transfer whose
origin namespace is not in the request is demoted onto an error table sized
from its live table, gets three error rows and nothing else. A transfer this
cntlr serves, a primary below the no-thin-pool level with an origin that
resolves and is not deferred, gets a dm-linear over the origin's raid0: one
number, info and table. Any other gets a dm-error of the origin's size. Then
its subsystem, its one namespace and its port link, as for a host-facing
subsystem below.

**9. Namespace devices** — below the disable level. Per namespace: when the
backing is a dm-clone, its status is read to see whether hydration is on;
off, or no dm-clone, means the device is parked instead. When the backing
is the dm-error, the dm-error is ensured again, info and table. Then the
backing's number, one `dmsetup info`; info and table of the device; a create
on absence, a reload on a difference, and a resume of a device found
suspended by an interrupted reload. The hold: while a dm-clone the plan does
not want may still write into the raid0, the device is not moved onto that
raid0. One already there keeps serving; one not there stays parked or is
created parked; either registers the connect retry (`cnagent.md`, CN16,
CN18).

**10. Subsystems and namespaces** — below the disable level. Per subsystem:
one `ls` of its directory and a `mkdir` on absence; the attribute that
admits every host is read and written to zero when it differs; the cntlid
maximum is read, and raised first when the whole slot lies above the live
range; the serial, model, cntlid minimum and maximum are read and written
on a difference; one `ls` of the allowed hosts, a `mkdir` and `ln` per
missing host and an `rm` per surplus one. An empty host list admits no host
(`architecture.md`, Primary cntlr, step 6). Per namespace: a probe, one `ls`
and five reads, then the ensure, which probes again, makes the directory on
absence, writes the device path and the identity on a difference, keeps the
ANA group a live namespace has or starts a new one inaccessible, and enables
it. Then the port link: one `ls` of the port's subsystem list and an `ln`
when the subsystem is not in it.

**11. ANA last.** Every namespace the plan wants optimized is probed, one
`ls` and five reads, and its group written when it differs; transfers too.
A namespace held off its raid0 keeps its group if it serves and goes
inaccessible if it is parked. The rule reads the plan, not the stack: a
promotion whose stack could not be built because a member is late moves
its namespaces to optimized over the dm-error all the same, and hosts take
IO errors until the retry has built the stack (`cnagent.md`, Known limits).

**12. The retries.** When no step registered the connect retry, it is
stopped; when no clone waits for room, the clone-metadata retry is stopped.

### The reply

The reply carries the sweep's verdict as its code, zero when clean and the
leftover code otherwise, the request's revision, the info, and the list of
bitmap chunks the cntlr holds per clone (`cnagent.md`, CN20). Before the
reply the request is saved to its file: one in-process write with a fsync
of the file and one of the directory, and a log record with the whole
request. A save that fails is only logged; the next start then converges
from the previous request, which is why the thin-id sweeps trust only a
request the worker delivered to this process.

### What a standby skips

A standby runs the same pass with a smaller plan. It connects and wraps
every leg, but starts no health probe and judges each leg by its
controllers' states. It builds no group, concat, pool, thin volume or raid0,
and no clone. It keeps the per-thin-device dm-error and parks every
namespace device on it, five `dmsetup` in the pre-step and five in the
build per namespace. Its subsystems and namespaces are built like a
primary's, and its first pre-step moves every namespace inaccessible on
every converge. Its sweep removes what a primary would have had: on a
demotion that is the raid0s, the thin volumes, the pools, the concats and
the arrays, and the dm-clones with their metadata devices and source
connections; a served transfer device is demoted onto an error table.

### Re-applies change nothing

Every ensure step reads first and writes only on a difference, because a
reload of a live table stalls host IO and a re-linked port link does too
(`dnagent.md`, SH16). A syncup of the same revision on a converged cntlr
therefore runs the reads of the table below and no write but the store
save; a retry attempt or a startup reconcile that finds everything in place
saves nothing and writes nothing.

### Cost of a steady converge, per object, on a primary

| step | per | processes | file reads in the agent's process |
|---|---|---|---|
| sweep enumeration | pass | `dmsetup ls`, nvmet `ls`, one `ls` per staying subsystem; with a live dm-clone on the node one `dmsetup table` per live dm-clone, and a listing and a read per clone-source connection | the walk: one listing and one read per connected subsystem on the node; the arrays: one listing, and per array one listing, one state read and one read per member |
| P2 park check | namespace | one `dmsetup table` when the backing is the raid0 or a dm-clone; five when it is the dm-error | — |
| leg | leg, spares included | four `dmsetup` (info and table twice) | about eight: the NQN, the directory, one nsid, and per controller four |
| group, RAID1 | group | none | eight: four of the array and two per member; plus the walk once per pass |
| group, no RAID1 | group | three `dmsetup` | — |
| slice | slice | ten `dmsetup`: two per concat, five for the pool, one status | one per RAID1 segment, for the array's number |
| thin volume | thin device × slice | four `dmsetup` | — |
| dm-error and raid0 | thin device | two, plus one per slice plus two | — |
| namespace device | namespace | three `dmsetup`; five when parked | — |
| subsystem | subsystem | three `ls` | six attribute reads |
| namespace | namespace | two `ls`, plus one for the ANA step | ten attribute reads, plus five for the ANA step |
| transfer | transfer | three `dmsetup` and six `ls`, the ANA step included | about twenty-one, the ANA step included |
| clone, steady | clone | about eleven `dmsetup`, the status read four times, plus per pass one `losetup`, one `dmsetup ls` and one `dmsetup table` per clone metadata device on the node | a few |
| store save | pass | none; two fsyncs | one write |

For the example shape that is about 1,010 processes, nearly all `dmsetup`,
and about 2,200 reads. A standby converge of the same pool is about 535
processes: the legs' 512, the namespace's park, the dm-error, the
subsystem and the sweep.

**In the code.** `SyncupCntlr` in `server.go` of the cn package takes the
locks; `syncupCntlr` in `syncup_cntlr.go` runs the gates (`gateCntlr`,
`agent.GateRevision`, `agent.ValidateBdevConf`), stores the request
(`cntlrState.storeReq`, `reqFromRpc`), calls `convergeCntlr`, saves
(`Store.Save`) and builds the reply (`bitmapInfoList` in
`push_clone_bm.go`). `convergeCntlr` runs `newCntlrPlan`, `keepTornMarks`,
`newPassBudget`, `stopLegProbers` with `wantedProbers`, `newPassWalk`,
`sweepCntlr` and `build`. The sweep is `sweep.go`: `enumerateCn`,
`nvmetOwner`, `hostFacingClaim`, `cntlrWanted`, `buildChain`,
`cntlrAnaPreStep`, `cntlrPreSteps`, `parkByTable`, `runChain` with its
layers, `removeThins`, `lingeringCloneHold`, `sweepAfterFailedDeletes`,
`sweepCloneChunks`. The build is `build` in `syncup_cntlr.go`, calling
`ensureLegs`, `ensureLeg`, `lookupSubsys`, `connectWithin`, `awaitNsHead`,
`disconnectDeadPaths` and `legInfo` in `leg.go`; `ensureGroup`,
`assembleGroup`, `reconcileMembers`, `Md.Walk`, `Md.Detail`, `mdDevNo` in
`md.go`; `ensureSlice`, `ensurePool`, `ensureThin`, `sweepThinIds`,
`createSnapId` in `pool.go` and `snapshotPrePass` in `syncup_cntlr.go`;
`ensureRaid0`, `ensureNsDev`, `nsDevNow`, `ensureSubsystem`,
`ensureNamespaceObject`, `setAna` in `td.go` and `ensureDmError` in
`dmutil.go`; `ensureClone`,
`ensureCloneSource`, `ensureDmClone`, `enableHydration`, `cloneRows` in
`clone.go` with `planArena`, `cloneMetaVerdict`, `ensureCloneMeta` in
`clonemeta.go` and `applyDstBitmaps`, `applyCloneChunks` in `thinbm.go`;
`ensureXfer` in `xfer.go`. The generic ensure and probe helpers are
`dmutil.go`; the nvmet steps are `Nvmet.EnsureSubsystem`,
`EnsureNamespace`, `ProbeNamespace`, `EnsurePortLink` in `agent/nvmet.go`;
the connect is `NvmeHost.Connect` in `agent/nvmehost.go`; the wait budget
is `agent.WaitBudget` in `agent/waitbudget.go`.

## 11. The cntlr check round and info read

A check round and an info read are the same pass with two entrances. The
round comes on the cntlr's check stream, once per check interval; the info
read comes as a single call from the gateway when an operator inspects the
cntlr. Both take the node read lock and the cntlr's lock, read the node,
write the rows, compute the verdict and change nothing on the node
(`cnagent.md`, CN23, CN24).

### The stream

The agent loops on the stream: receive a request, run the round under the
trace id the request carries, send the reply. An end of the stream or a
cancelled stream ends the loop quietly. For a cntlr this process does not
hold, the round replies the unknown-object code with no info and the stream
stays open; the worker then sends the syncup that introduces the cntlr.

### The reading

Under the locks, the pass first builds the plan and then takes the reading,
in this order:

1. `dmsetup info` over the whole node, one line per device with its name,
   its number and its attribute flags;
2. `dmsetup table --concise` over the whole node, every device's live table
   in one run;
3. the NVMe walk: one listing and one NQN read per entry;
4. the md walk: one listing of `/sys/block` and, per array, its state, its
   member directory and each member's device-mapper name.

A listing that exited non-zero after printing lines is partial and still
counts: another cntlr or a dn agent on the same kernel removes devices
during listings, about three in a hundred during a teardown. A device of
this cntlr that a partial listing lacks is read on its own, once, with one
`dmsetup info`, and the answer is kept for the pass. A table the second
listing lacks is read the same way. A listing that did not answer at all
turns every row that reads it into an error row, and the verdict names it
(`cnagent.md`, CN28).

### The rows

The rows are written in the build's order, from the reading wherever the
reading holds the fact, and from these reads outside it:

| row | from the reading | extra reads |
|---|---|---|
| leg, on a primary | the wrapper's flags and table | the health probe's last outcome, from memory |
| leg, on a standby | the wrapper's flags and table | the lookup: the entry's NQN again, its directory, one nsid, and four reads per controller |
| group, RAID1 | the array matched in the md walk | eight sysfs reads: four of the array, two per member |
| group, no RAID1 | the device's flags, table and its leg's number | — |
| meta concat, data concat | flags and table | one sysfs read per RAID1 segment, the array's number |
| thin pool | flags, table and the two concats' numbers | one `dmsetup status`, which commits the pool's metadata |
| thin volume | flags, table, the pool's number | — |
| dm-error, raid0 | flags, table, the volumes' numbers | — |
| clone | the dm-clone's flags, the metadata devices' tables | the source lookup; one `losetup` per pass and the loop device's number; one `dmsetup status` of the dm-clone |
| transfer device | flags and table | — |
| transfer subsystem and namespace | — | four `ls` and five reads; one `ls` and five reads |
| namespace device | flags, table, the backing's number | one `dmsetup status` of the dm-clone when the backing is a dm-clone |
| subsystem | — | four `ls`: the directory, the directory again, the allowed hosts, the port's subsystem list; five attribute reads |
| namespace | — | one `ls` and five attribute reads |

The status reads stay outside the reading on purpose: one `dmsetup status`
over the whole node would commit every pool's metadata on the node, other
pools' included (`cnagent.md`, CN28). The subsystem's directory is listed
twice because two helpers each check its presence.

Each row goes through the tracker, which keeps the epoch when the status did
not change. A leg row that reads ok on a primary clears the worker's leg
epoch, so the probe's outcome is pending, never ok, until a probe round has
completed (`cnagent.md`, CN11).

### The verdict

The verdict is the sweep with the removals left out. The enumeration takes
its device-mapper devices from the reading's first listing, its arrays from
the md walk under the strict rule, its connections from the walk, and lists
the nvmet tree with one `ls`, attributing each subsystem as the sweep does.
The chain is built the same way, one `ls` per staying subsystem of this
pool for its namespaces, and every unwanted object is reported as a
leftover without being touched. The clone-source check reads the live
dm-clones' tables from the reading and, only when a dm-clone is live, the
namespace node and number of each transfer-kind connection. The chunk files
of clones no longer in the request are reported too. The verdict is
recomputed on every round and stored nowhere, so a leftover that went away
stops being reported by itself (`cnagent.md`, CN30).

### The reply

The reply carries the verdict's code, the stored revision, and the info
when the request asked for it, when it is the first reply of the stream, or
when the freshly built info differs from the last one sent on the stream,
compared as protobuf messages. An info read always carries the info.

### Cost

For the example shape:

| cntlr | processes | file reads |
|---|---|---|
| primary | about 41: two listings, 32 thin-pool status reads, five `ls` of the subsystem and its namespace, two of the verdict | about 970: the walk's 128, the md walk's 256, the groups' 512, the concats' 64, the exports' 10 |
| standby | about 9: two listings, five `ls`, two of the verdict | about 1,030: the walk's 128, the legs' 896, the exports' 10 |

On the lab's 8 vCPU guests after 33c07d3, a primary's round took 0.36 to
0.54 s and a standby's 0.03 to 0.08 s. Nearly all of the primary's time is
its 32 status reads.

**In the code.** The stream is `CheckCntlr` and the round `checkCntlrRound`
in `check.go` of the cn package; the info read is `GetCntlrInfo` in
`server.go`. Both call `readCntlr` in `reading.go`, which runs
`newCntlrPlan`, `takeReading` (`Dm.ListInfo` and `Dm.ListTables` in
`agent/dm.go`, `newPassWalk` in `leg.go`, `Md.Scan` in `md.go`),
`probeCntlr` in `probe.go` and `sweepCntlr` with removals off. The reading
type is `cnReading`; its own reads of a lacking name are `cnReading.info`
and `cnReading.table`; the shared helpers that take a fact from the reading
or from one `dmsetup` are `dmInfo`, `dmTable`, `dmDevNo` and `blockDevNo`.
The per-kind probes are `legInfo` and `transportHealth` in `leg.go`,
`probeGroup` in `md.go`, `probeDmConcat`, `probeDmTarget` and `probeDmArgs`
in `dmutil.go`, `probePool` and `probeThin` in `pool.go`, `probeNsDev`,
`probeExport` and `probeNamespaceObject` in `td.go`, `probeXferDm` in
`xfer.go`, `cloneRows`, `readCloneSource` and `probeCloneDm` in `clone.go`.
The info suppression is the `proto.Equal` in `checkCntlrRound`.

## 12. The node check round and info read

The node's check round and the node info read take the node read lock
only, so they run beside the cntlrs' passes. They take no reading: the
node-level pass lists the node itself.

**The base-state probe** writes the four rows of the node: the tmpfs with
one or two `findmnt`, the metadata area file with one `stat`, its loop
device with one `losetup`, and the port with one `ls` and seven reads. It
also collects what a node syncup would cure: an absent tmpfs, an absent
file or loop device while the tmpfs is there or absent, a file of another
size that nothing uses, an absent port or ANA group, and an ANA group in a
state other than its fixed one on a port whose transport matches. Each of
these is reported as a failure in the verdict, so the worker re-sends the
node syncup every round until it is fixed (`cnagent.md`, CN30). What a
syncup would not cure, a wrong filesystem type, a file in use with another
size, two loop devices, a transport attribute that differs, stays an error
row only.

**The node-level verdict** runs the node sweep with removals off. Its
enumeration is the sweep's own: `dmsetup ls`, the md arrays from sysfs, an
NVMe walk of its own, `ls` of the nvmet tree and one attribution per
subsystem. Every pool on the node that the pointer list does not name is
reported with every object of its chain; unowned host-facing subsystems
and unused clone-source connections are reported, the latter after reading
each live dm-clone's table with one `dmsetup table`; clone metadata devices
no stored cntlr names are reported. Nothing is removed.

**The reply** carries the verdict's code, the stored revision, and the node
info when the request asked for it, on the first reply of the stream, and
when it changed since the last one sent on the stream.

**Cost.** About seven processes: two `findmnt`, one `stat`, one `losetup`,
one `ls` of the port, `dmsetup ls` and the nvmet `ls`, plus one
`dmsetup table` per live dm-clone. The reads are the node's: one per
connected subsystem, and per array one listing and three reads. On the lab
a primary's node round read about 400 things in about 0.3 s.

**In the code.** `CheckCn` and `checkCnRound` in `check.go` of the cn
package and `GetCnInfo` in `server.go` call `probeCn` in `syncup_cn.go`,
which returns the rows and the list of redrive failures (`baseRedrive`),
and `sweepCn` in `sweep.go` with removals off, which adds those failures to
its result.

## 13. The background loops

The agent starts no work of its own except what a converge leaves behind.
Four kinds of goroutines run outside the RPCs.

### The connect retry

A converge that met a transient failure registers the connect retry for the
cntlr: a leg's connect, lookup, namespace or wrapper failed; a group has a
late member; a namespace device was held off its raid0, or one over a clone
failed to converge in the pass that finished the clone's build; a clone's
source was not ready, its metadata area did not answer, or a step of its
build before the last one failed (`cnagent.md`, CN10, CN12, CN18). At most one
loop runs per cntlr. It ticks every 5 s (`CnConnectRetryInterval`), and
each tick:

1. makes a fresh trace id, so the attempt's records can be found;
2. takes the node read lock, checks that the cntlr is still the one it was
   registered for, and takes the cntlr's lock;
3. runs the whole converge, sweep and build, from the stored request;
4. releases the locks.

The attempt runs under the agent's root context, not under the loop's own,
so that the pass that stops the retry does not cancel itself. There is no
deadline: an attempt takes as long as its converge. The loop is stopped by
the first converge, of any entrance, that registers none of the triggers,
by the disable level, and by the cntlr's drop.

Two properties of the loop decide its cost. The ticker does not wait for the
attempt: an attempt longer than 5 s finds the next tick already due and the
next attempt starts at once, so under a slow converge the cntlr's lock is
held almost all the time. And every attempt is a whole converge: on a pool
of 32 slices during a build that was 21 to 25 s under the lock on the lab,
and every check round of that cntlr waited behind it. A member that stays
dead keeps the loop running until the worker's leg repair has switched the
leg out, or for ever when nothing switches it (`cnagent.md`, Known limits).
Nothing checks before an attempt whether anything has changed; that check
is not done, so that this loop stays the same pattern as the dn agent's.

### The clone-metadata retry

A clone whose metadata area has no free run of the size it needs makes
both of its rows read provisioning and registers the clone-metadata retry.
It is the same loop at 60 s (`CnCloneMetaRetryInterval`), slower because a
clone that waits for room is not broken and each attempt is a whole
converge under the lock the check rounds need (`cnagent.md`, CN18). A run
comes free when a clone of another cntlr of the node goes, which no request
of this cntlr says, so the cntlr asks again itself. Both retries of one
cntlr may run at once.

### The health probes

A primary starts one probe goroutine per leg when the leg's wrapper
converges, spares included (`cnagent.md`, CN11). The probe ticks every 5 s
(`CnLegProbeInterval`). Each round opens the wrapper, writes 4 KiB to the
leg's health block with a fsync, closes it, then opens it again and reads
the block back with O_DIRECT, so that the read goes to the disk node and not
to the page cache. The content is never interpreted; completing within the
limits is the whole signal (`architecture.md`, [D6]). The round runs inline
in the goroutine, so a blocked IO delays the next round rather than piling
up. Each half writes one log record under a fresh trace id per round.

The probe holds no lock and uses no OS client slot. Its IO can block for as
long as the leg has no serving path, which with no controller-loss timeout
is for ever, and a blocked probe must not hold a slot the teardown commands
need (`cnagent.md`, CN1). What releases a blocked probe is the sweep's
disconnect of the leg, which errors its queued IO.

The rows read the probe's last outcome: a round in flight longer than 15 s
(`CnLegProbeStallSeconds`) reads error "stalled"; before any round has
completed the row reads pending; otherwise the last round's result. A fresh
primary's leg rows therefore read pending for at least one interval, and a
promotion starts fresh probes, so the new primary cannot clear a dead leg's
epoch with a row it has not earned.

A converge trims the probes to the legs its plan still probes before its
sweep; a demotion, a leg leaving the pool, the no-side level and the
cntlr's drop cancel them. They are never joined, and a probe stuck in the
kernel is accepted as unreclaimable until the disconnect frees it.

### The background disconnects

The sweep's `nvme disconnect` of a subsystem runs in a goroutine of its own,
because a controller delete whose target has vanished waits in the kernel
for the admin timeout of 60 s, in an uninterruptible write that no signal
ends, and run inline it would hold the cntlr's lock for that minute. The
goroutine registers the NQN in the disconnect registry for exactly as long
as it lives, takes one of eight slots, a quarter of the OS client's
(`disconnectConcurrency`), and runs the command through the OS client, so
it also holds an OS client slot while the kernel holds it. A second request
for an NQN already in the registry does nothing. The connect steps refuse
to adopt a connection whose NQN is in the registry, since its controllers
are being deleted (`cnagent.md`, CN10).

The registry is bookkeeping of work in progress, never memory of a failure:
whether a connection is still there is always read from the node. The pass
that set a disconnect going reports the connection as a leftover. A later
removing pass reports it until its probe finds no controller left, and a
check round's verdict names it for as long as the subsystem's entry stays
in the walk, which it does while a wrapper still holds the multipath device.
A disconnect that failed is issued again by the first removing pass that
finds a controller still there after the goroutine has gone.

### Shutdown

Cancelling the root context stops the retries and the probes at their next
check and lets a disconnect goroutine return before it takes a slot. None
of them is joined. A disconnect already in the kernel finishes on its own.

**In the code.** `startConnectRetry`, `stopConnectRetry`,
`startCloneMetaRetry`, `stopCloneMetaRetry`, `connectRetryLoop` and
`reconvergeCntlr` in `leg.go` of the cn package; the triggers are the calls
of `startConnectRetry` in `ensureLegs` and `build` (`syncup_cntlr.go`) and
in `ensureClone` (`clone.go`), and of `startCloneMetaRetry` in
`ensureClone`. The probes are `startLegProber`, `stopLegProbers`,
`legProbeLoop`, `runLegProbe` and `legProbeOutcome` in `healthcheck.go`,
over `LegProbeIO`, whose real implementation calls `common.WriteBlockAt`
and `common.ReadBlockDirectAt` in `common/osclient.go`. The disconnects are
`startDisconnect`, `disconnectInFlight` and `disconnectConcurrency` in
`leg.go`, called from `disconnectVerified` in `sweep.go`. The intervals
are `CnConnectRetryInterval`, `CnCloneMetaRetryInterval`,
`CnLegProbeInterval` and `CnLegProbeStallSeconds` in `common/constants.go`,
held as fields of `CnAgentServer` so tests can shorten them.

## 14. The rare paths

**The bitmap push.** The worker pushes one chunk of a clone's source bitmap
per call. Under the node read lock and the cntlr's lock the agent checks
that the cntlr holds the clone and that the chunk's two indexes are in
range: the source slice index below the clone's source slice count and the
chunk index below 16 (`MaxCloneBmCnt`). A chunk equal to the one already
stored is answered ok with nothing done. A new or grown chunk is written to
its file first, with a fsync of the file and of the directory, and only then
counted as applied; a save that fails leaves it out of the applied set, so
the worker pushes it again (`cnagent.md`, CN22). When the clone's dm-clone
exists, one `dmsetup info`, every chunk of the clone is folded into the
dm-clone's regions and every region that every slice marks unwritten is
discarded on the dm-clone, one `blkdiscard` per run of such regions; the
discard is how dm-clone is told that a region needs no copy. The fold costs
CPU over the region count times the slice count, and it runs over all the
chunks held, not only the new one. A failed discard is logged; the reply is
ok either way.

**The bitmap reads.** The gateway reads a thin device's bitmap, or a leg's,
through the primary for a migration or a clone that wants to skip unwritten
blocks. Both calls take the node read lock and the cntlr's lock and refuse
a cntlr that is not the primary. A thin device read dumps the metadata of
the requested slice's pool: a `mkdir -p` and a `stat` of the private dump
directory, a `reserve_metadata_snap` message, `thin_dump` into a file, one
read of the file, `rm` of it and a `release_metadata_snap` message, six
processes and one read, each under the 3 s limit. A dump that outgrows the
limit fails the call, and the caller copies everything, since bitmaps are
only an optimization (`cnagent.md`, CN25). The reply has one
bit per block of the requested window, set when the block is unmapped. A
leg read of a meta group's leg answers all zeros without a dump; a data
group's leg dumps its slice's pool and projects every thin device's
mappings onto the group's span of the pool's data device. Every call dumps
afresh, a paged read included (`cnagent.md`, CN25 to CN27).

**The capacity call.** It returns the capacity flag as given, zero meaning
"no local opinion", under no lock and with no read (`cnagent.md`, CN3).

**In the code.** `PushCloneBitmap` in `server.go` of the cn package and
`pushCloneBitmap` with `applyCloneChunks` in `push_clone_bm.go` and
`thinbm.go`; the fold is `foldRegions` and `regionSkippable` in
`thinbm.go`, the discards `agent.SkipRanges` and `agent.ApplySkipRanges` in
`agent/bitmap.go`. The bitmap reads are `GetThinDeviceBm` and `GetLegBm` in
`bitmapread.go`, over `dumpThinMetadata`, `thinDeviceBitmap` and
`legDataBitmap` in `thinbm.go`. The capacity call is `GetCnSize` in
`server.go`.

## 15. Three worked examples

The examples use the shape of the opening section: 32 slices, RAID1 groups
of two legs, one meta and one data group per slice, 128 legs, two cntlrs.
The counts are the formulas of the previous sections applied to that shape.

### A standby's first build

The pool is created with every side unprovisioned. The worker sends the
standby's first cntlr syncup: every leg is provisioning, so the converge
builds nothing. Its sweep lists the node, finds nothing of this pool, and
its build writes 128 provisioning rows; the reply is clean.

The disk nodes zero their sides. Each time the worker marks sides
provisioned, the pool's revision moves, and every cntlr and side of the pool
gets a syncup (`dnv-worker.md`, RW18). The standby's converge now connects
the legs whose sides are provisioned and still have no controller. For each
such leg:

1. the lookup finds no entry for the leg's NQN;
2. the walk is marked stale and `nvme connect` runs, one process, a few
   tens of milliseconds to a live disk node;
3. the lookup runs again to wait for the namespace head. The first lookup
   after a connect walks the node again, one listing and one NQN read per
   entry, since the new subsystem may have taken a freed number. When the
   head is not there yet, the pass pauses 50 ms and looks again, as long
   as the pass budget of 1 s covers the pause;
4. the head's number is read, and the wrapper is created: one info that
   finds nothing, then the create;
5. the row: info and table once more, then the transport view, which reads
   ok once every provisioned side has a live controller.

The budget is shared by every leg of the pass, so after about twenty pauses
it is spent. From then on a leg whose head is not there at its first lookup
fails with "no multipath namespace", and the pass registers the connect
retry. Five seconds later a retry attempt runs the whole converge again:
every connected leg costs its four `dmsetup` and its lookup, every leg
connected in this attempt costs a new walk, and the heads that were late
are there now. The attempts repeat until a pass registers nothing. A
converge that connects k legs on a node with E entries walks k times and
reads about k times E NQNs: for the last batches of a 128-leg build that is
thousands of reads, all in the agent's process. On the lab the standby's
build attempts held the lock about 14 s at 32 slices.

When every leg is connected, a steady standby converge is about 535
processes: the 128 legs' 512, the namespace's park, the dm-error, the
subsystem's `ls` and the sweep's listings. The paths of a standby's legs are
ANA non-optimized, which its leg rows accept.

### A promotion

A failover is one change of the pool's desired state, one revision, which
the worker delivers in three phases: the old primary is demoted, the sides
flip their exports, the new primary is promoted. The worker orders the three
with its two bounded waits
(`architecture.md`, Failover; `dnv-worker.md`, RW14, RW22).

**The old primary** gets its demotion first. Its converge builds the standby
plan. The health probes of its 128 legs are cancelled before the sweep. The
sweep's first pre-step moves the namespace to inaccessible, one probe and
one write. Its second pre-step reads the namespace device's table, finds it
over the raid0, which the plan no longer wants, and parks it: a reload onto
the dm-error, whose suspend completes the host IO in flight. Then the chain:
the raid0 goes at the sixth layer, three `dmsetup` with its verification;
the 32 thin volumes at the seventh, four each including the table read for
their identity, and no `delete` message since the pools go too; the 32
pools and 64 concats at the eighth, three each; the 64 arrays at the ninth,
one `mdadm --stop` and one sysfs read each. The build then runs the standby
shape over the legs that stay: 512 `dmsetup` for the wrappers, the dm-error,
the parked namespace device, the subsystem. About 1,000 processes, several
seconds under the lock; the worker's demotion hold waits for this cntlr's
report for at most 3 s and then releases the sides whether or not the
report came.

**The sides** flip: each disk node moves the new primary's path to ANA
optimized and the old primary's onto a dm-error. The worker holds the new
primary's syncup until the healthy sides have reported, at most one check
interval.

**The new primary** gets its promotion. Its converge builds the primary plan
over legs that are already connected, so its sweep removes nothing and its
legs cost their 512 `dmsetup` and lookups. A leg is available only when its
path is both live and ANA optimized. When a side's flip has not reached this
kernel yet, the leg is late, its group is assembled without it or not at all,
and the pass registers the connect retry; the promotion syncup can land before
the flips because the worker's wait is bounded (`architecture.md`, [D16]). For
each group the md walk, taken once, shows no array, so each group is
assembled: `mdadm --examine` of each available member, 128 processes, then
`mdadm --assemble` of the members with a superblock, 64 processes, and mdadm
itself refuses a lone member whose superblock still counts a clean full array.
Then per slice the two concats are created, the pool is created, which reads
its metadata through the meta group, and its status is read; per slice the
thin volume is created with no message, since the thin device is created
already; the raid0 is created; the namespace device is reloaded from the
dm-error onto the raid0; the subsystem and namespace are probed; and last the
namespace is moved to optimized. About 980 processes, plus 192 `mdadm` runs
for the assemblies, several seconds under the lock. The ANA step reads the
plan, not the stack: with a late member the namespace is moved to optimized
over the dm-error all the same, and hosts take IO errors until a retry attempt
has built the stack, within about 5 s of the flips reaching the kernel
(`cnagent.md`, Known limits). The new primary's leg rows read pending until
its fresh probes have completed a round, so a dead leg's epoch is not cleared
by a row the new primary has not earned.

Every check round of either cntlr during these converges waits behind them.
The worker counts a primary's missed rounds, and the new primary is
settling, so its failover threshold is the cntlr threshold of 600 s rather
than 4 s until its first clean round as primary (`dnv-worker.md`, HL2,
AR5).

### A new thin device with its namespace on a settled pool

The operator creates a thin device. The gateway writes it and bumps the
pool's revision; the worker sends a syncup to both cntlrs.

**The primary's converge** runs the whole pass. The unchanged parts cost what
they always cost: the sweep's listings, the 128 legs' 512 `dmsetup` and
lookups, the 64 groups' sysfs reads, the 32 slices' 320 `dmsetup` with their
32 status reads. The new thin device adds, per slice: one `dmsetup info` of
the volume, absent; the `create_thin` message, since the thin device is not
yet created; the pool's number; one info and one create of the volume. Then
its dm-error, info and create, and its raid0, 32 numbers plus info and
create. About 1,030 processes; nothing of it is a namespace yet. The
standby's converge builds the thin device's dm-error, two `dmsetup`, inside
its usual 535.

**The created flip.** The primary's reply shows the thin volume ok in every
slice, so the worker sets the thin device created, which is a change the
agents must see: it ends the create messages. The pool's revision moves
again (`dnv-worker.md`, RW19), and both cntlrs converge once more, about
1,000 and 535 processes, writing nothing.

**The namespace.** The operator creates the namespace. A third revision, a
third converge on each cntlr. The primary adds the namespace device, the
raid0's number, one info and one create, the namespace in nvmet, two
probes, a `mkdir`, the device path, the two identities, the inaccessible
group and the enable, and the ANA step, one probe and one write of the
group. The standby adds the namespace
device parked on the dm-error, and the namespace inaccessible.

Three converges of about a thousand processes each on the primary, at 2 to
5 ms a process, are three windows of 2 to 5 s under the cntlr's lock, and a
check round that arrives in a window misses when the window outlasts the
2 s wait. The suite's pool creation and its first volume are a chain of such
windows, which is where the slow rounds of the slice-32 problem were first
seen.

**In the code.** The examples trace `convergeCntlr` and `build` in
`syncup_cntlr.go` of the cn package through `ensureLegs` and
`awaitNsHead` (`leg.go`), `ensureGroup` and `assembleGroup` (`md.go`),
`ensureSlice`, `ensurePool` and `ensureThin` (`pool.go`), `ensureRaid0`,
`ensureNsDev`, `parkNsDev`, `ensureSubsystem` and `setAna` (`td.go`), and
the sweep's `cntlrAnaPreStep`, `cntlrPreSteps` and `runChain` (`sweep.go`).
The standby's leg row is `transportHealth` in `leg.go`; the primary's is
`legProbeOutcome` in `healthcheck.go`.

## 16. Where the time goes, in full

This section puts numbers to the passes. The symbols: L legs of the cntlr,
spares included; G groups; N slices; T thin devices; NS namespaces; SS
subsystems; X transfers; C clones; E connected NVMe subsystems on the node,
one per leg of every cntlr on it plus clone sources; A md arrays on the
node, with m members each. Every count is for a steady pass, where nothing
has to be created or removed; a change adds its own commands. The unit
costs are the ones of the section "Where the time goes, in short".

### Processes per pass

| pass | processes |
|---|---|
| primary converge | 2 + SS (the sweep's listings and one `ls` per subsystem) + NS (the park check) + 4L + 10N + 4TN + (N + 4)T + 3NS (4 for a namespace over a dm-clone) + 3SS + 2NS (the exports) + NS (the ANA step) + about 8X + about 12C |
| standby converge | 2 + SS + NS (P1) + 5NS (the park check and the park) + 4L + 2T + 5NS + 3SS + 2NS |
| primary check round | 2 + N (status reads) + 1 + SS (the verdict's `ls`) + 4SS + NS (the exports) + C (status reads) + one status read per namespace over a dm-clone + 1 when C > 0 (`losetup`) + about 6X |
| standby check round | 2 + 1 + SS + 4SS + NS |
| node check round | 7 + the number of live dm-clones on the node |
| node syncup, all in place | about 6, plus the node sweep's 4, one `dmsetup table` per clone metadata device and per live dm-clone on the node, and its removals |

For the example shape, L = 128, G = 64, N = 32, T = 1, NS = 1, SS = 1:

| pass | processes | of which `dmsetup` |
|---|---|---|
| primary converge | about 1,010 | about 1,000 |
| standby converge | about 535 | about 525 |
| primary check round | about 41 | 34 |
| standby check round | about 9 | 2 |

### File reads per pass

| pass | reads in the agent's process |
|---|---|
| any cntlr pass | the walk: 1 listing + E |
| converge | the arrays for the sweep: 1 listing + A(2 + m); the md walk for the build: 1 listing + A(1 + m); the groups: 8G; the legs: about 8L; the concats: 2N; the exports: 6SS + 15NS |
| primary check round | the md walk: 1 listing + A(2 + m); the groups: 8G; the concats: 2N; the exports: 5SS + 5NS |
| standby check round | the legs' lookups: about 7L; the exports: 5SS + 5NS |

At 0.04 ms a read, two thousand reads are under a tenth of a second. The
reads matter less for time than for the log, where each is one record.

### Where a converge reads one device more than once

| device | reads in one converge | by |
|---|---|---|
| a leg's wrapper | info and table twice | the ensure, then the row's probe |
| a thin pool's number | once per thin device | every thin volume of the pool asks for it |
| a thin volume | info, info, table, then info once more | the ensure, then the raid0's argument list |
| a namespace in nvmet | three probes of one `ls` and five reads | the ensure's own probe, the shared ensure's probe, the ANA step |
| a dm-clone's status | four or five times | the step-2 read, the knobs, the enable, the final read, and once per namespace it backs |
| the md arrays | two walks | the sweep's strict listing, then the build's tolerant walk |
| a subsystem's directory in a round | two `ls` | the exists check and the probe's own exists check |

### The locks, in time

| holder | holds | for | measured on the lab |
|---|---|---|---|
| a cntlr syncup | the cntlr's lock | a converge and a save | primary 4.5 to 5.4 s, standby about 3 s, before 33c07d3 |
| a retry attempt | the cntlr's lock | a converge, with no deadline, and the next attempt right after it when it ran over 5 s | 21 to 25 s per attempt during a build |
| a check round or an info read | the cntlr's lock | a reading, the rows and the verdict | 0.36 to 0.54 s on a primary, 0.03 to 0.08 s on a standby, after 33c07d3 |
| a node syncup | the node lock for writing | the base state and the node sweep; it also keeps every new reader out from the moment it waits | not measured |
| the start of the agent | the node lock for writing | every node and cntlr converge, before the listener opens | not measured |

The worker's wait for a round is one check interval, 2 s by default. A
round that waits behind a converge of more than 2 s misses. While rounds
miss, the worker starts one every two intervals: it waits one interval for
the reply and then re-arms its timer. With the default thresholds, a primary
whose rounds miss twice in a row, which a converge of about 6 s under the
lock produces, has a health epoch of 4 s and two unhealthy verdicts in a
row, which are the failover's conditions (`dnv-worker.md`, AR5, AR10).
The e2e suite runs with thresholds of 1,800 s, so it sees no such failover.

### Serial order

Inside one cntlr's pass nothing runs in parallel: one loop over the legs,
one over the groups, one over the slices, one over the thin devices and
slices, one over the namespaces, one over the subsystems, and in the sweep
one over each layer's objects. Each loop body waits for its commands one at
a time. The OS client's 32 slots are therefore almost idle during a
converge of a single cntlr: one is in use, and the others serve the other
cntlrs' passes and the background disconnects. Two cntlrs of one node
converge beside each other, each on its own lock.

The plan, the walk, the md walk, the view of the metadata area, the wait
budget, the info's maps and the reading's own-read caches are all plain
per-pass values with no lock of their own. They are safe because one pass
runs them from one goroutine.

### Parts that grow with the square of a count

* **A first build of many legs.** Every connect marks the walk stale, so the
  next lookup walks again: a converge that connects k legs walks k times,
  each walk reading E NQNs. The repeated walks of a standby's first build of
  128 legs are about 8,000 reads; the build's own connects and creates are
  processes beside them.
* **A teardown of many connections.** The sweep's check that a connection is
  gone walks the subsystem entries once per connection, so a pool's
  teardown or a level that takes the legs away makes L walks of E reads
  (`cnagent.md`, Known limits).

Not of that kind, but worth knowing: the attribution of a host-facing
subsystem no held request names is linear, one `ls` and at most one read
per namespace in every sweep and verdict, while a subsystem the requests
name costs nothing.

### Waits inside a command

* **Thin-pool status.** `dmsetup status` of a thin pool makes the kernel
  commit the pool's metadata, and flush its data device when the metadata
  changed. Every converge of a slice and every primary round pays this once
  per pool, 9 to 33 ms on the lab. A pool whose commit cannot complete
  blocks the read, and with it the pass and the lock it holds (`dnagent.md`,
  SH17).
* **Connects to a dead disk node.** A side whose controller is gone and
  whose disk node is down costs about 3 s per pass for its first connect,
  which the budget never skips; one dead node's sides are tried one after
  another.
* **Controller deletes.** A `nvme disconnect` whose target has vanished sits
  in the kernel for the admin timeout of 60 s. It runs in the background,
  but holds one of the eight disconnect slots and one OS client slot for
  that minute; a teardown of many such connections proceeds eight at a
  time.
* **Configfs.** A `mkdir`, `rmdir` or `ls` in the nvmet tree waits for the
  directory's lock, which another operation holds for its whole run; the
  time limit cannot end that wait (`dnagent.md`, SH18).

### The log and the store

A converge of the example shape writes one record per process and per
read, about 3,200, plus two gRPC records and one store record. The request
is rendered to JSON twice, on receive and on save; a cntlr request at 32
slices lists 128 legs with their sides. A round writes a record per read
too, about a thousand on a standby, and the whole-node table listing is in
the record of its command in full. The store save is one write with two
fsyncs: at the end of a cntlr syncup under the cntlr's lock, before the
converge of a node syncup under the node write lock, and before the apply
of a bitmap push.

**In the code.** The loops are in `build` of `syncup_cntlr.go`, `probeCntlr`
of `probe.go` and `runChain` of `sweep.go` in the cn package. The repeated
reads are in `ensureLeg` and `legInfo` (`leg.go`), `ensureThin` and
`poolArgs` (`pool.go`), `raid0Args`, `ensureNamespaceObject`, `setAna` and
`probeExport` (`td.go`), `ensureClone`, `ensureHydrationKnobs`,
`enableHydration` and `probeCloneDm` (`clone.go`), `Nvmet.EnsureNamespace`
and `ProbeSubsystem` (`agent/nvmet.go`), `Md.ListArrays` and `Md.Walk`
(`md.go`). The stale-walk rule is `passWalk.stale` in `leg.go`; the
per-connection walk is `NvmeHost.ListSubsys` in `agent/nvmehost.go`, called
by `disconnectVerified`. The budget's first-connect rule is `connectWithin`.
The log records come from `common/osclient.go` and `common/interceptor.go`;
the save is `Store.Save` over `WriteProto` and `atomicWrite`.

## 17. Where the code and the tracked docs differ

While writing, every statement of a tracked doc that this doc leans on was
compared with the code at 33c07d3. The rules cited above match the code.
One statement is narrower than the code's behaviour:

1. **The reason for listing the nvmet tree with `ls`.** The shared agent
   rule says that a converge or a check round lists only a few configfs
   directories and that their number does not grow with the slice count
   (`dnagent.md`, SH18). That is true of the slice count. The number does
   grow with the subsystem and namespace counts: a converge lists about
   four directories per subsystem and three per namespace, a check round
   about five per subsystem and one per namespace, each `ls` a process, and
   a transfer adds as many again.
   The rule's conclusion stands for the pool shapes seen so far; its reason
   is incomplete for a pool with many namespaces.

Nothing else was found. The doc text was checked against the code for: the
lock mapping (`cnagent.md`, CN1); the pass structure and the effective
desired state (`cnagent.md`, CN9); the legs, the walk and the lookup
(`cnagent.md`, CN10); the health probes (`cnagent.md`, CN11); the groups
(`cnagent.md`, CN12); the thin volumes and the thin-id sweeps
(`cnagent.md`, CN14); the namespace backing and ANA rules (`cnagent.md`,
CN16); the clone steps (`cnagent.md`, CN18); the level gates
(`cnagent.md`, CN19); the sweep's scopes, enumeration, attribution and
layers (`cnagent.md`, CN21); the read-only calls and the reading
(`cnagent.md`, CN23, CN24, CN28, CN30); the lock hierarchy, the save
points, the time limits, the probe conventions and the check stream
(`dnagent.md`, SH5, SH10 to SH13, SH15 to SH20, SH24 to SH27); the OS
client's limit and logging (`osclient.md`); the device stack, the names,
the store paths, the check streams and the failover steps
(`architecture.md`); and the worker's round, hold and reaction rules this
doc cites (`dnv-worker.md`, RW4, RW5, RW8, RW14, RW19, RW22, HL1, HL2, AR5,
AR7, AR10).

## Appendix: the files

The cn package, `agent/cnagent`:

| file | holds |
|---|---|
| `server.go` | the server type, its state maps and registries, the lock-taking entry points of six calls: the capacity call, the two syncups, the push and the two info reads |
| `check.go` | the two check streams and their rounds |
| `reading.go` | the reading of a check round and an info read, and the helpers that take a device's flags, table and number from it or from one `dmsetup` |
| `plan.go` | the plan: names, sizes, level gates, the effective desired state, the namespace backing and ANA rules |
| `syncup_cn.go` | the startup reconcile, the node syncup, the base-state converge, the drop of removed cntlrs, the node-level probe |
| `syncup_cntlr.go` | the cntlr syncup, its gates, the converge frame, the build, the snapshot messages |
| `sweep.go` | the enumeration, the attribution, the wanted set, the chain, the pre-steps, the ten layers, the clone-source rules, the two sweep scopes |
| `leg.go` | the pass's NVMe walk and the lookup, the leg connect and wrapper, the leg rows, the background disconnects, the two retry loops |
| `healthcheck.go` | the health probes and their direct block IO |
| `md.go` | the md wrapper over sysfs and mdadm, the md walk, the group assembly and member reconciliation, the group row |
| `pool.go` | the concats, the thin pools, the thin volumes, the thin-id sweep, the pool and thin rows |
| `td.go` | the raid0, the namespace device and its park, the nvmet subsystem and namespace steps, the ANA write, their rows |
| `clone.go` | the clone build in its six steps, the clone rows |
| `clonemeta.go` | the base-state tools and the metadata area's allocator |
| `xfer.go` | the transfer device, export and rows |
| `dmutil.go` | the probe-first ensure helpers for single and multi-target devices, the dm-error among them, the dm probes, the removal helpers |
| `thinbm.go` | the thin metadata dump, the bitmap math, the folds, applying bitmaps to a dm-clone |
| `bitmapread.go` | the two bitmap reads |
| `push_clone_bm.go` | the bitmap push and the list of chunks held |
| `probe.go` | the rows of a check round and an info read |

The shared agent code, `agent`:

| file | holds |
|---|---|
| `agent.go` | the start routine: reconcile, then listen; the per-round trace id |
| `locks.go` | the node lock and the per-object locks |
| `oswrap.go` | the command wrapper with the 3 s limit, the strict and tolerant attribute reads, the in-process directory listing, the configfs `ls` |
| `dm.go` | the device-mapper wrapper: one-device reads, the two whole-node listings, create, reload, remove, message, discard, the table builders |
| `nvmet.go` | the nvmet wrapper: port, subsystem, namespace, port link, their probes and removals |
| `nvmehost.go` | the NVMe host wrapper: connect, disconnect, the walk and the lookup shared by both agents |
| `store.go` | the local store: list, load, save, remove |
| `resinfo.go` | the tracker of rows and epochs |
| `revision.go` | the revision gate and the reply builders |
| `conf.go` | the geometry check |
| `sweep.go` | the sweep result: leftovers, failures, the verdict's reply |
| `waitbudget.go` | the wait budget of a pass |
| `bitmap.go` | the chunk stores and the discard ranges |

The common code and the program:

| file | holds |
|---|---|
| `common/osclient.go` | the OS client with its slots and its log records; the two raw block helpers the health probes use |
| `common/constants.go` | every constant named in this doc but the disconnect slot count, which is in the cn package's `leg.go` |
| `common/name_fmt.go`, `common/name_parse.go` | the device, array, NQN and store names, and their parsers |
| `common/interceptor.go`, `common/log.go` | the gRPC logging and the rendering of a message to JSON |
| `cmd/dnv-agent/main.go` | the flags and the wiring of the cn role |
| `pb/schema.proto` | the requests, replies and info messages |
