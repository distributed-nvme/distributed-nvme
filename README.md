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

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/000HostOnly.png" width="300">

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

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/010HostAndTarget.png" width="300">

The `Target Server` connects to the physical disks, and creates device
mapper devices to provide features like raid, thin provision and so on. Then
the `Target Server` exports the logical disk to a `Host` through the
NVMe-oF interface. Now if a virtual machine or container is migrated
from one `Host` to another `Host`, the `Target Server` could export to
the new `Host`. We can have multple `Host`s and multiple `Target Server`s.
Each host could connect to multple `Target Server`s and each
`Target Server` could serve to multiple `Host`s.

When a `Host` connect to a NVMe-oF device, all the data of that
NVMe-oF devcie is on the same `Target Server`. Even we create a raid1
on top of two physical disk, if the `Target Server` fails, the NVMe-oF
device won't be accessed. To address this issue, we can split the
`Target Server` to two layers:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/020CnDnSingle.png" width="300">

Now the `Target Server` is splited to `Controller Node` and `Disk Node`.
The `Disk Node` connects to the physical devices, and uses
[dm-linear](https://docs.kernel.org/admin-guide/device-mapper/linear.html)
to split the physical devices, thus one physical device can server
multple `Host`s. We create the device mapper logical devices on the
`Controller Node`. E.g. We can create a raid1 on a `Controller Node`,
the two underling devices are from two different `Disk Node`. If one
`Disk Node` fails, we can create a `dm-linear` device from another
`Disk Node` then re-mirror the raid1 on the `Controller Node`. Thus
the `Disk Node` is not a single point of failure.

One `Disk Node` split the physical disks to logical disks, provide
these logical disks to multiple `Controller Node`s. For a given
virtual disk, the `Controller Node` creates a `Controller` for it, the
`Controller` connect to the logical disks from multiple `Disk Node`s,
create device mapper devices on the logical disks, then export it to
the `Host`.

From the `Host`'s perspective, there is a storage controller on the
`Controller Node`. The `Host` connect to the `Controller`, then use the
virtual disk on that `Controller`.

One `Disk Node` has multiple `Logical Disk`s. One `Controller Node`
has multple `Controller`s. All of them are connected by the
NVMe-oF. So they are a many to many relationship.

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/030CnDnMany.png" width="600">

The `Contgroller Node` is single point of failure. To address this
issue, for a given virtual disk, we can provide a `Standby Controller`:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/040ActiveStandby.png" width="600">

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
IO and dispatch 2 IOs to the two `Virtual Disk`s of the raid1
device. So the IOs on the `Active Controller` is 3 times than the
`Host`. If the `Controller Node` has the similar hardware
configuration as the `Host`, it can not even satisfy a single
`Host`. To address this issue, we can add a dispatcher layer between
the `Host` and the `Controller Node`:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/050ThreeLayers.png" width="600">

For keeping it simple, let's ignore the `Standby Controler`
temporary. For a given virtual disk, the `Controller Node` creates a
`Leg`. The `Leg` is similar as the `Controller`, it is aggregated by
multiple device mapper devices. A virtual disk could have multiple
`Leg`s, they are on the different `Controller Node`s. These `Leg`s
aggreagte to a raid0 device on the `Dispatcher Node`. Then the
`Dispatcher Node` provide the raid0 device to the `Host`.

The `Dispatcher Node` works as a stateless load balancer. It receives
IO requests from the `Host`, then dispatches the IOs to the different
`Leg`s. A single virtual disk could be associated to multiple
`Dispatch Node`s. They all export NVMe-oF connections to the `Host`,
and report their ANA group state to Optmize. The `Host` (like a linux
server) could be configured to send IOs in a round-robin manner to
them.

In this way, we removed the performance bottleneck. One `Host` could
dispatch IOs to multiple servers. But we don't really have a third
layer. We can combine the `Dispatch Node` and the `Controller Node`
together. Below is a more compact architecture:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/060Compact.png" width="600">

We still aggregate all things to the `Active Controller`. Each
`Active Controller` connects to mutliple `Logical Disk`s and aggregate
them to `Leg`s. All `Leg`s in all `Active Controller`s aggregate to a
raid0 disk, then we export the raid0 disk to the `Host`. As we should
aggregate all `Leg`s of all `Active Controller`s, each
`Active Controller` should export all its `Leg`s to other
`Active Controller`s. So each `Active Controller` should connect the
`Remove Leg`s from other `Active Controller`s, then aggregate both
`Leg`s and `Remove Leg`s to a raid0 device and export it over
NVMe-oF. The `Host` could read/write any `Active Controller`.

Then we can add the `Standby Controller` back:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/070CompactAndStandby.png" width="600">

An internal view of a `Leg`:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/090Leg.png" width="600">

The above `Leg` uses raid1 as an example, we could also change it to raid5/6.

When we have multiple `Active Controller`s, their `Leg`s should be "full mesh", each
`Leg` should be exported to all other `Active Controler`s:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/080MoreControllers.png" width="600">

We should be careful about a consistency issue of the multiple
`Active Controller`s architecture. Considering below case:
1. The `Host` sends a write IO to an `Active Controller`
2. The `Active Controller` fails, and no response.
3. The `Host` gets an IO timeout, then retries the IO on another `Active Controller`
4. The retired IO succeeds.
5. The `Host` sends more IOs to other `Active Contorller`s.
6. The failed `Active Controller` comes back, and delived all IOs on it.
7. The old IOs from the failed `Active Controller` overwrite the new
   IOs, so the data corrupt.

To avoid such thing, we can rely on the controlplan health check and
the NVMe-oF keep aliave between the `Active Controller`s. If any of
them reports an issue, we should fence the failed `Active Controller`.
But the `Host` shouldn't retry a failed IO on another path too
fast. To make sure we have enough time to detect the failed
`Active Controller` and fence it, the NVMe timeout on `Host` should be
at least 2 times than the NVMe-oF keep aliave timeout between the
`Active Controller`s. On a linux kernel, the default NVMe timeout is
10 seconds, the NVMe-oF keep alaive timeout is 5 seconds. So they
should just work.

Below is a view of the Distributed NVMe Cluster:

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/100Cluster.png" width="800">


The cluster Data Plane has multiple `Controler Node`s and multiple `Disk Node`s.
Each `Disk Node` connects to a single `Physical Disk` (NVMe SSD) The
whole customer could provide multiple virtual disks to the `Host`s.

The `Disk Node` is still a logical concept here. If a sever has
multiple NVMe disks, we could have multiple `Disk Node`s on that
server. These `Disk Node`s should use different NVMe-oF svc_id (which
means listen on different tcp ports in TCP NVMe-oF).

The cluster Control Plane is a etcd cluster and multiple `CP Server`s
(Control Plane Servers). We store all the virtual disks configurations
to the etcd cluster. All the `CP Server`s are stateless. They have
multiple responsibles:
* Accept APIs from users (e.g. Create/Delete/Attach/Detach/Clone
  disks), store the virtual disks information to the etcd cluster.
* Read data from etcd and send them to the agents on the
  `Controller Node`s and `Disk Node`s.
* Run health check against all `Controller Node`s and `Disk Node`s.
* Run background tasks like checking the status of online clone.

Below is a view of a single virtual disk:
<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/110VirtualDisk.png" width="800">

# Operations

Create snapshot

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/120Snapshot.png" width="800">

Extend a `Leg`

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/130Extend.png" width="800">

Failover

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/140Failover.png" width="800">

Clone

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/150Clone.png" width="800">

Move data from one `Logical Disk` to another

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/160Move.png" width="800">

# Performance
Below is the K IOPS of the single virtual disks. The virtual disks use
raid1 for data redundancy. They have different `Active Controller`s
and differnet `Leg`s. By using more `Active Controller`s and more
`Leg`s, we can get better performance. When we increase the workload
presure, the IOPS might be high but the latnecy would increase
too. During the test, we keep the average latency less than 1
millisecond and  keep the p99 latency less than 2 milliseconds, then
we measure the max IOPS we could get:

* The left one is the raw PCIe NVMe disk. It is about 140K IOPS.
* The second is a 1 `Controller` 4 `Leg`s virtaul disk. It is
  about 400K IOPS
* The third is a 2 `Controller`s 8 `Leg`s virtual disk, it is
  about 700K IOPS
* The fourth is a 4 `Controller`s 16 `Leg`s virtual disk, it is
  about 1.2M IOPS
* The fifth is a 8 `Controller`s 32 `Leg`s virtual disk, it is about
  2M IOPS.

<img src="https://github.com/distributed-nvme/distributed-nvme/blob/main/doc/img/iops.png">