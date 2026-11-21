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
lease, network policy, dnv name, follower and k3s. Here "the driver" is
dnv-csi, the machine a suite runs on is the suite's machine, and a stage is
the node plugin's call, not a step of a suite case, as the glossary's
entries for driver and stage say.

## The volume model

Every Kubernetes volume is a storage pool of its own with one thin device,
one subsystem and one namespace. The pool, the thin device and the
subsystem's NQN carry the volume's dnv name, the namespace uuid derives
from it, and the volume id Kubernetes stores names the cluster and that
name. The dnv name is the name Kubernetes gives the volume when dnv takes
it as a name (`architecture.md`, Common validation) free of the id's
separator and of "..", which the gateway refuses in the subsystem's NQN
(`architecture.md`, Common validation); any other name, longer than dnv's
names, with a character outside their set or holding the separator or
"..", which the spec lets a caller send, becomes its leading characters,
each one outside the set, the separator and the second dot of each ".."
among them made a dash, and a hash of the whole name, the same for the
same name, so a repeated create finds its objects; so every name the spec
allows makes a volume, mapped before any create. So every object of a
volume is found by one name, the per-pool limits on subsystems, namespaces
and hosts never bind, and the tenant promise of dnv holds per volume
(`architecture.md`, System overview). A volume costs the cluster what a
pool costs: its cntlrs count against the controller nodes' cntlr ceiling;
its sides, one per leg of each of its groups and one more for each
migration destination, repair's new leg and retired leg a group holds, up
to `MaxRetiredLegPerGrp` retired legs a group, against the disk nodes' side
ceiling; and the pool itself against the cluster's pool ceiling; so how
many volumes a cluster can hold follows from those ceilings and the node
counts, the free extents each volume reserves, the class's cntlr count, its
redundancy and the groups auto-grow adds to each volume (`architecture.md`,
Cardinality limits).

A storage class names one dnv cluster, the redundancy of every group and
the cntlr count of every pool. A pool has one slice, with a meta group of one
extent (`architecture.md`, Storage pools) and a first data group of one part in
`volumeSteps` of the volume size, rounded up to whole extents. Auto-grow adds a
data group of that size each time the pool's data use passes the mark the
driver sets at every create, `DefaultPoolLowWatermarkPct`, until the slice's
data covers the volume, and a meta group of the meta ladder's next size when
the metadata passes the same mark (`dnv-worker.md`, Thin-pool auto-grow). So a
volume reserves one extent and one part in `volumeSteps` of its size when it is
created and the cluster can be over-committed; when no free extent is left,
writes stall and then fail, as for any full pool (`architecture.md`, Known
limits).

Attach sets the subsystem's allowed hosts to the node's host NQN alone, and
writes the node into an empty list only once the primary cntlr has ended the
nvmet connections of every host off the list, where its agent runs in cut
mode (`cnagent.md` CN33), save a connection that shows only after the cut's
last listing; detach empties the list, which closes the subsystem to new
connections (`architecture.md`, Primary cntlr) and hides it from discovery
(`cdc.md` DS4), and returns once the primary cntlr has ended the old node's
nvmet connections, where its agent runs in cut mode, with the same exception,
or once the subsystem's record is gone (CSI10). The attach's wait, not the
detach's return, keeps the next node out while an old one is still
connected: Kubernetes may attach the next node before a detach has returned.
The node plugin follows the discovery log as a host must (`architecture.md`,
Host view): it holds a persistent discovery connection to every cdc address
of its own configuration, and to no other, connects every listed cntlr of the
subsystems it has staged, drops a path the cdc withdraws, and touches no
connection but those CSI15 names its own. It never talks to the gateway: the
controller plugin hands it the subsystem NQN and the namespace uuid at attach
time (CSI10).

## The driver

dnv-csi is one program with one subcommand per plugin; the controller
subcommand serves the identity service and the controller plugin, and the
node subcommand the identity service and the node plugin. Each is a thin
main over the library package `csi`: flags, a config file and the
environment are one thing, bound under the prefix `DNV_CSI`, as for every
daemon (`architecture.md`, Components: invocation reference). The
controller plugin takes the socket to serve (`--csi-address`), the
gateway's address (`--gateway-address`), the cdc addresses it puts in the
publish context at attach (`--cdc-addresses`; CSI10) and the bound on one
gateway call (`--gateway-timeout`). Both plugins take `--log-level`, one of
the three words of `log.md` R6, `info` by default, and change the log level on
SIGUSR1 and SIGUSR2 while they run (`log.md` R13). In a pod the plugin is
the first process of its container, and the signal is sent to it from the
pod's node, as root, at the pid the container runtime reports for the
container: a `kill` from a `kubectl exec` in a controller copy's container
is refused by the container runtime's AppArmor profile under the copy's
least rights, though the process runs as the same user. Whether a `kubectl
exec` in a node pod, which is privileged, may signal its plugin has not
been checked. A cdc address, in either plugin's list, is an IP literal and a port, an IPv6 address in brackets, since the
node connects to the addresses of its own list as the kernel's discovery
address, and the manifests give both plugins their list from one config
map (CSI19). The identity service reports the driver's name and its build
version (`common.BuildVersion`) and advertises the controller service and the
accessibility constraints (CSI13) from both plugins, since a capability
describes the driver, not the process that answers: the conformance test reads
it off the node plugin's socket before it drives a volume through the
controller plugin, and the sidecars read it off the controller plugin's.

CSI7. **The gateway is the controller plugin's only peer.** It holds one
plaintext connection to the gateway under the shared client chain and the
shared message limit (`grpc.md`, Wiring), and every gateway call is bounded by
the configured timeout, which is kept below what the provisioner sidecar gives
a whole create, since a create makes several calls. A detach's or an attach's
wait for the cut (CSI10) is a series of such calls, and the whole call, every
attempt of its write and every wait of it, has one bound, set at the call's
start: the call's deadline or a bound of the driver's own, whichever comes
first. The attacher sidecar, as the manifests run it, gives every call a
deadline far shorter than the driver's bound, which holds a caller that gives
none, as the conformance test does, and is sized for an attach right after a
create, which waits for the primary's first converge at the pool's revision
and reads and waits again at each move the worker makes to that revision
while it builds the pool. The driver takes no flag for its own bound; the
attacher's deadline is the attacher's own `--timeout`, which the manifests
leave at its default. It never touches etcd and never talks to an agent or a
node: the primary's report reaches it through the gateway. A gateway code
passes through to the CSI caller behind a message that names the call; a
connection failure is UNAVAILABLE, which every sidecar retries.

CSI8. **The CSI services take the driver's own interceptor.** The gRPC
rules of the tree bind the dnv services only (`grpc.md`, Placement), and the
shared server interceptor logs every message, so the CSI services get a
chain of their own, under the shared message limit as every server of the
tree: it mints a trace id for each call, since the kubelet
and the sidecars send none, logs the method on the way in and the method,
the code and the duration on the way out, and never logs a request or a
reply, so nothing the kubelet may send as a secret reaches a log. The calls
Kubernetes repeats on a timer — the liveness probe's, the provisioner's
capacity poll (`GetCapacity`), and the kubelet's volume stats of each
mounted volume (`NodeGetVolumeStats`) with the capability call it makes
before them (`NodeGetCapabilities`) — log both records at Debug while they
succeed and their reply at Info when they fail, as a read is logged
(`log.md` R6, R9), so at the default log level a node plugin with mounted
volumes logs no record per volume per minute. The minted id
travels to the gateway on the shared client chain, so the gateway's records
of the calls a CSI call makes carry it.

CSI9. **Create makes four objects in order, each step repeatable.** The
pool, named as the volume: one slice, the class's redundancy and cntlr
count, a first data group of one part in `volumeSteps` of the volume rounded up
to whole extents and at least one, dnv's default auto-grow mark whatever the
cluster stores, since a cluster's mark can switch auto-grow off and the volume
model needs it on, and the cluster's defaults for the rest. The thin device,
named as the volume and sized in whole units of the pool's stripe size: the
required bytes rounded up, or the limit rounded down when only a limit is
given, or the driver's default when neither is, and OUT_OF_RANGE when no
multiple of the unit fits the range. The driver reads that stripe from the
cluster on every create, ahead of the pool's step, as a one-slice pool that
names no stripe takes its cluster's stripe unnarrowed (`architecture.md`,
Common validation). The subsystem with the volume's NQN and no host. The
namespace at index one with the volume's uuid. A step that finds its object
there reads it back and goes on when it matches; a pool of
another redundancy is ALREADY_EXISTS, the spec's word for an incompatible
volume of the same name, and so are a pool of a slice count other than one or
of a stripe other than the one the create read from the cluster, since the thin
device is sized in that stripe's units, a thin device of another size and a
namespace that is not the volume's; a pool's cntlr count is not compared, since
an operator may add or delete cntlrs; a pool still being deleted under the name
is ABORTED, so the caller retries once the drain has removed it. The create
returns once the four records exist; the node plugin waits for the device. A
delete removes the objects in the reverse order, each step repeatable: the
namespace, the subsystem and the thin device, then the pool, last since its
delete is refused while a user-created object remains (`architecture.md`,
Storage pools); the worker drains the rest (`dnv-worker.md`, The sp drain). An
object already gone is done, and so is an id that names no volume of this
driver. The gateway refuses the namespace's delete both when the pool is
already deleting and while a transfer names the namespace as its origin
(`architecture.md`, Transfers), so a refused namespace delete reads the pool:
one already deleting or gone is done, and otherwise the refusal is passed on,
since the transfer is the operator's to end.
A create from a snapshot or another volume is refused.

## Attach and detach

CSI10. **Attach sets the host list to the node alone.** A node id is the node's
host NQN, which the node plugin reports and the controller plugin checks the
form of: an id of another form names no node of this driver, NOT_FOUND. Attach
reads the volume's subsystem: the node already alone in the allowed hosts is
done; an empty list is set to the node; another node in it is
FAILED_PRECONDITION, the spec's word for a volume published elsewhere under
single-node access, naming that node. The reply's publish context carries the
subsystem NQN, the namespace uuid as the record holds it, and the cdc addresses
of the controller plugin's configuration joined by commas, which the node
plugin does not read (CSI15); the key stays, so that every publish context
Kubernetes holds keeps one form. The volume context of a create carries the NQN
and the uuid as well. Detach takes the node out of the list, which empties it,
or empties it outright when no node is named; a subsystem or a pool that is
gone, an id of another form and a node id that is no host NQN, which no attach
put on a list, are done at once. Each of the two reads the pool's revision,
then the subsystem, and, when the list changes, writes it with that revision as
its token (`gateway.md` GW6): a write the gateway refuses as stale ran over
another change of the pool, another attach or detach among them, so the call
reads and decides again, a few times, and then answers ABORTED for the caller
to retry, but for an attach whose write waits for the cut, which reads and
decides again until its call's bound (below); any other refusal is passed on.
So two attaches of one volume to two nodes never both succeed, and a detach
that runs late finds its node gone and leaves the next attach to another node
alone; a late detach cannot tell an attach of its own node apart from the one
it was asked to undo.

The detach then waits for the cut, the primary cntlr's end of the nvmet
connections of every host off the list (`cnagent.md` CN33), and an attach that
puts its node on the empty list waits for the same cut before its write, at the
revision its attempt read, whose list is empty; an attach that writes nothing
does not wait. The attach's wait is exact: a poll that reads the pool, or a
report, past that revision ends the attempt, which reads and decides again with
the new revision as its token and its target, as a stale refusal of its write
does, since the worker sends the primary the latest revision alone
(`dnv-worker.md` RW3) and a report at the revision read may never come; so the
attach never writes under a token without an OK report at that token's revision
made in the same attempt. Such attempts count against no limit, so the moves
the worker makes to the revision while it builds a pool do not end the attach;
the call's one bound (CSI7) does. Reason for the attach's wait: Kubernetes'
forced detach, by the out-of-service taint, attaches the volume to the next
node as soon as one detach call has failed, while the detach that its timeout
ended has taken the old node off the list and its cut has not held, so only the
attach's own wait keeps the next node from being let in beside a host whose
connection still stands (the Kubernetes suite's detach case shows the order).
The detach's revision is the one its write made, the token's plus one, since a
write bumps the pool's revision once (`architecture.md`, Revision keys and the
sync fan-out); a detach that writes nothing, its node already off the list as
when Kubernetes repeats a detach, waits all the same, for the pool's revision
its first poll reads, which comes after its subsystem read, so that another
detach of the node that landed between the two reads is in it. Each poll reads
the pool, takes its primary from the pool's cntlr list, when the list names
one, and asks the gateway for that cntlr's report (`gateway.md`, Cntlrs and
inspects); the wait ends once a report made at the wait's revision, or for a
detach a later one, reads the volume's subsystem row OK, which on an agent that
cuts means the cut held (`cnagent.md` CN33), a later revision counting for a
detach since the worker sends the primary the latest revision alone
(`dnv-worker.md` RW3). A host whose connect passed nvmet's check before the
list step took it off, and whose connection shows only after the cut's last
listing, leaves that row OK too, so a wait, a detach's or an attach's, ends
with that host still connected, and the primary serves it until its next
converge cuts it. After an attach, that is the converge of the attach's own
write, whose list step lets the attach's node in and whose cut, right after
that step and before its ns-dev loop, ends the host's connection, so the two
hosts are let in together only for that cut's wait; with no later change of the
pool, it is the converge of the worker's re-send, once the primary's next read
names the connection (`cnagent.md`, Known limits). A report older than the
revision, a row that is not OK or is missing, an agent that answers no rows, a
pool with no primary, a failed inspect, its NOT_FOUND for a cntlr replaced
during the wait among them, and a pool read that fails but with NOT_FOUND each
mean another poll, and the polls of one call keep the poll period apart across
its attempts, so a pool that moves often, or a report ahead of the pool's
revision, makes no busy loop. Each poll takes the primary afresh, so the wait
follows a failover, and a standby that becomes the primary cuts before it
serves (`cnagent.md` CN33); standbys are not waited for, since they serve no
IO. A pool or a subsystem that is gone and a pool being deleted end a detach's
wait as done: what is gone takes no attach, and a pool is deleting only once it
holds no subsystem (CSI9); an attach's exact wait reads them as a move instead,
so that its next attempt's own reads answer NOT_FOUND and no write goes out
without an OK report at its token's revision; the cn agents' sweep takes down a
subsystem whose record is gone, its namespaces at the cntlr's next converge
(`cnagent.md` CN21), and the sp drain takes down the rest of a deleting pool
(`dnv-worker.md`, The sp drain), so a host still connected can keep its IO
until then. The call's one bound (CSI7) ends the call wherever it is, in a wait
or in a gateway call of an attempt, with DEADLINE_EXCEEDED, or CANCELED where
the caller ended the call first, as the attacher sidecar does at its own
deadline, naming the volume, the call, the node and the last state the polls
saw, the last report and any gateway call that failed after it, and Kubernetes
repeats the call, which waits again. So a pool whose primary's agent does not
answer holds the detach until the agent answers or a failover makes another
cntlr the primary, and the attach to another node waits as long, so no other
node gets the volume meanwhile. A primary whose agent runs in no cut mode —
started with `--ignore-host-disconnection` while its start check found a cause
(`agent.HostCutCauses`), a mode it keeps until it is started again, even once
the cause is gone (`cnagent.md` CN-CM1, CN-CM2, CN33) — reads the row OK with
no cut, so either wait passes at its first report at the revision, and a host
already connected keeps its controller and its IO (`architecture.md`, Primary
cntlr) until it disconnects or its own node plugin drops the path, which it
does once the cdcs list no cntlr of the subsystem to it (CSI15); a detach
Kubernetes forces while that plugin does not run, or cannot read a cdc it
follows, then lets the old host write on beside the host the volume moves to
(`architecture.md`, Known limits).

## Capacity

CSI11. **Capacity is the gateway's answer.** The plugin asks the gateway's
capacity read for a pool of the class's shape — the legs of its redundancy,
its cntlr count as the class states it, the gateway's default where it
states none, as a create sends it, and one slice — and does no capacity
arithmetic of its own: which nodes count, the limits, the locations and the
largest first data group are the gateway's (`architecture.md`, Clusters,
GetClusterCapacity), so the driver and an operator's `dnvctl cluster
get-capacity` read one answer. The available bytes are the answer's
placeable bytes. The largest volume is the one whose first data group, one
part in `volumeSteps` of it, is the largest first data group the answer
names, never above the available bytes; it is always reported, zero
included, since an absent one means no bound per volume. Both are
estimates, as the gateway's answer is, and the available bytes can be
over-promised, since a volume reserves one extent and one part in
`volumeSteps` of itself (The volume model). A create does not check them:
whether it builds a claim depends on the gateway's placement, which draws
the meta group first and at random and can take a retry. A cluster that does
not exist has nothing free: both answers are zero, not an error, since
Kubernetes asks the capacity of every class on a timer and a class may name
a cluster still to be created; a create in it is the class's error (CSI12).
A reply that carries no space answer, which a gateway older than the plugin
sends, is `UNAVAILABLE`, not zero: the provisioner then leaves the capacity
object as it last wrote it, where a zero would stop every claim of the class
that waits for its pod's node. Any other refusal of the gateway is the
call's.

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

The node subcommand runs on every Kubernetes node that may mount a volume. It
takes the socket to serve (`--csi-address`), the cdc addresses it follows
(`--cdc-addresses`; CSI15), the two identity files
(`--hostnqn-file`, `--hostid-file`), the bound on a stage's wait for the device
(`--device-wait`) and the follower's period (`--sync-period`). Everything it
does to the node goes through the OS client, the one path to the operating
system (`osclient.md`): nvme-cli for what the kernel takes as a command, the
mount tools, blkid, the direct read of a volume's start, the filesystem
makers, the sysfs and mount table reads.
Only the kernel's event socket is the plugin's own, since the OS client has no
socket. The plugin reports nothing to dnv, and the worker's reactions read the
agents' reports and the stored records, never a host (`architecture.md`,
Automatic reactions), so no reaction fails over a primary the worker reaches
but the node cannot, at the cntlr's address, and the node's IO to the volume
queues while a standby's path is left (`osclient.md`, Exported raw helpers and
the probe-IO carve-out) and otherwise waits with no error, since the plugin's
connect never gives up (CSI14); on a pool with an enabled standby of a zero
health epoch the operator ends that by disabling the primary
(`UpdateCntlrEnabled`), which starts a failover with no threshold wait
(`dnv-worker.md` AR5), and enabling it again after the failover, since a
disabled cntlr is never a candidate (`dnv-worker.md` AR3); on a pool with no
such standby a disable starts no failover (`dnv-worker.md` AR5) and takes the
primary out of the discovery records (`architecture.md`, Cntlrs), so the
follower drops the node's last path and the mount does not survive it (CSI14).

CSI13. **The node id is the host NQN, and every node is in one topology
segment.** The plugin reads the node's identity files at start and refuses
to start when either is missing, empty or not a host NQN, naming the file
and the fix: on a node whose connections carry an identity, the one they
carry, which sysfs shows, and on a node with no connection the commands that
generate the pair, since a plugin under a new host NQN hears from the cdcs no
path of the volumes attached under the old one and drops their paths as
after a detach. It then reads the node's kernel as sysfs shows it and
refuses to start, with one error that names each fact it read wrong and its
fix, when native NVMe multipath is off or not built, since
without it the loss of one path takes the device the mount uses, or when the
NVMe over Fabrics device is missing, through which every connect goes and
which nothing loads on demand; a read of either that fails refuses as well.
It does not read the NVMe/TCP module, which the kernel loads at the first
connect, as the agents' node check counts it (`dnagent.md` CM5), and it loads
nothing: from the pod a module the kernel could load cannot be told from one
it lacks, and a module named at boot is the one fix that holds after a reboot
(`operations.md`, A Kubernetes node plugin that does not start). Every connect
carries both the host NQN and the host id, since the kernel keeps a one-to-one
map between them.
The files are the operator's; the plugin never writes them. With its id the
plugin reports one topology segment, the same on every node, since a dnv
volume is reachable from every node that runs the plugin: the segment says
nothing of its own, and it exists because the provisioner sidecar deletes a
capacity object whose node selector is empty as soon as it has made it, so
a driver without a segment has no capacity objects at all (CSI19). A create
ignores the topology requirements it is sent, which can only name that
segment, and names no accessible topology for the volume, which Kubernetes
reads as every node.

CSI14. **The plugin's own connect, which never gives up.** A node's path to a
volume is made by nvme-cli with the node's identity and a reconnect that never
gives up, as the agents' paths have (`dnagent.md` SH20), and the kernel's
defaults for the rest; the agent's connect, which hard-codes dnv's settings for
a cntlr's path to a leg, is not reused, and neither are its sysfs readers. So a
mount survives an outage of every path, however long, since IO waits and the
kernel reconnects onto the same device, save where a path's reconnect reaches
its cntlr's port while the port listens for another subsystem and before the
volume's is built again, as on a controller node that comes back holding other
volumes' cntlrs, which refuses it with DNR and ends that path's reconnects for
good (`dnagent_integtest.md`, Assumptions and preflight checks; `cnagent.md`
CN2), or where a subsystem whose cntlid slot has run out refuses the reconnect
with DNR the same way (`architecture.md`, cntlid slots), or where a detach took
the node off the subsystem's list and the cut ended its connection (CSI10),
whose reconnect nvmet refuses with DNR, as for any host off the list. A mount
does not survive the loss of its last path that way: the kernel deletes the
controller, and a later connect to the subsystem makes a new namespace device,
which the mount does not follow. The cost of a reconnect without end is that IO
to a volume none of whose cntlrs comes back waits for ever, with no error; a
pod blocked in it cannot be killed, and the operator ends the wait with `nvme
disconnect -n` of the volume's subsystem on the node (`architecture.md`, Known
limits). The follower keeps a withdrawn path until a listed cntlr's path shows
the volume's namespace, or until the cdcs list no cntlr for the subsystem
(CSI15), so on a pool of one cntlr a cntlr replacement, whose new cntlr the cdc
lists before its agent has exported the subsystem, keeps the old path until the
new one has joined the device, as long as the old path's reconnects are not
refused before then; the new one cannot join while the old path holds the
CNTLID the replacement's controller node gives the connect, and the old path
then stays, its IO waiting, until a reconnect of it is refused with DNR or the
operator disconnects it (`architecture.md`, Known limits). Each path holds at
most the cn agent's IO queue cap of IO queues, one per CPU up to the cap, plus
its admin queue, when the cap is above zero (`dnagent.md` SH32): the cap binds
this connect too. Each connect and each of the two discovery reads of CSI15
runs under the agents' command bound (`architecture.md`, Common validation),
since the follower runs them one at a time and one that hangs would hold up
every stage of the node. The bound ends a hang of nvme-cli and a wait the
kernel lets a signal end, as the TCP connect and the first message of a
connection, and no wait the kernel holds whatever the signal: the Connect
command of a path or of a discovery connection to a cntlr or a cdc that took
the TCP connection and does not answer, which lasts up to the kernel's admin
timeout, and the discovery log read, a command on the persistent discovery
connection that a cdc which keeps that connection and does not answer holds
until the kernel's error recovery of the connection ends it: once the
connection's keep-alive fails, or, while the cdc still answers the keep-alives,
once the read's own admin timeout passes, the distribution kernel the lab runs
starting that recovery half a keep-alive period late, so the wait comes to
about a minute or a little more (CSI15). A disconnect has no bound, the stop's
included: the kernel's delete runs to its end whatever the signal, and a kill
would only make a delete that was done look failed. A connect that fails, or
that the bound ends, counts as made when sysfs then shows a controller of the
subsystem at the cntlr's address under the node's host NQN that is not being
deleted or dead, one for which the kernel would refuse a second connect,
whatever nvme-cli printed; the connect reads none of nvme-cli's words. The
plugin reports no volume health, so nothing in the driver or in Kubernetes acts
on a dead mount, and the operator deletes the pod. A stage connects to every
cntlr the cdcs list for the volume's subsystem and repeats the connects, since
the cdc lists a subsystem before its cntlrs have built it, until a pass ends
with no error or the node holds a path to the subsystem, a standby's included,
and a stage whose wait runs out first is UNAVAILABLE; a cntlr still refusing
when the connects end is handed to the follower's loop at once, whose retry
timer tries it again a second later and then at delays that double up to the
follower's period (CSI15), so the wait the stage has left covers those retries
while they come within it, as the first ones do at the defaults; the stage then
waits for the volume's device link by uuid under the disk-by-id directory,
which appears once a path is live and optimized. A volume whose link does not
come within the wait is UNAVAILABLE, and the kubelet calls again.

CSI15. **The follower.** The plugin keeps the set of subsystems it has staged
and follows the cdcs of its own configuration, read once at its start, and no
other; it holds one persistent discovery connection to each, and on every
trigger reads the discovery log through each of them, then for every staged
subsystem connects a listed cntlr the node has no path to and drops, by
controller, a path to a cntlr no cdc lists any more, which is the node's part
of the failover contract (`architecture.md`, Host view). It drops such a path
only once the node holds a path to a listed cntlr under which the kernel shows
the volume's namespace, a path connected in the same pass counting from the
next one and a path being dropped not counting, or when the cdcs list no cntlr
for the subsystem, which a detach leaves (CSI10): a cntlr that replaced the
withdrawn one but still refuses the connect, or whose path has not yet joined
the multipath device, does not cost the volume its last path.

The triggers are a stage; one pass when the loop starts, so a plugin that
restarted acts at once on a log that changed while it was away; every
change event of an nvme controller, read off the kernel's event socket,
among them the discovery-log-change notification and the notice of a
controller's connect or reconnect, but not a controller's add or remove
event, which a connect that fails makes, so a refused connect does not
start the next pass at once; a retry timer; and a timer of the follower's
period as the safety net. The retry timer runs one more pass after a pass
that failed, a cdc it could not reach or read included, or that held a
drop back only because no listed path shows the namespace yet, for which
no event comes: a path's namespace device sends none. Its delay starts at
a second, doubles after each such pass up to the period, and starts again
from a second after a pass that a stage or an event began, since news can
end what held the passes back, and after a pass that leaves nothing to
retry. The event socket survives a receive overflow, which starts a pass,
since events were lost, and any other failure, which starts a pass as well
and after which the socket is opened again after a pause that doubles up
to the period, its opening starting one more pass for the events lost
meanwhile; each receive returns within a second, so the loop sees its
stop, and a failure of the socket logs a sync error.

A stage, a pass, an unstage's forget and drop of the discovery connections, and
the stop's drop run one at a time, so an unstage that empties the set never
drops the discovery connections under a stage of another volume, whose pass
would read no log and hear no change notice until a later pass opened them
again. The connects, the discovery connects and log reads, and the last
unstage's drop of the discovery connections run inside that order, so a wait
the kernel holds whatever the signal (CSI14), a Connect command or a log read
to a cntlr or a cdc that keeps its TCP connection and does not answer, or the
delete of a connection to one, holds every stage and every pass of the node
for about a minute or a little more, and a cdc twin whose machine stops
without closing its connections costs every node one such wait besides holding
its drops (`architecture.md`, Known limits). The path drops run outside that
order, tracked by controller, at most a fixed number at once, since an
`nvme disconnect` of a target that vanished can wait out the kernel's admin
timeout: no other volume's stage waits for them, a pass does not start a
second drop of a controller being dropped, an unstage waits for the drops of
its own volume and then disconnects the subsystem outside the order too, a
disconnect neither tracked by controller nor counted in that number, and a
stage of a volume whose unstage still runs waits for it.

A cdc that cannot be reached, or whose log cannot be read, holds every drop off
and leaves the connects to the others, since twins exist so that one can be
down; a pass that read no log drops nothing: an answer that is missing is not
an answer that the cntlr is gone. A log whose output is empty, or is not
nvme-cli's JSON with its list of records, cannot be read: every dnv cdc lists
itself first in every log (`cdc.md` DS12), so a real answer is never empty. The
plugin's discovery connections are those under the node's host NQN to a cdc of
its configuration, and it uses and drops no other; a connection another tool
made to the same cdc under the same host NQN cannot be told apart from the
plugin's. After a restart the plugin reads the staged subsystems back from
sysfs, those of the driver's NQN prefix, and follows the configured cdcs from
the start, so a configured cdc that cannot be read holds the drops off after a
restart as before one, whichever shard codes each cdc serves. A discovery
connection under the node's host NQN to an address the configuration does not
name, as one the plugin opened before its configuration changed, is another
tool's: the plugin neither reads nor drops it. The list is the node's alone and
is read once: a change of it takes a restart of the plugin, since the
environment of a running pod does not follow its config map, and a list that
omits a cdc serving staged volumes drops their paths at the first pass in which
the listed cdcs answer, since they list no cntlr for those subsystems, which
reads as a detach; so the list names a cdc of every range that serves the
cluster's volumes (`architecture.md`, Known limits; `operations.md`, Change the
cdc addresses of the Kubernetes driver). The plugin touches no path of a
subsystem that is not its own.

CSI16. **Nothing staged, nothing connected.** The last unstage drops the
subsystem's paths and the persistent discovery connections with them, each
discovery connection even when the drop of another fails, and so does a stop
of the plugin with nothing staged; a stop with volumes staged
leaves every connection in place, since the data path must outlive the
plugin's pod. A stage that fails with a final code, after which the kubelet
counts the volume as not staged and never unstages it, leaves the set at once,
with its paths, and with the discovery connections when nothing else is staged,
as the last unstage does; a code after which the stage may still be running
keeps it, as does a mount at the staging path, which an earlier stage made, and
a mount table that cannot be read, since a missing answer is not an answer that
nothing is mounted.

CSI17. **Format only what is blank, mount by binding, repeat for free.** A
stage of a filesystem volume reads the device's filesystem and partition
table straight from the device: a device that carries the capability's
filesystem is mounted as it is, and a device that carries another, or a
partition table and no filesystem, is FAILED_PRECONDITION, since formatting
it would destroy data. A device in which blkid finds nothing and reports no
error is formatted with the capability's filesystem, ext4 when none is
named, only once it is proven blank: its start, the first MiB or the whole
of a smaller device, read past the page cache, is all zero. A read that
fails is UNAVAILABLE, so the kubelet stages the volume again and the
follower keeps its paths (CSI16), and a byte that is not zero is
FAILED_PRECONDITION. The reason: blkid gives the same silent answer when a
read of its probe fails as when it finds nothing, so that answer alone
would format over data; a thin device reads zeros where nobody wrote, so a
new volume passes; and the start holds the signatures of ext4 and xfs, the
two filesystems the driver makes, and of almost every other format. What
it leaves: a device whose data lies only past its first MiB, in a form
blkid does not know, is formatted; and a first format cut off part way, by
the end of the stage's time or a restart of the plugin, can leave bytes at
the start that blkid does not take for a filesystem, so every later stage
of that volume is refused until an operator zeroes its start
(`operations.md`, A Kubernetes volume the node plugin will not format). The
plugin makes xfs with the maker's profile of the oldest kernel series a node
may run (`xfsProfile`; Deployment), which names every feature the maker
knows, on or off, so the image's maker makes nothing that kernel cannot
mount, whatever its own defaults, and ext4 with the maker's defaults, which
that kernel mounts. A device already formatted is never formatted again, so
a volume keeps the features it was made with. The xfs maker refuses a device
smaller than 300 MiB, so such a volume's stage fails. A block volume is never
formatted or mounted at stage. Publish bind-mounts the staging path at the
target, read-only when asked, and for a block volume makes a file at the
target and bind-mounts the device node onto it. A read-only bind takes a
second mount, a remount of the bind read-only, so a publish whose remount
failed, or a plugin stopped between the two, leaves the bind read-write in the
plugin's own mount table: a repeat of a read-only publish that finds it so
runs the remount and answers its result, so that the plugin answers done only
for what it did. The remount changes the plugin's own mount alone: the copy of
the bind that reaches the host through the kubelet's directory, mounted both
ways, keeps the flags it had, and a write to a device node passes a read-only
bind, so what keeps a pod from writing is the read-only flag Kubernetes puts
on the container's mount. A read-write publish whose target is read-only is
done: a bind takes the read-only flag of its source, and the volume's mount
options can make the staging mount read-only. Unpublish unmounts the target
and removes it, a directory with a plain rmdir, never recursively; unstage
unmounts the staging path. Every step reads the mount table first, so a
repeat is done, as the spec asks, and a capability the driver does not serve
is FAILED_PRECONDITION at stage and publish, the spec's word for a capability
the volume does not support. The used-space report reads a mounted
filesystem's totals in bytes and inodes through the OS client, and a block
volume's size alone.

## Deployment

The driver ships as one container image and plain manifests in `deploy/`:
one file per Kubernetes object, and the Dockerfile beside them. It promises
Kubernetes 1.34 and newer; the lab proves it on the k3s release the suite
pins, with the sidecar images the manifests name.
The image is the Ubuntu release the lab runs with nvme-cli, the mount tools,
blkid and the two filesystem makers, plus the one binary; the pod's command
names the subcommand. Every Kubernetes node that runs the node plugin runs a
kernel of the 6.1 series or newer, the agents' floor too: the plugin formats
xfs with the maker's profile of that series (CSI17), and the image's build
fails on a base whose xfs maker has no such profile; the plugin reads no
kernel release, since an older kernel refuses an xfs mount with its own error
and loses nothing. Native NVMe multipath is on, and the NVMe over Fabrics
module, which nvme-tcp brings, loads at boot, since the plugin refuses to
start without either (CSI13). The manifests name the image by the
release's version, with no registry, and pull it only when it is absent, so a
lab imports it into every node under that name, and a user who publishes it in
a registry names it there in the controller and node manifests or maps that
name to the registry in every node's container runtime.
The kubelet directory is the one value of the node manifest an operator changes
per distribution, in its mounts and in the registrar's registration path, since
a distribution may keep the kubelet elsewhere; the manifest's header says where
it stands. The driver's namespace asks Kubernetes' pod security admission for
its privileged level, the one level that admits the node pods (CSI18), since a
cluster whose default level is stricter would refuse them with no sign but the
daemon set's events. The pods take the critical priority classes, which k3s
admits in any namespace; a cluster that admits them in kube-system alone
refuses the pods until the operator removes the class from the manifests or
allows it in the driver's namespace. Of dnv's services, the controller pod
reaches the gateway alone (CSI7) and the node pods the cdcs and the cntlrs'
ports; every other pod of the cluster reaches those ports, and the agents' and
etcd's, through its machine as well unless a network policy or the operator's
firewall blocks it (CSI20), and dnv authenticates no user and no host beyond
its NQN (`architecture.md`, Known limits), so a gateway bound to every address
of its machine is open to every pod and to the machine's network.

CSI18. **The node pods run privileged on the host's network.** The kernel
keeps an NVMe/TCP connection in the network namespace of the process that made
it, so a connect from a pod's own namespace would die with the pod. The
kubelet's pod and plugin directories are mounted both ways, so the mounts the
plugin makes reach the host and the pods; the device and sysfs trees give the
plugin the volumes' devices and the controllers' state; the host's nvme
directory, read-only, gives it the node's identity, and is made empty where it
is absent, so that the plugin starts and its own refusal names the missing
files (CSI13). The registrar sidecar announces the socket to the kubelet, and
the liveness probe sidecar answers the kubelet's health checks for the node
plugin, on the node's loopback alone, so the node pod opens no port to the
network; the controller copy's probe answers at its pod's own address (CSI20).
The two run as root, the owner of the plugin's socket and of the kubelet's
registration directory, so they need no capability, and have none, no
privilege escalation, the runtime's default system call filter and a read-only
root; no container of the pod talks to the API server, so the pod mounts no
account token. The node pods take the node-critical priority, so that the
scheduler makes room for them and the kubelet's eviction under resource
pressure passes them by, and every container of the driver requests a little
CPU and memory and has no limit: a memory limit could kill the plugin in the
middle of a stage, and a CPU limit would slow the follower.

CSI19. **The controller plugin runs as two copies, each sidecar leading in one
copy at a time.** Each copy is one pod with the plugin, the provisioner, the
attacher and the liveness probe around one socket, and both plugins serve: the
plugin keeps no state of its own, a create is repeatable by name (CSI9), and
an attach or a detach writes under the pool's revision (CSI10). The
provisioner and the attacher watch Kubernetes' objects and would act twice on
each, so each elects one leader through a lease in the driver's namespace; the
two leaders can sit in different copies, and a leader whose lease runs out
stops, its container starting again as a follower. A lease is no fence: at a
handover the old leader acts until its own renewal deadline passes, a few
seconds in which both copies can act, which CSI9's create by name and CSI10's
revision cover. When a machine is lost, the other copy takes both leases
within one lease time and one retry, about 20 seconds on the lab. The copies
prefer two machines, as a preference and no rule, so that a cluster of one
machine and a rolling update on two still run both; they tolerate the
control-plane taint, and leave a machine Kubernetes cannot reach after 30
seconds rather than the default 300, so that a second copy runs elsewhere
soon, about 75 seconds after the loss on the lab, and a second loss finds two
copies again. They take the cluster-critical priority, for CSI18's reason.
Every container of the pod runs as one unprivileged user, the owner of the
socket they share, with no capability, no privilege escalation, the runtime's
default system call filter and a read-only root. The pod keeps its account
token, which the sidecars need, and the plugin's container keeps it too:
hiding it from one container takes a token volume written by hand in the
others, and the plugin holds the gateway, the larger power. The sidecars'
rights within the driver's namespace, the leases, the capacity objects and the
lookup of the pod's owner, which every provisioner makes at its start, are a
namespaced role; their rights over the volume objects are a cluster role. The
plugin reads the gateway address and the cdc addresses from a config map, as
environment, and the node plugin reads the cdc addresses from the same map, so
the manifests of a cluster differ from the examples in that map, in the
storage classes, which name the operator's clusters, in the kubelet directory
where a distribution keeps the kubelet elsewhere, in the driver's image name
where a registry serves the image (Deployment), and in the addresses of the
example network policy, which stand as placeholders (CSI20). The driver object
asks for attach, no pod information on mount, the persistent lifecycle and
capacity tracking, since the plugin answers capacity; the leading provisioner
polls it and owns the capacity objects, for every class of the driver, those
with immediate binding included, since no scheduler needs those but the
capacity objects are the one place Kubernetes shows dnv's free space.

CSI20. **The driver opens one port per pod, and an example policy keeps other
pods off dnv.** The plugins serve their unix sockets alone; the controller
copy's liveness probe answers on its pod's own address, which the kubelet
probes, and the node pod's on the node's loopback (CSI18). Every other pod of
the cluster reaches dnv's ports through its machine unless something stops it,
and dnv checks no caller (`architecture.md`, Known limits).
`deploy/networkpolicy.yaml` is the example: one copy in every namespace whose
pods are not dnv's, `kube-system` aside where a dnv machine is a Kubernetes
node (below), an egress policy that lets the namespace's pods reach every IPv4
address but those of the dnv machines, and the API server's address and port
where a dnv machine runs it. The except list names every address of each dnv
machine, a Kubernetes node's pod bridge and overlay addresses and a docker
bridge included, since a dnv program bound to every address answers on each.
The policy is per namespace and allows only, so it allows all but dnv's
addresses. It names addresses, not ports, which keeps the list short and
closes a dnv program on whatever port it listens on, and so every other port
of a dnv machine too: the API server rule is the one way through, and it names
the API server's endpoint, which the policy sees after the cluster translates
the service address. Where a dnv machine is also a Kubernetes node, the
namespace's pods lose that node's own services with it: its kubelet, which a
metrics service in `kube-system` reads, any service on the node's host
network, and a node-local name cache, whose addresses sit on the node and so
stand in the except list. So `kube-system` takes no copy there, and a
namespace whose pods need such a service adds a rule for its address and port,
as the API server rule does. The cluster's name service otherwise needs no
rule: it runs in pods, whose addresses the first rule allows. The driver's
namespace takes no copy: its pods must reach dnv, the controller copies the
gateway and the node pods the cdcs and the cntlrs, and the node pods run on
the host network, which no policy holds. On k3s, whose embedded controller
enforces egress policies, the policy holds for a new pod within about a second
of its start, so its first connects can pass; network software that enforces
no egress policy, flannel alone, gives it no effect.

## The suite's dnv stack

The suite deploys the dnv stack itself, as the e2e suite does, with its
own copies of the deployment verbs: etcd, the gateway, the worker and the
cdc on the control-plane guest as the login user, one dn agent per
disk-node guest over a sparse loop device, one cn agent per controller-node
guest, each guest its own location, passed at its node's create (the lab's
disk-node and controller-node guests share one host name, the default
location: `architecture.md`, Disk nodes), and a cluster with every node
registered; the controller and disk nodes carry the udev rules of
`deploy/udev/` (`architecture.md`, Components: invocation reference), and
the hosts the mask of nvme-cli's autoconnector, for the run. No cn agent
of the suite runs with `--ignore-host-disconnection`, so every detach of the
suite waits for a real cut (CSI10), and a controller-node guest's nvmet must
be built with the kernel option the cut needs, as the lab's are, by the lab
tool `integtest/nvmet_debugfs.sh` (`layout.md`, Directory tree); preflight
checks it. The stack listens on a port block of its own, bound by no other
suite. The suite never builds a pool: the driver does, one per volume, so a
RAID1 pool of one slice takes four disk-node guests, which is the least the
suite accepts, and the failover needs the second controller-node guest. The
two storage classes of the cases name the suite's cluster, RAID1 and two
cntlrs, and the second also names xfs.

## Log records

The controller plugin logs `csi controller starting` with its endpoint,
gateway address, cdc addresses and build version (`common.BuildVersion`),
`csi controller serving` once it listens, and `csi controller stopping` when
it ends; the node plugin logs `csi node starting` with its endpoint, node
id, cdc addresses and build version, `csi node serving`
and `csi node stopping` the same way, and, since those are the moments the
failover contract turns on, `csi path connected` and `csi path dropped`
with the subsystem and the cntlr address, `csi discovery connection opened`
and `csi discovery connection dropped` with the cdc address, and
`csi follower sync error` with what failed when a follow could not finish or
the kernel's event socket failed: at Warn for a listed cdc it cannot reach
or whose log it cannot read, naming the cdc, since every drop waits and a
path that cdc alone lists is never learned until the cdc comes back or the
list is fixed, and at Info for every other failure, which its next pass, its
timer, the stage's own retry or the socket's reopening clears, but for a
failed drop after a stage that failed or at the stop, which nothing retries
(`log.md` R6).
Every CSI call logs `csi request` with the method and `csi reply` with the
method, the code, the duration and, on a failure, the error, under the
trace id the interceptor minted, at Info, but for the calls Kubernetes
repeats on a timer, whose records are at Debug while they succeed (CSI8).
The gateway calls a CSI call makes are the shared client chain's records
under the same id, at Info, the one `GetClusterCapacity` of a capacity poll
among them, and a pool read for every poll of a detach's or an attach's wait
and at most
one inspect of the primary for each poll, each with its whole reply (CSI10);
both plugins log
`log level changed` too (`log.md` R13).

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
without a registry, and a pod runs from it on each host and is then
removed. On that floor it proves the driver: a claim becomes a pool that
a pod reads and writes on either host, as a filesystem and as a raw block
device, and an xfs volume is formatted with the profile of the oldest kernel
series a node may run; a failover under IO fails no write, the host drops its
path to the old primary though every attach names a dead cdc, and the data
written before it reads back, for one pool at a time, since no case stops the
cn agent of a controller node that holds many pools; a node plugin away during
a failover drops the old primary's path at its start; a detach that Kubernetes
forces, while the old host is cut off from the control-plane guest and not
from the controller nodes, ends only once the primary has cut that host's
nvmet connection, which it cannot while the worker is stopped, as does the
attach to the other host that Kubernetes asks for meanwhile, and then the
old host's writes fail and the volume's new pod on the other host reads what
the old host wrote and writes; eight volumes live at once; a fresh volume
filled from its first block to its last at the device's speed, while its
pool grows under it, fails no write and reads back whole; the two controller
copies lead through two leases, and when the machine of the copy that leads
is cut off, the other copy takes both leases and serves a new volume, and a
new copy runs elsewhere while that machine stays cut; the example network
policy keeps the test namespace's pods off the gateway's, the cdc's and the
agents' ports, which a pod of an open namespace reaches, and leaves them the
API server and the name service; the upstream conformance test passes; and
every delete leaves nothing behind.
No case reads back a write made during a failover, which a failover can lose
in the cases that `architecture.md`, Known limits, A failover while the
old primary serves can lose an acknowledged write, lists.

### Topology and parameters

Every guest comes from the command line: one `--cp`, the k3s server and
dnv's control plane, exactly two `--host`, the k3s agents, at least two
`--cn` and at least four `--dn`, the dnv stack's nodes. Parameters: one
case to run alone, a cleanup-only run that sweeps every guest and stops,
a slice count the suite accepts and every pool ignores, since a volume's
pool has one slice, and a directory on the suite's machine to keep the logs
in. The pinned k3s version is a constant of the suite.
The suite's work directory on every guest and its shipped helper are its
own, named so that no other suite's is a prefix of them, and every process
its cleanup stops or lists by a pattern is matched by its program's path
under that directory, etcd's included, so that a cleanup never signals a
program another suite started.

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
which installs no package: besides the tree's own builds and the images of CSI4
it puts only csi-sanity, the pinned etcd release and, when the machine has no
`jq`, `gojq` into `integtest/bin` (`layout.md`, Directory tree); it needs Go,
ssh, scp and docker, and curl, tar, sha256sum and flock for the etcd fetch.

CSI4. **The suite's images reach a node by import.** The suite builds the
driver's image and the smoke case's image on its machine, saves them and
pipes them into the k3s image store of every node, so no pod pulls them and
no registry of the suite's takes part. The sidecar images the manifests
name come from their public registry, which each node pulls from once.

CSI5. **Cleanup removes what the suite started and nothing else.** The
detach case's out-of-service taint goes first, so that no host comes back
under it, which would evict the host's pods; then the cut of a host, the
machine case's or the detach case's, since Kubernetes cannot confirm the
deletes of a cut host's pods; then a worker the detach case stopped goes on,
since no cn agent converges without it, so no detach of the deletes would end
(CSI10); then the test namespace, the policy case's open namespace and the
driver's namespace are deleted and waited for, the driver's cluster-wide
objects and the storage classes with them; then the
hosts' connections of the driver's and dnv's and the discovery connections
to the suite's cdc go, with the hosts' mask of nvme-cli's autoconnector, then
the controller nodes' and the disk nodes' devices, exports, agents and udev
rules, with the controller nodes' nvmet entries of the two hosts that
attaches made, then the control plane's processes, the imported images and the
work directories, holder first. A disk-node guest whose work directory the
cleanup removed is then synced, given 10 s, and its root file system
trimmed, since the blocks of the backing files, about 2 GiB a guest after
the fill case, stay allocated in the host's image of the guest until the
guest discards them; a trim that fails is named in the cleanup's report. Cleanup runs at the start of every run and, on
success only, at its end; a failing run leaves everything in place and dumps
diagnostics, but for the taint and the cut, which it removes after the
diagnostics, the cut also by a timer on the host once its time limit has
passed, so that no cut outlives a run killed before its cleanup. k3s, the
hostnames and the identity files stay.

CSI6. **Test pods run on the hosts.** The control-plane node carries a
no-schedule taint the suite ensures on every run, and every pod names its
host.

CSI21. **A crash fails the case.** As each case ends, the suite reads every
log under the work directory of every guest, and the log of every container of
the driver's pods that has run, with the log of its previous run when its
restart count is above zero, and a line that starts as Go's report of a crash
starts, `panic: ` or `fatal error: `, fails the case with the guest or the
container, the log and the line, while a record that holds those words inside
a field never matches. Since a deleted pod's logs go with it, the suite reads
the containers the same way right before each step of its own that deletes or
replaces a driver pod: a rollout of the controller plugin, a change of the
node plugin's pod template, a scale-down, a pod's delete, the cut of a host
and the driver's removal. While the driver is up, a read that lists no pod, or
reads no log of a pod not being deleted, fails, and each read logs how many
logs it read; a pod being deleted whose log is gone, or a pod gone between the
listing and its read, is skipped and logged. A restart alone fails nothing, as
a sidecar that loses its lease exits by design (CSI19); the read logs the
count of each container that restarted. The preflight proves the read of the
guests' logs on a sample log it plants on the control-plane guest. Not read: a
container's runs before its previous one; a copy Kubernetes deletes from a cut
host, whose lines after the cut, and whose log once deleted, the API no longer
serves; and a crash while the cleanup at the end of a passing run stops a
program, as that cleanup removes the logs with the programs. Each read starts
one kubectl call per container log and reads each log from its start, about
twenty reads of some fifteen logs in a run, which by that count adds a few
minutes to a run.

### The cases

Every case runs on the suite's dnv stack with the driver deployed from the
manifests, the config map and the storage classes of the suite in place of
the examples, and both plugins at `--log-level debug`, the log level the
suites run their dnv daemons at, save one dn agent in one case of the dn
suite (`dnagent_integtest.md`, Conventions, Log level); the IO pods run the
driver's own image, since it carries the tools they need and is already on
every node. The deploy pulls the sidecar images a node lacks, on every node,
so that a controller copy that moves starts at once, and checks the driver's
shape: two controller copies on two nodes, both leases held by controller
pods, the critical priorities the pods were given, and the node probe
listening on the loopback alone; the test namespace then carries the example
network policy, so every case runs under it. Before the cases, the suite
sends SIGUSR1 and then SIGUSR2 to one controller plugin copy, from that
copy's node as root, at the pid `crictl` reports for its plugin container,
as the operations doc's step for a pod does (`operations.md`, The logs), and
each must show as one `log level changed` record of that copy, both naming
debug, the plugin's start log level (`log.md` R13). Each case that creates
volumes ends with the check that nothing is left: no pool in the cluster, no
dm device, array or export on a node, no connection of the driver's on a
host. Every detach of a case waits for the primary's cut (CSI10), one
converge and one report of the primary after the detach's write, and every
attach to an empty list waits for a report at the revision it read, which
right after a create is the primary's first converge at the pool's revision,
read again at each move the worker makes to it while it builds the pool, as
in the sanity case, whose conformance test also unpublishes volumes it has
just published.

**k8s.** The suite writes a program that prints one line and sleeps, builds
it static, wraps it in an image from scratch and imports it into every
node. It applies one pod per host, each pinned to its host by node selector
and never pulling. Each pod is Running on its host, each log is the one
line, and the pods go.

**policy.** A pod of the driver's image on host0 in the test namespace, under
the policy, and one in a namespace of the case's own without it, try the
ports of the dnv stack's programs: the gateway and the cdc, a controller
node's agent and a disk node's agent. Each port is open from the open
namespace, else its closing would prove nothing, and closed from the test
namespace, which the case waits for within a bound, since the policy holds
for a new pod only after about a second. The policy closes a machine's
address whole, so an agent's port stands for every port of its machine, the
NVMe/TCP port included, which is not tried: an nvmet port listens only once
a subsystem is linked to it, and no pool exists when the case runs. etcd
listens on the loopback alone and is not tried either. From the test
namespace the API server's service address answers and the cluster's name
service resolves a name. The pods and the open namespace go.

**volume.** The first class's capacity object is published with a capacity
above zero, and comes to hold the gateway's placeable bytes for the class's
shape, read with `dnvctl cluster get-capacity` (CSI11), which proves that
the provisioner publishes the gateway's figure; that the plugin keeps no
sums of its own is the plugin's unit tests' to prove, since on the suite's
disk nodes, of equal free space in a location each, sums over the node
entries would give the same bytes; a filesystem claim becomes a pool of one
slice and two cntlrs; a pod on host0 writes a pattern and its digest; the
same claim under a pod on host1 checks the digest, while every path host1
holds to it shows a controller-loss timeout of "off" (CSI14); a raw block
claim is written and read back through the device by a pod on host0; a
claim of a second class that names xfs is formatted with the profile of the
6.1 series (CSI17), which the filesystem's own report shows, read in
host0's node plugin container at the target of a pod that holds the claim,
since only a container with the host's devices can read it: two features
the image's maker turns on by default are off; then every delete, and
nothing left.

**failover.** The controller plugin's cdc list names a dead address beside
the cdc for the case, so the publish context of every attach does, while the
node plugins keep the list they started with (CSI15). A pod on host0 writes a
pattern, then writes in a loop with fsync, counting failures and the longest
write. The suite reads which controller node holds the volume's primary and
stops that cn agent, as the e2e suite's cutoff case does, so that every
kernel object it built goes on serving until the fence; the primary moves;
host0 drops its path to the old primary within a bound, which a node that
followed the dead cdc of the context would never do; the loop runs on
against the new primary and is stopped; it has failed nothing, and the
longest write is reported. The pod goes, and a pod on host1 reads the
pattern back through the new primary, since host0's page cache holds it.
The agent is given back, then every delete, the controller plugin's list
back, and nothing left.

**restart.** A pod on host0 holds a claim and runs no write loop. The node
plugin is taken off host0 by a node selector on its daemon set, which leaves
host0's paths and mount in place (CSI16), and the primary's cn agent is
stopped: the primary moves while no plugin runs on host0, which keeps its path
to the old primary. The plugin comes back, and within a bound well under its
timer's period host0 holds no path to the old primary, since the plugin's
first pass runs at its start (CSI15); the pod still writes its volume. No
change notice starts that pass: the old primary is not replaced within the
case. The agent is given back, then every delete, and nothing left.

**detach.** A pod on host0 writes a pattern and its digest, then writes in a
loop with fsync, keeping its counts and the times of its first failure in the
container, which the suite reads over ssh through the node's container runtime,
since host0 is later cut off from the API server; the primary's nvmet debug
directory shows host0's nvmet connection to the volume's subsystem. The node
plugin is taken off host0, as in the restart case, so that nothing on host0
drops a path, and both leases are brought to one controller copy off host0, as
the machine case brings them; host0 is cut off from the control-plane guest
alone, by a firewall table that keeps its paths to the controller nodes and the
suite's ssh, and reads NotReady. The worker is stopped, so that no cn agent
converges and the primary cannot cut, and host0 gets the out-of-service taint:
Kubernetes force-deletes the pod and detaches the volume, and the detach takes
host0 off the subsystem's list; for a time longer than one call of the
attacher, the volume's attachment to host0 stays, and so does host0's
connection on the primary, each read by a pass whose reads both answered, one
of them in the hold's last seconds, and the controller plugin's log shows a
detach that ran out of time meanwhile; the attachment to host1, which
Kubernetes asks for once that detach call has failed, is read standing and
unattached in some pass, and never attached, and host1 is never on the
subsystem's list, since the attach waits for the same cut before its write. The
worker goes on; host0's connection on the primary goes, and only then the
attachment to host0 goes and the one to host1 reads attached (CSI10), the last
detach reply, a repeat, OK; the standby's connection at the detach's end is
logged, as no detach waits for it. Within a bound host0's writes, which its
container goes on making, fail: they fail only once every cntlr of the pool has
ended host0's connection, the standby's included (`cnagent.md` CN33), since IO
queues while a standby's path is left (The node plugin), and each reconnect has
been refused with DNR and its controller deleted (CSI14), so a write that waits
instead fails the case naming each cntlr that still shows host0's connection.
Then host0 holds no controller of the volume, and so no cntlr shows its
connection. The claim's new pod on host1 checks the pattern host0 wrote, then
writes and checks its own. The case logs the controller plugin's detach replies
and its attach requests and replies, and the times from the taint: host0 off
the list, the worker back, the cut on the primary, the detach's end, host1's
attach, host0's first failed write and the new pod's start. The taint and the
cut go, host0 comes back Ready with its node plugin, the controller is scaled
back to two copies, then every delete, and nothing left.

**many.** Eight claims and eight pods at once, four per host, each writing
a pattern and checking it; the cluster holds eight pools; then every
delete, and nothing left.

**fill.** A fresh claim of 4 GiB in block mode, held by a pod on host0, and
fio on host0 against the volume's device: one pass that writes every block
from the first to the last with direct IO, eight writes of 1 MiB in flight,
each block carrying its own check, then a read of every block against its
check. The pool starts with one data group of an eighth of the volume and
grows one group at a time while the fill runs (`dnv-worker.md`, Thin-pool
auto-grow). A write error, a check that fails or a short count fails the
case, and so does a count of the worker's data grow records for the pool
other than the data groups the pool added past its first, or a record that
does not start from the total the one before it reached, since each grow
the fill causes is one the record times (`dnv-worker.md` AR6, its record
`grow done`). The longest write and the median, each a write's whole time from
its submission to its completion, the write rate and each grow's record,
its time and its seconds, are measured and logged, not asserted, since no
rule bounds how long a grow takes. On the lab, at the suite's extent size
and with the legs on four disk-node guests, the fill wrote at about
114 MiB/s; the median write took 17 ms and the longest at least 6.1 s, a
write that waited for a grow, both read as their completion time alone,
which leaves the submission out; the pool grew eight times, from 509 to
4581 data blocks; no write failed and every block read back. Each grow took
about 4 s from the pool passing its mark: the writes took 36 s, of which
the writes alone, eight of 1 MiB in flight at the median's 17 ms, need
about 9 s, so about 27 s went to waits for the eight grows. The waits for
the grows, not the device, set the fill's rate. Then the pod and the claim
go, and nothing left; the disk nodes' cleanup trims what the fill wrote
into their guests (CSI5).

**machine.** Every driver container has run without a restart so far,
which the case checks before it cuts anything, since a sidecar that loses its
lease exits by design. Both leases are brought to the copy on one host H,
never the control-plane guest: the other copy is given a lower deletion cost
and the controller scaled to one copy, which the ReplicaSet makes by deleting
it, and once H's copy holds both leases, back to two, the new copy on a node
other than H. H is then cut off the network by a firewall table that keeps
the suite's ssh and removes itself after a time limit; a new claim and a pod
on the other host follow at once. Within a bound the copy that is not cut
holds both leases, and the claim is bound and its pod writes and reads its
pattern; within a second bound, which the default 300 seconds of the
copies' toleration would pass, a new copy runs Ready on a node other than H
while H stays cut. H is healed, comes back Ready with two copies and its
node pod, and no lease names the copy that was cut; then every delete, and
nothing left.

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
hostnames and machine-ids distinct (CSI2), native multipath, the memory
floor, the firewall and timer tools of the machine and detach cases and the
fio of the fill case on the hosts, and the hosts' identities present and
different; on the node guests
the agents' tools and modules and this suite's ports free, and on a
controller-node guest, once nvmet is loaded, the facts the cn agent's start
check reads (`cnagent.md` CN-CM2): debugfs mounted at /sys/kernel/debug,
nvmet's debug directory there, which the kernel option
`CONFIG_NVME_TARGET_DEBUGFS` makes, and no lockdown the lockdown file
shows, as no start line of the suite takes `--ignore-host-disconnection` and
the detach case needs the cut, a failure naming `integtest/nvmet_debugfs.sh`,
the lab tool that builds and installs nvmet modules with the option
(`layout.md`, Directory tree). The start check's other fact, configfs
mounted, is read by no guest check, so a guest without it fails at the cn
agent's start. Then k3s:
present at the pin or installed (CSI1), every node Ready at the pin, the
taint (CSI6), and the cluster's own name service available. Then the dnv
stack and the driver.

### Diagnostics on failure

A failing run prints first every crash line the guests' logs and the
driver's container logs hold (CSI21), then the nodes, every pod, the claims,
volumes and
attachments, the pods and events of the suite's namespace, the state of the
cut of the machine and detach cases on each host, the nodes' taints, the
sidecars' leases, the logs of both controller copies and every node plugin,
with the previous run of a sidecar that restarted, dnvctl's list of pools,
and from each guest its facts, its k3s state, its dnv processes, the
driver's plugins in their pods among them, its NVMe
subsystems and discovery connections, its dm, md, nvmet and loop state, the
nvmet connections that nvmet's debug directory shows, its memory, and the
tails of its k3s journal and of its programs' logs. A run asked to keep its logs
copies to the suite's machine at its end the k3s journal of the
control-plane guest and of each host, the logs of the control plane's
programs and of the agents, one controller plugin's and one node plugin's
log, and the conformance test's logs, its plugins' included.
