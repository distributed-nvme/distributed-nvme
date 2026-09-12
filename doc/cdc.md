# cdc.md — `dnv-cdc`: NVMe-oF Central Discovery Controller (dnv)

Status: **normative**. Companion to `architecture.md` (whose §12 this document
details and, where they differ, supersedes — the §12/§13 edits are recorded in
§10 here), `layout.md`, `log.md`, `dnv-worker.md` (the `etcdutil` and `model`
packages, §3/§4 there), and the suite conventions of `dnagent_integtest.md` /
`cnagent_integtest.md` / `dnv-worker.md` §14.

Conventions: requirement tags **DS** (discovery service model, §3), **WV**
(etcd watcher, §4), **NP** (NVMe/TCP service, §5), **CM** (`cmd/dnv-cdc`, §6),
**LG** (log records, §7). Decisions are cited as "§0 #n". MUST / SHOULD in the
RFC sense. "The specs" means NVMe Base 2.x + NVMe-oF + NVMe/TCP; "nvmet" means
the Linux kernel NVMe target. Byte encodings (identify layout, log-entry
layout, status codes) are the specs' — this document names fields and values,
not offsets.

---

## 0. Decision record

The decisions behind this document, each with its rule.

1. **Userspace implementation, not kernel nvmet referrals.** A referral hub
   built from a local nvmet port cannot filter per host (referrals are served
   to every connecting host), only redirects (each host would still need a
   persistent discovery connection per CN, defeating centralization), needs
   the anchor-subsystem workaround just to listen (a referral-only nvmet port
   never comes up), and its AENs cannot carry per-host meaning. dnv-cdc
   therefore implements the discovery controller itself and serves straight
   from etcd, with no kernel state to converge.
2. **NVMe/TCP only, admin queue only, pull only** (§5). No other transports
   (`--tr-type` accepts only `tcp`), no TLS, no in-band authentication, no
   header/data digests (negotiated off), no I/O queues, and no TP-8010 host
   registration (DIM/DDC): dnv-cdc reads etcd and answers hosts, nothing
   registers *into* it. Identify reports DCTYPE = Direct Discovery Controller
   and a Discovery Information Management command is refused with invalid
   command opcode — refused as a *command*, never by dropping the connection
   (NP2).
3. **Static ranges, active-active, no vote worker** (§0 of `dnv-worker.md`
   notwithstanding). `--range` is plain configuration; two instances given the
   same range are twins that both serve — the redundancy mechanism is hosts
   holding discovery connections to **all** configured cdc endpoints, not
   election. Overlap is harmless (the service is read-only); a coverage gap is
   an operations error no instance can detect and is documented, not handled.
   dnv-cdc registers nothing in etcd and never writes it.
4. **`--range` defaults to all sixteen ranges**, mirroring `--roles`
   defaulting to all three roles: a single-instance deployment needs no
   sharding flags.
5. **Per-host views, per-host GENCTR, impact-scoped AENs** (DS5-DS8). A
   watched change produces an AEN only on hosts whose *rendered, filtered* log
   page content actually changes — gaining an entry, losing one (including by
   `allowed_hosts` removal), or a field change inside a visible entry. A
   change invisible to a host before **and** after produces neither an AEN nor
   a GENCTR bump for it. An entry with empty `allowed_hosts` is visible to
   everyone, so its changes impact every active host.
6. **GENCTR is scoped to (instance, hostnqn) while the host holds ≥ 1 live
   connection** (DS7). It restarts when the host state is recreated (all
   connections gone, or the instance restarted). Legal: every fabrics
   connection is a fresh dynamic controller and hosts compare GENCTR only
   between reads on one controller; a reconnecting host re-reads the full log
   anyway.
7. **Pending-bit AEN coalescing** (NP11, DS8). Impact marks every connection
   of the host; a connection with an armed AER completes it immediately, one
   without gets the completion when its next AER arrives. Multiple impacts
   before delivery coalesce into one AEN — never lost, never queued up.
8. **AENs are gated by the host's Asynchronous Event Configuration** (feature
   0Bh, NP9): the discovery-log-change bit defaults to disabled per the
   specs; kernels enable it because Identify's OAES advertises it (NP7).
9. **KATO = 0 does not mean immortal**: such connections (one-shot `nvme
   discover`) get a fixed idle cutoff `DefaultCdcZeroKatoTmoMs` = 120 000 ms,
   mirroring nvmet's discovery default (NP10).
10. **Where this document is silent, mirror nvmet's discovery controller** —
    identify field values, status codes, property behavior — as implemented
    by the reference kernel at implementation time (NP14). This pins the
    countless byte-level choices to something every host is already tested
    against.
11. **Log-page reads serve a snapshot** taken when the Get Log Page command
    arrives (DS9): paged offsets within one command are self-consistent;
    consistency across commands is the host's GENCTR re-read protocol, as the
    specs intend.
12. **nvme-stas is the production host stack; plain nvme-cli is equally
    supported.** The AEN path is stock kernel (the `NVME_AEN=0x70f002`
    uevent); stas adds automatic connect / re-point / disconnect. The
    integration suite tests both layers separately (§9.1) so a stas
    configuration problem cannot be mistaken for a cdc bug.
13. **The integration suite drives etcd directly** through a minimal
    `integtest/cdcctl` (§9.6): dnv-cdc reads only `CdcEntry` keys and never
    `ClusterConf`, so cluster ids are fabricated and the gateway/worker
    pipeline — already proven by `dnv-worker.md` §14 (its case D asserts
    `CdcEntry` contents) — stays out of this suite.
14. **Simulated CN namespaces are dm-zero devices** (§9.7): no backing files,
    no loop devices; reads return zeros, writes vanish. Discovery correctness
    needs connectable targets, not durable data. The nvmet side sets
    `attr_allow_any_host = 1` on every test subsystem **deliberately**: if
    dnv-cdc ever leaks an entry to the wrong host, the resulting connect
    *succeeds* and the wrong-device assertion catches it, instead of nvmet's
    ACL masking the leak.

---

## 1. Scope and placement

`dnv-cdc` is the control-plane process that serves NVMe-oF discovery to
hosts (`architecture.md` §1, §12): it watches the `{p} cdc` keys, keeps a
per-host filtered view of the discovery log, answers the well-known discovery
subsystem NQN `nqn.2014-08.org.nvmexpress.discovery` over NVMe/TCP, and sends
Discovery Log Page Change AENs to exactly the hosts a change impacts. It
never writes etcd, never serves or dials gRPC (no interceptors — the
`grpc.md` table already records "cdc: none"), and never touches the local
kernel's nvmet.

Who writes what it reads: the gateway creates and deletes `CdcEntry` at
`CreateSubsystem` / `DeleteSubsystem` and rewrites `allowed_hosts` at
`UpdateSubsystemHosts`; gateway and worker rewrite `nvme_tr_conf_list`
at `CreateCntlr` / `DeleteCntlr` / `UpdateCntlrEnabled` / `ReplaceCntlr`
(`architecture.md` §8, `dnv-worker.md` §4 MD8). dnv-cdc is the only *serving*
consumer; the gateway and worker also read entries inside their own
read-modify-write STMs (the `allowed_hosts` and `nvme_tr_conf_list` rewrites
above), but nothing else ever renders them to a host.

One process, three parts:

```
dnv-cdc process  (--range …; one listener)
├── watcher (§4)  — scan + watch of {p} cdc, filtered on owned shard codes,
│                   feeding the view registry
├── view registry (§3) — per-active-hostnqn: rendered records, GENCTR, pending
└── NVMe/TCP server (§5) — accept loop; per-connection admin-queue state
                    machine (Connect, properties, Identify, Get Log Page,
                    Features, Keep Alive, AER)
```

Packages and files (`layout.md` §2/§3 as amended by §10):

| package | files | may import (internal) |
|---|---|---|
| `cdc` | `cdc.go` (Run, deps), `watch.go` (§4), `view.go` (§3), `logpage.go` (DS3/DS9 rendering), `server.go` (listener, NP1), `conn.go` (per-connection state, NP4-NP12), `pdu.go` (NP2/NP3 codec) | `common`, `pb`, `etcdutil`, `model` |
| `cmd/dnv-cdc` | `main.go` | `cdc`, `common`, `etcdutil` (+ cobra, viper) |

Out of scope here: how operators distribute cdc endpoints to hosts (static
nvme-cli scripts or `stafd.conf`; mDNS/TP-8009 is not implemented), gateway
and worker behavior, and everything §9.17 lists for the suite.

Reading order for an implementer: §2 → §3 → §4 → §5 → §6 → §7 → §8 → §9.

---

## 2. Constants and schema

### 2.1 Additions to `common/constants.go`

| constant | value | meaning |
|---|---|---|
| `NvmeDiscoveryNqn` | `nqn.2014-08.org.nvmexpress.discovery` | the well-known discovery subsystem NQN (NP5) |
| `DefaultCdcTrType` | `tcp` | the only accepted `--tr-type` (§0 #2) |
| `DefaultCdcAdrFam` | `ipv4` | default `--adr-fam` |
| `CdcAdrFamIpv6` | `ipv6` | the other accepted `--adr-fam` value (CM1/CM2) |
| `DefaultCdcTrSvcId` | `8009` | default `--tr-svc-id` — the standard discovery port |
| `CdcRangeAll` | `0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f` | the `--range` default (§0 #4) |
| `CdcMaxAdminSqSize` | 32 | admin SQ entries; CAP.MQES = 31; Connect SQSIZE cap (NP4); ASQSZ in every log entry (DS3) |
| `CdcAerl` | 3 | Identify AERL: up to 4 outstanding AERs per connection (NP11) |
| `CdcMaxH2CData` | 8192 | ICResp MAXH2CDATA and the cap on any received PDU's data — Connect's 1024 B data blob is the only H2C data ever *read* (NP2, NP3) |
| `CdcDiscLogHeaderSize` | 1024 | discovery log: GENCTR + NUMREC + RECFMT header block (DS9) |
| `CdcDiscLogEntrySize` | 1024 | one discovery log entry (DS9) |
| `CdcCntlIdMax` | `0xffef` | dynamic CNTLIDs assigned round-robin in `[1, 0xffef]` (NP5) |
| `DefaultCdcKeepAliveGraceMs` | 10000 | expiry = KATO + grace (NP10) |
| `DefaultCdcZeroKatoTmoMs` | 120000 | idle cutoff for KATO = 0 connections (§0 #9) |
| `DefaultCdcRescanInterval` | 10 | seconds between retries of a failed etcd scan (WV5) |

### 2.2 Addition to `model/keys.go`

`CdcEntryKey` and `CdcEntryPrefix` exist (`dnv-worker.md` §4 MD2), and so does
the parser, in the same style as `ParseDnRevKey`:

```go
// ParseCdcEntryKey parses "{p} cdc {cid} {shard} {sp_id} {ss_id}" (MD2).
func ParseCdcEntryKey(key string) (cid uint64, shard uint32, spId uint64, ssId uint64, ok bool)
```

Malformed keys return `ok = false` (WV2). Unit tests: goldens both ways plus
rejects (wrong kind, wrong field count, non-hex fields) in `model`.

### 2.3 Schema

No `schema.proto` change: `CdcEntry{nqn, nvme_tr_conf_list, allowed_hosts}`
and its key schema already exist.

---

## 3. The discovery service model [DS]

* **DS1 — source of truth.** The served state is exactly the `CdcEntry`
  messages under `{p} cdc` whose `{shard_code}` key field is owned (DS2),
  across **all clusters** — the key places `cluster_id` before `shard_code`,
  so one prefix watch covers every cluster and a host allowed in two clusters
  sees both. dnv-cdc adds nothing, caches nothing else, and persists nothing.
* **DS2 — ownership.** The owned shard-code set is fixed at startup from
  `--range`: range digit `h` owns the 16 codes `h0…hf` (CM2). An entry is
  owned iff its `shard_code` is in the set.
* **DS3 — record rendering.** One `CdcEntry` renders to one discovery log
  entry **per element** of `nvme_tr_conf_list`:
  * TRTYPE/ADRFAM from `tr_type`/`adr_fam` (`tcp`→TCP, `ipv4`/`ipv6`); an
    element with any other `tr_type` is skipped with `cdc entry skipped`,
    reason `foreign_tr_type` (§0 #2), and one with any other `adr_fam` the
    same way with reason `foreign_adr_fam` — an address family this
    controller cannot name has no byte to put in the record. Either way the
    rest of the entry still serves.
  * TRADDR/TRSVCID verbatim from `tr_addr`/`tr_svc_id`; SUBNQN = `nqn`.
  * SUBTYPE = NVM subsystem; TREQ = not specified; CNTLID = `0xffff`
    (dynamic); ASQSZ = `CdcMaxAdminSqSize`; EFLAGS = 0; TSAS = TCP, sectype
    none.
  * PORTID = the low 16 bits of `fnv64a(tr_addr + " " + tr_svc_id)` —
    deterministic, equal for the same CN port wherever it appears; a
    collision is harmless (the field is informational).
* **DS4 — visibility.** Host `h` (its Connect hostnqn, exact string match)
  sees entry `e` iff `e.allowed_hosts` is empty or contains `h`.
* **DS5 — view.** A host's view is the rendered records of its visible owned
  entries in the deterministic order (`cluster_id`, `shard_code`, `sp_id`,
  `ss_id`, tr-conf index). NUMREC = record count; GENCTR from DS7. Views are
  materialized only for **active** hosts (DS7) and re-rendered on events.
* **DS6 — impact.** Applying one watched event (put or delete of an owned
  entry, WV3): for every active host where the entry was visible before
  **or** is visible after, re-render the view; if the rendered content
  differs, that host is *impacted*: `genctr++`, log `view changed`, set the
  pending bit on **each** of the host's connections, and attempt delivery on
  each (NP11). Not visible on either side ⇒ nothing, including no GENCTR
  move — note `allowed_hosts` edits that keep the host's membership do not
  change rendered bytes and therefore do not impact it (the suite asserts
  this, §9.12).
* **DS7 — host state.** Created at a hostnqn's first live connection
  (GENCTR = 1, no pending, view rendered), shared by all its connections on
  this instance, dropped at its last disconnect. Hosts without a connection
  are never tracked and never notified — they catch up by reading the log
  when they next connect (that is the whole point of persistent discovery
  connections).
* **DS8 — AEN value.** A delivered AEN completes an AER with the Notice
  event type, event information *Discovery Log Page Changed*, log page id
  70h — gated per connection by NP9's feature mask.
* **DS9 — log page layout and snapshot.** Byte 0: the
  `CdcDiscLogHeaderSize` header (GENCTR, NUMREC, RECFMT = 0); entry *i* at
  `CdcDiscLogHeaderSize + i × CdcDiscLogEntrySize`. Each Get Log Page
  command serves offsets from a `(genctr, records)` snapshot taken at
  command receipt; reads beyond the end return zeros.
* **DS10 — staleness.** Through etcd outages dnv-cdc keeps serving its last
  known state (WV5): stale-but-consistent beats unavailable for an advisory
  service. Recovery rescans diff against the held state through the same
  DS6 path, so changes missed during the outage still AEN.
* **DS11 — restart.** Nothing persists. Every connection drops; hosts
  reconnect, host states are rebuilt, GENCTRs restart (§0 #6).

---

## 4. The etcd watcher [WV]

The `shard.go` skeleton of `dnv-worker.md` §7, minus per-key workers: one
goroutine owns the entry map, and the view registry is serialized under one
mutex — entry mutations and impact fan-out run on the watcher goroutine,
while host-state attach/detach (a connection materializing or dropping its
view) runs on connection goroutines under that same mutex (`view.go` states
the rule as Go expresses it). The serialization invariant is what matters:
no served state is ever mutated concurrently.

* **WV1 — scan then watch.** `Range(CdcEntryPrefix())` → build the owned
  entry map → `WatchTyped` from `rev + 1` with `newMsg = &pb.CdcEntry{}` →
  apply events serially. Log `cdc scan complete` after each scan.
* **WV2 — parse and filter.** Keys parse with `ParseCdcEntryKey`; a key
  whose shard code is not owned is skipped **silently** (expected, the §12
  key-field filter); a malformed key or value logs `cdc entry skipped` and
  is dropped (the `dnv-worker.md` SW2 rule).
* **WV3 — events.** A put upserts the entry (log `cdc entry applied`,
  `op = put`); a delete removes it (`op = delete`). Either way the DS6
  impact pass runs against the change.
* **WV4 — watch failure ⇒ rescan.** Any watch error, compaction included,
  logs `cdc watch restarting` and returns to WV1. The rescan does not walk
  the two maps key by key: it installs the freshly scanned map wholesale and
  re-renders **every** active host (`cdc/view.go`, `registry.replace`),
  impacting the hosts whose rendered bytes moved. The impacted set is
  exactly the one a per-key diff would name — DS6 impact is defined on
  rendered content, not on which key changed — so changes missed across the
  gap still AEN. GENCTR is where the two differ: one `replace` bumps an
  impacted host once for the whole gap, where the same changes arriving as
  N watch events would have bumped it N times. Hosts compare GENCTR between
  reads rather than counting its steps (§0 #6), so that difference is not
  one they can act on, and a rescan is rare enough that re-rendering every
  host costs far less than getting a per-key diff subtly wrong.
* **WV5 — scan failure ⇒ retry.** A failed Range retries every
  `DefaultCdcRescanInterval` seconds; the server keeps answering from the
  held state meanwhile (DS10). The `etcdutil` record carries the error.
* **WV6 — read-only.** No puts, no deletes, no leases, no locks, ever.

---

## 5. The NVMe/TCP discovery service [NP]

* **NP1 — listener.** One TCP listener on `(--tr-addr, --tr-svc-id)`. Per
  accepted connection: one reader goroutine and one connection state owned
  by it. No connection cap in v1 (the KATO/idle reaping of NP10 bounds
  leakage). An `Accept` error that is not the shutdown logs `cdc accept
  failed` and pauses 100 ms (`acceptRetryDelay` in `cdc/server.go`) before
  accepting again, so a transient failure (a file-descriptor shortage, say)
  costs a pause rather than a spin that fills the log and the CPU.
  `SIGTERM`: stop accepting, close every connection, stop.
* **NP2 — PDU subset.** Implemented: ICReq, ICResp, H2CTermReq, C2HTermReq,
  CapsuleCmd (Connect is the only command whose in-capsule data is *read*),
  CapsuleResp, C2HData. Never sent: R2T (no host data is ever solicited).
  Anything malformed — unknown PDU type, bad HLEN/PLEN, a PDU longer than
  `CdcMaxH2CData` plus its header — answers C2HTermReq with the fitting FES
  and closes (log `pdu error`).

  In-capsule data on any *other* command is accepted and **discarded**, and
  the command is then answered on its own merits (NP12: an unsupported opcode
  is invalid command opcode, DNR). This rule was corrected in implementation,
  against a live nvme-stas host: stas sends the TP-8010 Discovery Information
  Management command (opcode 21h) with its 1024 byte payload in the capsule to
  every discovery controller it connects to, so the earlier "in-capsule data
  on a non-Connect command is a terminal PDU error" reading put the production
  host stack in a permanent connect/reset loop. The framing cap NP2 exists to
  enforce is unaffected: a PDU past `CdcMaxH2CData` is still refused before a
  byte of it is buffered.
* **NP3 — connection establishment.** ICReq must carry PFV 0 (else
  C2HTermReq). ICResp: PFV 0, CPDA 0, both digests disabled regardless of
  what the host requested (a controller enables only what both sides
  support), MAXH2CDATA = `CdcMaxH2CData`.
* **NP4 — one admin queue.** The first capsule MUST be Fabrics Connect with
  QID 0; SQSIZE is honored up to `CdcMaxAdminSqSize`. A Connect with QID ≠ 0
  is refused (connect invalid parameters — there are no I/O queues). The
  Disable SQ Flow Control attribute is honored; without it SQHD is
  maintained in every response.
* **NP5 — Connect.** Validates the 1024 B connect data: SUBNQN must be
  `NvmeDiscoveryNqn` (else connect invalid parameters, IPO pointing at
  subnqn), HOSTNQN well-formed (≤ 223 bytes); HOSTID is recorded for logs.
  KATO comes from the command. On success: a CNTLID from the round-robin
  `[1, CdcCntlIdMax]` counter is assigned and returned, the connection
  registers under its hostnqn (DS7), and `host connected` is logged. The
  connect data's CNTLID field is **not** validated — nothing in
  `cdc/conn.go` reads it, and the offset in `cdc/pdu.go` is there only for
  symmetry with the fields IPO does point at — because this controller is
  dynamic: it assigns the id and returns it, and every conforming host sends
  `0xffff` there; only a host asking for a static CNTLID could observe the
  difference.
* **NP6 — properties.** Property Get: CAP (MQES = `CdcMaxAdminSqSize` − 1,
  CQR = 1, DSTRD = 0, NVM command set, MPSMIN = MPSMAX = 0, TO per NP14),
  VS (NP14), CC, CSTS. Property Set: CC only (EN, SHN, IOSQES/IOCQES
  accepted). CSTS.RDY follows CC.EN; CC.SHN ⇒ CSTS.SHST = complete and the
  host is expected to close; CC.EN 1→0 marks the controller not ready and
  leaves teardown to the socket. Any other offset: invalid field, DNR.
* **NP7 — Identify.** CNS 01h (controller) only: CNTRLTYPE = discovery
  controller, MDTS = 0, KAS = 10 (100 ms units), AERL = `CdcAerl`, OAES with
  the Discovery Log Page Change bit set, LPA advertising extended NUMD +
  LPO, SGLS advertising SGL with in-capsule data, SUBNQN =
  `NvmeDiscoveryNqn`, MN `dnv`, SN/FR derived from the listen endpoint and
  build; every unlisted field per NP14. Any other CNS: invalid field, DNR.
* **NP8 — Get Log Page.** LID 70h only (anything else: invalid field, DNR —
  nvmet's discovery parser does the same). NUMDL/NUMDU and LPOL/LPOU are
  honored against the DS9 snapshot, and the command's SGL length bounds the
  transfer whatever NUMD asked for — the controller never writes past the
  buffer the host described. LPO must be dword aligned (nvmet says the same);
  one transfer is bounded at 1 MiB and a larger request is refused with
  invalid field, because a controller has to bound the buffer one command can
  make it allocate (hosts read the log in 4 KiB chunks, 256 times
  below it). RAE is accepted and ignored (event clearing is delivery-based,
  §0 #7). Data returns as C2HData followed by a CapsuleResp (the SUCCESS-flag
  shortcut is not used).
* **NP9 — Features.** Set/Get Features 0Bh (Asynchronous Event
  Configuration): stored per connection, default 0 — only the
  discovery-log-change bit is meaningful and it gates DS8 delivery. Set/Get
  0Fh (Keep Alive Timer) SHOULD update the connection's KATO. Any other FID:
  invalid field, DNR.
* **NP10 — Keep Alive.** Every received command restarts the timer (the
  nvmet rule). KATO > 0 expires at KATO + `DefaultCdcKeepAliveGraceMs`;
  KATO = 0 expires after `DefaultCdcZeroKatoTmoMs` idle. Expiry closes the
  socket with `host disconnected`, `reason = keep_alive`.
* **NP11 — AER.** Up to `CdcAerl` + 1 outstanding per connection; one more
  is refused with the AER-limit-exceeded status. An armed AER completes
  immediately when the connection's pending bit is set (and NP9 allows);
  delivery clears the bit; impacts while unarmed coalesce into the bit
  (§0 #7). AERs never complete on disconnect (the queue dies with the
  socket).
* **NP12 — everything else.** Unknown admin opcodes: invalid command
  opcode, DNR — which is what a TP-8010 Discovery Information Management
  command gets (§0 #2), in-capsule payload and all (NP2). Unknown fabrics
  command types: invalid field, DNR. dnv-cdc never fails a well-formed
  supported command for load reasons in v1; NP8's 1 MiB transfer bound is a
  buffer limit stated up front, not a load decision.
* **NP13 — teardown.** Socket close or error, keep-alive expiry, terminal
  PDU error, or a write that does not complete within 30 seconds
  (`socketWriteTimeout` in `cdc/conn.go`, set on every PDU write: a host
  that has stopped reading is gone, and the controller must not hold a
  goroutine and a socket on it forever): cancel timers, discard outstanding
  AERs, unregister from the host state (DS7 — the last connection drops the
  state), log `host disconnected` with the reason. A write that hits the
  deadline has no reason of its own: like any other socket error it ends the
  connection as `reason = closed`, the same value a host that simply went
  away produces — §7's enum has nothing finer. A connection that never
  completed Connect has no hostnqn and no CNTLID and belongs to no host
  state, so it logs nothing here — a port scan is not a host. What went
  wrong with it is already in its `pdu error` record.
* **NP14 — the mirror rule** (§0 #10). Field values and statuses this
  section does not pin follow the reference kernel's nvmet discovery
  controller, byte for byte where hosts can observe them.

---

## 6. `cmd/dnv-cdc` [CM]

* **CM1 — flags** (viper + a cobra root command, no subcommands; env prefix
  `DNV_CDC_`; every flag also settable via `--config` file):

  | flag | default | meaning |
  |---|---|---|
  | `--etcd-endpoints` | *(required)* | comma-separated client endpoints |
  | `--etcd-dial-timeout` | 5 | seconds (the worker's default) |
  | `--range` | `CdcRangeAll` | comma-separated hex digits, each claiming shards `h0…hf` |
  | `--tr-type` | `DefaultCdcTrType` | must be `tcp` |
  | `--adr-fam` | `DefaultCdcAdrFam` | `ipv4` or `ipv6` |
  | `--tr-addr` | *(required)* | listen address |
  | `--tr-svc-id` | `DefaultCdcTrSvcId` | listen port |
  | `--config` | — | optional config file |

* **CM2 — validation, refuse to start on:** a `--range` element not in
  `[0-9a-f]`, a duplicate element, `--tr-type != tcp`, an unparsable
  `--adr-fam`/`--tr-addr`/`--tr-svc-id`, empty `--etcd-endpoints`.
* **CM3 — wiring.** Build the `etcdutil` client, hand off to `cdc.Run`
  (watcher + listener + view registry). No interceptors (`grpc.md`
  unchanged). `common/log.go`'s `init` installs the JSON logger.
* **CM4/CM5 — lifecycle logs.** `cdc starting` first, `cdc stopping` on
  SIGTERM/SIGINT after the listener and watcher have stopped.

Invocation reference (`architecture.md` §13, amended per §10):

```shell
dnv-cdc --etcd-endpoints 192.168.0.10:2379,192.168.0.11:2379 \
  --range 0,1,2,3,4,5,6,7 \
  --tr-type tcp --adr-fam ipv4 --tr-addr 192.168.0.10 --tr-svc-id 8009
```

Production placement: one dnv-cdc per CP server (fig. `070Cluster`), twins of
a range on different CP servers, all endpoints listed on every host.

---

## 7. Log records [LG]

JSON per `log.md`, `Info` unless the row says otherwise. The `msg` strings are
normative — the §9 suite parses them.

| msg | attributes | when |
|---|---|---|
| `cdc starting` | `ranges`, `endpoints`, `tr_addr`, `tr_svc_id` | CM4 |
| `cdc scan complete` | `entries` (owned count), `rev` | WV1, every (re)scan |
| `cdc watch restarting` | `error`, `compacted` (bool) | WV4 |
| `cdc entry applied` | `key`, `op` (`put`/`delete`) | WV3 |
| `cdc entry skipped` | `key`, `reason` (`malformed_key`/`malformed_value`/`foreign_tr_type`/`foreign_adr_fam`) | WV2, DS3 |
| `host connected` | `hostnqn`, `hostid`, `cntlid`, `kato_ms`, `remote` | NP5 |
| `host disconnected` | `hostnqn`, `cntlid`, `reason` (`closed`/`keep_alive`/`pdu_error`/`shutdown`) | NP13 |
| `view changed` | `hostnqn`, `genctr`, `numrec` | DS6, per impacted host |
| `aen sent` | `hostnqn`, `cntlid`, `genctr` | DS8, per delivered AER |
| `pdu error` | `remote`, `reason` | NP2 |
| `cdc accept failed` (`Error`) | `error` | NP1, a listener error that is not the shutdown |
| `signal received` | `signal` | CM5, the first SIGINT/SIGTERM |
| `second signal, exiting without a clean drain` (`Warn`) | `signal` | CM5 |
| `etcd client close failed` (`Error`) | `error` | CM5, best effort |
| `cdc stopping` | — | CM5 |

Plus the `etcd *` records of `etcdutil`. Only the rows above are normative:
nothing in §9 may key off the `Error`/`Warn` rows, which exist so a failure
is visible in the diagnostics, not so a test can count them.

---

## 8. Unit tests

Colocated `_test.go` in `cdc/` (plus the `model` parser tests of §2.2). None
of them is etcd-backed and the EU7 rule therefore never applies here: the
watcher reaches etcd only through the narrow `etcdStore` interface (WV6), so
`cdc/` drives it with an in-memory fake store — which is also what lets WV6
be asserted structurally, by recording every call made through that
interface — and the §2.2 goldens are pure parsing in `model/keys_test.go`.
Fakes: a fake clock for the keep-alive deadlines (NP10) and the WV5 rescan
retry; an **in-process fake NVMe/TCP host** — a small test-only client
speaking NP2/NP3 over a loopback socket — for the server tests. Those
deadlines and that retry are the only timers that go through the package
clock: NP1's `acceptRetryDelay` pause and NP13's `socketWriteTimeout`
deadline call `time` directly, and no unit test exercises either.

* **view.go / logpage.go** — DS3 rendering goldens (a fixture entry to exact
  bytes, header and entry); skip-and-serve on a foreign `tr_type`; PORTID
  determinism; DS4 visibility table (empty list, member, non-member); DS5
  ordering; DS6 impact rows: gain, loss by `allowed_hosts` removal, tr-conf
  change on a visible entry, invisible-both-sides no-op, the
  membership-preserving `allowed_hosts` edit no-op, empty-`allowed_hosts`
  fan-out; GENCTR bumps exactly on impact; DS9 paging math (aligned and
  odd offsets, zero fill past the end) and snapshot isolation.
* **watch.go** — scan builds the owned map (foreign shards silently out,
  malformed logged out); put/delete events flow to impacts; the WV4 rescan's
  wholesale replace still emits impacts for changes missed across the gap;
  WV5 retry cadence on a fake clock.
* **server.go / conn.go / pdu.go** — handshake (PFV, digests off,
  MAXH2CDATA); Connect happy path and each NP5 reject; property dance to
  RDY; Identify fields (CNTRLTYPE, OAES, AERL, KAS); Get Log Page paged
  reads against an injected view; features 0Bh gating (no AEN before
  enable, AEN after); AER arm → impact → completion value, impact → arm →
  immediate completion, coalescing, AERL exceeded; keep-alive reset and
  expiry (KATO > 0 and the zero-KATO idle cutoff) on the fake clock; SQHD
  with and without disable-sqflow; C2HTermReq on garbage; NP13 unregister
  dropping the host state at last disconnect.
* **cmd** — CM2 rejects, `CdcRangeAll` default, env/flag precedence.

---

## 9. Integration test plan

### 9.1 Goal and scope

Prove a real dnv-cdc fleet — real `cdc/`, `model/`, `etcdutil/`, a real
single-node etcd — against **real kernel NVMe hosts** and **real nvmet
targets**, across four servers. What this suite proves: range sharding and
twin equality, per-host filtering, the per-host AEN/GENCTR semantics of
§0 #5, end-to-end automatic connect / re-point / disconnect through
nvme-stas, and twin/restart HA. What it does not: the gateway→`CdcEntry`
pipeline (worker suite), data-path correctness beyond one read (agent
suites), etcd failures, and §9.17.

| case | name | proves |
|---|---|---|
| S | `smoke` | bring-up: scan, one entry, per-range serving, connect, dm-zero read |
| M | `matrix` | the full instance × host discover grid, twins, unions, 2-paths-1-subsystem, cross-cluster, connect/absence per host |
| L | `lowlevel` | plain nvme-cli: persistent discovery, AEN uevent to impacted hosts only, per-host GENCTR, the no-op `allowed_hosts` edit |
| T | `stas` | nvme-stas end to end: converge, auto-connect, re-point on a tr-conf move, auto-disconnect, mass disconnect on wipe |
| H | `ha` | twin kill/serve/restart equality; full-fleet restart under live hosts |

Cases run in that order, fail-fast, each against a wiped `{p} cdc` prefix,
its own cluster ids and a restarted cdc fleet (§9.9), so nothing leaks
between cases.

### 9.2 Deliverables and usage contract

Two artifacts under `integtest/`, next to the four existing suites
(`dnagent_test.sh`, `cnagent_test.sh`, `worker_test.sh`, `gateway_test.sh`):

* `integtest/cdc_test.sh` — bash, `set -euo pipefail`, the orchestrator.
* `integtest/cdcctl/main.go` — the etcd driver that plays gateway + worker
  for `CdcEntry` keys only (§9.6). Imports `pb`, `common`, `model`,
  `etcdutil`. Runs **on server 1** (etcd listens on localhost) via ssh.

Two more scripts run on the remote servers — `cdc_target.sh` on s2 (§9.7) and
`cdc_host.sh` on h1/h2 (§9.8, §9.15). They are GENERATED by `cdc_test.sh` into
a temporary directory and scp'd, rather than kept as files of their own, so
that everything the suite is lives in the two artifacts above. Both need root
and are always invoked through `sudo`.

The script builds `bin/dnv-cdc` (`make build`) and `cdcctl` into
`integtest/bin/`, reuses the worker suite's pinned etcd tarball cache
(same version, same sha256, `integtest/bin/cache/`), and `scp`s
`etcd etcdctl dnv-cdc cdcctl` to server 1 and the target helper (§9.7) to
server 2.

```
bash integtest/cdc_test.sh [--only <case>] [--cleanup-only] \
    user@<s1> user@<s2> user@<h1> user@<h2>
```

* Exactly four positional args: **s1** = etcd + the cdc fleet (plain user,
  **no sudo**), **s2** = the nvmet target server (passwordless **sudo**),
  **h1**/**h2** = the hosts (passwordless **sudo**). Passwordless ssh to all
  four, checked in preflight.
* `--only <case>`: `smoke|matrix|lowlevel|stas|ha`; setup and start-cleanup
  still run (`--only stas` and `--only ha` also start the stas daemons).
* `--cleanup-only`: scrub all four servers and exit.
* No prompts; exit 0 on success, non-zero on first failure. Cleanup at
  **start, always**; at **end only on success** — a failing run leaves etcd
  data, cdc logs, nvmet/dm state and host connections in place and dumps
  §9.16 diagnostics.
* Lab note: this suite occupies **four** VMs — both lab pairs at once — so
  no other suite may run anywhere in the lab concurrently.

### 9.3 Topology

```
 driver (dev box)
   | ssh + scp (orchestration)
   v
 s1  (no sudo)                          s2  (sudo)
   etcd        127.0.0.1:13379/13380      nvmet ports (tcp, <ip2>):
   cdc0  --range 0..7  <ip1>:18009          port1 14420   port2 14421
   cdc1  --range 0..7  <ip1>:18010          port3 14422   port4 14423
   cdc2  --range 8..f  <ip1>:18011        subsystems ssA…ssF, ssX
   cdc3  --range 8..f  <ip1>:18012        namespaces: dm-zero, 64 MiB
   cdcctl  (via ssh, against 127.0.0.1)   attr_allow_any_host=1 (§0 #14)

 h1, h2  (sudo): nvme-cli + nvme-stas; identities = /etc/nvme/hostnqn
   stafd.conf lists all four <ip1>:1800x endpoints (cases T, H)
```

* Ports collide with no other suite (worker: 12379/12380, 296xx/297xx;
  agents: 29528/29529/4200).
* Per-server layout, all under `WORK=/var/tmp/dnv-cdc-integtest`:

```
s1:  bin/ (etcd etcdctl dnv-cdc cdcctl)   etcd/ (data, etcd.log)
     cdc0/ … cdc3/  cdc.log  pid
s2:  cdc_target.sh                        (all real state is kernel state)
h*:  uevents.log  stas-backup/            (captures, saved original confs)
```

* Launch lines (`nohup … &` over ssh; the JSON log is on stderr, which the
  `2>&1` in each line merges into the file). The redirect is `>>`, so a
  relaunched instance appends: §9.9's per-case reset is the only thing that
  truncates a `cdc.log`, and a mid-case restart's records land past the
  baseline that case counted from.

```
$WORK/bin/etcd --name dnv-cdc-it --data-dir $WORK/etcd \
  --listen-client-urls http://127.0.0.1:13379 --advertise-client-urls http://127.0.0.1:13379 \
  --listen-peer-urls http://127.0.0.1:13380 --initial-advertise-peer-urls http://127.0.0.1:13380 \
  --initial-cluster dnv-cdc-it=http://127.0.0.1:13380 >> $WORK/etcd/etcd.log 2>&1 &

$WORK/bin/dnv-cdc --etcd-endpoints 127.0.0.1:13379 --range 0,1,2,3,4,5,6,7 \
  --tr-type tcp --adr-fam ipv4 --tr-addr <ip1> --tr-svc-id 18009 \
  >> $WORK/cdc0/cdc.log 2>&1 &
```

* PIDs from `$!` into `$WORK/cdcN/pid`; signals by PID; cleanup falls back
  to `pkill -f 'bin/dnv-cdc'`, `pkill -f 'etcd --name dnv-cdc-it'`.

### 9.4 Assumptions and preflight checks

Aborts with a message on the first failure:

1. `ssh -o BatchMode=yes` works to all four; `sudo -n true` works on s2, h1,
   h2 (and is **not** required on s1).
2. s1: ports 13379/13380/18009-18012 free; `$WORK` writable.
3. s2: `modprobe nvmet nvmet-tcp` succeeds; `/sys/kernel/config/nvmet`
   present; `dmsetup targets` lists `zero`; ports 14420-14423 free.
4. h1/h2: `modprobe nvme-tcp` succeeds; `nvme-cli` present (version logged);
   `/etc/nvme/hostnqn` and `/etc/nvme/hostid` exist (created once with
   `nvme gen-hostnqn` / `uuidgen` under sudo if absent) and the two hosts'
   hostnqns differ; `udevadm` present.
5. h1/h2: nvme-stas present or installable: `command -v stafd || sudo
   apt-get install -y nvme-stas` (the one step needing outbound network;
   version logged; the package is **left installed** by cleanup). `systemctl
   status stafd stacd` resolvable. The exact stas conf keys for "connect
   what the DLPEs say, disconnect what they stop saying" differ across stas
   1.x/2.x; the implementation pins them for the lab's version and the
   preflight asserts that version.
6. Driver box: `jq` — or the Go toolchain: a missing `jq` does not abort,
   the runner builds the `gojq` drop-in into `integtest/bin/` and uses it,
   aborting only if that build fails too (all log/JSON parsing is local,
   `ssh cat … | jq`).
7. The kernel hostnqn/hostid 1:1 rule: every `nvme` invocation that passes
   `-q`/`--hostnqn` also passes the matching `-I`/`--hostid` (the lab trap:
   an implicit hostid with an explicit foreign hostnqn fails `EINVAL`).

### 9.5 Identity plan

* **Clusters** (fabricated ids, §0 #13; no `ClusterConf` exists or is
  needed): per case — S `0xcdc1`, M `0xcdc2`, L `0xcdc3`, T `0xcdc4`,
  H `0xcdc5`; the cross-cluster assertions use a second id = case id +
  `0x10000` (M: `0x1cdc2` — the prepended `1` nibble).
* **Shard codes**: `00`, `07` (low half), `3c` (low interior), `80`, `ff`
  (high-half edges), `81` (high interior, the cross-cluster entry). The high
  half starts at `80` because DS2 gives range digit `h` the codes `h0…hf`: an
  earlier draft wrote ssC's shard as `08`, which range 0 owns and which would
  therefore have put ssC in the LOW half, contradicting §9.11's grid.
* **Subsystems** (created once on s2 at setup, case-agnostic; cases differ
  only in etcd contents):

  | ss | NQN | nvmet ports | ns uuid |
  |---|---|---|---|
  | ssA | `nqn.2024-01.io.dnv-it:cdc:ssa` | 14420 | `0000cdc0-…-0001` |
  | ssB | `…:cdc:ssb` | 14421 | `…-0002` |
  | ssC | `…:cdc:ssc` | 14422 | `…-0003` |
  | ssD | `…:cdc:ssd` | 14420 **and** 14421 | `…-0004` |
  | ssE | `…:cdc:sse` | 14422, case T adds 14423 | `…-0005` |
  | ssF | `…:cdc:ssf` | 14423 | `…-0006` |
  | ssX | `…:cdc:ssx` | 14423 | `…-0007` |

  Full uuids `0000cdc0-0000-4000-8000-00000000000N`; host waits go through
  `/dev/disk/by-id/nvme-uuid.<uuid>`.
* **Host identities**: `H1`/`H2` = the hosts' `/etc/nvme/hostnqn`, read at
  setup. `H3` ("ghost") = `nqn.2024-01.io.dnv-it:cdc:ghost` with a
  per-run `uuidgen` hostid, used only via explicit `-q`/`-I` from h1
  (§9.4 item 7).
* **dm-zero devices** on s2: `dnv-cdc-it-<ss>`, 64 MiB
  (`dmsetup create dnv-cdc-it-ssa --table "0 131072 zero"`).
* **The standard entry set** (what "put the matrix" means; per-case cluster
  ids substituted; `port<n>` = `(tcp, ipv4, <ip2>, 1442<n-1>)`):

  | entry | shard | sp/ss ids | nqn | tr confs | allowed_hosts |
  |---|---|---|---|---|---|
  | ssA | `00` | `0x1/0xa` | ssA | port1 | *(empty — everyone)* |
  | ssB | `07` | `0x1/0xb` | ssB | port2 | H1 |
  | ssC | `80` | `0x2/0xc` | ssC | port3 | H2 |
  | ssD | `ff` | `0x2/0xd` | ssD | port1, port2 | H1, H2 |
  | ssE | `3c` | `0x3/0xe` | ssE | port3 | H1 |
  | ssF | `81` | `0x4/0xf` | ssF | port4 | H2 — in the **second** cluster |

### 9.6 The driver: `cdcctl`

Verbs (etcd endpoint from `--etcd`, default `127.0.0.1:13379`; `--endpoints`
is accepted as an alias for it — `workerctl` spells the same flag that way,
and the two suites are driven by the same hands, so the other spelling is
honored rather than dying as an unknown flag):

* `put --cluster <cid> --shard <hex> --sp <id> --ss <id> --nqn <nqn>
  --tr <type,fam,addr,svcid>… [--allowed <hostnqn>]…` — marshal a
  `CdcEntry`, put at `model.CdcEntryKey`. Repeatable `--tr` preserves
  order (it is the DS5 tr-conf index).
* `del --cluster --shard --sp --ss` — delete the key.
* `wipe` — delete `model.CdcEntryPrefix()` (the per-case reset).
* `list` — every `CdcEntry` as `key\t<protojson>` (diagnostics).
* `ping` — a point read of a key nothing ever writes, so that "not found"
  is success: the setup step waits on it to know etcd is answering.

### 9.7 Server 2: `cdc_target.sh`

A helper scp'd to s2 and invoked with sudo; verbs `setup`, `link <ss>
<port>`, `unlink <ss> <port>`, `teardown`. `setup` is idempotent (rerun
tolerant, the nvmet configfs rules baked in: create-guarded `mkdir`,
attributes written only when the object is first created — `attr_serial`
reads back space-padded and `device_uuid` reads back reformatted, so
equality re-checks are wrong by construction):

1. `modprobe nvmet nvmet-tcp`; the dm-zero devices of §9.5.
2. Ports 1-4: `addr_trtype=tcp`, `addr_adrfam=ipv4`, `addr_traddr=<ip2>`,
   `addr_trsvcid=1442{0..3}`.
3. Subsystems ssA…ssX: `attr_allow_any_host=1` (§0 #14), `attr_model=dnv-it`;
   one namespace each (`nsid 1`, `device_path=/dev/mapper/dnv-cdc-it-<ss>`,
   `device_uuid` per §9.5, enabled).
4. Port links per the §9.5 table (ssD on two ports gives the same subsystem
   two paths; the one kernel hands out distinct CNTLIDs itself).

`teardown` (also run by start-cleanup, tolerant of absence): unlink every
port link **first** (hosts are already disconnected by §9.15 order — an
unlink under a live connection kills the controller with DNR and the host
never reconnects by itself), then namespaces → subsystems → ports → `dmsetup
remove dnv-cdc-it-*`. Modules stay loaded.

### 9.8 Hosts: nvme-stas control

* `stas_start`: back up any existing `/etc/stas/*.conf` to
  `$WORK/stas-backup/`, write our `stafd.conf` — four
  `controller = transport=tcp;traddr=<ip1>;trsvcid=…` lines for 18009,
  18010, 18011, 18012, and mDNS/avahi discovery off — and our `stacd.conf`
  (`disconnect-scope = only-stas-connections` plus
  `disconnect-trtypes = tcp`: connect every DLPE, disconnect the connections
  stacd itself made once their DLPE vanishes), then
  `systemctl restart stafd stacd` and assert both active.
* The kernel's own autoconnect rule
  (`/usr/lib/udev/rules.d/70-nvmf-autoconnect.rules`, which starts
  `nvmf-connect@.service` on the very AEN this suite measures) is disabled by
  **masking that unit** — and `nvmf-connect.target` with it — at setup, and
  unmasking both at end-cleanup. nvme-stas 2.x has no knob of its own for
  this, and the rule is masked for EVERY case, not only the stas ones: it
  would otherwise connect behind case L's back the instant an AEN landed,
  which is precisely what case L is measuring.
* `stas_stop`: stop both daemons, restore the backups (or remove our
  confs). Cases S, M, L run with the daemons **stopped**; T starts them; H
  inherits them; teardown stops them.
* The rest of the host side lives in the same helper: verbs `wipe`
  (disconnect every `dnv-it` subsystem **and** every discovery controller
  pointing at `<ip1>`), `wipe_data` (the `dnv-it` subsystems only),
  `mask`/`unmask` (the autoconnect unit and target above), and
  `uev_start`/`uev_stop` (the §9.12 uevent capture, whose `udevadm monitor`
  must be started and reaped on the host itself). Those are the actions
  carrying state a one-liner would have to re-derive — a sysfs walk that
  must skip non-test subsystems (the `disconnect-all` ban of §9.9), a unit
  to remember across mask/unmask, a background monitor to reap — so they
  are written once on the host; the self-contained calls
  (`nvme connect-all`, `nvme discover`, `nvme disconnect -d <dev>`) stay
  ssh one-liners. Privilege is not the line: every host command, one-liners
  included, is dispatched through the suite's `sudo -n bash -c` wrapper
  (its non-sudo twin serves reads only).
* Assertions about stas behavior are made against **kernel state** —
  `nvme list-subsys -o json` (always the bare, no-argument form: with a
  device argument the output is ambiguous) and
  `/dev/disk/by-id/nvme-uuid.*` waits — never against `stafctl`/`stacctl`
  output, whose format varies by version; `stafctl ls`/`status` and
  `stacctl ls`/`status` are captured as §9.16 diagnostics only.

### 9.9 Conventions

* **Polling**: `wait_until <secs> <label> <cmd>` at 1 s; budgets
  `WAIT_SHORT=5` (scan/log lines), `WAIT_AEN=15` (uevent after a put),
  `WAIT_CONN=20` (device nodes), `WAIT_STAS=30` (daemon convergence).
* **Log reading**: `ssh <s1> cat $WORK/cdcN/cdc.log | jq -R 'fromjson? //
  empty'` (the worker suite's torn-line-tolerant `recs` helper); the §7
  msg strings are matched exactly.
* **Discover form**: `nvme discover -t tcp -a <ip1> -s <port> -o json`
  (+ `-q <H3> -I <hostid3>` for the ghost). Assertions compare the set of
  `(subnqn, traddr, trsvcid)` triples: a `jq` projection of `.records[]` to
  one sorted `subnqn|traddr|trsvcid` line each, against the same rendering
  of the expected set — not two whole JSON documents, so a mismatch prints
  the two sorted sets rather than a JSON diff (the polling twin `wait_disc`
  prints only `wait_until`'s label: a poll has no one answer to show).
  `genctr` comes from the same output. GENCTR assertions are only
  meaningful on a **persistent** connection
  (`nvme discover --device nvmeN` re-reads; one-shot discovers create and
  drop the DS7 host state each time) — case L only.
* **Per-case reset**: kill cdc0-3 → `cdcctl wipe` → truncate the four
  `cdc.log` files, so that every §7 line count a case makes counts only its
  own records → relaunch the fleet → wait four `cdc scan complete` lines →
  on both hosts, the §9.8 `wipe` verb: disconnect every subsystem whose
  subsysnqn starts `nqn.2024-01.io.dnv-it:` (sysfs walk +
  `nvme disconnect -n`; **never** `nvme disconnect-all`, which would take
  down non-test subsystems) **and** every discovery controller pointing at
  `<ip1>`.
  Whenever the stas daemons are already running as the reset runs — H's
  reset in a full run, since a case's reset precedes its body and T is what
  starts the daemons — that host step is the `wipe_data` verb instead,
  which leaves the discovery controllers to stafd: the fleet restart above
  already broke them and let stafd re-establish them, and one taken down a
  second time behind stafd's back is treated as gone for good and never
  re-created, so the host would silently stop receiving AENs for the rest
  of the run. s2 is built once at setup; the only case that mutates it
  (T, the ssE port move) restores the setup shape before finishing
  (§9.13 step 7), so every case sees identical target topology.
* **Connect form** (non-stas cases): `nvme connect-all -t tcp -a <ip1> -s
  <port>` with the host's default identity; devices awaited by uuid.

### 9.10 Case S — `smoke`

1. `put` ssA (cluster `0xcdc1`, shard `00`, open, port1).
2. Discover: cdc0 shows exactly {ssA}; cdc2 (high half) shows {} — range
   filtering at the instance boundary. cdc0's log has `cdc entry applied`
   `op=put` and `view changed` appears only after a host connects (no
   active hosts yet — assert **no** `view changed` line exists).
3. h1 `nvme connect-all` via cdc0; wait `…nvme-uuid.…0001`;
   `nvme list-subsys` shows ssA live at `<ip2>:14420`.
4. Read 4 KiB from the device (`dd` without `iflag=`, lab rule), compare
   to zeros (`cmp` against `/dev/zero` head) — the dm-zero data path.
5. cdc0 log: `host connected` with H1 and `host disconnected`
   `reason=closed` — both records come from the transient **discovery**
   connections steps 2-3 already made (a one-shot discover / connect-all
   opens and closes a discovery connection; the assert runs before the next
   sentence's disconnect). Then disconnect ssA on h1 — a *data*-subsystem
   disconnect touches only s2's nvmet and produces no cdc record.
6. `del` ssA; discover against cdc0 → {}.

Proves: build, launch, scan/watch, per-range serving, a real host connect,
the dm-zero backend, clean entry deletion.

### 9.11 Case M — `matrix`

1. `put` the full §9.5 entry set (cluster `0xcdc2`; ssF in `0x1cdc2`).
2. The discover grid — 4 instances × 3 identities (H1, H2, ghost), every
   cell exact:

   | | cdc0 | cdc1 | cdc2 | cdc3 |
   |---|---|---|---|---|
   | H1 | A,B,E | A,B,E | D | D |
   | H2 | A | A | C,D,F | C,D,F |
   | ghost | A | A | *(empty)* | *(empty)* |

   Twins identical (the same sorted triple set); per-host unions complete
   (H1: A,B,D,E; H2: A,C,D,F); the ghost row proves both
   open-entry-visible-to-anyone (A) and the genuinely empty log; the empty
   cells prove filtering, the column split proves sharding, ssF (second
   cluster id) proves the all-clusters watch.
3. ssD's JSON at cdc2 contains **two** records, same subnqn, trsvcids
   14420 and 14421.
4. h1: `connect-all` against all four instances; h2 likewise. Assert
   present/absent by uuid: h1 has 1,2,4,5 and **not** 3,6; h2 has 1,3,4,6
   and **not** 2,5. On h2, ssD's subsystem shows **two** live paths
   (`nvme list-subsys -o json`: two controllers, 14420 + 14421) — one
   subsystem, two simulated CNs.
5. Disconnect all `dnv-it` subsystems on both hosts.

Proves: the whole DS2-DS5 serving surface against real kernels.

### 9.12 Case L — `lowlevel` (stas stopped)

Cluster `0xcdc3`. Start: `put` ssA (open, port1), ssB (H1, port2).

1. h1: `nvme discover … -s 18009 --persistent --keep-alive-tmo 5` →
   controller `nvmeX` (recorded); h2: likewise → `nvmeY`. Baseline reads
   via `nvme discover --device`: h1 {A,B} genctr `g1`, h2 {A} genctr `g2`.
2. Start `sudo udevadm monitor --kernel --property --subsystem-match=nvme`
   captures into `$WORK/uevents.log` on both hosts.
3. `put` ssE (shard `3c`, H1, port3) — impacts h1 only. Assert within
   `WAIT_AEN`: h1's capture gains an `NVME_AEN=0x70f002` line for `nvmeX`;
   h1 re-read: genctr > `g1`, records {A,B,E}. Then assert h2's capture has
   **no** discovery event for `nvmeY` (checked after h1's positive, plus a
   3 s settle) and h2's genctr still equals `g2` — the §0 #5 negative.

   (`NVME_AEN=0x70f002`, not the `NVME_EVENT=discovery` an earlier draft
   named: the AER completion's dword 0 is 0x0070f002 — Notice / Discovery Log
   Page Changed / LID 70h — and `drivers/nvme/host/core.c` publishes it
   verbatim through `nvme_aen_uevent`. `NVME_EVENT=` exists too, but only as
   `connected` and `rediscover`, neither of which is the AEN.)
4. Rewrite ssB with `allowed_hosts = [H1, H2]` — h2 gains an entry (AEN +
   genctr bump + {A,B}); h1's rendered view is **unchanged** (allowed_hosts
   is not log-page content): assert no new h1 event and genctr stable —
   the DS6 content-not-key rule, observable only here.
5. Rewrite ssE with `allowed_hosts = [H2]` — h1 **loses** the entry (AEN,
   {A,B}); h2 gains it (AEN, {A,B,E}).
6. `del` ssA (open) — both hosts AEN: h1 → {B}, h2 → {B,E}.
7. Keep-alive: sleep ≥ 3 × the 5 s KATO of step 1; assert both
   controllers still live and no `host disconnected reason=keep_alive` on
   cdc0 (forcing expiry is unit-test territory, not hardware).
8. Cleanup: stop the captures; `nvme disconnect --device` both persistent
   controllers; cdc0 logs two `host disconnected reason=closed`.

Proves: DS6/DS7/DS8 exactly as decided in §0 #5-#7, on stock kernels with
no stas anywhere.

### 9.13 Case T — `stas`

Cluster `0xcdc4` (ssF in `0x1cdc4`). `put` ssA, ssB, ssC, ssD, ssE
(H1, port3), ssF first; then `stas_start` on both hosts.

1. Convergence: each host's kernel gains four persistent discovery
   controllers to `<ip1>:18009-18012` (assert via `nvme list-subsys -o
   json` on the discovery subsystem NQN filtered by traddr `<ip1>`); cdc
   logs show `host connected` for both hostnqns on all four instances.
2. Data connections appear with **no manual nvme commands**: h1 → uuids
   {1,2,4,5}, h2 → {1,3,4,6}, ssD with two paths on both, each device node
   within `WAIT_CONN` (§9.9's budget for device nodes).
3. Auto-connect: `put` ssX (shard `05`, H2, port4) → h2 gains uuid 7; h1
   unchanged (after h2's positive + settle).
4. Re-point (the ReplaceCntlr shape): `cdc_target.sh link sse 4`; rewrite
   ssE's entry `port3 → port4`; assert h1's ssE subsystem re-points —
   a path via 14423 appears, the 14422 path is disconnected by stacd, the
   uuid-5 device survives throughout; then `cdc_target.sh unlink sse 3`
   (graceful order: new path exists before the old link dies).
5. Auto-disconnect: `del` ssB → h1's uuid-2 device and its connection go
   away.
6. Mass teardown as an assertion: `cdcctl wipe` → both hosts converge to
   **zero** `dnv-it` subsystems (stacd disconnects everything whose DLPE
   vanished). Daemons stay running for case H.
7. Restore s2 to the setup shape (hosts are already disconnected):
   `cdc_target.sh link sse 3; cdc_target.sh unlink sse 4` — so every case,
   under `--only` too, starts from the identical target topology.

Proves: §0 #12's production stack end to end — the AENs of DS6 driving
stas's connect / re-point / disconnect with no operator action.

### 9.14 Case H — `ha` (stas running)

Cluster `0xcdc5`. `put` ssA (open, port1), ssC (H2, port3); wait
convergence (h1 {1}, h2 {1,3}).

1. `SIGKILL` cdc0 (its pid file). stas loses one discovery connection and
   keeps three.
2. `put` ssB (shard `07`, H1, port2) — the low half is now served by cdc1
   alone: h1 auto-connects uuid 2 within `WAIT_CONN` (§9.9's device-node
   budget). One-shot discover
   against cdc1 (H1) shows {A,B} while cdc0 is down.
3. Restart cdc0; wait its `cdc scan complete`; discover H1 against cdc0 =
   against cdc1, the same §9.9 triple set and non-empty (two failed
   discovers also "agree") — twins re-converge from etcd alone.
4. Full-fleet restart under load: `SIGKILL` all four, relaunch, wait four
   scans; assert stas re-established 4 × 2 discovery connections (`host
   connected` lines post-restart) and data connections are undisturbed
   throughout (uuid waits never dropped).
5. Post-restart AEN path: `put` ssE (H1, `3c`, port3 — the setup-shape
   link, §9.13 step 7) → h1 auto-connects uuid 5.

Proves: twin redundancy is real (a dead instance costs nothing), restart
recovers identical served state from etcd, and DS11's
reconnect-and-catch-up works under stas.

### 9.15 Teardown and cleanup (`cleanup()`, also `--cleanup-only`)

Order matters; every step tolerant of absence. Start-cleanup runs all of
it; end-cleanup (success only) additionally removes `$WORK` everywhere.

1. **Hosts first**: `stas_stop` (kills the reconnect loops before their
   targets vanish); kill any `udevadm monitor`; disconnect every
   `nqn.2024-01.io.dnv-it:*` subsystem and every discovery controller
   whose traddr is `<ip1>` (sysfs walk; never `disconnect-all`).
2. **s2**: `cdc_target.sh teardown` (§9.7 — links first, precisely because
   step 1 already ran).
3. **s1**: kill cdc0-3 and etcd by pid (pkill fallbacks), leaving
   `$WORK` in place unless end-cleanup.
4. nvme-stas stays installed (§9.4-5); kernel modules stay loaded.

### 9.16 Failure diagnostics

Dumped to stdout on any failure, each under a banner: s1 — the four cdc
logs (tail -100 through the jq filter) and `cdcctl list`; s2 — `find
/sys/kernel/config/nvmet` and `dmsetup ls --target zero` + tables; hosts —
`nvme list-subsys -o json`, `nvme list -o json`, the uevent captures,
`journalctl -u stafd -u stacd --since <run start>`, `stafctl ls`/`status`,
`stacctl ls`/`status`, `dmesg | tail -50`.

### 9.17 Out of scope (v1)

etcd quorum loss and compaction races; TLS / in-band auth; non-TCP
transports; digests; scale (256 shards, many hosts, log paging past one
page is unit-tested only); mDNS (TP-8009); TP-8010 registration; the
nvme-stas version matrix (one pinned lab version); forcing keep-alive
expiry on hardware; AERL exhaustion on hardware; referrals.

---

## 10. Amendments to companion documents

**Applied.** They were recorded here first — the recorded-then-edited-in
amendment pattern — and
edited into the companion documents when implementation started;
`layout.md` §8 records its own half.

* `architecture.md` §12 — replace "emits discovery-log-change AENs on any
  watched change" with the per-host semantics of §0 #5/#6 (impact-scoped
  AENs, per-host GENCTR) and add the listen flags to the §12 command line;
  note `--range` defaults to all ranges.
* `architecture.md` §13 — the `dnv-cdc` invocation gains
  `--tr-type/--adr-fam/--tr-addr/--tr-svc-id` (§6 CM1).
* `layout.md` §2 — `cdc/` becomes the §1 file split (`cdc.go`, `watch.go`,
  `view.go`, `logpage.go`, `server.go`, `conn.go`, `pdu.go`); the `doc/`
  tree gains `cdc.md`; `integtest/` gains `cdc_test.sh` and `cdcctl/`;
  §5 gains a `cmd/dnv-cdc` bullet (viper + cobra root, no subcommands,
  env prefix `DNV_CDC_`); §6 step 7 cites this document. Package
  boundaries are unchanged (`cdc` already imports `common`, `pb`,
  `etcdutil`, `model` per §3 there).
* `dnv-worker.md` §4 (MD2) — `model/keys.go` gains `ParseCdcEntryKey`
  (§2.2 here).
* `layout.md` §3 `integtest/*` row — `cdcctl` joins `workerctl` as a
  `model` + `etcdutil` importer.
