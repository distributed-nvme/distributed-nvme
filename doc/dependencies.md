# Direct dependencies

Current (must match `go.mod`):

* google.golang.org/grpc
* google.golang.org/protobuf
* golang.org/x/sync
* github.com/spf13/cobra
* github.com/spf13/pflag — cobra's flag package, imported directly by `ctl/`
  for the shared `*pflag.FlagSet` helpers that build the repeated flag groups
  (`dnvctl.md` §5.0, "Shared flag helpers"); arrives with cobra and cannot be
  avoided.
* github.com/spf13/viper
* go.etcd.io/etcd/client/v3 v3.6.14 — the v3.6 line; entered `go.mod` with
  `etcdutil/` (`layout.md` §3, `dnv-worker.md` §3) and linked into the three
  etcd-facing binaries `cmd/dnv-worker`, `cmd/dnv-gateway` and `cmd/dnv-cdc`;
  the agents and `dnvctl` stay etcd-free (`layout.md` §7 item 4). The worker
  integration suite runs an etcd server of the same minor.
* go.etcd.io/etcd/api/v3 — the same version; `v3rpc/rpctypes` for the
  `ErrCompacted` report of `dnv-worker.md` EU3; arrives with the client and
  cannot be avoided.
* go.uber.org/zap — only for `zap.NewNop()`, the silenced etcd client logger
  that `log.md` §7's acceptance grep pins, which keeps the JSON stderr log one
  record per line; arrives with the client and cannot be avoided.
