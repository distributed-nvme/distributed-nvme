# service Gateway

## CreateCluster

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(Description) > MaxDescSize

#### ALREADY_EXISTS
The key "{dnv_prefix} cluster_conf {cluster_name}" exists in etcd.
Check the key in the STM.

#### ABORTED
unexpected error

### Default Value
* Clustername = DefaultClusterName
* Description = bytes("")
* QosRatio.BytesPerIops = 0
* QosRatio.BytesPerBps = 0

### Action
Create Below resoruces in the same STM:
* ClusterConf
* ClusterDesc
* DnGlobal
* CnGlobal
* SpGlobal

NextId = 1
len(ShardBucket) = ShardBucketSize
All items in the ShardBuckets are 0.

## DeleteCluster

### Error Code

#### INVALID_ARGUMENT
len(ClusterName) > MaxStrSize

#### NOT_FOUND
The key "{dnv_prefix} cluster_conf {cluster_name}" doesn't exist in etcd.

#### ABORTED
unexpected error

#### FAILED_PRECONDITION
Found items in etcd which have below key prefix:
* "{dnv_prefix} disk_node {cluster_id}"
* "{dnv_prefix} controller_node {cluster_id}"
* "{dnv_prefix} storage_pool {cluster_id}"

### Default Value
ClusterName = DefaultClusterName

### Action
Delete below items from etcd according to the ClusterName and ClusterId:
* ClusterConf
* ClusterDesc
* DnGlobal
* CnGlobal
* SpGlobal

## GetCluster

### Error Code

#### INVALID_ARGUMENT
len(ClusterName) > MaxStrSize

#### NOT_FOUND
The key "{dnv_prefix} cluster_conf {cluster_name}" doesn't exist in etcd.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
Read data from etcd and fill to the GetClusterReply according to the ClusterName
and ClusterId.

## ListClusters

### Error Code

#### INVALID_ARGUMENT
* count > MaxListCnt
* `base64.StdEncoding.DecodeString` report error against page_token

### ABORTED
unexpected error

### Default Value
* count = DefaultListCnt
* PageToken = ""

### Action
Get keys from the "{dnv_prefix} cluster_conf " prefix, extract the
{cluster_name}, then return the {cluster_name} list and the new PageToken.

Don't use STM in this action.

## UpdateClusterDesc

### Error Code

#### INVALID_ARGUMENT
* len(cluster_name) > MaxStrSize
* len(description) > MaxDescSize

#### NOT_FOUND
Can't find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.

#### ABORTED
* Can't find "{dnv_prefix} cluster_desc {cluster_id}" in etcd.
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action
Update the ClusterDesc.

## CreateDiskNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize
* len(description) > MaxDescSize
* NvmeTrConf is empty
* len(NvmeTrConf.TrType) > MaxStrSize
* len(NvmeTrConf.AdrFam) > MaxStrSize
* len(NvmeTrConf.TrAddr) > MaxStrSize
* len(NvmeTrConf.TrSvcId) > MaxStrSize
* len(Location) > MaxStrSize
* GetDnSize return size is less than DnExtSize

#### NOT_FOUND
Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.

#### ALREADY_EXISTS
The key "{dnv_prefix} disk_node {cluster_id} {addr_port}" exits in etcd.

#### RESOURCE_EXHAUSTED
The sum of all the items in DnGlobal.ShardBucket is already
MaxDnCntPerCluster. The sum of the ShardBucket is the current dn count of the
cluster, so we never need a range query to count the dns.

#### ABORTED
* Can not find "{dnv_prefix} dn_global {cluster_id}" in etcd
* GetDnSize fails
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action

#### Find DnId and ShardCode
Get DnGlobal, use the NextId as the DnId, then increase 1 to the
DnGlobal.NextId. Check all values in the ShardBucket, find the smallest value,
use its index as ShardCode, add one to that field. E.g. Assuming we have below
values:
```
NextId=18
ShardBucket=[52, 76, 13, 17, 2, 5, 2]
```
We set DnId=18, ShardCode=4. The smallest value in the ShardBucket is 2, both
the ShardBucket[4] and ShardBucket[6] are 2. We always choose the first one.
Then we update the DnGlobal to below:
```
NextId=19
ShardBucket=[52, 76, 13, 17, 3, 5, 2]
```
The ShardBucket length is 7 in our example, but in real case, the ShardBucket length should always be ShardBucketSize which is 256.

#### Get device size
Invoke DiskNodeAgent.GetDnSize to get the block device size of the dn.

The GetDnSize is a network call, so it must happen before the STM (see the STM
section in the Appendix). At this moment we don't know the DnId yet, so we set
GetDnSizeRequest.DnId=0. The dn agent doesn't need the DnId to report the
device size, the DnId in the request is only for logging.

#### Split the block device to segments
Given the DnExtSize=1G, MaxExtCntPerSegment=4K, the MaxSegmentSize is 1G*4K=4T.
Split the device size to 4T segments. Given the MaxSegmentCntPerDn=16, we will
have up to 16 segments. If the device size is larger than 64T, we will only use
the first 64T. Except the last segment, all other segmetns are exactly 4T.

We round down the device size to DnExtSize. E.g. if the device size is 1025M, we only use the first 1024M (1G). So the device has only a single segment, and the sigment has a signle extend. We will create a DnSegment with below values:
* Start=0
* ExtCnt=1
* The bitmap has only a single bit, the init value is 0.

If the device size is 9T, we will have 3 segments.
* segment0: Start=0, ExtCnt=4096, the bitmap has 4096 bits (512 bytes), all
  bits are 0.
* segment1: Start=4T (0x40000000000), ExtCnt=4096, the bitmap has 4096 bits,
  all bits are 0.
* segment2: Start=8T (0x80000000000), ExtCnt=1024 (the remaining 1T), the
  bitmap has 1024 bits (128 bytes), all bits are 0.

The DnSegment.Start is the byte offset of the segment on the block device.
Each bit in the bitmap covers one extend of DnExtSize bytes, bit i covers the
byte range [Start + i*DnExtSize, Start + (i+1)*DnExtSize). Bit value 0 means
free, 1 means allocated. See the "bitmap encoding" section in the Appendix.

#### Write the etcd keys
Create below resources in the same STM:
* DiskNode "{dnv_prefix} disk_node {cluster_id} {addr_port}":
  DnId and ShardCode as calculated above, Enabled=CreateDiskNodeRequest.Online,
  Healthy=true, NvmeTrConf from the request, SpLdIdList is empty,
  SegmentCnt=the calculated segment count.
* DnDesc "{dnv_prefix} dn_desc {cluster_id} {dn_id}"
* One DnSegment "{dnv_prefix} segment {cluster_id} {dn_id} {segment_idx}" per
  segment. The segment_idx is formatted with SegmentIdxFmt.
* One DnCapacity "{dnv_prefix} dn_capacity {cluster_id} {bin_idx} {free_space}
  {addr_port} {segment_idx}" per segment. The free_space of a new segment is
  ExtCnt*DnExtSize. See the "dn_capacity index" section in the Appendix for
  the bin_idx calculation. DnCapacity.Location=CreateDiskNodeRequest.Location.
* DnRev "{dnv_prefix} dn_rev {cluster_id} {shard_code} {addr_port}",
  Revision=1. The dnv-dnworker of this shard_code will notice the new key and
  invoke DiskNodeAgent.SyncupDn (see the "revision and sync flow" section in
  the Appendix).

Also update the DnGlobal (NextId and ShardBucket) in the same STM.

## DeleteDiskNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} disk_node {cluster_id} {addr_port}" in etcd.

#### FAILED_PRECONDITION
DiskNode.SpLdIdList is not empty. All the lds on the dn must be removed first
(by deleting/moving the storage pools which own them).

#### ABORTED
* Can not find "{dnv_prefix} dn_global {cluster_id}" in etcd.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM:
* Read the DiskNode to get the DnId, ShardCode and SegmentCnt.
* Read every DnSegment of the dn, calculate each segment's free_space from the
  bitmap (the free bit count times DnExtSize), then delete the matching
  DnCapacity key of each segment. We need the free_space to rebuild the
  DnCapacity key, that is why the segments must be read before deleting.
* Delete all the DnSegment keys.
* Delete the DnDesc, the DnRev and the DiskNode.
* Decrease 1 from DnGlobal.ShardBucket[ShardCode].

The dnv-dnworker of the shard notices the deleted DnRev key and stops health
checking the dn. The dn agent process on the node can simply be stopped; there
is no desired state left for it.

## GetDiskNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} disk_node {cluster_id} {addr_port}" in etcd.

#### ABORTED
* Can not find the DnDesc, the DnRev or any DnSegment which should exist.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM read the DiskNode, the DnDesc, the DnRev and all the
DnSegments (segment_idx from 0 to SegmentCnt-1), fill them into the
GetDiskNodeReply. GetDiskNodeReply.Revision comes from the DnRev.

## ListDiskNodes

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* count > MaxListCnt
* `base64.StdEncoding.DecodeString` report error against page_token

#### NOT_FOUND
Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.

#### ABORTED
unexpected error

### Default Value
* ClusterName = DefaultClusterName
* count = DefaultListCnt
* PageToken = ""

### Action
Get keys from the "{dnv_prefix} disk_node {cluster_id} " prefix, extract the
{addr_port}, then return the {addr_port} list and the new PageToken.

Don't use STM in this action.

## UpdateDiskNodeDesc

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize
* len(description) > MaxDescSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} disk_node {cluster_id} {addr_port}" in etcd.

#### ABORTED
* Can not find "{dnv_prefix} dn_desc {cluster_id} {dn_id}" in etcd.
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action
Read the DiskNode to get the DnId, then update the DnDesc. Do not bump the
DnRev, the description is not part of the desired state of the dn agent.

## InspectDiskNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} disk_node {cluster_id} {addr_port}" in etcd.

#### ABORTED
* The DiskNodeAgent.GetDnInfo GRPC fails.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
Read the DiskNode in an STM to get the DnId. Then, outside the STM, invoke
DiskNodeAgent.GetDnInfo against the addr_port and return the NodeResInfo to
the caller. The NodeResInfo carries the live (applied) state of the dn agent,
including the last applied revision, so the caller can compare it with the
desired state in etcd (GetDiskNode).

## CreateControllerNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize
* len(description) > MaxDescSize
* NvmeTrConf is empty
* len(NvmeTrConf.TrType) > MaxStrSize
* len(NvmeTrConf.AdrFam) > MaxStrSize
* len(NvmeTrConf.TrAddr) > MaxStrSize
* len(NvmeTrConf.TrSvcId) > MaxStrSize
* len(Location) > MaxStrSize

#### NOT_FOUND
Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.

#### ALREADY_EXISTS
The key "{dnv_prefix} controller_node {cluster_id} {addr_port}" exits in etcd.

#### RESOURCE_EXHAUSTED
The sum of all the items in CnGlobal.ShardBucket is already
MaxCnCntPerCluster.

#### ABORTED
* Can not find "{dnv_prefix} cn_global {cluster_id}" in etcd.
* GetCnSize fails.
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action

#### Find CnId and ShardCode
Same algorithm as CreateDiskNode, but against the CnGlobal.

#### Get the cn capacity
Invoke ControllerNodeAgent.GetCnSize (before the STM, CnId=0 like GetDnSize).
The reply is the total sp capacity this cn is willing to host:
* If the reply size is 0, use DefaultCnCap.
* If the reply size is larger than MaxCnCap, use MaxCnCap.
The result is the ControllerNode.MaxSize.

#### Write the etcd keys
Create below resources in the same STM:
* ControllerNode "{dnv_prefix} controller_node {cluster_id} {addr_port}":
  CnId and ShardCode as calculated, Enabled=CreateControllerNodeRequest.Online,
  Healthy=true, NvmeTrConf from the request, SpCntlrIdList is empty,
  MaxSize as calculated.
* CnDesc "{dnv_prefix} cn_desc {cluster_id} {cn_id}"
* CnCapacity "{dnv_prefix} cn_capacity {cluster_id} {free_space} {addr_port}",
  free_space=MaxSize (formatted with FreeSpaceFmt),
  CnCapacity.Location=CreateControllerNodeRequest.Location.
* CnRev "{dnv_prefix} cn_rev {cluster_id} {shard_code} {addr_port}",
  Revision=1.

Also update the CnGlobal in the same STM.

## DeleteControllerNode

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(addr_port) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} controller_node {cluster_id} {addr_port}" in etcd.

#### FAILED_PRECONDITION
ControllerNode.SpCntlrIdList is not empty. All the cntlrs on the cn must be
removed first (DeleteCntlr, or delete the storage pools).

#### ABORTED
* Can not find "{dnv_prefix} cn_global {cluster_id}" in etcd.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM:
* Read the ControllerNode to get the CnId, ShardCode and MaxSize.
* Delete the CnCapacity key. Because the SpCntlrIdList must be empty, the
  free_space in the key is exactly the MaxSize.
* Delete the CnDesc, the CnRev and the ControllerNode.
* Decrease 1 from CnGlobal.ShardBucket[ShardCode].

## GetControllerNode

### Error Code
Same as GetDiskNode, but against the controller_node/cn_desc/cn_rev keys.

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM read the ControllerNode, the CnDesc and the CnRev, fill them
into the GetControllerNodeReply.

## ListControllerNodes

### Error Code
Same as ListDiskNodes.

### Default Value
* ClusterName = DefaultClusterName
* count = DefaultListCnt
* PageToken = ""

### Action
Get keys from the "{dnv_prefix} controller_node {cluster_id} " prefix, extract
the {addr_port}, then return the {addr_port} list and the new PageToken.

Don't use STM in this action.

## UpdateControllerNodeDesc

### Error Code
Same as UpdateDiskNodeDesc, but against the controller_node/cn_desc keys.

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action
Read the ControllerNode to get the CnId, then update the CnDesc. Do not bump
the CnRev.

## InspectControllerNode

### Error Code
Same as InspectDiskNode, but the GRPC is ControllerNodeAgent.GetCnInfo.

### Default Value
ClusterName = DefaultClusterName

### Action
Read the ControllerNode in an STM to get the CnId. Then, outside the STM,
invoke ControllerNodeAgent.GetCnInfo against the addr_port and return the
NodeResInfo to the caller.

## CreateStoragePool

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize, or sp_name doesn't match ValidStrPattern
* len(description) > MaxDescSize
* len of any item in features > MaxStrSize
* redun_type is REDUN_TYPE_RAID5_LS or REDUN_TYPE_RAID6_ZR. Both are defined
  in the schema but not supported yet, only REDUN_TYPE_NONE and
  REDUN_TYPE_RAID1 are accepted.
* cntlr_cnt > MaxCntlrCntPerSp
* leg_cnt > MaxLegCntPerSp
* init_size is 0
* init_size is not a multiple of leg_cnt*DnExtSize
* init_size/leg_cnt > DnExtSize*MaxExtCntPerSegment (a single group side must
  fit into a single dn segment)

#### NOT_FOUND
Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.

#### ALREADY_EXISTS
The key "{dnv_prefix} storage_pool {cluster_id} {sp_name}" exits in etcd.

#### RESOURCE_EXHAUSTED
* The sum of all the items in SpGlobal.ShardBucket is already
  MaxSpCntPerCluster.
* Can not find cntlr_cnt distinct eligible cns. An eligible cn is enabled,
  healthy and its cn_capacity free_space >= init_size.
* Can not allocate the extends of any group side from the dn segments. See the
  allocation rules below.

#### ABORTED
* Can not find "{dnv_prefix} sp_global {cluster_id}" in etcd.
* Can not acquire the storage pool lock before the lock timeout.
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")
* cntlr_cnt = 1
* leg_cnt = 1
* redun_type = REDUN_TYPE_NONE (the proto3 zero value; there is no way to
  distinguish "not set" for an enum, the caller should always set it
  explicitly, dnvcli defaults to REDUN_TYPE_RAID1)

### Action

## DeleteStoragePool

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### FAILED_PRECONDITION
Any of TdNameList, NqnList, CloneNameList, MoveNameList of the StoragePool is
not empty. The user created objects (thin devices, subsystems, clones, moves)
must be deleted first. The cntlrs, legs and lds are created implicitly by
CreateStoragePool/GrowStoragePool/CreateCntlr, so they are deleted implicitly
here.

#### ABORTED
* Can not acquire the storage pool lock before the lock timeout.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
Acquire the storage pool lock like CreateStoragePool. Then in a single STM:
* Read the StoragePool, all the Cntlrs, all the Legs, all the
  LogicalDisk/LdForDn/LdForCn records.
* Per ld: clear the LdForDn bits from the owning DnSegment.Bitmap, move the
  DnCapacity key (delete old, write new with the increased free_space), remove
  the (sp_id, ld_id) pair from DiskNode.SpLdIdList, delete the
  LogicalDisk/LdForDn/LdForCn keys. Bump the DnRev of every touched dn once.
* Per cntlr: remove the (sp_id, cntlr_id) pair from
  ControllerNode.SpCntlrIdList, move the CnCapacity key with free_space
  increased by the sp size (the sum of the data group side sizes of all legs),
  delete the Cntlr key. Bump the CnRev of every touched cn once.
* Delete all the Leg keys, the SpName, the SpDesc, the SpRev and the
  StoragePool.
* Decrease 1 from SpGlobal.ShardBucket[ShardCode].

The dnv-dnworker/dnv-cnworker notice the DnRev/CnRev bumps and push the
shrunken SpLdIdList/SpCntlrIdList to the agents, which tear down the local
resources of the deleted sp. The dnv-spworker notices the deleted SpRev and
stops dispatching the sp.

## GetStoragePool

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### ABORTED
* Can not find the SpDesc, the SpRev, or any Cntlr/Leg/LogicalDisk key listed
  by the StoragePool.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM read the StoragePool, the SpDesc, the SpRev, every Cntlr in
CntlrIdList, every Leg in LegIdList and every LogicalDisk in LdIdList, fill
them into the GetStoragePoolReply. The CntlrList/LegList/LdList are in the
same order as the id lists.

## ListStoragePools

### Error Code
Same as ListDiskNodes.

### Default Value
* ClusterName = DefaultClusterName
* count = DefaultListCnt
* PageToken = ""

### Action
Get keys from the "{dnv_prefix} storage_pool {cluster_id} " prefix, extract
the {sp_name}, then return the {sp_name} list and the new PageToken.

Don't use STM in this action.

## UpdateStoragePoolDesc

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* len(description) > MaxDescSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### ABORTED
* Can not find "{dnv_prefix} sp_desc {cluster_id} {sp_id}" in etcd.
* unexpected error

### Default Value
* ClusterName = DefaultClusterName
* Description = bytes("")

### Action
Read the StoragePool to get the SpId, then update the SpDesc. Do not bump the
SpRev.

## InspectStoragePool

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In an STM read the StoragePool, every Cntlr (to get the cn addr_ports) and
every LogicalDisk (to get the dn addr_ports). Then, outside the STM:
* Per cntlr invoke ControllerNodeAgent.GetSpCntlrInfo against its cn, append
  the NodeResInfo to CntlrResInfoList.
* Per ld invoke DiskNodeAgent.GetSpLdInfo against its dn, append the
  NodeResInfo to LdResInfoList.

If an agent GRPC fails, don't fail the whole call. Fill a NodeResInfo with the
node id and a single ResInfo {ResName: the addr_port, Status:
RES_STATUS_MISSING, Details: the error string}, so the caller can still see
the rest of the sp. This is the API to check the hydration progress of the
clones and the moves before FinishMove/DeleteClone.

## GrowStoragePool

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* grp_size is 0
* grp_size is not a multiple of DnExtSize
* grp_size > DnExtSize*MaxExtCntPerSegment

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.
* leg_id is not in StoragePool.LegIdList.

#### RESOURCE_EXHAUSTED
* Can not allocate the extends of the new group from the dn segments.
* Any cn hosting a cntlr of the sp has less than grp_size free space.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action

## FindStoragePoolName

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} sp_id_to_name {cluster_id} {sp_id}" in etcd.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
Read the SpName key and return the sp_name. This is the reverse lookup used by
dnvadmin and the log analysis: the etcd keys and the device names on the nodes
carry the sp_id, this API maps it back to the user visible sp_name.

## CreateCntlr

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### RESOURCE_EXHAUSTED
* len(StoragePool.CntlrIdList) is already MaxCntlrCntPerSp.
* Can not find an eligible cn: enabled, healthy, not already hosting a cntlr
  of this sp, len(SpCntlrIdList) below MaxCntlrCntPerCn, cn_capacity
  free_space >= the sp size (the sum of the data group side sizes of all
  legs).

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action

## DeleteCntlr

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* cntlr_id is 0

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.
* cntlr_id is not in StoragePool.CntlrIdList.

#### FAILED_PRECONDITION
Cntlr.Enabled is true. The cntlr must be disabled (DisableCntlr) before it can
be deleted, so a possible failover to another cntlr already happened before
the record disappears.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM:
* Remove the cntlr_id from StoragePool.CntlrIdList, delete the Cntlr key.
* Remove the (sp_id, cntlr_id) pair from the cn's
  ControllerNode.SpCntlrIdList, move the CnCapacity key with free_space
  increased by the sp size, bump the CnRev.
* Per subsystem in StoragePool.NqnList: remove the cn's NvmeTrConf from the
  CdcEntry.NvmeTrConfList.
* Bump the SpRev.

The dnv-cnworker pushes the shrunken SpCntlrIdList to the cn agent, which
tears down the local cntlr stack and disconnects the lds.

## EnableCntlr

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* cntlr_id is 0

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.
* cntlr_id is not in StoragePool.CntlrIdList.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM set Cntlr.Enabled=true, append the cn's NvmeTrConf back to
every CdcEntry.NvmeTrConfList of the sp, bump the SpRev. Idempotent: enabling
an already enabled cntlr succeeds without changes. The cn agent rejoins the
election for this cntlr.

## DisableCntlr

### Error Code
Same as EnableCntlr.

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM set Cntlr.Enabled=false, remove the cn's NvmeTrConf from every
CdcEntry.NvmeTrConfList of the sp, bump the SpRev. Idempotent.

A disabled cntlr leaves the election and moves all its namespaces to the
inaccessible ana group. Disabling the currently active cntlr triggers a
failover to another enabled cntlr. Disabling the last enabled cntlr is
allowed, but the sp stops serving IO until a cntlr is enabled again; dnvcli
prints a warning in that case.

## CreateThinDevice

### Error Code
### Default Value
* ClusterName = DefaultClusterName
* OriName = "" (create a fresh thin device, not a snapshot)
* size = the ori td's Size when OriName is set

### Action

## DeleteThinDevice

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* len(td_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.
* Can not find "{dnv_prefix} thin_device {cluster_id} {sp_id} {td_name}" in
  etcd.

#### FAILED_PRECONDITION
* The TdId is referenced by any Namespace.TdId of any subsystem of the sp
  (read StoragePool.NqnList and every Subsystem in the same STM; the lists are
  bounded by MaxSsCntPerSp*MaxNsCntPerSs so this is cheap).
* The TdId is referenced by any Clone.DstTdId of the sp.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
Remove the td_name from StoragePool.TdNameList, delete the ThinDevice key,
bump the SpRev. The DevId is not reused. A td which is the origin of other
tds can be deleted, dm-thin snapshots stay valid after the origin is deleted.
The cn agent applies it with a `delete DevId` message per leg pool.

## ListThinDevices

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### ABORTED
* Can not find a ThinDevice key listed in StoragePool.TdNameList.
* unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action

## CreateSubsystem

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* len(nqn) > MaxNqnLength, or nqn doesn't match ValidNqnPattern
* nqn is the well known discovery nqn "nqn.2014-08.org.nvmexpress.discovery"

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.

#### ALREADY_EXISTS
The key "{dnv_prefix} subsystem {cluster_id} {sp_id} {nqn}" exits in etcd.

#### RESOURCE_EXHAUSTED
len(StoragePool.NqnList) is already MaxSsCntPerSp.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action

## DeleteSubsystem

### Error Code

#### INVALID_ARGUMENT
* len(ClusterName) > MaxStrSize
* len(sp_name) > MaxStrSize
* len(nqn) > MaxNqnLength

#### NOT_FOUND
* Can not find "{dnv_prefix} cluster_conf {cluster_name}" in etcd.
* Can not find "{dnv_prefix} storage_pool {cluster_id} {sp_name}" in etcd.
* Can not find "{dnv_prefix} subsystem {cluster_id} {sp_id} {nqn}" in etcd.

#### FAILED_PRECONDITION
Subsystem.NsList is not empty. The namespaces must be deleted first. The
AllowedHosts don't block the deletion, they are removed implicitly.

#### ABORTED
unexpected error

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM remove the nqn from StoragePool.NqnList, delete the Subsystem
key, delete the CdcEntry key, bump the SpRev. The cdc agents send a discovery
log change AEN to the hosts which could see the subsystem, the hosts running
nvme-stas disconnect automatically (like the POC test 2/3).

## ListSubsystems

### Error Code
Same shape as ListThinDevices.

### Default Value
ClusterName = DefaultClusterName

### Action
In a single STM read the StoragePool and every Subsystem in NqnList, return
the NqnList and the Subsystem list in the same order.

## CreateNamespace

### Error Code
### Default Value
### Action

## DeleteNamespace

### Error Code
### Default Value
### Action

## UpdateNamespaceStatus

### Error Code
### Default Value
### Action

## UpdateNamespaceAna

### Error Code
### Default Value
### Action

## CreateHost

### Error Code
### Default Value
### Action

## DeleteHost

### Error Code
### Default Value
### Action

## CreateClone

### Error Code
### Default Value
### Action

## DeleteClone

### Error Code
### Default Value
### Action

## ListClones

### Error Code
### Default Value
### Action

## CreateMove

### Error Code
### Default Value
### Action

## FinishMove

### Error Code
### Default Value
### Action

## CancelMove

### Error Code
### Default Value
### Action

## ListMoves

### Error Code
### Default Value
### Action

# service DiskNodeAgent

# service ControllerNodeAgent

# Appendix

## Calcualte cluster_id from cluster_name
Use this function:
```go
func clusterNameToId(clusterName string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(clusterName))
	return h.Sum64()
}
```

## STM
The term STM means software transactional memory. We use the golang etcd stm
client to make sure the etcd data consitency. Unless explicitly stated
otherwise, all read and write operations must occur within an STM transaction.
To reduce the STM time, if it is possible, we should prepare variables before
the STM . E.g. if we will use the ClusterId in the STM, and we know the
ClusterName, we should calculate the ClusterId from ClusterName before the STM.

## etcd keys
All keys in etcd are defined in the schema.proto. If a message is stored in etcd, there is a comment on top of it to describe the key schema, e.g.:
```protobuf
// {dnv_prefix} sp_id_to_name {cluster_id} {sp_id}
message SpName {
    string sp_name = 1;
}
```

Given below values:
```go
DnvPrefix = "dnv"
ClusterId = 16982411286042166782 // ebada5168620c5fe in hex
SpId = 17 // 0000000000000011 in hex
```
The key of SpName should be:
`dnv sp_id_to_name 16982411286042166782 0000000000000011`
The key use a space " " as the delimiter. Use the IdKeyFmt to format all the
IDs.

## page_token
Use `base64.StdEncoding.EncodeToString` to encode the last key as the
page_token. Use the `base64.StdEncoding.DecodeString` to decode the page_token,
and query against the etcd from the next one.

## addr_port
The addr_port is a string like "192.168.0.17:9000". The gateway, dnworker,
cnworker, spworker invoke the dnagent/cnagent GRPC against the addr_port.

## unexpected error
* Error reported by the STM client itself, not the app code error inside the
  STM.
* The protobuf seralize/deseralize error.
* Network connection error against the etcd.
