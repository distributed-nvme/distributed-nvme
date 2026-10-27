# dnv on one machine

This tutorial runs a whole dnv system on one Linux machine and ends with a
50 GiB volume on that same machine, connected through nvme-stas. Every
command is complete: copy it, paste it, run it. Nothing is kept in shell
variables. The words are those of `doc/core_glossary.md`.

## What you build

One machine runs:

- the control plane: etcd, `dnv-gateway` (the API server), `dnv-worker`
  (the program that makes the nodes follow etcd) and `dnv-cdc` (the
  discovery service a host asks where its subsystems are);
- four dn agents, each a disk node over its own 10 GiB loop device;
- one cn agent, the controller node that runs the volume logic and exports
  the volume over NVMe/TCP;
- the host: nvme-stas on the same machine connects to the volume.

With `dnvctl` you create a cluster, the four disk nodes, the controller
node, a storage pool with RAID1 and one slice, a thin device of 50 GiB, a
subsystem and one namespace in it, and you let the host in.

Why four disk nodes: a storage pool create puts every leg of the pool on a
different disk node. A RAID1 pool with one slice has a meta group and a
data group, each with two legs, so it needs four disk nodes. Two disk
nodes would only hold a pool without redundancy.

Everything listens on 127.0.0.1:

| program | listens on | files in the working directory |
|---|---|---|
| etcd | 2379 for clients, 2380 for peers | `etcd-data/`, `etcd.log`, `etcd.pid` |
| dnv-gateway | 29527 (gRPC) | `gateway.log`, `gateway.pid` |
| dnv-worker | nothing | `worker.log`, `worker.pid` |
| dnv-cdc | 8009 (NVMe/TCP discovery) | `cdc.log`, `cdc.pid` |
| dn agents dn0 to dn3 | gRPC 29900 to 29903; nvmet port ids 1 to 4 on NVMe/TCP ports 4301 to 4304 | `dn0.img` to `dn3.img`, `store-dn0/` to `store-dn3/`, `dn0.log` to `dn3.log`, `dn0.pid` to `dn3.pid` |
| cn agent cn0 | gRPC 29950; nvmet port id 5 on NVMe/TCP port 4420 | `store-cn0/`, `cn0.log`, `cn0.pid` |

The fixed names and identities:

| what | value |
|---|---|
| cluster | `c0` |
| storage pool | `sp0` |
| thin device | `td0`, 53687091200 bytes (50 GiB) |
| subsystem | `nqn.2026-10.com.example:sp0-ss0` |
| namespace | NSID 1, uuid `11111111-2222-4333-8444-555555555555` |
| host NQN | `nqn.2014-08.org.nvmexpress:uuid:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee` |
| host id | `aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee` |

How the commands are written:

- All commands run in one terminal, in the directory that holds the
  binaries `etcd`, `dnv-gateway`, `dnv-worker`, `dnv-cdc`, `dnv-agent` and
  `dnvctl`. Change into that directory once; every file the tutorial makes
  lands there.
- Commands that touch the kernel run with `sudo`. The control plane and
  `dnvctl` run as you.
- A long-running program goes to the background with a log file and a pid
  file, the file that holds its process id, which the teardown uses to stop
  it.
- Every `dnvctl` command names the gateway and the cluster, and the pool
  where it acts on one: `--gateway-address 127.0.0.1:29527 --cluster c0
  --sp sp0`. `dnvctl` prints one JSON document per command; the checks
  below pick the field to look at with `grep`.
- The tutorial changes the machine: it installs packages, loads kernel
  modules, writes a udev rule, masks a systemd unit and replaces the
  nvme-stas settings. Use a test machine. The teardown at the end puts
  everything back.

## Prerequisites

The tools, on Ubuntu or Debian:

```
sudo apt-get install -y nvme-cli mdadm nvme-stas dmsetup
```

On another distribution install the same tools: `nvme` from nvme-cli,
`mdadm`, nvme-stas with its two daemons `stafd` and `stacd`, `dmsetup`,
and `losetup` from util-linux. nvme-stas must be a 2.x version:

```
sudo stafd --version
```

Kernel modules and configfs. The agents load nothing themselves. The last
line must print `Y`: the kernel's native NVMe multipath is what joins a
host's paths into one device and carries the ANA state.

```
sudo modprobe -a nvmet nvmet-tcp nvme-tcp nvme-fabrics loop dm-thin-pool raid1
mountpoint -q /sys/kernel/config || sudo mount -t configfs none /sys/kernel/config
cat /sys/module/nvme_core/parameters/multipath
```

The md mask. The controller node assembles its RAID1 arrays itself. The
system's own udev rule would race it for an array it is creating, and on
this one kernel it would also assemble half arrays from the superblocks
that the disk nodes' devices carry. The rule below marks every dnv array
as not ready, which keeps the system's rule off it.

```
sudo tee /etc/udev/rules.d/63-dnv-md.rules >/dev/null <<'EOF'
ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
  IMPORT{program}="/sbin/mdadm --examine --export $devnode"
ENV{MD_NAME}=="dnv-*|*:dnv-*", ENV{SYSTEMD_READY}="0"
EOF
sudo udevadm control --reload
```

One connector. The kernel ships a unit that connects to every subsystem a
discovery change announces. nvme-stas is the only connector in this
tutorial, so mask that unit:

```
sudo systemctl mask nvmf-connect@.service nvmf-connect.target
```

## Disks

Four sparse files of 10 GiB, one loop device each. A sparse file takes
disk space only where something is written: a disk node zeroes a whole
meta side and the first blocks of a data side, and the rest stays unwritten
until a host writes.

```
truncate -s 10G dn0.img dn1.img dn2.img dn3.img
sudo losetup --find --show dn0.img
sudo losetup --find --show dn1.img
sudo losetup --find --show dn2.img
sudo losetup --find --show dn3.img
```

Each line prints the loop device it attached, `/dev/loop0` and so on. The
numbers differ from machine to machine, so later commands look a device up
from its file name with `losetup -j`, which prints the device, a colon and
the file.

## Control plane

etcd, with the two request caps dnv needs and automatic compaction of its
history:

```
nohup ./etcd --name dnv --data-dir ./etcd-data \
  --listen-client-urls http://127.0.0.1:2379 --advertise-client-urls http://127.0.0.1:2379 \
  --listen-peer-urls http://127.0.0.1:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 \
  --initial-cluster dnv=http://127.0.0.1:2380 \
  --max-txn-ops=1024 --max-request-bytes=33554432 --auto-compaction-retention=1h \
  > etcd.log 2>&1 & echo $! > etcd.pid
```

The gateway, the worker and the cdc. The worker waits a grace window after
it registers, so that workers starting together share the work without a
switch; alone, the wait only delays, so the tutorial shortens it.

```
nohup ./dnv-gateway --grpc-address 127.0.0.1:29527 --etcd-endpoints 127.0.0.1:2379 \
  > gateway.log 2>&1 & echo $! > gateway.pid
nohup ./dnv-worker --etcd-endpoints 127.0.0.1:2379 --vote-interval 2 --vote-grace-time 6 \
  > worker.log 2>&1 & echo $! > worker.pid
nohup ./dnv-cdc --etcd-endpoints 127.0.0.1:2379 --tr-addr 127.0.0.1 --tr-svc-id 8009 \
  > cdc.log 2>&1 & echo $! > cdc.pid
```

Check: the gateway answers through etcd with an empty cluster list, and
the cdc has started.

```
./dnvctl --gateway-address 127.0.0.1:29527 cluster list
grep -c '"cdc starting"' cdc.log
```

The first prints `{"cluster_name":[],"page_token":""}`; if it says
UNAVAILABLE instead, the gateway is still starting, so run it again a
second later. The second prints `1`.

## Agents

An agent keeps its local state under a directory of its own, which must
exist before it starts. The agents run as root; `sudo -v` asks for your
password once, so that the background starts below need no prompt.

```
mkdir -p store-dn0 store-dn1 store-dn2 store-dn3 store-cn0
sudo -v
```

Four dn agents, each with its own gRPC address, nvmet port id, NVMe/TCP
port, store and disk:

```
sudo nohup ./dnv-agent dn --grpc-address 127.0.0.1:29900 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 127.0.0.1 --tr-svc-id 4301 --nvmet-port-id 1 \
  --local-store ./store-dn0 --disk $(sudo losetup -j dn0.img | cut -d: -f1) \
  > dn0.log 2>&1 & echo $! > dn0.pid
sudo nohup ./dnv-agent dn --grpc-address 127.0.0.1:29901 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 127.0.0.1 --tr-svc-id 4302 --nvmet-port-id 2 \
  --local-store ./store-dn1 --disk $(sudo losetup -j dn1.img | cut -d: -f1) \
  > dn1.log 2>&1 & echo $! > dn1.pid
sudo nohup ./dnv-agent dn --grpc-address 127.0.0.1:29902 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 127.0.0.1 --tr-svc-id 4303 --nvmet-port-id 3 \
  --local-store ./store-dn2 --disk $(sudo losetup -j dn2.img | cut -d: -f1) \
  > dn2.log 2>&1 & echo $! > dn2.pid
sudo nohup ./dnv-agent dn --grpc-address 127.0.0.1:29903 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 127.0.0.1 --tr-svc-id 4304 --nvmet-port-id 4 \
  --local-store ./store-dn3 --disk $(sudo losetup -j dn3.img | cut -d: -f1) \
  > dn3.log 2>&1 & echo $! > dn3.pid
```

The cn agent. Its NVMe/TCP port differs from the disk nodes', so it takes
an nvmet port id of its own:

```
sudo nohup ./dnv-agent cn --grpc-address 127.0.0.1:29950 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 127.0.0.1 --tr-svc-id 4420 --nvmet-port-id 5 \
  --local-store ./store-cn0 \
  > cn0.log 2>&1 & echo $! > cn0.pid
```

Check: five agents serve, a second after the starts.

```
grep -l '"agent serving"' dn0.log dn1.log dn2.log dn3.log cn0.log
```

It prints the five file names.

## Cluster and nodes

The cluster, at the default extent size of 1 GiB:

```
./dnvctl --gateway-address 127.0.0.1:29527 cluster create --name c0
```

The four disk nodes and the controller node. A node's name is the gRPC
address of its agent, and the transport values repeat the agent's flags.
The gateway asks the agent for its size at once, so the agent must be
running. A node's location defaults to its address, so the four disk nodes
are four failure domains, which the RAID1 placement needs.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn create --addr 127.0.0.1:29900 --tr-addr 127.0.0.1 --tr-svc-id 4301
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn create --addr 127.0.0.1:29901 --tr-addr 127.0.0.1 --tr-svc-id 4302
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn create --addr 127.0.0.1:29902 --tr-addr 127.0.0.1 --tr-svc-id 4303
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn create --addr 127.0.0.1:29903 --tr-addr 127.0.0.1 --tr-svc-id 4304
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 cn create --addr 127.0.0.1:29950 --tr-addr 127.0.0.1 --tr-svc-id 4420
```

Check: a disk node has nine extents. A 10 GiB disk minus the 256 MiB the
disk node keeps for its header, its table and its metadata area holds nine
whole 1 GiB extents.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn get --addr 127.0.0.1:29900 | grep -o '"[a-z_]*ext_cnt":"[0-9]*"'
```

It prints `"free_ext_cnt":"9"` and `"total_ext_cnt":"9"`.

Check: the worker has made each node follow its record. The agent's own
report shows every row OK: on a disk node the disk format, the table and
the nvmet port, three rows; on the controller node the port, one row. The
worker reaches a new node within a few seconds, and until then the count
is 0. Repeat until the counts are 3, 3, 3, 3 and 1.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn inspect --addr 127.0.0.1:29900 | grep -o RES_STATUS_OK | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn inspect --addr 127.0.0.1:29901 | grep -o RES_STATUS_OK | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn inspect --addr 127.0.0.1:29902 | grep -o RES_STATUS_OK | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 dn inspect --addr 127.0.0.1:29903 | grep -o RES_STATUS_OK | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 cn inspect --addr 127.0.0.1:29950 | grep -o RES_STATUS_OK | wc -l
```

## Storage pool

RAID1, one slice, one cntlr, and a data group of two extents on each of
its legs. The meta group, which holds the thin pool's metadata, is always
one extent.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 sp create --redund raid1 --slice-cnt 1 --cntlr-cnt 1 --init-ext-cnt 2
```

What happens now: the worker sends each of the four disk nodes its side. A
disk node zeroes what must read as zeros, a whole meta side and the first
blocks of a data side, and exports the side over NVMe/TCP; the worker then
marks the side provisioned. The controller node holds the pool's one
cntlr, so it is the primary: it connects the four sides, assembles the two
RAID1 arrays and builds the slice's thin pool.

Check: four sides provisioned, one primary. Zeroing the two meta sides
takes some seconds; repeat until the counts are 4 and 1.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 sp get | grep -o '"provisioned":true' | wc -l
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 sp get | grep -o '"primary":true' | wc -l
```

The pool holds about 2 GiB of real data space. The 50 GiB of the thin
device below are virtual: a block a host never wrote reads as zeros and
takes no space. When the thin pool passes half of its space, the worker
adds a data group on its own (auto-grow), as long as two disk nodes have
free extents; when they are full, a write to a new block fails.

## Volume and export

The thin device:

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 td create --name td0 --size 53687091200
```

Check: the primary has created it. Repeat until it prints
`"created":true`.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 td list | grep -o '"created":[a-z]*'
```

The subsystem, with the host's NQN as its one allowed host, and the
namespace over the thin device with the fixed uuid:

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 ss create --nqn nqn.2026-10.com.example:sp0-ss0 --hosts nqn.2014-08.org.nvmexpress:uuid:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 ns create --nqn nqn.2026-10.com.example:sp0-ss0 --idx 1 --td td0 --uuid 11111111-2222-4333-8444-555555555555
```

Check: the subsystem lists the namespace.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 ss list | grep -o '"dev_uuid":"[^"]*"'
```

It prints `"dev_uuid":"11111111-2222-4333-8444-555555555555"`.

## Host

First a plain discovery read. The cdc shows a host exactly the subsystems
whose allowed hosts name it, so the read carries the host NQN and host id:

```
sudo nvme discover -t tcp -a 127.0.0.1 -s 8009 -q nqn.2014-08.org.nvmexpress:uuid:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee -I aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee
```

It prints one record: the subsystem's NQN at `127.0.0.1` port `4420`, the
controller node's port.

nvme-stas. Back up its three files, write the host identity, point its
discovery daemon at the cdc with a persistent connection, and let its
connector connect every subsystem the cdc lists and disconnect only what
it connected itself. The last setting matters here: the controller node's
own connections to the disk nodes live on this same kernel.

```
sudo cp /etc/stas/stafd.conf /etc/stas/stafd.conf.dnv-bak
sudo cp /etc/stas/stacd.conf /etc/stas/stacd.conf.dnv-bak
test -f /etc/stas/sys.conf && sudo cp /etc/stas/sys.conf /etc/stas/sys.conf.dnv-bak
sudo tee /etc/stas/sys.conf >/dev/null <<'EOF'
[Host]
nqn = nqn.2014-08.org.nvmexpress:uuid:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee
id = aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee
EOF
sudo tee /etc/stas/stafd.conf >/dev/null <<'EOF'
[Global]
tron=false
hdr-digest=false
data-digest=false
kato=10
ignore-iface=false
ip-family=ipv4
pleo=disabled

[Service Discovery]
zeroconf=disabled

[Discovery controller connection management]
persistent-connections=true

[Controllers]
controller = transport=tcp;traddr=127.0.0.1;trsvcid=8009
EOF
sudo tee /etc/stas/stacd.conf >/dev/null <<'EOF'
[Global]
tron=false
hdr-digest=false
data-digest=false
ip-family=ipv4

[I/O controller connection management]
disconnect-scope=only-stas-connections
disconnect-trtypes=tcp
connect-attempts-on-ncc=0

[Controllers]
EOF
sudo systemctl restart stafd stacd
```

Check: the discovery daemon holds its connection to the cdc, and the
connector holds one to the subsystem. The two need a few seconds; repeat
until each lists one controller.

```
sudo stafctl ls
sudo stacctl ls
```

The first lists a controller at `127.0.0.1` port `8009`, the second one at
port `4420` with the subsystem's NQN.

Check: the volume is a block device of 50 GiB.

```
sudo nvme list
ls -l /dev/disk/by-id/nvme-uuid.11111111-2222-4333-8444-555555555555
sudo blockdev --getsize64 /dev/disk/by-id/nvme-uuid.11111111-2222-4333-8444-555555555555
```

The list shows the volume, 53.69 GB, and four small namespaces beside it:
the controller node's own connections to the disk nodes' sides, which live
on this kernel too. The link points at the volume's device, and the size
is `53687091200`.

Check: IO goes through the whole stack. Write 4 MiB of random bytes, drop
the page cache so that the read goes to the device, and read them back.

```
head -c 4194304 /dev/urandom > sample.bin
sudo dd if=sample.bin of=/dev/disk/by-id/nvme-uuid.11111111-2222-4333-8444-555555555555 bs=1M conv=fsync
sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
sudo dd if=/dev/disk/by-id/nvme-uuid.11111111-2222-4333-8444-555555555555 bs=1M count=4 | sha256sum
sha256sum sample.bin
```

The two sums are the same.

## Teardown

In the order a holder goes before what it holds. The host first: stop
nvme-stas, which keeps its connections when it stops, so disconnect them
by hand.

```
sudo systemctl stop stacd stafd
sudo nvme disconnect -n nqn.2026-10.com.example:sp0-ss0
sudo nvme disconnect -n nqn.2014-08.org.nvmexpress.discovery
```

The objects, inner first: the namespace, the subsystem, the thin device,
then the pool. A pool delete returns at once and the worker takes the pool
apart in the background (the drain): it first takes the cntlr off the
controller node, whose agent then removes the namespace device, the thin
pool, the two arrays and its four connections to the sides, and it then
frees the disk nodes' extents, so that each disk node's agent removes its
side. The first loop waits until the pool's record is gone.

```
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 ns delete --nqn nqn.2026-10.com.example:sp0-ss0 --idx 1
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 ss delete --nqn nqn.2026-10.com.example:sp0-ss0
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 td delete --name td0
./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 sp delete
while ./dnvctl --gateway-address 127.0.0.1:29527 --cluster c0 --sp sp0 sp get >/dev/null 2>&1; do sleep 1; done
```

A side can go while the controller node is still taking its stack down;
its agent then finishes on a later pass, which the worker drives as long
as the controller node's record exists. So wait until the kernel holds
nothing of the pool before the nodes are deleted:

```
while sudo dmsetup ls | grep -q dnv-; do sleep 1; done
while sudo nvme list-subsys 2>/dev/null | grep -q dnv; do sleep 1; done
cat /proc/mdstat
```

`/proc/mdstat` lists no array.

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
entries, the tutorial host's and the controller node's. Each port's two
ANA groups go before the port:

```
for p in 1 2 3 4 5; do sudo rmdir /sys/kernel/config/nvmet/ports/$p/ana_groups/3 /sys/kernel/config/nvmet/ports/$p/ana_groups/2 /sys/kernel/config/nvmet/ports/$p; done
sudo rmdir /sys/kernel/config/nvmet/hosts/nqn.2014-08.org.nvmexpress:uuid:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee /sys/kernel/config/nvmet/hosts/nqn.2024-01.io.dnv:1:*
```

The loop devices and the files:

```
for f in dn0.img dn1.img dn2.img dn3.img; do sudo losetup -d $(sudo losetup -j $f | cut -d: -f1); done
sudo rm -rf store-dn0 store-dn1 store-dn2 store-dn3 store-cn0 etcd-data
rm -f dn0.img dn1.img dn2.img dn3.img sample.bin
rm -f etcd.log gateway.log worker.log cdc.log dn0.log dn1.log dn2.log dn3.log cn0.log
rm -f etcd.pid gateway.pid worker.pid cdc.pid dn0.pid dn1.pid dn2.pid dn3.pid cn0.pid
```

The udev rule, the unit and the nvme-stas files:

```
sudo rm -f /etc/udev/rules.d/63-dnv-md.rules
sudo udevadm control --reload
sudo systemctl unmask nvmf-connect@.service nvmf-connect.target
sudo mv /etc/stas/stafd.conf.dnv-bak /etc/stas/stafd.conf
sudo mv /etc/stas/stacd.conf.dnv-bak /etc/stas/stacd.conf
if test -f /etc/stas/sys.conf.dnv-bak; then sudo mv /etc/stas/sys.conf.dnv-bak /etc/stas/sys.conf; else sudo rm -f /etc/stas/sys.conf; fi
```

The machine is as it was, except for the packages installed at the start.
