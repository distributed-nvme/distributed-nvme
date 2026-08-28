# CDC — Centralized Discovery Controller (NVMe-oF/TCP, Go)

A userspace NVMe-oF/TCP *Centralized Discovery Controller* written in Go,
deployed on `cdc0` (192.168.122.78). It serves only the discovery subsystem
(`nqn.2014-08.org.nvmexpress.discovery`) on port `8009`, returns per-host-
filtered I/O subsystem records from a watched JSON file, and emits Discovery
Log Change AENs to connected hosts when the file changes. No I/O proxying, no
namespaces, no ANA descriptors — pure steering.

This document is the map. The Go source and shell scripts in `cdc/` are the
source of truth — keep them in sync with this file when editing.

---

## 1. Wire protocol (from-scratch Go, no NVMe library)

Pure Go, no external NVMe deps; `go.mod` only. Little-endian throughout.

### 1.1 NVMe/TCP transport

- NVMe/TCP PDUs: ICReq/ICResp handshake + capsule header + data capsules for
  the admin queue. Mechanical codec in Go structs with `binary.LittleEndian`.
- No I/O queues. No Read/Write, no namespaces, no namespace table.

### 1.2 Admin command handlers (five only)

After CONNECT, stafd issues a small fixed set of admin commands on the
discovery subsystem. The CDC implements exactly these five:

| opcode | name            | behavior |
| ---    | ---             | --- |
| 0x06   | Identify Ctrl   | Static discovery-controller response (see §1.3) |
| 0x02   | GetLogPage      | LID=0x70 discovery log, with LPO byte slicing (see §1.4) |
| 0x18   | KeepAlive       | No-op success (keeps the admin queue alive) |
| 0x0C   | AsyncEventReq   | Arm the connection for AEN delivery; send `0xF0` on file change |
| 0x84   | Fabrics Disc    | Tear down the admin queue, remove per-host state |

Anything else → CQE status `Invalid Opcode` (`0x02`).

### 1.3 Identify Controller response

Minimal but correct, so stafd/stacd treat cdc0 as a Centralized DC:

- `CNTRLTYPE` (offset 0x518) = `1` (Discovery Controller).
- `CMIC` (offset 0x430) bit 7 = `1` — "Centralized Discovery Controller" per
  TP 8013. Tells stacd *not* to follow further referrals and to use cdc0's
  log entries as the final I/O subsystem list.
- `NN` (number of namespaces) = 0; `MAXNN` = 0.
- `KAS` (KeepAlive supported, in ONCS) = 1.
- `AERL` (Async Event Request Limit) = 4.
- `LPA` (Log Page Attributes) = 0.
- `AQSZ`AC / `MAXCMD` / `MAXXFER` = sane defaults; admin queue max 32,
  max transfer 4096 bytes (covers the discovery log for the test scale).

If interop issues arise during testing, fix empirically rather than
spec-lawyer.

### 1.4 GetLogPage(LID=0x70)

Accept any `LPO` (Log Page Offset, 8 bytes) and return byte slice
`[LPO, LPO+min(NUMD*4, len-LPO))` of the assembled log page. Proper LPO
slicing handles future larger logs and avoids garbage-on-paginate.

Page layout:

- 512-byte Discovery Log Page header:
  - `genctr` (u64 LE) — global monotonic counter, incremented per detected
    mtime change (even if no host ultimately gets an AEN).
  - `numrec` (u64 LE) — per-host filtered count (varies per connecting host).
- N × 1024-byte Discovery Log Page Entries, one entry **per port**:

| field          | value |
| ---            | --- |
| `trtype`       | `tcp` (= 2) |
| `adrfam`       | `ipv4` (= 1) |
| `subtype`      | `1` (I/O subsystem) |
| `portid`       | sequential from 1, auto-assigned per CDC-wide log |
| `cntlid`       | `0xFFFF` (dynamic; the I/O target assigns the real cntlid from its own range) |
| `asqsz`        | `32` |
| `traddr`       | from file, zero-padded fixed-width |
| `trsvcid`      | from file, zero-padded fixed-width |
| `subsysnqn`    | from file, 224 bytes zero-padded |
| ANA TLV        | **omitted** (`Tbytes=0`); the kernel reads ANA from each I/O controller's own log (LID=0x0c) |
| security/etc   | `0` (no TLS, no PDC) |

### 1.5 CONNECT / hostnqn handling

- Read `hostnqn` (224-byte string) and `hostid` from the Fabrics CONNECT
  `connect_data` payload.
- **Accept** any non-empty `hostnqn`; the connection succeeds regardless of
  which subsystems this host is allowed to see. Filtering happens *only* in
  GetLogPage (avoids leaking which hosts exist via CONNECT rejection patterns,
  and tolerates "host connects before its subsystem is published" orderings).
- Empty `hostnqn` → CONNECT reject with `Connect Invalid Parameters` (`0x02`).

---

## 2. Host filtering

- The CDC filters the discovery log by the connecting host's `hostnqn`
  against each subsystem's `allowed_hosts` list in the JSON file.
- A subsystem's port is included in host H's log iff `H.hostnqn ∈
  subsystem.allowed_hosts`.
- This is how test case 1 enforces "host0 doesn't reach cn2" — cn2's port
  simply isn't in host0's filtered log.

---

## 3. AEN semantics on file change

- File watcher: poll `stat()` mtime every 1 second on
  `/etc/dnv-cdc/subsystems.json`. (Avoid inotify; event coalescing on
  multi-edit saves can miss re-reads.) A 1-second poll loop is the natural
  rate-limit.
- On detected mtime change: re-parse JSON. **Parse failures keep the old
  in-memory model** (a half-saved file must not tear down live connections).
- Rebuild per-host filtered logs. Diff each connected host's new filtered log
  against its last-served filtered log.
- Send a Discovery Log Change AEN (`AEN Type=Notice (0)`, `AEN Info=0xF0`)
  **only to hosts whose filtered log actually changed**. Best-effort
  fire-and-forget. Log a warning to stderr if delivery fails; the host has
  `persistent-connections=true` and its own KeepAlive cadence, and the spec
  already allows the log to be re-fetched periodically.
- `genctr` (global) is incremented once per detected mtime change, regardless
  of which hosts received AENs. The host dedupes missed updates by observing
  `genctr` jumps in the page header.
- GetLogPage always returns the **current snapshot** — there is no queued
  history, no "missed intermediate states" replay.

---

## 4. Per-host state

- Keyed by `hostnqn` string (the prototype collapses to one entry per
  hostnqn; a second CONNECT with the same hostnqn replaces the first
  connection's handle).
- Each entry holds: hostnqn, last-served filtered log content, live
  admin-queue handle for AEN delivery.
- Removal: on Fabrics Disconnect, TCP reset, or 15 s idle (no admin command
  — KEEP_ALIVE or otherwise — within ~3× the default 5 s KATO). A gone host
  has no useful state; on reconnect it re-CONNECTs and re-fetches fresh.

---

## 5. Watched file format (JSON)

`/etc/dnv-cdc/subsystems.json`:

```json
{
  "subsystems": [
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-A",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host0"],
      "ports": [
        { "traddr": "192.168.122.125", "trsvcid": "4420" },
        { "traddr": "192.168.122.229", "trsvcid": "4420" }
      ]
    },
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-B",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host1"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" },
        { "traddr": "192.168.122.77",  "trsvcid": "4420" }
      ]
    }
  ]
}
```

- One subsystem = NQN + `allowed_hosts` (per-subsystem ACL) + list of ports
  (one per controller/path). Multipath falls out naturally (multiple entries
  with the same `subsysnqn` + different `traddr`/`portid`).
- **No `cntlid` field** — the CDC always advertises `0xFFFF` in the log.
  The per-cn cntlid range is a `setup_targets.sh`-side constant map, not a
  file concern.
- **No `ana_state` field** — ANA state lives on each cn's nvmet configfs and
  is the cn's own decision. The CDC relays no ANA information.

---

## 6. Targets (cn0/cn1/cn2)

Driven by `cdc/setup_targets.sh` (idempotent, SSHes to each cn, runs configfs
writes + `dmsetup`). Provisioned *from the same JSON file* the CDC watches,
so adding/removing ports stays in sync between what the CDC advertises and
what the cn actually serves. Has a `teardown_targets.sh` counterpart.

Per-port target, on the matching cn:
- dm-zero backend, 1 GiB (2097152 sectors), e.g. `dnv-cdc-cn0-subsysA`.
- nvmet subsystem with NQN from the file.
- Namespace NSID=1, fixed UUID/NGUID:
  - Subsystem A: `11111111-1111-1111-1111-111111111111`
  - Subsystem B: `22222222-2222-2222-2222-222222222222`
- Serial `DNV_CDC_POC`, Model `DNV_CDC` (same on every cn — needed for
  multipath fold-up).
- cntlid range, per-cn constant map (disjoint across cn so the kernel folds
  the paths into one multipath namespace):
  - cn0: 1–255
  - cn1: 256–511
  - cn2: 512–767
- `allowed_hosts` = **both** host0 and host1 NQNs (nvmet filters by host ACL,
  not by ANA; the CDC does the per-host subsystem filtering).
- nvmet port on `192.168.122.<cn>:4420` (the test's standard I/O port).
- `ana_groups/1/ana_state` written per the test's desired state — *this is
  the sole ANA authority*. The kernel reads it via GetLogPage(LID=0x0c) on
  each I/O controller after connect.

---

## 7. Hosts (host0/host1)

Driven by `cdc/setup_hosts.sh` / `cdc/teardown_hosts.sh`:

- `/etc/nvme/hostnqn`:
  - host0: `nqn.2026-08.org.dnv:host:host0`
  - host1: `nqn.2026-08.org.dnv:host:host1`
- `/etc/nvme/hostid`: a stable UUID per host.
- Install `nvme-stas` (`stafd` + `stacd`) if absent.
- `/etc/stas/stafd.conf`:
  - `zeroconf=disabled`
  - `[Discovery controller connection management]` `persistent-connections=true`
    (keeps the discovery connection to cdc0 open for AEN reception)
  - `[Controllers]` single entry:
    `controller=transport=tcp;traddr=192.168.122.78;trsvcid=8009`
- `/etc/stas/stacd.conf`:
  - `[I/O controller connection management]`
    `disconnect-scope=only-stas-connections;disconnect-trtypes=tcp`
  - No hard-coded I/O controllers — all come from the discovery log.
- Restart stafd/stacd after writing hostnqn (per the existing poc
  `stas_start` pattern). Existing identity + SIGHUP-reload behavior is fine.

---

## 8. CDC deployment

- Run on cdc0 via `nohup ./cdc > /tmp/cdc.log 2>&1 &`. Prototype; no systemd.
- Logs to stderr. No separate log file.
- Inputs via env vars with sane defaults:
  - `CDC_LISTEN` (default `192.168.122.78:8009`)
  - `CDC_FILE`  (default `/etc/dnv-cdc/subsystems.json`)
- Single dir `cdc/`. Pure Go, `go.mod` only (no external NVMe deps).

---

## 9. Edit ordering convention (operator-driven)

Initial setup:
1. Run `setup_targets.sh` to provision **all** cn targets first.
2. Start the CDC.

On delete (tests 2, 3):
1. Edit the file (drop the entry).
2. CDC AEN → host auto-disconnects.
3. Run `setup_targets.sh sync` to remove the now-orphaned cn target.

On add (test 4):
1. Run `setup_targets.sh sync` to create the new cn target.
2. Edit the file (add the entry).
3. CDC AEN → host auto-connects.

This avoids the host trying to connect to a not-yet-existent target (which
logs retries) and avoids it holding a stale connection to a torn-down one.

---

## 10. Identities (constant map for the test)

| node | IP              | role                          | cntlid range |
| ---  | ---             | ---                           | --- |
| host0 | 192.168.122.193 | NVMe initiator (stafd+stacd) | n/a |
| host1 | 192.168.122.197 | NVMe initiator (stafd+stacd) | n/a |
| cn0   | 192.168.122.125 | I/O target (subsys A path1)  | 1–255 |
| cn1   | 192.168.122.229 | I/O target (subsys A path2, subsys B path1) | 256–511 |
| cn2   | 192.168.122.77  | I/O target (subsys B path2, subsys A path3 in test 4) | 512–767 |
| cdc0  | 192.168.122.78  | Centralized Discovery Controller | n/a |

NQN constants:
- Discovery subsystem: `nqn.2014-08.org.nvmexpress.discovery`
- Subsystem A: `nqn.2026-08.org.dnv:subsys-A`, UUID `11111111-...-1111`
- Subsystem B: `nqn.2026-08.org.dnv:subsys-B`, UUID `22222222-...-2222`
- host0 NQN: `nqn.2026-08.org.dnv:host:host0`
- host1 NQN: `nqn.2026-08.org.dnv:host:host1`

ANA states (test seed; written to cn nvmet configfs only):
- Subsys A cn0: optimized, cn1: inaccessible. (Test 2 deletes cn0.)
- Subsys B cn1: optimized, cn2: inaccessible. (Test 3 deletes cn1.)
- Test 4 adds cn2 subsys A: optimized.

---

## 11. Test execution (`cdc/run_tests.sh`)

One script, prints PASS/FAIL per case, exits 0 on all pass. 5-second settle
after each file edit (1 s CDC poll + stacd connect/ANA re-eval margin).

### Test 1 — filtering
1. Write the file with subsys A (host0: cn0, cn1) and subsys B (host1: cn1,
   cn2). Provision cn0/cn1/cn2 targets. Start CDC. Start stafd+stacd on
   host0/host1.
2. Wait 5 s. `nvme list` on host0 shows subsys A's two paths (cn0, cn1).
   `nvme list` on host1 shows subsys B's two paths (cn1, cn2).
3. On cn0: start `tcpdump` on the test interface for ~10 s during a stafd
   restart on host1; assert host1's IP never appears in cn0's tcpdump. On
   cn2: same for host0's IP. (This enforces "host0 doesn't reach cn2;
   host1 doesn't reach cn0".)

### Test 2 — auto-disconnect (subsys A, cn0)
1. Delete the subsys-A cn0 entry from the file.
2. Wait 5 s. `nvme list-subsys` on host0 no longer lists cn0 for subsys A;
   only one A path remains (cn1).

### Test 3 — auto-disconnect (subsys B, cn1)
1. Delete the subsys-B cn1 entry from the file.
2. Wait 5 s. `nvme list-subsys` on host1 no longer lists cn1 for subsys B;
   only one B path remains (cn2).

### Test 4 — auto-add (subsys A, cn2)
1. `setup_targets.sh sync` creates the new subsys-A target on cn2
   (ANA optimized).
2. Add the subsys-A cn2 entry to the file.
3. Wait 5 s. `nvme list-subsys -o json` on host0 shows two A paths: cn1
   (inaccessible) and cn2 (optimized). The ANA states are read from the I/O
   controllers' own ANA logs, which the kernel fetches from each cn's
   configfs.

---

## 12. File layout (`cdc/`)

- `cdc/main.go` (and supporting `.go` files) — the CDC.
- `cdc/go.mod`
- `cdc/setup_targets.sh` / `cdc/teardown_targets.sh` — target provisioning.
- `cdc/setup_hosts.sh` / `cdc/teardown_hosts.sh` — host provisioning.
- `cdc/run_tests.sh` — the four test cases.
- `cdc/plan.md` — this document.

---

## 13. Out of scope / future

- RDMA transport — first version is TCP only; the file schema and CDC design
  are transport-agnostic and the `trtype` field can later carry `rdma`, but
  the wire layer only speaks TCP for now.
- CDC → target interface — replaced by "watch a file"; a future version
  might push configs to the cn targets directly (model B/C from the
  interview), but for now `setup_targets.sh sync` is operator-driven.
- TLS / SASL / DPDK / io_uring — not a concern for a TCP-only discovery
  server.