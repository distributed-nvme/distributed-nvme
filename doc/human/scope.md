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
[the introduction](introduction.md#status-and-limits-of-01). This document
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
through a volume. The RAID1 metadata of a new leg is zeroed before use, so
md never trusts an old superblock.

**What the promise does not cover.**

- **The network and root are trusted.** dnv checks no user. A host is
  known by its NQN alone, and a host can claim any NQN. All traffic is
  plain text. Root on a node, and anyone on the storage network, can read
  the old bytes of a recycled extent from a raw disk or from a leg. Anyone
  who reaches an agent's port can send it a desired state. dnv's programs
  log in to etcd with no password and no certificate, so etcd must accept
  such clients and stay on the trusted network. Deploy dnv on an isolated
  network you trust. State: accepted. Procedure to write: the
  network section of [the operations doc](operations.md), the document of
  procedures an operator follows by hand, with a table of who connects to whom and a firewall
  example.
- **In Kubernetes every pod reaches dnv's ports.** Every pod reaches the gateway, the
  agents, etcd, the cdc and the NVMe/TCP ports through its machine's
  network. Only the operator's firewall rule blocks it, or a network policy
  that the cluster's network software enforces. The trust dnv puts in the network is then trust in
  every pod. State: procedure to write, a network policy example in the
  CSI tutorial.

**Steps of a caller that can break the promise.** A migration and a clone
can skip the blocks a volume never wrote. For a migration the worker
reads the leg's list of written blocks itself, once the old side takes no
more writes, and hands it to the new side in pieces of a fixed size. For a
clone the caller reads the source volume's list and hands it over in
pieces; the gateway answers the read only while every namespace on the
volume is set aside on the primary, that is, ANA inaccessible with its
device serving no IO, and only in pieces that each cover a multiple of
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
  host's. Procedure to write, one line in the operations doc: list only
  the other pool's controller nodes.
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
  it with force. Procedure to write, one line in the operations doc: force
  is right only when an agent cannot answer the proof.
- **A forced migration cancel after the cutover.** A cancel after the
  cutover is refused unless forced, as the new side may hold the only copy
  of the leg's new writes. With force the old side serves its content as
  of the cutover: the leg loses every write since, and blocks first mapped
  since read old bytes of the old side's extents, possibly another pool's.
  In a group of more than one leg the other legs hold those writes, and md
  does not know this leg is behind, so reads can return its old bytes until
  the leg is repaired. State: accepted. The operator asks for it with
  force. Procedure to write: repair the leg right after a forced cancel.
- **A forced start from an older copy restores one group, not the pool.**
  A forced start is an operator's start of a group's RAID1 array from one
  leg they name, when md will not start it by itself. Blocks rewritten
  since the leg left read old bytes. In a data group, blocks first mapped
  since then read what the leg holds there, possibly another pool's bytes.
  No error is shown. A start from a half-copied leg that md marked in sync
  does the same over the regions md never copied (section 5.2). State:
  accepted. The operator asks for it. Procedure:
  [the forced-start guide](operations.md#the-forced-start-guide) says which
  leg to choose and what an older copy costs.

## 4. Data loss dnv accepts

dnv accepts data loss in four cases, and where an operator asks for it,
as with the forced finish and the forced start of section 3. Any other
data loss is a defect, unless section 3 or 5 marks it accepted and gives
the reason. Section 5 lists the data-loss defects known today.

1. **Every leg of a group is lost for good.** In a RAID1 pool no disk node
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
   operator must start that migration. No migration moves a side of a
   clone meta group, the group of the pool made for one clone. In a RAID1
   pool a repair replaces such a leg; in a pool without RAID nothing can
   move it today. State: fix later, small.
3. **A clone loses its whole source pool or its whole destination pool
   before the copy is complete.** With the source gone, the copy stops.
   A host's read of a block not yet copied waits or fails. With the
   destination gone, every write since the host moved over is lost.
4. **A migration loses a side.** Before the cutover, the old side's failure
   loses the leg, and the new side's failure loses nothing. After the
   cutover and before the copy is whole, either side's failure loses the
   leg. After the copy is whole, only the new side matters. RAID1 then
   decides whether the pool loses data.

Two more things belong with case 1:

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
  accepted. Procedure: [the forced-start guide](operations.md#the-forced-start-guide), "A lost clone meta
  group"; not tested.

**An error is not a loss.** A write that fails with an error is not a data
loss in the sense of this document. The application knows. Two cases:

- **A pool runs out of space.** Auto-grow is best effort. A grow fails
  when it finds no disk node with room, when one of the pool's controller
  nodes has no budget left for the new group, or when the slice already
  holds the most data groups it can. Nothing reserves space ahead. A write
  that needs a new block waits for the kernel's no-space timeout and fails. Written
  blocks keep serving. Under the CSI driver the cluster can be
  over-committed. State: accepted. An alert for it is not yet.
- **Writes wait for the kernel's timers when a disk node dies.** Over
  NVMe/TCP a dead disk node is noticed by the keep-alive alone, about 6 to
  10 seconds. md fails the member then, and the writes complete on the
  other leg. A flush md sent to that leg ends then too, with no error. IO sent
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
primary died is a defect; section 5.1 has it. So is a durable write lost
in a failover while the old primary still serves, but for the cases
section 5.1 lists and accepts.

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
  programs cannot fix this. State: fix later; a change inside dm-thin is
  under test. Read from the kernel's code; not seen in the lab.
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
- **A torn snapshot can read as whole.** Silent. A snapshot is taken slice
  by slice. A primary crash between two slices, or a slice whose step was
  skipped or failed, leaves a torn snapshot. The agent marks it torn and
  saves the mark at once in its local store, the directory where it keeps
  its own state on its node. Every cntlr reports a mark it holds, as the
  primary or not, and the worker records the mark from the first report,
  before or after the snapshot is recorded as created. The mark is lost
  only when the controller node that saw the tear never comes back or
  loses its local store, when its agent dies in the moment between the
  first slice's step and the save, or when the save fails and the agent
  restarts before the worker records the mark. A lost mark leaves no sign. Delete and
  re-create a snapshot reported torn. State: accepted. Not seen in the lab.

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
  carries the force flag, and with the flag drops the list, which is right
  for the leg that died. When a forced mark names the half-copied leg
  instead, hosts read, over the regions md never copied, whatever that
  leg's disk held there, possibly another pool's bytes, with no error. Both
  legs show the same change count. One force flag also passes the forced
  start's other refusals in the same start. State: accepted, the force is
  the operator's choice; procedure written,
  [the forced-start guide](operations.md#the-forced-start-guide): name the
  leg md copied from, not the new leg it copied onto; `dnvctl group
  inspect` shows each leg's bad-block count and md's view of it, and its
  dry run without the flag shows the refusal. Seen in the lab.
- **A failed stop during a forced start keeps a bad-block list.** When the
  named leg carries a bad-block list, the forced start starts the array,
  stops it at once, and starts it again with the list dropped. If that stop
  fails, the array runs with the list, and the legs added to it copy the
  list's regions as bad too. Hosts then get IO errors over those regions,
  though the named leg holds their data. State: procedure to write: the
  way back, a level step down and a new mark, is tried in the lab first;
  until then [the forced-start guide](operations.md#the-forced-start-guide)
  says to stop and ask for help. Forty such stops in the lab never failed.
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

No case is known today.

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
  next id back. A new volume can take the id of an old volume. The primary
  then reads the pool's records of its thin ids, and refuses a new volume
  whose id holds data, or whose pool's records it cannot read: the volume
  stays unbuilt and its row names the refusal. Two cases stay. A new snapshot can still take an old volume's id
  and serve its data, since a snapshot's id always holds data. And a host
  write to a new volume before it is recorded as created, then a failover,
  makes the new primary refuse the volume's own id. Both stay inside one
  pool. State: accepted. Procedure: [a new volume refused for a thin id
  that holds data](operations.md#a-new-volume-refused-for-a-thin-id-that-holds-data),
  which tells the two cases apart; section 7 has the restore's steps
  known today. Not seen in the lab.
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
- **dnvctl parses an id with leading zeros as octal.** An id copied from a
  device name can name another object. A forced start or a delete can act
  on the wrong group or leg. State: fix decided, dnvctl refuses such an
  id. Not seen in the lab.
- **A change without a revision token overwrites another operator's
  change.** A revision token is the revision the caller last read, which
  dnvctl sends with `--rev`; the gateway refuses the change if the
  revision has moved since. This loses a change of the desired state, not
  volume data. A caller may send the token and is then protected. State: accepted, by
  design. Procedure to write, one line in the operations doc.

## 6. The system comes back alone

The table says what dnv does by itself for each fault, and what an
operator does by hand. The third column gives the wait where a setting
controls it. The settings are the pool's thresholds, the check interval
and the cluster's start hold, which is how long the worker waits for an
agent that is still rebuilding after a restart, in seconds, and three
flags of the worker: `--vote-interval`, `--vote-grace-time` and
`--vote-leave-grace-time`.
Measured times are in [the introduction](introduction.md#status-and-limits-of-01).

| Fault | What dnv does by itself | The wait, where a setting controls it | By hand |
|---|---|---|---|
| A disk fails, one leg of a RAID1 group | md fails the leg. IO goes on over the other leg. Leg repair replaces the leg with a new leg on another disk node, and md copies the data onto it. The old leg becomes a retired leg. | `leg_unhealthy`, 1200 at the default, before the repair starts. | Nothing. In a pool without RAID the data is gone (accepted case 2). |
| A disk node is down and comes back | In a RAID1 pool, md fails its legs after the kernel's keep-alive. IO goes on over the other legs. A group with no IO sends md nothing to fail, so the primary fails such a leg in md itself once the leg has failed its health check twice in a row, while another leg holds the data in sync. md then records the death on the other leg, and a failover or a reboot can start the array from that leg alone. Back in time, its legs rejoin and md copies the changed regions. Down for long, leg repair moves each leg to another disk node. In a pool without RAID, the IO waits for the fast IO fail timeout and then fails (section 4). | `side_unhealthy`, 600 at the default, before the repairs start. | Nothing. A node that never returns needs the node replacement procedure (section 7). |
| A controller node is down and comes back | Each primary on it that has a healthy enabled standby is failed over to it. Hosts see a pause. A pool of one cntlr waits for a cntlr replacement. Back, the cntlrs that were failed over are standbys. A cntlr unhealthy for long is replaced on another controller node. | `primary_unhealthy`, 4 at the default, with two check rounds in a row that find the primary unhealthy; a round runs every `cntlr_interval`, 2 at the default. A missed round counts only once its own wait of one interval has run out, so a failover comes about 6 to 10 seconds after the node dies. `cntlr_unhealthy`, 600, before a replacement. | Nothing. |
| The network between a controller node and a disk node is cut | When the controller node holds the primary, as for a disk node down, seen from that controller node: md fails the legs behind the cut and takes them back when the cut ends. A long cut moves the legs. A cut from a node that holds only standbys changes nothing. | `leg_unhealthy`, 1200. | Nothing. |
| The network between hosts and a controller node is cut | Nothing. dnv reads the agents, never a host. The primary stays the primary and stays in the cdc's answer. The host's IO queues as long as a standby path is left. | None. | Disable the primary, so a failover runs at once. Enable it again after. State: procedure to write; fix later, a host health signal into the worker. |
| The network between the control plane and the agents is cut | The data plane keeps serving. The primaries of one cut-off controller node are failed over. The old primary never gets its demotion, and the fence comes after the three-second hold, so hosts that drop the old path inside the hold see a pause, and a host still on it sees IO errors (section 8) and, in the cases section 5.1 lists, can lose a write it was told was done. With every agent cut off, nothing changes until the network is back. | As for a controller node down. | Nothing. |
| etcd is down and comes back | The data plane keeps serving. No call and no reaction runs. A worker that cannot write to etcd stops driving its pools about 30 seconds into the outage, and drives again about one grace time after etcd is back. | The worker's flags: `--vote-interval`, 10 at the default, for the 30 seconds, which are three intervals; `--vote-grace-time`, 60, for the wait after. | Nothing. State: procedure to write, "running while the control plane is down". |
| A worker, the gateway or the cdc is down and comes back | Another worker, when one runs, takes the pools after a bounded window. The gateway has no state. Without the cdc, hosts keep their paths and get no path changes. | The worker's flags `--vote-interval`, 10 at the default, and `--vote-leave-grace-time`, 25: a dead worker counts as gone after twice the interval, and the change then holds for the leave grace time, about 45 seconds in all. | Nothing. A failover during a cdc outage leaves hosts on a withdrawn path (section 6.1). |
| An agent restarts | The agent rebuilds its desired state from its local store, the files in which it keeps the desired state it last accepted. The kernel keeps the devices. As soon as the program runs again, the agent answers each check round with "starting", an answer that says it is still rebuilding, and while it does, for the start hold at most, the worker starts no failover, cntlr replacement or leg repair because of that node. A controller node agent that stays stopped for longer than one check interval can have its primaries failed over, as for a controller node that is down. | For a stopped agent, `primary_unhealthy` with two check rounds in a row: its rounds fail at once, so a stop a little longer than one `cntlr_interval`, 2 at the default, can already fail a primary over, and a failover comes about 4 to 8 seconds after the agent stops, at the defaults. For an agent that answers "starting", the start hold: the longest time the worker waits for an agent that is still rebuilding, 120 at the default, counted from its first start even when it stops and starts again while it rebuilds or just after, before a check round has judged it, a cluster setting fixed when the cluster is created (`--start-hold-seconds` of `dnvctl cluster create`). | For a planned restart, the steps in [the operations doc](operations.md#a-planned-restart-of-an-agent). A rebuild longer than the start hold, and a reboot that the worker first sees as an agent that answers "starting", are in section 6.1. |
| A host reboots | Nothing on dnv's side. nvme-stas or the Kubernetes node plugin connects the paths again. | None. | See the host actions in section 7. |
| All disk nodes of a pool are down at once, as in a switch reload | md marks each array broken under IO. Hosts see IO errors. The broken array makes the primary unhealthy, so the worker fails it over at the primary threshold, even while the nodes are still down. Once the legs are back, the new primary assembles the arrays again. If a data group broke while its meta group still took writes, the thin pool comes back read-only and needs the repair by hand (section 6.1). A pool of one cntlr waits for a cntlr replacement at the cntlr threshold. | `primary_unhealthy` for the failover. `cntlr_unhealthy`, 600, for a replacement. | A forced start, when no failover comes. The blocks mapped since the last commit of a broken meta group are gone (accepted case 1). |

### 6.1 Cases where dnv does not come back alone today

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
  lab: on loop devices, and in the cn agent suite after a level step.
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
- **A thin pool marked for a metadata check, or with corrupt metadata, has
  no repair path.** A failed commit leaves a slice's thin pool read-only,
  with dm-thin's mark that its
  metadata needs a check. The mark is kept in the pool's metadata, so the
  pool opens read-only on every controller node. The primary reports the
  pool as an error, and the worker logs it and starts no failover, since a
  new primary would meet the same pool. A pool that dm-thin failed
  without that mark, or that is read-only because its metadata is full,
  is not this case: the first is failed over, since a new primary opens
  the pool from its last commit, and the second gets more metadata from
  the auto-grow. Corrupt thin pool metadata has no check and no repair
  flow in dnv. State: procedure to write, the repair by hand onto fresh
  space, with the level steps, the names of the devices involved, and the
  tools thin_check and thin_repair, with a test of the cn agent suite.
  Seen in the lab: a pool with the mark opened read-only, a failed pool
  opened again as writable, and a full one became writable after its
  metadata grew.
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
  nothing moves, and a region recorded on every leg is lost. Between two
  checks the drops go on, each a resync of a small part of the leg. A
  drop stops a running check, and the next try starts again from the
  leg's start, so a sector on which reads fail often enough can keep the
  check from reaching it. The worker gives that check up after two days
  and asks again one interval later. A cluster created before this
  setting existed, or with zero, has no check. A leg with slow IO reads
  ok, and every write of the group waits for it. State: accepted for the
  time between two checks and for slow legs; migrate the leg's side by
  hand. Seen in the lab: a check over a sector that fails reads healed
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
- **Orphans the sweep never removes.** An export of a side whose build
  stopped before the export named its device. A subsystem for hosts whose
  build stopped before dnv set the model name by which it knows its own
  subsystems. A connection to a clone
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
- **A step only a check round names costs a converge every second check
  round while the converge keeps failing it.** Some work is named only by
  a check round, not by the converge's own answer: a leg to put back into
  its array, an array to start, a forced start, a snapshot to make, the
  start or stop of md's check, a connection to make again. The worker then
  sends the syncup every second check round at most. When the check
  round's test passes but the converge keeps failing, each such syncup
  costs one converge that fails again. That happens when md keeps refusing
  a change of the legs of a clone meta group's array, which a check round
  does not try, so that it names the clone's dm-clone over that group;
  when a program outside dnv holds an array open, so that md refuses to
  stop it for a forced start or for a new assembly of a broken array; when
  a leg that a migration moves serves on one path while its other path
  cannot connect; when md keeps refusing to start or stop its check; when
  the message that makes a snapshot keeps failing, each try of which
  pauses the IO of the snapshot's origin for a moment; and when a slice's
  thin pool has lost a volume's thin id, dm-thin's device number of the
  volume inside the pool, which a check round cannot tell from a volume
  not built yet, and which only an operator ends. Between two converges a
  check round reads the cntlr anew, so the primary's check rounds still
  count toward a failover. State: accepted; each case lasts only as long
  as its fault. Not seen in the lab.
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
  allows. This lasts until the disk node answers, leg repair has replaced
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
  for another reason. The gateway refuses to delete a namespace a transfer
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
- **An old md superblock at the end of a new side can fail the leg.** The
  end of a new side is never zeroed. A host of an earlier pool may have
  written an md superblock of a format that sits at the end of a device,
  and mdadm then refuses the new leg as a member of another array, so the
  group reports an error. State: accepted. The failure is rare and shows
  as an error. A host of a running pool can write the same at any time, so
  zeroing the end of each new side would close only half of it. Not seen
  in the lab.
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
  down.** A forced start from another leg leaves the repair's bad leg with
  the array's old identity. The primary does not add that leg back, since
  it may hold data the array lacks. After the repair is cancelled, the next
  assembly, as after a failover or a restart, finds legs of two identities
  and leaves the group down, its row in error. A repair that would keep
  that leg's data is refused while the leg reads healthy, which it does,
  and a forced start from the leg wanted wipes it. While the array runs,
  the primary's report names that leg on its row as a leg of another
  identity. Once the group is down with two identities, the report names
  every leg of either identity as one dnv cannot decide on, since only the
  operator can tell which identity is current. State: fix decided, a
  repair may take as its bad leg a leg the report names while the array
  runs, and must ask for the force flag for a leg named undecided, since a
  wrong choice serves older data and loses the writes made since; until
  then [the forced-start guide](operations.md#the-forced-start-guide) says
  to stop and ask for help. Not seen in the lab.
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
  volume's data for good. State: procedure to write, how
  to restore one record from the etcd backup; never remove the name.
- **A node deleted before its sweep keeps leftovers.** A node delete asks
  no agent. A reused disk is refused, since its header block, where dnv
  records the disk, still names the old node, until that block is zeroed
  by hand. A dead disk node cannot be deleted while a side is on
  it. State: fix decided, the node delete asks the agent and refuses while
  leftovers stand unless forced. Procedure to write: node removal and disk
  reuse. Fix later: a forced node delete for a dead disk node. Not seen in
  the lab.
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
  deleted in that time keeps the side. When the kernel renews an export's
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
- **Worker membership windows.** Workers meet only in etcd. At every owner
  change two workers can drive the same pool, or none, for a few seconds.
  A worker learns of changes through streams from etcd. One whose stream
  loses another worker's heartbeats takes that worker for dead and drives
  its pools beside it for up to about a minute. A stream that loses
  changes while etcd's progress messages still arrive is never noticed,
  and what it follows stays as it was on that worker until a restart.
  State: accepted, by design.
- **A fault only a host sees never fails over.** See the table. On a pool
  with a healthy enabled standby, disabling the primary starts a failover
  at once. A disabled cntlr is never made the primary and never replaced,
  so enable it again after the failover. On a pool with no such standby,
  a pool of one cntlr among them, a disable starts no failover and stops
  the pool's IO at once, since a disabled cntlr serves no IO. The cdc stops
  listing the disabled primary, so a host that follows the discovery log
  drops its last path, and a Kubernetes mount does not survive it. State:
  procedure to write; fix later.
- **An nvme-stas host loses every path when its discovery daemon is
  away.** nvme-stas has two daemons: stafd talks to the cdc, and stacd
  connects the paths. stacd reads a missing stafd as an empty discovery
  log. With the tutorial's setting, which lets stacd disconnect what it
  connected itself, it drops every path it made at its next start or at
  its next pass over its connections. A reload, a removed controller, a
  device audit or one of its timers starts such a pass. This was read in the source of
  nvme-stas 2.3.1. To restart stafd, stop stacd first, which keeps its
  connections when it stops; restart stafd; once stafd holds its
  connection to the cdc again, start stacd. State: procedure to write,
  the restart order.
- **A host keeps a withdrawn path.** A path connected by hand is never
  dropped. A cdc cut off from etcd keeps listing a failed-over primary. The
  Kubernetes node plugin keeps a cdc address an old attach named. Such a
  host writes to the fenced old primary and gets IO errors in the failover
  window, and, in the cases section 5.1 lists, can lose a write it was
  told was done. State: procedure to write for the first two; fix decided for the
  plugin, which takes the cdc list from its deployment on every pass.
- **A Kubernetes mount waits for ever when no cntlr of its volume comes
  back.** The node plugin's connect keeps reconnecting through an outage
  of every path, however long, and the IO waits. So the mount survives a
  long outage, and a cntlr replacement on a pool of one cntlr while the
  old path lasts until the new one has joined. But a pod blocked in that
  IO cannot be stopped until the operator runs `nvme disconnect -n` with
  the volume's subsystem NQN on the node. Nothing reports a dead mount.
  State: procedure to write; no fix.
- **A Kubernetes mount does not follow a new device.** A reconnect refused
  with "do not retry" ends for good, as when a controller node comes back
  holding other volumes' cntlrs before the volume's. Once the subsystem's
  last connection is gone, the mount keeps the dead device, and a later
  connect makes a new device that the mount does not follow, until the pod
  is deleted and made again. State: procedure to write; no fix.
- **A cntlr replacement on a one-cntlr pool can cost the volume its path.**
  This happens when the old path's reconnect is refused with "do not
  retry" before the new path joins. State: accepted; use two cntlrs per
  volume. Procedure to write, one line in the CSI tutorial.
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
- **A disabled primary whose standbys refuse their syncups can stop the
  pool's IO.** A standby refuses its syncups when it cannot save them to
  its local files or finds a setting in them invalid. Once the worker has
  seen a standby refuse a syncup, it does not make that standby primary,
  since it would refuse the syncup that makes it primary too. When an
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
what exists today. "Tool" is a dnvctl command, a gateway call, or what the
worker does by itself. "Doc" names the document with the steps. The two
tutorials are [dnv on one machine](tutorial_all_in_one.md) and
[dnv under Kubernetes on one machine](tutorial_csi.md). "Operations doc"
names [the operations doc](operations.md), the procedures an operator
follows by hand, which holds the forced-start guide, the steps for a pool
made before the stripe bound and a planned restart of an agent so far, and
gets the others as they are written. Every row with no tool
or no doc carries its note. Section 6.1 and the tables give some short
steps; the section "Steps known today" below gives the longer ones.

### Nodes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Add a disk node or a controller node | `dnvctl dn create`, `dnvctl cn create` | the tutorials | done |
| Prepare a node: a udev rule on every node that keeps md's own assembly off dnv's arrays, the network and the firewall | none | the tutorials, in part | procedure to write, in the operations doc. Fix decided: a node check, a subcommand of `dnv-agent`, reports what a node lacks and how to fix it; files for nodes, with the udev rules, will ship with the programs. |
| Disable a node | `dnvctl dn set-disabled`, `dnvctl cn set-disabled` | none | procedure to write, one line: when to disable |
| Remove a node | `dnvctl dn delete`, `dnvctl cn delete`. Refused while a side or a cntlr is on it. | the teardown of the tutorials | procedure to write: drain first, check the leftovers. Fix decided: the delete asks the agent. |
| Replace a dead disk node | leg repair moves its RAID1 legs; `dnvctl retired-leg delete` frees the retired ones | none | procedure to write. Fix later: a forced node delete. |
| Replace a dead controller node | the worker replaces its cntlrs after `cntlr_unhealthy`; `dnvctl cntlr create` and `dnvctl cntlr delete` by hand | none | procedure to write |
| Reuse a disk from an old node | none: stop the agent; zero the disk's header block, where dnv records the disk; delete the agent's local store; start the agent; create the node | none | procedure to write |
| See a node's live state and the cluster's capacity | `dnvctl dn inspect`, `dnvctl cn inspect`, `dnvctl cluster get-capacity` | none | procedure to write, one line. Fix decided: a capacity answer that also says what can still be placed and which limit the cluster reaches first. |
| Compare a disk node's own free count with its record | `dnvctl dn inspect` shows the agent's count, `dnvctl dn get` the record's | none | procedure to write, one line: the counts are compared, never reconciled |
| Gather a node's state for a bug report | none | none | fix decided: a gather subcommand of `dnv-agent` writes one archive |
| Restart a node's agent | none; the agent answers "starting" while it rebuilds, and the worker waits for it up to the start hold | the operations doc | done |

### Pools

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create and delete a pool | `dnvctl sp create`, `dnvctl sp delete` | the tutorials | done |
| Grow a slice | the worker grows it by itself; `dnvctl sp grow-slice` | none | procedure to write, one line: when to grow by hand |
| Change a pool's level | `dnvctl sp set-level` | none | procedure to write, with the thin pool repair |
| Move a leg to another disk node | `dnvctl migr create`, `migr finish`, `migr cancel`; the worker reads and hands over the leg's list itself | none; the introduction explains the flow | procedure to write: the whole flow |
| Repair a leg by hand | `dnvctl repair create`, `repair finish`, `repair cancel`, `repair abort`; the worker repairs by itself | none | procedure to write |
| Force-start a group | `dnvctl group inspect` shows what the primary reads from each member, and with `--leg` says what a start from that leg would do or why it would refuse; `dnvctl group force-start`, `dnvctl group set-operator-in-charge` | [the forced-start guide](operations.md#the-forced-start-guide) | none |
| Delete or keep a retired leg | `dnvctl retired-leg list`, `dnvctl retired-leg delete`, `dnvctl retired-leg keep`; a forced start keeps the group's retired legs by itself | [the forced-start guide](operations.md#the-forced-start-guide), "Keeping the old copies", for a forced start | procedure to write: when to keep a leg apart from a forced start |
| Replace a cntlr by hand | `dnvctl cntlr set-enabled`, `dnvctl cntlr delete`, `dnvctl cntlr create` | none | procedure to write |
| Change a pool's cntlid slots, the ranges of NVMe controller ids its exports use | `dnvctl sp set-cntlid-slots` | none | procedure to write, one line |
| Change a pool's thresholds | none | — | not yet |

### Volumes

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create, snapshot and delete a volume | `dnvctl td create`, with `--ori` for a snapshot; `dnvctl td delete` | the tutorials, for the create and the delete | done; procedure to write, one line on snapshots |
| Export a volume to hosts; change its hosts | `dnvctl ss create`, `dnvctl ns create`, `dnvctl ss set-hosts` | the tutorials, for the export | done for the export; procedure to write, one line on `ss set-hosts`. Fix decided: a removed host is cut. |
| Park a namespace | `dnvctl ns set-suspended`, `dnvctl ns set-dev` | none | procedure to write |
| Move a volume to another pool | `dnvctl xfer create` on the source; `dnvctl clone create`, `clone append-bm` on the destination | none; the introduction explains the flow | procedure to write: the whole flow. |
| Clone from a source outside this cluster | `dnvctl clone create`, `clone set-tr` | none; the introduction explains the flow | procedure to write |
| Read a volume's written blocks | `dnvctl td get-bm` | none | procedure to write, with the move to another pool |
| Grow a volume; revert a volume to a snapshot | none | — | not yet |

### Control plane

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Back up and restore etcd | etcd's own tools; `dnvctl cluster advance-ids` after a restore | this page, below; [operations](operations.md#a-new-volume-refused-for-a-thin-id-that-holds-data) for a refused thin id | procedure to write. A new volume over a restored thin id that holds data is refused. |
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
| Restart nvme-stas | none | this page, section 6.1; [dnv on one machine](tutorial_all_in_one.md) | procedure to write: stop stacd first, restart stafd, and start stacd once stafd holds its connection to the cdc again |
| Recover a host that keeps a withdrawn path | none | none | procedure to write. Fix decided for the node plugin. |
| Recover a Kubernetes mount after the loss of its last path | none; `nvme disconnect -n`, then delete the pod | this page, section 6.1 | procedure to write; no fix |
| Recover from a fault only a host sees | disable the primary | this page, section 6.1 | procedure to write. Fix later. |

### Steps known today

These are the longer steps written so far. [The operations doc](operations.md)
will hold the full procedures.

**Back up and restore etcd.** Back up etcd with its own backup command,
`etcdctl snapshot save`, on a schedule. dnv adds nothing to etcd's backup
or restore. A restore brings the desired state back to the backup's time,
so what was created after it is lost with its data, and a pool whose
layout changed since breaks. A restore needs dnv's steps around etcd's:

1. Stop every worker, cdc and gateway. The agents keep serving.
2. Restore etcd, and start it with the request caps and the automatic
   compaction of the etcd command in
   [dnv on one machine](tutorial_all_in_one.md), as
   [the requirements](introduction.md#requirements) say.
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
cluster it holds, and every CSI driver that talks to its gateways: its
controller plugin, the part that calls the gateway, and its node plugins.
A stopped node plugin leaves the connections and mounts of its volumes in place. Every binary is replaced,
`dnvctl` included, and only then does any program start again. The workers
stop first and start last. There is no rolling upgrade, since no message
carries a build version, and two builds that differ in a message would
misread each other with no error.

**Logs.** The programs log to stderr as JSON, one record per line, at one
level, with no level flag and no rotation. Keep the logs on a filesystem
apart from the agents' local stores and etcd's data, and rotate them
yourself: a disk the logs fill is one the stores and etcd cannot write.

### Notes on the maintenance tools

- **md's behaviour is tested in the lab only.** Failover safety and the
  forced start rest on mdadm's behaviour, tested on the lab's mdadm. The
  order of fences of section 5.1 also rests on the kernel's md: it
  completes a write that landed on one leg and failed on the other, and
  when the legs' records read equal, the start of the array copies the
  leg in its first place over the others. A script,
  `integtest/md_fence_order.sh`, checks this by hand on one lab VM's
  kernel, and no suite runs it. Nothing checks the
  mdadm or the kernel of a deployed node. State: fix decided, the agent
  logs the mdadm version when it starts, the requirements name the tested
  version, and the node check reports a version outside the tested range
  and a kernel outside the tested series.
- **Retired legs keep their extents until deleted.** A group never back to
  whole keeps them for good. At the bound of two, an operator's repair
  with a fresh leg is refused. State: procedure to write, one line.
- **A forced clone delete returns before the teardown is done.** Success
  means the teardown started. The operator polls the clone and the pool.
  State: accepted, by the dnvctl design. Procedure to write, one line.

## 8. Waits a host can see

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
- **A migrating leg whose serving disk node dies waits without limit
  where its group has no other leg in sync, as in a pool without RAID.
  One whose destination dies after the cutover, before its copy is whole,
  keeps its group one leg short.** A migrating leg has two paths, and the
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
  Before that, an operator can end the migration. Where no other leg is in
  sync, as in a pool without RAID, the primary does not cut the path,
  since the write would fail. The IO then waits until the disk node comes
  back. State: accepted; the group left one leg short after a cutover with
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
  up to one interval more, 2 seconds at the default (section 5.1). A host
  that follows the discovery log drops the path inside the three seconds.
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
  other windows and the Kubernetes case are not seen in the lab.
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
- **Smaller bounded waits.** An agent that marks the regions of a piece
  of a written-block list holds that object's other work, and while the
  copy serves pauses the volume's IO, until the marks are done; a copy
  still in flight first holds the pause for a short bound. A grow whose new group cannot get ready waits while a repair,
  a retired leg, a forced-start mark, the operator-in-charge flag or a
  migration acts on that group. Outside a failover and a cntlr replacement
  of the primary, the hold delays the cntlrs' syncups by up to one check
  interval. In those two, the cntlrs but the old primary also wait for its
  demotion, three seconds at most, and for the other legs' disk nodes to
  report their fence, one more interval at most (the failover entries
  above). A repair waits while its array does not run. A clone
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
  subsystem no cdc serves, a frozen worker watch, a slow leg and a stuck
  repair show in a log line or not at all. The worker's log records the
  reactions it applied and the ones it skipped. No dnv program serves
  metrics, profiles or health over a port, though the CSI driver's
  manifests add a helper container that answers Kubernetes' health checks
  on port 9808 or 9809. State: not yet; log
  handling goes into the operations doc. Fix decided: one gateway read,
  with its dnvctl command, lists what is wrong across the cluster.
- **A retired leg's partial mark can miss a short stay in the array.** A
  repair can take a retired leg back as its new leg. When md has counted
  that leg as a member but not copied all of it, the worker marks the leg
  partial, and a forced start then refuses to name it unless forced. The
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
- **Log records that say less than they seem.** A call that gives up
  after its rounds (section 10) fails with the error code ABORTED, and the
  message does not say which node changed. A cdc write timeout and a
  vanished host log the same reason. State: accepted. Procedure to write:
  one paragraph on reading the logs.

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
- **Every change of a pool sends the whole desired state to its sides and
  cntlrs.** A volume
  create sends the whole desired state to every side and cntlr twice: once
  for the create, and once when the worker marks the volume created.
  State: accepted.
- **Larger counts make larger messages and more connections and kernel
  objects.** A row's text from a failed command and the answer of a
  volume's written-block list read have no cap. A migration reads a leg's
  list in one answer of at most 64 MiB, so a leg of more than 512 Mi
  blocks gets no list, and its copy copies every block. State: accepted.
  Fix decided: a cap on a row's text.
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
- **A migration in a pool whose block is above 4 MiB can be sent commands
  its new side refuses.** During a migration the device on the new side's
  disk node that pulls the data from the old side tells the controller node a best IO size of at least one
  block, so the controller node's path to it can carry commands up to the
  block, above what the disk node's kernel takes. Read from the kernel's
  code, not seen in the lab. The default block is 1 MiB. State: fix
  decided: the block is bounded at 4 MiB at cluster and pool create; a
  pool made before the bound keeps its block.
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
  disk nodes is refused with the error code RESOURCE_EXHAUSTED. A group added later may
  share disk nodes with the pool's other groups. State: accepted, by
  design.
- **Nothing limits recovery load.** A dead disk node starts a full copy in
  every group it held, and a dead controller node fails over every primary
  it held, all at about the same time. The agents set no copy speed on md. Only the
  kernel's md speed settings bound the copies: the sysctls
  `dev.raid.speed_limit_min` and `dev.raid.speed_limit_max`, which an
  array's own `md/sync_speed_min` and `md/sync_speed_max` override until
  the array stops, as read in the kernel's md source. State: accepted. Fix later, after the performance runs:
  per-pool copy speed settings.
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
  block, and `fstrim` can report success and free nothing. Read from the kernel's code, not measured. State: accepted.
  Procedure to write, one line on the stripe setting.
- **Every controller node of a cluster runs one kernel series.** From
  kernel 6.19 on, md records a new array's block size in bytes of its
  superblock that older kernels read as padding and refuse. A failover or
  a cntlr replacement to a controller node on an older kernel then cannot
  assemble the pool's arrays. State: accepted; a requirement, which the
  introduction names. Read in the kernel's md source, not in the lab.
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
  to write, one line.
- **A retired leg taken back is copied whole unless the array stayed
  degraded.** State: accepted.
- **Steady control-plane cost.** The reaction pass reads the whole pool
  from etcd every interval. Every check round logs two records per object.
  State: accepted. Fix decided: one subsystem listing per sweep pass.
- **The size cap on one message between dnv programs, and the cap on a
  program's commands.** Every dnv connection caps the size of one message,
  and every program caps how many commands and file operations it runs at
  once. State: accepted, by
  design.
- **CSI: a volume name with ".." leaves a pool with no volume id.** State:
  fix decided, refuse such a name before any create.
- **xfs volumes need kernel 5.19 or newer on the node.** State: accepted;
  a requirement. Fix decided: the node plugin formats xfs for a stated
  minimum node kernel.
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
