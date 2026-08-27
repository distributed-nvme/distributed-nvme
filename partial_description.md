# alloc DNs and CNs

## size to extent
All allocation from CN and DN are calculated by extent. By defualt the
ExtentSize is 1G. If a DN device has 1T size, it has 1024 extents If a CN can
handle up to 4T size, it has 4096 extents. We round down the extent counts,
10G+3M means 10 extents.

## Core logic of finding DN candidates
Given below configuration DnBin0Shift = 0 DnBIn1Shift = 4 DnBin2Shift = 8
DnBin3Shift = 12

level0 = 1 << DnBin0Shift = 1 level1 = 1 << DnBin1Shift = 16 level2 = 1 <<
DnBin2Shift = 256 level3 = 1 << DnBin3Shift = 4096

So we put the DNs to below bins: bin0: 1 <= DnConf.FreeExtCnt < 16 bin1: 16 <=
DnConf.FreeExtCnt < 256 bin2: 256 <= DnConf.FreeExtCnt < 4096 bin3: 4096 <=
DnConf.FreeExtCnt

If a DN has less than 1 extent, we don't put it to any bin.

When we want find DN candidates, the input has 2 parts:
1. CandExtCnt, the DN FreeExtCnt should equal or larger than CandExtCnt.
2. CandCnt, how many Candidates we want to find.

Assuming CandExtCnt=18, CandCnt=16.
0. Create an empty LocList and an empty DnList.
1. Check bin0, 18 is larger than 16, we ignore bin0.
2. Check bin1, 18 is in the range of 16 - 256, Go through all DNs from the
   largest FreeExtCnt to the smallest FreeExtCnt. For each DN:
   1. If the DN's FreeExtCnt < CandExtCnt, go to step 3.
   2. If the DN's location is already in the LocList, pick up next DN and repeat
      from 2.1.
   3. Put the DN to the DnList, put the DN's location to the LocList.
   4. If len(DnList) >= CandCnt, return the DnList.
   5. Pick up next DN and repeat from 2.1.
3. We check bin2, use the same policy as step 2, we either return the DnList or
   go to step 4.
4. We check bin3, use the same policy as step 2, we either return the DnList or
   go to step 5.
5. Return the DnList.

This policy tries to balance below to bad cases:
1. We have 1000 DNs, we alaways allocate sides from a single DN, once the DN has
   no capactiy, we go to the next one.
2. We have 1000 DNs, each DN capacity is 10 ExtCnt, each DN has 5 ExtCnt free
   space, I want to allocate 6 ExtCnt,  I can't find a DN so I get the out of
   capacity error.

## Use black/white list when finding DN candidates
Use similar policy as the "Core logic of finding DN candidates", but with two
additinoal input patameters:
* BlackList
* WhiteList

If WhiteList is empty, search from all DNs. If WhiteList is not empty, only find
DNs in this list. Id node_black_list is empty, ignore it. If BlackList is not
empty, exclude all DNs in this list from the returned DnList, even if the DN is
in the WhiteList.

## Core logic of finding CN candidates
There is no bin for CNs. We put all CNs in the same pool. Input:
1. CandExtCnt, the CN FreeExtCnt should equal or larger than CandExtCnt.
2. CandCnt, how many Candidates we want to find. Use the same policy as the step
2 in "Find DN candidates", return a CnList.

## Use black/white list when finding CN candidates
Use similar policy as the "Core logic of finding CN candidates", but with two
additinoal input patameters:
* BlackList
* WhiteList

The BlackList and WhiteList have the same meaning as the "Use black/white list
when finding DN candidates".

## Alloc DNs and CNs when creating a Storage Pool

### Alloc DNs
1. Init the BlackList and WhiteList according to the NodeSelector.
2. For each slice, We will create a meta group and a data group. Calculate the
   CandExtCnt for all the meta and data groups.
3. Check the RedundConf
   1. If it is RedundNone, each meta group and each data group has a single
      side. Set RequiredCnt = 1, DnCandCnt = 1 * DnBatchSize.
   2. If it is RedundMdRaid1, each meta group and each data group has 2 sides.
      Set RequiredCnt = 2, DnCandCnt = 2 * DnBatchSize.
4. Loop for each CandExtCnt of the meta and data groups.
   1. Provide the CandExtCnt, DnCandCnt, BlackList, WhiteList to the "Use
      black/white list when finding DN candidates"
   2. If the returned DnList length is less than RequiredCnt, return an out of
      capacity error.
   3. Pickup DNs randomly from the DnList. If RequiredCnt=1, pickup one, if
      RequiredCnt=2, pickup two. Add them to the BlackList.
   4. Repeat from the step 4.1 until we get DNs for all groups.

### Alloc CNs
1. Init the BlackList and WhiteList according to the NodeSelector.
2. Use the sum of the extent count of all groups as the CandExtCnt.
3. Set RequiredCnt = 1, CnCandCnt = 1 * CnBatchSize.
4. Repeated below steps CntlrCnt times.
   1. Provide the CandExtCnt, CnCandCnt, BlackList, WhiteList to the "Use
      black/white list when finding CN candidates".
   2. If the returend CnList length is less than 1, return an out of capacity
      error.
   3. Pickup a CN randomly from the CnList.
   4. Repeat from the step 4.1 until we get all CNs for all cntlrs.

## Allocate DNs when growing a slice
Similar as the "Alloc DNs and CNs when creating a Storage Pool", but only
allocate DNs for a single group.

## Allocate a DN when creating a migration
Similar as the "Alloc DNs and CNs when creating a Storage Pool", but only
allocate a single DN.

## Allocate a DN when ceating a spare leg
Similar as the "Alloc DNs and CNs when creating a Storage Pool", but only
allocate a single DN.

## Allocate a CN when creating a cntlr
Similar as the "Alloc DNs and CNs when creating a Storage Pool", but only
allocate a single CN.


# cntlid_slot
To make sure the nvme multipath works, in a single SP, all its cntlrs should
have different cntlid. We could control the cntlid range via below two files:
* /sys/kernel/config/nvmet/subsystems/{nvme_nqn}/attr_cntlid_max
* /sys/kernel/config/nvmet/subsystems/{nvme_nqn}/attr_cntlid_min

Given below variables:
```
	CnCntlidSlotBase   = 10000
	CnCntlidSlotStep   = 5000
	CnCntlidSlotCnt = 8

	DnCntlidSlotBase   = 10000
	DnCntlidSlotStep   = 5000
	DnCntlidSlotCnt = 8
```

We do the same thing for both CN and DN: Split the cntlid to 8 sets, each cntlid
set has 4096 cntlids. The 8 sets are:
* 10000 - 15000
* 15000 - 20000
* 20000 - 25000
* 25000 - 30000
* 30000 - 35000
* 35000 - 40000
* 40000 - 45000
* 45000 - 50000

When a DN export a side to CNs, it use the Side.cntlid_slot to make sure which
set it will use. A side will export to the CNs via different ports, but will use
the same cntlid_slot.

When a CN export a cntlr to a host, it use the Cntlr.cntlid_slot to make sure
which set it will use. A cntlr may export multiple subsystems, all of them on
that specific cntlr will use the same set.

nvem_id_set = 0 means we set attr_cntlid_min=10000 attr_cntlid_max=15000
nvem_id_set = 1 means we set attr_cntlid_min=15000 attr_cntlid_max=20000
nvem_id_set = 2 means we set attr_cntlid_min=20000 attr_cntlid_max=25000
nvem_id_set = 3 means we set attr_cntlid_min=25000 attr_cntlid_max=30000
nvem_id_set = 4 means we set attr_cntlid_min=30000 attr_cntlid_max=35000
nvem_id_set = 5 means we set attr_cntlid_min=35000 attr_cntlid_max=40000
nvem_id_set = 6 means we set attr_cntlid_min=40000 attr_cntlid_max=45000
nvem_id_set = 7 means we set attr_cntlid_min=45000 attr_cntlid_max=50000

When we use clone + transfer to live migrate data from one SP to another one, we
should make sure the cntlrs of the two SPs use different cntlid_slot. Each SP
has up to 4 cntlrs, we have 8 cntlid_slot, so the cntlid_slots are just enough.

When we use migration to live migrate data from one side to another, we should
also make sure the src side and the dst side use different cntlid_slots.

# Failover
A SP has mulitple cntlrs, only one is primary, all others are standby. A
failover means switch the primary from one cntlr to another. The failover
invovles 3 kind of components:
1. The old primary cntlr, we call it old_primary.
2. The new primary cntlr, we call it new_primary.
3. All sides below to the same SP.

## old_primary
The old_primary  receives a SyncupCntlr RPC, which says it is not a primary. The
revision of this RPC is larger than all its previous SyncupCntlr RPCs. Then the
old_primary should follow below steps:
1. Move all namespaces from the optimized ana group to the inaccessible ana group.
2. Wait until no inflight IOs on the dm-error device.
3. Reload the backend dm linear devices to the corresponding dm-error device.
4. Cleanup all resources that a priamry shouldn't have.

## new_primary
Once the new_primary receives a SyncupCntlr RPC, which says it is a primary. The
revision of this RPC is larger than all its previous SyncupCntlr RPCs. Then the
old_primary should follow below steps:
1. Make sure all groups are available.
2. Create thin pools, thin devices, raid0 devices.
3. Reload all namespaces' backend dm-linear devcies from dm-error to the raid0
   devices.
4. Move al namespaces from the inaccessible ana group to the optimized ana group.

About "Make sure all groups are available", it is a bit complex, below is an
example.

Assuing the SP has 2 slices, each slice has 1 meta group, 2 data groups. It uses
RedundMdRaid1, each group has two legs. So the primary should have these
resources:
* slice0: a dm-thinpool device
* slice0-meta: a dm-linear device
* slice0-meta-grp0: a md-raid1 device
* slice0-meta-grp0-leg0: a nvmeof device 
* slice0-meta-grp0-leg1: a nvmeof device
* slice0-data: a dm-linear device
* slice0-data-grp0: a md-raid1 device
* slice0-data-grp0-leg0: a nvmeof device
* slice0-data-grp0-leg1: a nvmeof device
* slice0-data-grp1: a md-raid1 device
* slice0-data-grp1-leg0: a nvmeof device
* slice0-data-grp1-leg1: a nvmeof device
* slice1: a dm-thinpool device
* slice1-meta: a dm-linear device
* slice1-meta-grp0: a md-raid1 device
* slice1-meta-grp0-leg0: a nvmeof device 
* slice1-meta-grp0-leg1: a nvmeof device
* slice1-data: a dm-linear device
* slice1-data-grp0: a md-raid1 device
* slice1-data-grp0-leg0: a nvmeof device
* slice1-data-grp0-leg1: a nvmeof device
* slice1-data-grp1: a md-raid1 device
* slice1-data-grp1-leg0: a nvmeof device
* slice1-data-grp1-leg1: a nvmeof device

"Make sure all groups are available" means below resoruces are created:
* slice0-meta-grp0
* slice0-data-grp0
* slice0-data-grp1
* slice1-meta-grp0
* slice1-data-grp0
* slice1-data-grp1

All these groups are md-raid1 devices. When we want to create a raid1 devcie, we
may have 3 cases.
1. Both the two legs are avaialble. We can create the raid1 device. We should
   run different commands in different sub-cases.
   1. Both two legs have no superblock. Use "mdadm --create" command to create
      the it.
   2. Only one leg as a superblock. Aussing leg0 has superblock, leg1 has no
      superblock. Use the "mdadm --assemble" to create the raid1 devcie with
      only leg0. Then use "mdadm --add" to add the leg1 to the raid1.
   3. Both leg0 and leg1 have superblocks. Use the "mdadm --assemble" to create
      the raid1, then run "mdadm --detail" to check the status, maybe one leg is
      not added to the raid1 because it has stale metadata, then run "mdadm
      --add" to add that leg again.
2. Only one leg is available. We may or may not create the raid1 device. The
   mdadm makes the decision. Use "mdadm --assemble" to create the raid1, do not
   use "--run" parameter.
   1. The "mdadm --assemble" command succeeds. The raid1 device is created.
   2. The "mdadm --assemble" command fails, the raid1 devcie is not created.
3. Both of the two legs are not available. We can not create the raid1 device.

So a group will be available in case 1 and and case 2.1.

Now we define the meaning of "a leg is avaialble": A leg is a nvmeof device,
most of the time, it is a single path device, with a single side. If we have a
transfer task, the leg might be a multipath device, it has a src_side and a
dst_side. In any case, "a leg is available" means the leg has an "optimized"
path. If a leg has a "non-optimized" path, it is not available, the side exports
a dm-error device via the "non-optimized" path, we can't use it. If there is a
transfer task, the two sides might be either "non-optimized" / "inaccessible" or
"optimzied" / "inaccessible". In any case, an "optimized" state means the leg is
available.

## sides
For each side, if it finds the primary is changed via the SyncupSide RPC, and
the RPC has the highest revision, it should follow below steps:
1. Change the old_primary from the optimized ana group to the non-optimized ana group.
2. Suspend the dm-linear device of the old_primary
3. Reload the dm-linear device of the new_primary, let it be on top of the
   logical volume.
4. Move the new_primary from the non-optimized ana group to the optimized ana group.
4. Sleep SideSwitchWait seconds
5. Reload the dm-linear device of the old_primary, let it be on top of the
   dm-error device.

# Migration
The process of starting a migration is similar as a failover. The leg will have
two sides, one is src_side, another is dst_side. From the cntlr perspective, the
two sides are nvmeof multipath. We perform below actions when we start a
migration.

## src_side
1. Move the namespaces to all cntlrs from the optimized/non-optimized ana group
   to inaccessible ana group.
2. Suspend the dm-linear devices of all cntlrs
3. Export the logical volume to the dst_side via nvmeof

## dst_side
1. Create the subsystems and namespaces to all cntlrs, use dm-error for all
   namespaces and set all ana state to inaccessible.
1. Create the DnMigrMetaName device from the DnMigrVgName volume group.
2. Connect the src_side via nvmeof, retry until succeeds
3. Create the dm-clone device, use the SP thin pool block size as the region
   size.
4. Reload the dm-linear device of the primary cntlr, let it be on top of the
   dm-clone device.
5. Set the subsystem to the primary as optimized, set other subsystems to
   non-optimized.

## bitmap
After starting the migration, the user can invoke the GetLegBitmap RPC. It
returns a bitmap, the bit 1 means the corresponding block is not written yet, so
we can skip that block in the dm-clone via the blkdiscard command. If the bitmap
is too large, use the start_block and block_cnt parameters to get a part of the
bitmap at one time. Then the user use the AppendMigrationBitmap RPC to add the
bitmap(s) to the migration. The dnv-worker will send the bitmap(s) to the dn via
the SyncupSide RPC. The dnv-worker should set SyncupSideRequest.partial=true to
send the bitmap(s) incrementally.


# transfer and clone
The source of the clone could be any nvmeof target. If the soruce is a
distributed-nvme transfer, we could perform data live migration. Below is an
example:
* We have a sp1, the sp1 as a ss, the ss has a ns.
* We create a sp2, the sp2 has the same ss and ns as the sp1, the two ss/ns
  could be aggregated as a nvme multipath device.
* Make sure the sp1 and sp2 have different cntlid_slots. If they have a same
  cntlid_slot, pickup one of the cntlr, perform below step to use a different
  cntlid_slot:
  * Run UpdateCntlrEnabled to disable the cntlr.
  * Run DeleteCntlr to delete the cntlrl.
  * Run CreateCntlr to create a new cntlr with a different cntlid_slot.
* Make sure the sp1 and sp2 cntlrs don't share the same CNs. If they do, use the
  UpdateCntlrEnabled/DeleteCntlr/CreateCntlr to move a cntlr to a new CN.
* Make sure Namespace.suspended = false on sp1
* Make sure Namespace.suspended = true on sp2
* Get cn_id list of the sp2 cntlrs CNs. Use the NameFmt.CnHostNqn function to
  get the corresponding host nqns.
* Invoke CreateTransfer against sp1, set auto_suspend = true.
* Invoke CreateClone against sp2, set auto_resume = true.
* With auto_suspend=true, the transfer on sp1 will perform below actions:
  1. Move the Namespace to the inaccessible ana group.
  2. Suspend the CnNsDevName.
  3. Create CnXferFinaName, which is on top of the CnRaid0Name.
  4. Export the CnXferFinaName via nvmeof according to the Transfer configuration.
* With auto_resume=true, the clone on sp2 will follow below order:
  1. Connect to the nvmeof targets according to Clone.src_tr_conf_list.
  2. Create the dm-clone meta device CnCloneMetaName.
  3. Create the dm-clone device CnCloneFinalName.
  4. Reload the CnNsDevName to be on top of the CnCloneFinalName, make sure it is resumed.
  5. Move the Namespace to the optimized ana group

DeleteTransfer: set Namespace.suspended to true.
DeleteClone: set Namespace.suspended to false.

## bitmap
The clone could also use bitmap to skip blocks in dm-clone. But it is more
complex. The src (xfer) and the dst (clone) are raid0 devices and have
underlying thin pools. Below is a description about how to calculate and use the
bitmap:

We use slice_cnt to indicate how many underling devices under the raid0. We use
stripe_size to indicate the stripe size (or chunk size) of the raid0. Each
underling device of the raid0 has a bitmap to indicate the blocks of this
underlying device are written. We use block_size to indicate the size of each
bit in the bitmap. We have below constraints:
```
slice_cnt: slice_cnt is an integer and 1 <= slice_cnt <= 16
stripe_size: i * 4KB, i is an integer and 1 <= i <= 256
block_size: j * 64KB, j is an integer and 1<= j <= 16384
block_size = k * stripe_size, k is an integer and k >= 1
```

E.g. here is a raid0 device: slice_cnt = 4, stripe_size = 16KB, block_size=1MB
We have below layout:
```
4KB 4KB 4KB 4KB
4KB 4KB 4KB 4KB
4KB 4KB 4KB 4KB
4KB 4KB 4KB 4KB
4KB 4KB 4KB 4KB
4KB 4KB 4KB 4KB
...
4KB 4KB 4KB 4KB
```
We wrote to the 1st 4KB, the 3rd 4KB, the 7th 4KB, the 8th KB. There are 4
bitmaps.
* In the first bitamp, the fist bit is 1, all other bits are zero.
* In the second bitmap, all bits are zoro.
* In the third bitmap, the first and the second bit are 1, all other bits are
  zero.
* In the forth bitmap, the second bit is 1, all other bits are zero.

Given two raid0 devices, they are A and B. We know the slice_cnt_A,
stripe_size_A, block_size_A, slice_cnt_B, stripe_size_B, block_size_B,
slice_cnt_A bitmaps for A and slice_cnt_B bitmaps for B. Assuming all bitmaps of
B are zero at first. The bitmaps of A have non-zero bits. We run a userspace
program to copy data from A to B. We want to avoid copying the zero data. So we
rely on the slice_cnt_A, stripe_size_A, block_size_A and the bitmaps of A to
know where we should read from A. And we use the slice_cnt_B, stripe_size_B,
block_size_B to know where to where we should write to B. The program can only
read/write against the top raid0 devices, it can't access the underlying
devices. It is OK to copy some zero data from A to B, we try to minimal the zero
data we copy. We must copy all written data from A to B. After we copy some data
to B, the corresponding bit of B's bitamps will be set to 1 automatically. The
program might be interrupted in the middle of the progress. When we restart the
program, it should rely on the bitmaps on B to understand the data has been
copied, and skip to copy all copied data. And we have below additional
constraints:
* If stripe_size_A > stripe_size_B, stripe_size_A = stripe_size_B * m, m is an
  integer and m > 1
* If stripe_size_A < stripe_size_B, stripe_size_B = stripe_size_A * m, m is an
  integer and m > 1
* The program each time only read exactly region_size data, region_size =
  min(stripe_size_A, stripe_size_B).

We could write a golang function, given below input:
```
slice_cnt_A, stripe_size_A, block_size_A, all bitmaps of A
slice_cnt_B, stripe_size_B, block_size_B, all bitmaps of B
```
The golang function output is a bitmap and the region_size. A bit 1 in the
output bitmap means we have copied the data and don't need to copy it again.
According to this bitmap and the region_size, the program we mentioned
previously could read data from A and write data to B. The size of the bitmap
equal to the smaller dev size of A and B.

# Components
All components use [viper](https://github.com/spf13/viper) to provide commpand
line parameters and configuration file, environment variables configurations.

## dnv-gateway
Provide the Gateway RPCs.
```shell
dnv-gateway --grpc-network tcp --grpc-address 192.168.0.20:29527 --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379
```

## dnv-worker
Monitor the etcd modifications, then invoke the
DiskNodeAgent/ControllerNodeAgent RPCs to apply them to the dnv-agents on disk
nodes and controller nodes.
```shell
dnv-worker --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379  --roles dn,cn,sp
```

The dnv-worker has 3 roles: dn, cn, sp. We use the dn role as an exmaple.

If a dnv-worke has the dn role. It register itself to etcd, and find all other
dnv-workers that have the dn role. There are 256 shard code. They use the same
hash algorithm to select the owner of each shard code. If there is a single
dnv-worker, it own all the 256 shard codes. If there are two dnv-workers, they
splite the shard codes to two sets, each dnv-worker onw a set of the shard
codes. If the third dnv-worker is added, the first two dnv-workers move about
1/3 of their shard codes to the third dnv-worker. So each one has roughly the
same count of shard codes.

If a dnv-worker own 4 shard codes: 0x00, 0x08, 0x1f, 0xe5. It listen on 4 etcd
key prefixs, they are:
* {dnv_prefix} dn_rev 00
* {dnv_prefix} dn_rev 08
* {dnv_prefix} dn_rev 1f
* {dnv_prefix} dn_rev e5

If the dnv-worker finds a dn is updated, it invokes the DiskNodeAgent.SyncupDn
RPC against the dn. If the dnv-worker has a cn role and finds a cn is updated,
it invokes the ControllerNodeAgent.SyncupCnRequest RPC against the cn. If the
dnv-worker finds a sp is updated, it invokes the DiskNodeAgent.SyncupSide to all
the sides of that sp, and also invokes ControllerNodeAgent.SyncupCntlr to all
the cntlrs of that sp.

For failover, clone, transfer, migration, if possible, the dnv-worker should use
the `partial` parameters in the DiskNodeAgent/ControllerNodeAgent RPCs to make
them faster.

## dnv-agent
Run on disk nodes or controller nodes, provide the DiskNodeAgent or ControllerNodeAgent RPCs.

```shell
dnv-agent dn --grpc-network tcp --grpc-address 192.168.0.20:29528 --tr-type tcp --adr-fam ipv4 --tr-addr 192.168.0.20 --tr-svc-id 4200 --local-store /path/to/sqlite.db --disk /dev/disk/by-uuid/4425c6a8-dc27-40a3-9fd5-0cc41f534360
dnv-agent cn --grpc-network tcp --grpc-address 192.168.0.20:29529 --tr-type tcp --adr-fam ipv4 --tr-addr 192.168.0.20 --tr-svc-id 4200 --local-store /path/to/sqlite.db
```

The dnv-agent stores the latest revision and the corresponding configurations to
a local sqlite database. When it starts, it loads the revision and
configurations and applies to the current system. It rejects all RPCs that have
lower revisions.

When the dnv-agent recevies a SyncupDn request, it compare the side_pointer_list
with the current list it has.
* If a SidePointer is in the SyncupDnRequest but not in its current list, add
  the SidePointer to the current list.
* If a SidePointer is not in the SyncupDnRequest but in the current list, remove
  it from the current list, add it to a to_be_deleted list, and will delete all
  its resources later. All ids in a cluster are incrementally, so we assume a
  side won't be create again if it is deleted.

When the dnv-agent receives a SyncupSideRequest request, it check the curent side_pointer_list, if no such side_pointer, it returns an error, if has such side_pointer, it creates the resources accordingly.

The "dnv-agent cn" use similar logics for the SyncupCn and the SyncupCntlr RPCs.

All these "Syncup*" RPCs have a `partial` parameter. If partial=true, don't delete any resources doesn't in the request, only add new resources. The dnv-agent only accepts partial=true in two cases:
* the current revision is exactly smaller 1 than the revision in the "Syncup*" RPC
* the current revision is exactly same as the revision in the "Syncup*" RPC
In all other cases, the dnv-gent should reject the request.

## dnv-cdc

```shell
dnv-cdc --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379 --range 0,1,2,3,4,5,6,7
dnv-cdc --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379 --range 8,9,a,b,c,d,e,f
```

Provide nvmeof cdc to hosts.
The range 0 means listen on below keys:
```
{dnv_prefix} cdc {cluster_id} 00
{dnv_prefix} cdc {cluster_id} 01
...
{dnv_prefix} cdc {cluster_id} 0f
```

The range 1 means listen on below keys:
```
{dnv_prefix} cdc {cluster_id} 10
{dnv_prefix} cdc {cluster_id} 11
...
{dnv_prefix} cdc {cluster_id} 1f
```

## dnvctl
dnvctl dn create
dnvctl cn create
dnvctl sp create
dnvctl vol create
dnvctl admin update_etcd
dnvctl admin move_dn
dnvctl admin move_cn
dnvctl admin clone
## dnvmon
