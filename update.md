# alloc DNs and CNs

## size to extent
All allocation from CN and DN are calculated by extent.
By defualt the ExtentSize is 1G.
If a DN device has 1T size, it has 1024 extents
If a CN can handle up to 4T size, it has 4096 extents.
We round down the extent counts, 10G+3M means 10 extents.

## Core logic of finding DN candidates
Given below configuration
DnBin0Shift = 0
DnBIn1Shift = 4
DnBin2Shift = 8
DnBin3Shift = 12

level0 = 1 << DnBin0Shift = 1
level1 = 1 << DnBin1Shift = 16
level2 = 1 << DnBin2Shift = 256
level3 = 1 << DnBin3Shift = 4096

So we put the DNs to below bins:
bin0: 1 <= DnConf.FreeExtCnt < 16
bin1: 16 <= DnConf.FreeExtCnt < 256
bin2: 256 <= DnConf.FreeExtCnt < 4096
bin3: 4096 <= DnConf.FreeExtCnt

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
2. CandCnt, how many Candidates we want to find.
Use the same policy as the step 2 in "Find DN candidates", return a CnList.

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

# nvmet port bitmap
## CN port bitmap
Each CN have multiple cntlrs. The cntlrs belong to different SPs. The CN export
these cntlrs via nvmeof. Each cntlr has its own port. E.g. if there are 20
cntlrs, they may use:
```
/sys/kernel/config/nvmet/ports/20000
/sys/kernel/config/nvmet/ports/20001
/sys/kernel/config/nvmet/ports/20002
/sys/kernel/config/nvmet/ports/20003
...
/sys/kernel/config/nvmet/ports/20018
/sys/kernel/config/nvmet/ports/20019
```

When the second and the third cntlr are deleted, the in used ports are:
```
/sys/kernel/config/nvmet/ports/20000
/sys/kernel/config/nvmet/ports/20003
...
/sys/kernel/config/nvmet/ports/20018
/sys/kernel/config/nvmet/ports/20019
```

When we create a new cntlr, it will check from the lowest port number, find the
first avaiable port, so when a new cntlr is created, the in used ports are:
```
/sys/kernel/config/nvmet/ports/20000
/sys/kernel/config/nvmet/ports/20001
/sys/kernel/config/nvmet/ports/20003
...
/sys/kernel/config/nvmet/ports/20018
/sys/kernel/config/nvmet/ports/20019
```

If a bit in CnConf.port_bitmap is 1, the corresponding port is in use. When we
create a new cntlr, we search the CnConf.port_bitmap, find the first 0, set it
to 1, then set the offset of that bit to Cntlr.port_offset.

Continue the above example, we will create another new cntlr on that CN. We find
the 20000, 20001, are in used, 20002 is empty. So we set the third bit to 1 in
the CnConf.port_bitmap, then set Cntlr.port_offset=2. Then the in used ports
are:
```
/sys/kernel/config/nvmet/ports/20000
/sys/kernel/config/nvmet/ports/20001
/sys/kernel/config/nvmet/ports/20002
/sys/kernel/config/nvmet/ports/20003
...
/sys/kernel/config/nvmet/ports/20018
/sys/kernel/config/nvmet/ports/20019
```

When we delete the cntlr, we find the Cntlr.port_offset=2, so we clear the third
bit from the CnConf.port_bitmap.

The CnConf.nvme_tr_conf.tr_svc_id is the first port number. In the above case,
the tr_svc_id=20000.

## DN port bitmap
The DN port bitmap works in a similar with as the CN port bitmap. The only difference is that 1 bit in the bitmap indicate multiple ports. Given MaxCntlrCntPerSp=4, 1 bit in the DnConf.port_bimtap indicates 4 + 1 = 5 ports.
On a DN, a side exports to all the cntlrs of its SP, each one use a different port.
E.g.:
* Dn.nvme_tr_conf.tr_svc_id = 10000.
* Side.port_offset = 12.
So the side uses ports from 10000 + 12 * 5 to 10000 + 12 * 5 + 4 -> 100060 - 10064.
If the SP has two cntlrs, The side will use the ports according to the Cntlr.cntlr_idx.
Assuming there are two cntlrs, their cntlr_idx are 1 and 3:
* cntlr0: Cntlr.cntlr_idx = 1
* cntlr1: Cntlr.cntlr_idx = 3
The side will use port 10061 and 10063 for the two cntlrs.
Assuing the cntlr0 is on CN0, the cntlr1 is on CN1, the CN0 should connect to 10061, the CN1 should connect to 10063. When we create a new cntlr for a SP, we will make sure they have different cntlr_idx and the max cntlr_idx should be less than MaxCntlrCntPerSp.

The last port (10064) will be used for the migration task of that side. A side should have  only a single migration task.

# nvme_id_set
To make sure the nvme multipath works, in a single SP, all its cntlrs should have different cntlid. We could control the cntlid range via below two files:
* /sys/kernel/config/nvmet/subsystems/{nvme_nqn}/attr_cntlid_max
* /sys/kernel/config/nvmet/subsystems/{nvme_nqn}/attr_cntlid_min

Given below variables:
```
	CnNvmeIdBase   = 10000
	CnNvmeIdStep   = 5000
	CnNvmeIdSetCnt = 8

	DnNvmeIdBase   = 10000
	DnNvmeIdStep   = 5000
	DnNvmeIdSetCnt = 8
```

We do the same thing for both CN and DN: Split the cntlid to 8 sets, each cntlid set has 4096 cntlids. The 8 sets are:
* 10000 - 15000
* 15000 - 20000
* 20000 - 25000
* 25000 - 30000
* 30000 - 35000
* 35000 - 40000
* 40000 - 45000
* 45000 - 50000

When a DN export a side to CNs, it use the Side.nvme_id_set to make sure which set it will use. A side will export to the CNs via different ports, but will use the same nvme_id_set.

When a CN export a cntlr to a host, it use the Cntlr.nvme_id_set to make sure which set it will use. A cntlr may export multiple subsystems, all of them on that specific cntlr will use the same set.

nvem_id_set = 0 means we set attr_cntlid_min=10000 attr_cntlid_max=15000
nvem_id_set = 1 means we set attr_cntlid_min=15000 attr_cntlid_max=20000
nvem_id_set = 2 means we set attr_cntlid_min=20000 attr_cntlid_max=25000
nvem_id_set = 3 means we set attr_cntlid_min=25000 attr_cntlid_max=30000
nvem_id_set = 4 means we set attr_cntlid_min=30000 attr_cntlid_max=35000
nvem_id_set = 5 means we set attr_cntlid_min=35000 attr_cntlid_max=40000
nvem_id_set = 6 means we set attr_cntlid_min=40000 attr_cntlid_max=45000
nvem_id_set = 7 means we set attr_cntlid_min=45000 attr_cntlid_max=50000

When we use clone + transfer to live migrate data from one SP to another one, we should make sure the cntlrs of the two SPs use different nvme_id_set. Each SP has up to 4 cntlrs, we have 8 nvme_id_sets, so the nvme_id_sets are just enough.

When we use migration to live migrate data from one side to another, we should also make sure the src side and the dst side use different nvme_id_sets.

# Failover
A SP has mulitple cntlrs, only one is primary, all others are standby. A failover means switch the primary from one cntlr to another. The failover invovles 3 kind of components:
1. The old primary cntlr, we call it old_primary.
2. The new primary cntlr, we call it new_primary.
3. All sides below to the same SP.

## old_primary
The old_primary  receives a SyncupCntlr RPC, which says it is not a primary. The
revision of this RPC is larger than all its previous SyncupCntlr RPCs. Then the
old_primary should follow below steps:
1. Set the ANA state from optimized to inaccessible.
2. Remove all subsystems from its port, including all normal subsystems and all subsystems created by the transfers.
3. Suspend all backend dm linear devices of the namespaces. Use the "--noflush" and "--nolockfs" to avoid stuck.
4. Reload the backend dm lienar devices to the corresponding dm-error device.
5. Cleanup all resources that a priamry shouldn't have.
6. Sleep OldPirmaryWait seconds.
7. Add all subsystems back to the port.

## new_primary
Once the new_primary receives a SyncupCntlr RPC, which says it is a primary. The
revision of this RPC is larger than all its previous SyncupCntlr RPCs. Then the
old_primary should follow below steps:
1. Make sure all groups are available.
2. Create thin pools, thin devices, raid0 devices.
3. Reload all namespaces' backend dm-linear devcies from dm-error to the raid0 devices.
4. Change ana state from inaccessible to optimized.

About "Make sure all groups are available", it is a bit complex, below is an example.

Assuing the SP has 2 slices, each slice has 1 meta group, 2 data groups. It uses RedundMdRaid1, each group has two legs. So the primary should have these resources:
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

Now we define the meaning of "a leg is avaialble". A leg is a nvmeof device,
most of the time, it is a single path device, with a single side. If we have a
transfer task, the leg might be a multipath device, it has a src_side and a
dst_side. In any case, "a leg is available" means the leg has a "optimized"
path. If a leg has a "non-optimized" path, it is not available, the side exports
a dm-error device via the "non-optimized" path, we can't use it.

## sides
For each side, if it finds the primary is changed via the SyncupSide RPC, and the RPC has the highest revision, it should follow below steps:
1. Change the old_primary ana group to "non-optimized"
2. Suspend the dm-linear device of the old_primary
3. Reload the dm-linear device of the new_primary, let it be on top of the loggical volume.
4. Sleep SideSwitchWait seconds
5. Reload the dm-linear device of the old_primary, let it be on top of the dm-error device.

# Start migration
The process of starting a migration is similar as a failover. The leg will have two sides, one is src_side, another is dst_side. From the cntlr perspective, the two sides are nvmeof multipath. When we start a migration.

## src_side
1. Set itself to inaccessible to all cntlrs
2. Suspend the dm-linear device to all cntlrs
3. Export the logical volume to the dst_side via nvmeof

## dst_side
1. Create the subsystems and namespaces to all cntlrs, use dm-error for all namespaces and set all ana state to inaccessible.
1. Create the DnMigrMetaDev
2. Connect the src_side via nvmeof, retry until succeeds
3. Create the dm-clone device
4. Reload the dm-linear device of the primary cntlr, let it be on top of the dm-clone device.
5. Set the subsystem to the primary as optimized, set other subsystems to non-optimized.

# transfer and clone
The source of the clone could be any nvmeof target. If the soruce is a distributed-nvme transfer, we could perform data live migration. Below is an example:
We have sp1 and sp2. sp1 has a ss, the ss has a ns.
The host connects to the sp1 ss and sends IOs to the ns.
Create a 

# cdc

# dn/cn and sp syncup
node/sp
cleanup
sqlite
version


# Components
## dnvctl
dnvctl dn create
dnvctl cn create
dnvctl sp create
dnvctl vol create
dnvctl admin update_etcd
dnvctl admin move_dn
dnvctl admin move_cn
dnvctl admin clone
## dnv-gateway
## dnv-worker
spworker, dnworker, cnworker
## dnv-agent
dnagent, cnagent
## dnv-cdc