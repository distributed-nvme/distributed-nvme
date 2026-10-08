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
five statements; a smaller shape proves no ceiling, and one without redundancy
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
  slice grows and the level ladder, a failover, a leg repair and all between.
* **The four reactions fire on real faults** (`dnv-worker.md` AR5 to AR8): a
  killed cn agent moves the primary role and then loses its cntlr, a killed dn
  agent with its nvmet port removed loses its leg to a spare, and strided
  writes that push a thin pool past its low water mark gain its slice a data
  group.
* **The lab is left as it was found.** After every case every disk node has all
  its extents back, no node guest holds a dnv dm device, md array or nvmet
  subsystem, and allocation stays under a per-file and a whole-run cap.

**What it does not prove.** The RPCs it never issues, the deletions and
listings of clusters, disk nodes and controller nodes: the gateway suite covers
the node deletions, and this run discards the whole cluster between cases.
Error paths, beyond the few refusals the ops case asserts on purpose and the
`NOT_FOUND` reads after the copy case's deletions; validation and CLI semantics
are the gateway and dnvctl suites'. Concurrency: one dnvctl call and one case
at a time, and never a revision token, since a present token changes what the
gateway checks (`gateway.md` GW6; `dnvctl.md` CT3). ANA path selection between
two usable controllers (Known limits).

## Topology and parameters

Every guest comes from the command line (E2E1), each in one role. The
control-plane guest runs etcd, `dnv-gateway`, `dnv-worker`, `dnv-cdc` and
`dnvctl` as the login user. At least three controller-node guests run one
`dnv-agent cn` each: two carry the pool's cntlrs and the rest none, where the
ops case's third cntlr and the cntlr replacement land. The disk-node guests, at
least as many as a group has legs, run many `dnv-agent dn` instances each,
every one on its own loop device and its own nvmet port and service id, as two
ports cannot listen on one address, all registered at their guest's location
(E2E4). Two hosts run only the kernel's nvme-tcp stack and nvme-cli.

The control plane has a guest of its own, as in a deployment
(`architecture.md`, System overview): it holds no kernel state and runs without
root, the react case's faults never reach the processes that must observe and
repair them, and its address, an IP literal as the cdc requires, is what the
hosts discover against. Every daemon log lives under a work directory the
between-cases rebuild removes, so each case's logs replace the last and a
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
set, as cntlr
replacement and leg repair wait out thresholds whose defaults lie beyond any
wait the suite could afford (`dnv-worker.md` AR7, AR8). The quiet set names all
four, as an omitted or zero threshold resolves to its default when read
(`gateway.md` GW11; `dnv-worker.md` AR4) and the default primary threshold is
short; its leg threshold exceeds its side threshold (`architecture.md`, Common
validation).

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
Backing files are created sparse. A loop device without Write Zeroes is refused
before its agent starts and again before the first pool create, which also
requires every agent's device on record: the dn agent only tags such a disk
(`dnagent.md` DN5), and side zeroing would write zero pages at bulk speed. The
gate guards speed, not space, as a loop device allocates what it zeroes either
way (`dnagent_integtest.md`, Assumptions and preflight checks). The allocation
caps are asserted after every case.

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
drop. The read-only level's read, which may block, runs detached and answers
"blocked" within its own budget, as `timeout` cannot bound a task in
uninterruptible sleep; every other host read, cache drop and device write,
except that level's refused write, which dm-flakey fails rather than queues,
runs under a driver-side watchdog (`ssh_host_watched`) that abandons IO
blocking where nothing said it would, so the run dies with diagnostics, never
hangs.

E2E8. **Pids in files, signals by pid, `pkill` only from helper files with
bracketed patterns.** Every agent and every control-plane daemon the suite
starts, etcd included, records its pid: the control-plane daemons are stopped
by theirs, and the react case stops each agent it kills by that agent's pid
file. The cleanup's pattern sweeps, which also reach a crashed run's
processes whose pid files are gone, sit in helper files on the guest behind
helper verbs, bracketed so they never match the ssh command's own shell, and
qualified by the suite's work directory or its etcd name so they never touch
another suite's processes.

E2E9. **One dnv suite runs at a time in the lab** (`layout.md`, Directory
tree). The suite occupies every guest it names; no other suite binds its
ports. Nothing can enforce the rule from inside: the suite prints it before
the first ssh and checks what it can, that no port of its block listens and no
nvmet port it needs exists.

E2E10. **Hosts reach namespaces through the cdc, and every path a host holds is
the suite's own.** The kernel's autoconnector is masked at preflight and at
every rebuild and read back, and nvme-stas must be inactive. A host discovers
through the cdc wherever the subsystem has a `CdcEntry`, expecting one record
per enabled cntlr (`cdc.md` DS3). Three connects are direct by necessity: to
the transfer, which has no `CdcEntry`; to the copy case's fallback source,
where discovery would also bring in a namespace whose uuid host1 already holds;
and to the new primary after a failover, while the cdc still advertises the
dead node until its replacement. Every connect sits between an export gate and
a connect verdict, as the discovery log runs ahead of the data plane and
nvme-cli can exit zero having connected nothing.

E2E11. **Each case starts from an empty etcd and a freshly built pool.** An
etcd reset is not enough: a recreated cluster mints new cluster and disk-node
ids (`architecture.md`, cluster_id derivation), the dn agent refuses a disk
whose header names the old ones (`dnagent.md` DN5), and only the cleanup zeroes
the headers and removes the controller nodes' stores and arenas. So between
cases the suite runs the whole cleanup and its start gate, then the
infrastructure and the whole of setup, which clears every id the last case
read; that rebuild is also where the next case's thresholds take effect.

E2E12. **The shell literals mirror named constants and say so.** Every number a
Go constant owns is repeated beside a comment naming the constant, except two
read from `workerctl constants` at the driver's preflight: `EtcdMaxTxnOps`,
which etcd must be started with and which this suite, committing the
transaction it is sized by at two cntlrs, short of the widest shape, must not
hand-copy; and `MaxAllocLegPerGrp`, cross-checked against the leg count.

## The cases

The cases run in order, each on a fresh pool (E2E11): smoke, ops, copy and
react. Each stage has its own trace id, sent by every dnvctl call of the stage
and carried by the gateway's records and an agent's records of the reads the
gateway makes for that call, never by the converge it sets off, which runs
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
(`cnagent.md` CN16). The random pattern written through it gives the digest
every later step compares.

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
a side to a disk node outside its group, on a distinct cntlid slot, and
finishes under the same leg id with an optimized path before any read
(`architecture.md`, Migration; `dnagent.md` DN12, DN13); a cancelled one
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
stops the run. The dead cntlr is replaced on a node that held none, keeping
its slot and role (`dnv-worker.md` AR7), and the dead agent, restarted,
removes what it left (`cnagent.md` CN7). Leg repair (raid1 only) needs both
planes, as kernel objects outlive their agent: a dn agent is killed and its
nvmet port removed, on a disk node with no other side of the pool, and the
group must gain a spare outside its nodes, park the dead leg and rebuild md
(`dnv-worker.md` AR8; `cnagent.md` CN11, CN28), with no failover meanwhile,
proved by an unchanged count of the worker's failover records.

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
each backing file's allocation, which bounds the extents its disk node zeroed,
since zeroing allocates, the run's allocation at the pool's own extents plus
slack, and every node's and the control-plane guest's free space at
preflight's floors.

**What a pass means.** Exit status zero and PASS mean that every assertion of
every case held in a run that stops at its first failure, and that the end
cleanup then finished on every guest with none of the suite's own nvmet ports
left standing. Data is checked against a pattern the system never sees,
removals by the nodes' own listings and `NOT_FOUND`, reactions by the records
they write. A pass may have taken the clone fallback, whose branch is logged,
and a skipped check — a raid1-only step without redundancy, a
guest-distinctness check without headroom — is a logged line, not a proof.

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
masked autoconnector (E2E10); and that the control-plane guest has its tools,
its ports free and the free-space floor the space guard holds it to, and needs
no sudo: the closing trim is the one root command tried there and may be
refused. The loop devices are checked once the agents have made them (E2E5).
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
nothing left (`cnagent_integtest.md`, Lab facts); a host drops the suite's and
dnv's NQNs and its discovery controller, never every controller, unmasks, and
keeps its identity files, node identity rather than run state, the dnv prefix
also taking the cdc suite's subsystems on a shared host (E2E9). Every
controller node goes before any disk node, as its stacks sit on the disk
nodes' sides, in the cn agent suite's order (`cnagent_integtest.md`, Teardown
and cleanup) and in two phases across all of them, so that every clone final
is gone, removed while its transfer source is connected, before any transfer
goes, as a dm-clone flushes through its source (`cnagent.md` L3). On a
disk-node guest every dnv md array goes before any dm device, as an array
assembled from the controller node's superblocks pins the device under it
(Known limits), each loop device's header is zeroed before it is detached, and
the mask goes once no device exposes a dnv superblock. The control plane goes
by recorded pids, the pattern sweep as fallback (E2E8), and a best-effort
filesystem trim on every guest closes.

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
nvmet and log state follows, and the failure line ends with the filter that
pulls the stage's records. When the stage connected, the host's kernel log is
the record of what the connect attempted.

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
  survivor by device; a wait after an explicit disconnect demands the path
  gone, which a wrong address also satisfies.
* **A blocked probe or abandoned host IO leaves an unkillable dd or sync
  behind** (E2E7) until its device or path serves IO again.
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
  exists, but an export linked later leaves the group unbuilt for a round, a
  settled primary can be failed over on that one round (`dnv-worker.md`, Known
  limits), and react then stops the run.
