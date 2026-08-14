# Live NVMe-oF Volume Migration (B → C), Transparent to Host A

## 1. The problem

- Host **A** (NVMe-oF initiator) is actively writing to a namespace backed by `dm-linear-B0` on target server **B**.
- We want the data moved to `dm-linear-C0` on target server **C**, and A writing to C — **without stopping A's I/O and without A noticing** (the block device on A must stay the same throughout).
- This must run at fleet scale: many A/B/C triplets, driven by a manager agent (etcd + gRPC to per-server agents), and it must survive **dirty reboots of A, B, or C at any point**.

## 2. The solution in one sentence

Export a **dm-clone** on C (destination = `dm-linear-C0`, source = B's data re-exported to C) under the **same NVMe subsystem identity** as B's export, so A sees B and C as two **ANA paths of one multipath namespace**; flip ANA to cut over, and let dm-clone hydrate the data from B while C serves A's live I/O.

```
A (host, native NVMe multipath — one head device the whole time)
├── path 1 → B: nvmet ns → dm-linear-B0 ────────────────┐  same physical
└── path 2 → C: nvmet ns → dm-clone                     │  extents on B
                             ├─ dest: dm-linear-C0      │
                             ├─ src : nvme conn → B (private NQN) → dm-linear-B1 ─┘
                             └─ meta: separate local disk on C
```

## 3. Procedure

### 3.1 Prep B
1. Create `dm-linear-B1` over the **same physical extents** as `dm-linear-B0`.
2. Export B1 via a **different, private NQN**, `allowed_hosts` = C only (replication channel).
3. Record B0's export identity from configfs: subsystem NQN, NSID, `device_uuid`, `device_nguid`, `attr_serial`, `attr_model`, size in sectors, LBA format.

### 3.2 Prep C
1. C connects to B1 with `ctrl_loss_tmo=-1`.
2. Verify B1 and C0 have **exactly** the same length and logical block size (dm-clone requirement).
3. **Zero the metadata device — first creation only** (stale persistent-data superblocks get reused otherwise). Never zero on crash reassembly.
4. Create the clone (destination before source):
   ```
   clone <meta_dev> <dest=dm-linear-C0> <src=B1> <region_size> 1 no_hydration
   ```

### 3.3 Export from C (identity cloning)
1. Subsystem with the **same NQN**, same `attr_serial` / `attr_model`, and **disjoint** `attr_cntlid_min`/`attr_cntlid_max` vs B (e.g. B: 1–100, C: 101–200).
2. Create a **dedicated ANA group**, set it `inaccessible` (nvmet's default group 1 is *optimized* — never use it here).
3. Namespace with the **same NSID**; set `device_uuid` / `device_nguid` to B's values **before enabling**; assign it to the inaccessible ANA group.
4. Only then enable the namespace/port.

### 3.4 Attach A
1. A connects to C (`ctrl_loss_tmo=-1` on both paths).
2. Verify: one multipath head, two paths, B = optimized, C = inaccessible, head device unchanged.

### 3.5 Cutover — all-paths-inaccessible ordering
1. **B: ANA → inaccessible.** A's new I/O fails back with ANA status and is requeued *inside A* — while live-but-inaccessible controllers exist, bios sit on the requeue list with **no timeout ticking**.
2. **B: `dmsetup suspend` B0** — drains in-flight I/O; source is now frozen. Optionally flush B's device caches (`nvme flush` / `sg_sync`).
3. **Verify by observation** that B is fenced + suspended before going further.
4. **C: `dmsetup message <clone> 0 enable_hydration`.**
5. **C: ANA → optimized.** A gets the AEN and drains its queued I/O to C. Automated, the stall is tens of milliseconds. No clone suspend/resume is needed in this ordering.

> **Why not "C optimized while clone suspended, then fence B":** a write can get stuck inside C's suspended clone; A times out (30 s default), retries the write via still-optimized B, then writes newer data W2 to the same LBA on B; after B is fenced and C resumed, the **stale stuck write replays on top of the hydrated region containing W2** → silent data loss. That ordering is only safe if the whole window stays far below A's I/O timeout; the all-inaccessible window removes target-side stuck I/O entirely.

### 3.6 Hydration
- Background copy runs; A's writes also hydrate their region on demand.
- Throttle with `hydration_threshold` / `hydration_batch_size` messages.
- **Do not touch B's data** — it is the recovery source if C dies mid-hydration.
- Alert if clone status reports metadata mode `ro`.

### 3.7 Finalize on C
1. `dmsetup suspend` the clone.
2. **While suspended**, re-check status: hydrated regions == total regions.
3. `dmsetup reload` with a linear table onto `dm-linear-C0`, then `dmsetup resume`. Same dm major:minor → the nvmet namespace on C never notices.
4. C disconnects from B1.

### 3.8 Release
1. A: `nvme disconnect` the B path.
2. Only now may B's exports be torn down and its disks repurposed.

## 4. Things that will break it if you get them wrong

### Path merging on A
| Attribute | Requirement |
|---|---|
| Subsystem NQN | identical on B and C |
| NSID | identical |
| UUID / NGUID | **explicitly set identical** — nvmet auto-generates a random UUID per namespace; on mismatch the host rejects the second path ("IDs don't match for shared namespace") |
| Serial / model | identical |
| Size in sectors, LBA format | exactly identical (also required by dm-clone) |
| CNTLID ranges | **disjoint** between B and C — a duplicate CNTLID within one subsystem makes the host reject the controller |

A must use native NVMe multipath (`nvme_core.multipath=1`, default on modern kernels).

### ANA
- C's namespace must be born inaccessible: dedicated group, set `inaccessible`, namespace assigned to it **before** it becomes visible. If C is ever momentarily optimized pre-cutover, a write triggers on-demand hydration from a still-changing source → **silent corruption**.
- nvmet **enforces** ANA at the target (I/O to an inaccessible group fails with ANA status), so this is real fencing, not a hint to the host.
- ANA state is per port + group: one dedicated group per concurrently migrating volume; nvmet caps at 128 groups per port.

### dm-clone
- Contract: **source is immutable.** It's violated between clone creation and the fence (A keeps writing to the same extents via B0) — safe *only* because zero I/O passes through the clone and hydration is off in that window.
- `no_hydration` disables **background** copy only; writes still hydrate their region on demand — exactly what you want post-cutover.
- Metadata device: zero on first create, **never** on crash reassembly. The orchestrator must distinguish `init` vs `reuse`.
- Region size trades write amplification vs metadata size and copy efficiency; 64 KiB–1 MiB is typical.
- Kernel ≥ 5.4.
- Crash semantics = volatile-cache disk: flushed/FUA writes are durable (metadata commits on flush), acked-but-unflushed writes may be lost on a dirty C reboot — the normal storage contract, so filesystems on A are fine.

### Connections
- `ctrl_loss_tmo=-1` on A→B, A→C, and C→B1, so transient outages become latency instead of I/O errors (a C→B blip mid-hydration then just stalls unhydrated reads instead of failing them up to A).

## 5. Fleet orchestration (manager + agents + etcd)

**Key insight:** nothing on these servers survives reboot — dm tables, nvmet configfs, nvme connections are all volatile. Treat that as a feature: a rebooted server comes up exporting nothing (fail-safe), and the agent's job is to *reconcile* toward a declarative desired state. Don't build imperative "do step 4" RPCs; build Kubernetes-style level-triggered reconciliation plus an etcd-persisted phase machine.

### Principles
1. **Declarative, complete per-server specs.** Per phase, the manager renders everything a server should look like: dm tables (devices referenced by stable IDs like `/dev/disk/by-id/wwn-...`, never `sdX`), nvmet subsystems/namespaces (NQN, NSID, UUID, cntlid range, ANA group + state), nvme-host connections. An agent can rebuild its entire local stack from the spec alone — that is exactly what makes dirty reboots recoverable.
2. **Idempotent "ensure" operations.** Agents converge: read actual state (configfs, `dmsetup table/status/info`, `/sys/class/nvme`), then act. Re-applying a spec is always safe. The one create-exactly-once exception is metadata formatting → explicit `metadata: init | reuse` field in the spec, controlled by the phase machine.
3. **Verify by observation, never by RPC success.** A gRPC timeout means *unknown*, not failed. Advance phases only after reading back observed state satisfying the phase postcondition. Most critical: never enable hydration until B has been **observed** with B0's ANA group inaccessible and B0 suspended.
4. **etcd as the write-ahead log.** Phase transitions are transactions guarded by mod-revision (makes manager restarts safe even without HA). Every spec carries a monotonically increasing generation; agents persist the last applied generation and reject stale/reordered pushes.
5. **Fail-safe boot.** Agent boots → applies nothing → reports `boot_id` (`/proc/sys/kernel/random/boot_id`) → receives current spec → converges. **Never auto-restore a local nvmetcli snapshot** — a stale snapshot re-exporting B0 as optimized post-cutover is the worst corruption scenario. A changed boot_id is also the dirty-reboot detector.

### Interfaces
```proto
service Agent {
  rpc ApplySpec(Spec) returns (Observed);   // full desired state + generation
  rpc GetObserved(Void) returns (Observed); // dm tables, nvmet cfg, paths,
}                                           // clone hydration %, boot_id
```
```
/migrations/<id>/spec                  # A/B/C ids, volume params, region size...
/migrations/<id>/state                 # {phase, ponr: bool, generation}
/migrations/<id>/servers/<sid>/desired # rendered spec per phase
/migrations/<id>/servers/<sid>/observed
/locks/volume/<vol>                    # one migration per volume
```

### Phase machine
Each phase = render desired specs → wait for observed postconditions → CAS `phase+1` in etcd.

| Phase | What happens | Advance when observed |
|---|---|---|
| `PREP_B` | B1 linear over B0's extents, exported on private NQN to C | export live |
| `PREP_C` | C connects B1; metadata `init` + zero; clone created `no_hydration`; size/LBA precheck | clone active, 0 hydrated |
| `EXPORT_C` | subsystem with cloned identity; ns in dedicated ANA group already `inaccessible`; enable | export live, inaccessible |
| `ATTACH_A` | A connects C | 2 paths, 1 head, B opt / C inacc |
| `FENCE_B` | B0's group → inaccessible | observed inaccessible |
| `FREEZE_B` | B0 suspended (+ optional cache flush) | observed suspended |
| `ENABLE_C` | **commit `ponr=true` first**, then `enable_hydration`, then C → optimized | C optimized, I/O flowing |
| `HYDRATE` | wait on `hydrated/total`; alert on metadata `ro` | hydrated == total |
| `FINALIZE_C` | suspend → verify full while suspended → reload linear → resume; C disconnects B1 | linear table live |
| `DETACH_A` | A disconnects B path | single path to C |
| `RELEASE_B` | tear down B, drop volume lock | done |

**Point of no return** = precisely "the first write can now reach C" — commit it to etcd *before* enabling hydration. Pre-PONR failures auto-roll-back (flip B back to optimized, tear C down). Post-PONR is forward-only: C has the newest writes; B is a read-only recovery source. Runtime cross-check that rollback is still safe: clone status shows 0 hydrated regions with hydration disabled.

### Dirty-reboot recovery (falls out of the design)
- **B reboots mid-hydration:** comes back with B1 exported and B0 *absent/suspended* — never optimized. C's `ctrl_loss_tmo=-1` meanwhile stalls unhydrated reads instead of erroring; they drain when B returns.
- **C reboots mid-hydration:** reconnect B1, reassemble the clone with `metadata: reuse`, re-export with cloned identity and optimized ANA; A's internally queued I/O drains when the path returns.
- **A reboots:** reconnects both paths per spec; ANA steers I/O correctly on its own.

### Second fence
If B becomes unreachable to the manager mid-cutover but might still be alive on the fabric: instruct **A's** agent to `nvme disconnect` the B path. That removes the only writer, so the migration can proceed safely without B's cooperation.