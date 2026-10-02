# cdc.md — `dnv-cdc`: NVMe-oF Central Discovery Controller

This document owns the `dnv-cdc` binary: package `cdc` and the
`cmd/dnv-cdc` command, with the discovery service model (DS1 to DS11), the
etcd watcher (WV1 to WV6), the NVMe/TCP discovery service (NP1 to NP14),
the command (CM1 to CM5), its log records and the intent of the cdc
integration suite. It leans on `architecture.md` for the system context,
the etcd data model with the `CdcEntry` key, and the RPCs that write the
entries; on `dnv-worker.md` for `etcdutil` (EU1 to EU7) and the key helpers
of `model` (MD2); on `log.md` and `grpc.md` for logging and trace ids; and
on `layout.md` for package placement and the dependency rules.
"The specs" are the NVMe Base 2.x, NVMe over Fabrics and NVMe/TCP
specifications: the byte encodings — the identify layout, the log entry
layout, the status codes — are theirs, and this document names fields,
never offsets. "nvmet" is the Linux kernel NVMe target, whose discovery
controller is the rule wherever this document is silent (NP14).

## Scope and placement

`dnv-cdc` is the control-plane process that serves NVMe-oF discovery to
hosts (`architecture.md`, System overview and dnv-cdc): it watches the
`{p} cdc` keys, keeps a per-host filtered view of the discovery log,
answers the well-known discovery subsystem NQN, `NvmeDiscoveryNqn`, over
NVMe/TCP, and sends Discovery Log Page Change AENs to exactly the hosts a
change impacts. It never writes etcd, never serves or dials gRPC, so it
chains no interceptor (`grpc.md`, Wiring), and never touches the local
kernel's nvmet.

The discovery controller is implemented in userspace and served straight
from etcd, with no kernel state to converge, rather than built as a hub of
kernel nvmet referrals. A referral hub built from a local nvmet port cannot
filter per host, because referrals are served to every connecting host; it
can only redirect, so each host would still need a persistent discovery
connection per controller node, defeating centralization; it needs an
anchor subsystem just to listen, because a referral-only nvmet port never
comes up; and its AENs cannot carry per-host meaning.

Who writes what it reads: the gateway creates and deletes `CdcEntry` at
`CreateSubsystem` and `DeleteSubsystem`, puts a missing one back when an
`UpdateSubsystemHosts`, `CreateCntlr`, `DeleteCntlr` or flag-changing
`UpdateCntlrEnabled` commits, and rewrites `allowed_hosts` at
`UpdateSubsystemHosts`; gateway and worker rewrite `nvme_tr_conf_list` at
`CreateCntlr`, `DeleteCntlr`, `UpdateCntlrEnabled` and `ReplaceCntlr`
(`architecture.md`, `service Gateway` — RPC specifications; `dnv-worker.md`
MD8). `dnv-cdc` is the only serving consumer; the gateway and worker also
read entries inside their own read-modify-write STMs (the `allowed_hosts`
and `nvme_tr_conf_list` rewrites above), but nothing else ever renders them
to a host.

One process, three parts:

```
dnv-cdc process  (--range …; one listener)
├── watcher (WV) — scan + watch of {p} cdc, filtered on owned shard codes,
│                  feeding the view registry
├── view registry (DS) — per active hostnqn: rendered records, GENCTR, pending
└── NVMe/TCP server (NP) — accept loop; per-connection admin-queue state
                   machine (Connect, properties, Identify, Get Log Page,
                   Features, Keep Alive, AER)
```

Packages and files (`layout.md`, Directory tree and Dependency rules):

| package | files | may import (internal) |
|---|---|---|
| `cdc` | `cdc.go` (`Run`, the dependencies), `watch.go` (The etcd watcher), `view.go` (The discovery service model), `logpage.go` (the DS3 and DS9 rendering), `server.go` (the listener, NP1), `conn.go` (the per-connection state, NP4 to NP12), `pdu.go` (the NP2 and NP3 codec) | `common`, `pb`, `etcdutil`, `model` |
| `cmd/dnv-cdc` | `main.go` | `cdc`, `common`, `etcdutil`, plus cobra and viper |

Out of scope here: how operators distribute cdc endpoints to hosts (static
nvme-cli scripts or the nvme-stas configuration; mDNS discovery, TP-8009,
is not implemented), gateway and worker behavior, and what the integration
suite leaves out (Integration test plan).

## Constants and schema

### Additions to `common/constants.go`

The shared constants of `dnv-cdc` are one region of the constant block in
`common/constants.go`, which is authoritative for their comments:

* `NvmeDiscoveryNqn`, the well-known discovery subsystem NQN (NP5);
* `DefaultCdcTrType`, the only accepted `--tr-type` (The NVMe/TCP discovery
  service); `DefaultCdcAdrFam`, the default `--adr-fam`, and
  `CdcAdrFamIpv6`, its other accepted value (CM1, CM2); `DefaultCdcTrSvcId`,
  the default `--tr-svc-id`, the standard discovery port;
* `CdcRangeAll`, the `--range` default: every range (CM1);
* `CdcMaxAdminSqSize`, the admin submission queue's entry count: CAP.MQES
  is one less, Connect's SQSIZE is capped at it (NP4), and it is the ASQSZ
  of every discovery log entry (DS3);
* `CdcAerl`, Identify's AERL: up to `CdcAerl` + 1 outstanding AERs per
  connection (NP11);
* `CdcMaxH2CData`, ICResp's MAXH2CDATA and the framing cap of `readPdu`: a
  PDU whose PLEN exceeds the CapsuleCmd header plus this value is refused
  before its payload is read. Connect's data is the only host-to-controller
  data `dnv-cdc` ever interprets; in-capsule data on any other command is
  accepted and discarded (NP2, NP3);
* `CdcDiscLogHeaderSize`, the discovery log's header block of GENCTR,
  NUMREC and RECFMT, and `CdcDiscLogEntrySize`, one discovery log entry
  (DS9);
* `CdcCntlIdMax`, the top of the dynamic CNTLID range that Connect assigns
  round-robin (NP5);
* `DefaultCdcKeepAliveGraceMs`, the grace past its KATO before a connection
  expires, and `DefaultCdcZeroKatoTmoMs`, the idle cutoff of a connection
  whose KATO is zero (NP10);
* `DefaultCdcRescanInterval`, the seconds between retries of a failed etcd
  scan (WV5).

### Addition to `model/keys.go`

`CdcEntryKey` and `CdcEntryPrefix` are `model`'s (`dnv-worker.md` MD2), and
so is the parser `ParseCdcEntryKey`, in the same style as `ParseDnRevKey`:
a malformed key parses as not ok, and the watcher skips it (WV2).

### Schema

This document adds nothing to `pb/schema.proto`: `CdcEntry`, with `nqn`,
`nvme_tr_conf_list` and `allowed_hosts`, and its key schema are
`architecture.md`'s (Key table).

## The discovery service model

DS1. **Source of truth.** The served state is exactly the `CdcEntry`
messages under `{p} cdc` whose shard-code key field is owned (DS2), across
all clusters: the key places `cluster_id` before `shard_code`, so one prefix
watch covers every cluster and a host allowed in two clusters sees both.
`dnv-cdc` adds nothing, caches nothing else, and persists nothing.

DS2. **Ownership.** The owned shard-code set is fixed at startup from
`--range`: range digit h owns the codes h0 to hf (CM2). An entry is owned
iff its `shard_code` is in the set. Ranges are static configuration and
active-active, with no vote worker: two instances given the same range are
twins that both serve, and the redundancy mechanism is hosts holding
discovery connections to all configured cdc endpoints, not election.
Overlap is harmless, because the service is read-only; a coverage gap is an
operations error no instance can detect, and it is documented, not handled.
`dnv-cdc` registers nothing in etcd and never writes it (WV6).

DS3. **Record rendering.** One `CdcEntry` renders to one discovery log
entry per element of `nvme_tr_conf_list`:

* TRTYPE and ADRFAM from `tr_type` and `adr_fam`, tcp naming TCP and ipv4
  and ipv6 their address families. An element with any other `tr_type` is
  skipped with reason `foreign_tr_type`, since only TCP is served (The
  NVMe/TCP discovery service), and a tcp element with any other `adr_fam`
  with reason `foreign_adr_fam`: an address family this controller cannot
  name has no byte to put in the record. Either way the rest of the entry
  still serves. These skips log `cdc entry skipped` once per distinct
  reason each time the entry is rendered (by a scan, or by a put event
  carrying it), not once per skipped element: however many elements share
  a foreign transport, they are one record.
* TRADDR and TRSVCID verbatim from `tr_addr` and `tr_svc_id`; SUBNQN from
  `nqn`.
* SUBTYPE an NVM subsystem; TREQ not specified; CNTLID the dynamic
  controller; ASQSZ `CdcMaxAdminSqSize`; no EFLAGS; TSAS TCP with no
  security type.
* PORTID an FNV-64a hash of `tr_addr` and `tr_svc_id`, cut to the field:
  deterministic, equal for the same CN port wherever it appears; a
  collision is harmless, because the field is informational.

DS4. **Visibility.** Host h — its Connect hostnqn, matched as an exact
string — sees entry e iff e's `allowed_hosts` is empty or contains h.

DS5. **View.** A host's view is the rendered records of its visible owned
entries in the deterministic order (`cluster_id`, `shard_code`, `sp_id`,
`ss_id`, tr-conf index). NUMREC is the record count; GENCTR comes from DS7.
Views are materialized only for active hosts (DS7), and re-rendered when
read after an event moved them (DS6) or by a rescan (WV4).

DS6. **Impact.** Applying one watched event (a put or a delete of an owned
entry, WV3): for every active host where the entry was visible before or is
visible after, if its view's rendered content differs, that host is
impacted — its GENCTR advances, `view changed` is logged, the pending bit
is set on each of the host's connections, and delivery is attempted on each
(NP11). An entry visible to a host on neither side changes nothing for it,
not even its GENCTR. So a watched change produces an AEN only on the hosts
whose rendered, filtered log page content actually changes — gaining an
entry, losing one (including by `allowed_hosts` removal), or a field change
inside a visible entry — and an entry with an empty `allowed_hosts` is
visible to everyone, so its changes impact every active host. An
`allowed_hosts` edit that keeps a host's membership changes no rendered
byte and therefore does not impact it (the cdc suite asserts this).

The decision renders no view: the event changes one entry, whose place in
the DS5 order its key fixes, so a view differs exactly when the records the
entry puts into it — all of its records for a host it is visible to, none
for one it is not — differ between before and after. An impacted host's
view is marked dirty instead of re-rendered — its NUMREC moves by the
difference, so `view changed` carries the new count — and is rendered by
the host's next snapshot (DS9), or by a rescan that comes first (WV4).
Cost: under the registry mutex an event costs, per active host, a
visibility test on each side and at most one compare of the entry's own
records, plus a `view changed` record per impacted host, and never a
render. A render copies every record the host sees, under the same mutex;
a view is rendered once per read that finds it moved, however many events
moved it since its last render.

DS7. **Host state.** Created at a hostnqn's first live connection, with a
fresh GENCTR, no pending bit and its view rendered; shared by all its
connections on this instance; dropped at its last disconnect. Hosts without
a connection are never tracked and never notified — they catch up by
reading the log when they next connect, which is the whole point of
persistent discovery connections. GENCTR is therefore scoped to the
instance and the hostnqn while the host holds a live connection, and
restarts when the host state is recreated (all connections gone, or the
instance restarted). That is legal: every fabrics connection is a fresh
dynamic controller, and hosts compare GENCTR only between reads on one
controller; a reconnecting host re-reads the full log anyway.

DS8. **AEN value.** A delivered AEN completes an AER with the Notice event
type and the event information Discovery Log Page Changed, naming the
discovery log page; it is gated per connection by NP9's feature mask.

DS9. **Log page layout and snapshot.** The log page is the
`CdcDiscLogHeaderSize` header — GENCTR, NUMREC and the record format — with
the entries after it, `CdcDiscLogEntrySize` each, in view order. Each Get
Log Page command serves offsets from a snapshot of GENCTR and the records
taken when the command arrives; reads beyond the end return zeros. Paged
offsets within one command are therefore self-consistent; consistency
across commands is the host's GENCTR re-read protocol, as the specs intend.

DS10. **Staleness.** Through etcd outages `dnv-cdc` keeps serving its last
known state (WV5): stale-but-consistent beats unavailable for an advisory
service. Recovery rescans diff against the held state — each re-renders
every active host and impacts, by the DS6 rule, the hosts whose view moved
(WV4) — so changes missed during the outage still AEN. A last known state
exists only once the first scan has landed: before it the registry is
empty, which is not the same as "no subsystems", so nothing is served from
it — the accept loop starts only after that scan (CM4).

DS11. **Restart.** Nothing persists. Every connection drops; hosts
reconnect, host states are rebuilt, GENCTRs restart (DS7).

## The etcd watcher

The watcher is the shard worker's skeleton (`dnv-worker.md` SW2 to SW4)
minus the per-key workers: one goroutine owns the entry map, and the view
registry is serialized under one mutex. Entry mutations and the impact
fan-out run on the watcher goroutine, while host-state attach and detach (a
connection materializing or dropping its view) and the render of a dirty
view (a Get Log Page's snapshot, DS6) run on connection goroutines under
that same mutex; `cdc/view.go` states the rule as Go expresses it. The
serialization invariant is what matters: no served state is ever mutated
concurrently.

WV1. **Scan then watch.** A `Range` of `CdcEntryPrefix` builds the owned
entry map; a `WatchTyped` from the scan's revision plus one, decoding every
value as a `CdcEntry`, then delivers the events, which are applied
serially. Log `cdc scan complete` after each scan.

WV2. **Parse and filter.** Keys parse with `ParseCdcEntryKey`; a key whose
shard code is not owned is skipped silently — it is expected, the key-field
filter of `architecture.md`, dnv-cdc — and a malformed key or value logs
`cdc entry skipped` and is dropped (the rule of `dnv-worker.md` SW2).

WV3. **Events.** A put upserts the entry (log `cdc entry applied`, `op`
put); a delete removes it (`op` delete). Either way the DS6 impact pass runs
against the change.

WV4. **Watch failure ⇒ rescan.** Any watch error, compaction included, logs
`cdc watch restarting` and returns to WV1. The rescan does not walk the two
maps key by key: it installs the freshly scanned map wholesale and
re-renders every active host, impacting the hosts whose rendered bytes
moved. The diff is against what the held state renders, so a view an event
left dirty (DS6) is rendered against the held state first, then against the
new map. The impacted set is exactly the one a per-key diff would name —
DS6 impact is defined on rendered content, not on which key changed — so
changes missed across the gap still AEN. GENCTR is where the two differ:
one rescan bumps an impacted host once for the whole gap, where the same
changes arriving as separate watch events bump it once each. Hosts compare
GENCTR between reads rather than counting its steps (DS7), so that
difference is not one they can act on, and a rescan is rare enough that
re-rendering every host (a dirty one twice) costs far less than getting a
per-key diff subtly wrong.

WV5. **Scan failure ⇒ retry.** A failed `Range` retries every
`DefaultCdcRescanInterval` seconds; the server keeps answering from the
held state meanwhile (DS10) — or, while the first scan has not landed,
answers no host at all, its accept loop not yet started (CM4). The
`etcdutil` record carries the error.

WV6. **Read-only.** No puts, no deletes, no leases, no locks, ever. The
watcher reaches etcd only through the narrow `etcdStore` interface, which
has no write method, so the rule holds by construction.

## The NVMe/TCP discovery service

The service is NVMe/TCP only, admin queue only and pull only. There is no
other transport (`--tr-type` accepts only tcp, CM2), no TLS, no in-band
authentication, no header or data digest (negotiated off, NP3), no I/O
queue (NP4), and no TP-8010 host registration (DIM/DDC): `dnv-cdc` reads
etcd and answers hosts, and nothing registers into it. Identify reports the
Direct Discovery Controller type (NP7), and a Discovery Information
Management command is refused with invalid command opcode — refused as a
command, never by dropping the connection (NP2, NP12).

NP1. **Listener.** One TCP listener on `--tr-addr` and `--tr-svc-id`. Per
accepted connection: one connection state behind the connection's lock, and
three goroutines of its own — the reader, which runs the NP3 handshake and
every command and writes every response but an AEN; the AEN writer, the
only goroutine that completes an AER with an AEN (NP11), so an impact just
sets the pending bit (taking that lock) and wakes it, and the watcher never
writes to a socket (DS6); and the NP10 keep-alive reaper, armed at accept
on the zero-KATO budget until Connect names a KATO. A second lock
serializes socket writes, so an AEN never interleaves with a response
inside one PDU. There is no connection cap: the keep-alive and idle reaping
of NP10 bounds leakage. An `Accept` error that is not the shutdown logs
`cdc accept failed` and pauses for `acceptRetryDelay` before accepting
again, so a transient failure (a file-descriptor shortage, say) costs a
pause rather than a spin that fills the log and the CPU. SIGTERM: stop
accepting, close every connection, stop. The listener is open from
startup, but nothing is accepted before the watcher's first scan has landed
(CM4): until then a connecting host's TCP handshake completes into the
kernel's listen backlog (while it has room) and nothing answers its ICReq.
A SIGTERM in that window closes the listener, which resets those
connections unanswered.

NP2. **PDU subset.** Implemented: ICReq, ICResp, H2CTermReq, C2HTermReq,
CapsuleCmd (Connect is the only command whose in-capsule data is read),
CapsuleResp, C2HData. Never sent: R2T, since no host data is ever
solicited. Anything malformed — an unknown PDU type, a bad HLEN or PLEN, a
PDU carrying data whose PDO lies outside the range from HLEN to PLEN, a
PLEN over `CdcMaxH2CData` plus the CapsuleCmd header (the one cap for every
PDU type) — answers C2HTermReq with the fitting FES and closes (log
`pdu error`).

An H2CTermReq after the ICReq ends the connection the way a closed socket
does — no C2HTermReq, reason `closed` (NP13) — only if it passes the
framing checks above like any other PDU. One that carries the header of the
PDU in error (the error data the specs define) with no PDO set fails the
PDO check and is itself a PDU error: `pdu error`, a C2HTermReq, reason
`pdu_error`. Only a data-less one (PLEN equal to HLEN), or one whose PDO
lies between HLEN and PLEN, ends as `closed`. The connection ends either
way; only the C2HTermReq and the records differ.

In-capsule data on any other command is accepted and discarded, and the
command is then answered on its own merits (NP12: an unsupported opcode is
invalid command opcode, DNR). nvme-stas, the production host stack, sends
the TP-8010 Discovery Information Management command with its payload in
the capsule to every discovery controller it connects to, and treating that
data as a terminal PDU error puts the host in a permanent connect and reset
loop. The framing cap NP2 exists to enforce is unaffected: a PDU past that
cap is still refused on its common header, before the rest of it is read.

NP3. **Connection establishment.** ICReq must carry the PDU format version
this controller speaks and ask for no host PDU data alignment (HPDA), else
C2HTermReq with FES unsupported parameter and FEI the offending field's
byte offset: this controller pads no PDU data, so a host that demands
alignment is refused, not ignored (nvmet refuses a non-zero HPDA too).
ICResp: the same format version, no controller PDU data alignment (CPDA),
both digests disabled regardless of what the host requested (a controller
enables only what both sides support), MAXH2CDATA `CdcMaxH2CData`.

NP4. **One admin queue.** The first capsule MUST be a Fabrics Connect of
the admin queue; SQSIZE is honored up to `CdcMaxAdminSqSize`. A Connect of
any other queue is refused with connect invalid parameters — there are no
I/O queues. The Disable SQ Flow Control attribute is honored; without it,
SQHD is maintained in every response.

NP5. **Connect.** Connect validates its connect data: SUBNQN must be
`NvmeDiscoveryNqn` (else connect invalid parameters, IPO pointing at
SUBNQN), HOSTNQN well-formed (at most `MaxNqnLength` bytes); HOSTID is
recorded for the logs. KATO comes from the command. On success a CNTLID
from the round-robin counter, which wraps at `CdcCntlIdMax`, is assigned
and returned, the connection registers under its hostnqn (DS7), and
`host connected` is logged. The connect data's CNTLID field is not
validated, because this controller is dynamic: it assigns the id and
returns it, and every conforming host asks for the dynamic controller
there; only a host asking for a static CNTLID could observe the difference.

NP6. **Properties.** Property Get: CAP (MQES one below
`CdcMaxAdminSqSize`, contiguous queues required, the smallest doorbell
stride and memory page size, the NVM command set, the enable timeout per
NP14), VS (NP14), CC, CSTS. Property Set: CC only, with EN, SHN, IOSQES and
IOCQES accepted. CSTS.RDY follows CC.EN; CC.SHN sets CSTS.SHST to shutdown
complete, and the host is expected to close; CC.EN cleared after being set
marks the controller not ready and leaves teardown to the socket. Any other
offset: invalid field, DNR.

NP7. **Identify.** The Identify Controller data structure only: CNTRLTYPE
discovery controller, DCTYPE Direct Discovery Controller, MDTS reporting no
limit, KAS its keep-alive granularity, AERL `CdcAerl`, OAES with the
Discovery Log Page Change bit set, LPA advertising the extended NUMD and
LPO, SGLS advertising SGLs with in-capsule data, SUBNQN `NvmeDiscoveryNqn`,
MN "dnv", SN derived from the listen endpoint and FR from the build; every
unlisted field per NP14. Any other CNS: invalid field, DNR.

NP8. **Get Log Page.** The discovery log page only (anything else: invalid
field, DNR — nvmet's discovery parser does the same). NUMDL and NUMDU, LPOL
and LPOU are honored against the DS9 snapshot, and the command's SGL length
bounds the transfer whatever NUMD asked for — the controller never writes
past the buffer the host described. LPO must be dword aligned (nvmet says
the same); one transfer is bounded by `maxLogTransfer`, and a larger
request is refused with invalid field, because a controller has to bound
the buffer one command can make it allocate (hosts read the log in chunks
far below the bound). RAE is accepted and ignored: event clearing is
delivery-based (NP11). Data returns as C2HData followed by a CapsuleResp;
the SUCCESS-flag shortcut is not used.

NP9. **Features.** Set and Get Features of the Asynchronous Event
Configuration: stored per connection, all clear by default — only the
discovery-log-change bit is meaningful, and it gates DS8 delivery. AENs are
gated by it because the specs default that bit to disabled; kernels enable
it because Identify's OAES advertises it (NP7). Set and Get of the Keep
Alive Timer SHOULD update the connection's KATO. Any other FID: invalid
field, DNR.

NP10. **Keep Alive.** Every received command restarts the timer (the nvmet
rule). A non-zero KATO expires at KATO plus `DefaultCdcKeepAliveGraceMs`; a
zero KATO expires after `DefaultCdcZeroKatoTmoMs` idle, because zero does
not mean immortal: such connections, a one-shot "nvme discover" among them,
get a fixed idle cutoff, mirroring nvmet's discovery default. Expiry closes
the socket with `host disconnected`, reason `keep_alive`.

NP11. **AER.** Up to `CdcAerl` + 1 outstanding per connection; one more is
refused with the AER-limit-exceeded status. An armed AER completes
immediately when the connection's pending bit is set (and NP9 allows);
delivery clears the bit; impacts while unarmed coalesce into the bit.
Impact marks every connection of the host: a connection with an armed AER
completes it at once, and one without gets the completion when its next AER
arrives, so several impacts before a delivery coalesce into one AEN — never
lost, never queued up. AERs never complete on disconnect: the queue dies
with the socket.

NP12. **Everything else.** Unknown admin opcodes: invalid command opcode,
DNR — which is what a TP-8010 Discovery Information Management command
gets, since nothing registers into this controller, in-capsule payload and
all (NP2). Unknown fabrics command types: invalid field, DNR. `dnv-cdc`
never fails a well-formed supported command for load reasons; NP8's
transfer bound is a buffer limit stated up front, not a load decision.

NP13. **Teardown.** On a socket close or error, a keep-alive expiry, a
terminal PDU error, or a write that does not complete within
`socketWriteTimeout` — set on every PDU write, because a host that has
stopped reading is gone and the controller must not hold a goroutine and a
socket on it forever: cancel the timers, discard the outstanding AERs,
unregister from the host state (DS7: the last connection drops the state),
and log `host disconnected` with the reason. A write that hits the deadline
has no reason of its own: like any other socket error it ends the
connection as `closed`, the same value a host that simply went away
produces, since the record's reasons have nothing finer (Log records). A
connection that never completed Connect has no hostnqn and no CNTLID and
belongs to no host state, so it logs nothing here — a port scan is not a
host; what went wrong with it is already in its `pdu error` record.

NP14. **The mirror rule.** Field values and statuses this section does not
pin — identify field values, status codes, property behavior — follow the
reference kernel's nvmet discovery controller, byte for byte where hosts
can observe them. This pins the countless byte-level choices to something
every host is already tested against.

## `cmd/dnv-cdc`

CM1. **Flags.** A cobra root command with viper and no subcommands; the
environment prefix is `DNV_CDC_`, and every flag is also settable through a
`--config` file. `--etcd-endpoints` (required), the comma-separated client
endpoints; `--etcd-dial-timeout`, in seconds, defaulting to the worker's
`DefaultEtcdDialTimeout`; `--range`, comma-separated hex digits each
claiming the shards h0 to hf, defaulting to `CdcRangeAll`; `--tr-type`,
defaulting to `DefaultCdcTrType`, the one value it accepts; `--adr-fam`,
`DefaultCdcAdrFam` by default, or `CdcAdrFamIpv6`; `--tr-addr` (required),
the listen address; `--tr-svc-id`, the listen port, defaulting to
`DefaultCdcTrSvcId`; and `--config`, the optional config file. `--range`
defaults to every range, mirroring the worker's `--roles` defaulting to all
three roles: a single-instance deployment needs no sharding flags.

CM2. **Validation.** Refuse to start on a `--range` element outside
[0-9a-f], a duplicate element, a `--tr-type` other than tcp, an unparsable
`--adr-fam`, `--tr-addr` or `--tr-svc-id`, or an empty `--etcd-endpoints`.

CM3. **Wiring.** Build the `etcdutil` client and hand off to `cdc.Run`,
which runs the watcher, the listener and the view registry. No interceptors
(`grpc.md`, Wiring). The `init` of `common/log.go` installs the JSON logger
(`log.md` R3).

CM4. **Lifecycle logs, the start half.** `cdc starting` first. Then the
start order: the listener opens (a busy port fails the process before the
watcher exists), the watcher starts, and the accept loop starts only once
the watcher's first scan has landed. Before that scan the registry holds no
state at all — empty, not "no subsystems" — yet a host answered out of it
would be told there are no subsystems. After a control-plane restart in
which etcd answers later than `dnv-cdc` listens, every discovery controller
would tell every host so at once, which is the very shape the cdc suite
uses to make nvme-stas disconnect everything (Integration test plan). So
until then nothing is accepted (NP1), and every `firstScanLogInterval` of
waiting logs `cdc waiting for first scan` at Error. A SIGTERM while waiting
still ends the process (CM5).

CM5. **Lifecycle logs, the stop half.** `cdc stopping` on SIGTERM or
SIGINT, after the listener and the watcher have stopped.

Production placement: one `dnv-cdc` per control-plane server (the physical
view of `architecture.md`, System overview), twins of a range on different
control-plane servers, and all endpoints listed on every host. A twin pair
covering the whole cluster runs the same command line on both servers, each
with its own `--tr-addr`, and omits `--range`, whose default is every range
(`architecture.md`, Components: invocation reference).

## Log records

The records of `dnv-cdc` are JSON records under the rules of `log.md`, R1
to R12, at Info unless stated. Record names and attribute names are
normative: the cdc suite parses them. The "etcd …" records `etcdutil` emits
for every scan, decode and watch event come on top (`log.md`, etcd), and
`dnv-cdc` has no gRPC records (`grpc.md`, Wiring). The records this binary
owns:

* `cdc starting` with `ranges`, `endpoints`, `tr_addr` and `tr_svc_id`, and
  `cdc waiting for first scan`, at Error, once per `firstScanLogInterval`
  while the first scan has not landed (CM4);
* `cdc scan complete` with `entries` (the owned count) and `rev`, at every
  scan and rescan (WV1);
* `cdc watch restarting` with `error` and `compacted` (a bool) (WV4);
* `cdc entry applied` with `key` and `op`, put or delete (WV3);
* `cdc entry skipped` with `key` and `reason`: `malformed_key` or
  `malformed_value` (WV2), `foreign_tr_type` or `foreign_adr_fam` (DS3);
* `host connected` with `hostnqn`, `hostid`, `cntlid`, `kato_ms` and
  `remote` (NP5);
* `host disconnected` with `hostnqn`, `cntlid` and `reason`: `closed`,
  `keep_alive`, `pdu_error` or `shutdown` (NP13);
* `view changed` with `hostnqn`, `genctr` and `numrec`, per impacted host
  (DS6);
* `aen sent` with `hostnqn`, `cntlid` and `genctr`, per delivered AER
  (DS8);
* `pdu error` with `remote` and `reason` (NP2);
* `cdc accept failed`, at Error, with `error`, on a listener error that is
  not the shutdown (NP1);
* `signal received` with `signal` at the first SIGINT or SIGTERM, and
  `second signal, exiting without a clean drain`, at Warn, with `signal`
  (CM5);
* `etcd client close failed`, at Error, with `error`, best effort (CM5);
* `cdc stopping` (CM5).

Only these records are normative, and the cdc suite may key off none of
the Error and Warn ones: those exist so a failure is visible in the
diagnostics, not so the suite can count them.

## Integration test plan

**What the suite proves.** `integtest/cdc_test.sh` proves a real `dnv-cdc`
fleet — the real `cdc`, `model` and `etcdutil` packages over a real
single-node etcd — against real kernel NVMe hosts and real nvmet targets:
range sharding and twin equality, per-host filtering, the per-host AEN and
GENCTR semantics of DS6 and DS7, automatic connect, re-point and disconnect
end to end through nvme-stas, and the high availability of twins and of
restarts. nvme-stas is the production host stack, and plain nvme-cli is
equally supported: the AEN path is stock kernel — the host kernel publishes
the discovery-change AEN as an "NVME_AEN" uevent — and nvme-stas adds the
automatic connect, re-point and disconnect on top. The suite tests the two
layers separately, so that an nvme-stas configuration problem cannot be
mistaken for a cdc bug. It does not prove the gateway-to-`CdcEntry`
pipeline, which the worker suite proves by asserting `CdcEntry` contents
(`dnv-worker.md`, Integration test plan), data-path correctness beyond one
read (the agent suites), or etcd failures.

**Topology.** The developer machine builds the binaries and drives four lab
servers over ssh; the suite occupies all four, so no other suite may run in
the lab while it does. The first server runs, as a plain user with no sudo,
one etcd, configured the way every etcd serving dnv must be (its
transaction-op cap at `EtcdMaxTxnOps`, which the suite reads from the
`workerctl` it builds instead of copying the number), four `dnv-cdc`
instances — two twins of the low half of the shard-code space and two of
the high half — and `cdcctl`. The second server, with passwordless sudo, is
the nvmet target that stands in for the controller nodes: a handful of
test subsystems, each with one dm-zero namespace, linked to several nvmet
ports, one of them on two ports so that one subsystem has two paths. The
last two servers are the hosts, with passwordless sudo, nvme-cli and
nvme-stas, each connecting under its own host NQN; a third, ghost identity,
named in no entry's allowed hosts, discovers from the first host under an
explicit host NQN and host id. The suite's ports collide with no other
suite's.

**Cases.** Five cases run in a fixed order, fail-fast, each against a wiped
cdc prefix, its own fabricated cluster ids and a restarted fleet whose logs
start empty, so nothing leaks between cases and every count a case makes
counts only its own records. The target is built once at setup, and the
one case that changes it restores it before it finishes, so every case sees
the same target.

* smoke — bring-up: scan, one entry, per-range serving at the instance
  boundary, a real host connect through the cdc, one read of the dm-zero
  backend, and the clean deletion of the entry;
* matrix — the full grid of every instance against every identity, exact
  in every cell: twins identical, each host's union complete, the open
  entry visible to anyone, a genuinely empty log for the ghost in one half,
  two records for the subsystem on two ports, an entry of a second cluster,
  and each host connecting to exactly the subsystems it may see, ending
  with two live paths to the two-port subsystem;
* lowlevel — plain nvme-cli with nvme-stas stopped, over one persistent
  discovery connection per host: the AEN uevent reaches exactly the
  impacted host, each host's GENCTR moves only on its own impact, an
  `allowed_hosts` edit that keeps a host's membership moves nothing for
  it, and the controllers stay live across several keep-alive periods;
* stas — nvme-stas end to end: its discovery connections to every
  instance, data connections with no manual command, auto-connect of a new
  entry, re-point of a subsystem whose entry moves its transport the way a
  cntlr replacement does, with the device surviving throughout,
  auto-disconnect of a deleted entry, and a mass disconnect when the prefix
  is wiped;
* ha — with nvme-stas running: a killed twin costs nothing while the other
  serves its half alone, a restarted twin serves exactly what its twin
  serves, and a full-fleet kill and relaunch under live hosts leaves every
  data connection undisturbed and the AEN path working.

**Rules exercised.** The smoke case exercises the launch (CM1 to CM3),
WV1, WV3, DS2 at the instance boundary, DS7 (a put while no host is
connected moves no view) and the connection records of NP5 and NP13; the
matrix case DS1 to DS5 against real kernels — DS1's all-cluster watch,
DS2's sharding, DS3's record per transport, DS4's filtering and DS5's
identical twins; the lowlevel case DS6, DS7, DS8 and NP11 on stock kernels
with no nvme-stas anywhere, and NP10 on live controllers; the stas case
DS6's AENs driving nvme-stas's connect, re-point and disconnect with no
operator action, and NP2 and NP12 on the in-capsule Discovery Information
Management command nvme-stas sends to every discovery controller; and the
ha case DS2's twins, WV1's recovery of identical served state from etcd
alone and DS11's reconnect and catch-up. Left to the unit tests, never
forced on hardware: log paging past one page, keep-alive expiry and
AER-limit exhaustion.

Out of scope: etcd quorum loss and compaction races; TLS and in-band
authentication; non-TCP transports; digests; scale — every shard code,
many hosts; mDNS (TP-8009); TP-8010 registration; the nvme-stas version
matrix, the lab pinning one version; and referrals.

**What a pass means.** Exit status zero and `PASS` mean every case's
assertions held, in a run that stops at the first failure. Discovery is
asserted as the set of (subsystem NQN, transport address, service id)
triples a discover returns, compared with the expected set, never as two
whole documents; GENCTR only on a persistent discovery connection, because
a one-shot discover creates and drops its host state each time (DS7).
nvme-stas is judged by kernel state — the host's subsystem list and the
device nodes it publishes by namespace uuid — never by its own output,
whose format varies by version. Every wait is a bounded poll; and
"undisturbed throughout" holds on every poll of
the wait, not at two point samples, which would pass a connection that
dropped and came back between them. The cdc records are matched by their
exact names (Log records), read through a filter that drops a line torn by
a concurrent append.

**Cleanup.** Cleanup runs unconditionally at the start of every run, and
at the end only on success: a failing run leaves etcd's data, every cdc
log, the nvmet and dm state and the host connections in place and prints
its diagnostics — the failing stage and its trace id, the tail of every
cdc log, the entries in etcd, the target's configfs tree and dm tables,
and the hosts' subsystems, uevent captures and nvme-stas journals — so the
debris is what the developer reads. Order matters, and every step tolerates
absence: the hosts first, with nvme-stas stopped so that its reconnect loops
die before their targets vanish; then the target, its port links first,
which is safe because the hosts are already disconnected: a port unlinked
under a live connection kills the host's controller and the host never
reconnects by itself; then the first server's processes. Cleanup touches
only what is the suite's — the test subsystems and the discovery
controllers that point at the fleet, never every subsystem of a host — and
leaves nvme-stas installed and the kernel modules loaded; the end-of-run
cleanup also removes the work directories and unmasks the kernel's
autoconnect unit.

**The driver, `cdcctl`.** `cdcctl` is the etcd driver that plays the
gateway and the worker for `CdcEntry` keys only: it formats the keys
through `model`, marshals `pb.CdcEntry`, and puts, deletes, wipes and lists
the prefix. `dnv-cdc` reads only those keys and never a `ClusterConf`, so
the suite fabricates its cluster ids and writes no other key, and the
pipeline that builds real entries stays out of this suite. It runs on the
first server against the loopback etcd, prints its result on stdout, and
leaves its `etcdutil` records on stderr under the stage's trace id; it
keeps the transport list in the order it is given, since that order is
DS5's tr-conf index.

**The target helper, `cdc_target.sh`.** The target helper builds the nvmet
side: the dm-zero devices, the ports and the test subsystems with their
namespaces and port links; it moves one subsystem's link for the stas
case's re-point, and it tears everything down, port links first. Simulated
controller-node namespaces are dm-zero devices — no backing files, no loop
devices; reads return zeros and writes vanish — because discovery
correctness needs connectable targets, not durable data. Every test
subsystem admits any host, deliberately: if `dnv-cdc` ever leaked an entry
to the wrong host, the resulting connect would succeed and the
wrong-device assertion would catch it, instead of nvmet's access list
masking the leak. Setup is re-runnable under the nvmet configfs rules:
directories are created only when absent, and attributes are written only
when their object is created, because some read back reformatted and an
equality re-check would be wrong by construction.

**The host helper, `cdc_host.sh`.** The host helper carries the host-side
actions that hold state: the nvme-stas control, which saves the host's own
nvme-stas configuration, writes the suite's — every cdc endpoint, no mDNS,
connect every entry and disconnect only the connections nvme-stas itself
made once their entry vanishes — and restores the original afterwards; the
masking of the kernel's autoconnect unit, which would otherwise connect on
the very AEN the lowlevel case measures, so it is masked for every case and
setup checks the mask rather than trusting it; the uevent capture of the
lowlevel case; and the scoped wipes. The wipes disconnect only the test
subsystems and the discovery controllers that point at the fleet; while
nvme-stas runs, the reset leaves the discovery controllers to it, because
one taken down behind its back is treated as gone for good and never
re-created. Both helpers are generated by `cdc_test.sh`, shipped to their
servers and run under sudo, so that everything the suite is lives in the
script and the driver.
