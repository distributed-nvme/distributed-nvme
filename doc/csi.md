# csi.md — dnv-csi, the CSI driver, and its suite

This document owns the Kubernetes side of dnv: dnv-csi, the driver that
connects dnv to Kubernetes through the Container Storage Interface (CSI),
the plugin API Kubernetes uses to create, attach and mount volumes, and the
suite that proves it, `integtest/csi_test.sh`. It leans on
`architecture.md` for the object model and the API contract, on
`gateway.md` for each call the driver makes, and on `cdc.md` for what a host
must do to follow a pool's cntlrs.

## Words

A CSI driver has two parts. The controller plugin runs once per Kubernetes
cluster and talks to the gateway. The node plugin runs on every Kubernetes
node that may mount a volume and talks to the kernel: it connects, finds the
device, formats and mounts. A storage class is the Kubernetes object that
names a driver and its settings. A persistent volume claim is a Kubernetes
user's request for a volume of a storage class. Attach means Kubernetes
binds a volume to one node, through the controller plugin; mount means the
node plugin puts the device under a path for a pod. k3s is a one-binary
Kubernetes distribution that bundles the container runtime and the network
plugin; the lab runs it.

## The volume model

Every Kubernetes volume is a storage pool of its own with one thin device,
one subsystem and one namespace. The pool, the thin device and the
subsystem's NQN carry the name Kubernetes gives the volume, the namespace
uuid derives from that name, and the volume id Kubernetes stores names the
cluster and the volume. So every object of a volume is found by one name,
the per-pool limits on subsystems, namespaces and hosts never bind, and the
tenant promise of dnv holds per volume (`architecture.md`, System
overview).

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
other connection. It never talks to the gateway: the controller plugin
hands it the subsystem NQN, the namespace uuid and the cdc addresses at
attach time.

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

The suite drives a real k3s cluster on three guests of the lab. Its one
case proves the Kubernetes floor everything else stands on: the three
guests are distinct machines, k3s is present at the pinned version or gets
installed, an image built on the driver reaches every node without a
registry, and a pod runs from it on each host and is removed with its
namespace.

### Topology and parameters

Every guest comes from the command line: one `--cp`, the k3s server, and
exactly two `--host`, the k3s agents. Controller-node and disk-node guests
are accepted for the dnv stack of the volume cases. Parameters: one case to
run alone, a cleanup-only run that sweeps the three guests and stops, the
slice count of the volume cases' pools, and a directory on the driver to
keep the k3s journals in. The pinned k3s version is a constant of the suite.
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
as the e2e suite does (`e2e_integtest.md` E2E10), and the two hosts' host
NQNs must differ; the fix is named, not applied.

CSI3. **kubectl runs on the control-plane guest only**, as the k3s binary's
subcommand over ssh, with every manifest piped from the driver. The driver
machine installs nothing: it needs Go, ssh, scp and docker.

CSI4. **Images reach a node by import.** The suite builds its image on the
driver, saves it and pipes it into the k3s image store of every node, and
every pod it runs pulls nothing. No registry takes part.

CSI5. **Cleanup removes what the suite started and nothing else.** Every
Kubernetes object of a run lives in the suite's namespace, so the cleanup
is one delete, waited for, plus the removal of the imported image from
every node and of the work directories. Cleanup runs at the start of every
run and, on success only, at its end; a failing run leaves everything in
place and dumps diagnostics. k3s, the hostnames and the identity files
stay.

CSI6. **Test pods run on the hosts.** The control-plane node carries a
no-schedule taint the suite ensures on every run, and every pod names its
host.

### The cases

**k8s.** The suite writes a program that prints one line and sleeps, builds
it static, wraps it in an image from scratch and imports it into every
node. It applies the namespace and one pod per host, each pinned to its
host by node selector and never pulling. Each pod is Running on its host,
each log is the one line, and the namespace's delete removes both pods.

### Preflight

The driver's checks run first: its tools and a usable docker. Then the
helper is shipped and the guest checks run: passwordless sudo on all three,
the hostnames and machine-ids distinct (CSI2), native multipath and the
memory floor on the hosts, and the hosts' identities present and
different. Then the start cleanup, then k3s: present at the pin or
installed (CSI1), every node Ready at the pin, the taint (CSI6), and the
cluster's own name service available.

### Diagnostics on failure

A failing run prints the nodes, every pod, the pods and events of the
suite's namespace, and from each guest its facts, its k3s state, its
memory and the tail of its k3s journal. A run asked to keep its logs copies
the k3s journal of each guest to the driver at its end.
