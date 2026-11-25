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
- **not yet**: dnv does not have it and may get it later. Nothing is
  planned for it today.

An entry leaves this document once its fix is done and tested.

Some limits were read from dnv's code or from the kernel's code. No test
has shown them yet. Such an entry carries the mark "not seen in the lab".
Such a limit is named all the same, so that the reader knows of it.

Sizes, measured times and the table of enforced sizes stay in
[the introduction](introduction.md#status-and-limits). This document
links to them. The limits of the test suites are not in this document.

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
Kubernetes through CSI, the Container Storage Interface, the API through
which Kubernetes creates and mounts volumes. It makes one storage pool for
each Kubernetes volume and connects the volume on the node where a pod,
the Kubernetes unit of one or more containers, uses it.

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
| Redundancy other than RAID1 | for good |
| RAID1 with three legs | not yet |
| Platforms other than Linux x64 | for good |
| Metrics, health endpoints of dnv's own programs, and alerts | not yet |
| Growing a volume | not yet; Cinder and Kubernetes ask for it |
| Reverting a volume to a snapshot | not yet; Cinder asks for it |
| Changing a pool's thresholds or the cluster's check intervals after creation | not yet |
| Changing a controller node's budget, the pool space it may serve, after creation | not yet |
| Changing a node's location after creation | not yet; empty the node, delete it and create it again ([the operations doc](operations.md#remove-a-node)), and use a disk node's disk again as [the operations doc](operations.md#reuse-a-disk-from-an-old-node) says |
| Rolling upgrades | not yet; every program is upgraded at once |
| Transports other than NVMe/TCP | not yet |
| Disks of logical blocks other than 512 bytes, such as 4 KiB disks | not yet; the dn agent refuses such a disk ([the introduction](introduction.md#requirements)) |
| More than one primary per pool | not yet |
| cdc: host registration and mDNS discovery | not yet |
| A login to an etcd that asks for a password or a client certificate | not yet |
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
through a volume. A suite shows this on a real kernel: 4 KiB written into
blocks never written, over disks that held old bytes, read back as zeros
around the write, on the host and on both legs. The RAID1 metadata of a new leg is zeroed before use, so
md never trusts an old superblock.

**What the promise does not cover.**

- **The network and root are trusted.** dnv checks no user. A host is
  known by its NQN alone, and a host can claim any NQN. Root on a host
  can claim the NQN of another host and read and write that host's
  volumes. The promise does not cover that, so keeping tenants apart also
  needs hosts on which no tenant has root. All traffic is plain text. Root
  on a node, and anyone who reaches a disk node's NVMe/TCP port, can read
  the old bytes of a recycled extent from a raw disk or from a leg. Anyone who reaches an agent's port can send it a
  desired state. dnv's programs log in to etcd with no password and no
  certificate, so etcd must accept such clients and stay on the trusted
  network. Deploy dnv on an isolated network you trust, where hosts reach
  only the controller nodes' NVMe/TCP ports and the cdc. State: accepted.
  Procedure written: [the operations doc](operations.md#the-networks),
  the document of procedures an operator follows by hand, says who
  connects to whom and gives a firewall example,
  `deploy/nftables/dnv-filter.nft`. The e2e suite at one slice passed in
  the lab on 2026-11-22 with the example loaded on every node and on the
  control plane, and so did every case of the Kubernetes suite except its
  policy case, each in a run of its own. In the e2e suite's passing run,
  each of its resets of a node guest took that guest's firewall table
  away, as any reboot does, until a loop that ran beside the suite loaded
  it again, within a minute of the guest's return. The example's drop
  counters, read after each run, showed no dropped packet; a firewall
  table loaded again counted drops only from its new load.
  For the Kubernetes suite the gateway's port was left open to the hosts,
  since one copy of the controller plugin, the part of the Kubernetes
  driver that calls the gateway, always runs on a host in the lab.
- **In Kubernetes every pod reaches dnv's ports.** Every pod reaches the
  gateway, the agents, etcd, the cdc and the NVMe/TCP ports through its
  machine's network. Only the operator's firewall rule blocks it, or a network
  policy, a Kubernetes record that limits the connections of the pods of one
  Kubernetes namespace, that the cluster's network software enforces. The
  trust dnv puts in the network is then trust in every pod. The CSI driver
  ships an example policy,
  [`deploy/networkpolicy.yaml`](../../deploy/networkpolicy.yaml), that keeps
  the pods of a Kubernetes namespace off every address of the dnv machines; a
  Kubernetes namespace without it, or a pod on its machine's own network,
  still reaches them. State: procedure written, [the CSI
  tutorial](tutorial_csi.md#keep-other-pods-off-dnv).

**Steps of a caller that can break the promise.** A migration and a clone
can skip the blocks a volume never wrote. For a migration the worker
reads the leg's list of written blocks itself, once the old side takes no
more writes, and hands it to the new side in pieces of a fixed size. For a
clone the caller reads the source volume's list and hands it over in
pieces; the gateway answers the read only while every namespace on the
volume that etcd holds is set aside on the primary, that is, ANA
inaccessible with its device serving no IO (section 5.3 has a namespace
that etcd no longer holds), and only in pieces that each cover a multiple of
eight blocks but the last, so that they join end to end; and dnvctl
always sends a piece's position.

- **A host a transfer lets in can write the source while a clone copies
  it.** A transfer exports the source volume to the hosts you list, meant
  to be the other pool's controller nodes, which only read it. The gateway
  checks that every namespace on the volume is set aside, but not the
  transfer's own export. A host you list there by mistake can write the
  volume after its list was read. The clone then reads zeros where that
  host wrote a block the list marked never written, and the older bytes
  where it wrote a region the copy had already taken, with no error. State: accepted, because the gateway
  cannot tell a reading controller node's connection from a writing
  host's. Procedure written: [the operations
  doc](operations.md#move-a-volume-to-another-pool) lists only the other
  pool's controller nodes, by host NQNs it shows how to work out.
- **A clone's list piece sent twice without its position is stored
  twice.** dnvctl always sends a piece's position, so this needs a program
  that calls the gateway itself and leaves the position out. A clone's
  list is cut into parts at fixed places, and each part goes in pieces;
  every later block of that part then moves, and the clone reads zeros
  where the source wrote, with no error. State: accepted. The position is optional in
  the call so that older callers keep working.

- **A forced finish of a migration before the copy is whole.** The finish
  with force skips the proof that the copy is whole, and the old side is
  removed. The regions the copy
  had not reached read old bytes of the new side's extents, possibly
  another pool's, with no error. State: accepted. The operator asks for
  it with force. Procedure written,
  [the operations doc](operations.md#ending-a-migration-with-force): force
  is right when the new side's agent alone cannot answer the proof of a
  copy seen whole, and, in a RAID1 group whose other leg md holds in sync,
  to end the migration toward a disk node that is gone for good, so that a
  repair replaces the leg; not tested as a whole.
- **A forced migration cancel after the cutover.** A cancel after the
  cutover is refused unless forced, as the new side may hold the only copy
  of the leg's new writes. With force the old side serves its content as
  of the cutover: the leg loses every write since, and blocks first mapped
  since read old bytes of the old side's extents, possibly another pool's.
  In a group of more than one leg the other legs hold those writes, but md
  does not know this leg is behind and counts it whole, so reads can
  return its old bytes. No call makes md copy the leg again: a repair is
  refused for a leg with no health epoch, and a member that md failed when
  the leg went back to its old side is added back from md's record of the
  regions written while it was out, which holds none of these. State: fix
  later, a way to have md copy the leg again after a forced cancel. Until
  then this loss comes only from a cancel that the operator forces. The
  call also returns before the primary stops using the new side: until the
  primary has taken the cancel, and dropped its path to the new side with
  it, a write can still land on the new side and is then lost with it, in
  a pool without RAID with no error; this part is read from dnv's code and
  the kernel's, not seen in the lab. Procedure written,
  [the operations doc](operations.md#ending-a-migration-with-force): a
  forced cancel loses nothing when nothing writes to the pool from the
  cutover until the primary has taken the cancel, which the operations doc
  says how to see, and after writes its one safe use is in a RAID1 group whose
  other leg md holds in sync, when the old side's disk node is gone for
  good and the copy is not whole, so that a repair replaces the leg. A copy
  that is whole is finished with no force instead, since the gateway asks
  only the new side's disk node for the proof.
- **A forced start from an older copy restores one group, not the pool.**
  A forced start is an operator's start of a group's RAID1 array from one
  leg they name, when md will not start it by itself. In a data group,
  blocks rewritten since the leg left read old bytes, and blocks first
  mapped since then read what the leg holds there, possibly another pool's
  bytes. No error is shown. In a slice's meta group, the thin pool's
  records go back to that time over data groups that kept their newer
  writes. A block first mapped since reads zeros. A block that a volume or
  its snapshot rewrote since, while the two shared it, reads its older
  bytes. A block freed and given out again since can read another
  volume's bytes. None of these reads shows an error. A volume or a
  snapshot made since is lost, which the primary reports as an error. A
  start from a half-copied leg that md marked in sync serves, over the
  regions md never copied, what that leg holds there (section 5.2). State:
  accepted. The operator asks for it. Procedure written:
  [the forced-start guide](operations.md#the-forced-start-guide) says which
  leg to choose and what an older copy costs.
- **Two pools with a subsystem of the same NQN can serve each other's
  volumes.** The gateway keeps a subsystem's NQN unique inside one pool
  only, and the move of a volume to another pool asks for the same NQN in
  both pools. A controller node's kernel has one subsystem for each NQN,
  and nothing keeps a cntlr of each pool off one controller node: a pool
  create, a cntlr an operator creates and the worker's cntlr replacement
  can each put them there. Each cntlr then writes its own values into the
  one kernel subsystem: its serial number, its list of hosts and the
  device under each namespace. A subsystem's serial number comes from its
  id in its pool, and every pool counts its ids from the same start, so two
  pools can give a subsystem the same serial number, as two pools of one
  shape do with nothing else made first; the two pools of a move usually
  differ. With the same serial number every write of the second cntlr goes
  through: a host connected under the first pool's list keeps its
  connection, where the controller node ends no connections, and reads and
  writes the second pool's volume, with no error when both pools give the
  namespace the same identity, as a move does. With different serial
  numbers, once a host has read the subsystem, the kernel refuses the
  second cntlr's serial number, and neither pool serves steadily on that
  node. State: fix decided. Until then, keep the cntlrs of two such pools
  on different controller nodes: [the operations
  doc](operations.md#move-a-volume-to-another-pool) checks it before a move
  and during the copy. Read from dnv's code; not seen in the lab.

## 4. Data loss dnv accepts

dnv accepts data loss in four cases, and where an operator asks for it,
as with the forced finish and the forced start of section 3. Any other
data loss is a defect, unless section 3 or 5 marks it accepted and gives
the reason. Section 5 lists the data-loss defects known today.

1. **Every leg of a group is lost for good.** In a RAID1 pool no disk node
   holds two legs of one group, and a new group never has two legs in one
   location. A node's location is the host name of its machine unless the
   operator names another, so the legs of a new group sit on different
   machines when every machine has a host name of its own. That does not
   hold for a disk node that the first release, 0.1, created with no
   location, which keeps its address as its location; for the disk nodes
   of one machine created before and after a rename of the machine; or for
   an agent in a container with a host name of its own, which reports the
   container's name. In each case two disk nodes of one machine can hold
   both legs of a new group
   ([the operations doc](operations.md#choose-a-nodes-location)). Losing
   fewer disk nodes than a group has legs never loses a group. Losing
   every leg of one group loses that slice's thin pool. Every volume is
   striped over all slices, so the whole pool's data is lost. A repair
   finished between two losses restores the redundancy. So a RAID1 pool
   survives the loss of any one disk node, but not every loss of two. The
   pairs of disk nodes that can cause it are at most the pool's group
   count. For two random failures among N disk nodes the chance is at most
   that count divided by N(N-1)/2.
2. **A pool without RAID loses a disk or its disk node.** A group has one
   leg there. The data of the pool is gone. No repair exists for such a
   leg. A migration can move a side that still reads but is sick. An
   operator must start that migration. No migration moves a side of a clone
   meta group, the group of the pool made for one clone, nor a side of a
   pool whose data block is above 4 MiB, which only a pool made before dnv
   bounded the data block at 4 MiB can have (section 10): such a pool's
   volumes move to a new pool instead. In a RAID1 pool a repair replaces
   such a leg; in a pool without RAID nothing can move it today. State: fix
   later, small.
3. **A clone loses its whole source pool or its whole destination pool
   before the copy is complete.** With the source gone, the copy stops.
   A host's read of a block not yet copied waits or fails. With the
   destination gone, every write since the host moved over is lost.
4. **A migration loses a side.** Before the cutover, the old side's failure
   loses the leg, and the new side's failure loses nothing. After the
   cutover and before the copy is whole, either side's failure loses the
   leg. After the copy is whole, only the new side matters. RAID1 then
   decides whether the pool loses data.

Three more things belong with case 1:

- **With too few machines, a repair or a migration can put two legs of a
  group on one machine.** The new leg of a repair and the new side of a
  migration keep out of the locations the group holds only while another
  location has a disk node with room. When none has, they go to a disk node
  in a location the group already holds, and that machine's loss then takes
  both legs. A new cntlr can share a machine with another cntlr of its pool
  the same way. State: accepted; have more machines with free space than a
  group has legs. Read from dnv's code; not seen in the lab.
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
  last commit. The blocks mapped since are gone with no error. The loss
  need not last long: a network cut just past the paths' keep-alive
  timeout is enough, since md writes its own records to the legs in a way
  that fails at once when a path is torn down. State: accepted; section
  6.1 has the way back. Seen in the lab for a data group whose meta group
  stayed, with the times in
  [the introduction](introduction.md#status-and-limits): a short cut
  only paused the host's writes, a longer one left the thin pool
  read-only on its primary and after a failover, and no write the host was
  told was done was lost. In the end-to-end suite, with the worker
  running, a cut that lasted 3.06 s paused the host's writes for 3.1 s
  and failed none. One that lasted 8.05 s failed the host's write in
  flight and left the thin pool read-only; the worker then moved the
  primary role to the standby on its own, which brought the array back
  but not a writable thin pool. Each length is of one run. In two runs
  that timed the cuts less closely, the shorter cut failed no write either
  and the longer one left the thin pool read-only too. The meta group's
  case is read from the kernel's code. When the paths of a group die a few
  seconds apart instead of at once, a write the host was told was done can
  be lost (section 5.1).
- **A clone whose clone meta group is lost stops.** The clone meta group
  holds the dm-clone's metadata. With no cntlr able to assemble it, no dm-clone is built. The
  clone's namespaces stay inaccessible and host IO queues. State:
  accepted. Procedure written: [the forced-start guide](operations.md#the-forced-start-guide), "A lost clone meta
  group"; not tested.

**An error is not a loss.** A write that fails with an error is not a data
loss in the sense of this document. The application knows. Two cases:

- **A pool runs out of space.** Auto-grow is best effort. A grow fails
  when it finds no disk node with room, when one of the pool's controller
  nodes has no budget left for the new group, or when the slice already
  holds the most data groups it can. Nothing reserves space ahead. A write
  that needs a new block waits for the kernel's no-space timeout and fails. Written
  blocks keep serving. Under the CSI driver the cluster can be
  over-committed. `dnvctl cluster get-capacity` says what the cluster can
  still give pools and which limit it reaches first ([the operations
  doc](operations.md#the-clusters-capacity)), and `dnvctl sp inspect` how
  full each thin pool of a pool is and how far its volumes overcommit it
  ([the operations doc](operations.md#a-pools-space)). State: accepted. An
  alert for it is not yet.
- **Writes wait for the kernel's timers when a disk node dies.** Over
  NVMe/TCP a dead disk node is noticed by the keep-alive alone. In the lab
  the controller node dropped its connections to the dead node 6 to 10
  seconds after the node went silent. md fails the member then, and the
  writes complete on the other leg. A write in flight when the node died
  waited 9 to 10 seconds in all, and no write failed. A flush md sent to
  that leg ends then too, with no error. IO that reaches the lost leg
  after the keep-alive gives up waits longer, for the fast IO fail
  timeout, the kernel's wait before it fails the IO queued on a lost path.
  That timeout is a pool setting, chosen when the pool is created: 1 to 5
  seconds, 5 by default. A shorter one shortens only this wait, not the
  wait of the write in flight. An fsync whose flush is in that IO waits as
  long. A one-leg group has no other leg: its writes and reads wait for
  the fast IO fail timeout too and return the error, and a failed commit
  leaves its thin pool read-only. The measured times are in [the
  introduction](introduction.md#status-and-limits). State: accepted.

**What a completed write means.** A volume tells the host, on the path to
its primary, that it has a volatile write cache, as many disks do, so the
host sends flushes. A write
is durable once a later flush from the host, or a write the host marks to
reach stable storage before it completes (FUA), has completed on the
primary that took the write. Without a flush, a write to a block the pool
has not mapped yet, or to a block a snapshot shares, is committed about
once a second. A primary that dies before that commit loses the write, and
the block reads as it did before the write: zeros where nothing was
written. This is the behaviour of a disk with a volatile write cache, not
a loss in the sense of this document. A flush that completes although its
primary died is a defect; section 5.1 has it. So is a flush the host never
sends because it still reads a volume's path as having no cache, as after a
failover, and a flush the primary never sends on to the disk nodes for the
same reason (section 5.1). So is a durable write lost in a failover while the old primary
still serves, but for the cases section 5.1 lists and accepts.

## 5. Data loss outside the accepted cases

These cases exist today. Each will be fixed, unless its entry says
otherwise and gives its reason. A case marked "silent" shows no error to
the host.

### 5.1 Hosts, flushes and snapshots

- **A host's fsync completes after the primary died with its writes.**
  Silent. A write to a new block completes before the pool commits its
  mapping. The pool commits on a flush or about once a second. The
  primary dies with uncommitted writes while the host's flush is in
  flight. The host's kernel then cancels the flush on the dead path and
  completes it as success: a flush carries no data, so the multipath code
  has nothing to retry. A later flush goes to the new primary, which
  commits nothing of the dead one's. The fsync returns success and the
  writes are gone. A plain PCIe disk never takes this path. dnv's own
  programs cannot fix this. State: fix later; it waits for a kernel whose
  thin pool has an option that holds a write back until the pool's commit,
  which dnv will use once kernels carry it. Until then an fsync across the
  death of a primary is no guarantee. Read from the kernel's code, and seen
  in the lab once. The
  end-to-end suite resets the primary's machine while a host writes 4 KiB
  to new blocks from four jobs, each write followed by an fsync, and reads
  every block back after the failover. It has run this four times at one
  slice. Two of the runs slowed the pool's commits so that the reset could
  land in an fsync. In the first of the two the reset landed in a write in
  each of the four jobs. In the second each of the four jobs was in its
  fsync at the reset: each fsync returned success about 4.2 seconds after
  it was sent, before the failover to the standby, and none of the four
  writes read back from the new primary. That is this loss. In every run,
  every write whose fsync had completed before the reset, any write in
  flight at the reset and every write after it read back, and no write or
  fsync failed. The suite logs the writes whose fsync was in flight and
  does not fail on them.
- **A host's flush through a path it last read while its volume served
  only errors is not sent.** Silent. A volume tells the host, path by
  path, whether it has a volatile write cache. A volume served only by
  errors has none. These are: a standby's volume; a volume of a disabled
  cntlr; a volume of a new pool whose sides are still zeroing; a volume
  whose namespace an operator suspended or that waits for its clone's
  copy; a clone's destination volume while its pool's level builds no
  clone; and every volume of a pool whose level builds no thin pool. The
  host reads this when it connects. It reads it again when its connection
  to the controller comes back after a loss or a reset, and when it
  rescans the controller's volumes: when a volume is added to or removed
  from the subsystem, or at `nvme ns-rescan`. A failover moves the primary
  role but makes the host read nothing again. Neither does the end of the
  zeroing, of the suspension, of the clone's wait, of the disable or of the
  level step. So the host still reads the new primary's path, or the path
  to a volume it connected to early, as having no cache. The host's kernel
  then completes each flush on that path at once without sending it, and
  sends a write it marks FUA as a plain write. An fsync returns success
  with nothing made durable. The pool commits on its own about once a
  second. So a write to a new block is lost when the serving primary dies
  before that commit, though its fsync completed. After a failover even
  that commit is not made durable on a disk node whose disk has a write
  cache (the next entry). A write over data
  already there waits in the disks' caches until something else flushes
  them. This ends with a rescan, or with a connection made, or made again,
  while the volume is served. It comes back the next time the host reads
  the path while the volume serves only errors, as at the next failover. A
  Kubernetes volume is attached without a wait for its pool's zeroing, so
  a pod that starts with a new claim can meet this with no failover.
  State: fix later; the question of how is open. Read from the kernel's
  code. Seen in the lab, on one controller node and one host, outside
  dnv's own steps: a
  host's path to a namespace made by hand over an error device, then moved
  onto a loop device that has a write cache, kept reading as having no
  cache, and the host's fsync sent no flush to that device; after
  `nvme ns-rescan` it did. Seen in the
  end-to-end suite's fsync stage: the fsyncs a host made through the new
  primary right after a failover took 0 ms, as fsyncs that send no flush
  do; no suite reads the cache setting of a host's path. The cases with no
  failover only follow from dnv's rules, and no run has shown them.
- **After a failover, no flush of the new primary reaches a disk node.**
  Silent. A controller node reads whether a leg's path has a volatile
  write cache as a host does: when it connects the leg, connects it again
  or rescans it, and not when the path's ANA state changes. A standby
  connects to every leg while each disk node serves that standby's path
  from a device that fails every IO, which has no write cache. A failover
  puts the side's own device under the path and moves its ANA state, and
  nothing makes the new primary read the setting again. So the new
  primary's kernel completes every flush to its legs at once and sends
  nothing: the hosts' flushes, md's own and those of each thin pool
  commit. The same holds for the new side of a migration, whose path the
  controller node connects before the cutover, while the new side serves
  it from such a device. Where a disk node's disk has a volatile write
  cache, completed and flushed writes then wait in that cache, and a power
  loss of the disk node can lose them: md can count that leg as whole, so
  reads can return old data, and a thin pool's metadata can come back
  damaged. This lasts until the controller node connects that path again
  and also loads its own device over the leg anew, which takes the setting
  only when it is loaded, as after the controller node's reboot. A disk with no volatile write cache loses nothing
  this way. State: fix decided. Read from dnv's code and the kernel's;
  not seen in the lab.
- **A failover while the old primary still serves can lose a write the
  host was told was done, in some cases.** Silent. A host still writing
  through the old primary can have a write that lands on one leg of a
  group and fails on another, where the fence came first: md fails that
  other leg and completes the write to the host from the one copy. md then
  records that failure on the leg that took the write. When that record
  cannot be written, because that leg is fenced too by then, the legs look
  equal to md, and when the new primary starts the array, md copies the
  leg in its first place over the others. So the worker fences, in each
  group, the leg md copies from last: it first hands the other legs' disk
  nodes the failover, and that leg's only once they report their fence
  done, or one interval later, 2 seconds at the default. A write that
  fails on the other legs then lands on that leg, md's record reaches it
  before its own fence, and the new primary starts from it. The worker
  learns which leg md copies from in the primary's reports, and keeps it
  in memory only. The loss stays possible in these cases. The worker that
  hands the sides a failover or a cntlr replacement of the primary has had
  no report on a group from the old primary as the primary since it
  started or took the pool over, so it does not know that group's leg. A
  report covers a group only while the primary's row for the group's
  array has the status ok. A worker that restarts or
  takes the pool over during the waits of a failover or a replacement,
  before the leg md copies from is told, is such a worker. A disk node
  does not report its fence within the one interval the worker waits, or
  reports the failover without having fenced, as one whose side device
  failed does. On the lab VMs, where 45 disk nodes share each VM that
  runs disk nodes, a late disk node was the common case at 32 slices: the
  wait ended at its interval in every failover of a pool of 32 slices,
  with 15 to 51 of roughly 64 sides it waited for not yet reported. So
  there the worker ensured the order only in the groups whose sides
  reported in time. The runs timed the disk nodes' reports, which come at
  the end of each converge, not the moment of each fence. md brought a leg
  into sync in a lower place after the primary's last report on the
  group: a leg the primary put back into the array, or the new leg of a
  leg repair, which can be a leg that an earlier repair took out of the
  group.
  The worker then fences that leg before the leg it read as the one md
  copies from. For the late disk node and the leg brought into sync, the
  write is lost only when, of the two legs, the one md copies from is
  fenced first. A fence holds the old primary's IO to its leg while the
  disk node reloads the path. When the other leg's fence comes in that
  time, after the write landed there, md cannot record the failure on it.
  That time is short, but it lasts until a later converge when the reload
  fails. On a meta group the lost write is part of the slice's thin
  pool metadata: the pool comes back as of an earlier commit, and the
  blocks mapped since are gone. On a clone meta group the clone forgets
  parts it had copied and copies them from its source again, over what
  hosts wrote there. These two are read from the kernel's code, not seen
  in the lab. The loss needs a host still writing through the old primary,
  and the old primary to keep serving until the fence. The old primary
  does so when it has not applied its demotion, as on a controller node
  whose agent is stopped or cut off from the control plane but not from
  the hosts. A cntlr replacement of the primary can lose a write the same
  way: the replaced primary serves until its controller node tears its
  stack down, which a stopped or cut-off agent never does. Seen once in
  the lab, in the e2e suite at 32 slices, with the paths fenced in no
  order, and shown with md alone: each run that began fencing md's first
  member first lost the last completed write, and each run that began it
  last kept it. State: accepted, for every case but the late disk node.
  The first case needs a worker restart, or a worker taking the pool over,
  between the old primary's last report as the primary and the fence of
  the leg md copies from. For an agent that is stopped or cut off from
  the control plane, that time is a few seconds, under 20 at the
  defaults. For an agent that answers "starting", it is a little more
  than the start hold, the longest time the worker waits for an agent
  that is still rebuilding, 120 seconds at the default. Before a cntlr
  replacement of the primary, it can be a little more than the cntlr
  threshold, the event threshold of any cntlr, 600 seconds at the
  default. A disk node that reports without having fenced needs a second
  fault, and a leg brought into sync needs md to change the array between
  the primary's last report and the failover. State for a late disk node:
  fix later. On the lab VMs a late disk node needed no fault at 32 slices.
  The fix waits for dnv's maintainers to decide whether the wait should
  grow with the pool.
- **A write can be lost when the paths of a group die one after the
  other.** Silent. A network cut can take every path of a RAID1 group
  away a few seconds apart, with no failover. A write that lands on the
  second leg alone, after the first leg's path died, makes md fail the
  first leg and complete the write to the host from that one copy. md's
  record of that failure cannot reach the second leg either, once its path
  is dead too. So the legs look equal to md, and when they return, md
  copies the leg in its first place over the other in the regions that
  were being written: the write is gone when the leg whose path died first,
  which lacks the write, holds that first place. On
  a meta group the lost write is part of a thin pool commit, so an fsync
  that returned success is undone. This is the loss of the entry above,
  with no failover, and the order of fences cannot help, since no fence
  takes part. Nothing reports it: the same sign, members with one change
  count, md's count of the changes of its array that each leg's superblock
  records, in an array that md did not mark as fully written, shows after
  any crash of a primary.
  State: fix later; no cheap cure is known. Read from the kernel's code;
  not seen in the lab, whose cuts take every leg at the same moment.
- **An old primary can serve again from a thin pool it did not remove.**
  Silent. A demotion makes the old primary remove its arrays and thin
  pools. The removal can stop part way, as when a listing does not answer
  or a command runs into its time limit. The agent then reports a leftover,
  and the worker counts that answer as healthy, so the old primary can
  become the primary again at a later failover. The primary builds an
  array or a thin pool only where none is there, and keeps one it finds.
  dm-thin keeps a pool's records in memory and does not read them from the
  legs again while the pool's device exists. So when the role comes back
  before a later sweep removes that thin pool, the old primary serves from
  records that miss what the other primary wrote in between. A block
  written then reads as zeros, and the old primary's next commit writes
  its old records over the newer ones: the blocks written in between are
  gone, and a block can be given out twice. No error shows, except for a
  volume made in between that the worker had already recorded as created.
  State: fix decided. Read from dnv's code; not seen in the lab.
- **A leg's device can be built over the wrong connection.** The primary
  puts a device over each leg it connects. It reads the device number of
  the leg's connection by the kernel's name for that connection, after a
  slow step and with no second look. The kernel can give a freed name to
  another connection in that time, and the leg's device then maps another
  connection's device, which can be another pool's leg. It needs that rare
  race. State: fix decided. Read from dnv's code; not seen in the lab.
- **A torn snapshot can read as whole.** Silent. A snapshot is taken slice
  by slice. A primary crash between two slices, or a slice whose step was
  skipped or failed, leaves a torn snapshot. The agent marks it torn and
  saves the mark at once in its local store, the directory where it keeps
  its own state on its node. Every cntlr reports a mark it holds, as the
  primary or not, and the worker records the mark from the first report,
  before or after the snapshot is recorded as created. The mark is lost
  only when the controller node that saw the tear never comes back or loses
  its local store, when its agent dies in the moment between the last
  slice's step and the save, when the save fails and the agent restarts
  before the worker records the mark, or when that cntlr is deleted before
  it reports the mark again, as by a cntlr replacement while its node is
  down. A lost mark leaves no sign. Delete and re-create a snapshot
  reported torn. State: accepted. Not seen in the lab.

### 5.2 The forced start

A forced start is the way back when every leg of a group failed. The
cases below can lose data. The words of this section:

- A repair is one run of leg repair on a group, by the worker or by an
  operator's call. A retired leg is a leg that left its group after a
  repair and keeps its extents, and its data, until it is deleted. A
  repair can take a retired leg back into the group. A group's list names
  its legs; a repair's new leg joins the list only when the repair
  finishes.
- A member is a leg that md counts in the group's array. Its superblock is
  md's record at the start of the leg: the array's identity, md's change
  count, which md calls the event count, and the member's copy position,
  how far md has copied onto it. md also keeps a bad-block list, the
  sectors of a member it could not read. A forced start gives the array a
  new identity, so a leg from before it has another identity. dnv reads
  each superblock itself, so a read that fails is never taken for a blank
  leg.
- An operator asks for a forced start with a mark on the group, the
  forced-start mark, that names the leg. The primary acts on the mark.

The entries:

- **A forced start cut short on a group that never reported its array
  running can leave legs that read as fresh.** Silent. The worker records
  that a group's array has run at the first report that reads the group
  running. An array can be created and break before any report reads
  it. When a forced start on such a group wipes the other legs and stops
  short, the primary would create a new array over them only if every leg
  of the group's list served with no md superblock. A forced start keeps
  the named leg's superblock, so no way to reach this is known. If one
  exists, hosts read a mix of the legs' data. State: accepted, written as a
  limit. Not seen in the lab.
- **A half-copied leg can look whole.** While md copies from the one leg it
  holds in sync and that leg dies, md can end the copy by marking the leg it
  copied onto in sync, with the regions it never copied in that leg's
  bad-block list, when the leg that died comes back before the broken array
  is stopped. The leg that died gets a bad-block list too. The forced
  start refuses a named leg that carries a bad-block list unless the mark
  passes that refusal by name (`--override bad-blocks`), and then drops the
  list, which is right for the leg that died. When such a mark names the
  half-copied leg instead, hosts read, over the regions md never copied,
  whatever that leg's disk held there, possibly another pool's bytes, with
  no error. Both legs show the same change count. The override passes no
  other refusal of the start, and the dry run lists each refusal the start
  meets. State: accepted, the override is the operator's choice; procedure
  written,
  [the forced-start guide](operations.md#the-forced-start-guide): name the
  leg md copied from, not the new leg it copied onto; `dnvctl group
  inspect` shows each leg's bad-block count and md's view of it. Seen in
  the lab.
- **A failed stop during a forced start keeps a bad-block list.** When the
  named leg carries a bad-block list, the forced start starts the array,
  stops it at once, and starts it again with the list dropped. If that stop
  fails, the array runs with the list, and the legs added to it copy the
  list's regions as bad too. Hosts then get IO errors over those regions,
  though the named leg holds their data. State: procedure written,
  [A forced start whose stop failed](operations.md#a-forced-start-whose-stop-failed):
  leave the group as it is, with IO errors over the listed regions alone,
  or drop the lists by hand with mdadm at the cost it names. Fix later: a
  call of the gateway that keeps or gives the mark. Forty such stops in the lab
  never failed. A suite case makes one fail on purpose and tries four ways
  from it.
  With the mark still standing, the pool turned down to the level that
  stops the arrays and back up: the forced start runs again and drops the
  list. But the worker clears the mark soon after such a start. With the
  mark cleared, the same steps keep the list, and dnv then refuses a new
  mark. A new mark given right after the failed stop, while the group
  reads as an error, is the one dnv takes; it waits, and the same steps
  then drop the list. That moment ends with the worker's next request to
  the node, which the failed stop's reply makes it send. By hand, at the
  level that stops the arrays, mdadm can start the array with an option
  that drops every leg's list, then stop it. That drops the list too, but
  then the places md never copied onto a leg added under the list read as
  good data, whatever that leg holds there. So a way back that loses
  nothing needs a way to keep or give the mark first.
- **A forced-start mark cleared during a change of the pool's owner.**
  Silent. A pool's owner is the worker that drives it. At an owner change
  the new owner sends a request that drops the mark. If the controller
  node's store disk refuses to save it, and then takes writes again while
  the old owner still sends its older request, the agent can save the old
  one first and act on the mark. When the group's array does not run, the
  agent then starts the array from the named leg and zeroes members that
  may hold newer data. State: accepted. A fix would make the
  agent remember a refused request, against dnv's rule that an agent keeps
  no memory of failed work. Not seen in the lab.

### 5.3 Transfers, clones and migrations

- **The read of a leg's written blocks can miss a block that dm-thin still
  prepares.** Silent. Once a migration's cutover has begun, the worker
  reads the leg's list of written blocks from the slice's thin pool. Just
  before that read, the primary waits for the writes in flight of each
  thin device, but only of a device whose kernel count of writes in flight
  is above zero, and dm-thin does not count a write it holds back while it
  zeroes or copies a new block. So such a write can be missing from the
  list while the zeros of its block are already on the old side. The new
  side then skips the block, and when the held write lands there later,
  the block holds, around the write's bytes, old bytes of a reused extent,
  possibly another pool's, where zeros belong. md reads from that leg with
  no error. It needs the group's other leg to stall across the cutover.
  State: fix decided. Read from dnv's code and the kernel's; not seen in
  the lab.
- **A volume's written blocks can be read while a namespace that etcd no
  longer holds still serves it.** Silent. `dnvctl td get-bm` reads a
  volume's list of written blocks, and the gateway answers only while
  every namespace on the volume is set aside on the primary. It takes
  those namespaces from etcd. A namespace just deleted, or pointed at
  another volume, is gone from etcd while the primary still serves it
  until its next converge, and for a volume with no namespace in etcd the
  gateway asks the primary nothing. A host can then still write the volume
  after the list was read. A block first written then is marked never
  written, and a clone that uses the list reads zeros there. The window is
  the usual delay before the primary applies a change, and longer while
  the worker is slow or down. State: fix decided. Read from dnv's code;
  not seen in the lab.
- **A host's discard can make a migration skip a block.** Silent. A
  discard is a host's message that a range of a volume holds no data any
  more, as `fstrim` sends. The thin pool passes a host's discard down to
  the legs. On a leg past its cutover the discard reaches the device on the
  new side that pulls the data from the old side, and that device marks the
  region as copied, though it did not copy it. The thin pool records the
  removal of the block up to a second later, at its commit. When the
  primary dies in that second, the new primary's thin pool still maps the
  block, and its reads go to the new side, which holds old bytes of a
  reused extent there, possibly another pool's. State: fix decided. Until
  then, stop the hosts' discards during a migration, as [the operations
  doc](operations.md#move-a-leg-to-another-disk-node) says. Read from dnv's
  code and the kernel's; not seen in the lab.
- **A host's discard during a clone's copy can undo a later write.**
  Silent. While a clone copies its source, a host's discard of the clone's
  volume marks a region as copied, and a copy of that region may still be
  on its way. A host write to the region right after goes straight to the
  destination, and the late copy then lands over it: the host reads the
  source's older bytes where it wrote. The window is short. State: fix
  later; the clean fix is in dm-clone, the kernel's device that copies a
  clone. Procedure written: [the operations
  doc](operations.md#move-a-volume-to-another-pool) stops the hosts'
  discards during the copy. Read from the kernel's code; not seen in the
  lab.

### 5.4 Hosts and the Kubernetes driver

- **A host taken off a subsystem's list keeps its connection where the
  controller node cannot end it.** Silent. `dnvctl ss set-hosts` takes a
  host off a subsystem's list, and so does a detach, the step of the
  Kubernetes driver that takes a Kubernetes node off a volume's list. The
  kernel checks the list only when a host connects. Where the cn agent of a
  controller node finds, when it starts, that the kernel can end one host's
  connection, that agent ends the connection of every host that is off the
  list at its next converge, on the primary and on every standby, and while
  such a connection is still there after a short wait, or the node cannot
  read or end the connections, that cntlr serves no namespace of the
  subsystem, the primary included (section 6.1), save a namespace whose move
  to ANA inaccessible fails, which the cn agent logs as
  `ana transition failed`: on a serving primary such a namespace can go on
  serving, the host taken off included, until a later converge ends that
  host's connection or moves the namespace, so two hosts can write the same
  volume meanwhile. A kernel can do this when it is 6.11 or newer, its NVMe
  target is built with the kernel option `CONFIG_NVME_TARGET_DEBUGFS` and
  loaded, configfs is mounted, debugfs is mounted at /sys/kernel/debug, and
  the kernel is not under lockdown, the mode that Secure Boot switches on.
  Where it cannot, or the cn agent's read of these facts fails, the cn agent
  starts only with `--ignore-host-disconnection`, and then ends no connection
  until it is started again, even once the kernel can. On such a node a
  connected host keeps its path after it leaves the list, and two hosts can
  write the same volume. A lockdown on a node where /sys/kernel/security is
  not mounted is the exception: the cn agent cannot see it and starts without
  the flag, the kernel refuses every end of a connection, and while such a
  connection lasts that cntlr serves no namespace of the subsystem, save one
  whose move fails as above (section 6.1). Where the cn agent ends
  connections, a host that connects just as it leaves the list can show its
  connection only after that agent's converge has looked: the cntlr then
  serves it until its next converge ends it, the one that a later change of
  the pool brings, such as an attach to another host, or the one that a later
  check round brings once it finds the connection. State: accepted. The
  option is off in the kernels of most distributions, Ubuntu, Debian and
  Fedora among them, and dnv gives no way to build it, since an NVMe target
  built by hand is not meant for production. `dnv-agent check cn` says
  whether a controller node can end a connection ([the operations
  doc](operations.md#check-a-node-before-it-joins)). The cn agent suite shows
  the end of a connection on controller nodes whose NVMe target is built with
  the option.
- **Two Kubernetes machines with the same host NQN defeat the detach.**
  Silent. The Kubernetes driver takes the content of a machine's
  `/etc/nvme/hostnqn` as the machine's name, and puts that name alone on a
  volume's list of hosts. The package of nvme-cli writes the file at
  install, so machines made from one image share it. A detach then takes
  the name off, and the attach to the second machine puts the same name
  back: the first machine's kernel connects again and is let in. A lost
  machine that still reaches the controller nodes then writes the file
  system beside the machine the volume moved to, with no error. State:
  accepted: the driver cannot see the other machines. Each Kubernetes
  machine needs a host NQN and a host id of its own, as [the
  introduction](introduction.md#requirements) and [the CSI
  tutorial](tutorial_csi.md) say. `kubectl get csinodes -o yaml` prints
  each machine's node id, which is its host NQN: two machines with one id
  share a host NQN. Read from dnv's code; not seen in the lab.
- **The Kubernetes driver can format a volume that holds data past its
  first MiB.** Silent. The node plugin formats a file system volume that
  it takes for blank, and its proof is blkid's answer and the volume's
  first MiB. A volume used before as a raw block device can have zeros in
  its first MiB and data further in, in a form blkid does not know. Given
  later to a pod as a file system volume, it is formatted and its data is
  gone. A form that blkid knows makes the plugin refuse instead. State: fix
  later: a wider proof, for example from the volume's count of written
  blocks, which the primary knows. Read from dnv's code; not seen in the
  lab.

### 5.5 Records and tools

- **An etcd restore can hand a volume's thin ids out again.** Silent. A
  thin id is dm-thin's device number inside a pool. A restore sets a pool's
  next id back. A new volume can take the id of an old volume. The primary
  then reads the pool's records of its thin ids, and refuses a new volume
  whose id holds data, or whose pool's records it cannot read: the volume
  stays unbuilt and its row names the refusal. Two cases stay. A new
  snapshot can still take an old volume's id and serve its data, since a
  snapshot's id always holds data. And a host write to a new volume before
  it is recorded as created, followed by anything that makes a primary
  build the volume again before then (a failover, a reboot of the primary's
  controller node, or a step of the pool's level down to `NO_THINPOOL` or
  further and back), makes that primary refuse the volume's own id. Both
  stay inside one pool. Before the agents' local files are reset, the
  restore check (`dnvctl cn check-restore`) names each volume the restore
  loses and whether a new volume of its pool can take its thin id again.
  State: accepted. Procedure written:
  [a new volume refused for a thin id that holds data](operations.md#a-new-volume-refused-for-a-thin-id-that-holds-data),
  which tells the two cases apart, and
  [restore etcd from a backup](operations.md#restore-etcd-from-a-backup).
  Not seen in the lab.
- **A disk node's agent that starts on older local files removes the sides
  made since.** At its start a disk node's agent builds again from its
  local files, and also removes what they do not name and frees its
  extents, before the worker can send it anything. Nothing checks that the
  files are as new as the node. They are older when a system disk is put
  back from a backup or a snapshot while the data disk stays, when old
  files are copied into the folder, or when a mount that was missing at one
  start is back at the next. Every side made after the files' time is then
  removed, and its extents are free for the next side: in a pool without
  RAID the group's data is lost, and in a RAID1 pool the group loses a leg,
  or both legs when both machines went back to one backup. An older file of
  a side also makes the agent build that side as the file has it until the
  worker's first request. Local files that are missing are safe: the
  worker then sends every object again. State: fix decided. Until then,
  never put an agent's local files back from an older copy: delete them
  instead, as [the operations doc](operations.md#prepare-a-node) says. Read
  from dnv's code; not seen in the lab.
- **An older request can pass a controller node's revision check after a
  failed read of its local files.** Silent. A controller node's agent
  refuses a request whose revision is lower than the one it holds. At its
  start it loads its local files, and when the node's own file cannot be
  read once, it skips every cntlr file too. It then holds no revision for
  those cntlrs, and any request passes. Two workers can drive one pool for
  a short time at a change of its owner, so an old owner can still send an
  older request. On a primary the converge of that request removes what
  the request does not name: a volume made since is removed with its thin
  id, and its data is gone. It needs an agent restart, one failed read and
  an older request at once. State: fix decided. Read from dnv's code; not
  seen in the lab.
- **One disk node's agent that holds two disk nodes checks its disk for
  the wrong one.** A disk node's agent refuses a disk whose header block,
  the disk's first block, where dnv records which node the disk belongs to,
  names another node, but it keeps one wanted name for the whole program,
  and each converge of one of its nodes overwrites it. An agent holds two
  disk nodes after an operator's mistake: its address created as a disk
  node in a second cluster, or two disk node agents that share one folder
  of local files. Nothing refuses either. The first node's sides then read
  as on a foreign disk and take no change, and a side of the second node
  passes the check and gets extents in the first node's table. Both
  clusters count their ids from the same start, so that side can even be
  built over the extents of a side of the first. State: fix decided. Read
  from dnv's code; not seen in the lab.
- **A side can be marked provisioned from the counts of a foreign disk.**
  A disk node's agent fills its counts of the zeroing from the disk's own
  record of which extents each side holds, before it has confirmed that
  record as its own, and the worker marks a side provisioned on
  those counts alone. It needs a disk of another cluster under the agent's
  path. State: fix decided. Read from dnv's code; not seen in the lab.
- **Making the old primary the primary again right after a failed fence
  can write old data.** Silent. A fence that suspends the old primary's
  path and then fails to load the error target leaves that path suspended
  on the side, holding the old primary's writes. If the next change makes
  that controller node the primary again before any later converge fences
  it, those held writes land on the side. The other primary could not
  write this side meanwhile, but it wrote the group's other leg, so the
  two legs can hold different data. Two things must happen. A table load
  fails. Then a change makes that controller node the primary again
  before the syncup of the next check round fences the path. State: fix
  later. Not seen in the lab.
- **An older build started after a newer one can lose data.** Silent. No
  program checks which build its peers run, and a build reads only what
  older builds wrote. An older build ignores what a newer one added. An
  agent of a controller node from a build that does not know the mark a
  newer build sets once a group's array has run, for one, can create a new
  RAID1 array over legs whose md superblocks a forced start cleared, and so
  lose the data those legs hold. State: accepted: a downgrade is not
  supported. Not seen in the lab.
- **A leg an older build took back into a leg repair it then cancelled
  can read as a whole copy.** A leg repair can take back a leg that an
  earlier leg repair removed from the group and kept. The cancel of such a
  repair marks the leg it gives back, since md has written on it, but a
  build from before that mark gave it back with no mark. A later build
  reads that leg as whole. If a leg repair takes it back again, a forced
  start may start the group from it with no `--override partial`, which a
  marked leg needs. State:
  accepted: a later build cannot tell such a leg apart. Not seen in the
  lab.
- **A clone an older build stored with namespace number 0 copies the first
  namespace of its source.** Silent. A clone names the namespace it copies
  by the number the source subsystem gives it, and dnvctl sends 0 when the
  operator leaves the number out. A build from before the gateway refused 0
  could store such a clone. The primary reads 0 as any namespace, so such a
  clone copies whichever namespace the source subsystem lists first. A
  transfer's subsystem holds one namespace, so a clone of a dnv volume
  copies the right one, but a source with several namespaces can give the
  volume the data of another namespace. A clone stored with the number
  0xFFFFFFFF, which names no namespace, waits for ever. No call changes the
  number: the clone is deleted and made again. State: accepted. Read in the
  code, not seen in the lab.
- **A change without a revision token overwrites another operator's
  change.** A revision token is the revision the caller last read, which
  dnvctl sends with `--rev`; the gateway refuses the change if the
  revision has moved since. This loses a change of the desired state, and
  a delete by name can delete a volume that another caller made again
  under that name since the read, with its data. A caller may send the
  token and is then protected. State: accepted, by
  design. Procedure written: the start of [the operations
  doc](operations.md) says which read gives each token and what its
  refusal means.

## 6. The system comes back alone

The table says what dnv does by itself for each fault, and what an
operator does by hand. The third column gives the wait where a setting
controls it. The settings are the pool's thresholds, the pool's fast IO
fail timeout (section 4), the check interval and the cluster's start
hold, which is how long the worker waits for an
agent that is still rebuilding after a restart, in seconds, and three
flags of the worker: `--vote-interval`, `--vote-grace-time` and
`--vote-leave-grace-time`.
Measured times are in [the introduction](introduction.md#status-and-limits).

| Fault | What dnv does by itself | The wait, where a setting controls it | By hand |
|---|---|---|---|
| A disk fails, one leg of a RAID1 group | md fails the leg. IO goes on over the other leg. Leg repair replaces the leg with a new leg on another disk node, and md copies the data onto it. The old leg becomes a retired leg. | `leg_unhealthy`, 1200 at the default, before the repair starts. | Nothing. In a pool without RAID the data is gone (accepted case 2). |
| A disk node is down and comes back | In a RAID1 pool, md fails its legs after the kernel's keep-alive. IO goes on over the other legs. A group with no IO sends md nothing to fail, so the primary fails such a leg in md itself once the leg has failed its health check twice in a row, while another leg holds the data in sync. md then records the death on the other leg, and a failover or a reboot can start the array from that leg alone. Back in time, its legs rejoin and md copies the changed regions. Down for long, leg repair moves each leg to another disk node. In a pool without RAID, the IO waits until the keep-alive gives up, then for the fast IO fail timeout, and then fails (section 4). The IO to a leg that a migration moves waits instead when its side on the node serves it and the path to its other side stays up, both in a pool without RAID and in a RAID1 group with no other leg in sync; in a group whose other leg in sync is on the same machine, it can fail instead. In a pool without RAID it waits until the node comes back. In a RAID1 group it waits until the node comes back or leg repair ends the migration, after `side_unhealthy`; that end keeps the side on the dead node, and the IO then fails, md marks the group's array broken and the pool fails over, as in a switch reload below. A migration past its cutover whose copy is not whole is not ended, and its IO waits until the node comes back. Section 8 gives the other cases, among them a failover that can come first. | `side_unhealthy`, 600 at the default, before the repairs start. The pool's fast IO fail timeout, 5 at the default, bounds how long IO waits after the keep-alive gives up: the IO of a pool without RAID, before it fails, and IO that reaches a lost leg. It does not bound the wait of the IO to a leg that a migration moves, which "What dnv does by itself" gives. | Nothing. A node that never returns: [replace it](operations.md#replace-a-dead-disk-node). |
| A controller node is down and comes back | Each primary on it that has a healthy enabled standby is failed over to it. Hosts see a pause. A pool of one cntlr waits for a cntlr replacement. Back, the cntlrs that were failed over are standbys; a node that was cut off from the network can fail a host's writes for a few seconds first (section 8). A cntlr unhealthy for long is replaced on another controller node. | `primary_unhealthy`, 4 at the default, with two check rounds in a row that find the primary unhealthy; a round runs every `cntlr_interval`, 2 at the default. A missed round counts only once its own wait of one interval has run out, so a failover comes about 6 to 10 seconds after the node dies. `cntlr_unhealthy`, 600, before a replacement. | Nothing. A node that never returns: [replace it](operations.md#replace-a-dead-controller-node). |
| The network between a controller node and a disk node is cut | When the controller node holds the primary, as for a disk node down, seen from that controller node: md fails the legs behind the cut and takes them back when the cut ends. A long cut moves the legs. A cut from a node that holds only standbys changes nothing. | `leg_unhealthy`, 1200. | Nothing. |
| The network between hosts and a controller node is cut | Nothing. dnv reads the agents, never a host. The primary stays the primary and stays in the cdc's answer. The host's IO queues as long as a standby path is left. | None. | First disable each enabled standby with no health epoch that the hosts cannot reach, since the failover takes the one with the smallest id, whatever the hosts see. Then, while such a standby that the hosts reach is left, disable the primary, so a failover runs at once. If none is left, as in a pool of one cntlr, do not disable the primary: enable again each cntlr you disabled, and repair the network. With no enabled standby that has no health epoch, the disable of the primary starts no failover and stops the pool's IO, and a host that follows the discovery log drops its last path, so writes not yet flushed are lost (section 6.1). When the agent of each enabled standby with no health epoch refuses the worker's syncups, the disable starts no failover either and stops the pool's IO, but the cdc still lists those standbys, so the hosts keep their paths to them and their IO waits (section 6.1); enable the primary again at once then. When another cntlr takes the role but a minute later has still not reported its devices built, as when its agent refuses the syncup that makes it primary, do not enable the old primary while the hosts cannot reach it: the worker can then give it the role back, and the cdc stops listing the new primary, so a host that follows the discovery log loses its last path. Disable the new primary only while another enabled standby with no health epoch that the hosts reach is left; otherwise repair the network first, then enable the old primary and, once it shows no health epoch and the hosts show its path live, disable the new primary (section 6.1). After a failover, enable each disabled cntlr again once the hosts reach it. State: procedure written, [the steps](operations.md#a-fault-only-a-host-sees); the worker suite runs the disable on fake agents and the e2e suite the disable and the enable on the lab's agents, and a failover past a disabled standby runs in a unit test of the worker alone; the fault is not tested; fix later, a host health signal into the worker. |
| The network between the control plane and the agents is cut | The data plane keeps serving. The primaries of one cut-off controller node are failed over. The old primary never gets its demotion, and the fence comes after the three-second hold, so hosts that drop the old path inside the hold see a pause, and a host still on it sees IO errors (section 8) and, in the cases section 5.1 lists, can lose a write it was told was done. With every agent cut off, nothing changes until the network is back. | As for a controller node down. | Nothing. |
| etcd is down and comes back | The data plane keeps serving. No call and no reaction runs. A worker that cannot write to etcd stops driving its pools about 30 seconds into the outage, and drives again about one grace time after etcd is back. | The worker's flags: `--vote-interval`, 10 at the default, for the 30 seconds, which are three intervals; `--vote-grace-time`, 60, for the wait after. | Nothing. State: procedure written, [while the control plane is down](operations.md#while-the-control-plane-is-down). |
| etcd's store reaches its quota | The data plane keeps serving. etcd refuses every write but a delete and still serves reads, so no call that writes the state and no reaction runs: each worker stops driving its pools within three vote intervals and does so again each time it starts. Once etcd takes writes again, every worker drives again about one grace time later. | The worker's flags: `--vote-interval`, 10 at the default, three intervals before it stops; `--vote-grace-time`, 60, for the wait after. The cause: etcd's `--quota-backend-bytes`, 2 GiB when not given; the history shrinks only with a compaction, which `--auto-compaction-retention` runs. | Compact, defragment and disarm the alarm: [the steps](operations.md#etcd-refuses-writes-its-store-is-full). |
| One etcd member of several hangs, or its machine is gone with no TCP reset | The data plane keeps serving. Each worker's writes to that member wait out the worker's etcd call timeout, 10 seconds, so the workers can all stop driving their pools at once, and drive again about one grace time later; no reaction runs meanwhile. Measured at a vote interval of 2 seconds: every worker stopped once, about 11 seconds after a member froze, and a leader frozen for 2 seconds stopped a worker in 3 of 6 runs. At a vote interval of 6 seconds, a leader frozen for 3 seconds stops no worker. | The worker's flags `--vote-interval` and `--vote-grace-time`. At the defaults, 10 and 60, none of this is measured. | Nothing. |
| A worker, the gateway or the cdc is down and comes back | Another worker, when one runs, takes the pools after a bounded window. The gateway has no state. Without the cdc, hosts keep their paths and get no path changes. On a host that uses nvme-stas, this is tested only for an outage of a few seconds. After a longer outage of every cdc the host lists, nvme-stas may drop the paths once the kernel gives up the host's connection to each stopped cdc, after `ctrl_loss_tmo`, 600 seconds at the kernel's default; the volumes' IO then fails and writes not yet flushed are lost. Not seen in the lab. A Kubernetes node keeps its paths while a cdc of its list cannot be reached. | The worker's flags `--vote-interval`, 10 at the default, and `--vote-leave-grace-time`, 25: a dead worker counts as gone after twice the interval, and the change then holds for the leave grace time, about 45 seconds in all. | Bring the program back. Bring a cdc back at once. On a host where a longer outage of every cdc made nvme-stas drop every path of a volume, unmount its file system and mount it again on the new device, which replays its journal; when the mount fails or the kernel log reports an error, unmount it if it is mounted, check it with its own check tool, and mount it again once the check finds nothing more to repair. A failover during a cdc outage leaves hosts on a withdrawn path (section 6.1). State: procedure written, [while the control plane is down](operations.md#while-the-control-plane-is-down). |
| An agent restarts | The agent rebuilds its desired state from its local store, the files in which it keeps the desired state it last accepted. The kernel keeps the devices. As soon as the program runs again, the agent answers each check round with "starting", an answer that says it is still rebuilding, and while it does, for the start hold at most, the worker starts no failover, cntlr replacement or leg repair because of that node. A controller node agent that stays stopped for longer than one check interval can have its primaries failed over, as for a controller node that is down. | For a stopped agent, `primary_unhealthy` with two check rounds in a row: its rounds fail at once, so a stop a little longer than one `cntlr_interval`, 2 at the default, can already fail a primary over, and a failover comes about 4 to 8 seconds after the agent stops, at the defaults. For an agent that answers "starting", the start hold: the longest time the worker waits for an agent that is still rebuilding, 120 at the default, counted from its first start even when it stops and starts again while it rebuilds or just after, before a check round has judged it, a cluster setting fixed when the cluster is created (`--start-hold-seconds` of `dnvctl cluster create`). | For a planned restart, the steps in [the operations doc](operations.md#a-planned-restart-of-an-agent). A rebuild longer than the start hold, and a reboot that the worker first sees as an agent that answers "starting", are in section 6.1. |
| A host reboots | Nothing on dnv's side. nvme-stas or the Kubernetes node plugin, the part of dnv's Kubernetes driver that runs on each Kubernetes node, connects the paths again. | None. | See the host actions in section 7. |
| All disk nodes of a pool are down at once, as in a switch reload | md marks each array broken under IO. Hosts see IO errors. The broken array makes the primary unhealthy, so the worker fails it over at the primary threshold, even while the nodes are still down. Once the legs are back, the new primary assembles the arrays again. If a data group broke while its meta group still took writes, the thin pool comes back read-only and needs the repair by hand ([the operations doc](operations.md#repair-a-thin-pools-metadata)). A pool of one cntlr waits for a cntlr replacement at the cntlr threshold. | `primary_unhealthy` for the failover. `cntlr_unhealthy`, 600, for a replacement. | A forced start, when no failover comes. The blocks mapped since the last commit of a broken meta group are gone (accepted case 1). |

### 6.1 Cases where dnv does not come back alone today

- **A leg repair an older build left open can wait for the operator after
  an upgrade.** A repair can take back a leg that an earlier leg repair
  removed from the group and kept. If that leg carries the md identity of
  an older array, as after a forced start from another leg, the primary of
  a later build does not add it, since it may hold data the array lacks,
  unless the repair carries the operator's force. An older build added it,
  and md copied the array over it, only when its md change count was not
  above that of the leg that keeps the data; when the count was above, it
  refused the leg as holding newer data, and the repair waited already. A
  repair created by a build from before that force carries none, so it
  waits, and the primary's report names the refusal. The operator aborts it
  with `dnvctl repair abort`, cancels it with `dnvctl repair cancel` and
  creates it again with `--force`. With the force, md copies the array over
  the leg whatever its count, even one the older build kept out as holding
  newer data. The same check keeps out an aborted repair's bad leg of an
  older identity. State: accepted: refusing is the safe reading, and the
  operator decides. Not seen in the lab.
- **A broken array that the pool holds is not re-assembled in place.**
  When every leg of a group fails under IO, md marks the array broken.
  When the legs return, the primary stops the array and assembles it
  again by itself, but only where nothing holds the array open, as at a
  level that builds no thin pool, and only when every leg of the group
  serves, its health check passes, and its md superblock reads as a full
  member of one array, with no bad-block list; a leg with no md superblock
  is copied onto afterwards. The operator-in-charge flag, an operator's mark on a
  group that keeps the worker's repairs off it, stops it too. At a level
  that builds thin pools the pool holds every array, and md cannot stop
  an array that is held. The group then stays down until a failover or a
  cntlr replacement builds it again on another cntlr, or an operator runs
  a forced start; a pool with one cntlr waits for the cntlr replacement.
  State: accepted: taking the pool and the volumes above down to assemble
  the array in place would take the volumes from hosts as well, and the
  failover or the cntlr replacement builds the group again. Seen in the
  lab: on loop devices, in the cn agent suite after a level step, and in
  the end-to-end suite, where a network cut broke the group and the
  worker's failover built it again on the standby.
- **A forced start cut short after its zeroing can leave no leg to start
  from.** The forced start zeroes the md superblock of every other member
  it keeps, except the open repair's bad leg. When the operator withdraws
  the mark, or a zeroed member dies before its add, the zeroed members
  carry no superblock but still hold data. The primary does not create a
  new array over them, since the group's array has run before, so no data
  is lost (section 5.2 has the one case where the worker has not yet
  recorded that the array ran); the group stays down until a leg with a
  superblock is back, such as the named leg, which a new mark can name. If that leg is gone for
  good, the data on the zeroed legs can be reached only by hand with mdadm.
  State: procedure written, [the forced-start guide](operations.md#the-forced-start-guide), "A group whose every
  leg was wiped". Not seen in the lab.
- **A thin pool whose metadata needs a check, or with corrupt metadata, has
  no repair flow in dnv.** A failed commit leaves a slice's thin pool
  read-only, with dm-thin's mark that its metadata needs a check. The mark
  is kept in the pool's metadata, so the pool opens read-only on every
  controller node. The primary reports the
  pool as an error, and the worker logs it and starts no failover, since a
  new primary would meet the same pool. A pool that dm-thin failed
  without that mark, or that is read-only because its metadata is full,
  is not this case: the first is failed over, since a new primary opens
  the pool from its last commit, and the second gets more metadata from
  the auto-grow. Corrupt thin pool metadata has no check and no repair
  flow in dnv. State: procedure written,
  [Repair a thin pool's metadata](operations.md#repair-a-thin-pools-metadata),
  for metadata that thin_check passes, in a slice of one meta group: the
  level steps, the names of the devices involved, thin_check and
  thin_repair onto a new device, and its copy back over the meta group's
  device, its array in a RAID1 pool. Fix later: a repair of metadata that
  thin_check finds damaged, or that spans more than one meta group, since
  no way is tried yet; for those the procedure says to stop.
  Seen in the lab: a pool with the mark opened read-only, a failed pool opened
  again as writable, and a full one became writable after its metadata grew. A
  suite case repairs by hand a pool whose metadata needs a check, one whose
  metadata sits in one group: the pool's level stepped down to the one that
  builds no thin pool, where the group's array still runs, the check, the
  repair onto a new device, that device's check, its copy back over the
  group's array, the mark cleared, and the level stepped back to read-write,
  the pool writable with its data. The copy back stands in for a swap of the
  new device, as dnv builds the pool over the group's array. That metadata was
  intact, so a repair of corrupt metadata is not tried, nor a pool whose
  metadata spans several groups.
- **A group whose in-sync leg dies during a copy waits for a forced
  start.** A repair finishes once the primary reports the new leg in place,
  while md may still copy onto it. The old leg dies during the copy. md
  will not start from the half-copied leg. State: procedure written,
  [the forced-start guide](operations.md#the-forced-start-guide). Fix
  later, the primary starts from the leg md recorded as last in sync. Seen
  in the lab: the cn agent suite and the end-to-end suite break a group
  this way and bring it back with a forced start.
- **A member md keeps dropping is put back until an md check reaches the
  bad sector; a slow leg reads ok.** md drops a leg on a read error of
  normal IO, and the agent puts it back, while the leg row reads ok.
  The worker asks the
  primary to run md's check on each group in turn, at an interval in days
  that is a cluster setting fixed when the cluster is created, 30 by
  default, zero for off. The check reads every leg and rewrites a sector
  one leg cannot read with the data of the other leg. A rewrite that works
  heals the sector, and the drops end. A rewrite that fails makes md record
  a bad block on the leg, and leg repair then moves the leg, copying from
  another leg of the group that has no bad block; where every leg has one,
  nothing moves, and a region recorded on every leg is lost from the
  group's legs. A retired leg that left the group before can still hold
  that region, but the worker deletes a retired leg seven days after it
  left once the group misses no leg and has no health epoch, and a region
  recorded on every leg does not stop that delete: the worker can delete
  the last copy, and its log does not say that the legs carry bad blocks.
  Keep such a leg with `dnvctl retired-leg keep --leg <leg id>` ([Retired
  legs](operations.md#retired-legs)); the state of that delete is fix
  decided, and it is read from dnv's code, not seen in the lab. Between
  two
  checks the drops go on, each a resync of a small part of the leg. A
  drop stops a running check, and the next try starts again from the
  leg's start, so a sector on which reads fail often enough can keep the
  check from reaching it. The worker gives that check up after two days
  and asks again one interval later. A cluster created before this
  setting existed, or with zero, has no check. A leg with slow IO reads
  ok, and every write of the group waits for it. `dnvctl cntlr inspect`
  shows how long the primary's last small test write and read of each leg
  took, and the kernel's IO counters of each leg, so the slow leg can be
  found ([A slow leg](operations.md#a-slow-leg)); nothing in dnv acts on
  them. State: accepted for the time between two checks and for slow
  legs; migrate the leg's side by hand. Seen in the lab: a check over a sector that fails reads healed
  it, and over a sector that fails reads and writes recorded a bad block.
  Not seen in the lab: drops that keep a check from reaching its sector.
- **A whole copy still needs its source until the worker drains the
  clone.** The worker drains a clone once it reads the primary's report of
  the whole copy. If the primary moves to another cntlr, as in a failover,
  or its controller node reboots before that read, or the worker's own
  checks of the pool stop it, the new build of the dm-clone needs the
  source, and the volume waits while the source is away; so does an
  operator who steps the pool's level down past the clone and back before
  that read. State: accepted. The window is short, but the operator's
  level step can hold it open as long as the level stays. Not seen in the
  lab.
- **A subsystem or a connection whose name has dnv's prefix but names no
  dnv object can stay.** dnv's prefix is `nqn.2024-01.io.dnv:`, the start
  of every NQN dnv makes; a name such as `nqn.2024-01.io.dnv.csi:...` does
  not have it. A subsystem's name starts with that prefix, but this build
  of dnv reads no pool from it. While a cntlr asks for it, the sweep treats
  it as that pool's subsystem. Once no cntlr asks for it, the sweep leaves
  it and its namespaces alone, since a newer build of dnv may have made it,
  and the pool's devices under those namespaces stay too. A controller
  node's connection to a clone's source whose name has dnv's prefix but is
  no transfer stays the same way once the clone has gone: the sweep never
  judges such a connection as a clone source. The gateway refuses such a
  subsystem name, so only a desired state written by hand, or one stored
  before that check, makes one. Such a connection stays only where an
  earlier build of the cn agent made it: this build refuses every desired
  state of a cntlr that carries such a source, the stored one at the
  agent's start included. So a pool whose stored clone carries one, as the
  gateway of 0.1 allowed, takes no change, and a cntlr of that pool builds
  nothing on its controller node after that node's reboot, until the clone
  is deleted. An operator removes such a subsystem or connection by hand.
  State: accepted. Not seen in the lab.
- **A controller node's sweep removes two things that may be an
  operator's.** A subsystem that is not dnv's and has no namespace that
  names a device, no host and no port, once it is 30 s old: a build or a
  removal of dnv cut short leaves the same shape, and nothing else tells
  the two apart. A namespace made but never given a device does not keep
  it. And a connection opened by hand with the host NQN that the cn agent
  uses, once no clone reads through it: dnv takes it for the source of a
  clone that has gone. State: accepted; a requirement, which
  [the introduction](introduction.md#requirements) names. Not seen in the
  lab.
- **What a host wrote in a clone's source reaches the controller node.**
  The primary reads a clone's source over an NVMe-oF connection, as a
  device that carries none of dnv's names, so dnv's udev rules do not reach
  it, nor the partitions the kernel finds on it from the source's partition
  table. The system's own udev rules then act on what a host wrote at the
  start of the source volume, or at the start of a partition in it:
  - A bcache superblock. bcache is a Linux block cache, and the
    bcache-tools package adds a udev rule that takes any device whose start
    holds a bcache superblock. That rule takes the device or the partition
    on the controller node, and the clone cannot read its source.
  - A file system or a partition table. The system adds links under
    `/dev/disk/by-uuid`, `by-label`, `by-partuuid` and `by-partlabel` from
    it, so a look-up on the node by a label or a uuid that the host's data
    also carries can lead to the host's device while the clone runs. A
    host volume made from the same cloud image as the node carries the
    node's own labels and uuids. A btrfs file system is also made known to
    the kernel.

  An md superblock or an LVM label there does no harm on a node prepared
  as [the operations doc](operations.md#install-the-udev-rules-on-a-node)
  says, and a controller node prepared that way mounts its own file
  systems by device path, never by a label or a uuid. State: accepted. dnv
  needs no bcache-tools, and a node without the package, or without its
  udev rule, does not have the bcache case. Not seen in the lab.
- **A clone build that keeps failing may start no failover.** Some ways of
  failing mark the primary unhealthy and others do not. The clone's
  namespaces stay inaccessible and the host waits, with no time limit,
  until the cause goes or an operator disables the cntlr, steps the level
  down or deletes the clone. State: fix later. Not seen in the lab.
- **A part a converge keeps failing to build holds the primary's
  failover.** Some parts of a cntlr's stack are named both by the
  converge's own answer and by every check round while they are missing or
  wrong: an export, a namespace's device, an ANA change, the device a
  cntlr puts over a leg, a clone's dm-clone. When the kernel keeps
  refusing to build such a part, the worker sends the cntlr's syncup again
  on every check round, and the agent converges the cntlr one interval
  after another. Every check round then answers with the last converge's
  report, which the worker does not count toward a failover. So a primary
  whose rows read error this way is failed over at its threshold only once
  its check rounds fail to answer in time, or once an operator disables
  it. A side on a disk node loops the same way, as a migration's new side
  does when it cannot connect to the old side after the cutover. The loop
  runs one converge of the cntlr or side at a time, with one interval
  between two, and the node's other cntlrs and sides go on. State:
  accepted. Not seen in the lab.
- **A host's connection that does not end takes the subsystem's volumes off
  that controller node.** Where the cn agent ends connections (section 5.4),
  each converge ends the connections of hosts that are off a subsystem's
  list before it changes the device under any of the subsystem's namespaces,
  and waits about two seconds at most for them to go. While one is still
  there after that wait, or while the node cannot read or end the
  connections, every namespace of that subsystem on that controller node
  moves to ANA inaccessible, a serving primary's included, so that no host
  is served through that node, the hosts left on the list among them. A move
  to ANA inaccessible that fails, which the cn agent logs as
  `ana transition failed`, can leave that namespace where it was: on a
  serving primary it then goes on serving, the host taken off included,
  until a later converge ends that host's connection or moves the namespace.
  A connection still there keeps the subsystem's row pending: no failover
  comes, `dnvctl cluster get-status` does not list the row, and the hosts
  wait until the connection ends, which it does once the removed host's IO
  held on the node ends, as on a device that does not resume. A node that
  cannot read the connections, after debugfs is unmounted or the NVMe target
  is loaded again without the option, reads the row of every subsystem there
  in error, with a host taken off or none, so that node serves none of its
  volumes. Under a lockdown raised while the agent runs, the row of a
  subsystem with a host still connected that is off its list reads in error.
  Where the pool has an enabled standby with no health epoch whose agent
  does not refuse the worker's syncups, the worker then fails the primary
  over to it at the pool's threshold; a pool with no such
  standby, such as a pool with one cntlr, waits for the cntlr replacement,
  which comes after the pool's `cntlr_unhealthy`, 600 seconds at the default.
  The agent cannot see a lockdown on a node where /sys/kernel/security is not
  mounted: there the row reads pending, no failover comes, and the kernel
  refuses the cn agent's end of that one connection until a reboot without
  the lockdown. Before then, only the host's own disconnect, the host's loss
  of power or of its network, which the NVMe target sees when the host's
  keep-alive messages stop, or the removal of the subsystem from that
  controller node, as at the delete of that cntlr, ends it: that removal ends
  every connection of the subsystem there. State: procedure written ([the
  operations doc](operations.md#change-a-volumes-hosts)). Not seen in the
  lab.
- **A step only a check round names costs a converge every second check
  round while the converge keeps failing it.** Some work is named only by a
  check round, not by the converge's own answer: a leg to put back into its
  array, an array to start, a forced start, a snapshot to make, the start
  or stop of md's check, a connection to make again. The worker then sends
  the syncup every second check round at most. When the check round's test
  passes but the converge keeps failing, each such syncup costs one
  converge that fails again. That happens when md keeps refusing a change
  of the legs of a clone meta group's array, which a check round does not
  try, so that it names the clone's dm-clone over that group; when a
  program outside dnv holds an array open, so that md refuses to stop it
  for a forced start or for a new assembly of a broken array; when a leg
  that a migration moves serves on one path while its other path cannot
  connect; when md keeps refusing to start or stop its check; when the
  message that makes a snapshot keeps failing, each try of which pauses the
  IO of the snapshot's origin for a moment; when a slice's thin pool has
  lost a volume's thin id, dm-thin's device number of the volume inside the
  pool, which a check round cannot tell from a volume not built yet, and
  which only an operator ends; and when the primary refuses a new volume's
  thin id because the id holds data, or because the read of the pool's
  records of its thin ids fails for a reason other than another read of
  them that is running
  ([a new volume refused for a thin id that holds data](operations.md#a-new-volume-refused-for-a-thin-id-that-holds-data)),
  which a check round cannot tell from a volume not built yet either: each
  converge reads those records again, until a read shows the id free or an
  operator ends it. Between two converges a check round reads the cntlr
  anew, so the primary's check rounds still count toward a failover. State:
  accepted; each case lasts only as long as its fault. Not seen in the lab.
- **A disk node or a clone source that stays away costs a converge every
  second check round once its connection is gone.** While a disk node is
  dead, the controller node's kernel keeps trying to connect to it again,
  and beyond failing its legs in md, and cutting the other path of a leg
  that a migration moves when the dead node's path was the one that served,
  dnv spends nothing on it. Once the kernel has deleted the connection, as
  after the disk node refused a reconnect with "do not retry" or after a
  reboot of the controller node, every check round of a cntlr with a leg
  there names the leg's side, since only a converge connects it again. The
  worker then sends the syncup every second check round at most, and each
  converge tries the connect, with the retries a budget of about one second
  allows. Before each connect the controller node checks that the disk
  node answers on the network. When the disk node's machine is down, that
  check gives up after about one and a half seconds, and every other leg
  on that disk node then fails at once in the same converge. This lasts
  until the disk node answers, leg repair has replaced
  the leg, or an operator takes the leg off the disk node, by a repair or a
  migration, steps the pool's level down to one that builds no side, or
  deletes the pool; the disk node itself cannot be deleted while the leg is
  on it. A clone source that the primary cannot connect costs the same
  until the source answers, the clone is deleted, or an operator steps the
  pool's level down to one that builds no clone. An operator who disables
  the primary ends the cost in two cases only: the fault is in that
  controller node's own path to the source, or no standby can become the
  primary, and then the pool serves no IO. Otherwise the failover makes a
  standby the primary, which tries to connect the same source and costs the
  same. State: accepted: the converge is the only way a deleted connection
  comes back. Not seen in the lab.
- **A disk node whose network answers while its NVMe target is stuck
  slows every connect of a controller node.** The kernel of a machine
  makes one NVMe connect at a time. Before each connect, the controller
  node checks that the disk node answers on the network. On a disk node
  whose machine is down, the check gives up after about one and a half
  seconds instead of holding the kernel. A disk node whose machine
  answers while its target does not passes the check. Its connect then
  holds the kernel until the kernel's own timers end it, and every other
  connect of that machine waits, a host's too. A disk node's connect to
  the old side of a migration makes no such check first, so a dead old
  side holds the kernel about three seconds for each try. State: accepted:
  dnv has no way to cut short a connect the kernel makes. Not seen in the
  lab.
- **The primary can guess wrong whether mdadm would start a stopped
  array.** A check round of the primary names a group whose array does not
  run when mdadm would start it, which the agent works out from the legs'
  md superblocks. Two rare shapes make the guess wrong. An md superblock
  that a newer kernel series wrote, which the controller node's older
  kernel refuses (section 10), reads as good, so the primary tries the
  start every second check round until the controller node runs the right
  kernel series. A replacement member, which only an operator's own mdadm
  run makes, holds the same place in the array as another member, and the
  guess, which keeps one member for each place, can hold back an array
  mdadm would start; the group then waits for a syncup that comes for
  another reason, or for the operator. State: accepted; each shape needs a
  step outside dnv's rules. Not seen in the lab.
- **A clone source address waits while the address of another connection
  to the source cannot be read.** A clone's source can have more than one
  address. When the controller node cannot read which address one of its
  connections to the source uses, a check round names no address of the
  source, since that connection may be the one an address needs, and the
  kernel would refuse a second connection there as a copy of it. An
  address with no connection then waits for a syncup that comes for
  another reason, such as a new revision, for as long as that read fails.
  State: accepted. Not seen in the lab.
- **A transfer whose namespace is gone is named by nothing.** A transfer
  is made from one of the pool's namespaces. When that namespace is gone
  from the desired state while its volume stays, the primary names nothing
  of the transfer. The transfer's device should then serve only errors;
  when that change fails, the device stays as it was until a syncup comes
  for another reason. Such a transfer also skips the step that sets its
  list of hosts, so a host taken off that list keeps its access: it reaches
  only errors, unless the change of the device failed too, and then it
  reaches the volume's data. The gateway refuses to delete a namespace a transfer
  is made from, or to point it at another volume, so only a record written
  before that refusal holds this state. State: accepted. Not seen in the
  lab.
- **A new side set read-only by hand does not finish zeroing.** When an
  operator sets a new side's device read-only with `blockdev --setro`
  while its disk node zeroes it, every zeroing step fails. The side's row
  reads error, but nothing makes the worker send its syncup again, so the
  side stays unprovisioned until a converge that comes for another reason
  makes the device writable. State: accepted; only an operator's own step
  makes it, and `blockdev --setrw` by hand ends it. Not seen in the lab.
- **A thin pool set read-only by hand holds back a new snapshot.** When an
  operator sets a slice's thin pool device read-only with `blockdev
  --setro`, the primary's check rounds no longer name a snapshot not yet
  made there, though a converge would make the pool writable again and
  make the snapshot. The snapshot then waits until a syncup comes for
  another reason. State: accepted; only an operator's own step makes it,
  and `blockdev --setrw` by hand ends it. Not seen in the lab.
- **A disk node that stays healthy but cannot serve a new leg can still
  get the group's next repair.** When the worker gives up a repair because
  its new leg failed, it cancels the repair and creates the next one in
  the same step, never on that disk node. When the next repair cannot be
  made at that moment, the worker cancels alone. That happens when no
  other disk node has room, or when the group waits for something else,
  such as an operator. The next repair may then go to the same node
  again, until an operator disables the node or it recovers. When the
  group waits only for a newer report of the primary, the worker keeps the
  repair and waits instead. Only the repair just cancelled names its disk
  node, so two failing disk nodes can take turns. State: accepted. The
  worker keeps no memory of failed work, and an operator can disable the
  node.
  Not seen in the lab.
- **A repair made for a leg's bad blocks can be called off after md
  refuses its new leg.** md keeps a bad-block list for each leg: the places
  it could not read or write. When a leg has such places and another leg of
  the group has none, the worker repairs that leg. Just before it adds the
  new leg, the primary takes the leg with bad blocks out of md. When md
  then refuses the new leg, the next report names no bad blocks on the old
  leg, since md no longer holds it, and the worker calls the repair off.
  The primary then puts the old leg back into md, and the group runs on
  its other legs until it does. A new repair comes only when a report
  names the bad blocks again, or when the leg's health fails. State: fix
  decided, the worker calls the repair off only while md still holds the
  leg. It needs md to refuse an add, which is rare, and nothing is lost.
  Not seen in the lab.
- **Leg repair for bad blocks takes the leg out before the other leg is
  known to be whole.** md's check finds a sector of one leg that it cannot
  read or write again, and records it in that leg's bad-block list. The
  worker then repairs that leg at once, since the other leg shows no bad
  block so far. The repair stops the running check, so the check never
  read the rest of the other leg, and the primary takes the leg with bad
  blocks out of the array before it adds the new leg: the group runs on
  the other leg alone during the whole copy. A bad sector there is then
  read from the last leg in sync: md marks the array broken, host writes
  fail, the primary is failed over, and the sector is recorded as bad on
  that leg and on the new leg, though the leg taken out holds it. Two legs
  with bad sectors in different places are something RAID1 survives; the
  repair turns them into a lost region and an outage. The leg taken out
  keeps its data as a retired leg until it is deleted. State: fix decided.
  Read from dnv's code and the kernel's; not seen in the lab.
- **A repair whose new leg the primary never reports stays open while
  the primary role cannot move.** It blocks every other repair of the
  group. This happens only while the primary does not apply the syncup
  that carries the repair. A primary whose agent refuses that syncup,
  because it cannot save it to its local files or finds a setting in it
  invalid, is unhealthy, so the worker fails it over at the primary
  threshold, and the new primary reports the new leg. The repair stays
  open when the role cannot move that way. When the pool has no healthy,
  enabled standby, the worker replaces the primary with a new cntlr after
  the cntlr threshold, 600 seconds at the default, and the wait ends when
  the new cntlr's node saves the syncup. When every healthy, enabled
  standby refuses its syncups too, the worker takes the refusal as one
  they all share, makes no such standby primary and does not replace the
  primary either, though a refusal of each node's own local files would
  not stop a new cntlr on another node. The wait then lasts until the
  refusals end, or until the primary stops answering, which the worker
  then replaces as a dead primary. The worker's request to abort the repair reaches the
  primary only in a later syncup that carries the repair too, so giving up
  on a timer would not end the wait, and it would abort repairs that a slow
  primary still carries out. State: accepted; a failover onto a standby
  that refuses its syncups too would not help. Not seen in the lab.
- **A repair's new leg that holds the whole data starts the array alone
  only where md recorded the other legs as failed.** When both legs of
  the group's list are dead, the group otherwise waits for its legs. An
  operator's forced start from the new leg waits too, for the leg of the
  list that is not the repair's bad leg. State: procedure written, one line in
  [the forced-start guide](operations.md#the-forced-start-guide). Not seen in the lab.
- **After a forced start, a leg of the old identity can keep the group
  down.** A forced start gives the array a new identity, the number md
  gives each array, and leaves the open repair's bad leg with the old one.
  The primary does not add that leg back, since it may hold data the array
  lacks. Once the repair is cancelled, the next assembly, as after a
  failover or a restart, finds legs of two identities and leaves the group
  down, its row in error. The worker does not replace such a leg by itself,
  since the leg has no health epoch. The operator replaces it with a
  repair: with no flag while the array runs, and with the flag
  `--force-undecided` once the group is down, since only the operator can
  tell which identity is current, and a wrong choice serves the older copy
  and loses the writes made since. The gateway asks for that flag only
  when the primary's report names the leg's identity as undecided: while
  the primary's agent is still starting, it takes a leg that has a health
  epoch without the flag, and once that repair finishes the worker may
  delete the old leg when it is old enough. The worker's own repair is a
  second way: the forced start's keep mark for that leg, which keeps the
  worker from deleting a retired leg, lives on the open
  repair's record, so the cancel of the repair drops it. When the leg
  later gets a health epoch, as when its disk node is down for a while,
  the worker replaces it with a repair of its own, whose finish retires the
  leg without the keep mark, and the worker deletes it seven days later,
  though it may hold newer data than the array. Keep it with `dnvctl
  retired-leg keep --leg <leg id>` once it is retired; the state of that
  delete is fix decided.
  State: procedure written,
  [a leg of another identity that the primary does not add](operations.md#a-leg-of-another-identity-that-the-primary-does-not-add),
  not run on real agents. Not seen in the lab.
- **A primary that keeps a leaving member is failed over, and the new
  primary leaves that member out.** The primary never fails a member that
  is leaving the group while no member that stays is in sync on a working
  leg. While it keeps such a member, the group's row reads error, so the
  primary is failed over at its threshold. The new primary assembles the
  group from the legs of the group's list and the open repair's new legs,
  which leave that member out. State: fix later. Not seen in the lab.
- **A forced-start mark the primary did not act on waits for the
  operator.** The worker clears a mark only when the primary reports that
  its own forced start of that mark started the array. The primary keeps
  this in memory, so a mark whose start an agent restart forgot stays, and
  so does a mark written over an array that runs. While it stays, the
  worker makes no leg repair of the group, and the primary acts on the mark
  at the array's next stop. State: accepted; the operator cancels the mark
  ([the forced-start guide](operations.md#the-forced-start-guide), "A forced start that waits"). Not seen in
  the lab.
- **A forced start waits on a primary whose layers above hold the broken
  array.** Only a failover or a level step that stops the pools lets it
  run. State: procedure written, [the forced-start guide](operations.md#the-forced-start-guide), "A forced start that
  waits".
- **An open repair, or the operator-in-charge flag, holds the worker's
  repair.** A group has one repair at a time. State: accepted, by design.
- **A pool with a lost record waits for an operator.** A pool's record
  lists the names of its volumes and its other objects. When the record of
  one of them is gone from etcd, the worker sends the pool nothing.
  Removing that name from the pool's list makes the primary delete a
  volume's data for good. The status read names the lost record's key
  ([the operations doc](operations.md#read-what-is-wrong)). State:
  procedure written for a volume's record, [restore one record from a
  backup](operations.md#restore-one-record-from-a-backup), rehearsed in the
  lab with fake agents. For a record of another kind, fix later: no safe
  way to put it back is known, since a backup's copy of such a record can
  be older than the pool's legs, health and hosts; ask for help, and never
  remove the name.
- **A node deleted with force keeps leftovers.** A node delete asks the
  node's agent. It is refused while the agent does not answer, reports a
  leftover or has not yet applied the node's latest desired state, and, on
  a disk node, while the disk still keeps a side or a part of its metadata
  area in use. `dnvctl dn delete --force` and `dnvctl cn delete --force`
  delete the node without asking. Such a node keeps what its last sweep
  had not removed, and its agent builds that again each time it starts,
  until the agent's local files are deleted. A disk node whose disk is dead
  or names another node is deleted only with force. A reused disk is
  refused, since its header block, where dnv records the disk, still names
  the old node, until that block is zeroed by hand. A dead disk node cannot
  be deleted while a side is on it, with force too. State: procedure
  written for the removal of a node ([the operations
  doc](operations.md#remove-a-node)), for what a forced delete leaves
  ([leftovers on a node](operations.md#leftovers-on-a-node)) and for the
  reuse of a disk ([reuse a disk from an old
  node](operations.md#reuse-a-disk-from-an-old-node)); no test zeroes a
  disk's header block and creates its node again, nor removes a forced
  delete's leftovers by hand. Fix later: a forced delete of a dead disk
  node that also frees the sides of its retired legs. Not seen in the lab.
- **Space of a dropped side can stay in use for a while.** When a side
  leaves a disk node and no other side of its pool has its device on that
  disk node, the agent keeps the side's extents in use for as long as the
  machine holds an empty NVMe-oF export of that pool, one that is being
  built or whose removal stopped part way, that is less than 30 seconds
  old. It also keeps them for as long as the agent cannot read an NVMe-oF
  export of that pool, or cannot list the machine's exports at all. The
  side's own removal leaves such an empty export when it stops after the
  export's namespace is gone; one that stops earlier leaves an export the
  check rounds report. Another agent on the same machine has such an export
  while it builds or removes an export of the pool. The check rounds report
  a read or a listing that fails, but not the young export. A side placed
  on that disk node in that time can fail for want of room, and a node
  delete without force is refused in that time. When the kernel renews an export's
  time stamp each time its settings are read, the wait can last longer:
  while another agent on the machine keeps failing part way through a build
  of an export of the pool, and, when two agents on one machine hold the
  two sides of one leg, until the other agent builds that export again or
  removes it. State: accepted. Only its age tells such an export from
  another agent's export still being built. Not seen in the lab.
- **A node's own converge whose call was cut off leaves part of its build
  waiting.** When the call of a disk node's or a controller node's own
  syncup is cut off, what its converge did not build, other than the
  node's nvmet port, waits for the node's next revision. What it did not
  remove is a leftover, which the next check round names, and the worker
  sends the syncup again. Sides and cntlrs do not wait: their next check
  round names what is left. State: fix later. Not seen in the lab.
- **A leg of a primary can go without its health check.** The primary
  starts the health check of a leg in a converge: after a restart, after
  a failover, and for a new leg. When that converge's step for the leg
  fails, and the controller node also does not answer the read of the
  device the cntlr puts over the leg, the leg gets no health check. Its
  row then reads pending, which sets no health epoch. No check round names
  the leg, and the primary does not fail it in md by itself either, as it
  does for a leg that fails its health check twice in a row (section 6).
  This lasts until a syncup comes for another reason, such as a new
  revision or a check round that names another part of the cntlr. A leg
  whose health check already runs keeps it. State: accepted; it needs two
  reads of the controller node that do not answer in one converge, as on
  a node whose reads stall. Not seen in the lab.
- **An agent restart during a fence can leave devices suspended.** A disk
  node's agent keeps the start of a fence's wait in memory. A restart
  during that wait can leave the old primary's devices on the side
  suspended, with no timer to end it. A syncup of the side, or a level
  step, ends it. Nothing in the wait can reach the data. State: accepted.
  Not seen in the lab.
- **A leg's device left suspended under a RAID1 array can stop a
  controller node's removals.** The cn agent puts a device mapper device
  over each leg it connects. When a reload of that device fails, the
  device can stay suspended under the leg's RAID1 array. The sweep resumes
  suspended devices only down to an md array, so it does not resume this
  one. md's writes to the leg then wait in it, and the removal of the
  namespaces above the array fails each time the sweep runs. While the
  cntlr still wants the leg, its next converge resumes the device. For a
  pool that has left the controller node, nothing does, until an operator
  runs `dmsetup resume` on that device. State: accepted. Not seen in the
  lab.
- **Worker membership windows.** Workers meet only in etcd. At every owner
  change two workers can drive the same pool, or none, for a few seconds.
  A worker learns of changes through streams from etcd. One whose stream
  loses another worker's heartbeats takes that worker for dead and drives
  its pools beside it for up to about a minute. A stream that loses
  changes while etcd's progress messages still arrive is never noticed,
  and what it follows stays as it was on that worker until a restart.
  State: accepted, by design.
- **A fault only a host sees never fails over.** See the table. On a pool
  with an enabled standby that has no health epoch, disabling the primary
  starts a failover at once, unless the agent of each such standby
  refuses the worker's syncups. The failover takes, of the enabled
  standbys with no health epoch, the one with the smallest id, whatever
  the hosts see, and passes over one whose agent refuses the worker's
  syncups. So first disable each enabled standby with no health
  epoch that the hosts cannot reach; the disable of a standby starts no
  failover. Then disable the primary only while such a standby
  that the hosts reach is left; if none is left, do not disable it,
  enable again each cntlr you disabled and repair the network. A disabled
  cntlr is never made the primary and never replaced, so enable each one
  again after the failover, once the hosts reach it: an enabled cntlr can
  take the role again at a later failover. On a pool with no enabled
  standby that has no health epoch, a pool of one cntlr among them, a
  disable starts no failover and stops the pool's IO at once, since a
  disabled cntlr serves no IO. The cdc stops listing the disabled
  primary, so a host that follows the discovery log drops its last path,
  writes not yet flushed are lost, and a Kubernetes mount does not
  survive it. When the agent of each enabled standby with no health
  epoch refuses the worker's syncups, no failover follows either and
  the pool's IO stops, but the cdc still lists those standbys, so the
  hosts keep their paths to them and their IO waits; enable the primary
  again at once then. When another cntlr takes the role but a minute
  later has still not reported its devices built, as when its agent
  refuses the syncup that makes it primary, the hosts' IO waits on its
  path. Do not enable the old primary while the hosts cannot reach it:
  the worker can then give it the role back, at once after a disable of
  the new primary and otherwise once the new primary's health epoch is as
  old as the longer of the pool's `primary_unhealthy` and
  `cntlr_unhealthy`, 600 seconds at the defaults, and the cdc stops
  listing the new primary, so a host that follows the discovery log
  drops its last path. Disable the new primary only while another
  enabled standby with no health epoch that the hosts reach is left.
  Otherwise repair the network first, then enable the old primary and,
  once it shows no health epoch and the hosts show its path live, disable
  the new primary. State:
  procedure written,
  [the operations doc](operations.md#a-fault-only-a-host-sees); the
  worker suite runs the disable on fake agents and the e2e suite the
  disable and the enable on the lab's agents, not the fault itself, and a
  failover past a disabled standby runs in a unit test of the worker
  alone; fix later.
- **An nvme-stas host loses every path when its discovery daemon is
  away.** nvme-stas has two daemons: stafd talks to the cdc, and stacd
  connects the paths. stacd reads a missing stafd as an empty discovery
  log. With the tutorial's setting, which lets stacd disconnect what it
  connected itself, it drops every path it made at its next start or at
  its next pass over its connections. A reload, a removed controller, a
  device audit or one of its timers starts such a pass. This was read in the source of
  nvme-stas 2.3.1; the lab runs 2.4.1. To restart stafd, stop stacd first,
  which keeps its connections when it stops; restart stafd; once stafd
  holds its connection to each cdc again and their discovery logs name
  every subsystem the host uses, or, with a cdc away, once the cdcs it
  reaches still list every path the host must keep, start stacd.
  State: procedure written, [the operations
  doc](operations.md#restart-nvme-stas); the order is not tested.
- **A host keeps a withdrawn path.** A path connected by hand, or to a
  controller named in stacd's own configuration, is never dropped. A cdc
  cut off from etcd keeps listing a failed-over primary. Such a host
  writes to the fenced old primary and gets IO errors in the failover
  window, and, in the cases section 5.1 lists, can lose a write it was
  told was done. While a host still holds the path of a cntlr that was
  lost, it also gets no path to the cntlr that replaces it, which takes the
  same controller id; the host must drop the old path first, as nvme-stas
  does once the cdc stops naming it (seen in the lab). A Kubernetes node
  drops it only once another path of the volume shows the volume, so on a
  pool of one cntlr it keeps the old path, and the volume waits (the next
  entry). State: procedure written, [the operations
  doc](operations.md#a-host-that-keeps-a-withdrawn-path); the e2e suite's
  cutoff case drops a path connected by hand and then connects the new
  primary, on a host that holds one path; no test runs the procedure's
  connect before the drop, its drop by device of one path among several,
  its drop of a replaced cntlr's path, its steps for a controller named in
  stacd's configuration, or its steps for a cdc cut off from etcd.
- **A Kubernetes mount waits for ever when no cntlr of its volume comes
  back.** The node plugin's connect keeps reconnecting through an outage
  of every path, however long, and the IO waits. So the mount survives a
  long outage. It survives a cntlr replacement on a pool of one cntlr only
  while the old path lasts until the new path has joined, and the new path
  cannot join while the old path holds the controller id that the new
  cntlr gives the node (A host keeps a withdrawn path, above). The plugin
  then keeps the old path, which keeps reconnecting, and the mount waits
  until that path ends, by a reconnect refused with "do not retry" or by
  the operator's `nvme disconnect`; the mount does not survive that end (A
  Kubernetes mount does not follow a new device, below). But a pod blocked
  in that IO cannot be stopped until the operator runs
  `nvme disconnect -n` with the volume's subsystem NQN on the node.
  Nothing reports a dead mount.
  State: procedure written, [the operations
  doc](operations.md#a-kubernetes-mount-that-lost-its-last-path), not
  tested; accepted, and no fix is planned.
- **A Kubernetes mount does not follow a new device.** A reconnect refused
  with "do not retry" ends for good, as when a controller node comes back
  holding other volumes' cntlrs before the volume's. Once the subsystem's
  last connection is gone, the mount keeps the dead device, and a later
  connect makes a new device that the mount does not follow. The node
  plugin mounts a filesystem volume, one it formats and mounts for its
  pods, once on each node, and a pod that starts on that node while this
  mount stands gets it as it is, the dead device with it. So the volume's
  pods must stop, and the plugin must unmount the volume on that node,
  which it does once no pod of the volume is left there, before a new pod
  mounts the new device. A raw block volume, which a pod gets as a bare
  device, needs only a new pod. State: procedure written, [the operations
  doc](operations.md#a-kubernetes-mount-that-lost-its-last-path), not
  tested; accepted, and no fix is planned.
- **A Kubernetes node whose cdc list leaves out a cdc drops that cdc's
  volumes.** The node plugin follows the cdc addresses of its own
  configuration and no other. When the list leaves out every cdc that serves
  some pools, the listed cdcs list no path for those pools' volumes, and the
  plugin drops their paths on that node as after a detach: their mounts
  fail, and their pods must be stopped and started again, as for a mount
  that does not follow a new device (above). A listed cdc that
  is restarted to serve fewer pools does the same to the pools it gives up,
  until the list names a cdc that took them. A change of the list reaches a
  plugin only when it starts again. So the order of a change is: start the
  new cdcs first, while the old ones still serve every pool; then change the
  list and restart the plugins; only then make an old cdc serve fewer pools
  or stop it. State: accepted; the list is configuration. Procedure written:
  [the operations
  doc](operations.md#change-the-cdc-addresses-of-the-kubernetes-driver).
- **A cntlr replacement on a one-cntlr pool can cost the volume its path.**
  This happens when the old path's reconnect is refused with "do not
  retry" before the new path joins. It also happens when the new cntlr
  gives the node the controller id that the old path, still reconnecting,
  holds: the new path cannot join, and the mount waits until the old path
  ends (A Kubernetes mount waits for ever when no cntlr of its volume
  comes back, above). State: accepted; use two cntlrs per volume.
  Procedure written: [the CSI tutorial](tutorial_csi.md#the-driver) says
  so where its one-machine class asks for one cntlr. For a Kubernetes
  volume whose new path cannot join: procedure written, [the operations
  doc](operations.md#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement);
  fix later.
- **A primary that never reports its stack built waits for the longer
  threshold.** It is failed over or replaced only after the longer of the
  primary and the cntlr thresholds, 600 seconds at the defaults. The pool
  stays on it that long. Disabling the primary ends it sooner. State:
  accepted, by design.
- **A controller node's agent that takes longer than the start hold to
  rebuild fails over its primaries.** After a restart the agent answers
  each check round with "starting", an answer that says it is still
  rebuilding from its local files, until it has rebuilt every cntlr of the
  node, one after another. The worker waits for such an agent for the
  start hold, a cluster setting fixed when the cluster is created
  (`--start-hold-seconds` of `dnvctl cluster create`), 120 seconds by
  default, counted from the agent's first start, also when the agent
  stops and starts again while it rebuilds, or just after, before a check
  round has judged it. When the worker that drives a
  pool restarts, or another worker takes the pool over, in that time, the
  new one counts from the agent's latest start. A rebuild that takes
  longer fails over every primary on the node that has a healthy standby.
  Create a cluster whose controller nodes hold many pools with a longer
  start hold. State: accepted; the wait must
  end, so that an agent whose rebuild never ends is treated as a node that
  is down. Not seen in the lab. A host still writing through such a
  primary can also lose a write it was told was done, in the cases
  section 5.1 lists.
- **A reboot that the worker first sees as an agent that answers
  "starting" delays the failover.** The worker cannot tell an agent that
  restarted on a running kernel, whose devices still serve, from the agent
  of a controller node that rebooted, whose devices are gone. When the
  agent of a rebooted controller node answers "starting" before the
  failover has begun, as after a fast reboot or when a worker has just
  taken over the pool, each primary on the node keeps its role for up to
  the start hold while its hosts' IO waits. State: accepted; the start
  hold bounds the wait. Not seen in the lab.
- **A machine that holds a pool's primary and a leg of a group stops the
  group when it stops.** A new pool keeps its legs out of the location of
  its first primary while another location has room, and a node's location
  is its machine by default. Nothing else keeps the primary away from the
  legs: a failover, a cntlr replacement, a cntlr an operator creates, a
  grow, a clone's own group, a repair and a migration each place without
  looking at it, and a pool created where too few locations have room can
  keep a leg there. A node an older build created with no location has its
  address as its location, and a rack name given to a machine's controller
  node and not to its disk nodes hides the machine the same way: the pool
  then has nothing to keep its legs from. When such a machine stops, the
  leg on the other machine still records both legs as in sync, so md does
  not start the array from it alone, and the new primary leaves the group
  stopped. The pool waits for the machine to come back, or for an
  operator's forced start
  ([the forced-start guide](operations.md#the-forced-start-guide)). State:
  accepted. Read from dnv's code; not seen in the lab.
- **A disabled primary whose standbys refuse their syncups can stop the
  pool's IO.** A standby refuses its syncups when it cannot save them to
  its local files or finds a setting in them invalid. Once the worker has
  seen a standby refuse a syncup, it does not make that standby primary
  until the standby accepts a syncup, reports the revision it refused as
  applied or misses a check round, since it would refuse the syncup that
  makes it primary too. When an
  operator disables a primary and every healthy, enabled standby of its
  pool has so refused, no other cntlr takes the role, and the worker does
  not replace a disabled cntlr. The pool's IO stays stopped until a
  standby accepts its syncups again, or until the operator enables the
  primary again, enables a healthy standby whose agent accepts its
  syncups, or adds a cntlr. If the primary's own agent refuses the disable
  too, the primary keeps serving the hosts that do not follow the cdc's
  changes. A standby that has refused no syncup yet, such as one whose
  node's local files began to refuse writes after its last syncup, can
  still be made primary by the disable, before its refusal of the
  disable's own syncup comes back. Its agent then refuses the syncup that
  makes it primary, and the pool's IO stays stopped until the worker moves
  the role again after the longer of the pool's event thresholds for the
  primary and for any cntlr, 600 seconds at the defaults, onto a cntlr
  that accepts its syncups, such as the old primary once the operator
  enables it again. State: accepted; a cntlr the operator disabled is left
  to the operator, and the worker does not replace it. Read from the code;
  not seen in the lab.
- **A corrupt cdc entry withdraws its subsystem from hosts.** A cdc entry
  is the record in etcd that the cdc serves for one subsystem. Only a hand
  edit or a bug corrupts it. State: accepted.
- **A disk node whose disk comes back with larger blocks keeps its data,
  but its legs stop serving.** A disk that dnv formatted at 512-byte
  logical blocks can read 4 KiB blocks later, as a loop device set up
  again with `losetup -b 4096` over the same file does. The dn agent then
  removes nothing: the disk keeps its records, its sides and their data,
  and the agent reports the disk as an error with the fix, so the node
  gets no new side. Until the operator sets the device up again at 512
  bytes, no controller node can use its legs reliably. In a RAID1 group
  the primary's md refuses to add such a leg to its running array, and
  after a failover the new primary still sees 512-byte blocks over the
  larger ones, so md fails the leg on its first write that the disk
  refuses. Leg repair then puts a spare in the place of each leg, on
  another disk node. State: accepted: the data stays in place for the fix
  and for a recovery by hand, and refusing the disk would stop the same
  legs. Read from the kernel's code; not seen in the lab.

## 7. Maintenance actions

Every maintenance action should have a tool and a document. The table says
what exists today. "Tool" is a dnvctl command, a gateway call, what the
worker does by itself, or a `dnv-agent` command an operator runs by hand on
a node. "Doc" names the document with the steps. The two
tutorials are [dnv on one machine](tutorial_all_in_one.md) and
[dnv under Kubernetes on one machine](tutorial_csi.md). "Operations doc"
names [the operations doc](operations.md), the procedures an operator
follows by hand; the list at its top names each of its sections by
subject: where to start, the forced-start guide, nodes, the networks,
pools, volumes, the control plane, Kubernetes and hosts. Every row with no
tool or no doc carries its note. A row's state uses the words of the list
at the top of this document, or "done": the action has its tool and its
doc. Section 6.1 and the tables give some short steps; the section "Steps
known today" below says what three of the longer procedures rest on.

### Nodes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Add a disk node or a controller node | `dnvctl dn create`, `dnvctl cn create` | the tutorials | done |
| Choose a node's location: the machine's host name by default, or a location the operator names | `--location` of `dnvctl dn create` and `dnvctl cn create` | the operations doc, Choose a node's location | procedure written |
| Prepare a node: the kernel modules at boot, the udev rules on every node that keep the system's own md assembly, LVM and bcache off dnv's devices, the LVM and md settings that go with them, the network and the firewall, the agents' units and their start order | the kit in the release's `deploy/` folder: the module lists of `deploy/modules-load.d/`, the udev rules of `deploy/udev/`, the systemd units of `deploy/systemd/` and the firewall example `deploy/nftables/dnv-filter.nft`; `dnv-agent check dn`, `dnv-agent check cn`: the node check reports what a node lacks and how to fix it | [the operations doc, Prepare a node](operations.md#prepare-a-node), which leads to the udev rules and the two settings, the node check and the networks; the tutorials, for the udev rules | procedure written; the suites install the udev rules and run the node check, but load the modules by hand and start no agent from a unit. A check by hand on one lab guest, on 2026-11-22, set the guest up from an archive that `make release` built from the repository, whose `deploy/` holds the kit. After a reboot its modules were loaded, every unit ran, the node check passed, and a volume read back the data written before the reboot. The check ran before a later change of the controller node agent's unit, which now stops only the agent's main process. The firewall's proof is in [the operations doc's network section](operations.md#the-networks). |
| Gather a node's state for a bug report | `dnv-agent collect` | the operations doc, Gather a node's state for a bug report | done |
| Disable a node | `dnvctl dn set-disabled`, `dnvctl cn set-disabled` | [the operations doc, Disable a node](operations.md#disable-a-node), which says when to disable and what a disable does not do: a standby on a disabled controller node can still become the primary | procedure written; the e2e suite's ops case checks the refusals of new legs and new cntlrs on disabled nodes |
| Remove a node | `dnvctl dn delete`, `dnvctl cn delete`. Refused while a side or a cntlr is on it, and until the node's agent shows the node clean; `--force` deletes without asking the agent. | the operations doc, Remove a node | procedure written |
| Replace a dead disk node | leg repair moves its RAID1 legs; `dnvctl retired-leg delete` frees the retired ones; `dnvctl dn delete --force` once no side is on it | [the operations doc, Replace a dead disk node](operations.md#replace-a-dead-disk-node) | procedure written; the e2e suite's react case tests leg repair of a dead disk node's leg at short thresholds, and its copy case deletes a retired leg whose disk node is dead, on real agents; no test deletes a dead disk node with real agents. Fix later: a forced delete that also frees the sides of the node's retired legs. |
| Replace a dead controller node | the worker replaces its cntlrs after `cntlr_unhealthy`; `dnvctl cntlr create` and `dnvctl cntlr delete` by hand; `dnvctl cn delete --force` once no cntlr is on it | [the operations doc, Replace a dead controller node](operations.md#replace-a-dead-controller-node) | procedure written; the e2e suite's react case tests the failover and the cntlr replacement at short thresholds, and no test replaces a dead node's cntlrs by hand or deletes the node with real agents |
| Reuse a disk from an old node | none: stop the agent; remove what dnv still built over the disk; zero the disk's header block, where dnv records the disk; delete the agent's local files of disk nodes; start the agent; create the node | [the operations doc, Reuse a disk from an old node](operations.md#reuse-a-disk-from-an-old-node) | procedure written; not tested |
| See a node's live state and the cluster's capacity | `dnvctl dn inspect`, `dnvctl cn inspect`, `dnvctl cluster get-capacity`, which also says what pools of one shape can still get and which limit the cluster reaches first | the operations doc, [Read a node's state](operations.md#read-a-nodes-state) and [The cluster's capacity](operations.md#the-clusters-capacity) | procedure written |
| Compare a disk node's own free count with its record | `dnvctl dn inspect` shows the agent's count, `dnvctl dn get` the record's | [the operations doc, Read a node's state](operations.md#read-a-nodes-state) | procedure written: the counts are compared, never reconciled; the e2e suite checks that they agree at the end of every case but the upgrade case, and no suite makes them disagree |
| Restart a node's agent | none; the agent answers "starting" while it rebuilds, and the worker waits for it up to the start hold | the operations doc, [A planned restart of an agent](operations.md#a-planned-restart-of-an-agent) | procedure written; the e2e suite's react case restarts the primary's cn agent this way, with its host connected, and no test restarts a dn agent this way |

### Pools

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create and delete a pool | `dnvctl sp create`, `dnvctl sp delete` | the tutorials | done |
| Grow a slice | the worker grows it by itself; `dnvctl sp grow-slice` | the operations doc, A pool's space | procedure written |
| See how full a pool is and how far its volumes overcommit it | `dnvctl sp inspect` | the operations doc, A pool's space | procedure written |
| Change a pool's level | `dnvctl sp set-level` | the operations doc, [Change a pool's level](operations.md#change-a-pools-level), and [Repair a thin pool's metadata](operations.md#repair-a-thin-pools-metadata) | procedure written; the e2e suite steps the whole ladder down and back, and the cn suite repairs a thin pool's metadata at the agent level |
| Move a leg to another disk node | `dnvctl migr create`, `migr finish`, `migr cancel`; the worker reads and hands over the leg's list itself | the operations doc, [Move a leg to another disk node](operations.md#move-a-leg-to-another-disk-node); the introduction explains the flow | procedure written; the e2e suite runs a migration to its finish and a cancel with force after a cutover; a forced end of a migration whose disk node is gone is not tested as a whole |
| Repair a leg by hand | `dnvctl repair create`, `repair finish`, `repair cancel`, `repair abort`; the worker repairs by itself | the operations doc, [Repair a leg by hand](operations.md#repair-a-leg-by-hand) | procedure written; the e2e suite runs two repairs of an operator's on one group |
| Force-start a group | `dnvctl group inspect` shows what the primary reads from each member, and with `--leg` says what a start from that leg would do or why it would refuse; `dnvctl group force-start`, `dnvctl group set-operator-in-charge` | [the forced-start guide](operations.md#the-forced-start-guide) | procedure written |
| Delete or keep a retired leg | `dnvctl retired-leg list`, `dnvctl retired-leg delete`, `dnvctl retired-leg keep`; a forced start keeps the group's retired legs by itself, and an operator's repair of a leg the primary keeps out for another identity keeps that leg once the repair finishes | [the forced-start guide](operations.md#the-forced-start-guide), "Keeping the old copies", for a forced start, [a leg of another identity that the primary does not add](operations.md#a-leg-of-another-identity-that-the-primary-does-not-add) for that repair, and [Retired legs](operations.md#retired-legs) for the others | procedure written |
| Replace a cntlr by hand | `dnvctl cntlr set-enabled`, `dnvctl cntlr delete`, `dnvctl cntlr create` | the operations doc, [Replace a cntlr by hand](operations.md#replace-a-cntlr-by-hand) | procedure written; the e2e suite makes, disables and deletes a cntlr, and disables a primary and enables it again, but runs no whole replacement |
| Change a pool's cntlid slots, the ranges of NVMe controller ids its exports use | `dnvctl sp set-cntlid-slots` | the operations doc, [Change a pool's cntlid slots](operations.md#change-a-pools-cntlid-slots) | procedure written |
| Change a pool's thresholds | none | — | not yet |

### Volumes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create, snapshot and delete a volume | `dnvctl td create`, with `--ori` for a snapshot; `dnvctl td delete` | the tutorials, for the create and the delete; the operations doc, [Snapshots](operations.md#snapshots) | done; procedure written for snapshots, which the e2e suite makes and waits for until they are recorded as created |
| Export a volume to hosts; change its hosts | `dnvctl ss create`, `dnvctl ns create`, `dnvctl ss set-hosts`; a host taken off the list loses its connection where the controller node's cn agent ends connections (section 5.4) | the tutorials, for the export; the operations doc, [Change a volume's hosts](operations.md#change-a-volumes-hosts) | done |
| Park a namespace | `dnvctl ns set-suspended`, `dnvctl ns set-dev` | the operations doc, [Park a namespace](operations.md#park-a-namespace) | procedure written; the e2e suite parks a namespace, resumes it, and points it at another volume and back |
| Move a volume to another pool | `dnvctl xfer create` on the source; `dnvctl clone create`, `clone append-bm` on the destination | the introduction explains the flow; the operations doc, [Move a volume to another pool](operations.md#move-a-volume-to-another-pool), gives the whole flow, and A pool made before the block bound the order of the moves out of a pool | procedure written. The e2e suite runs a transfer and a clone within one pool, into a namespace of another identity; the move into another pool under the same identity is not tested. |
| Clone from a source outside this cluster | `dnvctl clone create`, `clone set-tr` | the introduction explains the flow; the operations doc, [Clone from another cluster](operations.md#clone-from-another-cluster) | procedure written; not tested from another cluster or from a target that is not dnv. The e2e suite clones a plain namespace of a second pool of the same cluster, with no transfer. |
| Read a volume's written blocks | `dnvctl td get-bm` | the operations doc, [Read a volume's written blocks](operations.md#read-a-volumes-written-blocks) | procedure written; the e2e suite reads the list of a parked volume and of a snapshot. Reads in windows through `dnvctl` are not tested. |
| Grow a volume; revert a volume to a snapshot | none | — | not yet |

### Control plane

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Set up the control plane: etcd with the request limits dnv needs, the gateways, the workers and the cdcs | the kit in the release's `deploy/` folder: the units of `deploy/systemd/`, with an example unit for etcd, and the examples of settings in `deploy/dnv/` | the operations doc, [Set up the control plane](operations.md#set-up-the-control-plane) | procedure written; a check by hand on one lab guest, on 2026-11-22, ran etcd as one member and the gateway, the worker and the cdc from their units, through a reboot. An etcd of several members started from the example unit, and a control plane on machines of its own, are not tested. When every program of a cluster starts, as after a power loss of the whole cluster, the workers must start after every agent: a worker that starts first, once its `--vote-grace-time` has run out, sees a node whose agent does not answer yet as failed. Units enabled at boot are safe on one machine alone: there systemd starts the worker after the agents, and a new worker drives nothing for its `--vote-grace-time` and waits up to 120 seconds at the default, counted from the agent's start, for an agent that answers that it is starting. Across several machines nothing orders the units. On a cluster of several machines, leave the workers' units off at boot, and start the workers once every agent has logged `agent started` ([Prepare a node](operations.md#prepare-a-node), the start order); no program checks the order. |
| Back up and restore etcd | etcd's own tools; `dnvctl cluster advance-ids` after a restore; `dnvctl cn check-restore`, which says pool by pool what the restore loses, before the agents' local files are reset | the operations doc, [Back up etcd](operations.md#back-up-etcd), [Restore etcd from a backup](operations.md#restore-etcd-from-a-backup), and [a new volume refused for a thin id that holds data](operations.md#a-new-volume-refused-for-a-thin-id-that-holds-data) | procedure written; the e2e suite's upgrade case takes a backup under 0.1 and runs the restore's steps 1 to 6 from it, step 2 without its reads of each etcd server alone and its check of the cluster names, and of step 7 the start of the workers and the cdc and the new backup, once on the lab's real agents, with one etcd member and no Kubernetes; it has no host list to change, and step 8 does not run. A backup schedule, a backup taken from one of several etcd servers, a restore of several etcd servers or after a lost quorum, a restore from a server's own data directory, and a restore into a data directory that is the mount point of a file system of its own are not tested. The check does not see a pool none of whose controller nodes answers, and lists no disk node's sides. A new volume over a restored thin id that holds data is refused. |
| Recover an etcd that refuses writes because its store is full | etcd's own tools: `etcdctl compact --physical`, `etcdctl defrag --cluster`, `etcdctl alarm disarm` | the operations doc, [etcd refuses writes: its store is full](operations.md#etcd-refuses-writes-its-store-is-full) | procedure written; the worker suite's three-member mode runs the compaction, the defragmentation and the disarm on the lab, over a small store. The raise of the quota is not tested. |
| Upgrade | every program at once; `--version` on each program, and the version each running program logs when it starts | the operations doc, [Upgrade dnv](operations.md#upgrade-dnv) | procedure written; the e2e suite's upgrade case upgrades a cluster of 0.1 in lockstep in the order of steps 2 to 11 on the lab, under a host's writes, with no failed write and no reaction, and runs parts of steps 1 and 12. The rest of steps 1 and 12, the units of step 5 and the CSI driver's part are not tested. The case runs its programs from a work folder, not from units, so the `systemctl` commands of the steps are not tested either. Rolling upgrades are not yet. A downgrade is not supported. |
| Run while the control plane is down | nothing to do, since the data plane serves; on a host where nvme-stas dropped every path of a volume, unmount its file system and mount it again on the new device, and check it with its own check tool only when the mount or the kernel log reports an error | the operations doc, [While the control plane is down](operations.md#while-the-control-plane-is-down) | procedure written; the e2e suite stops etcd under a host's writes, at shorter vote times than the shipped ones; the cdc suite stops every cdc for a few seconds only, and a longer outage of every cdc, with the mount again on a host whose paths nvme-stas dropped, is not tested |
| Handle the logs | `--log-level` on every program; SIGUSR1 and SIGUSR2 to a running program; JSON on stderr, no rotation | [the operations doc](operations.md#the-logs) | procedure written, with [the steps to read them](operations.md#read-the-logs). |
| See what is wrong across the cluster | `dnvctl cluster get-status` | the operations doc, Read what is wrong | done |
| Give hosts the cdc addresses; front the gateways | none | the tutorials, for one host; the operations doc, [Give hosts the cdc addresses](operations.md#give-hosts-the-cdc-addresses) | procedure written; the cdc suite runs hosts that list several cdcs. The move or the removal of a cdc, with the change of each host's list, and a front over several gateways are not tested, and no suite loads a host's kernel modules from the kit's file. |
| Change the check intervals or a node's budget | none | — | not yet |

### Hosts

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Connect a host with nvme-stas | nvme-stas | the all-in-one tutorial | done |
| Connect a Kubernetes cluster | the CSI driver | the CSI tutorial | done |
| Restart nvme-stas | none | [the operations doc](operations.md#restart-nvme-stas); [dnv on one machine](tutorial_all_in_one.md) | procedure written: stop stacd first, restart stafd, and start stacd once stafd holds its connection to each cdc again and their discovery logs name every subsystem the host uses, or, with a cdc away, once the cdcs it reaches still list every path the host must keep; not tested |
| Recover a host that keeps a withdrawn path | none; `nvme connect` to the listed cntlrs, then `nvme disconnect -d` of the withdrawn path, or, for a cntlr that was replaced, the drop of the old path first and then the connect (a drop of the host's only path fails the IO that waits on it and loses writes not yet flushed); a stale cdc's link to etcd repaired; a controller taken out of `/etc/stas/stacd.conf` and stacd restarted; a stopped stacd started again as Restart nvme-stas says | [the operations doc](operations.md#a-host-that-keeps-a-withdrawn-path) | procedure written; the e2e suite's cutoff case drops a path connected by hand and connects the new primary, on a host that holds one path; the connect before the drop, the drop by device, the drop of a replaced cntlr's path, a controller named in stacd's configuration, a stopped stacd and a stale cdc are not tested |
| Recover a Kubernetes mount after the loss of its last path | none; `nvme disconnect -n` for a pod whose IO waits; for a filesystem volume, its pods stopped until the node holds nothing of the volume, then started again; for a raw block volume, a delete of the pod | [the operations doc](operations.md#a-kubernetes-mount-that-lost-its-last-path) | procedure written; not tested; accepted, and no fix is planned |
| Change the cdc addresses of the Kubernetes driver | the config map, the Kubernetes record that holds the cdc addresses both plugins start with, and a restart of both plugins | the operations doc, Change the cdc addresses of the Kubernetes driver | procedure written; the Kubernetes suite changes the list of the controller plugin, the part of the driver that calls the gateway, alone, and no suite restarts the node plugins with a new list |
| Start a Kubernetes node plugin that refuses to start | none; the plugin's log names the fact and its fix | the operations doc, A Kubernetes node plugin that does not start | procedure written; not tested in a suite |
| Keep a Kubernetes cluster's pods off dnv | the example network policy, [`deploy/networkpolicy.yaml`](../../deploy/networkpolicy.yaml) | [the CSI tutorial](tutorial_csi.md#keep-other-pods-off-dnv) | procedure written; the Kubernetes suite's policy case tries the ports of the gateway, the cdc and two agents, not the NVMe/TCP ports, which listen only once a pool uses them |
| Handle the loss of a Kubernetes machine | the controller plugin's second copy; the out-of-service mark of Kubernetes, which an operator puts on a machine that is gone so that Kubernetes moves its volumes | the operations doc, A Kubernetes machine is lost | procedure written; the Kubernetes suite checks the controller plugin's part, and its detach case the move of a pod's volume after the out-of-service mark, of a machine cut off from Kubernetes but not from the controller nodes, on controller nodes that end its connections; both passed in the lab on 2026-11-20 |
| Recover a one-cntlr Kubernetes volume after a cntlr replacement | `nvme disconnect -d` of the old path, then, for a filesystem volume, its pods stopped until the node holds nothing of the volume and started again, or, for a raw block volume, a delete of the pod | the operations doc, [A one-cntlr Kubernetes volume after a cntlr replacement](operations.md#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement) | procedure written; not tested |
| Recover a Kubernetes volume the node plugin will not format | none; the plugin formats only a volume whose first MiB reads as zeros; for data at the start, an operator reaches the volume through a pod that gets it as a raw block device, and zeroes the start by hand for a volume that never held data | the operations doc, A Kubernetes volume the node plugin will not format | procedure written; its steps for data at the start are not tested |
| Recover from a fault only a host sees | disable each enabled standby with no health epoch that the hosts cannot reach, then the primary, but the primary only while an enabled standby with no health epoch that the hosts reach is left; if none is left, enable again each standby you disabled and repair the network; if no other cntlr takes the role within a few check rounds, enable the primary again at once; if another cntlr takes the role but a minute later has still not reported its devices built, as `settling` true in `dnvctl sp get` shows, keep the old primary disabled while the hosts cannot reach it, disable the new primary only while another enabled standby with no health epoch that the hosts reach is left, and otherwise repair the network first, then enable the old primary and, once it shows no health epoch and the hosts show its path live, disable the new primary; after a failover, enable each disabled cntlr again once the hosts reach it | [the operations doc](operations.md#a-fault-only-a-host-sees) | procedure written; the worker suite's reaction case runs the disable on fake agents, and the e2e suite's copy case the disable and the enable on the lab's agents; a failover past a disabled standby runs in a unit test of the worker alone, and the fault itself is not tested. Fix later. |

### Steps known today

These are three longer procedures and what each rests on. [The
operations doc](operations.md) gives their full steps.

**Back up and restore etcd.** The steps are in the operations doc:
[back up etcd](operations.md#back-up-etcd), [restore etcd from a
backup](operations.md#restore-etcd-from-a-backup) and [restore one record
from a backup](operations.md#restore-one-record-from-a-backup). A restore
brings the desired state back to the backup's time, so what was created
after it is lost with its data, and a pool whose layout changed since can
break. The suites and the tutorials run etcd as one member. The worker
suite also has a three-member mode, which stops one member, compacts the
store and fills it to its quota, but it backs up and restores nothing, so a
backup and restore of an etcd of several members is untested.

**Upgrade.** Upgrades are lockstep. Every dnv program of one etcd
installation stops: its gateways, workers and cdc, the agents of every
cluster it holds, and every CSI driver that talks to its gateways. Every
binary is replaced, `dnvctl` included, and only then does any program start
again. The workers stop first and start last, and the new udev rules go on
every node before its new agents start. There is no rolling upgrade, since
no message carries a build version, and two builds that differ in a message
would misread each other with no error. No build may replace a newer one:
section 5.5 says why. [The operations doc](operations.md#upgrade-dnv) gives
the steps, with those of an upgrade from 0.1.

**Logs.** The programs log to stderr as JSON, one record per line, with no
rotation. Each program takes `--log-level`, the log level: the least serious
kind of record it writes, debug, info or warn. A running gateway, worker,
cdc, agent or CSI driver writes debug records from a SIGUSR1 on and goes
back to the log level it started with on a SIGUSR2. Keep the logs on a
filesystem apart from the agents' local stores and etcd's data, and rotate
them yourself: a disk the logs fill is one the stores and etcd cannot
write, and an agent whose store cannot be written refuses every syncup, a
failover's among them.
[The operations doc](operations.md#the-logs) gives the steps.

### Notes on the maintenance tools

- **Part of md's behaviour is tested in the lab only.** Failover safety
  and the forced start rest on mdadm's and the kernel's md behaviour,
  tested on the lab's. On a node, the node check (`dnv-agent check`)
  reports a kernel outside the tested 7.0 series and, on a controller
  node, an mdadm other than the tested 4.5; on a spare machine with a
  node's mdadm and kernel, its md replay (`dnv-agent check cn
  --md-replay`) runs md's refusal of a lone clean leg, the start of the
  survivor, the pick of the newer leg and a forced start
  ([operations](operations.md#check-a-node-before-it-joins)). Each agent
  logs the kernel release when it starts, and a controller node's agent
  also the mdadm version. The order of fences of section 5.1 also rests on
  the kernel's md: it completes a write that landed on one leg and failed
  on the other, and when the legs' records read equal, the start of the
  array copies the leg in its first place over the others. A script,
  `integtest/md_fence_order.sh`, checks this by hand on one lab VM's
  kernel; no suite runs it, and the md replay does not, since it needs a
  write in flight at the fence and does not show it on every run. Nor does
  anything check which part of a leg a re-add copies. State: accepted.
- **The node check counts only dnv's own md rule.** It counts a mask of
  the system's md assembly in two forms alone: dnv's rule
  `63-dnv-md.rules` exactly as shipped, or mdadm.conf's AUTO line refusing
  every format, as `AUTO -all` does, with no `homehost` on it, no POLICY
  line that sets `auto=`, no ARRAY line of a dnv array, and no ARRAY line
  that names devices (`devices=` or `super-minor=`) but no uuid; it reads
  an ARRAY line's device name as mdadm does, from the first word that
  starts with "/" or holds no "=". A rule
  of the node's own, or another mdadm.conf, that keeps md off dnv's
  devices fails the check all the same, since a rule cannot be judged by
  its text: udev's order of a line's parts, how it finds the program and
  what it puts in for its names all come between the text and what runs.
  The shipped rule rests on the system's own md rule skipping a device it
  marks not ready and coming after it, as the lab's Ubuntu ships that
  rule; the check does not read it. State: accepted.
- **Retired legs keep their extents until deleted.** A group never back to
  whole keeps them for good. At the bound of two, an operator's repair
  with a fresh leg is refused. State: procedure written,
  [Retired legs](operations.md#retired-legs): when to keep one, and how to
  delete it.
- **A forced clone delete returns before the teardown is done.** Success
  means the teardown started. The operator polls the clone and the pool.
  State: accepted, by the dnvctl design. Procedure written: [the operations
  doc](operations.md#move-a-volume-to-another-pool) deletes the
  destination's namespaces first and polls the clone until it is gone.

## 8. Waits a host can see

- **A detach of the Kubernetes driver, and the attach that follows it, wait
  until the primary has ended the old host's connection.** A detach takes a
  Kubernetes node off a volume's list of hosts, and then waits until the cn
  agent of the volume's primary reports the row of the volume's subsystem ok at
  that change's revision or a later one, which it does once no host off the
  list keeps a connection to the subsystem there: one converge of the primary,
  a second or two in the common case. An attach that puts a node on the empty
  list waits for that report at the revision it read before it writes the node,
  and reads again when the pool's revision moves on: Kubernetes, once a detach
  call has failed, may already ask to attach the volume to another node, and
  that attach is what keeps the other node out until the old host's connection
  has ended. A host that connects just as it leaves the list can show its
  connection only after that converge has looked: the detach and the attach
  then end while that host is still connected. The converge that the attach's
  write brings lets the new node in and ends that connection right after. With
  no attach, a later check round finds the connection, and the converge it
  brings ends it (section 5.4). Kubernetes gives each call 15 seconds, then
  tries it again, with a pause between tries that grows from one second up to
  five minutes; the driver bounds a call to two minutes, or to its caller's
  limit when that is shorter. A primary whose cn agent does not answer, or
  whose connection to the old host does not end, holds both until the cn agent
  answers, the connection ends or another controller node becomes the primary,
  so the volume's new pod waits. A connection that does not end reads pending
  and starts no failover, except under a lockdown, the mode Secure Boot
  switches on, on a node where /sys/kernel/security is mounted, which reports
  an error as a node that cannot read its connections does. The worker then
  fails the primary over where the pool has an enabled standby with no health
  epoch; a pool with no such standby, such as a pool with one cntlr, waits for
  the cntlr replacement, which comes after the pool's `cntlr_unhealthy`, 600
  seconds at the default. Where /sys/kernel/security is not mounted, the cn
  agent cannot see the lockdown, and the row reads pending. An operator who
  disables the primary's cntlr moves the primary to such a standby, where the
  pool has one ([the operations
  doc](operations.md#a-kubernetes-machine-is-lost)). Where the primary's cn
  agent started with `--ignore-host-disconnection` and found one of the causes
  that the `nvmet host cut (debugfs)` line of `dnv-agent check cn` names ([the
  operations doc](operations.md#check-a-node-before-it-joins)), both waits end
  at its first report at that revision and end no connection (section 5.4).
  State: accepted: the attach's wait keeps a second machine out until the
  primary has ended the old machine's connection, and the detach's wait makes a
  finished detach mean that the old machine's connection on the primary is
  gone. An attach that did not wait would let both machines write at once, and
  a new pod that waits loses no data. The Kubernetes suite's detach case checks
  both waits, with the worker stopped so that no cn agent converges, and passed
  in the lab on 2026-11-20.
- **A cntlr or a cdc that does not answer holds up a Kubernetes node.** The
  node plugin makes its connects and its reads of the cdcs' answers one at a
  time and bounds each by a few seconds, but no signal ends some waits in
  the kernel: the first command of a new connection, a read of a cdc's
  answer on a connection that stays open, and the close of a connection to
  a cdc that does not answer. A cntlr or a cdc that keeps its connection
  open and does not answer holds up every new volume and every path change
  on that node for about a minute, or a little more. A cdc whose machine
  stops costs each Kubernetes node one such wait, until the kernel finds the
  connection dead: 45 to 60 seconds on the Ubuntu kernels dnv is tested on,
  which wait half a keep-alive period before they end a dead connection, and
  30 to 45 seconds on a kernel without that delay. A cdc that still answers
  the kernel's keep-alive messages and not the read costs the kernel's admin
  timeout, 60 seconds at its default, plus that delay of 15 seconds, on each
  try. A cntlr that does not answer the first command of a connection costs
  up to the admin timeout. Not seen in the lab. State: accepted.
- **A pool commit in a migration gap waits for the gap to end.** The gap is
  the time with no usable path from the primary to the moving leg. The gap
  ends by the disk nodes' steps or by a forced cancel. Measured at 32 slices: one
  to two and a half seconds. When the new side cannot connect to the old
  side at the cutover, the gap lasts until a later syncup of the new side
  connects, and the worker sends that syncup again at its next check
  round. Two faults stretch the wait. While the new side's disk node
  cannot save to its store disk, the disk that holds the agent's local
  store, the files in which it keeps the desired state it last accepted,
  the agent refuses every syncup, so the gap lasts as long as the fault,
  unless the agent restarts. While no worker drives the pool, as before a
  new worker takes it over, the gap lasts until that worker's first check
  round. A destination that waits for free metadata units waits the same
  way under both faults. State: accepted.
- **A migrating leg whose serving disk node dies waits until that node
  comes back in a pool without RAID, and until that node comes back or
  leg repair ends the migration in a RAID1 group with no other leg in
  sync. One whose
  destination dies after the cutover, before its copy is whole, keeps its
  group one leg short, or, with no other leg in sync, makes its IO wait.** A
  migrating leg has two paths, and the
  one that does not serve is live but ANA inaccessible. The kernel keeps
  the leg's IO queued as long as that path stays live. When the serving
  disk node dies, the primary cuts that other path while another leg of
  the group is in sync. Within seconds md fails the leg, and the write
  goes to the group's other legs. Leg repair then ends the migration in
  the direction that loses nothing: a cancel before the cutover, or a
  finish after it when the destination's copy is whole. Then it repairs
  the leg. After the cutover with a copy not whole, neither side alone
  holds the leg's data, so the worker does neither and logs why. The group
  serves on its other legs until the destination comes back. The primary
  then puts the leg back into the array, and the copy goes on to its end.
  Before that, an operator can end the migration with force, as
  [the operations doc](operations.md#ending-a-migration-with-force) says.
  Where no other leg is in
  sync, as in a pool without RAID, the primary does not cut the path,
  since the write would fail, and the IO waits. In a pool without RAID it
  waits until the disk node comes back: the worker ends no migration
  there. In a RAID1 group it waits until then, or until leg repair ends
  the migration in one of the two directions above, after the pool's
  `side_unhealthy`, 600 seconds at the default, or its `leg_unhealthy`,
  1200, when only the network to that disk node is cut. Both ends keep the
  side on that disk node, so the controller node drops its path to the
  other side, and the waiting IO fails. md then marks the group's array
  broken, since that leg was its last member in sync, and the broken
  array makes the worker fail the pool over where the pool has an enabled
  standby with no health epoch whose agent does not refuse the worker's
  syncups, a pause for every volume of the pool; a pool with no such
  standby, such as a pool of one cntlr, waits for the cntlr replacement.
  No repair follows,
  since no other leg of the group is in
  sync: the group waits until the primary reaches that disk node again,
  or for a forced start, and a broken data group can leave the thin pool
  read-only, as in a switch reload (section 6). Leg repair ends nothing
  after a cutover whose copy the worker has not seen whole, as after a
  restart of the worker while the new side's disk node is down, nor while
  the group has the operator-in-charge flag or a forced-start mark, nor
  at the pool's level `NO_THINPOOL` or below; the IO then waits until the
  disk node comes back. When only the network from the controller node
  to the new side's disk node is cut, the copy goes on over the path
  between the two disk nodes, so it can become whole during the cut; leg repair
  then finishes the migration, and the IO fails. In both kinds of pool, the
  worker can also fail the pool over before any of this: while the write
  waits, a commit of the slice's thin pool can wait on the leg too, and
  the primary then reports nothing new to the worker. All of this is read
  from the code: of it, the lab saw only the waiting write, below.
  State: accepted; the group left one leg short after a cutover with
  a copy not whole is fix later. Seen in the lab: in a RAID1 pool, after the
  cutover with the copy whole, all traffic to the NVMe-oF port of the
  serving disk node was dropped. The primary cut the other path, md
  failed the leg, host writes went on and none failed, and the worker
  finished the migration and repaired the leg. The waiting write was seen in the lab
  too, and a disconnect by hand ended it.
- **The failover pause is the full stack build on the new primary.** A
  standby holds no array and no thin pool. The write in flight waits until
  the new primary has built the whole stack. The figures are in the
  introduction. Before the build, the worker hands each group's leg md
  copies from its fence last (section 5.1), which adds the time the
  slowest disk node takes to report its fence, at most one interval, 2
  seconds at the default. A new primary can start its build before the
  disk nodes have set the legs' paths to it ANA optimized. It then builds
  its groups in the converge of a later syncup, which the worker sends
  once a check round made after that change names the groups: up to about
  two intervals more, 4 seconds at the default, plus the time of that
  check round's read and of one more build. State: accepted. Fix later,
  large: a standby that pre-assembles its arrays.
- **Hosts still on the old primary's path see IO errors from the fence.**
  The worker hands the old primary its demotion first and holds the sides
  for three seconds at most. It holds the sides of the legs md copies from
  up to one interval more, 2 seconds at the default (section 5.1). The
  three seconds are the time a host that follows the discovery log needs
  to drop the path. When the old primary's agent reports its demotion
  before the host drops the path, the fence can come first, but the path
  is then ANA inaccessible and carries no IO.
  The failover writes the demotion into the old cntlr's record, so a
  worker that restarts or takes the pool over during the failover holds
  the sides too. A cntlr replacement of the primary holds the sides the
  same way, so the old controller node usually tears its stack down
  first. Three windows remain, all accepted
  for their IO errors; in each, a write the host was told was done can
  also be lost, in the cases section 5.1 lists. A slow old primary misses the hold. A
  replaced primary whose controller node takes longer than the hold to
  tear its stack down is fenced while it still serves.
  A worker that restarts or takes the pool over within those three
  seconds after a replacement of the primary does not hold the sides,
  since the replaced cntlr leaves no record. What stays besides: a
  Kubernetes host on an unreachable old node sees errors until the new
  path shows the volume, and can lose a write the same way. The e2e suite
  shows the first window, with an old primary whose agent is stopped; the
  other windows and the Kubernetes case are not seen in the lab. A read
  through the old path can do worse than fail. After the fence the old
  primary still runs its thin pools with the records they had, and a thin
  pool answers a read of a block it never mapped with zeros, with no leg
  needed. A block first written through the new primary is such a block,
  so a host still on the old path can read zeros, with success, where it
  wrote. This needs a thin pool that had no change waiting for its commit
  at the fence; a thin pool with one fails every read. For an old primary
  whose agent is stopped it can last for as long as that thin pool makes no
  commit. Read from dnv's code and the kernel's, not seen in the lab; the
  state of these reads is fix decided.
- **A failover while the disk nodes are still opening a pool's paths can
  leave the pool unbuilt.** When the new primary builds while the disk nodes
  are still opening its paths, it starts a group's RAID1 array only once both
  copies are reachable, and on a busy controller node that start can take
  longer than the 3 seconds the node's agent gives any command, which then
  stops it. Stopped after it has taken in one copy and before the array has
  started, the array stays inactive with that copy, and nothing starts it
  again: the pool serves nothing. By dnv's rules the worker then
  fails the pool over to its standby once the error has lasted the cntlr
  threshold, 600 seconds at the default. The run that met it had raised that
  threshold to 1800 seconds and gave up its wait after 1200 seconds, so no run
  has reached that failover. The e2e suite's many-pools case saw it once, in
  one of three builds of 64 pools at once, on a pool that had finished
  building and then failed over twice while the other pools still built, the
  second time while the disk nodes were still opening its paths. State: fix
  later; not decided yet.
- **Creating many volumes at once can fail over volumes that are already
  built.** A controller node that builds many new primaries at once can
  answer the worker's check rounds of the primaries it has finished with
  reports that are too old, which the worker counts as missed rounds, and
  the worker then fails those primaries over, so their hosts see a pause
  with no fault behind it. The e2e suite's many-pools case shows it on three
  controller nodes at the default primary threshold, with the other
  thresholds raised: 22 and 18 failovers in the two builds of 64 pools
  that finished, one pool failing over twice in a third, one in one of two
  builds of 32 and none at 8, each of a pool that had finished building,
  and in one build the pool made before the others
  ([the introduction](introduction.md#status-and-limits) has the
  numbers). A Kubernetes cluster that creates many volumes at once has this
  shape. State: fix later; whether the threshold or the build should change
  is not decided yet.
- **A controller node that comes back after a network cut fails a host's
  writes for a few seconds.** While the node is cut off, the failover moves
  its primaries to other controller nodes and the disk nodes fence its
  legs, but the node hears nothing of this, so it keeps telling hosts that
  it serves those volumes. Once the network is back, the worker's next
  message turns each of them into a standby. A host whose connection to the
  node comes back before that message sends its writes there, and they fail.
  No write the host was told was done is lost, but a read there of a block
  first written through the new primary can return zeros, with success,
  as the entry "Hosts still on the old primary's path see IO errors from
  the fence" says; the state of those reads is fix decided. The e2e suite's many-pools
  case shows it at 32 and 64 pools
  ([the introduction](introduction.md#status-and-limits) has the
  numbers). State: fix later; the way is not decided yet.
- **A node clock that steps back delays a failover.** The worker reads how
  old an agent's report is by the node's clock. After the clock steps
  back, reports read younger by up to the step, so a node whose reads hang
  is taken for unreachable up to the step later. State: accepted. Not seen
  in the lab.
- **Reactions read a state the primary reported a while ago.** The
  primary's last answer is at most one interval old, and the state it
  reports can be up to three intervals older still: at most four
  intervals, 8 seconds at the default, and more just after a pause in the
  rounds. A failover, a cntlr replacement or a grow can follow a row that
  has changed since, or be refused by one. The worker can delete a
  retired leg of a group that turned degraded after that report. State:
  accepted, by design. Not
  seen in the lab.
- **One slow or stuck command holds the agent's other work.** A command in
  an uninterruptible kernel wait outlives every timeout. The agent's
  reports age, and the worker can count a missed round. The agent's read
  of a leg's md superblock, made for a check round, can wait this way when
  the leg's path stops serving between the test that found it serving and
  the read. State: accepted; the waits are the kernel's. Not seen in the
  lab.
- **A controller node's build that stopped waits while no worker drives
  the pool, and while the node's store disk refuses writes.** The agent
  converges only when a syncup comes, and once at its own start. What a
  converge could not build waits for the syncup the worker sends again
  once a check round names it: a new primary's stack after a failover, a
  leg to put back into its array, a forced start, a clone's build. While
  no worker drives the pool, as before a new worker takes it over, that
  wait lasts until the new worker's first check rounds. While the
  controller node cannot save to its store disk, the disk that holds the
  agent's local files, the agent refuses every syncup, so the wait lasts
  as long as the fault, unless the agent restarts; a restart carries out
  no forced start. State: accepted; a store disk that refuses writes
  already stops every change of the node. Not seen in the lab.
- **A migration destination waits for a run of free metadata units the
  count allowed.** The gateway counts free units, not runs. While it
  waits, the destination does no work of its own. Once a run comes free,
  a check round soon after reports the side as not zeroing, and the
  converge of the worker's syncup that follows takes the run. The source
  keeps serving, and a cancel frees the units. State: accepted. Counting
  runs would need a map of the units in etcd that every allocation and
  release keeps. Not seen in the lab.
- **Other waits, some of which only the operator ends.** An agent that
  marks the regions of a piece of a written-block list holds that
  object's other work, and while the copy serves pauses the volume's IO,
  until the marks are done; a copy still in flight first holds the pause
  for a short bound. No read of that object runs until the marks are
  done, even when no syncup of the node comes, so its reports age: once
  the marks last past three check intervals, 6 seconds at the default,
  the worker counts the object's check rounds as missed and sets its
  health epoch, and a primary cntlr can then fail over. A syncup of the
  node itself that comes meanwhile waits for the marks too, and holds
  every other syncup and piece of that node behind it. No new read of the
  node's state starts while it waits, so the reports of every object of
  the node age the same way, and on a controller node the node's
  primaries can fail over. Read from the code; not seen in the lab.
  A grow whose new group cannot get ready waits while a repair, a retired
  leg, a forced-start mark, the operator-in-charge flag or a migration
  acts on that group. The worker ends its own repairs, and clears a
  forced-start mark once the primary reports the start done. Four of these
  holds can last until the operator acts:
  - an operator's repair, which the worker never drives, until the
    operator finishes or cancels it;
  - the operator-in-charge flag, which only the operator clears, with
    `dnvctl group set-operator-in-charge --grp <group id>
    --operator-in-charge=false` ([Taking the group's repairs
    over](operations.md#taking-the-groups-repairs-over));
  - a migration, until the operator finishes or cancels it; the worker
    ends one only when its leg goes bad;
  - a retired leg, until the operator deletes it with
    `dnvctl retired-leg delete --leg <leg id>` ([Retired
    legs](operations.md#retired-legs)). The worker's own delete of a
    retired leg after seven days needs the leg not kept, its group whole
    and no health epoch on a leg or side of the group; a group whose new
    side is dead carries one. `dnvctl retired-leg keep
    --leg <leg id>` keeps a leg, and a forced-start mark keeps every
    retired leg of its group.

  A fifth hold, a forced-start mark whose start waits or is refused, can
  last until the operator acts too: it holds the grow until what holds the
  start ends, which can take the operator ([A forced start that
  waits](operations.md#a-forced-start-that-waits)), or until the operator
  withdraws the mark with
  `dnvctl group force-start --grp <group id> --cancel`.
  Outside a failover and a cntlr replacement of the primary, the worker's
  hold delays the cntlrs' syncups by up to one check interval. In those two,
  the cntlrs but the old primary also wait for its demotion, three seconds
  at most, and for the other legs' disk nodes to report their fence, one
  more interval at most (the failover entries above). A repair waits while
  its array does not run, until the array runs again; that can take the
  operator's forced start or level step ([the forced-start
  guide](operations.md#the-forced-start-guide)). A repair whose bad leg
  still serves in its array waits until it is aborted: an operator's by the
  operator, and a worker's by the worker once the leg reads healthy again. A
  clone source the agent reads as unused can be cut under a new dm-clone
  until the next converge. The read-only level reaches a controller node
  only with its converge. The cdc answers nothing before its first etcd
  scan, which waits as long as etcd does not answer, with the record
  `cdc waiting for first scan` once a minute; it delays hosts under a flood
  of new connections, and serves old addresses through an etcd outage.
  State: accepted. Each wait above that the operator ends stops at the step
  named there; the others end once what they wait for is done or answers
  again. The cdc's first scan and its old addresses last as long as etcd
  does not answer, and an etcd that does not come back takes the
  operator's restore ([Restore etcd from a
  backup](operations.md#restore-etcd-from-a-backup)); its delay under a
  flood of new connections ends with the flood.
- **Disabling the last enabled cntlr stops the pool's IO with no
  warning.** State: accepted, by the dnvctl design.

## 9. What cannot be seen

- **No metrics, no alerts.** The logs are the only alert: the records at the
  warn and error log levels are the ones an operator must look at. The logs
  go to stderr as JSON with no rotation. The status read, `dnvctl cluster
  get-status`, shows what is wrong when a person asks: error rows, health
  epochs, an auto-grow that cannot run, a full thin pool, a repair, a
  migration or a clone that waits, a lost record and a worker that seems
  gone ([the operations doc](operations.md#read-what-is-wrong)). A
  subsystem no cdc serves and a frozen worker watch still show in a log
  line or not at all. A slow leg shows only when a person asks:
  `dnvctl cntlr inspect` prints each leg's last test write and read times
  and the IO counters of each leg and of each namespace's device, and
  `dnvctl dn inspect` the counters of a disk node's disk, read when asked;
  nothing keeps them or alerts on them. A disk's own health is for the usual
  tools of the node, such as smartd and node_exporter. The worker's log
  records the reactions it applied and the ones it skipped. No dnv program
  serves metrics, profiles or health over a port, though the CSI driver's
  manifests add a helper container that answers Kubernetes' health checks:
  on port 9808 at the address of each controller plugin copy's own pod, and
  on port 9809 on each Kubernetes node's loopback alone. State: not yet; log
  handling is in [the operations doc](operations.md#the-logs).
- **The status read sees what etcd and the primaries hold.** The status read
  asks each pool's primary and a migration's new side, and no standby and no
  other side. A standby's or a side's own trouble shows only once the worker
  sets its health epoch; the inspects show the rest. A failover of an
  unhealthy primary with no enabled standby to take the role, a leg repair
  the worker cannot create and a cntlr replacement that finds no controller
  node show only as a health line whose age grows; the worker's log names
  why. A disabled primary shows as a pool line, which says when no cntlr of
  the pool is enabled or the pool's level runs no reaction. It does not say
  when every enabled standby has a health epoch or an agent that refuses
  the worker's syncups, which also lets no failover follow; the worker's
  log then shows `reaction skipped` with the reason `no candidate` or
  `candidate_refuses`. A subsystem
  whose row on the primary reads pending
  while a host taken off its list keeps its connection shows no line, and no
  health epoch is set, although, once a converge has left the row so, the
  primary serves the subsystem's volumes to no host, save a namespace whose
  move to ANA inaccessible failed (section 6.1); `dnvctl cntlr inspect`
  shows the row. A worker that seems gone is a hint: the record a running
  worker writes in etcd every few seconds is older, by the gateway's clock,
  than twice the default time between two writes, and the gateway does not
  know a worker's own time between writes. No record keeps the time of a
  repair, a migration, a clone, a lost record or a refused grow, so their
  lines show no age, save the age of a row of the primary's report that a
  line shows. A pool shows a fixed number of lines at most, then one line
  that counts the rest. State: accepted.
- **A retired leg's partial mark can miss a short stay in the array.** A
  repair can take a retired leg back as its new leg. When md has counted
  that leg as a member but not copied all of it, the worker marks the leg
  partial, and a forced start then refuses to name it unless the mark
  passes `partial` (`--override partial`). The
  worker sees this only in the primary's reports, which it reads every few
  seconds. When md counts the leg and lets it go between two such reads,
  or a failed add still wrote md's record onto the leg, the leg gets no
  mark. The leg's own md superblock still shows what md left, which the
  forced start's read refuses, so a start from it fails with a reason.
  State: accepted. Not seen in the lab.
- **A retired leg's record does not say whether the leg was in sync when
  it left.** The record says whether the leg is kept, that is, held back
  from the worker's deletion by the operator or a forced start, and whether md wrote
  onto it as a repair's new leg that was then cancelled (its partial
  mark). It does not say whether a leg a repair replaced was in sync when
  it left the array. The worker's repair at the bound can then give back
  the only whole older copy; the leg md keeps in sync still holds the data.
  State: accepted. Not seen in the lab.
- **A disk node's full store disk is offered until a save fails.** A quiet
  node with a full store disk reads ok and is offered for new sides. The
  first refused request marks it. State: accepted; a small window. Not
  seen in the lab.
- **A row's text shows only the start and the end of a long error.** An
  agent cuts every text it puts in a row of its report, or beside one, to
  a fixed length, so that the largest report still fits in one message.
  A longer text, such as a failed command with its whole output, keeps its
  start, which says what failed, and its end, which says why, with "..."
  between. The agent's log keeps the failed command and its whole output.
  State: accepted, by design.
- **Log records that say less than they seem.** A call that gives up
  after its rounds (section 10) fails with the error code ABORTED, and the
  message does not say which node changed. A record of an etcd write
  inside a transaction does not say whether the write was made. The
  worker's leftover record names the first eight objects only. A cdc write
  timeout and a vanished host log the same reason. A listing of a
  controller node's devices that an error cut short still gives the
  devices it read, and the cn agent can judge the node from that part.
  State: accepted.
  Procedure written: [read the logs](operations.md#read-the-logs).

## 10. Sizes and performance

The enforced sizes and the measured figures are in
[the introduction](introduction.md#status-and-limits). The limits of
design:

- **One primary per pool bounds a volume's throughput.** Every volume of a
  pool is served by its one primary, and slices split the work inside that
  node, never across nodes. To scale out, spread the pools across
  controller nodes. State: accepted; more than one primary is not yet.
- **No throughput or latency figures are published.** Only the failover
  pause, the write pause when a disk node dies, the write pause while a
  volume grows, what the loss of every path of a data group does, what
  the loss of a controller node that holds many pools does, how soon the
  workers stop when one etcd member hangs, and the times and memory of one
  node of each role stepped to the largest counts are measured, on the lab
  VMs. State: accepted; the figures come from a
  performance run on AWS.
- **The largest counts ran on small machines only.** A lab tool stepped
  one real disk node to 1024 sides and one real controller node to 256
  cntlrs, each cntlr a one-slice pool with no RAID1, on small guests; every
  call succeeded and every check round was clean.
  [The introduction](introduction.md#status-and-limits) gives the
  guests' sizes and the times. State: accepted; larger machines and pools
  with RAID1 come with the performance run.
- **Every change of a pool sends the whole desired state to its sides and
  cntlrs.** A volume
  create sends the whole desired state to every side and cntlr twice: once
  for the create, and once when the worker marks the volume created.
  State: accepted.
- **Larger counts make larger messages and more connections and kernel
  objects.** The answer of a volume's written-block list read has no cap.
  A migration reads a leg's list in one answer of at most 64 MiB, so a leg
  of more than 512 Mi blocks gets no list, and its copy copies every
  block. State: accepted.
- **The controller nodes' budgets cap a cluster's space.** A budget is the
  pool space a controller node may serve to hosts. Every cntlr of a pool,
  primary or standby, charges its controller node the full size of the
  pool's groups. So the sum of the budgets divided by the cntlr count of
  the pools is the most group space the cluster holds, whatever its disks
  hold. The budget is read once, at the node's create, from the
  `--capacity` its cn agent was started with; unset, it is the largest
  budget, 64 TiB, so the cap binds a cluster whose operator set budgets, or
  one whose pool space passes 64 TiB times its controller nodes, divided
  by the cntlr count of a pool. No call changes it, so set `--capacity` on
  every cn agent before its node's create. A controller node that 0.1
  created with no budget keeps 0.1's 4 TiB after an upgrade. `dnvctl
  cluster get-capacity` names the budget when it is the limit the cluster
  reaches first, and an auto-grow refused for want of budget shows in the
  worker's log. State: accepted.
  Procedure written: [the operations doc](operations.md#the-clusters-capacity),
  The cluster's capacity.
- **The default cap on IO queues is unmeasured.** Each path holds one
  TCP connection per IO queue, and the agent caps the queues of a
  subsystem; no host with many CPUs has been measured for it. State:
  accepted; a measurement item of the performance plan.
- **At the defaults, a pool of more than four slices takes a smaller
  stripe, so that its hosts' reads and writes stay within what a
  controller node takes.** The stripe
  tells the host the volume's best IO size, the stripe times the slice
  count, and the host reads ahead twice that size and, when it is above
  4 MiB, sends commands up to it. A controller node's kernel refuses, over
  TCP, a command that carries more than 4 MiB of data, since a kernel fix
  of 2026 that the distributions' kernels carry; seen in the lab on kernel
  7.0.0-42, where a pool of five slices at a 1 MiB stripe gave its host IO
  errors. So a pool that names no stripe takes its cluster's stripe, 1 MiB
  at the defaults, halved until the stripe times the slice count is at
  most 4 MiB: 512 KiB at five to eight slices, 256 KiB at nine to sixteen
  and 128 KiB above. A pool create that names a stripe whose product with
  the slice count is above 4 MiB is refused with the reason and the
  largest stripe that fits. This fixes the IO errors, at a price: in a
  pool of more than four slices at the defaults a whole block a host
  writes is split over slices, so the thin pool does not skip the zero
  fill of a block written whole and the first write of a block costs
  more, and a discard frees space only over a longer range (the discard
  entry below). State: accepted. A pool created before dnv held the
  stripe to this bound, whose stripe times its slice count is above
  4 MiB, keeps its stripe, since a pool's layout never changes. Its hosts
  still get IO errors on a direct read or write over 4 MiB, and a buffered
  write fails at its flush; a buffered read still succeeds, more slowly.
  State for such a pool: procedure written,
  [A pool made before the stripe bound](operations.md#a-pool-made-before-the-stripe-bound):
  on every host, set the largest command (`max_sectors_kb`) to 4 MiB on
  each hidden path device of the pool's namespaces, one per controller
  node, and let a udev rule set it on each path that comes later. Seen in
  the lab: the setting on the path devices is enough, and the setting on
  the namespace's device alone changes nothing.
- **A clone between pools of some slice counts is refused.** A clone's
  device on the controller node tells the host a best IO size made from
  both pools: the least common multiple, the smallest size that is a whole
  multiple of each, of the source's stripe times its slice count and the
  destination's, or one
  block of the destination when that multiple is not a whole number of
  blocks. When it is above 4 MiB, the clone's hosts would send commands
  the controller node refuses, while the clone copies and after it, until
  the host reads the namespace's identity again; seen in the lab with a
  one-slice source of 1 MiB into a pool of five slices of 512 KiB. So the
  gateway refuses such a clone, and names both products. Pools whose slice
  counts are powers of two, 1, 2, 4, 8, 16 and 32, clone into each other
  freely at the defaults. At the defaults a one-slice volume goes only
  into a pool of 1 to 4, 6, 8, 12, 16, 24 or 32 slices, and the same
  holds the other way; a clone between a one-slice volume and a pool of
  any other slice count is refused. So is every clone into a pool whose
  block is above 4 MiB, and every clone into a pool of the default block
  when either pool was created before the bound above with more than four
  slices of the default stripe. State: accepted; a device layer that keeps
  the clone's best IO size at the destination's is fix later.
- **A pool made before dnv bounded the block can keep a block above 4 MiB,
  and then no migration moves its legs.** During a migration the device on
  the new side's disk node that pulls the data from the old side tells the
  controller node a best IO size of at least one block, so the controller
  node's path to it can carry commands up to the block, above what the disk
  node's kernel takes. Read from the kernel's code, not seen in the lab. So
  a cluster or pool create that names a block above 4 MiB is refused with
  the reason. The default block is 1 MiB. A pool made before this bound
  keeps its block, since a pool's layout never changes. When that block is
  above 4 MiB, the gateway refuses a migration in the pool, as it refuses a
  clone into it. An operator's repair replaces only a leg that failed, or
  one the primary keeps out for another identity, so an operator cannot
  move a leg of such a pool that still works, not to leave a slow disk and
  not to empty a disk node. The way out is to move the pool's volumes to a
  new pool by a transfer and a clone, which the old block does not stop. A
  cluster made before the bound with a block above 4 MiB refuses every pool
  create that names no block, every volume create of the CSI driver on it
  included, since that driver names no block. State: accepted; procedure
  written,
  [a pool made before the block bound](operations.md#a-pool-made-before-the-block-bound).
- **A cluster made before dnv held the extent size to whole 4 KiB keeps
  its extent size.** A cluster create refuses an extent size that is not
  a whole number of 4 KiB. With a size that is not a whole number of
  512 bytes, a side whose extents lie in two or more separate ranges of
  the disk can come out one or more sectors short, as the size and the
  range lengths fall, and the disk node cannot build it. With a whole
  number of 512 bytes that is not one of 4 KiB, every side builds, but
  its pieces do not start on the disk's 4 KiB blocks. No call
  changes a cluster's extent size, so such a cluster keeps it. No dnv
  tool or tutorial sets such a size. Read from the code, not seen in the
  lab. State: accepted.
- **A disk whose best IO size is above 4 MiB can be sent commands it
  refuses.** A disk can tell the kernel a best IO size, as a hardware RAID
  volume does from its stripe. The disk node passes it on to the
  controller node, whose path to the leg then sends commands up to that
  size, above the 4 MiB a disk node's kernel takes over TCP. Read from the
  kernel's code, not seen in the lab. State: accepted; the node check of
  a disk node (`dnv-agent check dn`) warns for such a disk
  ([operations](operations.md#check-a-node-before-it-joins)).
- **A CSI volume costs a pool.** Under the CSI driver every volume is a
  pool with its own cntlrs and sides, so the ceilings of the introduction's
  table bound the volume count. A controller node holds at most 256
  cntlrs, so with the default two cntlrs per volume a cluster holds at
  most 128 volumes per controller node it has. A RAID1 volume takes four
  sides at create, two more for every group auto-grow adds, and one more
  for every leg a repair adds, since the leg it replaces keeps its side
  for a time. A volume takes whole extents, one for its meta group and at
  least one for its first data group on every leg, however small the
  volume. State: accepted, by design. Procedure written,
  [One pool for each volume](operations.md#one-pool-for-each-volume): what
  a volume costs, and how to read what still fits.
- **A RAID1 pool needs four disk nodes per slice at create, in at least
  two locations.** The create puts every leg of the pool on a disk node of
  its own, and each slice has a meta group and a data group of two legs
  each, in two locations. A cluster with fewer disk nodes, or with its
  disk nodes in one location, is refused with the error code
  RESOURCE_EXHAUSTED. Four disk nodes per slice in two locations are not
  always enough: a location takes at most one leg of each group, so in two
  locations each one needs a disk node for every group, and with three or
  more locations the random draw of the picks can still refuse a create
  that another draw would place; such a create may place when it is sent
  again. The disk nodes of one machine created with no location are one
  location, so on one machine pass a location for each disk node
  ([the operations doc](operations.md#choose-a-nodes-location)).
  A group added later may share disk nodes with the pool's other groups.
  State: accepted, by design.
- **Nothing limits recovery load.** A dead disk node starts a full copy in
  every group it held, and a dead controller node fails over every primary
  it held, all at about the same time. The agents set no copy speed on md. Only the
  kernel's md speed settings bound the copies: the sysctls
  `dev.raid.speed_limit_min` and `dev.raid.speed_limit_max`, which an
  array's own `md/sync_speed_min` and `md/sync_speed_max` override until
  the array stops, as read in the kernel's md source. State: accepted.
  Procedure written, [md's copy speed](operations.md#mds-copy-speed): how
  to read and set them. Fix later, after the performance runs: per-pool
  copy speed settings.
- **A host discard frees space only over whole aligned blocks on every
  slice.** A thin pool frees whole blocks only, and completes a shorter or
  misaligned discard with success. A cluster created with no stripe gets
  one equal to its block, up to 1 MiB, so in a pool of at most four
  slices at the defaults an aligned 1 MiB discard frees one block. A
  pool's stripe is smaller than its block when its block is above 1 MiB,
  the largest stripe dnv takes, or when the pool names a smaller stripe.
  A pool that
  names no stripe takes its cluster's, narrowed as the stripe entry above
  says. That stripe is smaller than the pool's block when the cluster's
  stripe already is, or when the narrowing cuts it below the block, as at
  the defaults with more than four slices. Then a discard frees a
  block on one slice only when it covers nearly the slice count times the
  block, and `fstrim` can report success and free nothing. Seen in the
  lab: with one slice an aligned 4 MiB discard freed four blocks and a
  512 KiB one none; with two and eight slices each discard freed exactly
  what this entry says; and with one slice, after a host removed 12 MiB of
  files from an ext4 file system on a volume, `fstrim` freed 13 MiB of the
  pool's 1 MiB blocks. State: accepted. Procedure written,
  [Stripes and discards](operations.md#stripes-and-discards): a pool whose
  hosts count on discards takes a stripe equal to its block.
- **A deleted volume's space can stay in use on the disks.** The delete
  of a volume frees its blocks in the thin pool alone, so the primary
  first discards the blocks the volume wrote, and the discard reaches the
  disk nodes' disks. That matters for an SSD and for a thin disk, such as
  a cloud disk or a file. The primary skips the discard while a leg of
  the slice has no live path, while a device mapper device of the slice
  is suspended, or while the thin pool is read-only, since the discard
  could then wait for a long time or do nothing. It also sends at most a
  fixed amount of discards in one converge, and leaves the rest. A volume
  whose part on each slice is not a whole number of thin pool blocks keeps
  the last, partial block of each part once it was written. A volume deleted while its pool moves to another
  controller node, or while the pool is turned off, is never discarded:
  the thin pool deletes it later, with no volume left to discard. What is
  skipped or left stays in use on the disk until the pool writes those
  blocks again; no host can read it. A block that a snapshot shares stays
  with the snapshot. Read from the kernel's code, not measured. State:
  accepted.
- **Every controller node of a cluster runs one kernel series.** From
  kernel 6.19 on, md records a new array's block size in bytes of its
  superblock that older kernels read as padding and refuse. A failover or
  a cntlr replacement to a controller node on an older kernel then cannot
  assemble the pool's arrays. State: accepted; a requirement, which the
  introduction names. Read in the kernel's md source, not in the lab. The
  node check of a controller node says which side of 6.19 its kernel is
  on and warns for one before it; it reads one node, so it cannot hold the
  cluster to one series.
- **A thin id can leak after a failed delete.** When the delete of a
  volume fails twice, or the read of the volume's table does not answer,
  the volume's blocks stay allocated in its thin pool until a later pass
  over the pool's ids, and for the life of the pool on that primary if
  none comes. State: fix later. Not seen in the lab.
- **One cntlid slot serves most subsystems and can run out.** When its
  controller ids run out, the kernel's target refuses connects with "do
  not retry". State:
  fix later, after the performance runs: spread new sides and cntlrs over
  the slot list.
- **A pool's thin ids run out; a slice's data space is capped at
  creation.** State: accepted, by design.
- **An md check counts mismatches on never-written blocks.** The count
  means nothing, and dnv does not read it. State: accepted. Procedure
  written, [The md check's mismatch count](operations.md#the-md-checks-mismatch-count).
- **A retired leg taken back can join its array with old data.** A retired
  leg taken back is copied whole, unless md takes it back by a short copy
  of the regions its record names as written while the leg was away, as
  when the array stayed degraded since the leg left. In two cases md uses
  that short copy although its record lacks regions, since md clears the
  record behind a copy onto another new leg:
  that other new leg died during its copy and the array was then built
  anew, as by a failover or a reboot of the controller node; or its copy
  ended, no host write followed, and it then died. md then marks the leg
  in sync within seconds while it still holds its old bytes of everything
  written during the other leg's copy, and reads from both legs in turn, so
  hosts read old blocks with no error. State: accepted for the time of a
  whole copy; fix decided for the two cases. Read from the kernel's and
  mdadm's code; not seen in the lab.
- **Steady control-plane cost.** The reaction pass reads the whole pool
  from etcd every interval. At the debug log level every check round logs
  two records per object; at the default log level, info, a round that
  passes logs none. The CSI driver's capacity read, once a minute per
  storage class, is logged at info by the driver's controller plugin and by
  the gateway, and its reply names every node of the cluster, about 100
  bytes each: at 1,000 nodes, about 140 MB a day per storage class in each
  of the two logs.
  On a controller node that is also a host, every read of the node also
  reads the host NQN of each controller of a connection that is not dnv's,
  one read per controller. State: accepted.
- **The size cap on one message between dnv programs, and the cap on a
  program's commands.** Every dnv connection caps the size of one message,
  and every program caps how many commands and file operations it runs at
  once. State: accepted, by
  design.
- **Build time at the widest slice count.** A primary's build at 32 slices
  is bounded by the time to start its commands. Short thresholds then
  start failover after failover. A connect that reaches a grown group's
  side before its export exists leaves the group unbuilt until a later
  converge, which can outlast the primary threshold. State: accepted now;
  checked again by the next lab run at 32 slices.
- **A call that takes space gives up after a fixed number of rounds while
  other calls take space on the same nodes.** Most calls that take disk or
  controller space give up after 32 rounds with the error code ABORTED.
  The caller retries. State: accepted.

## 11. How this document is kept

- The LLM reads in this order: the core glossary, this document, then the
  design documents. This document owns the policy: the promise, the
  accepted loss, the states of the limits. A design document states the
  mechanism of a limit.
- A change that fixes a limit, finds a new one, or moves one to another
  state edits this document, and the design document that states the
  limit's mechanism, in the same change. A review checks it. No lint
  checks it today.
- This document names no release. The introduction says what a release
  holds and gives its figures.
