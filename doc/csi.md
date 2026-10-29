# csi.md — dnv-csi, the CSI driver, and its suite

This document owns the Kubernetes side of dnv: `dnv-csi`, the driver that
connects dnv to Kubernetes through the Container Storage Interface (CSI),
the plugin API Kubernetes uses to create, attach and mount volumes, in
`cmd/dnv-csi/` and `csi/`, and the suite that proves it,
`integtest/csi_test.sh`. It leans on
`architecture.md` for the object model and the API contract, on
`gateway.md` for each call the driver makes, and on `cdc.md` for what a host
must do to follow a pool's cntlrs. Its Kubernetes words are the glossary's:
CSI driver, controller plugin, node plugin, storage class, persistent volume
claim, attach, stage, publish, sidecar, capacity object, topology segment,
dnv name, follower and k3s. Here "the driver" is dnv-csi, the machine a
suite runs on is the suite's machine, and a stage is the node plugin's
call, not a step of a suite case, as the glossary's entries for driver and
stage say.

## The volume model

Every Kubernetes volume is a storage pool of its own with one thin device,
one subsystem and one namespace. The pool, the thin device and the
subsystem's NQN carry the volume's dnv name, the namespace uuid derives
from it, and the volume id Kubernetes stores names the cluster and that
name. The dnv name is the name Kubernetes gives the volume when dnv takes
it as a name (`architecture.md`, Common validation) free of the id's
separator; any other name, longer than dnv's names or with a character
outside their set, which the spec lets a caller send, becomes its leading
characters, each made acceptable, and a hash of the whole name, the same
for the same name, so a repeated create finds its objects. So every object
of a volume is found by one name, the per-pool limits on subsystems,
namespaces and hosts never bind, and the tenant promise of dnv holds per
volume (`architecture.md`, System overview).

A storage class names one dnv cluster, the redundancy of every group and
the cntlr count of every pool. A pool has one slice. Its first data group
holds one eighth of the volume size, rounded up to whole extents, and
auto-grow adds groups of that size until the slice covers the volume
(`dnv-worker.md`, Thin-pool auto-grow). So a volume reserves one eighth of
its size when it is created and the cluster can be over-committed; when no
free extent is left, writes stall and then fail, as for any full pool
(`architecture.md`, Known limits).

Attach sets the subsystem's allowed hosts to the node's host NQN alone;
detach empties the list, which admits nobody and hides the subsystem from
discovery (`cdc.md` DS4). The node plugin follows the discovery log as a
host must (`architecture.md`, Host view): it holds a persistent discovery
connection to every cdc address, connects every listed cntlr of the
subsystems it has staged, drops a path the cdc withdraws, and touches no
connection but those CSI15 names its own. It never talks to the gateway:
the controller plugin hands it the subsystem NQN, the namespace uuid and the
cdc addresses at attach time, and the plugin knows the cdc addresses from
its own configuration as well, so that a restart knows them.

## The driver

dnv-csi is one program with one subcommand per plugin; the controller
subcommand serves the identity service and the controller plugin, and the
node subcommand the identity service and the node plugin. Each is a thin
main over the library package `csi`: flags, a config file and the
environment are one thing, bound under the prefix `DNV_CSI`, as for every
daemon (`architecture.md`, Components: invocation reference). The controller
plugin takes the socket to serve (`--csi-address`), the gateway's address
(`--gateway-address`), the cdc addresses it hands to the node plugin at
attach (`--cdc-addresses`) and the bound on one gateway call
(`--gateway-timeout`); a cdc address is an IP literal and a port, an IPv6
address in brackets, since the node connects to it as the kernel's
discovery address. The identity service
reports the driver's name and version and advertises the controller service
and the accessibility constraints (CSI13) from both plugins, since a
capability describes the driver, not the process that answers: the
conformance test reads it off the node plugin's socket before it drives a
volume through the controller plugin, and the sidecars read it off the
controller plugin's.

CSI7. **The gateway is the controller plugin's only peer.** It holds one
plaintext connection to the gateway under the shared client chain and the
shared message limit (`grpc.md`, Wiring), and every gateway call is bounded
by the configured timeout, which is kept below what the provisioner sidecar
gives a whole create, since a create makes several calls. It never touches
etcd and never talks to an agent or a node. A gateway code passes through
to the CSI caller behind a message that names the call; a connection
failure is UNAVAILABLE, which every sidecar retries.

CSI8. **The CSI services take the driver's own interceptor.** The gRPC
rules of the tree bind the dnv services only (`grpc.md`, Placement), and the
shared server interceptor logs every message, so the CSI services get a
chain of their own, under the shared message limit as every server of the
tree: it mints a trace id for each call, since the kubelet
and the sidecars send none, logs the method on the way in and the method,
the code and the duration on the way out, and never logs a request or a
reply, so nothing the kubelet may send as a secret reaches a log. The
liveness probe's successes are left out of those records. The minted id
travels to the gateway on the shared client chain, so the gateway's records
of the calls a CSI call makes carry it.

CSI9. **Create makes four objects in order, each step repeatable.** The
pool, named as the volume: one slice, the class's redundancy and cntlr
count, a first data group of one eighth of the volume rounded up to whole
extents and at least one, dnv's default auto-grow mark whatever the cluster
stores, since a cluster's mark can switch auto-grow off and the volume model
needs it on, and the cluster's defaults for the rest. The thin device, named
as the volume, sized to the request rounded up to the pool's unit, the
stripe size: the required bytes rounded up, or the limit rounded down when
only a limit is given, or the driver's default when neither is, and
OUT_OF_RANGE when no multiple of the unit fits the range. The subsystem with
the volume's NQN and no host. The namespace at index one with the volume's
uuid. A step that finds its object there reads it back and goes on when it
matches; a pool of another redundancy is ALREADY_EXISTS, the spec's word
for an incompatible volume of the same name, and so are a thin device of
another size and a namespace that is not the volume's; a pool's cntlr count
is not compared, since an operator may add or delete cntlrs; a pool still
being deleted under the name is ABORTED, so the caller retries once the
drain has removed it. The create
returns once the four records exist; the node plugin waits for the device. A
delete removes the objects in the reverse order, each step repeatable: the
namespace, the subsystem and the thin device, then the pool, last since its
delete is refused while a user-created object remains (`architecture.md`,
Storage pools); the worker drains the rest (`dnv-worker.md`, The sp drain).
An object already gone is done, a write the gateway refuses because the pool
is already deleting is done, and so is an id that names no volume of this
driver. A create from a snapshot or another volume is refused.

## Attach and detach

CSI10. **Attach sets the host list to the node alone.** A node id is the
node's host NQN, which the node plugin reports and the controller plugin
checks the form of: an id of another form names no node of this driver,
NOT_FOUND. Attach reads the volume's subsystem: the node already alone in
the allowed hosts is done; an empty list is set to the node; another node in
it is FAILED_PRECONDITION, the spec's word for a volume published elsewhere
under single-node access, naming that node. The reply's publish context
carries the subsystem NQN, the namespace uuid as the record holds it, and
the cdc addresses joined by commas; the volume context of a create carries
the NQN and the uuid as well. Detach takes the node out of the list, which
empties it, or empties it outright when no node is named; a node not in the
list, a subsystem or a pool that is gone, and an id of another form are all
done.

## Capacity

CSI11. **Capacity is arithmetic over the cluster capacity call.** A disk
node counts when it is enabled, healthy, below its side ceiling and holds
at least the free extents of the cluster's smallest bin, below which no
scan finds it (`architecture.md`, DN bins); a controller node counts when
it is enabled, healthy, below its cntlr ceiling and has an extent free. A
pool puts every leg on a disk node of its own and every cntlr on a
controller node of its own, charged with the pool's footprint
(`architecture.md`, Per-operation allocation), so a class whose pool of a
meta group and a data group needs more disk nodes, or whose cntlr count
needs more controller nodes, than count has nothing available and a largest
volume of zero. Otherwise the available bytes are the smaller of two sides.
The disk side is the free extents of the disk nodes that count, times the
extent size, divided by the legs a group has under the class's redundancy.
The controller side is the free extents of the controller nodes that count,
times the extent size, divided by the class's cntlr count. The largest
volume is the one whose first data group, one eighth of it, fits whichever
disk nodes the meta group takes, since the gateway places the meta group
first and at random: of the twice-legs fullest disk nodes, legs stay for
the data group, so the bound is the twice-legs-th largest free count. It
also fits the controller node that would carry its last cntlr, the
cntlr-count-th largest free count less the meta group's extent; it is never
above the available bytes, and it is always reported, zero included, since
an absent one means no bound per volume. Locations are not counted, so a
cluster whose disk nodes share few locations can answer more than a create
builds. Both are estimates, as the capacity call's records are
(`architecture.md`, Clusters), and the available bytes can be
over-promised, since a volume reserves one eighth of itself (The volume
model). A create does not check them: it builds any claim whose first data
group the cluster has room for. A cluster that does not exist
has nothing free: both answers are zero, not an error, since Kubernetes
asks the capacity of every class on a timer and a class may name a cluster
still to be created; a create in it is the class's error (CSI12).

## Parameters and capabilities

CSI12. **Three parameters, single-node access, two filesystems.** A storage
class carries the cluster name, with the gateway's default when absent, the
redundancy, RAID1 or none, with RAID1 when absent, and the cntlr count, with
the gateway's default when absent; a cluster that does not exist is
INVALID_ARGUMENT on a create, the class's error; any other key is
INVALID_ARGUMENT, so a typo never passes silently, except the keys
Kubernetes reserves for itself, which are ignored, since the provisioner
strips them from a create but hands them over whole with a capacity query. A
volume capability is a single-node access mode, in every form the spec has
for one node, and a block volume or a mount volume of ext4 or xfs, ext4 when
no filesystem is named; a multi-node mode or another filesystem is
INVALID_ARGUMENT on a create and leaves a validation unconfirmed with the
reason. The controller plugin advertises create and delete, publish and
unpublish, and get capacity, and nothing else; the volume list, snapshots,
expansion and modification are not served.

## The node plugin

The node subcommand runs on every Kubernetes node that may mount a volume.
It takes the socket to serve (`--csi-address`), the cdc addresses, the same
list the controller plugin takes (`--cdc-addresses`), the two identity files
(`--hostnqn-file`, `--hostid-file`), the bound on a stage's wait for the
device (`--device-wait`) and the follower's period (`--sync-period`).
Everything it does to the node goes through the OS client, the one path to
the operating system (`osclient.md`): nvme-cli for what the kernel takes as
a command, the mount tools, blkid, the filesystem makers, the sysfs and
mount table reads. Only the kernel's event socket is the plugin's own, since
the OS client has no socket.

CSI13. **The node id is the host NQN, and every node is in one topology
segment.** The plugin reads the node's identity files at start and refuses
to start when either is missing, empty or not a host NQN, naming the file
and the commands that generate the pair. Every connect carries both the host
NQN and the host id, since the kernel keeps a one-to-one map between them.
The files are the operator's; the plugin never writes them. With its id the
plugin reports one topology segment, the same on every node, since a dnv
volume is reachable from every node that runs the plugin: the segment says
nothing of its own, and it exists because the provisioner sidecar deletes a
capacity object whose node selector is empty as soon as it has made it, so
a driver without a segment has no capacity objects at all (CSI19). A create
ignores the topology requirements it is sent, which can only name that
segment, and names no accessible topology for the volume, which Kubernetes
reads as every node.

CSI14. **The plugin's own connect, with the kernel's defaults.** A node's
path to a volume is made by nvme-cli with the node's identity and no
timeout settings of its own, so the kernel's defaults hold; the agent's
connect, which hard-codes dnv's settings for a cntlr's path to a leg, is
not reused, and neither are its sysfs readers. A stage connects to every
cntlr the cdcs list for the volume's subsystem, retrying a cntlr whose port
does not listen yet inside the stage's wait, since the cdc lists a
subsystem before its cntlrs have built it, and then waits for the volume's
device link by uuid under the disk-by-id directory, which appears once a
path is live and optimized. A volume whose link does not come within the
wait is UNAVAILABLE, and the kubelet calls again.

CSI15. **The follower.** The plugin keeps the set of subsystems it has
staged and the cdcs it follows, the configured ones and those the attaches
named, holds one persistent discovery connection to each cdc, and on every
trigger reads the discovery log through each of them, then for every staged
subsystem connects a listed cntlr the node has no path to and drops, by
controller, a path to a cntlr no cdc lists any more, which is the node's
part of the failover contract (`architecture.md`, Host view). The triggers
are a stage, the kernel's device events, read off its event socket, among
them the discovery-log-change notification, and a timer as the safety net. A
stage, an unstage, a pass and the stop's drop run one at a time, so an
unstage that empties the set never forgets the cdcs under a stage of another
volume. A cdc that cannot be reached, or whose log cannot be read, holds
every drop off and leaves the connects to the others, since twins exist so
that one can be down; a pass that read no log drops nothing: an answer that
is missing is not an answer that the cntlr is gone. The plugin's discovery
connections are those under the node's host NQN to a cdc it follows, and it
uses and drops no other; a connection another tool made to the same cdc
under the same host NQN cannot be told apart from the plugin's. After a
restart the plugin reads the staged subsystems back from sysfs, those of the
driver's NQN prefix, and follows the configured cdcs from the start, so a
configured cdc that cannot be read holds the drops off after a restart as
before one, whichever shard codes each cdc serves. A discovery connection
under the node's host NQN to another address is followed as well when its
log lists a staged subsystem, since an attach named that cdc before the
configuration changed; it only adds a cdc that must answer, and one whose
log cannot be read at the restart stays another tool's. It touches no path
of a subsystem that is not its own.

CSI16. **Nothing staged, nothing connected.** The last unstage drops the
subsystem's paths and the persistent discovery connections with them, and
so does a stop of the plugin with nothing staged; a stop with volumes staged
leaves every connection in place, since the data path must outlive the
plugin's pod. A stage that fails with a final code, after which the kubelet
counts the volume as not staged and never unstages it, leaves the set at
once, with its paths; a code after which the stage may still be running
keeps it, as does a mount at the staging path, which an earlier stage made.
A drop of the discovery connections forgets the cdcs the attaches named and
keeps the configured ones.

CSI17. **Format only what is blank, mount by binding, repeat for free.** A
stage of a filesystem volume reads the device's filesystem and partition
table straight from the device: a blank device, one in which blkid finds
nothing and reports no error, is formatted with the capability's filesystem,
ext4 when none is named, a device that carries that filesystem is mounted as
it is, and a device that carries another, or a partition table and no
filesystem, is FAILED_PRECONDITION, since formatting it would destroy data;
a block volume is never formatted or mounted at stage. Publish bind-mounts
the staging path at the target, read-only when asked, and for a block volume
makes a file at the target and bind-mounts the device node onto it.
Unpublish unmounts the target and removes it, a directory with a plain
rmdir, never recursively; unstage unmounts the staging path. Every step
reads the mount table first, so a repeat is done, as the spec asks, and a
capability the driver does not serve is FAILED_PRECONDITION at stage and
publish, the spec's word for a capability the volume does not support. The
used-space report reads a mounted filesystem's totals in bytes and inodes
through the OS client, and a block volume's size alone.

## Deployment

The driver ships as one container image and plain manifests in `deploy/`:
one file per Kubernetes object, and the Dockerfile beside them. It promises
Kubernetes 1.34 and newer; the lab proves it on the k3s release the suite
pins, with the sidecar images the manifests name.
The image is the Ubuntu release the lab runs with nvme-cli, the mount
tools, blkid and the two filesystem makers, plus the one binary; the pod's
command names the subcommand. The manifests name the image by the driver's
version and pull it only when it is absent, so a lab imports it into every
node under that name and a user who publishes it keeps the same files.

CSI18. **The node pods run privileged on the host's network.** The kernel
keeps an NVMe/TCP connection in the network namespace of the process that
made it, so a connect from a pod's own namespace would die with the pod.
The kubelet's pod and plugin directories are mounted both ways, so the
mounts the plugin makes reach the host and the pods; the device and sysfs
trees give the plugin the volumes' devices and the controllers' state; the
host's nvme directory, read-only, gives it the node's identity, and is made
empty where it is absent, so that the plugin starts and its own refusal
names the missing files (CSI13). The
registrar sidecar announces the socket to the kubelet, and the liveness
probe sidecar answers the kubelet's health checks for both plugins.

CSI19. **The controller pod runs anywhere and tolerates the control-plane
taint.** It holds the plugin, the provisioner, the attacher and the liveness
probe around one socket. The plugin reads the gateway address and the cdc
addresses from a config map, as environment, and the node plugin reads the
cdc addresses from the same map, so the manifests of a cluster differ from
the examples in that map and in the storage classes alone, which name the
operator's clusters. The driver object asks for attach, no pod information
on mount, the persistent lifecycle and capacity tracking, since the plugin
answers capacity; the provisioner polls it and owns the capacity objects,
for every class of the driver, those with immediate binding included, since
no scheduler needs those but the capacity objects are the one place
Kubernetes shows dnv's free space.

## The suite's dnv stack

The suite deploys the dnv stack itself, as the e2e suite does, with its
own copies of the deployment verbs: etcd, the gateway, the worker and the
cdc on the control-plane guest as the login user, one dn agent per
disk-node guest over a sparse loop device, one cn agent per controller-node
guest, each guest its own location, and a cluster with every node
registered; the hosts carry the mask of the kernel's own autoconnector for
the run. The stack listens on a port block of its own, bound by no other
suite. The suite never builds a pool: the driver does, one per volume, so
a RAID1 pool of one slice takes four disk-node guests, which is the least
the suite accepts, and the failover needs the second controller-node
guest. The storage class of the cases names the suite's cluster, RAID1 and
two cntlrs.

## Log records

The controller plugin logs `csi controller starting` with its endpoint,
gateway address, cdc addresses and version, `csi controller serving` once
it listens, and `csi controller stopping` when it ends; the node plugin
logs `csi node starting` with its endpoint and node id, `csi node serving`
and `csi node stopping` the same way, and, since those are the moments the
failover contract turns on, `csi path connected` and `csi path dropped`
with the subsystem and the cntlr address, `csi discovery connection opened`
and `csi discovery connection dropped` with the cdc address, and
`csi follower sync error` with what failed when a follow could not finish.
Every CSI call but a successful probe logs `csi request` with the method
and `csi reply` with the method, the code, the duration and, on a failure,
the error, under the trace id the interceptor minted (CSI8). The gateway
calls a CSI call makes are the shared client chain's records under the same
id.

## The Kubernetes lab

Three guests of the lab run Kubernetes. The control-plane guest runs the
k3s server beside dnv's control plane; the two hosts run the k3s agents, so
that pods mount volumes on the two real kernel NVMe hosts. k3s is
installed once and stays (CSI1). Each of the three is a distinct machine:
its own hostname, which k3s takes as the node name, its own machine-id, and
on a host its own NVMe identity (CSI2). A host carries enough memory for a
kubelet, its runtime, the node plugin and a pod that runs IO; preflight
holds it to a floor. The control-plane node carries a no-schedule taint,
so test pods run on the hosts (CSI6).

## Integration test plan

### Purpose — what the suite proves

The suite drives a real k3s cluster on three guests of the lab and the dnv
stack on all ten, with the driver deployed from its manifests. It first
proves the Kubernetes floor everything else stands on: the three guests
are distinct machines, k3s is present at the pinned version or gets
installed, an image built on the suite's machine reaches every node
without a registry, and a pod runs from it on each host and is removed with its
namespace. On that floor it proves the driver: a claim becomes a pool that
a pod reads and writes on either host, as a filesystem and as a raw block
device; a failover under IO loses no write; eight volumes live at once; the
upstream conformance test passes; and every delete leaves nothing behind.

### Topology and parameters

Every guest comes from the command line: one `--cp`, the k3s server and
dnv's control plane, exactly two `--host`, the k3s agents, at least two
`--cn` and at least four `--dn`, the dnv stack's nodes. Parameters: one
case to run alone, a cleanup-only run that sweeps every guest and stops,
a slice count the suite accepts and every pool ignores, since a volume's
pool has one slice, and a directory on the suite's machine to keep the logs
in. The pinned k3s version is a constant of the suite.
The suite's work directory on every guest and its shipped helper are its
own, named so that no other suite's is a prefix of them.

### The rules

CSI1. **Kubernetes is installed only when absent and never removed.** A
guest without k3s gets the pinned release installed by the suite, the
server with the lab's unused components switched off and the agents joined
by the server's token, which travels on the helper's stdin and never in a
command line. A guest with another version stops the run naming the
version and the uninstall script. The suite never uninstalls k3s: it keeps
running between runs, and an operator removes it with the k3s uninstall
scripts.

CSI2. **The three guests are distinct machines.** Preflight reads each
guest's hostname and machine-id and requires the three of each to differ.
The hosts' identity files are generated if absent and never overwritten,
as the e2e suite's preflight does (`e2e_integtest.md`, Preflight), and the
two hosts' host NQNs must differ; the fix is named, not applied.

CSI3. **kubectl runs on the control-plane guest only**, as the k3s binary's
subcommand over ssh, with every manifest piped from the suite's machine,
which installs nothing: it needs Go, ssh, scp and docker.

CSI4. **The suite's images reach a node by import.** The suite builds the
driver's image and the smoke case's image on its machine, saves them and
pipes them into the k3s image store of every node, so no pod pulls them and
no registry of the suite's takes part. The sidecar images the manifests
name come from their public registry, which each node pulls from once.

CSI5. **Cleanup removes what the suite started and nothing else.** The test
namespace and the driver's namespace are deleted and waited for, the
driver's cluster-wide objects and the storage class with them; then the
hosts' connections of the driver's and dnv's and the discovery connections
to the suite's cdc go, then the controller nodes' and the disk nodes'
devices, exports and agents, with the controller nodes' nvmet entries of the
two hosts that attaches made, then the control plane's processes, the
imported images and the work directories, holder first. Cleanup runs at the
start of every run and, on success only, at its end; a failing run leaves
everything in place and dumps diagnostics. k3s, the hostnames and the
identity files stay.

CSI6. **Test pods run on the hosts.** The control-plane node carries a
no-schedule taint the suite ensures on every run, and every pod names its
host.

### The cases

Every case runs on the suite's dnv stack with the driver deployed from the
manifests, the config map and the storage class of the suite in place of
the examples; the IO pods run the driver's own image, since it carries the
tools they need and is already on every node. Each case that creates
volumes ends with the check that nothing is left: no pool in the cluster,
no dm device, array or export on a node, no connection of the driver's on
a host.

**k8s.** The suite writes a program that prints one line and sleeps, builds
it static, wraps it in an image from scratch and imports it into every
node. It applies one pod per host, each pinned to its host by node selector
and never pulling. Each pod is Running on its host, each log is the one
line, and the pods go.

**volume.** The class's capacity object is published with a capacity above
zero; a filesystem claim becomes a pool of one slice and two cntlrs; a pod
on host0 writes a pattern and its digest; the same claim under a pod on
host1 checks the digest; a raw block claim is written and read back through
the device by a pod on host0; then every delete, and nothing left.

**failover.** A pod on host0 writes a pattern, then writes in a loop with
fsync, counting failures and the longest write. The suite reads which
controller node holds the volume's primary and stops that cn agent, as the
e2e suite's cutoff case does, so that every kernel object it built goes on
serving until the fence; the primary moves; the loop runs on against the
new primary and is stopped; it has failed nothing, and the longest write is
reported. The pod goes, and a pod on host1 reads the pattern back through
the new primary, since host0's page cache holds it. The agent is given
back, then every delete, and nothing left.

**many.** Eight claims and eight pods at once, four per host, each writing
a pattern and checking it; the cluster holds eight pools; then every
delete, and nothing left.

**sanity.** csi-sanity, the upstream conformance test, built on the suite's
machine from its pinned module in a module of its own, runs on host0 against
both plugins started there as plain processes with the lab's gateway and
cdc, after the driver's pods are taken down, since two node plugins on one
host would follow each other's subsystems. The tests of capabilities the
driver does not advertise skip on their own; a failure names its test. Then
the plugins stop, and nothing is left.

### Preflight

The suite's machine's checks run first: its tools and a usable docker,
the build of the binaries, the tree's two etcd constants read from the
worker's constants printer, and the pinned etcd release. Then the helper is
shipped and the start cleanup runs, before any guest check, so that a port
check never judges what a failed run left in place. Then the guest checks:
passwordless sudo on every guest; on the three Kubernetes guests the
hostnames and machine-ids distinct (CSI2), native multipath and the memory
floor on the hosts, and the hosts' identities present and different; on the
node guests the agents' tools and modules and this suite's ports free. Then
k3s: present at the pin or installed (CSI1), every node Ready at the pin,
the taint (CSI6), and the cluster's own name service available. Then the
dnv stack and the driver.

### Diagnostics on failure

A failing run prints the nodes, every pod, the pods and events of the
suite's namespace, and from each guest its facts, its k3s state, its
memory and the tail of its k3s journal. A run asked to keep its logs copies
the k3s journal of each guest to the suite's machine at its end.
