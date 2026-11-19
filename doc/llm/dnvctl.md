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

* any surface beyond one command per RPC (CT1);
* TLS and authentication: dnv is plaintext gRPC end to end;
* multi-gateway failover: dnvctl dials the one `--gateway-address`, and
  operators front the gateways themselves;
* table or human output modes but one: every result is one canonical JSON
  document, save the status read's table (CT4);
* token auto-fetch, and the warning on disabling the last enabled cntlr:
  dnvctl issues no RPC the operator did not type (CT8; `cntlr` —
  `ctl/cntlr.go`);
* `BdevFeature` flags on `sp create`, so its `bdev_conf.bdev_feature_list`
  is never sent: the gateway accepts only an empty feature list
  (`architecture.md`, Common validation), and dnvctl has no flag for one;
* shell-completion helpers beyond cobra's stock generator.

### Files

`ctl/root.go` holds everything the groups share: the cobra root and its
global flags, the viper binding (CT9), the dial (CT2), the per-invocation
trace id (Trace ids), the readers of leaf values and the shared flag helpers
(Conventions), the token builders (CT3), the rendering of a result and its
one checked write to stdout (CT4), and the error rendering and exit codes
(CT5). There is one file per noun group, named after the group, each
declaring its group's register function (`registerCluster` and its
siblings), its job funcs and any parser or flag helper kept beside them,
under names carrying the group's name, so the sibling files share no
package-level flag names. The command tree and its viper
binding live in `ctl/`, never in `cmd/dnvctl`, and what `ctl/` and
`cmd/dnvctl` may import is `layout.md`, Dependency rules; `cmd/dnvctl/main.go`
is a thin wrapper that sets the log level (CT7) and exits with the code
`ctl.Execute` returns (CT5). The suite is `integtest/dnvctl_test.sh` and its
fake is `integtest/fakegateway` (Integration test plan).

### Placement rules

CT6. **etcd-free.** dnvctl links no etcd code; the import rules that keep
it so are `layout.md`, Dependency rules.

CT7. **Log level Warn by default, logs to stderr.** dnvctl logs at Warn, set
as the first statement of its `main`, and then at its `--log-level`, `warn`
by default, which the root sets once the binding of CT9 has resolved it and
before anything is dialled, a word other than `debug`, `info` or `warn`
being a usage error (`log.md` R6; CT5). That is the whole of its logging
setup: `ctl/` installs no logger of its own, because the init of `common`
already puts every record on stderr as JSON (`log.md` R2, R3), which leaves
stdout reserved for the one result document (CT4). The records the Warn
log level silences are the client interceptors', every one of them Info, since
dnvctl makes only unary calls (`grpc.md` L1); `--log-level info` shows them
on stderr.

## Invocation model

### Global flags, env, config

A command line is "dnvctl <group> <verb> [flags]", or "dnvctl --version",
and the root's persistent flags are the global flags; `--help` lists them
with their types and defaults. `--version` is the root's own flag, not a
persistent one: a group or a verb refuses it as a usage error, and nothing
binds it into viper (`architecture.md`, Components: invocation reference).
`--gateway-address` is required and has no default, because
dnvctl has no lab address worth baking in, and it is one address, with no
multi-gateway failover list, because operators front the gateways
themselves. `--cluster` and `--sp` are the scope global flags: they fill
`cluster_name` and `sp_name` of every request that has the field — all
requests but two carry a cluster name, and most carry a pool name — so
neither is a leaf flag (Conventions). `--rev` is the revision token (CT3):
only the token-carrying mutators take it, it is a usage error on every other
leaf, and it is read off the command line alone. The rest steer the
invocation itself: `--timeout` is the per-invocation deadline in seconds,
`--trace-id` overrides the per-invocation trace-id mint (Trace ids),
`--log-level` sets the level of the records on stderr (CT7), and
`--config` names an optional viper config file, which supplies the
env-backed global flags only.

CT9. **Environment and config binding, global flags only.** Before a leaf runs,
the root binds every global flag but `--rev` — the env-backed ones — into
viper the way the five daemons bind their flags (`bindViper`), except that
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
`dn delete`, `cn delete`, `clone delete`, `migr finish` and `migr cancel`
typed without `--force` and turn such an `xfer delete` into an abort, and `DNVCTL_NAME` would stand in for every
untyped `--name`, redirecting a bare `cluster get` or `cluster delete`. A
revision token is per object and per write, never ambient context.

Reading leaf values off the command line also keeps viper's silent numeric
casts out of every request: text that is not a number never reads back as
zero. A leaf value that can be malformed is parsed before the dial — by
pflag, with the command line, for the flags declared as pflag numbers,
after dnvctl's check of their spelling (Conventions, Value types), or as
booleans, and by dnvctl's own parsers for the id flags, `--ids`,
`--bad-legs`, `--slots`, `--bm-hex`, `--level`, `--redund` and
`--override`, which are declared as strings (Conventions, CT8) — so text
that does not fit its field is exit 2 with no RPC issued.
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
lacks the field, such as `cluster list`, `worker list` or `sp find-names`,
ignores the global flag (CT8).

### Connection

CT2. **The mandatory client block.** dnvctl is a real dnv component, not an
integtest driver, so it dials the gateway with the mandatory client block of
`grpc.md`, Wiring: `grpc.NewClient` under plaintext transport credentials,
with both client interceptor chains and the message limit. One connection
per invocation, closed on exit. Every RPC runs under a deadline of
`--timeout` seconds. Every RPC of `service Gateway` is unary; the stream
chain is installed anyway, because the wiring rule puts both chains on every
dnv connection.

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
canonical JSON document on stdout, through its own emit pipeline (`emit`),
the same shape as gatewayctl's: the reply is marshalled as protojson with
proto field names and its unpopulated fields emitted (`marshalOpts`), then
re-parsed through encoding/json and encoded again, so the document has
sorted keys, stable spacing, proto field names, proto3 defaults visible and
uint64 fields as JSON strings, save that a proto3 `optional` field that is
not set, such as `probe_times.read_ms` after a failed write or
`last_success_age_ms` before any round succeeded, is left out, as protojson
leaves out every member of a oneof that is not set. Two leaves
deviate. The bitmap read, `td get-bm`, because protojson renders a bytes
field as base64: it prints a document of two keys, `bitmap_hex`, the
bitmap as lowercase hex with no prefix, and `byte_cnt`, its length in
bytes (`hexBitmapResult`). And the status read, `cluster get-status`,
which is read by a person in an incident: it prints a table by default
and the document only with `--json` (`statusTable`; `cluster` —
`ctl/cluster.go`). Every result reaches stdout the same way: `render`
makes the document and its newline, or the table, and `writeResult`
writes them in one checked write, the package's only write to stdout, under the exemption
`log.md` R1 makes for CLI results. A reply that cannot be rendered, or a
write that fails or ends short, is CT5's exit 3, never a silent exit 0.
Nothing else is ever printed to stdout on an RPC path; the only other texts
there are printed outside `writeResult`, all cobra's: its stock help and
the answers of its hidden command for the shell's completion requests,
whose writes cobra does not check; the scripts of its `completion` leaves,
whose write error cobra returns, so a failed one is CT5's exit 2; and the
root's `--version` line (CT5), whose write cobra checks too, a failed one
being CT5's exit 2. There is no quiet or verbose mode. The error lines of
CT5 are the package's only other direct writes, to stderr from `Execute`,
and `ctl/` calls slog nowhere at all — the records CT7's Warn level
silences are the client interceptors'.

### Errors and exit codes

CT5. **Errors and exit codes.** dnvctl exits with one of four codes:

* 0 — the RPC returned OK: stdout carries the CT4 document, or the
  status read's table, and stderr is empty; also `--help`, cobra's `help` and `completion` commands, and
  `dnvctl --version`, which prints "dnvctl" and the build version
  (`architecture.md`, Components: invocation reference), each with no RPC.
* 1 — the RPC or its connection failed: stdout is empty and stderr carries
  one line, "dnvctl: CODE: message (trace_id ID)".
* 2 — a usage error: an unknown command or flag, an unparsable flag value,
  such as a number spelled other than in decimal or in hex after `0x`, or
  an id or `--rev` typed empty (Conventions, Value types), a `--timeout`
  from the environment or the config file that is not a number, a NaN or
  infinite `--timeout` from any carrier, or a `--rev` on a command whose
  request carries no token. stdout is empty, stderr carries
  "dnvctl: message", and no RPC is issued. A failed write of the root's
  `--version` line exits 2 too: it is no usage error, but like one it
  issued no RPC, so a repeat changes nothing. Its stdout holds none or part
  of the line, and stderr two lines, cobra's of the write's error and then
  "dnvctl: message". So does a failed write of a `completion` script, for
  the same reason, with stderr the one line "dnvctl: message".
* 3 — the RPC returned OK, and its result was not written: the reply could
  not be rendered (CT4), or the write to stdout failed or ended short, as on
  a full disk under a redirect. stdout holds no complete document or
  table, and stderr
  carries one line, "dnvctl: the call returned OK, but its result was not
  written: error (trace_id ID)". The call ran to its end, so any change it
  asked for was made, and a script reads the state again before it repeats
  the command. It is a code of its own because exit 1 would tell a script the
  call failed, and a repeated create would then meet ALREADY_EXISTS, while
  exit 2 says no call was made. A stdout that is a pipe whose reader has gone
  never reaches this code: Go ends the process with SIGPIPE at the write,
  which a shell reports as status 141.

The message of a usage error is cobra's or pflag's (an unknown command or
flag, a value pflag cannot parse or dnvctl's spelling check refuses before
pflag reads it), viper's (a `--config` file it cannot read) or dnvctl's own: a
missing `--gateway-address`; `dnvctl` or a group typed without a verb; an
"invalid --flag" message for a value dnvctl parses itself — an id, `--ids`,
`--bad-legs`, `--slots`, `--bm-hex`, `--level`, `--redund`, `--override`,
`--rev`, a `--timeout` from the environment or the config file, a NaN or
infinite `--timeout` from any carrier; and, for a `--rev` on a command
whose request carries no token, whatever its value, an "invalid --rev"
message naming the command as one that takes no revision token. CODE is
the gRPC code in its UPPER_SNAKE wire spelling — NOT_FOUND, ABORTED,
DEADLINE_EXCEEDED, UNAVAILABLE and the rest — from the `codeNames` table,
because the code's own string form is CamelCase and operators grep for the
wire spelling; message is the status message verbatim, such as "stale
revision". A dial that cannot resolve, or is refused, maps to whatever
code gRPC reports — UNAVAILABLE for a refused connection; dnvctl adds no
translation layer.

## Revision tokens

CT3. **Revision tokens.** A request that carries a token carries one of
three messages: `dn_rev` on `DeleteDiskNode` and `UpdateDiskNodeDisabled`,
`cn_rev` on their two controller-node mirrors, and `sp_rev` on every
SP-scoped mutator — every create, delete, update, append, finish, cancel
and abort under a pool, `GrowSlice` and `ForceStartGroup`. Reads carry
none; an operator reads the tokens with `dn get`, `cn get` and `sp get`.
dnvctl sends exactly what was typed:

* `--rev` not given: the token field is absent, a nil message.
* `--rev` with a value, written in decimal or in hex after `0x`
  (Conventions, Value types): the token message is present with `revision`
  set to it and nothing else set (Conventions: the echo field is never
  sent).
* `--rev 0`: the message is present with revision zero — proto3 message
  presence keeps this distinguishable from omission — which is the
  always-stale probe of `gateway.md` GW6.
* "Given" means typed on this command line (pflag's Changed bit), nothing
  else: `--rev` has no environment or config carrier (CT9), so an exported
  `DNVCTL_REV` or a `rev` key in the `--config` file sends no token. On a
  token carrier a typed `--rev` must parse, an empty value included: a
  `--rev` fed an unset shell variable is a usage error (exit 2), not an
  ungated write.
* On every other command — the reads, and the writes that carry no token
  either, `cluster create`, `cluster delete`, `cluster advance-ids`,
  `dn create`, `cn create` and `sp create` — a typed `--rev` is a usage
  error (exit 2, no RPC issued), whatever its value. No field could carry
  it, so sending the request anyway would drop it unseen: a `cluster delete`
  typed with `--rev` would delete ungated while looking gated.

The check itself — made only when the token message is present — is
`gateway.md` GW6; a token-less mutator therefore succeeds ungated.

## The command tree

### Conventions

**Groups and leaves.** The tree is noun-grouped, "dnvctl <group> <verb>",
one group per resource — `cluster`, `dn`, `cn`, `sp`, `cntlr`, `td`, `ss`,
`ns`, `clone`, `xfer`, `migr`, `repair`, `retired-leg`, `group` and
`worker` — and
it reuses gatewayctl's verb and flag vocabulary. The leaves are the RPCs of
`service Gateway`, one leaf per RPC and nothing else (CT1): no convenience
verbs, no compound commands, and cobra's stock `help` and `completion` are
the only subcommands that are not RPCs. The RPCs without a group of their
own are homed in a group: `GrowSlice`, `InspectSide`, `FindStoragePoolNames`
and `UpdateStoragePoolCntlidSlotList` under `sp`, `GetThinDeviceBitmap`
under `td` and `AppendCloneBitmap` under `clone`. A group issues no RPC: typed without a
verb, or with a verb it lacks, it is a usage error (CT5), never a help
screen on stdout, unless `--help` is typed.

**Verbs.** An object's create, delete, get and list RPCs are its `create`,
`delete`, `get` and `list` leaves. An update RPC is a `set-` leaf named
after what it sets (`dn set-disabled`, `sp set-cntlid-slots`, `ns set-dev`,
`clone set-tr`), and it sends the value it is given, never a toggle:
re-sending a node's disabled flag or a cntlr's enabled flag as it is stored
is the gateway's idempotent no-write (`gateway.md` GW6), not a flip back, so
the leaf is safe to script. An inspect RPC is an `inspect` leaf
(`sp inspect-side` for a side), a read the gateway answers from the agent
that hosts the object, out of that agent's latest observation of it, not
from etcd, and `sp inspect` from the agent of the pool's primary beside the
pool's records; `dn inspect` and `cntlr inspect` add the block counters the agent
reads at the call (`dnagent.md` DN16, `cnagent.md` CN23), and
`group inspect` is the one whose agent reads the group's members then and
there, beside the records' view (`architecture.md`, Repairs). The bitmap
RPCs are `get-bm` and `append-bm`; every other leaf keeps its RPC's verb
(`cluster advance-ids`, `cluster get-capacity`, `sp find-names`,
`sp grow-slice`, `migr finish`, `migr cancel`, `group force-start`), save
one: `repair abort` is `TryToAbortRepair`, the one leaf whose verb is not
its RPC's (`repair` — `ctl/repair.go`).

**Fields and flags.** Every request field maps to exactly one flag, with
these exceptions:

* `cluster_name` and `sp_name` come from the scope global flags `--cluster` and
  `--sp`, `sp create` included, except that the `cluster` group's own
  commands name their cluster with `--name`, which falls back to the global
  `--cluster` when it is empty (`clusterNameOf`);
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

A leaf has a flag only where its request has a field, save
`cluster get-status`'s `--json`, which chooses how the reply is printed
(`cluster` — `ctl/cluster.go`). An
optional sub-message follows the "not given" convention: it is sent only
when one of its flags holds a non-zero value, a zero meaning the member was
not given, except for the token messages (CT3) and where a group below says
otherwise. Each leaf's flags,
with their types and defaults, are the code's, and `--help` lists them.

**Identity flags.** Each group names its object with identity flags: `cluster` with `--name`; `dn` and `cn` with
`--addr`, because a node's name is its ip:port; `sp` with the global `--sp`;
`cntlr` with `--id`; `td`, `clone`, `xfer`, `migr` and `repair` with
`--name`; `ss` with `--nqn`; `ns` with `--nqn` and `--idx`; `retired-leg`
with `--leg`; `group` with `--grp`; `worker` with none, since its one leaf
reads the whole registry. `sp inspect-side` takes its side id as
`--id`, deliberately the same spelling as the `cntlr` group's, as in
gatewayctl: the RPC, not the flag, says what the id means.

**Value types.** Every whole number that fills a request field — a flag
declared as a pflag number, an id, an item of a numeric list, `--rev` and the
number form of `--level` — is written in decimal, or in hex after `0x`
(either case, zeros after it allowed), and in no other form: a leading zero
without the prefix (0 itself is decimal), `0o`, `0b`, an underscore and a
sign fail to parse, a usage error, save one minus before a `--level` number,
whose field is a signed enum (`sp` — `ctl/sp.go`). Go's base-0 parse, pflag's
too, reads a leading zero as octal and takes the other forms; an id copied
from a device name or an NQN, which spell ids as zero-padded hex digits with
no prefix (`architecture.md`, Naming), would then name another object with no
error. dnvctl's own JSON prints ids in decimal, so an id is typed as printed,
or with `0x` before the hex of a name. A padded id starts with a zero until
an id needs all sixteen hex digits, which only many id steps
(`AdvanceClusterIds`, `architecture.md`, Clusters) could bring about, so its
copy without `0x` is refused; a hex id typed by hand without its zeros and
without `0x` still parses as decimal where its digits allow, which no rule
can tell apart. `dnv-agent --capacity` refuses a leading zero too, since Go
reads it as octal, and refuses `0x` as well (`dnagent.md` CM3); dnvctl keeps
`0x` because an id has a hex source, a device name or an NQN, and a size has
none. `--timeout` is a deadline, not a request field, and keeps Go's float
parse (CT9).

A number that fills a request field is parsed at the width of that field:
every value the field holds parses, and a value past either end of the
field fails to parse, a usage error. A wider parse would cut such a value
and send a number nobody typed; a narrower one would refuse values the
gateway should judge, the client-side check CT8 forbids. An id — `--id`,
`--slice`, `--leg`, `--src-side`, `--grp`, `--retired-leg` — is declared as
a string and parsed by dnvctl. An id flag left out sends zero, for the
gateway to judge (CT8); one typed with an empty value, or with spaces
alone, is a usage error, as a typed empty `--rev` is (CT3): an unset shell
variable must not send zero, which `repair create` reads as a fresh leg.
`--slots`, `--ids` and `--bad-legs` are comma-separated numeric lists;
`--hosts` and the selector flags are comma-separated string lists;
`--override` is a comma-separated list of words (`group` —
`ctl/group.go`). An empty list flag is the empty list. Any other value is
split at each comma: a string list sends every item as typed, spaces and
empty items included, and a numeric list trims each item and parses it by
the rule above, so an empty item is a usage error. No item is dropped,
because a comma list whose items are all unset shell variables, such as
`$A,$B`, would otherwise shrink to nothing and revoke every host of a
subsystem (`architecture.md`, Primary cntlr) or drop a white list
(`architecture.md`, Finding DN candidates), while the gateway refuses an
empty host NQN or node address (`architecture.md`, Common validation). A
list flag replaces its value on each occurrence rather than accumulating,
which is why the lists are plain string flags: pflag's own slice flags
append. A boolean that must be turned off is written `--enabled=false`,
never `--enabled false`, whose `false` would be a positional argument,
which no leaf accepts.

**Defaults.** dnvctl keeps a leaf-flag default of its own only where a spec
names one: `sp create`'s redundancy (`sp` — `ctl/sp.go`), the transport flags
wherever they appear, and the value flags of `sp set-level`,
`cntlr set-enabled`, `ns set-suspended` and `group set-operator-in-charge`.
A flag default is not a hidden RPC (CT8).

**Shared flag helpers.** Each has one implementation, in `ctl/root.go`, and
`--help` lists the flags it adds:

* `trConfFlags` adds the four flags of an `NvmeTrConf` under a prefix: none
  on `dn create` and `cn create`, `src-` for a clone's source. They carry a
  default of their own (Defaults), and all four emptied make a nil conf
  (`trConfOf`).
* `selectorFlags` adds the black and white lists of a `NodeSelector` under a
  node prefix; both empty make a nil selector (`selectorOf`).
* `dmCloneConfFlags` adds the two dm-clone tuning flags; both zero make a
  nil `DmCloneConf` (`dmCloneConfOf`), the "not given" convention of
  `gateway.md` GW11.
* `pageFlags` adds the page size, where zero asks for the server's default,
  and the page token to the paged list leaves.

CT8. **No client-side validation.** dnvctl rejects only what fails to parse
(exit 2) — a malformed `--bm-hex`, a non-numeric id, a number written with a
leading zero or in any form but decimal and hex after `0x`, an id typed empty
(Value types), a `--timeout` that is not a finite number of seconds (CT9) —
plus a `--rev` on a command whose request has no token field to carry it
(CT3). Every parsed value is sent as typed, and the gateway's validation
(`architecture.md`, Common validation) answers: empty required fields,
contradictory flags and unknown enum numbers are all forwarded, save
through `--override`, which takes words only (`group` — `ctl/group.go`).
Nor does dnvctl issue an RPC the operator did not type: no token
auto-fetch, no pre-read behind a warning (`cntlr` — `ctl/cntlr.go`), no
poller behind a latching delete (`sp` — `ctl/sp.go`).

CT1. **Completeness.** The leaves are exhaustive: one per RPC of
`service Gateway`, none left out and none doubled. A leaf carries the
`--rev` token exactly when its request has a token field, so the leaves that
take `--rev` are exactly CT3's carriers, and `--rev` on any other leaf is a usage
error. Both claims are pinned by the unit tests against
`pb.Gateway_ServiceDesc` and the request types, the refusal on every leaf with
no token field; on the wire the integration suite makes the first claim, sends
`--rev` on every carrier and types it on one leaf with no token field
(Integration test plan).

### `cluster` — `ctl/cluster.go`

`cluster list` and `worker list` (`worker` — `ctl/worker.go`) are the two
commands whose request has no `cluster_name`: they ignore both scope
global flags.

Three `cluster create` flags reach a `ClusterConf` member:
`--md-check-days`, `--start-hold-seconds` (both below) and `--extent-size`,
which sets `dn_bin_conf.extent_size`, the DN
and CN allocation unit (`architecture.md`, Size → extents), which
`DefaultDnExtSize` fixes otherwise. A pool of `MaxSliceCntPerSp` slices
takes a meta group and a data group per slice, each with one side per
leg, so on lab-sized backing devices
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
set that changes nothing or one the gateway refuses. The bounds on a non-zero
size, and its unit, a whole number of `DnExtSizeUnit`, are the gateway's
(`architecture.md`, Common validation): dnvctl range-checks nothing (CT8),
and no end applies a power-of-two rule. The command line is the flag's only carrier, like every
leaf flag's (CT9), which matters most here because the value is write-once:
`DNVCTL_EXTENT_SIZE` and an `extent-size` key in the `--config` file reach
no request, so no stale variable sizes every cluster created under it, and
text that is not a uint64 is a pflag parse error (exit 2, no RPC).

`--md-check-days` does not follow the "not given" convention: it
sends an `md_check_conf` carrying `interval_days` whenever it is typed, zero
included, and none when it is not. Reason: a zero turns the md check off,
while a request without the message takes the gateway's default
(`architecture.md`, Common validation and The md check), so the zero must
reach the gateway. dnvctl checks no bound on it (CT8).

`--start-hold-seconds` follows `--md-check-days`'s rule: it sends a
`start_hold_conf` carrying `seconds` whenever it is typed, zero included,
and none when it is not. Reason: a zero turns off the worker's hold of an
agent that answers that it is starting, while a request without the
message takes the gateway's default (`architecture.md`, Common
validation; `dnv-worker.md` HL8). dnvctl checks no bound on it (CT8).

No other `cluster create` flag reaches the request's other conf members.
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

`cluster advance-ids` is the id step of `architecture.md`, etcd backup and
restore. It names its cluster with `--name`, as `cluster create`,
`cluster delete` and `cluster get` do, and carries no token. Its RPC runs
one transaction per pool of the cluster (`architecture.md`, Clusters), so on
a cluster of many pools it needs a `--timeout` that covers them all.

`cluster get-capacity` is the capacity read of `architecture.md`, Clusters:
every node's recorded extent counts and flags, and what pools of one shape
can still place, with the limit the cluster reaches first. `--leg-cnt`,
`--cntlr-cnt` and `--slice-cnt` name the shape, each sent as typed, so a
zero takes the gateway's default as on `sp create`. It names its cluster
with `--name`, falling back to the global `--cluster`, and carries no
token, as every read does (CT3).

`cluster get-status` is the status read of `architecture.md`, Clusters:
what is wrong in the cluster, a page of pools at a time. It names its
cluster as `cluster get-capacity` does, and takes the page flags, since its
pages are `sp list`'s. It prints a table by default, for a person in an
incident, and the CT4 document with `--json`, for a script: the header
`POOL OBJECT STATUS AGE TEXT`, then one line per row — the row's pool, "-"
for a cluster row; its object; its kind, and its status and its reason
where it has them, each as its lower-case short name, as `row/error` or
`lost-record`; its age to the second, "-" where none is known; and its
text, a newline or a tab in it shown as " | " so the row stays one line —
then a line that counts the pools the page read, and `next page:
--page-token TOKEN` when the reply carries a token. A page with no row is
one line that says nothing is wrong in the pools it read. The ids and names
a row
carries for a script stand in its object; `--json` gives them as fields,
with the read's time and the cluster's name. The table goes through
`render` and `writeResult` as every result does (CT4), so a write that
fails is exit 3 (CT5). One invocation is one RPC (CT8): dnvctl never walks
the pages itself. Reason for the default: the read exists for a person in
an incident, who reads a table faster than a document, while a script
names `--json`. The unit tests' walks of every number flag run the leaf
with `--json`, so they read its document as every other leaf's.

### `dn` — `ctl/dn.go`

`dn create` reaches the agent: the gateway calls the DN agent's `GetDnSize`
inline (`gateway.md`, Agent calls), so the invocation's trace id chains
through to the agent's records (`grpc.md` T3). Its `--location` is empty
unless given, which lets the gateway take the host name the agent reports
(`architecture.md`, Disk nodes): every disk node of one machine created
without it is one location, so a RAID1 setup on one machine names one per
disk node. `dn inspect` is the group's
read of the DN agent's latest observation of the node, through its
`GetDnInfo`, rather than of etcd, so its disk counts are the agent's own,
to set beside the record's counts that `dn get` shows (`architecture.md`,
Live-state reporting); beside them it prints the disk's block counters,
which the agent reads at the call (`dnagent.md` DN16).

`dn delete` reaches the agent too, unless `--force` is typed: the gateway
asks the node's agent, through `GetDnInfo`, to prove the node clean, and
refuses the delete until it does (`architecture.md`, Disk nodes), so an
operator runs it again until it passes and reads a refusal's text for what
the agent still holds. `--force` sends `force` true, which skips the proof:
the node goes with what it still holds. A node that still hosts a side is
refused with `--force` too.

### `cn` — `ctl/cn.go`

The `cn` group mirrors `dn` exactly against the controller-node RPCs: the
same verbs and the same flags, the requests differing only in carrying
`cn_rev` where the `dn` ones carry `dn_rev`, `cn create` reaching the CN
agent's `GetCnSize` as `dn create` reaches `GetDnSize`, and `cn delete`
reaching its `GetCnInfo`, unless `--force` is typed, as `dn delete` reaches
`GetDnInfo`. A change to one group is a change to both. The one exception
is `cn check-restore`, which has no `dn` twin: the restore check of
`architecture.md`, Controller nodes, run after an etcd restore for the
node at `--addr`, which the restored state need not name. It is a read and
carries no token. The gateway reads each pool the node holds, so a node of
many pools needs a `--timeout` that covers them all.

### `sp` — `ctl/sp.go`

`sp create` always sends a `bdev_conf`, because it always sends a
`redund_conf`: `--redund` picks the arm and defaults to the md-raid1 one, as
`architecture.md`, Storage pools, says the CLI does. The raid1 arm carries
`--bitmap-chunk-blocks`, zero leaving the control plane its own chunk size,
and `none` sends `redund_none`. An unknown `--redund` spelling is a parse
failure (exit 2) rather than a forwarded value, because the flag names a
oneof arm and the oneof has no third arm to carry it. The other members
follow the "not given" convention: `dm_raid0_conf` is sent only for a
non-zero `--stripe-size`, `dm_pool_conf` only when `--block-size` or
`--low-water-mark-pct` is non-zero, carrying both, and `event_threshold`
only when one of `--thr-primary`, `--thr-cntlr`, `--thr-side`, `--thr-leg`
and `--leg-fast-io-fail`, the legs' fast IO fail timeout, is non-zero.
`--slice-cnt`, `--cntlr-cnt`, `--init-ext-cnt` and
`--low-water-mark-pct` are forwarded as typed, a zero and a mark above one
hundred included (CT8); which zeros the gateway fills with a default and
which it refuses is `architecture.md`, Storage pools and `architecture.md`,
Common validation. With no `--stripe-size` the pool takes the cluster's
stripe, narrowed where its product with the slice count is over the bound
of `architecture.md`, Common validation, the stripe's product, so
`cluster get` can show a larger stripe than `sp get` does. A
`--block-size` that this stripe does not divide needs a `--stripe-size`
with it: the gateway refuses the create otherwise, naming the way out. A
`--stripe-size` whose product with the slice count is over that bound is
refused; leaving the flag out takes the narrowed stripe instead. dnvctl
checks neither refusal (CT8).

`sp delete` latches and returns, so a successful `sp delete` means teardown
started, not gone; the drain, and what the pool answers while it runs, are
`architecture.md`, Storage pools, and every other mutator under the pool is
refused while it drains (`architecture.md`, `service Gateway` — RPC
specifications). dnvctl adds no wait flag: the polls would
be RPCs the operator did not type (CT8), and the surface stays one command
per RPC (CT1).

`sp set-cntlid-slots` sends an empty `--slots` as typed, and the gateway
refuses it, while an empty list on `sp create` asks for the gateway's
default (`architecture.md`, Storage pools). `sp set-level` takes a short
level name, an `SP_LEVEL_*` name or a raw number — the number so that an
undeclared enum value is forwarded for the gateway to refuse (CT8) — and
`--level` defaults to read-write, the enum's zero. `sp grow-slice`'s
`--meta` and `--ext` do not constrain each other client-side (CT8).
`sp inspect` takes no flag of its own: it reads the pool `--sp` names, its
data space per slice from the records and its thin pools' use from the
primary's report, with the overcommit as a whole percent
(`architecture.md`, Storage pools, InspectStoragePool).

### `cntlr` — `ctl/cntlr.go`

`cntlr set-enabled` takes `--enabled`, which defaults to true, so disabling
a cntlr is written `--enabled=false`. Disabling the last enabled cntlr of a
pool stops its IO; the gateway allows it (`architecture.md`, Cntlrs), and
dnvctl does not warn: the warning would need a pre-read of the pool's
cntlrs, an RPC the operator did not type (CT8). `cntlr delete` has
preconditions that are server state (`architecture.md`, Cntlrs); dnvctl
checks none of them (CT8). `cntlr inspect` prints, beside the cntlr's rows,
the `io_stats` the agent read at the call: the raw block counters of each
leg that has a wrapper and of each namespace device that serves IO, and, on
the primary, each probed leg's last completed probe round's times
(`cnagent.md` CN11, CN23). The counters count from when the device appeared,
so a slow leg shows as the difference of two inspects a few seconds apart
(`operations.md`, A slow leg).

### `td` — `ctl/td.go`

`td create` with `--ori` takes a snapshot of that thin device, and `--size 0`
is legal only then, the snapshot inheriting its origin's size;
dnvctl does not enforce the pairing (CT8). `td list` is the `created` poll,
the client's wait primitive of `architecture.md`, Thin devices: CT4's
rendering shows `created` and `torn` on every thin device, false included,
and that same place says when a token read before the poll is stale and
that a `torn` snapshot is one to delete and re-create.
`td get-bm` prints the hex map of CT4 and names its device with the
group's `--name`, where gatewayctl spells it `--td`. A leg's bitmap has no
leaf: the worker reads it itself (`dnv-worker.md` BM7).

### `ss` — `ctl/ss.go`

`ss list` is the only etcd read that shows a namespace's configuration: a
namespace is a field of its subsystem, so the `ns` group creates and changes
namespaces but has no read of its own; `cntlr inspect` shows a cntlr's
namespaces as its agent last observed them, not their configuration.
`ss set-hosts` sends the `--hosts` list as a full replacement of
`allowed_hosts`, an empty list included, and `ss create` without `--hosts`
sends an empty list too; what an empty list admits is `architecture.md`,
Primary cntlr.

### `ns` — `ctl/ns.go`

`ns create` sends `--uuid` and `--nguid` as typed, an empty value leaving
the gateway to mint that identity (`architecture.md`, Subsystems,
namespaces); dnvctl generates nothing itself. `ns set-suspended` takes
`--suspended`, which defaults to true, so resuming is written
`--suspended=false`; a re-sent matching flag is a write and a bump here,
unlike the flag updates `gateway.md` GW6 makes a no-op.

### `clone` — `ctl/clone.go`

`clone create` and `clone set-tr` send the source transport as a one-element
`src_tr_conf`, and as an empty list — never a default transport — when all
four `src-` transport flags are emptied, so the gateway's refusal of an
empty list stays reachable from the CLI (`cloneSrcTrConfList`).
`clone create` forwards `--src-slices`, `--src-stripe` and `--src-block` as
typed (CT8); a source whose geometry and the destination pool's would give
the clone's dm-clone an optimal IO size over `MaxNvmeTcpCmdBytes` is
refused `FAILED_PRECONDITION` with both products named (`architecture.md`,
Clones). No flag changes that, since the flags must describe the source as
it is: the way out is a destination pool whose geometry keeps that size
within the bound, and for a source stored with a stripe times slice count
over the bound there is not always one (`architecture.md`, Clones; Known
limits, A pool's IO hints are held to what a controller node takes, at a
price).

`clone delete` with `--force` sends `force` true, which skips the gateway's
proof that the copy finished; the RPC latches and returns, so a successful
`clone delete` means teardown started, not gone. A clone whose copy is
whole leaves by itself: the worker latches it (`dnv-worker.md` CLD13), so
`clone delete` then answers OK with no write — `ABORTED` for a `--rev`
read before the latch, whose bump makes it stale — or `NOT_FOUND` once the
drain is done. What the clone, its
destination thin device and the pool answer while the clone drains, and
the order in which an operator then takes them down, are `architecture.md`,
Clones. dnvctl adds no wait flag here either (CT8, CT1).

`clone append-bm` addresses a chunk by the pair `--src-slice-idx` and
`--bm-idx`, never by either alone; what a chunk is and how its pages append
is `architecture.md`, Clones. `--offset` names a page's position in its
chunk and is always sent, 0, the chunk's start, when it is not typed, so
dnvctl never sends a page without its position: a page sent again changes
nothing, and a later page sent with no `--offset` is refused by the
gateway, or taken with no write when its bytes equal those at the chunk's
start, but never appended where the chunk ends; dnvctl checks nothing
itself (CT8), and what the gateway does with each is `architecture.md`,
Clones. An empty `--bm-hex` sends an empty bitmap on
purpose, so the gateway's refusal of one stays reachable; a malformed
non-empty value is a usage error (exit 2). The leaf takes the bitmap as
one hex string in `--bm-hex`, with no file or stdin input, so the kernel's
bound on the length of one argument bounds one append to half that many
bytes of bitmap, and a larger chunk is appended in pages, each read over a
multiple of 8 blocks but the slice's last, which the gateway enforces
on the read (`architecture.md`, Bitmap reads).

### `xfer` — `ctl/xfer.go`

`xfer delete` with `--force` sends `force` true, which the gateway reads as
an abort rather than the finalizing of a hand-over (`architecture.md`,
Transfers). `xfer set-hosts` sends the `--hosts` list as a full replacement
of `allowed_hosts`.

### `migr` — `ctl/migr.go`

No leaf carries a migration's bitmap, unlike a clone's: the worker reads
the leg's bitmap and hands it to the new side itself (`dnv-worker.md` BM7).

`migr finish` with `--force` sends `force` true, which skips the gateway's
proof that the copy finished and nothing else: a migration whose cutover
has not started is refused with `--force` or without, and `migr cancel` is
the way out (`architecture.md`, Migrations). `migr cancel` with `--force`
sends `force` true, the only way to cancel once the cutover has started:
the old side then serves its content as of the cutover, and what that
costs, the leg's repair in a group of more than one leg included, is
`architecture.md`, Migrations. `migr get` prints the record
as stored, its `cutover_pending` included (CT4), which tells whether a
finish can succeed yet.

### `repair` — `ctl/repair.go`

`repair abort`, the one exception to the verb rule (Conventions), sends
`TryToAbortRepair`, which only sets the repair's abort flag, and the
primary's answer moves the repair to ABORTED, or to STARTED when it had
already added the new leg (`architecture.md`, Repair), so the leaf keeps
the operator's short verb and `repair get` shows the outcome.
`repair create` sends `--bad-legs` as the list it is, so a count other than
one reaches the gateway, which refuses it (CT8), and its new leg is the
retired leg `--retired-leg` names or, when that is zero or not given, a
fresh leg the `--dn-black` and `--dn-white` selector steers; `--force` lets
the primary copy over a leg that carries another md identity, when the
retired leg taken back or the bad leg after an abort does
(`architecture.md`, Repair), and its help says that what such a leg holds
is lost. `--force-undecided` is another flag, never `--force`: it sends
`force_undecided`, which lets the gateway take a bad leg the primary's
report marks of an undecided md identity, the operator naming it the stale
copy, and which the repair does not keep, so an abort leaves that leg as it
is unless `--force` was typed too (`architecture.md`, Repairs, A leg of
another identity). dnvctl checks no stage: a call the repair's stage does
not allow is the gateway's refusal, which names the stage
(`architecture.md`, Repairs). `repair finish` prints the reply as it is,
`given_back_leg_id` included when it is zero (CT4), so the operator always
sees whether the finish of a worker's repair gave a retired leg back
(`architecture.md`, Repairs). `repair list` is not paged.

### `retired-leg` — `ctl/retiredleg.go`

`retired-leg delete` names the retired leg by its own leg id, `--leg`; a
retired leg is taken back by `repair create --retired-leg` rather than by
a leaf of this group (`architecture.md`, Retired legs). `retired-leg list`
is not paged, and prints each leg's `keep` flag and its `partial` mark. `retired-leg keep` sends
`UpdateRetiredLegKeep` with `keep` true, which keeps the leg from the sp
worker's retire timer and from the give-back at the retired-leg bound,
and with `--clear` sends it false (`architecture.md`, Retired legs); it
sends the value typed, never a toggle, and a `retired-leg delete` still
deletes a kept leg.

### `group` — `ctl/group.go`

`group force-start` sends `ForceStartGroup`, the forced start of a raid1
group's array (`architecture.md`, Repairs). It names the group by `--grp`
alone, with no flag for the slice or clone that holds it, because the
gateway finds the group's parent itself, and the leg the array starts from
by `--leg`. With `--cancel` it withdraws the group's mark instead and names
no leg. `--override` names the refusals the start passes, a comma list of
words each sent as its kind in `override_list`, in the order typed:
`older-leg`, so that the start goes on from a named leg whose change count
is below that of a full member of the same array, which it overwrites;
`bad-blocks`, from a named leg that carries a bad-block list, which it
drops (`architecture.md`, Repair, Forced start); and `partial`, so that
the mark may name a taken-back leg that carries the partial mark
(`architecture.md`, Retired legs). A start passes a refusal only when its
word is named, which the leaf's help says. An item that is no word, an
empty one included, is a usage error (exit 2, CT5): `--override` has words
only and no number form, so a kind the enum does not declare cannot be
typed here and reaches the gateway's check of the enum only from another
client; there is no `--force`. dnvctl checks none of the pairings: a
non-zero `--leg` or an `--override` beside `--cancel`, a word typed twice,
and a write with no `--leg`, reach the gateway, which refuses them (CT8).
The forced start overwrites the other members it keeps, the open repair's
bad leg excepted, with a full copy of the named leg (`architecture.md`,
Repair, Forced start), and a start from an older copy restores that group
alone, not the pool (`architecture.md`, Known limits). dnvctl asks for no
confirmation: the leaf's name and its help say what it destroys, as a
delete leaf's do, and the help says what a start from an older copy
restores. The mark is a field of the group record, so `sp get`
shows a slice group's mark as stored and `clone get` a clone meta group's
(CT4); what ends a mark is `architecture.md`, Repair.

`group inspect` sends `InspectGroup`, a read with no token: the group's
primary reads each member the group keeps, its md superblock and md's view
of it, and the reply adds each leg's row and health (`architecture.md`,
Repairs). With `--leg` it is the dry run of `group force-start` from that
leg, `--override` as that leaf's: the reply says what the start would do,
or the refusal it would meet, and lists every refusal an override passes
that the start's checks met, each with whether the request passes it;
nothing runs. The dry run is this leaf and
not a `--dry-run` of `group force-start`, since a leaf sends one RPC
(CT1). dnvctl checks nothing but the words of `--override` (CT8); an
`--override` with no `--leg` reaches the gateway, which refuses it.

`group set-operator-in-charge` sends `UpdateGroupOperatorInCharge`, which
sets or clears the group's operator-in-charge flag, the operator's take-over
of the group's repair from the sp worker (`architecture.md`, Repairs). It
names the group by `--grp` alone, as `group force-start` does.
`--operator-in-charge` defaults to true, so the leaf typed without it takes
the group over and giving the group back is written
`--operator-in-charge=false`: the take-over is the deliberate act and the
give-back its undo, the reverse of `cntlr set-enabled`, whose default true
is the undo. The leaf sends the value given, never a toggle (Conventions).
The flag is a field of the group record too, so `sp get` shows a slice
group's flag and `clone get` a clone meta group's, as they show the mark
(CT4); what the flag stops, and why its write bumps the pool's revision,
so that a `--rev` token given before it is stale after it, are
`architecture.md`, Repairs.

### `worker` — `ctl/worker.go`

`worker list` sends `ListWorkers`, the read of the worker registry, and
takes no flag: its request has no field, so it ignores both scope global
flags, and the list is not paged. It prints the reply as it is (CT4): every
role, an empty one included, and under each role every registration by
seed, with the `epoch` its worker last wrote by its own clock. The listing
is the registrations as stored, not any worker's effective membership, and
a dead worker's key stays in it until a peer deletes it; what it shows and
what it does not is `architecture.md`, Worker registry.

## Integration test plan

**What the Go cases prove.** The unit tests of `ctl/` run the real command
tree through `Execute` inside the test process, with no etcd and no gateway.
The package's dial hook, `dialer`, hands an invocation `recordingClient`, a
stand-in for the gateway that records the one request a command line
produces and answers it from the test, with an injected status, an injected
reply, or a verdict it takes from the request itself, as the gateway's
validation and paging would. Against it they prove the field and flag rules
of every leaf, the tokens of CT3, the rendering of CT4, the status read's
table cell by cell, with every field of its reply either shown or named as
left out, the exit codes and
failure lines of CT5, exit 3 on a stdout that refuses the write and on a
reply that cannot be rendered among them, and the binding of CT9, and CT8
at the limits of the count, size, name and NQN flags. A number its flag
cannot hold does not parse, so it is a usage error that issues no RPC (CT5,
CT8). Walks of the command tree find every whole-number flag and hold each
to the spelling of Conventions, Value types, and each one but the numeric
lists to the refusal of a typed empty value. The ranges the gateway sets on
a count or a size and the lengths it allows a name or an NQN
(`architecture.md`, Common validation) are never checked by dnvctl: the
value goes out as typed and the gateway's verdict comes back, a full page
rendered whole with its page token reaching the next request verbatim, a
refusal rendered as the failure line with exit 1. The stand-in
judges against the constants of `common` with refusal texts of its own, so
the gateway's wording stays the gateway's tests' to pin. Other dials the
tests hand out record the address, the deadline and the close of the
connection, fail, for the connection half of CT5, or reach a real gRPC
server behind the server chain over an in-memory connection, for the trace
id and the deadline of CT2; the production dial itself carries the trace id
to such a server over loopback.

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

**Cases.** The cases run in a fixed order and fail fast, each against a
restarted fake whose behavior file is reset and whose state file holds only
the reset's own readiness probe, so counters are read only as deltas. Together
they exercise, on the wire, CT1 with one step per RPC in tree order, save
`sp inspect`, `cluster get-status` and `cn check-restore`, which run last,
in that order, so that the steps before them keep their numbers, CT2's
trace id and deadline, CT3's token on exactly its carriers and its refusal on
one leaf that carries none, CT4's goldens, the status read's table among
them, CT5's failure line and codes, exit
3 on a stdout that refuses the write among them, CT7's empty stderr, CT8's
parse-only refusals, a number with a leading zero on an id flag and on a pflag
number, an id typed empty, and a word `--override` does not know and a
`--force` on `group force-start`, which has no such flag, among them, CT9's
precedence of flag over
environment, and the field and flag rules of Conventions. What one step per
RPC has no room for is left to the unit tests: the transport conf's nil rule
and a clone's empty source list, a bare `cluster create` sending no
`dn_bin_conf`, no `md_check_conf` and no `start_hold_conf`, a typed
`--md-check-days` or `--start-hold-seconds` sent as typed (its zero included),
the silence of the environment and the config file on leaf flags (CT9),
`--timeout` refused or clamped from every carrier, the code name of every gRPC
code (CT5), and the `--rev` refusal on the leaves with no token field other
than the one the suite types it on, which the unit tests pin on every such
leaf (CT1).

Out of scope: real-gateway runs, which are the gateway suite's harness;
concurrency, which stays with gatewayctl's barrier runner; performance; and
any assertion about the gateway's reaction to what dnvctl sends.

**What a pass means.** Exit status zero and `PASS` mean every case's
assertions held, in a run that stops at the first failure. A request is
asserted as a whole object, the fake's recorded request against the
argv-implied one, so an unexpected key fails as loudly as a wrong value —
which is what makes "no token key" provable rather than assumed. The fake
records a request without its unpopulated fields, so an absent `--rev` is an
absent key and `--rev 0` an empty object, and uint64 fields compare as
strings, protojson's doing. A count is a delta between two state snapshots
taken in the same round trip as the call, so nothing can slip in between; a
usage error passes only with the count unmoved, and exit 3 only with it moved
by one. Every success but the status read's table parses stdout, and the
goldens, that table's among them, compare byte-exact. The token
assertions are request-side — what dnvctl put on the wire — so they hold
whatever a gateway does with the token. Waits are bounded polls for process
start and exit only: every dnvctl call is synchronous. The fake's log is read
through a filter that drops a line torn by a concurrent append.

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
it installs no logger of its own, sets its log level to Debug as the fakes do,
and prints nothing on stdout. It mirrors
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

