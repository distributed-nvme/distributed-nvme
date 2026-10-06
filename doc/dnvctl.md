# dnvctl.md — the dnv operator CLI and its integration test

This document owns `dnvctl`, the operator CLI of dnv: the `ctl/` package and
the `cmd/dnvctl` wrapper, which turn one command line into one
`service Gateway` request, with their placement rules (CT6, CT7), the
invocation model (CT9, CT2), the output and error contract (CT4, CT5), the
revision tokens (CT3) and the command tree (CT8, CT1), and the intent of the
dnvctl integration suite and its fake gateway. It owns the mapping from the
command line to the request and from the reply to the terminal; what an RPC
does — its validation, errors, defaults and action — is `architecture.md`,
`service Gateway` — RPC specifications, carried out by the gateway of
`gateway.md`, and dnvctl forwards to both without second-guessing them
(CT8). It leans on `grpc.md` for the client interceptors and the trace id,
on `log.md` for logging, and on `layout.md` and `dependencies.md` for
placement and the dependency rules.

## Scope and placement

### In scope / out of scope

In scope: the `ctl/` library package, the `cmd/dnvctl` wrapper, the
`integtest/fakegateway` binary and the `integtest/dnvctl_test.sh` suite.

Out of scope, each deliberately:

* the userspace copier of `architecture.md`, raid0 bitmap math, which is
  future work outside dnvctl: the surface is one command per RPC and nothing
  else (CT1);
* TLS and authentication: dnv is plaintext gRPC end to end;
* multi-gateway failover: dnvctl dials the one `--gateway-address`, and
  operators front the gateways themselves;
* table or human output modes: every result is one canonical JSON document
  (CT4);
* token auto-fetch, and the warning on disabling the last enabled cntlr:
  dnvctl issues no RPC the operator did not type (CT8; `cntlr` —
  `ctl/cntlr.go`);
* `BdevFeature` flags on `sp create`, so its `bdev_conf.bdev_feature_list`
  is never sent: the gateway accepts only an empty feature list
  (`architecture.md`, Common validation), and gatewayctl's `--feature-junk`,
  a driver-only poke at that validation, is not ported;
* shell-completion helpers beyond cobra's stock generator.

### Files

`ctl/root.go` holds everything the groups share: the cobra root and its
global flags, the viper binding (CT9), the dial (CT2), the per-invocation
trace id (Trace ids), the readers of leaf values and the shared flag helpers
(Conventions), the token builders (CT3), the emit pipeline (CT4), and the
error rendering and exit codes (CT5). There is one file per noun group,
named after the group, each declaring its group's register function
(`registerCluster` and its siblings), its job funcs and any parser or flag
helper kept beside them, under names carrying the group's name, so the sibling
files share no package-level flag names (the gatewayctl lesson). The command
tree and its viper binding live in `ctl/`, never in `cmd/dnvctl`
(`layout.md`, Dependency rules): `ctl/` imports only `common` and `pb`
besides cobra, pflag, viper and the gRPC and protobuf runtimes, and
`cmd/dnvctl/main.go` is a thin wrapper that imports only `common` and `ctl`,
sets the log level (CT7) and exits with the code `ctl.Execute` returns
(CT5). `make build` builds `cmd/dnvctl` with no Makefile edit. The suite is
`integtest/dnvctl_test.sh` and its fake is `integtest/fakegateway`
(Integration test plan).

### Placement rules

CT6. **etcd-free.** dnvctl links no etcd code: its only server-side
dependency is `pb` plus `common` (`layout.md`, Dependency rules;
`dependencies.md`, Direct dependencies).

CT7. **Log level Warn, logs to stderr.** `cmd/dnvctl/main.go` sets the log
level to Warn as its first statement (`log.md` R6), and that is the whole of
dnvctl's logging setup: `ctl/` installs no logger of its own, because the
init of `common` already puts every record on stderr as JSON
(`log.md` R2, R3). Leaving it there keeps the level live rather than baked
in — the handler that init builds holds `logLevel`, a slog.LevelVar, so
`SetLogLevel` still bites after the handler exists — and it leaves stdout
reserved for the one result document (CT4). Because every record of the
client interceptors is Info (`grpc.md` L1), dnvctl's own gRPC logging is
silenced by design (`log.md` R6).

## Invocation model

### Global flags, env, config

A command line is "dnvctl <group> <verb> [flags]", and the root's persistent
flags are the global flags. `--gateway-address` is the gateway's ip:port; it is
required and has no default, because unlike the test driver dnvctl has no
lab address worth baking in, and it is one address, with no multi-gateway
failover list, because operators front the gateways themselves. `--cluster`
and `--sp` are the scope global flags: they fill `cluster_name` and `sp_name` of
every request that has the field — all requests but one carry a cluster
name, and most carry a pool name — so neither is a leaf flag (Conventions).
`--rev` is the revision token (CT3): only the token-carrying mutators take
it, it is a usage error on every other leaf, and it is read off the
command line alone. `--timeout` is the per-invocation deadline in seconds,
`--trace-id` overrides the per-invocation trace-id mint (Trace ids), and
`--config` names an optional viper config file, which supplies the
env-backed global flags only.

CT9. **Environment and config binding, global flags only.** Before a leaf runs,
the root binds every global flag but `--rev` — the env-backed ones — into
viper the way the four daemons bind their flags (`bindViper`), except that
it binds those flags one by one, each looked up in the invoked command's
flag set, where a daemon binds its whole flag set; the environment prefix is
`DNVCTL`, a dash in a flag name becomes an underscore, every bound key is
looked up in the environment, and the optional `--config` file is read.
Those global flags are read back through viper — flag over environment over
config file over default, viper's standard order — so `DNVCTL_CLUSTER`,
`DNVCTL_SP` and `DNVCTL_GATEWAY_ADDRESS` work as ambient context. `--rev`
and every leaf flag are read off the parsed command line and never through
viper: `DNVCTL_REV`, `DNVCTL_FORCE`, `DNVCTL_NAME` — any environment
variable named after a leaf flag — and a config-file key of the same name
reach no request. Binding every flag would let one exported variable decide
requests nobody typed it for: `DNVCTL_REV` would put its revision on every
token-carrying write typed without `--rev`, `DNVCTL_FORCE` would force each
`clone delete` and `migr finish` typed without `--force` and turn such an
`xfer delete` into an abort, and `DNVCTL_NAME` would stand in for every
untyped `--name`, redirecting a bare `cluster get` or `cluster delete`. A
revision token is per object and per write, never ambient context.

Reading leaf values off the command line also keeps viper's silent numeric
casts out of every request: text that is not a number never reads back as
zero. A leaf value that can be malformed is parsed before the dial — by
pflag, with the command line, for the flags declared as pflag numbers or
booleans, and by dnvctl's own parsers for the id flags, `--ids`, `--slots`,
`--bm-hex`, `--level` and `--redund`, which are declared as strings
(Conventions, CT8) — so text that does not fit is exit 2 with no RPC issued.
`--timeout`, the one numeric env-backed global flag, is parsed from whichever
carrier supplied it: `DNVCTL_TIMEOUT` holding text that is not a number, or
such text under the `timeout` key of the config file, is a usage error (exit
2, no RPC issued) just as the same text after `--timeout` is, never a zero
deadline. NaN and the infinities (Inf or Infinity, signed or not), which
Go's float parser accepts in any letter case — pflag's is the same parser —
name no deadline, so they are refused the same way from any carrier, the
flag included. Every finite value is kept as typed, except that one past the
range of Go's time.Duration is clamped to it, so a huge positive `--timeout`
stays a deadline in the far future instead of being converted into one
already passed. A zero or negative `--timeout` is a deadline already passed
by its own value: the call fails DEADLINE_EXCEEDED (exit 1).

A `--cluster` or `--sp` left empty is sent empty; a command whose request
lacks the field, such as `cluster list` or `sp find-names`, ignores the
global flag (CT8).

### Connection

CT2. **The mandatory client block.** dnvctl is a real dnv component, not an
integtest driver, so it dials the gateway with the mandatory client block of
`grpc.md`, Wiring: `grpc.NewClient` under plaintext transport credentials,
with both client interceptor chains. One connection per invocation, closed
on exit. Every RPC runs under a deadline of `--timeout` seconds. Every RPC
of `service Gateway` is unary; the stream chain is installed anyway, because
the wiring rule puts both chains on every dnv connection.

### Trace ids

This is the trace-id half of CT2. Per invocation dnvctl puts one trace id
into its context with `WithTraceId`: the `--trace-id` value when it is
non-empty, else a fresh id from `NewTraceId`, the generator of `grpc.md` T4.
The client chain moves it into the outgoing `trace_id` metadata
(`grpc.md` T1); dnvctl never touches metadata itself — that shortcut is the
drivers' carve-out (`grpc.md`, Drivers and fakes) and does not apply here.
The id appears in the failure line (CT5), so a failed command hands the
operator the exact key that selects the call's records in the gateway's and
the agents' logs.

## Output and errors

### Result rendering

CT4. **Result rendering.** A successful invocation prints exactly one
canonical JSON document on stdout, through gatewayctl's emit pipeline: the
reply is marshalled as protojson with proto field names and its unpopulated
fields emitted (`marshalOpts`), then re-parsed through encoding/json and
encoded again, so the document has sorted keys, stable spacing, proto field
names, proto3 defaults visible and uint64 fields as JSON strings. The two
bitmap reads, `td get-bm` and `td get-leg-bm`, are the one deviation, for
gatewayctl's reason — protojson renders a bytes field as base64: they print
a document of two keys, `bitmap_hex`, the bitmap as lowercase hex with no
prefix, and `byte_cnt`, its length in bytes (`hexBitmapResult`). Nothing
else is ever printed to stdout on an RPC path; cobra's stock help and
completion write their text there too. There is no quiet or verbose mode.
The emit path is the package's only caller of fmt.Print*, under the
exemption `log.md` R1 makes for CLI results; the two error lines of CT5 are
the package's only other direct writes, to stderr from `Execute`, and `ctl/`
calls slog nowhere at all — the records CT7's Warn level silences are the
client interceptors'.

### Errors and exit codes

CT5. **Errors and exit codes.** dnvctl exits with one of three codes:

* 0 — the RPC returned OK: stdout carries the CT4 document and stderr is
  empty.
* 1 — the RPC or its connection failed: stdout is empty and stderr carries
  one line, "dnvctl: CODE: message (trace_id ID)".
* 2 — a usage error: an unknown command or flag, an unparsable flag value, a
  `--timeout` from the environment or the config file that is not a number,
  a NaN or infinite `--timeout` from any carrier, or a `--rev` on a command
  whose request carries no token. stdout is empty, stderr carries
  "dnvctl: message", and no RPC is issued.

The message of a usage error is cobra's or pflag's (an unknown command or
flag, a value pflag cannot parse), viper's (a `--config` file it cannot
read) or dnvctl's own: a missing `--gateway-address`; `dnvctl` or a group
typed without a verb; an "invalid --flag" message for a value dnvctl parses
itself — an id, `--ids`, `--slots`, `--bm-hex`, `--level`, `--redund`,
`--rev`, a `--timeout` from the environment or the config file, a NaN or
infinite `--timeout` from any carrier; and, for a `--rev` on a command whose
request carries no token, whatever its value, an "invalid --rev" message
naming the command as one that takes no revision token. CODE is the gRPC
code in its UPPER_SNAKE wire spelling — NOT_FOUND, ABORTED,
DEADLINE_EXCEEDED, UNAVAILABLE and the rest — from the `codeNames` table,
because the code's own string form is CamelCase and operators grep for the
wire spelling; message is the status message verbatim, such as
"stale revision". A dial that cannot resolve, or is refused, maps to
whatever code gRPC reports — UNAVAILABLE for a refused connection; dnvctl
adds no translation layer.

## Revision tokens

CT3. **Revision tokens.** A request that carries a token carries one of
three messages: `dn_rev` on `DeleteDiskNode` and `UpdateDiskNodeDisabled`,
`cn_rev` on their two controller-node mirrors, and `sp_rev` on every
SP-scoped mutator — every create, delete, update, append, finish, cancel and
switch under a pool, and `GrowSlice`. Reads carry none; an operator reads
the tokens with `dn get`, `cn get` and `sp get`. dnvctl sends exactly what
was typed:

* `--rev` not given: the token field is absent, a nil message.
* `--rev` with a value, parsed with Go's base-0 rule, so decimal and
  `0x`-prefixed hex both work: the token message is present with `revision`
  set to it and nothing else set — the gateway compares the revision alone
  and ignores the token's echo fields (`gateway.md` GW6).
* `--rev 0`: the message is present with revision zero — proto3 message
  presence keeps this distinguishable from omission — and is the deliberate
  always-stale probe, since a stored revision starts above zero and only
  grows.
* "Given" means typed on this command line (pflag's Changed bit), nothing
  else: `--rev` has no environment or config carrier (CT9), so an exported
  `DNVCTL_REV` or a `rev` key in the `--config` file sends no token. On a
  token carrier a typed `--rev` must parse, an empty value included: a
  `--rev` fed an unset shell variable is a usage error (exit 2), not an
  ungated write.
* On every other command — the reads, and the writes that carry no token
  either, `cluster create`, `cluster delete`, `dn create`, `cn create` and
  `sp create` — a typed `--rev` is a usage error (exit 2, no RPC issued),
  whatever its value. No field could carry it, so sending the request anyway
  would drop it unseen: a `cluster delete` typed with `--rev` would delete
  ungated while looking gated.

The gateway's check is presence-based (`gateway.md` GW6): an absent token
skips it, and a present one must equal the stored revision. A token-less
mutator therefore succeeds against a real gateway, and succeeds ungated —
the bypass costs exactly the optimistic-concurrency gate. For concurrent or
scripted work the operator still reads the token with `sp get` and passes it
with `--rev`, and keeps the token-less form for interactive single-operator
use.

## The command tree

### Conventions

**Groups and leaves.** The tree is noun-grouped, "dnvctl <group> <verb>",
one group per resource — `cluster`, `dn`, `cn`, `sp`, `cntlr`, `td`, `ss`,
`ns`, `clone`, `xfer`, `migr` and `spare` — and it reuses gatewayctl's verb
and flag vocabulary. The leaves are the RPCs of `service Gateway`, one leaf
per RPC and nothing else (CT1): no convenience verbs, no compound commands,
and cobra's stock `help` and `completion` are the only subcommands that are
not RPCs. The RPCs without a group of their own are homed in a group:
`GrowSlice`, `InspectSide`, `FindStoragePoolNames` and
`UpdateStoragePoolCntlidSlotList` under `sp`, `GetThinDeviceBitmap` and
`GetLegBitmap` under `td`, `AppendCloneBitmap` under `clone` and
`AppendMigrationBitmap` under `migr`. A group issues no RPC: typed without a
verb, or with a verb it lacks, it is a usage error (CT5), never a help
screen on stdout, unless `--help` is typed.

**Verbs.** An object's create, delete, get and list RPCs are its `create`,
`delete`, `get` and `list` leaves. An update RPC is a `set-` leaf named
after what it sets (`dn set-disabled`, `sp set-cntlid-slots`, `ns set-dev`,
`clone set-tr`), and it sends the value it is given, never a toggle:
re-sending a node's disabled flag or a cntlr's enabled flag as it is stored
is the gateway's idempotent no-write (`gateway.md` GW6), not a flip back, so
the leaf is safe to script. An inspect RPC is an `inspect` leaf
(`sp inspect-side` for a side), a live read the gateway answers from the
agent that hosts the object, not from etcd. The bitmap RPCs are `get-bm`,
`get-leg-bm` and `append-bm`; every other leaf keeps its RPC's verb
(`sp find-names`, `sp grow-slice`, `migr finish`, `migr cancel`,
`spare switch`).

**Fields and flags.** Every request field maps to exactly one flag, with
these exceptions:

* `cluster_name` and `sp_name` come from the scope global flags `--cluster` and
  `--sp`, `sp create` included, except that the `cluster` group's own
  commands name their cluster with `--name`, which falls back to the global
  `--cluster` when it is empty (gatewayctl's `clusterNameOf` rule);
* a token message takes its `revision` from the global `--rev` (CT3), and
  its echo field — `sp_rev.sp_name`, `dn_rev.addr_port`, `cn_rev.addr_port`
  — is never sent;
* `sp create`'s `redund_conf` is a oneof whose arm one flag, `--redund`,
  picks (`sp` — `ctl/sp.go`);
* the repeated `src_tr_conf` of `clone create` and `clone set-tr` is filled
  by one prefixed set of transport flags with one element, or with none when
  all four are emptied (`clone` — `ctl/clone.go`);
* the members no flag reaches are never sent: `cluster create`'s
  `qos_ratio`, `bdev_conf`, `alloc_conf`, `health_check_conf` and the four
  bin shifts of `dn_bin_conf` (`cluster` — `ctl/cluster.go`), and
  `sp create`'s `bdev_conf.bdev_feature_list` (In scope / out of scope).

A leaf has a flag only where its request has a field: `migr cancel` takes no
`--force` and `migr append-bm` no index (`migr` — `ctl/migr.go`). An
optional sub-message follows the "not given" convention: it is sent only
when one of its flags holds a non-zero value, a zero meaning the member was
not given, except for the token messages (CT3) and where a group below says
otherwise. Each leaf's flags,
with their types and defaults, are the code's, and `--help` lists them.

**Identity flags.** Each group names its object with identity flags: `cluster` with `--name`; `dn` and `cn` with
`--addr`, because a node's name is its ip:port; `sp` with the global `--sp`;
`cntlr` with `--id`; `td`, `clone`, `xfer` and `migr` with `--name`; `ss`
with `--nqn`; `ns` with `--nqn` and `--idx`; `spare` with `--grp`, plus
`--leg` on delete and `--spare` and `--target` on switch. `sp inspect-side` takes its side id as `--id`, deliberately the
same spelling as the `cntlr` group's, as in gatewayctl: the RPC, not the
flag, says what the id means.

**Value types.** An id — `--id`, `--slice`, `--leg`, `--src-side`, `--grp`,
`--spare`, `--target` — is declared as a string and parsed by dnvctl with
Go's base-0 rule, so a `0x` prefix is accepted. `--slots` and `--ids` are
comma-separated numeric lists; `--hosts` and the selector flags are
comma-separated string lists. A list flag replaces its value on each
occurrence rather than accumulating, which is why the lists are plain string
flags: pflag's own slice flags append. A boolean that must be turned off is
written `--enabled=false`, never `--enabled false`, whose `false` would be a
positional argument, which no leaf accepts.

**Defaults.** dnvctl keeps a leaf-flag default of its own only where a spec
names one: `sp create`'s redundancy (`sp` — `ctl/sp.go`), the transport flags
wherever they appear, and the value flags of `sp set-level`,
`cntlr set-enabled` and `ns set-suspended`. A flag default is not a hidden
RPC (CT8).

**Shared flag helpers.** Each has one implementation, in `ctl/root.go`:

* `trConfFlags` adds the four flags of an `NvmeTrConf` — `--tr-type`,
  `--adr-fam`, `--tr-addr` and `--tr-svc-id` — under a prefix: none on
  `dn create` and `cn create`, `src-` for a clone's source. They default to
  a lab-shaped loopback TCP transport, and all four emptied make a nil conf
  (`trConfOf`).
* `selectorFlags` adds the black and white lists of a `NodeSelector` under a
  prefix, `--dn-black` and `--dn-white` or `--cn-black` and `--cn-white`;
  both empty make a nil selector (`selectorOf`).
* `dmCloneConfFlags` adds `--hyd-threshold` and `--hyd-batch`; both zero
  make a nil `DmCloneConf` (`dmCloneConfOf`), the "not given" convention of
  `gateway.md` GW11.
* `pageFlags` adds `--count`, where zero asks for the server's default page
  size, and `--page-token`, to the paged list leaves.

CT8. **No client-side validation.** dnvctl rejects only what fails to parse
(exit 2) — a malformed `--bm-hex`, a non-numeric id, a `--timeout` that is
not a finite number of seconds (CT9) — plus a `--rev` on a command whose
request has no token field to carry it (CT3). Every parsed value is sent as
typed, and the gateway's validation (`architecture.md`, Common validation)
answers: empty required fields, contradictory flags and unknown enum numbers
are all forwarded. Nor does dnvctl issue an RPC the operator did not type:
no token auto-fetch, no pre-read behind a warning (`cntlr` —
`ctl/cntlr.go`), no poller behind a latching delete (`sp` — `ctl/sp.go`).

CT1. **Completeness.** The leaves are exhaustive: one per RPC of
`service Gateway`, none left out and none doubled. A leaf carries the
`--rev` token exactly when its request has a token field, so the leaves that
take `--rev` are exactly CT3's carriers, and `--rev` on any other leaf is a usage
error. Unit tests pin the leaves against `pb.Gateway_ServiceDesc` in both
directions and against the tree the root actually builds, and the token
carriers against the requests that have a token field; the integration sweep
makes the same claim on the wire (Integration test plan).

### `cluster` — `ctl/cluster.go`

`cluster list` is the one request with no `cluster_name`: it ignores both
scope global flags.

One `cluster create` flag, and only one, reaches a `ClusterConf` member:
`--extent-size` sets `dn_bin_conf.extent_size`, the DN and CN allocation
unit (`architecture.md`, Size → extents), which `DefaultDnExtSize` fixes
otherwise. A pool of `MaxSliceCntPerSp` slices takes a meta group and a data
group per slice, each with one side per leg, so on lab-sized backing devices
the unit has to come down to `MinDnExtSize` — that is why the flag exists. A
non-zero value sends a `dn_bin_conf` carrying `extent_size` and nothing
else; zero sends no `dn_bin_conf` at all, which is the pure-defaults request
of a bare `cluster create` and the same "not given" convention as
`sp create`'s `--stripe-size` and `--block-size`, which build
`dm_raid0_conf` and `dm_pool_conf` only when non-zero (`dm_pool_conf` also
for a non-zero `--low-water-mark-pct`). The four bin shifts have no flag and
are never sent, so the gateway reads the all-zero shift set — not a ladder
but proto3's "unset", accepted as the one exception, while any other
non-ladder set is INVALID_ARGUMENT — and falls back to the default ladder as
a whole set, never shift by shift (`architecture.md`, DN bins;
`architecture.md`, Clusters); a flag for a single shift could only build a
set that changes nothing or one the gateway refuses. The bound on a non-zero
size is the gateway's (`architecture.md`, Common validation): dnvctl
range-checks nothing (CT8), and neither end applies an alignment or
power-of-two rule. The command line is the flag's only carrier, like every
leaf flag's (CT9), which matters most here because the value is write-once:
`DNVCTL_EXTENT_SIZE` and an `extent-size` key in the `--config` file reach
no request, so no stale variable sizes every cluster created under it, and
text that is not a uint64 is a pflag parse error (exit 2, no RPC).

No `cluster create` flag reaches the request's other conf members.
`bdev_conf`, `alloc_conf` and `health_check_conf` are stored as the
gateway's resolved defaults — the cluster's `bdev_conf` is only the per-pool
default set, which `sp create`'s own `bdev_conf` flags override member by
member per pool (`architecture.md`, Storage pools) — while `qos_ratio` is
stored exactly as sent, because alone among the request's conf members it
has no default and its proto3 zero keeps meaning "unset" (`architecture.md`,
Common validation). `creation_epoch`, the remaining `ClusterConf` member, is
no request field: the gateway stamps it. And because `ClusterConf` is
write-once — no RPC updates it (`architecture.md`, Clusters) —
`--extent-size` is the only chance a cluster's extent size gets to be set.

### `dn` — `ctl/dn.go`

`dn create` reaches the agent: the gateway calls the DN agent's `GetDnSize`
inline (`gateway.md`, Agent calls), so the invocation's trace id chains
through to the agent's records (`grpc.md` T3). `dn inspect` is the group's
live read, the DN agent's `GetDnInfo` rather than etcd.

### `cn` — `ctl/cn.go`

The `cn` group mirrors `dn` exactly against the controller-node RPCs: the
same verbs and the same flags, the requests differing only in carrying
`cn_rev` where the `dn` ones carry `dn_rev`, and `cn create` reaching the CN
agent's `GetCnSize` as `dn create` reaches `GetDnSize`. A change to one
group is a change to both.

### `sp` — `ctl/sp.go`

`sp create` always sends a `bdev_conf`, because it always sends a
`redund_conf`: `--redund` picks the arm and defaults to the md-raid1 one, as
`architecture.md`, Storage pools, says the CLI does, while the gateway reads
an unset `redund_conf` as `redund_none`. The raid1 arm carries
`--bitmap-chunk-blocks`, zero leaving the control plane its own chunk size,
and `none` sends `redund_none`. An unknown `--redund` spelling is a parse
failure (exit 2) rather than a forwarded value, because the flag names a
oneof arm and the oneof has no third arm to carry it. The other members
follow the "not given" convention: `dm_raid0_conf` is sent only for a
non-zero `--stripe-size`, `dm_pool_conf` only when `--block-size` or
`--low-water-mark-pct` is non-zero, carrying both, and `event_threshold`
only when one of `--thr-primary`, `--thr-cntlr`, `--thr-side` and
`--thr-leg` is non-zero. `--low-water-mark-pct` is the pool usage percentage
past which the worker grows a slice (`architecture.md`, Automatic
reactions): zero leaves the gateway to take the cluster's mark, else
`DefaultPoolLowWatermarkPct`, and a value above 100 — forwarded as typed
like every other (CT8) — turns that auto-grow off. A zero `--slice-cnt` or
`--cntlr-cnt` is forwarded as the zero (CT8), and the gateway substitutes
`DefaultSliceCntPerSp` or `DefaultCntlrCntPerSp`; a zero `--init-ext-cnt`
gets no such substitution, and the gateway answers INVALID_ARGUMENT.

`sp delete` latches and returns (`architecture.md`, Storage pools), so a
successful `sp delete` means teardown started, not gone. An operator polls
`sp get` until NOT_FOUND; while the pool drains, `sp get` shows `deleting`
true and a shrinking inventory, an `sp create` of the same name is
ALREADY_EXISTS, every other `sp` mutator is FAILED_PRECONDITION, and a
repeated `sp delete` is an OK no-op. dnvctl adds no wait flag: the polls
would be RPCs the operator did not type (CT8), and the surface stays one
command per RPC (CT1).

`sp set-cntlid-slots` sends an empty `--slots` as typed and the gateway
refuses it, while an empty list on `sp create` asks for the gateway's
default. `sp set-level` takes a short level name, an `SP_LEVEL_*` name or a
raw number — the number so that an undeclared enum value is forwarded for
the gateway to refuse (CT8) — and `--level` defaults to read-write, the
enum's zero. `sp grow-slice`'s `--meta` and `--ext` do not constrain each
other client-side (CT8).

### `cntlr` — `ctl/cntlr.go`

`cntlr set-enabled` takes `--enabled`, which defaults to true, so disabling
a cntlr is written `--enabled=false`. Disabling the last enabled cntlr of a
pool stops its IO; the gateway allows it (`architecture.md`, Cntlrs), and
dnvctl does not warn: the warning would need a pre-read of the pool's
cntlrs, an RPC the operator did not type (CT8), so it is deferred until the
gateway itself carries the hint in its reply. `cntlr delete` needs a cntlr
that is neither the primary nor enabled; dnvctl checks neither condition,
both being server state (CT8).

### `td` — `ctl/td.go`

`td create` with `--ori` takes a snapshot of that thin device, and `--size 0`
is legal only then, the snapshot inheriting its origin's size;
dnvctl does not enforce the pairing (CT8). `td list` is the `created` poll,
the client's wait primitive of `architecture.md`, Thin devices: CT4's
rendering shows `created` on every thin device, false included. Its reply
carries no revision while the `created` flip bumps `SpRev`, so the token of
a snapshot's `td create` comes from an `sp get` made after the last poll —
for a clone's destination, after `clone get` answers NOT_FOUND (`clone` —
`ctl/clone.go`), because the latch and the drain bump `SpRev` too.
`td get-bm` and `td get-leg-bm` print the hex map of CT4; `td get-bm` names
its device with the group's `--name`, where gatewayctl spells it `--td`.

### `ss` — `ctl/ss.go`

`ss list` is the only gateway read that shows namespaces: a namespace is a
field of its subsystem, so the `ns` group creates and changes namespaces but
has no read of its own. `ss set-hosts` is a full replacement of
`allowed_hosts`. An empty list admits no host
(`architecture.md`, Primary cntlr), which is how a subsystem is staged or
closed to new connections: one made by `ss create` without `--hosts` admits
no host until `ss set-hosts` grants its hosts, and `ss set-hosts` with an
empty `--hosts` revokes every host.

### `ns` — `ctl/ns.go`

`ns create` with an empty `--uuid` or `--nguid` leaves the gateway to mint
that identity (`architecture.md`, Subsystems, namespaces); dnvctl generates
nothing itself. `ns set-suspended` takes `--suspended`, which defaults to
true, so resuming is written `--suspended=false`; the RPC writes and bumps
even when the stored flag already matches, outside the idempotent no-write
of `gateway.md` GW6.

### `clone` — `ctl/clone.go`

`clone create` and `clone set-tr` send the source transport as a one-element
`src_tr_conf`, and as an empty list — never a default transport — when all
four `src-` transport flags are emptied, so the gateway's refusal of an
empty list stays reachable from the CLI (`cloneSrcTrConfList`).

`clone delete` with `--force` skips the proof, read from the primary cntlr's
CN, that the copy finished. The RPC latches and returns (`architecture.md`,
Clones), so a successful `clone delete` means teardown started. An operator
polls `clone get` until NOT_FOUND; while the clone drains, `clone get` shows
`deleting` true, `clone append-bm` and `clone set-tr` are
FAILED_PRECONDITION, a same-name `clone create` is ALREADY_EXISTS —
RESOURCE_EXHAUSTED if the surviving entry holds the pool at
`MaxCloneCntPerSp`, which also blocks an unrelated `clone create` — and
`sp delete` is still refused, FAILED_PRECONDITION naming what the pool still
holds. The count it names is thin devices, which the gateway checks before
clones: the destination thin device cannot leave `td_name_list` while the
draining clone is in `clone_name_list`, which lasts until the drain's last
transaction, so `sp delete` stays refused until the `td delete` below, not
merely until the drain ends. The destination thin device is held for the
whole drain too: a `td delete` of it, a snapshot of it and a replacement
`clone create` onto it are FAILED_PRECONDITION until the drain's last
transaction, all three scans walking `clone_name_list`. Abandoning a clone
is therefore `clone delete`, a poll of `clone get` to NOT_FOUND, an
`ns delete` of the namespace the destination backs — the cross-SP live
migration shape always has one (`architecture.md`, Transfer + clone =
cross-SP live migration), and `td delete` checks namespaces before it scans
the clones — and then `td delete`. A repeated `clone delete` is an OK no-op,
forced or not. The destination namespace resumes with the latch, not at the
end of the drain.

`clone append-bm` addresses a chunk by the pair `--src-slice-idx` and
`--bm-idx`, never by either alone: chunk (s, b) is the bytes of source slice
s's bitmap from b times `CloneBmChunkBytes` on, so chunks may be sent in any
order and left unsent, while the pages of one chunk must arrive in order,
the gateway appending each at that chunk's current length (`gateway.md`,
Clones). An empty `--bm-hex` sends an empty bitmap on purpose, so the
gateway's refusal of one stays reachable; a malformed non-empty value is a
usage error (exit 2).

### `xfer` — `ctl/xfer.go`

`xfer delete` with `--force` is an abort, not a skipped proof: the origin
namespace's `suspended` is left false, so the next syncup unparks it and
restores its ANA state; without `--force` the delete finalizes the
hand-over, retiring the origin namespace in the same transaction
(`architecture.md`, Transfers). `xfer set-hosts` replaces the whole host
list.

### `migr` — `ctl/migr.go`

Two leaves are deliberately not symmetric with their siblings, because their
RPCs are not: `migr cancel` has no `--force` — throwing an unfinished copy
away needs no proof about the copy — while `migr finish` has one; and
`migr append-bm` takes no index, because a migration copies one side and its
chunks are an append sequence, while a clone chunk is addressed by the pair
of source slice and chunk index (`clone` — `ctl/clone.go`).

## Integration test plan

**What the suite proves.** `integtest/dnvctl_test.sh` proves, against a real
gRPC wire, that every leaf of the command tree marshals its argv into
exactly the intended request proto, renders replies per CT4, maps errors
per CT5 and carries trace ids per CT2 — with the gateway replaced by
`integtest/fakegateway`, so the suite tests dnvctl only. Gateway semantics —
validation, tokens, transactions — stay the gateway suite's job
(`gateway.md`, Integration test plan), and the data plane is not touched.
Correctness only: the suite's timing assertions bound a deadline, not a
latency.

**Topology.** One VM, in the pattern of the worker and gateway suites: the
developer machine builds `dnvctl` and the fake, ships both to the VM and
drives every step over ssh, as a plain user with no sudo anywhere — dnvctl
is itself the driver here, and the VM needs neither a Go toolchain nor jq,
since every filter runs on the developer machine. The fake listens on the
VM's loopback, so nothing is reachable off-box, on a port no other suite or
production uses, beside a second port that is deliberately never listened
on, for the refused dial. Every step runs under a trace id of its own, the
thread that ties a script stage to the fake's records, and brings back in
one round trip the exit code, stderr, stdout and the fake's state file from
before and after the call.

**Cases.** The cases run in a fixed order, fail-fast, each against a
restarted fake whose behavior file is reset and whose state file holds only
the reset's own readiness probe, so counters are read only as deltas.

* smoke — one call under an explicit trace id, observed on the wire; the
  same call without `--trace-id`, arriving under a minted id; and stderr
  empty on both successes.
* sweep — one step per RPC, in tree order, each asserting exit 0, a stdout
  that parses, the recorded request equal to the argv-implied one as a whole
  object, scope global flags and token included, and the RPC's count moved by
  exactly one; a closing audit finds one request record under each step's
  trace id, over as many distinct RPCs as there are steps. The replies held
  to byte-exact goldens are an empty canned reply, a revision beyond the
  exact range of a JSON number, and the bitmap reads' hex maps.
* behavior — the token trio: no `--rev` sends no token message, `--rev 0` a
  present empty one, a hex value its base-0 parse; `sp create`'s other
  redundancy arm; `td list` rendering `created` both true and false;
  `DNVCTL_CLUSTER` filling `--cluster` and `--cluster` on the command line
  winning over it; and a `cluster` command's `--name` winning over
  `--cluster`.
* errors — injected refusals rendered per CT5, the stale-revision refusal an
  operator meets among them; and usage errors — an unknown flag, a malformed
  `--bm-hex`, a `--rev` on a command with no token field — each exit 2 with
  the fake's count unmoved.
* transport — a dial to the port nothing listens on is UNAVAILABLE and fails
  fast; a hanging fake under a short `--timeout` is DEADLINE_EXCEEDED within
  a bound; and clearing the hang restores the command.

**Rules exercised.** The smoke case exercises CT2's trace id on the wire and
its mint, CT4's parsing stdout and CT7's empty stderr; the sweep
exercises CT1 on the wire, the field and flag rules of Conventions — the
scope global flags, the `cluster` group's `--name`, the selector and dm-clone
nil rules, `sp create`'s always-present `redund_conf`, `--extent-size` and
`--low-water-mark-pct`, a clone chunk's pair of distinct indices — CT3's
token on exactly its carriers, and CT4's goldens: the emitted defaults and
key order, uint64 as a string, the hex maps; the behavior case
exercises CT3's presence trio, CT4 on `created`, CT9's precedence of flag
over environment and the `cluster` group's `--name`; the errors case
exercises CT5's whole failure line and CT8's parse-only refusals, CT3's
refusal of a `--rev` with no field to fill among them, none of them reaching
the fake; and the transport case exercises CT2's deadline and CT5's
UNAVAILABLE and DEADLINE_EXCEEDED. Left to the unit tests: what one step per
RPC has no room for — a bare `cluster create` sending no `dn_bin_conf`, the
transport-conf nil rule and clone's empty `src_tr_conf`, the
refusal of `--rev` on every leaf without a token field, the silence of the
environment and the config file on leaf flags (CT9), `--timeout` refused or
clamped from every carrier, and the code name of every gRPC code (CT5).

Out of scope: real-gateway runs, which are the gateway suite's harness;
concurrency, which stays with gatewayctl's barrier runner; the copier;
performance; and any assertion about the gateway's reaction to what dnvctl
sends.

**What a pass means.** Exit status zero and `PASS` mean every case's
assertions held, in a run that stops at the first failure. A request is
asserted as a whole object, the fake's recorded request against the
argv-implied one, so an unexpected key fails as loudly as a wrong value —
which is what makes "no token key" provable rather than assumed. The fake
records a request without its unpopulated fields, so an absent `--rev` is an
absent key and `--rev 0` an empty object, and uint64 fields compare as
strings, protojson's doing. A count is a delta between two state snapshots
taken in the same round trip as the call, so nothing can slip in between; a
usage error passes only with the count unmoved. Every success parses stdout,
and goldens compare byte-exact. The token assertions are request-side — what
dnvctl put on the wire — so they hold whatever a gateway does with the
token. Waits are bounded polls for process start and exit only: every dnvctl call
is synchronous. The fake's log is read through a filter that drops a line
torn by a concurrent append.

**Cleanup.** Cleanup runs unconditionally at the start of every run, and at
the end only on success; a cleanup-only run does it alone and then verifies
that both ports are free and the work directory is gone. A failing run
leaves the work directory, the fake's log and both of its files in place and
prints its diagnostics — the failing stage and its trace id, the fake's
recent request records and its behavior-file and state-file records, both
files verbatim, the last invocation's exit code and output, the listeners on
the two ports and the suite's processes — so the debris is what the
developer reads. Cleanup signals the fake by its recorded pid, with a
pattern kill as a safety net for it and for a straggling dnvctl, which runs
in the foreground of each step's ssh and has no pid file, then removes the
work directory; no file outside it is removed.

**The fake gateway, `fakegateway`.** `integtest/fakegateway` stands in for
the whole control plane so that the suite can read the exact request a
command sends. It serves every method of `service Gateway`, all unary,
behind the real server interceptors, so its log carries a request and a
reply record for every call under the caller's trace id — the record of
which RPC dnvctl issued and under which id (`grpc.md`, Drivers and fakes);
it installs no logger of its own and prints nothing on stdout. It mirrors
the behavior-file and state-file contract of the worker suite's fake agent
(`dnv-worker.md`, Integration test plan), keyed per method rather than per
object, because it models no cluster state. Every call is recorded first —
the method's count and its last request, the state file written atomically —
so a refused or hung call is recorded too, and only then answered from the
behavior file: an injected status code and message; a hang, held until the
lever is cleared or the call's deadline ends; an injected reply, strict
protojson of the method's reply type; and otherwise an empty reply, whose
shape dnvctl's rendering fills in. The behavior file is re-read whenever it
changes, an unknown key or a malformed file leaves the previous behavior in
force, and a vanished file resets it to the defaults.

