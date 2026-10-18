# cdc.md — `dnv-cdc`: NVMe-oF Central Discovery Controller

This document owns the `dnv-cdc` binary: package `cdc` and the
`cmd/dnv-cdc` command, with the discovery service model (DS1 to DS11), the
etcd watcher (WV1 to WV6), the NVMe/TCP discovery service (NP1 to NP14),
the command (CM1 to CM5), its log records and the intent of the cdc
integration suite. It leans on `architecture.md` for the system context,
the etcd data model with the `CdcEntry` key, and the listing rule that
decides an entry's transports, with the writers that apply it ([D18]); on
`dnv-worker.md` for `etcdutil` (EU1 to EU7) and the key helpers of `model`
(MD2); on `log.md` and `grpc.md` for logging and trace ids; and on
`layout.md` for package placement and the dependency rules.
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
change impacts. It never writes etcd, never serves or dials dnv-internal
gRPC, so it chains no interceptor (`grpc.md`, Wiring), and never touches the
local kernel's nvmet.

The discovery controller is implemented in userspace and served straight
from etcd, with no kernel state to converge, rather than built as a hub of
kernel nvmet referrals. A referral hub built from a local nvmet port cannot
filter per host, because referrals are served to every connecting host; it
can only redirect, so each host would still need a persistent discovery
connection per controller node, defeating centralization; it needs an
anchor subsystem just to listen, because a referral-only nvmet port never
comes up; and its AENs cannot carry per-host meaning.

The gateway and the worker maintain the entries, every writer by the
listing rule of `architecture.md` [D18]. `dnv-cdc`, the only process that
renders them to a host, reads them and never writes them (WV6).

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

The package `cdc` is the engine and `cmd/dnv-cdc` the command; what each
may import is `layout.md`, Dependency rules.

Out of scope here: how operators distribute cdc endpoints to hosts (static
nvme-cli scripts or the nvme-stas configuration; mDNS discovery, TP-8009,
is not implemented), gateway and worker behavior, and what the integration
suite leaves out (Integration test plan).

## Constants and schema

### Constants this document owns

The shared constants of `dnv-cdc` are one region of the constant block in
`common/constants.go`, which is authoritative for their comments:

* `NvmeDiscoveryNqn`, the well-known discovery subsystem NQN (NP5);
* `DefaultCdcTrType`, the only accepted `--tr-type` (The NVMe/TCP discovery
  service); `DefaultCdcAdrFam`, the default `--adr-fam`, and
  `CdcAdrFamIpv6`, its other accepted value (CM1, CM2); `DefaultCdcTrSvcId`,
  the default `--tr-svc-id`, the standard discovery port;
* `CdcRangeAll`, the `--range` default: every range (CM1);
* `CdcMaxAdminSqSize`, the admin submission queue's entry count: CAP.MQES,
  a 0's-based value, is one less, Connect's 0's-based SQSIZE is capped at
  CAP.MQES so the queue never holds more entries than this (NP4), and it is
  the ASQSZ of every discovery log entry (DS3);
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
* `CdcMaxPendingConnCnt`, the most pending connections an instance holds,
  at which its accept loop stops accepting (NP1);
* `DefaultCdcPendingTmoMs`, a pending connection's pre-Connect deadline,
  counted from accept and never restarted (NP10);
* `DefaultCdcKeepAliveGraceMs`, the grace past its KATO before a connected
  connection expires, and `DefaultCdcZeroKatoTmoMs`, the idle cutoff of a
  connected connection whose KATO is zero (NP10);
* `DefaultCdcRescanInterval`, the seconds between retries of a failed etcd
  scan (WV5).

### Key helpers this document owns

`CdcEntryKey` and `CdcEntryPrefix` are `model`'s (`dnv-worker.md` MD2), and
so is the parser `ParseCdcEntryKey`, in the same style as `ParseDnRevKey`:
a malformed key parses as not ok, and the watcher skips it (WV2).

### Schema

`CdcEntry` — `nqn`, `nvme_tr_conf_list` and `allowed_hosts` — and its key
schema belong to `architecture.md`, Key table.

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
  `nqn`. Each fits its field by the gateway's validation
  (`architecture.md`, Common validation).
* SUBTYPE an NVM subsystem; TREQ not specified; CNTLID the dynamic
  controller; ASQSZ `CdcMaxAdminSqSize`; no EFLAGS; TSAS TCP with no
  security type.
* PORTID an FNV-64a hash of `tr_addr` and `tr_svc_id`, cut to the field:
  deterministic, equal for the same CN port wherever it appears; a
  collision is harmless, because the field is informational.

DS4. **Visibility.** Host h — its Connect hostnqn, matched as an exact
string — sees entry e iff e's `allowed_hosts` contains h, so an entry with
an empty `allowed_hosts` is visible to no host. Visibility thereby mirrors
the subsystem's admission (`architecture.md`, Primary cntlr, step 6): a
host is offered only subsystems whose allowed hosts name it.

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
inside a visible entry. An entry with an empty `allowed_hosts` is visible to
no host (DS4), so while its list stays empty nothing that happens to it
impacts a host: not its creation, not a field change and not its deletion.
An `allowed_hosts` edit that keeps a host's membership changes no rendered
byte and therefore does not impact it.

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
known state (WV5): stale-but-consistent beats unavailable, though a stale
instance also keeps listing an old primary a failover has taken out of the
entries (`architecture.md` [D18]; `architecture.md`, Known limits).
Recovery rescans diff against the held state — each re-renders every active
host and impacts, by the DS6 rule, the hosts whose view moved (WV4) — so
changes missed during the outage still AEN. A last known state
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
filter of `architecture.md`, dnv-cdc — and a malformed key logs
`cdc entry skipped` and is dropped, from a scan and from a watch event
alike (the rule of `dnv-worker.md` SW2). A malformed value is dropped the
same way by a scan; in a watch event it ends the watch generation instead,
because `WatchTyped` reports a value it cannot decode as the watch's error,
so the rescan that follows (WV4) is what skips it: one such put costs a
rescan and a re-render of every active host.

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
writes to a socket (DS6); and the NP10 reaper, armed at accept on the
pre-Connect deadline. A second lock serializes socket writes, so an AEN
never interleaves with a response inside one PDU.

A connection is **pending** from its accept until a Connect succeeds on
it; a refused Connect leaves it pending. At most `CdcMaxPendingConnCnt`
connections are pending at once: at that cap the accept loop accepts
nothing until a pending connection connects or ends, so a new connection
waits in the kernel's listen backlog (while it has room), its ICReq
unanswered, as it does before the first scan (CM4). A pending connection
names no host yet and still holds a read buffer and three goroutines, so
without a cap any number of them could pile up; the cap delays rather
than refuses, because a refusal at accept would turn the burst of
reconnects that follows a cdc restart (DS11) into failed connects. A
connected connection counts against no cap: its keep-alive bounds it
(NP10).

An `Accept` error that is not the shutdown logs `cdc accept failed` and
pauses for `acceptRetryDelay` before accepting again, so a transient
failure (a file-descriptor shortage, say) costs a pause rather than a spin
that fills the log and the CPU. SIGTERM: stop accepting — a loop waiting
at the cap stops too — close every connection, stop. The listener is open
from startup, but nothing is accepted before the watcher's first scan has
landed (CM4): until then a connecting host's TCP handshake completes into
the kernel's listen backlog (while it has room) and nothing answers its
ICReq. A SIGTERM in that window closes the listener, which resets those
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
the admin queue; SQSIZE, a 0's-based value, is capped at CAP.MQES, so the
admin queue never exceeds `CdcMaxAdminSqSize` entries. A Connect of
any other queue is refused with connect invalid parameters — there are no
I/O queues. The Disable SQ Flow Control attribute is honored; without it,
SQHD is maintained in every response.

NP5. **Connect.** Connect validates its connect data: SUBNQN must be
`NvmeDiscoveryNqn` (else connect invalid parameters, IPO pointing at
SUBNQN), HOSTNQN non-empty and at most `MaxNqnLength` bytes (else connect
invalid host, IPO pointing at HOSTNQN), its format unchecked; HOSTID is
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
MN `NvmeModel`, SN derived from the listen endpoint and FR from the build;
every unlisted field per NP14. Any other CNS: invalid field, DNR.

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

NP10. **Keep Alive.** A pending connection (NP1) has no keep-alive yet: it
ends at its pre-Connect deadline, `DefaultCdcPendingTmoMs` after its
accept, and nothing moves that deadline — not the ICReq, not a Connect the
controller refused. A host's kernel sends its ICReq at its TCP connect and
its Connect within moments of the ICResp, which the controller sends as
soon as it accepts, so a deadline counted from the accept costs a real
host nothing, while one that a refused Connect moved would let a client
that never connects keep its place under the cap for as long as it keeps
sending Connects the controller refuses. From a successful Connect on,
every received command restarts the timer (the nvmet rule). A
non-zero KATO expires at KATO plus `DefaultCdcKeepAliveGraceMs`; a zero
KATO expires after `DefaultCdcZeroKatoTmoMs` idle, because zero does not
mean immortal: such connections, a one-shot "nvme discover" among them,
get a fixed idle cutoff, mirroring nvmet's discovery default. Expiry
closes the socket; a connected connection then logs `host disconnected`,
reason `keep_alive`, and a pending one logs nothing (NP13).

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
never fails a well-formed supported command for load reasons: NP8's
transfer bound is a buffer limit stated up front, not a load decision, and
NP1's pending cap, which delays accepting a connection, fails no command
and refuses no connection.

NP13. **Teardown.** On a socket close or error, a keep-alive expiry, a
pending connection's pre-Connect deadline (NP10), a terminal PDU error,
or a write that does not complete within `socketWriteTimeout` — set on
every PDU write, because a host that has stopped reading is gone and the
controller must not hold a goroutine and a socket on it forever: cancel
the timers, discard the outstanding AERs, unregister from the host state
(DS7: the last connection drops the state) or, for a connection that
never completed Connect, give its place under the cap back (NP1), and log
`host disconnected` with the reason. A write that hits its write deadline
has no reason of its own: like any other socket error it ends the
connection as `closed`, the same value a host that simply went away
produces, since the record's reasons have nothing finer (Log records). A
connection that never completed Connect has no hostnqn and no CNTLID and
belongs to no host state, so it logs nothing here — a port scan is not a
host, and neither is a flood of connections that never connect; what went
wrong with one, if anything, is already in its `pdu error` record.

NP14. **The mirror rule.** Field values and statuses this section does not
pin — identify field values, status codes, property behavior — follow the
reference kernel's nvmet discovery controller, byte for byte where hosts
can observe them. This pins the countless byte-level choices to something
every host is already tested against.

## `cmd/dnv-cdc`

CM1. **Flags.** A cobra root command with viper and no subcommands: the
environment prefix is `DNV_CDC_`, every flag is also a config-file key and
an environment variable, and the optional `--config` file is read when it
is named. `--etcd-endpoints` and `--tr-addr` are required. `--range`
defaults to `CdcRangeAll`, every range, mirroring the worker's `--roles`
defaulting to all three roles: a single-instance deployment needs no
sharding flag. The flags, their types and their defaults are the code's,
and `--help` lists them.

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
would tell every host so at once, and nvme-stas would disconnect the data
connections it made. So until then nothing is accepted (NP1), and every
`firstScanLogInterval` of waiting logs `cdc waiting for first scan` at
Error. A SIGTERM while waiting still ends the process (CM5).

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
normative, and so are the reason values of `cdc entry skipped` and
`host disconnected`, so that the cdc suite can read them (Integration test
plan); the normative record names and those reasons are constants in
`cdc`, never formatted. The "etcd …" records `etcdutil` emits for every
scan, decode and watch event come on top (`log.md`, etcd), and `dnv-cdc`
has no gRPC records (`grpc.md`, Wiring). The normative records and the
decision each marks:

* `cdc starting` and `cdc stopping`, the two ends of the lifecycle (CM4,
  CM5);
* `cdc scan complete`, a scan or rescan that landed (WV1);
* `cdc watch restarting`, a watch generation ended by an error, a
  compaction included (WV4);
* `cdc entry applied`, a put or a delete folded into the held state (WV3);
* `cdc entry skipped`, with the reason: a malformed key or value dropped
  (WV2), or a transport element the record cannot name (DS3);
* `host connected` and `host disconnected`, the latter with the reason, the
  life of a connection that completed Connect (NP5, NP13);
* `view changed`, once per impacted host, with its new GENCTR (DS6);
* `aen sent`, once per delivered AER (DS8);
* `pdu error`, a terminal framing error (NP2).

The Error and Warn records — the wait for the first scan (CM4), a failed
accept (NP1), the second signal (CM5) and a failed client close (CM4, CM5)
— and the signal record are diagnostics, not normative: they exist so a
failure is visible, and the suite keys off none of them.

## Integration test plan

**What the suite proves.** `integtest/cdc_test.sh` proves a real `dnv-cdc`
fleet — the real `cdc`, `model` and `etcdutil` packages over a real
single-node etcd — against real kernel NVMe hosts and real nvmet targets:
range sharding and twin equality, per-host filtering, the per-host AEN and
GENCTR semantics of DS6 and DS7, automatic connect, re-point and disconnect
end to end through nvme-stas, the high availability of twins and of
restarts, that a flood of connections that never connect leaves a
connected host served (NP1, NP10), and that many nvme-stas hosts on one
host server each keep reading exactly their own discovery log through adds
and removals of entries. nvme-stas is the production host
stack, and the service serves plain nvme-cli equally: the AEN path is
stock kernel — the host kernel publishes the discovery-change AEN as an
"NVME_AEN" uevent — and nvme-stas adds the automatic connect, re-point and
disconnect on top, which is the host's part of the failover contract
(`architecture.md`, Host view). The suite tests the two layers
separately, so that an nvme-stas configuration problem cannot be mistaken
for a cdc bug. It does not prove what the
gateway and the worker write into `CdcEntry`: the gateway suite reads back
the entry the gateway's subsystem and cntlr RPCs write (`gateway.md`,
Integration test plan), and the worker suite asserts the worker's rewrites
of it, at a cntlr's health transitions, at a failover and at a cntlr
replacement (`dnv-worker.md`, Integration test plan). Nor does it prove
data-path correctness beyond one read (the agent suites) or etcd failures.

**What the Go cases prove.** The package's own tests prove, on the developer
machine, what the suite leaves out or cannot force; every test binary of the
package starts the real etcd of `dnv-worker.md` EU7 and fails without one as
EU7 says, which one test proves by running the test binary again with no etcd.
Most tests drive the codec, the registry, the watcher and the connection state
machine directly, or run an instance over an in-memory store. One test runs
`Run` itself, to prove that it goes through the gate of CM4, over an etcd
client whose only endpoint refuses connections, so that no scan ever lands.
The cases that plant entries run whole instances, composed as `Run` composes
them, over a real etcd whose watchers scan and watch it: the one the test
binary starts, except for the case at the largest shape, which starts an etcd
of its own so that its entries, written and deleted on every run, do not grow
the shared one's history. In every test but the one that runs `Run`, the
timers of NP10, WV5 and CM4 run on a fake clock; `Run` builds its instance on
the real clock. The entries the real-etcd instances serve are written into
etcd as the worker writes them: keys by `model`'s helpers, transport lists by
the listing rule over the pool's cntlrs ([D18]). Those instances serve
in-process fake hosts, NVMe/TCP hosts written in Go inside the test that speak
the PDU subset of NP2 and NP3 over loopback sockets, and they prove the
discovery service model at the largest shape a cluster holds — its most pools,
each with its most subsystems, every entry with the widest transport list and
the most allowed hosts, and one host seeing every record — through ownership
by range (DS2), the order of DS5 and the impact of one change (DS6); that a
burst of more hosts than `CdcMaxPendingConnCnt` waits at the cap and none is
refused (NP1, NP12); that one change reaches every connection of every host it
concerns on twins (DS6, NP11); that a host which stops reading is ended by its
write deadline while the watcher and the other hosts go on, and that a slow
host loses nothing and stalls nobody (NP13, DS9); the host NQN bounds of NP5;
NP8's transfer bound at its limit and one record past it; and the wrap of the
CNTLID counter (NP5).

**Topology.** The developer machine builds the binaries and drives four lab
servers over ssh; the suite occupies all four, so no other suite may run in
the lab while it does. The first server runs, as a plain user with no sudo,
one etcd, configured with the request caps every etcd serving dnv must have
(its transaction-op cap at `EtcdMaxTxnOps` and its request-size cap at
`EtcdMaxRequestBytes`, which the suite reads from the `workerctl` it builds
instead of copying the numbers), four `dnv-cdc`
instances — two twins of the low half of the shard-code space and two of
the high half — and `cdcctl`. The second server, with passwordless sudo, is
the nvmet target that stands in for the controller nodes: a handful of
test subsystems, each with one dm-zero namespace, linked to several nvmet
ports, one of them on two ports so that one subsystem has two paths. The
last two servers are the hosts, with passwordless sudo, nvme-cli and
nvme-stas, each connecting under its own host NQN; a third, ghost identity,
named in no entry's allowed hosts, discovers from the first host under an
explicit host NQN and host id. In the manyhosts case each host server also
runs many copies of nvme-stas's discovery daemon, `stafd`, each a host of
its own. The suite's ports collide with no other suite's.

**Cases.** The cases run in a fixed order and fail fast, each against a
wiped cdc prefix, its own fabricated cluster ids and a restarted fleet
whose logs start empty, so nothing leaks between cases and every count a
case makes counts only its own records; the target is built once at setup,
and the one case that changes it restores it. Together they exercise the
launch (CM1 to CM3), the watcher's scan, events and recovery of identical
served state from etcd alone (WV1, WV3), the discovery service model on
real kernels (DS1 to DS8 and DS11, an entry with an empty `allowed_hosts`
included), the connection records and keep-alive of NP5, NP10 and NP13,
the pending cap and the pre-Connect deadline of NP1 and NP10, the AER
path (NP11), and NP2 and NP12 on the in-capsule Discovery Information
Management command nvme-stas sends to every discovery controller. The
pending cap and the deadline meet a flood of idle connections from the
second host: a persistent discovery connection made before the flood
keeps reading the log and receiving its AEN while the flood's connections
past the cap wait in the listen backlog, the flood's connections end at
their deadline, and a new discover is answered once they are gone; the
flood and its waits are sized by `CdcMaxPendingConnCnt` and
`DefaultCdcPendingTmoMs`, read from the same `workerctl`. The manyhosts
case runs many copies of the discovery daemon on each host server, each
its own host — its own host NQN and host id, its own private bus and its
own runtime directory — listing every instance, with the host's own
nvme-stas stopped and the kernel's autoconnect unit masked as for every
case: each daemon starts that unit for every AEN another daemon receives,
so unmasked it would try to connect every entry of the other daemons' logs
at every change. Only the discovery daemon runs, because the case proves
the log each host reads: its entries then need no subsystem behind them,
and the stas case covers the connector. Each change of the entries is one
transaction of the driver, and after the adds, after removals that also
move entries from one host to another and narrow an entry's hosts, and
once every entry is gone, each daemon's log, read
over its own bus, holds exactly the records of the entries in its
instances' ranges that name it (DS2 to DS4). Each daemon holds a live
discovery connection to every instance under its own identity, every
instance sees connections from exactly the daemons' identities, so a daemon
that fell back to its server's own identity fails the case, and no instance
sees a daemon disconnect while the case runs, so every change reached the
daemons as an AEN on a persistent connection (DS6, NP11). The case leaves
each host server as it found it, which a record of the server's state taken
before and after the case proves. Left to the unit tests, never forced on
hardware: log paging past one page, keep-alive expiry, AER-limit
exhaustion, a refused Connect that keeps its connection pending, and the
exact instant a pre-Connect deadline ends a connection.

Out of scope: etcd quorum loss and compaction races; TLS and in-band
authentication; non-TCP transports; digests; scale past the manyhosts
case — every shard code, more hosts than two host servers can run; mDNS
(TP-8009); TP-8010 registration; the nvme-stas version matrix, the lab
pinning one version; and referrals.

**What a pass means.** Exit status zero and `PASS` mean every case's
assertions held, in a run that stops at the first failure. Discovery is
asserted as the set of (subsystem NQN, transport address, service id)
triples a discover returns, compared with the expected set, never as two
whole documents; GENCTR only on a persistent discovery connection, because
a one-shot discover creates and drops its host state each time (DS7).
nvme-stas is judged by kernel state — the host's subsystem list and the
device nodes it publishes by namespace uuid — never by its own output,
whose format varies by version. The manyhosts case is the one exception,
because it runs the discovery daemon without the connector and so what a
daemon read leaves no trace in the kernel: it reads each daemon's cached
log over that daemon's bus, parses the answer as data rather than as text,
and keeps the same triples, while the daemons' discovery connections are
judged by kernel state. Every wait is a bounded poll; and
"undisturbed throughout" holds on every poll of
the wait, not at two point samples, which would pass a connection that
dropped and came back between them. The cdc records are matched by their
exact names (Log records), read through a filter that drops a line torn by
a concurrent append. The flood case judges accepting by kernel socket
state — the listener's accept queue on the first server and the flooding
host's sockets the instance has closed — never by a record, since a
pending connection logs none (NP13), and never by a timing window.

**Cleanup.** Cleanup runs unconditionally at the start of every run, and
at the end only on success: a failing run leaves etcd's data, every cdc
log, the nvmet and dm state and the host connections in place and prints
its diagnostics — the failing stage and its trace id, the tail of every
cdc log, the entries in etcd, the target's configfs tree and dm tables,
and the hosts' subsystems, uevent captures, flood sockets, nvme-stas
journals and the manyhosts case's daemons' services, discovery controllers
and journals — so the debris is what the developer reads; a cleanup-only run
does the end-of-run cleanup alone. Order matters, and
every step tolerates absence: the hosts first, with the flood's idle
connections closed, the manyhosts case's daemons stopped and their discovery
controllers disconnected, and nvme-stas stopped, so that their reconnect
loops die before their targets vanish; then the target, its port links
first, which is safe because the hosts are already disconnected: a port
unlinked under a live connection kills the host's controller and the host never
reconnects by itself; then the first server's processes. Cleanup touches
only what is the suite's — the test subsystems and the discovery
controllers that point at the fleet, never every subsystem of a host —
and leaves nvme-stas installed and the kernel modules loaded; the
end-of-run cleanup also removes the work directories and unmasks the
kernel's autoconnect unit.

**The driver, `cdcctl`.** `cdcctl` is the etcd driver that plays the
gateway and the worker for `CdcEntry` keys only: it formats the keys
through `model`, marshals `pb.CdcEntry`, and puts, deletes, wipes and lists
the prefix. It also applies a batch of puts and deletes as one
transaction, refused as a whole when any of its lines would be: the
instances apply the events of one transaction back to back, so a host the
batch moves gets an AEN or two for the batch rather than about one per
entry, which is what keeps a case that writes many entries for many
daemons on one host server from spending its run in their AEN handling,
every daemon there reacting to every AEN the others receive. `dnv-cdc`
reads only those keys and never a `ClusterConf`, so
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
connect every entry and disconnect only the connections nvme-stas made or
took over once their entry vanishes — and restores the original afterwards;
the masking of the kernel's autoconnect unit, which would otherwise connect on
the very AEN the lowlevel case measures, so it is masked for every case and
setup checks the mask rather than trusting it; the manyhosts case's
daemons, each started as a transient service whose private mount of the
nvme-stas configuration directory carries the daemon's own identity, with a
bus and a runtime directory of its own, because the daemon reads its
identity from a fixed path, takes a fixed name on its bus, and adopts, and
then removes, the controllers its runtime directory remembers; the reads
of every daemon's log over its bus; the daemons' stop, which stops them
before their buses, then disconnects the discovery controllers under the
daemons' identities and only those, which outlive their daemon, and puts
back the override of the kernel's autoconnect rule that every daemon writes
and the first to stop removes; and the record of the server's state the
case compares; the uevent capture of the lowlevel and flood cases; the
flood case's idle connections, held by one process that the wipes end; and
the scoped wipes. The wipes disconnect only the test subsystems and the
discovery controllers that point at the fleet; while nvme-stas runs, the
reset leaves the discovery controllers to it, because one taken down behind
its back is treated as gone for good and never re-created. Both helpers are
generated by `cdc_test.sh`, shipped to their servers and run under sudo, so
that everything the suite is lives in the script and the driver.
