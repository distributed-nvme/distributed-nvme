# dnv: scope, promise and known limits

## How to read this document

This document says what dnv promises, what loss it accepts, what it does
not do, and which limits it has today. It has two readers. The first is a
person who runs dnv or builds a service on it. The second is the LLM that
implements dnv. The LLM follows this document as it follows the design
documents. Section 11 says how.

The words are those of [the core glossary](core_glossary.md). Any other
project word is explained where it first appears. The sentences are short
on purpose.

Every known limit has a state:

- **accepted**: dnv keeps the limit. The entry says why.
- **fix decided**: the fix is decided. It waits for its plan or its code.
- **fix later**: a fix is wanted. It waits for a named condition, or no
  way to fix it is known yet.
- **procedure to write**: a human can handle it by hand. The steps are not
  written yet.
- **procedure written**: the steps are in a document. The entry links to
  it.

An entry leaves this document once its fix is done and tested.

Some limits were read from dnv's code or from the kernel's code. No test
has shown them yet. Such an entry carries the mark "not seen in the lab".
Such a limit is named all the same, so that the reader knows of it.

Sizes, measured times and the table of enforced sizes stay in
[the introduction](introduction.md#status-and-limits-of-01). This document
links to them and does not repeat them. The limits of the test suites are
not in this document.

## 1. What dnv is

dnv is a distributed block storage system. It takes the raw disks of disk
nodes and joins them into storage pools. Controller nodes run the volume
logic: thin provisioning, striping over slices, RAID1 over two legs,
snapshots, clones and live migration. Hosts reach their volumes over
NVMe/TCP. A host joins its paths to a volume with the kernel's native NVMe
multipath. ANA, the NVMe way of saying which path serves IO, points the
host at the primary.

etcd holds the desired state. The gateway takes calls and writes the
desired state. The worker watches it, drives the agents and reacts to
faults. The agents on the nodes build the data plane from stock Linux
pieces: device mapper, md RAID1, the kernel's NVMe target and the kernel's
NVMe host driver. The cdc, dnv's discovery service, tells hosts which paths
exist. dnvctl is the command line. The CSI driver, `dnv-csi`, connects
Kubernetes: it makes one storage pool for each Kubernetes volume and
connects the volume on the node where a pod uses it.

dnv is meant to be the data plane of a multi-tenant block storage service,
for example behind Kubernetes or OpenStack Cinder. dnv knows no tenants.
The service above it authenticates its users. That service decides which
pools, subsystems and hosts serve each user.

## 2. What dnv does not do

| Absence | Out for good, or not yet |
|---|---|
| Authentication of users. Encryption on the wire or at rest. | for good: the service above dnv and the network own them |
| A login of hosts with keys, NVMe's in-band authentication | not yet |
| Backup of volume data | for good |
| QoS, an IO rate limit per volume or per pool | for good |
| Redundancy other than RAID1 with two legs | for good |
| Platforms other than Linux x64 | for good |
| Metrics, health endpoints and alerts | not yet |
| Growing a volume | not yet; Cinder and Kubernetes ask for it |
| Reverting a volume to a snapshot | not yet; Cinder asks for it |
| Changing a pool's thresholds or the cluster's check intervals after creation | not yet |
| Changing a controller node's budget, the pool space it may serve, after creation | not yet |
| Rolling upgrades | not yet; every program is upgraded at once |
| Transports other than NVMe/TCP | not yet |
| More than one primary per pool | not yet |
| cdc: host registration and mDNS discovery | not yet |
| CSI driver: volume list, snapshots, expansion, access from several nodes | not yet |

"For good" means dnv will not have it. "Not yet" means dnv may get it
later. Nothing is planned for it today.

## 3. The promise: one pool never reads another pool's bytes

dnv promises the service above it one thing. The data of one storage pool
is never readable from another storage pool. The service itself may copy
data across pools, with a transfer and a clone. That is not a break of the
promise.

**What keeps the promise.** A host reads a volume through a namespace. A
volume is thin. A block the volume never wrote reads as zeros, whatever
the disk holds there. dm-thin zeroes a block before the first partial
write lands on it. So the old bytes of a freed extent never reach a host
through a volume. The RAID1 metadata of a new leg is zeroed before use, so
md never trusts an old superblock.

**What the promise does not cover.**

- **The network and root are trusted.** dnv checks no user. A host is
  known by its NQN alone, and a host can claim any NQN. All traffic is
  plain text. Root on a node, and anyone on the storage network, can read
  the old bytes of a recycled extent from a raw disk or from a leg. Anyone
  who reaches an agent's port can send it a desired state. Deploy dnv on
  an isolated network you trust. State: accepted. Procedure to write: the
  network section of the operations doc, the document of procedures still
  to be written, with a table of who connects to whom and a firewall
  example.
- **In Kubernetes every pod reaches dnv's ports.** Every pod reaches the
  gateway, the agents, etcd, the cdc and the NVMe/TCP ports through its
  machine's network. Only a firewall rule or a network policy of the
  operator blocks it. The trust dnv puts in the network is then trust in
  every pod. State: procedure to write, a network policy example in the
  CSI tutorial.

**Steps of a caller that can break the promise.** A migration and a clone
can skip the blocks a volume never wrote. Today the caller reads the list
of written blocks and hands it over, in pieces. The list must be read at
the right time.

- **A leg's written-block list read too early in a migration.** The read
  must come after the old side stopped taking writes. An earlier read
  misses the later writes. The copy skips them. The pool then reads old
  bytes of the new side's extents there, with no error. The bytes may be
  another pool's. Until the fix, read the list only after the cutover,
  once the primary's path to the new side is ANA optimized. Today a piece
  that reaches the new side once it serves marks nothing, so such a read
  skips nothing until the new side is built again, as after a reboot of
  its disk node. State: fix decided, the
  worker reads the list itself once the new side serves and hands it over
  in pieces of a fixed size; the new side applies a piece while it serves;
  no caller reads or hands over a migration's list any more. Not seen in
  the lab.
- **A source volume's written-block list read too early for a clone.**
  The read must come after the source stopped taking writes. An earlier
  read makes the clone serve zeros where the source has data, with no
  error. State: fix decided, the gateway refuses the read until the
  transfer has moved the source volume's namespace to ANA inaccessible on
  the source pool's primary. Not seen in the lab.
- **A written-block list in pieces can shift.** One piece, one
  `--bm-hex` of dnvctl, covers only part of a large volume or leg, so such
  a list goes in pieces. A piece carries no block count. A piece before the
  last with a block count that is not a multiple of eight, or a piece sent
  twice without its position, shifts every later block. A migration's copy
  then skips written blocks, and the pool reads old bytes of the new
  side's extents there. A clone reads zeros where the source wrote. No
  error shows. Until the fix, read every piece but the last over a
  multiple of eight blocks, and give dnvctl the piece's position,
  `--offset` for a clone or `--bm-idx` for a migration, whenever you may
  send a piece again; a piece sent again with its position is left as it
  is. State:
  fix decided. For a migration the worker cuts the pieces itself. For a
  clone the gateway refuses a read whose block count is not a multiple of
  eight before the end of the volume, and dnvctl always sends a piece's
  position. Not seen in the lab.
- **A forced finish of a migration before the copy is whole.** The finish
  with force skips the proof that the copy is whole. The regions the copy
  had not reached read old bytes of the new side's extents, possibly
  another pool's, with no error. State: accepted. The operator asks for
  it with force. Procedure to write, one line in the operations doc: force
  is right only when an agent cannot answer the proof.
- **A migration cancel after the new side serves.** Once the new side
  serves, it holds the only copy of the leg's new writes. A cancel removes
  that side. The old side serves its content as of the cutover. State: fix
  decided, the gateway refuses the cancel once the new side has served. A
  force flag keeps the escape, and the operations doc will say what it
  loses. Not seen in the lab.
- **A forced start from an older copy restores one group, not the pool.**
  A forced start is an operator's start of a group's RAID1 array from one
  leg they name, when md will not start it by itself. Blocks rewritten
  since the leg left read old bytes. Blocks first mapped since then read
  what the leg holds there, possibly another pool's bytes. No error is
  shown. State: accepted. The operator asks for it. Procedure to write:
  the forced-start guide says which leg to choose and what an older copy
  costs.

## 4. Data loss dnv accepts

dnv accepts data loss in four cases. In every other case a data loss is a
defect. Section 5 lists the defects known today.

1. **Every leg of a group fails at once.** In a RAID1 pool no disk node
   holds two legs of one group. Losing fewer disk nodes than a group has
   legs never loses a group. Losing every leg of one group loses that
   slice's thin pool. Every volume is striped over all slices, so the
   whole pool's data is lost. A repair finished between two losses
   restores the redundancy. So a RAID1 pool survives the loss of any one
   disk node, but not every loss of two. The pairs of disk nodes that can
   cause it are at most the pool's group count. For two random failures
   among N disk nodes the chance is at most that count divided by
   N(N-1)/2.
2. **A pool without RAID loses a disk or its disk node.** A group has one
   leg there. The data of the pool is gone. No repair exists for such a
   leg. A migration can move a side that still reads but is sick. An
   operator must start that migration. A side of a clone meta group, the
   group of the pool made for one clone, cannot be migrated in a pool
   without RAID today. State: fix later, small.
3. **A clone loses its whole source pool or its whole destination pool
   before the copy is complete.** With the source gone, the copy stops.
   A host's read of a block not yet copied waits or fails. With the
   destination gone, every write since the host moved over is lost.
4. **A migration loses a side.** Before the cutover, the old side's failure
   loses the leg, and the new side's failure loses nothing. After the
   cutover and before the copy is whole, either side's failure loses the
   leg. After the copy is whole, only the new side matters. RAID1 then
   decides whether the pool loses data.

Two more things fall under case 1:

- **A group that loses every leg at once under IO is broken by md.** On a
  switch reload, or a reboot of all its disk nodes, every path of a group
  goes at once. md marks the array broken and refuses every write. An idle
  array whose legs all died keeps running until IO, a read or a write,
  reaches it. A one-leg group returns the errors to the
  host. A failed commit of the slice's thin pool leaves it read-only. When
  the broken group is a data group and the slice's meta group still takes
  writes, a commit under writes turns the thin pool read-only, and a new
  primary brings the array back but not a writable thin pool. When the
  broken group is the meta group, the group that holds the slice's thin
  pool metadata, a new primary brings the pool back writable as of its
  last commit. The blocks mapped since are gone with no error. State:
  accepted; section 6.1 has the way back. Not seen in the lab; read from
  the kernel's code.
- **A clone whose clone meta group is lost stops.** The clone meta group
  holds the dm-clone's metadata. With no cntlr able to assemble it, no dm-clone is built. The
  clone's namespaces stay inaccessible and host IO queues. State:
  accepted. Procedure to write: the forced-start guide covers the way
  back.

**An error is not a loss.** A write that fails with an error is not a data
loss in the sense of this document. The application knows. Two cases:

- **A pool runs out of space.** Auto-grow is best effort. A grow fails
  when it finds no disk node with room or no controller node with budget,
  or when the slice is at its group bound. Nothing reserves space ahead. A write that needs a
  new block waits for the kernel's no-space timeout and fails. Written
  blocks keep serving. Under the CSI driver the cluster can be
  over-committed. State: accepted. An alert for it is not yet.
- **Writes wait for the kernel's timers when a disk node dies.** A dead
  disk node is noticed by the keep-alive alone, about 6 to 10 seconds. md
  fails the member then, and the writes complete on the other leg. IO sent
  in the last second before that waits longer, for the fast IO fail
  timeout, the kernel's wait before it fails the IO queued on a lost path:
  5 seconds today, so 11 to 15 seconds in all. An fsync whose flush is in
  that IO waits as long. A one-leg group has no other leg: its writes and
  reads wait for the fast IO fail timeout and return the error, and a
  failed commit leaves its thin pool read-only. State: accepted. Fix decided: the fast IO fail timeout becomes a pool
  setting, fixed when the pool is created, with today's value as its
  default. Not seen in the lab; read from the kernel's code.

**What a completed write means.** A volume tells the host that it has a
volatile write cache, as many disks do, so the host sends flushes. A write
is durable once a later flush from the host, or a write the host marks to
reach stable storage before it completes (FUA), has completed on the
primary that took the write. Without a flush, a write to a block the pool
has not mapped yet, or to a block a snapshot shares, is committed about
once a second. A primary that dies before that commit loses the write, and
the block reads as it did before the write: zeros where nothing was
written. This is the behaviour of a disk with a volatile write cache, not
a loss in the sense of this document. A flush that completes although its
primary died is a defect; section 5.1 has it.

## 5. Data loss outside the accepted cases

No other data loss is accepted. These cases exist today. Each will be
fixed, unless its entry says otherwise. A case marked "silent" shows no
error to the host.

### 5.1 Hosts, flushes and snapshots

- **A host's fsync completes after the primary died with its writes.**
  Silent. A write to a new block completes before the pool commits its
  mapping. The pool commits on a flush or about once a second. The
  primary dies with uncommitted writes while the host's flush is in
  flight. The host's kernel then cancels the flush on the dead path and
  completes it as success: a flush carries no data, so the multipath code
  has nothing to retry. A later flush goes to the new primary, which
  commits nothing of the dead one's. The fsync returns success and the
  writes are gone. A plain PCIe disk never takes this path. dnv cannot fix
  this from the target side. State: fix later; a change inside dm-thin is
  under test. Read from the kernel's code; not seen in the lab.
- **A torn snapshot can read as whole.** Silent. A snapshot is taken slice
  by slice. A primary crash between two slices, or a slice whose step was
  skipped or failed, leaves a torn snapshot. The agent reports it torn, but holds the
  mark in memory until the snapshot is recorded as created. An agent
  restart, a failover or a cntlr replacement in between loses the mark.
  Delete and re-create a snapshot reported torn. State: fix decided, the
  agent saves the mark at once and the worker records it from the first
  report. Not seen in the lab.
- **A snapshot taken just before a clone into the same volume holds part
  of the copy.** Silent. A clone's create does not look for a snapshot of
  its destination that is not yet created. One slice of that snapshot can
  be made after the copy began. State: fix decided, the clone's create
  refuses such a destination. Not seen in the lab.

### 5.2 The forced start

A forced start is the way back when every leg of a group failed. Its edges
below are sharp. They are fixed together, as one plan, "forced start,
second pass", with the forced-start guide of section 7. The words of this
section:

- A repair is one run of leg repair on a group, by the worker or by an
  operator's call. A retired leg is a leg that left its group after a
  repair and keeps its extents, and its data, until it is deleted. A
  repair can take a retired leg back into the group.
- A member is a leg that md counts in the group's array. Its superblock is
  md's record at the start of the leg: the array's identity, md's change
  count, which md calls the event count, and the member's copy position,
  how far md has copied onto it. md also keeps a bad-block list, the
  sectors of a member it could not read.
- An operator asks for a forced start with a mark on the group, the
  forced-start mark, that names the leg. The primary acts on the mark.

The entries:

- **A repair copies over a retired leg from before a forced start.**
  Silent. After a forced start the array has a new md identity. A retired
  leg from before it can hold newer data. A repair can take that leg back
  and md copies the array over it. State: fix decided, the primary refuses
  a leg of another identity unless forced. Seen in the lab.
- **After a forced start, the aborted repair's bad leg is copied over.**
  Silent. The bad leg is the leg a repair replaces. The open repair's bad
  leg can hold the newest data. After the abort the primary adds it to the
  array with no check. State: fix decided, the same identity check. Seen
  in the lab.
- **A forced start cut short after its zeroing leaves legs that read as
  fresh.** Silent. The forced start zeroes the md superblock of every other
  kept member. A cancel, or a member that dies before its add, leaves
  every leg looking new. The primary then builds a new empty array over
  them. State: fix decided, a group that once had an array never gets a
  new empty one. Seen in the lab.
- **The worker deletes a retired leg that holds the newest copy.** Silent.
  The worker deletes a retired leg after seven days on a whole group. At
  the bound of two retired legs, a repair's finish deletes the oldest at
  any age. No setting keeps such a leg. State: fix decided, a keep flag on
  the retired leg. Not seen in the lab.
- **A forced-start mark over a running array zeroes members that may hold
  newer data.** Silent. Nothing compares the named leg with the members it
  zeroes. State: fix decided, the primary reads every member once, and
  refuses a named leg with a lower change count than a full member unless
  forced. Not seen in the lab.
- **A failed read of a member's superblock reads as "no superblock".**
  Silent. mdadm answers the same for an IO error and for a blank member. A
  leg with data is then taken as blank and copied over. State: fix
  decided, the agent reads the superblock area itself and refuses on a
  failed read. Not seen in the lab.
- **A half-copied leg looks whole, and a forced start keeps a bad-block
  list.** The host sees IO errors over regions a leg really holds. State:
  fix decided, the forced start reads the copy position and the bad-block
  list, refuses a half-copied leg, and drops the list. Seen in the lab.
- **A retired leg given back by a cancel may be partly overwritten.**
  Silent at a later forced start. State: fix decided, a "partial" flag on
  the retired leg. Not seen in the lab.
- **A zero of a member whose IO fails reads as done.** The member keeps its
  old identity, and a later assembly gets two identities. State: fix
  decided, the agent reads the area back after the zero. Not seen in the
  lab.
- **A forced-start mark cleared during a change of the pool's owner.**
  Silent. Two workers drive a pool for a short time at an owner change.
  The old owner can deliver a marked request after the new owner cleared
  the mark. State: accepted. A fix would make the agent remember a refused
  request, against the rule "never remember failed work". Not seen in the
  lab.

### 5.3 Transfers, clones and migrations

- **A second namespace on a live transfer's volume.** Silent. The gateway
  guards the namespace a transfer names, not the volume under it. Host
  writes through a second namespace change the source in the middle of the
  copy. State: fix decided, the gateway refuses it. Not seen in the lab.
- **A migration's old side opens its copy export before its writes have
  stopped.** Silent. Before the new side copies, the old side suspends the
  devices of the old primary's paths, so that no write is in flight, and
  then opens the export the new side copies from. A failed suspend is
  skipped, but the export is opened all the same. The new side can copy a
  region before a write in flight lands on the old side, and the two sides
  then differ. State: fix decided, the export waits for every suspend. Not
  seen in the lab.
- **A transfer of a volume a clone still fills gives a copy with holes.**
  Silent. The reader sees zeros where the copy has not reached. State: fix
  decided, the gateway refuses such a transfer. Not seen in the lab.

### 5.4 Hosts and the Kubernetes driver

- **A detach does not cut a connected host.** Silent. A detach takes the
  host off the subsystem's list. The kernel checks the list only at
  connect. A connected host keeps its path. Two hosts can write the same
  volume. State: fix decided. The fix needs a kernel debug option that
  most distributions leave off. Where the kernel cannot cut a connection,
  the cn agent starts only with a flag that accepts the limit, and the
  limit stays. Not seen in the lab.
- **The CSI node plugin formats a volume after a failed probe read.**
  Silent. The node plugin is the part of the CSI driver on each node. It
  takes an empty answer of the filesystem probe as a blank device. The
  probe answers the same after a failed read. State: fix decided, the
  plugin proves a device blank before it formats it. Not seen in the lab.

### 5.5 Records and tools

- **An etcd restore can hand a volume's thin ids out again.** Silent. A
  thin id is dm-thin's device number inside a pool. A restore sets a pool's
  next id back. A new volume can take the id of an old volume and serve its
  data. State: fix decided, the primary refuses to adopt an existing
  dm-thin device that holds data for a new volume. Procedure to write: the
  restore procedure (section 7 has the steps known today). Not seen in the
  lab.
- **A damaged copy of the disk node's own records can re-zero a serving
  leg.** Silent. A disk node keeps the records of its sides on its disk,
  in two copies written in turn, and loads the newest good one. A newest
  copy damaged after its write leaves the older one, one change behind. At
  a restart the agent can then zero again the part of a side it zeroed
  last, over the pool's data written there since. State:
  fix decided, the zeroing never writes a side that is provisioned. Not
  seen in the lab.
- **A failover step that fails before the fence leaves two writers on a
  side.** Silent. At a failover a disk node handles the new primary's path
  first. A failed step for the old primary before its fence leaves both
  primaries' paths live on the side, until the pool's next revision or an
  agent restart. State: fix decided, the fence comes first, before any
  other step of the failover. Not seen in the lab.
- **dnvctl parses an id with leading zeros as octal.** An id copied from a
  device name can name another object. A forced start or a delete can act
  on the wrong group or leg. State: fix decided, dnvctl refuses such an
  id. Not seen in the lab.
- **A change without a revision token overwrites another operator's
  change.** This loses a change of the desired state, not volume data. A
  caller may send the token and is then protected. State: accepted, by
  design. Procedure to write, one line in the operations doc.

## 6. The system comes back alone

After a fault is gone, dnv comes back without a human in the cases of the
table. The third column gives the wait where a setting controls it. The
settings are the pool's thresholds, in seconds, and the check interval.
Measured times are in [the introduction](introduction.md#status-and-limits-of-01).

| Fault | What dnv does by itself | The wait, where a setting controls it | By hand |
|---|---|---|---|
| A disk fails, one leg of a RAID1 group | md fails the leg. IO goes on over the other leg. Leg repair replaces the leg with a new leg on another disk node, and md copies the data onto it. The old leg becomes a retired leg. | `leg_unhealthy`, 1200 at the default, before the repair starts. | Nothing. In a pool without RAID the data is gone (accepted case 2). |
| A disk node is down and comes back | md fails its legs after the kernel's keep-alive. IO goes on over the other legs. Back in time, its legs rejoin and md copies the changed regions. Down for long, leg repair moves each leg to another disk node. | `side_unhealthy`, 600 at the default, before the repairs start. | Nothing. A node that never returns needs the node replacement procedure (section 7). |
| A controller node is down and comes back | Each primary on it is failed over to a standby. Hosts see a pause. Back, its cntlrs are standbys again. A cntlr unhealthy for long is replaced on another controller node. | `primary_unhealthy`, 4 at the default, plus two check rounds of `cntlr_interval`, 2 each at the default. `cntlr_unhealthy`, 600, before a replacement. | Nothing. |
| The network between a controller node and a disk node is cut | As for a disk node down, seen from that controller node: md fails the legs behind the cut and takes them back when the cut ends. A long cut moves the legs. | `leg_unhealthy`, 1200. | Nothing. |
| The network between hosts and a controller node is cut | Nothing. dnv reads the agents, never a host. The host's IO waits on the path. | None. | Disable the primary, so a failover runs at once. Enable it again after. State: procedure to write; fix later, a host health signal into the worker. |
| The network between the control plane and the agents is cut | The data plane keeps serving. The primaries of one cut-off controller node are failed over, and their hosts see a pause. With every agent cut off, nothing changes until the network is back. | As for a controller node down. | Nothing. |
| etcd is down and comes back | The data plane keeps serving. No call and no reaction runs. The workers resume when etcd is back. | None. | Nothing. State: procedure to write, "running while the control plane is down". |
| A worker, the gateway or the cdc is down and comes back | Another worker takes the pools after a bounded window. The gateway has no state. Without the cdc, hosts keep their paths and get no path changes. | None. | Nothing. A failover during a cdc outage leaves hosts on a withdrawn path (section 6.1). |
| An agent restarts | The agent rebuilds its desired state from its local store, the files in which it keeps the desired state it last accepted. The kernel keeps the devices. A controller node agent away for more than a few seconds has its primaries failed over. | `primary_unhealthy` plus two check rounds, about 4 to 6 seconds at the defaults. | Until the fix, stop the workers first for a planned restart of a controller node agent. State: fix decided, the agent answers "starting" while it restarts (section 6.1). |
| A host reboots | Nothing on dnv's side. nvme-stas or the Kubernetes node plugin connects the paths again. | None. | See the host actions in section 7. |
| All disk nodes of a pool are down at once, as in a switch reload | md marks each array broken under IO. Hosts see IO errors. The broken array makes the primary unhealthy, so the worker fails it over at the primary threshold, even while the nodes are still down. Once the legs are back, the new primary assembles the arrays again. A pool of one cntlr waits for a cntlr replacement at the cntlr threshold. | `primary_unhealthy` for the failover. `cntlr_unhealthy`, 600, for a replacement. | A forced start, when no failover comes. The blocks mapped since the last commit of a broken meta group are gone (accepted case 1). |

### 6.1 Cases where dnv does not come back alone today

- **A broken array is not re-assembled when its legs return.** The primary
  leaves a broken array as it is. The group stays down until a failover or
  a cntlr replacement builds it again on another cntlr, or an operator runs
  a forced start. State: fix decided, the primary stops the array and
  assembles it again only on an unambiguous reading: every kept member
  answers, with one identity and no half-copied member. Anything else
  waits for the operator. The operator-in-charge flag, an operator's mark
  on a group that keeps the worker's repairs off it, stops it too. A lab
  fact on md's behaviour comes first. Read from the kernel's code; not
  seen in the lab.
- **A read-only or corrupt thin pool has no repair path.** A failed commit
  leaves a slice's thin pool read-only. Corrupt thin pool metadata has no
  check and no repair flow in dnv. State: procedure to write, the repair
  by hand onto fresh space, with the level steps, the named devices and
  thin_check and thin_repair, with a test of the cn agent suite. Fix decided: the agent
  reports the pool's mode (section 9).
- **A group whose in-sync leg dies during a copy waits for a forced
  start.** A repair finishes once the primary reports the new leg in place,
  while md may still copy onto it. The old leg dies during the copy. md
  will not start from the half-copied leg. State: procedure to write, the
  forced-start guide. Fix later, the primary starts from the leg md
  recorded as last in sync. Not seen in the lab.
- **A member md keeps dropping is put back without end.** md drops a leg on
  any read error. The agent puts it back after every drop, and the leg row
  reads ok, so no repair acts. A leg with slow IO reads ok too, and every
  write of the group waits for it. State: fix decided, a scheduled md
  check. The worker asks the primary to run md's check on each group in
  turn, at an interval in days that is a cluster setting fixed when the
  cluster is created, 30 by default, zero for off. The check rewrites a
  sector one leg cannot read from the other leg and records a bad block
  on the leg that failed. One recorded bad block makes the leg unhealthy,
  so leg repair moves it. Slow legs: accepted; migrate the leg's side by
  hand. Not seen in the lab.
- **A fully copied clone goes out of service when its source is away at a
  rebuild.** A failover, a reboot or a level step rebuilds the dm-clone,
  and the build needs the source. State: fix decided, the worker drains a
  clone once the primary reports its copy whole, so the volume no longer
  needs the source. Not seen in the lab.
- **A side step that failed in the side's last syncup is never retried.**
  The side's converge answers "clean" after a passing failure, and the
  worker re-sends nothing. Seen in a rehearsal on AWS machines: a pool
  under build never built. State: fix decided, every answer of a side or a cntlr
  says "not clean" while anything the desired state wants there is absent
  or differs, so the worker sends the syncup again.
- **A metadata grow stopped halfway is never finished.** A grow of a
  slice's thin pool metadata takes two table loads. A pass stopped between
  them never runs the second. The thin pool keeps its old metadata size,
  and its row reads ok. State: fix decided. Not seen in the lab.
- **Orphans the sweep never removes.** An export of a side whose build
  stopped before the export named its device. A subsystem for hosts whose
  build stopped before dnv wrote its mark on it. A connection to a clone
  source outside this cluster, which reconnects for ever. A namespace in a
  subsystem whose name has dnv's prefix but names no dnv object. The sweep
  takes each for something that is not dnv's. Until the fix an operator
  removes them by hand. State: fix
  decided, the sweep learns to own them. Not seen in the lab.
- **A park whose load fails wedges the cntlr.** A park sets a namespace
  aside: its device is suspended and loaded with a table that serves
  nothing, so that the volume under it can be changed. When the load
  fails, the device stays suspended and host IO queues in it. State: fix
  decided, the next converge repeats the load or resumes the device. Not
  seen in the lab.
- **A refused side allocation never makes leg repair move the leg.** The
  leg's health goes on and off with each round. State: fix decided, the
  agent reports the refusal as an error too. Not seen in the lab.
- **A disk node that cannot serve a new leg gets a new repair after each
  cancel.** State: fix decided, the create skips a disk node whose side
  of the group reads error. Not seen in the lab.
- **A repair whose new leg the primary never reports stays open.** It
  blocks every other repair of the group. State: fix decided, the worker
  gives it up past the leg threshold. Not seen in the lab.
- **After a forced start, members of two identities keep the group down.**
  mdadm refuses an assembly handed two identities. State: fix decided, the
  primary assembles the members of one identity only. Seen in the lab.
- **The worker clears a forced-start mark with no start.** md moves a
  broken array between states while it retries a copy. A report from that
  window looks like a running array. State: fix decided, the primary
  reports the mark it acted on. Seen in the lab.
- **A forced start waits on a primary whose layers above hold the broken
  array.** Only a failover or a level step that stops the pools lets it
  run. State: procedure to write, one line in the forced-start guide.
- **An open repair, or the operator-in-charge flag, holds the worker's
  repair.** A group has one repair at a time. State: accepted, by design.
- **A pool with a lost record waits for an operator.** The worker sends
  the pool nothing. Removing the name from the pool's list makes the
  primary delete a volume's data for good. State: procedure to write, how
  to restore one record from the etcd backup; never remove the name.
- **A node deleted before its sweep keeps leftovers.** A node delete asks
  no agent. A reused disk reads as foreign until its header block is
  zeroed by hand. A dead disk node cannot be deleted while a side is on
  it. State: fix decided, the node delete asks the agent and refuses while
  leftovers stand unless forced. Procedure to write: node removal and disk
  reuse. Fix later: a forced node delete for a dead disk node. Not seen in
  the lab.
- **A port setting that differs is never re-driven.** State: fix decided,
  the agent answers "not clean" while the setting differs, so the worker
  sends the syncup again. Not seen in the lab.
- **A converge whose call was cut off leaves the rest waiting.** The
  unfinished work waits for the object's next revision. State: fix
  decided, for sides and cntlrs, by the same "not clean" answer. Not seen
  in the lab.
- **An agent restart during a fence can leave devices suspended.** A disk
  node's agent keeps the start of a fence's wait in memory. A restart
  during that wait can leave the old primary's devices on the side
  suspended, with no timer to end it. A syncup of the side, or a level
  step, ends it. Nothing in the wait can reach the data. State: accepted.
  Not seen in the lab.
- **Worker membership windows.** Two workers can drive the same pools for
  a few seconds after a partition heals. A watch etcd stopped serving is
  found only after a silence timeout. Nobody drives a pool for a bounded
  time at every owner change. Changes are delayed, never skipped. State:
  accepted, by design.
- **A fault only a host sees never fails over.** See the table. On a pool
  with a healthy enabled standby, disabling the primary starts a failover
  at once, and a disabled cntlr is never replaced, so enable it again
  after the failover. On a pool with no such standby, a pool of one cntlr
  among them, a disable starts no failover and takes the primary out of
  the cdc's answer, so a host that follows the discovery log drops its
  last path. State: procedure to write; fix later.
- **An nvme-stas host loses every path when its discovery daemon is
  away.** nvme-stas has two daemons. stacd reads a missing stafd as an
  empty discovery log. With the tutorial's setting, which lets stacd
  disconnect what it connected itself, it drops every path it made at its
  next start or reconfiguration: a reload, a removed controller, a device
  audit or one of its timers starts one. To restart stafd, stop stacd first, which keeps its
  connections when it stops; restart stafd; once stafd holds its
  connection to the cdc again, start stacd. State: procedure to write,
  the restart order.
- **A host keeps a withdrawn path.** A path connected by hand is never
  dropped. A cdc cut off from etcd keeps listing a failed-over primary. The
  Kubernetes node plugin keeps a cdc address an old attach named. Such a
  host writes to the fenced old primary and gets IO errors in the failover
  window. State: procedure to write for the first two; fix decided for the
  plugin, which takes the cdc list from its deployment on every pass.
- **A Kubernetes mount does not survive the loss of its subsystem's last
  connection.** The node plugin's connect keeps reconnecting through an
  outage of every path, however long, and the IO waits. The mount also
  survives a cntlr replacement on a pool of one cntlr while the old path
  lasts until the new one has joined, since the plugin keeps the old path
  until then. A pod blocked in that IO cannot be
  stopped until the operator runs `nvme disconnect -n` with the volume's
  subsystem NQN on the node. A reconnect refused with "do not retry" ends
  for good, as when a controller node comes back holding other volumes'
  cntlrs before the volume's. Once the subsystem's last connection is
  gone, the mount keeps the dead device, and a later connect makes a new
  device that the mount does not follow, until the pod is deleted and made
  again. Nothing reports a dead mount. State: procedure to write; no fix.
- **A cntlr replacement on a one-cntlr pool can cost the volume its path.**
  State: accepted; use two cntlrs per volume. Procedure to write, one line
  in the CSI tutorial.
- **A primary that never reports its stack built waits for the longer
  threshold.** It is failed over or replaced only after the longer of the
  primary and the cntlr thresholds, 600 seconds at the defaults. The pool
  stays on it that long. Disabling the primary ends it sooner. State:
  accepted, by design.
- **Restarting a controller node's agent fails over its primaries.** The
  agent answers no check round before its first converge ends. A restart
  longer than about 4 to 6 seconds at the defaults fails over every
  primary on the node. Until the fix, stop the workers first, as an
  upgrade does, or create pools with a larger primary threshold
  (`--thr-primary` of `dnvctl sp create`). State: fix decided, the agent
  answers "starting" until its first converge ends, and the worker holds
  its verdict on that node's cntlrs for a bounded time, a cluster setting
  fixed when the cluster is created, 120 seconds by default. Worked out
  from the defaults; not seen in the lab.
- **A corrupt cdc entry withdraws its subsystem from hosts.** A cdc entry
  is the record in etcd that the cdc serves for one subsystem. Only a hand
  edit or a bug corrupts it. State: accepted.

## 7. Maintenance actions

Every maintenance action should have a tool and a document. The table says
what exists today. "Tool" is a dnvctl command, a gateway call, or what the
worker does by itself. "Doc" names the document with the steps. The two
tutorials are [dnv on one machine](tutorial_all_in_one.md) and
[dnv under Kubernetes on one machine](tutorial_csi.md). "Operations doc"
names the document to write; it does not exist yet. Every row with no tool
or no doc carries its note. Where steps are known today and no document
holds them yet, the section "Steps known today" below gives them.

### Nodes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Add a disk node or a controller node | `dnvctl dn create`, `dnvctl cn create` | the tutorials | done |
| Prepare a node: mask md's own assembly on disk nodes, the udev rule for device mapper, the network and the firewall | none | the tutorials, in part | procedure to write, in the operations doc. Fix decided: a node check, a subcommand of `dnv-agent`, reports what a node lacks and how to fix it. |
| Disable a node | `dnvctl dn set-disabled`, `dnvctl cn set-disabled` | none | procedure to write, one line: when to disable |
| Remove a node | `dnvctl dn delete`, `dnvctl cn delete`. Refused while a side or a cntlr is on it. | the teardown of the tutorials | procedure to write: drain first, check the leftovers. Fix decided: the delete asks the agent. |
| Replace a dead disk node | repairs and migrations move its legs; `dnvctl retired-leg delete` frees the rest | none | procedure to write. Fix later: a forced node delete. |
| Replace a dead controller node | the worker replaces its cntlrs after `cntlr_unhealthy`; `dnvctl cntlr create` and `dnvctl cntlr delete` by hand | none | procedure to write |
| Reuse a disk from an old node | none: zero its header block by hand | none | procedure to write |
| See a node's live state and the cluster's capacity | `dnvctl dn inspect`, `dnvctl cn inspect`, `dnvctl cluster get-capacity` | none | procedure to write, one line. Fix decided: a capacity answer that also says what can still be placed and which limit the cluster reaches first. |
| Compare a disk node's own free count with its record | `dnvctl dn inspect` shows both | none | procedure to write, one line: the counts are compared, never reconciled |
| Gather a node's state for a bug report | none | none | fix decided: a gather subcommand of `dnv-agent` writes one archive |

### Pools

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create and delete a pool | `dnvctl sp create`, `dnvctl sp delete` | the tutorials | done |
| Grow a slice | the worker grows it by itself; `dnvctl sp grow-slice` | the introduction | done |
| Change a pool's level | `dnvctl sp set-level` | none | procedure to write, with the thin pool repair |
| Move a leg to another disk node | `dnvctl migr create`, `migr append-bm`, `migr finish`, `migr cancel` | the introduction | procedure to write: the whole flow. Fix decided: the worker reads and hands over the leg's list itself, and `migr append-bm` goes. |
| Repair a leg by hand | `dnvctl repair create`, `repair finish`, `repair cancel`, `repair abort`; the worker repairs by itself | none | procedure to write |
| Force-start a group | `dnvctl group force-start`, `dnvctl group set-operator-in-charge` | none | procedure to write: the forced-start guide. Fix decided: an inspect call that shows what the primary reads from each member, and a dry run that says what a start would do and refuse. |
| Delete a retired leg | `dnvctl retired-leg list`, `dnvctl retired-leg delete` | none | procedure to write, one line |
| Replace a cntlr by hand | `dnvctl cntlr set-enabled`, `dnvctl cntlr delete`, `dnvctl cntlr create` | none | procedure to write |
| Change a pool's cntlid slots, the ranges of NVMe controller ids its exports use | `dnvctl sp set-cntlid-slots` | none | procedure to write, one line |
| Change a pool's thresholds | none | — | not yet |

### Volumes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create, snapshot and delete a volume | `dnvctl td create`, with `--ori` for a snapshot; `dnvctl td delete` | the tutorials | done |
| Export a volume to hosts; change its hosts | `dnvctl ss create`, `dnvctl ns create`, `dnvctl ss set-hosts` | the tutorials | done. Fix decided: a removed host is cut. |
| Park a namespace | `dnvctl ns set-suspended`, `dnvctl ns set-dev` | none | procedure to write |
| Move a volume to another pool | `dnvctl xfer create` on the source; `dnvctl clone create`, `clone append-bm` on the destination | the introduction | procedure to write: the whole flow. Fix decided: the gateway guards the list read, and the worker drains a clone whose copy is whole. |
| Clone from a source outside this cluster | `dnvctl clone create`, `clone set-tr` | the introduction | procedure to write |
| Read a volume's written blocks | `dnvctl td get-bm`, `dnvctl td get-leg-bm` | none | procedure to write, with the move. Fix decided: `td get-leg-bm` goes, since the worker reads a leg's list itself. |
| Grow a volume; revert a volume to a snapshot | none | — | not yet |

### Control plane

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Back up and restore etcd | etcd's own tools; `dnvctl cluster advance-ids` after a restore | this page, below | procedure to write. Fix decided: a restored thin id that holds data is refused. |
| Upgrade | none; every program at once | this page, below | procedure to write. Rolling upgrades are not yet. Fix decided: every program gets a version flag. |
| Run while the control plane is down | nothing to do; the data plane serves | none | procedure to write |
| Handle the logs | none; JSON on stderr, no rotation | this page, below | procedure to write |
| See what is wrong across the cluster | none; the worker's log | none | fix decided: one gateway read, with its dnvctl command, lists what is wrong in every pool |
| Give hosts the cdc addresses; front the gateways | none | the tutorials, for one host | procedure to write |
| Change the check intervals or a node's budget | none | — | not yet |

### Hosts

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Connect a host with nvme-stas | nvme-stas | the all-in-one tutorial | done |
| Connect a Kubernetes cluster | the CSI driver | the CSI tutorial | done |
| Restart nvme-stas | none | this page, section 6.1 | procedure to write: stop stacd first, restart stafd, start stacd |
| Recover a host that keeps a withdrawn path | none | none | procedure to write. Fix decided for the node plugin. |
| Recover a Kubernetes mount after the loss of its last path | none; `nvme disconnect -n`, then delete the pod | this page, section 6.1 | procedure to write; no fix |
| Recover from a fault only a host sees | disable the primary | this page, section 6.1 | procedure to write. Fix later. |

### Steps known today

These steps are the only ones written so far. The operations doc will hold
the full procedures.

**Back up and restore etcd.** Back up etcd with its own backup command,
`etcdctl snapshot save`, on a schedule. dnv adds nothing to etcd's backup
or restore. A restore brings the desired state back to the backup's time,
so what was created after it is lost with its data, and a pool whose
layout changed since breaks. A restore needs dnv's steps around etcd's:

1. Stop every worker, cdc and gateway. The agents keep serving.
2. Restore etcd, and start it with the settings every etcd that serves dnv
   runs with.
3. Start the gateways. For every cluster, before any other call but
   `dnvctl cluster list`, which names the clusters, run
   `dnvctl cluster advance-ids`. Without this step a new object can take
   the ids, and with them the data on the nodes, of an object made after
   the backup. Even with it, a pool that created more than 1,048,576 thin
   devices, snapshots included, after the backup can hand one of their ids,
   with its data, to a new thin device.
4. Stop each agent, delete the files of its local store, the directory in
   which it keeps the desired state it last accepted, and start it again.
5. Start the workers and the cdc, let clients call the gateways again, and
   take a new backup.

The suites and the tutorials run etcd as one member, so an etcd of several
members is untested.

**Upgrade.** Upgrades are lockstep. Every dnv program of one etcd
installation stops: its gateways, workers and cdc, the agents of every
cluster it holds, and the controller plugin and the node plugins of every
CSI driver that talks to its gateways: its controller plugin, the part
that calls the gateway, and its node plugins. A stopped node plugin leaves the
connections and mounts of its volumes in place. Every binary is replaced,
`dnvctl` included, and only then does any program start again. The workers
stop first and start last. There is no rolling upgrade, since no message
carries a build version, and two builds that differ in a message would
misread each other with no error.

**Logs.** The programs log to stderr as JSON, one record per line, at one
level, with no level flag and no rotation. Keep the logs on a filesystem
apart from the agents' local stores and etcd's data, and rotate them
yourself: a disk the logs fill is one the stores and etcd cannot write.

### Notes on the maintenance tools

- **md's behaviour is pinned in the lab only.** Failover safety and the
  forced start rest on mdadm's behaviour, tested on the lab's mdadm.
  Nothing checks the mdadm of a deployed node. State: fix decided, the
  agent logs the mdadm version when it starts, the requirements name the
  tested version, and the node check reports a version outside the tested
  range.
- **Retired legs keep their extents until deleted.** A group never back to
  whole keeps them for good. At the bound of two, an operator's repair
  with a fresh leg is refused. State: procedure to write, one line.
- **A forced clone delete returns before the teardown is done.** Success
  means the teardown started. The operator polls the clone and the pool.
  State: accepted, by the dnvctl design. Procedure to write, one line.

## 8. Waits a host can see

- **A pool commit in a migration gap waits for the gap to end.** The gap is
  the time with no usable path from the primary to the moving leg. The gap
  ends by the disk nodes' steps or by a cancel. Measured at 32 slices: one
  to two and a half seconds. State: accepted.
- **A migrating leg whose serving side's disk node dies waits without
  limit.** A migrating leg has two paths. The one not serving is live but
  inaccessible. The kernel keeps the leg's IO queued as long as that path
  stays live. md never fails the member. A pool without RAID waits whole.
  State: fix decided; a lab test first, then the reaction is chosen. Read
  from the kernel's code; not seen in the lab.
- **The failover pause is the full stack build on the new primary.** A
  standby holds no array and no thin pool. The write in flight waits until
  the new primary has built the whole stack. The figures are in the
  introduction. State: accepted. Fix later, large: a standby that
  pre-assembles its arrays.
- **Hosts still on the old primary's path see IO errors from the fence.**
  The worker hands the old primary its demotion first and holds the sides
  for three seconds at most. A host that follows the discovery log drops
  the path inside that hold. Three windows remain. A slow old primary
  misses the hold: state accepted. The worker's coordinator, the part of
  the worker that drives one pool, runs no hold after a restart during the
  failover: state fix decided, the demotion is written into the cntlr's
  record. A cntlr replacement of a primary that still serves runs no hold:
  state fix decided, the replacement waits for the hold first. What stays:
  a Kubernetes host on an unreachable old node sees errors until the new
  path shows the volume. Not seen in the lab.
- **One slow or stuck command holds the agent's other work.** A command in
  an uninterruptible kernel wait outlives every timeout. The agent's
  reports age, and the worker can count a missed round. State: accepted;
  the waits are the kernel's. Not seen in the lab.
- **The retry for a leg that joins late holds the primary's reads off
  and can fail it over.** A leg that is not available when the primary
  builds its group makes the agent run the cntlr's whole converge again
  every retry interval. Each attempt holds the lock, so no
  read runs and the report ages. Two rounds answering from too old a
  report fail the primary over. State: fix decided, the agent reads the
  node once per interval on its own clock, reads and converges use the
  latest reading, and this retry goes. Seen in the lab.
- **A migration destination waits for a run of free metadata units the
  count allowed.** The gateway counts free units, not runs. The
  destination re-runs its converge every interval. The source keeps
  serving, and a cancel frees the units. State: accepted. Counting runs
  would need a map of the units in etcd that every allocation and release
  keeps. Not seen in the lab.
- **Smaller bounded waits.** An agent that marks the regions of a piece
  of a written-block list holds that object's other work until the marks
  are done. A grow that is stuck waits while another of the worker's waits
  stands on the group. The hold delays the cntlrs' syncups by up to one
  check interval. A repair waits while its array does not run. A clone
  source the agent reads as unused can be cut under a new dm-clone until
  the next converge. The read-only level reaches a controller node only
  with its converge. The cdc answers nothing before its first etcd scan,
  delays hosts under a flood of new connections, and serves old addresses
  through an etcd outage. State: accepted; each ends on its own.
- **Disabling the last enabled cntlr stops the pool's IO with no
  warning.** State: accepted, by the dnvctl design.

## 9. What cannot be seen

- **No metrics, no alerts.** The worker's log is the only alert. The logs
  go to stderr as JSON with no rotation. An auto-grow refused for budget, a
  cdc coverage gap, a frozen worker watch, a slow leg and a stuck repair
  show in a log line or not at all. The worker's log records the
  reactions it applied and the ones it skipped. No dnv program serves
  metrics, profiles or health over a port, though the CSI driver's
  manifests add a helper container that answers Kubernetes' health checks
  on port 9808 or 9809. State: not yet; log
  handling goes into the operations doc. Fix decided: one gateway read,
  with its dnvctl command, lists what is wrong across the cluster.
- **A read-only thin pool shows in no report.** dm-thin turns a pool
  read-only on a failed commit. The worker reads only the usage numbers.
  State: fix decided, the agent reports the pool's mode. A pool that
  dm-thin marks as needing a check, or a failed pool, is an error row of
  a fault a failover cannot cure: a new primary would meet the same pool,
  so the worker logs it and starts no failover. A pool that is read-only
  because its metadata is full stays ok, and the auto-grow of the metadata
  is its cure. Not seen in the lab.
- **A repair that waits gives no reason.** State: fix decided, the primary
  reports the reason and the worker logs it. Seen in the lab.
- **A retired leg's record does not say whether it is a whole copy.**
  State: fix decided, a keep flag and a partial flag on the retired leg,
  with the plan "forced start, second pass".
- **A disk node's full store disk is offered until a save fails.** A quiet
  node with a full store disk reads ok and is offered for new sides. The
  first refused request marks it. State: accepted; a small window. Not
  seen in the lab.
- **Log records that say less than they seem.** A create while the pool
  changes fast answers ABORTED with no word on why. A cdc write timeout and a vanished
  host log the same reason. State: accepted. Procedure to write: one
  paragraph on reading the logs.

## 10. Sizes and performance

The enforced sizes and the measured figures are in
[the introduction](introduction.md#status-and-limits-of-01). The limits of
design:

- **One primary per pool bounds a volume's throughput.** Every volume of a
  pool is served by its one primary, and slices split the work inside that
  node, never across nodes. To scale out, spread the pools across
  controller nodes. State: accepted; more than one primary is not yet.
- **No throughput or latency figures are published.** Only the failover
  pause is measured, on the lab VMs. State: accepted; the figures come
  from a performance run on AWS.
- **Every change of a pool fans the whole desired state out.** A volume
  create costs two check rounds. State: accepted.
- **Counts grow messages, connections and kernel objects.** A row's text
  from a failed command, the answer of a written-block list read and a
  migration's piece have no cap; a piece above etcd's request size fails. State: accepted. Fix decided: a
  cap on a row's text, and the worker cuts a migration's pieces to a fixed
  size.
- **The controller nodes' budgets cap a cluster's space.** A budget is the
  pool space a controller node may serve to hosts. Every cntlr of a pool,
  primary or standby, charges its controller node the full size of the
  pool's groups. So the sum of the budgets divided by the cntlr count of
  the pools is the most group space the cluster holds, whatever its disks
  hold. The budget is read once, at the node's create, from the
  `--capacity` its cn agent was started with; unset, it takes a default.
  No call changes it, so set `--capacity` on every cn agent before its
  node's create. An auto-grow refused for want of budget shows only in
  the worker's log. State: accepted. Fix decided: a node created without
  a budget gets the largest one.
- **The default cap on IO queues is unmeasured. The default stripe gives
  hosts large IO sizes.** Each path holds one TCP connection per IO queue,
  and the agent caps the queues of a subsystem; no host with many CPUs has
  been measured for it. The stripe tells the host the device's best IO
  size, and the host reads ahead and sends commands up to the stripe times
  the slice count. State: accepted; a measurement item of the performance
  plan. Procedure to write, one line on the stripe setting.
- **A CSI volume costs a pool.** Under the CSI driver every volume is a
  pool with its own cntlrs and sides, so the ceilings of the introduction's
  table bound the volume count. A controller node holds at most 256
  cntlrs, so with the default two cntlrs per volume a cluster holds at
  most 128 volumes per controller node it has. A RAID1 volume takes four
  sides at create, two more for every group auto-grow adds, and one more
  for every leg a repair adds, since the leg it replaces keeps its side
  for a time. A volume takes whole extents, one for its meta group and at
  least one for its first data group on every leg, however small the
  volume. State: accepted, by design.
- **A RAID1 pool needs four disk nodes per slice at create.** The create
  puts every leg of the pool on a disk node of its own, and each slice has
  a meta group and a data group of two legs each. A cluster with fewer
  disk nodes is refused with RESOURCE_EXHAUSTED. A group added later may
  share disk nodes with the pool's other groups. State: accepted, by
  design.
- **Nothing limits recovery load.** A dead disk node starts a full copy in
  every group it held, and a dead controller node fails over every primary
  it held, all at about the same time. The agents set no copy speed on md. Only the
  kernel's md speed settings bound the copies: the sysctls
  `dev.raid.speed_limit_min` and `dev.raid.speed_limit_max`, which an
  array's own `md/sync_speed_min` and `md/sync_speed_max` override until
  the array stops. State: accepted. Fix later, after the performance runs:
  per-pool copy speed settings.
- **A host discard frees space only over whole aligned blocks on every
  slice.** A thin pool frees whole blocks only, and completes a shorter or
  misaligned discard with success. A cluster created with no stripe gets
  one equal to its block, up to 1 MiB, so at the defaults an aligned 1 MiB
  discard frees one block. When a pool's stripe is smaller than its block,
  a discard frees a block on one slice only when it covers nearly the
  slice count times the block, and `fstrim` can report success and free
  nothing. Read from the kernel's code, not measured. State: accepted.
  Procedure to write, one line on the stripe setting.
- **One cntlid slot serves most subsystems and can run out.** When its
  controller ids run out, the kernel's target refuses connects with "do
  not retry". State:
  fix later, after the performance runs: spread new sides and cntlrs over
  the slot list.
- **A pool's thin ids run out; a slice's data space is capped at
  creation.** State: accepted, by design.
- **An md check counts mismatches on never-written blocks.** The count
  means nothing. State: accepted. Procedure to write, one line.
- **A retired leg taken back is copied whole unless the array stayed
  degraded.** State: accepted.
- **Steady control-plane cost.** The reaction pass reads the whole pool
  from etcd every interval. Every check round logs two records per object.
  State: accepted. Fix decided: one subsystem listing per sweep pass.
- **The gRPC message bound and the cap on an agent's commands.** Every
  dnv connection caps the size of one message, and every program caps how
  many commands and file operations it runs at once. State: accepted, by
  design.
- **CSI: a volume name with ".." leaves a pool with no volume id.** State:
  fix decided, refuse such a name before any create.
- **xfs volumes need kernel 5.19 or newer on the node.** State: accepted;
  a requirement. Fix decided: the node plugin formats xfs for a stated
  minimum node kernel.
- **Build time at the widest slice count.** A primary's build at 32 slices
  is bounded by the time to start its commands. Short thresholds then
  start failover after failover. A
  connect that reaches a grown group's side before its export exists
  leaves the group unbuilt until a later converge, which can outlast the
  primary threshold. State: accepted now; checked again once the agent
  reads the node once per interval (section 8).
- **A create while the pool changes fast gives up after a fixed number of
  rounds.** The caller retries. State: accepted.

## 11. How this document is kept

- The LLM reads in this order: the core glossary, this document, then the
  design documents. This document owns the policy: the
  promise, the accepted loss, the states of the limits. A design document
  keeps the mechanism of a limit in its own section of known limits.
- A change that fixes a limit, adds one, or moves one between the states
  edits this document in the same change. A limit found during the work
  is written in this document and in the design document's known limits.
  A review checks it. No lint checks it today.
- This document names no release. The introduction says what a release
  holds and gives its figures.
