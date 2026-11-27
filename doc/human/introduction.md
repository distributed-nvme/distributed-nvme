# dnv: an introduction

## What dnv is for

dnv is a distributed NVMe-oF block storage system. It aggregates the raw disks
of disk nodes into storage pools, runs the volume logic (thin provisioning,
striping, redundancy, snapshots, cloning, live migration) on controller nodes,
and exports virtual volumes to hosts over NVMe-oF with native NVMe multipath
and ANA, the NVMe way of saying which path serves IO. etcd holds all desired
state; the control-plane processes and the per-node agents converge the data
plane to it.

A host sees a volume as one namespace of an NVMe-oF subsystem. It may connect
to several controller nodes for the same volume; the kernel joins these paths
into one multipath device, and ANA picks the path. The data plane is built from stock Linux pieces: device
mapper, md RAID1 and the kernel's NVMe target and host drivers.

dnv can be the data plane of a multi-tenant block storage system, for example
behind Kubernetes or OpenStack Cinder. A tenant is a user of that system. dnv
knows no tenants and authenticates nobody: the layer above authenticates its
users and decides which storage pools, subsystems and hosts serve each of
them. dnv promises that layer that a host never reads, through the volumes of
one storage pool, the data of another storage pool, unless that layer exports
the data there itself, for example with a transfer. Steps that a caller gets
wrong and steps that an operator forces can break the promise, and [dnv's
scope and known limits](scope.md#3-the-promise-one-pool-never-reads-another-pools-bytes)
lists them. The promise is about what a host reads through its volumes, not
about the network or the nodes: it needs a trusted network under dnv and a
trusted root user on every node. Root on a host can pose as another host and
read that host's volumes, which the promise does not cover, so keeping
tenants apart also needs hosts where a tenant has no root.
[Status and limits](#status-and-limits) says who can read what.

The words are those of [the core glossary](core_glossary.md), the list to read
first; any other project word is explained where it first appears.

## The six programs

Three programs make the control plane, with etcd beside them. `dnv-gateway`
is the API server: it checks every request of an operator, writes any change
to etcd, and builds nothing on the nodes. `dnv-worker` makes the nodes follow
etcd, in its dn, cn and sp roles; it judges health and runs the reactions,
failover, auto-grow, cntlr replacement and leg repair, and several workers
share the work. `dnv-cdc` is the NVMe-oF discovery service: it tells a host
where its subsystems are, and when that answer changes. etcd holds the
desired state of every node and storage pool, and only those three talk to
it.

`dnv-agent` runs on every node, as the dn agent on a disk node and the cn
agent on a controller node: it builds and removes the local devices (device
mapper, md, nvmet, NVMe connections) so that the node matches its desired
state, reports the real state, keeps the last desired state it accepted in
local files, and never talks to etcd. The same program also checks a node
before it joins (`dnv-agent check dn` or `check cn`, the node check) and
gathers a node's state for a bug report (`dnv-agent collect`, the gather);
neither starts an agent ([operations](operations.md#check-a-node-before-it-joins)).
`dnvctl` is the command line tool of the operator, one command for each
call of the gateway API.

`dnv-csi` connects dnv to Kubernetes through the Container Storage Interface
(CSI), the plugin API Kubernetes uses to create, attach and mount volumes.
Every Kubernetes volume is a storage pool of its own with one thin device, one
subsystem and one namespace. Its controller plugin talks to the gateway; its
node plugin, one per Kubernetes node, asks the cdc where the subsystem is,
connects, formats and mounts, and it follows the discovery log itself,
connecting and dropping paths as the cdc's answer changes, so that node needs
no nvme-stas, the Linux program that does this for a host.

## The physical view

Hosts talk NVMe-oF to the controller nodes, and the controller nodes talk
NVMe-oF to the disk nodes, where all volume data lives. The control plane
servers read and write etcd and reach every node through its agent over gRPC,
the remote-call protocol of the programs, which carries no IO; the operator's
`dnvctl` talks gRPC to the gateway, and hosts ask the cdc on those servers
where their subsystems are.

```mermaid
flowchart LR
    OP["operator<br/>(dnvctl)"]
    H["hosts"]
    subgraph dp["data plane"]
        subgraph cns["controller nodes"]
            cnA["controller node<br/>(cn agent)"]
            cnB["controller node ..."]
        end
        subgraph dns["disk nodes"]
            dnA["disk node<br/>(dn agent, one raw disk)"]
            dnB["disk node ..."]
        end
    end
    subgraph cp["control plane"]
        cpA["control plane server 0<br/>dnv-gateway, dnv-worker, dnv-cdc"]
        cpB["control plane server 1 ..."]
        ETCD[("etcd")]
        cpA <--> ETCD
        cpB <--> ETCD
    end
    OP -->|"gRPC"| cpA
    H ==>|"NVMe-oF: the volumes"| cns
    cns ==>|"NVMe-oF: the legs"| dns
    H -.->|"NVMe/TCP: discovery"| cpA
    H -.->|"NVMe/TCP: discovery"| cpB
    cpA -->|"gRPC"| cns
    cpA -->|"gRPC"| dns
    cpB -->|"gRPC"| cns
    cpB -->|"gRPC"| dns
```

*The physical view: hosts, the data plane and the control plane; IO over
NVMe-oF, control over gRPC.*

## How a change travels

The gateway checks the request, writes the change to etcd and raises the
revision of the node or pool it touches. The worker sees the revision move and
sends the agent a syncup, which carries the whole desired state. The agent
converges: it reads what the node has now, builds what is missing and sweeps
what is not wanted. The check round runs the other way, at a fixed interval:
the worker asks, and the agent answers with the revision it holds, whether the
node is clean, and the real state when it changed, as rows. From the rows the
worker judges health, and a reaction is one more change in etcd, which travels
the same way down.

```mermaid
flowchart TB
    OP["operator"] -->|"a command"| CTL["dnvctl"]
    CTL -->|"one gRPC call"| GW["dnv-gateway"]
    GW -->|"checks the request,<br/>writes the change,<br/>raises the revision"| ETCD[("etcd")]
    ETCD -->|"watch: the revision moved"| W["dnv-worker"]
    W -->|"syncup:<br/>the whole desired state"| AG["dnv-agent<br/>(dn or cn)"]
    AG -->|"converge: build what is missing,<br/>sweep what is not wanted"| DEV["the node's devices:<br/>device mapper, md, nvmet,<br/>NVMe connections"]
    W -.->|"check round,<br/>at a fixed interval"| AG
    AG -.->|"the answer: the revision held,<br/>clean or not, the real state as rows"| W
    W -.->|"health epochs,<br/>reactions"| ETCD
```

*How a change travels down to the devices, and the check round's answer back
up.*

## A storage pool

On the controller nodes a pool has cntlrs: exactly one is the primary, which
serves IO; the others are standby, connected to the legs and ready to take
over. On the disk nodes it has space, organized as slices, groups, legs and
sides. A slice is one thin pool on the primary; a meta group holds that thin
pool's metadata and a data group its data; a group with redundancy is an md
RAID1 array over its legs; a leg is one copy, stored as a side on one disk
node. A thin device is one dm-thin volume in every slice, joined on the
primary by one striped device, and a host sees it as a namespace of a
subsystem, which every cntlr exports under the same NQN, the name of an
NVMe-oF subsystem, ANA optimized on the primary and inaccessible on the
standbys.

```mermaid
flowchart TB
    CL["cluster"] --> SP["storage pool"]
    SP --> PR["primary cntlr<br/>(on controller node 1)"]
    SP --> SB["standby cntlr<br/>(on controller node 2)"]
    SP --> SLA["slice 0<br/>(one thin pool on the primary)"]
    SP --> SLB["slice 1 ...<br/>(one thin pool each)"]
    SLA --> MG["meta group<br/>(md RAID1 over its legs)"]
    SLA --> DG["data group<br/>(md RAID1 over its legs)"]
    MG --> legA["leg"] --> sideA["side on disk node 1"]
    MG --> legB["leg"] --> sideB["side on disk node 2"]
    DG --> legC["leg"] --> sideC["side on disk node 3"]
    DG --> legD["leg"] --> sideD["side on disk node 4"]
    SLA -.-> TD["thin device:<br/>one dm-thin volume per slice,<br/>striped on the primary"]
    SLB -.-> TD
    TD --> NS["namespace of a subsystem:<br/>exported by every cntlr,<br/>ANA optimized on the primary"]
```

*A storage pool: cntlrs, slices, groups, legs, sides, and a thin device over
the slices.*

The space of a pool is the size of its groups, one copy. Each leg takes the
size of its own group from its disk node, and each cntlr, the primary and
every standby, takes the size of all the pool's groups from its controller
node's budget, the most pool space the node's cntlrs may serve, set with
`--capacity` on its cn agent and the largest, 64 TiB, when unset; a
controller node that 0.1 created with no budget keeps the 4 TiB of 0.1.
So the space a cluster can give pools is bounded by the free space of its
disks divided by the legs of a group, and by its controller nodes' budgets
divided by the cntlrs of a pool. `dnvctl cluster get-capacity` reads both for a pool of a given
shape and names the limit the cluster reaches first
([operations](operations.md#the-clusters-capacity)).

## Failover

The worker starts a failover when the primary is disabled or has been
unhealthy for longer than the pool's threshold. The old primary is out of the
cdc's answer by then, taken out by the failover's own change or by the disable
that triggered it, so the cdc tells the hosts, and a host that follows the
discovery log drops that path and queues its IO. The worker does not wait for
that: it drives the pool's syncups in a fixed order with a hold between the
steps. First the old primary's demotion: it moves its namespaces to ANA
inaccessible before anything else, so that the host is told to stop using the
path before the path stops working, then puts its namespace devices on an
error target, a device-mapper table that fails every IO, and stops its arrays
and thin pools; its legs stay connected. Then the fence: each disk node
reloads the old primary's path onto an error target and the new primary's
onto the side device, and moves ANA with them. In each RAID1 group the worker
fences the leg md copies from last: it tells that leg's disk node only once
the other legs' disk nodes report their fence done, or after five intervals,
10 seconds at the default, and it does not wait for a disk node that does not
answer. The order keeps a write that an old primary still serving completed
on that leg, the one the new primary's start of the array copies from, but
only in the groups whose other disk nodes report in time. The worker keeps
that leg in the group's record, so a worker that restarts knows it too.
[The scope page](scope.md#51-hosts-flushes-and-snapshots) says when a write
can still be lost. Then the new primary assembles
the arrays, builds the thin pools and the striped devices, and only then moves
its namespaces to ANA optimized. In the common case, a dead controller node,
the host sees a pause, not errors: its IO queues until the new primary moves
its namespaces to ANA optimized.

```mermaid
sequenceDiagram
    participant W as dnv-worker (sp role)
    participant O as old primary (cn agent)
    participant S as sides (dn agents)
    participant N as new primary (cn agent)
    participant H as host
    Note over W,O: the primary is disabled or unhealthy too long:<br/>the worker writes the failover to etcd
    Note over N,H: the cdc tells the host that the old primary<br/>is no longer listed
    W->>O: syncup, the demotion: you are a standby
    O->>H: namespaces to ANA inaccessible, the host queues its IO
    Note over O,S: namespace devices onto an error target,<br/>arrays and thin pools stop, legs stay connected
    H->>H: drops its path to the old primary
    Note over W,O: hold: the worker waits for the demotion's report,<br/>bounded in time
    O-->>W: demotion applied
    W->>S: syncup to the other legs' sides: the primary changed
    Note over S,N: fence: the old primary's path onto an error target,<br/>the new primary's onto the side device
    S-->>W: fence done
    W->>S: syncup to the side of the leg md copies from
    S->>N: the new primary's paths ANA optimized, the old primary's non-optimized
    Note over W,S: hold: the worker waits for the sides' reports,<br/>bounded in time
    S-->>W: revision applied
    W->>N: syncup: you are the primary
    Note over S,N: assemble the RAID1 arrays, build the thin pools<br/>and the striped devices, namespace devices onto them
    N->>H: namespaces to ANA optimized
    Note over N,H: the queued IO flows: a pause, not errors
```

*A failover: demotion, fence, the new primary's build, what the host sees.*

## Migration

For the time of the migration the leg has two sides; once the new side is
provisioned both export the same subsystem, so the primary's kernel sees one
namespace with two paths and nothing above the leg changes. The operator starts the migration; the new side
is provisioned on the other disk node, and the primary connects to it. The
worker starts the cutover only once the primary has a live path to the new
side, and its thin pool keeps the hosts' discards: a discard, the message
that a range holds no data any more, then frees the block in the pool and
goes no further, so it never reaches either side. At the cutover the old
side stops serving, and ANA moves the IO to the new side's
dm-clone, the device-mapper target that copies a source device into a
destination device while serving IO. The dm-clone pulls what is missing from
the old side on demand and copies the rest in the background; its metadata
lives on the disk node's disk, so a reboot resumes the copy. Once the old side
takes no more writes, the worker reads the leg's bitmap, the list of the blocks
the pool has written on the leg, and hands it to the new side, so that the copy
skips the rest. When the copy is complete the operator finishes the migration,
and the old side is removed.

```mermaid
flowchart LR
    subgraph cn["primary cntlr"]
        LEG["leg device:<br/>one multipath namespace<br/>with two paths"]
    end
    subgraph dnOld["disk node 1: the old side"]
        OS["side device"]
        OX["export to the primary<br/>(serves until the cutover,<br/>then an error target)"]
        MX["migration source export<br/>(for the new side alone)"]
        OS --> OX
        OS --> MX
    end
    subgraph dnNew["disk node 2: the new side"]
        NC["connection to the old side"]
        DC["dm-clone:<br/>pulls what is missing on demand,<br/>copies the rest in the background"]
        NSD["side device (the copy)"]
        NX["export to the primary<br/>(serves from the cutover on)"]
        NC -->|"source"| DC
        NSD -->|"destination"| DC
        DC --> NX
    end
    MX ==>|"NVMe-oF"| NC
    OX -.->|"path 1: ANA optimized until the cutover,<br/>then inaccessible"| LEG
    NX ==>|"path 2: ANA optimized from the cutover on"| LEG
    subgraph steps["in order"]
        direction LR
        stepA["operator starts<br/>the migration"] --> stepB["new side provisioned,<br/>primary connected to it"] --> stepC["cutover: the old side stops,<br/>ANA moves IO to the new side"] --> stepD["copy complete"] --> stepE["operator finishes:<br/>old side removed"]
    end
```

*A migration: one leg with two sides, the copy, the cutover and the finish.*

## Transfer and clone

The host on the right has one multipath device over the exports of two pools,
because the destination pool exports the same subsystem NQN and the same
namespace identity as the source. The origin is the volume being moved. A
transfer in the source pool exports the origin's bytes over a subsystem made
for this purpose, to the destination's cntlrs alone; created with the option
a live migration uses, it also parks the origin namespace, that is, moves it
to ANA inaccessible on every cntlr. A clone in the destination pool pulls those bytes
with dm-clone into a new, empty thin device, keeps the copy's metadata on the
clone meta group, a group of the pool made for this, and moves its namespace
to ANA optimized once the dm-clone serves; until that move the host's IO
queues. When the copy is complete, the clone leaves by itself: the worker
sees the whole copy in the primary's report, marks the clone as deleting
and drains it, and the new thin device then serves the namespace alone. The
operator deletes the transfer, which keeps the origin parked. An abort is lossless only while nothing has written through
the destination. The two pools can be in different clusters.

```mermaid
flowchart LR
    subgraph src["source pool (primary cntlr)"]
        OT["origin thin device"]
        ON["origin namespace<br/>(parked: ANA inaccessible)"]
        TX["transfer subsystem:<br/>exports the origin's bytes<br/>to the destination's cntlrs alone"]
        OT --> TX
        OT -.-> ON
    end
    subgraph dst["destination pool (primary cntlr)"]
        SC["connection to the transfer"]
        DC["dm-clone:<br/>pulls what is missing on demand,<br/>copies the rest in the background,<br/>metadata on the clone meta group"]
        DT["new thin device<br/>(empty at the start)"]
        DNS["same subsystem NQN,<br/>same namespace identity<br/>(ANA optimized once the dm-clone serves)"]
        SC -->|"source"| DC
        DT -->|"destination"| DC
        DC --> DNS
    end
    TX ==>|"NVMe-oF"| SC
    H["host:<br/>one multipath device<br/>over both pools' exports"]
    ON -.->|"inaccessible"| H
    DNS ==>|"optimized"| H
    DC -.->|"copy complete"| FIN["the worker drains the clone (the namespace<br/>is served by the new thin device alone);<br/>delete the transfer (the origin stays parked)"]
```

*A transfer and a clone: the source exports, the destination pulls, the host
moves over.*

## Requirements

dnv is tested on Ubuntu 26.04 LTS with the kernels 7.0.0-42-generic and
7.0.0-43-generic, the lab VMs' distribution and kernels of the 7.0 series,
and with mdadm 4.5. The md behaviour that a failover's order of fences
rests on was checked by hand on the first of these kernels, on one lab VM
([the scope page](scope.md#notes-on-the-maintenance-tools)). The agents
need a kernel of the 6.1 series or newer, the first with every kernel
feature they use but one: on a controller node, ending the connection of a
host taken off a subsystem's list needs 6.11 or newer, built with the
option below, unless the cn agent runs with `--ignore-host-disconnection`.

Run the node check, `dnv-agent check dn --disk <disk>` on a disk node and
`dnv-agent check cn` on a controller node, as root, before a node joins
([operations](operations.md#check-a-node-before-it-joins)). It fails a node
the agent cannot work on or whose setup a failover or a repair is not safe
on: a kernel older than 6.1, a udev older than systemd 253's, a missing tool
or module, nvmet's configfs tree missing (configfs not mounted), on a
controller node a kernel that cannot end a host's connection to it (below)
unless the check is given `--ignore-host-disconnection`, native NVMe
multipath off, one of dnv's three udev rules missing or masked, no mask of
the system's md assembly that the check counts (dnv's md rule exactly as
shipped, or mdadm.conf's AUTO line refusing every format, as `AUTO -all`
does, with no `homehost` on it, no POLICY line that sets `auto=`, no ARRAY
line of a dnv array and none that names devices but no uuid),
`nvme_core.io_timeout` at its largest value on a controller node, or a disk
without 512-byte blocks. It warns for four kinds of fact. Node settings to
fix: lvm2 able to read dnv's own devices, mdadm.conf not set as the node
settings below say, a module a reboot would not load again, a controller
node's kernel older than 6.19, where every controller node of the cluster
must run one kernel series (below), and `nvme_core.io_timeout` at its
largest value on a disk node. A known limit: a disk whose best IO size is
above 4 MiB, to which a controller node's write can be refused, and, with
`--ignore-host-disconnection`, a controller node that cannot end a host's
connection to it. What dnv was not tested with: a kernel outside the 7.0
series, on a controller node an mdadm other than 4.5 or
`nvme_core.io_timeout` other than 30 seconds, and a udev rule of dnv's
with another text. And a fact the check could not read, which the operator
then reads by hand; a failed read of a fact the agent needs, such as the
kernel release, the disk's block size, udev's version or the module
lists, fails the node instead. On a spare machine,
`dnv-agent check cn --md-replay` also runs md's start rules, which a
failover rests on, with that machine's mdadm and kernel.

Tools: nvme-cli, mdadm, nvme-stas, a 2.x version with its daemons stafd and
stacd, dmsetup, and thin-provisioning-tools, whose thin_dump the agents run,
all of which the all-in-one tutorial installs, and losetup from util-linux for
its loop-device disks, which the cn agent also runs. The agents also run
blkdiscard and lsblk from util-linux. nvme-stas is 2.x because dnv is tested with 2.x alone; 3.x is
untested with dnv.

Kernel: the NVMe/TCP target and host drivers nvmet, nvmet-tcp, nvme-tcp and
nvme-fabrics; native NVMe multipath switched on, which joins a host's paths
into one device and carries the ANA state; md RAID1; device mapper with the
thin-pool, thin, clone, linear, striped, error and flakey targets; loop
devices on a controller node; and configfs mounted. Every controller node of a cluster runs one kernel series: a RAID1
array created on a 6.19 or newer kernel is refused by an older one, so a
failover to a controller node on the older kernel cannot start the pool. On a
controller node `nvme_core.io_timeout` keeps the kernel's default, 30 s, and is
never raised to its largest value, 4294967295, which some cloud images set so
that an IO never times out: it is the only bound on a write to a disk that
stalls while its disk node still answers the keep-alives of the connection,
the messages by which the kernel finds a silent target.

A controller node ends the connection of a host taken off a subsystem's list,
so that the host's IO fails and another host can take the volume over. The
kernel can end one host's connection when it is 6.11 or newer, its NVMe target
is built with the kernel option `CONFIG_NVME_TARGET_DEBUGFS` and loaded,
configfs is mounted, debugfs is mounted at /sys/kernel/debug, and the kernel
is not under lockdown, the mode that Secure Boot switches on. The kernels of
most distributions, Ubuntu, Debian and Fedora among them, are built without
the option, and dnv gives no way to build it. Where the cn agent finds, when
it starts, that the kernel cannot, or cannot read whether it can, it starts
only with `--ignore-host-disconnection`, and then ends no connection until it
is started again, even once the kernel can: a host taken off a list keeps its
connection and its IO until it disconnects ([the scope
page](scope.md#54-hosts-and-the-kubernetes-driver)). So load nvmet, and mount
configfs and debugfs, before the cn agent starts. Where the cn agent started
without such a finding, debugfs must stay mounted while it runs, or that
controller node serves none of its volumes, save a namespace whose move to ANA
inaccessible fails ([the scope
page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)). A lockdown
on a node where /sys/kernel/security is not mounted is the exception: the cn
agent cannot see it and starts without the flag, and the kernel then refuses
to end a connection ([the scope
page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)). The end of
a connection is tested on 7.0.0-43-generic with the NVMe target modules
rebuilt with the option.

Disks: the disk of a disk node, a whole disk or a partition, has 512-byte
logical blocks. A disk of another block size, such as 4 KiB, cannot become
a disk node: the dn agent refuses it, and its error says how to change it,
with `nvme format` to a 512-byte format, which erases the disk, or with a
loop device set up by `losetup` without `-b` and without `--direct-io`, or
with `-b 512`. With direct IO and no `-b`, a loop device takes 4 KiB blocks
when its file lies on a disk of 4 KiB sectors.

A controller node holds no subsystem of anyone else that has no namespace
that names a device, no host and no port: the cn agent's sweep removes such
a subsystem once it is 30 s old, whoever made it, since a build or a
removal of dnv cut short leaves that shape. A namespace made but never
given a device does not keep it. And nothing but the cn agent connects with
the host NQN, the name a host gives when it connects, that the cn agent
uses on its node: the agent removes a connection opened with that name once
no clone reads through it.

Node settings: on every disk node and controller node, the three udev rules
in the release's `deploy/udev/` folder, which keep the system's own md
assembly, LVM scan and bcache off the devices dnv builds and off what hosts
write into them, on disk nodes too, since a controller node's md superblocks
reach their devices through the side exports; the rules need systemd 253 or
newer; no device-mapper device of the node's own whose name starts with
`dnv-`, such as a volume of an LVM volume group named `dnv` or `dnv-…`,
since dnv's udev rule keeps the system's own rules off every such device;
lvm2 set to read only the node's own physical volumes; `AUTO -all` in
`/etc/mdadm/mdadm.conf`, after an ARRAY line for each md array of the node's
own, so that md assembles by itself only those; and, on a controller node,
the node's own file systems mounted by device path, never by a label or a
uuid a tenant could copy. [The operations
doc](operations.md#install-the-udev-rules-on-a-node) has the steps. The
tutorials install the udev rules, and mask the nvmf-connect unit, so that
nvme-stas is the only program that connects to what a discovery change
announces. The node check fails a node where one of the three udev rules is
missing or masked, and warns when lvm2 can read dnv's own devices or
mdadm.conf is not set this way. It judges no udev rule of the node's own: a
rule that does the work of dnv's md rule does not count. It cannot tell
whether lvm2 reads a leg's NVMe device or a clone's source, which carry no
dnv name, and it does not read how the node's file systems are mounted.

Host names: every machine has a host name of its own, which is the
location of each node created on it with no location, so the nodes of one
machine are one location; a machine whose host name is not a valid
location, as the kernel's `(none)`, needs a location at each node create
([operations](operations.md#choose-a-nodes-location)).

etcd: a release of 3.5.13 or later, run with the request caps and automatic
compaction the tutorial's etcd command shows; the tutorial and the suites use
v3.6.14.

A host: an NVMe/TCP initiator with native multipath and a follower of the
discovery log such as nvme-stas, which drops a failed-over primary's path; a
path that no follower manages is never dropped. nvme-cli's own connect unit
is switched off on every host
([operations](operations.md#give-hosts-the-cdc-addresses)): it connects the
paths a cdc announces with reconnects that give up after 600 seconds, and it
is a second program that connects the host's paths.

To build: Go 1.26.5 or newer. For the CSI driver: Kubernetes 1.34 or newer,
k3s in its tutorial, and docker to build the driver's image. Every Kubernetes
node that mounts dnv volumes runs a kernel of the 6.1 series or newer, the
series whose xfs features the driver formats with, so that every such node
mounts every xfs volume the driver makes; an xfs volume made by 0.1 needs
5.19 or newer, which the same floor covers. Such a node also needs native
NVMe multipath on and the module nvme-fabrics loaded at boot, which the
release's `deploy/modules-load.d/dnv-host.conf` does once copied into
`/etc/modules-load.d`: it names nvme-tcp, which loads nvme-fabrics. The
node plugin refuses to start without either, and names the fix
([operations](operations.md#a-kubernetes-node-plugin-that-does-not-start)).
Such a node, a host too, also needs nvme-cli's own connect unit switched
off; the node plugin lifts the 600-second limit on its volumes' paths
should the unit run.
The tutorials load nvme-tcp by hand, which lasts until a reboot. Each
Kubernetes node needs a host NQN and a host id of its own, in
`/etc/nvme/hostnqn` and `/etc/nvme/hostid`. The driver takes the host NQN
as the node's name. Machines made from one image share the files that
nvme-cli's package wrote at install. Two nodes with one host NQN defeat
the driver's detach, which then no longer keeps a lost node off a
volume ([the scope page](scope.md#54-hosts-and-the-kubernetes-driver)).
`kubectl get csinodes -o yaml` prints each node's id, its host NQN: two
nodes with one id share a host NQN. The driver's pods, the Kubernetes units of one or more containers that run its
plugins, live in a Kubernetes namespace of their own, `dnv-csi`: a named
group of Kubernetes objects such as pods. The driver's file for it under
`deploy/` asks Kubernetes' pod security admission, the check that refuses a
pod that wants more rights than its Kubernetes namespace allows, to allow
privileged pods there, since the node plugin's pods are privileged. The
driver's pods take Kubernetes' critical priority classes, which make
Kubernetes find room for them first and stop them last when a machine runs
short of memory. k3s allows those classes in any Kubernetes namespace; a
Kubernetes cluster that allows them only in `kube-system`, Kubernetes' own
Kubernetes namespace, refuses the driver's pods until the operator takes the
classes out of the driver's files. Where other pods share the Kubernetes
cluster and its network software enforces network policies, give every
Kubernetes namespace of theirs a copy of the driver's example network
policy, a Kubernetes record that limits where the pods of one Kubernetes
namespace may connect. Where a dnv machine is also a Kubernetes machine,
leave `kube-system` out, since the policy keeps its pods off that machine's
own services too ([the CSI
tutorial](tutorial_csi.md#keep-other-pods-off-dnv)).

## Status and limits

0.1 is the first release of dnv. These documents describe a later build of
the repository: every part of that build is exercised by the integration
suites on lab VMs and by the two tutorials, which 0.1 itself cannot run
([Get dnv](#get-dnv)). No production deployment is known.
Linux x64 is the only platform.

dnv trusts the network, and the root user of every node and of every host,
so a tenant must not be root on a host. dnv authenticates no user, and no
host beyond its host NQN, a name that any machine can claim. gRPC and
NVMe/TCP carry plain text, and nothing is encrypted, on the wire or on the
disks. The table below says who can read what, and [the scope
page](scope.md#3-the-promise-one-pool-never-reads-another-pools-bytes) says
what the promise covers and which steps can break it.

A host needs to reach only the NVMe/TCP ports of the controller nodes and
the cdc, which sit on the host network, the one the hosts are on. The rest
belongs on a back network, one that hosts cannot reach: the disk nodes'
NVMe/TCP ports, the gateway, etcd, and the agents' ports, where the agents
take the calls of the gateway and the worker. Under Kubernetes, the machines
that may run the driver's controller plugin reach the gateway too. [The
operations doc](operations.md#the-networks) has the table of who connects to
whom, and a firewall example. Deploy dnv on an isolated network you trust.

| Who | What they can read |
|---|---|
| A tenant inside a volume, whose programs read and write only through the volume's namespace | Only what was written to that volume, and what the layer above put in it with a snapshot or a clone: a snapshot starts with what its origin held, and a clone with what its source holds. Every other block reads as zeros, whatever the disk holds there. This is the promise, and the scope page lists the steps that can break it. |
| Root on a host, or any machine or Kubernetes pod that reaches an NVMe/TCP port | Every volume whose subsystem lets in a host NQN that it claims, since a host gets in by its NQN alone, with no key: it can read and write those volumes, and the cdc tells it where they are. At a disk node's port, it can read and write the whole leg, old bytes included, when it claims the NQN of the controller node that holds the pool's primary. The NQN of a controller node that holds a standby of the pool gets in too, but its reads and writes fail until a failover makes that standby the primary. From a migration's cutover until the migration ends, it can read and write the whole old side when it claims the NQN of the destination disk node. These NQNs that dnv makes hold the cluster's id, a hash of the cluster's name and of the moment the cluster was created, so they cannot be worked out from a name alone. But they can be built from the cluster's id, which the gateway shows and etcd holds, and they cross the network in plain text, so whoever reads them can claim them. A machine on the traffic's path also reads the IO itself. |
| A machine that reaches the gateway, an agent's port or etcd | Every volume, since none of them asks for a login: through the gateway or etcd, and through a controller node's agent port too. That agent takes from each call the id of the controller node it acts as, so a caller can make it connect to the legs of any pool as the pool's primary. Through a disk node's agent port, what that node's disk holds. Through the gateway it can put a host NQN of its own on any subsystem's list, and then read as the row above says. Through an agent's port it can make the node build, export or remove what it asks. Through etcd it can write any desired state, which the worker then carries out. Through the gateway or etcd it can also delete every volume and every pool, and through a controller node's agent port the volumes of any pool. |
| Root on a node | Everything on the node: the raw disk with every side on it, the agent's local files, and on a controller node every device it builds, so the volumes of every pool whose primary runs there. A disk node's disk keeps the old bytes of freed extents, since nothing zeroes an extent when it is freed, and a new data leg is zeroed only over its first blocks. Root on a node also reaches the NVMe/TCP ports, and the back network, where the gateway, etcd and the other agents' ports are, so it can do what the second and the third rows say. |
| Someone holding a disk node's pulled disk | Every block written to the legs on it, the old bytes of its freed extents, and the disk's own table of which extents make up each side. Nothing on it is encrypted. |

Only the first row is dnv's promise. The other rows are held back only by
how dnv is deployed: a host network that only the hosts and dnv's own
machines reach, hosts where a tenant has no root, a back network that hosts
cannot reach, under Kubernetes a network policy that keeps the other pods
off dnv's machines ([the CSI
tutorial](tutorial_csi.md#keep-other-pods-off-dnv)), and people you trust as
root on the nodes and with the disks. The cn agent suite's isolation case,
in `integtest/cnagent_test.sh`, shows the first row's zeros on a real
kernel: blocks never written, over disks that held old bytes, read as zeros
around a write, on the host and on both legs. In the same suite, the thinbm
case reads a snapshot as its origin's blocks, with zeros where the origin
held none, and the clone_xfer case reads a clone as its source: the
source's pattern where the source was written, and zeros past it. The other
rows follow from the code, which asks for no login and encrypts nothing.
The suites show part of them by the way they work: they call the gateway
and the agents with no login, and the dn agent suite, in
`integtest/dnagent_test.sh`, writes and reads legs through a disk node's
exports from machines that only claim a controller node's NQN. Two cases read, as root on a node, the old bytes that the last
two rows name: the dn agent suite's zeroing case finds a new side of a data
group zeroed over its first blocks and an old pattern past them, and the
cn agent suite's isolation case finds the old pattern on both legs in a
block that no write reached. No test reads a pulled disk.

[dnv's scope and known limits](scope.md) says what dnv promises, the data
loss it accepts and what it does not do. It also gives every other known
limit of dnv, with its state and what you can do about it; the sizes
below and the limits of the test suites are not in it. Read it before you
deploy dnv.

An fsync across the death of a primary is no guarantee today. A write to
a block that a volume never wrote before is recorded in its slice's thin
pool at the thin pool's next commit, which comes at a flush or about once
a second. When the primary dies while a host's fsync waits for that
commit, the host's kernel can complete the fsync as a success, and those
writes are gone after the failover. dnv's own programs cannot fix this:
the fix waits for a kernel whose thin pool has an option that holds such a
write back until the commit, and for dnv to use that option. On a
controller node whose kernel is older than 6.7, a host can also send no
flush at all after a failover until it reads the new primary's path again,
so that an fsync makes nothing durable. [The scope
page](scope.md#51-hosts-flushes-and-snapshots) has both cases, beside the
other cases of hosts and flushes; its sections 4 and 5 list every known
way to lose a completed write.

0.1 publishes no throughput or latency figures. The failover pause was
measured on the lab VMs as the longest write queued through a failover of a
primary whose agent is stopped while its kernel goes on serving. The figure
therefore leaves out the time the worker takes to judge the primary
unhealthy, which a dead controller node adds. For a 4 KiB write with a
flush to the raw device of a host under nvme-stas, the pause was about 2 s
at one slice (commit 1b1cba4), and, on builds of ed69649 with uncommitted
changes, about 2 s at one slice, 3 s at four slices and, in the one run at
32 slices, 22 s. For a 1 MiB write with a flush to a file on a volume
mounted under the CSI driver, whose pools have one slice, it was about
3.6 s (commit 1b1cba4).

The write pause when a disk node dies was measured on the lab VMs (commit
40eef7f) as the longest 4 KiB write with a flush, one write at a time, to a
RAID1 pool while one of its disk nodes sent nothing back, as a dead node
does. The primary's controller node dropped its connections to that node
6.4 to 10.2 s after it went silent. The write in flight waited 9.2 to
10.0 s over five runs at the default fast IO fail timeout of 5 s, and 9.0
to 10.2 s over six runs at 1 s and at 15 s: the setting does not change
it, and no write failed. IO that reached the lost leg after the keep-alive
gave up waited until the fast IO fail timeout ended, 1, 5 or 15 s after
the drop. On a pool with no RAID1 the write failed after 13.2 s at 5 s and
after 23.3 s at 15 s.

The write pause while a volume grows was measured on the lab VMs on
2026-11-20 with fio filling a fresh 4 GiB volume of the CSI driver from its
first block to its last, with direct IO and eight writes of 1 MiB in
flight. The pool started with a data group of one eighth of the volume and
grew eight times while the fill ran. Each grow took about 4 s from the pool
passing half of its data space, and the writes spent most of the fill
waiting for the grows: the grows, not the device, set how fast the volume
filled. No write failed and every block read back. The longest write took
at least 6.1 s, a write that waited for a grow.

The loss of every leg of a data group at once was measured on the lab VMs
(commit 008fa9c7) with a 4 KiB write with a flush, one write at a time,
each into a block the pool had not mapped, to a RAID1 pool whose data group
lost every path from its primary while its meta group kept them, at the
default fast IO fail timeout of 5 s; each time counts from just before the
cut began. A cut of
3.3 s held the write in flight for 3.1 to 3.3 s over six runs, and one of
3.8 s for 6.3 to 6.4 s over three; no write failed. Cuts of 8.5, 20.5 and
60.5 s each broke the array: md failed its first member 9.6 to 10.2 s after
the cut began, the first write failed 14.9 to 15.5 s after it, and every
later write failed at once. The thin pool then read
read-only, needing a check, on its primary, and every block the host was
told was written read back as written. A failover to a controller node
none of whose paths to the legs the cut reached then checks the pool's
records, clears the mark and brings the pool back writable; the cn agent
suite showed that on 2026-11-27 with an 8-second cut and its failover.

The loss of a controller node that holds many pools was measured on the lab
VMs with the e2e suite's many-pools case: one-slice RAID1 pools with two
cntlrs each over three controller nodes, both hosts writing every pool with
one synced 4 KiB write a second, at the default primary threshold. With
every packet of the node that held the most primaries dropped, each of its
pools failed over 6.1 to 9.9 s after the cut at 8, 32 and 64 pools, and both
hosts saw it served by its new primary at most 13.6 s after the cut at 8
and 32 pools and 25.8 s at 64; the longest write was just under that, and
no write failed while the node was cut off. When the node came back, a host
whose connection to it came back before the node had made its old
primaries standbys had its writes there fail for a few seconds: none at 8
pools, 15 in one run at 32 and none in the other, and at 64 pools 112, 56 on
each host, in one of the two runs that reached the cut and none in the other
([dnv's scope and known limits](scope.md), section 8). With the node's agent
stopped instead, at 8 pools, the hosts' writes to the 6 pools that moved
failed 18 times in all while the agent was stopped, as the fence answers the
old primary's path with errors (section 8 of the same document), and none
failed once it ran again. Building 64 pools at
once took 314 s with no failover; the disk nodes' reports of their sides
grew 14 to 18 s old meanwhile over two runs, which starts no leg repair at
the default thresholds (section 8 of the same document).

The limits dnv enforces, each against the largest size a test has run. The
Go tests run the control plane against fake agents, test programs that answer
an agent's calls with no node behind them, and the agents against a fake
machine, test code in place of a node's kernel and tools; they reach every
limit, each at its value with the next one refused, or for a controller
node's budget cut to it. The suites run on the lab VMs, most of them with
real agents on real nodes and the worker's and the gateway's suites with fake
agents, and stay far below most limits. A size only the Go tests reach is
proven against fakes alone.

| Limit | Value | Largest run in the Go tests | Largest run on the lab VMs |
|---|---|---|---|
| Disk nodes per cluster | 1024 | 1024 | 180 dn agents on four guests, at 32 slices |
| Controller nodes per cluster | 1024 | 1024 | 3 |
| Storage pools per cluster | 4096 | 4096 | 64 at once on real nodes, in the e2e suite's many-pools case; 8 at once as CSI volumes; 10 at once in a suite on fake agents |
| Slices per pool | 32 | 32, committed on a real etcd | 32; 1 under the CSI driver |
| Cntlrs per pool | 4 | 4 | 3 |
| Cntlrs per controller node | 256 | 256, and a real cn agent on a fake machine converged 257 | 256 on one guest of 8 CPUs and 3.3 GiB, each cntlr a one-slice pool with no RAID1, one IO queue a leg, no host, no IO; 47 with both hosts writing, in the e2e suite's many-pools case |
| Sides per disk node | 1024 | 1024, and a real dn agent on a fake machine converged 1025 | 1024 on one guest of 2 CPUs and 7.2 GiB, no host, no IO; 28, every extent of its disk, with both hosts writing, in the e2e suite's many-pools case |
| Groups per slice, meta and data each | 255 | 255 data groups, grown without redundancy; a full meta list, and a full list with RAID1, only written to etcd directly | 9 data groups with RAID1, in a CSI volume filled whole; about 2 meta groups |
| Thin devices per pool | 1024 | 1024 | a few |
| Subsystems per pool; namespaces per subsystem; hosts per subsystem | 4; 4; 8 | each at its value | 1; 2; 4 |
| Clones; transfers; migrations per pool | 64; 4; 4 | each at its value | 1; 1; 2 |
| Retired legs per group, the replaced legs a group keeps with their data after its repairs | 2 | 2 | retired legs made, taken back into a group by a repair, and deleted |
| Bitmap chunks, the pieces of the list of blocks a copy must take: per source slice of a clone, which an operator appends, each holding up to 1 MiB; per migration, which the worker cuts from its own read, each 4 MiB but the last | 16; 16 | 16; 16 | 2; 2 on real agents; 13, written to etcd directly, and 1 in a suite on fake agents |
| Thin ids per pool, one taken from a counter by every thin device the pool creates, snapshots included | 16,777,215 | the counter set to the limit by hand, one create at it, the next refused | no test reaches it |
| Extent size | 64 MiB to 1 TiB, a whole number of 4 KiB | pools created on 5 GiB extents; 1 TiB checked by validation alone | 64 MiB and 1 GiB; no run at 1 TiB |
| Data block of a pool | 64 KiB to 4 MiB, a power of two | pools created on 4 MiB blocks; the next power of two refused | the default 1 MiB; 64 KiB in a suite on fake agents |
| Controller node budget | one extent to 64 TiB, set once per node: a larger `--capacity` is cut to 64 TiB, one smaller than an extent is refused, and one unset or under 1 KiB is 64 TiB | the bounds, through the create with a fake agent | 64 TiB, the default, on real nodes in the Kubernetes suite and on fake agents in the gateway suite |

A lab tool, `integtest/size_step.sh`, stepped one real disk node guest of 2
CPUs and 7.2 GiB to 1024 sides and one real controller node guest of 8 CPUs
and 3.3 GiB to 256 cntlrs, each cntlr a one-slice pool with no RAID1. Each
leg's connection had one IO queue, no host connected and no IO ran, so the
numbers hold for those conditions only. Every call succeeded and every
check round reported the node clean. A fresh read of a node came within the
tool's floor of about 0.2 s at every step, and one cntlr's at 256 cntlrs in
0.6 s. A cntlr's first build took about 0.4 s at 16 cntlrs and about 1 s
past 128, the slowest 1.7 s. The controller node used about 4 MiB of memory
a cntlr, and the disk node about 0.4 MiB a side. Emptying a disk node of
1024 sides in one desired state took 56 s. That run read no object's
rows. The tool reads the rows of a sample of each step's new objects and
stops at a failed one; a short run to 128 sides and 64 cntlrs read every
sample clean.

To measure dnv fairly, write the whole volume first, since a thin device
returns zeros for a block it has not written without reading any disk; run
4 KiB random IO over a range of queue depths; and compare the result with the
raw disk and with a plain nvmet export of the same disk, so that the cost of
dnv's layers is told apart from the disk's and the network's.

## Get dnv

The release is the archive `dnv-v0.1.0-linux-amd64.tar.gz` and the file
`SHA256SUMS` on the GitHub release of the tag v0.1.0:
https://github.com/distributed-nvme/distributed-nvme/releases/tag/v0.1.0.
Download both into one directory, then check and unpack:

```
sha256sum -c SHA256SUMS
tar xzf dnv-v0.1.0-linux-amd64.tar.gz
```

The archive unpacks to one folder, `dnv-v0.1.0-linux-amd64`, with the six
binaries, the `LICENSE` file and the `deploy/` directory, the CSI driver's
image recipe and manifests. 0.1 is older than these documents. Its
`deploy/` lacks the folders that the repository now holds for setting up
a machine: `udev/`, the udev rules; `modules-load.d/`, the kernel modules to
load at boot; `systemd/`, the units of the programs; `dnv/`, the examples
of their settings; and `nftables/`, the firewall example. It also lacks
`controller-role.yaml`, `controller-rolebinding.yaml` and
`networkpolicy.yaml`. Its programs also
lack commands and flags the tutorials use, such as `dnv-agent check` and the
cn agent's `--ignore-host-disconnection`. So follow the tutorials with a
build of the repository, below, or with a release after 0.1.

Or clone the repository and build it with Go 1.26.5 or newer, which puts the
six binaries in `bin/`:

```
git clone https://github.com/distributed-nvme/distributed-nvme.git
cd distributed-nvme
make build
```

The version is stamped into every binary and `go version -m <binary>` prints
it, v0.1.0 for a release build. Go stamps it from the nearest folder, at or
above the build's, whose `.git` is a directory: a build in a git worktree,
whose `.git` is a file, carries the commit of the clone whose folder holds
the worktree, when one does, and else prints `(devel)`. A build of later code also prints it with
`--version`, such as `dnv-agent --version`, and each agent, gateway,
worker, cdc and CSI plugin logs it when it starts; the programs of v0.1.0
have no such flag.

etcd is not in the archive. Fetch etcd v3.6.14 for linux-amd64 from
https://github.com/etcd-io/etcd/releases/tag/v3.6.14, the file
`etcd-v3.6.14-linux-amd64.tar.gz`, and put its `etcd` binary beside the dnv
binaries, where the tutorial expects it.

Then read [dnv on one machine](tutorial_all_in_one.md), a whole dnv system on
one Linux machine with a host connected to a volume, and [dnv under Kubernetes
on one machine](tutorial_csi.md), which adds a one-node Kubernetes and gives a
pod, the Kubernetes unit of one or more containers, a dnv volume through the
CSI driver. dnv is released under the MIT license: [LICENSE](../../LICENSE).
