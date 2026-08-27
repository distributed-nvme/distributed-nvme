# distributed-nvme (dnv) — Design v001

Status: consolidated design. This document merges every up-to-date design source of the
project into a single implementable specification:

* `schema.proto` — authoritative wire/storage schema (etcd values, Gateway + agent gRPC).
* `constants.go` — authoritative limits, defaults and name prefixes.
* `name_fmt.go` — authoritative device / volume / NQN naming (a Go sketch; syntax typos
  in it are corrected here, semantic corrections are listed in Appendix C "Errata").
* Diagrams `000DiskNode` … `100Transfer` (drawio PNGs) — authoritative topology pictures.
* `partial_description.md` — allocation, cntlid slots, failover, migration, transfer/clone,
  component descriptions (carried over almost verbatim, updated where the schema moved on).
* `old_design.md` / `old_cmd.md` — old API generation. Their per-RPC specification *style*
  (Error Code / Default Value / Action) and their appendix rules (STM, keys, page tokens)
  are still the project conventions and are re-applied here to the **new** API surface.
  Object names from the old generation (`disk_node`, `storage_pool`, `LogicalDisk`, `Ld*`,
  `Move`, `*Desc`, `Enable/DisableCntlr`, separate `dnv-dnworker`/`dnv-cnworker`/…) are
  obsolete and must not be implemented.

Where the up-to-date sources contradict each other, the resolution chosen by this document
is marked `[E<n>]` and explained in Appendix C. An implementer (human or LLM) should treat
this document as the single source of truth and only fall back to the raw files for
protobuf field numbers.

Conventions used below:

* `{p}` is the etcd key prefix `DnvPrefix` = `"dnv"`.
* All numeric IDs are `uint64` and are rendered in keys and device names with
  `IdKeyFmt = "%016x"` (16 lower-case hex digits, zero padded) unless stated otherwise.
* "STM" = software transactional memory, i.e. the Go etcd `clientv3/concurrency` STM.
* MUST / SHOULD are used in the RFC sense.

---

## 1. System overview

dnv is a distributed block-storage system. It aggregates the raw disks of many **disk
nodes (DN)** into **storage pools (SP)**, runs the volume logic (thin provisioning,
striping, redundancy, snapshots, cloning, live migration) on **controller nodes (CN)**,
and exports virtual volumes to **hosts** over **NVMe-oF** with native NVMe multipath and
ANA. The control plane keeps all desired state in **etcd**; stateless control-plane
processes (gateway, workers, cdc) and per-node agents converge the data plane to that
desired state. See diagram `070Cluster_drawio.png`.

```
Hosts  ──nvme-of──▶  Controller Nodes (cntlrs)  ──nvme-of──▶  Disk Nodes (sides)
                          ▲            ▲                            ▲
                          └── SyncupCn/SyncupCntlr        SyncupDn/SyncupSide
                                   │                                │
              dnv-gateway ──▶ etcd ◀── dnv-worker (roles dn,cn,sp) ─┘
                                   ▲
                              dnv-cdc (discovery)
```

Processes (all use [viper](https://github.com/spf13/viper) for flags / config file /
environment variables; see §13):

| binary       | role |
|--------------|------|
| `dnv-gateway`| Serves the `Gateway` gRPC service to users/CLI. Reads and writes etcd. Calls agents only for `GetDnSize`/`GetCnSize`, `Inspect*` and bitmap reads. |
| `dnv-worker` | One binary, roles `dn`, `cn`, `sp` (any subset per instance). Watches revision keys in etcd, shards work by shard code, and drives agents via `SyncupDn`, `SyncupSide`, `SyncupCn`, `SyncupCntlr`, `PushCloneBitmap`, `PushMigrBitmap`. Also performs health checking and the automatic reactions of §10.4 (primary election, replacements). |
| `dnv-agent dn` / `dnv-agent cn` | Runs on every DN / CN. Serves `DiskNodeAgent` / `ControllerNodeAgent`. Owns the local LVM / device-mapper / mdadm / nvmet state, persists last applied revision + config in a local sqlite DB. |
| `dnv-cdc`    | NVMe-oF Central Discovery Controller. Watches the `cdc` keys and serves discovery + AENs to hosts. |
| `dnvctl`     | CLI over the Gateway (plus admin helpers such as the userspace copier of §11.4). |
| `dnvmon`     | Monitoring (design TBD, out of scope of v001). |

## 2. Terminology and object model

| term | meaning |
|------|---------|
| **cluster** | Namespace for everything else. Identified by `cluster_name`; `cluster_id = fnv64a(cluster_name)` (§5.2). One etcd installation can host many clusters. |
| **DN, disk node** | A machine contributing one raw block device (`--disk`). The device becomes one LVM VG; capacity is handed out as **extents**. |
| **CN, controller node** | A machine running the volume logic. Contributes no persistent storage (only a tmpfs for clone metadata) but has a capacity budget in extents. |
| **extent** | The allocation unit for both DN space and CN budget. Size = `ClusterConf.dn_bin_conf.extent_size`, default `DefaultDnExtSize` = 1 GiB. Counts are always rounded **down** (10 GiB + 3 MiB = 10 extents). |
| **SP, storage pool** | The unit of volume service. Owns cntlrs, slices, thin devices, subsystems, clones, transfers, migrations. Identified by `sp_name` (user visible) and `sp_id` (internal, used in device names / keys; reverse lookup via `sp_id_to_name`). |
| **cntlr** | One SP instance on one CN. Exactly one cntlr of an SP is **primary** (runs the full device stack and serves IO); the others are **standby** (keep leg connections, export dm-error, ANA inaccessible). `1 ≤ cntlr_cnt ≤ MaxCntlrCntPerSp(4)`, default 2. Diagrams `020ControllerNode`, `030PrimaryCntlr`, `050StandbyCntlr`. |
| **slice** | A vertical shard of an SP. Each slice is one dm thin-pool on the primary. Thin devices are striped (raid0) across all slices. `1 ≤ slice_cnt ≤ MaxSliceCntPerSp(16)`, fixed at SP creation. Diagram `040Slice`. |
| **group** | A contiguous chunk of pool space inside a slice. Each slice has ≥1 **meta group** (backs the thin-pool metadata device) and ≥1 **data group** (backs the thin-pool data device). `GrowSlice` appends groups. A group is either a single leg (RedundNone) or an md-raid1 over its legs (RedundMdRaid1). |
| **leg** | One replica of a group. Backed by exactly one **side** normally, two sides while that leg is being migrated. Groups may also carry **spare legs** (`MaxSpareLegPerGrp` = 2). |
| **side** | The DN-resident part of a leg: one LV in the DN VG plus, per cntlr, a dm-error + dm-linear + nvmet subsystem exported to that cntlr's CN. Diagrams `000DiskNode`, `010Side`. |
| **thin device (td)** | A user volume: one dm-thin volume per slice + one raid0 (dm striped) across the per-slice thin volumes on the primary. Snapshots are thin devices with `ori_id` set. |
| **subsystem (ss) / namespace (ns)** | The host-facing NVMe-oF objects. A namespace binds an `ns_idx` (NSID) of a subsystem to a thin device. Diagram `060VirtualVolume`. |
| **clone** | Pull-copy of an external NVMe-oF namespace into a local thin device via dm-clone, on the primary cntlr. Diagram `090Clone`. |
| **transfer (xfer)** | The source-side counterpart of a clone: exports a local namespace's raid0 read-only-ish over a dedicated subsystem so another SP (possibly another cluster) can clone from it. Diagram `100Transfer`. Clone+transfer = cross-SP live migration of a volume (§11.3). |
| **migration (migr)** | Side-level live move of one leg's data from one DN to another via dm-clone on the destination DN. Diagram `080Migration`. §11.2. |
| **shard code** | `uint32` in `[0,256)`, formatted `ShardCodeFmt = "%02x"`. Every DN, CN and SP gets one at creation; workers and cdc split ownership by shard code. |
| **revision** | Monotonic `uint64` per DN / CN / SP stored in `DnRev`/`CnRev`/`SpRev`. Bumped whenever the agent-visible desired state changes; drives the worker→agent sync (§9, §10). Also the optimistic-concurrency token carried in mutating requests. |
| **location** | Free-form failure-domain string per node (rack/zone). Stored only in the capacity keys' values; used for anti-affinity during allocation (§6). |
| **err_epoch** | Unix seconds when the object was last detected unhealthy by a worker; `0` = healthy. Compared against `EventThreshold` to trigger the automatic reactions of §10.4. |

Ownership hierarchy (etcd side):

```
ClusterConf
├── DnGlobal / CnGlobal / SpGlobal            (id + shard bookkeeping)
├── DnConf(addr_port)  ──side_ptr_list──▶ (sp_id, leg_id, side_id) it hosts
├── CnConf(addr_port)  ──cntlr_ptr_list─▶ (sp_id, cntlr_id) it hosts
└── SpConf(sp_name)
    ├── Cntlr(cntlr_id)                        one per CN, one primary
    ├── Slice(slice_id)
    │   ├── meta_grp_list: Group ── leg_list: Leg ── side_list: Side
    │   │                        └─ spare_leg_list: Leg ── Side
    │   └── data_grp_list: (same shape)
    ├── ThinDevice(td_name), Subsystem(nqn)+Namespace, Clone(clone_name),
    │   Transfer(xfer_name), Migration(migr_name)  (+ bitmap keys)
    └── SpName (sp_id → sp_name reverse map), SpNote, SpRev, CdcEntry per ss
```

### 2.1 Cardinality limits (from `constants.go`)

| limit | value | limit | value |
|---|---|---|---|
| `MaxDnCntPerCluster` | 1024 | `MaxCnCntPerCluster` | 1024 |
| `MaxSpCntPerCluster` | 4096 | `MaxTdCntPerSp` | 1024 |
| `MaxSsCntPerSp` | 4 | `MaxNsCntPerSs` | 4 |
| `MaxHostCntPerSs` | 8 | `MaxSliceCntPerSp` | 16 |
| `MinCntlrCntPerSp` / `MaxCntlrCntPerSp` / default | 1 / 4 / 2 | `MaxCntlrCntPerCn` | 256 |
| `MaxCloneCntPerSp` | 64 | `MaxXferCntPerSp` | 4 |
| `MaxMigrCntPerSp` | 4 | `MaxSideCntPerDn` | 1024 |
| `MaxLegPerGrp` | 8 | `MaxSpareLegPerGrp` | 2 |
| `MaxCloneBmCnt` | 16 | `MaxMigrBmCnt` | 4 |
| `DnPortBitmapSize` | 512 | `CnPortBitmapSize` | 512 |
| `ShardBucketSize` | 256 | `MaxListCnt` / `DefaultListCnt` | 1024 / 64 |

---

## 3. Data-plane device stacks

The data plane is built from stock Linux pieces: LVM, device-mapper targets
(`error`, `linear`, `striped` a.k.a. raid0, `thin-pool`, `thin`, `clone`), md-raid1 and
the kernel `nvmet` target / `nvme` host. Every device dnv creates has a deterministic
name (§4) so agents are fully idempotent and crash-restartable.

### 3.1 Disk node (`000DiskNode`, `010Side`)

Per DN, once (created by the dn agent at first `SyncupDn`):

1. `pvcreate` on the `--disk` device, `vgcreate` the DN VG `DnVgName` with
   `--physicalextentsize` = cluster `extent_size`.
2. One LV `migr-pv` (`DnMigrPvName`, size `DefaultMigrVgSize` = 1 GiB) inside the DN VG,
   used as the PV of the per-DN migration VG `DnMigrVgName`
   (`--physicalextentsize` = `DefaultMigrVgExtSize` = 4 MiB). It stores dm-clone metadata
   LVs for migrations whose **destination** side lives on this DN. The extents consumed
   by `migr-pv` are subtracted when the CP computes `DnConf.total_ext_cnt` [E12].
3. One nvmet port pool: port indexes `0 … DnPortBitmapSize-1`; `tr_svc_id` of port *i* =
   `base_tr_svc_id + 1 + i` where the base comes from `DnConf.nvme_tr_conf` [E11].

Per **side** (one per hosted leg replica):

* LV `DnLvName = {sp_id}-{side_id}` in the DN VG, `--extents` = `Group.ext_cnt` of the
  owning group. LV provisioning MUST follow the trim protocol of §9.4 (`not_trimmed` /
  `trimmed` LV tags + `blkdiscard`).
* Per cntlr of the SP (primary and standbys — the side learns them from
  `SyncupSideRequest.primary_cn_id/primary_idx/standby_id_to_idx`):
  * a dm-error device `DnErrorName` sized like the LV,
  * a dm-linear device `DnLinearName` whose table points at the **LV** for the primary
    cntlr's CN and at the **dm-error** device for every standby CN,
  * an nvmet subsystem `SideToCnNqn(cluster,dn,sp,side,cn)` on its own port,
    `allowed_hosts = [CnHostNqn(cluster,cn)]`, `attr_cntlid_min/max` from
    `Side.cntlid_slot` (§11.7), exposing one namespace backed by the dm-linear device.
    ANA state of the namespace's group on that port: `optimized` for the primary CN's
    subsystem, `non-optimized` for standby CNs' subsystems (a standby path therefore
    exists but errors out — it is a pre-connected placeholder that makes failover fast).

Failover and migration only ever *reload* the dm-linear tables and flip ANA states; the
exported namespace object never changes, so host/CN NVMe connections survive.

Additionally, while this DN hosts the **source** side of a migration: a dm-linear
`DnMigrSrcName` on top of the LV, exported through subsystem `MigrSrcNqn` with
`allowed_hosts = [DnHostNqn(cluster, dst_dn)]`. While it hosts the **destination** side:
an nvme host connection to the source's `MigrSrcNqn` (using `DnHostNqn` as hostnqn), a
dm-clone metadata LV `DnMigrMetaName = {sp_id}-{migr_id}` in the migration VG, and a
dm-clone device `DnMigrFinalName` (dest = the local LV, source = the connected nvme
device, region size = the SP's `DmPoolConf.data_block_size`). The per-cntlr dm-linears of
the destination side sit on top of the dm-clone (primary CN) / dm-error (standbys). See
`080Migration_drawio.png` and §11.2.

### 3.2 Controller node, common (`020ControllerNode`)

Per CN, once (created by the cn agent at first `SyncupCn`):

1. tmpfs mounted at `CnTmpfsPath = {tmpfs_prefix}/{cluster_id}-{cn_id}`
   (`DefaultTmpfsPrefix = /tmp/dnv-tmpfs`) [E4], sized `DefaultCloneVgSize` + slack
   (mount `-o size=2G` is fine for the 1 GiB default VG).
2. A file `tmp-file` (`CnTmpFilePath`) of `DefaultCloneVgSize` = 1 GiB inside it,
   attached to a free loop device; `pvcreate` + `vgcreate` the clone VG `CnCloneVgName`
   with `--physicalextentsize` = `DefaultCloneVgExtSize` = 4 MiB. It stores the dm-clone
   metadata LVs `CnCloneMetaName = {sp_id}-{clone_id}` of clones running on this CN.
   The metadata is deliberately volatile: clone progress durability comes from the clone
   bitmaps in etcd (§8.10, §11.3), and a restarted clone simply re-hydrates what the
   bitmaps do not exclude.
3. An nvmet port pool like the DN one, `CnPortBitmapSize` = 512 [E11].

A CN hosts up to `MaxCntlrCntPerCn` cntlrs of different SPs; at most one cntlr **per SP**
per CN.

### 3.3 Primary cntlr (`030PrimaryCntlr`, `040Slice`, `090Clone`, `100Transfer`)

Bottom-up, everything below is created/owned by the cn agent when `SyncupCntlr` says
`Cntlr.primary = true` ("make sure all groups are available", §11.1):

1. **Legs.** For each side of each leg: `nvme connect` to
   `SideToCnNqn(..., cn_id = this CN)` at `Side.nvme_tr_conf`, hostnqn =
   `CnHostNqn(cluster, cn)`. The **leg device** is a cn-local dm-linear wrapper over the
   nvme namespace device of the side whose ANA state toward this CN is `optimized`
   (single-side legs: the only side). During a migration the leg has two connected sides
   and the wrapper is reloaded from the source to the destination device when ANA flips
   [E10]. Reported per leg in `CntlrInfo.leg_id_to_leg`.
2. **Groups.** RedundNone: the group device is the single leg device. RedundMdRaid1: an
   md-raid1 `CnMdDevName`/`CnMdArrayName` over the leg devices, internal write-intent
   bitmap, `--bitmap-chunk` = `RedundMdRaid1.bitmap_chunk_block_cnt ×
   DmPoolConf.data_block_size`, `--data-offset` = 2048 KiB (`Group.meta_blocks` /
   `data_blocks` record the split in units of `data_block_size` [E13]). Assembly rules
   (create vs assemble vs add) are in §11.1.1. Spare legs are `mdadm --add`-ed as hot
   spares.
3. **Per slice** (`040Slice`): a dm-linear concat `CnPoolMetaName` over the slice's meta
   group devices (in `meta_grp_list` order) and a dm-linear concat `CnPoolDataName` over
   its data group devices [E2]; a dm thin-pool `CnPoolFinalName` (metadata dev = pool
   meta, data dev = pool data, block size = `DmPoolConf.data_block_size`,
   `low_water_mark` = data-dev blocks × `EventThreshold.pool_low_water_mark_pct` / 100).
4. **Per thin device × slice**: a dm-thin volume `CnThinDevName`, created with pool
   message `create_thin {dev_id}` or `create_snap {dev_id} {ori_id}`, virtual size =
   `ThinDevice.size / slice_cnt`.
5. **Per thin device**: a dm-striped raid0 `CnRaid0Name` across its per-slice thin
   volumes (slice order = `slice_idx`), chunk = `DmRaid0Conf.stripe_size`; a dm-error
   `CnErrorName` of the same size (permanent reload target); a dm-linear `CnNsDevName`
   — **the** namespace backing device — whose table points at the raid0 (normal), the
   dm-clone `CnCloneFinalName` (while a clone targets this td, `090Clone`), or dm-error
   (standby / failover transitions).
6. **Host-facing nvmet**: one port for the cntlr (from the port pool); per `Subsystem` a
   nvmet subsystem with `attr_cntlid_min/max` from `Cntlr.cntlid_slot`, `serial`/`model`
   from the etcd `Subsystem`, allowed hosts as configured (empty list ⇒
   `attr_allow_any_host=1`); per `Namespace` an nvmet namespace `nsid = ns_idx`,
   `device_path` = the td's `CnNsDevName`, `uuid`/`nguid` from the etcd record, ANA group
   `ag_id`, state `optimized` (primary) unless `suspended` (§8.8) — suspended ⇒ dm
   device suspended + ANA `inaccessible` everywhere.
7. **Clones / transfers / migrations** hosted by the SP, per §11.

### 3.4 Standby cntlr (`050StandbyCntlr`)

A standby keeps only: the leg nvme connections + leg wrappers (step 1 above), the
per-td `CnErrorName` and `CnNsDevName` (table → dm-error), and the host-facing nvmet
objects with every namespace's ANA group `inaccessible`. It has **no** md arrays, pools,
thin volumes or raid0s ("cleanup all resources that a primary shouldn't have"). For a
transfer it also keeps the dm-error-backed `CnXferFinalName` + xfer subsystem
counterparts (`100Transfer` right half).

### 3.5 Host view (`060VirtualVolume`)

A host discovers the SP's subsystems through dnv-cdc, connects to every enabled cntlr's
port, and the kernel aggregates the paths into one nvme multipath device because all
cntlrs export the same subsystem NQN and the same namespace identity (`nguid`/`uuid`),
with distinct cntlids guaranteed by cntlid slots (§11.7). Primary path ANA `optimized`,
standby paths `inaccessible`.

---

## 4. Naming

All formats below are normative. `NameFmt` is constructed from the prefixes in
`constants.go`: `dmPrefix = DmPrefix = "dnv"`, `nqnPrefix = NqnPrefix =
"nqn.2024-01.io.dnv"`, `tmpfsPrefix = DefaultTmpfsPrefix`, `dnVgPrefix =
DefaultDnVgPrefix = "dnv-dn"`, `cloneVgPrefix = DefaultCloneVgPrefix = "dnv-clone-vg"`,
`migrVgPrefix = DefaultMigrVgPrefix = "dnv-migr"`. Unless noted, every id field is
`%016x` and every kind field is `%01x`, joined by `-`.

### 4.1 dm-device kinds

DN-side (`dmKindDn*`): `0` error, `1` linear, `2` migr-src, `3` migr-final(dm-clone).
CN-side (`dmKindCn*`): `0` pool-meta, `1` pool-data, `2` pool-final(thin-pool),
`3` thin-dev, `4` raid0, `5` error, `6` ns-dev, `7` clone-final(dm-clone),
`8` xfer-final.

### 4.2 dm device names (`dmsetup` names; node path = `DmPath` = `/dev/mapper/{name}`)

| function | format |
|---|---|
| `DnErrorName(cluster,dn,sp,side,cn)`  | `dnv-{cluster}-{dn}-0-{sp}-{side}-{cn}` [E1] |
| `DnLinearName(cluster,dn,sp,side,cn)` | `dnv-{cluster}-{dn}-1-{sp}-{side}-{cn}` [E1] |
| `DnMigrSrcName(cluster,dn,sp,migr)`   | `dnv-{cluster}-{dn}-2-{sp}-{migr}` |
| `DnMigrFinalName(cluster,dn,sp,migr)` | `dnv-{cluster}-{dn}-3-{sp}-{migr}` |
| `CnPoolMetaName(cluster,cn,sp,slice)` | `dnv-{cluster}-{cn}-0-{sp}-{slice}` [E2] |
| `CnPoolDataName(cluster,cn,sp,slice)` | `dnv-{cluster}-{cn}-1-{sp}-{slice}` [E2] |
| `CnPoolFinalName(cluster,cn,sp,slice)`| `dnv-{cluster}-{cn}-2-{sp}-{slice}` |
| `CnThinDevName(cluster,cn,sp,td,slice)`| `dnv-{cluster}-{cn}-3-{sp}-{td}-{slice}` |
| `CnRaid0Name(cluster,cn,sp,td)`       | `dnv-{cluster}-{cn}-4-{sp}-{td}` |
| `CnErrorName(cluster,cn,sp,td)`       | `dnv-{cluster}-{cn}-5-{sp}-{td}` |
| `CnNsDevName(cluster,cn,sp,td)`       | `dnv-{cluster}-{cn}-6-{sp}-{td}` |
| `CnCloneFinalName(cluster,cn,sp,clone)`| `dnv-{cluster}-{cn}-7-{sp}-{clone}` |
| `CnXferFinalName(cluster,cn,sp,xfer)` | `dnv-{cluster}-{cn}-8-{sp}-{xfer}` |

### 4.3 md names (node path = `MdPath` = `/dev/md/{name}`)

`getShortId(clusterId, nodeId) = fnv64a(sprintf("%016x%016x", clusterId, nodeId)) &
0x0000FFFFFFFFFFFF` (48 bits). With `sliceIdxM = slice_idx | 0x80` when the group is a
**meta** group, else `slice_idx`:

* `CnMdDevName(cluster,cn,sp,sliceIdx,grpIdx,isMeta)` =
  `{shortId:%012x}{sp_id:%016x}{sliceIdxM:%02x}{grpIdx:%02x}` — 32 hex chars, no
  separators (fits the kernel md name limit); used for `/dev/md/{name}`.
* `CnMdArrayName(sp,sliceIdx,grpIdx,isMeta)` = `{sp_id:%016x}-{sliceIdxM:%02x}-{grpIdx:%02x}`
  — used for `mdadm --name` (the array's superblock name).

`grpIdx` is the group's index inside its `meta_grp_list` / `data_grp_list` (stable:
groups are only appended, never removed).

### 4.4 NQNs

nqnKinds: `0` DnHost, `1` CnHost, `2` SideToCn, `3` MigrSrc, `4` Xfer [E5]. `:` joined.

| function | format |
|---|---|
| `DnHostNqn(cluster,dn)` | `nqn.2024-01.io.dnv:0:{cluster}:{dn}` — hostnqn a DN uses when it connects out (migration destination pulling from the source). |
| `CnHostNqn(cluster,cn)` | `nqn.2024-01.io.dnv:1:{cluster}:{cn}` — hostnqn a CN uses toward sides, migr-src and xfer targets; also what users put into `Transfer.allowed_hosts`. |
| `SideToCnNqn(cluster,dn,sp,side,cn)` | `nqn.2024-01.io.dnv:2:{cluster}:{dn}:{sp}:{side}:{cn}` — subsystem a side exports to one CN. |
| `MigrSrcNqn(cluster,dn,sp,migr)` | `nqn.2024-01.io.dnv:3:{cluster}:{dn}:{sp}:{migr}` — subsystem the migration source side exports to the destination DN. |
| `XferNqn(cluster,sp,xfer)` [E5] | `nqn.2024-01.io.dnv:4:{cluster}:{sp}:{xfer}` — subsystem a transfer exports; the destination SP's user passes it as `Clone.src_nqn`. |

### 4.5 LVM / tmpfs / file names

| function | value |
|---|---|
| `DnVgName(cluster,dn)` | `dnv-dn-{cluster}-{dn}` |
| `DnLvName(sp,side)` / `DnLvPath` | `{sp}-{side}` / `/dev/{DnVgName}/{DnLvName}` |
| `DnMigrPvName` / `DnMigrPvPath` | `migr-pv` / `/dev/{DnVgName}/migr-pv` |
| `DnMigrVgName(cluster,dn)` | `dnv-migr-{cluster}-{dn}` |
| `DnMigrMetaName(sp,migr)` / `DnMigrMetaPath` | `{sp}-{migr}` / `/dev/{DnMigrVgName}/{DnMigrMetaName}` |
| `CnCloneVgName(cluster,cn)` | `dnv-clone-vg-{cluster}-{cn}` |
| `CnCloneMetaName(sp,clone)` / `CnCloneMetaPath` | `{sp}-{clone}` / `/dev/{CnCloneVgName}/{CnCloneMetaName}` |
| `CnTmpfsPath(cluster,cn)` | `{tmpfs_prefix}/{cluster:%016x}-{cn:%016x}` [E4] |
| `CnTmpFileName` / `CnTmpFilePath` | `tmp-file` / `{CnTmpfsPath}/tmp-file` |

---

## 5. etcd data model

### 5.1 Key grammar

Every stored protobuf message has one key schema, declared as a comment above the
message in `schema.proto`. Fields of a key are joined by a single space `" "`.
Formatting: ids with `IdKeyFmt = "%016x"`, shard codes with `ShardCodeFmt = "%02x"`,
free extent counts with `FreeSpaceFmt = "%016x"`, `bin_idx` with `"%01x"` [E6].
Example (`DnvPrefix = "dnv"`, `ClusterId = 0xebada5168620c5fe`, `SpId = 17`):
`SpName` key = `dnv sp_id_to_name 16982411286042166782 0000000000000011` — note the
cluster id in this historical example is decimal; **normative rule: every id in a key
uses `IdKeyFmt`**, i.e. `dnv sp_id_to_name ebada5168620c5fe 0000000000000011` [E7].
Values are the binary-serialized protobuf messages.

### 5.2 cluster_id derivation

```go
func clusterNameToId(clusterName string) uint64 {
    h := fnv.New64a()
    h.Write([]byte(clusterName))
    return h.Sum64()
}
```

### 5.3 Key table

| message | key | notes |
|---|---|---|
| `ClusterConf` | `{p} cluster_conf {cluster_name}` | cluster-wide QoS/bdev/bin/alloc conf |
| `DnGlobal`    | `{p} dn_global {cluster_id}` | `next_id`, `shard_bucket[256]` |
| `CnGlobal`    | `{p} cn_global {cluster_id}` | idem |
| `SpGlobal`    | `{p} sp_global {cluster_id}` | idem |
| `DnRev`       | `{p} dn_rev {shard_code} {cluster_id} {addr_port}` | watched by dn-role workers |
| `CnRev`       | `{p} cn_rev {shard_code} {cluster_id} {addr_port}` | watched by cn-role workers |
| `SpRev`       | `{p} sp_rev {shard_code} {cluster_id} {sp_name}` | watched by sp-role workers |
| `DnConf`      | `{p} dn_conf {cluster_id} {addr_port}` | |
| `DnNote`      | `{p} dn_note {cluster_id} {addr_port}` | free-form user bytes ≤ `MaxNoteSize` |
| `CnConf`      | `{p} cn_conf {cluster_id} {addr_port}` | |
| `CnNote`      | `{p} cn_note {cluster_id} {addr_port}` | |
| `DnCapacity`  | `{p} dn_capacity {cluster_id} {bin_idx} {free_ext_cnt} {addr_port}` | value.location; allocation index, §6 |
| `CnCapacity`  | `{p} cn_capacity {cluster_id} {free_ext_cnt} {addr_port}` | value.location |
| `CdcEntry`    | `{p} cdc {cluster_id} {shard_code} {sp_id} {ss_id}` | shard_code = the SP's; watched by dnv-cdc |
| `SpConf`      | `{p} sp_conf {cluster_id} {sp_name}` | |
| `SpNote`      | `{p} sp_note {cluster_id} {sp_name}` | [E8] |
| `Cntlr`       | `{p} cntlr {cluster_id} {sp_id} {cntlr_id}` | |
| `Slice`       | `{p} slice {cluster_id} {sp_id} {slice_id}` | groups/legs/sides embedded |
| `ThinDevice`  | `{p} thin_device {cluster_id} {sp_id} {td_name}` | |
| `Subsystem`   | `{p} subsystem {cluster_id} {sp_id} {nqn}` | namespaces embedded |
| `Clone`       | `{p} clone {cluster_id} {sp_id} {clone_name}` | |
| `CloneBitmap` | `{p} clone_bitmap {cluster_id} {sp_id} {clone_name} {bm_idx}` | bm_idx = source slice_idx, `%02x` [E6] |
| `Transfer`    | `{p} transfer {cluster_id} {sp_id} {xfer_name}` | |
| `Migration`   | `{p} migration {cluster_id} {sp_id} {migr_name}` | |
| `MigrBitmap`  | `{p} migration_bitmap {cluster_id} {sp_id} {migr_name} {bm_idx}` | bm_idx = append sequence 0…, `%02x` |
| `SpName`      | `{p} sp_id_to_name {cluster_id} {sp_id}` | reverse lookup for admin/log analysis |
| (worker registry) | `{p} worker {role} {worker_uuid}` | lease-bound, value empty; defined by this doc, not protobuf (§10.1) |

### 5.4 Globals: id allocation + shard buckets

`DnGlobal`/`CnGlobal`/`SpGlobal` each hold `next_id` (starts at 1) and
`shard_bucket` of length `ShardBucketSize` = 256, all zeros at cluster creation.
Creating a DN/CN/SP inside the STM:

1. `id = next_id; next_id += 1`. Ids are never reused ("all ids in a cluster are
   incremental"), which lets agents assume a deleted object never comes back.
2. `shard_code` = index of the **smallest** bucket value, first index on ties;
   `shard_bucket[shard_code] += 1`. Example: `NextId=18`,
   `ShardBucket=[52,76,13,17,2,5,2]` ⇒ `DnId=18`, `ShardCode=4`, then `NextId=19`,
   `ShardBucket=[52,76,13,17,3,5,2]` (real length is always 256).
3. `sum(shard_bucket)` is the live object count — the `Max*CntPerCluster` check never
   needs a range query. Deletion decrements the bucket.

Per-SP sub-object ids (`cntlr_id`, `slice_id`, `grp_id`, `leg_id`, `side_id`, `ss_id`,
`ns_id`, `td_id`, `clone_id`, `xfer_id`, `migr_id`, `ag_id`) all come from the single
`SpConf.next_id` counter (starts at 1). Thin-device `dev_id`s come from
`SpConf.next_dev_id` (starts at 1; `ori_id = 0` means "no origin") and are never reused.

### 5.5 Revision keys and the sync fan-out

`DnRev`/`CnRev`/`SpRev` values hold a single `revision` (starts at 1 on creation).
Rules:

* Any STM that changes agent-visible desired state of a DN / CN / SP bumps the matching
  revision **once** (even if it touched many sub-keys).
* Note updates never bump revisions. Node flag changes don't either (flags only gate CP
  scheduling, §8.2). Every SpConf/sub-object mutation listed in §8 bumps `SpRev` unless
  the RPC spec says otherwise.
* The request-side `DnRev`/`CnRev`/`SpRev` fields are optimistic-concurrency tokens: the
  STM MUST assert `stored.revision == request.revision` and fail the RPC with `ABORTED`
  ("stale revision") on mismatch. `Get*` returns the current token.
* Workers watch the rev prefixes of the shard codes they own and react per §10.

### 5.6 Capacity index keys

`DnCapacity`/`CnCapacity` are pure indexes for the allocator: the key encodes
`(bin_idx,) free_ext_cnt` so a lexicographic range scan returns nodes ordered by free
space (the zero-padded `%016x` makes lexical order = numeric order). Whenever a node's
`free_ext_cnt` changes, the same STM deletes the old capacity key and writes the new one
(the value — `location` — is carried over; the capacity value is the **only** place a
node's location is stored). A DN whose free count falls below `1 << bin0_shift` has **no**
capacity key at all ("less than 1 extent ⇒ not in any bin"); it reappears when space is
freed. §6 defines `bin_idx`.

### 5.7 page_token

`base64.StdEncoding.EncodeToString(lastReturnedKey)`; decode with
`base64.StdEncoding.DecodeString` and range from the **next** key. Decode failure ⇒
`INVALID_ARGUMENT`. Empty token ⇒ start of prefix. List RPCs never use the STM.

### 5.8 STM discipline

Unless a spec says otherwise, all etcd reads/writes of one RPC happen in **one** STM
transaction. Prepare everything computable outside beforehand (e.g. `cluster_id` from
`cluster_name`, name formatting, candidate lists) to keep transactions short. Network
calls to agents (GetDnSize/GetCnSize, Inspect*, bitmap reads) MUST happen **outside**
(before/after) the STM.

### 5.9 UNEXPECTED_ERROR → `ABORTED`

The catch-all `ABORTED` covers: STM-client errors (not app logic inside the STM),
protobuf (de)serialization errors, etcd connection errors — plus the specific `ABORTED`
cases listed per RPC (missing invariant keys, agent RPC failures where specified, stale
revision tokens).

---

## 6. Allocation (extents, bins, candidates)

### 6.1 Size → extents

Everything is allocated in extents of `extent_size` (cluster-wide,
`ClusterConf.dn_bin_conf.extent_size`, default 1 GiB). Counts round **down**. A DN's
`total_ext_cnt` = floor(disk size / extent_size) − extents consumed by `migr-pv` [E12].
A CN's `total_ext_cnt` = floor(capacity budget / extent_size) where the budget comes
from `GetCnSize` (0 ⇒ `DefaultCnCap` = 4 TiB; > `MaxCnCap` = 64 TiB ⇒ clamp; a nonzero
reply below `MinCnCap` [E9] is treated like 0).

### 6.2 DN bins

With `DnBinConf` shifts (`0 ≤ bin0 < bin1 < bin2 < bin3 ≤ 63`, defaults 0/4/8/12) define
`level_i = 1 << bin_i_shift` (defaults 1/16/256/4096). A DN with free extents `f` sits in
bin `b` where `level_b ≤ f < level_{b+1}` (bin3: `f ≥ level3`; `f < level0` ⇒ no bin,
§5.6). Bins balance two failure modes: (a) always draining one big DN before touching the
next, and (b) spreading so thin that no single DN can satisfy a request although the
cluster has plenty of space in aggregate.

### 6.3 Finding DN candidates

Input: `CandExtCnt` (needed free extents per candidate), `CandCnt` (how many wanted),
plus `BlackList`/`WhiteList` of `addr_port`s from the `NodeSelector` (empty white list =
all DNs; black list always excludes, even if white-listed). Skip DNs that are `disabled`
(flag bit 0) or unhealthy (`err_epoch != 0`).

1. Start `LocList = []`, `DnList = []`.
2. Pick the smallest bin `b` whose range can hold `CandExtCnt` (skip bins with
   `level_{b+1} ≤ CandExtCnt`). For `b … 3`: range-scan
   `{p} dn_capacity {cluster_id} {b} ` **descending** (largest free first); for each DN:
   stop this bin when `free < CandExtCnt`; skip if filtered by the lists/flags/health or
   if its `location` is already in `LocList`; else append to `DnList`+`LocList`; return
   as soon as `len(DnList) ≥ CandCnt`.
3. After bin 3, return whatever was collected (the caller decides whether it is enough).

The `LocList` rule gives location anti-affinity for free: two legs of one group can never
land in the same failure domain in one allocation round.

### 6.4 Finding CN candidates

Same as §6.3 but there are no bins: one descending scan over
`{p} cn_capacity {cluster_id} `; skip disabled/unhealthy/black-listed/duplicate-location
CNs, plus CNs already hosting a cntlr of the same SP.

### 6.5 Per-operation allocation

* **CreateStoragePool — DNs.** For every group to create (per slice: 1 meta + 1 data):
  `CandExtCnt` = that group's `ext_cnt`; `RequiredCnt` = legs per group (RedundNone: 1,
  RedundMdRaid1: 2); `DnCandCnt = RequiredCnt × AllocConf.dn_batch_size`. Get candidates
  (§6.3); `< RequiredCnt` ⇒ `RESOURCE_EXHAUSTED`; pick `RequiredCnt` **randomly** from
  the list; add the picked DNs to the BlackList; continue with the next group. The
  growing black list means every leg of the SP lands on a distinct DN.
* **CreateStoragePool — CNs.** `CandExtCnt` = sum of `ext_cnt` over **all** groups of the
  SP; `RequiredCnt = 1`, `CnCandCnt = cn_batch_size`; repeat `cntlr_cnt` times, random
  pick, black-list the pick.
* **GrowSlice**: like the DN flow for exactly one group (black list starts with all DNs
  already hosting a leg of that group, so the new group still spreads; other groups' DNs
  are allowed).
* **CreateMigration**: one DN, `CandExtCnt` = the group's `ext_cnt`; black list starts
  with the DNs (and thus locations) of every leg/side of the group.
* **CreateSpareLeg**: one DN, same black-list seeding as CreateMigration.
* **CreateCntlr**: one CN, `CandExtCnt` = sum of all group `ext_cnt`s of the SP.

Free-extent bookkeeping in the same STM as the pick: each leg's DN
`free_ext_cnt -= group.ext_cnt`; each cntlr's CN `free_ext_cnt -= Σ group.ext_cnt`;
capacity keys moved per §5.6; reverse on delete.

---

## 7. Common validation

* `MaxStrSize` = 64 bytes for `cluster_name`, `addr_port`, `sp_name`, `td_name`,
  `clone_name`, `xfer_name`, `migr_name`, `location`, `NvmeTrConf` members,
  `Subsystem.serial/model`. Names additionally MUST match
  `ValidStrPattern = ^[a-zA-Z0-9\-_/.:]+$`.
* NQNs: length ≤ `MaxNqnLength` = 223 and match `ValidNqnPattern`; the well-known
  discovery NQN `nqn.2014-08.org.nvmexpress.discovery` is rejected for `CreateSubsystem`.
* Notes ≤ `MaxNoteSize` = 4 KiB.
* `addr_port` looks like `192.168.0.17:9000` — the gRPC endpoint of the node's agent.
* Bounded numeric parameters (reject outside `[Min, Max]`, substitute the `Default*`
  when a proto3 zero is received and a default exists):

| parameter | min | max | default |
|---|---|---|---|
| `dn_bin_conf.extent_size` | 64 MiB | 1 TiB | 1 GiB |
| `alloc_conf.dn_batch_size` / `cn_batch_size` | 1 | 1024 | 16 |
| `dm_pool_conf.data_block_size` | 64 KiB | 1 GiB | 1 MiB |
| `EventThreshold.pool_low_water_mark_pct` | 10 | 90 | 50 |
| `dm_raid0_conf.stripe_size` | 4 KiB | 64 MiB | 64 KiB |
| `redund_md_raid1.bitmap_chunk_block_cnt` | 1 | 1024 | 128 |
| (dm region block cnt, same meaning) | 1 | 1024 | 128 |
| `DmCloneConf.hydration_threshold` (clone) | 1 | 8 | 1 |
| `DmCloneConf.hydration_batch_size` (clone) | 1 | 4 | 1 |
| `DmCloneConf.hydration_threshold` (migr) | 1 | 8 | 1 |
| `DmCloneConf.hydration_batch_size` (migr) | 1 | 4 | 1 [E3] |
| `EventThreshold.primay_unhealthy` (s) | ≥1 | — | 5 |
| `EventThreshold.cntlr_unhealthy` (s) | ≥1 | — | 600 |
| `EventThreshold.side_unhealthy` (s) | ≥1 | — | 600 |
| `EventThreshold.leg_unhealthy` (s) | ≥1 | — | 1200 |
| list `count` | 1 | 1024 | 64 |

* `bdev_feature_list` MUST be empty in this version (`INVALID_ARGUMENT` otherwise);
  `RedundConf` accepts only `redund_none` and `redund_md_raid1`.
* Default-value resolution order everywhere: request field → the owning SP's stored conf
  → `ClusterConf` → `constants.go` default → proto3 zero.
* `CmdSoftTimeout` = 3 s / `CmdHardTimeout` = 5 s: agents SIGTERM a shell command at the
  soft timeout and SIGKILL at the hard timeout, reporting `RES_STATUS_ERROR` with the
  command output in `details`.

---

## 8. `service Gateway` — RPC specifications

Format follows the project convention: **Errors / Defaults / Action**. Errors common to
every RPC and therefore not repeated: `INVALID_ARGUMENT` for any §7 violation on any
supplied field; `ABORTED` per §5.9; `ABORTED` "stale revision" whenever the request's
`DnRev`/`CnRev`/`SpRev` token mismatches the stored one (§5.5). Default
`cluster_name = DefaultClusterName = "default"` for every RPC that takes one. Every RPC
that names an SP first resolves `{p} cluster_conf` (`NOT_FOUND` if absent) and
`{p} sp_conf {cluster_id} {sp_name}` (`NOT_FOUND` if absent); those two checks are
implied below. All mutating SP RPCs except `DeleteStoragePool` fail with
`FAILED_PRECONDITION` when `SpConf.deleting == true`.

### 8.1 Clusters

**CreateCluster** —
Errors: `ALREADY_EXISTS` if `{p} cluster_conf {cluster_name}` exists or any of
`{p} dn_global|cn_global|sp_global {cluster_id}` exists (hash-collision guard); checks
inside the STM.
Defaults: §7 tables for every `ClusterConf` member.
Action: one STM creates `ClusterConf` (from the request), and `DnGlobal`, `CnGlobal`,
`SpGlobal` each with `next_id = 1` and `shard_bucket` = 256 zeros. Reply `cluster_id`.

**DeleteCluster** —
Errors: `NOT_FOUND` cluster; `FAILED_PRECONDITION` if any key exists under
`{p} dn_conf {cluster_id} `, `{p} cn_conf {cluster_id} ` or `{p} sp_conf {cluster_id} `.
Action: one STM deletes `ClusterConf`, `DnGlobal`, `CnGlobal`, `SpGlobal`. Reply
`cluster_id`.

**GetCluster** — Errors: `NOT_FOUND` cluster; `ABORTED` if a global key is missing.
Action: one STM reads the four keys into the reply.

**ListClusters** — Errors: `INVALID_ARGUMENT` on bad `count`/token. Defaults:
`count = DefaultListCnt`, token = "". Action: no STM; range `{p} cluster_conf ` prefix,
return up to `count` `{cluster_name}`s + next token (§5.7).

### 8.2 Disk nodes

**CreateDiskNode** —
Errors: `NOT_FOUND` cluster; `ALREADY_EXISTS` `dn_conf` key; `RESOURCE_EXHAUSTED`
`sum(DnGlobal.shard_bucket) ≥ MaxDnCntPerCluster`; `INVALID_ARGUMENT` if `nvme_tr_conf`
is empty or the reported disk size yields `total_ext_cnt < 1`; `ABORTED` if `dn_global`
missing or `GetDnSize` fails.
Defaults: `location = addr_port`; `enabled` as sent.
Action: (pre-STM) call `DiskNodeAgent.GetDnSize` at `addr_port` with `dn_id = 0` (the id
is only for logging; the agent doesn't need it) and compute `total_ext_cnt` (§6.1). One
STM then: allocate `dn_id` + `shard_code` from `DnGlobal` (§5.4); write `DnConf`
(`disabled = !request.enabled`, `err_epoch = 0`, `nvme_tr_conf`, empty `side_ptr_list`,
`total_ext_cnt`, `free_ext_cnt = total_ext_cnt`); write `DnCapacity` at the computed
`bin_idx` with `location`; write `DnRev = 1` under the shard code (its appearance makes
the owning dn-worker start syncing and health-checking the node); update `DnGlobal`.
Reply `dn_id`.

**DeleteDiskNode** —
Errors: `NOT_FOUND` cluster/dn; `FAILED_PRECONDITION` `side_ptr_list` not empty (delete
or migrate the owning SPs first); `ABORTED` `dn_global` missing / stale `dn_rev`.
Action: one STM reads `DnConf`, deletes `DnRev` (the worker stops health checking; the
agent process can simply be stopped — no desired state remains), `DnConf`, `DnNote`,
`DnCapacity`; `DnGlobal.shard_bucket[shard_code] -= 1`. Reply `dn_id`.

**GetDiskNode** — Errors: `NOT_FOUND`; `ABORTED` if the `DnRev` that must exist is
missing. Action: one STM reads `DnConf` + `DnRev` into the reply. (The note is not in
the reply — see [E14].)

**ListDiskNodes** — like ListClusters over `{p} dn_conf {cluster_id} `, returning the
`{addr_port}` suffixes.

**UpdateDiskNodeNote** — Errors: `NOT_FOUND`; stale rev ⇒ `ABORTED`. Action: read
`DnConf` for the id, write `DnNote`. Do **not** bump `DnRev`. Reply `dn_id`.

**SetDiskNodeFlag / ClearDiskNodeFlag** —
Errors: `NOT_FOUND`; `INVALID_ARGUMENT` `idx ≥ 32` or `idx` names an undefined bit
(only bit 0 is defined today); stale rev ⇒ `ABORTED`.
Semantics: the node's flags word is a `uint32`; **bit 0 = disabled** (backed by
`DnConf.disabled` [E15]; a disabled node is skipped by allocation §6.3 but keeps serving
existing sides). Action: one STM flips the bit, no `DnRev` bump. Reply `dn_id` + the
resulting `flags` word.

**InspectDiskNode** — Errors: `NOT_FOUND`; `ABORTED` if the agent gRPC fails.
Action: STM-read `DnConf` for logging ids only; then, outside the STM, call
`DiskNodeAgent.GetDnInfo` and return the `DnInfo` (live/applied state incl. the last
applied revision, so callers can diff against `GetDiskNode`'s desired state).

### 8.3 Controller nodes

Mirror images of §8.2 against `cn_conf`/`cn_note`/`cn_capacity`/`cn_rev`/`CnGlobal`,
with these differences:

* **CreateControllerNode** calls `ControllerNodeAgent.GetCnSize` (`cn_id = 0`) and maps
  the reply to `total_ext_cnt` per §6.1 (0 ⇒ default 4 TiB, clamp to 64 TiB, [E9]).
  `RESOURCE_EXHAUSTED` at `MaxCnCntPerCluster`.
* **DeleteControllerNode**: `FAILED_PRECONDITION` if `cntlr_ptr_list` not empty. Since
  the list is empty, `free_ext_cnt == total_ext_cnt`, so the capacity key to delete is
  fully determined.
* **InspectControllerNode** calls `ControllerNodeAgent.GetCnInfo`.

### 8.4 Storage pools

**CreateStoragePool** —
Errors: `ALREADY_EXISTS` `sp_conf` key; `RESOURCE_EXHAUSTED` when
`sum(SpGlobal.shard_bucket) ≥ MaxSpCntPerCluster`, when < `cntlr_cnt` eligible CNs, or
when DN allocation fails (§6.5); `INVALID_ARGUMENT` when: `cntlr_cnt` outside
`[MinCntlrCntPerSp, MaxCntlrCntPerSp]`; `slice_cnt` outside `[1, MaxSliceCntPerSp]`;
`init_ext_cnt == 0`; `cntlid_slot_list` has duplicates, values ≥ `CnCntlidSlotCnt`(8) or
fewer entries than `cntlr_cnt`; any §7 violation in `bdev_conf` / `auto_threshold`
(type = `EventThreshold` [E16]).
Defaults: `cntlr_cnt = DefaultCntlrCntPerSp`(2); `cntlid_slot_list = [0..7]`;
`bdev_conf` member-wise from `ClusterConf.bdev_conf` then constants (`redund_conf`
unset ⇒ `redund_none`, and `dnvctl` defaults it to `redund_md_raid1` on the CLI);
`auto_threshold` member-wise from the §7 defaults.
Action:
1. Pre-STM: resolve defaults; plan groups: per slice one **data** group with
   `ext_cnt = init_ext_cnt` [E28] and one **meta** group with
   `ext_cnt = clamp(ceil(init_ext_cnt / 1024), 1, 16)` [E17].
2. Pre-STM candidate scan + STM commit may be retried as a unit on STM conflict. In the
   STM: allocate `sp_id`/`shard_code` from `SpGlobal`; run the §6.5 DN and CN picks
   against the capacity keys; write `SpConf` (`next_id` advanced past all consumed ids,
   `next_dev_id = 1`, `bdev_conf`, `event_threshold`, `cntlid_slot_list`,
   `sp_level = SP_LEVEL_READWRITE`, `deleting = false`, id/name lists filled);
   `SpName`; one `Cntlr` per picked CN (`cntlr_idx` = 0…`cntlr_cnt`-1, idx 0
   `primary = true`, `cntlid_slot` = the idx-th entry of `cntlid_slot_list`,
   `cn_addr_port` [E18] + `nvme_tr_conf` from the CN); one `Slice` per slice
   (`slice_idx` 0…, groups → legs (`leg_idx` 0…) → one `Side` each:
   `addr_port`/`nvme_tr_conf` from the picked DN with a freshly allocated port [E11],
   `cntlid_slot` = `cntlid_slot_list[0]` (§11.7), `err_epoch = 0`); update every picked
   DN's `DnConf.side_ptr_list`/`free_ext_cnt`/`DnCapacity` and bump each `DnRev` once;
   likewise every picked CN (`cntlr_ptr_list`) and `CnRev`; write `SpRev = 1`;
   update `SpGlobal`.
3. Reply `sp_id`. Workers then converge: dn-workers push the new side pointers
   (`SyncupDn`), sp-workers push side + cntlr configs (`SyncupSide`/`SyncupCntlr`), and
   the primary builds §3.3.

**DeleteStoragePool** —
Errors: `FAILED_PRECONDITION` if any of `td_name_list`, `nqn_list`, `clone_name_list`,
`xfer_name_list`, `migr_name_list` is non-empty (user-created objects first; cntlrs,
slices, groups, legs, sides were created implicitly and are deleted implicitly).
Action: one STM: read `SpConf`, all `Cntlr`s, all `Slice`s; per side: remove the pointer
from its DN's `side_ptr_list`, return `group.ext_cnt` to `free_ext_cnt`, move the
`DnCapacity` key, bump that `DnRev` once per DN; per cntlr: remove the pointer from its
CN, return the SP footprint to `free_ext_cnt`, move `CnCapacity`, bump `CnRev` once per
CN; delete every `Cntlr`, `Slice`, `SpName`, `SpNote`, `SpRev`, `SpConf`;
`SpGlobal.shard_bucket[shard_code] -= 1`. Agents notice the shrunken pointer lists via
`SyncupDn`/`SyncupCn` and tear the local stacks down; the sp-worker notices the deleted
`SpRev` and stops dispatching. Reply `sp_id`. (`SpConf.deleting` is reserved for a future
asynchronous teardown; in v001 it is only ever read by the §8-header guard.)

**GetStoragePool** — one STM reads `SpConf`, `SpRev`, every `Cntlr` in `cntlr_id_list`
and every `Slice` in `slice_id_list` (same order) into the reply; a missing listed key ⇒
`ABORTED`.

**ListStoragePools** — prefix `{p} sp_conf {cluster_id} `, returns `{sp_name}`s.

**UpdateStoragePoolCntlidSlotList** — Errors: `INVALID_ARGUMENT` duplicates / values ≥ 8
/ list missing a slot currently used by any cntlr or side of the SP. Action: STM write +
bump `SpRev`. Reply `sp_id`.

**UpdateStoragePoolLevel** — Action: STM set `SpConf.sp_level`, bump `SpRev`; workers
propagate to every side and cntlr [E19]. Levels (each includes all restrictions above
it): `READWRITE`(0) normal; `READONLY`(16) thin devices + sides read-only, clone/migr
hydration paused; `NO_CLONE`(32) also don't build clone dm-clones; `NO_THINPOOL`(48)
also no thin pools; `NO_REDUND`(64) also no raid1; `NO_MIGRATION`(80) also no migration
dm-clones; `NO_SIDE`(96) also don't export sides; `DISABLE`(112) agents keep only the
logical volumes. Levels exist for staged disaster recovery / maintenance.

**UpdateStoragePoolNote** — like node notes; no `SpRev` bump.

**FindStoragePoolNames** (rpc `FindStoragePoolName`) — no STM required beyond one
snapshot read; for each requested `sp_id` read `{p} sp_id_to_name {cluster_id} {sp_id}`;
missing ids are omitted from the reply map [E20]. This is the reverse lookup used by
dnvadmin/log analysis, since keys and device names carry `sp_id`, not `sp_name`.

### 8.5 GrowSlice

Errors: `NOT_FOUND` `slice_id` not in `SpConf.slice_id_list`; `INVALID_ARGUMENT`
`ext_cnt == 0`; `RESOURCE_EXHAUSTED` when no DN candidates (§6.5) or when any cntlr's CN
has `free_ext_cnt < ext_cnt`.
Action: allocate legs for one new group (`is_meta` selects the list); in the STM append
`Group{grp_id, ext_cnt, meta_blocks/data_blocks per [E13], legs+sides}` to the slice,
update the involved DNs (+`DnRev`s) and every cntlr CN's budget (+`CnRev`s), bump
`SpRev`. Agents extend the pool-meta/pool-data linear tables and resize the thin pool
online. Reply `slice_id`, `grp_id`.

### 8.6 Cntlrs

**CreateCntlr** —
Errors: `RESOURCE_EXHAUSTED` `len(cntlr_id_list) ≥ MaxCntlrCntPerSp` or no eligible CN
(enabled, healthy, not already hosting a cntlr of this SP, `len(cntlr_ptr_list)` <
`MaxCntlrCntPerCn`, `free_ext_cnt ≥` SP footprint); `INVALID_ARGUMENT` `cntlid_slot`
not in `SpConf.cntlid_slot_list` or already used by another cntlr of the SP.
Action: pick a CN (§6.5); STM: new `Cntlr` (`primary = false`, `enabled = true` [E21],
`cntlr_idx` = smallest unused idx), append id, CN bookkeeping + `CnRev`, append the CN's
`nvme_tr_conf` to every `CdcEntry` of the SP, bump `SpRev`. The sp-worker's next
`SyncupSide` round tells every side about the new standby (`standby_id_to_idx`), and the
sides grow a dm-error/dm-linear/nvmet export for it (§3.1). Reply `cntlr_id`.

**DeleteCntlr** —
Errors: `NOT_FOUND` id not in list; `FAILED_PRECONDITION` `primary == true` or
`enabled == true` (disable first so a failover has already happened before the record
disappears).
Action: STM: remove id + `Cntlr` key; CN bookkeeping (+footprint back, `CnRev`); remove
the CN's `nvme_tr_conf` from every `CdcEntry`; bump `SpRev`. Sides drop the export;
the cn agent tears down its stack. Reply `cntlr_id`.

**UpdateCntlrEnabled** —
Action: STM set `Cntlr.enabled` [E21]; on disable remove / on enable append the CN's
`nvme_tr_conf` in every `CdcEntry`; bump `SpRev`. Idempotent. A disabled cntlr leaves
primary eligibility and its namespaces go ANA-inaccessible; disabling the current
primary triggers the §10.4 primary re-election; disabling the last enabled cntlr is
allowed but stops IO (dnvctl prints a warning). Reply `cntlr_id`, `enabled`.

**InspectCntlr** — read the `Cntlr` in an STM for its `cn_addr_port`; outside the STM
call `ControllerNodeAgent.GetCntlrInfo(cluster_id, cn_id, sp_id, cntlr_id)`; agent
failure ⇒ `ABORTED`. Reply `CntlrInfo`.

**InspectSide** — locate the side by scanning the SP's slices in the STM (bounded by
`MaxSliceCntPerSp × groups × MaxLegPerGrp`), get its DN; outside the STM call
`DiskNodeAgent.GetSideInfo(cluster_id, dn_id, side_pointer)`. Reply `SideInfo`. Both
Inspect RPCs are the way to watch clone/migration hydration before
`DeleteClone`/`FinishMigration`.

### 8.7 Thin devices

**CreateThinDevice** —
Errors: `ALREADY_EXISTS` td key; `RESOURCE_EXHAUSTED` `len(td_name_list) ≥
MaxTdCntPerSp`; `NOT_FOUND` `ori_name` set but absent; `INVALID_ARGUMENT` `size == 0` or
`size` not a multiple of `slice_cnt × DmRaid0Conf.stripe_size` [E22].
Defaults: `ori_name = ""` (fresh device, not a snapshot); when `ori_name` is set and
`size == 0`, `size` = the origin's size (a snapshot may also be created larger).
Action: STM: `td_id` from `next_id`, `dev_id` from `next_dev_id++`,
`ori_id` = origin's `dev_id` or 0; write `ThinDevice`, append `td_name_list`, bump
`SpRev`. Reply `td_id`, `dev_id`. The primary creates one thin volume per slice
(`create_thin` / `create_snap` pool message) + the raid0/error/ns-dev of §3.3.

**DeleteThinDevice** —
Errors: `FAILED_PRECONDITION` if the `td_id` is referenced by any `Namespace.td_id` of
any subsystem of the SP (read `nqn_list` + every `Subsystem` in the same STM — bounded
by `MaxSsCntPerSp × MaxNsCntPerSs`, cheap) or by any `Clone.dst_td_id`.
Action: STM: remove from `td_name_list`, delete the key, bump `SpRev`. `dev_id` is never
reused. Deleting the origin of snapshots is allowed — dm-thin snapshots stay valid. The
primary applies it with a `delete {dev_id}` message per slice pool. Reply `td_id`.

**ListThinDevices** — one STM reads `SpConf` + every td in `td_name_list` into
`name_to_td`; a missing listed key ⇒ `ABORTED`.

### 8.8 Subsystems, namespaces

**CreateSubsystem** —
Errors: NQN rules of §7; `ALREADY_EXISTS` subsystem key; `RESOURCE_EXHAUSTED` at
`MaxSsCntPerSp`; `INVALID_ARGUMENT` `len(allowed_hosts) > MaxHostCntPerSs` or any entry
fails the NQN rules.
Action: STM: `ss_id` from `next_id`; `serial = sprintf("%016x", ss_id)`,
`model = "dnv"` [E23]; write `Subsystem` (empty `ns_list`), append `nqn_list`, write
`CdcEntry{nqn, nvme_tr_conf_list = tr confs of every enabled cntlr's CN,
allowed_hosts}`, bump `SpRev`. Reply `ss_id`.

**DeleteSubsystem** — Errors: `FAILED_PRECONDITION` `ns_list` non-empty (allowed_hosts
never block, they go implicitly). Action: STM remove from `nqn_list`, delete `Subsystem`
+ `CdcEntry`, bump `SpRev`. dnv-cdc sends a discovery-log-change AEN; hosts running
nvme-stas disconnect automatically. Reply `ss_id`.

**ListSubsystems** — STM read of `nqn_list` + each `Subsystem` into `nqn_to_subsystem`.

**UpdateSubsystemHosts** — STM update `Subsystem.allowed_hosts` and
`CdcEntry.allowed_hosts`, bump `SpRev`. Reply `ss_id`.

**CreateNamespace** —
Errors: `NOT_FOUND` nqn/td; `RESOURCE_EXHAUSTED` `len(ns_list) ≥ MaxNsCntPerSs`;
`INVALID_ARGUMENT` `ns_idx == 0`, `ns_idx` already used in this subsystem.
Action: STM: `ns_id` and `ag_id` from `next_id`; generate `dev_uuid` (RFC 4122 v4) and
`dev_nguid` (16 random bytes, hex) [E24]; append
`Namespace{ns_id, ns_idx, td_id, ag_id, dev_uuid, dev_nguid, suspended = false}` to the
subsystem, bump `SpRev`. Reply `ns_id`. `ns_idx` is the NVMe NSID; `ag_id` the ANA group
(one per namespace so states flip independently).

**DeleteNamespace** — STM remove the `ns_idx` entry, bump `SpRev`. Reply `ns_id`.

**UpdateNamespaceDev** — repoint the namespace at another td (`td_name`), e.g. to expose
a snapshot in place of the origin. STM update `td_id`, bump `SpRev`; agents reload the
nvmet namespace onto the new td's `CnNsDevName`. Reply `ns_id`.

**UpdateNamespaceSuspended** (rpc name `UpdatenamespaceSuspended` [E25]) — STM set
`suspended`, bump `SpRev`. Suspended ⇒ every cntlr suspends the td's `CnNsDevName` and
sets the ns ANA group `inaccessible`; resumed ⇒ reverse. Used by the transfer/clone
choreography of §11.3. Reply `ns_id`.

### 8.9 Clones (destination side of a copy; `090Clone`)

**CreateClone** —
Errors: `ALREADY_EXISTS` clone key; `RESOURCE_EXHAUSTED` at `MaxCloneCntPerSp`;
`NOT_FOUND` `dst_td_name`; `FAILED_PRECONDITION` the dst td already targeted by another
clone; `INVALID_ARGUMENT` when `src_tr_conf` is empty, `src_nqn` invalid,
`src_slice_cnt ∉ [1,16]`, `src_stripe_size ≠ i×4KiB (1 ≤ i ≤ 256)`,
`src_block_size ≠ j×64KiB (1 ≤ j ≤ 16384)`, or `src_block_size` not a multiple of
`src_stripe_size` (§11.4 constraints).
Defaults: `dm_clone_conf` per §7; `auto_resume` as sent.
Action: STM: `clone_id` from `next_id`, `bm_cnt = 0`; write `Clone`, append
`clone_name_list`, bump `SpRev`. Reply `clone_id`. Primary-cntlr behavior — connect to
the source (`src_tr_conf_list` + `src_nqn`, hostnqn `CnHostNqn`, retry until it
succeeds), `lvcreate` `CnCloneMetaName` in the clone VG, build dm-clone
`CnCloneFinalName` (metadata = that LV, dest = the dst td's `CnRaid0Name`, source = the
connected nvme ns at `src_ns_idx`, region size = this SP's `data_block_size`,
`hydration_threshold`/`batch_size` from `dm_clone_conf`), reload `CnNsDevName` onto the
dm-clone, and, iff `auto_resume`, resume the device and move the td's namespace(s) to
ANA `optimized` — i.e. steps 1-5 of §11.3's destination list.

**DeleteClone** —
Errors: `FAILED_PRECONDITION` when `force = false` and hydration is not complete —
checked outside the STM via `GetCntlrInfo` of the primary
(`clone_id_to_dm_clone.details` carries the dm-clone status; unreachable agent also ⇒
`FAILED_PRECONDITION`).
Action: STM: remove from `clone_name_list`, delete `Clone` + every `CloneBitmap`, set
`suspended = false` on every namespace whose `td_id == dst_td_id`, bump `SpRev`. The
primary reloads `CnNsDevName` back onto the raid0 (all data now local), removes the
dm-clone + metadata LV, disconnects the source. Reply `clone_id`.

**GetClone** — STM read. **UpdateCloneTrConf** — STM replace `src_tr_conf_list`, bump
`SpRev` (used when the source SP's cntlrs moved); the primary reconnects.

**AppendCloneBitmap** —
Errors: `INVALID_ARGUMENT` `slice_idx ≥ Clone.src_slice_cnt` or `≥ MaxCloneBmCnt`;
`INVALID_ARGUMENT` empty bitmap.
Action: STM: append the bytes to `CloneBitmap` key `bm_idx = slice_idx` (create if
absent), `bm_cnt = max(bm_cnt, slice_idx+1)`, bump `SpRev`. Bit semantics: bit *k* of
source slice `slice_idx` covers `src_block_size` bytes of that slice's local address
space; **1 = the source never wrote there ⇒ skippable**. The chunk-append exists because
callers page the source bitmap via `GetThinDeviceBitmap` and because etcd values are
size-limited; §11.4 defines how the agent turns these into dm-clone `blkdiscard`s.

### 8.10 Transfers (source side of a copy; `100Transfer`)

**CreateTransfer** —
Errors: `ALREADY_EXISTS`; `RESOURCE_EXHAUSTED` at `MaxXferCntPerSp`; `NOT_FOUND`
`ori_nqn` / `ori_ns_idx`; `INVALID_ARGUMENT` host-NQN rules on `allowed_hosts`
(callers put the destination cntlrs' `CnHostNqn`s here).
Action: STM: `xfer_id` from `next_id`, write `Transfer`, append `xfer_name_list`, bump
`SpRev`. Reply `xfer_id`. Every enabled cntlr creates the xfer stack: primary builds
`CnXferFinalName` = dm-linear on the origin td's raid0 and exports it as subsystem
`XferNqn` (nsid = `ori_ns_idx`, same `nguid`/`uuid` as the origin namespace,
`allowed_hosts` from the record, own port, `Cntlr.cntlid_slot` bounds); standbys export
the same subsystem backed by a dm-error `CnXferFinalName` with ANA `inaccessible`
(`100Transfer` right half). Iff `auto_suspend`, the primary first (1) moves the origin
namespace to ANA `inaccessible` on every cntlr, (2) suspends the origin td's
`CnNsDevName` — the destination clone is now the only reader/writer of the bytes.

**DeleteTransfer** —
Semantics: `force = false` **finalizes** a completed hand-over: the STM additionally
sets `suspended = true` on the origin namespace, so the source stays retired after the
xfer object disappears. `force = true` **aborts**: `suspended` is left `false`, so the
next syncup restores normal service (resume dev, ANA back).
Action: STM: remove from `xfer_name_list`, delete `Transfer`, (maybe) flip `suspended`,
bump `SpRev`. Agents drop the xfer subsystem + dm devices. Reply `xfer_id`.

**GetTransfer** — STM read. **UpdateTransferHosts** — STM replace `allowed_hosts`, bump
`SpRev` (destination cntlr moved to another CN).

### 8.11 Migrations (`080Migration`)

**CreateMigration** —
Errors: `NOT_FOUND` `src_side_id` not found in any leg of the SP; `RESOURCE_EXHAUSTED`
at `MaxMigrCntPerSp` or no DN candidate (§6.5); `FAILED_PRECONDITION` the owning leg
already has 2 sides (a migration is already running on it).
Action: allocate one DN; STM: `migr_id` + `dst_side_id` from `next_id`; append a new
`Side` to the leg's `side_list` (`cntlid_slot` = first slot in `cntlid_slot_list`
different from the src side's, §11.7; fresh port on the dst DN [E11]); write
`Migration{migr_id, src_side_id, dst_side_id, dm_clone_conf, bm_cnt = 0}`, append
`migr_name_list`; dst-DN bookkeeping (`side_ptr_list`, `free_ext_cnt -= group.ext_cnt`,
capacity move, `DnRev`); bump `SpRev`. Reply `migr_id`. Data-plane choreography: §11.2.

**FinishMigration** —
Errors: `FAILED_PRECONDITION` when `force = false` and hydration is incomplete
(checked outside the STM via `GetSideInfo` of the **destination** side —
`migr_dm_clone.details`).
Action: STM: remove the **src** `Side` from the leg (the dst side becomes the only
one); src-DN bookkeeping (pointer out, extents back, capacity move, `DnRev`); delete
`Migration` + every `MigrBitmap`, remove from `migr_name_list`; bump `SpRev`. The dst
side agent reloads its per-cntlr dm-linears from the dm-clone straight onto the LV,
drops the dm-clone + metadata LV + nvme host connection; the src DN agent sees the
pointer disappear and tears the side down. Reply `migr_id`.

**CancelMigration** — mirror rollback: STM removes the **dst** `Side` + `Migration` +
bitmaps, returns the dst DN's extents, bumps `SpRev` + dst `DnRev`. The src side
returns to normal service on the next `SyncupSide` (ANA back, migr export dropped).

**GetMigration** — STM read.

**AppendMigrationBitmap** —
Errors: `RESOURCE_EXHAUSTED` `bm_cnt ≥ MaxMigrBmCnt`.
Action: STM: write `MigrBitmap` at `bm_idx = bm_cnt`, `bm_cnt += 1`, bump `SpRev`. The
chunks concatenate (in `bm_idx` order) into one bitmap over the **leg's data region**:
bit *k* covers `data_block_size` bytes, **1 = never written ⇒ skippable**. The dst side
agent shifts by the leg's `meta_blocks` (an LV also holds the md superblock region,
which must always be copied) and `blkdiscard`s fully-skippable dm-clone regions [E13].

### 8.12 Spare legs

**CreateSpareLeg** — Errors: `NOT_FOUND` `grp_id` [E26] not in the SP;
`RESOURCE_EXHAUSTED` at `MaxSpareLegPerGrp` or no DN (§6.5). Action: STM: new
`Leg{leg_id, leg_idx = next unused idx in the group, one Side}` appended to
`spare_leg_list`; DN bookkeeping + `DnRev`; bump `SpRev`. The primary `mdadm --add`s it
as a hot spare (RedundMdRaid1 only; `INVALID_ARGUMENT` for RedundNone groups). Reply
`leg_id`.

**DeleteSpareLeg** — Errors as above; `leg_id` selects which spare [E26]. Action: STM
remove from `spare_leg_list`, DN bookkeeping back, bump `SpRev`. Reply `leg_id`.

**SwitchSpareLeg** — Errors: `NOT_FOUND` ids not in the group's lists. Action: STM swap:
`spare_leg_id` moves to `leg_list` (taking the active role), `target_leg_id` moves to
`spare_leg_list`; bump `SpRev`. The primary fails/removes the target from the md array
if still present and lets the (already-added) spare rebuild. Reply the current active +
spare leg ids. Used with §10.4's `leg_unhealthy` handling.

### 8.13 Bitmap reads

**GetThinDeviceBitmap** — inputs `td_name`, `slice_idx`, `start_block`, `block_cnt`.
The gateway resolves the primary cntlr's CN in an STM, then (outside) asks its agent
[E27] to compute, from a dm-thin metadata snapshot of that slice's pool, the mapping
bitmap of the td's thin volume in that slice: bit *k* (for block `start_block + k`,
block = `data_block_size`) = **1 iff unmapped/never written**. `block_cnt = 0` ⇒ to the
end. Callers page through it and feed `AppendCloneBitmap` on the destination.

**GetLegBitmap** — inputs `leg_id`, `start_block`, `block_cnt`. Same path [E27]; the
agent walks the pool metadata of the owning slice, translates pool-data blocks through
the pool-data linear concat and the group geometry down to this leg's data region, and
returns bit *k* = **1 iff no pool block maps there**. Callers feed
`AppendMigrationBitmap`.

---

## 9. Agent services and agent behavior

### 9.1 Common agent rules

* `dnv-agent dn|cn` persists, in the sqlite DB at `--local-store`, per synced object
  (the node itself; each side / cntlr): the last applied `revision`, the serialized
  request that carried it, the local port-bitmap allocations [E11], and the set of
  applied bitmap indexes per clone/migration. On start it loads everything and
  re-applies it to the system before serving (full idempotent reconcile).
* **Revision gate.** A `Syncup*`/`Push*` request with a revision **lower** than the
  stored one is rejected (`AgentReply.code != 0`, `details` explains). Equal revision:
  re-apply idempotently (workers retry).
* **`partial` gate.** `partial = true` means "only add/refresh what is in this request;
  delete nothing that is absent". It is accepted only when `request.revision ==
  local.revision` or `request.revision == local.revision + 1`; otherwise reject — the
  agent may have missed deletions and must get a full (non-partial) request first.
  Workers use `partial` to make failover / clone / transfer / migration pushes fast.
* **Full-sync diff.** On a full (`partial = false`) `SyncupDn`: pointers in the request
  but not local ⇒ add; local but not in the request ⇒ move to a to-be-deleted list and
  tear their resources down (ids are never reused, so a deleted side/cntlr never comes
  back). Same logic for `SyncupCn` with cntlr pointers, and inside
  `SyncupSide`/`SyncupCntlr` for their embedded object lists.
* Every reply embeds `AgentReply{code, details}` (0 = OK) and the current `*Info`
  (§9.5). All four `Syncup*` and both `Push*` RPCs are bidirectional streams: one
  request message yields one reply message; the worker keeps the stream open and reuses
  it for successive revisions of the same target.
* Shell execution uses the §7 timeouts; failures are captured into the affected
  resource's `ResInfo{status = RES_STATUS_ERROR, details}` rather than crashing the
  reconcile — the agent always converges as much as it can and reports the rest.

### 9.2 `service DiskNodeAgent`

| rpc | behavior |
|---|---|
| `GetDnSize` | Return the byte size of the `--disk` block device (`lsblk --bytes`). Called by the gateway pre-registration; `dn_id` in the request is for logging only. |
| `SyncupDn` (stream) | Carries `revision`, `cluster_conf`, `partial`, `side_pointer_list`. Ensure §3.1 base state (PV/VG, `migr-pv` + migration VG); diff the pointer list per §9.1; reply `DnInfo`. |
| `SyncupSide` (stream) | Carries one `side_pointer`, `revision`, `partial`, `ext_cnt`, `primary_cn_id`, `primary_idx`, `standby_id_to_idx` (cn_id → cntlr_idx), and — when this side is a migration endpoint — `migration` + `src_side`. Reject if the pointer is unknown (SyncupDn must introduce it first). Converge the §3.1 per-side stack: LV of `ext_cnt` extents (§9.4), per-CN dm-error/dm-linear/nvmet subsystem, primary vs standby table targets + ANA states, migration source/destination roles (§11.2). Reply `SideInfo` + `bm_info_list` (applied migration bitmap indexes). |
| `PushMigrBitmap` (stream) | Deliver one `MigrBitmap` chunk (`migr_id`, `bm_idx`, `bitmap`) at `revision` (gates of §9.1 apply). The **destination**-side agent records it, and once per new chunk recomputes + `blkdiscard`s the fully-skippable dm-clone regions (§8.11, §11.4); already-applied `bm_idx`es are remembered and skipped. |
| `GetDnInfo` / `GetSideInfo` | Return the current `DnInfo` / `SideInfo` without changing anything. |

### 9.3 `service ControllerNodeAgent`

| rpc | behavior |
|---|---|
| `GetCnSize` | Return the capacity budget in bytes this CN is willing to host (0 = "use the default"); typically from local config. |
| `SyncupCn` (stream) | `revision`, `cluster_conf`, `partial`, `cntlr_pointer_list`. Ensure §3.2 base state (tmpfs, loop, clone VG, port pool); diff pointers; reply `CnInfo`. |
| `SyncupCntlr` (stream) | One `cntlr_pointer`, `revision`, `partial`, `bdev_conf`, `cntlr`, `id_to_slice` (key = `sprintf(IdKeyFmt, slice_id)`), `td_list`, `nqn_to_subsystem`, `clone_list`, `xfer_list`, `migr_list`. Converge §3.3 (primary) or §3.4 (standby); a primary→standby or standby→primary flip follows §11.1 exactly. Reply `CntlrInfo` + `bm_info_list` (applied clone bitmap indexes). |
| `PushCloneBitmap` (stream) | Like `PushMigrBitmap` for clone bitmaps: the primary translates them through §11.4 and `blkdiscard`s the dm-clone. |
| `GetCnInfo` / `GetCntlrInfo` | Read-only live state. |

`SyncupCntlr` also implicitly defines what the agent needs for bitmap reads: the
gateway's `GetThinDeviceBitmap`/`GetLegBitmap` are served by the same process from the
pool metadata [E27].

### 9.4 LV provisioning protocol (trim tags)

To guarantee a new side never leaks a previous tenant's data, DN agents create LVs in
three idempotent steps (restart-safe at every point):

1. `lvcreate --addtag not_trimmed --name {DnLvName} --extents {ext_cnt} {DnVgName}`
2. `blkdiscard --force {DnLvPath}`
3. `lvchange --deltag not_trimmed --addtag trimmed {DnLvPath}`

An LV carrying `not_trimmed` is never exported; on restart the agent redoes 2-3.
(`lvs --report-format json --options lv_name,lv_tags` is the probe.)

### 9.5 Live-state reporting

`ResInfo{res_name, status, details, epoch}` with `ResStatus`: `UNKNOWN`(no answer from
the node — set by *workers*, never by the agent itself), `MISSING`(agent hasn't created
it), `ERROR`(tried and failed; `details` = why / command output), `OK`. `epoch` = unix
seconds of the last status change. `revision` in each `*Info` = last fully applied
revision. Map keys in `SideInfo`/`CntlrInfo` are the obvious owner ids (cntlr_id, td_id,
slice_id, grp_id, leg_id, ns_id, ss_id, clone_id, xfer_id). dm-clone `details` strings
carry the raw `dmsetup status` line so hydration progress is visible through
`Inspect*`.

---

## 10. Workers

### 10.1 Membership and shard ownership

Each `dnv-worker` instance, per role it carries, registers
`{p} worker {role} {worker_uuid}` bound to an etcd lease and watches the registry.
Shard-code ownership uses rendezvous (HRW) hashing: for shard `s`, owner =
`argmax over live workers w of fnv64a(worker_uuid ∥ role ∥ s)` — deterministic on every
worker, no coordinator, and adding/removing a worker only moves ~1/n of the shards
(matching the "third worker takes ~1/3" behavior in the source description). A worker
acts only on shards it currently owns and re-evaluates on every membership change.

### 10.2 dn / cn roles

For each owned shard `s`, watch prefix `{p} dn_rev {s} ` (resp. `cn_rev`). On a `put`:
read the node's desired state (`DnConf` + `ClusterConf`; the sides' details are pushed
by the sp role) and call `SyncupDn` (resp. `SyncupCn`) with that revision. On a
`delete`: stop syncing/health-checking the node. Additionally poll owned nodes
(`GetDnInfo`/`GetCnInfo`, suggested every few seconds): on failure or bad status set
`DnConf.err_epoch`/`CnConf.err_epoch` = now (if 0) in an STM; clear to 0 on recovery.
`err_epoch` changes do **not** bump revisions.

### 10.3 sp role

Watch `{p} sp_rev {s} ` per owned shard. On a bump: load the SP (SpConf, cntlrs,
slices, tds, subsystems, clones, xfers, migrs, bitmaps) and fan out `SyncupSide` to
**every side** of the SP and `SyncupCntlr` to **every cntlr**, with the new revision.
Use `partial = true` whenever the delta is additive (failover role flip, new
clone/transfer/migration, new bitmaps — for bitmaps prefer the cheap `Push*Bitmap`
RPCs, skipping indexes already acknowledged in `bm_info_list`); fall back to a full
sync when the agent rejects the partial. The replies' `*Info` also feed health: set/clear
`err_epoch` on `Cntlr`, `Leg`, `Side` records accordingly (STM, no rev bump).

### 10.4 Automatic reactions (`EventThreshold`)

All comparisons are `now − err_epoch ≥ threshold` with the SP's `event_threshold`
(0-valued fields fall back to the §7 defaults). All actions are ordinary STM mutations
that bump `SpRev`, so the data-plane choreography is the same as for the equivalent
manual RPC:

* `primay_unhealthy` (5 s): the primary cntlr is unhealthy ⇒ pick the healthy, enabled
  cntlr with the smallest `cntlr_idx`, flip the `primary` booleans. This *is* the
  failover trigger of §11.1.
* `cntlr_unhealthy` (600 s): a (non-primary, or already-failed-over) cntlr stays
  unhealthy ⇒ replace it: internal `DeleteCntlr` (skipping the enabled check) + internal
  `CreateCntlr` on a fresh CN with the same `cntlid_slot`.
* `side_unhealthy` (600 s): a side stays unhealthy ⇒ start an internal migration of its
  leg to a fresh DN (§8.11); a pre-existing healthy migration of that leg is left alone.
* `leg_unhealthy` (1200 s): a leg stays unhealthy ⇒ if the group has a spare, perform an
  internal `SwitchSpareLeg`; otherwise create a spare on a fresh DN first, wait for the
  rebuild, then switch.
* `pool_low_water_mark_pct`: not a worker action — it is compiled into the thin-pool
  table (§3.3) so the kernel emits a dm event; the agent surfaces it as a
  `RES_STATUS_ERROR`-with-details on the pool's `ResInfo`, and `dnvmon`/operators react
  with `GrowSlice`.

The worker never deletes user data on its own; every automatic action above only
re-homes redundancy or roles.

---

## 11. Procedures

### 11.1 Failover

An SP has multiple cntlrs; exactly one is primary. A failover switches the primary from
one cntlr (`old_primary`) to another (`new_primary`) and involves three kinds of
participants. The trigger is only ever an etcd change (§10.4 or `UpdateCntlrEnabled`),
delivered by revision-ordered syncups.

**old_primary** — on a `SyncupCntlr` saying it is not primary (revision higher than
everything it has seen):

1. Move all namespaces from the `optimized` ANA group state to `inaccessible`.
2. Wait until no inflight IO remains on the ns-dev layer.
3. Reload every td's `CnNsDevName` dm-linear onto its `CnErrorName`.
4. Clean up every resource a primary shouldn't have (raid0s, thin volumes, pools,
   pool-meta/data linears, md arrays) — leaving the §3.4 standby set.

**new_primary** — on a `SyncupCntlr` saying it is primary (highest revision):

1. Make sure all groups are available (below).
2. Create the pools, thin volumes and raid0 devices (§3.3 steps 3-5).
3. Reload every td's `CnNsDevName` from `CnErrorName` onto its raid0 (or dm-clone,
   for an in-flight clone).
4. Move all namespaces from `inaccessible` to `optimized`.

**"Make sure all groups are available".** Example: an SP with 2 slices, each slice 1
meta + 2 data groups, `RedundMdRaid1` (2 legs/group) needs md-raid1 devices
`slice{0,1}-{meta-grp0, data-grp0, data-grp1}`, each over 2 leg (nvmeof) devices. Cases
when creating one raid1:

1. Both legs available:
   1. Neither has an md superblock ⇒ `mdadm --create` (with `--assume-clean` only when
      both legs are freshly trimmed).
   2. Exactly one has a superblock ⇒ `mdadm --assemble` with that leg, then
      `mdadm --add` the other.
   3. Both have superblocks ⇒ `mdadm --assemble` with both, then `mdadm --detail`; if
      one leg was left out for stale metadata, `mdadm --add` it again.
2. One leg available ⇒ `mdadm --assemble` **without** `--run`; mdadm decides:
   success ⇒ the group is available (degraded), failure ⇒ it is not.
3. No leg available ⇒ the group is not available.

A group is available in cases 1 and 2-success. **A leg is available** iff it has an
`optimized` path: normally its single side's export toward this CN is optimized; a
`non-optimized` path means the side currently exports dm-error and cannot be used.
During a migration the leg has a src and a dst side whose states pair as
`non-optimized`/`inaccessible` or `optimized`/`inaccessible`; any `optimized` state
makes the leg available (§3.3 step 1, [E10]).

**sides** — on a `SyncupSide` (highest revision) showing a changed primary:

1. Move the old primary CN's subsystem from `optimized` to `non-optimized`.
2. Suspend the old primary CN's dm-linear.
3. Reload the new primary CN's dm-linear so it sits on the LV.
4. Move the new primary CN's subsystem from `non-optimized` to `optimized`.
5. Sleep `SideSwitchWait` = 300 s (grace so the old primary has drained, §11.1 step 2).
6. Reload the old primary CN's dm-linear back onto its dm-error and resume it.

### 11.2 Migration (side → side; `080Migration`)

Starting a migration resembles a failover; the leg temporarily owns two sides.

**src side** (driven by its `SyncupSide` carrying the `Migration`):

1. Move every per-CN subsystem's namespace to `inaccessible` (all cntlrs).
2. Suspend every per-CN dm-linear.
3. Build `DnMigrSrcName` (linear on the LV) and export it via `MigrSrcNqn`,
   `allowed_hosts = [DnHostNqn(cluster, dst_dn)]`.

**dst side** (its `SyncupSide` carries `Migration` + `src_side`):

1. Create the per-CN subsystems/namespaces with dm-error backing, all `inaccessible`.
2. `lvcreate` `DnMigrMetaName` in the migration VG (metadata for the dm-clone).
3. `nvme connect` to `MigrSrcNqn` at `src_side.nvme_tr_conf` (hostnqn `DnHostNqn`),
   retrying until success.
4. Create the dm-clone `DnMigrFinalName`: metadata = step 2's LV, dest = the local side
   LV, source = the connected nvme device, region size = the SP's `data_block_size`,
   hydration knobs from `Migration.dm_clone_conf`.
5. Reload the **primary** CN's dm-linear onto the dm-clone; set that subsystem
   `optimized`, all other CNs' `non-optimized`.

IO now flows host → primary cntlr → dst side (dm-clone pulls missing regions from the
src on demand and hydrates in the background). The optional bitmap fast-path:
`GetLegBitmap` (paged) → `AppendMigrationBitmap` → worker `PushMigrBitmap` → dst agent
`blkdiscard`s never-written regions so they are never copied (§8.11, §8.13, §11.4).
`FinishMigration` / `CancelMigration` per §8.11.

### 11.3 Transfer + clone = cross-SP live migration (`090Clone`, `100Transfer`)

A clone's source can be *any* NVMe-oF namespace; when the source is a dnv transfer the
pair performs live migration of a volume between SPs (even across clusters). Setup for
moving ss/ns from `sp1` to `sp2`:

* `sp2` gets an identical subsystem (same NQN) and namespace (same `ns_idx`, and the
  same `nguid`/`uuid` [E24]) on a fresh thin device, so hosts aggregate `sp1`'s and
  `sp2`'s exports into one multipath device.
* `sp1` and `sp2` cntlrs MUST use disjoint `cntlid_slot`s (≤ 4 cntlrs each, 8 slots —
  exactly enough). If not, rotate a cntlr: `UpdateCntlrEnabled(false)` → `DeleteCntlr`
  → `CreateCntlr` with a free slot. Likewise the two SPs' cntlrs MUST NOT share CNs;
  rotate the same way if they do.
* Ensure `Namespace.suspended = false` on sp1 and `= true` on sp2
  (`UpdateNamespaceSuspended`).
* Collect the sp2 cntlrs' CN ids ⇒ `CnHostNqn` list.
* `CreateTransfer` on sp1 (`ori_nqn`/`ori_ns_idx`, `allowed_hosts` = that list,
  `auto_suspend = true`) — the sp1 primary then: (1) origin ns → `inaccessible`,
  (2) suspend its `CnNsDevName`, (3) create `CnXferFinalName` on the raid0, (4) export
  it via `XferNqn` per the Transfer.
* `CreateClone` on sp2 (`src_tr_conf_list` = sp1 cntlr endpoints, `src_nqn` =
  the transfer's `XferNqn`, `src_ns_idx = ori_ns_idx`, the source geometry
  `src_slice_cnt`/`src_stripe_size`/`src_block_size` = sp1's slice count / raid0 stripe
  / pool block size, `dst_td_name`, `auto_resume = true`) — the sp2 primary then:
  (1) connect to the targets, (2) create `CnCloneMetaName`, (3) create
  `CnCloneFinalName`, (4) reload `CnNsDevName` onto it and resume, (5) origin
  namespace → `optimized`. Hosts flip to sp2 transparently.
* Optional bitmap fast-path: per sp1 slice `GetThinDeviceBitmap` (paged) →
  `AppendCloneBitmap(slice_idx, …)` on sp2 → `PushCloneBitmap` → sp2 primary
  `blkdiscard`s (§11.4).
* When hydration completes: `DeleteTransfer(force=false)` on sp1 (retires the source:
  `suspended = true`) and `DeleteClone` on sp2 (`suspended = false`, ns now backed by
  the local raid0). To abort instead: `DeleteClone(force=true)` on sp2 and
  `DeleteTransfer(force=true)` on sp1.

### 11.4 raid0 bitmap math

Definitions for one raid0 device: `slice_cnt` underlying devices, `stripe_size` chunk
size, and per-underlying-device write bitmaps whose bit granularity is `block_size`.
Constraints (validated in §8.9): `1 ≤ slice_cnt ≤ 16`; `stripe_size = i × 4 KiB,
1 ≤ i ≤ 256`; `block_size = j × 64 KiB, 1 ≤ j ≤ 16384`; `block_size = k × stripe_size,
k ≥ 1`. Address mapping for logical byte offset `off`: chunk `c = off / stripe_size`,
slice `= c mod slice_cnt`, slice-local offset `= (c div slice_cnt) × stripe_size +
(off mod stripe_size)`.

Worked example (`slice_cnt = 4`, `stripe_size = 16 KiB`, `block_size = 1 MiB`; here the
16 KiB chunks are drawn as 4 × 4 KiB rows for compactness): writing the 1st, 3rd, 7th
and 8th 4 KiB units of the logical device sets, in *written = 1* convention, bit 0 of
slice 0's bitmap, bits 0-1 of slice 2's, bit 1 of slice 3's, and nothing in slice 1's.

**Skip-bitmap function.** Given source geometry `A` (`slice_cnt_A`, `stripe_size_A`,
`block_size_A`, per-slice bitmaps, *written = 1*) and destination geometry `B`
(idem, bitmaps *copied = 1*, all-zero on first run), with the extra constraint that the
larger of the two stripe sizes is an integer multiple (`m > 1`) of the smaller, define
`region_size = min(stripe_size_A, stripe_size_B)` — so any region is contiguous inside
one chunk on **both** sides — and `region_cnt = min(size_A, size_B) / region_size`.
Output: a bitmap of `region_cnt` bits where bit `r` = 1 ⇔ region `r` need **not** be
copied ⇔ `NOT writtenA(r) OR copiedB(r)`, where `writtenA(r)` ORs and `copiedB(r)` ANDs
the covered source/destination bitmap bits after mapping region `r`'s byte range through
each side's address mapping. Consumers:

* `dnvctl admin clone` — a restartable userspace copier that reads/writes only the two
  top raid0 devices in `region_size` units, skipping 1-bits; B's bitmaps advance
  automatically as it writes, so a crash simply re-runs the function. It may copy some
  zeroes (over-copy is fine); it must never skip written source data.
* The **agents** applying `CloneBitmap`s/`MigrBitmap`s: same math with no B term
  (dm-clone tracks its own hydration) and `region_size` = the dm-clone region
  (= destination `data_block_size`, which may exceed `stripe_size_A` — then a region
  spans several source slices and `writtenA` simply ORs across all of them);
  `blkdiscard` region `r` ⇔ `NOT writtenA(r)`. Note the Gateway bitmap RPCs and the
  `Append*` payloads use the inverted *unwritten = 1* convention (§8.9/§8.11/§8.13);
  agents flip it once on ingest. For migrations, first shift by `meta_blocks` (§8.11).

### 11.5 Namespace suspend semantics

`suspended = true` ⇔ every cntlr keeps the td's `CnNsDevName` dm-suspended and the ns
ANA group `inaccessible`; `false` ⇔ normal §3.3/§3.4 behavior. Set by users
(`UpdateNamespaceSuspended`) and by the transfer/clone finalization (§8.9/§8.10).

### 11.6 SpLevel (see §8.4 UpdateStoragePoolLevel)

Levels gate agent behavior top-down for disaster recovery: each step removes one more
fragile layer until `SP_LEVEL_DISABLE` leaves only the LVs. Agents treat the level as
part of desired state: raising it tears layers down, lowering it rebuilds them.

### 11.7 cntlid slots

NVMe multipath requires distinct CNTLIDs among the controllers a host aggregates. dnv
partitions the cntlid space via
`/sys/kernel/config/nvmet/subsystems/{nqn}/attr_cntlid_{min,max}` into
`CnCntlidSlotCnt = DnCntlidSlotCnt = 8` slots, `Base = 10000`, `Step = 5000`, on both
CNs and DNs: slot *s* ⇒ `attr_cntlid_min = 10000 + s×5000`,
`attr_cntlid_max = attr_cntlid_min + 5000` (slot 0 = 10000-15000, … slot 7 =
45000-50000). Rules: every cntlr of an SP uses a distinct slot (host-facing multipath);
a side uses one slot for all its per-CN exports; the two sides of a migrating leg use
distinct slots (the CN sees both as paths of one leg); two SPs joined by
transfer+clone must use disjoint slot sets across their cntlrs (§11.3). All slots come
from `SpConf.cntlid_slot_list`.

---

## 12. dnv-cdc

`dnv-cdc --etcd-endpoints … --range 0,1,…` serves the NVMe-oF Central Discovery
Controller (well-known NQN `nqn.2014-08.org.nvmexpress.discovery`) to hosts. `--range h`
claims every shard code whose first hex digit is `h` (range `0` = shards `00…0f`, range
`1` = `10…1f`, …), i.e. it watches the `{p} cdc ` prefix and filters on the
`{shard_code}` key field. For each `CdcEntry` it advertises discovery log entries
(`nqn` × every `NvmeTrConf` in `nvme_tr_conf_list`) to hosts whose hostnqn is in
`allowed_hosts` (empty ⇒ everyone), and emits discovery-log-change AENs on any watched
change — hosts running `nvme-stas` then connect/disconnect automatically (this is what
makes `DeleteSubsystem`, `CreateCntlr`, `UpdateCntlrEnabled` transparent to hosts).
Deploy ≥ 2 instances with complementary ranges; hosts are configured with all cdc
endpoints.

---

## 13. Components: invocation reference

```shell
dnv-gateway --grpc-network tcp --grpc-address 192.168.0.20:29527 \
  --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379

dnv-worker --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379 \
  --roles dn,cn,sp

dnv-agent dn --grpc-network tcp --grpc-address 192.168.0.20:29528 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 192.168.0.20 --tr-svc-id 4200 \
  --local-store /path/to/sqlite.db \
  --disk /dev/disk/by-uuid/4425c6a8-dc27-40a3-9fd5-0cc41f534360

dnv-agent cn --grpc-network tcp --grpc-address 192.168.0.20:29529 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 192.168.0.20 --tr-svc-id 4200 \
  --local-store /path/to/sqlite.db

dnv-cdc --etcd-endpoints ... --range 0,1,2,3,4,5,6,7
dnv-cdc --etcd-endpoints ... --range 8,9,a,b,c,d,e,f
```

Every flag is also settable via config file and environment (viper). The agent's
`--tr-*` flags seed the node's `NvmeTrConf` base; `--tr-svc-id` is the base for the port
pool of §3.1/§3.2 [E11]. `dnvctl` subcommand sketch: `dnvctl dn|cn|sp|vol create|…`,
`dnvctl admin update_etcd | move_dn | move_cn | clone` (the §11.4 copier).

---

## Appendix A — system command patterns

The vocabulary agents implement (verified patterns; JSON output modes are the ones to
parse). Substitute the §4 names.

**CN clone-VG bootstrap (tmpfs + loop):**
```shell
mkdir --parents {CnTmpfsPath}
mount --types tmpfs --options size=2G tmpfs {CnTmpfsPath}
dd if=/dev/zero of={CnTmpFilePath} bs=1M count=1024
losetup --find --show {CnTmpFilePath}          # or losetup /dev/loopN {path}
losetup --all --output NAME,BACK-FILE --json   # rediscovery after restart
pvcreate --norestorefile --uuid {uuid} --metadatasize 4M --dataalignment 8M /dev/loopN
vgcreate --yes --physicalextentsize 4M {CnCloneVgName} /dev/loopN
losetup --detach /dev/loopN                    # teardown
```

**DN VG + side LV lifecycle:**
```shell
pvcreate --yes {disk}
vgcreate --yes --physicalextentsize {extent_size} {DnVgName} {disk}
lvcreate --yes --addtag not_trimmed --name {DnLvName} --extents {ext_cnt} {DnVgName}
blkdiscard --force {DnLvPath}
lvchange --yes --deltag not_trimmed --addtag trimmed {DnLvPath}
lvs --yes --report-format json --options lv_name,lv_tags {DnLvPath}
lvremove --yes {DnLvPath}
# migration VG on top of the migr-pv LV:
lvcreate --yes --name migr-pv --size 1G {DnVgName}
pvcreate --yes {DnMigrPvPath}
vgcreate --yes --physicalextentsize 4M {DnMigrVgName} {DnMigrPvPath}
# probes:
lsblk --bytes --nodeps --json {dev}
pvs --yes --report-format json --options pv_name,pv_uuid {dev}
vgs --yes --report-format json --options vg_name {vg}
```

**md-raid1 (mask the distro udev rules once per CN, they fight incremental assembly):**
```shell
for f in $(ls /usr/lib/udev/rules.d/ | grep -w 'md-raid'); do
    ln -s /dev/null /etc/udev/rules.d/$f
done
mdadm --create /dev/md/{CnMdDevName} --run --name {CnMdArrayName} --level=1 \
      --raid-devices={leg_cnt} --data-offset=2048K --bitmap=internal \
      --bitmap-chunk={bitmap_chunk_block_cnt*data_block_size} --assume-clean {legs...}
mdadm --assemble /dev/md/{CnMdDevName} --name {CnMdArrayName} {legs...}   # no --run (§11.1)
mdadm --detail /dev/md/{CnMdDevName}
mdadm --manage /dev/md/{CnMdDevName} --add {leg}
mdadm --stop /dev/md/{CnMdDevName}
# wipe a leg's stale superblock when re-provisioning:
dd if=/dev/zero of={leg} bs=1M count=1 ; blkdiscard --force {leg}
```

**device-mapper / nvme (targets per §3):** `dmsetup create|reload|suspend|resume|
remove|status|message {name}` with tables `error`, `linear`, `striped` (raid0),
`thin-pool`, `thin`, `clone`; pool messages `create_thin {dev_id}`,
`create_snap {dev_id} {ori_id}`, `delete {dev_id}`; `blkdiscard --offset --length` for
bitmap skips. nvmet via configfs (`/sys/kernel/config/nvmet/…`: subsystems with
`attr_allow_any_host`, `attr_cntlid_min/max`, `attr_serial`, `attr_model`, namespaces
with `device_path`/`device_uuid`/`device_nguid`/`ana_grpid`, `ana_groups/{ag}/ana_state`
per port, `allowed_hosts`, ports with `addr_trtype/adrfam/traddr/trsvcid`); host side
`nvme connect --transport … --traddr … --trsvcid … --nqn … --hostnqn …` and
`nvme disconnect`.

**Handy id calculator** (kept from the source tree; prints
`cluster_name nodeId clusterId shortId`):
```go
package main

import ("fmt"; "hash/fnv")

func clusterNameToId(clusterName string) uint64 {
    h := fnv.New64a(); h.Write([]byte(clusterName)); return h.Sum64()
}
func getShortId(clusterId, nodeId uint64) uint64 {
    h := fnv.New64a()
    fmt.Fprintf(h, "%016x%016x", clusterId, nodeId)
    return h.Sum64() & 0x0000FFFFFFFFFFFF
}
func main() {
    for _, n := range []string{"default", "_default"} {
        c := clusterNameToId(n)
        fmt.Printf("%20s %016x %016x %012x\n", n, 12, c, getShortId(c, 12))
    }
}
```

---

## Appendix B — suggested implementation order

1. `common` package: constants, `NameFmt` (with the Appendix C fixes), key builders,
   cluster/short id hashing, validation helpers, STM wrapper, page tokens.
2. Generated protobuf/gRPC code (after applying the Appendix C schema fixes).
3. Gateway: clusters → nodes (§8.1-8.3, needs agent `GetDnSize`/`GetCnSize` stubs) →
   allocator (§6) → storage pools (§8.4-8.6) → tds/subsystems/ns (§8.7-8.8) →
   clone/transfer/migration/spare-leg/bitmap RPCs (§8.9-8.13).
4. dn agent (§9.2, §9.4, §3.1) then cn agent (§9.3, §3.2-3.4) — each first as
   "reconcile from a fed request", then wired to sqlite persistence and the gates.
5. Workers (§10): membership/HRW, dn/cn loops, sp fan-out, health, §10.4 reactions.
6. Procedures end-to-end tests: failover (§11.1), migration (§11.2), transfer+clone
   (§11.3), bitmap math (§11.4) as a pure library with unit tests first.
7. dnv-cdc (§12), dnvctl, dnvmon.

---

## Appendix C — errata: source inconsistencies and the resolutions adopted here

Semantic fixes (normative — the implementation follows this document):

* **E1** `name_fmt.go` `DnErrorName`/`DnLinearName` accept `cnId` but the format string
  omits it, so two CNs' devices for one side would collide; `SideInfo` and `010Side`
  prove the devices are per-CN. Resolution: append `-{cnId:%016x}` (§4.2).
* **E2** `CnPoolMetaName`/`CnPoolDataName` take a `grpId`, but `040Slice` and
  `CntlrInfo.slice_id_to_meta/data` show exactly one meta and one data aggregation
  device per slice (a multi-segment dm-linear; `GrowSlice` reloads its table).
  Resolution: drop the `grpId` component (§4.2).
* **E3** `constants.go` has `DefaultMigrTheshold` (typo, keep as
  `DefaultMigrThreshold = 1`) and a duplicate `DefaultMgirThreshold = 1` where
  `DefaultMigrBatchSize = 1` was clearly intended (mirrors the clone constants).
  Also `DefaultCntlrUnhealhty` → `DefaultCntlrUnhealthy`.
* **E4** `CnTmpfsPath` formats `cn_id` with `%016d`; use `%016x` like every other id.
* **E5** No NQN format exists for the transfer's export although
  `CntlrInfo.xfer_id_to_subsystem` requires one. Added `XferNqn`, `nqnKindXfer = 0x4`
  (§4.4).
* **E6** Key-field formats not pinned by the sources: `bin_idx` → `%01x` (values 0-3),
  bitmap `bm_idx` → `%02x`.
* **E7** The old key example rendered `cluster_id` in decimal; normative rule: every id
  in every key uses `IdKeyFmt` (hex).
* **E8** `SpNote`'s key comment in `schema.proto` says `sp_conf` (copy-paste); the key
  is `{p} sp_note {cluster_id} {sp_name}`.
* **E9** `MinCpCap = 1024` is read as `MinCnCap` (floor on a `GetCnSize` reply, §6.1).
* **E10** "During a transfer the leg might be a multipath device" (source text) actually
  describes a *migration*'s two sides. Kernel NVMe multipath only merges controllers of
  one subsystem NQN, and the two sides export different NQNs, so the leg is realized as
  an agent-managed dm-linear wrapper reloaded between the sides' nvme devices on ANA
  flips (§3.3 step 1). If a future revision unifies the NQNs, the wrapper degenerates to
  the kernel device.
* **E11** `DnPortBitmapSize`/`CnPortBitmapSize` exist but no schema field stores the
  allocation, although CP-written `Side.nvme_tr_conf` must carry real `tr_svc_id`s.
  Resolution: add `repeated uint32 port_bitmap` (packed, 512 bits) to `DnConf` and
  `CnConf`; the CP allocates the lowest free index per export
  (`tr_svc_id = base + 1 + idx`) and frees it on delete; agents mirror the assignment.
* **E12** Reserving the `migr-pv` LV inside the DN VG: subtract its extents when
  computing `DnConf.total_ext_cnt` so allocation can never collide with it.
* **E13** `Group.meta_blocks`/`data_blocks` (units of `data_block_size`) record the
  md `--data-offset` split of each leg LV: `meta_blocks = ceil(2 MiB /
  data_block_size)` for RedundMdRaid1 (matching the verified `--data-offset=2048K`),
  `0` for RedundNone; `data_blocks` = the remainder. Introduce implementation constant
  `MdDataOffset = 2 MiB`.
* **E14** `Get{DiskNode,ControllerNode,StoragePool}Reply` carry no note field although
  notes are writable; extend the replies with the note (recommended) or add `GetNote`
  RPCs.
* **E15** `Set/Clear*Flag` operate on a `uint32 flags` word that the confs model only as
  `bool disabled`. Resolution: replace `disabled` with `uint32 flags` (bit 0 =
  disabled) in `DnConf`/`CnConf`, or keep the bool and map bit 0 onto it; only bit 0 is
  defined in v001.
* **E16** `CreateStoragePoolRequest.auto_threshold` references undefined type
  `AutoThreshold`; it is `EventThreshold` (also fix the field name to
  `event_threshold`). `EventThreshold.primay_unhealthy` keeps its typo in the wire name
  but means `primary_unhealthy`.
* **E17** No source defines the initial meta-group size; adopted:
  `clamp(ceil(init_ext_cnt / 1024), 1, 16)` extents per slice (dm-thin metadata caps at
  16 GiB; 1 GiB per ~1 TiB of data is ample at the 1 MiB default block).
* **E18** `Cntlr.cn_addr_addr` → read as `cn_addr_port` (the hosting CN's agent
  `addr_port`).
* **E19** `SpConf.sp_level` is agent-visible but missing from
  `SyncupSideRequest`/`SyncupCntlrRequest`; add `SpLevel sp_level` to both.
* **E20** Batch `FindStoragePoolNames` omits unknown ids from the reply map instead of
  failing the whole call (the old single-lookup RPC returned `NOT_FOUND`).
* **E21** `Cntlr` lacks the `enabled` field that `UpdateCntlrEnabled`/`DeleteCntlr`
  logic requires; add `bool enabled` (default true at creation).
* **E22** `CreateThinDevice` validation `size % (slice_cnt × stripe_size) == 0` is
  added here (dm-striped needs equal, chunk-aligned legs); recommend sizes also be
  multiples of `data_block_size`. Also `CreateThinDeviceRequest.ori_name` is typed
  `uint32` in the proto — it is the origin **td_name**, change to `string`.
* **E23** `Subsystem.serial`/`model` generation is unspecified; adopted:
  `serial = %016x(ss_id)`, `model = "dnv"` (both well under nvmet's limits).
* **E24** `CreateNamespaceRequest` cannot set `dev_uuid`/`dev_nguid`, but the
  transfer+clone flow (§11.3) needs sp2's namespace to reuse sp1's identity for host
  multipath aggregation. Add optional `dev_uuid`/`dev_nguid` request fields (empty ⇒
  generate).
* **E25** Cosmetic rpc/field typos to keep or fix at codegen time (wire-compatible
  either way, pick once): `UpdatenamespaceSuspended`, `CnXferFinaName`,
  `CntlrInfo.xfer_id_to_namespae`, `SyncupSideRequest.side_poiner`,
  `PushMigrBitmapRequest.side_ponter`, `DeleteClusterReply.cluser_id`,
  `SyncupSideReply.bm_info_list` should be `repeated`.
* **E26** Spare-leg RPCs: `grp_id` is `string` in Create/Delete but `uint64` in Switch —
  unify on `uint64`; `DeleteSpareLegRequest` lacks the `leg_id` needed to pick one of up
  to two spares — add `uint64 leg_id`.
* **E27** `GetThinDeviceBitmap`/`GetLegBitmap` need agent support that the proto lacks.
  Add to `ControllerNodeAgent`:
  `rpc GetTdBitmap(GetTdBitmapRequest) returns (GetTdBitmapReply)` and
  `rpc GetLegBitmap(GetAgentLegBitmapRequest) returns (GetAgentLegBitmapReply)` —
  request = `{cluster_id, cn_id, cntlr_pointer, td_id|leg_id, slice_idx, start_block,
  block_cnt}`, reply = `{AgentReply, bytes bitmap}`; the gateway proxies §8.13 to the
  primary cntlr's CN.
* **E28** `CreateStoragePoolRequest.init_ext_cnt` is interpreted as the initial **data**
  group's extent count **per slice** (so `GrowSlice(ext_cnt, is_meta)` and creation use
  the same per-group unit).

`name_fmt.go` additionally contains pure Go syntax slips (`uniq64`/`unit64`/`stirng`/
`rreturn`/`sideid`/`-> string`/missing `fmt`+`hash/fnv` imports/`nf.migrPvName` vs the
package-level `migrPvName`, `nf.tmpFilename`, missing return type on `CnTmpFileName`);
all are transcription noise — §4 is the clean normative version.

---

*End of design_v001.*
