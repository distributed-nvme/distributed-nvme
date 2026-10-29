# dnv under Kubernetes on one machine

This tutorial builds on [dnv on one machine](all_in_one.md): the same dnv
system on one Linux machine, then a one-node Kubernetes on the same
machine with the dnv CSI driver, and it ends with a claim of 8 GiB mounted
in a pod. Every command is complete: copy it, paste it, run it. Nothing is
kept in shell variables. The words are those of `doc/core_glossary.md`;
the few Kubernetes words are explained where they first appear.

## What you build

One machine runs:

- the dnv of the all-in-one tutorial: the control plane, four disk nodes
  and one controller node, with one change: the gateway listens on every
  address of the machine, not only on 127.0.0.1, because a pod reaches
  the machine through its address;
- k3s, a small Kubernetes in one program: the one node is the API server
  and the kubelet, the program that runs pods, at once;
- the dnv CSI driver, `dnv-csi`: a CSI driver is what Kubernetes calls to
  make, attach and mount volumes. Its controller plugin runs in one pod and
  talks to the gateway: a volume is one storage pool with one thin device,
  one subsystem and one namespace, and attaching it to a node puts the
  node's host NQN in the subsystem's allowed hosts. Its node plugin runs in
  one pod per node, on the machine's own network: it asks the cdc where the
  subsystem is, connects, formats and mounts;
- a storage class, the Kubernetes object that names the driver and the
  cluster, the redundancy and the cntlr count a volume gets;
- a claim (a persistent volume claim), the request for a volume of a size
  from a class, and a pod, the smallest thing Kubernetes runs, which mounts
  the claim at `/data`.

The fixed names:

| what | value |
|---|---|
| Kubernetes namespace of the driver | `dnv-csi` |
| storage class | `dnv`, cluster `c0`, RAID1, one cntlr |
| claim | `demo`, 8 GiB |
| pod | `demo`, image `busybox:1.37.0` |
| storage pool and thin device | the name Kubernetes gives the volume, `pvc-` and a uuid |
| subsystem | `nqn.2024-01.io.dnv.csi:` and that name |
| host NQN and host id | the machine's `/etc/nvme/hostnqn` and `/etc/nvme/hostid` |

How the commands are written:

- All commands run in one terminal, in the directory of the all-in-one
  tutorial, the one that holds the binaries. Before you start, copy
  `dnv-csi` from the build's `bin/` and the repository's `deploy/`
  directory, the image recipe and the driver's manifests, into it.
- Every Kubernetes command runs as `sudo k3s kubectl`: k3s keeps its
  credentials readable by root alone. Commands that touch the kernel run
  with `sudo`. The control plane and `dnvctl` run as you.
- The tutorial changes the machine: it installs k3s and docker besides
  what the all-in-one tutorial installs, and it starts Kubernetes pods that
  mount volumes. Use a test machine with 4 GiB of memory or more: the
  stack, k3s, the driver and the pod together stayed under 2 GiB on the
  machine this tutorial was checked on. The teardown at the end puts
  everything back.

## Prerequisites

The dnv system. Run the all-in-one tutorial's sections Prerequisites,
Disks, Control plane, Agents and Cluster and nodes, with one change: in
Control plane, start the gateway with this command in place of the one
written there, so that it listens on every address of the machine. Every
`dnvctl` command stays as written, since 127.0.0.1 is one of those
addresses.

```
nohup ./dnv-gateway --grpc-address 0.0.0.0:29527 --etcd-endpoints 127.0.0.1:2379 \
  > gateway.log 2>&1 & echo $! > gateway.pid
```

nvme-stas, which the all-in-one tutorial installs, is not used here: the
node plugin is the connector. Its two daemons stay idle, since nothing
points them at the cdc. The mask of the kernel's own connector unit from
the all-in-one tutorial's Prerequisites holds here too, for the same
reason: one connector.

The machine's identity as an NVMe host. The node plugin reads it from the
two files below and refuses to start without them; nvme-cli's package
usually writes them at install, so the two commands create a file only
when it is missing or empty.

```
sudo mkdir -p /etc/nvme
test -s /etc/nvme/hostnqn || sudo nvme gen-hostnqn | sudo tee /etc/nvme/hostnqn
test -s /etc/nvme/hostid || uuidgen | sudo tee /etc/nvme/hostid
cat /etc/nvme/hostnqn /etc/nvme/hostid
```

The first line printed is the host NQN, `nqn.2014-08.org.nvmexpress:uuid:`
and a uuid; the second is the host id, a uuid.

docker, to build the driver's image:

```
sudo apt-get install -y docker.io
```

k3s, pinned to the release the project's suite runs, without the three
parts this tutorial does not use: the ingress, the load balancer and the
metrics server. The install script writes the `k3s` binary and starts the
`k3s` service.

```
curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=v1.37.1+k3s1 sh -s - server --disable traefik --disable servicelb --disable metrics-server
```

Check: the one node is Ready. The server needs some seconds after the
install; repeat until the line says `Ready`.

```
sudo k3s kubectl get nodes
```

It prints one line with the machine's hostname, `Ready`, the role
`control-plane` and the version `v1.37.1+k3s1`.

## The image

The image holds the one binary and the tools the node plugin runs on the
node: nvme-cli, the mount tools, blkid and the two filesystem makers. The
build's context is a directory with the binary alone, so that nothing else
of the working directory, the disk files among it, is sent to the build.
k3s pulls nothing it already has, so the image goes straight into its
image store.

```
mkdir -p image && cp dnv-csi image/
sudo docker build -t dnv-csi:0.1.0 -f deploy/Dockerfile image
sudo docker save dnv-csi:0.1.0 | sudo k3s ctr images import -
```

Check: the image is in the store.

```
sudo k3s ctr images ls -q | grep dnv-csi
```

It prints `docker.io/library/dnv-csi:0.1.0`.

## The driver

The manifests under `deploy/` are applied as they are, except two: the
config map, which carries the addresses the controller plugin needs, and
the storage class. The controller plugin runs on the pod network, where
127.0.0.1 is the pod itself, so it reaches the gateway through the
machine's address; the node plugin runs on the machine's network, so it
reaches the cdc at 127.0.0.1, where the all-in-one tutorial started it.
The command below reads the machine's address off its default route.

```
sudo k3s kubectl apply -f deploy/namespace.yaml -f deploy/csidriver.yaml -f deploy/rbac.yaml
sed "s/192.0.2.10:29527/$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p'):29527/; s/192.0.2.10:8009/127.0.0.1:8009/" deploy/config.yaml | sudo k3s kubectl apply -f -
sudo k3s kubectl apply -f deploy/controller.yaml -f deploy/node.yaml
```

Check: the config map carries the two addresses.

```
sudo k3s kubectl -n dnv-csi get configmap dnv-csi-config -o jsonpath='{.data}{"\n"}'
```

It prints `{"cdcAddresses":"127.0.0.1:8009","gatewayAddress":"` and the
machine's address with `:29527"}`.

The storage class. The example names the cluster `default` and two cntlrs;
this machine's cluster is `c0` and has one controller node, so the class
asks for one cntlr.

```
sed 's/cluster: default/cluster: c0/; s/cntlrs: "2"/cntlrs: "1"/' deploy/storageclass.yaml | sudo k3s kubectl apply -f -
```

Check: the two plugins run. k3s pulls the sidecar images, the small
programs that run beside each plugin and talk to Kubernetes for it, which
takes a minute the first time; the two commands wait for it.

```
sudo k3s kubectl -n dnv-csi rollout status deploy/dnv-csi-controller --timeout=300s
sudo k3s kubectl -n dnv-csi rollout status ds/dnv-csi-node --timeout=300s
sudo k3s kubectl -n dnv-csi logs deploy/dnv-csi-controller -c dnv-csi | grep -c '"csi controller serving"'
sudo k3s kubectl -n dnv-csi logs ds/dnv-csi-node -c dnv-csi | grep -c '"csi node serving"'
```

The first two end with `successfully rolled out`, the last two print `1`.

Check: Kubernetes knows the free space of the class. The provisioner
sidecar asks the controller plugin, which reads every node's free extents
from the gateway; the first answer comes within a minute of the start, so
repeat until a row shows.

```
sudo k3s kubectl get csistoragecapacities -A -o custom-columns=CLASS:.storageClassName,CAPACITY:.capacity,LARGEST:.maximumVolumeSize
```

It prints one row: the class `dnv`, a capacity of `18Gi` and a largest
volume of `18Gi`. The four disk nodes hold 36 free extents of 1 GiB, and
RAID1 writes every byte twice, so 18 GiB can be promised; a volume can be
no larger than that.

## A volume

The claim:

```
sudo k3s kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: demo
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: dnv
  resources:
    requests:
      storage: 8Gi
EOF
```

What happens now: the provisioner sidecar asks the controller plugin for a
volume of 8 GiB. The plugin creates, in the cluster `c0`, a storage pool
named after the volume, RAID1 with one slice and one cntlr, whose data
group is one extent: one eighth of 8 GiB, rounded up to whole extents. In
it a thin device of 8 GiB, a subsystem with no allowed host yet, and a
namespace over the thin device. The worker then provisions the pool as in
the all-in-one tutorial. The volume is thin: it reserves an eighth of its
size, and auto-grow adds a data group of one extent whenever the pool
fills up, until the data groups cover the volume.

Check: the claim is bound, the pool exists, and the worker has provisioned
its four sides and made the cntlr the primary. Zeroing the meta sides takes
some seconds; repeat the last two until they print 4 and 1.

```
sudo k3s kubectl get pvc demo
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 sp list
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp $(sudo k3s kubectl get pvc demo -o jsonpath='{.spec.volumeName}') sp get | grep -o '"provisioned":true' | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp $(sudo k3s kubectl get pvc demo -o jsonpath='{.spec.volumeName}') sp get | grep -o '"primary":true' | wc -l
```

The first prints the claim as `Bound` to a volume named `pvc-` and a uuid,
`8Gi`, class `dnv`. The second prints `{"page_token":"","sp_name":["pvc-`
and that uuid `"]}`: the volume's name is the pool's name.

## A pod

The pod mounts the claim at `/data` and sleeps, so that the commands below
can run in it:

```
sudo k3s kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: demo
spec:
  containers:
    - name: app
      image: busybox:1.37.0
      command: ["sh", "-c", "sleep 1000000"]
      volumeMounts:
        - name: vol
          mountPath: /data
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: demo
EOF
```

What happens now: the attacher sidecar asks the controller plugin to attach
the volume to the node, and the plugin puts the node's host NQN in the
subsystem's allowed hosts. The kubelet then asks the node plugin to stage
the volume: the plugin connects to the cdc, reads in its discovery log
that the controller node at 127.0.0.1 port 4420 exports the subsystem,
connects to it as the machine's host, waits for the volume's device,
formats it with ext4, since it is blank, mounts it under the kubelet's
directory and bind-mounts it into the pod. The plugin keeps its discovery
connection and follows the log while the volume is staged, so that a
failover would move its path.

Check: the pod runs. The first run pulls the busybox image; the wait
covers it.

```
sudo k3s kubectl wait --for=condition=Ready pod/demo --timeout=300s
```

Check: the volume is a device of 8 GiB on this machine, mounted twice,
and the pod sees it at `/data`. The namespace uuid, which names the
device, is among the volume's attributes in Kubernetes.

```
sudo nvme list
sudo blockdev --getsize64 /dev/disk/by-id/nvme-uuid.$(sudo k3s kubectl get pv -o jsonpath='{.items[0].spec.csi.volumeAttributes.uuid}')
grep -c 'csi.dnv.io\|kubernetes.io~csi' /proc/mounts
sudo k3s kubectl exec demo -- df /data
```

The list shows the volume, 8.59 GB, and four small namespaces beside it:
the controller node's own connections to the disk nodes' sides, which live
on this kernel too. The size is `8589934592`. The count is `2`: the
plugin's own mount and the bind mount into the pod. `df` shows `/data` on
the volume's device.

Check: IO goes through the whole stack. Write 4 MiB of random bytes in the
pod and keep their sum, drop the machine's page cache so that the read goes
to the device, and read them back.

```
sudo k3s kubectl exec demo -- sh -c 'dd if=/dev/urandom of=/data/sample bs=1M count=4 conv=fsync && sha256sum /data/sample'
sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
sudo k3s kubectl exec demo -- sha256sum /data/sample
```

The two sums are the same.

Check: the data outlives the pod. Deleting the pod unmounts the volume,
drops the node's path and its discovery connection, since nothing is
staged any more, and takes the node out of the subsystem's allowed hosts;
a new pod attaches and stages it again, and finds the filesystem it
carries.

```
sudo k3s kubectl delete pod demo
while sudo nvme list-subsys 2>/dev/null | grep -q io.dnv.csi; do sleep 1; done
sudo nvme list-subsys
sudo k3s kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: demo
spec:
  containers:
    - name: app
      image: busybox:1.37.0
      command: ["sh", "-c", "sleep 1000000"]
      volumeMounts:
        - name: vol
          mountPath: /data
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: demo
EOF
sudo k3s kubectl wait --for=condition=Ready pod/demo --timeout=300s
sudo k3s kubectl exec demo -- sha256sum /data/sample
```

`nvme list-subsys` shows the controller node's four connections to the
sides and nothing of the volume. The sum is the same as before.

## Delete

The pod, then the claim. Deleting a claim of a class whose reclaim policy
is Delete asks the controller plugin to delete the volume: it removes the
namespace, the subsystem and the thin device, and deletes the pool, which
the worker takes apart in the background (the drain). The loop waits until
the pool's record is gone.

```
sudo k3s kubectl delete pod demo
sudo k3s kubectl delete pvc demo
while ./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 sp list | grep -q pvc-; do sleep 1; done
```

Check: Kubernetes holds no volume, and the kernel holds nothing of the
pool. The loops wait for the controller node's agent and the disk nodes'
agents to finish.

```
sudo k3s kubectl get pv
while sudo dmsetup ls | grep -q dnv-; do sleep 1; done
while sudo nvme list-subsys 2>/dev/null | grep -q dnv; do sleep 1; done
cat /proc/mdstat
```

The first prints `No resources found`; `/proc/mdstat` lists no array.

## Teardown

In the order a holder goes before what it holds. The driver's pods first,
which stop with nothing staged, then k3s as a whole: its uninstall script
stops the service and removes its program, its state and its containers.
Then the image and its build directory.

```
sudo k3s kubectl delete -f deploy/node.yaml -f deploy/controller.yaml
sudo /usr/local/bin/k3s-uninstall.sh
sudo docker image rm dnv-csi:0.1.0 ubuntu:26.04
rm -rf image
```

The nodes and the cluster:

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 cn delete --addr 127.0.0.1:29950
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn delete --addr 127.0.0.1:29900
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn delete --addr 127.0.0.1:29901
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn delete --addr 127.0.0.1:29902
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn delete --addr 127.0.0.1:29903
./dnvctl --gateway-address 127.0.0.1:29527 cluster delete --name c0
```

The programs, agents first, then the control plane. Each loop waits until
the processes of the pid files are gone, since a program finishes its
current work before it exits, which can take some seconds.

```
sudo kill $(cat dn0.pid dn1.pid dn2.pid dn3.pid cn0.pid)
for f in dn0.pid dn1.pid dn2.pid dn3.pid cn0.pid; do while [ -e /proc/$(cat $f) ]; do sleep 1; done; done
kill $(cat cdc.pid worker.pid gateway.pid etcd.pid)
for f in cdc.pid worker.pid gateway.pid etcd.pid; do while [ -e /proc/$(cat $f) ]; do sleep 1; done; done
```

The nvmet ports the agents built outlive them, and so do the two host
entries, the machine's own and the controller node's. Each port's two ANA
groups go before the port:

```
for p in 1 2 3 4 5; do sudo rmdir /sys/kernel/config/nvmet/ports/$p/ana_groups/3 /sys/kernel/config/nvmet/ports/$p/ana_groups/2 /sys/kernel/config/nvmet/ports/$p; done
sudo rmdir /sys/kernel/config/nvmet/hosts/$(cat /etc/nvme/hostnqn) /sys/kernel/config/nvmet/hosts/nqn.2024-01.io.dnv:1:*
```

The loop devices and the files:

```
for f in dn0.img dn1.img dn2.img dn3.img; do sudo losetup -d $(sudo losetup -j $f | cut -d: -f1); done
sudo rm -rf store-dn0 store-dn1 store-dn2 store-dn3 store-cn0 etcd-data
rm -f dn0.img dn1.img dn2.img dn3.img
rm -f etcd.log gateway.log worker.log cdc.log dn0.log dn1.log dn2.log dn3.log cn0.log
rm -f etcd.pid gateway.pid worker.pid cdc.pid dn0.pid dn1.pid dn2.pid dn3.pid cn0.pid
```

The udev rule and the unit. The nvme-stas files were not changed here, so
there is nothing to put back.

```
sudo rm -f /etc/udev/rules.d/63-dnv-md.rules
sudo udevadm control --reload
sudo systemctl unmask nvmf-connect@.service nvmf-connect.target
```

The machine is as it was, except for the packages installed at the start,
docker among them, and the two identity files under `/etc/nvme`, which are
the machine's and stay.
