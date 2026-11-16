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
  network section of the operations doc, the document of procedures still
  to be written, with a table of who connects to whom and a firewall
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
  accepted. The operator asks for it. Procedure to write: the forced-start
  guide, a document still to write, says which leg to choose and what an
  older copy costs.

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
  accepted. Procedure to write: the forced-start guide covers the way
  back.

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
primary died is a defect; section 5.1 has it.

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
- **A torn snapshot can read as whole.** Silent. A snapshot is taken slice
  by slice. A primary crash between two slices, or a slice whose step was
  skipped or failed, leaves a torn snapshot. The agent reports it torn, but holds the
  mark in memory until the snapshot is recorded as created. An agent
  restart, a failover or a cntlr replacement in between can lose the
  mark, and a lost mark leaves no sign. Delete and re-create a snapshot
  reported torn. State: fix decided, the
  agent saves the mark at once and the worker records it from the first
  report. Not seen in the lab.

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
  it. When a forced start on such a group zeroes the other members and is
  cut short, and the named leg is then lost, every leg reads new, and the
  primary creates an array over them with no copy. Hosts read a mix of the
  legs' data. It needs four rare events in a row. State: accepted, written
  as a limit. Not seen in the lab.
- **A half-copied leg can look whole.** While md copies from the one leg it
  holds in sync and that leg dies, md can end the copy by marking the leg it
  copied onto in sync, with the regions it never copied in that leg's
  bad-block list. The leg that died gets a bad-block list too. The forced
  start drops the named leg's list, which is right for the leg that died.
  When the mark names the half-copied leg instead, hosts read, over the
  regions md never copied, whatever that leg's disk held there, possibly
  another pool's bytes, with no error. Both legs show the same change
  count. State: procedure to write, the forced-start guide: choose the leg
  that died last, from the leg rows; `dnvctl group inspect` shows each
  leg's bad-block count and md's view of it. Seen in the lab.
- **A failed stop during a forced start keeps a bad-block list.** When the
  named leg carries a bad-block list, the forced start starts the array,
  stops it at once, and starts it again with the list dropped. If that stop
  fails, the array runs with the list, and the legs added to it copy the
  list's regions as bad too. Hosts then get IO errors over those regions,
  though the named leg holds their data. State: accepted, with a procedure
  to write: stop the group's arrays with a level step and mark the group
  again. Forty such stops in the lab never failed.
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

- **A migration under way across an upgrade can place pieces of its list
  wrongly.** The new side keeps the pieces of the list of written blocks
  that the older build sent, and the worker sends only the pieces it does
  not see there. A new piece that follows an older piece of another length
  lands at the wrong place. The copy can then skip a block the source
  wrote, and the pool reads there the old bytes of the new side's extents,
  possibly another pool's, with no error. State: procedure, finish
  or cancel every migration before an upgrade. Fix later. Not seen in the
  lab.

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
operator does by hand. The third column gives the wait where a setting controls it. The
settings are the pool's thresholds and the check interval, in seconds,
and two flags of the worker.
Measured times are in [the introduction](introduction.md#status-and-limits-of-01).

| Fault | What dnv does by itself | The wait, where a setting controls it | By hand |
|---|---|---|---|
| A disk fails, one leg of a RAID1 group | md fails the leg. IO goes on over the other leg. Leg repair replaces the leg with a new leg on another disk node, and md copies the data onto it. The old leg becomes a retired leg. | `leg_unhealthy`, 1200 at the default, before the repair starts. | Nothing. In a pool without RAID the data is gone (accepted case 2). |
| A disk node is down and comes back | In a RAID1 pool, md fails its legs after the kernel's keep-alive. IO goes on over the other legs. A group with no IO sends md nothing to fail, so the primary fails such a leg in md itself once the leg has failed its health check twice in a row, while another leg holds the data in sync. md then records the death on the other leg, and a failover or a reboot can start the array from that leg alone. Back in time, its legs rejoin and md copies the changed regions. Down for long, leg repair moves each leg to another disk node. In a pool without RAID, the IO waits for the fast IO fail timeout and then fails (section 4). | `side_unhealthy`, 600 at the default, before the repairs start. | Nothing. A node that never returns needs the node replacement procedure (section 7). |
| A controller node is down and comes back | Each primary on it that has a healthy enabled standby is failed over to it. Hosts see a pause. A pool of one cntlr waits for a cntlr replacement. Back, the cntlrs that were failed over are standbys. A cntlr unhealthy for long is replaced on another controller node. | `primary_unhealthy`, 4 at the default, with two check rounds in a row that find the primary unhealthy; a round runs every `cntlr_interval`, 2 at the default. A missed round counts only once its own wait of one interval has run out, so a failover comes about 6 to 10 seconds after the node dies. `cntlr_unhealthy`, 600, before a replacement. | Nothing. |
| The network between a controller node and a disk node is cut | When the controller node holds the primary, as for a disk node down, seen from that controller node: md fails the legs behind the cut and takes them back when the cut ends. A long cut moves the legs. A cut from a node that holds only standbys changes nothing. | `leg_unhealthy`, 1200. | Nothing. |
| The network between hosts and a controller node is cut | Nothing. dnv reads the agents, never a host. The primary stays the primary and stays in the cdc's answer. The host's IO queues as long as a standby path is left. | None. | Disable the primary, so a failover runs at once. Enable it again after. State: procedure to write; fix later, a host health signal into the worker. |
| The network between the control plane and the agents is cut | The data plane keeps serving. The primaries of one cut-off controller node are failed over. The old primary never gets its demotion, and the fence comes after the three-second hold, so hosts that drop the old path inside the hold see a pause, and a host still on it sees IO errors (section 8). With every agent cut off, nothing changes until the network is back. | As for a controller node down. | Nothing. |
| etcd is down and comes back | The data plane keeps serving. No call and no reaction runs. A worker that cannot write to etcd stops driving its pools about 30 seconds into the outage, and drives again about one grace time after etcd is back. | The worker's flags: `--vote-interval`, 10 at the default, for the 30 seconds, which are three intervals; `--vote-grace-time`, 60, for the wait after. | Nothing. State: procedure to write, "running while the control plane is down". |
| A worker, the gateway or the cdc is down and comes back | Another worker, when one runs, takes the pools after a bounded window. The gateway has no state. Without the cdc, hosts keep their paths and get no path changes. | The worker's flags `--vote-interval`, 10 at the default, and `--vote-grace-time`, 60: a dead worker counts as gone after twice the interval, and the change then holds for the grace time, about 80 seconds in all. | Nothing. A failover during a cdc outage leaves hosts on a withdrawn path (section 6.1). |
| An agent restarts | The agent rebuilds its desired state from its local store, the files in which it keeps the desired state it last accepted. The kernel keeps the devices. A controller node agent away for more than a few seconds has its primaries failed over. | `primary_unhealthy` with two check rounds in a row. A stopped agent's rounds fail at once, so a failover comes about 4 to 8 seconds after the agent stops, at the defaults. | Until the fix, stop the workers first for a planned restart of a controller node agent. State: fix decided, the agent answers "starting" while it restarts (section 6.1). |
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
  State: open question, whether the primary should take the pool and the
  volumes above down to assemble the array in place. md's stop and plain
  assemble were seen in the lab on loop devices.
- **A forced start cut short after its zeroing can leave no leg to start
  from.** The forced start zeroes the md superblock of every other member
  it keeps, except the open repair's bad leg. When the operator withdraws
  the mark, or a zeroed member dies before its add, the zeroed members
  carry no superblock but still hold data. The primary does not create a
  new array over them, since the group's array has run before, so no data
  is lost (section 5.2 has the one case where the worker has not yet
  recorded that the array ran); the group stays down until a leg with a superblock is back, the
  named leg among them, which a new mark can name. If that leg is gone for
  good, the data on the zeroed legs can be reached only by hand with mdadm.
  State: procedure to write, the forced-start guide. Not seen in the lab.
- **A read-only or corrupt thin pool has no repair path.** A failed commit
  leaves a slice's thin pool read-only. Corrupt thin pool metadata has no
  check and no repair flow in dnv. State: procedure to write, the repair
  by hand onto fresh space, with the level steps, the names of the devices
  involved, and the tools thin_check and thin_repair, with a test of the
  cn agent suite. Fix
  decided: the agent reports the pool's mode (section 9).
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
  sector one leg cannot read with the data of the other leg; when that
  rewrite fails too, md records a bad block on the leg. One recorded bad
  block makes the leg unhealthy, so leg repair moves it. Slow legs: accepted; migrate the leg's side by
  hand. Not seen in the lab.
- **A whole copy still needs its source until the worker drains the
  clone.** The worker drains a clone once it reads the primary's report of
  the whole copy. If the primary moves to another cntlr, as in a failover,
  or its controller node reboots before that read, or the worker's own
  checks of the pool stop it, the new build of the dm-clone needs the
  source, and the volume waits while the source is away; so does an
  operator who steps the pool's level down past the clone and back before
  that read. State: accepted: the window is short, but the operator's
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
- **An old md superblock at the end of a new side can fail the leg.** The
  end of a new side is never zeroed. A host of an earlier pool may have
  written an md superblock of a format that sits at the end of a device,
  and mdadm then refuses the new leg as a member of another array, so the
  group reports an error. State: accepted. The failure is rare and shows
  as an error. A host of a running pool can write the same at any time, so
  zeroing the end of each new side would close only half of it. Not seen
  in the lab.
- **A refused side allocation never makes leg repair move the leg.** The
  leg's health goes on and off with each round. State: fix decided, the
  agent reports the refusal as an error too. Not seen in the lab.
- **A disk node that cannot serve a new leg gets a new repair after each
  cancel.** State: fix decided, the create skips a disk node whose side
  of the group reads error. Not seen in the lab.
- **A repair whose new leg the primary never reports stays open.** It
  blocks every other repair of the group. State: fix decided, the worker
  gives it up past the leg threshold. Not seen in the lab.
- **A repair's new leg that holds the whole data starts the array alone
  only where md recorded the other legs as failed.** When both legs of
  the group's list are dead, the group otherwise waits for its legs. An
  operator's forced start from the new leg waits too, for the leg of the
  list that is not the repair's bad leg. State: procedure to write, one
  line in the forced-start guide. Not seen in the lab.
- **After a forced start, a leg of the old identity can keep the group
  down.** A forced start from another leg leaves the repair's bad leg with
  the array's old identity. The primary does not add that leg back, since
  it may hold data the array lacks. After the repair is cancelled, the next
  assembly, as after a failover or a restart, finds legs of two identities
  and leaves the group down, its row in error. State: procedure to write,
  the forced-start guide: create a repair whose bad leg is the leg not
  wanted, which keeps its data as a retired leg, or force-start the group
  from the leg wanted. Not seen in the lab.
- **A primary that keeps a leaving member is failed over, and the new
  primary leaves that member out.** The primary never fails a member that
  is leaving the group while no member that stays is in sync on a working
  leg. While it keeps such a member, the group's row reads error, so the
  primary is failed over at its threshold. The new primary assembles the
  group from the legs of the group's list and the open repair's new legs,
  which leave that member out. State: fix later, to be judged with
  the forced start's second pass. Not seen in the lab.
- **A forced-start mark the primary did not act on waits for the
  operator.** The worker clears a mark only when the primary reports that
  its own forced start of that mark started the array. The primary keeps
  this in memory, so a mark whose start an agent restart forgot stays, and
  so does a mark written over an array that runs. While it stays, the
  worker makes no leg repair of the group, and the primary acts on the mark
  at the array's next stop. State: accepted; the operator cancels the mark
  (forced-start guide, to write). Not seen in the lab.
- **A forced start waits on a primary whose layers above hold the broken
  array.** Only a failover or a level step that stops the pools lets it
  run. State: procedure to write, one line in the forced-start guide.
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
- **A node's own converge whose call was cut off leaves part of its
  build waiting.** When the call of a disk node's or a controller node's
  own syncup is cut off, what its converge did not build, other than the
  node's nvmet port, waits for the node's next revision. What it did not
  remove is a leftover, which the next check round names, and the worker
  sends the syncup again. Sides and cntlrs do not wait: their next check
  round names what is left, but for the parts of a snapshot not yet made,
  which wait for the next revision. State: fix later. Not seen in the
  lab.
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
  window. State: procedure to write for the first two; fix decided for the
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
- **Restarting a controller node's agent fails over its primaries.** The
  agent answers no check round before its first converge ends. At the
  defaults a restart of more than about 4 seconds can fail over every
  primary on the node that has a healthy standby and has reported its
  stack built, and one of more than about 8 seconds does. Until the fix,
  stop the workers first, as an upgrade does, or create pools with a larger primary threshold
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

### Pools

| Action | Tool today | Doc today | State |
|---|---|---|---|
| Create and delete a pool | `dnvctl sp create`, `dnvctl sp delete` | the tutorials | done |
| Grow a slice | the worker grows it by itself; `dnvctl sp grow-slice` | none | procedure to write, one line: when to grow by hand |
| Change a pool's level | `dnvctl sp set-level` | none | procedure to write, with the thin pool repair |
| Move a leg to another disk node | `dnvctl migr create`, `migr finish`, `migr cancel`; the worker reads and hands over the leg's list itself | none; the introduction explains the flow | procedure to write: the whole flow |
| Repair a leg by hand | `dnvctl repair create`, `repair finish`, `repair cancel`, `repair abort`; the worker repairs by itself | none | procedure to write |
| Force-start a group | `dnvctl group inspect` shows what the primary reads from each member, and with `--leg` says what a start from that leg would do or why it would refuse; `dnvctl group force-start`, `dnvctl group set-operator-in-charge` | none | procedure to write: the forced-start guide. |
| Delete or keep a retired leg | `dnvctl retired-leg list`, `dnvctl retired-leg delete`, `dnvctl retired-leg keep`; a forced start keeps the group's retired legs by itself | none | procedure to write: when to keep a leg, when to clear the flag |
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
| Restart nvme-stas | none | this page, section 6.1; [dnv on one machine](tutorial_all_in_one.md) | procedure to write: stop stacd first, restart stafd, and start stacd once stafd holds its connection to the cdc again |
| Recover a host that keeps a withdrawn path | none | none | procedure to write. Fix decided for the node plugin. |
| Recover a Kubernetes mount after the loss of its last path | none; `nvme disconnect -n`, then delete the pod | this page, section 6.1 | procedure to write; no fix |
| Recover from a fault only a host sees | disable the primary | this page, section 6.1 | procedure to write. Fix later. |

### Steps known today

These are the longer steps written so far. The operations doc will hold
the full procedures.

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
  ends by the disk nodes' steps or by a forced cancel. Measured at 32 slices: one
  to two and a half seconds. State: accepted.
- **A migrating leg whose serving side's disk node dies waits without
  limit.** A migrating leg has two paths. The one not serving is live but
  inaccessible. The kernel keeps the leg's IO queued as long as that path
  stays live. md never fails the member. In a pool without RAID, all of
  the pool's IO waits.
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
  reports age, and the worker can count a missed round. State: accepted;
  the waits are the kernel's. Not seen in the lab.
- **The retry for a leg that joins late holds the primary's reads off
  and can fail it over.** A leg that is not available when the primary
  builds its group makes the agent run the cntlr's whole converge again
  every retry interval. Each attempt holds the cntlr's lock, so no read
  runs and the report ages. Two rounds answering from too old a
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
  of a written-block list holds that object's other work, and while the
  copy serves pauses the volume's IO, until the marks are done; a copy
  still in flight first holds the pause for a short bound. A grow whose new group cannot get ready waits while a repair,
  a retired leg, a forced-start mark, the operator-in-charge flag or a
  migration acts on that group. The hold delays the cntlrs' syncups by up to one
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
  subsystem no cdc serves, a frozen worker watch, a slow leg and a stuck
  repair show in a log line or not at all. The worker's log records the
  reactions it applied and the ones it skipped. No dnv program serves
  metrics, profiles or health over a port, though the CSI driver's
  manifests add a helper container that answers Kubernetes' health checks
  on port 9808 or 9809. State: not yet; log
  handling goes into the operations doc. Fix decided: one gateway read,
  with its dnvctl command, lists what is wrong across the cluster.
- **A read-only thin pool reads ok.** dm-thin turns a pool read-only on a
  failed commit. The pool's row stays ok; only its text, dm-thin's own
  status line, says so, and the worker reads only the usage numbers.
  State: fix decided, the agent reports the pool's mode. With the fix, a
  pool that dm-thin marks as needing a check, or a failed pool, will be an
  error row of a fault a failover cannot cure: a new primary would meet
  the same pool, so the worker will log it and start no failover. A pool
  that is read-only because its metadata is full will stay ok, and the
  auto-grow of the metadata is its cure. Not seen in the lab.
- **A repair that waits gives no reason.** State: fix decided, the primary
  reports the reason and the worker logs it. Seen in the lab.
- **A retired leg's partial mark can miss a short hold.** The worker marks
  a taken-back leg partial when md holds it but not in sync, from the
  report it reads each pass. A hold md begins and ends between two passes,
  or a failed add that still wrote md's record onto the leg, leaves no
  mark. The leg's own md superblock still shows what md left, which the
  forced start's read refuses, so a start from it fails with a reason.
  State: accepted. Not seen in the lab.
- **A retired leg's record does not say whether the leg was in sync when
  it left.** The record says whether the leg is kept, and whether md wrote
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
  one equal to its block, up to 1 MiB, so at the defaults an aligned 1 MiB
  discard frees one block. A pool's stripe is smaller than its block when
  the pool or its cluster names a smaller stripe, or when the pool's block
  is larger than its cluster's stripe, as every block above 1 MiB is. Then
  a discard frees a block on one slice only when it covers nearly the
  slice count times the block, and `fstrim` can report success and free
  nothing. Read from the kernel's code, not measured. State: accepted.
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
  means nothing. State: accepted. Procedure to write, one line.
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
  checked again once the agent reads the node once per interval (section
  8).
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
