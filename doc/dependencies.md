# Direct dependencies

This document owns the rule for dnv's direct Go module dependencies: which
modules the code imports directly, why each is allowed, and which binaries may
link the etcd client. `go.mod` holds the versions and is authoritative for
them; `layout.md`, Dependency rules, says which package may import which.

The list below matches the direct requirements of `go.mod`, each with the
reason it is allowed. A module enters or leaves both in the same change, and
a new direct dependency enters `go.mod` only together with its entry here,
recording why it is allowed. The requirements `go.mod` marks indirect arrive
with these modules and are not listed.

* `google.golang.org/grpc` — the transport of the three dnv services,
  `Gateway`, `DiskNodeAgent` and `ControllerNodeAgent`, and of the shared
  interceptors every dnv connection and server chains (`grpc.md`, Placement).
* `google.golang.org/protobuf` — the runtime of the code generated from
  `pb/schema.proto` (`layout.md`, Protobuf generation) and the encodings of
  its messages: etcd values and the agents' local store are binary protobuf,
  the logs render messages through `PbToLogValue` (`log.md` R10), and dnvctl
  prints protojson (`dnvctl.md` CT4).
* `golang.org/x/sync` — the weighted semaphore that bounds the
  `LimitedOsClient`'s concurrent operations (`osclient.md`, Construction and
  concurrency limit).
* `github.com/spf13/cobra` — the command trees: the root command of every
  daemon, the `dn` and `cn` subcommands of `dnv-agent` (`dnagent.md` CM1) and
  dnvctl's noun-grouped tree (`dnvctl.md`, Files).
* `github.com/spf13/pflag` — cobra's flag package, imported directly by
  `ctl/`, whose shared flag helpers and leaf-value readers work on a
  `pflag.FlagSet` (`dnvctl.md`, Conventions); it arrives with cobra and
  cannot be avoided.
* `github.com/spf13/viper` — the binding of flags, config file and
  environment: every daemon flag is also a config key and an environment
  variable (`architecture.md`, Components: invocation reference), and so is
  each env-backed dnvctl global flag (`dnvctl.md` CT9).
* `go.etcd.io/etcd/client/v3` — the etcd client, its `concurrency` STM
  included, which `etcdutil` alone wraps (`dnv-worker.md` EU1 and EU4;
  `log.md`, etcd). The integration suites that run an etcd server run one of
  the client's minor version.
* `go.etcd.io/etcd/api/v3` — at the client's version; its `rpctypes` package
  is what `IsCompacted` needs to recognize a watch the server cancelled as
  compacted (`dnv-worker.md` EU3). It arrives with the client and cannot be
  avoided.
* `go.uber.org/zap` — only for `zap.NewNop`, the silenced etcd client logger
  that keeps stderr one JSON stream of dnv's own records (`log.md` R2). It
  arrives with the client and cannot be avoided, and it is not a logging
  library of dnv's: dnv logs through `log/slog` alone (`log.md` R1).

The etcd client and the two modules that arrive with it are linked only into
the three etcd-facing binaries, `dnv-worker`, `dnv-gateway` and `dnv-cdc`,
always through `etcdutil`; the agents and dnvctl stay etcd-free (`layout.md`,
Dependency rules; `dnvctl.md` CT6). Of the integration-test drivers,
`workerctl` and `cdcctl` link the client the same way.
