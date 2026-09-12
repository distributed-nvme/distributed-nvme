# cnagent_integtest.md — integration test plan for `dnv-agent cn`

Status: **normative** for the integration test harness under `integtest/`.
Required background: `cnagent.md` (the contract under test), `dnagent.md` and
`dnagent_integtest.md` (the dn agent this suite uses as its real backing, and
the harness conventions this document extends rather than restates),
`schema.proto` (service `ControllerNodeAgent`), `architecture.md`
§3.2-§3.6/§4/§11.1/§11.3-§11.5/§13. This document is deliberately
implementation-ready: a later session (or another engineer) implements
`integtest/cnagent_test.sh` and `integtest/cnagentctl/` from it without
re-deriving any decision.

---

## 1. Goal and scope

Prove that a real `dnv-agent cn` — real nvme-tcp leg connections, md-raid1,
dm thin pools, dm-clone, nvmet configfs — behaves per `cnagent.md` on two
machines, driven over gRPC from the developer machine, **against real
`dnv-agent dn` instances** as the side backing (the dn role is implemented
and has its own passing suite; this suite asserts dn health once at setup
and then treats it as infrastructure). All 10 `ControllerNodeAgent` RPCs are
exercised (§18). Happy-path correctness only; error-path testing is out of
scope (§19) except one stale-revision probe structurally required by case D.

Test cases:

| case | name | what it proves |
|---|---|---|
| S | `smoke` | end-to-end plumbing: one primary cntlr, RedundNone, one td/ss/ns, host IO round-trip host → CN → DN |
| A | `redund` | md-raid1 groups across two DNs, primary + standby cntlrs, failover, `SP_LEVEL_READONLY` |
| B | `thinbm` | thin snapshots; `GetThinDeviceBm`/`GetLegBm` arithmetic against a known write pattern |
| C | `clone_xfer` | §11.3 transfer + clone live move across two SPs, `PushCloneBitmap`, CN wipe + §11.5 recovery |
| D | `restart` | state persistence, reconcile, idempotent re-apply |

Cases are independent: each creates its own objects (own `sp_id`s) and tears
them down. They run in the order above, fail-fast.

## 2. Deliverables and usage contract

Two new artifacts under `integtest/`, alongside the existing dn pair:

- `integtest/cnagent_test.sh` — bash, `set -euo pipefail`, the orchestrator.
- `integtest/cnagentctl/main.go` — the `ControllerNodeAgent` gRPC driver
  (§8), built by the script. It imports the repo's `pb`, `agent` (for
  `ParseCloneStatus`) and `common` (name helpers, `CnMdDevName`) packages.

The script also builds and uses the existing `integtest/dnagentctl` to drive
the dn agents (side setup, primary flips). It shares the dn suite's harness
conventions — echo prefixes, stage lines, trace-id scheme `it-<case>-<step>`,
fail-fast + diagnostics — and this document only spells out what differs.

Usage:

```
bash integtest/cnagent_test.sh [--only <case>] [--cleanup-only] \
    user1@192.168.10.12 user2@192.168.10.13
```

Same contract as the dn suite: two ssh targets (passwordless ssh + sudo),
`--only <case>` (`smoke|redund|thinbm|clone_xfer|restart`), `--cleanup-only`,
no prompts, exit 0 on success; best-effort cleanup at **start always**, at
**end only on success** — on failure all debris and logs stay for inspection
and diagnostics are dumped (§17).

## 3. Topology

```
 driver (dev box, amd64 linux)
   | ssh + scp                     gRPC dn :29528 / cn :29529 (plaintext)
   v                               v
 VM1 = DN1 (0x1) + CN1 (0x11) --- VM2 = DN2 (0x2) + CN2 (0x12)
   one shared nvmet tcp port :4200   one shared nvmet tcp port :4200
   loop dev on 2G file = dn --disk   loop dev on 2G file = dn --disk
```

- **Each VM runs both agents**: `dnv-agent dn` (gRPC `<ip>:29528`) and
  `dnv-agent cn` (gRPC `<ip>:29529`, the §13 port convention). That is the
  only way two VMs yield the 2 DNs + 2 CNs that raid1 and failover need.
- **The two agents co-own the VM's single nvmet port.** Both are launched
  with identical `--tr-*` flags, so both converge the *same*
  `ports/{NvmetPortId}` with the same attributes and the same three [D4]
  ANA groups; `EnsurePort` is probe-first (SH16/SH19), so whichever agent
  runs first creates the port and the other issues zero writes. Their
  subsystem sets are disjoint (dn: `:2:`/`:3:` NQNs; cn: the test's
  host-facing NQNs + `:4:`), neither ever removes the port, and only
  cleanup tears it down. Production never co-locates the roles; the suite
  accepts the co-ownership because the shared-mechanism rules make it
  exactly convergent.
- The **host** role is emulated with plain
  `nvme connect --hostnqn nqn.2024-01.io.dnv-it:host:0 --hostid <host-id of
  that hostnqn>` from the VMs — the `--hostid` is mandatory, since each VM
  also runs a dn and a cn agent connecting under their own hostnqns and the
  kernel allows one hostnqn per hostid
  (cross-connected: the case says which VM plays host). The driver machine
  never touches the data path.
- The cn agent's own leg connections run VM-locally (CN1 → DN1 on the same
  VM) and cross-VM (CN1 → DN2) over nvme-tcp; loopback connections are
  ordinary nvme-tcp and need nothing special.
- Agents run as root; every remote command is `ssh <vm> sudo …`.

Per-VM layout (all under `WORK=/var/tmp/dnv-cn-integtest` — deliberately not
the dn suite's directory, so the suites never share debris):

```
/var/tmp/dnv-cn-integtest/
  backing.img       # 2 GiB, fallocate'd (the dn --disk backing)
  dn-store/         # dn agent --local-store (must pre-exist)
  cn-store/         # cn agent --local-store (must pre-exist)
  dnv-agent         # scp'd binary (one binary, both roles)
  dn-agent.log      # nohup'd dn stdout (JSON log records)
  cn-agent.log      # nohup'd cn stdout
  pattern-*.bin     # test data files (on the host-role VM)
```

The driver keeps a directory of the same path locally, holding only the §8
request files (`req-<case>-cn<n>.json`): `cnagentctl` runs on the driver and
reads them there, so they are never copied to a VM.

Agent launch lines (VM1 shown; VM2 identical with its own IP):

```
setsid nohup $WORK/dnv-agent dn \
  --grpc-network tcp --grpc-address <ip1>:29528 \
  --tr-type tcp --adr-fam ipv4 --tr-addr <ip1> --tr-svc-id 4200 \
  --local-store $WORK/dn-store \
  --disk <loopdev> >> $WORK/dn-agent.log 2>&1 < /dev/null &

setsid nohup $WORK/dnv-agent cn \
  --grpc-network tcp --grpc-address <ip1>:29529 \
  --tr-type tcp --adr-fam ipv4 --tr-addr <ip1> --tr-svc-id 4200 \
  --local-store $WORK/cn-store \
  --capacity 1099511627776 >> $WORK/cn-agent.log 2>&1 < /dev/null &
```

Both lines follow the dn suite's form for the same reasons (§3 there):
`setsid` puts the agent in its own session so the exiting ssh command cannot
signal it with the channel's process group, and `< /dev/null` keeps it off
the ssh stdin. The redirect **appends**, which is what makes the `mv` before
every relaunch load-bearing — §14 step 2's `cn-agent.pre-restart.log` and
§13 stage 6's `cn-agent.pre-wipe.log`: case D step 5 asserts the
*post*-restart log holds no mutating operation, and stage 6 reads the §11.5
recovery order out of the fresh log, so without the move the re-opened file
would still carry every earlier `dmsetup create` and `nvme connect`.

**Process control gotcha:** both processes are named `dnv-agent`, so the dn
suite's `pkill -x dnv-agent` would kill both. This suite always uses
`pkill -f 'dnv-agent dn'` / `pkill -f 'dnv-agent cn'` to address one role.
The kill helper escalates: SIGTERM, then up to 5 s of `pgrep` polling, then
SIGKILL, and it reports the process as still running if even that fails —
a stop that silently left an agent alive would have the old process racing
the relaunched one for the same store and the same kernel objects, so the
stage must fail instead.

## 4. Assumptions and preflight checks

The dn suite's hard assumptions carry over (same lab image, nvme native
multipath, dm-clone), and so does its **uutils dd rule** verbatim: never
`iflag=`/`oflag=`; writes `conv=fsync`; reads after
`sync; echo 3 > /proc/sys/vm/drop_caches`; block-unit `bs=1M skip/seek/count`
only.

Preflight (fail fast, installs nothing; driver half first, per-VM half after
the start-of-run cleanup; one per-VM item — the loop device's
`write_zeroes_max_bytes` — necessarily runs later, inside §7 setup right
after `losetup`, because it needs the device to exist, and fails the run with
its own message, `vm<n>: <loop> reports write_zeroes_max_bytes=0`, rather
than the `missing: <what> on <vm>` form the sudo and binary checks use:
nothing is absent there, a present device lacks a capability):

- driver: `go`, `ssh`, `scp`, `sha256sum`, jq (a system `jq` is used as-is;
  without one, the gitignored `gojq` fallback is built at the pinned
  `github.com/itchyny/gojq/cmd/gojq@v0.12.17`); repo builds (`make build`).
- per VM, via `ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new`:
  - `sudo -n true`.
  - binaries: everything the dn suite checks (`dmsetup`, `nvme`, `losetup`,
    `blkdiscard`, `lsblk`, `dd`, `fallocate`, `sha256sum`, `cmp`, `pkill`,
    `jq`, `timeout`) **plus** `mdadm`, `truncate`, `stat`, `findmnt` and
    `thin_dump` (thin-provisioning-tools — `GetThinDeviceBm`/`GetLegBm` and
    the §11.5 recovery shell out to it). **No LVM binary**: since
    [D14] the cn agent runs no LVM command either, so the
    dn suite's "no `lvm`" rule ([D13]) is now repo-wide — the clone-metadata
    arena is a slot allocator of kind-`b` dm-linears over one loop device.
  - `modprobe` (ignore errors, then verify): `nvmet`, `nvmet_tcp`,
    `nvme_tcp`, `nvme_fabrics`, `loop`, `dm_clone`, `dm_thin_pool`,
    `dm_flakey`, `raid1`; `/sys/kernel/config/nvmet` exists (mount configfs
    if absent); `/proc/mdstat` exists.
  - `/sys/module/nvme_core/parameters/multipath` == `Y` (leg aggregation,
    failover and the §11.3 host flip all depend on it).
  - the stock md udev rule (`64-md-raid-assembly.rules` under
    `/usr/lib/udev/rules.d` or `/lib/udev/rules.d`) exists and contains
    `SYSTEMD_READY` — the Appendix A mask the harness installs (§7) works
    by setting that env, so its absence means udev would race the agent's
    md assembly; fail preflight rather than debug that later.
  - `df /var/tmp` ≥ 3 GiB free; punch-hole probe (`fallocate -p`) — the CN
    clone-metadata allocator hole-punches every recycled unit range on the
    tmpfs-backed arena file (CN18: plain `blkdiscard`, never
    `--zeroout`), DN side provisioning writes zeros through the loop
    (§9.4) and the case C never-copied verification all rely on
    discard/Write-Zeroes reaching the backing file; `MemAvailable` ≥ 1.5 GiB
    (the 2 GiB tmpfs mount is lazily allocated and the 1 GiB
    `CnCloneMetaAreaSize` arena file is sparse, so real usage is a few MiB —
    the floor just keeps a swap-thrashing VM out of the suite). The arena
    loop must honor `blkdiscard` for the allocator's recycled-unit guard to
    hole-punch: it is a **lab prerequisite**, not an agent gate — the agent
    has no CN-side preflight for it and simply reports the clone
    `RES_STATUS_ERROR` with the `blkdiscard` output if the kernel refuses.
  - Write Zeroes on the DN backing loop: once §7 step 3 has created it,
    assert `/sys/class/block/<loop>/queue/write_zeroes_max_bytes` is
    non-zero — `/sys/class/block`, not `/sys/block`, because that is the
    directory `agent.Dm.WriteZeroesMaxBytes` reads, so the suite and the
    agent consult the very same file. `0` means the kernel would fall back
    to writing zero pages at bulk speed, so the §9.4
    fast-Write-Zeroes assumption cannot hold and DN5 fails the node fast
    (`meta_info = RES_STATUS_ERROR "disk lacks Write Zeroes"`), which would
    surface as a confusing §7 step 6 failure instead of a clear preflight
    message. This is the same underlying capability the punch-hole probe
    tests (loop maps WRITE_ZEROES onto `fallocate` on the backing file),
    asserted directly on the device the dn agent reads.
  - ports 29528, 29529 and 4200 not listening (`ss -ltn`).

## 5. Identity plan and naming

`cluster_id = 0x1`. `dn_id`: VM1 = `0x1`, VM2 = `0x2`. `cn_id`: VM1 =
`0x11`, VM2 = `0x12`. Host NQN (emulated host role):
`nqn.2024-01.io.dnv-it:host:0`. All per-SP sub-ids are chosen to look like
one `next_id` counter (distinct within their SP); every case has distinct
`sp_id`s so cases never collide even under `--only`.

| case | sp | cntlrs (cn, slot, role) | slice / groups (redund, ext, legs→sides@DN) | td / ss / ns |
|---|---|---|---|---|
| S | 0x3a1 | 0x1 (CN1, 0, primary) | slice 0x2; meta 0x3 (none, 1, leg 0x4→side 0x5@DN1); data 0x6 (none, 2, leg 0x7→side 0x8@DN1) | td 0x9 (dev_id 1, 64 MiB); ss 0xa `nqn.2024-01.io.dnv-it:s:vol1` (allow-any); ns 0xb idx 1, uuid `11111111-1111-4111-8111-111111111111` |
| A | 0x3b1 | 0x1 (CN1, 0, primary), 0x2 (CN2, 1, standby) | slice 0x3; meta 0x4 (raid1, 1, legs 0x5→0x6@DN1, 0x7→0x8@DN2); data 0x9 (raid1, 2, legs 0xa→0xb@DN1, 0xc→0xd@DN2) | td 0xe (64 MiB); ss 0xf `…:a:vol1` (allowed_hosts = [host NQN]); ns 0x10 idx 1, uuid `22222222-…` |
| B | 0x3c1 | 0x1 (CN1, 0, primary) | as S (slice 0x2, meta 0x3/leg 0x4/side 0x5, data 0x6/leg 0x7/side 0x8, all @DN1) | td1 0x9 (dev_id 1); snapshot td2 0xc (dev_id 2, ori_id 1); ss1 0xa `…:b:vol1` / ns1 0xb; ss2 0xd `…:b:snap1` / ns2 0xe |
| C | sp1 0x3d1, sp2 0x3d2 | sp1: 0x1 (CN1, slot 0); sp2: 0x1 (CN2, slot 2) — §11.3 disjoint slots | each sp: as S, sp1 sides @DN1, sp2 sides @DN2 | each: td 0x9 (64 MiB); **shared** ss NQN `…:c:vol1` (ss_id 0xa each) and ns 0xb idx 1 with **identical** uuid `33333333-…`/nguid; sp1 xfer 0xc (auto_suspend, allowed_hosts = [`CnHostNqn(0x1, 0x12)`]); sp2 clone 0xc (auto_resume, bm_cnt 1, src = sp1's `XferNqn`) |
| D | 0x3e1 | 0x1 (CN1, 0, primary), 0x2 (CN2, 1, standby) | as A (slice 0x3, meta 0x4, data 0x9, raid1 across DN1/DN2) | td 0xe; ss 0xf `…:d:vol1`; ns 0x10 idx 1 |

`nguid` is always the uuid with the dashes removed. All sides use
`cntlid_slot 0` (sides of different legs may share slots, §11.8).

Derived names the script greps for (ids `%016x`, cn dm kinds per
`architecture.md` §4.1 + `cnagent.md` §2.1), e.g. for cluster 0x1 / CN1 /
case S:

- leg wrapper (kind 9, leg 0x7):
  `dnv-0000000000000001-0000000000000011-9-00000000000003a1-0000000000000007`
- RedundNone group (kind a), pool meta/data/final (kinds 0/1/2), thin (3),
  raid0 (4), error (5), ns-dev (6), clone-final (7), xfer-final (8),
  clone-metadata wrapper (b): same scheme with the §4.2 id suffixes. Kind
  `b` carries the `{sp}-{clone}` suffix, exactly like kind 7.
- md names (case A/D): array name `CnMdArrayName` =
  `dnv-{sp:%016x}-{sliceIdxM:%02x}-{grpIdx:%02x}` (bash-computable;
  `sliceIdxM = slice_idx | 0x80` for meta groups), node path
  `/dev/md/{CnMdDevName}` via `cnagentctl md-name` (§8 — it needs the fnv
  `getShortId`).
- clone-metadata wrapper (kind `b`, `common.CnCloneMetaDmName`):
  `dnv-{cluster}-{cn}-b-{sp}-{clone}` — a dm-linear carved out of the single
  CN loop device by the CN18 slot allocator; its dm table
  (`0 {len} linear {loopdev} {offset_sectors}`) **is** the allocation
  registry, so `dmsetup table` of this name is the only thing to grep. Case
  C's is
  `dnv-0000000000000001-0000000000000012-b-00000000000003d2-000000000000000c`
  (CN2, sp2 `0x3d2`, clone `0xc`). tmpfs at `/tmp/dnv-tmpfs/{cluster}-{cn}`,
  carrying the 1 GiB sparse arena file on one loop device.
- host-side device nodes: `/dev/disk/by-id/nvme-uuid.<uuid>` (the ns uuids
  above are fixed inputs, so no helper is needed).

Cleanup can therefore target everything by prefix: dm `dnv-*` (one flat
namespace now — with LVM gone there are no `dnv--clone--vg-*` nodes with
doubled dashes left to special-case), md arrays whose mdadm name starts
`dnv-`, nvmet `nqn.2024-01.io.dnv:*` and `nqn.2024-01.io.dnv-it:*`, tmpfs
mounts under `/tmp/dnv-tmpfs/`.

## 6. Sizing and budgets

DN side — unchanged from the dn suite: 2 GiB `backing.img`, `extent_size`
67108864 (64 MiB), data area exactly **1879048192** bytes = 28 extents per
DN (the setup wait-up re-asserts the exact `GetDnSize` reply). Worst
concurrent draw is one case = 3 extents per DN (1-ext meta group + 2-ext
data group); even with `--only` skipping teardowns, all five cases total 15
extents per DN — comfortable.

CN side — `--capacity 1099511627776` (1 TiB): an arbitrary exact value the
setup asserts back from `GetCnSize`, proving the CN-CM1 plumbing; agents
never enforce budgets, so nothing else depends on it.

Bdev parameters everywhere: `block_size` 1048576 (1 MiB), `stripe_size`
65536, `bitmap_chunk_block_cnt` 128, `low_water_mark_pct` 50 (default),
`slice_cnt` 1, `dm_clone_conf {hydration_threshold 1, hydration_batch_size
1}` (slow hydration widens the case C observation windows).

Worked `meta_blocks`/`data_blocks` (§3.6; the request must carry the right
values — the agent consumes, never recomputes):

```
bitmap_chunk = 128 × 1 MiB = 128 MiB
RedundNone            : meta_blocks = 1
  1-extent group ( 64 MiB): data_blocks =  63
  2-extent group (128 MiB): data_blocks = 127
RedundMdRaid1 (bitmap_bits = 1 → bitmap_bytes = 257 → bitmap_blocks = 1)
                      : meta_blocks = 3
  1-extent group ( 64 MiB): data_blocks =  61
  2-extent group (128 MiB): data_blocks = 125
```

Thin pools: case S/B/C pools get a 63 MiB metadata dev and 127 MiB data dev
(RedundNone), case A/D 61/125 (raid1); `low_water_mark` in the pool table =
`data_blocks × 50 / 100` (63 for 127). Every td is 64 MiB = 67108864 (a
multiple of `slice_cnt × stripe_size`), 64 thin blocks — thin overcommit
against the 127-block pool is fine at these write volumes.

## 7. Setup phase (after start-cleanup)

Per VM, in order:

1. `mkdir -p $WORK $WORK/dn-store $WORK/cn-store`
2. Install the Appendix A md udev mask so only the agent assembles dnv
   arrays, then reload udev:

   ```
   cat > /etc/udev/rules.d/63-dnv-md.rules <<'EOF'
   ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
     IMPORT{program}="/sbin/mdadm --examine --export $devnode"
   ENV{MD_NAME}=="dnv-*|*:dnv-*", ENV{SYSTEMD_READY}="0"
   EOF
   udevadm control --reload
   ```

   (Removed again by cleanup — the VMs are shared lab machines.)
3. `fallocate -l 2G $WORK/backing.img`;
   `LOOP=$(losetup --find --show $WORK/backing.img)`; immediately after the
   `losetup`, run the deferred §4 preflight item — assert
   `/sys/class/block/$(basename $LOOP)/queue/write_zeroes_max_bytes != 0`
   (the file the agent itself reads), failing the run with
   `vm<n>: <loop> reports write_zeroes_max_bytes=0` — a capability report,
   not the `missing:` form of the absent-tool checks.
4. `scp bin/dnv-agent` (built once on the driver:
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build`)
5. launch **both** agents (§3 command lines), as root, via nohup
6. dn wait-up: `dnagentctl get-dn-size --wait 15`, assert exactly
   `1879048192`; `dnagentctl syncup-dn` baseline (revision 1, extent_size
   67108864, empty side list), assert code 0 and all three `dn_info`
   statuses OK — the dn backing is healthy before any cn assertion runs.
7. cn wait-up: `cnagentctl get-cn-size --wait 15`, assert exactly
   `1099511627776` (CN-CM1; `GetCnSize` is lock-free, so this doubles as
   the liveness probe).
8. `cnagentctl syncup-cn` baseline: revision 1, empty cntlr list; assert
   code 0 and `cn_info.{port,tmpfs,tmp_file,loop_dev}_info.status ==
   RES_STATUS_OK` — the §3.2 base state (tmpfs, sparse arena file, loop,
   shared port) converged on both VMs before any case runs. There is no
   fifth base-state info: [D14] deleted `CnInfo.clone_vg_info`
   (`reserved 5`), because `loop_dev_info` already covers the arena and
   per-clone metadata health lives in `CntlrInfo.clone_id_to_meta`.

Driver binaries `integtest/bin/dnagentctl` and `integtest/bin/cnagentctl`
are built natively on the driver.

## 8. The driver: `cnagentctl`

Plaintext gRPC (`insecure.NewCredentials()`), no server reflection — same
rationale and conventions as `dnagentctl`: global flags `--addr` (the cn
endpoint `<ip>:29529`), `--cluster`, `--cn`, `--trace-id` (metadata key
`trace_id`, script passes `it-<case>-<step>`), `--timeout` (default 10),
`0x` hex accepted on id flags, protojson replies on stdout, non-zero exit on
gRPC error **or** `agent_reply.code != 0` unless `--expect-code N`. The
script raises that deadline for the converge RPCs, which do real kernel work
under one lock: 60 s for `syncup-dn`/`syncup-side` (the dn suite's value) and
180 s for `syncup-cn`/`syncup-cntlr`, since a CN converge can create two md
arrays, a thin pool and a dm-clone in one call.

Subcommands:

| cmd | flags beyond globals | notes |
|---|---|---|
| `get-cn-size` | `--wait <sec>` | retry until success within wait; prints the size |
| `syncup-cn` | `--revision`, repeated `--cntlr sp:cntlr` | full desired cntlr list every call (declarative); never sends `qos_ratio` (deferred, cnagent.md CN6) |
| `syncup-cntlr` | `--req <file>` | reads one complete `SyncupCntlrRequest` as protojson (§ below); sanity-checks `cluster_id`/`cn_id` against the globals |
| `push-clone-bm` | `--revision`, `--sp --cntlr`, `--clone`, `--bm-idx`, `--bitmap-hex` | revision = the cntlr's current revision (gates, never advances) |
| `get-cn-info` / `get-cntlr-info` | (`--sp --cntlr`) | |
| `check-cn` / `check-cntlr` | `--revision`, `--show-info` (+ cntlr ptr) | opens the bidi stream, one round, closes |
| `get-td-bm` | `--sp --cntlr --td --slice-idx --start-block --block-cnt` | prints the bitmap as lowercase hex + a `bits=` count |
| `get-leg-bm` | `--sp --cntlr --leg --start-block --block-cnt` | idem |
| `wait-hydrated` | `--sp --cntlr --clone`, `--interval 0.5`, `--timeout 120`, `--min-first <n>`, `--sample-only` | polls `GetCntlrInfo`, parses `clone_id_to_dm_clone[clone].details` with `agent.ParseCloneStatus`; `--min-first` asserts the first sample's hydrated ≥ n; exits when hydrated == total. `--sample-only` takes exactly one sample, applies `--min-first`, prints `hydrated/total`, and exits without waiting (case C stage 5) |
| `md-name` | `--sp --slice-idx --grp-idx --meta` | prints `CnMdDevName` and `CnMdArrayName` (uses `--cluster`/`--cn` for `getShortId`) |
| `host-id` | `--hostnqn <nqn>` | prints `common.NvmeHostId(nqn)`; local, no gRPC. The emulated host's `nvme connect` passes it as `--hostid` (§3) |

**The `--req` protojson file.** `SyncupCntlrRequest` is too deep for flags;
the script writes each request as a heredoc-generated JSON file
(`$WORK/req-<case>-cn<n>.json` on the driver, passed locally — cnagentctl
runs on the driver — one file per CN for the whole case, not one per step,
so the file always mirrors that CN's current desired state) and edits
between steps only the fields that change (`revision`, `sp_level`,
`cntlr.primary`, `clone_list`/`xfer_list`, `ns_list[].suspended`).
protojson accepts original snake_case field names, 64-bit integers as
decimal strings, and enum value names; the script keeps a `d16()`
hex→decimal helper next to the dn suite's `hex16`. The complete
case S request, on CN1 with everything at `<ip1>` (revision 3: the setup
baseline `syncup-cn` used 1, case S's pointer-introducing `syncup-cn` used
2 — one monotonic counter per CN, §9):

```json
{
  "cluster_id": "1", "cn_id": "17",
  "cntlr_pointer": {"sp_id": "929", "cntlr_id": "1"},
  "revision": "3",
  "bdev_conf": {
    "dm_pool_conf": {"data_block_size": "1048576", "low_water_mark_pct": 50},
    "dm_raid0_conf": {"stripe_size": "65536"},
    "redund_conf": {"redund_none": {}}
  },
  "sp_level": "SP_LEVEL_READWRITE",
  "cntlr": {
    "addr_port": "<ip1>:29529",
    "nvme_tr_conf": {"tr_type": "tcp", "adr_fam": "ipv4",
                     "tr_addr": "<ip1>", "tr_svc_id": "4200"},
    "cntlid_slot": 0, "primary": true, "disabled": false
  },
  "id_to_slice": {
    "0000000000000002": {
      "slice_idx": 0,
      "meta_grp_list": [{
        "grp_id": "3", "ext_cnt": "1",
        "meta_blocks": "1", "data_blocks": "63",
        "leg_list": [{"leg_id": "4", "leg_idx": 0, "side_list": [{
          "side_id": "5", "addr_port": "<ip1>:29528", "cntlid_slot": 0,
          "nvme_tr_conf": {"tr_type": "tcp", "adr_fam": "ipv4",
                           "tr_addr": "<ip1>", "tr_svc_id": "4200"}}]}]
      }],
      "data_grp_list": [{
        "grp_id": "6", "ext_cnt": "2",
        "meta_blocks": "1", "data_blocks": "127",
        "leg_list": [{"leg_id": "7", "leg_idx": 0, "side_list": [{
          "side_id": "8", "addr_port": "<ip1>:29528", "cntlid_slot": 0,
          "nvme_tr_conf": {"tr_type": "tcp", "adr_fam": "ipv4",
                           "tr_addr": "<ip1>", "tr_svc_id": "4200"}}]}]
      }]
    }
  },
  "td_list": [{"td_id": "9", "dev_id": 1, "size": "67108864"}],
  "nqn_to_subsystem": {
    "nqn.2024-01.io.dnv-it:s:vol1": {
      "ss_id": "10", "serial": "000000000000000a", "model": "dnv",
      "ns_list": [{"ns_id": "11", "ns_idx": 1, "td_id": "9",
        "dev_uuid": "11111111-1111-4111-8111-111111111111",
        "dev_nguid": "11111111111141118111111111111111",
        "suspended": false}]
    }
  }
}
```

(`serial`/`model` are what the gateway would stamp per [D2]; the agent
consumes them verbatim. The `id_to_slice` key is `sprintf("%016x",
slice_id)` per §9.3.)

## 9. Conventions

- **Revisions**: one monotonic counter per node — `DNREV1`/`DNREV2` for the
  dn agents (dn suite convention) and `CNREV1`/`CNREV2` for the cn agents —
  incremented before every state-changing syncup on that node; both
  `SyncupCn` and `SyncupCntlr` on one CN share its counter, mirroring the
  single `CnRev` of §5.5. The agent, however, stores what each RPC last
  applied **separately** and gates each RPC against its own stored value,
  so the script also records `CNSYNC1`/`CNSYNC2` — the counter value the
  last `SyncupCn` carried. That, not the counter, is what a `check-cn`
  round echoes back and what the case D stale probe must undercut: right
  after a `syncup-cntlr`, `CNREV - 1` equals the value the last `SyncupCn`
  stored, so a `syncup-cn` at `CNREV - 1` would be an equal-revision
  re-apply (code 0), not a rejection. Equal-revision re-sends are legal
  full re-applies and are used deliberately (fetching `bm_info_list`, case
  D idempotency).
- **Ordering**: `syncup-cn` introduces a cntlr pointer before its first
  `syncup-cntlr` (else `code 2`); a side pointer likewise via `syncup-dn`
  (dn suite rule). The DN sides of a case are converged before the cn
  primary, so legs have optimized paths when md assembles (§11.1.1).
- **Two-phase DN side setup (the worker's `provisioned` flip, played by the
  script).** A side is exported only when the
  request carries `provisioned = true` **and** every extent's `zeroed_bits`
  bit is set, so every `dnagentctl syncup-side` that *creates* a side is
  issued twice:
  (a) `--provisioned=false` — the dn agent allocates the extent runs, builds
      `DnSideName` and starts the background zeroing goroutine;
      `side_dev_info` reports `RES_STATUS_PROVISIONING` — or already
      `RES_STATUS_OK` when the goroutine won the race
      (`assert_provisioning_or_ok`, the genuinely two-valued phase-(a) check
      the Status-assertions bullet below blesses; the `zeroing k/n` details
      string is not asserted) — and every
      `cn_id_to_dm_error/linear/nvmeof` entry reports
      `RES_STATUS_PROVISIONING` — **not** OK, and not absent;
  (b) `dnagentctl wait-zeroed` until `zeroed_ext_cnt == total_ext_cnt`,
      polled every 0.5 s with a 120 s budget — orders of magnitude above the
      sub-second `fallocate` path below, because the budget only has to cover
      a side that hit `DnZeroRetryInterval` (5 s) retries;
  (c) `DNREV<dn>++` (the `DNREV[dn]` counter of the Revisions bullet) and
      re-send the identical request with
      `--provisioned=true` (the flip), after which `side_dev_info` and every
      per-CN entry are `RES_STATUS_OK` and the exports exist.
  The helper is `dn_side()`, which memoizes each side in a
  `SIDE_PROVISIONED` map and runs phases (a)-(c) only on first use: every
  *later* call for that side — re-sends, the case A failover flip — must
  keep `--provisioned=true`. That memoization is load-bearing, not an
  optimization: a blind two-phase re-run at the failover flip would send
  `--provisioned=false` and retract live exports mid-failover (row 3
  of the §9.4 converge matrix makes that a legal instruction to retire them, not
  a no-op). `RES_STATUS_PROVISIONING` never sets `err_epoch`; it means
  *healthy, not ready, no action*. With 64 MiB extents on loop devices a
  `DnZeroBatchExtCnt = 10` batch is 640 MiB and loop maps Write Zeroes onto
  `fallocate`, so phase (b) is normally instant; `--zeroout` on loop
  materializes at most ~1 GiB of backing pages per DN, well inside the §4
  3 GiB `df` floor (and in practice nothing, since `backing.img` is
  `fallocate -l 2G`-preallocated). Sample once before (b) and log
  `provisioning window HIT` when `zeroed < total`, else a tolerated
  `window missed`, in the same tolerant style as case C's stage 5
  read-through probe.
- **Status assertions are exact.** `RES_STATUS_PROVISIONING` is a *fourth*
  healthy-ish status, so a bare "not OK" assertion silently accepts it. Any
  assertion that means "must be ERROR" compares against `RES_STATUS_ERROR`
  explicitly, and the genuinely two-valued phase-(a) checks use
  `assert_provisioning_or_ok` — never `assert_not_ok`.
- **Failover script order** (case A step 4; the safe serialization of the
  §11.1 revision fan-out): demote the old primary (`syncup-cntlr`
  `primary=false`) → flip every side (`syncup-side` with the new
  `primary_cn_id`) → **wait for the new primary's path to every one of the
  SP's legs to report ANA `optimized`** → promote the new primary. That
  barrier is not cosmetic: the flip is asynchronous, and a promote that
  arrives while a leg is still `non-optimized` finds that member
  unavailable and fails the md assembly (§11.1.1). Host IO is quiesced from
  before the demote until the new primary's path reports `optimized` —
  between those points the namespace can have no serving path, and
  ANA-inaccessible paths queue IO and have no `/dev` node.
- **Converge check**: after each case reaches steady state, one `check-cn`
  and one `check-cntlr` round with `--show-info` asserts code 0, matching
  revision, and every expected `ResInfo.status == RES_STATUS_OK`. Check
  rounds only ever run at steady state, i.e. after the two-phase flip above,
  so `RES_STATUS_PROVISIONING` must never appear in one.
- **Data IO on the host VM**: through the multipath device node
  `/dev/disk/by-id/nvme-uuid.<uuid>`; writes `dd conv=fsync`, verifying
  reads after `sync; echo 3 > drop_caches`; never `iflag=/oflag=` (§4).
- `mutations([trace])` — the cn twin of the dn suite helper, used by case D
  and by case B's rebuild stage: greps a cn-agent log — the whole log, or a
  single converge when a `trace_id` is given — for `dmsetup create|reload|remove|suspend|resume|message`,
  mutating mdadm verbs
  (`--create|--assemble|--add|--fail|--remove|--stop|--zero-superblock`),
  `nvme connect|disconnect`, `blkdiscard`,
  `mount|umount|losetup|truncate`, configfs `mkdir|rmdir|ln|rm`, and every
  `os write file direct` **and** `os write block` record. No path-based
  exemption is needed: the CN11 leg health
  probers bypass the `OsClient` entirely and log their own records under the
  msgs `probe write block` / `probe read block direct`, which are not in
  this grep list **by construction** — the `-9-`-path exemption that used to
  carve those continuous probes out of `os write block` /
  `os read block direct` is therefore deleted, not repointed. With the carve-out
  `healthcheck.go` is no longer a block-IO caller at all, so a converged CN
  emits zero `os write block` records and the msg can simply be listed.
  Probe commands (`lsblk`, `dmsetup info|table|status|ls`, `ls`, `findmnt`,
  `stat`, `losetup --associated`, `mdadm --detail|--examine`,
  `nvme list-subsys`) are expected and deliberately not in the list; the LVM
  verbs that used to appear on both halves of this list
  (`pvcreate|vgcreate|lvcreate|lvremove` mutating, `vgs|lvs` probing) are
  gone with [D14].

## 10. Case S — `smoke`

1. DN side: `syncup-dn` DN1 (DNREV1++) with sides `(0x3a1, 0x4, 0x5)` and
   `(0x3a1, 0x7, 0x8)`; `dn_side` each (meta side `ext_cnt 1`, data
   side `ext_cnt 2`, slot 0, `primary_cn_id 0x11`, no standbys,
   `readwrite`), i.e. the §9 two-phase sequence
   (`--provisioned=false` → `wait-zeroed` → DNREV1++ → `--provisioned=true`).
   Assert code 0; after phase (a) each side reports `PROVISIONING`-or-OK
   (§9(a)'s two-valued race), after phase (c) both report OK.
2. CN side: `syncup-cn` CN1 (CNREV1++) with cntlr `(0x3a1, 0x1)`;
   `syncup-cntlr` CN1 with the §8 request (CNREV1++). Assert code 0 and,
   in `cntlr_info`: `leg_id_to_leg[0x4,0x7]`, `grp_id_to_md_raid[0x3,0x6]`
   (RedundNone linears), `slice_id_to_{meta,data,dm_pool}[0x2]`,
   `td_id_to_{raid0,dm_error}[0x9]`, thin `[0x9][0x2]`,
   `ns_id_to_{namespace,dm_linear}[0xb]`, `ss_id_to_subsystem[0xa]` all
   OK. On VM1: `dmsetup ls` shows the kind-9/a devices; configfs
   `attr_allow_any_host == 1` for the ss, and its `allowed_hosts/` directory
   is separately asserted empty — the request sends an empty `allowed_hosts`
   list, which the agent converges into allow-any with no host links, and the
   two assertions are the two halves of that translation (case A sends the
   host NQN instead, and its step 2 asserts the directory holds exactly it).
3. Host (VM2): `nvme connect -t tcp -a <ip1> -s 4200 -n …:s:vol1 --hostid
   <cnagentctl host-id> --hostnqn
   nqn.2024-01.io.dnv-it:host:0`; wait for
   `/dev/disk/by-id/nvme-uuid.11111111-…`; `nvme list-subsys` shows the
   path `live`, and per-path sysfs `ana_state` reads `optimized`
   (list-subsys carries no ANA state — the dn suite's §10.3 note; the
   helper walks `/sys/class/nvme`).
4. IO: write 4 MiB from a urandom pattern file, sync, drop caches, read
   back, sha256 equal — host → CN thin/raid0 stack → DN side, end to end.
5. `check-cn` + `check-cntlr` rounds (§9); one `get-cntlr-info` asserting
   the pool status `details` is the raw `dmsetup status` line: it contains
   the `thin-pool` token followed by two `used/total` fields
   (`[0-9]+/[0-9]+` twice — metadata then data), which is what the §10.4
   auto-grow parses.
6. Teardown: host disconnect (`-n`, both paths of the NQN are dying
   anyway); `syncup-cn` CN1 (CNREV1++) with an **empty cntlr list** →
   declarative cntlr teardown (CN7/CN21); `syncup-dn` DN1 (DNREV1++) empty
   side list. Assert on VM1, in this order: no `dnv-*-0000000000000011-*` dm
   devices (that one pattern covers the kind-`b` clone-metadata wrappers too
   — the allocator's units are free again exactly when the wrappers are
   gone) and no `dnv-it` subsystem in configfs — that configfs half is what
   proves the host-facing `dnv-it:*` subsystems are gone, the per-SP residue
   check of §11 step 7 matching subsystems by sp id and so unable to see
   them. The kind-`b` list is then asserted empty a second time on its own,
   because it is the only allocation registry there is (CN18:
   no on-file table), so reading it by name reports a leaked clone unit as
   an arena leak rather than as one more anonymous dm device. The base state
   then still probes OK via `get-cn-info`, and last comes the per-SP residue
   check of §11 step 7, run over both VMs. Case C runs the same sweep on
   both its CNs (§13 stage 10).

Success proves: both ctl binaries, both agents, pointer gating, the full
§3.3 primary stack on real devices, host IO, declarative teardown.

## 11. Case A — `redund` (raid1, failover, readonly)

1. DN side: 4 sides (§5 table) on DN1+DN2, each `primary_cn_id 0x11`,
   `standby_id_list [0x12]`, each through `dn_side` (the §9 two-phase
   sequence; the four `wait-zeroed`s can run concurrently). CN side:
   `syncup-cn` both CNs; `syncup-cntlr` CN1 `primary=true`, CN2
   `primary=false` (same request otherwise).
2. Assertions — primary (VM1): `/proc/mdstat` shows both arrays up
   (`[UU]`); `cn-agent.log` shows `mdadm --create … --run --assume-clean`
   for both groups (CN12 case 1 — **fresh zeroed legs**: §9.4 provisioning
   writes zeros over the whole side with `blkdiscard --zeroout` before its
   first export, which is what genuinely funds `--assume-clean`; the old
   trim never did) and never `--assemble`; `md-name` cross-checks the device
   names. Standby (VM2):
   legs connected (`nvme list-subsys` on VM2 shows the four side paths,
   ana `non-optimized` read from per-path sysfs), **no** md arrays
   (asserted as: `/proc/mdstat` holds no `[UU]` line — no assembled
   two-member array; the sp-scoped md residue check at teardown is the
   exhaustive proof), ns-dev table = error, ns
   `ana_grpid` inaccessible; configfs `allowed_hosts/` of the ss contains
   exactly the host NQN on both CNs. "ns-dev table = error" is shorthand:
   the agent never gives an ns-dev an `error` target, it repoints the
   ns-dev's own table at the td's `CnErrorName` device (CN16 rule 1) — a
   dm-linear on this `readwrite` standby, since only rule 6's readonly level
   wraps the backing in dm-flakey (step 6) — so the assertion compares the
   ns-dev's backing devno with the kind-5 dm-error's, the state CN16
   actually prescribes.
3. Host (VM2): connect the ss at **both** CN ports; one multipath device,
   CN1 path `optimized`, CN2 path `inaccessible`. Write 8 MiB pattern,
   sha, drop caches, read back.
4. **Failover** (§9 order; host IO quiesced): `syncup-cntlr` CN1
   (CNREV1++, `primary=false`) — assert from `cn-agent.log` that the
   `ana_grpid` write to `3` precedes the ns-dev reload onto error (CN9
   retire order), that `mdadm --stop` ran for both arrays, and that no
   `nvme disconnect` of a leg NQN appears (standbys keep legs). 4×
   `syncup-side` (DNREV++, `primary_cn_id 0x12`, `standby_id_list
   [0x11]`) — still `--provisioned=true` (§9: these sides are already
   memoized, so the flip changes only `primary_cn_id`/`standby_id_list`;
   dropping the flag here would retract their exports mid-failover).
   `syncup-cntlr` CN2 (CNREV2++, `primary=true`) — assert
   `mdadm --assemble` (both members carry superblocks now; CN12 case 2)
   and **no** `--create` in `cn-agent.log` on VM2.
5. Host: CN2 path → `optimized`, CN1 path → `inaccessible`; read the 8 MiB
   back (sha equal — the data crossed the failover through md), write 1
   MiB more at `seek=9`, read back.
6. **Readonly**: `syncup-cntlr` CN2 (CNREV2++, `sp_level
   SP_LEVEL_READONLY`, still primary). No dn calls — the level has no
   DN-side behavior below `NO_MIGRATION` ([D11]). Assert: VM2
   `dmsetup table` of the ns-dev shows `flakey … error_writes`; host read
   of block 9 succeeds; host `dd` write of 1 MiB fails non-zero; ana still
   `optimized`. Back to `readwrite` (CNREV2++): write succeeds again.
7. `check-cn`/`check-cntlr` rounds on both CNs; teardown: host disconnect,
   empty cntlr lists both CNs, empty side lists both DNs; assert no
   `0x3b1` residue on either VM — dm devices of the SP, md arrays
   (`mdadm --detail --scan` lists no `dnv-{sp id}-*` array — the check is
   sp-scoped, like the rest of the residue sweep), and those nvmet
   subsystems whose NQN carries the sp id, i.e. the `:2:`/`:3:`/`:4:` ones,
   which is what the check matches on. The host-facing `dnv-it:*`
   subsystems are named by the request (§5) and carry no id, so no per-SP
   check can see them; the per-CN sweep of §10 step 6 — run by case S and
   by case C on both its CNs — is what does.

## 12. Case B — `thinbm` (snapshots and bitmap reads)

Setup: the S-shaped SP `0x3c1` on DN1+CN1 (§5, sides through `dn_side` per
the §9 two-phase sequence), host = VM2, connected to `…:b:vol1`.

1. Host writes one distinct 1 MiB pattern block at td offsets
   `seek= 0, 5, 6, 7` (four writes, `conv=fsync`), sync. These are td
   virtual blocks {0,5,6,7} of 64.
2. `get-td-bm --td 0x9 --slice-idx 0 --start-block 0 --block-cnt 0` on CN1:
   assert exactly `1effffffffffffff` (64 bits, LSB-first: bits 0,5,6,7 = 0
   written, rest 1 = unmapped — the §11.4 wire inversion).
3. `get-leg-bm --leg 0x7` (the data-group leg): assert
   `f0ffffffffffffffffffffffffffff7f` — 127 data-region bits, pool-data
   blocks 0..3 mapped. This encodes the assumption that a fresh dm-thin
   pool allocates data blocks sequentially from 0; if a kernel ever breaks
   that, relax to "exactly four 0-bits" and keep the count assertion — the
   count, not the position, is the contract.
4. `get-leg-bm --leg 0x4` (the meta-group leg): assert all-zero (8 bytes
   `00…0`) — meta legs are never skippable (cnagent.md CN27).
5. Paging: `get-td-bm --start-block 4 --block-cnt 8` ⇒ one byte for blocks
   4..11 ⇒ `f1` (LSB-first: bit 0 = block 4 unmapped = 1, bits 1-3 =
   blocks 5,6,7 written = 0, bits 4-7 = blocks 8..11 unmapped = 1 ⇒
   0x01 + 0xf0). One-byte window proves the window math end to end.
6. **Snapshot**: `syncup-cntlr` CN1 (CNREV1++) adding td2 `0xc`
   (`dev_id 2, ori_id 1`) and ss2/ns2. The origin `0x9` is re-sent with
   `created = true`: the gateway refuses a snapshot of a td it has not seen
   materialized in every slice pool (`ThinDeviceCreated.md` U2-S1), so this is
   the only `td_list` a real worker could publish here, and the snapshot
   itself is uncreated, which is what puts its `create_snap` inside the
   quiesce. Assert code 0; `cn-agent.log`
   shows suspend(origin raid0)/suspend(origin thin)/`create_snap 2 1`
   message/resume(origin thin)/resume(origin raid0) in that order, and the
   snapshot's own thin device created only after that last resume (CN14).
   What the script actually anchors is each of those
   four events against the `create_snap` message alone — both suspends
   before it, both resumes after it, the snapshot create after the raid0
   resume — since that is the property the message depends on; the pairwise
   raid0↔thin nesting is left to the unit tests, which drive a two-slice SP
   and compare recorded call indices directly. This SP has one slice, so
   the log is the only on-hardware evidence of the bracket; the nesting and
   the cross-slice point-in-time property are asserted in the unit tests
   (`cnagent.md` §6 test 19).
7. Host connects `…:b:snap1`; reads blocks {0,5,6,7} of the snapshot ⇒
   sha-equal to the four pattern blocks; block 3 reads zero.
8. Host writes a new pattern block to the **origin** at `seek=9`; snapshot
   block 9 still reads zero; `get-td-bm --td 0xc` unchanged
   (`1effffffffffffff`) while `get-td-bm --td 0x9` now shows bit 9 written
   (`1efdffffffffffff`) — snapshot isolation at the mapping level.
9. **Rebuild** (`ThinDeviceCreated.md` U5-S3, R14): the host disconnects both
   NQNs, then `cn_drop` tears the cntlr down — CN21 sends **no** `delete`, so
   both thin ids stay in the pool metadata on DN1's legs — and `syncup-cn`
   re-introduces the pointer (its own stage, because parking the ns-devs onto
   dm-error is a `dmsetup reload`, i.e. a suspend). Then `syncup-cntlr`
   (CNREV1++) with `.td_list |= map(.created = true)`, the desired state the
   sp-worker publishes once both tds have flipped. Assert on that stage's own
   trace: **no** `dmsetup message … create_thin`, **no** `… create_snap`, no
   `dmsetup suspend` at all; `mutations()` scoped to the trace shows
   `dmsetup create` lines and, of `dmsetup message`, exactly the activation
   sweep's `reserve_metadata_snap`/`release_metadata_snap` pair — no
   `create_thin`, no `create_snap`, no `delete` (CN14: the rebuild
   re-creates the pool device, which is a designed sweep trigger — the drop's
   CN21 teardown sends no `delete`s, so a td removed while the cntlr was down
   is healed exactly here, and this rebuild has nothing to sweep); every
   thin/pool/namespace row `OK`; and both bitmaps re-read through
   `get-td-bm` unchanged — `0xc` ⇒
   `1effffffffffffff`, `0x9` ⇒ `1efdffffffffffff`. The mappings of both tds
   survived the rebuild, so the bare `dmsetup create` attached the *existing*
   ids: no empty volume, no data loss. This is the only place a real dm-thin
   pool proves it.
10. Check rounds; teardown as usual; residue checks (§11 step 7's scope: dm,
    md and the id-carrying subsystems of `0x3c1`).

## 13. Case C — `clone_xfer` (§11.3 live move + §11.5 recovery)

sp1 `0x3d1` = DN1+CN1 (slot 0); sp2 `0x3d2` = DN2+CN2 (slot 2). Host = VM1
(connects to CN1 loopback and CN2 across). Shared ss NQN + ns identity, so
the host sees **one** namespace with an sp1 path and an sp2 path.

**Stage 0 — sp1 up, data prep.** dn/cn syncups for sp1 (as case S, on
DN1/CN1, including the §9 two-phase side provisioning); host connects the
sp1 path, waits `optimized`; writes
`pattern-c.bin` = 32 MiB urandom to td offsets 0..31 (`dd bs=1M count=32
conv=fsync`), records `sha256(pattern-c.bin)`; sync. The second 32 MiB is
never written — the skip range will come from **real** thin mappings.
`get-td-bm` sp1 td 0x9 ⇒ assert `00000000ffffffff` (blocks 0..31 written).
This is the production bitmap source (§8.13) — the suite pushes exactly
what it read.

**Stage 1 — sp2 up, identical namespace, suspended.** dn/cn syncups for
sp2 on DN2/CN2 (again including the §9 two-phase side provisioning, which
completes before CN2's cntlr converges — so this suite never presents the cn
agent with a provisioning-deferred leg), with ns 0xb `suspended: true` and
no clone yet. Host
connects the sp2 path: controller live, ana `inaccessible`, no by-id node
appears for it (expected — the multipath device still serves via sp1).
The dst td is freshly created and never written — the [D3] precondition.

**Stage 2 — transfer on sp1 (host IO quiesced from here).**
`syncup-cntlr` CN1 (CNREV1++) adding xfer 0xc (`ori_nqn = …:c:vol1`,
`ori_ns_idx 1`, `allowed_hosts = [CnHostNqn(0x1, 0x12)]`,
`auto_suspend: true`). Assert: `xfer_id_to_{dm_linear,subsystem,namespace}`
OK; on VM1 the origin ns-dev is **suspended** (`dmsetup info` — the CN16
effective-suspend) and the sp1 path went `inaccessible`; the xfer
subsystem `nqn.2024-01.io.dnv:4:…:00000000000003d1:000000000000000c` is on
VM1's port with `allowed_hosts` = exactly CN2's hostnqn.

**Stage 3 — clone declared, gated; bitmap pushed race-free.**
`syncup-cntlr` CN2 (CNREV2++) adding clone 0xc (`src_nqn` = the XferNqn,
`src_tr_conf_list = [<ip1> tcp 4200]`, `src_ns_idx 1`, `src_slice_cnt 1`,
`src_stripe_size 65536`, `src_block_size 1048576`, `dst_td_id 0x9`,
`dm_clone_conf {1,1}`, `auto_resume: true`, `bm_cnt 1`) **with `sp_level
SP_LEVEL_NO_CLONE`** — the staged gate that makes the push race-free
(cnagent.md CN19): assert `clone_id_to_dm_clone` reports `sp_level`, and
no `nvme connect` to a `:4:` NQN has run on VM2. `push-clone-bm --clone
0xc --bm-idx 0 --bitmap-hex 00000000ffffffff` (the stage 0 read-back);
equal-revision `syncup-cntlr` re-send purely to read the reply: assert
`bm_info_list == [{res_id: 0xc, bm_idx_list: [0]}]`.

**Stage 4 — enable.** Same request at `sp_level SP_LEVEL_READWRITE`
(CNREV2++). In this one converge the agent connects to the xfer, allocates
`ceil((4 MiB + region_cnt bytes) / CnCloneMetaUnit)` contiguous 4 MiB units
from the loop arena, `blkdiscard`s exactly that range **on the loop device**
(the CN18 recycled-unit guard — plain discard, never
`--zeroout`), creates the kind-`b` wrapper
`dnv-0000000000000001-0000000000000012-b-00000000000003d2-000000000000000c`
over it, builds the dm-clone `no_hydration` with that wrapper as its
metadata device, applies the chunk, enables hydration, reloads the ns-dev
onto the clone and — the `auto_resume` override, with the stored
`suspended` still `true` — moves the ns to `optimized`. Assert:
`clone_id_to_{target,dm_clone,meta}` OK, and that the kind-`b` wrapper of
that name is `live` on VM2 and that its `dmsetup table` opens with a linear
target, `0 <len> linear <maj:min> <off>` — the on-VM form of
`clone_id_to_meta[0xc].res_name`, taken in place of the reply field because
the table, not the name, is the allocation record (CN28 now probes it with
`dmsetup table`, not `lvs`). That grep is satisfied by the first matching
line and never compares `<maj:min>` against the arena loop's devno, so it
pins the shape of the allocation record and not the device it was carved
from; the dm-clone's own live `dmsetup table` matches
`' 2 no_hydration no_discard_passdown( |$)'` — the exact mandatory
feature pair, asserted here against a real kernel and not only in the
`cnagent.md` §6 unit tests. The pinned regex is safe even after
`enable_hydration`: `dmsetup table` (`STATUSTYPE_TABLE`) reprints the
constructor args dm-clone saved at create time, and only `dmsetup status`
(`STATUSTYPE_INFO`) recomputes the live flags the message changes;
`cn-agent.log` on VM2 carries **exactly one** `blkdiscard` **whose target is
the `dnv-*-7-*` device** with `--offset 33554432 --length 33554432` (skip
bits 32..63 → one coalesced 32 MiB range at 32 MiB — the log-arithmetic
proof that geometry and inversion are right), ordered after the clone create
and before the `enable_hydration` message. Scoping that count by target
device is load-bearing with the CN18 arena: the same converge also logs a `blkdiscard`
against the **loop device** (the arena unit range), ordered *before* the
`dmsetup create` of the kind-`b` wrapper — assert exactly one of those too —
so an unscoped "exactly one `blkdiscard`" grep would now fail. Neither ever
carries `--zeroout` (on a CN, `blkdiscard
--zeroout` must never appear at all). Host: sp2 path `optimized`, sp1
stays `inaccessible`; IO resumes (the §11.3 flip).

**Stage 5 — mid-hydration probe.**
`wait-hydrated --clone 0xc --min-first 32 --sample-only`: the single sample
must already show hydrated ≥ 32/64 (the bitmap jump; background copy at
batch size 1 has barely started), and the script logs `read-through window
HIT` when it is still < 64, else a tolerated `window missed` (64 regions on
loop devices can outrun the script). Hard assert regardless of the window:
host read of the last must-copy MiB (`skip=31`) sha-equals the same window
of `pattern-c.bin` (served by read-through from the xfer if not yet
hydrated). The completion wait deliberately does **not** run yet — the wipe
comes first:

**Stage 6 — CN2 wipe + §11.5 recovery.** Immediately after the stage 5
sample (hydration may or may not have finished by now — the recovery
contract covers both; log which): `pkill -f 'dnv-agent cn'` on VM2 (the dn agent
keeps running), `mv cn-agent.log cn-agent.pre-wipe.log`, then wipe CN2's
kernel state to simulate a CN reboot: remove sp2's host-facing subsystem
from configfs (inside-out), `dmsetup remove` kinds 6 and 8, then 7 (the
clone — **while its `:4:` source connection is still up**, it flushes
through it), disconnect the `:4:` and sp2 `:2:` connections, remove kinds
5,4,3,2,1,0,a,9 and then kind `b` (the clone-metadata wrapper the dm-clone
sat on — it must go before the loop device can be detached), `mdadm --stop`
every `dnv-` array, `losetup -d` the arena loop + `umount` the tmpfs (the
clone's metadata is now genuinely gone, arena and allocation registry
together — the tmpfs-volatility this recovery exists for; the
registry *is* the kernel's dm tables (CN18), so removing the
wrappers and the tmpfs in one sweep leaves nothing stale behind). The
shared port and the dn objects stay (co-location artifact, §3 — a real
reboot would take them too and `EnsurePort` would simply recreate them).
Kind 8 and the `mdadm --stop` find nothing on this RedundNone,
transfer-free CN: the wipe walks §16's full kind sweep, in the same kind
order (the `mdadm --stop` and the `:2:` disconnect sit at different points
in it), so that it stays a faithful reboot of *any* CN rather than a
hand-picked list that would silently stop matching if a later case gave CN2
an xfer or a raid1 group. One thing it deliberately does not copy from §16:
it disconnects the `:4:` controllers but leaves the node's *own* `:4:` xfer
subsystems in configfs, next to the shared port they hang off — so a CN2
that did carry an xfer would keep that export across the wipe, where §16
also `rmdir`s it. `$WORK/cn-store` is untouched. Relaunch the cn
agent; `get-cn-size --wait 120` — the listener opens only after the startup
reconcile returns, and here that reconcile is the whole §11.5 recovery
(reconnect, re-read the destination bitmaps, re-apply the chunk), so the
wait-up budget is the recovery's, not a process start's. Assert the
reconcile rebuilt everything from the store: `get-cntlr-info` all OK; the
fresh `cn-agent.log` shows the §11.5 order — `reserve_metadata_snap` →
`thin_dump` → `release_metadata_snap`, then the arena-unit `blkdiscard` +
`dmsetup create` of the fresh kind-`b` wrapper, then the dst-bitmap
`blkdiscard`s (on the `dnv-*-7-*` dm-clone) and the re-applied src chunk
**before** the `enable_hydration` message; equal-rev re-send still reports
`bm_idx_list [0]` (chunk files survived). Of that order the recovery stage
re-asserts a subset — the reserve/dump/release sequence, that the *first*
`blkdiscard` on the dm-clone precedes `enable_hydration`, and that exactly
one kind-`b` wrapper exists for this CN — because the reconcile mints its
own trace id and its per-`blkdiscard` arithmetic is already pinned on
stage 4's converge, whose trace can be isolated. Host: the wipe killed the
sp2 controller with DNR (it will not reconnect) — disconnect it **by
device** (`nvme disconnect -d`, never `-n`: the NQN is shared with the live
sp1 path) and reconnect; wait `optimized` again.

**Stage 7 — completion.** `wait-hydrated` to 64/64 (timeout 120 s).

**Stage 8 — finalize (production order).** `syncup-cntlr` CN1 (CNREV1++):
xfer removed, ns `suspended: true` (= `DeleteTransfer(force=false)`
semantics — the source stays retired). `syncup-cntlr` CN2 (CNREV2++):
clone removed, ns `suspended: false` (= `DeleteClone`). Assert on VM2:
ns-dev back on the raid0; the dm-clone and its kind-`b` metadata wrapper are
both gone (`dmsetup ls | grep -- '-b-'` empty for this CN, which is also the
allocator's proof that the units are free again — the tables are the
registry); the `:4:` connection disconnected — in the CN18 order (ns-dev
reload → clone remove → kind-`b` wrapper remove → disconnect, from the log).

**Stage 9 — verification via the host** (only the sp2 path serves): drop
caches; first 32 MiB sha == `sha256(pattern-c.bin)`; second 32 MiB reads
all zeros **and** `get-td-bm` on sp2 td 0x9 shows bits 32..63 still `1` —
never copied. That last bit is the differential proof: without the bitmap,
hydration would have copied the source's zeros and *mapped* those blocks
(dm-clone copies regions unconditionally); only the skip leaves them
unmapped. Then a write probe at `seek=40`, readback equal — the moved
volume is live and writable.

**Stage 10 — teardown**: host disconnects (by NQN now — every path is
going), empty cntlr and side lists everywhere, then §10 step 6's per-CN
sweep on **both** CNs — no dm device and no host-facing `dnv-it:*`
subsystem left of either CN's own, an empty kind-`b` arena, and the §3.2
base resources still probing OK via `get-cn-info`, because a teardown that
took the port, the tmpfs or the loop arena with it would satisfy every
residue check and still leave the CN unable to serve the next SP; and last
the residue checks on both VMs for `0x3d1` and `0x3d2` (§11 step 7's scope:
dm, md and the id-carrying subsystems). (This case's §18 check rounds —
`check-cn` **and** `check-cntlr`, on both CNs — run in their own
pre-teardown `check` stage, at steady state per §9, never after this
teardown.)

## 14. Case D — `restart`

Setup: the A-shaped SP `0x3e1` (raid1 across DN1/DN2, primary CN1, standby
CN2; sides through `dn_side` per the §9 two-phase sequence, so they are
fully zeroed before the step 1 snapshot), host VM2 connected to both paths,
4 MiB pattern written and verified.

1. Snapshot: `get-cn-info` + `get-cntlr-info` on both CNs (jq-normalized;
   excluded from later comparison: every `ResInfo.epoch`, and
   `leg_id_to_leg[].details` — the CN11 prober restarts and re-stamps its
   timestamps).
2. Restart both cn agents: `pkill -f 'dnv-agent cn'`; wait for exit; `mv
   cn-agent.log cn-agent.pre-restart.log`; relaunch; `get-cn-size --wait 60`
   — the listener opens only after the startup reconcile returns, so the
   budget covers a reconcile that re-probes two md arrays, a pool and four
   legs, not just the process start (§13 stage 6 needs 120 for the same
   reason, its reconcile being a full clone recovery).
   Kernel state (md, dm, pools, nvmet, leg and host connections) is
   untouched; the dn agents never stop.
3. **Data-plane continuity**: while the cn agents are down and again after
   restart, the host's read of the 4 MiB test data succeeds — the data
   path does not depend on the agent process.
4. Reconcile assertions: `get-cn-info`/`get-cntlr-info` on both CNs
   deep-equal the step 1 snapshots (modulo the step 1 exclusions); the
   post-restart logs show `mdadm --detail`-style probing only — an active
   array is recognized, not re-assembled.
5. **Idempotency (mutation-free re-apply)**: re-send the *same-revision*
   `syncup-cn` and `syncup-cntlr` to both CNs; assert code 0; then assert
   `mutations()` (§9) finds **zero** records in each post-restart
   `cn-agent.log`, which covers reconcile + these re-applies: no
   dm/md/nvme/configfs mutation, no `os write file direct` and no
   `os write block`, on a converged node. The CN11 probers' own
   `probe write block` / `probe read block direct` records are outside that
   list by construction (§9) and keep flowing throughout.
   The DN sides were fully zeroed during setup (§9 phase (b)), so the dn
   agents' zeroing registries find nothing to do on their own reconciles and
   emit no `blkdiscard` either; that is a precondition of the mutation-free
   claim, not an accident. (Only the cn agents are restarted here; the dn
   logs are covered by the same rule in `dnagent_integtest.md` §15.)
6. **Revision persistence probe**: `syncup-cn` CN1 with `revision
   CNSYNC1-1` — one below the revision the last `SyncupCn` stored; NOT
   `CNREV1-1`, which after the cntlr converge equals the stored `SyncupCn`
   revision and would re-apply with code 0 (§9) — and `--expect-code 1`
   (`ReplyCodeStaleRevision`, a normal reply, not a gRPC error) — the one
   intentional negative call: only the stale *rejection* proves the
   revision survived the restart.
7. Teardown as usual (host disconnect, empty lists everywhere, residue
   checks for `0x3e1` — §11 step 7's scope: dm, md and the id-carrying
   subsystems).

## 15. (reserved)

Numbering kept parallel with `dnagent_integtest.md`; this suite has no
sixth case.

## 16. Teardown and cleanup (`cleanup()`, also `--cleanup-only`)

Best-effort (`|| true` throughout), the order load-bearing. Phases run
across **both** VMs (a clone on one VM holds a source on the other):

1. `pkill -f 'dnv-agent cn'`, then `pkill -f 'dnv-agent dn'` (kills the
   retry loops and probers with the agents).
2. `resume_suspended` — every suspended dm device matching `dnv*`
   (one flat prefix — [D14] removed LVM's doubled-dash
   nodes). Load-bearing, not
   defensive: a transfer's origin ns-dev is **deliberately suspended**
   (CN16), and the dn cutover window may hold linears suspended; a
   suspended device wedges `dmsetup remove`, nvmet disable above it, and
   any block-device scan in D state.
3. Host-role disconnects: every `nqn.2024-01.io.dnv-it:*` connection on
   both VMs (nvmet controllers must die before their subsystems).
4. cn nvmet, host-facing: disable namespaces, unlink port links, rmdir the
   `dnv-it:*` subsystems (frees the ns-devs below).
5. cn dm pass 1: remove kinds `6` (ns-dev) and `8` (xfer-final), then `7`
   (dm-clones — **while their `:4:` source connections are still up**;
   a clone flushes through its source on remove and blocks otherwise).
6. Disconnect `:4:` connections; then remove the `:4:` xfer subsystems.
7. cn dm pass 2: kinds `5`, `4`, `3`, `2`, `1`, `0`; `mdadm --stop` every
   array whose `mdadm --detail --scan` name starts `dnv-`; kinds `a`, `9`;
   last kind `b`, the clone-metadata wrappers the pass-1 dm-clones sat on —
   they cannot go before their clones do, and removing them frees their
   arena units and unbusies the loop device for step 9. §13 stage 6's wipe
   reuses this kind order, though it stops the arrays after kind `b` rather
   than mid-pass and disconnects `:2:` ahead of the pass rather than after
   it (step 8).
8. Disconnect `:2:` (leg) connections.
9. cn tmpfs/arena: `losetup -d` every loop backed under
   `/tmp/dnv-tmpfs/`, `umount` the `/tmp/dnv-tmpfs/*` mounts, `rmdir` them.
   No LVM step — [D14] removed the clone VG; the kind-`b`
   wrappers went at the end of step 7, which is what makes `losetup -d`
   succeed here (a surviving wrapper holds the loop device EBUSY).
10. `resume_suspended` again, then the dn suite's §16 steps in that suite's
    own order: the remaining `:2:` subsystems inside-out, dn dm kinds
    `1`,`3` → disconnect `:3:` → the `:3:` subsystems inside-out (deferred
    past the dm-clones, which flush over the *peer's* `:3:` export on
    removal — `dnagent_integtest.md` §16 step 4) → `5`,`2`,`0`,`4` with a
    retry sweep, then the port-level objects (`hosts/*`,
    `ana_groups/{2,3}`, the shared port), zero the disk-format header
    (`dd … conv=fsync`, no `oflag=`) + `wipefs`, `losetup -d` the backing
    loops.
11. `rm -rf $WORK`; `rm -f /etc/udev/rules.d/63-dnv-md.rules` +
    `udevadm control --reload`.

End-of-run cleanup (success only) is the same function; start-of-run
cleanup runs it unconditionally before setup.

## 17. Failure diagnostics

On any failure, before exiting, dump to the driver console: last 120 lines
of all four agent logs; `dmsetup ls` + `dmsetup table` + `dmsetup status`
(which already shows every kind-`b` wrapper and its
backing offset — the allocation registry itself, so there is no LVM report
left to dump); `cat /proc/mdstat` + `mdadm --detail --scan`; `losetup -a`;
`findmnt | grep dnv-tmpfs`; `ls -R
/sys/kernel/config/nvmet/{ports,subsystems}`; `nvme list-subsys -o json`
(both VMs); the last `get-cntlr-info` of every involved cntlr. Debris stays
in place (§2); the failing stage name and its `trace_id`s are printed so
records can be pulled from the JSON logs on either VM.

## 18. RPC coverage matrix

| RPC | exercised by | asserted |
|---|---|---|
| `GetCnSize` | setup wait-up | exact `--capacity` echo 1099511627776; liveness |
| `SyncupCn` | setup + every case | reply code, the four base-state infos (`port`/`tmpfs`/`tmp_file`/`loop_dev`; `clone_vg_info` is `reserved 5`, [D14]), declarative cntlr add/remove, stale probe (D) |
| `SyncupCntlr` | S, A, B, C, D | full primary/standby converges, failover order, readonly, snapshot, xfer/clone lifecycle, `sp_level` gate, equal-rev idempotency, `bm_info_list`, created-td rebuild with no device-set-mutating pool message — only the activation sweep's reserve/release pair (B, CN14) |
| `PushCloneBitmap` | C | reply code 0; effects via the stage 4/9 layers |
| `GetCnInfo` | teardown checks, D | statuses, snapshot equality |
| `GetCntlrInfo` | S, C polling + recovery, D | pool-status details format, `ParseCloneStatus` hydration, snapshot equality |
| `GetThinDeviceBm` | B, C | exact bitmaps incl. paging window; the B rebuild mapping proof; the C stage 9 skip proof |
| `GetLegBm` | B | data-group span arithmetic; meta-group all-zero rule |
| `CheckCn` | every case (§9 converge-check convention) | stream round: code 0, revision echo, show_info statuses |
| `CheckCntlr` | every case (§9) | same |

## 19. Out of scope (v1)

Negative/error-path testing beyond the case D stale probe; QoS (explicitly
deferred — `cnagent.md` CN6 — so there is nothing to observe); `GrowSlice`
online pool growth; spare legs and `SwitchSpareLeg`; a CN watching a leg
gain/lose its second side (migration multipath — the dn suite's cases B/C
already prove the two-path merge with emulated CN identities); `sp_level`
values other than `READWRITE`/`READONLY`/`NO_CLONE`-as-gate; `slice_cnt >
1` (real striping — the concat/span math is unit-tested per `cnagent.md`
§6.14, and one slice keeps every bitmap assertion exactly computable);
multiple namespaces per subsystem; snapshots of snapshots;
`UpdateNamespaceDev` repoints; **namespace or subsystem deletion short of a
full cntlr teardown** (no case drops an entry from `ns_list` or
`nqn_to_subsystem`, so the park-before-nvmet-removal order of CN9
is unit-tested instead, `cnagent.md` §6 test 26);
cntlid-slot exhaustion; dnv-cdc/host auto-discovery; TLS/auth;
performance/soak; fault injection; **CN-side
provisioning deferral** (a group whose `leg_list` holds an unprovisioned leg
is skipped and reports `RES_STATUS_PROVISIONING`, together with the CN16 ANA
conjunct that keeps its namespace `inaccessible`) — every side here is fully
provisioned by §9's two-phase setup before its cntlr converges, so the
deferral paths are covered by the `cnagent.md` §6 unit tests instead;
likewise clone-metadata arena exhaustion,
whose `RES_STATUS_ERROR` pair is the old `lvcreate`-ENOSPC path (CN18).
Listed so later additions extend this file rather than reshaping it.

## 20. Amendments

Recorded for traceability; the `U*-T*` ids are this suite's amendment ids
from the since-retired design-review ledgers. The edits are
already applied above. Appended, never inserted, so no section number
another document or the harness cites can shift.

- **U2-T5 (the probe-IO carve-out)** — §9 `mutations()` lost the `-9-`-path
  exemption for `os write block` / `os read block direct`. The carve-out takes the CN11
  leg health probers out of the `LimitedOsClient` (they call the exported
  `common.WriteBlockAt` / `common.ReadBlockDirectAt` directly, so a probe
  wedged on a pathless leg can no longer starve the 32-slot semaphore) and
  has them log their own `probe write block` / `probe read block direct`
  records. Those msgs are outside the mutation grep list **by
  construction**, so no path filter is needed; `ReadBlockDirect` left the
  `OsClient` interface entirely and `healthcheck.go` was the cn agent's only
  block-IO caller, so a converged CN now emits zero `os write block` records
  and the msg is simply listed (§14 step 5).
- **U3-T5 ([D14])** — LVM left the CN. The
  clone-metadata arena is now a slot allocator: one tmpfs-backed sparse file
  (`CnCloneMetaAreaSize`, 1 GiB) on one loop device, carved into
  `CnCloneMetaUnit` (4 MiB) units by kind-`b` dm-linears named
  `dnv-{cluster}-{cn}-b-{sp}-{clone}` whose dm tables *are* the allocation
  registry. Consequences here: §4 preflight drops
  `pvcreate`/`vgcreate`/`lvcreate`/`lvs`/`vgs` and keeps `thin_dump`, and
  records the arena loop's `blkdiscard` support as a lab prerequisite (there
  is deliberately no agent-side gate for it); §5 lists kind `b` in place of
  the clone-VG/LV names and the `dnv--clone--vg-*` doubled-dash gotcha is
  gone; §7 step 8 and §18 assert four base-state infos
  (`CnInfo.clone_vg_info` is `reserved 5`); §9 `mutations()` drops the LVM
  verbs from both its mutating and its probe half; §10 step 6 and §13 stages
  4/6/8 assert kind-`b` wrappers instead of LVs, and the stage-4
  `blkdiscard` count is now scoped by target device because the allocator's
  recycled-unit guard adds a second `blkdiscard` (on the loop device, plain,
  never `--zeroout`); §16 removes kind `b` at the end of dm pass 2, once the
  dm-clones of pass 1 have released it, and step 9 loses `vgchange`; §17 no
  longer dumps `lvs`; Appendix A's volatile-VG bullet became the
  volatile-arena bullet. Rationale: the [D13](a) label-scan class (a bare
  `vgs`/`lvs` scanning a CN's suspended transfer-origin ns-devs wedges LVM
  in unkillable D state) plus deterministic naming.
- **U4-T6 ([D15])** — whole-side zeroing
  behind a `provisioned` gate: every DN side is fully written with
  `blkdiscard --zeroout` before its first export, tracked per extent in the
  volume table's `zeroed_bits`. This suite has no worker, so §9 gains the
  two-phase side-setup convention (`--provisioned=false` → `wait-zeroed` →
  `DNREV[dn]++` → `--provisioned=true`, memoized in `dn_side`) that every
  case's DN-side setup (§10, §11, §12, §13, §14) now follows, plus the rule
  that a "must be ERROR" assertion compares against `RES_STATUS_ERROR`
  explicitly and phase-(a) checks use `assert_provisioning_or_ok` — because
  `RES_STATUS_PROVISIONING` would otherwise slip through a bare "not OK";
  §4/§7 step 3 add the loop's `write_zeroes_max_bytes != 0` check (it runs
  inside setup, since the per-VM preflight block runs before `losetup`
  creates the device); §11 case A's `--assume-clean` assertion now says
  "fresh **zeroed** legs"; §14 step 5 records that the fully zeroed sides are
  a precondition of the mutation-free claim; §19 records that the cn-side
  provisioning-deferral paths are unit-tested rather than exercised here;
  Appendix A's DN-trim bullet became the zeroing bullet. Rationale:
  multi-tenancy — discard-reads-zeros is not a hardware guarantee (the kernel
  dropped `discard_zeroes_data` in 4.12; NVMe DLFEAT read-zeroes is
  optional), so the old trim could leak a previous tenant's bytes and could
  hand a fresh thin pool a stale superblock.
- **U1-T4 (dm-clone features)** — every dnv dm-clone table carries
  `2 no_hydration no_discard_passdown`, without exception, so a hydration or
  §11.4 skip `blkdiscard` stays metadata-only instead of also reaching the
  destination. §13 stage 4's assertion list therefore pins the CN clone's
  live `dmsetup table` against `' 2 no_hydration no_discard_passdown( |$)'`
  — the exact pair, not a substring, and mandatory rather than optional
  hardening. The pinned form survives `dmsetup message … enable_hydration`
  because `dmsetup table` (`STATUSTYPE_TABLE`) reprints the constructor args
  dm-clone saved at create time, while only `dmsetup status`
  (`STATUSTYPE_INFO`) recomputes the live flags; the converge never reloads
  a dm-clone on feature drift either. `dnagent_integtest.md` §12 step 11 and
  its §20 U1-T4 entry carry the twin assertion for the dn migration clone.
- **Park-before-remove for removed namespaces** — a namespace removed from
  `ns_list` (a whole removed
  subsystem's included) is now parked on its td's `CnErrorName` before the
  nvmet removal, as CN9 always said. **The suite is unchanged**: no case
  drops a host-facing namespace or subsystem short of a full cntlr teardown —
  the xfer/clone stages only flip `suspended` and *add* a snapshot ss — so the
  change alters no event any stage asserts. Both `mutations()` consumers stay green.
  The case B rebuild stage, the suite's one verb-set assertion over that
  helper, deletes nothing. The case D restart stage (§14 step 5) is the
  stricter one — it demands the post-restart mutation set be **empty**, so a
  stray park would fail it — and it is safe for a stronger reason than "no
  removal is asserted": its re-applies resend the *same* request, so
  `removedNamespaces(old, plan)` is empty and the new loop emits nothing at
  all. §19 records the gap, and `cnagent.md` §6 test 26 is the coverage.

## Appendix A — lab gotchas baked into this plan

- **Two agents, one port**: dn and cn co-own `ports/1` (§3). Never assert
  "the port's subsystem list is exactly my case's" — the other role's
  subsystems are legitimately there; assert membership, not equality.
- **Both processes are named `dnv-agent`**: `pkill -x` kills both roles;
  this suite's kill idiom is `pkill -f 'dnv-agent <role>'`.
- **`nvme disconnect -n` kills every path of the NQN.** The §11.3 pair
  shares one subsystem NQN across SPs, and a leg's NQN spans its sides —
  disconnect a single dead path by device (`-d`), the way case C stage 6
  and the dn suite's migration step do.
- **A deliberately suspended dm device is part of steady state here** (a
  transfer's origin ns-dev, CN16) — the reason `resume_suspended` runs
  before any teardown pass and why no cleanup step may scan block devices
  before it.
- **ANA `inaccessible` paths have no `/dev` node and queue IO** → the
  failover and transfer stages quiesce host IO across their no-serving-path
  windows.
- **Subsystem/port removal kills host controllers with DNR** — they never
  reconnect on their own → explicit host disconnect+reconnect after the
  case C wipe.
- **uutils dd 0.8.0**: `iflag=/oflag=direct` silently broken → the §4 rule,
  identical to the dn suite.
- **The tmpfs clone-metadata arena is volatile by design**
  (`architecture.md` §3.2) — case C's wipe removes it on purpose (arena
  file, loop device and every kind-`b` wrapper) and §11.5 rebuilds from the
  destination thin-pool bitmaps; nothing in cleanup tries to preserve it.
  The allocation registry is the set of kind-`b` dm
  tables themselves (CN18), so arena and registry are volatile *together*: a reboot
  clears both, an agent restart preserves both. That equivalence is exactly
  why the CN allocator needs no on-file allocation table.
- **A fresh thin pool needs zeroed metadata, and gets it from the DN side
  provisioning protocol** (§9.4) — the whole
  side is written with `blkdiscard --zeroout` before its first export, which
  is a real guarantee rather than the old trim's hope that discard reads back
  as zeros (the kernel dropped `discard_zeroes_data` in 4.12; NVMe DLFEAT
  read-zeroes is optional). It is the same guarantee that lets CN12 pass
  `--assume-clean` and that keeps a recycled meta-group extent from handing
  a fresh pool a previous SP's valid thin-metadata superblock. If a pool ever
  reports a metadata corruption on first create, suspect a side exported
  before its `zeroed_bits` completed — which the §9.4 converge matrix makes an
  `ERROR` rather than a silent export — before suspecting dm-thin.
- **udev vs md**: the stock incremental-assembly rule would grab dnv raid
  members before the agent does; the §7 mask (`63-dnv-md.rules`, matching
  `MD_NAME dnv-*` after its own `--examine --export` import) relies on the
  stock rule honoring `SYSTEMD_READY` — preflight verifies that.
- **mdadm gates degraded assembly** on the explicit-devlist path by the
  survivor's recorded Array State — a single-leg `--assemble` may
  legitimately refuse; the agent reports the group `RES_STATUS_ERROR`
  rather than forcing (`cnagent.md` CN12), and no case here depends on a
  degraded start.
- **dm-flakey `<num_features>` counts the feature name** — the readonly
  table is `… flakey <dev> 0 0 1 1 error_writes`. Case A's readonly stage
  greps the live ns-dev table for `flakey` and for `error_writes` as two
  independent substrings (the looser form §11 step 6 states), because
  dm-flakey prints its feature arguments back in a kernel-version-dependent
  order: the agent's own ns-dev probe (`nsDevTableMatches` in `td.go`)
  compares the four positional arguments and merely requires `error_writes`
  to be present somewhere after them, for exactly that reason.

### Integration-run fixes (first on-hardware run)

- **IR-T1 (`--hostid`)** — §3 gains the rule that the emulated host's
  `nvme connect` passes `--hostid`, §8 gains `cnagentctl host-id --hostnqn`, and
  §10's connect uses it. Each VM runs a dn agent and a cn agent that connect
  under their own hostnqns, so the host identity cannot share the node-wide
  `/etc/nvme/hostid` with them.
- `mdadm` is a hard per-VM prerequisite (§4 already lists it) and was genuinely
  absent on one lab VM; installing it is lab setup, not a suite change.
- The suite's `smoke` check round caught `cnagent.md` IR5 (the namespace
  identity comparison) on its first run, which is what that stage exists for.

### `ThinDeviceCreated.md`

- **U5 (consistency fixes)** — `req_td` gained the optional `created` argument (default `false`),
  `assert_absent` was added as `assert_before`'s negative twin, and
  `mutations()` gained an optional leading `trace_id` filter so a stage can
  scope it to its own converge (§9 bullet updated). Case B models the
  gateway's `created` on the origin at the snapshot step (§12 step 6) and
  gained the drop/rebuild stages (§12 step 9); the former step 9 is now step
  10.
