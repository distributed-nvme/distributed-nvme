# Introduction
Distributed NVMe is a distributed block storage system like AWS EBS or
Ceph block device. It can be used as a network storage for the k8s
persistent volume or openstack cinder driver. Below are key features
of the Distributed NVMe:
* Using standard NVMe-oF interface between the virtual block device
  and the host. Support both TCP NVMe-oF and RDMA protocol.
* High performance. A single virtual block device could provide more
  than 2M IOPS and less than 1 millisecond latency in the TCP NVMe-oF
  mode. The RDMA mode could be even better.
* Lots of features: multipath, data redundancy, snapshot, thin
  provision, online clone (Possile to support encryption,
  deduplication and compression in the future).
* The whole dataplane is pure linux kernel. The dataplane is
  implemented by the linux kernel nvme host/target and device
  mapper. Don't misunderstand, the Distributed NVMe is a distributed
  storage system, a single virtual block device is implemented by
  multiple storage servers, each storage server serves to multiple
  virtual block devices. Each storage server just use the linux kernel
  nvme host/target and devcie to implement its functions.

As the dataplane is ready in linux kernel, we just need to implement
a control plane. Right now it is in prototype stage, we have several
scripts in the `prototype` folder. We can invoke these scripts on each
storage server to create a virtual block device and verify its
functions and performance. The next step is creating a real
controlplane instead of the scripts, thus we can invoke an API to
create/delete a virtual block device.

# Architecture
Instead of showing the whole architecture directly, here we start from
a simplest scenario, then add more features to it, eventually it
becomes a real distributed storage system.

The simplest scenario is the single host case:

![000HostOnly](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/000HostOnly.png)

We have multiple NVMe disks, attach them to the PCIe bus of a linux
server. Then we can create one or more device mapper devices on top of
them. The [device mapper](https://pages.github.com/) is a linux
kernel framework, which could create logical block devices on top of
other block devices. E.g., we can create raid1/5/6 for data
reduandncy, raid0 for high performance, and we can also create thin
provision, encrption devices and so on.

All these are local disks. If we provide these disks to a virtual
machine or a container, then we migrate the virtual machine or
container to another server, these disks will be unavailable for the
virtual machine or container.

So we can decouple the host and the storage:

![010HostAndTarget](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/010HostAndTarget.png)

The `Target Server` connects to the physical disks, and creates device
mapper devices to provide features like raid, thin provision and so on. Then
the `Target Server` exports the logical disk to a `Host` through the
NVMe-oF interface. Now if a virtual machine or container is migrated
from one `Host` to another `Host`, the `Target Server` could export to
the new `Host`. We can have multple `Hosts` and multiple `Target Servers`.
Each host could connect to multple `Target Servers` and each
`Target Server` could serve to multiple `Hosts`.

When a `Host` connect to a NVMe-oF device, all the data of that
NVMe-oF devcie is on the same `Target Server`. Even we create a raid1
on top of two physical disk, if the `Target Server` fails, the NVMe-oF
device won't be accessed. To address this issue, we can split the
`Target Server` to two layers:

![020CnDnSingle](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/020CnDnSingle.png)

Now the `Target Server` is splited to `Controller Node` and `Disk Node`.
The `Disk Node` connects to the physical devices, and uses
[dm-linear](https://docs.kernel.org/admin-guide/device-mapper/linear.html)
to split the physical devices, thus one physical device can server
multple `Hosts`. We create the device mapper logical devices on the
`Controller Node`. E.g. We can create a raid1 on a `Controller Node`,
the two underling devices are from two different `Disk Node`. If one
`Disk Node` fails, we can create a `dm-linear` device from another
`Disk Node` then re-mirror the raid1 on the `Controller Node`. Thus
the `Disk Node` is not a single point of failure.

One `Disk Node` split the physical disks to logical disks, provide
these logical disks to multiple `Controller Nodes`. For a given
virtual disk, the `Controller Node` creates a `Controller` for it, the
`Controller` connect to the logical disks from multiple `Disk Nodes`,
create device mapper devices on the logical disks, then export it to
the `Host`.

From the `Host`'s perspective, there is a storage controller on the
`Controller Node`. The `Host` connect to the `Controller`, then use the
virtual disk on that `Controller`.

One `Disk Node` has multiple `Logical Disks`. One `Controller Node`
has multple `Controllers`. All of them are connected by the
NVMe-oF. So they are a many to many relationship.

![030CnDnMany](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/030CnDnMany.png)

The `Contgroller Node` is single point of failure. To address this
issue, for a given virtual disk, we can provide a `Standby Controller`:

![040ActiveStandby](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/040ActiveStandby.png)

From the `Host` perspective, it connects to the virtual disk from two
paths. One path connects to the `Active Controller`, another path
connects to the `Standby Controller`. The NVMe-oF protocol support
multipath natively. And it has an ANA group to show the different
state of the multipath. The `Active Controller` tells the `Host` it
is in the Optmized state. The `Standby Controller` tells the `Host` it
is in the Inaccessible state. So the `Host` will only send IOs to the
`Active Controller`. When the controlplane finds that the
`Active Controller` fails, the controlplane will promote the `Standby Controller`
to `Active Controller`. Then the `Host` will be notified by the NVMe
async event and send IOs to the new `Active Controller`.

Now the system is distributed and no single point of failure. But the
`Controller Node` is a performance bottleneck. Assuming we create a
raid1 on a `Active Controller`. When a `Host` send 1 write IO to
the `Active Controller`, the `Active Controller` should receive the 1
IO and dispatch 2 IOs to the two `Virtual Disks` of the raid1
device. So the IOs on the `Active Controller` is 3 times than the
`Host`. If the `Controller Node` has the similar hardware
configuration as the `Host`, it can not even satisfy a single
`Host`. To address this issue, we can add a dispatcher layer between
the `Host` and the `Controller Node`:

![050ThreeLayers](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/050ThreeLayers.png)

For keeping it simple, let's ignore the `Standby Controler`
temporary. For a given virtual disk, the `Controller Node` creates a
`Leg`. The `Leg` is similar as the `Controller`, it is aggregated by
multiple device mapper devices. A virtual disk could have multiple
`Legs`, they are on the different `Controller Nodes`. These `Legs`
aggreagte to a raid0 device on the `Dispatcher Node`. Then the
`Dispatcher Node` provide the raid0 device to the `Host`.

The `Dispatcher Node` works as a stateless load balancer. It receives
IO requests from the `Host`, then dispatches the IOs to the different
`Legs`. A single virtual disk could be associated to multiple
`Dispatch Nodes`. They all export NVMe-oF connections to the `Host`,
and report their ANA group state to Optmize. The `Host` (like a linux
server) could be configured to send IOs in a round-robin manner to
them.

In this way, we removed the performance bottleneck. One `Host` could
dispatch IOs to multiple servers. But we don't really have a third
layer. We can combine the `Dispatch Node` and the `Controller Node`
together. Below is a more compact architecture:

![060Compact](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/060Compact.png)

![070CompactAndStandby](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/070CompactAndStandby.png)

![080MoreControllers](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/080MoreControllers.png)

![090Leg](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/090Leg.png)

![100Cluster](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/100Cluster.png)

![110VirtualDisk](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/110VirtualDisk.png)

![120Snapshot](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/120Snapshot.png)

![130Extend](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/130Extend.png)

![140Failover](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/140Failover.png)

![150Clone](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/150Clone.png)

![160Move](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/160Move.png)

![iops](https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/iops.png)
