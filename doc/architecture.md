# architecture.md — dnv design

This document owns the design of dnv as a whole: the system overview, the
object model, the data-plane device stacks, naming, the etcd data model,
allocation, common validation, the `service Gateway` API contract, the
agent contract both roles share, the worker contract, the procedures that
span components, the recorded design decisions [D1] to [D18] and the
known limits. It leans on the component documents for how
each component carries its part out — `gateway.md` for the gateway,
`dnv-worker.md` for the workers with `model` and `etcdutil`, `dnagent.md`
and `cnagent.md` for the agent and its two roles, `cdc.md` for discovery
and `dnvctl.md` for the CLI —, on `log.md`, `grpc.md` and `osclient.md` for
logging, the interceptors and OS access, on `layout.md` and
`dependencies.md` for packages and modules, on `pb/schema.proto` for every
message, and on `glossary.md` for the vocabulary.

## System overview

dnv is a distributed block-storage system. It aggregates the raw disks of
many **disk nodes (DN)** into **storage pools (SP)**, runs the volume logic
(thin provisioning, striping, redundancy, snapshots, cloning, live
migration) on **controller nodes (CN)**, and exports virtual volumes to
**hosts** over **NVMe-oF** with native NVMe multipath and ANA. The control
plane keeps all desired state in **etcd**; stateless control-plane
processes (gateway, workers, cdc) and per-node agents converge the data
plane to that desired state. See fig. `070Cluster`.

dnv can be the data plane of a multi-tenant block storage system, for
example behind Kubernetes or OpenStack Cinder. It knows no tenants and
authenticates no user: the layer above authenticates its users and decides
which storage pools, subsystems and hosts serve each of them. What dnv
promises that layer is that the data of one storage pool is never readable
from another storage pool, unless that layer exports it there itself, for
example with a transfer (Transfers). The extents a pool frees can go to the
sides of any other pool with the bytes they hold, and dm-thin keeps those
bytes from every host ([D15]): a host reads a pool only through its thin
devices, a thin device reads a block it has not provisioned as zeros, and
dnv's thin pools write a block whole before a host can read any of it. The
promise is about what a host can read, not about the network or the nodes:
the fabric under dnv and the root user of every node are trusted (Known
limits).

```mermaid
flowchart LR
    H["Hosts …"]
    subgraph dp["Data plane"]
        subgraph CNs["Controller Nodes"]
            CN0["CN"]
            CN1["CN …"]
        end
        subgraph DNs["Disk Nodes"]
            DN0["DN"]
            DN1["DN …"]
        end
    end
    subgraph cp["Control plane"]
        CPS0["CP server 0<br/>dnv-gateway / dnv-worker / dnv-cdc"]
        CPS1["CP server 1 …"]
        ETCD[("etcd")]
        CPS0 <--> ETCD
        CPS1 <--> ETCD
    end
    H -->|"nvme-of"| CNs
    CNs -->|"nvme-of"| DNs
    CPS0 -->|"agent gRPC"| CNs
    CPS0 -->|"agent gRPC"| DNs
    CPS1 -->|"agent gRPC"| CNs
    CPS1 -->|"agent gRPC"| DNs
```

*Fig. `070Cluster` — physical view: hosts, data plane, control plane.*

Process and RPC view:

```
Hosts  ──nvme-of──▶  Controller Nodes (cntlrs)  ──nvme-of──▶  Disk Nodes (sides)
                          ▲            ▲                            ▲
                          └── SyncupCn/SyncupCntlr        SyncupDn/SyncupSide
                              + CheckCn/CheckCntlr        + CheckDn/CheckSide
                                   │                                │
              dnv-gateway ──▶ etcd ◀── dnv-worker (roles dn,cn,sp) ─┘
                                   ▲
                              dnv-cdc (discovery)
```

The processes all bind flags, a config file and environment variables
through viper (Components: invocation reference):

* `dnv-gateway` serves the `Gateway` gRPC service to users and CLIs. It
  reads and writes etcd, and calls agents only for `GetDnSize` and
  `GetCnSize`, the `Get*Info` behind its `Inspect*` RPCs and behind the
  `force` false hydration checks of `DeleteClone` and `FinishMigration`
  (Clones, Migrations), and the `Get*Bm` bitmap reads (Bitmap reads). The
  worker never calls `Get*Info`: it watches through `Check*` (Check
  streams, dn / cn roles). `gateway.md` owns it.
* `dnv-worker` is one binary with the roles `dn`, `cn` and `sp`, any subset
  per instance. It watches the revision keys in etcd, shards the work by
  shard code, drives agents through the unary `SyncupDn`, `SyncupSide`,
  `SyncupCn`, `SyncupCntlr`, `PushCloneBitmap` and `PushMigrBitmap`
  (Bitmap push protocol) and watches them through the `CheckDn`,
  `CheckSide`, `CheckCn` and `CheckCntlr` streams (Check streams). It also
  performs health checking and the automatic reactions — primary election,
  replacements, thin-pool auto-grow, leg repair (Automatic reactions).
  `dnv-worker.md` owns it.
* `dnv-agent dn` and `dnv-agent cn` run on every DN and CN and serve
  `DiskNodeAgent` and `ControllerNodeAgent`. The agent owns the local
  device-mapper, mdadm and nvmet state — on a DN also the [D13] on-disk
  extent metadata; dnv uses no LVM anywhere ([D13], [D14]) — and persists
  the last accepted request per object, and every received bitmap chunk, as
  protobuf files in its local store (Common agent rules, Bitmap push
  protocol, Agent local-store paths). `dnagent.md` and `cnagent.md` own it.
* `dnv-cdc` is the NVMe-oF Central Discovery Controller: it watches the
  `cdc` keys and serves discovery and AENs to hosts (dnv-cdc). `cdc.md`
  owns it.
* `dnvctl` is the CLI over the `Gateway` service, owned by `dnvctl.md`.

## Terminology and object model

Every numeric id is a uint64, rendered with `IdKeyFmt` in keys and device
names unless stated otherwise. The shard code and the thin-device `dev_id`
and `ori_id` (with their `SpConf.next_dev_id` counter) are uint32: the
shard code is a key field of `DnRev`, `CnRev`, `SpRev` and `CdcEntry`,
rendered with `ShardCodeFmt` (Key grammar, Key table), while `dev_id` and
`ori_id` are rendered into no key or device name. `block_size` unqualified
always means the SP's `DmPoolConf.data_block_size`.

* **cluster** — The namespace for everything else. Identified by
  `cluster_name`; `cluster_id` is the fnv64a hash of `cluster_name` followed
  by `creation_epoch` (cluster_id derivation), where `creation_epoch` is
  stamped once at `CreateCluster`. Deleting and recreating a cluster under
  the same name therefore yields a **different** `cluster_id`. One etcd
  installation can host many clusters.
* **DN, disk node** — A machine contributing one raw block device
  (`--disk`). The device carries the [D13] dnv disk format; capacity is
  handed out as **extents** from its data area.
* **CN, controller node** — A machine running the volume logic. It
  contributes no persistent storage (only a tmpfs for clone metadata) but
  has a capacity budget in extents.
* **extent** — The allocation unit for both DN space and CN budget, of
  `ClusterConf.dn_bin_conf.extent_size` bytes (default
  `DefaultDnExtSize`). Counts are always rounded **down**.
* **SP, storage pool** — The unit of volume service. It owns cntlrs,
  slices, thin devices, subsystems, clones, transfers and migrations, and
  is identified by `sp_name` (user visible) and `sp_id` (internal, used in
  device names and keys; reverse lookup through `sp_id_to_name`).
* **cntlr** — One SP instance on one CN. Exactly one cntlr of an SP is
  **primary**: it runs the full device stack and serves IO. The others are
  **standby**: they keep the leg connections, export dm-error and are ANA
  inaccessible. `cntlr_cnt` runs from `MinCntlrCntPerSp` to
  `MaxCntlrCntPerSp`, default `DefaultCntlrCntPerSp`. Figs
  `020ControllerNode`, `030PrimaryCntlr` and `050StandbyCntlr` (Controller
  node, common; Primary cntlr; Standby cntlr).
* **slice** — A vertical shard of an SP. Each slice is one dm thin-pool on
  the primary, and thin devices are striped (raid0) across all slices.
  `slice_cnt` runs from one to `MaxSliceCntPerSp` and is fixed at SP
  creation. Fig. `040Slice` (Primary cntlr).
* **group** — A contiguous chunk of pool space inside a slice. Each slice
  has at least one **meta group**, which backs the thin-pool metadata
  device, and at least one **data group**, which backs the thin-pool data
  device; `GrowSlice` appends groups. A group is either a single leg
  (`RedundNone`) or an md-raid1 over its legs (`RedundMdRaid1`). Every leg
  of a group reserves a small **meta region** at its start — md
  superblock, write-intent bitmap and health block (Group on-leg layout:
  meta region, data region, health block); only the remaining **data
  region** feeds the pool.
* **leg** — One replica of a group, backed by exactly one **side**
  normally and by two sides while that leg is being migrated. A group may
  also carry up to `MaxSpareLegPerGrp` **spare legs**: connected and
  health-checked, but not md members until a switch (Spare legs).
* **side** — The DN-resident part of a leg: one side device `DnSideName`
  (a dm-linear over the side's [D13] extent runs) plus, per cntlr, a
  dm-error, a dm-linear and an nvmet subsystem exported to that cntlr's
  CN. Figs `000DiskNode` and `010Side` (Disk node).
* **thin device (td)** — A user volume: one dm-thin volume per slice plus
  one raid0 (dm striped) across the per-slice thin volumes on the primary.
  Snapshots are thin devices with `ori_id` set. `created` marks a td whose
  thin volume the primary has reported `OK` in every slice (sp role); only
  a created td can be snapshotted, and an origin cannot be deleted while a
  snapshot of it is uncreated (Thin devices).
* **subsystem (ss), namespace (ns)** — The host-facing NVMe-oF objects. A
  namespace binds an `ns_idx` (NSID) of a subsystem to a thin device. Fig.
  `060VirtualVolume` (Host view).
* **clone** — A pull-copy of an external NVMe-oF namespace into a local
  thin device through dm-clone, on the primary cntlr. Fig. `090Clone`
  (Clones).
* **transfer (xfer)** — The source-side counterpart of a clone: it exports
  a local namespace's raid0 over a dedicated subsystem so that another SP,
  possibly of another cluster, can clone from it. Fig. `100Transfer`
  (Transfers). Clone plus transfer is the cross-SP live migration of a
  volume (Transfer + clone = cross-SP live migration).
* **migration (migr)** — The side-level live move of one leg's data from
  one DN to another through dm-clone on the destination DN. Fig.
  `080Migration` (Migration).
* **shard code** — A uint32 below `ShardBucketSize`, formatted with
  `ShardCodeFmt`. Every DN, CN and SP gets one at creation; workers and the
  cdc split ownership by shard code.
* **revision** — A monotonic uint64 per DN, CN and SP, stored in `DnRev`,
  `CnRev` and `SpRev`. It is bumped whenever the agent-visible desired
  state changes and drives the worker-to-agent sync (Agent services and
  agent behavior, Workers). It is also the optimistic-concurrency token
  carried in mutating requests. The rev keys are id-keyed (`dn_id`,
  `cn_id`, `sp_id`) and carry the node's `addr_port`, or the SP's
  `sp_name`, in the **value**, so one watch event gives a worker both the
  revision and the handle it needs to act (Revision keys and the sync
  fan-out).
* **location** — A free-form failure-domain string per node (rack, zone),
  stored in `DnConf` and `CnConf`; a copy rides in the capacity-key value
  for scan efficiency ([D5]). Allocation uses it for anti-affinity
  (Allocation).
* **creation_epoch** — The unix time in nanoseconds of the moment the
  cluster was created, stored in `ClusterConf.creation_epoch`.
  Gateway-generated (not a request field), immutable, and the second input
  of the `cluster_id` hash (cluster_id derivation). Unrelated to
  `err_epoch`, which is in unix **seconds**.
* **err_epoch** — The unix seconds at which a worker found the object
  unhealthy after last seeing it healthy; zero while it is healthy. It
  stays put while the object stays unhealthy (`dnv-worker.md` HL3), and the
  automatic reactions measure their thresholds, the `EventThreshold`, from
  it (Automatic reactions). A node with a nonzero `err_epoch` also loses
  its capacity key (Capacity index keys), and a cntlr with one leaves its
  SP's discovery records unless it is the primary ([D18]).
* **settling** — `Cntlr.settling`: true from the moment a cntlr becomes
  primary — created as one by `CreateStoragePool`, promoted by a failover,
  created as one by a sole-primary replacement, or re-enabled by
  `UpdateCntlrEnabled` while still primary (Cntlrs) — until the worker
  first observes it clean in that role, enabled, at the revision it drives
  and with its stack built, the rows its `sp_level` suppresses aside
  (`dnv-worker.md` HL2 says which rows read as built), or until a failover
  demotes it first. While it is set, the failover judges an unhealthy
  primary by `cntlr_unhealthy` instead of `primary_unhealthy` when that is
  the longer; a `disabled` one still fails over at once (`dnv-worker.md`
  AR5). It is health bookkeeping of the same kind as `err_epoch`: set by
  the op that makes the cntlr primary, cleared by the observation that
  proves the role, in a write that bumps no revision, or by the failover
  that demotes it.

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
    └── SpName (sp_id → sp_name reverse map), SpRev, CdcEntry per ss
```

### Cardinality limits

The cardinality limits are constants of `common/constants.go`, which holds
their values:

* per cluster: `MaxDnCntPerCluster`, `MaxCnCntPerCluster` and
  `MaxSpCntPerCluster`;
* per SP: `MaxTdCntPerSp`, `MaxSsCntPerSp`, `MaxCloneCntPerSp`,
  `MaxXferCntPerSp` and `MaxMigrCntPerSp`; the slice count up to
  `MaxSliceCntPerSp`, default `DefaultSliceCntPerSp`; the cntlr count from
  `MinCntlrCntPerSp` to `MaxCntlrCntPerSp`, default
  `DefaultCntlrCntPerSp`;
* per subsystem: `MaxNsCntPerSs` namespaces and `MaxHostCntPerSs` allowed
  hosts;
* per node: `MaxSideCntPerDn` sides on a DN, `MaxCntlrCntPerCn` cntlrs on a
  CN;
* per group and slice: `MaxLegPerGrp` legs, a ceiling nothing enforces —
  `MaxAllocLegPerGrp` is the allocator's real per-group leg count —,
  `MaxSpareLegPerGrp` spare legs, and `MaxGrpCntPerSlice` groups in
  **each** of a slice's two group lists (md names, GrowSlice);
* bitmaps: `MaxCloneBmCnt` chunks per source slice bitmap of a clone and
  `MaxMigrBmCnt` chunks per migration bitmap; `CloneBmChunkBytes` is one
  clone chunk's capacity **and** its positioning quantum (Bitmap push
  protocol);
* transactions: `EtcdMaxTxnOps` is the `--max-txn-ops` every etcd serving
  dnv must be started with, sized by the widest shape of
  `CreateStoragePool` (Storage pools, Components: invocation reference;
  `gateway.md`, Constants this document owns) and pinned by a test;
  the sp drain's batch and the created flip's transaction are bounded
  transactions over etcd's default limit too (`dnv-worker.md` SPD13,
  RW19). `MaxDelGrpPerTxn` is the groups one sp drain batch removes
  (`dnv-worker.md` SPD10), `MaxDelBmPerTxn` the clone bitmap chunk keys
  one clone drain batch deletes (`dnv-worker.md` CLD8), and
  `MaxFlipCreatedPerTxn` the tds one created-flip transaction carries
  (sp role, `dnv-worker.md` RW19);
* cntlid slots: `CnCntlidSlotCnt` and `DnCntlidSlotCnt`, the slots of the
  cntlid partition (cntlid slots); the gateway refuses a `cntlid_slot` or
  `cntlid_slot_list` value at or above `CnCntlidSlotCnt`;
* conf bounds: `MinChunkBlockCnt`, `MaxChunkBlockCnt` and
  `DefaultChunkBlockCnt` for `redund_md_raid1.bitmap_chunk_block_cnt`
  (Common validation); `MinAllocDnBatchSize`, `MaxAllocDnBatchSize` and
  `DefaultAllocDnBatchSize` for `alloc_conf.dn_batch_size`, and
  `MinAllocCnBatchSize`, `MaxAllocCnBatchSize` and `DefaultAllocCnBatchSize`
  for `alloc_conf.cn_batch_size` (Per-operation allocation, Common
  validation);
* `ShardBucketSize`, the number of shard codes and the length of every
  global's `shard_bucket`; `MaxListCnt` and `DefaultListCnt`, the bound and
  the default of a paged list's `count`.

## Data-plane device stacks

The data plane is built from stock Linux pieces: the device-mapper targets
error, linear, striped (raid0), thin-pool, thin and clone, md-raid1, and the
kernel nvmet target and nvme host. Every device dnv creates has a
deterministic name (Naming), so agents are fully idempotent and
crash-restartable. The port link is the last step of building every
nvmet subsystem, on either node kind, after its attributes, its allowed
hosts and its namespaces: on the DN any failure before it holds the link
back, while on the CN a namespace that fails to build does not. Teardown
runs in reverse, the port link first (`dnagent.md` SH19). The CN's host-facing and transfer subsystems and
a DN's side exports carry a cntlid range, a serial and a model, and their
namespaces an identity; a migration-source export carries none of them,
only its allowed host and its namespace's "device_path" and ANA group.
Every subsystem, that one included, has "attr_allow_any_host" written "0"
with its attributes, before its host links are made
(Primary cntlr, step 6). A later ANA transition rewrites only the
namespace's "ana_grpid" ([D4]).

### Disk node

```mermaid
flowchart BT
    subgraph dn["Disk Node"]
        VG["extent area ([D13] data area)"]
        S0["Side"]
        S1["Side …"]
        VG --> S0
        VG --> S1
    end
    S0 ==>|"ANA optimized"| P0["CN of its SP's primary cntlr"]
    S0 -->|"ANA non-optimized"| B0["CNs of its SP's standby cntlrs"]
    S1 ==>|"ANA optimized"| P1["primary cntlr CN …"]
    S1 -->|"ANA non-optimized"| B1["standby cntlr CNs …"]
```

*Fig. `000DiskNode` — one DN: its extent area hands extents to the sides it
hosts; every side exports one path per cntlr of its SP.*

Per DN, once (created by the dn agent at the first `SyncupDn`):

1. The **dnv disk format** [D13] on the `--disk` device, at fixed byte
   offsets that are constants of `common/constants.go`:
   * the header block at `DnHeaderOffset`, `DnHeaderSize` bytes: a
     self-describing header — a magic, a format version, a checksum and a
     `DnDiskHeader` carrying `cluster_id`, `dn_id`, `extent_size`, a random
     64-bit `format_uuid` and the three layout fields `data_offset`,
     `clone_meta_offset` and `clone_meta_size`;
   * the two alternating **volume-table** slots at `DnTableSlotAOffset` and
     `DnTableSlotBOffset`, each `DnTableSlotSize` bytes — a magic, the
     header's `format_uuid`, a monotonic `seq`, a checksum and a
     `DnDiskTable`. Every mutation writes the slot that is *not* the newest
     valid one, so a torn write can only damage the older copy;
   * the clone-metadata area at `DnCloneMetaOffset`, `DnCloneMetaSize`
     bytes in units of `DnCloneMetaUnit`: dm-clone metadata space for the
     migrations whose **destination** side lives on this DN. A slot is a
     contiguous run of whole units and every migration costs at least two
     of them, the fixed dm-clone metadata base alone filling one, so the
     area bounds the destination roles one DN can hold (Migrations,
     `dnagent.md` DN13);
   * the extent area from `DnDataOffset`. Extent *i* lives at
     `DnDataOffset` plus *i* times `extent_size`; the offset is fixed rather
     than extent-aligned on purpose, so the usable size, the disk size minus
     `DnDataOffset`, is a pure constant subtraction `GetDnSize` can answer
     before `extent_size` is known.

   The two envelopes are fixed; `agent/dnagent/diskmeta.go` holds their byte
   layout. Each checksum covers its envelope and its body alike — a slot's
   `seq` included, so one flipped bit there cannot make the stale slot
   outrank the live one and roll the whole volume table back. A slot is
   valid only when its magic, checksum and size check, its body parses and
   it carries the header's `format_uuid`, which is what makes a slot left
   over from an earlier format of the same disk lose, whatever its `seq`;
   among the valid slots the higher `seq` wins. The format writes slot A
   before the header, so a valid header implies a valid slot, and a valid
   header with no valid slot is a hard error, never a silently empty table.
   A header that names another cluster, dn or extent size is a foreign
   disk, which is never overwritten, and a corrupt header, or one written
   under another format version, is never formatted over either
   (`dnagent.md` DN5 is the procedure).

   The on-disk table — not the agent's local store — is authoritative for
   extent placement, so a node that loses `--local-store` but keeps its disk
   rebuilds exactly the same devices.
2. Exactly **one** nvmet port, built from the agent's `--tr-type`,
   `--adr-fam`, `--tr-addr` and `--tr-svc-id` flags. `DnConf.nvme_tr_conf`
   is the operator's copy of those flags, given to `CreateDiskNode`, and is
   what the control plane hands to the nodes that dial this DN: it is
   copied into `Side.nvme_tr_conf` (Storage pools), which a CN dials to
   reach that DN's sides (Primary cntlr, step 1) and a migration
   destination dials to reach its source (Migration). Every subsystem this
   DN ever exports — all side subsystems and all migration-source
   subsystems — attaches to this single port. The port is one per
   **agent**, not one per kernel: it lives at the configfs id
   `NvmetPortId` unless the agent is started with `--nvmet-port-id`
   (`dnagent.md` CM2). Several dn agents may therefore share one kernel,
   each taking a distinct port id and a transport address of its own (a
   different `--tr-addr` or `--tr-svc-id`), because two DNs answering on
   one address would be indistinguishable to the CN dialing them. Each also
   needs its own `--disk` and its own `--local-store` prefix: the names of
   Agent local-store paths do carry `cluster_id` and `dn_id`, but the store is
   read back by **kind** prefix alone, so two dn agents sharing a prefix
   would adopt and converge each other's records at startup (`dnagent.md`
   CM2, SH6). Agents that keep the default id co-own the default port
   instead, which is what lets a dn and a cn agent on one node share one
   port: `EnsurePort` is probe-first and writes only what differs
   (`dnagent.md` SH19). Sharing a kernel also carries one placement rule —
   the `SideToCnNqn` note under **Per side** below.

Per **side** (one per hosted leg replica):

* A **side device** `DnSideName` (dm kind `DmKindDnSide`): one dm-linear
  whose targets concatenate the extent runs the volume table allocated to
  (`sp_id`, `side_id`), `ext_cnt` extents of the `Group` in total.
  Provisioning must follow the Side provisioning protocol: the record is
  created with the request's length to zero and a zeroed count of zero,
  the assembled device is zeroed from its start up to that length, batch
  by batch, with `blkdiscard --zeroout` through the dm-linear, the count is
  persisted after each batch, and no per-CN export stack is converged
  until the side's `provisioned` flag is true **and** the count has
  reached the length ([D15]). Everything above the side device — the
  per-CN stacks, the migration endpoints — sees one ordinary single-device
  backing reference.
* Per cntlr of the SP, primary and standbys (the side learns their CN ids
  from `side_conf.primary_cn_id` and `standby_id_list` of the
  `SyncupSideRequest`): a dm-error device `DnErrorName`, a dm-linear device
  `DnLinearName` whose table points at the **side device** for the primary
  cntlr's CN and at the **dm-error** device for every standby CN, and an
  nvmet subsystem `SideToCnNqn` of the cluster, sp, leg and cn on the DN's
  port, admitting that CN alone and exposing one namespace backed by the
  dm-linear in the ANA group of the CN's role [D4] — the optimized group
  for the primary's, the non-optimized group for a standby's, short of a
  migration phase or an `sp_level` that moves it (Migration, SpLevel).
  `dnagent.md` DN10 builds the stack and sets its attributes. A standby
  path therefore exists but errors out — a pre-connected placeholder that
  makes failover fast.

    The NQN names the **leg**, not the side, and carries no dn component
    (NQNs), so while a leg is being migrated its src and dst sides — on two
    different DNs — export the same subsystem NQN toward the same CN. For
    the CN's kernel to merge them into one multipath namespace instead of
    rejecting a duplicate, both sides present the same namespace identity —
    the same nsid, and the "device_uuid" and "device_nguid" that
    `DnNsIdentity` derives from (`cluster_id`, `sp_id`, `leg_id`) — and,
    per cntlid slots, cntlid ranges that do **not** overlap.

    That identical NQN is also what stops two dn agents sharing a kernel
    from holding the two sides of one leg. A distinct `--nvmet-port-id`
    gives each agent its own port, but nvmet subsystems are **not** under
    the port — a subsystem's configfs directory is kernel-global and the
    port only links to it — so both agents would converge that one
    subsystem and its one namespace, each writing its own side device into
    the same "device_path", and either one's teardown would run
    `RemoveSubsystem` against the whole of it. No other dn-side name names
    an object the two would fight over: `DnSideName`, `DnErrorName`,
    `DnLinearName`, `DnMigrMetaDmName`, `DnMigrSrcName`, `DnMigrFinalName`,
    `MigrSrcNqn`, `DnHostNqn`, `LocalDnPath`, `LocalSidePath` and
    `LocalMigrBmPath` all carry `dn_id` (dm device names, NQNs, Agent
    local-store paths), and the ANA groups are nested under the port. Two
    dn-built names carry no `dn_id` and are harmless all the same:
    `CnHostNqn` of the cluster and cn, whose kernel-global host directory
    both agents merely create — with `mkdir -p`, and no code path anywhere
    removes a host — and the namespace identity `DnNsIdentity` of the
    cluster, sp and leg above, which lives inside the one subsystem the NQN
    rule already covers.

    Only a **migration** ever gives a leg two sides, so the rule is a
    placement one, not an agent-flag one: register every DN of one kernel
    under the same `location` and the first tier of Per-operation
    allocation keeps a migration destination (and a spare leg) off the
    source's kernel. Two things bound that. An omitted `location` defaults
    to the node's own `addr_port` (Disk nodes), so agents left to the
    default are separate failure domains and nothing keeps them apart at
    all; and even under a shared `location` the second tier of
    Per-operation allocation rescans without the exclusion rather than
    refuse to place, so the guarantee holds only while some other domain
    still has a free DN.

```mermaid
flowchart BT
    subgraph side["Side (one per hosted leg replica; cn0 = primary CN, cn1 = standby CN)"]
        LV["side device (dm-linear over<br/>the extent runs)<br/>DnSideName"]
        E0["dm-error (cn0)<br/>DnErrorName"]
        L0["dm-linear (cn0)<br/>DnLinearName"]
        N0["nvmet subsystem for cn0<br/>SideToCnNqn(cluster,sp,leg,cn0)"]
        E1["dm-error (cn1)<br/>DnErrorName"]
        L1["dm-linear (cn1)<br/>DnLinearName"]
        N1["nvmet subsystem for cn1<br/>SideToCnNqn(cluster,sp,leg,cn1)"]
        LV --> L0
        L0 --> N0
        E1 --> L1
        L1 --> N1
        E0 -.->|"table target when cn0 goes standby"| L0
        LV -.->|"table target when cn1 goes primary"| L1
    end
    VG["extent area ([D13] data area)"] --> LV
    N0 ==>|"ANA optimized"| CN0["cn0 (primary cntlr's CN)"]
    N1 -->|"ANA non-optimized"| CN1["cn1 (standby cntlr's CN)"]
```

*Fig. `010Side` — one side and its per-CN export stacks. Failover only
reloads the dm-linear tables and flips ANA (dashed alternatives).*

Failover and migration only ever *reload* the dm-linear tables and flip ANA
states; the exported namespace object never changes, so the CNs' NVMe
connections survive.

Additionally, while this DN hosts the **source** side of a migration (its
`SyncupSide` carries `migr_src_conf`): a dm-linear `DnMigrSrcName` on top of
the side device, exported through the subsystem `MigrSrcNqn` (same port)
whose allowed hosts are the `DnHostNqn` of `migr_src_conf.dst_dn_id`. While
it hosts the **destination** side (`migr_dst_conf`): an nvme host connection
to the `MigrSrcNqn` of the cluster, `migr_dst_conf.src_dn_id`, sp and
migration at `migr_dst_conf.src_nvme_tr_conf` (hostnqn `DnHostNqn`); a
dm-clone metadata **slot** in the [D13] clone-metadata area — whole
`DnCloneMetaUnit` units, with its head zeroed before its record is
persisted so whatever an earlier migration left there can never be
misparsed as a dm-clone superblock — fronted by a wrapper dm-linear
`DnMigrMetaDmName` (dm kind `DmKindDnMigrMeta`), because the dm-clone
target reads its metadata device from sector zero and takes no offset
argument; and a dm-clone device
`DnMigrFinalName` (dest the local side device, source the connected nvme
device, region size `migr_dst_conf.block_size`). The per-cntlr dm-linears of
the destination side sit on top of the dm-clone (primary CN) or the dm-error
(standbys). See fig. `080Migration` and Migration; the agent's sequence is
`dnagent.md` DN12 and DN13.

### Controller node, common

```mermaid
flowchart BT
    subgraph cn["Controller Node"]
        P0["Primary Cntlr<br/>(of sp a)"]
        P1["Primary Cntlr<br/>(of sp b)"]
        S0["Standby Cntlr<br/>(of sp c)"]
        S1["Standby Cntlr<br/>(of sp d)"]
    end
    LA["sides of sp a"] --> P0
    LB["sides of sp b"] --> P1
    LC["sides of sp c"] --> S0
    LD["sides of sp d"] --> S1
    P0 ==>|"ANA optimized"| H["hosts"]
    P1 ==>|"ANA optimized"| H
    S0 -.->|"ANA inaccessible"| H
    S1 -.->|"ANA inaccessible"| H
```

*Fig. `020ControllerNode` — one CN hosts up to `MaxCntlrCntPerCn` cntlrs of
different SPs, in a mix of primary and standby roles.*

Per CN, once (created by the cn agent at the first `SyncupCn`):

1. A tmpfs mounted at `CnTmpfsPath`, under `DefaultTmpfsPrefix` and named
   by the cluster and cn ids, sized `DefaultCnTmpfsSize`: the arena size
   `CnCloneMetaAreaSize` plus slack.
2. A **sparse** file `CnTmpFilePath` of `CnCloneMetaAreaSize` bytes inside
   it, attached to **one** loop device — kernel-assigned state the agent
   finds again on every converge and never persists (`cnagent.md` CN5).
   That loop device is the CN's **clone-metadata arena**. The arena is
   carved into **units** of `CnCloneMetaUnit` — "unit", never "extent": an
   extent is the allocation unit of Terminology and object model. A clone's
   dm-clone metadata device is a **wrapper dm-linear** `CnCloneMetaDmName`
   (cn dm kind `DmKindCnCloneMeta`, `cnagent.md`, Names and constants in
   `common`) over a first-fit run of **contiguous** units sized for the
   dm-clone metadata of the clone's region count, the destination td's size
   over `block_size` (`cnagent.md` CN18); the wrapper exists for the same
   reason `DnMigrMetaDmName` does — dm-clone reads its superblock from sector
   zero and takes no offset argument. **The kernel's dm tables are the
   allocation registry**: enumerating this CN's wrappers and reading their
   linear tables, each naming the loop device and an offset, reconstructs
   the used map, so there is no on-file allocation table and none of the
   [D13] header, CRC and A/B slot machinery is needed — the arena is
   volatile *together with* the kernel's dm state (a reboot clears both, an
   agent restart preserves both; `cnagent.md` CN5). Before creating a
   wrapper the agent hole-punches the range with a plain `blkdiscard` of
   it, so a recycled unit can never hand a new dm-clone a previous clone's
   valid dm-clone superblock; **`--zeroout` is forbidden here** — it would
   materialize up to the whole arena in RAM and defeat the sparse file.
   Arena exhaustion ⇒ the clone's resources report `RES_STATUS_ERROR`
   ([D14]). The metadata is deliberately volatile: after a reboot a clone
   is rebuilt from the **destination thin-pool bitmaps** (Clone crash
   recovery) — the copied blocks are exactly the mapped blocks of the
   destination td, so no clone state needs to survive the CN.
3. Exactly **one** nvmet port, built from the same four flags as a disk
   node's. `CnConf.nvme_tr_conf`, given to `CreateControllerNode`, is the
   operator's copy of them; the control plane copies it into the
   `Cntlr.nvme_tr_conf` of every cntlr this CN hosts and serves it to hosts
   in the discovery log while the SP's discovery records list that cntlr
   ([D18]; dnv-cdc). Every host-facing subsystem and every
   transfer subsystem of every cntlr on this CN attaches to the port. As on
   a disk node the port is one per **agent**, at the configfs id
   `NvmetPortId` unless `--nvmet-port-id` says otherwise
   (`dnagent.md` CM2); a cn agent that keeps the default co-owns the
   default port with a dn agent on the same node. Unlike the dn case,
   distinct port ids do **not** make two cn agents on one kernel safe, so
   run at most one: the host-facing subsystem NQN is the one the user
   passed to `CreateSubsystem` and every cntlr of that SP exports it
   (Primary cntlr, step 6; Host view; cntlid slots), `XferNqn` carries no
   node id (NQNs), and `CnMdArrayName` — the superblock name given to
   `mdadm --name` — carries neither cluster nor cn id (md names). Placement
   does not reliably separate them either: Per-operation allocation keeps
   the cntlrs of one SP in distinct `location`s only at tier 1, so two cn
   agents of one kernel registered under one `location` can still take two
   cntlrs of an SP once a pick finds no CN with room outside the domains
   that SP's cntlrs already hold — which any SP with more cntlrs than the
   cluster has domains with room reaches — and a cntlr replacement, which
   excludes only the surviving cntlrs' domains (`dnv-worker.md` AR7), can
   land on the other cn agent of the failed cntlr's kernel even at tier 1;
   under the default `location`, the node's `addr_port` (Controller nodes),
   they are simply two CNs to the allocator.
4. **QoS**: `SyncupCnRequest.qos_ratio`, a copy of `ClusterConf.qos_ratio`
   (dn / cn roles), is accepted and stored, and nothing enforces it: the cn
   agent persists it with the request and programs no limit (`cnagent.md`
   CN6; Known limits).

A CN hosts up to `MaxCntlrCntPerCn` cntlrs of different SPs; at most one
cntlr **per SP** per CN.

### Primary cntlr

```mermaid
flowchart BT
    subgraph pc["Primary Cntlr"]
        SL0["slice 0<br/>(per-td dm-thin volume)"]
        SL1["slice 1 …"]
        R["raid0 (dm-striped)<br/>CnRaid0Name"]
        NS["dm-linear ns-dev<br/>CnNsDevName"]
        T["nvmet target subsystem"]
        E["dm-error<br/>CnErrorName"]
        SL0 --> R
        SL1 --> R
        R --> NS
        NS --> T
        E -.->|"reload target on failover / standby"| NS
    end
    G0["leg connections<br/>(nvme-of to sides)"] --> SL0
    G1["leg connections …"] --> SL1
    T ==>|"ANA optimized"| H["host"]
```

*Fig. `030PrimaryCntlr` — the primary's per-td path for one thin device.*

Bottom-up, everything below is created and owned by the cn agent when
`SyncupCntlr` says `Cntlr.primary` is true ("Make sure all groups are
available"):

1. **Legs.** For each provisioned side of each leg, spare legs included —
   a side whose `provisioned` is false exports nothing yet and is skipped
   (`cnagent.md` CN10): `nvme connect` to the `SideToCnNqn` of the
   cluster, sp, leg and this CN at `Side.nvme_tr_conf`, with the hostnqn
   `CnHostNqn` of the cluster and cn and the flags of `dnagent.md` SH20.
   Because every side of a
   leg exports that one NQN with one namespace identity (Disk node, NQNs),
   the connections of a leg land in a single subsystem: the kernel presents
   **one** nvme multipath namespace device per leg, with one controller (one
   path) per connected side, and routes IO down the optimized path. A
   single-side leg is simply that device with one path; during a migration
   the same device gains the destination side as a second path and the ANA
   flip switches IO over — no device change, nothing above the leg notices
   [D1]. The **leg device** is a cn-local dm-linear wrapper over that
   multipath namespace device, and it is what md and the group devices
   consume as the leg ([D1] gives the reasons).
   The agent runs the health-check IO of Group on-leg layout: meta region,
   data region, health block, against every connected leg, spares too, and
   reports per leg in `CntlrInfo.leg_id_to_leg` (`cnagent.md` CN10, CN11).
2. **Groups.** `RedundNone`: the group device is a dm-linear over the
   single leg's **data region**, at the offset `meta_blocks` times
   `block_size` (Group on-leg layout: meta region, data region, health
   block). `RedundMdRaid1`: an md-raid1 `CnMdDevName`/`CnMdArrayName` over
   the member leg devices, with an internal write-intent bitmap and the
   create options of `cnagent.md` CN12. The assembly rules (create vs
   assemble vs add) are in "Make sure all groups are available".
   **Spare legs are not md members**: they stay connected, wrapped and
   health-checked, and are only added by an explicit `SwitchSpareLeg`
   (Spare legs).
3. **Per slice** (fig. `040Slice`): a dm-linear concat `CnPoolMetaName` over
   the slice's meta group devices (in `meta_grp_list` order) and a dm-linear
   concat `CnPoolDataName` over its data group devices; a dm thin-pool
   `CnPoolFinalName` whose metadata device is the pool meta, whose data
   device is the pool data and whose block size is `block_size`, with a
   low-water mark derived from the data device's blocks and
   `DmPoolConf.low_water_mark_pct` so that the dm event fires exactly when
   the pool's **usage** exceeds `low_water_mark_pct` percent, the condition
   the auto-grow acts on (Automatic reactions). Both values come from the
   `dm_pool_conf` of the `SyncupCntlrRequest`'s `bdev_conf`. A zero
   `low_water_mark_pct` is invalid: the control plane stores
   `DefaultPoolLowWatermarkPct` instead (Common validation), and the agent
   refuses the whole `SyncupCntlr` rather than defaulting it itself (Common
   agent rules). Above one hundred percent the agent passes a zero
   low-water mark — no dm events — as the value only means "auto-grow off"
   (Automatic reactions; `cnagent.md` CN13). The pool table carries no
   feature argument, so the pool never skips block zeroing ([D15]).
4. **Per thin device and slice**: a dm-thin volume `CnThinDevName`, created
   with the pool message "create_thin" of `dev_id` or "create_snap" of
   `dev_id` and `ori_id` — sent only while `ThinDevice.created` is false; a
   created td's volume is attached with a bare `dmsetup create`
   (`cnagent.md` CN14) — of virtual size `ThinDevice.size` over
   `slice_cnt`.
5. **Per thin device**: a dm-striped raid0 `CnRaid0Name` across its
   per-slice thin volumes (slice order `slice_idx`), with the chunk
   `DmRaid0Conf.stripe_size`, and a dm-error `CnErrorName` of the same size,
   the permanent reload target. **Per namespace** (Subsystems, namespaces):
   a dm-linear `CnNsDevName` of the cluster, cn, sp and `ns_id` — **the**
   namespace backing device — whose table points at its td's raid0
   (normal), at the dm-clone `CnCloneFinalName` (while a clone targets that
   td, fig. `090Clone`), or at the dm-error (standby, failover, parked — the
   state of any ns-dev pointed at its td's dm-error, Namespace suspend
   semantics, which includes every effectively suspended namespace); at a
   read-only level the table over the raid0 or the dm-clone is a dm-flakey
   one that fails writes, not a dm-linear ([D11], SpLevel).
   Namespaces backed by the same td each have their own `CnNsDevName` over
   the same raid0 (`cnagent.md` CN15, CN16).
6. **Host-facing nvmet**: per `Subsystem` an nvmet subsystem on the CN's
   port, with "attr_cntlid_min" and "attr_cntlid_max" from
   `Cntlr.cntlid_slot`, `serial` and `model` from the etcd `Subsystem`, and
   the allowed hosts as configured. The allowed hosts are the only
   admission gate of every subsystem dnv builds, host-facing or
   dnv-internal, a transfer subsystem included, whose record's list is
   used verbatim: a subsystem admits exactly the hosts its list names, and
   an empty list admits no host. A storage export defaults to deny: a
   `Subsystem` created without hosts, to have its namespaces attached
   before any host is let in, stays closed, its `CdcEntry` shown to no host
   (`cdc.md` DS4), until its hosts are granted; an empty list revokes every
   host; and host-facing and internal subsystems share one rule. dnv never
   sets "attr_allow_any_host", a mode no dnv flow needs: an agent writes it
   "0" before it links a subsystem's hosts, because nvmet refuses a new host
   link while it is set, and the same write closes a subsystem found with
   it set (`EnsureSubsystem`). Per `Namespace`: an nvmet namespace whose
   nsid is `ns_idx`, whose "device_path" is the namespace's own
   `CnNsDevName`, whose "uuid" and "nguid" come from the etcd record, and
   which is a member of the fixed optimized ANA group [D4] only on an
   enabled primary below `SP_LEVEL_DISABLE`, and only while it is not
   effectively suspended and its td's backing chain is not
   provisioning-deferred ([D15]) — otherwise of the inaccessible group
   (Subsystems, namespaces); suspended ⇒ the ns-dev is parked on its td's
   dm-error and the namespace moved to the inaccessible group everywhere
   (Namespace suspend semantics; `cnagent.md` CN16).
7. **Clones, transfers and migrations** hosted by the SP, per Procedures.

```mermaid
flowchart BT
    subgraph slice["Slice (RedundMdRaid1 example: 1 meta group + 2 data groups, 2 legs each)"]
        TD["dm-thin volume<br/>(one per td)"]
        TP["dm thin-pool<br/>CnPoolFinalName"]
        PM["pool meta concat (dm-linear)<br/>CnPoolMetaName"]
        PD["pool data concat (dm-linear)<br/>CnPoolDataName"]
        MG0["md-raid1<br/>meta group 0"]
        DG0["md-raid1<br/>data group 0"]
        DG1["md-raid1<br/>data group 1"]
        ML0["leg"]
        ML1["leg"]
        DL0["leg"]
        DL1["leg"]
        DL2["leg"]
        DL3["leg"]
        TP --> TD
        PM --> TP
        PD --> TP
        MG0 --> PM
        DG0 --> PD
        DG1 --> PD
        ML0 -->|"data region"| MG0
        ML1 -->|"data region"| MG0
        DL0 -->|"data region"| DG0
        DL1 -->|"data region"| DG0
        DL2 -->|"data region"| DG1
        DL3 -->|"data region"| DG1
    end
    X["nvme-of side connections<br/>(each leg backed by one DN)"] --> ML0
    X --> ML1
    X --> DL0
    X --> DL1
    X --> DL2
    X --> DL3
```

*Fig. `040Slice` — the per-slice stack of step 3. Every leg starts with its
meta region (md superblock, write-intent bitmap and health block; Group
on-leg layout: meta region, data region, health block); only the data
region becomes an md member or pool space.*

### Standby cntlr

```mermaid
flowchart BT
    subgraph sc["Standby Cntlr"]
        L0["leg"]
        L1["leg …"]
        E["dm-error<br/>CnErrorName"]
        NS["dm-linear ns-dev<br/>CnNsDevName (table → dm-error)"]
        T["nvmet target subsystem"]
        E --> NS
        NS --> T
    end
    S0["sides (nvme-of)"] -->|"ANA non-optimized"| L0
    S1["sides …"] -->|"ANA non-optimized"| L1
    T -.->|"ANA inaccessible"| H["host"]
```

*Fig. `050StandbyCntlr` — pre-connected but dark: legs stay connected, the
exported namespaces error out.*

A standby keeps only: the leg nvme connections and leg wrappers (Primary
cntlr, step 1) — whose health it reports from transport liveness, since it
cannot run the block probe (`cnagent.md` CN11) —, the per-td
`CnErrorName` and the per-namespace `CnNsDevName` (table → dm-error), and
the host-facing nvmet objects with every namespace in the inaccessible ANA
group [D4]. It has
**no** md arrays, pools, thin volumes or raid0s ("cleanup all resources that
a primary shouldn't have"). For a transfer it also keeps the
dm-error-backed `CnXferFinalName` and the transfer subsystem counterparts
(fig. `100Transfer`, right half; Transfers).

### Host view

```mermaid
flowchart BT
    subgraph host["Host"]
        MP["nvme-of multipath device"]
    end
    subgraph cn0["Controller Node 0"]
        PC["Primary Cntlr"]
    end
    subgraph cn1["Controller Node 1"]
        SC["Standby Cntlr"]
    end
    subgraph dn0["Disk Node 0"]
        EA0["extent area"] --> SD0["Side"]
    end
    subgraph dn1["Disk Node 1"]
        EA1["extent area"] --> SD1["Side"]
    end
    SD0 ==>|"optimized"| PC
    SD1 ==>|"optimized"| PC
    SD0 -->|"non-optimized"| SC
    SD1 -->|"non-optimized"| SC
    PC ==>|"ANA optimized"| MP
    SC -.->|"ANA inaccessible"| MP
```

*Fig. `060VirtualVolume` — one virtual volume end to end: the same subsystem
NQN and namespace identity from every cntlr, aggregated by the host
kernel.*

A host discovers the SP's subsystems through dnv-cdc and connects, through
nvme-stas or an equivalent that follows the discovery log, to the port of
every cntlr the SP's discovery records list: every enabled cntlr that is
the primary or has a zero `err_epoch` ([D18]). The kernel aggregates the
paths into one nvme multipath device because all cntlrs export the same
subsystem NQN and the same namespace identity ("nguid" and "uuid"), with
distinct cntlids guaranteed by cntlid slots. Primary path ANA optimized,
standby paths inaccessible. Following the discovery log is the host's part
of a failover: it is what drops a failed-over primary's path, and for an
old primary that does not answer its demotion within the demotion hold
it does so before the sides fence that primary (Failover). A path that
nothing following the discovery log manages is never dropped (Known
limits).

### Group on-leg layout: meta region, data region, health block

Every leg's side device of a group is split into a **meta region**
(`meta_blocks` blocks of `block_size`) followed by the **data region**
(`data_blocks` blocks). Both counts are computed automatically — never
configured — and stored in the `Group`; `model/ops.go` holds the
arithmetic. The group spans `ext_cnt` extents of `extent_size` bytes. The
two regions together are the side's **leg span**, every whole block its
extents hold, which a controller node's leg device covers (Primary
cntlr): the whole side whenever `extent_size` is a multiple of
`block_size`, and in any case all of it but a tail shorter than one block.
A `RedundMdRaid1` group's meta region is one block for the md superblock, the
blocks that hold the internal write-intent bitmap — the md bitmap superblock
plus one bit per bitmap chunk of the group, a chunk being
`RedundMdRaid1.bitmap_chunk_block_cnt` blocks, rounded up to whole blocks —
and one block for the health block; a `RedundNone` group's meta region is
the health block's one block alone. The data region is the rest.

The meta region must be large enough for the md superblock and the internal
write-intent bitmap, **plus the health block used for the leg health
check**: the last `LegHealthBlockSize` bytes of the last meta block. The
**primary** cntlr's cn agent periodically writes and reads back this block
through each connected leg device — spare legs included — (direct IO, a
magic, writer id and timestamp payload, [D6]) to decide whether the leg is
healthy, covering the whole path CN → side → side device even when md would
otherwise mask a member failure. A **standby** cntlr cannot run the block
probe — its path to every side deliberately terminates in the side's
dm-error (Disk node), so block IO through it can never succeed — and
instead reports leg health from transport liveness: a live controller per
side with the expected "ana_state" (`cnagent.md` CN11); that is what
failover readiness actually needs from a standby. md never touches this
area: the superblock sits at its fixed offset in block zero, the bitmap
right after it (the bitmap's block reservation is an upper bound), and
member data starts at the data offset of `meta_blocks` times `block_size`.
On migration the meta region is copied verbatim like any other bytes (the
md superblock must move with the data, Migrations).

## Naming

The formats below are normative and match `common/name_fmt.go`, whose
`NameFmt` builds its dm, NQN, tmpfs and local-store names from the
prefixes of `common/constants.go`:
`DmPrefix` for dm names, `NqnPrefix` for NQNs, `DefaultTmpfsPrefix` for the
CN tmpfs, and the agent's `--local-store` directory (default
`DefaultLocalStorPrefix`) for the local store. In the layouts below {dm},
{nqn}, {tmpfs} and {prefix} stand for those prefixes. Unless noted, every id
field is rendered with `IdKeyFmt` and the fields are joined by "-" (an
NQN's by ":"); a dm name's kind field is the two-character code of
dm-device kinds, an NQN's a bare hex digit (NQNs). Two of the formats
deserve their rationale up front: a side subsystem is keyed by `leg_id` and
carries **no** `dn_id`, so the two sides of a leg export one and the same
NQN from their two DNs (NQNs, Migration); a transfer NQN carries no node id
either — every enabled cntlr of the SP exports the same transfer subsystem
(Transfers), so the destination clone can pull through any of them.

The {cluster} component of every name below is the `cluster_id`, never the
`cluster_name`. Because `cluster_id` folds in `ClusterConf.creation_epoch`
(cluster_id derivation), a cluster deleted and recreated under the same name
produces a disjoint set of dm-device, NQN and local-file names, so a new
incarnation never collides with leftovers of the old one on a node that was
not cleaned up. The one cluster-free name is the mdadm superblock name
`CnMdArrayName` (md names): it folds in only `sp_id`, which restarts at one
in every cluster, so a recreated cluster can reproduce a superblock name
byte for byte. The exposure is bounded — every create and assemble names its
member devices explicitly ("Make sure all groups are available") and every
new side has its meta region, where an md superblock sits, zeroed before
it enters an array (Side provisioning protocol) — but a stale array of a
previous incarnation on an uncleaned node is not distinguishable by its
superblock name alone.

### dm-device kinds

The kind is a **role letter plus a hex digit** (Teardown by sweep): `d` for
a dn-agent device, `c` for a cn-agent one. The letter is not decoration. One
kernel can host both agents, and `dn_id` and `cn_id` come from two separate
counters (`DnGlobal.next_id`, `CnGlobal.next_id`; Globals: id allocation +
shard buckets) that hand out the same numbers, so a bare digit would leave a
dn side device and a cn raid0 of the same digit with the same field count
and nothing for a parse to separate them by. An agent decides what to
remove by reading back the names the kernel holds (Teardown by sweep), so
the hazard is mis-attribution: a name decoded into the wrong role is a
device torn down by the wrong agent. Non-attribution is the safe failure: a
name that carries a dnv prefix but decodes to nothing is dropped by every
attribution path — a failed `ParseDmName` keeps the device out of each
agent's own set, an md array with such a member is never stopped, and an
NQN in the dnv namespace that does not decode is never touched
(`IsDnvNqn`) — so no agent ever sweeps it, except under a cn's kind-`cb`
prefix: that prefix already names the role, the cluster and the cn, and
that cn's node-level sweep removes an unwanted name under it without
decoding the rest (`cnagent.md` CN2).

The kinds are the `DmKind` constants. DN side: `d0` error, `d1` linear,
`d2` migr-src, `d3` migr-final (dm-clone), `d4` side (the [D13] extent-run
concat), `d5` migr-meta (the dm-clone metadata wrapper). CN side: `c0`
pool-meta, `c1` pool-data, `c2` pool-final (thin-pool), `c3` thin-dev, `c4`
raid0, `c5` error, `c6` ns-dev, `c7` clone-final (dm-clone), `c8`
xfer-final. The CN kinds `c9` (the leg wrapper `CnLegName`), `ca` (the
`RedundNone` group device `CnGrpName`) and `cb` (the clone-metadata wrapper
`CnCloneMetaDmName`, [D14]) are cn-agent-internal and are defined in
`cnagent.md`, Names and constants in `common`; they follow the same
{dm}-{cluster}-{cn}-{kind}-… layout as the rows of dm device names.

### dm device names

These are the `dmsetup` names; a device's node path is `DmPath` of its
name, under /dev/mapper.

| function | layout |
|---|---|
| `DnErrorName` | {dm}-{cluster}-{dn}-d0-{sp}-{side}-{cn} |
| `DnLinearName` | {dm}-{cluster}-{dn}-d1-{sp}-{side}-{cn} |
| `DnMigrSrcName` | {dm}-{cluster}-{dn}-d2-{sp}-{migr} |
| `DnMigrFinalName` | {dm}-{cluster}-{dn}-d3-{sp}-{migr} |
| `DnSideName` | {dm}-{cluster}-{dn}-d4-{sp}-{side} — the side's data device ([D13]) |
| `DnMigrMetaDmName` | {dm}-{cluster}-{dn}-d5-{sp}-{migr} — the wrapper over the dm-clone metadata slot |
| `CnPoolMetaName` | {dm}-{cluster}-{cn}-c0-{sp}-{slice} |
| `CnPoolDataName` | {dm}-{cluster}-{cn}-c1-{sp}-{slice} |
| `CnPoolFinalName` | {dm}-{cluster}-{cn}-c2-{sp}-{slice} |
| `CnThinDevName` | {dm}-{cluster}-{cn}-c3-{sp}-{td}-{slice} |
| `CnRaid0Name` | {dm}-{cluster}-{cn}-c4-{sp}-{td} |
| `CnErrorName` | {dm}-{cluster}-{cn}-c5-{sp}-{td} |
| `CnNsDevName` | {dm}-{cluster}-{cn}-c6-{sp}-{ns} — one per **namespace**, not per td (Primary cntlr, Subsystems, namespaces) |
| `CnCloneFinalName` | {dm}-{cluster}-{cn}-c7-{sp}-{clone} |
| `CnXferFinalName` | {dm}-{cluster}-{cn}-c8-{sp}-{xfer} |

The leg wrapper of Primary cntlr is agent-internal — it is CN dm kind `c9`,
`CnLegName` in `cnagent.md`, Names and constants in `common` — and, like
kinds `ca` and `cb`, is not part of the cross-component contract [D1].

`common.ParseDmName` is the reverse of this table, and how a sweep decides
whether a name the kernel handed back is its own — except under a cn's
kind-`cb` prefix, which that cn's node-level sweep matches without decoding
(dm-device kinds). It is strict on purpose:
the dm prefix, a kind this build knows, and exactly the id count that kind
carries, each a full `IdKeyFmt` field. A name that does not decode exactly
is "not a dnv name" rather than a partly-read one, because the caller's next
move is a removal. Every kind of both roles carries the **sp id first**,
which is what lets a sweep group a node's devices by storage pool without
consulting any stored plan (Teardown by sweep).

### md names

`getShortId` of a cluster id and a node id is the low half of the fnv64a
hash of the two ids, each rendered with `IdKeyFmt` and concatenated. The
slice field of both md names is the group's slice index with its high bit
set when the group is a **meta** group, the plain slice index otherwise;
slice and group fields are two hex digits each.

* `CnMdDevName` of a cluster, cn, sp, slice index, group index and meta flag
  is {short}{sp}{slice}{grp}: the short id of the cluster and the cn in
  eight hex digits, then the sp id, the slice field and the group index,
  with no separators — so that even as the kernel disk name of a named array
  (the name behind an "md_" prefix) it stays within the kernel's disk-name
  limit ("DISK_NAME_LEN"). It names the node path, `MdPath` of the name,
  under /dev/md.
* `CnMdArrayName` of an sp, slice index, group index and meta flag is
  {dnv}-{sp}-{slice}-{grp}, where {dnv} is `DnvPrefix`: the array's mdadm
  `--name`, its superblock name, within mdadm's name limit. Its fixed
  prefix is what the udev mask of Components: invocation reference matches,
  so that only the agent assembles dnv arrays (the cn integration suite
  installs one).

The group index is the group's index inside its `meta_grp_list` or
`data_grp_list` (stable: a surviving group is never renumbered, because
groups are appended at the tail and removed only from the tail, by the
drain of a deleting SP, `dnv-worker.md` SPD10). Its two
hex digits bound it, so `GrowSlice` refuses to append to a list that
already holds `MaxGrpCntPerSlice` groups (GrowSlice): the index always fits
its field, and a test fails if the ceiling is raised past what the field
can carry.

### NQNs

The NQN kinds are the `NqnKind` constants: `0` DnHost, `1` CnHost, `2`
SideToCn, `3` MigrSrc, `4` Xfer. The fields of an NQN are joined by ":".

| function | layout and use |
|---|---|
| `DnHostNqn` | {nqn}:0:{cluster}:{dn} — the hostnqn a DN uses when it connects out (a migration destination pulling from the source). |
| `CnHostNqn` | {nqn}:1:{cluster}:{cn} — the hostnqn a CN uses toward sides and transfer targets, and toward any other clone source (a migration source is reached by the destination's dn agent, with `DnHostNqn`); also what users put into `Transfer.allowed_hosts`. |
| `SideToCnNqn` | {nqn}:2:{cluster}:{sp}:{leg}:{cn} — the subsystem a side exports to one CN. Keyed by the **leg**, with no dn component: during a migration the leg's source and destination sides sit on two different DNs and export this identical NQN, so the CN's kernel aggregates the two exports into a single nvme multipath namespace and ANA decides which path carries IO (Migration, [D1]). Outside a migration a leg has one side and the subsystem exists once. |
| `MigrSrcNqn` | {nqn}:3:{cluster}:{dn}:{sp}:{migr} — the subsystem the migration source side exports to the destination DN. |
| `XferNqn` | {nqn}:4:{cluster}:{sp}:{xfer} — the subsystem a transfer exports; no node id, so every enabled cntlr of the SP exports the identical subsystem (Transfers) and the destination clone may pull through any of them. The destination SP's user passes it as `Clone.src_nqn`. |

### tmpfs / file names

dnv uses no LVM ([D13] on the DN, [D14] on the CN): a DN carries the [D13]
disk format, a CN carries the loop-backed clone-metadata arena (Controller
node, common), and every dnv device name is a dm name of dm device names
(kinds `c9`, `ca` and `cb` per dm-device kinds).

| function | layout |
|---|---|
| `CnTmpfsPath` | {tmpfs}/{cluster}-{cn} |
| `CnTmpFileName`, `CnTmpFilePath` | the fixed name `CnTmpFileName` returns, and that file inside `CnTmpfsPath` |

### Agent local-store paths

The agent's persistent state (Common agent rules) lives as flat protobuf
files under the `--local-store` directory, {prefix} below (default
`DefaultLocalStorPrefix`, a directory that must exist before the agent
starts, `dnagent.md` SH3). The default is not /var/tmp, where the stock
tmpfiles rule of some distributions deletes files that nothing has touched
for weeks: a store file is written only when a request for its object is
accepted and read only at startup, so the file of an object that stays quiet
that long would go while its agent runs, and the next restart would find it
lost. {src_slice_idx} and {bm_idx} are both rendered with `BmIdxFmt`.

| function | path | content |
|---|---|---|
| `LocalDnPath` | {prefix}/dn-{cluster}-{dn} | the last accepted `SyncupDnRequest` |
| `LocalSidePath` | {prefix}/side-{cluster}-{dn}-{sp}-{side} | the last accepted `SyncupSideRequest` |
| `LocalCnPath` | {prefix}/cn-{cluster}-{cn} | the last accepted `SyncupCnRequest` |
| `LocalCntlrPath` | {prefix}/cntlr-{cluster}-{cn}-{sp}-{cntlr} | the last accepted `SyncupCntlrRequest` |
| `LocalMigrBmPath` | {prefix}/migr-bm-{cluster}-{dn}-{sp}-{migr}-{bm_idx} | one received `PushMigrBitmapRequest` chunk (Bitmap push protocol) |
| `LocalCloneBmPath` | {prefix}/clone-bm-{cluster}-{cn}-{sp}-{clone}-{src_slice_idx}-{bm_idx} | one received `PushCloneBitmapRequest` chunk (Bitmap push protocol); a clone chunk is addressed by the PAIR, so the name carries two index segments where the migration name carries one |

## etcd data model

### Key grammar

{p} is the etcd key prefix `DnvPrefix`. Every stored protobuf message has
one key schema, declared as a comment above the message in
`pb/schema.proto`. The fields of a key are joined by a single space.
Formatting: ids with `IdKeyFmt`, shard codes with `ShardCodeFmt`, free
extent counts with `FreeSpaceFmt`, `bin_idx` with `BinIdxFmt`, bitmap
indexes with `BmIdxFmt`. **Every id in a key uses `IdKeyFmt`**. Values are
the binary-serialized protobuf messages.

### cluster_id derivation

`cluster_id` is derived from **two** inputs: the `cluster_name` and the
cluster's `ClusterConf.creation_epoch`, stamped by the gateway inside
`CreateCluster` and never changed afterwards. `model.ClusterId` is the
fnv64a hash of the raw name bytes followed by the epoch as a big-endian
uint64 — no separator, no text formatting. Consequences that the rest of
this document relies on:

* **`cluster_id` is not computable from a request alone.**
  {p} cluster_conf {cluster_name} is the only name-keyed message; every
  other cluster-scoped key is `cluster_id`-keyed (`WorkerReg`, Key table, is
  the one cluster-independent key and carries neither). So any RPC beyond
  `CreateCluster` and `ListClusters` must first read `ClusterConf` to learn
  `creation_epoch`, derive `cluster_id`, and only then build its keys (STM
  discipline, `service Gateway` — RPC specifications).
* **Delete and recreate under the same name yields a fresh `cluster_id`,**
  hence a fresh etcd key space and fresh node-local names — `getShortId`
  (md names), the dm names (dm device names) and the NQNs (NQNs) all fold in
  `cluster_id`; the mdadm superblock name `CnMdArrayName` alone does not
  (Naming records that carve-out). Leftover keys of a previous incarnation
  can never be mistaken for the new one's, node-local objects are disjoint
  by name apart from that carve-out, and a re-created cluster never inherits
  stale agent state.
* **`creation_epoch` is immutable.** It is not a `CreateClusterRequest`
  field and no RPC updates it; rewriting it in etcd orphans every other key
  of the cluster.

### Key table

| message | key | notes |
|---|---|---|
| `ClusterConf` | {p} cluster_conf {cluster_name} | the only **name**-keyed message; holds `creation_epoch` (the second `cluster_id` input, cluster_id derivation) and the cluster-wide QoS, bdev, bin, alloc and health-check confs |
| `DnGlobal` | {p} dn_global {cluster_id} | `next_id`, `shard_bucket` |
| `CnGlobal` | {p} cn_global {cluster_id} | idem |
| `SpGlobal` | {p} sp_global {cluster_id} | idem |
| `DnRev` | {p} dn_rev {shard_code} {cluster_id} {dn_id} | watched by dn-role workers; value = `addr_port` + `revision` |
| `CnRev` | {p} cn_rev {shard_code} {cluster_id} {cn_id} | watched by cn-role workers; value = `addr_port` + `revision` |
| `SpRev` | {p} sp_rev {shard_code} {cluster_id} {sp_id} | watched by sp-role workers; value = `sp_name` + `revision` |
| `DnConf` | {p} dn_conf {cluster_id} {addr_port} | includes `location` |
| `CnConf` | {p} cn_conf {cluster_id} {addr_port} | includes `location` |
| `DnCapacity` | {p} dn_capacity {cluster_id} {bin_idx} {free_ext_cnt} {addr_port} | allocation index (Capacity index keys); value = the location copy [D5] |
| `CnCapacity` | {p} cn_capacity {cluster_id} {free_ext_cnt} {addr_port} | idem |
| `CdcEntry` | {p} cdc {cluster_id} {shard_code} {sp_id} {ss_id} | the shard code is the SP's; watched by dnv-cdc; its `nvme_tr_conf_list` lists the cntlrs of [D18] |
| `SpConf` | {p} sp_conf {cluster_id} {sp_name} | |
| `Cntlr` | {p} cntlr {cluster_id} {sp_id} {cntlr_id} | |
| `Slice` | {p} slice {cluster_id} {sp_id} {slice_id} | groups, legs and sides embedded |
| `ThinDevice` | {p} thin_device {cluster_id} {sp_id} {td_name} | |
| `Subsystem` | {p} subsystem {cluster_id} {sp_id} {nqn} | namespaces embedded |
| `Clone` | {p} clone {cluster_id} {sp_id} {clone_name} | |
| `CloneBitmap` | {p} clone_bitmap {cluster_id} {sp_id} {clone_name} {src_slice_idx} {bm_idx} | one chunk of source slice `src_slice_idx`'s bitmap, self-positioned at byte `bm_idx` times `CloneBmChunkBytes` within it (Bitmap push protocol); `src_slice_idx` is below `src_slice_cnt`, itself at most `MaxSliceCntPerSp`, and `bm_idx` is below `MaxCloneBmCnt` |
| `Transfer` | {p} transfer {cluster_id} {sp_id} {xfer_name} | |
| `Migration` | {p} migration {cluster_id} {sp_id} {migr_name} | |
| `MigrBitmap` | {p} migration_bitmap {cluster_id} {sp_id} {migr_name} {bm_idx} | `bm_idx` is the append sequence from zero |
| `SpName` | {p} sp_id_to_name {cluster_id} {sp_id} | reverse lookup for admin and log analysis; needed because `SpRev`, which also carries `sp_name`, can only be addressed with the SP's shard code [D10] |
| `WorkerReg` | {p} worker {role} {seed} | the worker registry; value = the writer's heartbeat `epoch` (unix seconds, informational — liveness is judged by the observer's own clock); refreshed every `DefaultVoteWorkerInterval`, **no lease**; deleted by its owner on shutdown or by peers once committed dead (Membership and shard ownership; `dnv-worker.md` VW2, VW4, VW6 and CM5) |

An **invariant key** is one the data model guarantees present while its
parent exists: the rev keys, the globals, the record of every object a
conf lists, and the node conf a stored side or cntlr names, which no node
delete removes while the node hosts one. A read that reaches one through
its parent and finds it absent has found a lost invariant: the gateway
answers it `ABORTED` (UNEXPECTED_ERROR → `ABORTED`), or
`FAILED_PRECONDITION` where one of the `model` mutations it shares with
the worker made the read, and never `NOT_FOUND`, which is for an object
the request itself names (`gateway.md` GW7).

### Globals: id allocation + shard buckets

`DnGlobal`, `CnGlobal` and `SpGlobal` each hold `next_id`, which starts at
one, and a `shard_bucket` of `ShardBucketSize` entries, all zero at cluster
creation. Creating a DN, CN or SP inside the STM:

1. The id is `next_id`, and `next_id` advances by one. Ids are never reused
   ("all ids in a cluster are incremental"), which lets agents assume a
   deleted object never comes back.
2. The `shard_code` is the index of the **smallest** bucket value, the first
   index on ties, and that bucket is incremented.
3. The sum of `shard_bucket` is the live object count, so the
   `MaxDnCntPerCluster`, `MaxCnCntPerCluster` and `MaxSpCntPerCluster` checks
   never need a range query. Deletion decrements the bucket.

`gateway.md` GW12 carries this out. Per-SP sub-object ids (`cntlr_id`,
`slice_id`, `grp_id`, `leg_id`, `side_id`, `ss_id`, `ns_id`, `td_id`,
`clone_id`, `xfer_id`, `migr_id`) all come from the single `SpConf.next_id`
counter, which starts at `SpFirstId`. Thin-device `dev_id`s come from
`SpConf.next_dev_id`, which starts at one (an `ori_id` of zero means "no
origin"), and are never reused.

### Revision keys and the sync fan-out

The rev keys are addressed by **id**:
{p} dn_rev {shard_code} {cluster_id} {dn_id}, and likewise {cn_id} and
{sp_id}. Their values hold the `revision`, which starts at one on creation,
**plus the mutable handle the watching worker needs**: `DnRev.addr_port` and
`CnRev.addr_port` (the agent's gRPC endpoint, the `DnConf` or `CnConf` key
suffix) and `SpRev.sp_name` (the `SpConf` key suffix) [D10]. Rules:

* Any STM that changes agent-visible desired state of a DN, CN or SP bumps
  the matching revision **once**, even if it touched many sub-keys.
* `err_epoch` and capacity-key maintenance never bump revisions: they gate
  control-plane scheduling and, for a cntlr, its place in the discovery
  records ([D18]), whose `CdcEntry` keys dnv-cdc watches directly, so no
  agent needs to hear of either. Every `SpConf` or sub-object mutation of
  `service Gateway` — RPC specifications bumps `SpRev` unless the RPC's
  spec says otherwise.
* The request-side `DnRev`, `CnRev` and `SpRev` fields are
  optimistic-concurrency tokens, and the check is **presence-based**. When
  the request carries the token message, the STM asserts that the stored
  revision equals the request's and fails the RPC with `ABORTED` ("stale
  revision") on a mismatch. When the message is absent, the STM skips the
  assertion and the mutation proceeds ungated: a client omits the token
  precisely to opt out, per request, and the residual is the lost update it
  accepts by omitting it. Presence, not value, selects the two modes: a
  stored revision starts at one and only grows, so a message present with a
  zero `revision` is a real token that can never match and is always
  refused. Only `revision` participates — the `addr_port` or `sp_name` a
  client echoes back from `Get*` inside that message is **ignored** on the
  request path (it is never a way to rename or relocate anything), so a
  message carrying only that echo is a present zero token, refused like any
  other; the RPC still names its target by `addr_port` or `sp_name` in its
  own top-level field. `Get*` returns the current token. `gateway.md` GW6
  carries the check out.
* The handle in the value is a copy of the authoritative one (`DnConf` and
  `CnConf` are keyed by `addr_port`, `SpConf` by `sp_name`). No RPC changes
  a node's `addr_port` or an SP's `sp_name`; any path that does must rewrite
  the rev value in the same STM, and **without** deleting and recreating the
  rev key: the key is id-based and therefore stable, so watchers see one
  put, not a delete and a put that would read as "node removed, then a
  different node added".
* Workers watch the rev prefixes of the shard codes they own and react as
  Workers describes. A watch event is self-sufficient: the key gives
  `cluster_id` and the object id, the value gives the revision and the
  `addr_port` or `sp_name` needed to form the next key or dial the agent —
  no extra lookup just to learn where to go.

### Capacity index keys

`DnCapacity` and `CnCapacity` are pure indexes for the allocator: the key
encodes the `bin_idx` (DN only) and the `free_ext_cnt`, so a lexicographic
range scan returns nodes ordered by free space (`FreeSpaceFmt` zero-pads, so
lexical order is numeric order). The value carries a copy of the node's
`location`, so a scan needs no extra point reads; `DnConf` and `CnConf` are
authoritative [D5].

**Presence rule.** A node has a capacity key **iff it is currently
allocatable**. The capacity key is absent while any of these holds:

* DN: `side_ptr_list` holds `MaxSideCntPerDn` entries or more; CN:
  `cntlr_ptr_list` holds `MaxCntlrCntPerCn` entries or more;
* `err_epoch` is nonzero (unhealthy);
* `disabled` is set;
* DN: `free_ext_cnt` is below the level of bin 0 (DN bins); CN:
  `free_ext_cnt` is zero.

Whichever STM changes one of those inputs (allocation or free of extents,
pointer-list growth or shrink, a worker setting or clearing `err_epoch`)
also deletes or rewrites the capacity key in the same transaction —
delete-if-present, recreate-when-allocatable (`dnv-worker.md` MD4 implements
the rule). Consequently the allocator (Allocation) never has to filter by
health or flags: everything it can see is eligible (it still applies the
black and white lists, location dedupe and the per-SP CN exclusion).

### page_token

The token is the last returned key in standard base64; it is decoded the
same way, and the page it asks for starts at the first key under the prefix
that sorts after the decoded one. A decode failure is `INVALID_ARGUMENT`; an
empty token is the start of the prefix (`gateway.md` GW10 carries the paging
out). The four paged lists — `ListClusters`,
`ListDiskNodes`, `ListControllerNodes`, `ListStoragePools` — never use the
STM, but every one of them except `ListClusters` still needs one plain read
of {p} cluster_conf {cluster_name} first (`NOT_FOUND` if absent) to derive
the `cluster_id` its prefix is built from (cluster_id derivation).
`ListThinDevices` and `ListSubsystems` take no page arguments and are each
one STM snapshot read (Thin devices; Subsystems, namespaces; `gateway.md`
GW5).

### STM discipline

"STM" is the software transactional memory of the etcd client's
`concurrency` package, which `etcdutil` wraps (`dnv-worker.md` EU4). Unless
a spec says otherwise, all etcd reads and writes of one RPC happen in
**one** STM transaction. Everything computable outside (name formatting,
candidate lists) is prepared beforehand to keep transactions short.
`cluster_id` is **not** among those: it depends on
`ClusterConf.creation_epoch` (cluster_id derivation), so the
{p} cluster_conf {cluster_name} read is the first step **inside** the STM
and every other key of the RPC is formatted from the `cluster_id` derived
there. Doing it in-STM also makes the transaction fail correctly when the
cluster is concurrently deleted or recreated. `GrowSlice`, `CreateSpareLeg`
and `SwitchSpareLeg` are the exceptions: they make that read in a planning
snapshot (`gateway.md` GW5). Network calls to agents (`GetDnSize` and
`GetCnSize`, the `Get*Info` behind `Inspect*` and behind the `force` false
hydration checks of Clones and Migrations, the bitmap reads) must happen
**outside** the STM, before or after it; when such a call needs
`cluster_id`, a plain pre-read of `ClusterConf` provides it and the in-STM
read stays authoritative.

### UNEXPECTED_ERROR → `ABORTED`

The catch-all `ABORTED` covers STM-client errors (not app logic inside the
STM), protobuf (de)serialization errors and etcd connection errors, plus the
specific `ABORTED` cases listed per RPC (missing invariant keys, agent RPC
failures where specified, stale revision tokens). A **stored** conf that
fails the validation of Common validation belongs here too: it is a lost
invariant like a missing key, never `INVALID_ARGUMENT`, because nothing
about the request is wrong. `gateway.md` GW7 is the gateway's error table.

## Allocation

### Size → extents

Everything is allocated in extents of `extent_size`, which is cluster-wide
(`ClusterConf.dn_bin_conf.extent_size`, default `DefaultDnExtSize`).
Counts round **down**. A DN's `total_ext_cnt` is the `GetDnSize` reply
divided by `extent_size`, rounded down; the agent already reports the
data-area bytes, the disk size less `DnDataOffset` ([D13]), so no further
subtraction exists. A CN's `total_ext_cnt` is its capacity budget divided
by `extent_size`, rounded down, where the budget comes from `GetCnSize`: a
zero reply takes `DefaultCnCap`, a reply above `MaxCnCap` is clamped to it,
and a non-zero reply below `MinCnCap` is treated like a zero.

### DN bins

The four `DnBinConf` shifts form a ladder, 0 ≤ `bin0_shift` < `bin1_shift`
< `bin2_shift` < `bin3_shift` ≤ 63 (defaults `DefaultDnBin0Shift` to
`DefaultDnBin3Shift`), and the level of bin i is one shifted left by its
shift. A DN with f free extents sits in the bin b whose level is at most f
and whose next level is above f; bin 3 holds every f at or above its own
level, and an f below bin 0's level is in no bin (Capacity index keys).
Bins balance two failure modes: (a) always draining one big DN before
touching the next, and (b) spreading so thin that no single DN can satisfy
a request although the cluster has plenty of space in aggregate.

The ladder is fixed once, at `CreateCluster`: all four shifts zero is the
proto3 "unset" that asks for the default ladder and is accepted, and any
other set must already **be** a ladder — an invalid one is
`INVALID_ARGUMENT` (Clusters), never silently replaced by the defaults,
which would hand the operator a cluster binned differently from the one
they asked for. Every reader afterwards shifts the stored values as they
are (Common validation). Capacity index keys force both halves: a capacity
key embeds the bin it was written under and can only be deleted by a
transaction that computes the same index, so one ladder must hold for the
life of the cluster — a ladder that moved, which is what a default
substituted from the running binary would do across an upgrade, would
strand every key already in the store.

### Finding DN candidates

Input: `candExt`, the free extents each candidate needs; `candCnt`, how
many candidates are wanted; the black and white lists of `addr_port`s of
the `NodeSelector` (an empty white list admits every DN, and the black
list always excludes, even a white-listed DN); and `excludeLocs`, the
`location`s the caller already occupies. Health, flags, fullness and
side-count eligibility are already enforced by key presence (Capacity
index keys).

1. Start the taken locations as `excludeLocs` (empty for every operation
   Per-operation allocation leaves on the plain scan), and the candidates
   as none.
2. Pick the smallest bin b whose range can hold `candExt`, skipping every
   bin whose next level is at most `candExt`. For bin b through bin 3:
   range-scan that bin's `DnCapacity` keys **descending**, largest free
   first; for each DN, stop this bin when its free count is below
   `candExt`; skip the DN when the lists filter it out or its `location` is
   already taken; else take it as a candidate and its `location` as taken;
   return as soon as `candCnt` candidates are collected.
3. After bin 3, return whatever was collected; the caller decides whether
   it is enough.

The taken-locations rule gives location anti-affinity for free: two legs of
one group can never land in the same failure domain in one allocation
round. Seeding it with `excludeLocs` is how **tier 1** of Per-operation
allocation extends the same rule across rounds — a black list of
`addr_port`s excludes DNs and not their domains — and **tier 2** is this
scan with `excludeLocs` empty.

### Finding CN candidates

The same as Finding DN candidates but without bins: one descending scan
over the cluster's `CnCapacity` keys, with the taken locations seeded from
`excludeLocs` (which every CN pick of Per-operation allocation fills, at
tier 1, with the `location`s of the SP's other cntlrs); it skips
black-listed, non-white-listed and duplicate-location CNs, plus CNs that
already host a cntlr of the same SP.

### Per-operation allocation

* **CreateStoragePool — DNs.** For every group to create (per slice, one
  meta group and one data group): `candExt` is that group's `ext_cnt` — the
  initial **data** group of each slice uses `init_ext_cnt`, and the initial
  **meta** group of each slice always one extent, the first rung of the
  meta ladder (GrowSlice). `requiredCnt` is the legs per group — one under
  `RedundNone`, `MaxAllocLegPerGrp` under `RedundMdRaid1` (`dnv-worker.md`
  SPD1) — and the scan width `candCnt` is `requiredCnt` times
  `alloc_conf.dn_batch_size`. Get candidates (Finding DN candidates); fewer
  than `requiredCnt` is `RESOURCE_EXHAUSTED`; pick `requiredCnt`
  **randomly** from the list; add the picked DNs to the black list; continue
  with the next group. The growing black list means every leg of the SP
  lands on a distinct DN.
* **CreateStoragePool — CNs.** `candExt` is the sum of `ext_cnt` over
  **all** groups of the SP; `requiredCnt` is one and `candCnt` is
  `alloc_conf.cn_batch_size`; repeat `cntlr_cnt` times, a random pick that
  is then black-listed. Placement is **two-tier**, as for CreateMigration
  below: **tier 1** also excludes the `location`s of the CNs picked so far,
  and **tier 2** rescans without that exclusion (the black list still
  applies) when tier 1 yields no candidate — so the cntlrs of one SP land in
  distinct failure domains as long as a domain none of them holds yet has a
  CN with room, and a cluster with fewer such domains than `cntlr_cnt`
  still places them when it has `cntlr_cnt` CNs with room. The picks'
  locations come out of the scan itself ([D5]).
* **GrowSlice** — like the DN flow for exactly one group, the new one. Its
  black list is seeded with the request's own `NodeSelector.black_list` and
  nothing else, and the gateway never appends to it: one scan-and-pick
  round serves the whole group, so a brand-new group normally scans against
  an empty list, and its legs still land on distinct DNs because the
  taken-locations rule of Finding DN candidates already left the candidate
  list they are drawn from with one entry per DN and per location. DNs that
  already carry another group of the slice or SP stay allowed on purpose: a
  grow spreads the new group, it does not avoid the slice's existing ones.
  Meta-group sizes follow the meta ladder (GrowSlice).
* **CreateMigration** — one DN; `candExt` is the group's `ext_cnt`; the
  black list starts with the DNs of every leg and side of the group.
  Placement is **two-tier**, because a black list of `addr_port`s excludes
  DNs and not failure domains: **tier 1** scans with the `location`s of
  those same DNs excluded as well (they seed `excludeLocs`, so a DN merely
  *sharing* a domain with a leg is skipped too); when tier 1 yields fewer
  candidates than `requiredCnt` — the DNs the operation must actually
  place, never the oversampled `candCnt` — **tier 2** rescans without the
  location exclusion (the black list still applies), so a cluster with too
  few domains places the destination instead of refusing. Tier 2's
  candidates are **merged behind** tier 1's rather than replacing them, so a
  distinct-domain DN tier 1 did find is never dropped by a tier-2 scan that
  fills `candCnt` out of the excluded domains. The resulting same-domain
  placement is visible in the stored topology; it is not logged separately.
  The locations are read before the STM, which is sound because `location`
  is immutable (Disk nodes) and because the STM drops the pick when the
  group, as the STM reads it, has gained a DN since the read the scan was
  planned from (the scan and the STM then re-run from a fresh read of the
  group, Migrations; `gateway.md` GW9): a side hung off the group meanwhile
  is the only way the set of domains to exclude can grow. That re-check,
  like the in-STM re-validation of the pick itself, stays address-based.
* **CreateSpareLeg** — one DN, with the same black-list seeding and the
  same two tiers as CreateMigration.
* **CreateCntlr** — one CN; `candExt` is the sum of every group's `ext_cnt`
  of the SP; the CNs of the SP's cntlrs are excluded (Finding CN
  candidates) and their `location`s form tier 1's exclusion, with the same
  two tiers as CreateStoragePool's CNs. The locations are read before the
  STM, which is sound because `location` is immutable (Controller nodes) and
  because the STM drops the pick when the SP, as the STM reads it, has a
  cntlr on a CN the read the scan was planned from did not hold (the scan
  and the STM then re-run from a fresh read of the SP's cntlrs, Cntlrs;
  `gateway.md` GW9): a cntlr committed meanwhile is the only way the set of
  CNs and domains to exclude can grow, and without the check the pick could
  land in its domain, or on its very CN when the scan ran after its charge.
  The sp worker's cntlr replacement holds the SP's surviving cntlrs to the
  plan of its own scan the same way (`dnv-worker.md` AR7). That re-check,
  like the in-STM re-validation of the pick itself, stays address-based.

Free-extent bookkeeping happens in the same STM as the pick: each leg's DN
loses the group's `ext_cnt` from its `free_ext_cnt`, and each cntlr's CN
the sum of `ext_cnt` over all the SP's groups; the capacity keys are
maintained per Capacity index keys; a delete reverses both.

## Common validation

* At most `MaxStrSize` bytes for `cluster_name`, `addr_port`, `sp_name`,
  `td_name`, `clone_name`, `xfer_name`, `migr_name`, `location` and the
  `NvmeTrConf` members, each of which must also match `ValidStrPattern`.
* NQNs (every NQN field but the `nqn` of `DeleteNamespace` and
  `DeleteSubsystem`, below): at most `MaxNqnLength` bytes, no "..", and a
  match for `ValidNqnPattern` — "nqn.", a date, a lower-case domain, a ":"
  and a suffix of letters, digits, ".", "_", ":" and "-". The pattern
  leaves out "/" and whitespace, and ".." is refused beside it, because a
  subsystem's NQN and an allowed host's each name a configfs directory. The
  pattern requires a ":" after the domain part, so the well-known discovery
  NQN, `NvmeDiscoveryNqn`, can never validate — `CreateSubsystem` needs no
  separate rejection for it.
* A subsystem's NQN must in addition lie outside the dnv namespace where a
  user picks one, `CreateSubsystem.nqn`, and where a transfer is built on
  one, `CreateTransfer.ori_nqn`: an NQN that starts with `NqnPrefix` and a
  ":" (`IsDnvNqn`, NQNs) is `INVALID_ARGUMENT` there. The agents attribute a
  subsystem by parsing its NQN (Teardown by sweep), so such a name would be
  judged as one dnv minted: the cn agent would never sweep one shaped like
  a side export's NQN after `DeleteSubsystem`, and would sweep one shaped
  like a transfer NQN naming another SP of this cluster as that SP's
  transfer export while a host still uses it. Host NQNs
  (`Transfer.allowed_hosts` carries `CnHostNqn`s), `CreateClone.src_nqn`
  (another SP's `XferNqn`, NQNs) and the `nqn` of `UpdateSubsystemHosts`,
  `CreateNamespace`, `UpdateNamespaceDev` and `UpdateNamespaceSuspended`,
  which address an existing subsystem, take only the NQN rule above. A
  subsystem already stored in the dnv namespace can therefore still be
  updated if its NQN passes the NQN rule, and emptied and deleted in any
  case (next bullet); only a transfer cannot be built on it. The rule does
  not change how the cn agent judges such a subsystem, though (Teardown by
  sweep). Unless its NQN decodes as a transfer NQN of this cluster, no sweep
  of the cn agent ever removes it or a namespace in it, and nothing else
  removes a namespace that left `ns_list`: the sweep is the only remover of
  one (`cnagent.md` CN21). So a namespace `DeleteNamespace` takes out of
  such a subsystem stays enabled on every CN that had it, and once a CN's
  cntlr stops wanting the subsystem — after `DeleteSubsystem`, at
  `SP_LEVEL_DISABLE` (SpLevel), or when that cntlr leaves the CN — the cn
  agent leaves its nvmet subsystem there too, still linked to that CN's
  port and holding every namespace that CN had enabled in it. Each such
  namespace keeps its ns-dev open, so that SP's sweep on that CN stops at
  the ns-dev layer on every pass and leaves the SP's unwanted objects in
  every lower layer in place (Teardown by sweep); once that cntlr or the SP
  is deleted, that is the rest of that SP's stack on that CN. And one that
  decodes as a transfer NQN of this cluster naming another SP is still
  swept as that SP's transfer export while a host uses it.
* `DeleteNamespace.nqn` and `DeleteSubsystem.nqn` take neither rule, only
  the length check: non-empty and at most `MaxNqnLength` bytes. Each names
  an exact subsystem key, and a string no subsystem is stored under is
  `NOT_FOUND` there. A subsystem stored under an NQN these rules refuse —
  for example an upper-case domain, a "+" or "@" in the suffix, "/", "..",
  whitespace, or the dnv namespace — can therefore still be emptied and
  deleted, and its SP after it (`DeleteStoragePool` refuses while
  `nqn_list` is non-empty, Storage pools); the bullet above says what the
  CNs keep of one in the dnv namespace. For an NQN that fails the NQN rule
  this is the only path that accepts such an NQN: every other RPC that
  names such a subsystem refuses it with `INVALID_ARGUMENT`.
* `addr_port` is the gRPC endpoint of the node's agent, a host and a port.
* **Bounded numeric parameters.** These bounds are checked on the
  **request**: a non-zero value outside its bounds is rejected, while a
  proto3 zero is always accepted — it means "unset" and asks for the
  default, which the create RPC substitutes once and for all (the
  resolution rule below, which also names the two stored messages it
  exempts). The list `count` is different: it reaches no create RPC and is
  stored nowhere — the paged list RPCs resolve it per request, cut one page
  with it and throw it away (page_token; `gateway.md` GW4). The bounds and
  the defaults are constants of `common/constants.go`:
  * `dn_bin_conf.extent_size`: `MinDnExtSize` to `MaxDnExtSize`, default
    `DefaultDnExtSize`;
  * `alloc_conf.dn_batch_size`: `MinAllocDnBatchSize` to
    `MaxAllocDnBatchSize`, default `DefaultAllocDnBatchSize`, and
    `alloc_conf.cn_batch_size` likewise with `MinAllocCnBatchSize`,
    `MaxAllocCnBatchSize` and `DefaultAllocCnBatchSize`;
  * `dm_pool_conf.data_block_size`, a power of two:
    `MinDmPoolDataBlockSize` to `MaxDmPoolDataBlockSize`, default
    `DefaultDmPoolDataBlockSize`;
  * `dm_pool_conf.low_water_mark_pct`: no bounds; default
    `DefaultPoolLowWatermarkPct`, which a zero selects. It is never
    rejected: a value above 100 is accepted and switches the auto-grow of
    Automatic reactions **off** (`pb/schema.proto`); the agent then passes
    a zero `low_water_mark` to the thin-pool table — no dm events;
  * `dm_raid0_conf.stripe_size`, a multiple of `MinDmRaid0StripeSize`:
    `MinDmRaid0StripeSize` to `MaxDmRaid0StripeSize`, default
    `DefaultDmRaid0StripeSize`;
  * `redund_md_raid1.bitmap_chunk_block_cnt`: `MinChunkBlockCnt` to
    `MaxChunkBlockCnt`, default `DefaultChunkBlockCnt`;
  * the `DmCloneConf` hydration pair, of a clone and of a migration alike:
    `hydration_threshold` from one to `MaxCloneThreshold` and
    `hydration_batch_size` from one to `MaxCloneBatchSize`; defaults
    `DefaultMigrThreshold` and `DefaultMigrBatchSize` for a migration and,
    for a clone, the dm-clone target's own (below);
  * the four `EventThreshold` members, in seconds — `primary_unhealthy`,
    `cntlr_unhealthy`, `side_unhealthy` and `leg_unhealthy`: any non-zero
    value, no maximum; defaults `DefaultPrimaryUnhealthy`,
    `DefaultCntlrUnhealthy`, `DefaultSideUnhealthy` and
    `DefaultLegUnhealthy`;
  * `health_check_conf.dn_interval`, `cn_interval`, `side_interval` and
    `cntlr_interval`, in seconds: `MinHealthCheckInterval` to
    `MaxHealthCheckInterval`, default `DefaultHealthCheckInterval`;
  * the list `count`: at most `MaxListCnt`, default `DefaultListCnt`.
* **Geometry rules.** Beyond the bounds above, `CreateCluster` and
  `CreateStoragePool` hold the geometry of the `bdev_conf` they store to
  four rules (`INVALID_ARGUMENT` otherwise), each refusing up front what
  dm-thin, dm-clone, mdadm or `CreateClone` would otherwise refuse, or mdadm
  mishandle, only once an SP with that geometry exists, and then for ever,
  because an SP's stored geometry never changes (Storage pools).
  `data_block_size` is a power of two: `MinDmPoolDataBlockSize` is the unit
  dm-thin demands of its block, so a power of two at or above it is a
  multiple of that unit, and the block is the dm-clone region of every
  clone into the SP and of every migration of one of its sides (Clones,
  Migration), which dm-clone refuses unless it is a power of two.
  `stripe_size` is a multiple of `MinDmRaid0StripeSize` and at most
  `MaxDmRaid0StripeSize`, and `data_block_size` is a multiple of
  `stripe_size` — the source rules of raid0 bitmap math that `CreateClone`
  enforces (Clones); under a power-of-two block that makes the stripe a
  power of two too. Under `redund_md_raid1` the md bitmap chunk,
  `bitmap_chunk_block_cnt` times `data_block_size` bytes (Group on-leg
  layout: meta region, data region, health block), is a power of two, which
  mdadm demands of `--bitmap-chunk`, and at most `maxMdBitmapChunk`, the
  largest chunk mdadm turns into bytes without overflowing the signed int it
  keeps the chunk in. That ceiling follows mdadm's source: the next power of
  two overflows that int, the one after it wraps
  it to zero, which mdadm then refuses, and md stores the chunk as a 32-bit
  byte count. So every SP created under these rules is a legal clone
  source, and its block is a region dm-clone takes wherever the SP is the
  destination of a clone or a migration. A zero member is unset here as
  well, so a request is judged by a rule between two members only when it
  sets both; every rule is then judged again on the `bdev_conf` the create
  RPC stores — `CreateCluster`'s resolved against the constants,
  `CreateStoragePool`'s merged over `ClusterConf.bdev_conf` and resolved
  (Storage pools) — because only that message shows what an omitted member
  becomes. An SP therefore cannot inherit a member these rules refuse, even
  from a cluster whose stored conf breaks them. An SP whose stored geometry
  breaks them keeps it, and no RPC judges a stored SP's `bdev_conf` against
  them: `CreateClone` still refuses one whose stripe or block breaks the
  source rules of raid0 bitmap math as a source, and one whose
  `data_block_size` is not a power of two still fails, at its dm-clone,
  every clone into it and every migration of one of its sides.
* `EventThreshold.leg_unhealthy` must be greater than
  `EventThreshold.side_unhealthy` after the defaults above are resolved
  (`INVALID_ARGUMENT` otherwise): the leg repair of Automatic reactions
  fires on the side threshold when the DN itself looks dead and on the leg
  threshold when only the cntlr's path is bad, so the leg threshold is the
  longer wait by construction (`dnv-worker.md` AR8).
* `EventThreshold.primary_unhealthy`, after its default is resolved, must
  be at least two of the cluster's `health_check_conf.cntlr_interval`
  (`INVALID_ARGUMENT` otherwise). The rule reads the cluster as well as the
  request, so `CreateStoragePool` judges it against the stored
  `ClusterConf`, on its plain pre-read before the scans and again in its
  STM (Storage pools). The reason: a check round that gets no reply stamps
  the primary's `err_epoch`, and the next round, due one interval later,
  clears it when that round is clean (`dnv-worker.md` RW4, RW8), so the
  epoch of one missed round lives about one interval, and the rule leaves
  a second interval of margin before that epoch can fail the primary
  over. Epochs are whole seconds and the next answer can come late, so the
  margin alone can be crossed; the failover therefore also needs two
  unhealthy verdicts in a row from the primary's check rounds
  (`dnv-worker.md` AR10), which one missed round followed by a clean answer
  never gives. The defaults meet
  the rule exactly, `DefaultPrimaryUnhealthy` being two of
  `DefaultHealthCheckInterval`, so an SP on a cluster whose
  `cntlr_interval` is longer than its default must set `primary_unhealthy`
  itself.
* `QosRatio.bytes_per_iops` and `bytes_per_bps` are **not** range-checked:
  nothing enforces them (Controller node, common), and any value is stored
  as sent.
* `bdev_feature_list` must be empty (`INVALID_ARGUMENT`
  otherwise); `RedundConf` accepts only `redund_none` and
  `redund_md_raid1`.
* `ClusterConf.creation_epoch` is never user input — no request message
  carries it, `CreateCluster` stamps it (Clusters) and no range check
  applies. It is in unix nanoseconds, not the unix seconds of `err_epoch`.
* **Defaults are resolved once, at the create RPC, and the stored conf is
  concrete.** The rungs are `gateway.md` GW11's; an SP's stored conf is
  none of them, being the geometry later RPCs compute with. Where a member
  has no default its proto3 zero stands and keeps meaning
  "unset" (`qos_ratio`, Controller node, common) — but a defaultable member
  is settled on the **write** path and never again: `CreateCluster`
  resolves a `ClusterConf` (Clusters) and `CreateStoragePool` an SP's
  `bdev_conf` (Storage pools), and the resolved message is what lands in
  etcd (`gateway.md` GW11 carries this out). Order inside that RPC is
  load-bearing: the bounds above are checked on the **raw** request, where
  a zero still means "give me the default", and only the accepted request
  is resolved — resolving first would make every bound check a tautology.
* A zero read back **out of** etcd is therefore invalid, not "unset".
  Every reader refuses the object rather than substituting: the gateway
  with `ABORTED` (UNEXPECTED_ERROR → `ABORTED`), the worker by skipping that
  object's pass (one Error record, nothing sent, retried next round;
  `dnv-worker.md` RW9, RW14 and AR1), an agent by refusing the `Syncup*`
  (Common agent rules). Two reasons, both about geometry rather than
  tidiness: a consumer that forgets to resolve computes with zeros instead
  of failing, and a stored zero pins the geometry to whatever default the
  **reading** binary carries — so changing a constant would re-geometry
  live storage pools and move the bin ladder every capacity key was written
  under (DN bins). There is no compatibility shim: a cluster whose stored
  conf holds zeros is refused loudly and must be recreated.
* Two stored messages are deliberately exempt, because they are policy
  timers and knobs rather than geometry — nothing is formatted or addressed
  with them: `EventThreshold` is stored exactly as sent (so an operator
  reads back what they asked for) and resolved member-wise when read
  (Automatic reactions), and the `DmCloneConf` hydration pair is stored as
  sent too — the sp worker resolves a migration's as it builds the
  `SyncupSide` request (`dnv-worker.md` RW15), while a zero in a clone's
  simply leaves the dm-clone target's own default in place (Clones).
* `CmdSoftTimeout` and `CmdHardTimeout` bound the shell commands an agent
  runs, a child blocked in an uninterruptible kernel wait aside. A command
  they end did not answer: its resource reports `RES_STATUS_ERROR` with
  the command's output in `details`, never an absence (`dnagent.md` SH15).
  `osclient.md`, RunCommand, owns the two signals and the caveat.

## `service Gateway` — RPC specifications

Each RPC is specified as **Errors**, **Defaults** and **Action**. This is
the API contract; `gateway.md`, Handlers by resource group, carries each
RPC out. Errors common to every RPC, and therefore not repeated:
`INVALID_ARGUMENT` for any violation of Common validation on any supplied
field; `ABORTED` per UNEXPECTED_ERROR → `ABORTED`; `ABORTED` "stale
revision" whenever the request **carries** a `DnRev`, `CnRev` or `SpRev`
token whose revision mismatches the stored one — omitting the token
message skips that check entirely (Revision keys and the sync fan-out;
`gateway.md` GW6). `cluster_name` defaults to `DefaultClusterName` for
every RPC that takes one. Every RPC except `CreateCluster` and
`ListClusters` starts by reading the `ClusterConf` of its `cluster_name`
(`NOT_FOUND` if absent) — not only as an existence check, but because the
`cluster_id` derived from its `creation_epoch` (cluster_id derivation) is
the prefix of every other key the RPC touches. An RPC that names an SP then
also resolves the `SpConf` of its `sp_name` (`NOT_FOUND` if absent). Both
checks are implied below, and both live inside the RPC's STM, except in
the RPCs that make the `ClusterConf` read in a planning snapshot
instead (STM discipline; `gateway.md` GW5). In every mutating SP RPC
except `DeleteStoragePool` the deciding transaction refuses an SP whose
`SpConf.deleting` is true with `FAILED_PRECONDITION`, so nothing such an
RPC writes lands on a deleting SP; which other answers a request can meet
first is `gateway.md` GW6's order. `DeleteStoragePool` is the one RPC that
sets the flag (Storage pools), the sp worker's drain is what acts on it
(`dnv-worker.md`, The sp drain), and the latch is one-way (`dnv-worker.md`
SPD5).

### Clusters

**CreateCluster** — the only RPC that computes `cluster_id` without
reading `ClusterConf` first, because it is the RPC that mints
`creation_epoch`.
Errors: `ALREADY_EXISTS` if the `ClusterConf` of `cluster_name` exists or
any of the `DnGlobal`, `CnGlobal` and `SpGlobal` of the `cluster_id`
exists (the hash-collision guard), checked inside the STM;
`INVALID_ARGUMENT` when `dn_bin_conf`'s shifts are set to anything that is
not a ladder (DN bins — all four zero asks for the default and is
accepted), or when the resolved `bdev_conf` breaks a geometry rule of
Common validation (a member the request omitted is judged as its
constant, and the message says the defaults were filled in).
Defaults: the defaults of Common validation for every `ClusterConf`
member, applied **here** and stored concrete — `ClusterConf` is
write-once, so this is the only chance its members ever get to be
resolved (Common validation; `gateway.md` GW11). `creation_epoch` is
**not** a request field: the gateway stamps it, the wall clock in
nanoseconds, once, immediately before entering the STM, and derives
`cluster_id` from it (cluster_id derivation). Internal STM retries of that
one attempt reuse the stamped epoch, so they stay idempotent; a client
that retries after a failed `CreateCluster` stamps a new epoch and thus
targets a different `cluster_id` — which is why the collision guard is
re-evaluated inside every attempt's STM.
Action: bound-check the raw request (Common validation), then resolve it
— in that order, or the bounds check nothing — and build the message to
store once, outside the STM, like the epoch above. The resolved
`bdev_conf` is judged by the geometry rules once more before it is stored,
since a member the request omitted meets them only as its constant. One
STM creates `ClusterConf` (the **resolved** members plus the stamped
`creation_epoch`), and `DnGlobal`, `CnGlobal` and `SpGlobal`, each at its
first `next_id` with every `shard_bucket` entry zero (Globals: id
allocation + shard buckets). Reply `cluster_id`. Nothing downstream
resolves again: a zero read back out of this key is corruption and is
refused, not substituted (Common validation).
`ClusterConf` is **write-once**: `qos_ratio`, `bdev_conf`, `dn_bin_conf`,
`alloc_conf` and `health_check_conf` can only be set here — no RPC updates
a `ClusterConf`, deliberately; changing them means creating a new
cluster. (`bdev_conf` defaults can still be overridden per SP at
`CreateStoragePool`, Storage pools.)

**DeleteCluster** —
Errors: `NOT_FOUND` cluster; `FAILED_PRECONDITION` while any DN, CN or SP
still exists, checked as a nonzero `shard_bucket` sum on `DnGlobal`,
`CnGlobal` or `SpGlobal` (the live count of Globals: id allocation + shard
buckets — an STM cannot range the `dn_conf`, `cn_conf` and `sp_conf`
prefixes).
Action: one STM reads `ClusterConf` for `creation_epoch`, derives
`cluster_id` (cluster_id derivation), runs the checks above, then deletes
`ClusterConf`, `DnGlobal`, `CnGlobal` and `SpGlobal`. Reply the deleted
`cluster_id` — a later `CreateCluster` with the same name reports a
different one.

**GetCluster** — Errors: `NOT_FOUND` cluster; `ABORTED` if a global key is
missing.
Action: one STM reads `ClusterConf` (name-keyed), derives `cluster_id` from
its `creation_epoch` (cluster_id derivation), then reads the three
`cluster_id`-keyed globals. The reply carries `cluster_name`, `cluster_id`
and the four messages; `creation_epoch` is visible inside the returned
`ClusterConf`.

**ListClusters** — Errors: `INVALID_ARGUMENT` on a bad `count` or token.
Defaults: a zero `count` takes `DefaultListCnt`; the token is empty.
Action: no STM; range the `cluster_conf` prefix and return up to `count`
cluster names plus the next token (page_token; `gateway.md` GW10). This is
the one list that needs no `cluster_id`, because `ClusterConf` is the only
name-keyed message; `GetCluster` turns a listed name into its
`cluster_id`.

### Disk nodes

**CreateDiskNode** —
Errors: `NOT_FOUND` cluster; `ALREADY_EXISTS` `dn_conf` key;
`RESOURCE_EXHAUSTED` when the `DnGlobal` `shard_bucket` sum is at or above
`MaxDnCntPerCluster`; `INVALID_ARGUMENT` if `nvme_tr_conf` is empty or the
reported disk size yields a `total_ext_cnt` below one; `ABORTED` if the
`DnGlobal` is missing or `GetDnSize` fails.
Defaults: `location` is the node's `addr_port`; `disabled` as sent. No RPC
changes a node's `location` after creation, so a `location` read outside a
transaction never goes stale — the premise of the location reads of
Per-operation allocation.
Action: (pre-STM) call `DiskNodeAgent.GetDnSize` at `addr_port` with a
zero `dn_id` (the id is only for logging; the agent does not need it) and
compute `total_ext_cnt` (Size → extents). One STM then: allocate `dn_id`
and `shard_code` from `DnGlobal` (Globals: id allocation + shard buckets);
write the `DnConf` (`disabled`, a zero `err_epoch`, `nvme_tr_conf`,
`location`, an empty `side_ptr_list`, `total_ext_cnt`, and `free_ext_cnt`
equal to `total_ext_cnt`); write the `DnCapacity` key at the computed
`bin_idx` iff the node is allocatable (Capacity index keys); write the
`DnRev` key with the first revision and `addr_port` set to the node's
endpoint — its appearance makes the owning dn worker start syncing and
health-checking the node; update `DnGlobal`. Reply `dn_id`. Later changes
to `disabled` go through `UpdateDiskNodeDisabled` below (and
`UpdateControllerNodeDisabled` for CNs, Controller nodes).

**DeleteDiskNode** —
Errors: `NOT_FOUND` cluster or dn; `FAILED_PRECONDITION` while
`side_ptr_list` is not empty (delete or migrate the owning SPs first);
`ABORTED` for a missing `DnGlobal` or a stale `DnRev` token.
Action: one STM reads the `DnConf` (for the `dn_id` and `shard_code` the
id-keyed `DnRev` key is built from), deletes the `DnRev` (the worker stops
health checking; the agent process can simply be stopped — no desired
state remains), the `DnConf` and the `DnCapacity` key if present
(Capacity index keys), and decrements the node's `shard_bucket` entry of
`DnGlobal`. Reply `dn_id`.

**GetDiskNode** — Errors: `NOT_FOUND`; `ABORTED` if the `DnRev` that must
exist is missing.
Action: one STM reads the `DnConf` (by `addr_port`) and then the `DnRev`
(by the `dn_id` and `shard_code` just read) into the reply — the request
names the node by `addr_port`, so the `DnConf` must be read first.

**ListDiskNodes** — like `ListClusters` (page_token), but over the
`dn_conf` prefix of the `cluster_id`, returning the `addr_port` suffixes.
Like every non-cluster list it first reads the `ClusterConf` (`NOT_FOUND`
if absent) to derive the `cluster_id` in that prefix; the range itself
uses no STM (`gateway.md` GW5, GW10).

**UpdateDiskNodeDisabled** —
Errors: `NOT_FOUND` cluster or dn; `ABORTED` for a stale `DnRev` token.
Action: one STM reads the `DnConf`, checks the token (`gateway.md` GW6),
sets `DnConf.disabled` to the request's `disabled` and maintains the
`DnCapacity` key (Capacity index keys): disabling deletes the key if
present, enabling recreates it iff the node is otherwise allocatable.
Idempotent. `disabled` only gates control-plane scheduling (Capacity index
keys), so — like `err_epoch` and capacity-key maintenance (Revision keys
and the sync fan-out) — it bumps no revision and reaches no agent; sides
already hosted by the node keep running. Reply `dn_id`.

**InspectDiskNode** — Errors: `NOT_FOUND`; `ABORTED` if the agent call
fails.
Action: read the `DnConf` in an STM for the ids, which address the agent
request and the log line; then, outside the STM, call
`DiskNodeAgent.GetDnInfo` and return its `revision` and `DnInfo`. The
reply's `applied_revision` is that `revision`, the revision of the last
`SyncupDn` the agent accepted for the node (Live-state reporting), so a
caller can diff it against the desired revision `GetDiskNode` returns in
its `DnRev`.

### Controller nodes

The mirror images of Disk nodes against `CnConf`, `CnCapacity`, `CnRev`
and `CnGlobal`, with these differences:

* **CreateControllerNode** calls `ControllerNodeAgent.GetCnSize` (with a
  zero `cn_id`) and maps the reply to `total_ext_cnt` per Size → extents —
  a zero takes `DefaultCnCap` and a larger size is clamped to `MaxCnCap`.
  `RESOURCE_EXHAUSTED` at `MaxCnCntPerCluster`.
* **DeleteControllerNode**: `FAILED_PRECONDITION` while `cntlr_ptr_list`
  is not empty.
* **UpdateControllerNodeDisabled**: as `UpdateDiskNodeDisabled` against
  `CnConf` and `CnCapacity`, with the `CnRev` token; reply `cn_id`.
* **InspectControllerNode** calls `ControllerNodeAgent.GetCnInfo`; reply
  `applied_revision` and `cn_info`, with the diff semantics of
  `InspectDiskNode` against `CnRev`.
* `location` is write-once here too: no RPC changes a CN's `location`
  after `CreateControllerNode`, so a `location` read outside a transaction
  never goes stale (Per-operation allocation).

### Storage pools

**CreateStoragePool** —
Errors: `ALREADY_EXISTS` `sp_conf` key; `RESOURCE_EXHAUSTED` when the
`SpGlobal` `shard_bucket` sum is at or above `MaxSpCntPerCluster`, when fewer than
`cntlr_cnt` CNs are eligible, or when DN allocation fails (Per-operation
allocation); `INVALID_ARGUMENT` when `cntlr_cnt` lies outside
`MinCntlrCntPerSp` to `MaxCntlrCntPerSp`; when `slice_cnt` is **above**
`MaxSliceCntPerSp` (a zero `slice_cnt` is not refused — it takes the
default below, as `cntlr_cnt` does); when `init_ext_cnt` is zero; when a
group is too small to hold its own meta blocks (Group on-leg layout: meta
region, data region, health block; `model.GroupBlocks` refuses a group
whose total blocks do not exceed its meta blocks) — the data group of
`init_ext_cnt` extents, or the fixed one-extent meta group when the
cluster's `extent_size` holds that few `data_block_size` blocks — or when
`init_ext_cnt` times `extent_size` overflows a uint64; when
`cntlid_slot_list` has duplicates, values at or above `CnCntlidSlotCnt`,
or fewer entries than `cntlr_cnt`; and for any violation of Common
validation in `bdev_conf` or `event_threshold`, the `bdev_conf` geometry
rules being judged on the merged conf the SP stores as well, and
`primary_unhealthy` against the cluster's `cntlr_interval` (step 2).
Defaults: `cntlr_cnt` is `DefaultCntlrCntPerSp`; `slice_cnt` is
`DefaultSliceCntPerSp`; `cntlid_slot_list` is every slot below
`CnCntlidSlotCnt`; `bdev_conf` is taken member-wise from
`ClusterConf.bdev_conf`, then from the constants (a `redund_conf` unset in
both the request and the cluster conf means `redund_none`; dnvctl defaults
it to `redund_md_raid1` on the CLI). What is stored is the result of that
merge, every defaultable member
concrete (Common validation), which is why resolution runs after the
merge and never before it (`gateway.md` GW11). `event_threshold` is stored
as sent and resolved when read (Common validation, Automatic reactions).
Action:
1. Pre-STM: apply the request-level defaults above (`cntlr_cnt`,
   `slice_cnt`, `cntlid_slot_list`) — the substituted `slice_cnt` is the
   one everything below is built from, so a defaulted SP has
   `DefaultSliceCntPerSp` slices in `slice_id_list` and that many `Slice`
   keys. Plan the groups: per slice one **meta** group of one extent (the
   first rung of the meta ladder, GrowSlice), then one **data** group of
   `init_ext_cnt` extents, in that order — **extent counts only**. The
   groups' `meta_blocks` and `data_blocks` are not computable yet: the
   on-leg layout needs the resolved `bdev_conf` and the cluster's stored
   `extent_size`, which step 2 is the first to hold (Common validation).
2. The pre-STM candidate scan and the STM commit re-run as a unit only
   when the STM finds a candidate changed — a pick it refuses below; an
   etcd conflict re-runs the STM closure alone (`gateway.md` GW9;
   `dnv-worker.md` EU4). Before the scan, check a plain pre-read of
   `ClusterConf` against Common validation as the scan does (`ABORTED`
   otherwise, UNEXPECTED_ERROR → `ABORTED`), then merge and
   resolve `bdev_conf` against it: the result gives the scan its leg
   count, and the geometry rules judge it right there (`INVALID_ARGUMENT`,
   and the message says it concerns the merged conf: a member the request
   omitted is inherited, and only the merge shows whether it meets the
   ones the request set), so a request whose merge breaks a rule is
   `INVALID_ARGUMENT`, not `RESOURCE_EXHAUSTED`, even while the cluster
   lacks the DNs or CNs the scan would need. The primary threshold of
   Common validation is judged against that pre-read's `cntlr_interval` at
   the same point, for the same reason. In the STM, in this order:
   merge and resolve `bdev_conf` against the `ClusterConf` this
   transaction read, **before any id is minted**, and refuse the picks
   right there if the leg count or the `cluster_id` moved since the scan;
   the geometry rules on that resolved `bdev_conf` once more
   (`ClusterConf` is write-once, so for an unchanged `cluster_id` this
   repeats the pre-read's verdict, but this read is the authoritative
   one), and the primary threshold against that `ClusterConf`; the
   `sp_conf` existence check; allocate `sp_id` and `shard_code`
   from `SpGlobal`; validate that `ClusterConf` (Common validation) and
   compute every group's `meta_blocks` and `data_blocks` (Group on-leg
   layout: meta region, data region, health block) from the resolved
   `bdev_conf` and the cluster's stored `extent_size`; verify every DN and
   CN pick (Per-operation allocation) against the capacity key the scan
   drew it from, and charge it; write the `SpConf` (`next_id` advanced
   past all consumed ids, `next_dev_id` at its first value, `bdev_conf`
   resolved, `event_threshold` as sent, `cntlid_slot_list`, `sp_level`
   `SP_LEVEL_READWRITE`, `deleting` false, the id and name lists filled);
   the `SpName`; one `Cntlr` per picked CN, created in pick order — the
   first created, the smallest `cntlr_id`, gets `primary` and `settling`
   true (Terminology and object model), the rest `primary` false; all
   `disabled` false; `cntlid_slot` the next unused entry of
   `cntlid_slot_list` in list order, since cntlid slots requires all cntlr
   slots of one SP distinct; `addr_port` and `nvme_tr_conf` copied from
   the CN; one `Slice` per slice (`slice_idx` from zero, groups, legs with
   `leg_idx` from zero, one `Side` each: `addr_port` and `nvme_tr_conf`
   copied from the picked DN, `cntlid_slot` the first entry of
   `cntlid_slot_list` — sides of different legs may share slots, cntlid
   slots —, `provisioned` false ([D15]: every new `Side` is written
   unprovisioned and is flipped by the sp worker, sp role), a zero
   `err_epoch`); update every picked DN's `side_ptr_list`, `free_ext_cnt`
   and `DnCapacity` key (Capacity index keys) and bump each `DnRev` once;
   likewise every picked CN (`cntlr_ptr_list`) and its `CnRev`; write the
   `SpRev` key with the first revision and `sp_name` set to the requested
   name; update `SpGlobal`.
3. Reply `sp_id`. Workers then converge: dn workers push the new side
   pointers (`SyncupDn`), sp workers push the side and cntlr configs
   (`SyncupSide`, `SyncupCntlr`), and the primary builds its stack
   (Primary cntlr).

Step 2's STM is the transaction `EtcdMaxTxnOps` is sized by (Cardinality
limits, Components: invocation reference): etcd counts the STM's reads
plus its writes, every factor of its widest shape is a ceiling constant,
and tests pin the budget (`gateway.md`, Constants this document owns).

The SP does **not** serve immediately: every side it just created is
`provisioned` false, so the DN agents build the side devices and zero the
part of each that must read as zeros (Side provisioning protocol) while
the CN stacks stay deferred and report `RES_STATUS_PROVISIONING` —
healthy, not ready, no action needed. How long the SP waits depends on
the disks' zeroing rate and on how much the DN agents zero: the whole leg
span of a meta group's side, and only the meta region and first data block
of a data group's side. Progress is visible through `InspectSide`
(`SideInfo.zeroed_bytes` and `zero_bytes`) and through `GetStoragePool`
(each `Side.provisioned`); the sp worker flips the flags and the normal
watch fan-out brings the SP up (sp role).

**DeleteStoragePool** — the RPC latches the SP, and the sp worker's drain
takes it apart (`dnv-worker.md`, The sp drain).
Errors: `FAILED_PRECONDITION` if any of `td_name_list`, `nqn_list`,
`clone_name_list`, `xfer_name_list` and `migr_name_list` is non-empty
(user-created objects first; cntlrs, slices, groups, legs and sides were
created implicitly and are deleted implicitly). A delete of an SP whose
`deleting` is **already** true returns OK with no writes and no revision
bump — the drain is running, and a second bump would only invalidate
every client's token; a stale revision token is still `ABORTED` first
(`dnv-worker.md` SPD3).
Action: one STM reads the `SpConf` and the `SpRev` (the token check),
checks the five lists, puts the `SpConf` with `deleting` true and bumps
`SpRev`; nothing else is written. The SP, its cntlrs, its slices and every
extent they charge survive the reply. Reply `sp_id`.

The teardown itself is the sp worker's drain (`dnv-worker.md`, The sp
drain), which the SP's next reaction pass starts and which then carries
itself from one commit to the next in steps of a constant size — the
cntlrs first, then one slice's groups at a time, then the SP's own keys
(`dnv-worker.md` SPD9 to SPD12) — because no single transaction could be
proven legal for an SP whose slices `GrowSlice` grows. What one would
guarantee — agreement between the node budgets and the keys that describe
them — every step keeps by releasing budget in the transaction that
shrinks the describing key (`dnv-worker.md` SPD13). Agents notice the
shrunken pointer lists through `SyncupDn` and `SyncupCn` and tear the local
stacks down (Teardown by sweep); the sp worker notices the deleted `SpRev`
and stops dispatching. Partial teardown is therefore a real, observable
state: `GetStoragePool` shows `deleting` true and a shrinking inventory,
`CreateStoragePool` keeps failing `ALREADY_EXISTS` on the surviving
`sp_conf` key until the last transaction commits, and an observer polls
`GetStoragePool` until `NOT_FOUND`.

**GetStoragePool** — one STM reads the `SpConf`, the `SpRev`, every
`Cntlr` in `cntlr_id_list` and every `Slice` in `slice_id_list` (same
order) into the reply; a missing listed key is `ABORTED`.

**ListStoragePools** — the `sp_conf` prefix of the `cluster_id`, returning
the `sp_name`s (page_token; `gateway.md` GW10).

**UpdateStoragePoolCntlidSlotList** — Errors: `INVALID_ARGUMENT` for an
empty list, duplicates, values at or above `CnCntlidSlotCnt`, or a list
missing a slot in use by any cntlr or side of the SP. Action: STM write
and `SpRev` bump. Reply `sp_id`.

**UpdateStoragePoolLevel** — Action: an STM sets `SpConf.sp_level` and
bumps `SpRev`; workers propagate it to every side and cntlr (the field
rides in both `Syncup*` requests). The levels are ordered, each including
all restrictions of the levels before it; SpLevel gives their meaning and
their purpose, staged disaster recovery and maintenance.

**FindStoragePoolNames** — no STM beyond one snapshot read: for each
requested `sp_id`, read its `SpName` key and put the pairs found into the
reply map. **Ids omitted from the reply are unknown ids** — the caller
reads absence as "no such `sp_id`", and the call itself never fails on
unknown ids. This is the reverse lookup admin tooling and log analysis
use, since keys and device names carry `sp_id`, not `sp_name`.

### GrowSlice

Errors: `NOT_FOUND` when `slice_id` is not in `SpConf.slice_id_list`;
`INVALID_ARGUMENT` when `is_meta` is false and `ext_cnt` is zero, or
`is_meta` is true and `ext_cnt` is not zero — the request's `ext_cnt` is
only this data-or-meta **exclusivity signal**, and group sizes are
computed (below); `FAILED_PRECONDITION` when `is_meta` is true and the
slice's meta total already sits at the dm-thin metadata ceiling, when the
slice's group list of the requested kind already holds
`MaxGrpCntPerSlice` groups (the width of md names; a count, but one
nothing frees on a live slice, because groups leave a slice only from the
tail, by the drain of a deleting SP, `dnv-worker.md` SPD10 — the gateway
answers it ahead of the candidate scan), or when `sp_level` is at
or above `SP_LEVEL_NO_THINPOOL` (the model op's level gate refuses: pools
are suppressed at those levels, so there is nothing to grow; the same gate
guards `CreateSpareLeg` and `SwitchSpareLeg`, Spare legs);
`RESOURCE_EXHAUSTED` when there are no DN
candidates (Per-operation allocation) or when any cntlr's CN has a
`free_ext_cnt` below the new group's `ext_cnt`. That last code is the
gateway's pre-check answer (`gateway.md`, Storage pools and GrowSlice, and
GW7): a CN whose budget falls short only between that pre-check and the
deciding STM is refused in-STM by `chargeSpCns`, which can only fail as a
precondition, so the caller sees `FAILED_PRECONDITION` for the same
shortfall.

**Meta ladder.** The sizes of a slice's meta groups are fixed by rule,
not by the caller: the first meta group, created with the SP, is one
extent, and each further meta grow appends a group whose `ext_cnt` equals
the slice's current meta total in extents, so the additions double. Meta
growth stops once the total meta size reaches the dm-thin metadata
ceiling (`metaSizeCap`), and further meta growth is refused.

Action: allocate legs for one new group. `is_meta` selects the list, and
the new group's size is always computed, never taken from the request: a
**data** grow appends a group of the slice's **first data group's**
`ext_cnt` — the original allocation unit, exactly like the auto-grow of
Automatic reactions (`gateway.md`, Storage pools and GrowSlice) — and a
**meta** grow's `ext_cnt` comes from the ladder. In the STM, with the SP's
stored `bdev_conf` and the cluster's `extent_size` both validated at the
top of the transaction (Common validation), compute `meta_blocks` and
`data_blocks` (Group on-leg layout: meta region, data region, health
block) and append a `Group` — its `grp_id`, `ext_cnt`, `meta_blocks`,
`data_blocks`, legs and sides, every new `Side` written `provisioned` false
([D15]) — to the slice; update the involved DNs and their `DnRev`s and the
budget of every cntlr's CN and its `CnRev`; bump `SpRev`. Agents extend
the pool-meta and pool-data linear tables and resize the thin pool online
— but the growth is **deferred on the CN** while the new group still
contains a provisioning leg: the concat and the pool keep their old,
effective size and keep reporting `OK` at that size, and the grow
completes by itself once the leg clears (Side provisioning protocol,
Automatic reactions). The per-kind "one grow per pool at a time" pending
rule (`dnv-worker.md` AR6) binds only the sp worker's own auto-grow, which
judges "pending" by the primary's reported usage; a user-driven
`GrowSlice` is explicit operator intent, holds no such report and is
**not** so gated (`gateway.md`, Storage pools and GrowSlice) — stacked
grows are absorbed by the deferred growth above, though every stacked
group's DN extents are charged immediately. Reply `slice_id` and `grp_id`.

### Cntlrs

**CreateCntlr** —
Errors: `RESOURCE_EXHAUSTED` when `cntlr_id_list` is at or above
`MaxCntlrCntPerSp` or no CN is eligible (Finding CN candidates,
Per-operation allocation — the presence of a capacity key already implies
healthy, enabled and not full); `INVALID_ARGUMENT` when `cntlid_slot` is
not in `SpConf.cntlid_slot_list` or is already used by another **cntlr**
of the SP (cntlid slots).
Action: pick a CN (Per-operation allocation). In the STM, a pick is
dropped when the SP, as this STM reads it, has a cntlr on a CN the scan's
read of its cntlrs did not hold, and the scan and the STM re-run as a unit
from a fresh read (`gateway.md` GW9): the scan's CN exclusion and tier-1
locations come from that earlier read, so a cntlr committed since could
have the pick land in its failure domain or on its CN. The STM then writes
the new `Cntlr` (`primary` false, `disabled` false, a zero `err_epoch`),
appends its id, does the CN bookkeeping with its `CnRev`, rewrites every
`CdcEntry` of the SP by the listing rule of [D18], which lists the new
cntlr from its creation, and bumps `SpRev`. A subsystem whose `CdcEntry`
is missing gets it back here, rebuilt as `CreateSubsystem` writes it
(Subsystems, namespaces); `DeleteCntlr`, an `UpdateCntlrEnabled` that
changes the flag and `UpdateSubsystemHosts` put a missing entry back the
same way. The sp worker's writers of the entries — its health write for a
cntlr (sp role), its failover and its cntlr replacement (Automatic
reactions) — do not: they rewrite only the entries that exist and leave a
missing one missing (`dnv-worker.md` MD6). The sp worker's next
`SyncupSide` round tells every side about the new standby
(`side_conf.standby_id_list`), and the sides grow a dm-error, dm-linear
and nvmet export for it (Disk node). Reply `cntlr_id`.

**DeleteCntlr** —
Errors: `NOT_FOUND` when the id is not in the list; `FAILED_PRECONDITION`
when `primary` is true or `disabled` is false (disable first, so that a
failover has already happened before the record disappears).
Action: STM: remove the id and the `Cntlr` key; the CN bookkeeping (the
footprint back, its `CnRev`); rewrite every `CdcEntry` of the SP by the
listing rule of [D18], which leaves the deleted cntlr out; bump `SpRev`.
The sides drop the export, and the cn agent tears down its stack. Reply
`cntlr_id`.

**UpdateCntlrEnabled** —
Action: an STM sets `Cntlr.disabled` to the negation of the request's
`enabled`; an enable of a cntlr that is still `primary` also sets
`settling` (Terminology and object model: its cn agent held the standby
shape while it was disabled and now builds the primary stack, the work of
a promotion); the STM then rewrites every `CdcEntry` of the SP by the
listing rule of [D18] and bumps `SpRev`. A disable takes the cntlr out of
the entries at once; an enable lists it at once only when it is the
primary or its `err_epoch` is zero, and any other at the sp worker's next
clean verdict on it (sp role). Idempotent. A disabled cntlr leaves primary
eligibility and its namespaces go ANA-inaccessible; disabling the current
primary triggers the primary re-election of Automatic reactions, whose
fan-out hands the disabled primary its demotion first (sp role);
disabling the last enabled cntlr is allowed but stops IO. No warning is
given: dnvctl issues no RPC the operator did not type (`dnvctl.md` CT8).
Reply `cntlr_id` and `enabled`.

**InspectCntlr** — read the `Cntlr` in an STM for its `addr_port`;
outside the STM call `ControllerNodeAgent.GetCntlrInfo` with the
`cluster_id`, `cn_id`, `sp_id` and `cntlr_id`; an agent failure is
`ABORTED`. Reply `applied_revision` — the revision of the last
`SyncupCntlr` the agent accepted for this cntlr, to diff against the
desired `SpRev` of `GetStoragePool`, with the semantics of
`InspectDiskNode` — and the `CntlrInfo`.

**InspectSide** — locate the side by scanning the SP's slices in the STM
(bounded by the SP's slices, groups and legs) and get its DN; outside the
STM call `DiskNodeAgent.GetSideInfo` with the `cluster_id`, the `dn_id`
and the side pointer. Reply `applied_revision` and the `SideInfo`. Both
Inspect RPCs are the way to watch clone and migration hydration before
`DeleteClone` and `FinishMigration`.

### Thin devices

**CreateThinDevice** — Errors: `ALREADY_EXISTS` td key; `RESOURCE_EXHAUSTED`
when `td_name_list` holds `MaxTdCntPerSp` tds or more; `NOT_FOUND`
`ori_name` set but absent; `FAILED_PRECONDITION` `ori_name` set and the
origin's `created` false (checked after `NOT_FOUND`); `FAILED_PRECONDITION`
`ori_name` set and the origin is the destination of a clone (any
`Clone.dst_td_id` of the SP, found by `DeleteThinDevice`'s
`clone_name_list` walk in the same STM; checked after `created`; the
details name the origin and the clone). While the
clone hydrates, the destination's thin volumes hold only the regions
hydrated so far, dm-clone serving the rest from the source (Clones), so a
snapshot would capture a partial copy; this RPC makes no agent call, so
hydration is invisible to it: the refusal holds while the `Clone` key
exists, drain included, and like the `created` one it writes nothing. It
does not reach a snapshot accepted before `CreateClone` named its origin:
a slice whose `create_snap` is retried after hydration began captures part
of the copy (Known limits, "A snapshot accepted before a clone targets its
origin can hold part of the copy"); `INVALID_ARGUMENT`
`size` zero without `ori_name`, or `size` not a multiple of `slice_cnt`
times the SP's `DmRaid0Conf.stripe_size` (dm-striped needs equal,
chunk-aligned members; sizes should also be multiples of `block_size`).
Defaults: `ori_name` empty (a fresh device, not a snapshot); when
`ori_name` is set and `size` is zero, `size` is the origin's size (a
snapshot may also be created larger). Action: STM: `td_id` from `next_id`,
`dev_id` from `next_dev_id` (advanced), `ori_id` the origin's `dev_id` or
zero; write the `ThinDevice` with `created` false, append to
`td_name_list`, bump `SpRev`. Reply `td_id` and `dev_id`. The primary
creates one thin volume per slice — a `create_thin` or `create_snap` pool
message while `created` is false, a bare `dmsetup create` once the flag is
set — plus the raid0 and the dm-error of Primary cntlr; namespace devices
are per `Namespace` and appear with it (Subsystems, namespaces).

**Snapshot point-in-time.** A td is striped across every slice, so a
snapshot is crash-consistent only if all `slice_cnt` `create_snap`
messages capture one instant. The primary therefore quiesces the origin
td's raid0 (`CnRaid0Name`) around the whole per-slice sequence, so that no
host write lands between two slices' messages; `cnagent.md` CN14 is the
agent-side spec, bounds of the suspension included ([D12]). A primary
crash *between* two slices' messages still yields a torn snapshot (the
per-slice snapshots date from different instants); a snapshot whose
creation raced a primary crash should be deleted and re-created (Known
limits).

**Materialization.** `ThinDevice.created` is written false here and set
true exactly once by the sp worker, when a cntlr has reported that td's
thin volume `RES_STATUS_OK` in **every** slice of the SP (sp role). It is
never cleared: a pool holds a thin id until a delete message of its
`dev_id` reaches it — sent by the td's own deletion or, when that fan-out
also demoted the applying cntlr (or suppressed its pool) so that CN14's
pool-presence gate skipped the message, by the CN14 **activation sweep**
at the next re-creation of the pool device (`cnagent.md` CN14). Either way
a later bad row is a health event (`err_epoch`), not evidence the id is
gone. A td deleted and re-created under the same name is a different td —
new `td_id`, new `dev_id`, `created` false — and `dev_id`s are never
reused, so an old snapshot's `ori_id` can never resolve to it. The
refusal's details (`msgOriginNotCreated`) tell the caller to wait for
`ListThinDevices` to report `created` true, and nothing is written — no
key, no `next_id` or `next_dev_id` consumed, no `SpRev` bump. A snapshot
*of a snapshot* follows the same rule: the **immediate** origin must be
created, and that snapshot's own materialization is what later makes it
eligible as an origin.

**Snapshot creation is the only gated operation.** `create_snap` is the
one operation with a kernel-level dependency on the origin's id already
being in each slice pool. `CreateNamespace` and `UpdateNamespaceDev` on an
uncreated td are allowed — CN16's backing rule never consults `created`:
the primary builds the td's thin volumes, its raid0 and the ns-dev over
that raid0 in one converge (over the dm-clone instead when a clone targets
the td; an ns-dev whose raid0 failed to appear reads `RES_STATUS_ERROR`, it
does not park), and the ns-dev parks on the td's `CnErrorName` only where
the CN16 backing rules park it anyway — a [D15]-deferred chain, an
effectively suspended namespace, a standby or disabled cntlr, or a
suppressing `sp_level` (`cnagent.md` CN16).
`CreateClone` with an uncreated destination is allowed ([D3]: the
destination td is empty by construction). `GetThinDeviceBitmap` is not
gated, and a slice whose pool does not hold the id yet fails in the agent,
which reports the `dev_id` absent from the metadata snapshot (`cnagent.md`
CN26) — surfacing as `ABORTED` like any other agent RPC failure
(UNEXPECTED_ERROR → `ABORTED`).

**The client's wait primitive** is `ListThinDevices`: poll until the td
reads `created` true, then snapshot it. A snapshot of a td that a clone
targets is refused until that clone has also been deleted and drained,
which only `DeleteClone` starts (refused until the copy is proven
complete, unless forced); the client then polls `GetClone` until
`NOT_FOUND` (Clones). A client whose snapshot request carries a token
re-reads it from `GetStoragePool` after its last poll: the flip bumps
`SpRev` (sp role), and so do the clone's latch and every drain
transaction (Clones, `dnv-worker.md` CLD12), while neither
`ListThinDevices` nor `GetClone` returns a revision, so a request carrying
a token read before the last of those writes is refused `ABORTED` ("stale
revision", Revision keys and the sync fan-out). `CreateThinDevice` never
blocks (STM discipline keeps every RPC short). Typical latency is one
fan-out — the `SpRev` bump of `CreateThinDevice` sends `SyncupCntlr` to the
primary, whose reply already reports every slice `OK` in the common case,
so the flip lands in the same round; the worst case is one
`health_check_conf.cntlr_interval` later through `CheckCntlr` (Check
streams), and up to one more while a side of the SP has not accepted the
bump (sp role, sides first).

**Interaction with `SpLevel` and provisioning.** At an `sp_level` that
suppresses pools the thin rows read `RES_STATUS_MISSING` with details
`ResDetailsSpLevel` (`cnagent.md` CN19), so no td of that SP ever flips and
every snapshot
request is refused until the level is restored and a converge reports the
rows `OK`. A td created while a slice is still provisioning-deferred
([D15]) reads `RES_STATUS_PROVISIONING` in that slice and flips when the
last slice clears. Both are the intended meaning of "not created yet".

**DeleteThinDevice** — Errors: `FAILED_PRECONDITION` if the `td_id` is
referenced by any `Namespace.td_id` of any subsystem of the SP (read
`nqn_list` and every `Subsystem` in the same STM — bounded by
`MaxSsCntPerSp` times `MaxNsCntPerSs`, cheap) or by any `Clone.dst_td_id`;
`FAILED_PRECONDITION` if any td of the SP has this td's `dev_id` as its
`ori_id` and `created` false, with details naming the blocking snapshots
(`msgSnapshotNotCreated`). A converge's sweep
phase runs before its build phase (`cnagent.md` CN9, CN21), so an origin
leaving `td_list` in the converge that would first materialize its
snapshot sends the delete message of the origin's `dev_id` before
`create_snap` and loses the snapshot for good; the guard is what makes that
unreachable. A snapshot with `created` true does **not** block, and
because the match is on `dev_id`, which is never reused, no stale snapshot
can block a same-name recreate.

The snapshot guard's reads — `td_name_list` and every `ThinDevice`, the
`ListThinDevices` read set, bounded by `MaxTdCntPerSp` — happen in a
read-only etcd snapshot that plans the delete, and the deciding STM
re-reads only the snapshots that plan found. They stay **out** of the
deciding transaction, this RPC's exception to the one STM of STM
discipline: a transaction's compares are its reads plus its writes, and
one compare per td would take a full pool's delete past `EtcdMaxTxnOps`
(Cardinality limits). The plan reads each listed td at the store revision
its Snapshot pinned on its first read, the revision its `SpRev` read is
served at too: a read served at an earlier revision could miss a snapshot
whose `SpRev` bump the plan then reads, and the deciding STM would find
nothing moved and delete the origin. The pool's identity and revision close
the window between plan and STM: every write to a td — a snapshot create,
a `created` flip, a delete — bumps `SpRev` (Revision keys and the sync
fan-out), and the deciding STM evaluates no guard and deletes nothing when
the SP it resolves is not the one the plan walked (`cluster_id`, `sp_id`: a
recreated SP's `SpRev` starts again at one) or its
`SpRev.revision` is not the one the plan read. A request carrying the token
is then refused `ABORTED` ("stale revision") by the token check, which runs
first and which the plan passed at the old revision; a token-less request
re-plans, and the new plan finds the snapshot (`gateway.md`, Thin devices,
and GW9 carry the plan and the deciding STM).

Action: STM: remove from `td_name_list`, delete the key, bump `SpRev`.
`dev_id` is never reused. Deleting the origin of snapshots is allowed —
dm-thin snapshots stay valid — once every snapshot of it is created. The
primary applies it with the delete message of the `dev_id` per slice pool.
Reply `td_id`.

**ListThinDevices** — one STM reads `SpConf` and every td in
`td_name_list` into `name_to_td`; a missing listed key is `ABORTED`.
`name_to_td` carries `created`; it is the client's wait primitive before
snapshotting (with `GetClone` for a clone's destination).

### Subsystems, namespaces

**CreateSubsystem** — Errors: the NQN rules of Common validation, the
dnv-namespace one included; `ALREADY_EXISTS` subsystem key;
`RESOURCE_EXHAUSTED` at `MaxSsCntPerSp`; `INVALID_ARGUMENT` when
`allowed_hosts` holds more than `MaxHostCntPerSs` entries or any entry
fails the host-NQN rules. Action: STM: `ss_id` from `next_id`; the serial
is the `ss_id` rendered with `IdKeyFmt` and the model is
`subsystemModel` ([D2]); write the `Subsystem` with an empty `ns_list`,
append to `nqn_list`, write the `CdcEntry` — the NQN, the transport confs
of the cntlrs the listing rule of [D18] lists as `nvme_tr_conf_list`, and
`allowed_hosts` —, bump `SpRev`. Reply `ss_id`.

**DeleteSubsystem** — Errors: of the NQN rules of Common validation the
`nqn` gets the length check alone; `NOT_FOUND` nqn; `FAILED_PRECONDITION`
`ns_list` non-empty (`allowed_hosts` never block, they go implicitly).
Action: STM: remove from `nqn_list`, delete the `Subsystem` and its
`CdcEntry`, bump `SpRev`. dnv-cdc sends a discovery-log-change AEN to the
hosts whose discovery log the deletion changes (`cdc.md` DS6); hosts that
follow the discovery log (Host view) disconnect automatically. Reply
`ss_id`.

**ListSubsystems** — STM read of `nqn_list` and each `Subsystem` into
`nqn_to_subsystem`.

**UpdateSubsystemHosts** — STM: update `Subsystem.allowed_hosts` and
`CdcEntry.allowed_hosts`, set the entry's `nvme_tr_conf_list` by the
listing rule of [D18], bump `SpRev`; a missing `CdcEntry` is written back,
rebuilt as `CreateSubsystem` writes it, with the new hosts. Reply `ss_id`.

**CreateNamespace** — Errors: `NOT_FOUND` nqn or td; `RESOURCE_EXHAUSTED`
when `ns_list` holds `MaxNsCntPerSs` namespaces or more;
`INVALID_ARGUMENT` `ns_idx` zero, `ns_idx` already used in this subsystem,
or a supplied `dev_uuid` or `dev_nguid` malformed. Defaults: an empty
`dev_uuid` is a generated RFC 4122 version 4 uuid, an empty `dev_nguid` a
generated random NGUID in hex. Callers supply both only when the namespace
must impersonate an existing one — the transfer and clone flow of Transfer
+ clone = cross-SP live migration. `suspended` is stored as sent (the
proto3 default false); true creates the namespace already suspended
(Namespace suspend semantics), which is how the clone destination
namespace of Transfer + clone = cross-SP live migration is created.
Action: STM: `ns_id` from `next_id`; append to the subsystem a `Namespace`
with `ns_id`, `ns_idx`, `td_id`, `dev_uuid`, `dev_nguid` and `suspended`
as requested, bump `SpRev`. Reply `ns_id`. `ns_idx` is the NVMe NSID.
Every cntlr creates the namespace's own dm-linear `CnNsDevName` of its
`ns_id` (Primary cntlr) and the nvmet namespace on top of it. ANA group
ids are not stored — every namespace joins one of the three fixed
node-local groups of [D4].

**DeleteNamespace** — Errors: of the NQN rules of Common validation the
`nqn` gets the length check alone; `NOT_FOUND` nqn or `ns_idx`. Action:
STM: remove the `ns_idx` entry, bump `SpRev`. Reply `ns_id`.

**UpdateNamespaceDev** — repoints the namespace at another td
(`td_name`), for example to expose a snapshot in place of the origin.
STM: update `td_id`, bump `SpRev`; agents reload the namespace's own
`CnNsDevName` onto the new td's raid0 (a dm reload — the nvmet
"device_path" never changes, so the switch is invisible to the host).
Reply `ns_id`.

**UpdateNamespaceSuspended** — STM: set `suspended`, bump `SpRev`.
Suspended ⇒ every cntlr parks the namespace's `CnNsDevName` on its td's
dm-error and moves the namespace to the `inaccessible` ANA group; resumed
⇒ the reverse. The transfer and clone choreography of Transfer + clone =
cross-SP live migration uses it. Reply `ns_id`.

### Clones

`DeleteClone` latches and the sp worker drains (`dnv-worker.md`, The clone
drain). Every clone mutator but `DeleteClone` — `UpdateCloneTrConf` and
`AppendCloneBitmap` — fails `FAILED_PRECONDITION` when `Clone.deleting` is
true (`dnv-worker.md` CLD1); `CreateClone` needs no check of its own, the
surviving `Clone` key keeping same-name creation refused until the drain's
last transaction (`DeleteClone` below says with which code).

```mermaid
flowchart BT
    subgraph pc["Primary Cntlr — clone destination"]
        SL0["slice 0"]
        SL1["slice 1 …"]
        R["raid0 of the dst td<br/>CnRaid0Name"]
        MLV["dm-clone metadata wrapper<br/>CnCloneMetaDmName (loop arena slot)"]
        SRC["nvme host device<br/>(connected to the source ns)"]
        DC["dm-clone<br/>CnCloneFinalName"]
        NS["dm-linear ns-dev<br/>CnNsDevName"]
        T["nvmet target subsystem"]
        E["dm-error<br/>CnErrorName"]
        SL0 --> R
        SL1 --> R
        R -->|"dest"| DC
        SRC -->|"source"| DC
        MLV --> DC
        DC --> NS
        NS --> T
        E -.->|"reload target"| NS
    end
    XS["source NVMe-oF namespace<br/>(e.g. a transfer's XferNqn)"] ==> SRC
    T ==>|"ANA optimized"| H["host"]
    G["leg connections"] --> SL0
    G --> SL1
```

*Fig. `090Clone` — while a clone targets a td, the `CnNsDevName` of each
namespace backed by that td is reloaded onto the dm-clone; dm-clone pulls
missing regions from the source and hydrates in the background.*

**CreateClone** — Errors: `ALREADY_EXISTS` clone key; `RESOURCE_EXHAUSTED`
at `MaxCloneCntPerSp`; `NOT_FOUND` `dst_td_name`; `FAILED_PRECONDITION` the
destination td already targeted by another clone; `INVALID_ARGUMENT` when
`src_tr_conf` is empty, `src_nqn` is invalid, `src_slice_cnt` is outside
one to `MaxSliceCntPerSp` (unlike `CreateStoragePool`'s own `slice_cnt`, a
`src_slice_cnt` of zero is refused, not defaulted — it describes a source
that already exists), `src_stripe_size` is not a positive multiple of
`MinDmRaid0StripeSize` at most `MaxDmRaid0StripeSize`, `src_block_size` is
not a positive multiple of `MinDmPoolDataBlockSize` at most
`MaxDmPoolDataBlockSize`, or `src_block_size` is not a multiple of
`src_stripe_size` (the source rules of raid0 bitmap math). Defaults:
`dm_clone_conf` per Common validation; `auto_resume` as sent.
Precondition (not verifiable by the control plane, a documented contract,
[D3]): the destination td must be **empty** — freshly created and never
written. Clone crash recovery equates "mapped in the destination thin
pools" with "already copied", which only holds for an initially empty td.
Action: STM: `clone_id` from `next_id`; write the `Clone`, append to
`clone_name_list`, bump `SpRev`. Reply `clone_id`.

On the primary cntlr the clone's build ends in the stack of fig.
`090Clone`: the dm-clone `CnCloneFinalName` — its metadata the wrapper
`CnCloneMetaDmName` over the clone's units in the clone-metadata arena of
Controller node, common ([D14]), its destination the destination td's
`CnRaid0Name`, its source the connected source namespace, its region one
`block_size` of this SP — with each destination-td namespace that is not
effectively suspended reloaded onto it and moved to ANA `optimized`;
`auto_resume` is what overrides a stored `suspended` true (`cnagent.md`
CN16). An effectively suspended namespace stays parked, live, on the td's
dm-error and `inaccessible` (nothing is dm-suspended, so there is no
resume step — Namespace suspend semantics). `cnagent.md` CN18 is the
sequence. After a CN reboot the primary rebuilds the clone by Clone crash
recovery before letting any IO through.

Admission: the checks above gate `MaxCloneCntPerSp` only. The binding
ceiling is the primary CN's clone-metadata arena, whose `CnCloneMetaUnit`
units are shared by every clone of every cntlr on that CN — every clone
costs at least two units, large tds at small block sizes far more
(`cnagent.md` CN18 carries the arithmetic). The control plane does not
gate against it: an over-committed clone is created normally and its
`clone_id_to_meta` and `clone_id_to_dm_clone` rows report
`RES_STATUS_ERROR` until a later converge of the cntlr finds units free;
the arena's refusal registers no retry of its own (`cnagent.md` CN18).
Callers must place clones against the per-CN budget, not against
`MaxCloneCntPerSp`.

**DeleteClone** — Errors: `FAILED_PRECONDITION` when `force` is false and
hydration is not complete — checked outside the STM through `GetCntlrInfo`
of the primary (`clone_id_to_dm_clone.details` carries the dm-clone
status); an unreachable agent is `FAILED_PRECONDITION` too, and so is an
SP with no primary cntlr while `force` is false (nobody to prove hydration
with). A delete of a clone whose `deleting` is **already** true returns OK
with no writes, no revision bump and **no agent call**, with or without
`force`; a stale revision token is still `ABORTED` ahead of it
(`dnv-worker.md` CLD3). Action: the deciding STM latches and writes
nothing beyond the latch: `Clone.deleting` true, `suspended` false on
every namespace whose `td_id` is the clone's `dst_td_id`, and one `SpRev`
bump. It does not delete the `Clone` key, touch a chunk key or shrink
`clone_name_list`: the key and its list entry survive until the drain's
last transaction, and the resume rides the latch so that it and the
fan-out exclusion land in one bump (`dnv-worker.md` CLD4); `gateway.md`,
Clones carries the two phases out. Reply `clone_id`.

The teardown is the sp worker's clone drain (`dnv-worker.md`, The clone
drain): from the first post-latch fan-out the clone is absent from every
cntlr's `clone_list` and from the primary's chunk-push plans, so the cn
agent's sweep (Teardown by sweep) removes its stack and its
`LocalCloneBmPath` files by name and disconnects a source no other clone
on the node uses (`dnv-worker.md` CLD5), while the worker deletes the
chunk keys in batches and then the `Clone` key together with its
`clone_name_list` entry.

Consequences, all of them following from the `Clone` key and its
`clone_name_list` entry surviving until the last transaction: `DeleteClone`
returns while the clone still exists, so an observer polls `GetClone` until
`NOT_FOUND`; a same-name `CreateClone` keeps failing until then —
`ALREADY_EXISTS`, or `RESOURCE_EXHAUSTED` on an SP the surviving entry
holds at `MaxCloneCntPerSp`, that ceiling being the length of
`clone_name_list` and checked ahead of the name, which is also why an
**unrelated** `CreateClone` on a full SP fails for the whole drain; a
second `CreateClone` onto the **same** destination td keeps failing
`FAILED_PRECONDITION`, because that scan walks `clone_name_list`;
`DeleteThinDevice` of the destination td, and a `CreateThinDevice`
snapshotting it (Thin devices), keep failing for the same reason; and
`DeleteStoragePool` keeps refusing while any clone drains — its refusal
names the first kind of object the pool still holds, checking thin devices
first, and a draining clone's destination td cannot be deleted while the
clone exists, so the thin-device refusal is the one a caller meets. A
top-down teardown is therefore: delete the clone, poll `GetClone` until
`NOT_FOUND`, then delete the td or the SP.

**GetClone** — STM read.

**UpdateCloneTrConf** — STM: replace `src_tr_conf_list`, bump `SpRev`
(used when the source SP's cntlrs moved); the primary reconnects.

**AppendCloneBitmap** — Errors: `FAILED_PRECONDITION` when the clone's
`deleting` is true (`DeleteClone` is the only RPC allowed to act on a
deleting clone, and a racing append could otherwise write a chunk key
behind the drain — which is also what makes the drain's final emptiness
guard stable, `dnv-worker.md` CLD1); `INVALID_ARGUMENT` empty `bitmap`;
`INVALID_ARGUMENT` a `bitmap` longer than `CloneBmChunkBytes` (stateless,
judged on this request alone); `INVALID_ARGUMENT` `src_slice_idx` at or
above `Clone.src_slice_cnt`; `INVALID_ARGUMENT` `bm_idx` at or above
`MaxCloneBmCnt`; `RESOURCE_EXHAUSTED` when the stored chunk plus the new
page would exceed `CloneBmChunkBytes` — the chunk's ceiling reached by
previous appends, the same shape as `AppendMigrationBitmap`'s `bm_cnt` cap.
Action: STM: append the bytes to the `CloneBitmap` key of
(`src_slice_idx`, `bm_idx`) (created if absent), bump `SpRev`. The `Clone`
record is not rewritten. These are the **source bitmaps**: bit *k* of
source slice `src_slice_idx` covers `src_block_size` bytes of that slice's
local address space; **1 = the source never wrote there, so the region is
skippable**. They are a pure optimization delivered through
`PushCloneBitmap` (Bitmap push protocol) and may be applied at any time,
even after the dm-clone started serving IO (Clone crash recovery);
durability never depends on them.

A slice's bitmap is split into up to `MaxCloneBmCnt` fixed-capacity chunks
of `CloneBmChunkBytes` each: chunk (s, b) holds the bytes of source slice
s's bitmap from offset b·C on, at most C of them, C being
`CloneBmChunkBytes`. Positions across chunks are fixed by `bm_idx` alone —
no chunk's meaning depends on any other chunk's existence or length — so a
caller may append to any (s, b) at any time, in any order, and may leave
chunks unsent entirely; an absent chunk is semantically all-zero
(all-written) and a short chunk's missing tail reads as written, both the
safe direction. **Within** one chunk a page lands at the chunk's current
length, so the pages of one chunk must arrive in slice-bitmap order — the
per-slice append contract scoped down to a chunk, which exists because
callers page the source bitmap through `GetThinDeviceBitmap` and because
etcd values are size-limited. A chunk already pushed and acknowledged may
keep growing ([D8]).

The `Clone` record carries no chunk count: the set of a clone's chunks is
the set of its `CloneBitmap` keys (Key table), which is what the drain and
`PushCloneBitmap` read.

### Transfers

```mermaid
flowchart BT
    subgraph prim["Primary Cntlr (source SP) — transfer active"]
        SL0["slice 0"]
        SL1["slice 1 …"]
        R["raid0 of the origin td<br/>CnRaid0Name"]
        LX["CnXferFinalName<br/>(dm-linear on the raid0)"]
        NX["nvmet subsystem<br/>XferNqn"]
        LP["CnNsDevName (parked)"]
        NP["nvmet target<br/>(origin ns)"]
        SL0 --> R
        SL1 --> R
        R --> LX
        LX --> NX
        LP --> NP
    end
    subgraph stby["Standby Cntlr (source SP)"]
        LEGS["legs …"]
        EX["CnXferFinalName<br/>(dm-error backed)"]
        NXS["nvmet subsystem<br/>XferNqn"]
        ES["dm-error"]
        LS["CnNsDevName"]
        NS2["nvmet target<br/>(origin ns)"]
        EX --> NXS
        ES --> LS
        LS --> NS2
    end
    SIDES["sides"] --> LEGS
    NX ==>|"nvme-of, allowed_hosts = dst cntlrs"| DST["destination SP primary's clone"]
    NXS -.->|"ANA inaccessible"| DST
    NP -.->|"ANA inaccessible"| H["hosts"]
    NS2 -.->|"ANA inaccessible"| H
```

*Fig. `100Transfer` — after `CreateTransfer` with `auto_suspend` true the
origin namespace is parked everywhere; the xfer subsystem is the only live
reader and writer of the bytes, served by the primary.*

**CreateTransfer** — Errors: `ALREADY_EXISTS`; `RESOURCE_EXHAUSTED` at
`MaxXferCntPerSp`; `NOT_FOUND` for an `ori_nqn` or an `ori_ns_idx` the SP
does not hold; `INVALID_ARGUMENT` for the NQN rules of Common validation on
`ori_nqn`, the dnv-namespace one included, and for the host-NQN rules on
`allowed_hosts`, where callers put the destination cntlrs' `CnHostNqn`s.
Action: STM: `xfer_id` from `next_id`, write the `Transfer`, append the
name to `xfer_name_list`, bump `SpRev`; reply `xfer_id`. Every cntlr
creates the xfer stack (`cnagent.md` CN17), a disabled cntlr in the
standby shape: the primary builds `CnXferFinalName`, a dm-linear on the
origin td's raid0, and exports it as subsystem `XferNqn` on the CN's port
— nsid `ori_ns_idx`, the origin namespace's `dev_nguid` and `dev_uuid`,
`allowed_hosts` from the record, verbatim — an empty list admits no host
(Primary cntlr) —, the cntlid bounds of `Cntlr.cntlid_slot`; the standbys
and a disabled cntlr export the same subsystem backed by a dm-error
`CnXferFinalName`, with ANA inaccessible (fig. `100Transfer`, right half).
Iff `auto_suspend`, the origin namespace is effectively suspended on every
cntlr (the effective suspend of `cnagent.md` CN16): each cntlr, the
primary included, parks the namespace's `CnNsDevName` on its td's dm-error
and holds the namespace in the inaccessible ANA group, each from its own
request, so the destination clone is the only reader and writer of the
bytes (Namespace suspend semantics).

**DeleteTransfer** — `force` false **finalizes** a completed hand-over:
the STM additionally sets `suspended` true on the origin namespace, so the
source stays parked after the xfer object disappears. `force` true
**aborts**: `suspended` is left false, so the next syncup restores normal
service (the ns-dev reloaded onto the raid0, ANA back). Action: STM: remove
the name from `xfer_name_list`, delete the `Transfer`, set `suspended` when
finalizing, bump `SpRev`; the agents drop the xfer subsystem and its dm
devices. Reply `xfer_id`.

**GetTransfer** — one STM read. **UpdateTransferHosts** — STM: replace
`allowed_hosts`, bump `SpRev` (a destination cntlr moved to another CN).

### Migrations

The data-plane procedure and its figure, fig. `080Migration`, are under
Migration.

**CreateMigration** — Errors: `ALREADY_EXISTS` for a name already in
`migr_name_list`; `NOT_FOUND` when `src_side_id` is in no leg of the SP,
spare legs included; `RESOURCE_EXHAUSTED` at `MaxMigrCntPerSp` or when no
DN is a candidate (Per-operation allocation); `FAILED_PRECONDITION` when
the owning leg already has two sides (a migration is already running on
it), or when `cntlid_slot_list` holds no slot different from the source
side's (cntlid slots: such an SP cannot migrate this leg at all;
`gateway.md`, Migrations).

Action: allocate one DN (Per-operation allocation). The deciding STM drops
a pick when the group, as that STM reads it, occupies a DN the scan's read
of the group did not, and the scan and the STM re-run as a unit from a
fresh read of the group (`gateway.md` GW9): the scan's black list and
tier-1 locations come from that earlier read, so a side hung off the group
since — a migration of its other leg, a spare — could put the pick on its
DN or in its failure domain. In the STM: `migr_id` and `dst_side_id` from
`next_id`; a new `Side` appended to the leg's `side_list`, with a
`cntlid_slot` from `cntlid_slot_list` different from the source side's —
the only slot constraint sides have (cntlid slots) —, `addr_port` and
`nvme_tr_conf` copied from the destination DN, and `provisioned` false
([D15]); a `Migration` written with `migr_id`, `src_side_id`,
`dst_side_id`, the request's `dm_clone_conf` and a zero `bm_cnt`, its name
appended to `migr_name_list`; the destination DN's bookkeeping — the side
in its `side_ptr_list`, the group's `ext_cnt` taken from its
`free_ext_cnt`, its capacity key per Capacity index keys, its `DnRev`
bumped —; a `SpRev` bump. Reply `migr_id`. The destination side therefore
provisions (Side provisioning protocol) before any migration machinery
starts; until the sp worker flips it, `migr_src_conf.dst_provisioned`
false keeps the source side serving untouched (Migration, which carries
the data-plane choreography).

**Admission.** The DN candidate scan covers **extents** only. The
destination DN's [D13] clone-metadata area — `DnCloneMetaSize` bytes of
`DnCloneMetaUnit` units per **DN**, shared by every destination role the
node hosts across every SP — is not part of admission: each migration's
slot costs a fixed base plus one byte per region of the side, rounded up to
whole units, so the area bounds the concurrent destination roles of one
DN, the more tightly the larger the side and the smaller the block size
(`dnagent.md` DN13). A migration the area cannot serve is still created;
each converge of its destination that meets the shortfall reports it on
the `migr_dst_info` rows (`dnagent.md` DN13).
`MaxMigrCntPerSp` bounds none of this.

**FinishMigration** — Errors: `FAILED_PRECONDITION` when `force` is false
and hydration is incomplete, which is checked outside the STM through
`GetSideInfo` of the **destination** side
(`migr_dst_info.dm_clone_info.details`); an agent that does not answer
that call is `FAILED_PRECONDITION` too (`gateway.md` AG3). Action: STM:
remove the **source** `Side` from the leg, the destination side becoming
its only one; the source DN's bookkeeping (pointer out, extents back,
capacity key per Capacity index keys, `DnRev`); delete the `Migration` and
every `MigrBitmap` and remove the name from `migr_name_list`; bump
`SpRev`. The destination agent reloads its per-cntlr dm-linears from the
dm-clone straight onto the side device and drops the dm-clone, the
metadata wrapper and its slot, the nvme host connection and the
migration's `LocalMigrBmPath` files (Bitmap push protocol; `dnagent.md`
DN13); the source DN agent sees the pointer disappear and tears the side
down. Reply `migr_id`.

**CancelMigration** — the mirror rollback: the STM removes the
**destination** `Side`, the `Migration` and its bitmaps, returns the
destination DN's extents, and bumps `SpRev` and the destination's `DnRev`.
The source side returns to normal service on the next `SyncupSide` (ANA
back, the migration export dropped); the destination agent tears its stack
down, the `LocalMigrBmPath` files included.

**GetMigration** — one STM read.

**AppendMigrationBitmap** — Errors: `INVALID_ARGUMENT` empty `bitmap`;
`RESOURCE_EXHAUSTED` when `bm_cnt` is at or above `MaxMigrBmCnt`.
Action: STM: write a `MigrBitmap` at a `bm_idx` equal to `bm_cnt`,
advance `bm_cnt`, bump `SpRev`. The chunks concatenate, in `bm_idx`
order, into one bitmap over the **leg's data region**: bit k covers
`block_size` bytes, and **1 means never written, hence skippable**. Every
append creates a **new** chunk key, so a written chunk is immutable.
Delivery to the destination DN goes through `PushMigrBitmap` (Bitmap push
protocol); the destination agent shifts the bitmap by the leg's
`meta_blocks` (`migr_dst_conf.meta_blocks`: the meta region — md
superblock, bitmap, health block — must always be copied, Group on-leg
layout: meta region, data region, health block) and discards the fully
skippable dm-clone regions (`dnagent.md` DN15).

### Spare legs

**CreateSpareLeg** — Errors: `NOT_FOUND` when `grp_id` is not in the SP;
`INVALID_ARGUMENT` when the group is `RedundNone`; `RESOURCE_EXHAUSTED` at
`MaxSpareLegPerGrp` or when there is no DN (Per-operation allocation);
`FAILED_PRECONDITION` at an `sp_level` at or above `SP_LEVEL_NO_THINPOOL`
(the model op's level gate, as for GrowSlice), and while a spare of the
group still has a side that is not `provisioned` (reason
`model.ReasonSpareUnprovisioned`): for as long as that holds, the refusal
keeps two overlapping sp-worker owners from creating two spares for one
repair (`dnv-worker.md` AR2, AR8). Action: STM: a new `Leg` — a fresh
`leg_id`, `leg_idx` the next unused index in the group, one `Side` written
`provisioned` false ([D15]) — appended to `spare_leg_list`; the DN's
bookkeeping and `DnRev`; a `SpRev` bump. Every cntlr connects to the
spare's side and health-checks it (Primary cntlr, the legs), **but the
spare is not added to the md array**: it is pre-connected standby capacity
only. An unprovisioned spare defers only itself — spares never assemble
("Make sure all groups are available") — and reports
`RES_STATUS_PROVISIONING` until it clears. Reply `leg_id`.

**DeleteSpareLeg** — Errors: `NOT_FOUND` for `grp_id` or `leg_id`;
`FAILED_PRECONDITION` when the spare has two sides: a migration is running
on it (Migrations accepts a spare's side as a migration source), and
releasing both would leave the `Migration` naming sides in no leg, which
neither `FinishMigration` nor `CancelMigration` could then end. Action:
STM: remove the spare from `spare_leg_list`, give the DN its bookkeeping
back, bump `SpRev`. Reply `leg_id`.

**SwitchSpareLeg** — the only way a spare becomes active; users invoke it,
and so does the sp worker's leg repair (Automatic reactions; `dnv-worker.md`
AR8). Errors: `NOT_FOUND` for ids not in the group's lists;
`FAILED_PRECONDITION` when the spare's side is not yet `provisioned` (a
spare must never become an md member before its meta region, where md
looks for a superblock, is zeroed, [D15]), when either leg has
two sides (a migration is running on it and holds the leg until
`FinishMigration` or `CancelMigration` ends it, so a migrating leg is
neither promoted nor parked), or at an `sp_level` at or above
`SP_LEVEL_NO_THINPOOL` (the model op's level gate, as for GrowSlice).
Action: STM swap: `spare_leg_id` moves to `leg_list`, taking
the active role, and `target_leg_id` moves to `spare_leg_list`; bump
`SpRev`. The primary then fails and removes the target if the array still
lists it and adds the promoted spare with failfast, and md rebuilds onto it
— cheaply through the write-intent bitmap when the target was only briefly
absent, but a fresh spare gets a full resync (`cnagent.md` CN12, member
reconciliation). Reply the current active and spare leg ids.

### Bitmap reads

**GetThinDeviceBitmap** — inputs `td_name`, `slice_idx`, `start_block` and
`block_cnt`. Errors: `NOT_FOUND` td; `FAILED_PRECONDITION` when the SP has
no primary cntlr; `INVALID_ARGUMENT` `slice_idx` at or above the SP's slice
count (judged after the td and the primary); `ABORTED` when the agent call
fails. The gateway resolves the primary cntlr's CN in an STM, then,
outside it, calls the agent's `GetThinDeviceBm`, which computes from a
dm-thin metadata snapshot of that slice's pool the mapping bitmap of the
td's thin volume in that slice (`cnagent.md` CN25, CN26): bit k, for block
`start_block` plus k of `block_size` bytes, is **1 iff the block is
unmapped, never written**. A zero `block_cnt` reads to the end. Callers
page through it and feed `AppendCloneBitmap` on the destination. A caller
that pages in reads aligned to the source blocks one chunk covers — eight
times `CloneBmChunkBytes`, one bit per block — maps each page to its chunk
statelessly, its `bm_idx` being `start_block` divided by that span, which
is all it takes to satisfy the within-a-chunk append order of Clones
without tracking any cursor.

**GetLegBitmap** — inputs `leg_id`, `start_block` and `block_cnt`. Errors:
`NOT_FOUND` leg; `FAILED_PRECONDITION` when the SP has no primary cntlr;
`ABORTED` when the agent call fails. The
same path through the agent's `GetLegBm`: the agent walks the pool
metadata of the owning slice and translates pool-data blocks through the
pool-data linear concat and the group geometry down to this leg's **data
region** (`cnagent.md` CN27), returning bit k as **1 iff no pool block maps
there**. Callers feed `AppendMigrationBitmap`, and Migration says when a
caller may read the bitmap.

**Wire convention.** Every bitmap RPC — `GetThinDeviceBitmap`,
`GetLegBitmap`, `Append*Bitmap`, `Push*Bitmap` — uses **1 = unwritten,
skippable**, while thin-pool metadata natively answers **mapped =
written**; agents and callers invert once at
the boundary (raid0 bitmap math), and the gateway passes the bytes through
verbatim (`gateway.md` GW14). The **bit order** is normative too: bits are
**LSB-first within each byte** — bit i is bit i mod 8 of byte i div 8 —
and the trailing pad bits of the last byte are zero. Chunks are
byte-aligned, so the bit-level concatenation of a migration's chunks is
plain byte concatenation (Bitmap push protocol, raid0 bitmap math).

## Agent services and agent behavior

### Common agent rules

* **Local store.** The agent persists, per synced object, the serialized
  **last accepted request**, at the point the Revision gate below gives,
  as a flat protobuf file: `SyncupDnRequest` at `LocalDnPath`,
  `SyncupSideRequest` at `LocalSidePath`, `SyncupCnRequest` at
  `LocalCnPath`, `SyncupCntlrRequest` at `LocalCntlrPath` — plus one
  file per received bitmap chunk at `LocalMigrBmPath` or
  `LocalCloneBmPath` (Agent local-store paths, Bitmap push protocol).
  Every write goes to a temp file in the same dir, fsync,
  rename, fsync the dir (until then a crash can undo the rename). A write
  that does not succeed can leave its temp file behind, as one whose process
  dies between the write and the rename does (`osclient.md`, ReadFile /
  WriteFile / WriteFileDirect, lists every way). The committed file is the
  only truth: the next start deletes such a file unread (`dnagent.md` SH6),
  since what it held was never committed and the worker sends again whatever
  the committed files lack. The stored request contains the revision, so no
  separate revision record exists. In memory each side's and each cntlr's
  current request is an atomic pointer, so another object's pass reads it
  whole beside the converge that replaces it; a node's request is replaced
  only under the node write lock. On start the agent loads every
  other file of its role's kinds under its prefix, reconciles the system to
  it (full idempotent re-apply; Clone crash recovery for clones), then
  serves — except that it skips, neither loading nor deleting them, the
  files that may belong under a file it cannot load: a file that did not
  load names no object, so it proves no owner gone (`dnagent.md` DN2 and
  `cnagent.md` CN2 say which files that holds back). When an object
  disappears from its parent's pointer list, the agent drops its file or
  files, its chunk files, its in-memory entry and its object lock in that
  same pass, **before** anything of it is removed — save a file a restart
  left unloaded that no memory entry has named since (a file that did not
  load, or one skipped as above): the sweep (Teardown by sweep) removes its
  object's resources by name in that pass, and such a file stays until a
  later restart decodes it and finds the pointer absent (`dnagent.md` SH7).
  The agent keeps no record of the object afterwards because it needs none:
  the sweep finds the object's resources by their names, and a record kept
  until they were gone would be a record of work owed — the very thing that
  lets a failed teardown be forgotten.
* **Revision gate.** A `Syncup*` request with a revision **lower** than
  the stored one is rejected (`AgentReply.code` non-zero, `details`
  explains). Equal revision: re-apply idempotently (workers retry).
  Higher: accept, subject to the conf gate below. When an accepted request
  is persisted is the agent docs' rule — a node's request before its
  converge, a side's or a cntlr's after it (`dnagent.md` SH5, `cnagent.md`
  CN7). `Push*Bitmap` carries no revision and passes no such gate; Bitmap
  push protocol says why.
* **Conf gate.** Agents resolve no conf defaults of their own (Common
  validation): a geometry an agent invented is one the rest of the
  cluster does not share, and dm, md and the on-disk headers would be
  built against it. A request carrying a value the control plane cannot
  have written — `SyncupDn`'s `extent_size` zero; a zero among the three
  always present defaultable members of `SyncupCntlr`'s `bdev_conf` plus
  the `bitmap_chunk_block_cnt` that exists only under an md-raid1
  `redund_conf`; or `SyncupSide`'s `side_conf.zero_bytes` zero, not a
  multiple of `DnZeroAlign`, or longer than the side's `ext_cnt` extents
  (Side provisioning protocol; `dnagent.md` DN8) — is refused with its
  own `AgentReply.code` (`ReplyCodeInvalidConf`, `dnagent.md` SH9)
  **after** the revision gate and **before** the request becomes the
  desired state, so nothing converges, nothing is persisted, and the
  restart reconcile cannot replay it; the reply echoes the revision the
  agent still holds, so the worker sees the request was not accepted. A
  persisted file carrying such a value never becomes live either, and
  both roles get there the same way: the file is **loaded**, never
  skipped, and refused inside the converge, which then converges and
  sweeps nothing of that DN and its sides, of that side, or of that
  cntlr — a conf fault must not destroy resources (`dnagent.md` DN2 says
  why skipping the file instead would, and DN8 what still runs for a
  refused side; `cnagent.md` CN8).
* **Full sync.** Every `Syncup*` request carries the complete desired
  state of its object — there is no partial mode.
  `SyncupDn.side_pointer_list` and `SyncupCn.cntlr_pointer_list` are
  authoritative: pointers in the request but not local ⇒ add; local but
  not in the request ⇒ drop the object's state and let the sweep (Teardown
  by sweep) remove its resources by name (ids are never reused, so a
  deleted side or cntlr never comes back). There is no to-be-deleted list:
  a list of removals still owed is memory of a failure, and the agent
  keeps none — what has to go is recomputed from the live node on every
  pass. The embedded object lists inside `SyncupSide` and `SyncupCntlr`
  are authoritative the same way, and what they do not name is swept,
  never diffed against the request applied before it. Bitmap chunks are
  the one thing that never rides in a `Syncup*` request; they travel only
  over the dedicated `Push*Bitmap` RPCs (Bitmap push protocol).
* **RPC shapes.** `Syncup*`, `Push*Bitmap`, `Get*Info`, `GetDnSize` and
  `GetCnSize`, and `GetThinDeviceBm` and `GetLegBm` are unary; the four
  `Check*` RPCs are the only bidirectional streams (Check streams).
  `Syncup*`, `Push*Bitmap`, `Get*Info` and `Check*` replies embed
  `AgentReply` with its `code` and `details` (zero is OK); `Syncup*` and
  `Get*Info` replies also carry the current `*Info` (Live-state reporting),
  and every `Syncup*`, `Get*Info` and `Check*` reply additionally carries
  the revision of the last request the agent accepted for the object
  (`revision`, Live-state reporting) — the exact reply fields are
  listed per RPC under `service DiskNodeAgent` and
  `service ControllerNodeAgent`. `PushMigrBitmapReply` and
  `PushCloneBitmapReply` carry only `AgentReply`; `GetDnSize`, `GetCnSize`,
  `GetThinDeviceBm` and `GetLegBm` return only their payload (`size`,
  `bitmap`) and report failure through the gRPC status. A worker issues the
  `Syncup*` calls for one object sequentially — the next revision only after
  the previous call returned.
* **Error capture.** Shell execution uses the command timeouts (Common
  validation); failures are captured into the affected resource's
  `ResInfo` as `RES_STATUS_ERROR` with the reason in `details` rather than
  crashing the reconcile — the agent always converges as much as it can
  and reports the rest.
* **Bitmap durability.** Received `Push*Bitmap` chunks are persisted per
  chunk under `LocalMigrBmPath` or `LocalCloneBmPath`; the applied sets
  reported through `bm_info.bm_idx_list` and `bm_info_list`'s
  `chunk_id_list` are derived from the files present, so they survive
  agent restarts and the worker does not have to re-push after one — save
  a chunk whose own file, or whose owner's or that owner's parent's, the
  restart could not load: it stays out of the applied set — left on disk
  or deleted — so the worker pushes it again if it still wants it
  (`dnagent.md` DN2, `cnagent.md` CN2). Re-applying a chunk is always
  harmless — re-`blkdiscard`ing an already-hydrated dm-clone region is a
  no-op, which holds only because every dnv dm-clone carries
  `no_discard_passdown` ([D7]; `cnagent.md` CN18). Bitmap push protocol
  and [D7] give the whole protocol.

### `service DiskNodeAgent`

* `GetDnSize` returns the byte size of the `--disk` device's **data area**:
  the raw size minus the fixed `DnDataOffset` prefix of the [D13] format.
  A device at or below `DnDataOffset` is an Internal error. The gateway
  calls it before registration; `dn_id` in the request is for logging
  only (`dnagent.md` DN3).
* `SyncupDn` carries `revision`, `side_pointer_list` and `extent_size` (the
  cluster's `dn_bin_conf.extent_size`, stamped into the disk header at
  format time and immutable from then on, Disk node). It ensures the Disk
  node base state — the [D13] disk format and this agent's own nvmet port
  — and diffs the pointer list per Common agent rules. Reply
  `agent_reply`, `revision`, `dn_info`.
* `SyncupSide` carries one `side_pointer`, `revision`, `side_conf`
  (`ext_cnt`, `cntlid_slot`, `primary_cn_id`, `standby_id_list`,
  `sp_level`, `provisioned`, `zero_bytes`) and — only when this side is a
  migration endpoint — `migr_src_conf` (`migr_id`, `dst_side_id`,
  `dst_dn_id`, `dst_provisioned`: the source role, Migration) and/or
  `migr_dst_conf` (`migr_id`, `src_side_id`, `src_dn_id`,
  `src_nvme_tr_conf`, `block_size`, `meta_blocks`, `dm_clone_conf`,
  `bm_cnt`: the destination role). It is rejected if the pointer is
  unknown (`SyncupDn` must introduce it first). It converges the per-side
  stack of Disk node: the side device of `ext_cnt` extents and its
  zeroing state (Side provisioning protocol), the per-CN dm-error,
  dm-linear and nvmet subsystem, the primary versus standby table targets
  and ANA states, and the migration source and destination roles
  (Migration). Reply `agent_reply`, `revision`, `side_info` (which always
  reports `zeroed_bytes` and `zero_bytes`, Side provisioning protocol) and
  `bm_info` (the applied migration-bitmap indexes, Bitmap push protocol).
* `PushMigrBitmap` delivers one `MigrBitmap` chunk (`side_pointer`,
  `migr_id`, `bm_idx`, `bitmap`) to the **destination**-side agent, per
  Bitmap push protocol: persist the chunk at `LocalMigrBmPath`, then
  recompute and `blkdiscard` the fully skippable dm-clone regions
  (Migrations, raid0 bitmap math). Reply `agent_reply` only.
* `GetDnInfo` and `GetSideInfo` return the current `DnInfo` or `SideInfo`
  without changing anything (`agent_reply`, `revision`, the info).
* `CheckDn` and `CheckSide` are the health streams, one per DN or per
  side, with the protocol of Check streams. Request: the ids, `revision`,
  `show_info` and `trace_id` (the sending round's id); reply:
  `agent_reply`, `revision`, `dn_info` or `side_info`.

### `service ControllerNodeAgent`

* `GetCnSize` returns the capacity budget in bytes this CN is willing to
  host, zero meaning "use the default"; typically from local config
  (`cnagent.md` CN3).
* `SyncupCn` carries `revision`, `qos_ratio` and `cntlr_pointer_list`. It
  ensures the base state of Controller node, common — the tmpfs, the
  sparse backing file and the single loop device of the clone-metadata
  arena ([D14]), the loop device re-learned with `losetup --associated`
  and never persisted, and the single nvmet port; it accepts and persists
  `qos_ratio` and programs no limit (Controller node, common; `cnagent.md`
  CN6); and it diffs the pointers.
  Reply `agent_reply`, `revision`, `cn_info`.
* `SyncupCntlr` carries one `cntlr_pointer`, `revision`, `bdev_conf`
  (`dm_pool_conf.low_water_mark_pct` included, Primary cntlr),
  `sp_level`, `cntlr`, `id_to_slice` (keyed by the `slice_id` rendered
  with `IdKeyFmt`), `td_list`, `nqn_to_subsystem`, `clone_list`,
  `xfer_list` and `migr_list`. It converges Primary cntlr or Standby
  cntlr; a primary-to-standby or standby-to-primary flip follows Failover
  exactly, and a clone rebuild follows Clone crash recovery. Reply
  `agent_reply`, `revision`, `cntlr_info` and `bm_info_list` (the applied
  clone-bitmap chunks, one `BitmapInfo` per clone, its applied set carried
  as `chunk_id_list` of `(src_slice_idx, bm_idx)` pairs, Bitmap push
  protocol; `bm_idx_list` is for migrations only).
* `PushCloneBitmap` delivers one `CloneBitmap` chunk (`cntlr_pointer`,
  `clone_id`, `src_slice_idx`, `bm_idx`, `bitmap`) to the **primary**
  cntlr's agent, per Bitmap push protocol: persist the chunk at the
  `LocalCloneBmPath` of its pair, translate through raid0 bitmap math,
  `blkdiscard` the dm-clone. Safe at any time (Clone crash recovery).
  Reply `agent_reply` only.
* `GetCnInfo` and `GetCntlrInfo` return the read-only live state
  (`agent_reply`, `revision`, the info).
* `GetThinDeviceBm` and `GetLegBm` serve the gateway's bitmap reads
  (Bitmap reads) from a dm-thin metadata snapshot, reserved, dumped and
  always released (`cnagent.md` CN25 to CN27): the per-slice td mapping
  bitmap, or the leg-projected pool mapping bitmap. Reply bitmaps use the
  wire convention **1 = unmapped**.
* `CheckCn` and `CheckCntlr` are the health streams, one per CN or per
  cntlr, with the protocol of Check streams. Request: the ids, `revision`,
  `show_info` and `trace_id` (the sending round's id); reply:
  `agent_reply`, `revision`, `cn_info` or `cntlr_info`.

### Side provisioning protocol

A new side is built from extents that an earlier side, of any pool, may
have written, and those bytes stay on the extents until something
overwrites them. No host reads them: a host reads a pool only through the
pool's thin devices, which read a block they have not provisioned as
zeros and write a block whole before a host can read any of it ([D15]). The
nodes, though, read metadata at fixed places of a side, and where this
design *assumes* zeros there, stale bytes break correctness (What is
zeroed, below). A discard cannot give those zeros: discard-reads-zeros is
not a hardware property — the kernel does not promise that a discarded
region reads as zeros, and NVMe read-zeroes after deallocate is
optional. Therefore **every side has the prefix its group needs zeroed
before its first export**, tracked on disk as a byte count, gated by a
CP-visible `provisioned` flag ([D15]). This holds for every new side:
made with the pool, by a grow or an auto-grow, as a spare leg, or as a
migration destination.

**What is zeroed.** Every side of a group has the same length zeroed from
its start (Group on-leg layout: meta region, data region, health block):

* A side of a **meta group** is zeroed over its whole leg span. The group
  holds the thin pool's metadata, and dm-thin formats a fresh pool only
  when the first block of its metadata device reads zero: a recycled
  extent can hold a previous SP's valid thin-metadata superblock, which a
  fresh pool would adopt.
* A side of a **data group** has its meta region and its first data block
  zeroed, `meta_blocks` blocks and one more, with RAID1 or without. With
  RAID1, a stale md superblock in the meta region would put the group
  assembly, which creates the array clean over legs that carry no
  superblock, in the wrong case ("Make sure all groups are available"),
  with no `--zero-superblock` escape. Without RAID1 the meta region is the
  health block alone, but a node's udev and md tools look for a
  superblock there all the same, and one rule for every data side costs
  one block. The first data block is the first block of the md array or,
  without RAID1, of the group device, and for a slice's first data group
  the first block of the pool's data device. Extents are reused
  lowest-first, so a new array would often start on an earlier pool's
  volume head — a partition table, an LVM label, a file system — if that
  block were not zeroed, and the controller node's udev, partition scan
  and LVM activation read it when the array appears.

The rest of a data side is never zeroed, nor is any tail; [D15] says why
no pool reads those bytes and what a recycled tail can still cause.

**Length.** The disk node knows nothing of groups. The sp worker computes
the length to zero when it builds a side's request, from the group it
holds and the pool's `block_size`, and sends it as `side_conf.zero_bytes`
(sp role; `dnv-worker.md` RW15). A meta side's length is its leg span
rather than its `ext_cnt` extents: the span is whole blocks, which are
multiples of `DnZeroAlign`, while `extent_size` need not be a multiple of
it (Common validation), and the span needs no extent size, so the sp
worker reads no cluster conf for it. The gateway stores nothing for the
length, and no agent carries a second copy of the leg layout rule. A
stored group whose block counts give no length is refused like an invalid
stored geometry: the sp worker builds no request for its pool and runs no
reaction for it (`dnv-worker.md` RW14, AR1). The dn agent records the
length when it allocates the side, and its conf gate refuses a length of
zero, one that is not a multiple of `DnZeroAlign`, and one longer than
the side (Common agent rules).

**Key invariant.** *Zeroed is a property of the side's allocation, not of
the disk extent* — extents freed and reallocated to a new side start
unzeroed again, whatever happened to them before. A side's volume-table
record holds the length to zero, written when the side is allocated, and
the count of bytes zeroed from the side's start (`zero_bytes`,
`zeroed_bytes`); the zeroing is complete when the two are equal and the
length is above zero.

**Batches and timeouts.** Zeroing commands run under the ordinary command
timeouts (Common validation); there is no special zeroing timeout. A disk
needs no Write Zeroes support: where it has none, the kernel answers
`blkdiscard --zeroout` by writing zero pages, which is slower. A batch is
bounded in bytes, at most `DnZeroBatchMaxBytes`, so how long it takes
depends on the disk's zeroing rate, not on the extent size. A slower or
busier disk costs kills rather than stopping zeroing: a batch the soft
timeout kills halves the side's next batch, down to `DnZeroBatchMinBytes`
(`dnagent.md` DN9), so zeroing stalls only where the zeroing rate left to
this agent, which is what host IO and any other dn agent on the same
physical disk leave it, split `DnZeroConcurrency` ways, cannot zero that
floor inside `CmdSoftTimeout`. Zeroing has no tuning of its own: tuning
describes hardware, so it would be per-node agent configuration, while
`ClusterConf` is write-once and cluster-wide; and raising the global
timeouts stretches every command's bound, not just zeroing's.

**Protocol and gate.** The dn agent allocates the side's extent runs and
persists their record with the request's length to zero and a zeroed
count of zero, builds `DnSideName` over the runs, zeroes from the stored
count up to the length in the background through that device —
persisting the count after each batch, so the protocol is restart-safe at
every point — and converges the per-CN export stacks **only** when the
request says `provisioned` true **and** the count has reached the length.
Allocation is permitted **only** at `provisioned` false: at true a
missing record means the data is gone (a lost or foreign disk), and
silently re-allocating would present a fresh impostor as the data-bearing
leg, so it is a hard resource error on `side_dev_info` that feeds
`err_epoch` and the replacement flows (raid1: the spare switch, which
Automatic reactions performs after `leg_unhealthy`; `RedundNone`:
effectively delete-SP). **The agent always trusts its own count over the
flag** — the disk is authoritative ([D13]); the etcd flag is a gate,
never evidence. A record whose extent total disagrees with `ext_cnt`, or
whose length to zero disagrees with the request's, is an error. While the
gate is closed every resource above the side device reports
`RES_STATUS_PROVISIONING`, and `RES_STATUS_ERROR` stays confined to
`side_dev_info`. `SideInfo.zeroed_bytes` and `zero_bytes` are filled on
every reply and every Check round; equal values, with the length above
zero, mean the zeroing is complete, which is what the sp worker's flip
rule reads (sp role). Zeroing runs at **every** `sp_level`,
`SP_LEVEL_DISABLE` included (SpLevel): it is bottom-layer provisioning.
`dnagent.md` DN9 holds the converge matrix, the zeroing goroutine and its
batch sizing; `dnagent.md` DN18 holds the rows of the read-only probes
(`GetSideInfo`, `CheckSide`), which never allocate and never start or
stop the goroutine (`dnagent.md` DN16).

### Live-state reporting

`ResInfo` carries `res_name`, `status`, `details` and `epoch`, with
`ResStatus`:

* `RES_STATUS_UNKNOWN` — no answer from the node; set by *workers*, never
  by the agent itself.
* `RES_STATUS_MISSING` — the agent has not created it.
* `RES_STATUS_ERROR` — tried and failed; `details` says why, or carries
  the command output — *needs intervention*.
* `RES_STATUS_OK`.
* `RES_STATUS_PROVISIONING` — deliberately not created, being prepared,
  or kept unprobed behind a side's provisioning gate that closed again
  (`dnagent.md` DN9): **healthy, not ready, no action needed**.
* `RES_STATUS_PENDING` — the primary's prober for the leg has not
  completed a round, or none is registered yet: **no verdict, no action
  needed**. Only the primary's leg report produces it (`cnagent.md` CN11).

`epoch` is the unix seconds of the last status change. The `revision`
returned next to an `*Info` (`Syncup*`, `Check*` and `Get*Info` replies)
is that of the request the agent holds for the object — the last one it
accepted (`dnagent.md` SH8, SH25) — whatever the outcome of its converge.
Map keys in `SideInfo` and `CntlrInfo` are the obvious owner ids —
`cn_id` for a side's per-CN export stack; the td, slice, group, leg,
namespace, subsystem, clone and transfer ids in `CntlrInfo`, with
`td_id_to_thin_info[td].slice_id_to_dm_thin` for the per-td × slice thin
volumes; the sp worker's `created` flip reads exactly these rows (sp
role). dm-clone `details` strings carry the raw `dmsetup status` line, so
hydration progress is visible through `Inspect*`.

`CntlrInfo.leg_id_to_leg` reflects the health check of Group on-leg
layout: meta region, data region, health block — the block-probe IO on the
primary, transport liveness plus the expected ANA state on a standby;
`cnagent.md` CN11 says how each probe judges and when a leg reads
`RES_STATUS_PENDING`. A leg whose prober has not completed a round does
not read `RES_STATUS_OK`, because an OK clears `Leg.err_epoch` (sp role);
`RES_STATUS_PENDING` neither sets nor clears it.

`RES_STATUS_PROVISIONING` **never** sets `err_epoch` (dn / cn roles, sp
role) and never counts as a bad status for the capacity keys (Capacity
index keys): it marks a side device still zeroing (Side provisioning
protocol), and a resource excluded from the *effective* desired state
while a side of its backing chain is still zeroing, or while a side's own
provisioning gate holds it back (Side provisioning protocol, [D15]). Only
those carry it — a
serving thin pool keeps reporting `RES_STATUS_OK` with its raw `dmsetup
status` details even while a grow is deferred, so the thin-pool auto-grow
of Automatic reactions keeps parsing them. `SideInfo` additionally
reports `zeroed_bytes` and `zero_bytes` on every reply and every Check
round, which the sp worker's flip rule reads (sp role).

### Bitmap push protocol

The skip bitmaps of Clones and Migrations reach the data plane exclusively
through two **unary** RPCs, `PushMigrBitmap` and `PushCloneBitmap` —
never through `Syncup*`. Both follow one protocol; the table maps the
per-RPC roles:

| | `PushMigrBitmap` (`DiskNodeAgent`) | `PushCloneBitmap` (`ControllerNodeAgent`) |
|---|---|---|
| chunk source in etcd | `MigrBitmap` keys, `bm_idx` the append sequence from zero below `bm_cnt` (at most `MaxMigrBmCnt`) | `CloneBitmap` keys, addressed by the pair `(src_slice_idx, bm_idx)`: `src_slice_idx` below `src_slice_cnt`, itself at most `MaxSliceCntPerSp`, and `bm_idx` below `MaxCloneBmCnt` chunks **per slice** |
| chunk meaning | chunks **concatenate in `bm_idx` order** into one bitmap over the leg's data region (Migrations); immutable once written | each chunk is a **self-positioned** slice of source slice `src_slice_idx`'s bitmap: chunk (s, b) holds the bytes of slice s's bitmap from b × C on, for its own length of at most C = `CloneBmChunkBytes` (Clones); `AppendCloneBitmap` may keep growing it within C |
| receiving agent | the DN hosting the migration's **destination** side | the CN hosting the **primary** cntlr (only it runs the dm-clone) |
| request fields | `cluster_id`, `dn_id`, `side_pointer`, `migr_id`, `bm_idx`, `bitmap` (no `revision`: below) | `cluster_id`, `cn_id`, `cntlr_pointer`, `clone_id`, `src_slice_idx`, `bm_idx`, `bitmap` (likewise) |
| local chunk file | `LocalMigrBmPath` of the cluster, dn, sp, migration and `bm_idx` | `LocalCloneBmPath` of the cluster, cn, sp, clone and the pair |
| applied-set report | `SyncupSideReply.bm_info` (`BitmapInfo.res_id` the `migr_id`, applied set in `bm_idx_list`) | `SyncupCntlrReply.bm_info_list` (one `BitmapInfo` per clone, `res_id` the `clone_id`, applied set in `chunk_id_list` as `(src_slice_idx, bm_idx)` pairs) |
| apply action | shift by the leg's `meta_blocks` (Migrations), then raid0 bitmap math → `blkdiscard` on `DnMigrFinalName` | raid0 bitmap math → `blkdiscard` on `CnCloneFinalName` |

**Why chunked, why ordered.** A whole bitmap might be too large for one etcd
value or one gRPC message (`GetThinDeviceBitmap` and `GetLegBitmap` are
paged for the same reason), so it is split into multiple parts and the key
fixes each part's position so the receiver can place it correctly. For
migration chunks the position is by concatenation: chunk *k*'s first bit
sits at the summed bit length of chunks zero to *k*−1, so chunk *k* is only
interpretable once every lower chunk is present — delivery order matters.
Clone chunks are positioned by `(src_slice_idx, bm_idx)` alone:
`src_slice_idx` picks the source slice and `bm_idx` fixes the chunk's byte
offset within that slice's bitmap at the FIXED quantum C, independently of
any other chunk's existence or length. They are therefore order-independent
among each other, gap-tolerant, and growable in place; skipping the all-zero
chunks of a mostly-written slice is a legitimate optimization, not a
protocol violation. An absent chunk reads as all-zero (= all-written) and a
short chunk's missing tail reads as written — always the safe direction, a
region copied instead of discarded.

C is sized so that a grown chunk value plus the rev-bump put stays inside
etcd's default request-size cap and every `PushCloneBitmap`
message inside gRPC's default message size; `MaxCloneBmCnt` chunks bound
the bitmap of one slice, and `MaxSliceCntPerSp` slices of them bound one
clone's bitmaps, measured against etcd's default storage quota. With the
defaults the bitmaps are far smaller: the chunking exists for the
value-size limits and the paged readers, not because bitmaps are
inherently huge.

**The push, end to end.** A push is one `PushMigrBitmapRequest` or
`PushCloneBitmapRequest` carrying the cluster and node ids, the object's
addressing ids (`side_pointer` and `migr_id`, or `cntlr_pointer` and
`clone_id`), the chunk address (`bm_idx`, or `src_slice_idx` and `bm_idx`)
and the raw chunk bytes. It carries **no revision and is gated on none**:
the chunk is position-addressed data under an id that is never reused,
applying it never advances the agent's stored revision, and an agent that
does not know the object refuses the push by name — so a push planned
against a report that a newer desired state has replaced is either still
correct or refused, and a revision gate would only ever discard work the
next round is about to redo. The worker learns what is missing from the
applied sets the `SyncupSide` and `SyncupCntlr` replies carry — the chunks
etcd holds that the set does not list, compared as whole pairs for a clone
— and pushes them one part per request, one in flight per migration or
clone, in ascending address order (load-bearing for a migration, whose
chunks concatenate; deterministic only for a clone), to the destination
side's DN or to the CN hosting the primary cntlr; calls for different
objects are independent. A failure ends that object's plan and is
remembered nowhere: the object's next `Syncup*` reply reports its applied
set again, and what is still missing is planned again from that
(`dnv-worker.md` BM1 to BM6).

**The agent's contract.** Its only gate is the object's identity and the
chunk's address, never a revision: a `migr_id` or `clone_id` it does not
know, or on the CN a pair outside the clone's own geometry, is refused
with a non-zero `code`. It **persists before it applies**: the chunk goes
to its `LocalMigrBmPath` or `LocalCloneBmPath`
file first, with the atomic, durable replace of Common agent rules, and
only then are the fully skippable dm-clone regions recomputed — for a
migration from the contiguous prefix of its chunks from `bm_idx` zero,
shifted by the leg's `meta_blocks` (Migrations); for a clone from every
present chunk, read in place, an absent chunk, a short chunk's tail and a
slice holding no chunk all reading as written — and `blkdiscard`ed onto
`DnMigrFinalName` or `CnCloneFinalName` (raid0 bitmap math). A chunk whose
dm-clone does not exist yet still counts as applied and is re-applied
whenever the dm-clone is (re)created, the clone rebuild of Clone crash
recovery included. The reply carries only `AgentReply`, and a code-zero
reply is the acknowledgement that releases the worker's next part — not
proof that the chunk was applied: a chunk whose persist failed is acked
code zero too, and heals because the applied set is derived from the files
present (Common agent rules, Bitmap durability), so the object's next
`Syncup*` reply reports it missing and the worker pushes it again. Nothing
else schedules that re-push — no push outcome schedules a `Syncup*`, and
the `Check*` rounds carry no applied set — so the chunk waits for whatever
re-issues the object's `Syncup*` anyway (a revision bump, or a `Check*`
round the agent answers with another revision or a non-zero code, Check
streams); until then its regions are copied instead of skipped, which
costs only the optional bitmap fast path. The files go with their object:
they are deleted when its pointer leaves the parent's list, before the
sweep of that pass removes its devices by name (Common agent rules;
`dnagent.md` SH7), while a role or an `sp_level` that merely suppresses the
dm-clone keeps them, applied-by-file, so that nothing is re-pushed when the
suppression lifts. `dnagent.md` DN15 and SH21 to SH23
and `cnagent.md` CN22 carry the agent side out.

**Grown clone chunks ([D8]).** `AppendCloneBitmap` may append more bytes
to a chunk `(src_slice_idx, bm_idx)` that was already pushed and
acknowledged — up to the chunk's `CloneBmChunkBytes` ceiling. The worker
therefore memoizes, per `res_id`, `src_slice_idx` and `bm_idx` and for
both kinds, the etcd mod revision of the chunk it last pushed, and
re-pushes a chunk whose mod revision advanced past it (`dnv-worker.md`
BM5); the agent overwrites the stored file and re-applies whenever a
received payload differs from it. If the worker changes (crash, shard
re-ownership) the memo is lost, and a chunk that grows afterwards while
staying in the acknowledged set may keep its shorter version at the agent
— accepted: source bitmaps are a pure optimization that never affects
correctness (Clones, Clone crash recovery); the only cost is copying some
regions that could have been skipped. A failed persist of a grown chunk
is the same bounded loss: the pair stays in the applied set with its
shorter payload and the code-zero ack refreshes the memo, so the agent
keeps the shorter version until that chunk grows again. Migration chunks
are immutable (every `AppendMigrationBitmap` creates a new `bm_idx`), so
the memo's comparison is inert for them and they have no such caveat.

### Check streams

The four `Check*` RPCs — `CheckDn`, `CheckSide`, `CheckCn` and
`CheckCntlr` — are the only bidirectional streams of the agent services.
They carry no desired state — `Syncup*` does that — and exist so a worker
can watch an object's live state cheaply instead of polling `Get*Info`:

* **One stream per object.** The owning worker keeps one `CheckDn` per
  DN, one `CheckSide` per side, one `CheckCn` per CN and one `CheckCntlr`
  per cntlr open for as long as it owns the object (Membership and shard
  ownership). Rounds are **worker-initiated**: one request per health
  round, exactly one reply per request, never an unsolicited agent
  message. The round interval is the object kind's field of
  `ClusterConf.health_check_conf` (`dn_interval`, `cn_interval`,
  `side_interval`, `cntlr_interval`, in seconds; a request's zero becomes
  `DefaultHealthCheckInterval` at `CreateCluster`, within
  `MinHealthCheckInterval` and `MaxHealthCheckInterval`, Common
  validation — a **stored** zero is invalid and costs the object its
  whole round, not a default).
* **Request:** the object ids (`cluster_id` and `dn_id` or `cn_id`, plus
  `side_pointer` or `cntlr_pointer`), `revision` — the worker's current
  **desired** revision for the object (`dnv-worker.md` RW4; the agents
  ignore this request field, and the mismatch check below is the worker
  comparing the *reply's* revision against desired) —, `show_info`, and
  `trace_id`: the id of the worker round that sent it, under which the
  agent runs the round, because the stream's metadata names only the
  round that opened it (`grpc.md` T3).
* **Reply:** `agent_reply`, `revision` — the revision of the last request
  the agent accepted for the object (Live-state reporting) — and the
  `*Info`:
  * `show_info` true ⇒ the agent always fills the complete current
    `*Info` (Live-state reporting);
  * `show_info` false ⇒ the `*Info` is filled on the first reply of the
    stream and whenever the freshly probed `*Info` differs from the last
    one sent on this stream — protobuf equality, so any changed row,
    `details` or `epoch` counts, a side's advancing `zeroed_bytes`
    included, which the sp worker's flip rule reads (sp role) — and is
    left unset otherwise (`dnagent.md` SH26).
* **Revision check, and the re-sync it drives.** The worker re-issues the
  object's `Syncup*` when the reply's `revision` differs from the
  **desired** one or when `agent_reply.code` is non-zero at all,
  `ReplyCodeLeftover` included — which is the whole of the sweep's retry
  machinery (Teardown by sweep; `dnv-worker.md` RW4 and RW5).
* **Health.** The worker turns a broken stream, a missing reply within the
  round timeout, or `RES_STATUS_ERROR` entries in the `*Info` into the
  `err_epoch` updates of dn / cn roles and sp role (`RES_STATUS_UNKNOWN`
  is what the worker records itself while the stream is dead, Live-state
  reporting). The `Check*` replies are the primary health signal; the
  `Syncup*` replies are the secondary one.

### Teardown by sweep

Creation is driven by the desired state alone; **removal is driven by the
difference between the desired state and the live node**. An agent never
derives what to remove from what it remembers — not from the plan it
applied last time, not from a list of removals it owes, not from a flag
saying that one failed. It enumerates what the node actually holds,
subtracts what the desired state wants, and removes the rest. This is the
contract both roles share; `dnagent.md` DN6 and `cnagent.md` CN21 are the
per-role instances, and every rule that names a role's own objects,
listings or layers is stated there.

The reason is that a removal derived from "applied plan minus new plan" is
forgotten the instant the new plan is stored, so a removal that failed is
never attempted again — the object stays on the node with nothing left
anywhere that names it. The node cannot forget in that way: while the
device is there, the next enumeration finds it.

* **Actual minus desired, at two scopes.** *Node-level*, in `SyncupDn` and
  `SyncupCn` and at startup, under the node write lock: everything
  belonging to a side or an sp whose pointer has left the parent's list,
  plus the objects no side or sp can be read off at all. *Object-level*,
  inside one side's or cntlr's own converge, under the node read lock plus
  that object's own lock, so the objects of one node converge
  concurrently: that object's resources minus its **wanted set**, which is
  what the build phase would ensure for the stored request with every
  [D15] deferral that merely postpones an object left in the set — a
  provisioning leg's wrapper and a deferred group's array are wanted
  although the build skips them, or the pass that is about to build them
  would sweep them away first. The one deferral that shrinks the set
  instead is the source role of Migration, whose `dst_provisioned` gate
  makes a deferred source neither built nor wanted. The node-level pass
  never touches a side or sp that *is* in the pointer list, even when its
  own file is missing: the pointer arrives before the object's `Syncup*`
  (Common agent rules), and after a lost `--local-store` the resources
  must be re-adopted probe-first by that `Syncup*`. What else each scope
  holds back, and until when, is `dnagent.md` DN6 and `cnagent.md` CN21.
* **Attribution by name first, and "not mine" is not "nobody's".** Two
  questions are asked of every object an enumeration finds, in this order:
  *is it mine*, then *does anything still want it*. The role letter and
  the ids of a dm name say which agent, which cluster, which node and
  which sp a dm device belongs to (dm-device kinds, dm device names), and
  a dnv-format NQN says the same of itself (NQNs); an object whose name
  carries no owner is attributed by what it is built out of, and only
  then judged. Several agents share one kernel — a lab node runs a dn
  agent per disk and often a cn beside them, and dm, nvmet and the nvme
  host namespace are all per kernel — so an object attributed to another
  agent is **foreign** and is never touched, and only one that no agent's
  object can account for is **unowned** and removable: "I cannot attribute
  it" followed by a removal takes a healthy sibling's object, and skipping
  the first question is how a sweep does exactly that. Which objects of
  each role carry their owner in their name, how the rest are attributed,
  which arm each one falls to and what the reads cost is `dnagent.md` DN6
  and `cnagent.md` CN21.
* **State is dropped at pointer removal; the parent's list is persisted
  first.** An object whose pointer has left the list loses its file, its
  memory entry and its object lock in the same pass, before anything of it
  is removed (Common agent rules), and the parent's own request is
  persisted **before** the sweep, not after it: a sweep can block for a
  whole fast-IO-fail window on a dead remote — every leg is connected with
  a fast-IO-fail timeout and a controller-loss timeout that never expires
  (Primary cntlr), which is also the bound that makes a pass safe to run
  at all, since once that timeout has passed after a path loss every IO
  queued at that multipath head fails immediately — and a request
  cancelled in that window would skip a save made after the sweep, leaving
  the next startup to rebuild the object from the old list against sides
  that no longer exist. With the new list on disk first, a crash mid-sweep
  is nothing worse than a startup sweep (`dnagent.md` SH5, SH7;
  `cnagent.md` CN7).
* **An enumeration that did not answer licenses no removal.** "Actual
  minus desired" with an unanswered listing subtracts to *remove nothing*,
  which looks like the safe direction and is not: an empty actual is also
  the shape of "there is nothing left", so every removal gated on ABSENCE
  rather than on presence fires — the layer stop rule below never
  triggers, lower layers run as though the ones above them had succeeded,
  and the live-device half of the evidence that protects a shared object
  (a dm-clone still mapping its source) vanishes node-wide. A listing that
  did not answer is therefore reported as a failed enumeration — the
  verdict is not clean — and licenses no removal derived from it; the rule
  applies per object too: a live device that will not say what it maps
  keeps everything it might map in use for that pass, because recording a
  failure is not the same as acting on one — nothing downstream reads the
  failure list before removing. Which listings each role gates its pass
  on, and what a converge whose sweep stopped this way still runs, is
  `dnagent.md` DN6 and `cnagent.md` CN21; what the dn's gate leaves open
  is `dnagent.md`, Known limits.
* **"Gone" is probe-verified, never inferred from an exit status.** A
  command that **did not answer** — killed at the soft or hard command
  timeout (Common validation), never started, ctx cancelled — has told the
  caller nothing at all, and in particular has not said the object is
  absent: the kernel operation may well have completed, an ioctl finishing
  regardless of the signal that killed its process. So the exit status
  never decides. A fresh probe does, and a probe that did not answer either
  reads as *unknown*, which counts as still present. For an nvme connection
  that probe asks for a CONTROLLER, not for the subsystem directory: after
  its last controller is deleted the kernel keeps the subsystem's entry
  under "/sys/class/nvme-subsystem", with no controller and no namespace
  node in it, for as long as something holds its multipath head open — a
  dm table or an md array over that head — and drops the entry once the
  holder lets go. Such an entry is no connection, and read as present it
  would stop the descent below the connection's layer over a connection
  that no longer exists. What each role probes with, and what it releases
  only on a verified removal — on the DN an allocation record, because
  freeing extents a live device still maps hands the same blocks to the
  next side — is `dnagent.md` DN6 and `cnagent.md` CN21; the cn's
  `nvme disconnect` is the one removal that runs off the pass's locks
  (`cnagent.md` CN10).
* **Top-down, and the descent stops at the first layer that left something
  behind.** The layers are the stacks of Disk node and Primary cntlr read
  downwards. Every unwanted object of a layer is attempted, but the layer
  below is skipped when this one left anything: a leg pulled out from
  under a live md array, or a migration source pulled out from under a
  live dm-clone, is destructive, while a pass that simply runs again next
  round costs nothing — by then the failfast window has passed and the
  same order succeeds. The skipped layers' objects are reported as
  leftovers without being touched. Each role's layers, and the step each
  runs ahead of its first layer, are `dnagent.md` DN6 and `cnagent.md`
  CN21.
* **The verdict is recomputed every time and stored nowhere.** "Something
  is left" is the result of one comparison, never a flag. The read-only
  paths run the same enumeration and the same comparison with nothing
  touched, so every `Check*` round and every `Get*Info` answers a verdict
  as current as the sweep's own while still removing nothing. A leftover
  that has since gone therefore stops being reported by itself, and one
  that is still there keeps being reported without anything having
  remembered it.
* **A leftover travels as `ReplyCodeLeftover`.** It is the one
  per-resource outcome that cannot ride in the `*Info` rows of Live-state
  reporting: those rows are keyed by the ids of *wanted* objects, and a
  leftover is by definition something nothing wants. The code means
  **accepted with residue** — the desired state is stored and every wanted
  object converged — and is not a rejection: the worker reads the reply's
  rows exactly as for code zero and re-issues the object's `Syncup*` every
  round until the code changes (`dnv-worker.md` RW4, RW5), and nothing on
  the agent re-drives a sweep because something is left (`dnagent.md`
  DN17; `cnagent.md` CN10, CN30). The startup case needs no recovery step,
  because the first `Check*` after a restart reports whatever the node
  still holds. `details` names the leftovers, at most `maxLeftoverNames`
  of them, and reports an enumeration that did not answer the same way —
  an answer the agent cannot trust cannot prove the node clean — as it
  does each role's conditions that only a `Syncup*` cures (`dnagent.md`
  DN16; `cnagent.md` CN30). The agent log carries the full list
  (`log.md`, Leftovers), so a lingering leftover is visible every round
  rather than once.

## Workers

The worker contract: how the `dnv-worker` instances share the work, what
each role watches and drives, and the automatic reactions. `dnv-worker.md`
owns the worker and its mechanics; this section states what the other
components rely on.

### Membership and shard ownership

Membership is heartbeat-based and uses no etcd lease ([D17]). Each
`dnv-worker` process mints one random seed per incarnation and, for every
role it carries, keeps its registration (`WorkerReg`, Key table) refreshed
every vote interval and watches the registry of that role; an observer
judges a registration live or dead by its own clock since the put it last
saw, never by the stored epoch, and commits a change into its **effective
membership** only once the change has held for the grace time, so a
flapping worker never becomes effective and never blocks others
(`dnv-worker.md` VW3, VW5). Per role and shard the effective member with
the largest ticket — a hash of its seed, the role and the shard code —
owns the shard: deterministic on every worker, no coordinator, and a
membership change moves only the shards whose winner changed
(`dnv-worker.md` VW9). A worker acts only on the shards it owns, and it
**fences** itself — stops driving everything and rejoins as a fresh
identity — when its own heartbeat stops reaching etcd or stops being
echoed by its watch, or when a peer, or its own observer, has committed it
dead (`dnv-worker.md` VW5, VW8).

### dn / cn roles

For each owned shard a dn-role worker watches the shard's `DnRev` keys
(`DnRevPrefix`), a cn-role worker its `CnRev` keys (`CnRevPrefix`); the key
yields the node's id and the value its revision and `addr_port` ([D10]).
The worker reads the node's desired state — the `DnConf` or `CnConf` keyed
by that `addr_port`, plus the `ClusterConf` — and calls `SyncupDn` or
`SyncupCn` at that `addr_port` with that revision; the sides' and cntlrs'
details are the sp role's to push (`dnv-worker.md` RW13). A put whose only
change is `addr_port` (a moved node, Revision keys and the sync fan-out)
therefore re-syncs the node at its new endpoint without the worker ever
seeing the node disappear; on a delete the worker stops syncing and
health-checking the node (`dnv-worker.md` SW3). The worker also keeps a
`CheckDn` or `CheckCn` stream (Check streams) open to every owned node,
and from its rounds and the syncup replies it maintains `DnConf.err_epoch`
or `CnConf.err_epoch` and the node's capacity key (Capacity index keys) in
STMs that bump no revision: unhealthy sets the epoch and deletes the key,
recovered clears the epoch and recreates the key (`dnv-worker.md` HL1,
MD4). A `revision` mismatch or a non-zero reply code re-issues the
`Syncup*` (`dnv-worker.md` RW4). `RES_STATUS_PROVISIONING` is **not** a bad
status and never sets `err_epoch` or removes a capacity key ([D15],
Live-state reporting).

### sp role

An sp-role worker watches the `SpRev` keys of each owned shard
(`SpRevPrefix`); the key yields `cluster_id` and `sp_id`, the value the
revision and `sp_name` — the `SpConf` key suffix, so this path needs no
`sp_id_to_name` lookup ([D10]). On a bump it loads the SP and fans out
`SyncupSide` to **every side** of the SP and `SyncupCntlr` to **every
cntlr**, with the new revision; each request always carries the full
desired state (Common agent rules), and each side's DN and each cntlr's CN
is resolved by reading the `DnConf` or `CnConf` at the record's endpoint
(`dnv-worker.md` RW14). The fan-out holds requests back in two waits, the
demotion hold and the sides-first hold, and neither is a correctness
dependency ([D16]):

* **Demotion first.** A fan-out that makes a primary a standby — in
  production only a failover does, on either of its triggers — hands that
  cntlr its demotion at once, and holds every side request and every other
  cntlr request until the demoted cntlr has reported the new revision
  applied, or for `DemotionHoldTimeout` at most (`dnv-worker.md` RW22).
  The worker judges no reachability: a reachable old primary that applies
  its demotion within the bound is ANA inaccessible before any side fences
  it, and the bound lets the hosts of an unreachable one drop the path the
  failover withdrew (Failover, [D16]).
* **Sides first.** The cntlrs' requests are then held until the sides the
  worker drives have reported the new revision applied, or for one
  `cntlr_interval` at most, counted from the end of the demotion hold when
  the fan-out had one. The order mitigates three races; `dnv-worker.md`
  RW14 says which sides are waited for, when the hold releases and what it
  costs.

A cntlr replacement is no demotion: it deletes the old cntlr, whose stack
goes with its CN's `SyncupCn` (dn / cn roles), so the sp role has no
request to hand that cntlr first, and a replacement starts no demotion
hold of its own.

Bitmap chunks travel over the dedicated `PushCloneBitmap` and
`PushMigrBitmap` calls instead of the `Syncup*` requests (Bitmap push
protocol): the replies' `bm_info` and `bm_info_list` tell the worker which
chunks each agent already holds, and it pushes the missing parts, one call
in flight per migration or clone, in address order (`dnv-worker.md` BM2,
BM3).

The `Syncup*` replies and, continuously, the `CheckSide` and `CheckCntlr`
streams the sp role keeps open to every side and cntlr of its SPs (Check
streams) feed health: `err_epoch` is set and cleared on the `Cntlr`, `Leg`
and `Side` records accordingly, in STMs that bump no revision — a failing
health block (Group on-leg layout: meta region, data region, health block)
reported by the primary turns into `Leg.err_epoch`; a side's own stream
and rows turn into `Side.err_epoch` (`dnv-worker.md` HL2). When a cntlr's
write moves it into or out of the listing of [D18], the same STM rewrites
the SP's discovery records, still bumping no revision: an enabled standby
leaves them when its `err_epoch` is set and returns when it is cleared,
while an enabled primary stays listed either way.
`RES_STATUS_PROVISIONING` entries never set `err_epoch` on any of the
three ([D15], Live-state reporting), and a `RES_STATUS_PENDING` leg row
neither sets nor clears `Leg.err_epoch` (Live-state reporting).

**Provisioning gate** ([D15], Side provisioning protocol). The sp role
fills `SideConf.provisioned` from the etcd `Side.provisioned`,
`SideConf.zero_bytes` from the side's group and the pool's `block_size`
(Side provisioning protocol), and `MigrSrcConf.dst_provisioned` from the
migration's **destination** side's flag, and flips `Side.provisioned`
true, bumping `SpRev`, when an accepted reply reports the side's zeroing
complete (`dnv-worker.md` RW18). The normal fan-out then re-syncs the
sides, now exporting (`dnagent.md` DN10), and the cntlrs, now connecting
(`cnagent.md` CN10); there are no long gRPC deadlines and no Check-round
exemptions — every RPC stays short.

**Materialization flip.** A reply from **any** cntlr of the SP may complete
a td: thin rows are only ever filled by a cntlr acting as primary, and the
ids live in the shared pool metadata on the DN legs. When an accepted
reply's thin rows for a td cover exactly the SP's slices, every row
`RES_STATUS_OK`, the worker sets `ThinDevice.created` and bumps `SpRev`
(`dnv-worker.md` RW19, MD6); the bump re-fans the SP, which is how
`created` reaches the cn agent (`cnagent.md` CN14). It cannot loop: the
re-sync's reply, reporting the same rows `OK`, finds no candidate. The
flip is idempotent and observation-driven, so a worker restart or a
shard-ownership change (Membership and shard ownership) needs no recovery
step: the new owner's first reply on a fresh Check stream carries the
complete `*Info` and flips whatever is complete and still false.

### Automatic reactions

Agents only report; the worker alone reacts. Every reaction is an ordinary
STM mutation that bumps `SpRev` (`dnv-worker.md` MD6), so the data-plane
choreography is the same as for the equivalent manual RPC. The sp role
evaluates the reactions per SP on its own cadence and applies at most one
per pass (`dnv-worker.md` AR1, AR2); none runs for an SP with `sp_level`
at or above `SP_LEVEL_NO_THINPOOL`, a disabled cntlr is never a candidate,
replaced or repaired, and a `deleting` SP runs a step of its drain instead
(`dnv-worker.md` AR3). A threshold is breached when now minus `err_epoch`
is at least the SP's `event_threshold` field of that kind, a zero field
meaning its default (Common validation; `dnv-worker.md` AR4). A breach
fires a reaction — the failover of an unhealthy primary, a cntlr
replacement, a leg repair on either of its clocks — only on an object
whose latest health verdict by this worker's coordinator of the SP is
unhealthy, and the failover only once the primary's last two health
verdicts from check rounds are both unhealthy, with no clean answer, of a
round or of a syncup, since the earlier of them. A coordinator has no verdict when it starts, after a restart
and after a handoff alike; the clock still runs from the stored
`err_epoch`, and the disabled trigger needs no verdict (`dnv-worker.md`
AR10). An epoch that an earlier owner left, or that another observer
wrote, therefore fires nothing before this worker has judged the object
itself. The four reactions:

* **Failover** (`dnv-worker.md` AR5): an unhealthy primary — or a
  `disabled` one, with no threshold wait (Cntlrs) — gives the role to a
  healthy, enabled cntlr; this *is* the failover trigger of Failover.
* **Thin-pool auto-grow** (`dnv-worker.md` AR6): a slice pool whose data or
  metadata usage passes `DmPoolConf.low_water_mark_pct`, which is not an
  `EventThreshold` field, is grown by an internal `GrowSlice` (GrowSlice);
  a grow whose new group still holds a provisioning leg completes by itself
  when the leg clears (`cnagent.md` CN13, [D15]). Auto-grow is best-effort,
  so a pool can fill (Known limits).
* **Cntlr replacement** (`dnv-worker.md` AR7): a cntlr that stays unhealthy
  — a non-primary one, or the primary of an SP with no failover candidate
  — is replaced on a fresh CN by an internal `ReplaceCntlr`, one
  transaction doing what `DeleteCntlr` and `CreateCntlr` do without
  `DeleteCntlr`'s not-primary and disabled preconditions, the rewrite of
  the discovery records by [D18] included (`dnv-worker.md` MD6). It
  deletes the old cntlr rather than demoting it, so it starts no demotion
  hold of its own (sp role).
* **Leg repair** (`dnv-worker.md` AR8): a leg unhealthy for `leg_unhealthy`,
  or unhealthy while its side has been unhealthy for `side_unhealthy`, is
  switched out for a ready spare by an internal `SwitchSpareLeg`; when the
  group has no spare on its way and room for one, an internal
  `CreateSpareLeg` makes one first (Spare legs). There is no migration
  reaction (`dnv-worker.md`, Automatic reactions — `worker/reaction.go`).

The worker never deletes user data on its own (`dnv-worker.md` AR9).

## Procedures

### Failover

An SP has several cntlrs; exactly one is primary. A failover switches the
primary from one cntlr (**old_primary**) to another (**new_primary**) and
involves three kinds of participants: the two cntlrs and the **sides**. The
trigger is only ever an etcd change (Automatic reactions, or
`UpdateCntlrEnabled`), delivered by revision-ordered syncups in the two
waits of sp role's hold: the demotion of **old_primary** first; the **sides** once
old_primary has reported its demotion applied or `DemotionHoldTimeout` has
passed (`dnv-worker.md` RW22); and **new_primary** once the sides have
reported the revision applied or one `cntlr_interval` has passed
(`dnv-worker.md` RW14). The two waits narrow what hosts see, and safety rests
on the sides' flips and md's arbitration below, not on them ([D16]).

**old_primary** — on a `SyncupCntlr` saying it is not primary (a revision
higher than everything it has seen):

1. Move all namespaces from the optimized ANA group to the inaccessible
   one, first, so the host is told to stop using the path before the path
   stops working (`cnagent.md` CN9).
2. Reload every namespace's `CnNsDevName` dm-linear onto its td's
   `CnErrorName`; the reload's own suspend flushes the in-flight IO first
   (`cnagent.md` CN16).
3. Clean up every resource a primary should not have (raid0s, thin
   volumes, pools, pool-meta and pool-data linears, md arrays), leaving the
   standby set of Standby cntlr.

**new_primary** — on a `SyncupCntlr` saying it is primary (the highest
revision):

1. Make sure all groups are available ("Make sure all groups are
   available"). A group whose member is not yet available is retried by
   the agent until it is (`cnagent.md` CN10); the worker is not involved.
   The fan-out's two waits are bounded in time ([D16]), so this `SyncupCntlr`
   can land before the sides' ANA flips have reached the new primary, and
   nothing re-sends it on that account; the sides-first hold (sp role,
   `dnv-worker.md` RW14) makes that rarer, not impossible.
2. Create the pools, thin volumes and raid0 devices (Primary cntlr, steps
   3 to 5).
3. Reload every namespace's `CnNsDevName` from its td's `CnErrorName` onto
   the td's raid0 — or onto the dm-clone, for an in-flight clone, after
   the clone crash recovery if the clone state was lost (Clone crash
   recovery).
4. Move all namespaces from inaccessible to optimized. Step 4 does not
   wait for steps 1 to 3 to succeed: a converge in which a member not yet
   available (step 1) kept the stack from being built moves the namespaces
   to optimized all the same, over the td's `CnErrorName` (the ANA rule of
   `cnagent.md` CN16 reads the plan, not the stack), and host IO on that
   path fails — target-internal (DNR), not a path error — until the
   agent's retry has built the stack (`cnagent.md`, Known limits).

The failover's own transaction marks the new primary settling; until it
has reported its stack built and clean as primary, a failover of it for
being unhealthy waits `cntlr_unhealthy` instead of `primary_unhealthy`
when that is the longer, unless it is disabled (`dnv-worker.md` HL2, AR5).
The same transaction rewrites the SP's discovery records by [D18]: an old
primary failed over for being unhealthy leaves them there, before either
wait of the fan-out has begun, and one failed over for being disabled
left them with its disable. Either returns to them as a standby once
enabled and clean again; the new primary was listed already, as every
candidate is.

**sides** — on a `SyncupSide` (the highest revision) showing a changed
primary. One converge pass, with no waits and no suspensions ([D12]) as
far as its reloads succeed, in this order:

1. Reload the new primary CN's dm-linear so that it sits on the side
   device.
2. Reload the old primary CN's dm-linear onto its dm-error device.
3. Move the new primary CN's namespace from non-optimized to optimized.
4. Move the old primary CN's namespace from optimized to non-optimized.

The agent does all four in a single `SyncupSide` converge — the two
reloads before the two ANA writes, and within each pair the new primary's
CN first, the order in which the dn agent walks the request's CNs — so for
the moment between its reload and its ANA move the old primary's path
fails IO (`dnagent.md` DN10). A reload fails closed (`dnagent.md`, OS
wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`): one whose load fails
leaves the device suspended on its old table, queueing its IO; what that
costs when it is the reload of step 2 is `dnagent.md`, Known limits.

**Why the fan-out needs no order to be write-safe ([D16]).** The sp worker
pushes the failover's `SyncupSide` calls with no order among the sides,
and each wait of its fan-out ends at its bound whether the reports it
waits for came or not, so mid-failover the old primary can still hold a
live path to one leg while the new primary already owns another. Two
mechanisms make that harmless, and both are load-bearing: (a) each side's
flip is **atomic within one DN converge** — the old primary's dm-linear
reloads onto dm-error in the same pass that puts the new primary's onto
the side device — so a single leg never has two writers, short of a reload
whose load fails (the known limit above); and (b) across the legs of a
group, **md's own arbitration** decides: a leg whose superblock still
claims a clean full array is not started degraded on its own ("Make sure
all groups are available"), and once both legs are reachable the event
counts pick the newer one and resync overwrites the stale leg. dnv adds no
fencing epoch of its own, so (b) is an explicit dependency on mdadm's
behaviour, and no suite in the tree pins that behaviour (Known limits).

**What hosts see at a failover ([D16]).** The common failover — a dead CN
— is clean from the host's side once the new primary's stack is built:
its paths drop, IO queues, and the new primary's optimized flip releases
it. A promotion that outruns the sides' ANA flips (new_primary step 1) can
make that flip over dm-error, and the released IO then fails until the
agent's retry builds the stack (new_primary step 4). In two cases an old
primary that is still running stops taking host IO before the sides fence
it, and the worker need not know which case it is in:

* An old primary that applies its demotion inside the demotion hold (sp
  role) moves its namespaces to ANA inaccessible (**old_primary** step 1)
  before any side fences it, so hosts queue their IO for the new primary's
  optimized flip — a pause, not errors.
* An old primary that keeps running while only its **control-plane**
  connectivity is lost never answers, so the demotion hold runs to its
  bound. Its
  address is out of the discovery records by then — taken out by the
  failover's own transaction, or by the disable that triggered the
  failover ([D18]) — and dnv-cdc has told the hosts; a host that follows
  the discovery log (Host view) drops the path inside the demotion hold,
  before the fence, and its IO then queues as for a dead CN.

What is left is an old primary still on a host's path at the fence that
has not applied its demotion: a path nothing following the discovery log
manages, a discovery view a cdc instance keeps stale, a demotion its agent
cannot apply within the demotion hold, or a failover fanned out with no
demotion hold by a coordinator that has just started, after a restart or a
handoff, as when the failover's owner lost the shard before its demotion
hold ended (Known limits). Its md arrays fail once the sides have fenced its
legs ([D16], whose write-safety the paragraph above explains), and the
errors nvmet returns on the still-optimized path are target-internal
(DNR), not path errors — host multipath does **not** retry them on the new
primary's path, so applications can see IO errors until the old primary
applies its demotion or is stopped. [D16] records why that residue is
accepted.

#### "Make sure all groups are available"

The primary builds each `RedundMdRaid1` group from the legs that are
available to it: it creates the array only when every leg of the group is
available and none carries an md superblock — a leg without one can only
be a freshly provisioned side, whose meta region Side provisioning
protocol has zeroed, the superblock check being the evidence the CN
actually has ([D15]) — and otherwise assembles the array from the legs
that carry a superblock, adding an available leg the array left out once
the array runs. The assemble leaves the degraded start to mdadm: a lone
survivor whose superblock still counts a clean full array is refused, and
the group is then not available this pass. That refusal is the cross-leg
half of failover safety ([D16]) — a stale leg cannot be started alone,
and once both legs are back the event counts pick the newer one. The
cases, the guards around the create, the sysfs read of the members and
what makes a leg available are `cnagent.md` CN12. The health block (Group
on-leg layout: meta region, data region, health block) is the ongoing
liveness probe on top of this: an available leg whose probe IO fails is
reported unhealthy and feeds the automatic reactions (Automatic
reactions). Spare legs never participate in assembly (Spare legs).

### Migration

```mermaid
flowchart BT
    subgraph cn0["Controller Node 0 — primary cntlr"]
        W0["leg wrapper (dm-linear)<br/>over one multipath ns<br/>(src + dst = 2 paths)"]
    end
    subgraph cn1["Controller Node 1 — standby cntlr"]
        W1["leg wrapper (dm-linear)<br/>over one multipath ns<br/>(src + dst = 2 paths)"]
    end
    subgraph dn0["Disk Node 0 (source)"]
        subgraph s0["Side 0 — src side"]
            LV0["side device"]
            MS["dm-linear migr-src<br/>DnMigrSrcName"]
            NM["nvmet subsystem<br/>MigrSrcNqn"]
            E00["dm-error"]
            L00["dm-linear (cn0)"]
            N00["nvmet for cn0"]
            E01["dm-error"]
            L01["dm-linear (cn1)"]
            N01["nvmet for cn1"]
            LV0 --> MS
            MS --> NM
            E00 --> L00
            L00 --> N00
            E01 --> L01
            L01 --> N01
        end
    end
    subgraph dn1["Disk Node 1 (destination)"]
        subgraph s1["Side 1 — dst side"]
            ND["nvme host device<br/>(connected to MigrSrcNqn)"]
            MM["dm-clone metadata wrapper<br/>DnMigrMetaDmName ([D13] slot)"]
            DC["dm-clone<br/>DnMigrFinalName"]
            LV1["side device"]
            L10["dm-linear (cn0)"]
            N10["nvmet for cn0"]
            E11["dm-error"]
            L11["dm-linear (cn1)"]
            N11["nvmet for cn1"]
            ND -->|"source"| DC
            LV1 -->|"dest"| DC
            MM --> DC
            DC --> L10
            L10 --> N10
            E11 --> L11
            L11 --> N11
        end
    end
    NM ==>|"nvme-of pull (hostnqn DnHostNqn)"| ND
    N10 ==>|"ANA optimized"| W0
    N11 -->|"ANA non-optimized"| W1
    N00 -.->|"inaccessible"| W0
    N01 -.->|"inaccessible"| W1
```

*Fig. `080Migration` — mid-migration: the leg temporarily owns two sides.
Both export the same `SideToCnNqn`, so each CN sees one multipath namespace
with two paths; ANA moved IO onto the destination path, whose dm-clone
pulls from the source. The leg wrappers are untouched by the switch.*

Starting a migration resembles a failover; the leg temporarily owns two
sides.

**Phase 0 — the destination provisions first.** `CreateMigration` writes
the destination `Side` with `provisioned` false (Migrations), so the
destination DN runs only the Side provisioning protocol: allocate the runs,
build `DnSideName`, zero the prefix its group needs batch by batch, as for
any side of the group. No per-CN stacks, no metadata slot, no
`nvme connect`, no dm-clone; what the destination's `migr_dst_info` rows
report meanwhile, and at which levels, is `dnagent.md` DN13.

For the **src** side, `migr_src_conf.dst_provisioned` false is normative
and means: **behave exactly as if `migr_src_conf` were absent** — keep
serving normally, no fence, no suspension, no migration-source export —
differing only in reporting the would-be `migr_src_info` rows as
`RES_STATUS_PROVISIONING` (`dnagent.md` DN12). Without this gate the
source would fence the primary's path at migration start and the leg would
have **no serving path for the whole zeroing window**.

When the sp worker flips the destination side (sp role), the next fan-out
carries `side_conf.provisioned` true on the destination and
`migr_src_conf.dst_provisioned` true on the source, and both roles run the
sequences below unchanged; the destination's connect retry of step 3
absorbs any cross-side ordering. The cost is added **migration-start
latency** — the destination's zeroing pass (Side provisioning protocol),
before any data moves.
`CancelMigration` during the zeroing window cancels the zeroing goroutine
and waits for it before the dm devices are removed (Side provisioning
protocol).

**src side** (driven by its `SyncupSide` carrying `migr_src_conf`:
`migr_id`, `dst_side_id`, `dst_dn_id`, `dst_provisioned` true):

1. Move every per-CN subsystem's namespace to inaccessible (all cntlrs).
2. Retire every per-CN dm-linear, in two phases ([D12]):
   a. **Suspend** it where it is, and hold it suspended for at least
      `SuspendSeconds`, the cutover grace window. Its namespace is already
      inaccessible from step 1, so the only IO this absorbs is what the
      old primary still had in flight.
   b. At the end of the window, **reload** it onto its dm-error device.
      Device-mapper releases a suspended device's deferred bios against
      whatever table is live at resume, and the reload installs dm-error
      *before* resuming, so the absorbed IO is failed here rather than
      replayed onto the side's data — which is what would otherwise let
      the source silently diverge from the destination after hydration
      had already copied the region.

   The window is a floor, not a schedule, and a bound as far as the
   reloads succeed: a device is never left suspended beyond it, including
   across an agent restart that finds it suspended, because a suspended dm
   target queues IO forever and wedges any block-device scanner that
   touches it. `dnagent.md` DN12 states the rules that keep the bound —
   how a restart, a teardown or export removal inside the window and a
   level with no export layer each end it, and that only a request ending
   the source role resumes a linear onto its pre-fence table — and
   `dnagent.md`, Known limits, where it does not hold.
3. Build `DnMigrSrcName` (a linear on the side device) and export it
   through `MigrSrcNqn`, its `allowed_hosts` the `DnHostNqn` of the cluster
   and `migr_src_conf.dst_dn_id` alone.

**dst side** (its `SyncupSide` carries `migr_dst_conf`: `migr_id`,
`src_side_id`, `src_dn_id`, `src_nvme_tr_conf`, `block_size`,
`meta_blocks`, `dm_clone_conf`, `bm_cnt`). The steps are logical; the
agent's converge builds bottom-up and reaches the same end state
(`dnagent.md` DN13):

1. Create the per-CN subsystems and namespaces with dm-error backing, all
   inaccessible.
2. Allocate the dm-clone metadata slot in the [D13] clone-metadata area
   and build its wrapper dm-linear `DnMigrMetaDmName`.
3. `nvme connect` to the `MigrSrcNqn` of the cluster,
   `migr_dst_conf.src_dn_id`, the sp and the `migr_id` at
   `migr_dst_conf.src_nvme_tr_conf`, with the host NQN `DnHostNqn` the
   source admits, retrying until success (`dnagent.md` DN13).
4. Create the dm-clone `DnMigrFinalName`: metadata the step-2 wrapper,
   destination the local side device `DnSideName`, source the connected
   nvme device, region size `migr_dst_conf.block_size` (the SP's
   `block_size`), hydration knobs from `migr_dst_conf.dm_clone_conf`.
5. Reload the **primary** CN's dm-linear onto the dm-clone; set that
   subsystem optimized and every other CN's non-optimized. The destination
   exports the same NQN as the source side, so to each CN it is a second
   path of the leg's existing multipath namespace (Primary cntlr): the CN
   connects to it as a newly provisioned side in the same fan-out
   (`cnagent.md` CN10), and the ANA change alone moves IO from the source
   path to the destination path once that path is up — no dm reload
   happens on the CN side.

IO then flows host → primary cntlr → destination side, whose dm-clone pulls
missing regions from the source on demand and hydrates in the background.
The dm-clone metadata lives on disk, in the [D13] clone-metadata area, so a
DN reboot resumes hydration where it left off — no special recovery is
needed, unlike clones (Clone crash recovery). The optional bitmap
fast-path: `GetLegBitmap` (paged) → `AppendMigrationBitmap` → the worker's
`PushMigrBitmap` (Bitmap push protocol) → the destination agent persists
each chunk at `LocalMigrBmPath` and `blkdiscard`s never-written regions so
that they are never copied (Migrations, Bitmap reads, raid0 bitmap math).
A skipped region keeps what the destination's recycled extents held, which
the pool never reads, since no pool block maps there ([D15]) — provided
the bitmap is read late enough. **A caller reads the leg bitmap only once
the primary reaches the leg through the destination**: the primary's path
to the destination is live and optimized, and the destination makes that
path optimized only over its live dm-clone (dst steps 4 and 5). The source
takes no more writes by then (src steps 1 and 2), and every block the pool
maps afterwards is written whole through the dm-clone ([D15]), so no skip
can lose it. A data leg's read also needs a usable path to the leg, and
from src step 1 on that path is the destination's: the primary's cn
agent reads the bitmap from a thin metadata snapshot of the slice's pool
(`cnagent.md` CN25, CN27), the snapshot's reservation commits the pool,
and dm-thin flushes the pool's data device, this leg included, before the
commit. Known limits says what a read before that point does, what else
the migration gap blocks on the primary, and why a read after that point
can still block.
`FinishMigration` and `CancelMigration` are specified under Migrations.

### Transfer + clone = cross-SP live migration

A clone's source can be *any* NVMe-oF namespace; when the source is a dnv
transfer, the pair performs live migration of a volume between SPs, even
across clusters (figs `090Clone` and `100Transfer`, Clones and Transfers).
Setup for moving a subsystem and namespace from sp1 to sp2:

* sp2 gets an identical subsystem (the same NQN) and namespace (the same
  `ns_idx`, and the same nguid and uuid, passed to `CreateNamespace`,
  Subsystems, namespaces) on a **fresh, empty** thin device [D3], so hosts
  aggregate sp1's and sp2's exports into one multipath device.
* The cntlrs of sp1 and sp2 must use disjoint `cntlid_slot`s: two SPs
  of `MaxCntlrCntPerSp` cntlrs each fill exactly the `CnCntlidSlotCnt`
  slots (cntlid slots). If they do not, rotate a cntlr:
  `UpdateCntlrEnabled` to disabled, `DeleteCntlr`, then `CreateCntlr` with
  a free slot. Likewise the two SPs' cntlrs must not share CNs; rotate the
  same way if they do.
* Ensure `Namespace.suspended` false on sp1 and true on sp2 (create the
  sp2 namespace with `CreateNamespace` and `suspended` true, or
  `UpdateNamespaceSuspended` on an existing one).
* Collect the sp2 cntlrs' CN ids into a `CnHostNqn` list.
* `CreateTransfer` on sp1 (`ori_nqn` and `ori_ns_idx`, `allowed_hosts`
  that list, `auto_suspend` true). The sp1 primary then (1) moves the
  origin namespace to inaccessible, (2) parks its `CnNsDevName`, (3)
  creates `CnXferFinalName` on the raid0, and (4) exports it through
  `XferNqn` per the `Transfer`.
* `CreateClone` on sp2 (`src_tr_conf` the sp1 cntlr endpoints,
  `src_nqn` the transfer's `XferNqn`, `src_ns_idx` the `ori_ns_idx`, the
  source geometry `src_slice_cnt`, `src_stripe_size` and `src_block_size`
  the slice count, raid0 stripe and pool block size of sp1,
  `dst_td_name`, `auto_resume` true). The sp2 primary then builds the
  clone (`cnagent.md` CN18): connected to the targets, its metadata
  wrapper `CnCloneMetaDmName` ([D14]) under the dm-clone
  `CnCloneFinalName`, the sp2 namespace's `CnNsDevName` on that dm-clone
  once hydration is enabled — the parked ns-dev is live and nothing is
  dm-suspended, so there is no resume step (Namespace suspend semantics)
  — and that namespace optimized. Hosts flip to sp2 transparently.
* Optional bitmap fast-path: per sp1 slice, `GetThinDeviceBitmap`
  (paged) → `AppendCloneBitmap` with that slice as `src_slice_idx` on sp2
  → the worker's `PushCloneBitmap` (Bitmap push protocol) → the sp2
  primary persists each chunk at `LocalCloneBmPath` and `blkdiscard`s
  (raid0 bitmap math). This is safe while IO runs (Clone crash recovery).
* When hydration completes: `DeleteTransfer` with `force` false on sp1
  (it retires the source: `suspended` true) and `DeleteClone` on sp2
  (`suspended` false, the namespace now backed by the local raid0). To
  abort instead: **first retire the sp2 namespace** with
  `DeleteNamespace` — not merely suspend it, because `DeleteClone`
  unconditionally sets `suspended` false on the destination td's
  namespaces (Clones), which would resume it optimized over the partial
  copy and hand the host a second, divergent path; then `DeleteClone` with
  `force` true on sp2 and `DeleteTransfer` with `force` true on sp1.
  sp1 resumes serving; the sp2 td and its partial bytes remain until
  deleted, and `DeleteClone` only latches (Clones), so deleting that td
  means polling `GetClone` to `NOT_FOUND` first: the destination-td guard
  walks `clone_name_list`, which the drain shrinks in its last transaction
  (`dnv-worker.md` CLD9). **Aborting after the cutover discards every
  write served through sp2**: with `auto_resume` the sp2 namespace
  became the serving path at `CreateClone`, its writes live only on the
  abandoned sp2 td, and sp1 resumes from its retained copy, which
  stopped receiving writes at `CreateTransfer` with `auto_suspend` — so an
  abort is lossless only while nothing has written through sp2.

### raid0 bitmap math

Definitions for one raid0 device: `slice_cnt` underlying devices, a chunk
size of `stripe_size`, and per-underlying-device write bitmaps whose bit
granularity is `block_size`. Constraints, which `CreateClone` validates
(Clones) and which the geometry of every SP created under the geometry
rules of Common validation meets too (Storage pools), so that any such SP
can be a clone source: `slice_cnt` from one to `MaxSliceCntPerSp`;
`stripe_size` a whole multiple of `MinDmRaid0StripeSize`, at most
`MaxDmRaid0StripeSize`; `block_size` a whole multiple of
`MinDmPoolDataBlockSize`, at most `MaxDmPoolDataBlockSize`; and
`block_size` a whole multiple of `stripe_size`. The address mapping of a
logical byte offset *off*:

```
chunk          c = off div stripe_size
slice            = c mod slice_cnt
slice offset     = (c div slice_cnt) × stripe_size + (off mod stripe_size)
```

**Skip-bitmap function.** Given a source geometry A (its `slice_cnt`,
`stripe_size`, `block_size` and per-slice bitmaps, *written = 1*) and a
destination geometry B (the same, with bitmaps *copied = 1*, all zero on a
first run), with the extra constraint that the larger of the two stripe
sizes is a whole multiple of the smaller: the region size is the smaller of
the two stripe sizes — so any region is contiguous inside one chunk on
**both** sides — and the region count is the smaller of the two device
sizes divided by the region size. The output is a bitmap of one bit per
region, where bit *r* is 1 iff region *r* need **not** be copied, iff
not *writtenA(r)* or *copiedB(r)*: *writtenA(r)* ORs and *copiedB(r)* ANDs
the covered source and destination bitmap bits after mapping region *r*'s
byte range through each side's address mapping. The function is the
definition; its two uses in the tree specialize it:

* The **agents** applying a source's `CloneBitmap`s and `MigrBitmap`s
  (Bitmap push protocol): the same math with no B term (dm-clone tracks its
  own hydration), the region size being the dm-clone region — the
  destination `block_size`, which may exceed A's stripe size; a region then
  spans several source slices and *writtenA* simply ORs across all of them.
  They `blkdiscard` region *r* iff not *writtenA(r)*. For a migration they
  first shift by the leg's `meta_blocks` (Migrations). The agent carries
  this out as `dnagent.md` SH22 and `cnagent.md` CN22 say.
* Clone **crash recovery** (Clone crash recovery): the B side only — it
  `blkdiscard`s region *r* iff *copiedB(r)*.

**Wire and local conventions.** Every bitmap of the `Gateway` service and of
etcd — `GetThinDeviceBitmap`, `GetLegBitmap`, `AppendCloneBitmap`,
`AppendMigrationBitmap`, the stored `CloneBitmap` and `MigrBitmap` — is
**1 = unwritten, skippable**; thin-pool metadata and the formulas above
use **written or copied = 1**. A bitmap is inverted exactly once, at the
boundary. Bit addressing is **LSB-first within each byte** in **both**
conventions — bit *i* is bit *i* mod 8 of byte *i* div 8 — and the trailing
pad bits of the last byte are zero (Bitmap reads). Only the **meaning** of a
set bit is inverted at the boundary, never the bit order.

### Clone crash recovery

Clone progress durability does **not** rely on the source bitmaps in etcd;
it relies on the **destination thin-pool bitmaps**. The destination td
starts empty ([D3]) and dm-clone hydrates in regions of one `block_size`,
so after any crash "block mapped in a destination thin pool" is "block
already copied". The volatile clone metadata — arena units under the
wrapper `CnCloneMetaDmName` on the CN's tmpfs and loop device (Controller
node, common) — is therefore rebuildable: whenever the primary (re)builds a
clone whose dm-clone metadata is missing or unusable, whose dm-clone is
absent, or whose dm-clone does not yet show hydration enabled, it parks the
td's namespaces off the clone, creates the dm-clone with hydration disabled
over a usable metadata wrapper, reads the destination td's mapping bitmap
from every thin pool of the SP (per slice, *mapped = 1 = copied*) and
`blkdiscard`s every dm-clone region whose bits are 1 there (raid0 bitmap
math, the B side only), marking those regions "already hydrated", and only
then enables hydration and puts the namespaces' ns-devs onto the dm-clone.
The triggers, the order of those steps and what each step keeps or removes
are `cnagent.md` CN18, and the agent puts a td's ns-devs onto its dm-clone
only while the dm-clone's status shows hydration enabled (`cnagent.md`
CN16).

The destination bitmaps must be fully applied **before** the dm-clone
handles any IO — otherwise a read of an already-copied, and possibly
since-rewritten, region would be fetched from the source again, returning
stale data over the newer local bytes. The **source** bitmaps (Clones)
carry no such hazard — they only mark regions the source never wrote — so
they may keep arriving through `PushCloneBitmap` after IO has started; any
source chunks already persisted at `LocalCloneBmPath` are simply re-applied
by the agent once the dm-clone is rebuilt (Bitmap push protocol), with no
worker involvement.

### Namespace suspend semantics

`suspended` true makes the namespace **effectively suspended** on every
cntlr; `cnagent.md` CN16 defines effective suspend, with its second source
and the one override. An effectively suspended namespace's `CnNsDevName`
is kept **parked** — live, its table a dm-linear over the td's dm-error —
with the namespace in the inaccessible ANA group; a namespace that is not
effectively suspended gets the normal behavior of Primary cntlr and
Standby cntlr. Users set the flag (`CreateNamespace`'s `suspended`,
`UpdateNamespaceSuspended`), and so do the transfer and clone
finalizations (Transfers, Clones). Nothing is dm-suspended, short of a park
whose load fails and leaves the ns-dev suspended on its old table
(`cnagent.md` CN16): hosts queue against the ANA state, which nvmet
enforces at the target, and a local opener of a parked ns-dev gets EIO
rather than blocking ([D12]).

*Parked*, of an ns-dev, is this device state and nothing else. It is
unrelated to a **parked spare leg** (`dnv-worker.md` AR8), which is an etcd
record state — a replaced leg retained in `spare_leg_list` for the
operator — with no dm meaning.

### SpLevel

Levels gate agent behavior top-down for disaster recovery: each step
removes one more fragile layer until `SP_LEVEL_DISABLE` leaves only the
bottom storage layer — on a DN, each side's data device `DnSideName` and
its [D13] extent record. The ladder, each level including the restrictions
of the ones before it: `SP_LEVEL_READONLY` fails user writes;
`SP_LEVEL_NO_CLONE` builds no clone dm-clone; `SP_LEVEL_NO_THINPOOL` no
thin pool; `SP_LEVEL_NO_REDUND` no group device; `SP_LEVEL_NO_MIGRATION` no
migration dm-clone; `SP_LEVEL_NO_SIDE` exports no side; `SP_LEVEL_DISABLE`
keeps only that bottom layer. Side zeroing (Side provisioning protocol) is
part of that bottom layer and keeps running at `SP_LEVEL_DISABLE`. Agents
treat the level as part of the desired state (it rides in every
`SyncupSide` and `SyncupCntlr`): raising it tears layers down, lowering it
rebuilds them.
`UpdateStoragePoolLevel` sets it (Storage pools); `dnagent.md` DN11 and
`cnagent.md` CN19 give each role's behavior per level.

`SP_LEVEL_READONLY` means exactly one thing: **every user-facing namespace
of Subsystems, namespaces — the ones backed by `CnNsDevName` — serves reads
and fails writes with an IO error.** Nothing else. It is enforced **on the
CN only**, by reloading each such namespace's `CnNsDevName` onto a dm-flakey
"error_writes" table over its normal backing (`FlakeyErrorWritesTable`);
never by a block-device read-only flag, and never on the DN ([D11]). An
effectively suspended namespace is exempt at every level: it is parked on
the td's dm-error and inaccessible (Namespace suspend semantics), so it
serves no reads to keep and no writes to fail — the CN16 parked rule is
evaluated above the dm-flakey one. Two kernel facts rule out a bdev flag at
any layer: nvmet opens a namespace's backing device for both read and write
("BLK_OPEN_READ | BLK_OPEN_WRITE"), so the top device of an export can
never be read-only; and a read-only flag **below** the top does not stop
writes that device-mapper remaps onto it, since "bio_check_ro()" runs at
top-level bio submission only. The DN, moreover, cannot fail writes at all
— md superblock and bitmap writes, resync, failover assembly and the
health-block writes of Group on-leg layout: meta region, data region,
health block must keep flowing at these levels — so the DN's behavior below
`SP_LEVEL_NO_MIGRATION` is identical to `SP_LEVEL_READWRITE`. Clone and
migration hydration is likewise **not** paused: hydration is infrastructure
IO, not user IO. A CN that has not yet converged to the level keeps serving
writes until it syncs (Known limits).

### cntlid slots

NVMe multipath requires distinct CNTLIDs among the controllers a host
aggregates. dnv partitions the cntlid space through each nvmet subsystem's
"attr_cntlid_min" and "attr_cntlid_max" into slots, the same partition on
CNs and DNs: `CnCntlidSlotCnt` slots from `CnCntlidSlotBase` in steps of
`CnCntlidSlotStep`, with their DN twins `DnCntlidSlotCnt`,
`DnCntlidSlotBase` and `DnCntlidSlotStep`. Slot *s* starts at the base plus
*s* steps and ends one CNTLID below the next slot's start; nvmet's range
includes both ends, so a slot holds exactly one step of CNTLIDs and no two
slots share one. All slots come from `SpConf.cntlid_slot_list`. Rules:

* Every **cntlr** of an SP uses a slot distinct from every other cntlr of
  the same SP (host-facing multipath aggregates them).
* A **side** uses one slot for all its per-CN exports. Sides of
  **different legs** may share slots freely — their subsystem NQNs differ,
  so no CN-side aggregation occurs. The sides of **one** leg are aggregated
  by the CN (they share an NQN; see NQNs), so while a leg is being migrated the
  source side and the destination side must use different slots —
  otherwise the two controllers the CN merges could pick the same CNTLID
  and the kernel would refuse the second path.
* Two SPs joined by transfer and clone must use disjoint slot sets across
  their cntlrs (Transfer + clone = cross-SP live migration) — their
  host-facing subsystems share NQNs.

An agent can find a subsystem it wants already live under another slot's
range, built by the cntlr that a rotation replaced on the same CN (Transfer
+ clone = cross-SP live migration); it converges that subsystem under its
own cntlr's slot. nvmet refuses an "attr_cntlid_min" above the
current "attr_cntlid_max" and an "attr_cntlid_max" below the current
"attr_cntlid_min", so no single write order converges every move: the
agent writes "attr_cntlid_max" first when the live one reads below the new
"attr_cntlid_min", and "attr_cntlid_min" first otherwise
(`EnsureSubsystem`).

## dnv-cdc

`dnv-cdc` serves the NVMe-oF Central Discovery Controller, the well-known
discovery subsystem `NvmeDiscoveryNqn`, to hosts over NVMe/TCP. `cdc.md`
owns it: the discovery service model (DS1 to DS11), the etcd watcher (WV1
to WV6) and the NVMe/TCP discovery service (NP1 to NP14). An instance
serves the ranges it is configured with — a range is the shard codes that
share one first hex digit — by watching the cdc prefix and filtering on
each key's shard code (`cdc.md` DS2). For each `CdcEntry` it owns — the
gateway writes them as host-facing subsystems and cntlrs change, and the
sp worker as a cntlr's health changes and at a failover or a cntlr
replacement, every writer by the listing rule of [D18] (Subsystems,
namespaces; Cntlrs; sp role) — it renders the discovery log records
(`cdc.md` DS3), shows each host exactly the entries whose `allowed_hosts`
name it (`cdc.md` DS4), and sends a discovery-log-change AEN to the hosts
whose rendered log changes (`cdc.md` DS6, DS7). Hosts that follow the
discovery log, through nvme-stas or an equivalent (Host view), then
connect and disconnect automatically, which is what makes
`DeleteSubsystem`, `CreateCntlr` and `UpdateCntlrEnabled` transparent to
hosts, and what takes a failed-over primary's path away from them, before
the sides fence it when it does not answer its demotion within the
demotion hold (Failover).

Redundancy is twins, not fail-over: the instances configured for one range
all serve it, and hosts hold discovery connections to every cdc endpoint
(`cdc.md` DS2; the placement is `cdc.md`, `cmd/dnv-cdc`). An instance
answers no host until its first scan of etcd has landed, and from then on
keeps serving its last known state through etcd outages (`cdc.md` CM4,
DS10). A twin that has lost etcd therefore keeps listing a path the
records have withdrawn, and nvme-stas keeps a path for as long as any
discovery controller it reads lists it (Known limits).

## Components: invocation reference

Each binary is a thin `main` over its library package (`layout.md`,
`cmd/` wiring) and binds flags, an optional config file and the
environment through viper, so every daemon flag is also a config key and
an environment variable. The flags, their defaults and the environment
prefixes are the code's and each binary's `--help`'s; the component
documents state the ones their rules depend on. The three control-plane
processes run co-located on any number of control-plane servers (fig.
`070Cluster`).

* `dnv-gateway` (`gateway.md`) — the stateless server of the `Gateway`
  gRPC service: it listens for users and CLIs, reads and writes etcd, and
  dials the agents only for the calls System overview lists.
* `dnv-worker` (`dnv-worker.md`) — the vote, shard and revision workers of
  the roles it runs, any subset of `dn`, `cn` and `sp` (`dnv-worker.md`
  CM1): it reads and writes etcd and dials the agents.
* `dnv-agent dn` and `dnv-agent cn` (`dnagent.md`, `cnagent.md`) — one
  cobra root command with exactly those two subcommands (`dnagent.md` CM1);
  the dn agent runs on every DN and the cn agent on every CN. An agent
  serves its gRPC service to the workers and the gateway, converges one
  nvmet port on the node's kernel from its transport configuration — which
  is mirrored into `DnConf` or `CnConf` when the node is created, so that a
  CN dials a DN's sides and a host a CN's namespaces there — and keeps its
  local store (Agent local-store paths). It never talks to etcd. The dn
  agent also takes the raw disk it formats ([D13]); the cn agent takes the
  capacity budget `GetCnSize` replies, the per-node input of the CN extent
  count (Size → extents; `cnagent.md`, `cmd/dnv-agent cn`). A node may run
  a dn agent and a cn agent side by side: the pair co-owns one nvmet port
  at the default port id, with identical transport values, and both use the
  default local-store prefix, since the two roles' store kinds are disjoint
  (Disk node; Controller node, common; `dnagent.md` CM2). Several agents on
  one kernel need more than distinct port ids (`dnagent.md` CM2), and two
  cn agents on one kernel are never safe (Controller node, common).
* `dnv-cdc` (`cdc.md`) — the discovery controller (dnv-cdc): it reads etcd
  and listens for hosts on its NVMe/TCP endpoint; its placement is
  `cdc.md`, `cmd/dnv-cdc`.
* `dnvctl` (`dnvctl.md`) — the operator CLI: one command per
  `service Gateway` RPC, sent to one gateway (`dnvctl.md` CT1).

**etcd.** etcd holds all desired state. It is deployed with `--max-txn-ops`
at `EtcdMaxTxnOps` or higher, a server flag dnv cannot set from the client
side; `gateway.md`, Constants this document owns, states the requirement
and the transactions that size the number.

**md assembly.** Only the agent assembles dnv arrays, so every node that
runs a dnv agent masks udev's incremental assembly of them — a DN too, the
superblocks of a CN's legs landing on its storage through the side export: a
udev rule ordered before the stock "64-md-raid-assembly.rules" imports the
md environment itself — the array name does not exist before the stock rule
runs its own import, so a bare name match in an earlier rule never fires —
and sets "SYSTEMD_READY" to zero for a device whose array name carries the
fixed dnv prefix of `CnMdArrayName` (md names), in the bare form and in the
"homehost:name" form; the cn agent creates its arrays with `--homehost any`,
which keeps the stored name bare. The mask works because the stock rule
skips a device whose "SYSTEMD_READY" is zero, which the deployed mdadm's
rules must do. On dedicated CN hosts two blunter means need no name
matching: an mdadm.conf "AUTO -all", under which only arrays with explicit
ARRAY lines, such as the OS array, assemble, or shadowing the stock rule
entirely.

## Recorded design decisions

Each decision states the rule it shapes and its reason; the rest of this
document and the component documents cite it by its id.

* **[D1] Leg identity and the leg wrapper.** A side subsystem's NQN names
  the *leg*, not the side, and omits the `dn_id` (NQNs), so both sides of a
  migrating leg export the same NQN with the same namespace identity (Disk
  node) from two different DNs. Kernel nvme multipath therefore merges
  them: the CN holds **one** namespace device per leg with one path per
  side, and a migration is an ANA flip between paths — md never sees a
  device change, and the switch costs no reconnect and no dm reload. The
  cost is the rule that the two sides of a migrating leg must use disjoint
  cntlid slots (cntlid slots). A cn-local dm-linear, the leg wrapper, still
  wraps that namespace device, for two reasons: md and the group devices
  consume a dnv-named member that a sweep can attribute (dm device names,
  Teardown by sweep) rather than a kernel node, whose name carries no owner
  and whose device number can change across reconnects; and its table is
  sized from the desired state, never from probing the device. It is not
  the mechanism that survives a side swap, and it stays agent-internal (dm
  device names).
* **[D2] Subsystem identity.** A subsystem's serial is its `ss_id` rendered
  with `IdKeyFmt`, and its model is the one fixed dnv model string
  (`subsystemModel`) — both stable across cntlrs, so hosts see one device.
* **[D3] Empty destination td.** Clone correctness (the precondition of
  Clones) and crash recovery (Clone crash recovery) both assume the
  destination td was never written before the clone; "mapped ⇒ copied"
  holds only then. The control plane cannot verify it cheaply; it is a
  documented contract, satisfied trivially by creating the td right before
  `CreateClone`.
* **[D4] Three fixed ANA groups per port.** Every agent's single port
  carries exactly three ANA groups, created at port setup with fixed ids
  and states dnv never uses as a transition — every port converge
  re-asserts the fixed state (`EnsurePort`), and every ANA move rewrites a
  namespace's "ana_grpid", never a group's state: `AnaGrpIdOptimized`
  (optimized; nvmet's always-present default group),
  `AnaGrpIdNonOptimized` (non-optimized) and `AnaGrpIdInaccessible`
  (inaccessible), constants of `common/constants.go`. Moving a namespace by
  rewriting its "ana_grpid" is safe on a live namespace precisely because
  the target group always exists (a
  nonexistent group id blackholes IO). The fixed set stays far below the
  kernel's per-port cap on ANA groups regardless of namespace count, and
  leaves nothing to allocate, persist (in etcd or locally) or recover after
  an agent restart. A group is nested under its port
  ("ports/{id}/ana_groups/{grp}"), so the ids mean nothing across ports —
  two agents sharing a kernel under distinct `--nvmet-port-id`s get three
  groups each — and nothing across nodes.
* **[D5] Location copy in capacity values.** `DnConf.location` and
  `CnConf.location` are authoritative; the capacity key's value carries a
  copy so that allocator scans stay read-only range scans. A `location`
  never changes, and every operation that changes an input of the capacity
  rule re-puts the node's capacity key, copy included, inside its own STM
  (`MaintainDnCapacity`, `MaintainCnCapacity`; `dnv-worker.md` MD4).
* **[D6] Health-block payload.** The leg health probe (Group on-leg layout:
  meta region, data region, health block) writes the magic, the writer id
  and a unix timestamp and reads the block back with O_DIRECT; success
  within the nvme and command timeouts means healthy. Several cntlrs
  probing the same block concurrently is harmless: the content is never
  interpreted beyond "IO completed".
* **[D7] Push-bitmap durability.** Every received `PushMigrBitmap` and
  `PushCloneBitmap` chunk is persisted as its own file under
  `LocalMigrBmPath` or `LocalCloneBmPath`, and the applied sets reported in
  `bm_info` and `bm_info_list` are derived from the files, so an agent
  restart never forces a re-push — save for a chunk whose own file, or
  whose owner's or that owner's parent's, the restart could not load
  (Common agent rules) — and a rebuilt dm-clone can re-apply source bitmaps
  locally (Bitmap push protocol, Clone crash recovery). Both dm-clone
  features — `no_hydration` **and** `no_discard_passdown` — are mandatory
  on **every** dnv dm-clone, cn clone and dn migration alike
  (`agent/dm.go` builds the table), because `blkdiscard` must stay
  metadata-only: with passdown enabled dm-clone also remaps the discard to
  the destination device, and passdown is on by default whenever the
  destination's discard granularity is at most one region — which both a
  raid0 over thin volumes (cn) and a dm-linear over a raw disk (dn)
  satisfy. Without it the idempotent re-apply this decision assumes, and
  the Common agent rules' "re-`blkdiscard`ing an already-hydrated region is
  a no-op", would destroy acknowledged writes: a chunk read from the CN
  thin metadata *before* a host write hydrated region *r* can arrive
  *after* it, and the resulting discard would reach the destination. The
  accepted gap: a clone chunk grown by `AppendCloneBitmap` after its last
  push is re-delivered only while the pushing worker keeps its in-memory
  revision memo (`dnv-worker.md` BM5) — source bitmaps never affect
  correctness, so a missed tail only costs some avoidable copying.
* **[D8] Grown clone chunks.** `AppendCloneBitmap` may grow an
  already-acknowledged chunk `(src_slice_idx, bm_idx)` up to its
  `CloneBmChunkBytes` ceiling; the worker re-pushes when it observes growth
  — the memo it keeps for that, and the key the memo is kept under, are
  `dnv-worker.md` BM5 — and a worker change can leave the shorter version
  at the agent — accepted, because source bitmaps only ever cost extra
  copying, never correctness (Bitmap push protocol, Clone crash recovery).
* **[D9] `creation_epoch` in `cluster_id`.** Hashing only the name would
  make `cluster_id` a pure function of a user-chosen string: a cluster
  deleted and recreated under the same name would reuse the whole key space
  and every derived node-local name (`getShortId`, dm names, NQNs), so any
  leftover etcd key or leftover device of the old incarnation would be
  silently adopted by the new one. Mixing in a gateway-stamped
  `creation_epoch` (unix nanoseconds) makes each incarnation a distinct id,
  at the cost of one `ClusterConf` read before any other key can be
  formatted (cluster_id derivation, STM discipline). The epoch lives in
  `ClusterConf` rather than in the key so that `cluster_name` stays the
  user-facing handle and `ListClusters` keeps working off a plain name
  prefix.
* **[D11] Read-only is a CN-only mechanism, never a bdev flag.**
  `SP_LEVEL_READONLY` means "every user-facing namespace serves reads and
  fails writes", and is implemented by reloading each `CnNsDevName` onto a
  dm-flakey `error_writes` table (SpLevel; `cnagent.md` CN16). A
  block-device read-only flag cannot work anywhere in the stack: nvmet
  opens a namespace's backing device for reading and writing, so an
  export's top device fails "echo 1 > namespaces/1/enable" with EACCES when
  it is read-only; and a read-only flag below the top is bypassed entirely
  by device-mapper remapping. Nor can the DN fail writes as a substitute:
  md superblock and bitmap writes, resync, failover assembly and the health
  probe of Group on-leg layout: meta region, data region, health block must
  keep flowing at every read-only level. Consequently the DN has **no**
  read-only behavior at all — every level below `SP_LEVEL_NO_MIGRATION`
  behaves there like `SP_LEVEL_READWRITE` — and hydration is never paused.
* **[D12] Fencing ends in a table reload onto an error target, and any
  suspension before it is bounded.** A path being decommissioned is taken
  out of service by reloading it onto its dm-error: dm-error's map
  function returns "DM_MAPIO_KILL", so the bio completes immediately with
  "BLK_STS_IOERR", which is the correct outcome for a path that is going
  away.

  The migration cutover (Migration) precedes that reload with a **bounded**
  suspension of `SuspendSeconds`, so that IO the old primary still had in
  flight is absorbed rather than instantly failed. This is safe only
  because of the ordering: device-mapper releases a suspended device's
  deferred bios against whatever table is live at resume, and the reload
  installs dm-error *before* resuming, so the absorbed IO is **errored at
  the end of the window, never replayed** onto the side's data. Replaying
  it is the outcome that matters — on a migration source it could land
  after hydration had already copied that region, silently diverging
  source and destination.

  Everything else about a suspension stays hostile, which is why the bound
  is load-bearing rather than advisory. A suspended dm target queues bios
  forever with no timeout and no error path, so (a) any block-device
  scanner that touches it blocks in uninterruptible D state — "exit_aio"
  then makes that task unkillable and the node needs a reboot — and (b)
  `dmsetup remove` on it does not succeed. The dn agent therefore suspends
  nowhere except inside this one window and lets no device outlive it —
  across a restart that finds it suspended, or under a teardown step that
  runs over it — short of a reload that fails and the cases `dnagent.md`,
  Known limits, names; `dnagent.md` DN6 and DN12 are the rules that keep
  the bound. A reload fails closed (`dnagent.md`, OS wrappers — `dm.go`,
  `nvmet.go`, `nvmehost.go`; `cnagent.md` CN16): one whose load fails
  leaves the device suspended on its old table, queueing its IO until a
  later reload or resume of it succeeds, so the bound holds only as far as
  the reloads succeed. The side failover (Failover) has no window at all:
  it reloads onto dm-error directly. The residual exposure is external
  tooling — udev, blkid, an operator's lsblk — reading a source's linears
  during the window, or longer where that bound does not hold; no dnv agent
  scans block devices, neither running an LVM command ([D13], [D14]).

  An effectively suspended namespace (Namespace suspend semantics) — a
  stored-suspended one, or the origin an `auto_suspend` transfer names
  (Transfers; Transfer + clone = cross-SP live migration) — is not held
  dm-suspended for the life of the suspension, which would be unbounded,
  with no `SuspendSeconds` floor or ceiling, and the one place a
  block-device walker on a CN could wedge in D state. It is **parked**
  instead: its ns-dev reloaded onto the td's dm-error and resumed, the
  namespace inaccessible — with **no cutover window**, because the
  namespace is moved to inaccessible before its own ns-dev is touched
  (`cnagent.md` CN16) and nvmet refuses IO to an inaccessible namespace at
  the target, so a window would absorb nothing but a local scanner's bios;
  the reload's own flushing suspend is what keeps in-flight IO from being
  replayed. Two bounds on that argument are stated rather than hidden: the
  ANA write is best-effort (a failed "ana_grpid" write is logged and the
  pass continues), so a host racing that failure takes IO errors rather
  than queueing in a suspended device — fail-fast rather than an
  unkillable wedge, and the next converge rewrites the group; and the clone
  rebuild (Clone crash recovery) parks a *serving* namespace's ns-dev with
  no ANA move at all, which is that recovery's own documented window and
  not this one (`cnagent.md` CN18). With the park, the CN holds no
  suspension **across** a converge pass at all, short of a `dmsetup`
  command that fails (a park whose load fails leaves its ns-dev suspended
  on its old table: `cnagent.md` CN16 and `cnagent.md`, Known limits).
  What is left there is the snapshot quiesce of `cnagent.md` CN14, a
  suspend bracketed around one per-slice snapshot sequence and resumed
  before the pass returns unless that resume fails — otherwise bounded by
  construction.
* **[D13] The DN carries a self-describing dnv disk format; the dn agent
  uses no LVM.** Three reasons keep LVM out. (a) Its label scan reads every
  block device on the node, so any dnv device in a bad state takes the
  whole node's LVM down with it — the suspended-device wedge of [D12] is
  one instance of that class, and [D12] removes only that instance. (b) A
  layout that puts a volume group on a logical volume needs
  "devices/scan_lvs = 1" in the host's "/etc/lvm/lvm.conf"; depending on
  host LVM configuration for correctness is a deployment hazard. (c) LVM
  brings global locks, per-command process spawns and version-to-version
  behavior differences into a converge path that must stay bounded by the
  command timeouts (Common validation). The DN carries instead the format
  of Disk node — a CRC-protected header block, two alternating
  CRC-protected volume-table slots (so a torn write can only damage the
  older copy), a fixed clone-metadata slot area and an extent area at
  `DnDataOffset` — all read and written natively in Go through the
  `OsClient`'s `ReadBlock` and `WriteBlock`. A side is one aggregate
  dm-linear `DnSideName` concatenating the side's extent runs, and a
  migration's dm-clone metadata is a slot plus a wrapper dm-linear
  `DnMigrMetaDmName`. **The on-disk volume table, not the agent's local
  store, is authoritative for extent placement**: converge is
  lookup-or-allocate, so a node that loses `--local-store` but keeps its
  disk recovers exactly the layout it had — which is also why the agent's
  orphan sweep is driven by the DN's authoritative `side_pointer_list` and
  never by "this side has no local state" (`dnagent.md` DN6). A disk
  already formatted for a different cluster, dn or `extent_size` is
  refused, never overwritten (as pvcreate refuses a foreign physical
  volume), and that refusal is enforced at the metadata layer: an
  unconfirmed disk rejects every mutation, because a failed `SyncupDn`
  does not stop the `SyncupSide` calls that follow it (Common agent rules;
  `dnagent.md` DN5). Changing any layout constant, or what a volume-table
  record holds, is a header-version bump, not a tweak: a disk written
  under another version is refused, never formatted over (Disk node).
* **[D14] The CN uses no LVM either; the clone-metadata arena is a slot
  allocator.** A clone volume group would bring onto the CN exactly the
  failure class [D13](a) keeps off the DN: a bare vgs or lvs label scan
  reads every block device on the node, and a CN holds error targets, md
  members and pathless multipath legs, and can hold suspended devices,
  which wedge an LVM scan in unkillable D state ("exit_aio";
  "ignore_suspended_devices" and "global_filter" do not help) — while the
  cn agent would run those scans on every converge and every Check round.
  The arena instead (Controller node, common; tmpfs / file names): one
  tmpfs-backed sparse file (`CnTmpFilePath`, `CnCloneMetaAreaSize` bytes)
  on **one** loop device, which the agent finds afresh rather than
  remembering, because loop names are unpredictable and a per-clone loop
  sprawl is unwanted — carved into `CnCloneMetaUnit` units and handed out
  first-fit in **contiguous** runs to wrapper dm-linears
  `CnCloneMetaDmName` (cn dm kind `cb`, `cnagent.md`, Names and constants
  in `common`; the wrapper exists because dm-clone reads its superblock
  from sector zero and takes no offset, the same reason `DnMigrMetaDmName`
  does). **The kernel's dm tables are the allocation registry** — each
  wrapper's linear table, its length over the loop device at its offset,
  records its own allocation, and the arena is volatile *together with*
  the dm state (a reboot clears both, an agent restart preserves both) —
  so there is no on-file allocation table and none of [D13]'s header, CRC
  and A/B machinery is needed. Every allocation hole-punches its range
  first, because a freed unit still holds the previous clone's valid
  dm-clone superblock and a hole punch gives guaranteed zeros by *file*
  semantics (no device DLFEAT involved) while releasing the tmpfs pages,
  where writing zeros would materialize up to the whole arena in RAM
  (`cnagent.md` CN5, CN18). The reasons: the [D13](a) label-scan class is
  eliminated rather than mitigated, lvm2 is one dependency fewer, and CN
  naming is as deterministic as everything else. Arena exhaustion reports
  `RES_STATUS_ERROR` on the clone's resources; orphaned kind-`cb` wrappers
  are removed by the sweep (Teardown by sweep), which frees their units; a
  wrapper whose length or backing loop path does not match the currently
  probed loop device is `RES_STATUS_ERROR` and is repaired by the clone
  rebuild (Clone crash recovery).
* **[D15] The tenant promise rests on dm-thin; a new side is zeroed where
  the design assumes zeros, behind a `provisioned` gate;
  `RES_STATUS_PROVISIONING`.** A host reads a pool's data only through the
  pool's thin devices, and dm-thin is what keeps another pool's bytes from
  it (System overview): a thin device returns zeros for a block it has not
  provisioned, and dm-thin writes a block it provisions whole — zeros
  around a write that covers only part of it — before a host can read any
  of it. dnv's thin pools therefore never skip block zeroing: the pool
  table carries no feature arguments, "skip_block_zeroing" among them, so
  dm-thin's default holds (`cnagent.md` CN13 builds the table), and a pool
  that skipped it would hand a host whatever a recycled extent holds
  around a partial write.
  What a recycled extent holds stays in a leg's data region, where no pool
  reads it, as long as every copy below the pool carries each block the
  pool has written: a migration skips only regions no pool block maps,
  which is why Migration says when a caller may read its leg bitmap. Root
  on a node can read those bytes from the raw leg, as it can read anything
  on its node, and the promise does not cover that (Known limits). Zeroing
  serves only the places where the design assumes zeros: a meta group's
  side is zeroed over its whole leg span, and a data group's side over its
  meta region and first data block (Side provisioning protocol says why),
  with `blkdiscard --zeroout` before its first export. A trim cannot
  deliver those zeros: `blkdiscard`
  does not imply zeros (the kernel does not promise that a discarded
  region reads as zeros, and NVMe's read-zeroes after deallocate is
  optional). The progress is a byte count on the authoritative volume
  table ([D13]), gated by the control-plane-visible `Side.provisioned`
  flag that the sp worker flips once the agent reports the zeroing
  complete (Side provisioning protocol; `dnv-worker.md` RW18). The rest of
  a data group's side is never zeroed: no pool reads a block of it before
  writing the block whole, and zeroing it would write every extent of
  every new side. Nor is a tail: old md formats and some partition and
  file-system signatures sit at the end of a device, and a recycled extent
  at the end of a new side may hold one that a host wrote into a thin
  device of an earlier pool. An md superblock there makes mdadm refuse the
  leg as a foreign member, and the group reports an error (Known limits).
  That failure is rare and loud, a host of a running pool can cause the
  same at any time, so zeroing a tail would close only half of it, and one
  zeroed prefix per side keeps the tracking simple. Resources deferred
  while a side underneath them zeroes report `RES_STATUS_PROVISIONING` —
  *healthy, not ready, no action needed* — which **never** sets
  `err_epoch`; `ERROR` means *needs intervention*, which is why a missing
  record at `provisioned` true (data loss on a lost or foreign disk) stays
  `ERROR` and feeds Automatic reactions rather than looking transitional.
  The zeroing runs in the background, off the agent's locks and under the
  ordinary command timeouts (Common validation; Side provisioning
  protocol; `dnagent.md` DN9); a synchronous zeroing inside one
  `SyncupSide` is not an option, because a meta group's side is zeroed
  over its whole leg span and a node-read holder plus one queued
  `SyncupDn` writer would freeze the whole DN agent. The meta-region
  zeroing is also what lets the group assembly take a leg without a
  superblock for a fresh side, and dm-thin's whole-block writes are what
  let it create that array clean over data regions that differ
  (`cnagent.md` CN12); a meta group's whole zeroing is what gives a fresh
  pool a metadata device that reads zero (`cnagent.md` CN13).
* **[D16] Failover fencing has no epoch; safety = per-side atomic flip + md
  arbitration.** Among the nodes a failover moves — the cntlrs and the
  sides — the only coordination is the revisioned fan-out of sp role,
  unordered among the sides; toward hosts the failover also withdraws the
  old primary's address from the discovery records ([D18]). The fan-out
  comes in two waits, each bounded in time. The demotion hold hands the
  old primary its demotion first and holds the sides and the other cntlrs
  until it reports the demotion applied, for `DemotionHoldTimeout` at most
  (`dnv-worker.md` RW22): an old primary that applies its demotion within
  the bound moves its namespaces to ANA inaccessible before any side
  fences it, so hosts queue their IO instead of failing it, and for an
  unreachable one the bound is what a host that follows the discovery log
  needs to drop the withdrawn path (`dnv-worker.md` RW22 says how it is sized).
  The worker judges no reachability: an old primary that answers ends the
  demotion hold early, and one that never answers uses up the bound its hosts
  need. The
  sides-first hold then keeps back the cntlrs' requests until every side the
  worker drives has reported the revision applied, for one
  `cntlr_interval` at most (`dnv-worker.md` RW14), which mitigates three
  races: a promotion outrunning the sides' ANA flips, a cntlr's connect
  outrunning a new side's export, and a leg removal's disconnect on a CN
  overlapping the disk node's unlink of that side's export. Neither wait
  is a correctness dependency: at each bound the next requests go whether
  the awaited reports came or not, and nothing below relies on them.
  Correctness rests on two things (Failover, "Make sure all groups are
  available"): each DN converges its side's old-primary-to-dm-error and
  new-primary-to-side-device reloads in one pass, so one leg never has
  two writers — short of a reload of the old
  primary's linear whose load fails (`dnagent.md`, Known limits); and
  mdadm's assembly rules arbitrate across legs — a stale leg whose
  superblock claims a clean full array will not start degraded alone, and
  event counts plus resync direction repair divergence once both legs
  return. md's assembly behaviour is therefore a load-bearing external
  dependency, and no suite in the tree pins it (Known limits). For a host
  that follows the discovery log, the two waits and the withdrawal close
  both edges of a failover: an old primary that applies its demotion within
  the demotion hold is inaccessible before the fence, and one the worker
  cannot reach is off the host's paths before it. Two costs are accepted.
  Every failover of an old primary that does not answer, a dead CN's
  among them, spends the demotion hold's bound before its sides are
  told, and its new primary comes up that much later. And an old primary
  still on a host's path at the fence that has not applied its demotion —
  the residual cases of Known limits — returns DNR
  internal errors that host multipath does not fail over from, so
  applications can see EIO until it applies its demotion or is stopped.
  Closing that residue takes a fencing epoch checked on the data path (NVMe
  reservations, or a per-revision gate at the side exports).
* **[D10] Id-keyed revision keys.** `DnRev`, `CnRev` and `SpRev` are keyed
  by `dn_id`, `cn_id` and `sp_id`, not by the `addr_port` or `sp_name`
  that keys the matching `DnConf`, `CnConf` or `SpConf`; the mutable handle
  lives in the value. The key is then immutable for the object's whole
  life, so relocating a node or renaming an SP — however that is driven —
  stays a single put on the rev key rather than a delete and a put, which a
  watching worker would otherwise have to interpret as one object leaving
  and another arriving, with a revision starting over and a spurious
  teardown in between. It also makes a watch event self-contained: the
  worker learns where to dial (`addr_port`) or which `SpConf` to read
  (`sp_name`) straight from the value it just received (Revision keys and
  the sync fan-out, dn / cn roles, sp role). `SpName` (`sp_id_to_name`)
  stays needed because reaching `SpRev` from an `sp_id` alone would require
  knowing the SP's `shard_code`, which only `SpConf` — reached by name —
  carries.
* **[D17] Worker membership by heartbeat, grace windows and sha256 tickets;
  no lease.** A worker refreshes its registration key (`WorkerReg`, Key
  table) every vote interval; observers judge liveness by their own
  monotonic clock since the last put they saw, a registration not refreshed
  for the dead threshold being dead; every observed transition must hold
  for the grace time before it changes the observer's effective membership;
  ownership is the largest sha256 ticket of seed, role and shard among
  effective members; and a worker whose own heartbeat stops reaching etcd
  or stops being echoed by its watch, or whose key a peer has deleted, or
  whose own observer commits it dead, fences itself and rejoins as a fresh
  identity (`dnv-worker.md` VW3, VW5, VW8, VW9). Why not a lease: a lease expiry is a single server-side event
  with no notion of "stable" — a worker flapping at the keep-alive boundary
  reshuffles ownership on every flap, and a newly started worker takes its
  shards instantly while the old owners still hold them. The explicit
  scheme makes appear, disappear and reappear first-class, testable
  transitions with one damping rule, keeps a dead worker's last heartbeat
  visible to operators, and depends on no lease keep-alive semantics. Why
  observer-local time rather than the stored epoch: comparing a writer's
  wall clock with a reader's makes correctness depend on NTP — a clock
  running ahead would let one worker claim every shard while the others
  keep theirs, undetected; the epoch in the value is therefore
  informational. The costs, all accepted: a key a
  scan finds untracked needs a whole dead threshold before it can be
  declared dead; a fresh worker drives nothing for its first grace window;
  ownership changes overlap or gap by a few seconds across workers, safe
  for the reasons and within the bounds of `dnv-worker.md` VW7; and dead
  keys are garbage-collected by their observers instead of expiring — which
  is also what lets a worker detect that the fleet has given up on it (it
  sees its own key deleted) and rejoin as a fresh identity.
* **[D18] The discovery records list the enabled primary and every standby
  a failover may elect.** A cntlr's address is in its SP's discovery
  records — the `nvme_tr_conf_list` of every `CdcEntry` of the SP, one
  entry per subsystem — while the cntlr is enabled and either is the
  primary or has a zero `err_epoch`. That is the listing rule. It lives in
  one place, `model` (`CdcListed`, `CdcTrConfList`), and every writer of the
  records applies it: the gateway's subsystem and cntlr calls (Subsystems,
  namespaces; Cntlrs), and the sp worker's health write for a cntlr, its
  failover and its cntlr replacement (sp role, Automatic reactions). Each
  writer sets the whole list from the cntlrs its own transaction reads, in
  `cntlr_id_list` order, and never adds or drops a single address, so no
  interleaving of writers can leave a list the rule does not give; a
  gateway writer puts a missing entry back, and a worker writer leaves it
  missing (Cntlrs). The reasons: a serving primary is never taken away from
  hosts, so a primary with an error that no failover fixes keeps its path
  and the IO it still serves; the health epoch exists already, so the rule
  needs no new field; and the rule names exactly the standbys the failover
  may elect (`dnv-worker.md` AR5), so a host that follows the records
  holds a path to every cntlr that can become its next primary and to no
  standby that cannot.

  The consequences. A new cntlr is listed from its creation, enabled and
  with a zero `err_epoch`: a host that connects to it before its agent has
  built the subsystem only retries, and listing it only once built would
  need a field for a harmless case. The failover's own transaction takes
  out an old primary that has an `err_epoch`, so hosts that follow the
  discovery log drop its path, and for one that does not answer its
  demotion they drop it while the demotion hold still keeps the sides back
  (Failover). Its next clean verdict lists it again as a standby, if it is
  enabled; a clean reply at a revision older than the one the worker
  drives gives no verdict, so that an old primary that resumes is not
  listed while it still exports its namespaces optimized, possibly over
  legs the sides have fenced (`dnv-worker.md` HL2). An enabled standby
  leaves the records when its `err_epoch` is set and returns when it is
  cleared: hosts drop and re-add a path that is ANA inaccessible
  anyway, with IO untouched, and nothing damps that churn, which shows in
  the worker's health records and points at the sick standby. An enable
  lists a cntlr at once only when it is the primary or its `err_epoch` is
  zero, and any other at the worker's next clean verdict on it. A health
  write still bumps no revision (Revision keys and the sync fan-out):
  dnv-cdc watches the `CdcEntry` keys themselves, and no agent's desired
  state moves.

## Known limits

These are the boundaries dnv accepts, stated so that scale, recovery and
security expectations are explicit rather than discovered. None of them is
a defect in the mechanisms above. A limit of one component is stated in
that component's document and only pointed to here.

* **One primary per SP is the volume throughput ceiling.** Every td of an
  SP is served by its single primary cntlr; slices shard *within* that CN,
  never across CNs. The aggregate bandwidth of one volume is one CN's.
  Scale-out is by storage pool: spread SPs, and so their primaries, across
  CNs.
* **Revision granularity is the SP.** Any `SpRev` bump re-fans the complete
  desired state to every side and every cntlr of the SP (Common agent rules,
  full sync; sp role). During initial provisioning each side's `provisioned`
  flip is itself a bump, so a large SP's bring-up costs
  O(sides × (sides + cntlrs)) syncups; the sp role's allowance to batch
  several flips into one STM (`dnv-worker.md` RW18) is the mitigation.
  Steady state is one fan-out per transaction that changes the SP's
  desired state — a user mutation, or a worker transaction such as the
  created flip (next bullet) — and none for health bookkeeping (Revision
  keys and the sync fan-out).
* **One more fan-out per td creation.** Every created flip bumps `SpRev`
  (sp role, `dnv-worker.md` RW19), so creating a td costs two full fan-outs
  of the SP instead of one; batching the flip keeps bulk creation at one
  extra fan-out per reply, or per `MaxFlipCreatedPerTxn` tds for a reply
  that completes more.
* **The agent node lock has head-of-line blocking.** A pending node write
  (`SyncupDn`/`SyncupCn`) blocks every new node-read acquisition behind the
  slowest in-flight object converge. Converges are bounded per command
  (Common validation) wherever the command's child can be killed; one
  blocked in an uninterruptible kernel wait is not (Common validation), nor
  is an in-process sysfs, configfs or device access once its syscall has
  started (`dnagent.md` SH15). The cn sweep's `nvme disconnect`, whose
  delete can wait out the kernel's admin timeout, runs off the locks for
  that reason (`cnagent.md` CN10 and CN21); the disconnects that still run
  under a lock are each role's own limit (`cnagent.md`, Known limits;
  `dnagent.md`, Known limits). On a node where many commands run to their
  timeouts, one sick object can delay every other object's converge and
  Check round by up to a whole converge pass. Accepted: object locks keep
  steady-state concurrency, and the zeroing loop's try-acquire rule
  (`dnagent.md` DN9) keeps the one long-running background writer out of
  that queue.
* **No thin-metadata repair path is specified.** Pool metadata is redundant
  at the block layer (meta groups can be `RedundMdRaid1`), but there is no
  "thin_check"-on-activate or "thin_repair" flow for a pool whose dm-thin
  metadata is corrupt. A pool-metadata `RES_STATUS_ERROR` is therefore an
  operator-intervention event (activate read-only, "thin_repair" onto fresh
  space by hand).
* **The fabric is trusted.** dnv authenticates no user and no host beyond
  its NQN: gRPC is plaintext (`grpc.md`, Wiring), etcd access is whatever
  the deployment configures, and data-plane access control is host-NQN
  allow-lists — a spoofable identifier. dnv has no NVMe in-band
  authentication and no TLS; the layer above authenticates its users
  (System overview). The [D15] guarantee is that a host never reads,
  through a pool's thin devices, bytes another pool wrote; it does not
  defend against an attacker on the storage network, nor against root on
  a node, which can read the old bytes of a recycled extent from the raw
  leg or disk. Deploy on an isolated, trusted fabric.
* **A pool commit in a migration gap can wedge the primary.** A migration
  gap is the time in which the primary has no usable path to a migrating
  leg: from src step 1, which moves the source's per-CN namespaces to
  inaccessible, until the primary's path to the destination is live and
  optimized (Migration). In that gap anything that commits the thin pool on
  that leg can block. A commit flushes the pool's data device and writes its
  metadata device, which between them reach every working leg the pool sits
  on, and the leg with no usable path holds that IO: nvme queues the IO of a
  namespace that has a live controller but no usable path. A leg that md has
  failed out of its md-raid1 group gets none of this IO; md fails such a leg
  when an IO it sends failfast (`cnagent.md` CN12) reaches the source after
  src step 1, before the primary has read the ANA change, and the array
  stays degraded afterwards (`cnagent.md`, Known limits). The primary's
  check rounds and converges read the pool's status (`cnagent.md` CN28), and
  that read commits the pool; so do a thin device's create or delete
  (`cnagent.md` CN14), every read through a thin metadata snapshot, the
  bitmap reads of a thin device and of a data leg among them (`cnagent.md`
  CN25), and dm-thin's own commit once host writes have mapped new blocks.
  While a commit waits, the rest of that pool's IO waits too. A check round,
  a converge or a bitmap read that waits holds the cntlr's lock and the node
  read lock meanwhile (`cnagent.md` CN1). If the primary has not connected
  the destination yet, the converge that would connect it (`cnagent.md`
  CN10) needs that cntlr lock, so the cntlr can stay blocked until the
  migration ends. A cancel gives the primary back its path to the source,
  which lets the commit finish. A finish removes the source's exports, which
  fails the IO that waits on the leg: an md-raid1 group that keeps another
  working member absorbs that error, as md completes the array's flush and
  charges a failed write to the leg, so the commit goes on; a `RedundNone`
  group passes it up, so the waiting commit fails, and dm-thin drops the
  block mappings made since the last commit and leaves the pool read-only
  with its needs_check flag set, which nothing clears (No thin-metadata
  repair path is specified, above), while the pool's row still reads OK
  (`cnagent.md` CN28). On such a group only a cancel ends the block with the
  pool intact. Meanwhile a node write queued behind the blocked call, a
  `SyncupCn`, holds up every later call of that agent that takes the node
  lock (the head-of-line blocking above). Migration's read rule does not
  keep a bitmap read out of a gap: a data leg's bitmap read that keeps the
  rule for its own leg can still block in the gap of another leg the same
  pool sits on, and a thin device's bitmap read can block in the gap of any
  leg the pool it reads sits on.
* **A leg bitmap read too early can hand a pool another pool's bytes.**
  Nothing in the gateway orders a `GetLegBitmap` read against the
  migration it feeds; the caller keeps the order (Migration). Read while
  the source still takes writes, the bitmap marks as never written every
  block the pool maps through the source afterwards; the destination skips
  those blocks, and the pool later reads there what the destination's
  recycled extents hold, which may be another pool's bytes ([D15]). A data
  leg's read in the migration gap can block instead (above).
* **A recycled tail can fail a fresh leg.** No tail of a new side is
  zeroed ([D15]): an md superblock of a format that sits at the end of a
  device, which a host of an earlier pool wrote there, can make a fresh
  leg read as carrying one, and mdadm then refuses the foreign member, so
  the group reports an error (`cnagent.md` CN12).
* **QoS is not enforced** (Controller node, common): agents accept and persist
  `qos_ratio` and enforce nothing (`cnagent.md` CN6).
* **Snapshot creation is not atomic across a primary crash.** With the
  origin quiesce of Thin devices a snapshot is point-in-time against live
  IO, but a crash between two slices' `create_snap` messages still leaves a
  torn snapshot; delete and re-create it. `ThinDevice.created` certifies
  materialization, not point-in-time consistency: a torn snapshot still
  flips to `created` once every slice's volume exists.
* **A snapshot accepted before a clone targets its origin can hold part of
  the copy.** `CreateThinDevice` refuses a snapshot of a clone's destination
  while the `Clone` key exists (Thin devices), but `CreateClone` does not
  look for an uncreated snapshot of its destination: finding one means
  walking every td of the SP, the walk `DeleteThinDevice` keeps out of its
  transaction. The primary's pre-pass sends every ready slice's snapshot
  message before the same pass builds the dm-clone (`cnagent.md` CN14,
  CN18); only a slice it skips — its pool not ready, or its thin device
  unreadable — or whose message fails is retried on a later converge, which
  can come after hydration has begun. That slice of the snapshot then holds
  whatever the copy has written into the destination by then, while the
  slices messaged in time hold the empty td, and the snapshot still flips to
  `created`. [D3]'s destination is empty, so the exposure is a snapshot of
  an empty td, and only a snapshot not yet created by the time a clone into
  that td is created.
* **md's degraded-start refusal is an unpinned external dependency.** The
  cross-leg half of failover safety ([D16], "Make sure all groups are
  available") rests on mdadm refusing to start an array from a lone
  survivor whose superblock still counts a clean full array; no suite in
  the tree asserts that refusal, so nothing in the tree detects a change
  in the deployed mdadm's behaviour.
* **An md check of a RAID1 data group counts mismatches.** The legs of a
  data group are never zeroed past their first data block, so they may
  differ wherever the pool has not written ([D15]), and an md "check" of
  the array, such as a distribution's periodic scrub, reports mismatches
  there. They are harmless, since the pool never reads those blocks, and
  dnv runs no check of its own.
* **A stale discovery view keeps a withdrawn path.** A cdc twin that has
  lost etcd keeps serving its last known records (dnv-cdc; `cdc.md` DS10),
  a failed-over primary's address among them, and nvme-stas keeps a path
  for as long as any discovery controller it reads still lists it
  (`cdc.md` DS2). A host running nvme-stas whose discovery connections
  include such a twin keeps the old primary's path past the fence
  (Failover).
* **A path that nothing following the discovery log manages is never
  dropped.** A path connected by hand on a host that runs no nvme-stas or
  equivalent, or a controller named in nvme-stas's own configuration
  rather than discovered, stays when the records withdraw its cntlr, so
  that host keeps a failed-over primary's path past the fence (Host view,
  Failover).
* **An old primary that cannot apply its demotion within the demotion
  hold fails IO from the fence.** An agent whose demotion waits behind a
  slow converge for the cntlr's locks (`cnagent.md` CN1; the head-of-line
  blocking above) applies it only after the sides have fenced its legs,
  and until then answers IO errors on its still-optimized path to every
  host still on that path — a host of the two entries above, or one whose
  discovery-log follower drops the path later than the demotion hold ends
  (Failover).
* **A cntlr replacement of a serving primary can fence it first.** A
  replacement deletes the cntlr it replaces, so its fan-out starts no
  demotion hold and keeps the sides-first order (`dnv-worker.md` RW14,
  RW22). When the replaced cntlr is a primary that still serves — the
  sole-primary replacement of Automatic reactions — its sides drop its
  exports in no order with its CN's teardown of its stack, which the CN
  learns of from its own node's syncup, and until that teardown it answers
  IO errors on its still-optimized path to every host still on it. The
  case needs a pool with no failover candidate, whose hosts lose the old
  primary's path either way; the replacement's path comes with its build.
* **A failover with no demotion hold can fence the old primary first**
  (`dnv-worker.md` RW22 and Known limits). When the SP's shard changes
  owner, or its worker restarts, before a failover's demotion hold ends,
  or a coordinator that has just started fans a failover out before its
  first hold has started its cntlr children, the sides can fence the old
  primary before it has applied its demotion, and until it has, it answers
  IO errors on its still-optimized path to every host still on that path.
  A host that follows the discovery log still drops the path the failover
  withdrew, possibly only after the fence (Failover).
* **Read-only reaches a CN only with its converge.** A CN that has not yet
  converged to a revision carrying `SP_LEVEL_READONLY` keeps serving
  writes until it syncs; there is no DN-side enforcement point that could
  close the gap ([D11], SpLevel).
* **A pool can run out of space.** Auto-grow is best-effort — a grow can
  find no DN candidates, a slice whose data list is full takes no more
  group (GrowSlice; `dnv-worker.md` AR6 and Known limits), and nothing
  reserves space ahead — so a pool's data space can run out before a grow
  lands. The cn agent writes no feature arguments to the thin-pool table
  (`cnagent.md` CN13 gives the table), so an exhausted pool behaves as
  dm-thin's default "queue_if_no_space": IO needing a new block queues for
  the kernel's "no_space_timeout" (a dm-thin module parameter) and then
  fails with EIO, while already-provisioned blocks keep serving. A
  `low_water_mark_pct` above a hundred percent switches auto-grow off
  (Common validation).
* **A created td is never re-created by message** (`cnagent.md` CN14): a
  pool that has lost a created td's thin id reads `RES_STATUS_ERROR` on
  every converge, which belongs next to the missing thin-metadata repair
  path above — pool-metadata loss is an operator-intervention event — and
  what a lost id costs in failovers and replacements is `dnv-worker.md`,
  Known limits.
* **Worker membership tolerates, but does not repair, a partitioned
  observer** ([D17]; `dnv-worker.md`, Known limits).
* **Undriven windows are part of the membership design** (`dnv-worker.md`,
  Known limits): revision keys are durable, so convergence is delayed, not
  skipped.
* **A group's spare list can fill with parked legs** (`dnv-worker.md` AR8
  and Known limits).
* **A spare whose DN fails while it zeroes blocks its group's next spare**
  (`dnv-worker.md` AR8 and Known limits).
* **A slice's data space is capped when its SP is created**: every data
  grow appends a group of the slice's first data group's size, and a
  slice's data list holds at most `MaxGrpCntPerSlice` groups
  (Per-operation allocation, GrowSlice); what the worker does at that cap
  is `dnv-worker.md`, Known limits.
* **A reload fails closed** (`dnagent.md`, OS wrappers — `dm.go`,
  `nvmet.go`, `nvmehost.go`): a dm reload whose load fails leaves its
  device suspended on its old table, so [D12]'s bound on a suspension
  holds only as far as the reloads succeed; what that costs on each role
  is `dnagent.md`, Known limits, and `cnagent.md`, Known limits.
