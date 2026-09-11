# dnvctl.md — the dnv operator CLI (`ctl/` + `cmd/dnvctl`) and its integration test

Status: implementation-ready spec, written 2026-09-11 from the design interview;
nothing under `ctl/` or `cmd/dnvctl/` exists yet. This is the document
`gateway.md` §1 item 1 promised, and it implements the last step of layout.md's
build-out order. Division of authority: architecture.md §8 keeps RPC semantics
and validation; grpc.md keeps interceptor/trace rules; log.md keeps logging
rules; this document owns the CLI mapping, the `ctl/` package, `cmd/dnvctl`,
`integtest/fakegateway` and `integtest/dnvctl_test.sh`. Normative rules here are
tagged `CT1`…; test obligations `CT-T1`….

Sources and precedents: `integtest/gatewayctl/` (the proven flag vocabulary and
emit pipeline this spec deliberately reuses), `integtest/fakeagent/` (the fake's
behavior/state/log contract), `integtest/gateway_test.sh` (suite boilerplate).

## 0. Decision record

Numbered for citation as "§0 #n". All decided in the 2026-09-11 interview.

1. **One document.** dnvctl and its integration test share this file (the
   `gateway.md` precedent), instead of the `cnagent.md` + `cnagent_integtest.md`
   pairing.
2. **Spec-first.** The doc is written before the code and is what the code is
   reviewed against.
3. **Surface = exactly the 59 `service Gateway` RPCs.** One command per RPC,
   nothing else — no copier (`architecture.md` §11.4's userspace copier is
   future work *outside* dnvctl; §8 amends the references), no convenience
   verbs, no compound commands. Cobra's stock `help`/`completion` are the only
   non-RPC subcommands.
4. **Noun-grouped tree.** `dnvctl <group> <verb>`, one group per resource
   (§5), reusing gatewayctl's verb and flag vocabulary. Odd RPCs are homed:
   `sp` ← GrowSlice, InspectSide, FindStoragePoolNames,
   UpdateStoragePoolCntlidSlotList; `td` ← GetThinDeviceBitmap, GetLegBitmap;
   `clone`/`migr` ← their Append*Bitmap.
5. **All-flags leaves; two scope globals.** Request fields map to flags 1:1;
   `--cluster` and `--sp` are persistent, env-backed globals because
   `cluster_name` appears in 58 requests and `sp_name` in 41.
6. **Single gateway address.** `--gateway-address` is required; no
   multi-gateway failover list in v1 (operators front the gateways themselves).
7. **Canonical protojson output** (gatewayctl's exact pipeline), bitmaps as
   hex maps; no `-o`/table mode in v1.
8. **Exit codes 0/1/2** and a one-line stderr error format carrying the gRPC
   code and the trace id; `--trace-id` overrides the per-invocation mint (T4).
9. **`--rev` is an optional pass-through** with message-presence semantics
   (§4). An *absent* token message bypasses the GW6 revision check; a present
   one is compared strictly. This was written as an assumption about a future
   gateway and is now simply the gateway's behavior: GW6 became
   presence-based on 2026-09-11 (gateway.md §0 #7 / GW6, architecture.md
   §5.5; `gateway/common.go`'s three `check*Token` helpers take the token
   *message* and compare only when it is non-nil). risks_and_gaps.md RK7
   records that window, now CLOSED; RK8 records what the bypass costs — a
   token-less mutator has no optimistic-concurrency gate. An explicit
   `--rev 0` keeps its meaning: a *present* zero token, and since revisions
   seed at 1 (`gateway/storagepool.go`) and only grow (`model/ops.go`
   `BumpSpRev`), a deliberate always-stale probe.
10. **No hidden RPCs, no client-side validation.** dnvctl never issues an RPC
    the operator did not type: no token auto-fetch, and no pre-read for the
    "disabling the last enabled cntlr" warning that `gateway/cntlr.go:383-392`
    and architecture.md §8.6 anticipated — that warning is deferred until the
    gateway itself carries the hint in a reply. dnvctl also does not
    second-guess values; the gateway's §7 validation is the only validator
    (CT8).
11. **One dnvctl-side default is kept:** `sp create` defaults the redundancy
    to `redund_md_raid1` (architecture.md §8.4 "dnvctl defaults it to
    `redund_md_raid1` on the CLI"). A flag default is not a hidden RPC.
12. **The integration test is dnvctl vs a fake gateway** on one VM, in the
    worker/gateway suite pattern: everything scp'd to the target, driven over
    ssh, **no sudo**, house flags `[--only <case>] [--cleanup-only] user@ip`.
13. **`integtest/fakegateway` mirrors `integtest/fakeagent`:** behavior.json
    (mtime+size reload, unknown fields rejected), state.json (atomic
    temp+rename), the mandatory §4 server interceptors so its JSON log is the
    assertion surface — but keyed per *method*, not per object, because the
    fake models no cluster state.
14. **Coverage: a 59-RPC sweep** asserting every request proto on the wire,
    plus targeted behavior/error/transport cases; reply-side checks are
    parse + spot-check everywhere with byte-exact goldens for a representative
    handful (§7.10).

## 1. Scope and placement

### 1.1 In scope / out of scope

In scope: the `ctl/` library package, the `cmd/dnvctl` wrapper, the
`integtest/fakegateway` binary, the `integtest/dnvctl_test.sh` suite, and the
§8 companion-document amendments.

Out of scope for v1 (each deliberate, §0 cited): the §11.4 userspace copier
(#3); TLS/auth — dnv is plaintext gRPC end to end (gateway.md); multi-gateway
failover (#6); table/human output modes (#7); token auto-fetch and the
last-enabled-cntlr warning (#10); any `BdevFeature` flags on `sp create`
(gatewayctl's `--feature-junk` was a driver-only validation poke and is not
ported); shell-completion helpers beyond cobra's stock generator.

### 1.2 Files

```
ctl/
├── root.go      # cobra root: globals, viper binding, dial, run, emit, errors
├── cluster.go   # §5.1     ├── ss.go     # §5.7
├── dn.go        # §5.2     ├── ns.go     # §5.8
├── cn.go        # §5.3     ├── clone.go  # §5.9
├── sp.go        # §5.4     ├── xfer.go   # §5.10
├── cntlr.go     # §5.5     ├── migr.go   # §5.11
├── td.go        # §5.6     └── spare.go  # §5.12
cmd/dnvctl/main.go           # thin wrapper (§1.3)
integtest/fakegateway/main.go  # §7.5 (+ main_test.go)
integtest/dnvctl_test.sh       # §7
```

One file per noun group, each declaring only `register<Group>(root)` plus its
job funcs, so the sibling files share no package-level flag names (the
gatewayctl lesson). `make build` picks `cmd/dnvctl` up automatically the moment
`main.go` lands (the Makefile globs `cmd/*/ *.go`).

### 1.3 Placement rules (all pre-existing, restated as CT rules)

* **CT6 — etcd-free.** `go list -deps ./cmd/dnvctl | grep etcd` finds nothing
  (layout.md §7 item 4, dependencies.md). dnvctl's only server-side dependency
  is `pb` + `common`.
* **CT7 — log level Warn, logs to stderr.** `cmd/dnvctl/main.go`'s first
  statement is `common.SetLogLevel(slog.LevelWarn)` (log.md R6, layout.md).
  `ctl/` installs `common.TraceIdHandler` over
  `slog.NewJSONHandler(os.Stderr, nil)` — stdout is reserved for command
  results (the grpc.md §6 driver rule, applied to the real CLI for the same
  reason). Because every §4 client-interceptor record is Info, dnvctl's own
  gRPC logging is silenced by design (log.md: "this is intended").
* **CT4 (part) — `fmt.Print*` only in the result-emit path** (log.md R1's
  explicit dnvctl exemption); everything else goes through slog.

## 2. Invocation model

### 2.1 Global flags, env, config

`dnvctl <group> <verb> [flags]`. Persistent flags on the root:

| flag | type | default | fills / does |
|---|---|---|---|
| `--gateway-address` | string | *(none — required)* | gateway `ip:port` to dial. No default: unlike the test driver there is no lab default worth baking in |
| `--cluster` | string | `""` | `cluster_name` of every request that has one (58 of 59) |
| `--sp` | string | `""` | `sp_name` of every request that has one (41 of 59) |
| `--rev` | hexUint (base-0) | *(absent)* | revision token, §4 — only meaningful on the 34 token-carrying mutators |
| `--timeout` | float64 | `10` | per-invocation deadline, seconds |
| `--trace-id` | string | `""` | override the T4 mint (§2.3) |
| `--config` | string | `""` | optional viper config file |

Env binding follows the four daemons' copy-pasted `bindViper` body verbatim
(**CT9**): `viper.BindPFlags` on the invoked command's full flag set (persistent
+ local) in the root's `PersistentPreRunE`, `SetEnvPrefix("DNVCTL")`,
`SetEnvKeyReplacer("-" → "_")`, `AutomaticEnv`, optional `--config` file; every
value is then read through viper, never off the flag. So `DNVCTL_CLUSTER`,
`DNVCTL_SP`, `DNVCTL_GATEWAY_ADDRESS` work as ambient context; mechanically
every flag is env-settable, but only the globals are documented for env use.
Flag > env > config > default, viper's standard order.

A global left empty is sent empty; commands whose request lacks the field
(e.g. `cluster list`, `sp find-names`) simply ignore the global (CT8).

### 2.2 Connection

dnvctl is a real dnv component, not an integtest driver, so **CT2**: it MUST
dial with the grpc.md §4 mandatory client block —

```go
grpc.NewClient(gatewayAddress,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
    grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
    grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()))
```

— one connection per invocation, closed on exit. Every RPC runs under
`context.WithTimeout(ctx, timeout seconds)`. All 59 RPCs are unary; the stream
chain is installed anyway because §4 says both chains on every dnv connection.

### 2.3 Trace ids

**CT2 (part).** Per invocation dnvctl builds `ctx` as
`common.WithTraceId(context.Background(), id)` where `id` is `--trace-id` when
non-empty, else `common.NewTraceId()` (the T4 generator). The §4 client chain's
`attachTraceId` moves it into the `trace_id` outgoing metadata; dnvctl never
touches metadata directly (that shortcut is the *drivers'* carve-out, grpc.md
§6, and does not apply here). The id appears in the failure line (§3.2) so a
failed command hands the operator the exact
`jq 'select(.trace_id == "…")'` key for the gateway/agent logs.

## 3. Output and errors

### 3.1 Result rendering — CT4

gatewayctl's emit pipeline, verbatim: marshal the reply with
`protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}`, re-parse
through `encoding/json` into `any`, `json.Marshal` that, `fmt.Println` — one
canonical JSON document per invocation on stdout (sorted keys, stable spacing,
proto field names, proto3 defaults visible, uint64 as JSON strings).

The two bitmap reads are the one deviation, for gatewayctl's reason (protojson
renders `bytes` as base64): `td get-bm` and `td get-leg-bm` print

```json
{"bitmap_hex":"<lowercase hex, no 0x>","byte_cnt":<n>}
```

Nothing else is ever printed to stdout. There is no quiet/verbose mode.

### 3.2 Errors and exit codes — CT5

| exit | meaning | stdout | stderr |
|---|---|---|---|
| 0 | RPC returned OK | the §3.1 document | empty |
| 1 | RPC or connection failure | empty | one line: `dnvctl: <CODE>: <message> (trace_id <id>)` |
| 2 | usage error (unknown command/flag, unparsable flag value) | empty | cobra's message; **no RPC is issued** |

`<CODE>` is the UPPER_SNAKE gRPC code (`NOT_FOUND`, `ABORTED`,
`DEADLINE_EXCEEDED`, `UNAVAILABLE`, …) via a `codeNames` table, because
`codes.Code.String()` is CamelCase and operators grep for the wire spelling.
`<message>` is the status message verbatim (e.g. `stale revision`). A dial
that cannot even resolve/refuses maps to whatever code gRPC reports
(`UNAVAILABLE` for a refused connection) — dnvctl adds no translation layer.

## 4. Revision tokens (`--rev`) — CT3

34 of the 59 requests carry a token message: `dn_rev` on DeleteDiskNode and
UpdateDiskNodeDisabled; `cn_rev` on the two CN mirrors; `sp_rev` on the 30
SP-scoped mutators (every Create/Delete/Update/Append/Finish/Cancel/Switch and
GrowSlice under an SP). Reads carry none; tokens are obtained from `dn get`,
`cn get`, `sp get`.

Presence semantics — dnvctl sends exactly what was typed:

* `--rev` **not given** ⇒ the token field is **absent** (nil message).
* `--rev N` (base-0: `7`, `0x1f`) ⇒ the token message is present with
  `revision = N` and nothing else set (the gateway ignores the token's echo
  fields; only `revision` participates — `gateway/common.go:296-298`).
* `--rev 0` ⇒ the message is present with revision 0 — proto3 message presence
  keeps this distinguishable from omission — and stays the deliberate
  always-stale probe the gateway suite's B4 stage relies on.

**The gateway side (§0 #9):** GW6 is presence-based — token message absent ⇒
the check is skipped; present ⇒ strict equality. So a token-less mutator
against a real gateway now succeeds. It succeeds *ungated*, which is the point
of risks_and_gaps.md RK8: for concurrent or scripted work the operator should
still do `sp get` → pass `--rev`, and reserve the token-less form for
interactive single-operator use. The §7 suite is immune to all of this: its
token assertions are request-side (what dnvctl put on the wire), which is true
under either gateway.

## 5. The command tree

### 5.0 Conventions

* **Field→flag rule.** Every request field maps to exactly one flag, with two
  exceptions: `cluster_name` (global `--cluster`; the `cluster` group's own
  commands take `--name`, falling back to the global when empty — gatewayctl's
  `clusterNameOf` rule) and `sp_name` (global `--sp` everywhere, including
  `sp create`).
* **Identity flags per group:** `cluster` → `--name`; `dn`/`cn` → `--addr`
  (their name *is* an `ip:port`); `sp` → the global; `cntlr` → `--id`;
  `td`/`clone`/`xfer`/`migr` → `--name`; `ss` → `--nqn`; `ns` → `--nqn` +
  `--idx`; `spare` → `--grp` (+ `--leg`). `sp inspect-side --id` is the side
  id — deliberately the same spelling as `cntlr … --id`, as in gatewayctl.
* **Flag value types** (ported from gatewayctl): ids are hexUint (Go base-0
  parse, `0x` accepted); `--slots` and `--ids` are comma-split numeric lists;
  `--hosts`, `--*-black`, `--*-white` are comma-split string lists that
  REPLACE on each occurrence; booleans that must be turned off are written
  `--enabled=false` (never `--enabled false`).
* **Shared flag helpers** (one implementation each in `ctl/`):
  `trConfFlags(prefix)` → `--<prefix>tr-type`/`--<prefix>adr-fam`/
  `--<prefix>tr-addr`/`--<prefix>tr-svc-id`, defaults `tcp`/`ipv4`/
  `127.0.0.1`/`4420`, all four empty ⇒ nil conf; `selectorFlags(prefix)` →
  `--<prefix>-black`/`--<prefix>-white` ⇒ `NodeSelector`, both empty ⇒ nil;
  `dmCloneConfFlags` → `--hyd-threshold`/`--hyd-batch`, both 0 ⇒ nil (the §7
  GW11 "not given" convention); page flags `--count` (0 = server default) +
  `--page-token`.
* **CT8 — no client-side validation.** dnvctl rejects only what fails to
  *parse* (exit 2 — malformed `--bm-hex`, non-numeric id); every parsed value
  is sent as typed, and the gateway's §7 validation answers. Empty required
  fields, contradictory flags, unknown enum numbers: all forwarded.
* **CT1 — completeness.** The 59 rows below are exhaustive; CT-T1 pins them
  against `pb.Gateway_ServiceDesc` in both directions.

Sweep argv values for every command are in §7.10; the tables here define the
surface.

### 5.1 `cluster` — `ctl/cluster.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `cluster create` | CreateCluster | `--name` | pure gateway defaults; no conf flags in v1 |
| `cluster delete` | DeleteCluster | `--name` | |
| `cluster get` | GetCluster | `--name` | |
| `cluster list` | ListClusters | `--count`, `--page-token` | the one request with no `cluster_name`; the globals are ignored |

### 5.2 `dn` — `ctl/dn.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `dn create` | CreateDiskNode | `--addr`, `--location`, `--disabled`, trConfFlags("") | the gateway calls the agent's `GetDnSize` inline; the trace id chains through |
| `dn delete` | DeleteDiskNode | `--addr` (+ `--rev` ⇒ `dn_rev`) | |
| `dn get` | GetDiskNode | `--addr` | DnRev token source |
| `dn list` | ListDiskNodes | `--count`, `--page-token` | |
| `dn set-disabled` | UpdateDiskNodeDisabled | `--addr`, `--disabled` (+ `--rev`) | explicit value, not a toggle (gateway §0 #17 idempotency) |
| `dn inspect` | InspectDiskNode | `--addr` | live agent read (`GetDnInfo`), not etcd |

### 5.3 `cn` — `ctl/cn.go`

Exact `dn` mirrors against the CN RPCs: `cn create`, `cn delete`, `cn get`,
`cn list`, `cn set-disabled`, `cn inspect` ↔ CreateControllerNode,
DeleteControllerNode (+`cn_rev`), GetControllerNode, ListControllerNodes,
UpdateControllerNodeDisabled (+`cn_rev`), InspectControllerNode. Same flags.

### 5.4 `sp` — `ctl/sp.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `sp create` | CreateStoragePool | `--cntlr-cnt`, `--slice-cnt`, `--init-ext-cnt`, `--slots`, `--redund` (default `raid1`; `raid1`\|`none`), `--bitmap-chunk-blocks`, `--stripe-size`, `--block-size`, `--thr-primary`/`--thr-cntlr`/`--thr-side`/`--thr-leg`, selectorFlags("dn"), selectorFlags("cn") | `bdev_conf` is **always** sent with `redund_conf` set per `--redund` (§0 #11): `raid1` ⇒ `redund_md_raid1{bitmap_chunk_block_cnt}` (0 = CP default), `none` ⇒ `redund_none{}`; `dm_raid0_conf.stripe_size`/`dm_pool_conf.data_block_size` only when non-zero; `event_threshold` only when a `--thr-*` is non-zero; no feature flags (§1.1) |
| `sp delete` | DeleteStoragePool | (+ `--rev`) | |
| `sp get` | GetStoragePool | — | **the SpRev token source** |
| `sp list` | ListStoragePools | `--count`, `--page-token` | no `sp_name` in the request |
| `sp set-cntlid-slots` | UpdateStoragePoolCntlidSlotList | `--slots` (+ `--rev`) | an empty list is sent as-is; the gateway refuses |
| `sp set-level` | UpdateStoragePoolLevel | `--level` (default `READWRITE`) (+ `--rev`) | accepts short name, `SP_LEVEL_*`, or a raw number for undeclared values |
| `sp find-names` | FindStoragePoolNames | `--ids` | `sp_id_list`; no `sp_name` — the global is ignored |
| `sp grow-slice` | GrowSlice | `--slice`, `--ext`, `--meta`, selectorFlags("dn") (+ `--rev`) | `--meta` and `--ext` do not constrain each other client-side (CT8) |
| `sp inspect-side` | InspectSide | `--id` | side id; live agent read |

### 5.5 `cntlr` — `ctl/cntlr.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `cntlr create` | CreateCntlr | `--slot`, selectorFlags("cn") (+ `--rev`) | |
| `cntlr delete` | DeleteCntlr | `--id` (+ `--rev`) | gateway requires non-primary + disabled |
| `cntlr set-enabled` | UpdateCntlrEnabled | `--id`, `--enabled` (default `true`) (+ `--rev`) | disabling the last enabled cntlr stops IO; the gateway allows it and dnvctl does **not** pre-warn in v1 (§0 #10) |
| `cntlr inspect` | InspectCntlr | `--id` | live agent read |

### 5.6 `td` — `ctl/td.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `td create` | CreateThinDevice | `--name`, `--ori`, `--size` (+ `--rev`) | `--ori` set ⇒ snapshot; `--size 0` legal only then |
| `td delete` | DeleteThinDevice | `--name` (+ `--rev`) | |
| `td list` | ListThinDevices | — | the `created` poll (ThinDeviceCreated.md R13): §3.1's EmitUnpopulated shows the field on every td |
| `td get-bm` | GetThinDeviceBitmap | `--name`, `--slice-idx`, `--start`, `--cnt` | hex-map output (§3.1); gatewayctl spelled the name flag `--td`, dnvctl uses the group-uniform `--name` |
| `td get-leg-bm` | GetLegBitmap | `--leg`, `--start`, `--cnt` | hex-map output |

### 5.7 `ss` — `ctl/ss.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `ss create` | CreateSubsystem | `--nqn`, `--hosts` (+ `--rev`) | |
| `ss delete` | DeleteSubsystem | `--nqn` (+ `--rev`) | |
| `ss list` | ListSubsystems | — | the only gateway read that shows namespaces |
| `ss set-hosts` | UpdateSubsystemHosts | `--nqn`, `--hosts` (+ `--rev`) | full replacement |

### 5.8 `ns` — `ctl/ns.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `ns create` | CreateNamespace | `--nqn`, `--idx`, `--td`, `--uuid`, `--nguid`, `--suspended` (+ `--rev`) | empty `--uuid`/`--nguid` ⇒ the gateway mints them |
| `ns delete` | DeleteNamespace | `--nqn`, `--idx` (+ `--rev`) | |
| `ns set-dev` | UpdateNamespaceDev | `--nqn`, `--idx`, `--td` (+ `--rev`) | |
| `ns set-suspended` | UpdateNamespaceSuspended | `--nqn`, `--idx`, `--suspended` (default `true`) (+ `--rev`) | writes + bumps even when unchanged |

### 5.9 `clone` — `ctl/clone.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `clone create` | CreateClone | `--name`, `--dst-td`, `--src-nqn`, `--src-idx`, `--src-slices`, `--src-stripe`, `--src-block`, trConfFlags("src-"), `--auto-resume`, dmCloneConfFlags (+ `--rev`) | src trConf nil ⇒ an *empty* `src_tr_conf` list is sent (the gateway's "must not be empty" refusal is reachable) |
| `clone delete` | DeleteClone | `--name`, `--force` (+ `--rev`) | `--force` skips the CN copy-finished proof |
| `clone get` | GetClone | `--name` | |
| `clone set-tr` | UpdateCloneTrConf | `--name`, trConfFlags("src-") (+ `--rev`) | |
| `clone append-bm` | AppendCloneBitmap | `--name`, `--slice-idx`, `--bm-hex` (+ `--rev`) | empty `--bm-hex` sends an empty bitmap on purpose; a malformed non-empty value is a usage error (exit 2) |

### 5.10 `xfer` — `ctl/xfer.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `xfer create` | CreateTransfer | `--name`, `--ori-nqn`, `--ori-idx`, `--hosts`, `--auto-suspend` (+ `--rev`) | |
| `xfer delete` | DeleteTransfer | `--name`, `--force` (+ `--rev`) | `--force` = the abort path (no origin resume proof) |
| `xfer get` | GetTransfer | `--name` | |
| `xfer set-hosts` | UpdateTransferHosts | `--name`, `--hosts` (+ `--rev`) | replaces |

### 5.11 `migr` — `ctl/migr.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `migr create` | CreateMigration | `--name`, `--src-side`, selectorFlags("dn"), dmCloneConfFlags (+ `--rev`) | |
| `migr finish` | FinishMigration | `--name`, `--force` (+ `--rev`) | |
| `migr cancel` | CancelMigration | `--name` (+ `--rev`) | no `--force` — the RPC has none |
| `migr get` | GetMigration | `--name` | |
| `migr append-bm` | AppendMigrationBitmap | `--name`, `--bm-hex` (+ `--rev`) | no slice index — the RPC has none |

### 5.12 `spare` — `ctl/spare.go`

| command | RPC | flags beyond globals | notes |
|---|---|---|---|
| `spare create` | CreateSpareLeg | `--grp`, selectorFlags("dn") (+ `--rev`) | |
| `spare delete` | DeleteSpareLeg | `--grp`, `--leg` (+ `--rev`) | |
| `spare switch` | SwitchSpareLeg | `--grp`, `--spare`, `--target` (+ `--rev`) | |

Tally: 4+6+6+9+4+5+4+4+5+4+5+3 = **59**.

## 6. Unit tests

All in `ctl/` (`*_test.go`) except CT-T6.

* **CT-T1 — completeness.** An `rpcToCmd` map of all 59 RPC names →
  `"<group> <verb>"`, cross-checked against `pb.Gateway_ServiceDesc.Methods`
  in both directions, with `len == 59` pinned (the gatewayctl test, ported).
* **CT-T2 — argv → request.** A `recordingClient` embedding `pb.GatewayClient`
  and overriding only the driven method (anything else panics); a
  `runArgv(argv)` helper parses through the real cobra tree and returns the
  captured request. Table tests per command assert every field, including:
  global `--cluster`/`--sp` fill; the `cluster` group's `--name` fallback; the
  §4 token trio (absent flag ⇒ nil message; `--rev 0` ⇒ present, revision 0;
  `--rev 0x1f` ⇒ 31); trConf/selector/dmClone nil rules; list replace-on-set;
  `sp create`'s always-present `redund_conf` for both `--redund` values.
* **CT-T3 — rendering goldens.** The §3.1 pipeline: canonical key order,
  uint64-as-string, EmitUnpopulated, and the bitmap hex map for both bitmap
  reads (`{"bitmap_hex":"a5","byte_cnt":1}`).
* **CT-T4 — error surface.** A stub client returning
  `status.Error(codes.Aborted, "stale revision")` yields exit path 1 and the
  exact §3.2 stderr line; an unknown flag yields 2 with zero client calls;
  the `codeNames` table covers every `codes.Code`.
* **CT-T5 — trace id.** `--trace-id x` arrives verbatim in the server's
  incoming `trace_id` metadata (bufconn server behind the real §4 server
  chain); with the flag empty a non-empty minted id arrives; two invocations
  mint two different ids.
* **CT-T6 — fakegateway** (`integtest/fakegateway/main_test.go`, bufconn):
  default success on every RPC; behavior.json code/message injection; `hang`
  released by ctx cancel and by lever clear; `reply` injection; state.json
  count/last_request writing and the atomic-rename property; malformed
  behavior keeps the previous behavior; unknown behavior keys rejected.

## 7. Integration test plan

### 7.1 Goal and scope

Prove, against a real gRPC wire, that every dnvctl command marshals its argv
into exactly the intended request proto, renders replies per §3.1, maps errors
per §3.2, and carries trace ids per §2.3 — with the gateway replaced by
`integtest/fakegateway`, so this suite tests **dnvctl only**. Gateway
semantics (validation, tokens, STM behavior) stay `gateway_test.sh`'s job; the
data plane is not touched. No sudo anywhere.

### 7.2 Deliverables and usage contract

```
bash integtest/dnvctl_test.sh [--only <case>] [--cleanup-only] user@ip
```

`CASES=(smoke sweep behavior errors transport)`; `--only` is validated against
it (unknown ⇒ usage, exit 2). Exactly one `user@ip` positional (`IP` split off
with `##*@`). Cleanup runs unconditionally at start; on success at the end;
`--cleanup-only` does only that. Success prints the literal `PASS` line on
stderr; any failure exits 1 leaving all debris + logs on the target and dumps
§7.15 diagnostics. All output conventions (`log`, `die`, `assert_eq`,
`assert_ge`, `stage`, `QUIET`) are the gateway suite's, copied.

### 7.3 Topology

One VM. The suite runs on the developer machine; `dnvctl` and `fakegateway`
run on the VM; every step is `ssh` (the worker/gateway pattern — the driver
here *is* dnvctl). The fake listens on `127.0.0.1:29840`, so nothing is
reachable off-box. Layout on the VM:

```
$WORK = /var/tmp/dnv-dnvctl-integtest
├── bin/dnvctl  bin/fakegateway
└── fgw/{behavior.json,state.json,fakegateway.log,pid}
```

Ports: `ALL_PORTS=(29840 29841)` — 29840 the fake, 29841 **deliberately never
listened on** (the transport case dials it expecting a refusal, so preflight
must prove it free too). Both are outside every range the other five suites
and production use (29527/2379 reserved; 295xx/296xx/297xx/298[1-3]x taken).

### 7.4 Assumptions and preflight

Local (`preflight_driver`): `go ssh scp tar sha256sum date awk sed` present;
`resolve_jq` (system jq else gojq, the shared idiom); then
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build` (produces `bin/dnvctl`) and
`go build -o integtest/bin/fakegateway ./integtest/fakegateway`. No etcd, no
downloads. Remote (`preflight_server`, after the start cleanup): passwordless
ssh; `command -v bash nohup pkill ss df awk sed`; ≥ 1048576 KiB free under
`/var/tmp`; both `ALL_PORTS` free (`ports_up` with `grep -c`, never `-q` —
the SIGPIPE/pipefail gotcha). `SSH_OPTS` as in the gateway suite. Binaries are
scp'd to `$WORK/bin` and `chmod 0755`.

### 7.5 The fake: `integtest/fakegateway`

`fakegateway --grpc-address <ip:port> --dir <dir>` — registers **all 59**
`service Gateway` methods (all unary) behind the mandatory §4 server
interceptors (`grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor())`
+ the stream twin, installed even though unused — same rule as fakeagent: the
JSON log on stdout is the suite's record). Startup record
`"fakegateway started"` with `grpc_address`, `dir`.

**Request recording (always first).** On every call, before any behavior is
applied, the fake bumps the method's count and stores the request, then writes
`state.json` atomically (`json.MarshalIndent` → `CreateTemp` → `Rename`, the
fakeagent idiom):

```json
{"methods": {"<RpcName>": {"count": <n>, "last_request": <protojson>}}}
```

`last_request` uses `protojson.MarshalOptions{UseProtoNames: true}` **without**
EmitUnpopulated — so an absent token message is an absent key, which is what
the §4 assertions grep for (and a present-but-zero token renders `{}`).

**behavior.json** — reloaded at the top of every request when mtime *or size*
changed; `DisallowUnknownFields` + trailing-data check; malformed ⇒ previous
behavior kept + `"behavior file malformed"`; file gone ⇒ reset to defaults
(all the fakeagent reload semantics, including the own-write mtime guard on
state.json so an operator edit of state is picked up but the fake's own writes
are not re-read):

```json
{"default": {"code": "...", "message": "...", "hang": false},
 "methods": {"<RpcName>": {"code": "...", "message": "...",
                            "hang": false, "reply": { …protojson… }}}}
```

Per method, most-specific wins key-by-key (`methods.<Rpc>` over `default`).
`code` is UPPER_SNAKE (`OK` when absent); non-OK ⇒
`status.Error(code, message)` with `message` defaulting to
`"behavior.json <code>"`. `hang` ⇒ poll every 200 ms until the lever clears or
the ctx dies (how the transport case manufactures DEADLINE_EXCEEDED). `reply`
is strict protojson of the method's reply type (malformed ⇒ the whole file is
treated as malformed); absent ⇒ the canned default, an **empty reply message**
— dnvctl's EmitUnpopulated rendering fills the shape client-side.

### 7.6 Identity plan and fixtures

`CLUSTER=itctl`, `SP=sp0`; the global argv prefix every step uses is

```
$WORK/bin/dnvctl --gateway-address 127.0.0.1:29840 \
  --cluster $CLUSTER --sp $SP --trace-id $TRACE
```

wrapped by `ctl()` (the `gw_at` pattern: `printf '%q '` quoting, run via ssh;
`ctl_ok` captures stdout, asserts exit 0 and empty stderr; `ctl_fail <CODE>`
captures stderr locally via a temp file, asserts exit 1 and that the single
stderr line matches `dnvctl: <CODE>: .* (trace_id $TRACE)`; `ctl_usage`
asserts exit 2 and, via §7.7's counts, that no request reached the fake).

Fixture values (payload only — nothing dials them): DN/CN addr
`127.0.0.1:29901` / `127.0.0.1:29902`, locations `rack0`/`rack1`;
`NQN=nqn.2025-01.io.dnv:itctl:sp0:ss0`, host `nqn.2025-01.io.dnv:host0`,
clone source `nqn.2025-01.io.dnv:src:ss0`;
uuid `6f7d0f3e-0dd6-4f22-9a34-5e0f1a2b3c4d`, nguid
`00112233445566778899aabbccddeeff`; td `t0`/`t1`, clone `cl0`, xfer `x0`,
migr `m0`; td size `67108864`; `--rev 7` on every token-carrying sweep step.
Trace ids `it-<case>-<step>` via the house `stage()`.

### 7.7 Assertion conventions

* `rec()` / `count_recs` over `$WORK/fgw/fakegateway.log` (the
  `fromjson? // empty` torn-line guard) for interceptor records:
  `select(.msg == "grpc server request" and .trace_id == "$TRACE" and
  (.method | endswith("<Rpc>")))`.
* `last_req <Rpc>` = `jq '.methods["<Rpc>"].last_request'` over
  `$WORK/fgw/state.json`; `req_count <Rpc>` likewise. Field equality via
  `jq -e` filters; **uint64 fields compare as strings** (`.sp_rev.revision ==
  "7"`), protojson's doing.
* `set_behavior` writes behavior.json via the atomic tmp+`mv` ssh idiom;
  every case starts by resetting it to `{}` (`case_reset`).
* dnvctl stdout must satisfy `jq -e .` (parse) on every `ctl_ok`; goldens
  compare byte-exact after the fact.

### 7.8 Setup phase (after the start cleanup)

1. `mkdir -p $WORK/bin $WORK/fgw`; scp the two binaries.
2. `remote_start fgw fakegateway.log "$WORK/bin/fakegateway --grpc-address
   127.0.0.1:29840 --dir $WORK/fgw"` (nohup + pid file, the house helper).
3. `wait_until 15 "fakegateway up" fgw_ready` where `fgw_ready` is
   `ctl cluster list` exiting 0 (dnvctl doubles as the readiness probe).

### 7.9 Case S — smoke (`--only smoke`)

| step | action | asserts |
|---|---|---|
| s1 | `ctl_ok cluster list` | exit 0; stdout parses; fake log has the ListClusters request with `trace_id it-smoke-1`; `req_count ListClusters` bumped |
| s2 | same but **without** `--trace-id` | the request record's `.trace_id` is non-empty and is not any `it-*` id — the §2.3 mint, observed on the wire |
| s3 | stderr of s1/s2 | empty: Warn silencing + stdout reservation hold on the success path (CT7) |

### 7.10 Case A — sweep (`--only sweep`)

59 steps, trace ids `it-sweep-01`…`it-sweep-59`, one per §5 row, in §5 order.
Uniform per-step assertions: exit 0; stdout parses; `last_req <Rpc>` equals
the argv-implied request **field for field** (globals included:
`cluster_name == "itctl"` on the 58, `sp_name == "sp0"` on the 41, token
`revision == "7"` on the 34); `req_count` bumped exactly once. The table lists
argv after the global prefix and only the *distinctive* assertions.

| # | argv | distinctive assertions |
|---|---|---|
| 01 | `cluster create --name c1` | `cluster_name == "c1"` (`--name` wins over global) |
| 02 | `cluster delete --name c1` | |
| 03 | `cluster get` | `cluster_name == "itctl"` (fallback to global) |
| 04 | `cluster list --count 2 --page-token pt0` | no `cluster_name` key at all |
| 05 | `dn create --addr 127.0.0.1:29901 --location rack0` | `nvme_tr_conf == {tcp, ipv4, 127.0.0.1, 4420}` (helper defaults); `disabled` absent/false |
| 06 | `dn delete --addr 127.0.0.1:29901 --rev 7` | `dn_rev.revision == "7"` |
| 07 | `dn get --addr 127.0.0.1:29901` | no token key |
| 08 | `dn list --count 8` | |
| 09 | `dn set-disabled --addr 127.0.0.1:29901 --disabled --rev 7` | `disabled == true` |
| 10 | `dn inspect --addr 127.0.0.1:29901` | |
| 11-16 | the six `cn` mirrors at `127.0.0.1:29902`, `rack1` | `cn_rev.revision == "7"` on delete/set-disabled |
| 17 | `sp create --cntlr-cnt 2 --slice-cnt 1 --init-ext-cnt 2 --slots 0,1 --rev 7` | `bdev_conf.redund_conf.redund_md_raid1` present (§0 #11 default); no `dm_raid0_conf`/`event_threshold` keys |
| 18 | `sp delete --rev 7` | `sp_name == "sp0"` from the global |
| 19 | `sp get` | |
| 20 | `sp list --count 4` | no `sp_name` key |
| 21 | `sp set-cntlid-slots --slots 0,1,2 --rev 7` | `cntlid_slot_list == [0,1,2]` |
| 22 | `sp set-level --level READONLY --rev 7` | enum by name |
| 23 | `sp find-names --ids 1,2` | `sp_id_list == ["1","2"]`; no `sp_name` |
| 24 | `sp grow-slice --slice 0 --ext 2 --dn-white 127.0.0.1:29901 --rev 7` | `dn_selector.white_list` set; no `is_meta` key |
| 25 | `sp inspect-side --id 5` | `side_id == "5"` |
| 26 | `cntlr create --slot 1 --rev 7` | |
| 27 | `cntlr delete --id 3 --rev 7` | |
| 28 | `cntlr set-enabled --id 3 --enabled=false --rev 7` | `enabled` absent/false — the `=` spelling |
| 29 | `cntlr inspect --id 3` | |
| 30 | `td create --name t0 --size 67108864 --rev 7` | no `ori_name` key |
| 31 | `td delete --name t0 --rev 7` | |
| 32 | `td list` | |
| 33 | `td get-bm --name t0 --slice-idx 0 --start 0 --cnt 64` | golden stdout (behavior-injected 1-byte bitmap `a5`): `{"bitmap_hex":"a5","byte_cnt":1}` |
| 34 | `td get-leg-bm --leg 9 --start 0 --cnt 64` | hex-map output |
| 35 | `ss create --nqn $NQN --hosts nqn.…:host0 --rev 7` | |
| 36 | `ss delete --nqn $NQN --rev 7` | |
| 37 | `ss list` | |
| 38 | `ss set-hosts --nqn $NQN --hosts a,b --rev 7` | `allowed_hosts == ["a","b"]` |
| 39 | `ns create --nqn $NQN --idx 1 --td t0 --uuid $UUID --nguid $NGUID --rev 7` | |
| 40 | `ns delete --nqn $NQN --idx 1 --rev 7` | |
| 41 | `ns set-dev --nqn $NQN --idx 1 --td t1 --rev 7` | |
| 42 | `ns set-suspended --nqn $NQN --idx 1 --rev 7` | `suspended == true` — the flag's default-true |
| 43 | `clone create --name cl0 --dst-td t1 --src-nqn $SRC_NQN --src-idx 0 --src-slices 1 --src-stripe 16384 --src-block 1048576 --rev 7` | `src_tr_conf` is a 1-element list of the `src-` defaults; no `dm_clone_conf` key |
| 44 | `clone delete --name cl0 --force --rev 7` | `force == true` |
| 45 | `clone get --name cl0` | |
| 46 | `clone set-tr --name cl0 --src-tr-addr 127.0.0.1 --rev 7` | |
| 47 | `clone append-bm --name cl0 --slice-idx 0 --bm-hex a5 --rev 7` | `bitmap` b64 of `0xa5` in state.json |
| 48 | `xfer create --name x0 --ori-nqn $NQN --ori-idx 1 --hosts nqn.…:host0 --auto-suspend --rev 7` | |
| 49 | `xfer delete --name x0 --rev 7` | no `force` key |
| 50 | `xfer get --name x0` | |
| 51 | `xfer set-hosts --name x0 --hosts c --rev 7` | |
| 52 | `migr create --name m0 --src-side 5 --hyd-threshold 8 --hyd-batch 4 --rev 7` | `dm_clone_conf == {8,4}` |
| 53 | `migr finish --name m0 --rev 7` | |
| 54 | `migr cancel --name m0 --rev 7` | |
| 55 | `migr get --name m0` | |
| 56 | `migr append-bm --name m0 --bm-hex a5a5 --rev 7` | |
| 57 | `spare create --grp 1 --rev 7` | |
| 58 | `spare delete --grp 1 --leg 2 --rev 7` | |
| 59 | `spare switch --grp 1 --spare 6 --target 4 --rev 7` | `spare_leg_id == "6"`, `target_leg_id == "4"` |

Reply-side goldens (§0 #14): steps 03 (`cluster get`, empty canned reply —
pins EmitUnpopulated + key order), 19 (`sp get` with a behavior-injected reply
carrying a revision — pins uint64-as-string), 33 (the hex map). Everything
else: parse + one `jq -e` spot-check against the injected/canned reply.

### 7.11 Case B — behavior (`--only behavior`)

| step | action | asserts |
|---|---|---|
| b1 | `td create --name t0` (no `--rev`) | `last_req CreateThinDevice` has **no `sp_rev` key** — omission = absent message (§4) |
| b2 | same with `--rev 0` | `sp_rev` key present, `== {}` (present message, zero revision — the protojson rendering of the always-stale probe) |
| b3 | same with `--rev 0x1f` | `sp_rev.revision == "31"` — base-0 parse |
| b4 | `sp create --redund none --rev 7` | `bdev_conf.redund_conf.redund_none` present, no `redund_md_raid1` key |
| b5 | inject `ListThinDevices` reply with one td `created:true`, run `td list` | stdout `jq -e '.name_to_td.t0.created == true'`; then with a `created:false` td, the key is still present (EmitUnpopulated). **Corrected 2026-09-11:** this row said `.td_list[0].created`, but `ListThinDevicesReply` carries `map<string, ThinDevice> name_to_td` and has no `td_list` at all — the old filter evaluated to `null` against a *correct* dnvctl, so it would have passed nothing and failed nothing |
| b6 | `DNVCTL_CLUSTER=envclu` in the remote env, no `--cluster` flag | `cluster_name == "envclu"`; then with both, the flag wins (CT9 precedence) |
| b7 | `cluster get --name other` vs bare | `--name` wins; fallback covered in sweep 03 |

### 7.12 Case C — errors (`--only errors`)

Each step injects via behavior.json, calls, and asserts the full §3.2 contract
(exit code, empty stdout, the one-line stderr with UPPER_SNAKE code, message,
trace id):

| step | injection | call | stderr contains |
|---|---|---|---|
| c1 | `GetStoragePool` → `NOT_FOUND` | `sp get` | `dnvctl: NOT_FOUND: … (trace_id it-errors-1)` |
| c2 | `DeleteThinDevice` → `ABORTED`, message `stale revision` | `td delete --name t0 --rev 7` | `ABORTED: stale revision` — the §4 failure an operator will actually meet |
| c3 | `CreateCluster` → `ALREADY_EXISTS` | `cluster create --name c1` | `ALREADY_EXISTS` |
| c4 | `CreateStoragePool` → `INVALID_ARGUMENT` | `sp create --rev 7` | `INVALID_ARGUMENT` |
| c5 | *(none)* | `ctl_usage td create --no-such-flag` | exit 2; `req_count CreateThinDevice` unchanged — no RPC on usage errors |
| c6 | *(none)* | `ctl_usage clone append-bm --name cl0 --bm-hex zz --rev 7` | exit 2 (parse failure), count unchanged |

### 7.13 Case D — transport (`--only transport`)

| step | action | asserts |
|---|---|---|
| d1 | `ctl_fail UNAVAILABLE` with `--gateway-address 127.0.0.1:29841 cluster list` | exit 1, `UNAVAILABLE`, returns well inside the 10 s default |
| d2 | behavior `methods.ListClusters.hang = true`; `--timeout 2 cluster list` | exit 1, `DEADLINE_EXCEEDED`; wall clock in [2 s, 5 s); then clear the lever and `ctl_ok cluster list` proves recovery |

### 7.14 Teardown and cleanup

`cleanup_script` heredoc over `ssh bash -s`, the house shape: gather
`$WORK/*/pid`, `CONT` → `TERM` → poll 20×0.25 s → `KILL`, then
`pkill -f 'bin/fakegateway'`, `rm -rf $WORK`, `echo cleaned`. Run at start
always, at end on success, alone under `--cleanup-only`. After it, 29840/29841
are free and `$WORK` is absent.

### 7.15 Failure diagnostics

On any failure `on_exit` prints: failing stage + trace id; the last 50
`fakegateway.log` records filtered to `grpc server *` and the fake's own
`behavior/state file` records; `state.json` and `behavior.json` verbatim;
`ss -ltn` grepped to the two ports; `ps -ef | grep -F $WORK`; and the ready
line `jq 'select(.trace_id=="<TRACE>")' $WORK/fgw/fakegateway.log`.

### 7.16 RPC coverage matrix

Sweep = all 59 (§7.10 steps 01-59). Beyond it:

| group | RPCs | extra coverage |
|---|---|---|
| cluster | 4 | S s1/s2 (ListClusters), B b6/b7, C c3, D d1/d2 (ListClusters) |
| dn / cn | 6+6 | sweep only (token trio generalizes via b1-b3) |
| sp | 9 | B b4, C c1/c4; goldens 19 |
| cntlr | 4 | sweep only |
| td | 5 | B b1-b3/b5, C c2/c5; golden 33 |
| ss / ns | 4+4 | sweep only |
| clone | 5 | C c6 |
| xfer / migr / spare | 4+5+3 | sweep only |

### 7.17 Out of scope (v1)

Real-gateway runs of this suite (that is `gateway_test.sh`'s harness; a
dnvctl-vs-real-gateway smoke can join it later); concurrency (`race` stays a
gatewayctl-only tool); the copier; performance; any assertion about the
gateway's *reaction* to what dnvctl sends.

## 8. Amendments to companion documents

Recorded for traceability; the edits are applied with this document.

* `README.md` "Not yet implemented" — the spec pointer for `ctl/` +
  `cmd/dnvctl` becomes **`doc/dnvctl.md`** (was architecture.md §13).
* `doc/architecture.md` §1 component table, `dnvctl` row — now points here;
  the §11.4 userspace copier is marked future work outside dnvctl v1 (§0 #3).
* `doc/architecture.md` §13 — the one-line subcommand sketch is replaced by a
  pointer to this document (same copier note).
* `doc/layout.md` `ctl/` block — the file list becomes §1.2's (per-group
  files; `copier.go` removed as future work); build-out step 8 now cites this
  doc and its §7 suite.
* `doc/gateway.md` §1 item 1 — "its own future document" becomes a reference
  to `dnvctl.md`.
* `doc/ThinDeviceCreated.md` R13 — "`dnvctl`'s `vol` subcommands" becomes
  "`dnvctl`'s `td list` (dnvctl.md §5.6)": the group is named `td`, not `vol`.
* `doc/risks_and_gaps.md` — **RK7** (a dnvctl mutator without `--rev` fails
  until GW6 becomes presence-based, §0 #9) was added with this document and
  CLOSED the same day by the gateway change; **RK8** replaces it and records
  what the bypass costs — a token-less mutator has no optimistic-concurrency
  gate. Id range in the preamble extended to RK8.

Deliberately **not** amended: `doc/grpc.md` (its dnvctl rows were already
correct).

Amended after all, once the code landed: `gateway/cntlr.go`'s comment and
architecture.md §8.6 both asserted in the present tense that "dnvctl warns"
about disabling the last enabled cntlr. That was tolerable while dnvctl did
not exist; with the CLI shipped and §0 #10 deciding against the warning, the
sentence became simply false, so both now say the warning is deferred and why.
The behavior is unchanged — only the claim about it.

Also amended with the implementation: `doc/layout.md`'s `integtest/` block,
which gained the `dnvctl_test.sh` and `fakegateway/` rows.

## 9. Acceptance checklist

1. `go build ./...`, `go vet ./...`, `go test ./...` pass.
2. `go list -deps ./cmd/dnvctl | grep etcd` finds nothing (CT6), and the
   paired dnv-agent gate of layout.md still holds.
3. `cmd/dnvctl/main.go`'s first statement is
   `common.SetLogLevel(slog.LevelWarn)`; `grep -rn "fmt.Print" ctl/` hits only
   the §3.1 emit path (CT4/CT7).
4. `grep -rn "grpc.NewClient" ctl/` hits exactly one site carrying both §4
   chain options (CT2); `grep -rn "AppendToOutgoingContext" ctl/` finds
   nothing (the metadata shortcut is the drivers', not dnvctl's).
5. CT-T1 pins 59 both ways; CT-T2's token trio (absent / `0` / `0x1f`) passes.
6. `bash integtest/dnvctl_test.sh user@<lab vm>` prints `PASS`; each
   `--only` case passes in isolation; `--cleanup-only` leaves 29840/29841 free
   and `$WORK` absent.
7. The §8 amendments are present in the six companion files (spot-grep:
   `rg -l 'dnvctl.md' README.md doc/` hits them all).
8. Against a *real* gateway (manual step, not in the suite): `dnvctl sp get`
   then a mutator with the returned `--rev` succeeds; the same mutator
   **without** `--rev` also succeeds, because GW6 is presence-based; and the
   same mutator with `--rev 0` fails `ABORTED "stale revision"`. The three
   together are what distinguishes presence-based from value-based checking.
