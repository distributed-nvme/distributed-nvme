# AGENTS.md

This repo is a **proof-of-concept** for a distributed NVMe-oF block storage
system with live failover. There is no build, test, lint, or typecheck
toolchain — the only "verification" is running the scripts end-to-end against a
6-VM testbed. Do not invent commands; the scripts are the source of truth.

## Layout

Everything lives in `poc/`. There is no application code outside it.

- `common.sh` — shared library, sourced once by every other script. Two parts:
  Part A (resource API: `create_*`/`delete_*` that SSH into nodes internally)
  and Part B (low-level helpers uploaded to each node via the quoted heredoc
  `_emit_common`).
- `setup.sh` / `teardown.sh` / `failover.sh` / `host0_io.sh` — orchestrators.
- `servers.txt` — the 6 testbed nodes (host0, dn0/dn1, cn0/cn1, ref0).
- `note.md` — design notes / map. `plan.md` — the build plan that produced t04.

When editing, keep `note.md` and the scripts in sync — the doc explicitly says
"the scripts are the source of truth, this document is the map."

## Running the POC (requires the live testbed)

All scripts are run from `poc/` and SSH to the nodes listed in `servers.txt`
(hardcoded IPs also live in `common.sh`). They will not work on a random host.

```bash
./setup.sh all          # build the 6-node env; idempotent re-run is safe
./setup.sh verify       # check every dm device: linear/raid/thin read OK, error=EIO, delay blocks
./host0_io.sh start     # continuous verified O_DIRECT I/O on host0
./failover.sh [force]   # cn0 -> cn1; `force` skips steps 3 & 7 (models cn0 dying)
./host0_io.sh mark <label>; ./host0_io.sh report   # phase summary, 0 failures expected
./teardown.sh all       # remove everything; safe on partial/clean systems
```

There is **no automated test runner**. The pass criterion from `note.md` §12:
`setup`/`verify`/`failover`/`teardown` all exit 0, host0 reports 0 I/O
failures, and `teardown` leaves `dm=0 nvmet=0 loop=0` on all nodes.

## Architecture that is NOT obvious from filenames

- **6 nodes**: `dn0/dn1` (disk nodes, loop files), `cn0/cn1` (controller nodes,
  active/standby), `ref0` (discovery-only referral server, no storage),
  `host0` (NVMe initiator running `nvme-stas`, never `nvme connect`).
- **Storage stack** (bottom-up, see `note.md` §4): `pd` (loop) → `vd` (NVMe-oF
  slice + symmetric fault-injection pair `-real`/`-err`/`-delay`/`-cn`) →
  `grp` (raid1 + thinmeta/thindata) → `leg` (thin-pool + snap0) → `da`
  (container) → `exp` (raid0 + nvmet export to host). Glossary in `note.md` §2.
- **Naming is strict**: all dm devices and NQNs follow `dnv-<node>-da0-...`.
  `failover.sh` inlines `dmsetup` commands that **must produce names identical**
  to `common.sh`'s `create_*`. If you change a name, change it everywhere.
- **Snap-0 inheritance invariant**: during failover, cn0 removing its
  thin-pool commits metadata; cn1's `create_thin 0` must **fail** (inherited).
  If it succeeds, all pre-existing data is lost. See `note.md` §8.
- **disarm/arm dance**: the kernel's `nvme_partition_scan_work` reads sector 0
  on connect; if cn1's namespaces sit on a 3600s dm-delay that read hangs and
  wedges nvme-wq/udev/nvmet. `setup.sh` disarms DN delays before cn1 connects,
  re-arms after. `setup.sh verify` also disarm/arm around its cn1 checks.
- **teardown order is mandatory**: defuse **all** dm-delay first (parked bios
  make nvmet teardown hang), then ref0 unexport → host0 wait-disconnect →
  cn1 → cn0 → dn1 → dn0 → ref0. `teardown.sh` is safe on partial/clean systems.

## Conventions that differ from defaults

- Scripts use `set -uo pipefail` (note: **no `-e`**). Errors are handled
  explicitly with `_fail`/`_warn`, not by `set -e` aborts. Preserve this.
- All sizes are 512-byte sectors, defined once as `SEC_*` constants in
  `common.sh`. Do not hardcode sizes per-call.
- Node IPs/NQNs/host IDs are hardcoded constants in `common.sh`
  (`_emit_vars` ships them to each node). `servers.txt` is for humans.
- `host0_io.sh` uses Python (`os.O_DIRECT` + `preadv`/`pwritev` on
  page-aligned `mmap`), **not `dd`**, because uutils coreutils 0.8.0's
  `iflag=direct` is broken on these devices. Do not "simplify" it to `dd`.
- Every `create_*`/`delete_*` is idempotent (existence-checked, no-op-on-match).
  `cfg_set`/`nvmet_link` are no-op-on-match. Keep this when adding functions.

## Git

- Branches: `main` (default remote) and `dev` (active dev branch).
- Commit messages are short, lowercase, imperative ("fix failover image
  steps", "create standby cntlr"). Match that style.
- `.gitignore` excludes `*conf.sh`, `_out`, `*.pb.go`, `mockclient.go`,
  `mockutils.go` — leftover patterns from an earlier Go-based iteration
  (`*.pb.go`); they are vestigial here but do not remove them.

## Before editing a script

Read `note.md` §6 (resource naming) and the function's header comment in
`common.sh`. The `create_*` functions each take a node name + IP as the first
args and SSH internally; the orchestrator (`setup.sh` etc.) should read like a
recipe. Do not move SSH calls into the orchestrator.
