# Core glossary

The core words of dnv in plain language: the short list to read first. The entries are in reading order, not in alphabetical order. An entry says what a thing is and leaves out how it works in detail, and it uses only ordinary Linux storage words and other words of this list. `glossary.md` holds the full vocabulary with the exact definitions; where an entry here and `glossary.md` disagree, `glossary.md` is right.

## Machines

**cluster** — The container of everything else: every node, storage pool and volume belongs to one cluster. One etcd can hold many clusters.

**node** — A disk node or a controller node.

**disk node, DN** — A machine that gives one raw disk to the cluster. The space of that disk is handed out to storage pools in extents. A machine with several disks can run one dn agent for each disk, and each is then a disk node of its own.

**extent** — The unit in which disk space is handed out. Its size is set per cluster. The capacity of a controller node is counted in extents as well.

**controller node, CN** — A machine that runs the volume logic on top of disk-node space: RAID1, thin pools, striping and the NVMe-oF export to hosts. It gives no storage to the cluster; all volume data is on the disk nodes.

**host** — A machine that uses the volumes, as an NVMe-oF initiator. It may connect to several controller nodes for the same volume; the kernel joins these paths into one multipath device, and ANA tells it which path serves IO.

**location** — The failure domain a node is registered with, for example a rack. By default every node is its own location. Allocation places the legs of a new group in different locations. It also keeps the cntlrs of a pool, a new spare leg and the new side of a migration out of locations that are already in use, when it can.

## Programs and state

**control plane, data plane** — The control plane is etcd and the programs that work on it: the gateway, the worker and the cdc. The data plane is the disk nodes, the controller nodes and the NVMe-oF paths that carry the IO.

**etcd** — The key-value store of the control plane. It holds the desired state of every node and storage pool. Only the gateway, the worker and the cdc talk to it.

**desired state** — What etcd says a node or a storage pool should look like. The agents make the nodes match it.

**gateway** — `dnv-gateway`, the API server. Every request of an operator goes through it: it checks the request and, when the request changes something, writes the change to etcd. It builds nothing on the nodes.

**worker** — `dnv-worker`, the program that makes the nodes follow etcd. It watches etcd, sends each agent the desired state, checks health and runs the reactions. Its roles are the dn role, which drives the disk nodes, the cn role, which drives the controller nodes, and the sp role, which drives the sides and the cntlrs of the storage pools. Several workers can run at once and share the work.

**agent** — `dnv-agent`, the program on every node: the dn agent on a disk node and the cn agent on a controller node. It builds and removes the local devices (device mapper, md, nvmet, NVMe connections) so that the node matches its desired state, and it reports the real state of the node. It keeps the last desired state it accepted in local files and rebuilds from them after a restart. It never talks to etcd.

**cdc** — `dnv-cdc`, the NVMe-oF discovery service. A host asks it where its subsystems are, and it answers with the addresses of the controller nodes that export them. It tells a connected host when that answer changes.

**dnvctl** — The command line tool of the operator. It has one command for each call of the gateway API.

## Inside a storage pool

**storage pool, SP** — The unit that serves volumes. It has cntlrs on controller nodes, and disk space on disk nodes that is organized as slices, groups and legs. Thin devices are created in it.

**tenant** — A user of the block storage system built on dnv, for example a Kubernetes or OpenStack Cinder user. dnv knows no tenants and authenticates nobody. What it promises the layer above is that the data of one storage pool is never readable from another storage pool.

**cntlr** — One instance of a storage pool on one controller node. A pool has one or more cntlrs, each on a different controller node. Exactly one of them is the primary, and the others are standby.

**primary** — The cntlr that serves IO. It connects the legs, assembles the RAID1 arrays, runs the thin pools and exports the namespaces as ANA optimized. Only the worker changes which cntlr is the primary.

**standby** — A cntlr that serves no IO. It stays connected to the legs so that it can take over fast, and it exports the namespaces as ANA inaccessible.

**slice** — One of the parts a storage pool is split into. Each slice is one dm thin pool on the primary, and every thin device is striped across all slices of its pool. The number of slices is fixed when the pool is created.

**group** — A piece of the disk space of a slice. A meta group holds the metadata of the slice's thin pool and a data group holds its data; growing a slice adds groups. A group is either one leg, with no redundancy, or an md RAID1 array over its legs.

**leg** — One copy of the data of a group. It is stored on one disk node, or on two while a migration moves it, and the cntlrs of the pool reach it over NVMe-oF.

**side** — The part of a leg that lives on a disk node: its extents and, once the side is provisioned, its NVMe-oF exports, one for each cntlr of the pool. A leg has one side, and two while a migration moves it.

**spare leg** — A leg that is connected and health-checked but is not a member of the RAID1 array. It takes the place of a leg that fails.

## Volumes

**thin device, td** — A volume of a storage pool. It is one dm-thin volume in every slice, joined on the primary by one dm striped device (raid0).

**snapshot** — A thin device made from another thin device, which is its origin.

**subsystem** — An NVMe-oF subsystem that a storage pool shows to hosts; a pool can have several. The cntlrs of the pool export it under the same NQN, and it lists the hosts that may connect.

**namespace** — A volume as a host sees it: one namespace of a subsystem, backed by one thin device.

## How a change reaches a node

**revision** — A counter kept for every disk node, controller node and storage pool. Every change of its desired state that an agent must see raises it by one. The worker sends a syncup when it sees a revision move, and an agent refuses a syncup whose revision is lower than the one it holds.

**syncup** — The call in which the worker sends an agent the desired state of one disk node, side, controller node or cntlr. It always carries the whole desired state, never only the change. A change of the desired state of a storage pool is sent as a syncup to the sides and the cntlrs of the pool.

**converge** — What an agent does with a desired state: it reads what the node has now, builds what is missing and removes what is not wanted. A converge that runs again changes only what still differs.

**sweep** — The removing part of a converge. The agent lists what the node really has, and removes each of its own devices, exports and connections that the desired state does not name.

**leftover** — Something a node still has although the desired state does not want it, because a sweep has not removed it yet. The agent reports it, and the worker sends the syncup again until it is gone.

**check round** — The question the worker asks an agent at a fixed interval about one disk node, side, controller node or cntlr. The agent answers with the revision it holds, whether the node is clean, and the real state when it changed. From the answers the worker decides what is healthy and whether to send the syncup again.

## Health and reactions

**health epoch** — The time at which the worker first saw a node, a cntlr, a leg or a side unhealthy, written `err_epoch` in the records. It is zero while that node, cntlr, leg or side is healthy.

**event thresholds** — How long a cntlr, a side or a leg must stay unhealthy before the worker reacts. A storage pool has one for its primary, one for any cntlr, one for a side and one for a leg.

**reaction** — Something the worker does to a storage pool on its own, without an operator: failover, auto-grow, cntlr replacement or leg repair. A reaction never deletes user data.

**failover** — The reaction that makes a healthy standby the primary when the primary is disabled or has been unhealthy for too long. The old primary becomes a standby.

**demotion** — The syncup that tells the old primary it is a standby. Its namespaces move to ANA inaccessible, its arrays stop, and its legs stay connected.

**fence** — The step where a disk node cuts the old primary's path to a side by reloading it onto an error target, so that the old primary can no longer read or write the leg. A failover fences the old primary on every side of the pool.

**hold** — The worker's wait before it sends a pool's syncups to the cntlrs: first until the old primary reports its demotion applied, then until the sides report the new revision applied, each bounded in time.

**cntlr replacement** — The reaction that deletes a cntlr that has been unhealthy for too long and creates a new one on another controller node.

**leg repair** — The reaction for a leg of a RAID1 group that has been unhealthy for too long: the worker puts a ready spare leg of the group in its place in the array. A spare is ready when it is provisioned and the primary reports it healthy. When the group has no spare that is ready or still getting ready, the worker first creates one on another disk node, if the group has room for one more spare, and waits until it is ready. The replaced leg stays in the group as a spare.

**auto-grow** — The reaction that adds a group to a slice when the thin pool of the slice is filling up.

**disabled** — A flag an operator sets on a node or on a cntlr. A disabled node gets no new allocations, and what it already holds keeps running. A disabled cntlr serves no IO and is never made the primary, and the cdc does not show it to hosts.

**level** — A setting of a storage pool, with steps from read-write down to fully switched off. Each step switches off more of the pool, so that an operator can take a damaged pool apart in stages.

## Creating, moving and deleting

**provisioned, zeroing** — A disk node fills a new side with zeros before it exports the side; this is zeroing. When it is complete, the worker marks the side provisioned. The controller nodes do not use a leg before a side of it is provisioned.

**migration** — The move of one leg from one disk node to another while the leg stays in use. An operator starts it: a new side is created on the other disk node and pulls the data from the old side. The operator finishes it when the copy is complete, and the old side is removed.

**clone** — A copy of an outside NVMe-oF namespace into a local thin device. The primary pulls the data with dm-clone.

**transfer** — The other end of a clone: a storage pool exports one of its volumes over a subsystem made for this purpose, so that a clone in another pool can read it.

**cross-SP live migration** — The move of a volume from one storage pool to another, even in another cluster, while hosts keep using it. It is a transfer in the source pool together with a clone in the destination pool.

**drain** — The removal of a storage pool or of a clone in the background. The delete call marks the pool or the clone as deleting, and the worker then removes its parts step by step.

## Tests and logs

**suite** — An integration test script that drives the real programs on lab VMs. There is one for each of: the dn agent, the cn agent, the worker, the gateway, dnvctl, the cdc, and the whole system end to end (e2e).

**case** — One scenario of a suite. A suite runs its cases in a fixed order and stops at the first failure.

**stage** — One step of a case.

**trace id** — The id that joins the log lines of one call across the programs the call passes through. Work that the worker does afterwards for that call runs under ids of the worker's own.
