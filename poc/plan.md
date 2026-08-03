# t04 Plan — Distributed Block Storage POC Scripts

## 1. Goal
Create `common.sh`, `setup.sh`, `teardown.sh`, `failover.sh`, `host0_io.sh`, `servers.txt`
in `/home/yupeng/Code/distributed-nvme/failover/t04/`, modeled on `../t03/` but with
renamed terminology, a snap/exp abstraction, and a flat `common.sh` API. Then test
against the t03 servers.

The scripts must be easy to be understood by both human and LLM, easy to describe the
concepts of the system, easy to describe how to create such a system. We will refer
these scripts to build a true distributed block storage system.

## 2. Terminology (t03 -> t04)
| t04 term | meaning | t03 equivalent |
|---|---|---|
| `pd` | physical disk (loop bdev on a dn) | 4G loop file |
| `vd` | virtual disk, dn->cn via nvmeof | `ld0`/`ld1` + `-cn0`/`-cn1` exports |
| `grp` | thin-pool underlying group (raid1 slice pair) | `grp0`/`grp1` (unchanged) |
| `leg` | raid0 underlying disk | `stripe0`/`stripe1` |
| `side` | raid1 underlying vd | `ld0`/`ld1` (raid1 "leg" comments) |
| `da` | disk array (container, N legs, thin pools, snaps) | `vol0` |
| `snap` | snapshot id in a da (shared across all legs) | hardcoded `thin 23` |
| `exp` | exporter (raid0 of snap thin-devs + nvmeof export) | `${vp}` exported volume |

Node roles:
- `dn`  = disk node (linux server with physical disks)
- `cn`  = controller node (linux server exporting nvmet targets to nvme hosts)

**Renames:** `vol`->`da` everywhere (NQN, device names); `stripe`->`leg`; raid1 "leg"
comments->"side". `RAID0_CHUNK_SECTORS` stays (it is a chunk size, not a leg).

## 3. Resource naming
- Host NQN: `nqn.2026-07.org.dnv:da:da0:snap0:exp0`
- DN linear/vd: `dnv-<dn>-<da>-leg<leg>-grp<grp>-vd<vd>-cn<cn>` (+ `-real`,
  `-err-<cn>`, `-delay-<cn>`)
- CN raid1: `dnv-<cn>-<da>-leg<leg>-grp<grp>-raid1-side<side>`
  (+ `-meta-side0`/`-data-side0`/...)
- CN thin pool: `dnv-<cn>-<da>-leg<leg>-thinpool`
  (+ `-thinmeta`/`-thindata`/`-grp<grp>-thinmeta`/`-grp<grp>-thindata`)
- CN exp: `dnv-<cn>-<da>-snap<id>-exp<id>` (+ `-real`/`-error`/`-delay`)
- DN-side fault injection: symmetric (both cn0 and cn1 get `-real`/`-err`/`-delay`/`-cn`)

## 4. `common.sh` structure (sourced by setup/teardown/failover)

### Part A - Resource API
Idempotent: existence checks, skip-if-correct writes.

```
create_pd    <dn_name> <dn_ip>
delete_pd    <dn_name> <dn_ip>
create_vd    <dn_name> <dn_ip> <cn_name> <cn_ip> <vd_id>   # full symmetric fault-injection pair + nvmet export
delete_vd    <dn_name> <dn_ip> <cn_name> <cn_ip> <vd_id>
connect_vd    <cn_name> <cn_ip> <dn_name> <dn_ip> <vd_id> <hostnqn> <hostid>
disconnect_vd <cn_name> <cn_ip> <dn_name> <dn_ip> <vd_id>
create_grp   <cn_name> <cn_ip> <da> <leg> <grp> <hostnqn> <hostid>   # connects vds internally + builds raid1 + thinmeta/thindata
delete_grp   <cn_name> <cn_ip> <da> <leg> <grp>
create_leg   <cn_name> <cn_ip> <da> <leg>                           # concat grps -> thinpool -> create_snap 0 0
delete_leg   <cn_name> <cn_ip> <da> <leg>
create_da_active   <cn_name> <cn_ip> <da> <nlegs> <hostnqn> <hostid>   # calls create_grp+create_leg for all legs
create_da_standby  <cn_name> <cn_ip> <da> <nlegs> <hostnqn> <hostid>   # calls connect_vd for all the da's vds (no stack)
delete_da_active   <cn_name> <cn_ip> <da>
delete_da_standby  <cn_name> <cn_ip> <da>
create_snap  <cn_name> <cn_ip> <da> <new_id> <src_id>   # create_thin (id 0,src 0) OR create_snap (src>0) across ALL leg pools
delete_snap  <cn_name> <cn_ip> <da> <id>
create_exp_active  <cn_name> <cn_ip> <da> <snap_id> <host_nqn> <host_id> <cntlid_min> <cntlid_max>   # raid0+real+error+delay+exp, ANA optimized
create_exp_standby <cn_name> <cn_ip> <da> <snap_id> <host_nqn> <host_id> <cntlid_min> <cntlid_max>   # error+delay+exp stub, ANA inaccessible
delete_exp_active  <cn_name> <cn_ip> <da> <snap_id>
delete_exp_standby <cn_name> <cn_ip> <da> <snap_id>
```

Sizes (`SEC_LD`, `SEC_RMETA`, etc.) and `NQN_PREFIX`, `NVME_PORT` are constants defined
once at the top of `common.sh` (not per-call params).

### Part B - Infrastructure helpers
- `prep_node` (udev rule + modprobe + configfs)
- `set_host_identity`
- `disarm_vd_delay`/`arm_vd_delay` (dn-side delay-cn1 disarm/arm)
- `dm_create`/`dm_reload`/`dm_exists`/`dm_remove`/`dm_defuse_delay`/`dnv_remove_all_dm`
- `cfg_set`/`nvmet_add_subsys`/`nvmet_port`/`nvmet_link`/`nvmet_referral`/`nvmet_remove_*`
- `nvme_dev_by_nqn`/`nvme_wait_dev`
- `_t`/`slow_summary`
- `rd_ok`/`rd_eio`/`rd_hang`/`verify_summary`
- `stas_install`/`stas_write_config`/`stas_start`/`stas_stop_restore`

## 5. `setup.sh`
Same 6-node topology and sequence as t03, using `common.sh` primitives:
- dn0/dn1: `create_pd` -> `create_vd` (x4 vds per dn: leg0/1 x grp0/1, each for cn0 and cn1)
- cn0: `create_grp` x4 -> `create_da_active cn0 da0 2 <hnqn> <hid>`
  -> `create_exp_active cn0 da0 0 host0... 1 255`
- cn1 namespace-scan coordination: `disarm_vd_delay` dn0/dn1
  -> `create_da_standby cn1 da0 2 <hnqn> <hid>` -> `arm_vd_delay` dn0/dn1
- cn1: `create_exp_standby cn1 da0 0 host0... 256 511`
- ref0: anchor subsystem + 2 referrals (infra helpers, unchanged from t03)
- host0: nvme-stas install/config/start, multipath verify, write/read-back
  (unchanged from t03, NQN updated)
- `setup.sh verify` carries over with renamed devices; disarm/arm dance around
  cn1 vd reads.

## 6. `teardown.sh`
Phase 0 defuse (infra `dnv_defuse_all`) -> ref0 unexport -> host0 wait-disconnect +
`stas_stop_restore` -> cn1 -> cn0 (via `delete_exp_*` -> `delete_da_*` ->
`delete_grp`/`delete_leg` -> `disconnect_vd`) -> dn0/dn1 (`delete_vd` -> `delete_pd`)
-> ref0. Reverse recipe using `common.sh` `delete_*`. Idempotent, safe on
partial/clean systems.

## 7. `failover.sh`
Keeps t03's inline dmsetup for suspend/reload/repoint steps (table swaps are not
full create/delete), BUT inline commands must produce resources exactly matching
`common.sh`'s names/tables/sizes. Uses `create_da_active` for cn1's step-6 rebuild.
7-step structure preserved:

1. cn1: suspend exp, ANA->optimized
2. ref0: drop cn0 referral (runs in `force` too)
3. cn0: ANA->inaccessible, flushing-suspend, repoint exp->delay early, dismantle
   stack (commits snap-0 metadata)
4/5. dn0/dn1: swap `-cn0`->delay, `-cn1`->real
6. cn1: `create_da_active` (rebuild, `create_snap cn1 da0 0 0` should fail =>
   inherited), repoint exp->real, resume
7. cn0: exp->delay

- `force` mode: skip 3 and 7. Snap-0 inheritance invariant preserved.

## 8. `host0_io.sh` + `servers.txt`
- `host0_io.sh`: copy t03 verbatim, change only `NQN_VOL` to
  `nqn.2026-07.org.dnv:da:da0:snap0:exp0`.
- `servers.txt`: copy t03 verbatim (same 6 servers).
- No `note.md` upfront (written after testing).

## 9. Testing (after build)
Run on the t03 servers per `../t03/servers.txt`:
- `./setup.sh all` (expect exit 0, idempotent re-run exit 0)
- `./setup.sh verify` (expect all device checks pass)
- `./host0_io.sh start` -> `./failover.sh` -> `./host0_io.sh mark`/`report`
  -> `./teardown.sh all`
- Verify: 0 I/O failures, anchor matches post-failover, all nodes clean
  post-teardown.

## 10. Comment cleanup
Remove/replace all raid1 "leg" comments -> "side":
- setup.sh:61-62 (`dm-raid1 metadata leg` / `dm-raid1 data leg`)
- setup.sh:699 (`split each leg`)
- setup.sh:705 (`across both legs`)
- failover_cn0_to_cn1.sh:439 (`Both legs hold identical data`)

Rename `stripe`->`leg` in names and comments throughout.

## 11. Decision log (resolved during grilling)

1. **snap/exp model** -- `create_exp` takes a snap/dev id; setup always passes 0.
   Snap 0 is the live writable origin.
2. **raid1 side naming** -- use "side" (mirror conflicts with future raid5/6).
3. **da scope** -- one da is the container for the whole volume. A da declares N legs
   (2 in t03). A snap is a set of N thin-device ids (one per leg's pool). An exp
   creates N thin devices (one per leg's snap id) + a raid0 of them, and exports
   that raid0 to a host.
4. **snap id coordination** -- shared snap id across all legs. `snap_create` messages
   every leg pool with the same id.
5. **common.sh function shape** -- each function takes a node argument and SSHes
   internally. The orchestrator reads like a recipe.
6. **resource naming** -- `da` replaces `vol` everywhere; leg/side/grp/-cn retained.
   Host NQN: `...:da:da0:snap0:exp0`.
7. **vd fault-injection pair** -- symmetric: both cns get full `-real`/`-err`/`-delay`/`-cn`
   stack. Create all resources that might be used in the future.
8. **create_grp layering** -- `create_grp` owns the raid1 + thinmeta/thindata slices
   (one grp = one mirrored slice pair). `create_da` assembles grps into a per-leg
   thin pool.
9. **create_leg vs create_da** -- `create_da` orchestrates (calls `create_leg` per
   leg); `create_leg` assembles its grps into a thin pool + creates default snap 0;
   `create_grp` is the leaf.
10. **create_exp** -- builds the full stack: per-leg thin devices from the snap id ->
    raid0 (`-real`) -> `-error`/`-delay`/`-exp` (exported linear) -> nvmet export to
    host, with ANA state + cntlid inferred from the cn's role.
11. **snap primitives** -- one `create_snap` function handles both `create_thin`
    (default empty origin, id 0) and `create_snap` (derived). `delete_snap` issues
    `delete <id>` to all pools.
12. **connect_vd/disconnect_vd** -- added to the API. `create_grp` calls `connect_vd`
    internally for both cns; on cn1 the caller still invokes `connect_vd`. The
    disarm/arm coordination for cn1's namespace-scan hangs lives in setup.sh around
    the connect, not in `common.sh`.
13. **active/standby split** -- `create_da_active`/`create_da_standby` and
    `create_exp_active`/`create_exp_standby` (+ deletes). `create_da_active` calls
    `create_grp` (builds the stack); `create_da_standby` calls `connect_vd` (holds
    connections, no stack). `create_exp_active` builds the full raid0+real+export
    stack (ANA optimized); `create_exp_standby` builds only the error/delay/exp stub
    (ANA inaccessible). Failover step 6 builds the active stack on cn1 via
    `create_da_active` + repointing the exp.
14. **failover sequence** -- 14a option 2: failover keeps t03's inline-dmsetup style,
    but inline commands must produce resources exactly matching `common.sh`'s
    names/tables/sizes. 14b option 1: snap-0 inheritance invariant preserved
    (`create_snap cn1 da0 0 0` should fail => metadata inherited).
15. **config** -- functions take IPs/NQNs as explicit parameters. Sizes and
    `NQN_PREFIX`/`NVME_PORT` are constants in `common.sh`.
16. **signatures** -- 16a: pass `hostnqn`/`hostid` on every cn-side call. 16b: pass
    `cntlid_min`/`cntlid_max` explicitly on `create_exp_*`; ANA inferred from
    active/standby. 16c: `vd_id`/`snap_id` are plain integers.
17. **setup topology** -- full t03 topology carries over: 6 nodes, ref0 anchor trick,
    nvme-stas config, host0 verify, udev rule, `dnv_prep_node`, disarm/arm
    coordination. Only resource names and snap/exp layering change.
18. **infra helpers location** -- one `common.sh` with two sections: the resource API
    on top, infrastructure helpers below. setup/teardown/failover `source ./common.sh`
    once.
19. **idempotency + verify** -- both carry over. `create_*`/`delete_*` check existence
    and skip/no-op; `cfg_set`/`nvmet_link` stay no-op-on-match; `setup.sh verify` runs
    the renamed device checks; `teardown.sh` stays safe on partial/clean systems.
20. **teardown/failover inline vs API** -- asymmetric: teardown uses `common.sh`'s
    `delete_*` primitives (reverse recipe); failover inlines table-swap steps but
    calls `create_da_active` for step 6's rebuild.
21. **delivery artifacts** -- 21a: copy host0_io.sh + change NQN; 21b: copy
    servers.txt; 21c: no note.md upfront (write it after testing).
