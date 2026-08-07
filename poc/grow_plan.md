# grow.sh Plan — Extend leg0 Thin Pool with grp2

## 1. Goal

Create `grow.sh` under `poc/` that adds a third group (`grp2`) to leg0's thin
pool while host I/O is running on the exported volume. The extension grows the
pool's metadata and data devices from 2 groups to 3, resuming without I/O
errors or stuck commands. After grow, `teardown.sh all` must still clean up
every resource (including grp2) correctly.

Workflow (grow and failover are never combined):

```
setup.sh all  ->  host0_io.sh start  ->  grow.sh  ->  host0_io.sh report  ->  teardown.sh all
```

## 2. Decisions resolved (grilling interview)

| # | Decision | Choice |
|---|----------|--------|
| 1 | `create_ld` params | All mandatory: `<dn> <dn_ip> <cn> <cn_ip> <ld_id> <leg> <grp>` |
| 2 | Scope of generalization | All six: `create_ld`, `delete_ld`, `connect_ld`, `disconnect_ld`, `disarm_ld_delay`, `arm_ld_delay` |
| 3 | cn1 connects to grp2 LDs | `connect_ld` for dn0/ld0 + dn1/ld1, wrapped in disarm/arm |
| 4 | Pool extension sequence | Suspend pool (noflush) -> `dm_reload` thinmeta concat -> `dm_reload` thindata concat -> raw `dmsetup load` pool -> raw `dmsetup resume` pool |
| 5 | New sizes | Inline in grow.sh: `meta_sz=$((SEC_TMETA * 3))`, `data_sz=$((SEC_TDATA * 3))` |
| 6 | Teardown robustness | Dynamic discovery (Option B) via two new Part B helpers (Option i) |
| 7 | I/O verification | Follows failover.sh pattern -- user starts/stops I/O, grow.sh does NOT call host0_io.sh |
| 8 | Failover compatibility | None -- grow and failover are mutually exclusive |
| 9 | Post-checks | Existing verification mechanisms suffice (`host0_io.sh report`, `_t`/`slow_summary`) |

## 3. Files changed

### 3.1 `common.sh` -- generalize six functions

Each function drops its `for leg in 0 1; for grp in 0 1` loop and takes `leg`
and `grp` as explicit params at the end of the signature.

**`create_ld <dn_name> <dn_ip> <cn_name> <cn_ip> <ld_id> <leg> <grp>`**

Creates one (leg,grp) pair's full symmetric fault-injection stack:
- LV `-real` in `dnv-<dn>-sp0-vg` (the backing store)
- `-err-cn0`, `-err-cn1` (dm-error)
- `-delay-cn0`, `-delay-cn1` (dm-delay on -err)
- `-cn<C>` (dm-linear on -real for cn0, on -delay-cn1 for cn1)
- nvmet subsystem `nqn...:dn:<dn>:sp0-leg<leg>-grp<grp>-ld<ld>-cn<C>`

The orchestrator calls it once per (dn, cn, ld, leg, grp) combination.

**`delete_ld <dn_name> <dn_ip> <cn_name> <cn_ip> <ld_id> <leg> <grp>`**

Removes one (leg,grp) pair's dm stack + both cn nvmet subsystems + LV.
Note: `cn`/`cn_ip` params are vestigial (the function removes subsystems for
both cns regardless); kept for signature consistency with `create_ld` and
used only in the log message.

**`connect_ld <cn> <cn_ip> <dn> <dn_ip> <ld_id> <hostnqn> <hostid> <leg> <grp>`**

Connects one (leg,grp) NQN from this dn. Previously looped 4 NQNs; now one.

**`disconnect_ld <cn> <cn_ip> <dn> <dn_ip> <ld_id> <leg> <grp>`**

Disconnects one (leg,grp) NQN.

**`disarm_ld_delay <dn> <dn_ip> <leg> <grp>`**

Disarms the 4 delay devices (`lid 0 1` x `cn cn0 cn1`) for this (leg,grp)
pair. Still loops `lid`/`cn` internally -- disarming a group means disarming
all its delay devices.

**`arm_ld_delay <dn> <dn_ip> <leg> <grp>`**

Re-arms the same 4 delay devices.

### 3.2 `common.sh` -- two new Part B helpers (teardown safety nets)

Matching the `dnv_remove_all_dm` pattern (scan-and-remove, topology-agnostic):

**`nvmet_remove_all_dnv_subsys`**

Lists `$NVMET/subsystems/`, matches `nqn.2026-07.org.dnv:dn:*` prefix, removes
each via `nvmet_remove_subsys`. Called by `delete_pd` before `nvmet_remove_port`.

**`nvme_disconnect_all_dnv_ld`**

Scans `/sys/class/nvme/*/subsysnqn` for NQNs matching
`nqn.2026-07.org.dnv:dn:*`, disconnects each via `nvme_disc`. Replaces
`teardown_cn`'s hardcoded `for leg in 0 1; for grp in 0 1; for d in 0 1` loop.

### 3.3 `setup.sh` -- update call sites

**`setup_dn`**: loop `for leg in 0 1; for grp in 0 1`, call `create_ld` with
explicit `leg`/`grp` for both cn0 and cn1.

**`setup_cn1`**: loop `for leg in 0 1; for grp in 0 1` for `disarm_ld_delay`
and `arm_ld_delay`. `create_cntlr_standby` (which calls `connect_ld`
internally) is updated to loop and pass `leg`/`grp`.

**`verify` path**: disarm/arm loop updated similarly.

### 3.4 `teardown.sh` -- use dynamic discovery

**`teardown_dn`**: loop `for leg in 0 1; for grp in 0 1` for `delete_ld`
(explicit params). `delete_pd` now also calls `nvmet_remove_all_dnv_subsys`
internally.

**`teardown_cn`'s inline cleanup**: replace hardcoded LD disconnect loop with
`nvme_disconnect_all_dnv_ld`.

**`delete_cntlr_active`/`delete_cntlr_standby`**: these still hardcode
`for grp in 1 0` / `for grp in 0 1`. They remain as the "happy path" (they
handle the 2-group base correctly and are fast). The safety nets
(`dnv_remove_all_dm` for dm, `nvme_disconnect_all_dnv_ld` for LD connections,
`nvmet_remove_all_dnv_subsys` for nvmet) catch any stragglers (grp2+).

### 3.5 `grow.sh` -- new script

```
#!/bin/bash
set -uo pipefail
source "$(dirname "$0")/common.sh"

LEG=0
GRP=2

main() {
    # Precondition: base stack must exist (dnv-cn0-sp0-leg0-thinpool on cn0)

    # Step 1: Create grp2 LDs on dn0 and dn1 (full symmetric stack for both cns)
    create_ld dn0 "$DN0_IP" cn0 "$CN0_IP" 0 "$LEG" "$GRP"
    create_ld dn0 "$DN0_IP" cn1 "$CN1_IP" 0 "$LEG" "$GRP"
    create_ld dn1 "$DN1_IP" cn0 "$CN0_IP" 1 "$LEG" "$GRP"
    create_ld dn1 "$DN1_IP" cn1 "$CN1_IP" 1 "$LEG" "$GRP"

    # Step 2-3: Disarm, cn1 connects to grp2 LDs, arm
    disarm_ld_delay dn0 "$DN0_IP" "$LEG" "$GRP"
    disarm_ld_delay dn1 "$DN1_IP" "$LEG" "$GRP"
    connect_ld cn1 "$CN1_IP" dn0 "$DN0_IP" 0 "$HOSTNQN_CN1" "$HOSTID_CN1" "$LEG" "$GRP"
    connect_ld cn1 "$CN1_IP" dn1 "$DN1_IP" 1 "$HOSTNQN_CN1" "$HOSTID_CN1" "$LEG" "$GRP"
    arm_ld_delay dn0 "$DN0_IP" "$LEG" "$GRP"
    arm_ld_delay dn1 "$DN1_IP" "$LEG" "$GRP"

    # Step 4: cn0 aggregates grp2 (create_grp connects LDs + builds raid1 + slices)
    create_grp cn0 "$CN0_IP" sp0 "$LEG" "$GRP" "$HOSTNQN_CN0" "$HOSTID_CN0"

    # Step 5: Extend leg0 thin pool
    # Inline SSH block on cn0:
    #   meta_sz = SEC_TMETA * 3 = 49152  (24M)
    #   data_sz = SEC_TDATA * 3 = 2998272  (1464M)
    #
    #   a. dmsetup suspend --nolockfs --noflush leg0-thinpool  (queues host I/O)
    #   b. dm_reload leg0-thinmeta  (3-way concat: grp0 + grp1 + grp2 thinmeta)
    #   c. dm_reload leg0-thindata  (3-way concat: grp0 + grp1 + grp2 thindata)
    #   d. dmsetup load leg0-thinpool  (new table, size = data_sz, no re-suspend)
    #   e. dmsetup resume leg0-thinpool  (drains queued host I/O)
}
main "$@"
```

#### Exact concat tables

Current `leg0-thinmeta` (2-way, size = SEC_POOL_META = 32768 = 16M):
```
0 16384 linear /dev/mapper/dnv-cn0-sp0-leg0-grp0-thinmeta 0
16384 16384 linear /dev/mapper/dnv-cn0-sp0-leg0-grp1-thinmeta 0
```

New `leg0-thinmeta` (3-way, size = 49152 = 24M):
```
0 16384 linear /dev/mapper/dnv-cn0-sp0-leg0-grp0-thinmeta 0
16384 16384 linear /dev/mapper/dnv-cn0-sp0-leg0-grp1-thinmeta 0
32768 16384 linear /dev/mapper/dnv-cn0-sp0-leg0-grp2-thinmeta 0
```

Current `leg0-thindata` (2-way, size = SEC_POOL_DATA = 1998848 = 976M):
```
0 999424 linear /dev/mapper/dnv-cn0-sp0-leg0-grp0-thindata 0
999424 999424 linear /dev/mapper/dnv-cn0-sp0-leg0-grp1-thindata 0
```

New `leg0-thindata` (3-way, size = 2998272 = 1464M):
```
0 999424 linear /dev/mapper/dnv-cn0-sp0-leg0-grp0-thindata 0
999424 999424 linear /dev/mapper/dnv-cn0-sp0-leg0-grp1-thindata 0
1998848 999424 linear /dev/mapper/dnv-cn0-sp0-leg0-grp2-thindata 0
```

New `leg0-thinpool` table (size = 2998272):
```
0 2998272 thin-pool /dev/mapper/dnv-cn0-sp0-leg0-thinmeta /dev/mapper/dnv-cn0-sp0-leg0-thindata 128 0 1 skip_block_zeroing
```

### 3.6 `note.md` -- update to reflect grow.sh

Per AGENTS.md: "keep note.md and the scripts in sync." Add grow.sh to the
script table (section 11) and mention the generalized function signatures in
section 7.

## 4. Not changed

- `create_grp` -- already takes `leg`/`grp` explicitly; works for grp2 as-is.
- `create_leg` -- keeps hardcoded 2-way concat (grow and failover are never
  combined; grow.sh extends the pool inline, not via `create_leg`).
- `create_cntlr_active` -- keeps `for grp in 0 1` (only called by setup.sh
  and failover.sh, never by grow.sh).
- `failover.sh` -- unchanged. Grow and failover are mutually exclusive.
- `host0_io.sh` -- unchanged. User manages I/O lifecycle externally.

## 5. Verification

No extra checks in grow.sh. Existing mechanisms suffice:

- **I/O errors**: `host0_io.sh report` shows 0 failures (user runs it after
  grow).
- **Stuck commands**: `_t`/`slow_summary` in every SSH block flags anything
  over 3s.
- **All resources deleted**: `teardown.sh all` with dynamic discovery
  (`dnv_remove_all_dm` for dm, `nvme_disconnect_all_dnv_ld` for LD
  connections, `nvmet_remove_all_dnv_subsys` for nvmet) catches grp2
  resources regardless of topology.
